// Copyright (c) 2016 Company 0, LLC.
// Use of this source code is governed by an ISC
// license that can be found in the LICENSE file.

// Package ratchet implements the axolotl ratchet, by Trevor Perrin. See
// https://github.com/trevp/axolotl/wiki.
package ratchet

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/binary"
	"errors"
	"hash"
	"io"
	"math/big"
	"time"

	"github.com/companyzero/sntrup4591761"
	"github.com/companyzero/zkc/blobshare"
	"github.com/companyzero/zkc/ratchet/disk"

	"golang.org/x/crypto/curve25519"
	"golang.org/x/crypto/nacl/secretbox"
)

const (
	// headerSize is the size, in bytes, of a header's plaintext contents.
	headerSize = 4 /* uint32 message count */ +
		4 /* uint32 previous message count */ +
		32 /* curve25519 ratchet public */ +
		24 /* nonce for message */
	// sealedHeader is the size, in bytes, of an encrypted header.
	sealedHeaderSize = 24 /* nonce */ + headerSize + secretbox.Overhead
	// nonceInHeaderOffset is the offset of the message nonce in the
	// header's plaintext.
	nonceInHeaderOffset = 4 + 4 + 32
	// maxMissingMessages is the maximum number of missing messages that
	// we'll keep track of.
	maxMissingMessages = 80
)

// Ratchet contains the per-contact, crypto state.
type Ratchet struct {
	MyPrivateKey        *[sntrup4591761.PrivateKeySize]byte
	MySigningPublic     *[32]byte
	TheirIdentityPublic *[32]byte
	TheirSigningPublic  *[32]byte
	TheirPublicKey      *[sntrup4591761.PublicKeySize]byte

	// Now is an optional function that will be used to get the current
	// time. If nil, time.Now is used.
	Now func() time.Time

	// rootKey gets updated by the DH ratchet.
	rootKey [32]byte
	// Header keys are used to encrypt message headers.
	sendHeaderKey, recvHeaderKey         [32]byte
	nextSendHeaderKey, nextRecvHeaderKey [32]byte
	// Chain keys are used for forward secrecy updating.
	sendChainKey, recvChainKey            [32]byte
	sendRatchetPrivate, recvRatchetPublic [32]byte
	sendCount, recvCount                  uint32
	prevSendCount                         uint32
	// ratchet is true if we will send a new ratchet value in the next message.
	ratchet bool

	// saved is a map from a header key to a map from sequence number to
	// message key.
	saved map[[32]byte]map[uint32]savedKey

	MyHalf    *[32]byte
	TheirHalf *[32]byte
	kxPrivate *[32]byte

	rand io.Reader
}

// savedKey contains a message key and timestamp for a message which has not
// been received. The timestamp comes from the message by which we learn of the
// missing message.
type savedKey struct {
	key       [32]byte
	timestamp time.Time
}

func (r *Ratchet) randBytes(buf []byte) {
	if _, err := io.ReadFull(r.rand, buf); err != nil {
		panic(err)
	}
}

func New(rand io.Reader) *Ratchet {
	r := new(Ratchet)
	r.rand = rand
	r.kxPrivate = new([32]byte)
	r.randBytes(r.kxPrivate[:])
	return r
}

type KeyExchange struct {
	Cipher [sntrup4591761.CiphertextSize]byte
	Public []byte
}

// FillKeyExchange sets elements of kx with key exchange information from the
// ratchet.
func (r *Ratchet) FillKeyExchange(kx *KeyExchange) error {
	c, k, err := sntrup4591761.Encapsulate(r.rand, r.TheirPublicKey)
	if err != nil {
		return err
	}
	pub := new([32]byte)
	curve25519.ScalarBaseMult(pub, r.kxPrivate)
	blobshare.SetNrp(32768, 16, 2)
	key, salt, err := blobshare.NewKey(k[:])
	if err != nil {
		return err
	}
	encrypted, nonce, err := blobshare.Encrypt(pub[:], key)
	if err != nil {
		return err
	}
	packed := blobshare.PackSaltNonce(salt, nonce, encrypted)

	r.MyHalf = k
	copy(kx.Cipher[:], c[:])
	kx.Public = packed

	return nil
}

