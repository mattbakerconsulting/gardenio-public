package main

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"gardenio/controller/internal/beadsapi"
	"gardenio/controller/internal/config"
	"gardenio/controller/internal/lands/landns"
	"gardenio/controller/internal/lands/purse"
	"gardenio/controller/internal/lands/wall"
	"gardenio/controller/internal/reconciler"
	"gardenio/controller/internal/spawnguard"

	"github.com/prometheus/client_golang/prometheus"
)

// spawnWallRefusal is the Lands spawn wall (internal/lands/wall, Track B spec
// §3.5, step 8.3) at the controller's two pod doors: the AgentSpawn event and
// the reconciler's create-missing pass, for a spawn of role (the land's
// steward passes with tenant spawns off, gd-KOAoUELMWo). A project the cache knows as an
// operator land passes with no daemon read, so operator lands behave exactly
// as before. Anything else is read live (land_frozen is a kill switch, so a
// cached copy is not good enough), and a read that fails is refused as
// unknown: the project is not known to be the operator's, and fail closed is
// the rule for a tenant's money.
func spawnWallRefusal(ctx context.Context, cfg *config.Config, daemon spawnWallDaemon, project, role string, now time.Time) wall.Refusal {
	if project == "" || daemon == nil {
		return wall.Refusal{}
	}
	cfg.ProjectCacheMu.RLock()
	entry, cached := cfg.ProjectCache[project]
	cfg.ProjectCacheMu.RUnlock()
	if cached && entry.IsOperatorLand() {
		return wall.Refusal{}
	}
	r, err := wall.Check(ctx, daemon, project, role, cfg.LandsTenantSpawns, now)
	if err != nil {
		return wall.Unknown(project, err)
	}
	return r
}

// spawnAdmission is spawnWallRefusal plus the land fence check (step 10.2),
// the land's live-agent cap (step 9.5) and the per-land spawn rate (Track B
// spec §5.4, step 10.4). A tenant land the wall lets through is refused with
// land_unfenced unless the project cache places its pods in the land's
// namespace (landFenceRefusal), with max_live_agents while it runs its cap of
// agents (landLiveCap; a steward is never refused there), and with
// spawn_rate once it has started limit's cap of agents in the window. beadID
// is the agent's own slot, so the event door and the reconciler deciding the
// same agent count it once. Operator lands never reach any of them.
func spawnAdmission(ctx context.Context, cfg *config.Config, daemon spawnWallDaemon, limit *spawnguard.Keyed, project, role, beadID string, now time.Time) wall.Refusal {
	r := spawnWallRefusal(ctx, cfg, daemon, project, role, now)
	if r.Refused() || !r.Tenant {
		return r
	}
	if r := landFenceRefusal(cfg, project); r.Refused() {
		return r
	}
	// The cap goes before the rate, so a spawn refused for the cap does not
	// spend a slot of the hour.
	if r := landLiveCap(ctx, daemon, project, role, beadID); r.Refused() {
		return r
	}
	return wall.Rate(limit, project, beadID)
}

// landLiveCap is wall.LiveCap with the land's project bead read live, for a
// tenant land the wall let through. A project bead that cannot be read is
// refused: the cap cannot be known, and fail closed is the rule.
func landLiveCap(ctx context.Context, daemon spawnWallDaemon, project, role, beadID string) wall.Refusal {
	if daemon == nil {
		return wall.Refusal{Tenant: true}
	}
	proj, _, err := purse.Project(ctx, daemon, project)
	if err != nil {
		return wall.Refusal{Code: wall.CodeMaxLiveAgents, Tenant: true, Reason: fmt.Sprintf(
			"land %q's project bead could not be read for its max_live_agents: %v", project, err)}
	}
	return wall.LiveCap(ctx, daemon, project, proj.Fields, role, beadID)
}

// landHaltRefusal is the land's kill switches alone (wall.Halted), for the
// GitHub token issuer's mint, reuse and refresh. Like spawnWallRefusal, an
// operator land the cache knows passes with no daemon read.
func landHaltRefusal(cfg *config.Config, daemon spawnWallDaemon) func(context.Context, string) wall.Refusal {
	return func(ctx context.Context, project string) wall.Refusal {
		cfg.ProjectCacheMu.RLock()
		entry, cached := cfg.ProjectCache[project]
		cfg.ProjectCacheMu.RUnlock()
		if cached && entry.IsOperatorLand() {
			return wall.Refusal{}
		}
		return wall.Halted(ctx, daemon, project)
	}
}

