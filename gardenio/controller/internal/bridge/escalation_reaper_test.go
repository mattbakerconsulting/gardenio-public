package bridge

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"maps"
	"strings"
	"testing"
	"time"

	"gardenio/controller/internal/beadsapi"
)

// --- fakes ----------------------------------------------------------------

// fakeReaperGitLab is a tiny in-memory GitLab client for the reaper tests. MRs
// and approvals are keyed by "projectPath!iid".
type fakeReaperGitLab struct {
	mrs       map[string]*GitLabMR
	approvals map[string]*GitLabMRApprovals
	mrErr     map[string]error
	apprErr   map[string]error
	// getCalls / apprCalls record how many times each endpoint was hit, so a
	// test can assert the verb picked the RIGHT signal (never call the merge
	// endpoint for an approve_only ask, and vice-versa).
	getCalls  map[string]int
	apprCalls map[string]int
}

func newFakeReaperGitLab() *fakeReaperGitLab {
	return &fakeReaperGitLab{
		mrs:       map[string]*GitLabMR{},
		approvals: map[string]*GitLabMRApprovals{},
		mrErr:     map[string]error{},
		apprErr:   map[string]error{},
		getCalls:  map[string]int{},
		apprCalls: map[string]int{},
	}
}

func mrKey(projectPath string, iid int) string {
	return fmt.Sprintf("%s!%d", projectPath, iid)
}

func (f *fakeReaperGitLab) GetMergeRequestByPath(_ context.Context, projectPath string, iid int) (*GitLabMR, error) {
	k := mrKey(projectPath, iid)
	f.getCalls[k]++
	if err := f.mrErr[k]; err != nil {
		return nil, err
	}
	mr, ok := f.mrs[k]
	if !ok {
		return nil, fmt.Errorf("GitLab get MR %s: returned 404", k)
	}
	return mr, nil
}

func (f *fakeReaperGitLab) ListMergeRequestApprovals(_ context.Context, projectPath string, iid int) (*GitLabMRApprovals, error) {
	k := mrKey(projectPath, iid)
	f.apprCalls[k]++
	if err := f.apprErr[k]; err != nil {
		return nil, err
	}
	a, ok := f.approvals[k]
	if !ok {
		// Mirror the CE 404-as-zero-value behavior of the real client.
		return &GitLabMRApprovals{}, nil
	}
	return a, nil
}

// fakeReaperDaemon records comments and closes and serves a fixed bead list.
type fakeReaperDaemon struct {
	beads        []*beadsapi.BeadDetail
	listErr      error
	comments     map[string][]string           // bead ID → reaper-authored audit comment bodies
	existing     map[string][]beadsapi.Comment // bead ID → pre-existing comments served by GetComments
	getCommErr   map[string]error              // bead ID → error returned by GetComments
	closed       map[string]map[string]string  // bead ID → merged result/close fields
	resultCalls  map[string]int                // bead ID → CaptureEscalationResult call count
	closeErr     map[string]error
	commErr      map[string]error
	listCalls    int
	getCommCalls map[string]int // bead ID → GetComments call count
	// fieldUpdates accumulates every UpdateBeadFields patch per bead (in call
	// order) so the review-flag tests can assert the latch + pingback stamps.
	fieldUpdates map[string][]map[string]string
	fieldErr     map[string]error
	// labels records AddLabel calls per bead; labelErr injects a failure.
	labels   map[string][]string
	labelErr map[string]error
}

func newFakeReaperDaemon(beads ...*beadsapi.BeadDetail) *fakeReaperDaemon {
	return &fakeReaperDaemon{
		beads:        beads,
		comments:     map[string][]string{},
		existing:     map[string][]beadsapi.Comment{},
		getCommErr:   map[string]error{},
		closed:       map[string]map[string]string{},
		resultCalls:  map[string]int{},
		closeErr:     map[string]error{},
		commErr:      map[string]error{},
		getCommCalls: map[string]int{},
		fieldUpdates: map[string][]map[string]string{},
		fieldErr:     map[string]error{},
		labels:       map[string][]string{},
		labelErr:     map[string]error{},
	}
}

func (d *fakeReaperDaemon) UpdateBeadFields(_ context.Context, beadID string, fields map[string]string) error {
	if err := d.fieldErr[beadID]; err != nil {
		return err
	}
	cp := map[string]string{}
	maps.Copy(cp, fields)
	d.fieldUpdates[beadID] = append(d.fieldUpdates[beadID], cp)
	// Reflect the patch onto the in-memory bead so a follow-up sweep sees the
	// latch (mirrors the daemon's additive merge — this is what makes the
	// idempotency test meaningful).
	for _, b := range d.beads {
		if b.ID == beadID {
			if b.Fields == nil {
				b.Fields = map[string]string{}
			}
			maps.Copy(b.Fields, fields)
		}
	}
	return nil
}

func (d *fakeReaperDaemon) AddLabel(_ context.Context, beadID, label string) error {
	if err := d.labelErr[beadID]; err != nil {
		return err
	}
	d.labels[beadID] = append(d.labels[beadID], label)
	return nil
}

func (d *fakeReaperDaemon) GetComments(_ context.Context, beadID string) ([]beadsapi.Comment, error) {
	d.getCommCalls[beadID]++
	if err := d.getCommErr[beadID]; err != nil {
		return nil, err
	}
	return d.existing[beadID], nil
}

func (d *fakeReaperDaemon) ListBeadsFiltered(_ context.Context, _ beadsapi.ListBeadsQuery) (*beadsapi.ListBeadsResult, error) {
	d.listCalls++
	if d.listErr != nil {
		return nil, d.listErr
	}
	// Return only still-open beads, mirroring the daemon's status filter — this
	// is what makes the sweep idempotent (a closed bead drops out).
	var open []*beadsapi.BeadDetail
	for _, b := range d.beads {
		if _, isClosed := d.closed[b.ID]; !isClosed {
			open = append(open, b)
		}
	}
	return &beadsapi.ListBeadsResult{Beads: open}, nil
}

func (d *fakeReaperDaemon) AddComment(_ context.Context, beadID, _, text string) error {
	if err := d.commErr[beadID]; err != nil {
		return err
	}
	d.comments[beadID] = append(d.comments[beadID], text)
	return nil
}

// CloseBead models the daemon's dedicated-verbs guard (ka-0000005516): a generic
// close of an OPEN escalation is REJECTED with the exact 409 the daemon returns.
// It is deliberately NOT part of ReaperBeadClient, so the reaper cannot call it —
// this method exists only so TestReaper_GenericCloseOfOpenEscalation_Rejected can
// prove the fake reproduces the real 409 environment. Without that fidelity, the
// "reaches closed via the result verb" assertions below would be vacuous (the old
// tests passed straight through the regression precisely because their stub let a
// generic close succeed).
func (d *fakeReaperDaemon) CloseBead(_ context.Context, beadID string, _ map[string]string) error {
	return fmt.Errorf("HTTP 409: escalation %s must be resolved through its dedicated verbs, not the generic close path", beadID)
}

// CaptureEscalationResult models POST /v1/escalations/{id}/result: like the
// daemon's handleCaptureEscalationResult (P7 Task 10) it refuses any Fields key
// outside the render/witness allowlist (reapRenderField) and ignores the body
// executor, then merges Fields and records the close. This is the ONLY close
// path the reaper is allowed to take. resultCalls records that the verb was
// used; d.closed keeps the merged fields so every close-field assertion in this
// suite reads the render/audit trail (reaped*, mr_*, approvals_*).
func (d *fakeReaperDaemon) CaptureEscalationResult(_ context.Context, escalationID string, req beadsapi.CaptureEscalationResultRequest) error {
	if err := d.closeErr[escalationID]; err != nil {
		return err
	}
	for k := range req.Fields {
		if !reapRenderField(k) {
			return fmt.Errorf("400: result field %s is not allowed", k)
		}
	}
	d.resultCalls[escalationID]++
	cp := map[string]string{}
	for k, v := range req.Fields {
		if v != "" {
			cp[k] = v
		}
	}
	cp["result_status"] = req.ResultStatus
	d.closed[escalationID] = cp
	return nil
}

// reapRenderField mirrors the daemon's resultRenderField: the only keys a
// bridge may send in /result's fields.
func reapRenderField(key string) bool {
	return key == "reaped" || strings.HasPrefix(key, "reaped_") ||
		strings.HasPrefix(key, "mr_") || strings.HasPrefix(key, "approvals_")
}

// --- helpers --------------------------------------------------------------

func escBead(id, title string, spec map[string]any) *beadsapi.BeadDetail {
	raw, _ := json.Marshal(spec)
	return &beadsapi.BeadDetail{
		ID:    id,
		Title: title,
		Type:  "escalation",
		Fields: map[string]string{
			"operation_type": "manual_intervention",
			"operation_spec": string(raw),
		},
	}
}

