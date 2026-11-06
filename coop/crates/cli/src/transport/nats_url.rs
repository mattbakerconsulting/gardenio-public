// SPDX-License-Identifier: BUSL-1.1
// Copyright (c) 2026 Matt Baker <fimbaz@gmail.com>

//! Pure URL host helpers for the NATS supervisor's thaw reconnect (bug
//! ka-0000001492). Split out of `nats_supervisor.rs` so the supervisor stays
//! focused on connection orchestration and these string-only transforms are
//! unit-tested in isolation.
//!
//! We do targeted string rewrites rather than pull in the `url` crate: the
//! transport URLs coop dials are always `nats://<host>:<port>` (the controller
//! emits `nats://gardenio-nats:4222`), so swapping the authority's host is exact
//! and avoids a new direct dependency.

use std::net::IpAddr;

/// Replace the host of a `nats://host:port` (or bare `host:port`) URL with a
/// pre-resolved IP literal, preserving the scheme, port, and any path/query.
/// IPv6 literals are bracketed per RFC 3986. Returns `None` when the URL has no
/// recognizable host segment to swap (in which case the caller keeps the
/// original URL — the IP rewrite is a best-effort accelerator, never a
/// correctness dependency).
pub(super) fn rewrite_host_to_ip(url: &str, ip: IpAddr) -> Option<String> {
    // Split off an optional scheme (`scheme://`).
    let (scheme, rest) = match url.split_once("://") {
        Some((s, r)) => (Some(s), r),
        None => (None, url),
    };
    // The authority runs until the first `/`, `?`, or `#`; keep the tail.
    let auth_end = rest.find(['/', '?', '#']).unwrap_or(rest.len());
    let (authority, tail) = rest.split_at(auth_end);
    if authority.is_empty() {
        return None;
    }
    // Strip optional `user@` userinfo, preserve it on the rewrite.
    let (userinfo, hostport) = match authority.rsplit_once('@') {
        Some((u, hp)) => (Some(u), hp),
        None => (None, authority),
    };
    // Split host from `:port`. The host may itself be a bracketed IPv6 literal.
    let port = if hostport.starts_with('[') {
        // `[v6]:port` → port is whatever follows the closing bracket.
        hostport.rsplit_once("]:").map(|(_, p)| p)
    } else {
        hostport.rsplit_once(':').map(|(_, p)| p)
    };
    let host_literal = match ip {
        IpAddr::V4(v4) => v4.to_string(),
        IpAddr::V6(v6) => format!("[{v6}]"),
    };
    let new_hostport = match port {
        Some(p) => format!("{host_literal}:{p}"),
        None => host_literal,
    };
    let mut out = String::new();
    if let Some(s) = scheme {
        out.push_str(s);
        out.push_str("://");
    }
    if let Some(u) = userinfo {
        out.push_str(u);
        out.push('@');
    }
    out.push_str(&new_hostport);
    out.push_str(tail);
    Some(out)
}

/// Extract the `host` from a `nats://host:port` (or bare `host:port`) URL for a
/// DNS pre-resolve. Returns `None` when the host is already an IP literal
/// (nothing to resolve) or cannot be isolated.
pub(super) fn host_of_url(url: &str) -> Option<String> {
    let rest = url.split_once("://").map(|(_, r)| r).unwrap_or(url);
    let auth_end = rest.find(['/', '?', '#']).unwrap_or(rest.len());
    let authority = &rest[..auth_end];
    let hostport = authority.rsplit_once('@').map(|(_, hp)| hp).unwrap_or(authority);
    // A bracketed `[…]` authority is an IPv6 literal — already an IP, nothing to
    // pre-resolve.
    if hostport.starts_with('[') {
        return None;
    }
    let host = hostport.rsplit_once(':').map(|(h, _)| h).unwrap_or(hostport);
    if host.is_empty() || host.parse::<IpAddr>().is_ok() {
        return None; // empty or already a bare IP literal
    }
    Some(host.to_owned())
}

#[cfg(test)]
#[path = "nats_url_tests.rs"]
mod tests;
