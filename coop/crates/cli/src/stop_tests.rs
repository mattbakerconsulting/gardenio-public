// SPDX-License-Identifier: BUSL-1.1
// Copyright (c) 2026 Matt Baker <fimbaz@gmail.com>

use std::collections::BTreeMap;

use super::*;

#[test]
fn default_stop_config_is_allow() {
    let config = StopConfig::default();
    assert_eq!(config.mode, StopMode::Allow);
    assert!(config.prompt.is_none());
    assert!(config.schema.is_none());
}

#[test]
fn deserialize_allow_mode() -> anyhow::Result<()> {
    let json = r#"{"mode": "allow"}"#;
    let config: StopConfig = serde_json::from_str(json)?;
    assert_eq!(config.mode, StopMode::Allow);
    Ok(())
}

#[test]
fn deserialize_auto_mode_with_prompt() -> anyhow::Result<()> {
    let json = r#"{"mode": "auto", "prompt": "Complete the task before stopping."}"#;
    let config: StopConfig = serde_json::from_str(json)?;
    assert_eq!(config.mode, StopMode::Auto);
    assert_eq!(config.prompt.as_deref(), Some("Complete the task before stopping."));
    Ok(())
}

#[test]
fn deserialize_auto_mode_with_schema() -> anyhow::Result<()> {
    let json = r#"{
        "mode": "auto",
        "prompt": "Signal when done.",
        "schema": {
            "fields": {
                "status": {
                    "required": true,
                    "enum": ["success", "failure"],
                    "descriptions": {
                        "success": "Task completed successfully",
                        "failure": "Task could not be completed"
                    },
                    "description": "Outcome of the task"
                },
                "notes": {
                    "description": "Optional notes"
                }
            }
        }
    }"#;
    let config: StopConfig = serde_json::from_str(json)?;
    assert_eq!(config.mode, StopMode::Auto);
    let schema = config.schema.as_ref().expect("schema should be present");
    assert_eq!(schema.fields.len(), 2);
    let status = &schema.fields["status"];
    assert!(status.required);
    assert_eq!(status.r#enum.as_ref().map(|v| v.len()), Some(2));
    let notes = &schema.fields["notes"];
    assert!(!notes.required);
    Ok(())
}

#[test]
fn deserialize_gate_mode() -> anyhow::Result<()> {
    let json = r#"{"mode": "gate", "prompt": "Run bd decision create to resolve."}"#;
    let config: StopConfig = serde_json::from_str(json)?;
    assert_eq!(config.mode, StopMode::Gate);
    assert_eq!(config.prompt.as_deref(), Some("Run bd decision create to resolve."));
    Ok(())
}

#[test]
fn deserialize_empty_object_is_defaults() -> anyhow::Result<()> {
    let json = "{}";
    let config: StopConfig = serde_json::from_str(json)?;
    assert_eq!(config.mode, StopMode::Allow);
    assert!(config.prompt.is_none());
    assert!(config.schema.is_none());
    Ok(())
}

#[test]
fn generate_block_reason_default_no_schema() {
    let config = StopConfig { mode: StopMode::Auto, prompt: None, schema: None };
    assert_eq!(
        generate_block_reason(&config),
        concat!(
            "Please confirm by running one of:\n",
            "1. Work is complete\n",
            "    `coop send '{\"status\":\"done\",\"message\":\"<message>\"}'`\n",
            "2. Still working, not ready to stop\n",
            "    `coop send '{\"status\":\"continue\",\"message\":\"<message>\"}'`",
        )
    );
}

#[test]
fn generate_block_reason_custom_prompt_no_schema() {
    let config = StopConfig {
        mode: StopMode::Auto,
        prompt: Some("Finish your work first.".to_owned()),
        schema: None,
    };
    assert_eq!(
        generate_block_reason(&config),
        concat!(
            "Finish your work first.\n",
            "Please confirm by running one of:\n",
            "1. Work is complete\n",
            "    `coop send '{\"status\":\"done\",\"message\":\"<message>\"}'`\n",
            "2. Still working, not ready to stop\n",
            "    `coop send '{\"status\":\"continue\",\"message\":\"<message>\"}'`",
        )
    );
}

