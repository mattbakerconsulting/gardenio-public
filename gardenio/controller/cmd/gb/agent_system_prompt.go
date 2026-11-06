package main

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"

	"gardenio/controller/internal/harness"
)

// systemPromptFileName is the per-agent file written before each coop launch.
// Claude Code reads it via --append-system-prompt-file and appends its contents
// to the system prompt — full content, no truncation, survives compaction.
const systemPromptFileName = "system-prompt.md"

// envSystemPromptFile records the system-prompt file THIS session was actually
// launched with. Set by writeSystemPromptFile on success and inherited by the
// hook processes (which run under the coop env), so outputPrimeForHook can tell
// a healthy boot from a degraded one and decide whether the sections carried by
// the system prompt still need a hook-channel fallback (ka-0000007437).
//
// A per-session env var rather than a file-existence check: on a warm-pool pod
// a stale system-prompt.md from pool-fill time is already on disk before the
// agent is assigned, so the file alone cannot prove the CURRENT launch got one.
const envSystemPromptFile = "BOAT_SYSTEM_PROMPT_FILE"

// writeSystemPromptFile generates the rules-only prime body (`gb prime
// --system-prompt`) and writes it to <workdir>/.gb/system-prompt.md.
//
// Returns the file path and true on success. Returns an empty string and false
// on any failure (mkdir, gb prime exec, empty output, write) — callers must
// treat this as best-effort and skip the --append-system-prompt-file flag in
// that case so the agent still launches.
//
// Regenerated on every coop session start so the system prompt reflects the
// agent's current advice/identity. Cheap (single gb subprocess + small file
// write); the cost is amortized into agent startup which already takes seconds.
func writeSystemPromptFile(workdir string, logf func(string, ...any)) (string, bool) {
	// Clear any value left by a PRIOR session in this same pod: the supervisor
	// re-runs this per coop session, and a stale "success" marker from an
	// earlier session would make the hook suppress its fallback on a session
	// whose write actually failed. Re-set at the end on success.
	_ = os.Unsetenv(envSystemPromptFile)

	if workdir == "" {
		return "", false
	}
	if logf == nil {
		logf = func(string, ...any) {}
	}

	dir := filepath.Join(workdir, ".gb")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		logf("warning: mkdir %s: %v", dir, err)
		return "", false
	}
	path := filepath.Join(dir, systemPromptFileName)

	// Resolve `gb` from PATH; fall back to the running binary so this works
	// in tests and non-PATH-installed environments.
	prime, err := runPrimeSystemPrompt()
	if err != nil {
		logf("warning: %v; skipping --append-system-prompt-file", err)
		return "", false
	}
	buf := bytes.NewBuffer(prime)
	if err := os.WriteFile(path, buf.Bytes(), 0o644); err != nil {
		logf("warning: write %s: %v", path, err)
		return "", false
	}
	// Record the successful write for this session so the SessionStart hook can
	// skip re-emitting what the system prompt now carries (ka-0000007437). Set
	// on the process env so both coop-env builders (buildCoopEnv copies
	// os.Environ(); agent_start_k8s.go filters it) propagate it to the hooks.
	if err := os.Setenv(envSystemPromptFile, path); err != nil {
		// Non-fatal: the hook just falls back to re-emitting those sections,
		// which is redundant but safe.
		logf("warning: setenv %s: %v", envSystemPromptFile, err)
	}
	logf("wrote %s (%d bytes) for --append-system-prompt-file", path, buf.Len())
	return path, true
}

// prepareAgentPrompt is the per-launch prompt step for both coop launch paths
// (gb init and gb agent start). h is the caller's already-validated harness
// (internal/harness). Claude gets the system-prompt file to pass as
// --append-system-prompt-file. Codex has no such flag; the same `gb prime
// --system-prompt` content reaches it as $CODEX_HOME/AGENTS.md (ADR 0004 §E),
// and Gemini as ~/.gemini/GEMINI.md (ADR 0005 §5 E),
// installed here directly rather than as a side effect of writeSystemPromptFile
// so it survives any launch path that skips the Claude file. Returns the file
// path and true only when a --append-system-prompt-file flag should be added.
func prepareAgentPrompt(h, workdir string, logf func(string, ...any)) (string, bool) {
	if harness.IsCodex(h) || harness.IsGemini(h) {
		if logf == nil {
			logf = func(string, ...any) {}
		}
		if harness.IsGemini(h) {
			// GEMINI.md at user level lands in the CLI's systemInstruction (ADR 0005 R3).
			installGeminiMDFromPrime(logf)
		} else {
			installCodexAgentsMDFromPrime(logf)
		}
		return "", false
	}
	return writeSystemPromptFile(workdir, logf)
}

// currentBinary returns the absolute path of the running executable, or "gb"
// if it can't be resolved. Used as a fallback when `gb` isn't in PATH.
func currentBinary() string {
	if p, err := os.Executable(); err == nil {
		return p
	}
	return "gb"
}

// systemPromptFlag returns the --append-system-prompt-file flag for the given
// path, or an empty string if path is empty.
func systemPromptFlag(path string) string {
	if path == "" || isCodexHarness() || isGeminiHarness() {
		// Codex and Gemini reject the flag; the prompt rides in AGENTS.md /
		// GEMINI.md instead.
		return ""
	}
	return fmt.Sprintf("--append-system-prompt-file=%s", path)
}

// takesSystemPromptFile reports whether the agent command accepts
// --append-system-prompt-file. claudeless 0.4.0 does not: it exits 2 on the
// unknown flag before its first screen, so a mock pod never runs a turn
// (gd-K77LsUtqhK).
func takesSystemPromptFile(cmd string) bool {
	return !harness.IsMockCommand(cmd)
}

// injectSystemPromptArgs splices --append-system-prompt-file into a parsed
// claude argv. The flag is inserted immediately after argv[0] (the binary)
// so it precedes other flags, matching how Claude Code documents its CLI.
//
// If parts is empty, path is empty or the binary is claudeless, returns parts
// unchanged.
func injectSystemPromptArgs(parts []string, path string) []string {
	flag := systemPromptFlag(path)
	if flag == "" || len(parts) == 0 || !takesSystemPromptFile(parts[0]) {
		return parts
	}
	out := make([]string, 0, len(parts)+1)
	out = append(out, parts[0], flag)
	out = append(out, parts[1:]...)
	return out
}

// injectSystemPromptShellCommand splices --append-system-prompt-file into a
// shell-style command string ("claude --dangerously-skip-permissions"). The
// flag is inserted after the first whitespace-delimited token.
//
// Used by codepaths that pass the agent command via `sh -c <string>` rather
// than as a parsed argv. Returns the command unchanged if path is empty or the
// binary is claudeless.
func injectSystemPromptShellCommand(command, path string) string {
	flag := systemPromptFlag(path)
	if flag == "" || command == "" || !takesSystemPromptFile(command) {
		return command
	}
	// Find the first whitespace; insert flag right after the binary token.
	for i, r := range command {
		if r == ' ' || r == '\t' {
			return command[:i] + " " + flag + command[i:]
		}
	}
	// Single-token command (just "claude"): append the flag.
	return command + " " + flag
}
