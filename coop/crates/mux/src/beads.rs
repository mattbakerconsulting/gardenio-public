// SPDX-License-Identifier: BUSL-1.1
// Copyright (c) 2026 Matt Baker <fimbaz@gmail.com>

//! Lightweight HTTP client for querying account and auth-config beads from
//! the beads daemon. Used by coopmux for local credential failover without
//! requiring the gardenio controller as intermediary.

use std::collections::HashMap;

use serde::{Deserialize, Serialize};

// ---------------------------------------------------------------------------
// Public types
// ---------------------------------------------------------------------------

/// Summary of a single bead returned by [`BeadsClient::get_beads_batch`].
#[derive(Debug, Clone)]
pub struct BeadSummary {
    pub id: String,
    pub title: String,
    pub fields: HashMap<String, String>,
}

/// An account bead describes a credential profile (provider, model, key, etc.).
#[derive(Debug, Clone, Serialize, Deserialize)]
pub struct AccountBead {
    /// Account name (from the bead title).
    pub name: String,
    /// Bead ID.
    pub id: String,
    /// Provider name (e.g. "anthropic", "openai").
    pub provider: String,
    /// Fallback account name when budget is exhausted.
    pub fallback: String,
    /// Token source (e.g. "coopmux:<account>", "secret:<k8s-secret>").
    pub credential: String,
    /// Default model for agents using this account.
    pub model: String,
    /// Bead status (e.g. "open", "active", "exhausted").
    pub status: String,
    /// LiteLLM virtual key for budget tracking.
    pub litellm_key: String,
    /// API endpoint override (e.g. LiteLLM proxy URL).
    pub base_url: String,
    /// Model override for health probes.
    pub probe_model: String,
    /// Anthropic rate-limit probe status: "allowed" | "rejected" | "" (unknown).
    pub throttle_status: String,
    /// 5-hour window utilization, as a string (e.g. "0.8700").
    pub utilization_5h: String,
    /// 7-day window utilization, as a string (e.g. "0.9300").
    pub utilization_7d: String,
    /// RFC3339 timestamp when the rate-limit quota resets.
    pub throttle_reset_at: String,
    /// Which window is binding: "five_hour" | "seven_day" | "".
    pub representative_claim: String,
    /// Per-account billing window length in hours (e.g. "5"). Drives the
    /// cooldown duration when the account is marked exhausted. Empty if unset.
    pub billing_period_hours: String,
    /// Raw bead labels (e.g. `["project:gardenio", "project:monorepo"]`).
    /// Used to build the project account pool — the candidate set that the
    /// authorized-account computation spreads across (mirrors the Go
    /// `selectSpreadAccount` candidate set keyed on `project:<name>`).
    pub labels: Vec<String>,
}

/// An auth-config bead maps a project to its active account, with optional
/// failover metadata.
#[derive(Debug, Clone, Serialize, Deserialize)]
pub struct AuthConfig {
    /// Bead ID.
    pub bead_id: String,
    /// Project name (extracted from labels like `project:gardenio`).
    pub project: String,
    /// Currently active account name.
    pub account: String,
    /// Original account name before failover (empty when not failed over).
    pub primary_account: String,
    /// ISO-8601 timestamp when failover occurred.
    pub failed_over_at: String,
    /// Human-readable reason for failover.
    pub failover_reason: String,
}

/// A role-account pin extracted from a `type=role` config bead's
/// `fields.account` value. Mirrors `RoleInfo.Account` on the gardenio
/// controller side (see `gardenio/controller/internal/beadsapi/roles.go`).
/// Used by coopmux at session-register time to override the project
/// default when the agent is running a role that pins a specific account.
#[derive(Debug, Clone, Serialize, Deserialize)]
pub struct RoleAccount {
    /// Role name (the role bead's `title`).
    pub role: String,
    /// Account name to use for sessions running this role.
    pub account: String,
}

// ---------------------------------------------------------------------------
// Client
// ---------------------------------------------------------------------------

