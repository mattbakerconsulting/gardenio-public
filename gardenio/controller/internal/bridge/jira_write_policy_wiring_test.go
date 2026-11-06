package bridge

// End-to-end coverage for the config-driven write-policy SWAP (ka-0000007252,
// epic ka-0000003475): the four outbound Jira handlers (jira_sync,
// jira_squawk_forward, jira_mentions, jira_poller) now consult a single
// *WritePolicyStore instead of the legacy ProjectSyncGate + AllowlistGate pair.
//
// The parity oracle (jira_write_policy_test.go) already proves the STORE's
// verdict equals the legacy gates' verdict across the full matrix. These tests
// prove the SWAP is wired correctly: a handler constructed with WritePolicy set
// actually routes its decision through the store, reproducing the same
// project-gating (FSM toggle), field-allowlist, and fail-closed behavior the
// legacy gate wiring produced. They deliberately use their own fixtures and a
// dedicated file so they neither collide with the legacy-gate integration tests
// in jira_selectivesync_test.go nor with the strong-allowlist suite authored
// under gd-yOllH26pFY (which extends the allowlist/no-bypass files).
//
// storePolicy builds a WritePolicyStore from DefaultWritePolicy with the given
// FSM toggle + enforcement mode — exactly how cmd/jira-bridge wires it.

import (
	"context"
	"log/slog"
	"testing"

	"gardenio/controller/internal/beadsapi"
)

func storePolicy(outboxOn bool, mode AllowlistMode) *WritePolicyStore {
	wp := DefaultWritePolicy(outboxOn)
	wp.Mode = mode
	return NewWritePolicyStore(wp, slog.Default())
}

// TestWritePolicyWiring_SyncPath_OutboxToggle proves the sync-back path wired
// with a WritePolicyStore honors the OUTBOX toggle (epic ka-0000002067; 1:1 reshape
// ka-0000001491): an fsm-triage bead bound to an FSM ticket upserts the ONE GBFSM
// ticket only when the outbox toggle is on. Off ⇒ zero outbound writes (the FSM
// ticket is never touched, and GBFSM is gated off); on ⇒ a single GBFSM ticket
// carrying the labels + version as fields flows to GBFSM.
func TestWritePolicyWiring_SyncPath_OutboxToggle(t *testing.T) {
	event := func() []byte {
		return marshalSSEBeadPayload(BeadEvent{
			ID: "bd-wp-outbox", Type: "task", Title: "[FSM-100] Customer request",
			Status:   "in_progress",
			Assignee: "agent-triage-1",
			Labels:   []string{"source:jira", "jira:FSM-100", "gardenio-triaged", "sev-1"},
			Fields:   map[string]string{"jira_key": "FSM-100", "role": "fsm-triage", "recommended_affects_version": "2026.7.0"},
		})
	}

	t.Run("toggle off blocks all writes", func(t *testing.T) {
		srv, count := countingJiraServer(t)
		s := NewJiraSync(JiraSyncConfig{
			Jira:              newTestJiraClient(srv.URL),
			Logger:            slog.Default(),
			WritePolicy:       storePolicy(false, AllowlistModeEnforce),
			AnalysisIssueType: analysisTestType,
			LabelsFieldID:     labelsTestField,
			VersionFieldID:    versionTestField,
		})
		s.handleUpdated(context.Background(), event())
		if n := count(); n != 0 {
			t.Errorf("outbox off (store): expected 0 Jira writes, got %d", n)
		}
	})

	t.Run("toggle on upserts the one GBFSM ticket", func(t *testing.T) {
		srv, rec := newJiraRecordingServer()
		defer srv.Close()
		s := NewJiraSync(JiraSyncConfig{
			Jira:              newTestJiraClient(srv.URL),
			Logger:            slog.Default(),
			WritePolicy:       storePolicy(true, AllowlistModeEnforce),
			AnalysisIssueType: analysisTestType,
			LabelsFieldID:     labelsTestField,
			VersionFieldID:    versionTestField,
		})
		s.handleUpdated(context.Background(), event())
		// Exactly ONE GBFSM ticket carrying labels (gardenio-triaged, sev-1) + version.
		creates := outboxCreates(rec)
		if len(creates) != 1 {
			t.Fatalf("outbox on (store): expected exactly 1 GBFSM ticket, got %d", len(creates))
		}
		if got := fieldStrings(t, creates[0], labelsTestField); len(got) != 2 {
			t.Errorf("outbox on (store): expected 2 labels on the ticket, got %v", got)
		}
		if got, _ := creates[0][versionTestField].(string); got != "2026.7.0" {
			t.Errorf("outbox on (store): version field = %q, want 2026.7.0", got)
		}
		// No direct write to the FSM ticket, ever.
		if n := len(rec.writesTo("/issue/FSM-100")); n != 0 {
			t.Errorf("bot must make NO direct write to the FSM ticket, got %d", n)
		}
	})
}

