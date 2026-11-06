// SPDX-License-Identifier: BUSL-1.1
// Copyright (c) 2026 Matt Baker <fimbaz@gmail.com>

use std::time::Duration;

use clap::Parser;
use serde_json::json;
use serial_test::serial;

use super::{merge_settings, AgentFileConfig, AgentType, Config, GroomLevel};

fn parse(args: &[&str]) -> Config {
    Config::parse_from(args)
}

#[test]
fn valid_config_with_port_and_command() -> anyhow::Result<()> {
    let config = parse(&["coop", "--port", "8080", "--", "echo", "hello"]);
    config.validate()?;
    assert_eq!(config.port, Some(8080));
    assert_eq!(config.command, vec!["echo", "hello"]);
    Ok(())
}

#[test]
fn valid_config_with_socket_and_command() -> anyhow::Result<()> {
    let config = parse(&["coop", "--socket", "/tmp/coop.sock", "--", "bash"]);
    config.validate()?;
    assert_eq!(config.socket.as_deref(), Some("/tmp/coop.sock"));
    Ok(())
}

#[test]
fn valid_config_with_attach() -> anyhow::Result<()> {
    let config = parse(&["coop", "--port", "8080", "--attach", "tmux:my-session"]);
    config.validate()?;
    assert_eq!(config.attach.as_deref(), Some("tmux:my-session"));
    Ok(())
}

#[yare::parameterized(
    no_transport        = { &["coop", "--", "echo"], "--port or --socket" },
    no_command          = { &["coop", "--port", "8080"], "agent command is required" },
    both_cmd_and_attach = { &["coop", "--port", "8080", "--attach", "tmux:sess", "--", "echo"],
                            "cannot specify both" },
)]
fn invalid_config(args: &[&str], expected_substr: &str) {
    let config = parse(args);
    crate::assert_err_contains!(config.validate(), expected_substr);
}

#[test]
fn agent_explicit_flag() -> anyhow::Result<()> {
    let config = parse(&["coop", "--port", "8080", "--agent", "claude", "--", "echo"]);
    assert_eq!(config.agent_enum()?, AgentType::Claude);
    Ok(())
}

#[test]
fn agent_unknown_when_no_match() -> anyhow::Result<()> {
    let config = parse(&["coop", "--port", "8080", "--", "echo"]);
    assert_eq!(config.agent_enum()?, AgentType::Unknown);
    Ok(())
}

#[test]
fn agent_invalid() {
    let config = parse(&["coop", "--port", "8080", "--agent", "gpt", "--", "echo"]);
    assert!(config.agent_enum().is_err());
}

#[test]
fn agent_auto_detect_claude() -> anyhow::Result<()> {
    let config =
        parse(&["coop", "--port", "8080", "--", "claude", "--dangerously-skip-permissions"]);
    assert_eq!(config.agent_enum()?, AgentType::Claude);
    Ok(())
}

#[test]
fn agent_auto_detect_claudeless() -> anyhow::Result<()> {
    let config = parse(&["coop", "--port", "8080", "--", "claudeless", "--scenario", "test.toml"]);
    assert_eq!(config.agent_enum()?, AgentType::Claude);
    Ok(())
}

#[test]
fn agent_auto_detect_gemini() -> anyhow::Result<()> {
    let config = parse(&["coop", "--port", "8080", "--", "gemini"]);
    assert_eq!(config.agent_enum()?, AgentType::Gemini);
    Ok(())
}

#[test]
fn agent_auto_detect_from_path() -> anyhow::Result<()> {
    let config = parse(&["coop", "--port", "8080", "--", "/usr/local/bin/claude"]);
    assert_eq!(config.agent_enum()?, AgentType::Claude);
    Ok(())
}

#[test]
fn agent_explicit_overrides_auto_detect() -> anyhow::Result<()> {
    let config = parse(&["coop", "--port", "8080", "--agent", "unknown", "--", "claude"]);
    assert_eq!(config.agent_enum()?, AgentType::Unknown);
    Ok(())
}

// -- Socket path validation --

