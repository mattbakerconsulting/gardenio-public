package gardenermode

import (
	"context"
	"encoding/json"
	"fmt"
	"reflect"
	"slices"
	"strings"
	"time"

	"gardenio/controller/internal/beadsapi"
	"gardenio/controller/internal/scheduler"
	"gardenio/controller/internal/scheduler/conditions"
)

// Mode is a gardener trigger bead's firing mode.
type Mode string

const (
	// ModeTimed fires the gardener on a cron schedule (label trigger:schedule).
	ModeTimed Mode = "timed"
	// ModeEvent fires the gardener on subtree bead events, with a slow cron
	// watchdog fallback (label trigger:multi + triggers[]).
	ModeEvent Mode = "event"
)

const (
	// LabelSchedule claims a trigger bead for the schedule loader (timed mode).
	LabelSchedule = "trigger:schedule"
	// LabelMulti claims a trigger bead for the multi loader (event mode).
	LabelMulti = "trigger:multi"
)

const (
	epicPrefix = "gardener-epic:"
	defaultTZ  = "America/Los_Angeles"
	// planGardenerMarker is the label that tags a trigger bead as a plan-bound
	// gardener (mirrors plangardener.MarkerLabel — duplicated here to avoid an
	// import cycle, since plangardener imports gardenermode). An event-mode plan
	// gardener gets NO default fallback watchdog cron unless the operator asks
	// for one explicitly (operator ask ka-0000008237).
	planGardenerMarker = "plan-gardener"
)

// Writer is the subset of *beadsapi.Client that Set needs.
type Writer interface {
	UpdateBeadFieldsTyped(ctx context.Context, beadID string, fields map[string]any) error
	AddLabel(ctx context.Context, beadID, label string) error
	RemoveLabel(ctx context.Context, beadID, label string) error
	AddComment(ctx context.Context, beadID, author, text string) error
}

// Opts tunes a Set call.
type Opts struct {
	Operator     string // audit-comment author; required
	FallbackCron string // optional override; default StaggeredCron(epic, 6h). Ignored when NoFallbackCron.
	// NoFallbackCron authors an event-mode gardener with NO watchdog cron entry
	// — purely event-driven, no clock floor. This is the plan-gardener default
	// (operator ask: an event-triggered plan gardener should not carry a default
	// cron schedule; ka-0000008237). When true, FallbackCron is ignored and the
	// event-mode triggers[] carries only the bead_event source.
	NoFallbackCron bool
	Timezone       string // optional; default bead's timezone field, then "America/Los_Angeles"
}

// Result reports what Set did.
type Result struct {
	Changed bool // false = already in requested mode (no writes)
	Mode    Mode
	Cron    string // effective cadence: cron (timed) or fallback cron (event)
	// Refreshed marks a write to a gardener already in event mode: its
	// triggers[] predated the current EventTriggers shape and Set brought it up
	// to date (see RefreshEventTriggers). Changed is true too.
	Refreshed bool
}

// Detect returns the bead's current mode, or an error for ambiguous label
// state (both/neither of trigger:schedule, trigger:multi).
func Detect(b *beadsapi.BeadDetail) (Mode, error) {
	hasSched := slices.Contains(b.Labels, LabelSchedule)
	hasMulti := slices.Contains(b.Labels, LabelMulti)
	switch {
	case hasSched && hasMulti:
		return "", fmt.Errorf("ambiguous gardener mode: bead %s carries both %s and %s", b.ID, LabelSchedule, LabelMulti)
	case hasSched:
		return ModeTimed, nil
	case hasMulti:
		return ModeEvent, nil
	default:
		return "", fmt.Errorf("ambiguous gardener mode: bead %s carries neither %s nor %s", b.ID, LabelSchedule, LabelMulti)
	}
}

// EpicOf extracts the epic id from the gardener-epic:<id> label ("" if none).
func EpicOf(labels []string) string {
	for _, l := range labels {
		if rest, ok := strings.CutPrefix(l, epicPrefix); ok {
			return rest
		}
	}
	return ""
}

