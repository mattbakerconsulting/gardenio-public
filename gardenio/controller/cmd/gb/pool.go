package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strings"

	"gardenio/controller/internal/beadsapi"

	"github.com/spf13/cobra"
)

var poolCmd = &cobra.Command{
	Use:     "pool",
	Short:   "Manage prewarmed agent pools",
	GroupID: "orchestration",
}

var poolProject string

// ── pool status ─────────────────────────────────────────────────────

var poolStatusCmd = &cobra.Command{
	Use:   "status",
	Short: "Show prewarmed pool state per project",
	RunE:  runPoolStatus,
}

func runPoolStatus(cmd *cobra.Command, args []string) error {
	ctx := context.Background()

	projects, err := daemon.ListProjectBeads(ctx)
	if err != nil {
		return fmt.Errorf("listing projects: %w", err)
	}

	// List all prewarmed agents.
	agents, err := daemon.ListAgentBeads(ctx)
	if err != nil {
		return fmt.Errorf("listing agents: %w", err)
	}

	// Count prewarmed agents by project.
	prewarmedByProject := make(map[string]int)
	for _, a := range agents {
		if a.AgentState == "prewarmed" {
			prewarmedByProject[a.Project]++
		}
	}

	found := false
	for name, proj := range projects {
		if poolProject != "" && name != poolProject {
			continue
		}

		pool := proj.PrewarmedPool
		enabled := pool != nil && pool.Enabled

		if !enabled && poolProject == "" {
			// Skip disabled projects unless specifically requested.
			if prewarmedByProject[name] == 0 {
				continue
			}
		}

		found = true
		fmt.Fprintf(os.Stdout, "Project: %s\n", name)
		if enabled {
			fmt.Fprintf(os.Stdout, "  Enabled:   true\n")
			// A project may run several role-keyed pools. EffectivePools
			// normalizes both the single-role and multi-role configs into one
			// entry per (project,role) pool.
			effective := pool.EffectivePools()
			if len(effective) == 1 {
				p := effective[0]
				fmt.Fprintf(os.Stdout, "  Min size:  %d\n", p.MinSize)
				fmt.Fprintf(os.Stdout, "  Max size:  %d\n", p.MaxSize)
				fmt.Fprintf(os.Stdout, "  Role:      %s\n", p.Role)
				fmt.Fprintf(os.Stdout, "  Mode:      %s\n", p.Mode)
			} else {
				fmt.Fprintf(os.Stdout, "  Pools (%d):\n", len(effective))
				for _, p := range effective {
					fmt.Fprintf(os.Stdout, "    - role=%s mode=%s min=%d max=%d\n",
						p.Role, p.Mode, p.MinSize, p.MaxSize)
				}
			}
		} else {
			fmt.Fprintf(os.Stdout, "  Enabled:   false\n")
		}
		fmt.Fprintf(os.Stdout, "  Prewarmed: %d\n\n", prewarmedByProject[name])
	}

	if !found {
		if poolProject != "" {
			fmt.Fprintf(os.Stdout, "No pool configuration found for project %q\n", poolProject)
		} else {
			fmt.Fprintf(os.Stdout, "No prewarmed pools configured\n")
		}
	}

	return nil
}

// ── pool disable ────────────────────────────────────────────────────

var poolDisableCmd = &cobra.Command{
	Use:   "disable",
	Short: "Disable prewarmed pool for a project",
	Long:  "Flips enabled=false on the project bead's prewarmed_pool field, preserving the existing role/min_size/max_size/mode so re-enabling restores the same config. The pool manager will close existing prewarmed agents on its next reconcile pass.",
	RunE:  runPoolDisable,
}

