// SPDX-License-Identifier: BUSL-1.1
// Copyright (c) 2026 Matt Baker <fimbaz@gmail.com>

//! Pre-spawn preparation for `--agent codex` sessions.
//!
//! Creates the session directory, the hook FIFO and token, and writes coop's
//! hook fragment where the pod-init step can pick it up. Unlike Claude and
//! Gemini, nothing is injected into the command line: Codex reads its config
//! from `$CODEX_HOME`, which gardenio's init owns (ADR 0004 §7).

use super::hooks::write_hook_config;
use crate::driver::SessionSetup;

/// Env var naming the generated hooks fragment, for the pod-init step.
pub const HOOKS_JSON_ENV: &str = "COOP_CODEX_HOOKS_JSON";

/// Prepare a Codex session setup.
///
/// - **pristine**: no FIFO or hooks.
/// - **fresh**: FIFO, hook token and the hooks fragment.
pub fn prepare(coop_url: &str, pristine: bool) -> anyhow::Result<SessionSetup> {
    let session_id = uuid::Uuid::new_v4().to_string();
    let session_dir = crate::driver::coop_session_dir(&session_id)?;

    if pristine {
        return Ok(SessionSetup {
            session_id,
            hook_pipe_path: None,
            session_log_path: None,
            session_dir,
            env_vars: vec![("COOP_URL".to_string(), coop_url.to_string())],
            extra_args: vec![],
            hook_token: None,
        });
    }

    let hook_pipe_path = session_dir.join("hook.pipe");
    let hooks_json = write_hook_config(&session_dir, &hook_pipe_path)?;
    let (hook_token, hook_token_path) = crate::driver::write_hook_token(&session_dir)?;

    let mut env_vars = crate::driver::hook_env_vars(&hook_pipe_path, coop_url);
    env_vars.push((HOOKS_JSON_ENV.to_string(), hooks_json.display().to_string()));
    env_vars.push(("COOP_HOOK_TOKEN_FILE".to_string(), hook_token_path.display().to_string()));

    Ok(SessionSetup {
        session_id,
        hook_pipe_path: Some(hook_pipe_path),
        // The rollout path is only known once the thread opens; the backend
        // publishes it through the resolved-log-path slot.
        session_log_path: None,
        session_dir,
        env_vars,
        extra_args: vec![],
        hook_token: Some(hook_token),
    })
}

#[cfg(test)]
#[path = "setup_tests.rs"]
mod tests;
