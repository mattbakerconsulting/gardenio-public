// SPDX-License-Identifier: BUSL-1.1
// Copyright (c) 2026 Matt Baker <fimbaz@gmail.com>

use std::path::Path;

use serde_json::json;
use serial_test::serial;

use super::{project_dir_name, write_credentials_file};

#[test]
fn project_dir_name_keeps_leading_dash() {
    let name = project_dir_name(Path::new("/Users/alice/projects/myapp"));
    assert!(name.starts_with('-'));
    assert_eq!(name, "-Users-alice-projects-myapp");
}

#[test]
fn project_dir_name_replaces_all_non_alphanumeric() {
    let name = project_dir_name(Path::new("/a/b.c/d"));
    assert!(!name.contains('/'));
    assert!(!name.contains('.'));
    assert_eq!(name, "-a-b-c-d");

    // Spaces, underscores, tildes — all become dashes
    let name2 = project_dir_name(Path::new("/home/user/my_project (copy)"));
    assert!(!name2.contains('_'));
    assert!(!name2.contains(' '));
    assert!(!name2.contains('('));
    assert_eq!(name2, "-home-user-my-project--copy-");
}

#[test]
fn prepare_session_creates_settings_file() -> anyhow::Result<()> {
    let work_dir = tempfile::tempdir()?;
    let setup =
        super::prepare(work_dir.path(), "http://127.0.0.1:0", None, None, false, None, None)?;

    let settings_arg_idx = setup
        .extra_args
        .iter()
        .position(|a| a == "--settings")
        .ok_or_else(|| anyhow::anyhow!("no --settings arg"))?;
    let settings_path = Path::new(&setup.extra_args[settings_arg_idx + 1]);
    assert!(settings_path.exists());

    let content = std::fs::read_to_string(settings_path)?;
    let parsed: serde_json::Value = serde_json::from_str(&content)?;
    assert!(parsed.get("hooks").is_some());
    Ok(())
}

#[test]
fn prepare_fresh_session_has_no_session_id_arg() -> anyhow::Result<()> {
    // Fresh sessions must NOT pass --session-id to Claude Code.
    // In newer Claude Code versions, --session-id is treated as --resume,
    // which fails with "No conversation found" for a brand-new session UUID.
    let work_dir = tempfile::tempdir()?;
    let setup =
        super::prepare(work_dir.path(), "http://127.0.0.1:0", None, None, false, None, None)?;

    assert!(
        !setup.extra_args.contains(&"--session-id".to_owned()),
        "--session-id must not be passed for fresh sessions (newer Claude Code treats it as --resume)"
    );
    Ok(())
}

#[test]
fn prepare_session_has_env_vars() -> anyhow::Result<()> {
    let work_dir = tempfile::tempdir()?;
    let setup =
        super::prepare(work_dir.path(), "http://127.0.0.1:0", None, None, false, None, None)?;

    assert!(setup.env_vars.iter().any(|(k, _)| k == "COOP_HOOK_PIPE"));
    Ok(())
}

#[test]
fn prepare_session_pipe_path_in_session_dir() -> anyhow::Result<()> {
    let work_dir = tempfile::tempdir()?;
    let setup =
        super::prepare(work_dir.path(), "http://127.0.0.1:0", None, None, false, None, None)?;

    let pipe =
        setup.hook_pipe_path.as_ref().ok_or_else(|| anyhow::anyhow!("expected hook_pipe_path"))?;
    assert_eq!(pipe.file_name().and_then(|n| n.to_str()), Some("hook.pipe"));
    Ok(())
}

