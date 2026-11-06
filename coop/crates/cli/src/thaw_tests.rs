// SPDX-License-Identifier: BUSL-1.1
// Copyright (c) 2026 Matt Baker <fimbaz@gmail.com>

use std::sync::atomic::{AtomicI64, Ordering};
use std::sync::Arc;
use std::time::Duration;

use tempfile::TempDir;
use tokio_util::sync::CancellationToken;

use super::*;

/// Write a marker the way the Go controller does (atomic-ish; tests only need a
/// plain write since no concurrent reader races mid-write here).
fn write_marker(dir: &std::path::Path, m: &Marker) -> anyhow::Result<()> {
    std::fs::create_dir_all(dir)?;
    let bytes = serde_json::to_vec_pretty(m)?;
    std::fs::write(dir.join(MARKER_NAME), bytes)?;
    Ok(())
}

#[test]
fn marker_deadline_default_and_override() {
    assert_eq!(Marker::default().deadline(), DEFAULT_DEADLINE);
    let m = Marker { deadline_ms: 2500, ..Default::default() };
    assert_eq!(m.deadline(), Duration::from_millis(2500));
}

#[test]
fn read_absent_marker_is_none() -> anyhow::Result<()> {
    let dir = TempDir::new()?;
    assert!(read_marker(dir.path())?.is_none());
    assert!(read_ack(dir.path(), CONSUMER)?.is_none());
    Ok(())
}

#[test]
fn ack_round_trips_and_is_atomic() -> anyhow::Result<()> {
    let dir = TempDir::new()?;
    let mut ack = Ack::new(3, true, None);
    ack.stages.insert("reconnect".into(), "ok".into());
    write_ack(dir.path(), &ack)?;

    let got = read_ack(dir.path(), CONSUMER)?.ok_or_else(|| anyhow::anyhow!("no ack"))?;
    assert_eq!(got.epoch, 3);
    assert!(got.ok);
    assert_eq!(got.stages.get("reconnect").map(String::as_str), Some("ok"));

    // No leftover temp files.
    for entry in std::fs::read_dir(dir.path())? {
        let name = entry?.file_name();
        let name = name.to_string_lossy();
        assert!(!name.contains(".tmp."), "leftover temp file: {name}");
    }
    Ok(())
}

#[test]
fn write_armed_creates_sentinel_idempotently() -> anyhow::Result<()> {
    let dir = TempDir::new()?;
    let sentinel = dir.path().join(format!("thaw-epoch.{CONSUMER}.armed"));

    assert!(!sentinel.exists(), "sentinel should be absent before write");
    write_armed(dir.path(), CONSUMER)?;
    assert!(sentinel.exists(), "sentinel should exist after write");

    // Filename matches the Go thawepoch.ArmedName contract for the same consumer.
    assert_eq!(
        sentinel.file_name().map(|s| s.to_string_lossy().into_owned()),
        Some("thaw-epoch.coop.armed".to_string())
    );

    // Idempotent re-arm leaves exactly the one sentinel, no temp residue.
    write_armed(dir.path(), CONSUMER)?;
    let mut names: Vec<String> = std::fs::read_dir(dir.path())?
        .filter_map(|e| e.ok())
        .map(|e| e.file_name().to_string_lossy().into_owned())
        .collect();
    names.sort();
    assert_eq!(
        names,
        vec!["thaw-epoch.coop.armed".to_string()],
        "exactly one sentinel, no temp files"
    );
    Ok(())
}

/// A test handler that counts invocations and can be told to fail.
struct CountingHandler {
    calls: Arc<AtomicI64>,
    fail_until_epoch: i64, // fail while marker.epoch <= this
}

impl RehydrateHandler for CountingHandler {
    async fn rehydrate(&self, marker: &Marker) -> Outcome {
        self.calls.fetch_add(1, Ordering::SeqCst);
        if marker.epoch <= self.fail_until_epoch {
            Outcome::failed("forced failure")
        } else {
            Outcome::ok()
        }
    }
}

