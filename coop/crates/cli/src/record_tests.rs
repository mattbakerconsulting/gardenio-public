// SPDX-License-Identifier: BUSL-1.1
// Copyright (c) 2026 Matt Baker <fimbaz@gmail.com>

use crate::record::{RecordingEntry, RecordingState};
use crate::screen::{CursorPosition, ScreenSnapshot};

fn test_snapshot() -> ScreenSnapshot {
    ScreenSnapshot {
        lines: vec!["hello".to_owned()],
        ansi: vec![],
        cols: 80,
        rows: 24,
        alt_screen: false,
        cursor: CursorPosition { row: 0, col: 5 },
        sequence: 1,
    }
}

#[tokio::test]
async fn enable_disable_toggle() -> anyhow::Result<()> {
    let state = RecordingState::new(None, 80, 24);
    assert!(!state.is_enabled());
    state.enable().await;
    assert!(state.is_enabled());
    state.disable();
    assert!(!state.is_enabled());
    Ok(())
}

#[tokio::test]
async fn push_when_disabled_is_noop() -> anyhow::Result<()> {
    let state = RecordingState::new(None, 80, 24);
    state.push("state", serde_json::json!({}), test_snapshot()).await;
    assert_eq!(state.status().entries, 0);
    Ok(())
}

#[tokio::test]
async fn push_increments_seq() -> anyhow::Result<()> {
    let state = RecordingState::new(None, 80, 24);
    state.enable().await;
    state
        .push("state", serde_json::json!({"prev":"Starting","next":"Working"}), test_snapshot())
        .await;
    state.push("hook", serde_json::json!({"hook_seq":0}), test_snapshot()).await;
    assert_eq!(state.status().entries, 2);
    Ok(())
}

#[tokio::test]
async fn file_write_and_catchup() -> anyhow::Result<()> {
    let dir = tempfile::tempdir()?;
    let state = RecordingState::new(Some(dir.path()), 80, 24);
    state.enable().await;

    let snap = test_snapshot();
    state
        .push("state", serde_json::json!({"prev":"Starting","next":"Working"}), snap.clone())
        .await;
    state.push("hook", serde_json::json!({"hook_seq":0}), snap).await;

    // Catchup from 0 returns both
    let entries = state.catchup(0).await;
    assert_eq!(entries.len(), 2);
    assert_eq!(entries[0].seq, 1);
    assert_eq!(entries[1].seq, 2);

    // Catchup from 1 returns only the second
    let entries = state.catchup(1).await;
    assert_eq!(entries.len(), 1);
    assert_eq!(entries[0].seq, 2);

    Ok(())
}

#[tokio::test]
async fn download_returns_file_contents() -> anyhow::Result<()> {
    let dir = tempfile::tempdir()?;
    let state = RecordingState::new(Some(dir.path()), 80, 24);
    state.enable().await;
    state.push("state", serde_json::json!({}), test_snapshot()).await;

    let data = state.download().await;
    assert!(data.is_some());
    let text = String::from_utf8(data.ok_or_else(|| anyhow::anyhow!("no data"))?)?;
    let lines: Vec<&str> = text.lines().collect();
    // Header + 1 entry
    assert_eq!(lines.len(), 2);
    assert!(lines[0].contains("\"version\":1"));
    assert!(lines[1].contains("\"seq\":1"));

    Ok(())
}

fn secret_snapshot() -> ScreenSnapshot {
    ScreenSnapshot {
        lines: vec![
            "$ export API_KEY=sk-live-supersecret123".to_owned(),
            "bearer: eyJhbGciOiJIUzI1NiJ9.payload.sig".to_owned(),
            "password: hunter2".to_owned(),
            "Token=ghp_aaaaBBBBccccDDDDeeeeFFFF".to_owned(),
            "ordinary log line".to_owned(),
        ],
        ansi: vec!["api-key=topsecret".to_owned()],
        cols: 80,
        rows: 24,
        alt_screen: false,
        cursor: CursorPosition { row: 3, col: 0 },
        sequence: 1,
    }
}

async fn read_recorded_entries(state: &RecordingState) -> anyhow::Result<Vec<RecordingEntry>> {
    let bytes = state.download().await.ok_or_else(|| anyhow::anyhow!("no data"))?;
    let text = String::from_utf8(bytes)?;
    Ok(text
        .lines()
        .skip(1) // skip header
        .filter_map(|l| serde_json::from_str::<RecordingEntry>(l).ok())
        .collect())
}

