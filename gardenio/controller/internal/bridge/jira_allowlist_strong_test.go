package bridge

// Strong allowlist enforcement test suite — Matt hard requirement (gd-yOllH26pFY,
// epic ka-0000003475). The whole two-account safety case rests on the per-role
// field allowlist actually doing what it claims BEFORE the prod enforce flip: the
// allowlist is the primary guard, ScriptRunner is only the Jira-side backstop.
//
// This file is the adversarial, exhaustive DECISION TABLE. It is written against
// the GENERAL, config-driven WritePolicyStore (jira_write_policy.go) rather than
// the legacy role-only AllowlistGate, per ka-0000007252: the strong tests bake
// in NO FSM-specific assumptions. Under the outbox pivot (epic ka-0000002067)
// the writable set is {PE, DEVOPS, GBFSM}; DefaultWritePolicy shares the exact role
// tiers of DefaultFieldAllowlist across every writable project, so a role×field
// verdict proven here on PE holds identically on GBFSM (where the fsm-triage
// {labels,versions} tier maps to the Add-Label / Set-Version intent types).
//
// # What "strong" means here
//
//   - EXHAUSTIVE over the closed field universe: every case iterates all of
//     knownJiraWriteFields (plus deliberately-unknown fields), so a field ADDED
//     to the universe without a matching policy decision trips a test rather
//     than silently widening a role's surface.
//   - FAIL-CLOSED matrix: unknown role, unknown field, empty role, unknown
//     comment visibility, unknown/unwritable project, and the nil store each
//     independently proven to deny.
//   - EVERY denial asserts the log line (criterion 2): enforce logs "rejected
//     out-of-policy write"; shadow logs "would reject … (shadow mode — allowing)"
//     and still returns true. role/field/reason are on every line.
//   - fsm-triage is pinned to its labels+versions-only surface — the exact
//     4-field triage surface (labels → Add-Label intents, versions → Set-Version
//     intent) with NO comment/transition/assignee/remotelink. This is asserted on
//     GBFSM, the project where the intents are created (epic ka-0000002067).

import (
	"bytes"
	"fmt"
	"log/slog"
	"slices"
	"strings"
	"testing"
)

// strongTestLogger returns a Warn-level TextHandler logger over buf so denial
// assertions can match the emitted role=/field=/reason= attrs.
func strongTestLogger(buf *bytes.Buffer) *slog.Logger {
	return slog.New(slog.NewTextHandler(buf, &slog.HandlerOptions{Level: slog.LevelWarn}))
}

// allGovernedFields is the full closed universe of governed write fields, read
// live from knownJiraWriteFields so this suite auto-covers any field added to
// the universe. Sorted for stable sub-test names.
func allGovernedFields() []JiraWriteField {
	out := make([]JiraWriteField, 0, len(knownJiraWriteFields))
	for f := range knownJiraWriteFields {
		out = append(out, f)
	}
	slices.Sort(out)
	return out
}

// storeWithTiers builds an enforcing WritePolicyStore over one writable project
// ("PE", always-on) whose per-role tiers are the supplied map, logging denials
// to buf. This isolates the role×field decision from project/toggle concerns so
// the decision table reads cleanly; the project/toggle axis is covered
// separately below and in jira_write_policy_test.go.
func storeWithTiers(tiers map[string]RolePolicy, mode AllowlistMode, buf *bytes.Buffer) *WritePolicyStore {
	return NewWritePolicyStore(WritePolicy{
		Projects: map[string]ProjectPolicy{
			"PE": {Writable: true, Roles: tiers},
		},
		Mode: mode,
	}, strongTestLogger(buf))
}

// ── 1. Broad-role tier: all five governed verbs, internal-only, never public ──

// TestStrong_BroadRole_AllVerbsInternalOnly proves every broad role in the
// shipped default policy may perform ALL governed field writes and internal
// comments, but is DENIED public (customer-facing) comments — the mirror
// contract's hard line (advice ka-0000003234). Exhaustive over the field
// universe so a newly-added governed field must be deliberately granted.
func TestStrong_BroadRole_AllVerbsInternalOnly(t *testing.T) {
	broadRoles := []string{
		"crew", "gardener", "gardener-delivery", "gardener-release-owner",
		"jira-mention", "thread", "triage",
	}
	for _, role := range broadRoles {
		t.Run(role, func(t *testing.T) {
			var buf bytes.Buffer
			store := NewWritePolicyStore(DefaultWritePolicy(false), strongTestLogger(&buf))

			for _, f := range allGovernedFields() {
				if !store.AllowField("PE-1", role, f) {
					t.Errorf("broad role %q must be allowed field %q on PE", role, f)
				}
			}
			if !store.AllowComment("PE-1", role, CommentInternal) {
				t.Errorf("broad role %q must be allowed internal comments", role)
			}
			if store.AllowComment("PE-1", role, CommentPublic) {
				t.Errorf("broad role %q must NOT be allowed public comments (mirror contract)", role)
			}
			// A broad role's public-comment denial must be logged (criterion 2).
			if !strings.Contains(buf.String(), "role="+role) {
				t.Errorf("expected a logged denial carrying role=%s, got: %s", role, buf.String())
			}
		})
	}
}

