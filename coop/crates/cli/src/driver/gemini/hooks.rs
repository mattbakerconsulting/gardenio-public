// SPDX-License-Identifier: BUSL-1.1
// Copyright (c) 2026 Matt Baker <fimbaz@gmail.com>

use std::path::Path;

use serde_json::{json, Value};

/// Generate the Gemini CLI hook configuration JSON.
///
/// Gemini hooks receive JSON on stdin and must output JSON on stdout.
/// The hooks read stdin, wrap it, and write to the named pipe at `$COOP_HOOK_PIPE`:
/// - `BeforeAgent`: fires at the start of each turn (after user prompt)
/// - `BeforeTool`: fires before each tool call
/// - `AfterTool`: fires after each tool call, includes tool name and result
/// - `AfterAgent`: fires after each turn; curls gating endpoint
/// - `SessionEnd`: fires when the session ends
/// - `Notification`: fires on system notifications (e.g. `ToolPermission`)
pub fn generate_hook_config(pipe_path: &Path) -> Value {
    // Use $COOP_HOOK_PIPE so the config is portable across processes.
    // The actual path is passed via environment variable.
    let _ = pipe_path; // validated by caller; config uses env var

    // SessionStart hook: write start event to pipe, then curl start endpoint.
    // The response is a shell script that gets eval'd.
    //
    // The Bearer token is read from $COOP_HOOK_TOKEN_FILE (per-session, 0600)
    // and passed via Authorization header. POSIX positional parameters carry
    // the header safely so that an embedded space does not split mid-arg.
    let session_start_command = concat!(
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

    // AfterAgent hook: write stop event to pipe, then curl gating endpoint.
    // Gemini continues a turn on {"decision":"block","reason":...} (or exit 2), which
    // is exactly coop's verdict, so a block verdict is echoed as is. The earlier
    // {"continue":true} does NOT hold a turn open on 0.64.0 (phase 0 spike, R2), so
    // that shim failed open. If curl fails (coop not ready) the hook outputs nothing
    // and the agent proceeds. Builds the event envelope once and sends it to both
    // the pipe and the endpoint.
    //
    // This is the interim gate: ADR 0005 §6 replaces it with
    // `gb hook stop-gate --format gemini` (phase 2, `gb setup gemini`) so there is a
    // single source of gate policy, and then this curl shim is deleted.
    let after_agent_command = concat!(
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
            "SessionStart": [{
                "matcher": "*",
                "hooks": [{
                    "type": "command",
                    "command": session_start_command
                }]
            }],
            "BeforeAgent": [{
                "matcher": "*",
                "hooks": [{
                    "type": "command",
                    "command": "input=$(cat); printf '{\"event\":\"before_agent\",\"data\":%s}\\n' \"$input\" > \"$COOP_HOOK_PIPE\""
                }]
            }],
            "BeforeTool": [{
                "matcher": "*",
                "hooks": [{
                    "type": "command",
                    "command": "input=$(cat); printf '{\"event\":\"pre_tool_use\",\"data\":%s}\\n' \"$input\" > \"$COOP_HOOK_PIPE\""
                }]
            }],
            "AfterTool": [{
                "matcher": "*",
                "hooks": [{
                    "type": "command",
                    "command": "input=$(cat); printf '{\"event\":\"after_tool\",\"data\":%s}\\n' \"$input\" > \"$COOP_HOOK_PIPE\""
                }]
            }],
            "AfterAgent": [{
                "matcher": "*",
                "hooks": [{
                    "type": "command",
                    "command": after_agent_command
                }]
            }],
            "SessionEnd": [{
                "matcher": "*",
                "hooks": [{
                    "type": "command",
                    "command": "cat > /dev/null; echo '{\"event\":\"session_end\"}' > \"$COOP_HOOK_PIPE\""
                }]
            }],
            "Notification": [{
                "matcher": "*",
                "hooks": [{
                    "type": "command",
                    "command": "input=$(cat); printf '{\"event\":\"notification\",\"data\":%s}\\n' \"$input\" > \"$COOP_HOOK_PIPE\""
                }]
            }]
        }
    })
}

/// Return environment variables to set on the Gemini child process.
pub use crate::driver::hook_env_vars;

/// Write the hook config to a settings file and return its path.
///
/// The config file is written into `config_dir` so Gemini can load it
/// via `GEMINI_CLI_SYSTEM_SETTINGS_PATH`.
pub fn write_hook_config(
    config_dir: &Path,
    pipe_path: &Path,
) -> anyhow::Result<std::path::PathBuf> {
    let config = generate_hook_config(pipe_path);
    let config_path = config_dir.join("coop-gemini-settings.json");
    let contents = serde_json::to_string_pretty(&config)?;
    std::fs::write(&config_path, contents)?;
    Ok(config_path)
}

#[cfg(test)]
#[path = "hooks_tests.rs"]
mod tests;
