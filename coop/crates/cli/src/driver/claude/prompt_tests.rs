// SPDX-License-Identifier: BUSL-1.1
// Copyright (c) 2026 Matt Baker <fimbaz@gmail.com>

use serde_json::json;

use super::{
    extract_ask_user_context, extract_ask_user_from_tool_input, extract_permission_context,
};

#[test]
fn permission_context_extracts_tool_and_preview() {
    let entry = json!({
        "type": "assistant",
        "message": {
            "content": [{
                "type": "tool_use",
                "name": "Bash",
                "input": { "command": "npm install express" }
            }]
        }
    });
    let ctx = extract_permission_context(&entry);
    assert_eq!(ctx.kind, crate::driver::PromptKind::Permission);
    assert_eq!(ctx.tool.as_deref(), Some("Bash"));
    assert!(ctx.input.is_some());
    let preview = ctx.input.as_deref().unwrap_or("");
    assert!(preview.contains("npm install express"));
}

#[test]
fn permission_context_truncates_large_input() {
    let long_text = "x".repeat(500);
    let entry = json!({
        "type": "assistant",
        "message": {
            "content": [{
                "type": "tool_use",
                "name": "Write",
                "input": { "content": long_text }
            }]
        }
    });
    let ctx = extract_permission_context(&entry);
    let preview = ctx.input.unwrap_or_default();
    assert!(preview.len() <= 210); // 200 + "..."
    assert!(preview.ends_with("..."));
}

#[test]
fn ask_user_context_extracts_question_and_options() {
    let block = json!({
        "type": "tool_use",
        "name": "AskUserQuestion",
        "input": {
            "questions": [{
                "question": "Which database should we use?",
                "options": [
                    { "label": "PostgreSQL", "description": "Relational" },
                    { "label": "SQLite", "description": "Embedded" },
                    { "label": "MySQL", "description": "Popular" }
                ]
            }]
        }
    });
    let ctx = extract_ask_user_context(&block);
    assert_eq!(ctx.kind, crate::driver::PromptKind::Question);
    assert_eq!(ctx.questions.len(), 1);
    assert_eq!(ctx.questions[0].question, "Which database should we use?");
    assert_eq!(ctx.questions[0].options, vec!["PostgreSQL", "SQLite", "MySQL"]);
}

#[test]
fn ask_user_context_with_no_options() {
    let block = json!({
        "type": "tool_use",
        "name": "AskUserQuestion",
        "input": {
            "questions": [{
                "question": "What do you want to do?"
            }]
        }
    });
    let ctx = extract_ask_user_context(&block);
    assert_eq!(ctx.questions.len(), 1);
    assert_eq!(ctx.questions[0].question, "What do you want to do?");
    assert!(ctx.questions[0].options.is_empty());
}

#[test]
fn ask_user_context_with_empty_input() {
    let block = json!({
        "type": "tool_use",
        "name": "AskUserQuestion",
        "input": {}
    });
    let ctx = extract_ask_user_context(&block);
    assert_eq!(ctx.kind, crate::driver::PromptKind::Question);
    assert!(ctx.questions.is_empty());
}

#[test]
fn ask_user_from_tool_input_extracts_question_and_options() {
    let tool_input = json!({
        "questions": [{
            "question": "Which framework?",
            "options": [
                { "label": "React", "description": "Popular" },
                { "label": "Vue", "description": "Progressive" }
            ]
        }]
    });
    let ctx = extract_ask_user_from_tool_input(Some(&tool_input));
    assert_eq!(ctx.kind, crate::driver::PromptKind::Question);
    assert_eq!(ctx.questions.len(), 1);
    assert_eq!(ctx.questions[0].question, "Which framework?");
    assert_eq!(ctx.questions[0].options, vec!["React", "Vue"]);
}

#[test]
fn ask_user_from_tool_input_with_none() {
    let ctx = extract_ask_user_from_tool_input(None);
    assert_eq!(ctx.kind, crate::driver::PromptKind::Question);
    assert!(ctx.questions.is_empty());
}

#[test]
fn ask_user_extracts_all_questions() {
    let tool_input = json!({
        "questions": [
            {
                "question": "Which database?",
                "options": [
                    { "label": "PostgreSQL" },
                    { "label": "SQLite" }
                ]
            },
            {
                "question": "Which framework?",
                "options": [
                    { "label": "Axum" },
                    { "label": "Actix" },
                    { "label": "Rocket" }
                ]
            }
        ]
    });
    let ctx = extract_ask_user_from_tool_input(Some(&tool_input));

    // All questions parsed into the questions vec.
    assert_eq!(ctx.questions.len(), 2);
    assert_eq!(ctx.questions[0].question, "Which database?");
    assert_eq!(ctx.questions[0].options, vec!["PostgreSQL", "SQLite"]);
    assert_eq!(ctx.questions[1].question, "Which framework?");
    assert_eq!(ctx.questions[1].options, vec!["Axum", "Actix", "Rocket"]);

    assert_eq!(ctx.question_current, 0);
}

#[test]
fn ask_user_single_question_populates_questions_vec() {
    let tool_input = json!({
        "questions": [{
            "question": "Which DB?",
            "options": [
                { "label": "Postgres" },
                { "label": "SQLite" }
            ]
        }]
    });
    let ctx = extract_ask_user_from_tool_input(Some(&tool_input));
    assert_eq!(ctx.questions.len(), 1);
    assert_eq!(ctx.questions[0].question, "Which DB?");
}

#[test]
fn missing_fields_produce_sensible_defaults() {
    // Permission context with no message field
    let entry = json!({});
    let ctx = extract_permission_context(&entry);
    assert_eq!(ctx.kind, crate::driver::PromptKind::Permission);
    assert!(ctx.tool.is_none());
    assert!(ctx.input.is_none());

    // AskUser context with no input field
    let block = json!({ "type": "tool_use", "name": "AskUserQuestion" });
    let ctx = extract_ask_user_context(&block);
    assert!(ctx.questions.is_empty());
}
