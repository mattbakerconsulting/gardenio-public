package conditions

import (
	"context"
	"encoding/json"
	"fmt"
	"reflect"
	"slices"
	"strings"
	"sync"
	"time"

	"gardenio/controller/internal/beadsapi"
)

// Quiet plan gardeners (gd-bo2RUmtpHb). On 11-01 the home garden's plan
// gardeners moved from a 30-minute cron to event mode: each wakes on its own
// plan's bead changes, with a daily fallback cron. The risk that brings is a
// gardener that stays quiet while its plan has work ready. The land's steward
// owns watching for it (owner ruling 11-01), so the check is deterministic Go
// shared by two callers:
//
//   - the quiet_gardeners daemon leaf, the preflight of the home steward's
//     quiet-gardeners wake: trigger-engine evaluates it without spawning
//     anything, so the wake costs nothing while every gardener is fine;
//   - the steward's prompt (internal/steward), which lists each plan found
//     with what to do about it.
//
// A quiet gardener is first nudged: the steward comments on the plan epic,
// and that comment wakes the gardener through its steward-nudge entry
// (gardenermode.StewardNudgeEntry), so the steward needs no new privilege.
// Only a gardener still quiet QuietGardenerAfter after the nudge goes to the
// owner as a decision.
//
// It lives here, not in internal/steward or internal/plangardener, because
// both of those import this package. The plan-gardener labels are mirrored
// below for the same reason (gardenermode does the same).

// QuietGardenersLeaf is the leaf's Type.
const QuietGardenersLeaf = "quiet_gardeners"

const (
	// QuietGardenerAfter is how long a plan gardener may go without a run
	// while its plan has a ready step before it is quiet.
	QuietGardenerAfter = 2 * time.Hour
	// QuietGardenerAskWindow is how far back the steward's own decisions
	// about a gardener are read: a second quiet spell inside it is said to
	// the owner, and a timed gardener is asked about at most once in it.
	QuietGardenerAskWindow = 24 * time.Hour

	// quietPlanGardenerMarker mirrors plangardener.MarkerLabel: the label
	// plangardener.Bind puts on every plan gardener.
	quietPlanGardenerMarker = "plan-gardener"
	// quietEpicLabelPrefix mirrors plangardener.GardenerEpicLabel's prefix.
	quietEpicLabelPrefix = "gardener-epic:"
	// quietTimedLabel and quietEventLabel mirror gardenermode's mode labels.
	quietTimedLabel = "trigger:schedule"
	quietEventLabel = "trigger:multi"
	// quietStewardRole is the steward role (steward.Role), which names its
	// runs <land>-steward-<suffix>.
	quietStewardRole = "steward"

	// quietGardenerScan caps the open plan gardeners read.
	quietGardenerScan = 1000
	// quietReadyScan caps the ready steps read per plan.
	quietReadyScan = 50
	// quietReadyShown caps the ready step ids carried for the prompt.
	quietReadyShown = 3
	// quietOutcomeChars cuts a gardener's last_outcome.
	quietOutcomeChars = 160
)

// QuietNudgeMarker ends every steward nudge comment (QuietNudgeText). The
// gardener's steward-nudge entry matches it (gardenermode.StewardNudgeEntry),
// and the check reads a plan comment carrying it, by a steward run of the
// land, as the nudge for the spell.
const QuietNudgeMarker = "; nudging"

// QuietNudgeLabel is the label of the gardener's triggers[] entry a nudge
// wakes (gardenermode.StewardNudgeLabel). A gardener without one cannot be
// nudged, so its spell goes straight to the owner.
const QuietNudgeLabel = "steward-nudge"

// QuietNudgeCap is the most nudges the steward makes on one plan in a rolling
// QuietGardenerAskWindow. A gardener that has answered that many while steps
// that were ready before the first are still ready may be passing over them,
// and the next spell goes to the owner as one decision instead of a third
// nudge.
const QuietNudgeCap = 2

// StewardRunPrefix is the name prefix of land's steward runs, which is the
// author of every comment and the created_by of every decision they write:
// the engine names a run <project>-<slug of the trigger title>-<suffix>, and
// a steward trigger is titled "Steward (<land>)".
func StewardRunPrefix(land string) string { return land + "-" + quietStewardRole + "-" }