/// How a [`BeadsClient`] authenticates to the daemon.
enum BeadsAuth {
    /// No `Authorization` header (dev/test daemons with auth off).
    None,
    /// The shared static bearer (`BEADS_TOKEN`).
    Static(String),
    /// coopmux's own machine principal: a Keycloak service-account token
    /// fetched per call from the source (cached, half-life refreshed).
    Machine(std::sync::Arc<crate::machine_token::MachineTokenSource>),
}

/// Daemon base URL from `BEADS_HTTP_ADDR` (default `http://localhost:8080`).
pub fn http_addr_from_env() -> String {
    std::env::var("BEADS_HTTP_ADDR").unwrap_or_else(|_| "http://localhost:8080".to_owned())
}

/// HTTP client for the beads daemon REST API.
pub struct BeadsClient {
    base_url: String,
    auth: BeadsAuth,
    http: reqwest::Client,
}

impl BeadsClient {
    /// Create a new client from explicit parameters.
    ///
    /// Returns an error if the HTTP client cannot be constructed (e.g. missing
    /// TLS crypto provider — the caller's `main()` must install one first via
    /// `rustls::crypto::ring::default_provider().install_default()`).
    pub fn new(base_url: String, token: Option<String>) -> anyhow::Result<Self> {
        let auth = match token {
            Some(t) => BeadsAuth::Static(t),
            None => BeadsAuth::None,
        };
        Self::with_auth(base_url, auth)
    }

    /// Create a client that authenticates as coopmux's machine principal
    /// (gd-Wcu0DOkHfN): every request carries a token from `source`. A token
    /// fetch failure fails the request (error text contains
    /// `auth_unavailable`); there is no fallback to the static token.
    pub fn new_machine(
        base_url: String,
        source: std::sync::Arc<crate::machine_token::MachineTokenSource>,
    ) -> anyhow::Result<Self> {
        Self::with_auth(base_url, BeadsAuth::Machine(source))
    }

    fn with_auth(base_url: String, auth: BeadsAuth) -> anyhow::Result<Self> {
        let http = reqwest::Client::builder()
            .user_agent(concat!("coopmux/", env!("CARGO_PKG_VERSION")))
            .timeout(std::time::Duration::from_secs(30))
            .build()?;
        Ok(Self { base_url: base_url.trim_end_matches('/').to_owned(), auth, http })
    }

    /// Create a client from environment variables.
    ///
    /// - `BEADS_HTTP_ADDR` — daemon base URL (default `http://localhost:8080`).
    /// - `BEADS_TOKEN` — optional bearer token for authentication.
    pub fn from_env() -> anyhow::Result<Self> {
        let token = std::env::var("BEADS_TOKEN").ok().filter(|t| !t.is_empty());
        Self::new(http_addr_from_env(), token)
    }

    /// Whether this client authenticates as the machine principal.
    pub fn is_machine(&self) -> bool {
        matches!(self.auth, BeadsAuth::Machine(_))
    }

    // -------------------------------------------------------------------
    // Public API
    // -------------------------------------------------------------------

    /// List all open account beads (`type=account&status=open`).
    pub async fn list_account_beads(&self) -> anyhow::Result<Vec<AccountBead>> {
        let resp: ListBeadsResponse = self.get("/v1/beads?type=account&status=open").await?;

        let mut accounts = Vec::new();
        for bead in resp.beads {
            let name = bead.title.clone();
            if name.is_empty() {
                continue;
            }
            let fields = bead.fields_map();
            accounts.push(AccountBead {
                name,
                id: bead.id,
                provider: fields.get("provider").cloned().unwrap_or_default(),
                fallback: fields.get("fallback").cloned().unwrap_or_default(),
                credential: fields.get("credential").cloned().unwrap_or_default(),
                model: fields.get("model").cloned().unwrap_or_default(),
                status: fields.get("status").cloned().unwrap_or_default(),
                litellm_key: fields.get("litellm_key").cloned().unwrap_or_default(),
                base_url: fields.get("base_url").cloned().unwrap_or_default(),
                probe_model: fields.get("probe_model").cloned().unwrap_or_default(),
                throttle_status: fields.get("throttle_status").cloned().unwrap_or_default(),
                utilization_5h: fields.get("utilization_5h").cloned().unwrap_or_default(),
                utilization_7d: fields.get("utilization_7d").cloned().unwrap_or_default(),
                throttle_reset_at: fields.get("throttle_reset_at").cloned().unwrap_or_default(),
                representative_claim: fields
                    .get("representative_claim")
                    .cloned()
                    .unwrap_or_default(),
                billing_period_hours: fields
                    .get("billing_period_hours")
                    .cloned()
                    .unwrap_or_default(),
                labels: bead.labels.clone(),
            });
        }
        Ok(accounts)
    }

