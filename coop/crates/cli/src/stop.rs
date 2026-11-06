// SPDX-License-Identifier: BUSL-1.1
// Copyright (c) 2026 Matt Baker <fimbaz@gmail.com>

//! Agent-agnostic stop hook configuration and gating logic.
//!
//! The stop hook becomes a gating HTTP call: the hook script `curl`s coop,
//! which returns a verdict (`{}` for allow, `{"decision":"block","reason":"..."}`
//! for block). A signal endpoint lets orchestrators unblock the next stop check.

use std::collections::BTreeMap;
use std::sync::atomic::AtomicBool;

use serde::{Deserialize, Serialize};
use serde_json::Value;
use tokio::sync::{broadcast, RwLock};

/// Top-level stop hook configuration.
#[derive(Debug, Clone, Serialize, Deserialize)]
pub struct StopConfig {
    /// How to handle stop hook calls.
    #[serde(default)]
    pub mode: StopMode,
    /// Custom prompt text included in the block reason.
    #[serde(default, skip_serializing_if = "Option::is_none")]
    pub prompt: Option<String>,
    /// Schema describing the expected signal body fields.
    #[serde(default, skip_serializing_if = "Option::is_none")]
    pub schema: Option<StopSchema>,
}

impl Default for StopConfig {
    fn default() -> Self {
        Self { mode: StopMode::Allow, prompt: None, schema: None }
    }
}

/// Stop hook mode.
#[derive(Debug, Clone, Copy, Default, PartialEq, Eq, Serialize, Deserialize)]
#[serde(rename_all = "snake_case")]
pub enum StopMode {
    /// Always allow the agent to stop (default behavior).
    #[default]
    Allow,
    /// Block stops until a signal is received. Generates actionable `coop send`
    /// instructions in the block reason (batteries-included).
    Auto,
    /// Block stops until a signal is received. Returns the configured `prompt`
    /// verbatim as the block reason. The orchestrator controls resolution.
    /// Requires `prompt` to be set.
    Gate,
}

/// Schema describing expected fields in the signal body.
#[derive(Debug, Clone, Serialize, Deserialize)]
pub struct StopSchema {
    /// Named fields the signal body should contain.
    pub fields: BTreeMap<String, StopSchemaField>,
}

/// A single field in the stop schema.
#[derive(Debug, Clone, Serialize, Deserialize)]
pub struct StopSchemaField {
    /// Whether this field is required.
    #[serde(default)]
    pub required: bool,
    /// Allowed values (if restricted to an enum).
    #[serde(default, skip_serializing_if = "Option::is_none", rename = "enum")]
    pub r#enum: Option<Vec<String>>,
    /// Per-value descriptions for enum fields.
    #[serde(default, skip_serializing_if = "Option::is_none")]
    pub descriptions: Option<BTreeMap<String, String>>,
    /// Field-level description.
    #[serde(default, skip_serializing_if = "Option::is_none")]
    pub description: Option<String>,
}

/// Returns the default schema used in Auto mode when no custom schema is configured.
///
/// Defines two fields:
/// - `status` (required): enum `["done", "continue"]`
/// - `message` (optional): freeform summary
pub fn default_auto_schema() -> StopSchema {
    let mut fields = BTreeMap::new();
    fields.insert(
        "status".to_owned(),
        StopSchemaField {
            required: true,
            r#enum: Some(vec!["done".to_owned(), "continue".to_owned()]),
            descriptions: Some({
                let mut d = BTreeMap::new();
                d.insert("done".to_owned(), "Work is complete".to_owned());
                d.insert("continue".to_owned(), "Still working, not ready to stop".to_owned());
                d
            }),
            description: Some("Task outcome".to_owned()),
        },
    );
    fields.insert(
        "message".to_owned(),
        StopSchemaField {
            required: false,
            r#enum: None,
            descriptions: None,
            description: Some("Summary of completed work or what remains".to_owned()),
        },
    );
    StopSchema { fields }
}

/// A stop verdict event emitted to WebSocket/gRPC consumers.
#[derive(Debug, Clone, Serialize, Deserialize)]
pub struct StopEvent {
    /// What happened at the stop check.
    #[serde(rename = "type")]
    pub r#type: StopType,
    /// Signal body (when type is Signaled).
    #[serde(default, skip_serializing_if = "Option::is_none")]
    pub signal: Option<Value>,
    /// Error details (when type is Error).
    #[serde(default, skip_serializing_if = "Option::is_none")]
    pub error_detail: Option<String>,
    /// Monotonic sequence number.
    pub seq: u64,
}

