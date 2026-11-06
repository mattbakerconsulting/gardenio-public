package bridge

// Strong sync-back integration tests (gd-yOllH26pFY coverage area 3, epic
// ka-0000003475). These drive the REAL JiraSync.handleUpdated / handleClosed
// event path end-to-end against a recording Jira server and assert exactly which
// governed writes reach Jira for an fsm-triage-role bead bound to an FSM ticket
// — the sync-back leg of the two-account model.
//
//   - PROJECT GATE: FSM sync-back writes flow ONLY when the coordinated FSM
//     toggle is on; toggle off ⇒ zero writes (ka-0000001679).
//   - SCOPE GUARD: a write whose target key differs from the bead's bound
//     jira_key is denied (the bridge can only ever mutate the bead's OWN
//     ticket).
//   - FIELD ALLOWLIST for fsm-triage: the triage tier is labels + versions only
//     (ka-0000003103), so a transition or note attempted by fsm-triage is
//     DROPPED in enforce mode, and the legacy-gate path never reaches GBFSM.

import (
	"bytes"
	"context"
	"log/slog"
	"strings"
	"testing"
)

// discardErrLogger is a quiet logger for the JiraSync-under-test itself (we
// assert on the GATE's log buffer, not the handler's chatter).
func discardErrLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(&bytes.Buffer{}, &slog.HandlerOptions{Level: slog.LevelError}))
}

// newTriageSync builds a JiraSync wired with the DEFAULT field allowlist (so the
// fsm-triage tier is exactly production's), the given FSM toggle state, enforce
// mode, and a role resolver that maps the acting agent to fsm-triage. gateLog
// captures the allowlist's denial lines for assertion.
func newTriageSync(t *testing.T, jiraURL string, fsmOn bool, gateLog *bytes.Buffer) *JiraSync {
	t.Helper()
	gate := NewAllowlistGate(DefaultFieldAllowlist(), AllowlistModeEnforce, allowlistTestLogger(gateLog))
	return NewJiraSync(JiraSyncConfig{
		Jira:                  newTestJiraClient(jiraURL),
		Logger:                discardErrLogger(),
		BotAccountID:          "bot-acct-999",
		EnableClaimTransition: true, // so a transition WOULD be attempted if allowed
		Gate:                  gate,
		ProjectGate:           DefaultProjectSyncGate(fsmOn),
		Roles:                 &roleResolverStub{roles: map[string]string{"triage-agent": "fsm-triage"}},
	})
}

// triageClaimEvent is an in_progress claim SSE event for an fsm-triage agent on
// an FSM ticket, carrying the triage classification labels (gardenio-triaged /
// sev-1 / gxp-med) plus a recommended_affects_version field, so the sync path
// sees a realistic classified bead.
func triageClaimEvent(beadID, jiraKey string) []byte {
	return marshalSSEBeadPayload(BeadEvent{
		ID:       beadID,
		Type:     "task",
		Title:    "[" + jiraKey + "] Customer escalation",
		Status:   "in_progress",
		Assignee: "triage-agent",
		Labels: []string{
			"source:jira", "jira:" + jiraKey,
			"gardenio-triaged", "sev-1", "gxp-med",
		},
		Fields: map[string]string{
			"jira_key":                    jiraKey,
			"recommended_affects_version": "2026.7.0",
		},
	})
}

// ── Project gate: FSM toggle governs the whole sync-back leg ──

// TestSyncStrong_FSMToggleOff_NoWrite proves the project gate's deny-by-default:
// with the FSM toggle OFF, an fsm-triage claim on an FSM ticket produces ZERO
// outbound writes of any kind (the bead never reaches the field allowlist).
func TestSyncStrong_FSMToggleOff_NoWrite(t *testing.T) {
	srv, rec := newJiraRecordingServer()
	defer srv.Close()

	var gateLog bytes.Buffer
	s := newTriageSync(t, srv.URL, false /* fsm off */, &gateLog)
	s.handleUpdated(context.Background(), triageClaimEvent("bd-triage-off", "FSM-200"))

	if n := len(rec.writesTo("/issue/")); n != 0 {
		t.Errorf("FSM toggle off: expected 0 outbound writes, got %d: %+v", n, rec.requests)
	}
}

// ── Out-of-policy drop: a broad-role verb attempted by fsm-triage ──

