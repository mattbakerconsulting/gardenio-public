package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"gardenio/controller/internal/beadsapi"
	"gardenio/controller/internal/bridge"
)

// gardener_successor_test.go — card-graph AUTHORING coverage (task
// ka-0000005273 / "C3"): drawing a successor off a gardener card serializes it
// to the predecessor's chain.to, and a second draw fans out to an array.

// seedSuccessorFake seeds a gardener bead the successor authoring routes accept.
func seedSuccessorFake(t *testing.T, chain string) *fakeDaemon {
	t.Helper()
	f := newFake()
	fields := map[string]string{"role": "gardener", "project": "gardenio"}
	if chain != "" {
		fields["chain"] = chain
	}
	f.beads["gd-pred"] = &beadsapi.BeadDetail{
		ID:     "gd-pred",
		Title:  "Research gardener",
		Type:   "trigger",
		Status: "open",
		Labels: []string{"trigger:schedule", "role:gardener", "project:gardenio", "gardener-epic:gd-epicRoot"},
		Fields: fields,
	}
	return f
}

func successorReq(method, path string, form url.Values) *http.Request {
	var req *http.Request
	if form != nil {
		req = httptest.NewRequest(method, path, strings.NewReader(form.Encode()))
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	} else {
		req = httptest.NewRequest(method, path, nil)
	}
	req.Header.Set("X-Auth-Request-Email", "op@example.com")
	return req
}

func TestGardenerSuccessorForm_RendersAuthoringSheet(t *testing.T) {
	f := seedSuccessorFake(t, "")
	mux := mkServer(t, f)

	w := httptest.NewRecorder()
	mux.ServeHTTP(w, successorReq("GET", "/gardener/gd-pred/successor", nil))
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d body=%s", w.Code, w.Body.String())
	}
	body := w.Body.String()
	for _, want := range []string{
		`hx-post="/gardener/gd-pred/successor"`,
		`name="phase"`,
		`name="terminal_goal"`,
		`name="join_parent"`,
		`name="branch_decision_key"`,
		"Draw successor",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("authoring sheet missing %q", want)
		}
	}
}

func TestGardenerSuccessor_FirstHeirWritesObject(t *testing.T) {
	f := seedSuccessorFake(t, "")
	mux := mkServer(t, f)

	form := url.Values{}
	form.Set("phase", "impl")
	form.Set("terminal_goal", "All children merged & green.")
	form.Set("prompt_advice", "gd-implPromptX")

	w := httptest.NewRecorder()
	mux.ServeHTTP(w, successorReq("POST", "/gardener/gd-pred/successor", form))
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d body=%s", w.Code, w.Body.String())
	}
	persisted, ok := f.updateFieldsLastVal["chain"].(string)
	if !ok {
		t.Fatalf("chain not persisted; got %v", f.updateFieldsLastVal)
	}
	// Single heir → `to` is a bare object (byte-compatible with C1).
	var probe struct {
		To json.RawMessage `json:"to"`
	}
	if err := json.Unmarshal([]byte(persisted), &probe); err != nil {
		t.Fatalf("persisted chain not JSON: %v (%q)", err, persisted)
	}
	if len(probe.To) == 0 || probe.To[0] != '{' {
		t.Errorf("single heir `to` = %s, want a bare object", probe.To)
	}
	// The chain must parse back into a real heir via the shared codec.
	var spec bridge.ChainSpec
	if err := json.Unmarshal([]byte(persisted), &spec); err != nil {
		t.Fatalf("chain reparse: %v", err)
	}
	if spec.To == nil || spec.To.Phase != "impl" || spec.To.PromptAdvice != "gd-implPromptX" {
		t.Errorf("reparsed heir = %+v, want phase=impl advice=gd-implPromptX", spec.To)
	}
	if len(spec.Heirs()) != 1 {
		t.Errorf("Heirs() = %d, want 1", len(spec.Heirs()))
	}
}

