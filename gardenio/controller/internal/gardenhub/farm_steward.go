package gardenhub

// farm_steward.go — a land's steward on its cost row (gd-srGLTTTxPb, Lands
// Track B spec §4.3): "runs per day are capped (default 24), and the cap is
// visible on the land header when hit". The steward is a standing gardener
// trigger (internal/steward) whose gardener blob carries the cap and the
// engine-owned count of today's runs; the farm reads them on the cost tier, one
// trigger list for every land, and says whether the cap is hit now.
//
// The row also carries what the map shows on the steward at its land's
// homestead (step 9.8, spec §4.4): its name and its reason. The reason is the
// purse wall, else a line of the steward's memory (amendment 2026-10-23 §3,
// §8: "the reason text comes from memory"), the first of In flight, else of
// Promises, as the steward last wrote it. It is never recomputed from the
// land's beads; a handle the line names that still waits on a person only
// makes it a parked reason, so the bubble leads to the letter.
//
// gardenhub does not import internal/steward or internal/bridge (they pull in
// the whole trigger engine), so the labels, the role, the notes title, the
// section rule and the cap rule are copied here. TestFarmStewardMatchesEngine
// in internal/steward pins the copies to steward.MarkerLabel,
// steward.LandLabelPrefix, steward.Role, steward.NotesTitle, its notes
// sections and bridge.GardenerDailyRunCapHit.

import (
	"context"
	"encoding/json"
	"regexp"
	"slices"
	"strings"
	"time"

	"gardenio/controller/internal/beadsapi"
	"gardenio/controller/internal/lands/purse"
)

const (
	// StewardMarkerLabel is steward.MarkerLabel: the label on a land's steward trigger.
	StewardMarkerLabel = "steward"
	// StewardLandLabelPrefix is steward.LandLabelPrefix: gardener-land:<land> binds the steward to its land.
	StewardLandLabelPrefix = "gardener-land:"
	// StewardRole is steward.Role: the role a steward's runs carry, and the
	// last word of its name, <land>-steward.
	StewardRole = "steward"
	// StewardNotesPrefix is steward.NotesTitle's prefix: steward-notes:<land>
	// is the land's memory.
	StewardNotesPrefix = "steward-notes:"
	// StewardSectionInFlight and StewardSectionPromises are
	// steward.SectionInFlight and steward.SectionPromises: the memory's
	// sections the map reads its reason from.
	StewardSectionInFlight = "In flight"
	StewardSectionPromises = "Promises"
)

// A steward's reason kinds beyond a gardener's (FarmReason.Kind,
// farm_reason.go). At the purse wall it is ReasonSleeping, and a memory line
// naming a handle that waits on a person is ReasonParked.
const (
	ReasonInFlight = "in_flight" // the first In flight line of its memory
	ReasonPromise  = "promise"   // the first Promises line, when nothing is in flight
)

// StewardParkedText is the steward's reason at the purse wall (spec §4.4).
const StewardParkedText = "Parked: the purse is empty"

// stewardNoteStatuses are the statuses steward's notesBead reads.
var stewardNoteStatuses = []string{"open", "in_progress", "blocked", "deferred"}

// stewardStatuses are the trigger statuses steward.FindSteward reads: a
// steward enabled or paused.
var stewardStatuses = []string{"open", "in_progress", "blocked"}

// FarmSteward is a land's steward as its land header shows it: the daily run
// cap and whether it is hit. Absent on a land with no steward.
type FarmSteward struct {
	ID            string `json:"id"`               // the steward's trigger bead
	Href          string `json:"href"`             // the trigger's bead page
	Agent         string `json:"agent"`            // its name, <land>-steward, as its runs post
	MaxRunsPerDay int    `json:"max_runs_per_day"` // 0 = no daily cap
	RunsToday     int    `json:"runs_today"`       // runs spawned since 00:00 UTC
	// Capped is true when RunsToday has reached MaxRunsPerDay: no wake spawns
	// the steward until ResumesAt, the next 00:00 UTC (unix seconds; 0 and
	// omitted while not capped).
	Capped    bool  `json:"capped"`
	ResumesAt int64 `json:"resumes_at,omitempty"`
	// Reason is what the map's steward says on hover and tap (step 9.8):
	// the purse wall, else its memory's first In flight or Promises line.
	// null when neither says anything.
	Reason *FarmReason `json:"reason"`
	// Notes is the land's steward-notes:<land> bead, the steward's memory the
	// land header links (gd-q4NgYNGpy8; GET /garden/notes/{land},
	// land_notes.go). Omitted on a land with none (a pre-9.3 land).
	Notes string `json:"notes,omitempty"`
}

