package ui

import (
	"os"
	"strings"

	"golang.org/x/term"
)

// ShouldUseColor returns true when ANSI colors should be used on stdout.
// It respects NO_COLOR, CLICOLOR_FORCE, CLICOLOR, and TTY detection.
func ShouldUseColor() bool {
	// https://no-color.org — any non-empty value disables color.
	if os.Getenv("NO_COLOR") != "" {
		return false
	}
	// CLICOLOR_FORCE=1 forces color even without a TTY.
	if strings.TrimSpace(os.Getenv("CLICOLOR_FORCE")) == "1" {
		return true
	}
	// CLICOLOR=0 explicitly disables color.
	if strings.TrimSpace(os.Getenv("CLICOLOR")) == "0" {
		return false
	}
	// Default: color if stdout is a terminal.
	return term.IsTerminal(int(os.Stdout.Fd()))
}
