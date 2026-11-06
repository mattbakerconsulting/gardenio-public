package gardenhub

import (
	"context"
	"errors"
	"fmt"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"
	"time"

	"gardenio/controller/internal/beadsapi"
)

// failAsks is a farm source whose batch read of a seed decision fails.
type failAsks struct{ *fakeFarmSrc }

func (f failAsks) BatchGetBeads(ctx context.Context, ids []string) (map[string]*beadsapi.BeadDetail, error) {
	if slices.Contains(ids, "gd-seed2ask") {
		return nil, errors.New("daemon down")
	}
	return f.fakeFarmSrc.BatchGetBeads(ctx, ids)
}

// seedWithAsk is fixtureWorld plus the steward's "run it?" decision on the
// ready seed gd-seed2 (steward.WritePlan: requesting_plan and
// requesting_task name the plan, the plan is blocked on it), and an older,
// unrelated decision that names no plan.
func seedWithAsk() *fakeFarmSrc {
	src := fixtureWorld()
	dec := fb("gd-seed2ask", "decision", "open", "", 70)
	dec.Title = "Plan gd-seed2: Local agents - run it?"
	dec.OperatedBy = "OIDC:Owner@Example.com"
	dec.Fields["requesting_plan"] = "gd-seed2"
	dec.Fields["requesting_task"] = "gd-seed2"
	dec.Fields["prompt"] = dec.Title
	dec.Fields["options"] = `[{"id":"approve","short":"Run the plan","label":"Run the plan: the steward starts its first step"},{"id":"change","short":"Change it first"},{"id":"drop","label":"Drop it"},{"short":"no id"}]`
	stray := fb("gd-stray", "decision", "open", "", 71)
	stray.Fields["requesting_task"] = "gd-M1"
	src.add(dec, stray)
	return src
}

// TestPotUnderChromium walks a staked plot's card in a real browser (testdata/pot_browser.js): the ready seed gd-seed2,
// staked out with two steps, carries the steward's "run it?" decision for the viewer; a desktop hovers the plot, reads its
// steps in the tip and answers approve from its card, and the plan is planted in place; a phone reads the plan's bead view
// (the card, then every step with its body and the narrative); a tablet's first tap opens the tip and its second goes in.
func TestPotUnderChromium(t *testing.T) {
	underChromium(t, "pot_browser.js", func() *httptest.Server {
		return cardWorldAs(t, time.Now, "oidc:matt@example.com", func(src *fakeFarmSrc) {
			src.mutate("gd-seed2", func(b *beadsapi.BeadDetail) {
				b.ShortTitle, b.Description = "local agents", "Run the gardener next to the owner, on their laptop."
				b.Fields["step_order"] = `["gd-seed2s1","gd-seed2s2"]`
			})
			s1, s2 := fb("gd-seed2s1", "task", "open", "gd-seed2", 71), fb("gd-seed2s2", "task", "open", "gd-seed2", 72)
			s1.Title, s1.Description = "Package the gardener", "A brew formula."
			s2.Title, s2.Description = "Reach the cluster through the edge", strings.Repeat("Through the edge, never a tunnel. ", 12)+"The end."
			src.add(s1, s2)
			dec := fb("gd-seed2ask", "decision", "open", "", 70)
			dec.Title = "Plan gd-seed2: Local agents - run it?"
			dec.OperatedBy = "oidc:matt@example.com"
			dec.Fields["requesting_plan"], dec.Fields["requesting_task"], dec.Fields["prompt"] = "gd-seed2", "gd-seed2", dec.Title
			dec.Fields["options"] = `[{"id":"approve","short":"Run the plan","label":"Run the plan: the steward starts its first step"},{"id":"change","short":"Change it first"},{"id":"drop","short":"Drop it"}]`
			src.add(dec)
		})
	})
}

func findSeed(s snap, id string) *FarmSeed {
	for i := range s.Seeds {
		if s.Seeds[i].ID == id {
			return &s.Seeds[i]
		}
	}
	return nil
}

