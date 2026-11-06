package bridge

import (
	"fmt"
	"strings"
	"testing"
)

// matrixKeys are Jira issue keys spanning the edge universe a future refactor
// could break silently. Parity between the unified store and the legacy oracle
// is STRUCTURAL — both funnel every key through the same strings.TrimSpace +
// jiraProjectKey extractor and the same project sets — so each form below must
// yield an identical verdict on both sides. A one-sided change (e.g. making
// project matching case-insensitive, trimming differently, or altering the
// hyphen-parse rule in only the store) would diverge here and fail the test.
var matrixKeys = []string{
	// Canonical: allowed software project (PE), second allowed project (DEVOPS),
	// the toggle-gated GBFSM outbox, the never-writable FSM service desk, an unknown
	// project (ZZ), and degenerate no-hyphen / empty inputs.
	"PE-1", "DEVOPS-2", "GBFSM-3", "FSM-3", "ZZ-9", "BARE", "",
	// Case variants — project extraction is case-SENSITIVE, so these
	// lowercase/mixed-case forms resolve to a project NOT in the allow set and
	// must be denied.
	"pe-1", "Gbfsm-3", "gbfsm", "fsm",
	// Whitespace forms — both sides TrimSpace before extraction, so a leading /
	// trailing / tab-prefixed key must resolve to its trimmed project.
	" PE-1", "PE-1 ", "\tGBFSM-3", "  DEVOPS-2  ",
	// Malformed hyphen/number keys — jiraProjectKey's "first hyphen at index > 0"
	// rule governs a leading hyphen (whole string), a double or trailing hyphen
	// (split at first), and a numeric prefix (unknown project).
	"-PE", "PE--1", "PE-", "123-4", "GBFSMX-1",
	// Prefix-substring collisions — a project key that is a prefix or superstring
	// of an allowed one (PEP⊃PE, GBFSMA⊃GBFSM, DEVOPSX⊃DEVOPS) must NOT be treated as
	// that allowed project. Guards against a future prefix/HasPrefix-based match.
	"PEP-1", "GBFSMA-1", "DEVOPSX-2",
}

// matrixFields is every known governed write field plus an unknown one, so the
// fail-closed-on-unknown-field path is exercised.
var matrixFields = []JiraWriteField{
	FieldComment, FieldTransition, FieldLabels, FieldAssignee, FieldRemoteLink,
	FieldVersions, JiraWriteField("bogus-field"),
}

var matrixVisibilities = []CommentVisibility{
	CommentInternal, CommentPublic, CommentVisibility("weird"),
}

// matrixRoles is the union of every role the legacy allowlist knows plus
// unknown/degenerate roles, so unknown-role denial is covered too. Roles are
// matched case-SENSITIVELY after TrimSpace on both sides, so the untrimmed
// " crew " must behave exactly like "crew", while the case-variant "CREW" and
// "FSM-TRIAGE" must be unknown (denied) — parity that a one-sided role
// normalization change would break.
func matrixRoles() []string {
	roles := DefaultAllowlistGate(nil).Roles()
	return append(roles,
		"totally-unknown-role", "",
		" crew ", "CREW", "FSM-TRIAGE",
	)
}

// expectWritable is the outbox-model project oracle: PE and DEVOPS are always
// writable, GBFSM is writable iff the outbox toggle is on, and EVERYTHING else —
// crucially FSM — is never writable. jiraProjectKey does the (case-sensitive,
// trimmed) extraction, identical to the store, so the oracle diverges from the
// store only if the store's project verdict is wrong.
func expectWritable(key string, outboxOn bool) bool {
	switch jiraProjectKey(strings.TrimSpace(key)) {
	case "PE", "DEVOPS":
		return true
	case outboxProjectKey: // GBFSM
		return outboxOn
	default:
		return false
	}
}