/// Classification of a stop verdict.
#[derive(Debug, Clone, Copy, PartialEq, Eq, Serialize, Deserialize)]
#[serde(rename_all = "snake_case")]
pub enum StopType {
    /// Signal was received; agent is allowed to stop.
    Signaled,
    /// Agent is in an unrecoverable error state; allowed to stop.
    Error,
    /// Claude's safety valve (`stop_hook_active`) triggered; must allow.
    SafetyValve,
    /// Stop was blocked; agent should continue working.
    Blocked,
    /// Mode is `allow`; agent is always allowed to stop.
    Allowed,
    /// Signal body failed schema validation.
    Rejected,
}

impl StopType {
    /// Wire-format string for this type.
    pub fn as_str(&self) -> &'static str {
        match self {
            Self::Signaled => "signaled",
            Self::Error => "error",
            Self::SafetyValve => "safety_valve",
            Self::Blocked => "blocked",
            Self::Allowed => "allowed",
            Self::Rejected => "rejected",
        }
    }
}

impl std::fmt::Display for StopType {
    fn fmt(&self, f: &mut std::fmt::Formatter<'_>) -> std::fmt::Result {
        f.write_str(self.as_str())
    }
}

/// Runtime state for the stop hook gating system.
pub struct StopState {
    /// Mutable stop config (can be changed at runtime via API).
    pub config: RwLock<StopConfig>,
    /// Fast check: has a signal been received?
    pub signaled: AtomicBool,
    /// The signal body stored by the signal endpoint.
    pub signal_body: RwLock<Option<Value>>,
    /// Broadcast channel for stop events.
    pub stop_tx: broadcast::Sender<StopEvent>,
    /// Precomputed resolve URL for block reason generation.
    pub resolve_url: String,
    /// Monotonic sequence counter for stop events.
    pub stop_seq: std::sync::atomic::AtomicU64,
}

impl StopState {
    /// Create a new `StopState` with the given initial config and resolve URL.
    pub fn new(config: StopConfig, resolve_url: String) -> Self {
        let (stop_tx, _) = broadcast::channel(64);
        Self {
            config: RwLock::new(config),
            signaled: AtomicBool::new(false),
            signal_body: RwLock::new(None),
            stop_tx,
            resolve_url,
            stop_seq: std::sync::atomic::AtomicU64::new(0),
        }
    }

    /// Resolve a stop signal: validate the body against the schema (if any),
    /// store it, and set the signaled flag.
    ///
    /// In Auto mode, validates against the configured schema or the default
    /// schema when none is configured.
    ///
    /// On validation failure, emits a `Rejected` event and returns `Err` with
    /// a descriptive message.
    pub async fn resolve(&self, body: serde_json::Value) -> Result<(), String> {
        let config = self.config.read().await;
        if let Some(ref schema) = config.schema {
            validate_signal(schema, &body)?;
        } else if config.mode == StopMode::Auto {
            let default = default_auto_schema();
            validate_signal(&default, &body)?;
        }
        drop(config);
        *self.signal_body.write().await = Some(body);
        self.signaled.store(true, std::sync::atomic::Ordering::Release);
        Ok(())
    }

