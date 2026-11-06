package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"

	"gardenio/controller/internal/beadsapi"
)

// triggersListLimit caps the daemon list call. Trigger beads are bounded by
// hand (operators write them, not bots), so a single fetch covers any
// realistic deployment.
const triggersListLimit = 500

// triggerFilter is the ?filter= query value.
type triggerFilter int

const (
	triggerFilterAll triggerFilter = iota
	triggerFilterEnabled
	triggerFilterDisabled
)

// triggerSort is the ?sort= query value. Default is source+title — works for
// every source. next-fire is kept as an explicit option but is meaningful
// only for schedule-source triggers.
type triggerSort int

const (
	triggerSortSource triggerSort = iota
	triggerSortLastFired
	triggerSortFireCount
	triggerSortNextFire
	triggerSortCron
)

// triggerSources is the set of recognized trigger source values, in chip-strip
// order. Used to build the ?source= filter chips and to validate inbound
// query values (unknown values fall back to "all").
var triggerSources = []string{
	"jira", "gitlab", "webhook", "alert", "bead", "account", "schedule", "slack-reaction",
}

// triggerActions is the set of recognized action values, in chip-strip order.
// Drives the ?action= filter chips and validates inbound query values.
var triggerActions = []string{
	"spawn_agent", "apply_formula", "create_bead", "notify",
}

// triggerDensity is the ?density= query value / persisted preference. Drives
// the CSS class on the page wrapper; the actual layout is pure CSS so the
// toggle requires no re-render.
type triggerDensity int

const (
	triggerDensityComfortable triggerDensity = iota
	triggerDensityCompact
)

// triggerRowView is one row on /triggers.
type triggerRowView struct {
	ID    string
	Title string

	// Source is the trigger's data feed: jira, gitlab, webhook, bead, account,
	// schedule, slack-reaction. Computed from the trigger:<source> label;
	// legacy type=schedule beads are "schedule".
	Source      string
	SourceLabel string // human-readable, e.g. "jira", "slack-reaction"
	SourceGlyph string // 2-letter compact-mode glyph, e.g. "ji", "gl"

	// Action is what fires on match: spawn_agent, apply_formula, create_bead,
	// notify. Legacy schedules default to spawn_agent.
	Action      string
	ActionLabel string // "spawn", "formula", "bead", "notify" (compact)
	ActionFull  string // "spawn_agent" etc. (comfortable)

	// MatchSummary is the source-specific "what is this watching?" string.
	// E.g. `JQL: project = FSM …` for jira, `*/30 * * * * (UTC) · next 12m`
	// for schedule.
	MatchSummary string

	// DestinationSummary is the action-specific "what happens when it fires?"
	// string. E.g. `→ spawn role=crew (gardenio)`.
	DestinationSummary string

	// Activity is the freshest of last_fired/last_run/last_checked, rendered
	// as a relative time ("12s", "3h"). Empty when none are set.
	Activity      string
	ActivityKind  string // "fired" / "run" / "checked"
	activityAt    time.Time
	FireCount     string
	Cooldown      string
	MaxConcurrent string

	Cron     string
	Enabled  bool
	Timezone string
	Project  string
	Role     string

	// NextFire / NextFireRelative / NextFireError populate ONLY for source =
	// "schedule"; the template suppresses them otherwise (no more bogus
	// `next: <error>` on non-cron triggers).
	NextFire         string
	NextFireRelative string
	NextFireError    string

	LastRun      string
	RunsSpawned  string
	IsClosed     bool
	Ago          string
	UpdatedAtRFC string

	// Kind is "schedule" for legacy type=schedule beads or "trigger" for
	// type=trigger label=trigger:schedule beads. Rendered as a small chip so
	// operators can distinguish the two primitives at a glance — the source
	// chip alone doesn't disambiguate (both are source=schedule).
	Kind      string
	KindLabel string

	// Type / HasReport / BlockedOnDecision* mirror the BeadDetail predicates
	// so the universal row_badges partial can render the report/blocked-by
	// affordances. The blocked-by badge surfaces the decision but the toggle
	// remains live (policy ka-0000001354: surface, do not block).
	Type                    string
	HasReport               bool
	BlockedOnDecision       bool
	BlockedOnDecisionBeadID string

	// nextFireAt is the parsed next-fire time used for sorting; zero when
	// the cron expression is missing or unparseable. Not rendered.
	nextFireAt time.Time
	// nextFireAt2 is the fire AFTER nextFireAt — used purely as a
	// deterministic tiebreaker when two schedules share the same next-fire
	// instant (e.g. an @hourly UTC row and a 0 21 PDT row both landing on
	// 04:00 UTC during the 03:00–04:00 UTC window). The schedule that fires
	// again soonest (the more frequent one) sorts first, which keeps the
	// hourly row above a coincident daily row regardless of wall-clock
	// (ka-0000003725 / ka-0000007664). Zero when nextFireAt is zero. Not
	// rendered.
	nextFireAt2 time.Time

	// PourStalled and PourStallDetail surface the engine's pouring-but-never-
	// worked verdict (ka-0000000957), stamped as the `pour_stall` field by the
	// trigger-engine's pouring-stall sweep. True means the trigger keeps
	// pouring molecules on cadence (so it looks alive — Activity is fresh) while
	// those molecules go unworked. Read directly off the stamped field so the
	// row builder does no graph walk.
	PourStalled     bool
	PourStallDetail string
}