// TestWritePolicyWiring_SyncPath_FieldAllowlist proves the sync path wired with
// a store enforces the per-role field allowlist: a restricted (internal-note-
// only) role on an allowed project posts the claim comment but has its
// transition + assignment DROPPED — the same verdict the legacy AllowlistGate
// produced (see TestJiraSync_GateDropsDisallowedTransition). Uses the
// "restricted" role (still internal-note-only after ka-0000003103 flipped
// fsm-triage to the labels+versions tier).
func TestWritePolicyWiring_SyncPath_FieldAllowlist(t *testing.T) {
	srv, rec := newJiraRecordingServer()
	defer srv.Close()

	s := NewJiraSync(JiraSyncConfig{
		Jira:         newTestJiraClient(srv.URL),
		Logger:       slog.Default(),
		BotAccountID: "bot-acct-123",
		WritePolicy:  storePolicy(false, AllowlistModeEnforce),
		Roles:        &roleResolverStub{roles: map[string]string{"agent-x": "restricted"}},
	})

	event := marshalSSEBeadPayload(BeadEvent{
		ID: "bd-wp-1", Type: "task", Title: "[PE-8001] thing", Status: "in_progress", Assignee: "agent-x",
		Labels: []string{"source:jira", "jira:PE-8001"},
		Fields: map[string]string{"jira_key": "PE-8001"},
	})
	s.handleUpdated(context.Background(), event)

	// restricted is internal-note-only: comment allowed, transition + assignee denied.
	if got := len(rec.writesTo("/comment")); got != 1 {
		t.Errorf("expected 1 internal-note comment write, got %d", got)
	}
	if got := rec.writesTo("/transitions"); len(got) != 0 {
		t.Errorf("expected transition DROPPED for internal-note-only role, got %d writes", len(got))
	}
	if got := rec.writesTo("/assignee"); len(got) != 0 {
		t.Errorf("expected assignment DROPPED for internal-note-only role, got %d writes", len(got))
	}
}

// TestWritePolicyWiring_SyncPath_UnknownRoleFailsClosed proves the store-wired
// sync path fails closed on an unresolvable role in enforce mode: EVERY governed
// write — including the comment — is dropped (matches
// TestJiraSync_GateUnknownRoleFailsClosed for the legacy gate).
func TestWritePolicyWiring_SyncPath_UnknownRoleFailsClosed(t *testing.T) {
	srv, rec := newJiraRecordingServer()
	defer srv.Close()

	s := NewJiraSync(JiraSyncConfig{
		Jira:        newTestJiraClient(srv.URL),
		Logger:      slog.Default(),
		WritePolicy: storePolicy(false, AllowlistModeEnforce),
		Roles:       &roleResolverStub{roles: map[string]string{}}, // agent not found → role "" → deny
	})

	event := marshalSSEBeadPayload(BeadEvent{
		ID: "bd-wp-2", Type: "task", Title: "[PE-8002] thing", Status: "closed", Assignee: "ghost",
		Labels: []string{"source:jira", "jira:PE-8002"},
		Fields: map[string]string{"jira_key": "PE-8002"},
	})
	s.handleClosed(context.Background(), event)

	if got := len(rec.writesTo("/comment")); got != 0 {
		t.Errorf("unknown role must fail closed (store): expected 0 comment writes, got %d", got)
	}
}