// Set flips a gardener trigger bead between timed (cron) and event
// (subtree bead events + slow fallback cron) firing.
//
// Write-ordering contract (this is the double-fire guard):
//
//   - → event: (1) UpdateBeadFieldsTyped{timed_cron: <old cron>, cron: "",
//     triggers: <json>} — cron cleared first so the schedule loader goes
//     dormant before the label swap; (2) AddLabel("trigger:multi");
//     (3) RemoveLabel("trigger:schedule"); (4) AddComment.
//   - → timed: (1) RemoveLabel("trigger:multi") — multi loader releases
//     first; (2) UpdateBeadFieldsTyped{cron: <restored>, triggers: "",
//     timed_cron: ""}; (3) AddLabel("trigger:schedule"); (4) AddComment.
//     Restored cron = timed_cron if non-empty, else StaggeredCron(epic, 30m).
//
// Both label sets present or both absent → error, zero writes. Requested
// mode == current mode → Result{Changed: false}, zero writes. Empty
// Opts.Operator → error, zero writes.
func Set(ctx context.Context, w Writer, b *beadsapi.BeadDetail, mode Mode, opts Opts) (Result, error) {
	if strings.TrimSpace(opts.Operator) == "" {
		return Result{}, fmt.Errorf("gardenermode.Set: operator is required for the audit comment")
	}

	cur, err := Detect(b)
	if err != nil {
		return Result{}, err
	}
	if cur == mode {
		if mode == ModeEvent {
			return refreshEvent(ctx, w, b, opts)
		}
		return Result{Changed: false, Mode: mode}, nil
	}

	// Epic id: gardener-epic label if present, else the trigger bead's own id
	// as the stagger seed (still deterministic). But the in_subtree_of match
	// value MUST be the epic id — event mode is meaningless without a subtree
	// to scope to.
	epicID := EpicOf(b.Labels)
	seed := epicID
	if seed == "" {
		seed = b.ID
	}

	switch mode {
	case ModeEvent:
		return setEvent(ctx, w, b, epicID, seed, opts)
	case ModeTimed:
		return setTimed(ctx, w, b, seed, opts)
	default:
		return Result{}, fmt.Errorf("gardenermode.Set: unknown mode %q", mode)
	}
}

func setEvent(ctx context.Context, w Writer, b *beadsapi.BeadDetail, epicID, seed string, opts Opts) (Result, error) {
	if epicID == "" {
		return Result{}, fmt.Errorf("gardener has no gardener-epic label; cannot scope event mode")
	}

	oldCron := b.Fields["cron"]

	tz := opts.Timezone
	if strings.TrimSpace(tz) == "" {
		tz = b.Fields["timezone"]
	}
	if strings.TrimSpace(tz) == "" {
		tz = defaultTZ
	}

	// The fallback watchdog cron is a slow floor that bounds staleness if the
	// SSE stream drops (spec 2026-07-13-gardener-mode-flip-design.md). It is
	// OPTIONAL. Resolution order:
	//   - NoFallbackCron → no watchdog at all (purely event-driven).
	//   - explicit FallbackCron → use it verbatim.
	//   - a PLAN gardener with no explicit cron → no watchdog (operator ask
	//     ka-0000008237: an event-triggered plan gardener gets no default cron).
	//     A plan gardener plangardener.Bind authors is born in event mode with a
	//     daily watchdog already, so this default only applies to a hand flip.
	//   - any other gardener with no explicit cron → the 6h staggered default.
	fallbackCron := ""
	wantFallback := !opts.NoFallbackCron
	if wantFallback && strings.TrimSpace(opts.FallbackCron) == "" && slices.Contains(b.Labels, planGardenerMarker) {
		wantFallback = false
	}
	if wantFallback {
		fallbackCron = opts.FallbackCron
		if strings.TrimSpace(fallbackCron) == "" {
			fallbackCron = StaggeredCron(seed, 6*time.Hour)
		}
	}
	entries := EventTriggers(epicID, b.Fields["project"], fallbackCron, tz, PreflightFromConditions(b.Fields["conditions"]))

	triggersJSON, err := json.Marshal(entries)
	if err != nil {
		return Result{}, fmt.Errorf("gardenermode.Set: marshal triggers: %w", err)
	}

	// (1) cron cleared first so the schedule loader goes dormant before the
	// label swap.
	if err := w.UpdateBeadFieldsTyped(ctx, b.ID, map[string]any{
		"timed_cron": oldCron,
		"cron":       "",
		"triggers":   string(triggersJSON),
	}); err != nil {
		return Result{}, err
	}
	// (2) then claim the multi loader.
	if err := w.AddLabel(ctx, b.ID, LabelMulti); err != nil {
		return Result{}, err
	}
	// (3) then release the schedule loader.
	if err := w.RemoveLabel(ctx, b.ID, LabelSchedule); err != nil {
		return Result{}, err
	}
	// (4) audit.
	fallbackDesc := fallbackCron
	if fallbackDesc == "" {
		fallbackDesc = "none (purely event-driven)"
	}
	comment := fmt.Sprintf("mode flip timed→event by %s (was cron %s; fallback %s)", opts.Operator, oldCron, fallbackDesc)
	if err := w.AddComment(ctx, b.ID, opts.Operator, comment); err != nil {
		return Result{}, err
	}

	return Result{Changed: true, Mode: ModeEvent, Cron: fallbackCron}, nil
}