async fn wait_for<F: Fn() -> bool>(cond: F) -> anyhow::Result<()> {
    for _ in 0..600 {
        if cond() {
            return Ok(());
        }
        tokio::time::sleep(Duration::from_millis(5)).await;
    }
    anyhow::bail!("condition not met before timeout")
}

#[tokio::test]
async fn watcher_runs_handler_once_per_epoch() -> anyhow::Result<()> {
    let dir = TempDir::new()?;
    let calls = Arc::new(AtomicI64::new(0));
    let handler = CountingHandler { calls: Arc::clone(&calls), fail_until_epoch: 0 };
    let watcher = ThawWatcher::new(dir.path().to_path_buf(), handler)
        .with_poll_interval(Duration::from_millis(5));

    write_marker(dir.path(), &Marker { epoch: 1, pod_uid: "b1".into(), ..Default::default() })?;

    let shutdown = CancellationToken::new();
    let sd = shutdown.clone();
    let dirpath = dir.path().to_path_buf();
    let handle = tokio::spawn(async move { watcher.run(sd).await });

    wait_for(|| read_ack(&dirpath, CONSUMER).ok().flatten().is_some_and(|a| a.epoch == 1 && a.ok))
        .await?;

    // Give a few extra poll ticks: the epoch must not be re-handled.
    tokio::time::sleep(Duration::from_millis(40)).await;
    shutdown.cancel();
    handle.await?;

    assert_eq!(calls.load(Ordering::SeqCst), 1, "handler should run exactly once for epoch 1");
    Ok(())
}

#[tokio::test]
async fn watcher_reruns_on_epoch_increment() -> anyhow::Result<()> {
    let dir = TempDir::new()?;
    let calls = Arc::new(AtomicI64::new(0));
    let handler = CountingHandler { calls: Arc::clone(&calls), fail_until_epoch: 0 };
    let watcher = ThawWatcher::new(dir.path().to_path_buf(), handler)
        .with_poll_interval(Duration::from_millis(5));

    let shutdown = CancellationToken::new();
    let sd = shutdown.clone();
    let dirpath = dir.path().to_path_buf();
    let handle = tokio::spawn(async move { watcher.run(sd).await });

    for epoch in 1..=3 {
        write_marker(&dirpath, &Marker { epoch, pod_uid: "b".into(), ..Default::default() })?;
        let want = epoch;
        wait_for(|| {
            read_ack(&dirpath, CONSUMER).ok().flatten().is_some_and(|a| a.epoch == want && a.ok)
        })
        .await?;
    }
    shutdown.cancel();
    handle.await?;
    assert_eq!(calls.load(Ordering::SeqCst), 3);
    Ok(())
}

#[tokio::test]
async fn watcher_fail_closed_does_not_latch_epoch() -> anyhow::Result<()> {
    let dir = TempDir::new()?;
    let calls = Arc::new(AtomicI64::new(0));
    // Fail while epoch <= 0 means: never fail. We instead simulate a transient
    // failure by writing an ack=false manually is awkward; use a handler that
    // fails on the first call only.
    struct FlipHandler {
        calls: Arc<AtomicI64>,
    }
    impl RehydrateHandler for FlipHandler {
        async fn rehydrate(&self, _m: &Marker) -> Outcome {
            let n = self.calls.fetch_add(1, Ordering::SeqCst);
            if n == 0 {
                Outcome::failed("transient")
            } else {
                Outcome::ok()
            }
        }
    }
    let watcher =
        ThawWatcher::new(dir.path().to_path_buf(), FlipHandler { calls: Arc::clone(&calls) })
            .with_poll_interval(Duration::from_millis(5));

    write_marker(dir.path(), &Marker { epoch: 1, pod_uid: "b".into(), ..Default::default() })?;

    let shutdown = CancellationToken::new();
    let sd = shutdown.clone();
    let dirpath = dir.path().to_path_buf();
    let handle = tokio::spawn(async move { watcher.run(sd).await });

    // First ack is fail-closed; epoch not latched, so the watcher re-runs and
    // the second ack succeeds.
    wait_for(|| read_ack(&dirpath, CONSUMER).ok().flatten().is_some_and(|a| a.epoch == 1 && a.ok))
        .await?;
    shutdown.cancel();
    handle.await?;
    assert!(calls.load(Ordering::SeqCst) >= 2, "fail-closed should re-run the same epoch");
    Ok(())
}