#[test]
fn socket_path_too_long() {
    let long_path = format!("/tmp/{}", "a".repeat(200));
    let config = parse(&["coop", "--socket", &long_path, "--", "echo"]);
    crate::assert_err_contains!(config.validate(), "socket path");
}

#[test]
fn socket_path_within_limit() -> anyhow::Result<()> {
    let config = parse(&["coop", "--socket", "/tmp/coop.sock", "--", "echo"]);
    config.validate()?;
    Ok(())
}

// -- GroomLevel --

#[test]
fn groom_auto_default() -> anyhow::Result<()> {
    let config = parse(&["coop", "--port", "8080", "--", "echo"]);
    assert_eq!(config.groom_level()?, GroomLevel::Auto);
    Ok(())
}

#[test]
fn groom_manual() -> anyhow::Result<()> {
    let config = parse(&["coop", "--port", "8080", "--groom", "manual", "--", "echo"]);
    assert_eq!(config.groom_level()?, GroomLevel::Manual);
    Ok(())
}

#[test]
fn groom_case_insensitive() -> anyhow::Result<()> {
    let config = parse(&["coop", "--port", "8080", "--groom", "AUTO", "--", "echo"]);
    assert_eq!(config.groom_level()?, GroomLevel::Auto);
    Ok(())
}

#[test]
fn groom_pristine_validates() -> anyhow::Result<()> {
    let config = parse(&["coop", "--port", "8080", "--groom", "pristine", "--", "echo"]);
    config.validate()?;
    assert_eq!(config.groom_level()?, GroomLevel::Pristine);
    Ok(())
}

#[test]
fn groom_pristine_rejects_resume() {
    let config = parse(&[
        "coop", "--port", "8080", "--agent", "claude", "--groom", "pristine", "--resume",
        "some-id", "--", "claude",
    ]);
    crate::assert_err_contains!(
        config.validate(),
        "--resume cannot be combined with groom=pristine"
    );
}

#[test]
fn groom_invalid() {
    let config = parse(&["coop", "--port", "8080", "--groom", "nope", "--", "echo"]);
    assert!(config.groom_level().is_err());
}

#[test]
#[serial]
fn defaults_are_correct() {
    // `host` is a clap arg with env="COOP_HOST"; clear it so we assert the
    // compiled-in default rather than whatever the ambient process env holds
    // (gardenio agent pods set COOP_HOST=0.0.0.0). #[serial] keeps the env
    // mutation isolated from other env-reading tests.
    let saved = std::env::var("COOP_HOST").ok();
    std::env::remove_var("COOP_HOST");

    let config = parse(&["coop", "--port", "8080", "--", "echo"]);
    assert_eq!(config.host, "127.0.0.1");

    match saved {
        Some(v) => std::env::set_var("COOP_HOST", v),
        None => std::env::remove_var("COOP_HOST"),
    }

    assert_eq!(config.cols, 200);
    assert_eq!(config.rows, 50);
    assert_eq!(config.ring_size, 1048576);
    assert_eq!(config.log_format, "json");
    assert_eq!(config.log_level, "info");
}

#[test]
fn env_duration_defaults() {
    // These read env vars, so with no env set we get production defaults.
    let config = parse(&["coop", "--port", "8080", "--", "echo"]);
    assert_eq!(config.shutdown_timeout(), Duration::from_secs(10));
    assert_eq!(config.screen_debounce(), Duration::from_millis(50));
    assert_eq!(config.process_poll(), Duration::from_secs(10));
    assert_eq!(config.screen_poll(), Duration::from_secs(3));
    assert_eq!(config.log_poll(), Duration::from_secs(3));
    assert_eq!(config.tmux_poll(), Duration::from_secs(1));
    assert_eq!(config.reap_poll(), Duration::from_millis(50));
    assert_eq!(config.input_delay(), Duration::from_millis(200));
    assert_eq!(config.input_delay_per_byte(), Duration::from_millis(1));
    assert_eq!(config.nudge_timeout(), Duration::from_millis(2000));
    assert_eq!(config.idle_timeout(), Duration::ZERO);
    assert_eq!(config.groom_dismiss_delay(), Duration::from_millis(500));
}

// -- Config::test() fast timings --

