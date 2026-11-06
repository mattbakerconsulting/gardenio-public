// SPDX-License-Identifier: BUSL-1.1
// Copyright (c) 2026 Matt Baker <fimbaz@gmail.com>

use super::*;

// ===== Unit tests ============================================================

#[test]
fn pty_before_replay_dropped() {
    let mut gate = ReplayGate::new();
    assert!(gate.on_pty(10, 0).is_none());
}

#[test]
fn first_replay_accepts_all() {
    let mut gate = ReplayGate::new();
    let action = gate.on_replay(100, 0, 100).expect("should accept");
    assert_eq!(action.skip, 0);
    assert!(action.is_first);
    assert_eq!(gate.offset(), Some(100));
}

#[test]
fn pty_after_replay_no_overlap() {
    let mut gate = ReplayGate::new();
    gate.on_replay(100, 0, 100);
    let skip = gate.on_pty(20, 100).expect("should accept");
    assert_eq!(skip, 0);
    assert_eq!(gate.offset(), Some(120));
}

#[test]
fn pty_fully_covered_by_replay() {
    let mut gate = ReplayGate::new();
    gate.on_replay(100, 0, 100);
    assert!(gate.on_pty(30, 50).is_none());
}

#[test]
fn pty_partial_overlap() {
    let mut gate = ReplayGate::new();
    gate.on_replay(100, 0, 100);
    let skip = gate.on_pty(20, 90).expect("should accept");
    assert_eq!(skip, 10);
    assert_eq!(gate.offset(), Some(110));
}

#[test]
fn pty_with_gap() {
    let mut gate = ReplayGate::new();
    gate.on_replay(100, 0, 100);
    let skip = gate.on_pty(10, 120).expect("should accept");
    assert_eq!(skip, 0);
    assert_eq!(gate.offset(), Some(130));
}

#[test]
fn second_replay_dedup() {
    let mut gate = ReplayGate::new();
    gate.on_replay(100, 0, 100);
    let action = gate.on_replay(150, 0, 150).expect("should accept");
    assert_eq!(action.skip, 100);
    assert!(!action.is_first);
    assert_eq!(gate.offset(), Some(150));
}

#[test]
fn second_replay_no_new_data() {
    let mut gate = ReplayGate::new();
    gate.on_replay(100, 0, 100);
    assert!(gate.on_replay(80, 0, 80).is_none());
}

#[test]
fn reset_returns_to_pending() {
    let mut gate = ReplayGate::new();
    gate.on_replay(100, 0, 100);
    gate.reset();
    assert!(gate.offset().is_none());
    assert!(gate.on_pty(10, 100).is_none());
    let action = gate.on_replay(50, 0, 50).expect("should accept");
    assert!(action.is_first);
}

#[test]
fn empty_replay_still_syncs() {
    let mut gate = ReplayGate::new();
    let action = gate.on_replay(0, 0, 0).expect("should accept");
    assert_eq!(action.skip, 0);
    assert!(action.is_first);
    assert_eq!(gate.offset(), Some(0));
}

#[test]
fn sequential_pty_stream() {
    let mut gate = ReplayGate::new();
    gate.on_replay(0, 0, 0);
    for i in 0..5u64 {
        let skip = gate.on_pty(10, i * 10).expect("should accept");
        assert_eq!(skip, 0);
        assert_eq!(gate.offset(), Some((i + 1) * 10));
    }
}

#[test]
fn replay_after_pty_stream() {
    let mut gate = ReplayGate::new();
    gate.on_replay(10, 0, 10);
    gate.on_pty(10, 10); // gate = 20
    gate.on_pty(10, 20); // gate = 30
    let action = gate.on_replay(50, 0, 50).expect("should accept");
    assert_eq!(action.skip, 30);
    assert!(!action.is_first);
    assert_eq!(gate.offset(), Some(50));
}

// ===== RenderHarness =========================================================

/// Test harness: feeds a sequence of events through a [`ReplayGate`] and
/// collects the bytes that would be written to the terminal.
struct RenderHarness {
    gate: ReplayGate,
    output: Vec<u8>,
    resets: usize,
}

