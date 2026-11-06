// SPDX-License-Identifier: BUSL-1.1
// Copyright (c) 2026 Matt Baker <fimbaz@gmail.com>

use crate::driver::{AgentState, PromptKind};
use crate::screen::{CursorPosition, ScreenTextSnapshot};

use super::classify_claude_screen;

fn snapshot(lines: &[&str]) -> ScreenTextSnapshot {
    ScreenTextSnapshot {
        lines: lines.iter().map(|s| s.to_string()).collect(),
        cols: 80,
        rows: 24,
        alt_screen: false,
        cursor: CursorPosition { row: 0, col: 0 },
        sequence: 1,
    }
}

/// Extract just the state from the classify result for simple assertions.
fn state(snap: &ScreenTextSnapshot) -> Option<AgentState> {
    classify_claude_screen(snap).map(|(s, _)| s)
}

/// Extract the cause string from the classify result.
fn cause(snap: &ScreenTextSnapshot) -> Option<String> {
    classify_claude_screen(snap).map(|(_, c)| c)
}

#[test]
fn detects_idle_prompt() {
    let snap = snapshot(&["Claude Code v2.1.37", "", "\u{276f} Try \"fix lint errors\"", ""]);
    assert_eq!(state(&snap), Some(AgentState::Idle));
    assert_eq!(cause(&snap).as_deref(), Some("screen:idle"));
}

#[test]
fn no_idle_on_empty_screen() {
    let snap = snapshot(&["", "", ""]);
    assert_eq!(classify_claude_screen(&snap), None);
}

#[test]
fn no_idle_on_working_output() {
    let snap = snapshot(&["Reading file src/main.rs...", "Analyzing code...", ""]);
    assert_eq!(classify_claude_screen(&snap), None);
}

#[test]
fn thinking_ellipsis_emits_working() {
    let snap = snapshot(&["Some output", "\u{2026} thinking", ""]);
    assert_eq!(state(&snap), Some(AgentState::Working));
    assert_eq!(cause(&snap).as_deref(), Some("screen:thinking"));
}

#[test]
fn thought_for_ellipsis_emits_working() {
    let snap =
        snapshot(&["\u{2026} (2m 19s \u{00b7} \u{2191} 4.7k tokens \u{00b7} thought for 9s)", ""]);
    assert_eq!(state(&snap), Some(AgentState::Working));
    assert_eq!(cause(&snap).as_deref(), Some("screen:thinking"));
}

#[test]
fn startup_trust_prompt_emits_setup() {
    let snap = snapshot(&["Do you trust the files in this folder?", "(y/n)", ""]);
    let (s, c) = classify_claude_screen(&snap).expect("should emit state");
    let prompt = s.prompt().expect("should be Prompt");
    assert_eq!(prompt.kind, PromptKind::Setup);
    assert_eq!(prompt.subtype.as_deref(), Some("startup_trust"));
    assert!(prompt.options.is_empty(), "text prompts have no parsed options");
    assert_eq!(c, "screen:setup");
}

#[test]
fn startup_bypass_prompt_emits_setup() {
    let snap = snapshot(&["Allow tool use without prompting?", "dangerously-skip-permissions", ""]);
    let (s, c) = classify_claude_screen(&snap).expect("should emit state");
    let prompt = s.prompt().expect("should be Prompt");
    assert_eq!(prompt.kind, PromptKind::Setup);
    assert_eq!(prompt.subtype.as_deref(), Some("startup_bypass"));
    assert_eq!(c, "screen:setup");
}

#[test]
fn startup_login_prompt_emits_setup() {
    let snap = snapshot(&["Please sign in to continue", ""]);
    let (s, c) = classify_claude_screen(&snap).expect("should emit state");
    let prompt = s.prompt().expect("should be Prompt");
    assert_eq!(prompt.kind, PromptKind::Setup);
    assert_eq!(prompt.subtype.as_deref(), Some("startup_login"));
    assert_eq!(c, "screen:setup");
}

#[test]
fn settings_error_emits_setup() {
    let lines: Vec<String> =
        include_str!("fixtures/bad_settings.screen.txt").lines().map(String::from).collect();
    let snap = ScreenTextSnapshot {
        lines,
        cols: 200,
        rows: 50,
        alt_screen: false,
        cursor: CursorPosition { row: 0, col: 0 },
        sequence: 1,
    };
    let (s, c) = classify_claude_screen(&snap).expect("should emit state");
    let prompt = s.prompt().expect("should be Prompt");
    assert_eq!(prompt.kind, PromptKind::Setup);
    assert_eq!(prompt.subtype.as_deref(), Some("settings_error"));
    assert!(!prompt.options.is_empty(), "should parse numbered options");
    assert_eq!(c, "screen:setup");
}

#[test]
fn detects_bare_prompt() {
    let snap = snapshot(&["\u{276f} ", ""]);
    assert_eq!(state(&snap), Some(AgentState::Idle));
}

#[test]
fn workspace_trust_dialog_emits_permission() {
    let snap = snapshot(&[
        " Accessing workspace:",
        " /Users/kestred/Developer/foo",
        "",
        " \u{276f} 1. Yes, I trust this folder",
        "   2. No, exit",
        "",
        " Enter to confirm \u{00b7} Esc to cancel",
    ]);
    let (s, c) = classify_claude_screen(&snap).expect("should emit state");
    let prompt = s.prompt().expect("should be Prompt");
    assert_eq!(prompt.kind, PromptKind::Permission);
    assert_eq!(prompt.subtype.as_deref(), Some("trust"));
    assert!(!prompt.options.is_empty(), "should parse options");
    assert_eq!(c, "screen:permission");
}

