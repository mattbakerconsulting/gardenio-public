package gardenhub

// farm.go — the Homestead plans read model (gd-L9fjdMGV7P).
//
// Where Hub keeps flat HTML fragments for the three /garden lists, Farm keeps
// the structure a farm map needs: plans -> phases -> tasks, `blocks` edges,
// count-only piles for everything closed, seed drafts, and the meadow of loose
// beads. It is served as ONE JSON snapshot plus versioned SSE patches over the
// same stream machinery (stream.go) and the same freshness model as Hub:
//
//   - the request path reads memory only (no daemon call);
//   - a daemon event marks the farm dirty (Hub forwards its single upstream
//     stream through OnEvent, so there is still ONE subscription), a trailing
//     coalesce re-reads, and a periodic reconcile repairs lost events;
//   - the diff is per entity (a plan, a seed, or one of the singleton blobs), so
//     a task changing re-sends one plan, never the whole farm.
//
// Daemon cost is bounded and tiered. The FAST tier runs on every refresh: one
// paged list of non-closed work beads (row-capped, newest first) and one list
// of the live agent beads (the gardeners, and the roster waits are checked
// against; farm_gardeners.go). The SLOW tier
// (closed counts, `blocks` edges, piles, closed-plan tiles, closed phase titles)
// runs at most every farmSlowEvery, or sooner when a bead left the open set (a
// close must not leave the closed counts stale); it keeps the closed rows and
// reads only what changed since its last read (farm_closed.go), and every row
// read is projected lean (farmRowOmit). Nothing closed is sent as a
// row except a phase's newest closed steps, capped by the cells its bed has
// left (FarmPhase.Closed, bedCells) and per plan and per document
// (maxClosedPerPlan, maxClosedListed), so the payload is bounded by the map's
// geometry and those caps, not by history.

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"sort"
	"strings"
	"sync"
	"time"

	"gardenio/controller/internal/beadsapi"
	"gardenio/controller/internal/gardenhub/captioner"
)

const (
	// farmRowCap bounds how many non-closed work beads one refresh reads (5
	// pages). Past it the farm is built from the newest rows and
	// FarmMeta.Truncated says so. 500/page matches the other bounded list reads
	// (the daemon list cap note in Hub's listLimit; gd-2fIZWEXF1X).
	farmRowCap   = 2500
	farmPageSize = 500

	// farmSlowEvery bounds how often the SLOW tier re-reads (see above).
	farmSlowEvery = 20 * time.Second
	// farmSlowBackoff is how long a failed SLOW tier waits before it is tried
	// again. A failed slow read keeps the last one (gd-NiFezSFVxz: under a
	// saturated daemon the closed-row read timed out on every refresh, and
	// failing the refresh froze the gardeners for 45 min).
	farmSlowBackoff = time.Minute

	// farmClosedRowCap bounds how many closed work beads the SLOW tier reads
	// (newest first) to list a phase's closed steps; past it the oldest closed
	// steps are count-only stubble. bedCells is a bed's 9x9 (web/world.js
	// BED.cap): a phase never lists more closed steps than its bed has cells
	// left after the open tasks and the "+N" crate.
	farmClosedRowCap = 2000
	bedCells         = 81

	// Per-entity payload bounds.
	maxPhaseTasks       = 60 // listed tasks per phase; the rest are only counted (FarmPhase.More)
	maxBlockedBy        = 8
	maxMeadowShown      = 40
	maxClosedPlansShown = 24
	maxWaiting          = 12 // listed open decisions/escalations per plan (FarmPlan.Waiting)
	maxAgentNames       = 1000
	maxNameLookups      = 20 // agent-name lookups one refresh may make (each is one list read)

	// The live-agent roster and the gardeners are re-read on every refresh.
	// Past farmRosterStale without a good read the roster is unknown (waits name
	// their party unchecked) and meta.gardeners_as_of says how old the
	// gardeners are.
	farmRosterStale = 2 * time.Minute

	farmMaxDepth = 6 // ancestor hops when resolving a task to its plan
)

// FarmSource is the slice of the daemon client the farm needs.
type FarmSource interface {
	ListBeadsFiltered(ctx context.Context, q beadsapi.ListBeadsQuery) (*beadsapi.ListBeadsResult, error)
	BatchGetBeads(ctx context.Context, ids []string) (map[string]*beadsapi.BeadDetail, error)
	BatchGetDependencies(ctx context.Context, ids []string, direction string) (map[string][]beadsapi.Dependency, error)
	BatchGetChildrenByStatus(ctx context.Context, parentIDs []string) (map[string]map[string]int, error)
}

// farmWorkTypes are the bead types read as plants and containers. Everything
// else (agent, merge-request, mail, report, ...) is not farm work.
var farmWorkTypes = []string{"task", "feature", "bug", "chore", "decision", "escalation", "epic", "molecule"}

// farmOpenStatuses are the non-closed statuses.
var farmOpenStatuses = []string{"open", "in_progress", "blocked", "deferred", "partial"}

// farmRowOmit and farmRowOmitFields are the daemon's opt-in list projection
// (gd-zXdZq4Traf) on the farm's row reads: the free-text bodies, and the
// escalation and decision payloads no farm entity is built from. Without them
// the open rows were about 2.4 MB a refresh and the closed rows 5.6 MB a slow
// tick, most of the viewer's 36.6 GB/day of GET /v1/beads (gd-2COXnx326M).
// A field the farm starts reading must come off this list.
var (
	farmRowOmit       = []string{"description", "notes"}
	farmRowOmitFields = []string{"context", "operation_spec", "prompt", "options", "execution_script", "expected_outputs", "slack_root_text"}
	// farmClosedOmitFields also drops what only an open wait reads (waitOf,
	// agentRefs): no closed row becomes a wait.
	farmClosedOmitFields = append(append([]string{}, farmRowOmitFields...), "result", "executor")
)

// meadowTypes are the leaf types that can be "loose". A decision or escalation
// with no plan is a letter for the mailbox, not a plant.
var meadowTypes = map[string]bool{"task": true, "feature": true, "bug": true, "chore": true}

type entry struct {
	raw []byte
	key int64 // sort key, newest first
}

type phaseInfo struct{ title, shortTitle, status, heraldry string }

// slowState is what the slow tier last read. It is only touched by refresh,
// which is serialised (Start's warm-up, then the loop goroutine).
type slowState struct {
	at     time.Time
	hist   map[string]map[string]int // plan/phase id -> direct children by status
	deps   map[string][]string       // task id -> ids it `blocks`-depends on
	phases map[string]phaseInfo      // closed/off-list phase containers, kept (closed is terminal)
	// closedRows are the newest closed work beads (farmClosedRowCap), placed
	// under their plan and phase at compose time by the same walk as the open
	// rows, for FarmPhase.Closed.
	closedRows []*beadsapi.BeadDetail
	piles      FarmPiles
	closed     FarmClosedPlans
	// closedWishes is the daemon's count of closed thread heads (FarmWishes.ClosedTotal).
	closedWishes int
	// orphans caches PR url -> the work bead that links it, for PRs no open
	// row carries (see readOrphans). Kept while the PR is in the open-PR read.
	orphans map[string]orphanRef
	// orphanAll is every orphaned PR found, before maxOrphanMRs (the cut
	// recounts them per project set).
	orphanAll []FarmOrphanMR
	// produce / produceMore are each plan's bin (farm_produce.go); lineage
	// caches closed container id -> parent id learned while resolving it.
	produce     map[string][]FarmProduce
	produceMore map[string]int
	wishProduce wishYield // each wish's kin's merged MRs, for the headstones (farm_produce.go wishProduce)
	lineage     map[string]string
	mrDone      map[string]mrDone // mr url -> the terminal state its MR bead records (farm_produce.go resolveMRs)
	harvest     harvestIdx        // the evidence and yield indexes behind FarmClosedStep.Harvest (farm_harvest.go)
}

