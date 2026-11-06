// Package poolmanager maintains per-project pools of prewarmed agent pods
// ready for instant assignment. It reads pool configuration from the project
// cache (populated from project bead "prewarmed_pool" JSON fields) and
// reconciles each project's pool independently.
package poolmanager

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"

	"gardenio/controller/internal/beadsapi"
	"gardenio/controller/internal/config"
	"gardenio/controller/internal/mailproto"
	"gardenio/controller/internal/podmanager"
)

// defaultMicrovmMaximumDurationSeconds mirrors the microvm backend's compiled
// default MaximumDurationInSeconds (internal/microvm/backend.go
// defaultMaximumDurationSeconds) used when MICROVM_MAXIMUM_DURATION_SECONDS is
// unset. It is duplicated here (not imported) to keep poolmanager free of an
// internal/microvm dependency (which pulls in the AWS SDK); microvmRecycleAge
// only needs it to derive a recycle cadence when the config leaves the cap at
// its default. Drift is safe-side: a stale smaller value just recycles members
// a little earlier, still far under any real cap. TestMicrovmRecycleAge_ExplicitKnobWins
// pins the derived age against this constant.
const defaultMicrovmMaximumDurationSeconds = 3600

// poolSystemOperator is the X-System-Operator source label the pool manager
// stamps on its machine-origin bead writes (prewarmed-agent create / close /
// recycle). These reconciler writes carry NO human operator: there is no OIDC
// identity, no X-Operated-By header, and no spawn-chain human behind a periodic
// pool reconcile. The daemon's resolver fail-closes a header-less static-token
// write with HTTP 400 "operator could not be resolved" (ka-0000005772), which is
// exactly what froze the pool-recycle E2E in a fresh ephemeral namespace
// (ka-0000006169): prod/the persistent singleton tolerate the omission because
// their daemon carries the bridge-fleet-tokens secret so the controller token
// resolves to a fleet: sentinel, but an ephemeral instance disables bridge
// tokens (bridgeFleetTokenSecret: "") and the controller falls back to the
// primary static token whose header-less write is refused. Routing these writes
// through WithSystemOperator binds the auditable system:<source> machine sink in
// BOTH environments (and survives the eventual BEADS_AUTHORING_HUMAN_GATE
// collapse to system:machine), satisfying the main.go:255 contract that TRUE
// machine-origin reconciler/housekeeping writes carry an explicit per-call
// WithSystemOperator. The label must satisfy sanitizeSystemOperatorHeader (ASCII
// lowercase/digits/single-dash) — pinned by TestPoolSystemOperatorIsValidLabel.
const poolSystemOperator = "pool-manager"

// PrewarmedCycleRecorder observes stale-image prewarmed-agent recycle events for
// metrics (ka-0000001689). Satisfied by *metrics.PoolMetrics; nil is a no-op.
type PrewarmedCycleRecorder interface {
	// RecordPrewarmedAgentCycled records that count unassigned stale-image
	// prewarmed agents were recycled in the (project, role) pool on a release.
	RecordPrewarmedAgentCycled(project, role string, count int)
}

// AssignRecorder observes prewarmed-pool assignment outcomes for the
// prewarmed_assigns_total hit/empty/error counter (ka-0000000550) and the
// prewarmed_assign_duration_seconds warm-assign-latency histogram (warm-pool
// observability, ka-0000001143). Satisfied by *metrics.PoolMetrics; nil is a
// no-op. Outcome values are the metrics.PoolOutcome* constants.
type AssignRecorder interface {
	RecordPrewarmedAssign(outcome, project, role string) bool
	// RecordPrewarmedAssignDuration observes the wall-clock of one
	// AssignPrewarmed call, split by execution_target so the MicroVM warm pool's
	// assign latency (the ~1-2s warm path) is a distinct series from the K8s
	// pool's. Recorded on the same outcomes as RecordPrewarmedAssign.
	RecordPrewarmedAssignDuration(seconds float64, outcome, project, role, executionTarget string) bool
}

// Manager maintains prewarmed agent pools across multiple projects.
// Each project with a prewarmed_pool config gets its own independent pool.
type Manager struct {
	daemon  *beadsapi.Client
	cfg     *config.Config
	logger  *slog.Logger
	mu      sync.Mutex
	seq     int           // monotonic counter for unique agent names
	tracker *spawnTracker // tracks assignment rate for predictive scaling

	// drainRate caps stale-image recycles per (project,role) pool per pass.
	drainRate int
	// cycleRecorder optionally observes stale-image recycle counts for the
	// prewarmed_agents_cycled_total metric. Nil disables the metric (the log
	// line still fires).
	cycleRecorder PrewarmedCycleRecorder
	// assignRecorder optionally observes AssignPrewarmed outcomes for the
	// prewarmed_assigns_total hit/empty/error metric. Nil disables the metric.
	assignRecorder AssignRecorder
}

// New creates a multi-pool Manager.
func New(daemon *beadsapi.Client, cfg *config.Config, logger *slog.Logger) *Manager {
	return &Manager{
		daemon:    daemon,
		cfg:       cfg,
		logger:    logger,
		tracker:   newSpawnTracker(),
		drainRate: DefaultStaleDrainRate,
	}
}

// SetCycleRecorder wires the stale-image recycle metric recorder. Optional; a
// nil recorder leaves the metric disabled (the recycle log line still fires).
func (m *Manager) SetCycleRecorder(r PrewarmedCycleRecorder) {
	m.cycleRecorder = r
}

// SetAssignRecorder wires the assignment-outcome metric recorder. Optional; a
// nil recorder leaves the metric disabled (the assign log lines still fire).
func (m *Manager) SetAssignRecorder(r AssignRecorder) {
	m.assignRecorder = r
}

// recordAssign emits one assignment outcome (counter + latency histogram) when
// a recorder is wired. The outcome strings mirror metrics.PoolOutcome*
// ("hit"/"empty"/"error"); they are literals here so this package does not
// import internal/metrics (the same boundary rule as CycleRecorder). since is
// the AssignPrewarmed entry time — the histogram measures the whole call
// (bead scan + atomic flip) regardless of outcome. executionTarget splits the
// MicroVM warm pool's assign latency from the K8s pool's; on the empty/error
// paths (no picked agent) it is resolved from the requested (project,role).
func (m *Manager) recordAssign(since time.Time, outcome, project, role, executionTarget string) {
	if m.assignRecorder != nil {
		m.assignRecorder.RecordPrewarmedAssign(outcome, project, role)
		m.assignRecorder.RecordPrewarmedAssignDuration(time.Since(since).Seconds(), outcome, project, role, executionTarget)
	}
}

// prewarmedAgent represents a prewarmed agent bead from the daemon.
type prewarmedAgent struct {
	ID        string
	AgentName string
	Project   string
	Role      string
	CreatedAt time.Time

	// State is the bead's agent_state. Only {prewarmed,idle} agents are
	// recycle-eligible; anything else (assigning, working, …) is in use.
	State string
	// SourceImage is the agent image stamped at create time (empty for agents
	// minted before stamping existed → treated as stale and recycled once).
	SourceImage string
	// ExecutionTarget is the backend this member will (or did) launch on,
	// stamped at create time by createPrewarmedAgent (Phase 1, ka-0000006985).
	// Empty means the default (Kubernetes) — a runc pool member. Only
	// ExecutionTargetLambdaMicroVM members bill for idle standby and burn a
	// MaximumDurationInSeconds wall-clock cap, so this is the dimension the
	// lifetime/cost machinery (freshest-pick + age-recycle) keys on.
	ExecutionTarget string
	// RuntimeClass is the Kubernetes RuntimeClass stamped at create time
	// (createPrewarmedAgent, ka-0000003859 gap 3a); "runsc" marks a gVisor
	// standby routed into cfg.GVisorNamespace. Empty for a runc/default pool, a
	// MicroVM member, or any member minted before stamping existed — the assign
	// runtime gate falls back to effectiveRuntimeClass for those.
	RuntimeClass string
	// HasThread/HasTask mark an agent as already handed work; never recycled.
	HasThread bool
	HasTask   bool
}

