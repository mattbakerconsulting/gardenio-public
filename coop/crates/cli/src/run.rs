// SPDX-License-Identifier: BUSL-1.1
// Copyright (c) 2026 Matt Baker <fimbaz@gmail.com>

//! Top-level session runner — shared by `main` and integration tests.

use std::path::{Path, PathBuf};
use std::sync::atomic::{AtomicBool, AtomicI32, AtomicU32, AtomicU64, Ordering};
use std::sync::Arc;
use std::time::Instant;

use tokio::net::TcpListener;
use tokio::sync::{broadcast, mpsc, RwLock};
use tokio_util::sync::CancellationToken;
use tracing::{error, info};

use tracing_subscriber::EnvFilter;

use crate::backend::adapter::{AdapterSpec, TmuxBackend};
use crate::backend::codex::{CodexAppServerBackend, CodexBackendOptions, CodexExecBackend};
use crate::backend::gemini::{GeminiAcpBackend, GeminiBackendOptions};
use crate::backend::spawn::NativePty;
use crate::backend::Backend;
use crate::config::{self, Config, GroomLevel};
use crate::driver::claude::resume;
use crate::driver::claude::setup as claude_setup;
use crate::driver::codex::auth as codex_auth;
use crate::driver::codex::setup as codex_setup;
use crate::driver::codex::CodexMode;
use crate::driver::gemini::session as gemini_session;
use crate::driver::gemini::setup as gemini_setup;
use crate::driver::gemini::GeminiMode;
use crate::driver::AgentType;
use crate::driver::{
    build_claude_driver, build_codex_driver, build_gemini_driver, AgentState, DetectorSinks,
    DriverContext, SessionSetup,
};
use crate::event::InputEvent;
use crate::event_log::EventLog;
use crate::profile::ProfileState;
use crate::record::RecordingState;
use crate::ring::RingBuffer;
use crate::screen::Screen;
use crate::session::{Session, SessionConfig, SessionOutcome};
use crate::start::StartState;
use crate::stop::StopState;
use crate::switch::{SwitchRequest, SwitchState};
use crate::transcript::TranscriptState;
#[cfg(not(debug_assertions))]
use crate::transport::build_router;
#[cfg(debug_assertions)]
use crate::transport::build_router_hot;
use crate::transport::grpc::CoopGrpc;
use crate::transport::state::{
    DetectionInfo, DriverState, LifecycleState, SessionSettings, TerminalState, TransportChannels,
};
use crate::transport::{build_health_router, build_metrics_router, Store};
use crate::usage::UsageState;

pub struct RunResult {
    pub status: crate::driver::ExitStatus,
    pub store: Arc<Store>,
}

/// Exit code indicating a credential switch was requested.
///
/// When gb init sees this exit code, it reads the switch metadata file,
/// applies new credentials, and restarts coop with `--resume`.  This makes
/// gb init the single authority for resume decisions.
pub const EXIT_CODE_CREDENTIAL_SWITCH: i32 = 10;

/// Well-known path for credential switch metadata.
///
/// Written by coop before exiting with [`EXIT_CODE_CREDENTIAL_SWITCH`];
/// read and cleaned up by gb init before restarting coop.
pub const CREDENTIAL_SWITCH_FILE: &str = "/tmp/coop_credential_switch.json";

// ── Shared session setup helpers ─────────────────────────────────────────

/// Parameters for [`build_agent_setup`].
struct AgentSetupParams<'a> {
    agent_enum: AgentType,
    working_dir: &'a std::path::Path,
    coop_url: &'a str,
    base_settings: Option<&'a serde_json::Value>,
    mcp_config: Option<&'a serde_json::Value>,
    pristine: bool,
    /// For initial startup, this comes from `--resume` CLI option parsing.
    resume_log_path: Option<&'a std::path::Path>,
    /// Authoritative Claude session UUID for resumes (e.g.
    /// `config.s3_resume_session`). When `None`, `prepare_resume` parses it
    /// from the JSONL transcript content (the file-stem-based "predicted
    /// UUID" path was removed in ka-0000003077).
    resume_session_id: Option<&'a str>,
}

/// True when `agent` can pick a stored session back up from S3: Claude
/// transcripts, Codex rollouts, and Gemini sessions driven over ACP
/// (`session/load`; the PTY modes have no resume hook).
#[cfg(feature = "s3")]
fn can_resume_from_s3(agent: AgentType, command: &[String]) -> bool {
    match agent {
        AgentType::Claude | AgentType::Codex => true,
        AgentType::Gemini => GeminiMode::from_command(command) == GeminiMode::Acp,
        _ => false,
    }
}

/// Prepare agent-specific setup (session ID, env vars, CLI args, paths).
///
/// Delegates to the agent's setup module (Claude, Gemini, etc.) and returns
/// the unified [`SessionSetup`].  Used by `prepare()` on initial startup.
fn build_agent_setup(params: &AgentSetupParams<'_>) -> anyhow::Result<Option<SessionSetup>> {
    match params.agent_enum {
        AgentType::Claude => Ok(Some(claude_setup::prepare(
            params.working_dir,
            params.coop_url,
            params.base_settings,
            params.mcp_config,
            params.pristine,
            params.resume_log_path,
            params.resume_session_id,
        )?)),
        AgentType::Gemini => Ok(Some(gemini_setup::prepare(
            params.coop_url,
            params.base_settings,
            params.mcp_config,
            params.pristine,
        )?)),
        AgentType::Codex => Ok(Some(codex_setup::prepare(params.coop_url, params.pristine)?)),
        _ => Ok(None),
    }
}

/// Merge credential overrides into env vars and write the OAuth credentials
/// file for Claude agents.
///
/// `base_env` is the env vars from the agent setup.  `overrides` contains
/// credential keys from a [`SwitchRequest`] or from the process environment
/// (initial startup).  For Claude, the OAuth token is also written to
/// `~/.claude/.credentials.json` since Claude Code reads it from disk.
fn apply_credentials(
    base_env: &mut Vec<(String, String)>,
    overrides: &std::collections::HashMap<String, String>,
    agent: AgentType,
) {
    for (k, v) in overrides {
        // The Codex auth document is written to $CODEX_HOME/auth.json below,
        // never exported to the agent's environment.
        if k == codex_auth::AUTH_ENV {
            continue;
        }
        if let Some(existing) = base_env.iter_mut().find(|(ek, _)| ek == k) {
            existing.1 = v.clone();
        } else {
            base_env.push((k.clone(), v.clone()));
        }
    }

    // Claude Code reads OAuth tokens from ~/.claude/.credentials.json rather
    // than env vars, so write the file for any OAuth token we find.
    if agent == AgentType::Claude {
        let token =
            overrides.get("CLAUDE_CODE_OAUTH_TOKEN").or_else(|| overrides.get("ANTHROPIC_API_KEY"));
        if let Some(token) = token {
            if !token.is_empty() {
                if let Err(e) = claude_setup::write_credentials_file(token) {
                    error!("failed to write OAuth credentials file: {e}");
                }
            }
        }
    }

    // Codex reads its ChatGPT-subscription login from $CODEX_HOME/auth.json.
    if agent == AgentType::Codex {
        codex_auth::apply_credentials(overrides);
    }
}

/// Header-line prefix for LiteLLM spend-attribution tags inside the
/// newline-separated `ANTHROPIC_CUSTOM_HEADERS` value.
const LITELLM_TAGS_PREFIX: &str = "x-litellm-tags:";

/// Preserve the pod's `x-litellm-tags` header line across credential switches.
///
/// The controller bakes `ANTHROPIC_CUSTOM_HEADERS` into the pod env as TWO
/// lines: the per-account `x-litellm-api-key` plus the per-agent
/// `x-litellm-tags` (agent:/role:/project:/model: spend attribution — see
/// gardenio podspec.go). The coopmux credential broker is account-keyed and
/// knows nothing about per-agent tags, so the credentials it distributes on
/// rotation carry only the api-key line. Applying such a switch verbatim
/// drops the tags — every subsequent request becomes invisible to per-agent
/// settlement and the role/project/model spend gauges (ka-0000000333: ~8% of
/// the daily bill went untagged after fleet-wide rotations).
///
/// The merge is session-side by design (tags are per-agent, the broker is
/// per-account): take the tags line from the env this process inherited and
/// append it to the incoming header set. The merged value flows into the
/// switch metadata file, gb init re-exports it, and the next coop session
/// inherits a tagged header again — so the tags survive any number of
/// rotations. Incoming headers that already carry their own tags line win
/// unchanged, so this becomes a no-op if the broker ever turns tags-aware.
/// (A cross-account switch keeps the spawn-time model: tag; by-model
/// attribution is account-default granularity per podspec.go.)
fn merge_litellm_tags(new_headers: &str, existing_headers: &str) -> String {
    fn find_tags_line(headers: &str) -> Option<&str> {
        headers
            .lines()
            .map(str::trim)
            .find(|l| l.to_ascii_lowercase().starts_with(LITELLM_TAGS_PREFIX))
    }
    if find_tags_line(new_headers).is_some() {
        return new_headers.to_owned();
    }
    let Some(tags_line) = find_tags_line(existing_headers) else {
        return new_headers.to_owned();
    };
    if new_headers.is_empty() {
        return tags_line.to_owned();
    }
    format!("{new_headers}\n{tags_line}")
}

/// Return the switch request's credentials with the `x-litellm-tags` line
/// merged in from the environment (see [`merge_litellm_tags`]).
///
/// `get_env` is injected for testability (same pattern as
/// [`initial_profile_from`]); production passes `std::env::var`.
fn merged_switch_credentials(
    credentials: Option<&std::collections::HashMap<String, String>>,
    get_env: impl Fn(&str) -> Option<String>,
) -> Option<std::collections::HashMap<String, String>> {
    let creds = credentials?;
    let mut creds = creds.clone();
    if let Some(headers) = creds.get("ANTHROPIC_CUSTOM_HEADERS") {
        if let Some(existing) = get_env("ANTHROPIC_CUSTOM_HEADERS") {
            let merged = merge_litellm_tags(headers, &existing);
            creds.insert("ANTHROPIC_CUSTOM_HEADERS".to_owned(), merged);
        }
    }
    Some(creds)
}