    /// Emit a stop event to all subscribers and return it.
    pub fn emit(
        &self,
        r#type: StopType,
        signal: Option<Value>,
        error_detail: Option<String>,
    ) -> StopEvent {
        let seq = self.stop_seq.fetch_add(1, std::sync::atomic::Ordering::Relaxed);
        let event = StopEvent { r#type, signal, error_detail, seq };
        // Ignore send errors (no receivers is fine).
        let _ = self.stop_tx.send(event.clone());
        event
    }
}

impl std::fmt::Debug for StopState {
    fn fmt(&self, f: &mut std::fmt::Formatter<'_>) -> std::fmt::Result {
        f.debug_struct("StopState")
            .field("signaled", &self.signaled.load(std::sync::atomic::Ordering::Relaxed))
            .field("resolve_url", &self.resolve_url)
            .finish()
    }
}

/// Assemble the block reason text from the stop config.
///
/// This is the `reason` field returned in `{"decision":"block","reason":"..."}`.
///
/// - **Auto** mode: generates actionable `coop send` commands the agent can
///   copy-paste (batteries-included).
/// - **Gate** mode: returns the configured `prompt` verbatim. The orchestrator
///   controls resolution.
pub fn generate_block_reason(config: &StopConfig) -> String {
    match config.mode {
        StopMode::Gate => {
            // Gate mode: prompt is required (enforced at config time).
            // Return it verbatim.
            config.prompt.clone().unwrap_or_default()
        }
        _ => generate_auto_block_reason(config),
    }
}

/// Auto-mode block reason: actionable `coop send` instructions.
fn generate_auto_block_reason(config: &StopConfig) -> String {
    let mut parts = Vec::new();

    // Use the configured schema, falling back to the default for Auto mode.
    let default_schema;
    let effective_schema = match config.schema.as_ref() {
        Some(s) => s,
        None => {
            default_schema = default_auto_schema();
            &default_schema
        }
    };

    // Find enum fields eligible for command expansion.
    let primary_enum = {
        let enum_fields: Vec<_> =
            effective_schema.fields.iter().filter(|(_, f)| f.r#enum.is_some()).collect();
        if enum_fields.len() == 1 {
            let (name, field) = enum_fields[0];
            Some((name.clone(), field.clone()))
        } else {
            None
        }
    };

    if let Some((enum_name, enum_field)) = primary_enum {
        // Custom prompt (or default directive)
        if let Some(ref prompt) = config.prompt {
            parts.push(prompt.clone());
        }

        // Expand into one `coop send` per enum value.
        parts.push("Please confirm by running one of:".to_owned());
        let values = enum_field.r#enum.as_deref().unwrap_or_default();
        let descs = enum_field.descriptions.as_ref();
        for (i, v) in values.iter().enumerate() {
            let body = generate_example_body(Some(effective_schema), Some((&enum_name, v)));
            if let Some(vd) = descs.and_then(|d| d.get(v)) {
                parts.push(format!("{}. {vd}", i + 1));
            } else {
                parts.push(format!("{}. {v}", i + 1));
            }
            parts.push(format!("    `coop send '{body}'`"));
        }
    } else {
        // No enum to expand — custom prompt + schema example.
        if let Some(ref prompt) = config.prompt {
            parts.push(prompt.clone());
        }

        let body = generate_example_body(Some(effective_schema), None);
        parts.push(format!("When ready to stop, run: `coop send '{body}'`"));
    }

    parts.join("\n")
}

/// Validate a signal body against a stop schema.
///
/// Checks that required fields are present and enum fields have allowed values.
/// Returns `Err` with a descriptive message on failure.
pub fn validate_signal(schema: &StopSchema, body: &serde_json::Value) -> Result<(), String> {
    let obj = body.as_object();

    for (name, field) in &schema.fields {
        let value = obj.and_then(|o| o.get(name));

        // Required field check.
        if field.required && (value.is_none() || value == Some(&serde_json::Value::Null)) {
            return Err(format!("missing required field: {name}"));
        }

        // Enum validation.
        if let (Some(ref allowed), Some(val)) = (&field.r#enum, value) {
            if let Some(s) = val.as_str() {
                if !allowed.contains(&s.to_owned()) {
                    return Err(format!(
                        "field \"{name}\": value \"{s}\" is not one of: {}",
                        allowed.join(", ")
                    ));
                }
            }
        }
    }

    Ok(())
}

/// Build an example JSON body string from a schema.
///
/// When `enum_override` is provided, that field uses the given value; other
/// fields use their first enum value or a `<name>` placeholder.
fn generate_example_body(
    schema: Option<&StopSchema>,
    enum_override: Option<(&str, &str)>,
) -> String {
    let schema = match schema {
        Some(s) if !s.fields.is_empty() => s,
        _ => return "{}".to_owned(),
    };

    // Emit required/enum fields first so examples read naturally
    // (e.g. `{"status":"done","message":"..."}` not `{"message":"...","status":"done"}`).
    // We build the JSON string manually because serde_json::Map (BTreeMap-backed)
    // re-sorts keys alphabetically, discarding insertion order.
    let ordered = schema
        .fields
        .iter()
        .filter(|(_, f)| f.required || f.r#enum.is_some())
        .chain(schema.fields.iter().filter(|(_, f)| !f.required && f.r#enum.is_none()));

    let mut pairs = Vec::new();
    for (name, field) in ordered {
        let val = if let Some((override_name, override_val)) = enum_override {
            if name == override_name {
                override_val.to_owned()
            } else if let Some(ref values) = field.r#enum {
                values.first().cloned().unwrap_or_default()
            } else {
                format!("<{name}>")
            }
        } else if let Some(ref values) = field.r#enum {
            values.first().cloned().unwrap_or_default()
        } else {
            format!("<{name}>")
        };
        // JSON-escape both name and value.
        let k = serde_json::to_string(name).unwrap_or_else(|_| format!("\"{name}\""));
        let v = serde_json::to_string(&val).unwrap_or_else(|_| format!("\"{val}\""));
        pairs.push(format!("{k}:{v}"));
    }
    format!("{{{}}}", pairs.join(","))
}

#[cfg(test)]
#[path = "stop_tests.rs"]
mod tests;
