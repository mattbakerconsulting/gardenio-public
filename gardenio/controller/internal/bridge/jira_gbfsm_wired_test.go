package bridge

// Wired end-to-end tests for the GBFSM upsert through the REAL handleUpdated event
// path + the LIVE WritePolicyStore (1:1 reshape ka-0000002637, Phase 2
// ka-0000001491). The client-level create/search/edit units + the security guards
// live in jira_analysis_test.go / jira_gbfsm_upsert_test.go; this file drives the
// full sync path (create-vs-edit selection, idempotency, inertness, and the
// behavioral nobypass guarantee that a poisoned search can never redirect an edit
// onto FSM) against a search-aware recording Jira server.

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// newGBFSMSync builds a JiraSync wired exactly like production (WritePolicyStore,
// enforce mode) with the GBFSM issue type + label/version field ids armed and the
// acting agent mapped to fsm-triage.
func newGBFSMSync(t *testing.T, jiraURL string, outboxOn bool) *JiraSync {
	t.Helper()
	return NewJiraSync(JiraSyncConfig{
		Jira:              newTestJiraClient(jiraURL),
		Logger:            discardErrLogger(),
		BotAccountID:      "bot-acct-gbfsm",
		WritePolicy:       storePolicy(outboxOn, AllowlistModeEnforce),
		Roles:             &roleResolverStub{roles: map[string]string{"triage-agent": "fsm-triage"}},
		AnalysisIssueType: analysisTestType,
		LabelsFieldID:     labelsTestField,
		VersionFieldID:    versionTestField,
	})
}

// TestGBFSMUpsert_NoExisting_CreatesOne proves the happy path: no existing GBFSM
// ticket ⇒ exactly ONE create (GBFSM project, labels + version as fields) and NO
// edit and NO direct FSM write.
func TestGBFSMUpsert_NoExisting_CreatesOne(t *testing.T) {
	srv, g := newGBFSMSearchServer(`{"issues":[]}`, "GFO-900")
	defer srv.Close()

	s := newGBFSMSync(t, srv.URL, true)
	s.handleUpdated(context.Background(), triageClaimEvent("bd-create", "FSM-700"))

	creates := outboxCreates(g.jiraRecordingServer)
	if len(creates) != 1 {
		t.Fatalf("expected exactly 1 GBFSM create, got %d", len(creates))
	}
	if got := projectID(t, creates[0]); got != outboxProjectID {
		t.Errorf("create project = %q, want GBFSM", got)
	}
	// The classification rides as FIELDS on the one ticket (triageClaimEvent stamps
	// gardenio-triaged/sev-1/gxp-med + 2026.7.0).
	if got := fieldStrings(t, creates[0], labelsTestField); len(got) != 3 {
		t.Errorf("create labels field = %v, want the 3 triage labels", got)
	}
	if got, _ := creates[0][versionTestField].(string); got != "2026.7.0" {
		t.Errorf("create version field = %q, want 2026.7.0", got)
	}
	if n := len(editPuts(g.jiraRecordingServer, "/issue/")); n != 0 {
		t.Errorf("no-existing path must make NO edit PUT, got %d", n)
	}
	if n := len(g.jiraRecordingServer.writesTo("/issue/FSM-700")); n != 0 {
		t.Errorf("bot must make NO direct write to the FSM ticket, got %d", n)
	}
}

// TestGBFSMUpsert_Existing_Edits proves re-triage UPSERTS: an existing GBFSM ticket
// ⇒ exactly ONE edit PUT to the GBFSM key (never a second create, never an FSM write).
func TestGBFSMUpsert_Existing_Edits(t *testing.T) {
	srv, g := newGBFSMSearchServer(gbfsmIssueJSON("GFO-501", outboxProjectID, "FSM-700"), "GFO-501")
	defer srv.Close()

	s := newGBFSMSync(t, srv.URL, true)
	s.handleUpdated(context.Background(), triageClaimEvent("bd-edit", "FSM-700"))

	if n := len(outboxCreates(g.jiraRecordingServer)); n != 0 {
		t.Errorf("existing ticket must be EDITED, not re-created — got %d creates", n)
	}
	puts := editPuts(g.jiraRecordingServer, "/issue/GFO-501")
	if len(puts) != 1 {
		t.Fatalf("expected exactly 1 edit PUT to the GBFSM key GFO-501, got %d", len(puts))
	}
	// The edit must NEVER target the FSM ticket.
	if n := len(editPuts(g.jiraRecordingServer, "/issue/FSM-700")); n != 0 {
		t.Errorf("edit must NEVER target the FSM ticket, got %d", n)
	}
}

// TestGBFSMUpsert_PoisonedSearch_NeverEditsFSM is the behavioral nobypass guard: a
// compromised Jira returns an FSM-project issue (matching fsm_target_key) as the
// search result. SearchGBFSM rejects it, so the upsert falls to the (GBFSM-hardcoded)
// create — and NOT a single PUT ever targets an FSM key.
func TestGBFSMUpsert_PoisonedSearch_NeverEditsFSM(t *testing.T) {
	// Poisoned: an FSM-project (id 10099) issue whose KEY is an FSM key.
	srv, g := newGBFSMSearchServer(gbfsmIssueJSON("FSM-700", "10099", "FSM-700"), "GFO-1")
	defer srv.Close()

	s := newGBFSMSync(t, srv.URL, true)
	s.handleUpdated(context.Background(), triageClaimEvent("bd-poison", "FSM-700"))

	// The poisoned result was rejected ⇒ a fresh GBFSM ticket is created instead.
	creates := outboxCreates(g.jiraRecordingServer)
	if len(creates) != 1 {
		t.Fatalf("poisoned search: expected the fallback GBFSM create, got %d creates", len(creates))
	}
	if got := projectID(t, creates[0]); got != outboxProjectID {
		t.Errorf("fallback create must be GBFSM, got project %q", got)
	}
	// The load-bearing assertion: NO edit PUT to ANY FSM key, ever.
	if n := len(editPuts(g.jiraRecordingServer, "/issue/FSM-")); n != 0 {
		t.Fatalf("SECURITY: a poisoned search result redirected an edit onto an FSM key (%d PUTs) — "+
			"the project-id reject guard failed (ka-0000001491)", n)
	}
}