/// Build an initial named credential profile from the controller-injected
/// environment, if one is present.
///
/// Returns `(account_name, credentials)` when both an account name
/// (`BOAT_ACCOUNT`) and a usable credential are found in the environment.
/// This is the credential the controller baked into the pod at spawn; naming
/// it lets coop report the account it is actually using via
/// [`ProfileState::active_name`] before coopmux distributes any profiles.
///
/// Returns `None` when `BOAT_ACCOUNT` is unset/empty (the account identity is
/// unknown, so there is nothing accurate to report) or when no credential env
/// var is set. The credential map mirrors the keys [`apply_credentials`]
/// understands, so a later switch back to this profile reproduces the same
/// environment.
fn initial_profile_from_env(
    agent: AgentType,
) -> Option<(String, std::collections::HashMap<String, String>)> {
    initial_profile_from(agent, |name| std::env::var(name).ok())
}

/// Seed `profile_state` with the controller-injected account credential so
/// [`ProfileState::active_name`] reports the account the pod is actually using
/// before coopmux ever pushes a profile.
///
/// This is the exact wiring [`prepare`] performs at session setup, factored out
/// so the controller→coop account-seeding contract (read real `BOAT_ACCOUNT`
/// from the environment → register it → it becomes the Active profile) is
/// testable end-to-end without standing up a backend. See ka-0000002509 and the
/// DIM-C test-adequacy gap (ka-0000004278): the Playwright e2e suite drives a
/// mock coop, so only a test that drives this real env→active_name() path can
/// catch a regression in the seed itself.
async fn seed_initial_profile(profile_state: &ProfileState, agent: AgentType) {
    if let Some((account, creds)) = initial_profile_from_env(agent) {
        profile_state
            .register(vec![crate::profile::ProfileEntry {
                name: account,
                credentials: creds,
                // The env-injected booted account seeds Active; no health signal
                // to apply yet (coopmux pushes it later), so leave it unset.
                usable: None,
            }])
            .await;
    }
}

/// Inner implementation that accepts a lookup function for testability.
fn initial_profile_from(
    agent: AgentType,
    get_env: impl Fn(&str) -> Option<String>,
) -> Option<(String, std::collections::HashMap<String, String>)> {
    let account = get_env("BOAT_ACCOUNT").filter(|s| !s.is_empty())?;

    let mut creds = std::collections::HashMap::new();
    // Only the session's own harness's keys seed the profile: a Codex pod must
    // not seed a Claude token (nor a Claude pod a Codex auth document), or the
    // profile would be of the wrong harness and never rotate correctly.
    let codex = agent == AgentType::Codex;
    let token_keys: &[&str] = if codex {
        &[codex_auth::AUTH_ENV]
    } else {
        &["CLAUDE_CODE_OAUTH_TOKEN", "ANTHROPIC_API_KEY", "GEMINI_API_KEY"]
    };
    // Routing/context env the controller sets alongside the credential. These
    // are not secrets but travel with the profile so a switch-back restores the
    // same upstream wiring (LiteLLM base URL, model, per-agent spend headers).
    let routing_keys: &[&str] = if codex {
        &[codex_auth::MODEL_ENV]
    } else {
        &["ANTHROPIC_BASE_URL", "ANTHROPIC_MODEL", "ANTHROPIC_CUSTOM_HEADERS"]
    };
    for key in token_keys.iter().chain(routing_keys) {
        if let Some(val) = get_env(key) {
            if !val.is_empty() {
                creds.insert((*key).to_owned(), val);
            }
        }
    }

    // Require at least one actual credential token — an account name with no
    // credential is not a usable profile. (Bedrock-routed accounts authenticate
    // via base_url + headers and may legitimately carry no OAuth token, so a
    // base_url alone also counts as usable.)
    let has_token = creds.contains_key("CLAUDE_CODE_OAUTH_TOKEN")
        || creds.contains_key("ANTHROPIC_API_KEY")
        || creds.contains_key("GEMINI_API_KEY")
        || creds.contains_key(codex_auth::AUTH_ENV);
    let has_routing = creds.contains_key("ANTHROPIC_BASE_URL");
    if !has_token && !has_routing {
        tracing::debug!(
            account,
            agent = %agent,
            "BOAT_ACCOUNT set but no credential env found; not seeding initial profile"
        );
        return None;
    }

    tracing::info!(account, "seeding initial credential profile from env (BOAT_ACCOUNT)");
    Some((account, creds))
}

/// Build the composite driver (detectors + encoders) for an agent type.
///
/// Includes the Tier 5 screen detector for Claude agents.  The `sinks`
/// closure is called once to produce detector sinks.
// TODO(refactor): group related params into a DriverConfig struct
#[allow(clippy::too_many_arguments)]
fn build_driver(
    config: &Config,
    agent_enum: AgentType,
    setup: Option<&SessionSetup>,
    log_start_offset: u64,
    sinks: DetectorSinks,
    snapshot_fn: Option<Arc<dyn Fn() -> Arc<crate::screen::ScreenTextSnapshot> + Send + Sync>>,
    child_pid_fn: Option<Arc<dyn Fn() -> Option<u32> + Send + Sync>>,
    ring_total_fn: Option<Arc<dyn Fn() -> u64 + Send + Sync>>,
) -> anyhow::Result<DriverContext> {
    let mut driver = match agent_enum {
        AgentType::Claude => build_claude_driver(config, setup, log_start_offset, sinks)?,
        AgentType::Gemini => build_gemini_driver(
            config,
            GeminiMode::from_command(&config.command),
            setup,
            child_pid_fn.unwrap_or_else(|| Arc::new(|| None)),
            ring_total_fn.unwrap_or_else(|| Arc::new(|| 0)),
            sinks,
        )?,
        AgentType::Unknown => DriverContext {
            nudge_encoder: None,
            respond_encoder: None,
            detectors: crate::driver::unknown::build_detectors(
                config,
                child_pid_fn.unwrap_or_else(|| Arc::new(|| None)),
                ring_total_fn.unwrap_or_else(|| Arc::new(|| 0)),
                None,
            )?,
            option_parser: None,
            screen_classifier: None,
        },
        AgentType::Codex => build_codex_driver(
            config,
            CodexMode::from_command(&config.command),
            setup,
            child_pid_fn.unwrap_or_else(|| Arc::new(|| None)),
            ring_total_fn.unwrap_or_else(|| Arc::new(|| 0)),
            sinks,
        )?,
    };

    // Tier 5: Claude screen detector for idle prompt detection.
    if agent_enum == AgentType::Claude {
        if let Some(snap) = snapshot_fn {
            driver.detectors.push(Box::new(
                crate::driver::claude::screen::ClaudeScreenDetector::new(config, snap),
            ));
            driver.detectors.sort_by_key(|d| d.tier());
        }
    }

    Ok(driver)
}

/// A fully-prepared session ready to run.
///
/// Returned by [`prepare`] so callers can access [`Store`]
/// including broadcast channels and the shutdown token.
pub struct PreparedSession {
    pub store: Arc<Store>,
    /// `Option` because `Session::run` takes ownership; after each run we
    /// build a new Session for the next switch iteration.
    session: Option<Session>,
    config: Config,
    /// Receivers that survive across switch iterations.
    consumer_input_rx: mpsc::Receiver<InputEvent>,
    switch_rx: mpsc::Receiver<SwitchRequest>,
    #[cfg(feature = "s3")]
    s3_handle: Option<crate::s3::S3SessionHandle>,
}

impl PreparedSession {
    /// Run the session loop to completion, handling credential switches.
    ///
    /// gb init is the single authority for resume decisions.  When a credential
    /// switch is requested, coop writes switch metadata to disk and exits with
    /// [`EXIT_CODE_CREDENTIAL_SWITCH`] so gb init can restart coop with the
    /// correct `--resume` flag and re-inject the work prompt.
    pub async fn run(mut self) -> anyhow::Result<RunResult> {
        let session = self.session.take().ok_or_else(|| anyhow::anyhow!("no session available"))?;
        let outcome =
            session.run(&self.config, &mut self.consumer_input_rx, &mut self.switch_rx).await?;

        match outcome {
            SessionOutcome::Exit(status) => {
                // Save a final transcript snapshot so the current session
                // log is preserved. Without this, sessions that never hit
                // context compaction (short sessions, normal exits) would
                // have zero transcripts archived.
                if let Err(e) = self.store.transcript.save_snapshot().await {
                    tracing::warn!("final transcript snapshot failed: {e}");
                }

                // Provide exit info to the S3 subscriber before finalizing.
                // Fold in the graceful-shutdown bit so a signal-termination
                // code (143/130/129) on the gb-stop / pod-teardown path is
                // stamped "stopped" rather than a false-positive "error"
                // (ka-0000001328). The token is cancelled by the SIGTERM/SIGINT/
                // SIGHUP handler (run.rs) and by transport shutdown requests.
                #[cfg(feature = "s3")]
                if let Some(ref handle) = self.s3_handle {
                    let shutdown_requested = self.store.lifecycle.shutdown.is_cancelled();
                    let exit_status = crate::s3::exit_status_from_code(
                        status.code,
                        status.signal,
                        shutdown_requested,
                        // Set by the idle-timeout branch (session/run.rs) before it
                        // cancels the shutdown token, so an idle-suspend teardown is
                        // stamped "suspended" for warm-pool reclaim (ka-0000002921).
                        self.store.lifecycle.idle_suspended.load(Ordering::Acquire),
                    );
                    handle.set_exit(status.code, exit_status);
                }

                // Agent exited — wait for a switch or shutdown.
                //
                // On a non-switch terminal exit we must finalize the S3
                // subscriber explicitly (drain its OWN token + await the
                // flush) so the final transcript snapshot saved just above,
                // plus the stamped exit info, actually land in S3. The
                // subscriber's drain token is decoupled from the session
                // shutdown token precisely so an external SIGTERM/SIGHUP/
                // idle-timeout (which cancels lifecycle.shutdown) can no
                // longer race ahead and upload a placeholder meta.json
                // (exit_status="error", transcript_count=0) before this
                // finalize runs. See ka-0000001666.
                if self.store.lifecycle.shutdown.is_cancelled() {
                    self.finalize_s3().await;
                    return Ok(RunResult { status, store: self.store });
                }
                info!(
                    "agent exited (code={:?}, signal={:?}), awaiting switch or shutdown",
                    status.code, status.signal
                );
                let req = tokio::select! {
                    req = self.switch_rx.recv() => match req {
                        Some(req) => req,
                        None => {
                            self.finalize_s3().await;
                            return Ok(RunResult { status, store: self.store });
                        }
                    },
                    _ = self.store.lifecycle.shutdown.cancelled() => {
                        self.finalize_s3().await;
                        return Ok(RunResult { status, store: self.store });
                    }
                };

                // Credential switch after exit: write metadata and exit so
                // gb init can restart with the correct resume decision.
                self.write_switch_metadata(&req).await?;
                self.flush_s3_for_switch().await;
                Ok(RunResult {
                    status: crate::driver::ExitStatus {
                        code: Some(EXIT_CODE_CREDENTIAL_SWITCH),
                        signal: None,
                    },
                    store: self.store,
                })
            }
            SessionOutcome::Switch(req) => {
                // Credential switch mid-session: save transcript, write
                // metadata, and exit so gb init can restart.
                if let Err(e) = self.store.transcript.save_snapshot().await {
                    tracing::warn!("transcript snapshot before switch failed: {e}");
                }
                self.write_switch_metadata(&req).await?;
                self.flush_s3_for_switch().await;
                Ok(RunResult {
                    status: crate::driver::ExitStatus {
                        code: Some(EXIT_CODE_CREDENTIAL_SWITCH),
                        signal: None,
                    },
                    store: self.store,
                })
            }
        }
    }

