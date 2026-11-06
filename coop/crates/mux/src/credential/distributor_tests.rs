// SPDX-License-Identifier: BUSL-1.1
// Copyright (c) 2026 Matt Baker <fimbaz@gmail.com>

use super::*;
use crate::state::SessionEntry;
use std::sync::atomic::AtomicU32;
use tokio::sync::RwLock;
use tokio_util::sync::CancellationToken;

fn make_entry(metadata: serde_json::Value) -> SessionEntry {
    SessionEntry {
        id: "test".into(),
        url: "http://localhost:8080".into(),
        auth_token: None,
        metadata,
        registered_at: std::time::Instant::now(),
        cached_screen: RwLock::new(None),
        cached_status: RwLock::new(None),
        cached_usage: RwLock::new(None),
        health_failures: AtomicU32::new(0),
        cancel: CancellationToken::new(),
        ws_bridge: RwLock::new(None),
        assigned_account: RwLock::new(None),
        claimed_bead_title: RwLock::new(None),
        transport: crate::state::SessionTransport::default(),
    }
}

#[test]
fn test_no_metadata_receives_all() {
    let entry = make_entry(serde_json::Value::Null);
    assert!(session_needs_account_metadata(&entry.metadata, "claude-max"));
    assert!(session_needs_account_metadata(&entry.metadata, "openai-api"));
}

#[test]
fn test_empty_profiles_needed_receives_all() {
    let entry = make_entry(serde_json::json!({ "profiles_needed": [] }));
    assert!(session_needs_account_metadata(&entry.metadata, "claude-max"));
}

#[test]
fn test_wildcard_receives_all() {
    let entry = make_entry(serde_json::json!({ "profiles_needed": ["*"] }));
    assert!(session_needs_account_metadata(&entry.metadata, "claude-max"));
    assert!(session_needs_account_metadata(&entry.metadata, "openai-api"));
}

#[test]
fn test_specific_filter() {
    let entry = make_entry(serde_json::json!({ "profiles_needed": ["claude-max"] }));
    assert!(session_needs_account_metadata(&entry.metadata, "claude-max"));
    assert!(!session_needs_account_metadata(&entry.metadata, "openai-api"));
}

#[test]
fn test_case_insensitive() {
    let entry = make_entry(serde_json::json!({ "profiles_needed": ["Claude-Max"] }));
    assert!(session_needs_account_metadata(&entry.metadata, "claude-max"));
    assert!(session_needs_account_metadata(&entry.metadata, "CLAUDE-MAX"));
}

#[test]
fn test_multiple_profiles() {
    let entry = make_entry(serde_json::json!({ "profiles_needed": ["claude-max", "gemini"] }));
    assert!(session_needs_account_metadata(&entry.metadata, "claude-max"));
    assert!(session_needs_account_metadata(&entry.metadata, "gemini"));
    assert!(!session_needs_account_metadata(&entry.metadata, "openai-api"));
}

#[test]
fn test_malformed_profiles_needed() {
    // Not an array → treat as no filter
    let entry = make_entry(serde_json::json!({ "profiles_needed": "claude-max" }));
    assert!(session_needs_account_metadata(&entry.metadata, "anything"));
}
