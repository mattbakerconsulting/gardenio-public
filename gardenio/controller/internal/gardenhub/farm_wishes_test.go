package gardenhub

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	"gardenio/controller/internal/beadsapi"
)

// wishBead builds a thread head: a type=task bead with the thread label and a source.
func wishBead(id, status, source string, upd int64) *beadsapi.BeadDetail {
	b := fb(id, "task", status, "", upd)
	b.Labels = append(b.Labels, "source:"+source, "thread")
	return b
}

// addFixtureWishes adds the fixture world's wishes (gd-EGpbDpqsec).
func addFixtureWishes(f *fakeFarmSrc) {
	// Thread heads are never plants. One gave rise to kin on two of plan A's plots and in a seed, and is
	// tended (its kin gd-A1a is in review); one from the Commons has nothing yet. Added last, so no other bead's created_at
	// moves.
	wish1 := fb("gd-wish1", "task", "open", "", 310)
	wish1.Title, wish1.ShortTitle = "Slack thread: can the exchange run behind the edge?", "exchange behind the edge"
	wish1.Labels = append(wish1.Labels, "source:slack", "thread")
	wish1.Fields["slack_user_id"] = "U1"
	wish2 := fb("gd-wish2", "task", "open", "", 320)
	wish2.Title = "Commons thread: a pen for the goats"
	wish2.Labels = append(wish2.Labels, "source:commons", "thread")
	// The herder has judged both (gd-RkkGXAz643): wish 1 in progress (its kin is in review), wish 2 fulfilled, so the goat's
	// tip and the pen's sign say it is about to close.
	wish1.Fields["herder_verdict"], wish1.Fields["herder_checked_at"] = "in-progress", time.Unix(baseUnix+9_400, 0).UTC().Format(time.RFC3339)
	wish2.Fields["herder_verdict"], wish2.Fields["herder_checked_at"] = "fulfilled", time.Unix(baseUnix+9_400, 0).UTC().Format(time.RFC3339)
	f.add(wish1, wish2)
	for _, id := range []string{"gd-A1a", "gd-A9", "gd-S1t"} {
		f.mutate(id, func(b *beadsapi.BeadDetail) { b.Fields["thread_id"] = "gd-wish1" })
	}
	// gardenio's graveyard (3.2, gd-s9KzRk6boh): three closed wishes, one per outcome. The done one's kin, a loose closed
	// step, merged an MR, so its headstone's hover lists it. They and the kin count in closed_work (the closed pile).
	done := fb("gd-wishx1", "task", "closed", "", 330)
	done.Title, done.ShortTitle = "Slack thread: can the goats have a pen?", "a pen for the goats"
	done.Labels = append(done.Labels, "source:slack", "thread")
	done.Fields["close_reason"], done.ClosedAt = "slack-thread-user-close", time.Unix(baseUnix+9_000, 0).UTC().Format(time.RFC3339)
	kin := fb("gd-X1k", "task", "closed", "", 329)
	kin.Title = "Fence the pen by the Yard"
	kin.Fields["thread_id"], kin.Fields["mr_url"], kin.Fields["mr_iid"], kin.Fields["mr_state"], kin.Fields["mr_title"] = "gd-wishx1", "https://example.com/mr/1124", "#1124", "merged", "The pen: a fenced band by the Yard"
	gave := fb("gd-wishx2", "task", "closed", "", 331)
	gave.Title = "Slack thread: retry the nightly e2e"
	gave.Labels = append(gave.Labels, "source:slack", "thread")
	gave.Fields["triage_reason"], gave.ClosedAt = "slack-thread-agent-failed", time.Unix(baseUnix+8_000, 0).UTC().Format(time.RFC3339)
	idle := fb("gd-wishx3", "task", "closed", "", 332)
	idle.Title = "Commons thread: a quieter night sky"
	idle.Labels = append(idle.Labels, "source:commons", "thread")
	idle.Fields["close_reason"], idle.ClosedAt = "slack-thread-idle", time.Unix(baseUnix+6_000, 0).UTC().Format(time.RFC3339)
	f.add(done, kin, gave, idle)
}

