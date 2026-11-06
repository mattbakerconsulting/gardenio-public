package main

// coop subprocess env construction and the env deny/allow lists it applies,
// split out of init_agent_loop.go to keep that file under the quench cloc limit.

import (
	"context"
	"log"
	"os"
	"strings"

	"gardenio/controller/internal/harness"
)

// claudeCodeEnvAllowlist names the CLAUDE_CODE_* env vars that buildCoopEnv
// allows to flow through from the parent (pod) env to the coop subprocess.
// Every other CLAUDE_CODE_* key is stripped: those are interpreted directly
// by Claude Code's agent runtime, so an accidental pod-spec or operator-set
// value would silently mutate behavior with no log trace. CLAUDE_CODE_DISABLE_
// AUTO_MEMORY is intentionally absent — buildCoopEnv re-adds it itself when
// the role is not memory-eligible.
//
// See gardenio/docs/audits/buildcoopenv-pod-env-audit.md and ka-0000007438.
var claudeCodeEnvAllowlist = map[string]struct{}{
	"CLAUDE_CODE_OAUTH_TOKEN":  {}, // provider auth
	"CLAUDE_CODE_ENABLE_TASKS": {}, // set explicitly by gb init
	"CLAUDE_CODE_USE_BEDROCK":  {}, // provider routing
	"CLAUDE_CODE_USE_VERTEX":   {}, // provider routing
	"CLAUDE_CODE_USE_LITELLM":  {}, // provider routing
}

// buildCoopEnv constructs the env passed to the coop subprocess. It starts
// from os.Environ() and layers on:
//
//   - COOP_LOG_LEVEL (always)
//   - CLAUDE_CODE_DISABLE_AUTO_MEMORY=1 for non-memory-eligible agents
//     (one-shot crew, thread agents, etc.). See ka-0000006599 / ka-0000008250.
//     Any inherited CLAUDE_CODE_DISABLE_AUTO_MEMORY value from the parent env
//     is always stripped so the controlled value is authoritative.
//   - COOP_S3_SESSION_KEY when both an agent bead ID and an S3 bucket are set.
//
// CLAUDE_CODE_* keys outside claudeCodeEnvAllowlist are denied — this prevents
// any future Claude Code flag from leaking through pod env. See ka-0000007438.
func buildCoopEnv(cfg initConfig) []string {
	parentEnv := os.Environ()
	agentBeadID := envOr("KD_AGENT_ID", os.Getenv("BOAT_AGENT_BEAD_ID"))
	eligible := isMemoryEligible(context.Background(), agentBeadID, cfg.role, nil)

	env := make([]string, 0, len(parentEnv)+2)
	for _, kv := range parentEnv {
		if isDeniedClaudeCodeEnv(kv) || isDeniedControllerSecretEnv(kv) {
			logDeniedClaudeCodeEnv(kv)
			continue
		}
		if harness.IsDeniedEnv(cfg.harness, kv) {
			logDeniedHarnessEnv(kv)
			continue
		}
		env = append(env, kv)
	}
	env = append(env, "COOP_LOG_LEVEL="+envOr("COOP_LOG_LEVEL", "info"))
	if !eligible {
		env = append(env, "CLAUDE_CODE_DISABLE_AUTO_MEMORY=1")
	}
	// Keep Claude Code's messaging sockets off the FSGroup'd /tmp EmptyDir,
	// which is 2777 and fails its ancestor vet (ka-0000002604). Best-effort:
	// an empty value means the agent launches unchanged, without the override.
	if kv := messagingTmpDirEnv(logInit); kv != "" {
		env = append(env, kv)
	}
	if key := agentS3SessionKey(); key != "" && os.Getenv("COOP_S3_BUCKET") != "" {
		env = append(env, "COOP_S3_SESSION_KEY="+key)
	}
	// Force-clean context-blowup policy (ka-0000007242): when resolveForceCleanPolicy
	// latched a force-clean at boot, tell coop to skip the S3 transcript restore and
	// ignore any local --resume hint (config.rs force_clean_active), so it starts a
	// clean session. COOP_S3_SESSION_KEY is deliberately still set above, so this
	// fresh life's transcript uploads under the same stable prefix — only the boot
	// rehydration of the oversized prior transcript is suppressed.
	if forceCleanActive() {
		env = append(env, forceCleanEnvVar+"="+forceCleanEnvValue)
	}
	return env
}

// isDeniedClaudeCodeEnv reports whether kv ("KEY=val") is a CLAUDE_CODE_* key
// not in the allowlist. Denied keys are stripped from the coop env.
func isDeniedClaudeCodeEnv(kv string) bool {
	if !strings.HasPrefix(kv, "CLAUDE_CODE_") {
		return false
	}
	key, _, _ := strings.Cut(kv, "=")
	_, ok := claudeCodeEnvAllowlist[key]
	return !ok
}

// controllerSecretEnvDenylist names env vars that are legitimately
// controller-side ONLY and must never ride into the coop/claude agent child.
// The buildCoopEnv audit (ka-0000007438) chose a CLAUDE_CODE_* denylist over a
// full allowlist because per-project secrets/env_vars are unbounded — but the
// LiteLLM master/admin key is a fixed, high-value controller credential that a
// pod-spec or operator misconfiguration could theoretically place in the pod
// env. Claudemore closes F2 (R4-SEC-2) by asserting that even if such a key is
// present it is stripped here, keeping the master key controller-side only
// (R4-SEC "master key controller-only"). These are exact keys, never the
// per-agent virtual key (x-litellm-api-key), which is passed as a header and is
// intentionally scoped/revocable.
var controllerSecretEnvDenylist = map[string]struct{}{
	"LITELLM_MASTER_KEY":   {}, // controller spend-sync auth to LiteLLM
	"LITELLM_ADMIN_SECRET": {}, // controller admin ops against LiteLLM
}

// isDeniedControllerSecretEnv reports whether kv ("KEY=val") is a
// controller-only secret that must be stripped from the coop env
// (defense-in-depth for F2 / master-key-controller-only).
func isDeniedControllerSecretEnv(kv string) bool {
	key, _, _ := strings.Cut(kv, "=")
	_, denied := controllerSecretEnvDenylist[key]
	return denied
}

// logDeniedHarnessEnv is logDeniedClaudeCodeEnv for the codex CODEX_*/OPENAI_*
// allowlist (internal/harness): key name and value length only, never the value.
func logDeniedHarnessEnv(kv string) {
	key, val, _ := strings.Cut(kv, "=")
	log.Printf("[gb agent start] coop-env: stripped denied CODEX_*/OPENAI_* key=%s value_len=%d", key, len(val))
}

// logDeniedClaudeCodeEnv emits one observability line per CLAUDE_CODE_* key
// stripped by buildCoopEnv / runCoopOnce. The line records the key name and
// value length only — never the value itself. See ka-0000007104 and
// gardenio/docs/audits/buildcoopenv-pod-env-audit.md.
func logDeniedClaudeCodeEnv(kv string) {
	key, val, _ := strings.Cut(kv, "=")
	log.Printf("[gb agent start] coop-env: stripped denied CLAUDE_CODE_* key=%s value_len=%d", key, len(val))
}