// EventTriggers builds the event-mode triggers[] array for a gardener scoped to
// epicID. It is the ONE builder of that shape: `gb gardener mode --event` (Set),
// `gb gardener cadence` (SetCadence, via WatchdogEntry) and plangardener.Bind,
// which authors every new plan gardener in event mode (gd-gsvlS6YDj3), all call
// it, so a bound gardener and a flipped one carry the same triggers[].
//
// The array is a plan-activity bead_event entry (any bead created, updated or
// closed in the epic's subtree by anyone but the plan's own gardener, see
// PlanActivityMatch; debounced 10m and throttled 15m), then a
// steward-nudge entry when project is non-empty (see StewardNudgeEntry),
// followed by a fallback-watchdog cron entry when watchdogCron is non-empty.
// preflight, when non-nil, gates the watchdog (see WatchdogEntry); the
// bead_event entries are never gated, since a real change is always worth a
// look. project is the gardener trigger bead's project field.
func EventTriggers(epicID, project, watchdogCron, tz string, preflight *conditions.Expression) []scheduler.TriggerEntry {
	entries := []scheduler.TriggerEntry{
		{
			Kind:     scheduler.TriggerKindBeadEvent,
			Label:    PlanActivityLabel,
			Events:   []string{"created", "updated", "closed"},
			Match:    PlanActivityMatch(epicID),
			Debounce: "10m",
			Throttle: "15m",
		},
	}
	if strings.TrimSpace(project) != "" {
		entries = append(entries, StewardNudgeEntry(epicID, project))
	}
	if strings.TrimSpace(watchdogCron) != "" {
		entries = append(entries, WatchdogEntry(watchdogCron, tz, preflight))
	}
	return entries
}

// PlanActivityMatch is the plan-activity entry's match: a bead in epicID's
// subtree, changed by anyone but the plan's own gardener. Every bead event
// fires on its own (gd-wRullKPlWJ), so without the actor leaf a gardener's
// own step updates, creates and closes would wake it again each time its
// debounce and throttle ran out. Its runs are named
// <project>-plan-gardener-for-<epic, lowercased>-<suffix> (the trigger
// engine's run name from the default "Plan gardener for <epic>" title). An
// event with no actor matches.
func PlanActivityMatch(epicID string) *conditions.Expression {
	return &conditions.Expression{All: []conditions.Expression{
		// The actor leaf first: it is free, the subtree walk is a daemon read.
		{Not: &conditions.Expression{Leaf: conditions.Leaf{Field: "actor", Op: "contains", Value: GardenerRunMarker(epicID)}}},
		{Leaf: conditions.Leaf{Field: "id", Op: "in_subtree_of", Value: epicID}},
	}}
}

// UpgradeLegacyPlanActivity rewrites, in entries, a plan-activity match still
// in its pre-gd-wRullKPlWJ shape (the bare in_subtree_of epicID leaf) to
// PlanActivityMatch, and reports whether it did. The trigger engine applies it
// on load, so a gardener whose stored triggers[] predates the actor leaf is
// not woken by its own writes before `gb gardener mode <id> --event` rewrites
// it. Any other match is left as it is.
func UpgradeLegacyPlanActivity(entries []scheduler.TriggerEntry, epicID string) bool {
	if epicID == "" {
		return false
	}
	legacy := conditions.Expression{Leaf: conditions.Leaf{Field: "id", Op: "in_subtree_of", Value: epicID}}
	upgraded := false
	for i := range entries {
		e := &entries[i]
		if e.Kind != scheduler.TriggerKindBeadEvent || e.Label != PlanActivityLabel || e.Match == nil {
			continue
		}
		if reflect.DeepEqual(*e.Match, legacy) {
			e.Match = PlanActivityMatch(epicID)
			upgraded = true
		}
	}
	return upgraded
}

// GardenerRunMarker is the part of every run name of epicID's plan gardener
// that no other agent's name carries.
func GardenerRunMarker(epicID string) string {
	return "-plan-gardener-for-" + strings.ToLower(epicID) + "-"
}