// isMicrovm reports whether this pool member launches on the lambda_microvm
// backend. MicroVM members are the only ones that bill for idle standby and
// burn a wall-clock lifetime cap, so the lifetime/cost paths (prefer-freshest
// assignment, recycle-before-cap) apply only to them; K8s members keep the
// legacy FIFO-oldest, never-age-recycled behaviour.
func (a prewarmedAgent) isMicrovm() bool {
	return a.ExecutionTarget == podmanager.ExecutionTargetLambdaMicroVM
}

// poolKey identifies a single prewarmed pool by (project,role). A project may
// run several role-keyed pools concurrently (e.g. a "thread" pool and a
// "test-role" pool), so reconciliation, accounting, and predictive scaling all
// key on this pair rather than project alone.
type poolKey struct {
	project string
	role    string
}

func (k poolKey) String() string { return k.project + "/" + k.role }

// GenericPoolValue is the sentinel a prewarmed pool member carries in its
// project and/or role fields to mark it PROJECT-AGNOSTIC / ROLE-AGNOSTIC — a
// generic-warm pod that can be SPECIALIZED to any concrete (project,role) at
// assign time (W1, ka-0000007566). Runtime class (runc vs runsc) is an immutable
// pod-spec field, so a generic pool is still bucketed by runtime (W2); within a
// runtime bucket one generic pool serves many projects/roles. A member is also
// treated as generic along a dimension when that field is empty, so a pool
// warmed with no project/role set is generic by construction and no migration is
// needed. On assign the filter accepts a generic candidate for any request along
// the generic dimension while still honoring the runtime-class gate; on a HIT the
// picked pod's bead is stamped with the requested project/role plus a
// re-credential marker so it re-credentials + re-roles itself on spawn.
const GenericPoolValue = "*"

// isGenericPoolValue reports whether a prewarmed member's project/role field
// marks it agnostic along that dimension — either the explicit sentinel or an
// unset (empty) value.
func isGenericPoolValue(v string) bool {
	return v == "" || v == GenericPoolValue
}

// poolDimensionMatches reports whether a prewarmed member whose stamped value is
// have can serve a request asking for want along one dimension (project or
// role). It matches when the request does not constrain the dimension
// (want == ""), when the member is generic along it (isGenericPoolValue(have)),
// or on an exact match. This is the lifted replacement for the old
// exact-equality candidate filter (ka-0000007566): a generic-warm pod serves any
// request, while a project/role-specific pod still only serves its own
// (project,role) — so existing dedicated pools behave exactly as before.
func poolDimensionMatches(have, want string) bool {
	if want == "" || isGenericPoolValue(have) {
		return true
	}
	return have == want
}

// Reconcile performs a single reconciliation pass across all projects.
// For each project with a prewarmed_pool config:
// 1. List prewarmed agents for that project
// 2. Create new agents to reach min_pool_size
func (m *Manager) Reconcile(ctx context.Context) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	// Attribute every machine-origin write this pass makes (prewarmed-agent
	// create / close / recycle) to the system:pool-manager sink. A periodic
	// pool reconcile has no human behind it, so without this the daemon's
	// fail-closed resolver refuses each header-less static-token write with
	// HTTP 400 "operator could not be resolved" in environments where the
	// controller token is not fleet-scoped (e.g. a fresh ephemeral E2E
	// namespace with bridge tokens disabled). See poolSystemOperator and
	// ka-0000006169. OIDC / X-Operated-By still win over this override on the
	// server, so it is safe to set unconditionally.
	ctx = beadsapi.WithSystemOperator(ctx, poolSystemOperator)

	// Snapshot pool configs from the project cache under read lock. Each
	// project may declare several role-keyed pools (PrewarmedPoolConfig.Roles);
	// EffectivePools expands the config into one normalized pool per role.
	m.cfg.ProjectCacheMu.RLock()
	type rolePool struct {
		key poolKey
		cfg beadsapi.PrewarmedRolePool
		// configuredMin is the operator's RAW min_size for this pool (0 when
		// unset), captured under the same lock so the post-unlock lambda_microvm
		// opt-in guard can distinguish "sized" from "K8s-defaulted". Read via
		// ConfiguredMinSize (no lock) — safe to call while holding the RLock.
		configuredMin int
	}
	var pools []rolePool
	for name, entry := range m.cfg.ProjectCache {
		for _, rp := range entry.PrewarmedPool.EffectivePools() {
			pools = append(pools, rolePool{
				key:           poolKey{project: name, role: rp.Role},
				cfg:           rp,
				configuredMin: entry.PrewarmedPool.ConfiguredMinSize(rp.Role),
			})
		}
	}
	m.cfg.ProjectCacheMu.RUnlock()

	// lambda_microvm opt-in guard (Phase 4 cost guard, ka-0000008079 /
	// ka-0000003470): an idle MicroVM member bills continuously and burns its
	// wall-clock lifetime cap, so a MicroVM pool must be default-0/off and warm
	// members only when the operator EXPLICITLY sizes it (feature ka-0000005825,
	// cost model §3c). EffectivePools fills the K8s default of 2 for an unset
	// min_size — correct for free-to-idle K8s pods, but for a lambda_microvm pool
	// that would silently mint 2 billing VMs nobody asked for. Drop such pools
	// here (before any create/recycle) unless configuredMin > 0. effectiveExecu-
	// tionTarget re-locks ProjectCacheMu, so this runs AFTER the RUnlock above.
	// K8s pools are never dropped — their default-2 fill is preserved verbatim.
	kept := pools[:0]
	for _, pp := range pools {
		if m.effectiveExecutionTarget(pp.key.project, pp.key.role) == podmanager.ExecutionTargetLambdaMicroVM &&
			pp.configuredMin <= 0 {
			m.logger.Info("lambda_microvm pool not opted in (min_size unset/0) — keeping at 0/off",
				"project", pp.key.project, "role", pp.key.role)
			continue
		}
		kept = append(kept, pp)
	}
	pools = kept

	// List all prewarmed agents once (shared across pools). includeParked=true:
	// count parked pool members (capacity-deferred "queued" or
	// started-but-unassigned "idle") toward each pool's size, so the deficit
	// math does not mint replacements that would also strand and pile up
	// (ka-0000007786).
	allAgents, err := m.listPrewarmedAgents(ctx, true)
	if err != nil {
		return fmt.Errorf("listing prewarmed agents: %w", err)
	}

	// Group by (project,role) so each role-keyed pool is accounted separately.
	byKey := make(map[poolKey][]prewarmedAgent)
	for _, a := range allAgents {
		byKey[poolKey{project: a.Project, role: a.Role}] = append(
			byKey[poolKey{project: a.Project, role: a.Role}], a)
	}

	// Build set of enabled pool keys for fast lookup.
	enabled := make(map[poolKey]bool, len(pools))
	for _, pp := range pools {
		enabled[pp.key] = true
	}

	// Close prewarmed agents whose (project,role) pool is now disabled or
	// removed. This handles the enabled→disabled transition and role removal:
	// any prewarmed agents lingering for a pool with no active config get
	// closed so the reconciler deletes their pods.
	for key, agents := range byKey {
		if enabled[key] {
			continue
		}
		m.logger.Info("pool disabled, closing prewarmed agents",
			"project", key.project, "role", key.role, "count", len(agents))
		for _, a := range agents {
			if err := m.closePrewarmedAgent(ctx, a.ID); err != nil {
				m.logger.Warn("failed to close prewarmed agent",
					"bead", a.ID, "project", key.project, "role", key.role, "error", err)
			}
		}
	}

	if len(pools) == 0 {
		return nil
	}

	// Reconcile each (project,role) pool: scale to target (between min and
	// max), adjusted by recent spawn rate for predictive scaling.
	for _, pp := range pools {
		active := byKey[pp.key]

		// Recycle unassigned warm pods minted from a stale agent image so a
		// release self-heals the pool (ka-0000001689). Rate-capped per pass;
		// survivors drive the deficit/refill math below, which mints fresh
		// replacements on the current image. Assigned/working pods are never
		// touched.
		active = m.recycleStaleAgents(ctx, pp.key, active)

		// Recycle-before-cap (Phase 4, ka-0000003944): close unassigned
		// lambda_microvm members that have aged past recycleAge so they tear
		// down and are re-minted fresh, well before their MaximumDurationInSeconds
		// wall-clock cap and their per-run credential TTLs expire. K8s members
		// carry no cap and no idle billing, so this pass never touches them.
		// Survivors feed the deficit/refill math below, which mints replacements.
		active = m.recycleAgedMicrovmAgents(ctx, pp.key, active)

		// Compute dynamic target based on recent assignment rate.
		spawnRate := m.tracker.Rate(pp.key.String())
		target := targetPoolSize(spawnRate, pp.cfg.MinSize, pp.cfg.MaxSize)

		// Enforce max_size: close excess agents if pool was shrunk or
		// spawn rate dropped.
		if len(active) > target {
			excess := len(active) - target
			m.logger.Info("pool above target, closing excess agents",
				"project", pp.key.project, "role", pp.key.role, "current", len(active),
				"target", target, "spawn_rate", spawnRate, "closing", excess)
			// Close oldest first (FIFO order).
			sortByAge(active)
			for i := 0; i < excess; i++ {
				if err := m.closePrewarmedAgent(ctx, active[i].ID); err != nil {
					m.logger.Warn("failed to close excess agent",
						"bead", active[i].ID, "project", pp.key.project,
						"role", pp.key.role, "error", err)
				}
			}
			active = active[excess:]
		}

		deficit := target - len(active)
		if deficit <= 0 {
			continue
		}
		// Hard cap at max_size regardless of target calculation.
		if len(active)+deficit > pp.cfg.MaxSize {
			deficit = pp.cfg.MaxSize - len(active)
		}
		if deficit <= 0 {
			continue
		}

		m.logger.Info("pool below target, creating prewarmed agents",
			"project", pp.key.project, "role", pp.key.role, "current", len(active),
			"target", target, "spawn_rate", spawnRate, "creating", deficit)

		for i := 0; i < deficit; i++ {
			if err := m.createPrewarmedAgent(ctx, pp.key.project, pp.cfg); err != nil {
				m.logger.Warn("failed to create prewarmed agent",
					"project", pp.key.project, "role", pp.key.role, "error", err)
				break
			}
		}
	}

	return nil
}

