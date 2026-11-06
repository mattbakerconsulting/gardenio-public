// Package releasegardener binds an open release's own gardener (plan
// gd-lgN4U7X5Tb, step gd-BS9fpWa1bx): a type=trigger that spawns the
// release-captain role, so release-cut.sh and release.sh admit its runs
// (gd-CVotEQ3vk9). Its runs cut the release in a window, mark it built, watch
// its tag's runs and its deployment, and retire it once it is live or
// superseded. It is plangardener's twin for a release; the rules it is written
// to are gardenio/docs/gardener-authoring-guide.md's, and its playbook is an
// advice bead (PlaybookLabel), not a git file (rule 5).
//
// A release has at most one gardener: the trigger is pinned by the
// gardener-release:<release> label, Ensure adopts one already pinned, and the
// release names it in its gardener_id field. (Not "gardener": on a bead that
// field is a gardener's loop blob, which the daemon's enable gate and runaway
// validation read.)
package releasegardener

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"gardenio/controller/internal/beadsapi"
	"gardenio/controller/internal/bridge"
	"gardenio/controller/internal/scheduler"
	"gardenio/controller/internal/scheduler/conditions"
	"gardenio/controller/internal/scheduler/progress"
)

const (
	// SpawnRole is the role every run spawns in: the release captain's, which
	// the release writer check and the cut scripts' role guard admit.
	SpawnRole = "release-captain"
	// MarkerLabel tags the trigger as a release's gardener.
	MarkerLabel = "release-gardener"
	// PlaybookLabel finds the playbook advice bead the prompt points at.
	PlaybookLabel = "release-gardener-playbook"
	// ReleaseField is the release bead's field naming its gardener.
	ReleaseField = "gardener_id"

	// CutWindowCron fires in the two daily cut windows, ~04:00Z and ~16:00Z
	// (root CLAUDE.md "Who cuts"). One cut per window is the playbook's rule.
	CutWindowCron = "0 4,16 * * *"
	cutWindowTZ   = "UTC"

	// DefaultMaxRuns bounds one release's gardener: two windows a day, the
	// release's own changes (throttled) and the run that retires it.
	DefaultMaxRuns = 16
	// DefaultLoopWallClock is the backstop: a release not live within a week
	// stops its gardener rather than looping on.
	DefaultLoopWallClock = "168h"
)

// ReleaseLabel pins a gardener trigger to its release.
func ReleaseLabel(releaseID string) string { return "gardener-release:" + releaseID }

// RunMarker is the part of every run name of releaseID's gardener that no
// other agent's name carries: the engine names a run <project>-<slug of the
// title>-<suffix>, and the title is "Release gardener for <release>".
func RunMarker(releaseID string) string {
	return "-release-gardener-for-" + strings.ToLower(releaseID) + "-"
}

// Client is the daemon surface Ensure needs; *beadsapi.Client satisfies it.
type Client interface {
	ListBeadsFiltered(ctx context.Context, q beadsapi.ListBeadsQuery) (*beadsapi.ListBeadsResult, error)
	CreateBead(ctx context.Context, req beadsapi.CreateBeadRequest) (string, error)
	UpdateBeadFieldsTyped(ctx context.Context, beadID string, fields map[string]any) error
}

var _ Client = (*beadsapi.Client)(nil)

// Spec parameterizes Ensure. Zero values take the defaults.
type Spec struct {
	// CreatedBy is the actor recorded on the trigger.
	CreatedBy string
	// Paused births the gardener disabled; an admin's enable then seals and
	// starts it.
	Paused bool
	// MaxRuns overrides DefaultMaxRuns (<=0 takes the default).
	MaxRuns int
}

// Result reports what Ensure did.
type Result struct {
	// ID is the gardener trigger, created or adopted.
	ID string
	// Adopted is true when a gardener was already pinned to the release.
	Adopted bool
	// Named is true when this call wrote the release's gardener_id.
	Named bool
}

// Find is the gardener pinned to releaseID, enabled or paused, nil when none.
func Find(ctx context.Context, c Client, releaseID string) (*beadsapi.BeadDetail, error) {
	res, err := c.ListBeadsFiltered(ctx, beadsapi.ListBeadsQuery{
		Types:    []string{"trigger"},
		Statuses: []string{"open", "in_progress", "blocked"},
		Labels:   []string{ReleaseLabel(releaseID)},
		Limit:    1,
	})
	if err != nil {
		return nil, fmt.Errorf("finding release %s's gardener: %w", releaseID, err)
	}
	if res == nil || len(res.Beads) == 0 {
		return nil, nil
	}
	return res.Beads[0], nil
}