#[test]
fn theme_picker_emits_setup() {
    let snap = snapshot(&[
        " Choose the text style that looks best with your terminal",
        "",
        " \u{276f} 1. Dark mode \u{2714}",
        "   2. Light mode",
        "",
        " Enter to confirm",
    ]);
    let (s, c) = classify_claude_screen(&snap).expect("should emit state");
    let prompt = s.prompt().expect("should be Prompt");
    assert_eq!(prompt.kind, PromptKind::Setup);
    assert_eq!(prompt.subtype.as_deref(), Some("theme_picker"));
    assert!(!prompt.options.is_empty(), "should parse options");
    assert_eq!(c, "screen:setup");
}

#[test]
fn tool_permission_dialog_still_suppressed() {
    let snap = snapshot(&[
        " Bash command",
        "",
        "   coop send '{\"status\":\"done\",\"message\":\"Said goodbye as requested.\"}'",
        "   Signal done to coop stop hook",
        "",
        " Do you want to proceed?",
        " \u{276f} 1. Yes",
        "   2. Yes, and don't ask again for coop send commands in /Users/kestred/Developer/coop",
        "   3. No",
        "",
        " Esc to cancel \u{00b7} Tab to amend \u{00b7} ctrl+e to explain",
    ]);
    assert_eq!(classify_claude_screen(&snap), None);
}

#[test]
fn rm_circuit_breaker_dialog_emits_permission() {
    // Same shape as a tool-permission dialog, but the command is an
    // `rm -rf /home/agent` (home dir itself) — the bypass-immune circuit
    // breaker. It must be surfaced (not suppressed) so groom can decline it.
    let lines = fixture_lines(include_str!("fixtures/rm_circuit_breaker.screen.txt"));
    let snap = ScreenTextSnapshot {
        lines,
        cols: 80,
        rows: 24,
        alt_screen: false,
        cursor: CursorPosition { row: 0, col: 0 },
        sequence: 1,
    };
    let (s, c) = classify_claude_screen(&snap).expect("should emit state");
    let prompt = s.prompt().expect("should be Prompt");
    assert_eq!(prompt.kind, PromptKind::Permission);
    assert_eq!(prompt.subtype.as_deref(), Some("rm_circuit_breaker"));
    assert_eq!(c, "screen:permission");
}

#[test]
fn rm_circuit_breaker_variants_emit_permission() {
    // Each of these targets home/root *itself* and must be surfaced.
    for cmd in [
        "   rm -rf /",
        "   rm -rf ~",
        "   rm -rf $HOME",
        "   rm -rf \"$HOME\"",
        "   rm -rf ${HOME}/",
        "   rm -rf /home/agent",
        "   rm -rf /home/agent/",
        "   rmdir /root",
        "   rm -rf /Users/kestred",
    ] {
        let snap = snapshot(&[
            " Bash command",
            "",
            cmd,
            "   dangerous removal",
            "",
            " Do you want to proceed?",
            " \u{276f} 1. Yes",
            "   2. No",
            "",
            " Esc to cancel \u{00b7} Tab to amend",
        ]);
        let s = state(&snap).unwrap_or_else(|| panic!("{cmd:?} should emit a prompt"));
        let prompt = s.prompt().unwrap_or_else(|| panic!("{cmd:?} should be Prompt"));
        assert_eq!(
            prompt.subtype.as_deref(),
            Some("rm_circuit_breaker"),
            "{cmd:?} should classify as rm_circuit_breaker"
        );
    }
}

#[test]
fn rm_of_subpath_stays_suppressed() {
    // An `rm` NOT targeting home/root itself is an ordinary tool-permission
    // dialog (handled via hooks) and must remain suppressed — the circuit
    // breaker only fires on the home/root directory itself.
    for cmd in [
        "   rm -rf /home/agent/build",
        "   rm -rf ./dist",
        "   rm -rf /tmp/scratch",
        "   rm file.txt",
        "   rm -rf /var/lib/apt/lists/*",
    ] {
        let snap = snapshot(&[
            " Bash command",
            "",
            cmd,
            "   clean up",
            "",
            " Do you want to proceed?",
            " \u{276f} 1. Yes",
            "   2. No",
            "",
            " Esc to cancel \u{00b7} Tab to amend",
        ]);
        assert_eq!(classify_claude_screen(&snap), None, "{cmd:?} should stay suppressed");
    }
}

#[test]
fn edit_permission_dialog_emits_permission() {
    let lines = fixture_lines(include_str!("fixtures/edit_permission.screen.txt"));
    let snap = ScreenTextSnapshot {
        lines,
        cols: 80,
        rows: 24,
        alt_screen: false,
        cursor: CursorPosition { row: 0, col: 0 },
        sequence: 1,
    };
    let (s, c) = classify_claude_screen(&snap).expect("should emit state");
    let prompt = s.prompt().expect("should be Prompt");
    assert_eq!(prompt.kind, PromptKind::Permission);
    assert_eq!(prompt.subtype.as_deref(), Some("edit_protected"));
    assert!(!prompt.options.is_empty(), "should parse options");
    assert_eq!(prompt.options[1], "Yes, allow all edits during this session (shift+tab)");
    assert_eq!(c, "screen:permission");
}

#[test]
fn edit_permission_dialog_settings_file() {
    // Protected directory dialog for .claude/settings.json
    let snap = snapshot(&[
        "────────────────────────────────────────────────────────────────────────────────",
        " Edit file",
        " .claude/settings.json",
        "╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌",
        " 1 +{\"permissions\": {}}",
        "╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌",
        " Do you want to make this edit to .claude/settings.json?",
        " \u{276f} 1. Yes",
        "   2. Yes, allow all edits during this session (shift+tab)",
        "   3. No",
        "",
        " Esc to cancel \u{00b7} Tab to amend",
    ]);
    let (s, c) = classify_claude_screen(&snap).expect("should emit state");
    let prompt = s.prompt().expect("should be Prompt");
    assert_eq!(prompt.kind, PromptKind::Permission);
    assert_eq!(prompt.subtype.as_deref(), Some("edit_protected"));
    assert_eq!(c, "screen:permission");
}

