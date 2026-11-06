// SPDX-License-Identifier: BUSL-1.1
// Copyright (c) 2026 Matt Baker <fimbaz@gmail.com>

use super::parse_options_from_screen;

fn fixture_lines(text: &str) -> Vec<String> {
    text.lines().map(String::from).collect()
}

#[test]
fn parse_options_bash_permission() {
    let lines = fixture_lines(include_str!("fixtures/bash_permission.screen.txt"));
    let opts = parse_options_from_screen(&lines);
    assert_eq!(opts, vec!["Allow once", "Allow for this session", "No, suggest changes (esc)"]);
}

#[test]
fn parse_options_empty_screen() {
    let opts = parse_options_from_screen(&[]);
    assert!(opts.is_empty());
}

#[test]
fn parse_options_no_match() {
    let lines = vec!["Working on your task...".into(), "Reading files".into()];
    let opts = parse_options_from_screen(&lines);
    assert!(opts.is_empty());
}

#[test]
fn parse_options_spinner_only() {
    let lines = vec!["⠏ Waiting for user confirmation...".into()];
    let opts = parse_options_from_screen(&lines);
    assert!(opts.is_empty());
}

#[test]
fn parse_options_inline_box() {
    let lines = vec![
        "╭──────────────────╮".into(),
        "│ Choose one:      │".into(),
        "│ ● 1. Option A    │".into(),
        "│   2. Option B    │".into(),
        "╰──────────────────╯".into(),
    ];
    let opts = parse_options_from_screen(&lines);
    assert_eq!(opts, vec!["Option A", "Option B"]);
}
