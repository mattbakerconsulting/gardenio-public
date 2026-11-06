// SPDX-License-Identifier: BUSL-1.1
// Copyright (c) 2026 Matt Baker <fimbaz@gmail.com>

//! Domain-to-proto conversion functions for gRPC responses.

use super::proto;
use crate::driver::PromptContext;
use crate::event::TransitionEvent;
use crate::screen::ScreenTextSnapshot;
use crate::transport::handler::{extract_error_fields, extract_parked_fields};

/// Convert a domain [`crate::screen::CursorPosition`] to proto.
pub fn cursor_to_proto(c: &crate::screen::CursorPosition) -> proto::CursorPosition {
    proto::CursorPosition { row: c.row as i32, col: c.col as i32 }
}

/// Convert a text-only domain snapshot to proto [`proto::ScreenSnapshot`].
pub fn screen_text_snapshot_to_proto(s: &ScreenTextSnapshot) -> proto::ScreenSnapshot {
    proto::ScreenSnapshot {
        lines: s.lines.clone(),
        cols: s.cols as i32,
        rows: s.rows as i32,
        alt_screen: s.alt_screen,
        cursor: Some(cursor_to_proto(&s.cursor)),
        seq: s.sequence,
    }
}

/// Convert a text-only domain snapshot to a [`proto::GetScreenResponse`],
/// optionally omitting the cursor.
pub fn screen_text_snapshot_to_response(
    s: &ScreenTextSnapshot,
    cursor: bool,
) -> proto::GetScreenResponse {
    proto::GetScreenResponse {
        lines: s.lines.clone(),
        cols: s.cols as i32,
        rows: s.rows as i32,
        alt_screen: s.alt_screen,
        cursor: if cursor { Some(cursor_to_proto(&s.cursor)) } else { None },
        seq: s.sequence,
    }
}

/// Convert a domain [`PromptContext`] to proto.
pub fn prompt_to_proto(p: &PromptContext) -> proto::PromptContext {
    proto::PromptContext {
        r#type: p.kind.as_str().to_owned(),
        tool: p.tool.clone(),
        input: p.input.clone(),
        questions: p
            .questions
            .iter()
            .map(|q| proto::QuestionContext {
                question: q.question.clone(),
                options: q.options.clone(),
            })
            .collect(),
        question_current: p.question_current as u32,
        options: p.options.clone(),
        options_fallback: p.options_fallback,
        subtype: p.subtype.clone(),
        ready: p.ready,
    }
}

/// Convert a domain [`crate::event::ProfileEvent`] to proto [`proto::ProfileEvent`].
pub fn profile_event_to_proto(e: &crate::event::ProfileEvent) -> proto::ProfileEvent {
    match e {
        crate::event::ProfileEvent::ProfileSwitched { from, to } => proto::ProfileEvent {
            event_type: "profile:switched".to_owned(),
            from: from.clone(),
            to: Some(to.clone()),
            profile: None,
            retry_after_secs: None,
        },
        crate::event::ProfileEvent::ProfileExhausted { profile } => proto::ProfileEvent {
            event_type: "profile:exhausted".to_owned(),
            from: None,
            to: None,
            profile: Some(profile.clone()),
            retry_after_secs: None,
        },
        crate::event::ProfileEvent::ProfileRotationExhausted { retry_after_secs } => {
            proto::ProfileEvent {
                event_type: "profile:rotation:exhausted".to_owned(),
                from: None,
                to: None,
                profile: None,
                retry_after_secs: Some(*retry_after_secs),
            }
        }
    }
}

/// Convert a domain [`TransitionEvent`] to proto [`proto::TransitionEvent`].
pub fn transition_to_proto(e: &TransitionEvent) -> proto::TransitionEvent {
    let (error_detail, error_category) = extract_error_fields(&e.next);
    let (parked_reason, resume_at_epoch_ms) = extract_parked_fields(&e.next);
    let cause = if e.cause.is_empty() { None } else { Some(e.cause.clone()) };
    proto::TransitionEvent {
        prev: e.prev.as_str().to_owned(),
        next: e.next.as_str().to_owned(),
        seq: e.seq,
        prompt: e.next.prompt().map(prompt_to_proto),
        error_detail,
        error_category,
        cause,
        last_message: e.last_message.clone(),
        parked_reason,
        resume_at_epoch_ms,
    }
}
