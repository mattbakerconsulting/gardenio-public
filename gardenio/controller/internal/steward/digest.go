package steward

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"time"

	"gardenio/controller/internal/beadsapi"
	"gardenio/controller/internal/lands/purse"
	"gardenio/controller/internal/plangardener"
)

// The daily digest (spec §4.2 item 4, step 9.6; amendment 2026-10-23 §0 item
// 5 and §8). The digest is the steward's only scheduled post: the daily cron
// (WakeDigest) wakes it, gb lands digest gathers what changed in the land
// since the last digest, and the steward writes it for a person (what
// happened, what it means for them, what if anything they must do) and posts
// it in #general with gb lands digest --post, which records where the next
// digest starts. A quiet day, nothing done, planned, answered or run, posts
// nothing.
//
// Where the last digest stopped lives on the notes bead: digest_at, the
// as-of time of the last posted digest, and digest_used_credits, the land's
// used credits (spent plus in flight) at that time, so the credits spent
// since are a difference of two purse readings. Before the first digest the
// window is a day back and the spend since is not known.

// Notes bead fields recording the last posted digest.
const (
	DigestAtField   = "digest_at"
	DigestUsedField = "digest_used_credits"
)

const (
	// digestDefaultWindow is how far back the first digest looks.
	digestDefaultWindow = 24 * time.Hour
	// digestScan caps each list the digest reads; escalationScan the open
	// escalations, where the merges the desk holds crowd the owner's few.
	digestScan     = 200
	escalationScan = 1000
)

// stepTypes are the bead types a plan step can have.
var stepTypes = []string{"task", "bug", "feature"}

// DigestItem is one line of the digest.
type DigestItem struct {
	ID    string `json:"id"`
	Title string `json:"title"`
	// Who is the person or gardener the line is about: a step's gardener, the
	// person a decision waits on or who answered it.
	Who string `json:"who,omitempty"`
	// Note is a plan's plan_state, or a decision's chosen answer.
	Note string `json:"note,omitempty"`
	// New marks a waiting decision asked since the last digest.
	New bool `json:"new,omitempty"`
}

// Digest is what changed in a land since the last digest, and where it
// stands now.
type Digest struct {
	Land string `json:"land"`
	// Since is where this digest starts: the last digest's as-of time, or a
	// day back before the first (SinceLastDigest false).
	Since           time.Time `json:"since"`
	SinceLastDigest bool      `json:"since_last_digest"`
	// AsOf is when it was read; --post records it as the next Since.
	AsOf time.Time `json:"as_of"`

	// What happened.
	Done        []DigestItem `json:"done"`
	NewPlans    []DigestItem `json:"new_plans"`
	ClosedPlans []DigestItem `json:"closed_plans"`
	Answered    []DigestItem `json:"answered"`
	// Gardeners counts the gardener sessions started since.
	Gardeners int `json:"gardeners"`

	// Where it stands.
	Working []DigestItem `json:"working"`
	// Errands are what the land's people must do, grouped by place
	// (errands.go): the decisions waiting on a person, the owner-audience
	// escalations and the owner-at-device steps.
	Errands []Errand `json:"errands"`
	// InFlight is the In flight section of the steward's notes, one entry a
	// line.
	InFlight []string `json:"in_flight"`

	// Credits. Used is spent plus in flight now (nil when the purse cannot
	// say); SpentSince is Used less what the last digest recorded (nil before
	// the first digest, or when either reading is unknown).
	Used       *float64 `json:"used_credits"`
	SpentSince *float64 `json:"spent_since_credits"`
	Remaining  *float64 `json:"remaining_credits"`
	Granted    float64  `json:"granted_credits"`
	// Unmetered is true for the home garden: operator-paid, no credits to
	// report.
	Unmetered bool `json:"unmetered,omitempty"`

	// Quiet is true when nothing was done, planned, answered or run since:
	// the steward posts nothing.
	Quiet bool `json:"quiet"`
}

// usedCredits is what a land has used: spent plus in flight, nil when spent
// is not known.
func usedCredits(p purse.Purse) *float64 {
	if p.SpentCredits == nil {
		return nil
	}
	u := *p.SpentCredits
	if p.InFlightCredits != nil {
		u += *p.InFlightCredits
	}
	return &u
}

