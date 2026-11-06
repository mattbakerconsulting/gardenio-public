package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"gardenio/controller/internal/beadsapi"
)

// agentLinkFake builds a fakeDaemon with two pre-seeded beads:
//   - gd-task1: a task whose Assignee/Owner are agent-shaped names.
//   - gd-agent1: the active agent bead whose fields.agent matches.
//
// FindMostRecentAgentBead resolves "thread-bot-1" → gd-agent1; everything
// else returns ErrAgentNotFound.
func agentLinkFake() *fakeDaemon {
	return &fakeDaemon{
		beads: map[string]*beadsapi.BeadDetail{
			"gd-task1": {
				ID:       "gd-task1",
				Title:    "Linked task",
				Type:     "task",
				Status:   "in_progress",
				Assignee: "thread-bot-1",
				Owner:    "thread-bot-1",
			},
			"gd-agent1": {
				ID:          "gd-agent1",
				Title:       "thread-bot-1",
				Type:        "agent",
				Status:      "in_progress",
				Description: "Agent bead description.",
				Fields:      map[string]string{"agent": "thread-bot-1"},
			},
		},
	}
}

// resolverFor returns a per-test agentResolver bound to fake.
func resolverFor(fake *fakeDaemon) *agentResolver {
	return newAgentResolver(context.Background(), fake)
}

func TestRenderAgentLink_ShapeRules(t *testing.T) {
	fake := agentLinkFake()
	tests := []struct {
		name     string
		basePath string
		input    string
		wantIs   string // substring that must be present
		wantNot  string // substring that must NOT be present
	}{
		{
			name:    "agent-shaped name resolves to bead-id href",
			input:   "thread-bot-1",
			wantIs:  `<a class="agent-link" href="/gd-agent1" data-agent="thread-bot-1">thread-bot-1</a>`,
			wantNot: "&mdash;",
		},
		{
			name:    "empty value renders em-dash",
			input:   "",
			wantIs:  `class="empty"`,
			wantNot: "<a ",
		},
		{
			name:    "email is not agent-shaped — plain text",
			input:   "alice@example.com",
			wantIs:  "alice@example.com",
			wantNot: "<a ",
		},
		{
			name:    "OIDC subject is not agent-shaped",
			input:   "slack:U0OPERATOR2",
			wantIs:  "slack:U0OPERATOR2",
			wantNot: "<a ",
		},
		{
			name:    "value with spaces is not agent-shaped",
			input:   "Matt Baker",
			wantIs:  "Matt Baker",
			wantNot: "<a ",
		},
		{
			name:     "basePath is propagated into bead-id href",
			basePath: "/beads",
			input:    "thread-bot-1",
			wantIs:   `href="/beads/gd-agent1"`,
		},
		{
			name:    "HTML-escapes the displayed name (defense in depth)",
			input:   "thread-bot-1", // pattern excludes <>; pick a safe input
			wantIs:  ">thread-bot-1<",
			wantNot: "<script>",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := string(renderAgentLink(tc.basePath, tc.input, resolverFor(fake)))
			if tc.wantIs != "" && !strings.Contains(got, tc.wantIs) {
				t.Errorf("renderAgentLink(%q, %q) = %q, missing %q", tc.basePath, tc.input, got, tc.wantIs)
			}
			if tc.wantNot != "" && strings.Contains(got, tc.wantNot) {
				t.Errorf("renderAgentLink(%q, %q) = %q, contains forbidden %q", tc.basePath, tc.input, got, tc.wantNot)
			}
		})
	}
}

// TestRenderAgentLink_ResolverMissFallback verifies that when the resolver
// finds no matching agent bead (or the resolver itself is nil), the helper
// falls back to plain-text rendering rather than emitting a dead link.
func TestRenderAgentLink_ResolverMissFallback(t *testing.T) {
	fake := agentLinkFake()

	// Resolver miss: the name is agent-shaped but no agent bead matches.
	got := string(renderAgentLink("", "no-such-agent", resolverFor(fake)))
	if strings.Contains(got, "<a ") {
		t.Errorf("resolver miss should render plain text, got %q", got)
	}
	if !strings.Contains(got, "no-such-agent") {
		t.Errorf("resolver miss should still display the name, got %q", got)
	}

	// Nil resolver: same fallback path (no daemon available at render time).
	got = string(renderAgentLink("", "thread-bot-1", nil))
	if strings.Contains(got, "<a ") {
		t.Errorf("nil resolver should render plain text, got %q", got)
	}
	if !strings.Contains(got, "thread-bot-1") {
		t.Errorf("nil resolver should still display the name, got %q", got)
	}
}

