// SPDX-License-Identifier: BUSL-1.1
// Copyright (c) 2026 Matt Baker <fimbaz@gmail.com>

use std::sync::Arc;

use axum::http::StatusCode;

use crate::driver::AgentState;
use crate::event::{RawHookEvent, TransitionEvent};
use crate::event_log::EventLog;
use crate::test_support::{AnyhowExt, StoreBuilder, StoreCtx};
use crate::transport::build_router;

/// Create a `Store` with a file-backed `EventLog`.
fn event_log_store() -> (StoreCtx, tempfile::TempDir) {
    let tmp = tempfile::tempdir().expect("create tempdir");
    let event_log = Arc::new(EventLog::new(Some(tmp.path())));
    let ctx = StoreBuilder::new().child_pid(1234).event_log(event_log).build();
    (ctx, tmp)
}

#[tokio::test]
async fn catchup_empty_when_no_events() -> anyhow::Result<()> {
    let (StoreCtx { store, .. }, _tmp) = event_log_store();
    let app = build_router(store);
    let server = axum_test::TestServer::new(app).anyhow()?;

    let resp = server.get("/api/v1/events/catchup").await;
    resp.assert_status(StatusCode::OK);
    let body: serde_json::Value = serde_json::from_str(&resp.text())?;
    assert_eq!(body["state_events"].as_array().map(|a| a.len()), Some(0));
    assert_eq!(body["hook_events"].as_array().map(|a| a.len()), Some(0));
    Ok(())
}

#[tokio::test]
async fn catchup_returns_state_events() -> anyhow::Result<()> {
    let (StoreCtx { store, .. }, _tmp) = event_log_store();

    store
        .event_log
        .push_transition(&TransitionEvent {
            prev: AgentState::Starting,
            next: AgentState::Working,
            seq: 1,
            cause: "hook".into(),
            last_message: None,
        })
        .await;
    store
        .event_log
        .push_transition(&TransitionEvent {
            prev: AgentState::Working,
            next: AgentState::Idle,
            seq: 2,
            cause: "hook".into(),
            last_message: Some("done".into()),
        })
        .await;

    let app = build_router(store);
    let server = axum_test::TestServer::new(app).anyhow()?;

    let resp = server.get("/api/v1/events/catchup").await;
    resp.assert_status(StatusCode::OK);
    let body: serde_json::Value = serde_json::from_str(&resp.text())?;
    let events = body["state_events"].as_array().expect("state_events array");
    assert_eq!(events.len(), 2);
    assert_eq!(events[0]["prev"], "starting");
    assert_eq!(events[0]["next"], "working");
    assert_eq!(events[1]["last_message"], "done");
    Ok(())
}

#[tokio::test]
async fn catchup_returns_hook_events() -> anyhow::Result<()> {
    let (StoreCtx { store, .. }, _tmp) = event_log_store();

    // Push two hooks: hook_seq starts at 0, and catchup_hooks(0) returns seq > 0.
    store
        .event_log
        .push_hook(&RawHookEvent {
            json: serde_json::json!({"event": "start", "source": "initial"}),
        })
        .await;
    store
        .event_log
        .push_hook(&RawHookEvent {
            json: serde_json::json!({"event": "stop", "source": "compact"}),
        })
        .await;

    let app = build_router(store);
    let server = axum_test::TestServer::new(app).anyhow()?;

    // Default since_hook_seq=0 filters out hook_seq=0, returns hook_seq=1.
    let resp = server.get("/api/v1/events/catchup").await;
    resp.assert_status(StatusCode::OK);
    let body: serde_json::Value = serde_json::from_str(&resp.text())?;
    let hooks = body["hook_events"].as_array().expect("hook_events array");
    assert_eq!(hooks.len(), 1);
    assert_eq!(hooks[0]["json"]["event"], "stop");
    Ok(())
}

#[tokio::test]
async fn catchup_filters_by_since_seq() -> anyhow::Result<()> {
    let (StoreCtx { store, .. }, _tmp) = event_log_store();

    for i in 1..=3 {
        store
            .event_log
            .push_transition(&TransitionEvent {
                prev: AgentState::Working,
                next: AgentState::Idle,
                seq: i,
                cause: "hook".into(),
                last_message: None,
            })
            .await;
    }

    let app = build_router(store);
    let server = axum_test::TestServer::new(app).anyhow()?;

    // Skip events with seq <= 2.
    let resp = server.get("/api/v1/events/catchup?since_seq=2").await;
    resp.assert_status(StatusCode::OK);
    let body: serde_json::Value = serde_json::from_str(&resp.text())?;
    let events = body["state_events"].as_array().expect("state_events array");
    assert_eq!(events.len(), 1);
    assert_eq!(events[0]["seq"], 3);
    Ok(())
}

#[tokio::test]
async fn catchup_filters_hooks_by_since_hook_seq() -> anyhow::Result<()> {
    let (StoreCtx { store, .. }, _tmp) = event_log_store();

    for _ in 0..3 {
        store
            .event_log
            .push_hook(&RawHookEvent { json: serde_json::json!({"event": "test"}) })
            .await;
    }

    let app = build_router(store);
    let server = axum_test::TestServer::new(app).anyhow()?;

    // Skip hook events with hook_seq <= 1 (0-indexed, so first two are seq 0 and 1).
    let resp = server.get("/api/v1/events/catchup?since_hook_seq=1").await;
    resp.assert_status(StatusCode::OK);
    let body: serde_json::Value = serde_json::from_str(&resp.text())?;
    let hooks = body["hook_events"].as_array().expect("hook_events array");
    assert_eq!(hooks.len(), 1);
    Ok(())
}

#[tokio::test]
async fn catchup_combined_state_and_hook_events() -> anyhow::Result<()> {
    let (StoreCtx { store, .. }, _tmp) = event_log_store();

    store
        .event_log
        .push_transition(&TransitionEvent {
            prev: AgentState::Starting,
            next: AgentState::Working,
            seq: 1,
            cause: "hook".into(),
            last_message: None,
        })
        .await;
    // Push two hooks so that one survives the default since_hook_seq=0 filter.
    store
        .event_log
        .push_hook(&RawHookEvent {
            json: serde_json::json!({"event": "start", "source": "initial"}),
        })
        .await;
    store
        .event_log
        .push_hook(&RawHookEvent {
            json: serde_json::json!({"event": "stop", "source": "compact"}),
        })
        .await;

    let app = build_router(store);
    let server = axum_test::TestServer::new(app).anyhow()?;

    let resp = server.get("/api/v1/events/catchup").await;
    resp.assert_status(StatusCode::OK);
    let body: serde_json::Value = serde_json::from_str(&resp.text())?;
    assert_eq!(body["state_events"].as_array().map(|a| a.len()), Some(1));
    assert_eq!(body["hook_events"].as_array().map(|a| a.len()), Some(1));
    Ok(())
}