// Ensure gives rel its gardener unless it has one, and makes rel name it. A
// release already live or superseded gets none: that is when a gardener
// retires (as it does on a skipped release, whose status stays cut or built).
func Ensure(ctx context.Context, c Client, rel *beadsapi.Release, spec Spec) (*Result, error) {
	if rel == nil || strings.TrimSpace(rel.ID) == "" {
		return nil, fmt.Errorf("a release is required")
	}
	switch rel.Status {
	case "live", "superseded":
		return nil, fmt.Errorf("release %s is %s; its gardener has retired", rel.ID, rel.Status)
	}
	if strings.TrimSpace(rel.Project) == "" {
		return nil, fmt.Errorf("release %s names no project", rel.ID)
	}
	res := &Result{}
	existing, err := Find(ctx, c, rel.ID)
	if err != nil {
		return nil, err
	}
	if existing != nil {
		res.ID, res.Adopted = existing.ID, true
	} else {
		req, err := buildTrigger(ctx, c, rel, spec)
		if err != nil {
			return nil, err
		}
		if res.ID, err = c.CreateBead(ctx, req); err != nil {
			return nil, fmt.Errorf("creating release %s's gardener: %w", rel.ID, err)
		}
	}
	if rel.GardenerID != res.ID {
		if err := c.UpdateBeadFieldsTyped(ctx, rel.ID, map[string]any{ReleaseField: res.ID}); err != nil {
			return res, fmt.Errorf("naming gardener %s on release %s: %w", res.ID, rel.ID, err)
		}
		res.Named = true
	}
	return res, nil
}

// playbookID is the playbook advice bead's id.
func playbookID(ctx context.Context, c Client) (string, error) {
	res, err := c.ListBeadsFiltered(ctx, beadsapi.ListBeadsQuery{
		Types:    []string{"advice"},
		Statuses: []string{"open"},
		Labels:   []string{PlaybookLabel},
		Limit:    1,
	})
	if err != nil {
		return "", fmt.Errorf("finding the release gardener playbook: %w", err)
	}
	if res == nil || len(res.Beads) == 0 {
		return "", fmt.Errorf("no open advice bead carries label %s: the release gardener's playbook is missing", PlaybookLabel)
	}
	return res.Beads[0].ID, nil
}

// buildTrigger is the create request for rel's gardener.
func buildTrigger(ctx context.Context, c Client, rel *beadsapi.Release, spec Spec) (beadsapi.CreateBeadRequest, error) {
	advice, err := playbookID(ctx, c)
	if err != nil {
		return beadsapi.CreateBeadRequest{}, err
	}
	maxRuns := spec.MaxRuns
	if maxRuns <= 0 {
		maxRuns = DefaultMaxRuns
	}
	blob, err := loopBlob(rel, maxRuns)
	if err != nil {
		return beadsapi.CreateBeadRequest{}, err
	}
	if v := bridge.ValidateGardenerBlob(blob); v != nil {
		return beadsapi.CreateBeadRequest{}, fmt.Errorf("%s", v.Error())
	}
	triggers, err := json.Marshal(Triggers(rel.ID))
	if err != nil {
		return beadsapi.CreateBeadRequest{}, fmt.Errorf("marshalling release gardener triggers: %w", err)
	}
	fields, err := json.Marshal(map[string]any{
		"enabled":          !spec.Paused,
		"action":           "spawn_agent",
		"role":             SpawnRole,
		"project":          rel.Project,
		"prompt":           Prompt(rel.ID, rel.Project, advice),
		"timezone":         cutWindowTZ,
		"triggers":         string(triggers),
		"gardener":         blob,
		"cooldown_minutes": 5,
		"max_concurrent":   1,
	})
	if err != nil {
		return beadsapi.CreateBeadRequest{}, fmt.Errorf("marshalling release gardener fields: %w", err)
	}
	labels := []string{"trigger:multi", "role:gardener", MarkerLabel, ReleaseLabel(rel.ID), "project:" + rel.Project}
	if v := bridge.ValidateGardenerStandingBlob(blob, labels); v != nil {
		return beadsapi.CreateBeadRequest{}, fmt.Errorf("%s", v.Error())
	}
	return beadsapi.CreateBeadRequest{
		Title:      "Release gardener for " + rel.ID,
		ShortTitle: "tend " + rel.Project + "'s next release",
		Type:       "trigger",
		Kind:       "data",
		Labels:     labels,
		Fields:     fields,
		CreatedBy:  spec.CreatedBy,
	}, nil
}