#[test]
fn security_notes_emits_setup() {
    let snap = snapshot(&[
        " Security notes:",
        "",
        " Claude can make mistakes. Review tool use requests carefully.",
        "",
        " Press Enter to continue...",
    ]);
    let (s, c) = classify_claude_screen(&snap).expect("should emit state");
    let prompt = s.prompt().expect("should be Prompt");
    assert_eq!(prompt.kind, PromptKind::Setup);
    assert_eq!(prompt.subtype.as_deref(), Some("security_notes"));
    assert_eq!(c, "screen:setup");
}

#[test]
fn login_success_emits_setup() {
    let snap = snapshot(&[
        " Login successful. Press Enter to continue...",
        "",
        " Logged in as user@example.com",
    ]);
    let (s, _) = classify_claude_screen(&snap).expect("should emit state");
    let prompt = s.prompt().expect("should be Prompt");
    assert_eq!(prompt.kind, PromptKind::Setup);
    assert_eq!(prompt.subtype.as_deref(), Some("login_success"));
}

#[test]
fn oauth_login_extracts_auth_url_single_line() {
    let snap = snapshot(&[
        "",
        " Paste code here if prompted",
        "",
        "https://claude.ai/oauth/authorize?client_id=abc&state=xyz",
        "",
    ]);
    let (s, c) = classify_claude_screen(&snap).expect("should emit state");
    let prompt = s.prompt().expect("should be Prompt");
    assert_eq!(prompt.kind, PromptKind::Setup);
    assert_eq!(prompt.subtype.as_deref(), Some("oauth_login"));
    assert_eq!(
        prompt.input.as_deref(),
        Some("https://claude.ai/oauth/authorize?client_id=abc&state=xyz")
    );
    assert_eq!(c, "screen:setup");
}

#[test]
fn oauth_login_extracts_wrapped_auth_url() {
    // Real Claude wraps the URL across multiple terminal lines.
    let snap = snapshot(&[
        " Browser didn't open? Use the url below to sign in (c to copy)",
        "",
        "https://claude.ai/oauth/authorize?code=true&client_id=9d1c&redirect_uri=",
        "https%3A%2F%2Fplatform.claude.com%2Foauth%2Fcode%2Fcallback&scope=user",
        "%3Asessions&state=BwPX",
        "",
        " Paste code here if prompted >",
    ]);
    let (s, _) = classify_claude_screen(&snap).expect("should emit state");
    let prompt = s.prompt().expect("should be Prompt");
    assert_eq!(prompt.subtype.as_deref(), Some("oauth_login"));
    assert_eq!(
        prompt.input.as_deref(),
        Some("https://claude.ai/oauth/authorize?code=true&client_id=9d1c&redirect_uri=https%3A%2F%2Fplatform.claude.com%2Foauth%2Fcode%2Fcallback&scope=user%3Asessions&state=BwPX")
    );
}

#[test]
fn oauth_login_extracts_platform_domain_auth_url() {
    let snap = snapshot(&[
        " Browser didn't open? Use the url below to sign in (c to copy)",
        "",
        "https://platform.claude.com/oauth/authorize?code=true&client_id=9d1c",
        "",
        " Paste code here if prompted >",
    ]);
    let (s, _) = classify_claude_screen(&snap).expect("should emit state");
    let prompt = s.prompt().expect("should be Prompt");
    assert_eq!(prompt.subtype.as_deref(), Some("oauth_login"));
    assert_eq!(
        prompt.input.as_deref(),
        Some("https://platform.claude.com/oauth/authorize?code=true&client_id=9d1c")
    );
}

#[test]
fn oauth_login_no_url_has_none_auth_url() {
    let snap =
        snapshot(&[" Paste code here if prompted", " Please visit this URL: oauth/authorize"]);
    let (s, _) = classify_claude_screen(&snap).expect("should emit state");
    let prompt = s.prompt().expect("should be Prompt");
    assert_eq!(prompt.subtype.as_deref(), Some("oauth_login"));
    assert!(prompt.input.is_none());
}

#[test]
fn detects_prompt_with_status_text_below() {
    let snap = snapshot(&[
        "Claude Code v2.1.37",
        "",
        "\u{276f}\u{00a0}Try \"create a util logging.py that...\"",
        "────────────────────────────────",
        "  ctrl+t to hide tasks",
    ]);
    assert_eq!(state(&snap), Some(AgentState::Idle));
}

#[test]
fn plan_context_returns_plan_kind() {
    use super::extract_plan_context;

    let screen = ScreenTextSnapshot {
        lines: vec![
            "Plan: Implement auth system".to_string(),
            "Step 1: Add middleware".to_string(),
            "[y] Accept  [n] Reject".to_string(),
        ],
        cols: 80,
        rows: 24,
        alt_screen: false,
        cursor: CursorPosition { row: 2, col: 0 },
        sequence: 42,
    };
    let ctx = extract_plan_context(&screen);
    assert_eq!(ctx.kind, crate::driver::PromptKind::Plan);
    assert!(ctx.input.is_none());
}

use super::parse_options_from_screen;

/// Helper: load a fixture file and split into screen lines.
fn fixture_lines(text: &str) -> Vec<String> {
    text.lines().map(String::from).collect()
}

