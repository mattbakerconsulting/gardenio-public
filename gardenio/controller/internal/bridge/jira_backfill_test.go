package bridge

// Unit tests for the FSM→GBFSM backfill DRY-RUN scaffold (epic ka-0000002637,
// Phase 5a ka-0000000123).
//
// The load-bearing acceptance criterion this file pins: the dry-run enumerates +
// logs the would-upsert GBFSM ticket for each in-scope FSM ticket while making
// ZERO Jira mutation calls (no create, no edit — not even the read-only search).
// It drives the backfill against a REAL wired JiraSync whose JiraClient points at
// a recording Jira server, and asserts that server received NOT ONE request.

import (
	"bytes"
	"context"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"testing"

	"gardenio/controller/internal/beadsapi"
)

// backfillFakeDaemon is a read-only beads source that serves a fixed set of
// BeadDetail rows, paginated by the query's Limit/Offset. It records each query so
// tests can assert the enumeration scope (kind/labels/field filter).
type backfillFakeDaemon struct {
	mu      sync.Mutex
	beads   []*beadsapi.BeadDetail
	queries []beadsapi.ListBeadsQuery
	err     error
}

func (d *backfillFakeDaemon) ListBeadsFiltered(_ context.Context, q beadsapi.ListBeadsQuery) (*beadsapi.ListBeadsResult, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.queries = append(d.queries, q)
	if d.err != nil {
		return nil, d.err
	}
	start := min(q.Offset, len(d.beads))
	end := len(d.beads)
	if q.Limit > 0 && start+q.Limit < end {
		end = start + q.Limit
	}
	page := append([]*beadsapi.BeadDetail(nil), d.beads[start:end]...)
	return &beadsapi.ListBeadsResult{Beads: page, Total: len(d.beads)}, nil
}

// fsmBead builds an in-scope FSM-sourced BeadDetail carrying the triage
// classification labels + recommended_affects_version the live upsert keys on.
//
// It is shaped like the REAL FSM population — bound by jira_key + jira:<KEY> label
// with NO jira_project field, exactly what the spawn_agent JIRA trigger stamps
// (trigger_source_jira.go). The backfill must recognize + plan it from the bound-key
// prefix alone; deliberately omitting jira_project keeps the whole suite honest
// against a regression back to the field filter that matched zero real beads
// (ka-0000006784).
func fsmBead(id, fsmKey string) *beadsapi.BeadDetail {
	return &beadsapi.BeadDetail{
		ID:     id,
		Type:   "task",
		Kind:   "issue",
		Title:  "[" + fsmKey + "] Customer escalation",
		Status: "open",
		Labels: []string{"source:jira", "jira:" + fsmKey, "gardenio-triaged", "sev-1", "gxp-med"},
		Fields: map[string]string{
			"jira_key":                    fsmKey,
			"recommended_affects_version": "2026.7.0",
		},
	}
}

// newBackfillSync builds a JiraSync wired like production (WritePolicyStore, enforce
// mode, GBFSM issue type + field ids armed) pointing at jiraURL. The acting agent
// "triage-agent" maps to fsm-triage so the field gate admits labels + version.
func newBackfillSync(t *testing.T, jiraURL string, outboxOn bool) *JiraSync {
	t.Helper()
	return NewJiraSync(JiraSyncConfig{
		Jira:              newTestJiraClient(jiraURL),
		Logger:            discardErrLogger(),
		WritePolicy:       storePolicy(outboxOn, AllowlistModeEnforce),
		Roles:             &roleResolverStub{roles: map[string]string{"triage-agent": "fsm-triage"}},
		AnalysisIssueType: analysisTestType,
		LabelsFieldID:     labelsTestField,
		VersionFieldID:    versionTestField,
	})
}