// Triggers are a release gardener's wakes: the release bead changing (a PR
// boarding, the cut, built, a deployment row), not by its own runs, and the
// two cut windows. The tag's workflow conclusions wake the run that cut it
// through gb wait ci, which resumes that run rather than firing a new one.
func Triggers(releaseID string) []scheduler.TriggerEntry {
	return []scheduler.TriggerEntry{
		{
			Kind:   scheduler.TriggerKindBeadEvent,
			Label:  "release-activity",
			Events: []string{"updated", "closed"},
			Match: &conditions.Expression{All: []conditions.Expression{
				{Not: &conditions.Expression{Leaf: conditions.Leaf{Field: "actor", Op: "contains", Value: RunMarker(releaseID)}}},
				{Leaf: conditions.Leaf{Field: "id", Op: "==", Value: releaseID}},
			}},
			// Boarding writes the release on every merge; a window's cut
			// is what they wait for, so they wake a run at most hourly.
			Debounce: "15m",
			Throttle: "1h",
		},
		{Kind: scheduler.TriggerKindCron, Label: "cut-window", Cron: CutWindowCron, Timezone: cutWindowTZ},
	}
}

// loopBlob is the gardener blob: the run budget and a week's wall clock as the
// backstop, and the run's own goal_satisfied (the release is live, superseded
// or skipped) as the end. no_progress_for is left out on purpose: a window
// with nothing to cut is a normal quiet run, not a wedge.
func loopBlob(rel *beadsapi.Release, maxRuns int) (string, error) {
	stop := scheduler.StopSpec{
		LoopEnd: scheduler.StopSlot{Any: []scheduler.StopEntry{
			{Kind: scheduler.StopKindGoalSatisfied},
			{Kind: scheduler.StopKindMaxRuns},
		}},
		HardStop: scheduler.StopSlot{Any: []scheduler.StopEntry{
			{Kind: scheduler.StopKindConsecutiveFailures, Value: "5"},
			{Kind: scheduler.StopKindWallClock, Duration: DefaultLoopWallClock},
		}},
		OnLoopEnd: "close",
	}
	stopRaw, err := json.Marshal(stop)
	if err != nil {
		return "", err
	}
	prog := progress.Spec{
		Events: []progress.EventMatcher{
			{Kind: progress.EventKindWrapupField, Field: "progress_score", Op: ">=", Value: "1"},
		},
		Windows: []progress.Window{{Name: "this_run", Scope: progress.WindowScopeSinceRunStart}},
		Surface: progress.Surface{InPrime: true, InWrapup: true},
	}
	progRaw, err := json.Marshal(prog)
	if err != nil {
		return "", err
	}
	raw, err := json.Marshal(bridge.GardenerSpec{
		AssignBead:      rel.ID,
		TerminalGoal:    fmt.Sprintf("Release %s of %s is live, superseded or skipped.", rel.ID, rel.Project),
		RunBudget:       maxRuns,
		MaxRuns:         maxRuns,
		OneSpawnAtATime: true,
		Stop:            stopRaw,
		Progress:        progRaw,
	})
	return string(raw), err
}

// Prompt is a run's prompt: which release, and the playbook advice to read
// first. The steps live in the advice so they change without a re-bind.
func Prompt(releaseID, project, adviceID string) string {
	return fmt.Sprintf(`You are the gardener of %[2]s's release %[1]s, in role release-captain. You
cut it in a cut window, mark it built, watch its tag's runs and its
deployment, and retire when it is live, superseded or skipped (never
deployed while a newer release went out: kd release show %[1]s --json
names it in skipped_by).

First run: gb advice show %[3]s
That advice is your playbook. Follow it for release %[1]s (project %[2]s):
one pass per wake, then gb stop. When %[1]s is live, superseded or skipped, stop with
"goal_satisfied":"yes" in the wrap-up; that retires this gardener.`, releaseID, project, adviceID)
}