const (
	// PlanActivityLabel labels the subtree bead_event entry.
	PlanActivityLabel = "plan-activity"
	// StewardNudgeLabel labels the entry that wakes a gardener on its land
	// steward's nudge comment (gd-bo2RUmtpHb).
	StewardNudgeLabel = conditions.QuietNudgeLabel
	// StewardNudgeMarker is the text every steward nudge comment carries
	// (conditions.QuietNudgeText).
	StewardNudgeMarker = conditions.QuietNudgeMarker
	// stewardNudgeThrottle bounds the wakes the nudge entry can cause.
	stewardNudgeThrottle = "1h"
)

// StewardAuthor is the comment-author prefix of a land's steward runs
// (conditions.StewardRunPrefix). The trigger engine names a run
// <project>-<slug of the trigger title>-<suffix> (bridge.triggerAgentName),
// and a steward trigger's title is "Steward (<land>)", so the home steward's
// runs are gardenio-steward-<suffix>. A comment's author is the daemon's
// authenticated subject (an agent's per-agent token subject is its name),
// never a value the writer supplies.
func StewardAuthor(project string) string { return conditions.StewardRunPrefix(project) }

// StewardNudgeEntry builds the steward-nudge bead_event entry: a comment added
// on the plan epic itself (id == epicID, so not a comment anywhere in the
// subtree) whose author is one of the land steward's runs and whose text
// carries StewardNudgeMarker. The comment fields come from the comment.added
// frame (scheduler.PayloadCommentAuthor / PayloadCommentText; the hydrated
// bead's own created_by is the epic's author); a missing field never matches,
// so no other edge event (a label or dependency added) can satisfy it.
// Throttled to 1h, so one steward pass wakes a gardener at most once an hour.
// A crew or gardener comment fails the author leaf: their runs are named after
// their task or plan, under <project>-plan-gardener-for-... for a gardener.
func StewardNudgeEntry(epicID, project string) scheduler.TriggerEntry {
	return scheduler.TriggerEntry{
		Kind:   scheduler.TriggerKindBeadEvent,
		Label:  StewardNudgeLabel,
		Events: []string{"comment.added"},
		Match: &conditions.Expression{All: []conditions.Expression{
			{Leaf: conditions.Leaf{Field: "id", Op: "==", Value: epicID}},
			{Leaf: conditions.Leaf{Field: scheduler.PayloadCommentAuthor, Op: "contains", Value: StewardAuthor(project)}},
			{Leaf: conditions.Leaf{Field: scheduler.PayloadCommentText, Op: "contains", Value: StewardNudgeMarker}},
		}},
		Throttle: stewardNudgeThrottle,
	}
}

// RefreshEventTriggers returns the triggers[] an event-mode gardener should
// carry now, and whether that differs from what it carries. The standard
// bead_event entries (plan-activity, steward-nudge) are rewritten to the current
// EventTriggers shape; any other non-cron entry is kept as it is; the watchdog
// cron entries are kept unless opts names a new one (FallbackCron) or none
// (NoFallbackCron). This is how `gb gardener mode --event` brings a gardener
// flipped before an entry existed up to date (gd-bo2RUmtpHb). A bead with no
// gardener-epic label has no subtree to scope to and is left alone.
func RefreshEventTriggers(b *beadsapi.BeadDetail, opts Opts) ([]scheduler.TriggerEntry, bool, error) {
	epicID := EpicOf(b.Labels)
	if epicID == "" {
		return nil, false, nil
	}
	cur, err := scheduler.ParseTriggers(b.Fields["triggers"])
	if err != nil {
		return nil, false, fmt.Errorf("gardenermode: parse triggers: %w", err)
	}
	tz := opts.Timezone
	if strings.TrimSpace(tz) == "" {
		tz = b.Fields["timezone"]
	}
	if strings.TrimSpace(tz) == "" {
		tz = defaultTZ
	}
	newWatchdog := opts.NoFallbackCron || strings.TrimSpace(opts.FallbackCron) != ""

	next := EventTriggers(epicID, b.Fields["project"], "", tz, nil)
	var crons []scheduler.TriggerEntry
	for _, e := range cur {
		switch {
		case e.Kind == scheduler.TriggerKindCron:
			if !newWatchdog {
				crons = append(crons, e)
			}
		case e.Label == PlanActivityLabel || e.Label == StewardNudgeLabel:
			// rewritten above
		default:
			next = append(next, e)
		}
	}
	if newWatchdog && !opts.NoFallbackCron {
		crons = []scheduler.TriggerEntry{WatchdogEntry(strings.TrimSpace(opts.FallbackCron), tz, PreflightFromConditions(b.Fields["conditions"]))}
	}
	next = append(next, crons...)

	was, err := json.Marshal(cur)
	if err != nil {
		return nil, false, err
	}
	now, err := json.Marshal(next)
	if err != nil {
		return nil, false, err
	}
	return next, string(was) != string(now), nil
}

