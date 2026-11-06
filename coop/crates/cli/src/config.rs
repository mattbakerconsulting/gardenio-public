// SPDX-License-Identifier: BUSL-1.1
// Copyright (c) 2026 Matt Baker <fimbaz@gmail.com>

use std::path::{Path, PathBuf};
use std::time::Duration;

use clap::Parser;
use serde::{Deserialize, Serialize};

use crate::driver::AgentType;
use crate::start::StartConfig;
use crate::stop::StopConfig;

/// Controls how much coop auto-responds to agent prompts during startup.
///
/// - `Auto`: auto-dismiss "disruption" prompts (setup dialogs, workspace trust)
///   so the agent reaches idle ASAP.
/// - `Manual`: detection works and API exposes prompts, but nothing is
///   auto-dismissed (today's behavior).
/// - `Pristine`: no hook/FIFO injection; passive monitoring only (Tier 2 log + Tier 5 screen).
#[derive(Debug, Default, Clone, Copy, PartialEq, Eq, Serialize, Deserialize)]
#[serde(rename_all = "lowercase")]
pub enum GroomLevel {
    #[default]
    Auto,
    Manual,
    Pristine,
}

impl std::fmt::Display for GroomLevel {
    fn fmt(&self, f: &mut std::fmt::Formatter<'_>) -> std::fmt::Result {
        match self {
            Self::Auto => f.write_str("auto"),
            Self::Manual => f.write_str("manual"),
            Self::Pristine => f.write_str("pristine"),
        }
    }
}

impl std::str::FromStr for GroomLevel {
    type Err = anyhow::Error;

    fn from_str(s: &str) -> Result<Self, Self::Err> {
        match s.to_lowercase().as_str() {
            "auto" => Ok(Self::Auto),
            "manual" => Ok(Self::Manual),
            "pristine" => Ok(Self::Pristine),
            other => anyhow::bail!("invalid groom level: {other}"),
        }
    }
}

/// Terminal session manager for AI coding agents.
#[derive(Debug, Parser)]
#[command(name = "coop", version, about)]
pub struct Config {
    /// Host address to bind to.
    #[arg(long, env = "COOP_HOST", default_value = "127.0.0.1")]
    pub host: String,

    /// HTTP port to listen on.
    #[arg(long, env = "COOP_PORT")]
    pub port: Option<u16>,

    /// gRPC port to listen on.
    #[arg(long, env = "COOP_GRPC_PORT")]
    pub port_grpc: Option<u16>,

    /// Health-check-only HTTP port.
    #[arg(long, env = "COOP_HEALTH_PORT")]
    pub port_health: Option<u16>,

    /// Dedicated Prometheus `/metrics` HTTP port.
    ///
    /// Serves an unauthenticated `/metrics`-only router so Prometheus can
    /// scrape session usage counters and latency histograms without the bearer
    /// token required on the main API port. In gardenio pods this is `:9093`
    /// (matching the `prometheus.io/port` scrape annotation).
    #[arg(long, env = "COOP_METRICS_PORT")]
    pub port_metrics: Option<u16>,

    /// Unix socket path for HTTP.
    #[arg(long, env = "COOP_SOCKET")]
    pub socket: Option<String>,

    /// Bearer token for API authentication.
    #[arg(long, env = "COOP_AUTH_TOKEN")]
    pub auth_token: Option<String>,

    /// Agent type (claude, codex, gemini, unknown). Auto-detected from command if omitted.
    #[arg(long, env = "COOP_AGENT")]
    pub agent: Option<String>,

    /// Path to agent-specific config file.
    #[arg(long, env = "COOP_AGENT_CONFIG")]
    pub agent_config: Option<PathBuf>,

    /// Attach to an existing session (e.g. tmux:session-name).
    #[arg(long, env = "COOP_ATTACH")]
    pub attach: Option<String>,

    /// Terminal columns.
    #[arg(long, env = "COOP_COLS", default_value = "200")]
    pub cols: u16,

    /// Terminal rows.
    #[arg(long, env = "COOP_ROWS", default_value = "50")]
    pub rows: u16,

    /// Ring buffer size in bytes.
    #[arg(long, env = "COOP_RING_SIZE", default_value = "1048576")]
    pub ring_size: usize,

    /// TERM environment variable for the child process.
    #[arg(long, env = "TERM", default_value = "xterm-256color")]
    pub term: String,

