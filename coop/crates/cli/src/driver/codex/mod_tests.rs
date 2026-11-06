// SPDX-License-Identifier: BUSL-1.1
// Copyright (c) 2026 Matt Baker <fimbaz@gmail.com>

use super::CodexMode;

fn mode(cmd: &[&str]) -> CodexMode {
    let cmd: Vec<String> = cmd.iter().map(|s| (*s).to_owned()).collect();
    CodexMode::from_command(&cmd)
}

#[test]
fn app_server_command_selects_app_server() {
    assert_eq!(mode(&["codex", "app-server"]), CodexMode::AppServer);
    assert_eq!(
        mode(&["/usr/local/bin/codex", "app-server", "--strict-config"]),
        CodexMode::AppServer
    );
}

#[test]
fn exec_command_selects_exec() {
    assert_eq!(mode(&["codex", "exec", "--yolo"]), CodexMode::Exec);
    assert_eq!(mode(&["codex", "e"]), CodexMode::Exec);
}

#[test]
fn value_flags_do_not_hide_the_subcommand() {
    assert_eq!(mode(&["codex", "-c", "model=\"o3\"", "app-server"]), CodexMode::AppServer);
    assert_eq!(mode(&["codex", "--model", "x", "--yolo", "exec"]), CodexMode::Exec);
}

#[test]
fn interactive_invocations_are_tui() {
    assert_eq!(mode(&["codex"]), CodexMode::Tui);
    assert_eq!(mode(&["codex", "--yolo"]), CodexMode::Tui);
    assert_eq!(mode(&["codex", "fix the build"]), CodexMode::Tui);
    assert_eq!(mode(&["codex", "resume", "abc"]), CodexMode::Tui);
    assert_eq!(mode(&[]), CodexMode::Tui);
}

#[test]
fn only_app_server_and_exec_are_headless() {
    assert!(CodexMode::AppServer.is_headless());
    assert!(CodexMode::Exec.is_headless());
    assert!(!CodexMode::Tui.is_headless());
}
