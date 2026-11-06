package bridge

// Focused coverage for the fsm-triage OUTBOX path (epic ka-0000002067; 1:1 reshape
// ka-0000002637, Phase 2 ka-0000001491): the triage classification (labels
// gardenio-triaged / sev-<N> / gxp-<X> + Affects-Version) is written as the labels +
// version FIELDS on the ONE upserted GBFSM ticket in the internal GBFSM outbox
// project — the bot NEVER touches FSM. The write is gated by the SAME fail-closed
// gates as every other outbound write (GBFSM project + allowlist + scope of the FSM
// target key). These tests drive the REAL handleUpdated event path against a
// recording Jira server, wired with the LIVE config-driven WritePolicyStore (exactly
// how cmd/jira-bridge wires prod), so they prove the live decision path, not just the
// policy.
//
// The self-arming suites (jira_allowlist_strong_test.go / jira_sync_strong_test.go)
// pin the policy surface and the legacy-gate wire path; the create/edit/upsert +
// security halves live in jira_analysis_test.go / jira_gbfsm_upsert_test.go; this
// file complements them with the WritePolicyStore wire path plus the
// behavior-preserving guarantees the reshape must not break.

import (
	"bytes"
	"context"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"
)

// newTriageStoreSync builds a JiraSync wired with the LIVE WritePolicyStore (outbox
// toggle per outboxOn, enforce mode), a role resolver mapping the acting agent to
// the given role, and the GBFSM ticket surface ARMED (issue type + label/version
// field ids) — mirroring the production jira-bridge wiring in cmd/jira-bridge/main.go
// with the outbox fully configured. The recording server (newJiraRecordingServer)
// answers the SearchGBFSM GET with an empty result set, so the upsert takes the
// CREATE branch (no existing GBFSM ticket).
func newTriageStoreSync(t *testing.T, jiraURL string, outboxOn bool, agent, role string) *JiraSync {
	t.Helper()
	return NewJiraSync(JiraSyncConfig{
		Jira:                  newTestJiraClient(jiraURL),
		Logger:                slog.New(slog.NewTextHandler(&bytes.Buffer{}, &slog.HandlerOptions{Level: slog.LevelError})),
		BotAccountID:          "bot-acct-triage",
		EnableClaimTransition: true, // so a claim transition WOULD be attempted if allowed
		WritePolicy:           storePolicy(outboxOn, AllowlistModeEnforce),
		Roles:                 &roleResolverStub{roles: map[string]string{agent: role}},
		AnalysisIssueType:     analysisTestType,
		LabelsFieldID:         labelsTestField,
		VersionFieldID:        versionTestField,
	})
}