    /// Flush pending in-memory transcript / recording / event state to S3
    /// before exiting on a credential switch.
    ///
    /// Without this, when coop exits on credential rotation the tokio
    /// runtime (and any in-flight S3 uploads) is destroyed by
    /// `std::process::exit`, and the next spawn under the same
    /// agent-bead-keyed prefix would resume from the previous periodic-sync
    /// point — losing any transcript/recording activity since the last
    /// upload interval.
    ///
    /// Steps:
    /// 1. Stamp the S3 subscriber's exit info as `EXIT_CODE_CREDENTIAL_SWITCH`
    ///    / `"switch"` so the final `meta.json` reflects the rotation.
    /// 2. Finalize the subscriber: cancel its own drain token and await the
    ///    join handle with a bounded timeout so a hung S3 endpoint cannot
    ///    wedge the rotation.
    async fn flush_s3_for_switch(&mut self) {
        #[cfg(feature = "s3")]
        if let Some(ref handle) = self.s3_handle {
            handle.set_exit(Some(EXIT_CODE_CREDENTIAL_SWITCH), "switch".into());
        }
        self.finalize_s3().await;
    }

    /// Finalize the S3 subscriber on a terminal exit path.
    ///
    /// Drains the subscriber's *own* token (decoupled from the session
    /// shutdown token) and awaits the final flush so the last transcript
    /// snapshot and stamped exit info land in S3 before the process exits.
    /// Callers stamp exit info via [`S3SessionHandle::set_exit`] first.
    /// Idempotent — takes the handle so a second call is a no-op.
    async fn finalize_s3(&mut self) {
        #[cfg(feature = "s3")]
        {
            let Some(handle) = self.s3_handle.take() else {
                return;
            };
            let drained = handle.finalize(std::time::Duration::from_secs(30)).await;
            if !drained {
                tracing::warn!(
                    "s3: did not finish draining before exit; \
                     final transcript/meta may be missing in S3"
                );
            } else {
                tracing::info!("s3: drained in-memory state before exit");
            }
        }
    }

    /// Write credential switch metadata to disk and apply credentials.
    ///
    /// gb init reads [`CREDENTIAL_SWITCH_FILE`] on restart to pick up the
    /// new credentials as environment variables for the next coop session.
    async fn write_switch_metadata(&self, request: &SwitchRequest) -> anyhow::Result<()> {
        let agent_enum = self.config.agent_enum()?;

        // Broker-pushed credentials are account-keyed and drop the per-agent
        // x-litellm-tags header line; re-merge it from the pod env so spend
        // attribution survives the rotation (ka-0000000333).
        let credentials =
            merged_switch_credentials(request.credentials.as_ref(), |k| std::env::var(k).ok());

        // Apply credentials to disk (writes ~/.claude/.credentials.json for
        // OAuth tokens) so the file is up-to-date for the next session.
        if let Some(ref creds) = credentials {
            let mut env_vars = Vec::new();
            apply_credentials(&mut env_vars, creds, agent_enum);
        }

        // Write switch metadata for gb init.
        let mut metadata = serde_json::json!({
            "credentials": credentials,
            "profile": request.profile,
        });
        // Codex resumes by thread id, not by Claude's transcript path: hand gb
        // init the id of the thread this session was running so the respawn on
        // the new account continues it.
        if agent_enum == AgentType::Codex {
            let rollout = self.store.switch.session_log_path.read().await.clone();
            if let Some(thread) = rollout.as_deref().and_then(codex_auth::thread_id_from_rollout) {
                metadata["resume"] = serde_json::Value::String(thread);
            }
        }
        // Gemini resumes by session id, read from the session record's header.
        if agent_enum == AgentType::Gemini
            && GeminiMode::from_command(&self.config.command) == GeminiMode::Acp
        {
            let record = self.store.switch.session_log_path.read().await.clone();
            if let Some(id) = record.as_deref().and_then(gemini_session::session_id_from_record) {
                metadata["resume"] = serde_json::Value::String(id);
            }
        }
        write_switch_file(CREDENTIAL_SWITCH_FILE, &serde_json::to_string_pretty(&metadata)?)?;

        info!(
            profile = ?request.profile,
            "credential switch metadata written to {}, exiting with code {}",
            CREDENTIAL_SWITCH_FILE, EXIT_CODE_CREDENTIAL_SWITCH
        );
        Ok(())
    }
}

/// Write the credential switch file readable by its owner only: it holds the
/// pushed access token (or Codex `auth.json` document). A stale file from an
/// earlier switch keeps its old mode under `create`, so tighten it explicitly.
fn write_switch_file(path: &str, contents: &str) -> anyhow::Result<()> {
    use std::io::Write;
    use std::os::unix::fs::{OpenOptionsExt, PermissionsExt};

    let mut file = std::fs::OpenOptions::new()
        .write(true)
        .create(true)
        .truncate(true)
        .mode(0o600)
        .open(path)?;
    file.set_permissions(std::fs::Permissions::from_mode(0o600))?;
    file.write_all(contents.as_bytes())?;
    Ok(())
}

/// Run a coop session to completion.
///
/// This is the full production codepath: prepare claude session, build driver,
/// spawn backend, start servers, and run the session loop.
pub async fn run(config: Config) -> anyhow::Result<RunResult> {
    prepare(config).await?.run().await
}

/// Initialize tracing/logging from config.
///
/// Uses `try_init` so it's safe to call multiple times (e.g. from tests).
pub fn init_tracing(config: &Config) {
    use tracing_subscriber::fmt;

    // Priority: --log-level / COOP_LOG_LEVEL > RUST_LOG > default ("info").
    let filter = if std::env::var("COOP_LOG_LEVEL").is_err() && config.log_level == "info" {
        EnvFilter::try_from_default_env().unwrap_or_else(|_| EnvFilter::new(&config.log_level))
    } else {
        EnvFilter::try_new(&config.log_level).unwrap_or_else(|_| EnvFilter::new("info"))
    };

    let result = match config.log_format.as_str() {
        "json" => fmt::fmt().with_env_filter(filter).json().try_init(),
        _ => fmt::fmt().with_env_filter(filter).try_init(),
    };
    drop(result);
}

