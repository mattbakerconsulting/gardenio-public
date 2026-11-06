// Package gardenerwatch is the home steward's watch on its plans' gardeners:
// the building plans with no live gardener (gd-hVTkwcDwxK, stalled.go) and
// those whose gardener is quiet or still timed (gd-bo2RUmtpHb), the ask that
// follows, the line the steward's prompt shows for each, and the
// quiet-gardeners wake.
// It lives apart from package steward, which imports it, to keep steward
// inside its line budget; it imports nothing of steward's.
package gardenerwatch

import (
	"context"
	"fmt"
	"hash/fnv"
	"strconv"
	"strings"
	"time"

	"gardenio/controller/internal/scheduler"
	"gardenio/controller/internal/scheduler/conditions"
)

// Quiet and timed plan gardeners (gd-bo2RUmtpHb). Since 11-01 each home
// plan's gardener wakes on its own plan's changes (event mode) rather than a
// 30-minute cron, so a gardener can now stay quiet while its plan has work
// ready. The owner ruled that the land's steward watches for it. The check is
// conditions.FindQuietGardeners, shared with the quiet_gardeners preflight of
// the steward's quiet-gardeners wake; this file turns what it finds into the
// asks the steward makes.
//
// Firing a plan gardener is a spawn into the plan-gardener role, which the
// steward may not make: its lands-tenant tier spawns only lands-gardener in
// its own land (kbeads lands_tenant_authz.go landsTenantSpawnTargetDenial),
// the role holds a privileged ownership_grants row so a non-human spawn of it
// must escalate
// (spawn_authz.go roleIsPrivileged), and the tier may not turn a denial into
// an escalation (restricted_ops.go opEscalationRetry: tenantNever). What the
// steward can do is comment, and a comment it adds on the plan epic carrying
// conditions.QuietNudgeMarker wakes the gardener through its steward-nudge
// entry (gardenermode.StewardNudgeEntry). So the first sighting in a quiet
// spell is a nudge; only a gardener still quiet QuietGardenerAfter later, or
// one with no steward-nudge entry, goes to the owner as a decision naming the
// exact command, the way the gardener-less asks do, noted on the plan.

// Ask is what the steward asks of the owner for a quiet or timed plan
// gardener.
type Ask string

const (
	// AskNudge: the gardener is quiet with steps ready and not yet nudged in
	// this spell; the steward adds the nudge comment on the plan.
	AskNudge Ask = "nudge"
	// AskNudged: the steward nudged it less than QuietGardenerAfter ago;
	// nothing to do until the gardener has had that long to answer.
	AskNudged Ask = "nudged"
	// AskPassed: the gardener answered conditions.QuietNudgeCap nudges in the
	// day, yet steps ready before the first are still ready, so it may be
	// passing over them: the owner gets one decision (look at the steps,
	// pause the gardener, or leave it) in place of a third nudge.
	AskPassed Ask = "passed"
	// AskFire: the gardener is quiet with steps ready, and a nudge did not
	// wake it (or it has no steward-nudge entry); the steward fires it once
	// with gb gardener fire-now.
	AskFire Ask = "fire"
	// AskFireAgain: AskFire, and the steward asked about this gardener
	// earlier in the day, so it is the second time: the owner is told too.
	AskFireAgain Ask = "again"
	// AskEventMode: the gardener is still timed (trigger:schedule); the owner
	// puts it in event mode with gb gardener mode --event.
	AskEventMode Ask = "event"
	// AskAsked: a decision the steward filed already raises this; nothing
	// to file.
	AskAsked Ask = "asked"
)

// Plan is a building plan of the home garden whose gardener is quiet or
// still timed, with the ask that follows and the owner's command.
type Plan struct {
	conditions.QuietGardener
	Ask Ask
	// Fix is the command the home steward runs to set it right.
	Fix string
	// Nudge is the steward's own nudge command (AskNudge only).
	Nudge string
}

// shown caps the quiet and timed plans in the prompt.
const shown = 20

// Find checks the land's building plans, refs, for a quiet or timed gardener
// at now; more counts those past the 20 shown.
func Find(ctx context.Context, src conditions.BeadClient, land string, refs []conditions.QuietPlanRef, now time.Time) (out []Plan, more int, err error) {
	found, err := conditions.FindQuietGardeners(ctx, src, land, refs, now)
	if err != nil {
		return nil, 0, err
	}
	for _, q := range found {
		if len(out) == shown {
			more++
			continue
		}
		qp := Plan{QuietGardener: q, Ask: quietAsk(q), Fix: quietFix(q)}
		if qp.Ask == AskNudge {
			qp.Nudge = fmt.Sprintf("kd comment add %s %q", q.Plan, conditions.QuietNudgeText(q.Trigger, q.QuietFor, q.ReadySteps))
		}
		out = append(out, qp)
	}
	return out, more, nil
}

// quietAsk is the ask a quiet or timed gardener calls for.
func quietAsk(q conditions.QuietGardener) Ask {
	switch {
	case q.Raised != "":
		return AskAsked
	case q.Kind == conditions.QuietKindTimed:
		return AskEventMode
	case q.NudgePending():
		return AskNudged
	case q.NeedsNudge():
		return AskNudge
	case q.NudgeCapped() && len(q.Passed) > 0:
		return AskPassed
	case q.AskedBefore():
		return AskFireAgain
	}
	return AskFire
}