// deriveKey takes an HMAC object and a label and calculates out = HMAC(k, label).
func deriveKey(out *[32]byte, label []byte, h hash.Hash) {
	h.Reset()
	h.Write(label)
	n := h.Sum(out[:0])
	if &n[0] != &out[0] {
		panic("hash function too large")
	}
}

// These constants are used as the label argument to deriveKey to derive
// independent keys from a master key.
var (
	chainKeyLabel          = []byte("chain key")
	headerKeyLabel         = []byte("header key")
	nextRecvHeaderKeyLabel = []byte("next receive header key")
	rootKeyLabel           = []byte("root key")
	rootKeyUpdateLabel     = []byte("root key update")
	sendHeaderKeyLabel     = []byte("next send header key")
	messageKeyLabel        = []byte("message key")
	chainKeyStepLabel      = []byte("chain key step")
)

// validateECDHpoint() performs a set of basic checks on the validity of a
// peer's randomly chosen ECDH point. The term "point" is slightly
// misleading, as all we are given are the x-coordinates of a point.
func validateECDHpoint(p []byte) error {
	if len(p) != 32 {
		return errors.New("ratchet: invalid ECDH point length")
	}
	pn := new(big.Int).SetBytes(inv32(p))
	min := big.NewInt(3)
	if pn.Cmp(min) == -1 {
		return errors.New("ratchet: invalid ECDH points") // too small
	}
	max := big.NewInt(0).Sub(big.NewInt(0).Exp(big.NewInt(2),
		big.NewInt(255), nil), big.NewInt(19))
	if pn.Cmp(max) != -1 {
		return errors.New("ratchet: invalid ECDH points") // too large
	}
	return nil
}

// CompleteKeyExchange takes a KeyExchange message from the other party and
// establishes the ratchet.
func (r *Ratchet) CompleteKeyExchange(kx *KeyExchange, Alice bool) error {
	k, ok := sntrup4591761.Decapsulate(&kx.Cipher, r.MyPrivateKey)
	if ok != 1 {
		return errors.New("CompleteKeyExchange: decapsulation error")
	}
	r.TheirHalf = k

	blobshare.SetNrp(32768, 16, 2)
	salt, nonce, data, err := blobshare.UnpackSaltNonce(kx.Public)
	if err != nil {
		return err
	}
	key, err := blobshare.DeriveKey(k[:], salt)
	if err != nil {
		return err
	}
	ratchetPublic, err := blobshare.Decrypt(key, nonce, data)
	if err != nil {
		return err
	}
	err = validateECDHpoint(ratchetPublic)
	if err != nil {
		return err
	}

	d := sha256.New()
	if Alice {
		d.Write(r.MyHalf[:])
		d.Write(r.TheirHalf[:])
	} else {
		d.Write(r.TheirHalf[:])
		d.Write(r.MyHalf[:])
	}
	sharedKey := d.Sum(nil)

	keyMaterial := make([]byte, 0, 32*5)
	keyMaterial = append(keyMaterial, sharedKey[:]...)
	h := hmac.New(sha256.New, keyMaterial)
	deriveKey(&r.rootKey, rootKeyLabel, h)

	if Alice {
		deriveKey(&r.recvHeaderKey, headerKeyLabel, h)
		deriveKey(&r.nextSendHeaderKey, sendHeaderKeyLabel, h)
		deriveKey(&r.nextRecvHeaderKey, nextRecvHeaderKeyLabel, h)
		deriveKey(&r.recvChainKey, chainKeyLabel, h)
		copy(r.recvRatchetPublic[:], ratchetPublic[:])
	} else {
		deriveKey(&r.sendHeaderKey, headerKeyLabel, h)
		deriveKey(&r.nextRecvHeaderKey, sendHeaderKeyLabel, h)
		deriveKey(&r.nextSendHeaderKey, nextRecvHeaderKeyLabel, h)
		deriveKey(&r.sendChainKey, chainKeyLabel, h)
		copy(r.sendRatchetPrivate[:], r.kxPrivate[:])
	}

	r.ratchet = Alice
	r.kxPrivate = nil

	return nil
}

