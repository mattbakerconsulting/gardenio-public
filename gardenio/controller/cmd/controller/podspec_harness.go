package main

import (
	"context"
	"fmt"
	"log/slog"
	"strings"

	"gardenio/controller/internal/beadsapi"
	"gardenio/controller/internal/config"
	"gardenio/controller/internal/podmanager"
)

// EnvHarness is the pod env var naming the agent CLI the pod runs. Set only for
// non-default harnesses, so a claude pod's env is byte-for-byte what it was
// before the harness field existed.
const EnvHarness = "GARDENIO_HARNESS"

// resolveHarness returns the harness for a spec: agent > role > project >
// cfg.DefaultHarness > claude (the agent layer is the caller's agentHarness
// argument; this resolves the layers below it). Mirrors resolveModelOverride:
// for multi-role agents ("thread,crew") the first role with a harness wins, and
// the caller (applyCommonConfig) already holds ProjectCacheMu.RLock, so
// cfg.ProjectCache is read directly while RoleCacheMu is taken internally.
//
// Every layer is validated with beadsapi.ParseHarness, so a hand-edited bead
// that bypassed the kbeads enum fails this spawn with a readable error instead
// of silently launching the wrong CLI.
func resolveHarness(cfg *config.Config, project, role, agentHarness string) (string, error) {
	if h, err := beadsapi.ParseHarness(agentHarness); err != nil {
		return "", fmt.Errorf("agent bead: %w", err)
	} else if h != "" {
		return h, nil
	}
	if role != "" && len(cfg.RoleCache) > 0 {
		cfg.RoleCacheMu.RLock()
		for _, r := range parseRoleList(role) {
			entry, ok := cfg.RoleCache[r]
			if !ok || strings.TrimSpace(entry.Harness) == "" {
				continue
			}
			cfg.RoleCacheMu.RUnlock()
			h, err := beadsapi.ParseHarness(entry.Harness)
			if err != nil {
				return "", fmt.Errorf("role %q: %w", r, err)
			}
			return h, nil
		}
		cfg.RoleCacheMu.RUnlock()
	}
	if project != "" {
		if entry, ok := cfg.ProjectCache[project]; ok && strings.TrimSpace(entry.Harness) != "" {
			h, err := beadsapi.ParseHarness(entry.Harness)
			if err != nil {
				return "", fmt.Errorf("project %q: %w", project, err)
			}
			return h, nil
		}
	}
	h, err := beadsapi.ParseHarness(cfg.DefaultHarness)
	if err != nil {
		return "", fmt.Errorf("controller default: %w", err)
	}
	if h == "" {
		h = beadsapi.DefaultHarness
	}
	return h, nil
}