func runPoolDisable(cmd *cobra.Command, args []string) error {
	project := resolvePoolProject()
	if project == "" {
		return fmt.Errorf("--project is required (or set BOAT_PROJECT)")
	}

	ctx := context.Background()
	poolCfg, projectBeadID, err := getProjectPoolConfig(ctx, project)
	if err != nil {
		return err
	}

	if poolCfg == nil {
		poolCfg = &beadsapi.PrewarmedPoolConfig{}
	}
	poolCfg.Enabled = false

	return updatePoolConfig(ctx, projectBeadID, project, poolCfg)
}

// ── pool enable ─────────────────────────────────────────────────────

var poolEnableCmd = &cobra.Command{
	Use:   "enable",
	Short: "Enable prewarmed pool for a project",
	Long: `Flips enabled=true on the project bead's prewarmed_pool field, preserving any existing role/min_size/max_size/mode. Unset fields default to role=thread, max_size=5, mode=crew, and — for a Kubernetes (default) pool — min_size=2. The pool manager will start creating prewarmed agents on its next reconcile pass.

A lambda_microvm pool is different: an idle MicroVM member bills continuously and burns a wall-clock lifetime cap, so it is default 0/off and enable does NOT fill a min_size. Set an explicit min_size (e.g. edit prewarmed_pool JSON or pass a sized config) to opt into paying for warm MicroVMs — until then enable only flips the flag and the pool stays empty.`,
	RunE: runPoolEnable,
}

func runPoolEnable(cmd *cobra.Command, args []string) error {
	project := resolvePoolProject()
	if project == "" {
		return fmt.Errorf("--project is required (or set BOAT_PROJECT)")
	}

	ctx := context.Background()
	poolCfg, projectBeadID, err := getProjectPoolConfig(ctx, project)
	if err != nil {
		return err
	}

	if poolCfg == nil {
		poolCfg = &beadsapi.PrewarmedPoolConfig{}
	}
	if poolCfg.Role == "" {
		poolCfg.Role = "thread"
	}
	if poolCfg.Mode == "" {
		poolCfg.Mode = "crew"
	}
	if poolCfg.MaxSize <= 0 {
		poolCfg.MaxSize = 5
	}
	// min_size defaulting is execution-target aware. A lambda_microvm pool is a
	// real, continuously-billing cost knob (feature ka-0000005825, cost model
	// §3c): default 0/off, explicit opt-in. Filling min_size=2 here would write an
	// EXPLICIT opt-in the operator never asked for, defeating the reconciler's
	// default-off guard — so for MicroVM we leave min_size at its stored value (0
	// when unset) and print an opt-in hint. K8s idle pods are free, so they keep
	// the historical min_size=2 default.
	microvm := resolvePoolExecutionTarget(ctx, project, poolCfg.Role) == "lambda_microvm"
	if poolCfg.MinSize <= 0 && !microvm {
		poolCfg.MinSize = 2
	}
	poolCfg.Enabled = true

	if microvm && poolCfg.MinSize <= 0 {
		fmt.Fprintf(os.Stdout, "Note: %q resolves to a lambda_microvm pool — enabled but kept at 0/off (idle MicroVMs bill continuously). Set an explicit min_size to opt into warm members.\n", project)
	}
	return updatePoolConfig(ctx, projectBeadID, project, poolCfg)
}

// resolvePoolExecutionTarget resolves the effective execution target for a
// (project,role) pool from the project and role beads, mirroring podspec.go's
// resolveExecutionTarget and poolmanager.effectiveExecutionTarget layering
// (project override < role override; the multi-role first-set-wins split). It is
// best-effort: any lookup error resolves to "" (kubernetes), so enable never
// hard-fails on a transient daemon read — the reconciler's guard is the real
// enforcement point, this only keeps the CLI's min_size defaulting honest.
func resolvePoolExecutionTarget(ctx context.Context, project, role string) string {
	target := ""
	if projects, err := daemon.ListProjectBeads(ctx); err == nil {
		if p, ok := projects[project]; ok && p.ExecutionTarget != "" {
			target = p.ExecutionTarget
		}
	}
	if roles, err := daemon.ListRoleBeads(ctx); err == nil {
		for _, r := range strings.Split(role, ",") {
			if entry, ok := roles[strings.TrimSpace(r)]; ok && entry.ExecutionTarget != "" {
				target = entry.ExecutionTarget
				break
			}
		}
	}
	return target
}

