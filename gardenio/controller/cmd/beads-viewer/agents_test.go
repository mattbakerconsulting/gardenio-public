package main

import (
	"bytes"
	"context"
	"errors"
	"html/template"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"
	"time"

	"gardenio/controller/internal/beadsapi"
	"gardenio/controller/internal/coopmuxapi"
)

// fakeScreenClient implements screenClient for tests.
type fakeScreenClient struct {
	disabled bool
	screen   *coopmuxapi.Screen
	err      error
	lastID   string

	// GetAgentState hooks.
	agentState       *coopmuxapi.AgentState
	agentStateErr    error
	agentStateCalls  int
	agentStateLastID string

	// SendInput hooks.
	inputErr      error
	inputCalls    int
	inputLastID   string
	inputLastText string

	// SendKeys hooks.
	keysErr      error
	keysCalls    int
	keysLastID   string
	keysLastKeys []string

	// Nudge hooks.
	nudgeErr      error
	nudgeCalls    int
	nudgeLastID   string
	nudgeLastText string

	// Respond hooks.
	respondErr     error
	respondCalls   int
	respondLastID  string
	respondLastReq coopmuxapi.RespondRequest
}

func (f *fakeScreenClient) Disabled() bool { return f.disabled }

func (f *fakeScreenClient) GetScreen(_ context.Context, sessionID string) (*coopmuxapi.Screen, error) {
	f.lastID = sessionID
	return f.screen, f.err
}

func (f *fakeScreenClient) GetAgentState(_ context.Context, sessionID string) (*coopmuxapi.AgentState, error) {
	f.agentStateCalls++
	f.agentStateLastID = sessionID
	return f.agentState, f.agentStateErr
}

func (f *fakeScreenClient) SendInput(_ context.Context, sessionID, text string) error {
	f.inputCalls++
	f.inputLastID = sessionID
	f.inputLastText = text
	return f.inputErr
}

func (f *fakeScreenClient) SendKeys(_ context.Context, sessionID string, keys []string) error {
	f.keysCalls++
	f.keysLastID = sessionID
	f.keysLastKeys = append([]string(nil), keys...)
	return f.keysErr
}

func (f *fakeScreenClient) Nudge(_ context.Context, sessionID, message string) error {
	f.nudgeCalls++
	f.nudgeLastID = sessionID
	f.nudgeLastText = message
	return f.nudgeErr
}

func (f *fakeScreenClient) Respond(_ context.Context, sessionID string, req coopmuxapi.RespondRequest) error {
	f.respondCalls++
	f.respondLastID = sessionID
	f.respondLastReq = req
	return f.respondErr
}

func mkAgentServer(t *testing.T, daemon beadAPI, coopmux screenClient) http.Handler {
	t.Helper()
	srv := NewServer(daemon, slog.Default(), "", "", "", coopmux)
	mux := http.NewServeMux()
	srv.RegisterRoutes(mux)
	return edgeAuth(mux)
}

