package covflush

import (
	"context"
	"os/signal"
	"syscall"
)

// contextWithTERM mirrors the signal.NotifyContext pattern used by every
// gardenio main(). Lives in a _test.go file so the production package has no
// signal handling concerns of its own (callers wire signals; covflush only
// flushes).
func contextWithTERM() (context.Context, context.CancelFunc) {
	return signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT)
}
