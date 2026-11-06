package bridge

import (
	"context"
	"log/slog"
	"maps"
	"slices"
	"strings"
	"testing"
	"time"

	"gardenio/controller/internal/beadsapi"
)

// newTestReviewReaper builds a reaper with the MR-landed review flag ENABLED
// (the pre-existing newTestReaper leaves it off so the reap tests stay scoped to
// the reap detectors).
func newTestReviewReaper(gl ReaperGitLabClient, d ReaperBeadClient) *EscalationReaper {
	r := NewEscalationReaper(EscalationReaperConfig{
		GitLab:     gl,
		Daemon:     d,
		Logger:     slog.New(slog.NewTextHandler(nopWriter{}, nil)),
		ReviewFlag: true,
	})
	r.now = func() time.Time { return time.Date(2026, 7, 30, 12, 0, 0, 0, time.UTC) }
	return r
}

// lastFieldUpdate merges every UpdateBeadFields patch a bead received into one
// map (later writes win), so a test can assert the net stamped state.
func (d *fakeReaperDaemon) mergedFields(beadID string) map[string]string {
	out := map[string]string{}
	for _, patch := range d.fieldUpdates[beadID] {
		maps.Copy(out, patch)
	}
	return out
}

// --- THE CORE CASE: merge-then-apply, MR merged → flag, never close ----------

// TestReviewFlag_ApplyVerbMergedMR_FlaggedNotClosed is the ka-0000001987 shape:
// an infrastructure apply ask whose referenced MR is merged. The reaper must NOT
// close it (the apply step may still stand), but the review flag MUST flip it to
// needs-review so the operator gets one signal instead of the endless stale nag.
func TestReviewFlag_ApplyVerbMergedMR_FlaggedNotClosed(t *testing.T) {
	gl := newFakeReaperGitLab()
	proj := "Acme/Platform/site-deployment"
	gl.mrs[mrKey(proj, 1168)] = &GitLabMR{
		IID: 1168, State: "merged",
		WebURL: "https://gitlab.com/Acme/Platform/site-deployment/-/merge_requests/1168",
	}
	bead := &beadsapi.BeadDetail{
		ID:    "ka-0000001987",
		Title: "Apply perf-harness terragrunt (site-deployment !1168 / DEVOPS-958) in prod-class perf acct 234567890123",
		Type:  "escalation",
		Fields: map[string]string{
			"operation_type": "infrastructure",
			"operation_spec": `{"repo":"Acme/Platform/site-deployment","merge_request":1168,"commands":["terragrunt plan","terragrunt apply"]}`,
		},
	}
	d := newFakeReaperDaemon(bead)
	r := newTestReviewReaper(gl, d)

	r.sweepOnce(context.Background())

	// NEVER closed — that is the load-bearing invariant.
	if _, closed := d.closed["ka-0000001987"]; closed {
		t.Fatal("FALSE-CLOSE: an apply-verb escalation with a merged MR must be flagged, never closed")
	}
	// Flipped to needs-review: latch + companions.
	mf := d.mergedFields("ka-0000001987")
	if mf[mrLandedField] != "true" {
		t.Fatalf("mr_landed = %q, want true", mf[mrLandedField])
	}
	if mf[mrLandedStateField] != "merged" {
		t.Fatalf("mr_landed_state = %q, want merged", mf[mrLandedStateField])
	}
	if mf[mrLandedIIDField] != "1168" {
		t.Fatalf("mr_landed_iid = %q, want 1168", mf[mrLandedIIDField])
	}
	if mf[mrLandedAtField] == "" {
		t.Fatal("mr_landed_at should be stamped")
	}
	// needs-review label surfaced.
	if !slices.Contains(d.labels["ka-0000001987"], needsReviewLabel) {
		t.Fatalf("needs-review label not added; got %v", d.labels["ka-0000001987"])
	}
	// Exactly one audit comment.
	if got := len(d.comments["ka-0000001987"]); got != 1 {
		t.Fatalf("want exactly 1 audit comment, got %d", got)
	}
	// One review pingback requested.
	if mf["pingback_source"] != "review-flag" {
		t.Fatalf("pingback_source = %q, want review-flag", mf["pingback_source"])
	}
	if mf["pingback_requested_at"] == "" {
		t.Fatal("pingback_requested_at should be stamped so the operator gets one signal")
	}
}