// landFenceRefusal refuses a tenant land's spawn the pod spec builders could
// not fence: the wall read the land live as a tenant land, but the pod spec
// is built from the project cache, so the cache has to know it as a tenant
// land whose namespace step has run (applyLandFence). The pods go to
// land-<land> whatever the bead says, so a project bead naming any other
// namespace is refused too: it was not written by the provisioner. A cache
// that lags a new land refuses until the next refresh; the bead waits at
// budget_exceeded and is re-decided on each pass, as for every wall code.
//
// A tenant pod never carries the shared daemon token (step 10.3,
// fenceTenantPodContents), so with per-agent tokens off it would start with
// no daemon credential at all: refused until PER_AGENT_TOKENS is on.
func landFenceRefusal(cfg *config.Config, project string) wall.Refusal {
	cfg.ProjectCacheMu.RLock()
	entry, cached := cfg.ProjectCache[project]
	cfg.ProjectCacheMu.RUnlock()
	if !cached || !entry.IsTenantLand() {
		return wall.Unfenced(project, "the controller's project cache does not know it as a tenant land yet")
	}
	want, err := landns.Name(project)
	switch {
	case err != nil:
		return wall.Unfenced(project, err.Error())
	case entry.Namespace == "":
		return wall.Unfenced(project, "its project bead names no namespace (the provisioner's namespace step has not run)")
	case entry.Namespace != want:
		return wall.Unfenced(project, fmt.Sprintf("its project bead names namespace %q, not its own %q", entry.Namespace, want))
	case !cfg.PerAgentTokens:
		return wall.Unfenced(project, "per-agent daemon tokens are off (PER_AGENT_TOKENS), and a tenant pod never carries the shared token")
	}
	return wall.Refusal{}
}

// landSpawnRate is the controller's one per-land spawn rate, shared by both
// pod doors; run() sets it from LANDS_SPAWNS_PER_HOUR. Nil admits.
var landSpawnRate *spawnguard.Keyed

// landSpawnRefusals counts spawn wall refusals by land and code, once per
// agent bead and code (a re-decided refusal is not counted again). It backs
// the Lands abuse alerts (LandSpawnRefusals, LandPurseWall; Track B §5.4).
// Labels are bounded: tenant lands and the wall's codes.
var landSpawnRefusals = prometheus.NewCounterVec(prometheus.CounterOpts{
	Namespace: "gardenio",
	Name:      "lands_spawn_refusals_total",
	Help:      "Agent spawns refused by the Lands spawn wall, by tenant land and refusal code (land_frozen, lands_paused, tenant_spawns_off, purse_wall, purse_unknown, land_gone, land_unfenced, max_live_agents, spawn_rate). Counted once per agent bead and code.",
}, []string{"land", "code"})

// spawnWallDaemon is the slice of the daemon client the wall reads and
// records through.
type spawnWallDaemon interface {
	purse.Source
	wall.Recorder
}

// asSpawnWallDaemon is daemon as a spawnWallDaemon, nil for a nil client (an
// interface holding a nil *Client is not nil).
func asSpawnWallDaemon(daemon *beadsapi.Client) spawnWallDaemon {
	if daemon == nil {
		return nil
	}
	return daemon
}

// recordSpawnWallRefusal logs a wall refusal and records it on the agent bead
// (agent_state=budget_exceeded, spawn_wall fields, one comment). The refusal
// holds whether or not the writes land; fields are the bead's current fields,
// which keep a re-decided refusal from commenting twice.
func recordSpawnWallRefusal(ctx context.Context, logger *slog.Logger, rec spawnWallDaemon, project, agentName, beadID string, fields map[string]string, r wall.Refusal) {
	logger.Warn("refusing agent spawn — Lands spawn wall",
		"agent", agentName, "project", project, "bead", beadID,
		"code", r.Code, "reason", r.Reason)
	if rec == nil {
		return
	}
	wrote, err := wall.Record(ctx, rec, beadID, fields, r)
	if wrote {
		landSpawnRefusals.WithLabelValues(project, r.Code).Inc()
	}
	if err != nil {
		logger.Warn("spawn wall: failed to record the refusal on the agent bead",
			"bead", beadID, "code", r.Code, "error", err)
	}
}

// reconcilerSpawnWall wires the wall into the reconciler's create-missing
// door with the same decision and recording as the event door, the per-land
// spawn rate (limit) included. A running agent's recreated pod is asked
// about the land's kill switches alone (landHaltRefusal, gd-Nc7yMpW7U6).
func reconcilerSpawnWall(cfg *config.Config, logger *slog.Logger, daemon spawnWallDaemon, limit *spawnguard.Keyed) reconciler.SpawnWall {
	var halted func(context.Context, string) wall.Refusal
	if daemon != nil {
		halt := landHaltRefusal(cfg, daemon)
		halted = func(ctx context.Context, project string) wall.Refusal {
			if project == "" {
				return wall.Refusal{}
			}
			return halt(ctx, project)
		}
	}
	return reconciler.SpawnWall{
		Halted: halted,
		Check: func(ctx context.Context, project, role string) wall.Refusal {
			r := spawnWallRefusal(ctx, cfg, daemon, project, role, time.Now())
			if r.Refused() || !r.Tenant {
				return r
			}
			if f := landFenceRefusal(cfg, project); f.Refused() {
				return f
			}
			return r
		},
		Rate: func(ctx context.Context, bead beadsapi.AgentBead) wall.Refusal {
			if r := landLiveCap(ctx, daemon, bead.Project, bead.Role, bead.ID); r.Refused() {
				return r
			}
			return wall.Rate(limit, bead.Project, bead.ID)
		},
		Record: func(ctx context.Context, bead beadsapi.AgentBead, r wall.Refusal) {
			recordSpawnWallRefusal(ctx, logger, daemon, bead.Project, bead.AgentName, bead.ID, bead.Metadata, r)
		},
	}
}