// TestBackfillDryRun_ZeroJiraMutations is THE acceptance test: the dry-run
// enumerates + logs a would-upsert for each in-scope FSM ticket while the wired
// Jira client receives ZERO requests — no create, no edit, no search.
func TestBackfillDryRun_ZeroJiraMutations(t *testing.T) {
	srv, rec := newJiraRecordingServer()
	defer srv.Close()

	daemon := &backfillFakeDaemon{beads: []*beadsapi.BeadDetail{
		fsmBead("bd-1", "FSM-101"),
		fsmBead("bd-2", "FSM-102"),
		fsmBead("bd-3", "FSM-103"),
	}}

	b := NewGBFSMBackfill(GBFSMBackfillConfig{
		Sync:   newBackfillSync(t, srv.URL, true /* outbox on */),
		Daemon: daemon,
		Logger: discardErrLogger(),
		DryRun: true,
	})

	stats, err := b.Run(context.Background())
	if err != nil {
		t.Fatalf("dry-run Run: %v", err)
	}

	// The whole point: NOT ONE request reached Jira.
	rec.mu.Lock()
	n := len(rec.requests)
	reqs := append([]recordedJiraRequest(nil), rec.requests...)
	rec.mu.Unlock()
	if n != 0 {
		t.Fatalf("dry-run must make ZERO Jira requests (no create/edit/search), got %d: %+v", n, reqs)
	}

	if stats.Scanned != 3 || stats.Planned != 3 {
		t.Errorf("stats = %+v, want Scanned=3 Planned=3", stats)
	}
}

// TestBackfillDryRun_PlanMatchesLiveTicket proves the preview equals what the live
// upsert would build: the logged plan carries the FSM target, the classification
// as fields (labels + version), the armed issue type, and the shared dedup key —
// exactly buildGBFSMTicket's output for the same bead.
func TestBackfillDryRun_PlanMatchesLiveTicket(t *testing.T) {
	srv, _ := newJiraRecordingServer()
	defer srv.Close()

	var logBuf bytes.Buffer
	sync := newBackfillSync(t, srv.URL, true)
	daemon := &backfillFakeDaemon{beads: []*beadsapi.BeadDetail{fsmBead("bd-1", "FSM-408")}}

	b := NewGBFSMBackfill(GBFSMBackfillConfig{
		Sync:   sync,
		Daemon: daemon,
		Logger: slog.New(slog.NewJSONHandler(&logBuf, &slog.HandlerOptions{Level: slog.LevelInfo})),
		DryRun: true,
	})
	if _, err := b.Run(context.Background()); err != nil {
		t.Fatalf("dry-run Run: %v", err)
	}

	// The independently-computed live ticket for the same bead is the oracle. The
	// backfill gates the classification under the canonical triageRole (see planOne).
	bead := fsmBead("bd-1", "FSM-408")
	want := sync.buildGBFSMTicket(beadDetailToEvent(bead), "FSM-408", triageRole)

	logged := logBuf.String()
	for _, frag := range []string{
		`"fsm_target_key":"FSM-408"`,
		`"issue_type":"` + analysisTestType + `"`,
		`"affects_version":"` + want.Version + `"`,
		`"dedup_key":"` + want.DedupKey() + `"`,
		`"role":"` + triageRole + `"`,
	} {
		if !strings.Contains(logged, frag) {
			t.Errorf("dry-run log missing %s\nlog: %s", frag, logged)
		}
	}
	// Sanity: the oracle actually carried the classification (guards against a
	// vacuous match if buildGBFSMTicket ever silently dropped the fields).
	if want.Version != "2026.7.0" || len(want.Labels) != 3 {
		t.Fatalf("oracle ticket unexpectedly empty: version=%q labels=%v", want.Version, want.Labels)
	}
}

