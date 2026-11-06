package main

// plans_escalation_op_test.go — coverage for the oldest blocking-escalation
// OPERATION TYPE folded onto the "⛔ blocked by escalation" badge FACE
// (ka-0000005352 iteration 37). The escalation badge is the #2 attention tier
// (planAttentionEscalation), a HARDER stop than ⚠ stalled — a privileged
// operator must DO something before any blocked work can move — yet it was the
// LAST attention-tier badge whose face still hid its whole discriminator (WHAT
// to do) in the tooltip. Iter-8 gave the badge its wait-age; this gives it its
// operation type, the exact "fold the discriminator onto the face" move iter-10
// made for the ⚠ stall badge's reason. Three levels: the pure
// humanizeEscalationOp truth-table + OldestOperationLabel projection (incl. the
// oldest-ref selection + fail-quiet unknown/legacy fallback), the HTTP render of
// the op suffix (fail-without/pass-with) alongside the age, and a must-not-
// regress that a legacy/unrecognized op renders age-only exactly as iter-8.

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// TestHumanizeEscalationOp pins the free-text → scannable-face map: the families
// that actually occur on the live board humanize to a short WHAT-to-do label,
// spelling drift normalizes to the same face, and anything it can't shorten
// meaningfully (empty, a vague "manual"-class type, or a novel token) returns ""
// so the badge never dumps a raw snake_case operation_type onto the card.
func TestHumanizeEscalationOp(t *testing.T) {
	cases := []struct {
		op   string
		want string
	}{
		// The dominant GitLab merge family — all collapse to one face.
		{"gitlab_mr_merge", "merge MR"},
		{"gitlab_merge", "merge MR"},
		{"merge_request", "merge MR"},
		{"merge_approval", "merge MR"},
		{"gitlab_mr_approval", "merge MR"},
		{"gitlab-mr-approve-merge", "merge MR"}, // separator-drift normalized
		// GitLab admin is distinct from a merge.
		{"gitlab_admin", "GitLab admin"},
		// AWS / infra-as-code.
		{"aws_iam", "AWS IAM"},
		{"aws_ec2", "AWS access"},
		{"aws_eks", "AWS access"},
		{"terraform_apply", "terraform"},
		// K8s.
		{"k8s_rbac", "K8s access"},
		{"k8s_admission_control", "K8s access"},
		// Credential provisioning / rotation / vault.
		{"credential_provisioning", "credential"},
		{"credential_rotation", "credential"},
		{"vault_provisioning", "credential"},
		// Deploy family.
		{"deploy", "deploy"},
		{"deploy_release", "deploy"},
		{"deployment", "deploy"},
		// Misc concrete actions.
		{"db_access", "DB access"},
		{"ticket_management", "JIRA"},
		{"jira_edit", "JIRA"},
		{"slack-admin", "Slack admin"}, // separator-drift normalized
		{"trigger_lifecycle", "trigger"},
		// Case-insensitivity.
		{"GITLAB_MR_MERGE", "merge MR"},
		{"AWS_IAM", "AWS IAM"},
		// Fail-quiet: no meaningful shortening → "" (badge reads age-only).
		{"", ""},
		{"manual_intervention", ""},   // "a human must act" — the badge already says that
		{"infrastructure", ""},        // too vague to name an action
		{"infrastructure_change", ""}, //
		{"configuration", ""},         //
		{"other", ""},                 //
		{"some_new_op_type", ""},      // a novel token never leaks raw onto the card
	}
	for _, tc := range cases {
		t.Run(tc.op, func(t *testing.T) {
			if got := humanizeEscalationOp(tc.op); got != tc.want {
				t.Errorf("humanizeEscalationOp(%q) = %q, want %q", tc.op, got, tc.want)
			}
		})
	}
}

