// Package covflush is the runtime coverage flusher used by every gardenio
// long-running Go binary.
//
// Background: Go's `-cover` runtime registers a writeout via runtime atexit
// that fires only when main returns or os.Exit is called with the runtime
// fully initialized. SIGTERM with no Go-side handler kills the process before
// atexit runs and counter data is lost. Every gardenio binary already installs
// signal.NotifyContext(SIGTERM, SIGINT) and shuts down cleanly, which means
// the atexit path *does* fire — but a deferred Flush is a cheap belt-and-
// braces guarantee that survives future refactors of main's shutdown sequence.
//
// Usage (top of main):
//
//	func main() {
//	    defer covflush.Flush()
//	    ...
//	}
//
// In non-cover builds, runtime/coverage.WriteCountersDir is a no-op and Flush
// returns nil immediately when GOCOVERDIR is unset (the prod case).
package covflush

import (
	"fmt"
	"log/slog"
	"os"
	"runtime/coverage"
)

// Flush writes coverage counters to $GOCOVERDIR if set. Safe to call from any
// goroutine and safe to call multiple times. Returns nil when GOCOVERDIR is
// unset (the prod case) so callers can `defer covflush.Flush()` unconditionally.
//
// In binaries built without `-cover`, runtime/coverage.WriteCountersDir is a
// runtime no-op that returns an error; we swallow that error and return nil
// when GOCOVERDIR is unset, and log + return it when GOCOVERDIR is set so the
// E2E pipeline can spot a coverage-build/runtime mismatch.
func Flush() error {
	dir := os.Getenv("GOCOVERDIR")
	if dir == "" {
		return nil
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		slog.Warn("covflush: mkdir GOCOVERDIR failed", "dir", dir, "err", err)
		return fmt.Errorf("covflush: mkdir %q: %w", dir, err)
	}
	if err := coverage.WriteCountersDir(dir); err != nil {
		slog.Warn("covflush: WriteCountersDir failed (binary may not be -cover built)", "dir", dir, "err", err)
		return fmt.Errorf("covflush: WriteCountersDir(%q): %w", dir, err)
	}
	slog.Info("covflush: coverage counters flushed", "dir", dir)
	return nil
}
