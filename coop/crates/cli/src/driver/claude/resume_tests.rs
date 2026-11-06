// SPDX-License-Identifier: BUSL-1.1
// Copyright (c) 2026 Matt Baker <fimbaz@gmail.com>

use std::io::Write;

use super::{
    discover_session_log, parse_resume_state, parse_session_id, resume_args, SessionLogBaseline,
};
use crate::driver::AgentState;

#[test]
fn discover_returns_none_for_nonexistent() -> anyhow::Result<()> {
    let result = discover_session_log("/nonexistent/path")?;
    assert!(result.is_none());
    Ok(())
}

/// A .jsonl file with enough content is resumable — no companion directory needed.
/// Claude Code creates companion dirs lazily (only for subagents/tool-results),
/// so most sessions don't have one. `--resume` works from the .jsonl alone.
#[test]
fn discover_returns_direct_jsonl_without_companion_dir() -> anyhow::Result<()> {
    let dir = tempfile::tempdir()?;
    let log_path = dir.path().join("session.jsonl");
    // Write enough content to exceed the 256-byte minimum.
    let content = format!("{}\n", "x".repeat(300));
    std::fs::write(&log_path, content)?;

    let result = discover_session_log(log_path.to_str().ok_or(anyhow::anyhow!("path"))?)?;
    assert_eq!(result, Some(log_path));
    Ok(())
}

/// A tiny .jsonl (< 256 bytes) is likely just the init message — skip it.
#[test]
fn discover_skips_tiny_jsonl() -> anyhow::Result<()> {
    let dir = tempfile::tempdir()?;
    let log_path = dir.path().join("session.jsonl");
    std::fs::write(&log_path, "{}\n")?;

    let result = discover_session_log(log_path.to_str().ok_or(anyhow::anyhow!("path"))?)?;
    assert_eq!(result, None);
    Ok(())
}

#[test]
fn parse_empty_log_returns_starting() -> anyhow::Result<()> {
    let dir = tempfile::tempdir()?;
    let log_path = dir.path().join("session.jsonl");
    std::fs::write(&log_path, "")?;

    let state = parse_resume_state(&log_path)?;
    assert_eq!(state.last_state, AgentState::Starting);
    assert_eq!(state.log_offset, 0);
    Ok(())
}

#[test]
fn parse_log_with_assistant_message() -> anyhow::Result<()> {
    let dir = tempfile::tempdir()?;
    let log_path = dir.path().join("session.jsonl");
    let mut f = std::fs::File::create(&log_path)?;
    // Write a session entry with an assistant message (should yield Idle)
    writeln!(
        f,
        r#"{{"sessionId":"abc-123","type":"assistant","message":{{"role":"assistant","content":[{{"type":"text","text":"Hello"}}]}}}}"#
    )?;

    let state = parse_resume_state(&log_path)?;
    assert_eq!(state.last_state, AgentState::Idle);
    assert!(state.log_offset > 0);
    Ok(())
}

#[test]
fn resume_args_with_session_id() {
    let args = resume_args("sess-42");
    assert_eq!(args, vec!["--resume", "sess-42"]);
}