    /// Log format (json or text).
    #[arg(long, env = "COOP_LOG_FORMAT", default_value = "json")]
    pub log_format: String,

    /// Log level (trace, debug, info, warn, error).
    #[arg(long, env = "COOP_LOG_LEVEL", default_value = "info")]
    pub log_level: String,

    /// Resume a previous session. For claude: a .jsonl log path, a workspace
    /// path (e.g. /Users/me/myapp), or a project directory name. For codex: a
    /// Codex session (thread) id.
    #[arg(long, env = "COOP_RESUME", value_name = "HINT")]
    pub resume: Option<String>,

    /// Agent command to run.
    #[arg(trailing_var_arg = true, allow_hyphen_values = true, value_name = "AGENT")]
    pub command: Vec<String>,

    /// Enable session recording from start.
    //
    // NOTE (and the pattern every env-backed bool in this struct follows): a
    // clap-derive `bool` is an `ArgAction::SetTrue` flag, and when its value
    // arrives through `env` clap parses it with the STRICT bool value parser,
    // which accepts ONLY the literal strings `true` / `false`. Anything else is
    // a usage error — and a usage error is fatal: `main` exits 2 before coop
    // starts. That is not theoretical: the gardenio controller wired
    // `COOP_S3_NO_RESTORE=1`, coop refused to boot on all 10 restarts, and the
    // supervisor gave up with `max_restarts_reached` — killing the very agent
    // the force-clean policy exists to RECOVER (gardenio ka-0000001975).
    //
    // `BoolishValueParser` accepts the shell-idiomatic spellings (1/0,
    // true/false, yes/no, on/off, any case); `num_args(0..=1)` +
    // `default_missing_value` keeps the bare `--flag` form working. Use this
    // bundle for any bool readable from the environment.
    #[arg(
        long,
        env = "COOP_RECORD",
        action = clap::ArgAction::Set,
        num_args = 0..=1,
        default_value_t = false,
        default_missing_value = "true",
        value_parser = clap::builder::BoolishValueParser::new(),
    )]
    pub record: bool,

    /// NATS server URL (e.g. nats://localhost:4222). Enables NATS publishing when set.
    #[arg(long, env = "COOP_NATS_URL")]
    pub nats_url: Option<String>,

    /// NATS subject prefix for published events (state, prompt, hook, usage,
    /// messages.*, account.exhausted). These are the general session-event and
    /// message-forwarding subjects the beads-viewer / controller subscribe to
    /// (`coop.events.>`).
    #[arg(long, env = "COOP_NATS_PREFIX", default_value = "coop.events")]
    pub nats_prefix: String,

    /// NATS subject prefix for the coopmux discovery relay
    /// (`{relay_prefix}.session.{id}.*`). Kept DISTINCT from `--nats-prefix` so
    /// the relay's session traffic does not collide with — or get published
    /// onto — the general `coop.events.>` event space (ka-0000005381). Must
    /// match coopmux's `--nats-relay-prefix` (default "coop.mux") and the NATS
    /// authorization rule that scopes agents to `coop.mux.session.>`.
    #[arg(long, env = "COOP_NATS_RELAY_PREFIX", default_value = "coop.mux")]
    pub nats_relay_prefix: String,

    /// NATS auth token.
    ///
    /// Prefer `--nats-token-file` (env `COOP_NATS_TOKEN_FILE`) for in-cluster
    /// deployments — that path is backed by a tmpfs Secret volume so the
    /// token does not appear in `/proc/self/environ` or `kubectl describe
    /// pod` (W5, ka-0000006585). When both are set, the file wins.
    #[arg(long, env = "COOP_NATS_TOKEN")]
    pub nats_token: Option<String>,

    /// Path to a file containing the NATS auth token. Read at startup; the
    /// file contents (trimmed of trailing whitespace) replace `--nats-token`
    /// when set. Used in K8s agent pods so the token lives on a tmpfs Secret
    /// mount instead of an env-var (W5, ka-0000006585).
    #[arg(long, env = "COOP_NATS_TOKEN_FILE")]
    pub nats_token_file: Option<std::path::PathBuf>,

    /// NATS username (used with --nats-password).
    #[arg(long, env = "COOP_NATS_USER")]
    pub nats_user: Option<String>,

    /// NATS password (used with --nats-user).
    #[arg(long, env = "COOP_NATS_PASSWORD")]
    pub nats_password: Option<String>,

    /// Path to NATS credentials file (.creds) for JWT/NKey auth.
    #[arg(long, env = "COOP_NATS_CREDS")]
    pub nats_creds: Option<std::path::PathBuf>,

    /// Enable NATS relay publishing for session-scoped events (announce, status, state).
    /// When set (any non-empty value), coop publishes to `{nats_relay_prefix}.session.{id}.*`
    /// subjects so coopmux can auto-discover local sessions via NATS.
    #[arg(long, env = "COOP_NATS_RELAY")]
    pub nats_relay: Option<String>,

    /// Enable NATS message forwarding for session log data (text generation, thinking, usage).
    /// When set (any non-empty value), coop subscribes to raw_message_tx and publishes
    /// classified events to `{nats_prefix}.messages.*` subjects.
    #[arg(long, env = "COOP_NATS_FORWARD")]
    pub nats_forward: Option<String>,

    /// Agent name for inbox subscription (e.g., "mayor"). Uses GT_ROLE env var if not set.
    /// Enables inbox JetStream consumer when both this and --nats-url are set.
    #[arg(long, env = "COOP_INBOX_AGENT")]
    pub inbox_agent: Option<String>,

    /// Rig name for inbox rig-scoped messages.
    #[arg(long, env = "COOP_INBOX_RIG")]
    pub inbox_rig: Option<String>,

    /// Path to inject-queue directory for inbox JSONL delivery.
    #[arg(long, env = "COOP_INJECT_DIR")]
    pub inject_dir: Option<PathBuf>,

    /// Groom level: auto, manual, pristine.
    #[arg(long, env = "COOP_GROOM", default_value = "auto")]
    pub groom: String,

    /// Serve web assets from disk instead of embedded (for live reload during dev).
    #[cfg(debug_assertions)]
    #[arg(
        long,
        hide = true,
        env = "COOP_HOT",
        action = clap::ArgAction::Set,
        num_args = 0..=1,
        default_value_t = false,
        default_missing_value = "true",
        value_parser = clap::builder::BoolishValueParser::new(),
    )]
    pub hot: bool,

    /// Metadata labels (key=value, dots create nesting: a.b=v → {"a":{"b":"v"}}).
    #[arg(long = "label", value_name = "KEY=VALUE")]
    pub label: Vec<String>,

    /// Profile rotation mode: auto or manual.
    #[arg(long, env = "COOP_PROFILE", default_value = "auto")]
    pub profile: String,

    /// Directory the controller writes the thaw-epoch marker into
    /// (`thaw-epoch.json`). When set, coop runs a `ThawWatcher` that, on a
    /// thaw, tears down and reconnects all four NATS transports with a freshly
    /// re-read token and re-announces presence (rehydration-design Stage 3–4,
    /// ka-0000001901). Unset (the common, non-gVisor case) disables the watcher.
    #[arg(long, env = "COOP_THAW_STATE_DIR")]
    pub thaw_state_dir: Option<std::path::PathBuf>,

    /// S3 bucket for session data persistence. Enables S3 uploads when set.
    #[arg(long, env = "COOP_S3_BUCKET")]
    pub s3_bucket: Option<String>,

    /// S3 key prefix for session artifacts.
    #[arg(long, env = "COOP_S3_PREFIX", default_value = "coop/sessions")]
    pub s3_prefix: String,

    /// S3 region override (uses default AWS region chain if unset).
    #[arg(long, env = "COOP_S3_REGION")]
    pub s3_region: Option<String>,

    /// S3 endpoint URL override (e.g. an in-box MinIO at
    /// `http://minio.gardenio.svc:9000`). When set, coop targets this endpoint
    /// instead of AWS S3 and enables path-style addressing (`force_path_style`),
    /// which the Rust AWS SDK can only enable in code — not via env/config
    /// (aws-sdk-rust #1230, #390). Unset → default AWS S3, virtual-hosted style.
    #[arg(long, env = "COOP_S3_ENDPOINT")]
    pub s3_endpoint: Option<String>,

    /// Path to a file containing the object-store access key ID. Endpoint
    /// mode only (in-box MinIO has no IRSA) — projected onto agent pods as a
    /// tmpfs-backed Secret volume, mirroring `--nats-token-file`, so the key
    /// never lands in `/proc/self/environ` or `kubectl describe pod`
    /// (gd-W8OwRoHLLn). Unset in AWS/IRSA mode, where the default AWS
    /// credential chain supplies credentials instead.
    #[arg(long, env = "COOP_S3_ACCESS_KEY_ID_FILE")]
    pub s3_access_key_id_file: Option<std::path::PathBuf>,

    /// Path to a file containing the object-store secret access key. Read
    /// alongside `--s3-access-key-id-file`; see that field for the rationale
    /// (gd-W8OwRoHLLn).
    #[arg(long, env = "COOP_S3_SECRET_ACCESS_KEY_FILE")]
    pub s3_secret_access_key_file: Option<std::path::PathBuf>,

    /// Session ID to restore from S3 on startup (downloads transcripts + session log).
    #[arg(long, env = "COOP_S3_RESUME_SESSION")]
    pub s3_resume_session: Option<String>,

    /// Explicit S3 session key (prefix component) for upload + restore. When set,
    /// overrides the coop-generated session UUID so that artifacts persist under
    /// a stable, agent-scoped prefix (e.g. an agent bead ID). Falls back to
    /// `--s3-resume-session`, then to the coop session UUID.
    #[arg(long, env = "COOP_S3_SESSION_KEY")]
    pub s3_session_key: Option<String>,

    /// Force a clean (fresh) session: skip the S3 transcript RESTORE at boot even
    /// when a session key / resume session is set, so no prior `session.jsonl` is
    /// handed to `--resume`. Uploads still key under the stable session prefix, so
    /// audit + the transcript chain continue — only the boot-time rehydration is
    /// suppressed. The controller (`gb init`) sets this when the prior life crossed
    /// a turn/token/wake threshold, past which resuming a huge JSONL fails at boot
    /// (silent death); a clean start is the recovery. See gardenio ka-0000007242.
    //
    // Boolish env parsing — see the note on `record` above (ka-0000001975).
    #[arg(
        long,
        env = "COOP_S3_NO_RESTORE",
        action = clap::ArgAction::Set,
        num_args = 0..=1,
        default_value_t = false,
        default_missing_value = "true",
        value_parser = clap::builder::BoolishValueParser::new(),
    )]
    pub s3_no_restore: bool,

    // -- Knobs (set via env var only, sane testing defaults in Config::test()) --------
    /// Mux registration URL (default http://127.0.0.1:9800)
    #[clap(skip)]
    pub mux_url: Option<String>,
    /// Drain timeout in ms (0 = disabled, immediate kill on shutdown).
    #[clap(skip)]
    pub drain_timeout_ms: Option<u64>,
    #[clap(skip)]
    pub shutdown_timeout_ms: Option<u64>,
    #[clap(skip)]
    pub screen_debounce_ms: Option<u64>,
    #[clap(skip)]
    pub process_poll_ms: Option<u64>,
    #[clap(skip)]
    pub screen_poll_ms: Option<u64>,
    #[clap(skip)]
    pub log_poll_ms: Option<u64>,
    #[clap(skip)]
    pub tmux_poll_ms: Option<u64>,
    #[clap(skip)]
    pub reap_poll_ms: Option<u64>,
    #[clap(skip)]
    pub input_delay_ms: Option<u64>,
    #[clap(skip)]
    pub input_delay_per_byte_ms: Option<u64>,
    #[clap(skip)]
    pub nudge_timeout_ms: Option<u64>,
    #[clap(skip)]
    pub nudge_enter_max_retries: Option<u32>,
    #[clap(skip)]
    pub idle_timeout_ms: Option<u64>,
    #[clap(skip)]
    pub switch_timeout_ms: Option<u64>,
    #[clap(skip)]
    pub groom_dismiss_delay_ms: Option<u64>,
    #[clap(skip)]
    pub groom_reclassify_poll_ms: Option<u64>,
    #[clap(skip)]
    pub s3_upload_interval_ms: Option<u64>,
}