// GatherDigest reads what changed in land since its last digest. p is the
// land's purse as a tenant sees it, read at now.
func GatherDigest(ctx context.Context, src purse.Source, land string, p purse.Purse, now time.Time) (*Digest, error) {
	d := &Digest{Land: land, AsOf: now.UTC(), Granted: p.GrantedCredits, Remaining: p.RemainingCredits, Used: usedCredits(p), Unmetered: p.IsUnmetered()}
	notes, err := notesBead(ctx, src, land)
	if err != nil {
		return nil, err
	}
	var lastUsed *float64
	if notes != nil {
		if at, err := time.Parse(time.RFC3339, strings.TrimSpace(notes.Fields[DigestAtField])); err == nil {
			d.Since, d.SinceLastDigest = at.UTC(), true
			if f, err := strconv.ParseFloat(strings.TrimSpace(notes.Fields[DigestUsedField]), 64); err == nil && f >= 0 {
				lastUsed = &f
			}
		}
		d.InFlight = inFlight(NotesBody(notes))
	}
	if !d.SinceLastDigest {
		d.Since = d.AsOf.Add(-digestDefaultWindow)
	}
	if d.Used != nil && lastUsed != nil {
		s := max(*d.Used-*lastUsed, 0)
		d.SpentSince = &s
	}

	inLand := []string{"project:" + land}
	list := func(what string, q beadsapi.ListBeadsQuery) ([]*beadsapi.BeadDetail, error) {
		if q.Limit == 0 {
			q.Limit = digestScan
		}
		res, err := src.ListBeadsFiltered(ctx, q)
		if err != nil {
			return nil, fmt.Errorf("reading %s's %s: %w", land, what, err)
		}
		var out []*beadsapi.BeadDetail
		for _, b := range res.Beads {
			if b != nil {
				out = append(out, b)
			}
		}
		return out, nil
	}
	// closedSince and createdSince re-check the window the daemon filtered
	// on: a closed_at that does not parse is taken as the daemon gave it.
	closedSince := func(b *beadsapi.BeadDetail) bool {
		at, err := time.Parse(time.RFC3339, strings.TrimSpace(b.ClosedAt))
		return err != nil || at.After(d.Since)
	}
	createdSince := func(b *beadsapi.BeadDetail) bool { return b.CreatedAt.After(d.Since) }

	steps, err := list("closed steps", beadsapi.ListBeadsQuery{Types: stepTypes, Labels: inLand, Statuses: []string{"closed"}, ClosedAfter: d.Since})
	if err != nil {
		return nil, err
	}
	for _, b := range steps {
		if closedSince(b) {
			d.Done = append(d.Done, DigestItem{ID: b.ID, Title: b.Title})
		}
	}

	planLabels := []string{"project:" + land, plangardener.PlanLabel}
	plans, err := list("new plans", beadsapi.ListBeadsQuery{Types: []string{"epic"}, Labels: planLabels, CreatedAfter: d.Since})
	if err != nil {
		return nil, err
	}
	for _, b := range plans {
		if createdSince(b) {
			d.NewPlans = append(d.NewPlans, DigestItem{ID: b.ID, Title: b.Title, Note: b.Fields[planStateField]})
		}
	}
	if plans, err = list("closed plans", beadsapi.ListBeadsQuery{Types: []string{"epic"}, Labels: planLabels, Statuses: []string{"closed"}, ClosedAfter: d.Since}); err != nil {
		return nil, err
	}
	for _, b := range plans {
		if closedSince(b) {
			d.ClosedPlans = append(d.ClosedPlans, DigestItem{ID: b.ID, Title: b.Title, Note: b.Fields[planStateField]})
		}
	}

	decs, err := list("answered decisions", beadsapi.ListBeadsQuery{Types: []string{"decision"}, Labels: inLand, Statuses: []string{"closed"}, ClosedAfter: d.Since})
	if err != nil {
		return nil, err
	}
	for _, b := range decs {
		if closedSince(b) {
			d.Answered = append(d.Answered, DigestItem{ID: b.ID, Title: b.Title, Who: strings.TrimSpace(b.Fields["responded_by"]), Note: chosenOption(b.Fields)})
		}
	}

	agents, err := list("gardeners", beadsapi.ListBeadsQuery{Types: []string{"agent"}, Labels: []string{"project:" + land, "role:" + GardenerRole}, CreatedAfter: d.Since})
	if err != nil {
		return nil, err
	}
	for _, b := range agents {
		if createdSince(b) {
			d.Gardeners++
		}
	}

	working, err := list("steps in progress", beadsapi.ListBeadsQuery{Types: stepTypes, Labels: inLand, Statuses: []string{"in_progress"}})
	if err != nil {
		return nil, err
	}
	for _, b := range working {
		if !b.HumanOnly() { // an owner-at-device step is an errand
			d.Working = append(d.Working, DigestItem{ID: b.ID, Title: b.Title, Who: b.Assignee})
		}
	}

	if d.Errands, err = gatherErrands(inLand, list, createdSince); err != nil {
		return nil, err
	}
	asked := slices.ContainsFunc(d.Errands, func(e Errand) bool { return e.New })

	d.Quiet = len(d.Done) == 0 && len(d.NewPlans) == 0 && len(d.ClosedPlans) == 0 &&
		len(d.Answered) == 0 && d.Gardeners == 0 && !asked
	return d, nil
}