// FarmStats is the farm's observability surface.
type FarmStats struct {
	Version   uint64 `json:"version"`
	Plans     int    `json:"plans"`
	Seeds     int    `json:"seeds"`
	Gardeners int    `json:"gardeners"`
	Clients   int    `json:"clients"`
	// The per-viewer cut streams (farm_cut_stream.go): one per project set in use, and the scoped viewers on them.
	CutStreams    int    `json:"cut_streams"`
	CutClients    int    `json:"cut_clients"`
	RingLen       int    `json:"ring_len"`
	Refreshes     uint64 `json:"refreshes"`
	SlowRefreshes uint64 `json:"slow_refreshes"`
	Changes       uint64 `json:"changes"`
	Resets        uint64 `json:"resets"`
	// Deltas counts the catch-ups sent as one delta patch (farm_delta.go) where the ring's replay or a reset went before.
	Deltas        uint64 `json:"deltas"`
	LastRefreshMS int64  `json:"last_refresh_ms"`
	SnapshotBytes int    `json:"snapshot_bytes"`
	LastError     string `json:"last_error,omitempty"`
	// The activity feed (activity.go): its viewers, the events and bytes (as written on the wire) queued to them, those a
	// full queue refused, and the views the pages declared (gd-fBGGNdgrLc).
	ActivityClients int    `json:"activity_clients"`
	ActivitySent    uint64 `json:"activity_sent"`
	ActivityBytes   uint64 `json:"activity_bytes"`
	ActivityDropped uint64 `json:"activity_dropped"`
	ActivityViews   uint64 `json:"activity_views"`
	// The stream budget (gd-iAq4va9XDt): per fold (ActivityFolds), the streams on it and the bytes of calls sent at it.
	ActivityFoldClients [activityFoldTiers]int    `json:"activity_fold_clients"`
	ActivityFoldBytes   [activityFoldTiers]uint64 `json:"activity_fold_bytes"`
	// The transcripts (transcript.go): rows written to the rings, the streams holding one open and the bytes of the
	// call events sent to them.
	TranscriptRows    uint64 `json:"transcript_rows"`
	TranscriptClients int    `json:"transcript_clients"`
	TranscriptBytes   uint64 `json:"transcript_bytes"`
}

// Farm holds the farm snapshot and its subscribers.
type Farm struct {
	src         FarmSource
	logger      *slog.Logger
	dirty       chan struct{} // a work row may have changed: full refresh
	rosterDirty chan struct{} // only an agent changed: roster-only refresh

	mu          sync.Mutex
	st          *stream
	dl          *deltaLog // st's change log, for the delta catch-up (farm_delta.go)
	plans       map[string]entry
	seeds       map[string]entry
	gardeners   map[string]entry
	closedPlans []byte
	meadow      []byte
	wishes      []byte // FarmWishes (farm_wishes.go)
	home        []byte // FarmHomes (farm_home.go)
	escalations []byte // FarmEscalations (farm_escalations.go)
	releases    []byte // FarmReleases (farm_releases.go)
	piles       []byte
	meta        []byte
	cost        []byte // nil when no spend source is wired
	// drawn is every project the farm draws something for (a plan, a seed or
	// a loose bead carries its project label): with the cost rows, the
	// projects KnowsProject answers for.
	drawn   map[string]bool
	builtAt time.Time
	memo    struct {
		v   uint64
		act uint64    // the activity epoch gardeners[].doing was baked at
		at  time.Time // when
		b   []byte
		gz  []byte // gzip of b, built on first request that accepts it
	}
	// act is every agent's last-seen activity (activity.go): gardeners[].doing and the SSE activity events.
	act *activity
	// line caches the line arms of the wishes' kin (line_arms.go); ShareLineArms makes it the hub's.
	line *lineArms
	// capt is the Haiku fallback for a gardener's Bash call with no description (SetCaptioner); nil: none, and no
	// caption counted.
	capt *captioner.Captioner
	// capOff is FARM_CAPTIONS=off (SetCaptionSettings): every event goes out without a caption, as before captions.
	capOff                            bool
	refreshes, slowRefreshes, changes uint64
	lastRefresh                       time.Duration
	lastErr                           string
	// slowFailedAt is when the SLOW tier last failed, zero after a good one;
	// it is not retried before farmSlowBackoff has passed.
	slowFailedAt time.Time

	prevIDs map[string]struct{}
	slow    slowState
	// The last readRows result, reused by roster-only refreshes. Touched only
	// by refresh, which is serialised (Start's warm-up, then loop).
	rows          []*beadsapi.BeadDetail
	rowsTruncated bool
	rowsAt        time.Time
	// phaseEdges is bead id -> phase id for rows whose primary parent is a
	// plan epic but which also carry a parent-child edge to one of that
	// plan's phases (see readPhaseEdges). Rebuilt with rows; a roster-only
	// refresh reuses it.
	phaseEdges map[string]string
	// agentNames caches agent bead id -> what that bead said when read, for the
	// waits_on of an escalation or decision that names its agent by bead id. An
	// agent bead's name never changes, so an entry is never re-read; whether the
	// agent is still live comes from roster.
	agentNames map[string]agentRef
	// agentByName caches agent name -> whether some agent bead carries that name
	// (and whether it looked finished when read). Never re-read either.
	agentByName map[string]agentRef
	// seedAsks caches an open seed decision's prompt and options by decision
	// id (farm_seeds.go), dropped once the decision closes.
	seedAsks map[string]seedAsk
	// seedNarrs caches a ready or asked seed's summary by plan id
	// (farm_seeds.go), re-read when the plan's updated_at moves and dropped once
	// it is no longer such a seed.
	seedNarrs map[string]seedNarr
	// refTitles caches a bead a gardener names (its task, send, escalation, or
	// a thread agent's head) not among the rows read (farm_gardeners.go
	// readRefTitles): its title and place in the line (gardenerArms), empty
	// for one the read did not find. Kept only for beads live gardeners name.
	refTitles map[string]refBead
	// runSpend is the gardener triggers' spend rollup the gardeners' cost
	// carries (farm_gardener_cost.go readRunSpend).
	runSpend runSpendState
	roster   agentRoster
	// gs is what the last agent read saw: the gardener set kept across a
	// failed read, and the previous entries an exit is described from.
	gs gardenerState
	// spend is the costs rollup behind the cost blob (farm_cost.go); nil = no
	// cost blob. costSt is what the last cost read saw.
	spend  SpendSource
	costSt costState
	// rel reads the releases (farm_releases.go, SetReleaseSource); nil: no
	// releases blob read. relSt is what the last releases read kept.
	rel   ReleaseSource
	relSt releaseState
	// closedSt is the closed-row set the slow tier keeps between reads
	// (farm_closed.go).
	closedSt closedState
	// The per-viewer cut (farm_cut.go): what the last refresh kept for it, its
	// generation (bumped whenever it or an archive is replaced), the cut streams
	// by project set (farm_cut_stream.go) and the counter their versions come
	// from, and the per-project archives scoped requests asked for (archWant:
	// project -> last asked).
	cutSt    *farmCutState
	cutGen   uint64
	cuts     map[string]*cutStream
	cutSeq   uint64
	arch     map[string]projectArchive
	archWant map[string]time.Time
	now      func() time.Time // test seam; time.Now in production

	cancel context.CancelFunc
	done   chan struct{}
}