// triggerSortLink is one sort link in the sort strip.
type triggerSortLink struct {
	Label  string
	Href   string
	Active bool
}

// triggerDensityChoice is one density toggle option.
type triggerDensityChoice struct {
	Label  string
	Value  string
	Href   string
	Active bool
}

// handleTriggers renders /triggers — a list of trigger beads (both legacy
// type=schedule and type=trigger label=trigger:schedule) with a density-
// managed row shape that surfaces source + action + match summary +
// destination summary uniformly across all 7 sources.
//
// Query params:
//
//	?filter=  one of all|enabled|disabled (default: all)
//	?sort=    one of source|last-fired|fire-count|next-fire|cron
//	          (default: source — groups by source then title)
//	?density= one of comfortable|compact (default: comfortable; the
//	          localStorage script overrides this client-side)
func (s *Server) handleTriggers(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	filter := parseTriggerFilter(q.Get("filter"))
	sortBy := parseTriggerSort(q.Get("sort"))
	density := parseTriggerDensity(q.Get("density"))
	source := parseTriggerEnumFilter(q.Get("source"), triggerSources)
	action := parseTriggerEnumFilter(q.Get("action"), triggerActions)

	data := map[string]any{
		"Filter":         triggerFilterString(filter),
		"Sort":           triggerSortString(sortBy),
		"Density":        triggerDensityString(density),
		"Source":         source,
		"Action":         action,
		"Chips":          []filterChip{},
		"SourceChips":    []filterChip{},
		"ActionChips":    []filterChip{},
		"SortLinks":      []triggerSortLink{},
		"DensityChoices": []triggerDensityChoice{},
		"Rows":           []triggerRowView{},
		"Truncated":      false,
		"Limit":          triggersListLimit,
	}

	res, err := s.daemon.ListBeadsFiltered(r.Context(), beadsapi.ListBeadsQuery{
		Types: []string{"schedule"},
		Sort:  "updated_at",
		Limit: triggersListLimit,
	})
	if err != nil {
		s.logger.Error("listing triggers", "error", err)
		data["Error"] = "Failed to load triggers."
		s.render(w, r, "triggers.html", data)
		return
	}

	// Also fetch type=trigger beads across every source label. The row shape
	// is universal — per-source pills and match summaries diverge inside the
	// template — so we want one list query that returns trigger:jira,
	// trigger:gitlab, trigger:webhook, trigger:bead, trigger:account,
	// trigger:schedule, and trigger:slack-reaction together. Operators want
	// "every active trigger in one place"; legacy type=schedule rolls in
	// alongside via the first query above.
	trigRes, err := s.daemon.ListBeadsFiltered(r.Context(), beadsapi.ListBeadsQuery{
		Types: []string{"trigger"},
		Sort:  "updated_at",
		Limit: triggersListLimit,
	})
	if err != nil {
		s.logger.Error("listing trigger:schedule beads", "error", err)
		data["Error"] = "Failed to load triggers."
		s.render(w, r, "triggers.html", data)
		return
	}

	now := s.now()
	allRows := make([]triggerRowView, 0, len(res.Beads)+len(trigRes.Beads))
	for _, b := range res.Beads {
		if b == nil {
			continue
		}
		allRows = append(allRows, buildTriggerRow(b, now))
	}
	for _, b := range trigRes.Beads {
		if b == nil {
			continue
		}
		allRows = append(allRows, buildTriggerRow(b, now))
	}

	// Counts for chip badges. The enabled/disabled count is post-source/action
	// scope (so "Enabled (12)" matches what you'd see after clicking it).
	// Source/action chip counts ignore each other but respect the
	// enabled-filter (so "jira (5)" reflects the visible page after clicking).
	enabledCount, disabledCount := 0, 0
	sourceCounts := make(map[string]int, len(triggerSources))
	actionCounts := make(map[string]int, len(triggerActions))
	for _, row := range allRows {
		matchSource := source == "" || row.Source == source
		matchAction := action == "" || row.Action == action
		if matchSource && matchAction {
			if row.Enabled {
				enabledCount++
			} else {
				disabledCount++
			}
		}
		if matchAction && triggerFilterMatches(row, filter) {
			sourceCounts[row.Source]++
		}
		if matchSource && triggerFilterMatches(row, filter) {
			actionCounts[row.Action]++
		}
	}

	// Filter to the selected chip(s).
	rows := make([]triggerRowView, 0, len(allRows))
	for _, row := range allRows {
		if !triggerFilterMatches(row, filter) {
			continue
		}
		if source != "" && row.Source != source {
			continue
		}
		if action != "" && row.Action != action {
			continue
		}
		rows = append(rows, row)
	}

	sortTriggerRows(rows, sortBy)

	hrefFor := func(filterValue string) string {
		return triggerHref(s.basePath, triggerHrefOpts{
			Filter:  filterValue,
			Sort:    triggerSortString(sortBy),
			Density: triggerDensityString(density),
			Source:  source,
			Action:  action,
		})
	}
	chips := []filterChip{
		{Label: "All", Value: "", Count: len(allRows), Active: filter == triggerFilterAll, Href: hrefFor("")},
		{Label: "Enabled", Value: "enabled", Count: enabledCount, Active: filter == triggerFilterEnabled, Href: hrefFor("enabled")},
		{Label: "Disabled", Value: "disabled", Count: disabledCount, Active: filter == triggerFilterDisabled, Href: hrefFor("disabled")},
	}

	sourceHref := func(v string) string {
		return triggerHref(s.basePath, triggerHrefOpts{
			Filter:  triggerFilterString(filter),
			Sort:    triggerSortString(sortBy),
			Density: triggerDensityString(density),
			Source:  v,
			Action:  action,
		})
	}
	// DataVal is written as data-source="<value>" on every chip (including the
	// empty "All sources" chip) by the shared filter_strip partial invoked with
	// DataKey "source", reproducing the strip's pre-consolidation markup.
	sourceChips := make([]filterChip, 0, len(triggerSources)+1)
	sourceChips = append(sourceChips, filterChip{
		Label: "All sources", Value: "", Count: len(allRows), Active: source == "", Href: sourceHref(""),
		DataVal: "",
	})
	for _, src := range triggerSources {
		sourceChips = append(sourceChips, filterChip{
			Label:  src,
			Value:  src,
			Count:  sourceCounts[src],
			Active: source == src,
			Href:   sourceHref(src),

			DataVal: src,
		})
	}

	actionHref := func(v string) string {
		return triggerHref(s.basePath, triggerHrefOpts{
			Filter:  triggerFilterString(filter),
			Sort:    triggerSortString(sortBy),
			Density: triggerDensityString(density),
			Source:  source,
			Action:  v,
		})
	}
	actionChips := make([]filterChip, 0, len(triggerActions)+1)
	actionChips = append(actionChips, filterChip{
		Label: "All actions", Value: "", Count: len(allRows), Active: action == "", Href: actionHref(""),
		DataVal: "",
	})
	for _, act := range triggerActions {
		actionChips = append(actionChips, filterChip{
			Label:  act,
			Value:  act,
			Count:  actionCounts[act],
			Active: action == act,
			Href:   actionHref(act),

			DataVal: act,
		})
	}

	sortHref := func(v string) string {
		return triggerHref(s.basePath, triggerHrefOpts{
			Filter:  triggerFilterString(filter),
			Sort:    v,
			Density: triggerDensityString(density),
			Source:  source,
			Action:  action,
		})
	}
	sortLinks := []triggerSortLink{
		{Label: "source", Href: sortHref(""), Active: sortBy == triggerSortSource},
		{Label: "last fired", Href: sortHref("last-fired"), Active: sortBy == triggerSortLastFired},
		{Label: "fire count", Href: sortHref("fire-count"), Active: sortBy == triggerSortFireCount},
		{Label: "next fire", Href: sortHref("next-fire"), Active: sortBy == triggerSortNextFire},
		{Label: "cron", Href: sortHref("cron"), Active: sortBy == triggerSortCron},
	}

	densityHref := func(v string) string {
		return triggerHref(s.basePath, triggerHrefOpts{
			Filter:  triggerFilterString(filter),
			Sort:    triggerSortString(sortBy),
			Density: v,
			Source:  source,
			Action:  action,
		})
	}
	densityChoices := []triggerDensityChoice{
		{Label: "Comfortable", Value: "comfortable", Href: densityHref(""), Active: density == triggerDensityComfortable},
		{Label: "Compact", Value: "compact", Href: densityHref("compact"), Active: density == triggerDensityCompact},
	}

	data["Chips"] = chips
	data["SourceChips"] = sourceChips
	data["ActionChips"] = actionChips
	data["SortLinks"] = sortLinks
	data["DensityChoices"] = densityChoices
	data["Rows"] = rows
	data["Truncated"] = res.Total > len(res.Beads)
	s.render(w, r, "triggers.html", data)
}

