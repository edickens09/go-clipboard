// Copyright (c) 2023 Tiago Melo. All rights reserved.
// Use of this source code is governed by the MIT License that can be found in
// the LICENSE file.

package clipsync

import (
	"crypto/rand"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"time"
)

const (
	// protocolVersion is bumped whenever the wire format changes in an
	// incompatible way.
	protocolVersion = 2

	// maxFrameSize limits the size of a single frame. Messages larger than
	// this are rejected instead of exhausting memory.
	maxFrameSize = 16 << 20 // 16 MiB

	// maxClipboardSize leaves room for the JSON envelope and the AEAD
	// overhead so that a clipboard value accepted by the sync loop always
	// fits in a frame.
	maxClipboardSize = maxFrameSize - (64 << 10)

	// handshakeTimeout bounds how long a peer may take to complete the
	// handshake.
	handshakeTimeout = 10 * time.Second
)

const (
	msgHello     = "hello"
	msgAuth      = "auth"
	msgClipboard = "clipboard"
	msgError     = "error"
)

const (
	codeUnauthorized = "unauthorized"
	codeProtocol     = "protocol"
)

// message is the JSON payload carried by every frame. The hello frame is sent
// in the clear and only carries a version and a random salt; every other frame
// is encrypted.
type message struct {
	Type    string `json:"type"`
	Version int    `json:"version,omitempty"`
	Salt    []byte `json:"salt,omitempty"`
	Text    string `json:"text,omitempty"`
	HasText bool   `json:"has_text,omitempty"`
	Code    string `json:"code,omitempty"`
}

// writeFrame writes a length-prefixed payload.
func writeFrame(w io.Writer, payload []byte) error {
	if len(payload) > maxFrameSize {
		return fmt.Errorf("message too large: %d bytes", len(payload))
	}

	var header [4]byte
	binary.BigEndian.PutUint32(header[:], uint32(len(payload)))
	if _, err := w.Write(header[:]); err != nil {
		return fmt.Errorf("writing message header: %w", err)
	}
	if _, err := w.Write(payload); err != nil {
		return fmt.Errorf("writing message payload: %w", err)
	}
	return nil
}

// readFrame reads a length-prefixed payload.
func readFrame(r io.Reader) ([]byte, error) {
	var header [4]byte
	if _, err := io.ReadFull(r, header[:]); err != nil {
		return nil, fmt.Errorf("reading message header: %w", err)
	}

	size := binary.BigEndian.Uint32(header[:])
	if size == 0 {
		return nil, errors.New("empty message")
	}
	if size > maxFrameSize {
		return nil, fmt.Errorf("message too large: %d bytes", size)
	}

	payload := make([]byte, size)
	if _, err := io.ReadFull(r, payload); err != nil {
		return nil, fmt.Errorf("reading message payload: %w", err)
	}
	return payload, nil
}

// writePlainMessage writes an unencrypted JSON message. Only the opening
// hello frame uses this.
func writePlainMessage(w io.Writer, msg message) error {
	payload, err := json.Marshal(msg)
	if err != nil {
		return fmt.Errorf("encoding message: %w", err)
	}
	return writeFrame(w, payload)
}

// readPlainMessage reads an unencrypted JSON message. Only the opening hello
// frame uses this.
func readPlainMessage(r io.Reader) (message, error) {
	payload, err := readFrame(r)
	if err != nil {
		return message{}, err
	}
	return decodeMessage(payload)
}

// writeEncryptedMessage encrypts and writes a JSON message.
func writeEncryptedMessage(w io.Writer, cs *cipherState, msg message) error {
	payload, err := json.Marshal(msg)
	if err != nil {
		return fmt.Errorf("encoding message: %w", err)
	}
	sealed, err := cs.seal(payload)
	if err != nil {
		return err
	}
	return writeFrame(w, sealed)
}

// readEncryptedMessage reads and decrypts a JSON message.
func readEncryptedMessage(r io.Reader, cs *cipherState) (message, error) {
	payload, err := readFrame(r)
	if err != nil {
		return message{}, err
	}
	plaintext, err := cs.open(payload)
	if err != nil {
		return message{}, err
	}
	return decodeMessage(plaintext)
}

func decodeMessage(payload []byte) (message, error) {
	var msg message
	if err := json.Unmarshal(payload, &msg); err != nil {
		return message{}, fmt.Errorf("decoding message: %w", err)
	}
	return msg, nil
}