// QuietNudgeText is the comment the steward adds to a quiet gardener's plan:
// "steward: gardener <trigger> quiet <n>h with <k> ready steps; nudging".
func QuietNudgeText(trigger string, quietFor time.Duration, ready int) string {
	steps := "steps"
	if ready == 1 {
		steps = "step"
	}
	return fmt.Sprintf("steward: gardener %s quiet %dh with %d ready %s%s", trigger, int(quietFor/time.Hour), ready, steps, QuietNudgeMarker)
}

// CommentClient reads a bead's comments. The quiet check needs it to see the
// steward's nudges; *beadsapi.Client has it. A client without it fails the
// check, which wakes nobody.
type CommentClient interface {
	GetComments(ctx context.Context, beadID string) ([]beadsapi.Comment, error)
}

// QuietReadyTypes are the step types a ready step may be.
var QuietReadyTypes = []string{"task", "bug", "feature", "chore"}

// QuietGardenerKind says what is wrong with a plan's gardener.
type QuietGardenerKind string

const (
	// QuietKindQuiet: the plan has a ready step and its gardener has not
	// run for QuietGardenerAfter, with no cycle live.
	QuietKindQuiet QuietGardenerKind = "quiet"
	// QuietKindTimed: the gardener is still in timed mode (label
	// trigger:schedule), a regression since 11-01: it should be in event
	// mode, fixed with gb gardener mode <trigger> --event.
	QuietKindTimed QuietGardenerKind = "timed"
)

// QuietPlanRef is a building plan the check reads.
type QuietPlanRef struct {
	ID    string
	Title string
}

// QuietDecision is a decision a steward run of the land filed about a
// gardener (gb decision create --subject <gardener>, which stamps
// subject_trigger_id).
type QuietDecision struct {
	ID        string
	CreatedAt time.Time
	Status    string
}

// QuietGardener is one building plan whose bound, enabled gardener is quiet
// or timed.
type QuietGardener struct {
	Plan      string
	PlanTitle string
	Kind      QuietGardenerKind
	// Trigger is the gardener trigger bead; Cron its timed cron (timed only).
	Trigger string
	Cron    string
	// LastRun is the gardener's last_run (its last fire), zero when it never
	// fired; Since is LastRun, else when the trigger was created. QuietFor is
	// how long before the check's now that was.
	LastRun  time.Time
	Since    time.Time
	QuietFor time.Duration
	// LastOutcome is the first line of the gardener's last_outcome, cut.
	LastOutcome string
	// ReadySteps counts the plan's ready steps; Ready names the first few.
	ReadySteps int
	Ready      []string
	// Asked are the steward's decisions about this gardener filed in the
	// last QuietGardenerAskWindow, oldest first.
	Asked []QuietDecision
	// Raised is the decision that already raises this spell: for a quiet
	// gardener one filed since Since (the gardener has not run since it was
	// asked), for a timed one any in the window. "" when none does.
	Raised string
	// Nudgeable reports whether the gardener carries a QuietNudgeLabel entry
	// in triggers[], so a nudge comment wakes it.
	Nudgeable bool
	// Nudged is when the steward last nudged a quiet gardener in this spell
	// (a QuietNudgeMarker comment on the plan by a steward run, since
	// Since), zero when it has not; NudgedFor is how long before now. Read
	// only for a quiet gardener no decision raises.
	Nudged    time.Time
	NudgedFor time.Duration
	// Nudges are the steward's nudges on the plan in the last
	// QuietGardenerAskWindow, oldest first, in any spell.
	Nudges []time.Time
	// Passed are the ready steps (the first few) that were already open
	// before the first of Nudges and are still ready: the steps a gardener
	// that answered its nudges has passed over.
	Passed []string
}

// NudgeCapped reports whether a quiet gardener is due a nudge the cap
// forbids: it can be nudged and is not nudged in this spell, but the steward
// has nudged the plan QuietNudgeCap times in the window already.
func (q QuietGardener) NudgeCapped() bool {
	return q.Kind == QuietKindQuiet && q.Nudgeable && q.Nudged.IsZero() && len(q.Nudges) >= QuietNudgeCap
}