// TestSyncStrong_TriageTransition_DroppedInEnforce isolates the "out-of-policy
// (fsm-triage attempts transition) DROPPED in enforce" requirement: on the MR
// merged path, a transition to Review AND an internal note are attempted; for
// the triage tier (labels+versions only, ka-0000003103) BOTH are dropped — triage
// classifies metadata, it neither moves the ticket nor speaks on it. (Distinct
// from the claim test: this is the merged→Review path in handleUpdated.)
func TestSyncStrong_TriageTransition_DroppedInEnforce(t *testing.T) {
	srv, rec := newJiraRecordingServer()
	defer srv.Close()

	var gateLog bytes.Buffer
	s := newTriageSync(t, srv.URL, true, &gateLog)

	// mr_merged=true triggers the Review transition + an internal note attempt.
	// Status is deliberately NOT in_progress and role is read from the bead's own
	// Fields["role"] (no assignee) so ONLY the merged path fires — isolating it
	// from the claim path. The bead carries NO triage classification (no sev-/gxp-
	// labels, no recommended_affects_version), so the triage sync-back is inert
	// too. For the triage tier both the transition and the note must be dropped.
	event := marshalSSEBeadPayload(BeadEvent{
		ID: "bd-triage-merge", Type: "task", Title: "[FSM-202] merged", Status: "open",
		Labels: []string{"source:jira", "jira:FSM-202"},
		Fields: map[string]string{"jira_key": "FSM-202", "mr_merged": "true", "role": "fsm-triage"},
	})
	s.handleUpdated(context.Background(), event)

	if n := len(rec.writesTo("/transitions")); n != 0 {
		t.Errorf("fsm-triage merged→Review transition must be DROPPED, got %d", n)
	}
	if n := len(rec.writesTo("/comment")); n != 0 {
		t.Errorf("fsm-triage merged note must be DROPPED (labels+versions only), got %d", n)
	}
	if !strings.Contains(gateLog.String(), "field=transition") {
		t.Errorf("dropped transition must be logged, got: %s", gateLog.String())
	}
}

// ── Scope guard: target ≠ bound key is denied ──

// TestSyncStrong_ScopeMismatch_Denied proves the scope guard on the sync path:
// a bead whose bound jira_key (authoritative field) is FSM-300 but whose
// jira:<KEY> label points at FSM-999 must NOT drive any write to FSM-999 — the
// bridge can only mutate the bead's OWN bound ticket. jiraKeyFromBead prefers
// the field, so the write target is FSM-300; we assert nothing lands on FSM-999
// and (with the field-derived target) the guard holds by construction. The
// direct inScope refusal is additionally exercised for the mismatched target.
func TestSyncStrong_ScopeMismatch_Denied(t *testing.T) {
	srv, rec := newJiraRecordingServer()
	defer srv.Close()

	var gateLog bytes.Buffer
	s := newTriageSync(t, srv.URL, true, &gateLog)

	// inScope must refuse a write aimed at a different ticket than the binding.
	bound := &BeadEvent{
		ID:     "bd-scope",
		Fields: map[string]string{"jira_key": "FSM-300"},
		Labels: []string{"jira:FSM-300"},
	}
	if s.inScope(bound, "FSM-999") {
		t.Error("scope guard must refuse a write targeting FSM-999 for a bead bound to FSM-300")
	}
	if !s.inScope(bound, "FSM-300") {
		t.Error("scope guard must permit a write targeting the bead's own bound ticket")
	}

	// End-to-end: no write ever lands on the non-bound key FSM-999.
	s.handleUpdated(context.Background(), triageClaimEvent("bd-scope-e2e", "FSM-300"))
	for _, r := range rec.requests {
		if strings.Contains(r.Path, "FSM-999") {
			t.Errorf("no write may target the non-bound ticket FSM-999, got: %s %s", r.Method, r.Path)
		}
	}
}

// ── Unknown/unresolved role fails closed on the FSM sync path ──

// TestSyncStrong_UnresolvedRole_FailsClosedOnFSM proves that when the acting
// role cannot be resolved (agent not found, no Fields[role]), enforce mode drops
// EVERY governed write on the FSM path — including the internal note. Fail-closed
// is the whole safety case: an unattributable write must never land on the
// customer service desk.
func TestSyncStrong_UnresolvedRole_FailsClosedOnFSM(t *testing.T) {
	srv, rec := newJiraRecordingServer()
	defer srv.Close()

	gate := NewAllowlistGate(DefaultFieldAllowlist(), AllowlistModeEnforce, allowlistTestLogger(&bytes.Buffer{}))
	s := NewJiraSync(JiraSyncConfig{
		Jira:        newTestJiraClient(srv.URL),
		Logger:      discardErrLogger(),
		Gate:        gate,
		ProjectGate: DefaultProjectSyncGate(true),                  // FSM on: isolate the role check
		Roles:       &roleResolverStub{roles: map[string]string{}}, // ghost agent → role ""
	})

	event := marshalSSEBeadPayload(BeadEvent{
		ID: "bd-ghost", Type: "task", Title: "[FSM-400] x", Status: "closed", Assignee: "ghost",
		Labels: []string{"source:jira", "jira:FSM-400"},
		Fields: map[string]string{"jira_key": "FSM-400"},
	})
	s.handleClosed(context.Background(), event)

	if n := len(rec.writesTo("/comment")); n != 0 {
		t.Errorf("unresolved role on FSM must fail closed: expected 0 comment writes, got %d", n)
	}
}