fn env_duration_ms(var: &str, default: u64) -> Duration {
    let ms = std::env::var(var).ok().and_then(|v| v.parse().ok()).unwrap_or(default);
    Duration::from_millis(ms)
}

macro_rules! duration_field {
    ($method:ident, $field:ident, $env:literal, $default:expr) => {
        pub fn $method(&self) -> Duration {
            match self.$field {
                Some(ms) => Duration::from_millis(ms),
                None => env_duration_ms($env, $default),
            }
        }
    };
}

impl Config {
    /// Validate the configuration after parsing.
    pub fn validate(&self) -> anyhow::Result<()> {
        // Must have at least one transport
        if self.port.is_none() && self.socket.is_none() {
            anyhow::bail!("either --port or --socket must be specified");
        }

        // Validate socket path length (sockaddr_un.sun_path limits).
        if let Some(ref socket) = self.socket {
            #[cfg(target_os = "macos")]
            const MAX_SOCKET_PATH: usize = 104;
            #[cfg(not(target_os = "macos"))]
            const MAX_SOCKET_PATH: usize = 108;
            if socket.len() >= MAX_SOCKET_PATH {
                anyhow::bail!(
                    "socket path ({} bytes) must be shorter than {} bytes",
                    socket.len(),
                    MAX_SOCKET_PATH
                );
            }
        }

        // Must have either command or attach (not both, not neither)
        let has_command = !self.command.is_empty();
        let has_attach = self.attach.is_some();

        if has_command && has_attach {
            anyhow::bail!("cannot specify both a command and --attach");
        }
        if !has_command && !has_attach {
            anyhow::bail!("an agent command is required (e.g. coop --port 8080 claude)");
        }

        // Validate agent type
        self.agent_enum()?;

        // Validate groom level
        let groom = self.groom_level()?;

        // --resume is only valid with --agent claude (a session log),
        // --agent codex (a Codex session id) or --agent gemini in ACP mode (a
        // Gemini session id, `session/load`), and cannot combine with --attach
        if self.resume.is_some() {
            let agent = self.agent_enum()?;
            if !matches!(agent, AgentType::Claude | AgentType::Codex | AgentType::Gemini) {
                anyhow::bail!("--resume is only supported with --agent claude, codex or gemini");
            }
            if agent == AgentType::Gemini
                && crate::driver::gemini::GeminiMode::from_command(&self.command)
                    != crate::driver::gemini::GeminiMode::Acp
            {
                anyhow::bail!("--resume with --agent gemini needs `gemini --acp`");
            }
            if self.attach.is_some() {
                anyhow::bail!("--resume cannot be combined with --attach");
            }
            if groom == GroomLevel::Pristine {
                anyhow::bail!("--resume cannot be combined with groom=pristine");
            }
        }

        Ok(())
    }

