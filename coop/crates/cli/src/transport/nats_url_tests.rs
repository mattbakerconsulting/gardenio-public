// SPDX-License-Identifier: BUSL-1.1
// Copyright (c) 2026 Matt Baker <fimbaz@gmail.com>

//! Unit tests for the pure NATS URL host helpers (bug ka-0000001492).

use super::*;

#[test]
fn host_of_url_extracts_bare_service_name() {
    // The exact prod endpoint the controller emits.
    assert_eq!(host_of_url("nats://gardenio-nats:4222").as_deref(), Some("gardenio-nats"));
    // No scheme, with port.
    assert_eq!(host_of_url("gardenio-nats:4222").as_deref(), Some("gardenio-nats"));
    // No port.
    assert_eq!(host_of_url("nats://gardenio-nats").as_deref(), Some("gardenio-nats"));
    // FQDN.
    assert_eq!(
        host_of_url("nats://gardenio-nats.ns.svc.cluster.local:4222").as_deref(),
        Some("gardenio-nats.ns.svc.cluster.local")
    );
    // With trailing path.
    assert_eq!(host_of_url("wss://nats.example.com/nats").as_deref(), Some("nats.example.com"));
}

#[test]
fn host_of_url_returns_none_for_ip_literals() {
    // Already an IPv4 literal → nothing to pre-resolve.
    assert_eq!(host_of_url("nats://10.0.0.5:4222"), None);
    // Loopback IPv4.
    assert_eq!(host_of_url("ws://127.0.0.1:40000/"), None);
    // Bracketed IPv6 literal.
    assert_eq!(host_of_url("nats://[2001:db8::1]:4222"), None);
}

#[test]
fn rewrite_host_to_ip_swaps_host_preserving_scheme_port_path() -> anyhow::Result<()> {
    let ip: std::net::IpAddr = "10.96.1.7".parse()?;
    // The core prod case: bare name → ClusterIP, scheme + port preserved.
    assert_eq!(
        rewrite_host_to_ip("nats://gardenio-nats:4222", ip).as_deref(),
        Some("nats://10.96.1.7:4222")
    );
    // FQDN host with port.
    assert_eq!(
        rewrite_host_to_ip("nats://gardenio-nats.ns.svc.cluster.local:4222", ip).as_deref(),
        Some("nats://10.96.1.7:4222")
    );
    // Scheme + host + path (no port).
    assert_eq!(
        rewrite_host_to_ip("wss://nats.example.com/nats", ip).as_deref(),
        Some("wss://10.96.1.7/nats")
    );
    // No scheme.
    assert_eq!(rewrite_host_to_ip("gardenio-nats:4222", ip).as_deref(), Some("10.96.1.7:4222"));
    Ok(())
}

#[test]
fn rewrite_host_to_ip_brackets_ipv6() -> anyhow::Result<()> {
    let ip: std::net::IpAddr = "fd00::1234".parse()?;
    assert_eq!(
        rewrite_host_to_ip("nats://gardenio-nats:4222", ip).as_deref(),
        Some("nats://[fd00::1234]:4222")
    );
    Ok(())
}

#[test]
fn rewrite_host_to_ip_preserves_userinfo() -> anyhow::Result<()> {
    let ip: std::net::IpAddr = "10.0.0.9".parse()?;
    assert_eq!(
        rewrite_host_to_ip("nats://user@gardenio-nats:4222", ip).as_deref(),
        Some("nats://user@10.0.0.9:4222")
    );
    Ok(())
}