// TestPlanEscalationInfo_OldestOperationLabel proves OldestOperationLabel reads
// the OLDEST ref's operation_type — the SAME ref the wait-age (OldestAge) reads —
// so the op face and the age face always describe one escalation, and that it
// skips an unstamped leading ref exactly as oldestCreatedAt does.
func TestPlanEscalationInfo_OldestOperationLabel(t *testing.T) {
	now := time.Date(2026, 8, 1, 12, 0, 0, 0, time.UTC)
	oldest := now.Add(-40 * 24 * time.Hour)
	newer := now.Add(-2 * 24 * time.Hour)

	// The oldest ref is a terraform_apply; a NEWER ref is a gitlab_mr_merge. The
	// face must name the OLDEST (terraform), matching the age.
	info := planEscalationInfo{
		Count: 2,
		Escalations: []planEscalationRef{
			{ID: "gd-old", OperationType: "terraform_apply", createdAt: oldest},
			{ID: "gd-new", OperationType: "gitlab_mr_merge", createdAt: newer},
		},
	}
	if got := info.OldestOperationLabel(); got != "terraform" {
		t.Errorf("OldestOperationLabel = %q, want %q (must name the OLDEST ref, matching OldestAge)", got, "terraform")
	}
	// Sanity: the age names the same (oldest) ref.
	if got := info.OldestAge(now); got != "40d" {
		t.Errorf("OldestAge = %q, want 40d (the op + age must describe one escalation)", got)
	}

	// Empty list → no label.
	if got := (planEscalationInfo{}).OldestOperationLabel(); got != "" {
		t.Errorf("empty info OldestOperationLabel = %q, want \"\"", got)
	}

	// An unstamped LEADING ref must not displace a stamped one — the op label
	// tracks the earliest REAL stamp, mirroring oldestCreatedAt's guard.
	unstampedLead := planEscalationInfo{
		Count: 2,
		Escalations: []planEscalationRef{
			{ID: "gd-nostamp", OperationType: "gitlab_admin"},                              // zero createdAt
			{ID: "gd-real", OperationType: "aws_iam", createdAt: now.Add(-9 * time.Hour)}, // the datable ref
		},
	}
	if got := unstampedLead.OldestOperationLabel(); got != "AWS IAM" {
		t.Errorf("OldestOperationLabel = %q, want %q (unstamped ref must not win)", got, "AWS IAM")
	}

	// A legacy/unrecognized oldest op → "" (fail quiet; badge reads age-only).
	legacy := planEscalationInfo{Count: 1, Escalations: []planEscalationRef{
		{ID: "gd-x", OperationType: "manual_intervention", createdAt: oldest}}}
	if got := legacy.OldestOperationLabel(); got != "" {
		t.Errorf("OldestOperationLabel = %q, want \"\" for a vague op (fail quiet)", got)
	}
}

// mkEscOpTestServer builds a one-plan fake blocked by an open escalation with the
// given operation_type, created a fixed 3 days before a pinned clock, so both the
// op face and the wait-age render deterministically.
func mkEscOpTestServer(t *testing.T, opType string) http.Handler {
	t.Helper()
	now := time.Date(2026, 8, 1, 12, 0, 0, 0, time.UTC)
	f := seedPlansFake()
	esc := seedPlanEscalation(f, "gd-esc1", "gd-step1", "Approve AND merge the fix", opType, "internal")
	esc.CreatedAt = now.Add(-3 * 24 * time.Hour)
	return mkServerWithNow(t, f, func() time.Time { return now })
}

// TestPlansIndex_EscalationBadgeShowsOpFace proves a gitlab_mr_merge escalation
// renders the "· merge MR" op suffix on the badge face beside the wait-age —
// the fail-without/pass-with anchor (reverting the template seam removes the op
// span but the age span stays, so this test alone reds on the revert).
func TestPlansIndex_EscalationBadgeShowsOpFace(t *testing.T) {
	mux := mkEscOpTestServer(t, "gitlab_mr_merge")
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, httptest.NewRequest("GET", "/plans", nil))
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	body := w.Body.String()
	if !strings.Contains(body, "blocked by escalation") {
		t.Fatalf("expected the ⛔ escalation badge; body missing it")
	}
	if !strings.Contains(body, `<span class="plan-esc-badge-op">· merge MR</span>`) {
		t.Errorf("expected the '· merge MR' op face span on the escalation badge; body missing it")
	}
	// The op face must sit BEFORE the age face (WHAT then WHEN), and both render.
	if !strings.Contains(body, `<span class="plan-esc-badge-age">· 3d`) {
		t.Errorf("expected the '· 3d' wait-age suffix to still render beside the op face")
	}
	// Order the two facets WITHIN the rendered badge anchor (not the <style>
	// block, where the CSS rules are defined age-before-op). Slice from the
	// rendered badge's <a> so the indices reflect DOM order on the face.
	badgeStart := strings.Index(body, `<a class="plan-esc-badge`)
	if badgeStart < 0 {
		t.Fatalf("could not locate the rendered escalation badge anchor")
	}
	badge := body[badgeStart:]
	if end := strings.Index(badge, "</a>"); end >= 0 {
		badge = badge[:end]
	}
	opIdx := strings.Index(badge, `plan-esc-badge-op`)
	ageIdx := strings.Index(badge, `plan-esc-badge-age`)
	if opIdx < 0 || ageIdx < 0 || opIdx > ageIdx {
		t.Errorf("op face must render before the age face on the badge (op=%d, age=%d)", opIdx, ageIdx)
	}
}

