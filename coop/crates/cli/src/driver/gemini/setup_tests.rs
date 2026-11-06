// SPDX-License-Identifier: BUSL-1.1
// Copyright (c) 2026 Matt Baker <fimbaz@gmail.com>

use serde_json::json;

#[test]
fn prepare_session_creates_settings_file() -> anyhow::Result<()> {
    let setup = super::prepare_fresh("http://127.0.0.1:0", None, None)?;

    let settings_path = setup
        .env_vars
        .iter()
        .find(|(k, _)| k == "GEMINI_CLI_SYSTEM_SETTINGS_PATH")
        .map(|(_, v)| std::path::PathBuf::from(v))
        .ok_or_else(|| anyhow::anyhow!("no GEMINI_CLI_SYSTEM_SETTINGS_PATH env var"))?;
    assert!(settings_path.exists());

    let content = std::fs::read_to_string(&settings_path)?;
    let parsed: serde_json::Value = serde_json::from_str(&content)?;
    assert!(parsed.get("hooks").is_some());
    Ok(())
}

#[test]
fn prepare_session_has_env_vars() -> anyhow::Result<()> {
    let setup = super::prepare_fresh("http://127.0.0.1:0", None, None)?;

    assert!(setup.env_vars.iter().any(|(k, _)| k == "COOP_HOOK_PIPE"));
    assert!(setup.env_vars.iter().any(|(k, _)| k == "COOP_URL"));
    assert!(setup.env_vars.iter().any(|(k, _)| k == "GEMINI_CLI_SYSTEM_SETTINGS_PATH"));
    Ok(())
}

#[test]
fn prepare_session_pipe_path_in_session_dir() -> anyhow::Result<()> {
    let setup = super::prepare_fresh("http://127.0.0.1:0", None, None)?;

    let pipe =
        setup.hook_pipe_path.as_ref().ok_or_else(|| anyhow::anyhow!("expected hook_pipe_path"))?;
    assert_eq!(pipe.file_name().and_then(|n| n.to_str()), Some("hook.pipe"));
    assert!(pipe.starts_with(&setup.session_dir));
    Ok(())
}

#[test]
fn prepare_session_has_no_extra_args() -> anyhow::Result<()> {
    let setup = super::prepare_fresh("http://127.0.0.1:0", None, None)?;
    assert!(setup.extra_args.is_empty());
    Ok(())
}

#[test]
fn prepare_session_with_base_settings_merges_hooks() -> anyhow::Result<()> {
    let orchestrator = json!({
        "hooks": {
            "SessionStart": [{"matcher": "*", "hooks": [{"type": "command", "command": "gt-prime"}]}],
            "BeforeTool": [{"matcher": "*", "hooks": [{"type": "command", "command": "gt-guard"}]}]
        },
        "permissions": { "allow": ["shell"] }
    });
    let setup = super::prepare_fresh("http://127.0.0.1:0", Some(&orchestrator), None)?;

    let settings_path = setup
        .env_vars
        .iter()
        .find(|(k, _)| k == "GEMINI_CLI_SYSTEM_SETTINGS_PATH")
        .map(|(_, v)| std::path::PathBuf::from(v))
        .ok_or_else(|| anyhow::anyhow!("no GEMINI_CLI_SYSTEM_SETTINGS_PATH env var"))?;
    let content = std::fs::read_to_string(&settings_path)?;
    let parsed: serde_json::Value = serde_json::from_str(&content)?;

    // SessionStart: orchestrator entry first, coop entry second
    let session_start = parsed["hooks"]["SessionStart"]
        .as_array()
        .ok_or_else(|| anyhow::anyhow!("no SessionStart hooks"))?;
    assert!(session_start.len() >= 2);
    assert_eq!(session_start[0]["hooks"][0]["command"], "gt-prime");

    // Permissions pass through from orchestrator
    assert_eq!(parsed["permissions"]["allow"][0], "shell");

    // Coop-only hook types present
    assert!(parsed["hooks"]["AfterAgent"].as_array().is_some());
    assert!(parsed["hooks"]["BeforeAgent"].as_array().is_some());
    Ok(())
}

#[test]
fn prepare_session_with_mcp_config_includes_servers() -> anyhow::Result<()> {
    let mcp = json!({
        "my-server": { "command": "node", "args": ["server.js"] }
    });
    let setup = super::prepare_fresh("http://127.0.0.1:0", None, Some(&mcp))?;

    let settings_path = setup
        .env_vars
        .iter()
        .find(|(k, _)| k == "GEMINI_CLI_SYSTEM_SETTINGS_PATH")
        .map(|(_, v)| std::path::PathBuf::from(v))
        .ok_or_else(|| anyhow::anyhow!("no GEMINI_CLI_SYSTEM_SETTINGS_PATH env var"))?;
    let content = std::fs::read_to_string(&settings_path)?;
    let parsed: serde_json::Value = serde_json::from_str(&content)?;

    // mcpServers merged into settings
    assert_eq!(parsed["mcpServers"]["my-server"]["command"], "node");
    // hooks still present
    assert!(parsed.get("hooks").is_some());
    Ok(())
}