// applyHarness resolves the spec's harness and checks it against the account
// the spec was credentialed with. It records the outcome on the spec
// (Harness / HarnessError) and never panics or blocks: a bad harness is
// surfaced as spec.HarnessError, which the spawn path turns into a refusal.
//
// Invoked from applyCommonConfig right after applyModelOverride, under the
// caller's ProjectCacheMu.RLock (see resolveHarness). Env shaping for the codex
// harness is deferred to shapeHarnessEnv, which must see the final model.
func applyHarness(cfg *config.Config, spec *podmanager.AgentPodSpec) {
	h, err := resolveHarness(cfg, spec.Project, spec.Role, spec.RequestedHarness)
	if err != nil {
		spec.HarnessError = err.Error()
		return
	}
	spec.Harness = h

	// Gemini is accepted by the enum and validation from ADR 0005 phase 1, but
	// the pod side (config, hooks, stop gate, driver) is not built yet: refuse
	// until the operator turns the harness on, so no role can select it early.
	if h == beadsapi.HarnessGemini && !cfg.GeminiHarnessEnabled {
		spec.HarnessError = fmt.Sprintf(
			"harness %q is not enabled on this controller (ADR 0005 phases 2-3 are not live; set GEMINI_HARNESS_ENABLED=true once they are); "+
				"set harness=claude or codex on the agent, role or project bead",
			h)
		return
	}

	// Validate account.provider against the harness (harnessProviders). Account
	// selection already filters to the harness's providers (accountMatchesHarness),
	// so a mismatch here means a role account pin pointed at the wrong provider.
	// A claude spec with no account has nothing to mismatch (the credential guard
	// handles loginless pods separately). A codex or gemini spec with no account
	// fails closed: without a provider-matched account there is no credential
	// (no CodexAuthSecret / OAuth SecretEnv, no GEMINI_API_KEY push), so the
	// credential guard would let a loginless pod launch.
	// Transient spawn blocks are left alone: the spawn is already being declined
	// and retried.
	need := harnessNeedsProvider(h)
	if spec.ResolvedAccount == "" {
		if need != "" && !spec.SpawnBlocked {
			spec.HarnessError = fmt.Sprintf(
				"harness %q needs an account with provider %q, but no account resolved; "+
					"label a %s account project:%s (or pin one with account=<%s-account> on the role bead) or set harness=claude",
				h, need, need, spec.Project, need)
		}
		return
	}
	acct, ok := cfg.AccountCache[spec.ResolvedAccount]
	if !ok {
		return
	}
	if harnessAcceptsAccount(acct, h) {
		return
	}
	if need != "" {
		spec.HarnessError = fmt.Sprintf(
			"harness %q needs an account with provider %q, but account %q has provider %q; "+
				"pin the role to a %s account or set harness=claude",
			h, need, spec.ResolvedAccount, acct.Provider, need)
		return
	}
	owner := providerHarness(acct.Provider)
	spec.HarnessError = fmt.Sprintf(
		"harness %q cannot use account %q (provider %q); set harness=%s on the role/project/agent or pick an anthropic account",
		h, spec.ResolvedAccount, acct.Provider, owner)
}

// refuseHarnessOnTarget fails a codex or gemini spec whose execution target is not the
// Kubernetes pod path. The lambda_microvm image, entrypoint, credential push
// and state persistence are Claude-only (the image ships no codex binary and
// the entrypoint writes only ~/.claude/.credentials.json: no CODEX_HOME, no
// -session auth Secret, no hooks seeding; likewise no gemini binary or ~/.gemini), so launching codex there would boot
// a VM that cannot authenticate. ADR 0004 section 11 and its addendum record the
// follow-up. It runs after resolveExecutionTarget, whose output it reads, and
// reports through spec.HarnessError so all three spawn doors (event,
// recreate, reconciler) refuse permanently with a readable reason.
func refuseHarnessOnTarget(spec *podmanager.AgentPodSpec) {
	if spec.HarnessError != "" || (spec.Harness != beadsapi.HarnessCodex && spec.Harness != beadsapi.HarnessGemini) {
		return
	}
	if spec.ExecutionTarget != podmanager.ExecutionTargetLambdaMicroVM {
		return
	}
	spec.HarnessError = fmt.Sprintf(
		"harness %q is not supported on execution target %q (the microvm image and credential path are Claude-only); "+
			"set harness=claude or execution_target=kubernetes on the agent, role or project bead",
		spec.Harness, spec.ExecutionTarget)
}

// harnessDisplayName is the capitalised CLI name used in operator-facing text.
func harnessDisplayName(h string) string {
	switch h {
	case beadsapi.HarnessCodex:
		return "Codex"
	case beadsapi.HarnessGemini:
		return "Gemini"
	}
	return "Claude"
}

// harnessModelEnv names the pod env var carrying the resolved model for a
// non-claude harness (internal/harness.ModelEnv; the two must stay equal).
var harnessModelEnv = map[string]string{
	beadsapi.HarnessCodex:  "CODEX_MODEL",
	beadsapi.HarnessGemini: "GEMINI_MODEL",
}

// geminiTrustWorkspaceEnv clears the Gemini CLI's untrusted-directory gate
// (exit 55 headless; ADR 0005 §11.5). It is on the harness env allowlist.
const geminiTrustWorkspaceEnv = "GEMINI_CLI_TRUST_WORKSPACE"