// triggerFilterMatches reports whether row satisfies the enabled/disabled
// filter chip selection.
func triggerFilterMatches(row triggerRowView, filter triggerFilter) bool {
	switch filter {
	case triggerFilterEnabled:
		return row.Enabled
	case triggerFilterDisabled:
		return !row.Enabled
	}
	return true
}

// parseTriggerEnumFilter normalizes a query value against an allowlist of
// known enum values. Empty / unknown input returns "" (no filter).
func parseTriggerEnumFilter(q string, allow []string) string {
	q = strings.TrimSpace(q)
	if q == "" {
		return ""
	}
	if slices.Contains(allow, q) {
		return q
	}
	return ""
}

func buildTriggerRow(b *beadsapi.BeadDetail, now time.Time) triggerRowView {
	f := b.Fields
	kind, kindLabel := triggerKind(b)
	source, sourceLabel, sourceGlyph := triggerSource(b)
	action, actionLabel, actionFull := triggerAction(b)
	// Default for `enabled` differs between the two primitives: legacy
	// schedules require explicit `enabled: "true"`; trigger:schedule beads
	// are enabled-by-default unless `enabled: "false"`. Match the loaders.
	enabled := parseBool(f["enabled"])
	if kind == "trigger" {
		enabled = f["enabled"] != "false"
	}
	row := triggerRowView{
		ID:                      b.ID,
		Title:                   b.Title,
		Type:                    b.Type,
		HasReport:               b.HasReport,
		BlockedOnDecision:       b.BlockedOnDecision,
		BlockedOnDecisionBeadID: b.BlockedOnDecisionBeadID,
		Kind:                    kind,
		KindLabel:               kindLabel,
		Source:                  source,
		SourceLabel:             sourceLabel,
		SourceGlyph:             sourceGlyph,
		Action:                  action,
		ActionLabel:             actionLabel,
		ActionFull:              actionFull,
		Cron:                    f["cron"],
		Enabled:                 enabled,
		Timezone:                f["timezone"],
		Project:                 f["project"],
		Role:                    f["role"],
		LastRun:                 f["last_run"],
		RunsSpawned:             f["runs_spawned"],
		FireCount:               pickNonEmpty(f["fire_count"], f["runs_spawned"]),
		Cooldown:                f["cooldown_minutes"],
		MaxConcurrent:           f["max_concurrent"],
		IsClosed:                statusOpenSortKey(b.Status) != 0,
		Ago:                     fmtTimeAgo(b.UpdatedAt, now),
		UpdatedAtRFC:            rfcTime(b.UpdatedAt),
	}
	if row.Project == "" {
		row.Project = projectFromLabels(b.Labels)
	}
	// Cadence fallback (ka-0000007640): a schedule-source bead migrated onto a
	// `triggers:` array keeps its cadence in the first kind:cron entry. Prefer
	// the legacy top-level cron/timezone when present (no change for
	// un-migrated beads); fall back to the triggers array once the deprecation
	// pass clears the legacy fields, so the row still renders cron + next-fire.
	// Scoped to schedule-source rows so an event-mode (trigger:multi) gardener's
	// watchdog cron entry is never surfaced here as a clock cadence.
	if row.Source == "schedule" && row.Cron == "" {
		row.Cron, row.Timezone = scheduleCronTimezone(b)
	}
	// Next-fire only renders for cron-bearing schedule-source rows. Other
	// sources have no cron, so we deliberately leave NextFire / NextFireError
	// empty — the template suppresses the slot entirely (no more
	// `next: expected exactly 5 fields` noise on jira / gitlab / etc.).
	if row.Source == "schedule" && row.Cron != "" {
		nf, nf2, err := computeNextFire2(row.Cron, row.Timezone, now)
		if err != nil {
			row.NextFireError = err.Error()
		} else {
			row.nextFireAt = nf
			row.nextFireAt2 = nf2
			row.NextFire = nf.Format("2006-01-02 15:04 MST")
			row.NextFireRelative = fmtTimeUntil(nf, now)
		}
	}
	row.MatchSummary = triggerMatchSummary(row, f)
	row.DestinationSummary = triggerDestinationSummary(row, f)
	row.Activity, row.ActivityKind, row.activityAt = triggerActivity(f, now)
	if f["pour_stall"] == "true" {
		row.PourStalled = true
		detail := fmt.Sprintf("%s inert molecule(s)", pickNonEmpty(f["pour_stall_molecules"], "?"))
		if age := f["pour_stall_oldest_age"]; age != "" {
			detail += ", oldest quiet " + age
		}
		row.PourStallDetail = detail
	}
	return row
}