/// Prepare a coop session: set up driver, spawn backend, start servers.
///
/// Returns a [`PreparedSession`] whose [`AppState`] is accessible before
/// calling [`PreparedSession::run`] to enter the session loop.
pub async fn prepare(mut config: Config) -> anyhow::Result<PreparedSession> {
    init_tracing(&config);

    let shutdown = CancellationToken::new();
    let agent_enum = config.agent_enum()?;

    // 0. Load agent config file if provided.
    let agent_file_config = match config.agent_config {
        Some(ref path) => Some(config::load_agent_config(path)?),
        None => None,
    };
    let stop_config = agent_file_config.as_ref().and_then(|c| c.stop.clone()).unwrap_or_default();
    let start_config = agent_file_config.as_ref().and_then(|c| c.start.clone()).unwrap_or_default();
    let base_settings = agent_file_config.as_ref().and_then(|c| c.settings.clone());
    let mcp_config = agent_file_config.as_ref().and_then(|c| c.mcp.clone());

    // Times the whole resume preparation — S3 transcript restore (when
    // configured) through local discovery + parse — for the /metrics
    // resume report (ka-0000000550).
    let resume_prepare_started = Instant::now();

    // 0b. S3 resume: download session data from S3 before local resume discovery.
    #[cfg(feature = "s3")]
    let s3_client: Option<std::sync::Arc<crate::s3::S3Client>> = if config.s3_bucket.is_some()
        || config.s3_resume_session.is_some()
        || config.s3_session_key.is_some()
    {
        let bucket = config.s3_bucket.clone().unwrap_or_default();
        if bucket.is_empty()
            && (config.s3_resume_session.is_some() || config.s3_session_key.is_some())
        {
            anyhow::bail!("--s3-resume-session / --s3-session-key require --s3-bucket");
        }
        if !bucket.is_empty() {
            match crate::s3::S3Client::new(
                bucket,
                config.s3_prefix.clone(),
                config.s3_region.clone(),
                config.s3_endpoint.clone(),
                config.s3_access_key_id_file.clone(),
                config.s3_secret_access_key_file.clone(),
            )
            .await
            {
                Ok(client) => Some(std::sync::Arc::new(client)),
                Err(e) => {
                    error!("s3: failed to create client: {e}");
                    None
                }
            }
        } else {
            None
        }
    } else {
        None
    };

    // The S3 prefix component used to restore. When a bead is supplied, the
    // sessions live under `<bead>/<spawn>/...` — pick the most recent spawn
    // by metadata `started_at`. With only a legacy resume-session, use it
    // verbatim (flat layout). Without either, no restore.
    //
    // Force-clean (`--s3-no-restore` / COOP_S3_NO_RESTORE): the controller has
    // decided this pod must start FRESH — the prior life crossed a turn/token/wake
    // threshold past which resuming its (huge) transcript fails at boot. Skip the
    // restore entirely so no prior `session.jsonl` is discovered for `--resume`.
    // The upload path is unaffected (it keys off config.s3_session_key directly),
    // so this session's transcript still lands under the stable prefix — only the
    // boot-time rehydration is suppressed. See gardenio ka-0000007242.
    #[cfg(feature = "s3")]
    let s3_restore_key: Option<String> = if !can_resume_from_s3(agent_enum, &config.command) {
        // Only Claude transcripts, Codex rollouts and Gemini ACP sessions
        // know how to resume.
        None
    } else if force_clean_active(config.s3_no_restore) {
        if s3_client.is_some() {
            info!(
                "s3: --s3-no-restore set — forcing a clean session (skipping transcript restore)"
            );
        }
        None
    } else if let Some(ref s3) = s3_client {
        crate::s3::pick_restore_key(
            &**s3,
            config.s3_session_key.as_deref(),
            config.s3_resume_session.as_deref(),
        )
        .await
    } else {
        None
    };

    // A session written by another harness cannot be resumed by this one
    // (ADR 0004: no cross-harness migration), e.g. a bead whose agent moved from
    // Claude to Codex. Start fresh instead of feeding one harness the other's log.
    #[cfg(feature = "s3")]
    let s3_restore_key: Option<String> = match (&s3_client, s3_restore_key) {
        (Some(s3), Some(key)) => {
            use crate::s3::S3Storage;
            match s3.download_meta(&key).await {
                Ok(Some(meta)) if meta.harness_name() != agent_enum.to_string() => {
                    tracing::warn!(
                        session = %key,
                        stored = %meta.harness_name(),
                        running = %agent_enum,
                        "s3: stored session belongs to a different harness; starting fresh"
                    );
                    None
                }
                _ => Some(key),
            }
        }
        (_, key) => key,
    };

    #[cfg(feature = "s3")]
    if let (Some(ref s3), Some(ref source_session)) = (&s3_client, &s3_restore_key) {
        info!(session = %source_session, "s3: restoring session data for resume");

        // Determine a local directory for restored transcripts.
        // Use the working directory to build a Claude-style session dir.
        let restore_dir = {
            let state_home = std::env::var("XDG_STATE_HOME").unwrap_or_else(|_| {
                let home = std::env::var("HOME").unwrap_or_else(|_| "/tmp".into());
                format!("{home}/.local/state")
            });
            PathBuf::from(format!("{state_home}/coop/sessions/{source_session}"))
        };
        let transcripts_dir = restore_dir.join("transcripts");

        match crate::s3::restore_transcripts(&**s3, source_session, &transcripts_dir).await {
            Ok(count) => info!(count, "s3: restored transcripts from S3"),
            Err(e) => tracing::warn!("s3: failed to restore transcripts: {e}"),
        }

        // Download session log so --resume can discover it.
        let session_log_dest = restore_dir.join("session.jsonl");
        let mut session_log_restored = false;
        if let Err(e) =
            crate::s3::restore_session_log(&**s3, source_session, &session_log_dest).await
        {
            tracing::warn!("s3: failed to restore session log: {e}");
        } else {
            session_log_restored = true;
        }

        // Materialize the restored transcript where Claude actually reads it on
        // `--resume <uuid>`: `<config>/projects/<project>/<uuid>.jsonl`. Without
        // this copy, `--resume <uuid>` fails with "No conversation found with
        // session ID: <uuid>", the session exits fast, and the gb restart loop
        // retries the same broken resume — a visible restart loop (ka-0000007228).
        //
        // The `<uuid>` is the Claude conversation UUID, parsed from the restored
        // transcript content (ground truth — see `resume::parse_session_id`),
        // NOT the coop S3 session key.
        if session_log_restored && agent_enum == AgentType::Codex {
            // Codex: the restored `session.jsonl` is the rollout. Put it back
            // under `$CODEX_HOME/sessions/...` and resume by thread id
            // (`thread/resume` in app-server mode, `exec resume <id>` in exec
            // mode). There is no companion workflow dir to restore.
            let home = crate::driver::codex::rollout::codex_home();
            match crate::driver::codex::rollout::place_restored_rollout(&session_log_dest, &home) {
                Ok(Some(thread_id)) => {
                    if config.resume.is_none() {
                        info!(thread = %thread_id, "s3: restored Codex rollout; resuming by thread id");
                        config.resume = Some(thread_id);
                    }
                }
                Ok(None) => tracing::warn!(
                    "s3: restored session log is not a Codex rollout; starting fresh"
                ),
                Err(e) => tracing::warn!("s3: failed to place restored Codex rollout: {e}"),
            }
        } else if session_log_restored && agent_enum == AgentType::Gemini {
            // Gemini: the restored `session.jsonl` is the session record. Put it
            // back under `<gemini-home>/tmp/<slug>/chats/` and resume by session
            // id (`session/load` in ACP mode).
            let home = gemini_session::gemini_home();
            let cwd = std::env::current_dir().unwrap_or_else(|_| PathBuf::from("."));
            let slug = gemini_session::project_slug(&cwd);
            match gemini_session::place_restored_session(&session_log_dest, &home, &slug) {
                Ok(Some(session_id)) => {
                    if config.resume.is_none() {
                        info!(session = %session_id, "s3: restored Gemini session; resuming by session id");
                        config.resume = Some(session_id);
                    }
                }
                Ok(None) => tracing::warn!(
                    "s3: restored session log is not a Gemini session record; starting fresh"
                ),
                Err(e) => tracing::warn!("s3: failed to place restored Gemini session: {e}"),
            }
        } else if session_log_restored {
            if let Some(uuid) = crate::driver::claude::resume::parse_session_id(&session_log_dest) {
                let working_dir = std::env::current_dir().unwrap_or_else(|_| PathBuf::from("."));
                let claude_log =
                    crate::driver::claude::setup::session_log_path(&working_dir, &uuid);

                // Place the transcript at the Claude-read path (creating the
                // projects/<hash>/ dir, skipping if a fresher local copy already
                // exists), then point --resume at it.
                match place_restored_transcript(&session_log_dest, &claude_log).await {
                    Ok(true) => {
                        if config.resume.is_none() {
                            config.resume = Some(claude_log.display().to_string());
                            info!("s3: auto-set --resume to restored transcript at Claude path");
                        }
                    }
                    Ok(false) => {
                        // Copy failed but a usable path may still exist; fall
                        // back to the raw coop-sessions transcript for resume.
                        if config.resume.is_none() {
                            config.resume = Some(session_log_dest.display().to_string());
                        }
                    }
                    Err(e) => {
                        tracing::warn!(
                            "s3: failed to place transcript at Claude path {}: {e}",
                            claude_log.display()
                        );
                        if config.resume.is_none() {
                            config.resume = Some(session_log_dest.display().to_string());
                        }
                    }
                }

                // Restore the dynamic-workflow companion dir (`agent-*.jsonl` +
                // run-state) so an interrupted Workflow run resumes via
                // `resumeFromRunId` instead of re-running from scratch after
                // off-PVC recovery. The journals must land in the sibling
                // directory `<config>/projects/<project>/<uuid>/`.
                if let Some(companion_dir) = crate::s3::workflow_companion_dir(&claude_log) {
                    match crate::s3::restore_workflow_dir(&**s3, source_session, &companion_dir)
                        .await
                    {
                        Ok(0) => {}
                        Ok(count) => info!(
                            count,
                            dir = %companion_dir.display(),
                            "s3: restored workflow companion dir from S3"
                        ),
                        Err(e) => tracing::warn!("s3: failed to restore workflow dir: {e}"),
                    }
                }
            } else {
                // No parseable UUID — fall back to the coop-sessions copy so
                // resume discovery can at least try the raw transcript path.
                tracing::warn!(
                    "s3: restored transcript has no parseable sessionId; \
                     resume may fail and fall back to fresh"
                );
                if config.resume.is_none() {
                    config.resume = Some(session_log_dest.display().to_string());
                }
            }
        }
    }

    // Force-clean also suppresses any explicit local resume hint (--resume /
    // COOP_RESUME): the flag's contract is "start this session FRESH", so a
    // leftover local transcript path must not resurrect the very conversation the
    // controller decided is too big to resume. The S3 restore above is already
    // skipped; clearing config.resume here closes the local-hint path too.
    if config.resume.is_some() && force_clean_active(config.s3_no_restore) {
        info!("s3: --s3-no-restore set — ignoring local --resume hint to start fresh");
        config.resume = None;
    }

    // 1. Handle --resume: discover session log and build resume state.
    //    Graceful fallback: if the session log is missing, corrupt, or too
    //    small to contain a real conversation, start a fresh session.
    //    This avoids restart loops when a pod was SIGKILL'd before Claude Code
    //    finished persisting conversation state.
    let (resume_state, resume_log_path, resume_outcome) = if let Some(resume_hint) =
        config.resume.as_ref().filter(|_| agent_enum == AgentType::Claude)
    {
        match resume::discover_session_log(resume_hint) {
            Ok(Some(log_path)) => {
                info!("resuming from session log: {}", log_path.display());
                match resume::parse_resume_state(&log_path) {
                    Ok(state) => (Some(state), Some(log_path), "resumed"),
                    Err(e) => {
                        tracing::warn!(
                            "failed to parse resume state from {}, starting fresh: {e}",
                            log_path.display()
                        );
                        (None, None, "fresh_parse_error")
                    }
                }
            }
            Ok(None) => {
                tracing::warn!("no resumable session found for {resume_hint}, starting fresh");
                (None, None, "fresh_no_session")
            }
            Err(e) => {
                tracing::warn!("resume discovery failed, starting fresh: {e}");
                (None, None, "fresh_discovery_error")
            }
        }
    } else {
        (None, None, "")
    };

    // 1b. Resolve port 0 → real port early, before agent setup needs the URL.
    //     Bind immediately so the OS assigns an ephemeral port; we keep the
    //     listener and hand it to axum later (no TOCTOU race).
    let mut pre_bound_http = None;
    if config.port == Some(0) {
        let addr = format!("{}:0", config.host);
        let listener = TcpListener::bind(&addr).await?;
        let real_port = listener.local_addr()?.port();
        config.port = Some(real_port);
        pre_bound_http = Some(listener);
        info!("resolved port 0 → {real_port}");
    }

    // 2. Prepare agent session setup. Each agent's setup module produces a
    //    unified `SessionSetup` containing env vars, CLI args, and paths.
    //    In pristine mode, hooks/FIFO are omitted but Tier 2 paths are kept.
    let working_dir = std::env::current_dir()?;
    let coop_url_for_setup = format!("http://127.0.0.1:{}", config.port.unwrap_or(0));
    let pristine = config.groom_level()? == GroomLevel::Pristine;

    let base_settings = base_settings.as_ref();
    let mcp_config = mcp_config.as_ref();

    let setup = build_agent_setup(&AgentSetupParams {
        agent_enum,
        working_dir: &working_dir,
        coop_url: &coop_url_for_setup,
        base_settings,
        mcp_config,
        pristine,
        resume_log_path: resume_log_path.as_deref(),
        resume_session_id: config.s3_resume_session.as_deref(),
    })?;

    // Finalize the /metrics resume report (ka-0000000550). Discovery said
    // "resumed", but claude_setup::prepare can still fall through to a fresh
    // session when the transcript carries no parseable session UUID — detect
    // that by whether the setup actually injected `--resume` args.
    let resume_report = if resume_outcome.is_empty() {
        None
    } else {
        let outcome = if resume_outcome == "resumed"
            && !setup.as_ref().is_some_and(|s| s.extra_args.iter().any(|a| a == "--resume"))
        {
            "fresh_no_uuid"
        } else {
            resume_outcome
        };
        let log_bytes = resume_log_path
            .as_deref()
            .and_then(|p| p.metadata().ok())
            .map(|m| m.len())
            .unwrap_or(0);
        Some(crate::transport::state::ResumeReport {
            outcome,
            prepare_seconds: resume_prepare_started.elapsed().as_secs_f64(),
            log_bytes,
        })
    };

    // 3. Build the command with extra args from setup.
    let mut command = config.command.clone();
    if let Some(ref s) = setup {
        inject_extra_args(&mut command, &s.extra_args);
    }

    // 4. Build terminal state early so driver closures can reference its atomics.
    let terminal = Arc::new(TerminalState {
        screen: RwLock::new(Screen::new(config.cols, config.rows)),
        ring: RwLock::new(RingBuffer::new(config.ring_size)),
        ring_total_written: Arc::new(AtomicU64::new(0)),
        child_pid: AtomicU32::new(0),
        exit_status: RwLock::new(None),
    });

    // 5. Build driver (detectors + encoders). For Claude, uses real paths
    //    from the setup so detectors actually activate.
    //    Create raw broadcast channels early so detectors can capture senders.
    let (hook_tx, _) = broadcast::channel(64);
    // Sized well above 64 so a tight-loop catch-up burst from the log detector
    // cannot Lagged-drop an assistant_text before the NATS forwarder is
    // scheduled (ka-0000006823). See `event::MESSAGE_TX_CAPACITY`.
    let (message_tx, _) = broadcast::channel(crate::event::MESSAGE_TX_CAPACITY);

    let last_message: Arc<RwLock<Option<String>>> = Arc::new(RwLock::new(None));
    let usage_state = Arc::new(UsageState::new());
    // Shared slot where the Tier 2 LogDetector publishes the resolved session
    // log path after discovering Claude's actual UUID-named file.
    let resolved_log_path: Arc<RwLock<Option<PathBuf>>> = Arc::new(RwLock::new(None));
    // Shared slot the S3 subscriber reads on each upload. Seeded with the
    // predicted setup path (which Claude probably won't actually write to —
    // see `claude/setup.rs`); the propagation task overwrites it with the
    // Tier 2-resolved path so periodic uploads target the file Claude is
    // actually appending to.
    let s3_log_path: Arc<RwLock<Option<PathBuf>>> =
        Arc::new(RwLock::new(setup.as_ref().and_then(|s| s.session_log_path.clone())));
    // Codex's PTY-less modes tee their raw JSON stream to the Tier 3 detector.
    let codex_mode = (agent_enum == AgentType::Codex).then(|| CodexMode::from_command(&command));
    let (codex_tee_tx, codex_tee_rx) = match codex_mode {
        Some(mode) if mode.is_headless() => {
            let (tx, rx) = mpsc::channel(256);
            (Some(tx), Some(rx))
        }
        _ => (None, None),
    };
    // Gemini's ACP mode does the same.
    let gemini_mode = (agent_enum == AgentType::Gemini).then(|| GeminiMode::from_command(&command));
    let (gemini_tee_tx, gemini_tee_rx) = match gemini_mode {
        Some(mode) if mode.is_headless() => {
            let (tx, rx) = mpsc::channel(256);
            (Some(tx), Some(rx))
        }
        _ => (None, None),
    };
    let mut sinks = DetectorSinks::default()
        .with_last_message(Arc::clone(&last_message))
        .with_hook_tx(hook_tx.clone())
        .with_message_tx(message_tx.clone())
        .with_usage(Arc::clone(&usage_state))
        .with_resolved_log_path(Arc::clone(&resolved_log_path));
    if let Some(rx) = codex_tee_rx.or(gemini_tee_rx) {
        sinks = sinks.with_stdout_rx(rx);
    }
    let log_start_offset = resume_state.as_ref().map(|s| s.log_offset).unwrap_or(0);
    let driver = build_driver(
        &config,
        agent_enum,
        setup.as_ref(),
        log_start_offset,
        sinks,
        Some(terminal.snapshot_fn()),
        Some(terminal.child_pid_fn()),
        Some(terminal.ring_total_written_fn()),
    )?;

    // 5a. Write credentials file if CLAUDE_CODE_OAUTH_TOKEN env var is set.
    // Claude Code reads OAuth tokens from ~/.claude/.credentials.json rather
    // than the env var, so we bridge the gap on initial startup too.
    if agent_enum == AgentType::Claude {
        if let Ok(token) = std::env::var("CLAUDE_CODE_OAUTH_TOKEN") {
            if !token.is_empty() {
                if let Err(e) = claude_setup::write_credentials_file(&token) {
                    error!("failed to write OAuth credentials file on startup: {e}");
                }
            }
        }
    }

    // 5b. Codex: materialise the pushed ChatGPT-subscription login. This covers
    // a credential switch, where the new document arrives via env. Nothing in
    // this repo copies a mounted session Secret at pod start, so a cold start
    // has no auth.json until coopmux pushes the profile (register -> switch);
    // until then Codex runs unauthenticated.
    if agent_enum == AgentType::Codex {
        if let Ok(doc) = std::env::var(codex_auth::AUTH_ENV) {
            codex_auth::apply_credentials(&std::collections::HashMap::from([(
                codex_auth::AUTH_ENV.to_owned(),
                doc,
            )]));
        }
    }

    // 6. Spawn backend AFTER driver is built (FIFO must exist before child starts).
    let extra_env = setup.as_ref().map(|s| s.env_vars.as_slice()).unwrap_or(&[]);
    let codex_options = CodexBackendOptions {
        cwd: Some(working_dir.display().to_string()),
        model: (agent_enum == AgentType::Codex)
            .then(|| std::env::var(codex_auth::MODEL_ENV).ok().filter(|m| !m.is_empty()))
            .flatten(),
        resume_thread: config.resume.clone().filter(|_| agent_enum == AgentType::Codex),
        rollout_slot: Some(Arc::clone(&resolved_log_path)),
    };
    let gemini_options = GeminiBackendOptions {
        cwd: working_dir.display().to_string(),
        model: (agent_enum == AgentType::Gemini)
            .then(|| {
                std::env::var(crate::driver::gemini::acp::MODEL_ENV).ok().filter(|m| !m.is_empty())
            })
            .flatten(),
        resume_session: config.resume.clone().filter(|_| agent_enum == AgentType::Gemini),
        approval: crate::driver::gemini::acp::Approval::from_env(),
        session_slot: Some(Arc::clone(&resolved_log_path)),
    };
    let backend: Box<dyn Backend> = if let Some(ref attach_spec) = config.attach {
        let spec: AdapterSpec = attach_spec.parse()?;
        match spec {
            AdapterSpec::Tmux { session } => {
                Box::new(TmuxBackend::new(session)?.with_poll_interval(config.tmux_poll()))
            }
            AdapterSpec::Screen { session: _ } => {
                anyhow::bail!("screen attach is not yet implemented");
            }
        }
    } else {
        if command.is_empty() {
            anyhow::bail!("no command specified");
        }
        match (gemini_mode, codex_mode) {
            (Some(GeminiMode::Acp), _) => Box::new(GeminiAcpBackend::spawn(
                &command,
                extra_env,
                gemini_options,
                gemini_tee_tx,
            )?),
            (_, Some(CodexMode::AppServer)) => Box::new(CodexAppServerBackend::spawn(
                &command,
                extra_env,
                codex_options,
                codex_tee_tx,
            )?),
            (_, Some(CodexMode::Exec)) => {
                Box::new(CodexExecBackend::new(&command, extra_env, codex_options, codex_tee_tx)?)
            }
            _ => Box::new(
                NativePty::spawn(&command, config.cols, config.rows, extra_env)?
                    .with_reap_interval(config.reap_poll()),
            ),
        }
    };

    // Capture the PTY child PID *before* the backend is moved into the
    // session config below, so the signal handler can forward operator
    // signals (TERM/INT/HUP) to the PTY's foreground process group. Without
    // this, SIGTERM at the coop process only triggers `shutdown.cancel()`
    // and the PTY child is reaped via Drop's SIGHUP/SIGKILL — bypassing
    // any TERM/INT trap inside the child (e.g. claudeless's Bash trap).
    // See ka-0000005033.
    let pty_child_pid: Option<i32> = backend.child_pid().map(|p| p as i32);

    // Create shared channels
    let (input_tx, consumer_input_rx) = mpsc::channel(256);
    let (output_tx, _) = broadcast::channel(256);
    let (screen_tx, _) = broadcast::channel::<Arc<crate::screen::ScreenTextSnapshot>>(16);
    let (state_tx, _) = broadcast::channel(64);
    let (prompt_tx, _) = broadcast::channel(64);

    // Switch channel: capacity 1 enforces single-switch-at-a-time.
    let (switch_tx, switch_rx) = mpsc::channel::<SwitchRequest>(1);

    let resolve_url = format!("{coop_url_for_setup}/api/v1/stop/resolve");
    let stop_state = Arc::new(StopState::new(stop_config, resolve_url));
    let start_state = Arc::new(StartState::new(start_config));
    let switch_state = Arc::new(SwitchState {
        switch_tx,
        session_log_path: RwLock::new(setup.as_ref().and_then(|s| s.session_log_path.clone())),
        base_settings: base_settings.cloned(),
        mcp_config: mcp_config.cloned(),
    });
    let transcript_state = Arc::new(TranscriptState::new(
        setup
            .as_ref()
            .map(|s| s.session_dir.join("transcripts"))
            .unwrap_or_else(|| PathBuf::from("/tmp/coop-transcripts")),
        setup.as_ref().and_then(|s| s.session_log_path.clone()),
    )?);

    // Keep clones for the log-path propagation task (spawned below) before
    // the originals are moved into Store.
    let transcript_for_logpath = Arc::clone(&transcript_state);
    let switch_for_logpath = Arc::clone(&switch_state);

    let session_id = setup
        .as_ref()
        .map(|s| s.session_id.clone())
        .unwrap_or_else(|| uuid::Uuid::new_v4().to_string());

    // Honour the COOP_ROTATE_* env knobs in production. Resolving them once
    // here (rather than inside try_auto_rotate) keeps rotation logic — and its
    // unit tests — hermetic (ka-0000003994).
    let profile_state = Arc::new(
        ProfileState::with_rotation(crate::profile::RotationConfig::from_env())
            .for_agent(agent_enum),
    );
    // Apply --profile mode from CLI/env.
    if let Ok(mode) = config.profile.parse::<crate::profile::ProfileMode>() {
        profile_state.set_mode(mode);
    }

    // Seed the initial credential profile from the controller-injected
    // environment so the session can report which account it is actually
    // using BEFORE coopmux ever pushes a profile.
    //
    // The controller bakes one account's OAuth token into the pod env
    // (CLAUDE_CODE_OAUTH_TOKEN) and names that account in BOAT_ACCOUNT
    // (set from the authoritative `spec.ResolvedAccount`). Without seeding,
    // `Store.profile` starts empty, `active_name()` returns None, and the
    // mux's health reconcile has nothing to write — so the dashboard shows a
    // null/blank account for the pod even though it is happily using that
    // account's credential. Registering the env credential as a named Active
    // profile closes that gap: `active_name()` now returns BOAT_ACCOUNT, the
    // mux reconciles `assigned_account` to it, and the per-session account
    // display is accurate from the first health poll. See ka-0000002509.
    seed_initial_profile(&profile_state, agent_enum).await;

    let event_log = Arc::new(EventLog::new(setup.as_ref().map(|s| s.session_dir.as_path())));

    let record_state = Arc::new(RecordingState::new(
        setup.as_ref().map(|s| s.session_dir.as_path()),
        config.cols,
        config.rows,
    ));

    let store = Arc::new(Store {
        terminal,
        driver: Arc::new(DriverState {
            agent_state: RwLock::new(AgentState::Starting),
            state_seq: AtomicU64::new(0),
            detection: RwLock::new(DetectionInfo { tier: u8::MAX, cause: String::new() }),
            error: RwLock::new(None),
            last_message,
        }),
        channels: TransportChannels {
            input_tx,
            output_tx,
            screen_tx,
            state_tx,
            prompt_tx,
            hook_tx,
            message_tx,
        },
        config: SessionSettings {
            started_at: Instant::now(),
            agent: agent_enum,
            auth_token: config.auth_token.clone(),
            hook_token: setup.as_ref().and_then(|s| s.hook_token.clone()),
            nudge_encoder: driver.nudge_encoder,
            respond_encoder: driver.respond_encoder,
            nudge_timeout: config.nudge_timeout(),
            nudge_enter_max_retries: config.nudge_enter_max_retries(),
            groom: config.groom_level()?,
        },
        lifecycle: LifecycleState {
            shutdown: shutdown.clone(),
            ws_client_count: AtomicI32::new(0),
            bytes_written: AtomicU64::new(0),
            zombie: AtomicBool::new(false),
            last_transition_ms: AtomicU64::new(0),
            idle_suspended: AtomicBool::new(false),
        },
        session_id: RwLock::new(session_id),
        ready: Arc::new(AtomicBool::new(false)),
        input_gate: Arc::new(crate::transport::state::InputGate::new(config.input_delay())),
        stop: stop_state,
        switch: switch_state,
        start: start_state,
        transcript: transcript_state,
        usage: usage_state,
        profile: profile_state,
        input_activity: Arc::new(tokio::sync::Notify::new()),
        event_log: Arc::clone(&event_log),
        record: Arc::clone(&record_state),
        session_dir: setup.as_ref().map(|s| s.session_dir.clone()),
        resume_report,
    });

    // Enable recording if --record flag is set, or if S3 is configured:
    // the S3 subscriber uploads recording chunks by reading broadcasts from
    // `record_tx`, but `RecordingState::push` short-circuits when recording
    // is disabled, so chunks never reach the subscriber. Auto-enabling here
    // when S3 is wired is the only way to actually produce recording
    // artifacts in prod pods (which set `COOP_S3_BUCKET` but not `COOP_RECORD`).
    let mut enable_recording = config.record;
    #[cfg(feature = "s3")]
    if s3_client.is_some() {
        enable_recording = true;
    }
    if enable_recording {
        store.record.enable().await;
    }

    // Spawn event log subscriber — persists state/hook events to JSONL files.
    {
        let log = Arc::clone(&event_log);
        let mut state_rx = store.channels.state_tx.subscribe();
        let mut hook_rx = store.channels.hook_tx.subscribe();
        let sd = shutdown.clone();
        tokio::spawn(async move {
            loop {
                tokio::select! {
                    _ = sd.cancelled() => break,
                    event = state_rx.recv() => {
                        match event {
                            Ok(e) => log.push_transition(&e).await,
                            Err(broadcast::error::RecvError::Lagged(n)) => {
                                tracing::warn!("event log: state subscriber lagged by {n}");
                            }
                            Err(_) => break,
                        }
                    }
                    event = hook_rx.recv() => {
                        match event {
                            Ok(e) => log.push_hook(&e).await,
                            Err(broadcast::error::RecvError::Lagged(n)) => {
                                tracing::warn!("event log: hook subscriber lagged by {n}");
                            }
                            Err(_) => break,
                        }
                    }
                }
            }
        });
    }

    // Spawn log-path propagation task — when the Tier 2 LogDetector discovers
    // Claude's actual session log path (which differs from the predicted path
    // for fresh sessions), update the transcript and switch subsystems so
    // transcript snapshots and S3 uploads use the correct file.
    {
        let rlp = Arc::clone(&resolved_log_path);
        let s3_lp = Arc::clone(&s3_log_path);
        let transcript = transcript_for_logpath;
        let switch = switch_for_logpath;
        let sd = shutdown.clone();
        tokio::spawn(async move {
            // Poll until the LogDetector writes the resolved path or shutdown.
            loop {
                tokio::select! {
                    _ = sd.cancelled() => return,
                    _ = tokio::time::sleep(std::time::Duration::from_secs(2)) => {}
                }
                let path = rlp.read().await.clone();
                if let Some(p) = path {
                    info!("propagating resolved session log path: {}", p.display());
                    transcript.update_session_log_path(Some(p.clone())).await;
                    *switch.session_log_path.write().await = Some(p.clone());
                    *s3_lp.write().await = Some(p);
                    return;
                }
            }
        });
    }

    // Spawn recording subscriber — captures screen snapshots at semantic events,
    // plus periodic screenshots 30s after the last hook event.
    crate::record::spawn_subscriber(
        Arc::clone(&store.record),
        Arc::clone(&store.terminal),
        &store.channels.state_tx,
        &store.channels.hook_tx,
        shutdown.clone(),
    );

    // Spawn S3 persistence subscriber if configured.
    #[cfg(feature = "s3")]
    let s3_handle = if let Some(ref s3) = s3_client {
        // Compose the S3 prefix component. When a bead is supplied, every
        // spawn writes under `<bead>/<coop_uuid>/...` so multi-spawn writes
        // remain distinguishable; restore picks the latest. Without a bead,
        // falls back to the legacy flat layout (resume key, else coop UUID).
        let coop_session_id = store.session_id.read().await.clone();
        let s3_session_key = crate::s3::compose_session_key(
            config.s3_session_key.as_deref(),
            config.s3_resume_session.as_deref(),
            &coop_session_id,
        );
        if s3_session_key != coop_session_id {
            info!(
                key = %s3_session_key,
                coop_session = %coop_session_id,
                "s3: using composed session key"
            );
        }
        let handle = crate::s3::spawn_subscriber(
            Arc::clone(s3),
            s3_session_key,
            store.session_dir.clone(),
            &store.transcript.transcript_tx,
            &store.record.record_tx,
            &store.channels.state_tx,
            Arc::clone(&s3_log_path),
            config.s3_upload_interval(),
            agent_enum.to_string(),
            config.label.clone(),
            std::env::var("BOAT_AGENT").ok(),
            std::env::var("BOAT_PROJECT").ok(),
            std::env::var("BOAT_ROLE").ok(),
            std::env::var("KD_AGENT_ID").ok(),
            // Independent drain token — NOT shutdown.clone(). The session
            // shutdown token is cancelled by external signals (SIGTERM pod
            // restart, SIGHUP terminal loss, idle timeout); if the subscriber
            // drained on it directly it would race the session loop and upload
            // a placeholder meta.json before the final transcript snapshot was
            // saved. PreparedSession::finalize_s3 cancels THIS token after the
            // snapshot + set_exit, on every terminal exit path. See
            // ka-0000001666.
            CancellationToken::new(),
        );
        info!("s3: persistence subscriber started");
        Some(handle)
    } else {
        None
    };

    // The thaw-epoch credential re-mint handler (Stages 1-2 of the rehydration
    // design, ka-0000007211) is composed into the single coop ThawWatcher
    // alongside the NATS-reconnect handler below — see the `thaw_state_dir`
    // branch in the NATS block. Both coop stages key off the SAME marker the
    // controller writes into COOP_THAW_STATE_DIR, so they must be one watcher
    // writing one `thaw-epoch.coop.done` ack (bug ka-0000004088). The earlier
    // shape armed a second re-mint watcher on XDG_STATE_HOME/coop — a dir the
    // controller never writes the marker into — so Stages 1-2 silently no-op'd
    // on every real thaw.

    // Spawn the four NATS transports (publisher, relay, forwarder, inbox)
    // through a single supervisor so boot and thaw-time reconnect share one
    // code path. On a gVisor thaw the supervisor's `reconnect_all` tears down
    // the dead generation (sockets bound to Pod A's 4-tuple) and re-`connect()`s
    // with a freshly re-read token (rehydration-design Stage 3–4, ka-0000001901).
    if let Some(ref nats_url) = config.nats_url {
        let inbox_agent = config.inbox_agent.clone().or_else(|| std::env::var("GT_ROLE").ok());
        let params = crate::transport::nats_supervisor::NatsParams {
            url: nats_url.clone(),
            prefix: config.nats_prefix.clone(),
            relay_prefix: config.nats_relay_prefix.clone(),
            agent: agent_enum.to_string(),
            labels: config.label.clone(),
            publisher: true,
            relay: config.nats_relay.is_some(),
            forward: config.nats_forward.is_some(),
            inbox_agent,
            inbox_rig: config.inbox_rig.clone(),
            inject_dir: config
                .inject_dir
                .clone()
                .unwrap_or_else(|| PathBuf::from(".runtime/inject-queue")),
            nats_token: config.nats_token.clone(),
            nats_token_file: config.nats_token_file.clone(),
            nats_user: config.nats_user.clone(),
            nats_password: config.nats_password.clone(),
            nats_creds: config.nats_creds.clone(),
        };
        let supervisor = crate::transport::nats_supervisor::NatsSupervisor::new(
            params,
            Arc::clone(&store),
            shutdown.clone(),
        );
        // Pre-resolve the bare NATS host to an IP while DNS is warm (bug
        // ka-0000001492), but only for freezable gVisor agent pods (those with a
        // thaw state dir). A gVisor freeze snapshots this in-RAM, so the
        // post-restore thaw reconnect dials the cached IP literal and skips the
        // slow cold-netns search-domain walk that otherwise burns the whole thaw
        // deadline. Done before connect_all so the boot connect also benefits
        // from the warmed resolver cache.
        if config.thaw_state_dir.is_some() {
            supervisor.prewarm_dns().await;
        }
        supervisor.connect_all().await;

        // When a thaw-epoch marker dir is configured (gVisor agent pods), run a
        // SINGLE ThawWatcher whose handler runs both coop rehydration halves in
        // the design-mandated order on epoch bump: credential re-mint FIRST
        // (Stages 1-2 — AWS STS cache-poison + the fail-closed Pod-A-identity
        // JWT freshness gate), then NATS reconnect + presence re-announce
        // (Stages 3-4). Ordering is load-bearing (rehydration-design §5):
        // reconnecting before re-reading would reconnect under a poisoned token.
        // One watcher → one `thaw-epoch.coop.done` ack on the dir the controller
        // actually writes the marker into (bug ka-0000004088). Disabled in the
        // common non-gVisor case.
        if let Some(ref thaw_dir) = config.thaw_state_dir {
            #[cfg(feature = "s3")]
            let aws_reset =
                s3_client.as_ref().map(|s3| crate::rehydrate::s3_aws_reset(Arc::clone(s3)));
            #[cfg(not(feature = "s3"))]
            let aws_reset: Option<crate::rehydrate::AwsReset> = None;

            let reminter = crate::rehydrate::CredentialReminter::from_env(aws_reset);
            // Mark the connection stages "skipped" in the ack if the creds stage
            // fail-closes and short-circuits the reconnect.
            let handler = crate::thaw::Sequence::new(
                reminter,
                Arc::clone(&supervisor),
                &["nats_publisher", "nats_relay", "nats_forward", "nats_inbox"],
            );
            let watcher = crate::thaw::ThawWatcher::new(thaw_dir.clone(), handler);
            let sd = shutdown.clone();
            tokio::spawn(async move {
                watcher.run(sd).await;
            });
            tracing::info!(
                dir = %thaw_dir.display(),
                "thaw: coop rehydration watcher started (cred re-mint → NATS reconnect)"
            );
        }
    }

    // Spawn HTTP server
    if let Some(port) = config.port {
        #[cfg(debug_assertions)]
        let router = build_router_hot(Arc::clone(&store), config.hot);
        #[cfg(not(debug_assertions))]
        let router = build_router(Arc::clone(&store));
        let listener = match pre_bound_http.take() {
            Some(l) => l,
            None => {
                let addr = format!("{}:{}", config.host, port);
                TcpListener::bind(&addr).await?
            }
        };
        info!("HTTP listening on {}", listener.local_addr()?);
        let sd = shutdown.clone();
        tokio::spawn(async move {
            let result =
                axum::serve(listener, router).with_graceful_shutdown(sd.cancelled_owned()).await;
            if let Err(e) = result {
                error!("HTTP server error: {e}");
            }
        });
    }

    // Spawn Unix socket server
    if let Some(ref socket_path) = config.socket {
        #[cfg(debug_assertions)]
        let router = build_router_hot(Arc::clone(&store), config.hot);
        #[cfg(not(debug_assertions))]
        let router = build_router(Arc::clone(&store));
        let path = socket_path.clone();
        // Remove stale socket
        let _ = std::fs::remove_file(&path);
        let uds_listener = tokio::net::UnixListener::bind(&path)?;
        info!("Unix socket listening on {path}");
        let sd = shutdown.clone();
        tokio::spawn(async move {
            let mut make_svc = router.into_make_service();
            loop {
                tokio::select! {
                    _ = sd.cancelled() => break,
                    accept = uds_listener.accept() => {
                        match accept {
                            Ok((stream, _)) => {
                                // IntoMakeService implements Service<T> for any T
                                let svc_future = <_ as tower::Service<_>>::call(&mut make_svc, ());
                                tokio::spawn(async move {
                                    let Ok(svc) = svc_future.await;
                                    let io = hyper_util::rt::TokioIo::new(stream);
                                    let hyper_svc = hyper_util::service::TowerToHyperService::new(svc);
                                    let _ = hyper_util::server::conn::auto::Builder::new(
                                        hyper_util::rt::TokioExecutor::new(),
                                    )
                                    .serve_connection_with_upgrades(io, hyper_svc)
                                    .await;
                                });
                            }
                            Err(e) => {
                                tracing::debug!("unix socket accept error: {e}");
                            }
                        }
                    }
                }
            }
        });
    }

    // Spawn gRPC server
    if let Some(grpc_port) = config.port_grpc {
        let grpc = CoopGrpc::new(Arc::clone(&store));
        let addr = format!("{}:{}", config.host, grpc_port).parse()?;
        info!("gRPC listening on {addr}");
        let sd = shutdown.clone();
        tokio::spawn(async move {
            let result = grpc.into_router().serve_with_shutdown(addr, sd.cancelled_owned()).await;
            if let Err(e) = result {
                error!("gRPC server error: {e}");
            }
        });
    }

    // Spawn health probe
    if let Some(health_port) = config.port_health {
        let health_router = build_health_router(Arc::clone(&store));
        let addr = format!("{}:{}", config.host, health_port);
        let listener = TcpListener::bind(&addr).await?;
        info!("health probe listening on {addr}");
        let sd = shutdown.clone();
        tokio::spawn(async move {
            let result = axum::serve(listener, health_router)
                .with_graceful_shutdown(sd.cancelled_owned())
                .await;
            if let Err(e) = result {
                error!("health server error: {e}");
            }
        });
    }

    // Spawn dedicated Prometheus metrics server (unauthenticated /metrics).
    // In gardenio pods this binds :9093 so vmagent can scrape session usage
    // counters and latency histograms without the main port's bearer token.
    if let Some(metrics_port) = config.port_metrics {
        let metrics_router = build_metrics_router(Arc::clone(&store));
        let addr = format!("{}:{}", config.host, metrics_port);
        let listener = TcpListener::bind(&addr).await?;
        info!("metrics server listening on {addr}");
        let sd = shutdown.clone();
        tokio::spawn(async move {
            let result = axum::serve(listener, metrics_router)
                .with_graceful_shutdown(sd.cancelled_owned())
                .await;
            if let Err(e) = result {
                error!("metrics server error: {e}");
            }
        });
    }

    // Spawn mux self-registration client if configured.
    // Placed after all server binds so we never register a session that isn't
    // reachable, and never orphan a registration if a bind fails.
    {
        // Allow overriding the mux session ID (e.g. to use the K8s pod hostname
        // so that deep-links from Slack resolve correctly).
        let sid = match std::env::var("COOP_MUX_SESSION_ID").ok().filter(|s| !s.is_empty()) {
            Some(override_id) => override_id,
            None => store.session_id.read().await.clone(),
        };
        let mux_account = store.profile.active_name().await;
        crate::mux_client::spawn_if_configured(
            &sid,
            config.port,
            config.auth_token.as_deref(),
            config.mux_url(),
            &agent_enum.to_string(),
            &config.label,
            mux_account,
            shutdown.clone(),
        )
        .await;
    }

    // Spawn signal handler
    {
        let sd = shutdown.clone();
        let pty_pid = pty_child_pid;
        tokio::spawn(async move {
            let mut sigterm =
                tokio::signal::unix::signal(tokio::signal::unix::SignalKind::terminate()).ok();
            let mut sigint =
                tokio::signal::unix::signal(tokio::signal::unix::SignalKind::interrupt()).ok();
            // SIGHUP arrives when the controlling terminal disappears (parent
            // process death, pod restart). Without this branch the process
            // exits with code 129 before the S3 subscriber drain runs, so the
            // session leaves zero archive trail. Treat it as a graceful
            // shutdown signal so the drain has a chance to flush.
            let mut sighup =
                tokio::signal::unix::signal(tokio::signal::unix::SignalKind::hangup()).ok();

            // First signal: forward to PTY child process group, then start
            // graceful shutdown. Forwarding lets traps inside the child
            // (e.g. claudeless's `trap ... TERM INT` in Bash) fire before
            // NativePty::drop escalates to SIGHUP/SIGKILL on the group.
            // See ka-0000005033.
            tokio::select! {
                _ = async {
                    if let Some(ref mut s) = sigterm { s.recv().await } else { std::future::pending().await }
                } => {
                    info!("received SIGTERM");
                    forward_signal_to_pty_pgroup(pty_pid, nix::sys::signal::Signal::SIGTERM);
                }
                _ = async {
                    if let Some(ref mut s) = sigint { s.recv().await } else { std::future::pending().await }
                } => {
                    info!("received SIGINT");
                    forward_signal_to_pty_pgroup(pty_pid, nix::sys::signal::Signal::SIGINT);
                }
                _ = async {
                    if let Some(ref mut s) = sighup { s.recv().await } else { std::future::pending().await }
                } => {
                    info!("received SIGHUP");
                    forward_signal_to_pty_pgroup(pty_pid, nix::sys::signal::Signal::SIGHUP);
                }
            }
            sd.cancel();

            // Second signal: force exit
            tokio::select! {
                _ = async {
                    if let Some(ref mut s) = sigterm { s.recv().await } else { std::future::pending().await }
                } => {
                    info!("received SIGTERM again, forcing exit");
                    forward_signal_to_pty_pgroup(pty_pid, nix::sys::signal::Signal::SIGTERM);
                }
                _ = async {
                    if let Some(ref mut s) = sigint { s.recv().await } else { std::future::pending().await }
                } => {
                    info!("received SIGINT again, forcing exit");
                    forward_signal_to_pty_pgroup(pty_pid, nix::sys::signal::Signal::SIGINT);
                }
                _ = async {
                    if let Some(ref mut s) = sighup { s.recv().await } else { std::future::pending().await }
                } => {
                    info!("received SIGHUP again, forcing exit");
                    forward_signal_to_pty_pgroup(pty_pid, nix::sys::signal::Signal::SIGHUP);
                }
            }
            std::process::exit(130);
        });
    }

    // Build session (but don't run yet — caller may need store first).
    // Session::run borrows the receivers via &mut; PreparedSession retains
    // ownership so they survive across switch iterations.
    let mut session_config = SessionConfig::new(Arc::clone(&store), backend)
        .with_detectors(driver.detectors)
        .with_shutdown(shutdown);
    if let Some(parser) = driver.option_parser {
        session_config = session_config.with_option_parser(parser);
    }
    if let Some(classifier) = driver.screen_classifier {
        session_config = session_config.with_screen_classifier(classifier);
    }
    let session = Session::new(&config, session_config);

    // `setup` is intentionally dropped here — session artifacts live in
    // persistent XDG_STATE_HOME directories, not ephemeral temp dirs.
    drop(setup);

    Ok(PreparedSession {
        store,
        session: Some(session),
        config,
        consumer_input_rx,
        switch_rx,
        #[cfg(feature = "s3")]
        s3_handle,
    })
}

