// SPDX-License-Identifier: BUSL-1.1
// Copyright (c) 2026 Matt Baker <fimbaz@gmail.com>

//! The land wall (gd-bD6BNm16oA): a land's purse account (`lands-<land>`) is
//! never rebalanced onto, nor pushed to, a fleet session, and a land session is
//! never moved off it nor handed a fleet account.

use std::collections::{HashMap, HashSet};
use std::sync::atomic::Ordering;
use std::sync::Arc;

use super::e2e_tests::{entry, mock_upstream, state_with_accounts};
use super::*;
use crate::credential::failover::AccountInfo;
use crate::credential::land::{fleet_rebalance_candidates, known_land_accounts};

fn names(v: &[&str]) -> HashSet<String> {
    v.iter().map(|n| (*n).to_owned()).collect()
}

/// Mark `accounts` usable in the bead-authoritative account cache.
async fn cache_usable(state: &MuxState, accounts: &[&str]) {
    let mut cache = state.account_cache.write().await;
    for name in accounts {
        cache.accounts_mut().insert(
            (*name).to_owned(),
            AccountInfo {
                name: (*name).to_owned(),
                status: "open".to_owned(),
                provider: "claude".to_owned(),
                ..Default::default()
            },
        );
    }
}

fn project(p: &str) -> serde_json::Value {
    serde_json::json!({ "project": p })
}

fn creds() -> HashMap<String, String> {
    HashMap::from([("CLAUDE_CODE_OAUTH_TOKEN".to_owned(), "tok".to_owned())])
}

#[tokio::test]
async fn candidates_drop_land_accounts_with_and_without_a_cache() {
    let state = state_with_accounts(&["fleet-a", "fleet-b", "lands-x"]).await;

    // No account cache: every broker account but the land's.
    assert_eq!(fleet_rebalance_candidates(&state).await, names(&["fleet-a", "fleet-b"]));

    // With a cache: the usable accounts, still minus the land's.
    cache_usable(&state, &["fleet-a", "lands-x"]).await;
    assert_eq!(fleet_rebalance_candidates(&state).await, names(&["fleet-a"]));
}

#[tokio::test]
async fn known_land_accounts_unions_broker_and_cache() {
    let state = state_with_accounts(&["fleet-a", "lands-x"]).await;
    cache_usable(&state, &["lands-y"]).await;
    assert_eq!(known_land_accounts(&state).await, names(&["lands-x", "lands-y"]));
}

#[tokio::test]
async fn refresh_failure_on_fleet_account_never_picks_land_account() -> anyhow::Result<()> {
    let state = state_with_accounts(&["fleet-a", "lands-x", "fleet-b"]).await;
    cache_usable(&state, &["fleet-a", "lands-x", "fleet-b"]).await;
    state.credential_broker.mark_throttled("fleet-a").await;
    let (url, _hits) = mock_upstream("idle").await?;
    let e = entry("s1", url, Some("fleet-a"), serde_json::Value::Null);
    state.sessions.write().await.insert("s1".into(), Arc::clone(&e));

    rebalance_from_account(&state, "fleet-a").await;

    assert_eq!(e.assigned_account.read().await.as_deref(), Some("fleet-b"));
    Ok(())
}

#[tokio::test]
async fn refresh_failure_with_only_land_account_left_moves_no_one() -> anyhow::Result<()> {
    let state = state_with_accounts(&["fleet-a", "lands-x"]).await;
    cache_usable(&state, &["fleet-a", "lands-x"]).await;
    state.credential_broker.mark_throttled("fleet-a").await;
    let (url, hits) = mock_upstream("idle").await?;
    let e = entry("s1", url, Some("fleet-a"), serde_json::Value::Null);
    state.sessions.write().await.insert("s1".into(), Arc::clone(&e));

    rebalance_from_account(&state, "fleet-a").await;

    assert_eq!(hits.switch.load(Ordering::SeqCst), 0, "fleet session moved onto a land purse");
    assert_eq!(e.assigned_account.read().await.as_deref(), Some("fleet-a"));
    Ok(())
}

#[tokio::test]
async fn refresh_failure_with_empty_cache_never_picks_land_account() -> anyhow::Result<()> {
    // No account cache: the broker-only branch must still respect the wall.
    let state = state_with_accounts(&["fleet-a", "lands-x"]).await;
    state.credential_broker.mark_throttled("fleet-a").await;
    let (url, hits) = mock_upstream("idle").await?;
    let e = entry("s1", url, Some("fleet-a"), serde_json::Value::Null);
    state.sessions.write().await.insert("s1".into(), Arc::clone(&e));

    rebalance_from_account(&state, "fleet-a").await;

    assert_eq!(hits.switch.load(Ordering::SeqCst), 0, "fleet session moved onto a land purse");
    assert_eq!(e.assigned_account.read().await.as_deref(), Some("fleet-a"));
    Ok(())
}

