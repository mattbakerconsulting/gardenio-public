// SPDX-License-Identifier: BUSL-1.1
// Copyright (c) 2026 Matt Baker <fimbaz@gmail.com>

//! Credential re-mint on thaw — Stages 1 & 2 of the agent rehydration design
//! (`gardenio/docs/gvisor-runsc-cr/rehydration-design.md`, ka-0000001515; build
//! bead ka-0000007211). This is coop's [`RehydrateHandler`] (replacing the
//! Stage-0 `AckOnlyHandler`): when the controller bumps the thaw epoch, the
//! [`ThawWatcher`](crate::thaw::ThawWatcher) drives this handler to **poison
//! the in-RAM Pod-A credentials and re-mint identity from the kubelet-fresh
//! on-disk tokens** before the agent is allowed to resume issuing work.
//!
//! The thawed process is living in the past: its guest RAM is a byte-for-byte
//! copy of the freeze instant, so every credential it cached before the freeze
//! is now stale or, worse, *still valid for a few minutes under Pod A's
//! identity* — the identity-confusion hazard. Rehydration is overwhelmingly
//! **cache invalidation**, not file plumbing: restore-through-lifecycle already
//! repointed the token mounts at Pod B's kubelet-owned dirs, so the files on
//! disk are correct and fresh; this stage just forces the consumers to re-read
//! them and fails closed if what's on disk still presents Pod A's identity.
//!
//! ## Stages (rehydration-design §4)
//!
//! 1. **Poison AWS STS cache (C1/C2, K1).** Rebuild the AWS credentials
//!    provider so the next AWS call re-reads `AWS_WEB_IDENTITY_TOKEN_FILE` and
//!    re-runs `AssumeRoleWithWebIdentity` rather than reusing STS creds cached
//!    under Pod A's web-identity token.
//! 2. **Re-read + gate the token files (K1, K2/K3).** Re-read the IRSA
//!    web-identity token and the projected SA token (the beads/k8s bearer
//!    source) and run each through the [`freshness_gate`]: the **fail-closed**
//!    JWT `aud`/`sub`/`iat` check that rejects a token still presenting Pod A's
//!    identity (design §5 failure-mode matrix, row 1).
//! 3. **Conditional account-cred re-resolution (K4).** Re-run
//!    `resolve-auth.sh`'s `_resolve_account_auth` *only* when the on-disk
//!    `.credentials.json` predates the freeze (a rotation under it), so the
//!    Claude CLI re-reads a fresh credentials file. Best-effort.
//!
//! NATS reconnect + re-announce (Stage 3/4) is a separate handler
//! (ka-0000001901); the coop watcher composes this re-mint stage with that
//! reconnect stage once both land.

use std::collections::BTreeMap;
use std::path::PathBuf;
use std::sync::Arc;

use futures_util::future::BoxFuture;
use serde::Deserialize;

use crate::thaw::{Marker, Outcome, RehydrateHandler};

/// Default on-disk path of the projected SA token in a pod. kubelet rotates
/// this file; after restore-through-lifecycle it points at Pod B's
/// kubelet-owned source dir, so re-reading it yields a fresh, Pod-B token.
pub const DEFAULT_SA_TOKEN_FILE: &str = "/var/run/secrets/kubernetes.io/serviceaccount/token";

/// The audience an IRSA web-identity token must carry. AWS STS only accepts a
/// web-identity token minted for this audience, so it is also a cheap sanity
/// check that we re-read the right file.
pub const AWS_STS_AUDIENCE: &str = "sts.amazonaws.com";

// ===========================================================================
// JWT freshness gate (the fail-closed identity check — fully unit-tested)
// ===========================================================================