// TestBackfillDryRun_EnumerationScope pins the read-only query: kind=issue,
// source:jira, the full status set — and CRUCIALLY that it does NOT filter on a
// jira_project field. FSM scoping is applied per-bead on the bound key's prefix
// (see TestBackfillDryRun_SkipsNonFSMProject); a jira_project field filter would
// match ZERO real FSM beads because the trigger ingestion path never stamps that
// field (ka-0000006784).
func TestBackfillDryRun_EnumerationScope(t *testing.T) {
	srv, _ := newJiraRecordingServer()
	defer srv.Close()

	daemon := &backfillFakeDaemon{beads: []*beadsapi.BeadDetail{fsmBead("bd-1", "FSM-1")}}
	b := NewGBFSMBackfill(GBFSMBackfillConfig{
		Sync:   newBackfillSync(t, srv.URL, true),
		Daemon: daemon,
		Logger: discardErrLogger(),
		DryRun: true,
	})
	if _, err := b.Run(context.Background()); err != nil {
		t.Fatalf("dry-run Run: %v", err)
	}

	daemon.mu.Lock()
	defer daemon.mu.Unlock()
	if len(daemon.queries) == 0 {
		t.Fatal("expected at least one enumeration query")
	}
	q := daemon.queries[0]
	if len(q.Kinds) != 1 || q.Kinds[0] != "issue" {
		t.Errorf("query Kinds = %v, want [issue]", q.Kinds)
	}
	if !hasLabel(q.Labels, "source:jira") {
		t.Errorf("query Labels = %v, want to include source:jira", q.Labels)
	}
	// Regression guard (ka-0000006784): the enumeration must NOT filter on a
	// jira_project field. Only the poller stamps jira_project; FSM enters via a
	// spawn_agent trigger that stamps jira_key / jira:<KEY> but NOT jira_project, so
	// a jira_project=FSM filter matched zero real FSM beads and made the whole
	// backfill vacuously scan 0. Scoping is by the bound-key prefix, per-bead.
	if _, ok := q.Fields["jira_project"]; ok {
		t.Errorf("query must NOT filter on jira_project (trigger-sourced FSM beads lack it); Fields = %v", q.Fields)
	}
	// The status set must be the full FSM-bead population (incl. deferred/blocked),
	// NOT the narrower 3-status dedup-marker set — else the preview under-covers the
	// backlog and the eventual live run would skip blocked/deferred FSM tickets.
	for _, want := range []string{"open", "in_progress", "closed", "deferred", "blocked"} {
		if !hasLabel(q.Statuses, want) { // hasLabel is a slice-membership check
			t.Errorf("query Statuses = %v, must include %q (full FSM population)", q.Statuses, want)
		}
	}
}

// TestBackfillDryRun_SkipsNonFSMProject is the regression test for ka-0000006784:
// FSM scoping is applied on the BOUND Jira key's project prefix, not a jira_project
// field. An FSM-bound bead (identified by jira:<KEY> / jira_key alone, NO
// jira_project field — exactly how the trigger ingestion path shapes it) is PLANNED;
// a non-FSM-bound source:jira issue bead (PE-/DEVOPS-, poller-sourced into the same
// daemon) is scanned but SkippedProject.
func TestBackfillDryRun_SkipsNonFSMProject(t *testing.T) {
	srv, rec := newJiraRecordingServer()
	defer srv.Close()

	// An FSM bead shaped like the trigger path: jira_key + jira:<KEY> label, but NO
	// jira_project field. This is the population the old field-filter query missed.
	fsmTriggerShaped := &beadsapi.BeadDetail{
		ID: "bd-fsm", Type: "task", Kind: "issue", Status: "closed",
		Labels: []string{"source:jira", "jira:FSM-450", "gardenio-triaged", "sev-1"},
		Fields: map[string]string{"jira_key": "FSM-450"}, // NO jira_project
	}
	// A poller-sourced PE bead sharing the daemon — out of scope for the FSM backfill.
	peBead := &beadsapi.BeadDetail{
		ID: "bd-pe", Type: "task", Kind: "issue", Status: "open",
		Labels: []string{"source:jira", "jira:PE-1234"},
		Fields: map[string]string{"jira_key": "PE-1234", "jira_project": "PE"},
	}

	daemon := &backfillFakeDaemon{beads: []*beadsapi.BeadDetail{fsmTriggerShaped, peBead}}
	b := NewGBFSMBackfill(GBFSMBackfillConfig{
		Sync:   newBackfillSync(t, srv.URL, true),
		Daemon: daemon,
		Logger: discardErrLogger(),
		DryRun: true,
	})
	stats, err := b.Run(context.Background())
	if err != nil {
		t.Fatalf("dry-run Run: %v", err)
	}

	if stats.Scanned != 2 {
		t.Errorf("Scanned = %d, want 2", stats.Scanned)
	}
	if stats.Planned != 1 {
		t.Errorf("Planned = %d, want 1 (the FSM-bound bead with NO jira_project field must still be planned)", stats.Planned)
	}
	if stats.SkippedProject != 1 {
		t.Errorf("SkippedProject = %d, want 1 (the PE-bound bead is out of scope)", stats.SkippedProject)
	}
	rec.mu.Lock()
	n := len(rec.requests)
	rec.mu.Unlock()
	if n != 0 {
		t.Errorf("dry-run must make ZERO Jira requests, got %d", n)
	}
}

