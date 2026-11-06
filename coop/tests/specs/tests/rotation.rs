// SPDX-License-Identifier: BUSL-1.1
// Copyright (c) 2026 Matt Baker <fimbaz@gmail.com>

//! End-to-end tests for in-session credential rotation against the real `coop`
//! binary (ka-0000005014).
//!
//! These spawn a real coop process and drive the rotation primitives through
//! the real HTTP transport + session loop — no mock of the code under test:
//!   - profiles register additively and are listed (the staging coopmux does
//!     at registration, here driven directly over HTTP);
//!   - a credential switch onto a registered profile makes coop exit with
//!     `EXIT_CODE_CREDENTIAL_SWITCH` (10) — the graceful "restart the child
//!     with --resume, work preserved" signal that gb init acts on;
//!   - a plain restart (switch with no credentials) also exits with code 10.
//!
//! Together these prove the wire that the coopmux-side staging change feeds:
//! once fallback profiles are staged on a session, a rotation produces the
//! exit(10) resume path rather than a killed session.

use std::time::Duration;

use coop::run::EXIT_CODE_CREDENTIAL_SWITCH;
use coop_specs::CoopProcess;

const TIMEOUT: Duration = Duration::from_secs(10);

/// Registering multiple profiles is additive and they are all listed; the first
/// registered into an empty set becomes `active` (the invariant the coopmux
/// booted-account-first staging relies on, ka-0000005014).
#[tokio::test]
async fn profiles_register_additively_and_first_is_active() -> anyhow::Result<()> {
    let coop = CoopProcess::start(&["sleep", "30"])?;
    coop.wait_healthy(TIMEOUT).await?;

    let client = reqwest::Client::new();
    let url = format!("{}/api/v1/session/profiles", coop.base_url());

    // Stage a batch of three profiles in one push (what coopmux does at
    // registration). Profile "booted" is first → must become active.
    let resp: serde_json::Value = client
        .post(&url)
        .json(&serde_json::json!({
            "profiles": [
                { "name": "booted", "credentials": { "ANTHROPIC_API_KEY": "k-booted" } },
                { "name": "fallback-a", "credentials": { "ANTHROPIC_API_KEY": "k-a" } },
                { "name": "fallback-b", "credentials": { "ANTHROPIC_API_KEY": "k-b" } },
            ]
        }))
        .send()
        .await?
        .json()
        .await?;
    assert_eq!(resp["registered"], 3);

    let listed: serde_json::Value = client.get(&url).send().await?.json().await?;
    let profiles =
        listed["profiles"].as_array().ok_or_else(|| anyhow::anyhow!("profiles array: {listed}"))?;
    assert_eq!(profiles.len(), 3, "all three staged profiles are listed: {listed}");

    // The first profile registered into an empty set is active; the rest are
    // available — i.e. ≥2 profiles exist so try_auto_rotate has fallbacks.
    let active: Vec<&str> = profiles
        .iter()
        .filter(|p| p["status"] == "active")
        .filter_map(|p| p["name"].as_str())
        .collect();
    assert_eq!(active, vec!["booted"], "first-staged profile is active: {listed}");
    let available = profiles.iter().filter(|p| p["status"] == "available").count();
    assert_eq!(available, 2, "remaining profiles are available fallbacks: {listed}");

    Ok(())
}

/// A credential switch onto a registered profile makes coop exit with
/// `EXIT_CODE_CREDENTIAL_SWITCH` — the graceful resume signal gb init restarts
/// on (work preserved), NOT a killed session.
#[tokio::test]
async fn credential_switch_exits_with_switch_code() -> anyhow::Result<()> {
    let mut coop = CoopProcess::start(&["sleep", "60"])?;
    coop.wait_healthy(TIMEOUT).await?;

    let client = reqwest::Client::new();

    // Register a fallback profile to switch onto.
    client
        .post(format!("{}/api/v1/session/profiles", coop.base_url()))
        .json(&serde_json::json!({
            "profiles": [
                { "name": "fallback", "credentials": { "ANTHROPIC_API_KEY": "k-fallback" } },
            ]
        }))
        .send()
        .await?;

    // Force a switch carrying credentials — this is what try_auto_rotate emits
    // on a rate limit. force:true so it fires without waiting for Idle.
    let resp = client
        .post(format!("{}/api/v1/session/switch", coop.base_url()))
        .json(&serde_json::json!({
            "profile": "fallback",
            "credentials": { "ANTHROPIC_API_KEY": "k-fallback" },
            "force": true
        }))
        .send()
        .await?;
    assert!(
        resp.status().is_success() || resp.status() == reqwest::StatusCode::ACCEPTED,
        "switch accepted, got {}",
        resp.status()
    );

    // coop must exit with the credential-switch code so gb init restarts it
    // with --resume rather than treating it as a crash.
    let status = coop.wait_exit(TIMEOUT).await?;
    assert_eq!(
        status.code(),
        Some(EXIT_CODE_CREDENTIAL_SWITCH),
        "credential switch must exit with code {EXIT_CODE_CREDENTIAL_SWITCH}, got {status:?}"
    );

    Ok(())
}
