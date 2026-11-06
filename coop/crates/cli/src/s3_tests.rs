// SPDX-License-Identifier: BUSL-1.1
// Copyright (c) 2026 Matt Baker <fimbaz@gmail.com>

use std::collections::HashMap;
use std::path::Path;
use std::sync::Arc;
use std::time::Duration;

use clap::Parser;
use serial_test::serial;
use tokio::sync::{broadcast, RwLock};
use tokio_util::sync::CancellationToken;

use super::*;
use crate::config::Config;

// ---------------------------------------------------------------------------
// MockS3 — in-memory S3Storage for testing
// ---------------------------------------------------------------------------

struct MockS3 {
    objects: parking_lot::Mutex<HashMap<String, Vec<u8>>>,
}

impl MockS3 {
    fn new() -> Self {
        Self { objects: parking_lot::Mutex::new(HashMap::new()) }
    }

    /// Pre-load an object into the mock store.
    fn put(&self, session_id: &str, s3_path: &str, data: Vec<u8>) {
        let key = Self::mock_key(session_id, s3_path);
        self.objects.lock().insert(key, data);
    }

    /// Read an object from the mock store (for assertions).
    fn get(&self, session_id: &str, s3_path: &str) -> Option<Vec<u8>> {
        let key = Self::mock_key(session_id, s3_path);
        self.objects.lock().get(&key).cloned()
    }

    /// Number of stored objects.
    fn object_count(&self) -> usize {
        self.objects.lock().len()
    }

    /// All stored keys.
    fn keys(&self) -> Vec<String> {
        self.objects.lock().keys().cloned().collect()
    }

    fn mock_key(session_id: &str, s3_path: &str) -> String {
        format!("{session_id}/{s3_path}")
    }
}

impl S3Storage for MockS3 {
    async fn upload_file(
        &self,
        session_id: &str,
        s3_path: &str,
        local_path: &Path,
    ) -> anyhow::Result<()> {
        let data = tokio::fs::read(local_path).await?;
        let key = Self::mock_key(session_id, s3_path);
        self.objects.lock().insert(key, data);
        Ok(())
    }

    async fn upload_bytes(
        &self,
        session_id: &str,
        s3_path: &str,
        data: Vec<u8>,
    ) -> anyhow::Result<()> {
        let key = Self::mock_key(session_id, s3_path);
        self.objects.lock().insert(key, data);
        Ok(())
    }

    async fn download_file(
        &self,
        session_id: &str,
        s3_path: &str,
        local_path: &Path,
    ) -> anyhow::Result<()> {
        let key = Self::mock_key(session_id, s3_path);
        let data = {
            let objects = self.objects.lock();
            objects.get(&key).cloned().ok_or_else(|| anyhow::anyhow!("object not found: {key}"))?
        };
        if let Some(parent) = local_path.parent() {
            tokio::fs::create_dir_all(parent).await?;
        }
        tokio::fs::write(local_path, data).await?;
        Ok(())
    }

    async fn list_transcripts(&self, session_id: &str) -> anyhow::Result<Vec<u32>> {
        let objects = self.objects.lock();
        let prefix = format!("{session_id}/transcripts/");
        let mut numbers = Vec::new();
        for key in objects.keys() {
            if let Some(rest) = key.strip_prefix(&prefix) {
                if let Some(num_str) = rest.strip_suffix(".jsonl") {
                    if let Ok(n) = num_str.parse::<u32>() {
                        numbers.push(n);
                    }
                }
            }
        }
        numbers.sort();
        Ok(numbers)
    }

    async fn upload_meta(&self, session_id: &str, meta: &SessionMeta) -> anyhow::Result<()> {
        let json = serde_json::to_vec_pretty(meta)?;
        self.upload_bytes(session_id, "meta.json", json).await
    }

    async fn download_meta(&self, session_id: &str) -> anyhow::Result<Option<SessionMeta>> {
        let key = Self::mock_key(session_id, "meta.json");
        let objects = self.objects.lock();
        match objects.get(&key) {
            Some(data) => {
                let meta: SessionMeta = serde_json::from_slice(data)?;
                Ok(Some(meta))
            }
            None => Ok(None),
        }
    }

    async fn exists(&self, session_id: &str, s3_path: &str) -> bool {
        let key = Self::mock_key(session_id, s3_path);
        self.objects.lock().contains_key(&key)
    }

    async fn list_sessions(&self) -> anyhow::Result<Vec<String>> {
        let objects = self.objects.lock();
        let mut session_ids = std::collections::BTreeSet::new();
        for key in objects.keys() {
            // Keys are "session_id/path" — extract session_id.
            if let Some(sid) = key.split('/').next() {
                session_ids.insert(sid.to_owned());
            }
        }
        Ok(session_ids.into_iter().collect())
    }

    async fn list_spawns(&self, bead_id: &str) -> anyhow::Result<Vec<String>> {
        // Mirror the S3Client semantics: a spawn is identified by the
        // presence of `<bead>/<spawn>/meta.json`.
        let objects = self.objects.lock();
        let mut spawns = std::collections::BTreeSet::new();
        let prefix = format!("{bead_id}/");
        for key in objects.keys() {
            if let Some(rest) = key.strip_prefix(&prefix) {
                if let Some(spawn) = rest.strip_suffix("/meta.json") {
                    if !spawn.is_empty() && !spawn.contains('/') {
                        spawns.insert(spawn.to_owned());
                    }
                }
            }
        }
        Ok(spawns.into_iter().collect())
    }

    async fn list_recording_chunks(&self, session_id: &str) -> anyhow::Result<Vec<String>> {
        let objects = self.objects.lock();
        let prefix = format!("{session_id}/recording/");
        let mut chunks = Vec::new();
        for key in objects.keys() {
            if let Some(rest) = key.strip_prefix(&prefix) {
                if rest.ends_with(".jsonl") {
                    chunks.push(rest.to_owned());
                }
            }
        }
        chunks.sort();
        Ok(chunks)
    }

    async fn list_workflow_files(&self, session_id: &str) -> anyhow::Result<Vec<String>> {
        let objects = self.objects.lock();
        let prefix = format!("{session_id}/workflow/");
        let mut files = Vec::new();
        for key in objects.keys() {
            if let Some(rest) = key.strip_prefix(&prefix) {
                if !rest.is_empty() {
                    files.push(rest.to_owned());
                }
            }
        }
        files.sort();
        Ok(files)
    }

    async fn download_bytes(&self, session_id: &str, s3_path: &str) -> anyhow::Result<Vec<u8>> {
        let key = Self::mock_key(session_id, s3_path);
        let objects = self.objects.lock();
        objects.get(&key).cloned().ok_or_else(|| anyhow::anyhow!("object not found: {key}"))
    }
}

// ---------------------------------------------------------------------------
// FailingS3 — always returns errors, for error-handling tests
// ---------------------------------------------------------------------------

struct FailingS3;

impl S3Storage for FailingS3 {
    async fn upload_file(&self, _: &str, _: &str, _: &Path) -> anyhow::Result<()> {
        anyhow::bail!("mock failure: upload_file")
    }
    async fn upload_bytes(&self, _: &str, _: &str, _: Vec<u8>) -> anyhow::Result<()> {
        anyhow::bail!("mock failure: upload_bytes")
    }
    async fn download_file(&self, _: &str, _: &str, _: &Path) -> anyhow::Result<()> {
        anyhow::bail!("mock failure: download_file")
    }
    async fn list_transcripts(&self, _: &str) -> anyhow::Result<Vec<u32>> {
        anyhow::bail!("mock failure: list_transcripts")
    }
    async fn upload_meta(&self, _: &str, _: &SessionMeta) -> anyhow::Result<()> {
        anyhow::bail!("mock failure: upload_meta")
    }
    async fn download_meta(&self, _: &str) -> anyhow::Result<Option<SessionMeta>> {
        anyhow::bail!("mock failure: download_meta")
    }
    async fn exists(&self, _: &str, _: &str) -> bool {
        false
    }
    async fn list_sessions(&self) -> anyhow::Result<Vec<String>> {
        anyhow::bail!("mock failure: list_sessions")
    }
    async fn list_spawns(&self, _: &str) -> anyhow::Result<Vec<String>> {
        anyhow::bail!("mock failure: list_spawns")
    }
    async fn list_recording_chunks(&self, _: &str) -> anyhow::Result<Vec<String>> {
        anyhow::bail!("mock failure: list_recording_chunks")
    }
    async fn list_workflow_files(&self, _: &str) -> anyhow::Result<Vec<String>> {
        anyhow::bail!("mock failure: list_workflow_files")
    }
    async fn download_bytes(&self, _: &str, _: &str) -> anyhow::Result<Vec<u8>> {
        anyhow::bail!("mock failure: download_bytes")
    }
}

// ---------------------------------------------------------------------------
// Helper: build a test RecordingEntry
// ---------------------------------------------------------------------------

fn test_recording_entry(seq: u64) -> RecordingEntry {
    use crate::screen::{CursorPosition, ScreenSnapshot};
    RecordingEntry {
        ts: 1700000000 + seq,
        seq,
        kind: "state".into(),
        detail: serde_json::json!({"prev": "Starting", "next": "Working"}),
        screen: ScreenSnapshot {
            lines: vec!["hello".into()],
            ansi: vec!["hello".into()],
            cols: 80,
            rows: 24,
            alt_screen: false,
            cursor: CursorPosition { row: 0, col: 5 },
            sequence: seq,
        },
    }
}

// ===========================================================================
// Unit tests — pure logic, no I/O
// ===========================================================================

#[test]
fn session_meta_serializes() -> anyhow::Result<()> {
    let meta = SessionMeta {
        session_id: "test-123".into(),
        agent_type: "claude".into(),
        started_at: 1700000000,
        ended_at: Some(1700003600),
        exit_code: Some(0),
        labels: vec!["project:gardenio".into()],
        agent_name: Some("test-agent".into()),
        project: Some("gardenio".into()),
        role: Some("crew".into()),
        bead_id: Some("ka-123".into()),
        exit_status: Some("success".into()),
        transcript_count: Some(5),
        harness: None,
    };
    let json = serde_json::to_string(&meta)?;
    let parsed: SessionMeta = serde_json::from_str(&json)?;
    assert_eq!(parsed.session_id, "test-123");
    assert_eq!(parsed.exit_code, Some(0));
    assert_eq!(parsed.labels.len(), 1);
    assert_eq!(parsed.agent_name.as_deref(), Some("test-agent"));
    assert_eq!(parsed.project.as_deref(), Some("gardenio"));
    assert_eq!(parsed.role.as_deref(), Some("crew"));
    assert_eq!(parsed.bead_id.as_deref(), Some("ka-123"));
    assert_eq!(parsed.exit_status.as_deref(), Some("success"));
    assert_eq!(parsed.transcript_count, Some(5));
    Ok(())
}

#[test]
fn session_meta_skips_none_fields() -> anyhow::Result<()> {
    let meta = SessionMeta {
        session_id: "test-456".into(),
        agent_type: "claude".into(),
        started_at: 1700000000,
        ended_at: None,
        exit_code: None,
        labels: vec![],
        agent_name: None,
        project: None,
        role: None,
        bead_id: None,
        exit_status: None,
        transcript_count: None,
        harness: None,
    };
    let json = serde_json::to_string(&meta)?;
    assert!(!json.contains("ended_at"));
    assert!(!json.contains("exit_code"));
    assert!(!json.contains("agent_name"));
    assert!(!json.contains("project"));
    assert!(!json.contains("role"));
    assert!(!json.contains("bead_id"));
    assert!(!json.contains("exit_status"));
    assert!(!json.contains("transcript_count"));
    Ok(())
}

#[test]
fn s3_key_format() {
    // Verify the key format logic without needing a real S3 client.
    let prefix = "coop/sessions";
    let session_id = "abc-123";
    let path = "transcripts/1.jsonl";
    let key = format!("{prefix}/{session_id}/{path}");
    assert_eq!(key, "coop/sessions/abc-123/transcripts/1.jsonl");
}

#[test]
fn static_credentials_from_files_reads_and_trims_both_files() -> anyhow::Result<()> {
    let dir = tempfile::tempdir()?;
    let access_path = dir.path().join("access_key_id");
    let secret_path = dir.path().join("secret_access_key");
    std::fs::write(&access_path, "AKIAEXAMPLE\n")?;
    std::fs::write(&secret_path, "  supersecret  \n")?;

    let creds = S3Client::static_credentials_from_files(Some(&access_path), Some(&secret_path));
    assert!(creds.is_some());
    Ok(())
}