#[test]
fn test_config_uses_fast_timings() {
    let config = Config::test();
    assert_eq!(config.shutdown_timeout(), Duration::from_millis(100));
    assert_eq!(config.screen_debounce(), Duration::from_millis(10));
    assert_eq!(config.process_poll(), Duration::from_millis(50));
    assert_eq!(config.screen_poll(), Duration::from_millis(50));
    assert_eq!(config.log_poll(), Duration::from_millis(50));
    assert_eq!(config.tmux_poll(), Duration::from_millis(50));
    assert_eq!(config.reap_poll(), Duration::from_millis(10));
    assert_eq!(config.input_delay(), Duration::from_millis(10));
    assert_eq!(config.input_delay_per_byte(), Duration::ZERO);
    assert_eq!(config.nudge_timeout(), Duration::from_millis(100));
    assert_eq!(config.idle_timeout(), Duration::ZERO);
    assert_eq!(config.drain_timeout(), Duration::from_millis(100));
    assert_eq!(config.groom_dismiss_delay(), Duration::from_millis(50));
}

// -- AgentFileConfig deserialization --

#[test]
fn agent_file_config_deserializes_settings_and_mcp() -> anyhow::Result<()> {
    let input = json!({
        "settings": {
            "hooks": { "PostToolUse": [{"matcher": "", "hooks": []}] },
            "permissions": { "allow": ["Bash"] }
        },
        "mcp": {
            "my-server": { "command": "node", "args": ["server.js"] }
        }
    });
    let config: AgentFileConfig = serde_json::from_value(input)?;
    assert!(config.settings.is_some());
    assert!(config.mcp.is_some());
    let mcp = config.mcp.as_ref().unwrap();
    assert!(mcp.get("my-server").is_some());
    Ok(())
}

#[test]
fn agent_file_config_missing_settings_and_mcp() -> anyhow::Result<()> {
    let input = json!({});
    let config: AgentFileConfig = serde_json::from_value(input)?;
    assert!(config.settings.is_none());
    assert!(config.mcp.is_none());
    Ok(())
}

// -- merge_settings --

#[test]
fn merge_no_orchestrator_returns_coop_config() {
    let coop = json!({
        "hooks": {
            "PostToolUse": [{"matcher": "", "hooks": [{"type": "command", "command": "coop-detect"}]}],
            "Stop": [{"matcher": "", "hooks": [{"type": "command", "command": "coop-gate"}]}]
        }
    });
    // When orchestrator has no hooks, coop hooks should appear as-is.
    let orchestrator = json!({});
    let merged = merge_settings(&orchestrator, coop.clone());
    assert_eq!(merged["hooks"]["PostToolUse"], coop["hooks"]["PostToolUse"]);
    assert_eq!(merged["hooks"]["Stop"], coop["hooks"]["Stop"]);
}

#[test]
fn merge_concatenates_hook_arrays_orchestrator_first() {
    let orchestrator = json!({
        "hooks": {
            "SessionStart": [{"matcher": "", "hooks": [{"type": "command", "command": "gt-prime"}]}]
        }
    });
    let coop = json!({
        "hooks": {
            "SessionStart": [{"matcher": "", "hooks": [{"type": "command", "command": "coop-detect"}]}]
        }
    });
    let merged = merge_settings(&orchestrator, coop);
    let arr = merged["hooks"]["SessionStart"].as_array().unwrap();
    assert_eq!(arr.len(), 2);
    // Orchestrator entry first
    assert_eq!(arr[0]["hooks"][0]["command"], "gt-prime");
    // Coop entry second
    assert_eq!(arr[1]["hooks"][0]["command"], "coop-detect");
}

#[test]
fn merge_preserves_non_hook_keys() {
    let orchestrator = json!({
        "permissions": { "allow": ["Bash", "Read"] },
        "env": { "MY_VAR": "hello" },
        "hooks": {}
    });
    let coop = json!({
        "hooks": {
            "Stop": [{"matcher": "", "hooks": []}]
        }
    });
    let merged = merge_settings(&orchestrator, coop);
    assert_eq!(merged["permissions"]["allow"][0], "Bash");
    assert_eq!(merged["env"]["MY_VAR"], "hello");
}