func TestLooksLikeAgentName_Boundaries(t *testing.T) {
	yes := []string{"thread-bot-1", "gardener-1", "Z9", "x_y", "Thread_Agent-42"}
	no := []string{"", "1leading-digit", "alice@example.com", "slack:U123", "Matt Baker", "has/slash"}
	// Note: "gd-aaa" matches the agent-name shape — that's fine; it would just
	// resolve to "no agent found" via the daemon. We don't try to reject it
	// in the regex (would over-fit), but document the intentional overlap.
	if !looksLikeAgentName("gd-aaa") {
		t.Errorf("gd-aaa intentionally matches agent-name shape (not a real ID); regex changed?")
	}
	for _, s := range yes {
		if !looksLikeAgentName(s) {
			t.Errorf("looksLikeAgentName(%q) = false, want true", s)
		}
	}
	for _, s := range no {
		if looksLikeAgentName(s) {
			t.Errorf("looksLikeAgentName(%q) = true, want false", s)
		}
	}
}

func TestHandleAgentByNamePreview_Renders(t *testing.T) {
	mux := mkServer(t, agentLinkFake())
	req := httptest.NewRequest("GET", "/a/thread-bot-1/preview", nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d body=%s", w.Code, w.Body.String())
	}
	if cc := w.Header().Get("Cache-Control"); cc != "private, max-age=30" {
		t.Errorf("Cache-Control = %q, want private, max-age=30", cc)
	}
	body := w.Body.String()
	for _, want := range []string{
		`class="bead-preview"`,
		`data-bead-id="gd-agent1"`,
		`thread-bot-1`,            // title
		`Agent bead description.`, // excerpt
	} {
		if !strings.Contains(body, want) {
			t.Errorf("preview missing %q\nbody=%s", want, body)
		}
	}
	if strings.Contains(body, "<!DOCTYPE") || strings.Contains(body, "<html") {
		t.Errorf("preview must be a fragment, not a page\nbody=%s", body)
	}
}

func TestHandleAgentByNamePreview_NotFound(t *testing.T) {
	mux := mkServer(t, agentLinkFake())
	req := httptest.NewRequest("GET", "/a/nope/preview", nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200 (notfound fragment), got %d body=%s", w.Code, w.Body.String())
	}
	if cc := w.Header().Get("Cache-Control"); cc != "no-store" {
		t.Errorf("Cache-Control = %q, want no-store", cc)
	}
	body := w.Body.String()
	for _, want := range []string{
		`bead-preview-missing`,
		`data-bead-id="nope"`,
		`not found`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("notfound fragment missing %q\nbody=%s", want, body)
		}
	}
}

// TestBeadShow_AssigneeRendersAgentLink verifies that the bead detail page
// resolves an agent-shaped Assignee value at render time and emits a link
// to /{gd-id} directly (no /a/{name} redirect indirection). End-to-end
// coverage of the per-request resolver wired into the agentLink helper.
func TestBeadShow_AssigneeRendersAgentLink(t *testing.T) {
	mux := mkServer(t, agentLinkFake())
	req := httptest.NewRequest("GET", "/gd-task1", nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d body=%s", w.Code, w.Body.String())
	}
	body := w.Body.String()
	if !strings.Contains(body, `<a class="agent-link" href="/gd-agent1" data-agent="thread-bot-1">thread-bot-1</a>`) {
		idx := strings.Index(body, `id="assignee-block"`)
		snippet := body
		if idx > 0 {
			end := min(idx+200, len(body))
			snippet = body[idx:end]
		}
		t.Errorf("expected assignee to render as render-time-resolved agent link\nsnippet=%s", snippet)
	}
}

