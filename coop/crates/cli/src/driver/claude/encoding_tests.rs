// SPDX-License-Identifier: BUSL-1.1
// Copyright (c) 2026 Matt Baker <fimbaz@gmail.com>

use std::time::Duration;

use crate::driver::{compute_nudge_delay, NudgeEncoder, QuestionAnswer, RespondEncoder};

use super::{ClaudeNudgeEncoder, ClaudeRespondEncoder};

fn test_nudge_encoder() -> ClaudeNudgeEncoder {
    ClaudeNudgeEncoder {
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
    // Short message (11 bytes < 256): delay == base
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

#[test]
fn nudge_delay_scales_with_length() {
    let encoder = test_nudge_encoder();
    // 1024 bytes: base 200ms + (1024-256)*1ms = 200+768 = 968ms
    let msg = "x".repeat(1024);
    let steps = encoder.encode(&msg);
    assert_eq!(steps[0].delay_after, Some(Duration::from_millis(968)));
}

#[test]
fn nudge_delay_scales_for_large_input() {
    let encoder = test_nudge_encoder();
    // 10000 bytes: 200ms + (10000-256)*1ms = 200+9744 = 9944ms
    let msg = "x".repeat(10000);
    let steps = encoder.encode(&msg);
    assert_eq!(steps[0].delay_after, Some(Duration::from_millis(9944)));
}

#[test]
fn compute_nudge_delay_short_message() {
    let d = compute_nudge_delay(Duration::from_millis(200), Duration::from_millis(1), 100);
    assert_eq!(d, Duration::from_millis(200));
}

#[test]
fn compute_nudge_delay_medium_message() {
    // 512 bytes: 200ms + (512-256)*1ms = 456ms
    let d = compute_nudge_delay(Duration::from_millis(200), Duration::from_millis(1), 512);
    assert_eq!(d, Duration::from_millis(456));
}

#[test]
fn compute_nudge_delay_large_message() {
    // 20000 bytes: 200ms + (20000-256)*1ms = 19944ms
    let d = compute_nudge_delay(Duration::from_millis(200), Duration::from_millis(1), 20000);
    assert_eq!(d, Duration::from_millis(19944));
}

#[yare::parameterized(
    option_1_yes            = { 1, b"1" as &[u8] },
    option_2_dont_ask_again = { 2, b"2" },
    option_3_no             = { 3, b"3" },
)]
fn permission_encoding(option: u32, expected: &[u8]) {
    let encoder = ClaudeRespondEncoder::default();
    let steps = encoder.encode_permission(option);
    assert_eq!(steps.len(), 1);
    assert_eq!(steps[0].bytes, expected);
}

#[yare::parameterized(
    option_1_clear_context = { 1, b"1" as &[u8] },
    option_2_auto_accept   = { 2, b"2" },
    option_3_manual        = { 3, b"3" },
)]
fn plan_accept_options(option: u32, expected: &[u8]) {
    let encoder = ClaudeRespondEncoder::default();
    let steps = encoder.encode_plan(option, None);
    assert_eq!(steps.len(), 1);
    assert_eq!(steps[0].bytes, expected);
    assert!(steps[0].delay_after.is_none());
}

#[test]
fn plan_feedback_sends_up_arrow_then_text_then_enter() {
    let encoder = ClaudeRespondEncoder::default();
    let steps = encoder.encode_plan(4, Some("Don't modify the schema"));
    assert_eq!(steps.len(), 3);
    assert_eq!(steps[0].bytes, b"\x1b[A");
    assert_eq!(steps[0].delay_after, Some(Duration::from_millis(200)));
    assert_eq!(steps[1].bytes, b"Don't modify the schema");
    assert_eq!(steps[1].delay_after, Some(Duration::from_millis(200)));
    assert_eq!(steps[2].bytes, b"\r");
    assert!(steps[2].delay_after.is_none());
}

