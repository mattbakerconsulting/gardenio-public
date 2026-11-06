package server

import (
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"testing"

	beadsv1 "gardenio/kbeads/gen/beads/v1"
	"gardenio/kbeads/internal/model"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// TestValidateFilterTypes is the unit-level coverage the bead (ka-0000008041)
// asks for: the server-side --type filter validator rejects unknown types and
// accepts valid ones, reusing the create-path resolveTypeConfig so it can never
// drift from the known-type set.
func TestValidateFilterTypes(t *testing.T) {
	srv, ms, ctx := testCtx(t)
	// Register a custom type in the store so the "custom type accepted" case
	// exercises the store path (not just the compiled-in builtin floor).
	ms.configs["type:widget"] = &model.Config{
		Key:   "type:widget",
		Value: json.RawMessage(`{"kind":"config","fields":[]}`),
	}

	t.Run("empty set is a no-op", func(t *testing.T) {
		if err := srv.validateFilterTypes(ctx, nil); err != nil {
			t.Fatalf("nil types: unexpected error: %v", err)
		}
		if err := srv.validateFilterTypes(ctx, []model.BeadType{}); err != nil {
			t.Fatalf("empty types: unexpected error: %v", err)
		}
	})

	t.Run("builtin types accepted", func(t *testing.T) {
		for _, tp := range []model.BeadType{"task", "bug", "epic", "decision", "agent"} {
			if err := srv.validateFilterTypes(ctx, []model.BeadType{tp}); err != nil {
				t.Errorf("type %q should be valid, got error: %v", tp, err)
			}
		}
	})

	t.Run("multiple valid types accepted", func(t *testing.T) {
		if err := srv.validateFilterTypes(ctx, []model.BeadType{"task", "bug", "epic"}); err != nil {
			t.Errorf("mixed valid types: unexpected error: %v", err)
		}
	})

	t.Run("custom stored type accepted", func(t *testing.T) {
		if err := srv.validateFilterTypes(ctx, []model.BeadType{"widget"}); err != nil {
			t.Errorf("custom stored type 'widget' should be valid, got error: %v", err)
		}
	})

	t.Run("empty-string type is skipped, not rejected", func(t *testing.T) {
		// Mirrors the CLI guard: an empty --type= value never matches anything
		// but is not an "unknown type" — the pre-existing no-op behaviour.
		if err := srv.validateFilterTypes(ctx, []model.BeadType{""}); err != nil {
			t.Errorf("empty type value should be skipped, got error: %v", err)
		}
	})

	t.Run("unknown type rejected as inputError", func(t *testing.T) {
		err := srv.validateFilterTypes(ctx, []model.BeadType{"zzznotatype"})
		if err == nil {
			t.Fatal("expected error for unknown type")
		}
		var ie inputError
		if !errors.As(err, &ie) {
			t.Fatalf("expected inputError, got %T: %v", err, err)
		}
		msg := err.Error()
		if !strings.Contains(msg, "unknown bead type") || !strings.Contains(msg, "zzznotatype") {
			t.Errorf("error should name the unknown type; got %q", msg)
		}
		// The message must list valid types so the caller can self-correct
		// (matches the CLI guard shape).
		if !strings.Contains(msg, "valid types:") || !strings.Contains(msg, "task") {
			t.Errorf("error should list valid types; got %q", msg)
		}
	})

	t.Run("one unknown among valid types still rejected", func(t *testing.T) {
		err := srv.validateFilterTypes(ctx, []model.BeadType{"task", "gardener", "bug"})
		if err == nil {
			t.Fatal("expected error when any type is unknown")
		}
		if !strings.Contains(err.Error(), "gardener") {
			t.Errorf("error should name the offending value 'gardener'; got %q", err.Error())
		}
	})
}

// TestListBeads_RejectsUnknownType locks in that the gRPC ListBeads RPC returns
// codes.InvalidArgument (not a silent empty page) for an unknown --type value —
// the RPC every non-CLI client (bridges, automation, beads-viewer's gRPC-less
// path notwithstanding) shares.
func TestListBeads_RejectsUnknownType(t *testing.T) {
	srv, _, ctx := testCtx(t)

	_, err := srv.ListBeads(ctx, &beadsv1.ListBeadsRequest{
		Type: []string{"zzznotatype"},
	})
	if err == nil {
		t.Fatal("expected error for unknown --type on ListBeads")
	}
	if status.Code(err) != codes.InvalidArgument {
		t.Fatalf("expected InvalidArgument, got %v (%v)", status.Code(err), err)
	}
}

// TestListBeads_AcceptsValidTypeAndAlias confirms a valid type — and a
// deprecated-but-valid alias (template→formula) — is NOT rejected by the new
// validation, so the guard never regresses a legitimate query.
func TestListBeads_AcceptsValidTypeAndAlias(t *testing.T) {
	srv, ms, ctx := testCtx(t)
	ms.beads["gd-f-1"] = &model.Bead{
		ID: "gd-f-1", Title: "Formula 1", Type: "formula", Kind: "data", Status: "open",
	}

	// Valid canonical type.
	if _, err := srv.ListBeads(ctx, &beadsv1.ListBeadsRequest{Type: []string{"formula"}}); err != nil {
		t.Fatalf("valid type 'formula' rejected: %v", err)
	}
	// Deprecated alias resolves to formula and must still be accepted.
	if _, err := srv.ListBeads(ctx, &beadsv1.ListBeadsRequest{Type: []string{"template"}}); err != nil {
		t.Fatalf("valid alias 'template' rejected: %v", err)
	}
}

// TestHandleListBeads_RejectsUnknownType covers the HTTP /v1/beads path — the
// one beads-viewer and every REST consumer use. An unknown ?type= must 400,
// not return an empty 200 body indistinguishable from a genuinely empty result.
func TestHandleListBeads_RejectsUnknownType(t *testing.T) {
	_, _, h := newTestServer()

	rec := doJSON(t, h, "GET", "/v1/beads?type=zzznotatype", nil)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400; body = %s", rec.Code, rec.Body.String())
	}
	var body map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decoding error body: %v", err)
	}
	msg, _ := body["error"].(string)
	if !strings.Contains(msg, "unknown bead type") || !strings.Contains(msg, "zzznotatype") {
		t.Errorf("error body should name the unknown type; got %q", msg)
	}
}

// TestHandleListBeads_AcceptsValidType confirms the HTTP path still 200s for a
// valid ?type=, including the template→formula alias (which the handler now
// resolves before validating).
func TestHandleListBeads_AcceptsValidType(t *testing.T) {
	_, _, h := newTestServer()

	for _, q := range []string{"/v1/beads?type=task", "/v1/beads?type=template", "/v1/beads?type=task,bug"} {
		rec := doJSON(t, h, "GET", q, nil)
		if rec.Code != http.StatusOK {
			t.Errorf("GET %s: status = %d, want 200; body = %s", q, rec.Code, rec.Body.String())
		}
	}
}