func newTestReaper(gl ReaperGitLabClient, d ReaperBeadClient) *EscalationReaper {
	r := NewEscalationReaper(EscalationReaperConfig{
		GitLab: gl,
		Daemon: d,
		Logger: slog.New(slog.NewTextHandler(nopWriter{}, nil)),
	})
	r.now = func() time.Time { return time.Date(2026, 7, 26, 2, 0, 0, 0, time.UTC) }
	return r
}

// --- THE LOAD-BEARING CORRECTNESS TEST ------------------------------------

// TestReaper_ApplyVerbWithMergedMR_NotReaped is the named counterexample from
// the bug: ka-0000001987 references site-deployment !1168 which IS merged, but
// the ask is to terragrunt apply the merged code in a prod-class account. Its
// operation_spec carries no `action` field (operation_type=infrastructure). A
// naive MR-state reaper would false-reap it and silently drop a live P0. The
// verb allowlist MUST keep it open.
func TestReaper_ApplyVerbWithMergedMR_NotReaped(t *testing.T) {
	gl := newFakeReaperGitLab()
	// !1168 is genuinely merged — exactly the trap.
	mergedBy := "fshot"
	mergedAt := "2026-07-24T20:44:36Z"
	gl.mrs[mrKey("example-org/site-deployment", 1168)] = &GitLabMR{
		IID: 1168, State: "merged",
		MergedBy: &GitLabUser{Username: mergedBy}, MergedAt: &mergedAt,
	}

	// The real ka-0000001987 shape: an apply ask, no `action` verb, merge_request
	// as a bare number and terragrunt commands.
	bead := &beadsapi.BeadDetail{
		ID:    "ka-0000001987",
		Title: "Apply perf-harness terragrunt (site-deployment !1168 / DEVOPS-958) in prod-class perf acct 234567890123",
		Type:  "escalation",
		Fields: map[string]string{
			"operation_type": "infrastructure",
			"operation_spec": `{"repo":"example-org/site-deployment","merge_request":1168,"aws_account":"234567890123","commands":["terragrunt plan","terragrunt apply"]}`,
		},
	}
	d := newFakeReaperDaemon(bead)
	r := newTestReaper(gl, d)

	r.sweepOnce(context.Background())

	if _, closed := d.closed["ka-0000001987"]; closed {
		t.Fatalf("FALSE-REAP: apply-verb escalation with a merged MR was auto-closed — this silently drops a live P0")
	}
	if len(d.comments["ka-0000001987"]) != 0 {
		t.Fatalf("apply-verb escalation should not even be commented on; got %v", d.comments["ka-0000001987"])
	}
	// And the MR endpoint must never have been consulted — the verb guard
	// short-circuits BEFORE any GitLab call.
	if n := gl.getCalls[mrKey("example-org/site-deployment", 1168)]; n != 0 {
		t.Fatalf("verb guard should short-circuit before GitLab; GetMergeRequest was called %d times", n)
	}
}

// --- merge verbs: MR state IS the completion signal -----------------------

func TestReaper_MergeVerb_MergedMR_Reaped(t *testing.T) {
	gl := newFakeReaperGitLab()
	mergedBy := "matthewbaker"
	mergedAt := "2026-07-21T19:14:10Z"
	gl.mrs[mrKey("example-org/gardenio", 1896)] = &GitLabMR{
		IID: 1896, State: "merged",
		WebURL:   "https://gitlab.com/example-org/gardenio/-/merge_requests/1896",
		MergedBy: &GitLabUser{Username: mergedBy}, MergedAt: &mergedAt,
	}
	bead := escBead("gd-BjqfzsOtJz", "merge MR !1896", map[string]any{
		"action":  "merge_mr",
		"iid":     1896,
		"project": "example-org/gardenio",
		"mr_url":  "https://gitlab.com/example-org/gardenio/-/merge_requests/1896",
	})
	d := newFakeReaperDaemon(bead)
	r := newTestReaper(gl, d)

	r.sweepOnce(context.Background())

	cf, closed := d.closed["gd-BjqfzsOtJz"]
	if !closed {
		t.Fatal("merge_mr escalation with merged MR should have been reaped")
	}
	if cf["result_status"] != "success" {
		t.Errorf("result_status = %q, want success", cf["result_status"])
	}
	if cf["mr_merged_by"] != mergedBy {
		t.Errorf("mr_merged_by = %q, want %q", cf["mr_merged_by"], mergedBy)
	}
	if cf["mr_merged_at"] != mergedAt {
		t.Errorf("mr_merged_at = %q, want %q", cf["mr_merged_at"], mergedAt)
	}
	if cf["reaped_satisfier"] != mergedBy {
		t.Errorf("reaped_satisfier = %q, want %q (the out-of-band satisfier)", cf["reaped_satisfier"], mergedBy)
	}
	if cf["reaped"] != "true" {
		t.Errorf("reaped = %q, want true", cf["reaped"])
	}
	// Audit: comment records merged_by/merged_at.
	comments := d.comments["gd-BjqfzsOtJz"]
	if len(comments) != 1 {
		t.Fatalf("want 1 audit comment, got %d", len(comments))
	}
	if !strings.Contains(comments[0], mergedBy) || !strings.Contains(comments[0], mergedAt) {
		t.Errorf("audit comment missing merged_by/merged_at: %q", comments[0])
	}
}

// TestReaper_MergeFamilyOpType_NoAction_MergedMR_Reaped is the LOAD-BEARING
// regression for bug ka-0000006698: a `gitlab_mr_merge`-family escalation whose
// operation_spec carries NO canonical `action` verb.
//
// The boot prompt tells a crew agent parked on an unmerged MR to file a
// `gitlab_mr_merge` escalation as its re-entry handle. Measured 2026-08-16: 17
// of 19 open merge escalations had NO operation_spec.action at all (the rest
// used non-canonical spellings like "merge"). Because the reaper keyed
// reapability solely on operation_spec.action ∈ reapableActions, every one of
// them fell through to flagMRLandedApplyPending and stayed open forever after
// its MR merged — the "56 dead merge escalations, nothing closed them, worst
// offenders sat ~4 months" symptom the bug reports. The fix recognizes the
// merge-family operation_type (isSelfExecMergeEscalation) and synthesizes a
// merge_mr verb, so MR state=merged reaps it exactly like an explicit verb.
func TestReaper_MergeFamilyOpType_NoAction_MergedMR_Reaped(t *testing.T) {
	gl := newFakeReaperGitLab()
	mergedBy := "matthewbaker"
	mergedAt := "2026-04-22T19:14:10Z"
	proj := "example-org/gardenio"
	gl.mrs[mrKey(proj, 1228)] = &GitLabMR{
		IID: 1228, State: "merged",
		WebURL:   "https://gitlab.com/example-org/gardenio/-/merge_requests/1228",
		MergedBy: &GitLabUser{Username: mergedBy}, MergedAt: &mergedAt,
	}
	// The real dead-handle shape: a gitlab_mr_merge escalation whose spec has an
	// mr_iid + project but NO `action` field at all.
	raw, _ := json.Marshal(map[string]any{
		"mr_iid":  1228,
		"mr_url":  "https://gitlab.com/example-org/gardenio/-/merge_requests/1228",
		"project": proj,
	})
	bead := &beadsapi.BeadDetail{
		ID:    "gd-deadmerge",
		Title: "Merge gardenio !1228 (protected main, bot cannot self-approve)",
		Type:  "escalation",
		Fields: map[string]string{
			"operation_type": "gitlab_mr_merge",
			"operation_spec": string(raw),
		},
	}
	d := newFakeReaperDaemon(bead)
	r := newTestReaper(gl, d)

	r.sweepOnce(context.Background())

	cf, closed := d.closed["gd-deadmerge"]
	if !closed {
		t.Fatal("gitlab_mr_merge escalation with a merged MR and no operation_spec.action should have been reaped")
	}
	if cf["result_status"] != "success" {
		t.Errorf("result_status = %q, want success", cf["result_status"])
	}
	if cf["mr_state"] != "merged" {
		t.Errorf("mr_state = %q, want merged", cf["mr_state"])
	}
	if cf["reaped_satisfier"] != mergedBy {
		t.Errorf("reaped_satisfier = %q, want %q", cf["reaped_satisfier"], mergedBy)
	}
	if cf["reaped_action"] != "merge_mr" {
		t.Errorf("reaped_action = %q, want merge_mr (synthesized from the merge-family operation_type)", cf["reaped_action"])
	}
}

