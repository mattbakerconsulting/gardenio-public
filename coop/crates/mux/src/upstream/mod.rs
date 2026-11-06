// SPDX-License-Identifier: BUSL-1.1
// Copyright (c) 2026 Matt Baker <fimbaz@gmail.com>

//! Upstream coop communication: HTTP client, pollers, and WebSocket bridge.

pub mod bridge;
pub mod client;
pub mod feed;
pub mod health;
pub mod poller;
pub mod prewarm;
