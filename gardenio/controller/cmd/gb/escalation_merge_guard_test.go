package main

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"gardenio/controller/internal/beadsapi"
	"gardenio/controller/internal/bridge"
)

// The spec shapes below are the ones agents actually wrote across the open
// escalation queue on 2026-08-16 — not invented variants. If a future spec
// spelling shows up in the wild, add it here first.
func TestParseMergeTarget(t *testing.T) {
	tests := []struct {
		name        string
		spec        string
		wantOK      bool
		wantProject string
		wantIID     int
	}{
		{
			name:        "project plus mr_iid",
			spec:        `{"mr_iid":2399,"project":"Acme/Platform/monorepo"}`,
			wantOK:      true,
			wantProject: "Acme/Platform/monorepo",
			wantIID:     2399,
		},
		{
			name:        "project plus merge_request_iid",
			spec:        `{"merge_request_iid":7385,"project":"Acme/Platform/gardenio"}`,
			wantOK:      true,
			wantProject: "Acme/Platform/gardenio",
			wantIID:     7385,
		},
		{
			name:        "mr_url alone carries project and iid",
			spec:        `{"mr_url":"https://gitlab.com/Acme/Platform/monorepo/-/merge_requests/2452"}`,
			wantOK:      true,
			wantProject: "Acme/Platform/monorepo",
			wantIID:     2452,
		},
		{
			name:        "bare url key",
			spec:        `{"project":"Acme/Platform/monorepo","url":"https://gitlab.com/Acme/Platform/monorepo/-/merge_requests/2449"}`,
			wantOK:      true,
			wantProject: "Acme/Platform/monorepo",
			wantIID:     2449,
		},
		{
			name:        "iid as string still parses",
			spec:        `{"project":"g/p","mr_iid":"123"}`,
			wantOK:      true,
			wantProject: "g/p",
			wantIID:     123,
		},
		// The empty spec is the single most common shape in the queue; it must
		// fail open rather than block the escalation.
		{name: "empty object yields no target", spec: `{}`, wantOK: false},
		{name: "project without iid yields no target", spec: `{"project":"g/p"}`, wantOK: false},
		{name: "iid without project yields no target", spec: `{"mr_iid":5}`, wantOK: false},
		{name: "non-object spec yields no target", spec: `"just a string"`, wantOK: false},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			var obj any
			if err := json.Unmarshal([]byte(tc.spec), &obj); err != nil {
				t.Fatalf("bad test fixture: %v", err)
			}
			got, ok := parseMergeTarget(obj)
			if ok != tc.wantOK {
				t.Fatalf("ok = %v, want %v (target %+v)", ok, tc.wantOK, got)
			}
			if !tc.wantOK {
				return
			}
			if got.ProjectPath != tc.wantProject || got.IID != tc.wantIID {
				t.Errorf("target = %s!%d, want %s!%d", got.ProjectPath, got.IID, tc.wantProject, tc.wantIID)
			}
		})
	}
}

