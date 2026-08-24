//go:build !windows

package main

import (
	"fmt"
	"os"
)

// showFatalDialog falls back to stderr; on Linux and macOS a GUI binary
// launched from a terminal still has a usable stderr.
func showFatalDialog(title, body string) {
	fmt.Fprintf(os.Stderr, "%s\n%s\n", title, body)
}
