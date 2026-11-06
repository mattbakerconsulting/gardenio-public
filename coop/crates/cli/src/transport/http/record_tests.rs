// SPDX-License-Identifier: BUSL-1.1
// Copyright (c) 2026 Matt Baker <fimbaz@gmail.com>

use std::sync::Arc;

use axum::http::StatusCode;

use crate::record::RecordingState;
use crate::screen::{CursorPosition, ScreenSnapshot};
use crate::test_support::{AnyhowExt, StoreBuilder, StoreCtx};
use crate::transport::build_router;

/// Create a `Store` with a file-backed `RecordingState`.
/// Returns `(StoreCtx, TempDir)` — hold `TempDir` for test lifetime.
fn recording_store() -> (StoreCtx, tempfile::TempDir) {
    let tmp = tempfile::tempdir().expect("create tempdir");
    let record = Arc::new(RecordingState::new(Some(tmp.path()), 80, 24));
    let ctx = StoreBuilder::new().child_pid(1234).record(record).build();
    (ctx, tmp)
}

fn test_snapshot() -> ScreenSnapshot {
    ScreenSnapshot {
        lines: vec!["hello world".to_owned()],
        ansi: vec!["hello world".to_owned()],
        cols: 80,
        rows: 24,
        alt_screen: false,
        cursor: CursorPosition { row: 0, col: 11 },
        sequence: 0,
    }
}

#[tokio::test]
async fn get_recording_status_disabled() -> anyhow::Result<()> {
    let (StoreCtx { store, .. }, _tmp) = recording_store();
    let app = build_router(store);
    let server = axum_test::TestServer::new(app).anyhow()?;

    let resp = server.get("/api/v1/recording").await;
    resp.assert_status(StatusCode::OK);
    let body: serde_json::Value = serde_json::from_str(&resp.text())?;
    assert_eq!(body["enabled"], false);
    assert_eq!(body["entries"], 0);
    Ok(())
}

#[tokio::test]
async fn put_recording_enables() -> anyhow::Result<()> {
    let (StoreCtx { store, .. }, _tmp) = recording_store();
    let app = build_router(store);
    let server = axum_test::TestServer::new(app).anyhow()?;

    let resp = server.put("/api/v1/recording").json(&serde_json::json!({"enabled": true})).await;
    resp.assert_status(StatusCode::OK);
    let body: serde_json::Value = serde_json::from_str(&resp.text())?;
    assert_eq!(body["enabled"], true);
    assert!(body["path"].as_str().is_some());
    Ok(())
}

#[tokio::test]
async fn put_recording_disables() -> anyhow::Result<()> {
    let (StoreCtx { store, .. }, _tmp) = recording_store();
    let app = build_router(store.clone());
    let server = axum_test::TestServer::new(app).anyhow()?;

    // Enable first.
    server.put("/api/v1/recording").json(&serde_json::json!({"enabled": true})).await;

    // Then disable.
    let resp = server.put("/api/v1/recording").json(&serde_json::json!({"enabled": false})).await;
    resp.assert_status(StatusCode::OK);
    let body: serde_json::Value = serde_json::from_str(&resp.text())?;
    assert_eq!(body["enabled"], false);
    Ok(())
}

#[tokio::test]
async fn catchup_empty_when_no_entries() -> anyhow::Result<()> {
    let (StoreCtx { store, .. }, _tmp) = recording_store();
    let app = build_router(store);
    let server = axum_test::TestServer::new(app).anyhow()?;

    let resp = server.get("/api/v1/recording/catchup").await;
    resp.assert_status(StatusCode::OK);
    let body: serde_json::Value = serde_json::from_str(&resp.text())?;
    assert_eq!(body["entries"].as_array().map(|a| a.len()), Some(0));
    Ok(())
}