// TestReaper_MergeFamilyOpType_OpenMR_NotReaped guards the other direction: a
// merge-family escalation whose MR is still OPEN must stay open — the synthesized
// verb only reaps on a genuinely-landed MR, never on the mere presence of the
// merge-family operation_type.
func TestReaper_MergeFamilyOpType_OpenMR_NotReaped(t *testing.T) {
	gl := newFakeReaperGitLab()
	proj := "example-org/gardenio"
	gl.mrs[mrKey(proj, 7424)] = &GitLabMR{IID: 7424, State: "opened"}
	raw, _ := json.Marshal(map[string]any{"mr_iid": 7424, "project": proj})
	bead := &beadsapi.BeadDetail{
		ID:    "gd-openmerge",
		Title: "Merge gardenio !7424",
		Type:  "escalation",
		Fields: map[string]string{
			"operation_type": "gitlab_mr_merge",
			"operation_spec": string(raw),
		},
	}
	d := newFakeReaperDaemon(bead)
	r := newTestReaper(gl, d)

	r.sweepOnce(context.Background())

	if _, closed := d.closed["gd-openmerge"]; closed {
		t.Fatal("gitlab_mr_merge escalation with an OPEN MR must stay open")
	}
}

// TestReaper_MergeFamilyOpType_EmptySpec_ShortNameTitle_Reaped is the regression
// for bug ka-0000006851: the self-exec merge escalations a crew agent files as its
// re-entry handle carry operation_spec={} and name their MR ONLY as a short-name
// bang-reference in the TITLE ("gardenio !7656") — no structured project+iid, no
// mr_url. Measured 2026-08-25: the ENTIRE open gardenio merge-escalation queue with
// empty specs stayed open forever after its MR merged, because resolveMRRef cannot
// resolve a short repo name into a project path. The short-name fallback expands
// "gardenio" → "example-org/gardenio" under the group namespace, so the
// merge-state check fires and the escalation is reaped exactly like a structured
// one.
func TestReaper_MergeFamilyOpType_EmptySpec_ShortNameTitle_Reaped(t *testing.T) {
	gl := newFakeReaperGitLab()
	mergedBy := "group_14176856_bot_x"
	mergedAt := "2026-08-24T17:15:00Z"
	proj := "example-org/gardenio"
	gl.mrs[mrKey(proj, 7656)] = &GitLabMR{
		IID: 7656, State: "merged",
		WebURL:   "https://gitlab.com/example-org/gardenio/-/merge_requests/7656",
		MergedBy: &GitLabUser{Username: mergedBy}, MergedAt: &mergedAt,
	}
	// The real dead-handle shape: operation_spec is empty and the ONLY MR
	// reference is the short-name bang-ref in the title.
	bead := &beadsapi.BeadDetail{
		ID:    "gd-shortname",
		Title: "Approve+merge gardenio !7656 — intake-groomer hard-skip rules (ka-0000004440)",
		Type:  "escalation",
		Fields: map[string]string{
			"operation_type": "gitlab_mr_merge",
			"operation_spec": "{}",
		},
	}
	d := newFakeReaperDaemon(bead)
	r := newTestReaper(gl, d)

	r.sweepOnce(context.Background())

	cf, closed := d.closed["gd-shortname"]
	if !closed {
		t.Fatal("empty-spec gitlab_mr_merge escalation whose title names a merged short-name MR should have been reaped")
	}
	if cf["mr_state"] != "merged" {
		t.Errorf("mr_state = %q, want merged", cf["mr_state"])
	}
	if cf["reaped_action"] != "merge_mr" {
		t.Errorf("reaped_action = %q, want merge_mr", cf["reaped_action"])
	}
	if cf["reaped_satisfier"] != mergedBy {
		t.Errorf("reaped_satisfier = %q, want %q", cf["reaped_satisfier"], mergedBy)
	}
	// The reaper must have GET the expanded full path, not the bare short name.
	if gl.getCalls[mrKey(proj, 7656)] == 0 {
		t.Errorf("expected a GET on the expanded path %s!7656, calls=%v", proj, gl.getCalls)
	}
}

// TestReaper_MergeFamilyOpType_EmptySpec_ShortNameTitle_OpenMR_NotReaped guards
// the other direction for the short-name fallback: an empty-spec merge escalation
// whose short-name MR is still OPEN stays open (ka-0000006851). The fallback only
// FINDS the MR; the live merge-state check is still authoritative.
func TestReaper_MergeFamilyOpType_EmptySpec_ShortNameTitle_OpenMR_NotReaped(t *testing.T) {
	gl := newFakeReaperGitLab()
	proj := "example-org/gardenio"
	gl.mrs[mrKey(proj, 7698)] = &GitLabMR{IID: 7698, State: "opened"}
	bead := &beadsapi.BeadDetail{
		ID:    "gd-shortname-open",
		Title: "Approve+merge gardenio !7698 — pg_trgm + GIN trgm indexes",
		Type:  "escalation",
		Fields: map[string]string{
			"operation_type": "gitlab_mr_merge",
			"operation_spec": "{}",
		},
	}
	d := newFakeReaperDaemon(bead)
	r := newTestReaper(gl, d)

	r.sweepOnce(context.Background())

	if _, closed := d.closed["gd-shortname-open"]; closed {
		t.Fatal("empty-spec merge escalation with an OPEN short-name MR must stay open")
	}
}

// TestReaper_ShortNameFallback_WrongRepo404_LeavesOpen proves the fallback is safe
// against a mis-guessed repo: a title naming a repo that does not exist (or whose
// iid does not exist there) 404s on the live GET, and the reaper leaves the
// escalation open rather than manufacturing a merge from a bad guess
// (ka-0000006851 / advice ka-0000001276). Here the fake has no MR for the
// expanded path, so GetMergeRequestByPath returns a 404 error.
func TestReaper_ShortNameFallback_WrongRepo404_LeavesOpen(t *testing.T) {
	gl := newFakeReaperGitLab()
	bead := &beadsapi.BeadDetail{
		ID:    "gd-shortname-404",
		Title: "Approve+merge nonesuch !999 — a repo that does not exist",
		Type:  "escalation",
		Fields: map[string]string{
			"operation_type": "gitlab_mr_merge",
			"operation_spec": "{}",
		},
	}
	d := newFakeReaperDaemon(bead)
	r := newTestReaper(gl, d)

	r.sweepOnce(context.Background())

	if _, closed := d.closed["gd-shortname-404"]; closed {
		t.Fatal("a 404 on the expanded short-name path must leave the escalation open, never reap it")
	}
}

// TestResolveMergeShortNameRef unit-tests the short-name expansion against the
// exact title shapes the live board carries (ka-0000006851). It also asserts the
// namespace-empty disable and the no-match cases.
func TestResolveMergeShortNameRef(t *testing.T) {
	r := &EscalationReaper{groupNamespace: "example-org"}
	cases := []struct {
		name     string
		title    string
		wantOK   bool
		wantPath string
		wantIID  int
	}{
		{"space bang", "Approve+merge gardenio !7656 — intake-groomer", true, "example-org/gardenio", 7656},
		{"tight bang", "Approve+merge site-deployment!1228 (keycloak)", true, "example-org/site-deployment", 1228},
		{"MR literal", "Approve+merge fluentbit MR !7 (VictoriaLogs)", true, "example-org/fluentbit", 7},
		{"hyphenated repo", "Merge acme-helm-chart !450 — backfill", true, "example-org/acme-helm-chart", 450},
		{"monorepo space", "Merge monorepo !2527 (FSM-526)", true, "example-org/monorepo", 2527},
		{"no bang ref", "Group B consolidated (109 items, re-scoped): approvals", false, "", 0},
		{"empty title", "", false, "", 0},
		// Bare "!iid" titles whose token-before-! is a verb, not a repo. These must
		// NOT expand to example-org/merge etc. — they carry no resolvable repo.
		{"bare iid after merge", "Approve + merge !2578 — ci-e2e merge-train diff base fix", false, "", 0},
		{"bare iid leading Merge", "Merge !2490 — coverage floors get their first CI caller", false, "", 0},
		{"bare iid after review", "Review !999 — something", false, "", 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ref, ok := r.resolveMergeShortNameRef(tc.title)
			if ok != tc.wantOK {
				t.Fatalf("ok = %v, want %v (ref=%+v)", ok, tc.wantOK, ref)
			}
			if !tc.wantOK {
				return
			}
			if ref.ProjectPath != tc.wantPath {
				t.Errorf("ProjectPath = %q, want %q", ref.ProjectPath, tc.wantPath)
			}
			if ref.IID != tc.wantIID {
				t.Errorf("IID = %d, want %d", ref.IID, tc.wantIID)
			}
			if ref.Host != "" {
				t.Errorf("Host = %q, want empty (structured ref, no host)", ref.Host)
			}
		})
	}

	// Empty namespace disables the fallback entirely.
	disabled := &EscalationReaper{groupNamespace: ""}
	if _, ok := disabled.resolveMergeShortNameRef("Approve+merge gardenio !7656"); ok {
		t.Error("empty groupNamespace must disable the short-name fallback")
	}
}

