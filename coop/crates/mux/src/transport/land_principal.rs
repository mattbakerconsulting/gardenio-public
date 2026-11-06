// SPDX-License-Identifier: BUSL-1.1
// Copyright (c) 2026 Matt Baker <fimbaz@gmail.com>

//! Land principal: a coopmux bearer bound to one land (gd-zVTBvaSIdG).
//!
//! The static `COOP_MUX_AUTH_TOKEN` is fleet-wide, and `register_session` used
//! to take a session's url, project and account from the request body. A
//! tenant pod holding that token could register a session that claimed a
//! fleet project and be pushed fleet credentials. A tenant pod authenticates
//! with a land token instead, and coopmux learns the land from the token,
//! never from the body.
//!
//! A land token names its land and carries an HMAC-SHA256 of that name under
//! a key only coopmux and the controller (which mints it) hold:
//!
//! ```text
//! coopmux-land.v1.<land>.<base64url, no padding: HMAC(key, "coopmux-land.v1.<land>")>
//! ```
//!
//! The key file is named by `COOP_MUX_LAND_TOKEN_KEY_FILE`; with no key, land
//! tokens are refused. A land principal has the scope of a land OIDC member
//! ([`SessionScope::Projects`] of its land, capped at the operator tier) and
//! may also register and deregister its own land's sessions.
//! [`bind_registration`] and [`check_upstream_identity`] hold a registration
//! to that land.

use std::path::Path;

use base64::engine::general_purpose::URL_SAFE_NO_PAD as B64URL;
use base64::Engine as _;
use ring::hmac;

use crate::credential::land::{land_account_name, land_of_account};
use crate::transport::project_scope::SessionScope;

/// Env var: path of the file holding the land-token HMAC key.
pub const ENV_KEY_FILE: &str = "COOP_MUX_LAND_TOKEN_KEY_FILE";
/// Prefix of every land token; also the HMAC message prefix.
const TOKEN_PREFIX: &str = "coopmux-land.v1.";
/// Metadata key coopmux stamps on a land principal's session. It decides the
/// session's land ahead of its account or project (`credential::land`).
pub const LAND_BINDING_KEY: &str = "land";
/// Longest land name: a DNS label less the `land-` namespace prefix.
const MAX_LAND_LEN: usize = 58;

/// The verified land of the request's bearer, stamped on the request
/// extensions by `auth_layer`.
#[derive(Debug, Clone, PartialEq, Eq)]
pub struct LandPrincipal {
    pub land: String,
}

impl LandPrincipal {
    pub fn scope(&self) -> SessionScope {
        SessionScope::Projects(vec![self.land.clone()])
    }
}

/// The HMAC key land tokens are minted and verified with.
pub struct LandTokenKey(hmac::Key);

impl std::fmt::Debug for LandTokenKey {
    fn fmt(&self, f: &mut std::fmt::Formatter<'_>) -> std::fmt::Result {
        f.write_str("LandTokenKey(..)")
    }
}

impl LandTokenKey {
    /// A key from raw bytes; `None` for an empty key, which would make every
    /// land's token public.
    pub fn new(secret: &[u8]) -> Option<Self> {
        (!secret.is_empty()).then(|| Self(hmac::Key::new(hmac::HMAC_SHA256, secret)))
    }

    /// Load the key named by [`ENV_KEY_FILE`]. Unset means land tokens are
    /// off. A set path that cannot be read, or holds an empty key, is an error:
    /// coopmux refuses to start rather than silently refusing every tenant.
    pub fn from_env() -> anyhow::Result<Option<Self>> {
        match std::env::var(ENV_KEY_FILE) {
            Ok(path) if !path.is_empty() => Self::from_file(Path::new(&path)).map(Some),
            _ => Ok(None),
        }
    }

    fn from_file(path: &Path) -> anyhow::Result<Self> {
        let raw = std::fs::read(path)
            .map_err(|e| anyhow::anyhow!("{ENV_KEY_FILE} {}: {e}", path.display()))?;
        Self::new(raw.trim_ascii())
            .ok_or_else(|| anyhow::anyhow!("{ENV_KEY_FILE} {} is empty", path.display()))
    }

    /// The token for `land`, or `None` for a name that is not a land.
    pub fn mint(&self, land: &str) -> Option<String> {
        if !valid_land(land) {
            return None;
        }
        let msg = format!("{TOKEN_PREFIX}{land}");
        let tag = hmac::sign(&self.0, msg.as_bytes());
        Some(format!("{msg}.{}", B64URL.encode(tag.as_ref())))
    }

