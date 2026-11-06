package advice

import (
	"testing"

	"gardenio/controller/internal/beadsapi"
)

func lintBead(id string, labels []string) *beadsapi.BeadDetail {
	return &beadsapi.BeadDetail{
		ID:     id,
		Title:  "advice " + id,
		Type:   "advice",
		Status: "open",
		Labels: labels,
	}
}

// lintBeadTitled is lintBead with an explicit title, for checks that read the
// bead title (undeliverable-capability).
func lintBeadTitled(id, title string, labels []string) *beadsapi.BeadDetail {
	b := lintBead(id, labels)
	b.Title = title
	return b
}

func TestLintAdvice(t *testing.T) {
	beads := []*beadsapi.BeadDetail{
		lintBead("gd-ok-global", []string{"global"}),
		lintBead("gd-ok-role", []string{"g0:project:gardenio", "g0:role:crew"}),
		lintBead("gd-denied", []string{"project:gardenio"}),
		lintBead("gd-optin", []string{"project:gardenio", BroadcastOptInLabel}),
		lintBead("gd-stray-optin", []string{"g0:project:gardenio", "g0:role:crew", BroadcastOptInLabel}),
		lintBead("gd-jfz0lps3xs", []string{"needs-intake"}),
		// Intentionally unmatched on-demand doc — exempt from no-targeting.
		lintBead("gd-ondemand", []string{OnDemandLabel}),
		lintBead("gd-junk", []string{"global", "project:ka-0000005132"}),
		lintBead("gd-hippo", []string{"project:gardenio", "source:hippo-memory"}),
		lintBead("gd-fixture", []string{"project:gardenio", "fixture:e2e"}),
		// global + role: is self-contradictory — the role gate silently
		// narrows delivery and `global` is inert (ka-0000004354). Group-
		// prefixed role: labels must trip the check too.
		lintBead("gd-8zmzr7f0ci", []string{"global", "role:gardener", "role:crew"}),
		lintBead("gd-8zmzr7f0ci-grouped", []string{"global", "g0:role:thread"}),
	}
	closed := lintBead("gd-closed", []string{"project:gardenio"})
	closed.Status = "closed"
	beads = append(beads, closed)

	// nil liveRoles skips the dead-role-target check, so gd-ok-role (targeting
	// the "crew" builtin) yields nothing here — the check itself is exercised
	// by TestLintAdvice_DeadRoleTarget below.
	findings := LintAdvice(beads, nil, nil)

	byID := map[string][]LintFinding{}
	for _, f := range findings {
		byID[f.ID] = append(byID[f.ID], f)
	}

	// Clean beads, fixtures, and closed beads yield nothing.
	for _, id := range []string{"gd-ok-global", "gd-ok-role", "gd-ondemand", "gd-fixture", "gd-closed"} {
		if len(byID[id]) != 0 {
			t.Errorf("%s: unexpected findings %+v", id, byID[id])
		}
	}

	expect := map[string]struct {
		check    string
		severity string
	}{
		"gd-denied":             {LintDeniedBroadcast, LintError},
		"gd-optin":              {LintOptedInBroadcast, LintWarn},
		"gd-stray-optin":        {LintUselessOptIn, LintWarn},
		"gd-jfz0lps3xs":         {LintNoTargeting, LintError},
		"gd-junk":               {LintJunkProjectLabel, LintWarn},
		"gd-hippo":              {LintHippoMirror, LintWarn},
		"gd-8zmzr7f0ci":         {LintGlobalRoleClash, LintError},
		"gd-8zmzr7f0ci-grouped": {LintGlobalRoleClash, LintError},
	}
	for id, want := range expect {
		fs := byID[id]
		if len(fs) != 1 {
			t.Errorf("%s: got %d findings %+v, want exactly 1", id, len(fs), fs)
			continue
		}
		if fs[0].Check != want.check || fs[0].Severity != want.severity {
			t.Errorf("%s: got (%s, %s), want (%s, %s)", id, fs[0].Check, fs[0].Severity, want.check, want.severity)
		}
	}
}

