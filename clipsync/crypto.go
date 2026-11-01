// Copyright (c) 2023 Tiago Melo. All rights reserved.
// Use of this source code is governed by the MIT License that can be found in
// the LICENSE file.

package clipsync

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/hkdf"
	"crypto/rand"
	"crypto/sha256"
	"encoding/binary"
	"errors"
	"fmt"
)

const (
	// keySize is the AES-256 key size in bytes.
	keySize = 32

	// saltSize is the size of the per-connection salt exchanged during the
	// handshake.
	saltSize = 16

	// keyInfo domain-separates keys derived for this protocol from keys used
	// by other applications that happen to use the same token.
	keyInfo = "github.com/edickens09/go-clipboard/clipsync/v2"
)

var (
	// ErrMissingToken is returned when a Syncer is used without a token.
	ErrMissingToken = errors.New("a non-empty token is required")

	// ErrDecrypt is returned when a frame cannot be authenticated or
	// decrypted.
	ErrDecrypt = errors.New("decryption failed")
)

// cipherState encrypts and authenticates protocol frames with AES-256-GCM.
// sendSeq and recvSeq are implicit per-direction frame counters. They are
// bound into the AEAD's additional authenticated data, so a replayed frame is
// rejected even within the same session.
type cipherState struct {
	aead    cipher.AEAD
	sendSeq uint64
	recvSeq uint64
}

// newCipherState derives a per-connection AES-256 key from token and salt.
func newCipherState(token string, salt []byte) (*cipherState, error) {
	if token == "" {
		return nil, ErrMissingToken
	}
	if len(salt) != saltSize {
		return nil, fmt.Errorf("invalid salt length: %d", len(salt))
	}

	key, err := hkdf.Key(sha256.New, []byte(token), salt, keyInfo, keySize)
	if err != nil {
		return nil, fmt.Errorf("deriving key: %w", err)
	}
	defer clear(key)

	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, fmt.Errorf("creating cipher: %w", err)
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		return nil, fmt.Errorf("creating GCM: %w", err)
	}
	return &cipherState{aead: aead}, nil
}

// seal encrypts plaintext and prepends the random nonce.
func (c *cipherState) seal(plaintext []byte) ([]byte, error) {
	nonce := make([]byte, c.aead.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return nil, fmt.Errorf("generating nonce: %w", err)
	}

	sealed := c.aead.Seal(nil, nonce, plaintext, sequenceAAD(c.sendSeq))
	c.sendSeq++
	payload := make([]byte, 0, len(nonce)+len(sealed))
	payload = append(payload, nonce...)
	payload = append(payload, sealed...)
	return payload, nil
}

// open decrypts and authenticates payload, which must start with a nonce.
func (c *cipherState) open(payload []byte) ([]byte, error) {
	nonceSize := c.aead.NonceSize()
	if len(payload) < nonceSize+c.aead.Overhead() {
		return nil, fmt.Errorf("%w: ciphertext is too short", ErrDecrypt)
	}

	nonce := payload[:nonceSize]
	ciphertext := payload[nonceSize:]
	plaintext, err := c.aead.Open(nil, nonce, ciphertext, sequenceAAD(c.recvSeq))
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrDecrypt, err)
	}
	c.recvSeq++
	return plaintext, nil
}

// sequenceAAD binds a frame to its position in the stream.
func sequenceAAD(seq uint64) []byte {
	aad := make([]byte, len(keyInfo)+8)
	copy(aad, keyInfo)
	binary.BigEndian.PutUint64(aad[len(keyInfo):], seq)
	return aad
}