// TestWritePolicyWiring_SyncPath_ShadowAllowsButProjectStillHardGated proves the
// crucial mode subtlety preserved by the swap: in SHADOW mode a field-policy
// denial is logged-but-allowed, yet the PROJECT verdict stays hard-enforced
// (mode-independent). So a would-be-denied field write on an ALLOWED project
// flows (shadow), but ANY write to a toggle-off FSM project is still blocked.
func TestWritePolicyWiring_SyncPath_ShadowAllowsButProjectStillHardGated(t *testing.T) {
	// Shadow + unknown role on PE: field denial is shadowed ⇒ comment flows.
	t.Run("shadow allows out-of-policy field write on allowed project", func(t *testing.T) {
		srv, rec := newJiraRecordingServer()
		defer srv.Close()
		s := NewJiraSync(JiraSyncConfig{
			Jira:        newTestJiraClient(srv.URL),
			Logger:      slog.Default(),
			WritePolicy: storePolicy(false, AllowlistModeShadow),
			Roles:       &roleResolverStub{roles: map[string]string{}}, // role "" → would-deny
		})
		s.handleClosed(context.Background(), marshalSSEBeadPayload(BeadEvent{
			ID: "bd-wp-3", Type: "task", Title: "[PE-8003] x", Status: "closed", Assignee: "ghost",
			Labels: []string{"source:jira", "jira:PE-8003"}, Fields: map[string]string{"jira_key": "PE-8003"},
		}))
		if got := len(rec.writesTo("/comment")); got != 1 {
			t.Errorf("shadow mode should ALLOW the would-be-denied comment on PE, got %d writes", got)
		}
	})

	// Shadow + FSM toggle off: project verdict is mode-independent ⇒ still blocked.
	t.Run("shadow does NOT leak writes to a toggle-off project", func(t *testing.T) {
		srv, rec := newJiraRecordingServer()
		defer srv.Close()
		s := NewJiraSync(JiraSyncConfig{
			Jira:        newTestJiraClient(srv.URL),
			Logger:      slog.Default(),
			WritePolicy: storePolicy(false, AllowlistModeShadow),
			Roles:       &roleResolverStub{roles: map[string]string{"agent-x": "crew"}},
		})
		s.handleClosed(context.Background(), marshalSSEBeadPayload(BeadEvent{
			ID: "bd-wp-4", Type: "task", Title: "[FSM-8004] x", Status: "closed", Assignee: "agent-x",
			Labels: []string{"source:jira", "jira:FSM-8004"}, Fields: map[string]string{"jira_key": "FSM-8004"},
		}))
		if got := len(rec.writesTo("/comment")); got != 0 {
			t.Errorf("shadow mode must NOT leak writes to toggle-off FSM (project gate is hard), got %d writes", got)
		}
	})
}

// TestWritePolicyWiring_SquawkPath_FSMNeverWritten proves the squawk forwarder
// wired with a store never posts to an FSM ticket — FSM is permanently removed
// from the writable set (epic ka-0000002067), so an FSM-bound agent's squawk is
// dropped in BOTH outbox-toggle states (the squawk path forwards a comment to the
// bound ticket; it does not emit GBFSM intents, so the toggle is irrelevant to it).
// A PE-bound squawk is the positive control: it posts regardless of the toggle.
func TestWritePolicyWiring_SquawkPath_FSMNeverWritten(t *testing.T) {
	build := func(boundKey string, outboxOn bool) (*JiraSquawkForwarder, *mockJiraSquawkJira) {
		daemon := &mockJiraSquawkDaemon{
			agentBeads: map[string]*beadsapi.BeadDetail{
				"test-agent": {
					ID:    "bd-test-agent",
					Title: "test-agent",
					// "restricted" is internal-note-only — the squawk forwarder posts
					// internal notes, so use a comment-capable role here.
					Fields: map[string]string{"jira_key": boundKey, "role": "restricted"},
				},
			},
		}
		jira := &mockJiraSquawkJira{}
		f := NewJiraSquawkForwarder(JiraSquawkForwarderConfig{
			Daemon:      daemon,
			Jira:        jira,
			Logger:      jiraSquawkTestLogger(),
			WritePolicy: storePolicy(outboxOn, AllowlistModeEnforce),
		})
		return f, jira
	}
	event := marshalSSEBeadPayload(BeadEvent{
		ID:     "bd-wp-squawk",
		Type:   "message",
		Labels: []string{"squawk", "from:test-agent"},
		Fields: map[string]string{"source_agent": "test-agent", "text": "status note"},
	})

	for _, outboxOn := range []bool{false, true} {
		f, jira := build("FSM-100", outboxOn)
		f.handleClosed(context.Background(), event)
		if n := len(jira.getComments()); n != 0 {
			t.Errorf("FSM-bound squawk (outbox=%v): expected 0 comments (FSM never writable), got %d", outboxOn, n)
		}
	}

	// Positive control: a PE-bound squawk posts its internal note, unaffected by
	// the outbox toggle.
	f, jira := build("PE-100", false)
	f.handleClosed(context.Background(), event)
	if n := len(jira.getComments()); n != 1 {
		t.Errorf("PE-bound squawk: expected 1 internal-note comment, got %d", n)
	}
}

