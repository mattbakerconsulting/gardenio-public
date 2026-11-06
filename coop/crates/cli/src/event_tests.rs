// SPDX-License-Identifier: BUSL-1.1
// Copyright (c) 2026 Matt Baker <fimbaz@gmail.com>

//! Regression guards for the `message_tx` broadcast capacity (ka-0000006823).
//!
//! These pin the concrete failure mode the capacity bump removes: a catch-up
//! burst of raw agent messages, sent into the ring faster than the NATS
//! forwarder is scheduled, must not Lagged-drop `assistant_text` (an unpublished
//! plan turn). They drive the real `tokio::sync::broadcast` type with `try_recv`
//! so the send/receive ordering is fully deterministic — no sleeps, no runtime
//! scheduling to race on.

use super::*;
use tokio::sync::broadcast::{self, error::TryRecvError};

/// Build an assistant-text `RawMessageEvent` tagged with its ordinal so a drained
/// stream can be checked for gaps (dropped turns).
fn msg(i: usize) -> RawMessageEvent {
    RawMessageEvent {
        json: serde_json::json!({
            "type": "assistant",
            "message": { "content": [{ "type": "text", "text": format!("turn {i}") }] },
            "idx": i,
        }),
        source: "log".to_owned(),
    }
}

fn idx_of(ev: &RawMessageEvent) -> Option<usize> {
    ev.json.get("idx").and_then(|v| v.as_u64()).map(|v| v as usize)
}

/// Drain everything currently buffered for `rx`, returning the received ordinals
/// in order and the total count the channel reported as Lagged (dropped). Uses
/// `try_recv`, which mirrors `recv().await`'s Lagged semantics without needing a
/// runtime, keeping the test deterministic.
fn drain(rx: &mut broadcast::Receiver<RawMessageEvent>) -> (Vec<usize>, u64) {
    let mut got = Vec::new();
    let mut lagged = 0u64;
    loop {
        match rx.try_recv() {
            Ok(ev) => {
                if let Some(i) = idx_of(&ev) {
                    got.push(i);
                }
            }
            Err(TryRecvError::Lagged(n)) => lagged += n,
            Err(TryRecvError::Empty | TryRecvError::Closed) => break,
        }
    }
    (got, lagged)
}

/// The historical capacity (64) Lagged-drops a catch-up burst: a batch larger
/// than the ring overwrites the oldest slots before the consumer drains, so
/// `assistant_text` is lost — the pre-JetStream drop this fix removes.
#[test]
fn undersized_ring_lagged_drops_assistant_text_burst() {
    let (tx, mut rx) = broadcast::channel::<RawMessageEvent>(64);
    let burst = 200;
    for i in 0..burst {
        assert!(tx.send(msg(i)).is_ok());
    }
    let (got, lagged) = drain(&mut rx);
    // Only the ring's worth survives; the rest are gone forever.
    assert_eq!(got.len(), 64, "a capacity-64 ring retains exactly its capacity");
    assert_eq!(lagged, (burst - 64) as u64, "the overflow is Lagged-dropped");
}

/// At `MESSAGE_TX_CAPACITY` the same catch-up burst — far larger than the old 64
/// cap — drains completely and contiguously: every plan turn is preserved, in
/// order, with zero Lagged drops.
#[test]
fn message_tx_capacity_absorbs_catchup_burst() {
    assert!(MESSAGE_TX_CAPACITY > 64, "the fix must widen the ring past the old cap");
    let (tx, mut rx) = broadcast::channel::<RawMessageEvent>(MESSAGE_TX_CAPACITY);
    // A burst well beyond the old 64 cap that would have Lagged-dropped ~936
    // turns, yet still within the new ring.
    let burst = 1000;
    assert!(burst > 64 && burst <= MESSAGE_TX_CAPACITY);
    for i in 0..burst {
        assert!(tx.send(msg(i)).is_ok());
    }
    let (got, lagged) = drain(&mut rx);
    assert_eq!(lagged, 0, "no assistant_text may be Lagged-dropped at the new capacity");
    assert_eq!(got.len(), burst, "every message in the burst is delivered");
    assert!(got.iter().copied().eq(0..burst), "delivered contiguously, in order — no gaps");
}

/// The bead framed the drop as "a slow gRPC/WS/S3 consumer causes the forwarder
/// to Lagged-drop". tokio broadcast receivers have INDEPENDENT cursors, so that
/// cannot happen: a stalled sibling only lags itself. Driven deterministically
/// (interleaved send/recv, no sleeps) — the forwarder stays caught up and loses
/// nothing while a secondary consumer never reads and overflows its own cursor.
#[test]
fn slow_secondary_consumer_does_not_lag_the_forwarder() -> anyhow::Result<()> {
    let (tx, mut forwarder_rx) = broadcast::channel::<RawMessageEvent>(64);
    // A secondary consumer (stand-in for a stalled WS/gRPC live viewer) that
    // never reads.
    let mut slow_rx = tx.subscribe();

    // Push far more than the ring through. The forwarder consumes one-for-one
    // and therefore never falls behind, regardless of the untouched sibling.
    let total = 64 * 4;
    for i in 0..total {
        assert!(tx.send(msg(i)).is_ok());
        let ev = match forwarder_rx.try_recv() {
            Ok(ev) => ev,
            Err(e) => anyhow::bail!("forwarder unexpectedly missed idx {i}: {e:?}"),
        };
        assert_eq!(idx_of(&ev), Some(i), "forwarder receives every message in order");
    }
    // The forwarder is fully drained with zero drops.
    let (tail, forwarder_lagged) = drain(&mut forwarder_rx);
    assert!(tail.is_empty(), "forwarder is caught up");
    assert_eq!(forwarder_lagged, 0, "the slow sibling never made the forwarder Lag");

    // The untouched secondary consumer, by contrast, overflowed its own cursor —
    // proving the ring really did wrap while the forwarder stayed whole.
    let (slow_got, slow_lagged) = drain(&mut slow_rx);
    assert!(slow_lagged > 0, "the untouched secondary consumer lags on its own cursor");
    assert!(slow_got.len() <= 64, "and recovers at most the ring's worth");
    Ok(())
}