// Encrypt acts like append() but appends an encrypted version of msg to out.
func (r *Ratchet) Encrypt(out, msg []byte) []byte {
	if r.ratchet {
		r.randBytes(r.sendRatchetPrivate[:])
		copy(r.sendHeaderKey[:], r.nextSendHeaderKey[:])

		var sharedKey, keyMaterial [32]byte
		curve25519.ScalarMult(&sharedKey, &r.sendRatchetPrivate, &r.recvRatchetPublic)
		sha := sha256.New()
		sha.Write(rootKeyUpdateLabel)
		sha.Write(r.rootKey[:])
		sha.Write(sharedKey[:])
		sha.Sum(keyMaterial[:0])
		h := hmac.New(sha256.New, keyMaterial[:])
		deriveKey(&r.rootKey, rootKeyLabel, h)
		deriveKey(&r.nextSendHeaderKey, sendHeaderKeyLabel, h)
		deriveKey(&r.sendChainKey, chainKeyLabel, h)
		r.prevSendCount, r.sendCount = r.sendCount, 0
		r.ratchet = false
	}

	h := hmac.New(sha256.New, r.sendChainKey[:])
	var messageKey [32]byte
	deriveKey(&messageKey, messageKeyLabel, h)
	deriveKey(&r.sendChainKey, chainKeyStepLabel, h)

	var sendRatchetPublic [32]byte
	curve25519.ScalarBaseMult(&sendRatchetPublic, &r.sendRatchetPrivate)
	var header [headerSize]byte
	var headerNonce, messageNonce [24]byte
	r.randBytes(headerNonce[:])
	r.randBytes(messageNonce[:])

	binary.LittleEndian.PutUint32(header[0:4], r.sendCount)
	binary.LittleEndian.PutUint32(header[4:8], r.prevSendCount)
	copy(header[8:], sendRatchetPublic[:])
	copy(header[nonceInHeaderOffset:], messageNonce[:])
	out = append(out, headerNonce[:]...)
	out = secretbox.Seal(out, header[:], &headerNonce, &r.sendHeaderKey)
	r.sendCount++
	return secretbox.Seal(out, msg, &messageNonce, &messageKey)
}

// trySavedKeys tries to decrypt ciphertext using keys saved for missing messages.
func (r *Ratchet) trySavedKeys(ciphertext []byte) ([]byte, error) {
	if len(ciphertext) < sealedHeaderSize {
		return nil, errors.New("ratchet: header too small to be valid")
	}

	sealedHeader := ciphertext[:sealedHeaderSize]
	var nonce [24]byte
	copy(nonce[:], sealedHeader)
	sealedHeader = sealedHeader[len(nonce):]

	for headerKey, messageKeys := range r.saved {
		header, ok := secretbox.Open(nil, sealedHeader, &nonce, &headerKey)
		if !ok {
			continue
		}
		if len(header) != headerSize {
			continue
		}
		msgNum := binary.LittleEndian.Uint32(header[:4])
		msgKey, ok := messageKeys[msgNum]
		if !ok {
			// This is a fairly common case: the message key might
			// not have been saved because it's the next message
			// key.
			return nil, nil
		}

		sealedMessage := ciphertext[sealedHeaderSize:]
		copy(nonce[:], header[nonceInHeaderOffset:])
		msg, ok := secretbox.Open(nil, sealedMessage, &nonce, &msgKey.key)
		if !ok {
			return nil, errors.New("ratchet: corrupt message")
		}
		delete(messageKeys, msgNum)
		if len(messageKeys) == 0 {
			delete(r.saved, headerKey)
		}
		return msg, nil
	}

	return nil, nil
}

