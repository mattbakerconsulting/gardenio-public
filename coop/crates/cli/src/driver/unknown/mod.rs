// SPDX-License-Identifier: BUSL-1.1
// Copyright (c) 2026 Matt Baker <fimbaz@gmail.com>

use std::sync::Arc;

use crate::config::Config;
use crate::screen::ScreenTextSnapshot;

use super::process::ProcessMonitor;
use super::screen_parse::{load_agent_config, ScreenParser};
use super::{Detector, NudgeEncoder, RespondEncoder};

/// Build detectors for the unknown driver.
///
/// Always includes a Tier 4 `ProcessMonitor`. When `agent_config` is provided,
/// also includes a Tier 5 `ScreenParser` (requires `snapshot_fn`).
pub fn build_detectors(
    config: &Config,
    child_pid: Arc<dyn Fn() -> Option<u32> + Send + Sync>,
    ring_total_written: Arc<dyn Fn() -> u64 + Send + Sync>,
    snapshot_fn: Option<Arc<dyn Fn() -> Arc<ScreenTextSnapshot> + Send + Sync>>,
) -> anyhow::Result<Vec<Box<dyn Detector>>> {
    let mut detectors: Vec<Box<dyn Detector>> = Vec::new();

    detectors.push(Box::new(
        ProcessMonitor::new(child_pid, ring_total_written)
            .with_poll_interval(config.process_poll()),
    ));

    if let Some(ref config_path) = config.agent_config {
        let snap_fn = snapshot_fn.ok_or_else(|| {
            anyhow::anyhow!("snapshot_fn is required when agent_config is provided")
        })?;
        let patterns = load_agent_config(config_path)?;
        detectors.push(Box::new(
            ScreenParser::new(patterns, snap_fn).with_poll_interval(config.screen_poll()),
        ));
    }

    Ok(detectors)
}

/// Unknown driver does not support nudge.
pub fn nudge_encoder() -> Option<Box<dyn NudgeEncoder>> {
    None
}

/// Unknown driver does not support respond.
pub fn respond_encoder() -> Option<Box<dyn RespondEncoder>> {
    None
}

#[cfg(test)]
#[path = "mod_tests.rs"]
mod tests;