#[test]
fn parse_session_id_extracts_first_entry_session_id() -> anyhow::Result<()> {
    let dir = tempfile::tempdir()?;
    let log_path = dir.path().join("session.jsonl");
    let mut f = std::fs::File::create(&log_path)?;
    writeln!(
        f,
        r#"{{"sessionId":"abc-123","type":"assistant","message":{{"role":"assistant","content":[{{"type":"text","text":"Hi"}}]}}}}"#
    )?;
    writeln!(f, r#"{{"sessionId":"abc-123","type":"user","message":{{"role":"user"}}}}"#)?;

    assert_eq!(parse_session_id(&log_path), Some("abc-123".to_owned()));
    Ok(())
}

#[test]
fn parse_session_id_skips_entries_without_session_id() -> anyhow::Result<()> {
    let dir = tempfile::tempdir()?;
    let log_path = dir.path().join("session.jsonl");
    let mut f = std::fs::File::create(&log_path)?;
    writeln!(f, r#"{{"type":"meta"}}"#)?;
    writeln!(f, r#"{{"sessionId":"real-uuid","type":"assistant"}}"#)?;

    assert_eq!(parse_session_id(&log_path), Some("real-uuid".to_owned()));
    Ok(())
}

#[test]
fn parse_session_id_rejects_session_sentinel() -> anyhow::Result<()> {
    // Defense in depth: even if a corrupt log somehow contains
    // `"sessionId":"session"`, never accept it (ka-0000005939).
    let dir = tempfile::tempdir()?;
    let log_path = dir.path().join("session.jsonl");
    let mut f = std::fs::File::create(&log_path)?;
    writeln!(f, r#"{{"sessionId":"session","type":"assistant"}}"#)?;
    writeln!(f, r#"{{"sessionId":"real-uuid","type":"user"}}"#)?;

    assert_eq!(parse_session_id(&log_path), Some("real-uuid".to_owned()));
    Ok(())
}

#[test]
fn parse_session_id_returns_none_for_empty_or_malformed() -> anyhow::Result<()> {
    let dir = tempfile::tempdir()?;
    let empty = dir.path().join("empty.jsonl");
    std::fs::write(&empty, "")?;
    assert_eq!(parse_session_id(&empty), None);

    let junk = dir.path().join("junk.jsonl");
    std::fs::write(&junk, "not json\n{garbage\n")?;
    assert_eq!(parse_session_id(&junk), None);

    let only_sentinel = dir.path().join("sentinel.jsonl");
    std::fs::write(&only_sentinel, r#"{"sessionId":"session"}"#)?;
    assert_eq!(parse_session_id(&only_sentinel), None);

    Ok(())
}

#[test]
fn parse_session_id_returns_none_for_missing_file() {
    let result = parse_session_id(std::path::Path::new("/nonexistent/file.jsonl"));
    assert_eq!(result, None);
}

#[test]
fn discover_respects_claude_config_dir() -> anyhow::Result<()> {
    let root = tempfile::tempdir()?;
    let config_dir = root.path().join("custom-claude");
    let project_dir = config_dir.join("projects").join("-fake-project");
    std::fs::create_dir_all(&project_dir)?;
    let log = project_dir.join("abc-123.jsonl");
    // Write enough content to be considered resumable.
    let content = format!("{}\n", "x".repeat(300));
    std::fs::write(&log, content)?;

    // Point CLAUDE_CONFIG_DIR at our temp dir.
    std::env::set_var("CLAUDE_CONFIG_DIR", &config_dir);
    let result = discover_session_log("-fake-project");
    std::env::remove_var("CLAUDE_CONFIG_DIR");

    assert_eq!(result?, Some(log));
    Ok(())
}

/// A temp projects dir plus the project directory Claude would use for a
/// temp working dir, so baseline tests never touch `CLAUDE_CONFIG_DIR`.
struct ProjectFixture {
    _root: tempfile::TempDir,
    projects_dir: std::path::PathBuf,
    working_dir: std::path::PathBuf,
    project_dir: std::path::PathBuf,
}

fn project_fixture() -> anyhow::Result<ProjectFixture> {
    let root = tempfile::tempdir()?;
    let projects_dir = root.path().join("projects");
    let working_dir = root.path().join("work");
    std::fs::create_dir_all(&working_dir)?;
    let project_dir = projects_dir.join(super::project_dir_name(&working_dir));
    std::fs::create_dir_all(&project_dir)?;
    Ok(ProjectFixture { _root: root, projects_dir, working_dir, project_dir })
}

fn write_log(path: &std::path::Path, bytes: usize) -> anyhow::Result<()> {
    std::fs::write(path, format!("{}\n", "x".repeat(bytes)))?;
    Ok(())
}

#[test]
fn baseline_ignores_logs_from_earlier_incarnations() -> anyhow::Result<()> {
    let f = project_fixture()?;
    // The previous incarnation's transcript: the newest, resumable file on
    // disk, which plain discovery would latch (gd-2g0LXWF4Sz).
    write_log(&f.project_dir.join("previous.jsonl"), 300)?;

    let baseline = SessionLogBaseline::capture_in(f.projects_dir.clone(), &f.working_dir);
    assert_eq!(baseline.len(), 1);
    assert_eq!(baseline.find_new_log(), None);
    Ok(())
}

#[test]
fn baseline_finds_log_created_after_capture_from_offset_zero() -> anyhow::Result<()> {
    let f = project_fixture()?;
    write_log(&f.project_dir.join("previous.jsonl"), 300)?;
    let baseline = SessionLogBaseline::capture_in(f.projects_dir.clone(), &f.working_dir);

    let live = f.project_dir.join("live.jsonl");
    write_log(&live, 10)?;
    assert_eq!(baseline.find_new_log(), None, "a new log under 256 bytes is not yet accepted");

    write_log(&live, 300)?;
    assert_eq!(baseline.find_new_log(), Some((live, 0)));
    Ok(())
}

#[test]
fn baseline_reads_grown_preexisting_log_from_captured_size() -> anyhow::Result<()> {
    let f = project_fixture()?;
    let previous = f.project_dir.join("previous.jsonl");
    write_log(&previous, 300)?;
    let baseline = SessionLogBaseline::capture_in(f.projects_dir.clone(), &f.working_dir);

    let mut file = std::fs::OpenOptions::new().append(true).open(&previous)?;
    writeln!(file, "{{}}")?;
    assert_eq!(baseline.find_new_log(), Some((previous, 301)));
    Ok(())
}

#[test]
fn baseline_works_when_project_dir_appears_after_capture() -> anyhow::Result<()> {
    let f = project_fixture()?;
    std::fs::remove_dir_all(&f.project_dir)?;
    // First start in a fresh pod: Claude creates the project dir lazily.
    let baseline = SessionLogBaseline::capture_in(f.projects_dir.clone(), &f.working_dir);
    assert!(baseline.is_empty());

    std::fs::create_dir_all(&f.project_dir)?;
    let live = f.project_dir.join("live.jsonl");
    write_log(&live, 300)?;
    assert_eq!(baseline.find_new_log(), Some((live, 0)));
    Ok(())
}

#[test]
fn baseline_ignores_logs_in_an_ancestor_project_dir() -> anyhow::Result<()> {
    let f = project_fixture()?;
    // The owner's own Claude in an ancestor dir (`-home-me` for
    // `/home/me/ws/wt`), still writing after capture (gd-4tAWXFTMVL).
    let parent = f.working_dir.parent().ok_or_else(|| anyhow::anyhow!("no parent"))?;
    let ancestor_dir = f.projects_dir.join(super::project_dir_name(parent));
    std::fs::create_dir_all(&ancestor_dir)?;
    let owner = ancestor_dir.join("owner.jsonl");
    write_log(&owner, 300)?;
    let baseline = SessionLogBaseline::capture_in(f.projects_dir.clone(), &f.working_dir);
    assert!(baseline.is_empty());

    let mut file = std::fs::OpenOptions::new().append(true).open(&owner)?;
    writeln!(file, "{{}}")?;
    write_log(&ancestor_dir.join("other.jsonl"), 300)?;
    assert_eq!(baseline.find_new_log(), None);
    Ok(())
}

#[test]
fn baseline_offset_of_is_captured_size_or_zero() -> anyhow::Result<()> {
    let f = project_fixture()?;
    let previous = f.project_dir.join("previous.jsonl");
    write_log(&previous, 300)?;
    let baseline = SessionLogBaseline::capture_in(f.projects_dir.clone(), &f.working_dir);

    assert_eq!(baseline.offset_of(&previous), 301);
    assert_eq!(baseline.offset_of(&f.project_dir.join("live.jsonl")), 0);
    Ok(())
}
