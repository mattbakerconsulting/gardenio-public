// SPDX-License-Identifier: BUSL-1.1
// Copyright (c) 2026 Matt Baker <fimbaz@gmail.com>

use std::fs;

use serial_test::serial;
use tempfile::TempDir;

use super::*;

fn write_log(dir: &std::path::Path, name: &str) -> anyhow::Result<std::path::PathBuf> {
    fs::create_dir_all(dir)?;
    let p = dir.join(name);
    fs::write(&p, "{}\n")?;
    Ok(p)
}

#[test]
#[serial(claude_config_dir)]
fn retires_jsonl_and_companion_and_session_env() -> anyhow::Result<()> {
    let tmp = TempDir::new()?;
    std::env::set_var("CLAUDE_CONFIG_DIR", tmp.path());

    let uuid = "abc123";
    let projects = tmp.path().join("projects").join("-home-agent-workspace");
    let log = write_log(&projects, &format!("{uuid}.jsonl"))?;
    let companion = projects.join(uuid);
    fs::create_dir_all(&companion)?;
    let env_dir = tmp.path().join("session-env").join(uuid);
    fs::create_dir_all(&env_dir)?;

    let out = retire_session_log(&log);

    assert!(out.any(), "expected at least one retirement");
    assert!(out.log_retired.is_some());
    assert!(out.companion_retired.is_some());
    assert!(out.session_env_retired.is_some());

    assert!(!log.exists(), "original log should be gone");
    assert!(projects.join(format!("{uuid}.jsonl.stale")).exists());
    assert!(projects.join(format!("{uuid}.stale")).is_dir());
    assert!(tmp.path().join("session-env").join(format!("{uuid}.stale")).is_dir());

    std::env::remove_var("CLAUDE_CONFIG_DIR");
    Ok(())
}

#[test]
#[serial(claude_config_dir)]
fn idempotent_when_paths_missing() -> anyhow::Result<()> {
    let tmp = TempDir::new()?;
    std::env::set_var("CLAUDE_CONFIG_DIR", tmp.path());

    let log = tmp.path().join("projects").join("-x").join("nope.jsonl");
    let out = retire_session_log(&log);

    assert!(!out.any());
    assert!(out.log_retired.is_none());
    assert!(out.companion_retired.is_none());
    assert!(out.session_env_retired.is_none());

    std::env::remove_var("CLAUDE_CONFIG_DIR");
    Ok(())
}

#[test]
#[serial(claude_config_dir)]
fn retires_only_log_when_companion_and_env_missing() -> anyhow::Result<()> {
    let tmp = TempDir::new()?;
    std::env::set_var("CLAUDE_CONFIG_DIR", tmp.path());

    let projects = tmp.path().join("projects").join("-home-agent-workspace");
    let log = write_log(&projects, "lone.jsonl")?;

    let out = retire_session_log(&log);

    assert!(out.log_retired.is_some());
    assert!(out.companion_retired.is_none());
    assert!(out.session_env_retired.is_none());

    std::env::remove_var("CLAUDE_CONFIG_DIR");
    Ok(())
}

#[test]
fn empty_path_is_noop() {
    let out = retire_session_log(std::path::Path::new(""));
    assert!(!out.any());
}