/// The subset of JWT claims the freshness gate reasons about. We do **not**
/// verify the signature: kubelet already minted the on-disk token, and STS /
/// the API server verify it on use. The gate only asserts the *identity* the
/// token presents is Pod B's — not the poisoned Pod-A copy from guest RAM.
#[derive(Debug, Clone, Default, PartialEq, Eq)]
pub struct JwtClaims {
    /// `sub` — for a k8s SA token this is `system:serviceaccount:<ns>:<sa>`
    /// (identical across pods of the same SA, so not pod-distinguishing on its
    /// own); for IRSA it mirrors the SA. Surfaced for observability + the aud
    /// check pairing.
    pub sub: Option<String>,
    /// `aud` — JWT allows a single string or an array; normalized to a vec.
    pub aud: Vec<String>,
    /// `iat` — issued-at (Unix seconds). The load-bearing freshness signal: a
    /// token issued at/before the freeze is a Pod-A token still in RAM.
    pub iat: Option<i64>,
    /// `exp` — expiry (Unix seconds), when present.
    pub exp: Option<i64>,
    /// `kubernetes.io.pod.uid` — the pod the token was bound to. This *is*
    /// pod-distinguishing: a token bound to Pod A's uid under Pod B is the
    /// identity-confusion hazard.
    pub pod_uid: Option<String>,
}

/// What a fresh, Pod-B token must look like. Driven from the thaw marker
/// (`frozen_at`, `pod_uid`) plus the per-token expected audience.
#[derive(Debug, Clone, Default)]
pub struct Expectations {
    /// Unix seconds of the freeze. `iat` is the *fallback* freshness signal,
    /// used only when `pod_uid` has not already proven the binding: for a token
    /// with no pod-uid proof, `iat <= frozen_at` means it was issued before the
    /// freeze and is treated as stale. A token whose `pod_uid` matches Pod B is
    /// kubelet-bound proof of identity and is fresh regardless of `iat` (its SA
    /// token may have been minted at warm-pool-fill, before the freeze). `None`
    /// disables the iat signal.
    pub frozen_at: Option<i64>,
    /// kubelet pod UID of the pod we were thawed *into* (Pod B). A token whose
    /// `pod_uid` claim differs is Pod A's (or some other pod's). `None`
    /// disables the pod-uid signal.
    pub expected_pod_uid: Option<String>,
    /// Required audience. `None` disables the audience check (used for the SA
    /// token, whose audience is cluster-configured).
    pub expected_aud: Option<String>,
}

/// Why a token failed the freshness gate. Every variant is a **fail-closed**
/// outcome: the agent must not resume under this token.
#[derive(Debug, Clone, PartialEq, Eq)]
pub enum FreshnessError {
    /// The token file does not exist (ENOENT). Distinct from [`Malformed`](Self::Malformed):
    /// an absent file means the mount was never repointed or the warm copy dropped
    /// it (ka-0000008074 — the freeze sweep silently dropped the kubelet
    /// atomic-writer `token` symlink), NOT that the JWT bytes are bad. Classifying
    /// it separately keeps a missing-mount bug from masquerading as a malformed-JWT
    /// error in the ack — that misclassification cost the gVisor canary forensics a
    /// cycle (the prod ack read `malformed JWT: ... No such file or directory`). Fail
    /// closed all the same.
    Missing(String),
    /// The token could not be parsed as a JWT (wrong segment count, bad
    /// base64url, or non-JSON claims). Fail closed — we cannot prove freshness.
    Malformed(String),
    /// `exp` is at/before now — the token is already expired.
    Expired { exp: i64, now: i64 },
    /// `aud` does not contain the required audience.
    WrongAudience { want: String, got: Vec<String> },
    /// The `pod_uid` claim names a pod other than the one we thawed into — the
    /// Pod-A identity-confusion hazard.
    PodAIdentity { want: String, got: String },
    /// `iat <= frozen_at` — the token was issued before the freeze, so it is a
    /// pre-thaw (Pod A) token still in RAM.
    StaleIssuedAt { iat: i64, frozen_at: i64 },
    /// No freshness signal was available (no pod-uid claim and no `frozen_at`
    /// to compare `iat` against) — freshness is indeterminate. The design says
    /// "indistinguishable → fail closed" (§5), so we reject rather than risk
    /// running under an ambiguous identity.
    Indeterminate,
}

