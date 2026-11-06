package steward

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"gardenio/controller/internal/beadsapi"
	"gardenio/controller/internal/lands/purse"
	"gardenio/controller/internal/lands/wall"
)

func okPurse(remaining, margin float64) purse.Purse {
	return purse.Purse{State: purse.StateOK, RemainingCredits: &remaining, MarginCredits: margin, GrantedCredits: 100}
}

// Each refusal of a gardener step, in the order they are decided.
func TestDecideStepSpawn(t *testing.T) {
	est := Estimate{Credits: 6}
	wallPurse := okPurse(1, 2)
	wallPurse.State, wallPurse.Blocked = purse.StateWall, true
	for _, c := range []struct {
		name   string
		p      purse.Purse
		e      Estimate
		others int
		want   string
	}{
		{"fits", okPurse(10, 2), est, 1, ""},
		{"exactly fits", okPurse(8, 2), est, 1, ""},
		{"wall", wallPurse, est, 0, SpawnPurseWall},
		{"unknown", purse.Purse{State: purse.StateUnknown, Blocked: true, Reason: "spend not synced"}, est, 0, SpawnPurseUnknown},
		{"unknown state with figures", func() purse.Purse { p := okPurse(50, 2); p.State = ""; return p }(), est, 0, SpawnPurseUnknown},
		{"cap before ask", okPurse(1, 2), est, 2, SpawnMaxLiveAgents},
		{"ask", okPurse(7.99, 2), est, 1, SpawnAskOwner},
	} {
		got := DecideStepSpawn(c.p, c.e, c.others, 3)
		if got.Code != c.want {
			t.Errorf("%s: code = %q (%s), want %q", c.name, got.Code, got.Detail, c.want)
		}
	}
	ask := DecideStepSpawn(okPurse(7.99, 2), est, 1, 3)
	if ask.AboveMarginCredits == nil || *ask.AboveMarginCredits != 5.99 || ask.Detail == "" {
		t.Errorf("ask = %+v, want 5.99 above the margin and a detail", ask)
	}
}

// fundedLand adds a rate, a grant of 100 credits and a synced account with
// spentUSD to the farm.
func fundedLand(d *fakeDaemon, spentUSD string, now time.Time) {
	d.beads = append(d.beads,
		&beadsapi.BeadDetail{ID: "tp-rate", Type: "credit_rate", Status: "closed", Fields: map[string]string{"version": "1", "llm_multiplier": "1.5"}},
		&beadsapi.BeadDetail{ID: "tp-grant", Type: "credit_grant", Status: "closed", Labels: []string{"project:" + land}, Fields: map[string]string{"land": land, "amount_credits": "100"}},
		&beadsapi.BeadDetail{ID: "tp-acct", Type: "account", Title: "lands-" + land, Status: "open", Fields: map[string]string{
			"total_cost_usd": spentUSD, purse.FieldSpendSyncedAt: now.Add(-time.Minute).Format(time.RFC3339)}},
	)
}

func TestCheckStepSpawn(t *testing.T) {
	ctx := context.Background()
	now := time.Now()

	// 100 granted, $60 spent at x1.5 = 90: 10 left, margin 5, 5 above it.
	d := newFarm()
	fundedLand(d, "60", now)
	// The default estimate is 5 and 5 remain above: it fits exactly.
	if got := CheckStepSpawn(ctx, d, land, now); got.Refused() || got.Estimate.Credits != DefaultStepEstimateCredits {
		t.Fatalf("default estimate, 5 above the margin = %+v, want spawned", got)
	}
	// One more dollar spent: 1.5 credits less, and the step no longer fits.
	d.beads[len(d.beads)-1].Fields["total_cost_usd"] = "61"
	if got := CheckStepSpawn(ctx, d, land, now); got.Code != SpawnAskOwner {
		t.Fatalf("estimate past the margin = %+v, want %s", got, SpawnAskOwner)
	}
	// The cap: max_live_agents 2, one gardener live.
	d.beads[len(d.beads)-1].Fields["total_cost_usd"] = "10"
	d.beads[0].Fields["max_live_agents"] = "2"
	d.beads = append(d.beads, &beadsapi.BeadDetail{ID: "tp-ag1", Type: "agent", Status: "open", Labels: []string{"project:" + land},
		Fields: map[string]string{"project": land, "role": GardenerRole, "agent_state": "working"}})
	if got := CheckStepSpawn(ctx, d, land, now); got.Code != wall.CodeMaxLiveAgents {
		t.Fatalf("full land = %+v, want %s", got, wall.CodeMaxLiveAgents)
	}
	// Unsynced spend: unknown.
	d.beads[0].Fields["max_live_agents"] = "3"
	d.beads[len(d.beads)-2].Fields[purse.FieldSpendSyncedAt] = now.Add(-time.Hour).Format(time.RFC3339)
	if got := CheckStepSpawn(ctx, d, land, now); got.Code != SpawnPurseUnknown {
		t.Fatalf("stale spend = %+v, want %s", got, SpawnPurseUnknown)
	}
	// An operator land is never refused here.
	if got := CheckStepSpawn(ctx, d, "gardenio", now); got.Refused() {
		t.Errorf("operator land = %+v", got)
	}
}