// TestPlansIndex_EscalationBadgeOpFacesVary proves distinct operation types
// render distinct faces — an aws_iam escalation reads "· AWS IAM", not the
// gitlab merge face — so the discriminator actually discriminates on the board.
func TestPlansIndex_EscalationBadgeOpFacesVary(t *testing.T) {
	mux := mkEscOpTestServer(t, "aws_iam")
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, httptest.NewRequest("GET", "/plans", nil))
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	body := w.Body.String()
	if !strings.Contains(body, `<span class="plan-esc-badge-op">· AWS IAM</span>`) {
		t.Errorf("expected the '· AWS IAM' op face for an aws_iam escalation; body missing it")
	}
	if strings.Contains(body, "· merge MR") {
		t.Errorf("an aws_iam escalation must NOT render the gitlab merge face")
	}
}

// TestPlansIndex_EscalationBadgeNoOpFaceWhenUnrecognized proves the density-
// discipline must-not-regress: an escalation whose operation_type humanizes to
// "" (a vague manual_intervention, or a legacy row with no op) renders exactly
// as iter-8 — badge + age, no op span, no raw token dumped onto the card.
func TestPlansIndex_EscalationBadgeNoOpFaceWhenUnrecognized(t *testing.T) {
	for _, opType := range []string{"manual_intervention", ""} {
		name := opType
		if name == "" {
			name = "empty"
		}
		t.Run(name, func(t *testing.T) {
			mux := mkEscOpTestServer(t, opType)
			w := httptest.NewRecorder()
			mux.ServeHTTP(w, httptest.NewRequest("GET", "/plans", nil))
			if w.Code != http.StatusOK {
				t.Fatalf("expected 200, got %d", w.Code)
			}
			body := w.Body.String()
			if !strings.Contains(body, "blocked by escalation") {
				t.Fatalf("expected the ⛔ escalation badge; body missing it")
			}
			if strings.Contains(body, `<span class="plan-esc-badge-op">`) {
				t.Errorf("op %q must render NO op face span (fail quiet); body has one", opType)
			}
			// The card must NOT leak the raw operation_type token onto the face.
			if opType != "" && strings.Contains(body, "· "+opType) {
				t.Errorf("a raw operation_type token %q must never render on the badge face", opType)
			}
			// The age suffix (iter-8) must still render — the op-face is additive.
			if !strings.Contains(body, `<span class="plan-esc-badge-age">· 3d`) {
				t.Errorf("the '· 3d' wait-age must still render (op-face is additive to iter-8)")
			}
		})
	}
}

// TestPlanEscalationsBulk_OpAddsNoFanOut proves the density contract: the op
// label is a pure projection off the refs planEscalationsBulk already hydrates
// (operation_type is on every ref), so OldestOperationLabel adds ZERO daemon
// round-trips beyond the escalation aggregation's own budget.
func TestPlanEscalationsBulk_OpAddsNoFanOut(t *testing.T) {
	now := time.Date(2026, 8, 1, 12, 0, 0, 0, time.UTC)
	f := seedPlansFake()
	esc := seedPlanEscalation(f, "gd-esc1", "gd-step1", "Approve AND merge the fix", "gitlab_mr_merge", "internal")
	esc.CreatedAt = now.Add(-3 * 24 * time.Hour)
	s := testServerWithDaemon(f)

	depsBefore, beadsBefore := f.batchGetDepsCalls, f.batchGetBeadsCalls
	listBefore := len(f.listAllQ)

	info := s.planEscalationsBulk(context.Background(), map[string][]string{
		"gd-plan1": {"gd-plan1", "gd-step1"},
	})["gd-plan1"]
	if info.Count != 1 {
		t.Fatalf("Count = %d, want 1", info.Count)
	}
	if got := info.OldestOperationLabel(); got != "merge MR" {
		t.Errorf("OldestOperationLabel = %q, want %q", got, "merge MR")
	}
	// The projection reads the ALREADY-hydrated refs — no further daemon calls.
	if got := f.batchGetDepsCalls - depsBefore; got != 1 {
		t.Errorf("BatchGetDependencies calls = %d, want 1", got)
	}
	if got := f.batchGetBeadsCalls - beadsBefore; got != 1 {
		t.Errorf("BatchGetBeads calls = %d, want 1", got)
	}
	if got := len(f.listAllQ) - listBefore; got != 1 {
		t.Errorf("ListBeadsFiltered calls = %d, want 1", got)
	}
}
