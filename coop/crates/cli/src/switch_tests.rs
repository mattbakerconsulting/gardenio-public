// SPDX-License-Identifier: BUSL-1.1
// Copyright (c) 2026 Matt Baker <fimbaz@gmail.com>

use super::*;

#[test]
fn switch_request_defaults() {
    let req: SwitchRequest = serde_json::from_str("{}").unwrap_or_else(|e| panic!("{e}"));
    assert!(req.credentials.is_none());
    assert!(!req.force);
}

#[test]
fn switch_request_round_trips() {
    let req = SwitchRequest {
        credentials: Some([("ANTHROPIC_API_KEY".to_owned(), "sk-test".to_owned())].into()),
        force: true,
        profile: None,
    };
    let json = serde_json::to_string(&req).unwrap_or_else(|e| panic!("{e}"));
    let decoded: SwitchRequest = serde_json::from_str(&json).unwrap_or_else(|e| panic!("{e}"));
    assert!(decoded.force);
    assert_eq!(
        decoded.credentials.as_ref().and_then(|c| c.get("ANTHROPIC_API_KEY")).map(|s| s.as_str()),
        Some("sk-test"),
    );
}