// TestReviewFlag_BareApproveMergeVerb_NotOnAllowlist_Flagged covers the real
// escalations whose action is `approve_and_merge` (NOT the reaper's
// `approve_and_merge_mr`): they fail the reap allowlist, so they fall to the
// flag. With the MR merged, the flag flips them to needs-review rather than
// letting the pingback loop nag the stale "approve + merge MR !N" text forever.
func TestReviewFlag_BareApproveMergeVerb_NotOnAllowlist_Flagged(t *testing.T) {
	gl := newFakeReaperGitLab()
	proj := "Acme/Platform/gardenio"
	gl.mrs[mrKey(proj, 6218)] = &GitLabMR{IID: 6218, State: "merged"}
	bead := escBead("ka-0000007362",
		"Approve + merge MR !6218 — turn ON MicroVM native suspend/resume in prod",
		map[string]any{
			"action": "approve_and_merge", // NOT on the reap allowlist
			"target": "gitlab MR Acme/Platform/gardenio!6218",
			"repo":   proj,
		})
	d := newFakeReaperDaemon(bead)
	r := newTestReviewReaper(gl, d)

	r.sweepOnce(context.Background())

	if _, closed := d.closed["ka-0000007362"]; closed {
		t.Fatal("approve_and_merge (config-effect) ask must be flagged, not closed — merge != deploy")
	}
	if d.mergedFields("ka-0000007362")[mrLandedField] != "true" {
		t.Fatal("bare approve_and_merge verb with merged MR should be flagged for review")
	}
}

// --- MR still open → no-op (the flag never acts on a not-yet-landed MR) -------

func TestReviewFlag_MROpen_NoOp(t *testing.T) {
	gl := newFakeReaperGitLab()
	proj := "Acme/Platform/site-deployment"
	gl.mrs[mrKey(proj, 1133)] = &GitLabMR{IID: 1133, State: "opened"}
	bead := escBead("ka-0000005964", "review+merge site-deployment !1133 then apply", map[string]any{
		"repo":   proj,
		"mr":     1133,
		"action": "review_and_merge_then_apply",
	})
	d := newFakeReaperDaemon(bead)
	r := newTestReviewReaper(gl, d)

	r.sweepOnce(context.Background())

	if d.mergedFields("ka-0000005964")[mrLandedField] == "true" {
		t.Fatal("an MR that is still `opened` has not landed — the flag must not fire")
	}
	if len(d.comments["ka-0000005964"]) != 0 {
		t.Fatalf("no comment expected for an open MR; got %v", d.comments["ka-0000005964"])
	}
}

// TestReviewFlag_MRClosed_Flagged: a CLOSED (declined) MR also lands the flag —
// the ask as written references an MR that can never merge, so the operator must
// re-scope or close (gd-l02aMkz7M5 / ka-0000005964 shapes).
func TestReviewFlag_MRClosed_Flagged(t *testing.T) {
	gl := newFakeReaperGitLab()
	proj := "Acme/Platform/monorepo"
	gl.mrs[mrKey(proj, 1386)] = &GitLabMR{IID: 1386, State: "closed"}
	bead := escBead("gd-l02aMkz7M5", "Add pikevsten as reviewer on monorepo MR !1386", map[string]any{
		"action":     "add_mr_reviewer",
		"mr_iid":     1386,
		"project":    proj,
		"project_id": 79511414,
	})
	d := newFakeReaperDaemon(bead)
	r := newTestReviewReaper(gl, d)

	r.sweepOnce(context.Background())

	if _, closed := d.closed["gd-l02aMkz7M5"]; closed {
		t.Fatal("the flag must never close; a closed MR is flagged for review, not reaped")
	}
	mf := d.mergedFields("gd-l02aMkz7M5")
	if mf[mrLandedField] != "true" || mf[mrLandedStateField] != "closed" {
		t.Fatalf("closed MR should flag with mr_landed_state=closed; got %v", mf)
	}
	if len(d.comments["gd-l02aMkz7M5"]) != 1 || !strings.Contains(d.comments["gd-l02aMkz7M5"][0], "closed/declined") {
		t.Fatalf("audit comment should describe the closed/declined MR; got %v", d.comments["gd-l02aMkz7M5"])
	}
}

// --- idempotency: flag once, never re-nag ------------------------------------

