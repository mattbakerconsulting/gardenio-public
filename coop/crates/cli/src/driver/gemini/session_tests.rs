// SPDX-License-Identifier: BUSL-1.1
// Copyright (c) 2026 Matt Baker <fimbaz@gmail.com>

use std::path::{Path, PathBuf};

use super::*;

/// The record the phase 0 spike resumed from in an otherwise empty HOME.
const FIXTURE: &str =
    include_str!("../../../../../tests/fixtures/gemini/session/session-record-tool.jsonl");
const FIXTURE_ID: &str = "f28f39ef-f45b-41ef-add8-904e85e59b7b";
const FIXTURE_NAME: &str = "session-2026-10-16T07-09-f28f39ef.jsonl";

fn write_fixture(dir: &Path) -> anyhow::Result<PathBuf> {
    let path = dir.join("session.jsonl");
    std::fs::write(&path, FIXTURE)?;
    Ok(path)
}

#[test]
fn parses_session_id_and_start_minute_from_the_header() -> anyhow::Result<()> {
    let dir = tempfile::tempdir()?;
    let meta = parse_session_meta(&write_fixture(dir.path())?);
    assert_eq!(
        meta,
        Some(SessionMeta { session_id: FIXTURE_ID.into(), started: "2026-10-16T07-09".into() })
    );
    Ok(())
}

#[test]
fn rejects_files_that_are_not_session_records() -> anyhow::Result<()> {
    let dir = tempfile::tempdir()?;
    let p = dir.path().join("x.jsonl");
    for body in [
        "",
        "not json\n",
        r#"{"id":"1","type":"user"}"#,
        // A path-traversal id or timestamp must never reach the filesystem.
        r#"{"sessionId":"../../etc","startTime":"2026-10-16T07:09:08.582Z"}"#,
        r#"{"sessionId":"f28f39ef-f45b-41ef-add8-904e85e59b7b","startTime":"../../../../x/y"}"#,
    ] {
        std::fs::write(&p, body)?;
        assert_eq!(parse_session_meta(&p), None, "{body}");
    }
    assert_eq!(parse_session_meta(&dir.path().join("missing")), None);
    Ok(())
}

#[test]
fn session_path_matches_the_cli_layout() {
    let meta = SessionMeta { session_id: FIXTURE_ID.into(), started: "2026-10-16T07-09".into() };
    assert_eq!(
        session_path(Path::new("/h"), "work", &meta),
        Path::new("/h/tmp/work/chats").join(FIXTURE_NAME)
    );
}

#[test]
fn project_slug_is_the_workspace_basename() {
    assert_eq!(project_slug(Path::new("/home/agent/bot/gardenio/work")), "work");
    assert_eq!(project_slug(Path::new("/")), "root");
}

#[test]
fn restore_places_the_record_where_gemini_looks_and_finds_it_again() -> anyhow::Result<()> {
    let dir = tempfile::tempdir()?;
    let src = write_fixture(dir.path())?;
    let home = dir.path().join("home/.gemini");

    assert_eq!(find_session(&home, FIXTURE_ID), None);
    let id = place_restored_session(&src, &home, "work")?;
    assert_eq!(id.as_deref(), Some(FIXTURE_ID));

    let placed = home.join("tmp/work/chats").join(FIXTURE_NAME);
    assert_eq!(std::fs::read_to_string(&placed)?, FIXTURE);
    assert_eq!(find_session(&home, FIXTURE_ID), Some(placed));
    Ok(())
}

#[test]
fn restore_keeps_a_fresher_local_record() -> anyhow::Result<()> {
    let dir = tempfile::tempdir()?;
    let src = write_fixture(dir.path())?;
    let home = dir.path().join("home/.gemini");
    place_restored_session(&src, &home, "work")?;
    let local = home.join("tmp/work/chats").join(FIXTURE_NAME);
    let grown = format!("{FIXTURE}{{\"$set\":{{\"lastUpdated\":\"2026-10-16T08:00:00.000Z\"}}}}\n");
    std::fs::write(&local, &grown)?;

    place_restored_session(&src, &home, "work")?;
    assert_eq!(std::fs::read_to_string(&local)?, grown);
    Ok(())
}

#[test]
fn restore_refuses_a_file_that_is_not_a_session() -> anyhow::Result<()> {
    let dir = tempfile::tempdir()?;
    let src = dir.path().join("session.jsonl");
    std::fs::write(&src, "{\"type\":\"session_meta\"}\n")?;
    let home = dir.path().join("home/.gemini");
    assert_eq!(place_restored_session(&src, &home, "work")?, None);
    assert!(!home.exists());
    Ok(())
}

#[test]
fn find_session_checks_the_header_not_just_the_file_name() -> anyhow::Result<()> {
    let dir = tempfile::tempdir()?;
    let chats = dir.path().join("tmp/work/chats");
    std::fs::create_dir_all(&chats)?;
    // Same id8 as the fixture, different session.
    let other = FIXTURE.replace(FIXTURE_ID, "f28f39ef-0000-4000-8000-000000000000");
    std::fs::write(chats.join(FIXTURE_NAME), other)?;
    assert_eq!(find_session(dir.path(), FIXTURE_ID), None);
    Ok(())
}

#[test]
fn retire_renames_the_record_out_of_the_resume_set() -> anyhow::Result<()> {
    let dir = tempfile::tempdir()?;
    let record = write_fixture(dir.path())?;
    let stale = retire_session(&record);
    assert_eq!(stale, Some(dir.path().join("session.jsonl.stale")));
    assert!(!record.exists());
    // Idempotent.
    assert_eq!(retire_session(&record), None);
    Ok(())
}

#[test]
fn the_session_id_of_a_record_is_its_resume_handle() -> anyhow::Result<()> {
    let dir = tempfile::tempdir()?;
    let record = write_fixture(dir.path())?;
    assert_eq!(session_id_from_record(&record).as_deref(), Some(FIXTURE_ID));
    Ok(())
}
