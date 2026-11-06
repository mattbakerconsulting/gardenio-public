package main

import (
	"maps"
	"strings"
	"testing"

	"gardenio/kbeads/internal/model"
)

// The open-MR close guard (ka-0000003953). A bead carrying an mr_url whose MR
// is still `opened` must NOT be closable without --allow-open-mr, so the ledger
// can never claim a fix shipped while its code is off main — the exact drift
// that silently dropped MR !6183 for ~50min. The guard fires only on POSITIVE
// evidence that the MR is open; any read failure or ambiguous state lets the
// close proceed, so it can never manufacture a spurious refusal.
//
// mustFields(map[string]any) and mrBead(id, url, status) are shared helpers
// from admin_backfill_*_test.go; here we build MR-state beads via
// canonicalMRBead so tests read declaratively.

// issueWithMR registers a mock issue bead carrying mr_url (+ optional own
// mr_state fallback) keyed by id in mc.Beads (so GetBead resolves it).
func issueWithMR(mc *mockBeadsClient, id, mrURL, ownState string) {
	f := map[string]any{"mr_url": mrURL}
	if ownState != "" {
		f["mr_state"] = ownState
	}
	mc.Beads[id] = &model.Bead{ID: id, Type: model.TypeBug, Status: model.StatusInProgress, Fields: mustFields(f)}
}

// canonicalMRBead builds a type=merge-request bead carrying mr_url + the given
// state fields (mr_state / mr_merged / mr_outcome), as findMRBead reads it.
func canonicalMRBead(mrURL string, stateFields map[string]any) *model.Bead {
	f := map[string]any{"mr_url": mrURL}
	maps.Copy(f, stateFields)
	return &model.Bead{ID: "gd-7prqnd0vyy", Type: model.BeadType("merge-request"), Fields: mustFields(f)}
}

const openMRTestURL = "https://gitlab.com/acme/app/-/merge_requests/6183"

// TestClose_RefusesOpenMR is the core regression lock: closing a bead whose
// canonical MR bead says mr_state=opened is refused, CloseBead is NEVER called,
// and the refusal names both the bead and the MR url.
func TestClose_RefusesOpenMR(t *testing.T) {
	mc := newMockClient()
	issueWithMR(mc, "gd-c3u76usvml", openMRTestURL, "")
	mc.ListBeadsResult = []*model.Bead{canonicalMRBead(openMRTestURL, map[string]any{"mr_state": "opened"})}
	withMockClient(t, mc)

	out, err := execClose(t, "gd-c3u76usvml")
	if err == nil {
		t.Fatalf("close of a bead with an OPEN MR returned nil; want a refusal.\n%s", out)
	}
	if !strings.Contains(err.Error(), "gd-c3u76usvml") || !strings.Contains(err.Error(), openMRTestURL) {
		t.Errorf("refusal = %q; want it to name the bead AND the MR url", err.Error())
	}
	if !strings.Contains(err.Error(), "--allow-open-mr") {
		t.Errorf("refusal = %q; want it to point at the --allow-open-mr override", err.Error())
	}
	if len(mc.CloseBeadCalls) != 0 {
		t.Fatalf("CloseBead called %d times on a refused close; want 0", len(mc.CloseBeadCalls))
	}
}

// TestClose_AllowOpenMR_Overrides proves the deliberate override still closes an
// open-MR bead (e.g. the MR was abandoned), AND stamps mr_close_override="true"
// so the gitlab-bridge reconcile sweep (ka-0000000267) knows the close was
// intentional and must not re-open it — directions 1 (guard) and 2 (sweep)
// composing instead of fighting.
func TestClose_AllowOpenMR_Overrides(t *testing.T) {
	mc := newMockClient()
	issueWithMR(mc, "gd-c3u76usvml", openMRTestURL, "")
	mc.ListBeadsResult = []*model.Bead{canonicalMRBead(openMRTestURL, map[string]any{"mr_state": "opened"})}
	withMockClient(t, mc)

	out, err := execClose(t, "gd-c3u76usvml", "--allow-open-mr")
	if err != nil {
		t.Fatalf("`close --allow-open-mr` on an open-MR bead errored: %v\n%s", err, out)
	}
	if len(mc.CloseBeadCalls) != 1 || mc.CloseBeadCalls[0].ID != "gd-c3u76usvml" {
		t.Fatalf("CloseBead calls = %+v, want one close of gd-c3u76usvml", mc.CloseBeadCalls)
	}
	// The override marker must be stamped BEFORE the close so the sweep skips it.
	if len(mc.UpdateBeadCalls) != 1 || mc.UpdateBeadCalls[0].ID != "gd-c3u76usvml" {
		t.Fatalf("UpdateBead calls = %+v, want one stamp on gd-c3u76usvml", mc.UpdateBeadCalls)
	}
	if got := string(mc.UpdateBeadCalls[0].Req.Fields); !strings.Contains(got, "mr_close_override") || !strings.Contains(got, "true") {
		t.Errorf("stamp fields = %q; want mr_close_override=true", got)
	}
}