// fmtTimeUntil formats a forward-looking duration as "in 42s" / "in 12m" /
// "in 3h" / "in 2d". Returns "" for the zero time. Past times read as "now"
// — the trigger next-fire is always in the future, so this is a safety net
// for rows whose computed time briefly straddles `now`.
func fmtTimeUntil(t, now time.Time) string {
	if t.IsZero() {
		return ""
	}
	d := t.Sub(now)
	if d <= 0 {
		return "now"
	}
	switch {
	case d < time.Minute:
		return "in " + strconv.Itoa(int(d.Seconds())) + "s"
	case d < time.Hour:
		return "in " + strconv.Itoa(int(d.Minutes())) + "m"
	case d < 24*time.Hour:
		return "in " + strconv.Itoa(int(d.Hours())) + "h"
	default:
		return "in " + strconv.Itoa(int(d.Hours()/24)) + "d"
	}
}

func sortTriggerRows(rows []triggerRowView, sortBy triggerSort) {
	sort.SliceStable(rows, func(i, j int) bool {
		// Disabled / closed triggers sink below active ones regardless of sort.
		ai := triggerActiveSortKey(rows[i])
		aj := triggerActiveSortKey(rows[j])
		if ai != aj {
			return ai < aj
		}
		switch sortBy {
		case triggerSortCron:
			if rows[i].Cron != rows[j].Cron {
				return rows[i].Cron < rows[j].Cron
			}
		case triggerSortNextFire:
			ti, tj := rows[i].nextFireAt, rows[j].nextFireAt
			if ti.IsZero() != tj.IsZero() {
				return !ti.IsZero()
			}
			if !ti.Equal(tj) {
				return ti.Before(tj)
			}
			// Equal next-fire instant (e.g. @hourly UTC and 0 21 PDT both
			// landing on 04:00 UTC): break the tie by the subsequent fire so
			// the more frequent schedule ranks first. Without this the tie
			// fell through to ID order, making the hourly row's position
			// wall-clock-dependent (ka-0000007664).
			ti2, tj2 := rows[i].nextFireAt2, rows[j].nextFireAt2
			if ti2.IsZero() != tj2.IsZero() {
				return !ti2.IsZero()
			}
			if !ti2.Equal(tj2) {
				return ti2.Before(tj2)
			}
		case triggerSortLastFired:
			// Most recent activity first; rows with no activity sink last.
			ti, tj := rows[i].activityAt, rows[j].activityAt
			if ti.IsZero() != tj.IsZero() {
				return !ti.IsZero()
			}
			if !ti.Equal(tj) {
				return ti.After(tj)
			}
		case triggerSortFireCount:
			// Highest fire count first; ties fall through to ID.
			ci, cj := parseInt(rows[i].FireCount), parseInt(rows[j].FireCount)
			if ci != cj {
				return ci > cj
			}
		default:
			// Source, then title — works for every source.
			if rows[i].Source != rows[j].Source {
				return rows[i].Source < rows[j].Source
			}
			ti, tj := strings.ToLower(rows[i].Title), strings.ToLower(rows[j].Title)
			if ti != tj {
				return ti < tj
			}
		}
		return rows[i].ID < rows[j].ID
	})
}

