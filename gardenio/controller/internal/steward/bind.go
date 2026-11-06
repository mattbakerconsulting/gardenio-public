package steward

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"hash/fnv"
	"slices"
	"strings"
	"time"

	"gardenio/controller/internal/beadsapi"
	"gardenio/controller/internal/bridge"
	"gardenio/controller/internal/conversation"
	"gardenio/controller/internal/lands/purse"
	"gardenio/controller/internal/lands/wall"
	"gardenio/controller/internal/lands/watcher"
	"gardenio/controller/internal/plangardener"
	"gardenio/controller/internal/scheduler"
	"gardenio/controller/internal/scheduler/conditions"
	"gardenio/controller/internal/steward/gardenerwatch"
)

// The land-bound gardener (spec §4.3, step 9.2). A steward is a gardener
// trigger built the way plangardener.Bind builds a plan's, with three
// differences: it is bound to a land by the gardener-land:<land> label (and
// carries the land's project: label, so its beads are inside the land's
// scope); it is woken by the land's events rather than a plan's subtree, plus
// a daily cron for the digest, under a runs-per-day cap; and its prompt is the
// land playbook, re-rendered with the land's live state at every wake
// (TriggerPromptRenderer).

// MarkerLabel tags a trigger bead as a land's steward, as
// plangardener.MarkerLabel tags a plan's gardener. trigger-engine keys the
// fire-time prompt renderer on it.
const MarkerLabel = "steward"

// LandLabelPrefix is the binding label's prefix: gardener-land:<land>.
const LandLabelPrefix = "gardener-land:"

// LandLabel is the label that binds a steward trigger to its land.
func LandLabel(land string) string { return LandLabelPrefix + land }

const (
	// DefaultMaxRunsPerDay is the steward's daily run cap (spec §4.3).
	DefaultMaxRunsPerDay = 24
	// runBudget is the run budget the bounded-stop guard requires of every
	// gardener. A steward is standing, so the engine never spends it down;
	// the daily cap is what bounds it.
	runBudget = DefaultMaxRunsPerDay
	// DefaultDigestHour is the local hour of the daily digest wake.
	DefaultDigestHour = 8
	// DefaultStepEstimateCredits is what one gardener step is estimated to
	// cost before the land has EstimateSessions settled gardener sessions to
	// take the mean of (StepEstimate, step 9.5).
	DefaultStepEstimateCredits = 5.0
	// title is the trigger's title. The engine names each run
	// <land>-<slug of title>-<suffix>, and the slug of "Steward (<land>)" is
	// "steward" (a trailing parenthetical is dropped), so every run is
	// <land>-steward-<suffix>: the decision wake matches on that.
	titleFormat = "Steward (%s)"
)

// Wake entry labels: one per §4.3 wake event, plus the digest cron.
const (
	WakeGeneral          = "general-message"
	WakeMention          = "steward-mention"
	WakeDecision         = "decision-resolved"
	WakeCreditGrant      = "credit-grant"
	WakePurseThreshold   = "purse-threshold"
	WakeGardenerFinished = "gardener-finished"
	WakeMachines         = "machines-delta"
	WakeNotes            = "notes-edited"
	WakeOpened           = "land-opened"
	WakePersonJoined     = "person-joined"
	WakeDigest           = "daily-digest"
	// WakeQuietGardeners is the home steward's watch on its plans'
	// gardeners (gd-bo2RUmtpHb, gardenerwatch.Entry).
	WakeQuietGardeners = gardenerwatch.WakeLabel
)

// MachinesChannel is the name of the land's channel for machine traffic
// (amendment 2026-10-23 §2; the name is owner question 1).
const MachinesChannel = "machines"

// NotesTitle is the title of the land's steward memory bead (step 9.2a).
func NotesTitle(land string) string { return "steward-notes:" + land }

// NotesHighWaterField is the notes bead field holding the #machines
// high-water mark (RFC 3339): the newest message the steward has read. The
// steward writes it at the end of a run (step 9.2a); until it does, the
// delta starts at the previous run.
const NotesHighWaterField = "machines_high_water"

const (
	// machinesLines caps the #machines delta in the prompt (amendment §6: a
	// run under 30k input tokens).
	machinesLines = 40
	// machinesLineChars caps one #machines line.
	machinesLineChars = 200
	// machinesDefaultWindow is how far back the delta reaches with no mark
	// and no previous run.
	machinesDefaultWindow = 24 * time.Hour
)

// Channels are the land's channel beads the steward wakes on. Machines is ""
// when the land has no #machines yet.
type Channels struct {
	General  string
	Machines string
}

// Mention is what a person writes to call the steward from any channel.
const Mention = "@" + Role