// TestBackfillStatusesMatchPopulation pins the population oracle: the backfill's
// status set is exactly the shared fsmSourcedBeadStatuses the poller enumerates,
// so the two can never drift and the preview always covers the whole backlog.
func TestBackfillStatusesMatchPopulation(t *testing.T) {
	want := []string{"open", "in_progress", "closed", "deferred", "blocked"}
	if len(fsmSourcedBeadStatuses) != len(want) {
		t.Fatalf("fsmSourcedBeadStatuses = %v, want %v", fsmSourcedBeadStatuses, want)
	}
	for i, s := range want {
		if fsmSourcedBeadStatuses[i] != s {
			t.Errorf("fsmSourcedBeadStatuses[%d] = %q, want %q", i, fsmSourcedBeadStatuses[i], s)
		}
	}
}

// TestBackfillDryRun_EnumeratesDeferredAndBlocked proves a deferred/blocked FSM
// bead is enumerated + planned (the fake daemon serves whatever statuses the query
// asks for, so this asserts the backfill asks for — and processes — them).
func TestBackfillDryRun_EnumeratesDeferredAndBlocked(t *testing.T) {
	srv, _ := newJiraRecordingServer()
	defer srv.Close()

	deferred := fsmBead("bd-deferred", "FSM-700")
	deferred.Status = "deferred"
	blocked := fsmBead("bd-blocked", "FSM-701")
	blocked.Status = "blocked"

	daemon := &backfillFakeDaemon{beads: []*beadsapi.BeadDetail{deferred, blocked}}
	b := NewGBFSMBackfill(GBFSMBackfillConfig{
		Sync:   newBackfillSync(t, srv.URL, true),
		Daemon: daemon,
		Logger: discardErrLogger(),
		DryRun: true,
	})
	stats, err := b.Run(context.Background())
	if err != nil {
		t.Fatalf("dry-run Run: %v", err)
	}
	if stats.Planned != 2 {
		t.Errorf("Planned = %d, want 2 (deferred + blocked FSM beads must be covered)", stats.Planned)
	}
}

// TestBackfillDryRun_SkipsUnboundAndDark proves the two skip paths: a bead with no
// bound jira_key is skipped (nothing to seed), and a dark-mirrored bead is skipped
// (never flows upstream). Neither is planned; both are tallied.
func TestBackfillDryRun_SkipsUnboundAndDark(t *testing.T) {
	srv, rec := newJiraRecordingServer()
	defer srv.Close()

	unbound := &beadsapi.BeadDetail{
		ID: "bd-unbound", Type: "task", Kind: "issue", Status: "open",
		Labels: []string{"source:jira", "gardenio-triaged"},
		Fields: map[string]string{"jira_project": "FSM"}, // no jira_key, no jira:<KEY> label
	}
	dark := fsmBead("bd-dark", "FSM-900")
	dark.Labels = append(dark.Labels, jiraDarkLabel)

	daemon := &backfillFakeDaemon{beads: []*beadsapi.BeadDetail{
		fsmBead("bd-ok", "FSM-500"),
		unbound,
		dark,
	}}
	b := NewGBFSMBackfill(GBFSMBackfillConfig{
		Sync:   newBackfillSync(t, srv.URL, true),
		Daemon: daemon,
		Logger: discardErrLogger(),
		DryRun: true,
	})
	stats, err := b.Run(context.Background())
	if err != nil {
		t.Fatalf("dry-run Run: %v", err)
	}

	if stats.Scanned != 3 {
		t.Errorf("Scanned = %d, want 3", stats.Scanned)
	}
	if stats.Planned != 1 {
		t.Errorf("Planned = %d, want 1 (only the bound, non-dark bead)", stats.Planned)
	}
	if stats.SkippedKey != 1 {
		t.Errorf("SkippedKey = %d, want 1", stats.SkippedKey)
	}
	if stats.SkippedDark != 1 {
		t.Errorf("SkippedDark = %d, want 1", stats.SkippedDark)
	}
	rec.mu.Lock()
	n := len(rec.requests)
	rec.mu.Unlock()
	if n != 0 {
		t.Errorf("dry-run must still make ZERO Jira requests, got %d", n)
	}
}

