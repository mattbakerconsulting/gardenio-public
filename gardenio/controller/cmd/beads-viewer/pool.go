package main

// The prewarmed-pool view at /pool (epic ka-0000007327, task ka-0000001128).
//
// Prewarmed-pool members are warm-standby pods the pool manager owns. They are
// deliberately filtered out of /agents (ka-0000005111 / isUnengagedPrewarmedAgent)
// so idle standby rows do not clutter the fleet with cards an operator can do
// nothing about. That left the pool with no home for its own signal. This page
// is that home: one row per (project, role) pool, joining the operator-configured
// pool sizing (the project bead's prewarmed_pool JSON, normalized by
// EffectivePools) against the LIVE pool-member agent beads, plus a short recent
// hit/expiry window derived from just-closed pool members.
//
// Data sources, all best-effort (a failure of one degrades that panel, never
// the page):
//
//	(a) ListProjectBeads → ProjectInfo.PrewarmedPool.EffectivePools() gives the
//	    configured (project,role) pools with their min/max/mode. This is the
//	    authoritative registry of which pools SHOULD exist.
//	(b) The live agent beads (open + in_progress) whose agent name carries the
//	    pool minting convention (prefix "prewarmed-"). These are the current
//	    members: warm (unengaged, holding pod capacity) or engaged (assigned off
//	    the pool onto real work — a live hit still in flight).
//	(c) The recently-closed pool members give a short-window hit/expiry read:
//	    a closed member that was engaged is a HIT the pool served; one that
//	    closed still unengaged is an EXPIRY (evicted/pruned before use).
//
// The union of (a), (b) and (c) is what makes the page a drift surface, not just
// a config dump: a configured pool with zero warm members reads as starved, and
// a live pool with no config entry reads as undeclared drift.

import (
	"context"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"

	"gardenio/controller/internal/beadsapi"
	"gardenio/controller/internal/oidcauth"
)

// poolAgentLimit caps both the live-member and recent-closed fetches. The pool
// spans every project, so the bound matches the other cross-project fleet
// scans (rolesIndexAgentLimit / agentsActiveLimit) — a safety cap, not an
// expected truncation point for a realistic pool.
const poolAgentLimit = 500

// poolRecentWindow bounds the recently-closed member scan used for the
// hit/expiry counters. The daemon has no updated_after filter on this path, so
// we fetch the newest closed members and trim client-side to this window —
// matching how /agents trims its Recent section (ka-0000001416).
const poolRecentWindow = 24 * time.Hour

// poolMemberNamePrefix is the minting convention the pool manager stamps on
// every prewarmed member's agent name (poolmanager.createPrewarmedAgent:
// "prewarmed-<unix>-<seq>"). Membership on this page keys off the prefix so an
// ASSIGNED member — one flipped to agent_state=assigning/working with a thread
// or task — is still recognised as a pool member (a hit), not lost from the
// pool's accounting the moment it starts real work. Shared with the controller's
// isParkedPoolMember convention.
const poolMemberNamePrefix = "prewarmed-"

// poolKey identifies one pool by (project, role) — the same key the pool
// manager uses (poolmanager.poolKey).
type poolKey struct {
	project string
	role    string
}

// poolStatsRow is one (project, role) pool on the /pool view.
type poolStatsRow struct {
	Project string
	Role    string
	// Mode is the spawn mode the pool prewarms (crew/thread/…), from the config
	// entry. Empty for a drift pool with no config.
	Mode string

	// Configured reports whether a prewarmed_pool entry backs this (project,role).
	// A row can exist with Configured=false when live members carry a
	// (project,role) no config declares — undeclared drift.
	Configured bool
	// MinSize / MaxSize are the operator-configured target bounds (EffectivePools
	// normalization applied). Zero for a drift row.
	MinSize int
	MaxSize int

	// Current is the total live pool members (Warm + Engaged) — the pool's
	// current occupancy.
	Current int
	// Warm is the count of unengaged members holding standby capacity, ready to
	// be handed to a cold spawn (isUnengagedPrewarmedAgent). This is also the
	// capacity the pool is holding right now: each warm member is a real pod
	// with node CPU/mem requests.
	Warm int
	// Engaged is the count of members already assigned off the pool onto real
	// work (a thread/task attached) — live hits still in flight.
	Engaged int

	// OldestWarmAge / NewestWarmAge are compact human ages of the warm members
	// (by bead creation time). Empty when the pool holds no warm member. A large
	// oldest age on a healthy pool is normal (warm pods are long-lived); it
	// turns interesting next to a starvation signal.
	OldestWarmAge string
	NewestWarmAge string

	// RecentHits / RecentExpired summarize the last poolRecentWindow of closed
	// members: a hit was engaged before it closed (the pool served a spawn); an
	// expiry closed still unengaged (pruned/evicted before use).
	RecentHits    int
	RecentExpired int

	// Health signals, precomputed so the template stays declarative.
	// BelowMin: a configured pool whose warm capacity has fallen under its min
	// target — starved or mid-refill (the exact starvation the epic is about).
	BelowMin bool
	// OverMax: a configured pool whose current occupancy exceeds its max.
	OverMax bool
	// Drift: live members with no config entry — an undeclared pool.
	Drift bool
}