// TestReaper_GenericCloseOfOpenEscalation_Rejected is the LOAD-BEARING regression
// test for ka-0000000207. The reaper shipped calling the GENERIC bead-close path
// (r.daemon.CloseBead); commit 93d585f25 then made the daemon 409 any generic
// close of an OPEN escalation, silently decapitating the feature — 34/34 close
// attempts failed in prod, 0 reaps. The pre-existing tests passed straight through
// that regression because their stub let a generic close succeed and never
// reproduced the 409.
//
// This test closes that gap two ways:
//  1. The interface guard: ReaperBeadClient no longer exposes CloseBead, so the
//     reaper CANNOT compile against the 409-dead path. (Compile-time; this test
//     documents the runtime consequence.)
//  2. The fake's CloseBead now 409s exactly like the daemon — and the reaper still
//     reaches closed, proving it took the dedicated result verb, not the generic
//     path. If a future change re-points the reaper at CloseBead, this fatals.
func TestReaper_GenericCloseOfOpenEscalation_Rejected(t *testing.T) {
	// Sanity: the fake models the daemon's 409 for a generic close of an OPEN
	// escalation. If this ever starts succeeding, the "reaches closed" assertion
	// below would be vacuous — the exact way the original regression hid.
	d0 := newFakeReaperDaemon()
	if err := d0.CloseBead(context.Background(), "gd-open", nil); err == nil {
		t.Fatal("fake CloseBead must reject a generic close of an open escalation (the daemon 409); " +
			"without this fidelity the result-verb assertions are vacuous")
	}

	gl := newFakeReaperGitLab()
	mergedBy := "matthewbaker"
	mergedAt := "2026-07-21T19:14:10Z"
	gl.mrs[mrKey("example-org/gardenio", 1896)] = &GitLabMR{
		IID: 1896, State: "merged",
		MergedBy: &GitLabUser{Username: mergedBy}, MergedAt: &mergedAt,
	}
	bead := escBead("gd-regress", "merge MR !1896", map[string]any{
		"action":  "merge_mr",
		"iid":     1896,
		"project": "example-org/gardenio",
	})
	d := newFakeReaperDaemon(bead)
	r := newTestReaper(gl, d)

	r.sweepOnce(context.Background())

	// It reached closed — via the dedicated result verb, since the generic path
	// 409s and is not even reachable through the narrowed interface.
	cf, closed := d.closed["gd-regress"]
	if !closed {
		t.Fatal("reaper failed to close a satisfied escalation — the result-verb close path regressed")
	}
	if d.resultCalls["gd-regress"] != 1 {
		t.Fatalf("CaptureEscalationResult called %d times, want exactly 1 (the dedicated result verb is the only close path)", d.resultCalls["gd-regress"])
	}
	// The render/audit trail must survive the transport swap: approved_by keeps
	// the Slack card green ("Complete"), and reaped=true keeps the reap
	// attributable + idempotent. These land as TOP-LEVEL bead fields (via the
	// result verb's Fields pass-through), not buried inside the result blob.
	if cf["result_status"] != "success" {
		t.Errorf("result_status = %q, want success", cf["result_status"])
	}
	if cf["reaped_satisfier"] != mergedBy {
		t.Errorf("reaped_satisfier = %q, want %q (the out-of-band satisfier)", cf["reaped_satisfier"], mergedBy)
	}
	if cf["reaped"] != "true" {
		t.Errorf("reaped = %q, want true (fleet-wide reap audit + idempotency guard)", cf["reaped"])
	}
}

func TestReaper_MergeVerb_OpenMR_NotReaped(t *testing.T) {
	gl := newFakeReaperGitLab()
	gl.mrs[mrKey("example-org/site-deployment", 1180)] = &GitLabMR{
		IID: 1180, State: "opened",
	}
	bead := escBead("gd-e3aBaUQxrs", "Approve + merge !1180", map[string]any{
		"action":  "approve_and_merge_mr",
		"iid":     1180,
		"project": "example-org/site-deployment",
	})
	d := newFakeReaperDaemon(bead)
	r := newTestReaper(gl, d)

	r.sweepOnce(context.Background())

	if _, closed := d.closed["gd-e3aBaUQxrs"]; closed {
		t.Fatal("approve_and_merge_mr escalation with an OPEN MR must stay open")
	}
}

func TestReaper_MergeVerb_ClosedMR_Reaped(t *testing.T) {
	// gd-FgIc3MAf9A: !433 was CLOSED/declined (superseded). A merge ask on a
	// closed MR is moot → reap.
	gl := newFakeReaperGitLab()
	gl.mrs[mrKey("example-org/acme-helm-chart", 433)] = &GitLabMR{
		IID: 433, State: "closed",
	}
	bead := escBead("gd-FgIc3MAf9A", "re-approve !433", map[string]any{
		"action":  "approve_and_merge_mr",
		"iid":     433,
		"project": "example-org/acme-helm-chart",
	})
	d := newFakeReaperDaemon(bead)
	r := newTestReaper(gl, d)

	r.sweepOnce(context.Background())

	cf, closed := d.closed["gd-FgIc3MAf9A"]
	if !closed {
		t.Fatal("merge ask on a closed/declined MR should be reaped (moot)")
	}
	if cf["mr_state"] != "closed" {
		t.Errorf("mr_state = %q, want closed", cf["mr_state"])
	}
}

// --- approve_only: MR state is the WRONG signal; approvals endpoint is right -

// TestReaper_ApproveOnly_DraftUnmerged_Approved_Reaped is the ka-0000000844
// shape: the expected output is "!1181 carries 1 approval and REMAINS in DRAFT
// (not merged)". A state=merged reaper never closes it. approvals_left==0 must.
func TestReaper_ApproveOnly_DraftUnmerged_Approved_Reaped(t *testing.T) {
	gl := newFakeReaperGitLab()
	proj := "example-org/site-deployment"
	// The MR is still opened + draft — deliberately NOT merged.
	gl.mrs[mrKey(proj, 1181)] = &GitLabMR{IID: 1181, State: "opened", Draft: true}
	// But it now carries its 1 required approval.
	gl.approvals[mrKey(proj, 1181)] = &GitLabMRApprovals{
		Approved: true, ApprovalsRequired: 1, ApprovalsLeft: 0,
		ApprovedBy: []string{"matthewbaker"},
	}
	bead := escBead("ka-0000000844", "Approve (do NOT merge) !1181", map[string]any{
		"action":  "approve_only_do_not_merge",
		"iid":     1181,
		"project": proj,
	})
	d := newFakeReaperDaemon(bead)
	r := newTestReaper(gl, d)

	r.sweepOnce(context.Background())

	cf, closed := d.closed["ka-0000000844"]
	if !closed {
		t.Fatal("approve_only escalation with approvals_left==0 should be reaped even though MR is unmerged")
	}
	if cf["reaped_signal"] != "approvals" {
		t.Errorf("reaped_signal = %q, want approvals (NOT mr_state)", cf["reaped_signal"])
	}
	if cf["mr_approved_by"] != "matthewbaker" {
		t.Errorf("mr_approved_by = %q, want matthewbaker", cf["mr_approved_by"])
	}
	// The merge endpoint must NEVER be consulted for an approve_only ask —
	// otherwise a not-yet-merged draft would read as unsatisfied.
	if n := gl.getCalls[mrKey(proj, 1181)]; n != 0 {
		t.Errorf("approve_only must not call GetMergeRequest; called %d times", n)
	}
	if n := gl.apprCalls[mrKey(proj, 1181)]; n == 0 {
		t.Error("approve_only must consult the approvals endpoint")
	}
}

func TestReaper_ApproveOnly_NotYetApproved_NotReaped(t *testing.T) {
	gl := newFakeReaperGitLab()
	proj := "example-org/site-deployment"
	gl.approvals[mrKey(proj, 1181)] = &GitLabMRApprovals{
		Approved: false, ApprovalsRequired: 1, ApprovalsLeft: 1,
	}
	bead := escBead("ka-0000000844", "Approve (do NOT merge) !1181", map[string]any{
		"action":  "approve_only_do_not_merge",
		"iid":     1181,
		"project": proj,
	})
	d := newFakeReaperDaemon(bead)
	r := newTestReaper(gl, d)

	r.sweepOnce(context.Background())

	if _, closed := d.closed["ka-0000000844"]; closed {
		t.Fatal("approve_only escalation with approvals_left>0 must stay open")
	}
}