// TestBackfillDryRun_PaginatesAllPages proves the enumeration walks every page: a
// backlog larger than the page size is fully scanned (no silent truncation).
func TestBackfillDryRun_PaginatesAllPages(t *testing.T) {
	srv, _ := newJiraRecordingServer()
	defer srv.Close()

	var beads []*beadsapi.BeadDetail
	for i := range 7 {
		beads = append(beads, fsmBead(fmt.Sprintf("bd-%d", i), fmt.Sprintf("FSM-%d", 1000+i)))
	}
	daemon := &backfillFakeDaemon{beads: beads}
	b := NewGBFSMBackfill(GBFSMBackfillConfig{
		Sync:     newBackfillSync(t, srv.URL, true),
		Daemon:   daemon,
		Logger:   discardErrLogger(),
		DryRun:   true,
		PageSize: 3, // 7 beads ⇒ pages of 3,3,1
	})
	stats, err := b.Run(context.Background())
	if err != nil {
		t.Fatalf("dry-run Run: %v", err)
	}
	if stats.Scanned != 7 || stats.Planned != 7 {
		t.Errorf("stats = %+v, want Scanned=7 Planned=7 (all pages walked)", stats)
	}
	daemon.mu.Lock()
	pages := len(daemon.queries)
	daemon.mu.Unlock()
	if pages != 3 {
		t.Errorf("expected 3 pages (3+3+1), got %d queries", pages)
	}
}

// TestBackfillDryRun_PaginatesExactMultiple covers the boundary the short-page
// terminator relies on: when the backlog is an EXACT multiple of the page size,
// the loop issues one extra (empty) page to confirm the end rather than stopping
// early or looping forever. 6 beads at page size 3 ⇒ pages of 3,3,0.
func TestBackfillDryRun_PaginatesExactMultiple(t *testing.T) {
	srv, _ := newJiraRecordingServer()
	defer srv.Close()

	var beads []*beadsapi.BeadDetail
	for i := range 6 {
		beads = append(beads, fsmBead(fmt.Sprintf("bd-%d", i), fmt.Sprintf("FSM-%d", 2000+i)))
	}
	daemon := &backfillFakeDaemon{beads: beads}
	b := NewGBFSMBackfill(GBFSMBackfillConfig{
		Sync:     newBackfillSync(t, srv.URL, true),
		Daemon:   daemon,
		Logger:   discardErrLogger(),
		DryRun:   true,
		PageSize: 3, // 6 beads ⇒ full pages 3,3 then an empty terminator page
	})
	stats, err := b.Run(context.Background())
	if err != nil {
		t.Fatalf("dry-run Run: %v", err)
	}
	if stats.Scanned != 6 || stats.Planned != 6 {
		t.Errorf("stats = %+v, want Scanned=6 Planned=6 (each bead counted exactly once)", stats)
	}
	daemon.mu.Lock()
	pages := len(daemon.queries)
	daemon.mu.Unlock()
	if pages != 3 {
		t.Errorf("exact-multiple backlog must issue a terminating empty page (3+3+0), got %d queries", pages)
	}
}