// AttentionRank folds the health signals into a sort/label priority: drift and
// over-max and below-min all deserve to float above healthy rows.
func (r poolStatsRow) NeedsAttention() bool { return r.BelowMin || r.OverMax || r.Drift }

// poolView is the assembled view-model for /pool.
type poolView struct {
	Rows []poolStatsRow

	// Headline totals across all rows.
	ConfiguredPools int // rows backed by a prewarmed_pool entry
	DriftPools      int // rows with live members but no config
	TotalWarm       int // warm members across all pools (== standby capacity held)
	TotalEngaged    int // engaged members across all pools (live hits in flight)
	StarvedPools    int // configured pools below their min target

	// RecentWindowLabel is the compact label for the hit/expiry window.
	RecentWindowLabel string

	// Error is a page-level error, set only when the one hard dependency (the
	// live-member fetch) fails. Per-panel degradation does not set it.
	Error string
}

// handlePool renders the /pool prewarmed-pool view. Viewer-scoped read (plain
// HandleFunc behind the ambient auth middleware), mirroring handleRolesIndex.
func (s *Server) handlePool(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	email := oidcauth.EmailFromContext(ctx)

	view := s.buildPoolView(ctx, s.now())

	data := map[string]any{
		"Pool":        view,
		"ViewerEmail": email,
		"InboxCount":  s.inboxCountForViewer(ctx, email),
	}
	s.render(w, r, "pool.html", data)
}

// buildPoolView assembles the /pool view-model. The live-member fetch is the
// only hard dependency: if it fails the page carries a page-level Error.
// The configured-pool registry and the recent-closed window each degrade
// independently — a failure there just yields fewer rows or zeroed counters.
func (s *Server) buildPoolView(ctx context.Context, now time.Time) poolView {
	var view poolView
	view.RecentWindowLabel = shortDuration(poolRecentWindow)

	// (a) Configured pools from project beads. Best-effort: a fetch error just
	// means we cannot flag drift/starvation against config — live members still
	// render.
	configured := s.configuredPools(ctx)

	// (b) Live members — the hard dependency.
	live, err := s.daemon.ListBeadsFiltered(ctx, beadsapi.ListBeadsQuery{
		Types:    []string{"agent"},
		Statuses: []string{"open", "in_progress"},
		Sort:     "-updated_at",
		Limit:    poolAgentLimit,
	})
	if err != nil {
		s.logger.Error("pool view: listing live agents", "error", err)
		view.Error = "Failed to load pool members."
		return view
	}

	// (c) Recent closed members for the hit/expiry window. Best-effort.
	recent, err := s.daemon.ListBeadsFiltered(ctx, beadsapi.ListBeadsQuery{
		Types:    []string{"agent"},
		Statuses: []string{"closed"},
		Sort:     "-updated_at",
		Limit:    poolAgentLimit,
	})
	if err != nil {
		s.logger.Warn("pool view: listing recent closed agents", "error", err)
		recent = &beadsapi.ListBeadsResult{}
	}

	// Accumulator per pool key. Warm member ages tracked to derive oldest/newest.
	type acc struct {
		row          poolStatsRow
		oldestWarm   time.Time
		newestWarm   time.Time
		haveWarmTime bool
	}
	pools := map[poolKey]*acc{}
	get := func(k poolKey) *acc {
		a := pools[k]
		if a == nil {
			a = &acc{row: poolStatsRow{Project: k.project, Role: k.role}}
			pools[k] = a
		}
		return a
	}

	// Seed from config so a configured-but-empty pool still gets a (starved) row.
	for k, cfg := range configured {
		a := get(k)
		a.row.Configured = true
		a.row.Mode = cfg.Mode
		a.row.MinSize = cfg.MinSize
		a.row.MaxSize = cfg.MaxSize
	}

	// (b) Fold live members into occupancy + warm-age tracking.
	if live != nil {
		for _, b := range live.Beads {
			if !isPoolMemberBead(b) {
				continue
			}
			a := get(poolKeyForMember(b))
			a.row.Current++
			if isUnengagedPrewarmedAgent(b) {
				a.row.Warm++
				t := b.CreatedAt
				if !t.IsZero() {
					if !a.haveWarmTime || t.Before(a.oldestWarm) {
						a.oldestWarm = t
					}
					if !a.haveWarmTime || t.After(a.newestWarm) {
						a.newestWarm = t
					}
					a.haveWarmTime = true
				}
			} else {
				a.row.Engaged++
			}
		}
	}

	// (c) Fold the recent-closed window into hit/expiry counters.
	if recent != nil {
		cutoff := now.Add(-poolRecentWindow)
		for _, b := range recent.Beads {
			if !isPoolMemberBead(b) {
				continue
			}
			if b.UpdatedAt.Before(cutoff) {
				continue
			}
			a := get(poolKeyForMember(b))
			if poolMemberEngaged(b) {
				a.row.RecentHits++
			} else {
				a.row.RecentExpired++
			}
		}
	}

	// Finalize each row: derive ages, health signals, and headline totals.
	rows := make([]poolStatsRow, 0, len(pools))
	for _, a := range pools {
		row := a.row
		if a.haveWarmTime {
			row.OldestWarmAge = poolAge(now.Sub(a.oldestWarm))
			row.NewestWarmAge = poolAge(now.Sub(a.newestWarm))
		}
		if row.Configured {
			row.BelowMin = row.Warm < row.MinSize
			row.OverMax = row.MaxSize > 0 && row.Current > row.MaxSize
			view.ConfiguredPools++
			if row.BelowMin {
				view.StarvedPools++
			}
		} else {
			row.Drift = true
			view.DriftPools++
		}
		view.TotalWarm += row.Warm
		view.TotalEngaged += row.Engaged
		rows = append(rows, row)
	}

	// Sort: attention rows (starved/over-max/drift) first so an operator lands
	// on the pools that need action, then by current occupancy desc, then
	// project + role for a stable order.
	sort.SliceStable(rows, func(i, j int) bool {
		if rows[i].NeedsAttention() != rows[j].NeedsAttention() {
			return rows[i].NeedsAttention()
		}
		if rows[i].Current != rows[j].Current {
			return rows[i].Current > rows[j].Current
		}
		if rows[i].Project != rows[j].Project {
			return rows[i].Project < rows[j].Project
		}
		return rows[i].Role < rows[j].Role
	})

	view.Rows = rows
	return view
}

