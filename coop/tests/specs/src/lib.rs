// SPDX-License-Identifier: BUSL-1.1
// Copyright (c) 2026 Matt Baker <fimbaz@gmail.com>

//! Test harness for end-to-end binary smoke tests.
//!
//! Spawns the real `coop` binary as a subprocess and exercises it
//! over HTTP, WebSocket, gRPC, and Unix socket transports.

use std::path::{Path, PathBuf};
use std::process::{Child, Command, Stdio};
use std::sync::Once;
use std::time::Duration;

static CRYPTO_INIT: Once = Once::new();

/// Install the ring crypto provider for reqwest/rustls.
/// Safe to call multiple times — only the first call has effect.
pub fn ensure_crypto() {
    CRYPTO_INIT.call_once(|| {
        let _ = rustls::crypto::ring::default_provider().install_default();
    });
}

/// Resolve the path to the compiled `coop` binary.
///
/// The test binary lives at `<target>/debug/deps/<name>-<hash>`, so two
/// parents up is the `<target>/debug` directory where the `coop` binary
/// also lands. This works for both the default cargo layout and under
/// cargo-llvm-cov (where the target dir is `target/llvm-cov-target` but
/// `CARGO_TARGET_DIR` is *not* exported to spawned test processes —
/// llvm-cov passes `--target-dir` on the cargo command line instead).
/// Falls back to `CARGO_TARGET_DIR` env var, then the default `target/`
/// directory under the workspace root.
pub fn coop_binary() -> PathBuf {
    if let Ok(exe) = std::env::current_exe() {
        // exe: <target>/debug/deps/<name>-<hash>
        if let Some(debug_dir) = exe.parent().and_then(|p| p.parent()) {
            let candidate = debug_dir.join("coop");
            if candidate.exists() {
                return candidate;
            }
        }
    }
    let manifest = Path::new(env!("CARGO_MANIFEST_DIR"));
    // tests/specs → tests → workspace root
    let workspace = manifest.parent().and_then(|p| p.parent()).unwrap_or(manifest);
    let target_dir = std::env::var_os("CARGO_TARGET_DIR")
        .map(PathBuf::from)
        .unwrap_or_else(|| workspace.join("target"));
    target_dir.join("debug").join("coop")
}

/// Find a free TCP port by binding to :0 then releasing.
pub fn free_port() -> anyhow::Result<u16> {
    let listener = std::net::TcpListener::bind("127.0.0.1:0")?;
    Ok(listener.local_addr()?.port())
}

/// Make a raw HTTP/1.0 GET request over a Unix socket, returning the response body.
pub async fn unix_http_get(socket_path: &Path, path: &str) -> anyhow::Result<String> {
    use tokio::io::{AsyncReadExt, AsyncWriteExt};

    let mut stream = tokio::net::UnixStream::connect(socket_path).await?;
    let request = format!("GET {path} HTTP/1.1\r\nHost: localhost\r\nConnection: close\r\n\r\n");
    stream.write_all(request.as_bytes()).await?;

    let mut buf = Vec::new();
    stream.read_to_end(&mut buf).await?;
    let response = String::from_utf8(buf)?;

    let body = response.split_once("\r\n\r\n").map(|(_, b)| b).unwrap_or("").to_string();
    Ok(body)
}

/// A running `coop` process that is killed on drop.
pub struct CoopProcess {
    child: Child,
    port: Option<u16>,
    grpc_port: Option<u16>,
    health_port: Option<u16>,
    socket_path: Option<PathBuf>,
    _socket_dir: Option<tempfile::TempDir>,
}

/// Builder for configuring which transports a [`CoopProcess`] enables.
///
/// By default, only the main TCP port is enabled.
pub struct CoopBuilder {
    tcp: bool,
    grpc: bool,
    health: bool,
    socket: bool,
    nats_url: Option<String>,
}

impl Default for CoopBuilder {
    fn default() -> Self {
        Self { tcp: true, grpc: false, health: false, socket: false, nats_url: None }
    }
}

impl CoopBuilder {
    /// Disable the main TCP port (`--port`).
    pub fn no_tcp(mut self) -> Self {
        self.tcp = false;
        self
    }

    /// Enable the gRPC server (`--port-grpc`).
    pub fn grpc(mut self) -> Self {
        self.grpc = true;
        self
    }

    /// Enable the health-only HTTP port (`--port-health`).
    pub fn health(mut self) -> Self {
        self.health = true;
        self
    }

    /// Enable the Unix socket transport (`--socket`).
    pub fn socket(mut self) -> Self {
        self.socket = true;
        self
    }

    /// Enable NATS publishing (`--nats-url`).
    pub fn nats(mut self, url: &str) -> Self {
        self.nats_url = Some(url.to_owned());
        self
    }