#[tokio::test]
async fn redacts_known_secret_lines_in_jsonl() -> anyhow::Result<()> {
    let dir = tempfile::tempdir()?;
    let state = RecordingState::new_with_redact(Some(dir.path()), 80, 24, true);
    state.enable().await;
    state.push("state", serde_json::json!({}), secret_snapshot()).await;

    let entries = read_recorded_entries(&state).await?;
    assert_eq!(entries.len(), 1);
    let joined = entries[0].screen.lines.join("\n");
    assert!(!joined.contains("supersecret123"), "api key leaked: {joined}");
    assert!(!joined.contains("hunter2"), "password leaked: {joined}");
    assert!(!joined.contains("eyJhbGciOiJIUzI1NiJ9"), "bearer leaked: {joined}");
    assert!(!joined.contains("ghp_aaaaBBBB"), "token leaked: {joined}");
    assert!(joined.contains("<redacted>"), "expected redaction marker: {joined}");
    assert!(joined.contains("ordinary log line"), "non-secret line was lost: {joined}");

    // ANSI lines redacted too.
    let ansi_joined = entries[0].screen.ansi.join("\n");
    assert!(!ansi_joined.contains("topsecret"), "ansi secret leaked: {ansi_joined}");
    assert!(ansi_joined.contains("<redacted>"), "expected ansi redaction: {ansi_joined}");
    Ok(())
}

/// Screen with BARE credential shapes (no `key=`/`token:` prefix) — the F16
/// (R4-SEC-3) case a reasoning agent activates by echoing a raw provider key.
fn bare_credential_snapshot() -> ScreenSnapshot {
    ScreenSnapshot {
        lines: vec![
            "using key sk-ant-oat01-AbCdEf012345_gh-ijkl for auth".to_owned(),
            "x-litellm-api-key: sk-1234567890abcdefABCDEF".to_owned(), // gitleaks:allow: fake token for the redaction test
            "virtual key vk-abcdEFGH01234567 minted".to_owned(),
            "cloned with ghp_aaaaBBBBccccDDDDeeeeFFFF1234".to_owned(),
            "plain narrative with no secret at all".to_owned(),
            "the task-sk-workaround shipped; disk-space is low".to_owned(),
        ],
        ansi: vec!["glpat-ABCdef0123456789xyzQ".to_owned()], // gitleaks:allow: fake token for the redaction test
        cols: 80,
        rows: 24,
        alt_screen: false,
        cursor: CursorPosition { row: 4, col: 0 },
        sequence: 1,
    }
}

#[tokio::test]
async fn redacts_bare_credential_shapes_in_jsonl() -> anyhow::Result<()> {
    let dir = tempfile::tempdir()?;
    let state = RecordingState::new_with_redact(Some(dir.path()), 80, 24, true);
    state.enable().await;
    state.push("state", serde_json::json!({}), bare_credential_snapshot()).await;

    let entries = read_recorded_entries(&state).await?;
    assert_eq!(entries.len(), 1);
    let joined = entries[0].screen.lines.join("\n");
    // Anthropic key body must not survive even without a keyword prefix.
    assert!(!joined.contains("oat01-AbCdEf012345"), "sk-ant- key leaked: {joined}");
    // LiteLLM sk-/vk- virtual-key shapes.
    assert!(!joined.contains("1234567890abcdefABCDEF"), "litellm sk- key leaked: {joined}");
    assert!(!joined.contains("abcdEFGH01234567"), "litellm vk- key leaked: {joined}");
    // SCM token shapes that also ride the pod env.
    assert!(!joined.contains("aaaaBBBBccccDDDD"), "ghp_ token leaked: {joined}");
    assert!(joined.contains("<redacted>"), "expected redaction marker: {joined}");
    // Surrounding prose on the same line is preserved.
    assert!(joined.contains("using key <redacted> for auth"), "prose not preserved: {joined}");
    assert!(joined.contains("plain narrative with no secret at all"), "clean line lost: {joined}");
    // False-positive guard: an ordinary hyphenated word containing a bare `sk-`
    // fragment (body below the 16-char floor) must NOT be redacted.
    assert!(
        joined.contains("the task-sk-workaround shipped; disk-space is low"),
        "hyphenated word wrongly redacted: {joined}"
    );

    // ANSI lines get the same treatment.
    let ansi_joined = entries[0].screen.ansi.join("\n");
    assert!(!ansi_joined.contains("ABCdef0123456789xyzQ"), "glpat- token leaked: {ansi_joined}");
    assert!(ansi_joined.contains("<redacted>"), "expected ansi redaction: {ansi_joined}");
    Ok(())
}

