// SPDX-License-Identifier: BUSL-1.1
// Copyright (c) 2026 Matt Baker <fimbaz@gmail.com>

use std::path::{Path, PathBuf};

use serde_json::{json, Value};

/// Generate coop's Codex `hooks.json` fragment.
///
/// Codex hook commands receive the event payload as JSON on stdin (see
/// `coop/tests/fixtures/codex/hook-payloads.jsonl`). Each hook wraps it in coop's
/// `{"event": ..., "data": ...}` envelope and writes it to `$COOP_HOOK_PIPE`:
/// - `SessionStart` → `start`
/// - `UserPromptSubmit` → `user_prompt_submit`
/// - `PreToolUse` → `pre_tool_use`; `PostToolUse` → `post_tool_use`
/// - `Stop` → `stop`, then asks `$COOP_URL/api/v1/hooks/stop` for a gating
///   verdict and echoes a `{"decision":"block",...}` reply on **stdout** with
///   exit 0, the form Codex honours (ADR 0004 §8; exit code 2 is untested).
///
/// This only *generates* the fragment. Installing it into
/// `$CODEX_HOME/hooks.json` and seeding `[hooks.state]` trust is the pod-init
/// step's job (ADR 0004 §7.1): Codex silently skips untrusted hooks, so
/// until that runs the Tier 1 detector simply receives nothing and Tier 3
/// carries detection.
pub fn generate_hook_config(pipe_path: &Path) -> Value {
    // The config is portable: the real path travels in $COOP_HOOK_PIPE.
    let _ = pipe_path;

    let stop_command = concat!(
        "input=$(cat); ",
        "event=$(printf '{\"event\":\"stop\",\"data\":%s}' \"$input\"); ",
        "printf '%s\\n' \"$event\" > \"$COOP_HOOK_PIPE\"; ",
        "tok=; ",
        "if [ -n \"$COOP_HOOK_TOKEN_FILE\" ] && [ -r \"$COOP_HOOK_TOKEN_FILE\" ]; then ",
        "tok=$(cat \"$COOP_HOOK_TOKEN_FILE\" 2>/dev/null); ",
        "fi; ",
        "set --; ",
        "if [ -n \"$tok\" ]; then set -- -H \"Authorization: Bearer $tok\"; fi; ",
        "response=$(printf '%s' \"$event\" | curl -sf -X POST ",
        "\"$@\" ",
        "-H 'Content-Type: application/json' ",
        "-d @- \"$COOP_URL/api/v1/hooks/stop\" 2>/dev/null); ",
        "if printf '%s' \"$response\" | grep -q '\"block\"'; then printf '%s' \"$response\"; fi"
    );

    json!({
        "hooks": {
            "SessionStart": [pipe_hook("start")],
            "UserPromptSubmit": [pipe_hook("user_prompt_submit")],
            "PreToolUse": [pipe_hook("pre_tool_use")],
            "PostToolUse": [pipe_hook("post_tool_use")],
            "Stop": [{
                "matcher": "*",
                "hooks": [{ "type": "command", "command": stop_command }]
            }]
        }
    })
}

/// A hook group that forwards its stdin to the pipe under `event`.
fn pipe_hook(event: &str) -> Value {
    let command = format!(
        "input=$(cat); printf '{{\"event\":\"{event}\",\"data\":%s}}\\n' \"$input\" > \"$COOP_HOOK_PIPE\""
    );
    json!({ "matcher": "*", "hooks": [{ "type": "command", "command": command }] })
}

/// Write the hook config to `<dir>/coop-codex-hooks.json` and return its path.
pub fn write_hook_config(dir: &Path, pipe_path: &Path) -> anyhow::Result<PathBuf> {
    let path = dir.join("coop-codex-hooks.json");
    std::fs::write(&path, serde_json::to_string_pretty(&generate_hook_config(pipe_path))?)?;
    Ok(path)
}

#[cfg(test)]
#[path = "hooks_tests.rs"]
mod tests;
