package main

import (
	"context"
	"io"
	"log/slog"
	"sync/atomic"
	"testing"
	"time"

	"k8s.io/client-go/kubernetes/fake"
)

// TestLeaderHandoverWaitsForInFlightRotation pins the single-writer invariant of
// the Codex keeper across a leader handover: while the old leader still has a
// token rotation in flight (OpenAI already spent the old single-use refresh
// token, the new one is not yet in the Secret), a standby must not be elected,
// because it would read the dead token and spend it again.
// The lease record stores whole seconds, so the timings below must be too.
func TestLeaderHandoverWaitsForInFlightRotation(t *testing.T) {
	client := fake.NewSimpleClientset()
	timing := leaderTiming{lease: 2 * time.Second, renewDeadline: 1500 * time.Millisecond, retryPeriod: 100 * time.Millisecond}
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))

	gate := &rotationGate{}
	var gateEnded atomic.Int64 // unix nanos of the moment the rotation finished

	aCtx, aSignal := context.WithCancel(context.Background())
	defer aSignal()
	aLeading := make(chan struct{})
	aExited := make(chan struct{})
	go runLeaderElectionWith(aCtx, logger, "a", "lease", "ns", client, timing,
		func() { gate.Wait(30 * time.Second) },
		func() { close(aExited) },
		func(ctx context.Context) {
			close(aLeading)
			<-ctx.Done()
		})
	select {
	case <-aLeading:
	case <-time.After(5 * time.Second):
		t.Fatal("a never became leader")
	}

	bCtx, bSignal := context.WithCancel(context.Background())
	defer bSignal()
	bLeading := make(chan time.Time, 1)
	go runLeaderElectionWith(bCtx, logger, "b", "lease", "ns", client, timing,
		func() {}, func() {},
		func(ctx context.Context) {
			bLeading <- time.Now()
			<-ctx.Done()
		})

	// A is mid-rotation when SIGTERM arrives.
	gate.begin()
	aSignal()

	// Well past the lease (and past the point where a release-on-cancel would have
	// let B in): B must still be a standby.
	select {
	case <-bLeading:
		t.Fatal("standby was elected while the old leader's rotation was still in flight")
	case <-time.After(3 * timing.lease):
	}
	select {
	case <-aExited:
		t.Fatal("old leader exited before its rotation finished")
	default:
	}

	gateEnded.Store(time.Now().UnixNano())
	gate.end()

	select {
	case at := <-bLeading:
		if at.UnixNano() < gateEnded.Load() {
			t.Fatal("standby took over before the in-flight rotation finished")
		}
	case <-time.After(10 * time.Second):
		t.Fatal("standby never took over after the old leader released the lease")
	}
	select {
	case <-aExited:
	case <-time.After(5 * time.Second):
		t.Fatal("old leader never exited after releasing the lease")
	}
}

// A standby that gets SIGTERM before it ever led has nothing to drain and must
// not hang on the election.
func TestLeaderElectionStandbyShutsDownOnSignal(t *testing.T) {
	client := fake.NewSimpleClientset()
	timing := leaderTiming{lease: 2 * time.Second, renewDeadline: 1500 * time.Millisecond, retryPeriod: 100 * time.Millisecond}
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))

	aCtx, aSignal := context.WithCancel(context.Background())
	defer aSignal()
	aLeading := make(chan struct{})
	go runLeaderElectionWith(aCtx, logger, "a", "lease", "ns", client, timing, func() {}, func() {},
		func(ctx context.Context) { close(aLeading); <-ctx.Done() })
	<-aLeading

	bCtx, bSignal := context.WithCancel(context.Background())
	bDone := make(chan struct{})
	go func() {
		runLeaderElectionWith(bCtx, logger, "b", "lease", "ns", client, timing, func() {}, func() {},
			func(ctx context.Context) { <-ctx.Done() })
		close(bDone)
	}()
	time.Sleep(200 * time.Millisecond)
	bSignal()
	select {
	case <-bDone:
	case <-time.After(5 * time.Second):
		t.Fatal("a standby ignored SIGTERM")
	}
}
