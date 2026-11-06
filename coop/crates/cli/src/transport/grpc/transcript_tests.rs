// SPDX-License-Identifier: BUSL-1.1
// Copyright (c) 2026 Matt Baker <fimbaz@gmail.com>

use super::*;
use crate::test_support::{StoreBuilder, StoreCtx};
use crate::transcript::TranscriptState;

#[tokio::test]
async fn list_transcripts_empty() -> anyhow::Result<()> {
    let tmp = tempfile::tempdir()?;
    let ts_dir = tmp.path().join("transcripts");
    let log = tmp.path().join("session.jsonl");
    std::fs::write(&log, "")?;
    let ts = std::sync::Arc::new(TranscriptState::new(ts_dir, Some(log))?);

    let StoreCtx { store: state, .. } = StoreBuilder::new().child_pid(1234).transcript(ts).build();
    let svc = CoopGrpc::new(state);

    let req = tonic::Request::new(proto::ListTranscriptsRequest {});
    let resp = proto::coop_server::Coop::list_transcripts(&svc, req).await?;
    assert!(resp.into_inner().transcripts.is_empty());
    Ok(())
}

#[tokio::test]
async fn list_transcripts_after_save() -> anyhow::Result<()> {
    let tmp = tempfile::tempdir()?;
    let ts_dir = tmp.path().join("transcripts");
    let log = tmp.path().join("session.jsonl");
    std::fs::write(&log, "{\"msg\":\"one\"}\n{\"msg\":\"two\"}\n")?;
    let ts = std::sync::Arc::new(TranscriptState::new(ts_dir, Some(log))?);

    ts.save_snapshot().await?;

    let StoreCtx { store: state, .. } = StoreBuilder::new().child_pid(1234).transcript(ts).build();
    let svc = CoopGrpc::new(state);

    let req = tonic::Request::new(proto::ListTranscriptsRequest {});
    let resp = proto::coop_server::Coop::list_transcripts(&svc, req).await?;
    let list = resp.into_inner().transcripts;
    assert_eq!(list.len(), 1);
    assert_eq!(list[0].number, 1);
    assert_eq!(list[0].line_count, 2);
    Ok(())
}

#[tokio::test]
async fn get_transcript_not_found() -> anyhow::Result<()> {
    let tmp = tempfile::tempdir()?;
    let ts_dir = tmp.path().join("transcripts");
    let log = tmp.path().join("session.jsonl");
    std::fs::write(&log, "")?;
    let ts = std::sync::Arc::new(TranscriptState::new(ts_dir, Some(log))?);

    let StoreCtx { store: state, .. } = StoreBuilder::new().child_pid(1234).transcript(ts).build();
    let svc = CoopGrpc::new(state);

    let req = tonic::Request::new(proto::GetTranscriptRequest { number: 99 });
    let result = proto::coop_server::Coop::get_transcript(&svc, req).await;
    assert!(result.is_err());
    let status = result.unwrap_err();
    assert_eq!(status.code(), tonic::Code::NotFound);
    Ok(())
}

#[tokio::test]
async fn catchup_transcripts_returns_data() -> anyhow::Result<()> {
    let tmp = tempfile::tempdir()?;
    let ts_dir = tmp.path().join("transcripts");
    let log = tmp.path().join("session.jsonl");
    std::fs::write(&log, "{\"turn\":1}\n")?;
    let ts = std::sync::Arc::new(TranscriptState::new(ts_dir, Some(log.clone()))?);

    ts.save_snapshot().await?;

    // Add more lines to the session log.
    std::fs::write(&log, "{\"turn\":1}\n{\"turn\":2}\n")?;

    let StoreCtx { store: state, .. } = StoreBuilder::new().child_pid(1234).transcript(ts).build();
    let svc = CoopGrpc::new(state);

    let req = tonic::Request::new(proto::CatchupTranscriptsRequest {
        since_transcript: 0,
        since_line: 0,
    });
    let resp = proto::coop_server::Coop::catchup_transcripts(&svc, req).await?;
    let inner = resp.into_inner();

    assert_eq!(inner.transcripts.len(), 1, "should include transcript 1");
    assert_eq!(inner.transcripts[0].number, 1);
    assert_eq!(inner.live_lines.len(), 2, "should include live lines");
    assert_eq!(inner.current_line, 2);
    Ok(())
}
