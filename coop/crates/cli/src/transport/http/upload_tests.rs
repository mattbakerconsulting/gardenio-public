// SPDX-License-Identifier: BUSL-1.1
// Copyright (c) 2026 Matt Baker <fimbaz@gmail.com>

use axum::http::StatusCode;
use base64::Engine;

use crate::test_support::StoreBuilder;
use crate::transport::build_router;

fn b64(data: &[u8]) -> String {
    base64::engine::general_purpose::STANDARD.encode(data)
}

#[tokio::test]
async fn upload_writes_file_and_returns_path() -> anyhow::Result<()> {
    let tmp = tempfile::tempdir()?;
    let ctx = StoreBuilder::new().session_dir(tmp.path().to_path_buf()).build();

    let app = build_router(ctx.store);
    let server = axum_test::TestServer::new(app)?;

    let res = server
        .post("/api/v1/upload")
        .json(&serde_json::json!({
            "filename": "test.txt",
            "data": b64(b"hello world"),
        }))
        .await;

    res.assert_status(StatusCode::OK);
    let body: serde_json::Value = res.json();
    assert_eq!(body["bytes_written"], 11);

    let path = body["path"].as_str().expect("path should be a string");
    let contents = std::fs::read_to_string(path)?;
    assert_eq!(contents, "hello world");

    // Verify it's under the uploads subdirectory.
    assert!(path.contains("/uploads/"));
    Ok(())
}

#[tokio::test]
async fn upload_rejects_without_session_dir() -> anyhow::Result<()> {
    let ctx = StoreBuilder::new().build();
    let app = build_router(ctx.store);
    let server = axum_test::TestServer::new(app)?;

    let res = server
        .post("/api/v1/upload")
        .json(&serde_json::json!({
            "filename": "test.txt",
            "data": b64(b"hello"),
        }))
        .await;

    res.assert_status(StatusCode::BAD_REQUEST);
    Ok(())
}

#[tokio::test]
async fn upload_rejects_bad_base64() -> anyhow::Result<()> {
    let tmp = tempfile::tempdir()?;
    let ctx = StoreBuilder::new().session_dir(tmp.path().to_path_buf()).build();

    let app = build_router(ctx.store);
    let server = axum_test::TestServer::new(app)?;

    let res = server
        .post("/api/v1/upload")
        .json(&serde_json::json!({
            "filename": "test.txt",
            "data": "!!!not-base64!!!",
        }))
        .await;

    res.assert_status(StatusCode::BAD_REQUEST);
    Ok(())
}

#[tokio::test]
async fn upload_sanitizes_traversal_filename() -> anyhow::Result<()> {
    let tmp = tempfile::tempdir()?;
    let ctx = StoreBuilder::new().session_dir(tmp.path().to_path_buf()).build();

    let app = build_router(ctx.store);
    let server = axum_test::TestServer::new(app)?;

    let res = server
        .post("/api/v1/upload")
        .json(&serde_json::json!({
            "filename": "../../etc/passwd",
            "data": b64(b"sneaky"),
        }))
        .await;

    res.assert_status(StatusCode::OK);
    let body: serde_json::Value = res.json();
    let path = body["path"].as_str().expect("path");

    // File should be in uploads dir, not traversed out.
    assert!(path.contains("/uploads/"));
    assert!(path.ends_with("passwd"));

    // Verify the file is actually inside the tmp dir.
    // Canonicalize both sides — on macOS /var → /private/var.
    let canonical = std::fs::canonicalize(path)?;
    let tmp_canonical = std::fs::canonicalize(tmp.path())?;
    assert!(canonical.starts_with(tmp_canonical));
    Ok(())
}

#[tokio::test]
async fn upload_handles_filename_conflict() -> anyhow::Result<()> {
    let tmp = tempfile::tempdir()?;
    let ctx = StoreBuilder::new().session_dir(tmp.path().to_path_buf()).build();

    let app = build_router(ctx.store);
    let server = axum_test::TestServer::new(app)?;

    // First upload.
    let res1 = server
        .post("/api/v1/upload")
        .json(&serde_json::json!({
            "filename": "dup.txt",
            "data": b64(b"first"),
        }))
        .await;
    res1.assert_status(StatusCode::OK);

    // Second upload with same name.
    let res2 = server
        .post("/api/v1/upload")
        .json(&serde_json::json!({
            "filename": "dup.txt",
            "data": b64(b"second"),
        }))
        .await;
    res2.assert_status(StatusCode::OK);

    let path1 = res1.json::<serde_json::Value>()["path"].as_str().expect("p1").to_owned();
    let path2 = res2.json::<serde_json::Value>()["path"].as_str().expect("p2").to_owned();

    assert_ne!(path1, path2);
    assert!(path2.contains("dup.1.txt"));

    assert_eq!(std::fs::read_to_string(&path1)?, "first");
    assert_eq!(std::fs::read_to_string(&path2)?, "second");
    Ok(())
}

#[tokio::test]
async fn upload_rejects_empty_filename() -> anyhow::Result<()> {
    let tmp = tempfile::tempdir()?;
    let ctx = StoreBuilder::new().session_dir(tmp.path().to_path_buf()).build();

    let app = build_router(ctx.store);
    let server = axum_test::TestServer::new(app)?;

    let res = server
        .post("/api/v1/upload")
        .json(&serde_json::json!({
            "filename": "",
            "data": b64(b"data"),
        }))
        .await;

    res.assert_status(StatusCode::BAD_REQUEST);
    Ok(())
}