// NewFarm builds a farm over src.
func NewFarm(src FarmSource, logger *slog.Logger) *Farm {
	if logger == nil {
		logger = slog.Default()
	}
	f := &Farm{
		src:         src,
		logger:      logger,
		dirty:       make(chan struct{}, 1),
		rosterDirty: make(chan struct{}, 1),
		// Wall-clock version base, as in Hub: a restarted process always issues
		// versions above anything a previous incarnation handed out.
		st: newStream(uint64(time.Now().UnixMilli())),
		// The cut streams count in microseconds from the same wall clock, so a
		// cut's version is never one the whole farm's stream issued (still below
		// 2^53, a JavaScript number) nor one a previous incarnation issued.
		cutSeq:      uint64(time.Now().UnixMicro()),
		plans:       map[string]entry{},
		seeds:       map[string]entry{},
		gardeners:   map[string]entry{},
		prevIDs:     map[string]struct{}{},
		slow:        slowState{phases: map[string]phaseInfo{}, orphans: map[string]orphanRef{}},
		agentNames:  map[string]agentRef{},
		agentByName: map[string]agentRef{},
		seedAsks:    map[string]seedAsk{},
		seedNarrs:   map[string]seedNarr{},
		refTitles:   map[string]refBead{},
		runSpend:    runSpendState{usd: map[string]*float64{}},
		act:         newActivity(),
		line:        newLineArms(),
		now:         time.Now,
		done:        make(chan struct{}),
	}
	f.dl = newDeltaLog(f.st.version, cutView{})
	return f
}

// ---- reading the daemon ---------------------------------------------------

// readRows pages the non-closed work beads, newest first, up to farmRowCap.
func (f *Farm) readRows(ctx context.Context) (rows []*beadsapi.BeadDetail, truncated bool, err error) {
	seen := map[string]struct{}{}
	for off := 0; off < farmRowCap; off += farmPageSize {
		res, err := f.src.ListBeadsFiltered(ctx, beadsapi.ListBeadsQuery{
			Types:            farmWorkTypes,
			Statuses:         farmOpenStatuses,
			ExcludeScheduled: true,
			WithParent:       true,
			// Newest first so a truncated read drops the oldest rows. created_at
			// is immutable: a concurrent insert shifts pages by whole rows (a
			// duplicate the dedup below drops), never skips one.
			Sort:       "-created_at",
			Limit:      farmPageSize,
			Offset:     off,
			Omit:       farmRowOmit,
			OmitFields: farmRowOmitFields,
			// A short page ends the walk; a full page's COUNT(*) is never
			// read (gd-XOi5cqZP3I).
			NoTotal: true,
		})
		if err != nil {
			return nil, false, err
		}
		for _, b := range res.Beads {
			if b == nil {
				continue
			}
			if _, dup := seen[b.ID]; dup {
				continue
			}
			seen[b.ID] = struct{}{}
			rows = append(rows, b)
		}
		if len(res.Beads) < farmPageSize {
			return rows, false, nil
		}
	}
	return rows, true, nil
}

func (f *Farm) count(ctx context.Context, q beadsapi.ListBeadsQuery) (int, error) {
	q.CountOnly = true
	res, err := f.src.ListBeadsFiltered(ctx, q)
	if err != nil {
		return 0, err
	}
	return res.Total, nil
}

// refreshSlow re-reads the slow tier.
func (f *Farm) refreshSlow(ctx context.Context, histIDs, depIDs, phaseIDs []string, byID map[string]*beadsapi.BeadDetail) error {
	hist, err := f.src.BatchGetChildrenByStatus(ctx, histIDs)
	if err != nil {
		return err
	}
	deps := map[string][]string{}
	if len(depIDs) > 0 {
		all, err := f.src.BatchGetDependencies(ctx, depIDs, "forward")
		if err != nil {
			return err
		}
		for id, ds := range all {
			for _, d := range ds {
				if d.Type == "blocks" && d.DependsOnID != "" {
					deps[id] = append(deps[id], d.DependsOnID)
				}
			}
		}
	}
	var missing []string
	for _, id := range phaseIDs {
		if _, ok := byID[id]; ok {
			continue
		}
		if _, ok := f.slow.phases[id]; ok {
			continue
		}
		missing = append(missing, id)
	}
	phases := f.slow.phases
	if len(missing) > 0 {
		got, err := f.src.BatchGetBeads(ctx, missing)
		if err != nil {
			return err
		}
		for id, b := range got {
			// Only a closed phase is cached: it is terminal. An open phase that
			// missed the row read (cap) is re-fetched each slow tick.
			if b != nil && b.Status == "closed" {
				phases[id] = phaseInfo{title: b.Title, shortTitle: strings.TrimSpace(b.ShortTitle), status: b.Status, heraldry: b.Heraldry}
			}
		}
		// Open-but-unlisted phases still need a title this tick.
		for id, b := range got {
			if b != nil && b.Status != "closed" {
				byID[id] = b
			}
		}
	}

	var piles FarmPiles
	if piles.ClosedDecisions, err = f.count(ctx, beadsapi.ListBeadsQuery{Types: []string{"decision"}, Statuses: []string{"closed"}}); err != nil {
		return err
	}
	if piles.ClosedEscalations, err = f.count(ctx, beadsapi.ListBeadsQuery{Types: []string{"escalation"}, Statuses: []string{"closed"}}); err != nil {
		return err
	}
	if piles.ClosedWork, err = f.count(ctx, beadsapi.ListBeadsQuery{Types: []string{"task", "feature", "bug", "chore"}, Statuses: []string{"closed"}}); err != nil {
		return err
	}
	closed, err := f.readClosedPlans(ctx)
	if err != nil {
		return err
	}
	piles.ClosedPlans = closed.Total
	closedWishes, err := f.countClosedWishes(ctx)
	if err != nil {
		return err
	}

	closedRows, closedTruncated, err := f.readClosedRows(ctx)
	if err != nil {
		return err
	}
	if closedTruncated {
		f.logger.Warn("garden farm: closed-step read hit the row cap", "cap", farmClosedRowCap)
	}

	orphans, err := f.readOrphans(ctx, byID)
	if err != nil {
		return err
	}
	piles.OrphanMRTotal = len(orphans)
	orphanAll := orphans
	if len(orphans) > maxOrphanMRs {
		orphans = orphans[:maxOrphanMRs]
	}
	piles.OrphanMRs = orphans

	f.slow = slowState{at: f.now(), hist: hist, deps: deps, phases: phases, closedRows: closedRows, piles: piles, closed: closed, closedWishes: closedWishes, orphans: f.slow.orphans, orphanAll: orphanAll,
		produce: f.slow.produce, produceMore: f.slow.produceMore, wishProduce: f.slow.wishProduce, lineage: f.slow.lineage, mrDone: f.slow.mrDone, harvest: f.slow.harvest}
	return nil
}

