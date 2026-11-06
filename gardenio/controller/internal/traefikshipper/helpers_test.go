package traefikshipper

import (
	"io"
	"log/slog"
)

// testLogger returns a slog.Logger that discards output — keeps test runs quiet.
func testLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}