// triggerKind returns ("schedule", "legacy schedule") for type=schedule beads
// and ("trigger", "trigger") for type=trigger beads (which we only fetch when
// they carry the trigger:schedule label). The label text is what gets rendered
// on the row chip so operators can tell the two primitives apart.
func triggerKind(b *beadsapi.BeadDetail) (kind, label string) {
	if b.Type == "trigger" {
		return "trigger", "trigger"
	}
	return "schedule", "legacy schedule"
}

// triggerSource extracts the source name from a trigger bead's labels.
// type=trigger beads carry a `trigger:<source>` label; legacy type=schedule
// beads are implicitly source=schedule.
//
// Returns (source, label, glyph). Glyph is the 2-letter abbreviation used
// in compact mode.
func triggerSource(b *beadsapi.BeadDetail) (source, label, glyph string) {
	if b.Type == "schedule" {
		return "schedule", "schedule", "sc"
	}
	for _, l := range b.Labels {
		if rest, ok := strings.CutPrefix(l, "trigger:"); ok {
			return rest, rest, sourceGlyph(rest)
		}
	}
	// Type=trigger with no trigger:<source> label is malformed but render as
	// "unknown" rather than crash — operators sometimes hand-edit these.
	return "unknown", "unknown", "??"
}

func sourceGlyph(source string) string {
	switch source {
	case "jira":
		return "ji"
	case "gitlab":
		return "gl"
	case "webhook":
		return "wh"
	case "alert":
		return "al"
	case "bead":
		return "bd"
	case "account":
		return "ac"
	case "schedule":
		return "sc"
	case "slack-reaction":
		return "sl"
	}
	if len(source) >= 2 {
		return source[:2]
	}
	return source
}