/// Bash permission dialog (from bash_permission_dialog.tui.txt)
#[test]
fn parse_options_bash_permission() {
    let lines = fixture_lines(include_str!("fixtures/bash_permission.screen.txt"));
    let opts = parse_options_from_screen(&lines);
    assert_eq!(opts, vec!["Yes", "Yes, and always allow access to tmp/ from this project", "No"]);
}

/// Edit permission dialog (from edit_permission_dialog.tui.txt)
#[test]
fn parse_options_edit_permission() {
    let lines = fixture_lines(include_str!("fixtures/edit_permission.screen.txt"));
    let opts = parse_options_from_screen(&lines);
    assert_eq!(opts, vec!["Yes", "Yes, allow all edits during this session (shift+tab)", "No"]);
}

/// Trust folder / Bash permission dialog (from trust_folder_dialog.tui.txt)
#[test]
fn parse_options_trust_folder() {
    let lines = fixture_lines(include_str!("fixtures/trust_folder.screen.txt"));
    let opts = parse_options_from_screen(&lines);
    assert_eq!(opts, vec!["Yes", "Yes, allow reading from Downloads/ from this project", "No"]);
}

/// Thinking dialog (from thinking_dialog_disabled_selected.tui.txt)
#[test]
fn parse_options_thinking_dialog() {
    let lines = fixture_lines(include_str!("fixtures/thinking_dialog.screen.txt"));
    let opts = parse_options_from_screen(&lines);
    assert_eq!(
        opts,
        vec![
            "Enabled ✔  Claude will think before responding",
            "Disabled   Claude will respond without extended thinking",
        ]
    );
}

/// Multi-question dialog Q1 (from multi_question_dialog_q1.tui.txt)
/// Options are split across a separator line, with description lines under each.
#[test]
fn parse_options_multi_question_dialog() {
    let lines = fixture_lines(include_str!("fixtures/multi_question_q1.screen.txt"));
    let opts = parse_options_from_screen(&lines);
    assert_eq!(opts, vec!["Rust", "Python", "Type something.", "Chat about this"]);
}

/// Non-breaking space after ❯ (Claude uses U+00A0 in practice)
#[test]
fn parse_options_nbsp_after_selector() {
    let lines =
        vec![" Do you want to proceed?".into(), " ❯\u{00A0}1. Yes".into(), "   2. No".into()];
    let opts = parse_options_from_screen(&lines);
    assert_eq!(opts, vec!["Yes", "No"]);
}

#[test]
fn parse_options_empty_screen() {
    let opts = parse_options_from_screen(&[]);
    assert!(opts.is_empty());
}

/// Theme picker with trailing checkmark on selected option
#[test]
fn parse_options_strips_trailing_checkmark() {
    let lines = vec![
        " Choose the text style".into(),
        " ❯ 1. Dark mode ✔".into(),
        "   2. Light mode".into(),
        "   3. Light mode (high contrast)".into(),
        " Enter to confirm · Esc to exit".into(),
    ];
    let opts = parse_options_from_screen(&lines);
    assert_eq!(opts, vec!["Dark mode", "Light mode", "Light mode (high contrast)"]);
}

#[test]
fn parse_options_no_match() {
    let lines = vec!["Working on your task...".into(), "Reading files".into()];
    let opts = parse_options_from_screen(&lines);
    assert!(opts.is_empty());
}

/// Login success fixture (real TUI screen with ASCII art)
#[test]
fn login_success_fixture_emits_setup() {
    let lines = fixture_lines(include_str!("fixtures/login_success.screen.txt"));
    let snap = ScreenTextSnapshot {
        lines,
        cols: 200,
        rows: 50,
        alt_screen: false,
        cursor: CursorPosition { row: 0, col: 0 },
        sequence: 1,
    };
    let (s, c) = classify_claude_screen(&snap).expect("should emit state");
    let prompt = s.prompt().expect("should be Prompt");
    assert_eq!(prompt.kind, PromptKind::Setup);
    assert_eq!(prompt.subtype.as_deref(), Some("login_success"));
    assert!(prompt.options.is_empty(), "press-Enter screens have no numbered options");
    assert_eq!(c, "screen:setup");
}

/// Security notes fixture (real TUI screen with ASCII art)
#[test]
fn security_notes_fixture_emits_setup() {
    let lines = fixture_lines(include_str!("fixtures/security_notes.screen.txt"));
    let snap = ScreenTextSnapshot {
        lines,
        cols: 200,
        rows: 50,
        alt_screen: false,
        cursor: CursorPosition { row: 0, col: 0 },
        sequence: 1,
    };
    let (s, c) = classify_claude_screen(&snap).expect("should emit state");
    let prompt = s.prompt().expect("should be Prompt");
    assert_eq!(prompt.kind, PromptKind::Setup);
    assert_eq!(prompt.subtype.as_deref(), Some("security_notes"));
    assert!(prompt.options.is_empty(), "press-Enter screens have no numbered options");
    assert_eq!(c, "screen:setup");
}

/// Bypass permissions dialog should be detected as setup (not idle)
#[test]
fn bypass_permissions_dialog_emits_setup() {
    let snap = snapshot(&[
        "────────────────────────────────",
        " WARNING: Claude Code running in Bypass Permissions mode",
        "",
        " In Bypass Permissions mode, Claude Code will not ask for your approval",
        " before running potentially dangerous commands.",
        "",
        " By proceeding, you accept all responsibility for actions taken while",
        " running in Bypass Permissions mode.",
        "",
        " https://code.claude.com/docs/en/security",
        "",
        " \u{276f} 1. No, exit",
        "   2. Yes, I accept",
        "",
        " Enter to confirm \u{00b7} Esc to cancel",
    ]);
    let (s, c) = classify_claude_screen(&snap).expect("should emit state");
    let prompt = s.prompt().expect("should be Prompt");
    assert_eq!(prompt.kind, PromptKind::Setup);
    assert_eq!(prompt.subtype.as_deref(), Some("bypass_permissions"));
    assert!(!prompt.options.is_empty(), "should parse options");
    assert_eq!(c, "screen:setup");
}

