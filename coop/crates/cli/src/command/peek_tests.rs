// SPDX-License-Identifier: BUSL-1.1
// Copyright (c) 2026 Matt Baker <fimbaz@gmail.com>

use std::collections::HashMap;
use std::path::Path;

use crate::s3::{S3Storage, SessionMeta};

// ---------------------------------------------------------------------------
// MockS3 — in-memory S3Storage for testing peek archived functions
// ---------------------------------------------------------------------------

struct MockS3 {
    objects: parking_lot::Mutex<HashMap<String, Vec<u8>>>,
}

impl MockS3 {
    fn new() -> Self {
        Self { objects: parking_lot::Mutex::new(HashMap::new()) }
    }

    fn put(&self, session_id: &str, s3_path: &str, data: Vec<u8>) {
        let key = format!("{session_id}/{s3_path}");
        self.objects.lock().insert(key, data);
    }

    fn put_meta(&self, meta: &SessionMeta) {
        let json = serde_json::to_vec_pretty(meta).expect("serialize meta");
        self.put(&meta.session_id, "meta.json", json);
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
        let key = format!("{session_id}/{s3_path}");
        self.objects.lock().insert(key, data);
        Ok(())
    }

    async fn upload_bytes(
        &self,
        session_id: &str,
        s3_path: &str,
        data: Vec<u8>,
    ) -> anyhow::Result<()> {
        let key = format!("{session_id}/{s3_path}");
        self.objects.lock().insert(key, data);
        Ok(())
    }

    async fn download_file(
        &self,
        session_id: &str,
        s3_path: &str,
        local_path: &Path,
    ) -> anyhow::Result<()> {
        let key = format!("{session_id}/{s3_path}");
        let data = {
            let objects = self.objects.lock();
            objects.get(&key).cloned().ok_or_else(|| anyhow::anyhow!("not found: {key}"))?
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
        let key = format!("{session_id}/meta.json");
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
        let key = format!("{session_id}/{s3_path}");
        self.objects.lock().contains_key(&key)
    }

    async fn list_sessions(&self) -> anyhow::Result<Vec<String>> {
        let objects = self.objects.lock();
        let mut ids = std::collections::BTreeSet::new();
        for key in objects.keys() {
            if key.ends_with("/meta.json") {
                if let Some(sid) = key.strip_suffix("/meta.json") {
                    ids.insert(sid.to_owned());
                }
            }
        }
        Ok(ids.into_iter().collect())
    }

    async fn list_spawns(&self, bead_id: &str) -> anyhow::Result<Vec<String>> {
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
        let key = format!("{session_id}/{s3_path}");
        let objects = self.objects.lock();
        objects.get(&key).cloned().ok_or_else(|| anyhow::anyhow!("not found: {key}"))
    }
}

fn test_meta(session_id: &str) -> SessionMeta {
    SessionMeta {
        session_id: session_id.to_owned(),
        agent_type: "claude".to_owned(),
        started_at: 1711000000,
        ended_at: Some(1711003600),
        exit_code: Some(0),
        labels: vec!["project:gardenio".to_owned()],
        agent_name: Some("test-agent".to_owned()),
        project: Some("gardenio".to_owned()),
        role: None,
        bead_id: None,
        exit_status: Some("success".to_owned()),
        transcript_count: None,
        harness: None,
    }
}

#[tokio::test]
async fn archived_list_empty() {
    let mock = MockS3::new();
    let code = super::archived_list(&mock).await;
    assert_eq!(code, 0);
}

#[tokio::test]
async fn archived_list_shows_sessions() {
    let mock = MockS3::new();
    mock.put_meta(&test_meta("sess-1"));
    mock.put_meta(&test_meta("sess-2"));

    let code = super::archived_list(&mock).await;
    assert_eq!(code, 0);
}

#[tokio::test]
async fn archived_meta_shows_session() {
    let mock = MockS3::new();
    mock.put_meta(&test_meta("sess-1"));
    mock.put("sess-1", "session.jsonl", b"log data\n".to_vec());
    mock.put("sess-1", "transcripts/1.jsonl", b"transcript\n".to_vec());
    mock.put("sess-1", "recording/1-10.jsonl", b"recording\n".to_vec());

    let code = super::archived_meta(&mock, "sess-1").await;
    assert_eq!(code, 0);
}

#[tokio::test]
async fn archived_meta_missing_session() {
    let mock = MockS3::new();
    let code = super::archived_meta(&mock, "nonexistent").await;
    assert_eq!(code, 1);
}

#[tokio::test]
async fn archived_transcripts_lists_and_downloads() {
    let mock = MockS3::new();
    mock.put("sess-1", "transcripts/1.jsonl", b"line1\nline2\n".to_vec());
    mock.put("sess-1", "transcripts/2.jsonl", b"line3\n".to_vec());

    let code = super::archived_transcripts(&mock, "sess-1").await;
    assert_eq!(code, 0);
}

#[tokio::test]
async fn archived_transcripts_empty() {
    let mock = MockS3::new();
    let code = super::archived_transcripts(&mock, "no-transcripts").await;
    assert_eq!(code, 0);
}

#[tokio::test]
async fn archived_recording_lists_and_downloads() {
    let mock = MockS3::new();
    mock.put("sess-1", "recording/1-50.jsonl", b"{\"seq\":1}\n{\"seq\":2}\n".to_vec());
    mock.put("sess-1", "recording/51-100.jsonl", b"{\"seq\":51}\n".to_vec());

    let code = super::archived_recording(&mock, "sess-1").await;
    assert_eq!(code, 0);
}

#[tokio::test]
async fn archived_recording_empty() {
    let mock = MockS3::new();
    let code = super::archived_recording(&mock, "no-recordings").await;
    assert_eq!(code, 0);
}
