package main

// plans_mrs_test.go — the plan-level "open MRs" index signal (Matt's ask: "the
// plans index should show open MRs so we can click through there too, just like
// w crew"). Covers the batched produces-edge walk + MR-bead hydration
// (planOpenMRsBulk): the open/closed lifecycle filter, per-plan dedup, the
// newest-first ordering, the inline render cap, and the card pill render.

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"gardenio/controller/internal/beadsapi"
)

// seedProducedMR wires an MR bead a step produced: a step task carries a FORWARD
// `produces` edge to a type=merge-request bead (step --produces--> MR), so the
// step's FORWARD deps surface its MRs. Mirrors the live linkage planOpenMRsBulk
// walks. status is the MR BEAD's lifecycle ("open" / "closed"); state is mr_state.
func seedProducedMR(f *fakeDaemon, mrID, stepID, iid, title, state, status string, updated time.Time) *beadsapi.BeadDetail {
	b := &beadsapi.BeadDetail{
		ID:     mrID,
		Title:  title,
		Kind:   "data",
		Type:   "merge-request",
		Status: status,
		Fields: map[string]string{
			"mr_iid":   iid,
			"mr_title": title,
			"mr_state": state,
			"mr_url":   "https://gitlab.example/mr/" + iid,
		},
		UpdatedAt: updated,
	}
	f.beads[mrID] = b
	if f.deps == nil {
		f.deps = map[string][]beadsapi.Dependency{}
	}
	f.deps[stepID] = append(f.deps[stepID], beadsapi.Dependency{
		BeadID: stepID, DependsOnID: mrID, Type: "produces",
	})
	return b
}

// withPipelineStatus stamps mr_pipeline_status on a seeded MR bead — the field
// the gitlab-bridge pipeline webhook keeps fresh and the index MR pill surfaces
// as its CI signal (ka-0000002426).
func withPipelineStatus(b *beadsapi.BeadDetail, status string) *beadsapi.BeadDetail {
	b.Fields["mr_pipeline_status"] = status
	return b
}

func mrsTestServer(f *fakeDaemon) *Server {
	s := testServerWithDaemon(f)
	s.SetNow(func() time.Time { return time.Date(2026, 7, 5, 12, 0, 0, 0, time.UTC) })
	return s
}

// TestPlanOpenMRsBulk_SurfacesOpenDropsClosed proves the bulk walk keeps OPEN MR
// beads and drops closed (merged) ones — "open" is the MR bead's own lifecycle,
// so a merged MR (its bead closed) never shows as an open MR on the index.
func TestPlanOpenMRsBulk_SurfacesOpenDropsClosed(t *testing.T) {
	f := seedPlansFake()
	openMR := time.Date(2026, 7, 4, 9, 0, 0, 0, time.UTC)
	seedProducedMR(f, "gd-mrOpen", "gd-step1", "101", "feat: wire route", "opened", "open", openMR)
	seedProducedMR(f, "gd-mrMerged", "gd-step1", "99", "chore: old", "merged", "closed", openMR)
	s := mrsTestServer(f)

	info := s.planOpenMRsBulk(context.Background(), map[string][]string{
		"gd-plan1": {"gd-step1"},
	})["gd-plan1"]

	if !info.Any() || info.Count() != 1 {
		t.Fatalf("Any=%v Count=%d, want true/1 (merged MR dropped)", info.Any(), info.Count())
	}
	got := info.MRs[0]
	if got.IID != "101" || got.URL != "https://gitlab.example/mr/101" {
		t.Errorf("open MR = (iid %q url %q), want (101, .../101)", got.IID, got.URL)
	}
	if got.Draft {
		t.Errorf("an opened MR must not read draft")
	}
}