    /// The land a token was minted for, or `None` when it is not a land
    /// token or its MAC does not verify.
    pub fn verify(&self, token: &str) -> Option<LandPrincipal> {
        let rest = token.strip_prefix(TOKEN_PREFIX)?;
        let (land, mac) = rest.split_once('.')?;
        if !valid_land(land) {
            return None;
        }
        let mac = B64URL.decode(mac).ok()?;
        let msg = format!("{TOKEN_PREFIX}{land}");
        hmac::verify(&self.0, msg.as_bytes(), &mac).ok()?;
        Some(LandPrincipal { land: land.to_owned() })
    }
}

/// A land name is a DNS-label fragment: lowercase letters, digits and `-`.
fn valid_land(land: &str) -> bool {
    !land.is_empty()
        && land.len() <= MAX_LAND_LEN
        && land.bytes().all(|b| b.is_ascii_lowercase() || b.is_ascii_digit() || b == b'-')
}

/// Why a land principal's registration was refused. Every variant answers
/// 403 with the same body, so a tenant cannot probe fleet session ids or urls.
#[derive(Debug, Clone, PartialEq, Eq)]
pub enum Refusal {
    /// The registration named no session id.
    NoId,
    /// The metadata is not a JSON object.
    Metadata,
    /// `metadata.project` names another land or a fleet project.
    Project(String),
    /// `metadata.land` names another land.
    Land(String),
    /// `account_name` is not this land's purse account.
    Account(String),
    /// The session id belongs to a session outside this land.
    IdHeld,
    /// The url belongs to a session outside this land.
    UrlHeld,
    /// The upstream at the url reports another session id.
    UpstreamId(Option<String>),
    /// The upstream at the url reports an account outside this land.
    UpstreamAccount(String),
}

/// Hold a land principal's registration body to its land.
///
/// Returns the metadata to store, with `project` and [`LAND_BINDING_KEY`] set
/// to the land. The body may repeat its own land but may not name another,
/// and the only account it may report is its land's purse account.
pub fn bind_registration(
    principal: &LandPrincipal,
    id: Option<&str>,
    metadata: Option<serde_json::Value>,
    account_name: Option<&str>,
) -> Result<serde_json::Value, Refusal> {
    let land = principal.land.as_str();
    if id.is_none_or(str::is_empty) {
        return Err(Refusal::NoId);
    }
    let mut map = match metadata {
        None | Some(serde_json::Value::Null) => serde_json::Map::new(),
        Some(serde_json::Value::Object(map)) => map,
        Some(_) => return Err(Refusal::Metadata),
    };
    claim(&mut map, "project", land).map_err(Refusal::Project)?;
    claim(&mut map, LAND_BINDING_KEY, land).map_err(Refusal::Land)?;
    if let Some(account) = account_name.filter(|a| !a.is_empty()) {
        if land_of_account(account) != Some(land) {
            return Err(Refusal::Account(account.to_owned()));
        }
    }
    Ok(serde_json::Value::Object(map))
}

/// Set `map[key]` to `land`, refusing (with the value found) a value that
/// names anything else.
fn claim(
    map: &mut serde_json::Map<String, serde_json::Value>,
    key: &str,
    land: &str,
) -> Result<(), String> {
    match map.get(key) {
        None | Some(serde_json::Value::Null) => {}
        Some(serde_json::Value::String(v)) if v == land => {}
        Some(other) => return Err(other.to_string()),
    }
    map.insert(key.to_owned(), serde_json::Value::String(land.to_owned()));
    Ok(())
}

/// Check that the coop answering at a land registration's url is that
/// session: it reports the registered id, and no account but its land's.
///
/// A fleet coop reports its own id, which is a fleet session's, and a fleet
/// account once it has one; either refuses the registration, so a tenant
/// cannot put a fleet pod's url under its land and type into it through
/// coopmux.
pub fn check_upstream_identity(
    principal: &LandPrincipal,
    id: &str,
    health: &serde_json::Value,
) -> Result<(), Refusal> {
    let reported = health.get("session_id").and_then(|v| v.as_str());
    if reported != Some(id) {
        return Err(Refusal::UpstreamId(reported.map(str::to_owned)));
    }
    match health.get("account_name").and_then(|v| v.as_str()).filter(|a| !a.is_empty()) {
        Some(account) if account != land_account_name(&principal.land) => {
            Err(Refusal::UpstreamAccount(account.to_owned()))
        }
        _ => Ok(()),
    }
}

#[cfg(test)]
#[path = "land_principal_tests.rs"]
mod tests;
