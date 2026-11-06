// SPDX-License-Identifier: BUSL-1.1
// Copyright (c) 2026 Matt Baker <fimbaz@gmail.com>

//! Land (project) scope for OIDC callers (gd-DcKLFjZuyl, boundary rec
//! gd-uYNbyW4a9Z §6 step 8).
//!
//! Before this, any token the OIDC verifier accepted could attach to every
//! terminal. A land tenant holds a Keycloak token too (its `/project-<land>`
//! group carries the `gardenio-project-member` realm role), so a tenant could
//! read or type into another land's agents.
//!
//! The rule mirrors kbeads' scoped-principal engine
//! (`kbeads/internal/server/scoped_principal.go`) so the two walls read the
//! same:
//!
//! * An OIDC caller that is not `gardenio-admin` and holds
//!   `gardenio-project-member` is *scoped*. Its lands come only from the
//!   token's verified groups claim (`/project-<land>`), never from a bead, and
//!   a missing or empty claim admits nothing.
//! * Everyone else (static token, edge envelope, admin and operator OIDC
//!   tokens) is unrestricted, as before.
//!
//! A scoped caller sees only sessions whose registration metadata carries
//! `project: <land>` for one of its lands. A session with no project is
//! nobody's land and is hidden. On top of the land check a scoped caller is
//! capped at the operator tier: it may watch and type into its own land's
//! terminals, but admin-reserved actions (raw bytes, uploads, signals,
//! lifecycle, launch/register/deregister, credentials) stay denied, and the
//! multiplexed feeds (`/ws/mux`, `/sse/mux`), which subscribe across
//! sessions, are refused outright.

use crate::state::MuxState;
use crate::transport::oidc::OidcClaims;

/// Realm role that grants full authority and is never scoped.
const ROLE_ADMIN: &str = "gardenio-admin";
/// Realm role every `/project-*` Keycloak group carries
/// (`templates/keycloak/groups.yaml`).
const ROLE_PROJECT_MEMBER: &str = "gardenio-project-member";
/// Keycloak group-name prefix that names a land (kbeads `groupProjectPrefix`).
const GROUP_PROJECT_PREFIX: &str = "project-";
/// Highest edge-role tier a scoped caller may exercise.
const SCOPED_MAX_ROLE: &str = "gardenio-operator";

/// Which sessions a caller may see and attach to.
#[derive(Debug, Clone, PartialEq, Eq)]
pub enum SessionScope {
    /// Every session (static token, edge envelope, admin/operator OIDC).
    Unrestricted,
    /// Only sessions whose `metadata.project` is one of these lands. Empty
    /// admits nothing.
    Projects(Vec<String>),
}

impl SessionScope {
    /// Resolve the scope of a verified OIDC token (kbeads
    /// `isScopedPrincipal` + `scopeFromContext`).
    pub fn from_claims(claims: &OidcClaims) -> Self {
        let has = |role: &str| claims.roles.iter().any(|r| r == role);
        if has(ROLE_ADMIN) || !has(ROLE_PROJECT_MEMBER) {
            return Self::Unrestricted;
        }
        let mut projects = groups_to_projects(&claims.groups);
        projects.sort();
        projects.dedup();
        Self::Projects(projects)
    }

    pub fn is_scoped(&self) -> bool {
        matches!(self, Self::Projects(_))
    }

    /// Whether a session registered with this metadata is inside the scope.
    pub fn allows_metadata(&self, metadata: &serde_json::Value) -> bool {
        match self {
            Self::Unrestricted => true,
            Self::Projects(projects) => metadata
                .get("project")
                .and_then(|v| v.as_str())
                .is_some_and(|project| projects.iter().any(|p| p == project)),
        }
    }

    /// Whether the session `id` exists and is inside the scope. An unknown
    /// session and an out-of-scope one answer the same, so a scoped caller
    /// cannot probe other lands' session ids.
    pub async fn allows_session(&self, state: &MuxState, id: &str) -> bool {
        if !self.is_scoped() {
            return true;
        }
        let sessions = state.sessions.read().await;
        sessions.get(id).is_some_and(|entry| self.allows_metadata(&entry.metadata))
    }

    /// Whether this scope may exercise an action that needs `required` (an
    /// edge role name from [`crate::transport::auth::edge_required_role`] or
    /// [`crate::transport::auth::edge_session_frame_required_role`]).
    pub fn allows_role(&self, required: &str) -> bool {
        !self.is_scoped() || role_rank(required) <= role_rank(SCOPED_MAX_ROLE)
    }
}

/// Map Keycloak group paths (`/project-alpha`) to land names (`alpha`), as
/// kbeads `groupsToProjects` does.
pub fn groups_to_projects(groups: &[String]) -> Vec<String> {
    groups
        .iter()
        .filter_map(|g| g.strip_prefix('/').unwrap_or(g).strip_prefix(GROUP_PROJECT_PREFIX))
        .filter(|name| !name.is_empty())
        .map(str::to_owned)
        .collect()
}

/// The session id a scoped caller's HTTP request targets, or `None` when the
/// route is not one a scoped caller may use at all.
///
/// Scoped callers get the session list and the per-session routes; every other
/// route (credentials, launch config, version, the dashboard) is
/// operator-facing and refused.
pub(crate) enum ScopedRoute<'a> {
    /// `GET /api/v1/sessions` — the handler filters the list.
    List,
    /// `/api/v1/sessions/{id}/...`.
    Session(&'a str),
}

pub(crate) fn scoped_route(path: &str) -> Option<ScopedRoute<'_>> {
    if path == "/api/v1/sessions" {
        return Some(ScopedRoute::List);
    }
    let rest = path.strip_prefix("/api/v1/sessions/")?;
    let id = rest.split('/').next().unwrap_or_default();
    // `/api/v1/sessions/{id}` alone is DELETE (deregister), which the role cap
    // already refuses; require a sub-route so a new bare-id verb cannot slip in.
    if id.is_empty() || id == "launch" || !rest[id.len()..].starts_with('/') {
        return None;
    }
    Some(ScopedRoute::Session(id))
}

fn role_rank(role: &str) -> u8 {
    match role {
        "gardenio-viewer" => 1,
        "gardenio-operator" => 2,
        "gardenio-admin" => 3,
        // Unknown requirements rank above admin so they are never in reach.
        _ => u8::MAX,
    }
}

#[cfg(test)]
#[path = "project_scope_tests.rs"]
mod tests;
