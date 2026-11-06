package steward

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"slices"
	"strconv"
	"strings"
	"time"

	"gardenio/controller/internal/beadsapi"
	"gardenio/controller/internal/lands/purse"
	"gardenio/controller/internal/lands/wall"
)

// Spawning within the purse (spec §4.2 item 3, step 9.5). Before a
// lands-gardener is created, gb spawn asks CheckStepSpawn whether the land
// can run one more step: the purse must not be at the wall or unknown, the
// land must have a free slot under max_live_agents, and the step estimate
// must fit in what remains above the margin. When it does not fit the
// steward asks the owner instead of spawning. Nothing is created, so a
// refusal here leaves no bead waiting at the wall.
//
// This is the steward's check, made before the create. The controller's
// spawn wall (internal/lands/wall) still decides every spawn, so a reading
// that was wrong, or a create that went round gb spawn, is refused there
// for the wall, the unknown purse and the cap. The estimate is not a wall
// code: only the steward weighs it, and the owner may answer yes.

// Spawn refusal codes. The first three match the wall's codes.
const (
	SpawnPurseWall     = wall.CodePurseWall
	SpawnPurseUnknown  = wall.CodePurseUnknown
	SpawnMaxLiveAgents = wall.CodeMaxLiveAgents
	// SpawnAskOwner: the step estimate is more than remains above the
	// margin. The steward asks the owner; an owner's yes is passed back with
	// gb spawn --owner-approved <decision>.
	SpawnAskOwner = "ask_owner"
)

// StepSpawn is the verdict on one more gardener step. The zero Code spawns.
type StepSpawn struct {
	Code   string `json:"reason,omitempty"`
	Detail string `json:"detail,omitempty"`
	// What it was decided on.
	Estimate Estimate `json:"estimate"`
	// AboveMarginCredits is remaining - margin, nil when the purse is unknown.
	AboveMarginCredits *float64 `json:"above_margin_credits"`
	LiveGardeners      int      `json:"live_gardeners"`
	MaxLiveAgents      int      `json:"max_live_agents"`
}

// Refused reports whether the step is not spawned.
func (s StepSpawn) Refused() bool { return s.Code != "" }

// DecideStepSpawn is the pure half of CheckStepSpawn: p is the land's purse,
// e the step estimate, others the land's live agents other than the steward
// and limit its max_live_agents. The wall and an unknown purse come first,
// then the cap, then the estimate, so the owner is asked for credits only
// when credits are what is missing.
func DecideStepSpawn(p purse.Purse, e Estimate, others, limit int) StepSpawn {
	s := StepSpawn{Estimate: e, LiveGardeners: others, MaxLiveAgents: limit}
	if p.RemainingCredits != nil {
		above := math.Round((*p.RemainingCredits-p.MarginCredits)*100) / 100
		s.AboveMarginCredits = &above
	}
	switch {
	case p.State == purse.StateWall:
		s.Code = SpawnPurseWall
		s.Detail = "the purse is at the wall: nothing new starts until the operator grants credits"
	case p.Blocked || p.State != purse.StateOK || s.AboveMarginCredits == nil:
		s.Code = SpawnPurseUnknown
		s.Detail = "the purse cannot be read, so nothing new starts"
		if p.Reason != "" {
			s.Detail += " (" + p.Reason + ")"
		}
	case wall.LiveCapFull(others, limit):
		s.Code = SpawnMaxLiveAgents
		s.Detail = fmt.Sprintf("the land runs at most %d agents at once, you included, and %d gardener(s) are live: wait for one to finish", limit, others)
	case e.Credits > *s.AboveMarginCredits:
		s.Code = SpawnAskOwner
		s.Detail = fmt.Sprintf("the next step is estimated at %s credits and %s remain above the margin: ask the owner before spawning",
			formatCredits(e.Credits), formatCredits(*s.AboveMarginCredits))
	}
	return s
}

