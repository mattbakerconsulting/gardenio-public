package bridge

// F5 guardrails spine (task gd-LY6qQ2vEhQ, plan ka-0000007401): the consolidated
// NEGATIVE proof that no outbound comment path can ever post to FSM, the
// customer-facing service desk.
//
// Two outbound paths turn agent text into a Jira comment:
//
//  1. squawk-forward (jira_squawk_forward.go, F1) — SSE-driven: a closed squawk
//     bead from a Jira-bound agent becomes an internal note on its bound ticket.
//  2. reply (jira_reply.go, F2) — POST /api/jira/reply behind `gb jira reply`:
//     an agent-initiated comment on its bound ticket.
//
// The production bridge (cmd/jira-bridge/main.go) threads ONE shared
// *WritePolicyStore into both. FSM is deliberately ABSENT from
// DefaultWritePolicy (epic ka-0000002067) so it resolves to "unknown project"
// and is denied in every toggle state, and both handlers consult the
// mode-independent AllowProject verdict FIRST — so shadow mode (which
// logs-but-allows role/field denials) can never soften the FSM block.
//
// This file consolidates the per-path FSM negatives into one spine:
//
//   - TestJiraReply_FSMHardBlocked / TestJiraReply_FSMBlockedEvenWithExplicitKey
//     — the reply half, moved here from jira_reply_test.go (landed with F2,
//     MR !5638), unchanged.
//   - TestJiraSquawk_FSMHardBlocked — the squawk half across the SAME policy
//     matrix (enforce/shadow × outbox on/off × unwired-nil-store), with a PE
//     positive control per configuration so the zero-writes assertion is never
//     vacuous.
//   - TestF5Spine_FSMDeniedForEveryRoleAndField — the per-role field-allowlist
//     assertion: every role the default policy knows (plus unknown/empty roles)
//     is denied every governed field write and every comment visibility on FSM.
//   - TestF5Spine_InternalByDefault — internal-by-default: both paths post
//     internal notes when unspecified, and NO role may post a public
//     (customer-facing) comment on any writable project.
//
// Related guards that stay in their own files: the write-policy swap wiring
// tests (jira_write_policy_wiring_test.go), the exhaustive decision table
// (jira_allowlist_strong_test.go), the source-level no-bypass scan
// (jira_field_gate_nobypass_test.go — its noBypassFiles set includes
// jira_reply.go so the reply write is structurally gate-guarded), and the
// no-issue-lifecycle guard (jira_no_issue_lifecycle_write_test.go).
//
// If any test here goes red, a change has re-opened an FSM write path — fix
// the wiring, not the test.

import (
	"context"
	"fmt"
	"net/http"
	"testing"

	"gardenio/controller/internal/beadsapi"
)

// f5PolicyMatrix returns the policy configurations the spine drives each
// handler through: both enforcement modes × both outbox-toggle states, plus the
// nil store (a mis-wired bridge). Every configuration must refuse FSM.
func f5PolicyMatrix() map[string]*WritePolicyStore {
	build := func(outboxOn bool, mode AllowlistMode) *WritePolicyStore {
		wp := DefaultWritePolicy(outboxOn)
		wp.Mode = mode
		return NewWritePolicyStore(wp, jiraSquawkTestLogger())
	}
	return map[string]*WritePolicyStore{
		"enforce/outbox-off": build(false, AllowlistModeEnforce),
		"enforce/outbox-on":  build(true, AllowlistModeEnforce),
		"shadow/outbox-off":  build(false, AllowlistModeShadow),
		"shadow/outbox-on":   build(true, AllowlistModeShadow),
		"nil-store":          nil,
	}
}

// ── Squawk-forward path: FSM can NEVER be squawked to ───────────────────────