// ── pool flush ──────────────────────────────────────────────────────

var poolFlushCmd = &cobra.Command{
	Use:   "flush",
	Short: "Close all prewarmed agents for a project immediately",
	Long:  "Closes all prewarmed agent beads for the project. The reconciler will delete their pods.",
	RunE:  runPoolFlush,
}

func runPoolFlush(cmd *cobra.Command, args []string) error {
	project := resolvePoolProject()
	if project == "" {
		return fmt.Errorf("--project is required (or set BOAT_PROJECT)")
	}

	ctx := context.Background()
	agents, err := daemon.ListAgentBeads(ctx)
	if err != nil {
		return fmt.Errorf("listing agents: %w", err)
	}

	closed := 0
	for _, a := range agents {
		if a.AgentState != "prewarmed" || a.Project != project {
			continue
		}
		status := "closed"
		if err := daemon.UpdateBead(ctx, a.ID, beadsapi.UpdateBeadRequest{
			Status: &status,
		}); err != nil {
			fmt.Fprintf(os.Stderr, "warning: failed to close %s: %v\n", a.ID, err)
			continue
		}
		closed++
	}

	fmt.Fprintf(os.Stdout, "Closed %d prewarmed agent(s) for project %q\n", closed, project)
	return nil
}

// ── pool assign ─────────────────────────────────────────────────────

var (
	poolAssignTaskID       string
	poolAssignRole         string
	poolAssignRuntimeClass string
	poolAssignModel        string
)

var poolAssignCmd = &cobra.Command{
	Use:   "assign",
	Short: "Assign a task to a prewarmed agent from the pool",
	Long: `Assigns work to an idle prewarmed agent. The pool manager picks the oldest
available agent (FIFO) and transitions it from prewarmed to assigning.

The agent's entrypoint detects the state change, exits standby, and starts
Claude with the assigned work context.

--role scopes the pick to a single role's pool. --require-runtime-class
constrains it to an agent whose effective pod runtime (project < role layering)
matches the given class (e.g. "runsc"), so a first-spawn runsc agent claims a
warm runsc pod instead of cold-provisioning a gVisor node. Both are optional;
empty matches any role / any runtime (legacy behavior).

--model bakes a specific model into the assigned agent: it stamps fields.model
so applyAgentModelOverride sets ANTHROPIC_MODEL/CLAUDE_MODEL at highest
precedence. Pass the canonical LiteLLM alias-KEY form (e.g. "claude-opus-5").
Empty keeps the account/role default the pod baked at prewarm time.`,
	RunE: runPoolAssign,
}

