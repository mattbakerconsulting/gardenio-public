// SPDX-License-Identifier: BUSL-1.1
// Copyright (c) 2026 Matt Baker <fimbaz@gmail.com>

use crate::test_support::{AnyhowExt, StoreBuilder, StoreCtx};
use crate::transport::build_router;
use crate::usage::UsageDelta;

/// GET /metrics returns valid Prometheus text with zero counters initially.
#[tokio::test]
async fn metrics_returns_zero_counters() -> anyhow::Result<()> {
    let StoreCtx { store, .. } = StoreBuilder::new().build();
    let app = build_router(store);
    let server = axum_test::TestServer::new(app).anyhow()?;

    let resp = server.get("/metrics").await;
    resp.assert_status_ok();

    let body = resp.text();
    assert!(body.contains("# TYPE gardenio_session_input_tokens_total counter"));
    assert!(body.contains("# TYPE gardenio_session_output_tokens_total counter"));
    assert!(body.contains("gardenio_session_input_tokens_total{"));
    assert!(body.contains("} 0\n"));
    Ok(())
}

/// GET /metrics reflects accumulated usage deltas.
#[tokio::test]
async fn metrics_reflects_accumulated_deltas() -> anyhow::Result<()> {
    let StoreCtx { store, .. } = StoreBuilder::new().build();

    store
        .usage
        .accumulate(UsageDelta {
            input_tokens: 100,
            output_tokens: 50,
            cache_creation_input_tokens: 10,
            cache_read_input_tokens: 20,
            duration_api_ms: Some(1200),
            ..Default::default()
        })
        .await;

    let app = build_router(store);
    let server = axum_test::TestServer::new(app).anyhow()?;

    let resp = server.get("/metrics").await;
    resp.assert_status_ok();

    let body = resp.text();
    assert!(body.contains("gardenio_session_input_tokens_total{") && body.contains("} 100\n"));
    assert!(body.contains("gardenio_session_output_tokens_total{") && body.contains("} 50\n"));
    assert!(body.contains("gardenio_session_cache_read_tokens_total{") && body.contains("} 20\n"));
    assert!(body.contains("gardenio_session_cache_write_tokens_total{") && body.contains("} 10\n"));
    assert!(body.contains("gardenio_session_request_count_total{") && body.contains("} 1\n"));
    assert!(body.contains("gardenio_session_api_ms_total{") && body.contains("} 1200\n"));
    Ok(())
}

/// GET /metrics renders the latency histograms in Prometheus format.
#[tokio::test]
async fn metrics_renders_latency_histograms() -> anyhow::Result<()> {
    let StoreCtx { store, .. } = StoreBuilder::new().build();

    // One LLM request (1.2s, via accumulate) and two tool calls.
    store
        .usage
        .accumulate(UsageDelta {
            input_tokens: 100,
            output_tokens: 50,
            cache_creation_input_tokens: 0,
            cache_read_input_tokens: 0,
            duration_api_ms: Some(1200),
            ..Default::default()
        })
        .await;
    store.usage.observe_tool_call(0.02).await;
    store.usage.observe_tool_call(3.0).await;

    let app = build_router(store);
    let server = axum_test::TestServer::new(app).anyhow()?;

    let resp = server.get("/metrics").await;
    resp.assert_status_ok();
    let body = resp.text();

    // Histogram TYPE lines.
    assert!(body.contains("# TYPE gardenio_agent_tool_call_latency_seconds histogram"));
    assert!(body.contains("# TYPE gardenio_agent_llm_request_latency_seconds histogram"));

    // Mandatory +Inf bucket, _sum, and _count for both histograms.
    assert!(body.contains("gardenio_agent_tool_call_latency_seconds_bucket{"));
    assert!(body.contains("le=\"+Inf\""));
    assert!(
        body.contains("gardenio_agent_tool_call_latency_seconds_count{") && body.contains("} 2\n")
    );
    assert!(
        body.contains("gardenio_agent_llm_request_latency_seconds_count{")
            && body.contains("} 1\n")
    );

    // Tool-call sum = 0.02 + 3.0 = 3.02; LLM sum = 1.2.
    assert!(
        body.contains("gardenio_agent_tool_call_latency_seconds_sum{") && body.contains("} 3.02\n")
    );
    assert!(
        body.contains("gardenio_agent_llm_request_latency_seconds_sum{")
            && body.contains("} 1.2\n")
    );
    Ok(())
}