// TestDefaultWritePolicy_OutboxShape is the behavior-anchor for the outbox pivot
// (epic ka-0000002067): the unified store built from DefaultWritePolicy has the
// writable set {PE, DEVOPS, GBFSM(outbox)} with FSM permanently removed, and its
// per-role field/comment verdicts on a WRITABLE project still equal the
// unchanged AllowlistGate role tiers (so only the PROJECT axis changed — the
// field policy is preserved). Runs the full (toggle × key × role × field ×
// visibility) matrix.
func TestDefaultWritePolicy_OutboxShape(t *testing.T) {
	for _, outboxOn := range []bool{false, true} {
		t.Run(fmt.Sprintf("outboxEnabled=%v", outboxOn), func(t *testing.T) {
			// Role tiers are unchanged from the legacy allowlist and shared across
			// every writable project, so the tier verdicts are the oracle for the
			// role×field axis. The project axis uses expectWritable.
			tierAllow := NewAllowlistGate(DefaultFieldAllowlist(), AllowlistModeEnforce, nil)
			store := NewWritePolicyStore(DefaultWritePolicy(outboxOn), nil)

			// 1) Project verdict: exactly {PE, DEVOPS, GBFSM(outbox)}; FSM never.
			for _, key := range matrixKeys {
				if got, want := store.AllowProject(key), expectWritable(key, outboxOn); got != want {
					t.Errorf("AllowProject(%q) outbox=%v: store=%v want=%v", key, outboxOn, got, want)
				}
			}

			// 2) Field-write verdict = project writable AND the (unchanged) role tier.
			for _, key := range matrixKeys {
				for _, role := range matrixRoles() {
					for _, field := range matrixFields {
						want := expectWritable(key, outboxOn) && tierAllow.AllowField(role, field)
						if got := store.AllowField(key, role, field); got != want {
							t.Errorf("AllowField(%q,%q,%q) outbox=%v: store=%v want=%v",
								key, role, field, outboxOn, got, want)
						}
					}
				}
			}

			// 3) Comment verdict = project writable AND the role's comment tier.
			for _, key := range matrixKeys {
				for _, role := range matrixRoles() {
					for _, vis := range matrixVisibilities {
						want := expectWritable(key, outboxOn) && tierAllow.AllowComment(role, vis)
						if got := store.AllowComment(key, role, vis); got != want {
							t.Errorf("AllowComment(%q,%q,%q) outbox=%v: store=%v want=%v",
								key, role, vis, outboxOn, got, want)
						}
					}
				}
			}
		})
	}
}

// TestWritePolicyStore_FailsClosed asserts the deny-by-default invariants hold
// independently of the legacy comparison: a nil store, unknown project,
// toggle-off project, unknown role, unknown field, and an unpermitted public
// comment all deny in enforce mode.
func TestWritePolicyStore_FailsClosed(t *testing.T) {
	var nilStore *WritePolicyStore
	if nilStore.AllowField("PE-1", "crew", FieldComment) {
		t.Error("nil store must deny AllowField")
	}
	if nilStore.AllowProject("PE-1") {
		t.Error("nil store must deny AllowProject")
	}

	store := NewWritePolicyStore(DefaultWritePolicy(false), nil) // outbox toggle OFF

	cases := []struct {
		name  string
		check func() bool
	}{
		{"unknown project", func() bool { return store.AllowField("ZZ-1", "crew", FieldComment) }},
		{"fsm never writable", func() bool { return store.AllowField("FSM-1", "crew", FieldComment) }},
		{"gbfsm toggle off", func() bool { return store.AllowField("GBFSM-1", "crew", FieldComment) }},
		{"unknown role", func() bool { return store.AllowField("PE-1", "nope", FieldComment) }},
		{"unknown field", func() bool { return store.AllowField("PE-1", "crew", "bogus") }},
		{"empty role", func() bool { return store.AllowField("PE-1", "", FieldComment) }},
		{"triage tier transition", func() bool { return store.AllowField("PE-1", "fsm-triage", FieldTransition) }},
		{"triage tier comment denied", func() bool { return store.AllowComment("PE-1", "fsm-triage", CommentInternal) }},
		{"public comment not permitted", func() bool { return store.AllowComment("PE-1", "crew", CommentPublic) }},
		{"unknown visibility", func() bool { return store.AllowComment("PE-1", "crew", "weird") }},
	}
	for _, c := range cases {
		if c.check() {
			t.Errorf("%s: expected deny, got allow", c.name)
		}
	}

	// Positive controls: things that SHOULD pass in enforce mode.
	if !store.AllowField("PE-1", "crew", FieldTransition) {
		t.Error("crew should transition PE")
	}
	if !store.AllowComment("PE-1", "crew", CommentInternal) {
		t.Error("crew should post internal comment on PE")
	}
	if !store.AllowField("PE-1", "fsm-triage", FieldLabels) {
		t.Error("fsm-triage should write labels (triage tier)")
	}
	if !store.AllowField("PE-1", "fsm-triage", FieldVersions) {
		t.Error("fsm-triage should write versions (triage tier)")
	}
}

