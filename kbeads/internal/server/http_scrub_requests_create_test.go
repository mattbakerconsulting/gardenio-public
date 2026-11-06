package server

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"gardenio/kbeads/internal/model"
)

// scrubRequestBody is the create body for POST /v1/scrub-requests
// (docs/adr/taints.md §6.2/§7): unlike the generic POST /v1/beads path, taint
// and kind sit at the top level rather than nested under fields.
func scrubRequestBody(taint, kind string, scope map[string]any) map[string]any {
	return map[string]any{
		"taint": taint,
		"kind":  kind,
		"scope": scope,
	}
}

// TestHandleCreateScrubRequest_DetaintEchoesTaintMode covers acceptance
// criteria 3-5: three taints with distinct scrub_policy.detaint_mode values
// (unset/default, strip, lift) each produce the correspondingly different
// surfaced detaint_mode on a kind=detaint create response, proving the value
// is read fresh off the taint rather than hardcoded.
func TestHandleCreateScrubRequest_DetaintEchoesTaintMode(t *testing.T) {
	for _, tc := range []struct {
		name     string
		policy   json.RawMessage
		wantMode string
	}{
		{"unset defaults to fork", nil, "fork"},
		{"explicit strip", json.RawMessage(`{"detaint_mode":"strip"}`), "strip"},
		{"explicit lift", json.RawMessage(`{"detaint_mode":"lift"}`), "lift"},
		{"future mode value passes through verbatim", json.RawMessage(`{"detaint_mode":"quarantine"}`), "quarantine"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, ms, h := newTestServer()
			taintName := "acme-" + tc.name
			grantScrubTaint(ms, 1, taintName, defaultTestSubject, 5)
			ms.taintRegistry[taintName].ScrubPolicy = tc.policy

			rec := doJSON(t, h, "POST", "/v1/scrub-requests",
				scrubRequestBody(taintName, "detaint", map[string]any{"bead_ids": []string{"gd-a"}}))
			requireStatus(t, rec, 201)

			var resp map[string]any
			decodeJSON(t, rec, &resp)
			if resp["detaint_mode"] != tc.wantMode {
				t.Errorf("expected detaint_mode=%q, got %v", tc.wantMode, resp["detaint_mode"])
			}
			if resp["type"] != "scrub-request" {
				t.Errorf("expected type=scrub-request, got %v", resp["type"])
			}
			fields, _ := resp["fields"].(map[string]any)
			ids, _ := fields["resolved_bead_ids"].([]any)
			if len(ids) != 1 || ids[0] != "gd-a" {
				t.Errorf("expected resolved_bead_ids=[gd-a], got %v", fields["resolved_bead_ids"])
			}
		})
	}
}

// TestHandleCreateScrubRequest_PurgeHasNoDetaintMode covers acceptance
// criterion 4's purge half: the response for kind=purge carries no
// detaint_mode field at all, not even an empty string.
func TestHandleCreateScrubRequest_PurgeHasNoDetaintMode(t *testing.T) {
	_, ms, h := newTestServer()
	grantScrubTaint(ms, 1, "acme-production", defaultTestSubject, 5)
	ms.beads["gd-a"] = &model.Bead{ID: "gd-a", Kind: model.KindIssue, Type: model.TypeTask, Title: "a", Status: model.StatusOpen}

	rec := doJSON(t, h, "POST", "/v1/scrub-requests",
		scrubRequestBody("acme-production", "purge", map[string]any{"bead_ids": []string{"gd-a"}}))
	requireStatus(t, rec, 201)

	var resp map[string]any
	decodeJSON(t, rec, &resp)
	if _, present := resp["detaint_mode"]; present {
		t.Errorf("expected no detaint_mode field on a purge response, got %v", resp["detaint_mode"])
	}
}