#[tokio::test]
async fn escape_hatch_disables_redaction() -> anyhow::Result<()> {
    // The runtime escape hatch is `COOP_RECORD_REDACTION=off`, which is read
    // once in `RecordingState::new`. Construct directly with redact=false to
    // exercise the same disabled path without mutating process-global env.
    let dir = tempfile::tempdir()?;
    let state = RecordingState::new_with_redact(Some(dir.path()), 80, 24, false);
    state.enable().await;
    state.push("state", serde_json::json!({}), secret_snapshot()).await;

    let entries = read_recorded_entries(&state).await?;
    assert_eq!(entries.len(), 1);
    let joined = entries[0].screen.lines.join("\n");
    assert!(joined.contains("supersecret123"), "escape hatch should not redact: {joined}");
    assert!(joined.contains("hunter2"), "escape hatch should not redact: {joined}");
    assert!(joined.contains("ghp_aaaaBBBB"), "escape hatch should not redact: {joined}");
    assert!(!joined.contains("<redacted>"), "no redaction marker expected: {joined}");
    Ok(())
}

#[tokio::test]
async fn broadcast_sends_entries() -> anyhow::Result<()> {
    let state = RecordingState::new(None, 80, 24);
    let mut rx = state.record_tx.subscribe();
    state.enable().await;

    state.push("state", serde_json::json!({}), test_snapshot()).await;

    let entry = rx.try_recv()?;
    assert_eq!(entry.seq, 1);
    assert_eq!(entry.kind, "state");

    Ok(())
}

/// Flush-barrier ordering: `catchup` sends a `Flush` down the same FIFO writer
/// queue the entries travel on, and each `push` round-trips through the writer
/// (awaiting its per-entry `complete` oneshot) before returning. So an entry
/// whose `push` future has resolved is always visible to the next `catchup`,
/// and interleaving pushes with catchups yields a strictly monotonic view.
#[tokio::test]
async fn interleaved_push_and_catchup_preserve_ordering() -> anyhow::Result<()> {
    let dir = tempfile::tempdir()?;
    let state = RecordingState::new(Some(dir.path()), 80, 24);
    state.enable().await;

    // First batch, then a barrier read.
    for _ in 0..3 {
        state.push("state", serde_json::json!({}), test_snapshot()).await;
    }
    let entries = state.catchup(0).await;
    let seqs: Vec<u64> = entries.iter().map(|e| e.seq).collect();
    assert_eq!(seqs, vec![1, 2, 3], "barrier must expose all entries pushed before it");

    // Second batch interleaved after the first read.
    for _ in 0..3 {
        state.push("hook", serde_json::json!({}), test_snapshot()).await;
    }
    let entries = state.catchup(0).await;
    let seqs: Vec<u64> = entries.iter().map(|e| e.seq).collect();
    assert_eq!(seqs, vec![1, 2, 3, 4, 5, 6], "second barrier must expose the union in order");

    // A cursored catchup returns only the tail.
    let tail = state.catchup(3).await;
    let tail_seqs: Vec<u64> = tail.iter().map(|e| e.seq).collect();
    assert_eq!(tail_seqs, vec![4, 5, 6]);
    Ok(())
}

/// Full catch-up after enqueueing more than the writer batch size
/// (`RECORDING_BATCH_SIZE` = 64) and past the queue capacity
/// (`RECORDING_QUEUE_CAPACITY` = 256): the batching drain must never drop an
/// entry, so catchup returns every one in strict seq order.
#[tokio::test]
async fn catchup_after_more_than_batch_size_returns_all_in_order() -> anyhow::Result<()> {
    let dir = tempfile::tempdir()?;
    let state = RecordingState::new(Some(dir.path()), 80, 24);
    state.enable().await;

    // 300 > batch size (64) and > queue capacity (256).
    const N: u64 = 300;
    for _ in 0..N {
        state.push("state", serde_json::json!({}), test_snapshot()).await;
    }

    let entries = state.catchup(0).await;
    assert_eq!(entries.len() as u64, N, "no recording entries may be dropped");
    let seqs: Vec<u64> = entries.iter().map(|e| e.seq).collect();
    let expected: Vec<u64> = (1..=N).collect();
    assert_eq!(seqs, expected, "entries must survive in strict seq order");
    assert_eq!(state.status().entries, N, "status entry count must match");
    Ok(())
}