// errSource fails every list, as a daemon that cannot be read.
type errSource struct{}

func (errSource) ListBeadsFiltered(context.Context, beadsapi.ListBeadsQuery) (*beadsapi.ListBeadsResult, error) {
	return nil, errors.New("daemon down")
}

// An owner's yes lifts ask_owner only when the owner gave it, for this step.
func TestCheckOwnerApproval(t *testing.T) {
	ctx := context.Background()
	const steward, step = "tp-steward-run", "tp-step-2"
	d := newFarm()
	d.beads = append(d.beads,
		&beadsapi.BeadDetail{ID: "tp-a1", Type: "agent", Title: steward, Status: "open", Labels: []string{"project:" + land}, Fields: map[string]string{"agent": steward}},
		&beadsapi.BeadDetail{ID: "tp-a2", Type: "agent", Title: "tp-gardener-1", Status: "closed", Labels: []string{"project:" + land, "role:" + GardenerRole}, Fields: map[string]string{"agent": "tp-gardener-1"}},
	)
	opts := `[{"id":"spawn","short":"Spawn the step anyway"},{"id":"wait","short":"Wait for credits"}]`
	answered := func(edit func(*beadsapi.BeadDetail)) *beadsapi.BeadDetail {
		b := &beadsapi.BeadDetail{ID: "tp-d1", Type: "decision", Status: "closed", CreatedBy: steward, Labels: []string{"project:" + land},
			Fields: map[string]string{"options": opts, "requesting_task": step, "requested_by": steward,
				"chosen": AskSpawn, "responded_by": "oidc:ada@example.com", "responded_at": "2026-10-23T14:00:00Z"}}
		if edit != nil {
			edit(b)
		}
		return b
	}
	if err := CheckOwnerApproval(ctx, d, answered(nil), land, step, steward); err != nil {
		t.Fatalf("the owner's spawn answer for the step: %v", err)
	}
	if err := CheckOwnerApproval(ctx, d, answered(func(b *beadsapi.BeadDetail) { b.Fields["chosen"] = "1" }), land, step, steward); err != nil {
		t.Fatalf("spawn chosen by its place in the options: %v", err)
	}
	for _, c := range []struct {
		name   string
		dec    *beadsapi.BeadDetail
		step   string
		src    purse.Source
		refuse string
	}{
		{"unanswered", answered(func(b *beadsapi.BeadDetail) {
			b.Status = "open"
			delete(b.Fields, "chosen")
			delete(b.Fields, "responded_by")
			delete(b.Fields, "responded_at")
		}), step, d, "not answered yet"},
		{"answered no", answered(func(b *beadsapi.BeadDetail) { b.Fields["chosen"] = AskWait }), step, d, `answered "wait"`},
		{"answered no by place", answered(func(b *beadsapi.BeadDetail) { b.Fields["chosen"] = "2" }), step, d, `answered "wait"`},
		{"dismissed", answered(func(b *beadsapi.BeadDetail) { b.Fields["chosen"] = "dismissed" }), step, d, `answered "dismissed"`},
		{"words, no option", answered(func(b *beadsapi.BeadDetail) {
			delete(b.Fields, "chosen")
			b.Fields["response_text"] = "sure, go"
		}), step, d, "not answered yet"},
		{"answered by the caller", answered(func(b *beadsapi.BeadDetail) { b.Fields["responded_by"] = steward }), step, d, "who asked it"},
		{"answered by the filer", answered(func(b *beadsapi.BeadDetail) {
			b.CreatedBy, b.Fields["requested_by"], b.Fields["responded_by"] = "tp-other", "tp-other", "tp-other"
		}), step, d, "who asked it"},
		{"answered by another agent of the land", answered(func(b *beadsapi.BeadDetail) { b.Fields["responded_by"] = "tp-gardener-1" }), step, d, "an agent of the land"},
		{"no one answered", answered(func(b *beadsapi.BeadDetail) { delete(b.Fields, "responded_by") }), step, d, "names no one"},
		{"agents unreadable", answered(nil), step, errSource{}, "daemon down"},
		{"a different step", answered(nil), "tp-step-3", d, `filed for step "tp-step-2"`},
		{"no step", answered(nil), "", d, "pass the step with --task"},
		{"filed for no step", answered(func(b *beadsapi.BeadDetail) { delete(b.Fields, "requesting_task") }), step, d, `filed for step ""`},
		{"another land", answered(func(b *beadsapi.BeadDetail) { b.Labels = []string{"project:gardenio"} }), step, d, "not a decision of land"},
		{"not a decision", answered(func(b *beadsapi.BeadDetail) { b.Type = "task" }), step, d, "not a decision bead"},
		{"no bead", nil, step, d, "not a decision bead"},
	} {
		err := CheckOwnerApproval(ctx, c.src, c.dec, land, c.step, steward)
		if err == nil || !strings.Contains(err.Error(), c.refuse) {
			t.Errorf("%s: err = %v, want a refusal containing %q", c.name, err, c.refuse)
		}
	}
}
