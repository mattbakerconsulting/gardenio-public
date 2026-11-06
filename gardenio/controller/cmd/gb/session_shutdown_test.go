package main

import (
	"context"
	"testing"
	"time"
)

// TestSessionShutdownRequested: on a pod terminate the preStop hook shuts coop
// down before kubelet signals gb, so coop's exit 143 comes first and gb's own
// SIGTERM a few seconds later. That exit is a requested teardown, not a crash
// (gd-Ga8iHjLaYR).
func TestSessionShutdownRequested(t *testing.T) {
	t.Setenv("KD_AGENT_ID", "")
	t.Setenv("BOAT_AGENT_BEAD_ID", "")
	old := podTerminateSignalWait
	t.Cleanup(func() { podTerminateSignalWait = old })
	podTerminateSignalWait = 2 * time.Second

	signalled, cancel := context.WithCancel(context.Background())
	cancel()
	if !sessionShutdownRequested(context.Background(), signalled, false) {
		t.Error("signalled before the exit: want requested")
	}
	if !sessionShutdownRequested(context.Background(), context.Background(), true) {
		t.Error("control:kill mail: want requested")
	}

	late, cancelLate := context.WithCancel(context.Background())
	time.AfterFunc(50*time.Millisecond, cancelLate)
	if !sessionShutdownRequested(context.Background(), late, false) {
		t.Error("signalled just after the exit (preStop shutdown): want requested")
	}

	podTerminateSignalWait = 50 * time.Millisecond
	if sessionShutdownRequested(context.Background(), context.Background(), false) {
		t.Error("no signal, no kill, no stop_requested: want a crash")
	}
}
