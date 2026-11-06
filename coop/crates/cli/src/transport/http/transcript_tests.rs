// SPDX-License-Identifier: BUSL-1.1
// Copyright (c) 2026 Matt Baker <fimbaz@gmail.com>

use std::sync::Arc;

use axum::http::StatusCode;

use crate::test_support::{AnyhowExt, StoreBuilder, StoreCtx};
use crate::transcript::TranscriptState;
use crate::transport::build_router;

/// Create a `Store` with a real `TranscriptState` backed by a temp dir and session log.
/// Returns `(StoreCtx, TempDir)` — the `TempDir` guard must be held for the test lifetime.
fn transcript_state() -> (StoreCtx, tempfile::TempDir) {
    let tmp = tempfile::tempdir().expect("create tempdir");
    let transcripts_dir = tmp.path().join("transcripts");
    let session_log = tmp.path().join("session.jsonl");
    std::fs::write(&session_log, "").expect("create session log");

    let ts = Arc::new(
        TranscriptState::new(transcripts_dir, Some(session_log)).expect("create transcript state"),
    );
    let ts = StoreBuilder::new().child_pid(1234).transcript(ts).build();
    (ts, tmp)
}

#[tokio::test]
async fn list_transcripts_empty() -> anyhow::Result<()> {
    let (StoreCtx { store: state, .. }, _tmp) = transcript_state();
    let app = build_router(state);
    let server = axum_test::TestServer::new(app).anyhow()?;

    let resp = server.get("/api/v1/transcripts").await;
    resp.assert_status(StatusCode::OK);
    let body: serde_json::Value = serde_json::from_str(&resp.text())?;
    assert_eq!(body["transcripts"].as_array().map(|a| a.len()), Some(0));
    Ok(())
}

#[tokio::test]
async fn list_transcripts_after_save() -> anyhow::Result<()> {
    let (StoreCtx { store: state, .. }, _tmp) = transcript_state();

    // Write some content to the session log so the snapshot has data.
    let log_path = _tmp.path().join("session.jsonl");
    std::fs::write(&log_path, "{\"type\":\"message\"}\n{\"type\":\"tool\"}\n")?;

    state.transcript.save_snapshot().await?;

    let app = build_router(state);
    let server = axum_test::TestServer::new(app).anyhow()?;

    let resp = server.get("/api/v1/transcripts").await;
    resp.assert_status(StatusCode::OK);
    let body: serde_json::Value = serde_json::from_str(&resp.text())?;
    let transcripts = body["transcripts"].as_array().expect("transcripts array");
    assert_eq!(transcripts.len(), 1);
    assert_eq!(transcripts[0]["number"], 1);
    assert_eq!(transcripts[0]["line_count"], 2);
    assert!(transcripts[0]["byte_size"].as_u64().unwrap_or(0) > 0);
    Ok(())
}

#[tokio::test]
async fn get_transcript_returns_content() -> anyhow::Result<()> {
    let (StoreCtx { store: state, .. }, _tmp) = transcript_state();

    let log_path = _tmp.path().join("session.jsonl");
    let log_content = "{\"type\":\"message\",\"text\":\"hello\"}\n";
    std::fs::write(&log_path, log_content)?;

    state.transcript.save_snapshot().await?;

    let app = build_router(state);
    let server = axum_test::TestServer::new(app).anyhow()?;

    let resp = server.get("/api/v1/transcripts/1").await;
    resp.assert_status(StatusCode::OK);
    let body: serde_json::Value = serde_json::from_str(&resp.text())?;
    assert_eq!(body["number"], 1);
    assert_eq!(body["content"].as_str(), Some(log_content));
    Ok(())
}

#[tokio::test]
async fn get_transcript_not_found() -> anyhow::Result<()> {
    let (StoreCtx { store: state, .. }, _tmp) = transcript_state();
    let app = build_router(state);
    let server = axum_test::TestServer::new(app).anyhow()?;

    let resp = server.get("/api/v1/transcripts/99").await;
    resp.assert_status(StatusCode::BAD_REQUEST);
    let body = resp.text();
    assert!(body.contains("not found"), "body: {body}");
    Ok(())
}