func TestReviewFlag_Idempotent_SecondSweepNoOp(t *testing.T) {
	gl := newFakeReaperGitLab()
	proj := "Acme/Platform/site-deployment"
	gl.mrs[mrKey(proj, 1168)] = &GitLabMR{IID: 1168, State: "merged"}
	bead := escBead("ka-0000001987", "Apply terragrunt (site-deployment !1168)", map[string]any{
		"repo": proj, "merge_request": 1168, "action": "terraform_apply",
	})
	d := newFakeReaperDaemon(bead)
	r := newTestReviewReaper(gl, d)

	r.sweepOnce(context.Background()) // flips + reflects mr_landed onto the bead
	r.sweepOnce(context.Background()) // sees mr_landed=true → no-op

	if got := len(d.comments["ka-0000001987"]); got != 1 {
		t.Fatalf("idempotency: want exactly 1 audit comment across two sweeps, got %d", got)
	}
	if got := len(d.labels["ka-0000001987"]); got != 1 {
		t.Fatalf("idempotency: want exactly 1 label add, got %d", got)
	}
	// Exactly one pingback request (the second sweep must not re-nag).
	pings := 0
	for _, patch := range d.fieldUpdates["ka-0000001987"] {
		if patch["pingback_source"] == "review-flag" {
			pings++
		}
	}
	if pings != 1 {
		t.Fatalf("idempotency: want exactly 1 review pingback across two sweeps, got %d", pings)
	}
}

// TestReviewFlag_PreExistingLatch_NotReflagged: a bead that already carries
// mr_landed=true (flagged on a prior process lifetime) is left untouched.
func TestReviewFlag_PreExistingLatch_NotReflagged(t *testing.T) {
	gl := newFakeReaperGitLab()
	proj := "Acme/Platform/site-deployment"
	gl.mrs[mrKey(proj, 1168)] = &GitLabMR{IID: 1168, State: "merged"}
	bead := escBead("ka-0000001987", "Apply terragrunt !1168", map[string]any{
		"repo": proj, "merge_request": 1168, "action": "terraform_apply",
	})
	bead.Fields[mrLandedField] = "true"
	d := newFakeReaperDaemon(bead)
	r := newTestReviewReaper(gl, d)

	r.sweepOnce(context.Background())

	if len(d.comments["ka-0000001987"]) != 0 || len(d.fieldUpdates["ka-0000001987"]) != 0 {
		t.Fatal("a bead already flagged (mr_landed=true) must not be re-processed")
	}
	// And GitLab must never even be consulted — the latch short-circuits first.
	if gl.getCalls[mrKey(proj, 1168)] != 0 {
		t.Fatalf("latched bead should short-circuit before GitLab; GET called %d times", gl.getCalls[mrKey(proj, 1168)])
	}
}

// --- the reap path still wins when it should ---------------------------------

// TestReviewFlag_ReapableVerbMerged_ReapedNotFlagged: a pure approve/merge verb
// whose MR merged is CLOSED by the reap detector — the flag must not also fire
// (the reap path returns before the flag branch).
func TestReviewFlag_ReapableVerbMerged_ReapedNotFlagged(t *testing.T) {
	gl := newFakeReaperGitLab()
	proj := "Acme/Platform/gardenio"
	mergedAt := "2026-07-21T19:14:10Z"
	gl.mrs[mrKey(proj, 1896)] = &GitLabMR{
		IID: 1896, State: "merged",
		MergedBy: &GitLabUser{Username: "matthewbaker"}, MergedAt: &mergedAt,
	}
	bead := escBead("gd-reapable", "merge MR !1896", map[string]any{
		"action": "merge_mr", "iid": 1896, "project": proj,
	})
	d := newFakeReaperDaemon(bead)
	r := newTestReviewReaper(gl, d)

	r.sweepOnce(context.Background())

	if _, closed := d.closed["gd-reapable"]; !closed {
		t.Fatal("a reapable merge_mr with a merged MR should be reaped (closed), not flagged")
	}
	if d.mergedFields("gd-reapable")[mrLandedField] == "true" {
		t.Fatal("a reaped bead must NOT also carry the review flag — reap wins")
	}
}

// TestReviewFlag_ReapableVerbUnmerged_NotFlagged: a reapable verb whose MR is
// NOT yet merged is left for the reap detector + pingback loop — the flag must
// not step in (an unmerged MR is not "landed").
func TestReviewFlag_ReapableVerbUnmerged_NotFlagged(t *testing.T) {
	gl := newFakeReaperGitLab()
	proj := "Acme/Platform/gardenio"
	gl.mrs[mrKey(proj, 1896)] = &GitLabMR{IID: 1896, State: "opened"}
	bead := escBead("gd-unmerged", "merge MR !1896", map[string]any{
		"action": "merge_mr", "iid": 1896, "project": proj,
	})
	d := newFakeReaperDaemon(bead)
	r := newTestReviewReaper(gl, d)

	r.sweepOnce(context.Background())

	if _, closed := d.closed["gd-unmerged"]; closed {
		t.Fatal("unmerged reapable MR must not be closed")
	}
	if d.mergedFields("gd-unmerged")[mrLandedField] == "true" {
		t.Fatal("unmerged reapable MR must not be flagged for review either")
	}
}

