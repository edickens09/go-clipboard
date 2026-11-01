// Copyright (c) 2023 Tiago Melo. All rights reserved.
// Use of this source code is governed by the MIT License that can be found in
// the LICENSE file.

// Command clipsync keeps the clipboard of two machines in sync over TCP.
//
// Start a listener on one machine:
//
//	clipsync -listen :9876 -token secret
//
// and connect to it from the other machine:
//
//	clipsync -connect 192.168.1.10:9876 -token secret
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"os"
	"os/signal"
	"time"

	"github.com/edickens09/go-clipboard/clipboard"
	"github.com/edickens09/go-clipboard/clipsync"
)

func main() {
	if err := run(); err != nil {
		_, _ = fmt.Fprintln(os.Stderr, "clipsync:", err)
		os.Exit(1)
	}
}

func run() error {
	var (
		listenAddr  = flag.String("listen", "", "address to listen on, for example :9876")
		connectAddr = flag.String("connect", "", "address of the listening peer, for example 192.168.1.10:9876")
		token       = flag.String("token", os.Getenv("CLIPSYNC_TOKEN"), "shared secret used to authenticate the peer (default $CLIPSYNC_TOKEN)")
		interval    = flag.Duration("interval", 500*time.Millisecond, "how often the local clipboard is checked for changes")
		primary     = flag.Bool("primary", false, "sync the X11/Wayland primary selection instead of the clipboard (Unix only)")
		syncInitial = flag.Bool("sync-initial", true, "send the connecting peer's clipboard when a session starts")
		verbose     = flag.Bool("v", false, "enable verbose logging")
	)

	flag.Usage = func() {
		_, _ = fmt.Fprintf(flag.CommandLine.Output(), "Usage: clipsync -listen ADDR | -connect ADDR [options]\n\n")
		flag.PrintDefaults()
	}
	flag.Parse()

	if (*listenAddr == "") == (*connectAddr == "") {
		flag.Usage()
		return errors.New("exactly one of -listen or -connect is required")
	}
	if *interval <= 0 {
		return errors.New("-interval must be greater than zero")
	}

	logger := log.New(io.Discard, "", 0)
	if *verbose {
		logger = log.New(os.Stderr, "clipsync: ", log.LstdFlags)
	}

	if *listenAddr != "" && *token == "" {
		_, _ = fmt.Fprintln(os.Stderr, "clipsync: warning: no -token set; any peer that can reach the listener can read and write the clipboard")
	}

	syncer := clipsync.New(clipsync.Config{
		Clipboard:    clipboard.New(clipboard.ClipboardOptions{Primary: *primary}),
		PollInterval: *interval,
		Token:        *token,
		SyncInitial:  *syncInitial,
		Logger:       logger,
	})

	ctx, stop := signal.NotifyContext(context.Background(), shutdownSignals...)
	defer stop()

	var err error
	if *listenAddr != "" {
		err = syncer.ListenAndServe(ctx, *listenAddr)
	} else {
		err = syncer.Connect(ctx, *connectAddr)
	}
	if errors.Is(err, context.Canceled) {
		return nil
	}
	return err
}