/// Bypass status bar with idle prompt should be idle (not setup)
#[test]
fn bypass_status_bar_with_idle_prompt_is_idle() {
    let snap = snapshot(&[
        " \u{25d0} Claude Code v2.1.39",
        "",
        "\u{276f} ",
        "────────────────────────────────",
        "  \u{23f5}\u{23f5} bypass permissions on (shift+tab to cycle)",
    ]);
    assert_eq!(state(&snap), Some(AgentState::Idle));
    assert_eq!(cause(&snap).as_deref(), Some("screen:idle"));
}

/// Accessing workspace fixture (real TUI trust dialog with numbered options)
#[test]
fn accessing_workspace_fixture_emits_permission() {
    let lines = fixture_lines(include_str!("fixtures/accessing_workspace.screen.txt"));
    let snap = ScreenTextSnapshot {
        lines,
        cols: 200,
        rows: 50,
        alt_screen: false,
        cursor: CursorPosition { row: 0, col: 0 },
        sequence: 1,
    };
    let (s, c) = classify_claude_screen(&snap).expect("should emit state");
    let prompt = s.prompt().expect("should be Prompt");
    assert_eq!(prompt.kind, PromptKind::Permission);
    assert_eq!(prompt.subtype.as_deref(), Some("trust"));
    assert_eq!(prompt.options, vec!["Yes, I trust this folder", "No, exit"]);
    assert_eq!(c, "screen:permission");
}

/// Fable usage-credit consent, mid-session variant: credits are already
/// provisioned, so confirming costs nothing beyond using them.
#[test]
fn fable_consent_fixture_emits_setup() {
    let lines = fixture_lines(include_str!("fixtures/fable_consent.screen.txt"));
    let snap = ScreenTextSnapshot {
        lines,
        cols: 80,
        rows: 24,
        alt_screen: false,
        cursor: CursorPosition { row: 0, col: 0 },
        sequence: 1,
    };
    let (s, c) = classify_claude_screen(&snap).expect("should emit state");
    let prompt = s.prompt().expect("should be Prompt");
    assert_eq!(prompt.kind, PromptKind::Setup);
    assert_eq!(prompt.subtype.as_deref(), Some("fable_consent"));
    assert_eq!(prompt.options, vec!["Continue with Fable 5", "Switch to Opus 4.8 and continue"]);
    assert_eq!(c, "screen:setup");
}

/// Fable usage-credit dialog, no-credits variant: confirming would *buy*
/// credits, so it must classify distinctly from the free consent.
#[test]
fn fable_credits_fixture_emits_setup() {
    let lines = fixture_lines(include_str!("fixtures/fable_credits.screen.txt"));
    let snap = ScreenTextSnapshot {
        lines,
        cols: 80,
        rows: 24,
        alt_screen: false,
        cursor: CursorPosition { row: 0, col: 0 },
        sequence: 1,
    };
    let (s, c) = classify_claude_screen(&snap).expect("should emit state");
    let prompt = s.prompt().expect("should be Prompt");
    assert_eq!(prompt.kind, PromptKind::Setup);
    assert_eq!(prompt.subtype.as_deref(), Some("fable_credits"));
    assert_eq!(prompt.options, vec!["Yes, buy usage credits", "No, keep my current model"]);
    assert_eq!(c, "screen:setup");
}

/// Every rendered variant of the Fable dialog must classify — the harness
/// swaps the confirm label and body copy by credit balance / entry point, and
/// an unmatched variant is exactly the startup wedge this detector exists to
/// prevent (ka-0000001044).
#[test]
fn fable_dialog_variants_all_classify() {
    // (confirm label, decline label, body line, expected subtype)
    let variants: &[(&str, &str, &str, &str)] = &[
        // Picker entry, credits available.
        (
            "Continue with Fable 5",
            "No, keep my current model",
            " Fable 5 runs on usage credits \u{2014} you have $4.00 in credits.",
            "fable_consent",
        ),
        // Overage disabled but a balance remains.
        (
            "Yes, re-enable and continue",
            "Not now",
            " Usage credits are turned off. Re-enable to use Fable 5.",
            "fable_credits",
        ),
        // No credits, self-serve purchase available.
        (
            "Yes, buy usage credits",
            "Not now",
            " You don't have usage credits yet.",
            "fable_credits",
        ),
        // No credits, purchase happens on the web.
        (
            "Buy usage credits",
            "Switch to Opus 4.8 and continue",
            " You don't have usage credits yet.",
            "fable_credits",
        ),
        // Credits managed on the web — no in-CLI purchase (ka-0000001616).
        (
            "Set up usage credits on claude.ai",
            "No, keep my current model",
            " You don't have usage credits yet.",
            "fable_credits",
        ),
        (
            "Manage usage credits on claude.ai",
            "Not now",
            " Usage credits are turned off. Re-enable to use Fable 5.",
            "fable_credits",
        ),
        // Credits managed by an org admin — confirming files a request with a
        // human and still leaves Fable unusable (ka-0000001616).
        (
            "Request usage credits from your admin",
            "Switch to Opus 4.8 and continue",
            " You don't have usage credits yet.",
            "fable_credits",
        ),
        (
            "Request more from your admin",
            "Not now",
            " Usage credits are turned off. Re-enable to use Fable 5.",
            "fable_credits",
        ),
    ];

    for (confirm, decline, body, expected) in variants {
        let snap = snapshot(&[
            "────────────────────────────────────────────────────────────────────────────────",
            " Switch to Fable 5?",
            "",
            body,
            "",
            &format!(" \u{276f} 1. {confirm}"),
            &format!("   2. {decline}"),
            "",
            " Enter to confirm \u{00b7} Esc to cancel",
        ]);
        let (s, _) = classify_claude_screen(&snap)
            .unwrap_or_else(|| panic!("variant {confirm:?} should classify"));
        let prompt = s.prompt().expect("should be Prompt");
        assert_eq!(prompt.kind, PromptKind::Setup, "variant {confirm:?}");
        assert_eq!(prompt.subtype.as_deref(), Some(*expected), "variant {confirm:?}");
        assert_eq!(prompt.options, vec![*confirm, *decline], "variant {confirm:?}");
    }
}