// saveKeys takes a header key, the current chain key, a received message
// number and the expected message number and advances the chain key as needed.
// It returns the message key for given given message number and the new chain
// key. If any messages have been skipped over, it also returns savedKeys, a
// map suitable for merging with r.saved, that contains the message keys for
// the missing messages.
func (r *Ratchet) saveKeys(headerKey, recvChainKey *[32]byte, messageNum, receivedCount uint32) (provisionalChainKey, messageKey [32]byte, savedKeys map[[32]byte]map[uint32]savedKey, err error) {
	if messageNum < receivedCount {
		// This is a message from the past, but we didn't have a saved
		// key for it, which means that it's a duplicate message or we
		// expired the save key.
		err = errors.New("ratchet: duplicate message or message delayed longer than tolerance")
		return
	}

	missingMessages := messageNum - receivedCount
	if missingMessages > maxMissingMessages {
		err = errors.New("ratchet: message exceeds reordering limit")
		return
	}

	// messageKeys maps from message number to message key.
	var messageKeys map[uint32]savedKey
	var now time.Time
	if missingMessages > 0 {
		messageKeys = make(map[uint32]savedKey)
		if r.Now == nil {
			now = time.Now()
		} else {
			now = r.Now()
		}
	}

	copy(provisionalChainKey[:], recvChainKey[:])

	for n := receivedCount; n <= messageNum; n++ {
		h := hmac.New(sha256.New, provisionalChainKey[:])
		deriveKey(&messageKey, messageKeyLabel, h)
		deriveKey(&provisionalChainKey, chainKeyStepLabel, h)
		if n < messageNum {
			messageKeys[n] = savedKey{messageKey, now}
		}
	}

	if messageKeys != nil {
		savedKeys = make(map[[32]byte]map[uint32]savedKey)
		savedKeys[*headerKey] = messageKeys
	}

	return
}

// mergeSavedKeys takes a map of saved message keys from saveKeys and merges it
// into r.saved.
func (r *Ratchet) mergeSavedKeys(newKeys map[[32]byte]map[uint32]savedKey) {
	for headerKey, newMessageKeys := range newKeys {
		messageKeys, ok := r.saved[headerKey]
		if !ok {
			r.saved[headerKey] = newMessageKeys
			continue
		}

		for n, messageKey := range newMessageKeys {
			messageKeys[n] = messageKey
		}
	}
}

// isZeroKey returns true if key is all zeros.
func isZeroKey(key *[32]byte) bool {
	var x uint8
	for _, v := range key {
		x |= v
	}

	return x == 0
}