// isParkedPoolMember reports whether an agent bead is an UNASSIGNED pool member
// that has left the "prewarmed" state without being handed work (ka-0000007786).
// Two transitions strand a pool member this way:
//
//   - queued: the reconciler flips a prewarmed member to agent_state=queued when
//     its pod is deferred by a capacity cap (COOP_MAX_PODS etc.). It stays in the
//     reconciler's desired set and is retried when capacity frees.
//   - idle:   a prewarmed pod that started its container before being assigned
//     reports coop "idle", flipping the bead off "prewarmed".
//
// In BOTH cases the member is still a live pool slot, but listPrewarmedAgents'
// state==prewarmed filter stopped counting it — so the pool saw a deficit and
// minted a replacement, which also stranded, ad infinitum (the leak that piled
// up 180+ shells). We identify a parked member by the pool's own minting
// convention (agentName prefix "prewarmed-") AND the absence of thread/task
// context: a member ASSIGNED off the pool carries a thread or task and is real
// work, NOT idle surplus to count or reap.
func isParkedPoolMember(b beadsapi.AgentBead) bool {
	if b.Metadata["slack_thread_ts"] != "" || b.Metadata["task_id"] != "" {
		return false
	}
	if !strings.HasPrefix(b.AgentName, "prewarmed-") {
		return false
	}
	return b.AgentState == "queued" || b.AgentState == "idle"
}

// listPrewarmedAgents returns active agent beads that belong to a prewarmed
// pool. With includeParked=false it returns only agent_state=prewarmed members
// (the assignable warm pods — AssignPrewarmed's view). With includeParked=true
// it ALSO returns parked pool members the pool stranded in agent_state=queued
// (capacity-deferred) or idle (started-but-unassigned) — isParkedPoolMember.
// That is Reconcile's view: counting parked members in the deficit/excess math
// stops the pool from minting replacements that would also strand and pile up
// (the 180+-shell leak, ka-0000007786).
func (m *Manager) listPrewarmedAgents(ctx context.Context, includeParked bool) ([]prewarmedAgent, error) {
	// Lean: the pool reads only runtime keys (gd-zXdZq4Traf).
	beads, err := m.daemon.ListAgentBeadsLean(ctx)
	if err != nil {
		return nil, err
	}

	var result []prewarmedAgent
	for _, b := range beads {
		if b.AgentState != "prewarmed" && !(includeParked && isParkedPoolMember(b)) {
			continue
		}
		// Age comes from the daemon's authoritative bead-creation timestamp
		// (AgentBead.CreatedAt, "used for FIFO scheduling"). createPrewarmedAgent
		// never writes a created_at FIELD, so Metadata["created_at"] is absent in
		// production — reading it alone left every pool member at a zero CreatedAt,
		// silently disabling FIFO/freshest pick ordering, MicroVM age-recycle, and
		// sortByAge (ka-0000007925). Prefer the struct field; keep the metadata
		// read as a fallback for any bead that does stamp the field.
		created := b.CreatedAt
		if created.IsZero() {
			created = parseTime(b.Metadata["created_at"])
		}
		result = append(result, prewarmedAgent{
			ID:              b.ID,
			AgentName:       b.AgentName,
			Project:         b.Project,
			Role:            b.Role,
			CreatedAt:       created,
			State:           b.AgentState,
			SourceImage:     b.Metadata["source_image"],
			ExecutionTarget: b.Metadata["execution_target"],
			RuntimeClass:    b.Metadata["runtime_class"],
			HasThread:       b.Metadata["slack_thread_ts"] != "",
			HasTask:         b.Metadata["task_id"] != "",
		})
	}

	return result, nil
}

// createPrewarmedAgent creates a new agent bead in prewarmed state for a
// (project,role) pool.
func (m *Manager) createPrewarmedAgent(ctx context.Context, project string, poolCfg beadsapi.PrewarmedRolePool) error {
	m.seq++
	agentName := fmt.Sprintf("prewarmed-%d-%d", time.Now().Unix(), m.seq)

	fields := map[string]string{
		"agent":       agentName,
		"mode":        poolCfg.Mode,
		"role":        poolCfg.Role,
		"project":     project,
		"agent_state": "prewarmed",
	}
	// Stamp the execution target so the spawn routes to the right backend when
	// the reconciler/event handler later builds this member's runtime. A pool
	// bead carries no per-agent override, so this resolves the project<role
	// layering up front and pins it — without it the bead has no
	// execution_target field and microvm.IsMicrovmAgent returns false, so a
	// lambda_microvm project's warm members would silently launch as K8s pods
	// (ka-0000006985). Kubernetes (the default) is left unstamped to match the
	// existing K8s pool behaviour and keep the field absent for runc pools.
	target := m.effectiveExecutionTarget(project, poolCfg.Role)
	if target == podmanager.ExecutionTargetLambdaMicroVM {
		fields["execution_target"] = target
	}
	// Stamp the agent image this pod will be created from so a later release
	// can detect it as stale and recycle it (ka-0000001689). Mirrors the image
	// resolution in cmd/controller/podspec.go (cfg.CoopImage < project < role).
	if img := m.effectiveImage(project, poolCfg.Role); img != "" {
		fields["source_image"] = img
	}
	// Stamp the resolved Kubernetes RuntimeClass so the standby pod is pinned to
	// the runtime this (project,role) pool requires — e.g. "runsc" for a
	// gvisor-poc pool, which BuildSpecFromBeadInfo honors at highest precedence
	// (podspec.go metadata["runtime_class"]) and routeGVisorNamespace then routes
	// into cfg.GVisorNamespace (gardenio-gvisor). Resolving the layering up front
	// and pinning it on the bead — exactly as execution_target/source_image above
	// — makes the runsc standby spawn robust against role-cache drift between
	// create and the reconciler/event-handler build: without the stamp a
	// momentarily-empty RoleCache (controller restart, role bead rename) would
	// silently rebuild the member as a runc pod in the baseline namespace, the
	// same class of restart-revert bug podspec.go already guards for model/
	// runtime_class (ka-0000003859 gap 3a). Empty (a runc/default pool, or a
	// MicroVM member with no pod runtime) is left unstamped to keep the field
	// absent, matching the pre-existing behaviour.
	//
	// The assign gate also reads this stamped runtime_class (via candidateRuntime
	// Class) so a GENERIC-warm pod carrying a sentinel/empty project+role still
	// gates on its ACTUAL baked runtime rather than effectiveRuntimeClass("","")
	// resolving to the cluster default (ka-0000007566).
	if rc := m.effectiveRuntimeClass(project, poolCfg.Role); rc != "" {
		fields["runtime_class"] = rc
	}
	fieldsJSON, err := json.Marshal(fields)
	if err != nil {
		return fmt.Errorf("marshalling agent fields: %w", err)
	}

	labels := []string{"prewarmed", "project:" + project}

	beadID, err := m.daemon.CreateBead(ctx, beadsapi.CreateBeadRequest{
		Title:       agentName,
		Type:        "agent",
		Description: "Prewarmed agent ready for assignment",
		Fields:      json.RawMessage(fieldsJSON),
		Labels:      labels,
	})
	if err != nil {
		return fmt.Errorf("creating prewarmed agent bead: %w", err)
	}

	// Add role label for advice matching.
	_ = m.daemon.AddLabel(ctx, beadID, "role:"+poolCfg.Role)

	m.logger.Info("created prewarmed agent",
		"bead", beadID, "agent", agentName,
		"project", project, "role", poolCfg.Role)
	return nil
}