/// The Fable detector must not swallow an ordinary tool-permission dialog that
/// merely mentions usage credits (those stay suppressed for the Tier-1 hooks).
#[test]
fn fable_detector_does_not_capture_tool_permission() {
    let snap = snapshot(&[
        "────────────────────────────────────────────────────────────────────────────────",
        " Bash command",
        "",
        "   gb account limits",
        "   Check usage credits for each account",
        "",
        " Do you want to proceed?",
        " \u{276f} 1. Yes",
        "   2. Yes, and don't ask again",
        "   3. No",
        "",
        " Esc to cancel \u{00b7} Tab to amend",
    ]);
    assert_eq!(classify_claude_screen(&snap), None);
}

/// The standalone "Buy usage credits" purchase dialog is a multi-step
/// card-charging flow coop must never answer. It shares one signal phrase with
/// the Fable consent dialog but never names Fable, so it must stay below the
/// signal threshold.
#[test]
fn extra_usage_purchase_dialog_is_not_classified() {
    let snap = snapshot(&[
        "────────────────────────────────────────────────────────────────────────────────",
        " Buy usage credits",
        "",
        " Select an amount to add to your usage credit balance.",
        "",
        " \u{276f} 1. $25",
        "   2. $50",
        "   3. $75",
        "",
        " By confirming, you allow Anthropic to charge your card in the amount above.",
        "",
        " Enter to confirm \u{00b7} Esc to cancel",
    ]);
    assert_eq!(classify_claude_screen(&snap), None, "purchase dialog must never be auto-answered");
}

/// Choosing the admin-request option opens a *second* dialog that mails this
/// org's admins. coop never reaches it (it declines the dialog before it), and
/// must not answer it if something else surfaces it: the phrase is its title
/// here, not an option label, so it stays a single signal (ka-0000001616).
#[test]
fn admin_credit_request_confirmation_is_not_classified() {
    let snap = snapshot(&[
        "────────────────────────────────────────────────────────────────────────────────",
        " Request usage credits from your admin",
        "",
        " This will send a request to your organization's admins to turn on usage credits.",
        " Only send this if you're running into usage limits — your admins are notified.",
        "",
        " \u{276f} 1. Send request",
        "   2. Cancel",
        "",
        " Enter to confirm \u{00b7} Esc to cancel",
    ]);
    assert_eq!(classify_claude_screen(&snap), None, "admin request must never be auto-answered");
}

/// The weekly-limit dialog ("What do you want to do?") is classified as
/// `fable_limit` (ka-0000003007) and answered toward the STOP/WAIT option — the
/// one that neither spends nor upgrades — selected by LABEL. The point of the
/// fix: it is no longer left unanswered (which wedged the pod), yet the
/// card-charging "Add funds…" option is still never pressed.
#[test]
fn weekly_limit_dialog_classifies_and_answers_safe_option() {
    let snap = snapshot(&[
        "────────────────────────────────────────────────────────────────────────────────",
        " You've reached your Fable 5 limit.",
        "",
        " What do you want to do?",
        "",
        " \u{276f} 1. Upgrade to Max",
        "   2. Add funds to continue with usage credits",
        "   3. Stop and wait for limit to reset",
        "",
        " Enter to confirm \u{00b7} Esc to cancel",
    ]);
    let (state, cause) =
        classify_claude_screen(&snap).expect("weekly-limit dialog should classify");
    let prompt = state.prompt().expect("should be a Prompt");
    assert_eq!(prompt.kind, PromptKind::Setup);
    assert_eq!(prompt.subtype.as_deref(), Some("fable_limit"));
    assert_eq!(cause, "screen:setup");
    assert_eq!(
        prompt.options,
        vec![
            "Upgrade to Max".to_owned(),
            "Add funds to continue with usage credits".to_owned(),
            "Stop and wait for limit to reset".to_owned(),
        ]
    );

    // The policy layer answers the SAFE stop/wait option (option 3), never the
    // spend option 2 ("Add funds…") nor the upgrade option 1.
    let safe = crate::driver::fable_limit_safe_option(&prompt.options);
    assert_eq!(safe, Some(3), "must select the stop/wait option, not upgrade or add-funds");

    // It is NOT routed through the fixed-option auto-confirm path — that path
    // would press a positional number blind to the label.
    assert_eq!(
        crate::driver::disruption_option(prompt),
        None,
        "fable_limit must be answered by label, never by fixed position"
    );
}