#[tokio::test]
async fn watcher_recovers_acked_epoch_on_restart() -> anyhow::Result<()> {
    let dir = TempDir::new()?;
    write_marker(dir.path(), &Marker { epoch: 2, pod_uid: "b".into(), ..Default::default() })?;
    write_ack(dir.path(), &Ack::new(2, true, None))?;

    let calls = Arc::new(AtomicI64::new(0));
    let handler = CountingHandler { calls: Arc::clone(&calls), fail_until_epoch: 0 };
    let watcher = ThawWatcher::new(dir.path().to_path_buf(), handler)
        .with_poll_interval(Duration::from_millis(5));

    let shutdown = CancellationToken::new();
    let sd = shutdown.clone();
    let handle = tokio::spawn(async move { watcher.run(sd).await });
    tokio::time::sleep(Duration::from_millis(80)).await;
    shutdown.cancel();
    handle.await?;

    assert_eq!(
        calls.load(Ordering::SeqCst),
        0,
        "watcher must not re-run an already-acked epoch on restart"
    );
    Ok(())
}

#[tokio::test]
async fn ack_only_handler_acks_ok() -> anyhow::Result<()> {
    let dir = TempDir::new()?;
    let watcher = ThawWatcher::new(dir.path().to_path_buf(), AckOnlyHandler)
        .with_poll_interval(Duration::from_millis(5));
    write_marker(dir.path(), &Marker { epoch: 1, pod_uid: "b".into(), ..Default::default() })?;

    let shutdown = CancellationToken::new();
    let sd = shutdown.clone();
    let dirpath = dir.path().to_path_buf();
    let handle = tokio::spawn(async move { watcher.run(sd).await });
    wait_for(|| read_ack(&dirpath, CONSUMER).ok().flatten().is_some_and(|a| a.ok)).await?;
    shutdown.cancel();
    handle.await?;
    Ok(())
}

/// Cross-language contract: a marker written by the Go controller (RFC3339 ts,
/// extra fields) must deserialize cleanly here, and an ack we write must carry
/// the fields the Go gate reads (epoch, consumer, ok).
#[test]
fn parses_go_written_marker_and_emits_go_readable_ack() -> anyhow::Result<()> {
    let dir = TempDir::new()?;
    // Shape a Go-marshalled marker by hand: time.Time -> RFC3339 string,
    // and a field coop does not model is ignored.
    let go_marker = r#"{
        "epoch": 7,
        "pod_uid": "c38e2800-uid",
        "node": "ip-10-0-1-9",
        "ts": "2026-06-04T08:30:00.123456789Z",
        "frozen_at": "2026-06-04T08:29:50Z",
        "deadline_ms": 8000,
        "future_field": "ignored"
    }"#;
    std::fs::write(dir.path().join(MARKER_NAME), go_marker)?;

    let m = read_marker(dir.path())?.ok_or_else(|| anyhow::anyhow!("no marker"))?;
    assert_eq!(m.epoch, 7);
    assert_eq!(m.pod_uid, "c38e2800-uid");
    assert_eq!(m.deadline(), Duration::from_millis(8000));

    // The ack we write carries the gate's required keys.
    write_ack(dir.path(), &Ack::new(7, true, None))?;
    let raw = std::fs::read_to_string(dir.path().join("thaw-epoch.coop.done"))?;
    let v: serde_json::Value = serde_json::from_str(&raw)?;
    assert_eq!(v["epoch"], 7);
    assert_eq!(v["consumer"], "coop");
    assert_eq!(v["ok"], true);
    Ok(())
}

// ===========================================================================
// Sequence combinator (cred re-mint → NATS reconnect composition, ka-0000004088)
// ===========================================================================

/// A handler that records its invocation order into a shared log and returns a
/// preset outcome carrying one named stage. Lets the Sequence tests assert both
/// ordering and stage-map merging.
struct RecordingHandler {
    label: &'static str,
    stage: &'static str,
    ok: bool,
    order: Arc<std::sync::Mutex<Vec<&'static str>>>,
}

