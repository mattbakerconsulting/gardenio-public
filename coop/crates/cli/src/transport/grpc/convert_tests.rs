// SPDX-License-Identifier: BUSL-1.1
// Copyright (c) 2026 Matt Baker <fimbaz@gmail.com>

use super::convert::*;
use crate::driver::AgentState;
use crate::screen::{CursorPosition, ScreenTextSnapshot};

#[test]
fn cursor_to_proto_converts_u16_to_i32() {
    let cursor = CursorPosition { row: 5, col: 42 };
    let p = cursor_to_proto(&cursor);
    assert_eq!(p.row, 5);
    assert_eq!(p.col, 42);
}

#[test]
fn cursor_to_proto_handles_max_u16() {
    let cursor = CursorPosition { row: u16::MAX, col: u16::MAX };
    let p = cursor_to_proto(&cursor);
    assert_eq!(p.row, u16::MAX as i32);
    assert_eq!(p.col, u16::MAX as i32);
}

#[test]
fn screen_text_snapshot_to_proto_converts_all_fields() {
    let snap = ScreenTextSnapshot {
        lines: vec!["hello".to_owned(), "world".to_owned()],
        cols: 80,
        rows: 24,
        alt_screen: true,
        cursor: CursorPosition { row: 1, col: 5 },
        sequence: 42,
    };
    let p = screen_text_snapshot_to_proto(&snap);
    assert_eq!(p.lines, vec!["hello", "world"]);
    assert_eq!(p.cols, 80);
    assert_eq!(p.rows, 24);
    assert!(p.alt_screen);
    assert_eq!(p.seq, 42);
    assert_eq!(p.cursor.as_ref().map(|cursor| cursor.row), Some(1));
    assert_eq!(p.cursor.as_ref().map(|cursor| cursor.col), Some(5));
}

#[test]
fn screen_text_snapshot_to_response_omits_cursor() {
    let snap = ScreenTextSnapshot {
        lines: vec![],
        cols: 40,
        rows: 10,
        alt_screen: false,
        cursor: CursorPosition { row: 0, col: 0 },
        sequence: 1,
    };
    let resp = screen_text_snapshot_to_response(&snap, false);
    assert!(resp.cursor.is_none());
    assert_eq!(resp.cols, 40);
}

#[test]
fn screen_text_snapshot_to_response_includes_cursor() {
    let snap = ScreenTextSnapshot {
        lines: vec![],
        cols: 40,
        rows: 10,
        alt_screen: false,
        cursor: CursorPosition { row: 3, col: 7 },
        sequence: 1,
    };
    let resp = screen_text_snapshot_to_response(&snap, true);
    assert!(resp.cursor.is_some());
    let c = resp.cursor.as_ref();
    assert!(c.is_some());
}

#[test]
fn prompt_to_proto_converts_all_fields() {
    let prompt = crate::driver::PromptContext::new(crate::driver::PromptKind::Permission)
        .with_tool("bash")
        .with_input("rm -rf /");
    let p = prompt_to_proto(&prompt);
    assert_eq!(p.r#type, "permission");
    assert_eq!(p.tool.as_deref(), Some("bash"));
    assert_eq!(p.input.as_deref(), Some("rm -rf /"));
    // auth_url removed; OAuth URL goes in input
}

#[test]
fn prompt_to_proto_maps_subtype() {
    let prompt = crate::driver::PromptContext::new(crate::driver::PromptKind::Setup)
        .with_subtype("theme_picker")
        .with_options(vec!["Dark mode".to_owned(), "Light mode".to_owned()])
        .with_ready();
    let p = prompt_to_proto(&prompt);
    assert_eq!(p.r#type, "setup");
    assert_eq!(p.subtype.as_deref(), Some("theme_picker"));
    assert_eq!(p.options, vec!["Dark mode", "Light mode"]);
    assert!(p.tool.is_none());
}

#[test]
fn prompt_to_proto_handles_none_fields() {
    let prompt =
        crate::driver::PromptContext::new(crate::driver::PromptKind::Question).with_ready();
    let p = prompt_to_proto(&prompt);
    assert_eq!(p.r#type, "question");
    assert!(p.tool.is_none());
    assert!(p.input.is_none());
    // auth_url removed; OAuth URL goes in input
}

#[test]
fn transition_to_proto_converts_simple_transition() {
    let event = crate::event::TransitionEvent {
        prev: AgentState::Starting,
        next: AgentState::Working,
        seq: 7,
        cause: String::new(),
        last_message: None,
    };
    let p = transition_to_proto(&event);
    assert_eq!(p.prev, "starting");
    assert_eq!(p.next, "working");
    assert_eq!(p.seq, 7);
    assert!(p.prompt.is_none());
}

#[test]
fn transition_to_proto_includes_prompt() {
    let prompt =
        crate::driver::PromptContext::new(crate::driver::PromptKind::Permission).with_tool("write");
    let event = crate::event::TransitionEvent {
        prev: AgentState::Working,
        next: AgentState::Prompt { prompt: prompt.clone() },
        seq: 10,
        cause: String::new(),
        last_message: None,
    };
    let p = transition_to_proto(&event);
    assert_eq!(p.next, "prompt");
    assert!(p.prompt.is_some());
    let pp = p.prompt.as_ref();
    assert!(pp.is_some());
}

#[test]
fn transition_to_proto_includes_error_fields() {
    let event = crate::event::TransitionEvent {
        prev: AgentState::Working,
        next: AgentState::Error { detail: "rate_limit_error".to_owned() },
        seq: 5,
        cause: String::new(),
        last_message: None,
    };
    let p = transition_to_proto(&event);
    assert_eq!(p.next, "error");
    assert_eq!(p.error_detail.as_deref(), Some("rate_limit_error"));
    assert_eq!(p.error_category.as_deref(), Some("rate_limited"));
}

#[test]
fn transition_to_proto_omits_error_fields_for_non_error() {
    let event = crate::event::TransitionEvent {
        prev: AgentState::Starting,
        next: AgentState::Working,
        seq: 1,
        cause: String::new(),
        last_message: None,
    };
    let p = transition_to_proto(&event);
    assert!(p.error_detail.is_none());
    assert!(p.error_category.is_none());
}