// gatherErrands reads the land's open owner-only items (errands.go): steps
// first, so an escalation filed for a step folds into it.
func gatherErrands(inLand []string, list func(string, beadsapi.ListBeadsQuery) ([]*beadsapi.BeadDetail, error), createdSince func(*beadsapi.BeadDetail) bool) ([]Errand, error) {
	var errands []Errand
	add := func(bs []*beadsapi.BeadDetail, kind string, keep func(*beadsapi.BeadDetail) bool) {
		for _, b := range bs {
			if keep == nil || keep(b) {
				e := errandFor(b, kind)
				e.New = createdSince(b)
				errands = append(errands, e)
			}
		}
	}
	for _, l := range beadsapi.HumanOnlyLabels {
		steps, err := list("owner steps", beadsapi.ListBeadsQuery{Types: stepTypes, Labels: append(slices.Clone(inLand), l), Statuses: []string{"open", "in_progress", "blocked"}})
		if err != nil {
			return nil, err
		}
		add(steps, ErrandStep, nil)
	}
	escs, err := list("open escalations", beadsapi.ListBeadsQuery{Types: []string{"escalation"}, Labels: inLand, Statuses: []string{"open"}, Limit: escalationScan})
	if err != nil {
		return nil, err
	}
	add(escs, ErrandEscalation, func(b *beadsapi.BeadDetail) bool { return beadsapi.EscalationForOwner(b.Fields) })
	open, err := list("open decisions", beadsapi.ListBeadsQuery{Types: []string{"decision"}, Labels: inLand, Statuses: []string{"open", "in_progress"}})
	if err != nil {
		return nil, err
	}
	add(open, ErrandDecision, nil)
	return foldErrands(errands), nil
}

// inFlight is the In flight section of notes, one line per entry.
func inFlight(notes string) []string {
	doc := parseNotes(notes)
	i := doc.section(SectionInFlight)
	if i < 0 {
		return nil
	}
	_, es := entries(doc.sections[i].body)
	out := make([]string, 0, len(es))
	for _, e := range es {
		if t := entryText(e); t != "" {
			out = append(out, t)
		}
	}
	return out
}

// FormatDigest is the digest as gb lands digest prints it for the steward:
// the facts, then how to post. A quiet digest is one line: post nothing.
func FormatDigest(d *Digest) string {
	var b strings.Builder
	since := "a day back (no digest yet)"
	if d.SinceLastDigest {
		since = "the last digest, " + d.Since.Format("2006-01-02 15:04Z")
	}
	if d.Quiet {
		fmt.Fprintf(&b, "Quiet: nothing was done, planned, answered or run in %s since %s.\nPost nothing.\n", d.Land, since)
		return b.String()
	}
	fmt.Fprintf(&b, "Digest for %s, since %s, as of %s.\n", d.Land, since, d.AsOf.Format("2006-01-02 15:04Z"))

	section := func(head string, items []DigestItem, line func(DigestItem) string) {
		if len(items) == 0 {
			return
		}
		fmt.Fprintf(&b, "\n%s:\n", head)
		for _, it := range items {
			fmt.Fprintf(&b, "- %s: %s%s\n", it.ID, it.Title, line(it))
		}
	}
	none := func(DigestItem) string { return "" }
	state := func(it DigestItem) string {
		if it.Note == "" {
			return ""
		}
		return " [" + it.Note + "]"
	}
	section("Done (steps closed: their pull requests merged)", d.Done, none)
	section("New plans", d.NewPlans, state)
	section("Plans finished or dropped", d.ClosedPlans, state)
	section("Answered", d.Answered, func(it DigestItem) string {
		var parts []string
		if it.Note != "" {
			parts = append(parts, it.Note)
		}
		if it.Who != "" {
			parts = append(parts, "by "+strings.TrimPrefix(it.Who, "oidc:"))
		}
		if len(parts) == 0 {
			return ""
		}
		return " (" + strings.Join(parts, ", ") + ")"
	})
	switch d.Gardeners {
	case 0:
	case 1:
		b.WriteString("\n1 gardener session started.\n")
	default:
		fmt.Fprintf(&b, "\n%d gardener sessions started.\n", d.Gardeners)
	}

	section("In progress now", d.Working, func(it DigestItem) string {
		if it.Who == "" {
			return ""
		}
		return " (gardener " + it.Who + ")"
	})
	formatErrands(&b, d.Errands)
	if len(d.InFlight) > 0 {
		b.WriteString("\nIn flight, from your notes:\n")
		for _, l := range d.InFlight {
			fmt.Fprintf(&b, "- %s\n", l)
		}
	}

	b.WriteString("\nCredits: ")
	switch {
	case d.Unmetered:
		b.WriteString("none; the land is operator-paid, so say nothing of credits.\n")
	case d.SpentSince != nil:
		fmt.Fprintf(&b, "%s spent since the last digest; ", formatCredits(*d.SpentSince))
	case d.Used != nil:
		fmt.Fprintf(&b, "%s used so far; ", formatCredits(*d.Used))
	}
	switch {
	case d.Unmetered:
	case d.Remaining != nil:
		fmt.Fprintf(&b, "%s of %s remaining.\n", formatCredits(*d.Remaining), formatCredits(d.Granted))
	default:
		fmt.Fprintf(&b, "%s granted, remaining not known.\n", formatCredits(d.Granted))
	}

	fmt.Fprintf(&b, "\nWrite it for a person and post it in #general with:\n  %s\n", d.PostCommand())
	return b.String()
}