// WakeTriggers is the steward's triggers[] for a land whose #general is the
// channel bead general. Every event entry is fenced to the land by its
// project label, so an event in another land never wakes it, and the event
// entries are disjoint, so one event wakes it once:
//
//   - a human message in #general (a Commons author is oidc:<email>; agents,
//     the purse watcher among them, are agent:<name>);
//   - an @steward mention by a human in any other channel of the land (one in
//     #general is already a #general message);
//   - a decision a steward run filed, resolved (closed). Runs are named
//     <land>-steward-<suffix>; kbeads publishes no decision-resolved event,
//     so this matches the decision bead's close;
//   - a credit grant (a credit_grant bead is born closed);
//   - a purse threshold notice the purse watcher posts in #general (the
//     refill post is left to the grant);
//   - a lands-gardener of the land finished (its agent bead closed);
//   - machine traffic in #machines, the steward's own posts and the
//     provisioner's wake aside (a person there wakes it by mention),
//     coalesced: a burst settles for 10 minutes and the entry fires at most
//     every 30, since the run reads the delta;
//   - a person (an OIDC actor, not the steward's own write) editing the
//     steward-notes:<land> bead, the tenant's Holds;
//   - the provisioner's one wake when the land opens (ProvisionHook.Wake), so the
//     first run greets #general without waiting for a person;
//   - the provisioner's wake when a person joins the land (ProvisionHook.Greet,
//     step 9.14), so the steward greets them by name in #general;
//   - the daily digest, a cron at hour:minute in tz.
func WakeTriggers(land string, ch Channels, digestCron, tz string) []scheduler.TriggerEntry {
	return wakeTriggers(land, ch, digestCron, tz, false)
}

// HomeWakeTriggers is the home garden's steward's triggers[] (step 9.13). The
// home garden is the fleet's own land, so it keeps WakeTriggers' entries for
// what is the steward's alone: an @steward mention by a human, in any channel
// with #general among them, a decision a steward run filed, a person editing
// its notes, the land-opened and person-joined wakes and the digest. It has no wake on every
// #general message, where the fleet's @gardenio agents answer side by side
// (decision gd-QaoNbHRJPC), none on #machines, where the whole fleet talks
// (the run still reads the delta), and none on a purse or a lands-gardener:
// the home garden has neither. It adds one of its own, the quiet-gardeners
// watch (gardenerwatch.Entry): its plans are run by their own gardeners, and
// one that goes quiet while its plan has work ready is the steward's to see.
func HomeWakeTriggers(land string, ch Channels, digestCron, tz string) []scheduler.TriggerEntry {
	return wakeTriggers(land, ch, digestCron, tz, true)
}

func wakeTriggers(land string, ch Channels, digestCron, tz string, home bool) []scheduler.TriggerEntry {
	general := ch.General
	inLand := field("label:project:"+land, "present", "")
	message := func(extra ...conditions.Expression) *conditions.Expression {
		return all(append([]conditions.Expression{inLand, field("type", "==", "message")}, extra...)...)
	}
	human := field("author", "contains", "oidc:")
	decision := scheduler.TriggerEntry{Kind: scheduler.TriggerKindBeadEvent, Label: WakeDecision, Events: []string{"closed"},
		Match: all(inLand, field("type", "==", "decision"), field("created_by", "contains", land+"-"+Role+"-"))}
	tail := []scheduler.TriggerEntry{
		{Kind: scheduler.TriggerKindBeadEvent, Label: WakeNotes, Events: []string{"updated"},
			Match: all(inLand, field("title", "==", NotesTitle(land)), field("actor", "contains", "@"))},
		{Kind: scheduler.TriggerKindBeadEvent, Label: WakeOpened, Events: []string{"created"},
			Match: message(field("author", "==", OpenedAuthor), field("origin", "==", OriginLandOpened))},
		personJoinedEntry(land),
		{Kind: scheduler.TriggerKindCron, Label: WakeDigest, Cron: digestCron, Timezone: tz},
	}
	if home {
		mention := scheduler.TriggerEntry{Kind: scheduler.TriggerKindBeadEvent, Label: WakeMention, Events: []string{"created"},
			Match: message(field("text", "contains", Mention), human)}
		// The watch goes before person-joined, where UpgradeHomeWakes puts
		// it on a steward bound before it.
		tail = slices.Insert(tail, 2, gardenerwatch.Entry(land, tz))
		return append([]scheduler.TriggerEntry{mention, decision}, tail...)
	}
	entries := []scheduler.TriggerEntry{
		{Kind: scheduler.TriggerKindBeadEvent, Label: WakeGeneral, Events: []string{"created"},
			Match: message(field("channel_id", "==", general), human)},
		{Kind: scheduler.TriggerKindBeadEvent, Label: WakeMention, Events: []string{"created"},
			Match: message(field("channel_id", "!=", general), field("text", "contains", Mention), human)},
		decision,
		{Kind: scheduler.TriggerKindBeadEvent, Label: WakeCreditGrant, Events: []string{"created"},
			Match: all(inLand, field("type", "==", "credit_grant"))},
		{Kind: scheduler.TriggerKindBeadEvent, Label: WakePurseThreshold, Events: []string{"created"},
			Match: message(field("author", "==", watcher.Author), field("origin", "==", watcher.OriginNotice))},
		{Kind: scheduler.TriggerKindBeadEvent, Label: WakeGardenerFinished, Events: []string{"closed"},
			Match: all(inLand, field("type", "==", "agent"), field("label:role:"+GardenerRole, "present", ""))},
	}
	if ch.Machines != "" {
		entries = append(entries, scheduler.TriggerEntry{
			Kind: scheduler.TriggerKindBeadEvent, Label: WakeMachines, Events: []string{"created"},
			Match: message(field("channel_id", "==", ch.Machines),
				not(human), not(field("author", "contains", land+"-"+Role+"-")), not(field("author", "==", watcher.Author)),
				not(field("origin", "==", OriginLandOpened)), notJoined),
			Debounce: "10m", Throttle: "30m",
		})
	}
	return append(entries, tail...)
}