impl RehydrateHandler for RecordingHandler {
    async fn rehydrate(&self, _m: &Marker) -> Outcome {
        if let Ok(mut g) = self.order.lock() {
            g.push(self.label);
        }
        let mut stages = std::collections::BTreeMap::new();
        stages.insert(self.stage.to_string(), if self.ok { "ok" } else { "failed" }.to_string());
        Outcome {
            ok: self.ok,
            error: if self.ok { None } else { Some(format!("{} failed", self.label)) },
            stages,
        }
    }
}

#[tokio::test]
async fn sequence_runs_first_then_second_and_merges_stages() {
    let order = Arc::new(std::sync::Mutex::new(Vec::new()));
    let seq = Sequence::new(
        RecordingHandler { label: "creds", stage: "aws_sts", ok: true, order: Arc::clone(&order) },
        RecordingHandler {
            label: "nats",
            stage: "nats_publisher",
            ok: true,
            order: Arc::clone(&order),
        },
        &["nats_publisher", "nats_relay"],
    );
    let out = seq.rehydrate(&Marker { epoch: 1, ..Default::default() }).await;

    assert!(out.ok, "both halves ok → merged ok");
    // Creds ran strictly before NATS (design §5 ordering).
    assert_eq!(*order.lock().expect("order lock"), vec!["creds", "nats"]);
    // Both halves' per-stage detail survives in the merged ack.
    assert_eq!(out.stages.get("aws_sts").map(String::as_str), Some("ok"));
    assert_eq!(out.stages.get("nats_publisher").map(String::as_str), Some("ok"));
}

#[tokio::test]
async fn sequence_short_circuits_when_creds_fail_closed() {
    let order = Arc::new(std::sync::Mutex::new(Vec::new()));
    let seq = Sequence::new(
        RecordingHandler {
            label: "creds",
            stage: "sa_token",
            ok: false,
            order: Arc::clone(&order),
        },
        RecordingHandler {
            label: "nats",
            stage: "nats_publisher",
            ok: true,
            order: Arc::clone(&order),
        },
        &["nats_publisher", "nats_relay"],
    );
    let out = seq.rehydrate(&Marker { epoch: 1, ..Default::default() }).await;

    assert!(!out.ok, "creds fail-closed → merged fail-closed");
    // The NATS handler MUST NOT run — we never reconnect under a poisoned token.
    assert_eq!(*order.lock().expect("order lock"), vec!["creds"]);
    // The failed creds error is preserved.
    assert_eq!(out.error.as_deref(), Some("creds failed"));
    // The creds stage detail is kept, and the skipped connection stages are
    // recorded so the ack explains why reconnect did not run.
    assert_eq!(out.stages.get("sa_token").map(String::as_str), Some("failed"));
    assert_eq!(out.stages.get("nats_publisher").map(String::as_str), Some("skipped"));
    assert_eq!(out.stages.get("nats_relay").map(String::as_str), Some("skipped"));
}

#[tokio::test]
async fn sequence_fails_closed_when_reconnect_fails_after_creds_ok() {
    let order = Arc::new(std::sync::Mutex::new(Vec::new()));
    let seq = Sequence::new(
        RecordingHandler { label: "creds", stage: "aws_sts", ok: true, order: Arc::clone(&order) },
        RecordingHandler {
            label: "nats",
            stage: "nats_publisher",
            ok: false,
            order: Arc::clone(&order),
        },
        &["nats_publisher"],
    );
    let out = seq.rehydrate(&Marker { epoch: 1, ..Default::default() }).await;

    assert!(!out.ok, "creds ok but reconnect failed → merged fail-closed");
    assert_eq!(*order.lock().expect("order lock"), vec!["creds", "nats"]);
    assert_eq!(out.error.as_deref(), Some("nats failed"));
    // Creds stage stays "ok"; the connection stage carries its real failure
    // (NOT overwritten by the skip label, since reconnect actually ran).
    assert_eq!(out.stages.get("aws_sts").map(String::as_str), Some("ok"));
    assert_eq!(out.stages.get("nats_publisher").map(String::as_str), Some("failed"));
}
