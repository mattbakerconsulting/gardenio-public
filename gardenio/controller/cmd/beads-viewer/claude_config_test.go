package main

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"gardenio/controller/internal/beadsapi"
)

// claudeSettingsBead builds a type=config claude-settings bead fixture.
func claudeSettingsBead(id string, labels []string, desc string) *beadsapi.BeadDetail {
	return &beadsapi.BeadDetail{
		ID:          id,
		Title:       "claude-settings",
		Type:        "config",
		Status:      "open",
		Labels:      labels,
		Description: desc,
	}
}

// TestHandleClaudeConfig_Empty renders the matrix with no config beads → the
// empty-state copy, and confirms the config query shape.
func TestHandleClaudeConfig_Empty(t *testing.T) {
	f := newFake()
	f.listResp = &beadsapi.ListBeadsResult{Beads: nil, Total: 0}
	mux := mkServer(t, f)

	req := httptest.NewRequest("GET", "/claude-config", nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	body := w.Body.String()
	if !strings.Contains(body, "No <code>claude-settings</code> config beads carry a <code>role:</code> label yet.") {
		t.Errorf("expected empty-state copy, body=%s", body)
	}
}

// TestHandleClaudeConfig_Matrix renders a role×knob matrix from seeded config
// beads and asserts the effective values + the effort-trap surfacing.
func TestHandleClaudeConfig_Matrix(t *testing.T) {
	f := newFake()
	f.listResp = &beadsapi.ListBeadsResult{
		Beads: []*beadsapi.BeadDetail{
			// Global effort pin — the trap source.
			claudeSettingsBead("gd-cc-global", []string{"global"}, `{"env":{"CLAUDE_CODE_EFFORT_LEVEL":"high"}}`),
			// crew flips ultracode but does NOT re-pin effort → TRAP.
			claudeSettingsBead("gd-cc-crew", []string{"role:crew"}, `{"ultracode":true}`),
			// thread flips ultracode AND clears effort by not inheriting a pin →
			// clean xhigh (global pin still merges though, so also a trap unless
			// thread pins its own). Give thread an explicit xhigh pin: clean.
			claudeSettingsBead("gd-cc-thread", []string{"role:thread"}, `{"ultracode":true,"env":{"CLAUDE_CODE_EFFORT_LEVEL":"xhigh"}}`),
		},
		Total: 3,
	}
	mux := mkServer(t, f)

	req := httptest.NewRequest("GET", "/claude-config", nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	body := w.Body.String()

	// Both roles appear.
	if !strings.Contains(body, ">crew<") {
		t.Error("expected crew row")
	}
	if !strings.Contains(body, ">thread<") {
		t.Error("expected thread row")
	}
	// The trap banner + marker must render for crew (global effort pin blocks xhigh).
	if !strings.Contains(body, "blocks xhigh") {
		t.Errorf("expected effort-trap marker in body, body=%s", body)
	}
	// thread pinned its own xhigh → the value appears.
	if !strings.Contains(body, "xhigh") {
		t.Error("expected xhigh value for thread")
	}
}

// roleBead builds a type=role bead fixture carrying the given fields (model,
// agent_type, …). Title is the role name.
func roleBead(id, name string, fields map[string]string) *beadsapi.BeadDetail {
	return &beadsapi.BeadDetail{
		ID:     id,
		Title:  name,
		Type:   "role",
		Status: "open",
		Fields: fields,
	}
}

// TestHandleClaudeConfig_RoleBeadOverlay confirms the role-bead overlay: model
// falls back to the type:role bead's model field when no claude-settings bead
// pins it, and agent_type gets its own column sourced from the role bead.
func TestHandleClaudeConfig_RoleBeadOverlay(t *testing.T) {
	f := newFake()
	f.listResp = &beadsapi.ListBeadsResult{
		Beads: []*beadsapi.BeadDetail{
			// A claude-settings bead makes `crew` part of the row vocabulary.
			claudeSettingsBead("gd-cc-crew", []string{"role:crew"}, `{"ultracode":true}`),
			// The type:role bead carries model + agent_type (not in any settings bead).
			roleBead("gd-role-crew", "crew", map[string]string{
				"model":      "claude-opus-4-8-oauth",
				"agent_type": "general-purpose",
			}),
		},
		Total: 2,
	}
	mux := mkServer(t, f)

	req := httptest.NewRequest("GET", "/claude-config", nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	body := w.Body.String()
	// model fallback from the role bead.
	if !strings.Contains(body, "claude-opus-4-8-oauth") {
		t.Errorf("expected role-bead model fallback in matrix, body=%s", body)
	}
	// agent_type column value from the role bead.
	if !strings.Contains(body, "general-purpose") {
		t.Errorf("expected agent_type value in matrix, body=%s", body)
	}
	// The agent_type column header exists.
	if !strings.Contains(body, "agent_type") {
		t.Error("expected an agent_type column header")
	}
}

// TestHandleClaudeConfig_Search filters rows by the ?q= substring over role
// names and effective knob values.
func TestHandleClaudeConfig_Search(t *testing.T) {
	f := newFake()
	f.listResp = &beadsapi.ListBeadsResult{
		Beads: []*beadsapi.BeadDetail{
			claudeSettingsBead("gd-cc-crew", []string{"role:crew"}, `{"ultracode":true,"env":{"CLAUDE_CODE_EFFORT_LEVEL":"xhigh"}}`),
			claudeSettingsBead("gd-cc-thread", []string{"role:thread"}, `{"model":"claude-fable-5"}`),
		},
		Total: 2,
	}
	mux := mkServer(t, f)

	// Filter by role name.
	req := httptest.NewRequest("GET", "/claude-config?q=thread", nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	body := w.Body.String()
	if !strings.Contains(body, ">thread<") {
		t.Error("expected thread row to survive q=thread")
	}
	if strings.Contains(body, ">crew<") {
		t.Error("crew row must be filtered out by q=thread")
	}
	if !strings.Contains(body, "Showing 1 of 2 roles") {
		t.Errorf("expected match summary, body=%s", body)
	}

	// Filter by an effective value (crew's xhigh).
	req = httptest.NewRequest("GET", "/claude-config?q=xhigh", nil)
	w = httptest.NewRecorder()
	mux.ServeHTTP(w, req)
	body = w.Body.String()
	if !strings.Contains(body, ">crew<") {
		t.Error("expected crew row to survive q=xhigh (matches effort value)")
	}
	if strings.Contains(body, ">thread<") {
		t.Error("thread row must be filtered out by q=xhigh")
	}

	// A non-matching query yields the empty-match copy.
	req = httptest.NewRequest("GET", "/claude-config?q=zzzznope", nil)
	w = httptest.NewRecorder()
	mux.ServeHTTP(w, req)
	if !strings.Contains(w.Body.String(), "No roles match") {
		t.Errorf("expected no-match copy, body=%s", w.Body.String())
	}
}

// TestHandleClaudeConfig_ProjectScoped mirrors the real fleet shape: per-role
// claude-settings beads carry BOTH project: and role: labels, so a project-less
// resolution would miss them. With SetViewerProject wired, it now acts as the
// *default filter*: ci-gardener (project:monorepo) is absent by default and
// present only when explicitly requested via ?project=monorepo.
func TestHandleClaudeConfig_ProjectScoped(t *testing.T) {
	f := newFake()
	f.listResp = &beadsapi.ListBeadsResult{
		Beads: []*beadsapi.BeadDetail{
			claudeSettingsBead("gd-cc-g", []string{"global"}, `{"env":{"CLAUDE_CODE_EFFORT_LEVEL":"high"}}`),
			claudeSettingsBead("gd-cc-crew", []string{"project:gardenio", "role:crew"}, `{"ultracode":true}`),
			claudeSettingsBead("gd-cc-ci", []string{"project:monorepo", "role:ci-gardener"}, `{"model":"claude-opus-4-8"}`),
		},
		Total: 3,
	}
	srv := NewServer(f, testLogger(), "", "", "", nil)
	srv.SetViewerProject("gardenio")
	handler := edgeAuthMux(srv)

	req := httptest.NewRequest("GET", "/claude-config", nil)
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	body := w.Body.String()
	if !strings.Contains(body, ">crew<") {
		t.Error("expected project-scoped crew row (project:gardenio + role:crew)")
	}
	if strings.Contains(body, ">ci-gardener<") {
		t.Error("ci-gardener (project:monorepo) must NOT appear by default for a gardenio viewer")
	}
	// crew flips ultracode but inherits the global effort pin → trap surfaced.
	if !strings.Contains(body, "blocks xhigh") {
		t.Errorf("expected effort-trap marker for crew, body=%s", body)
	}

	// Explicitly requesting project=monorepo surfaces ci-gardener instead.
	req = httptest.NewRequest("GET", "/claude-config?project=monorepo", nil)
	w = httptest.NewRecorder()
	handler.ServeHTTP(w, req)
	body = w.Body.String()
	if !strings.Contains(body, ">ci-gardener<") {
		t.Errorf("expected ci-gardener row for ?project=monorepo, body=%s", body)
	}
	if strings.Contains(body, ">crew<") {
		t.Error("crew (project:gardenio) must NOT appear for ?project=monorepo")
	}
}

// The same role name resolves DIFFERENTLY per project. Both rows must render,
// each resolved against its own project.
func TestHandleClaudeConfig_MultiProjectRows(t *testing.T) {
	f := newFake()
	f.listResp = &beadsapi.ListBeadsResult{
		Beads: []*beadsapi.BeadDetail{
			claudeSettingsBead("gd-cc-gard", []string{"project:gardenio", "role:crew"}, `{"ultracode":true}`),
			claudeSettingsBead("gd-cc-gas", []string{"project:gascity", "role:crew"}, `{"model":"claude-opus-4-8"}`),
		},
		Total: 2,
	}
	mux := mkServer(t, f) // no viewer project → show every project

	req := httptest.NewRequest("GET", "/claude-config", nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	body := w.Body.String()
	if !strings.Contains(body, ">gardenio<") || !strings.Contains(body, ">gascity<") {
		t.Errorf("both project cells must render, body=%s", body)
	}
	if !strings.Contains(body, "claude-opus-4-8") {
		t.Errorf("the gascity crew row must carry its own model value, body=%s", body)
	}
	if !strings.Contains(body, "Showing all projects") {
		t.Errorf("expected the all-projects informational line, body=%s", body)
	}
}

// TestHandleClaudeConfig_DetailHref pins the row link's href shape — the exact
// bug behind gd-n1GkyK0wXf (viewer-e2e claude_config.spec.js red on main): a
// row whose scope.Project equals the viewer's own default must link to the
// PLAIN "/claude-config/{role}" path with no "?project=" query string, since
// that is exactly what handleClaudeConfigDetail falls back to with no query
// param. Appending "?project=" whenever scope.Project != "" (rather than when
// it differs from s.viewerProject) made every row in the common
// single-project deployment carry a redundant query string, breaking any
// selector/link built on the plain per-role path (e.g. the e2e spec's
// `a[href$="/claude-config/<role>"]` locator).
func TestHandleClaudeConfig_DetailHref(t *testing.T) {
	f := newFake()
	f.listResp = &beadsapi.ListBeadsResult{
		Beads: []*beadsapi.BeadDetail{
			claudeSettingsBead("gd-cc-crew", []string{"project:gardenio", "role:crew"}, `{"ultracode":true}`),
		},
		Total: 1,
	}
	srv := NewServer(f, testLogger(), "", "", "", nil)
	srv.SetViewerProject("gardenio")
	handler := edgeAuthMux(srv)

	req := httptest.NewRequest("GET", "/claude-config", nil)
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)

	body := w.Body.String()
	if !strings.Contains(body, `href="/claude-config/crew"`) {
		t.Errorf("expected clean /claude-config/crew href (row project matches viewer default), body=%s", body)
	}
	if strings.Contains(body, `/claude-config/crew?project=`) {
		t.Errorf("row project matches viewer default — href must NOT carry a redundant ?project=, body=%s", body)
	}
}

// TestHandleClaudeConfig_DetailHref_CrossProject is the multi-project twin: a
// row whose scope.Project DIFFERS from the viewer's default (including the
// no-default "" case) must carry ?project= — omitting it would make the
// detail page resolve against the wrong (or no) project.
func TestHandleClaudeConfig_DetailHref_CrossProject(t *testing.T) {
	f := newFake()
	f.listResp = &beadsapi.ListBeadsResult{
		Beads: []*beadsapi.BeadDetail{
			claudeSettingsBead("gd-cc-gard", []string{"project:gardenio", "role:crew"}, `{"ultracode":true}`),
			claudeSettingsBead("gd-cc-gas", []string{"project:gascity", "role:crew"}, `{"model":"claude-opus-4-8"}`),
		},
		Total: 2,
	}
	mux := mkServer(t, f) // no viewer project configured → every row differs from ""

	req := httptest.NewRequest("GET", "/claude-config", nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	body := w.Body.String()
	if !strings.Contains(body, `href="/claude-config/crew?project=gardenio"`) {
		t.Errorf("expected gardenio row to carry ?project=gardenio, body=%s", body)
	}
	if !strings.Contains(body, `href="/claude-config/crew?project=gascity"`) {
		t.Errorf("expected gascity row to carry ?project=gascity, body=%s", body)
	}
}

func TestHandleClaudeConfig_ProjectFilter(t *testing.T) {
	f := newFake()
	f.listResp = &beadsapi.ListBeadsResult{
		Beads: []*beadsapi.BeadDetail{
			claudeSettingsBead("gd-cc-gard", []string{"project:gardenio", "role:crew"}, `{"ultracode":true}`),
			claudeSettingsBead("gd-cc-gas", []string{"project:gascity", "role:crew"}, `{"model":"claude-opus-4-8"}`),
		},
		Total: 2,
	}
	mux := mkServer(t, f)

	req := httptest.NewRequest("GET", "/claude-config?project=gardenio", nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	body := w.Body.String()
	if strings.Contains(body, ">gascity<") {
		t.Errorf("?project=gardenio must exclude gascity rows, body=%s", body)
	}
	if !strings.Contains(body, ">gardenio<") {
		t.Errorf("?project=gardenio must keep gardenio rows, body=%s", body)
	}
}

func TestHandleClaudeConfigDetail_ProjectQueryResolvesThatProject(t *testing.T) {
	f := newFake()
	f.listResp = &beadsapi.ListBeadsResult{
		Beads: []*beadsapi.BeadDetail{
			claudeSettingsBead("gd-cc-gas", []string{"project:gascity", "role:crew"}, `{"model":"claude-opus-4-8"}`),
		},
		Total: 1,
	}
	mux := mkServer(t, f)

	req := httptest.NewRequest("GET", "/claude-config/crew?project=gascity", nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	body := w.Body.String()
	if !strings.Contains(body, "project:gascity") {
		t.Errorf("the resolved subscriptions must name the queried project, body=%s", body)
	}
	if !strings.Contains(body, "claude-opus-4-8") {
		t.Errorf("the gascity bead must resolve, body=%s", body)
	}
}

// TestHandleClaudeConfig_DaemonError degrades to the error banner, not a 500.
func TestHandleClaudeConfig_DaemonError(t *testing.T) {
	f := newFake()
	f.listErr = &beadsapi.APIError{StatusCode: http.StatusInternalServerError, Message: "boom"}
	mux := mkServer(t, f)

	req := httptest.NewRequest("GET", "/claude-config", nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200 (degraded), got %d", w.Code)
	}
	if !strings.Contains(w.Body.String(), "Failed to load role config.") {
		t.Errorf("expected error banner, body=%s", w.Body.String())
	}
}

// TestHandleClaudeConfigDetail renders the per-role detail with the resolved
// settings.json and knob provenance.
func TestHandleClaudeConfigDetail(t *testing.T) {
	f := newFake()
	f.listResp = &beadsapi.ListBeadsResult{
		Beads: []*beadsapi.BeadDetail{
			claudeSettingsBead("gd-cc-thread", []string{"role:thread"}, `{"ultracode":true,"env":{"CLAUDE_CODE_WORKFLOWS":"1"}}`),
		},
		Total: 1,
	}
	mux := mkServer(t, f)

	req := httptest.NewRequest("GET", "/claude-config/thread", nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	body := w.Body.String()
	if !strings.Contains(body, "role <code>thread</code>") {
		t.Errorf("expected role header, body=%s", body)
	}
	// Effective knobs table + resolved settings.json.
	if !strings.Contains(body, "Effective knobs") {
		t.Error("expected knobs table")
	}
	if !strings.Contains(body, "Resolved <code>settings.json</code>") {
		t.Error("expected resolved settings pane")
	}
	// ultracode ON → effort resolves to xhigh (no pin, cleared).
	if !strings.Contains(body, "xhigh") {
		t.Errorf("expected effective effort xhigh under ultracode, body=%s", body)
	}
	// Subscriptions surfaced.
	if !strings.Contains(body, "role:thread") {
		t.Error("expected subscriptions to include role:thread")
	}
}

// Superseded by per-(project, role) resolution: the matrix no longer has a
// blind spot when no default project is configured — it resolves every
// project's row against its OWN project — so the old "hidden beads" warning
// becomes an informational "showing everything" line instead.
func TestHandleClaudeConfig_NoViewerProject_WarnsAndStillRenders(t *testing.T) {
	f := newFake()
	f.listResp = &beadsapi.ListBeadsResult{
		Beads: []*beadsapi.BeadDetail{
			claudeSettingsBead("gd-cc-crew", []string{"project:gardenio", "role:crew"}, `{"ultracode":true}`),
		},
		Total: 1,
	}
	mux := mkServer(t, f) // NewServer without SetViewerProject → viewerProject == ""

	req := httptest.NewRequest("GET", "/claude-config", nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	body := w.Body.String()
	if !strings.Contains(body, "Showing all projects") {
		t.Errorf("expected the all-projects informational line, body=%s", body)
	}
	// Nothing is hidden any more: the role is still discovered and rendered.
	if !strings.Contains(body, ">crew<") {
		t.Errorf("the crew row must still render, body=%s", body)
	}
}

func TestHandleClaudeConfig_WithViewerProject_NoWarning(t *testing.T) {
	f := newFake()
	f.listResp = &beadsapi.ListBeadsResult{
		Beads: []*beadsapi.BeadDetail{
			claudeSettingsBead("gd-cc-crew", []string{"project:gardenio", "role:crew"}, `{"ultracode":true}`),
		},
		Total: 1,
	}
	srv := NewServer(f, testLogger(), "", "", "", nil)
	srv.SetViewerProject("gardenio")
	handler := edgeAuthMux(srv)

	req := httptest.NewRequest("GET", "/claude-config", nil)
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)

	if strings.Contains(w.Body.String(), "No project scope is configured") {
		t.Error("a viewer with a project scope must not show the warning")
	}
}

func TestHandleClaudeConfigDetail_NoViewerProject_Warns(t *testing.T) {
	f := newFake()
	f.listResp = &beadsapi.ListBeadsResult{
		Beads: []*beadsapi.BeadDetail{
			claudeSettingsBead("gd-cc-crew", []string{"project:gardenio", "role:crew"}, `{"ultracode":true}`),
		},
		Total: 1,
	}
	mux := mkServer(t, f)

	req := httptest.NewRequest("GET", "/claude-config/crew", nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if !strings.Contains(w.Body.String(), "No project scope is configured") {
		t.Errorf("the detail page must carry the same warning, body=%s", w.Body.String())
	}
}
