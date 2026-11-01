// Copyright (c) 2023 Tiago Melo. All rights reserved.
// Use of this source code is governed by the MIT License that can be found in
// the LICENSE file.

package clipsync

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"io"
	"log"
	"net"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestEncryptedMessageRoundTrip(t *testing.T) {
	cs := newTestCipher(t, "secret")

	var buf bytes.Buffer
	want := message{Type: msgClipboard, Text: "hello, 世界"}

	require.NoError(t, writeEncryptedMessage(&buf, cs, want))

	got, err := readEncryptedMessage(&buf, cs)
	require.NoError(t, err)
	require.Equal(t, want, got)
}

func TestReadFrameRejectsInvalidFrames(t *testing.T) {
	t.Run("empty frame", func(t *testing.T) {
		var header [4]byte
		_, err := readFrame(bytes.NewReader(header[:]))
		require.Error(t, err)
		require.Contains(t, err.Error(), "empty message")
	})

	t.Run("oversized frame", func(t *testing.T) {
		var header [4]byte
		binary.BigEndian.PutUint32(header[:], uint32(maxFrameSize+1))
		_, err := readFrame(bytes.NewReader(header[:]))
		require.Error(t, err)
		require.Contains(t, err.Error(), "too large")
	})
}

func TestCipherRoundTrip(t *testing.T) {
	cs := newTestCipher(t, "secret")
	plaintext := []byte("some clipboard text")

	first, err := cs.seal(plaintext)
	require.NoError(t, err)
	second, err := cs.seal(plaintext)
	require.NoError(t, err)
	require.NotEqual(t, first, second, "each frame must use a fresh nonce")

	got, err := cs.open(first)
	require.NoError(t, err)
	require.Equal(t, plaintext, got)
}

func TestCipherRejectsWrongToken(t *testing.T) {
	sealer := newTestCipher(t, "right-token")
	opener := newTestCipher(t, "wrong-token")

	payload, err := sealer.seal([]byte("secret clipboard text"))
	require.NoError(t, err)

	_, err = opener.open(payload)
	require.ErrorIs(t, err, ErrDecrypt)
}

func TestCipherRejectsTamperedPayload(t *testing.T) {
	cs := newTestCipher(t, "secret")

	payload, err := cs.seal([]byte("secret clipboard text"))
	require.NoError(t, err)
	payload[len(payload)-1] ^= 0xff

	_, err = cs.open(payload)
	require.ErrorIs(t, err, ErrDecrypt)
}

func TestCipherRejectsReplay(t *testing.T) {
	cs := newTestCipher(t, "secret")

	payload, err := cs.seal([]byte("secret clipboard text"))
	require.NoError(t, err)

	_, err = cs.open(payload)
	require.NoError(t, err)

	// The same frame is no longer valid at the next expected position.
	_, err = cs.open(payload)
	require.ErrorIs(t, err, ErrDecrypt)
}

func TestCipherRequiresToken(t *testing.T) {
	_, err := newCipherState("", bytes.Repeat([]byte{1}, saltSize))
	require.ErrorIs(t, err, ErrMissingToken)
}

func TestCipherRejectsInvalidSalt(t *testing.T) {
	_, err := newCipherState("secret", []byte{1, 2, 3})
	require.Error(t, err)
	require.Contains(t, err.Error(), "invalid salt length")
}

func TestNewAppliesDefaults(t *testing.T) {
	s := New(Config{})

	require.NotNil(t, s.cfg.Clipboard)
	require.Equal(t, defaultPollInterval, s.cfg.PollInterval)
	require.NotNil(t, s.cfg.Logger)
}

func TestServeRequiresToken(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	t.Cleanup(func() { _ = ln.Close() })

	s := New(Config{Clipboard: &fakeClipboard{}})
	require.ErrorIs(t, s.Serve(context.Background(), ln), ErrMissingToken)
}

func TestConnectRequiresToken(t *testing.T) {
	s := New(Config{Clipboard: &fakeClipboard{}})
	require.ErrorIs(t, s.Connect(context.Background(), "127.0.0.1:1"), ErrMissingToken)
}

func TestHandshakeDoesNotSendToken(t *testing.T) {
	const token = "super-secret-token"

	clientConn, serverConn := net.Pipe()
	t.Cleanup(func() {
		_ = clientConn.Close()
		_ = serverConn.Close()
	})

	client := New(Config{
		Clipboard:   &fakeClipboard{},
		Token:       token,
		SyncInitial: false,
		Logger:      log.New(io.Discard, "", 0),
	})

	type handshakeResult struct {
		cs  *cipherState
		err error
	}
	done := make(chan handshakeResult, 1)
	go func() {
		_, _, cs, err := client.handshakeClient(clientConn)
		done <- handshakeResult{cs: cs, err: err}
	}()

	raw, err := readFrame(serverConn)
	require.NoError(t, err)
	require.NotContains(t, string(raw), token)

	var hello message
	require.NoError(t, json.Unmarshal(raw, &hello))
	require.Equal(t, msgHello, hello.Type)
	require.Equal(t, protocolVersion, hello.Version)
	require.Len(t, hello.Salt, saltSize)

	server, err := newCipherState(token, hello.Salt)
	require.NoError(t, err)
	require.NoError(t, writeEncryptedMessage(serverConn, server, message{Type: msgHello, Version: protocolVersion}))

	auth, err := readEncryptedMessage(serverConn, server)
	require.NoError(t, err)
	require.Equal(t, msgAuth, auth.Type)

	result := <-done
	require.NoError(t, result.err)
	require.NotNil(t, result.cs)
}

