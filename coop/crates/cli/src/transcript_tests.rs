// SPDX-License-Identifier: BUSL-1.1
// Copyright (c) 2026 Matt Baker <fimbaz@gmail.com>

use super::*;

/// Create a temp dir and return the transcripts dir + session log path.
fn setup_dirs() -> (tempfile::TempDir, PathBuf, PathBuf) {
    let tmp = tempfile::tempdir().expect("create temp dir");
    let transcripts_dir = tmp.path().join("transcripts");
    let log_path = tmp.path().join("session.jsonl");
    (tmp, transcripts_dir, log_path)
}

#[tokio::test]
async fn new_creates_directory() -> anyhow::Result<()> {
    let (_tmp, transcripts_dir, log_path) = setup_dirs();
    let _state = TranscriptState::new(transcripts_dir.clone(), Some(log_path))?;
    assert!(transcripts_dir.exists());
    Ok(())
}

#[tokio::test]
async fn save_and_list() -> anyhow::Result<()> {
    let (_tmp, transcripts_dir, log_path) = setup_dirs();
    std::fs::write(&log_path, "{\"type\":\"message\"}\n{\"type\":\"tool\"}\n")?;

    let state = TranscriptState::new(transcripts_dir.clone(), Some(log_path))?;
    let mut rx = state.transcript_tx.subscribe();

    let meta = state.save_snapshot().await?;
    assert_eq!(meta.number, 1);
    assert_eq!(meta.line_count, 2);
    assert!(meta.byte_size > 0);

    let list = state.list().await;
    assert_eq!(list.len(), 1);
    assert_eq!(list[0].number, 1);

    // Verify broadcast event was sent.
    let event = rx.try_recv()?;
    assert_eq!(event.number, 1);
    assert_eq!(event.line_count, 2);
    assert_eq!(event.seq, 0);

    Ok(())
}

#[tokio::test]
async fn save_increments_number() -> anyhow::Result<()> {
    let (_tmp, transcripts_dir, log_path) = setup_dirs();
    std::fs::write(&log_path, "line1\n")?;

    let state = TranscriptState::new(transcripts_dir, Some(log_path))?;

    let m1 = state.save_snapshot().await?;
    let m2 = state.save_snapshot().await?;
    assert_eq!(m1.number, 1);
    assert_eq!(m2.number, 2);

    let list = state.list().await;
    assert_eq!(list.len(), 2);

    Ok(())
}

#[tokio::test]
async fn get_content() -> anyhow::Result<()> {
    let (_tmp, transcripts_dir, log_path) = setup_dirs();
    let content = "{\"role\":\"user\"}\n{\"role\":\"assistant\"}\n";
    std::fs::write(&log_path, content)?;

    let state = TranscriptState::new(transcripts_dir, Some(log_path))?;
    state.save_snapshot().await?;

    let retrieved = state.get_content(1).await?;
    assert_eq!(retrieved, content);

    Ok(())
}

#[tokio::test]
async fn get_content_not_found() -> anyhow::Result<()> {
    let (_tmp, transcripts_dir, log_path) = setup_dirs();
    let state = TranscriptState::new(transcripts_dir, Some(log_path))?;

    let result = state.get_content(99).await;
    assert!(result.is_err());

    Ok(())
}

#[tokio::test]
async fn catchup_returns_transcripts_and_live_lines() -> anyhow::Result<()> {
    let (_tmp, transcripts_dir, log_path) = setup_dirs();

    // Write initial log and save a transcript.
    std::fs::write(&log_path, "line1\nline2\n")?;
    let state = TranscriptState::new(transcripts_dir, Some(log_path.clone()))?;
    state.save_snapshot().await?;

    // Write more lines to the live log (simulating post-compact activity).
    std::fs::write(&log_path, "line1\nline2\nline3\nline4\n")?;

    // Catch up from transcript 0 (before any), line 0.
    let resp = state.catchup(0, 0).await?;
    assert_eq!(resp.transcripts.len(), 1);
    assert_eq!(resp.transcripts[0].number, 1);
    assert_eq!(resp.transcripts[0].lines.len(), 2);
    assert_eq!(resp.live_lines.len(), 4);
    assert_eq!(resp.current_line, 4);

    // Catch up from transcript 1, line 2 (should only get new live lines).
    let resp = state.catchup(1, 2).await?;
    assert_eq!(resp.transcripts.len(), 0);
    assert_eq!(resp.live_lines.len(), 2);
    assert_eq!(resp.live_lines[0], "line3");
    assert_eq!(resp.live_lines[1], "line4");

    Ok(())
}

#[tokio::test]
async fn no_log_path_graceful() -> anyhow::Result<()> {
    let (_tmp, transcripts_dir, _log_path) = setup_dirs();
    let state = TranscriptState::new(transcripts_dir, None)?;

    // save_snapshot should fail gracefully when no log path.
    let result = state.save_snapshot().await;
    assert!(result.is_err());

    // list should return empty.
    let list = state.list().await;
    assert!(list.is_empty());

    // catchup should return empty with no log.
    let resp = state.catchup(0, 0).await?;
    assert!(resp.transcripts.is_empty());
    assert!(resp.live_lines.is_empty());

    Ok(())
}

#[tokio::test]
async fn resume_scan_picks_up_existing_transcripts() -> anyhow::Result<()> {
    let (_tmp, transcripts_dir, log_path) = setup_dirs();
    std::fs::write(&log_path, "data\n")?;

    // Save two transcripts with the first state.
    let state = TranscriptState::new(transcripts_dir.clone(), Some(log_path.clone()))?;
    state.save_snapshot().await?;
    state.save_snapshot().await?;
    drop(state);

    // Create a new state (simulating resume) — should find existing files.
    let state2 = TranscriptState::new(transcripts_dir, Some(log_path))?;
    let list = state2.list().await;
    assert_eq!(list.len(), 2);
    assert_eq!(list[0].number, 1);
    assert_eq!(list[1].number, 2);

    // Next save should be number 3.
    let m3 = state2.save_snapshot().await?;
    assert_eq!(m3.number, 3);

    Ok(())
}

#[tokio::test]
async fn update_session_log_path_fixes_snapshot() -> anyhow::Result<()> {
    let (_tmp, transcripts_dir, log_path) = setup_dirs();
    // Start with a non-existent path (simulates stale path after session switch).
    let stale_path = _tmp.path().join("stale.jsonl");
    let state = TranscriptState::new(transcripts_dir, Some(stale_path))?;

    // save_snapshot should fail because the stale path doesn't exist.
    assert!(state.save_snapshot().await.is_err());

    // Now write the actual log and update the path.
    std::fs::write(&log_path, "line1\nline2\n")?;
    state.update_session_log_path(Some(log_path)).await;

    // save_snapshot should succeed now.
    let meta = state.save_snapshot().await?;
    assert_eq!(meta.number, 2); // number 1 was consumed by the failed attempt
    assert_eq!(meta.line_count, 2);

    Ok(())
}

#[tokio::test]
async fn broadcast_seq_increments() -> anyhow::Result<()> {
    let (_tmp, transcripts_dir, log_path) = setup_dirs();
    std::fs::write(&log_path, "x\n")?;

    let state = TranscriptState::new(transcripts_dir, Some(log_path))?;
    let mut rx = state.transcript_tx.subscribe();

    state.save_snapshot().await?;
    state.save_snapshot().await?;

    let e1 = rx.try_recv()?;
    let e2 = rx.try_recv()?;
    assert_eq!(e1.seq, 0);
    assert_eq!(e2.seq, 1);

    Ok(())
}