// notJoined keeps the person-joined wake post out of the #machines delta: its
// own entry wakes the steward for it.
var notJoined = not(field("origin", "==", OriginPersonJoined))

// personJoinedEntry is the wake entry for the provisioner's person-joined post
// in land (ProvisionHook.Greet).
func personJoinedEntry(land string) scheduler.TriggerEntry {
	return scheduler.TriggerEntry{Kind: scheduler.TriggerKindBeadEvent, Label: WakePersonJoined, Events: []string{"created"},
		Match: all(field("label:project:"+land, "present", ""), field("type", "==", "message"),
			field("author", "==", OpenedAuthor), field("origin", "==", OriginPersonJoined))}
}

// UpgradeWakes is entries, a bound steward's triggers[], with what a fresh
// bind has since gained: the person-joined entry before the digest (or at the
// end), and the #machines delta keeping its post out. It reports whether it
// changed anything; entries already up to date come back as they are. No
// entries (a steward gardenermode switched to timed mode has triggers="")
// come back unchanged: there is no bind to bring up to date, and writing the
// entry alone would put the steward back in event mode.
func UpgradeWakes(land string, entries []scheduler.TriggerEntry) ([]scheduler.TriggerEntry, bool) {
	if len(entries) == 0 {
		return entries, false
	}
	out := slices.Clone(entries)
	changed := false
	if !slices.ContainsFunc(out, func(e scheduler.TriggerEntry) bool { return e.Label == WakePersonJoined }) {
		at := slices.IndexFunc(out, func(e scheduler.TriggerEntry) bool { return e.Label == WakeDigest })
		if at < 0 {
			at = len(out)
		}
		out = slices.Insert(out, at, personJoinedEntry(land))
		changed = true
	}
	for i, e := range out {
		if e.Label == WakeMachines && e.Match != nil && !excludes(*e.Match, OriginPersonJoined) {
			// A bound #machines entry is an all-of match (wakeTriggers).
			m := *e.Match
			m.All = append(slices.Clone(m.All), notJoined)
			out[i].Match = &m
			changed = true
		}
	}
	return out, changed
}

// UpgradeHomeWakes is UpgradeWakes for the home garden's steward, which also
// gains the quiet-gardeners watch (gd-bo2RUmtpHb) before its person-joined
// entry (or its digest, or at the end), in the digest's zone. Like
// UpgradeWakes it leaves a steward with no entries alone and reports whether
// it changed anything. gb lands steward gardenio --bind runs it.
func UpgradeHomeWakes(land string, entries []scheduler.TriggerEntry) ([]scheduler.TriggerEntry, bool) {
	out, changed := UpgradeWakes(land, entries)
	if len(out) == 0 || slices.ContainsFunc(out, func(e scheduler.TriggerEntry) bool { return e.Label == WakeQuietGardeners }) {
		return out, changed
	}
	tz := ""
	at := len(out)
	for i, e := range out {
		switch e.Label {
		case WakePersonJoined:
			at = min(at, i)
		case WakeDigest:
			tz, at = e.Timezone, min(at, i)
		}
	}
	return slices.Insert(slices.Clone(out), at, gardenerwatch.Entry(land, tz)), true
}

// excludes reports whether e, an all-of match, already refuses origin.
func excludes(e conditions.Expression, origin string) bool {
	return slices.ContainsFunc(e.All, func(c conditions.Expression) bool {
		return c.Not != nil && c.Not.Field == "origin" && c.Not.Op == "==" && c.Not.Value == origin
	})
}

func not(e conditions.Expression) conditions.Expression {
	return conditions.Expression{Not: &e}
}

func field(name, op, value string) conditions.Expression {
	return conditions.Expression{Leaf: conditions.Leaf{Field: name, Op: op, Value: value}}
}

func all(e ...conditions.Expression) *conditions.Expression {
	return &conditions.Expression{All: e}
}

// DigestCron is the daily digest's cron: hour local, at a minute seeded from
// the land's name so lands do not all wake at once.
func DigestCron(land string, hour int) string {
	h := fnv.New32a()
	_, _ = h.Write([]byte(land))
	return fmt.Sprintf("%d %d * * *", h.Sum32()%60, hour)
}