func (r *Ratchet) Decrypt(ciphertext []byte) ([]byte, error) {
	msg, err := r.trySavedKeys(ciphertext)
	if err != nil || msg != nil {
		return msg, err
	}

	sealedHeader := ciphertext[:sealedHeaderSize]
	sealedMessage := ciphertext[sealedHeaderSize:]
	var nonce [24]byte
	copy(nonce[:], sealedHeader)
	sealedHeader = sealedHeader[len(nonce):]

	header, ok := secretbox.Open(nil, sealedHeader, &nonce, &r.recvHeaderKey)
	ok = ok && !isZeroKey(&r.recvHeaderKey)
	if ok {
		if len(header) != headerSize {
			return nil, errors.New("ratchet: incorrect header size")
		}
		messageNum := binary.LittleEndian.Uint32(header[:4])
		provisionalChainKey, messageKey, savedKeys, err := r.saveKeys(&r.recvHeaderKey, &r.recvChainKey, messageNum, r.recvCount)
		if err != nil {
			return nil, err
		}

		copy(nonce[:], header[nonceInHeaderOffset:])
		msg, ok := secretbox.Open(nil, sealedMessage, &nonce, &messageKey)
		if !ok {
			return nil, errors.New("ratchet: corrupt message")
		}

		copy(r.recvChainKey[:], provisionalChainKey[:])
		r.mergeSavedKeys(savedKeys)
		r.recvCount = messageNum + 1
		return msg, nil
	}

	header, ok = secretbox.Open(nil, sealedHeader, &nonce, &r.nextRecvHeaderKey)
	if !ok {
		return nil, errors.New("ratchet: cannot decrypt")
	}
	if len(header) != headerSize {
		return nil, errors.New("ratchet: incorrect header size")
	}

	if r.ratchet {
		return nil, errors.New("ratchet: received message encrypted to next header key without ratchet flag set")
	}

	messageNum := binary.LittleEndian.Uint32(header[:4])
	prevMessageCount := binary.LittleEndian.Uint32(header[4:8])

	_, _, oldSavedKeys, err := r.saveKeys(&r.recvHeaderKey, &r.recvChainKey, prevMessageCount, r.recvCount)
	if err != nil {
		return nil, err
	}

	var dhPublic, sharedKey, rootKey, chainKey, keyMaterial [32]byte
	copy(dhPublic[:], header[8:])

	curve25519.ScalarMult(&sharedKey, &r.sendRatchetPrivate, &dhPublic)

	sha := sha256.New()
	sha.Write(rootKeyUpdateLabel)
	sha.Write(r.rootKey[:])
	sha.Write(sharedKey[:])

	var rootKeyHMAC hash.Hash

	sha.Sum(keyMaterial[:0])
	rootKeyHMAC = hmac.New(sha256.New, keyMaterial[:])
	deriveKey(&rootKey, rootKeyLabel, rootKeyHMAC)
	deriveKey(&chainKey, chainKeyLabel, rootKeyHMAC)

	provisionalChainKey, messageKey, savedKeys, err := r.saveKeys(&r.nextRecvHeaderKey, &chainKey, messageNum, 0)
	if err != nil {
		return nil, err
	}

	copy(nonce[:], header[nonceInHeaderOffset:])
	msg, ok = secretbox.Open(nil, sealedMessage, &nonce, &messageKey)
	if !ok {
		return nil, errors.New("ratchet: corrupt message")
	}

	copy(r.rootKey[:], rootKey[:])
	copy(r.recvChainKey[:], provisionalChainKey[:])
	copy(r.recvHeaderKey[:], r.nextRecvHeaderKey[:])
	deriveKey(&r.nextRecvHeaderKey, sendHeaderKeyLabel, rootKeyHMAC)
	for i := range r.sendRatchetPrivate {
		r.sendRatchetPrivate[i] = 0
	}
	copy(r.recvRatchetPublic[:], dhPublic[:])

	r.recvCount = messageNum + 1
	r.mergeSavedKeys(oldSavedKeys)
	r.mergeSavedKeys(savedKeys)
	r.ratchet = true

	return msg, nil
}

func dup32(x *[32]byte) []byte {
	if x == nil {
		return nil
	}
	ret := make([]byte, 32)
	copy(ret, x[:])
	return ret
}

func inv32(x []byte) []byte {
	if x == nil {
		return nil
	}
	ret := make([]byte, 32)
	for i := 0; i < 32; i++ {
		ret[i] = x[31-i]
	}
	return ret
}