/// GET /metrics includes all required labels.
#[tokio::test]
async fn metrics_includes_labels() -> anyhow::Result<()> {
    let StoreCtx { store, .. } = StoreBuilder::new().build();
    let app = build_router(store);
    let server = axum_test::TestServer::new(app).anyhow()?;

    let resp = server.get("/metrics").await;
    let body = resp.text();

    // Every metric line should have agent_name, project, and account labels.
    for line in body.lines() {
        if line.starts_with('#') || line.is_empty() {
            continue;
        }
        assert!(line.contains("agent_name="), "missing agent_name label: {line}");
        assert!(line.contains("project="), "missing project label: {line}");
        assert!(line.contains("account="), "missing account label: {line}");
    }
    Ok(())
}

/// GET /metrics returns the correct content type.
#[tokio::test]
async fn metrics_content_type() -> anyhow::Result<()> {
    let StoreCtx { store, .. } = StoreBuilder::new().build();
    let app = build_router(store);
    let server = axum_test::TestServer::new(app).anyhow()?;

    let resp = server.get("/metrics").await;
    resp.assert_status_ok();

    let hdr = resp.header("content-type");
    let ct = hdr.to_str()?;
    assert!(ct.contains("text/plain"), "unexpected content-type: {ct}");
    assert!(ct.contains("0.0.4"), "missing version in content-type: {ct}");
    Ok(())
}

/// Prometheus label escaping handles backslash, quote, newline.
#[test]
fn prom_escape_special_chars() {
    use super::prom_escape;
    assert_eq!(prom_escape(r#"hello"world"#), r#"hello\"world"#);
    assert_eq!(prom_escape("line\nbreak"), "line\\nbreak");
    assert_eq!(prom_escape(r"back\slash"), r"back\\slash");
    assert_eq!(prom_escape("normal"), "normal");
}

/// GET /metrics renders the one-shot --resume report when present
/// (ka-0000000550): outcome-labelled attempt counter, prepare latency, and
/// the transcript byte length (the partial-log signal).
#[tokio::test]
async fn metrics_renders_resume_report() -> anyhow::Result<()> {
    let StoreCtx { store, .. } = StoreBuilder::new()
        .resume_report(crate::transport::state::ResumeReport {
            outcome: "resumed",
            prepare_seconds: 1.25,
            log_bytes: 4096,
        })
        .build();
    let app = build_router(store);
    let server = axum_test::TestServer::new(app).anyhow()?;

    let resp = server.get("/metrics").await;
    resp.assert_status_ok();

    let body = resp.text();
    assert!(body.contains("# TYPE gardenio_coop_resume_attempts_total counter"), "{body}");
    assert!(
        body.contains(r#"outcome="resumed""#)
            && body.contains("gardenio_coop_resume_attempts_total{"),
        "missing outcome-labelled attempt counter:\n{body}"
    );
    assert!(body.contains("gardenio_coop_resume_prepare_seconds{"), "{body}");
    assert!(body.contains("} 1.25\n"), "prepare_seconds value missing:\n{body}");
    assert!(body.contains("gardenio_coop_resume_log_bytes{"), "{body}");
    assert!(body.contains("} 4096\n"), "log_bytes value missing:\n{body}");
    Ok(())
}

/// No resume requested → the resume series are entirely absent (a fresh first
/// spawn must not emit a misleading zero-valued resume series).
#[tokio::test]
async fn metrics_omits_resume_report_when_absent() -> anyhow::Result<()> {
    let StoreCtx { store, .. } = StoreBuilder::new().build();
    let app = build_router(store);
    let server = axum_test::TestServer::new(app).anyhow()?;

    let resp = server.get("/metrics").await;
    resp.assert_status_ok();
    let body = resp.text();
    assert!(!body.contains("gardenio_coop_resume"), "resume series must be absent:\n{body}");
    Ok(())
}
