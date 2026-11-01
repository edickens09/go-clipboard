// Copyright (c) 2023 Tiago Melo. All rights reserved.
// Use of this source code is governed by the MIT License that can be found in
// the LICENSE file.

package clipsync

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"sync"
	"time"

	"github.com/edickens09/go-clipboard/clipboard"
)

const (
	// defaultPollInterval is how often the local clipboard is checked when
	// Config.PollInterval is not set.
	defaultPollInterval = 500 * time.Millisecond

	// minReconnectDelay and maxReconnectDelay bound the exponential backoff
	// used by Connect.
	minReconnectDelay = time.Second
	maxReconnectDelay = 30 * time.Second
)

var (
	// ErrUnauthorized is returned when the peer rejects the shared token.
	ErrUnauthorized = errors.New("unauthorized")

	// ErrProtocol is returned when the peers cannot agree on the wire protocol.
	ErrProtocol = errors.New("protocol error")
)

// Config controls a Syncer.
type Config struct {
	// Clipboard is the local clipboard implementation. When nil,
	// clipboard.New() is used.
	Clipboard clipboard.Clipboard

	// PollInterval is how often the local clipboard is checked for changes.
	// When zero or negative, 500ms is used.
	PollInterval time.Duration

	// Token is a shared secret that peers must present to establish a
	// session. When empty, authentication is disabled.
	Token string

	// SyncInitial makes the connecting peer send its current clipboard to the
	// listening peer when a session starts. This also makes the peers converge
	// again after a connection was lost.
	SyncInitial bool

	// Logger receives connection and synchronization messages. When nil, log
	// output is discarded.
	Logger *log.Logger
}

// Syncer synchronizes the local clipboard with a remote peer.
type Syncer struct {
	cfg Config

	// writeMu serializes writes to the local clipboard so that several peers
	// cannot modify it at the same time.
	writeMu sync.Mutex
}

// New creates a Syncer from cfg, applying defaults for unset fields.
func New(cfg Config) *Syncer {
	if cfg.Clipboard == nil {
		cfg.Clipboard = clipboard.New()
	}
	if cfg.PollInterval <= 0 {
		cfg.PollInterval = defaultPollInterval
	}
	if cfg.Logger == nil {
		cfg.Logger = log.New(io.Discard, "", 0)
	}
	return &Syncer{cfg: cfg}
}

// ListenAndServe listens on the given TCP address and serves clipboard
// synchronization sessions until ctx is canceled.
func (s *Syncer) ListenAndServe(ctx context.Context, addr string) error {
	var lc net.ListenConfig
	ln, err := lc.Listen(ctx, "tcp", addr)
	if err != nil {
		return fmt.Errorf("listening on %s: %w", addr, err)
	}
	return s.Serve(ctx, ln)
}

// Serve accepts connections from ln and serves a synchronization session for
// each one until ctx is canceled. It closes ln before returning.
func (s *Syncer) Serve(ctx context.Context, ln net.Listener) error {
	defer func() { _ = ln.Close() }()

	done := make(chan struct{})
	defer close(done)
	go func() {
		select {
		case <-ctx.Done():
			_ = ln.Close()
		case <-done:
		}
	}()

	s.logf("listening on %s", ln.Addr())
	for {
		conn, err := ln.Accept()
		if err != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			return fmt.Errorf("accepting connection: %w", err)
		}
		go s.handleConn(ctx, conn)
	}
}

// Connect dials addr and keeps the clipboard synchronized until ctx is
// canceled. If the connection is lost, it reconnects with exponential backoff.
// Authentication and protocol errors are permanent and are returned
// immediately.
func (s *Syncer) Connect(ctx context.Context, addr string) error {
	delay := minReconnectDelay
	for {
		if ctx.Err() != nil {
			return ctx.Err()
		}

		connected, err := s.connectOnce(ctx, addr)
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if errors.Is(err, ErrUnauthorized) || errors.Is(err, ErrProtocol) {
			return err
		}
		if err == nil {
			err = errors.New("connection closed")
		}

		s.logf("connection to %s lost: %v; retrying in %s", addr, err, delay)
		if connected {
			delay = minReconnectDelay
		}

		timer := time.NewTimer(delay)
		select {
		case <-ctx.Done():
			timer.Stop()
			return ctx.Err()
		case <-timer.C:
		}

		if !connected {
			delay *= 2
			if delay > maxReconnectDelay {
				delay = maxReconnectDelay
			}
		}
	}
}

// connectOnce performs a single connection attempt and session. The returned
// boolean reports whether the handshake succeeded.
func (s *Syncer) connectOnce(ctx context.Context, addr string) (bool, error) {
	var d net.Dialer
	conn, err := d.DialContext(ctx, "tcp", addr)
	if err != nil {
		return false, fmt.Errorf("dialing %s: %w", addr, err)
	}

	stop := closeOnDone(ctx, conn)
	defer stop()
	defer func() { _ = conn.Close() }()

	initial, hasInitial, err := s.handshakeClient(conn)
	if err != nil {
		return false, err
	}
	s.logf("connected to %s", addr)

	var initialPtr *string
	if hasInitial {
		initialPtr = &initial
	}
	if err := s.runSession(ctx, conn, initialPtr, false); err != nil {
		if ctx.Err() != nil {
			return true, ctx.Err()
		}
		return true, err
	}
	return true, nil
}

// handleConn authenticates a single inbound connection and runs a session.
func (s *Syncer) handleConn(ctx context.Context, conn net.Conn) {
	stop := closeOnDone(ctx, conn)
	defer stop()
	defer func() { _ = conn.Close() }()

	initial, hasInitial, err := s.handshakeServer(conn)
	if err != nil {
		s.logf("handshake with %s failed: %v", conn.RemoteAddr(), err)
		return
	}
	s.logf("peer connected from %s", conn.RemoteAddr())

	var initialPtr *string
	if hasInitial {
		initialPtr = &initial
	}
	if err := s.runSession(ctx, conn, initialPtr, true); err != nil && ctx.Err() == nil {
		s.logf("session with %s ended: %v", conn.RemoteAddr(), err)
	}
	s.logf("peer disconnected: %s", conn.RemoteAddr())
}

// copyToClipboard writes text to the local clipboard while holding the shared
// write lock.
func (s *Syncer) copyToClipboard(text string) error {
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	return s.cfg.Clipboard.CopyText(text)
}

func (s *Syncer) logf(format string, args ...any) {
	s.cfg.Logger.Printf(format, args...)
}

// closeOnDone closes c when ctx is canceled. The returned function stops the
// watcher; it must be called exactly once.
func closeOnDone(ctx context.Context, c io.Closer) func() {
	done := make(chan struct{})
	go func() {
		select {
		case <-ctx.Done():
			_ = c.Close()
		case <-done:
		}
	}()
	return func() { close(done) }
}