// TestPlanOpenMRsBulk_NoOpenMROmitsPlan proves a plan with only merged MRs (or
// none) is absent from the map — its card then renders no MR pills.
func TestPlanOpenMRsBulk_NoOpenMROmitsPlan(t *testing.T) {
	f := seedPlansFake()
	seedProducedMR(f, "gd-mrMerged", "gd-step1", "99", "chore: old", "merged", "closed",
		time.Date(2026, 7, 4, 9, 0, 0, 0, time.UTC))
	s := mrsTestServer(f)
	if info, ok := s.planOpenMRsBulk(context.Background(), map[string][]string{"gd-plan1": {"gd-step1"}})["gd-plan1"]; ok {
		t.Errorf("plan with no OPEN MR should be omitted, got %+v", info)
	}
}

// TestPlanOpenMRsBulk_DraftTinted proves a draft MR is flagged Draft (the card
// tints it distinctly from a ready-for-review MR).
func TestPlanOpenMRsBulk_DraftTinted(t *testing.T) {
	f := seedPlansFake()
	seedProducedMR(f, "gd-mrDraft", "gd-step1", "102", "wip: draft", "draft", "open",
		time.Date(2026, 7, 4, 9, 0, 0, 0, time.UTC))
	s := mrsTestServer(f)
	info := s.planOpenMRsBulk(context.Background(), map[string][]string{"gd-plan1": {"gd-step1"}})["gd-plan1"]
	if info.Count() != 1 || !info.MRs[0].Draft {
		t.Errorf("draft MR: count=%d draft=%v, want 1/true", info.Count(), info.MRs[0].Draft)
	}
}

// TestPlanOpenMRsBulk_DedupesAcrossSteps proves one MR produced by two steps of
// the same plan counts once (the multi-producer pattern).
func TestPlanOpenMRsBulk_DedupesAcrossSteps(t *testing.T) {
	f := seedPlansFake()
	up := time.Date(2026, 7, 4, 9, 0, 0, 0, time.UTC)
	seedProducedMR(f, "gd-mrShared", "gd-step1", "103", "feat: shared", "opened", "open", up)
	// A second produces edge from another step to the SAME MR bead.
	f.deps["gd-stepX"] = []beadsapi.Dependency{
		{BeadID: "gd-stepX", DependsOnID: "gd-mrShared", Type: "produces"},
	}
	s := mrsTestServer(f)
	info := s.planOpenMRsBulk(context.Background(), map[string][]string{
		"gd-plan1": {"gd-step1", "gd-stepX"},
	})["gd-plan1"]
	if info.Count() != 1 {
		t.Errorf("Count = %d, want 1 (deduped by MR bead id)", info.Count())
	}
}

// TestPlanOpenMRsBulk_DedupesByMRURL proves TWO DISTINCT merge-request beads that
// carry the SAME GitLab mr_url collapse to ONE pill (gd-UosSGB1i87). The mrSeen
// bead-id dedup only guards a single bead reached via two steps; a re-created /
// re-linked MR bead is a second bead on the same MR (observed live: ka-0000007659
// + ka-0000002580 both on !6037), which would otherwise render the SAME MR twice
// AND inflate the "+N MR" overflow — a card-vs-truth delta. The newest bead's ref
// wins so its title/state/pipeline are the ones shown.
func TestPlanOpenMRsBulk_DedupesByMRURL(t *testing.T) {
	f := seedPlansFake()
	older := time.Date(2026, 7, 3, 9, 0, 0, 0, time.UTC)
	newer := time.Date(2026, 7, 4, 9, 0, 0, 0, time.UTC)
	// Two DISTINCT beads, produced by two different steps, both pointing at the
	// SAME canonical MR url — the live re-link shape. The newer bead carries the
	// current title; the older is the stale duplicate.
	a := seedProducedMR(f, "gd-mrDupOld", "gd-step1", "6037", "Draft: Draft: stale dup", "opened", "open", older)
	b := seedProducedMR(f, "gd-mrDupNew", "gd-stepX", "6037", "Draft: current title", "opened", "open", newer)
	shared := "https://gitlab.example/mr/6037"
	a.Fields["mr_url"] = shared
	b.Fields["mr_url"] = shared

	info := mrsTestServer(f).planOpenMRsBulk(context.Background(), map[string][]string{
		"gd-plan1": {"gd-step1", "gd-stepX"},
	})["gd-plan1"]

	if info.Count() != 1 {
		t.Fatalf("Count = %d, want 1 (two beads on the same MR url collapse to one pill)", info.Count())
	}
	if info.HasMore() {
		t.Errorf("HasMore = true, want false — the duplicate must not inflate the +N overflow")
	}
	if got := info.MRs[0].Title; got != "Draft: current title" {
		t.Errorf("kept ref Title = %q, want the newest bead's %q", got, "Draft: current title")
	}
}