// TestReaper_ApproveOnly_CommunityEdition404_NotReaped guards the zero-value
// struct the real client returns on a CE 404 (required==0, left==0): that must
// NOT read as "approved" and false-reap.
func TestReaper_ApproveOnly_CommunityEdition404_NotReaped(t *testing.T) {
	gl := newFakeReaperGitLab()
	proj := "example-org/site-deployment"
	// No approvals entry → fake returns zero-value struct, like a CE 404.
	bead := escBead("ka-0000000844", "Approve (do NOT merge) !1181", map[string]any{
		"action":  "approve_only_do_not_merge",
		"iid":     1181,
		"project": proj,
	})
	d := newFakeReaperDaemon(bead)
	r := newTestReaper(gl, d)

	r.sweepOnce(context.Background())

	if _, closed := d.closed["ka-0000000844"]; closed {
		t.Fatal("approve_only with a zero-value (CE-404) approvals response must NOT be reaped")
	}
}

// --- coverage: iid discovery via structured fields, mr_url, and prose --------

func TestReaper_IIDFromMRURLField(t *testing.T) {
	gl := newFakeReaperGitLab()
	proj := "example-org/site-deployment"
	gl.mrs[mrKey(proj, 1178)] = &GitLabMR{IID: 1178, State: "merged"}
	// No structured iid/project — only mr_url. The URL yields host+path+iid.
	bead := escBead("gd-9o47cirNlN", "Approve + merge !1178", map[string]any{
		"action": "approve_and_merge_mr",
		"mr_url": "https://gitlab.com/example-org/site-deployment/-/merge_requests/1178",
	})
	d := newFakeReaperDaemon(bead)
	r := newTestReaper(gl, d)

	r.sweepOnce(context.Background())

	if _, closed := d.closed["gd-9o47cirNlN"]; !closed {
		t.Fatal("should resolve iid+project from mr_url and reap the merged MR")
	}
}

func TestReaper_IIDFromTitleProse(t *testing.T) {
	// The "MR-shaped in prose only" case: iid lives in the title as `!1178`,
	// project path in a structured field, no iid field, no mr_url.
	gl := newFakeReaperGitLab()
	proj := "example-org/site-deployment"
	gl.mrs[mrKey(proj, 1178)] = &GitLabMR{IID: 1178, State: "merged"}
	bead := escBead("gd-9o47cirNlN",
		"Approve + merge site-deployment merge_requests/1178 (run-role tag comma fix)",
		map[string]any{
			"action":  "approve_and_merge_mr",
			"project": proj,
		})
	d := newFakeReaperDaemon(bead)
	r := newTestReaper(gl, d)

	r.sweepOnce(context.Background())

	if _, closed := d.closed["gd-9o47cirNlN"]; !closed {
		t.Fatal("should parse iid from merge_requests/<iid> prose and reap")
	}
}

// TestReaper_StructuredProjectWinsOverForeignProseURL is the LOAD-BEARING
// regression for bug ka-0000000198: the reaper must key the MR state check on
// the AUTHORITATIVE structured (project, iid) from operation_spec, never let a
// merge_requests/<iid> URL scraped from prose redirect it to a DIFFERENT
// project.
//
// The real failure: an escalation for site-deployment!1230 (genuinely open) was
// falsely reaped 3x because its title/description mentioned a colliding
// monorepo!1230 URL (a different project's MR that happened to be closed). GitLab
// IIDs are per-project, so keying on the bare IID against the wrong project
// resolved to the closed monorepo MR and marked the site-deployment ask moot.
// The fix makes the structured project+iid win, so prose cannot hijack the check.
func TestReaper_StructuredProjectWinsOverForeignProseURL(t *testing.T) {
	gl := newFakeReaperGitLab()
	authoritative := "example-org/site-deployment"
	foreign := "example-org/monorepo"
	// The AUTHORITATIVE MR (the actual ask) is still open — must NOT be reaped.
	gl.mrs[mrKey(authoritative, 1230)] = &GitLabMR{
		IID: 1230, State: "opened",
		WebURL: "https://gitlab.com/example-org/site-deployment/-/merge_requests/1230",
	}
	// The FOREIGN colliding MR (same IID, different project) is closed. If prose
	// wins, the reaper would GET this one, see "closed", and falsely reap.
	gl.mrs[mrKey(foreign, 1230)] = &GitLabMR{
		IID: 1230, State: "closed",
		WebURL: "https://gitlab.com/example-org/monorepo/-/merge_requests/1230",
	}
	bead := escBead("gd-siteDeploy1230",
		// The colliding foreign-project URL sits in the prose, exactly as the real
		// escalation bodies did (e.g. an "orthogonal to monorepo!1230" note).
		"Approve + merge site-deployment!1230 — orthogonal to https://gitlab.com/example-org/monorepo/-/merge_requests/1230",
		map[string]any{
			"action":  "merge_mr",
			"mr_iid":  1230,
			"project": authoritative,
		})
	bead.Description = "Depends on / is orthogonal to monorepo/-/merge_requests/1230; land independently."
	d := newFakeReaperDaemon(bead)
	r := newTestReaper(gl, d)

	r.sweepOnce(context.Background())

	if _, closed := d.closed["gd-siteDeploy1230"]; closed {
		t.Fatal("must NOT reap: the authoritative site-deployment!1230 is open; a closed foreign-project monorepo!1230 URL in prose must not redirect the state check")
	}
	// Positive proof the RIGHT project was queried, and the wrong one never was.
	if n := gl.getCalls[mrKey(authoritative, 1230)]; n == 0 {
		t.Error("reaper never checked the authoritative site-deployment MR")
	}
	if n := gl.getCalls[mrKey(foreign, 1230)]; n != 0 {
		t.Errorf("reaper checked the FOREIGN monorepo MR %d time(s) — prose hijacked the project resolution", n)
	}
}

func TestReaper_ReapableVerbButNoMRRef_NotReaped(t *testing.T) {
	gl := newFakeReaperGitLab()
	bead := escBead("gd-noref", "merge something", map[string]any{
		"action": "merge_mr",
		// no iid, no project, no mr_url, nothing parseable in title
	})
	d := newFakeReaperDaemon(bead)
	r := newTestReaper(gl, d)

	r.sweepOnce(context.Background())

	if _, closed := d.closed["gd-noref"]; closed {
		t.Fatal("reapable verb but no resolvable MR reference → must stay open")
	}
}

// --- idempotency ----------------------------------------------------------

func TestReaper_Idempotent_SecondSweepNoOp(t *testing.T) {
	gl := newFakeReaperGitLab()
	proj := "example-org/gardenio"
	mergedAt := "2026-07-21T19:14:10Z"
	gl.mrs[mrKey(proj, 1896)] = &GitLabMR{
		IID: 1896, State: "merged",
		MergedBy: &GitLabUser{Username: "matthewbaker"}, MergedAt: &mergedAt,
	}
	bead := escBead("gd-BjqfzsOtJz", "merge !1896", map[string]any{
		"action": "merge_mr", "iid": 1896, "project": proj,
	})
	d := newFakeReaperDaemon(bead)
	r := newTestReaper(gl, d)

	r.sweepOnce(context.Background())
	r.sweepOnce(context.Background()) // second sweep: bead is now closed → dropped

	if got := len(d.comments["gd-BjqfzsOtJz"]); got != 1 {
		t.Fatalf("idempotency: want exactly 1 audit comment across two sweeps, got %d", got)
	}
}

// TestReaper_Idempotent_ReapedFlagGuard covers the belt-and-suspenders guard:
// even if a reaped bead somehow reappears in the open list, the reaped=true
// field stops a second action (ka-0000004105).
func TestReaper_Idempotent_ReapedFlagGuard(t *testing.T) {
	gl := newFakeReaperGitLab()
	proj := "example-org/gardenio"
	gl.mrs[mrKey(proj, 1896)] = &GitLabMR{IID: 1896, State: "merged"}
	bead := escBead("gd-x", "merge !1896", map[string]any{
		"action": "merge_mr", "iid": 1896, "project": proj,
	})
	bead.Fields["reaped"] = "true" // already reaped
	d := newFakeReaperDaemon(bead)
	r := newTestReaper(gl, d)

	r.sweepOnce(context.Background())

	if _, closed := d.closed["gd-x"]; closed {
		t.Fatal("a bead already stamped reaped=true must not be re-closed")
	}
}

// --- safety: E2E fixtures, non-reapable verbs, dry-run --------------------

func TestReaper_E2EFixture_Skipped(t *testing.T) {
	gl := newFakeReaperGitLab()
	proj := "example-org/gardenio"
	gl.mrs[mrKey(proj, 1896)] = &GitLabMR{IID: 1896, State: "merged"}
	bead := escBead("gd-e2e", "merge !1896", map[string]any{
		"action": "merge_mr", "iid": 1896, "project": proj,
	})
	bead.Labels = []string{"e2e:true"}
	d := newFakeReaperDaemon(bead)
	r := newTestReaper(gl, d)

	r.sweepOnce(context.Background())

	if _, closed := d.closed["gd-e2e"]; closed {
		t.Fatal("E2E fixture escalation must never be auto-reaped against real GitLab")
	}
}

