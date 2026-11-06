// SPDX-License-Identifier: BUSL-1.1
// Copyright (c) 2026 Matt Baker <fimbaz@gmail.com>

use axum::http::StatusCode;
use base64::Engine;

use crate::driver::AgentState;
use crate::start::{StartConfig, StartEventConfig};
use crate::stop::{StopConfig, StopMode};
use crate::test_support::{AnyhowExt, StoreBuilder, StoreCtx};
use crate::transport::build_router;

fn stop_state(config: StopConfig) -> StoreCtx {
    StoreBuilder::new().child_pid(1234).agent_state(AgentState::Working).stop_config(config).build()
}

fn start_state(config: StartConfig) -> StoreCtx {
    StoreBuilder::new()
        .child_pid(1234)
        .agent_state(AgentState::Working)
        .start_config(config)
        .build()
}

// -- Stop hook tests ----------------------------------------------------------

#[tokio::test]
async fn hooks_stop_allow_mode_returns_empty() -> anyhow::Result<()> {
    let StoreCtx { store: state, .. } = stop_state(StopConfig::default());
    let app = build_router(state);
    let server = axum_test::TestServer::new(app).anyhow()?;

    let resp = server
        .post("/api/v1/hooks/stop")
        .json(&serde_json::json!({"event": "stop", "data": {"stop_hook_active": false}}))
        .await;
    resp.assert_status(StatusCode::OK);
    let body: serde_json::Value = serde_json::from_str(&resp.text())?;
    // Allow mode returns empty object (no decision field).
    assert!(body.get("decision").is_none());
    Ok(())
}

#[tokio::test]
async fn hooks_stop_auto_mode_blocks_without_signal() -> anyhow::Result<()> {
    let config = StopConfig {
        mode: StopMode::Auto,
        prompt: Some("Finish work first.".to_owned()),
        schema: None,
    };
    let StoreCtx { store: state, .. } = stop_state(config);
    let app = build_router(state);
    let server = axum_test::TestServer::new(app).anyhow()?;

    let resp = server
        .post("/api/v1/hooks/stop")
        .json(&serde_json::json!({"event": "stop", "data": {"stop_hook_active": false}}))
        .await;
    resp.assert_status(StatusCode::OK);
    let body: serde_json::Value = serde_json::from_str(&resp.text())?;
    assert_eq!(body["decision"], "block");
    assert!(body["reason"].as_str().unwrap_or("").contains("Finish work first."));
    Ok(())
}

#[tokio::test]
async fn hooks_stop_auto_mode_allows_after_signal() -> anyhow::Result<()> {
    let config = StopConfig { mode: StopMode::Auto, prompt: None, schema: None };
    let StoreCtx { store: state, .. } = stop_state(config);
    let app = build_router(state.clone());
    let server = axum_test::TestServer::new(app).anyhow()?;

    // Send a signal first.
    let resp =
        server.post("/api/v1/stop/resolve").json(&serde_json::json!({"status": "done"})).await;
    resp.assert_status(StatusCode::OK);

    // Now stop should be allowed.
    let resp = server
        .post("/api/v1/hooks/stop")
        .json(&serde_json::json!({"event": "stop", "data": {"stop_hook_active": false}}))
        .await;
    resp.assert_status(StatusCode::OK);
    let body: serde_json::Value = serde_json::from_str(&resp.text())?;
    assert!(body.get("decision").is_none(), "should allow after signal");
    Ok(())
}

#[tokio::test]
async fn hooks_stop_safety_valve_always_allows() -> anyhow::Result<()> {
    let config = StopConfig { mode: StopMode::Auto, prompt: None, schema: None };
    let StoreCtx { store: state, .. } = stop_state(config);
    let app = build_router(state);
    let server = axum_test::TestServer::new(app).anyhow()?;

    // stop_hook_active = true => must allow
    let resp = server
        .post("/api/v1/hooks/stop")
        .json(&serde_json::json!({"event": "stop", "data": {"stop_hook_active": true}}))
        .await;
    resp.assert_status(StatusCode::OK);
    let body: serde_json::Value = serde_json::from_str(&resp.text())?;
    assert!(body.get("decision").is_none(), "safety valve must allow");
    Ok(())
}

