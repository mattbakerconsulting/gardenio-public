// Package pausegate is the single source of truth for the fleet-wide pause
// gates — the gardeners-pause / scheduler-pause storm-guards — and the tiny
// find/open core that operates them.
//
// Two surfaces mutate these gates: the Slack `/schedule` command
// (internal/bridge/bot_schedule_cmd.go) and the /plans index master switch
// (cmd/beads-viewer). Before this package each surface would have had to
// re-derive "how do I open the gardeners-pause gate" — the exact reimplementation
// the master-switch task (ka-0000004048) was told NOT to do. Both now find/open
// the gate through this one core over the same well-known titles, so the gate a
// gardener's pre-fire check reads (bridge.gardenersPauseEngaged) is byte-identical
// to the one an operator toggles from either surface, and the two can never
// drift.
//
// This package owns the shared, do-not-reimplement pieces: the well-known gate
// TITLES, the find-by-exact-title lookup, and the open (create the type=gate
// bead). Closing a gate is a trivial one-liner each surface issues with its OWN
// attribution convention (the bridge closes unattributed; the viewer closes via
// CloseBeadAs so the audit trail names the operator), so there is deliberately
// no Close helper — a single signature could not carry both. It also does NOT
// read the gate at fire time — that stays in bridge.gardenersPauseEngaged /
// triggers.loadGateState, whose fail-open semantics are load-bearing and
// unchanged.
package pausegate

import (
	"context"

	"gardenio/controller/internal/beadsapi"
)

// Well-known gate titles. An OPEN gate bead with one of these exact titles is
// the engaged storm-guard:
//
//   - GardenersPauseTitle halts ALL gardener firing fleet-wide — read by
//     bridge.gardenersPauseEngaged in gardenerPreFireCheck. This is the gate the
//     /plans master switch pauses/resumes: plan gardeners ARE gardeners, so
//     opening it pauses every plan gardener at once.
//   - SchedulerPauseTitle silences the cron firing of schedule-flavored trigger
//     sources (trigger:schedule + schedule-mirror) — read by
//     triggers.loadGateState. `/schedule off` opens both; the /plans master
//     switch operates only the gardeners gate but the banner surfaces either.
//   - LandsPauseTitle stops every tenant land from starting an agent (Lands
//     Track B spec §5.5, the all-lands kill switch) — read by
//     internal/lands/wall.Check at every spawn door and in
//     gardenerPreFireCheck. Unlike the two above it FAILS CLOSED: a gate that
//     cannot be read refuses the tenant spawn. Operator lands never read it.
//     gb lands pause|resume operate it.
//
// These strings are part of the operator-facing contract behind `/schedule` and
// the `kd create "<title>" --type gate` manual escape hatch. Do not change them
// without migrating every open gate bead in the fleet.
const (
	GardenersPauseTitle = "gardeners-pause"
	SchedulerPauseTitle = "scheduler-pause"
	LandsPauseTitle     = "lands-pause"
)

// Lister is the read half of Client: all Find needs, so a read-only caller
// (the Lands spawn wall) can look a gate up without a create.
type Lister interface {
	ListBeadsFiltered(ctx context.Context, q beadsapi.ListBeadsQuery) (*beadsapi.ListBeadsResult, error)
}

// Client is the minimal daemon surface the shared gate lifecycle needs (find +
// open). It is satisfied by *beadsapi.Client and by both the bridge
// (BeadClient) and beads-viewer (beadAPI) daemon interfaces, so the same core
// serves every caller without either package depending on the other. Closing is
// intentionally left to each caller's own attribution idiom (see the package
// doc), so it is not part of this interface.
type Client interface {
	Lister
	CreateBead(ctx context.Context, req beadsapi.CreateBeadRequest) (string, error)
}

// Find returns the first OPEN gate bead whose title exactly matches title, or
// nil when no such gate is engaged. The daemon narrows on the exact title
// (Titles), so any number of other open gates that mention it cannot push the
// real one off the page and read as "not engaged" (gd-i74esUHCTg); that
// matters most for the fail-closed lands-pause switch. Search stays as the
// prefilter a daemon without ?title= still applies, and the exact-title check
// below is what makes the match precise on either.
func Find(ctx context.Context, c Lister, title string) (*beadsapi.BeadDetail, error) {
	res, err := c.ListBeadsFiltered(ctx, beadsapi.ListBeadsQuery{
		Types:    []string{"gate"},
		Statuses: []string{"open", "in_progress"},
		Titles:   []string{title},
		Search:   title,
		Limit:    5,
	})
	if err != nil {
		return nil, err
	}
	if res == nil {
		return nil, nil
	}
	for _, b := range res.Beads {
		if b != nil && b.Title == title {
			return b, nil
		}
	}
	return nil, nil
}

// Open creates a gate bead with the given title, engaging the storm-guard, and
// returns the new bead id. Callers Find first for idempotency: an already-open
// gate should be left untouched rather than duplicated (the fire-time lookups
// match on title, so a second gate would be redundant, not harmful — but Find-
// first keeps the graph clean). createdBy is the operator id stamped as the
// gate's author for the audit trail.
func Open(ctx context.Context, c Client, title, createdBy string) (string, error) {
	return c.CreateBead(ctx, beadsapi.CreateBeadRequest{
		Title:     title,
		Type:      "gate",
		CreatedBy: createdBy,
	})
}
