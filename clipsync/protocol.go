// Copyright (c) 2023 Tiago Melo. All rights reserved.
// Use of this source code is governed by the MIT License that can be found in
// the LICENSE file.

package clipsync

import (
	"crypto/sha256"
	"crypto/subtle"
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
	protocolVersion = 1

	// maxFrameSize limits the size of a single clipboard message. Clipboards
	// larger than this are rejected instead of exhausting memory.
	maxFrameSize = 16 << 20 // 16 MiB

	// maxClipboardSize leaves room for the JSON envelope so that a clipboard
	// value accepted by the sync loop always fits in a frame.
	maxClipboardSize = maxFrameSize - (64 << 10)

	// handshakeTimeout bounds how long a peer may take to complete the
	// authentication handshake.
	handshakeTimeout = 10 * time.Second
)

const (
	msgHello     = "hello"
	msgClipboard = "clipboard"
	msgError     = "error"
)

const (
	codeUnauthorized = "unauthorized"
	codeProtocol     = "protocol"
)

// message is the JSON payload carried by every frame.
type message struct {
	Type    string `json:"type"`
	Version int    `json:"version,omitempty"`
	Token   string `json:"token,omitempty"`
	Text    string `json:"text,omitempty"`
	HasText bool   `json:"has_text,omitempty"`
	Code    string `json:"code,omitempty"`
}

// writeMessage encodes msg as a length-prefixed JSON frame.
func writeMessage(w io.Writer, msg message) error {
	payload, err := json.Marshal(msg)
	if err != nil {
		return fmt.Errorf("encoding message: %w", err)
	}
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

// readMessage reads a single length-prefixed JSON frame.
func readMessage(r io.Reader) (message, error) {
	var header [4]byte
	if _, err := io.ReadFull(r, header[:]); err != nil {
		return message{}, fmt.Errorf("reading message header: %w", err)
	}

	size := binary.BigEndian.Uint32(header[:])
	if size == 0 {
		return message{}, errors.New("empty message")
	}
	if size > maxFrameSize {
		return message{}, fmt.Errorf("message too large: %d bytes", size)
	}

	payload := make([]byte, size)
	if _, err := io.ReadFull(r, payload); err != nil {
		return message{}, fmt.Errorf("reading message payload: %w", err)
	}

	var msg message
	if err := json.Unmarshal(payload, &msg); err != nil {
		return message{}, fmt.Errorf("decoding message: %w", err)
	}
	return msg, nil
}

// handshakeClient sends the opening hello frame and validates the peer's
// response. It returns the clipboard text sent to the peer, if any.
func (s *Syncer) handshakeClient(conn net.Conn) (string, bool, error) {
	if err := conn.SetDeadline(time.Now().Add(handshakeTimeout)); err != nil {
		return "", false, fmt.Errorf("setting handshake deadline: %w", err)
	}
	defer func() { _ = conn.SetDeadline(time.Time{}) }()

	hello := message{Type: msgHello, Version: protocolVersion, Token: s.cfg.Token}
	if s.cfg.SyncInitial {
		text, err := s.cfg.Clipboard.PasteText()
		switch {
		case err != nil:
			s.logf("reading local clipboard for initial sync: %v", err)
		case len(text) > maxClipboardSize:
			s.logf("skipping initial sync: clipboard is %d bytes, limit is %d", len(text), maxClipboardSize)
		default:
			hello.Text = text
			hello.HasText = true
		}
	}

	if err := writeMessage(conn, hello); err != nil {
		return "", false, fmt.Errorf("sending handshake: %w", err)
	}

	resp, err := readMessage(conn)
	if err != nil {
		return "", false, fmt.Errorf("reading handshake response: %w", err)
	}

	switch resp.Type {
	case msgError:
		return "", false, handshakeError(resp)
	case msgHello:
		if resp.Version != protocolVersion {
			return "", false, fmt.Errorf("%w: peer speaks version %d, local version is %d", ErrProtocol, resp.Version, protocolVersion)
		}
	default:
		return "", false, fmt.Errorf("%w: unexpected handshake response %q", ErrProtocol, resp.Type)
	}

	return hello.Text, hello.HasText, nil
}

// handshakeServer reads the peer's hello frame, validates it and acknowledges
// the connection. It returns the clipboard text sent by the peer, if any.
func (s *Syncer) handshakeServer(conn net.Conn) (string, bool, error) {
	if err := conn.SetDeadline(time.Now().Add(handshakeTimeout)); err != nil {
		return "", false, fmt.Errorf("setting handshake deadline: %w", err)
	}
	defer func() { _ = conn.SetDeadline(time.Time{}) }()

	hello, err := readMessage(conn)
	if err != nil {
		return "", false, fmt.Errorf("reading handshake: %w", err)
	}

	if hello.Type != msgHello {
		reject(conn, codeProtocol, fmt.Sprintf("expected %q, got %q", msgHello, hello.Type))
		return "", false, fmt.Errorf("%w: unexpected message %q", ErrProtocol, hello.Type)
	}
	if hello.Version != protocolVersion {
		reject(conn, codeProtocol, fmt.Sprintf("peer speaks version %d, local version is %d", hello.Version, protocolVersion))
		return "", false, fmt.Errorf("%w: peer speaks version %d, local version is %d", ErrProtocol, hello.Version, protocolVersion)
	}
	if !s.authorized(hello.Token) {
		reject(conn, codeUnauthorized, "invalid token")
		return "", false, ErrUnauthorized
	}

	if err := writeMessage(conn, message{Type: msgHello, Version: protocolVersion}); err != nil {
		return "", false, fmt.Errorf("sending handshake response: %w", err)
	}
	return hello.Text, hello.HasText, nil
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

// reject sends a best-effort rejection frame to the peer.
func reject(conn net.Conn, code, reason string) {
	_ = writeMessage(conn, message{Type: msgError, Code: code, Text: reason})
}

// authorized reports whether token matches the configured shared secret. An
// empty configured token disables authentication.
func (s *Syncer) authorized(token string) bool {
	if s.cfg.Token == "" {
		return true
	}
	want := sha256.Sum256([]byte(s.cfg.Token))
	got := sha256.Sum256([]byte(token))
	return subtle.ConstantTimeCompare(got[:], want[:]) == 1
}