// LandState is a land as Bind and the fire-time renderer read it: the
// playbook's Land and the land's channel beads.
type LandState struct {
	Land     Land
	Channels Channels
	// Owner is the home garden's owner (HomeOwner), "" for a tenant land.
	Owner string
}

// landMembers is the optional members read (*beadsapi.Client has it).
type landMembers interface {
	LandMembers(ctx context.Context, land string) ([]beadsapi.LandMember, error)
}

// LoadLand reads a tenant land's, or the home garden's, live state for the
// playbook. The purse is told in credits only, and a purse that cannot be read
// is rendered unknown rather than failing the read; the home garden's is
// unmetered (ReadPurse). Members are read when src can (the land's members
// API), with the land role their last sign-in recorded; the home garden's
// owner is always among them. prevRun is when the previous steward run started
// (zero if unknown): the #machines delta starts at the notes bead's high-water
// mark, else there, else a day back.
func LoadLand(ctx context.Context, src purse.Source, land string, now, prevRun time.Time) (LandState, error) {
	proj, tenant, err := purse.Project(ctx, src, land)
	if err != nil {
		return LandState{}, err
	}
	owner := ""
	if !tenant {
		if owner = HomeOwner(proj); owner == "" {
			return LandState{}, fmt.Errorf("%s is an operator land that is not the home garden; a steward is bound to tenant lands and the home garden only", land)
		}
	}
	repo := repoOf(proj.Fields["git_url"])
	if repo == "" {
		return LandState{}, fmt.Errorf("land %s has no GitHub repo (git_url %q)", land, proj.Fields["git_url"])
	}
	p := landPurse(ctx, src, land, proj, tenant, now)
	// A gardener history that cannot be read leaves the default estimate:
	// the spawn gate still refuses what the purse cannot cover. An unmetered
	// land has no estimate: nothing is weighed against its purse.
	var est Estimate
	if !p.IsUnmetered() {
		est, _ = StepEstimate(ctx, src, land, p.LLMMultiplier)
	}
	l := Land{
		Name:                  land,
		Repo:                  repo,
		Home:                  owner != "",
		Gatehouse:             tenant && proj.Fields[GatehouseField] == "true",
		GatehouseTools:        tenant && proj.Fields[GatehouseField] == "true" && proj.Fields[GatehouseToolsField] == "true",
		Purse:                 p.CreditsOnly(),
		StepEstimateCredits:   est.Credits,
		MaxLiveAgents:         wall.MaxLiveAgents(proj.Fields),
		Authority:             ParseAuthority(proj.Fields[AuthorityField]),
		StewardApprovesMerges: ParseStewardApprovesMerges(proj.Fields[StewardApprovesField]),
	}
	plans, err := src.ListBeadsFiltered(ctx, beadsapi.ListBeadsQuery{
		Types:    []string{"epic"},
		Labels:   []string{"project:" + land, plangardener.PlanLabel},
		Statuses: []string{"open", "in_progress", "blocked"},
	})
	if err != nil {
		return LandState{}, fmt.Errorf("reading %s's open plans: %w", land, err)
	}
	for _, b := range plans.Beads {
		if b != nil {
			l.OpenPlans = append(l.OpenPlans, Plan{ID: b.ID, Title: b.Title, State: b.Fields["plan_state"]})
		}
	}
	if l.Home {
		if err := homePlans(ctx, src, &l, now); err != nil {
			return LandState{}, err
		}
	}
	if l.ParkedSteps, err = parkedSteps(ctx, src, land); err != nil {
		return LandState{}, err
	}
	if l.Live, err = liveAgents(ctx, src, land, now); err != nil {
		return LandState{}, err
	}
	l.Members = landMembersOf(ctx, src, land, proj, owner)
	chans := Channels{General: strings.TrimSpace(proj.Fields["land_default_channel"])}
	if chans.Machines, err = channelNamed(ctx, src, land, MachinesChannel); err != nil {
		return LandState{}, err
	}
	notes, err := notesBead(ctx, src, land)
	if err != nil {
		return LandState{}, err
	}
	var mark time.Time
	if notes != nil {
		l.NotesID, l.Notes = notes.ID, NotesBody(notes)
		mark, _ = time.Parse(time.RFC3339, strings.TrimSpace(notes.Fields[NotesHighWaterField]))
		if !mark.IsZero() {
			l.MachinesMark = mark.UTC().Format(time.RFC3339Nano)
		}
	}
	if chans.Machines != "" {
		l.HasMachines = true
		since := mark
		if since.IsZero() {
			since = prevRun
		}
		if since.IsZero() {
			since = now.Add(-machinesDefaultWindow)
		}
		if l.Machines, l.MachinesMore, err = machinesSince(ctx, src, land, chans.Machines, since); err != nil {
			return LandState{}, err
		}
		if n := len(l.Machines); n > 0 {
			l.MachinesMark = l.Machines[n-1].At.UTC().Format(time.RFC3339Nano)
		}
	}
	return LandState{Land: l, Channels: chans, Owner: owner}, nil
}