// readPhaseEdges finds, for every row whose primary parent is a plan epic and
// that is not itself one of that plan's phases, a parent-child edge to a phase
// of the SAME plan, and returns bead id -> that phase id.
//
// A bulk re-parent that adds a direct task -> plan edge on every step of a
// plan (the Lands split, gd-RCwGfF1Z4k) leaves each step with two open epic
// parents; the daemon's primary-parent pick takes the newest edge, so every
// step reports the plan and the phase beds read as empty. One batched forward
// deps read over the candidates settles it; in healthy data the candidates
// are only the plan's unphased steps. A failed read keeps the last map rather
// than failing the refresh. Ties between several same-plan phase edges go to
// the first in phase_order.
func (f *Farm) readPhaseEdges(ctx context.Context, rows []*beadsapi.BeadDetail, byID map[string]*beadsapi.BeadDetail, planSet map[string]*beadsapi.BeadDetail, phaseIdx map[string]string, order map[string][]string) map[string]string {
	var ids []string
	for _, b := range rows {
		if _, isPhase := phaseIdx[b.ID]; isPhase {
			continue
		}
		if _, ok := planSet[b.ParentID]; ok {
			ids = append(ids, b.ID)
		}
	}
	if len(ids) == 0 {
		return map[string]string{}
	}
	all, err := f.src.BatchGetDependencies(ctx, ids, "forward")
	if err != nil {
		f.logger.Warn("garden farm: reading phase edges failed; keeping the last read", "beads", len(ids), "error", err)
		return f.phaseEdges
	}
	out := map[string]string{}
	for _, id := range ids {
		pid := byID[id].ParentID
		edges := map[string]struct{}{}
		for _, d := range all[id] {
			if d.Type == "parent-child" && phaseIdx[d.DependsOnID] == pid {
				edges[d.DependsOnID] = struct{}{}
			}
		}
		if len(edges) == 0 {
			continue
		}
		for _, ph := range order[pid] {
			if _, ok := edges[ph]; ok {
				out[id] = ph
				break
			}
		}
	}
	return out
}

// ---- building the model ---------------------------------------------------

type bucket struct {
	counts FarmCounts
	tasks  []FarmTask
	full   []FarmTask // tasks sorted, before maxPhaseTasks (the per-viewer cut's)
}