// TestWritePolicyStore_ToggleGatesGBFSM proves the SyncToggle gates the GBFSM outbox:
// GBFSM is writable iff the "outbox" toggle is on, PE is unaffected, and FSM is
// never writable in either toggle state (epic ka-0000002067).
func TestWritePolicyStore_ToggleGatesGBFSM(t *testing.T) {
	off := NewWritePolicyStore(DefaultWritePolicy(false), nil)
	on := NewWritePolicyStore(DefaultWritePolicy(true), nil)
	if off.AllowProject("GBFSM-1") {
		t.Error("GBFSM must be denied when outbox toggle off")
	}
	if !on.AllowProject("GBFSM-1") {
		t.Error("GBFSM must be allowed when outbox toggle on")
	}
	// PE is unaffected by the outbox toggle.
	if !off.AllowProject("PE-1") || !on.AllowProject("PE-1") {
		t.Error("PE must be writable regardless of outbox toggle")
	}
	// FSM is never writable, in any toggle state.
	if off.AllowProject("FSM-1") || on.AllowProject("FSM-1") {
		t.Error("FSM must NEVER be writable — the bot holds no FSM write permission")
	}
}

// TestWritePolicyStore_AllowSpawnDecoupledFromOutboxToggle proves the
// spawn-eligibility gate (AllowSpawn) is decoupled from the write toggle
// (ka-0000001940). AllowSpawn admits a Writable project regardless of its outbox
// SyncToggle, so a GBFSM @gardenio mention is spawn-eligible in BOTH toggle
// states — whereas AllowProject (write-eligibility) still gates GBFSM on the
// toggle. FSM and unknown projects remain denied for spawns (deny-by-default),
// and a nil store denies everything.
func TestWritePolicyStore_AllowSpawnDecoupledFromOutboxToggle(t *testing.T) {
	off := NewWritePolicyStore(DefaultWritePolicy(false), nil)
	on := NewWritePolicyStore(DefaultWritePolicy(true), nil)

	// GBFSM: spawn-eligible in BOTH states (the decoupling), even though it is
	// write-eligible only when the toggle is on.
	if !off.AllowSpawn("GBFSM-1") || !on.AllowSpawn("GBFSM-1") {
		t.Error("GBFSM must be spawn-eligible regardless of the outbox toggle (ka-0000001940)")
	}
	if off.AllowProject("GBFSM-1") {
		t.Error("write-eligibility invariant broken: GBFSM must stay write-gated when the toggle is off")
	}

	// PE/DEVOPS: always spawn-eligible (no toggle).
	for _, key := range []string{"PE-1", "DEVOPS-2"} {
		if !off.AllowSpawn(key) || !on.AllowSpawn(key) {
			t.Errorf("%s must be spawn-eligible in both toggle states", key)
		}
	}

	// FSM + unknown projects: never spawn-eligible (deny-by-default, fail closed).
	for _, key := range []string{"FSM-1", "ZZ-9", "BARE", ""} {
		if off.AllowSpawn(key) || on.AllowSpawn(key) {
			t.Errorf("%q must never be spawn-eligible (deny-by-default)", key)
		}
	}

	// A nil store denies spawns.
	var nilStore *WritePolicyStore
	if nilStore.AllowSpawn("PE-1") {
		t.Error("nil store must deny AllowSpawn")
	}
}

// TestAllowReplyProject_MirrorsAllowSpawn pins the parity the reply pre-gate
// relies on: AllowReplyProject and AllowSpawn share the SAME predicate
// (projectSpawnable — present and Writable, SyncToggle ignored). They are named
// separately because a reply and a spawn are distinct operations that happen to
// share an eligibility rule today; this parity is what makes the reply path
// toggle-independent exactly like the spawn path (ka-0000007628). If a future
// change diverges one from the other, this test flags it so the divergence is a
// deliberate call, not an accident.
func TestAllowReplyProject_MirrorsAllowSpawn(t *testing.T) {
	for _, outboxOn := range []bool{false, true} {
		store := NewWritePolicyStore(DefaultWritePolicy(outboxOn), nil)
		for _, key := range matrixKeys {
			if got, want := store.AllowReplyProject(key), store.AllowSpawn(key); got != want {
				t.Errorf("AllowReplyProject(%q)=%v but AllowSpawn(%q)=%v (outbox=%v): the reply "+
					"pre-gate must share the spawn predicate", key, got, key, want, outboxOn)
			}
		}
	}
	// A nil store denies reply-eligibility (fail closed), matching AllowSpawn.
	var nilStore *WritePolicyStore
	if nilStore.AllowReplyProject("PE-1") {
		t.Error("nil store must deny AllowReplyProject")
	}
}