/// SAFETY INVARIANT (ka-0000003007): if the weekly-limit dialog offers no
/// stop/wait option — only spend/upgrade ones — no numbered option is selected.
/// The caller falls back to Esc; the purchase/upgrade options are never pressed.
#[test]
fn weekly_limit_dialog_with_no_safe_option_selects_none() {
    let snap = snapshot(&[
        "────────────────────────────────────────────────────────────────────────────────",
        " You've reached your Fable 5 limit.",
        "",
        " What do you want to do?",
        "",
        " \u{276f} 1. Upgrade to Max",
        "   2. Add funds to continue with usage credits",
        "   3. Adjust monthly limit",
        "",
        " Enter to confirm \u{00b7} Esc to cancel",
    ]);
    let (state, _) = classify_claude_screen(&snap).expect("weekly-limit dialog should classify");
    let prompt = state.prompt().expect("should be a Prompt");
    assert_eq!(prompt.subtype.as_deref(), Some("fable_limit"));
    assert_eq!(
        crate::driver::fable_limit_safe_option(&prompt.options),
        None,
        "no stop/wait option present — must select nothing (caller sends Esc)"
    );
}

/// The alternate option wording ("Wait for limit to reset", "Don't continue
/// automatically", bare "Stop") is also recognized as safe, and the earliest
/// safe option wins.
#[test]
fn weekly_limit_dialog_recognizes_alternate_safe_labels() {
    let snap = snapshot(&[
        "────────────────────────────────────────────────────────────────────────────────",
        " You've reached your Fable 5 limit.",
        "",
        " What do you want to do?",
        "",
        " \u{276f} 1. Upgrade to Team plan",
        "   2. Wait for limit to reset",
        "   3. Don't continue automatically",
        "",
        " Enter to confirm \u{00b7} Esc to cancel",
    ]);
    let (state, _) = classify_claude_screen(&snap).expect("weekly-limit dialog should classify");
    let prompt = state.prompt().expect("should be a Prompt");
    assert_eq!(prompt.subtype.as_deref(), Some("fable_limit"));
    assert_eq!(
        crate::driver::fable_limit_safe_option(&prompt.options),
        Some(2),
        "'Wait for limit to reset' is the first safe option"
    );
}

/// End-to-end: real Claude TUI byte stream -> coop's terminal emulator ->
/// screen classifier. The fixture-based tests above use hand-typed lines; this
/// one feeds the ANSI Claude actually emits (per-word absolute-column cursor
/// moves, SGR styling, box drawing) through the same `Screen` the live session
/// uses, proving the detector survives real rendering and not just clean text.
#[test]
fn fable_consent_classifies_from_real_tui_byte_stream() {
    let mut screen = crate::screen::Screen::new(80, 24);
    screen.feed(b"\x1b[?25l\x1b[38;5;220m\xe2\x94\x80\xe2\x94\x80\xe2\x94\x80\xe2\x94\x80\xe2\x94\x80\xe2\x94\x80\xe2\x94\x80\xe2\x94\x80\xe2\x94\x80\xe2\x94\x80\xe2\x94\x80\xe2\x94\x80\xe2\x94\x80\xe2\x94\x80\xe2\x94\x80\xe2\x94\x80\xe2\x94\x80\xe2\x94\x80\xe2\x94\x80\xe2\x94\x80\xe2\x94\x80\xe2\x94\x80\xe2\x94\x80\xe2\x94\x80\xe2\x94\x80\xe2\x94\x80\xe2\x94\x80\xe2\x94\x80\xe2\x94\x80\xe2\x94\x80\xe2\x94\x80\xe2\x94\x80\xe2\x94\x80\xe2\x94\x80\xe2\x94\x80\xe2\x94\x80\xe2\x94\x80\xe2\x94\x80\xe2\x94\x80\xe2\x94\x80\xe2\x94\x80\xe2\x94\x80\xe2\x94\x80\xe2\x94\x80\xe2\x94\x80\xe2\x94\x80\xe2\x94\x80\xe2\x94\x80\xe2\x94\x80\xe2\x94\x80\xe2\x94\x80\xe2\x94\x80\xe2\x94\x80\xe2\x94\x80\xe2\x94\x80\xe2\x94\x80\xe2\x94\x80\xe2\x94\x80\xe2\x94\x80\xe2\x94\x80\xe2\x94\x80\xe2\x94\x80\xe2\x94\x80\xe2\x94\x80\xe2\x94\x80\xe2\x94\x80\xe2\x94\x80\xe2\x94\x80\xe2\x94\x80\xe2\x94\x80\xe2\x94\x80\xe2\x94\x80\xe2\x94\x80\xe2\x94\x80\xe2\x94\x80\xe2\x94\x80\xe2\x94\x80\xe2\x94\x80\xe2\x94\x80\xe2\x94\x80\x1b[39m\x0d\x0a\x1b[38;5;220m\x1b[1m\x1b[2GYou've\x1b[9Greached\x1b[17Gyour\x1b[22GFable\x1b[28G5\x1b[30Glimit\x1b[22m\x1b[39m\x0d\x0a\x0d\x0a\x1b[2GYou've\x1b[9Gused\x1b[14Gyour\x1b[19Gincluded\x1b[28GFable\x1b[34G5\x1b[36Gusage\x1b[42Gfor\x1b[46Gthis\x1b[51Gweek.\x1b[57GContinuing\x0d\x0a\x1b[2Gon\x1b[5GFable\x1b[11G5\x1b[13Guses\x1b[18Gusage\x1b[24Gcredits\x1b[32G\xe2\x80\x94\x1b[34Gyou\x1b[38Ghave\x1b[43G$18.40\x1b[50Gin\x1b[53Gcredits.\x0d\x0a\x0d\x0a\x1b[2G\x1b[38;5;153m\xe2\x9d\xaf\x1b[4G\x1b[38;5;246m1.\x1b[7G\x1b[38;5;153mContinue\x1b[16Gwith\x1b[21GFable\x1b[27G5\x1b[39m\x0d\x0a\x1b[4G\x1b[38;5;246m2.\x1b[7G\x1b[39mSwitch\x1b[14Gto\x1b[17GOpus\x1b[22G4.8\x1b[26Gand\x1b[30Gcontinue\x0d\x0a\x0d\x0a\x1b[2G\x1b[38;5;246mEnter\x1b[8Gto\x1b[11Gconfirm\x1b[19G\xc2\xb7\x1b[21GEsc\x1b[25Gto\x1b[28Gcancel\x1b[39m");
    let snap = screen.snapshot_text();

    let (s, c) = classify_claude_screen(&snap).expect("rendered dialog should classify");
    let prompt = s.prompt().expect("should be Prompt");
    assert_eq!(prompt.kind, PromptKind::Setup);
    assert_eq!(prompt.subtype.as_deref(), Some("fable_consent"));
    assert_eq!(prompt.options, vec!["Continue with Fable 5", "Switch to Opus 4.8 and continue"]);
    assert_eq!(c, "screen:setup");

    // Answered by LABEL, not fixed position (ka-0000006026): cursor located and
    // the "Continue with Fable" confirm found, so groom arrow-navs — no digit.
    assert_eq!(prompt.selected, Some(0), "cursor rests on the first option");
    assert_eq!(crate::driver::fable_consent_confirm_index(&prompt.options), Some(0));
    assert_eq!(crate::driver::disruption_option(prompt), None);
}