#[test]
fn static_credentials_from_files_none_when_either_path_missing() {
    assert!(S3Client::static_credentials_from_files(None, None).is_none());
}

#[test]
fn static_credentials_from_files_none_when_only_access_key_given() -> anyhow::Result<()> {
    let dir = tempfile::tempdir()?;
    let access_path = dir.path().join("access_key_id");
    std::fs::write(&access_path, "AKIAEXAMPLE")?;

    assert!(S3Client::static_credentials_from_files(Some(&access_path), None).is_none());
    Ok(())
}

#[test]
fn static_credentials_from_files_falls_back_on_unreadable_file() {
    let missing = Path::new("/nonexistent/path/does-not-exist");
    assert!(S3Client::static_credentials_from_files(Some(missing), Some(missing)).is_none());
}

#[test]
fn static_credentials_from_files_falls_back_on_empty_file() -> anyhow::Result<()> {
    let dir = tempfile::tempdir()?;
    let access_path = dir.path().join("access_key_id");
    let secret_path = dir.path().join("secret_access_key");
    std::fs::write(&access_path, "")?;
    std::fs::write(&secret_path, "supersecret")?;

    assert!(
        S3Client::static_credentials_from_files(Some(&access_path), Some(&secret_path)).is_none()
    );
    Ok(())
}

#[test]
fn session_meta_default_labels() -> anyhow::Result<()> {
    let json = r#"{"session_id":"x","agent_type":"claude","started_at":0}"#;
    let meta: SessionMeta = serde_json::from_str(json)?;
    assert!(meta.labels.is_empty());
    assert!(meta.ended_at.is_none());
    assert!(meta.exit_code.is_none());
    assert!(meta.agent_name.is_none());
    assert!(meta.project.is_none());
    assert!(meta.role.is_none());
    assert!(meta.bead_id.is_none());
    assert!(meta.exit_status.is_none());
    assert!(meta.transcript_count.is_none());
    Ok(())
}

#[test]
fn exit_status_from_code_maps_correctly() {
    // Codes whose status is independent of the shutdown / idle-suspend bits.
    for shutdown in [false, true] {
        for idle in [false, true] {
            assert_eq!(exit_status_from_code(Some(0), None, shutdown, idle), "success");
            assert_eq!(exit_status_from_code(Some(1), None, shutdown, idle), "error");
            assert_eq!(exit_status_from_code(Some(124), None, shutdown, idle), "timeout");
            // SIGKILL (137 / signal 9) means the child overran the grace window
            // — surface it as an error even when shutdown / idle-suspend was
            // requested (the idle-suspend path forwards a graceful SIGTERM so a
            // 137 here is a genuine hung-child failure, not a clean suspend).
            assert_eq!(exit_status_from_code(Some(137), None, shutdown, idle), "error");
            assert_eq!(exit_status_from_code(None, Some(9), shutdown, idle), "error");
            assert_eq!(exit_status_from_code(None, None, shutdown, idle), "error");
            assert_eq!(
                exit_status_from_code(
                    Some(crate::run::EXIT_CODE_CREDENTIAL_SWITCH),
                    None,
                    shutdown,
                    idle
                ),
                "switch"
            );
        }
    }
}

#[test]
fn exit_status_signal_termination_depends_on_shutdown_bit() {
    // 143=SIGTERM, 130=SIGINT, 129=SIGHUP (128 + signum). A clean gb stop /
    // pod teardown cancels the shutdown token before the child exits with one
    // of these codes → "stopped", not a false-positive "error" (ka-0000001328).
    //
    // The signal reaches us two ways: a trapped exit (code=Some(128+sig)) and a
    // kill-outright (code=None, signal=Some(sig)). Both must score identically.
    for (sig, trapped_code) in [(15, 143), (2, 130), (1, 129)] {
        for (code, signal) in [(Some(trapped_code), None), (None, Some(sig))] {
            assert_eq!(
                exit_status_from_code(code, signal, true, false),
                "stopped",
                "signal {sig} (code={code:?}, signal={signal:?}) after requested shutdown should be 'stopped'"
            );
            // A genuine mid-task signal-kill (no shutdown requested) must still
            // surface as an error so real crashes are not camouflaged.
            assert_eq!(
                exit_status_from_code(code, signal, false, false),
                "error",
                "signal {sig} (code={code:?}, signal={signal:?}) without a requested shutdown should stay 'error'"
            );
        }
    }
}

#[test]
fn exit_status_idle_suspend_scores_suspended() {
    // The idle-timeout branch (session/run.rs) sets the idle_suspended bit and
    // forwards a graceful SIGTERM before cancelling the shutdown token, so the
    // child exits with a signal-termination code AND both bits are set. That
    // combination must score "suspended" — a distinct reclaim-onto-warm-pool
    // signal — instead of the plain "stopped" a gb-stop teardown gets
    // (ka-0000002921). Idle-suspend always rides a requested shutdown, so the
    // realistic combination is shutdown=true; assert both bits explicitly.
    for (sig, trapped_code) in [(15, 143), (2, 130), (1, 129)] {
        for (code, signal) in [(Some(trapped_code), None), (None, Some(sig))] {
            assert_eq!(
                exit_status_from_code(code, signal, true, true),
                "suspended",
                "signal {sig} (code={code:?}, signal={signal:?}) on idle-suspend should be 'suspended'"
            );
        }
    }
    // Idle-suspend takes precedence over the plain stopped mapping: the bit is
    // what disambiguates the identical signal-termination codes.
    assert_eq!(exit_status_from_code(Some(143), None, true, true), "suspended");
    assert_eq!(exit_status_from_code(Some(143), None, true, false), "stopped");
    // A non-signal-termination code is unaffected by the idle-suspend bit — a
    // hung child SIGKILLed at the grace deadline (137) is still an error even
    // if the idle-suspend was requested.
    assert_eq!(exit_status_from_code(Some(137), None, true, true), "error");
    assert_eq!(exit_status_from_code(Some(0), None, true, true), "success");
}

#[test]
fn recording_chunk_name_format() {
    let first_seq = 10u64;
    let last_seq = 59u64;
    let name = format!("recording/{first_seq}-{last_seq}.jsonl");
    assert_eq!(name, "recording/10-59.jsonl");
}

#[test]
fn recording_entry_serializes_to_jsonl() -> anyhow::Result<()> {
    let entry = test_recording_entry(1);
    let line = serde_json::to_string(&entry)?;
    assert!(line.contains("\"seq\":1"));
    assert!(line.contains("\"kind\":\"state\""));
    // Verify it's valid JSON
    let _: serde_json::Value = serde_json::from_str(&line)?;
    Ok(())
}

// ===========================================================================
// MockS3 basic operations
// ===========================================================================

#[tokio::test]
async fn mock_upload_download_bytes_roundtrip() -> anyhow::Result<()> {
    let mock = MockS3::new();
    let data = b"hello world".to_vec();
    mock.upload_bytes("sess-1", "test.txt", data.clone()).await?;

    assert!(mock.exists("sess-1", "test.txt").await);
    assert!(!mock.exists("sess-1", "other.txt").await);

    let stored = mock.get("sess-1", "test.txt");
    assert_eq!(stored, Some(data));
    Ok(())
}

#[tokio::test]
async fn mock_upload_download_file_roundtrip() -> anyhow::Result<()> {
    let tmp = tempfile::tempdir()?;
    let mock = MockS3::new();

    // Write a local file and upload it.
    let src = tmp.path().join("source.txt");
    tokio::fs::write(&src, b"file content").await?;
    mock.upload_file("sess-1", "uploaded.txt", &src).await?;

    // Download to a different path.
    let dest = tmp.path().join("downloaded.txt");
    mock.download_file("sess-1", "uploaded.txt", &dest).await?;

    let content = tokio::fs::read_to_string(&dest).await?;
    assert_eq!(content, "file content");
    Ok(())
}

#[tokio::test]
async fn mock_download_missing_object_errors() {
    let tmp = tempfile::tempdir().ok();
    let Some(tmp) = tmp else { return };
    let mock = MockS3::new();
    let dest = tmp.path().join("missing.txt");
    let result = mock.download_file("sess-1", "nope.txt", &dest).await;
    assert!(result.is_err());
}

#[tokio::test]
async fn mock_list_transcripts() -> anyhow::Result<()> {
    let mock = MockS3::new();

    // No transcripts initially.
    let nums = mock.list_transcripts("sess-1").await?;
    assert!(nums.is_empty());

    // Add some transcripts.
    mock.put("sess-1", "transcripts/1.jsonl", b"t1".to_vec());
    mock.put("sess-1", "transcripts/3.jsonl", b"t3".to_vec());
    mock.put("sess-1", "transcripts/2.jsonl", b"t2".to_vec());
    // Non-transcript files should be ignored.
    mock.put("sess-1", "meta.json", b"{}".to_vec());
    mock.put("sess-1", "transcripts/readme.txt", b"x".to_vec());

    let nums = mock.list_transcripts("sess-1").await?;
    assert_eq!(nums, vec![1, 2, 3]);

    // Different session should be isolated.
    let nums = mock.list_transcripts("sess-2").await?;
    assert!(nums.is_empty());
    Ok(())
}

#[tokio::test]
async fn mock_upload_download_meta() -> anyhow::Result<()> {
    let mock = MockS3::new();

    // No meta initially.
    let meta = mock.download_meta("sess-1").await?;
    assert!(meta.is_none());

    // Upload meta.
    let meta = SessionMeta {
        session_id: "sess-1".into(),
        agent_type: "claude".into(),
        started_at: 1700000000,
        ended_at: None,
        exit_code: None,
        labels: vec!["project:test".into()],
        agent_name: None,
        project: None,
        role: None,
        bead_id: None,
        exit_status: None,
        transcript_count: None,
        harness: None,
    };
    mock.upload_meta("sess-1", &meta).await?;

    // Download and verify.
    let fetched = mock.download_meta("sess-1").await?;
    let fetched = fetched.ok_or_else(|| anyhow::anyhow!("expected meta"))?;
    assert_eq!(fetched.session_id, "sess-1");
    assert_eq!(fetched.agent_type, "claude");
    assert_eq!(fetched.labels, vec!["project:test"]);
    Ok(())
}

// ===========================================================================
// restore_transcripts tests
// ===========================================================================

#[tokio::test]
async fn restore_transcripts_downloads_all() -> anyhow::Result<()> {
    let tmp = tempfile::tempdir()?;
    let mock = Arc::new(MockS3::new());

    // Pre-load transcripts.
    mock.put("sess-1", "transcripts/1.jsonl", b"line1\nline2\n".to_vec());
    mock.put("sess-1", "transcripts/2.jsonl", b"line3\n".to_vec());
    mock.put("sess-1", "transcripts/5.jsonl", b"line4\n".to_vec());

    let dir = tmp.path().join("transcripts");
    let count = restore_transcripts(mock.as_ref(), "sess-1", &dir).await?;

    assert_eq!(count, 3);
    assert_eq!(tokio::fs::read_to_string(dir.join("1.jsonl")).await?, "line1\nline2\n");
    assert_eq!(tokio::fs::read_to_string(dir.join("2.jsonl")).await?, "line3\n");
    assert_eq!(tokio::fs::read_to_string(dir.join("5.jsonl")).await?, "line4\n");
    Ok(())
}

#[tokio::test]
async fn restore_transcripts_skips_existing() -> anyhow::Result<()> {
    let tmp = tempfile::tempdir()?;
    let mock = Arc::new(MockS3::new());

    mock.put("sess-1", "transcripts/1.jsonl", b"from-s3\n".to_vec());
    mock.put("sess-1", "transcripts/2.jsonl", b"from-s3\n".to_vec());

    let dir = tmp.path().join("transcripts");
    tokio::fs::create_dir_all(&dir).await?;
    // Pre-create transcript 1 locally.
    tokio::fs::write(dir.join("1.jsonl"), b"local-data\n").await?;

    let count = restore_transcripts(mock.as_ref(), "sess-1", &dir).await?;
    assert_eq!(count, 2); // Both counted (1 skipped, 1 downloaded).

    // Local file should NOT be overwritten.
    assert_eq!(tokio::fs::read_to_string(dir.join("1.jsonl")).await?, "local-data\n");
    // S3 file should be downloaded.
    assert_eq!(tokio::fs::read_to_string(dir.join("2.jsonl")).await?, "from-s3\n");
    Ok(())
}

