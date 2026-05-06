// Copyright (c) 2026 TrendVidia, LLC.
// SPDX-License-Identifier: MIT

package ctl

import (
	"bufio"
	"fmt"
	"io"
	"os"
	"strings"
)

// confirm prompts the user with `prompt` followed by ` [y/N] ` and reads a
// single line of input from stdin. It returns true only on an explicit
// "y" / "yes" (case-insensitive). Any other input — including a bare
// newline or EOF — returns false.
//
// If yes is true (the user passed --yes), the prompt is skipped and the
// function returns true. If stdin is not a TTY and yes is false, the
// function refuses to proceed: this prevents accidental misfires from
// scripts and pipelines that did not explicitly opt in.
func confirm(in io.Reader, isTTY bool, yes bool, prompt string) (bool, error) {
	if yes {
		return true, nil
	}
	if !isTTY {
		return false, fmt.Errorf("non-interactive shell: pass --yes to skip the confirmation prompt")
	}
	fmt.Fprintf(os.Stderr, "%s [y/N] ", prompt)
	reader := bufio.NewReader(in)
	line, err := reader.ReadString('\n')
	if err != nil && err != io.EOF {
		return false, err
	}
	switch strings.ToLower(strings.TrimSpace(line)) {
	case "y", "yes":
		return true, nil
	default:
		return false, nil
	}
}

// stdinIsTTY reports whether os.Stdin is connected to a terminal. Used by
// confirm to decide whether prompting is meaningful. We avoid pulling in
// golang.org/x/term for this single call and inspect the file mode
// directly: character devices (terminals) have ModeCharDevice set; pipes
// and redirected files do not.
func stdinIsTTY() bool {
	fi, err := os.Stdin.Stat()
	if err != nil {
		return false
	}
	return fi.Mode()&os.ModeCharDevice != 0
}