#[tokio::test]
async fn catchup_returns_entries_after_push() -> anyhow::Result<()> {
    let (StoreCtx { store, .. }, _tmp) = recording_store();

    // Enable recording and push an entry.
    store.record.enable().await;
    store
        .record
        .push("state", serde_json::json!({"prev": "Starting", "next": "Working"}), test_snapshot())
        .await;

    let app = build_router(store);
    let server = axum_test::TestServer::new(app).anyhow()?;

    let resp = server.get("/api/v1/recording/catchup?since_seq=0").await;
    resp.assert_status(StatusCode::OK);
    let body: serde_json::Value = serde_json::from_str(&resp.text())?;
    let entries = body["entries"].as_array().expect("entries array");
    assert_eq!(entries.len(), 1);
    assert_eq!(entries[0]["kind"], "state");
    assert_eq!(entries[0]["seq"], 1);
    Ok(())
}

#[tokio::test]
async fn catchup_filters_by_since_seq() -> anyhow::Result<()> {
    let (StoreCtx { store, .. }, _tmp) = recording_store();

    store.record.enable().await;
    store.record.push("state", serde_json::json!({"seq": 1}), test_snapshot()).await;
    store.record.push("hook", serde_json::json!({"seq": 2}), test_snapshot()).await;
    store.record.push("state", serde_json::json!({"seq": 3}), test_snapshot()).await;

    let app = build_router(store);
    let server = axum_test::TestServer::new(app).anyhow()?;

    // Skip the first 2 entries.
    let resp = server.get("/api/v1/recording/catchup?since_seq=2").await;
    resp.assert_status(StatusCode::OK);
    let body: serde_json::Value = serde_json::from_str(&resp.text())?;
    let entries = body["entries"].as_array().expect("entries array");
    assert_eq!(entries.len(), 1);
    assert_eq!(entries[0]["seq"], 3);
    Ok(())
}

#[tokio::test]
async fn download_returns_404_when_no_recording() -> anyhow::Result<()> {
    // Use a store without a session dir (no file-backed recording).
    let ctx = StoreBuilder::new().child_pid(1234).build();
    let app = build_router(ctx.store);
    let server = axum_test::TestServer::new(app).anyhow()?;

    let resp = server.get("/api/v1/recording/download").await;
    resp.assert_status(StatusCode::NOT_FOUND);
    Ok(())
}

#[tokio::test]
async fn download_returns_recording_file() -> anyhow::Result<()> {
    let (StoreCtx { store, .. }, _tmp) = recording_store();

    store.record.enable().await;
    store.record.push("state", serde_json::json!({"test": true}), test_snapshot()).await;

    let app = build_router(store);
    let server = axum_test::TestServer::new(app).anyhow()?;

    let resp = server.get("/api/v1/recording/download").await;
    resp.assert_status(StatusCode::OK);
    let body = resp.text();
    // Should contain at least the header line and one entry.
    let lines: Vec<&str> = body.lines().collect();
    assert!(lines.len() >= 2, "expected header + entry, got {} lines", lines.len());
    // First line should be a header with version field.
    let header: serde_json::Value = serde_json::from_str(lines[0])?;
    assert_eq!(header["version"], 1);
    Ok(())
}

#[tokio::test]
async fn get_status_reflects_enable_state() -> anyhow::Result<()> {
    let (StoreCtx { store, .. }, _tmp) = recording_store();
    let app = build_router(store.clone());
    let server = axum_test::TestServer::new(app).anyhow()?;

    // Initially disabled.
    let resp = server.get("/api/v1/recording").await;
    let body: serde_json::Value = serde_json::from_str(&resp.text())?;
    assert_eq!(body["enabled"], false);

    // Enable via PUT.
    server.put("/api/v1/recording").json(&serde_json::json!({"enabled": true})).await;

    // Push an entry so we can verify entries count too.
    store.record.push("state", serde_json::json!({"test": true}), test_snapshot()).await;

    // Status should now show enabled with 1 entry.
    let resp = server.get("/api/v1/recording").await;
    let body: serde_json::Value = serde_json::from_str(&resp.text())?;
    assert_eq!(body["enabled"], true);
    assert_eq!(body["entries"], 1);
    Ok(())
}