#[tokio::test]
async fn restore_transcripts_empty_session() -> anyhow::Result<()> {
    let tmp = tempfile::tempdir()?;
    let mock = Arc::new(MockS3::new());

    let dir = tmp.path().join("transcripts");
    let count = restore_transcripts(mock.as_ref(), "no-such-session", &dir).await?;
    assert_eq!(count, 0);
    // Directory should still be created.
    assert!(dir.exists());
    Ok(())
}

#[tokio::test]
async fn restore_transcripts_creates_parent_dirs() -> anyhow::Result<()> {
    let tmp = tempfile::tempdir()?;
    let mock = Arc::new(MockS3::new());
    mock.put("s", "transcripts/1.jsonl", b"data".to_vec());

    let dir = tmp.path().join("deep").join("nested").join("transcripts");
    let count = restore_transcripts(mock.as_ref(), "s", &dir).await?;
    assert_eq!(count, 1);
    assert!(dir.join("1.jsonl").exists());
    Ok(())
}

// ===========================================================================
// restore_session_log tests
// ===========================================================================

#[tokio::test]
async fn restore_session_log_downloads() -> anyhow::Result<()> {
    let tmp = tempfile::tempdir()?;
    let mock = Arc::new(MockS3::new());
    mock.put("sess-1", "session.jsonl", b"{\"ts\":1}\n{\"ts\":2}\n".to_vec());

    let dest = tmp.path().join("restored-session.jsonl");
    restore_session_log(mock.as_ref(), "sess-1", &dest).await?;

    let content = tokio::fs::read_to_string(&dest).await?;
    assert_eq!(content, "{\"ts\":1}\n{\"ts\":2}\n");
    Ok(())
}

#[tokio::test]
async fn restore_session_log_creates_parent_dirs() -> anyhow::Result<()> {
    let tmp = tempfile::tempdir()?;
    let mock = Arc::new(MockS3::new());
    mock.put("s", "session.jsonl", b"log-data".to_vec());

    let dest = tmp.path().join("a").join("b").join("session.jsonl");
    restore_session_log(mock.as_ref(), "s", &dest).await?;

    assert_eq!(tokio::fs::read_to_string(&dest).await?, "log-data");
    Ok(())
}

#[tokio::test]
async fn restore_session_log_missing_errors() {
    let tmp = tempfile::tempdir().ok();
    let Some(tmp) = tmp else { return };
    let mock = Arc::new(MockS3::new());
    let dest = tmp.path().join("session.jsonl");
    let result = restore_session_log(mock.as_ref(), "no-session", &dest).await;
    assert!(result.is_err());
}

// ===========================================================================
// resolve_session_key tests
// ===========================================================================

#[test]
fn resolve_session_key_prefers_explicit() {
    let key = resolve_session_key(Some("ka-0000002795"), Some("uuid-resume"), "uuid-fallback");
    assert_eq!(key, "ka-0000002795");
}

#[test]
fn resolve_session_key_falls_back_to_resume() {
    let key = resolve_session_key(None, Some("uuid-resume"), "uuid-fallback");
    assert_eq!(key, "uuid-resume");
}

#[test]
fn resolve_session_key_falls_back_to_default() {
    let key = resolve_session_key(None, None, "uuid-fallback");
    assert_eq!(key, "uuid-fallback");
}

#[test]
fn resolve_session_key_treats_empty_explicit_as_unset() {
    let key = resolve_session_key(Some(""), Some("uuid-resume"), "uuid-fallback");
    assert_eq!(key, "uuid-resume");
    let key = resolve_session_key(Some("   "), Some("uuid-resume"), "uuid-fallback");
    assert_eq!(key, "uuid-resume");
}

#[test]
fn resolve_session_key_treats_empty_resume_as_unset() {
    let key = resolve_session_key(None, Some(""), "uuid-fallback");
    assert_eq!(key, "uuid-fallback");
    let key = resolve_session_key(None, Some("\t"), "uuid-fallback");
    assert_eq!(key, "uuid-fallback");
}

#[test]
fn resolve_session_key_trims_whitespace() {
    let key = resolve_session_key(Some("  ka-abc  "), None, "uuid-fallback");
    assert_eq!(key, "ka-abc");
}

// COOP_S3_RESUME_SESSION env-var path tests (ka-0000006166 / G13 audit).
//
// `--s3-resume-session` / `COOP_S3_RESUME_SESSION` is retained as a manual
// operator override — see the doc-comment on `resolve_session_key`. No
// production caller (helm/controller/e2e) sets it; these tests pin the
// clap → Config → resolve_session_key wiring so the override cannot rot
// silently. `#[serial]` is required because env mutations are process-global.

#[test]
#[serial]
fn resolve_session_key_uses_resume_env_var() -> anyhow::Result<()> {
    std::env::remove_var("COOP_S3_RESUME_SESSION");
    std::env::remove_var("COOP_S3_SESSION_KEY");
    std::env::set_var("COOP_S3_RESUME_SESSION", "sess-from-env");

    let cfg = Config::try_parse_from(["coop"])?;
    assert_eq!(cfg.s3_resume_session.as_deref(), Some("sess-from-env"));
    assert!(cfg.s3_session_key.is_none(), "explicit key must remain unset");

    let key = resolve_session_key(
        cfg.s3_session_key.as_deref(),
        cfg.s3_resume_session.as_deref(),
        "coop-uuid-fallback",
    );
    assert_eq!(key, "sess-from-env", "resume env var must drive the resolved key");

    std::env::remove_var("COOP_S3_RESUME_SESSION");
    Ok(())
}

#[test]
#[serial]
fn resolve_session_key_explicit_env_overrides_resume_env() -> anyhow::Result<()> {
    // Pins the documented precedence: when both env vars are set, the
    // explicit `COOP_S3_SESSION_KEY` (live production wire) wins over
    // the operator-override `COOP_S3_RESUME_SESSION`.
    std::env::remove_var("COOP_S3_RESUME_SESSION");
    std::env::remove_var("COOP_S3_SESSION_KEY");
    std::env::set_var("COOP_S3_RESUME_SESSION", "sess-from-resume-env");
    std::env::set_var("COOP_S3_SESSION_KEY", "ka-explicit-bead");

    let cfg = Config::try_parse_from(["coop"])?;
    let key = resolve_session_key(
        cfg.s3_session_key.as_deref(),
        cfg.s3_resume_session.as_deref(),
        "coop-uuid-fallback",
    );
    assert_eq!(key, "ka-explicit-bead");

    std::env::remove_var("COOP_S3_RESUME_SESSION");
    std::env::remove_var("COOP_S3_SESSION_KEY");
    Ok(())
}

#[test]
#[serial]
fn compose_session_key_uses_resume_env_var_when_no_bead() -> anyhow::Result<()> {
    // Operator override path: with no bead context (legacy flat layout),
    // `COOP_S3_RESUME_SESSION` should drive the composed key.
    std::env::remove_var("COOP_S3_RESUME_SESSION");
    std::env::set_var("COOP_S3_RESUME_SESSION", "legacy-flat-session");

    let cfg = Config::try_parse_from(["coop"])?;
    let composed = compose_session_key(None, cfg.s3_resume_session.as_deref(), "coop-uuid");
    assert_eq!(composed, "legacy-flat-session");

    std::env::remove_var("COOP_S3_RESUME_SESSION");
    Ok(())
}

// COOP_S3_ENDPOINT is the in-box object-store override (e.g. MinIO). When set,
// `S3Client::build_aws_client` must also enable `force_path_style(true)` — the
// Rust AWS SDK cannot turn on path-style from env/config, only in code
// (aws-sdk-rust #1230, #390). These tests pin the env → clap → Config wiring so
// the override cannot rot silently; the in-code path-style + endpoint plumbing
// is covered by the `cargo check --features s3` build. `#[serial]` is required
// because env mutations are process-global.

#[test]
#[serial]
fn s3_endpoint_env_var_populates_config() -> anyhow::Result<()> {
    std::env::remove_var("COOP_S3_ENDPOINT");
    std::env::set_var("COOP_S3_ENDPOINT", "http://minio.gardenio.svc:9000");

    let cfg = Config::try_parse_from(["coop"])?;
    assert_eq!(cfg.s3_endpoint.as_deref(), Some("http://minio.gardenio.svc:9000"));

    std::env::remove_var("COOP_S3_ENDPOINT");
    Ok(())
}

#[test]
#[serial]
fn s3_endpoint_unset_defaults_to_none() -> anyhow::Result<()> {
    // Unset endpoint → default AWS S3 (virtual-hosted), behavior unchanged.
    std::env::remove_var("COOP_S3_ENDPOINT");

    let cfg = Config::try_parse_from(["coop"])?;
    assert!(cfg.s3_endpoint.is_none(), "endpoint must default to None (AWS S3)");
    Ok(())
}

// ===========================================================================
// spawn_subscriber tests
// ===========================================================================

#[tokio::test]
async fn subscriber_uploads_metadata_on_start() -> anyhow::Result<()> {
    let mock = Arc::new(MockS3::new());
    let (transcript_tx, _) = broadcast::channel::<TranscriptEvent>(16);
    let (record_tx, _) = broadcast::channel::<RecordingEntry>(16);
    let (state_tx, _) = broadcast::channel::<crate::event::TransitionEvent>(16);
    let shutdown = CancellationToken::new();

    spawn_subscriber(
        Arc::clone(&mock),
        "test-session".into(),
        None,
        &transcript_tx,
        &record_tx,
        &state_tx,
        Arc::new(RwLock::new(None)),
        Duration::from_secs(60),
        "claude".into(),
        vec!["project:gardenio".into()],
        None,
        None,
        None,
        None,
        shutdown.clone(),
    );

    // Wait for the metadata upload task.
    tokio::time::sleep(Duration::from_millis(100)).await;
    shutdown.cancel();

    let meta = mock.download_meta("test-session").await?;
    let meta = meta.ok_or_else(|| anyhow::anyhow!("expected metadata"))?;
    assert_eq!(meta.session_id, "test-session");
    assert_eq!(meta.agent_type, "claude");
    assert!(meta.ended_at.is_none());
    assert_eq!(meta.labels, vec!["project:gardenio"]);
    Ok(())
}

#[tokio::test]
async fn subscriber_uploads_transcript_on_event() -> anyhow::Result<()> {
    let tmp = tempfile::tempdir()?;
    let session_dir = tmp.path().to_path_buf();
    let transcripts_dir = session_dir.join("transcripts");
    tokio::fs::create_dir_all(&transcripts_dir).await?;

    // Write a transcript file that the subscriber will upload.
    tokio::fs::write(transcripts_dir.join("1.jsonl"), b"transcript-data\n").await?;

    let mock = Arc::new(MockS3::new());
    let (transcript_tx, _) = broadcast::channel::<TranscriptEvent>(16);
    let (record_tx, _) = broadcast::channel::<RecordingEntry>(16);
    let (state_tx, _) = broadcast::channel::<crate::event::TransitionEvent>(16);
    let shutdown = CancellationToken::new();

    spawn_subscriber(
        Arc::clone(&mock),
        "sess-t".into(),
        Some(session_dir),
        &transcript_tx,
        &record_tx,
        &state_tx,
        Arc::new(RwLock::new(None)),
        Duration::from_secs(60),
        "claude".into(),
        vec![],
        None,
        None,
        None,
        None,
        shutdown.clone(),
    );

    // Wait for metadata upload.
    tokio::time::sleep(Duration::from_millis(50)).await;

    // Send a transcript event.
    let _ = transcript_tx.send(TranscriptEvent {
        number: 1,
        timestamp: "2026-01-01T00:00:00Z".into(),
        line_count: 10,
        seq: 1,
    });

    // Wait for the upload.
    tokio::time::sleep(Duration::from_millis(100)).await;
    shutdown.cancel();
    tokio::time::sleep(Duration::from_millis(50)).await;

    // Verify transcript was uploaded.
    let data = mock.get("sess-t", "transcripts/1.jsonl");
    assert_eq!(data, Some(b"transcript-data\n".to_vec()));
    Ok(())
}