// closePrewarmedAgent closes a prewarmed agent bead so the reconciler
// deletes its pod.
func (m *Manager) closePrewarmedAgent(ctx context.Context, beadID string) error {
	status := "closed"
	if err := m.daemon.UpdateBead(ctx, beadID, beadsapi.UpdateBeadRequest{
		Status: &status,
	}); err != nil {
		return fmt.Errorf("closing prewarmed agent bead %s: %w", beadID, err)
	}
	m.logger.Info("closed prewarmed agent", "bead", beadID)
	return nil
}

// effectiveExecutionTarget resolves the runtime backend a member of this
// (project,role) pool will launch on, mirroring cmd/controller/podspec.go's
// resolveExecutionTarget layering: project override < role override (a
// prewarmed bead carries no per-agent override). An unrecognized value falls
// back to Kubernetes, matching the event-handler's routing. This is the value
// stamped on new prewarmed beads so the spawn routes to the correct backend.
func (m *Manager) effectiveExecutionTarget(project, role string) string {
	if m.cfg == nil {
		return podmanager.ExecutionTargetKubernetes
	}
	target := ""
	m.cfg.ProjectCacheMu.RLock()
	if entry, ok := m.cfg.ProjectCache[project]; ok && entry.ExecutionTarget != "" {
		target = entry.ExecutionTarget
	}
	m.cfg.ProjectCacheMu.RUnlock()
	// Role layer (overrides project). Multi-role: first matching role that sets
	// a value wins, mirroring resolveExecutionTarget.
	m.cfg.RoleCacheMu.RLock()
	for _, r := range strings.Split(role, ",") {
		entry, ok := m.cfg.RoleCache[strings.TrimSpace(r)]
		if !ok {
			continue
		}
		if entry.ExecutionTarget != "" {
			target = entry.ExecutionTarget
			break
		}
	}
	m.cfg.RoleCacheMu.RUnlock()
	if target == podmanager.ExecutionTargetLambdaMicroVM {
		return podmanager.ExecutionTargetLambdaMicroVM
	}
	return podmanager.ExecutionTargetKubernetes
}

// effectiveImage resolves the agent image a pod for this (project,role) pool
// will be created from, mirroring cmd/controller/podspec.go's layering:
// cfg.CoopImage < project override < role override. This is the value stamped
// on new prewarmed beads and compared against on a release to detect staleness.
func (m *Manager) effectiveImage(project, role string) string {
	if m.cfg == nil {
		return ""
	}
	img := m.cfg.CoopImage
	m.cfg.ProjectCacheMu.RLock()
	if entry, ok := m.cfg.ProjectCache[project]; ok && entry.Image != "" {
		img = entry.Image
	}
	m.cfg.ProjectCacheMu.RUnlock()
	m.cfg.RoleCacheMu.RLock()
	if entry, ok := m.cfg.RoleCache[role]; ok && entry.Image != "" {
		img = entry.Image
	}
	m.cfg.RoleCacheMu.RUnlock()
	return img
}

// effectiveImageOverride resolves ONLY the per-project/role agent-image OVERRIDE
// for this (project,role) pool — project override < role override — and
// deliberately EXCLUDES the cfg.CoopImage base. It returns "" when neither layer
// overrides the image, i.e. the pool runs the single base agent image.
//
// This is the image-unification (W4, ka-0000001019) counterpart to
// effectiveRuntimeClass: the assign gate uses it to keep the base-image
// generic-warm pool (which W1 lets serve many projects/roles, ka-0000007566)
// from being handed to a pool that requires a DIFFERENT, baked image. The
// container image is a pod-spec field fixed at create — like RuntimeClass it
// cannot be swapped on a running pod — so a project/role that overrides the image
// forms its own bucket exactly as runc-vs-runsc does (W2), while everything on
// the base image shares one generic pool. Project/role differentiation that does
// NOT need a distinct image (credentials, role advice, env) is instead applied
// lazily at assign time by W1's assign-time specialization, so the single base
// image needs no project-specific layer at boot.
//
// It compares only the OVERRIDE layer, never cfg.CoopImage, so a release bumping
// the base version (handled separately by recycleStaleAgents, ka-0000001689) does
// not spuriously make base-image members read as image-incompatible here.
func (m *Manager) effectiveImageOverride(project, role string) string {
	if m.cfg == nil {
		return ""
	}
	img := ""
	m.cfg.ProjectCacheMu.RLock()
	if entry, ok := m.cfg.ProjectCache[project]; ok && entry.Image != "" {
		img = entry.Image
	}
	m.cfg.ProjectCacheMu.RUnlock()
	m.cfg.RoleCacheMu.RLock()
	if entry, ok := m.cfg.RoleCache[role]; ok && entry.Image != "" {
		img = entry.Image
	}
	m.cfg.RoleCacheMu.RUnlock()
	return img
}

// memberImageOverride returns the per-project/role image OVERRIDE a specific pool
// member was built with (empty for a base-image member, including any generic
// pod). It re-resolves the (project,role) override layering via
// effectiveImageOverride: unlike runtime_class the full baked image is stamped as
// source_image (a base-or-override value that also carries the release version),
// so it cannot be decomposed back into the override layer here; the override layer
// is stable project/role config, so re-resolving it is robust in practice. A
// generic member (sentinel/empty project+role) has no cache entry and correctly
// resolves to "" — the base image — so it only ever satisfies base-image requests.
func (m *Manager) memberImageOverride(a prewarmedAgent) string {
	return m.effectiveImageOverride(a.Project, a.Role)
}

// effectiveRuntimeClass resolves the Kubernetes RuntimeClass a pod for this
// (project,role) pool will be created with, mirroring cmd/controller/podspec.go's
// layering: project override < role override (a prewarmed bead carries no
// per-agent runtime_class). Empty means the cluster default (runc). This lets the
// assign path verify that a candidate's eventual pod runtime matches the runtime a
// runsc-pinned channel requires, so a runc warm pod is never handed to a channel
// that needs gVisor freeze/thaw — and, conversely, a runsc-role pooled agent IS
// assignable to such a channel instead of forcing a cold mint (ka-0000004563).
func (m *Manager) effectiveRuntimeClass(project, role string) string {
	if m.cfg == nil {
		return ""
	}
	// A lambda_microvm member is a Firecracker VM, not a K8s pod — the
	// RuntimeClass (a runc/runsc pod concept) does not apply. Return empty so
	// the runsc-channel assign guard treats these members as unconstrained by
	// pod runtime, never comparing them against a K8s RuntimeClass requirement.
	if m.effectiveExecutionTarget(project, role) == podmanager.ExecutionTargetLambdaMicroVM {
		return ""
	}
	rc := ""
	m.cfg.ProjectCacheMu.RLock()
	if entry, ok := m.cfg.ProjectCache[project]; ok && entry.RuntimeClassName != "" {
		rc = entry.RuntimeClassName
	}
	m.cfg.ProjectCacheMu.RUnlock()
	m.cfg.RoleCacheMu.RLock()
	if entry, ok := m.cfg.RoleCache[role]; ok && entry.RuntimeClassName != "" {
		rc = entry.RuntimeClassName
	}
	m.cfg.RoleCacheMu.RUnlock()
	return rc
}