// Wakes reports whether the plan counts for the steward's wake: it is quiet
// or timed, no decision already raises it, and it is not waiting out a nudge.
func (q QuietGardener) Wakes() bool { return q.Raised == "" && !q.NudgePending() }

// NeedsNudge reports whether a quiet gardener is to be nudged: it can be, no
// decision raises the spell, and the steward has not nudged it in it.
func (q QuietGardener) NeedsNudge() bool {
	return q.Kind == QuietKindQuiet && q.Nudgeable && q.Raised == "" && q.Nudged.IsZero() && len(q.Nudges) < QuietNudgeCap
}

// NudgePending reports whether a quiet gardener was nudged less than
// QuietGardenerAfter ago in this spell: the nudge counts as acted on until
// then, the way a decision does.
func (q QuietGardener) NudgePending() bool {
	return q.Kind == QuietKindQuiet && q.Raised == "" && !q.Nudged.IsZero() && q.NudgedFor < QuietGardenerAfter
}

// AskedBefore reports whether the steward asked about this gardener earlier
// in the window, for a spell it does not already raise: the second time in
// a day.
func (q QuietGardener) AskedBefore() bool { return q.Raised == "" && len(q.Asked) > 0 }

// ListQuietPlans reads land's building plans: open plan epics whose
// plan_state is building.
func ListQuietPlans(ctx context.Context, client BeadClient, land string) ([]QuietPlanRef, error) {
	res, err := client.ListBeadsFiltered(ctx, beadsapi.ListBeadsQuery{
		Types:    []string{"epic"},
		Labels:   []string{"project:" + land, "plan"},
		Statuses: []string{"open", "in_progress", "blocked"},
	})
	if err != nil {
		return nil, fmt.Errorf("quiet_gardeners: reading %s's plans: %w", land, err)
	}
	var out []QuietPlanRef
	for _, b := range res.Beads {
		if b != nil && b.Status != "closed" && strings.TrimSpace(b.Fields["plan_state"]) == "building" {
			out = append(out, QuietPlanRef{ID: b.ID, Title: b.Title})
		}
	}
	return out, nil
}

// FindQuietGardeners checks each building plan of land (plans, from
// ListQuietPlans or the caller's own read) at now. A plan counts when its
// newest open, enabled gardener trigger, labelled plan-gardener and
// gardener-epic:<plan>, is:
//
//   - timed: still labelled trigger:schedule; or
//   - quiet: in event mode, with no live cycle, no run for at least
//     QuietGardenerAfter (last_run, else since the trigger was created), and
//     at least one ready step under the plan.
//
// A ready step is open, unassigned, of a QuietReadyTypes type, not HumanOnly,
// neither blocked on nor parked behind an open decision (gb ready's PARKED,
// ka-0000003797), with no open blocking dependency: the daemon's NoOpenDeps
// filter, as gb ready and the ready_queue leaf use. An open blocker keeps a
// step waiting however deep its chain runs, so the one-hop filter is the
// transitive answer; bead_query's exclude_blocked_by_open_types walk only
// tells an operator wait from other waits, and this check counts neither.
//
// A plan with no live gardener is not here: the steward's gardener-less list
// (gd-hVTkwcDwxK) has it, and the leaf counts it apart (countGardenerless). Any read error fails the whole check: a watch that
// cannot see must not wake anyone, nor say a plan is fine. A last_run that
// cannot be parsed leaves that plan out for the same reason.
func FindQuietGardeners(ctx context.Context, client BeadClient, land string, plans []QuietPlanRef, now time.Time) ([]QuietGardener, error) {
	if len(plans) == 0 {
		return nil, nil
	}
	res, err := client.ListBeadsFiltered(ctx, beadsapi.ListBeadsQuery{
		Types:    []string{"trigger", "schedule"},
		Labels:   []string{quietPlanGardenerMarker},
		Statuses: []string{"open", "in_progress", "blocked"},
		Limit:    quietGardenerScan,
	})
	if err != nil {
		return nil, fmt.Errorf("quiet_gardeners: reading the plan gardeners: %w", err)
	}
	byPlan := map[string]*beadsapi.BeadDetail{}
	for _, b := range res.Beads {
		if b == nil || b.Status == "closed" || !slices.Contains(b.Labels, quietPlanGardenerMarker) ||
			strings.TrimSpace(b.Fields["enabled"]) == "false" {
			continue
		}
		for _, l := range b.Labels {
			plan, ok := strings.CutPrefix(l, quietEpicLabelPrefix)
			if !ok || plan == "" {
				continue
			}
			if cur := byPlan[plan]; cur == nil || b.CreatedAt.After(cur.CreatedAt) {
				byPlan[plan] = b
			}
		}
	}
	sorted := slices.Clone(plans)
	slices.SortFunc(sorted, func(a, b QuietPlanRef) int { return strings.Compare(a.ID, b.ID) })
	var out []QuietGardener
	for _, p := range sorted {
		g := byPlan[p.ID]
		if g == nil {
			continue
		}
		q, ok, err := checkGardener(ctx, client, land, p, g, now)
		if err != nil {
			return nil, err
		}
		if ok {
			out = append(out, q)
		}
	}
	return out, nil
}