#[tokio::test]
async fn subscriber_batches_recording_entries() -> anyhow::Result<()> {
    let mock = Arc::new(MockS3::new());
    let (transcript_tx, _) = broadcast::channel::<TranscriptEvent>(16);
    let (record_tx, _) = broadcast::channel::<RecordingEntry>(256);
    let (state_tx, _) = broadcast::channel::<crate::event::TransitionEvent>(16);
    let shutdown = CancellationToken::new();

    spawn_subscriber(
        Arc::clone(&mock),
        "sess-r".into(),
        None,
        &transcript_tx,
        &record_tx,
        &state_tx,
        Arc::new(RwLock::new(None)),
        Duration::from_secs(60), // Long interval so periodic doesn't trigger.
        "claude".into(),
        vec![],
        None,
        None,
        None,
        None,
        shutdown.clone(),
    );

    tokio::time::sleep(Duration::from_millis(50)).await;

    // Send 50 recording entries to trigger a batch flush (batch_size = 50).
    for i in 1..=50 {
        let _ = record_tx.send(test_recording_entry(i));
    }

    // Wait for processing.
    tokio::time::sleep(Duration::from_millis(200)).await;
    shutdown.cancel();
    tokio::time::sleep(Duration::from_millis(50)).await;

    // Should have a recording chunk named "recording/1-50.jsonl".
    let data = mock.get("sess-r", "recording/1-50.jsonl");
    assert!(data.is_some(), "expected recording chunk 1-50");
    let data = data.ok_or_else(|| anyhow::anyhow!("missing chunk"))?;
    let lines: Vec<&str> = std::str::from_utf8(&data)?.lines().collect();
    assert_eq!(lines.len(), 50);
    Ok(())
}

#[tokio::test]
async fn subscriber_flushes_remaining_on_shutdown() -> anyhow::Result<()> {
    let mock = Arc::new(MockS3::new());
    let (transcript_tx, _) = broadcast::channel::<TranscriptEvent>(16);
    let (record_tx, _) = broadcast::channel::<RecordingEntry>(256);
    let (state_tx, _) = broadcast::channel::<crate::event::TransitionEvent>(16);
    let shutdown = CancellationToken::new();

    spawn_subscriber(
        Arc::clone(&mock),
        "sess-flush".into(),
        None,
        &transcript_tx,
        &record_tx,
        &state_tx,
        Arc::new(RwLock::new(None)),
        Duration::from_secs(60),
        "claude".into(),
        vec![],
        None,
        None,
        None,
        None,
        shutdown.clone(),
    );

    tokio::time::sleep(Duration::from_millis(50)).await;

    // Send fewer than batch_size (50) entries — they should NOT be flushed yet.
    for i in 1..=5 {
        let _ = record_tx.send(test_recording_entry(i));
    }
    tokio::time::sleep(Duration::from_millis(100)).await;

    // Verify nothing flushed yet (except metadata).
    assert!(mock.get("sess-flush", "recording/1-5.jsonl").is_none());

    // Trigger shutdown — should flush remaining.
    shutdown.cancel();
    tokio::time::sleep(Duration::from_millis(100)).await;

    let data = mock.get("sess-flush", "recording/1-5.jsonl");
    assert!(data.is_some(), "expected recording chunk flushed on shutdown");
    let data = data.ok_or_else(|| anyhow::anyhow!("missing chunk"))?;
    let lines: Vec<&str> = std::str::from_utf8(&data)?.lines().collect();
    assert_eq!(lines.len(), 5);
    Ok(())
}

#[tokio::test]
async fn subscriber_uploads_session_log_with_transcript() -> anyhow::Result<()> {
    let tmp = tempfile::tempdir()?;
    let session_dir = tmp.path().to_path_buf();
    let transcripts_dir = session_dir.join("transcripts");
    tokio::fs::create_dir_all(&transcripts_dir).await?;

    // Write session log and transcript files.
    let session_log = session_dir.join("session.jsonl");
    tokio::fs::write(&session_log, b"session-log-data\n").await?;
    tokio::fs::write(transcripts_dir.join("1.jsonl"), b"t1\n").await?;

    let mock = Arc::new(MockS3::new());
    let (transcript_tx, _) = broadcast::channel::<TranscriptEvent>(16);
    let (record_tx, _) = broadcast::channel::<RecordingEntry>(16);
    let (state_tx, _) = broadcast::channel::<crate::event::TransitionEvent>(16);
    let shutdown = CancellationToken::new();

    spawn_subscriber(
        Arc::clone(&mock),
        "sess-log".into(),
        Some(session_dir),
        &transcript_tx,
        &record_tx,
        &state_tx,
        Arc::new(RwLock::new(Some(session_log))),
        Duration::from_secs(60),
        "claude".into(),
        vec![],
        None,
        None,
        None,
        None,
        shutdown.clone(),
    );

    tokio::time::sleep(Duration::from_millis(50)).await;

    // Send transcript event — should also trigger session log upload.
    let _ = transcript_tx.send(TranscriptEvent {
        number: 1,
        timestamp: "2026-01-01T00:00:00Z".into(),
        line_count: 5,
        seq: 1,
    });

    tokio::time::sleep(Duration::from_millis(200)).await;
    shutdown.cancel();
    tokio::time::sleep(Duration::from_millis(50)).await;

    // Session log should have been uploaded.
    let log_data = mock.get("sess-log", "session.jsonl");
    assert_eq!(log_data, Some(b"session-log-data\n".to_vec()));
    Ok(())
}

#[tokio::test]
async fn subscriber_uploads_event_logs_on_shutdown() -> anyhow::Result<()> {
    let tmp = tempfile::tempdir()?;
    let session_dir = tmp.path().to_path_buf();

    // Write event log files.
    tokio::fs::write(session_dir.join("state_events.jsonl"), b"state-events\n").await?;
    tokio::fs::write(session_dir.join("hook_events.jsonl"), b"hook-events\n").await?;

    let mock = Arc::new(MockS3::new());
    let (transcript_tx, _) = broadcast::channel::<TranscriptEvent>(16);
    let (record_tx, _) = broadcast::channel::<RecordingEntry>(16);
    let (state_tx, _) = broadcast::channel::<crate::event::TransitionEvent>(16);
    let shutdown = CancellationToken::new();

    spawn_subscriber(
        Arc::clone(&mock),
        "sess-ev".into(),
        Some(session_dir),
        &transcript_tx,
        &record_tx,
        &state_tx,
        Arc::new(RwLock::new(None)),
        Duration::from_secs(60),
        "claude".into(),
        vec![],
        None,
        None,
        None,
        None,
        shutdown.clone(),
    );

    tokio::time::sleep(Duration::from_millis(50)).await;
    shutdown.cancel();
    tokio::time::sleep(Duration::from_millis(200)).await;

    assert_eq!(mock.get("sess-ev", "state_events.jsonl"), Some(b"state-events\n".to_vec()));
    assert_eq!(mock.get("sess-ev", "hook_events.jsonl"), Some(b"hook-events\n".to_vec()));
    Ok(())
}

#[tokio::test]
async fn subscriber_uploads_final_meta_with_exit_info() -> anyhow::Result<()> {
    let tmp = tempfile::tempdir()?;
    let session_dir = tmp.path().to_path_buf();
    let transcripts_dir = session_dir.join("transcripts");
    tokio::fs::create_dir_all(&transcripts_dir).await?;
    tokio::fs::write(transcripts_dir.join("1.jsonl"), b"t1\n").await?;
    tokio::fs::write(transcripts_dir.join("2.jsonl"), b"t2\n").await?;

    let mock = Arc::new(MockS3::new());
    let (transcript_tx, _) = broadcast::channel::<TranscriptEvent>(16);
    let (record_tx, _) = broadcast::channel::<RecordingEntry>(16);
    let (state_tx, _) = broadcast::channel::<crate::event::TransitionEvent>(16);
    let shutdown = CancellationToken::new();

    let handle = spawn_subscriber(
        Arc::clone(&mock),
        "sess-final".into(),
        Some(session_dir),
        &transcript_tx,
        &record_tx,
        &state_tx,
        Arc::new(RwLock::new(None)),
        Duration::from_secs(60),
        "claude".into(),
        vec!["project:gardenio".into()],
        Some("my-agent".into()),
        Some("gardenio".into()),
        Some("crew".into()),
        Some("ka-abc".into()),
        shutdown.clone(),
    );

    // Wait for initial meta upload.
    tokio::time::sleep(Duration::from_millis(50)).await;

    // Send two transcript events.
    for i in 1..=2 {
        let _ = transcript_tx.send(TranscriptEvent {
            number: i,
            timestamp: format!("2026-01-01T00:00:0{i}Z"),
            line_count: 10,
            seq: i.into(),
        });
        tokio::time::sleep(Duration::from_millis(50)).await;
    }

    // Set exit info before shutdown.
    handle.set_exit(Some(0), "success".into());
    shutdown.cancel();
    tokio::time::sleep(Duration::from_millis(200)).await;

    // Verify final meta was uploaded with all fields.
    let meta = mock.download_meta("sess-final").await?;
    let meta = meta.ok_or_else(|| anyhow::anyhow!("expected final metadata"))?;
    assert_eq!(meta.session_id, "sess-final");
    assert_eq!(meta.agent_type, "claude");
    assert!(meta.ended_at.is_some());
    assert_eq!(meta.exit_code, Some(0));
    assert_eq!(meta.exit_status.as_deref(), Some("success"));
    assert_eq!(meta.transcript_count, Some(2));
    assert_eq!(meta.agent_name.as_deref(), Some("my-agent"));
    assert_eq!(meta.project.as_deref(), Some("gardenio"));
    assert_eq!(meta.role.as_deref(), Some("crew"));
    assert_eq!(meta.bead_id.as_deref(), Some("ka-abc"));
    assert_eq!(meta.labels, vec!["project:gardenio"]);
    Ok(())
}

// ===========================================================================
// await_drain — deterministic flush before exit (credential-switch path)
// ===========================================================================

#[tokio::test]
async fn await_drain_flushes_pending_recording_before_exit() -> anyhow::Result<()> {
    // Simulates the credential-switch exit path: caller cancels shutdown
    // and awaits drain to ensure dirty in-memory state lands in S3 before
    // the process exits. Without await_drain, std::process::exit would
    // abort the subscriber tokio task mid-upload and the next spawn under
    // the same agent-bead-keyed prefix would resume from a stale sync point.
    let mock = Arc::new(MockS3::new());
    let (transcript_tx, _) = broadcast::channel::<TranscriptEvent>(16);
    let (record_tx, _) = broadcast::channel::<RecordingEntry>(256);
    let (state_tx, _) = broadcast::channel::<crate::event::TransitionEvent>(16);
    let shutdown = CancellationToken::new();

    let handle = spawn_subscriber(
        Arc::clone(&mock),
        "sess-drain".into(),
        None,
        &transcript_tx,
        &record_tx,
        &state_tx,
        Arc::new(RwLock::new(None)),
        Duration::from_secs(60),
        "claude".into(),
        vec![],
        None,
        None,
        None,
        None,
        shutdown.clone(),
    );

    // Wait for the initial meta upload so the subscriber is in its select loop.
    tokio::time::sleep(Duration::from_millis(50)).await;

    // Send fewer than batch_size (50) entries — buffered, not yet flushed.
    for i in 1..=7 {
        let _ = record_tx.send(test_recording_entry(i));
    }
    tokio::time::sleep(Duration::from_millis(50)).await;
    assert!(
        mock.get("sess-drain", "recording/1-7.jsonl").is_none(),
        "buffer should not have flushed below batch threshold yet"
    );

    // Stamp exit info as the credential-switch path would, then cancel
    // shutdown and deterministically await drain.
    handle.set_exit(Some(crate::run::EXIT_CODE_CREDENTIAL_SWITCH), "switch".into());
    shutdown.cancel();
    let drained = handle.await_drain(Duration::from_secs(5)).await;
    assert!(drained, "subscriber should drain within timeout");

    // Buffered recording entries must be in S3 now — no sleep, no race.
    let chunk = mock.get("sess-drain", "recording/1-7.jsonl");
    assert!(chunk.is_some(), "recording buffer should be flushed before drain returns");
    let chunk = chunk.ok_or_else(|| anyhow::anyhow!("missing chunk"))?;
    let lines: Vec<&str> = std::str::from_utf8(&chunk)?.lines().collect();
    assert_eq!(lines.len(), 7);

    // Final meta should reflect the credential-switch exit so the next
    // spawn can see this session ended on rotation.
    let meta = mock.download_meta("sess-drain").await?;
    let meta = meta.ok_or_else(|| anyhow::anyhow!("expected final meta"))?;
    assert_eq!(meta.exit_code, Some(crate::run::EXIT_CODE_CREDENTIAL_SWITCH));
    assert_eq!(meta.exit_status.as_deref(), Some("switch"));
    assert!(meta.ended_at.is_some());
    Ok(())
}

