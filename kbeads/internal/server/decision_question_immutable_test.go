package server

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"

	"google.golang.org/grpc/codes"

	beadsv1 "gardenio/kbeads/gen/beads/v1"
	"gardenio/kbeads/internal/model"
)

func TestCheckDecisionQuestionImmutable(t *testing.T) {
	prior := json.RawMessage(`{"prompt":"Deploy?","options":[{"id":"go"},{"id":"hold"}],"context":"ctx","urgency":"high"}`)
	mk := func(fields string) *model.Bead {
		return &model.Bead{Type: model.TypeDecision, Fields: json.RawMessage(fields)}
	}
	for name, tc := range map[string]struct {
		fields  string
		wantErr bool
	}{
		"unchanged echo":             {`{"prompt":"Deploy?","options":[{"id":"go"},{"id":"hold"}],"context":"ctx","urgency":"high"}`, false},
		"unrelated edit":             {`{"prompt":"Deploy?","options":[{"id":"go"},{"id":"hold"}],"context":"ctx","urgency":"low"}`, false},
		"prompt reworded":            {`{"prompt":"Rollback?","options":[{"id":"go"},{"id":"hold"}],"context":"ctx","urgency":"high"}`, true},
		"options swapped":            {`{"prompt":"Deploy?","options":[{"id":"hold"},{"id":"go"}],"context":"ctx","urgency":"high"}`, true},
		"context rewritten":          {`{"prompt":"Deploy?","options":[{"id":"go"},{"id":"hold"}],"context":"other","urgency":"high"}`, true},
		"context removed":            {`{"prompt":"Deploy?","options":[{"id":"go"},{"id":"hold"}],"urgency":"high"}`, true},
		"question added late":        {`{"prompt":"Deploy?","options":[{"id":"go"},{"id":"hold"}],"context":"ctx","urgency":"high","question":"q"}`, true},
		"requesting_task added late": {`{"prompt":"Deploy?","options":[{"id":"go"},{"id":"hold"}],"context":"ctx","urgency":"high","requesting_task":"gd-plan"}`, true},
	} {
		t.Run(name, func(t *testing.T) {
			err := checkDecisionQuestionImmutable(mk(tc.fields), prior)
			if (err != nil) != tc.wantErr {
				t.Fatalf("err = %v, wantErr %v", err, tc.wantErr)
			}
		})
	}
	other := &model.Bead{Type: model.TypeTask, Fields: json.RawMessage(`{"prompt":"a"}`)}
	if err := checkDecisionQuestionImmutable(other, json.RawMessage(`{"prompt":"b"}`)); err != nil {
		t.Errorf("non-decision bead must not be gated, got %v", err)
	}
}

// Neither the filer nor a caller the shared-queue carve-out lets past ownership
// (human session, machine principal, bridge) may rewrite a decision's question
// through the generic write paths. Echoing the stored value back still works.
func TestDecisionQuestion_ImmutableOnGenericWrites(t *testing.T) {
	callers := filerMutators()
	callers["oidc admin"] = withMethod(withRoles(identityMutator(AuthIdentity{Type: "oidc", Subject: "admin@example.com"}, ""), RoleAdmin), authMethodOIDC)
	callers["machine"] = withMethod(identityMutator(AuthIdentity{Type: "machine", Subject: "machine:gardenio-lands-intake"}, ""), authMethodMachine)
	callers["bridge"] = withMethod(identityMutator(AuthIdentity{Type: "fleet", Subject: "bridge:slack"}, "slack-bridge"), authMethodStatic)
	for name, mutate := range callers {
		t.Run(name+"/PATCH prompt", func(t *testing.T) {
			ms, h, _ := decisionFilerFixture(t)
			rec := doFilerJSON(h, "PATCH", "/v1/beads/gd-dec-own", map[string]any{"fields": map[string]any{"prompt": "Rollback?"}}, mutate)
			if rec.Code != http.StatusBadRequest {
				t.Fatalf("expected 400, got %d: %s", rec.Code, rec.Body.String())
			}
			assertDecisionPrompt(t, ms, "Deploy?")
		})
		t.Run(name+"/PATCH options added", func(t *testing.T) {
			_, h, _ := decisionFilerFixture(t)
			rec := doFilerJSON(h, "PATCH", "/v1/beads/gd-dec-own", map[string]any{"fields": map[string]any{"options": []any{map[string]any{"id": "x"}}}}, mutate)
			if rec.Code != http.StatusBadRequest {
				t.Fatalf("expected 400, got %d: %s", rec.Code, rec.Body.String())
			}
		})
		t.Run(name+"/close body context", func(t *testing.T) {
			ms, h, _ := decisionFilerFixture(t)
			rec := doFilerJSON(h, "POST", "/v1/beads/gd-dec-own/close", map[string]any{"context": "rewritten"}, mutate)
			if rec.Code != http.StatusForbidden {
				t.Fatalf("expected 403, got %d: %s", rec.Code, rec.Body.String())
			}
			if ms.beads["gd-dec-own"].Status == model.StatusClosed {
				t.Fatal("decision must stay open")
			}
		})
		t.Run(name+"/field endpoint options", func(t *testing.T) {
			_, h, _ := decisionFilerFixture(t)
			rec := doFilerJSON(h, "POST", "/v1/beads/gd-dec-own/fields/options/append", map[string]any{"element": `{"id":"x"}`}, mutate)
			if rec.Code != http.StatusForbidden {
				t.Fatalf("expected 403, got %d: %s", rec.Code, rec.Body.String())
			}
		})
	}
	// Positive control: the filer may still echo the stored prompt back while
	// editing an unrelated field.
	ms, h, _ := decisionFilerFixture(t)
	rec := doFilerJSON(h, "PATCH", "/v1/beads/gd-dec-own", map[string]any{"fields": map[string]any{"prompt": "Deploy?", "session": "s-1"}}, filerMutators()["filer static token"])
	if rec.Code != http.StatusOK {
		t.Fatalf("echoing the stored prompt should pass, got %d: %s", rec.Code, rec.Body.String())
	}
	assertDecisionPrompt(t, ms, "Deploy?")
}

