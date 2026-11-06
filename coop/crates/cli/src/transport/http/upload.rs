// SPDX-License-Identifier: BUSL-1.1
// Copyright (c) 2026 Matt Baker <fimbaz@gmail.com>

//! File upload handler for drag-and-drop in the web terminal.

use std::path::{Path, PathBuf};
use std::sync::Arc;

use axum::extract::State;
use axum::response::IntoResponse;
use axum::Json;
use serde::{Deserialize, Serialize};

use crate::error::ErrorCode;
use crate::transport::state::Store;

/// Maximum decoded file size: 10 MiB.
const MAX_FILE_SIZE: usize = 10 * 1024 * 1024;

#[derive(Debug, Deserialize)]
pub struct UploadRequest {
    pub filename: String,
    pub data: String,
}

#[derive(Debug, Serialize)]
pub struct UploadResponse {
    pub path: String,
    pub bytes_written: usize,
}

/// `POST /api/v1/upload` — accept a base64-encoded file and write it to the
/// session uploads directory.
pub async fn upload(
    State(s): State<Arc<Store>>,
    Json(req): Json<UploadRequest>,
) -> Result<impl IntoResponse, impl IntoResponse> {
    let session_dir = s.session_dir.as_ref().ok_or_else(|| {
        ErrorCode::BadRequest.to_http_response("upload not available (no session directory)")
    })?;

    let sanitized = sanitize_filename(&req.filename)
        .ok_or_else(|| ErrorCode::BadRequest.to_http_response("invalid filename"))?;

    let decoded = base64_decode(&req.data)
        .map_err(|e| ErrorCode::BadRequest.to_http_response(format!("invalid base64: {e}")))?;

    if decoded.len() > MAX_FILE_SIZE {
        return Err(ErrorCode::BadRequest.to_http_response(format!(
            "file too large: {} bytes (max {})",
            decoded.len(),
            MAX_FILE_SIZE,
        )));
    }

    let uploads_dir = session_dir.join("uploads");
    tokio::fs::create_dir_all(&uploads_dir).await.map_err(|e| {
        ErrorCode::Internal.to_http_response(format!("failed to create uploads dir: {e}"))
    })?;

    let dest = resolve_unique_path(&uploads_dir, &sanitized).await;

    tokio::fs::write(&dest, &decoded)
        .await
        .map_err(|e| ErrorCode::Internal.to_http_response(format!("failed to write file: {e}")))?;

    let abs_path = dest.canonicalize().unwrap_or(dest).to_string_lossy().into_owned();

    Ok(Json(UploadResponse { path: abs_path, bytes_written: decoded.len() }))
}

/// Decode base64 (standard or URL-safe, with or without padding).
fn base64_decode(input: &str) -> Result<Vec<u8>, String> {
    use base64::Engine;
    // Try standard alphabet first, then URL-safe.
    base64::engine::general_purpose::STANDARD
        .decode(input)
        .or_else(|_| base64::engine::general_purpose::URL_SAFE.decode(input))
        .map_err(|e| e.to_string())
}

/// Sanitize a user-provided filename to prevent path traversal.
///
/// Extracts `Path::file_name()`, rejects `.` / `..` / empty, replaces
/// null bytes and path separators, and truncates to 255 bytes.
fn sanitize_filename(raw: &str) -> Option<String> {
    // Use Path::file_name to strip directory components.
    let name = Path::new(raw).file_name()?.to_str()?;

    if name.is_empty() || name == "." || name == ".." {
        return None;
    }

    // Replace null bytes and path separators.
    let clean: String =
        name.chars().map(|c| if c == '\0' || c == '/' || c == '\\' { '_' } else { c }).collect();

    if clean.is_empty() {
        return None;
    }

    // Truncate to 255 bytes (filesystem limit).
    let truncated = if clean.len() > 255 {
        let mut end = 255;
        while end > 0 && !clean.is_char_boundary(end) {
            end -= 1;
        }
        &clean[..end]
    } else {
        &clean
    };

    Some(truncated.to_owned())
}

/// Resolve a unique file path in `dir`, appending `.1`, `.2`, etc. before
/// the extension on conflict.
async fn resolve_unique_path(dir: &Path, name: &str) -> PathBuf {
    let candidate = dir.join(name);
    if !candidate.exists() {
        return candidate;
    }

    let stem = Path::new(name).file_stem().and_then(|s| s.to_str()).unwrap_or(name);
    let ext = Path::new(name).extension().and_then(|s| s.to_str());

    for i in 1u32.. {
        let numbered = match ext {
            Some(e) => format!("{stem}.{i}.{e}"),
            None => format!("{stem}.{i}"),
        };
        let path = dir.join(&numbered);
        if !path.exists() {
            return path;
        }
    }

    // Unreachable in practice.
    candidate
}

#[cfg(test)]
#[path = "upload_tests.rs"]
mod tests;