#[tokio::test]
async fn finalize_captures_final_snapshot_and_exit_info() -> anyhow::Result<()> {
    // Regression for ka-0000001666: on a terminal exit the session loop saves
    // a final transcript snapshot (emitting a TranscriptEvent) and stamps exit
    // info, THEN finalizes the subscriber. Because the subscriber drains its
    // OWN token (decoupled from session shutdown), finalize must capture that
    // last-moment transcript and persist the stamped exit info — not the
    // placeholder (exit_status="error", transcript_count=0) that an external
    // shutdown-token cancellation used to upload by racing ahead.
    let tmp = tempfile::tempdir()?;
    let session_dir = tmp.path().to_path_buf();
    let transcripts_dir = session_dir.join("transcripts");
    tokio::fs::create_dir_all(&transcripts_dir).await?;
    tokio::fs::write(transcripts_dir.join("1.jsonl"), b"final-snapshot\n").await?;

    let mock = Arc::new(MockS3::new());
    let (transcript_tx, _) = broadcast::channel::<TranscriptEvent>(16);
    let (record_tx, _) = broadcast::channel::<RecordingEntry>(16);
    let (state_tx, _) = broadcast::channel::<crate::event::TransitionEvent>(16);
    let drain = CancellationToken::new();

    let handle = spawn_subscriber(
        Arc::clone(&mock),
        "sess-finalize".into(),
        Some(session_dir),
        &transcript_tx,
        &record_tx,
        &state_tx,
        Arc::new(RwLock::new(None)),
        Duration::from_secs(60),
        "claude".into(),
        vec![],
        None,
        None,
        None,
        None,
        drain.clone(),
    );

    // Subscriber reaches its select loop and uploads the initial meta.
    tokio::time::sleep(Duration::from_millis(50)).await;

    // Mirror the real exit ordering: emit the final snapshot event, stamp the
    // real exit, then finalize — all with NO intervening sleep, so the drain
    // arm (not a periodic flush) is what must capture the transcript.
    let _ = transcript_tx.send(TranscriptEvent {
        number: 1,
        timestamp: "2026-01-01T00:00:00Z".into(),
        line_count: 1,
        seq: 1,
    });
    handle.set_exit(Some(0), "success".into());
    assert!(
        handle.finalize(Duration::from_secs(5)).await,
        "subscriber should finalize within timeout"
    );

    let data = mock.get("sess-finalize", "transcripts/1.jsonl");
    assert_eq!(
        data,
        Some(b"final-snapshot\n".to_vec()),
        "final transcript snapshot must land in S3 via the drain arm"
    );

    let meta = mock.download_meta("sess-finalize").await?;
    let meta = meta.ok_or_else(|| anyhow::anyhow!("expected final meta"))?;
    assert_eq!(
        meta.exit_status.as_deref(),
        Some("success"),
        "must persist stamped exit, not placeholder error"
    );
    assert_eq!(meta.exit_code, Some(0));
    assert_eq!(meta.transcript_count, Some(1), "drained transcript must be counted in final meta");
    assert!(meta.ended_at.is_some());
    Ok(())
}

#[tokio::test]
async fn await_drain_is_idempotent() -> anyhow::Result<()> {
    let mock = Arc::new(MockS3::new());
    let (transcript_tx, _) = broadcast::channel::<TranscriptEvent>(16);
    let (record_tx, _) = broadcast::channel::<RecordingEntry>(16);
    let (state_tx, _) = broadcast::channel::<crate::event::TransitionEvent>(16);
    let shutdown = CancellationToken::new();

    let handle = spawn_subscriber(
        Arc::clone(&mock),
        "sess-idem".into(),
        None,
        &transcript_tx,
        &record_tx,
        &state_tx,
        Arc::new(RwLock::new(None)),
        Duration::from_secs(60),
        "claude".into(),
        vec![],
        None,
        None,
        None,
        None,
        shutdown.clone(),
    );

    tokio::time::sleep(Duration::from_millis(50)).await;
    shutdown.cancel();
    assert!(handle.await_drain(Duration::from_secs(5)).await);
    // Calling again after the join handle has been consumed must succeed
    // trivially rather than panic — the credential-switch path may end up
    // calling this defensively.
    assert!(handle.await_drain(Duration::from_secs(1)).await);
    Ok(())
}

// ===========================================================================
// Edge cases and error scenarios
// ===========================================================================

#[tokio::test]
async fn subscriber_handles_no_session_dir() -> anyhow::Result<()> {
    let mock = Arc::new(MockS3::new());
    let (transcript_tx, _) = broadcast::channel::<TranscriptEvent>(16);
    let (record_tx, _) = broadcast::channel::<RecordingEntry>(16);
    let (state_tx, _) = broadcast::channel::<crate::event::TransitionEvent>(16);
    let shutdown = CancellationToken::new();

    // No session_dir, no session_log_path.
    spawn_subscriber(
        Arc::clone(&mock),
        "sess-none".into(),
        None,
        &transcript_tx,
        &record_tx,
        &state_tx,
        Arc::new(RwLock::new(None)),
        Duration::from_secs(60),
        "claude".into(),
        vec![],
        None,
        None,
        None,
        None,
        shutdown.clone(),
    );

    tokio::time::sleep(Duration::from_millis(50)).await;
    shutdown.cancel();
    tokio::time::sleep(Duration::from_millis(100)).await;

    // Only metadata should be uploaded.
    let meta = mock.download_meta("sess-none").await?;
    assert!(meta.is_some());
    // No event logs or session log.
    assert!(mock.get("sess-none", "state_events.jsonl").is_none());
    assert!(mock.get("sess-none", "session.jsonl").is_none());
    Ok(())
}

#[tokio::test]
async fn subscriber_survives_s3_upload_errors() -> anyhow::Result<()> {
    // FailingS3 returns errors for all operations, but the subscriber should
    // not crash — it logs warnings and continues.
    let mock = Arc::new(FailingS3);
    let (transcript_tx, _) = broadcast::channel::<TranscriptEvent>(16);
    let (record_tx, _) = broadcast::channel::<RecordingEntry>(16);
    let (state_tx, _) = broadcast::channel::<crate::event::TransitionEvent>(16);
    let shutdown = CancellationToken::new();

    spawn_subscriber(
        Arc::clone(&mock),
        "sess-fail".into(),
        None,
        &transcript_tx,
        &record_tx,
        &state_tx,
        Arc::new(RwLock::new(None)),
        Duration::from_secs(60),
        "claude".into(),
        vec![],
        None,
        None,
        None,
        None,
        shutdown.clone(),
    );

    tokio::time::sleep(Duration::from_millis(50)).await;

    // Send events — should not crash despite S3 errors.
    for i in 1..=3 {
        let _ = record_tx.send(test_recording_entry(i));
    }
    tokio::time::sleep(Duration::from_millis(100)).await;

    shutdown.cancel();
    tokio::time::sleep(Duration::from_millis(100)).await;

    // Test passes if we get here without panic.
    Ok(())
}

#[tokio::test]
async fn restore_transcripts_with_failing_s3() -> anyhow::Result<()> {
    let tmp = tempfile::tempdir()?;
    let mock = FailingS3;
    let dir = tmp.path().join("transcripts");

    let result = restore_transcripts(&mock, "sess-1", &dir).await;
    // list_transcripts fails, so the whole function fails.
    assert!(result.is_err());
    Ok(())
}

#[tokio::test]
async fn flush_recording_buffer_empty_is_noop() -> anyhow::Result<()> {
    let mock = MockS3::new();
    let initial_count = mock.object_count();
    let mut buffer = Vec::new();

    flush_recording_buffer(&mock, "sess-1", &mut buffer).await;

    // No new objects should have been created.
    assert_eq!(mock.object_count(), initial_count);
    Ok(())
}

#[tokio::test]
async fn flush_recording_buffer_builds_correct_jsonl() -> anyhow::Result<()> {
    let mock = MockS3::new();
    let mut buffer =
        vec![test_recording_entry(10), test_recording_entry(11), test_recording_entry(12)];

    flush_recording_buffer(&mock, "sess-1", &mut buffer).await;

    // Buffer should be cleared.
    assert!(buffer.is_empty());

    // Check the uploaded chunk.
    let data = mock.get("sess-1", "recording/10-12.jsonl");
    assert!(data.is_some());
    let data = data.ok_or_else(|| anyhow::anyhow!("missing chunk"))?;
    let content = std::str::from_utf8(&data)?;
    let lines: Vec<&str> = content.lines().collect();
    assert_eq!(lines.len(), 3);

    // Each line should be valid JSON.
    for line in &lines {
        let entry: RecordingEntry = serde_json::from_str(line)?;
        assert_eq!(entry.kind, "state");
    }
    Ok(())
}

#[tokio::test]
async fn upload_session_log_missing_path_is_noop() -> anyhow::Result<()> {
    let mock = MockS3::new();
    // None path — should return immediately.
    upload_session_log(&mock, "sess-1", None).await;
    assert_eq!(mock.object_count(), 0);
    Ok(())
}

#[tokio::test]
async fn upload_session_log_missing_file_is_noop() -> anyhow::Result<()> {
    let mock = MockS3::new();
    let nonexistent = Path::new("/tmp/coop-test-does-not-exist-session.jsonl");
    upload_session_log(&mock, "sess-1", Some(nonexistent)).await;
    assert_eq!(mock.object_count(), 0);
    Ok(())
}

#[tokio::test]
async fn upload_event_logs_missing_dir_is_noop() -> anyhow::Result<()> {
    let mock = MockS3::new();
    // None session_dir — should return immediately.
    upload_event_logs(&mock, "sess-1", None).await;
    assert_eq!(mock.object_count(), 0);
    Ok(())
}

#[tokio::test]
async fn upload_event_logs_partial_files() -> anyhow::Result<()> {
    let tmp = tempfile::tempdir()?;
    let mock = MockS3::new();

    // Only create state_events, not hook_events.
    tokio::fs::write(tmp.path().join("state_events.jsonl"), b"events\n").await?;

    upload_event_logs(&mock, "sess-1", Some(tmp.path())).await;

    assert!(mock.get("sess-1", "state_events.jsonl").is_some());
    assert!(mock.get("sess-1", "hook_events.jsonl").is_none());
    Ok(())
}

#[tokio::test]
async fn subscriber_periodic_upload_triggers() -> anyhow::Result<()> {
    let tmp = tempfile::tempdir()?;
    let session_dir = tmp.path().to_path_buf();
    let session_log = session_dir.join("session.jsonl");
    tokio::fs::write(&session_log, b"periodic-test\n").await?;

    let mock = Arc::new(MockS3::new());
    let (transcript_tx, _) = broadcast::channel::<TranscriptEvent>(16);
    let (record_tx, _) = broadcast::channel::<RecordingEntry>(16);
    let (state_tx, _) = broadcast::channel::<crate::event::TransitionEvent>(16);
    let shutdown = CancellationToken::new();

    // Short upload interval to trigger periodic upload quickly.
    spawn_subscriber(
        Arc::clone(&mock),
        "sess-periodic".into(),
        Some(session_dir),
        &transcript_tx,
        &record_tx,
        &state_tx,
        Arc::new(RwLock::new(Some(session_log))),
        Duration::from_millis(50), // 50ms interval
        "claude".into(),
        vec![],
        None,
        None,
        None,
        None,
        shutdown.clone(),
    );

    // Wait longer than the interval.
    tokio::time::sleep(Duration::from_millis(300)).await;
    shutdown.cancel();
    tokio::time::sleep(Duration::from_millis(50)).await;

    // Session log should have been uploaded by the periodic timer.
    let log_data = mock.get("sess-periodic", "session.jsonl");
    assert_eq!(log_data, Some(b"periodic-test\n".to_vec()));
    Ok(())
}