func TestGardenerSuccessor_SecondHeirFansOutToArray(t *testing.T) {
	// Predecessor already declares one heir; a second draw fans out.
	f := seedSuccessorFake(t, `{"to":{"phase":"impl-a","epic":"gd-ANVVMrCTqf"}}`)
	mux := mkServer(t, f)

	form := url.Values{}
	form.Set("phase", "impl-b")
	form.Set("epic", "gd-U0ogvpVjao")

	w := httptest.NewRecorder()
	mux.ServeHTTP(w, successorReq("POST", "/gardener/gd-pred/successor", form))
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d body=%s", w.Code, w.Body.String())
	}
	persisted := f.updateFieldsLastVal["chain"].(string)
	var probe struct {
		To json.RawMessage `json:"to"`
	}
	if err := json.Unmarshal([]byte(persisted), &probe); err != nil {
		t.Fatalf("persisted chain not JSON: %v", err)
	}
	if len(probe.To) == 0 || probe.To[0] != '[' {
		t.Errorf("fan-out `to` = %s, want an array", probe.To)
	}
	var spec bridge.ChainSpec
	if err := json.Unmarshal([]byte(persisted), &spec); err != nil {
		t.Fatalf("chain reparse: %v", err)
	}
	heirs := spec.Heirs()
	if len(heirs) != 2 {
		t.Fatalf("Heirs() = %d, want 2 (fan-out)", len(heirs))
	}
	if heirs[0].Phase != "impl-a" || heirs[1].Phase != "impl-b" {
		t.Errorf("heirs = %q,%q, want impl-a,impl-b", heirs[0].Phase, heirs[1].Phase)
	}
}

func TestGardenerSuccessor_JoinAndBranchFieldsPersist(t *testing.T) {
	f := seedSuccessorFake(t, "")
	mux := mkServer(t, f)

	form := url.Values{}
	form.Set("phase", "ship")
	form.Set("join_parent", "gd-twppcntwuz")
	form.Set("branch_decision_key", "gd-cfuibfupjl")

	w := httptest.NewRecorder()
	mux.ServeHTTP(w, successorReq("POST", "/gardener/gd-pred/successor", form))
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d body=%s", w.Code, w.Body.String())
	}
	var spec bridge.ChainSpec
	if err := json.Unmarshal([]byte(f.updateFieldsLastVal["chain"].(string)), &spec); err != nil {
		t.Fatalf("chain reparse: %v", err)
	}
	if spec.To.JoinParent != "gd-twppcntwuz" {
		t.Errorf("JoinParent = %q, want gd-twppcntwuz", spec.To.JoinParent)
	}
	if spec.To.BranchDecisionKey != "gd-cfuibfupjl" {
		t.Errorf("BranchDecisionKey = %q, want gd-cfuibfupjl", spec.To.BranchDecisionKey)
	}
}

func TestGardenerSuccessor_EmptyHeirRejected(t *testing.T) {
	f := seedSuccessorFake(t, "")
	mux := mkServer(t, f)

	// No phase / goal / advice → nothing identity-bearing → rejected, no write.
	form := url.Values{}
	form.Set("epic", "gd-epicX")

	w := httptest.NewRecorder()
	mux.ServeHTTP(w, successorReq("POST", "/gardener/gd-pred/successor", form))
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200 (inline error), got %d", w.Code)
	}
	if f.updateFieldsCalls != 0 {
		t.Errorf("empty heir should not persist; updateFieldsCalls=%d", f.updateFieldsCalls)
	}
	if !strings.Contains(w.Body.String(), "at least a phase") {
		t.Errorf("expected inline validation error, got %q", w.Body.String())
	}
}

func TestGardenerSuccessor_Unauthenticated401(t *testing.T) {
	f := seedSuccessorFake(t, "")
	mux := mkServer(t, f)

	form := url.Values{}
	form.Set("phase", "impl")
	req := httptest.NewRequest("POST", "/gardener/gd-pred/successor", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	// No X-Auth-Request-Email → unauthenticated.
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401, got %d", w.Code)
	}
	if f.updateFieldsCalls != 0 {
		t.Errorf("unauthenticated write leaked; updateFieldsCalls=%d", f.updateFieldsCalls)
	}
}