func TestHandleAgents_Empty(t *testing.T) {
	f := newFake()
	f.listResp = &beadsapi.ListBeadsResult{Beads: nil, Total: 0}
	mux := mkServer(t, f)

	req := httptest.NewRequest("GET", "/agents", nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	body := w.Body.String()
	if !strings.Contains(body, "No Agents") {
		t.Errorf("expected empty-state copy, body=%s", body)
	}
	// Active-list query: Types=[agent], Statuses=[open, in_progress].
	// /agents fans the active + recent calls out concurrently so the
	// recorded order is non-deterministic — look up by content instead.
	active, ok := findQuery(f.listAllQ, func(q beadsapi.ListBeadsQuery) bool {
		return slices.Contains(q.Statuses, "open") && slices.Contains(q.Statuses, "in_progress")
	})
	if !ok {
		t.Fatalf("expected an active-agent ListBeadsFiltered call, got %d calls: %+v", len(f.listAllQ), f.listAllQ)
	}
	if got := active.Types; len(got) != 1 || got[0] != "agent" {
		t.Errorf("active-call types = %v, want [agent]", got)
	}
	if len(active.Labels) != 0 {
		t.Errorf("active-call labels = %v, want none (no project filter)", active.Labels)
	}
}

// findQuery returns the first recorded ListBeadsFiltered call satisfying
// pred. Used by tests of handlers that fan multiple list calls out
// concurrently (handleAgents, handleHealth, handleInbox) where the
// recording order is non-deterministic.
func findQuery(qs []beadsapi.ListBeadsQuery, pred func(beadsapi.ListBeadsQuery) bool) (beadsapi.ListBeadsQuery, bool) {
	for _, q := range qs {
		if pred(q) {
			return q, true
		}
	}
	return beadsapi.ListBeadsQuery{}, false
}

func TestHandleAgents_NonEmpty_RendersActiveAndRecent(t *testing.T) {
	now := time.Now()
	f := newFake()
	f.listResp = &beadsapi.ListBeadsResult{
		Beads: []*beadsapi.BeadDetail{
			{
				ID:        "gd-ag1",
				Title:     "crew-alpha-7q3z",
				Type:      "agent",
				Status:    "in_progress",
				Labels:    []string{"project:gardenio"},
				UpdatedAt: now.Add(-5 * time.Minute),
				Fields: map[string]string{
					"agent":          "crew-alpha-7q3z",
					"role":           "crew",
					"agent_state":    "working",
					"pod_phase":      "Running",
					"task_id":        "gd-task-1",
					"task_title":     "Implement /agents page",
					"project":        "gardenio",
					"context_tokens": "128000",
					"live_cost_usd":  "1.50",
					"live_turns":     "47",
				},
			},
			{
				ID:        "gd-ag2",
				Title:     "researcher-beta-9k1",
				Type:      "agent",
				Status:    "in_progress",
				Labels:    []string{"project:kbeads"},
				UpdatedAt: now.Add(-30 * time.Minute),
				Fields: map[string]string{
					"agent":       "researcher-beta-9k1",
					"role":        "researcher",
					"agent_state": "idle",
					"project":     "kbeads",
				},
			},
		},
		Total: 2,
	}
	f.agentRecentResp = &beadsapi.ListBeadsResult{
		Beads: []*beadsapi.BeadDetail{
			{
				ID:          "gd-ag3",
				Title:       "crew-gamma-old",
				Type:        "agent",
				Status:      "closed",
				Labels:      []string{"project:gardenio"},
				UpdatedAt:   now.Add(-2 * time.Hour),
				Description: "Worked on the previous decisions page MR.",
				Fields: map[string]string{
					"agent":   "crew-gamma-old",
					"role":    "crew",
					"project": "gardenio",
				},
			},
		},
		Total: 1,
	}
	mux := mkServer(t, f)

	req := httptest.NewRequest("GET", "/agents", nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	body := w.Body.String()

	for _, want := range []string{
		// Active agents and per-row content.
		"crew-alpha-7q3z",
		"researcher-beta-9k1",
		"Implement /agents page",
		"working",
		"idle",
		// Context-window occupancy now renders in its own sortable Context
		// column as an always-visible gauge (ka-0000006399): 128k → 76% of the
		// ~167k auto-compact threshold on the default 200k window. The column
		// header carries the click-to-sort hook; the cell carries the gauge and
		// the numeric sort key.
		`data-sort-key="ctx"`,
		`class="agents-table-ctx" data-ctx-pct="76"`,
		"agent-ctx-mini",
		"76%",
		// Cost + turns live in the Usage cell (context split out above).
		"$1.50",
		// The 🔁 turns chip — the triad's third metric, driven off
		// live_turns while the agent runs (ka-0000001199).
		"🔁 47",
		// Pod phase renders as a meta span in the compact (default) view.
		">Running<",
		`href="/gd-ag1"`,
		`href="/gd-ag2"`,
		// Linked task page.
		`href="/gd-task-1"`,
		"gd-task-1",
		// Recent section.
		"<h2>Recent",
		"crew-gamma-old",
		// Project chip strip.
		"<h2>gardenio",
		"<h2>kbeads",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("agents page missing %q\nbody=%s", want, body)
		}
	}
	if strings.Contains(body, "No Agents") {
		t.Errorf("unexpected empty-state when agents present")
	}

	// Two daemon calls — active and recent — fanned out concurrently. Look
	// up the recent (closed) call by content rather than position.
	if len(f.listAllQ) < 2 {
		t.Fatalf("expected 2 daemon calls, got %d", len(f.listAllQ))
	}
	recentQ, ok := findQuery(f.listAllQ, func(q beadsapi.ListBeadsQuery) bool {
		return slices.Contains(q.Types, "agent") && slices.Contains(q.Statuses, "closed")
	})
	if !ok {
		t.Fatalf("expected a recent-agent (closed) ListBeadsFiltered call, got %+v", f.listAllQ)
	}
	if !slices.Contains(recentQ.Types, "agent") {
		t.Errorf("recent call types = %v, want [agent]", recentQ.Types)
	}
	if !slices.Contains(recentQ.Statuses, "closed") {
		t.Errorf("recent call statuses = %v, want [closed]", recentQ.Statuses)
	}
}

// TestHandleAgents_HidesUnengagedPrewarmedAgents verifies that prewarmed-pool
// members (open type=agent beads with agent_state=prewarmed) are dropped from
// the active fleet so they don't render as idle agent cards before anyone
// engages them (ka-0000005111). An assigned pool member (agent_state=working)
// must still render — assignment is what makes a pod real work.
func TestHandleAgents_HidesUnengagedPrewarmedAgents(t *testing.T) {
	now := time.Now()
	f := newFake()
	f.listResp = &beadsapi.ListBeadsResult{
		Beads: []*beadsapi.BeadDetail{
			{
				ID:        "gd-pool1",
				Title:     "prewarmed-1780966109-5",
				Type:      "agent",
				Status:    "open",
				Labels:    []string{"project:gardenio", "prewarmed"},
				UpdatedAt: now.Add(-1 * time.Minute),
				Fields: map[string]string{
					"agent":       "prewarmed-1780966109-5",
					"role":        "thread",
					"agent_state": "prewarmed",
					"project":     "gardenio",
				},
			},
			{
				ID:        "gd-live1",
				Title:     "crew-engaged",
				Type:      "agent",
				Status:    "in_progress",
				Labels:    []string{"project:gardenio"},
				UpdatedAt: now.Add(-2 * time.Minute),
				Fields: map[string]string{
					"agent":       "crew-engaged",
					"role":        "crew",
					"agent_state": "working",
					"project":     "gardenio",
				},
			},
		},
		Total: 2,
	}
	mux := mkServer(t, f)

	req := httptest.NewRequest("GET", "/agents", nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	body := w.Body.String()

	// Engaged agent renders; unengaged prewarmed pool member does not.
	if !strings.Contains(body, "crew-engaged") {
		t.Errorf("expected engaged agent crew-engaged to render, body=%s", body)
	}
	if strings.Contains(body, "prewarmed-1780966109-5") {
		t.Errorf("unengaged prewarmed pool agent leaked into the active list, body=%s", body)
	}
	if strings.Contains(body, `href="/gd-pool1"`) {
		t.Errorf("unengaged prewarmed pool agent row linked into the active list, body=%s", body)
	}
}

func TestHandleAgents_ProjectFilter_FiltersAndKeepsBackLink(t *testing.T) {
	now := time.Now()
	f := newFake()
	f.listResp = &beadsapi.ListBeadsResult{
		Beads: []*beadsapi.BeadDetail{
			{
				ID:        "gd-ag-g",
				Title:     "crew-gardenio",
				Type:      "agent",
				Status:    "in_progress",
				Labels:    []string{"project:gardenio"},
				UpdatedAt: now,
				Fields:    map[string]string{"agent": "crew-gardenio", "project": "gardenio"},
			},
			{
				ID:        "gd-ag-k",
				Title:     "crew-kbeads",
				Type:      "agent",
				Status:    "in_progress",
				Labels:    []string{"project:kbeads"},
				UpdatedAt: now,
				Fields:    map[string]string{"agent": "crew-kbeads", "project": "kbeads"},
			},
		},
		Total: 2,
	}
	mux := mkServer(t, f)

	req := httptest.NewRequest("GET", "/agents?project=gardenio", nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	body := w.Body.String()

	// Only the gardenio-project agent appears in the rendered groups.
	if !strings.Contains(body, "crew-gardenio") {
		t.Errorf("expected crew-gardenio to render, body=%s", body)
	}
	// kbeads agent is still in the chip strip (link), but should NOT appear
	// as a group row when filtered by ?project=gardenio.
	if strings.Contains(body, "<h2>kbeads") {
		t.Errorf("kbeads project section rendered despite ?project=gardenio filter, body=%s", body)
	}
	// Active-chip highlight on the selected project.
	if !strings.Contains(body, "chip-active") {
		t.Errorf("expected an active chip when project filter is set, body=%s", body)
	}
	// Header reflects the filter.
	if !strings.Contains(body, "Agents · gardenio") {
		t.Errorf("expected filtered header, body=%s", body)
	}
}

// TestHandleAgents_OperatorChipLinksToUserHomepage verifies that the `op …`
// meta chip on each agent row links to the operator's /u/{slug} homepage
// when a slug is resolvable. ka-0000007502: prior to this fix the operator
// display rendered as plain text, so clicking it did nothing.
func TestHandleAgents_OperatorChipLinksToUserHomepage(t *testing.T) {
	now := time.Now()
	opID := int64(42)
	f := newFake()
	f.opSlugResp = map[int64]*beadsapi.GetOperatorSlugResponse{
		42: {Slug: "matt-baker", Found: true},
	}
	f.listResp = &beadsapi.ListBeadsResult{
		Beads: []*beadsapi.BeadDetail{
			{
				ID:                "gd-ag1",
				Title:             "crew-alpha",
				Type:              "agent",
				Status:            "in_progress",
				Labels:            []string{"project:gardenio"},
				OperatedByID:      &opID,
				OperatedByDisplay: "Matt Baker",
				UpdatedAt:         now.Add(-3 * time.Minute),
				Fields: map[string]string{
					"agent":       "crew-alpha",
					"role":        "crew",
					"agent_state": "working",
					"task_id":     "gd-task-1",
					"task_title":  "Refactor agents page",
					"project":     "gardenio",
				},
			},
		},
		Total: 1,
	}
	mux := mkServer(t, f)

	req := httptest.NewRequest("GET", "/agents", nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	body := w.Body.String()
	if !strings.Contains(body, `href="/u/matt-baker"`) {
		t.Errorf("expected operator chip to link to /u/matt-baker; body=%s", body)
	}
	if !strings.Contains(body, `>Matt Baker<`) {
		t.Errorf("expected display name Matt Baker to render inside the link; body=%s", body)
	}
	// Owner-initials avatar badge: a compact "MB" monogram with the full
	// owner name on hover, linked to the operator homepage (ka-0000001514).
	if !strings.Contains(body, `class="owner-initials"`) {
		t.Errorf("expected owner-initials badge on the agent row; body=%s", body)
	}
	if !strings.Contains(body, `title="owner: Matt Baker"`) {
		t.Errorf("expected full owner name on hover via title=; body=%s", body)
	}
	if !strings.Contains(body, ">MB</a>") {
		t.Errorf("expected MB initials inside the linked owner badge; body=%s", body)
	}
}

// TestHandleAgents_OperatorChipFallbackWhenNoSlug confirms the chip falls
// back to plain (escaped) text when the operator has no canonical slug
// resolvable, so list rendering doesn't regress for unresolved operators.
func TestHandleAgents_OperatorChipFallbackWhenNoSlug(t *testing.T) {
	now := time.Now()
	opID := int64(43)
	f := newFake()
	// No opSlugResp entry for 43 → daemon returns Found=false.
	f.listResp = &beadsapi.ListBeadsResult{
		Beads: []*beadsapi.BeadDetail{
			{
				ID:                "gd-ag1",
				Title:             "crew-alpha",
				Type:              "agent",
				Status:            "in_progress",
				Labels:            []string{"project:gardenio"},
				OperatedByID:      &opID,
				OperatedByDisplay: "Unknown User",
				UpdatedAt:         now.Add(-3 * time.Minute),
				Fields: map[string]string{
					"agent":   "crew-alpha",
					"role":    "crew",
					"project": "gardenio",
				},
			},
		},
		Total: 1,
	}
	mux := mkServer(t, f)

	req := httptest.NewRequest("GET", "/agents", nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	body := w.Body.String()
	// resolveOperatorSlug falls through to the op-<id> opaque sentinel slug,
	// so the link is still emitted — just pointing at the sentinel route
	// which still resolves to the operator's homepage via the retired-slug
	// branch in LookupOperatorBySlug.
	if !strings.Contains(body, "Unknown User") {
		t.Errorf("expected display name to render even without canonical slug; body=%s", body)
	}
}

// TestHandleAgents_CompactDensity_RendersTable verifies that ?density=compact
// switches the /agents page to a table view with column headers and all the
// agent's data inline. Fixes gd-SDSGqoEXAK.
func TestHandleAgents_CompactDensity_RendersTable(t *testing.T) {
	now := time.Now()
	f := newFake()
	f.listResp = &beadsapi.ListBeadsResult{
		Beads: []*beadsapi.BeadDetail{
			{
				ID:        "gd-ag1",
				Title:     "crew-alpha",
				Type:      "agent",
				Status:    "in_progress",
				Labels:    []string{"project:gardenio"},
				UpdatedAt: now.Add(-3 * time.Minute),
				Fields: map[string]string{
					"agent":       "crew-alpha",
					"role":        "crew",
					"agent_state": "working",
					"pod_phase":   "Running",
					"task_id":     "gd-task-1",
					"task_title":  "Refactor agents page",
					"project":     "gardenio",
				},
			},
		},
		Total: 1,
	}
	mux := mkServer(t, f)

	req := httptest.NewRequest("GET", "/agents?density=compact", nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	body := w.Body.String()
	for _, want := range []string{
		`class="agents-table"`,
		"<th>Agent</th>",
		`<th title="Owner initials — hover for full name">Owner</th>`,
		"<th>Role</th>",
		"<th>State</th>",
		"<th>Task</th>",
		"<th>Links</th>",
		"<th>Up</th>",
		"<th>Active</th>",
		"crew-alpha",
		"Refactor agents page",
		"working",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("compact /agents missing %q", want)
		}
	}
	// The agent-bead-ID column was replaced by Links (operators reported the
	// raw ID column was not useful; the bead ID is now exposed via the agent
	// name link's title attribute for hover access).
	for _, unwanted := range []string{
		"<th>ID</th>",
	} {
		if strings.Contains(body, unwanted) {
			t.Errorf("compact /agents should not contain %q", unwanted)
		}
	}
	// The default list-view markup ("bead-row-text") should NOT render in
	// compact mode.
	if strings.Contains(body, `class="bead-row-text"`) {
		t.Errorf("compact view should not render list-view rows")
	}
}

// TestHandleAgents_ProjectFilter_RendersFleetTileAndClearChip verifies that
// /agents?project=<name> renders the per-project fleet tile (alive/stuck/idle
// /total) and a clear-filter chip with an x-to-clear link back to /agents.
// Covers gd-3w1vbuj7hd info-density additions for /agents-filtered.
func TestHandleAgents_ProjectFilter_RendersFleetTileAndClearChip(t *testing.T) {
	now := time.Now()
	f := newFake()
	f.listResp = &beadsapi.ListBeadsResult{
		Beads: []*beadsapi.BeadDetail{
			{
				ID:        "gd-ag-w",
				Type:      "agent",
				Status:    "in_progress",
				Labels:    []string{"project:gardenio"},
				UpdatedAt: now,
				Fields:    map[string]string{"agent": "w", "agent_state": "working", "project": "gardenio"},
			},
			{
				ID:        "gd-ag-s",
				Type:      "agent",
				Status:    "in_progress",
				Labels:    []string{"project:gardenio"},
				UpdatedAt: now,
				Fields:    map[string]string{"agent": "s", "agent_state": "spawning", "project": "gardenio"},
			},
			{
				ID:        "gd-ag-f",
				Type:      "agent",
				Status:    "in_progress",
				Labels:    []string{"project:gardenio"},
				UpdatedAt: now,
				Fields:    map[string]string{"agent": "f", "agent_state": "failed", "project": "gardenio"},
			},
			{
				ID:        "gd-ag-i",
				Type:      "agent",
				Status:    "in_progress",
				Labels:    []string{"project:gardenio"},
				UpdatedAt: now,
				Fields:    map[string]string{"agent": "i", "agent_state": "idle", "project": "gardenio"},
			},
			// Different project — must not contribute to the gardenio fleet tile.
			{
				ID:        "gd-ag-k",
				Type:      "agent",
				Status:    "in_progress",
				Labels:    []string{"project:kbeads"},
				UpdatedAt: now,
				Fields:    map[string]string{"agent": "k", "agent_state": "working", "project": "kbeads"},
			},
		},
		Total: 5,
	}
	mux := mkServer(t, f)

	req := httptest.NewRequest("GET", "/agents?project=gardenio", nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	body := w.Body.String()

	// Fleet tile header + counts (alive=2, stuck=1, idle=1, total=4 — kbeads agent excluded).
	for _, want := range []string{
		`class="fleet-tile"`,
		"gardenio fleet",
		// dd values render in fleet-tile-list — assert each count surface.
		`<dt>Alive</dt><dd><strong class="ok">2</strong></dd>`,
		`<dt>Stuck</dt><dd>`,
		`<strong class="err">1</strong>`,
		`<dt>Idle</dt><dd>1</dd>`,
		`<dt>Total</dt><dd>4</dd>`,
		// Clear-filter chip with x-to-clear link back to unfiltered view.
		// Default density is "compact" (ka-0000004132), so the clear link
		// preserves it via the density query param.
		`data-filter="project"`,
		"project: gardenio",
		`<a class="chip-clear" href="/agents?density=compact"`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("agents-filtered page missing %q\nbody=%s", want, body)
		}
	}
}

// TestHandleAgents_DormantBucket_EngineSetNotDerived verifies the parked/dormant
// bucket (ka-0000004675): a CLOSED agent carrying the engine-set field
// agent_state=dormant renders in its own Dormant section and count, is queried
// with a dedicated window-independent field filter (so a long-parked identity
// with a far-future next_respawn_at never ages out of the recent window), and is
// NOT double-counted in the Recent/Closed24h buckets.
func TestHandleAgents_DormantBucket_EngineSetNotDerived(t *testing.T) {
	now := time.Now()
	f := newFake()
	// One live agent so the fleet tile renders.
	f.listResp = &beadsapi.ListBeadsResult{
		Beads: []*beadsapi.BeadDetail{
			{
				ID:        "gd-live",
				Type:      "agent",
				Status:    "in_progress",
				Labels:    []string{"project:gardenio"},
				UpdatedAt: now,
				Fields:    map[string]string{"agent": "live", "agent_state": "working", "project": "gardenio"},
			},
		},
		Total: 1,
	}
	// The dormant bucket is served by its OWN query, disjoint from recent. The
	// parked identity carries a far-future next_respawn_at and was updated 3 days
	// ago — proving the bucket does NOT depend on a recent-activity window.
	f.agentDormantResp = &beadsapi.ListBeadsResult{
		Beads: []*beadsapi.BeadDetail{
			{
				ID:        "gd-parked",
				Title:     "gardenio-thread-parked",
				Type:      "agent",
				Status:    "closed",
				Labels:    []string{"project:gardenio"},
				UpdatedAt: now.Add(-72 * time.Hour),
				Fields: map[string]string{
					"agent":           "gardenio-thread-parked",
					"role":            "crew",
					"project":         "gardenio",
					"agent_state":     "dormant",
					"successor":       "same",
					"next_respawn_at": "2099-01-01T00:00:00Z",
					"close_reason":    "idle_ttl_parked",
				},
			},
		},
		Total: 1,
	}
	mux := mkServer(t, f)

	req := httptest.NewRequest("GET", "/agents?project=gardenio", nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	body := w.Body.String()

	for _, want := range []string{
		"gardenio-thread-parked", // the parked identity renders
		"dormant",                // count/label surface
	} {
		if !strings.Contains(body, want) {
			t.Errorf("agents page missing dormant surface %q\nbody=%s", want, body)
		}
	}

	// The dormant query MUST be a dedicated closed-agent call filtered on the
	// engine-set field agent_state=dormant — not derived from the recent list.
	dormantQ, ok := findQuery(f.listAllQ, func(q beadsapi.ListBeadsQuery) bool {
		return slices.Contains(q.Types, "agent") &&
			slices.Contains(q.Statuses, "closed") &&
			q.Fields[beadsapi.FieldAgentState] == "dormant"
	})
	if !ok {
		t.Fatalf("expected a dedicated dormant (agent_state=dormant) ListBeadsFiltered call, got %+v", f.listAllQ)
	}
	if dormantQ.Fields[beadsapi.FieldAgentState] != "dormant" {
		t.Errorf("dormant call field filter = %v, want agent_state=dormant", dormantQ.Fields)
	}
}

// TestHandleAgents_NoFilter_OmitsFleetTile verifies the unfiltered /agents
// view still renders without the per-project tile, so the existing /agents
// layout is unchanged.
func TestHandleAgents_NoFilter_OmitsFleetTile(t *testing.T) {
	f := newFake()
	f.listResp = &beadsapi.ListBeadsResult{
		Beads: []*beadsapi.BeadDetail{
			{
				ID:     "gd-ag1",
				Type:   "agent",
				Status: "in_progress",
				Labels: []string{"project:gardenio"},
				Fields: map[string]string{"agent": "a", "agent_state": "working"},
			},
		},
		Total: 1,
	}
	mux := mkServer(t, f)
	req := httptest.NewRequest("GET", "/agents", nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	body := w.Body.String()
	// Match rendered tile element (not the CSS class definition in <style>).
	if strings.Contains(body, `class="fleet-tile"`) {
		t.Errorf("did not expect fleet tile on unfiltered /agents, body=%s", body)
	}
	if strings.Contains(body, `class="chip-clear"`) {
		t.Errorf("did not expect clear-filter chip on unfiltered /agents, body=%s", body)
	}
}

// TestHandleAgents_ProjectFilter_RendersProjectContextStrip verifies that
// /agents?project=<name> appends a project-context strip with open-bead
// counts, blocked work, stale work, and top owners pulled from the same
// data /p/<project> renders. Covers ka-0000004331 (info-density: when a
// project filter is active, /agents stops being a duplicate of unfiltered
// /agents with only a banner difference).
func TestHandleAgents_ProjectFilter_RendersProjectContextStrip(t *testing.T) {
	now := time.Now()
	staleUpdated := now.Add(-30 * time.Hour)
	f := newFake()
	f.listResp = &beadsapi.ListBeadsResult{
		Beads: []*beadsapi.BeadDetail{
			{
				ID:        "gd-pc-1",
				Type:      "task",
				Status:    "in_progress",
				Labels:    []string{"project:gardenio"},
				Assignee:  "alice",
				UpdatedAt: staleUpdated, // stale > 24h
				CreatedAt: now.Add(-2 * time.Hour),
			},
			{
				ID:                "gd-pc-2",
				Type:              "bug",
				Status:            "open",
				Labels:            []string{"project:gardenio"},
				Assignee:          "alice",
				BlockedOnDecision: true,
				UpdatedAt:         now.Add(-1 * time.Hour),
				CreatedAt:         now.Add(-1 * time.Hour),
			},
			{
				ID:        "gd-pc-3",
				Type:      "task",
				Status:    "open",
				Labels:    []string{"project:gardenio"},
				Assignee:  "bob",
				UpdatedAt: now,
				CreatedAt: now,
			},
		},
		Total: 3,
	}
	mux := mkServer(t, f)

	req := httptest.NewRequest("GET", "/agents?project=gardenio", nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	body := w.Body.String()

	for _, want := range []string{
		`class="project-header-band agents-project-context"`,
		"Active beads",
		`href="/p/gardenio?status=open"`,
		"Blocked",
		"Stale &gt;24h",
		"Top owners",
		"alice",
		"bob",
		`href="/p/gardenio"`,
		"all gardenio beads",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("agents project-context strip missing %q\nbody=%s", want, body)
		}
	}
}

// TestHandleAgents_NoFilter_OmitsProjectContext verifies the project-context
// strip is hidden on unfiltered /agents — the strip is only meaningful when a
// single project is in scope.
func TestHandleAgents_NoFilter_OmitsProjectContext(t *testing.T) {
	f := newFake()
	f.listResp = &beadsapi.ListBeadsResult{
		Beads: []*beadsapi.BeadDetail{
			{
				ID:     "gd-pc-only",
				Type:   "task",
				Status: "open",
				Labels: []string{"project:gardenio"},
			},
		},
		Total: 1,
	}
	mux := mkServer(t, f)

	req := httptest.NewRequest("GET", "/agents", nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	if strings.Contains(w.Body.String(), "agents-project-context") {
		t.Errorf("did not expect project-context strip on unfiltered /agents")
	}
}

// TestBuildProjectFleetTile_Classification unit-tests the agent_state →
// alive/stuck/idle classification independent of the HTTP plumbing.
func TestBuildProjectFleetTile_Classification(t *testing.T) {
	beads := []*beadsapi.BeadDetail{
		// Alive bucket.
		{Labels: []string{"project:gardenio"}, Fields: map[string]string{"agent_state": "working"}},
		{Labels: []string{"project:gardenio"}, Fields: map[string]string{"agent_state": "spawning"}},
		{Labels: []string{"project:gardenio"}, Fields: map[string]string{"agent_state": "yielding"}},
		// Stuck bucket.
		{Labels: []string{"project:gardenio"}, Fields: map[string]string{"agent_state": "failed"}},
		{Labels: []string{"project:gardenio"}, Fields: map[string]string{"agent_state": "rate_limited"}},
		{Labels: []string{"project:gardenio"}, Fields: map[string]string{"agent_state": "budget_exceeded"}},
		// Idle bucket (explicit idle + empty + unknown all fold here).
		{Labels: []string{"project:gardenio"}, Fields: map[string]string{"agent_state": "idle"}},
		{Labels: []string{"project:gardenio"}, Fields: map[string]string{"agent_state": ""}},
		{Labels: []string{"project:gardenio"}, Fields: map[string]string{"agent_state": "done"}},
		// Different project — must be ignored.
		{Labels: []string{"project:kbeads"}, Fields: map[string]string{"agent_state": "working"}},
		// Nil entry — must be tolerated.
		nil,
	}
	now := time.Now()
	// Two recent (within 24h) project-matching, one out-of-window, one
	// other-project. Only the first two should fold into Closed24h=2.
	recent := []*beadsapi.BeadDetail{
		{Labels: []string{"project:gardenio"}, UpdatedAt: now.Add(-1 * time.Hour)},
		{Labels: []string{"project:gardenio"}, UpdatedAt: now.Add(-23 * time.Hour)},
		{Labels: []string{"project:gardenio"}, UpdatedAt: now.Add(-25 * time.Hour)},
		{Labels: []string{"project:kbeads"}, UpdatedAt: now.Add(-1 * time.Hour)},
		nil,
	}
	// Dormant bucket: two in-project parked identities from the dedicated
	// engine-set query (one aged past the 24h window to prove the count is
	// window-independent), plus an other-project one that must be ignored and a
	// closed-but-not-dormant bead that must NOT count.
	dormant := []*beadsapi.BeadDetail{
		{Status: "closed", Labels: []string{"project:gardenio"}, Fields: map[string]string{"agent_state": "dormant"}, UpdatedAt: now.Add(-1 * time.Hour)},
		{Status: "closed", Labels: []string{"project:gardenio"}, Fields: map[string]string{"agent_state": "dormant"}, UpdatedAt: now.Add(-72 * time.Hour)},
		{Status: "closed", Labels: []string{"project:kbeads"}, Fields: map[string]string{"agent_state": "dormant"}},
		{Status: "closed", Labels: []string{"project:gardenio"}, Fields: map[string]string{"agent_state": "done"}},
		nil,
	}
	got := buildProjectFleetTile(beads, recent, dormant, "gardenio", now)
	if got == nil {
		t.Fatal("expected non-nil tile when project is set")
	}
	if got.Alive != 3 || got.Stuck != 3 || got.Idle != 3 || got.Total != 9 {
		t.Errorf("classification = alive=%d stuck=%d idle=%d total=%d; want 3/3/3/9",
			got.Alive, got.Stuck, got.Idle, got.Total)
	}
	if got.Closed24h != 2 {
		t.Errorf("Closed24h = %d; want 2", got.Closed24h)
	}
	if got.Dormant != 2 {
		t.Errorf("Dormant = %d; want 2 (engine-set, window-independent)", got.Dormant)
	}
	// State breakdown: each of the 9 in-project agents contributes one count
	// to its raw agent_state, with "" folded to "idle". So expected counts
	// are 1 each for working/spawning/yielding/failed/rate_limited/
	// budget_exceeded/done plus 2 for idle (explicit + empty). Order is by
	// count desc, then state asc.
	wantStates := []agentStateCount{
		{State: "idle", Count: 2},
		{State: "budget_exceeded", Count: 1},
		{State: "done", Count: 1},
		{State: "failed", Count: 1},
		{State: "rate_limited", Count: 1},
		{State: "spawning", Count: 1},
		{State: "working", Count: 1},
		{State: "yielding", Count: 1},
	}
	if len(got.StateBreakdown) != len(wantStates) {
		t.Fatalf("StateBreakdown len = %d; want %d (%+v)", len(got.StateBreakdown), len(wantStates), got.StateBreakdown)
	}
	for i, want := range wantStates {
		if got.StateBreakdown[i] != want {
			t.Errorf("StateBreakdown[%d] = %+v; want %+v", i, got.StateBreakdown[i], want)
		}
	}
	if buildProjectFleetTile(beads, recent, dormant, "", now) != nil {
		t.Errorf("expected nil tile when project is empty")
	}
}

// TestHandleAgents_RecentOnly_NoEmptyState verifies the empty-state line is
// suppressed when there are no active agents but Recent is populated — the
// old behavior contradicted itself ("No Agents" above a non-empty Recent
// list). Fixes gd-SDSGqoEXAK.
func TestHandleAgents_RecentOnly_NoEmptyState(t *testing.T) {
	now := time.Now()
	f := newFake()
	f.listResp = &beadsapi.ListBeadsResult{Beads: nil, Total: 0}
	f.agentRecentResp = &beadsapi.ListBeadsResult{
		Beads: []*beadsapi.BeadDetail{
			{
				ID:        "gd-ag-old",
				Title:     "crew-old",
				Type:      "agent",
				Status:    "closed",
				Labels:    []string{"project:gardenio"},
				UpdatedAt: now.Add(-3 * time.Hour),
				Fields:    map[string]string{"agent": "crew-old", "project": "gardenio"},
			},
		},
		Total: 1,
	}
	mux := mkServer(t, f)

	req := httptest.NewRequest("GET", "/agents", nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	body := w.Body.String()
	if strings.Contains(body, "No Agents") {
		t.Errorf("empty-state should be suppressed when Recent is non-empty, body=%s", body)
	}
	if !strings.Contains(body, "crew-old") {
		t.Errorf("recent agent should still render, body=%s", body)
	}
}

// TestHandleAgents_Recent_FiltersByActivityWindow verifies the Recent section
// only includes closed agents whose updated_at is within agentsRecentWindow.
// Closed agents older than the window must be discarded — they should not
// leak into the "recent" view just because activity is sparse (ka-0000001416).
func TestHandleAgents_Recent_FiltersByActivityWindow(t *testing.T) {
	now := time.Now()
	f := newFake()
	f.listResp = &beadsapi.ListBeadsResult{Beads: nil, Total: 0}
	f.agentRecentResp = &beadsapi.ListBeadsResult{
		Beads: []*beadsapi.BeadDetail{
			{
				ID:        "gd-recent-in",
				Title:     "crew-fresh",
				Type:      "agent",
				Status:    "closed",
				Labels:    []string{"project:gardenio"},
				UpdatedAt: now.Add(-1 * time.Hour),
				Fields:    map[string]string{"agent": "crew-fresh", "project": "gardenio"},
			},
			{
				ID:        "gd-recent-edge",
				Title:     "crew-edge",
				Type:      "agent",
				Status:    "closed",
				Labels:    []string{"project:gardenio"},
				UpdatedAt: now.Add(-agentsRecentWindow + time.Minute),
				Fields:    map[string]string{"agent": "crew-edge", "project": "gardenio"},
			},
			{
				ID:        "gd-recent-out",
				Title:     "crew-ancient",
				Type:      "agent",
				Status:    "closed",
				Labels:    []string{"project:gardenio"},
				UpdatedAt: now.Add(-agentsRecentWindow - time.Hour),
				Fields:    map[string]string{"agent": "crew-ancient", "project": "gardenio"},
			},
			{
				ID:        "gd-recent-old",
				Title:     "crew-veryold",
				Type:      "agent",
				Status:    "closed",
				Labels:    []string{"project:gardenio"},
				UpdatedAt: now.Add(-72 * time.Hour),
				Fields:    map[string]string{"agent": "crew-veryold", "project": "gardenio"},
			},
		},
		Total: 4,
	}
	mux := mkServer(t, f)

	req := httptest.NewRequest("GET", "/agents", nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)
	body := w.Body.String()

	for _, want := range []string{"crew-fresh", "crew-edge"} {
		if !strings.Contains(body, want) {
			t.Errorf("agent within window missing: %q\nbody=%s", want, body)
		}
	}
	for _, gone := range []string{"crew-ancient", "crew-veryold"} {
		if strings.Contains(body, gone) {
			t.Errorf("agent outside window leaked into Recent: %q", gone)
		}
	}
	// Header documents the chosen window so operators don't have to guess.
	if !strings.Contains(body, "last 6h") {
		t.Errorf("Recent header should document the activity window, body=%s", body)
	}
}

// TestHandleAgents_DensityToggle_RendersLink verifies the density-toggle
// affordance is present and links to the opposite mode.
// TestHandleAgents_InfoDensity_RendersSummaryAndEnrichedRow verifies the
// ka-0000000571 info-density additions: an overall summary header on the
// unfiltered view, plus per-row spawned_by, uptime tooltip, coopmux terminal
// link, Slack thread link, and parsed wrap-up summary for terminated agents.
func TestHandleAgents_InfoDensity_RendersSummaryAndEnrichedRow(t *testing.T) {
	now := time.Now()
	f := newFake()
	f.listResp = &beadsapi.ListBeadsResult{
		Beads: []*beadsapi.BeadDetail{
			{
				ID:        "gd-ag-live",
				Type:      "agent",
				Status:    "in_progress",
				Labels:    []string{"project:gardenio"},
				Notes:     "pod_name: gardenio-crew-abc-xyz123\n",
				CreatedAt: now.Add(-2 * time.Hour),
				UpdatedAt: now.Add(-3 * time.Minute),
				Fields: map[string]string{
					"agent":                "crew-live",
					"role":                 "crew",
					"agent_state":          "working",
					"project":              "gardenio",
					"spawned_by":           "sched-gardenio-bugfix-abc",
					"task_id":              "gd-task-xyz",
					"task_title":           "Add info density",
					"slack_thread_channel": "C123",
					"slack_thread_ts":      "1700000000.000001",
				},
			},
		},
		Total: 1,
	}
	f.agentRecentResp = &beadsapi.ListBeadsResult{
		Beads: []*beadsapi.BeadDetail{
			{
				ID:        "gd-ag-done",
				Type:      "agent",
				Status:    "closed",
				Labels:    []string{"project:gardenio"},
				CreatedAt: now.Add(-3 * time.Hour),
				UpdatedAt: now.Add(-30 * time.Minute),
				Fields: map[string]string{
					"agent":   "crew-done",
					"role":    "crew",
					"project": "gardenio",
					"wrapup":  `{"accomplishments":"Closed gd-foo","blockers":"CI flake"}`,
				},
			},
		},
		Total: 1,
	}

	srv := NewServer(f, slog.Default(), "", "", "", nil)
	srv.coopmuxPublicURL = "https://mux.example/dash"
	srv.slackWorkspaceDomain = "acme"
	mux := http.NewServeMux()
	srv.RegisterRoutes(mux)

	req := httptest.NewRequest("GET", "/agents", nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	body := w.Body.String()

	for _, want := range []string{
		// Overall summary section.
		`class="agents-summary"`,
		`<dt>Alive</dt><dd><strong class="ok">1</strong>`,
		`<dt>Recent</dt>`,
		// Active-row info-density bits.
		"sched-gardenio-bugfix-abc",
		`title="spawned_by"`,
		`up `, // uptime chip prefix in list view
		`https://mux.example/dash#gardenio-crew-abc-xyz123`,
		`https://acme.slack.com/archives/C123/p1700000000000001`,
		// Terminated row: wrap-up summary replaces raw description.
		"Closed gd-foo",
		"blocked: CI flake",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("info-density agents page missing %q", want)
		}
	}
}

// TestHandleAgents_Summary_RendersRoleAndProjectChips verifies the ka-0000004132
// info-density additions: the fleet summary header surfaces the role and
// project distribution as small chip badges so operators can read the active
// fleet mix without paging through the list. Counts are sorted by count desc
// then label asc — deterministic across reloads.
func TestHandleAgents_Summary_RendersRoleAndProjectChips(t *testing.T) {
	now := time.Now()
	f := newFake()
	f.listResp = &beadsapi.ListBeadsResult{
		Beads: []*beadsapi.BeadDetail{
			{ID: "gd-a1", Type: "agent", Status: "in_progress", Labels: []string{"project:gardenio"}, UpdatedAt: now, Fields: map[string]string{"agent": "a1", "role": "crew", "agent_state": "working", "project": "gardenio"}},
			{ID: "gd-a2", Type: "agent", Status: "in_progress", Labels: []string{"project:gardenio"}, UpdatedAt: now, Fields: map[string]string{"agent": "a2", "role": "crew", "agent_state": "working", "project": "gardenio"}},
			{ID: "gd-a3", Type: "agent", Status: "in_progress", Labels: []string{"project:kbeads"}, UpdatedAt: now, Fields: map[string]string{"agent": "a3", "role": "gardener", "agent_state": "working", "project": "kbeads"}},
		},
		Total: 3,
	}
	mux := mkServer(t, f)

	req := httptest.NewRequest("GET", "/agents", nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	body := w.Body.String()

	for _, want := range []string{
		`class="chip-strip agents-summary-roles"`,
		`class="chip-strip agents-summary-projects"`,
		// crew=2 (sorted before gardener=1).
		`crew <span class="chip-count">2</span>`,
		`gardener <span class="chip-count">1</span>`,
		// gardenio=2 (sorted before kbeads=1).
		`gardenio <span class="chip-count">2</span>`,
		`kbeads <span class="chip-count">1</span>`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("summary breakdown missing %q\nbody=%s", want, body)
		}
	}

	// Ordering: crew chip must precede gardener chip within the role strip.
	rolesStart := strings.Index(body, "agents-summary-roles")
	rolesEnd := strings.Index(body[rolesStart:], "</div>") + rolesStart
	rolesBlock := body[rolesStart:rolesEnd]
	if i, j := strings.Index(rolesBlock, "crew "), strings.Index(rolesBlock, "gardener "); i < 0 || j < 0 || i > j {
		t.Errorf("expected crew chip before gardener chip in role strip, block=%s", rolesBlock)
	}
}

// TestBuildOverallSummary_RolesAndProjects exercises the count-and-sort logic
// independent of HTTP plumbing.
func TestBuildOverallSummary_RolesAndProjects(t *testing.T) {
	beads := []*beadsapi.BeadDetail{
		{Labels: []string{"project:gardenio"}, Fields: map[string]string{"agent_state": "working", "role": "crew"}},
		{Labels: []string{"project:gardenio"}, Fields: map[string]string{"agent_state": "working", "role": "crew"}},
		{Labels: []string{"project:kbeads"}, Fields: map[string]string{"agent_state": "idle", "role": "gardener"}},
		// Role missing — should not crash, just skipped from Roles.
		{Labels: []string{"project:gardenio"}, Fields: map[string]string{"agent_state": "failed"}},
	}
	// Two parked identities in the dedicated dormant slice; a closed-but-done
	// bead there must not count.
	dormant := []*beadsapi.BeadDetail{
		{Status: "closed", Labels: []string{"project:gardenio"}, Fields: map[string]string{"agent_state": "dormant"}},
		{Status: "closed", Labels: []string{"project:kbeads"}, Fields: map[string]string{"agent_state": "dormant"}},
		{Status: "closed", Labels: []string{"project:gardenio"}, Fields: map[string]string{"agent_state": "done"}},
	}
	v := buildOverallSummary(beads, nil, dormant, nil, 0, "6h", time.Now())
	if v == nil {
		t.Fatal("expected non-nil summary")
	}
	if v.Total != 4 || v.Alive != 2 || v.Stuck != 1 || v.Idle != 1 {
		t.Errorf("counts: total=%d alive=%d stuck=%d idle=%d", v.Total, v.Alive, v.Stuck, v.Idle)
	}
	if v.Dormant != 2 {
		t.Errorf("Dormant = %d; want 2", v.Dormant)
	}
	if len(v.Roles) != 2 || v.Roles[0].Label != "crew" || v.Roles[0].Count != 2 || v.Roles[1].Label != "gardener" || v.Roles[1].Count != 1 {
		t.Errorf("roles = %+v, want [crew:2 gardener:1]", v.Roles)
	}
	if len(v.Projects) != 2 || v.Projects[0].Label != "gardenio" || v.Projects[0].Count != 3 || v.Projects[1].Label != "kbeads" || v.Projects[1].Count != 1 {
		t.Errorf("projects = %+v, want [gardenio:3 kbeads:1]", v.Projects)
	}
}

// TestBuildRecentRuns_OutcomeClassificationAndCap verifies the ka-0000003320
// recent-runs strip classifies wrapup payloads into ok/fail/unknown and caps
// the result at recentRunsLimit while preserving recent-first order.
func TestBuildRecentRuns_OutcomeClassificationAndCap(t *testing.T) {
	now := time.Now()
	mk := func(id, agent, wrapup string, ageSec int) *beadsapi.BeadDetail {
		return &beadsapi.BeadDetail{
			ID:        id,
			Type:      "agent",
			Status:    "closed",
			Labels:    []string{"project:gardenio"},
			UpdatedAt: now.Add(-time.Duration(ageSec) * time.Second),
			Fields: map[string]string{
				"agent":  agent,
				"role":   "crew",
				"wrapup": wrapup,
			},
		}
	}
	recent := []*beadsapi.BeadDetail{
		mk("gd-a1", "crew-ok", `{"accomplishments":"shipped X"}`, 60),
		mk("gd-a2", "crew-fail", `{"accomplishments":"part","blockers":"flaky tests"}`, 120),
		mk("gd-a3", "crew-noop", ``, 180),
		mk("gd-a4", "crew-bad-json", `not json`, 240),
	}
	// Pad with extras so we hit the cap.
	for i := range recentRunsLimit + 5 {
		recent = append(recent, mk("gd-pad", "pad", `{"accomplishments":"ok"}`, 300+i))
	}

	runs := buildRecentRuns(recent, now)
	if len(runs) != recentRunsLimit {
		t.Fatalf("len(runs) = %d, want %d", len(runs), recentRunsLimit)
	}
	wantHead := []struct{ id, outcome string }{
		{"gd-a1", "ok"},
		{"gd-a2", "fail"},
		{"gd-a3", "unknown"},
		{"gd-a4", "ok"}, // non-JSON wrapup → no blockers field → ok-by-presence
	}
	for i, w := range wantHead {
		if runs[i].ID != w.id || runs[i].Outcome != w.outcome {
			t.Errorf("runs[%d] = {%s,%s}, want {%s,%s}", i, runs[i].ID, runs[i].Outcome, w.id, w.outcome)
		}
	}
	// Title must include the role + ago context so hover surfaces "what was
	// that agent doing".
	if !strings.Contains(runs[1].Title, "blocked: flaky tests") {
		t.Errorf("fail title missing blocker detail: %q", runs[1].Title)
	}
	if !strings.Contains(runs[0].Title, "clean") {
		t.Errorf("ok title missing clean marker: %q", runs[0].Title)
	}
}

// TestBuildRecentRuns_ZeroInferencePlatformFailure verifies the fix for
// ka-0000000315: a closed agent whose usage fields prove it never got an
// inference turn (a request was counted but no context reached the model and no
// tool ran — the 2026-07-25 opus-5 outage shape) renders the distinct
// "platform" outcome, not the misleading "unknown"/"ok" glyph the synthesized
// wrap-up would otherwise produce.
func TestBuildRecentRuns_ZeroInferencePlatformFailure(t *testing.T) {
	now := time.Now()
	// The synthesized fallback the close path writes for a never-started agent.
	synthWrapup := `{"tldr":"Pod terminated before an explicit wrap-up was submitted","custom":{"synthesized":"true","platform_failure":"true"}}`
	zeroInf := &beadsapi.BeadDetail{
		ID:        "gd-zeroinf",
		Type:      "agent",
		Status:    "closed",
		Labels:    []string{"project:gardenio"},
		UpdatedAt: now.Add(-60 * time.Second),
		Fields: map[string]string{
			"agent":               "crew-never-ran",
			"role":                "crew",
			"wrapup":              synthWrapup,
			"total_request_count": "1",
			"total_input_tokens":  "0",
			"total_tool_calls":    "0",
		},
	}
	// A normal run for contrast — same synthesized-looking wrap-up shape but real
	// usage — must stay classified by the wrap-up, never as a platform failure.
	worked := &beadsapi.BeadDetail{
		ID:        "gd-worked",
		Type:      "agent",
		Status:    "closed",
		Labels:    []string{"project:gardenio"},
		UpdatedAt: now.Add(-120 * time.Second),
		Fields: map[string]string{
			"agent":               "crew-worked",
			"role":                "crew",
			"wrapup":              `{"accomplishments":"shipped X"}`,
			"total_request_count": "8",
			"total_input_tokens":  "4096",
			"total_tool_calls":    "12",
		},
	}
	runs := buildRecentRuns([]*beadsapi.BeadDetail{zeroInf, worked}, now)
	if len(runs) != 2 {
		t.Fatalf("len(runs) = %d, want 2", len(runs))
	}
	if runs[0].Outcome != "platform" {
		t.Errorf("zero-inference run outcome = %q, want %q", runs[0].Outcome, "platform")
	}
	if !strings.Contains(runs[0].Title, "platform failure (no inference turn)") {
		t.Errorf("zero-inference tooltip missing platform-failure detail: %q", runs[0].Title)
	}
	if runs[1].Outcome != "ok" {
		t.Errorf("worked run outcome = %q, want %q (usage present ⇒ not a platform failure)", runs[1].Outcome, "ok")
	}
}

// TestAgentZeroInferenceTurn covers the viewer's never-started classifier across
// the reported outage shape, a normal run, and absent-usage legacy beads.
func TestAgentZeroInferenceTurn(t *testing.T) {
	cases := []struct {
		name   string
		fields map[string]string
		want   bool
	}{
		{"nil fields", nil, false},
		{"empty fields (legacy bead)", map[string]string{}, false},
		{"reported outage shape", map[string]string{"total_request_count": "1", "total_input_tokens": "0", "total_tool_calls": "0"}, true},
		{"request with tokens", map[string]string{"total_request_count": "1", "total_input_tokens": "10", "total_tool_calls": "0"}, false},
		{"request with tool calls", map[string]string{"total_request_count": "1", "total_input_tokens": "0", "total_tool_calls": "1"}, false},
		{"no request counted", map[string]string{"total_input_tokens": "0", "total_tool_calls": "0"}, false},
	}
	for _, tc := range cases {
		if got := agentZeroInferenceTurn(tc.fields); got != tc.want {
			t.Errorf("%s: agentZeroInferenceTurn = %v, want %v", tc.name, got, tc.want)
		}
	}
}

// TestDecorateRecentRunsWithMRState_PicksMostRecentAndAnnotatesTitle covers
// the ka-0000006109 extension: the recent-runs strip stamps the most-recent
// produced MR's state onto each run glyph and extends the tooltip. Runs with
// no produced MR are left untouched.
func TestDecorateRecentRunsWithMRState_PicksMostRecentAndAnnotatesTitle(t *testing.T) {
	now := time.Now()
	f := newFake()
	// Two produced MRs on gd-ag-merged — the newer "merged" one should win.
	f.beads["gd-ag-merged"] = &beadsapi.BeadDetail{ID: "gd-ag-merged", Type: "agent"}
	f.beads["gd-task-A"] = &beadsapi.BeadDetail{ID: "gd-task-A", Type: "task"}
	f.beads["gd-task-B"] = &beadsapi.BeadDetail{ID: "gd-task-B", Type: "task"}
	f.beads["gd-mr-old"] = &beadsapi.BeadDetail{
		ID: "gd-mr-old", Type: "merge-request",
		UpdatedAt: now.Add(-2 * time.Hour),
		Fields:    map[string]string{"mr_state": "opened", "mr_iid": "11"},
	}
	f.beads["gd-mr-new"] = &beadsapi.BeadDetail{
		ID: "gd-mr-new", Type: "merge-request",
		UpdatedAt: now.Add(-10 * time.Minute),
		Fields:    map[string]string{"mr_state": "merged", "mr_iid": "42"},
	}
	f.deps["gd-ag-merged"] = []beadsapi.Dependency{
		{BeadID: "gd-ag-merged", DependsOnID: "gd-task-A", Type: "produces"},
		{BeadID: "gd-ag-merged", DependsOnID: "gd-task-B", Type: "produces"},
	}
	f.deps["gd-task-A"] = []beadsapi.Dependency{
		{BeadID: "gd-task-A", DependsOnID: "gd-mr-old", Type: "produces"},
	}
	f.deps["gd-task-B"] = []beadsapi.Dependency{
		{BeadID: "gd-task-B", DependsOnID: "gd-mr-new", Type: "produces"},
	}
	// gd-ag-bare has no produced MRs — must stay un-decorated.
	f.beads["gd-ag-bare"] = &beadsapi.BeadDetail{ID: "gd-ag-bare", Type: "agent"}

	srv := NewServer(f, slog.Default(), "", "", "", &fakeScreenClient{disabled: true})
	runs := []agentRecentRun{
		{ID: "gd-ag-merged", Title: "crew-merged · clean"},
		{ID: "gd-ag-bare", Title: "crew-bare · no wrapup"},
	}
	srv.decorateRecentRunsWithMRState(context.Background(), runs)

	if runs[0].MRState != "merged" {
		t.Errorf("runs[0].MRState = %q, want merged", runs[0].MRState)
	}
	if runs[0].MRIID != "42" {
		t.Errorf("runs[0].MRIID = %q, want 42", runs[0].MRIID)
	}
	if !strings.Contains(runs[0].Title, "· merged !42") {
		t.Errorf("runs[0].Title = %q, want suffix ' · merged !42'", runs[0].Title)
	}
	if runs[1].MRState != "" || runs[1].MRIID != "" {
		t.Errorf("runs[1] should stay bare, got %+v", runs[1])
	}
	if !strings.HasSuffix(runs[1].Title, "no wrapup") {
		t.Errorf("runs[1].Title was mutated: %q", runs[1].Title)
	}
}

// TestDecorateRecentRunsWithMRState_DaemonErrorDegradesSilently asserts that
// a hard MRBeadsForAgent error on one run does not corrupt other runs and
// leaves the failing run un-decorated (best-effort degradation).
func TestDecorateRecentRunsWithMRState_DaemonErrorDegradesSilently(t *testing.T) {
	f := newFake()
	f.mrsForAgentErr = errors.New("daemon down")
	srv := NewServer(f, slog.Default(), "", "", "", &fakeScreenClient{disabled: true})
	runs := []agentRecentRun{{ID: "gd-ag-x", Title: "crew · clean"}}
	srv.decorateRecentRunsWithMRState(context.Background(), runs)
	if runs[0].MRState != "" {
		t.Errorf("MRState should stay empty on daemon error, got %q", runs[0].MRState)
	}
	if runs[0].Title != "crew · clean" {
		t.Errorf("Title was mutated on daemon error: %q", runs[0].Title)
	}
}

// TestHandleAgents_RecentRunsCarryMRStateBadge wires the full path: a closed
// agent with a produced merged MR causes the recent-runs strip to emit the
// agents-run-mr-merged class + corner-badge span. Asserts the glyph remains
// the primary signal (agents-run-ok class is still present).
func TestHandleAgents_RecentRunsCarryMRStateBadge(t *testing.T) {
	now := time.Now()
	f := newFake()
	f.listResp = &beadsapi.ListBeadsResult{Beads: nil, Total: 0}
	closedAgent := &beadsapi.BeadDetail{
		ID:        "gd-ag-closed",
		Title:     "crew-shipper",
		Type:      "agent",
		Status:    "closed",
		Labels:    []string{"project:gardenio"},
		UpdatedAt: now.Add(-15 * time.Minute),
		Fields: map[string]string{
			"agent":  "crew-shipper",
			"role":   "crew",
			"wrapup": `{"accomplishments":"shipped X"}`,
		},
	}
	f.agentRecentResp = &beadsapi.ListBeadsResult{Beads: []*beadsapi.BeadDetail{closedAgent}, Total: 1}
	f.beads["gd-ag-closed"] = closedAgent
	f.beads["gd-task-ship"] = &beadsapi.BeadDetail{ID: "gd-task-ship", Type: "task"}
	f.beads["gd-mr-ship"] = &beadsapi.BeadDetail{
		ID: "gd-mr-ship", Type: "merge-request",
		UpdatedAt: now.Add(-5 * time.Minute),
		Fields:    map[string]string{"mr_state": "merged", "mr_iid": "2837"},
	}
	f.deps["gd-ag-closed"] = []beadsapi.Dependency{
		{BeadID: "gd-ag-closed", DependsOnID: "gd-task-ship", Type: "produces"},
	}
	f.deps["gd-task-ship"] = []beadsapi.Dependency{
		{BeadID: "gd-task-ship", DependsOnID: "gd-mr-ship", Type: "produces"},
	}

	mux := mkServer(t, f)
	req := httptest.NewRequest("GET", "/agents", nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	body := w.Body.String()
	for _, want := range []string{
		`agents-run-ok`,        // outcome glyph stays primary
		`agents-run-mr-merged`, // MR-state badge class
		`agents-run-mr-badge`,  // corner-badge span hook
		`merged !2837`,         // tooltip carries the state + iid
	} {
		if !strings.Contains(body, want) {
			t.Errorf("recent-runs strip missing %q\nbody: %s", want, body)
		}
	}
}

// TestBuildLast24h_CountsAndAvgLifetime exercises the 24h rollup block
// from ka-0000003534: closures + failures + average lifetime across a
// mixed-history slice.
func TestBuildLast24h_CountsAndAvgLifetime(t *testing.T) {
	now := time.Now()
	mk := func(ageMinutes, lifetimeMinutes int, blockers string) *beadsapi.BeadDetail {
		updated := now.Add(-time.Duration(ageMinutes) * time.Minute)
		return &beadsapi.BeadDetail{
			Type:      "agent",
			Status:    "closed",
			CreatedAt: updated.Add(-time.Duration(lifetimeMinutes) * time.Minute),
			UpdatedAt: updated,
			Fields: map[string]string{
				"wrapup": func() string {
					if blockers == "" {
						return `{"accomplishments":"ok"}`
					}
					return `{"accomplishments":"ok","blockers":"` + blockers + `"}`
				}(),
			},
		}
	}
	history := []*beadsapi.BeadDetail{
		mk(10, 30, ""),         // clean, 30m
		mk(60, 90, ""),         // clean, 90m
		mk(120, 60, "timeout"), // failed, 60m
		mk(1500, 60, ""),       // outside 24h → ignored
		nil,                    // skip
	}
	v := buildLast24h(history, now)
	if v == nil {
		t.Fatal("expected non-nil Last24h")
	}
	if v.Closed != 3 {
		t.Errorf("Closed = %d, want 3", v.Closed)
	}
	if v.Failed != 1 {
		t.Errorf("Failed = %d, want 1", v.Failed)
	}
	wantAvg := (30 + 90 + 60) * time.Minute / 3
	if v.AvgLifetime != wantAvg {
		t.Errorf("AvgLifetime = %s, want %s", v.AvgLifetime, wantAvg)
	}
	if v.AvgLifetimeLabel == "" {
		t.Errorf("AvgLifetimeLabel = %q, want non-empty", v.AvgLifetimeLabel)
	}
	if v.WindowLabel == "" {
		t.Errorf("WindowLabel must be set")
	}
}

// TestBuildLast24h_EmptyWhenNothingClosed verifies the block suppresses
// itself on a quiet 24h window (everything older than the cutoff).
func TestBuildLast24h_EmptyWhenNothingClosed(t *testing.T) {
	now := time.Now()
	old := &beadsapi.BeadDetail{
		Type:      "agent",
		Status:    "closed",
		CreatedAt: now.Add(-50 * time.Hour),
		UpdatedAt: now.Add(-48 * time.Hour),
	}
	if v := buildLast24h([]*beadsapi.BeadDetail{old}, now); v != nil {
		t.Errorf("expected nil, got %+v", v)
	}
	if v := buildLast24h(nil, now); v != nil {
		t.Errorf("expected nil for empty history, got %+v", v)
	}
}

// TestBuildSparkline_CoversBucketsAndScales verifies each hourly bucket
// counts the agents whose lifespan covered its start instant, oldest
// bucket first.
func TestBuildSparkline_CoversBucketsAndScales(t *testing.T) {
	now := time.Now()
	// One agent alive throughout (created 10h ago, still active).
	alive := &beadsapi.BeadDetail{
		Type:      "agent",
		Status:    "in_progress",
		CreatedAt: now.Add(-10 * time.Hour),
		UpdatedAt: now,
	}
	// One agent that ran from 5h ago to 2h ago (closed).
	mid := &beadsapi.BeadDetail{
		Type:      "agent",
		Status:    "closed",
		CreatedAt: now.Add(-5 * time.Hour),
		UpdatedAt: now.Add(-2 * time.Hour),
	}
	// One agent that ran from 9h ago to 7h ago — entirely before bucket 0
	// (6h ago), should not contribute to any bucket.
	preWindow := &beadsapi.BeadDetail{
		Type:      "agent",
		Status:    "closed",
		CreatedAt: now.Add(-9 * time.Hour),
		UpdatedAt: now.Add(-7 * time.Hour),
	}
	v := buildSparkline([]*beadsapi.BeadDetail{alive}, []*beadsapi.BeadDetail{mid, preWindow}, now)
	if v == nil {
		t.Fatal("expected non-nil sparkline")
	}
	if len(v.Buckets) != agentsSparklineBuckets {
		t.Fatalf("len(Buckets) = %d, want %d", len(v.Buckets), agentsSparklineBuckets)
	}
	// Buckets are oldest-first: bucket 0 samples 6h ago, bucket 5 samples 1h ago.
	// Expected per bucket:
	//   bucket 0 (6h ago): alive only → 1
	//   bucket 1 (5h ago): alive + mid → 2
	//   bucket 2 (4h ago): alive + mid → 2
	//   bucket 3 (3h ago): alive + mid → 2
	//   bucket 4 (2h ago): alive + mid (updated_at == 2h ago, !Before) → 2
	//   bucket 5 (1h ago): alive only (mid closed) → 1
	want := []int{1, 2, 2, 2, 2, 1}
	for i, w := range want {
		if v.Buckets[i].Count != w {
			t.Errorf("Buckets[%d].Count = %d, want %d", i, v.Buckets[i].Count, w)
		}
	}
	if v.Max != 2 {
		t.Errorf("Max = %d, want 2", v.Max)
	}
}

// TestBuildSparkline_EmptyInputs returns nil so the template suppresses
// the strip on a fully quiet page.
func TestBuildSparkline_EmptyInputs(t *testing.T) {
	if v := buildSparkline(nil, nil, time.Now()); v != nil {
		t.Errorf("expected nil, got %+v", v)
	}
}

// TestHandleAgents_Summary_RendersLast24hAndSparkline wires the 24h
// history block and the inline sparkline into the rendered page.
func TestHandleAgents_Summary_RendersLast24hAndSparkline(t *testing.T) {
	now := time.Now()
	f := newFake()
	f.listResp = &beadsapi.ListBeadsResult{Beads: []*beadsapi.BeadDetail{
		{ID: "gd-live", Type: "agent", Status: "in_progress", Labels: []string{"project:gardenio"}, CreatedAt: now.Add(-8 * time.Hour), UpdatedAt: now, Fields: map[string]string{"agent": "live", "role": "crew", "agent_state": "working"}},
	}}
	f.agentRecentResp = &beadsapi.ListBeadsResult{Beads: []*beadsapi.BeadDetail{
		{ID: "gd-r1", Type: "agent", Status: "closed", Labels: []string{"project:gardenio"}, CreatedAt: now.Add(-3 * time.Hour), UpdatedAt: now.Add(-2 * time.Hour), Fields: map[string]string{"agent": "r1", "role": "crew", "wrapup": `{"accomplishments":"done"}`}},
		{ID: "gd-r2", Type: "agent", Status: "closed", Labels: []string{"project:gardenio"}, CreatedAt: now.Add(-20 * time.Hour), UpdatedAt: now.Add(-18 * time.Hour), Fields: map[string]string{"agent": "r2", "role": "crew", "wrapup": `{"accomplishments":"part","blockers":"flake"}`}},
	}}
	mux := mkServer(t, f)

	req := httptest.NewRequest("GET", "/agents", nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	body := w.Body.String()

	for _, want := range []string{
		// Sparkline rendered next to the Alive cell.
		`class="agents-sparkline"`,
		`agents-sparkline-bar`,
		// 24h block headings.
		`agents-summary-last24h`,
		`Closed 24h`,
		`Failed 24h`,
		`Avg lifetime`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("agents summary missing %q\nbody=%s", want, body)
		}
	}
}

// TestHandleAgents_Summary_RendersRecentRunsStrip verifies the ka-0000003320
// recent-runs glyph strip is wired into the /agents Summary tile when the
// recent list is non-empty.
func TestHandleAgents_Summary_RendersRecentRunsStrip(t *testing.T) {
	now := time.Now()
	f := newFake()
	f.listResp = &beadsapi.ListBeadsResult{
		Beads: []*beadsapi.BeadDetail{
			{
				ID:     "gd-live",
				Type:   "agent",
				Status: "in_progress",
				Labels: []string{"project:gardenio"},
				Fields: map[string]string{"agent": "live", "role": "crew", "agent_state": "working"},
			},
		},
	}
	f.agentRecentResp = &beadsapi.ListBeadsResult{
		Beads: []*beadsapi.BeadDetail{
			{
				ID:        "gd-ok",
				Type:      "agent",
				Status:    "closed",
				Labels:    []string{"project:gardenio"},
				UpdatedAt: now.Add(-2 * time.Minute),
				Fields:    map[string]string{"agent": "winner", "role": "crew", "wrapup": `{"accomplishments":"shipped"}`},
			},
			{
				ID:        "gd-fail",
				Type:      "agent",
				Status:    "closed",
				Labels:    []string{"project:gardenio"},
				UpdatedAt: now.Add(-5 * time.Minute),
				Fields:    map[string]string{"agent": "loser", "role": "crew", "wrapup": `{"blockers":"died"}`},
			},
		},
	}
	mux := mkServer(t, f)
	req := httptest.NewRequest("GET", "/agents", nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("status %d", w.Code)
	}
	body := w.Body.String()
	for _, want := range []string{
		`class="agents-recent-runs"`,
		`class="agents-run-glyph agents-run-ok"`,
		`class="agents-run-glyph agents-run-fail"`,
		`href="/gd-ok"`,
		`href="/gd-fail"`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("recent-runs strip missing %q", want)
		}
	}
}

// TestHandleAgents_Compact_RendersProjectColumn_WhenUnfiltered verifies the
// ka-0000007486 info-density addition: the compact table on the unfiltered
// /agents view carries a Project column so an operator can see each agent's
// project per row instead of inferring it from the surrounding group header.
func TestHandleAgents_Compact_RendersProjectColumn_WhenUnfiltered(t *testing.T) {
	now := time.Now()
	f := newFake()
	f.listResp = &beadsapi.ListBeadsResult{
		Beads: []*beadsapi.BeadDetail{
			{
				ID:        "gd-a1",
				Type:      "agent",
				Status:    "in_progress",
				Labels:    []string{"project:gardenio"},
				UpdatedAt: now.Add(-3 * time.Minute),
				Fields:    map[string]string{"agent": "a1", "role": "crew", "agent_state": "working", "project": "gardenio"},
			},
			{
				ID:        "gd-a2",
				Type:      "agent",
				Status:    "in_progress",
				Labels:    []string{"project:kbeads"},
				UpdatedAt: now.Add(-2 * time.Minute),
				Fields:    map[string]string{"agent": "a2", "role": "gardener", "agent_state": "working", "project": "kbeads"},
			},
		},
		Total: 2,
	}
	mux := mkServer(t, f)

	req := httptest.NewRequest("GET", "/agents?density=compact", nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("status %d", w.Code)
	}
	body := w.Body.String()
	if !strings.Contains(body, "<th>Project</th>") {
		t.Errorf("expected Project column header in unfiltered compact view")
	}
	// Each row should link the project chip back to a filtered view.
	if !strings.Contains(body, `href="/agents?project=gardenio"`) {
		t.Errorf("expected gardenio project link in row cell")
	}
	if !strings.Contains(body, `href="/agents?project=kbeads"`) {
		t.Errorf("expected kbeads project link in row cell")
	}
}

// TestHandleAgents_Compact_OmitsProjectColumn_WhenFiltered verifies that when
// the page is already scoped to one project the redundant per-row Project cell
// is dropped — keeps the filtered view as tight as the prior behaviour.
func TestHandleAgents_Compact_OmitsProjectColumn_WhenFiltered(t *testing.T) {
	now := time.Now()
	f := newFake()
	f.listResp = &beadsapi.ListBeadsResult{
		Beads: []*beadsapi.BeadDetail{
			{
				ID:        "gd-a1",
				Type:      "agent",
				Status:    "in_progress",
				Labels:    []string{"project:gardenio"},
				UpdatedAt: now.Add(-3 * time.Minute),
				Fields:    map[string]string{"agent": "a1", "role": "crew", "agent_state": "working", "project": "gardenio"},
			},
		},
		Total: 1,
	}
	mux := mkServer(t, f)

	req := httptest.NewRequest("GET", "/agents?density=compact&project=gardenio", nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("status %d", w.Code)
	}
	body := w.Body.String()
	if strings.Contains(body, "<th>Project</th>") {
		t.Errorf("Project column should be omitted on project-filtered view")
	}
}

// TestHandleAgents_StaleRow_HighlightedAndCountedInSummary verifies the
// ka-0000007486 stale-bucket signal: an agent whose state is "alive" but
// whose last activity is older than agentStaleThreshold gets a row class +
// inline chip, AND contributes to the Summary's Stale stat.
func TestHandleAgents_StaleRow_HighlightedAndCountedInSummary(t *testing.T) {
	now := time.Now()
	f := newFake()
	f.listResp = &beadsapi.ListBeadsResult{
		Beads: []*beadsapi.BeadDetail{
			{
				ID:        "gd-fresh",
				Type:      "agent",
				Status:    "in_progress",
				Labels:    []string{"project:gardenio"},
				UpdatedAt: now.Add(-2 * time.Minute),
				Fields:    map[string]string{"agent": "fresh", "role": "crew", "agent_state": "working", "project": "gardenio"},
			},
			{
				ID:        "gd-stale",
				Type:      "agent",
				Status:    "in_progress",
				Labels:    []string{"project:gardenio"},
				UpdatedAt: now.Add(-90 * time.Minute),
				Fields:    map[string]string{"agent": "stale", "role": "crew", "agent_state": "working", "project": "gardenio"},
			},
		},
		Total: 2,
	}
	mux := mkServer(t, f)

	req := httptest.NewRequest("GET", "/agents?density=compact", nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("status %d", w.Code)
	}
	body := w.Body.String()

	for _, want := range []string{
		`class="agent-row-stale"`,
		`class="chip chip-type agent-state-stale"`,
		`<dt title="Alive agents whose last bead activity is older than 1h — likely hung or crashed.">Stale</dt>`,
		`<dd><strong class="warn">1</strong></dd>`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("stale signal missing %q", want)
		}
	}
	// The fresh agent's row should NOT carry the stale class.
	freshRowIdx := strings.Index(body, `data-bead-id="gd-fresh"`)
	staleRowIdx := strings.Index(body, `data-bead-id="gd-stale"`)
	if freshRowIdx < 0 || staleRowIdx < 0 {
		t.Fatalf("expected both rows rendered (fresh=%d stale=%d)", freshRowIdx, staleRowIdx)
	}
	// Extract each row's opening tag and verify only the stale row has the class.
	freshTag := body[freshRowIdx : freshRowIdx+200]
	staleTag := body[staleRowIdx : staleRowIdx+200]
	if strings.Contains(freshTag, "agent-row-stale") {
		t.Errorf("fresh row should not be marked stale: %q", freshTag)
	}
	if !strings.Contains(staleTag, "agent-row-stale") {
		t.Errorf("stale row should be marked stale: %q", staleTag)
	}
}

// TestIsAgentStale_Buckets exercises the stale classifier independent of
// HTTP plumbing.
func TestIsAgentStale_Buckets(t *testing.T) {
	now := time.Now()
	mk := func(state string, ago time.Duration) *beadsapi.BeadDetail {
		return &beadsapi.BeadDetail{
			UpdatedAt: now.Add(-ago),
			Fields:    map[string]string{"agent_state": state},
		}
	}
	cases := []struct {
		name string
		bead *beadsapi.BeadDetail
		want bool
	}{
		{"fresh alive", mk("working", 5*time.Minute), false},
		{"stale alive", mk("working", 2*time.Hour), true},
		{"spawning alive but stale", mk("spawning", 90*time.Minute), true},
		{"stuck bucket never stale", mk("failed", 2*time.Hour), false},
		{"idle never stale", mk("idle", 24*time.Hour), false},
		{"empty state never stale", mk("", 2*time.Hour), false},
		{"zero updated_at not stale", &beadsapi.BeadDetail{Fields: map[string]string{"agent_state": "working"}}, false},
		{"nil bead", nil, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := isAgentStale(tc.bead, now); got != tc.want {
				t.Errorf("isAgentStale = %v, want %v", got, tc.want)
			}
		})
	}
}

// TestParseWrapUpSummary_Variants exercises the wrapup parser corner cases.
func TestParseWrapUpSummary_Variants(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want string
	}{
		{"empty", "", ""},
		{"only accomplishments", `{"accomplishments":"shipped X"}`, "shipped X"},
		{"only blockers", `{"blockers":"needs review"}`, "blocked: needs review"},
		{"both", `{"accomplishments":"shipped X","blockers":"needs review"}`, "shipped X · blocked: needs review"},
		{"non-json falls back to snippet", "not json blob", "not json blob"},
		// tldr is the featured headline when present (ka-0000001187), winning
		// over accomplishments/blockers.
		{"tldr wins over accomplishments", `{"tldr":"merged MR 42","accomplishments":"shipped X","blockers":"needs review"}`, "merged MR 42"},
		{"only tldr", `{"tldr":"merged MR 42"}`, "merged MR 42"},
		{"blank tldr falls through to accomplishments", `{"tldr":"  ","accomplishments":"shipped X"}`, "shipped X"},
		// Legacy pre-escaped tldrs (ka-0000005806): beads written before the
		// write-time-escape removal stored "&lt;2h"/"P1-&gt;P0". parseWrapUpSummary
		// must return the RAW value so the single render-time html/template escape
		// produces "&lt;2h" in the byte stream (i.e. the browser shows "<2h"),
		// never the double-escaped "&amp;lt;2h".
		{"legacy escaped comparison unescaped", `{"tldr":"parked on human merge with fresh &lt;2h P0"}`, "parked on human merge with fresh <2h P0"},
		{"legacy escaped arrow unescaped", `{"tldr":"esc P1-&gt;P0, decisions P2-&gt;P1"}`, "esc P1->P0, decisions P2->P1"},
		// A fresh raw tldr with real angle brackets is passed through verbatim.
		{"raw angle brackets passthrough", `{"tldr":"fixed <2h window"}`, "fixed <2h window"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := parseWrapUpSummary(tc.in)
			if got != tc.want {
				t.Errorf("parseWrapUpSummary(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}

// TestUnescapeLegacyWrapUpTLDR covers the defensive reversal of the legacy
// pre-escaped angle brackets (ka-0000005806). New beads store raw text (no-op);
// legacy beads carry "&lt;"/"&gt;" which must be restored so the template's
// single render-time escape is correct.
func TestUnescapeLegacyWrapUpTLDR(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want string
	}{
		{"empty", "", ""},
		{"no entities is no-op", "merged MR 42", "merged MR 42"},
		{"raw angle brackets left untouched", "fresh <2h P0", "fresh <2h P0"},
		{"legacy comparison reversed", "fresh &lt;2h P0", "fresh <2h P0"},
		{"legacy arrow reversed", "P1-&gt;P0", "P1->P0"},
		{"both entities reversed", "&lt;tag&gt;", "<tag>"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := unescapeLegacyWrapUpTLDR(tc.in); got != tc.want {
				t.Errorf("unescapeLegacyWrapUpTLDR(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}

// TestWrapUpTLDRRendersSingleEscaped is the end-to-end golden: a wrap-up tldr
// containing '<2h' and 'a->b' must appear in the rendered HTML byte stream as
// single-escaped entities ('&lt;2h' / 'a-&gt;b') — never double-escaped
// ('&amp;lt;' / '&amp;gt;'), which is what operators saw on the gardener page
// before ka-0000005806. Covers both a fresh raw tldr and a legacy pre-escaped
// one; both must land on the same rendered bytes.
func TestWrapUpTLDRRendersSingleEscaped(t *testing.T) {
	tmpl := template.Must(template.New("t").Parse(`<span>{{.}}</span>`))
	cases := []struct {
		name    string
		wrapup  string
		wantSub []string
	}{
		{
			name:    "fresh raw tldr",
			wrapup:  `{"tldr":"parked with fresh <2h P0; esc P1->P0"}`,
			wantSub: []string{"&lt;2h", "P1-&gt;P0"},
		},
		{
			name:    "legacy pre-escaped tldr",
			wrapup:  `{"tldr":"parked with fresh &lt;2h P0; esc P1-&gt;P0"}`,
			wantSub: []string{"&lt;2h", "P1-&gt;P0"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			summary := parseWrapUpSummary(tc.wrapup)
			var buf bytes.Buffer
			if err := tmpl.Execute(&buf, summary); err != nil {
				t.Fatalf("template execute: %v", err)
			}
			out := buf.String()
			for _, sub := range tc.wantSub {
				if !strings.Contains(out, sub) {
					t.Errorf("rendered %q missing single-escaped %q", out, sub)
				}
			}
			if strings.Contains(out, "&amp;lt;") || strings.Contains(out, "&amp;gt;") {
				t.Errorf("rendered %q is double-escaped (&amp;lt;/&amp;gt;)", out)
			}
		})
	}
}

// TestWrapUpBeadsClosedCount covers the beads_closed counter that drives the
// "N closed" outcome chip (ka-0000001187).
func TestWrapUpBeadsClosedCount(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want int
	}{
		{"empty", "", 0},
		{"missing field", `{"accomplishments":"x"}`, 0},
		{"non-json", "garbage", 0},
		{"one", `{"beads_closed":["gd-A"]}`, 1},
		{"several", `{"beads_closed":["gd-A","gd-B","gd-C"]}`, 3},
		{"blanks ignored", `{"beads_closed":["gd-A","  ",""]}`, 1},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := wrapUpBeadsClosedCount(tc.in); got != tc.want {
				t.Errorf("wrapUpBeadsClosedCount(%q) = %d, want %d", tc.in, got, tc.want)
			}
		})
	}
}

// TestDerivedRunOutcome covers the structured-signal fallback that backstops an
// empty-wrapup card so it never renders a bare title (ka-0000001187).
func TestDerivedRunOutcome(t *testing.T) {
	cases := []struct {
		name        string
		mrCount     int
		beadsClosed int
		outcome     string
		want        string
	}{
		{"nothing derivable, unknown", 0, 0, "unknown", ""},
		{"clean exit no work", 0, 0, "ok", "finished — no wrap-up"},
		{"blocked exit no work", 0, 0, "fail", "ended blocked — no wrap-up"},
		{"one mr", 1, 0, "ok", "1 MR opened"},
		{"two mrs", 2, 0, "ok", "2 MRs opened"},
		{"one bead", 0, 1, "ok", "1 bead closed"},
		{"beads plural", 0, 4, "ok", "4 beads closed"},
		{"mrs and beads", 2, 3, "ok", "2 MRs opened · 3 beads closed"},
		{"work signals win over status phrase", 1, 0, "fail", "1 MR opened"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := derivedRunOutcome(tc.mrCount, tc.beadsClosed, tc.outcome)
			if got != tc.want {
				t.Errorf("derivedRunOutcome(%d,%d,%q) = %q, want %q", tc.mrCount, tc.beadsClosed, tc.outcome, got, tc.want)
			}
		})
	}
}

func TestHandleAgents_DensityToggle_RendersLink(t *testing.T) {
	f := newFake()
	f.listResp = &beadsapi.ListBeadsResult{Beads: nil, Total: 0}
	mux := mkServer(t, f)

	req := httptest.NewRequest("GET", "/agents", nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)
	body := w.Body.String()
	if !strings.Contains(body, `agents-density-toggle`) {
		t.Errorf("expected density toggle, body=%s", body)
	}
	// Default density is "compact" (ka-0000004132) so the toggle highlights
	// compact and links out to the list view.
	if !strings.Contains(body, `<span class="chip chip-active" aria-current="true">compact</span>`) {
		t.Errorf("expected compact mode active, body=%s", body)
	}
	if !strings.Contains(body, `href="/agents?density=list"`) {
		t.Errorf("expected list-mode link, body=%s", body)
	}
}

func TestHandleAgents_DaemonError_RendersBanner(t *testing.T) {
	f := newFake()
	f.listErr = &beadsapi.APIError{StatusCode: http.StatusInternalServerError, Message: "boom"}
	mux := mkServer(t, f)

	req := httptest.NewRequest("GET", "/agents", nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	if !strings.Contains(w.Body.String(), "Failed to load agents") {
		t.Errorf("expected error banner, body=%s", w.Body.String())
	}
}

func TestHandleAgents_NavLink(t *testing.T) {
	// The Agents nav link must appear on every page (added in layout.html).
	mux := mkServer(t, newFake())
	req := httptest.NewRequest("GET", "/", nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)
	body := w.Body.String()
	if !strings.Contains(body, `href="/agents"`) {
		t.Errorf("expected /agents nav link in layout, body=%s", body)
	}
}

// TestFormulasNavLink_CoreAlwaysOn guards that the Formulas nav link is a CORE
// (always-on) link, visible in the DEFAULT simple mode (VibeMode=false) — not
// gated behind {{if .VibeMode}}. Regression guard for ka-0000005902: the whole
// formula-legibility feature (plan ka-0000007376) shipped but its front door
// was invisible to operators in simple mode because the link sat inside the
// vibe-only block. The default GET / request has no viewer, so VibeMode is
// false — the link must still render.
func TestFormulasNavLink_CoreAlwaysOn(t *testing.T) {
	mux := mkServer(t, newFake())
	req := httptest.NewRequest("GET", "/", nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)
	body := w.Body.String()
	if !strings.Contains(body, `href="/formulas"`) {
		t.Errorf("expected /formulas nav link to be a core (always-on) link in simple mode, body=%s", body)
	}
}

// TestHandleAgentScreen_Success verifies that GET /agents/{id}/screen
// returns a fragment containing the cached PTY screen text from coopmux.
func TestHandleAgentScreen_Success(t *testing.T) {
	f := newFake()
	f.beads["gd-agt"] = &beadsapi.BeadDetail{
		ID:     "gd-agt",
		Title:  "crew-1",
		Type:   "agent",
		Status: "open",
		Fields: map[string]string{
			"agent":           "crew-1",
			"last_session_id": "sess-abc",
		},
	}
	cm := &fakeScreenClient{
		screen: &coopmuxapi.Screen{Lines: []string{"hello", "world"}},
	}
	mux := mkAgentServer(t, f, cm)

	req := httptest.NewRequest("GET", "/agents/gd-agt/screen", nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d, body=%s", w.Code, w.Body.String())
	}
	body := w.Body.String()
	if !strings.Contains(body, `id="term-fallback"`) {
		t.Errorf("missing term-fallback container, body=%s", body)
	}
	if !strings.Contains(body, "hello\nworld") {
		t.Errorf("missing screen text, body=%s", body)
	}
	if cm.lastID != "sess-abc" {
		t.Errorf("expected GetScreen called with sess-abc, got %q", cm.lastID)
	}
}

// TestHandleAgentScreen_NoSession verifies that an agent with no
// last_session_id renders an empty <pre> rather than failing.
func TestHandleAgentScreen_NoSession(t *testing.T) {
	f := newFake()
	f.beads["gd-agt"] = &beadsapi.BeadDetail{
		ID:     "gd-agt",
		Title:  "crew-1",
		Type:   "agent",
		Status: "open",
		Fields: map[string]string{"agent": "crew-1"},
	}
	cm := &fakeScreenClient{}
	mux := mkAgentServer(t, f, cm)

	req := httptest.NewRequest("GET", "/agents/gd-agt/screen", nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	if cm.lastID != "" {
		t.Errorf("expected GetScreen NOT called when last_session_id empty, got %q", cm.lastID)
	}
	if !strings.Contains(w.Body.String(), `id="term-fallback"`) {
		t.Errorf("missing term-fallback, body=%s", w.Body.String())
	}
}

// TestHandleAgentScreen_CoopmuxDisabled verifies graceful degradation when
// the coopmux client is in disabled mode.
func TestHandleAgentScreen_CoopmuxDisabled(t *testing.T) {
	f := newFake()
	f.beads["gd-agt"] = &beadsapi.BeadDetail{
		ID:     "gd-agt",
		Title:  "crew-1",
		Type:   "agent",
		Status: "open",
		Fields: map[string]string{
			"agent":           "crew-1",
			"last_session_id": "sess-abc",
		},
	}
	cm := &fakeScreenClient{disabled: true}
	mux := mkAgentServer(t, f, cm)

	req := httptest.NewRequest("GET", "/agents/gd-agt/screen", nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	if cm.lastID != "" {
		t.Errorf("expected GetScreen NOT called when disabled, got %q", cm.lastID)
	}
}

// TestHandleAgentScreen_NonAgent rejects non-agent beads.
func TestHandleAgentScreen_NonAgent(t *testing.T) {
	f := newFake()
	f.beads["gd-task"] = &beadsapi.BeadDetail{
		ID:     "gd-task",
		Title:  "a task",
		Type:   "task",
		Status: "open",
	}
	mux := mkAgentServer(t, f, &fakeScreenClient{})

	req := httptest.NewRequest("GET", "/agents/gd-task/screen", nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 for non-agent, got %d", w.Code)
	}
}

// TestBeadShow_AgentTerminal verifies the agent-detail page wires the
// terminal-pane scaffolding (fallback <pre>, hidden #term, status div) and
// drops the cached screen text into #term-fallback for first paint.
func TestBeadShow_AgentTerminal(t *testing.T) {
	f := newFake()
	f.beads["gd-agt"] = &beadsapi.BeadDetail{
		ID:     "gd-agt",
		Title:  "crew-1",
		Type:   "agent",
		Status: "open",
		Fields: map[string]string{
			"agent":           "crew-1",
			"role":            "crew",
			"agent_state":     "running",
			"pod_phase":       "Running",
			"task_id":         "gd-task-9",
			"spawned_by":      "gardener-1",
			"last_session_id": "sess-abc",
		},
	}
	cm := &fakeScreenClient{
		screen: &coopmuxapi.Screen{Lines: []string{"first paint"}},
	}
	mux := mkAgentServer(t, f, cm)

	req := httptest.NewRequest("GET", "/gd-agt", nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	body := w.Body.String()
	for _, want := range []string{
		`id="agent-terminal"`,
		`id="term-fallback"`,
		`id="term"`,
		`id="term-status"`,
		`id="agent-events"`,
		`id="agent-decisions"`,
		`first paint`,
		`data-session-id="sess-abc"`,
		`xterm.bundle.js`,
		`agent_terminal.js`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("agent-detail page missing %q\nbody snippet: %s", want, snippetN(body, 600))
		}
	}
}

func TestCoopmuxTerminalURL(t *testing.T) {
	tests := []struct {
		name      string
		publicURL string
		notes     string
		want      string
	}{
		{
			name:      "happy path",
			publicURL: "https://gardenio.example/mux",
			notes:     "coop_url: http://10.0.0.1:8080\npod_name: agent-hq-0",
			want:      "https://gardenio.example/mux#agent-hq-0",
		},
		{
			name:      "no public url",
			publicURL: "",
			notes:     "pod_name: agent-hq-0",
			want:      "",
		},
		{
			name:      "no pod name",
			publicURL: "https://gardenio.example/mux",
			notes:     "coop_url: http://10.0.0.1:8080",
			want:      "",
		},
		{
			name:      "blank pod name",
			publicURL: "https://gardenio.example/mux",
			notes:     "pod_name:   \ncoop_url: http://10.0.0.1:8080",
			want:      "",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			s := &Server{coopmuxPublicURL: tc.publicURL}
			bead := &beadsapi.BeadDetail{Notes: tc.notes}
			if got := s.coopmuxTerminalURL(bead); got != tc.want {
				t.Errorf("coopmuxTerminalURL = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestCoopmuxTerminalURL_NilBead(t *testing.T) {
	s := &Server{coopmuxPublicURL: "https://gardenio.example/mux"}
	if got := s.coopmuxTerminalURL(nil); got != "" {
		t.Errorf("coopmuxTerminalURL(nil) = %q, want empty", got)
	}
}

func TestSlackThreadURL(t *testing.T) {
	tests := []struct {
		name      string
		workspace string
		fields    map[string]string
		want      string
	}{
		{
			name:      "happy path",
			workspace: "acme",
			fields: map[string]string{
				"slack_thread_channel": "C0AHE7C0E1G",
				"slack_thread_ts":      "1777831245.785019",
			},
			want: "https://acme.slack.com/archives/C0AHE7C0E1G/p1777831245785019?thread_ts=1777831245.785019&cid=C0AHE7C0E1G",
		},
		{
			name:      "no workspace domain",
			workspace: "",
			fields: map[string]string{
				"slack_thread_channel": "C0AHE7C0E1G",
				"slack_thread_ts":      "1777831245.785019",
			},
			want: "",
		},
		{
			name:      "no channel",
			workspace: "acme",
			fields: map[string]string{
				"slack_thread_ts": "1777831245.785019",
			},
			want: "",
		},
		{
			name:      "no ts",
			workspace: "acme",
			fields: map[string]string{
				"slack_thread_channel": "C0AHE7C0E1G",
			},
			want: "",
		},
		{
			name:      "blank fields",
			workspace: "acme",
			fields: map[string]string{
				"slack_thread_channel": "  ",
				"slack_thread_ts":      "  ",
			},
			want: "",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			s := &Server{slackWorkspaceDomain: tc.workspace}
			bead := &beadsapi.BeadDetail{Fields: tc.fields}
			if got := s.slackThreadURL(bead); got != tc.want {
				t.Errorf("slackThreadURL = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestSlackThreadURL_NilBead(t *testing.T) {
	s := &Server{slackWorkspaceDomain: "acme"}
	if got := s.slackThreadURL(nil); got != "" {
		t.Errorf("slackThreadURL(nil) = %q, want empty", got)
	}
}

func TestBeadShow_AgentRendersSlackThreadLink(t *testing.T) {
	f := newFake()
	f.beads["gd-agt-slack"] = &beadsapi.BeadDetail{
		ID:     "gd-agt-slack",
		Title:  "thread-agent-1",
		Type:   "agent",
		Status: "open",
		Fields: map[string]string{
			"agent":                "thread-agent-1",
			"role":                 "gardener",
			"agent_state":          "running",
			"slack_thread_channel": "C0AHE7C0E1G",
			"slack_thread_ts":      "1777831245.785019",
		},
	}
	srv := NewServer(f, slog.Default(), "", "", "", nil)
	srv.slackWorkspaceDomain = "acme"
	mux := http.NewServeMux()
	srv.RegisterRoutes(mux)

	req := httptest.NewRequest("GET", "/gd-agt-slack", nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	body := w.Body.String()
	want := `href="https://acme.slack.com/archives/C0AHE7C0E1G/p1777831245785019?thread_ts=1777831245.785019&amp;cid=C0AHE7C0E1G"`
	if !strings.Contains(body, want) {
		t.Errorf("expected agent page to contain Slack thread link %q\nsnippet: %s", want, snippetN(body, 800))
	}
	if !strings.Contains(body, ">Open Slack thread<") {
		t.Errorf("expected agent page to contain 'Open Slack thread' link text\nsnippet: %s", snippetN(body, 800))
	}
}

func TestBeadShow_AgentNoSlackLinkWhenWorkspaceUnset(t *testing.T) {
	f := newFake()
	f.beads["gd-agt-noslack"] = &beadsapi.BeadDetail{
		ID:     "gd-agt-noslack",
		Title:  "thread-agent-1",
		Type:   "agent",
		Status: "open",
		Fields: map[string]string{
			"agent":                "thread-agent-1",
			"role":                 "gardener",
			"agent_state":          "running",
			"slack_thread_channel": "C0AHE7C0E1G",
			"slack_thread_ts":      "1777831245.785019",
		},
	}
	srv := NewServer(f, slog.Default(), "", "", "", nil)
	// slackWorkspaceDomain intentionally unset.
	mux := http.NewServeMux()
	srv.RegisterRoutes(mux)

	req := httptest.NewRequest("GET", "/gd-agt-noslack", nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	body := w.Body.String()
	if strings.Contains(body, "Open Slack thread") {
		t.Errorf("did not expect Slack thread link with workspace unset\nsnippet: %s", snippetN(body, 800))
	}
}

func TestBeadShow_AgentRendersCoopmuxPublicLink(t *testing.T) {
	f := newFake()
	f.beads["gd-agt-link"] = &beadsapi.BeadDetail{
		ID:     "gd-agt-link",
		Title:  "crew-1",
		Type:   "agent",
		Status: "open",
		Fields: map[string]string{
			"agent":           "crew-1",
			"role":            "crew",
			"agent_state":     "running",
			"last_session_id": "sess-abc",
		},
		Notes: "coop_url: http://10.0.0.1:8080\npod_name: agent-hq-7",
	}
	cm := &fakeScreenClient{screen: &coopmuxapi.Screen{Lines: []string{"hi"}}}
	srv := NewServer(f, slog.Default(), "", "", "", cm)
	srv.coopmuxPublicURL = "https://gardenio.example/mux"
	mux := http.NewServeMux()
	srv.RegisterRoutes(mux)

	req := httptest.NewRequest("GET", "/gd-agt-link", nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	body := w.Body.String()
	want := `href="https://gardenio.example/mux#agent-hq-7"`
	if !strings.Contains(body, want) {
		t.Errorf("expected agent page to contain %q\nsnippet: %s", want, snippetN(body, 800))
	}
	if !strings.Contains(body, "Watch live terminal") {
		t.Errorf("expected 'Watch live terminal' CTA text\nsnippet: %s", snippetN(body, 800))
	}
	if !strings.Contains(body, `class="agent-watch-terminal"`) {
		t.Errorf("expected prominent .agent-watch-terminal CTA button\nsnippet: %s", snippetN(body, 800))
	}
	// The internal pod-IP coop_url is still visible in the raw notes block —
	// that's intentional: this fix is display-only on the agent header. The
	// new public link gives the operator a clickable handle alongside the
	// raw notes.
}

func TestBeadShow_AgentHidesCoopmuxLinkWhenUnconfigured(t *testing.T) {
	f := newFake()
	f.beads["gd-agt-nolink"] = &beadsapi.BeadDetail{
		ID:    "gd-agt-nolink",
		Title: "crew-1",
		Type:  "agent",
		Fields: map[string]string{
			"agent":           "crew-1",
			"last_session_id": "sess-x",
		},
		Notes: "pod_name: agent-hq-3",
	}
	cm := &fakeScreenClient{screen: &coopmuxapi.Screen{}}
	mux := mkAgentServer(t, f, cm) // coopmuxPublicURL left empty

	req := httptest.NewRequest("GET", "/gd-agt-nolink", nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	if strings.Contains(w.Body.String(), `class="agent-watch-terminal"`) {
		t.Errorf("public-link UI rendered when COOPMUX_PUBLIC_URL is unset")
	}
}

func snippetN(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "..."
}

// TestHandleAgents_ProjectFilter_RendersSecondaryChips verifies the
// ka-0000001370 info-density additions to /agents?project=<name>: a role
// breakdown badge strip inside the fleet tile, plus role + state secondary
// filter chips below the project chip with counts that respect the
// opposite-dimension filter.
func TestHandleAgents_ProjectFilter_RendersSecondaryChips(t *testing.T) {
	now := time.Now()
	f := newFake()
	f.listResp = &beadsapi.ListBeadsResult{
		Beads: []*beadsapi.BeadDetail{
			// gardenio / crew / working (alive)
			{
				ID: "gd-a", Type: "agent", Status: "in_progress",
				Labels: []string{"project:gardenio"}, UpdatedAt: now,
				Fields: map[string]string{"agent": "a", "role": "crew", "agent_state": "working", "project": "gardenio"},
			},
			// gardenio / crew / failed (stuck)
			{
				ID: "gd-b", Type: "agent", Status: "in_progress",
				Labels: []string{"project:gardenio"}, UpdatedAt: now,
				Fields: map[string]string{"agent": "b", "role": "crew", "agent_state": "failed", "project": "gardenio"},
			},
			// gardenio / gardener / working (alive)
			{
				ID: "gd-c", Type: "agent", Status: "in_progress",
				Labels: []string{"project:gardenio"}, UpdatedAt: now,
				Fields: map[string]string{"agent": "c", "role": "gardener", "agent_state": "working", "project": "gardenio"},
			},
			// gardenio / no-role / idle (folds to "untyped")
			{
				ID: "gd-d", Type: "agent", Status: "in_progress",
				Labels: []string{"project:gardenio"}, UpdatedAt: now,
				Fields: map[string]string{"agent": "d", "agent_state": "idle", "project": "gardenio"},
			},
			// kbeads / crew — excluded from the gardenio-scoped chips.
			{
				ID: "gd-e", Type: "agent", Status: "in_progress",
				Labels: []string{"project:kbeads"}, UpdatedAt: now,
				Fields: map[string]string{"agent": "e", "role": "crew", "agent_state": "working", "project": "kbeads"},
			},
		},
		Total: 5,
	}
	mux := mkServer(t, f)

	req := httptest.NewRequest("GET", "/agents?project=gardenio", nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	body := w.Body.String()

	for _, want := range []string{
		// Fleet-tile role-breakdown badges (2 crew, 1 gardener, 1 untyped).
		// Configured roles cross-link to the role hub (/role/<name>, task
		// ka-0000007118); "untyped" has no hub and stays a plain chip.
		`class="fleet-tile-roles chip-strip"`,
		`href="/role/crew" title="Role hub for crew">crew <span class="chip-count">2`,
		`href="/role/gardener" title="Role hub for gardener">gardener <span class="chip-count">1`,
		`fleet-tile-role">untyped <span class="chip-count">1`,
		// Fleet-tile raw-state breakdown badges (2 working, 1 failed, 1 idle).
		// ka-0000005313 — surfaces within-bucket distribution beyond the
		// bucketed Alive/Stuck/Idle dl above.
		`class="fleet-tile-states chip-strip"`,
		`agent-state-working">working <span class="chip-count">2`,
		`agent-state-failed">failed <span class="chip-count">1`,
		`agent-state-idle">idle <span class="chip-count">1`,
		// Secondary filter chip strips. ka-0000005685 (audit F4): the
		// page-level filter menus are <nav> wrappers, standardized to match
		// project.html / project_hub.html so a filter strip is consistently a
		// landmark rather than a coin-flip <div>/<nav>.
		`class="agents-secondary-filters"`,
		`<nav class="chip-strip filter-strip" aria-label="Role filter">`,
		`<nav class="chip-strip filter-strip" aria-label="State filter">`,
		// Role chip links carry project + role; default density omitted.
		`href="/agents?project=gardenio&amp;role=crew"`,
		`href="/agents?project=gardenio&amp;role=gardener"`,
		`href="/agents?project=gardenio&amp;role=untyped"`,
		// State chip links carry project + state.
		`href="/agents?project=gardenio&amp;state=alive"`,
		`href="/agents?project=gardenio&amp;state=stuck"`,
		`href="/agents?project=gardenio&amp;state=idle"`,
		// Counts: alive=2, stuck=1, idle=1, crew=2.
		`alive <span class="chip-count">2`,
		`stuck <span class="chip-count">1`,
		`idle <span class="chip-count">1`,
		`crew <span class="chip-count">2`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("filtered agents page missing %q\nbody=%s", want, body)
		}
	}
}

// TestHandleAgents_SecondaryFilters_NarrowRows verifies the role + state
// query params actually filter rendered rows and that chip counts shift to
// reflect the opposite-dimension filter (role-strip counts use the active
// state filter, and vice versa).
func TestHandleAgents_SecondaryFilters_NarrowRows(t *testing.T) {
	now := time.Now()
	f := newFake()
	f.listResp = &beadsapi.ListBeadsResult{
		Beads: []*beadsapi.BeadDetail{
			{
				ID: "gd-cw", Type: "agent", Status: "in_progress",
				Labels: []string{"project:gardenio"}, UpdatedAt: now,
				Fields: map[string]string{"agent": "crew-working", "role": "crew", "agent_state": "working", "project": "gardenio"},
			},
			{
				ID: "gd-cf", Type: "agent", Status: "in_progress",
				Labels: []string{"project:gardenio"}, UpdatedAt: now,
				Fields: map[string]string{"agent": "crew-failed", "role": "crew", "agent_state": "failed", "project": "gardenio"},
			},
			{
				ID: "gd-pw", Type: "agent", Status: "in_progress",
				Labels: []string{"project:gardenio"}, UpdatedAt: now,
				Fields: map[string]string{"agent": "gardener-working", "role": "gardener", "agent_state": "working", "project": "gardenio"},
			},
		},
		Total: 3,
	}
	mux := mkServer(t, f)

	// role=crew → crew-working and crew-failed survive; gardener-working drops.
	req := httptest.NewRequest("GET", "/agents?project=gardenio&role=crew", nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)
	body := w.Body.String()

	for _, want := range []string{"crew-working", "crew-failed"} {
		if !strings.Contains(body, want) {
			t.Errorf("role=crew filter dropped %q", want)
		}
	}
	if strings.Contains(body, "gardener-working") {
		t.Errorf("role=crew filter leaked non-crew row")
	}
	// Active-chip highlight on the role chip, with the standardized
	// aria-current="true" screen-reader cue for in-page filter toggles
	// (ka-0000002684 / audit F3).
	if !strings.Contains(body, `<span class="chip chip-active" aria-current="true">crew <span class="chip-count">2`) {
		t.Errorf("expected active crew chip with aria-current=\"true\", body=%s", body)
	}
	// State chips' counts now reflect the role=crew narrowing: alive=1, stuck=1.
	if !strings.Contains(body, `alive <span class="chip-count">1`) {
		t.Errorf("state chip alive count not narrowed by role filter, body=%s", body)
	}
	if !strings.Contains(body, `stuck <span class="chip-count">1`) {
		t.Errorf("state chip stuck count not narrowed by role filter, body=%s", body)
	}

	// state=alive → only crew-working and gardener-working survive.
	req = httptest.NewRequest("GET", "/agents?project=gardenio&state=alive", nil)
	w = httptest.NewRecorder()
	mux.ServeHTTP(w, req)
	body = w.Body.String()

	for _, want := range []string{"crew-working", "gardener-working"} {
		if !strings.Contains(body, want) {
			t.Errorf("state=alive filter dropped %q", want)
		}
	}
	if strings.Contains(body, "crew-failed") {
		t.Errorf("state=alive filter leaked stuck row")
	}

	// Combined role=crew + state=stuck → only crew-failed.
	req = httptest.NewRequest("GET", "/agents?project=gardenio&role=crew&state=stuck", nil)
	w = httptest.NewRecorder()
	mux.ServeHTTP(w, req)
	body = w.Body.String()

	if !strings.Contains(body, "crew-failed") {
		t.Errorf("combined filter dropped crew-failed, body=%s", body)
	}
	for _, gone := range []string{"crew-working", "gardener-working"} {
		if strings.Contains(body, gone) {
			t.Errorf("combined filter leaked %q", gone)
		}
	}
}

// TestHandleAgents_NoProjectFilter_OmitsSecondaryChips verifies the role +
// state secondary filters are only rendered alongside a project filter —
// the unfiltered fleet view stays unchanged.
func TestHandleAgents_NoProjectFilter_OmitsSecondaryChips(t *testing.T) {
	now := time.Now()
	f := newFake()
	f.listResp = &beadsapi.ListBeadsResult{
		Beads: []*beadsapi.BeadDetail{
			{
				ID: "gd-x", Type: "agent", Status: "in_progress",
				Labels: []string{"project:gardenio"}, UpdatedAt: now,
				Fields: map[string]string{"agent": "x", "role": "crew", "agent_state": "working"},
			},
		},
		Total: 1,
	}
	mux := mkServer(t, f)
	// Bogus role/state params on an unfiltered view: must be ignored, not
	// thrash the page.
	req := httptest.NewRequest("GET", "/agents?role=crew&state=alive", nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)
	body := w.Body.String()

	if strings.Contains(body, `class="agents-secondary-filters"`) {
		t.Errorf("secondary filter strip rendered without project filter")
	}
	if strings.Contains(body, `data-filter="role"`) {
		t.Errorf("role clear-filter chip rendered without project filter")
	}
	if !strings.Contains(body, "x") {
		t.Errorf("row should not be filtered when project filter is absent")
	}
}

// TestBuildAgentsHref covers the URL builder's empty-value pruning and
// stable ordering.
func TestBuildAgentsHref(t *testing.T) {
	cases := []struct {
		name                                string
		base, project, role, state, density string
		want                                string
	}{
		{"empty", "", "", "", "", "", "/agents"},
		{"project only", "", "gardenio", "", "", "", "/agents?project=gardenio"},
		{"project + role", "", "gardenio", "crew", "", "", "/agents?project=gardenio&role=crew"},
		{"project + state", "", "gardenio", "", "alive", "", "/agents?project=gardenio&state=alive"},
		{"all four", "", "gardenio", "crew", "alive", "compact", "/agents?project=gardenio&role=crew&state=alive&density=compact"},
		{"non-compact density dropped", "", "gardenio", "", "", "list", "/agents?project=gardenio"},
		{"with base path", "/v", "gardenio", "", "", "", "/v/agents?project=gardenio"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := buildAgentsHref(tc.base, tc.project, tc.role, tc.state, tc.density)
			if got != tc.want {
				t.Errorf("buildAgentsHref(%q,%q,%q,%q,%q) = %q; want %q",
					tc.base, tc.project, tc.role, tc.state, tc.density, got, tc.want)
			}
		})
	}
}

// isUnengagedPrewarmedAgent must hide unassigned pool shells in ALL their parked
// states — prewarmed, queued (capacity-deferred), idle (started-but-unassigned)
// — but never an assigned member (ka-0000007786).
func TestIsUnengagedPrewarmedAgent(t *testing.T) {
	mk := func(state, agent, thread, task string) *beadsapi.BeadDetail {
		f := map[string]string{"agent_state": state, "agent": agent}
		if thread != "" {
			f["slack_thread_ts"] = thread
		}
		if task != "" {
			f["task_id"] = task
		}
		return &beadsapi.BeadDetail{Fields: f}
	}
	cases := []struct {
		name string
		bead *beadsapi.BeadDetail
		want bool
	}{
		{"prewarmed shell", mk("prewarmed", "prewarmed-1-2", "", ""), true},
		{"queued shell", mk("queued", "prewarmed-1-2", "", ""), true},
		{"idle shell", mk("idle", "prewarmed-1-2", "", ""), true},
		{"assigned-queued (thread) shown", mk("queued", "prewarmed-1-2", "1.1", ""), false},
		{"assigned-idle (task) shown", mk("idle", "prewarmed-1-2", "", "gd-x"), false},
		{"working member shown", mk("working", "prewarmed-1-2", "", ""), false},
		{"non-pool queued shown", mk("queued", "crew-k8s", "", ""), false},
		{"nil", nil, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := isUnengagedPrewarmedAgent(tc.bead); got != tc.want {
				t.Errorf("isUnengagedPrewarmedAgent(%s) = %v, want %v", tc.name, got, tc.want)
			}
		})
	}
}

// TestBeadShow_AgentRendersLineageStrip verifies the succession lineage strip
// (task ka-0000007852) renders on the agent card when the bead carries a
// resolvable prior_agent_id chain: the head is marked, the predecessor chip
// links to its bead, and the section label appears.
func TestBeadShow_AgentRendersLineageStrip(t *testing.T) {
	f := newFake()
	f.beads["gd-prior"] = &beadsapi.BeadDetail{
		ID: "gd-prior", Title: "crew-prev", Type: "agent", Status: "closed",
		Fields: map[string]string{"agent": "crew-prev"},
	}
	f.beads["gd-agt"] = &beadsapi.BeadDetail{
		ID: "gd-agt", Title: "crew-1", Type: "agent", Status: "in_progress",
		Fields: map[string]string{
			"agent":           "crew-1",
			"role":            "crew",
			"agent_state":     "working",
			"prior_agent_id":  "gd-prior",
			"last_session_id": "sess-1",
		},
	}
	mux := mkAgentServer(t, f, &fakeScreenClient{screen: &coopmuxapi.Screen{Lines: []string{"x"}}})

	req := httptest.NewRequest("GET", "/gd-agt", nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	body := w.Body.String()
	for _, want := range []string{
		`class="agent-lineage"`,
		`agent-lineage-chip-head`,
		`href="/gd-prior"`,
		`crew-prev`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("lineage strip missing %q\nbody snippet: %s", want, snippetN(body, 800))
		}
	}
}

// TestBeadShow_AgentNoLineageStripForFirstOfLine verifies a first-of-line
// agent (no predecessor) renders no lineage section.
func TestBeadShow_AgentNoLineageStripForFirstOfLine(t *testing.T) {
	f := newFake()
	f.beads["gd-agt"] = &beadsapi.BeadDetail{
		ID: "gd-agt", Title: "crew-1", Type: "agent", Status: "in_progress",
		Fields: map[string]string{"agent": "crew-1", "role": "crew", "agent_state": "working"},
	}
	mux := mkAgentServer(t, f, &fakeScreenClient{screen: &coopmuxapi.Screen{Lines: []string{"x"}}})

	req := httptest.NewRequest("GET", "/gd-agt", nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	if strings.Contains(w.Body.String(), `class="agent-lineage"`) {
		t.Error("first-of-line agent should not render a lineage strip")
	}
}
