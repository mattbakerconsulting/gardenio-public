package bridge

import (
	"log/slog"
	"sync"
	"testing"
	"time"
)

// TestAwaitShutdownDrainJoinsBeforeWait is a regression test for the
// WaitGroup Add-concurrent-with-Wait race in the slack-bridge shutdown path.
//
// Bug: Run() spawned handleEvents(ctx) fire-and-forget, then — after
// socket.RunContext(ctx) returned — immediately started handlerWg.Wait().
// Both unblock off the same ctx.Done() with no happens-before edge, and
// handleEvents' drain path calls handlerWg.Add(1) per queued event. So a
// drain-path Add(1) could run concurrently with the Wait(), which
// sync.WaitGroup explicitly forbids ("Add called concurrently with Wait") —
// a data race that -race detects and the runtime can panic on, crashing the
// single-replica bridge on SIGTERM (defeating the very drain-safety the code
// exists to provide).
//
// Fix: awaitShutdownDrain JOINs the drain goroutine (handleEventsDone) BEFORE
// calling handlerWg.Wait(), so every drain-path Add has completed first.
//
// This test drives awaitShutdownDrain against a drain goroutine that performs
// the exact hostile pattern — Add(1)/Done() several times and only THEN closes
// handleEventsDone. Under `go test -race`, an implementation that Waited before
// joining would flag the Add-vs-Wait race here. The fixed implementation is
// race-clean because the join edge orders all Adds before the Wait.
func TestAwaitShutdownDrainJoinsBeforeWait(t *testing.T) {
	b := &Bot{logger: slog.New(slog.NewTextHandler(discardWriter{}, nil))}

	handleEventsDone := make(chan struct{})

	// Simulate handleEvents' drain loop: each queued event does Add(1) then a
	// quick handler that Done()s. The goroutine closes handleEventsDone only
	// after every Add/Done pair — mirroring handleEvents returning after
	// drainEvents finishes dispatching.
	go func() {
		defer close(handleEventsDone)
		var handlers sync.WaitGroup
		for i := 0; i < 64; i++ {
			b.handlerWg.Add(1)
			handlers.Add(1)
			go func() {
				defer handlers.Done()
				defer b.handlerWg.Done()
			}()
		}
		handlers.Wait()
	}()

	done := make(chan struct{})
	go func() {
		defer close(done)
		b.awaitShutdownDrain(handleEventsDone)
	}()

	select {
	case <-done:
		// awaitShutdownDrain returned cleanly (and -race stayed quiet).
	case <-time.After(10 * time.Second):
		t.Fatal("awaitShutdownDrain did not return within 10s")
	}
}

// TestAwaitShutdownDrainBoundedWhenDrainStuck asserts awaitShutdownDrain does
// not wedge process exit if the drain goroutine never signals completion: the
// join wait is bounded by shutdownDrainTimeout + 2s, so exit still proceeds.
func TestAwaitShutdownDrainBoundedWhenDrainStuck(t *testing.T) {
	b := &Bot{logger: slog.New(slog.NewTextHandler(discardWriter{}, nil))}

	// handleEventsDone is never closed: emulate a drain goroutine that hangs.
	handleEventsDone := make(chan struct{})

	done := make(chan struct{})
	go func() {
		defer close(done)
		b.awaitShutdownDrain(handleEventsDone)
	}()

	// Must return via the timeout branch, well under a generous ceiling.
	select {
	case <-done:
	case <-time.After(shutdownDrainTimeout + 5*time.Second + 3*time.Second):
		t.Fatalf("awaitShutdownDrain blocked past its bounded timeout")
	}
}

// discardWriter is an io.Writer that drops everything, keeping test logs quiet.
type discardWriter struct{}

func (discardWriter) Write(p []byte) (int, error) { return len(p), nil }
