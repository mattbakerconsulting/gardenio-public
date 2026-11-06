// SPDX-License-Identifier: BUSL-1.1
// Copyright (c) 2026 Matt Baker <fimbaz@gmail.com>

use serde::{Deserialize, Serialize};
use std::fmt;

/// Unified error codes shared across HTTP, WebSocket, and gRPC transports.
#[derive(Debug, Clone, Copy, PartialEq, Eq, Serialize, Deserialize)]
pub enum ErrorCode {
    NotReady,
    Exited,
    Unauthorized,
    BadRequest,
    NotFound,
    NoDriver,
    AgentBusy,
    NoPrompt,
    SwitchInProgress,
    Internal,
}

impl ErrorCode {
    pub fn http_status(&self) -> u16 {
        match self {
            Self::NotReady => 503,
            Self::Exited => 410,
            Self::Unauthorized => 401,
            Self::BadRequest => 400,
            Self::NotFound => 404,
            Self::NoDriver => 404,
            Self::AgentBusy => 409,
            Self::NoPrompt => 409,
            Self::SwitchInProgress => 409,
            Self::Internal => 500,
        }
    }

    pub fn as_str(&self) -> &'static str {
        match self {
            Self::NotReady => "NOT_READY",
            Self::Exited => "EXITED",
            Self::Unauthorized => "UNAUTHORIZED",
            Self::BadRequest => "BAD_REQUEST",
            Self::NotFound => "NOT_FOUND",
            Self::NoDriver => "NO_DRIVER",
            Self::AgentBusy => "AGENT_BUSY",
            Self::NoPrompt => "NO_PROMPT",
            Self::SwitchInProgress => "SWITCH_IN_PROGRESS",
            Self::Internal => "INTERNAL",
        }
    }
}

impl ErrorCode {
    /// Convert this error code into a [`tonic::Status`] with the given message.
    pub fn to_grpc_status(&self, message: impl Into<String>) -> tonic::Status {
        let code = match self {
            Self::NotReady => tonic::Code::Unavailable,
            Self::Exited => tonic::Code::NotFound,
            Self::Unauthorized => tonic::Code::Unauthenticated,
            Self::BadRequest => tonic::Code::InvalidArgument,
            Self::NotFound => tonic::Code::NotFound,
            Self::NoDriver => tonic::Code::Unimplemented,
            Self::AgentBusy => tonic::Code::FailedPrecondition,
            Self::NoPrompt => tonic::Code::FailedPrecondition,
            Self::SwitchInProgress => tonic::Code::FailedPrecondition,
            Self::Internal => tonic::Code::Internal,
        };
        tonic::Status::new(code, message)
    }
}

impl fmt::Display for ErrorCode {
    fn fmt(&self, f: &mut fmt::Formatter<'_>) -> fmt::Result {
        f.write_str(self.as_str())
    }
}

#[cfg(test)]
#[path = "error_tests.rs"]
mod tests;