// TestBackfillDryRun_EnumeratesRegardlessOfOutboxState proves the dry-run is
// decoupled from the arm state: with the outbox toggle OFF and the analysis issue
// type empty, the dry-run STILL enumerates + plans (it previews before arming) and
// STILL makes zero Jira requests. The live-run gates are not the dry-run's gates.
func TestBackfillDryRun_EnumeratesRegardlessOfOutboxState(t *testing.T) {
	srv, rec := newJiraRecordingServer()
	defer srv.Close()

	// Outbox OFF and NO analysis issue type configured (fully inert live surface).
	sync := NewJiraSync(JiraSyncConfig{
		Jira:        newTestJiraClient(srv.URL),
		Logger:      discardErrLogger(),
		WritePolicy: storePolicy(false /* outbox off */, AllowlistModeEnforce),
		Roles:       &roleResolverStub{roles: map[string]string{"triage-agent": "fsm-triage"}},
		// AnalysisIssueType, LabelsFieldID, VersionFieldID all empty.
	})
	daemon := &backfillFakeDaemon{beads: []*beadsapi.BeadDetail{
		fsmBead("bd-1", "FSM-1"), fsmBead("bd-2", "FSM-2"),
	}}
	b := NewGBFSMBackfill(GBFSMBackfillConfig{
		Sync: sync, Daemon: daemon, Logger: discardErrLogger(), DryRun: true,
	})
	stats, err := b.Run(context.Background())
	if err != nil {
		t.Fatalf("dry-run Run: %v", err)
	}
	if stats.Scanned != 2 || stats.Planned != 2 {
		t.Errorf("stats = %+v, want Scanned=2 Planned=2 (dry-run enumerates pre-arming)", stats)
	}
	rec.mu.Lock()
	n := len(rec.requests)
	rec.mu.Unlock()
	if n != 0 {
		t.Errorf("dry-run must make ZERO Jira requests even pre-arming, got %d", n)
	}
}

// TestBackfill_LiveModeFailsLoud proves the live flip is refused: DryRun=false
// returns an error naming Phase 6 (ka-0000000885) and touches neither Jira nor the
// daemon (the live run is deferred, gated on Phase 4 + a green E2E pipeline).
func TestBackfill_LiveModeFailsLoud(t *testing.T) {
	srv, rec := newJiraRecordingServer()
	defer srv.Close()

	daemon := &backfillFakeDaemon{beads: []*beadsapi.BeadDetail{fsmBead("bd-1", "FSM-1")}}
	b := NewGBFSMBackfill(GBFSMBackfillConfig{
		Sync:   newBackfillSync(t, srv.URL, true),
		Daemon: daemon,
		Logger: discardErrLogger(),
		DryRun: false, // live flip — must be refused
	})
	_, err := b.Run(context.Background())
	if err == nil {
		t.Fatal("live mode (DryRun=false) must return an error — the live run is Phase 6 proper")
	}
	if !strings.Contains(err.Error(), "ka-0000000885") {
		t.Errorf("error should name the deferred Phase-6 bead, got: %v", err)
	}
	// No enumeration and no Jira contact happened.
	daemon.mu.Lock()
	nq := len(daemon.queries)
	daemon.mu.Unlock()
	if nq != 0 {
		t.Errorf("live-mode refusal must not enumerate the daemon, got %d queries", nq)
	}
	rec.mu.Lock()
	nr := len(rec.requests)
	rec.mu.Unlock()
	if nr != 0 {
		t.Errorf("live-mode refusal must not contact Jira, got %d requests", nr)
	}
}

// TestBackfill_RequiresCollaborators proves Run errors (rather than silently
// no-op'ing) when a required collaborator is missing.
func TestBackfill_RequiresCollaborators(t *testing.T) {
	b := NewGBFSMBackfill(GBFSMBackfillConfig{
		Sync:   nil, // missing
		Daemon: &backfillFakeDaemon{},
		Logger: discardErrLogger(),
		DryRun: true,
	})
	if _, err := b.Run(context.Background()); err == nil {
		t.Error("expected an error when Sync is nil")
	}
}

// TestBackfillDryRun_ListErrorPropagates proves a daemon list error surfaces
// (fail-loud) rather than being swallowed.
func TestBackfillDryRun_ListErrorPropagates(t *testing.T) {
	srv, _ := newJiraRecordingServer()
	defer srv.Close()

	daemon := &backfillFakeDaemon{err: fmt.Errorf("daemon down")}
	b := NewGBFSMBackfill(GBFSMBackfillConfig{
		Sync:   newBackfillSync(t, srv.URL, true),
		Daemon: daemon,
		Logger: discardErrLogger(),
		DryRun: true,
	})
	if _, err := b.Run(context.Background()); err == nil {
		t.Error("expected the daemon list error to propagate")
	}
}