// refreshOnce rebuilds the snapshot. readRows false (an agent-only change)
// reuses the last work rows; nothing below writes into them.
//
// Only a first work-row read that fails fails the refresh: there is nothing
// to draw yet. Past it, a failed work-row read or SLOW tier keeps the last
// good read and the snapshot is still built, so the gardeners (one cheap read
// of the live agents) never wait on the heavier reads; the error is returned
// after the snapshot is published.
func (f *Farm) refreshOnce(ctx context.Context, readRows bool) error {
	rows, truncated := f.rows, f.rowsTruncated
	rowsRead := false
	var degraded error
	if readRows || f.rowsAt.IsZero() {
		got, gotTruncated, err := f.readRows(ctx)
		switch {
		case err == nil:
			rows, truncated = got, gotTruncated
			f.rows, f.rowsTruncated, f.rowsAt = rows, truncated, f.now()
			rowsRead = true
		case f.rowsAt.IsZero():
			return err
		default:
			f.logger.Warn("garden farm: reading the work rows failed; keeping the last read", "error", err)
			degraded = fmt.Errorf("work rows: %w", err)
		}
	}
	// One agent read feeds both the roster and the gardeners. It never fails
	// the refresh: a bad read keeps the last good roster and gardener set.
	agents, agentsErr := f.readAgents(ctx)
	if agentsErr != nil {
		f.logger.Warn("garden farm: reading the live agents failed", "error", agentsErr)
	} else {
		f.setRoster(agents)
	}
	byID := make(map[string]*beadsapi.BeadDetail, len(rows))
	cur := make(map[string]struct{}, len(rows))
	for _, b := range rows {
		byID[b.ID] = b
		cur[b.ID] = struct{}{}
	}

	// Plans and phase membership.
	planSet := map[string]*beadsapi.BeadDetail{}
	var planIDs []string
	for _, b := range rows {
		if b.Type == "epic" && hasLabel(b, "plan") {
			planSet[b.ID] = b
			planIDs = append(planIDs, b.ID)
		}
	}
	sort.Strings(planIDs)
	phaseIdx := map[string]string{} // phase id -> plan id
	order := map[string][]string{}
	var phaseIDs []string
	for _, pid := range planIDs {
		order[pid] = phaseOrderOf(planSet[pid])
		for _, ph := range order[pid] {
			if _, taken := phaseIdx[ph]; taken {
				continue
			}
			phaseIdx[ph] = pid
			phaseIDs = append(phaseIDs, ph)
		}
	}

	if rowsRead {
		f.phaseEdges = f.readPhaseEdges(ctx, rows, byID, planSet, phaseIdx, order)
	}

	// resolve walks a bead's ancestry to the first phase or plan. ParentID is
	// the daemon's ONE primary parent (open over closed, epic over non-epic,
	// then the newest edge), so a step parented to both its phase and the
	// plan reports the plan; at every hop a same-plan phase edge read by
	// readPhaseEdges wins over that, so the step (and anything under it)
	// lands in its phase bed, not in Other (gd-RCwGfF1Z4k).
	resolve := func(b *beadsapi.BeadDetail) (plan, phase string) {
		cur := b
		for depth := 0; cur != nil && depth < farmMaxDepth; depth++ {
			if ph, ok := f.phaseEdges[cur.ID]; ok {
				if pid, ok := phaseIdx[ph]; ok {
					return pid, ph
				}
			}
			at := cur.ParentID
			if at == "" {
				break
			}
			if pid, ok := phaseIdx[at]; ok {
				return pid, at
			}
			if _, ok := planSet[at]; ok {
				return at, ""
			}
			cur = byID[at]
		}
		return "", ""
	}

	// plotOf is where a gardener whose task is id stands: the plot its task is
	// drawn on, found by the same walk over the same rows. A seed is a pot in
	// the nursery, not a plot; a closed task, a meadow bead or a bead not read
	// is at home (nil).
	plotOf := func(id string) *FarmPlot {
		b := byID[id]
		if b == nil {
			return nil
		}
		var plan, phase string
		if _, ok := planSet[id]; ok {
			plan = id
		} else if pid, ok := phaseIdx[id]; ok {
			plan, phase = pid, id
		} else {
			plan, phase = resolve(b)
		}
		if plan == "" {
			return nil
		}
		if st := strings.TrimSpace(planSet[plan].Fields["plan_state"]); st == "draft" || st == "ready" {
			return nil
		}
		return &FarmPlot{Plan: plan, Phase: phase}
	}
	isContainer := func(id string) bool {
		_, plan := planSet[id]
		_, phase := phaseIdx[id]
		return plan || phase
	}

	type placed struct {
		b           *beadsapi.BeadDetail
		plan, phase string
	}
	var inPlans []placed
	var loose, asks, wishes []*beadsapi.BeadDetail
	var depIDs []string
	for _, b := range rows {
		if _, isPlan := planSet[b.ID]; isPlan {
			continue
		}
		if _, isPhase := phaseIdx[b.ID]; isPhase {
			continue
		}
		if isWish(b) {
			// A thread head is a wish, not loose work: never a meadow flower, never a
			// plant on a plot (farm_wishes.go).
			wishes = append(wishes, b)
			continue
		}
		pl, ph := resolve(b)
		if pl == "" {
			if meadowTypes[b.Type] {
				loose = append(loose, b)
			} else if b.Type == "escalation" {
				asks = append(asks, b) // a letter with no plan: the Escalations cell names its party too
			}
			continue
		}
		inPlans = append(inPlans, placed{b, pl, ph})
		depIDs = append(depIDs, b.ID)
		if b.Type == "decision" || b.Type == "escalation" {
			asks = append(asks, b)
		}
	}

	// Slow tier.
	removed := false
	for id := range f.prevIDs {
		if _, ok := cur[id]; !ok {
			removed = true
			break
		}
	}
	slowRan := false
	slowDue := f.slow.hist == nil || removed || f.now().Sub(f.slow.at) >= farmSlowEvery
	if slowDue && !f.slowFailedAt.IsZero() && f.now().Sub(f.slowFailedAt) < farmSlowBackoff {
		slowDue = false
	}
	if slowDue {
		histIDs := append(append([]string{}, planIDs...), phaseIDs...)
		if err := f.refreshSlow(ctx, histIDs, depIDs, phaseIDs, byID); err != nil {
			// refreshSlow writes f.slow only once every read is in, so the last
			// good slow read stands.
			f.slowFailedAt = f.now()
			f.logger.Warn("garden farm: slow tier read failed; keeping the last read", "error", err, "retry_in", farmSlowBackoff)
			if degraded == nil {
				degraded = fmt.Errorf("slow tier: %w", err)
			}
		} else {
			f.slowFailedAt = time.Time{}
			slowRan = true
		}
	}
	if slowRan {
		f.readYields(ctx, planSet, phaseIdx, byID) // the produce bins and the evidence index (farm_harvest.go); never fails the refresh
		if f.spend != nil && (f.costSt.at.IsZero() || f.now().Sub(f.costSt.at) >= farmCostEvery) {
			f.refreshCost(ctx, rows)
		}
		if f.rel != nil && (f.relSt.at.IsZero() || f.now().Sub(f.relSt.at) >= farmReleasesEvery) {
			f.refreshReleases(ctx)
		}
	}
	f.prevIDs = cur

	// Compose.
	buckets := map[string]map[string]*bucket{} // plan -> phase ("" = unphased) -> bucket
	bk := func(plan, phase string) *bucket {
		m := buckets[plan]
		if m == nil {
			m = map[string]*bucket{}
			buckets[plan] = m
		}
		b := m[phase]
		if b == nil {
			b = &bucket{}
			m[phase] = b
		}
		return b
	}
	f.resolveAgents(ctx, asks)
	var agentRows []*beadsapi.BeadDetail
	agentsFresh := agentsErr == nil && !agents.MayHaveMore()
	if agentsFresh {
		agentRows = agents.Beads
	}
	execs := escalationExecutors(agentRows, agentsFresh, f.gs.byID)
	activity := map[string]int64{}
	decisions, escalations := map[string]int{}, map[string]int{}
	waits := map[string][]FarmWait{}
	stepsOf := map[string][]*beadsapi.BeadDetail{} // plan -> its open work beads, a seed's steps
	for _, p := range inPlans {
		if meadowTypes[p.b.Type] {
			stepsOf[p.plan] = append(stepsOf[p.plan], p.b)
		}
		ft := f.task(p.b, byID)
		bk(p.plan, p.phase).counts.add(ft.State)
		bk(p.plan, p.phase).tasks = append(bk(p.plan, p.phase).tasks, ft)
		if ft.Updated > activity[p.plan] {
			activity[p.plan] = ft.Updated
		}
		var w FarmWait
		switch p.b.Type {
		case "decision":
			decisions[p.plan]++
			w = waitOf(p.b, f.party)
		case "escalation":
			escalations[p.plan]++
			w, _ = escalationWait(p.b, f.party, execs)
		default:
			continue
		}
		waits[p.plan] = append(waits[p.plan], w)
	}

	phaseMeta := func(id string) phaseInfo {
		if b := byID[id]; b != nil {
			return phaseInfo{title: b.Title, shortTitle: strings.TrimSpace(b.ShortTitle), status: b.Status, heraldry: b.Heraldry}
		}
		if pi, ok := f.slow.phases[id]; ok {
			return pi
		}
		return phaseInfo{title: id, status: "unknown"}
	}
	finish := func(b *bucket, closed int) ([]FarmTask, int, FarmCounts) {
		if b == nil {
			return []FarmTask{}, 0, FarmCounts{Closed: closed}
		}
		sortTasks(b.tasks)
		b.full = b.tasks
		more := 0
		if len(b.tasks) > maxPhaseTasks {
			more = len(b.tasks) - maxPhaseTasks
			b.tasks = b.tasks[:maxPhaseTasks]
		}
		sortPlanted(b.tasks)
		c := b.counts
		c.Closed = closed
		return b.tasks, more, c
	}

	closedBy := f.placeClosed(planSet, phaseIdx, buckets, resolve)

	nextPlans := map[string]entry{}
	nextSeeds := map[string]entry{}
	cut := &farmCutState{plans: map[string]*cutPlan{}, seeds: map[string]*cutSeed{}}
	drawn := map[string]bool{}
	for _, b := range loose {
		if p := projectOf(b); p != "" {
			drawn[p] = true
		}
	}
	seeds := seedPlans(planIDs, planSet)
	seedAsks := f.seedDecisions(ctx, rows, seeds)
	summaries := f.seedSummaries(ctx, seeds, seedAsks)
	spendPassAt := planSpendPassAt(planSet, f.now())
	for _, pid := range planIDs {
		pb := planSet[pid]
		state := strings.TrimSpace(pb.Fields["plan_state"])
		act := pb.UpdatedAt.Unix()
		if activity[pid] > act {
			act = activity[pid]
		}
		if state == "draft" || state == "ready" {
			seed := FarmSeed{
				ID: pid, Title: pb.Title, Status: pb.Status, Project: projectOf(pb),
				Operator: lower(pb.OperatedBy), Priority: pb.Priority, Created: pb.CreatedAt.Unix(),
				Updated: act, Phases: len(order[pid]), Cost: slimCostOf(pb, f.now()),
			}
			seed.State, seed.Decision = seedState(state, seedAsks[pid])
			if seed.State != SeedFrosted { // a draft keeps counts only, so 20+ stay cheap
				seed.Steps, seed.Summary = seedSteps(pb, stepsOf[pid]), summaries[pid]
				seed.ShortTitle = strings.TrimSpace(pb.ShortTitle)
			}
			if seed.Project != "" {
				drawn[seed.Project] = true
			}
			for _, b := range buckets[pid] {
				seed.Tasks += b.counts.InProgress + b.counts.Review + b.counts.Blocked + b.counts.Open + b.counts.Deferred
			}
			raw, _ := json.Marshal(seed)
			nextSeeds[pid] = entry{raw: raw, key: act}
			cut.seeds[pid] = &cutSeed{seed: seed, raw: raw}
			continue
		}

		plan := FarmPlan{
			ID: pid, Title: pb.Title, ShortTitle: strings.TrimSpace(pb.ShortTitle), State: state, Status: pb.Status, Project: projectOf(pb),
			Operator: lower(pb.OperatedBy), Owner: pb.Owner, Priority: pb.Priority,
			Created: pb.CreatedAt.Unix(), Updated: pb.UpdatedAt.Unix(), Activity: act, Heraldry: pb.Heraldry, HeraldryName: armsName(pb.Heraldry),
			Phases:        make([]FarmPhase, 0, len(order[pid])),
			OpenDecisions: decisions[pid], OpenEscalations: escalations[pid],
			Cost: planCostAt(pb, f.now(), spendPassAt),
		}
		plan.NeedsHuman, plan.Waiting = finishWaits(waits[pid])
		if plan.Project != "" {
			drawn[plan.Project] = true
		}
		plan.Produce, plan.ProduceMore = f.slow.produce[pid], f.slow.produceMore[pid]
		plan.Shipped, plan.Boarded = f.relSt.shipped.of(pid, plan.Project, plan.Produce)
		closedPhases := 0
		for _, ph := range order[pid] {
			if phaseIdx[ph] != pid {
				continue // claimed by an earlier plan
			}
			pi := phaseMeta(ph)
			if pi.status == "closed" {
				closedPhases++
			}
			tasks, more, counts := finish(buckets[pid][ph], f.slow.hist[ph]["closed"])
			plan.Phases = append(plan.Phases, FarmPhase{ID: ph, Title: pi.title, ShortTitle: pi.shortTitle, Status: pi.status, Counts: counts, Tasks: tasks, More: more, Heraldry: pi.heraldry, HeraldryName: armsName(pi.heraldry), Closed: closedBy[pid+"\x00"+ph]})
			plan.Counts.sum(counts)
		}
		unClosed := f.slow.hist[pid]["closed"] - closedPhases
		if unClosed < 0 {
			unClosed = 0
		}
		tasks, more, counts := finish(buckets[pid][""], unClosed)
		plan.Unphased = FarmPhase{ID: "", Title: "Other", Status: "", Counts: counts, Tasks: tasks, More: more, Closed: closedBy[pid+"\x00"]}
		plan.Counts.sum(counts)
		f.harvestPlan(&plan)
		raw, _ := json.Marshal(plan)
		nextPlans[pid] = entry{raw: raw, key: act}
		cp := &cutPlan{plan: plan, raw: raw, full: map[string][]FarmTask{}, waits: waits[pid], execs: execs}
		for ph, b := range buckets[pid] {
			cp.full[ph] = b.full
		}
		cut.plans[pid] = cp
	}

	// Meadow.
	sort.Slice(loose, func(i, j int) bool {
		if !loose[i].UpdatedAt.Equal(loose[j].UpdatedAt) {
			return loose[i].UpdatedAt.After(loose[j].UpdatedAt)
		}
		return loose[i].ID < loose[j].ID
	})
	meadow := FarmMeadow{Total: len(loose), Shown: []FarmTask{}}
	cut.loose = make([]cutLoose, 0, len(loose))
	for i, b := range loose {
		ft := f.task(b, nil)
		cl := cutLoose{t: ft}
		cl.t.Parent = b.ParentID
		if p := byID[b.ParentID]; p != nil {
			cl.parentProj, cl.parentKnown = projectOf(p), true
		}
		cut.loose = append(cut.loose, cl)
		if ft.MR != nil {
			meadow.WithMR++
		}
		if i < maxMeadowShown {
			ft.Parent = b.ParentID
			meadow.Shown = append(meadow.Shown, ft)
		}
	}
	meadowRaw, _ := json.Marshal(meadow)
	ws := readWishSet(wishes, rows, f.slow.closedRows, f.slow.closedWishes, f.slow.wishProduce, plotOf)
	ws.dressArms(ctx, f.line, f.src)
	wishesDoc := ws.build(nil, nil, ws.closedTotal)
	wishesRaw, _ := json.Marshal(wishesDoc)
	cut.wishes = ws
	es := readEscalationSet(rows, func(b *beadsapi.BeadDetail) string { pl, _ := resolve(b); return pl }, f.party, execs)
	escalationsRaw, _ := json.Marshal(es.build(nil, nil))
	cut.escalations = es
	releasesRaw := []byte(emptyReleases)
	if f.relSt.blob != nil {
		releasesRaw, _ = json.Marshal(f.relSt.blob)
	}
	cut.releases = f.relSt.blob
	pilesRaw, _ := json.Marshal(f.slow.piles)
	closedRaw, _ := json.Marshal(f.slow.closed)
	var costRaw []byte
	if f.spend != nil {
		costRaw, _ = json.Marshal(f.costSt.cost)
	}
	// Gardeners. A read the daemon cut (more agents than one list returns) is
	// treated like a failed one: the last good set stays. Exits are read only
	// when this read and the previous one were complete, so an id dropped by
	// a cap or a failed read never looks like an exit.
	nextGardeners := f.gardeners
	if f.cutSt != nil {
		cut.gardenerSet = f.cutSt.gardenerSet
	}
	var exits []FarmExit
	if agentsErr == nil && !agents.MayHaveMore() {
		f.readRefTitles(ctx, agents.Beads, byID)
		f.readRunSpend(ctx, agents.Beads)
		refOf := func(id string) string {
			if b := byID[id]; b != nil {
				return b.Title
			}
			return f.refTitles[id].title
		}
		gs, capped := buildGardeners(agents.Beads, plotOf, refOf, refOf, gardenerArms(byID, f.refTitles), f.runSpendOf, buildParkIndex(rows), f.now())
		if !capped && f.gs.complete {
			var gone []string
			for id := range f.gs.byID {
				if _, ok := gs[id]; !ok {
					gone = append(gone, id)
				}
			}
			if len(gone) > 0 {
				sort.Strings(gone)
				exits = f.readExits(ctx, gone, f.gs.byID, byID, isContainer)
			}
		}
		f.gs = gardenerState{at: f.now(), complete: !capped, capped: capped, byID: gs}
		f.act.setGardeners(gs)
		nextGardeners = gardenerEntries(gs)
		cut.gardenerSet = gs
	} else {
		f.gs.complete = false
	}
	homeRaw := cut.homeRaw(rows, wishesDoc)
	meta := FarmMeta{Schema: FarmSchema, Truncated: truncated, RowCap: farmRowCap, Rows: len(rows), GardenersTruncated: f.gs.capped}
	if !f.gs.at.IsZero() && f.now().Sub(f.gs.at) >= farmRosterStale {
		meta.GardenersAsOf = f.gs.at.Unix()
	}
	metaRaw, _ := json.Marshal(meta)
	if truncated {
		f.logger.Warn("garden farm: work-bead read hit the row cap", "cap", farmRowCap)
	}
	cut.meta, cut.orphans = meta, f.slow.orphanAll
	composeCut(cut, rows, f.slow.closedRows)
	f.refreshArchives(ctx)

	f.mu.Lock()
	defer f.mu.Unlock()
	f.refreshes++
	if slowRan {
		f.slowRefreshes++
	}
	f.builtAt = f.now()
	f.drawn = drawn
	// The cut can move where the document does not (a row past a cap), so it is
	// replaced on every refresh; its ETag is its content's (farm_cut.go).
	f.cutSt = cut
	f.cutGen++

	planUp, planUpIDs, planRm := diffEntries(f.plans, nextPlans)
	seedUp, seedUpIDs, seedRm := diffEntries(f.seeds, nextSeeds)
	gardenerUp, gardenerUpIDs, gardenerRm := diffEntries(f.gardeners, nextGardeners)
	patch := farmPatch{}
	if len(planUp)+len(planRm) > 0 {
		patch.Plans = &setPatch{Up: planUps(f.plans, planUp, planUpIDs), Rm: planRm}
	}
	if len(seedUp)+len(seedRm) > 0 {
		patch.Seeds = &setPatch{Up: seedUp, Rm: seedRm}
	}
	if len(gardenerUp)+len(gardenerRm) > 0 {
		patch.Gardeners = &setPatch{Up: gardenerUp, Rm: gardenerRm}
		patch.Exits = exits
	}
	if !bytes.Equal(f.closedPlans, closedRaw) {
		patch.ClosedPlans = closedRaw
	}
	if !bytes.Equal(f.meadow, meadowRaw) {
		patch.Meadow = meadowRaw
	}
	if !bytes.Equal(f.wishes, wishesRaw) {
		patch.Wishes = wishesRaw
	}
	if !bytes.Equal(f.home, homeRaw) {
		patch.Home = homeRaw
	}
	if !bytes.Equal(f.escalations, escalationsRaw) {
		patch.Escalations = escalationsRaw
	}
	if !bytes.Equal(f.releases, releasesRaw) {
		patch.Releases = releasesRaw
	}
	if !bytes.Equal(f.piles, pilesRaw) {
		patch.Piles = pilesRaw
	}
	if !bytes.Equal(f.meta, metaRaw) {
		patch.Meta = metaRaw
	}
	if !bytes.Equal(f.cost, costRaw) {
		patch.Cost = costRaw
	}
	// Every cut stream follows, from the new cut state, whether or not the whole farm moved.
	defer f.advanceCutsLocked(exits)
	if patch.empty() {
		return degraded
	}
	f.plans, f.seeds, f.gardeners = nextPlans, nextSeeds, nextGardeners
	f.closedPlans, f.meadow, f.wishes, f.home, f.releases, f.piles, f.meta, f.cost = closedRaw, meadowRaw, wishesRaw, homeRaw, releasesRaw, pilesRaw, metaRaw, costRaw
	f.escalations = escalationsRaw
	f.changes++
	m := f.st.publish(func(prev, v uint64) []byte {
		patch.V, patch.Prev = v, prev
		data, _ := json.Marshal(patch)
		return data
	})
	f.dl.note(m.V, patch, [3][]string{planUpIDs, seedUpIDs, gardenerUpIDs})
	return degraded
}

