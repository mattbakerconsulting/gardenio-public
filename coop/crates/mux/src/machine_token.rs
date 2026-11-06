// SPDX-License-Identifier: BUSL-1.1
// Copyright (c) 2026 Matt Baker <fimbaz@gmail.com>

//! Machine principal (gd-Wcu0DOkHfN, design
//! `gardenio/docs/machine-principal-design.md`).
//!
//! coopmux authenticates its account-bead write (marking a throttled account
//! `status=exhausted`, `credential/failover.rs`) to the beads daemon as the
//! Keycloak service account of its own confidential client instead of the
//! shared static `BEADS_TOKEN`. kbeads classifies that token as a machine
//! principal (client registered in `BEADS_MACHINE_CLIENTS`) and admits exactly
//! the writes its `gardenio-machine-*` realm roles grant.
//!
//! This is the Rust mirror of the controller's
//! `gardenio/controller/internal/beadsapi/machine_token.go`: same environment
//! contract, same half-life refresh, same fail-closed semantics.
//!
//! Environment contract (rendered by `templates/coopmux/deployment.yaml` when
//! `coopmux.machinePrincipal.enabled`):
//!
//! ```text
//! BEADS_OIDC_TOKEN_URL           in-cluster token endpoint
//!                                (<discoveryURL>/protocol/openid-connect/token)
//! BEADS_OIDC_CLIENT_ID           the confidential client (gardenio-coopmux)
//! BEADS_OIDC_CLIENT_SECRET_FILE  file holding the client secret, mounted from
//!                                a Secret the operator creates by hand
//! BEADS_OIDC_CA_FILE             optional PEM CA the token endpoint's cert
//!                                chains to, trusted beside the system roots
//!                                (the keycloak-runtime in-cluster CA,
//!                                gd-lYPZlslOM7)
//! ```

use std::path::{Path, PathBuf};
use std::time::{Duration, Instant};

use base64::Engine as _;
use serde::Deserialize;
use zeroize::Zeroizing;

/// Env var: the OAuth 2.0 token endpoint.
pub const ENV_TOKEN_URL: &str = "BEADS_OIDC_TOKEN_URL";
/// Env var: the confidential client id.
pub const ENV_CLIENT_ID: &str = "BEADS_OIDC_CLIENT_ID";
/// Env var: path of the file holding the client secret.
pub const ENV_CLIENT_SECRET_FILE: &str = "BEADS_OIDC_CLIENT_SECRET_FILE";
/// Env var (optional): path of a PEM CA to trust beside the system roots.
pub const ENV_CA_FILE: &str = "BEADS_OIDC_CA_FILE";

/// Marker every fetch failure carries. Callers (and the coopmux log) can grep
/// for it; it never carries the secret or a token. There is deliberately NO
/// fallback to the static token: a write that should be attributed to the
/// machine principal fails closed instead of silently running as the shared
/// identity.
pub const AUTH_UNAVAILABLE: &str = "auth_unavailable";

/// Lifetime assumed when the token endpoint reports no expiry at all (no
/// `expires_in` and no `exp`/`iat` in the JWT). Keycloak always sends
/// `expires_in`, so this is a belt-and-braces floor.
const DEFAULT_LIFETIME: Duration = Duration::from_secs(5 * 60);

/// HTTP timeout for one token exchange.
const FETCH_TIMEOUT: Duration = Duration::from_secs(30);

/// Error returned when only some of the three `BEADS_OIDC_*` variables are
/// set. A half-configured principal is a misconfiguration, not "disabled".
pub const ENV_PARTIAL_ERROR: &str = "machine principal: BEADS_OIDC_TOKEN_URL, BEADS_OIDC_CLIENT_ID and BEADS_OIDC_CLIENT_SECRET_FILE must all be set, or none";

type Clock = Box<dyn Fn() -> Instant + Send + Sync>;

/// Caches one client-credentials token and refreshes it at half-life.
///
/// Safe for concurrent use: callers that find the cache stale funnel through
/// one fetch. The secret file is re-read on every fetch, so a rotated Secret
/// (kubelet refreshes a mounted Secret volume in place) is picked up without a
/// restart. It never logs the token or the secret.
pub struct MachineTokenSource {
    http: reqwest::Client,
    token_url: String,
    client_id: String,
    secret_file: PathBuf,
    /// Optional PEM CA (`BEADS_OIDC_CA_FILE`), re-read on every fetch.
    ca_file: Option<PathBuf>,
    /// The client built from `ca_file`'s contents, rebuilt only when they
    /// change so fetches reuse one connection pool.
    ca_http: std::sync::Mutex<Option<(Vec<u8>, reqwest::Client)>>,
    /// The clock; tests pin it to drive the half-life refresh.
    clock: Clock,
    state: tokio::sync::Mutex<Cached>,
}