// TestLintAdvice_DeadRoleTarget exercises the dead-role-target check: a role:
// target outside the live-role union warns, unless the bead is on-demand or the
// union is unknown (nil). The union here models the real one built by the CLI:
// "crew" is a builtin, "plan-gardener-live" a type:role bead / open-trigger role (ka-0000005093).
func TestLintAdvice_DeadRoleTarget(t *testing.T) {
	liveRoles := map[string]struct{}{
		"crew":               {},
		"plan-gardener-live": {},
	}

	tests := []struct {
		name      string
		labels    []string
		liveRoles map[string]struct{}
		wantDead  bool // expect exactly one dead-role-target WARN
	}{
		{
			name:      "builtin role target — no finding",
			labels:    []string{"g0:project:gardenio", "g0:role:crew"},
			liveRoles: liveRoles,
			wantDead:  false,
		},
		{
			name:      "live type:role / trigger role target — no finding",
			labels:    []string{"project:gardenio", "role:plan-gardener-live"},
			liveRoles: liveRoles,
			wantDead:  false,
		},
		{
			// Plural label against a singular live role: the delivery path
			// subscribes to both role:crew and role:crews, so lint must not
			// flag the plural form as dead. Regression guard for gd-LhS8M8Dlno.
			name:      "plural role label, singular live role — no finding",
			labels:    []string{"project:gardenio", "role:crews"},
			liveRoles: liveRoles,
			wantDead:  false,
		},
		{
			// Singular label against a plural live role — the mirror case,
			// normalized the same way.
			name:      "singular role label, plural live role — no finding",
			labels:    []string{"project:gardenio", "role:job"},
			liveRoles: map[string]struct{}{"jobs": {}},
			wantDead:  false,
		},
		{
			name:      "unknown role, no on-demand — dead-role-target WARN",
			labels:    []string{"project:gardenio", "role:gardener-intake"},
			liveRoles: liveRoles,
			wantDead:  true,
		},
		{
			// Multiple role: labels, one live one dead — the check breaks after
			// the first dead label, so exactly one WARN regardless of order.
			name:      "live + dead role labels — single WARN, not doubled",
			labels:    []string{"project:gardenio", "role:crew", "role:gardener-intake"},
			liveRoles: liveRoles,
			wantDead:  true,
		},
		{
			name:      "unknown role but on-demand — no finding",
			labels:    []string{"role:gardener-intake", OnDemandLabel},
			liveRoles: liveRoles,
			wantDead:  false,
		},
		{
			name:      "unknown role but liveRoles nil — check skipped",
			labels:    []string{"project:gardenio", "role:gardener-intake"},
			liveRoles: nil,
			wantDead:  false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			findings := LintAdvice([]*beadsapi.BeadDetail{lintBead("gd-x", tt.labels)}, tt.liveRoles, nil)
			var dead int
			for _, f := range findings {
				if f.Check == LintDeadRoleTarget {
					if f.Severity != LintWarn {
						t.Errorf("dead-role-target severity = %q, want %q", f.Severity, LintWarn)
					}
					dead++
				}
			}
			if tt.wantDead && dead != 1 {
				t.Errorf("got %d dead-role-target findings, want 1 (findings=%+v)", dead, findings)
			}
			if !tt.wantDead && dead != 0 {
				t.Errorf("got %d dead-role-target findings, want 0 (findings=%+v)", dead, findings)
			}
		})
	}
}