#[tokio::test]
async fn catchup_returns_transcripts_and_live_lines() -> anyhow::Result<()> {
    let (StoreCtx { store: state, .. }, _tmp) = transcript_state();

    let log_path = _tmp.path().join("session.jsonl");
    std::fs::write(&log_path, "{\"turn\":1}\n")?;
    state.transcript.save_snapshot().await?;

    // Write more to the session log after the snapshot.
    std::fs::write(&log_path, "{\"turn\":1}\n{\"turn\":2}\n{\"turn\":3}\n")?;

    let app = build_router(state);
    let server = axum_test::TestServer::new(app).anyhow()?;

    let resp = server.get("/api/v1/transcripts/catchup?since_transcript=0&since_line=0").await;
    resp.assert_status(StatusCode::OK);
    let body: serde_json::Value = serde_json::from_str(&resp.text())?;

    let transcripts = body["transcripts"].as_array().expect("transcripts array");
    assert_eq!(transcripts.len(), 1, "should include the one transcript");
    assert_eq!(transcripts[0]["number"], 1);

    let live_lines = body["live_lines"].as_array().expect("live_lines array");
    assert_eq!(live_lines.len(), 3, "should include all current live lines");
    Ok(())
}

#[tokio::test]
async fn catchup_with_cursor_skips_old() -> anyhow::Result<()> {
    let (StoreCtx { store: state, .. }, _tmp) = transcript_state();

    let log_path = _tmp.path().join("session.jsonl");
    std::fs::write(&log_path, "{\"turn\":1}\n")?;
    state.transcript.save_snapshot().await?;

    // Write 3 lines to session log; ask to skip the first 2.
    std::fs::write(&log_path, "{\"line\":1}\n{\"line\":2}\n{\"line\":3}\n")?;

    let app = build_router(state);
    let server = axum_test::TestServer::new(app).anyhow()?;

    let resp = server.get("/api/v1/transcripts/catchup?since_transcript=1&since_line=2").await;
    resp.assert_status(StatusCode::OK);
    let body: serde_json::Value = serde_json::from_str(&resp.text())?;

    // since_transcript=1 means only transcripts with number > 1.
    let transcripts = body["transcripts"].as_array().expect("transcripts array");
    assert_eq!(transcripts.len(), 0, "should skip transcript 1");

    // since_line=2 means skip first 2 lines.
    let live_lines = body["live_lines"].as_array().expect("live_lines array");
    assert_eq!(live_lines.len(), 1, "should have only the 3rd line");
    assert!(live_lines[0].as_str().unwrap_or("").contains("\"line\":3"));
    Ok(())
}

#[tokio::test]
async fn hooks_start_compact_triggers_snapshot() -> anyhow::Result<()> {
    let (StoreCtx { store: state, .. }, _tmp) = transcript_state();

    let log_path = _tmp.path().join("session.jsonl");
    std::fs::write(&log_path, "{\"msg\":\"before compact\"}\n")?;

    let app = build_router(state.clone());
    let server = axum_test::TestServer::new(app).anyhow()?;

    server
        .post("/api/v1/hooks/start")
        .json(&serde_json::json!({"event": "start", "data": {"source": "compact"}}))
        .await;

    // The snapshot is spawned asynchronously — wait briefly for it.
    tokio::time::sleep(std::time::Duration::from_millis(100)).await;

    let list = state.transcript.list().await;
    assert_eq!(list.len(), 1, "compact source should trigger a transcript snapshot");
    Ok(())
}

#[tokio::test]
async fn hooks_start_non_compact_does_not_trigger_snapshot() -> anyhow::Result<()> {
    let (StoreCtx { store: state, .. }, _tmp) = transcript_state();

    let log_path = _tmp.path().join("session.jsonl");
    std::fs::write(&log_path, "{\"msg\":\"session start\"}\n")?;

    let app = build_router(state.clone());
    let server = axum_test::TestServer::new(app).anyhow()?;

    server
        .post("/api/v1/hooks/start")
        .json(&serde_json::json!({"event": "start", "data": {"source": "clear"}}))
        .await;

    tokio::time::sleep(std::time::Duration::from_millis(50)).await;

    let list = state.transcript.list().await;
    assert_eq!(list.len(), 0, "non-compact source should not trigger a snapshot");
    Ok(())
}