#[derive(Default)]
struct Cached {
    token: Option<String>,
    /// When the cached token should be refreshed (issued + lifetime/2).
    refresh_at: Option<Instant>,
    /// When the cached token stops being served at all.
    expires_at: Option<Instant>,
    logged_once: bool,
}

impl std::fmt::Debug for MachineTokenSource {
    fn fmt(&self, f: &mut std::fmt::Formatter<'_>) -> std::fmt::Result {
        f.debug_struct("MachineTokenSource")
            .field("token_url", &self.token_url)
            .field("client_id", &self.client_id)
            .field("secret_file", &self.secret_file)
            .field("ca_file", &self.ca_file)
            .finish_non_exhaustive()
    }
}

impl MachineTokenSource {
    /// Build a source from `BEADS_OIDC_TOKEN_URL`, `BEADS_OIDC_CLIENT_ID` and
    /// `BEADS_OIDC_CLIENT_SECRET_FILE`.
    ///
    /// Returns `Ok(Some)` when all three are set, `Ok(None)` when none are set
    /// (coopmux behaves as today, on the static token) and an error when only
    /// some are set. `BEADS_OIDC_CA_FILE`, when set, is applied with
    /// [`Self::with_ca_file`].
    pub fn from_env() -> anyhow::Result<Option<Self>> {
        let read = |k: &str| std::env::var(k).ok().map(|v| v.trim().to_owned()).unwrap_or_default();
        let token_url = read(ENV_TOKEN_URL);
        let client_id = read(ENV_CLIENT_ID);
        let secret_file = read(ENV_CLIENT_SECRET_FILE);
        let ca_file = read(ENV_CA_FILE);
        let set = [&token_url, &client_id, &secret_file].iter().filter(|v| !v.is_empty()).count();
        match set {
            0 => Ok(None),
            3 => {
                let src = Self::new(token_url, client_id, PathBuf::from(secret_file))?;
                Ok(Some(if ca_file.is_empty() { src } else { src.with_ca_file(ca_file) }))
            }
            _ => anyhow::bail!(ENV_PARTIAL_ERROR),
        }
    }

    /// Build a source from explicit values. Fails only if the HTTP client
    /// cannot be constructed (the TLS crypto provider is installed by
    /// `main()`).
    pub fn new(
        token_url: String,
        client_id: String,
        secret_file: impl AsRef<Path>,
    ) -> anyhow::Result<Self> {
        let http = Self::http_builder().build()?;
        Ok(Self {
            http,
            token_url,
            client_id,
            secret_file: secret_file.as_ref().to_path_buf(),
            ca_file: None,
            ca_http: std::sync::Mutex::new(None),
            clock: Box::new(Instant::now),
            state: tokio::sync::Mutex::new(Cached::default()),
        })
    }

    /// Trust the PEM CA in `path`, beside the system roots, when dialing the
    /// token endpoint (the keycloak-runtime in-cluster CA, gd-lYPZlslOM7). The
    /// file is re-read on every fetch, like the secret, so a pod that starts
    /// before cert-manager has issued (the CA volume is optional) fails closed
    /// until kubelet projects it and then recovers without a restart.
    pub fn with_ca_file(mut self, path: impl AsRef<Path>) -> Self {
        self.ca_file = Some(path.as_ref().to_path_buf());
        self
    }

    fn http_builder() -> reqwest::ClientBuilder {
        reqwest::Client::builder()
            .user_agent(concat!("coopmux/", env!("CARGO_PKG_VERSION")))
            .timeout(FETCH_TIMEOUT)
    }

    /// The client for one fetch: the plain one, or with `ca_file` set one that
    /// also trusts that CA.
    fn http_client(&self) -> anyhow::Result<reqwest::Client> {
        let Some(ca_file) = &self.ca_file else {
            return Ok(self.http.clone());
        };
        let pem = std::fs::read(ca_file).map_err(|e| {
            anyhow::anyhow!(
                "{AUTH_UNAVAILABLE}: machine token CA for {} unreadable: {e}",
                self.client_id
            )
        })?;
        let mut cache = self.ca_http.lock().unwrap_or_else(std::sync::PoisonError::into_inner);
        if let Some((cached, client)) = cache.as_ref() {
            if *cached == pem {
                return Ok(client.clone());
            }
        }
        let certs = reqwest::Certificate::from_pem_bundle(&pem).unwrap_or_default();
        if certs.is_empty() {
            anyhow::bail!(
                "{AUTH_UNAVAILABLE}: machine token CA file {} for {} holds no PEM certificate",
                ca_file.display(),
                self.client_id
            );
        }
        let client = Self::http_builder().tls_certs_merge(certs).build().map_err(|e| {
            anyhow::anyhow!(
                "{AUTH_UNAVAILABLE}: machine token CA for {} unusable: {e}",
                self.client_id
            )
        })?;
        *cache = Some((pem, client.clone()));
        Ok(client)
    }