// --- safety: disabled, dry-run, E2E, foreign host, unresolvable --------------

func TestReviewFlag_Disabled_NoOp(t *testing.T) {
	gl := newFakeReaperGitLab()
	proj := "Acme/Platform/site-deployment"
	gl.mrs[mrKey(proj, 1168)] = &GitLabMR{IID: 1168, State: "merged"}
	bead := escBead("ka-0000001987", "Apply terragrunt !1168", map[string]any{
		"repo": proj, "merge_request": 1168, "action": "terraform_apply",
	})
	d := newFakeReaperDaemon(bead)
	r := newTestReaper(gl, d) // review flag OFF

	r.sweepOnce(context.Background())

	if len(d.fieldUpdates["ka-0000001987"]) != 0 || len(d.comments["ka-0000001987"]) != 0 {
		t.Fatal("with the review flag disabled, the reaper must not touch a non-reapable escalation")
	}
}

func TestReviewFlag_DryRun_NoWrite(t *testing.T) {
	gl := newFakeReaperGitLab()
	proj := "Acme/Platform/site-deployment"
	gl.mrs[mrKey(proj, 1168)] = &GitLabMR{IID: 1168, State: "merged"}
	bead := escBead("ka-0000001987", "Apply terragrunt !1168", map[string]any{
		"repo": proj, "merge_request": 1168, "action": "terraform_apply",
	})
	d := newFakeReaperDaemon(bead)
	r := NewEscalationReaper(EscalationReaperConfig{
		GitLab:     gl,
		Daemon:     d,
		Logger:     slog.New(slog.NewTextHandler(nopWriter{}, nil)),
		ReviewFlag: true,
		DryRun:     true,
	})
	r.now = func() time.Time { return time.Date(2026, 7, 30, 12, 0, 0, 0, time.UTC) }

	r.sweepOnce(context.Background())

	if len(d.fieldUpdates["ka-0000001987"]) != 0 || len(d.comments["ka-0000001987"]) != 0 || len(d.labels["ka-0000001987"]) != 0 {
		t.Fatal("dry-run must not write comment/fields/label")
	}
}

func TestReviewFlag_E2EFixture_Skipped(t *testing.T) {
	gl := newFakeReaperGitLab()
	proj := "Acme/Platform/site-deployment"
	gl.mrs[mrKey(proj, 1168)] = &GitLabMR{IID: 1168, State: "merged"}
	bead := escBead("gd-e2e", "Apply terragrunt !1168", map[string]any{
		"repo": proj, "merge_request": 1168, "action": "terraform_apply",
	})
	bead.Labels = []string{"e2e:true"}
	d := newFakeReaperDaemon(bead)
	r := newTestReviewReaper(gl, d)

	r.sweepOnce(context.Background())

	if len(d.fieldUpdates["gd-e2e"]) != 0 {
		t.Fatal("E2E fixtures must never be flagged against the real GitLab API")
	}
}

func TestReviewFlag_ForeignHost_Skipped(t *testing.T) {
	gl := newFakeReaperGitLab()
	// The ref carries a host (from a full URL) that differs from the bridge host.
	bead := escBead("gd-foreign", "Apply after merge", map[string]any{
		"action": "terraform_apply",
		"mr_url": "https://gitlab.example.com/Acme/Platform/site-deployment/-/merge_requests/1168",
	})
	d := newFakeReaperDaemon(bead)
	r := newTestReviewReaper(gl, d)
	r.host = "gitlab.com" // bridge is on gitlab.com; the ref is on gitlab.example.com

	r.sweepOnce(context.Background())

	if len(d.fieldUpdates["gd-foreign"]) != 0 {
		t.Fatal("a foreign-host MR ref must be skipped — never GET it against this bridge")
	}
	if gl.getCalls[mrKey("Acme/Platform/site-deployment", 1168)] != 0 {
		t.Fatal("foreign-host ref must short-circuit before any GitLab GET")
	}
}