// triggerAction extracts the action from a trigger bead's fields. Legacy
// schedules default to spawn_agent (the only action they support).
//
// Returns (action, shortLabel, fullLabel). Short is used in compact mode,
// full in comfortable mode.
func triggerAction(b *beadsapi.BeadDetail) (action, short, full string) {
	a := strings.TrimSpace(b.Fields["action"])
	if a == "" {
		a = "spawn_agent"
	}
	switch a {
	case "spawn_agent":
		return a, "spawn", "spawn_agent"
	case "apply_formula":
		return a, "formula", "apply_formula"
	case "create_bead":
		return a, "bead", "create_bead"
	case "notify":
		return a, "notify", "notify"
	}
	return a, a, a
}

// triggerMatchSummary renders the source-specific "what does this watch?"
// answer (design bundle ka-0000003886, rank 5). For schedule-source rows the
// next-fire countdown is folded in here — the global meta line no longer
// renders a `next: <error>` slot for non-cron triggers.
func triggerMatchSummary(row triggerRowView, f map[string]string) string {
	switch row.Source {
	case "jira":
		jql := strings.TrimSpace(f["jql"])
		if jql == "" {
			return "JQL: (none)"
		}
		return "JQL: " + jql
	case "gitlab":
		var parts []string
		if pp := strings.TrimSpace(f["project_path"]); pp != "" {
			parts = append(parts, pp)
		}
		ev := strings.TrimSpace(f["event_type"])
		state := strings.TrimSpace(f["state"])
		pstatus := strings.TrimSpace(f["pipeline_status"])
		pref := strings.TrimSpace(f["pipeline_ref"])
		prefPattern := strings.TrimSpace(f["pipeline_ref_pattern"])
		pminDuration := strings.TrimSpace(f["min_duration"])
		switch {
		case ev == "pipeline" && pstatus != "":
			tail := "pipeline=" + pstatus
			if pref != "" {
				tail += "@" + pref
			}
			// The ref regexp is a distinct filter from the exact ref (it applies
			// client-side after the fetch); surface it so an operator can see at
			// a glance that non-matching refs are being suppressed.
			if prefPattern != "" {
				tail += " ~/" + prefPattern + "/"
			}
			// The slow-pipeline threshold is likewise a distinct filter (applied
			// client-side against the pipeline's run duration); surface it so an
			// operator can see at a glance that only slow pipelines fire.
			if pminDuration != "" {
				tail += " ≥" + pminDuration
			}
			parts = append(parts, tail)
		case ev != "" && state != "":
			parts = append(parts, ev+"="+state)
		case ev != "":
			parts = append(parts, ev)
		}
		if len(parts) == 0 {
			return "(any gitlab event)"
		}
		return strings.Join(parts, " · ")
	case "webhook":
		token := strings.TrimSpace(f["webhook_token"])
		var head string
		if token == "" {
			head = "POST /webhooks/(any)"
		} else {
			head = "POST /webhooks/" + token
		}
		if n := countJSONArray(f["conditions"]); n > 0 {
			head += " · " + strconv.Itoa(n) + " conditions"
		}
		return head
	case "alert":
		// Alertmanager webhook + severity→bead-type routing (W3 alerts-in).
		token := strings.TrimSpace(f["webhook_token"])
		head := "POST /webhooks/(any)"
		if token != "" {
			head = "POST /webhooks/" + token
		}
		if route := strings.TrimSpace(f["alert_route"]); route != "" {
			head += " · route " + route
		}
		if n := countJSONArray(f["conditions"]); n > 0 {
			head += " · " + strconv.Itoa(n) + " conditions"
		}
		return head
	case "bead":
		events := strings.TrimSpace(f["event_types"])
		if events == "" {
			events = "(any bead event)"
		}
		out := events
		if n := countJSONArray(f["conditions"]); n > 0 {
			out += " · " + strconv.Itoa(n) + " conditions"
		}
		return out
	case "account":
		ct := strings.TrimSpace(f["condition_type"])
		fld := strings.TrimSpace(f["field"])
		op := strings.TrimSpace(f["op"])
		thresh := strings.TrimSpace(f["threshold"])
		var parts []string
		if ct != "" {
			parts = append(parts, ct)
		}
		if fld != "" {
			parts = append(parts, fld)
		}
		if op != "" {
			parts = append(parts, op)
		}
		if thresh != "" {
			parts = append(parts, thresh)
		}
		if len(parts) == 0 {
			return "(any account event)"
		}
		return strings.Join(parts, " ")
	case "schedule":
		cron := strings.TrimSpace(row.Cron)
		if cron == "" {
			return "(no cron)"
		}
		out := "cron: " + cron
		if row.Timezone != "" {
			out += " (" + row.Timezone + ")"
		}
		switch {
		case row.NextFireRelative != "":
			out += " · next " + strings.TrimPrefix(row.NextFireRelative, "in ")
		case row.NextFireError != "":
			out += " · next: " + row.NextFireError
		}
		return out
	case "slack-reaction":
		emoji := strings.TrimSpace(f["emoji"])
		channel := strings.TrimSpace(f["channel_filter"])
		match := strings.TrimSpace(f["match"])
		var head string
		switch {
		case emoji != "" && channel != "":
			head = ":" + emoji + ": in #" + channel
		case emoji != "":
			head = ":" + emoji + ":"
		case channel != "":
			head = "in #" + channel
		default:
			head = "(any reaction)"
		}
		if match != "" {
			head += ` (substring "` + match + `")`
		}
		return head
	}
	return ""
}