func TestReaper_UnknownVerb_NotReaped(t *testing.T) {
	gl := newFakeReaperGitLab()
	proj := "example-org/site-deployment"
	gl.mrs[mrKey(proj, 1200)] = &GitLabMR{IID: 1200, State: "merged"}
	for _, action := range []string{"deploy", "provision_iam", "mint_secret", "terragrunt_apply", ""} {
		bead := escBead("gd-"+action, "do "+action, map[string]any{
			"action": action, "iid": 1200, "project": proj,
		})
		d := newFakeReaperDaemon(bead)
		r := newTestReaper(gl, d)
		r.sweepOnce(context.Background())
		if _, closed := d.closed["gd-"+action]; closed {
			t.Fatalf("verb %q is not on the allowlist and must not be reaped", action)
		}
	}
}

func TestReaper_DryRun_ChecksButDoesNotClose(t *testing.T) {
	gl := newFakeReaperGitLab()
	proj := "example-org/gardenio"
	gl.mrs[mrKey(proj, 1896)] = &GitLabMR{IID: 1896, State: "merged"}
	bead := escBead("gd-dry", "merge !1896", map[string]any{
		"action": "merge_mr", "iid": 1896, "project": proj,
	})
	d := newFakeReaperDaemon(bead)
	r := NewEscalationReaper(EscalationReaperConfig{
		GitLab: gl, Daemon: d, DryRun: true,
		Logger: slog.New(slog.NewTextHandler(nopWriter{}, nil)),
	})

	r.sweepOnce(context.Background())

	if _, closed := d.closed["gd-dry"]; closed {
		t.Fatal("dry-run must not close beads")
	}
	if len(d.comments["gd-dry"]) != 0 {
		t.Fatal("dry-run must not comment")
	}
}

// --- host scoping ---------------------------------------------------------

func TestReaper_ForeignHost_Skipped(t *testing.T) {
	gl := newFakeReaperGitLab()
	// The MR URL is on gitlab.com, but the reaper is scoped to a dark host.
	bead := escBead("gd-foreign", "merge foreign MR", map[string]any{
		"action": "merge_mr",
		"mr_url": "https://gitlab.com/example-org/gardenio/-/merge_requests/1896",
	})
	d := newFakeReaperDaemon(bead)
	r := NewEscalationReaper(EscalationReaperConfig{
		GitLab: gl, Daemon: d,
		Host:   "gitlab-dark.gardenio-e2e.app.e2e.dev.acme.example",
		Logger: slog.New(slog.NewTextHandler(nopWriter{}, nil)),
	})

	r.sweepOnce(context.Background())

	if _, closed := d.closed["gd-foreign"]; closed {
		t.Fatal("MR on a foreign host must be skipped under host scoping")
	}
	if n := gl.getCalls[mrKey("example-org/gardenio", 1896)]; n != 0 {
		t.Fatalf("foreign-host MR must not be queried; called %d times", n)
	}
}

func TestReaper_MatchingHost_Reaped(t *testing.T) {
	gl := newFakeReaperGitLab()
	proj := "example-org/gardenio"
	gl.mrs[mrKey(proj, 1896)] = &GitLabMR{IID: 1896, State: "merged"}
	bead := escBead("gd-samehost", "merge MR", map[string]any{
		"action": "merge_mr",
		"mr_url": "https://gitlab.com/example-org/gardenio/-/merge_requests/1896",
	})
	d := newFakeReaperDaemon(bead)
	r := NewEscalationReaper(EscalationReaperConfig{
		GitLab: gl, Daemon: d, Host: "gitlab.com",
		Logger: slog.New(slog.NewTextHandler(nopWriter{}, nil)),
	})

	r.sweepOnce(context.Background())

	if _, closed := d.closed["gd-samehost"]; !closed {
		t.Fatal("MR on the matching host should be reaped")
	}
}

// --- error resilience -----------------------------------------------------

func TestReaper_GitLabError_LeavesOpen(t *testing.T) {
	gl := newFakeReaperGitLab()
	proj := "example-org/gardenio"
	gl.mrErr[mrKey(proj, 1896)] = fmt.Errorf("GitLab 500")
	bead := escBead("gd-err", "merge !1896", map[string]any{
		"action": "merge_mr", "iid": 1896, "project": proj,
	})
	d := newFakeReaperDaemon(bead)
	r := newTestReaper(gl, d)

	r.sweepOnce(context.Background())

	if _, closed := d.closed["gd-err"]; closed {
		t.Fatal("a GitLab error must leave the escalation open (fail safe)")
	}
}

func TestReaper_CloseError_NoCrashComment(t *testing.T) {
	gl := newFakeReaperGitLab()
	proj := "example-org/gardenio"
	gl.mrs[mrKey(proj, 1896)] = &GitLabMR{IID: 1896, State: "merged"}
	bead := escBead("gd-closeerr", "merge !1896", map[string]any{
		"action": "merge_mr", "iid": 1896, "project": proj,
	})
	d := newFakeReaperDaemon(bead)
	d.closeErr["gd-closeerr"] = fmt.Errorf("daemon down")
	r := newTestReaper(gl, d)

	// Must not panic; comment recorded, close attempted.
	r.sweepOnce(context.Background())

	if _, closed := d.closed["gd-closeerr"]; closed {
		t.Fatal("close failed, so bead must not be recorded closed")
	}
}

// --- unit tests for the resolver + field helpers --------------------------

func TestResolveMRRef(t *testing.T) {
	tests := []struct {
		name        string
		spec        map[string]any
		title       string
		description string
		wantOK      bool
		wantProj    string
		wantIID     int
		wantHost    string
	}{
		{
			name:     "structured iid+project",
			spec:     map[string]any{"iid": float64(1180), "project": "grp/proj"},
			wantOK:   true,
			wantProj: "grp/proj", wantIID: 1180,
		},
		{
			name:     "mr_iid alias",
			spec:     map[string]any{"mr_iid": float64(55), "mr_project": "grp/proj"},
			wantOK:   true,
			wantProj: "grp/proj", wantIID: 55,
		},
		{
			name:     "mr_url resolves when no structured project",
			spec:     map[string]any{"mr_url": "https://gitlab.com/grp/proj/-/merge_requests/42"},
			wantOK:   true,
			wantProj: "grp/proj", wantIID: 42, wantHost: "gitlab.com",
		},
		{
			// Bug ka-0000000198: the authoritative structured project+iid must win
			// over a colliding foreign-project MR URL living in prose or mr_url.
			name: "structured project+iid wins over foreign prose url",
			spec: map[string]any{
				"mr_iid":  float64(1230),
				"project": "grp/site-deployment",
				"mr_url":  "https://gitlab.com/grp/monorepo/-/merge_requests/1230",
			},
			title:       "merge grp/monorepo/-/merge_requests/1230 (orthogonal)",
			description: "https://gitlab.com/grp/monorepo/-/merge_requests/1230",
			wantOK:      true,
			wantProj:    "grp/site-deployment", wantIID: 1230, wantHost: "",
		},
		{
			name:     "numeric string iid",
			spec:     map[string]any{"iid": "77", "project": "grp/proj"},
			wantOK:   true,
			wantProj: "grp/proj", wantIID: 77,
		},
		{
			name:     "prose fallback from title",
			spec:     map[string]any{"project": "grp/proj"},
			title:    "fix merge_requests/1178 tag comma",
			wantOK:   true,
			wantProj: "grp/proj", wantIID: 1178,
		},
		{
			name:   "unresolvable",
			spec:   map[string]any{"foo": "bar"},
			wantOK: false,
		},
		{
			name:   "iid without project (and no url) unresolvable",
			spec:   map[string]any{"iid": float64(9)},
			wantOK: false,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ref, ok := resolveMRRef(tt.spec, tt.title, tt.description)
			if ok != tt.wantOK {
				t.Fatalf("ok = %v, want %v", ok, tt.wantOK)
			}
			if !ok {
				return
			}
			if ref.ProjectPath != tt.wantProj {
				t.Errorf("project = %q, want %q", ref.ProjectPath, tt.wantProj)
			}
			if ref.IID != tt.wantIID {
				t.Errorf("iid = %d, want %d", ref.IID, tt.wantIID)
			}
			if ref.Host != tt.wantHost {
				t.Errorf("host = %q, want %q", ref.Host, tt.wantHost)
			}
		})
	}
}

