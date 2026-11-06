// SPDX-License-Identifier: BUSL-1.1
// Copyright (c) 2026 Matt Baker <fimbaz@gmail.com>

//! Shared nudge encoder used by all agent drivers.

use std::time::Duration;

use crate::driver::{NudgeEncoder, NudgeStep};

/// Basic nudge encoder: types the message, waits a scaled delay, then presses Enter.
pub struct SafeNudgeEncoder {
    /// Base delay between typing the message and pressing enter to send.
    pub input_delay: Duration,
    /// Per-byte delay added for messages longer than 256 bytes.
    pub input_delay_per_byte: Duration,
}

impl NudgeEncoder for SafeNudgeEncoder {
    fn encode(&self, message: &str) -> Vec<NudgeStep> {
        let delay = crate::driver::compute_nudge_delay(
            self.input_delay,
            self.input_delay_per_byte,
            message.len(),
        );
        vec![
            NudgeStep { bytes: message.as_bytes().to_vec(), delay_after: Some(delay) },
            NudgeStep { bytes: b"\r".to_vec(), delay_after: None },
        ]
    }
}
