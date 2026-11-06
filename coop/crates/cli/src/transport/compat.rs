// SPDX-License-Identifier: BUSL-1.1
// Copyright (c) 2026 Matt Baker <fimbaz@gmail.com>

//! HTTP compatibility middleware: echoes `Connection: close` so hyper closes
//! the connection after responding when the client requests it.

use axum::http::header::{self, HeaderValue};
use axum::http::Request;
use axum::middleware::Next;
use axum::response::Response;

/// Middleware that echoes `Connection: close` on the response when the request
/// includes it, so hyper tears down the connection instead of keeping it alive.
pub async fn http_compat_layer(req: Request<axum::body::Body>, next: Next) -> Response {
    let conn_close = req
        .headers()
        .get(header::CONNECTION)
        .and_then(|v| v.to_str().ok())
        .map(|v| v.eq_ignore_ascii_case("close"))
        .unwrap_or(false);

    let mut response = next.run(req).await;

    if conn_close {
        response.headers_mut().insert(header::CONNECTION, HeaderValue::from_static("close"));
    }

    response
}

#[cfg(test)]
#[path = "compat_tests.rs"]
mod tests;