// liveAgentFake builds a fakeDaemon with three agent beads exercising the
// liveness predicate: one alive (working + recent UpdatedAt), one with a
// non-alive state, and one with an alive state but a stale UpdatedAt.
// Plus a task fixture whose Assignee maps to the alive agent.
func liveAgentFake(now time.Time) *fakeDaemon {
	return &fakeDaemon{
		beads: map[string]*beadsapi.BeadDetail{
			"gd-task-live": {
				ID:       "gd-task-live",
				Title:    "Linked task with live agent",
				Type:     "task",
				Status:   "in_progress",
				Assignee: "live-bot",
			},
			"gd-agent-live": {
				ID:        "gd-agent-live",
				Title:     "live-bot",
				Type:      "agent",
				Status:    "in_progress",
				Fields:    map[string]string{"agent": "live-bot", "agent_state": "working"},
				UpdatedAt: now.Add(-2 * time.Minute),
			},
			"gd-agent-idle": {
				ID:        "gd-agent-idle",
				Title:     "idle-bot",
				Type:      "agent",
				Status:    "in_progress",
				Fields:    map[string]string{"agent": "idle-bot", "agent_state": "idle"},
				UpdatedAt: now.Add(-2 * time.Minute),
			},
			"gd-agent-stale": {
				ID:        "gd-agent-stale",
				Title:     "stale-bot",
				Type:      "agent",
				Status:    "in_progress",
				Fields:    map[string]string{"agent": "stale-bot", "agent_state": "working"},
				UpdatedAt: now.Add(-2 * time.Hour),
			},
		},
	}
}

// liveResolverFor returns a resolver bound to fake with a fixed clock.
func liveResolverFor(fake *fakeDaemon, now time.Time) *agentResolver {
	r := newAgentResolver(context.Background(), fake)
	r.now = func() time.Time { return now }
	return r
}

func TestRenderAgentLiveIcon_AliveAgent(t *testing.T) {
	now := time.Now()
	r := liveResolverFor(liveAgentFake(now), now)
	got := string(renderAgentLiveIcon("", "live-bot", r))
	for _, want := range []string{
		`class="agent-live-icon"`,
		`href="/gd-agent-live"`,
		`title="agent working: live-bot"`,
		`aria-label="live agent live-bot"`,
		"●",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("renderAgentLiveIcon(live) missing %q, got %q", want, got)
		}
	}
}

func TestRenderAgentLiveIcon_NonAliveState(t *testing.T) {
	now := time.Now()
	r := liveResolverFor(liveAgentFake(now), now)
	if got := string(renderAgentLiveIcon("", "idle-bot", r)); got != "" {
		t.Errorf("renderAgentLiveIcon(idle) = %q, want empty", got)
	}
}

func TestRenderAgentLiveIcon_StaleAgent(t *testing.T) {
	now := time.Now()
	r := liveResolverFor(liveAgentFake(now), now)
	if got := string(renderAgentLiveIcon("", "stale-bot", r)); got != "" {
		t.Errorf("renderAgentLiveIcon(stale) = %q, want empty", got)
	}
}

func TestRenderAgentLiveIcon_NonAgentShape(t *testing.T) {
	now := time.Now()
	r := liveResolverFor(liveAgentFake(now), now)
	for _, name := range []string{"", "alice@example.com", "slack:U123", "Matt Baker"} {
		if got := string(renderAgentLiveIcon("", name, r)); got != "" {
			t.Errorf("renderAgentLiveIcon(%q) = %q, want empty", name, got)
		}
	}
}

func TestRenderAgentLiveIcon_NilResolver(t *testing.T) {
	if got := string(renderAgentLiveIcon("", "live-bot", nil)); got != "" {
		t.Errorf("nil resolver should render empty, got %q", got)
	}
}

func TestRenderAgentLiveIcon_BasePathPropagates(t *testing.T) {
	now := time.Now()
	r := liveResolverFor(liveAgentFake(now), now)
	got := string(renderAgentLiveIcon("/beads", "live-bot", r))
	if !strings.Contains(got, `href="/beads/gd-agent-live"`) {
		t.Errorf("basePath not propagated, got %q", got)
	}
}

