package main

import (
	"sort"
	"strings"
)

// Gemini hook translation (ADR 0005 §5 E, task gd-xdnGt0w9tW).
//
// The Claude hook set (defaultHookSettings, or the resolved claude-hooks config
// bead) is translated into the `hooks` table of ~/.gemini/settings.json. Event
// names are renamed (Claude's PreToolUse is Gemini's BeforeTool), the command
// allowlist is the one Codex uses (setup_codex_hooks.go: the hook set comes
// from a bead any agent token can write, so only fixed gardenio commands with
// allowlisted flags are installed), and the stop gate gets an explicit
// `--format gemini`.
//
// Three Gemini facts shape the output (phase 0 spike, docs/research/gemini-harness):
//
//   - Timeouts are MILLISECONDS (default 60000), not seconds as in Claude and
//     Codex, so the sanitized seconds value is multiplied by 1000.
//   - Plain stdout from a hook does NOT reach the model; only JSON
//     `hookSpecificOutput.additionalContext` does (verified on 0.64.0: a
//     BeforeAgent hook printing text was absent from the request, the JSON form
//     was present). The context-injection hooks (`gb hook prime`, `load-state`,
//     `check-mail`, the drain-queue/check-* scripts) print plain text, so they
//     would run for nothing and are skipped. Advice reaches Gemini through
//     GEMINI.md instead, and mail/nudges through the coop driver.
//   - ACP fires only BeforeAgent, AfterAgent, PreCompress and SessionEnd
//     (spike R2), so under ACP the relay's tool events come from the driver.
//     The stop gate is an AfterAgent hook and does fire.

// geminiHookEvents maps the Claude hook events that have a Gemini counterpart.
// Everything else defaultHookSettings registers (PostToolUseFailure,
// StopFailure, SubagentStart/Stop, TaskCompleted, TeammateIdle, CwdChanged,
// FileChanged, ConfigChange, InstructionsLoaded, Elicitation*, Worktree*,
// Setup, PermissionRequest, ...) has no Gemini event and is dropped.
var claudeToGeminiHookEvents = map[string]string{
	"SessionStart":     "SessionStart",
	"SessionEnd":       "SessionEnd",
	"UserPromptSubmit": "BeforeAgent",
	"Stop":             "AfterAgent",
	"PreToolUse":       "BeforeTool",
	"PostToolUse":      "AfterTool",
	"Notification":     "Notification",
	"PreCompact":       "PreCompress",
}

// geminiToolEvents are the Gemini events whose matcher is a tool-name regex.
// Claude's matchers name Claude tools (Bash, Edit), which do not exist in
// Gemini (run_shell_command, replace), so a matcher on these cannot be
// translated and the group is dropped rather than silently never matching.
var geminiToolEvents = map[string]bool{"BeforeTool": true, "AfterTool": true}

// geminiStdoutContextCommands are hook commands whose job is to print text for
// the model. Gemini discards plain stdout (see the file comment), so they are
// not installed.
var geminiStdoutContextCommands = []string{
	"gb hook prime",
	"gb hook load-state",
	"gb hook check-mail",
	"/hooks/prime.sh",
	"/hooks/check-mail.sh",
	"/hooks/check-jira.sh",
	"/hooks/drain-queue.sh",
}

// skipGeminiHandler reports hooks that must not be installed on Gemini because
// their output is model context that Gemini would throw away.
func skipGeminiHandler(command string) bool {
	command = strings.TrimSpace(command)
	for _, c := range geminiStdoutContextCommands {
		if command == c || strings.HasPrefix(command, c+" ") {
			return true
		}
	}
	return false
}

// geminiHookCommand adapts a Claude hook command for Gemini. The stop-gate
// gets an explicit --format gemini so its JSON-on-stdout contract does not
// depend on GARDENIO_HARNESS reaching the hook's environment.
func geminiHookCommand(command string) string {
	const gate = "gb hook stop-gate"
	if strings.HasPrefix(strings.TrimSpace(command), gate) && !strings.Contains(command, "--format") {
		return strings.Replace(command, gate, gate+" --format gemini", 1)
	}
	return command
}

// buildGeminiHooks translates a Claude-shaped {"hooks": {...}} document into
// the Gemini `hooks` document. dropped lists the Claude events with no Gemini
// counterpart, skipped the context-injection commands that were left out (both
// sorted, de-duplicated), rejected the commands the allowlist refused, and
// badTimeouts the handlers whose timeout was dropped or clamped.
func buildGeminiHooks(src map[string]any) (doc map[string]any, dropped, skipped, rejected, badTimeouts []string) {
	droppedSet, skippedSet, rejectedSet, badTimeoutSet := map[string]bool{}, map[string]bool{}, map[string]bool{}, map[string]bool{}
	srcHooks, _ := src["hooks"].(map[string]any)
	out := map[string]any{}
	for event, v := range srcHooks {
		geminiEvent, ok := claudeToGeminiHookEvents[event]
		if !ok {
			droppedSet[event] = true
			continue
		}
		groups, _ := v.([]any)
		var outGroups []any
		for _, g := range groups {
			group, ok := g.(map[string]any)
			if !ok {
				continue
			}
			matcher, _ := group["matcher"].(string)
			if matcher != "" && geminiToolEvents[geminiEvent] {
				rejectedSet[event+": matcher "+matcher+" names Claude tools, no Gemini equivalent"] = true
				continue
			}
			handlers, _ := group["hooks"].([]any)
			var outHandlers []any
			for _, h := range handlers {
				handler, ok := h.(map[string]any)
				if !ok {
					continue
				}
				typ, _ := handler["type"].(string)
				command, _ := handler["command"].(string)
				if typ != "command" || command == "" {
					continue
				}
				if !isAllowedHookCommand(command) {
					rejectedSet[event+": "+command] = true
					continue
				}
				if skipGeminiHandler(strings.TrimSuffix(command, hookQuietSuffix)) {
					skippedSet[strings.TrimSuffix(command, hookQuietSuffix)] = true
					continue
				}
				oh := map[string]any{"type": "command", "command": geminiHookCommand(command)}
				if t, ok := handler["timeout"]; ok {
					secs, note := sanitizeCodexTimeout(t)
					if note != "" {
						badTimeoutSet[event+": "+command+": "+note] = true
					}
					if secs > 0 {
						oh["timeout"] = secs * 1000 // Gemini hook timeouts are milliseconds
					}
				}
				outHandlers = append(outHandlers, oh)
			}
			if len(outHandlers) == 0 {
				continue
			}
			og := map[string]any{"hooks": outHandlers}
			// A matcher on the non-tool events (SessionStart source, Notification
			// type) carries over; Claude's empty matcher means "all", which Gemini
			// spells by omitting it.
			if matcher != "" && geminiEvent != "BeforeAgent" && geminiEvent != "AfterAgent" {
				og["matcher"] = matcher
			}
			outGroups = append(outGroups, og)
		}
		if len(outGroups) > 0 {
			out[geminiEvent] = outGroups
		}
	}
	return map[string]any{"hooks": out}, sortedSetKeys(droppedSet), sortedSetKeys(skippedSet), sortedSetKeys(rejectedSet), sortedSetKeys(badTimeoutSet)
}

func sortedSetKeys(set map[string]bool) []string {
	var out []string
	for k := range set {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