/// Forward an operator-delivered signal to the PTY child's foreground
/// process group, so traps registered inside the child (e.g. a Bash
/// `trap ... TERM INT`) fire before NativePty's Drop escalates to
/// SIGHUP/SIGKILL on the group.
///
/// `forkpty` makes the PTY child its own session and process-group
/// leader, so the child PID equals its PGID; we signal `-pgid` to
/// reach every descendant. Best-effort: ESRCH (child already gone) and
/// other errors are silently ignored — this is a courtesy forward, not
/// a correctness gate. See ka-0000005033.
fn forward_signal_to_pty_pgroup(child_pid: Option<i32>, sig: nix::sys::signal::Signal) {
    let Some(pid) = child_pid else { return };
    if pid <= 0 {
        return;
    }
    let pgid = nix::unistd::Pid::from_raw(-pid);
    match nix::sys::signal::kill(pgid, sig) {
        Ok(()) => info!("forwarded {sig:?} to PTY process group {pid}"),
        Err(nix::errno::Errno::ESRCH) => {
            // Child already gone — nothing to do.
        }
        Err(e) => {
            tracing::warn!("failed to forward {sig:?} to PTY pgroup {pid}: {e}");
        }
    }
}

/// Whether force-clean is active for this session. When true, `prepare()` skips
/// the S3 transcript restore AND clears any local `--resume` hint, so the session
/// starts fresh regardless of prior state. This is the single predicate both
/// suppression sites consult, so the two can never drift. Split out as a pure
/// function purely so it is trivially unit-testable (`force_clean_*` in
/// run_tests.rs) without spinning up a full `prepare()`.
fn force_clean_active(s3_no_restore: bool) -> bool {
    s3_no_restore
}