    /// Resolve the NATS auth token.
    ///
    /// When `--nats-token-file` (env `COOP_NATS_TOKEN_FILE`) is set, read the
    /// file contents (trimmed of trailing whitespace) and return those — that
    /// path is backed by a tmpfs Secret volume in K8s agent pods so the token
    /// never lives in `/proc/self/environ` (W5, ka-0000006585). Otherwise
    /// fall back to `--nats-token` / `COOP_NATS_TOKEN`. An empty resolved
    /// value returns `None`.
    pub fn nats_token_resolved(&self) -> Option<String> {
        if let Some(path) = self.nats_token_file.as_deref() {
            match std::fs::read_to_string(path) {
                Ok(s) => {
                    let trimmed = s.trim_end_matches(['\r', '\n', '\t', ' ']).to_owned();
                    if !trimmed.is_empty() {
                        return Some(trimmed);
                    }
                    tracing::warn!(
                        path = %path.display(),
                        "nats: token file is empty; falling back to COOP_NATS_TOKEN"
                    );
                }
                Err(e) => {
                    tracing::warn!(
                        path = %path.display(),
                        err = %e,
                        "nats: failed to read token file; falling back to COOP_NATS_TOKEN"
                    );
                }
            }
        }
        self.nats_token.clone()
    }