// task projects a bead to a FarmTask using the cached `blocks` edges.
func (f *Farm) task(b *beadsapi.BeadDetail, open map[string]*beadsapi.BeadDetail) FarmTask {
	mr := mrOf(b)
	ft := FarmTask{
		ID: b.ID, Title: b.Title, ShortTitle: strings.TrimSpace(b.ShortTitle), Type: b.Type, State: taskState(b, mr), Priority: b.Priority,
		Project: projectOf(b), Assignee: b.Assignee, Updated: b.UpdatedAt.Unix(), Created: b.CreatedAt.Unix(), MR: mr,
		Heraldry: b.Heraldry,
	}
	if b.Type == "escalation" {
		ft.Op = strings.TrimSpace(b.Fields["operation_type"])
		ft.Family = EscalationFamily(ft.Op)
		if b.Status != "closed" {
			ft.Wait = escalationState(b)
		}
	}
	// Only blockers still in the open set count: a closed blocker's edge is gone
	// the moment the fast tier sees it leave, without waiting for the slow tier.
	for _, id := range f.slow.deps[b.ID] {
		if _, ok := open[id]; ok && id != b.ID {
			ft.BlockedBy = append(ft.BlockedBy, id)
		}
	}
	if len(ft.BlockedBy) > 1 {
		sort.Strings(ft.BlockedBy)
	}
	if len(ft.BlockedBy) > maxBlockedBy {
		ft.BlockedBy = ft.BlockedBy[:maxBlockedBy]
	}
	for _, id := range ft.BlockedBy {
		if p := projectOf(open[id]); p != ft.Project {
			if ft.BlockedByProject == nil {
				ft.BlockedByProject = map[string]string{}
			}
			ft.BlockedByProject[id] = p
		}
	}
	return ft
}

