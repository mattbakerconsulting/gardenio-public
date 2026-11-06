// SPDX-License-Identifier: BUSL-1.1
// Copyright (c) 2026 Matt Baker <fimbaz@gmail.com>

use axum::http::StatusCode;

use crate::test_support::{AnyhowExt, StoreBuilder, StoreCtx};
use crate::transport::build_router;

/// POST /api/v1/session/profiles registers profiles.
#[tokio::test]
async fn register_profiles_returns_count() -> anyhow::Result<()> {
    let StoreCtx { store, .. } = StoreBuilder::new().build();
    let app = build_router(store);
    let server = axum_test::TestServer::new(app).anyhow()?;

    let resp = server
        .post("/api/v1/session/profiles")
        .json(&serde_json::json!({
            "profiles": [
                { "name": "alice", "credentials": { "API_KEY": "key-a" } },
                { "name": "bob", "credentials": { "API_KEY": "key-b" } },
            ]
        }))
        .await;
    resp.assert_status(StatusCode::OK);
    let body: serde_json::Value = serde_json::from_str(&resp.text())?;
    assert_eq!(body["registered"], 2);
    Ok(())
}

/// GET /api/v1/session/profiles lists registered profiles.
#[tokio::test]
async fn list_profiles_returns_registered() -> anyhow::Result<()> {
    let StoreCtx { store, .. } = StoreBuilder::new().build();
    // Pre-register profiles directly.
    store
        .profile
        .register(vec![
            crate::profile::ProfileEntry {
                name: "alice".to_owned(),
                credentials: [("API_KEY".to_owned(), "key-a".to_owned())].into(),
                usable: None,
            },
            crate::profile::ProfileEntry {
                name: "bob".to_owned(),
                credentials: [("API_KEY".to_owned(), "key-b".to_owned())].into(),
                usable: None,
            },
        ])
        .await;

    let app = build_router(store);
    let server = axum_test::TestServer::new(app).anyhow()?;

    let resp = server.get("/api/v1/session/profiles").await;
    resp.assert_status(StatusCode::OK);
    let body: serde_json::Value = serde_json::from_str(&resp.text())?;
    assert_eq!(body["profiles"].as_array().map(|a| a.len()), Some(2));
    assert_eq!(body["active_profile"], "alice");
    assert_eq!(body["profiles"][0]["status"], "active");
    assert_eq!(body["profiles"][1]["status"], "available");
    assert_eq!(body["mode"], "auto");
    Ok(())
}

/// GET/PUT /api/v1/session/profiles/mode manages rotation mode.
#[tokio::test]
async fn profile_mode_get_put() -> anyhow::Result<()> {
    let StoreCtx { store, .. } = StoreBuilder::new().build();
    let app = build_router(store);
    let server = axum_test::TestServer::new(app).anyhow()?;

    // Default mode is auto.
    let resp = server.get("/api/v1/session/profiles/mode").await;
    resp.assert_status(StatusCode::OK);
    let body: serde_json::Value = serde_json::from_str(&resp.text())?;
    assert_eq!(body["mode"], "auto");

    // Set to manual.
    let resp = server
        .put("/api/v1/session/profiles/mode")
        .json(&serde_json::json!({ "mode": "manual" }))
        .await;
    resp.assert_status(StatusCode::OK);
    let body: serde_json::Value = serde_json::from_str(&resp.text())?;
    assert_eq!(body["mode"], "manual");

    // Verify it persisted.
    let resp = server.get("/api/v1/session/profiles/mode").await;
    let body: serde_json::Value = serde_json::from_str(&resp.text())?;
    assert_eq!(body["mode"], "manual");

    // Invalid mode returns error.
    let resp = server
        .put("/api/v1/session/profiles/mode")
        .json(&serde_json::json!({ "mode": "invalid" }))
        .await;
    resp.assert_status(StatusCode::BAD_REQUEST);
    Ok(())
}