    // -- Tuning knobs (field override → env var → compiled default) --------

    /// Resolve the mux URL. Returns `None` when disabled (empty string),
    /// `Some(url)` when enabled. Checks field → `COOP_MUX_URL` → default.
    pub fn mux_url(&self) -> Option<String> {
        let raw = match self.mux_url {
            Some(ref v) => v.clone(),
            None => {
                std::env::var("COOP_MUX_URL").unwrap_or_else(|_| "http://127.0.0.1:9800".to_owned())
            }
        };
        if raw.is_empty() {
            None
        } else {
            Some(raw)
        }
    }

    duration_field!(shutdown_timeout, shutdown_timeout_ms, "COOP_SHUTDOWN_TIMEOUT_MS", 10_000);
    duration_field!(screen_debounce, screen_debounce_ms, "COOP_SCREEN_DEBOUNCE_MS", 50);
    duration_field!(process_poll, process_poll_ms, "COOP_PROCESS_POLL_MS", 10_000);
    duration_field!(screen_poll, screen_poll_ms, "COOP_SCREEN_POLL_MS", 3_000);
    duration_field!(log_poll, log_poll_ms, "COOP_LOG_POLL_MS", 3_000);
    duration_field!(tmux_poll, tmux_poll_ms, "COOP_TMUX_POLL_MS", 1_000);
    duration_field!(reap_poll, reap_poll_ms, "COOP_REAP_POLL_MS", 50);
    duration_field!(input_delay, input_delay_ms, "COOP_INPUT_DELAY_MS", 200);
    duration_field!(
        input_delay_per_byte,
        input_delay_per_byte_ms,
        "COOP_INPUT_DELAY_PER_BYTE_MS",
        1
    );
    duration_field!(nudge_timeout, nudge_timeout_ms, "COOP_NUDGE_TIMEOUT_MS", 2_000);