// checkGardener is FindQuietGardeners for one plan and its gardener g.
func checkGardener(ctx context.Context, client BeadClient, land string, p QuietPlanRef, g *beadsapi.BeadDetail, now time.Time) (QuietGardener, bool, error) {
	var ready []*beadsapi.BeadDetail
	q := QuietGardener{Plan: p.ID, PlanTitle: p.Title, Trigger: g.ID, LastOutcome: quietFirstLine(g.Fields["last_outcome"])}
	if raw := strings.TrimSpace(g.Fields["last_run"]); raw != "" {
		t, err := time.Parse(time.RFC3339, raw)
		if err != nil {
			return q, false, nil
		}
		q.LastRun = t
	}
	q.Since = q.LastRun
	if q.Since.IsZero() {
		q.Since = g.CreatedAt
	}
	if q.Since.IsZero() {
		return q, false, nil
	}
	q.QuietFor = now.Sub(q.Since)
	switch {
	case slices.Contains(g.Labels, quietTimedLabel):
		q.Kind, q.Cron = QuietKindTimed, strings.TrimSpace(g.Fields["cron"])
	case slices.Contains(g.Labels, quietEventLabel):
		if q.QuietFor < QuietGardenerAfter {
			return q, false, nil
		}
		live, err := client.ListBeadsFiltered(ctx, beadsapi.ListBeadsQuery{
			Types: []string{"agent"}, Statuses: []string{"open", "in_progress", "blocked", "deferred"},
			Fields: map[string]string{"spawned_by": "trigger:" + g.ID}, Limit: 1,
		})
		if err != nil {
			return q, false, fmt.Errorf("quiet_gardeners: reading gardener %s's live cycle: %w", g.ID, err)
		}
		if slices.ContainsFunc(live.Beads, func(b *beadsapi.BeadDetail) bool { return b != nil && b.Status != "closed" }) {
			return q, false, nil
		}
		if q.ReadySteps, q.Ready, ready, err = readySteps(ctx, client, p.ID); err != nil {
			return q, false, err
		}
		if q.ReadySteps == 0 {
			return q, false, nil
		}
		q.Kind = QuietKindQuiet
		q.Nudgeable = hasNudgeEntry(g.Fields["triggers"])
	default:
		// Neither mode label: gardenermode.Detect calls it ambiguous, and
		// the gardener-less list or a person sees to it.
		return q, false, nil
	}
	asked, err := stewardDecisions(ctx, client, land, g.ID, now)
	if err != nil {
		return q, false, err
	}
	q.Asked = asked
	for _, d := range asked {
		if q.Kind == QuietKindTimed || d.CreatedAt.After(q.Since) {
			q.Raised = d.ID
		}
	}
	if q.Kind == QuietKindQuiet && q.Raised == "" {
		if q.Nudges, err = stewardNudges(ctx, client, land, p.ID, now); err != nil {
			return q, false, err
		}
		for _, at := range q.Nudges {
			if at.After(q.Since) {
				q.Nudged = at
			}
		}
		if !q.Nudged.IsZero() {
			q.NudgedFor = now.Sub(q.Nudged)
		}
		if q.NudgeCapped() {
			first, capAt := q.Nudges[0], q.Nudges[QuietNudgeCap-1]
			for _, b := range ready {
				if b.CreatedAt.Before(first) && len(q.Passed) < quietReadyShown {
					q.Passed = append(q.Passed, b.ID)
				}
			}
			// One decision per capped run of spells: any the steward filed
			// after the nudge that reached the cap raises it.
			for _, d := range asked {
				if d.CreatedAt.After(capAt) {
					q.Raised = d.ID
				}
			}
		}
	}
	return q, true, nil
}