func runPoolAssign(cmd *cobra.Command, args []string) error {
	project := resolvePoolProject()
	if project == "" {
		return fmt.Errorf("--project is required (or set BOAT_PROJECT)")
	}

	ctx := context.Background()

	agents, err := daemon.ListAgentBeads(ctx)
	if err != nil {
		return fmt.Errorf("listing agents: %w", err)
	}

	// Resolve the effective runtime class per candidate only when the caller
	// constrains the assign to a runtime (--require-runtime-class). A
	// runsc-pinned channel must claim a warm pod that will itself run on runsc,
	// so a brand-new first-spawn runsc agent grafts onto a warm runsc pod
	// instead of cold-provisioning a gVisor node (WS3 proposal A, ka-0000001811;
	// CLI parity with poolmanager.AssignPrewarmed's RequireRuntimeClass gate,
	// ka-0000004563).
	var runtimeClassFor func(role string) string
	if poolAssignRuntimeClass != "" {
		runtimeClassFor, err = buildRuntimeClassResolver(ctx, project)
		if err != nil {
			return fmt.Errorf("resolving runtime classes: %w", err)
		}
	}

	// Find a prewarmed agent for the project. When a role is requested, only
	// agents of that role are eligible; when a runtime class is required, the
	// candidate's eventual pod runtime (project < role layering) must match.
	var pick *beadsapi.AgentBead
	for i, a := range agents {
		if a.AgentState != "prewarmed" || a.Project != project {
			continue
		}
		if poolAssignRole != "" && a.Role != poolAssignRole {
			continue
		}
		if poolAssignRuntimeClass != "" &&
			runtimeClassFor(a.Role) != poolAssignRuntimeClass {
			continue
		}
		pick = &agents[i]
		break
	}
	if pick == nil {
		switch {
		case poolAssignRuntimeClass != "":
			return fmt.Errorf("no prewarmed agents available for project %q with runtime class %q", project, poolAssignRuntimeClass)
		case poolAssignRole != "":
			return fmt.Errorf("no prewarmed agents available for project %q with role %q", project, poolAssignRole)
		default:
			return fmt.Errorf("no prewarmed agents available for project %q", project)
		}
	}

	// Transition agent_state to assigning and set task_id if provided.
	fields := map[string]string{
		"agent_state":  "assigning",
		"spawn_source": "pool-assign-cli",
	}
	if poolAssignTaskID != "" {
		fields["task_id"] = poolAssignTaskID
	}
	if poolAssignModel != "" {
		// Pool-assign model awareness (design D1, ka-0000000406 / ka-0000006250):
		// stamp the requested model so podspec.go's applyAgentModelOverride bakes
		// ANTHROPIC_MODEL/CLAUDE_MODEL from the pod's own bead. This is the CLI
		// counterpart to poolmanager.AssignRequest.Model. The value is passed
		// verbatim (canonical LiteLLM alias-KEY form, e.g. claude-opus-5) — it
		// is an env-var value, not a k8s label, so it must NOT be sanitized.
		fields["model"] = poolAssignModel
	}
	if err := daemon.UpdateBeadFields(ctx, pick.ID, fields); err != nil {
		return fmt.Errorf("updating agent bead: %w", err)
	}

	fmt.Fprintf(os.Stdout, "Assigned agent %s (%s) for project %q\n", pick.AgentName, pick.ID, project)
	if poolAssignTaskID != "" {
		fmt.Fprintf(os.Stdout, "  Task: %s\n", poolAssignTaskID)
	}
	if poolAssignModel != "" {
		fmt.Fprintf(os.Stdout, "  Model: %s\n", poolAssignModel)
	}
	return nil
}

// ── helpers ─────────────────────────────────────────────────────────

func resolvePoolProject() string {
	if poolProject != "" {
		return poolProject
	}
	return defaultGBProject()
}

// buildRuntimeClassResolver returns a function that resolves the effective
// Kubernetes RuntimeClass a prewarmed agent's pod will be created with, given
// its role. It mirrors poolmanager.effectiveRuntimeClass (and podspec.go's
// layering): the project override is the base, a non-empty role override
// supersedes it. Empty means the cluster default (runc). Project and role
// config beads are fetched once so the per-candidate lookup is a map read.
func buildRuntimeClassResolver(ctx context.Context, project string) (func(role string) string, error) {
	projectRC := ""
	if projects, err := daemon.ListProjectBeads(ctx); err != nil {
		return nil, fmt.Errorf("listing project beads: %w", err)
	} else if p, ok := projects[project]; ok {
		projectRC = p.RuntimeClassName
	}

	roles, err := daemon.ListRoleBeads(ctx)
	if err != nil {
		return nil, fmt.Errorf("listing role beads: %w", err)
	}

	return func(role string) string {
		if r, ok := roles[role]; ok && r.RuntimeClassName != "" {
			return r.RuntimeClassName
		}
		return projectRC
	}, nil
}