impl RenderHarness {
    fn new() -> Self {
        Self { gate: ReplayGate::new(), output: Vec::new(), resets: 0 }
    }

    fn replay(&mut self, data: &[u8], offset: u64, next_offset: u64) {
        if let Some(action) = self.gate.on_replay(data.len(), offset, next_offset) {
            if action.is_first {
                self.output.clear();
                self.resets += 1;
            }
            self.output.extend_from_slice(&data[action.skip..]);
        }
    }

    fn pty(&mut self, data: &[u8], offset: u64) {
        if let Some(skip) = self.gate.on_pty(data.len(), offset) {
            self.output.extend_from_slice(&data[skip..]);
        }
    }

    fn reconnect(&mut self) {
        self.gate.reset();
        self.output.clear();
    }

    fn output_str(&self) -> &str {
        std::str::from_utf8(&self.output).unwrap_or("<invalid utf-8>")
    }
}

// ===== RenderHarness scenario tests ==========================================

#[test]
fn race_pty_before_replay() {
    let mut h = RenderHarness::new();
    h.pty(b"AB", 0);
    h.replay(b"ABCD", 0, 4);
    assert_eq!(h.output_str(), "ABCD");
    assert_eq!(h.resets, 1);
}

#[test]
fn race_pty_overlapping_replay() {
    let mut h = RenderHarness::new();
    h.pty(b"AB", 0);
    h.pty(b"CD", 2);
    h.replay(b"ABCDEF", 0, 6);
    assert_eq!(h.output_str(), "ABCDEF");
}

#[test]
fn clean_connect() {
    let mut h = RenderHarness::new();
    h.replay(b"HELLO", 0, 5);
    h.pty(b"!", 5);
    assert_eq!(h.output_str(), "HELLO!");
}

#[test]
fn lag_recovery() {
    let mut h = RenderHarness::new();
    h.replay(b"AB", 0, 2);
    h.pty(b"CD", 2);
    h.pty(b"EF", 4);
    h.replay(b"ABCDEF", 0, 6);
    assert_eq!(h.output_str(), "ABCDEF");
}

#[test]
fn reconnect_full_replay() {
    let mut h = RenderHarness::new();
    h.replay(b"OLD", 0, 3);
    h.reconnect();
    h.replay(b"NEW", 0, 3);
    assert_eq!(h.output_str(), "NEW");
    assert_eq!(h.resets, 2);
}

#[test]
fn resize_refresh() {
    let mut h = RenderHarness::new();
    h.replay(b"AB", 0, 2);
    h.pty(b"CD", 2);
    h.reconnect();
    h.replay(b"ABCD", 0, 4);
    assert_eq!(h.output_str(), "ABCD");
    assert_eq!(h.resets, 2);
}

#[test]
fn interleaved_stream() {
    let mut h = RenderHarness::new();
    h.replay(b"A", 0, 1);
    h.pty(b"B", 1);
    h.pty(b"C", 2);
    h.pty(b"BC", 1); // late duplicate — fully covered
    assert_eq!(h.output_str(), "ABC");
}

// ===== WsSimHarness ==========================================================
//
// Simulates the server's ring + broadcast path, including the offset race
// that occurs when another writer sneaks bytes between ring.write() and the
// offset read.

use crate::ring::RingBuffer;

struct WsSimHarness {
    ring: RingBuffer,
    gate: ReplayGate,
    output: Vec<u8>,
    resets: usize,
    /// Extra bytes injected between the ring write and offset read,
    /// simulating a concurrent writer.
    race_bytes: usize,
}

impl WsSimHarness {
    fn new(race_bytes: usize) -> Self {
        Self {
            ring: RingBuffer::new(65536),
            gate: ReplayGate::new(),
            output: Vec::new(),
            resets: 0,
            race_bytes,
        }
    }

    /// Simulate the server writing data, computing the offset the racy way
    /// (total_written - data.len() AFTER a possible concurrent write).
    fn server_write(&mut self, data: &[u8]) -> u64 {
        self.ring.write(data);
        // Simulate a concurrent writer injecting extra bytes.
        if self.race_bytes > 0 {
            let filler = vec![0u8; self.race_bytes];
            self.ring.write(&filler);
        }
        // Racy offset computation (the old server bug):
        let msg_offset = self.ring.total_written().saturating_sub(data.len() as u64);
        msg_offset
    }