// TestTriageBridge_StorePath_SingleTicket proves the collapse end-to-end through the
// LIVE WritePolicyStore: an fsm-triage bead bound to an FSM ticket with the outbox
// toggle ON drives exactly ONE GBFSM ticket carrying the N classification labels +
// the Affects-Version as FIELDS — NOT a fan-out of intent tickets — and NOTHING
// else: no comment / transition / assignee, and NO direct write to the FSM ticket,
// even though the bead is an in_progress claim that would otherwise trigger all three
// for a broad role.
func TestTriageBridge_StorePath_SingleTicket(t *testing.T) {
	srv, rec := newJiraRecordingServer()
	defer srv.Close()

	s := newTriageStoreSync(t, srv.URL, true /* outbox on */, "triage-agent", "fsm-triage")
	s.handleUpdated(context.Background(), triageClaimEvent("bd-store-lv", "FSM-700"))

	// Exactly ONE GBFSM ticket (the collapse) — never the old fan-out of intents.
	creates := outboxCreates(rec)
	if len(creates) != 1 {
		t.Fatalf("expected exactly 1 GBFSM ticket (the collapse), got %d creates", len(creates))
	}
	if n := outboxCreatesByType(rec, outboxTypeAddLabel); n != 0 {
		t.Errorf("collapsed writer must emit NO Add-Label intents, got %d", n)
	}
	if n := outboxCreatesByType(rec, outboxTypeSetVersion); n != 0 {
		t.Errorf("collapsed writer must emit NO Set-Version intents, got %d", n)
	}
	// The N labels (gardenio-triaged / sev-1 / gxp-med) + version ride as FIELDS.
	if got := fieldStrings(t, creates[0], labelsTestField); len(got) != 3 {
		t.Errorf("expected 3 triage labels on the ticket's labels field, got %v", got)
	}
	if got, _ := creates[0][versionTestField].(string); got != "2026.7.0" {
		t.Errorf("version field = %q, want 2026.7.0", got)
	}
	// The bound FSM ticket rides only as DATA in fsm_target_key.
	if got, _ := creates[0][outboxFieldTargetKey].(string); got != "FSM-700" {
		t.Errorf("fsm_target_key = %q, want the bound FSM-700", got)
	}
	// The bot NEVER writes the FSM ticket directly.
	if n := len(rec.writesTo("/issue/FSM-700")); n != 0 {
		t.Errorf("bot must make NO direct write to the FSM ticket, got %d", n)
	}
	// No comment / transition / assignee anywhere.
	if n := len(rec.writesTo("/comment")); n != 0 {
		t.Errorf("triage tier must post NO comment, got %d", n)
	}
	if n := len(rec.writesTo("/transitions")); n != 0 {
		t.Errorf("triage tier must make NO transition, got %d", n)
	}
	if n := len(rec.writesTo("/assignee")); n != 0 {
		t.Errorf("triage tier must make NO assignment, got %d", n)
	}
}

// TestTriageBridge_StorePath_ToggleOff_NoWrite proves the outbox toggle still wins:
// with JIRA_OUTBOX_ENABLED OFF, a fully-classified fsm-triage bead produces ZERO
// outbound writes (not even the read-only search) — the pivot is inert until the
// toggle is on, so merging it changes no live behavior (the default is OFF).
func TestTriageBridge_StorePath_ToggleOff_NoWrite(t *testing.T) {
	srv, rec := newJiraRecordingServer()
	defer srv.Close()

	s := newTriageStoreSync(t, srv.URL, false /* outbox OFF */, "triage-agent", "fsm-triage")
	s.handleUpdated(context.Background(), triageClaimEvent("bd-store-off", "FSM-701"))

	rec.mu.Lock()
	n := len(rec.requests)
	rec.mu.Unlock()
	if n != 0 {
		t.Errorf("outbox toggle off: expected 0 outbound requests, got %d: %+v", n, rec.requests)
	}
}

// TestTriageBridge_NonTriageRole_NoTriageWrite is the behavior-preserving guarantee:
// a NON-triage role (crew) whose bead happens to carry triage classification labels
// must NOT drive the GBFSM ticket — that path is gated on the acting role being
// fsm-triage. crew still does its normal broad-role claim writes (comment), but the
// GBFSM ticket must be absent. A stray sev-/gxp- label on a crew bead is inert.
func TestTriageBridge_NonTriageRole_NoTriageWrite(t *testing.T) {
	srv, rec := newJiraRecordingServer()
	defer srv.Close()

	// crew on an allowed project (PE, always writable) carrying triage labels.
	s := newTriageStoreSync(t, srv.URL, false /* fsm toggle irrelevant on PE */, "crew-agent", "crew")
	event := marshalSSEBeadPayload(BeadEvent{
		ID: "bd-crew-triagelabels", Type: "task", Title: "[PE-9100] work", Status: "in_progress", Assignee: "crew-agent",
		Labels: []string{"source:jira", "jira:PE-9100", "gardenio-triaged", "sev-2", "gxp-high"},
		Fields: map[string]string{"jira_key": "PE-9100", "recommended_affects_version": "2026.7.0"},
	})
	s.handleUpdated(context.Background(), event)

	// No GBFSM ticket for a non-triage role, regardless of the classification labels.
	if n := len(outboxCreates(rec)); n != 0 {
		t.Errorf("non-triage role must NOT drive a GBFSM ticket, got %d creates", n)
	}
	// crew's normal broad-role claim behavior is unchanged (sanity: it still
	// comments), proving we didn't accidentally suppress the non-triage path.
	if n := len(rec.writesTo("/comment")); n != 1 {
		t.Errorf("crew claim should still post its 1 internal note, got %d", n)
	}
}