    /// List auth-config beads (`type=config&status=open`, title="auth").
    ///
    /// Each returned [`AuthConfig`] represents a project-to-account mapping.
    /// A single bead with multiple `project:` labels produces multiple entries.
    pub async fn list_auth_configs(&self) -> anyhow::Result<Vec<AuthConfig>> {
        let resp: ListBeadsResponse = self.get("/v1/beads?type=config&status=open").await?;

        let mut configs = Vec::new();
        for bead in resp.beads {
            if bead.title != "auth" {
                continue;
            }
            let desc_str = bead.description.as_deref().unwrap_or_default();
            if desc_str.is_empty() {
                continue;
            }
            let desc: AuthConfigDesc = match serde_json::from_str(desc_str) {
                Ok(d) => d,
                Err(_) => continue,
            };
            if desc.account.is_empty() {
                continue;
            }

            // Extract project names from labels like "project:gardenio".
            let projects: Vec<&str> = bead
                .labels
                .iter()
                .filter_map(|l| l.strip_prefix("project:"))
                .filter(|p| !p.is_empty())
                .collect();

            for project in projects {
                configs.push(AuthConfig {
                    bead_id: bead.id.clone(),
                    project: project.to_owned(),
                    account: desc.account.clone(),
                    primary_account: desc.primary_account.clone(),
                    failed_over_at: desc.failed_over_at.clone(),
                    failover_reason: desc.failover_reason.clone(),
                });
            }
        }
        Ok(configs)
    }

    /// List role-account pins (`type=role&status=open`, fields.account set).
    ///
    /// Each returned [`RoleAccount`] represents a `role → account` override
    /// — sessions whose `metadata.role` matches will get this account as the
    /// preferred assignment, overriding the project's default. Roles without
    /// a `fields.account` value are skipped (no pin, fall through to project
    /// auth config).
    pub async fn list_role_accounts(&self) -> anyhow::Result<Vec<RoleAccount>> {
        let resp: ListBeadsResponse = self.get("/v1/beads?type=role&status=open").await?;

        let mut roles = Vec::new();
        for bead in resp.beads {
            if bead.title.is_empty() {
                continue;
            }
            let fields = bead.fields_map();
            let account = match fields.get("account") {
                Some(a) if !a.is_empty() => a.clone(),
                _ => continue,
            };
            roles.push(RoleAccount { role: bead.title, account });
        }
        Ok(roles)
    }

    /// Batch-fetch multiple beads by their IDs.
    ///
    /// Returns a map from bead ID to [`BeadSummary`]. Missing beads are
    /// silently omitted. Uses the dedicated `/v1/batch/beads?ids=` endpoint
    /// (capped at 50 IDs per request server-side, so larger inputs are
    /// chunked).
    pub async fn get_beads_batch(
        &self,
        ids: &[&str],
    ) -> anyhow::Result<HashMap<String, BeadSummary>> {
        if ids.is_empty() {
            return Ok(HashMap::new());
        }
        let mut map = HashMap::with_capacity(ids.len());
        for chunk in ids.chunks(50) {
            let id_param = chunk.iter().map(|id| encode_path(id)).collect::<Vec<_>>().join(",");
            let resp: BatchBeadsResponse =
                self.get(&format!("/v1/batch/beads?ids={}", id_param)).await?;
            for r in resp.results {
                if let Some(bead) = r.bead {
                    let fields = bead.fields_map();
                    map.insert(
                        bead.id.clone(),
                        BeadSummary { id: bead.id, title: bead.title, fields },
                    );
                }
            }
        }
        Ok(map)
    }