// TestGBFSMUpsert_SearchError_NoWrite proves fail-safe-against-duplication: when the
// search itself errors (HTTP 500), the upsert makes NO create and NO edit (the ticket
// may already exist; creating blind would duplicate).
func TestGBFSMUpsert_SearchError_NoWrite(t *testing.T) {
	rec := &jiraRecordingServer{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		rec.mu.Lock()
		rec.requests = append(rec.requests, recordedJiraRequest{Method: r.Method, Path: r.URL.Path, Body: body})
		rec.mu.Unlock()
		if r.Method == http.MethodGet && strings.Contains(r.URL.Path, "/search/jql") {
			w.WriteHeader(http.StatusInternalServerError)
			fmt.Fprint(w, `{"errorMessages":["boom"]}`)
			return
		}
		w.WriteHeader(http.StatusCreated)
		fmt.Fprint(w, `{"id":"1","key":"GFO-1"}`)
	}))
	defer srv.Close()

	s := NewJiraSync(JiraSyncConfig{
		Jira:              newTestJiraClient(srv.URL),
		Logger:            discardErrLogger(),
		WritePolicy:       storePolicy(true, AllowlistModeEnforce),
		Roles:             &roleResolverStub{roles: map[string]string{"triage-agent": "fsm-triage"}},
		AnalysisIssueType: analysisTestType,
		LabelsFieldID:     labelsTestField,
		VersionFieldID:    versionTestField,
	})
	s.handleUpdated(context.Background(), triageClaimEvent("bd-searcherr", "FSM-700"))

	if n := len(outboxCreates(rec)); n != 0 {
		t.Errorf("search error must NOT create (fail-safe against duplication), got %d creates", n)
	}
	if n := len(editPuts(rec, "/issue/")); n != 0 {
		t.Errorf("search error must NOT edit, got %d PUTs", n)
	}
}

// TestGBFSMUpsert_ReplayDeduped proves the idempotency guarantee: two SSE replays of
// the same triage produce EXACTLY ONE GBFSM ticket — the second replay is deduped in
// the TTL window (no second create, no edit).
func TestGBFSMUpsert_ReplayDeduped(t *testing.T) {
	srv, g := newGBFSMSearchServer(`{"issues":[]}`, "GFO-900")
	defer srv.Close()

	s := newGBFSMSync(t, srv.URL, true)
	ev := triageClaimEvent("bd-replay", "FSM-700")
	s.handleUpdated(context.Background(), ev)
	s.handleUpdated(context.Background(), ev)

	if n := len(outboxCreates(g.jiraRecordingServer)); n != 1 {
		t.Errorf("two replays must yield exactly ONE GBFSM ticket, got %d creates", n)
	}
}

// TestGBFSMUpsert_ToggleOff_NoWrite proves the outbox toggle still wins: with
// JIRA_OUTBOX_ENABLED off, a fully-classified triage bead makes ZERO outbound
// requests — not even the read-only search.
func TestGBFSMUpsert_ToggleOff_NoWrite(t *testing.T) {
	srv, g := newGBFSMSearchServer(`{"issues":[]}`, "GFO-1")
	defer srv.Close()

	s := newGBFSMSync(t, srv.URL, false /* outbox off */)
	s.handleUpdated(context.Background(), triageClaimEvent("bd-off", "FSM-700"))

	g.jiraRecordingServer.mu.Lock()
	n := len(g.jiraRecordingServer.requests)
	g.jiraRecordingServer.mu.Unlock()
	if n != 0 {
		t.Errorf("outbox toggle off: expected 0 outbound requests, got %d", n)
	}
}

// TestGBFSMUpsert_InertNoIssueType proves inertness axis 1: outbox ON but no GBFSM
// issue type configured ⇒ ZERO outbound requests (no search, no create).
func TestGBFSMUpsert_InertNoIssueType(t *testing.T) {
	srv, g := newGBFSMSearchServer(`{"issues":[]}`, "GFO-1")
	defer srv.Close()

	s := NewJiraSync(JiraSyncConfig{
		Jira:        newTestJiraClient(srv.URL),
		Logger:      discardErrLogger(),
		WritePolicy: storePolicy(true, AllowlistModeEnforce),
		Roles:       &roleResolverStub{roles: map[string]string{"triage-agent": "fsm-triage"}},
		// AnalysisIssueType deliberately empty ⇒ inert.
	})
	s.handleUpdated(context.Background(), triageClaimEvent("bd-notype", "FSM-700"))

	g.jiraRecordingServer.mu.Lock()
	n := len(g.jiraRecordingServer.requests)
	g.jiraRecordingServer.mu.Unlock()
	if n != 0 {
		t.Errorf("no issue type configured: expected 0 outbound requests, got %d", n)
	}
}