// hasNudgeEntry reports whether a triggers[] field carries a QuietNudgeLabel
// entry. An unreadable field has none.
func hasNudgeEntry(raw string) bool {
	type entry struct {
		Label string `json:"label"`
	}
	var entries []entry
	if err := json.Unmarshal([]byte(raw), &entries); err != nil {
		return false
	}
	return slices.ContainsFunc(entries, func(e entry) bool { return e.Label == QuietNudgeLabel })
}

// stewardNudges are a steward run of land's nudges on plan in the last
// QuietGardenerAskWindow, oldest first: comments on the plan by a
// StewardRunPrefix author carrying QuietNudgeMarker.
func stewardNudges(ctx context.Context, client BeadClient, land, plan string, now time.Time) ([]time.Time, error) {
	cc, ok := client.(CommentClient)
	if !ok {
		return nil, fmt.Errorf("quiet_gardeners: the daemon client cannot read comments")
	}
	comments, err := cc.GetComments(ctx, plan)
	if err != nil {
		return nil, fmt.Errorf("quiet_gardeners: reading plan %s's comments: %w", plan, err)
	}
	run := StewardRunPrefix(land)
	var out []time.Time
	for _, c := range comments {
		if strings.Contains(c.Author, run) && strings.Contains(c.Text, QuietNudgeMarker) &&
			c.CreatedAt.After(now.Add(-QuietGardenerAskWindow)) && !c.CreatedAt.After(now) {
			out = append(out, c.CreatedAt)
		}
	}
	slices.SortFunc(out, func(a, b time.Time) int { return a.Compare(b) })
	return out, nil
}

// readySteps counts the plan's ready steps and names the first few; ready
// are all of them, by id.
func readySteps(ctx context.Context, client BeadClient, plan string) (n int, shown []string, ready []*beadsapi.BeadDetail, err error) {
	res, err := client.ListBeadsFiltered(ctx, beadsapi.ListBeadsQuery{
		Types: QuietReadyTypes, Parent: plan, Statuses: []string{"open"},
		NoOpenDeps: true, Unassigned: true, Limit: quietReadyScan,
		WithBlockedOnDecision: true, WithParkedOnDecision: true, // opt-in projections (gd-V4QAFgnPA8)
	})
	if err != nil {
		return 0, nil, nil, fmt.Errorf("quiet_gardeners: reading plan %s's ready steps: %w", plan, err)
	}
	for _, b := range res.Beads {
		if b == nil || b.Status != "open" || strings.TrimSpace(b.Assignee) != "" || b.BlockedOnDecision || b.ParkedOnDecision ||
			!slices.Contains(QuietReadyTypes, b.Type) || b.HumanOnly() {
			continue
		}
		ready = append(ready, b)
	}
	slices.SortFunc(ready, func(a, b *beadsapi.BeadDetail) int { return strings.Compare(a.ID, b.ID) })
	for _, b := range ready {
		if len(shown) < quietReadyShown {
			shown = append(shown, b.ID)
		}
	}
	return len(ready), shown, ready, nil
}

// stewardDecisions are the decisions a steward run of land filed about
// trigger in the last QuietGardenerAskWindow, oldest first.
func stewardDecisions(ctx context.Context, client BeadClient, land, trigger string, now time.Time) ([]QuietDecision, error) {
	res, err := client.ListBeadsFiltered(ctx, beadsapi.ListBeadsQuery{
		Types:        []string{"decision"},
		Statuses:     []string{"open", "in_progress", "blocked", "deferred", "closed"},
		Fields:       map[string]string{"subject_trigger_id": trigger},
		CreatedAfter: now.Add(-QuietGardenerAskWindow),
		Limit:        quietReadyScan,
	})
	if err != nil {
		return nil, fmt.Errorf("quiet_gardeners: reading the steward's decisions about %s: %w", trigger, err)
	}
	run := StewardRunPrefix(land)
	var out []QuietDecision
	for _, b := range res.Beads {
		if b == nil || b.Fields["subject_trigger_id"] != trigger || !strings.Contains(b.CreatedBy, run) ||
			b.CreatedAt.Before(now.Add(-QuietGardenerAskWindow)) {
			continue
		}
		out = append(out, QuietDecision{ID: b.ID, CreatedAt: b.CreatedAt, Status: b.Status})
	}
	slices.SortFunc(out, func(a, b QuietDecision) int { return a.CreatedAt.Compare(b.CreatedAt) })
	return out, nil
}