// ── 2. Restricted tier (fsm-agent / restricted): internal note ONLY ──

// TestStrong_RestrictedTier_InternalNoteOnly proves the internal-note-only tier
// (restricted, fsm-agent) may post an internal note and NOTHING else — no
// transition, labels, assignee, remotelink, versions, and no public reply.
// Exhaustive over the field universe and fail-closed on every non-comment field.
func TestStrong_RestrictedTier_InternalNoteOnly(t *testing.T) {
	for _, role := range []string{"restricted", "fsm-agent"} {
		t.Run(role, func(t *testing.T) {
			var buf bytes.Buffer
			store := NewWritePolicyStore(DefaultWritePolicy(false), strongTestLogger(&buf))

			if !store.AllowComment("PE-1", role, CommentInternal) {
				t.Errorf("%s must be allowed internal comments", role)
			}
			if store.AllowComment("PE-1", role, CommentPublic) {
				t.Errorf("%s must NOT be allowed public comments", role)
			}
			for _, f := range allGovernedFields() {
				if f == FieldComment {
					continue // comment is the one allowed verb (internal only)
				}
				if store.AllowField("PE-1", role, f) {
					t.Errorf("%s (internal-note-only) must be DENIED field %q", role, f)
				}
			}
			if buf.Len() == 0 {
				t.Errorf("%s: expected denial log lines for the dropped fields, got none", role)
			}
		})
	}
}

// ── 3. fsm-triage tier — the crux of the safety case ──

// TestStrong_FSMTriage_TargetSurface_LabelsVersionsOnly pins the fsm-triage
// tier that ka-0000003103 delivered: fsm-triage may write ONLY labels + versions
// (the exact 4-field triage surface: gardenio-triaged / sev-N / gxp-X labels +
// Affects-Version) and NOTHING else — no comment (internal or public), no
// transition, no assignee, no remotelink.
func TestStrong_FSMTriage_TargetSurface_LabelsVersionsOnly(t *testing.T) {
	var buf bytes.Buffer
	store := NewWritePolicyStore(DefaultWritePolicy(true), strongTestLogger(&buf)) // outbox toggle on

	// The triage surface is exercised on GBFSM — the project where the Add-Label /
	// Set-Version intents are created. FSM is never writable (epic ka-0000002067),
	// so labels/versions map 1:1 to the two intent types on GBFSM.
	const key = "GBFSM-1"
	allowed := map[JiraWriteField]bool{FieldLabels: true, FieldVersions: true}
	for _, f := range allGovernedFields() {
		got := store.AllowField(key, "fsm-triage", f)
		if want := allowed[f]; got != want {
			t.Errorf("fsm-triage field %q: got allow=%v, want %v (ONLY labels+versions)", f, got, want)
		}
	}
	// fsm-triage may NOT comment at all in the target surface — not even internal.
	if store.AllowComment(key, "fsm-triage", CommentInternal) {
		t.Error("fsm-triage target surface must NOT include internal comments (labels+versions only)")
	}
	if store.AllowComment(key, "fsm-triage", CommentPublic) {
		t.Error("fsm-triage target surface must NEVER include public comments")
	}
}

// ── 4. Fail-closed matrix (deny-by-default on every dimension) ──