func TestFarmSeedCarriesItsDecision(t *testing.T) {
	src := seedWithAsk()
	f := newTestFarm(src)
	ctx := context.Background()
	if err := f.refresh(ctx); err != nil {
		t.Fatal(err)
	}
	s := decodeSnap(t, f)
	sd := findSeed(s, "gd-seed2")
	if sd == nil || sd.State != SeedAsked || sd.Decision == nil {
		t.Fatalf("gd-seed2 = %+v, want state asked with its decision", sd)
	}
	d := sd.Decision
	if d.ID != "gd-seed2ask" || d.Prompt != "Run it?" || d.For != "oidc:owner@example.com" || d.Since == 0 {
		t.Errorf("decision = %+v, want gd-seed2ask, \"Run it?\", for the owner, since set", d)
	}
	want := []QueueOption{{ID: "approve", Text: "Run the plan", Label: "Run the plan: the steward starts its first step"}, {ID: "change", Text: "Change it first"}, {ID: "drop", Text: "Drop it", Label: "Drop it"}}
	if len(d.Options) != len(want) {
		t.Fatalf("options = %+v, want %+v", d.Options, want)
	}
	for i := range want {
		if d.Options[i] != want[i] {
			t.Errorf("option %d = %+v, want %+v", i, d.Options[i], want[i])
		}
	}
	if fr := findSeed(s, "gd-seed1"); fr == nil || fr.State != SeedFrosted || fr.Decision != nil {
		t.Errorf("gd-seed1 = %+v, want frosted with no decision", fr)
	}

	// The prompt and options are read once per decision, not per refresh.
	src.mu.Lock()
	gets := src.getCalls
	src.mu.Unlock()
	src.mutate("gd-seed2", func(b *beadsapi.BeadDetail) { b.Title = "Local agents" })
	if err := f.refresh(ctx); err != nil {
		t.Fatal(err)
	}
	src.mu.Lock()
	again := src.getCalls - gets
	src.mu.Unlock()
	if again != 0 {
		t.Errorf("a second refresh read the seed decision again (%d batch reads)", again)
	}
	if _, ok := f.seedAsks["gd-seed2ask"]; !ok {
		t.Error("the decision's card is not cached")
	}

	// Approve: the decision closes and plan_state goes building; the pot leaves
	// the Nursery for a plot in one patch, and the cache lets the decision go.
	src.mutate("gd-seed2ask", func(b *beadsapi.BeadDetail) { b.Status = "closed" })
	src.mutate("gd-seed2", func(b *beadsapi.BeadDetail) { b.Fields["plan_state"] = "building" })
	if err := f.refresh(ctx); err != nil {
		t.Fatal(err)
	}
	s = decodeSnap(t, f)
	if findSeed(s, "gd-seed2") != nil || findPlan(s, "gd-seed2") == nil {
		t.Fatalf("after approve: seed %v, plan %v; want the plan, not the seed", findSeed(s, "gd-seed2"), findPlan(s, "gd-seed2"))
	}
	if len(f.seedAsks) != 0 {
		t.Errorf("seedAsks = %v after the decision closed, want empty", f.seedAsks)
	}
}

func TestFarmSeedDecisionUnreadKeepsTitle(t *testing.T) {
	src := seedWithAsk()
	f := newTestFarm(failAsks{src})
	if err := f.refresh(context.Background()); err != nil {
		t.Fatal(err)
	}
	sd := findSeed(decodeSnap(t, f), "gd-seed2")
	if sd == nil || sd.Decision == nil || sd.Decision.Prompt != "Run it?" || len(sd.Decision.Options) != 0 {
		t.Fatalf("seed = %+v, want the decision from its title with no options", sd)
	}
	if len(f.seedAsks) != 0 {
		t.Errorf("a failed read was cached: %v", f.seedAsks)
	}
}