// TestAllowReplyComment_DecoupledFromOutboxToggle is the store-level proof of the
// fix (ka-0000007628): the reply-comment verdict relaxes ONLY the project axis to
// the spawn predicate, so GBFSM internal replies are permitted in BOTH toggle
// states, while role gating and the public-comment ceiling are untouched. The
// oracle is: AllowReplyComment ignores the toggle, so its verdict for ANY toggle
// state equals AllowComment computed with the toggle ON (the one state where the
// write and reply project verdicts coincide). Runs the full key×role×visibility
// matrix so no combination can regress silently.
func TestAllowReplyComment_DecoupledFromOutboxToggle(t *testing.T) {
	// Oracle: write verdict with the outbox toggle ON — the toggle-independent
	// baseline the reply verdict must equal in every toggle state.
	oracle := NewWritePolicyStore(DefaultWritePolicy(true), nil)

	for _, outboxOn := range []bool{false, true} {
		store := NewWritePolicyStore(DefaultWritePolicy(outboxOn), nil)
		for _, key := range matrixKeys {
			for _, role := range matrixRoles() {
				for _, vis := range matrixVisibilities {
					got := store.AllowReplyComment(key, role, vis)
					want := oracle.AllowComment(key, role, vis)
					if got != want {
						t.Errorf("AllowReplyComment(%q,%q,%q) outbox=%v = %v, want %v "+
							"(reply must ignore the toggle: verdict = write-with-toggle-on)",
							key, role, vis, outboxOn, got, want)
					}
				}
			}
		}
	}

	// Spot-check the headline behaviors the matrix subsumes, stated plainly:
	off := NewWritePolicyStore(DefaultWritePolicy(false), nil)
	// GBFSM internal reply for the mention role: ALLOWED with the toggle off (the bug).
	if !off.AllowReplyComment("GBFSM-2", "jira-mention", CommentInternal) {
		t.Error("GBFSM internal reply must be allowed with the outbox toggle off (ka-0000007628)")
	}
	// GBFSM write (non-reply) for the same role: still DENIED with the toggle off —
	// the fix does not arm the outbox; it only frees the internal reply.
	if off.AllowComment("GBFSM-2", "jira-mention", CommentInternal) {
		t.Error("the write-comment verdict on GBFSM must stay toggle-gated (only reply is decoupled)")
	}
	// FSM: never reply-eligible, in any toggle state (absent from the policy).
	for _, s := range []*WritePolicyStore{off, oracle} {
		if s.AllowReplyComment("FSM-100", "jira-mention", CommentInternal) {
			t.Error("FSM must NEVER be reply-eligible — the bot holds no FSM write permission")
		}
	}
	// Public reply stays denied for GBFSM (the AllowPublicComment ceiling).
	if off.AllowReplyComment("GBFSM-2", "jira-mention", CommentPublic) {
		t.Error("a public GBFSM reply must stay denied (public-comment ceiling preserved)")
	}
	// A nil store denies reply-comments (fail closed).
	var nilStore *WritePolicyStore
	if nilStore.AllowReplyComment("PE-1", "crew", CommentInternal) {
		t.Error("nil store must deny AllowReplyComment")
	}
}

