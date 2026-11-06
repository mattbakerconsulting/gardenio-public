// SPDX-License-Identifier: BUSL-1.1
// Copyright (c) 2026 Matt Baker <fimbaz@gmail.com>

use std::path::Path;

use serde_json::{json, Value};

/// Generate the Claude Code hook configuration JSON.
///
/// The hooks write JSON events to the named pipe at `$COOP_HOOK_PIPE`:
/// - `PostToolUse`: fires after each tool call, writes tool name
/// - `Stop`: curls `$COOP_URL/api/v1/hooks/stop` for a gating verdict
/// - `Notification`: fires on `idle_prompt` and `permission_prompt`
/// - `PreToolUse`: fires before every tool call. Prompt-shaped tools
///   (`AskUserQuestion`, `ExitPlanMode`, `EnterPlanMode`) map to prompt
///   states; every other tool pairs with its `PostToolUse` to feed the
///   `gardenio_agent_tool_call_latency_seconds` histogram (ka-0000004093)
pub fn generate_hook_config(pipe_path: &Path) -> Value {
    // Use $COOP_HOOK_PIPE so the config is portable across processes.
    // The actual path is passed via environment variable.
    let _ = pipe_path; // validated by caller; config uses env var

    // Start hook uses curl to call coop's start endpoint. The response is
    // a shell script that gets eval'd. If curl fails or returns empty, nothing happens.
    //
    // The Bearer token is read from $COOP_HOOK_TOKEN_FILE (per-session token
    // written by coop's setup with mode 0600) and passed via the
    // Authorization header. When the file is missing or empty the header is
    // omitted — the FIFO write still happens; only the HTTP gating call
    // degrades to unauthenticated, which coop will then reject.
    //
    // POSIX positional parameters (`set --`) are used to safely include the
    // header (with an embedded space) only when the token is present.
    let start_command = concat!(
        "input=$(cat); ",
        "event=$(printf '{\"event\":\"start\",\"data\":%s}' \"$input\"); ",
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
        "-d @- \"$COOP_URL/api/v1/hooks/start\" 2>/dev/null); ",
        "if [ -n \"$response\" ]; then eval \"$response\"; fi" // OK: eval executes trusted coop server response for dynamic hook injection
    );

    // Stop hook uses curl to call coop's gating endpoint. If curl fails
    // (coop not ready), the hook outputs nothing and exits 0 → agent proceeds.
    // The -f flag makes curl return non-zero on HTTP errors.
    // Builds the event envelope once and sends it to both the pipe and the endpoint.
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
        "if [ -n \"$response\" ]; then printf '%s' \"$response\"; fi"
    );

    json!({
        "hooks": {
            "SessionStart": [{
                "matcher": "",
                "hooks": [{
                    "type": "command",
                    "command": start_command
                }]
            }],
            "PostToolUse": [{
                "matcher": "",
                "hooks": [{
                    "type": "command",
                    "command": "input=$(cat); printf '{\"event\":\"post_tool_use\",\"data\":%s}\\n' \"$input\" > \"$COOP_HOOK_PIPE\""
                }]
            }],
            "Stop": [{
                "matcher": "",
                "hooks": [{
                    "type": "command",
                    "command": stop_command
                }]
            }],
            "Notification": [{
                "matcher": "idle_prompt|permission_prompt",
                "hooks": [{
                    "type": "command",
                    "command": "input=$(cat); printf '{\"event\":\"notification\",\"data\":%s}\\n' \"$input\" > \"$COOP_HOOK_PIPE\""
                }]
            }],
            // Matches ALL tools (not just the prompt-shaped ones): the
            // ToolBefore events this produces are what start the per-tool
            // timers behind gardenio_agent_tool_call_latency_seconds. With a
            // restricted matcher the fleet-wide histogram registered but
            // never observed (ka-0000004093).
            "PreToolUse": [{
                "matcher": "",
                "hooks": [{
                    "type": "command",
                    "command": "input=$(cat); printf '{\"event\":\"pre_tool_use\",\"data\":%s}\\n' \"$input\" > \"$COOP_HOOK_PIPE\""
                }]
            }],
            "UserPromptSubmit": [{
                "matcher": "",
                "hooks": [{
                    "type": "command",
                    "command": "input=$(cat); printf '{\"event\":\"user_prompt_submit\",\"data\":%s}\\n' \"$input\" > \"$COOP_HOOK_PIPE\""
                }]
            }]
        }
    })
}

/// Return environment variables to set on the Claude child process.
pub use crate::driver::hook_env_vars;

/// Write the hook config to a file and return its path.
///
/// The config file is written into `config_dir` so Claude can load it
/// via `--hook-config`.
pub fn write_hook_config(
    config_dir: &Path,
    pipe_path: &Path,
) -> anyhow::Result<std::path::PathBuf> {
    let config = generate_hook_config(pipe_path);
    let config_path = config_dir.join("coop-hooks.json");
    let contents = serde_json::to_string_pretty(&config)?;
    std::fs::write(&config_path, contents)?;
    Ok(config_path)
}

#[cfg(test)]
#[path = "hooks_tests.rs"]
mod tests;