// triggerDestinationSummary renders the action-specific "what happens when it
// fires?" answer (design bundle ka-0000003886, rank 6).
func triggerDestinationSummary(row triggerRowView, f map[string]string) string {
	switch row.Action {
	case "spawn_agent":
		role := strings.TrimSpace(row.Role)
		project := strings.TrimSpace(row.Project)
		switch {
		case role != "" && project != "":
			return "→ spawn role=" + role + " (" + project + ")"
		case role != "":
			return "→ spawn role=" + role
		case project != "":
			return "→ spawn (" + project + ")"
		}
		return "→ spawn agent"
	case "apply_formula":
		fid := strings.TrimSpace(f["formula_id"])
		if fid == "" {
			return "→ apply formula"
		}
		return "→ formula " + fid
	case "create_bead":
		bt := strings.TrimSpace(f["bead_type"])
		bp := strings.TrimSpace(f["bead_priority"])
		switch {
		case bt != "" && bp != "":
			return "→ create type=" + bt + " priority=" + bp
		case bt != "":
			return "→ create type=" + bt
		case bp != "":
			return "→ create priority=" + bp
		}
		return "→ create bead"
	case "notify":
		return "→ notify Slack"
	}
	if row.Action != "" {
		return "→ " + row.Action
	}
	return ""
}

// triggerActivity returns the freshest of last_fired / last_run /
// last_checked as a relative-time string ("12s", "3h"), the source kind
// label, and the parsed time used for sorting. Returns ("", "", zero) when
// no activity timestamps are set.
func triggerActivity(f map[string]string, now time.Time) (string, string, time.Time) {
	candidates := []struct {
		kind  string
		field string
	}{
		{"fired", "last_fired"},
		{"run", "last_run"},
		{"checked", "last_checked"},
	}
	var (
		best     time.Time
		bestKind string
	)
	for _, c := range candidates {
		raw := strings.TrimSpace(f[c.field])
		if raw == "" {
			continue
		}
		t, ok := parseTriggerTime(raw)
		if !ok {
			continue
		}
		if t.After(best) {
			best = t
			bestKind = c.kind
		}
	}
	if best.IsZero() {
		return "", "", time.Time{}
	}
	return fmtTimeAgo(best, now), bestKind, best
}

