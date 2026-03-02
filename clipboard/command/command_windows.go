//go:build windows

// Copyright (c) 2023 Tiago Melo. All rights reserved.
// Use of this source code is governed by the MIT License that can be found in
// the LICENSE file.

package command

import (
	"fmt"
)

// textInput sends the provided text as input to the system command.
// It wraps the underlying system call processes with additional error handling.
// It returns an error if any step of the command execution process fails.
func textInput(c sysCommand, text string) error {
	in, err := c.StdinPipe()
	if err != nil {
		return fmt.Errorf("getting pipe for command: %w", err)
	}
	if err := c.Start(); err != nil {
		return fmt.Errorf("starting command: %w", err)
	}
	if _, err := in.Write([]byte(text)); err != nil {
		return fmt.Errorf("writing input for command: %w", err)
	}
	if err := in.Close(); err != nil {
		return fmt.Errorf("closing input: %w", err)
	}
	if err := c.Wait(); err != nil {
		return fmt.Errorf("waiting for command: %w", err)
	}
	return nil
}

// textOutput executes the system command and captures its standard output.
// It returns the captured output as a string along with any error that occurred during execution.
func textOutput(c sysCommand) (string, error) {
	out, err := c.Output()
	if err != nil {
		return "", fmt.Errorf("getting output for command: %w", err)
	}
	return string(out), nil
}