type setPatch struct {
	Up []json.RawMessage `json:"up,omitempty"`
	Rm []string          `json:"rm,omitempty"`
}

// diffEntries is what changed from old to next: the entries upserted (up, with their ids in upIDs) and the ids removed.
func diffEntries(old, next map[string]entry) (up []json.RawMessage, upIDs, rm []string) {
	ids := make([]string, 0, len(next))
	for id := range next {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	for _, id := range ids {
		if o, ok := old[id]; ok && bytes.Equal(o.raw, next[id].raw) {
			continue
		}
		up = append(up, next[id].raw)
		upIDs = append(upIDs, id)
	}
	for id := range old {
		if _, ok := next[id]; !ok {
			rm = append(rm, id)
		}
	}
	sort.Strings(rm)
	return up, upIDs, rm
}

// ---- reading the snapshot -------------------------------------------------

func sortedRaw(m map[string]entry) []json.RawMessage {
	es := make([]struct {
		id string
		entry
	}, 0, len(m))
	for id, e := range m {
		es = append(es, struct {
			id string
			entry
		}{id, e})
	}
	sort.Slice(es, func(i, j int) bool {
		if es[i].key != es[j].key {
			return es[i].key > es[j].key
		}
		return es[i].id < es[j].id
	})
	out := make([]json.RawMessage, len(es))
	for i, e := range es {
		out[i] = e.raw
	}
	return out
}

func orRaw(b []byte, empty string) json.RawMessage {
	if len(b) == 0 {
		return json.RawMessage(empty)
	}
	return b
}

// snapshotLocked returns the full-state document for the current version,
// memoised per version. It doubles as the SSE "reset" payload.
//
// gardeners[].doing changes without a version: it is baked in when the activity epoch moved, at most every
// activitySnapEvery at one version, so a burst of tool calls does not re-marshal the document per request.
func (f *Farm) snapshotLocked() (uint64, []byte) {
	now := f.now()
	if f.memo.v == f.st.version && f.memo.b != nil && (f.memo.act == f.act.currentEpoch() || now.Sub(f.memo.at) < activitySnapEvery) {
		return f.memo.v, f.memo.b
	}
	doings, epoch := f.act.doings(f.gardeners, now)
	doc := struct {
		V           uint64            `json:"v"`
		BuiltAt     string            `json:"built_at"`
		Plans       []json.RawMessage `json:"plans"`
		Gardeners   []json.RawMessage `json:"gardeners"`
		Seeds       []json.RawMessage `json:"seeds"`
		ClosedPlans json.RawMessage   `json:"closed_plans"`
		Meadow      json.RawMessage   `json:"meadow"`
		Wishes      json.RawMessage   `json:"wishes"`
		Home        json.RawMessage   `json:"home"` // FarmHomes
		Escalations json.RawMessage   `json:"escalations"`
		Releases    json.RawMessage   `json:"releases"`
		Piles       json.RawMessage   `json:"piles"`
		Meta        json.RawMessage   `json:"meta"`
		Cost        json.RawMessage   `json:"cost,omitempty"` // FarmCost; absent when no spend source is wired
	}{
		V: f.st.version, Plans: sortedRaw(f.plans), Gardeners: sortedRaw(withDoing(f.gardeners, doings)), Seeds: sortedRaw(f.seeds),
		ClosedPlans: orRaw(f.closedPlans, `{"total":0,"shown":[]}`),
		Meadow:      orRaw(f.meadow, `{"total":0,"shown":[],"with_mr":0}`),
		Wishes:      orRaw(f.wishes, emptyWishes),
		Home:        orRaw(f.home, emptyHome),
		Escalations: orRaw(f.escalations, emptyEscalations),
		Releases:    orRaw(f.releases, emptyReleases),
		Piles:       orRaw(f.piles, `{}`),
		Meta:        orRaw(f.meta, `{}`),
		Cost:        f.cost,
	}
	if !f.builtAt.IsZero() {
		doc.BuiltAt = f.builtAt.UTC().Format(time.RFC3339)
	}
	b, _ := json.Marshal(doc)
	f.memo.v, f.memo.act, f.memo.at, f.memo.b, f.memo.gz = f.st.version, epoch, now, b, nil
	return f.st.version, b
}

// snapshotTag is the ETag of the document Snapshot returns: its version and the activity epoch baked into it.
func (f *Farm) snapshotTag() (string, []byte, time.Time) {
	f.mu.Lock()
	defer f.mu.Unlock()
	v, b := f.snapshotLocked()
	return farmTag(v, f.memo.act), b, f.builtAt
}

// farmTag is the whole farm's ETag at version v and activity epoch act.
func farmTag(v, act uint64) string { return fmt.Sprintf(`W/"f%d-a%d"`, v, act) }

// SnapshotGzip is Snapshot's body, gzip-compressed once per version. The
// document is highly repetitive JSON (about 15x), and the viewer has no
// response-compression layer of its own.
func (f *Farm) SnapshotGzip() (uint64, []byte) {
	f.mu.Lock()
	defer f.mu.Unlock()
	v, b := f.snapshotLocked()
	if f.memo.gz == nil {
		f.memo.gz = gzipOf(b)
	}
	return v, f.memo.gz
}

// ProjectCost is the project's row of the published cost blob (its spend and
// budget as the map's header reads them, farm_cost.go), or nil when the farm
// has no spend source or no row for it. It performs no I/O.
func (f *Farm) ProjectCost(project string) *FarmCostProject {
	f.mu.Lock()
	raw := f.cost
	f.mu.Unlock()
	if len(raw) == 0 {
		return nil
	}
	var c FarmCost
	if json.Unmarshal(raw, &c) != nil {
		return nil
	}
	for i := range c.Projects {
		if c.Projects[i].Project == project {
			return &c.Projects[i]
		}
	}
	return nil
}

// GardenerReason is the reason the published farm gives agent bead id
// (farm_reason.go): its kind and words, or "" when it is working or not drawn
// as a gardener. The workbench's agent head offers Wake or Restart from it
// (commons Handler.Reason, gd-7DyHdnx8fC). It performs no I/O.
func (f *Farm) GardenerReason(id string) (kind, text string) {
	f.mu.Lock()
	raw := f.gardeners[id].raw
	f.mu.Unlock()
	var g FarmGardener
	if len(raw) == 0 || json.Unmarshal(raw, &g) != nil || g.Reason == nil {
		return "", ""
	}
	return g.Reason.Kind, g.Reason.Text
}

// IsTenantLand reports whether project is a tenant land: its row of the
// published cost blob carries a purse, which attachPurses gives every project
// bead whose land_kind is set and not operator (an unreadable purse included).
// False when the farm cannot say. It performs no I/O.
func (f *Farm) IsTenantLand(project string) bool {
	row := f.ProjectCost(project)
	return row != nil && row.Purse != nil
}

// KnowsProject reports whether project is one the farm knows: a type:project
// bead the published cost blob has a row for, or a project it draws a plan, a
// seed or a loose bead on (a plot on the map whose project has no bead yet,
// or no row, is still a project). listed is false while the farm has no blob
// (no spend source, or before its first cost read), when it cannot say. It
// performs no I/O.
func (f *Farm) KnowsProject(project string) (known, listed bool) {
	f.mu.Lock()
	raw, drawn := f.cost, f.drawn[project]
	f.mu.Unlock()
	var c FarmCost
	if len(raw) == 0 || json.Unmarshal(raw, &c) != nil {
		return false, false
	}
	if drawn {
		return true, true
	}
	for i := range c.Projects {
		if c.Projects[i].Project == project {
			return true, true
		}
	}
	return false, true
}

// Snapshot returns the current version and its full JSON document. It performs
// no I/O.
func (f *Farm) Snapshot() (uint64, []byte, time.Time) {
	f.mu.Lock()
	defer f.mu.Unlock()
	v, b := f.snapshotLocked()
	return v, b, f.builtAt
}

func (f *Farm) resetLocked() Msg {
	v, b := f.snapshotLocked()
	return Msg{Event: "reset", V: v, Data: b}
}

// Reset returns a full-state reset message for the current version.
func (f *Farm) Reset() Msg {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.st.resets++
	return f.resetLocked()
}

// viewLocked is the whole farm's state as the delta log reads it.
func (f *Farm) viewLocked() cutView {
	return cutView{plans: f.plans, seeds: f.seeds, gardeners: f.gardeners, closedPlans: f.closedPlans, meadow: f.meadow, wishes: f.wishes, home: orRaw(f.home, emptyHome),
		escalations: f.escalations, releases: f.releases, piles: f.piles, meta: f.meta, cost: f.cost}
}

// deltaLocked is the one patch from since to the current version (farm_delta.go); false when the log does not cover since.
func (f *Farm) deltaLocked(since uint64) (Msg, bool) {
	return f.dl.msg(since, f.st.version, f.viewLocked())
}

// Resume is the catch-up for a client that last saw version since, as Subscribe sends it: nothing when it is current,
// one delta patch when the log covers since, else the ring's replay, else a reset. farmEvents answers a laggard with it.
func (f *Farm) Resume(since uint64) []Msg {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.st.catchUp(since, f.deltaLocked, f.resetLocked)
}

// Subscribe registers a client that last saw version since (0 = unknown); see
// Hub.Subscribe. The catch-up and the registration happen under one lock.
func (f *Farm) Subscribe(since uint64) (catchUp []Msg, live <-chan Msg, cancel func()) {
	f.mu.Lock()
	defer f.mu.Unlock()
	catchUp, live, c := f.st.subscribe(since, f.deltaLocked, f.resetLocked)
	return catchUp, live, func() {
		f.mu.Lock()
		defer f.mu.Unlock()
		c()
	}
}

// Stats returns a point-in-time view for diagnostics.
func (f *Farm) Stats() FarmStats {
	f.mu.Lock()
	defer f.mu.Unlock()
	_, b := f.snapshotLocked()
	act := f.ActivityStats()
	return FarmStats{
		Version: f.st.version, Plans: len(f.plans), Seeds: len(f.seeds), Gardeners: len(f.gardeners), Clients: len(f.st.clients),
		CutStreams: len(f.cuts), CutClients: f.cutClientsLocked(),
		RingLen: len(f.st.ring), Refreshes: f.refreshes, SlowRefreshes: f.slowRefreshes, Changes: f.changes,
		Resets: f.st.resets, Deltas: f.st.deltas, LastRefreshMS: f.lastRefresh.Milliseconds(), SnapshotBytes: len(b), LastError: f.lastErr,
		ActivityClients: act.ActivityClients, ActivitySent: act.ActivitySent, ActivityBytes: act.ActivityBytes,
		ActivityDropped: act.ActivityDropped, ActivityViews: act.ActivityViews,
		ActivityFoldClients: act.ActivityFoldClients, ActivityFoldBytes: act.ActivityFoldBytes,
		TranscriptRows: act.TranscriptRows, TranscriptClients: act.TranscriptClients, TranscriptBytes: act.TranscriptBytes,
	}
}