// TestClose_MergedMR_NoOverrideStamp proves the override marker is stamped ONLY
// when the guard was actually overridden (an OPEN MR + --allow-open-mr). A
// normal close of a merged-MR bead must NOT stamp mr_close_override — the sweep
// never touches it, so marking it would be misleading noise.
func TestClose_MergedMR_NoOverrideStamp(t *testing.T) {
	mc := newMockClient()
	issueWithMR(mc, "gd-c3u76usvml", openMRTestURL, "")
	mc.ListBeadsResult = []*model.Bead{canonicalMRBead(openMRTestURL, map[string]any{"mr_state": "merged"})}
	withMockClient(t, mc)

	out, err := execClose(t, "gd-c3u76usvml", "--allow-open-mr")
	if err != nil {
		t.Fatalf("close of a merged-MR bead errored: %v\n%s", err, out)
	}
	if len(mc.CloseBeadCalls) != 1 {
		t.Fatalf("CloseBead called %d times; want 1", len(mc.CloseBeadCalls))
	}
	if len(mc.UpdateBeadCalls) != 0 {
		t.Fatalf("UpdateBead called %d times for a merged-MR close; want 0 (no override to record)", len(mc.UpdateBeadCalls))
	}
}

// TestClose_MergedMR_Closes proves a bead whose MR is MERGED closes normally —
// the guard only trips on an OPEN MR. Covers all three merged spellings the
// bridge uses (mr_state=merged / mr_merged=true / mr_outcome=merged).
func TestClose_MergedMR_Closes(t *testing.T) {
	for _, mergedFields := range []map[string]any{
		{"mr_state": "merged"},
		{"mr_merged": "true"},
		{"mr_outcome": "merged"},
	} {
		mc := newMockClient()
		issueWithMR(mc, "gd-c3u76usvml", openMRTestURL, "")
		mc.ListBeadsResult = []*model.Bead{canonicalMRBead(openMRTestURL, mergedFields)}
		withMockClient(t, mc)

		out, err := execClose(t, "gd-c3u76usvml")
		if err != nil {
			t.Fatalf("close of a MERGED-MR bead (%v) errored: %v\n%s", mergedFields, err, out)
		}
		if len(mc.CloseBeadCalls) != 1 {
			t.Fatalf("CloseBead called %d times for merged MR %v; want 1", len(mc.CloseBeadCalls), mergedFields)
		}
	}
}

// TestClose_NoMRURL_Closes proves a bead with no mr_url is unaffected.
func TestClose_NoMRURL_Closes(t *testing.T) {
	mc := newMockClient()
	mc.Beads["gd-c3u76usvml"] = &model.Bead{ID: "gd-c3u76usvml", Status: model.StatusInProgress}
	withMockClient(t, mc)

	out, err := execClose(t, "gd-c3u76usvml")
	if err != nil {
		t.Fatalf("close of a bead with no mr_url errored: %v\n%s", err, out)
	}
	if len(mc.CloseBeadCalls) != 1 {
		t.Fatalf("CloseBead called %d times; want 1", len(mc.CloseBeadCalls))
	}
}

// TestClose_GetBeadError_DoesNotBlock proves a read failure NEVER manufactures a
// refusal: if we can't fetch the bead to check its MR, the close proceeds (the
// guard is fail-open by design). This is also why every pre-existing close test
// — which never populates mc.Beads — stays green.
func TestClose_GetBeadError_DoesNotBlock(t *testing.T) {
	mc := newMockClient() // GetBead errors "not found" for unknown ids
	withMockClient(t, mc)

	out, err := execClose(t, "gd-unknown0001")
	if err != nil {
		t.Fatalf("close errored on a GetBead failure; the guard must be fail-open: %v\n%s", err, out)
	}
	if len(mc.CloseBeadCalls) != 1 {
		t.Fatalf("CloseBead called %d times; want 1 (fail-open)", len(mc.CloseBeadCalls))
	}
}

// TestClose_NoCanonicalMRBead_FallsBackToIssueState proves the fallback path:
// when the bridge hasn't projected a canonical MR bead yet, the guard reads the
// issue bead's OWN mr_state — but only blocks on a positive `opened`.
func TestClose_NoCanonicalMRBead_FallsBackToIssueState(t *testing.T) {
	mc := newMockClient()
	issueWithMR(mc, "gd-c3u76usvml", openMRTestURL, "opened")
	// ListBeadsResult left nil → findMRBead returns (nil,nil) → fallback.
	withMockClient(t, mc)

	if _, err := execClose(t, "gd-c3u76usvml"); err == nil {
		t.Fatal("close with own mr_state=opened and no canonical MR bead returned nil; want a refusal via fallback")
	}
	if len(mc.CloseBeadCalls) != 0 {
		t.Fatalf("CloseBead called %d times; want 0 (fallback refusal)", len(mc.CloseBeadCalls))
	}
}