// TestWritePolicyWiring_SquawkPath_UnknownRoleFailsClosed proves the store-wired
// squawk forwarder drops a comment from an agent whose role is not in the
// allowlist, even on an allowed project (fail closed in enforce mode).
func TestWritePolicyWiring_SquawkPath_UnknownRoleFailsClosed(t *testing.T) {
	daemon := &mockJiraSquawkDaemon{
		agentBeads: map[string]*beadsapi.BeadDetail{
			"rogue-agent": {
				ID:     "bd-rogue",
				Title:  "rogue-agent",
				Fields: map[string]string{"jira_key": "PE-9001", "role": "totally-unknown-role"},
			},
		},
	}
	jira := &mockJiraSquawkJira{}
	f := NewJiraSquawkForwarder(JiraSquawkForwarderConfig{
		Daemon:      daemon,
		Jira:        jira,
		Logger:      jiraSquawkTestLogger(),
		WritePolicy: storePolicy(false, AllowlistModeEnforce),
	})
	f.handleClosed(context.Background(), marshalSSEBeadPayload(BeadEvent{
		ID: "bd-wp-squawk2", Type: "message", Labels: []string{"squawk", "from:rogue-agent"},
		Fields: map[string]string{"source_agent": "rogue-agent", "text": "hi"},
	}))
	if n := len(jira.getComments()); n != 0 {
		t.Errorf("unknown role must fail closed (store): expected 0 comments, got %d", n)
	}
}

// TestWritePolicyWiring_MentionPath_FSMNeverSpawns proves the mention handler
// wired with a store NEVER spawns an agent for an FSM ticket — FSM is permanently
// unwritable AND unspawnable (absent from the policy), so an @gardenio mention on
// an FSM ticket is ineligible in BOTH outbox-toggle states. The mention path uses
// the SPAWN-eligibility gate (AllowSpawn) and reaches no JiraClient write, per
// TestJiraMention_NoJiraWriteSurface. A PE mention is the positive control.
func TestWritePolicyWiring_MentionPath_FSMNeverSpawns(t *testing.T) {
	build := func(outboxOn bool) (*JiraMentionHandler, *mockJiraMentionDaemon) {
		daemon := &mockJiraMentionDaemon{
			createID:     "task-1",
			spawnID:      "agent-1",
			triggerBeads: jiraTestSpawnTriggerBeads("gardenio"),
		}
		h := NewJiraMentionHandler(JiraMentionHandlerConfig{
			Users:       allowJiraMentionUsers{},
			Daemon:      daemon,
			Project:     "gardenio",
			Logger:      jiraMentionTestLogger(),
			WritePolicy: storePolicy(outboxOn, AllowlistModeEnforce),
		})
		return h, daemon
	}

	for _, outboxOn := range []bool{false, true} {
		h, daemon := build(outboxOn)
		event := makeJiraMentionEvent("FSM-100", "Customer ticket", "@gardenio fix this", "User")
		h.HandleMention(context.Background(), event)
		if len(daemon.created) != 0 || len(daemon.spawned) != 0 {
			t.Errorf("FSM mention (outbox=%v): expected no bead/agent, got created=%d spawned=%d",
				outboxOn, len(daemon.created), len(daemon.spawned))
		}
	}

	// Positive control: a PE mention spawns regardless of the outbox toggle.
	h, daemon := build(false)
	h.HandleMention(context.Background(), makeJiraMentionEvent("PE-100", "Eng ticket", "@gardenio fix this", "User"))
	if len(daemon.created) != 1 || len(daemon.spawned) != 1 {
		t.Errorf("PE mention: expected 1 bead + 1 spawn, got created=%d spawned=%d",
			len(daemon.created), len(daemon.spawned))
	}
}

