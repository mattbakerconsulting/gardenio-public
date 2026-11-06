// SPDX-License-Identifier: BUSL-1.1
// Copyright (c) 2026 Matt Baker <fimbaz@gmail.com>

use std::time::Duration;

use crate::driver::{NudgeStep, QuestionAnswer, RespondEncoder};

pub use crate::driver::nudge::SafeNudgeEncoder as ClaudeNudgeEncoder;

/// Encodes prompt responses for Claude Code's terminal input.
pub struct ClaudeRespondEncoder {
    /// Delay between keystrokes in multi-step sequences.
    pub input_delay: Duration,
}

impl Default for ClaudeRespondEncoder {
    fn default() -> Self {
        Self { input_delay: Duration::from_millis(200) }
    }
}

impl RespondEncoder for ClaudeRespondEncoder {
    fn encode_permission(&self, option: u32) -> Vec<NudgeStep> {
        // Number key auto-confirms in Claude's TUI picker — no Enter needed.
        vec![NudgeStep { bytes: format!("{option}").into_bytes(), delay_after: None }]
    }

    fn encode_plan(&self, option: u32, feedback: Option<&str>) -> Vec<NudgeStep> {
        // Number key auto-confirms in Claude's TUI picker — no Enter needed.
        // Options 1-3 are direct selections; the last option is freeform feedback.
        if feedback.is_none() {
            return vec![NudgeStep { bytes: format!("{option}").into_bytes(), delay_after: None }];
        }

        // Feedback: Up Arrow navigates to the text input field, type the
        // feedback text, then press Enter to submit. Each step needs a delay
        // so the TUI can process the input.
        let text = feedback.unwrap_or_default();
        vec![
            NudgeStep { bytes: b"\x1b[A".to_vec(), delay_after: Some(self.input_delay) },
            NudgeStep { bytes: text.as_bytes().to_vec(), delay_after: Some(self.input_delay) },
            NudgeStep { bytes: b"\r".to_vec(), delay_after: None },
        ]
    }

    fn encode_question(
        &self,
        answers: &[QuestionAnswer],
        _total_questions: usize,
    ) -> Vec<NudgeStep> {
        if answers.is_empty() {
            return vec![];
        }

        // All-at-once: multiple answers → emit each answer with delay, then confirm.
        if answers.len() > 1 {
            let mut steps = Vec::new();
            for answer in answers {
                let mut answer_steps = self.encode_single_answer(answer);
                // Ensure a delay after the last step of each answer.
                if let Some(last) = answer_steps.last_mut() {
                    last.delay_after = Some(self.input_delay);
                }
                steps.extend(answer_steps);
            }
            // Final confirm (Enter on the confirm tab).
            steps.push(NudgeStep { bytes: b"\r".to_vec(), delay_after: None });
            return steps;
        }

        // Single answer: digit auto-confirms in the TUI picker, no Enter needed.
        // Freeform text needs Up Arrow → text → Enter with delays between steps.
        self.encode_single_answer(&answers[0])
    }

    fn encode_setup(&self, option: u32) -> Vec<NudgeStep> {
        // Number key auto-confirms in Claude's TUI picker — no Enter needed.
        vec![NudgeStep { bytes: format!("{option}").into_bytes(), delay_after: None }]
    }
}

impl ClaudeRespondEncoder {
    fn encode_single_answer(&self, answer: &QuestionAnswer) -> Vec<NudgeStep> {
        if let Some(n) = answer.option {
            return vec![NudgeStep { bytes: format!("{n}").into_bytes(), delay_after: None }];
        }
        if let Some(ref text) = answer.text {
            // Freeform text: Up Arrow to navigate to text field, type text, Enter.
            return vec![
                NudgeStep { bytes: b"\x1b[A".to_vec(), delay_after: Some(self.input_delay) },
                NudgeStep { bytes: text.as_bytes().to_vec(), delay_after: Some(self.input_delay) },
                NudgeStep { bytes: b"\r".to_vec(), delay_after: None },
            ];
        }
        vec![]
    }
}

#[cfg(test)]
#[path = "encoding_tests.rs"]
mod tests;
