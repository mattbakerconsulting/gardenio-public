package server

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"gardenio/kbeads/internal/events"
	"gardenio/kbeads/internal/model"
)

// TestEscalationSpecAmend_FilerWhileOpen pins gd-3VaIxgZfDp: the filer may
// correct operation_spec on its own open, unclaimed, never-handed-up,
// unapproved merge escalation (the #966-for-#964 misfile), the filer is known
// only from a credential (its per-agent token), and nothing else about the
// immutability guard moves.
func TestEscalationSpecAmend_FilerWhileOpen(t *testing.T) {
	const (
		crewTok  = "tok-crew"  // per-agent token of gd-crew-agent, the filer
		otherTok = "tok-other" // per-agent token of another crew
	)
	ms := newMockStore()
	s := NewBeadsServer(ms, &events.NoopPublisher{})
	fc := NewFleetConfig("gardenio", ParseFleetMap(crewTok+":agent-gd-crew-agent,"+otherTok+":agent-gd-other-agent"))
	h := s.NewHTTPHandler(nil, fc)

	const wrong = "https://github.com/acme/app/pull/966"
	const right = "https://github.com/acme/app/pull/964"
	seed := func(id string, status model.Status, extra map[string]any) {
		f := map[string]any{
			"operation_type": "gitlab_mr_merge", "operation_spec": map[string]any{"mr_url": wrong},
			"requesting_agent": "gd-crew-agent", "requesting_agent_verified": "true",
		}
		for k, v := range extra {
			f[k] = v
		}
		fields, _ := json.Marshal(f)
		ms.beads[id] = &model.Bead{
			ID: id, Kind: model.KindData, Type: model.TypeEscalation, Title: "Merge PR #964",
			Status: status, Fields: fields, CreatedBy: "agent-gd-crew-agent",
		}
	}
	// patch sends body with tok; an empty tok means the shared static token
	// with an X-Actor naming the filer, which is a claim, not a credential.
	patch := func(id, tok string, body map[string]any) (int, string) {
		var buf bytes.Buffer
		_ = json.NewEncoder(&buf).Encode(body)
		req := httptest.NewRequest(http.MethodPatch, "/v1/beads/"+id, &buf)
		req.Header.Set("Content-Type", "application/json")
		if tok == "" {
			req.Header.Set("Authorization", "Bearer primary-token")
			req.Header.Set("X-Actor", "gd-crew-agent")
		} else {
			req.Header.Set("Authorization", "Bearer "+tok)
		}
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		return rec.Code, rec.Body.String()
	}
	specPatch := map[string]any{"fields": map[string]any{"operation_spec": map[string]any{"mr_url": right}}}
	specOf := func(id string) string {
		var f map[string]json.RawMessage
		_ = json.Unmarshal(ms.beads[id].Fields, &f)
		return string(f["operation_spec"])
	}

	// The filer, on its own per-agent token, fixes its open merge escalation.
	seed("gd-esc-amend-ok", model.StatusOpen, nil)
	if code, body := patch("gd-esc-amend-ok", crewTok, specPatch); code != http.StatusOK {
		t.Fatalf("filer amend: got %d %s, want 200", code, body)
	}
	if got := specOf("gd-esc-amend-ok"); !strings.Contains(got, "/pull/964") {
		t.Fatalf("filer amend: spec = %s, want it to name #964", got)
	}
	// github_pr_merge is the other merge spelling.
	seed("gd-esc-amend-gh", model.StatusOpen, map[string]any{"operation_type": "github_pr_merge"})
	if code, body := patch("gd-esc-amend-gh", crewTok, specPatch); code != http.StatusOK {
		t.Fatalf("filer amend github_pr_merge: got %d %s, want 200", code, body)
	}

	refused := []struct {
		name   string
		status model.Status
		tok    string
		extra  map[string]any
		body   map[string]any
	}{
		// Every agent pod holds the static token and can send any X-Actor.
		{"static token with the filer's X-Actor", model.StatusOpen, "", nil, nil},
		{"another agent's token", model.StatusOpen, otherTok, nil, nil},
		{"requesting_agent not verified", model.StatusOpen, crewTok, map[string]any{"requesting_agent_verified": ""}, nil},
		{"claimed", model.StatusOpen, crewTok, map[string]any{"claimed_by": "merge-desk"}, nil},
		// applyHandUp clears claimed_by; the desk already reviewed the spec.
		{"handed up", model.StatusOpen, crewTok, map[string]any{
			"handed_up_by": "merge-desk", "handed_up_at": "2026-10-30T00:00:00Z", "hand_up_reason": "operator-only",
			"held_by": "operator", "claimed_by": "", "claimed_at": ""}, nil},
		{"in progress", model.StatusInProgress, crewTok, nil, nil},
		{"closed", model.StatusClosed, crewTok, nil, nil},
		// The prior status counts, not only the one the PATCH leaves.
		{"closed, reopened in the same patch", model.StatusClosed, crewTok, nil, map[string]any{
			"status": "open", "fields": map[string]any{"operation_spec": map[string]any{"mr_url": right}}}},
		{"approved", model.StatusOpen, crewTok, map[string]any{"approved_at": "2026-10-30T00:00:00Z"}, nil},
		{"not a merge", model.StatusOpen, crewTok, map[string]any{"operation_type": "kbeads_bead_close"}, nil},
		{"spawn role", model.StatusOpen, crewTok, map[string]any{"spawn_role": "crew"}, nil},
		{"spec plus type", model.StatusOpen, crewTok, nil, map[string]any{"fields": map[string]any{
			"operation_spec": map[string]any{"mr_url": right}, "operation_type": "kbeads_bead_close"}}},
		{"spec case variant", model.StatusOpen, crewTok, nil, map[string]any{"fields": map[string]any{
			"Operation_Spec": map[string]any{"mr_url": right}}}},
	}
	for i, tc := range refused {
		t.Run(tc.name, func(t *testing.T) {
			id := "gd-esc-amend-no-" + string(rune('a'+i))
			seed(id, tc.status, tc.extra)
			before := *ms.beads[id]
			body := tc.body
			if body == nil {
				body = specPatch
			}
			code, resp := patch(id, tc.tok, body)
			if code == http.StatusOK {
				t.Fatalf("got 200, want a refusal: %s", resp)
			}
			after := ms.beads[id]
			if string(after.Fields) != string(before.Fields) || after.Status != before.Status {
				t.Fatalf("bead changed despite the refusal:\nbefore %s %s\nafter  %s %s", before.Status, before.Fields, after.Status, after.Fields)
			}
		})
	}
}
