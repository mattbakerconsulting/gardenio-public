// SPDX-License-Identifier: BUSL-1.1
// Copyright (c) 2026 Matt Baker <fimbaz@gmail.com>

//! Offset-gated dedup for PTY replay streams.
//!
//! When a WebSocket client subscribes to `pty` events and requests `replay:get`,
//! there is a race window where broadcast PTY events cover the same byte range
//! as the replay response. Writing both to the terminal corrupts the display.
//!
//! `ReplayGate` tracks the highest committed byte offset and deduplicates
//! incoming Replay and Pty messages so each byte is written exactly once.
//!
//! The TypeScript mirror lives at `crates/web/src/lib/replay-gate.ts`.
//! Changes must be mirrored and both test suites must pass.

/// Action returned by [`ReplayGate::on_replay`].
pub struct ReplayAction {
    /// Number of leading bytes to skip (already seen).
    pub skip: usize,
    /// True on the very first replay (caller should reset the terminal).
    pub is_first: bool,
}

/// Offset-gated dedup for interleaved Replay and Pty WebSocket messages.
///
/// Tracks a high-water mark (`next_offset`) representing the next byte the
/// terminal needs. Messages whose data falls entirely below this mark are
/// dropped; partially-overlapping messages are sliced to the unseen suffix.
pub struct ReplayGate {
    /// `None` = pre-replay (pending first sync). Pty messages are dropped in
    /// this state because the terminal hasn't been initialized yet.
    next_offset: Option<u64>,
}

impl Default for ReplayGate {
    fn default() -> Self {
        Self::new()
    }
}

impl ReplayGate {
    /// Create a new gate in the pending (pre-replay) state.
    pub fn new() -> Self {
        Self { next_offset: None }
    }

    /// Reset to the pending state (for reconnect or Ctrl+L refresh).
    pub fn reset(&mut self) {
        self.next_offset = None;
    }

    /// Current byte offset, or `None` if no replay has been received yet.
    pub fn offset(&self) -> Option<u64> {
        self.next_offset
    }

    /// Process an incoming Replay message.
    ///
    /// Returns a [`ReplayAction`] describing how many leading bytes to skip
    /// and whether this is the first replay (caller should reset the terminal),
    /// or `None` if the entire message should be dropped.
    pub fn on_replay(
        &mut self,
        data_len: usize,
        _offset: u64,
        next_offset: u64,
    ) -> Option<ReplayAction> {
        let is_first = self.next_offset.is_none();
        let gate = self.next_offset.unwrap_or(0);

        if !is_first && next_offset <= gate {
            // This replay is entirely behind our high-water mark.
            return None;
        }

        let skip =
            if next_offset > gate { gate.saturating_sub(next_offset - data_len as u64) } else { 0 }
                as usize;

        self.next_offset = Some(next_offset);
        Some(ReplayAction { skip, is_first })
    }

    /// Process an incoming Pty broadcast message.
    ///
    /// Returns the number of leading bytes to skip (0 = write all), or `None`
    /// to drop the message entirely.
    pub fn on_pty(&mut self, data_len: usize, offset: u64) -> Option<usize> {
        let gate = self.next_offset?; // None → pre-replay, drop
        let msg_end = offset + data_len as u64;
        if msg_end <= gate {
            return None; // Entirely behind the high-water mark.
        }
        let skip = gate.saturating_sub(offset) as usize;
        self.next_offset = Some(msg_end);
        Some(skip)
    }
}

#[cfg(test)]
#[path = "replay_gate_tests.rs"]
mod tests;
