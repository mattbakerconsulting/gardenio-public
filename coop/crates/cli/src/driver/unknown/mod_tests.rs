// SPDX-License-Identifier: BUSL-1.1
// Copyright (c) 2026 Matt Baker <fimbaz@gmail.com>

use std::sync::Arc;

use super::{build_detectors, nudge_encoder, respond_encoder};
use crate::config::Config;

#[test]
fn build_detectors_without_config_returns_one_tier() -> anyhow::Result<()> {
    let config = Config::test();
    let detectors = build_detectors(&config, Arc::new(|| None), Arc::new(|| 0), None)?;
    assert_eq!(detectors.len(), 1);
    assert_eq!(detectors[0].tier(), 4);
    Ok(())
}

#[test]
fn build_detectors_config_without_snapshot_fn_errors() {
    let mut config = Config::test();
    config.agent_config = Some("/nonexistent/config.json".into());
    let result = build_detectors(&config, Arc::new(|| None), Arc::new(|| 0), None);
    assert!(result.is_err());
}

#[test]
fn nudge_encoder_returns_none() {
    assert!(nudge_encoder().is_none());
}

#[test]
fn respond_encoder_returns_none() {
    assert!(respond_encoder().is_none());
}