// TestWritePolicyWiring_MentionPath_GBFSMSpawnsRegardlessOfOutboxToggle is the
// regression lock for ka-0000001940: a @gardenio mention on a GBFSM ticket spawns
// an agent in BOTH outbox-toggle states, because the mention path now gates on
// spawn-eligibility (AllowSpawn), NOT write-eligibility (AllowProject). Before the
// fix, the shared write gate coupled GBFSM mention-spawns to JIRA_OUTBOX_ENABLED
// (off in prod), so GBFSM mentions never spawned. GBFSM still holds no ungated
// Jira WRITE surface — that coupling is what AllowProject preserves and
// TestJiraMention_NoJiraWriteSurface + the no-bypass scan enforce.
func TestWritePolicyWiring_MentionPath_GBFSMSpawnsRegardlessOfOutboxToggle(t *testing.T) {
	for _, outboxOn := range []bool{false, true} {
		daemon := &mockJiraMentionDaemon{
			createID:     "task-gbfsm",
			spawnID:      "agent-gbfsm",
			triggerBeads: jiraTestSpawnTriggerBeads("gardenio"),
		}
		h := NewJiraMentionHandler(JiraMentionHandlerConfig{
			Users:       allowJiraMentionUsers{},
			Daemon:      daemon,
			Project:     "gardenio",
			Logger:      jiraMentionTestLogger(),
			WritePolicy: storePolicy(outboxOn, AllowlistModeEnforce),
		})
		event := makeJiraMentionEvent("GBFSM-123", "Outbox-mirror ticket", "@gardenio look into this", "User")
		if handled := h.HandleMention(context.Background(), event); !handled {
			t.Errorf("GBFSM mention (outbox=%v): expected handled=true", outboxOn)
		}
		if len(daemon.created) != 1 || len(daemon.spawned) != 1 {
			t.Errorf("GBFSM mention (outbox=%v): expected 1 bead + 1 spawn (decoupled from outbox toggle), got created=%d spawned=%d",
				outboxOn, len(daemon.created), len(daemon.spawned))
		}
	}
}

// TestWritePolicyWiring_PollerPath_FSMNeverCommented proves the poller back-off
// comment NEVER reaches an FSM ticket — FSM is permanently unwritable, so the
// comment is suppressed in BOTH outbox-toggle states. A PE ticket is the positive
// control: the back-off comment posts regardless of the toggle.
func TestWritePolicyWiring_PollerPath_FSMNeverCommented(t *testing.T) {
	runFSM := func(outboxOn bool) int {
		p, _, count := pollerBackoffServer(t, "FSM-100")
		p.cfg.WritePolicy = storePolicy(outboxOn, AllowlistModeEnforce)
		p.checkAssigneeBatch(context.Background(), []string{"FSM-100"})
		return count()
	}
	for _, outboxOn := range []bool{false, true} {
		if n := runFSM(outboxOn); n != 0 {
			t.Errorf("FSM back-off (outbox=%v): expected 0 comments (FSM never writable), got %d", outboxOn, n)
		}
	}

	// Positive control: PE back-off comment posts.
	p, _, count := pollerBackoffServer(t, "PE-100")
	p.cfg.WritePolicy = storePolicy(false, AllowlistModeEnforce)
	p.checkAssigneeBatch(context.Background(), []string{"PE-100"})
	if n := count(); n != 1 {
		t.Errorf("PE back-off: expected 1 comment, got %d", n)
	}
}

// TestWritePolicyWiring_PollerPath_UnknownProjectBlocked proves the store-wired
// poller suppresses the back-off comment for an unknown (non-allowlisted)
// project — deny-by-default holds through the store.
func TestWritePolicyWiring_PollerPath_UnknownProjectBlocked(t *testing.T) {
	p, _, count := pollerBackoffServer(t, "ZZ-100")
	p.cfg.WritePolicy = storePolicy(false, AllowlistModeEnforce)
	p.checkAssigneeBatch(context.Background(), []string{"ZZ-100"})
	if n := count(); n != 0 {
		t.Errorf("unknown project (store): expected 0 back-off comments, got %d", n)
	}
}
