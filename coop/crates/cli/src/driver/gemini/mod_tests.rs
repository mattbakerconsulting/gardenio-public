// SPDX-License-Identifier: BUSL-1.1
// Copyright (c) 2026 Matt Baker <fimbaz@gmail.com>

use super::GeminiMode;

fn cmd(args: &[&str]) -> Vec<String> {
    args.iter().map(|a| (*a).to_owned()).collect()
}

#[yare::parameterized(
    acp = { &["gemini", "--acp"], GeminiMode::Acp },
    acp_with_other_flags = { &["gemini", "-m", "gemini-2.5-pro", "--acp"], GeminiMode::Acp },
    old_spelling = { &["gemini", "--experimental-acp"], GeminiMode::Acp },
    interactive = { &["gemini"], GeminiMode::Tui },
    headless_prompt = { &["gemini", "-p", "hi", "-o", "stream-json"], GeminiMode::Tui },
    // The binary name is not a flag.
    binary_named_acp = { &["--acp"], GeminiMode::Tui },
)]
fn mode_follows_the_command_line(args: &[&str], expected: GeminiMode) {
    assert_eq!(GeminiMode::from_command(&cmd(args)), expected);
}

#[test]
fn only_acp_has_no_terminal() {
    assert!(GeminiMode::Acp.is_headless());
    assert!(!GeminiMode::Tui.is_headless());
}
