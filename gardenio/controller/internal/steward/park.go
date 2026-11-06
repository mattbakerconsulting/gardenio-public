package steward

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"strings"

	"gardenio/controller/internal/beadsapi"
	"gardenio/controller/internal/lands/purse"
	"gardenio/controller/internal/lands/watcher"
)

// Parking (spec §4.2 item 5, step 9.7). At the wall the steward cannot run:
// the Lands spawn wall refuses its fires like any other gardener's
// (gardenerPreFireCheck). So its empty-purse line comes from the purse
// watcher, which calls Hook.PurseEmpty once per wall and records the park on
// the land's account bead (watcher.FieldStewardPark) until the purse has
// credit again. The steward sleeps until a credit grant wakes it
// (WakeCreditGrant), and its prompt then lists the steps whose gardeners were
// parked, so what resumes is its choice. Nothing resumes on its own.

// ParkText is the steward's empty-purse line in #general.
const ParkText = "The purse is empty; work is parked; ask the operator for credits in #support."

// Author is the steward's voice on a post the controller makes for it. Its
// runs post as agent:<land>-steward-<suffix>; a post for the steward as a
// whole carries no run suffix.
func Author(land string) string { return "agent:" + land + "-" + Role }

// HookDaemon is what the steward hook reads and writes. *beadsapi.Client
// satisfies it.
type HookDaemon interface {
	purse.Source
	CreateBead(ctx context.Context, req beadsapi.CreateBeadRequest) (string, error)
}

// Hook is the purse watcher's steward hook (watcher.Steward).
type Hook struct {
	Daemon HookDaemon
}

var _ watcher.Steward = Hook{}

// PurseEmpty says the steward's empty-purse line in the land's #general. A
// land with no steward, or no #general to say it in, has nothing said. The
// post is keyed on the land and its granted credits, so a retry after a lost
// write does not double it, and the next wall (after a refill) is said again.
func (h Hook) PurseEmpty(ctx context.Context, land string, p purse.Purse) error {
	st, err := FindSteward(ctx, h.Daemon, land)
	if err != nil || st == nil {
		return err
	}
	proj, _, err := purse.Project(ctx, h.Daemon, land)
	if err != nil {
		return err
	}
	general := strings.TrimSpace(proj.Fields["land_default_channel"])
	if general == "" {
		return nil
	}
	fields, _ := json.Marshal(map[string]string{ // a map[string]string always marshals
		"channel_id": general,
		"kind":       "text",
		"author":     Author(land),
		"text":       ParkText,
	})
	if _, err := h.Daemon.CreateBead(ctx, beadsapi.CreateBeadRequest{
		Title:          ParkText,
		Type:           "message",
		Kind:           "data",
		Status:         "closed",
		Labels:         []string{"project:" + land},
		CreatedBy:      Author(land),
		Fields:         fields,
		IdempotencyKey: fmt.Sprintf("steward-park:%s:%s", land, formatCredits(p.GrantedCredits)),
	}); err != nil {
		return fmt.Errorf("posting the steward's empty-purse line in %s's #general: %w", land, err)
	}
	return nil
}

// PurseRefilled does nothing: the steward wakes on the credit_grant bead
// through its own trigger, and the watcher unparks it once the purse has
// credit.
func (Hook) PurseRefilled(context.Context, string, purse.Purse) error { return nil }

// ParkedStep is an open step of the land whose gardener the purse watcher
// parked and that nothing has picked up since.
type ParkedStep struct {
	ID    string
	Title string
	// Agent is the parked gardener.
	Agent string
}

const (
	// parkedAgentsScan caps the land's gardener agent beads read for parked
	// steps, newest first.
	parkedAgentsScan = 500
	// openStepsScan caps the land's open steps read to match them against.
	openStepsScan = 1000
	// parkedStepsShown caps the parked steps in the prompt.
	parkedStepsShown = 20
)

// running are the agent states of a gardener still at work (or starting).
var running = map[string]bool{
	"spawning": true, "assigning": true, "idle": true, "working": true, "yielding": true,
	"rate_limited": true, "model_rate_limited": true, "resuming": true,
}

// parkedSteps are the land's open steps whose newest gardener was parked at
// the wall (park_reason=purse_empty) and is not running again: a step that
// was resumed has a newer gardener, or its parked one thawed.
func parkedSteps(ctx context.Context, src purse.Source, land string) ([]ParkedStep, error) {
	agents, err := src.ListBeadsFiltered(ctx, beadsapi.ListBeadsQuery{
		Types: []string{"agent"}, Labels: []string{"project:" + land, "role:" + GardenerRole},
		Sort: "-created_at", Limit: parkedAgentsScan,
	})
	if err != nil {
		return nil, fmt.Errorf("reading %s's gardeners: %w", land, err)
	}
	newest := map[string]*beadsapi.BeadDetail{}
	for _, b := range agents.Beads {
		if b == nil || b.Type != "agent" {
			continue
		}
		task := strings.TrimSpace(b.Fields["task_id"])
		if cur := newest[task]; task != "" && (cur == nil || b.CreatedAt.After(cur.CreatedAt)) {
			newest[task] = b
		}
	}
	parked := map[string]*beadsapi.BeadDetail{}
	for task, b := range newest {
		if b.Fields[watcher.FieldParkReason] == watcher.ParkReason && (b.Status == "closed" || !running[b.Fields["agent_state"]]) {
			parked[task] = b
		}
	}
	if len(parked) == 0 {
		return nil, nil
	}
	steps, err := src.ListBeadsFiltered(ctx, beadsapi.ListBeadsQuery{
		Types: []string{"task"}, Labels: []string{"project:" + land}, Statuses: []string{"open", "in_progress", "blocked"},
		Limit: openStepsScan,
	})
	if err != nil {
		return nil, fmt.Errorf("reading %s's parked steps: %w", land, err)
	}
	var out []ParkedStep
	for _, s := range steps.Beads {
		if s == nil || s.Status == "closed" {
			continue
		}
		if a := parked[s.ID]; a != nil {
			out = append(out, ParkedStep{ID: s.ID, Title: s.Title, Agent: firstNonEmpty(a.Fields["agent"], a.ID)})
		}
	}
	slices.SortFunc(out, func(a, b ParkedStep) int { return strings.Compare(a.ID, b.ID) })
	if len(out) > parkedStepsShown {
		out = out[:parkedStepsShown]
	}
	return out, nil
}