    /// Update typed fields on a bead (partial merge on the server).
    pub async fn update_bead_fields(
        &self,
        bead_id: &str,
        fields: &HashMap<String, String>,
    ) -> anyhow::Result<()> {
        let body = serde_json::json!({ "fields": fields });
        self.patch(&format!("/v1/beads/{}", encode_path(bead_id)), &body).await
    }

    // -------------------------------------------------------------------
    // Internal helpers
    // -------------------------------------------------------------------

    /// The bearer token for the next request, if any. For the machine
    /// principal this may perform a token fetch; its error propagates so the
    /// write fails closed rather than going out unauthenticated.
    async fn bearer(&self) -> anyhow::Result<Option<String>> {
        match &self.auth {
            BeadsAuth::None => Ok(None),
            BeadsAuth::Static(t) => Ok(Some(t.clone())),
            BeadsAuth::Machine(source) => source.token().await.map(Some),
        }
    }

    /// Build a request with common headers (content-type, auth).
    async fn request(
        &self,
        method: reqwest::Method,
        path: &str,
    ) -> anyhow::Result<reqwest::RequestBuilder> {
        let url = format!("{}{}", self.base_url, path);
        let mut req = self.http.request(method, &url);
        if let Some(token) = self.bearer().await? {
            req = req.header("Authorization", format!("Bearer {token}"));
        }
        Ok(req)
    }

    /// Perform a GET request and deserialize the JSON response.
    async fn get<T: serde::de::DeserializeOwned>(&self, path: &str) -> anyhow::Result<T> {
        let resp = self.request(reqwest::Method::GET, path).await?.send().await?;

        let status = resp.status();
        if status.is_client_error() || status.is_server_error() {
            let body = resp.text().await.unwrap_or_default();
            let msg = parse_error_message(&body).unwrap_or(body);
            anyhow::bail!("HTTP {status}: {msg}");
        }

        let body = resp.bytes().await?;
        let parsed = serde_json::from_slice(&body)?;
        Ok(parsed)
    }

    /// Perform a PATCH request with a JSON body; ignore the response body.
    async fn patch(&self, path: &str, body: &serde_json::Value) -> anyhow::Result<()> {
        let resp = self
            .request(reqwest::Method::PATCH, path)
            .await?
            .header("Content-Type", "application/json")
            .json(body)
            .send()
            .await?;

        let status = resp.status();
        if status.is_client_error() || status.is_server_error() {
            let text = resp.text().await.unwrap_or_default();
            let msg = parse_error_message(&text).unwrap_or(text);
            anyhow::bail!("HTTP {status}: {msg}");
        }
        Ok(())
    }
}

// ---------------------------------------------------------------------------
// Wire types (internal JSON representations)
// ---------------------------------------------------------------------------

/// Response envelope from `GET /v1/beads`.
#[derive(Debug, Deserialize)]
struct ListBeadsResponse {
    beads: Vec<BeadJson>,
}

/// Response envelope from `GET /v1/batch/beads`.
#[derive(Debug, Deserialize)]
struct BatchBeadsResponse {
    results: Vec<BatchBeadResult>,
}

/// One entry in a batch-beads response. `bead` is absent when the ID was not
/// found; `error` carries the per-ID error message in that case (we ignore
/// it — callers see the missing key in the returned map).
#[derive(Debug, Deserialize)]
struct BatchBeadResult {
    #[serde(default)]
    bead: Option<BeadJson>,
}

/// A single bead as returned by the daemon HTTP API.
#[derive(Debug, Deserialize)]
struct BeadJson {
    id: String,
    title: String,
    #[serde(default)]
    description: Option<String>,
    #[serde(default)]
    fields: Option<serde_json::Value>,
    #[serde(default)]
    labels: Vec<String>,
}

