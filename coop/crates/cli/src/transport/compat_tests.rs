// SPDX-License-Identifier: BUSL-1.1
// Copyright (c) 2026 Matt Baker <fimbaz@gmail.com>

use axum::body::Body;
use axum::http::{Request, StatusCode, Version};
use axum::middleware;
use axum::routing::get;
use axum::Router;
use tower::ServiceExt;

use super::http_compat_layer;

fn test_router() -> Router {
    Router::new()
        .route("/test", get(|| async { "ok" }))
        .layer(middleware::from_fn(http_compat_layer))
}

#[tokio::test]
async fn passes_http10() -> anyhow::Result<()> {
    let app = test_router();
    let req = Request::builder().version(Version::HTTP_10).uri("/test").body(Body::empty())?;
    let resp = app.oneshot(req).await?;
    assert_eq!(resp.status(), StatusCode::OK);
    Ok(())
}

#[tokio::test]
async fn passes_http11() -> anyhow::Result<()> {
    let app = test_router();
    let req = Request::builder().version(Version::HTTP_11).uri("/test").body(Body::empty())?;
    let resp = app.oneshot(req).await?;
    assert_eq!(resp.status(), StatusCode::OK);
    Ok(())
}

#[tokio::test]
async fn echoes_connection_close() -> anyhow::Result<()> {
    let app = test_router();
    let req = Request::builder()
        .version(Version::HTTP_11)
        .uri("/test")
        .header("connection", "close")
        .body(Body::empty())?;
    let resp = app.oneshot(req).await?;
    assert_eq!(resp.status(), StatusCode::OK);
    let conn = resp.headers().get("connection").and_then(|v| v.to_str().ok());
    assert_eq!(conn, Some("close"));
    Ok(())
}

#[tokio::test]
async fn no_connection_header_without_request_header() -> anyhow::Result<()> {
    let app = test_router();
    let req = Request::builder().version(Version::HTTP_11).uri("/test").body(Body::empty())?;
    let resp = app.oneshot(req).await?;
    assert_eq!(resp.status(), StatusCode::OK);
    assert!(resp.headers().get("connection").is_none());
    Ok(())
}
