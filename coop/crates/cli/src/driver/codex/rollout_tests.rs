// SPDX-License-Identifier: BUSL-1.1
// Copyright (c) 2026 Matt Baker <fimbaz@gmail.com>

use std::path::{Path, PathBuf};

use super::*;

const FIXTURE: &str = include_str!("../../../../../tests/fixtures/codex/rollout-sample.jsonl");
const FIXTURE_ID: &str = "01a13cb8-970e-74a2-b81f-f40294555aa8";
const FIXTURE_NAME: &str = "rollout-2026-10-14T23-21-32-01a13cb8-970e-74a2-b81f-f40294555aa8.jsonl";

fn write_fixture(dir: &Path) -> PathBuf {
    let path = dir.join("session.jsonl");
    std::fs::write(&path, FIXTURE).unwrap();
    path
}

#[test]
fn parses_thread_id_and_start_from_session_meta() {
    let dir = tempfile::tempdir().unwrap();
    let meta = parse_rollout_meta(&write_fixture(dir.path())).unwrap();
    assert_eq!(meta.thread_id, FIXTURE_ID);
    assert_eq!(meta.started, "2026-10-14T23:21:32");
}

#[test]
fn rejects_files_that_are_not_rollouts() {
    let dir = tempfile::tempdir().unwrap();
    let p = dir.path().join("x.jsonl");
    for body in [
        "",
        "not json\n",
        r#"{"type":"event_msg","payload":{}}"#,
        // A path-traversal id must never reach the filesystem.
        r#"{"type":"session_meta","payload":{"id":"../../etc","timestamp":"2026-10-14T23:21:32Z"}}"#,
        r#"{"type":"session_meta","payload":{"id":"01a13cb8-970e-74a2-b81f-f40294555aa8","timestamp":"../../../../x/y/zzzz"}}"#,
    ] {
        std::fs::write(&p, body).unwrap();
        assert_eq!(parse_rollout_meta(&p), None, "{body}");
    }
    assert_eq!(parse_rollout_meta(&dir.path().join("missing")), None);
}

#[test]
fn rollout_path_matches_codex_layout() {
    let meta = RolloutMeta { thread_id: FIXTURE_ID.into(), started: "2026-10-14T23:21:32".into() };
    assert_eq!(
        rollout_path(Path::new("/h"), &meta),
        Path::new("/h/sessions/2026/10/14").join(FIXTURE_NAME)
    );
}

#[test]
fn place_restores_into_a_fresh_codex_home_and_find_locates_it() {
    let src_dir = tempfile::tempdir().unwrap();
    let home = tempfile::tempdir().unwrap();
    let src = write_fixture(src_dir.path());

    assert_eq!(find_rollout(home.path(), FIXTURE_ID), None);
    let id = place_restored_rollout(&src, home.path()).unwrap();
    assert_eq!(id.as_deref(), Some(FIXTURE_ID));

    let dest = home.path().join("sessions/2026/10/14").join(FIXTURE_NAME);
    assert_eq!(std::fs::read_to_string(&dest).unwrap(), FIXTURE);
    assert_eq!(find_rollout(home.path(), FIXTURE_ID), Some(dest));
}

#[test]
fn place_keeps_a_fresher_local_rollout_and_replaces_a_staler_one() {
    let src_dir = tempfile::tempdir().unwrap();
    let home = tempfile::tempdir().unwrap();
    let src = write_fixture(src_dir.path());
    let dest = home.path().join("sessions/2026/10/14").join(FIXTURE_NAME);
    std::fs::create_dir_all(dest.parent().unwrap()).unwrap();

    let fresher = format!("{FIXTURE}{{\"type\":\"event_msg\"}}\n");
    std::fs::write(&dest, &fresher).unwrap();
    place_restored_rollout(&src, home.path()).unwrap();
    assert_eq!(std::fs::read_to_string(&dest).unwrap(), fresher);

    std::fs::write(&dest, "stale").unwrap();
    place_restored_rollout(&src, home.path()).unwrap();
    assert_eq!(std::fs::read_to_string(&dest).unwrap(), FIXTURE);
}

#[test]
fn place_ignores_a_non_rollout() {
    let dir = tempfile::tempdir().unwrap();
    let home = tempfile::tempdir().unwrap();
    let src = dir.path().join("session.jsonl");
    std::fs::write(&src, "{\"type\":\"user\"}\n").unwrap();
    assert_eq!(place_restored_rollout(&src, home.path()).unwrap(), None);
    assert!(!home.path().join("sessions").exists());
}

#[test]
fn retire_hides_the_rollout_from_id_lookup() {
    let src_dir = tempfile::tempdir().unwrap();
    let home = tempfile::tempdir().unwrap();
    place_restored_rollout(&write_fixture(src_dir.path()), home.path()).unwrap();
    let live = find_rollout(home.path(), FIXTURE_ID).unwrap();

    let stale = retire_rollout(&live).unwrap();
    assert!(stale.to_string_lossy().ends_with(".jsonl.stale"));
    assert_eq!(find_rollout(home.path(), FIXTURE_ID), None);
    // Idempotent: nothing left to retire.
    assert_eq!(retire_rollout(&live), None);
}
