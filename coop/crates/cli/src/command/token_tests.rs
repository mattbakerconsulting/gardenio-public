// SPDX-License-Identifier: BUSL-1.1
// Copyright (c) 2026 Matt Baker <fimbaz@gmail.com>

use super::resolve_mux_token;

/// Guard against parallel env-var races from other tests in this binary
/// (mirrors `command::attach_tests::ENV_LOCK`).
static ENV_LOCK: std::sync::Mutex<()> = std::sync::Mutex::new(());

const TOKEN_ENVS: [&str; 3] = ["GARDENIO_TOKEN", "COOP_MUX_TOKEN", "COOP_BROKER_TOKEN"];

fn clear_env() {
    for k in TOKEN_ENVS {
        std::env::remove_var(k);
    }
}

#[test]
fn flag_overrides_all_envs() {
    let _lock = ENV_LOCK.lock();
    clear_env();
    std::env::set_var("GARDENIO_TOKEN", "gb-1");
    std::env::set_var("COOP_MUX_TOKEN", "cm-1");
    std::env::set_var("COOP_BROKER_TOKEN", "cb-1");
    assert_eq!(resolve_mux_token(Some("flag-1")), Some("flag-1".into()));
    clear_env();
}

#[test]
fn flag_empty_string_falls_back_to_env() {
    let _lock = ENV_LOCK.lock();
    clear_env();
    std::env::set_var("GARDENIO_TOKEN", "gb-1");
    assert_eq!(resolve_mux_token(Some("")), Some("gb-1".into()));
    clear_env();
}

#[test]
fn gardenio_token_wins_over_mux_token() {
    let _lock = ENV_LOCK.lock();
    clear_env();
    std::env::set_var("GARDENIO_TOKEN", "gb-1");
    std::env::set_var("COOP_MUX_TOKEN", "cm-1");
    assert_eq!(resolve_mux_token(None), Some("gb-1".into()));
    clear_env();
}

#[test]
fn mux_token_wins_over_broker_token() {
    let _lock = ENV_LOCK.lock();
    clear_env();
    std::env::set_var("COOP_MUX_TOKEN", "cm-1");
    std::env::set_var("COOP_BROKER_TOKEN", "cb-1");
    assert_eq!(resolve_mux_token(None), Some("cm-1".into()));
    clear_env();
}

#[test]
fn broker_token_used_when_only_source() {
    let _lock = ENV_LOCK.lock();
    clear_env();
    std::env::set_var("COOP_BROKER_TOKEN", "cb-1");
    assert_eq!(resolve_mux_token(None), Some("cb-1".into()));
    clear_env();
}

#[test]
fn no_token_returns_none() {
    let _lock = ENV_LOCK.lock();
    clear_env();
    assert_eq!(resolve_mux_token(None), None);
}

#[test]
fn empty_env_value_is_skipped() {
    let _lock = ENV_LOCK.lock();
    clear_env();
    std::env::set_var("GARDENIO_TOKEN", "");
    std::env::set_var("COOP_MUX_TOKEN", "cm-1");
    assert_eq!(resolve_mux_token(None), Some("cm-1".into()));
    clear_env();
}