// -- 2.1.257 UNNUMBERED consent picker (ka-0000006026) --
use super::parse_picker;

/// 2.1.257 made the FREE consent gate an UNNUMBERED picker with confirm SECOND
/// and the cursor on DECLINE. The old `N. ` parser returned empty options and
/// the digit '1' selected nothing — wedging every fable pod. It must now parse
/// the unnumbered options, locate the cursor, and expose the confirm by label.
#[test]
fn fable_consent_unnumbered_picker_classifies_and_answers_by_label() {
    let lines = fixture_lines(include_str!("fixtures/fable_consent_unnumbered.screen.txt"));
    let snap = snapshot(&lines.iter().map(String::as_str).collect::<Vec<_>>());
    let prompt = state(&snap).expect("should classify").prompt().expect("Prompt").clone();
    assert_eq!(prompt.subtype.as_deref(), Some("fable_consent"));
    assert_eq!(prompt.options, vec!["Switch to Sonnet 5 and continue", "Continue with Fable 5.1"]);
    assert_eq!(prompt.selected, Some(0), "cursor defaults to the DECLINE option");
    // Confirm found by label at index 1 despite the ".1" suffix (groom arrow-navs).
    assert_eq!(crate::driver::fable_consent_confirm_index(&prompt.options), Some(1));
    assert_eq!(crate::driver::disruption_option(&prompt), None);
}

/// A consent screen whose TITLE carries the model minor version — "Fable 5.1
/// now uses usage credits" — must still classify (ka-0000001501). The title
/// signal was pinned to the literal "Fable 5 now uses usage credits", which a
/// "Fable 5.1 …" title fails, leaving only the "Continue with Fable 5" confirm
/// substring as a lone hit — below threshold, so a 5.1-titled screen wedged
/// unclassified. Generalizing the title signal to "now uses usage credits"
/// restores threshold for every Fable minor version.
#[test]
fn fable_consent_51_title_classifies() {
    let lines = fixture_lines(include_str!("fixtures/fable_consent_51_title.screen.txt"));
    let snap = snapshot(&lines.iter().map(String::as_str).collect::<Vec<_>>());
    let prompt = state(&snap).expect("should classify").prompt().expect("Prompt").clone();
    assert_eq!(prompt.subtype.as_deref(), Some("fable_consent"));
    assert_eq!(prompt.options, vec!["Switch to Sonnet 5 and continue", "Continue with Fable 5.1"]);
    // Confirm found by label; the model-switch option is never a confirm target.
    assert_eq!(crate::driver::fable_consent_confirm_index(&prompt.options), Some(1));
    assert_eq!(crate::driver::disruption_option(&prompt), None);
}

/// The unnumbered *credits* (spend) picker classifies as `fable_credits` (Esc-declined).
#[test]
fn fable_credits_unnumbered_picker_classifies_as_credits() {
    let lines = fixture_lines(include_str!("fixtures/fable_credits_unnumbered.screen.txt"));
    let snap = snapshot(&lines.iter().map(String::as_str).collect::<Vec<_>>());
    let prompt = state(&snap).expect("should classify").prompt().expect("Prompt").clone();
    assert_eq!(prompt.subtype.as_deref(), Some("fable_credits"), "spend gate, not free consent");
    assert_eq!(prompt.options, vec!["Switch to Sonnet 5 and continue", "Yes, buy usage credits"]);
    assert_eq!(crate::driver::fable_consent_confirm_index(&prompt.options), None);
}

/// `parse_picker` handles UNNUMBERED (the 2.1.257 gap) + NUMBERED (strips `N. `).
#[test]
fn parse_picker_handles_unnumbered_and_numbered() {
    let unnumbered = fixture_lines(
        " Title\n\n Body text.\n\n \u{276f} Switch to Sonnet 5 and continue\n \
         Continue with Fable 5.1\n\n Enter to confirm \u{00b7} Esc to cancel",
    );
    assert_eq!(
        parse_picker(&unnumbered),
        (vec!["Switch to Sonnet 5 and continue".into(), "Continue with Fable 5.1".into()], Some(0))
    );

    // Numbered, cursor on the SECOND option.
    let numbered = fixture_lines(
        " Title\n\n 1. Continue with Fable 5\n \u{276f} 2. No, keep my current model\n\n \
         Enter to confirm \u{00b7} Esc to cancel",
    );
    assert_eq!(
        parse_picker(&numbered),
        (vec!["Continue with Fable 5".into(), "No, keep my current model".into()], Some(1))
    );
}
