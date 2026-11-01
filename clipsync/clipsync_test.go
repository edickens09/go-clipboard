// Copyright (c) 2023 Tiago Melo. All rights reserved.
// Use of this source code is governed by the MIT License that can be found in
// the LICENSE file.

package clipsync

import (
	"bytes"
	"context"
	"encoding/binary"
	"io"
	"log"
	"net"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestWriteReadMessage(t *testing.T) {
	var buf bytes.Buffer
	want := message{Type: msgClipboard, Text: "hello, 世界"}

	require.NoError(t, writeMessage(&buf, want))

	got, err := readMessage(&buf)
	require.NoError(t, err)
	require.Equal(t, want, got)
}

func TestReadMessageRejectsInvalidFrames(t *testing.T) {
	t.Run("empty frame", func(t *testing.T) {
		var header [4]byte
		_, err := readMessage(bytes.NewReader(header[:]))
		require.Error(t, err)
		require.Contains(t, err.Error(), "empty message")
	})

	t.Run("oversized frame", func(t *testing.T) {
		var header [4]byte
		binary.BigEndian.PutUint32(header[:], uint32(maxFrameSize+1))
		_, err := readMessage(bytes.NewReader(header[:]))
		require.Error(t, err)
		require.Contains(t, err.Error(), "too large")
	})
}

func TestNewAppliesDefaults(t *testing.T) {
	s := New(Config{})

	require.NotNil(t, s.cfg.Clipboard)
	require.Equal(t, defaultPollInterval, s.cfg.PollInterval)
	require.NotNil(t, s.cfg.Logger)
}

func TestAuthorized(t *testing.T) {
	s := New(Config{Token: "secret"})
	require.True(t, s.authorized("secret"))
	require.False(t, s.authorized("wrong"))

	require.True(t, New(Config{}).authorized("anything"))
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