// stewardCap is the part of a steward trigger's gardener blob
// (bridge.GardenerSpec) the cap needs.
type stewardCap struct {
	MaxRunsPerDay int    `json:"max_runs_per_day"`
	RunsDay       string `json:"runs_day"` // YYYY-MM-DD, UTC: the day RunsToday counts
	RunsToday     int    `json:"runs_today"`
}

// StewardRunCap reads a steward trigger's gardener blob at now: the cap, the
// runs on now's UTC day, and whether the cap is hit (the rule of
// bridge.GardenerDailyRunCapHit). ok is false when the blob is not JSON.
func StewardRunCap(gardener string, now time.Time) (s FarmSteward, ok bool) {
	var c stewardCap
	if err := json.Unmarshal([]byte(gardener), &c); err != nil {
		return FarmSteward{}, false
	}
	u := now.UTC()
	s.MaxRunsPerDay = max(c.MaxRunsPerDay, 0)
	if c.RunsDay == u.Format(time.DateOnly) {
		s.RunsToday = c.RunsToday
	}
	if s.MaxRunsPerDay > 0 && s.RunsToday >= s.MaxRunsPerDay {
		s.Capped = true
		s.ResumesAt = time.Date(u.Year(), u.Month(), u.Day()+1, 0, 0, 0, 0, time.UTC).Unix()
	}
	return s, true
}

// stewardLand is the land a steward trigger is bound to: its one
// gardener-land: label, which must name a project: label it also carries, as
// steward's prompt renderer requires. "" for anything else, so a stray label
// never shows one land's steward on another's header.
func stewardLand(labels []string) string {
	land := ""
	for _, l := range labels {
		if rest, ok := strings.CutPrefix(l, StewardLandLabelPrefix); ok {
			if land != "" {
				return ""
			}
			land = rest
		}
	}
	if land == "" || !slices.Contains(labels, "project:"+land) {
		return ""
	}
	return land
}

// readStewards lists the steward triggers, one per land: the lowest bead id
// when a land has two. Only the trigger and its gardener blob are kept; the
// cap is read against the clock when it is attached.
func (f *Farm) readStewards(ctx context.Context) (map[string]*beadsapi.BeadDetail, error) {
	res, err := f.src.ListBeadsFiltered(ctx, beadsapi.ListBeadsQuery{
		Types: []string{"trigger"}, Labels: []string{StewardMarkerLabel}, Statuses: stewardStatuses, Limit: listLimit,
	})
	if err != nil {
		return nil, err
	}
	out := map[string]*beadsapi.BeadDetail{}
	for _, b := range res.Beads {
		if b == nil || !slices.Contains(b.Labels, StewardMarkerLabel) || strings.TrimSpace(b.Fields["gardener"]) == "" {
			continue
		}
		land := stewardLand(b.Labels)
		if land == "" {
			continue
		}
		if prev, ok := out[land]; ok && prev.ID < b.ID {
			continue
		}
		out[land] = &beadsapi.BeadDetail{ID: b.ID, Fields: map[string]string{"gardener": b.Fields["gardener"]}}
	}
	return out, nil
}

// readStewardNotes reads each land's steward-notes:<land> bead, the way
// steward's notesBead does, and keeps its body and id by land. A land whose read
// fails keeps the body last read; a land with no notes bead has none.
func (f *Farm) readStewardNotes(ctx context.Context, lands []string) {
	if f.costSt.notes == nil {
		f.costSt.notes = map[string]string{}
	}
	if f.costSt.notesID == nil {
		f.costSt.notesID = map[string]string{}
	}
	for _, land := range lands {
		b, err := findLandNotes(ctx, f.src, land)
		if err != nil {
			f.logger.Warn("garden farm: steward notes read failed", "land", land, "error", err)
			continue
		}
		delete(f.costSt.notes, land)
		delete(f.costSt.notesID, land)
		if b != nil {
			f.costSt.notes[land] = firstNonEmpty(strings.TrimSpace(b.Fields["content"]), strings.TrimSpace(b.Description))
			f.costSt.notesID[land] = b.ID
		}
	}
}