    /// Replace the clock (tests only: drives the half-life refresh).
    #[cfg(test)]
    pub(crate) fn with_clock(mut self, clock: Clock) -> Self {
        self.clock = clock;
        self
    }

    /// The confidential client this source authenticates as.
    pub fn client_id(&self) -> &str {
        &self.client_id
    }

    /// Return a valid access token, fetching or refreshing as needed.
    ///
    /// A fetch failure returns an error whose text contains
    /// [`AUTH_UNAVAILABLE`]. A stale-but-unexpired cached token is still
    /// served when a refresh fails (the IdP blipped); past expiry the source
    /// fails closed. Never falls back to a static token.
    pub async fn token(&self) -> anyhow::Result<String> {
        let mut st = self.state.lock().await;
        let now = (self.clock)();
        if let (Some(tok), Some(refresh_at)) = (&st.token, st.refresh_at) {
            if now < refresh_at {
                return Ok(tok.clone());
            }
        }
        match self.fetch().await {
            Ok(fetched) => {
                st.token = Some(fetched.token.clone());
                st.expires_at = Some(now + fetched.lifetime);
                st.refresh_at = Some(now + fetched.lifetime / 2);
                if !st.logged_once {
                    st.logged_once = true;
                    let c = &fetched.claims;
                    tracing::info!(
                        client = %self.client_id,
                        azp = %c.azp,
                        preferred_username = %c.preferred_username,
                        aud = ?c.audience(),
                        realm_roles = ?c.roles(),
                        exp_minus_iat_seconds = c.exp_minus_iat(),
                        lifetime_seconds = fetched.lifetime.as_secs(),
                        "machine token acquired"
                    );
                }
                Ok(fetched.token)
            }
            Err(e) => {
                if let (Some(tok), Some(expires_at)) = (&st.token, st.expires_at) {
                    if now < expires_at {
                        tracing::warn!(
                            client = %self.client_id,
                            err = %e,
                            "machine token refresh failed; serving cached token until expiry"
                        );
                        return Ok(tok.clone());
                    }
                }
                Err(e)
            }
        }
    }

    /// One client-credentials exchange. The caller holds the state lock.
    async fn fetch(&self) -> anyhow::Result<Fetched> {
        let secret = self.read_secret()?;
        let http = self.http_client()?;
        let resp = http
            .post(&self.token_url)
            .basic_auth(&self.client_id, Some(secret.as_str()))
            .form(&[("grant_type", "client_credentials")])
            .send()
            .await
            .map_err(|e| {
                anyhow::anyhow!(
                    "{AUTH_UNAVAILABLE}: machine token fetch for client {} failed: {e}",
                    self.client_id
                )
            })?;
        let status = resp.status();
        let body = resp.bytes().await.map_err(|e| {
            anyhow::anyhow!(
                "{AUTH_UNAVAILABLE}: machine token fetch for client {} failed reading the response: {e}",
                self.client_id
            )
        })?;
        if !status.is_success() {
            // An error body never carries a token; a short excerpt helps the
            // operator tell "unknown client" from "invalid secret".
            let excerpt: String = String::from_utf8_lossy(&body).chars().take(200).collect();
            anyhow::bail!(
                "{AUTH_UNAVAILABLE}: machine token fetch for client {} failed: HTTP {status}: {excerpt}",
                self.client_id
            );
        }
        let tr: TokenResponse = serde_json::from_slice(&body).map_err(|e| {
            anyhow::anyhow!(
                "{AUTH_UNAVAILABLE}: machine token fetch for client {} returned an unparseable body: {e}",
                self.client_id
            )
        })?;
        if tr.access_token.is_empty() {
            anyhow::bail!(
                "{AUTH_UNAVAILABLE}: machine token fetch for client {} returned an empty access token",
                self.client_id
            );
        }
        let claims = parse_jwt_claims(&tr.access_token);
        let mut lifetime = tr.expires_in().filter(|d| !d.is_zero());
        if lifetime.is_none() {
            lifetime = claims.lifetime();
        }
        let lifetime = lifetime.unwrap_or(DEFAULT_LIFETIME);
        Ok(Fetched { token: tr.access_token, lifetime, claims })
    }

