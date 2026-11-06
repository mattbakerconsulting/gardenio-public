// SPDX-License-Identifier: BUSL-1.1
// Copyright (c) 2026 Matt Baker <fimbaz@gmail.com>

use crate::driver::AgentState;
use crate::event::{RawHookEvent, TransitionEvent};

use super::EventLog;

#[tokio::test]
async fn push_and_catchup_state() -> anyhow::Result<()> {
    let tmp = tempfile::tempdir()?;
    let log = EventLog::new(Some(tmp.path()));

    let events = vec![
        TransitionEvent {
            prev: AgentState::Starting,
            next: AgentState::Working,
            seq: 1,
            cause: "hook".into(),
            last_message: None,
        },
        TransitionEvent {
            prev: AgentState::Working,
            next: AgentState::Idle,
            seq: 2,
            cause: "hook".into(),
            last_message: Some("hello".into()),
        },
        TransitionEvent {
            prev: AgentState::Idle,
            next: AgentState::Working,
            seq: 3,
            cause: "nudge".into(),
            last_message: None,
        },
    ];

    for e in &events {
        log.push_transition(e).await;
    }

    // Catchup from seq 1 should return seq 2 and 3.
    let caught = log.catchup_state(1).await;
    assert_eq!(caught.len(), 2);
    assert_eq!(caught[0].seq, 2);
    assert_eq!(caught[1].seq, 3);
    assert_eq!(caught[0].last_message.as_deref(), Some("hello"));
    Ok(())
}

#[tokio::test]
async fn push_and_catchup_hooks() -> anyhow::Result<()> {
    let tmp = tempfile::tempdir()?;
    let log = EventLog::new(Some(tmp.path()));

    log.push_hook(&RawHookEvent { json: serde_json::json!({"event": "a"}) }).await;
    log.push_hook(&RawHookEvent { json: serde_json::json!({"event": "b"}) }).await;

    let caught = log.catchup_hooks(0).await;
    // hook_seq starts at 0 and we filter > since, so hook_seq=0 is not included
    // when since_hook_seq=0.
    assert_eq!(caught.len(), 1);
    assert_eq!(caught[0].hook_seq, 1);
    assert_eq!(caught[0].json["event"], "b");
    Ok(())
}

#[tokio::test]
async fn catchup_empty_when_no_events() -> anyhow::Result<()> {
    let tmp = tempfile::tempdir()?;
    let log = EventLog::new(Some(tmp.path()));

    assert!(log.catchup_state(0).await.is_empty());
    assert!(log.catchup_hooks(0).await.is_empty());
    Ok(())
}

#[tokio::test]
async fn catchup_with_no_session_dir() {
    let log = EventLog::new(None);

    // Push should be no-op, catchup returns empty.
    log.push_transition(&TransitionEvent {
        prev: AgentState::Starting,
        next: AgentState::Working,
        seq: 1,
        cause: String::new(),
        last_message: None,
    })
    .await;
    log.push_hook(&RawHookEvent { json: serde_json::json!({}) }).await;

    assert!(log.catchup_state(0).await.is_empty());
    assert!(log.catchup_hooks(0).await.is_empty());
}

fn transition(seq: u64) -> TransitionEvent {
    TransitionEvent {
        prev: AgentState::Working,
        next: AgentState::Idle,
        seq,
        cause: "hook".into(),
        last_message: None,
    }
}

/// Flush-barrier ordering: a transition whose `push_transition` future has
/// resolved is enqueued ahead of the `Flush` that `catchup_state` sends down
/// the same FIFO writer queue, so the very next catchup is guaranteed to
/// observe it. Interleaving pushes with catchups must therefore preserve a
/// strictly monotonic, gap-free view.
#[tokio::test]
async fn interleaved_push_and_catchup_preserve_ordering() -> anyhow::Result<()> {
    let tmp = tempfile::tempdir()?;
    let log = EventLog::new(Some(tmp.path()));

    // First batch, then a barrier read.
    for seq in 1..=3 {
        log.push_transition(&transition(seq)).await;
    }
    let caught = log.catchup_state(0).await;
    let seqs: Vec<u64> = caught.iter().map(|e| e.seq).collect();
    assert_eq!(seqs, vec![1, 2, 3], "barrier must expose all writes enqueued before it");

    // Second batch interleaved after the first read; the barrier read must now
    // observe the union, still in enqueue order.
    for seq in 4..=6 {
        log.push_transition(&transition(seq)).await;
    }
    let caught = log.catchup_state(0).await;
    let seqs: Vec<u64> = caught.iter().map(|e| e.seq).collect();
    assert_eq!(seqs, vec![1, 2, 3, 4, 5, 6], "second barrier must expose the union in order");

    // A cursored catchup after the barrier returns only the tail.
    let tail = log.catchup_state(3).await;
    let tail_seqs: Vec<u64> = tail.iter().map(|e| e.seq).collect();
    assert_eq!(tail_seqs, vec![4, 5, 6]);
    Ok(())
}

/// Queue-full backpressure: `push_transition` awaits the bounded writer channel
/// (capacity `EVENT_LOG_QUEUE_CAPACITY` = 256, drained in `EVENT_LOG_BATCH_SIZE`
/// = 64 batches). Enqueueing well past both bounds must apply backpressure, not
/// drop — every event survives and catchup returns the full, in-order set.
#[tokio::test]
async fn catchup_after_more_than_queue_capacity_returns_all_in_order() -> anyhow::Result<()> {
    let tmp = tempfile::tempdir()?;
    let log = EventLog::new(Some(tmp.path()));

    // 300 > queue capacity (256) and > batch size (64): forces the writer to
    // batch and backpressure while producing.
    const N: u64 = 300;
    for seq in 1..=N {
        log.push_transition(&transition(seq)).await;
    }

    let caught = log.catchup_state(0).await;
    assert_eq!(caught.len() as u64, N, "no events may be dropped under backpressure");
    let seqs: Vec<u64> = caught.iter().map(|e| e.seq).collect();
    let expected: Vec<u64> = (1..=N).collect();
    assert_eq!(seqs, expected, "events must survive in strict enqueue order");
    Ok(())
}

/// The same backpressure guarantee for the hook stream, whose `hook_seq` is
/// assigned server-side and increments monotonically across a large burst.
#[tokio::test]
async fn catchup_hooks_after_more_than_queue_capacity_returns_all_in_order() -> anyhow::Result<()> {
    let tmp = tempfile::tempdir()?;
    let log = EventLog::new(Some(tmp.path()));

    const N: usize = 300;
    for i in 0..N {
        log.push_hook(&RawHookEvent { json: serde_json::json!({ "i": i }) }).await;
    }

    // hook_seq starts at 0; catchup(0) filters strictly > 0, so it returns
    // hook_seq 1..=N-1 (the first, hook_seq=0, is excluded by the > filter).
    let caught = log.catchup_hooks(0).await;
    assert_eq!(caught.len(), N - 1, "no hook events may be dropped under backpressure");
    let seqs: Vec<u64> = caught.iter().map(|e| e.hook_seq).collect();
    let expected: Vec<u64> = (1..N as u64).collect();
    assert_eq!(seqs, expected, "hook events must survive in strict hook_seq order");
    Ok(())
}