// memberRuntimeClass returns the Kubernetes RuntimeClass a specific pool member
// runs on. It prefers the value stamped on the bead at create time
// (createPrewarmedAgent, ka-0000003859 gap 3a) — the runtime the pod was
// actually built with — and falls back to re-resolving the (project,role)
// layering via effectiveRuntimeClass only when the stamp is absent (a member
// minted before stamping existed). This keeps the assign runtime gate matching
// each member against its real runtime even if the RoleCache drifts between the
// member's creation and the assign.
//
// It is also what keeps the runtime gate correct for a GENERIC-warm pod: a
// sentinel/empty project+role member would otherwise resolve to the cluster
// default via effectiveRuntimeClass("",""), losing its ACTUAL baked runtime
// (ka-0000007566).
func (m *Manager) memberRuntimeClass(a prewarmedAgent) string {
	if a.RuntimeClass != "" {
		return a.RuntimeClass
	}
	return m.effectiveRuntimeClass(a.Project, a.Role)
}

// recycleStaleAgents closes up to drainRate unassigned prewarmed agents in the
// pool whose stamped source image differs from the pool's current effective
// image, so a release self-heals the warm pool instead of handing threads
// old-image pods (ka-0000001689). It returns the agents that remain after
// recycling, so the caller's deficit/refill math runs against the survivors and
// mints replacements on the current image in the same pass.
//
// Safety: only {prewarmed,idle,queued} pool members with no thread and no task
// are eligible — assigned/working pods and any queued-WITH-work member (carries
// a thread/task) are never touched. The {prewarmed,idle,queued} set mirrors
// isParkedPoolMember's definition of a reclaimable pool member: a prewarmed
// member whose pod was capacity-deferred flips to agent_state=queued (or, once
// its container starts unassigned, idle), and a stale queued surplus member must
// still be recycled — otherwise it lingers on the OLD image after a release
// until the 1h queue-expiry, and (under a saturated COOP_MAX_PODS cap) never
// converges (ka-0000006901). Agents minted before stamping existed (empty
// source_image) read as stale and are recycled once. When the current image is
// unknown (empty), nothing is recycled (we cannot tell stale from fresh, so fail
// safe).
func (m *Manager) recycleStaleAgents(ctx context.Context, key poolKey, agents []prewarmedAgent) []prewarmedAgent {
	current := m.effectiveImage(key.project, key.role)
	if current == "" {
		return agents
	}

	// Recycle oldest-first so the longest-stale pods drain first.
	sortByAge(agents)

	rate := m.drainRate
	if rate <= 0 {
		rate = DefaultStaleDrainRate
	}

	survivors := make([]prewarmedAgent, 0, len(agents))
	cycled := 0
	for _, a := range agents {
		eligible := cycled < rate &&
			(a.State == "prewarmed" || a.State == "idle" || a.State == "queued") &&
			!a.HasThread && !a.HasTask &&
			a.SourceImage != current
		if !eligible {
			survivors = append(survivors, a)
			continue
		}
		if err := m.closePrewarmedAgent(ctx, a.ID); err != nil {
			m.logger.Warn("failed to recycle stale-image prewarmed agent",
				"bead", a.ID, "project", key.project, "role", key.role,
				"stamped_image", a.SourceImage, "current_image", current, "error", err)
			survivors = append(survivors, a)
			continue
		}
		m.logger.Info("recycled stale-image prewarmed agent",
			"bead", a.ID, "agent", a.AgentName, "project", key.project, "role", key.role,
			"stamped_image", a.SourceImage, "current_image", current)
		cycled++
	}

	if cycled > 0 && m.cycleRecorder != nil {
		m.cycleRecorder.RecordPrewarmedAgentCycled(key.project, key.role, cycled)
	}
	return survivors
}

// microvmRecycleAge returns the standby age past which an unassigned
// lambda_microvm pool member is recycled before it reaches its wall-clock
// lifetime cap (Phase 4, ka-0000003944).
//
// Precedence:
//   - An explicit PrewarmedMicrovmMaxAge (>0) wins verbatim — an operator can
//     pin an absolute cadence.
//   - Otherwise it derives from the VM lifetime cap: half of the effective
//     MicrovmMaximumDurationSeconds (default 3600s → 30m). Deriving from the cap
//     means the knob self-adjusts as the cap is raised (ka-0000002537
//     3600s→8h30m) and guarantees an assigned member always has ≥50% of its run
//     budget remaining. Half the cap is also always far under every per-run
//     credential TTL (S3 ~12h, beads PAT 24h, git ~48h), so recycle-before-cap
//     subsumes recycle-before-credential-expiry.
//
// Returns 0 when no cap can be derived (both the knob and the resolved lifetime
// are non-positive), which disables age-recycling — fail-open to the pre-Phase-4
// behaviour rather than churning the pool on a bad config.
func (m *Manager) microvmRecycleAge() time.Duration {
	if m.cfg != nil && m.cfg.PrewarmedMicrovmMaxAge > 0 {
		return m.cfg.PrewarmedMicrovmMaxAge
	}
	capSecs := defaultMicrovmMaximumDurationSeconds
	if m.cfg != nil && m.cfg.MicrovmMaximumDurationSeconds > 0 {
		capSecs = m.cfg.MicrovmMaximumDurationSeconds
	}
	if capSecs <= 0 {
		return 0
	}
	return time.Duration(capSecs) * time.Second / 2
}

// recycleAgedMicrovmAgents closes unassigned lambda_microvm pool members whose
// standby age exceeds microvmRecycleAge and returns the survivors, so the
// caller's deficit/refill math mints fresh replacements in the same pass
// (Phase 4 recycle-before-cap, ka-0000003944).
//
// Why MicroVM-only: a parked K8s pod carries no wall-clock lifetime cap and
// bills nothing while idle beyond its held node resources, so the legacy
// pattern (keep it until stale-image or excess recycling drains it) is correct
// for K8s. A lambda_microvm member is different on both axes — it burns a
// MaximumDurationInSeconds cap counted from Launch AND bills for its entire
// idle standby life — so it must be retired and re-minted before either the cap
// or its per-run credential TTLs are reached. Closing the bead is what triggers
// the reconciler/leak-GC to terminate the VM (its bead leaves the live set).
//
// Safety mirrors recycleStaleAgents exactly: only {prewarmed,idle} members with
// no thread and no task are eligible — an assigned/working member (which carries
// a thread or task) is real work and never touched. Rate-capped per pass by the
// same drainRate so a burst of aged members drains smoothly instead of emptying
// the pool in one tick. When recycleAge<=0 the pass is disabled (returns the
// input unchanged).
func (m *Manager) recycleAgedMicrovmAgents(ctx context.Context, key poolKey, agents []prewarmedAgent) []prewarmedAgent {
	recycleAge := m.microvmRecycleAge()
	if recycleAge <= 0 {
		return agents
	}

	// Recycle oldest-first so the closest-to-cap members drain before newer ones.
	sortByAge(agents)

	rate := m.drainRate
	if rate <= 0 {
		rate = DefaultStaleDrainRate
	}

	now := time.Now()
	survivors := make([]prewarmedAgent, 0, len(agents))
	cycled := 0
	for _, a := range agents {
		// A member with no parseable created_at has zero age here; treat it as
		// not-yet-aged (never recycle on a missing timestamp) — the stale-image
		// recycler already handles never-stamped members.
		aged := !a.CreatedAt.IsZero() && now.Sub(a.CreatedAt) >= recycleAge
		eligible := cycled < rate &&
			a.isMicrovm() &&
			(a.State == "prewarmed" || a.State == "idle") &&
			!a.HasThread && !a.HasTask &&
			aged
		if !eligible {
			survivors = append(survivors, a)
			continue
		}
		if err := m.closePrewarmedAgent(ctx, a.ID); err != nil {
			m.logger.Warn("failed to recycle aged MicroVM prewarmed agent",
				"bead", a.ID, "project", key.project, "role", key.role,
				"age", now.Sub(a.CreatedAt).String(), "recycle_age", recycleAge.String(),
				"error", err)
			survivors = append(survivors, a)
			continue
		}
		m.logger.Info("recycled aged MicroVM prewarmed agent",
			"bead", a.ID, "agent", a.AgentName, "project", key.project, "role", key.role,
			"age", now.Sub(a.CreatedAt).String(), "recycle_age", recycleAge.String())
		cycled++
	}

	if cycled > 0 && m.cycleRecorder != nil {
		m.cycleRecorder.RecordPrewarmedAgentCycled(key.project, key.role, cycled)
	}
	return survivors
}