// TestEvaluateMergeGuard: a gitlab_mr_merge escalation is refused when the
// filing agent can merge the MR itself (ka-0000004913).
func TestEvaluateMergeGuard(t *testing.T) {
	target := mergeTarget{ProjectPath: "Acme/Platform/gardenio", IID: 7421}

	t.Run("can_merge true refuses", func(t *testing.T) {
		// The gardenio case: 39 of 40 such escalations were filed by an agent
		// that could merge, and every one of those MRs merged itself.
		d := evaluateMergeGuard(target, &bridge.GitLabMR{
			State: "opened",
			User:  &bridge.GitLabMRUserPerms{CanMerge: true},
		}, nil)
		if !d.Refuse {
			t.Fatalf("expected refusal, got %+v", d)
		}
		if !strings.Contains(d.Reason, "YOU CAN MERGE") {
			t.Errorf("reason should lead with the capability finding, got: %s", d.Reason)
		}
		// The message must hand back a runnable command, not just a complaint.
		if !strings.Contains(d.Reason, "Acme%2FPlatform%2Fgardenio") {
			t.Errorf("reason should include the encoded project path, got: %s", d.Reason)
		}
		// Regression guard for ka-0000007570. The guard already produced the
		// right verdict here and agents bypassed it anyway, arguing that
		// can_merge only reflects push access. Escalation ka-0000007844 carried
		// that reasoning verbatim while GitLab reported can_merge=true and the
		// MR was merged minutes later by an agent. So the refusal has to rebut
		// the rationalization, not merely state the verdict.
		for _, want := range []string{
			"NOT merely 'push access'",
			"merge_access_levels",
			"protected main",
		} {
			if !strings.Contains(d.Reason, want) {
				t.Errorf("refusal must pre-empt the 'can_merge is only push access' rationalization; missing %q in: %s", want, d.Reason)
			}
		}
	})

	t.Run("can_merge false proceeds", func(t *testing.T) {
		// The classic legitimate case: a protected branch whose merge_access_levels
		// grant merge only to a role the bot lacks, so GitLab reports
		// can_merge=false and PUT /merge genuinely 401s. (NOT monorepo — monorepo
		// grants merge to Developers/30 and the bot IS 30, so there can_merge=true;
		// monorepo is legitimate for a DIFFERENT reason, see the not_approved
		// subtests below.) This escalation must be allowed through untouched.
		d := evaluateMergeGuard(target, &bridge.GitLabMR{
			State: "opened",
			User:  &bridge.GitLabMRUserPerms{CanMerge: false},
		}, nil)
		if d.Refuse || d.Warn {
			t.Fatalf("legitimate escalation must pass clean, got %+v", d)
		}
	})

	// ── ka-0000005160: can_merge=true is NOT "self-mergeable" ────────────────
	//
	// Reading can_merge alone stranded the entire non-gardenio merge queue. On
	// monorepo the bot IS Developer/30 and protected main grants merge at 30, so
	// can_merge=true — but monorepo also carries approval rules the field cannot
	// see. MEASURED 2026-08-27: !2646 and !2643 both reported can_merge=true,
	// merge_status=can_be_merged, detailed_merge_status=not_approved,
	// approvals_left=1. The bot cannot self-approve, so that IS an operator gate,
	// and refusing the escalation deadlocked against mr_dispatch_gate.go — which
	// tells the very same agent that only gardenio is self-mergeable.
	t.Run("can_merge true but not_approved proceeds", func(t *testing.T) {
		mono := mergeTarget{ProjectPath: "Acme/Platform/monorepo", IID: 2646}
		d := evaluateMergeGuard(mono, &bridge.GitLabMR{
			State:               "opened",
			MergeStatus:         "can_be_merged",
			DetailedMergeStatus: "not_approved",
			User:                &bridge.GitLabMRUserPerms{CanMerge: true},
		}, nil)
		if d.Refuse {
			t.Fatalf("an approval the bot cannot give is a REAL operator gate; must not refuse, got %+v", d)
		}
		if !d.Warn {
			t.Errorf("the can_merge/not_approved split should be surfaced, not silent; got %+v", d)
		}
		for _, want := range []string{"not_approved", "cannot approve its own MR", "REAL operator gate"} {
			if !strings.Contains(d.Reason, want) {
				t.Errorf("missing %q in: %s", want, d.Reason)
			}
		}
	})

	for _, status := range []string{"requested_changes", "policies_denied"} {
		t.Run("can_merge true but "+status+" proceeds", func(t *testing.T) {
			d := evaluateMergeGuard(target, &bridge.GitLabMR{
				State:               "opened",
				DetailedMergeStatus: status,
				User:                &bridge.GitLabMRUserPerms{CanMerge: true},
			}, nil)
			if d.Refuse {
				t.Fatalf("%s is only clearable by a human; must not refuse, got %+v", status, d)
			}
		})
	}

	// The carve-out must stay narrow: a blocker the AGENT can clear still earns
	// the refusal, or we are back to the 39-of-40 already-mergeable inflow.
	for _, status := range []string{"", "mergeable", "ci_still_running", "need_rebase", "conflict", "draft_status", "discussions_not_resolved", "checking", "preparing"} {
		name := status
		if name == "" {
			name = "absent"
		}
		t.Run("can_merge true with agent-clearable "+name+" refuses", func(t *testing.T) {
			d := evaluateMergeGuard(target, &bridge.GitLabMR{
				State:               "opened",
				DetailedMergeStatus: status,
				User:                &bridge.GitLabMRUserPerms{CanMerge: true},
			}, nil)
			if !d.Refuse {
				t.Fatalf("detailed_merge_status=%q is the agent's own to clear; expected refusal, got %+v", status, d)
			}
			if status != "" && !strings.Contains(d.Reason, status) {
				t.Errorf("refusal should name the observed status %q so the agent knows what to fix; got: %s", status, d.Reason)
			}
		})
	}

	// gardenio carries zero approval rules, so no gardenio MR can reach the
	// operator-only set — the standing self-merge rule (ka-0000002230) is intact.
	t.Run("operator-only set excludes every agent-clearable status", func(t *testing.T) {
		for _, agentClearable := range []string{"mergeable", "ci_still_running", "ci_must_pass", "need_rebase", "conflict", "draft_status", "discussions_not_resolved", "checking", "unchecked", "preparing"} {
			if _, ok := operatorOnlyMergeBlockers[agentClearable]; ok {
				t.Errorf("%q is the agent's own to clear and must not be in operatorOnlyMergeBlockers", agentClearable)
			}
		}
		if _, ok := operatorOnlyMergeBlockers["not_approved"]; !ok {
			t.Error("not_approved must be operator-only — the bot cannot self-approve (ka-0000005160)")
		}
	})

	t.Run("already merged refuses", func(t *testing.T) {
		d := evaluateMergeGuard(target, &bridge.GitLabMR{State: "merged"}, nil)
		if !d.Refuse {
			t.Fatalf("expected refusal for a merged MR, got %+v", d)
		}
		if !strings.Contains(d.Reason, "ALREADY MERGED") {
			t.Errorf("reason should say the MR already merged, got: %s", d.Reason)
		}
	})

	t.Run("closed refuses", func(t *testing.T) {
		d := evaluateMergeGuard(target, &bridge.GitLabMR{State: "closed"}, nil)
		if !d.Refuse {
			t.Fatalf("expected refusal for a closed MR, got %+v", d)
		}
	})

	// Everything below is the fail-open contract: an unknown must never block.
	t.Run("fetch error warns but proceeds", func(t *testing.T) {
		d := evaluateMergeGuard(target, nil, errors.New("403 Forbidden"))
		if d.Refuse {
			t.Fatalf("a fetch error must not block the escalation, got %+v", d)
		}
		if !d.Warn {
			t.Errorf("expected a warning, got %+v", d)
		}
	})

	t.Run("nil MR warns but proceeds", func(t *testing.T) {
		d := evaluateMergeGuard(target, nil, nil)
		if d.Refuse || !d.Warn {
			t.Fatalf("nil MR must warn and proceed, got %+v", d)
		}
	})

	t.Run("absent permission block warns but proceeds", func(t *testing.T) {
		// nil User means "GitLab didn't tell us", which is NOT "not allowed".
		d := evaluateMergeGuard(target, &bridge.GitLabMR{State: "opened"}, nil)
		if d.Refuse {
			t.Fatalf("unknown permissions must not block, got %+v", d)
		}
		if !d.Warn {
			t.Errorf("expected a warning, got %+v", d)
		}
	})
}