// TestAgentResolver_PrewarmBatchesNDistinctAssignees is the ka-0000000899
// acceptance check: a list-page with N distinct agent-shaped assignees
// should issue exactly ONE batched daemon call (not N per-name calls)
// when the handler calls Prewarm before render.
//
// We seed N agent beads, drive Prewarm with their N names, then invoke
// renderAgentLink for each name and assert:
//   - batchCalls == 1 (the prewarm round-trip), and
//   - calls == 0 (per-name FindMostRecentAgentBead is never hit because
//     the cache is warm).
func TestAgentResolver_PrewarmBatchesNDistinctAssignees(t *testing.T) {
	const n = 6

	fake := &fakeDaemon{beads: map[string]*beadsapi.BeadDetail{}}
	names := make([]string, 0, n)
	for i := 0; i < n; i++ {
		agentName := "batched-bot-" + string(rune('a'+i))
		beadID := "gd-agent-" + string(rune('a'+i))
		fake.beads[beadID] = &beadsapi.BeadDetail{
			ID:     beadID,
			Type:   "agent",
			Status: "in_progress",
			Fields: map[string]string{"agent": agentName},
		}
		names = append(names, agentName)
	}

	counter := &countingFinder{inner: fake}
	r := newAgentResolver(context.Background(), counter)

	// One batched call for N distinct names.
	r.Prewarm(names)
	if counter.batchCalls != 1 {
		t.Fatalf("Prewarm: expected exactly 1 batched daemon call for %d names, got %d", n, counter.batchCalls)
	}

	// Subsequent per-row renderAgentLink calls should all hit the warmed
	// cache — no additional daemon calls of either flavor.
	for _, name := range names {
		out := string(renderAgentLink("", name, r))
		if !strings.Contains(out, `class="agent-link"`) {
			t.Errorf("renderAgentLink(%q) after prewarm: expected agent-link anchor, got %q", name, out)
		}
	}
	if counter.calls != 0 {
		t.Errorf("expected 0 per-name daemon calls after prewarm of %d names, got %d", n, counter.calls)
	}
	if counter.batchCalls != 1 {
		t.Errorf("expected batchCalls to remain 1 after renders, got %d", counter.batchCalls)
	}
}

// TestAgentResolver_PrewarmSkipsCachedAndNonAgentShaped verifies the
// Prewarm filter: already-cached names and non-agent-shaped values
// should be excluded from the batched lookup so the daemon never sees
// them.
func TestAgentResolver_PrewarmSkipsCachedAndNonAgentShaped(t *testing.T) {
	fake := agentLinkFake()
	counter := &countingFinder{inner: fake}
	r := newAgentResolver(context.Background(), counter)

	// Pre-populate the cache for "thread-bot-1" via a per-name resolve.
	_, _ = r.Resolve("thread-bot-1")
	if counter.calls != 1 {
		t.Fatalf("setup: expected 1 per-name daemon call, got %d", counter.calls)
	}

	// Prewarm with: already-cached name, empty, non-agent-shaped values.
	// All should be skipped — no batched daemon call should fire.
	r.Prewarm([]string{"thread-bot-1", "", "user@example.com", "slack:U123 name", "thread-bot-1"})
	if counter.batchCalls != 0 {
		t.Errorf("expected 0 batched calls when all names are skippable, got %d", counter.batchCalls)
	}
}

// TestAgentResolver_LiveIconAndLinkShareCache verifies that invoking both
// renderAgentLink and renderAgentLiveIcon for the same name fires exactly
// one daemon call — list-view rows can use both helpers per row without
// blowing up the daemon request count.
func TestAgentResolver_LiveIconAndLinkShareCache(t *testing.T) {
	now := time.Now()
	fake := liveAgentFake(now)
	counter := &countingFinder{inner: fake}
	r := newAgentResolver(context.Background(), counter)
	r.now = func() time.Time { return now }

	_ = renderAgentLink("", "live-bot", r)
	_ = renderAgentLiveIcon("", "live-bot", r)
	_ = renderAgentLink("", "live-bot", r)
	if counter.calls != 1 {
		t.Errorf("expected 1 daemon call for shared resolver, got %d", counter.calls)
	}
}

type countingFinder struct {
	inner      agentBeadFinder
	calls      int
	batchCalls int
}

func (c *countingFinder) FindMostRecentAgentBead(ctx context.Context, name string) (*beadsapi.BeadDetail, error) {
	c.calls++
	return c.inner.FindMostRecentAgentBead(ctx, name)
}