impl BeadJson {
    /// Parse the `fields` JSON object into a flat string map.
    ///
    /// String values are kept as-is; non-string values are serialized to their
    /// JSON representation (mirrors the Go `ParseFieldsJSON` helper).
    fn fields_map(&self) -> HashMap<String, String> {
        let Some(ref val) = self.fields else {
            return HashMap::new();
        };
        let Some(obj) = val.as_object() else {
            return HashMap::new();
        };
        let mut map = HashMap::with_capacity(obj.len());
        for (k, v) in obj {
            match v.as_str() {
                Some(s) => {
                    map.insert(k.clone(), s.to_owned());
                }
                None => {
                    // Re-serialize booleans, numbers, arrays, objects.
                    map.insert(k.clone(), v.to_string());
                }
            }
        }
        map
    }
}

/// JSON structure stored in config:auth bead descriptions.
#[derive(Debug, Deserialize)]
struct AuthConfigDesc {
    account: String,
    #[serde(default)]
    primary_account: String,
    #[serde(default)]
    failed_over_at: String,
    #[serde(default)]
    failover_reason: String,
}

// ---------------------------------------------------------------------------
// Utilities
// ---------------------------------------------------------------------------

/// Percent-encode a bead ID for use in URL path segments.
fn encode_path(segment: &str) -> String {
    // Encode characters that are unsafe in path segments. Bead IDs are
    // typically alphanumeric + hyphens, so this is mostly a safety net.
    let mut out = String::with_capacity(segment.len());
    for ch in segment.chars() {
        match ch {
            'A'..='Z' | 'a'..='z' | '0'..='9' | '-' | '_' | '.' | '~' => out.push(ch),
            _ => {
                for byte in ch.to_string().as_bytes() {
                    use std::fmt::Write as _;
                    let _ = write!(out, "%{byte:02X}");
                }
            }
        }
    }
    out
}

/// Try to extract an `"error"` field from a JSON error response body.
fn parse_error_message(body: &str) -> Option<String> {
    #[derive(Deserialize)]
    struct ErrResp {
        error: String,
    }
    serde_json::from_str::<ErrResp>(body).ok().map(|e| e.error)
}

// ---------------------------------------------------------------------------
// BeadsSource implementation (for AccountCache integration)
// ---------------------------------------------------------------------------

impl crate::credential::failover::BeadsSource for BeadsClient {
    async fn list_account_beads(
        &self,
    ) -> anyhow::Result<Vec<crate::credential::failover::AccountBead>> {
        let beads = self.list_account_beads().await?;
        Ok(beads
            .into_iter()
            .map(|b| crate::credential::failover::AccountBead {
                name: b.name,
                id: b.id,
                fallback: if b.fallback.is_empty() { None } else { Some(b.fallback) },
                provider: b.provider,
                status: b.status,
                credential_secret: b.credential,
                throttle_status: b.throttle_status,
                utilization_5h: b.utilization_5h,
                utilization_7d: b.utilization_7d,
                throttle_reset_at: b.throttle_reset_at,
                representative_claim: b.representative_claim,
                billing_period_hours: b.billing_period_hours,
                labels: b.labels,
            })
            .collect())
    }

    async fn list_auth_configs(
        &self,
    ) -> anyhow::Result<Vec<crate::credential::failover::AuthConfig>> {
        let configs = self.list_auth_configs().await?;
        Ok(configs
            .into_iter()
            .map(|c| crate::credential::failover::AuthConfig {
                bead_id: c.bead_id,
                account: c.account,
                primary_account: if c.primary_account.is_empty() {
                    None
                } else {
                    Some(c.primary_account)
                },
                project: c.project,
            })
            .collect())
    }

    async fn list_role_accounts(
        &self,
    ) -> anyhow::Result<Vec<crate::credential::failover::RoleAccount>> {
        let pins = self.list_role_accounts().await?;
        Ok(pins
            .into_iter()
            .map(|p| crate::credential::failover::RoleAccount { role: p.role, account: p.account })
            .collect())
    }
}

// ---------------------------------------------------------------------------
// Tests
// ---------------------------------------------------------------------------

#[cfg(test)]
#[path = "beads_tests.rs"]
mod tests;