#[test]
fn merge_orchestrator_only_hook_types_pass_through() {
    let orchestrator = json!({
        "hooks": {
            "PreToolUse": [{"matcher": "Bash", "hooks": [{"type": "command", "command": "gt-guard"}]}]
        }
    });
    let coop = json!({
        "hooks": {
            "PostToolUse": [{"matcher": "", "hooks": []}]
        }
    });
    let merged = merge_settings(&orchestrator, coop);
    // Orchestrator-only hook type preserved
    assert_eq!(merged["hooks"]["PreToolUse"][0]["hooks"][0]["command"], "gt-guard");
    // Coop-only hook type added
    assert!(merged["hooks"]["PostToolUse"].as_array().is_some());
}

#[test]
fn merge_coop_only_hook_types_appear_in_result() {
    let orchestrator = json!({
        "hooks": {
            "SessionStart": [{"matcher": "", "hooks": []}]
        }
    });
    let coop = json!({
        "hooks": {
            "Stop": [{"matcher": "", "hooks": [{"type": "command", "command": "coop-gate"}]}],
            "Notification": [{"matcher": "idle_prompt", "hooks": []}]
        }
    });
    let merged = merge_settings(&orchestrator, coop);
    assert!(merged["hooks"]["Stop"].as_array().is_some());
    assert!(merged["hooks"]["Notification"].as_array().is_some());
    // Original orchestrator hook still there
    assert!(merged["hooks"]["SessionStart"].as_array().is_some());
}

#[test]
fn merge_realistic_gt_config() {
    let orchestrator = json!({
        "hooks": {
            "SessionStart": [{"matcher": "", "hooks": [{"type": "command", "command": "gt-prime-context"}]}],
            "PreToolUse": [{"matcher": "Bash", "hooks": [{"type": "command", "command": "gt-sandbox-guard"}]}]
        },
        "permissions": {
            "allow": ["Bash", "Read", "Write", "Edit"],
            "deny": []
        },
        "env": { "GT_WORKSPACE_ID": "ws-123" }
    });
    let coop = json!({
        "hooks": {
            "SessionStart": [{"matcher": "", "hooks": [{"type": "command", "command": "coop-start-hook"}]}],
            "PostToolUse": [{"matcher": "", "hooks": [{"type": "command", "command": "coop-post-tool"}]}],
            "Stop": [{"matcher": "", "hooks": [{"type": "command", "command": "coop-stop-gate"}]}],
            "Notification": [{"matcher": "idle_prompt|permission_prompt", "hooks": [{"type": "command", "command": "coop-notify"}]}],
            "PreToolUse": [{"matcher": "ExitPlanMode|AskUserQuestion|EnterPlanMode", "hooks": [{"type": "command", "command": "coop-pre-tool"}]}],
            "UserPromptSubmit": [{"matcher": "", "hooks": [{"type": "command", "command": "coop-prompt-submit"}]}]
        }
    });
    let merged = merge_settings(&orchestrator, coop);

    // SessionStart: gt-prime first, coop second
    let session_start = merged["hooks"]["SessionStart"].as_array().unwrap();
    assert_eq!(session_start.len(), 2);
    assert_eq!(session_start[0]["hooks"][0]["command"], "gt-prime-context");
    assert_eq!(session_start[1]["hooks"][0]["command"], "coop-start-hook");

    // PreToolUse: gt-guard first, coop second
    let pre_tool = merged["hooks"]["PreToolUse"].as_array().unwrap();
    assert_eq!(pre_tool.len(), 2);
    assert_eq!(pre_tool[0]["hooks"][0]["command"], "gt-sandbox-guard");
    assert_eq!(pre_tool[1]["hooks"][0]["command"], "coop-pre-tool");

    // Coop-only hooks present
    assert!(merged["hooks"]["PostToolUse"].as_array().is_some());
    assert!(merged["hooks"]["Stop"].as_array().is_some());
    assert!(merged["hooks"]["Notification"].as_array().is_some());
    assert!(merged["hooks"]["UserPromptSubmit"].as_array().is_some());

    // Non-hook keys pass through
    assert_eq!(merged["permissions"]["allow"][0], "Bash");
    assert_eq!(merged["env"]["GT_WORKSPACE_ID"], "ws-123");
}