impl std::fmt::Display for FreshnessError {
    fn fmt(&self, f: &mut std::fmt::Formatter<'_>) -> std::fmt::Result {
        match self {
            FreshnessError::Missing(path) => write!(f, "token file not found: {path}"),
            FreshnessError::Malformed(why) => write!(f, "malformed JWT: {why}"),
            FreshnessError::Expired { exp, now } => {
                write!(f, "token expired (exp={exp} <= now={now})")
            }
            FreshnessError::WrongAudience { want, got } => {
                write!(f, "wrong audience (want={want}, got={got:?})")
            }
            FreshnessError::PodAIdentity { want, got } => {
                write!(f, "stale Pod-A identity (token pod_uid={got}, expected Pod-B {want})")
            }
            FreshnessError::StaleIssuedAt { iat, frozen_at } => {
                write!(f, "token issued before freeze (iat={iat} <= frozen_at={frozen_at})")
            }
            FreshnessError::Indeterminate => {
                write!(f, "no freshness signal available; failing closed")
            }
        }
    }
}

impl std::error::Error for FreshnessError {}

/// Decode the *claims* of a JWT without verifying its signature.
///
/// A JWT is `header.payload.signature`, each segment base64url (no padding).
/// We decode only the payload. Signature verification is intentionally out of
/// scope — the on-disk token was minted by kubelet and is verified by the
/// service that consumes it; the gate's job is the identity/freshness check.
pub fn decode_jwt_claims(token: &str) -> Result<JwtClaims, FreshnessError> {
    use base64::Engine;

    let token = token.trim();
    let mut parts = token.split('.');
    let (_header, payload, sig) = match (parts.next(), parts.next(), parts.next()) {
        (Some(h), Some(p), Some(s)) => (h, p, s),
        _ => return Err(FreshnessError::Malformed("expected 3 dot-separated segments".into())),
    };
    if sig.is_empty() || parts.next().is_some() {
        return Err(FreshnessError::Malformed("expected exactly 3 segments".into()));
    }

    let bytes = base64::engine::general_purpose::URL_SAFE_NO_PAD
        .decode(payload)
        .map_err(|e| FreshnessError::Malformed(format!("payload base64url decode: {e}")))?;

    let raw: RawClaims = serde_json::from_slice(&bytes)
        .map_err(|e| FreshnessError::Malformed(format!("payload JSON: {e}")))?;

    Ok(JwtClaims {
        sub: raw.sub,
        aud: raw.aud.map(Audience::into_vec).unwrap_or_default(),
        iat: raw.iat,
        exp: raw.exp,
        pod_uid: raw.kubernetes_io.and_then(|k| k.pod).and_then(|p| p.uid),
    })
}

/// The fail-closed JWT freshness gate (rehydration-design §5).
///
/// Returns `Ok(())` iff `claims` are *proven* to be a fresh Pod-B token, and
/// `Err` otherwise. The logic is: reject on any unconditional hard disqualifier
/// (expired, wrong audience, Pod-A pod-uid); then require at least one *positive*
/// freshness proof (matching pod-uid or post-freeze `iat`). A pre-freeze `iat`
/// is a hard disqualifier *only as the fallback signal* — when a matching
/// pod-uid has already proven the binding (kubelet-bound, unforgeable), a stale
/// `iat` is ignored, because Pod B's warm-pool token is legitimately pre-freeze.
/// With no proof either way, fail closed ([`FreshnessError::Indeterminate`]) — a
/// stale-but-not-yet-expired Pod-A token must never slip through.
pub fn freshness_gate(
    claims: &JwtClaims,
    expect: &Expectations,
    now: i64,
) -> Result<(), FreshnessError> {
    // --- Hard disqualifiers: proven stale ---------------------------------
    if let Some(exp) = claims.exp {
        if exp <= now {
            return Err(FreshnessError::Expired { exp, now });
        }
    }
    if let Some(want) = &expect.expected_aud {
        if !claims.aud.iter().any(|a| a == want) {
            return Err(FreshnessError::WrongAudience {
                want: want.clone(),
                got: claims.aud.clone(),
            });
        }
    }

    // A token can be proven fresh by either a matching pod-uid or an iat after
    // the freeze line. Disqualifiers above always win; a positive proof here
    // only rescues a token that cleared every disqualifier.
    let mut proven_fresh = false;

    if let Some(want_uid) = &expect.expected_pod_uid {
        match &claims.pod_uid {
            Some(got) if got == want_uid => proven_fresh = true,
            Some(got) => {
                return Err(FreshnessError::PodAIdentity {
                    want: want_uid.clone(),
                    got: got.clone(),
                });
            }
            None => {} // no pod-uid claim on this token; fall back to iat
        }
    }

    if let Some(frozen_at) = expect.frozen_at {
        match claims.iat {
            // A matching pod-uid (set `proven_fresh` above) is a kubelet-bound
            // proof this is Pod B's own token — it cannot be a stale Pod-A copy
            // (those carry Pod A's uid and already hit `PodAIdentity`). So a
            // proven pod-uid *suppresses* the stale-iat disqualifier: on the
            // warm-graft hot path Pod B's honest token was minted at
            // warm-pool-fill, i.e. before the agent freeze, so `iat <= frozen_at`
            // is the expected case and must NOT fail closed. The iat line is only
            // the *fallback* freshness signal, for tokens with no pod-uid proof.
            Some(iat) if !proven_fresh && iat <= frozen_at => {
                return Err(FreshnessError::StaleIssuedAt { iat, frozen_at });
            }
            Some(_) => proven_fresh = true,
            None => {} // no iat claim; rely on pod-uid
        }
    }

    if proven_fresh {
        Ok(())
    } else {
        Err(FreshnessError::Indeterminate)
    }
}