// TestWithSpawnProjects_SeparatesSpawnFromWriteAxis is the core proof for the
// mention-spawn widening (ka-0000005656): a project folded in via
// WithSpawnProjects is SPAWN-eligible (and, by the shared predicate,
// REPLY-eligible) but NOT outbound-Writable. This is the safety property the
// re-scope decision (ka-0000007763, option A) turns on — widening the spawn
// surface must not silently open the poller/sync-back/squawk write paths, which
// key on projectWritable (Writable alone) rather than projectSpawnable.
func TestWithSpawnProjects_SeparatesSpawnFromWriteAxis(t *testing.T) {
	tiers := map[string]RolePolicy(DefaultFieldAllowlist())
	policy := DefaultWritePolicy(false).WithSpawnProjects([]string{"FSM", "DOSE"}, tiers)
	store := NewWritePolicyStore(policy, nil)

	// The newly spawn-only projects: spawn + reply eligible, NOT write eligible.
	for _, key := range []string{"FSM-100", "DOSE-7"} {
		if !store.AllowSpawn(key) {
			t.Errorf("%s must be spawn-eligible after WithSpawnProjects", key)
		}
		if !store.AllowReplyProject(key) {
			t.Errorf("%s must be reply-eligible (shares the spawn predicate)", key)
		}
		if store.AllowProject(key) {
			t.Errorf("%s must NOT be outbound-writable — spawn axis must not open the write axis", key)
		}
		// A broad role may leave an internal note there (the reply path the
		// spawned agent uses); it may never post a public reply, and it never
		// gains outbound-write eligibility above.
		if !store.AllowReplyComment(key, "crew", CommentInternal) {
			t.Errorf("%s internal reply by crew must be permitted (Roles wired)", key)
		}
		if store.AllowReplyComment(key, "crew", CommentPublic) {
			t.Errorf("%s public reply must stay denied (no AllowPublicComment)", key)
		}
	}

	// A spawn-only project with a role that is unknown to the tiers is denied
	// its reply (fail closed on the role axis).
	if store.AllowReplyComment("FSM-100", "no-such-role", CommentInternal) {
		t.Error("unknown role on a spawn-only project must be denied (fail closed)")
	}

	// The canonical projects are untouched: still writable exactly as before.
	if !store.AllowProject("PE-1") || !store.AllowProject("DEVOPS-2") {
		t.Error("PE/DEVOPS must remain outbound-writable after the spawn overlay")
	}
	// GBFSM keeps its toggle-gated write behavior (toggle off ⇒ not writable).
	if store.AllowProject("GBFSM-3") {
		t.Error("GBFSM must remain write-gated on the outbox toggle after the overlay")
	}
	if !store.AllowSpawn("GBFSM-3") {
		t.Error("GBFSM must remain spawn-eligible after the overlay")
	}

	// A project in neither the default set nor the spawn set stays denied on
	// both axes (deny-by-default preserved).
	if store.AllowSpawn("ZZ-9") || store.AllowProject("ZZ-9") {
		t.Error("an unlisted project must stay denied on both spawn and write axes")
	}
}

// TestWithSpawnProjects_EmptyIsNoOp pins that the default render (no
// JIRA_SPAWN_PROJECTS) is byte-for-byte unchanged: an empty/blank overlay
// leaves the spawn AND write verdicts identical to DefaultWritePolicy across the
// full edge matrix, in both toggle states.
func TestWithSpawnProjects_EmptyIsNoOp(t *testing.T) {
	tiers := map[string]RolePolicy(DefaultFieldAllowlist())
	for _, outboxOn := range []bool{false, true} {
		base := NewWritePolicyStore(DefaultWritePolicy(outboxOn), nil)
		for _, extra := range [][]string{nil, {}, {"", "   "}} {
			overlaid := NewWritePolicyStore(
				DefaultWritePolicy(outboxOn).WithSpawnProjects(extra, tiers), nil)
			for _, key := range matrixKeys {
				if base.AllowSpawn(key) != overlaid.AllowSpawn(key) {
					t.Errorf("empty overlay changed AllowSpawn(%q) (outbox=%v)", key, outboxOn)
				}
				if base.AllowProject(key) != overlaid.AllowProject(key) {
					t.Errorf("empty overlay changed AllowProject(%q) (outbox=%v)", key, outboxOn)
				}
			}
		}
	}
}

// TestWithSpawnProjects_ExistingKeyNotDowngraded proves the overlay never
// downgrades an already-present writable project (PE/DEVOPS/GBFSM) to spawn-only
// even if it is (redundantly) listed in JIRA_SPAWN_PROJECTS: the existing
// policy wins, so PE stays outbound-writable.
func TestWithSpawnProjects_ExistingKeyNotDowngraded(t *testing.T) {
	tiers := map[string]RolePolicy(DefaultFieldAllowlist())
	store := NewWritePolicyStore(
		DefaultWritePolicy(false).WithSpawnProjects([]string{"PE", "DEVOPS", "GBFSM"}, tiers), nil)
	if !store.AllowProject("PE-1") {
		t.Error("PE must stay writable even when redundantly listed in the spawn set")
	}
	if !store.AllowProject("DEVOPS-2") {
		t.Error("DEVOPS must stay writable even when redundantly listed in the spawn set")
	}
	// GBFSM stays toggle-gated (off ⇒ not writable) — the overlay did not strip
	// its SyncToggle by re-adding it as a plain spawn-only entry.
	if store.AllowProject("GBFSM-3") {
		t.Error("GBFSM must stay toggle-gated, not silently made writable, by a redundant spawn listing")
	}
}