    /// Maximum number of Enter retries after nudge delivery (exponential backoff).
    pub fn nudge_enter_max_retries(&self) -> u32 {
        self.nudge_enter_max_retries.unwrap_or_else(|| {
            std::env::var("COOP_NUDGE_ENTER_MAX_RETRIES")
                .ok()
                .and_then(|v| v.parse().ok())
                .unwrap_or(10)
        })
    }

    duration_field!(idle_timeout, idle_timeout_ms, "COOP_IDLE_TIMEOUT_MS", 0);
    duration_field!(switch_timeout, switch_timeout_ms, "COOP_SWITCH_TIMEOUT_MS", 60_000);
    duration_field!(drain_timeout, drain_timeout_ms, "COOP_DRAIN_TIMEOUT_MS", 20_000);
    duration_field!(
        groom_dismiss_delay,
        groom_dismiss_delay_ms,
        "COOP_GROOM_DISMISS_DELAY_MS",
        500
    );
    duration_field!(
        groom_reclassify_poll,
        groom_reclassify_poll_ms,
        "COOP_GROOM_RECLASSIFY_POLL_MS",
        1000
    );
    duration_field!(
        s3_upload_interval,
        s3_upload_interval_ms,
        "COOP_S3_UPLOAD_INTERVAL_MS",
        60_000
    );

    /// Build a minimal `Config` for tests (port 0, `echo` command).
    #[doc(hidden)]
    pub fn test() -> Self {
        Self {
            port: Some(0),
            socket: None,
            host: "127.0.0.1".into(),
            port_grpc: None,
            auth_token: None,
            agent: None,
            agent_config: None,
            attach: None,
            cols: 80,
            rows: 24,
            ring_size: 4096,
            term: "xterm-256color".into(),
            port_health: None,
            port_metrics: None,
            log_format: "json".into(),
            log_level: "debug".into(),
            resume: None,
            record: false,
            nats_url: None,
            nats_prefix: "coop.events".into(),
            nats_relay_prefix: "coop.mux".into(),
            nats_token: None,
            nats_token_file: None,
            nats_user: None,
            nats_password: None,
            nats_creds: None,
            nats_relay: None,
            nats_forward: None,
            inbox_agent: None,
            inbox_rig: None,
            inject_dir: None,
            groom: "manual".into(),
            #[cfg(debug_assertions)]
            hot: false,
            label: Vec::new(),
            profile: "auto".into(),
            thaw_state_dir: None,
            command: vec!["echo".into()],
            mux_url: Some(String::new()), // Disable mux registration in tests
            drain_timeout_ms: Some(100),
            shutdown_timeout_ms: Some(100),
            screen_debounce_ms: Some(10),
            process_poll_ms: Some(50),
            screen_poll_ms: Some(50),
            log_poll_ms: Some(50),
            tmux_poll_ms: Some(50),
            reap_poll_ms: Some(10),
            input_delay_ms: Some(10),
            input_delay_per_byte_ms: Some(0),
            nudge_timeout_ms: Some(100),
            nudge_enter_max_retries: Some(5),
            idle_timeout_ms: Some(0),
            switch_timeout_ms: Some(500),
            groom_dismiss_delay_ms: Some(50),
            groom_reclassify_poll_ms: Some(25),
            s3_bucket: None,
            s3_prefix: "coop/sessions".into(),
            s3_region: None,
            s3_endpoint: None,
            s3_access_key_id_file: None,
            s3_secret_access_key_file: None,
            s3_resume_session: None,
            s3_session_key: None,
            s3_no_restore: false,
            s3_upload_interval_ms: Some(1_000),
        }
    }