// CheckStepSpawn decides one more lands-gardener step on land now. A project
// that is not a tenant land has no purse and is never refused here. A land
// whose state cannot be read is refused as unknown: fail closed is the rule
// for a tenant's money.
func CheckStepSpawn(ctx context.Context, src purse.Source, land string, now time.Time) StepSpawn {
	proj, tenant, err := purse.Project(ctx, src, land)
	switch {
	case errors.Is(err, purse.ErrNotLand):
		return StepSpawn{}
	case err != nil:
		return StepSpawn{Code: SpawnPurseUnknown, Detail: fmt.Sprintf("land %s could not be read: %v", land, err), Estimate: DefaultEstimate()}
	case !tenant:
		return StepSpawn{}
	}
	p, err := purse.Read(ctx, src, land, now)
	if err != nil {
		p = purse.Unreadable(land, err, now)
	}
	e, _ := StepEstimate(ctx, src, land, p.LLMMultiplier)
	limit := wall.MaxLiveAgents(proj.Fields)
	others, _, err := wall.Live(ctx, src, land, "")
	if err != nil {
		return StepSpawn{Code: SpawnMaxLiveAgents, Detail: fmt.Sprintf("the land's live agents could not be counted: %v", err), Estimate: e, MaxLiveAgents: limit}
	}
	return DecideStepSpawn(p.CreditsOnly(), e, others, limit)
}

// The ask_owner decision's options. The steward files the owner a decision
// bound to the step (--requesting-task <step>) with these two; an answer of
// AskSpawn is what gb spawn --owner-approved takes.
const (
	AskSpawn = "spawn"
	AskWait  = "wait"
)

// CheckOwnerApproval decides whether dec, passed as gb spawn --owner-approved,
// lifts ask_owner for step: a decision of land, filed for step, answered
// AskSpawn by a person. caller is the identity running gb spawn. An answer by
// the caller, by whoever filed the decision, or by any agent of the land is
// not the owner's, so the steward cannot approve its own ask. The land's
// agents are read through src; when they cannot be, the approval is refused.
func CheckOwnerApproval(ctx context.Context, src purse.Source, dec *beadsapi.BeadDetail, land, step, caller string) error {
	if dec == nil || dec.Type != "decision" {
		return errors.New("not a decision bead")
	}
	if !slices.Contains(dec.Labels, "project:"+land) && dec.Fields["project"] != land {
		return fmt.Errorf("%s is not a decision of land %s", dec.ID, land)
	}
	if step == "" {
		return fmt.Errorf("%s approves one step: pass the step with --task", dec.ID)
	}
	if got := strings.TrimSpace(dec.Fields["requesting_task"]); got != step {
		return fmt.Errorf("%s was filed for step %q, not %s", dec.ID, got, step)
	}
	switch chosen := askChosen(dec.Fields); chosen {
	case AskSpawn:
	case "":
		return fmt.Errorf("%s is not answered yet", dec.ID)
	default:
		return fmt.Errorf("%s was answered %q, not %q", dec.ID, chosen, AskSpawn)
	}
	by := strings.TrimSpace(dec.Fields["responded_by"])
	if by == "" {
		return fmt.Errorf("%s names no one who answered it", dec.ID)
	}
	for _, self := range []string{caller, dec.CreatedBy, dec.Fields["requested_by"]} {
		if self = strings.TrimSpace(self); self != "" && by == self {
			return fmt.Errorf("%s was answered by %s, who asked it, not by the owner", dec.ID, by)
		}
	}
	res, err := src.ListBeadsFiltered(ctx, beadsapi.ListBeadsQuery{
		Types: []string{"agent"}, Labels: []string{"project:" + land}, Limit: 1000,
	})
	if err != nil {
		return fmt.Errorf("reading %s's agents to check who answered %s: %w", land, dec.ID, err)
	}
	for _, a := range res.Beads {
		if by == a.Title || by == a.Fields["agent"] || by == a.ID {
			return fmt.Errorf("%s was answered by %s, an agent of the land, not by the owner", dec.ID, by)
		}
	}
	return nil
}

// askChosen is the option an answered ask_owner decision chose, by its id or
// by its 1-based place in the decision's own options; "" when unanswered,
// "dismissed" when dismissed or cancelled.
func askChosen(f map[string]string) string {
	chosen := strings.TrimSpace(f["chosen"])
	if chosen == "dismissed" || f["canceled"] == "true" {
		return "dismissed"
	}
	if n, err := strconv.Atoi(chosen); err == nil {
		var opts []struct {
			ID string `json:"id"`
		}
		if json.Unmarshal([]byte(f["options"]), &opts) == nil && n >= 1 && n <= len(opts) {
			return strings.ToLower(opts[n-1].ID)
		}
	}
	return strings.ToLower(chosen)
}