func TestReapableActions_ExactSet(t *testing.T) {
	want := []string{"approve_and_merge_mr", "approve_only_do_not_merge", "merge_mr"}
	if len(reapableActions) != len(want) {
		t.Fatalf("allowlist has %d entries, want %d — a change here changes what gets auto-closed", len(reapableActions), len(want))
	}
	for _, a := range want {
		if !reapableActions[a] {
			t.Errorf("allowlist missing %q", a)
		}
	}
	for _, forbidden := range []string{"deploy", "apply", "terragrunt_apply", "provision", "mint_secret", "infrastructure", ""} {
		if reapableActions[forbidden] {
			t.Errorf("allowlist must NOT contain %q", forbidden)
		}
	}
}

func TestParseOperationSpec_Malformed(t *testing.T) {
	for _, raw := range []string{"", "not json", "[1,2,3]", "null", "42"} {
		if m := parseOperationSpec(raw); m == nil {
			t.Errorf("parseOperationSpec(%q) returned nil map, want empty map", raw)
		}
	}
	m := parseOperationSpec(`{"action":"merge_mr","iid":5}`)
	if stringField(m, "action") != "merge_mr" {
		t.Errorf("action = %q, want merge_mr", stringField(m, "action"))
	}
	if firstIntField(m, "iid") != 5 {
		t.Errorf("iid = %d, want 5", firstIntField(m, "iid"))
	}
}

// ─────────────────────────────────────────────────────────────────────────
// ORCHESTRATION-LAYER detector tests (ka-0000007203).
//
// These exercise the second detector: reap an escalation the operator resolved
// OUT-OF-BAND and recorded on the bead (a result_status field, or a
// human-operator resolve-comment) — no GitLab witness, covering the
// apply/deploy/mint asks the verb allowlist deliberately refuses.

// applyEsc builds a NON-MR apply/deploy escalation (no reapable verb, no MR
// ref) — exactly the shape the MR detector cannot touch, so any reap here comes
// from the orchestration detector.
func applyEsc(id, title string, fields map[string]string) *beadsapi.BeadDetail {
	f := map[string]string{
		"operation_type": "infrastructure",
		"operation_spec": `{"aws_account":"234567890123","commands":["terragrunt apply"]}`,
	}
	for k, v := range fields {
		f[k] = v
	}
	return &beadsapi.BeadDetail{ID: id, Title: title, Type: "escalation", Fields: f}
}

// THE CORE ACCEPTANCE TEST (AC#4): an escalation whose result was recorded
// out-of-band (result_status=success on a still-open bead) is reaped on the
// next sweep, even though it is a non-MR apply ask the verb guard refuses.
func TestReaper_Orchestration_ResultStatusSuccess_Reaped(t *testing.T) {
	gl := newFakeReaperGitLab()
	bead := applyEsc("gd-oob1", "Apply terragrunt in perf acct 909", map[string]string{
		"result_status": "success",
		"executor":      "slack:U0OPERATOR1",
	})
	d := newFakeReaperDaemon(bead)
	r := newTestReaper(gl, d)

	r.sweepOnce(context.Background())

	cf, closed := d.closed["gd-oob1"]
	if !closed {
		t.Fatal("escalation with result_status=success on an open bead must be reaped out-of-band")
	}
	if cf["reaped"] != "true" {
		t.Errorf("reaped = %q, want true", cf["reaped"])
	}
	if cf["reaped_signal"] != "result_status" {
		t.Errorf("reaped_signal = %q, want result_status", cf["reaped_signal"])
	}
	if cf["result_status"] != "success" {
		t.Errorf("result_status = %q, want success", cf["result_status"])
	}
	// reaped_satisfier → the recorded executor.
	if cf["reaped_satisfier"] != "slack:U0OPERATOR1" {
		t.Errorf("reaped_satisfier = %q, want the recorded executor", cf["reaped_satisfier"])
	}
	// No verb was involved — reaped_action must NOT be stamped.
	if _, ok := cf["reaped_action"]; ok {
		t.Errorf("reaped_action must be unset for an orchestration-layer reap, got %q", cf["reaped_action"])
	}
	// An audit comment is left, and the MR endpoint was never consulted.
	if len(d.comments["gd-oob1"]) != 1 {
		t.Fatalf("want 1 audit comment, got %d", len(d.comments["gd-oob1"]))
	}
	if len(gl.getCalls) != 0 {
		t.Errorf("orchestration detector must not call GitLab; got %v", gl.getCalls)
	}
}

// partial is also a terminal, satisfied status.
func TestReaper_Orchestration_ResultStatusPartial_Reaped(t *testing.T) {
	bead := applyEsc("gd-oobp", "Provision half the thing", map[string]string{
		"result_status": "partial",
	})
	d := newFakeReaperDaemon(bead)
	r := newTestReaper(newFakeReaperGitLab(), d)

	r.sweepOnce(context.Background())

	if _, closed := d.closed["gd-oobp"]; !closed {
		t.Fatal("result_status=partial on an open bead should be reaped")
	}
}

// LOAD-BEARING: result_status=failure is the OPPOSITE of satisfied — a failed
// ask is unfinished work and must NOT be auto-closed (buries it).
func TestReaper_Orchestration_ResultStatusFailure_NotReaped(t *testing.T) {
	bead := applyEsc("gd-oobf", "Apply that failed", map[string]string{
		"result_status": "failure",
	})
	d := newFakeReaperDaemon(bead)
	r := newTestReaper(newFakeReaperGitLab(), d)

	r.sweepOnce(context.Background())

	if _, closed := d.closed["gd-oobf"]; closed {
		t.Fatal("result_status=failure must NEVER be reaped — a failed ask is unfinished, not satisfied")
	}
}

// A human-operator comment carrying an explicit resolve directive reaps the
// escalation — the "operator closes it in Slack" path (AC#2/#4).
func TestReaper_Orchestration_OperatorResolveComment_Reaped(t *testing.T) {
	bead := applyEsc("gd-oobc", "Rotate the secret by hand", nil)
	d := newFakeReaperDaemon(bead)
	d.existing["gd-oobc"] = []beadsapi.Comment{
		{Author: "some-agent", Text: "filed this, please rotate"},
		// A genuine operator comment: the AUTHOR is a human shape (email / typed
		// operator id), with the operator's operated_by riding along — the exact
		// shape a real operator reply lands as (verified on ka-0000006977).
		{Author: "matthew.baker@acme.example", OperatedBy: "slack:U0OPERATOR1", Text: "resolved out of band — rotated it in the console just now"},
	}
	r := newTestReaper(newFakeReaperGitLab(), d)

	r.sweepOnce(context.Background())

	cf, closed := d.closed["gd-oobc"]
	if !closed {
		t.Fatal("a human-operator resolve-comment must reap the escalation")
	}
	if cf["reaped_signal"] != "operator_comment" {
		t.Errorf("reaped_signal = %q, want operator_comment", cf["reaped_signal"])
	}
	if cf["reaped_satisfier"] != "slack:U0OPERATOR1" {
		t.Errorf("reaped_satisfier = %q, want the commenting operator", cf["reaped_satisfier"])
	}
}

// LOAD-BEARING: an AGENT comment carrying the directive must NOT reap — only a
// HUMAN operator can resolve out-of-band. Agents file and comment on
// escalations constantly; "resolved out of band" from one is not authority.
func TestReaper_Orchestration_AgentResolveComment_NotReaped(t *testing.T) {
	bead := applyEsc("gd-ooba", "Apply the thing", nil)
	d := newFakeReaperDaemon(bead)
	d.existing["gd-ooba"] = []beadsapi.Comment{
		// A bare agent name is not a human-operator shape.
		{Author: "gardenio-some-crew-agent", Text: "I think this was resolved out of band already?"},
	}
	r := newTestReaper(newFakeReaperGitLab(), d)

	r.sweepOnce(context.Background())

	if _, closed := d.closed["gd-ooba"]; closed {
		t.Fatal("an AGENT comment must never reap — only a human operator can resolve out-of-band")
	}
}

// REGRESSION (ka-0000002488): the exact shape that false-reaped ka-0000006977 —
// an AGENT-authored comment (bare agent name) carrying the operator's human
// operated_by (agents auto-inherit their spawning operator's operated_by). The
// old gate keyed on operated_by and read this as the operator's own comment,
// closing a live merge handle. The author-based gate must reject it.
func TestReaper_Orchestration_AgentCommentWithHumanOperatedBy_NotReaped(t *testing.T) {
	bead := applyEsc("gd-oobag", "Merge the gated branch to main", nil)
	d := newFakeReaperDaemon(bead)
	d.existing["gd-oobag"] = []beadsapi.Comment{
		{
			Author:     "cs-schedule-trigger-17-2-u49w",
			OperatedBy: "slack:U0OPERATOR1",
			Text:       "GARDENER RE-CHECK — resolved the head_sha question out of band already.",
		},
	}
	r := newTestReaper(newFakeReaperGitLab(), d)

	r.sweepOnce(context.Background())

	if _, closed := d.closed["gd-oobag"]; closed {
		t.Fatal("an agent comment carrying a human operated_by must NOT reap — authorship is the author, not operated_by (ka-0000002488)")
	}
}

