// SPDX-License-Identifier: BUSL-1.1
// Copyright (c) 2026 Matt Baker <fimbaz@gmail.com>

/// Configuration for the coopmux proxy.
#[derive(Debug, Clone, clap::Args)]
pub struct MuxConfig {
    /// Host to bind on.
    #[arg(long, default_value = "127.0.0.1", env = "COOP_MUX_HOST")]
    pub host: String,

    /// Port to listen on.
    #[arg(long, default_value_t = 9800, env = "COOP_MUX_PORT")]
    pub port: u16,

    /// Bearer token for downstream API auth. If unset, auth is disabled.
    #[arg(long, env = "COOP_MUX_AUTH_TOKEN")]
    pub auth_token: Option<String>,

    /// OIDC issuer URL (e.g. Keycloak realm URL). When set, OIDC JWT tokens
    /// are accepted as an alternative to the static auth token.
    #[arg(long, env = "COOP_MUX_OIDC_ISSUER_URL")]
    pub oidc_issuer_url: Option<String>,

    /// OIDC client ID (audience) for token validation.
    #[arg(long, env = "COOP_MUX_OIDC_CLIENT_ID")]
    pub oidc_client_id: Option<String>,

    /// Screen poll interval in milliseconds.
    #[arg(long, default_value_t = 1000, env = "COOP_MUX_SCREEN_POLL_MS")]
    pub screen_poll_ms: u64,

    /// Status poll interval in milliseconds.
    #[arg(long, default_value_t = 2000, env = "COOP_MUX_STATUS_POLL_MS")]
    pub status_poll_ms: u64,

    /// Health check interval in milliseconds.
    #[arg(long, default_value_t = 10000, env = "COOP_MUX_HEALTH_CHECK_MS")]
    pub health_check_ms: u64,

    /// Max consecutive health failures before evicting a session.
    #[arg(long, default_value_t = 3, env = "COOP_MUX_MAX_HEALTH_FAILURES")]
    pub max_health_failures: u32,

    /// Launch command template for spawning new sessions (shell command via `sh -c`).
    #[arg(long, env = "COOP_MUX_LAUNCH")]
    pub launch: Option<String>,

    /// Path to credential configuration JSON file.
    ///
    /// **Deprecated**: Credentials should be synced from the controller via the
    /// credentials/set API. This flag will be removed in a future release.
    #[arg(long, env = "COOP_MUX_CREDENTIAL_CONFIG")]
    pub credential_config: Option<std::path::PathBuf>,

    /// Path to a file containing the API key for the first configured account.
    /// The file contents are trimmed and injected via the credentials/set API
    /// at startup. Use this for static API key deployments (e.g. K8s Secrets).
    #[arg(long, env = "COOP_MUX_API_KEY_FILE")]
    pub api_key_file: Option<std::path::PathBuf>,

    /// Pre-warm LRU cache capacity (number of sessions to slow-poll).
    #[arg(long, default_value_t = 64, env = "COOP_MUX_PREWARM_CAPACITY")]
    pub prewarm_capacity: usize,

    /// Pre-warm poll interval in milliseconds.
    #[arg(long, default_value_t = 15000, env = "COOP_MUX_PREWARM_POLL_MS")]
    pub prewarm_poll_ms: u64,

    /// State directory for persistent data (credentials, etc.).
    #[arg(skip)]
    pub state_dir: Option<std::path::PathBuf>,

    /// Dev mode: skip fail-closed auth when credentials are present but no
    /// auth token or OIDC is configured. **Not for production use.**
    #[arg(long, env = "COOP_DEV_MODE")]
    pub dev_mode: bool,

    /// Optional URL path prefix when serving behind a unified-host IngressRoute
    /// (e.g. `/mux` so requests to `gardenio.<env>/mux/...` reach coopmux).
    ///
    /// When set, the dashboard HTML is served at both `/<base>/mux` and root
    /// `/` so it works behind both prefix-preserving and strip-prefix proxies,
    /// and `window.__COOP_BASE_PATH__` is injected into the HTML so the SPA
    /// prefixes its API/WS URLs. Requests arriving with the prefix are
    /// rewritten to root before route matching, keeping in-cluster probes
    /// (which hit root directly) working unchanged.
    ///
    /// See gardenio/plans/stage3-ava-cli-eb-rollout.md (Workstream E).
    #[arg(long, env = "BASE_PATH")]
    pub base_path: Option<String>,

    /// Serve web assets from disk instead of embedded (for live reload during dev).
    #[cfg(debug_assertions)]
    #[arg(long, hide = true, env = "COOP_HOT")]
    pub hot: bool,
}

impl MuxConfig {
    pub fn screen_poll_interval(&self) -> std::time::Duration {
        std::time::Duration::from_millis(self.screen_poll_ms)
    }

    pub fn status_poll_interval(&self) -> std::time::Duration {
        std::time::Duration::from_millis(self.status_poll_ms)
    }

    pub fn health_check_interval(&self) -> std::time::Duration {
        std::time::Duration::from_millis(self.health_check_ms)
    }

    pub fn prewarm_poll_interval(&self) -> std::time::Duration {
        std::time::Duration::from_millis(self.prewarm_poll_ms)
    }

    /// Resolve the state directory for persistent data.
    ///
    /// Uses the explicit `state_dir` if set, otherwise falls back to
    /// `COOP_MUX_STATE_DIR` env → `$XDG_STATE_HOME/coop/mux` → `$HOME/.local/state/coop/mux`.
    pub fn state_dir(&self) -> std::path::PathBuf {
        self.state_dir.clone().unwrap_or_else(crate::credential::state_dir)
    }

    /// Normalized base path: empty string for unset/root, otherwise a
    /// leading-slash, no-trailing-slash form (e.g. `/mux`).
    pub fn normalized_base_path(&self) -> String {
        crate::transport::base_path::normalize(self.base_path.as_deref().unwrap_or(""))
    }
}