// --- merge→close race (bug ka-0000004113) ------------------------------------
//
// Both MR-state sources the guard reads are bridge PROJECTIONS that trail a
// synchronous `glab mr merge`. On the reported incident (gardenio MR !6746) the
// numbers were: merged on GitLab 17:26:47, `kd close` refused 17:27:12, bridge
// projected `merged` 17:27:15 — the refusal landed 25s after the merge and 3s
// before the projection caught up. These lock in that a live, authoritative
// `merged` acquits, while every other verdict leaves the guard intact.

// TestClose_JustMergedMR_LiveMergedAcquitsStaleOpenProjection is the core fix
// lock: the canonical MR bead still says `opened`, but GitLab says merged — the
// close must proceed WITHOUT --allow-open-mr, and must NOT stamp
// mr_close_override on a correctly-shipped bead (the stamp tells the reconcile
// sweep to stop re-verifying, so a false stamp is real damage).
func TestClose_JustMergedMR_LiveMergedAcquitsStaleOpenProjection(t *testing.T) {
	mc := newMockClient()
	issueWithMR(mc, "gd-c3u76usvml", openMRTestURL, "")
	mc.ListBeadsResult = []*model.Bead{canonicalMRBead(openMRTestURL, map[string]any{"mr_state": "opened"})}
	withMockClient(t, mc)
	var checked []string
	withMRLiveState(t, mrLiveMerged, &checked)

	out, err := execClose(t, "gd-c3u76usvml")
	if err != nil {
		t.Fatalf("close of a JUST-MERGED bead whose MR bead still reads `opened` was refused: %v\n%s", err, out)
	}
	if len(mc.CloseBeadCalls) != 1 {
		t.Fatalf("CloseBead called %d times; want 1 (live `merged` must acquit a lagging projection)", len(mc.CloseBeadCalls))
	}
	if len(mc.UpdateBeadCalls) != 0 {
		t.Fatalf("UpdateBead called %d times; want 0 — a correctly-shipped bead must not be stamped mr_close_override", len(mc.UpdateBeadCalls))
	}
	if len(checked) != 1 || checked[0] != openMRTestURL {
		t.Errorf("live check calls = %v; want exactly one for %s", checked, openMRTestURL)
	}
}

// TestClose_GenuinelyOpenMR_LiveOpenStillRefuses is the other half of the lock:
// when GitLab CONFIRMS the MR is open, the guard must still refuse. The fix
// must not become a way to close beads whose code is genuinely off main.
func TestClose_GenuinelyOpenMR_LiveOpenStillRefuses(t *testing.T) {
	mc := newMockClient()
	issueWithMR(mc, "gd-c3u76usvml", openMRTestURL, "")
	mc.ListBeadsResult = []*model.Bead{canonicalMRBead(openMRTestURL, map[string]any{"mr_state": "opened"})}
	withMockClient(t, mc)
	withMRLiveState(t, mrLiveOpen, nil)

	if _, err := execClose(t, "gd-c3u76usvml"); err == nil {
		t.Fatal("close was allowed for an MR GitLab confirms is still OPEN; want a refusal")
	}
	if len(mc.CloseBeadCalls) != 0 {
		t.Fatalf("CloseBead called %d times; want 0", len(mc.CloseBeadCalls))
	}
}

// TestClose_LiveCheckUnknown_KeepsRefusing proves the live check is fail-SAFE in
// the direction that matters: an unverifiable MR (no token, network error,
// non-200 — every failure mode returns mrLiveUnknown) must NOT become an
// acquittal. Absence of evidence is not evidence of a merge, so the projection's
// `opened` verdict stands.
func TestClose_LiveCheckUnknown_KeepsRefusing(t *testing.T) {
	mc := newMockClient()
	issueWithMR(mc, "gd-c3u76usvml", openMRTestURL, "")
	mc.ListBeadsResult = []*model.Bead{canonicalMRBead(openMRTestURL, map[string]any{"mr_state": "opened"})}
	withMockClient(t, mc)
	withMRLiveState(t, mrLiveUnknown, nil)

	if _, err := execClose(t, "gd-c3u76usvml"); err == nil {
		t.Fatal("an UNVERIFIABLE live check acquitted an `opened` MR; want the refusal to stand")
	}
	if len(mc.CloseBeadCalls) != 0 {
		t.Fatalf("CloseBead called %d times; want 0", len(mc.CloseBeadCalls))
	}
}