#[tokio::test]
async fn refresh_failure_on_land_account_moves_none_of_its_sessions() -> anyhow::Result<()> {
    let state = state_with_accounts(&["lands-x", "fleet-b"]).await;
    cache_usable(&state, &["lands-x", "fleet-b"]).await;
    state.credential_broker.mark_throttled("lands-x").await;
    let (url, hits) = mock_upstream("idle").await?;
    let e = entry("s1", url, Some("lands-x"), project("x"));
    state.sessions.write().await.insert("s1".into(), Arc::clone(&e));

    rebalance_from_account(&state, "lands-x").await;

    assert_eq!(hits.switch.load(Ordering::SeqCst), 0, "land session moved onto the fleet");
    assert_eq!(e.assigned_account.read().await.as_deref(), Some("lands-x"));
    Ok(())
}

#[tokio::test]
async fn refresh_failure_leaves_land_session_on_fleet_account_alone() -> anyhow::Result<()> {
    // A land-x session that is (wrongly) on a fleet account is not moved to
    // another fleet account: no fleet account is its to hold.
    let state = state_with_accounts(&["fleet-a", "fleet-b", "lands-x"]).await;
    cache_usable(&state, &["fleet-a", "fleet-b", "lands-x"]).await;
    state.credential_broker.mark_throttled("fleet-a").await;
    let (url, hits) = mock_upstream("idle").await?;
    let e = entry("s1", url, Some("fleet-a"), project("x"));
    state.sessions.write().await.insert("s1".into(), Arc::clone(&e));

    rebalance_from_account(&state, "fleet-a").await;

    assert_eq!(hits.switch.load(Ordering::SeqCst), 0);
    assert_eq!(e.assigned_account.read().await.as_deref(), Some("fleet-a"));
    Ok(())
}

#[tokio::test]
async fn land_account_profile_reaches_only_its_land() -> anyhow::Result<()> {
    let state = state_with_accounts(&["fleet-a", "lands-x", "lands-y"]).await;
    let (fleet_url, fleet_hits) = mock_upstream("idle").await?;
    let (x_url, x_hits) = mock_upstream("idle").await?;
    let (x_proj_url, x_proj_hits) = mock_upstream("idle").await?;
    let (y_url, y_hits) = mock_upstream("idle").await?;
    {
        let mut sessions = state.sessions.write().await;
        // A fleet session with no profiles_needed filter would otherwise
        // receive every account's profile.
        sessions.insert("f".into(), entry("f", fleet_url, Some("fleet-a"), project("gardenio")));
        sessions.insert("x".into(), entry("x", x_url, Some("lands-x"), project("x")));
        // A land-x session known only by its project.
        sessions.insert("xp".into(), entry("xp", x_proj_url, None, project("x")));
        sessions.insert("y".into(), entry("y", y_url, Some("lands-y"), project("y")));
    }

    distribute_to_sessions(&state, "lands-x", &creds(), false).await;

    assert_eq!(fleet_hits.profiles.load(Ordering::SeqCst), 0, "fleet session got a land purse");
    assert_eq!(y_hits.profiles.load(Ordering::SeqCst), 0, "land y got land x's purse");
    assert_eq!(x_hits.profiles.load(Ordering::SeqCst), 1);
    assert_eq!(x_proj_hits.profiles.load(Ordering::SeqCst), 1);
    Ok(())
}

#[tokio::test]
async fn fleet_account_profile_skips_land_sessions() -> anyhow::Result<()> {
    let state = state_with_accounts(&["fleet-a", "lands-x"]).await;
    let (fleet_url, fleet_hits) = mock_upstream("idle").await?;
    let (x_url, x_hits) = mock_upstream("idle").await?;
    let (x_proj_url, x_proj_hits) = mock_upstream("idle").await?;
    {
        let mut sessions = state.sessions.write().await;
        sessions.insert("f".into(), entry("f", fleet_url, Some("fleet-a"), project("gardenio")));
        // An empty profiles_needed admits everything — but not past the wall.
        let mut meta = project("x");
        meta["profiles_needed"] = serde_json::json!([]);
        sessions.insert("x".into(), entry("x", x_url, Some("lands-x"), meta));
        sessions.insert("xp".into(), entry("xp", x_proj_url, Some("fleet-a"), project("x")));
    }

    distribute_to_sessions(&state, "fleet-a", &creds(), true).await;

    assert_eq!(fleet_hits.profiles.load(Ordering::SeqCst), 1);
    assert_eq!(x_hits.profiles.load(Ordering::SeqCst), 0, "land session got a fleet account");
    assert_eq!(x_proj_hits.profiles.load(Ordering::SeqCst), 0, "land session got a fleet account");
    assert_eq!(x_proj_hits.switch.load(Ordering::SeqCst), 0);
    Ok(())
}

#[test]
fn session_accepts_account_applies_the_wall_before_profiles_needed() {
    let all = serde_json::json!({ "agent": "claude", "profiles_needed": ["*"] });
    assert!(!session_accepts_account(&all, "lands-x", "claude", None));
    assert!(session_accepts_account(&all, "lands-x", "claude", Some("x")));
    assert!(!session_accepts_account(&all, "lands-x", "claude", Some("y")));
    assert!(!session_accepts_account(&all, "fleet-a", "claude", Some("x")));
    assert!(session_accepts_account(&all, "fleet-a", "claude", None));
}
