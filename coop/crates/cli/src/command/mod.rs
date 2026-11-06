// SPDX-License-Identifier: BUSL-1.1
// Copyright (c) 2026 Matt Baker <fimbaz@gmail.com>

//! CLI subcommands: `attach`, `open`, `send`, `cred`, `peek`.

pub mod attach;
pub mod cred;
pub mod open;
pub mod peek;
pub mod send;

#[cfg(test)]
mod token_tests;

/// Resolve the bearer token for mux-facing subcommands (`peek`, `cred`).
///
/// Precedence:
///   1. `flag` — explicit `--token` value from the CLI.
///   2. `GARDENIO_TOKEN` — preferred for the edge-auth (`cli.gardenio.<env>`)
///      path where the token is a user-scoped credential (e.g. GitLab PAT).
///   3. `COOP_MUX_TOKEN` — direct-to-mux canonical env var.
///   4. `COOP_BROKER_TOKEN` — legacy alias for `COOP_MUX_TOKEN`.
///
/// Empty-string env values are skipped so callers can disable a higher-priority
/// source without unsetting it. See `plans/stage3-ava-cli-eb-rollout.md`.
pub fn resolve_mux_token(flag: Option<&str>) -> Option<String> {
    if let Some(t) = flag {
        if !t.is_empty() {
            return Some(t.to_owned());
        }
    }
    for k in ["GARDENIO_TOKEN", "COOP_MUX_TOKEN", "COOP_BROKER_TOKEN"] {
        if let Ok(v) = std::env::var(k) {
            if !v.is_empty() {
                return Some(v);
            }
        }
    }
    None
}