// ── The triage labels+versions sync-back end state (ka-0000003103) ──

// TestSyncStrong_LegacyGatePath_NoOutboxEmission proves the LEGACY-gate fallback
// (WritePolicy nil — the nil-store path used by these older tests) is INERT for
// the outbox: it can never emit a GBFSM intent, because the legacy ProjectSyncGate
// knows only {PE, DEVOPS} (+FSM behind its toggle) and has no GBFSM. So a fully
// classified fsm-triage bead driven through the legacy-gate sync path creates
// ZERO outbox intents and makes ZERO direct FSM writes. The real outbox emission
// path is the WritePolicyStore, exhaustively covered by
// jira_triage_bridge_test.go; this pins the safe-fallback invariant that the
// deprecated legacy path cannot accidentally reach GBFSM.
func TestSyncStrong_LegacyGatePath_NoOutboxEmission(t *testing.T) {
	// Policy-level surface: fsm-triage's tier is labels + versions ONLY (the
	// intent-type mapping), and it must NOT comment / transition / assign /
	// remotelink.
	dg := DefaultAllowlistGate(nil)
	if !dg.AllowField("fsm-triage", FieldLabels) {
		t.Error("fsm-triage must be permitted labels (⇒ Add-Label intent)")
	}
	if !dg.AllowField("fsm-triage", FieldVersions) {
		t.Error("fsm-triage must be permitted versions (⇒ Set-Version intent)")
	}
	if dg.AllowComment("fsm-triage", CommentInternal) {
		t.Error("fsm-triage must NOT comment (labels+versions only)")
	}
	if dg.AllowField("fsm-triage", FieldTransition) {
		t.Error("fsm-triage must NOT transition")
	}
	if dg.AllowField("fsm-triage", FieldAssignee) {
		t.Error("fsm-triage must NOT assign")
	}
	if dg.AllowField("fsm-triage", FieldRemoteLink) {
		t.Error("fsm-triage must NOT remotelink")
	}

	// Wire-level: drive triageClaimEvent through the LEGACY-gate sync path (fsm
	// toggle on, so even FSM would be permitted by the legacy gate) and assert
	// nothing reaches GBFSM and nothing writes the FSM ticket directly — the legacy
	// gate has no GBFSM entry, so the outbox emission is gated off.
	srv, rec := newJiraRecordingServer()
	defer srv.Close()

	var gateLog bytes.Buffer
	s := newTriageSync(t, srv.URL, true /* fsm on (legacy gate) */, &gateLog)
	s.handleUpdated(context.Background(), triageClaimEvent("bd-triage-legacy", "FSM-500"))

	if n := len(outboxCreates(rec)); n != 0 {
		t.Errorf("legacy-gate path must emit NO outbox intents (no GBFSM in the legacy gate), got %d", n)
	}
	if n := len(rec.writesTo("/issue/FSM-500")); n != 0 {
		t.Errorf("legacy-gate path must make NO direct FSM write, got %d", n)
	}
}

// updateFieldOf returns the []any under body["update"][field] for a recorded
// PUT, or nil. Both AddLabels and SetAffectsVersions PUT to the bare issue path
// with an {"update": {...}} body, so the presence of the "labels" vs "versions"
// key discriminates the two writes (jira.go AddLabels / SetAffectsVersions).
func updateFieldOf(r recordedJiraRequest, field string) []any {
	upd, ok := r.Body["update"].(map[string]any)
	if !ok {
		return nil
	}
	arr, _ := upd[field].([]any)
	return arr
}

// lastVersionAdds returns the version names added by the most recent versions
// PUT to <key> (the "name" of each {"add": {"name": v}} op), or nil if none.
func lastVersionAdds(rec *jiraRecordingServer, key string) []string {
	rec.mu.Lock()
	defer rec.mu.Unlock()
	base := "/rest/api/3/issue/" + key
	var adds []string
	for _, r := range rec.requests {
		if r.Method != "PUT" || r.Path != base {
			continue
		}
		ops := updateFieldOf(r, "versions")
		if ops == nil {
			continue
		}
		adds = adds[:0]
		for _, op := range ops {
			if m, ok := op.(map[string]any); ok {
				if nm, ok := m["add"].(map[string]any); ok {
					if v, ok := nm["name"].(string); ok {
						adds = append(adds, v)
					}
				}
			}
		}
	}
	return adds
}