// sortByAge sorts prewarmed agents by creation time (oldest first).
func sortByAge(agents []prewarmedAgent) {
	for i := 1; i < len(agents); i++ {
		for j := i; j > 0 && agents[j].CreatedAt.Before(agents[j-1].CreatedAt); j-- {
			agents[j], agents[j-1] = agents[j-1], agents[j]
		}
	}
}

// AssignRequest holds the parameters for assigning a prewarmed agent.
type AssignRequest struct {
	// Thread context for the assigned work.
	Channel     string `json:"channel"`
	ThreadTS    string `json:"thread_ts"`
	Description string `json:"description"`
	Project     string `json:"project"`
	// Role selects which (project,role) pool to assign from. When set, only
	// prewarmed agents of that role are eligible — this carries the Slack
	// channel_roles override through the fast path so a thread in a role-mapped
	// channel gets a warm pod of the right role. Empty means any role in the
	// project (legacy single-pool behavior).
	Role string `json:"role,omitempty"`
	// TaskID is an optional pre-assigned task bead ID. When set, it is written
	// to the agent bead's task_id field so the entrypoint can hydrate
	// BOAT_TASK_ID after leaving standby mode.
	TaskID string `json:"task_id,omitempty"`
	// OperatedBy is the human-operator id (slack:U… / oidc:…@…) on whose
	// behalf the assigned agent will act. A prewarmed pod bakes
	// BEADS_OPERATED_BY empty at create time, so the only way it can attribute
	// kd/gb writes is to read fields.operated_by off its own agent bead (the
	// reader-fallback shipped in MR !3874). Stamping it here — atomically,
	// before the assign returns and the nudge fires — closes the window where
	// the agent starts working before the bridge's separate post-assign
	// UpdateBeadFields lands, which previously 400'd attributed writes with
	// "operator could not be resolved". Empty is tolerated: the bridge's
	// post-assign write still sets it, and the reader-fallback only accepts
	// well-formed human shapes regardless. See ka-0000000528 / ka-0000008135.
	OperatedBy string `json:"operated_by,omitempty"`
	// ThreadID is the Slack-thread (wish) head bead id for the assigned work.
	// When set, it is stamped as fields.thread_id (+ its legacy alias
	// slack_thread_bead_id) in the SAME atomic UpdateBeadFields that flips
	// agent_state — the identical race-close applied to OperatedBy above. A
	// prewarmed pod carries no thread_id at create time, so without this stamp
	// the only writer is the bridge's separate post-assign UpdateBeadFields,
	// which races the pod leaving standby: the pod's first `kd create` can run
	// before thread_id lands, and the daemon's transitive thread_id copy
	// (maybeCopyThreadID) then reads an empty pointer and no-ops, orphaning the
	// new bead from its thread. Stamping it atomically here closes that window
	// so thread-spawned derivative beads reliably inherit the thread pointer and
	// become discoverable to a resumed sibling (ka-0000001160). Empty is
	// tolerated: the bridge's post-assign write still sets it.
	ThreadID string `json:"thread_id,omitempty"`
	// RequireRuntimeClass constrains the assign to prewarmed agents whose
	// eventual pod runtime (resolved from project/role beads via
	// effectiveRuntimeClass) equals this value. A runsc-pinned channel
	// (channel_runtime_class, e.g. gb-turbo) sets "runsc" so it only ever
	// claims a gVisor-freeze-eligible warm pod — never a runc one, which would
	// forfeit freeze/thaw. Empty means "any runtime" (legacy behavior): default
	// runc channels match every pool. This is the dimension that lets runsc
	// channels warm-assign a NEW agent off the pool instead of paying a cold
	// mint on first mention (ka-0000004563).
	RequireRuntimeClass string `json:"require_runtime_class,omitempty"`
	// ResumeS3Key is the S3 session prefix (the prior thread agent's canonical
	// coop session key) whose transcript this warm assignment should RESUME.
	// When set, it is stamped as fields.warm_resume_s3_key in the SAME atomic
	// UpdateBeadFields that flips agent_state, so the assigned pod reads it off
	// its own bead the instant it leaves standby (init_agent standbyWait), adopts
	// it as coop's COOP_S3_SESSION_KEY, and relaunches its coop session keyed on
	// the prior transcript — a real coop --resume on the already-warm pod
	// (ka-0000000983). A warm pool pod boots coop under its OWN empty S3 prefix,
	// so without this hand-off a warm follow-up starts a fresh session with no
	// prior conversation; this field is what grafts the transcript onto the fast
	// path. Empty means "no resume" — today's behavior (fresh warm session),
	// preserved exactly, so the field is inert unless the bridge opts in
	// (WARM_POOL_RESUME_ENABLED). The bridge only sends it for a thread that has
	// a resolvable prior transcript.
	ResumeS3Key string `json:"resume_s3_key,omitempty"`
	// Model is the canonical LiteLLM model id (alias-KEY form, e.g.
	// claude-opus-5) the assigned agent should run on. When set, it is stamped
	// as fields.model in the SAME atomic UpdateBeadFields that flips agent_state,
	// so podspec.go's applyAgentModelOverride bakes ANTHROPIC_MODEL/CLAUDE_MODEL
	// from the agent's own bead. This carries the pool-assign protocol's model
	// awareness for the `gb pool assign --model` CLI and future
	// model-partitioned pools. Empty means "no override" — the assigned pod keeps
	// the account/role default baked at prewarm time.
	//
	// NOTE (design D1, ka-0000000406): the Slack @mention spawn path does NOT
	// exercise this via a pool HIT. A prewarmed pod bakes its model at prewarm
	// time and cannot be re-modeled, so a --model mention deliberately BYPASSES
	// the pool and falls through to a cold mint that stamps fields.model itself.
	// This field makes the protocol model-aware for the CLI / future partitioned
	// pools; it is not a promise that an already-booted warm pod switches models.
	Model string `json:"model,omitempty"`
}

// AssignResult is returned when a prewarmed agent is successfully assigned.
type AssignResult struct {
	BeadID    string `json:"bead_id"`
	AgentName string `json:"agent_name"`
}

// ErrPoolEmpty is returned when no prewarmed agents are available for assignment.
var ErrPoolEmpty = fmt.Errorf("no prewarmed agents available")