#[test]
fn prepare_session_with_base_settings_merges_hooks() -> anyhow::Result<()> {
    let work_dir = tempfile::tempdir()?;
    let orchestrator = json!({
        "hooks": {
            "SessionStart": [{"matcher": "", "hooks": [{"type": "command", "command": "gt-prime"}]}],
            "PreToolUse": [{"matcher": "Bash", "hooks": [{"type": "command", "command": "gt-guard"}]}]
        },
        "permissions": { "allow": ["Bash", "Read"] }
    });
    let setup = super::prepare(
        work_dir.path(),
        "http://127.0.0.1:0",
        Some(&orchestrator),
        None,
        false,
        None,
        None,
    )?;

    let settings_arg_idx = setup
        .extra_args
        .iter()
        .position(|a| a == "--settings")
        .ok_or_else(|| anyhow::anyhow!("no --settings arg"))?;
    let settings_path = Path::new(&setup.extra_args[settings_arg_idx + 1]);
    let content = std::fs::read_to_string(settings_path)?;
    let parsed: serde_json::Value = serde_json::from_str(&content)?;

    // SessionStart: orchestrator entry first, coop entry second
    let session_start = parsed["hooks"]["SessionStart"]
        .as_array()
        .ok_or_else(|| anyhow::anyhow!("no SessionStart hooks"))?;
    assert!(session_start.len() >= 2);
    assert_eq!(session_start[0]["hooks"][0]["command"], "gt-prime");

    // Orchestrator permissions pass through, coop send rule appended
    let allow = parsed["permissions"]["allow"].as_array().unwrap();
    assert_eq!(allow[0], "Bash");
    assert_eq!(allow[1], "Read");
    assert!(allow.contains(&json!("Bash(coop send:*)")));

    // Coop-only hook types present
    assert!(parsed["hooks"]["PostToolUse"].as_array().is_some());
    assert!(parsed["hooks"]["Stop"].as_array().is_some());
    Ok(())
}

#[test]
fn prepare_session_injects_coop_send_permission() -> anyhow::Result<()> {
    let work_dir = tempfile::tempdir()?;
    let setup =
        super::prepare(work_dir.path(), "http://127.0.0.1:0", None, None, false, None, None)?;

    let settings_arg_idx = setup
        .extra_args
        .iter()
        .position(|a| a == "--settings")
        .ok_or_else(|| anyhow::anyhow!("no --settings arg"))?;
    let settings_path = Path::new(&setup.extra_args[settings_arg_idx + 1]);
    let content = std::fs::read_to_string(settings_path)?;
    let parsed: serde_json::Value = serde_json::from_str(&content)?;

    let allow = parsed["permissions"]["allow"].as_array().unwrap();
    assert!(allow.contains(&json!("Bash(coop send:*)")));
    Ok(())
}

#[test]
fn prepare_session_with_mcp_writes_config() -> anyhow::Result<()> {
    let work_dir = tempfile::tempdir()?;
    let mcp = json!({
        "my-server": { "command": "node", "args": ["server.js"] }
    });
    let setup =
        super::prepare(work_dir.path(), "http://127.0.0.1:0", None, Some(&mcp), false, None, None)?;

    let mcp_idx = setup
        .extra_args
        .iter()
        .position(|a| a == "--mcp-config")
        .ok_or_else(|| anyhow::anyhow!("no --mcp-config arg"))?;
    let mcp_path = Path::new(&setup.extra_args[mcp_idx + 1]);
    assert!(mcp_path.exists());

    let content = std::fs::read_to_string(mcp_path)?;
    let parsed: serde_json::Value = serde_json::from_str(&content)?;
    assert_eq!(parsed["mcpServers"]["my-server"]["command"], "node");
    Ok(())
}

#[test]
fn prepare_resume_uses_explicit_session_id() -> anyhow::Result<()> {
    // An S3-restored session log is always named `session.jsonl`.
    // The legacy file-stem path would have derived `session_id = "session"`,
    // clobbering the agent bead's last_session_id and breaking --resume.
    //
    // With an explicit resume_session_id, that UUID must drive everything.
    let work_dir = tempfile::tempdir()?;
    let log_path = work_dir.path().join("session.jsonl");
    std::fs::write(&log_path, "")?;
    let real_uuid = "550e8400-e29b-41d4-a716-446655440000";

    let setup = super::prepare(
        work_dir.path(),
        "http://127.0.0.1:0",
        None,
        None,
        false,
        Some(&log_path),
        Some(real_uuid),
    )?;

    assert_eq!(setup.session_id, real_uuid, "explicit resume_session_id must win");
    let resume_idx = setup
        .extra_args
        .iter()
        .position(|a| a == "--resume")
        .ok_or_else(|| anyhow::anyhow!("no --resume arg"))?;
    assert_eq!(setup.extra_args[resume_idx + 1], real_uuid);
    Ok(())
}