// TestJiraSquawk_FSMHardBlocked asserts the F5 invariant for the squawk path:
// an FSM-bound agent's squawk is dropped in EVERY policy configuration. With a
// nil store the forwarder falls back to the legacy default project gate, whose
// FSM toggle is off — so even a mis-wired forwarder refuses FSM. A PE-bound
// squawk through the same forwarder is the positive control proving the
// dispatch harness is live (the FSM zero is not vacuous).
func TestJiraSquawk_FSMHardBlocked(t *testing.T) {
	event := func(id, agent string) []byte {
		return marshalSSEBeadPayload(BeadEvent{
			ID:     id,
			Type:   "message",
			Labels: []string{"squawk", "from:" + agent},
			Fields: map[string]string{"source_agent": agent, "text": "status update"},
		})
	}

	for name, policy := range f5PolicyMatrix() {
		t.Run(name, func(t *testing.T) {
			daemon := &mockJiraSquawkDaemon{
				agentBeads: map[string]*beadsapi.BeadDetail{
					"fsm-crew": {ID: "bd-fsm-crew", Title: "fsm-crew",
						Fields: map[string]string{"jira_key": "FSM-100", "role": "crew"}},
					"pe-crew": {ID: "bd-pe-crew", Title: "pe-crew",
						Fields: map[string]string{"jira_key": "PE-1", "role": "crew"}},
				},
			}
			jira := &mockJiraSquawkJira{}
			f := NewJiraSquawkForwarder(JiraSquawkForwarderConfig{
				Daemon:      daemon,
				Jira:        jira,
				Logger:      jiraSquawkTestLogger(),
				WritePolicy: policy,
			})

			f.handleClosed(context.Background(), event("bd-f5-fsm-"+name, "fsm-crew"))
			if got := len(jira.getComments()); got != 0 {
				t.Fatalf("SECURITY REGRESSION: %d squawk comment(s) reached FSM under %s; "+
					"the bot must never write FSM", got, name)
			}

			// Positive control: the same forwarder posts a PE squawk, so the FSM
			// zero above proves a refusal, not a dead harness.
			f.handleClosed(context.Background(), event("bd-f5-pe-"+name, "pe-crew"))
			comments := jira.getComments()
			if len(comments) != 1 || comments[0].Key != "PE-1" {
				t.Fatalf("positive control: expected exactly 1 comment on PE-1, got %+v", comments)
			}
			if comments[0].Vis != CommentInternal {
				t.Errorf("squawk visibility = %q, want internal (a squawk is an internal note)", comments[0].Vis)
			}
		})
	}
}

// ── Reply path: FSM can NEVER be replied to ─────────────────────────────────
// (Moved from jira_reply_test.go, where they landed with F2 / MR !5638, so the
// spine holds both halves. The reply helpers still live in jira_reply_test.go.)

// TestJiraReply_FSMHardBlocked asserts the F5 invariant for the reply path:
// an FSM-bound agent's reply is refused in EVERY policy configuration — the
// project verdict is mode-independent and FSM is not a writable project in any
// toggle state (epic ka-0000002067).
func TestJiraReply_FSMHardBlocked(t *testing.T) {
	for name, policy := range f5PolicyMatrix() {
		t.Run(name, func(t *testing.T) {
			daemon := replyAgentDaemon("fsm-crew", "FSM-100", "crew")
			jira := &mockJiraSquawkJira{}
			api := newJiraReplyAPI(daemon, jira, policy)

			for _, vis := range []string{"", "internal", "public"} {
				rec := postJiraReply(t, api, jiraReplyRequest{Agent: "fsm-crew", Text: "hi", Visibility: vis})
				if rec.Code != http.StatusForbidden {
					t.Errorf("visibility %q: status = %d, want 403 — FSM must be un-replyable", vis, rec.Code)
				}
			}
			if got := len(jira.getComments()); got != 0 {
				t.Fatalf("SECURITY REGRESSION: %d comment(s) reached FSM; the bot must never write FSM", got)
			}
		})
	}
}

// TestJiraReply_FSMBlockedEvenWithExplicitKey covers the request-forgery
// angle: an agent bound to a WRITABLE ticket naming an FSM key explicitly is
// rejected by bound-ticket scoping before the project gate is even consulted.
func TestJiraReply_FSMBlockedEvenWithExplicitKey(t *testing.T) {
	daemon := replyAgentDaemon("crew-1", "PE-42", "crew")
	jira := &mockJiraSquawkJira{}
	api := newJiraReplyAPI(daemon, jira, enforcedWritePolicy(true))

	rec := postJiraReply(t, api, jiraReplyRequest{Agent: "crew-1", JiraKey: "FSM-100", Text: "hi"})
	if rec.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403", rec.Code)
	}
	if len(jira.getComments()) != 0 {
		t.Fatal("SECURITY REGRESSION: comment reached FSM via explicit jira_key override")
	}
}

// ── Per-role field allowlist: FSM denied for EVERY role, field, visibility ──