// TestLintAdvice_UndeliverableCapability exercises the undeliverable-capability
// check: a bead whose TITLE asserts a capability ("X CAN do Y") or de-escalates
// ("no escalation needed") while being undeliverable (on-demand or denied
// broadcast) is a self-defeating contradiction. Titles are the real ones from
// the bug (ka-0000003515) plus the legitimate on-demand pointer docs the check
// must NOT flag.
func TestLintAdvice_UndeliverableCapability(t *testing.T) {
	// The two real capability-correcting titles that carried scope:on-demand.
	const canTitle = "Gardenio-project agents CAN read pod/service logs cluster-wide (SA = gardenio-agent-cluster-admin); non-gardenio projects still cannot"
	const fetchTitle = "Query dev/uat OTEL observability (VictoriaTraces/Logs/Metrics on dev.acme.example) as a gardenio agent — creds already fetchable via tf-dev-admin-role"
	// Legitimate on-demand pointer docs that must stay clean.
	const escalationHowto = "Use `gb escalation create` to ask a privileged operator to do something — not `gb decision`"
	const memoryProtocol = "Memory protocol — file format, when to save, what not to save"
	const selfMerge = "Gardenio is in charge of its own repo — agents may self-merge any gardenio MR ('humans merge their own things' does NOT apply)"

	tests := []struct {
		name     string
		title    string
		labels   []string
		replaced bool // bead id is in a prerun's replaces_advice (delivered live)
		want     bool // expect exactly one undeliverable-capability ERROR
	}{
		{
			name:   "CAN-assertion on-demand — the ka-0000006777 bug",
			title:  canTitle,
			labels: []string{OnDemandLabel},
			want:   true,
		},
		{
			// The prerun exemption: ka-0000006777 is scope:on-demand ON PURPOSE
			// because the capability-probe prerun delivers its correction live
			// per-pod (replaces_advice names it). Flagging it here would push an
			// agent to retarget it back to project:/role: — the exact churn
			// decision ka-0000005100 had to undo after the 2026-07-30 revert.
			name:     "CAN-assertion on-demand BUT replaced by a prerun — exempt, no finding",
			title:    canTitle,
			labels:   []string{OnDemandLabel},
			replaced: true,
			want:     false,
		},
		{
			name:   "already-fetchable on-demand — the ka-0000005080 bug",
			title:  fetchTitle,
			labels: []string{OnDemandLabel},
			want:   true,
		},
		{
			name:   "CAN-assertion denied broadcast — undeliverable the other way",
			title:  canTitle,
			labels: []string{"project:gardenio"}, // bare broadcast, no opt-in
			want:   true,
		},
		{
			// The fix: once retargeted to a real project WITH the opt-in it is
			// deliverable, so the contradiction is resolved — no finding.
			name:   "CAN-assertion, retargeted + opted-in — resolved, no finding",
			title:  canTitle,
			labels: []string{"project:gardenio", BroadcastOptInLabel},
			want:   false,
		},
		{
			// Delivered by a role: target — not undeliverable, no finding.
			name:   "CAN-assertion, role-targeted — deliverable, no finding",
			title:  canTitle,
			labels: []string{"project:gardenio", "role:crew"},
			want:   false,
		},
		{
			name:   "escalation how-to on-demand — legit pointer, no finding",
			title:  escalationHowto,
			labels: []string{OnDemandLabel},
			want:   false,
		},
		{
			name:   "memory protocol on-demand — legit pointer, no finding",
			title:  memoryProtocol,
			labels: []string{OnDemandLabel},
			want:   false,
		},
		{
			// REVERSED 2026-08-27 (ka-0000007570). This was asserted to be a
			// "legit pointer", but it is a standing operator GRANT of merge
			// rights, and parking it on scope:on-demand is what let the fleet
			// keep re-deriving that it may not merge gardenio MRs. Measured the
			// same day: 4 open gitlab_mr_merge escalations against gardenio,
			// 2 for MRs that had ALREADY merged, all 4 needing no operator.
			name:   "self-merge GRANT on-demand — undeliverable grant, must flag",
			title:  selfMerge,
			labels: []string{OnDemandLabel},
			want:   true,
		},
		{
			// The fix: broadcast to the project whose agents hold the grant.
			name:   "self-merge grant, project broadcast + opt-in — deliverable, no finding",
			title:  selfMerge,
			labels: []string{"project:gardenio", BroadcastOptInLabel},
			want:   false,
		},
		{
			// "cannot" / "must escalate" must never trip the heuristic.
			name:   "cannot + must-escalate on-demand — no finding",
			title:  "Non-gardenio agents CANNOT read logs and must escalate for access",
			labels: []string{OnDemandLabel},
			want:   false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var replaced map[string]struct{}
			if tt.replaced {
				replaced = map[string]struct{}{"gd-x": {}}
			}
			findings := LintAdvice([]*beadsapi.BeadDetail{lintBeadTitled("gd-x", tt.title, tt.labels)}, nil, replaced)
			var got int
			for _, f := range findings {
				if f.Check == LintUndeliverableCap {
					if f.Severity != LintError {
						t.Errorf("undeliverable-capability severity = %q, want %q", f.Severity, LintError)
					}
					got++
				}
			}
			if tt.want && got != 1 {
				t.Errorf("got %d undeliverable-capability findings, want 1 (findings=%+v)", got, findings)
			}
			if !tt.want && got != 0 {
				t.Errorf("got %d undeliverable-capability findings, want 0 (findings=%+v)", got, findings)
			}
		})
	}
}