// homePlans reads the home garden's two plan watches (gardenerwatch) into l:
// the building plans with no live gardener, and those whose gardener is
// quiet or still timed. A quiet watch that cannot be read is said in the
// prompt rather than failing the render, which would fall back to the
// bind-time prompt.
func homePlans(ctx context.Context, src purse.Source, l *Land, now time.Time) error {
	var refs []conditions.QuietPlanRef
	for _, p := range l.OpenPlans {
		if p.State == "building" {
			refs = append(refs, conditions.QuietPlanRef{ID: p.ID, Title: p.Title})
		}
	}
	var err error
	if l.Stalled, l.StalledMore, err = gardenerwatch.Stalled(ctx, src, refs); err != nil {
		return err
	}
	if l.Quiet, l.QuietMore, err = gardenerwatch.Find(ctx, src, l.Name, refs, now); err != nil {
		l.Quiet, l.QuietMore, l.QuietUnread = nil, 0, gardenerwatch.ErrorLine(err)
	}
	return nil
}

// landMembersOf is the land's members as LoadLand reads them: the land's
// members API when src has one (a read error leaves none), with the home
// garden's owner always among them, first.
func landMembersOf(ctx context.Context, src purse.Source, land string, proj *beadsapi.BeadDetail, owner string) []Member {
	var members []Member
	if m, ok := src.(landMembers); ok {
		if people, err := m.LandMembers(ctx, land); err == nil {
			for _, p := range people {
				members = append(members, Member{Name: firstNonEmpty(p.Name, p.Principal), Identity: p.Principal, Role: landRole(p.Role)})
			}
		}
	}
	if owner != "" && !slices.ContainsFunc(members, func(m Member) bool { return m.Identity == owner }) {
		name := firstNonEmpty(strings.TrimSpace(proj.OperatedByDisplay), strings.TrimPrefix(owner, "oidc:"))
		members = append([]Member{{Name: name, Identity: owner, Role: conversation.LandRoleOwner}}, members...)
	}
	return members
}

// IsHome reports whether land is the home garden (HomeOwner): an operator
// land whose operator is a person.
func IsHome(ctx context.Context, src purse.Source, land string) (bool, error) {
	proj, tenant, err := purse.Project(ctx, src, land)
	if err != nil {
		return false, err
	}
	return !tenant && HomeOwner(proj) != "", nil
}

// ReadPurse is a land's purse as its steward reads it: a tenant land's purse
// (unknown when it cannot be read), the home garden's unmetered one, and
// unknown for any other land.
func ReadPurse(ctx context.Context, src purse.Source, land string, now time.Time) purse.Purse {
	proj, tenant, err := purse.Project(ctx, src, land)
	if err != nil {
		return purse.Unreadable(land, err, now)
	}
	return landPurse(ctx, src, land, proj, tenant, now)
}

// landPurse is ReadPurse for a land whose project bead is read.
func landPurse(ctx context.Context, src purse.Source, land string, proj *beadsapi.BeadDetail, tenant bool, now time.Time) purse.Purse {
	if !tenant {
		if HomeOwner(proj) != "" {
			return purse.Unmetered(land, now)
		}
		return purse.Unreadable(land, fmt.Errorf("land %q: %w", land, purse.ErrOperatorLand), now)
	}
	p, err := purse.Read(ctx, src, land, now)
	if err != nil {
		return purse.Unreadable(land, err, now)
	}
	return p
}

// landRole reads a member's recorded role. Anything but the three tiers a
// sign-in records is none, said as "role not recorded", never guessed up.
func landRole(r string) conversation.LandRole {
	switch lr := conversation.LandRole(r); lr {
	case conversation.LandRoleOwner, conversation.LandRoleMember, conversation.LandRoleGuest:
		return lr
	}
	return conversation.LandRoleNone
}

var liveStatuses = []string{"open", "in_progress", "blocked", "deferred"}

// channelNamed is the land's live channel bead with that name, or "".
func channelNamed(ctx context.Context, src purse.Source, land, name string) (string, error) {
	res, err := src.ListBeadsFiltered(ctx, beadsapi.ListBeadsQuery{
		Types: []string{"channel"}, Labels: []string{"project:" + land}, Statuses: liveStatuses,
	})
	if err != nil {
		return "", fmt.Errorf("reading %s's channels: %w", land, err)
	}
	for _, b := range res.Beads {
		if b != nil && b.Fields["name"] == name {
			return b.ID, nil
		}
	}
	return "", nil
}

