// SPDX-License-Identifier: BUSL-1.1
// Copyright (c) 2026 Matt Baker <fimbaz@gmail.com>

use axum::http::StatusCode;

use crate::switch::SwitchRequest;
use crate::test_support::{AnyhowExt, StoreBuilder, StoreCtx};
use crate::transport::build_router;

/// POST /api/v1/session/switch returns 202 Accepted.
#[tokio::test]
async fn switch_returns_202() -> anyhow::Result<()> {
    let StoreCtx { store: state, switch_rx: _switch_rx, .. } =
        StoreBuilder::new().child_pid(1234).build();
    let app = build_router(state);
    let server = axum_test::TestServer::new(app).anyhow()?;

    let resp =
        server.post("/api/v1/session/switch").json(&serde_json::json!({"force": true})).await;
    resp.assert_status(StatusCode::ACCEPTED);
    Ok(())
}

/// Second switch while first is pending returns 409.
#[tokio::test]
async fn switch_rejects_when_in_progress() -> anyhow::Result<()> {
    let StoreCtx { store: state, switch_rx: _switch_rx, .. } =
        StoreBuilder::new().child_pid(1234).build();
    // Pre-fill the switch channel so next try_send returns Full.
    state
        .switch
        .switch_tx
        .try_send(SwitchRequest { credentials: None, force: false, profile: None })
        .ok();

    let app = build_router(state);
    let server = axum_test::TestServer::new(app).anyhow()?;

    let resp =
        server.post("/api/v1/session/switch").json(&serde_json::json!({"force": true})).await;
    resp.assert_status(StatusCode::CONFLICT);
    let body: serde_json::Value = serde_json::from_str(&resp.text())?;
    assert_eq!(body["error"]["code"], "SWITCH_IN_PROGRESS");
    Ok(())
}