// AssignPrewarmed atomically picks a prewarmed agent from the pool and
// transitions it to "assigning" state with the given thread context.
// Returns ErrPoolEmpty if no prewarmed agents are available for the project.
func (m *Manager) AssignPrewarmed(ctx context.Context, req AssignRequest) (*AssignResult, error) {
	// assignStart bounds the warm-assign latency window
	// (gardenio_prewarmed_assign_duration_seconds): everything from here — the
	// bead scan and the atomic agent_state flip — to a returned result or error.
	// The WARM-path counterpart to the cold launch histogram; recorded on every
	// exit below via recordAssign.
	assignStart := time.Now()
	m.mu.Lock()
	defer m.mu.Unlock()

	// The assign PATCH is performed BY the controller (a machine action); the
	// human operator on whose behalf the pod will act is carried separately in
	// fields.operated_by (set below from req.OperatedBy) and resolved by the
	// agent's own reader-fallback on its first write. The write here still
	// needs a resolvable operator or the daemon fail-closes it (HTTP 400) in
	// environments where the controller token is not fleet-scoped — the same
	// ephemeral-ns exposure as the reconcile path (ka-0000006169). Bind the
	// system:pool-manager sink so the assign write always commits.
	ctx = beadsapi.WithSystemOperator(ctx, poolSystemOperator)

	// includeParked=false: only agent_state=prewarmed members are assignable —
	// a parked (queued/idle) member is not a ready warm pod (ka-0000007786).
	agents, err := m.listPrewarmedAgents(ctx, false)
	if err != nil {
		m.recordAssign(assignStart, "error", req.Project, req.Role, m.effectiveExecutionTarget(req.Project, req.Role))
		return nil, fmt.Errorf("listing prewarmed agents: %w", err)
	}

	// Filter to candidates that can serve the requested (project,role). The old
	// filter required exact project AND role equality, so a pod warmed for
	// project A / role X could never serve a request for project B / role Y. The
	// lifted filter (W1, ka-0000007566) accepts a GENERIC-warm pod (sentinel/empty
	// project and/or role) for any request along the generic dimension, while a
	// project/role-specific pod still only serves its own (project,role) — so
	// existing dedicated pools behave exactly as before. Project and role remain
	// each optional in the request: an empty value matches any (legacy single-pool
	// behavior); a non-empty value is honored so a dedicated pod of the wrong
	// (project,role) is still skipped.
	var candidates []prewarmedAgent
	for _, a := range agents {
		if !poolDimensionMatches(a.Project, req.Project) {
			continue
		}
		if !poolDimensionMatches(a.Role, req.Role) {
			continue
		}
		// Runtime gate (ka-0000004563): a runsc-pinned channel must only claim a
		// warm pod that will itself run on runsc — otherwise it grabs a runc pod
		// and forfeits freeze/thaw. runc vs runsc is immutable at prewarm, so this
		// gate is NOT lifted for generic pods — a generic pool is still bucketed by
		// runtime (W2, ka-0000007566). Prefer the runtime stamped on the member at
		// create time (ka-0000003859 gap 3a) — that is the runtime the pod was
		// actually built with — and fall back to re-resolving project/role beads
		// only for members minted before stamping existed, so a mid-flight
		// RoleCache change (or a generic pod whose sentinel project+role would
		// resolve to the cluster default) can never mismatch a member against the
		// runtime it is really running. Empty RequireRuntimeClass keeps legacy
		// behavior (default runc channels match any pool).
		if req.RequireRuntimeClass != "" &&
			m.memberRuntimeClass(a) != req.RequireRuntimeClass {
			continue
		}
		// Image gate (W4 image unification, ka-0000001019): the container image is
		// a pod-spec field baked at create and cannot be swapped on a running pod,
		// so — exactly like the runtime gate above — a member may only serve a
		// request whose (project,role) resolves to the SAME image. Compared on the
		// project/role OVERRIDE layer only (not cfg.CoopImage), so this is a pure
		// no-op while every pool runs the single base image (the state today), and
		// release-version drift stays the recycler's concern (ka-0000001689). When a
		// project/role DOES override the image, this keeps a base-image generic-warm
		// pod (W1, ka-0000007566) out of that pool — it would otherwise be handed a
		// pod running the wrong, un-swappable image — and keeps an override-image pod
		// from serving a base-image request. Such an override pool simply forms its
		// own bucket, the same partition runc-vs-runsc forces (W2). A mismatch here
		// falls through to a cold mint on the correct image rather than a wrong-image
		// warm assign.
		if m.effectiveImageOverride(req.Project, req.Role) != m.memberImageOverride(a) {
			continue
		}
		candidates = append(candidates, a)
	}
	if len(candidates) == 0 {
		m.recordAssign(assignStart, "empty", req.Project, req.Role, m.effectiveExecutionTarget(req.Project, req.Role))
		return nil, ErrPoolEmpty
	}

	// Per-runtimeClass bucket partition (W2, ka-0000003380). runc vs runsc is an
	// immutable pod-spec field, so warm pods form one generic pool PER runtime
	// bucket — a runc pod can never serve a runsc freeze/thaw channel and vice
	// versa. The runsc bucket is the scarce/expensive one (gVisor freeze/thaw
	// warm-B placeholders); a runtime-UNCONSTRAINED request (a default runc
	// channel sends RequireRuntimeClass="", meaning "any runtime", ka-0000004563)
	// must not needlessly drain it when a same-bucket (runc/default) warm pod is
	// available, or a burst of default mentions could starve the runsc channels
	// the runsc bucket was warmed for. When the request PINS a runtime the gate
	// above already confined candidates to that one bucket, so this preference is
	// a no-op there. We keep the cross-bucket FALLBACK — a runsc pod still serves
	// a default request when no runc pod is warm — so warm-hit rate never
	// regresses; we only prefer the request's home bucket when it has members.
	// memberRuntimeClass is the per-member bucket key (the create-time stamp,
	// falling back to effectiveRuntimeClass) — the SAME resolution the assign gate
	// and warm-time stamping key on, so bucket selection is driven end-to-end by
	// effectiveRuntimeClass at both warm-time and assign-time.
	if req.RequireRuntimeClass == "" {
		var homeBucket []prewarmedAgent
		for _, a := range candidates {
			if m.memberRuntimeClass(a) == "" {
				homeBucket = append(homeBucket, a)
			}
		}
		if len(homeBucket) > 0 {
			candidates = homeBucket
		}
	}

	// Pick a prewarmed agent. Ordering is execution-target aware (Phase 4,
	// ka-0000003944):
	//
	//   - Kubernetes (default): FIFO-oldest. A parked pod carries no wall-clock
	//     lifetime cap and no idle billing, so handing out the longest-parked
	//     member first maximizes churn/reuse and lets the stale-image recycler
	//     drain aged pods predictably.
	//   - lambda_microvm: FRESHEST (newest created_at). A MicroVM member burns a
	//     MaximumDurationInSeconds wall-clock cap and its per-run Ziti lease + SM
	//     credentials expire on TTLs anchored at Launch; standby time eats into
	//     the assigned run's budget. Picking the freshest member hands the caller
	//     the most remaining run budget and the longest credential headroom,
	//     and pairs with recycleAgedMicrovmAgents (which drains the oldest
	//     members before they reach the cap) so the pool self-selects toward
	//     fresh capacity.
	//
	// A pool is single-backend per (project,role) — every candidate here shares
	// req.Project/req.Role, and execution_target resolves from that pair — so the
	// first candidate's target decides the ordering for the whole set.
	preferFreshest := candidates[0].isMicrovm()
	pick := candidates[0]
	for _, a := range candidates[1:] {
		if a.CreatedAt.IsZero() {
			continue
		}
		if pick.CreatedAt.IsZero() {
			pick = a
			continue
		}
		if preferFreshest {
			if a.CreatedAt.After(pick.CreatedAt) {
				pick = a
			}
		} else if a.CreatedAt.Before(pick.CreatedAt) {
			pick = a
		}
	}

	// servedProject/servedRole are the (project,role) the picked pod will now
	// serve — the request's values when the pod was generic along a dimension,
	// else the pod's own. Metrics and predictive scaling key on these so a
	// generic-warm HIT is attributed to the pool that actually got served, not to
	// the generic sentinel (ka-0000007566).
	servedProject := pick.Project
	if isGenericPoolValue(pick.Project) && req.Project != "" {
		servedProject = req.Project
	}
	servedRole := pick.Role
	if isGenericPoolValue(pick.Role) && req.Role != "" {
		servedRole = req.Role
	}

	// Update the bead: set agent_state to "assigning" and add thread context.
	fields := map[string]string{
		"agent_state":  "assigning",
		"spawn_source": "slack-thread-pool",
	}
	if req.Channel != "" {
		fields["slack_thread_channel"] = req.Channel
	}
	if req.ThreadTS != "" {
		fields["slack_thread_ts"] = req.ThreadTS
	}
	if req.Project != "" {
		fields["project"] = req.Project
	}
	// Assign-time specialization (W1, ka-0000007566): stamp the requested role in
	// the SAME atomic UpdateBeadFields that flips agent_state, so a generic-warm
	// pod carries its concrete role the instant it leaves the pool. A dedicated
	// pod already carries this exact role (poolDimensionMatches required it), so
	// the write is idempotent there; a generic pod (sentinel/empty role) gets
	// specialized to the request's role. This is what makes the role label the
	// assign-time nudge / gb prime resolve advice against correct on spawn.
	if req.Role != "" {
		fields["role"] = req.Role
	}
	// Credential swap for a generic-warm pod (W1, ka-0000007566). A pod warmed
	// generically holds no project-scoped credentials/ServiceAccount, so serving a
	// concrete project needs re-credentialing — the same "re-read secrets fresh"
	// step the freeze/thaw rehydrate-ack gate performs on resume (ka-0000004088).
	// Detect specialization by the PICKED pod being generic along the project
	// dimension while the request pins a concrete project (a dedicated pod already
	// holds the right creds and needs no swap — poolDimensionMatches guarantees
	// pick.Project == req.Project there). Stamp a marker + the target project in
	// the same atomic update so the pod's standbyWait (hydrateAssignmentEnv) reads
	// a populated pointer the instant it leaves the pool — before the nudge fires
	// — and re-credentials + re-roles itself for the target project on spawn.
	//
	// NOTE (open design question, deliberately deferred — see the bead): the
	// per-project K8s ServiceAccount and ExternalSecret-mounted env are pod-spec
	// fields, immutable on a running pod. This marker drives the pod's runtime
	// re-credential of the credentials it CAN re-fetch (Claude creds via coopmux;
	// project scope via env); a full SA/token swap (mount a superset/broker token
	// or re-exec with new projected creds) is the W1 follow-up spike.
	if req.Project != "" && isGenericPoolValue(pick.Project) {
		fields["specialized_from_generic"] = "true"
		fields["recredential_project"] = req.Project
	}
	if req.TaskID != "" {
		fields["task_id"] = req.TaskID
	}
	if req.OperatedBy != "" {
		// Defense-in-depth (ka-0000000528): stamp the operator in the same
		// atomic UpdateBeadFields that flips agent_state, so a warm pod's
		// own bead carries operated_by the instant it transitions out of the
		// pool — before the nudge goroutine fires and before the bridge's
		// separate post-assign write. The reader-fallback then resolves a
		// human on the agent's very first attributed kd/gb write.
		fields["operated_by"] = req.OperatedBy
	}
	if req.ThreadID != "" {
		// Defense-in-depth (ka-0000001160): stamp the thread (wish) head id in
		// the same atomic UpdateBeadFields that flips agent_state, so a warm
		// pod's own bead carries thread_id the instant it leaves the pool —
		// before the nudge fires and before the bridge's separate post-assign
		// write. The daemon's transitive thread_id copy (maybeCopyThreadID) then
		// reads a populated pointer on the pod's first `kd create`, so
		// thread-spawned derivative beads inherit the thread and are
		// discoverable to a resumed sibling. SetWishPointer stamps BOTH the
		// canonical wish-pointer field and its legacy alias, mirroring the
		// bridge's post-assign shape (bot_mentions.go / bot_agent_kill.go).
		beadsapi.SetWishPointer(fields, req.ThreadID)
	}
	if req.ResumeS3Key != "" {
		// Warm-pool coop-resume graft (ka-0000000983): stamp the prior thread
		// agent's S3 session key so the assigned pod resumes that transcript on
		// relaunch. Written in the SAME atomic UpdateBeadFields that flips
		// agent_state so the pod's standbyWait reads a populated pointer the
		// instant it leaves the pool — before the nudge fires — the same
		// race-close applied to operated_by / thread_id above. Empty (the common
		// case) leaves warm assignment exactly as before: a fresh warm session.
		fields["warm_resume_s3_key"] = req.ResumeS3Key
	}
	if req.Model != "" {
		// Pool-assign model awareness (design D1, ka-0000000406 / ka-0000006250):
		// stamp the requested model in the SAME atomic UpdateBeadFields that flips
		// agent_state, so podspec.go's applyAgentModelOverride reads it off the
		// pod's own bead and bakes ANTHROPIC_MODEL/CLAUDE_MODEL — the same
		// race-close applied to operated_by / thread_id / warm_resume_s3_key
		// above. Carried by the `gb pool assign --model` CLI and future
		// model-partitioned pools. Empty (the common case, and the ONLY case on
		// the Slack path — a --model mention bypasses the pool entirely) leaves
		// the pod on its prewarm-baked account/role default.
		fields["model"] = req.Model
	}
	if err := m.daemon.UpdateBeadFields(ctx, pick.ID, fields); err != nil {
		m.recordAssign(assignStart, "error", servedProject, servedRole, m.effectiveExecutionTarget(servedProject, servedRole))
		return nil, fmt.Errorf("updating prewarmed agent %s: %w", pick.ID, err)
	}
	// Key the hit by the SERVED pool (the request's project/role for a generic
	// pod, else the picked pod's own), not the raw pick (whose role may be empty
	// on the legacy any-role path or the generic sentinel on a lifted assign), so
	// the series matches the pool that actually served it.
	m.recordAssign(assignStart, "hit", servedProject, servedRole, m.effectiveExecutionTarget(servedProject, servedRole))

	// Update the bead description with thread context.
	if req.Description != "" {
		desc := req.Description
		if err := m.daemon.UpdateBead(ctx, pick.ID, beadsapi.UpdateBeadRequest{
			Description: &desc,
		}); err != nil {
			m.logger.Warn("failed to set description on assigned agent",
				"agent", pick.ID, "error", err)
		}
	}

	m.logger.Info("assigned prewarmed agent",
		"bead", pick.ID, "agent", pick.AgentName,
		"channel", req.Channel, "thread_ts", req.ThreadTS)

	// Record assignment for predictive scaling, keyed by the SERVED (project,role)
	// pool so one hot role does not skew another's scaling — and so a generic-warm
	// HIT drives demand for the pool it served, not the generic sentinel.
	m.tracker.Record(poolKey{project: servedProject, role: servedRole}.String())

	// Mail the agent's Claude session the work description.
	go m.nudgeAssignedAgent(pick.ID, pick.AgentName, req.Description)

	return &AssignResult{
		BeadID:    pick.ID,
		AgentName: pick.AgentName,
	}, nil
}