// notesBead is the land's steward-notes:<land> bead, or nil.
func notesBead(ctx context.Context, src purse.Source, land string) (*beadsapi.BeadDetail, error) {
	title := NotesTitle(land)
	res, err := src.ListBeadsFiltered(ctx, beadsapi.ListBeadsQuery{
		Labels: []string{"project:" + land}, Statuses: liveStatuses, Search: title,
	})
	if err != nil {
		return nil, fmt.Errorf("reading %s: %w", title, err)
	}
	for _, b := range res.Beads {
		if b != nil && b.Title == title {
			return b, nil
		}
	}
	return nil, nil
}

// machinesSince is #machines after since, oldest first, cut to the newest
// machinesLines; more is how many older ones were cut.
func machinesSince(ctx context.Context, src purse.Source, land, channel string, since time.Time) (out []MachineMessage, more int, err error) {
	res, err := src.ListBeadsFiltered(ctx, beadsapi.ListBeadsQuery{
		Types: []string{"message"}, Labels: []string{"project:" + land},
		Fields: map[string]string{"channel_id": channel}, CreatedAfter: since, Sort: "-created_at", Limit: 500,
	})
	if err != nil {
		return nil, 0, fmt.Errorf("reading %s's #%s: %w", land, MachinesChannel, err)
	}
	for _, b := range res.Beads {
		if b == nil || b.Fields["channel_id"] != channel || !b.CreatedAt.After(since) {
			continue
		}
		text := firstNonEmpty(strings.TrimSpace(b.Fields["text"]), strings.TrimSpace(b.Title))
		text = strings.Join(strings.Fields(text), " ")
		if r := []rune(text); len(r) > machinesLineChars {
			text = string(r[:machinesLineChars-1]) + "…"
		}
		out = append(out, MachineMessage{At: b.CreatedAt, Author: firstNonEmpty(b.Fields["author"], b.CreatedBy), Text: text})
	}
	slices.SortStableFunc(out, func(a, b MachineMessage) int { return a.At.Compare(b.At) })
	if len(out) > machinesLines {
		more = len(out) - machinesLines
		out = out[more:]
	}
	return out, more, nil
}

// repoOf is owner/name from a GitHub git URL.
func repoOf(gitURL string) string {
	r := strings.TrimSpace(gitURL)
	r = strings.TrimPrefix(r, "https://github.com/")
	r = strings.TrimSuffix(r, ".git")
	if r == gitURL || strings.Count(r, "/") != 1 {
		return ""
	}
	return r
}

// BindSpec parameterizes Bind. Zero values take the steward defaults.
type BindSpec struct {
	// Operator is the operated-by alias attributed as the steward's owner
	// (plangardener.ResolveGardenerOwner).
	Operator string
	// CreatedBy is the actor recorded on the trigger bead.
	CreatedBy string
	// Timezone is the digest cron's zone (default plangardener.DefaultTimezone).
	Timezone string
	// DigestHour is the digest's local hour (default DefaultDigestHour).
	DigestHour int
	// MaxRunsPerDay overrides DefaultMaxRunsPerDay.
	MaxRunsPerDay int
	// Now is the clock for the purse read (default time.Now).
	Now func() time.Time
}

// BindResult reports what Bind did.
type BindResult struct {
	// ID is the steward trigger: created, or the land's existing one.
	ID string
	// Adopted is true when the land already had a steward; nothing was
	// written but a missing notes bead.
	Adopted bool
	// NotesID is the land's steward-notes:<land> bead; NotesCreated is true
	// when this bind made it (step 9.2a: the notes are born with the
	// steward, sections empty).
	NotesID      string
	NotesCreated bool
	// Triggers is the wake list of a fresh bind.
	Triggers []scheduler.TriggerEntry
}

// ErrNoGeneral: the land has no #general channel bead yet (provisioning step
// 4 makes it before the steward is bound).
var ErrNoGeneral = errors.New("land has no #general channel")

