// SPDX-License-Identifier: BUSL-1.1
// Copyright (c) 2026 Matt Baker <fimbaz@gmail.com>

use std::path::Path;

use super::{generate_hook_config, hook_env_vars, write_hook_config};

#[test]
fn generated_config_has_required_hooks() {
    let config = generate_hook_config(Path::new("/tmp/coop.pipe"));
    let hooks = &config["hooks"];

    assert!(hooks.get("SessionStart").is_some());
    assert!(hooks.get("PostToolUse").is_some());
    assert!(hooks.get("Stop").is_some());
    assert!(hooks.get("Notification").is_some());
    assert!(hooks.get("PreToolUse").is_some());
    assert!(hooks.get("UserPromptSubmit").is_some());

    // Verify nested matcher + hooks structure
    let post_tool = &hooks["PostToolUse"];
    assert!(post_tool.is_array());
    assert_eq!(post_tool[0]["matcher"], "");
    assert!(post_tool[0]["hooks"].is_array());
    assert_eq!(post_tool[0]["hooks"][0]["type"], "command");

    let stop = &hooks["Stop"];
    assert!(stop.is_array());
    assert_eq!(stop[0]["matcher"], "");
    assert!(stop[0]["hooks"].is_array());
    assert_eq!(stop[0]["hooks"][0]["type"], "command");

    let notification = &hooks["Notification"];
    assert!(notification.is_array());
    assert_eq!(notification[0]["matcher"], "idle_prompt|permission_prompt");
    assert!(notification[0]["hooks"].is_array());
    assert_eq!(notification[0]["hooks"][0]["type"], "command");

    // PreToolUse must match ALL tools — the ToolBefore events start the
    // per-tool timers behind gardenio_agent_tool_call_latency_seconds. A
    // restricted matcher here left the histogram registered but never
    // observed fleet-wide (ka-0000004093).
    let pre_tool = &hooks["PreToolUse"];
    assert!(pre_tool.is_array());
    assert_eq!(pre_tool[0]["matcher"], "");
    assert!(pre_tool[0]["hooks"].is_array());
    assert_eq!(pre_tool[0]["hooks"][0]["type"], "command");
}

#[test]
fn config_references_env_var() {
    let config = generate_hook_config(Path::new("/tmp/coop.pipe"));
    let config_str = serde_json::to_string(&config).unwrap_or_default();

    // Config should use $COOP_HOOK_PIPE, not a hardcoded path
    assert!(config_str.contains("COOP_HOOK_PIPE"));
    // Stop hook should reference $COOP_URL for the gating endpoint
    assert!(config_str.contains("COOP_URL"));
}

#[test]
fn env_vars_include_pipe_path_and_url() {
    let vars = hook_env_vars(Path::new("/tmp/coop.pipe"), "http://127.0.0.1:8080");
    assert_eq!(vars.len(), 2);
    assert_eq!(vars[0].0, "COOP_HOOK_PIPE");
    assert_eq!(vars[0].1, "/tmp/coop.pipe");
    assert_eq!(vars[1].0, "COOP_URL");
    assert_eq!(vars[1].1, "http://127.0.0.1:8080");
}

#[test]
fn generated_json_is_valid() {
    let config = generate_hook_config(Path::new("/tmp/coop.pipe"));
    // Round-trip through string to verify valid JSON
    let s = serde_json::to_string(&config).unwrap_or_default();
    let parsed: serde_json::Value = serde_json::from_str(&s).unwrap_or_default();
    assert!(parsed.get("hooks").is_some());
}

#[test]
fn write_hook_config_creates_file() -> anyhow::Result<()> {
    let dir = tempfile::tempdir()?;
    let pipe_path = Path::new("/tmp/test-coop.pipe");

    let config_path = write_hook_config(dir.path(), pipe_path)?;
    assert!(config_path.exists());

    let content = std::fs::read_to_string(&config_path)?;
    let parsed: serde_json::Value = serde_json::from_str(&content)?;
    assert!(parsed.get("hooks").is_some());
    Ok(())
}
