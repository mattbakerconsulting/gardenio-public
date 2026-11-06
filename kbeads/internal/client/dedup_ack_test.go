package client

// Client-side decode tests for the dedup-ack gate rejection (epic
// ka-0000004599). The daemon returns HTTP 409 with code=dedup_ack_required and
// a structured body; the client must surface it as a typed
// *DedupAckRequiredError carrying the open list, hash, and retry hint so the
// kd CLI can render it. Mirrors operator_refusal_test.go.

import (
	"errors"
	"net/http"
	"testing"
)

func TestDecodeResponse_DedupAckRequired(t *testing.T) {
	body := `{
		"error": "1 open decision(s) already exist in project \"gardenio\" that you have not acknowledged",
		"code": "dedup_ack_required",
		"bead_type": "decision",
		"project": "gardenio",
		"hash": "abc123",
		"open": [{"id":"gd-open1","title":"Self-merge MR !2982?","age_seconds":7320}],
		"truncated": false,
		"total_open": 1,
		"retry_with": "--saw-open-decisions=abc123"
	}`
	err := decodeResponse(http.StatusConflict, []byte(body), nil)
	var dedup *DedupAckRequiredError
	if !errors.As(err, &dedup) {
		t.Fatalf("expected *DedupAckRequiredError, got %T: %v", err, err)
	}
	if dedup.StatusCode != http.StatusConflict {
		t.Errorf("StatusCode = %d, want 409", dedup.StatusCode)
	}
	if dedup.BeadType != "decision" {
		t.Errorf("BeadType = %q, want decision", dedup.BeadType)
	}
	if dedup.Project != "gardenio" {
		t.Errorf("Project = %q, want gardenio", dedup.Project)
	}
	if dedup.Hash != "abc123" {
		t.Errorf("Hash = %q, want abc123", dedup.Hash)
	}
	if dedup.TotalOpen != 1 || len(dedup.Open) != 1 {
		t.Fatalf("expected 1 open entry, got total=%d len=%d", dedup.TotalOpen, len(dedup.Open))
	}
	if dedup.Open[0].ID != "gd-open1" || dedup.Open[0].AgeSeconds != 7320 {
		t.Errorf("open[0] = %+v, want {gd-open1, …, 7320}", dedup.Open[0])
	}
	if dedup.RetryWith != "--saw-open-decisions=abc123" {
		t.Errorf("RetryWith = %q", dedup.RetryWith)
	}
}

// A generic 409 with no dedup_ack_required code must NOT be surfaced as a
// DedupAckRequiredError — the parse must be code-gated so ordinary conflicts
// (CAS, embedding-missing) stay plain APIErrors.
func TestDecodeResponse_Generic409NotDedup(t *testing.T) {
	body := `{"error":"compare-and-swap conflict: bead was modified concurrently"}`
	err := decodeResponse(http.StatusConflict, []byte(body), nil)
	var dedup *DedupAckRequiredError
	if errors.As(err, &dedup) {
		t.Fatalf("did not expect DedupAckRequiredError, got %v", err)
	}
	var apiErr *APIError
	if !errors.As(err, &apiErr) {
		t.Fatalf("expected *APIError, got %T: %v", err, err)
	}
}
