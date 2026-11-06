package main

// Regression guard for the gardener-owner N+1 (perf fix ka-0000003665).
//
// The per-user gardener ownership feature (ka-0000002818) resolves every
// gardener row's owner. Before the gardenerOwnerCache fix, each Slack-owned row
// triggered an uncached LookupOperatorByAlias PLUS a full ListOperators
// directory scan — so /gardeners with N gardeners issued O(N) alias lookups and
// O(N) full-directory fetches. These tests assert the daemon-call count stays
// BOUNDED as the gardener count grows, which is the property a future
// re-introduction of the N+1 would break.
//
// We assert on deterministic call COUNTS rather than wall-clock ns/op so the
// guard is stable on shared CI hardware (the perf_bench_test.go benchmarks are
// the advisory ns/op signal; this is the hard gate).

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"testing"
	"time"

	"gardenio/controller/internal/beadsapi"
)

// seedSlackOwnedGardeners builds a fakeDaemon serving n gardeners that ALL
// resolve their owner through the Slack-alias tier (GardenerSpec.SlackUserID),
// distributed across `distinctOwners` operators. The Slack tier is the
// expensive one: it hits LookupOperatorByAlias and the full ListOperators
// directory scan. distinctOwners < n exercises the per-owner memo; an equal
// value exercises the shared-directory memo on all-distinct owners.
func seedSlackOwnedGardeners(t *testing.T, n, distinctOwners int) *fakeDaemon {
	t.Helper()
	return newFakeSlackOwnedGardeners(n, distinctOwners)
}

// newFakeSlackOwnedGardeners is the testing.T-free body of seedSlackOwnedGardeners
// so the perf benchmarks (perf_bench_test.go) can reuse the same fixture.
func newFakeSlackOwnedGardeners(n, distinctOwners int) *fakeDaemon {
	if distinctOwners < 1 {
		distinctOwners = 1
	}
	f := newFake()

	dir := make([]beadsapi.OperatorDirectoryEntry, 0, distinctOwners)
	aliases := make(map[string]*beadsapi.LookupOperatorByAliasResponse, distinctOwners)
	slugs := make(map[int64]*beadsapi.GetOperatorSlugResponse, distinctOwners)
	for o := range distinctOwners {
		opID := int64(100 + o)
		uid := "U" + strconv.Itoa(o)
		aliases["slack|"+uid] = &beadsapi.LookupOperatorByAliasResponse{OperatorID: opID, Found: true}
		slugs[opID] = &beadsapi.GetOperatorSlugResponse{Slug: "owner" + strconv.Itoa(o), Found: true}
		dir = append(dir, beadsapi.OperatorDirectoryEntry{
			ID: opID, Display: "Owner " + strconv.Itoa(o), Email: "owner" + strconv.Itoa(o) + "@acme.example",
		})
	}
	f.aliasResp = aliases
	f.opSlugResp = slugs
	f.listOperatorsResp = &beadsapi.ListOperatorsResponse{Operators: dir}

	beads := make([]*beadsapi.BeadDetail, 0, n)
	for i := range n {
		uid := "U" + strconv.Itoa(i%distinctOwners)
		spec := map[string]any{
			"name":          "gardener-" + strconv.Itoa(i),
			"terminal_goal": "goal",
			"slack_user_id": uid,
			"runs_spawned":  3,
		}
		raw, _ := json.Marshal(spec)
		beads = append(beads, &beadsapi.BeadDetail{
			ID:        "gd-cap" + strconv.Itoa(i),
			Title:     "gardener-" + strconv.Itoa(i),
			Type:      "trigger",
			Status:    "open",
			Labels:    []string{"trigger:schedule", "role:gardener", "project:gardenio"},
			Fields:    map[string]string{"role": "gardener", "gardener": string(raw)},
			UpdatedAt: time.Date(2026, 6, 1, 11, 0, 0, 0, time.UTC),
		})
	}
	f.triggerScheduleResp = &beadsapi.ListBeadsResult{Beads: beads, Total: n}
	return f
}

func gardenerOwnerCalls(f *fakeDaemon) (listOps, alias int) {
	f.getBeadMu.Lock()
	listOps = f.listOperatorsCalls
	f.getBeadMu.Unlock()
	return listOps, f.aliasCalls
}

// TestHandleGardeners_OwnerResolutionIsBounded renders /gardeners with a large
// synthetic gardener set and asserts the expensive owner-resolution daemon
// calls stay bounded (independent of gardener count) — the property the N+1
// fix restored. With the cache, the operator directory is fetched at most a
// small constant number of times and each distinct Slack owner is looked up
// once, regardless of how many gardeners share them.
func TestHandleGardeners_OwnerResolutionIsBounded(t *testing.T) {
	const (
		gardeners      = 300
		distinctOwners = 5
	)
	f := seedSlackOwnedGardeners(t, gardeners, distinctOwners)
	mux := mkServerWithNow(t, f, gardenersNow)

	req := httptest.NewRequest(http.MethodGet, "/gardeners", nil)
	req.Header.Set("X-Auth-Request-Email", "alice@example.com")
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", w.Code, w.Body.String())
	}

	listOps, alias := gardenerOwnerCalls(f)

	// Directory scan: ONE shared load serves every row. Allow a tiny slack
	// constant for any incidental directory use elsewhere on the page, but
	// well below the pre-fix O(gardeners) blow-up.
	if listOps > 2 {
		t.Errorf("ListOperators called %d times for %d gardeners — N+1 directory scan regressed (want <=2)", listOps, gardeners)
	}
	// Alias lookups: one per DISTINCT owner, not one per gardener.
	if alias > distinctOwners {
		t.Errorf("LookupOperatorByAlias called %d times for %d distinct owners — per-row alias N+1 regressed (want <=%d)", alias, distinctOwners, distinctOwners)
	}
	// Hard ceiling proving independence from gardener count.
	if listOps >= gardeners || alias >= gardeners {
		t.Errorf("owner-resolution calls scale with gardener count (listOps=%d alias=%d, gardeners=%d)", listOps, alias, gardeners)
	}
}

// TestHandleGardeners_OwnerResolutionScalesSublinearly renders the same page at
// two gardener counts and asserts the daemon-call totals do NOT grow with the
// gardener count — a direct, fixture-count-independent encoding of "no N+1".
func TestHandleGardeners_OwnerResolutionScalesSublinearly(t *testing.T) {
	render := func(n int) (listOps, alias int) {
		f := seedSlackOwnedGardeners(t, n, 3)
		mux := mkServerWithNow(t, f, gardenersNow)
		req := httptest.NewRequest(http.MethodGet, "/gardeners", nil)
		req.Header.Set("X-Auth-Request-Email", "alice@example.com")
		w := httptest.NewRecorder()
		mux.ServeHTTP(w, req)
		if w.Code != http.StatusOK {
			t.Fatalf("n=%d status=%d body=%s", n, w.Code, w.Body.String())
		}
		return gardenerOwnerCalls(f)
	}

	smallList, smallAlias := render(20)
	bigList, bigAlias := render(400)

	// 20× the gardeners must not mean 20× the daemon calls. With the cache both
	// renders touch the directory the same bounded number of times and resolve
	// the same 3 distinct owners.
	if bigList > smallList {
		t.Errorf("ListOperators grew with gardener count: 20 gardeners -> %d, 400 gardeners -> %d", smallList, bigList)
	}
	if bigAlias > smallAlias {
		t.Errorf("LookupOperatorByAlias grew with gardener count: 20 gardeners -> %d, 400 gardeners -> %d", smallAlias, bigAlias)
	}
}