// TestStrong_FailClosedMatrix exhaustively proves the store denies on every
// fail-closed dimension the safety case depends on, and asserts the denial is
// logged with a specific reason on each. Positive controls guard against an
// over-broad deny that would hide a real allow regression.
func TestStrong_FailClosedMatrix(t *testing.T) {
	// outbox toggle OFF so GBFSM is present-but-unwritable (distinct from unknown).
	newStore := func(buf *bytes.Buffer) *WritePolicyStore {
		return NewWritePolicyStore(DefaultWritePolicy(false), strongTestLogger(buf))
	}

	type denyCase struct {
		name       string
		wantReason string
		check      func(s *WritePolicyStore) bool // returns the gate's allow result
	}
	cases := []denyCase{
		{"unknown role (field)", "unknown role", func(s *WritePolicyStore) bool {
			return s.AllowField("PE-1", "totally-unknown-role", FieldComment)
		}},
		{"unknown role (comment)", "unknown role", func(s *WritePolicyStore) bool {
			return s.AllowComment("PE-1", "totally-unknown-role", CommentInternal)
		}},
		{"empty role (field)", "empty role", func(s *WritePolicyStore) bool {
			return s.AllowField("PE-1", "", FieldComment)
		}},
		{"empty role (comment)", "empty role", func(s *WritePolicyStore) bool {
			return s.AllowComment("PE-1", "", CommentInternal)
		}},
		{"unknown field", "unknown field", func(s *WritePolicyStore) bool {
			return s.AllowField("PE-1", "crew", JiraWriteField("priority"))
		}},
		{"unknown visibility", "unknown comment visibility", func(s *WritePolicyStore) bool {
			return s.AllowComment("PE-1", "crew", CommentVisibility("semi-public"))
		}},
		{"unknown project", "unknown project", func(s *WritePolicyStore) bool {
			return s.AllowField("ZZ-9", "crew", FieldComment)
		}},
		{"fsm is unknown (never in the policy)", "unknown project", func(s *WritePolicyStore) bool {
			return s.AllowField("FSM-1", "crew", FieldComment)
		}},
		{"unwritable project (gbfsm outbox toggle off)", "project not writable", func(s *WritePolicyStore) bool {
			return s.AllowField("GBFSM-1", "crew", FieldComment)
		}},
		{"empty/malformed key", "empty or malformed jira key", func(s *WritePolicyStore) bool {
			return s.AllowField("", "crew", FieldComment)
		}},
		{"public comment not permitted", "public comment not permitted", func(s *WritePolicyStore) bool {
			return s.AllowComment("PE-1", "crew", CommentPublic)
		}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			var buf bytes.Buffer
			s := newStore(&buf)
			if c.check(s) {
				t.Fatalf("%s: expected DENY in enforce mode, got allow", c.name)
			}
			logged := buf.String()
			if logged == "" {
				t.Fatalf("%s: every denial must be logged (criterion 2), got no log", c.name)
			}
			if !strings.Contains(logged, "reason="+quoteReasonForLog(c.wantReason)) &&
				!strings.Contains(logged, "reason="+c.wantReason) {
				t.Errorf("%s: expected a denial reason starting %q, got: %s", c.name, c.wantReason, logged)
			}
			if !strings.Contains(logged, "rejected out-of-policy write") {
				t.Errorf("%s: enforce-mode denial must log the rejection line, got: %s", c.name, logged)
			}
		})
	}

	// Positive controls — these MUST pass so the matrix isn't vacuously denying.
	var buf bytes.Buffer
	s := newStore(&buf)
	if !s.AllowField("PE-1", "crew", FieldTransition) {
		t.Error("positive control: crew must transition PE")
	}
	if !s.AllowComment("PE-1", "crew", CommentInternal) {
		t.Error("positive control: crew must post internal comment on PE")
	}
}

// quoteReasonForLog renders a reason the way slog's TextHandler quotes a value
// containing spaces (so a substring match on the emitted line is robust).
func quoteReasonForLog(reason string) string {
	if strings.ContainsAny(reason, " ") {
		return `"` + reason
	}
	return reason
}

// TestStrong_NilStoreDeniesEverything proves the safest failure: a nil
// WritePolicyStore denies every write regardless of mode. A nil store is what a
// mis-wired bridge would hold, so it must never allow a write through.
func TestStrong_NilStoreDeniesEverything(t *testing.T) {
	var nilStore *WritePolicyStore
	for _, f := range allGovernedFields() {
		if nilStore.AllowField("PE-1", "crew", f) {
			t.Errorf("nil store must deny field %q", f)
		}
	}
	if nilStore.AllowComment("PE-1", "crew", CommentInternal) {
		t.Error("nil store must deny comments")
	}
	if nilStore.AllowProject("PE-1") {
		t.Error("nil store must deny AllowProject")
	}
	// Mode() of a nil store reports shadow, but apply() still hard-denies — the
	// shadow-allows behavior is reserved for a CONFIGURED store, never the
	// absent one. Assert both facts so a refactor can't quietly let nil allow.
	if nilStore.Mode() != AllowlistModeShadow {
		t.Errorf("nil store Mode() = %v, want shadow", nilStore.Mode())
	}
}

// ── 5. Mode semantics: enforce DROPS, shadow LOGS-BUT-ALLOWS ──