// parseTriggerTime accepts RFC3339 and a few common variants seen in trigger
// fields. Returns (zero, false) when none match.
func parseTriggerTime(raw string) (time.Time, bool) {
	for _, layout := range []string{
		time.RFC3339Nano,
		time.RFC3339,
		"2006-01-02 15:04:05",
		"2006-01-02T15:04:05",
		"2006-01-02",
	} {
		if t, err := time.Parse(layout, raw); err == nil {
			return t, true
		}
	}
	return time.Time{}, false
}

// countJSONArray returns the number of top-level elements in a JSON array
// string, or 0 if the string is empty or not a valid JSON array.
func countJSONArray(raw string) int {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return 0
	}
	var arr []json.RawMessage
	if err := json.Unmarshal([]byte(raw), &arr); err != nil {
		return 0
	}
	return len(arr)
}

func parseInt(s string) int {
	n, err := strconv.Atoi(strings.TrimSpace(s))
	if err != nil {
		return 0
	}
	return n
}

func pickNonEmpty(a, b string) string {
	if strings.TrimSpace(a) != "" {
		return a
	}
	return b
}

// triggerActiveSortKey returns 0 for live (enabled + not closed), 1 otherwise.
func triggerActiveSortKey(r triggerRowView) int {
	if r.Enabled && !r.IsClosed {
		return 0
	}
	return 1
}

func parseTriggerFilter(q string) triggerFilter {
	switch strings.TrimSpace(q) {
	case "enabled":
		return triggerFilterEnabled
	case "disabled":
		return triggerFilterDisabled
	}
	return triggerFilterAll
}

func parseTriggerSort(q string) triggerSort {
	switch strings.TrimSpace(q) {
	case "cron":
		return triggerSortCron
	case "next-fire", "next_fire", "nextfire":
		return triggerSortNextFire
	case "last-fired", "last_fired", "lastfired":
		return triggerSortLastFired
	case "fire-count", "fire_count", "firecount":
		return triggerSortFireCount
	case "source", "":
		return triggerSortSource
	}
	return triggerSortSource
}

func parseTriggerDensity(q string) triggerDensity {
	switch strings.TrimSpace(q) {
	case "compact":
		return triggerDensityCompact
	}
	return triggerDensityComfortable
}

func triggerFilterString(f triggerFilter) string {
	switch f {
	case triggerFilterEnabled:
		return "enabled"
	case triggerFilterDisabled:
		return "disabled"
	}
	return ""
}

func triggerSortString(s triggerSort) string {
	switch s {
	case triggerSortCron:
		return "cron"
	case triggerSortNextFire:
		return "next-fire"
	case triggerSortLastFired:
		return "last-fired"
	case triggerSortFireCount:
		return "fire-count"
	}
	return ""
}

func triggerDensityString(d triggerDensity) string {
	if d == triggerDensityCompact {
		return "compact"
	}
	return ""
}

// triggerHrefOpts is the parameter bag for triggerHref. Each non-empty field
// becomes a query parameter; empty fields are omitted to keep canonical-
// default URLs short.
type triggerHrefOpts struct {
	Filter  string
	Sort    string
	Density string
	Source  string
	Action  string
}

// triggerHref builds a /triggers URL with the given options. Empty values are
// omitted (canonical defaults).
func triggerHref(basePath string, opts triggerHrefOpts) string {
	var b strings.Builder
	b.WriteString(basePath)
	b.WriteString("/triggers")
	first := true
	add := func(k, v string) {
		if first {
			b.WriteByte('?')
			first = false
		} else {
			b.WriteByte('&')
		}
		b.WriteString(k)
		b.WriteByte('=')
		b.WriteString(queryEscape(v))
	}
	if opts.Filter != "" {
		add("filter", opts.Filter)
	}
	if opts.Sort != "" {
		add("sort", opts.Sort)
	}
	if opts.Density != "" {
		add("density", opts.Density)
	}
	if opts.Source != "" {
		add("source", opts.Source)
	}
	if opts.Action != "" {
		add("action", opts.Action)
	}
	return b.String()
}