#[derive(Debug, Deserialize)]
struct RawClaims {
    sub: Option<String>,
    aud: Option<Audience>,
    iat: Option<i64>,
    exp: Option<i64>,
    #[serde(rename = "kubernetes.io")]
    kubernetes_io: Option<KubernetesIo>,
}

/// JWT `aud` is either a single string or an array of strings.
#[derive(Debug, Deserialize)]
#[serde(untagged)]
enum Audience {
    One(String),
    Many(Vec<String>),
}

impl Audience {
    fn into_vec(self) -> Vec<String> {
        match self {
            Audience::One(s) => vec![s],
            Audience::Many(v) => v,
        }
    }
}

#[derive(Debug, Deserialize)]
struct KubernetesIo {
    pod: Option<KubePod>,
}

#[derive(Debug, Deserialize)]
struct KubePod {
    uid: Option<String>,
}

// ===========================================================================
// AWS STS cache poison (Stage 1)
// ===========================================================================

/// A reload hook the watcher invokes to rebuild the AWS credentials provider so
/// the next AWS call re-reads `AWS_WEB_IDENTITY_TOKEN_FILE`. Boxed so the
/// handler stays one concrete type regardless of whether S3 (and thus an AWS
/// client) is wired in. Construct from an `Arc<S3Client>` via
/// [`s3_aws_reset`](crate::rehydrate::s3_aws_reset) when the `s3` feature is on.
pub type AwsReset = Arc<dyn Fn() -> BoxFuture<'static, ()> + Send + Sync>;

/// Build an [`AwsReset`] that rebuilds the given S3 client's credentials
/// provider on thaw.
#[cfg(feature = "s3")]
pub fn s3_aws_reset(client: Arc<crate::s3::S3Client>) -> AwsReset {
    Arc::new(move || {
        let client = Arc::clone(&client);
        Box::pin(async move {
            client.reload_credentials().await;
        })
    })
}

// ===========================================================================
// Conditional account-cred re-resolution (Stage 2, K4)
// ===========================================================================

/// Config for the conditional `resolve-auth.sh` re-run (K4). Enabled only when
/// both the script and the on-disk credentials file are known; the re-run fires
/// only when the credentials file predates the freeze (a rotation under us).
#[derive(Debug, Clone)]
pub struct ResolveAuthCfg {
    /// Path to `resolve-auth.sh` (the sourceable script exposing
    /// `_resolve_account_auth`).
    pub script: PathBuf,
    /// Path to the on-disk `.credentials.json` the Claude CLI reads.
    pub creds_file: PathBuf,
}

// ===========================================================================
// The handler
// ===========================================================================

/// A token source the handler re-reads and gates on thaw.
#[derive(Debug, Clone)]
struct TokenSource {
    /// Stage label for the ack's per-stage map (e.g. `"aws_token"`).
    stage: &'static str,
    /// File path to re-read.
    path: PathBuf,
    /// Required audience for this token (`None` to skip the aud check).
    expected_aud: Option<String>,
}