// TestHandleCreateScrubRequest_ScopeShapes covers acceptance criterion 2:
// both scope shapes (bead_ids, and older_than_days+labels_any) are accepted.
func TestHandleCreateScrubRequest_ScopeShapes(t *testing.T) {
	t.Run("bead_ids", func(t *testing.T) {
		_, ms, h := newTestServer()
		grantScrubTaint(ms, 1, "acme-production", defaultTestSubject, 5)

		rec := doJSON(t, h, "POST", "/v1/scrub-requests",
			scrubRequestBody("acme-production", "detaint", map[string]any{"bead_ids": []string{"gd-a"}}))
		requireStatus(t, rec, 201)
	})

	t.Run("older_than_days+labels_any", func(t *testing.T) {
		_, ms, h := newTestServer()
		grantScrubTaint(ms, 1, "acme-production", defaultTestSubject, 5)
		old := time.Now().UTC().Add(-90 * 24 * time.Hour)
		ms.beads["gd-old"] = &model.Bead{ID: "gd-old", Kind: model.KindIssue, Type: model.TypeTask, Title: "old", Status: model.StatusOpen, CreatedAt: old}
		ms.labels["gd-old"] = []string{"project:acme"}
		seedBeadTaints(ms, "gd-old", 1)

		rec := doJSON(t, h, "POST", "/v1/scrub-requests",
			scrubRequestBody("acme-production", "purge", map[string]any{
				"older_than_days": 30,
				"labels_any":      []string{"project:acme"},
			}))
		requireStatus(t, rec, 201)

		var resp map[string]any
		decodeJSON(t, rec, &resp)
		fields, _ := resp["fields"].(map[string]any)
		ids, _ := fields["resolved_bead_ids"].([]any)
		found := false
		for _, id := range ids {
			if id == "gd-old" {
				found = true
			}
		}
		if !found {
			t.Errorf("expected resolved_bead_ids to include gd-old, got %v", fields["resolved_bead_ids"])
		}
	})
}

// TestHandleCreateScrubRequest_MalformedScopeRejected covers acceptance
// criterion 2's negative half: a scope naming neither/both shapes, or with
// the wrong value types, is rejected.
func TestHandleCreateScrubRequest_MalformedScopeRejected(t *testing.T) {
	for _, tc := range []struct {
		name  string
		scope map[string]any
	}{
		{"empty scope", map[string]any{}},
		{"both shapes at once", map[string]any{"bead_ids": []string{"gd-a"}, "older_than_days": 30}},
		{"bead_ids wrong type", map[string]any{"bead_ids": "gd-a"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, ms, h := newTestServer()
			grantScrubTaint(ms, 1, "acme-production", defaultTestSubject, 5)

			rec := doJSON(t, h, "POST", "/v1/scrub-requests",
				scrubRequestBody("acme-production", "detaint", tc.scope))
			if rec.Code < 400 || rec.Code >= 500 {
				t.Fatalf("expected a 4xx rejection, got %d; body: %s", rec.Code, rec.Body.String())
			}
		})
	}
}

// TestHandleCreateScrubRequest_MissingRequiredFields covers the handler's
// own top-level validation (taint/kind required before scope is even
// examined).
func TestHandleCreateScrubRequest_MissingRequiredFields(t *testing.T) {
	_, ms, h := newTestServer()
	grantScrubTaint(ms, 1, "acme-production", defaultTestSubject, 5)

	rec := doJSON(t, h, "POST", "/v1/scrub-requests",
		map[string]any{"kind": "detaint", "scope": map[string]any{"bead_ids": []string{"gd-a"}}})
	requireStatus(t, rec, 400)

	rec = doJSON(t, h, "POST", "/v1/scrub-requests",
		map[string]any{"taint": "acme-production", "scope": map[string]any{"bead_ids": []string{"gd-a"}}})
	requireStatus(t, rec, 400)
}

// TestHandleCreateScrubRequest_NonGrantHolderForbidden confirms the
// dedicated endpoint enforces the same grant-holder authorization as the
// generic path (applyScrubRequestGuard), rather than bypassing it — a
// requester with no grant for the named taint is rejected, matching
// TestScrubRequestRequiresGrant's 400 for the generic POST /v1/beads path.
func TestHandleCreateScrubRequest_NonGrantHolderForbidden(t *testing.T) {
	_, ms, h := newTestServer()
	seedTaint(ms, 1, "acme-production", "gd-owner-team")

	rec := doJSON(t, h, "POST", "/v1/scrub-requests",
		scrubRequestBody("acme-production", "detaint", map[string]any{"bead_ids": []string{"gd-a"}}))
	requireStatus(t, rec, 400)
}

// TestCurrentTaintDetaintMode exercises currentTaintDetaintMode directly:
// its default-to-fork behavior on an unset policy, and its error path for an
// unknown taint.
func TestCurrentTaintDetaintMode(t *testing.T) {
	s, ms, _ := newTestServer()
	seedTaint(ms, 1, "acme-production", "gd-owner-team")
	ms.taintRegistry["acme-production"].ScrubPolicy = json.RawMessage(`{"detaint_mode":"strip"}`)

	mode, err := s.currentTaintDetaintMode(context.Background(), "acme-production")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if mode != "strip" {
		t.Errorf("expected strip, got %q", mode)
	}

	if _, err := s.currentTaintDetaintMode(context.Background(), "does-not-exist"); err == nil {
		t.Errorf("expected an error for an unknown taint")
	}
}