#[test]
fn plan_option_4_without_feedback_sends_digit() {
    let encoder = ClaudeRespondEncoder::default();
    let steps = encoder.encode_plan(4, None);
    assert_eq!(steps.len(), 1);
    assert_eq!(steps[0].bytes, b"4");
    assert!(steps[0].delay_after.is_none());
}

#[test]
fn question_single_with_option_number() {
    let encoder = ClaudeRespondEncoder::default();
    let answers = [QuestionAnswer { option: Some(2), text: None }];
    let steps = encoder.encode_question(&answers, 1);
    assert_eq!(steps.len(), 1);
    assert_eq!(steps[0].bytes, b"2");
}

#[test]
fn question_single_with_freeform_text() {
    let encoder = ClaudeRespondEncoder::default();
    let answers = [QuestionAnswer { option: None, text: Some("Use Redis instead".to_string()) }];
    let steps = encoder.encode_question(&answers, 1);
    assert_eq!(steps.len(), 3);
    assert_eq!(steps[0].bytes, b"\x1b[A");
    assert_eq!(steps[0].delay_after, Some(Duration::from_millis(200)));
    assert_eq!(steps[1].bytes, b"Use Redis instead");
    assert_eq!(steps[1].delay_after, Some(Duration::from_millis(200)));
    assert_eq!(steps[2].bytes, b"\r");
    assert!(steps[2].delay_after.is_none());
}

#[test]
fn question_with_empty_answers() {
    let encoder = ClaudeRespondEncoder::default();
    let steps = encoder.encode_question(&[], 1);
    assert!(steps.is_empty());
}

#[test]
fn question_one_at_a_time_emits_digit_only() {
    let encoder = ClaudeRespondEncoder::default();
    let answers = [QuestionAnswer { option: Some(1), text: None }];
    // Single answer in a multi-question dialog → just digit, no CR.
    let steps = encoder.encode_question(&answers, 3);
    assert_eq!(steps.len(), 1);
    assert_eq!(steps[0].bytes, b"1");
    assert!(steps[0].delay_after.is_none());
}

#[test]
fn question_all_at_once_emits_sequence_with_delays() {
    let encoder = ClaudeRespondEncoder::default();
    let answers = [
        QuestionAnswer { option: Some(1), text: None },
        QuestionAnswer { option: Some(2), text: None },
    ];
    let steps = encoder.encode_question(&answers, 2);
    // Two answer steps + one confirm step.
    assert_eq!(steps.len(), 3);
    assert_eq!(steps[0].bytes, b"1");
    assert_eq!(steps[0].delay_after, Some(Duration::from_millis(200)));
    assert_eq!(steps[1].bytes, b"2");
    assert_eq!(steps[1].delay_after, Some(Duration::from_millis(200)));
    assert_eq!(steps[2].bytes, b"\r");
    assert!(steps[2].delay_after.is_none());
}

#[test]
fn question_all_at_once_freeform_mixed() {
    let encoder = ClaudeRespondEncoder::default();
    let answers = [
        QuestionAnswer { option: Some(1), text: None },
        QuestionAnswer { option: None, text: Some("custom answer".to_string()) },
    ];
    let steps = encoder.encode_question(&answers, 2);
    // option: digit(1 step) + freeform: up+text+enter(3 steps) + final confirm(1 step)
    assert_eq!(steps.len(), 5);
    assert_eq!(steps[0].bytes, b"1");
    assert_eq!(steps[0].delay_after, Some(Duration::from_millis(200)));
    assert_eq!(steps[1].bytes, b"\x1b[A");
    assert_eq!(steps[2].bytes, b"custom answer");
    assert_eq!(steps[3].bytes, b"\r");
    assert_eq!(steps[3].delay_after, Some(Duration::from_millis(200)));
    assert_eq!(steps[4].bytes, b"\r");
    assert!(steps[4].delay_after.is_none());
}