/// coop's Stage 1+2 rehydration handler: poison the AWS STS cache, re-read the
/// IRSA + SA tokens and fail closed if either still presents Pod-A identity,
/// and conditionally re-resolve account creds.
pub struct CredentialReminter {
    sources: Vec<TokenSource>,
    aws_reset: Option<AwsReset>,
    resolve_auth: Option<ResolveAuthCfg>,
    now_fn: fn() -> i64,
}

impl std::fmt::Debug for CredentialReminter {
    fn fmt(&self, f: &mut std::fmt::Formatter<'_>) -> std::fmt::Result {
        f.debug_struct("CredentialReminter")
            .field("sources", &self.sources)
            .field("aws_reset", &self.aws_reset.is_some())
            .field("resolve_auth", &self.resolve_auth)
            .finish()
    }
}

fn now_unix_secs() -> i64 {
    std::time::SystemTime::now()
        .duration_since(std::time::UNIX_EPOCH)
        .map(|d| d.as_secs() as i64)
        .unwrap_or(0)
}

impl CredentialReminter {
    /// Construct an empty reminter (no token sources, no AWS reset). Builders
    /// below add sources; [`from_env`](Self::from_env) wires the production
    /// layout.
    pub fn new() -> Self {
        Self { sources: Vec::new(), aws_reset: None, resolve_auth: None, now_fn: now_unix_secs }
    }

    /// Wire the production credential layout from the environment:
    ///
    /// - IRSA web-identity token: `AWS_WEB_IDENTITY_TOKEN_FILE` (required when
    ///   set — IRSA is configured iff the env var is present).
    /// - projected SA token: `COOP_SA_TOKEN_FILE` or the default pod path, when
    ///   it exists (so dev/local runs without a pod simply skip it).
    /// - conditional account re-resolution: `COOP_RESOLVE_AUTH_SCRIPT` +
    ///   `CREDS_PVC` (or `COOP_CREDS_FILE`), when both are set.
    pub fn from_env(aws_reset: Option<AwsReset>) -> Self {
        let mut me = Self::new();
        me.aws_reset = aws_reset;

        if let Ok(path) = std::env::var("AWS_WEB_IDENTITY_TOKEN_FILE") {
            if !path.is_empty() {
                me.sources.push(TokenSource {
                    stage: "aws_token",
                    path: PathBuf::from(path),
                    expected_aud: Some(AWS_STS_AUDIENCE.to_owned()),
                });
            }
        }

        let sa_path = std::env::var("COOP_SA_TOKEN_FILE")
            .ok()
            .filter(|s| !s.is_empty())
            .unwrap_or_else(|| DEFAULT_SA_TOKEN_FILE.to_owned());
        if std::path::Path::new(&sa_path).exists() {
            me.sources.push(TokenSource {
                stage: "sa_token",
                path: PathBuf::from(sa_path),
                expected_aud: None,
            });
        }

        if let Some(cfg) = resolve_auth_from_env() {
            me.resolve_auth = Some(cfg);
        }

        me
    }

    /// Add an IRSA web-identity token source (required, audience-checked).
    pub fn with_web_identity_token(mut self, path: PathBuf) -> Self {
        self.sources.push(TokenSource {
            stage: "aws_token",
            path,
            expected_aud: Some(AWS_STS_AUDIENCE.to_owned()),
        });
        self
    }

    /// Add a projected SA token source (required, no audience check).
    pub fn with_sa_token(mut self, path: PathBuf) -> Self {
        self.sources.push(TokenSource { stage: "sa_token", path, expected_aud: None });
        self
    }

    /// Set the AWS STS cache-poison hook.
    pub fn with_aws_reset(mut self, reset: AwsReset) -> Self {
        self.aws_reset = Some(reset);
        self
    }

    /// Set the conditional account-cred re-resolution config (K4).
    pub fn with_resolve_auth(mut self, cfg: ResolveAuthCfg) -> Self {
        self.resolve_auth = Some(cfg);
        self
    }

    /// Override the clock (tests).
    #[cfg(test)]
    fn with_now_fn(mut self, f: fn() -> i64) -> Self {
        self.now_fn = f;
        self
    }