#[tokio::test]
async fn hooks_stop_unrecoverable_error_allows() -> anyhow::Result<()> {
    let config = StopConfig { mode: StopMode::Auto, prompt: None, schema: None };
    let StoreCtx { store: state, .. } = stop_state(config);
    // Set unrecoverable error state.
    *state.driver.error.write().await = Some(crate::transport::state::ErrorInfo {
        detail: "invalid api key".to_owned(),
        category: crate::driver::ErrorCategory::Unauthorized,
    });

    let app = build_router(state);
    let server = axum_test::TestServer::new(app).anyhow()?;

    let resp = server
        .post("/api/v1/hooks/stop")
        .json(&serde_json::json!({"event": "stop", "data": {"stop_hook_active": false}}))
        .await;
    resp.assert_status(StatusCode::OK);
    let body: serde_json::Value = serde_json::from_str(&resp.text())?;
    assert!(body.get("decision").is_none(), "unrecoverable error should allow");
    Ok(())
}

#[tokio::test]
async fn resolve_stop_stores_body() -> anyhow::Result<()> {
    let StoreCtx { store: state, .. } = stop_state(StopConfig::default());
    let app = build_router(state.clone());
    let server = axum_test::TestServer::new(app).anyhow()?;

    let resp = server
        .post("/api/v1/stop/resolve")
        .json(&serde_json::json!({"status": "complete", "notes": "all good"}))
        .await;
    resp.assert_status(StatusCode::OK);
    let body = resp.text();
    assert!(body.contains("\"accepted\":true"));

    // Check that signal flag is set.
    assert!(state.stop.signaled.load(std::sync::atomic::Ordering::Acquire));
    // Check that signal body is stored.
    let stored = state.stop.signal_body.read().await;
    let stored_val = stored.as_ref().expect("signal body should be stored");
    assert_eq!(stored_val["status"], "complete");
    Ok(())
}

#[tokio::test]
async fn get_stop_config_returns_current() -> anyhow::Result<()> {
    let config =
        StopConfig { mode: StopMode::Auto, prompt: Some("test prompt".to_owned()), schema: None };
    let StoreCtx { store: state, .. } = stop_state(config);
    let app = build_router(state);
    let server = axum_test::TestServer::new(app).anyhow()?;

    let resp = server.get("/api/v1/config/stop").await;
    resp.assert_status(StatusCode::OK);
    let body: serde_json::Value = serde_json::from_str(&resp.text())?;
    assert_eq!(body["mode"], "auto");
    assert_eq!(body["prompt"], "test prompt");
    Ok(())
}

#[tokio::test]
async fn put_stop_config_updates() -> anyhow::Result<()> {
    let StoreCtx { store: state, .. } = stop_state(StopConfig::default());
    let app = build_router(state.clone());
    let server = axum_test::TestServer::new(app).anyhow()?;

    // Default is allow mode.
    let resp = server.get("/api/v1/config/stop").await;
    let body: serde_json::Value = serde_json::from_str(&resp.text())?;
    assert_eq!(body["mode"], "allow");

    // Update to signal mode.
    let resp = server
        .put("/api/v1/config/stop")
        .json(&serde_json::json!({"mode": "auto", "prompt": "Wait for signal"}))
        .await;
    resp.assert_status(StatusCode::OK);
    let body = resp.text();
    assert!(body.contains("\"updated\":true"));

    // Verify the update.
    let resp = server.get("/api/v1/config/stop").await;
    let body: serde_json::Value = serde_json::from_str(&resp.text())?;
    assert_eq!(body["mode"], "auto");
    assert_eq!(body["prompt"], "Wait for signal");
    Ok(())
}