// notesSection is the body of the memory's section name: the lines under its
// "## " heading, which is name alone or name and more words ("## In flight
// (plans, PRs, crews)"), the rule steward's parser reads sections by.
func notesSection(notes, name string) []string {
	var body []string
	in := false
	for _, l := range strings.Split(notes, "\n") {
		if h, ok := strings.CutPrefix(strings.TrimRight(l, " \t\r"), "## "); ok {
			h = strings.TrimSpace(h)
			if in {
				break
			}
			in = h == name || strings.HasPrefix(h, name+" ")
			continue
		}
		if in {
			body = append(body, l)
		}
	}
	return body
}

// firstNote is the first "- " or "* " entry of the memory's section name, its
// first line only, without markdown emphasis or code marks; "" when the section
// is empty or missing.
func firstNote(notes, name string) string {
	for _, l := range notesSection(notes, name) {
		t := strings.TrimSpace(l)
		if rest, ok := strings.CutPrefix(t, "- "); ok {
			t = rest
		} else if rest, ok := strings.CutPrefix(t, "* "); ok {
			t = rest
		} else {
			continue
		}
		t = strings.NewReplacer("**", "", "`", "").Replace(t)
		if t = strings.TrimSpace(t); t != "" {
			return t
		}
	}
	return ""
}

// noteBeadID is a bead id written in a memory line (gd-BdJ7R2i7Cg, ka-0000001089).
var noteBeadID = regexp.MustCompile(`\b[a-z]{2,6}-[A-Za-z0-9]{6,}\b`)

// StewardReason is the land's steward's reason (step 9.8): at the purse
// wall, StewardParkedText; else memory's first In flight line, else its first
// Promises line, as written. A line naming a handle that waitOn finds still
// waiting on a person is parked on it, with the handle's href. nil when the
// purse is not at the wall and memory has nothing in flight or promised.
func StewardReason(p *purse.Purse, notes string, waitOn func(id string) bool) *FarmReason {
	if p != nil && p.State == purse.StateWall {
		return &FarmReason{Kind: ReasonSleeping, Text: StewardParkedText}
	}
	kind, text := ReasonInFlight, firstNote(notes, StewardSectionInFlight)
	if text == "" {
		kind, text = ReasonPromise, firstNote(notes, StewardSectionPromises)
	}
	if text == "" {
		return nil
	}
	r := &FarmReason{Kind: kind, Text: clip(text, maxReasonText)}
	for _, id := range noteBeadID.FindAllString(text, -1) {
		if waitOn(id) {
			r.Kind, r.Href = ReasonParked, beadHref("", id)
			break
		}
	}
	return r
}

// attachStewards re-reads the steward triggers and their lands' memory and
// puts each on its land's row. rows are the farm's open work rows, where a
// handle a memory line names is looked up. A failed read keeps the triggers
// (and notes) last read, whose cap is still read against now, so a cap lifts
// at midnight even while the daemon is down; it never fails the farm refresh.
func (f *Farm) attachStewards(ctx context.Context, cost *FarmCost, rows []*beadsapi.BeadDetail) {
	if got, err := f.readStewards(ctx); err != nil {
		f.logger.Warn("garden farm: steward trigger read failed", "error", err)
	} else {
		f.costSt.stewards = got
	}
	var lands []string
	for _, row := range cost.Projects {
		if _, ok := f.costSt.stewards[row.Project]; ok {
			lands = append(lands, row.Project)
		}
	}
	f.readStewardNotes(ctx, lands)
	byID := make(map[string]*beadsapi.BeadDetail, len(rows))
	for _, b := range rows {
		if b != nil {
			byID[b.ID] = b
		}
	}
	waitOn := func(id string) bool { return waitsOnHuman(byID[id]) }
	now := f.now()
	for i := range cost.Projects {
		row := &cost.Projects[i]
		b, ok := f.costSt.stewards[row.Project]
		if !ok {
			continue
		}
		s, ok := StewardRunCap(b.Fields["gardener"], now)
		if !ok {
			continue
		}
		s.ID, s.Href, s.Agent = b.ID, beadHref("", b.ID), row.Project+"-"+StewardRole
		s.Reason = StewardReason(row.Purse, f.costSt.notes[row.Project], waitOn)
		s.Notes = f.costSt.notesID[row.Project]
		row.Steward = &s
	}
}