// TestF5Spine_FSMDeniedForEveryRoleAndField is the per-role field-allowlist
// assertion of the spine, at the store level: no role the default policy knows
// (nor an unknown or empty role) may perform ANY governed field write or post a
// comment of ANY visibility on FSM, in either outbox-toggle state.
//
// The project verdict (AllowProject) is asserted in BOTH modes — it is
// mode-independent, which is the load-bearing fact that lets shadow mode exist
// at all. The field/comment verdicts are asserted in enforce mode only: in
// shadow the store logs-but-allows role/field denials BY CONTRACT
// (TestStrong_EnforceVsShadow pins that), which is exactly why both handlers
// consult AllowProject FIRST — proven end-to-end by the handler matrices above.
func TestF5Spine_FSMDeniedForEveryRoleAndField(t *testing.T) {
	roles := append(DefaultAllowlistGate(nil).Roles(), "totally-unknown-role", "")
	visibilities := []CommentVisibility{CommentInternal, CommentPublic, CommentVisibility("weird")}

	for _, outboxOn := range []bool{false, true} {
		// FSM must be structurally ABSENT from the default policy — not merely
		// non-writable — so it can never be re-enabled by a toggle flip.
		if _, ok := DefaultWritePolicy(outboxOn).Projects["FSM"]; ok {
			t.Fatalf("outbox=%v: DefaultWritePolicy carries an FSM project entry — "+
				"FSM must be absent so it resolves to unknown-project (epic ka-0000002067)", outboxOn)
		}

		for _, mode := range []AllowlistMode{AllowlistModeEnforce, AllowlistModeShadow} {
			name := fmt.Sprintf("outbox=%v/%s", outboxOn, mode)
			wp := DefaultWritePolicy(outboxOn)
			wp.Mode = mode
			store := NewWritePolicyStore(wp, jiraSquawkTestLogger())

			if store.AllowProject("FSM-100") {
				t.Errorf("%s: AllowProject(FSM-100) = true — the FSM project verdict "+
					"must be denied in every mode and toggle state", name)
			}
			if mode != AllowlistModeEnforce {
				continue
			}
			for _, role := range roles {
				for _, field := range allGovernedFields() {
					if store.AllowField("FSM-100", role, field) {
						t.Errorf("%s: AllowField(FSM-100, role=%q, field=%q) = true — "+
							"no role may hold any FSM field write", name, role, field)
					}
				}
				for _, vis := range visibilities {
					if store.AllowComment("FSM-100", role, vis) {
						t.Errorf("%s: AllowComment(FSM-100, role=%q, vis=%q) = true — "+
							"no role may comment on FSM at any visibility", name, role, vis)
					}
				}
			}
		}
	}
}

// ── Internal-by-default: no autonomous customer-facing reply ────────────────

// TestF5Spine_InternalByDefault pins the internal-by-default contract on both
// outbound paths plus the policy fact underneath it: (a) NO role in the default
// policy may post a public (customer-facing) comment on ANY writable project,
// (b) the squawk forwarder always posts internal notes, and (c) a reply with no
// visibility specified defaults to internal. Public replies are opt-in per role
// via AllowPublicComment, which no shipped role grants.
func TestF5Spine_InternalByDefault(t *testing.T) {
	// (a) Policy level: public comments denied for every known role on every
	// writable project (outbox on, so GBFSM is writable and covered too).
	store := enforcedWritePolicy(true)
	for _, key := range []string{"PE-1", "DEVOPS-2", "GBFSM-3"} {
		for _, role := range DefaultAllowlistGate(nil).Roles() {
			if store.AllowComment(key, role, CommentPublic) {
				t.Errorf("AllowComment(%s, role=%q, public) = true — no shipped role "+
					"may post a customer-facing comment", key, role)
			}
		}
	}

	// (b) Squawk path: the forwarded note is internal, unconditionally.
	daemon := &mockJiraSquawkDaemon{
		agentBeads: map[string]*beadsapi.BeadDetail{
			"pe-crew": {ID: "bd-pe-crew", Title: "pe-crew",
				Fields: map[string]string{"jira_key": "PE-7", "role": "crew"}},
		},
	}
	jira := &mockJiraSquawkJira{}
	f := NewJiraSquawkForwarder(JiraSquawkForwarderConfig{
		Daemon:      daemon,
		Jira:        jira,
		Logger:      jiraSquawkTestLogger(),
		WritePolicy: enforcedWritePolicy(false),
	})
	f.handleClosed(context.Background(), marshalSSEBeadPayload(BeadEvent{
		ID: "bd-f5-internal", Type: "message", Labels: []string{"squawk", "from:pe-crew"},
		Fields: map[string]string{"source_agent": "pe-crew", "text": "done"},
	}))
	comments := jira.getComments()
	if len(comments) != 1 || comments[0].Vis != CommentInternal {
		t.Fatalf("squawk must post exactly 1 INTERNAL note, got %+v", comments)
	}

	// (c) Reply path: omitted visibility defaults to internal.
	replyJira := &mockJiraSquawkJira{}
	api := newJiraReplyAPI(replyAgentDaemon("crew-1", "PE-42", "crew"), replyJira, enforcedWritePolicy(false))
	rec := postJiraReply(t, api, jiraReplyRequest{Agent: "crew-1", Text: "done"})
	if rec.Code != http.StatusOK {
		t.Fatalf("reply status = %d, want 200 (body: %s)", rec.Code, rec.Body.String())
	}
	replies := replyJira.getComments()
	if len(replies) != 1 || replies[0].Vis != CommentInternal {
		t.Fatalf("reply with no visibility must default to INTERNAL, got %+v", replies)
	}
}