func TestReviewFlag_NoResolvableMR_NoOp(t *testing.T) {
	gl := newFakeReaperGitLab()
	bead := escBead("gd-nomr", "Grant Keycloak realm admin", map[string]any{
		"action": "create_keycloak_client",
		"realm":  "acme",
	})
	d := newFakeReaperDaemon(bead)
	r := newTestReviewReaper(gl, d)

	r.sweepOnce(context.Background())

	if len(d.fieldUpdates["gd-nomr"]) != 0 || len(d.comments["gd-nomr"]) != 0 {
		t.Fatal("a non-MR ask (no resolvable MR reference) must be left untouched")
	}
}

// TestReviewFlag_MRFetchError_NoOp: a transport error / 404 on the re-verify GET
// is a no-op — the flag never acts on its own failed read.
func TestReviewFlag_MRFetchError_NoOp(t *testing.T) {
	gl := newFakeReaperGitLab()
	proj := "Acme/Platform/site-deployment"
	// No MR registered → the fake returns a 404 error from GetMergeRequestByPath.
	bead := escBead("ka-0000001987", "Apply terragrunt !1168", map[string]any{
		"repo": proj, "merge_request": 1168, "action": "terraform_apply",
	})
	d := newFakeReaperDaemon(bead)
	r := newTestReviewReaper(gl, d)

	r.sweepOnce(context.Background())

	if len(d.fieldUpdates["ka-0000001987"]) != 0 {
		t.Fatal("a failed MR fetch must not flag — under-acting is the safe direction")
	}
}

// --- resolver unit coverage ---------------------------------------------------

func TestResolveReviewMRRef_BangRefInTitle(t *testing.T) {
	spec := map[string]any{"repo": "Acme/Platform/gardenio"}
	ref, ok := resolveReviewMRRef(spec, "Approve + merge MR !6218 turn on suspend", "", "")
	if !ok {
		t.Fatal("should resolve a bare !6218 bang-ref paired with the structured repo")
	}
	if ref.ProjectPath != "Acme/Platform/gardenio" || ref.IID != 6218 {
		t.Fatalf("resolved %+v, want gardenio!6218", ref)
	}
}

func TestResolveReviewMRRef_QualifiedBangRef(t *testing.T) {
	// project!iid inline, no structured project — the qualified bang carries it.
	ref, ok := resolveReviewMRRef(map[string]any{}, "Apply site-deployment/foo!1168 in prod", "", "")
	if !ok {
		t.Fatal("should resolve a project-qualified bang-ref")
	}
	if ref.ProjectPath != "site-deployment/foo" || ref.IID != 1168 {
		t.Fatalf("resolved %+v, want site-deployment/foo!1168", ref)
	}
}

func TestResolveReviewMRRef_NumericMergeRequestField(t *testing.T) {
	spec := map[string]any{"repo": "Acme/Platform/site-deployment", "merge_request": float64(1168)}
	ref, ok := resolveReviewMRRef(spec, "", "", "")
	if !ok || ref.IID != 1168 || ref.ProjectPath != "Acme/Platform/site-deployment" {
		t.Fatalf("resolved (%+v,%v), want site-deployment!1168", ref, ok)
	}
}

func TestResolveReviewMRRef_FullURLWins(t *testing.T) {
	ref, ok := resolveReviewMRRef(
		map[string]any{"mr_url": "https://gitlab.com/Acme/Platform/gardenio/-/merge_requests/6218"},
		"", "", "")
	if !ok || ref.Host != "gitlab.com" || ref.IID != 6218 {
		t.Fatalf("full URL should win with host; got (%+v,%v)", ref, ok)
	}
}

func TestResolveReviewMRRef_Unresolvable(t *testing.T) {
	if _, ok := resolveReviewMRRef(map[string]any{"realm": "acme"}, "grant admin", "", ""); ok {
		t.Fatal("no MR reference anywhere → must be unresolvable")
	}
}

func TestParseMRBangRef_BareVsQualified(t *testing.T) {
	if p, iid, ok := parseMRBangRef("merge !42 now"); !ok || p != "" || iid != 42 {
		t.Fatalf("bare bang: got (%q,%d,%v), want ('',42,true)", p, iid, ok)
	}
	if p, iid, ok := parseMRBangRef("a/b/c!7"); !ok || p != "a/b/c" || iid != 7 {
		t.Fatalf("qualified bang: got (%q,%d,%v), want ('a/b/c',7,true)", p, iid, ok)
	}
	// A prefix without a namespace separator is not a project.
	if p, _, ok := parseMRBangRef("foo!9"); !ok || p != "" {
		t.Fatalf("non-path prefix should yield bare ref; got (%q,%v)", p, ok)
	}
	if _, _, ok := parseMRBangRef("no bang here"); ok {
		t.Fatal("no bang-ref → ok=false")
	}
}