    /// Re-read one token file and run it through the freshness gate. Returns
    /// the gate result; a read failure / empty file is itself a fail-closed
    /// error (a configured-but-blank token means the mount was not repointed —
    /// design §5 failure-mode matrix). An ENOENT is classified as
    /// [`FreshnessError::Missing`] (not `Malformed`): the file is absent, not a bad
    /// JWT, so a dropped/unrepointed token mount reads as a missing-file diagnostic
    /// rather than masquerading as malformed-JWT in the ack (ka-0000008074).
    fn gate_token(&self, src: &TokenSource, expect: &Expectations) -> Result<(), FreshnessError> {
        let raw = std::fs::read_to_string(&src.path).map_err(|e| {
            if e.kind() == std::io::ErrorKind::NotFound {
                FreshnessError::Missing(src.path.display().to_string())
            } else {
                FreshnessError::Malformed(format!("read {}: {e}", src.path.display()))
            }
        })?;
        if raw.trim().is_empty() {
            return Err(FreshnessError::Malformed(format!("{} is empty", src.path.display())));
        }
        let claims = decode_jwt_claims(&raw)?;
        let mut expect = expect.clone();
        expect.expected_aud = src.expected_aud.clone();
        freshness_gate(&claims, &expect, (self.now_fn)())
    }

    /// Expectations derived from the marker for *this* thaw.
    fn expectations(&self, marker: &Marker) -> Expectations {
        Expectations {
            frozen_at: marker.frozen_at.as_deref().and_then(parse_unix_ts),
            expected_pod_uid: if marker.pod_uid.is_empty() {
                None
            } else {
                Some(marker.pod_uid.clone())
            },
            expected_aud: None,
        }
    }

    /// Best-effort conditional account-cred re-resolution (K4). Fires only when
    /// the on-disk credentials file predates the freeze. Returns the stage
    /// status string for the ack.
    fn maybe_resolve_account(&self, marker: &Marker) -> &'static str {
        let Some(cfg) = &self.resolve_auth else { return "skipped" };
        let frozen_at = marker.frozen_at.as_deref().and_then(parse_unix_ts);

        // Only re-run when we can see the creds file predates the freeze.
        let stale = match (frozen_at, creds_mtime_unix(&cfg.creds_file)) {
            (Some(frozen), Some(mtime)) => mtime <= frozen,
            // No freeze stamp or unreadable mtime → don't burn a coopmux
            // round-trip on every thaw; the Claude CLI re-reads per request.
            _ => false,
        };
        if !stale {
            return "skipped";
        }

        match run_resolve_auth(cfg) {
            Ok(()) => {
                tracing::info!(creds = %cfg.creds_file.display(), "thaw: re-resolved account creds");
                "ok"
            }
            Err(e) => {
                // K4 is best-effort (design §5): log, do not fail the thaw.
                tracing::warn!(error = %e, "thaw: account-cred re-resolution failed (best-effort)");
                "failed"
            }
        }
    }
}

impl Default for CredentialReminter {
    fn default() -> Self {
        Self::new()
    }
}

impl RehydrateHandler for CredentialReminter {
    async fn rehydrate(&self, marker: &Marker) -> Outcome {
        let mut stages: BTreeMap<String, String> = BTreeMap::new();
        let mut first_error: Option<String> = None;
        let expect = self.expectations(marker);

        // --- Stage 1: poison the AWS STS cache (required when wired) -------
        match &self.aws_reset {
            Some(reset) => {
                reset().await;
                stages.insert("aws_sts".into(), "ok".into());
            }
            None => {
                stages.insert("aws_sts".into(), "skipped".into());
            }
        }

        // --- Stage 2: re-read + gate the identity tokens (required) --------
        // Each configured token is required: a stale/blank token fails the
        // whole thaw closed (C5) rather than resuming under Pod-A identity.
        for src in &self.sources {
            match self.gate_token(src, &expect) {
                Ok(()) => {
                    stages.insert(src.stage.into(), "ok".into());
                }
                Err(e) => {
                    let msg = format!("{}: {e}", src.stage);
                    tracing::warn!(stage = src.stage, error = %e, "thaw: token freshness gate failed");
                    stages.insert(src.stage.into(), format!("failed: {e}"));
                    first_error.get_or_insert(msg);
                }
            }
        }

        // --- Stage 2 (K4): conditional account-cred re-resolution ----------
        let account_status = self.maybe_resolve_account(marker);
        stages.insert("account".into(), account_status.into());

        match first_error {
            None => Outcome { ok: true, error: None, stages },
            Some(err) => Outcome { ok: false, error: Some(err), stages },
        }
    }
}