/// Append extra args to a command vector, handling `sh -c` wrapping.
///
/// When the command is `["sh", "-c", "<script>"]` (or any shell with `-c`),
/// naively appending args would make them positional parameters to the shell
/// (`$0`, `$1`, …) instead of arguments to the program inside the script.
/// This function detects that pattern and appends the args to the script
/// string with proper shell quoting.
fn inject_extra_args(command: &mut Vec<String>, extra: &[String]) {
    if extra.is_empty() {
        return;
    }

    // Detect "<shell> -c <script>" pattern (exactly 3 elements, [1] == "-c").
    if command.len() == 3 && command[1] == "-c" {
        let script = &mut command[2];
        for arg in extra {
            script.push(' ');
            shell_quote_into(script, arg);
        }
    } else {
        command.extend_from_slice(extra);
    }
}

/// Append a shell-quoted version of `arg` to `out`.
///
/// Uses single quotes, which protect all characters except single-quote
/// itself. Single quotes within the arg are escaped as `'\''`.
fn shell_quote_into(out: &mut String, arg: &str) {
    // If the arg is clean (no whitespace, no shell metacharacters), emit raw.
    let needs_quoting = arg.is_empty()
        || arg.bytes().any(|b| b.is_ascii_whitespace() || b"\"'\\$`!#&|;(){}[]<>?*~".contains(&b));
    if needs_quoting {
        out.push('\'');
        for ch in arg.chars() {
            if ch == '\'' {
                out.push_str("'\\''");
            } else {
                out.push(ch);
            }
        }
        out.push('\'');
    } else {
        out.push_str(arg);
    }
}

