// SPDX-License-Identifier: BUSL-1.1
// Copyright (c) 2026 Matt Baker <fimbaz@gmail.com>

use std::path::Path;

use super::{generate_hook_config, hook_env_vars, write_hook_config};

#[test]
fn generated_config_has_required_hooks() {
    let config = generate_hook_config(Path::new("/tmp/coop.pipe"));
    let hooks = &config["hooks"];

    assert!(hooks.get("SessionStart").is_some());
    assert!(hooks.get("BeforeAgent").is_some());
    assert!(hooks.get("BeforeTool").is_some());
    assert!(hooks.get("AfterTool").is_some());
    assert!(hooks.get("AfterAgent").is_some());
    assert!(hooks.get("SessionEnd").is_some());
    assert!(hooks.get("Notification").is_some());

    // Verify nested matcher + hooks structure
    for hook_name in [
        "SessionStart",
        "BeforeAgent",
        "BeforeTool",
        "AfterTool",
        "AfterAgent",
        "SessionEnd",
        "Notification",
    ] {
        let hook = &hooks[hook_name];
        assert!(hook.is_array(), "{hook_name} should be an array");
        assert_eq!(hook[0]["matcher"], "*", "{hook_name} matcher should be wildcard");
        assert!(hook[0]["hooks"].is_array(), "{hook_name} hooks should be array");
        assert_eq!(hook[0]["hooks"][0]["type"], "command", "{hook_name} type should be command");
    }
}

#[test]
fn config_references_env_vars() {
    let config = generate_hook_config(Path::new("/tmp/coop.pipe"));
    let config_str = serde_json::to_string(&config).unwrap_or_default();

    // Config should use $COOP_HOOK_PIPE, not a hardcoded path
    assert!(config_str.contains("COOP_HOOK_PIPE"));
    // AfterAgent hook should reference $COOP_URL for gating
    assert!(config_str.contains("COOP_URL"));
}

#[test]
fn env_vars_include_pipe_path_and_coop_url() {
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

/// Run the generated AfterAgent command with a stand-in `curl` that answers
/// `verdict`, and return what the hook printed to stdout.
fn after_agent_stdout(verdict: &str) -> anyhow::Result<String> {
    use std::os::unix::fs::PermissionsExt;

    let dir = tempfile::tempdir()?;
    let curl = dir.path().join("curl");
    std::fs::write(&curl, format!("#!/bin/sh\ncat >/dev/null\nprintf '%s' '{verdict}'\n"))?;
    std::fs::set_permissions(&curl, std::fs::Permissions::from_mode(0o755))?;
    std::fs::write(dir.path().join("pipe"), "")?;

    let config = generate_hook_config(Path::new("/tmp/coop.pipe"));
    let command = config["hooks"]["AfterAgent"][0]["hooks"][0]["command"]
        .as_str()
        .ok_or_else(|| anyhow::anyhow!("AfterAgent has no command"))?;
    let path = format!("{}:{}", dir.path().display(), std::env::var("PATH").unwrap_or_default());
    let out = std::process::Command::new("sh")
        .arg("-c")
        .arg(command)
        .env("PATH", path)
        .env("COOP_HOOK_PIPE", dir.path().join("pipe"))
        .env("COOP_URL", "http://127.0.0.1:1")
        .env_remove("COOP_HOOK_TOKEN_FILE")
        .stdin(std::process::Stdio::piped())
        .output()?;
    Ok(String::from_utf8(out.stdout)?)
}

#[test]
fn stop_hook_echoes_a_block_verdict_in_the_form_gemini_honours() -> anyhow::Result<()> {
    // Gemini continues a turn on {"decision":"block","reason":...}; the old
    // {"continue":true} did nothing on 0.64.0, so the gate failed open.
    let verdict = r#"{"decision":"block","reason":"checkpoint","last_message":null}"#;
    let out = after_agent_stdout(verdict)?;
    let printed: serde_json::Value = serde_json::from_str(&out)?;
    assert_eq!(printed["decision"], "block");
    assert_eq!(printed["reason"], "checkpoint");
    assert!(printed.get("continue").is_none(), "{out}");
    Ok(())
}

#[test]
fn stop_hook_prints_nothing_when_the_stop_is_allowed() -> anyhow::Result<()> {
    assert_eq!(after_agent_stdout(r#"{"last_message":"done"}"#)?, "");
    Ok(())
}