// TestPlanOpenMRsBulk_NewestFirstAndCap proves the MRs order newest-first (by
// the MR bead's update instant) and the inline render cap elides the remainder
// into a HasMore/MoreCount overflow.
func TestPlanOpenMRsBulk_NewestFirstAndCap(t *testing.T) {
	f := seedPlansFake()
	base := time.Date(2026, 7, 1, 0, 0, 0, 0, time.UTC)
	// Four open MRs on one step, seeded oldest→newest; expect newest to lead.
	for i, iid := range []string{"1", "2", "3", "4"} {
		seedProducedMR(f, "gd-mr"+iid, "gd-step1", iid, "mr "+iid, "opened", "open",
			base.Add(time.Duration(i)*time.Hour))
	}
	s := mrsTestServer(f)
	info := s.planOpenMRsBulk(context.Background(), map[string][]string{"gd-plan1": {"gd-step1"}})["gd-plan1"]
	if info.Count() != 4 {
		t.Fatalf("Count = %d, want 4", info.Count())
	}
	if info.MRs[0].IID != "4" {
		t.Errorf("first MR iid = %q, want 4 (newest-first)", info.MRs[0].IID)
	}
	if !info.HasMore() || info.MoreCount() != 4-planMRMaxRender {
		t.Errorf("HasMore=%v MoreCount=%d, want true/%d", info.HasMore(), info.MoreCount(), 4-planMRMaxRender)
	}
	if len(info.Rendered()) != planMRMaxRender {
		t.Errorf("Rendered len = %d, want cap %d", len(info.Rendered()), planMRMaxRender)
	}
}

// TestPlanOpenMRsBulk_SkipsURLlessAndNonMR proves a produced target with no
// mr_url (dead-link pill) and a non-merge-request target on a stray produces
// edge are both skipped.
func TestPlanOpenMRsBulk_SkipsURLlessAndNonMR(t *testing.T) {
	f := seedPlansFake()
	// An MR bead with NO mr_url — must be skipped (would dead-link).
	f.beads["gd-mrNoURL"] = &beadsapi.BeadDetail{
		ID: "gd-mrNoURL", Type: "merge-request", Status: "open",
		Fields: map[string]string{"mr_state": "opened"},
	}
	// A non-MR target on a produces edge — must be skipped.
	f.beads["gd-notMR"] = &beadsapi.BeadDetail{ID: "gd-notMR", Type: "task", Status: "open"}
	f.deps["gd-step1"] = []beadsapi.Dependency{
		{BeadID: "gd-step1", DependsOnID: "gd-mrNoURL", Type: "produces"},
		{BeadID: "gd-step1", DependsOnID: "gd-notMR", Type: "produces"},
	}
	s := mrsTestServer(f)
	if info, ok := s.planOpenMRsBulk(context.Background(), map[string][]string{"gd-plan1": {"gd-step1"}})["gd-plan1"]; ok {
		t.Errorf("URL-less + non-MR targets should yield no open MR, got %+v", info)
	}
}