func TestSeedPrompt(t *testing.T) {
	for _, c := range []struct{ prompt, want string }{
		{"Plan gd-p: Goats - run it?", "Run it?"},
		{"  Plan gd-p: Goats - run it?  ", "Run it?"},
		{"Plan gd-q: Goats - run it?", "Plan gd-q: Goats - run it?"}, // another plan's: kept whole
		{"Plan gd-p: Goats - ", "Plan gd-p: Goats -"},
		{"Ship the goats?", "Ship the goats?"},
	} {
		if got := seedPrompt(c.prompt, "gd-p", "Goats"); got != c.want {
			t.Errorf("seedPrompt(%q) = %q, want %q", c.prompt, got, c.want)
		}
	}
}

// TestFarmSeedSteps: a ready seed carries its open steps in step_order (those
// it does not list after them, by created) and its description cut to a
// summary, and its short title; a frosted seed keeps counts only. The summary is read once per
// plan updated_at, and only for ready or asked seeds.
func TestFarmSeedSteps(t *testing.T) {
	src := fixtureWorld()
	src.mutate("gd-seed1", func(b *beadsapi.BeadDetail) { b.Description, b.ShortTitle = "A draft's narrative.", "a draft" })
	src.mutate("gd-seed2", func(b *beadsapi.BeadDetail) {
		b.ShortTitle = " local agents "
		b.Description = "  " + strings.Repeat("é", seedSummaryMax+50)
		b.Fields["step_order"] = `["gd-st3","gd-st1","gd-gone"]`
	})
	st1, st2, st3 := fb("gd-st1", "task", "open", "gd-seed2", 72), fb("gd-st2", "bug", "open", "gd-seed2", 73), fb("gd-st3", "task", "in_progress", "gd-seed2", 74)
	st3.Heraldry = "blue 🌱 . 🔥 . 📦"
	ask := fb("gd-st-ask", "decision", "open", "gd-seed2", 75) // a decision under the plan is not a step
	src.add(st1, st2, st3, ask)
	f := newTestFarm(src)
	ctx := context.Background()
	if err := f.refresh(ctx); err != nil {
		t.Fatal(err)
	}
	s := decodeSnap(t, f)
	sd := findSeed(s, "gd-seed2")
	if sd == nil || sd.State != SeedReady {
		t.Fatalf("gd-seed2 = %+v, want ready", sd)
	}
	want := []FarmSeedStep{
		{ID: "gd-st3", Title: "Title gd-st3", N: 1, Status: "in_progress", Heraldry: "blue 🌱 . 🔥 . 📦"},
		{ID: "gd-st1", Title: "Title gd-st1", N: 2, Status: "open"},
		{ID: "gd-st2", Title: "Title gd-st2", N: 3, Status: "open"}, // not in step_order: after, by created
	}
	if !slices.Equal(sd.Steps, want) {
		t.Errorf("steps = %+v, want %+v", sd.Steps, want)
	}
	if r := []rune(sd.Summary); len(r) != seedSummaryMax || r[0] != 'é' || r[len(r)-1] != '…' {
		t.Errorf("summary is %d runes (%q...), want the description cut at %d with an ellipsis", len(r), string(r[:min(4, len(r))]), seedSummaryMax)
	}
	if sd.ShortTitle != "local agents" {
		t.Errorf("short title = %q, want %q (its staked plot's sign)", sd.ShortTitle, "local agents")
	}
	if fr := findSeed(s, "gd-seed1"); fr == nil || fr.State != SeedFrosted || fr.Steps != nil || fr.Summary != "" || fr.ShortTitle != "" || fr.Tasks != 2 {
		t.Errorf("gd-seed1 = %+v, want frosted with its 2 tasks counted and no steps, summary or short title", fr)
	}
	_, raw, _ := f.Snapshot()
	if strings.Contains(string(raw), "A draft's narrative.") {
		t.Error("a frosted seed's description is in the document")
	}

	// A quiet refresh reads no description again; an edit to the plan does.
	src.mu.Lock()
	gets := src.getCalls
	src.mu.Unlock()
	if err := f.refresh(ctx); err != nil {
		t.Fatal(err)
	}
	src.mu.Lock()
	again := src.getCalls - gets
	src.mu.Unlock()
	if again != 0 {
		t.Errorf("a quiet refresh read the seeds again (%d batch reads)", again)
	}
	src.mutate("gd-seed2", func(b *beadsapi.BeadDetail) {
		b.Description = "Shorter now."
		b.UpdatedAt = b.UpdatedAt.Add(time.Minute)
	})
	if err := f.refresh(ctx); err != nil {
		t.Fatal(err)
	}
	if sd := findSeed(decodeSnap(t, f), "gd-seed2"); sd == nil || sd.Summary != "Shorter now." {
		t.Errorf("after an edit gd-seed2 = %+v, want the new summary", sd)
	}

	// Approved: no longer a seed, so its summary is let go.
	src.mutate("gd-seed2", func(b *beadsapi.BeadDetail) { b.Fields["plan_state"] = "building" })
	if err := f.refresh(ctx); err != nil {
		t.Fatal(err)
	}
	if len(f.seedNarrs) != 0 {
		t.Errorf("seedNarrs = %v after approve, want empty", f.seedNarrs)
	}
}

