// SPDX-License-Identifier: BUSL-1.1
// Copyright (c) 2026 Matt Baker <fimbaz@gmail.com>

use std::collections::HashMap;
use std::os::unix::fs::PermissionsExt;

use super::*;

#[test]
fn codex_home_prefers_env_then_dot_codex() {
    let env = |k: &str| match k {
        "CODEX_HOME" => Some("/srv/codex".to_owned()),
        "HOME" => Some("/home/agent".to_owned()),
        _ => None,
    };
    assert_eq!(codex_home_from(env), PathBuf::from("/srv/codex"));
    let env = |k: &str| (k == "HOME").then(|| "/home/agent".to_owned());
    assert_eq!(codex_home_from(env), PathBuf::from("/home/agent/.codex"));
    let env = |k: &str| match k {
        "CODEX_HOME" => Some(String::new()),
        "HOME" => Some("/h".to_owned()),
        _ => None,
    };
    assert_eq!(codex_home_from(env), PathBuf::from("/h/.codex"));
}

#[test]
fn write_auth_json_is_0600_and_replaces_atomically() {
    let dir = tempfile::tempdir().unwrap();
    let home = dir.path().join("codex");
    write_auth_json(&home, r#"{"v":1}"#).unwrap();
    write_auth_json(&home, r#"{"v":2}"#).unwrap();

    let path = home.join("auth.json");
    assert_eq!(std::fs::read_to_string(&path).unwrap(), r#"{"v":2}"#);
    assert_eq!(std::fs::metadata(&path).unwrap().permissions().mode() & 0o777, 0o600);
    let leftovers: Vec<_> = std::fs::read_dir(&home)
        .unwrap()
        .filter_map(|e| e.ok())
        .filter(|e| e.file_name() != "auth.json")
        .collect();
    assert!(leftovers.is_empty(), "temp file left behind: {leftovers:?}");
}

#[test]
fn write_auth_json_tightens_a_pre_existing_loose_file() {
    let dir = tempfile::tempdir().unwrap();
    let path = dir.path().join("auth.json");
    std::fs::write(&path, "old").unwrap();
    std::fs::set_permissions(&path, std::fs::Permissions::from_mode(0o644)).unwrap();
    write_auth_json(dir.path(), "new").unwrap();
    assert_eq!(std::fs::metadata(&path).unwrap().permissions().mode() & 0o777, 0o600);
}

#[test]
fn apply_credentials_ignores_maps_without_a_document() {
    assert!(!apply_credentials(&HashMap::new()));
    assert!(!apply_credentials(&HashMap::from([(AUTH_ENV.to_owned(), String::new())])));
    assert!(!apply_credentials(&HashMap::from([("CLAUDE_CODE_OAUTH_TOKEN".into(), "x".into())])));
}

#[test]
fn thread_id_from_rollout_parses_the_trailing_uuid() {
    let p = Path::new(
        "/h/.codex/sessions/2026/10/15/rollout-2026-10-15T01-02-03-0199a1b2-c3d4-7e5f-8a9b-0c1d2e3f4a5b.jsonl",
    );
    assert_eq!(thread_id_from_rollout(p).as_deref(), Some("0199a1b2-c3d4-7e5f-8a9b-0c1d2e3f4a5b"));
    assert_eq!(thread_id_from_rollout(Path::new("/x/session.jsonl")), None);
    assert_eq!(thread_id_from_rollout(Path::new("/x/rollout-short.jsonl")), None);
    assert_eq!(
        thread_id_from_rollout(Path::new(
            "/x/rollout-2026-zzzzzzzz-zzzz-zzzz-zzzz-zzzzzzzzzzzz.jsonl"
        )),
        None
    );
}