#[tokio::test]
async fn subscriber_refreshes_meta_on_state_transition() -> anyhow::Result<()> {
    use crate::driver::AgentState;
    use crate::event::TransitionEvent;

    let tmp = tempfile::tempdir()?;
    let session_dir = tmp.path().to_path_buf();
    let session_log = session_dir.join("session.jsonl");
    tokio::fs::write(&session_log, b"line-1\n").await?;

    let mock = Arc::new(MockS3::new());
    let (transcript_tx, _) = broadcast::channel::<TranscriptEvent>(16);
    let (record_tx, _) = broadcast::channel::<RecordingEntry>(16);
    let (state_tx, _) = broadcast::channel::<TransitionEvent>(16);
    let shutdown = CancellationToken::new();

    spawn_subscriber(
        Arc::clone(&mock),
        "sess-trans".into(),
        Some(session_dir),
        &transcript_tx,
        &record_tx,
        &state_tx,
        Arc::new(RwLock::new(Some(session_log.clone()))),
        // Long interval so the periodic timer cannot win the race.
        Duration::from_secs(3600),
        "claude".into(),
        vec!["project:gardenio".into()],
        None,
        None,
        None,
        None,
        shutdown.clone(),
    );

    // Wait for initial meta upload to complete.
    tokio::time::sleep(Duration::from_millis(100)).await;

    // Initial meta is present but session.jsonl has not been uploaded yet
    // (it is only flushed on transcript / state / periodic / shutdown events).
    assert!(mock.get("sess-trans", "meta.json").is_some());
    assert!(mock.get("sess-trans", "session.jsonl").is_none());

    // Append more data to the session log and emit a state transition.
    tokio::fs::write(&session_log, b"line-1\nline-2\n").await?;
    let _ = state_tx.send(TransitionEvent {
        prev: AgentState::Starting,
        next: AgentState::Working,
        seq: 1,
        cause: "test".into(),
        last_message: None,
    });

    // Allow the subscriber to process the transition.
    tokio::time::sleep(Duration::from_millis(100)).await;

    // Meta is still present, and the session log has been pushed up
    // *because of* the state transition (acceptance criterion 2 + 1).
    let meta_bytes =
        mock.get("sess-trans", "meta.json").ok_or_else(|| anyhow::anyhow!("meta missing"))?;
    let meta: SessionMeta = serde_json::from_slice(&meta_bytes)?;
    assert_eq!(meta.session_id, "sess-trans");
    assert!(meta.ended_at.is_none(), "ended_at should still be None mid-session");
    let log_data = mock.get("sess-trans", "session.jsonl");
    assert_eq!(log_data, Some(b"line-1\nline-2\n".to_vec()));

    shutdown.cancel();
    tokio::time::sleep(Duration::from_millis(50)).await;
    Ok(())
}

#[tokio::test]
async fn subscriber_multiple_transcript_events() -> anyhow::Result<()> {
    let tmp = tempfile::tempdir()?;
    let session_dir = tmp.path().to_path_buf();
    let transcripts_dir = session_dir.join("transcripts");
    tokio::fs::create_dir_all(&transcripts_dir).await?;

    // Write multiple transcript files.
    tokio::fs::write(transcripts_dir.join("1.jsonl"), b"t1\n").await?;
    tokio::fs::write(transcripts_dir.join("2.jsonl"), b"t2\n").await?;
    tokio::fs::write(transcripts_dir.join("3.jsonl"), b"t3\n").await?;

    let mock = Arc::new(MockS3::new());
    let (transcript_tx, _) = broadcast::channel::<TranscriptEvent>(16);
    let (record_tx, _) = broadcast::channel::<RecordingEntry>(16);
    let (state_tx, _) = broadcast::channel::<crate::event::TransitionEvent>(16);
    let shutdown = CancellationToken::new();

    spawn_subscriber(
        Arc::clone(&mock),
        "sess-multi".into(),
        Some(session_dir),
        &transcript_tx,
        &record_tx,
        &state_tx,
        Arc::new(RwLock::new(None)),
        Duration::from_secs(60),
        "claude".into(),
        vec![],
        None,
        None,
        None,
        None,
        shutdown.clone(),
    );

    tokio::time::sleep(Duration::from_millis(50)).await;

    // Send transcript events sequentially.
    for i in 1..=3 {
        let _ = transcript_tx.send(TranscriptEvent {
            number: i,
            timestamp: format!("2026-01-01T00:00:0{i}Z"),
            line_count: (i * 10).into(),
            seq: i.into(),
        });
        tokio::time::sleep(Duration::from_millis(50)).await;
    }

    tokio::time::sleep(Duration::from_millis(100)).await;
    shutdown.cancel();
    tokio::time::sleep(Duration::from_millis(50)).await;

    assert_eq!(mock.get("sess-multi", "transcripts/1.jsonl"), Some(b"t1\n".to_vec()));
    assert_eq!(mock.get("sess-multi", "transcripts/2.jsonl"), Some(b"t2\n".to_vec()));
    assert_eq!(mock.get("sess-multi", "transcripts/3.jsonl"), Some(b"t3\n".to_vec()));
    Ok(())
}

// ---------------------------------------------------------------------------
// Phase 5 — list_sessions, list_recording_chunks, download_bytes
// ---------------------------------------------------------------------------

#[tokio::test]
async fn mock_list_sessions_empty() -> anyhow::Result<()> {
    let mock = MockS3::new();
    let sessions = mock.list_sessions().await?;
    assert!(sessions.is_empty());
    Ok(())
}

#[tokio::test]
async fn mock_list_sessions_multiple() -> anyhow::Result<()> {
    let mock = MockS3::new();
    mock.put("alpha", "meta.json", b"{}".to_vec());
    mock.put("beta", "transcripts/1.jsonl", b"t1".to_vec());
    mock.put("gamma", "recording/chunk-0000.jsonl", b"r1".to_vec());
    // Same session appears in multiple keys — should deduplicate.
    mock.put("alpha", "transcripts/1.jsonl", b"t1".to_vec());

    let mut sessions = mock.list_sessions().await?;
    sessions.sort();
    assert_eq!(sessions, vec!["alpha", "beta", "gamma"]);
    Ok(())
}

#[tokio::test]
async fn mock_list_recording_chunks_empty() -> anyhow::Result<()> {
    let mock = MockS3::new();
    // Session exists but has no recording/ objects.
    mock.put("sess-1", "meta.json", b"{}".to_vec());
    let chunks = mock.list_recording_chunks("sess-1").await?;
    assert!(chunks.is_empty());
    Ok(())
}

#[tokio::test]
async fn mock_list_recording_chunks_sorted() -> anyhow::Result<()> {
    let mock = MockS3::new();
    mock.put("sess-1", "recording/chunk-0002.jsonl", b"r2".to_vec());
    mock.put("sess-1", "recording/chunk-0000.jsonl", b"r0".to_vec());
    mock.put("sess-1", "recording/chunk-0001.jsonl", b"r1".to_vec());
    // Non-jsonl file under recording/ should be excluded.
    mock.put("sess-1", "recording/index.txt", b"idx".to_vec());
    // Object under a different session should not appear.
    mock.put("sess-2", "recording/chunk-0000.jsonl", b"other".to_vec());

    let chunks = mock.list_recording_chunks("sess-1").await?;
    assert_eq!(chunks, vec!["chunk-0000.jsonl", "chunk-0001.jsonl", "chunk-0002.jsonl"]);
    Ok(())
}

#[tokio::test]
async fn mock_download_bytes_roundtrip() -> anyhow::Result<()> {
    let mock = MockS3::new();
    let payload = b"hello world".to_vec();
    mock.put("sess-1", "artifact.bin", payload.clone());

    let downloaded = mock.download_bytes("sess-1", "artifact.bin").await?;
    assert_eq!(downloaded, payload);
    Ok(())
}

#[tokio::test]
async fn mock_download_bytes_missing_errors() -> anyhow::Result<()> {
    let mock = MockS3::new();
    let result = mock.download_bytes("no-such-session", "missing.bin").await;
    assert!(result.is_err());
    assert!(result.unwrap_err().to_string().contains("object not found"));
    Ok(())
}

#[tokio::test]
async fn failing_s3_list_sessions_errors() -> anyhow::Result<()> {
    let fail = FailingS3;
    let result = fail.list_sessions().await;
    assert!(result.is_err());
    Ok(())
}

#[tokio::test]
async fn failing_s3_list_recording_chunks_errors() -> anyhow::Result<()> {
    let fail = FailingS3;
    let result = fail.list_recording_chunks("any").await;
    assert!(result.is_err());
    Ok(())
}

#[tokio::test]
async fn failing_s3_download_bytes_errors() -> anyhow::Result<()> {
    let fail = FailingS3;
    let result = fail.download_bytes("any", "any.bin").await;
    assert!(result.is_err());
    Ok(())
}

#[tokio::test]
async fn subscriber_drains_pending_transcripts_on_shutdown() -> anyhow::Result<()> {
    let tmp = tempfile::tempdir()?;
    let session_dir = tmp.path().to_path_buf();
    let transcripts_dir = session_dir.join("transcripts");
    tokio::fs::create_dir_all(&transcripts_dir).await?;
    tokio::fs::write(transcripts_dir.join("1.jsonl"), b"final-transcript\n").await?;

    let mock = Arc::new(MockS3::new());
    let (transcript_tx, _) = broadcast::channel::<TranscriptEvent>(16);
    let (record_tx, _) = broadcast::channel::<RecordingEntry>(16);
    let (state_tx, _) = broadcast::channel::<crate::event::TransitionEvent>(16);
    let shutdown = CancellationToken::new();

    let handle = spawn_subscriber(
        Arc::clone(&mock),
        "sess-drain".into(),
        Some(session_dir),
        &transcript_tx,
        &record_tx,
        &state_tx,
        Arc::new(RwLock::new(None)),
        Duration::from_secs(60),
        "claude".into(),
        vec![],
        None,
        None,
        None,
        None,
        shutdown.clone(),
    );

    // Wait for initial meta upload.
    tokio::time::sleep(Duration::from_millis(50)).await;

    // Send a transcript event and cancel shutdown immediately, simulating
    // the race where a final transcript snapshot is saved just before
    // the session ends and the shutdown token fires.
    let _ = transcript_tx.send(TranscriptEvent {
        number: 1,
        timestamp: "2026-01-01T00:00:01Z".into(),
        line_count: 1,
        seq: 1,
    });
    handle.set_exit(Some(0), "success".into());
    shutdown.cancel();
    tokio::time::sleep(Duration::from_millis(200)).await;

    // The shutdown drain should have picked up the transcript event and
    // uploaded it, even though cancellation happened near-simultaneously.
    let data = mock.get("sess-drain", "transcripts/1.jsonl");
    assert_eq!(data, Some(b"final-transcript\n".to_vec()));

    // Final metadata should reflect the drained transcript count.
    let meta = mock.download_meta("sess-drain").await?;
    let meta = meta.ok_or_else(|| anyhow::anyhow!("expected final metadata"))?;
    assert_eq!(meta.transcript_count, Some(1));
    assert_eq!(meta.exit_code, Some(0));
    assert_eq!(meta.exit_status.as_deref(), Some("success"));
    Ok(())
}

// ===========================================================================
// Multi-spawn / orphan handling — ka-0000002503
// ===========================================================================

/// Write a meta.json under a composed `<bead>/<spawn>` key with the supplied
/// `started_at`. Helper for the spawn-picker tests.
async fn put_spawn_meta(mock: &MockS3, bead: &str, spawn: &str, started_at: u64) {
    let meta = SessionMeta {
        session_id: format!("{bead}/{spawn}"),
        agent_type: "claude".into(),
        started_at,
        ended_at: None,
        exit_code: None,
        labels: vec![],
        agent_name: None,
        project: None,
        role: None,
        bead_id: Some(bead.into()),
        exit_status: None,
        transcript_count: None,
        harness: None,
    };
    let composed = format!("{bead}/{spawn}");
    let _ = mock.upload_meta(&composed, &meta).await;
}

#[test]
fn compose_session_key_uses_bead_and_spawn() {
    let key = compose_session_key(Some("ka-bead"), None, "coop-uuid");
    assert_eq!(key, "ka-bead/coop-uuid");
}

#[test]
fn compose_session_key_falls_back_to_resume_without_bead() {
    let key = compose_session_key(None, Some("legacy-resume"), "coop-uuid");
    assert_eq!(key, "legacy-resume");
}

#[test]
fn compose_session_key_falls_back_to_coop_id() {
    let key = compose_session_key(None, None, "coop-uuid");
    assert_eq!(key, "coop-uuid");
}

#[test]
fn compose_session_key_treats_empty_bead_as_unset() {
    let key = compose_session_key(Some(""), Some("legacy-resume"), "coop-uuid");
    assert_eq!(key, "legacy-resume");
    let key = compose_session_key(Some("   "), None, "coop-uuid");
    assert_eq!(key, "coop-uuid");
}

#[test]
fn compose_session_key_trims_bead_whitespace() {
    let key = compose_session_key(Some("  ka-bead  "), None, "coop-uuid");
    assert_eq!(key, "ka-bead/coop-uuid");
}