// Bind idempotently binds the one steward to a tenant land or the home
// garden: a type=trigger, trigger:multi gardener whose iterations spawn in the
// steward role. A land that already has a steward (open, enabled or paused)
// keeps it.
func Bind(ctx context.Context, c plangardener.Client, land string, spec BindSpec) (*BindResult, error) {
	land = strings.TrimSpace(land)
	if land == "" {
		return nil, errors.New("a land is required")
	}
	if existing, err := FindSteward(ctx, c, land); err != nil {
		return nil, err
	} else if existing != nil {
		notesID, created, err := EnsureNotes(ctx, c, land, spec.CreatedBy)
		if err != nil {
			return nil, err
		}
		return &BindResult{ID: existing.ID, Adopted: true, NotesID: notesID, NotesCreated: created}, nil
	}

	now := time.Now
	if spec.Now != nil {
		now = spec.Now
	}
	st, err := LoadLand(ctx, c, land, now(), time.Time{})
	if err != nil {
		return nil, err
	}
	if st.Channels.General == "" {
		return nil, fmt.Errorf("binding a steward to %s: %w", land, ErrNoGeneral)
	}
	notesCreated := false
	if st.Land.NotesID == "" {
		if st.Land.NotesID, notesCreated, err = EnsureNotes(ctx, c, land, spec.CreatedBy); err != nil {
			return nil, err
		}
		st.Land.Notes = strings.TrimSpace(EmptyNotes(land))
	}
	prompt, err := RenderPrompt(st.Land)
	if err != nil {
		return nil, err
	}

	tz := firstNonEmpty(strings.TrimSpace(spec.Timezone), plangardener.DefaultTimezone)
	hour := spec.DigestHour
	if hour <= 0 || hour > 23 {
		hour = DefaultDigestHour
	}
	perDay := spec.MaxRunsPerDay
	if perDay <= 0 {
		perDay = DefaultMaxRunsPerDay
	}
	wakes := WakeTriggers(land, st.Channels, DigestCron(land, hour), tz)
	if st.Land.Home {
		wakes = HomeWakeTriggers(land, st.Channels, DigestCron(land, hour), tz)
	}
	triggersJSON, err := json.Marshal(wakes)
	if err != nil {
		return nil, fmt.Errorf("marshalling the steward's triggers: %w", err)
	}

	operator := spec.Operator
	if operator == "" {
		// The home garden's steward belongs to its owner.
		operator = st.Owner
	}
	blob, err := gardenerBlob(ctx, c, land, operator, perDay)
	if err != nil {
		return nil, err
	}
	if v := bridge.ValidateGardenerBlob(blob); v != nil {
		return nil, fmt.Errorf("%s", v.Error())
	}
	labels := []string{"trigger:multi", "role:gardener", MarkerLabel, LandLabel(land), "project:" + land, "perpetual:true"}
	if v := bridge.ValidateGardenerStandingBlob(blob, labels); v != nil {
		return nil, fmt.Errorf("%s", v.Error())
	}

	fields, err := json.Marshal(map[string]any{
		"enabled":          true,
		"action":           "spawn_agent",
		"role":             Role,
		"prompt":           prompt,
		"triggers":         string(triggersJSON),
		"timezone":         tz,
		"project":          land,
		"cooldown_minutes": 5,
		"max_concurrent":   1,
		"gardener":         blob,
	})
	if err != nil {
		return nil, fmt.Errorf("marshalling the steward trigger's fields: %w", err)
	}
	id, err := c.CreateBead(ctx, beadsapi.CreateBeadRequest{
		Title:     fmt.Sprintf(titleFormat, land),
		Type:      "trigger",
		Kind:      "data",
		Labels:    labels,
		Fields:    json.RawMessage(fields),
		CreatedBy: spec.CreatedBy,
	})
	if err != nil {
		return nil, fmt.Errorf("creating the steward trigger for %s: %w", land, err)
	}
	return &BindResult{ID: id, Triggers: wakes, NotesID: st.Land.NotesID, NotesCreated: notesCreated}, nil
}

// gardenerBlob is the steward's gardener spec: standing (it looks after the
// land, it never finishes), one run at a time, at most perDay runs a day, and
// the bounded-stop guard's run budget and wall clock (advice ka-0000000974).
// The guard requires a bounded wall_clock; a standing gardener's is renewed in
// place, never closed (ka-0000004573).
func gardenerBlob(ctx context.Context, c plangardener.Client, land, operator string, perDay int) (string, error) {
	stop, err := json.Marshal(scheduler.StopSpec{
		HardStop: scheduler.StopSlot{Any: []scheduler.StopEntry{
			{Kind: scheduler.StopKindConsecutiveFailures, Value: "5"},
			{Kind: scheduler.StopKindWallClock, Duration: bridge.GardenerMaxWallClock.String()},
		}},
		OnLoopEnd: "close",
	})
	if err != nil {
		return "", fmt.Errorf("marshalling the steward's stop spec: %w", err)
	}
	g := bridge.GardenerSpec{
		Name:            fmt.Sprintf(titleFormat, land),
		TerminalGoal:    fmt.Sprintf("Standing steward of land %s: look after the land for its people while the land is open.", land),
		Standing:        true,
		Stop:            stop,
		RunBudget:       runBudget,
		MaxRuns:         runBudget,
		MaxRunsPerDay:   perDay,
		OneSpawnAtATime: true,
	}
	g.SlackUserID, g.UserBeadID = plangardener.ResolveGardenerOwner(ctx, c, operator)
	raw, err := json.Marshal(g)
	if err != nil {
		return "", fmt.Errorf("marshalling the steward's gardener spec: %w", err)
	}
	return string(raw), nil
}

// FindSteward is the land's steward trigger, enabled or paused, or nil.
func FindSteward(ctx context.Context, c purse.Source, land string) (*beadsapi.BeadDetail, error) {
	res, err := c.ListBeadsFiltered(ctx, beadsapi.ListBeadsQuery{
		Types:    []string{"trigger"},
		Labels:   []string{LandLabel(land)},
		Statuses: []string{"open", "in_progress", "blocked"},
	})
	if err != nil {
		return nil, fmt.Errorf("looking for %s's steward: %w", land, err)
	}
	for _, b := range res.Beads {
		if b != nil && slices.Contains(b.Labels, LandLabel(land)) && plangardener.IsGardenerBead(b) {
			return b, nil
		}
	}
	return nil, nil
}