// getProjectPoolConfig finds the project bead and returns its current pool config.
//
// The raw prewarmed_pool JSON field is parsed directly off the project bead
// rather than via ListProjectBeads, because the latter discards the config
// entirely when enabled=false (see beadsapi.ListProjectBeads). For a faithful
// read-modify-write — preserving role/min_size/max_size/mode while flipping
// only the enabled flag — enable/disable must see the stored config even when
// the pool is currently disabled.
func getProjectPoolConfig(ctx context.Context, project string) (*beadsapi.PrewarmedPoolConfig, string, error) {
	// Find the project bead ID. We need to query by project name.
	result, err := daemon.ListBeadsFiltered(ctx, beadsapi.ListBeadsQuery{
		Types:    []string{"project"},
		Statuses: []string{"open", "in_progress", "blocked", "deferred"},
		Limit:    50,
	})
	if err != nil {
		return nil, "", fmt.Errorf("listing project beads: %w", err)
	}

	for _, b := range result.Beads {
		name := strings.TrimPrefix(b.Title, "Project: ")
		if name != project {
			continue
		}
		// Parse the raw prewarmed_pool field directly so we preserve the
		// stored config regardless of the enabled flag. nil when unset or
		// malformed (callers treat nil as "no existing config").
		raw := b.Fields["prewarmed_pool"]
		if raw == "" {
			return nil, b.ID, nil
		}
		var cfg beadsapi.PrewarmedPoolConfig
		if err := json.Unmarshal([]byte(raw), &cfg); err != nil {
			return nil, b.ID, fmt.Errorf("parsing prewarmed_pool for project %q: %w", project, err)
		}
		return &cfg, b.ID, nil
	}

	return nil, "", fmt.Errorf("project %q not found", project)
}

// updatePoolConfig writes the prewarmed_pool JSON field on the project bead.
func updatePoolConfig(ctx context.Context, beadID, project string, cfg *beadsapi.PrewarmedPoolConfig) error {
	poolJSON, err := json.Marshal(cfg)
	if err != nil {
		return fmt.Errorf("marshalling pool config: %w", err)
	}

	if err := daemon.UpdateBeadFields(ctx, beadID, map[string]string{
		"prewarmed_pool": string(poolJSON),
	}); err != nil {
		return fmt.Errorf("updating project bead: %w", err)
	}

	state := "disabled"
	if cfg.Enabled {
		state = "enabled"
	}
	fmt.Fprintf(os.Stdout, "Pool %s for project %q (min=%d, max=%d, role=%s, mode=%s)\n",
		state, project, cfg.MinSize, cfg.MaxSize, cfg.Role, cfg.Mode)
	return nil
}

func init() {
	poolCmd.PersistentFlags().StringVar(&poolProject, "project", "", "project name (default: from BOAT_PROJECT)")

	poolAssignCmd.Flags().StringVar(&poolAssignTaskID, "task", "", "task bead ID to pre-assign (optional)")
	poolAssignCmd.Flags().StringVar(&poolAssignRole, "role", "", "only assign a prewarmed agent of this role (optional)")
	poolAssignCmd.Flags().StringVar(&poolAssignRuntimeClass, "require-runtime-class", "", "only assign a prewarmed agent whose effective pod runtime class matches (e.g. \"runsc\"); empty matches any (optional)")
	poolAssignCmd.Flags().StringVar(&poolAssignModel, "model", "", "canonical LiteLLM model id (alias-KEY form, e.g. \"claude-opus-5\") to bake into the assigned agent; empty keeps the account/role default (optional)")

	poolCmd.AddCommand(poolStatusCmd)
	poolCmd.AddCommand(poolDisableCmd)
	poolCmd.AddCommand(poolEnableCmd)
	poolCmd.AddCommand(poolFlushCmd)
	poolCmd.AddCommand(poolAssignCmd)
}
