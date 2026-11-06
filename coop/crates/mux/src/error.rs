// SPDX-License-Identifier: BUSL-1.1
// Copyright (c) 2026 Matt Baker <fimbaz@gmail.com>

use axum::http::StatusCode;
use axum::Json;
use serde::{Deserialize, Serialize};
use std::fmt;

/// Error codes for the mux API.
#[derive(Debug, Clone, Copy, PartialEq, Eq, Serialize, Deserialize)]
pub enum MuxError {
    Unauthorized,
    BadRequest,
    SessionNotFound,
    UpstreamError,
    Internal,
}

impl MuxError {
    pub fn http_status(&self) -> u16 {
        match self {
            Self::Unauthorized => 401,
            Self::BadRequest => 400,
            Self::SessionNotFound => 404,
            Self::UpstreamError => 502,
            Self::Internal => 500,
        }
    }

    pub fn as_str(&self) -> &'static str {
        match self {
            Self::Unauthorized => "UNAUTHORIZED",
            Self::BadRequest => "BAD_REQUEST",
            Self::SessionNotFound => "SESSION_NOT_FOUND",
            Self::UpstreamError => "UPSTREAM_ERROR",
            Self::Internal => "INTERNAL",
        }
    }

    pub fn to_error_body(&self, message: impl Into<String>) -> ErrorBody {
        ErrorBody { code: self.as_str().to_owned(), message: message.into() }
    }

    pub fn to_http_response(
        &self,
        message: impl Into<String>,
    ) -> (StatusCode, Json<ErrorResponse>) {
        let status =
            StatusCode::from_u16(self.http_status()).unwrap_or(StatusCode::INTERNAL_SERVER_ERROR);
        let body = ErrorResponse { error: self.to_error_body(message) };
        (status, Json(body))
    }
}

impl fmt::Display for MuxError {
    fn fmt(&self, f: &mut fmt::Formatter<'_>) -> fmt::Result {
        f.write_str(self.as_str())
    }
}

/// Top-level error response envelope.
#[derive(Debug, Clone, Serialize, Deserialize)]
pub struct ErrorResponse {
    pub error: ErrorBody,
}

/// Error body with machine-readable code and human-readable message.
#[derive(Debug, Clone, Serialize, Deserialize)]
pub struct ErrorBody {
    pub code: String,
    pub message: String,
}
