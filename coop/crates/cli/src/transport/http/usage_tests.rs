// SPDX-License-Identifier: BUSL-1.1
// Copyright (c) 2026 Matt Baker <fimbaz@gmail.com>

use crate::test_support::{AnyhowExt, StoreBuilder, StoreCtx};
use crate::transport::build_router;
use crate::usage::UsageDelta;

/// GET /api/v1/session/usage returns zero counters initially.
#[tokio::test]
async fn usage_returns_zero_initially() -> anyhow::Result<()> {
    let StoreCtx { store, .. } = StoreBuilder::new().build();
    let app = build_router(store);
    let server = axum_test::TestServer::new(app).anyhow()?;

    let resp = server.get("/api/v1/session/usage").await;
    resp.assert_status_ok();
    let body: serde_json::Value = serde_json::from_str(&resp.text())?;
    assert_eq!(body["input_tokens"], 0);
    assert_eq!(body["output_tokens"], 0);
    assert_eq!(body["request_count"], 0);
    Ok(())
}

/// GET /api/v1/session/usage reflects accumulated deltas.
#[tokio::test]
async fn usage_reflects_accumulated_deltas() -> anyhow::Result<()> {
    let StoreCtx { store, .. } = StoreBuilder::new().build();

    store
        .usage
        .accumulate(UsageDelta {
            input_tokens: 100,
            output_tokens: 50,
            cache_creation_input_tokens: 10,
            cache_read_input_tokens: 20,
            duration_api_ms: Some(1200),
            response_cost_usd: 0.42,
        })
        .await;

    let app = build_router(store);
    let server = axum_test::TestServer::new(app).anyhow()?;

    let resp = server.get("/api/v1/session/usage").await;
    resp.assert_status_ok();
    let body: serde_json::Value = serde_json::from_str(&resp.text())?;
    assert_eq!(body["input_tokens"], 100);
    assert_eq!(body["output_tokens"], 50);
    assert_eq!(body["cache_read_tokens"], 20);
    assert_eq!(body["cache_write_tokens"], 10);
    assert_eq!(body["request_count"], 1);
    assert_eq!(body["total_api_ms"], 1200);
    // context_tokens = input + cache_read + cache_creation (latest turn level).
    assert_eq!(body["context_tokens"], 130);
    assert_eq!(body["total_cost_usd"], 0.42);
    Ok(())
}

/// GET /api/v1/agent surfaces the live request_count ("turns") so the
/// controller can mirror it onto the agent bead as live_turns (ka-0000006449).
#[tokio::test]
async fn agent_body_includes_request_count() -> anyhow::Result<()> {
    let StoreCtx { store, .. } = StoreBuilder::new().build();

    // Two round-trips → request_count == 2 on the /api/v1/agent snapshot.
    for _ in 0..2 {
        store
            .usage
            .accumulate(UsageDelta {
                input_tokens: 10,
                output_tokens: 5,
                cache_creation_input_tokens: 0,
                cache_read_input_tokens: 0,
                duration_api_ms: Some(100),
                response_cost_usd: 0.01,
            })
            .await;
    }

    let app = build_router(store);
    let server = axum_test::TestServer::new(app).anyhow()?;

    let resp = server.get("/api/v1/agent").await;
    resp.assert_status_ok();
    let body: serde_json::Value = serde_json::from_str(&resp.text())?;
    assert_eq!(body["request_count"], 2, "agent body: {body}");
    Ok(())
}