// LastSteward is the land's steward trigger that closed last, or nil when it
// never had one closed. Each re-bind leaves one behind, so a land has few.
func LastSteward(ctx context.Context, c purse.Source, land string) (*beadsapi.BeadDetail, error) {
	res, err := c.ListBeadsFiltered(ctx, beadsapi.ListBeadsQuery{
		Types:    []string{"trigger"},
		Labels:   []string{LandLabel(land)},
		Statuses: []string{"closed"},
	})
	if err != nil {
		return nil, fmt.Errorf("looking for %s's last steward: %w", land, err)
	}
	var last *beadsapi.BeadDetail
	for _, b := range res.Beads {
		if b == nil || b.Status != "closed" || !slices.Contains(b.Labels, LandLabel(land)) || !plangardener.IsGardenerBead(b) {
			continue
		}
		if last == nil || closedAt(b).After(closedAt(last)) {
			last = b
		}
	}
	return last, nil
}

// closedAt is when b closed: closed_at, else its last update.
func closedAt(b *beadsapi.BeadDetail) time.Time {
	if t, err := time.Parse(time.RFC3339, strings.TrimSpace(b.ClosedAt)); err == nil {
		return t
	}
	return b.UpdatedAt
}

// TriggerPromptRenderer is trigger-engine's fire-time prompt for a steward
// trigger: the playbook rendered with the land's state at this wake, so the
// steward never reads a purse or plan list from the day it was bound, and with
// the message that woke it (ev, step 9.10). Any
// failure returns the error and the engine falls back to the stored prompt.
//
// It renders only a trigger that spawns the steward role. Labels are not
// spawn content (kbeads' spawn-lineage seal does not cover them), so a label
// alone must never swap another role's prompt for one carrying the land's
// tenant-written plan titles and names. For the same reason the label alone
// never picks the land: it must be the trigger's only gardener-land: label and
// name the trigger's project (sealed), so a gardener-land:B label on land A's
// steward cannot render B's notes, #machines and people into A's prompt.
func TriggerPromptRenderer(src purse.Source) bridge.PromptRenderer {
	return func(ctx context.Context, trig bridge.TriggerDef, ev bridge.FireEvent) (string, error) {
		if trig.Role != Role {
			return "", fmt.Errorf("trigger %s spawns role %q, not %s", trig.ID, trig.Role, Role)
		}
		land, err := boundLand(trig)
		if err != nil {
			return "", err
		}
		st, err := LoadLand(ctx, src, land, time.Now(), previousRun(ctx, src, trig))
		if err != nil {
			return "", err
		}
		st.Land.Wake = wakeMessage(ctx, src, land, st.Channels, ev)
		st.Land.Joined = wakeJoined(ctx, src, land, ev)
		st.Land.WakeEntry, st.Land.WakeBead = ev.Entry, ev.BeadID
		return RenderPrompt(st.Land)
	}
}

// boundLand is the land a steward trigger renders: its one gardener-land:
// label, which must match its project.
func boundLand(trig bridge.TriggerDef) (string, error) {
	var lands []string
	for _, l := range trig.Labels {
		if rest, ok := strings.CutPrefix(l, LandLabelPrefix); ok {
			lands = append(lands, rest)
		}
	}
	switch {
	case len(lands) == 0:
		return "", fmt.Errorf("trigger %s has no %s label", trig.ID, LandLabelPrefix+"<land>")
	case len(lands) > 1:
		return "", fmt.Errorf("trigger %s has %d %s labels, want one", trig.ID, len(lands), LandLabelPrefix+"<land>")
	case lands[0] != trig.Project:
		return "", fmt.Errorf("trigger %s is bound to land %q but runs in project %q", trig.ID, lands[0], trig.Project)
	}
	return lands[0], nil
}

// beadGetter is the optional single-bead read (*beadsapi.Client has it).
type beadGetter interface {
	GetBead(ctx context.Context, id string) (*beadsapi.BeadDetail, error)
}

// previousRun is when the steward's previous run started: its agent bead's
// creation (the gardener blob's last_agent_id), or zero.
func previousRun(ctx context.Context, src purse.Source, trig bridge.TriggerDef) time.Time {
	g, ok := src.(beadGetter)
	if !ok || trig.Gardener == nil || trig.Gardener.LastAgentID == "" {
		return time.Time{}
	}
	b, err := g.GetBead(ctx, trig.Gardener.LastAgentID)
	if err != nil || b == nil {
		return time.Time{}
	}
	return b.CreatedAt
}

func firstNonEmpty(values ...string) string {
	for _, v := range values {
		if v != "" {
			return v
		}
	}
	return ""
}