// TestHandlePlans_IndexRendersOpenMRPill is the end-to-end guard: an open MR a
// plan's step produced renders a click-through pill on the /plans index card.
func TestHandlePlans_IndexRendersOpenMRPill(t *testing.T) {
	f := seedPlansFake()
	seedProducedMR(f, "gd-mrOpen", "gd-step1", "204", "feat: ship it", "opened", "open",
		time.Date(2026, 7, 4, 9, 0, 0, 0, time.UTC))
	mux := mkServer(t, f)
	req := httptest.NewRequest("GET", "/plans", nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	body := w.Body.String()
	for _, want := range []string{
		"plan-card-mr",                  // the pill class
		"https://gitlab.example/mr/204", // links through to GitLab
		"!204",                          // the MR iid label
		`target="_blank"`,               // opens in a new tab
	} {
		if !strings.Contains(body, want) {
			t.Errorf("index body missing %q", want)
		}
	}
}

// TestPipelineStatusBucket_Families proves the many raw GitLab pipeline statuses
// collapse to the small class-driving set the pill styling keys on: success,
// failed, and canceled stay distinct; everything else in-flight reads
// "running"; a blank status yields "" (no CI badge). This is the source of
// truth the CIState render gate + CSS class both derive from.
func TestPipelineStatusBucket_Families(t *testing.T) {
	cases := map[string]string{
		"":                     "",
		"success":              "success",
		"failed":               "failed",
		"canceled":             "canceled",
		"running":              "running",
		"pending":              "running",
		"created":              "running",
		"scheduled":            "running",
		"manual":               "running",
		"skipped":              "running",
		"waiting_for_resource": "running",
	}
	for status, want := range cases {
		if got := pipelineStatusBucket(status); got != want {
			t.Errorf("pipelineStatusBucket(%q) = %q, want %q", status, got, want)
		}
	}
}

// TestPlanMRRef_CIStateAndGlyph proves the pill accessors: a known pipeline
// status yields a non-empty CIState bucket + a glyph; a blank status yields
// "" / "" so the template renders no CI badge (an MR with no pipeline reads
// exactly as before the increment).
func TestPlanMRRef_CIStateAndGlyph(t *testing.T) {
	if ref := (planMRRef{PipelineStatus: "success"}); ref.CIState() != "success" || ref.CIGlyph() == "" {
		t.Errorf("success ref: CIState=%q CIGlyph=%q, want success/non-empty", ref.CIState(), ref.CIGlyph())
	}
	if ref := (planMRRef{PipelineStatus: "failed"}); ref.CIState() != "failed" || ref.CIGlyph() == "" {
		t.Errorf("failed ref: CIState=%q CIGlyph=%q, want failed/non-empty", ref.CIState(), ref.CIGlyph())
	}
	if ref := (planMRRef{PipelineStatus: ""}); ref.CIState() != "" || ref.CIGlyph() != "" {
		t.Errorf("no-pipeline ref: CIState=%q CIGlyph=%q, want empty/empty (no CI badge)", ref.CIState(), ref.CIGlyph())
	}
}

// TestPlanOpenMRsBulk_CarriesPipelineStatus proves the bulk walk copies
// mr_pipeline_status off the hydrated MR bead onto the ref — no extra daemon
// call, the field is already on the bead the produces-walk fetched.
func TestPlanOpenMRsBulk_CarriesPipelineStatus(t *testing.T) {
	f := seedPlansFake()
	withPipelineStatus(
		seedProducedMR(f, "gd-mrRed", "gd-step1", "301", "feat: risky", "opened", "open",
			time.Date(2026, 7, 4, 9, 0, 0, 0, time.UTC)),
		"failed")
	s := mrsTestServer(f)
	info := s.planOpenMRsBulk(context.Background(), map[string][]string{"gd-plan1": {"gd-step1"}})["gd-plan1"]
	if info.Count() != 1 {
		t.Fatalf("Count = %d, want 1", info.Count())
	}
	if got := info.MRs[0].PipelineStatus; got != "failed" {
		t.Errorf("PipelineStatus = %q, want failed", got)
	}
	if info.MRs[0].CIState() != "failed" {
		t.Errorf("CIState = %q, want failed", info.MRs[0].CIState())
	}
}

// TestHandlePlans_IndexRendersMRPipelineStatus is the end-to-end guard: a failed
// pipeline on a plan's open MR renders the loud "needs attention" CI class on
// the index card pill, so an operator sees a red MR without clicking through.
func TestHandlePlans_IndexRendersMRPipelineStatus(t *testing.T) {
	f := seedPlansFake()
	withPipelineStatus(
		seedProducedMR(f, "gd-mrRed", "gd-step1", "302", "feat: needs fix", "opened", "open",
			time.Date(2026, 7, 4, 9, 0, 0, 0, time.UTC)),
		"failed")
	mux := mkServer(t, f)
	req := httptest.NewRequest("GET", "/plans", nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	body := w.Body.String()
	for _, want := range []string{
		"plan-card-mr-ci-failed",       // the whole-pill loud-red class
		"plan-card-mr-ci-glyph-failed", // the glyph color class
		"CI failed",                    // the tooltip carries the status
	} {
		if !strings.Contains(body, want) {
			t.Errorf("index body missing %q (failed-pipeline pill signal)", want)
		}
	}
}

// TestPlanMRInfo_FailedHiddenProjection is the crux projection test for
// iteration 20 (ka-0000000387): FailedHiddenCount counts ONLY the failed-CI MRs
// in the ELIDED tail (past planMRMaxRender), never the rendered head — the head
// pills already carry their own CI signal, so counting them would double-signal.
func TestPlanMRInfo_FailedHiddenProjection(t *testing.T) {
	mk := func(status string) planMRRef { return planMRRef{URL: "u", PipelineStatus: status} }
	// Head (first planMRMaxRender): a failed one here must NOT count (it renders
	// its own loud pill). Tail: two failed + one green among the elided MRs.
	head := make([]planMRRef, 0, planMRMaxRender)
	for i := 0; i < planMRMaxRender; i++ {
		head = append(head, mk("failed")) // every head MR is red — must be ignored
	}
	tail := []planMRRef{mk("failed"), mk("success"), mk("failed"), mk("running")}
	info := planMRInfo{MRs: append(head, tail...)}
	if got := info.FailedHiddenCount(); got != 2 {
		t.Errorf("FailedHiddenCount = %d, want 2 (only the elided failed MRs)", got)
	}
	if !info.HasFailedHidden() {
		t.Error("HasFailedHidden = false, want true (elided set has failed MRs)")
	}
	// No overflow at all → no hidden signal, whatever the head CI is.
	small := planMRInfo{MRs: head} // exactly planMRMaxRender, all failed, but none elided
	if small.FailedHiddenCount() != 0 || small.HasFailedHidden() {
		t.Errorf("no-overflow plan must report 0 hidden-failed, got count=%d has=%v",
			small.FailedHiddenCount(), small.HasFailedHidden())
	}
	// Overflow whose elided MRs are all green/running → calm, count-only.
	allGreen := planMRInfo{MRs: append(append([]planMRRef{}, head...), mk("success"), mk("running"))}
	if allGreen.HasFailedHidden() {
		t.Error("an all-green overflow must not report hidden-failed (adds no attention ink)")
	}
}

// TestPlanMRPromotion_FailedLeadsPreservingNewestWithinTier is the crux
// ordering test for iteration 21 (gd-VHwFGOoKDV): planOpenMRsBulk pulls every
// FAILED-CI MR ahead of the non-failed ones, so a failing MR lands in a rendered
// (clickable) slot — while the newest-first order is preserved WITHIN each tier.
// Without promotion a failing MR that is merely OLDER than planMRMaxRender green
// MRs sits in the un-clickable "+N" overflow (the iter-20 blind spot this closes).
func TestPlanMRPromotion_FailedLeadsPreservingNewestWithinTier(t *testing.T) {
	f := seedPlansFake()
	base := time.Date(2026, 7, 1, 0, 0, 0, 0, time.UTC)
	// Two GREEN MRs seeded NEWEST (highest age keys) + two FAILED MRs seeded OLDER.
	// On age alone the greens would take the lead; promotion must flip the failures
	// to the front, newest-failed first, then the greens newest-first.
	withPipelineStatus(seedProducedMR(f, "gd-g1", "gd-step1", "10", "green new", "opened", "open",
		base.Add(40*time.Hour)), "success")
	withPipelineStatus(seedProducedMR(f, "gd-g2", "gd-step1", "11", "green old", "opened", "open",
		base.Add(30*time.Hour)), "success")
	withPipelineStatus(seedProducedMR(f, "gd-f1", "gd-step1", "20", "red new", "opened", "open",
		base.Add(20*time.Hour)), "failed")
	withPipelineStatus(seedProducedMR(f, "gd-f2", "gd-step1", "21", "red old", "opened", "open",
		base.Add(10*time.Hour)), "failed")
	s := mrsTestServer(f)
	info := s.planOpenMRsBulk(context.Background(), map[string][]string{"gd-plan1": {"gd-step1"}})["gd-plan1"]
	gotIIDs := make([]string, 0, len(info.MRs))
	for _, r := range info.MRs {
		gotIIDs = append(gotIIDs, r.IID)
	}
	// Failed tier first (newest-failed 20 before older-failed 21), then green tier
	// (newest-green 10 before older-green 11) — newest-first is preserved WITHIN
	// each tier, only the tier boundary moves.
	wantIIDs := []string{"20", "21", "10", "11"}
	if strings.Join(gotIIDs, ",") != strings.Join(wantIIDs, ",") {
		t.Errorf("MR order = %v, want %v (failed-CI promoted, newest-first within tier)", gotIIDs, wantIIDs)
	}
	// The failing MRs now occupy the first planMRMaxRender rendered (clickable)
	// slots, so nothing failing overflows — the iter-20 hidden-failed count is 0.
	if info.FailedHiddenCount() != 0 || info.HasFailedHidden() {
		t.Errorf("after promotion FailedHiddenCount=%d Has=%v, want 0/false (both failures now clickable)",
			info.FailedHiddenCount(), info.HasFailedHidden())
	}
}

// TestPlanMRPromotion_NoFailuresIsNoOp proves the density discipline: a plan
// whose open MRs are ALL green/running partitions to a no-op — the stable sort
// preserves the newest-first order exactly as before iteration 21, so a healthy
// plan renders identically (no reordering, no regression).
func TestPlanMRPromotion_NoFailuresIsNoOp(t *testing.T) {
	f := seedPlansFake()
	base := time.Date(2026, 7, 1, 0, 0, 0, 0, time.UTC)
	// Seeded oldest→newest; newest-first must still lead, untouched by promotion.
	withPipelineStatus(seedProducedMR(f, "gd-a", "gd-step1", "1", "one", "opened", "open",
		base.Add(1*time.Hour)), "success")
	withPipelineStatus(seedProducedMR(f, "gd-b", "gd-step1", "2", "two", "opened", "open",
		base.Add(2*time.Hour)), "running")
	seedProducedMR(f, "gd-c", "gd-step1", "3", "three", "opened", "open", base.Add(3*time.Hour)) // no pipeline
	s := mrsTestServer(f)
	info := s.planOpenMRsBulk(context.Background(), map[string][]string{"gd-plan1": {"gd-step1"}})["gd-plan1"]
	got := make([]string, 0, len(info.MRs))
	for _, r := range info.MRs {
		got = append(got, r.IID)
	}
	if strings.Join(got, ",") != "3,2,1" {
		t.Errorf("no-failure MR order = %v, want [3 2 1] (pure newest-first, promotion a no-op)", got)
	}
}

// TestHandlePlans_IndexPromotesFailedMRToClickablePill is the end-to-end render
// regression for iteration 21 (gd-VHwFGOoKDV): a plan with planMRMaxRender green
// MRs (newest) plus ONE older failed MR renders the FAILED MR as a click-through
// inline pill (its GitLab URL appears as an href), and the overflow pill is NOT
// tinted failed-red (nothing failing overflows anymore) — the exact live shape
// of plan ka-0000003619 ("+17 MR · ✕ 1" with the red MR un-clickable) this fixes.
func TestHandlePlans_IndexPromotesFailedMRToClickablePill(t *testing.T) {
	f := seedPlansFake()
	base := time.Date(2026, 7, 1, 0, 0, 0, 0, time.UTC)
	// planMRMaxRender green MRs seeded NEWEST (would fill every rendered slot on
	// age), then one OLDER failed MR — pre-iter-21 it would be elided + un-clickable.
	for i := 0; i < planMRMaxRender; i++ {
		iid := "60" + strconv.Itoa(i)
		withPipelineStatus(
			seedProducedMR(f, "gd-pg"+iid, "gd-step1", iid, "green "+iid, "opened", "open",
				base.Add(time.Duration(10+i)*time.Hour)),
			"success")
	}
	withPipelineStatus(
		seedProducedMR(f, "gd-pRed", "gd-step1", "6099", "promote me", "opened", "open",
			base), // oldest by age, but failed → promoted into a rendered slot
		"failed")
	mux := mkServer(t, f)
	req := httptest.NewRequest("GET", "/plans", nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	body := w.Body.String()
	// The failed MR's GitLab URL is rendered as a clickable inline pill href — it
	// was promoted out of the overflow into a rendered slot.
	if !strings.Contains(body, "https://gitlab.example/mr/6099") {
		t.Error("index body missing the promoted failed MR's clickable URL (it should render as an inline pill)")
	}
	// And nothing failing overflows now, so the overflow's FAILED-CI tooltip is ABSENT.
	if strings.Contains(body, "with FAILED CI") {
		t.Error("no failure should overflow after promotion, but the overflow pill still names a FAILED-CI hidden count")
	}
}

// TestHandlePlans_IndexOverflowPillTintsFailedCI is the end-to-end render
// regression (ka-0000000387): a plan with more open MRs than the render cap,
// where a HIDDEN (elided) MR failed CI, tints the "+N MR" overflow pill loud red
// and folds a "✕ K" facet onto it — so the collapsed overflow no longer hides
// that some of the plan's MRs are red. Without the fix the overflow pill is a
// bare calm-gray count identical to an all-green overflow.
//
// iter-21 (gd-VHwFGOoKDV) makes failed MRs PROMOTE ahead of green ones into the
// rendered slots, so this scenario seeds MORE failing MRs than there are inline
// slots: planMRMaxRender+1 failing MRs means one still overflows AFTER promotion
// fills every rendered slot with a (now-clickable) failure — the residual the
// overflow ✕ facet must still surface. (A single failed MR would now be promoted
// to a clickable inline pill and correctly NO LONGER counts as hidden — that is
// the iter-21 win, asserted by TestPlanMRPromotion_* below.)
func TestHandlePlans_IndexOverflowPillTintsFailedCI(t *testing.T) {
	f := seedPlansFake()
	base := time.Date(2026, 7, 1, 0, 0, 0, 0, time.UTC)
	// planMRMaxRender GREEN MRs seeded NEWEST (they would take every rendered slot
	// on age alone), then planMRMaxRender+1 OLDER FAILED MRs. After iter-21
	// attention-first promotion, the failures fill all planMRMaxRender rendered
	// slots and exactly ONE failure still overflows — so the overflow pill must
	// still tint red with a ✕ 1 residual facet.
	for i := 0; i < planMRMaxRender; i++ {
		iid := "80" + strconv.Itoa(i)
		withPipelineStatus(
			seedProducedMR(f, "gd-mrGreen"+iid, "gd-step1", iid, "green "+iid, "opened", "open",
				base.Add(time.Duration(10+i)*time.Hour)), // newest by age
			"success")
	}
	for i := 0; i < planMRMaxRender+1; i++ {
		iid := "99" + strconv.Itoa(i)
		withPipelineStatus(
			seedProducedMR(f, "gd-mrHidRed"+iid, "gd-step1", iid, "hidden red "+iid, "opened", "open",
				base.Add(time.Duration(i)*time.Minute)), // older than the greens
			"failed")
	}
	mux := mkServer(t, f)
	req := httptest.NewRequest("GET", "/plans", nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	body := w.Body.String()
	// Key on RENDERED-ONLY markers, not the class names — .plan-card-mr-more-failed
	// / .plan-card-mr-more-ci live in the always-present page <style> block, so the
	// class name alone would false-match (the same footgun NoPipelineNoCIBadge
	// avoids). The tooltip "with FAILED CI" (uppercase) and the facet entity
	// "&#x2715;" appear ONLY on the actually-rendered overflow pill.
	for _, want := range []string{
		"with FAILED CI", // the tooltip names the hidden failing count
		"&#x2715;",       // the "✕" facet glyph, rendered only when a hidden MR failed
		`aria-label="1 hidden MR with failed CI"`, // the facet's a11y label
	} {
		if !strings.Contains(body, want) {
			t.Errorf("index body missing %q (failed-hidden overflow signal)", want)
		}
	}
}

// TestHandlePlans_IndexOverflowPillCalmWhenHiddenGreen proves the density
// discipline for iteration 20: an overflow whose ELIDED MRs are all green
// renders the "+N MR" pill exactly as before — no loud-red class, no "✕" facet —
// so a healthy plan with many MRs adds no false attention ink.
func TestHandlePlans_IndexOverflowPillCalmWhenHiddenGreen(t *testing.T) {
	f := seedPlansFake()
	base := time.Date(2026, 7, 1, 0, 0, 0, 0, time.UTC)
	// planMRMaxRender + 2 MRs, ALL green — the overflow exists but hides nothing red.
	for i := 0; i < planMRMaxRender+2; i++ {
		iid := "70" + strconv.Itoa(i)
		withPipelineStatus(
			seedProducedMR(f, "gd-mrOk"+iid, "gd-step1", iid, "ok "+iid, "opened", "open",
				base.Add(time.Duration(i)*time.Hour)),
			"success")
	}
	mux := mkServer(t, f)
	req := httptest.NewRequest("GET", "/plans", nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)
	body := w.Body.String()
	// The overflow pill renders — its "+N MR" text is rendered-only (the count is
	// dynamic, unlike the class name which also lives in <style>).
	if !strings.Contains(body, "MR</a>") {
		t.Fatalf("index body missing the overflow pill itself")
	}
	// The rendered-only failed markers must be ABSENT — a healthy overflow adds no
	// attention ink. (Key on the rendered tooltip/entity, not the class, which is
	// always present in the page <style>.)
	if strings.Contains(body, "with FAILED CI") {
		t.Error("an all-green overflow must NOT render the FAILED-CI tooltip")
	}
	if strings.Contains(body, `aria-label="`) && strings.Contains(body, "hidden MR") && strings.Contains(body, "failed CI") {
		t.Error("an all-green overflow must NOT render the ✕ hidden-failed facet")
	}
}

// TestHandlePlans_IndexNoPipelineNoCIBadge proves the density discipline: an
// open MR with NO pipeline status renders no CI badge — the pill reads exactly
// as it did before the increment (no phantom "running"/glyph on a pipeline-less
// MR).
func TestHandlePlans_IndexNoPipelineNoCIBadge(t *testing.T) {
	f := seedPlansFake()
	seedProducedMR(f, "gd-mrPlain", "gd-step1", "303", "feat: no ci yet", "opened", "open",
		time.Date(2026, 7, 4, 9, 0, 0, 0, time.UTC))
	mux := mkServer(t, f)
	req := httptest.NewRequest("GET", "/plans", nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)
	body := w.Body.String()
	if !strings.Contains(body, "https://gitlab.example/mr/303") {
		t.Fatalf("index body missing the MR pill itself")
	}
	// `aria-label="CI ` is stamped only on the rendered CI-badge span (the
	// .plan-card-mr-ci* CSS rules always live in the page <style> block, so the
	// class name alone would false-match). Its absence proves no badge rendered.
	if strings.Contains(body, `aria-label="CI `) {
		t.Errorf("a pipeline-less MR must render no CI badge, but body carries a CI aria-label")
	}
}