#[test]
fn prepare_resume_does_not_use_file_stem_for_session_id() -> anyhow::Result<()> {
    // The predicted-UUID branch was removed in ka-0000003077. Even when the
    // log path stem looks like a UUID, we MUST NOT use the file name as the
    // session id — the JSONL `sessionId` field is the source of truth.
    //
    // With an empty log (no parseable sessionId) and no explicit id, the
    // dispatcher falls through to a fresh session.
    let work_dir = tempfile::tempdir()?;
    let stem_uuid = "12345678-1234-1234-1234-123456789abc";
    let log_path = work_dir.path().join(format!("{stem_uuid}.jsonl"));
    std::fs::write(&log_path, "")?;

    let setup = super::prepare(
        work_dir.path(),
        "http://127.0.0.1:0",
        None,
        None,
        false,
        Some(&log_path),
        None,
    )?;

    // No --resume arg: dispatcher fell back to fresh.
    assert!(
        !setup.extra_args.iter().any(|a| a == "--resume"),
        "must not pass --resume when session id cannot be resolved"
    );
    // session_id is freshly generated, not the file stem.
    assert_ne!(setup.session_id, stem_uuid, "must not use file stem as session id");
    Ok(())
}

#[test]
fn prepare_resume_rejects_session_sentinel_stem() -> anyhow::Result<()> {
    // Defense in depth: if no explicit id is passed and the log file is the
    // S3-restore sentinel (`session.jsonl`) with no parseable content, we must
    // NOT use the literal "session" string as a session id. The dispatcher
    // falls through to a fresh session.
    let work_dir = tempfile::tempdir()?;
    let log_path = work_dir.path().join("session.jsonl");
    std::fs::write(&log_path, "")?;

    let setup = super::prepare(
        work_dir.path(),
        "http://127.0.0.1:0",
        None,
        None,
        false,
        Some(&log_path),
        None,
    )?;

    assert_ne!(setup.session_id, "session", "must not use 'session' sentinel as id");
    assert!(
        !setup.extra_args.iter().any(|a| a == "--resume"),
        "must not pass --resume when no real session id is available"
    );
    Ok(())
}

#[test]
fn prepare_resume_parses_session_id_from_jsonl() -> anyhow::Result<()> {
    // No explicit resume_session_id, but the log file (named `session.jsonl`,
    // simulating an S3 restore) contains a real Claude `sessionId` in its
    // first JSONL entry. The dispatcher must parse it out and use it as the
    // resume id — replacing the removed file-stem prediction.
    let work_dir = tempfile::tempdir()?;
    let log_path = work_dir.path().join("session.jsonl");
    let real_uuid = "aaaa1111-bbbb-2222-cccc-333344445555";
    std::fs::write(
        &log_path,
        format!(
            r#"{{"sessionId":"{real_uuid}","type":"assistant","message":{{"role":"assistant","content":[{{"type":"text","text":"Hi"}}]}}}}
"#
        ),
    )?;

    let setup = super::prepare(
        work_dir.path(),
        "http://127.0.0.1:0",
        None,
        None,
        false,
        Some(&log_path),
        None,
    )?;

    assert_eq!(setup.session_id, real_uuid, "must parse sessionId from JSONL");
    let resume_idx = setup
        .extra_args
        .iter()
        .position(|a| a == "--resume")
        .ok_or_else(|| anyhow::anyhow!("no --resume arg"))?;
    assert_eq!(setup.extra_args[resume_idx + 1], real_uuid);
    Ok(())
}

#[test]
fn prepare_resume_no_local_state_starts_fresh() -> anyhow::Result<()> {
    // Acceptance criterion (ka-0000003077): when no resume hint is provided
    // and there is no local state, prepare() must start a fresh session
    // cleanly — no panics, no `--resume`, no spurious `--session-id`.
    let work_dir = tempfile::tempdir()?;

    let setup = super::prepare(
        work_dir.path(),
        "http://127.0.0.1:0",
        None,
        None,
        false,
        None, // no resume_log
        None, // no resume_session_id
    )?;

    assert!(
        !setup.extra_args.iter().any(|a| a == "--resume"),
        "fresh start must not pass --resume"
    );
    assert!(
        !setup.extra_args.iter().any(|a| a == "--session-id"),
        "fresh start must not pass --session-id (Claude v2.1+ treats it as --resume)"
    );
    // Coop's internal session id should be a non-empty UUID-ish value.
    assert!(!setup.session_id.is_empty());
    assert_ne!(setup.session_id, "session");
    Ok(())
}