func TestSyncerSyncsClipboardBothWays(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	t.Cleanup(func() { _ = ln.Close() })

	serverClip := &fakeClipboard{}
	clientClip := &fakeClipboard{text: "initial-from-client"}

	server := New(Config{
		Clipboard:    serverClip,
		PollInterval: 10 * time.Millisecond,
		Token:        "secret",
		SyncInitial:  true,
		Logger:       log.New(io.Discard, "", 0),
	})
	client := New(Config{
		Clipboard:    clientClip,
		PollInterval: 10 * time.Millisecond,
		Token:        "secret",
		SyncInitial:  true,
		Logger:       log.New(io.Discard, "", 0),
	})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	serveErr := make(chan error, 1)
	go func() { serveErr <- server.Serve(ctx, ln) }()

	connectErr := make(chan error, 1)
	go func() { connectErr <- client.Connect(ctx, ln.Addr().String()) }()

	// The connecting peer's clipboard is sent during the handshake.
	waitFor(t, 2*time.Second, func() bool {
		return serverClip.get() == "initial-from-client"
	})

	// Changes made on the listener reach the connector.
	serverClip.set("from-server")
	waitFor(t, 2*time.Second, func() bool {
		return clientClip.get() == "from-server"
	})

	// Changes made on the connector reach the listener.
	clientClip.set("from-client")
	waitFor(t, 2*time.Second, func() bool {
		return serverClip.get() == "from-client"
	})

	// Once both sides agree, the poll loops must not keep echoing updates.
	time.Sleep(100 * time.Millisecond)
	serverCopies := serverClip.copyCount()
	clientCopies := clientClip.copyCount()
	time.Sleep(100 * time.Millisecond)
	require.Equal(t, serverCopies, serverClip.copyCount())
	require.Equal(t, clientCopies, clientClip.copyCount())

	cancel()
	select {
	case err := <-serveErr:
		require.ErrorIs(t, err, context.Canceled)
	case <-time.After(2 * time.Second):
		t.Fatal("server did not stop")
	}
	select {
	case err := <-connectErr:
		require.ErrorIs(t, err, context.Canceled)
	case <-time.After(2 * time.Second):
		t.Fatal("client did not stop")
	}
}

func TestConnectRejectsInvalidToken(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	t.Cleanup(func() { _ = ln.Close() })

	server := New(Config{
		Clipboard:    &fakeClipboard{},
		PollInterval: time.Second,
		Token:        "secret",
		Logger:       log.New(io.Discard, "", 0),
	})
	client := New(Config{
		Clipboard:    &fakeClipboard{},
		PollInterval: time.Second,
		Token:        "wrong",
		Logger:       log.New(io.Discard, "", 0),
	})

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	go func() {
		conn, err := ln.Accept()
		if err != nil {
			return
		}
		server.handleConn(ctx, conn)
	}()

	_, err = client.connectOnce(ctx, ln.Addr().String())
	require.ErrorIs(t, err, ErrUnauthorized)
}

func TestServeStopsWhenContextIsCanceled(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	t.Cleanup(func() { _ = ln.Close() })

	s := New(Config{
		Clipboard: &fakeClipboard{},
		Token:     "secret",
		Logger:    log.New(io.Discard, "", 0),
	})

	ctx, cancel := context.WithCancel(context.Background())
	errCh := make(chan error, 1)
	go func() { errCh <- s.Serve(ctx, ln) }()

	cancel()

	select {
	case err := <-errCh:
		require.ErrorIs(t, err, context.Canceled)
	case <-time.After(2 * time.Second):
		t.Fatal("Serve did not stop")
	}
}

func newTestCipher(t *testing.T, token string) *cipherState {
	t.Helper()
	cs, err := newCipherState(token, bytes.Repeat([]byte{0x42}, saltSize))
	require.NoError(t, err)
	return cs
}

func waitFor(t *testing.T, timeout time.Duration, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("condition was not met within %s", timeout)
}

// fakeClipboard is an in-memory clipboard used by the tests.
type fakeClipboard struct {
	mu       sync.Mutex
	text     string
	copyErr  error
	pasteErr error
	copies   int
}

func (f *fakeClipboard) CopyText(s string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.copyErr != nil {
		return f.copyErr
	}
	f.text = s
	f.copies++
	return nil
}

func (f *fakeClipboard) PasteText() (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.pasteErr != nil {
		return "", f.pasteErr
	}
	return f.text, nil
}

func (f *fakeClipboard) set(s string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.text = s
}

func (f *fakeClipboard) get() string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.text
}

func (f *fakeClipboard) copyCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.copies
}
