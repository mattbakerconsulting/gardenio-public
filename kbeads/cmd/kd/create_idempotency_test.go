package main

import (
	"testing"
)

// TestCreateCmd_IdempotencyKeyPassthrough verifies the CLI threads
// --idempotency-key into the CreateBeadRequest so the server-side dedup
// (ka-0000002856) sees it. We assert on the captured request the mock client
// recorded.
func TestCreateCmd_IdempotencyKeyPassthrough(t *testing.T) {
	mc := newMockClient()
	origClient := beadsClient
	beadsClient = mc
	defer func() { beadsClient = origClient }()

	if err := createCmd.Flags().Set("idempotency-key", "run-42"); err != nil {
		t.Fatalf("set idempotency-key: %v", err)
	}
	if err := createCmd.Flags().Set("project", "gardenio"); err != nil {
		t.Fatalf("set project: %v", err)
	}
	defer func() {
		_ = createCmd.Flags().Set("idempotency-key", "")
		_ = createCmd.Flags().Set("project", "")
	}()

	_ = captureStdout(t, func() {
		if err := createCmd.RunE(createCmd, []string{"a keyed task"}); err != nil {
			t.Fatalf("RunE: %v", err)
		}
	})

	if got := len(mc.CreateBeadCalls); got != 1 {
		t.Fatalf("expected 1 CreateBead call, got %d", got)
	}
	req := mc.CreateBeadCalls[0]
	if req.IdempotencyKey != "run-42" {
		t.Errorf("IdempotencyKey = %q, want run-42", req.IdempotencyKey)
	}
}

// TestCreateCmd_NoIdempotencyKey_LeavesEmpty — without the flag the request
// carries an empty key, preserving the always-create behavior.
func TestCreateCmd_NoIdempotencyKey_LeavesEmpty(t *testing.T) {
	mc := newMockClient()
	origClient := beadsClient
	beadsClient = mc
	defer func() { beadsClient = origClient }()

	if err := createCmd.Flags().Set("project", "gardenio"); err != nil {
		t.Fatalf("set project: %v", err)
	}
	defer func() { _ = createCmd.Flags().Set("project", "") }()

	_ = captureStdout(t, func() {
		if err := createCmd.RunE(createCmd, []string{"an unkeyed task"}); err != nil {
			t.Fatalf("RunE: %v", err)
		}
	})

	if got := len(mc.CreateBeadCalls); got != 1 {
		t.Fatalf("expected 1 CreateBead call, got %d", got)
	}
	if k := mc.CreateBeadCalls[0].IdempotencyKey; k != "" {
		t.Errorf("IdempotencyKey = %q, want empty", k)
	}
}