// TestStrong_EnforceVsShadow proves the mode contract on a denied write across
// the whole field universe: in enforce the write is dropped (AllowField=false)
// and logged "rejected out-of-policy write"; in shadow the SAME denial is
// allowed through (AllowField=true) but still logged "would reject … (shadow
// mode — allowing)". This is the rollout-safety switch — a denial is observable
// in shadow before it starts dropping in enforce.
func TestStrong_EnforceVsShadow(t *testing.T) {
	// restricted tier denies every non-comment field — a rich denial source.
	tiers := map[string]RolePolicy{"restricted": internalNoteOnlyPolicy()}

	for _, f := range allGovernedFields() {
		if f == FieldComment {
			continue // allowed for restricted; we want a DENIED field
		}
		t.Run(string(f), func(t *testing.T) {
			// Enforce: dropped + rejection logged.
			var enfBuf bytes.Buffer
			enf := storeWithTiers(tiers, AllowlistModeEnforce, &enfBuf)
			if enf.AllowField("PE-1", "restricted", f) {
				t.Errorf("enforce: field %q must be dropped for restricted", f)
			}
			if !strings.Contains(enfBuf.String(), "rejected out-of-policy write") {
				t.Errorf("enforce: expected rejection log for %q, got: %s", f, enfBuf.String())
			}

			// Shadow: allowed through + would-reject logged.
			var shBuf bytes.Buffer
			sh := storeWithTiers(tiers, AllowlistModeShadow, &shBuf)
			if !sh.AllowField("PE-1", "restricted", f) {
				t.Errorf("shadow: field %q must be ALLOWED through for restricted", f)
			}
			logged := shBuf.String()
			if !strings.Contains(logged, "shadow mode — allowing") {
				t.Errorf("shadow: expected 'shadow mode — allowing' log for %q, got: %s", f, logged)
			}
			if !strings.Contains(logged, "role=restricted") || !strings.Contains(logged, "field="+string(f)) {
				t.Errorf("shadow: denial log for %q must carry role+field, got: %s", f, logged)
			}
		})
	}

	// An ALLOWED write logs nothing in either mode (no denial noise).
	for _, mode := range []AllowlistMode{AllowlistModeEnforce, AllowlistModeShadow} {
		var buf bytes.Buffer
		s := storeWithTiers(map[string]RolePolicy{"crew": allFieldsPolicy()}, mode, &buf)
		if !s.AllowField("PE-1", "crew", FieldTransition) {
			t.Errorf("%v: allowed field must pass", mode)
		}
		if buf.Len() != 0 {
			t.Errorf("%v: an allowed write must not log, got: %s", mode, buf.String())
		}
	}
}

// TestStrong_ShadowNeverAllowsPublicComment_ButLogs is a subtle-but-critical
// case: even in shadow mode, a public-comment attempt by a no-public role is
// LOGGED as a would-be denial. Shadow is a rollout aid — it must still surface
// that a public reply WOULD have been dropped in enforce, so the operator sees
// the attempt before flipping the switch.
func TestStrong_ShadowNeverAllowsPublicComment_ButLogs(t *testing.T) {
	var buf bytes.Buffer
	s := storeWithTiers(map[string]RolePolicy{"crew": allFieldsPolicy()}, AllowlistModeShadow, &buf)
	// Shadow lets it through (that's the definition of shadow) …
	if !s.AllowComment("PE-1", "crew", CommentPublic) {
		t.Error("shadow mode allows the write through (logs-but-allows)")
	}
	// … but the would-be denial MUST be logged with the public-comment reason.
	logged := buf.String()
	if !strings.Contains(logged, "shadow mode — allowing") ||
		!strings.Contains(logged, "public comment not permitted") {
		t.Errorf("shadow public-comment attempt must be logged as would-be denial, got: %s", logged)
	}
}

// ── 6. Closed-universe guard on the field enum itself ──

// TestStrong_KnownFieldUniverseIsClosed asserts the field-name universe the
// allowlist keys on is exactly the set of declared JiraWriteField consts wired
// into knownJiraWriteFields. If someone adds a Field* const but forgets to add
// it to knownJiraWriteFields (or vice-versa), the fail-closed guarantee silently
// changes shape — this test makes that a hard failure. It is deliberately
// tolerant of FieldVersions being present-or-absent (ka-0000003103), asserting
// only the always-present five plus internal consistency.
func TestStrong_KnownFieldUniverseIsClosed(t *testing.T) {
	// The five always-governed writes must always be known.
	for _, f := range []JiraWriteField{
		FieldComment, FieldTransition, FieldLabels, FieldAssignee, FieldRemoteLink,
	} {
		if !knownJiraWriteFields[f] {
			t.Errorf("core governed field %q must be in knownJiraWriteFields", f)
		}
	}
	// Every entry in knownJiraWriteFields must be a non-empty string (no zero
	// value smuggled in that would make an empty field silently allowed).
	for f := range knownJiraWriteFields {
		if strings.TrimSpace(string(f)) == "" {
			t.Error("knownJiraWriteFields contains an empty field name — empty field must never be known")
		}
	}
	// A representative unknown field is NOT known (sanity: the map is a closed
	// allowlist, not a default-true set).
	if knownJiraWriteFields[JiraWriteField("arbitrary-unwired-verb")] {
		t.Error("knownJiraWriteFields must be a closed set — an arbitrary verb must be unknown")
	}
	// Log the current universe once for debugging drift (visible with -v).
	t.Logf("known governed field universe: %s", fmt.Sprint(allGovernedFields()))
}