// An asked draft (a frosted plan with an open decision naming it) reads as a
// ready one does; more than maxSeedSteps steps are cut.
func TestFarmSeedStepsAskedAndCapped(t *testing.T) {
	src := seedWithAsk()
	src.mutate("gd-seed2", func(b *beadsapi.BeadDetail) { b.Fields["plan_state"] = "draft"; b.Description = "Why." })
	for i := range maxSeedSteps + 5 {
		src.add(fb(fmt.Sprintf("gd-many%02d", i), "task", "open", "gd-seed2", 80))
	}
	f := newTestFarm(src)
	if err := f.refresh(context.Background()); err != nil {
		t.Fatal(err)
	}
	sd := findSeed(decodeSnap(t, f), "gd-seed2")
	if sd == nil || sd.State != SeedAsked || sd.Summary != "Why." || len(sd.Steps) != maxSeedSteps {
		t.Fatalf("gd-seed2 = %+v, want asked with its summary and %d steps", sd, maxSeedSteps)
	}
	if first, last := sd.Steps[0], sd.Steps[maxSeedSteps-1]; first.ID != "gd-many00" || last.N != maxSeedSteps || last.ID != fmt.Sprintf("gd-many%02d", maxSeedSteps-1) {
		t.Errorf("steps run %+v .. %+v, want the oldest %d by created", first, last, maxSeedSteps)
	}
}

func TestFarmSeedSummaryUnreadIsRetried(t *testing.T) {
	src := seedWithAsk()
	src.mutate("gd-seed2", func(b *beadsapi.BeadDetail) { b.Description = "Why." })
	f := newTestFarm(failAsks{src}) // the one batch holding gd-seed2ask fails; the description batch does not
	if err := f.refresh(context.Background()); err != nil {
		t.Fatal(err)
	}
	if sd := findSeed(decodeSnap(t, f), "gd-seed2"); sd == nil || sd.Summary != "Why." {
		t.Fatalf("gd-seed2 = %+v, want its summary", sd)
	}
	f = newTestFarm(failSummaries{src})
	if err := f.refresh(context.Background()); err != nil {
		t.Fatal(err)
	}
	if sd := findSeed(decodeSnap(t, f), "gd-seed2"); sd == nil || sd.Summary != "" || sd.Decision == nil {
		t.Fatalf("gd-seed2 = %+v, want its decision and no summary", sd)
	}
	if len(f.seedNarrs) != 0 {
		t.Errorf("a failed read was cached: %v", f.seedNarrs)
	}
}

// failSummaries is a farm source whose batch read of a seed plan fails.
type failSummaries struct{ *fakeFarmSrc }

func (f failSummaries) BatchGetBeads(ctx context.Context, ids []string) (map[string]*beadsapi.BeadDetail, error) {
	if slices.Contains(ids, "gd-seed2") {
		return nil, errors.New("daemon down")
	}
	return f.fakeFarmSrc.BatchGetBeads(ctx, ids)
}