#[tokio::test]
async fn list_spawns_returns_only_spawns_with_meta() -> anyhow::Result<()> {
    let mock = MockS3::new();
    // Two spawns, each with a meta.json under <bead>/<spawn>/meta.json.
    put_spawn_meta(&mock, "bead-1", "spawn-A", 1000).await;
    put_spawn_meta(&mock, "bead-1", "spawn-B", 2000).await;

    // Legacy flat-layout artifacts under <bead>/ should not be confused for
    // spawns just because they share the same bead prefix.
    mock.put("bead-1", "transcripts/1.jsonl", b"legacy".to_vec());
    mock.put("bead-1", "meta.json", b"{}".to_vec());
    mock.put("bead-1", "session.jsonl", b"log".to_vec());

    // A spawn's transcript directory must not be parsed as a deeper spawn.
    mock.put("bead-1/spawn-A", "transcripts/1.jsonl", b"t1".to_vec());

    let spawns = mock.list_spawns("bead-1").await?;
    assert_eq!(spawns, vec!["spawn-A".to_owned(), "spawn-B".to_owned()]);

    // A bead with no spawn meta returns empty.
    let spawns = mock.list_spawns("bead-empty").await?;
    assert!(spawns.is_empty());
    Ok(())
}

#[tokio::test]
async fn pick_latest_spawn_picks_highest_started_at() -> anyhow::Result<()> {
    let mock = MockS3::new();
    put_spawn_meta(&mock, "bead-1", "spawn-old", 1000).await;
    put_spawn_meta(&mock, "bead-1", "spawn-mid", 2000).await;
    put_spawn_meta(&mock, "bead-1", "spawn-new", 3000).await;

    let latest = pick_latest_spawn(&mock, "bead-1").await;
    assert_eq!(latest, Some("bead-1/spawn-new".to_owned()));
    Ok(())
}

#[tokio::test]
async fn pick_latest_spawn_breaks_ties_lexicographically() -> anyhow::Result<()> {
    let mock = MockS3::new();
    put_spawn_meta(&mock, "bead-1", "spawn-a", 1000).await;
    put_spawn_meta(&mock, "bead-1", "spawn-b", 1000).await;
    put_spawn_meta(&mock, "bead-1", "spawn-c", 1000).await;

    // Tie-break is descending lex order on the composed key.
    let latest = pick_latest_spawn(&mock, "bead-1").await;
    assert_eq!(latest, Some("bead-1/spawn-c".to_owned()));
    Ok(())
}

#[tokio::test]
async fn pick_latest_spawn_skips_spawns_without_meta() -> anyhow::Result<()> {
    let mock = MockS3::new();
    put_spawn_meta(&mock, "bead-1", "spawn-good", 1000).await;
    // A spawn with artifacts but no meta.json should not be picked
    // (it also won't be discovered by list_spawns, which keys on meta.json).
    mock.put("bead-1/spawn-orphan", "transcripts/1.jsonl", b"t".to_vec());

    let latest = pick_latest_spawn(&mock, "bead-1").await;
    assert_eq!(latest, Some("bead-1/spawn-good".to_owned()));
    Ok(())
}

#[tokio::test]
async fn pick_latest_spawn_returns_none_when_empty() -> anyhow::Result<()> {
    let mock = MockS3::new();
    let latest = pick_latest_spawn(&mock, "bead-empty").await;
    assert!(latest.is_none());
    Ok(())
}

#[tokio::test]
async fn pick_restore_key_picks_latest_spawn_when_bead_set() -> anyhow::Result<()> {
    let mock = MockS3::new();
    put_spawn_meta(&mock, "bead-1", "spawn-A", 1000).await;
    put_spawn_meta(&mock, "bead-1", "spawn-B", 5000).await;

    let key = pick_restore_key(&mock, Some("bead-1"), None).await;
    assert_eq!(key, Some("bead-1/spawn-B".to_owned()));
    Ok(())
}

#[tokio::test]
async fn pick_restore_key_falls_back_to_bead_for_legacy_layout() -> anyhow::Result<()> {
    let mock = MockS3::new();
    // Legacy flat artifacts under <bead>/ — no spawn-shaped sub-prefix.
    mock.put("bead-legacy", "meta.json", b"{}".to_vec());
    mock.put("bead-legacy", "transcripts/1.jsonl", b"t1".to_vec());

    let key = pick_restore_key(&mock, Some("bead-legacy"), None).await;
    // No spawn meta found → fall back to flat layout under <bead>.
    assert_eq!(key, Some("bead-legacy".to_owned()));
    Ok(())
}

#[tokio::test]
async fn pick_restore_key_falls_back_to_bead_when_completely_empty() -> anyhow::Result<()> {
    let mock = MockS3::new();
    let key = pick_restore_key(&mock, Some("bead-fresh"), None).await;
    // Fresh start: no data anywhere. Restore code must still return a key
    // so callers can attempt restore (which will be a no-op).
    assert_eq!(key, Some("bead-fresh".to_owned()));
    Ok(())
}

#[tokio::test]
async fn pick_restore_key_uses_resume_without_bead() -> anyhow::Result<()> {
    let mock = MockS3::new();
    let key = pick_restore_key(&mock, None, Some("legacy-resume-id")).await;
    assert_eq!(key, Some("legacy-resume-id".to_owned()));
    Ok(())
}

#[tokio::test]
async fn pick_restore_key_returns_none_without_bead_or_resume() -> anyhow::Result<()> {
    let mock = MockS3::new();
    let key = pick_restore_key(&mock, None, None).await;
    assert!(key.is_none());
    let key = pick_restore_key(&mock, Some("   "), Some("")).await;
    assert!(key.is_none());
    Ok(())
}

#[tokio::test]
async fn restore_uses_latest_spawn_layout_end_to_end() -> anyhow::Result<()> {
    let tmp = tempfile::tempdir()?;
    let mock = Arc::new(MockS3::new());

    // Two spawns under the same bead. The newer one has a different
    // transcript so we can verify which one is picked.
    put_spawn_meta(&mock, "bead-1", "spawn-old", 1000).await;
    mock.put("bead-1/spawn-old", "transcripts/1.jsonl", b"old-data\n".to_vec());

    put_spawn_meta(&mock, "bead-1", "spawn-new", 2000).await;
    mock.put("bead-1/spawn-new", "transcripts/1.jsonl", b"new-data\n".to_vec());
    mock.put("bead-1/spawn-new", "session.jsonl", b"new-log\n".to_vec());

    // The restore picker resolves to the latest spawn key.
    let restore_key = pick_restore_key(mock.as_ref(), Some("bead-1"), None).await;
    let restore_key = restore_key.ok_or_else(|| anyhow::anyhow!("expected key"))?;
    assert_eq!(restore_key, "bead-1/spawn-new");

    // Restoring transcripts pulls from the latest spawn only.
    let dir = tmp.path().join("transcripts");
    let count = restore_transcripts(mock.as_ref(), &restore_key, &dir).await?;
    assert_eq!(count, 1);
    assert_eq!(tokio::fs::read_to_string(dir.join("1.jsonl")).await?, "new-data\n");

    // Session log restore uses the same key.
    let log_dest = tmp.path().join("session.jsonl");
    restore_session_log(mock.as_ref(), &restore_key, &log_dest).await?;
    assert_eq!(tokio::fs::read_to_string(&log_dest).await?, "new-log\n");
    Ok(())
}

#[tokio::test]
async fn subscriber_writes_under_composed_key() -> anyhow::Result<()> {
    // End-to-end: the subscriber accepts an opaque session_id, and we feed
    // it the composed `<bead>/<spawn>` key. Every artifact is stored under
    // that prefix in S3.
    let tmp = tempfile::tempdir()?;
    let session_dir = tmp.path().to_path_buf();
    let transcripts_dir = session_dir.join("transcripts");
    tokio::fs::create_dir_all(&transcripts_dir).await?;
    tokio::fs::write(transcripts_dir.join("1.jsonl"), b"t1\n").await?;

    let mock = Arc::new(MockS3::new());
    let (transcript_tx, _) = broadcast::channel::<TranscriptEvent>(16);
    let (record_tx, _) = broadcast::channel::<RecordingEntry>(16);
    let (state_tx, _) = broadcast::channel::<crate::event::TransitionEvent>(16);
    let shutdown = CancellationToken::new();

    // Compose the key the way run.rs will: bead set, no resume, coop UUID
    // is the spawn discriminator.
    let composed = compose_session_key(Some("ka-bead"), None, "spawn-XYZ");
    assert_eq!(composed, "ka-bead/spawn-XYZ");

    let handle = spawn_subscriber(
        Arc::clone(&mock),
        composed.clone(),
        Some(session_dir),
        &transcript_tx,
        &record_tx,
        &state_tx,
        Arc::new(RwLock::new(None)),
        Duration::from_secs(60),
        "claude".into(),
        vec![],
        None,
        None,
        None,
        Some("ka-bead".into()),
        shutdown.clone(),
    );

    // Initial meta upload.
    tokio::time::sleep(Duration::from_millis(50)).await;

    let _ = transcript_tx.send(TranscriptEvent {
        number: 1,
        timestamp: "2026-01-01T00:00:00Z".into(),
        line_count: 1,
        seq: 1,
    });

    tokio::time::sleep(Duration::from_millis(100)).await;
    handle.set_exit(Some(0), "success".into());
    shutdown.cancel();
    tokio::time::sleep(Duration::from_millis(100)).await;

    // Artifacts written under the composed key.
    assert!(mock.get(&composed, "meta.json").is_some());
    assert_eq!(mock.get(&composed, "transcripts/1.jsonl"), Some(b"t1\n".to_vec()));

    // The bead lists exactly this spawn.
    let spawns = mock.list_spawns("ka-bead").await?;
    assert_eq!(spawns, vec!["spawn-XYZ".to_owned()]);

    // pick_restore_key returns the composed key for that bead.
    let key = pick_restore_key(mock.as_ref(), Some("ka-bead"), None).await;
    assert_eq!(key, Some(composed));
    Ok(())
}

// ===========================================================================
// Workflow companion dir — upload + restore (ka-0000000659)
//
// Dynamic workflows write per-run journals (agent-*.jsonl + run-state) into
// the Claude companion dir at `<config>/projects/<project>/<uuid>/` — the
// session log path with `.jsonl` stripped. These tests pin the upload (on
// periodic / state / shutdown cadence) and the S3-only restore so an
// interrupted Workflow run can resume via resumeFromRunId after off-PVC
// recovery instead of re-running from scratch.
// ===========================================================================

#[test]
fn workflow_companion_dir_strips_jsonl_suffix() {
    let log = Path::new("/home/a/.claude/projects/-proj/abc-123.jsonl");
    let dir = workflow_companion_dir(log);
    assert_eq!(dir, Some(PathBuf::from("/home/a/.claude/projects/-proj/abc-123")));
}

#[test]
fn workflow_companion_dir_none_without_jsonl_suffix() {
    assert!(workflow_companion_dir(Path::new("/home/a/session.log")).is_none());
    assert!(workflow_companion_dir(Path::new("/home/a/abc-123")).is_none());
}

#[tokio::test]
async fn mock_list_workflow_files_relative_and_sorted() -> anyhow::Result<()> {
    let mock = MockS3::new();
    mock.put("sess-1", "workflow/agent-2.jsonl", b"a2".to_vec());
    mock.put("sess-1", "workflow/agent-1.jsonl", b"a1".to_vec());
    mock.put("sess-1", "workflow/runs/wf_abc.json", b"state".to_vec());
    // Non-workflow keys and a different session must not leak in.
    mock.put("sess-1", "transcripts/1.jsonl", b"t".to_vec());
    mock.put("sess-2", "workflow/agent-9.jsonl", b"other".to_vec());

    let files = mock.list_workflow_files("sess-1").await?;
    assert_eq!(files, vec!["agent-1.jsonl", "agent-2.jsonl", "runs/wf_abc.json"]);

    let none = mock.list_workflow_files("sess-empty").await?;
    assert!(none.is_empty());
    Ok(())
}