    /// Parse the groom level string into an enum.
    pub fn groom_level(&self) -> anyhow::Result<GroomLevel> {
        self.groom.parse()
    }

    /// Parse the agent type string into an enum.
    ///
    /// When `--agent` is not set, infers the type from the basename of `command[0]`.
    pub fn agent_enum(&self) -> anyhow::Result<AgentType> {
        if let Some(ref agent) = self.agent {
            if !agent.is_empty() {
                return match agent.to_lowercase().as_str() {
                    "claude" => Ok(AgentType::Claude),
                    "codex" => Ok(AgentType::Codex),
                    "gemini" => Ok(AgentType::Gemini),
                    "unknown" => Ok(AgentType::Unknown),
                    other => anyhow::bail!("invalid agent type: {other}"),
                };
            }
        }

        // Auto-detect from command basename
        let basename = self
            .command
            .first()
            .and_then(|cmd| std::path::Path::new(cmd).file_name())
            .and_then(|name| name.to_str())
            .unwrap_or("");

        Ok(match basename {
            "claude" | "claudeless" => AgentType::Claude,
            "codex" => AgentType::Codex,
            "gemini" => AgentType::Gemini,
            _ => AgentType::Unknown,
        })
    }
}

/// Contents of the `--agent-config` JSON file.
#[derive(Debug, Clone, Default, Serialize, Deserialize)]
pub struct AgentFileConfig {
    /// Stop hook configuration. `None` means default allow behavior.
    #[serde(default, skip_serializing_if = "Option::is_none")]
    pub stop: Option<StopConfig>,
    /// Start hook configuration. `None` means no injection.
    #[serde(default, skip_serializing_if = "Option::is_none")]
    pub start: Option<StartConfig>,
    /// Agent settings (hooks, permissions, env, plugins) merged with coop's hooks.
    /// Orchestrator settings form the base layer; coop's detection hooks are appended.
    #[serde(default, skip_serializing_if = "Option::is_none")]
    pub settings: Option<serde_json::Value>,
    /// MCP server definitions (`{"server-name": {"command": ...}, ...}`).
    /// For Claude, wrapped in `{"mcpServers": ...}` and passed via `--mcp-config`.
    /// For Gemini, inserted as `mcpServers` in the settings file.
    #[serde(default, skip_serializing_if = "Option::is_none")]
    pub mcp: Option<serde_json::Value>,
}

/// Load and parse the agent config file at `path`.
///
/// Returns `AgentFileConfig` with any missing keys set to `None`.
pub fn load_agent_config(path: &Path) -> anyhow::Result<AgentFileConfig> {
    let contents = std::fs::read_to_string(path)?;
    let config: AgentFileConfig = serde_json::from_str(&contents)?;
    Ok(config)
}

/// Merge orchestrator settings with coop's generated hook config.
///
/// Rules:
/// 1. `hooks`: per hook type, concatenate arrays (orchestrator entries first, coop entries appended)
/// 2. All other top-level keys: orchestrator values pass through unchanged (coop never sets these)
///
/// Returns the merged settings as a JSON value.
pub fn merge_settings(
    orchestrator: &serde_json::Value,
    coop: serde_json::Value,
) -> serde_json::Value {
    let mut merged = orchestrator.clone();

    let Some(coop_hooks) = coop.get("hooks").and_then(|h| h.as_object()) else {
        return merged;
    };

    // Ensure merged has a hooks object
    let merged_obj = match merged.as_object_mut() {
        Some(obj) => obj,
        None => return coop,
    };
    if !merged_obj.contains_key("hooks") {
        merged_obj.insert("hooks".to_string(), serde_json::json!({}));
    }
    let merged_hooks = merged_obj.get_mut("hooks").and_then(|h| h.as_object_mut());
    let Some(merged_hooks) = merged_hooks else {
        return merged;
    };

    for (hook_type, coop_entries) in coop_hooks {
        let Some(coop_arr) = coop_entries.as_array() else {
            continue;
        };
        match merged_hooks.get_mut(hook_type) {
            Some(existing) => {
                if let Some(existing_arr) = existing.as_array_mut() {
                    existing_arr.extend(coop_arr.iter().cloned());
                }
            }
            None => {
                merged_hooks.insert(hook_type.clone(), coop_entries.clone());
            }
        }
    }

    merged
}

#[cfg(test)]
#[path = "config_tests.rs"]
mod tests;