// refreshEvent is Set on a gardener already in event mode: it writes the
// RefreshEventTriggers array when it differs, and is a no-op otherwise.
func refreshEvent(ctx context.Context, w Writer, b *beadsapi.BeadDetail, opts Opts) (Result, error) {
	entries, changed, err := RefreshEventTriggers(b, opts)
	if err != nil || !changed {
		return Result{Changed: false, Mode: ModeEvent}, err
	}
	triggersJSON, err := json.Marshal(entries)
	if err != nil {
		return Result{}, fmt.Errorf("gardenermode.Set: marshal triggers: %w", err)
	}
	if err := w.UpdateBeadFieldsTyped(ctx, b.ID, map[string]any{"triggers": string(triggersJSON)}); err != nil {
		return Result{}, err
	}
	labels := make([]string, 0, len(entries))
	cron := ""
	for _, e := range entries {
		labels = append(labels, e.Label)
		if e.Kind == scheduler.TriggerKindCron && cron == "" {
			cron = e.Cron
		}
	}
	comment := fmt.Sprintf("event-mode triggers refreshed to %s by %s", strings.Join(labels, ", "), opts.Operator)
	if err := w.AddComment(ctx, b.ID, opts.Operator, comment); err != nil {
		return Result{}, err
	}
	return Result{Changed: true, Mode: ModeEvent, Cron: cron, Refreshed: true}, nil
}

// WatchdogEntry builds the fallback-watchdog cron entry of an event-mode
// gardener. The preflight is the bead's open-children gate carried onto the
// entry: MultiTriggerSource never reads the bead's top-level `conditions` field
// (only ScheduleTriggerSource does), so without it the watchdog would spawn an
// idle iteration even after every step closes (ka-0000008237). A nil preflight
// leaves the entry ungated.
func WatchdogEntry(cron, tz string, preflight *conditions.Expression) scheduler.TriggerEntry {
	return scheduler.TriggerEntry{
		Kind:      scheduler.TriggerKindCron,
		Label:     "fallback-watchdog",
		Cron:      cron,
		Timezone:  tz,
		Preflight: preflight,
	}
}

// PreflightFromConditions lowers a gardener trigger bead's top-level
// `conditions` field (a JSON array of legacy Condition leaves, the open-children
// pre-flight gate plangardener.Bind writes) into a single Expression suitable for
// a TriggerEntry.Preflight. Returns nil when the field is empty, unparseable, or
// carries no leaves — the safe default (no per-entry gate), which just restores
// the pre-flip behaviour of an ungated watchdog.
//
// A single condition lowers to that leaf directly; multiple conditions AND-
// combine (matching EvaluateConditions' all-must-pass semantics).
func PreflightFromConditions(raw string) *conditions.Expression {
	if strings.TrimSpace(raw) == "" {
		return nil
	}
	conds, err := conditions.ParseConditions(raw)
	if err != nil || len(conds) == 0 {
		return nil
	}
	if len(conds) == 1 {
		e := conds[0].ToExpression()
		return &e
	}
	all := make([]conditions.Expression, 0, len(conds))
	for _, c := range conds {
		all = append(all, c.ToExpression())
	}
	return &conditions.Expression{All: all}
}

func setTimed(ctx context.Context, w Writer, b *beadsapi.BeadDetail, seed string, opts Opts) (Result, error) {
	restored := b.Fields["timed_cron"]
	if strings.TrimSpace(restored) == "" {
		restored = StaggeredCron(seed, 30*time.Minute)
	}

	// (1) multi loader releases first.
	if err := w.RemoveLabel(ctx, b.ID, LabelMulti); err != nil {
		return Result{}, err
	}
	// (2) restore the timed cron and clear the event-mode fields.
	if err := w.UpdateBeadFieldsTyped(ctx, b.ID, map[string]any{
		"cron":       restored,
		"triggers":   "",
		"timed_cron": "",
	}); err != nil {
		return Result{}, err
	}
	// (3) then claim the schedule loader.
	if err := w.AddLabel(ctx, b.ID, LabelSchedule); err != nil {
		return Result{}, err
	}
	// (4) audit.
	comment := fmt.Sprintf("mode flip event→timed by %s (restored cron %s)", opts.Operator, restored)
	if err := w.AddComment(ctx, b.ID, opts.Operator, comment); err != nil {
		return Result{}, err
	}

	return Result{Changed: true, Mode: ModeTimed, Cron: restored}, nil
}