    /// Spawn coop with the configured transports, wrapping `cmd`.
    pub fn spawn(self, cmd: &[&str]) -> anyhow::Result<CoopProcess> {
        ensure_crypto();
        let binary = coop_binary();
        anyhow::ensure!(binary.exists(), "coop binary not found at {}", binary.display());

        let port = if self.tcp { Some(free_port()?) } else { None };
        let grpc_port = if self.grpc { Some(free_port()?) } else { None };
        let health_port = if self.health { Some(free_port()?) } else { None };

        let (socket_path, socket_dir) = if self.socket {
            let dir = tempfile::tempdir()?;
            let path = dir.path().join("coop.sock");
            (Some(path), Some(dir))
        } else {
            (None, None)
        };

        let mut args: Vec<String> = Vec::new();

        if let Some(p) = port {
            args.extend(["--port".into(), p.to_string()]);
        }
        if let Some(p) = grpc_port {
            args.extend(["--port-grpc".into(), p.to_string()]);
        }
        if let Some(p) = health_port {
            args.extend(["--port-health".into(), p.to_string()]);
        }
        if let Some(ref p) = socket_path {
            args.extend(["--socket".into(), p.to_string_lossy().into_owned()]);
        }
        if let Some(ref url) = self.nats_url {
            args.extend(["--nats-url".into(), url.clone()]);
        }

        args.extend([
            "--host".into(),
            "127.0.0.1".into(),
            "--log-format".into(),
            "text".into(),
            "--log-level".into(),
            "warn".into(),
            "--".into(),
        ]);
        args.extend(cmd.iter().map(|s| s.to_string()));

        let state_dir = std::env::temp_dir().join(format!("coop-spec-{}", std::process::id()));

        let child = Command::new(&binary)
            .args(&args)
            .env("COOP_MUX_URL", "") // Disable mux registration in tests
            .env("COOP_MUX_STATE_DIR", &state_dir) // Isolate credential persistence
            .env("COOP_DRAIN_TIMEOUT_MS", "5000")
            .env("COOP_SHUTDOWN_TIMEOUT_MS", "2000")
            // Don't let a BOAT_ACCOUNT inherited from the test runner's
            // environment (e.g. an agent pod) pre-seed an initial credential
            // profile via seed_initial_profile — specs must start from a known
            // empty profile set. (ka-0000005014)
            .env_remove("BOAT_ACCOUNT")
            .stdout(Stdio::null())
            .stderr(Stdio::null())
            .spawn()?;

        Ok(CoopProcess {
            child,
            port,
            grpc_port,
            health_port,
            socket_path,
            _socket_dir: socket_dir,
        })
    }
}

impl CoopProcess {
    /// Create a builder for custom transport configuration.
    pub fn build() -> CoopBuilder {
        CoopBuilder::default()
    }

    /// Spawn coop with the default TCP-only configuration.
    pub fn start(cmd: &[&str]) -> anyhow::Result<Self> {
        ensure_crypto();
        Self::build().spawn(cmd)
    }

    /// The main HTTP port (if TCP is enabled).
    pub fn port(&self) -> Option<u16> {
        self.port
    }

    /// The gRPC port (if enabled).
    pub fn grpc_port(&self) -> Option<u16> {
        self.grpc_port
    }

    /// The health-only port (if enabled).
    pub fn health_port(&self) -> Option<u16> {
        self.health_port
    }

    /// The Unix socket path (if enabled).
    pub fn socket_path(&self) -> Option<&Path> {
        self.socket_path.as_deref()
    }

    /// Base URL for HTTP requests (requires TCP).
    pub fn base_url(&self) -> String {
        format!("http://127.0.0.1:{}", self.port.unwrap_or(0))
    }

    /// WebSocket URL (requires TCP).
    pub fn ws_url(&self) -> String {
        format!("ws://127.0.0.1:{}/ws", self.port.unwrap_or(0))
    }

    /// gRPC endpoint URL (requires gRPC port).
    pub fn grpc_url(&self) -> String {
        format!("http://127.0.0.1:{}", self.grpc_port.unwrap_or(0))
    }

    /// Health port URL (requires health port).
    pub fn health_url(&self) -> String {
        format!("http://127.0.0.1:{}", self.health_port.unwrap_or(0))
    }

    /// Poll health until responsive, using TCP or Unix socket (whichever is available).
    pub async fn wait_healthy(&self, timeout: Duration) -> anyhow::Result<()> {
        let deadline = tokio::time::Instant::now() + timeout;

        if let Some(port) = self.port {
            let client = reqwest::Client::new();
            let url = format!("http://127.0.0.1:{port}/api/v1/health");
            loop {
                if tokio::time::Instant::now() > deadline {
                    anyhow::bail!("coop did not become healthy within {timeout:?}");
                }
                if let Ok(resp) = client.get(&url).send().await {
                    if resp.status().is_success() {
                        return Ok(());
                    }
                }
                tokio::time::sleep(Duration::from_millis(50)).await;
            }
        } else if let Some(ref socket_path) = self.socket_path {
            loop {
                if tokio::time::Instant::now() > deadline {
                    anyhow::bail!("coop did not become healthy within {timeout:?}");
                }
                if let Ok(body) = unix_http_get(socket_path, "/api/v1/health").await {
                    if body.contains("running") {
                        return Ok(());
                    }
                }
                tokio::time::sleep(Duration::from_millis(50)).await;
            }
        } else {
            anyhow::bail!("no transport available for health check");
        }
    }

    /// Wait for the process to exit within `timeout`.
    pub async fn wait_exit(
        &mut self,
        timeout: Duration,
    ) -> anyhow::Result<std::process::ExitStatus> {
        let deadline = tokio::time::Instant::now() + timeout;
        loop {
            if tokio::time::Instant::now() > deadline {
                anyhow::bail!("coop did not exit within {timeout:?}");
            }
            if let Some(status) = self.child.try_wait()? {
                return Ok(status);
            }
            tokio::time::sleep(Duration::from_millis(50)).await;
        }
    }
}

impl Drop for CoopProcess {
    fn drop(&mut self) {
        let _ = self.child.kill();
        let _ = self.child.wait();
    }
}