// quietFirstLine is s's first non-empty line, spaces collapsed, cut.
func quietFirstLine(s string) string {
	for _, l := range strings.Split(s, "\n") {
		if l = strings.Join(strings.Fields(l), " "); l != "" {
			if r := []rune(l); len(r) > quietOutcomeChars {
				l = string(r[:quietOutcomeChars-1]) + "…"
			}
			return l
		}
	}
	return ""
}

// quietMemoTTL is how long a quiet_gardeners count is reused. A cron
// preflight is evaluated on every engine tick for the rest of its hour while
// it does not hold (the cron-due cooldown bypass), so without it a two-hourly
// wake would run the check every tick for half the day. Two hours is the
// threshold, so ten minutes late is nothing.
const quietMemoTTL = 10 * time.Minute

type quietMemoEntry struct {
	at    time.Time
	count int
}

// quietMemoKey is one daemon client's count for one land. A client whose
// type cannot be a map key is never memoized.
type quietMemoKey struct {
	client BeadClient
	land   string
}

var (
	quietMemoMu sync.Mutex
	quietMemo   = map[quietMemoKey]quietMemoEntry{}
	// quietNow is the leaf's clock (a test seam).
	quietNow = time.Now
)

// evalQuietGardeners is the quiet_gardeners leaf: the number of building
// plans of its project (or the env's default) whose gardener is quiet or
// timed and not already raised by a steward decision or a nudge less than
// QuietGardenerAfter old, plus the gardener-less plans the steward would
// rebind and has not seen to (countGardenerless), compared with op and
// value (e.g. "> 0"). A read error is returned, so the caller suppresses the
// fire (cronPreflightHolds fails closed), and an error also survives a not():
// a watch that cannot see wakes nobody.
func evalQuietGardeners(ctx context.Context, client BeadClient, l Leaf, defaultProject string) (bool, error) {
	land := l.Project
	if land == "" {
		land = defaultProject
	}
	if land == "" {
		return false, fmt.Errorf("quiet_gardeners: no project")
	}
	now := quietNow()
	key := quietMemoKey{client: client, land: land}
	memo := client != nil && reflect.TypeOf(client).Comparable()
	if memo {
		quietMemoMu.Lock()
		m, ok := quietMemo[key]
		quietMemoMu.Unlock()
		if ok && now.Sub(m.at) < quietMemoTTL && !now.Before(m.at) {
			return CompareCount(m.count, l.Op, l.Value), nil
		}
	}
	count, err := CountQuietGardeners(ctx, client, land, now)
	if err != nil {
		return false, err
	}
	if memo {
		quietMemoMu.Lock()
		for k, m := range quietMemo {
			if now.Sub(m.at) >= quietMemoTTL {
				delete(quietMemo, k)
			}
		}
		quietMemo[key] = quietMemoEntry{at: now, count: count}
		quietMemoMu.Unlock()
	}
	return CompareCount(count, l.Op, l.Value), nil
}

// CountQuietGardeners is the leaf's count, read fresh: land's building plans
// whose gardener is quiet or timed and that no steward decision already
// raises, nor a nudge the gardener has had QuietGardenerAfter to answer, and
// those with no live gardener that countGardenerless counts.
func CountQuietGardeners(ctx context.Context, client BeadClient, land string, now time.Time) (int, error) {
	plans, err := ListQuietPlans(ctx, client, land)
	if err != nil {
		return 0, err
	}
	found, err := FindQuietGardeners(ctx, client, land, plans, now)
	if err != nil {
		return 0, err
	}
	n, err := countGardenerless(ctx, client, land, plans, now)
	if err != nil {
		return 0, err
	}
	for _, q := range found {
		if q.Wakes() {
			n++
		}
	}
	return n, nil
}