// RunLoop runs the multi-pool reconciler periodically until the context is cancelled.
func (m *Manager) RunLoop(ctx context.Context) {
	interval := m.cfg.PrewarmedPoolInterval
	if interval <= 0 {
		interval = 30 * time.Second
	}

	m.logger.Info("multi-pool manager started", "interval", interval)

	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	// Run immediately on start.
	if err := m.Reconcile(ctx); err != nil {
		m.logger.Warn("initial pool reconcile failed", "error", err)
	}

	for {
		select {
		case <-ctx.Done():
			m.logger.Info("multi-pool manager stopped")
			return
		case <-ticker.C:
			if err := m.Reconcile(ctx); err != nil {
				m.logger.Warn("pool reconcile failed", "error", err)
			}
		}
	}
}

// nudgeAssignedAgent sends the work description to the agent as interrupt
// mail. Its mail subscriber hands the mail to coop on loopback once it is up
// (it drains unread mail when it starts), so there is nothing to retry.
func (m *Manager) nudgeAssignedAgent(beadID, agentName, description string) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	msg := "You have been assigned work from the prewarmed pool."
	if description != "" {
		msg += "\n\n" + description
	}

	id, err := m.daemon.CreateBead(ctx, beadsapi.CreateBeadRequest{
		Title:       "You have been assigned work from the prewarmed pool.",
		Type:        "mail",
		Kind:        "data",
		Description: msg,
		Assignee:    agentName,
		Labels:      []string{mailproto.FromLabel("controller"), mailproto.LabelDeliveryInterrupt},
		CreatedBy:   "controller",
	})
	if err != nil {
		m.logger.Warn("mailing work description to assigned agent failed",
			"bead", beadID, "agent", agentName, "error", err)
		return
	}
	m.logger.Info("mailed work description to assigned agent", "bead", beadID, "agent", agentName, "mail", id)
}

func parseTime(s string) time.Time {
	if s == "" {
		return time.Time{}
	}
	for _, layout := range []string{
		time.RFC3339,
		"2006-01-02T15:04:05",
		"2006-01-02 15:04:05",
	} {
		if t, err := time.Parse(layout, s); err == nil {
			return t
		}
	}
	return time.Time{}
}