#[test]
fn generate_block_reason_prompt_with_inline_json() {
    let config = StopConfig {
        mode: StopMode::Auto,
        prompt: Some(r#"Send {"result":"ok"} when done."#.to_owned()),
        schema: None,
    };
    assert_eq!(
        generate_block_reason(&config),
        concat!(
            "Send {\"result\":\"ok\"} when done.\n",
            "Please confirm by running one of:\n",
            "1. Work is complete\n",
            "    `coop send '{\"status\":\"done\",\"message\":\"<message>\"}'`\n",
            "2. Still working, not ready to stop\n",
            "    `coop send '{\"status\":\"continue\",\"message\":\"<message>\"}'`",
        )
    );
}

#[test]
fn generate_block_reason_with_enum_schema_expands_commands() {
    let mut fields = BTreeMap::new();
    fields.insert(
        "status".to_owned(),
        StopSchemaField {
            required: true,
            r#enum: Some(vec!["done".to_owned(), "error".to_owned()]),
            descriptions: Some({
                let mut d = BTreeMap::new();
                d.insert("done".to_owned(), "Work completed".to_owned());
                d.insert("error".to_owned(), "Something went wrong".to_owned());
                d
            }),
            description: Some("Task outcome".to_owned()),
        },
    );
    let config = StopConfig {
        mode: StopMode::Auto,
        prompt: Some("Signal when ready.".to_owned()),
        schema: Some(StopSchema { fields }),
    };
    assert_eq!(
        generate_block_reason(&config),
        concat!(
            "Signal when ready.\n",
            "Please confirm by running one of:\n",
            "1. Work completed\n",
            "    `coop send '{\"status\":\"done\"}'`\n",
            "2. Something went wrong\n",
            "    `coop send '{\"status\":\"error\"}'`",
        )
    );
}

#[test]
fn generate_block_reason_enum_with_extra_fields() {
    // Required/enum fields come first in examples, then optional freeform.
    let mut fields = BTreeMap::new();
    fields.insert(
        "notes".to_owned(),
        StopSchemaField {
            required: false,
            r#enum: None,
            descriptions: None,
            description: Some("Optional notes".to_owned()),
        },
    );
    fields.insert(
        "status".to_owned(),
        StopSchemaField {
            required: true,
            r#enum: Some(vec!["success".to_owned(), "failure".to_owned()]),
            descriptions: Some({
                let mut d = BTreeMap::new();
                d.insert("success".to_owned(), "Task completed successfully".to_owned());
                d.insert("failure".to_owned(), "Task could not be completed".to_owned());
                d
            }),
            description: Some("Outcome of the task".to_owned()),
        },
    );
    let config =
        StopConfig { mode: StopMode::Auto, prompt: None, schema: Some(StopSchema { fields }) };
    assert_eq!(
        generate_block_reason(&config),
        concat!(
            "Please confirm by running one of:\n",
            "1. Task completed successfully\n",
            "    `coop send '{\"status\":\"success\",\"notes\":\"<notes>\"}'`\n",
            "2. Task could not be completed\n",
            "    `coop send '{\"status\":\"failure\",\"notes\":\"<notes>\"}'`",
        )
    );
}

#[test]
fn generate_block_reason_non_enum_schema() {
    let mut fields = BTreeMap::new();
    fields.insert(
        "message".to_owned(),
        StopSchemaField {
            required: true,
            r#enum: None,
            descriptions: None,
            description: Some("A message".to_owned()),
        },
    );
    let config =
        StopConfig { mode: StopMode::Auto, prompt: None, schema: Some(StopSchema { fields }) };
    // No enum field → single example from the schema
    assert_eq!(
        generate_block_reason(&config),
        "When ready to stop, run: `coop send '{\"message\":\"<message>\"}'`"
    );
}

#[test]
fn generate_block_reason_gate_mode_prompt_verbatim() {
    let config = StopConfig {
        mode: StopMode::Gate,
        prompt: Some("Run `bd decision create` to continue.".to_owned()),
        schema: None,
    };
    assert_eq!(generate_block_reason(&config), "Run `bd decision create` to continue.");
}

#[test]
fn generate_block_reason_gate_mode_no_prompt() {
    // Gate mode with no prompt returns empty string (prompt is required at
    // config level, but generate_block_reason is defensive).
    let config = StopConfig { mode: StopMode::Gate, prompt: None, schema: None };
    assert_eq!(generate_block_reason(&config), "");
}

#[test]
fn generate_block_reason_gate_mode_ignores_schema() {
    let mut fields = BTreeMap::new();
    fields.insert(
        "status".to_owned(),
        StopSchemaField {
            required: true,
            r#enum: Some(vec!["done".to_owned()]),
            descriptions: None,
            description: None,
        },
    );
    let config = StopConfig {
        mode: StopMode::Gate,
        prompt: Some("Custom gate prompt.".to_owned()),
        schema: Some(StopSchema { fields }),
    };
    // Schema is ignored in gate mode — prompt is returned verbatim.
    assert_eq!(generate_block_reason(&config), "Custom gate prompt.");
}

#[test]
fn stop_type_as_str() {
    assert_eq!(StopType::Signaled.as_str(), "signaled");
    assert_eq!(StopType::Error.as_str(), "error");
    assert_eq!(StopType::SafetyValve.as_str(), "safety_valve");
    assert_eq!(StopType::Blocked.as_str(), "blocked");
    assert_eq!(StopType::Allowed.as_str(), "allowed");
    assert_eq!(StopType::Rejected.as_str(), "rejected");
}

#[test]
fn stop_config_roundtrip_json() -> anyhow::Result<()> {
    let config =
        StopConfig { mode: StopMode::Auto, prompt: Some("test prompt".to_owned()), schema: None };
    let json = serde_json::to_string(&config)?;
    let parsed: StopConfig = serde_json::from_str(&json)?;
    assert_eq!(parsed.mode, StopMode::Auto);
    assert_eq!(parsed.prompt.as_deref(), Some("test prompt"));
    Ok(())
}

#[test]
fn stop_state_emit_increments_seq() {
    let state = StopState::new(StopConfig::default(), "http://test".to_owned());
    let mut rx = state.stop_tx.subscribe();

    let e1 = state.emit(StopType::Blocked, None, None);
    assert_eq!(e1.seq, 0);
    let e2 = state.emit(StopType::Allowed, None, None);
    assert_eq!(e2.seq, 1);

    // Events should also be received on the broadcast channel.
    let received = rx.try_recv().expect("should receive event");
    assert_eq!(received.seq, 0);
}

// -- validate_signal tests ----------------------------------------------------

#[test]
fn validate_signal_happy_path() {
    let mut fields = BTreeMap::new();
    fields.insert(
        "status".to_owned(),
        StopSchemaField {
            required: true,
            r#enum: Some(vec!["done".to_owned(), "error".to_owned()]),
            descriptions: None,
            description: None,
        },
    );
    let schema = StopSchema { fields };
    let body = serde_json::json!({"status": "done"});
    assert!(validate_signal(&schema, &body).is_ok());
}

#[test]
fn validate_signal_missing_required_field() {
    let mut fields = BTreeMap::new();
    fields.insert(
        "status".to_owned(),
        StopSchemaField { required: true, r#enum: None, descriptions: None, description: None },
    );
    let schema = StopSchema { fields };
    let body = serde_json::json!({});
    let err = validate_signal(&schema, &body).unwrap_err();
    assert!(err.contains("missing required field: status"), "got: {err}");
}

#[test]
fn validate_signal_bad_enum_value() {
    let mut fields = BTreeMap::new();
    fields.insert(
        "status".to_owned(),
        StopSchemaField {
            required: true,
            r#enum: Some(vec!["done".to_owned(), "error".to_owned()]),
            descriptions: None,
            description: None,
        },
    );
    let schema = StopSchema { fields };
    let body = serde_json::json!({"status": "bogus"});
    let err = validate_signal(&schema, &body).unwrap_err();
    assert!(err.contains("bogus"), "got: {err}");
    assert!(err.contains("done"), "got: {err}");
}

#[test]
fn validate_signal_no_schema_passes_anything() {
    // Empty schema = no fields to validate.
    let schema = StopSchema { fields: BTreeMap::new() };
    let body = serde_json::json!({"anything": "goes"});
    assert!(validate_signal(&schema, &body).is_ok());
}

// -- StopState::resolve tests -------------------------------------------------

#[tokio::test]
async fn stop_state_resolve_accepted() {
    let state = StopState::new(StopConfig::default(), "http://test".to_owned());
    let body = serde_json::json!({"ok": true});
    let result = state.resolve(body).await;
    assert!(result.is_ok());
    assert!(state.signaled.load(std::sync::atomic::Ordering::Acquire));
    let stored = state.signal_body.read().await;
    assert!(stored.is_some());
}

#[tokio::test]
async fn stop_state_resolve_auto_default_schema_rejects_bad_status() {
    let config = StopConfig { mode: StopMode::Auto, prompt: None, schema: None };
    let state = StopState::new(config, "http://test".to_owned());
    let body = serde_json::json!({"status": "bogus"});
    let result = state.resolve(body).await;
    assert!(result.is_err());
    let err = result.unwrap_err();
    assert!(err.contains("bogus"), "got: {err}");
    assert!(!state.signaled.load(std::sync::atomic::Ordering::Acquire));
}

#[tokio::test]
async fn stop_state_resolve_auto_default_schema_accepts_done() {
    let config = StopConfig { mode: StopMode::Auto, prompt: None, schema: None };
    let state = StopState::new(config, "http://test".to_owned());
    let body = serde_json::json!({"status": "done", "message": "all good"});
    let result = state.resolve(body).await;
    assert!(result.is_ok());
    assert!(state.signaled.load(std::sync::atomic::Ordering::Acquire));
}

#[tokio::test]
async fn stop_state_resolve_auto_default_schema_accepts_continue() {
    let config = StopConfig { mode: StopMode::Auto, prompt: None, schema: None };
    let state = StopState::new(config, "http://test".to_owned());
    let body = serde_json::json!({"status": "continue"});
    let result = state.resolve(body).await;
    assert!(result.is_ok());
    assert!(state.signaled.load(std::sync::atomic::Ordering::Acquire));
}

#[tokio::test]
async fn stop_state_resolve_auto_default_schema_rejects_missing_status() {
    let config = StopConfig { mode: StopMode::Auto, prompt: None, schema: None };
    let state = StopState::new(config, "http://test".to_owned());
    let body = serde_json::json!({"message": "no status field"});
    let result = state.resolve(body).await;
    assert!(result.is_err());
    let err = result.unwrap_err();
    assert!(err.contains("missing required field: status"), "got: {err}");
}

#[tokio::test]
async fn stop_state_resolve_rejected_with_event() {
    let mut fields = BTreeMap::new();
    fields.insert(
        "status".to_owned(),
        StopSchemaField {
            required: true,
            r#enum: Some(vec!["done".to_owned()]),
            descriptions: None,
            description: None,
        },
    );
    let config =
        StopConfig { mode: StopMode::Auto, prompt: None, schema: Some(StopSchema { fields }) };
    let state = StopState::new(config, "http://test".to_owned());
    let body = serde_json::json!({"status": "bad"});
    let result = state.resolve(body).await;
    assert!(result.is_err());
    // Signal should NOT be set on rejection.
    assert!(!state.signaled.load(std::sync::atomic::Ordering::Acquire));
}