// --- nats_token_resolved (W5, ka-0000006585) ----------------------------------

#[test]
fn nats_token_resolved_returns_env_when_no_file() {
    let mut config = Config::test();
    config.nats_token = Some("from-env".to_owned());
    config.nats_token_file = None;
    assert_eq!(config.nats_token_resolved().as_deref(), Some("from-env"));
}

#[test]
fn nats_token_resolved_reads_file_and_trims_trailing_whitespace() -> anyhow::Result<()> {
    let tmp = tempfile::NamedTempFile::new()?;
    std::fs::write(tmp.path(), b"file-token\n  \r\n")?;
    let mut config = Config::test();
    config.nats_token = Some("env-token-should-be-ignored".to_owned());
    config.nats_token_file = Some(tmp.path().to_path_buf());
    assert_eq!(config.nats_token_resolved().as_deref(), Some("file-token"));
    Ok(())
}

#[test]
fn nats_token_resolved_falls_back_to_env_when_file_missing() {
    let mut config = Config::test();
    config.nats_token = Some("env-fallback".to_owned());
    config.nats_token_file = Some(std::path::PathBuf::from("/nonexistent/path/nats-token"));
    assert_eq!(config.nats_token_resolved().as_deref(), Some("env-fallback"));
}

#[test]
fn nats_token_resolved_falls_back_when_file_empty() -> anyhow::Result<()> {
    let tmp = tempfile::NamedTempFile::new()?;
    std::fs::write(tmp.path(), b"\n  \n")?;
    let mut config = Config::test();
    config.nats_token = Some("env-fallback".to_owned());
    config.nats_token_file = Some(tmp.path().to_path_buf());
    assert_eq!(config.nats_token_resolved().as_deref(), Some("env-fallback"));
    Ok(())
}

#[test]
fn nats_token_resolved_returns_none_when_neither_set() {
    let mut config = Config::test();
    config.nats_token = None;
    config.nats_token_file = None;
    assert_eq!(config.nats_token_resolved(), None);
}

// --- object-store credential file env vars (gd-W8OwRoHLLn) -------------------

/// Endpoint mode (in-box MinIO, no IRSA): the controller projects the
/// credential Secret as two files and wires their paths via
/// COOP_S3_ACCESS_KEY_ID_FILE / COOP_S3_SECRET_ACCESS_KEY_FILE. Pin that both
/// env vars parse into the config fields untouched (no processing at parse
/// time — the files are read later, in s3.rs).
#[test]
#[serial]
fn s3_credential_file_env_vars_parse_into_config() -> anyhow::Result<()> {
    std::env::set_var("COOP_S3_ACCESS_KEY_ID_FILE", "/etc/object-store-creds/access_key_id");
    std::env::set_var(
        "COOP_S3_SECRET_ACCESS_KEY_FILE",
        "/etc/object-store-creds/secret_access_key",
    );
    let cfg = Config::try_parse_from(["coop"])?;
    assert_eq!(
        cfg.s3_access_key_id_file.as_deref(),
        Some(std::path::Path::new("/etc/object-store-creds/access_key_id"))
    );
    assert_eq!(
        cfg.s3_secret_access_key_file.as_deref(),
        Some(std::path::Path::new("/etc/object-store-creds/secret_access_key"))
    );
    std::env::remove_var("COOP_S3_ACCESS_KEY_ID_FILE");
    std::env::remove_var("COOP_S3_SECRET_ACCESS_KEY_FILE");
    Ok(())
}

/// AWS/IRSA mode (endpoint unset): the controller never sets either env var,
/// so both fields stay `None` and the S3 client build path falls back to the
/// default AWS credential chain untouched.
#[test]
#[serial]
fn s3_credential_file_env_vars_default_to_none() -> anyhow::Result<()> {
    std::env::remove_var("COOP_S3_ACCESS_KEY_ID_FILE");
    std::env::remove_var("COOP_S3_SECRET_ACCESS_KEY_FILE");
    let cfg = Config::try_parse_from(["coop"])?;
    assert_eq!(cfg.s3_access_key_id_file, None);
    assert_eq!(cfg.s3_secret_access_key_file, None);
    Ok(())
}