    /// Feed data through the gate as a PTY message with the given offset.
    fn feed_pty(&mut self, data: &[u8], offset: u64) {
        if let Some(skip) = self.gate.on_pty(data.len(), offset) {
            self.output.extend_from_slice(&data[skip..]);
        }
    }

    /// Request a replay from offset 0 (simulates full reconnect replay).
    fn request_replay(&mut self, from_offset: u64) {
        if let Some((a, b)) = self.ring.read_from(from_offset) {
            let mut combined = Vec::with_capacity(a.len() + b.len());
            combined.extend_from_slice(a);
            combined.extend_from_slice(b);
            let total = self.ring.total_written();
            let replay_offset = total - combined.len() as u64;
            if let Some(action) = self.gate.on_replay(combined.len(), replay_offset, total) {
                if action.is_first {
                    self.output.clear();
                    self.resets += 1;
                }
                self.output.extend_from_slice(&combined[action.skip..]);
            }
        }
    }

    fn output_str(&self) -> &str {
        std::str::from_utf8(&self.output).unwrap_or("<invalid utf-8>")
    }
}

// ===== WsSimHarness tests ====================================================

#[test]
fn server_race_inflates_offset() {
    let mut h = WsSimHarness::new(5);
    let offset = h.server_write(b"AB");
    // With race_bytes=5: ring has 7 bytes, msg_offset = 7 - 2 = 5, not 0.
    assert_eq!(offset, 5, "racy offset should be inflated");
}

#[test]
fn server_race_causes_gap() {
    let mut h = WsSimHarness::new(3);
    // Sync the gate with an empty replay.
    h.request_replay(0);

    // Write "AB" — racy offset = (2+3) - 2 = 3 instead of 0.
    let offset = h.server_write(b"AB");
    assert_eq!(offset, 3, "racy: first write offset inflated");
    h.feed_pty(b"AB", offset);

    // Gate jumped to offset 5, but real data only covers [0,2).
    // Bytes [0,3) were never delivered to the client — that's the gap.
    assert_eq!(h.gate.offset(), Some(5));

    // Write "CD" — racy offset = (2+3+2+3) - 2 = 8 instead of 2.
    let offset2 = h.server_write(b"CD");
    assert_eq!(offset2, 8, "racy: second write offset inflated");
    h.feed_pty(b"CD", offset2);

    // Gate jumped to 10, but real data only covers [0,2) + [2,4) = [0,4).
    // Client thinks it has [3,5) + [8,10) — gaps at [5,8).
    assert_eq!(h.gate.offset(), Some(10));
}

#[test]
fn no_race_clean_stream() {
    let mut h = WsSimHarness::new(0); // No race
    h.request_replay(0);
    let offset = h.server_write(b"AB");
    assert_eq!(offset, 0, "no race: offset should be 0");
    h.feed_pty(b"AB", offset);
    let offset2 = h.server_write(b"CD");
    assert_eq!(offset2, 2, "no race: offset should be 2");
    h.feed_pty(b"CD", offset2);
    assert_eq!(h.output_str(), "ABCD");
    // Gate matches ring exactly.
    assert_eq!(h.gate.offset(), Some(4));
    assert_eq!(h.ring.total_written(), 4);
}

#[test]
fn race_then_replay_recovery() {
    let mut h = WsSimHarness::new(5);
    // Sync with empty replay.
    h.request_replay(0);
    // Racy server write — offset is inflated.
    let offset = h.server_write(b"AB");
    assert_eq!(offset, 5, "racy offset");
    h.feed_pty(b"AB", offset);
    // Gate is at 7 (wrong), ring has 7 bytes.
    assert_eq!(h.gate.offset(), Some(7));

    // Replay recovers: reads all ring data from offset 0.
    h.gate.reset();
    h.output.clear();
    h.request_replay(0);
    // After replay, output contains the full ring contents (AB + 5 filler).
    assert_eq!(h.output.len(), 7, "replay should recover all ring data");
    assert_eq!(h.resets, 2, "two resets: initial + recovery");
}
