package main

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"gardenio/kbeads/internal/client"
)

func TestAdminAuditBlocksCycles_CleanGraphExitsZero(t *testing.T) {
	mc := newMockClient()
	mc.AuditBlocksCyclesResp = &client.AuditBlocksCyclesResponse{
		EdgeCount:  4,
		NodeCount:  5,
		CycleCount: 0,
		Cycles:     nil,
	}

	origClient := beadsClient
	beadsClient = mc
	defer func() { beadsClient = origClient }()

	out := captureStdout(t, func() {
		if err := runAdminAuditBlocksCycles(adminAuditBlocksCyclesCmd, nil); err != nil {
			t.Fatalf("RunE on a clean graph should return nil, got %v", err)
		}
	})

	var manifest map[string]any
	if err := json.Unmarshal([]byte(out), &manifest); err != nil {
		t.Fatalf("output is not JSON: %v\n%s", err, out)
	}
	if got := manifest["cycle_count"]; got != float64(0) {
		t.Errorf("cycle_count = %v, want 0", got)
	}
	if mc.AuditBlocksCyclesCalls != 1 {
		t.Fatalf("expected 1 call, got %d", mc.AuditBlocksCyclesCalls)
	}
}

func TestAdminAuditBlocksCycles_DirtyGraphExitsTwo(t *testing.T) {
	mc := newMockClient()
	mc.AuditBlocksCyclesResp = &client.AuditBlocksCyclesResponse{
		EdgeCount:  3,
		NodeCount:  3,
		CycleCount: 1,
		Cycles: []client.BlocksCycleDTO{
			{Members: []string{"ka-0000001355", "ka-0000000696"}, Size: 2},
		},
	}

	origClient := beadsClient
	beadsClient = mc
	defer func() { beadsClient = origClient }()

	var runErr error
	out := captureStdout(t, func() {
		runErr = runAdminAuditBlocksCycles(adminAuditBlocksCyclesCmd, nil)
	})

	// A dirty graph must both emit the manifest AND signal a non-zero exit.
	var ec *exitCodeError
	if !errors.As(runErr, &ec) {
		t.Fatalf("expected *exitCodeError on a dirty graph, got %v", runErr)
	}
	if ec.ExitCode() != 2 {
		t.Errorf("exit code = %d, want 2", ec.ExitCode())
	}

	var manifest map[string]any
	if err := json.Unmarshal([]byte(out), &manifest); err != nil {
		t.Fatalf("output is not JSON: %v\n%s", err, out)
	}
	if got := manifest["cycle_count"]; got != float64(1) {
		t.Errorf("cycle_count = %v, want 1", got)
	}
	cycles, ok := manifest["cycles"].([]any)
	if !ok || len(cycles) != 1 {
		t.Fatalf("cycles = %v, want 1 cycle", manifest["cycles"])
	}
}

func TestAdminAuditBlocksCycles_ExitZeroFlagSuppressesNonZero(t *testing.T) {
	mc := newMockClient()
	mc.AuditBlocksCyclesResp = &client.AuditBlocksCyclesResponse{
		CycleCount: 2,
		Cycles: []client.BlocksCycleDTO{
			{Members: []string{"gd-a", "gd-b"}, Size: 2},
			{Members: []string{"gd-p", "gd-q", "gd-r"}, Size: 3},
		},
	}

	origClient := beadsClient
	beadsClient = mc
	defer func() { beadsClient = origClient }()

	if err := adminAuditBlocksCyclesCmd.Flags().Set("exit-zero", "true"); err != nil {
		t.Fatalf("set exit-zero: %v", err)
	}
	defer func() { _ = adminAuditBlocksCyclesCmd.Flags().Set("exit-zero", "false") }()

	_ = captureStdout(t, func() {
		if err := runAdminAuditBlocksCycles(adminAuditBlocksCyclesCmd, nil); err != nil {
			t.Fatalf("--exit-zero must return nil even on a dirty graph, got %v", err)
		}
	})
}

func TestAdminAuditBlocksCycles_PropagatesError(t *testing.T) {
	mc := newMockClient()
	mc.AuditBlocksCyclesErr = errors.New("boom")

	origClient := beadsClient
	beadsClient = mc
	defer func() { beadsClient = origClient }()

	err := runAdminAuditBlocksCycles(adminAuditBlocksCyclesCmd, nil)
	if err == nil || !strings.Contains(err.Error(), "boom") {
		t.Fatalf("err = %v, want substring 'boom'", err)
	}
}