// handshakeClient performs the client side of the encrypted handshake:
//
//  1. send a clear hello containing a random salt,
//  2. derive a per-session AES-256-GCM key from the token and salt,
//  3. read the encrypted hello response, and
//  4. send an encrypted auth frame with the optional initial clipboard.
//
// The token itself is never sent over the wire.
func (s *Syncer) handshakeClient(conn net.Conn) (string, bool, *cipherState, error) {
	if err := conn.SetDeadline(time.Now().Add(handshakeTimeout)); err != nil {
		return "", false, nil, fmt.Errorf("setting handshake deadline: %w", err)
	}
	defer func() { _ = conn.SetDeadline(time.Time{}) }()

	salt := make([]byte, saltSize)
	if _, err := rand.Read(salt); err != nil {
		return "", false, nil, fmt.Errorf("generating handshake salt: %w", err)
	}
	cs, err := newCipherState(s.cfg.Token, salt)
	if err != nil {
		return "", false, nil, err
	}

	if err := writePlainMessage(conn, message{Type: msgHello, Version: protocolVersion, Salt: salt}); err != nil {
		return "", false, nil, fmt.Errorf("sending handshake: %w", err)
	}

	resp, err := readEncryptedMessage(conn, cs)
	if err != nil {
		if errors.Is(err, ErrDecrypt) {
			return "", false, nil, fmt.Errorf("%w: peer could not authenticate (token mismatch?)", ErrUnauthorized)
		}
		return "", false, nil, fmt.Errorf("reading handshake response: %w", err)
	}

	switch resp.Type {
	case msgError:
		return "", false, nil, handshakeError(resp)
	case msgHello:
		if resp.Version != protocolVersion {
			return "", false, nil, fmt.Errorf("%w: peer speaks version %d, local version is %d", ErrProtocol, resp.Version, protocolVersion)
		}
	default:
		return "", false, nil, fmt.Errorf("%w: unexpected handshake response %q", ErrProtocol, resp.Type)
	}

	auth := message{Type: msgAuth}
	if s.cfg.SyncInitial {
		text, err := s.cfg.Clipboard.PasteText()
		switch {
		case err != nil:
			s.logf("reading local clipboard for initial sync: %v", err)
		case len(text) > maxClipboardSize:
			s.logf("skipping initial sync: clipboard is %d bytes, limit is %d", len(text), maxClipboardSize)
		default:
			auth.Text = text
			auth.HasText = true
		}
	}
	if err := writeEncryptedMessage(conn, cs, auth); err != nil {
		return "", false, nil, fmt.Errorf("sending authentication: %w", err)
	}
	return auth.Text, auth.HasText, cs, nil
}

// handshakeServer performs the server side of the encrypted handshake. A
// successful decryption of the auth frame authenticates the client.
func (s *Syncer) handshakeServer(conn net.Conn) (string, bool, *cipherState, error) {
	if err := conn.SetDeadline(time.Now().Add(handshakeTimeout)); err != nil {
		return "", false, nil, fmt.Errorf("setting handshake deadline: %w", err)
	}
	defer func() { _ = conn.SetDeadline(time.Time{}) }()

	hello, err := readPlainMessage(conn)
	if err != nil {
		return "", false, nil, fmt.Errorf("reading handshake: %w", err)
	}
	if hello.Type != msgHello {
		return "", false, nil, fmt.Errorf("%w: unexpected message %q", ErrProtocol, hello.Type)
	}
	if hello.Version != protocolVersion {
		return "", false, nil, fmt.Errorf("%w: peer speaks version %d, local version is %d", ErrProtocol, hello.Version, protocolVersion)
	}

	cs, err := newCipherState(s.cfg.Token, hello.Salt)
	if err != nil {
		return "", false, nil, err
	}
	if err := writeEncryptedMessage(conn, cs, message{Type: msgHello, Version: protocolVersion}); err != nil {
		return "", false, nil, fmt.Errorf("sending handshake response: %w", err)
	}

	auth, err := readEncryptedMessage(conn, cs)
	if err != nil {
		if errors.Is(err, ErrDecrypt) {
			_ = writeEncryptedMessage(conn, cs, message{Type: msgError, Code: codeUnauthorized, Text: "authentication failed"})
			return "", false, nil, ErrUnauthorized
		}
		return "", false, nil, fmt.Errorf("reading authentication: %w", err)
	}
	if auth.Type != msgAuth {
		_ = writeEncryptedMessage(conn, cs, message{Type: msgError, Code: codeProtocol, Text: fmt.Sprintf("expected %q, got %q", msgAuth, auth.Type)})
		return "", false, nil, fmt.Errorf("%w: unexpected message %q", ErrProtocol, auth.Type)
	}
	return auth.Text, auth.HasText, cs, nil
}

// handshakeError converts a rejection frame into a sentinel error.
func handshakeError(resp message) error {
	switch resp.Code {
	case codeUnauthorized:
		return fmt.Errorf("%w: %s", ErrUnauthorized, resp.Text)
	case codeProtocol:
		return fmt.Errorf("%w: %s", ErrProtocol, resp.Text)
	default:
		return fmt.Errorf("peer rejected connection: %s", resp.Text)
	}
}
