// SPDX-License-Identifier: BUSL-1.1
// Copyright (c) 2026 Matt Baker <fimbaz@gmail.com>

use super::{detect_startup_prompt, StartupPrompt};

#[test]
fn detect_workspace_trust_prompt() {
    let lines = vec![
        "".to_owned(),
        "  Welcome to Claude Code!".to_owned(),
        "".to_owned(),
        "  Do you trust the files in this folder?".to_owned(),
        "  (y/n)".to_owned(),
    ];
    assert_eq!(detect_startup_prompt(&lines), Some(StartupPrompt::WorkspaceTrust));
}

#[test]
fn detect_trust_this_workspace() {
    let lines = vec!["Do you trust this workspace? [y/N]".to_owned()];
    assert_eq!(detect_startup_prompt(&lines), Some(StartupPrompt::WorkspaceTrust));
}

#[test]
fn detect_bypass_permissions_prompt() {
    let lines = vec![
        "".to_owned(),
        "  --dangerously-skip-permissions detected".to_owned(),
        "  Allow tool use without prompting? (y/n)".to_owned(),
    ];
    assert_eq!(detect_startup_prompt(&lines), Some(StartupPrompt::BypassPermissions));
}

#[test]
fn detect_login_required() {
    let lines =
        vec!["Claude Code".to_owned(), "".to_owned(), "Please sign in to continue.".to_owned()];
    assert_eq!(detect_startup_prompt(&lines), Some(StartupPrompt::LoginRequired));
}

#[test]
fn no_prompt_on_empty_screen() {
    let lines: Vec<String> = vec![];
    assert_eq!(detect_startup_prompt(&lines), None);
}

#[test]
fn no_prompt_on_normal_output() {
    let lines = vec![
        "$ claude --model opus".to_owned(),
        "I'll help you with that task.".to_owned(),
        "Let me start by reading the file.".to_owned(),
    ];
    assert_eq!(detect_startup_prompt(&lines), None);
}

#[test]
fn detect_not_logged_in() {
    let lines =
        vec!["".to_owned(), "  Not logged in · Please run /login".to_owned(), "".to_owned()];
    assert_eq!(detect_startup_prompt(&lines), Some(StartupPrompt::LoginRequired));
}

#[test]
fn detect_not_logged_in_without_slash_login() {
    let lines = vec!["Not logged in".to_owned()];
    assert_eq!(detect_startup_prompt(&lines), Some(StartupPrompt::LoginRequired));
}

#[test]
fn detect_please_run_login() {
    let lines = vec!["Please run /login to authenticate".to_owned()];
    assert_eq!(detect_startup_prompt(&lines), Some(StartupPrompt::LoginRequired));
}

#[test]
fn no_prompt_on_bypass_status_bar() {
    // Regression: "bypass permissions on" in Claude's status bar should NOT
    // trigger the startup prompt detector (bd-qcn1).
    let lines = vec![
        "❯ ".to_owned(),
        "────────────────────────".to_owned(),
        "  ⏵⏵ bypass permissions on (shift+tab to cycle)".to_owned(),
        "".to_owned(),
    ];
    // The substring "bypass permissions" is present, but this is a status bar
    // indicator — NOT a startup prompt. The caller (classify_claude_screen)
    // should detect the idle prompt ❯ first and never reach this function,
    // but the detector itself still matches. This test documents the known
    // broad matching; the real fix is the priority reorder in
    // classify_claude_screen that checks idle before startup.
    //
    // If we later tighten the detector to reject status-bar text, this test
    // should be updated to assert None.
    assert_eq!(detect_startup_prompt(&lines), Some(StartupPrompt::BypassPermissions));
}