// TestTriageBridge_LabelsOnly_VersionFieldOmitted proves the field independence
// survives the collapse: a triage bead with classification labels but NO recommended
// Affects-Version yields ONE GBFSM ticket carrying the labels field but NO version
// field.
func TestTriageBridge_LabelsOnly_VersionFieldOmitted(t *testing.T) {
	srv, rec := newJiraRecordingServer()
	defer srv.Close()

	s := newTriageStoreSync(t, srv.URL, true, "triage-agent", "fsm-triage")
	event := marshalSSEBeadPayload(BeadEvent{
		ID: "bd-labels-only", Type: "task", Title: "[FSM-702] x", Status: "in_progress", Assignee: "triage-agent",
		Labels: []string{"source:jira", "jira:FSM-702", "gardenio-triaged", "sev-0"},
		Fields: map[string]string{"jira_key": "FSM-702"},
	})
	s.handleUpdated(context.Background(), event)

	creates := outboxCreates(rec)
	if len(creates) != 1 {
		t.Fatalf("labels-only: expected exactly 1 GBFSM ticket, got %d", len(creates))
	}
	if got := fieldStrings(t, creates[0], labelsTestField); len(got) != 2 {
		t.Errorf("labels-only: expected 2 labels on the ticket, got %v", got)
	}
	if _, has := creates[0][versionTestField]; has {
		t.Error("labels-only: version field must be OMITTED (no recommended version)")
	}
}

// TestTriageBridge_VersionOnly_LabelsFieldOmitted is the mirror: a triage bead with
// a recommended Affects-Version but NO classification labels yields ONE GBFSM ticket
// carrying the version field but NO labels field.
func TestTriageBridge_VersionOnly_LabelsFieldOmitted(t *testing.T) {
	srv, rec := newJiraRecordingServer()
	defer srv.Close()

	s := newTriageStoreSync(t, srv.URL, true, "triage-agent", "fsm-triage")
	event := marshalSSEBeadPayload(BeadEvent{
		ID: "bd-version-only", Type: "task", Title: "[FSM-703] x", Status: "in_progress", Assignee: "triage-agent",
		Labels: []string{"source:jira", "jira:FSM-703"},
		Fields: map[string]string{"jira_key": "FSM-703", "recommended_affects_version": "2026.8.0"},
	})
	s.handleUpdated(context.Background(), event)

	creates := outboxCreates(rec)
	if len(creates) != 1 {
		t.Fatalf("version-only: expected exactly 1 GBFSM ticket, got %d", len(creates))
	}
	if got, _ := creates[0][versionTestField].(string); got != "2026.8.0" {
		t.Errorf("version-only: version field = %q, want 2026.8.0", got)
	}
	if _, has := creates[0][labelsTestField]; has {
		t.Error("version-only: labels field must be OMITTED (no classification labels)")
	}
}

// TestTriageBridge_ScopeGuard_NoWriteToOtherTicket proves the scope guard applies to
// the GBFSM path too: for a triage bead bound to FSM-800, the emitted ticket's
// fsm_target_key must be FSM-800 (the binding) and NO write may reference any other
// ticket. The target key is derived from the in-scope binding, so it can only ever be
// the bead's own ticket.
func TestTriageBridge_ScopeGuard_NoWriteToOtherTicket(t *testing.T) {
	srv, rec := newJiraRecordingServer()
	defer srv.Close()

	s := newTriageStoreSync(t, srv.URL, true, "triage-agent", "fsm-triage")
	s.handleUpdated(context.Background(), triageClaimEvent("bd-scope-triage", "FSM-800"))

	targets := outboxTargetKeys(rec)
	if len(targets) == 0 {
		t.Fatal("expected the GBFSM ticket to be emitted for the bound ticket FSM-800")
	}
	for _, k := range targets {
		if k != "FSM-800" {
			t.Errorf("ticket target leaked to a non-bound ticket: fsm_target_key=%q, want FSM-800", k)
		}
	}
	// The create is the GBFSM endpoint; no direct write to the FSM ticket path.
	if n := len(rec.writesTo("/issue/FSM-800")); n != 0 {
		t.Errorf("no direct write may target the FSM ticket, got %d", n)
	}
}

