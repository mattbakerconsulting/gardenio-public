// SPDX-License-Identifier: BUSL-1.1
// Copyright (c) 2026 Matt Baker <fimbaz@gmail.com>

//! HTTP client for communicating with a single upstream coop instance.
//!
//! `UpstreamClient` wraps a shared `reqwest::Client` (cloned cheaply from
//! [`MuxState::http_client`](crate::state::MuxState::http_client)) plus
//! per-instance `(base_url, auth_token, timeout)`. The shared client owns
//! the connection pool, idle-keepalive task, DNS resolver, TLS context,
//! and tokio dispatcher — building one per request leaks resources at
//! steady state (see ka-0000001805).

use std::time::Duration;

use reqwest::Client;

/// HTTP client wrapper for one upstream coop instance.
///
/// Cheap to construct: holds a clone of a shared `reqwest::Client` (whose
/// internals are reference-counted) plus three small fields.
pub struct UpstreamClient {
    base_url: String,
    auth_token: Option<String>,
    client: Client,
    timeout: Option<Duration>,
}

impl UpstreamClient {
    /// Construct a client wrapper around a shared `reqwest::Client`.
    ///
    /// Pass `state.http_client.clone()` from a [`MuxState`](crate::state::MuxState).
    pub fn new(client: Client, base_url: String, auth_token: Option<String>) -> Self {
        Self { base_url, auth_token, client, timeout: None }
    }

    /// Construct a client wrapper that applies a per-request timeout.
    ///
    /// Pass `state.http_client.clone()`. The `timeout` is applied via
    /// `RequestBuilder::timeout(...)` on each call rather than baked into
    /// the underlying `reqwest::Client`, so the shared pool is preserved.
    pub fn with_timeout(
        client: Client,
        base_url: String,
        auth_token: Option<String>,
        timeout: Duration,
    ) -> Self {
        Self { base_url, auth_token, client, timeout: Some(timeout) }
    }

    fn url(&self, path: &str) -> String {
        format!("{}{}", self.base_url, path)
    }

    fn prepare(&self, req: reqwest::RequestBuilder) -> reqwest::RequestBuilder {
        let req = match &self.auth_token {
            Some(token) => req.bearer_auth(token),
            None => req,
        };
        match self.timeout {
            Some(t) => req.timeout(t),
            None => req,
        }
    }

    /// Check upstream health.
    pub async fn health(&self) -> anyhow::Result<serde_json::Value> {
        let req = self.client.get(self.url("/api/v1/health"));
        let resp = self.prepare(req).send().await?;
        let value = resp.error_for_status()?.json().await?;
        Ok(value)
    }

    /// Fetch screen snapshot from upstream.
    pub async fn get_screen(&self) -> anyhow::Result<serde_json::Value> {
        let req = self.client.get(self.url("/api/v1/screen"));
        let resp = self.prepare(req).send().await?;
        let value = resp.error_for_status()?.json().await?;
        Ok(value)
    }

    /// Fetch status from upstream.
    pub async fn get_status(&self) -> anyhow::Result<serde_json::Value> {
        let req = self.client.get(self.url("/api/v1/status"));
        let resp = self.prepare(req).send().await?;
        let value = resp.error_for_status()?.json().await?;
        Ok(value)
    }

    /// Fetch session usage (cost + tokens) from upstream.
    pub async fn get_usage(&self) -> anyhow::Result<serde_json::Value> {
        let req = self.client.get(self.url("/api/v1/session/usage"));
        let resp = self.prepare(req).send().await?;
        let value = resp.error_for_status()?.json().await?;
        Ok(value)
    }

    /// Fetch agent state from upstream.
    pub async fn get_agent(&self) -> anyhow::Result<serde_json::Value> {
        let req = self.client.get(self.url("/api/v1/agent"));
        let resp = self.prepare(req).send().await?;
        let value = resp.error_for_status()?.json().await?;
        Ok(value)
    }

    /// GET a JSON response from an upstream endpoint path (including query string).
    pub async fn get_json(&self, path: &str) -> anyhow::Result<serde_json::Value> {
        let req = self.client.get(self.url(path));
        let resp = self.prepare(req).send().await?.error_for_status()?;
        let bytes = resp.bytes().await?;
        if bytes.is_empty() {
            return Ok(serde_json::Value::Null);
        }
        Ok(serde_json::from_slice(&bytes)?)
    }

    /// POST JSON to an upstream endpoint and return the response body.
    pub async fn post_json(
        &self,
        path: &str,
        body: &serde_json::Value,
    ) -> anyhow::Result<serde_json::Value> {
        let req = self.client.post(self.url(path)).json(body);
        let resp = self.prepare(req).send().await?.error_for_status()?;
        let bytes = resp.bytes().await?;
        if bytes.is_empty() {
            return Ok(serde_json::Value::Null);
        }
        Ok(serde_json::from_slice(&bytes)?)
    }
}
