//go:build windows

// Copyright (c) 2023 Tiago Melo. All rights reserved.
// Use of this source code is governed by the MIT License that can be found in
// the LICENSE file.

package main

import "os"

var shutdownSignals = []os.Signal{os.Interrupt}