/// Parse a marker timestamp into Unix seconds. The on-disk contract emits
/// `@<unix-secs>` (coop), a bare numeric `<unix-secs>`, or an RFC3339 string
/// (Go: [`thawepoch.Marker.FrozenAt`] marshals as RFC3339 via `time.Time`'s
/// `json:"frozen_at"`). Accepting the RFC3339 form is what makes the `iat`
/// freshness line of the gate live — without it, a Go-written `frozen_at`
/// parsed `None` and the `StaleIssuedAt`/post-freeze-`iat` signal was inert
/// (ka-0000004956). An unparseable stamp still yields `None` (the gate then
/// relies on the pod-uid signal alone).
fn parse_unix_ts(s: &str) -> Option<i64> {
    let s = s.trim();
    if let Some(rest) = s.strip_prefix('@') {
        return rest.trim().parse::<i64>().ok();
    }
    if let Ok(n) = s.parse::<i64>() {
        return Some(n);
    }
    parse_rfc3339_unix(s)
}

/// Parse an RFC3339 / ISO-8601 timestamp into Unix seconds. coop carries no
/// date crate (it has historically only round-tripped the marker `ts` as an
/// opaque string), so this is a small, dependency-free decoder for exactly the
/// shape Go's `time.Time` JSON marshaller emits: `YYYY-MM-DDThh:mm:ss` followed
/// by an optional fractional part and a `Z`/`±hh:mm` offset. Fractional seconds
/// are truncated (we only need second precision for the freshness compare).
/// Returns `None` on any shape it does not fully recognize — fail closed to the
/// pod-uid signal rather than guess.
fn parse_rfc3339_unix(s: &str) -> Option<i64> {
    let bytes = s.as_bytes();
    // Date: YYYY-MM-DD, then a 'T' or ' ' separator.
    if bytes.len() < 19 {
        return None;
    }
    let year: i64 = s.get(0..4)?.parse().ok()?;
    if bytes[4] != b'-' {
        return None;
    }
    let month: i64 = s.get(5..7)?.parse().ok()?;
    if bytes[7] != b'-' {
        return None;
    }
    let day: i64 = s.get(8..10)?.parse().ok()?;
    if bytes[10] != b'T' && bytes[10] != b't' && bytes[10] != b' ' {
        return None;
    }
    let hour: i64 = s.get(11..13)?.parse().ok()?;
    if bytes[13] != b':' {
        return None;
    }
    let min: i64 = s.get(14..16)?.parse().ok()?;
    if bytes[16] != b':' {
        return None;
    }
    let sec: i64 = s.get(17..19)?.parse().ok()?;
    if !(1..=12).contains(&month) || !(1..=31).contains(&day) {
        return None;
    }
    // The (1..=31) bound alone lets impossible civil dates through:
    // `days_from_civil` is a pure proleptic-Gregorian day count with no
    // calendar-validity guard, so e.g. 2023-02-29 would silently roll forward
    // to 2023-03-01. Reject any day past the actual days-in-month so the parser
    // fails closed to the pod-uid signal instead (ka-0000007623).
    if day > i64::from(days_in_month(year, month)) {
        return None;
    }
    if hour > 23 || min > 59 || sec > 60 {
        return None;
    }

    // Skip an optional fractional-seconds part (".123456789"); we keep second
    // precision only.
    let mut rest = &s[19..];
    if rest.starts_with('.') {
        let frac_end = rest[1..].find(|c: char| !c.is_ascii_digit()).map(|i| i + 1);
        match frac_end {
            Some(i) => rest = &rest[i..],
            None => return None, // fractional digits with no trailing offset/Z
        }
    }

    // Timezone offset: 'Z'/'z' (UTC) or ±hh:mm.
    let offset_secs: i64 = match rest.as_bytes().first() {
        Some(b'Z') | Some(b'z') if rest.len() == 1 => 0,
        Some(sign @ (b'+' | b'-')) => {
            // Expect ±hh:mm (6 chars) or ±hhmm (5 chars).
            let off = &rest[1..];
            let (oh, om): (i64, i64) = if off.len() == 5 && off.as_bytes()[2] == b':' {
                (off.get(0..2)?.parse().ok()?, off.get(3..5)?.parse().ok()?)
            } else if off.len() == 4 {
                (off.get(0..2)?.parse().ok()?, off.get(2..4)?.parse().ok()?)
            } else {
                return None;
            };
            if oh > 23 || om > 59 {
                return None;
            }
            let mag = oh * 3600 + om * 60;
            if *sign == b'-' {
                -mag
            } else {
                mag
            }
        }
        _ => return None,
    };

    // Days since the Unix epoch via Howard Hinnant's days_from_civil.
    let days = days_from_civil(year, month, day);
    // The wall-clock above is in the named offset; subtract it to reach UTC.
    Some(days * 86400 + hour * 3600 + min * 60 + sec - offset_secs)
}