// poolConfigEntry is the normalized config for one (project, role) pool.
type poolConfigEntry struct {
	Mode    string
	MinSize int
	MaxSize int
}

// configuredPools reads every project bead's prewarmed_pool config and expands
// it into the normalized (project,role) → sizing map via EffectivePools.
// Best-effort: a daemon error yields an empty map (no config to join against),
// never a failure — the live members still render as drift rows.
func (s *Server) configuredPools(ctx context.Context) map[poolKey]poolConfigEntry {
	out := map[poolKey]poolConfigEntry{}
	projects, err := s.daemon.ListProjectBeads(ctx)
	if err != nil {
		s.logger.Warn("pool view: listing project beads", "error", err)
		return out
	}
	for name, info := range projects {
		if name == "" || info.PrewarmedPool == nil {
			continue
		}
		for _, p := range info.PrewarmedPool.EffectivePools() {
			k := poolKey{project: name, role: p.Role}
			// First entry wins (EffectivePools already dedupes per project, and a
			// role name is unique within a project's pools).
			if _, dup := out[k]; dup {
				continue
			}
			out[k] = poolConfigEntry{Mode: p.Mode, MinSize: p.MinSize, MaxSize: p.MaxSize}
		}
	}
	return out
}

// isPoolMemberBead reports whether an agent bead is a prewarmed-pool member,
// keyed off the pool's minting convention (agent name prefix "prewarmed-").
// Both warm and engaged members match — an engaged member is a pool hit still
// in flight, not something to drop from the pool's accounting.
func isPoolMemberBead(b *beadsapi.BeadDetail) bool {
	if b == nil {
		return false
	}
	name := b.Fields["agent"]
	if name == "" {
		name = b.Title
	}
	return strings.HasPrefix(name, poolMemberNamePrefix)
}

// poolKeyForMember resolves a pool member bead to its (project, role) key from
// its fields, matching how the pool manager and role hub read them.
func poolKeyForMember(b *beadsapi.BeadDetail) poolKey {
	return poolKey{
		project: strings.TrimSpace(b.Fields["project"]),
		role:    strings.TrimSpace(b.Fields["role"]),
	}
}

// poolMemberEngaged reports whether a pool member was handed real work — a
// thread or task attached, or the pool-assign spawn_source stamp
// (poolmanager.AssignPrewarmed writes spawn_source=slack-thread-pool alongside
// the agent_state flip). Used to classify a CLOSED member as a hit vs an expiry.
func poolMemberEngaged(b *beadsapi.BeadDetail) bool {
	if b == nil {
		return false
	}
	if b.Fields["slack_thread_ts"] != "" || b.Fields["task_id"] != "" {
		return true
	}
	return b.Fields["spawn_source"] == "slack-thread-pool"
}

// poolAge renders a duration as a compact age string (e.g. "45s", "12m", "3h",
// "2d") for the warm-member age columns. Coarser than shortDuration on purpose:
// the pool view wants an at-a-glance magnitude, not sub-unit precision.
func poolAge(d time.Duration) string {
	if d < 0 {
		d = 0
	}
	switch {
	case d < time.Minute:
		return strconv.Itoa(int(d/time.Second)) + "s"
	case d < time.Hour:
		return strconv.Itoa(int(d/time.Minute)) + "m"
	case d < 24*time.Hour:
		return strconv.Itoa(int(d/time.Hour)) + "h"
	default:
		return strconv.Itoa(int(d/(24*time.Hour))) + "d"
	}
}