// formatErrands writes the What you must do section: each errand under its
// place, with its clicks and the evidence that closes it.
func formatErrands(b *strings.Builder, errands []Errand) {
	if len(errands) == 0 {
		return
	}
	b.WriteString("\nWhat you must do, by place:\n")
	place := ""
	for _, e := range errands {
		if e.Place != place {
			place = e.Place
			fmt.Fprintf(b, "%s:\n", e.PlaceName())
		}
		var parts []string
		if e.Kind != ErrandDecision {
			parts = append(parts, e.Kind)
		}
		if e.Who != "" {
			parts = append(parts, "for "+strings.TrimPrefix(e.Who, "oidc:"))
		}
		if e.Approved {
			parts = append(parts, "approved, not yet done")
		}
		if e.New {
			parts = append(parts, "asked since the last digest")
		}
		if len(e.Also) > 0 {
			parts = append(parts, "also "+strings.Join(e.Also, ", "))
		}
		fmt.Fprintf(b, "- %s: %s", e.ID, e.Title)
		if len(parts) > 0 {
			fmt.Fprintf(b, " (%s)", strings.Join(parts, "; "))
		}
		b.WriteString("\n")
		if e.Clicks != "" {
			fmt.Fprintf(b, "  clicks: %s\n", strings.Join(strings.Fields(e.Clicks), " "))
		} else if e.Kind != ErrandDecision {
			fmt.Fprintf(b, "  clicks: not written down; read %s\n", e.ID)
		}
		fmt.Fprintf(b, "  done when: %s\n", e.Evidence)
	}
}

// PostCommand is the gb lands digest --post call that posts this digest and
// records where the next one starts.
func (d *Digest) PostCommand() string {
	cmd := fmt.Sprintf("gb lands digest %s --post digest.md --as-of %s", d.Land, d.AsOf.Format(time.RFC3339))
	if d.Used != nil {
		cmd += " --used " + formatCredits(*d.Used)
	}
	return cmd
}

// ErrDigestPosted: the digest as of that time, or a later one, was already
// posted. A retried --post never says it twice.
var ErrDigestPosted = errors.New("a digest as of this time or later was already posted")

// CheckDigestMark refuses a post as of asOf when the land's notes record a
// digest at or after it. It returns the notes bead the mark goes on.
func CheckDigestMark(ctx context.Context, src purse.Source, land string, asOf time.Time) (*beadsapi.BeadDetail, error) {
	notes, err := notesBead(ctx, src, land)
	if err != nil {
		return nil, err
	}
	if notes == nil {
		return nil, fmt.Errorf("land %s has no %s bead to record the digest on", land, NotesTitle(land))
	}
	if at, err := time.Parse(time.RFC3339, strings.TrimSpace(notes.Fields[DigestAtField])); err == nil && !at.Before(asOf) {
		return nil, fmt.Errorf("%w (%s)", ErrDigestPosted, at.UTC().Format(time.RFC3339))
	}
	return notes, nil
}

// RecordDigest writes where the next digest starts on the notes bead: asOf,
// and the used credits then ("" when not known, so the next digest does not
// difference against a stale figure).
func RecordDigest(ctx context.Context, w notesWriter, notesID string, asOf time.Time, used *float64) error {
	fields := map[string]string{DigestAtField: asOf.UTC().Format(time.RFC3339), DigestUsedField: ""}
	if used != nil {
		fields[DigestUsedField] = formatCredits(*used)
	}
	if err := w.UpdateBeadFields(ctx, notesID, fields); err != nil {
		return fmt.Errorf("recording the digest on %s: %w", notesID, err)
	}
	return nil
}
