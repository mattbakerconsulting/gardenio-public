// SPDX-License-Identifier: BUSL-1.1
// Copyright (c) 2026 Matt Baker <fimbaz@gmail.com>

use serde_json::Value;

use crate::driver::{PromptContext, PromptKind, QuestionContext};

/// Extract permission prompt context from a session log entry.
///
/// Finds the last `tool_use` block in the message and extracts the tool
/// name and a truncated preview of its input.
pub fn extract_permission_context(json: &Value) -> PromptContext {
    let tool_use = find_last_tool_use(json);
    let (tool, input) = match tool_use {
        Some(block) => {
            let tool = block.get("name").and_then(|v| v.as_str()).map(String::from);
            let preview = block.get("input").and_then(summarize_tool_input);
            (tool, preview)
        }
        None => (None, None),
    };

    let mut ctx = PromptContext::new(PromptKind::Permission);
    ctx.tool = tool;
    ctx.input = input;
    ctx
}

/// Extract question context from an `AskUserQuestion` tool_use block.
///
/// Handles Claude's tool input format where questions are in a
/// `questions` array with `question` text and `options[].label`.
pub fn extract_ask_user_context(block: &Value) -> PromptContext {
    extract_ask_user_from_tool_input(block.get("input"))
}

/// Extract question context directly from the tool input value.
///
/// Used by the `ToolBefore` hook path where `tool_input` is provided
/// directly (not wrapped in a `tool_use` block).
///
/// Parses all questions from `input.questions[]` into the `questions` vec.
/// Top-level `question`/`options` fields are populated from `questions[0]`
/// for backwards compatibility.
pub fn extract_ask_user_from_tool_input(input: Option<&Value>) -> PromptContext {
    let questions_arr = input.and_then(|i| i.get("questions")).and_then(|q| q.as_array());

    let questions: Vec<QuestionContext> = questions_arr
        .map(|arr| {
            arr.iter()
                .filter_map(|q| {
                    let question = q.get("question")?.as_str()?.to_string();
                    let options = q
                        .get("options")
                        .and_then(|v| v.as_array())
                        .map(|opts| {
                            opts.iter()
                                .filter_map(|v| {
                                    v.get("label")
                                        .and_then(|l| l.as_str())
                                        .or_else(|| v.as_str())
                                        .map(String::from)
                                })
                                .collect()
                        })
                        .unwrap_or_default();
                    Some(QuestionContext { question, options })
                })
                .collect()
        })
        .unwrap_or_default();

    PromptContext::new(PromptKind::Question)
        .with_tool("AskUserQuestion")
        .with_questions(questions)
        .with_ready()
}

/// Truncate tool input JSON to a ~200 character preview string.
fn summarize_tool_input(input: &Value) -> Option<String> {
    let s = serde_json::to_string(input).ok()?;
    if s.len() <= 200 {
        return Some(s);
    }

    // Find a safe truncation point that doesn't split multi-byte chars
    let mut end = 200;
    while end > 0 && !s.is_char_boundary(end) {
        end -= 1;
    }
    let mut result = s[..end].to_string();
    result.push_str("...");
    Some(result)
}

/// Find the last `tool_use` block in a message's content array.
fn find_last_tool_use(json: &Value) -> Option<&Value> {
    let content = json.get("message")?.get("content")?.as_array()?;
    content
        .iter()
        .rev()
        .find(|block| block.get("type").and_then(|v| v.as_str()) == Some("tool_use"))
}

#[cfg(test)]
#[path = "prompt_tests.rs"]
mod tests;
