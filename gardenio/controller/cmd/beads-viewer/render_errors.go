package main

import (
	"context"
	"errors"
	"net"
	"syscall"
)

// isClientDisconnect reports whether err originates from the HTTP client
// closing the connection before the response body finished writing. These
// are benign — the page was abandoned, not corrupted — and shouldn't pollute
// ERROR-level logs (ka-0000007007: localhost monitoring probes were generating
// recurring "template render error" ERRORs on health.html with broken-pipe
// errno). Callers downgrade matches to DEBUG.
func isClientDisconnect(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, syscall.EPIPE) ||
		errors.Is(err, syscall.ECONNRESET) ||
		errors.Is(err, net.ErrClosed) ||
		errors.Is(err, context.Canceled) ||
		errors.Is(err, context.DeadlineExceeded) {
		return true
	}
	return false
}