// TestLintAdvice_BrokenGlabFlag exercises the broken-glab-mr-flag content rule:
// a description that teaches the gh-style `glab mr list --state=<value>` (or a
// bare `--opened`) invocation is an ERROR, unless the line teaches AGAINST it
// (Unknown flag / deprecated / glab-lint-ok / "NO `--state`" / "is NOT gh").
// Mirrors scripts/lint-glab-mr-flags.sh (ka-0000003528).
func TestLintAdvice_BrokenGlabFlag(t *testing.T) {
	tests := []struct {
		name string
		desc string
		want bool // expect exactly one broken-glab-mr-flag ERROR
	}{
		{
			name: "state=opened — flagged",
			desc: "Run `glab mr list --state=opened` to see open MRs.",
			want: true,
		},
		{
			name: "space-separated --state closed — flagged",
			desc: "Try glab mr list --state closed for closed ones.",
			want: true,
		},
		{
			name: "bare --opened flag at EOL — flagged",
			desc: "Use glab mr list --opened",
			want: true,
		},
		{
			name: "bare --opened flag mid-line — flagged",
			desc: "Use glab mr list --opened to filter.",
			want: true,
		},
		{
			name: "--state merged — flagged",
			desc: "glab mr list --state=merged shows merged MRs.",
			want: true,
		},
		{
			name: "correct invocation — not flagged",
			desc: "Use `glab mr list --merged` (or -M) for merged MRs.",
			want: false,
		},
		{
			name: "the word state in prose — not flagged",
			desc: "The MR list reflects the current state of review.",
			want: false,
		},
		{
			name: "teaching line with Unknown flag — exempt",
			desc: "glab mr list --state=opened errors with `Unknown flag: --state`. Use bare `glab mr list`.",
			want: false,
		},
		{
			name: "teaching line with deprecated — exempt",
			desc: "The `glab mr list --opened` flag is deprecated; use bare `glab mr list`.",
			want: false,
		},
		{
			name: "glab-lint-ok suppression token — exempt",
			desc: "Historical snapshot: glab mr list --state=opened  # glab-lint-ok",
			want: false,
		},
		{
			name: "is NOT gh warning phrasing — exempt",
			desc: "glab is NOT gh: glab mr list --state=opened does not work.",
			want: false,
		},
		{
			name: "violation on one line, exemption on another — flagged",
			desc: "glab is NOT gh.\nSome agents still write glab mr list --state=opened by habit.",
			want: true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			b := lintBead("gd-x", []string{"global"})
			b.Description = tt.desc
			findings := LintAdvice([]*beadsapi.BeadDetail{b}, nil, nil)
			var got int
			for _, f := range findings {
				if f.Check == LintBrokenGlabFlag {
					if f.Severity != LintError {
						t.Errorf("broken-glab-mr-flag severity = %q, want %q", f.Severity, LintError)
					}
					got++
				}
			}
			if tt.want && got != 1 {
				t.Errorf("got %d broken-glab-mr-flag findings, want 1 (findings=%+v)", got, findings)
			}
			if !tt.want && got != 0 {
				t.Errorf("got %d broken-glab-mr-flag findings, want 0 (findings=%+v)", got, findings)
			}
		})
	}
}
