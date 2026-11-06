// SPDX-License-Identifier: BUSL-1.1
// Copyright (c) 2026 Matt Baker <fimbaz@gmail.com>

use std::time::Duration;

use super::{CodexNudgeEncoder, CodexRespondEncoder};
use crate::driver::codex::CodexMode;
use crate::driver::{NudgeEncoder, QuestionAnswer, RespondEncoder};

const DELAY: Duration = Duration::from_millis(200);

#[test]
fn headless_nudge_is_one_atomic_line() {
    for mode in [CodexMode::AppServer, CodexMode::Exec] {
        let steps = CodexNudgeEncoder::for_mode(mode, DELAY, DELAY).encode("do the thing");
        assert_eq!(steps.len(), 1);
        assert_eq!(steps[0].bytes, b"do the thing\r");
        assert!(steps[0].delay_after.is_none());
    }
}

#[test]
fn tui_nudge_types_waits_then_presses_enter() {
    let steps = CodexNudgeEncoder::for_mode(CodexMode::Tui, DELAY, Duration::ZERO).encode("hi");
    assert_eq!(steps.len(), 2);
    assert_eq!(steps[0].bytes, b"hi");
    assert_eq!(steps[0].delay_after, Some(DELAY));
    assert_eq!(steps[1].bytes, b"\r");
}

#[test]
fn nudge_bytes_round_trip_through_the_line_discipline() {
    use crate::driver::codex::rpc::{InputAction, LineDiscipline};
    let steps =
        CodexNudgeEncoder::for_mode(CodexMode::AppServer, DELAY, DELAY).encode("multi\nline");
    let mut ld = LineDiscipline::default();
    let actions = ld.feed(&steps[0].bytes);
    assert_eq!(actions, vec![InputAction::Submit("multi\nline".into())]);
}

#[test]
fn respond_encoder_has_nothing_to_send() {
    let enc = CodexRespondEncoder;
    let answer = QuestionAnswer { option: Some(1), text: None };
    assert!(enc.encode_permission(1).is_empty());
    assert!(enc.encode_plan(1, Some("x")).is_empty());
    assert!(enc.encode_question(&[answer], 1).is_empty());
    assert!(enc.encode_setup(1).is_empty());
}