#[tokio::test]
async fn upload_workflow_dir_uploads_nested_files() -> anyhow::Result<()> {
    let tmp = tempfile::tempdir()?;
    // Build a Claude-style session log path + sibling companion dir.
    let log_path = tmp.path().join("abc-123.jsonl");
    tokio::fs::write(&log_path, b"transcript\n").await?;
    let companion = tmp.path().join("abc-123");
    tokio::fs::create_dir_all(companion.join("runs")).await?;
    tokio::fs::write(companion.join("agent-1.jsonl"), b"agent-one\n").await?;
    tokio::fs::write(companion.join("agent-2.jsonl"), b"agent-two\n").await?;
    tokio::fs::write(companion.join("runs").join("wf_abc.json"), b"{\"step\":3}").await?;

    let mock = MockS3::new();
    upload_workflow_dir(&mock, "sess-wf", Some(&log_path)).await;

    assert_eq!(mock.get("sess-wf", "workflow/agent-1.jsonl"), Some(b"agent-one\n".to_vec()));
    assert_eq!(mock.get("sess-wf", "workflow/agent-2.jsonl"), Some(b"agent-two\n".to_vec()));
    assert_eq!(mock.get("sess-wf", "workflow/runs/wf_abc.json"), Some(b"{\"step\":3}".to_vec()));
    Ok(())
}

#[tokio::test]
async fn upload_workflow_dir_noop_when_dir_missing_or_path_unset() -> anyhow::Result<()> {
    let tmp = tempfile::tempdir()?;
    let mock = MockS3::new();

    // None log path → no-op.
    upload_workflow_dir(&mock, "s", None).await;
    assert_eq!(mock.object_count(), 0);

    // Log path present but companion dir was never created (no active
    // workflow run) → no-op.
    let log_path = tmp.path().join("no-companion.jsonl");
    tokio::fs::write(&log_path, b"x\n").await?;
    upload_workflow_dir(&mock, "s", Some(&log_path)).await;
    assert_eq!(mock.object_count(), 0);

    // Log path without a .jsonl suffix → no companion dir derivable → no-op.
    let weird = tmp.path().join("session.log");
    tokio::fs::write(&weird, b"x\n").await?;
    upload_workflow_dir(&mock, "s", Some(&weird)).await;
    assert_eq!(mock.object_count(), 0);
    Ok(())
}

#[tokio::test]
async fn restore_workflow_dir_downloads_all_and_recreates_layout() -> anyhow::Result<()> {
    let tmp = tempfile::tempdir()?;
    let mock = Arc::new(MockS3::new());
    mock.put("sess-1", "workflow/agent-1.jsonl", b"a1\n".to_vec());
    mock.put("sess-1", "workflow/agent-2.jsonl", b"a2\n".to_vec());
    mock.put("sess-1", "workflow/runs/wf_xyz.json", b"{\"run\":1}".to_vec());

    let companion = tmp.path().join("projects").join("-proj").join("uuid-1");
    let count = restore_workflow_dir(mock.as_ref(), "sess-1", &companion).await?;
    assert_eq!(count, 3);

    assert_eq!(tokio::fs::read_to_string(companion.join("agent-1.jsonl")).await?, "a1\n");
    assert_eq!(tokio::fs::read_to_string(companion.join("agent-2.jsonl")).await?, "a2\n");
    assert_eq!(
        tokio::fs::read_to_string(companion.join("runs").join("wf_xyz.json")).await?,
        "{\"run\":1}"
    );
    Ok(())
}

#[tokio::test]
async fn restore_workflow_dir_skips_existing_files() -> anyhow::Result<()> {
    let tmp = tempfile::tempdir()?;
    let mock = Arc::new(MockS3::new());
    mock.put("sess-1", "workflow/agent-1.jsonl", b"from-s3\n".to_vec());
    mock.put("sess-1", "workflow/agent-2.jsonl", b"from-s3\n".to_vec());

    let companion = tmp.path().join("uuid-1");
    tokio::fs::create_dir_all(&companion).await?;
    // A fresher local copy must NOT be clobbered by the S3 archive.
    tokio::fs::write(companion.join("agent-1.jsonl"), b"local-fresh\n").await?;

    let count = restore_workflow_dir(mock.as_ref(), "sess-1", &companion).await?;
    assert_eq!(count, 2); // 1 skipped + 1 downloaded.
    assert_eq!(tokio::fs::read_to_string(companion.join("agent-1.jsonl")).await?, "local-fresh\n");
    assert_eq!(tokio::fs::read_to_string(companion.join("agent-2.jsonl")).await?, "from-s3\n");
    Ok(())
}

#[tokio::test]
async fn restore_workflow_dir_empty_is_noop() -> anyhow::Result<()> {
    let tmp = tempfile::tempdir()?;
    let mock = Arc::new(MockS3::new());
    let companion = tmp.path().join("uuid-empty");

    let count = restore_workflow_dir(mock.as_ref(), "no-such-session", &companion).await?;
    assert_eq!(count, 0);
    // With nothing to restore, we must not eagerly create the companion dir.
    assert!(!companion.exists());
    Ok(())
}

#[tokio::test]
async fn restore_workflow_dir_with_failing_s3_errors() -> anyhow::Result<()> {
    let tmp = tempfile::tempdir()?;
    let fail = FailingS3;
    let result = restore_workflow_dir(&fail, "sess-1", &tmp.path().join("c")).await;
    assert!(result.is_err());
    Ok(())
}

#[tokio::test]
async fn failing_s3_list_workflow_files_errors() -> anyhow::Result<()> {
    let fail = FailingS3;
    assert!(fail.list_workflow_files("any").await.is_err());
    Ok(())
}

#[tokio::test]
async fn subscriber_uploads_workflow_dir_on_shutdown() -> anyhow::Result<()> {
    // End-to-end: the subscriber reads the resolved session log path and
    // mirrors the sibling companion dir to S3 on the shutdown drain.
    let tmp = tempfile::tempdir()?;
    let session_dir = tmp.path().to_path_buf();

    // Session log + sibling companion dir with workflow journals.
    let session_log = session_dir.join("abc-123.jsonl");
    tokio::fs::write(&session_log, b"transcript\n").await?;
    let companion = session_dir.join("abc-123");
    tokio::fs::create_dir_all(&companion).await?;
    tokio::fs::write(companion.join("agent-1.jsonl"), b"wf-journal\n").await?;

    let mock = Arc::new(MockS3::new());
    let (transcript_tx, _) = broadcast::channel::<TranscriptEvent>(16);
    let (record_tx, _) = broadcast::channel::<RecordingEntry>(16);
    let (state_tx, _) = broadcast::channel::<crate::event::TransitionEvent>(16);
    let shutdown = CancellationToken::new();

    let handle = spawn_subscriber(
        Arc::clone(&mock),
        "sess-wf-e2e".into(),
        Some(session_dir),
        &transcript_tx,
        &record_tx,
        &state_tx,
        Arc::new(RwLock::new(Some(session_log))),
        Duration::from_secs(60),
        "claude".into(),
        vec![],
        None,
        None,
        None,
        None,
        shutdown.clone(),
    );

    tokio::time::sleep(Duration::from_millis(50)).await;
    shutdown.cancel();
    assert!(handle.await_drain(Duration::from_secs(5)).await);

    assert_eq!(mock.get("sess-wf-e2e", "workflow/agent-1.jsonl"), Some(b"wf-journal\n".to_vec()));
    Ok(())
}

#[tokio::test]
async fn workflow_dir_upload_restore_roundtrip() -> anyhow::Result<()> {
    // Simulate the full crash-resilience loop: spawn N writes the companion
    // dir to S3 on shutdown; spawn N+1 (PVC destroyed) restores it so the
    // Workflow tool can resume via resumeFromRunId.
    let tmp = tempfile::tempdir()?;
    let mock = MockS3::new();

    // --- spawn N: upload ---
    let log_path = tmp.path().join("uuid-N.jsonl");
    tokio::fs::write(&log_path, b"t\n").await?;
    let companion = tmp.path().join("uuid-N");
    tokio::fs::create_dir_all(companion.join("runs")).await?;
    tokio::fs::write(companion.join("agent-1.jsonl"), b"line1\nline2\n").await?;
    tokio::fs::write(companion.join("runs").join("wf_run.json"), b"{\"cached\":true}").await?;
    upload_workflow_dir(&mock, "ka-bead/spawn-N", Some(&log_path)).await;

    // --- spawn N+1: restore into a fresh (empty) companion dir ---
    let restore_dir = tmp.path().join("fresh-pvc").join("uuid-Nplus1");
    let count = restore_workflow_dir(&mock, "ka-bead/spawn-N", &restore_dir).await?;
    assert_eq!(count, 2);
    assert_eq!(
        tokio::fs::read_to_string(restore_dir.join("agent-1.jsonl")).await?,
        "line1\nline2\n"
    );
    assert_eq!(
        tokio::fs::read_to_string(restore_dir.join("runs").join("wf_run.json")).await?,
        "{\"cached\":true}"
    );
    Ok(())
}

// ===========================================================================
// Codex rollout round trip (gd-JeZ78WY3we)
// ===========================================================================

const CODEX_ROLLOUT: &str = include_str!("../../../tests/fixtures/codex/rollout-sample.jsonl");
const CODEX_THREAD: &str = "01a13cb8-970e-74a2-b81f-f40294555aa8";

#[test]
fn harness_name_falls_back_to_agent_type_for_old_metas() -> anyhow::Result<()> {
    let old: SessionMeta =
        serde_json::from_str(r#"{"session_id":"s","agent_type":"claude","started_at":1}"#)?;
    assert_eq!(old.harness, None);
    assert_eq!(old.harness_name(), "claude");
    let new: SessionMeta = serde_json::from_str(
        r#"{"session_id":"s","agent_type":"codex","started_at":1,"harness":"codex"}"#,
    )?;
    assert_eq!(new.harness_name(), "codex");
    Ok(())
}

/// A fake Codex writes a rollout under pod A's `$CODEX_HOME`; coop's subscriber
/// uploads it to the S3 stub; a fresh `$CODEX_HOME` (pod B) restores it and can
/// resume by thread id.
#[tokio::test]
async fn codex_rollout_round_trips_through_s3_into_a_fresh_codex_home() -> anyhow::Result<()> {
    use crate::driver::codex::rollout;

    // Pod A: the fake codex writes its rollout.
    let home_a = tempfile::tempdir()?;
    let live = home_a
        .path()
        .join("sessions/2026/10/14")
        .join(format!("rollout-2026-10-14T23-21-32-{CODEX_THREAD}.jsonl"));
    tokio::fs::create_dir_all(live.parent().unwrap()).await?;
    tokio::fs::write(&live, CODEX_ROLLOUT).await?;

    // coop uploads it (the path arrives through the rollout slot).
    let session_dir = tempfile::tempdir()?;
    let mock = Arc::new(MockS3::new());
    let (transcript_tx, _) = broadcast::channel::<TranscriptEvent>(16);
    let (record_tx, _) = broadcast::channel::<RecordingEntry>(16);
    let (state_tx, _) = broadcast::channel::<crate::event::TransitionEvent>(16);
    let shutdown = CancellationToken::new();
    let handle = spawn_subscriber(
        Arc::clone(&mock),
        "gd-bead/spawn-a".into(),
        Some(session_dir.path().to_path_buf()),
        &transcript_tx,
        &record_tx,
        &state_tx,
        Arc::new(RwLock::new(Some(live.clone()))),
        Duration::from_secs(60),
        "codex".into(),
        vec![],
        None,
        None,
        None,
        Some("gd-bead".into()),
        shutdown.clone(),
    );
    handle.set_exit(Some(0), "success".into());
    shutdown.cancel();
    tokio::time::sleep(Duration::from_millis(200)).await;

    assert_eq!(
        mock.get("gd-bead/spawn-a", "session.jsonl").as_deref(),
        Some(CODEX_ROLLOUT.as_bytes())
    );
    assert!(
        mock.keys().iter().all(|k| !k.contains("/workflow/")),
        "codex has no workflow companion dir: {:?}",
        mock.keys()
    );

    // Pod B: pick the spawn, check the harness, restore into a fresh home.
    let key = pick_restore_key(&*mock, Some("gd-bead"), None).await.unwrap();
    assert_eq!(key, "gd-bead/spawn-a");
    let meta = mock.download_meta(&key).await?.unwrap();
    assert_eq!(meta.harness.as_deref(), Some("codex"));

    let restore_dir = tempfile::tempdir()?;
    let dest = restore_dir.path().join("session.jsonl");
    restore_session_log(&*mock, &key, &dest).await?;

    let home_b = tempfile::tempdir()?;
    let thread = rollout::place_restored_rollout(&dest, home_b.path())?;
    assert_eq!(thread.as_deref(), Some(CODEX_THREAD));
    let restored = rollout::find_rollout(home_b.path(), CODEX_THREAD).unwrap();
    assert_eq!(restored.strip_prefix(home_b.path())?, live.strip_prefix(home_a.path())?);
    assert_eq!(std::fs::read_to_string(restored)?, CODEX_ROLLOUT);
    Ok(())
}