/// Number of days in the given civil month, with the proleptic-Gregorian
/// leap-year rule (a year is a leap year iff divisible by 4, except centuries,
/// which must also be divisible by 400). Used to reject impossible civil dates
/// in `parse_rfc3339_unix` before they reach `days_from_civil` (which has no
/// calendar-validity guard). `month` is assumed already range-checked to 1..=12.
fn days_in_month(year: i64, month: i64) -> u8 {
    match month {
        1 | 3 | 5 | 7 | 8 | 10 | 12 => 31,
        4 | 6 | 9 | 11 => 30,
        2 => {
            let leap = year % 4 == 0 && (year % 100 != 0 || year % 400 == 0);
            if leap {
                29
            } else {
                28
            }
        }
        _ => 0, // unreachable for a range-checked month; fail closed.
    }
}

/// Days from 1970-01-01 to the given civil (proleptic Gregorian) date. Howard
/// Hinnant's `days_from_civil` (http://howardhinnant.github.io/date_algorithms.html);
/// exact for all dates we will ever see and free of any date dependency.
fn days_from_civil(y: i64, m: i64, d: i64) -> i64 {
    let y = if m <= 2 { y - 1 } else { y };
    let era = if y >= 0 { y } else { y - 399 } / 400;
    let yoe = y - era * 400; // [0, 399]
    let doy = (153 * (if m > 2 { m - 3 } else { m + 9 }) + 2) / 5 + d - 1; // [0, 365]
    let doe = yoe * 365 + yoe / 4 - yoe / 100 + doy; // [0, 146096]
    era * 146097 + doe - 719468
}

fn creds_mtime_unix(path: &std::path::Path) -> Option<i64> {
    let meta = std::fs::metadata(path).ok()?;
    let mtime = meta.modified().ok()?;
    mtime.duration_since(std::time::UNIX_EPOCH).ok().map(|d| d.as_secs() as i64)
}

fn resolve_auth_from_env() -> Option<ResolveAuthCfg> {
    let script = std::env::var("COOP_RESOLVE_AUTH_SCRIPT").ok().filter(|s| !s.is_empty())?;
    let creds_file =
        std::env::var("COOP_CREDS_FILE").ok().filter(|s| !s.is_empty()).or_else(|| {
            std::env::var("CREDS_PVC").ok().filter(|s| !s.is_empty()).map(|d| {
                std::path::Path::new(&d).join(".credentials.json").to_string_lossy().into_owned()
            })
        })?;
    Some(ResolveAuthCfg { script: PathBuf::from(script), creds_file: PathBuf::from(creds_file) })
}

/// Source `resolve-auth.sh` and run `_resolve_account_auth`. The script writes
/// a fresh `.credentials.json` to `$CREDS_PVC`, which the Claude CLI re-reads
/// per request — that on-disk refresh is the meaningful effect on thaw.
fn run_resolve_auth(cfg: &ResolveAuthCfg) -> anyhow::Result<()> {
    use std::process::Command;
    let script = cfg.script.to_string_lossy();
    let out = Command::new("bash")
        .arg("-c")
        .arg(format!("set -e; source \"{script}\"; _resolve_account_auth"))
        .output()?;
    if !out.status.success() {
        anyhow::bail!(
            "resolve-auth.sh exited {:?}: {}",
            out.status.code(),
            String::from_utf8_lossy(&out.stderr).trim()
        );
    }
    Ok(())
}

#[cfg(test)]
#[path = "rehydrate_tests.rs"]
mod tests;