#[tokio::test]
async fn hooks_stop_emits_stop_events() -> anyhow::Result<()> {
    let config = StopConfig { mode: StopMode::Auto, prompt: None, schema: None };
    let StoreCtx { store: state, .. } = stop_state(config);
    let mut stop_rx = state.stop.stop_tx.subscribe();

    let app = build_router(state);
    let server = axum_test::TestServer::new(app).anyhow()?;

    // First call should block.
    server
        .post("/api/v1/hooks/stop")
        .json(&serde_json::json!({"event": "stop", "data": {"stop_hook_active": false}}))
        .await;

    let event = stop_rx.try_recv()?;
    assert_eq!(event.r#type.as_str(), "blocked");
    assert_eq!(event.seq, 0);
    Ok(())
}

#[tokio::test]
async fn signal_consumed_after_stop_check() -> anyhow::Result<()> {
    let config = StopConfig { mode: StopMode::Auto, prompt: None, schema: None };
    let StoreCtx { store: state, .. } = stop_state(config);
    let app = build_router(state);
    let server = axum_test::TestServer::new(app).anyhow()?;

    // Signal, then check stop — should allow.
    server.post("/api/v1/stop/resolve").json(&serde_json::json!({"status": "done"})).await;
    let resp = server
        .post("/api/v1/hooks/stop")
        .json(&serde_json::json!({"event": "stop", "data": {"stop_hook_active": false}}))
        .await;
    let body: serde_json::Value = serde_json::from_str(&resp.text())?;
    assert!(body.get("decision").is_none(), "first check after signal should allow");

    // Second stop check should block again (signal was consumed).
    let resp = server
        .post("/api/v1/hooks/stop")
        .json(&serde_json::json!({"event": "stop", "data": {"stop_hook_active": false}}))
        .await;
    let body: serde_json::Value = serde_json::from_str(&resp.text())?;
    assert_eq!(body["decision"], "block", "second check should block (signal consumed)");
    Ok(())
}

#[tokio::test]
async fn auth_exempt_for_hooks_stop_and_resolve() -> anyhow::Result<()> {
    let StoreCtx { store: state, .. } =
        StoreBuilder::new().child_pid(1234).auth_token("secret-token").build();

    let app = build_router(state);
    let server = axum_test::TestServer::new(app).anyhow()?;

    // Hooks stop should work without auth.
    let resp = server
        .post("/api/v1/hooks/stop")
        .json(&serde_json::json!({"event": "stop", "data": {"stop_hook_active": false}}))
        .await;
    resp.assert_status(StatusCode::OK);

    // Resolve stop should work without auth.
    let resp = server.post("/api/v1/stop/resolve").json(&serde_json::json!({"ok": true})).await;
    resp.assert_status(StatusCode::OK);

    // Start hook should work without auth.
    let resp = server
        .post("/api/v1/hooks/start")
        .json(&serde_json::json!({"event": "start", "data": {}}))
        .await;
    resp.assert_status(StatusCode::OK);

    // But other endpoints should still require auth.
    let resp = server.get("/api/v1/screen").await;
    resp.assert_status(StatusCode::UNAUTHORIZED);

    Ok(())
}

#[tokio::test]
async fn resolve_stop_rejects_invalid_body() -> anyhow::Result<()> {
    use std::collections::BTreeMap;

    use crate::stop::{StopSchema, StopSchemaField};

    let mut fields = BTreeMap::new();
    fields.insert(
        "status".to_owned(),
        StopSchemaField {
            required: true,
            r#enum: Some(vec!["done".to_owned(), "error".to_owned()]),
            descriptions: None,
            description: None,
        },
    );
    let config =
        StopConfig { mode: StopMode::Auto, prompt: None, schema: Some(StopSchema { fields }) };
    let StoreCtx { store: state, .. } = stop_state(config);
    let app = build_router(state);
    let server = axum_test::TestServer::new(app).anyhow()?;

    // Send a body that fails schema validation.
    let resp =
        server.post("/api/v1/stop/resolve").json(&serde_json::json!({"status": "bogus"})).await;
    resp.assert_status(StatusCode::UNPROCESSABLE_ENTITY);
    let body: serde_json::Value = serde_json::from_str(&resp.text())?;
    assert!(body["error"].as_str().unwrap_or("").contains("bogus"));
    Ok(())
}

#[tokio::test]
async fn hooks_stop_gate_mode_blocks() -> anyhow::Result<()> {
    let config = StopConfig {
        mode: StopMode::Gate,
        prompt: Some("Waiting for orchestrator decision.".to_owned()),
        schema: None,
    };
    let StoreCtx { store: state, .. } = stop_state(config);
    let app = build_router(state);
    let server = axum_test::TestServer::new(app).anyhow()?;

    let resp = server
        .post("/api/v1/hooks/stop")
        .json(&serde_json::json!({"event": "stop", "data": {"stop_hook_active": false}}))
        .await;
    resp.assert_status(StatusCode::OK);
    let body: serde_json::Value = serde_json::from_str(&resp.text())?;
    assert_eq!(body["decision"], "block");
    Ok(())
}

#[tokio::test]
async fn hooks_stop_gate_mode_prompt_verbatim() -> anyhow::Result<()> {
    let config = StopConfig {
        mode: StopMode::Gate,
        prompt: Some("Run bd decision create.".to_owned()),
        schema: None,
    };
    let StoreCtx { store: state, .. } = stop_state(config);
    let app = build_router(state);
    let server = axum_test::TestServer::new(app).anyhow()?;

    let resp = server
        .post("/api/v1/hooks/stop")
        .json(&serde_json::json!({"event": "stop", "data": {"stop_hook_active": false}}))
        .await;
    resp.assert_status(StatusCode::OK);
    let body: serde_json::Value = serde_json::from_str(&resp.text())?;
    assert_eq!(body["decision"], "block");
    // Gate mode returns prompt verbatim — no `coop send` commands appended.
    assert_eq!(body["reason"], "Run bd decision create.");
    Ok(())
}

// -- Start hook tests ---------------------------------------------------------

#[tokio::test]
async fn hooks_start_empty_config_returns_empty() -> anyhow::Result<()> {
    let StoreCtx { store: state, .. } = start_state(StartConfig::default());
    let app = build_router(state);
    let server = axum_test::TestServer::new(app).anyhow()?;

    let resp = server
        .post("/api/v1/hooks/start")
        .json(&serde_json::json!({"event": "start", "data": {}}))
        .await;
    resp.assert_status(StatusCode::OK);
    let body = resp.text();
    assert!(body.is_empty(), "empty config should return empty body: {body}");
    Ok(())
}

#[tokio::test]
async fn hooks_start_text_returns_base64_script() -> anyhow::Result<()> {
    let config = StartConfig { text: Some("hello context".to_owned()), ..Default::default() };
    let StoreCtx { store: state, .. } = start_state(config);
    let app = build_router(state);
    let server = axum_test::TestServer::new(app).anyhow()?;

    let resp = server
        .post("/api/v1/hooks/start")
        .json(&serde_json::json!({"event": "start", "data": {}}))
        .await;
    resp.assert_status(StatusCode::OK);
    let body = resp.text();
    assert!(body.contains("base64 -d"), "should contain base64 decode: {body}");
    assert!(body.contains("printf"), "should contain printf: {body}");
    Ok(())
}

#[tokio::test]
async fn hooks_start_shell_returns_commands() -> anyhow::Result<()> {
    let config = StartConfig {
        shell: vec!["echo one".to_owned(), "echo two".to_owned()],
        ..Default::default()
    };
    let StoreCtx { store: state, .. } = start_state(config);
    let app = build_router(state);
    let server = axum_test::TestServer::new(app).anyhow()?;

    let resp = server
        .post("/api/v1/hooks/start")
        .json(&serde_json::json!({"event": "start", "data": {}}))
        .await;
    resp.assert_status(StatusCode::OK);
    let body = resp.text();
    assert_eq!(body, "echo one\necho two");
    Ok(())
}

#[tokio::test]
async fn hooks_start_text_and_shell_combined() -> anyhow::Result<()> {
    let config = StartConfig {
        text: Some("ctx".to_owned()),
        shell: vec!["echo done".to_owned()],
        ..Default::default()
    };
    let StoreCtx { store: state, .. } = start_state(config);
    let app = build_router(state);
    let server = axum_test::TestServer::new(app).anyhow()?;

    let resp = server
        .post("/api/v1/hooks/start")
        .json(&serde_json::json!({"event": "start", "data": {}}))
        .await;
    resp.assert_status(StatusCode::OK);
    let body = resp.text();
    let lines: Vec<&str> = body.lines().collect();
    assert_eq!(lines.len(), 2);
    assert!(lines[0].contains("base64 -d"));
    assert_eq!(lines[1], "echo done");
    Ok(())
}

#[tokio::test]
async fn hooks_start_event_override() -> anyhow::Result<()> {
    let mut events = std::collections::BTreeMap::new();
    events.insert(
        "clear".to_owned(),
        StartEventConfig { text: Some("override".to_owned()), shell: vec![] },
    );
    let config = StartConfig { text: Some("default".to_owned()), shell: vec![], event: events };
    let StoreCtx { store: state, .. } = start_state(config);
    let app = build_router(state);
    let server = axum_test::TestServer::new(app).anyhow()?;

    let resp = server
        .post("/api/v1/hooks/start")
        .json(&serde_json::json!({"event": "start", "data": {"source": "clear"}}))
        .await;
    resp.assert_status(StatusCode::OK);
    let body = resp.text();
    // Verify override text is used (base64 of "override" not "default")
    let override_b64 = base64::engine::general_purpose::STANDARD.encode(b"override");
    assert!(body.contains(&override_b64), "should use override config: {body}");
    Ok(())
}

#[tokio::test]
async fn hooks_start_event_fallback() -> anyhow::Result<()> {
    let mut events = std::collections::BTreeMap::new();
    events.insert("clear".to_owned(), StartEventConfig::default());
    let config = StartConfig { text: Some("fallback".to_owned()), shell: vec![], event: events };
    let StoreCtx { store: state, .. } = start_state(config);
    let app = build_router(state);
    let server = axum_test::TestServer::new(app).anyhow()?;

    // Unknown source → falls back to top-level
    let resp = server
        .post("/api/v1/hooks/start")
        .json(&serde_json::json!({"event": "start", "data": {"source": "resume"}}))
        .await;
    resp.assert_status(StatusCode::OK);
    let body = resp.text();
    let fallback_b64 = base64::engine::general_purpose::STANDARD.encode(b"fallback");
    assert!(body.contains(&fallback_b64), "should fall back to top-level: {body}");
    Ok(())
}

#[tokio::test]
async fn hooks_start_emits_event() -> anyhow::Result<()> {
    let config = StartConfig { text: Some("ctx".to_owned()), ..Default::default() };
    let StoreCtx { store: state, .. } = start_state(config);
    let mut start_rx = state.start.start_tx.subscribe();

    let app = build_router(state);
    let server = axum_test::TestServer::new(app).anyhow()?;

    server
        .post("/api/v1/hooks/start")
        .json(
            &serde_json::json!({"event": "start", "data": {"source": "init", "session_id": "s1"}}),
        )
        .await;

    let event = start_rx.try_recv()?;
    assert_eq!(event.source, "init");
    assert_eq!(event.session_id.as_deref(), Some("s1"));
    assert!(event.injected);
    assert_eq!(event.seq, 0);
    Ok(())
}

#[tokio::test]
async fn get_start_config_returns_current() -> anyhow::Result<()> {
    let config = StartConfig { text: Some("test text".to_owned()), ..Default::default() };
    let StoreCtx { store: state, .. } = start_state(config);
    let app = build_router(state);
    let server = axum_test::TestServer::new(app).anyhow()?;

    let resp = server.get("/api/v1/config/start").await;
    resp.assert_status(StatusCode::OK);
    let body: serde_json::Value = serde_json::from_str(&resp.text())?;
    assert_eq!(body["text"], "test text");
    Ok(())
}

#[tokio::test]
async fn put_start_config_updates() -> anyhow::Result<()> {
    let StoreCtx { store: state, .. } = start_state(StartConfig::default());
    let app = build_router(state);
    let server = axum_test::TestServer::new(app).anyhow()?;

    // Default has no text.
    let resp = server.get("/api/v1/config/start").await;
    let body: serde_json::Value = serde_json::from_str(&resp.text())?;
    assert!(body.get("text").is_none());

    // Update.
    let resp = server
        .put("/api/v1/config/start")
        .json(&serde_json::json!({"text": "new context", "shell": ["echo hi"]}))
        .await;
    resp.assert_status(StatusCode::OK);
    let body = resp.text();
    assert!(body.contains("\"updated\":true"));

    // Verify.
    let resp = server.get("/api/v1/config/start").await;
    let body: serde_json::Value = serde_json::from_str(&resp.text())?;
    assert_eq!(body["text"], "new context");
    assert_eq!(body["shell"][0], "echo hi");
    Ok(())
}

#[tokio::test]
async fn hooks_start_extracts_session_type_as_source() -> anyhow::Result<()> {
    let config = StartConfig::default();
    let StoreCtx { store: state, .. } = start_state(config);
    let mut start_rx = state.start.start_tx.subscribe();

    let app = build_router(state);
    let server = axum_test::TestServer::new(app).anyhow()?;

    // session_type should be used as source when source is absent
    server
        .post("/api/v1/hooks/start")
        .json(&serde_json::json!({"event": "start", "data": {"session_type": "init"}}))
        .await;

    let event = start_rx.try_recv()?;
    assert_eq!(event.source, "init");
    Ok(())
}

#[tokio::test]
async fn hooks_start_clear_resets_last_message() -> anyhow::Result<()> {
    let StoreCtx { store: state, .. } = start_state(StartConfig::default());

    // Pre-populate last_message with a stale value.
    *state.driver.last_message.write().await = Some("stale message".to_owned());

    let app = build_router(state.clone());
    let server = axum_test::TestServer::new(app).anyhow()?;

    // POST a clear event.
    server
        .post("/api/v1/hooks/start")
        .json(&serde_json::json!({"event": "start", "data": {"source": "clear"}}))
        .await;

    // last_message should have been cleared.
    let lm = state.driver.last_message.read().await;
    assert!(lm.is_none(), "expected last_message to be cleared after /clear, got: {lm:?}");
    Ok(())
}