func (r *Ratchet) Marshal(now time.Time, lifetime time.Duration) *disk.RatchetState {
	s := &disk.RatchetState{
		RootKey:            dup32(&r.rootKey),
		SendHeaderKey:      dup32(&r.sendHeaderKey),
		RecvHeaderKey:      dup32(&r.recvHeaderKey),
		NextSendHeaderKey:  dup32(&r.nextSendHeaderKey),
		NextRecvHeaderKey:  dup32(&r.nextRecvHeaderKey),
		SendChainKey:       dup32(&r.sendChainKey),
		RecvChainKey:       dup32(&r.recvChainKey),
		SendRatchetPrivate: dup32(&r.sendRatchetPrivate),
		RecvRatchetPublic:  dup32(&r.recvRatchetPublic),
		SendCount:          r.sendCount,
		RecvCount:          r.recvCount,
		PrevSendCount:      r.prevSendCount,
		Ratchet:            r.ratchet,
		Private:            dup32(r.kxPrivate),
		MyHalf:             dup32(r.MyHalf),
		TheirHalf:          dup32(r.TheirHalf),
	}

	for headerKey, messageKeys := range r.saved {
		keys := make([]disk.RatchetState_SavedKeys_MessageKey, 0, len(messageKeys))
		for messageNum, savedKey := range messageKeys {
			if now.Sub(savedKey.timestamp) > lifetime {
				continue
			}
			keys = append(keys, disk.RatchetState_SavedKeys_MessageKey{
				Num:          messageNum,
				Key:          dup32(&savedKey.key),
				CreationTime: savedKey.timestamp.Unix(),
			})
		}
		s.SavedKeys = append(s.SavedKeys, disk.RatchetState_SavedKeys{
			HeaderKey:   dup32(&headerKey),
			MessageKeys: keys,
		})
	}

	return s
}

func unmarshalKey(dst *[32]byte, src []byte) bool {
	if len(src) != 32 {
		return false
	}
	copy(dst[:], src)
	return true
}

var badSerialisedKeyLengthErr = errors.New("ratchet: bad serialised key length")

func (r *Ratchet) Unmarshal(s *disk.RatchetState) error {
	if !unmarshalKey(&r.rootKey, s.RootKey) ||
		!unmarshalKey(&r.sendHeaderKey, s.SendHeaderKey) ||
		!unmarshalKey(&r.recvHeaderKey, s.RecvHeaderKey) ||
		!unmarshalKey(&r.nextSendHeaderKey, s.NextSendHeaderKey) ||
		!unmarshalKey(&r.nextRecvHeaderKey, s.NextRecvHeaderKey) ||
		!unmarshalKey(&r.sendChainKey, s.SendChainKey) ||
		!unmarshalKey(&r.recvChainKey, s.RecvChainKey) ||
		!unmarshalKey(&r.sendRatchetPrivate, s.SendRatchetPrivate) ||
		!unmarshalKey(&r.recvRatchetPublic, s.RecvRatchetPublic) {
		return badSerialisedKeyLengthErr
	}

	r.sendCount = s.SendCount
	r.recvCount = s.RecvCount
	r.prevSendCount = s.PrevSendCount
	r.ratchet = s.Ratchet

	if len(s.Private) > 0 {
		if !unmarshalKey(r.kxPrivate, s.Private) {
			return badSerialisedKeyLengthErr
		}
	} else {
		r.kxPrivate = nil
	}
	if len(s.MyHalf) > 0 {
		if r.MyHalf == nil {
			r.MyHalf = new([32]byte)
		}
		if !unmarshalKey(r.MyHalf, s.MyHalf) {
			return badSerialisedKeyLengthErr
		}
	} else {
		r.MyHalf = nil
	}
	if len(s.TheirHalf) > 0 {
		if r.TheirHalf == nil {
			r.TheirHalf = new([32]byte)
		}
		if !unmarshalKey(r.TheirHalf, s.TheirHalf) {
			return badSerialisedKeyLengthErr
		}
	} else {
		r.TheirHalf = nil
	}

	for _, saved := range s.SavedKeys {
		var headerKey [32]byte
		if !unmarshalKey(&headerKey, saved.HeaderKey) {
			return badSerialisedKeyLengthErr
		}
		messageKeys := make(map[uint32]savedKey)
		for _, messageKey := range saved.MessageKeys {
			var savedKey savedKey
			if !unmarshalKey(&savedKey.key, messageKey.Key) {
				return badSerialisedKeyLengthErr
			}
			savedKey.timestamp = time.Unix(messageKey.CreationTime, 0)
			messageKeys[messageKey.Num] = savedKey
		}

		r.saved[headerKey] = messageKeys
	}

	return nil
}