// quietFix is the steward's command for q.
func quietFix(q conditions.QuietGardener) string {
	switch {
	case q.Kind == conditions.QuietKindTimed:
		return fmt.Sprintf("gb gardener mode %s --event --fallback-cron %q", q.Trigger, FallbackCron(q.Trigger, q.Cron))
	case q.NudgeCapped() && len(q.Passed) > 0:
		// Pausing is the one fix of the passed ask that is a command.
		return "gb trigger disable " + q.Trigger
	}
	return "gb gardener fire-now " + q.Trigger
}

// FallbackCron is the daily watchdog cron a timed plan gardener gets when it
// is put in event mode, the shape the 11-01 flip gave every plan gardener
// (gd-fULNT8wZNF: one fire a day, at the minute of its old cron): the first
// minute of cron, else one seeded from the trigger, at an hour seeded from
// the trigger so they do not all fire at once.
func FallbackCron(trigger, cron string) string {
	h := fnv.New32a()
	_, _ = h.Write([]byte(trigger))
	n := h.Sum32()
	minute := int(n % 60)
	if f := strings.Fields(cron); len(f) > 0 {
		first, _, _ := strings.Cut(f[0], ",")
		if m, err := strconv.Atoi(first); err == nil && m >= 0 && m < 60 {
			minute = m
		}
	}
	return fmt.Sprintf("%d %d * * *", minute, (n/60)%24)
}

// Describe is one quiet or timed plan's line in the prompt.
func Describe(q Plan) string {
	line := q.Plan + ": " + q.PlanTitle + ": gardener " + q.Trigger
	if q.Kind == conditions.QuietKindTimed {
		line += " is still timed (trigger:schedule"
		if q.Cron != "" {
			line += ", cron " + strconv.Quote(q.Cron)
		}
		line += ")"
	} else {
		if q.LastRun.IsZero() {
			line += " has not run since it was bound " + agoText(q.QuietFor) + " ago"
		} else {
			line += " has not run for " + agoText(q.QuietFor) + " (last ran " + q.LastRun.UTC().Format("01-02 15:04Z") + ")"
		}
		if q.LastOutcome != "" {
			line += ", last outcome " + strconv.Quote(q.LastOutcome)
		}
		line += fmt.Sprintf("; %d ready: %s", q.ReadySteps, strings.Join(q.Ready, ", "))
		if q.ReadySteps > len(q.Ready) {
			line += ", …"
		}
	}
	if !q.Nudged.IsZero() {
		line += "; you nudged it " + agoText(q.NudgedFor) + " ago"
	} else if q.NudgeCapped() {
		line += fmt.Sprintf("; it answered your %d nudges today (the most), the last %s ago", len(q.Nudges), agoText(q.Since.Add(q.QuietFor).Sub(q.Nudges[len(q.Nudges)-1])))
		if len(q.Passed) > 0 {
			line += "; ready since before the first: " + strings.Join(q.Passed, ", ")
		}
	} else if q.Kind == conditions.QuietKindQuiet && !q.Nudgeable && (q.Ask == AskFire || q.Ask == AskFireAgain) {
		line += "; it has no steward-nudge wake"
	}
	switch q.Ask {
	case AskAsked:
		line += "; you asked in decision " + q.Raised
	case AskFireAgain:
		line += "; you asked about it earlier today (decision " + q.Asked[len(q.Asked)-1].ID + ")"
	}
	line += ". Ask: " + string(q.Ask)
	if q.Nudge != "" {
		line += ". Nudge: " + q.Nudge
	}
	return line + ". Fix: " + q.Fix
}

// agoText is d as hours and minutes, "3h05m", or minutes under an hour.
func agoText(d time.Duration) string {
	d = d.Round(time.Minute)
	if d < time.Hour {
		return fmt.Sprintf("%dm", int(d/time.Minute))
	}
	return fmt.Sprintf("%dh%02dm", int(d/time.Hour), int(d%time.Hour/time.Minute))
}

// WakeLabel is the home steward's quiet-gardeners wake: a cron every two
// hours whose preflight is the quiet_gardeners leaf, so it fires only when a
// plan's gardener is quiet or still timed.
const WakeLabel = "quiet-gardeners"

// Entry is the home steward's quiet-gardeners wake: a cron every two hours
// (WatchCron) gated by the quiet_gardeners preflight, which trigger-engine
// evaluates itself, with no agent, and which holds only while at least one
// building plan's gardener is quiet or still timed and no decision the
// steward filed already raises it (conditions.FindQuietGardeners). A read
// error fails it closed: a watch that cannot see wakes nobody.
func Entry(land, tz string) scheduler.TriggerEntry {
	return scheduler.TriggerEntry{
		Kind: scheduler.TriggerKindCron, Label: WakeLabel, Cron: WatchCron(land), Timezone: tz,
		Preflight: &conditions.Expression{Leaf: conditions.Leaf{Type: conditions.QuietGardenersLeaf, Op: ">", Value: "0", Project: land}},
	}
}

// WatchCron is the quiet-gardeners wake's cron: every two hours, at a minute
// seeded from the land's name.
func WatchCron(land string) string {
	h := fnv.New32a()
	_, _ = h.Write([]byte(land + "/" + WakeLabel))
	return fmt.Sprintf("%d */2 * * *", h.Sum32()%60)
}
