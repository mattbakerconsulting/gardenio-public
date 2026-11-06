// SPDX-License-Identifier: BUSL-1.1
// Copyright (c) 2026 Matt Baker <fimbaz@gmail.com>

use std::time::Duration;

use crate::driver::{NudgeEncoder, QuestionAnswer, RespondEncoder};

use super::{GeminiNudgeEncoder, GeminiRespondEncoder};

fn test_nudge_encoder() -> GeminiNudgeEncoder {
    GeminiNudgeEncoder {
        input_delay: Duration::from_millis(200),
        input_delay_per_byte: Duration::from_millis(1),
    }
}

#[test]
fn nudge_encodes_message_then_enter() {
    let encoder = test_nudge_encoder();
    let steps = encoder.encode("Fix the bug");
    assert_eq!(steps.len(), 2);
    assert_eq!(steps[0].bytes, b"Fix the bug");
    assert_eq!(steps[0].delay_after, Some(Duration::from_millis(200)));
    assert_eq!(steps[1].bytes, b"\r");
    assert!(steps[1].delay_after.is_none());
}

#[test]
fn nudge_with_multiline_message() {
    let encoder = test_nudge_encoder();
    let steps = encoder.encode("line1\nline2");
    assert_eq!(steps.len(), 2);
    assert_eq!(steps[0].bytes, b"line1\nline2");
    assert_eq!(steps[1].bytes, b"\r");
}

#[yare::parameterized(
    option_1_accept = { 1, b"1\r" as &[u8] },
    option_2_reject = { 2, b"\x1b" },
    option_3_reject = { 3, b"\x1b" },
)]
fn permission_encoding(option: u32, expected: &[u8]) {
    let encoder = GeminiRespondEncoder::default();
    let steps = encoder.encode_permission(option);
    assert_eq!(steps.len(), 1);
    assert_eq!(steps[0].bytes, expected);
}

#[yare::parameterized(
    option_1 = { 1, b"y\r" as &[u8] },
    option_2 = { 2, b"y\r" },
    option_3 = { 3, b"y\r" },
)]
fn plan_accept_options(option: u32, expected: &[u8]) {
    let encoder = GeminiRespondEncoder::default();
    let steps = encoder.encode_plan(option, None);
    assert_eq!(steps.len(), 1);
    assert_eq!(steps[0].bytes, expected);
    assert!(steps[0].delay_after.is_none());
}

#[test]
fn plan_option_4_with_feedback() {
    let encoder = GeminiRespondEncoder::default();
    let steps = encoder.encode_plan(4, Some("Don't modify the schema"));
    assert_eq!(steps.len(), 2);
    assert_eq!(steps[0].bytes, b"n\r");
    assert_eq!(steps[0].delay_after, Some(Duration::from_millis(200)));
    assert_eq!(steps[1].bytes, b"Don't modify the schema\r");
    assert!(steps[1].delay_after.is_none());
}

#[test]
fn plan_option_4_without_feedback() {
    let encoder = GeminiRespondEncoder::default();
    let steps = encoder.encode_plan(4, None);
    assert_eq!(steps.len(), 1);
    assert_eq!(steps[0].bytes, b"n\r");
    assert!(steps[0].delay_after.is_none());
}

#[test]
fn question_with_option_number() {
    let encoder = GeminiRespondEncoder::default();
    let answers = [QuestionAnswer { option: Some(2), text: None }];
    let steps = encoder.encode_question(&answers, 1);
    assert_eq!(steps.len(), 1);
    assert_eq!(steps[0].bytes, b"2\r");
}

#[test]
fn question_with_freeform_text() {
    let encoder = GeminiRespondEncoder::default();
    let answers = [QuestionAnswer { option: None, text: Some("Use Redis instead".to_string()) }];
    let steps = encoder.encode_question(&answers, 1);
    assert_eq!(steps.len(), 1);
    assert_eq!(steps[0].bytes, b"Use Redis instead\r");
}

#[test]
fn question_with_neither_option_nor_text() {
    let encoder = GeminiRespondEncoder::default();
    let steps = encoder.encode_question(&[], 0);
    assert!(steps.is_empty());
}