// ── Unit tests for the new primitives ──

// TestSetAffectsVersions_WireFormat proves SetAffectsVersions issues an additive
// PUT to the bare issue path with an {"update":{"versions":[{"add":{"name":v}}]}}
// body (additive-only — never a replace/remove), for each non-blank name.
func TestSetAffectsVersions_WireFormat(t *testing.T) {
	srv, rec := newJiraRecordingServer()
	defer srv.Close()
	c := newTestJiraClient(srv.URL)

	if err := c.SetAffectsVersions(context.Background(), "FSM-1", []string{"2026.7.0", "2026.8.0"}); err != nil {
		t.Fatalf("SetAffectsVersions: %v", err)
	}
	puts := rec.writesTo("/rest/api/3/issue/FSM-1")
	if len(puts) != 1 {
		t.Fatalf("expected exactly 1 PUT to the bare issue path, got %d", len(puts))
	}
	ops := updateFieldOf(puts[0], "versions")
	if len(ops) != 2 {
		t.Fatalf("expected 2 version add ops, got %d (%v)", len(ops), puts[0].Body)
	}
	// Every op must be an additive {"add":{"name":...}} — no set/remove keys.
	for _, op := range ops {
		m, ok := op.(map[string]any)
		if !ok {
			t.Fatalf("version op is not an object: %v", op)
		}
		if _, hasAdd := m["add"]; !hasAdd {
			t.Errorf("version op missing additive \"add\" key: %v", m)
		}
		for k := range m {
			if k != "add" {
				t.Errorf("version op carries non-additive key %q (must be add-only): %v", k, m)
			}
		}
	}
	if v := lastVersionAdds(rec, "FSM-1"); len(v) != 2 || v[0] != "2026.7.0" || v[1] != "2026.8.0" {
		t.Errorf("expected adds [2026.7.0 2026.8.0], got %v", v)
	}
}

// TestSetAffectsVersions_EmptyIsNoOp proves an empty or all-blank name slice
// sends no request at all (a triage bead with no version must not PUT).
func TestSetAffectsVersions_EmptyIsNoOp(t *testing.T) {
	srv, rec := newJiraRecordingServer()
	defer srv.Close()
	c := newTestJiraClient(srv.URL)

	for _, names := range [][]string{nil, {}, {"", "   "}} {
		if err := c.SetAffectsVersions(context.Background(), "FSM-1", names); err != nil {
			t.Fatalf("SetAffectsVersions(%v): unexpected error %v", names, err)
		}
	}
	if n := len(rec.writesTo("/rest/api/3/issue/FSM-1")); n != 0 {
		t.Errorf("empty/blank version names must send no request, got %d PUTs", n)
	}
}

// TestSetAffectsVersions_SurfacesError proves an unknown version name (Jira 400)
// is surfaced, not swallowed — the bridge never auto-creates a version.
func TestSetAffectsVersions_SurfacesError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`{"errors":{"versions":"Version name is not valid"}}`))
	}))
	defer srv.Close()
	c := newTestJiraClient(srv.URL)

	err := c.SetAffectsVersions(context.Background(), "FSM-1", []string{"does-not-exist"})
	if err == nil {
		t.Fatal("expected an error for an unknown version name, got nil")
	}
}