// The guard reads user.can_merge off a real single-MR payload; this pins the
// decoding so a struct-tag change cannot silently turn every verdict into
// "unknown" (which would fail open forever and quietly disable the guard).
func TestGitLabMRDecodesUserCanMerge(t *testing.T) {
	const payload = `{"iid":7421,"state":"opened","user":{"can_merge":true}}`
	var mr bridge.GitLabMR
	if err := json.Unmarshal([]byte(payload), &mr); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if mr.User == nil {
		t.Fatal("user block did not decode — the guard would fail open forever")
	}
	if !mr.User.CanMerge {
		t.Error("can_merge did not decode as true")
	}
}

// The recovery path (ka-0000003288): a spec-less gitlab_mr_merge escalation used
// to fail open SILENTLY, which taught agents that stripping --operation-spec was
// the cheapest way past a refusal. The guard now recovers the target from the
// requesting bead's mr_url first, and only warns (never silent) when no MR is
// discoverable anywhere.

func TestMRURLFromBead(t *testing.T) {
	tests := []struct {
		name string
		bead *beadsapi.BeadDetail
		want string
	}{
		{name: "nil bead", bead: nil, want: ""},
		{name: "nil fields", bead: &beadsapi.BeadDetail{}, want: ""},
		{
			name: "mr_url field",
			bead: &beadsapi.BeadDetail{Fields: map[string]string{
				"mr_url": "https://gitlab.com/Acme/Platform/gardenio/-/merge_requests/7705"}},
			want: "https://gitlab.com/Acme/Platform/gardenio/-/merge_requests/7705",
		},
		{
			name: "merge_request_url fallback spelling",
			bead: &beadsapi.BeadDetail{Fields: map[string]string{
				"merge_request_url": "https://gitlab.com/g/p/-/merge_requests/9"}},
			want: "https://gitlab.com/g/p/-/merge_requests/9",
		},
		{
			name: "mr_url wins over the fallback spelling",
			bead: &beadsapi.BeadDetail{Fields: map[string]string{
				"mr_url":            "https://gitlab.com/g/p/-/merge_requests/1",
				"merge_request_url": "https://gitlab.com/g/p/-/merge_requests/2"}},
			want: "https://gitlab.com/g/p/-/merge_requests/1",
		},
		{
			name: "whitespace-only is empty",
			bead: &beadsapi.BeadDetail{Fields: map[string]string{"mr_url": "   "}},
			want: "",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := mrURLFromBead(tc.bead); got != tc.want {
				t.Errorf("mrURLFromBead = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestTargetFromMRURL(t *testing.T) {
	got := targetFromMRURL("https://gitlab.com/Acme/Platform/gardenio/-/merge_requests/7705")
	if got.ProjectPath != "Acme/Platform/gardenio" || got.IID != 7705 {
		t.Fatalf("target = %s!%d, want Acme/Platform/gardenio!7705", got.ProjectPath, got.IID)
	}
	if !mrURLIsParseable("https://gitlab.com/g/p/-/merge_requests/1") {
		t.Error("a real MR URL should parse")
	}
	for _, bad := range []string{"", "not a url", "https://gitlab.com/g/p"} {
		if mrURLIsParseable(bad) {
			t.Errorf("%q should not parse as an MR URL", bad)
		}
		if got := targetFromMRURL(bad); got != (mergeTarget{}) {
			t.Errorf("targetFromMRURL(%q) = %+v, want zero value", bad, got)
		}
	}
}

// The behavior the bug is about: with no spec target AND no recoverable MR the
// guard must WARN (visible), not return an empty/silent decision.
func TestCheckMergeEscalationWarnsWhenNoTargetAnywhere(t *testing.T) {
	// daemon is nil in the unit-test binary, so recoverTargetFromRequestingTask
	// fails soft and the guard falls through to the visible warning.
	d := checkMergeEscalationCapability(t.Context(), map[string]any{}, "")
	if d.Refuse {
		t.Fatalf("a target-less escalation must not be refused, got %+v", d)
	}
	if !d.Warn {
		t.Fatalf("a target-less escalation must WARN (not fail open silently), got %+v", d)
	}
	if !strings.Contains(d.Reason, "names no MR") {
		t.Errorf("warning should explain the missing target, got: %s", d.Reason)
	}
}

func TestDecodeOperationSpecAcceptsStringAndObject(t *testing.T) {
	obj := decodeOperationSpec(map[string]any{"project": "g/p", "mr_iid": float64(7)})
	if _, ok := parseMergeTarget(obj); !ok {
		t.Error("map form should parse")
	}
	str := decodeOperationSpec(`{"project":"g/p","mr_iid":7}`)
	if _, ok := parseMergeTarget(str); !ok {
		t.Error("string form should parse")
	}
	if got := decodeOperationSpec(`not json`); got != nil {
		t.Errorf("unparseable string should yield nil, got %v", got)
	}
}
