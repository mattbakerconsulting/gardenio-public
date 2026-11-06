package main

import (
	"net/http"
	"os"
	"testing"
	"time"

	"gardenio/controller/internal/localreg"
)

// pickDeadPID returns a PID that is guaranteed not to be alive in the
// current process namespace. Mirrors the local-orphan-cleanup scenario's
// approach.
func pickDeadPID(t *testing.T) int {
	t.Helper()
	pid := 4194303
	for pid > 2 {
		proc, err := os.FindProcess(pid)
		if err != nil {
			pid--
			continue
		}
		// Signal 0 on a non-running PID returns an error.
		if err := proc.Signal(os.Signal(nil)); err != nil {
			return pid
		}
		pid--
	}
	t.Fatal("could not find a dead PID")
	return -1
}

func seedLocalReg(t *testing.T, entry localreg.Entry) {
	t.Helper()
	if _, err := localreg.Register(entry); err != nil {
		t.Fatalf("register localreg entry: %v", err)
	}
}

func TestReapLocal_ClosesOrphanAndPrunes(t *testing.T) {
	t.Setenv("GARDENIO_HOME", t.TempDir())
	stub := &agentDaemonStub{}
	setupAgentDaemon(t, stub)

	deadPID := pickDeadPID(t)
	seedLocalReg(t, localreg.Entry{
		Name:      "dead-agent",
		BeadID:    "gd-orphan-1",
		Role:      "crew",
		PID:       deadPID,
		StartedAt: time.Now().Add(-time.Hour),
	})

	if err := runAgentReapLocal(cmdWithCtx(), false); err != nil {
		t.Fatalf("runAgentReapLocal: %v", err)
	}

	stub.mu.Lock()
	defer stub.mu.Unlock()
	if len(stub.closes) != 1 || stub.closes[0].BeadID != "gd-orphan-1" {
		t.Fatalf("expected one close for gd-orphan-1, got %+v", stub.closes)
	}
	if got, _ := stub.closes[0].Body["agent_state"].(string); got != "failed" {
		t.Errorf("agent_state = %q, want failed", got)
	}
	if got, _ := stub.closes[0].Body["failure_reason"].(string); got != "local-agent-crashed" {
		t.Errorf("failure_reason = %q, want local-agent-crashed", got)
	}
	entries, _, err := localreg.List()
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Errorf("expected registry empty after reap, got %d entries", len(entries))
	}
}

func TestReapLocal_PreservesAliveEntries(t *testing.T) {
	t.Setenv("GARDENIO_HOME", t.TempDir())
	stub := &agentDaemonStub{}
	setupAgentDaemon(t, stub)

	seedLocalReg(t, localreg.Entry{
		Name:      "live-agent",
		BeadID:    "gd-live-1",
		PID:       os.Getpid(), // definitely alive
		StartedAt: time.Now(),
	})

	if err := runAgentReapLocal(cmdWithCtx(), false); err != nil {
		t.Fatalf("runAgentReapLocal: %v", err)
	}

	stub.mu.Lock()
	defer stub.mu.Unlock()
	if len(stub.closes) != 0 {
		t.Errorf("expected no closes for live agent, got %+v", stub.closes)
	}
	entries, _, err := localreg.List()
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 {
		t.Errorf("expected live agent to remain in registry, got %d entries", len(entries))
	}
}

func TestReapLocal_PrunesEvenWhen404(t *testing.T) {
	t.Setenv("GARDENIO_HOME", t.TempDir())
	stub := &agentDaemonStub{missingBeads: map[string]bool{"gd-fake": true}}
	setupAgentDaemon(t, stub)

	deadPID := pickDeadPID(t)
	seedLocalReg(t, localreg.Entry{
		Name:      "fake-bead-agent",
		BeadID:    "gd-fake",
		PID:       deadPID,
		StartedAt: time.Now().Add(-time.Hour),
	})

	if err := runAgentReapLocal(cmdWithCtx(), false); err != nil {
		t.Fatalf("runAgentReapLocal should not error on 404: %v", err)
	}
	entries, _, err := localreg.List()
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Errorf("expected registry pruned even on 404, got %d entries", len(entries))
	}
}

func TestReapLocal_PrunesEvenWhenDaemonError(t *testing.T) {
	t.Setenv("GARDENIO_HOME", t.TempDir())
	stub := &agentDaemonStub{closeFailCode: http.StatusInternalServerError}
	setupAgentDaemon(t, stub)

	deadPID := pickDeadPID(t)
	seedLocalReg(t, localreg.Entry{
		Name:      "daemon-down",
		BeadID:    "gd-orphan-500",
		PID:       deadPID,
		StartedAt: time.Now().Add(-time.Hour),
	})

	if err := runAgentReapLocal(cmdWithCtx(), false); err != nil {
		t.Fatalf("runAgentReapLocal should not error when daemon returns 500: %v", err)
	}
	entries, _, err := localreg.List()
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Errorf("expected registry pruned despite daemon error, got %d entries", len(entries))
	}
}

func TestReapLocal_DryRunPreservesRegistry(t *testing.T) {
	t.Setenv("GARDENIO_HOME", t.TempDir())
	stub := &agentDaemonStub{}
	setupAgentDaemon(t, stub)

	deadPID := pickDeadPID(t)
	seedLocalReg(t, localreg.Entry{
		Name:      "dry-run-agent",
		BeadID:    "gd-orphan-dry",
		PID:       deadPID,
		StartedAt: time.Now().Add(-time.Hour),
	})

	if err := runAgentReapLocal(cmdWithCtx(), true); err != nil {
		t.Fatalf("runAgentReapLocal: %v", err)
	}

	stub.mu.Lock()
	defer stub.mu.Unlock()
	if len(stub.closes) != 0 {
		t.Errorf("dry-run should not call daemon, got %+v", stub.closes)
	}
	entries, _, err := localreg.List()
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 {
		t.Errorf("dry-run should preserve registry entry, got %d entries", len(entries))
	}
}