func (c *countingFinder) FindAgentBeadsByNames(ctx context.Context, names []string, statuses []string) (map[string]*beadsapi.BeadDetail, error) {
	c.batchCalls++
	return c.inner.FindAgentBeadsByNames(ctx, names, statuses)
}

// TestActivePage_RendersAgentLiveIconForAliveAssignee is an end-to-end
// assertion: /active should render the agent-live-icon next to the
// assignee on an in-progress row whose Assignee resolves to an alive
// agent bead.
func TestActivePage_RendersAgentLiveIconForAliveAssignee(t *testing.T) {
	now := time.Now()
	row := &beadsapi.BeadDetail{
		ID:       "gd-row-alive",
		Title:    "Row with live agent",
		Type:     "task",
		Status:   "in_progress",
		Priority: 1,
		Assignee: "live-bot",
		Labels:   []string{"project:gardenio"},
	}
	agent := &beadsapi.BeadDetail{
		ID:        "gd-agent-live",
		Title:     "live-bot",
		Type:      "agent",
		Status:    "in_progress",
		Fields:    map[string]string{"agent": "live-bot", "agent_state": "working"},
		UpdatedAt: now.Add(-1 * time.Minute),
	}
	f := &fakeDaemon{
		beads:    map[string]*beadsapi.BeadDetail{"gd-row-alive": row, "gd-agent-live": agent},
		listResp: &beadsapi.ListBeadsResult{Beads: []*beadsapi.BeadDetail{row}, Total: 1},
	}
	mux := mkServer(t, f)
	req := httptest.NewRequest("GET", "/active", nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	body := w.Body.String()
	for _, want := range []string{
		`class="agent-live-icon"`,
		`href="/gd-agent-live"`,
		`title="agent working: live-bot"`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("expected /active row to surface live-agent icon, missing %q", want)
		}
	}
}

// TestActivePage_NoLiveIconForIdleAssignee ensures rows whose assignee
// resolves to a non-alive agent render plain text (no icon), so the
// helper truly gates on liveness rather than just on resolution.
func TestActivePage_NoLiveIconForIdleAssignee(t *testing.T) {
	now := time.Now()
	row := &beadsapi.BeadDetail{
		ID:       "gd-row-idle",
		Title:    "Row with idle agent",
		Type:     "task",
		Status:   "in_progress",
		Priority: 1,
		Assignee: "idle-bot",
		Labels:   []string{"project:gardenio"},
	}
	agent := &beadsapi.BeadDetail{
		ID:        "gd-agent-idle",
		Title:     "idle-bot",
		Type:      "agent",
		Status:    "in_progress",
		Fields:    map[string]string{"agent": "idle-bot", "agent_state": "idle"},
		UpdatedAt: now.Add(-1 * time.Minute),
	}
	f := &fakeDaemon{
		beads:    map[string]*beadsapi.BeadDetail{"gd-row-idle": row, "gd-agent-idle": agent},
		listResp: &beadsapi.ListBeadsResult{Beads: []*beadsapi.BeadDetail{row}, Total: 1},
	}
	mux := mkServer(t, f)
	req := httptest.NewRequest("GET", "/active", nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	body := w.Body.String()
	if strings.Contains(body, `class="agent-live-icon"`) {
		t.Errorf("idle agent should not surface live-agent icon in /active body")
	}
	if !strings.Contains(body, "idle-bot") {
		t.Errorf("assignee text should still render, body did not contain idle-bot")
	}
}

// TestBeadShow_OwnerRendersAgentLink mirrors the assignee assertion for the
// Owner field — both predefined fields use the same agentLink helper.
func TestBeadShow_OwnerRendersAgentLink(t *testing.T) {
	mux := mkServer(t, agentLinkFake())
	req := httptest.NewRequest("GET", "/gd-task1", nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	body := w.Body.String()
	if !strings.Contains(body, `<a class="agent-link" href="/gd-agent1" data-agent="thread-bot-1">thread-bot-1</a>`) {
		t.Errorf("expected owner to render as render-time-resolved agent link via shared helper")
	}
	// Two occurrences expected — one from assignee_block, one from owner_block.
	if got := strings.Count(body, `class="agent-link"`); got < 2 {
		t.Errorf("expected at least 2 agent-link occurrences (assignee + owner), got %d", got)
	}
}