/// Place an S3-restored transcript at the path Claude reads on `--resume <uuid>`.
///
/// Claude looks for a resumed conversation under
/// `<config>/projects/<project>/<uuid>.jsonl`. The S3 restore downloads the
/// transcript to coop's own sessions dir, so without this copy `--resume <uuid>`
/// fails with "No conversation found with session ID: <uuid>" and the agent
/// enters a fast-exit restart loop (ka-0000007228).
///
/// Creates the parent `projects/<project>/` directory as needed. If a transcript
/// already exists at `claude_log` (e.g. a partial PVC carries fresher state), it
/// is left in place. Returns `Ok(true)` when a usable transcript is present at
/// `claude_log` afterwards, `Ok(false)` when the copy was skipped for a
/// recoverable reason, and `Err` on an IO failure the caller should log.
#[cfg(feature = "s3")]
async fn place_restored_transcript(src: &Path, claude_log: &Path) -> anyhow::Result<bool> {
    if claude_log.exists() {
        info!(
            dest = %claude_log.display(),
            "s3: transcript already present at Claude path, leaving as-is"
        );
        return Ok(true);
    }
    if let Some(parent) = claude_log.parent() {
        tokio::fs::create_dir_all(parent).await?;
    }
    tokio::fs::copy(src, claude_log).await?;
    info!(
        src = %src.display(),
        dest = %claude_log.display(),
        "s3: placed restored transcript at Claude read path for --resume"
    );
    Ok(true)
}

#[cfg(test)]
#[path = "run_tests.rs"]
mod tests;
