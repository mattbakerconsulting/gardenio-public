// SPDX-License-Identifier: BUSL-1.1
// Copyright (c) 2026 Matt Baker <fimbaz@gmail.com>

//! OIDC JWT token verification using JWKS discovery.

use std::sync::Arc;

use jsonwebtoken::{decode, decode_header, Algorithm, DecodingKey, Validation};
use tokio::sync::RwLock;

use crate::error::MuxError;

/// User-Agent string sent on every OIDC request.
///
/// A non-empty UA avoids tripping empty-User-Agent WAF rules (e.g. AWS managed
/// common-rule-set) when calling discovery, JWKS, token, refresh, or userinfo
/// endpoints.
pub(crate) const OIDC_USER_AGENT: &str = concat!("coopmux/", env!("CARGO_PKG_VERSION"));

/// The authorization-relevant claims of a verified OIDC token.
///
/// Both carry the union of the claims the two issuers we validate against
/// emit, mirroring kbeads' `ExtractRoles` / `ExtractGroups`
/// (`kbeads/internal/server/oidc.go`): roles from Keycloak's
/// `realm_access.roles` and the chart's flat `realm_roles` mapper, groups
/// from Keycloak's `groups` and GitLab's `groups_direct`.
#[derive(Debug, Clone, Default, PartialEq, Eq)]
pub struct OidcClaims {
    pub roles: Vec<String>,
    pub groups: Vec<String>,
}

impl OidcClaims {
    /// Parse the role and group claims out of a verified token's payload.
    ///
    /// Each claim is read on its own and non-string entries are skipped, so a
    /// malformed groups claim cannot also discard the roles: a project member
    /// whose groups we cannot read is scoped to no lands, never unrestricted
    /// (kbeads `scopeFromContext`).
    pub(crate) fn from_payload(payload: &serde_json::Value) -> Self {
        Self {
            roles: merge_unique(
                string_array(payload.pointer("/realm_access/roles")),
                string_array(payload.get("realm_roles")),
            ),
            groups: merge_unique(
                string_array(payload.get("groups")),
                string_array(payload.get("groups_direct")),
            ),
        }
    }
}

fn string_array(value: Option<&serde_json::Value>) -> Vec<String> {
    value
        .and_then(|v| v.as_array())
        .map(|items| items.iter().filter_map(|v| v.as_str()).map(str::to_owned).collect())
        .unwrap_or_default()
}

fn merge_unique(a: Vec<String>, b: Vec<String>) -> Vec<String> {
    let mut out: Vec<String> = Vec::with_capacity(a.len() + b.len());
    for v in a.into_iter().chain(b) {
        if !v.is_empty() && !out.contains(&v) {
            out.push(v);
        }
    }
    out
}

/// OIDC token verifier that validates JWTs against a JWKS endpoint.
///
/// Fetches the JWKS from the OIDC provider on creation and caches it.
/// If a token references an unknown `kid`, the JWKS is refreshed once.
pub struct OidcVerifier {
    client: reqwest::Client,
    jwks_uri: String,
    audience: String,
    issuer: String,
    jwks: Arc<RwLock<jsonwebtoken::jwk::JwkSet>>,
}

impl OidcVerifier {
    /// Create a new OIDC verifier by discovering the provider configuration.
    ///
    /// Fetches `{issuer_url}/.well-known/openid-configuration` and then the JWKS.
    pub async fn new(issuer_url: &str, audience: &str) -> anyhow::Result<Self> {
        let issuer = issuer_url.trim_end_matches('/').to_owned();
        let discovery_url = format!("{issuer}/.well-known/openid-configuration");
        let client = reqwest::Client::builder().user_agent(OIDC_USER_AGENT).build()?;

        let discovery: serde_json::Value =
            client.get(&discovery_url).send().await?.error_for_status()?.json().await?;

        let jwks_uri = discovery["jwks_uri"]
            .as_str()
            .ok_or_else(|| anyhow::anyhow!("OIDC discovery missing jwks_uri"))?
            .to_owned();

        let jwks: jsonwebtoken::jwk::JwkSet =
            client.get(&jwks_uri).send().await?.error_for_status()?.json().await?;

        tracing::info!(
            issuer = %issuer,
            jwks_uri = %jwks_uri,
            keys = jwks.keys.len(),
            "OIDC verifier initialized"
        );

        Ok(Self {
            client,
            jwks_uri,
            audience: audience.to_owned(),
            issuer,
            jwks: Arc::new(RwLock::new(jwks)),
        })
    }

    /// A verifier over a fixed key set, for tests that sign their own tokens.
    #[cfg(test)]
    pub(crate) fn from_jwks(issuer: &str, audience: &str, jwks: jsonwebtoken::jwk::JwkSet) -> Self {
        Self {
            client: reqwest::Client::new(),
            jwks_uri: String::new(),
            audience: audience.to_owned(),
            issuer: issuer.to_owned(),
            jwks: Arc::new(RwLock::new(jwks)),
        }
    }

    /// Verify a JWT token against the cached JWKS and return its role and
    /// group claims.
    ///
    /// If the token's `kid` is not found in the cache, the JWKS is refreshed
    /// once and verification retried.
    pub async fn verify(&self, token: &str) -> Result<OidcClaims, MuxError> {
        let header = decode_header(token).map_err(|_| MuxError::Unauthorized)?;
        let kid = header.kid.as_deref().ok_or(MuxError::Unauthorized)?;

        // Try with cached JWKS first.
        if let Ok(claims) = self.try_verify(token, kid).await {
            return Ok(claims);
        }

        // Key not found or verification failed — refresh JWKS once and retry.
        if self.refresh_jwks().await.is_err() {
            return Err(MuxError::Unauthorized);
        }

        self.try_verify(token, kid).await
    }

    async fn try_verify(&self, token: &str, kid: &str) -> Result<OidcClaims, MuxError> {
        let jwks = self.jwks.read().await;
        let jwk = jwks.find(kid).ok_or(MuxError::Unauthorized)?;
        let key = DecodingKey::from_jwk(jwk).map_err(|_| MuxError::Unauthorized)?;

        let mut validation = Validation::new(Algorithm::RS256);
        validation.set_audience(&[&self.audience]);
        validation.set_issuer(&[&self.issuer]);

        let data = decode::<serde_json::Value>(token, &key, &validation)
            .map_err(|_| MuxError::Unauthorized)?;

        Ok(OidcClaims::from_payload(&data.claims))
    }

    async fn refresh_jwks(&self) -> Result<(), MuxError> {
        let new_jwks: jsonwebtoken::jwk::JwkSet = self
            .client
            .get(&self.jwks_uri)
            .send()
            .await
            .map_err(|_| MuxError::Unauthorized)?
            .error_for_status()
            .map_err(|_| MuxError::Unauthorized)?
            .json()
            .await
            .map_err(|_| MuxError::Unauthorized)?;

        tracing::debug!(keys = new_jwks.keys.len(), "JWKS refreshed");

        let mut jwks = self.jwks.write().await;
        *jwks = new_jwks;

        Ok(())
    }
}

#[cfg(test)]
#[path = "oidc_tests.rs"]
mod tests;