// TestFarmWishes pins the wishes blob (gd-EGpbDpqsec Phase 1): a thread head
// is a wish, never a meadow flower; its kin are the rows that carry its id as
// thread_id, with the plots they are drawn on; a wish nobody tends is untended;
// a closed head is a headstone with its outcome and kin count.
func TestFarmWishes(t *testing.T) {
	seq = 0
	src := newFakeFarm()
	src.add(planBead("gd-planA", "building", "in_progress", "gd-A1"), fb("gd-A1", "epic", "open", "gd-planA", 40))
	src.add(planBead("gd-planB", "building", "in_progress", "gd-B1"), fb("gd-B1", "epic", "open", "gd-planB", 40))
	src.add(planBead("gd-seed", "draft", "open", "gd-S1"), fb("gd-S1", "epic", "open", "gd-seed", 40))

	// Wish 1: kin on two plans (a step in review on A, an open step on B, the plan epic B itself), a kin in a seed (no plot),
	// and two closed kin. The newest kin lists first.
	w1 := wishBead("gd-wish1", "open", "slack", 500)
	w1.ShortTitle = "exchange behind the edge"
	a1 := fb("gd-A1a", "task", "in_progress", "gd-A1", 900)
	a1.Fields["mr_url"], a1.Fields["mr_state"] = "https://example.com/mr/1", "opened"
	b1 := fb("gd-B1a", "task", "open", "gd-B1", 800)
	s1 := fb("gd-S1a", "task", "open", "gd-S1", 700)
	c1 := fb("gd-A1x", "task", "closed", "gd-A1", 20)
	c2 := fb("gd-B1x", "task", "closed", "gd-B1", 21)
	for _, b := range []*beadsapi.BeadDetail{a1, b1, s1, c1, c2} {
		b.Fields["thread_id"] = "gd-wish1"
	}
	src.mutate("gd-planB", func(b *beadsapi.BeadDetail) { b.Fields["thread_id"] = "gd-wish1" })
	// Wish 2: nothing yet, from the Commons. Wish 3: a kin that is open but not in progress: untended.
	w2 := wishBead("gd-wish2", "open", "commons", 600)
	w3 := wishBead("gd-wish3", "open", "slack", 400)
	w3.Labels = []string{"project:zeta", "thread"}
	// The herder's verdicts (gd-RkkGXAz643): wish 1 unfulfilled, its agent woken that pass (as the herder writes it, trimmed
	// and lowercased here); wish 2 fulfilled; wish 3 a verdict the farm does not know, which is none.
	w1.Fields["herder_verdict"], w1.Fields["herder_checked_at"], w1.Fields["herder_woke_at"] = " Unfulfilled", "2026-11-01T04:49:55Z", "2026-11-01T04:50:10Z"
	w2.Fields["herder_verdict"], w2.Fields["herder_checked_at"] = "fulfilled", "2026-11-01T05:00:00Z"
	w3.Fields["herder_verdict"], w3.Fields["herder_checked_at"] = "judged", "2026-11-01T05:00:00Z"
	z := fb("gd-Z1", "task", "open", "", 300)
	z.Fields["thread_id"] = "gd-wish3"
	// Closed wishes: autoclosed idle with one closed kin; closed by a reaction; abandoned after its agent failed.
	x1 := wishBead("gd-wishx1", "closed", "slack", 30)
	x1.Fields["close_reason"] = "slack-thread-idle"
	x1.ClosedAt = time.Unix(baseUnix+35, 0).UTC().Format(time.RFC3339)
	x1k := fb("gd-X1k", "task", "closed", "", 25)
	x1k.Fields["thread_id"] = "gd-wishx1"
	// What its kin merged: a loose kin's merged MR is listed; an MR still open is not produce yet.
	x1k.Fields["mr_url"], x1k.Fields["mr_iid"], x1k.Fields["mr_state"], x1k.Fields["mr_title"] = "https://example.com/mr/77", "#77", "merged", "Idle threads close themselves"
	x2 := wishBead("gd-wishx2", "closed", "slack", 40)
	x2.Fields["close_reason"] = "slack-thread-user-close"
	x3 := wishBead("gd-wishx3", "closed", "commons", 50)
	x3.Fields["triage_reason"] = "slack-thread-agent-failed"
	// A loose bead that only looks like a thread (no label) stays a meadow flower.
	m := fb("gd-M1", "task", "open", "", 100)
	m.Title = "Slack thread: not a head"
	src.add(w1, a1, b1, s1, c1, c2, w2, w3, z, x1, x1k, x2, x3, m)

	f := newTestFarm(src)
	if err := f.refresh(context.Background()); err != nil {
		t.Fatal(err)
	}
	s := decodeSnap(t, f)

	// Heads are not loose work and not plants: the meadow holds the look-alike and wish 3's kin only.
	if s.Meadow.Total != 2 || len(s.Meadow.Shown) != 2 {
		t.Fatalf("meadow should hold only gd-M1 and gd-Z1: %+v", s.Meadow)
	}
	for _, p := range s.Plans {
		for _, ph := range append(p.Phases, p.Unphased) {
			for _, task := range ph.Tasks {
				if strings.HasPrefix(task.ID, "gd-wish") {
					t.Fatalf("a wish is drawn as a plant: %s on %s", task.ID, p.ID)
				}
			}
		}
	}

	w := s.Wishes
	if w.OpenTotal != 3 || len(w.Open) != 3 || w.OpenMore != nil || w.Fulfilled != 1 {
		t.Fatalf("open wishes: %+v", w)
	}
	// Newest asked first.
	if w.Open[0].ID != "gd-wish3" || w.Open[1].ID != "gd-wish2" || w.Open[2].ID != "gd-wish1" {
		t.Fatalf("open order: %s %s %s", w.Open[0].ID, w.Open[1].ID, w.Open[2].ID)
	}
	got := w.Open[2]
	if got.Title != "exchange behind the edge" || got.Source != WishFromSlack || got.Asker != "oidc:matt@example.com" || got.Project != "gardenio" {
		t.Fatalf("wish 1 head: %+v", got)
	}
	if got.Href != "/garden/c/bead/gd-wish1" || got.Opened == 0 || got.Updated != baseUnix+500 {
		t.Fatalf("wish 1 href/times: %+v", got)
	}
	// Kin: the four open rows newest change first (A1a 900, B1a 800, S1a 700, planB 50); two closed ones counted.
	if len(got.Kin) != 4 || got.Kin[0] != "gd-A1a" || got.Kin[1] != "gd-B1a" || got.Kin[2] != "gd-S1a" || got.Kin[3] != "gd-planB" {
		t.Fatalf("wish 1 kin: %v", got.Kin)
	}
	if got.KinTotal != 4 || got.KinClosed != 2 || !got.Tending {
		t.Fatalf("wish 1 counts: %+v", got)
	}
	// Plots: A's phase, B's phase, then plan B itself; the seed's step has none.
	wantPlots := []FarmPlot{{"gd-planA", "gd-A1"}, {"gd-planB", "gd-B1"}, {"gd-planB", ""}}
	if len(got.Plots) != len(wantPlots) {
		t.Fatalf("wish 1 plots: %+v", got.Plots)
	}
	for i := range wantPlots {
		if got.Plots[i] != wantPlots[i] {
			t.Fatalf("wish 1 plots: %+v", got.Plots)
		}
	}
	if w2 := w.Open[1]; w2.Source != WishFromCommons || w2.Title != "Title gd-wish2" || len(w2.Kin) != 0 || w2.KinTotal != 0 || w2.Tending || len(w2.Plots) != 0 {
		t.Fatalf("wish 2: %+v", w2)
	}
	if w3 := w.Open[0]; w3.Project != "zeta" || w3.KinTotal != 1 || w3.Tending || len(w3.Plots) != 0 {
		t.Fatalf("wish 3 (untended, kin in the meadow): %+v", w3)
	}
	checkFarmWishesHerder(t, w.Open)

	// Closed: newest close first; the outcome from the close evidence; kin counted across open and closed rows.
	if w.ClosedTotal != 3 || len(w.Closed) != 3 {
		t.Fatalf("closed wishes: %+v", w)
	}
	if w.Closed[0].ID != "gd-wishx3" || w.Closed[1].ID != "gd-wishx2" || w.Closed[2].ID != "gd-wishx1" {
		t.Fatalf("closed order: %+v", w.Closed)
	}
	if x := w.Closed[2]; x.Outcome != WishExpired || x.Kin != 1 || x.Closed != baseUnix+35 || x.Source != WishFromSlack || x.Reason != "slack-thread-idle" || x.Href != "/garden/c/bead/gd-wishx1" {
		t.Fatalf("expired wish: %+v", x)
	}
	// The headstone's hover lists what its kin merged (the produce read's MR rows, by thread_id).
	if x := w.Closed[2]; len(x.Produce) != 1 || x.Produce[0].URL != "https://example.com/mr/77" || x.Produce[0].IID != "#77" || !x.Produce[0].Ready || x.Produce[0].Title != "Idle threads close themselves" || x.ProduceMore != 0 {
		t.Fatalf("expired wish's produce: %+v", x.Produce)
	}
	if x := w.Closed[1]; x.Outcome != WishDone || x.Kin != 0 || x.Closed != baseUnix+40 || x.Reason != "slack-thread-user-close" || len(x.Produce) != 0 {
		t.Fatalf("done wish: %+v", x)
	}
	if x := w.Closed[0]; x.Outcome != WishAbandoned || x.Source != WishFromCommons || x.Reason != "slack-thread-agent-failed" {
		t.Fatalf("abandoned wish: %+v", x)
	}
	// Wish 1 is open: its kin's opened MR is no stone's produce.
	if len(f.slow.wishProduce.byWish["gd-wish1"]) != 0 {
		t.Fatalf("an opened MR is not produce: %+v", f.slow.wishProduce.byWish["gd-wish1"])
	}

	// A head closing leaves the open set and joins the stones on the next refresh; the blob rides a patch.
	_, live, cancel := f.Subscribe(0)
	defer cancel()
	src.mutate("gd-wish2", func(b *beadsapi.BeadDetail) {
		b.Status, b.UpdatedAt = "closed", time.Unix(baseUnix+9_999, 0) // after the closed delta's cursor (now - farmClosedCursorSlack)
		b.Fields["close_reason"] = "slack-thread-user-close"
	})
	if err := f.refresh(context.Background()); err != nil {
		t.Fatal(err)
	}
	select {
	case msg := <-live:
		var p struct {
			Wishes *FarmWishes `json:"wishes"`
		}
		if err := json.Unmarshal(msg.Data, &p); err != nil || p.Wishes == nil {
			t.Fatalf("patch should carry wishes: %v %s", err, msg.Data)
		}
		if p.Wishes.OpenTotal != 2 || p.Wishes.ClosedTotal != 4 || p.Wishes.Closed[0].ID != "gd-wish2" || p.Wishes.Closed[0].Outcome != WishDone {
			t.Fatalf("after the close: %+v", p.Wishes)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("no patch")
	}
}

// checkFarmWishesHerder checks TestFarmWishes' herder verdicts on its open wishes, newest asked first (gd-RkkGXAz643).
func checkFarmWishesHerder(t *testing.T, open []FarmWish) {
	t.Helper()
	checked := time.Date(2026, 11, 1, 4, 49, 55, 0, time.UTC).Unix()
	if w1 := open[2]; w1.HerderVerdict != HerderUnfulfilled || w1.HerderCheckedAt != checked || !w1.HerderActed {
		t.Fatalf("wish 1's herder verdict: %+v", w1)
	}
	if w2 := open[1]; w2.HerderVerdict != HerderFulfilled || w2.HerderCheckedAt != checked+10*60+5 || w2.HerderActed {
		t.Fatalf("wish 2's herder verdict: %+v", w2)
	}
	if w3 := open[0]; w3.HerderVerdict != "" || w3.HerderCheckedAt != 0 {
		t.Fatalf("wish 3's unknown verdict is none: %+v", w3)
	}
}

// TestWishHerder pins the herder's verdict on a head (gd-RkkGXAz643): the four
// verdicts the herder gives, any other none; "agent woken" or "asked" only on
// an unfulfilled or unclear verdict whose wake is no older than a day before it.
func TestWishHerder(t *testing.T) {
	const at = "2026-11-01T05:00:00Z"
	checked := time.Date(2026, 11, 1, 5, 0, 0, 0, time.UTC).Unix()
	for _, c := range []struct {
		verdict, woke string
		want          string
		acted         bool
	}{
		{"fulfilled", "", HerderFulfilled, false},
		{"in-progress", at, HerderInProgress, false},
		{"unfulfilled", "2026-11-01T05:00:30Z", HerderUnfulfilled, true},
		{"unclear", "2026-10-31T06:00:00Z", HerderUnclear, true},  // woken the day before: this pass did not wake again
		{"unclear", "2026-10-30T04:00:00Z", HerderUnclear, false}, // an old wake is not this verdict's
		{"unfulfilled", "not a time", HerderUnfulfilled, false},
		{"stale", at, "", false},
		{"", at, "", false},
	} {
		b := wishBead("gd-h", "open", "slack", 1)
		b.Fields["herder_verdict"], b.Fields["herder_checked_at"], b.Fields["herder_woke_at"] = c.verdict, at, c.woke
		v, ch, acted := WishHerder(b)
		wantAt := checked
		if c.want == "" {
			wantAt = 0
		}
		if v != c.want || ch != wantAt || acted != c.acted {
			t.Errorf("%q woke %q: got %q %d %v, want %q %d %v", c.verdict, c.woke, v, ch, acted, c.want, wantAt, c.acted)
		}
	}
	// In words, as the goat's tip says them (goats_test.go pins web/goats.js herderWords to the same).
	for _, c := range []struct {
		verdict string
		acted   bool
		want    string
	}{
		{HerderFulfilled, false, "fulfilled, closing"}, {HerderInProgress, true, "in progress"},
		{HerderUnfulfilled, true, "unfulfilled, agent woken"}, {HerderUnfulfilled, false, "unfulfilled"},
		{HerderUnclear, true, "unclear, asked"}, {HerderUnclear, false, "unclear"}, {"", true, ""},
	} {
		if got := HerderWords(c.verdict, c.acted); got != c.want {
			t.Errorf("HerderWords(%q, %v) = %q, want %q", c.verdict, c.acted, got, c.want)
		}
	}
}

// TestFarmWishesCapsPerProject pins the per-project caps: the newest
// maxWishesPerProject heads of a project are listed and the rest counted in
// open_more; another project's heads are unaffected.
func TestFarmWishesCapsPerProject(t *testing.T) {
	seq = 0
	src := newFakeFarm()
	for i := range maxWishesPerProject + 3 {
		src.add(wishBead(fmt.Sprintf("gd-w%d", i), "open", "slack", int64(i)))
	}
	z := wishBead("gd-wz", "open", "slack", 1)
	z.Labels = []string{"project:zeta", "thread"}
	src.add(z)
	// The oldest, which the cap leaves out, is one the herder judged fulfilled: the pen's count still has it.
	src.mutate("gd-w0", func(b *beadsapi.BeadDetail) { b.Fields["herder_verdict"] = HerderFulfilled })
	f := newTestFarm(src)
	if err := f.refresh(context.Background()); err != nil {
		t.Fatal(err)
	}
	w := decodeSnap(t, f).Wishes
	if w.OpenTotal != maxWishesPerProject+4 || len(w.Open) != maxWishesPerProject+1 || w.OpenMore["gardenio"] != 3 || w.OpenMore["zeta"] != 0 || w.Fulfilled != 1 {
		t.Fatalf("caps: total=%d listed=%d more=%v fulfilled=%d", w.OpenTotal, len(w.Open), w.OpenMore, w.Fulfilled)
	}
	// Newest asked first, so the ones left out are the oldest.
	if w.Open[0].ID != "gd-wz" || w.Open[1].ID != fmt.Sprintf("gd-w%d", maxWishesPerProject+2) {
		t.Fatalf("order: %s %s", w.Open[0].ID, w.Open[1].ID)
	}
}

// TestFarmWishArms pins FarmWish.arms (heraldry 5.10): the line arms of its kin in kin order, at most two distinct runs, read
// through the line-arms cache; a wish whose kin's lines bear none, or that has no kin, shows none and never its own.
func TestFarmWishArms(t *testing.T) {
	seq = 0
	src := newFakeFarm()
	pa, pb, pc := planBead("gd-planA", "building", "in_progress", "gd-A1"), planBead("gd-planB", "building", "in_progress"), planBead("gd-planC", "building", "in_progress")
	pa.Heraldry, pb.Heraldry, pc.Heraldry = planArms, phaseArms, "🌱🛡️🔥"
	src.add(pa, pb, pc, fb("gd-A1", "epic", "open", "gd-planA", 40))
	// Wish 1: kin on B (newest), twice on A (once through its unarmed phase), on C: B's run, then A's; C's is a third.
	w1 := wishBead("gd-wish1", "open", "slack", 500)
	w1.Heraldry = "🎯🎯🎯" // a head is a task: never its own arms (R6)
	kin := []*beadsapi.BeadDetail{fb("gd-B1a", "task", "in_progress", "gd-planB", 900), fb("gd-A1a", "task", "open", "gd-A1", 800),
		fb("gd-A1b", "task", "open", "gd-planA", 700), fb("gd-C1a", "task", "open", "gd-planC", 600)}
	// Wish 2: a loose kin, whose line bears none. Wish 3: no kin.
	w2, w3 := wishBead("gd-wish2", "open", "slack", 510), wishBead("gd-wish3", "open", "slack", 520)
	loose := fb("gd-L1", "task", "open", "", 300)
	loose.Fields["thread_id"] = "gd-wish2"
	for _, b := range kin {
		b.Fields["thread_id"] = "gd-wish1"
	}
	src.add(w1, w2, w3, loose)
	src.add(kin...)
	f := newTestFarm(src)
	if err := f.refresh(context.Background()); err != nil {
		t.Fatal(err)
	}
	arms := map[string][]string{}
	for _, w := range decodeSnap(t, f).Wishes.Open {
		for _, a := range w.Arms {
			if a.N != 3 || a.Name == "" {
				t.Errorf("%s: arms %+v want n and a name", w.ID, a)
			}
			arms[w.ID] = append(arms[w.ID], a.Canonical)
		}
	}
	if got := arms["gd-wish1"]; len(got) != 2 || got[0] != phaseArms || got[1] != planArms {
		t.Fatalf("wish 1 arms %q, want B's then A's", got)
	}
	if len(arms["gd-wish2"]) != 0 || len(arms["gd-wish3"]) != 0 {
		t.Fatalf("unarmed wishes show arms: %v", arms)
	}
	// A second refresh inside the cache's TTL reads no bead again for the arms.
	gets := src.getCalls
	if err := f.refresh(context.Background()); err != nil {
		t.Fatal(err)
	}
	if src.getCalls != gets {
		t.Fatalf("refresh within the TTL re-read beads: %d -> %d", gets, src.getCalls)
	}
}

// TestWishProduce pins wishProduce: merged MRs only (an opened or closed-unmerged one is not produce), one per URL, newest
// first, keyed by the thread_id; a row naming itself or no wish is skipped.
func TestWishProduce(t *testing.T) {
	seq = 0
	mr := func(id, wish, url, state string, upd int64) *beadsapi.BeadDetail {
		b := fb(id, "task", "closed", "", upd)
		b.Fields["thread_id"], b.Fields["mr_url"], b.Fields["mr_state"] = wish, url, state
		return b
	}
	got := wishProduce([]*beadsapi.BeadDetail{
		mr("gd-a", "gd-w", "https://example.com/mr/1", "merged", 10),
		mr("gd-b", "gd-w", "https://example.com/mr/2", "merged", 30),
		mr("gd-c", "gd-w", "https://example.com/mr/1", "merged", 20), // the same MR again
		mr("gd-d", "gd-w", "https://example.com/mr/3", "opened", 40),
		mr("gd-e", "gd-w", "https://example.com/mr/4", "closed", 50),
		mr("gd-f", "", "https://example.com/mr/5", "merged", 60),
		mr("gd-g", "gd-g", "https://example.com/mr/6", "merged", 70),
		nil,
	})
	if len(got.byWish) != 1 || len(got.byWish["gd-w"]) != 2 || got.byWish["gd-w"][0].ID != "gd-b" || got.byWish["gd-w"][1].ID != "gd-a" {
		t.Fatalf("wish produce: %+v", got)
	}
}
