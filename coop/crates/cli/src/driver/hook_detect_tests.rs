// SPDX-License-Identifier: BUSL-1.1
// Copyright (c) 2026 Matt Baker <fimbaz@gmail.com>

use std::time::{Duration, Instant};

use super::ToolTimer;

/// Acceptance test for gd-Kl42VWWRd6: two overlapping same-name calls must
/// record their own durations (1s, 10s), not the FIFO-mismatched pairing
/// (10s, 1s) the old name-only queue would have produced.
#[test]
fn keys_overlapping_same_name_calls_by_tool_use_id_not_fifo_order() {
    let mut timer = ToolTimer::default();
    let base = Instant::now();

    // call_a starts first and runs for 10s; call_b starts 1s later, while
    // call_a is still in flight, and finishes after only 1s.
    timer.start("Bash", Some("call_a"), base);
    timer.start("Bash", Some("call_b"), base + Duration::from_secs(1));

    let b_elapsed = timer
        .finish("Bash", Some("call_b"), base + Duration::from_secs(2))
        .expect("call_b should have a matching start");
    assert!((b_elapsed - 1.0).abs() < 1e-9, "call_b ran for 1s, got {b_elapsed}");

    let a_elapsed = timer
        .finish("Bash", Some("call_a"), base + Duration::from_secs(10))
        .expect("call_a should have a matching start");
    assert!((a_elapsed - 10.0).abs() < 1e-9, "call_a ran for 10s, got {a_elapsed}");
}

#[test]
fn falls_back_to_fifo_when_tool_use_id_is_absent() {
    let mut timer = ToolTimer::default();
    let base = Instant::now();

    timer.start("Bash", None, base);
    timer.start("Bash", None, base + Duration::from_secs(1));

    let first = timer
        .finish("Bash", None, base + Duration::from_secs(2))
        .expect("oldest start should pop first");
    assert!((first - 2.0).abs() < 1e-9, "oldest start matched first, got {first}");
}

#[test]
fn finish_with_no_matching_start_returns_none() {
    let mut timer = ToolTimer::default();
    assert!(timer.finish("Bash", Some("missing"), Instant::now()).is_none());
    assert!(timer.finish("Bash", None, Instant::now()).is_none());
}