// REGRESSION (ka-0000002488): even a genuine HUMAN operator comment must not
// reap when its text vetoes the reap — "leave this escalation OPEN and do not
// execute it" is the OPPOSITE of satisfaction and must hard-block, including
// when a resolve-looking phrase co-occurs in the same comment. Fail closed.
func TestReaper_Orchestration_HumanVetoLeaveOpen_NotReaped(t *testing.T) {
	bead := applyEsc("gd-oobv", "Merge the gated branch to main", nil)
	d := newFakeReaperDaemon(bead)
	d.existing["gd-oobv"] = []beadsapi.Comment{
		{
			Author:     "matthew.baker@acme.example",
			OperatedBy: "slack:U0OPERATOR1",
			Text:       "I resolved the sibling one out of band, but leave this escalation OPEN and do not execute it against that head — I'll re-point it.",
		},
	}
	r := newTestReaper(newFakeReaperGitLab(), d)

	r.sweepOnce(context.Background())

	if _, closed := d.closed["gd-oobv"]; closed {
		t.Fatal("a human comment vetoing the reap (\"leave open / do not execute\") must NOT reap — fail closed (ka-0000002488)")
	}
}

// LOAD-BEARING: an operator comment WITHOUT a resolve directive must NOT reap —
// a human musing "should I reap this?" is discussion, not resolution.
func TestReaper_Orchestration_OperatorCommentNoDirective_NotReaped(t *testing.T) {
	bead := applyEsc("gd-oobn", "Apply the thing", nil)
	d := newFakeReaperDaemon(bead)
	d.existing["gd-oobn"] = []beadsapi.Comment{
		{Author: "matthew.baker@acme.example", OperatedBy: "slack:U0OPERATOR1", Text: "looking into this, might do it later"},
	}
	r := newTestReaper(newFakeReaperGitLab(), d)

	r.sweepOnce(context.Background())

	if _, closed := d.closed["gd-oobn"]; closed {
		t.Fatal("an operator comment without a resolve directive must not reap")
	}
}

// The field witness short-circuits before the comment fetch — no GetComments
// round-trip when result_status already settles it.
func TestReaper_Orchestration_FieldWitnessSkipsCommentFetch(t *testing.T) {
	bead := applyEsc("gd-oobs", "Apply", map[string]string{"result_status": "success"})
	d := newFakeReaperDaemon(bead)
	r := newTestReaper(newFakeReaperGitLab(), d)

	r.sweepOnce(context.Background())

	if n := d.getCommCalls["gd-oobs"]; n != 0 {
		t.Errorf("field witness should short-circuit; GetComments called %d times", n)
	}
}

// A comment-fetch error is non-fatal: no comment witness this sweep, bead stays
// open, and the MR path still runs (here there is none, so it stays open).
func TestReaper_Orchestration_CommentFetchError_LeavesOpen(t *testing.T) {
	bead := applyEsc("gd-oobe", "Apply", nil)
	d := newFakeReaperDaemon(bead)
	d.getCommErr["gd-oobe"] = fmt.Errorf("daemon down")
	r := newTestReaper(newFakeReaperGitLab(), d)

	r.sweepOnce(context.Background())

	if _, closed := d.closed["gd-oobe"]; closed {
		t.Fatal("a comment-fetch error must leave the escalation open (under-reap is the safe direction)")
	}
}

// The orchestration detector runs BEFORE the verb guard, so an approve/merge
// escalation the operator recorded as done out-of-band is reaped via the
// orchestration path without a GitLab call — the field witness wins.
func TestReaper_Orchestration_PrecedesMRPath(t *testing.T) {
	gl := newFakeReaperGitLab()
	bead := escBead("gd-oobmr", "merge MR !1896", map[string]any{
		"action":  "merge_mr",
		"iid":     1896,
		"project": "example-org/gardenio",
	})
	bead.Fields["result_status"] = "success"
	d := newFakeReaperDaemon(bead)
	r := newTestReaper(gl, d)

	r.sweepOnce(context.Background())

	cf, closed := d.closed["gd-oobmr"]
	if !closed {
		t.Fatal("should be reaped via the orchestration field witness")
	}
	if cf["reaped_signal"] != "result_status" {
		t.Errorf("reaped_signal = %q, want result_status (orchestration precedes MR path)", cf["reaped_signal"])
	}
	if len(gl.getCalls) != 0 {
		t.Errorf("orchestration detector should short-circuit before GitLab; got %v", gl.getCalls)
	}
}

// Dry-run honours the shared flag for the orchestration path too: it detects
// the witness but neither comments nor closes.
func TestReaper_Orchestration_DryRun_DoesNotClose(t *testing.T) {
	bead := applyEsc("gd-oobd", "Apply", map[string]string{"result_status": "success"})
	d := newFakeReaperDaemon(bead)
	r := NewEscalationReaper(EscalationReaperConfig{
		GitLab: newFakeReaperGitLab(),
		Daemon: d,
		Logger: slog.New(slog.NewTextHandler(nopWriter{}, nil)),
		DryRun: true,
	})
	r.now = func() time.Time { return time.Date(2026, 7, 26, 2, 0, 0, 0, time.UTC) }

	r.sweepOnce(context.Background())

	if _, closed := d.closed["gd-oobd"]; closed {
		t.Fatal("dry-run must not close the escalation")
	}
	if len(d.comments["gd-oobd"]) != 0 {
		t.Fatalf("dry-run must not comment; got %v", d.comments["gd-oobd"])
	}
}

// The orchestration reap is idempotent with the reaped flag, same as the MR
// path: a bead already stamped reaped=true is never re-processed
// (ka-0000007203).
func TestReaper_Orchestration_ReapedFlagGuard(t *testing.T) {
	bead := applyEsc("gd-oobr", "Apply", map[string]string{
		"result_status": "success",
		"reaped":        "true",
	})
	d := newFakeReaperDaemon(bead)
	r := newTestReaper(newFakeReaperGitLab(), d)

	r.sweepOnce(context.Background())

	if _, closed := d.closed["gd-oobr"]; closed {
		t.Fatal("a bead already stamped reaped=true must not be re-processed")
	}
}

func TestOperatorResolveDirective(t *testing.T) {
	fire := []string{
		"/reap",
		"/resolve",
		"resolved out of band",
		"resolved out-of-band",
		"I merged this manually",
		"approved by hand already",
		"handled out of band, closing",
		"applied it myself in the console",
		"already done out of band",
	}
	for _, s := range fire {
		if !operatorResolveDirective.MatchString(s) {
			t.Errorf("directive should MATCH %q", s)
		}
	}
	noFire := []string{
		"done",
		"ok thanks",
		"looking into this",
		"should I reap this?",
		"this needs an out-of-band fix eventually",
		"working on it",
	}
	for _, s := range noFire {
		if operatorResolveDirective.MatchString(s) {
			t.Errorf("directive should NOT match %q", s)
		}
	}
}

func TestOperatorReapVeto(t *testing.T) {
	fire := []string{
		"leave this escalation OPEN and do not execute it against d6af530",
		"do not merge yet",
		"don't merge this",
		"please keep this open for now",
		"this should stay open until the trunk has a verdict",
		"not satisfied yet",
		"still needed",
		"still blocked on the rebase",
		"do not reap",
		"never merge without a re-gate",
	}
	for _, s := range fire {
		if !operatorReapVeto.MatchString(s) {
			t.Errorf("veto should MATCH %q", s)
		}
	}
	noFire := []string{
		"resolved out of band, rotated it in the console",
		"merged this manually",
		"approved by hand already",
		"handled out of band, closing",
		"done out of band",
		"the door is open, come in", // "open" without a keep/leave/stay context
	}
	for _, s := range noFire {
		if operatorReapVeto.MatchString(s) {
			t.Errorf("veto should NOT match %q", s)
		}
	}
}

func TestCommentOperator(t *testing.T) {
	cases := []struct {
		name string
		c    beadsapi.Comment
		want string
	}{
		{"operated_by human wins", beadsapi.Comment{OperatedBy: "slack:U0OPERATOR1", Author: "slack-bridge"}, "slack:U0OPERATOR1"},
		{"author human when no operated_by", beadsapi.Comment{Author: "oidc:matthew.baker@acme.example"}, "oidc:matthew.baker@acme.example"},
		{"agent author returns author (caller gates)", beadsapi.Comment{Author: "gardenio-crew-x"}, "gardenio-crew-x"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := commentOperator(tc.c); got != tc.want {
				t.Errorf("commentOperator = %q, want %q", got, tc.want)
			}
		})
	}
}