    /// Re-read the client secret from disk on every call so a rotated Secret
    /// takes effect at the next refresh without a restart.
    fn read_secret(&self) -> anyhow::Result<Zeroizing<String>> {
        let raw = std::fs::read_to_string(&self.secret_file).map_err(|e| {
            anyhow::anyhow!(
                "{AUTH_UNAVAILABLE}: machine client secret for {} unreadable: {e}",
                self.client_id
            )
        })?;
        let secret = Zeroizing::new(raw.trim().to_owned());
        drop(Zeroizing::new(raw));
        if secret.is_empty() {
            anyhow::bail!(
                "{AUTH_UNAVAILABLE}: machine client secret file for {} is empty",
                self.client_id
            );
        }
        Ok(secret)
    }
}

struct Fetched {
    token: String,
    lifetime: Duration,
    claims: MachineTokenClaims,
}

/// The token endpoint's response (RFC 6749 §5.1). Only the two fields we use.
#[derive(Debug, Deserialize)]
struct TokenResponse {
    #[serde(default)]
    access_token: String,
    /// Seconds. Keycloak sends an integer; tolerate a float or a numeric
    /// string from other IdPs.
    #[serde(default)]
    expires_in: Option<serde_json::Value>,
}

impl TokenResponse {
    fn expires_in(&self) -> Option<Duration> {
        let secs = match self.expires_in.as_ref()? {
            serde_json::Value::Number(n) => n.as_f64()?,
            serde_json::Value::String(s) => s.trim().parse::<f64>().ok()?,
            _ => return None,
        };
        if secs.is_finite() && secs > 0.0 {
            Some(Duration::from_secs_f64(secs))
        } else {
            None
        }
    }
}

/// The subset of a Keycloak service-account access token the first-fetch log
/// line reports. Decoded WITHOUT signature verification: it is for
/// operator-visible metadata only; kbeads verifies the signature.
#[derive(Debug, Default, Deserialize)]
pub(crate) struct MachineTokenClaims {
    #[serde(default)]
    pub azp: String,
    #[serde(default)]
    pub preferred_username: String,
    #[serde(default)]
    pub aud: Option<serde_json::Value>,
    #[serde(default)]
    pub exp: i64,
    #[serde(default)]
    pub iat: i64,
    #[serde(default)]
    pub realm_roles: Vec<String>,
    #[serde(default)]
    pub realm_access: RealmAccess,
}

#[derive(Debug, Default, Deserialize)]
pub(crate) struct RealmAccess {
    #[serde(default)]
    pub roles: Vec<String>,
}

impl MachineTokenClaims {
    /// `aud` (a string or an array in OIDC) as a flat list.
    pub fn audience(&self) -> Vec<String> {
        match &self.aud {
            Some(serde_json::Value::String(s)) => vec![s.clone()],
            Some(serde_json::Value::Array(items)) => {
                items.iter().filter_map(|v| v.as_str().map(str::to_owned)).collect()
            }
            _ => Vec::new(),
        }
    }

    /// `realm_roles` (the chart's realm-roles mapper) merged with
    /// `realm_access.roles` (Keycloak's default), deduplicated, order kept.
    pub fn roles(&self) -> Vec<String> {
        let mut out: Vec<String> = Vec::new();
        for r in self.realm_roles.iter().chain(self.realm_access.roles.iter()) {
            if !r.is_empty() && !out.iter().any(|o| o == r) {
                out.push(r.clone());
            }
        }
        out
    }

    pub fn exp_minus_iat(&self) -> i64 {
        if self.exp == 0 || self.iat == 0 {
            0
        } else {
            self.exp - self.iat
        }
    }

    pub fn lifetime(&self) -> Option<Duration> {
        let s = self.exp_minus_iat();
        (s > 0).then(|| Duration::from_secs(s.unsigned_abs()))
    }
}

/// Decode the payload segment of a JWT without verifying it. Any malformed
/// input yields default (empty) claims rather than an error: the metadata is
/// informational only.
pub(crate) fn parse_jwt_claims(raw: &str) -> MachineTokenClaims {
    let Some(payload) = raw.split('.').nth(1) else {
        return MachineTokenClaims::default();
    };
    let Ok(bytes) = base64::engine::general_purpose::URL_SAFE_NO_PAD.decode(payload) else {
        return MachineTokenClaims::default();
    };
    serde_json::from_slice(&bytes).unwrap_or_default()
}

#[cfg(test)]
#[path = "machine_token_tests.rs"]
mod tests;
