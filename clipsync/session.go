// Copyright (c) 2023 Tiago Melo. All rights reserved.
// Use of this source code is governed by the MIT License that can be found in
// the LICENSE file.

package clipsync

import (
	"context"
	"fmt"
	"net"
	"sync"
	"time"
)

// session synchronizes one connection. lastSynced holds the clipboard value
// both sides are known to agree on; it is used to avoid echoing changes back
// and forth.
type session struct {
	syncer     *Syncer
	conn       net.Conn
	lastSynced string

	// mu guards lastSynced and serializes clipboard reads with remote writes.
	mu sync.Mutex

	// writeMu serializes writes to conn.
	writeMu sync.Mutex
}

// runSession initializes the session and runs the send and receive loops until
// one of them fails or ctx is canceled. When initial is non-nil it is used as
// the first synchronized value; applyInitial controls whether it is written to
// the local clipboard.
func (s *Syncer) runSession(ctx context.Context, conn net.Conn, initial *string, applyInitial bool) error {
	sess := &session{syncer: s, conn: conn}
	sess.initialize(initial, applyInitial)

	errCh := make(chan error, 2)
	go func() { errCh <- sess.receiveLoop() }()
	go func() { errCh <- sess.pollLoop(ctx) }()

	select {
	case <-ctx.Done():
		return ctx.Err()
	case err := <-errCh:
		return err
	}
}

// initialize establishes the baseline clipboard value for the session.
func (s *session) initialize(initial *string, applyInitial bool) {
	if initial != nil {
		s.lastSynced = *initial
		if applyInitial {
			if err := s.syncer.copyToClipboard(*initial); err != nil {
				s.syncer.logf("applying initial clipboard: %v", err)
			}
		}
		return
	}

	text, err := s.syncer.cfg.Clipboard.PasteText()
	if err != nil {
		s.syncer.logf("reading local clipboard: %v", err)
		return
	}
	s.lastSynced = text
}

// pollLoop watches the local clipboard and sends changes to the peer.
func (s *session) pollLoop(ctx context.Context) error {
	ticker := time.NewTicker(s.syncer.cfg.PollInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
			text, changed := s.localChange()
			if !changed {
				continue
			}
			if err := s.sendClipboard(text); err != nil {
				return fmt.Errorf("sending clipboard: %w", err)
			}
			s.syncer.logf("sent clipboard update (%d bytes)", len(text))
		}
	}
}

// localChange returns the local clipboard text when it differs from the last
// synchronized value. The read and the comparison happen under mu so a
// concurrent remote update cannot be mistaken for a local change.
func (s *session) localChange() (string, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()

	text, err := s.syncer.cfg.Clipboard.PasteText()
	if err != nil {
		s.syncer.logf("reading local clipboard: %v", err)
		return "", false
	}
	if text == s.lastSynced {
		return "", false
	}
	if len(text) > maxClipboardSize {
		s.syncer.logf("skipping clipboard update: clipboard is %d bytes, limit is %d", len(text), maxClipboardSize)
		s.lastSynced = text
		return "", false
	}
	s.lastSynced = text
	return text, true
}

// receiveLoop applies clipboard updates received from the peer.
func (s *session) receiveLoop() error {
	for {
		msg, err := readMessage(s.conn)
		if err != nil {
			return fmt.Errorf("reading message: %w", err)
		}

		switch msg.Type {
		case msgClipboard:
			s.applyRemote(msg.Text)
		case msgError:
			return fmt.Errorf("peer error: %s", msg.Text)
		default:
			s.syncer.logf("ignoring unknown message type %q", msg.Type)
		}
	}
}

// applyRemote writes a remote clipboard update to the local clipboard. The
// lock is held across the write so the poll loop cannot read a stale value and
// send it back to the peer.
func (s *session) applyRemote(text string) {
	s.mu.Lock()
	defer s.mu.Unlock()

	if text == s.lastSynced {
		return
	}

	if err := s.syncer.copyToClipboard(text); err != nil {
		s.syncer.logf("writing local clipboard: %v", err)
		// Keep the current local value as the baseline so the stale value is
		// not echoed back to the peer.
		if current, pasteErr := s.syncer.cfg.Clipboard.PasteText(); pasteErr == nil {
			s.lastSynced = current
		} else {
			s.lastSynced = text
		}
		return
	}

	s.lastSynced = text
	s.syncer.logf("applied remote clipboard update (%d bytes)", len(text))
}

// sendClipboard writes a clipboard frame to the peer.
func (s *session) sendClipboard(text string) error {
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	return writeMessage(s.conn, message{Type: msgClipboard, Text: text})
}
