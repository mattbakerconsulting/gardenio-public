package main

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"gardenio/kbeads/internal/client"
)

func TestAdminForgetOperator_EmitsJSONAndPassesFlags(t *testing.T) {
	mc := newMockClient()
	mc.ForgetOperatorResp = &client.ForgetOperatorResponse{
		OperatorID:         264,
		SlugCleared:        true,
		SlugAliasesRetired: 1,
		AuditRows:          3,
	}

	origClient := beadsClient
	beadsClient = mc
	defer func() { beadsClient = origClient }()

	if err := adminForgetOperatorCmd.Flags().Set("reason", "GDPR-42"); err != nil {
		t.Fatalf("set reason: %v", err)
	}
	if err := adminForgetOperatorCmd.Flags().Set("confirm", "true"); err != nil {
		t.Fatalf("set confirm: %v", err)
	}
	if err := adminForgetOperatorCmd.Flags().Set("threshold", "50"); err != nil {
		t.Fatalf("set threshold: %v", err)
	}
	defer func() {
		_ = adminForgetOperatorCmd.Flags().Set("reason", "")
		_ = adminForgetOperatorCmd.Flags().Set("confirm", "false")
		_ = adminForgetOperatorCmd.Flags().Set("threshold", "0")
	}()

	out := captureStdout(t, func() {
		if err := adminForgetOperatorCmd.RunE(adminForgetOperatorCmd, []string{"264"}); err != nil {
			t.Fatalf("RunE: %v", err)
		}
	})

	var resp map[string]any
	if err := json.Unmarshal([]byte(out), &resp); err != nil {
		t.Fatalf("output is not JSON: %v\n%s", err, out)
	}
	if got := resp["operator_id"]; got != float64(264) {
		t.Errorf("operator_id = %v, want 264", got)
	}
	if got := resp["slug_cleared"]; got != true {
		t.Errorf("slug_cleared = %v, want true", got)
	}

	if len(mc.ForgetOperatorCalls) != 1 {
		t.Fatalf("expected 1 call, got %d", len(mc.ForgetOperatorCalls))
	}
	call := mc.ForgetOperatorCalls[0]
	if call.id != 264 {
		t.Errorf("id = %d, want 264", call.id)
	}
	if call.req.Reason != "GDPR-42" || !call.req.Confirm || call.req.Threshold != 50 {
		t.Errorf("req mismatch: %+v", call.req)
	}
}

func TestAdminForgetOperator_RejectsBadID(t *testing.T) {
	mc := newMockClient()
	origClient := beadsClient
	beadsClient = mc
	defer func() { beadsClient = origClient }()

	err := adminForgetOperatorCmd.RunE(adminForgetOperatorCmd, []string{"not-a-number"})
	if err == nil || !strings.Contains(err.Error(), "invalid operator id") {
		t.Fatalf("expected invalid-id error, got %v", err)
	}
	if len(mc.ForgetOperatorCalls) != 0 {
		t.Errorf("client should not be called on bad id, got %d calls", len(mc.ForgetOperatorCalls))
	}
}

func TestAdminForgetOperator_ErrorPropagates(t *testing.T) {
	mc := newMockClient()
	mc.ForgetOperatorErr = errors.New("boom")
	origClient := beadsClient
	beadsClient = mc
	defer func() { beadsClient = origClient }()

	err := adminForgetOperatorCmd.RunE(adminForgetOperatorCmd, []string{"264"})
	if err == nil || !strings.Contains(err.Error(), "boom") {
		t.Fatalf("expected error containing boom, got %v", err)
	}
}