// SetCadence tunes the firing cadence of a gardener WITHOUT flipping its mode.
// It is the "set/tune the cadence" operator verb the plan interface exposes
// (ka-0000008237), so an operator can dial a plan gardener's tempo from the plan
// cockpit rather than only the /gardener/{id} config page.
//
// The meaning of `cron` depends on the gardener's current mode:
//
//   - timed  — `cron` becomes the flat `cron` field (the firing schedule). An
//     empty cron is rejected: a timed gardener with no cron never fires.
//   - event  — `cron` becomes the fallback-watchdog cron entry in triggers[].
//     An EMPTY cron removes the watchdog entirely (purely event-driven, the
//     plan default); a non-empty cron installs/replaces the watchdog, carrying
//     the bead's `conditions` gate onto it as a per-entry preflight (same as a
//     fresh flip). The bead_event entry is preserved untouched.
//
// tz defaults to the bead's timezone field then defaultTZ. Empty Operator →
// error, zero writes (the audit comment must be attributed). Detect's ambiguous
// label state → error, zero writes.
func SetCadence(ctx context.Context, w Writer, b *beadsapi.BeadDetail, cron, tz string, opts Opts) (Result, error) {
	if strings.TrimSpace(opts.Operator) == "" {
		return Result{}, fmt.Errorf("gardenermode.SetCadence: operator is required for the audit comment")
	}
	cur, err := Detect(b)
	if err != nil {
		return Result{}, err
	}
	cron = strings.TrimSpace(cron)
	if strings.TrimSpace(tz) == "" {
		tz = b.Fields["timezone"]
	}
	if strings.TrimSpace(tz) == "" {
		tz = defaultTZ
	}

	switch cur {
	case ModeTimed:
		if cron == "" {
			return Result{}, fmt.Errorf("gardenermode.SetCadence: a timed gardener requires a non-empty cron")
		}
		if err := w.UpdateBeadFieldsTyped(ctx, b.ID, map[string]any{
			"cron":     cron,
			"timezone": tz,
		}); err != nil {
			return Result{}, err
		}
		comment := fmt.Sprintf("cadence set to cron %s by %s (timed)", cron, opts.Operator)
		if err := w.AddComment(ctx, b.ID, opts.Operator, comment); err != nil {
			return Result{}, err
		}
		return Result{Changed: true, Mode: ModeTimed, Cron: cron}, nil

	case ModeEvent:
		entries, err := scheduler.ParseTriggers(b.Fields["triggers"])
		if err != nil {
			return Result{}, fmt.Errorf("gardenermode.SetCadence: parse triggers: %w", err)
		}
		// Rebuild the triggers[] array: keep every non-cron source (the
		// bead_event plan-activity entry), then append a single fresh watchdog
		// cron entry iff a cron was given.
		rebuilt := make([]scheduler.TriggerEntry, 0, len(entries)+1)
		for _, e := range entries {
			if e.Kind == scheduler.TriggerKindCron {
				continue
			}
			rebuilt = append(rebuilt, e)
		}
		if cron != "" {
			rebuilt = append(rebuilt, WatchdogEntry(cron, tz, PreflightFromConditions(b.Fields["conditions"])))
		}
		triggersJSON, err := json.Marshal(rebuilt)
		if err != nil {
			return Result{}, fmt.Errorf("gardenermode.SetCadence: marshal triggers: %w", err)
		}
		if err := w.UpdateBeadFieldsTyped(ctx, b.ID, map[string]any{
			"triggers": string(triggersJSON),
		}); err != nil {
			return Result{}, err
		}
		desc := cron
		if desc == "" {
			desc = "none (purely event-driven)"
		}
		comment := fmt.Sprintf("event-mode fallback cadence set to %s by %s", desc, opts.Operator)
		if err := w.AddComment(ctx, b.ID, opts.Operator, comment); err != nil {
			return Result{}, err
		}
		return Result{Changed: true, Mode: ModeEvent, Cron: cron}, nil

	default:
		return Result{}, fmt.Errorf("gardenermode.SetCadence: unknown mode %q", cur)
	}
}
