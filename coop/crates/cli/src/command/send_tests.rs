// SPDX-License-Identifier: BUSL-1.1
// Copyright (c) 2026 Matt Baker <fimbaz@gmail.com>

use super::*;

#[test]
fn invalid_json_body_returns_2() {
    assert_eq!(send("http://localhost:0", Some("not json")), 2);
}

#[test]
fn missing_brace_returns_2() {
    assert_eq!(send("http://localhost:0", Some("{status")), 2);
}

#[test]
fn connection_refused_returns_1() {
    // Tests don't go through main(), so install the crypto provider here.
    let _ = rustls::crypto::ring::default_provider().install_default();
    // Port 0 should refuse connections.
    assert_eq!(send("http://127.0.0.1:1", None), 1);
}