// TestTriageClassification_ExtractsOnlyTriageLabels proves triageClassification
// returns exactly the triage labels (gardenio-triaged / sev-<N> / gxp-<X>) and the
// recommended_affects_version — never bookkeeping labels like source:jira /
// jira:<KEY> / arbitrary project labels.
func TestTriageClassification_ExtractsOnlyTriageLabels(t *testing.T) {
	bead := &BeadEvent{
		Labels: []string{
			"source:jira", "jira:FSM-1", "project:gardenio",
			"gardenio-triaged", "sev-3", "gxp-none", "unrelated-label",
		},
		Fields: map[string]string{"recommended_affects_version": "2026.9.0"},
	}
	labels, version := triageClassification(bead)
	want := map[string]bool{"gardenio-triaged": true, "sev-3": true, "gxp-none": true}
	if len(labels) != len(want) {
		t.Fatalf("expected exactly %d triage labels, got %v", len(want), labels)
	}
	for _, l := range labels {
		if !want[l] {
			t.Errorf("triageClassification leaked a non-triage label %q", l)
		}
	}
	if version != "2026.9.0" {
		t.Errorf("expected version 2026.9.0, got %q", version)
	}

	// A bead with no triage signal yields empty results (nothing to sync).
	empty := &BeadEvent{Labels: []string{"source:jira", "jira:FSM-1"}, Fields: map[string]string{}}
	if l, v := triageClassification(empty); len(l) != 0 || v != "" {
		t.Errorf("expected no triage classification, got labels=%v version=%q", l, v)
	}
	// Nil bead is safe.
	if l, v := triageClassification(nil); l != nil || v != "" {
		t.Errorf("nil bead must yield (nil, \"\"), got labels=%v version=%q", l, v)
	}
}

// TestIsTriageLabel pins the triage-label vocabulary. The CANONICAL skill labels
// (trigger prompt ka-0000002740; advice ka-0000003412) are the gardenio-triaged
// sentinel and the gardenio-triaged-sev-{0..3} / gardenio-triaged-gxp-{...} forms;
// the abbreviated sev-/gxp- prefixes are also accepted for back-compat. Every
// bookkeeping label must be rejected.
func TestIsTriageLabel(t *testing.T) {
	triage := []string{
		// Canonical skill vocabulary — the form fsm_label_write.sh stamps and the
		// trigger prompt directs the agent to write to the bead.
		"gardenio-triaged",
		"gardenio-triaged-sev-0", "gardenio-triaged-sev-3",
		"gardenio-triaged-gxp-none", "gardenio-triaged-gxp-high",
		// Abbreviated form — accepted for robustness/back-compat.
		"sev-0", "sev-3", "gxp-none", "gxp-high",
	}
	for _, l := range triage {
		if !isTriageLabel(l) {
			t.Errorf("%q should be a triage label", l)
		}
	}
	notTriage := []string{"source:jira", "jira:FSM-1", "jira-label:foo", "project:gardenio", "severity", "gxp", ""}
	for _, l := range notTriage {
		if isTriageLabel(l) {
			t.Errorf("%q must NOT be a triage label", l)
		}
	}
}

// TestTriageClassification_CanonicalSkillVocabulary proves the bridge collects
// the FULL canonical skill labels (gardenio-triaged-sev-N / gardenio-triaged-gxp-X)
// off a bead — not just the bare sentinel. This is the regression guard for the
// vocabulary mismatch where gardenio-triaged-sev-3 was silently dropped because it
// matched neither the bare "gardenio-triaged" nor the short "sev-" prefix.
func TestTriageClassification_CanonicalSkillVocabulary(t *testing.T) {
	bead := &BeadEvent{
		Labels: []string{
			"source:jira", "jira:FSM-408", "project:monorepo",
			"gardenio-triaged", "gardenio-triaged-sev-3", "gardenio-triaged-gxp-none",
		},
		Fields: map[string]string{"recommended_affects_version": "2.4.1"},
	}
	labels, version := triageClassification(bead)
	want := map[string]bool{
		"gardenio-triaged":          true,
		"gardenio-triaged-sev-3":    true,
		"gardenio-triaged-gxp-none": true,
	}
	if len(labels) != len(want) {
		t.Fatalf("expected exactly %d canonical triage labels, got %v", len(want), labels)
	}
	for _, l := range labels {
		if !want[l] {
			t.Errorf("triageClassification leaked or mismatched label %q", l)
		}
	}
	if version != "2.4.1" {
		t.Errorf("expected version 2.4.1, got %q", version)
	}
}