// -- env-backed boolean flags (gardenio ka-0000001975) ------------------------
//
// A clap-derive `bool` is an ArgAction::SetTrue flag whose env value is parsed
// by the STRICT bool parser: only the literal "true" / "false" are accepted,
// and anything else is a fatal usage error (`main` exits 2 before coop starts).
// The gardenio controller wired `COOP_S3_NO_RESTORE=1`; coop refused to boot on
// every restart and the supervisor gave up with max_restarts_reached — killing
// the agent that force-clean exists to recover. These tests pin the boolish
// parsing that makes the wire format forgiving in both directions.

/// The exact byte sequence the gardenio controller writes (init_force_clean.go
/// `forceCleanEnvValue`), plus every other spelling an operator or a shell
/// script plausibly reaches for. All must parse; none may abort.
#[test]
#[serial]
fn s3_no_restore_accepts_boolish_env_spellings() -> anyhow::Result<()> {
    for truthy in ["1", "true", "TRUE", "True", "yes", "on"] {
        std::env::set_var("COOP_S3_NO_RESTORE", truthy);
        let cfg = Config::try_parse_from(["coop"])
            .unwrap_or_else(|e| panic!("COOP_S3_NO_RESTORE={truthy} must parse, got: {e}"));
        assert!(cfg.s3_no_restore, "COOP_S3_NO_RESTORE={truthy} should be true");
    }
    for falsy in ["0", "false", "FALSE", "no", "off"] {
        std::env::set_var("COOP_S3_NO_RESTORE", falsy);
        let cfg = Config::try_parse_from(["coop"])
            .unwrap_or_else(|e| panic!("COOP_S3_NO_RESTORE={falsy} must parse, got: {e}"));
        assert!(!cfg.s3_no_restore, "COOP_S3_NO_RESTORE={falsy} should be false");
    }
    std::env::remove_var("COOP_S3_NO_RESTORE");
    Ok(())
}

/// The bare flag form must keep working — `num_args(0..=1)` plus
/// `default_missing_value` is what preserves it once the arg takes a value.
#[test]
#[serial]
fn s3_no_restore_bare_flag_still_sets_true() -> anyhow::Result<()> {
    std::env::remove_var("COOP_S3_NO_RESTORE");
    let cfg = Config::try_parse_from(["coop", "--s3-no-restore"])?;
    assert!(cfg.s3_no_restore, "bare --s3-no-restore must still set the flag");

    let cfg = Config::try_parse_from(["coop"])?;
    assert!(!cfg.s3_no_restore, "unset must default to false (resume normally)");
    Ok(())
}

/// An explicit CLI value must beat the environment, so an operator can turn the
/// controller's force-clean back off on a one-off debug run.
#[test]
#[serial]
fn s3_no_restore_cli_value_overrides_env() -> anyhow::Result<()> {
    std::env::set_var("COOP_S3_NO_RESTORE", "1");
    let cfg = Config::try_parse_from(["coop", "--s3-no-restore", "false"])?;
    assert!(!cfg.s3_no_restore, "explicit CLI value must win over env");
    std::env::remove_var("COOP_S3_NO_RESTORE");
    Ok(())
}

/// `--record` shares the exact same shape and would have failed the same way.
#[test]
#[serial]
fn record_accepts_boolish_env_spellings() -> anyhow::Result<()> {
    std::env::set_var("COOP_RECORD", "1");
    let cfg = Config::try_parse_from(["coop"])?;
    assert!(cfg.record, "COOP_RECORD=1 must parse as true");

    std::env::set_var("COOP_RECORD", "0");
    let cfg = Config::try_parse_from(["coop"])?;
    assert!(!cfg.record, "COOP_RECORD=0 must parse as false");

    std::env::remove_var("COOP_RECORD");
    let cfg = Config::try_parse_from(["coop"])?;
    assert!(!cfg.record, "unset COOP_RECORD must default to false");
    Ok(())
}