// The freeze holds on the gRPC transport too: UpdateBead funnels through the
// same updateBead, so the filer's own static-token session is refused a
// reworded prompt or new options and may still echo the stored values back.
func TestDecisionQuestion_ImmutableOnGRPCUpdateBead(t *testing.T) {
	ctx := withAuthMethod(context.Background(), authMethodStatic)
	ctx = WithAuthIdentity(ctx, AuthIdentity{Type: "static", Subject: "filer-agent"})
	ctx = WithOperatedByHeader(ctx, "oidc:owner@example.com")
	for name, patch := range map[string]string{
		"prompt":  `{"prompt":"Rollback?"}`,
		"options": `{"options":[{"id":"x"}]}`,
		"context": `{"context":"rewritten"}`,
	} {
		t.Run(name, func(t *testing.T) {
			ms, _, s := decisionFilerFixture(t)
			_, err := s.UpdateBead(ctx, &beadsv1.UpdateBeadRequest{Id: "gd-dec-own", Fields: json.RawMessage(patch)})
			requireCode(t, err, codes.InvalidArgument)
			assertDecisionPrompt(t, ms, "Deploy?")
		})
	}
	ms, _, s := decisionFilerFixture(t)
	if _, err := s.UpdateBead(ctx, &beadsv1.UpdateBeadRequest{Id: "gd-dec-own", Fields: json.RawMessage(`{"prompt":"Deploy?","session":"s-1"}`)}); err != nil {
		t.Fatalf("echoing the stored prompt should pass, got %v", err)
	}
	assertDecisionPrompt(t, ms, "Deploy?")
}

// requesting_task names the bead a decision asks about, and a plan's approve
// answer seals that plan's gardener, so it is fixed at create like the question:
// no caller may repoint an answered decision at another bead
// (gd-YaoqmhIx8x).
func TestDecisionRequestingTask_ImmutableAfterAnswer(t *testing.T) {
	answered := func(t *testing.T) (*mockStore, http.Handler) {
		ms, h, _ := decisionFilerFixture(t)
		d := ms.beads["gd-dec-own"]
		d.Status = model.StatusClosed
		d.Fields = json.RawMessage(`{"prompt":"Deploy?","requesting_task":"gd-other","chosen":"approve"}`)
		return ms, h
	}
	assertTask := func(t *testing.T, ms *mockStore) {
		t.Helper()
		if got := fieldsAsMap(ms.beads["gd-dec-own"].Fields)["requesting_task"]; got != "gd-other" {
			t.Fatalf("requesting_task = %v, want gd-other", got)
		}
	}
	for name, mutate := range filerMutators() {
		t.Run(name+"/PATCH repoint", func(t *testing.T) {
			ms, h := answered(t)
			rec := doFilerJSON(h, "PATCH", "/v1/beads/gd-dec-own", map[string]any{"fields": map[string]any{"requesting_task": "gd-plan"}}, mutate)
			if rec.Code != http.StatusBadRequest {
				t.Fatalf("expected 400, got %d: %s", rec.Code, rec.Body.String())
			}
			assertTask(t, ms)
		})
		t.Run(name+"/PATCH remove", func(t *testing.T) {
			ms, h := answered(t)
			rec := doFilerJSON(h, "PATCH", "/v1/beads/gd-dec-own", map[string]any{"fields": map[string]any{"requesting_task": nil}}, mutate)
			if rec.Code != http.StatusBadRequest {
				t.Fatalf("expected 400, got %d: %s", rec.Code, rec.Body.String())
			}
			assertTask(t, ms)
		})
		t.Run(name+"/field endpoint", func(t *testing.T) {
			ms, h := answered(t)
			rec := doFilerJSON(h, "POST", "/v1/beads/gd-dec-own/fields/requesting_task/append", map[string]any{"element": "gd-plan"}, mutate)
			if rec.Code != http.StatusForbidden {
				t.Fatalf("expected 403, got %d: %s", rec.Code, rec.Body.String())
			}
			assertTask(t, ms)
		})
	}
	t.Run("close body", func(t *testing.T) {
		ms, h, _ := decisionFilerFixture(t)
		ms.beads["gd-dec-own"].Fields = json.RawMessage(`{"prompt":"Deploy?","requesting_task":"gd-other"}`)
		rec := doFilerJSON(h, "POST", "/v1/beads/gd-dec-own/close", map[string]any{"requesting_task": "gd-plan"}, filerMutators()["filer static token"])
		if rec.Code != http.StatusForbidden {
			t.Fatalf("expected 403, got %d: %s", rec.Code, rec.Body.String())
		}
		assertTask(t, ms)
	})
}

func assertDecisionPrompt(t *testing.T, ms *mockStore, want string) {
	t.Helper()
	var f map[string]any
	_ = json.Unmarshal(ms.beads["gd-dec-own"].Fields, &f)
	if f["prompt"] != want {
		t.Fatalf("prompt = %v, want %q", f["prompt"], want)
	}
}