#[test]
#[serial(claude_config_dir)]
fn write_credentials_creates_new_file() -> anyhow::Result<()> {
    let tmp = tempfile::tempdir()?;
    // Point CLAUDE_CONFIG_DIR to temp dir so we don't touch real config.
    std::env::set_var("CLAUDE_CONFIG_DIR", tmp.path());

    let path = write_credentials_file("sk-ant-test-token")?;
    assert!(path.exists());

    let content = std::fs::read_to_string(&path)?;
    let parsed: serde_json::Value = serde_json::from_str(&content)?;
    assert_eq!(parsed["claudeAiOauth"]["accessToken"], "sk-ant-test-token");
    assert_eq!(parsed["claudeAiOauth"]["refreshToken"], "");
    assert_eq!(parsed["claudeAiOauth"]["scopes"][0], "user:inference");

    std::env::remove_var("CLAUDE_CONFIG_DIR");
    Ok(())
}

#[test]
#[serial(claude_config_dir)]
fn write_credentials_preserves_existing_fields() -> anyhow::Result<()> {
    let tmp = tempfile::tempdir()?;
    std::env::set_var("CLAUDE_CONFIG_DIR", tmp.path());

    // Write an existing credentials file with extra fields.
    let existing = json!({
        "claudeAiOauth": {
            "accessToken": "old-token",
            "refreshToken": "rt-keep-this",
            "expiresAt": 1234567890000_i64,
            "scopes": ["user:inference", "user:sessions:claude_code"],
            "subscriptionType": "max",
            "rateLimitTier": "default_claude_max_20x"
        }
    });
    let cred_path = tmp.path().join(".credentials.json");
    std::fs::write(&cred_path, serde_json::to_string_pretty(&existing)?)?;

    // Write new token — should update accessToken but keep everything else.
    write_credentials_file("sk-ant-new-token")?;

    let content = std::fs::read_to_string(&cred_path)?;
    let parsed: serde_json::Value = serde_json::from_str(&content)?;
    assert_eq!(parsed["claudeAiOauth"]["accessToken"], "sk-ant-new-token");
    assert_eq!(parsed["claudeAiOauth"]["refreshToken"], "rt-keep-this");
    assert_eq!(parsed["claudeAiOauth"]["subscriptionType"], "max");
    assert_eq!(parsed["claudeAiOauth"]["scopes"][1], "user:sessions:claude_code");

    std::env::remove_var("CLAUDE_CONFIG_DIR");
    Ok(())
}

#[test]
#[serial(claude_config_dir)]
fn write_credentials_also_writes_xdg_state_path() -> anyhow::Result<()> {
    let config_tmp = tempfile::tempdir()?;
    let xdg_tmp = tempfile::tempdir()?;

    std::env::set_var("CLAUDE_CONFIG_DIR", config_tmp.path());
    std::env::set_var("XDG_STATE_HOME", xdg_tmp.path());

    write_credentials_file("sk-ant-dual-path-token")?;

    // Primary path (config dir) written.
    let primary = config_tmp.path().join(".credentials.json");
    assert!(primary.exists(), "primary config-dir credentials must exist");
    let primary_parsed: serde_json::Value =
        serde_json::from_str(&std::fs::read_to_string(&primary)?)?;
    assert_eq!(primary_parsed["claudeAiOauth"]["accessToken"], "sk-ant-dual-path-token");

    // XDG state path also written.
    let xdg = xdg_tmp.path().join("claude-code").join(".credentials.json");
    assert!(xdg.exists(), "XDG state credentials must also be written");
    let xdg_parsed: serde_json::Value = serde_json::from_str(&std::fs::read_to_string(&xdg)?)?;
    assert_eq!(xdg_parsed["claudeAiOauth"]["accessToken"], "sk-ant-dual-path-token");

    std::env::remove_var("CLAUDE_CONFIG_DIR");
    std::env::remove_var("XDG_STATE_HOME");
    Ok(())
}