// shapeHarnessEnv rewrites the pod env for a non-claude harness once every
// model layer (account, project/role, agent, downgrade) has been applied. It is
// a no-op for claude, so existing roles are unchanged. For codex and gemini it:
//   - sets GARDENIO_HARNESS to the harness;
//   - sets the harness's model var (CODEX_MODEL / GEMINI_MODEL) from the
//     resolved model (ANTHROPIC_MODEL is only ever written by the account
//     default or a model override; the global cfg.ClaudeModel sets CLAUDE_MODEL
//     alone, so it never leaks into another harness);
//   - drops the Claude-only model pins (CLAUDE_MODEL, ANTHROPIC_MODEL, the
//     ANTHROPIC_DEFAULT_*_MODEL alias pins, and BOAT_MODEL_PINNED, which the gb
//     init loop would use to re-assert CLAUDE_MODEL after a credential reload);
//   - for gemini, also sets GEMINI_CLI_TRUST_WORKSPACE=true.
func shapeHarnessEnv(spec *podmanager.AgentPodSpec) {
	modelEnv, ok := harnessModelEnv[spec.Harness]
	if spec.HarnessError != "" || !ok {
		return
	}
	if spec.Env == nil {
		spec.Env = make(map[string]string)
	}
	spec.Env[EnvHarness] = spec.Harness
	if model := spec.Env["ANTHROPIC_MODEL"]; model != "" {
		spec.Env[modelEnv] = model
	}
	if spec.Harness == beadsapi.HarnessGemini {
		spec.Env[geminiTrustWorkspaceEnv] = "true"
	}
	for _, k := range []string{
		"CLAUDE_MODEL", "ANTHROPIC_MODEL", EnvModelPinned,
		"ANTHROPIC_DEFAULT_FABLE_MODEL", "ANTHROPIC_DEFAULT_OPUS_MODEL", "ANTHROPIC_DEFAULT_HAIKU_MODEL",
	} {
		delete(spec.Env, k)
	}
}

// harnessStamper is the slice of *beadsapi.Client stampResolvedHarness needs.
type harnessStamper interface {
	StampResolvedHarness(ctx context.Context, beadID, harness string) error
}

// stampResolvedHarness writes the spec's resolved harness onto the agent bead
// (ADR 0004 section 4) so the Slack card, metrics label, gb init and coop launch
// read it and none re-resolve. Called just before CreateAgentPod, after every
// refusal guard, so only a spawn that is actually going ahead is stamped.
//
// Best-effort: a failed write is logged and never blocks the spawn. The pod env
// (GARDENIO_HARNESS) still carries a non-default harness, and the next spawn of
// the same bead re-stamps. A nil stamper, an empty bead id or an unresolved
// harness (HarnessError specs are refused before this point) is a no-op.
func stampResolvedHarness(ctx context.Context, logger *slog.Logger, stamper harnessStamper, agentBeadID string, spec podmanager.AgentPodSpec) {
	if stamper == nil || agentBeadID == "" || spec.Harness == "" || spec.HarnessError != "" {
		return
	}
	if err := stamper.StampResolvedHarness(ctx, agentBeadID, spec.Harness); err != nil {
		logger.Warn("failed to stamp resolved harness on agent bead",
			"bead", agentBeadID, "harness", spec.Harness, "error", err)
	}
}

// spawnTaskClaimer is the slice of *beadsapi.Client claimSpawnTask needs.
type spawnTaskClaimer interface {
	ClaimSpawnTask(ctx context.Context, agentBeadID string) (string, error)
}

// claimSpawnTask makes the agent the assignee of the task its bead names, on
// the controller's machine principal, when the spawner's own pre-claim did not
// land (gd-Yo28zKgkGx; see beadsapi.ClaimSpawnTask). Called just before
// CreateAgentPod, beside stampResolvedHarness, so the claim is in place before
// the crew boots. Best-effort: a failure is logged and never blocks the spawn.
func claimSpawnTask(ctx context.Context, logger *slog.Logger, claimer spawnTaskClaimer, agentBeadID string, spec podmanager.AgentPodSpec) {
	if claimer == nil || spec.TaskID == "" {
		return
	}
	outcome, err := claimer.ClaimSpawnTask(ctx, agentBeadID)
	if err != nil {
		logger.Warn("failed to claim agent's task at spawn",
			"bead", agentBeadID, "agent", spec.AgentName, "task", spec.TaskID, "error", err)
		return
	}
	logger.Info("agent task claim at spawn",
		"bead", agentBeadID, "agent", spec.AgentName, "task", spec.TaskID, "outcome", outcome)
}