// TestClose_FallbackIssueState_LiveMergedAcquits covers the SECOND projection
// path: no canonical MR bead yet, so the guard falls back to the issue bead's
// own (also bridge-fed, also laggy) mr_state. A brand-new MR merged before the
// bridge ever projected it must close cleanly.
func TestClose_FallbackIssueState_LiveMergedAcquits(t *testing.T) {
	mc := newMockClient()
	issueWithMR(mc, "gd-c3u76usvml", openMRTestURL, "opened")
	// ListBeadsResult left nil → findMRBead returns (nil,nil) → fallback path.
	withMockClient(t, mc)
	withMRLiveState(t, mrLiveMerged, nil)

	out, err := execClose(t, "gd-c3u76usvml")
	if err != nil {
		t.Fatalf("fallback path refused a bead whose MR is merged on GitLab: %v\n%s", err, out)
	}
	if len(mc.CloseBeadCalls) != 1 {
		t.Fatalf("CloseBead called %d times; want 1", len(mc.CloseBeadCalls))
	}
}

// TestClose_NoLiveCheckOnCleanClose proves the authoritative check costs nothing
// on the common path: a merged-projection close and a no-MR close must make ZERO
// network calls. The check runs only when we are otherwise about to refuse.
func TestClose_NoLiveCheckOnCleanClose(t *testing.T) {
	for name, setup := range map[string]func(mc *mockBeadsClient){
		"merged projection": func(mc *mockBeadsClient) {
			issueWithMR(mc, "gd-c3u76usvml", openMRTestURL, "")
			mc.ListBeadsResult = []*model.Bead{canonicalMRBead(openMRTestURL, map[string]any{"mr_state": "merged"})}
		},
		"no mr_url": func(mc *mockBeadsClient) {
			mc.Beads["gd-c3u76usvml"] = &model.Bead{ID: "gd-c3u76usvml", Status: model.StatusInProgress}
		},
	} {
		t.Run(name, func(t *testing.T) {
			mc := newMockClient()
			setup(mc)
			withMockClient(t, mc)
			var checked []string
			withMRLiveState(t, mrLiveMerged, &checked)

			if _, err := execClose(t, "gd-c3u76usvml"); err != nil {
				t.Fatalf("close errored: %v", err)
			}
			if len(checked) != 0 {
				t.Errorf("live GitLab check ran %d time(s) on a clean close (%v); want 0 — no network cost on the common path", len(checked), checked)
			}
		})
	}
}

// TestClose_RefusalNamesBothOverrideCases locks the wording fix (defect 2 of
// ka-0000004113). The refusal used to offer only "the MR was abandoned", but the
// dominant real trigger is the opposite — already merged, projection behind — so
// an agent who did the right thing was told to assert a falsehood. Both cases
// must be named.
func TestClose_RefusalNamesBothOverrideCases(t *testing.T) {
	mc := newMockClient()
	issueWithMR(mc, "gd-c3u76usvml", openMRTestURL, "")
	mc.ListBeadsResult = []*model.Bead{canonicalMRBead(openMRTestURL, map[string]any{"mr_state": "opened"})}
	withMockClient(t, mc)
	withMRLiveState(t, mrLiveOpen, nil)

	_, err := execClose(t, "gd-c3u76usvml")
	if err == nil {
		t.Fatal("want a refusal")
	}
	if !strings.Contains(err.Error(), "abandoned") {
		t.Errorf("refusal = %q; want it to still name the abandoned-MR case", err.Error())
	}
	if !strings.Contains(err.Error(), "caught up") {
		t.Errorf("refusal = %q; want it to ALSO name the already-merged-but-projection-lagging case", err.Error())
	}
}

// TestClose_StaleIssueStateOverriddenByMergedCanonical is the anti-false-positive
// case behind advice ka-0000000519: the issue bead keeps a STALE mr_state=opened
// after its MR merged, but the canonical MR bead says merged — the canonical
// state wins, so the close proceeds. This is the whole reason the guard trusts
// the canonical bead over the issue field.
func TestClose_StaleIssueStateOverriddenByMergedCanonical(t *testing.T) {
	mc := newMockClient()
	issueWithMR(mc, "gd-c3u76usvml", openMRTestURL, "opened") // STALE issue field
	mc.ListBeadsResult = []*model.Bead{canonicalMRBead(openMRTestURL, map[string]any{"mr_state": "merged"})}
	withMockClient(t, mc)

	out, err := execClose(t, "gd-c3u76usvml")
	if err != nil {
		t.Fatalf("stale issue mr_state=opened must be overridden by a MERGED canonical bead: %v\n%s", err, out)
	}
	if len(mc.CloseBeadCalls) != 1 {
		t.Fatalf("CloseBead called %d times; want 1 (canonical merged wins over stale issue state)", len(mc.CloseBeadCalls))
	}
}
