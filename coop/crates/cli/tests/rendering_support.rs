// SPDX-License-Identifier: BUSL-1.1
// Copyright (c) 2026 Matt Baker <fimbaz@gmail.com>

//! Shared test infrastructure for Layer B (tmux oracle) and Layer C (attach)
//! rendering fidelity tests.
//!
//! Provides `TmuxOracle` (isolated tmux server with capture-pane) and
//! `CoopScenario` (start coop with a claudeless scenario, poll until ready).

#![allow(dead_code)]

use std::path::{Path, PathBuf};
use std::process::{Command, Stdio};
use std::sync::Once;
use std::time::{Duration, Instant};

static INIT: Once = Once::new();

/// Install the rustls crypto provider (needed for reqwest even on plain HTTP).
pub fn ensure_crypto_provider() {
    INIT.call_once(|| {
        let _ = rustls::crypto::ring::default_provider().install_default();
    });
}

/// Check that a binary is available in PATH.
pub fn has_binary(name: &str) -> bool {
    Command::new("which")
        .arg(name)
        .stdout(Stdio::null())
        .stderr(Stdio::null())
        .status()
        .map(|s| s.success())
        .unwrap_or(false)
}

/// Skip the test if claudeless is not installed.
pub fn require_claudeless() {
    if !has_binary("claudeless") {
        panic!(
            "claudeless not found in PATH — install via: brew install alfredjeanlab/tap/claudeless"
        );
    }
}

/// Skip the test if tmux is not installed.
pub fn require_tmux() {
    if !has_binary("tmux") {
        panic!("tmux not found in PATH — install via: brew install tmux");
    }
}

// -- Host environment isolation -----------------------------------------------

/// Names of the `COOP_*` variables currently exported by the host.
///
/// These tests also run inside gardenio agent pods, which export a full
/// production coop configuration (`COOP_S3_BUCKET`, `COOP_S3_SESSION_KEY`,
/// `COOP_MUX_URL`, ...). A test coop that inherits them is not running the
/// scenario we asked for: the S3 layer restores the *host* agent's Claude
/// transcript, auto-sets `--resume <uuid>`, and passes it to `claudeless`,
/// which prints `Error: Session not found: <uuid>` instead of the scenario
/// text — so every `wait_for_screen_text` sentinel times out (gd-d0xS21YGah).
///
/// Read at call time rather than cached: the list must reflect the process
/// environment the child would actually inherit.
fn host_coop_vars() -> Vec<String> {
    let mut vars: Vec<String> =
        std::env::vars().map(|(k, _)| k).filter(|k| k.starts_with("COOP_")).collect();
    vars.sort();
    vars
}

/// Strip inherited coop configuration and pin an isolated Claude config dir.
///
/// Applied to every coop process this suite spawns so a scenario run depends
/// only on the flags the test passes.
fn isolate_env(cmd: &mut Command, claude_config_dir: &Path) {
    for key in host_coop_vars() {
        cmd.env_remove(key);
    }
    cmd.env("CLAUDE_CONFIG_DIR", claude_config_dir);
}

/// `env(1)` prefix that applies [`isolate_env`]'s scrubbing to a shell command.
///
/// tmux inherits the test process environment and offers no way to *unset* a
/// variable in the spawned pane, so the isolation has to ride on the command
/// line instead.
fn isolating_env_prefix(claude_config_dir: &Path) -> String {
    let mut prefix = String::from("env");
    for key in host_coop_vars() {
        prefix.push_str(" -u ");
        prefix.push_str(&key);
    }
    prefix.push_str(&format!(" CLAUDE_CONFIG_DIR={} ", claude_config_dir.display()));
    prefix
}

/// Resolve a scenario TOML file path relative to the crate's test directory.
pub fn scenario_path(name: &str) -> String {
    PathBuf::from(env!("CARGO_MANIFEST_DIR"))
        .join("tests/scenarios")
        .join(name)
        .display()
        .to_string()
}

// -- TmuxOracle ---------------------------------------------------------------

/// An isolated tmux server for oracle comparison.
///
/// Creates a unique tmux socket so tests don't interfere with each other or
/// the user's running tmux sessions. Implements Drop to kill the server.
pub struct TmuxOracle {
    socket_name: String,
    session_name: String,
    /// Isolated `CLAUDE_CONFIG_DIR` for the pane; dropped with the oracle.
    _config_dir: tempfile::TempDir,
}

impl TmuxOracle {
    /// Create a new tmux server and session with the given shell command.
    ///
    /// The command string is passed to tmux which runs it via the default shell.
    pub fn new(shell_cmd: &str, cols: u16, rows: u16) -> anyhow::Result<Self> {
        let id = uuid::Uuid::new_v4().to_string()[..8].to_owned();
        let socket_name = format!("coop-test-{id}");
        let session_name = format!("test-{id}");

        // Start a new detached tmux session with fixed size.
        // Append a long sleep so the tmux session stays alive after the command
        // exits — this gives capture_pane time to read the final output.
        // The `env` prefix keeps inherited host coop config out of the pane
        // (see `isolating_env_prefix`).
        let config_dir = tempfile::tempdir()?;
        let scrub = isolating_env_prefix(config_dir.path());
        let wrapped_cmd = format!("{scrub}{shell_cmd}; sleep 300");
        let status = Command::new("tmux")
            .args([
                "-L",
                &socket_name,
                "new-session",
                "-d",
                "-s",
                &session_name,
                "-x",
                &cols.to_string(),
                "-y",
                &rows.to_string(),
                "-e",
                "LANG=C.UTF-8",
                "-e",
                "LC_ALL=C.UTF-8",
                &wrapped_cmd,
            ])
            .stdout(Stdio::null())
            .stderr(Stdio::null())
            .status()?;

        if !status.success() {
            anyhow::bail!("failed to start tmux session: {status}");
        }

        Ok(Self { socket_name, session_name, _config_dir: config_dir })
    }

    /// Capture the current pane contents as a vector of lines.
    pub fn capture_pane(&self) -> anyhow::Result<Vec<String>> {
        let output = Command::new("tmux")
            .args(["-L", &self.socket_name, "capture-pane", "-t", &self.session_name, "-p"])
            .output()?;

        if !output.status.success() {
            anyhow::bail!("tmux capture-pane failed: {}", String::from_utf8_lossy(&output.stderr));
        }

        let text = String::from_utf8_lossy(&output.stdout);
        Ok(text.lines().map(|l| l.to_owned()).collect())
    }

    /// Poll until the pane contains `sentinel` text, with a timeout.
    pub fn wait_for_text(&self, sentinel: &str, timeout: Duration) -> anyhow::Result<Vec<String>> {
        let start = Instant::now();
        loop {
            let lines = self.capture_pane()?;
            let text = lines.join("\n");
            if text.contains(sentinel) {
                return Ok(lines);
            }
            if start.elapsed() > timeout {
                anyhow::bail!("timed out waiting for {sentinel:?} in tmux (got: {text:?})");
            }
            std::thread::sleep(Duration::from_millis(200));
        }
    }
}

impl Drop for TmuxOracle {
    fn drop(&mut self) {
        // Kill the tmux server to clean up.
        let _ = Command::new("tmux")
            .args(["-L", &self.socket_name, "kill-server"])
            .stdout(Stdio::null())
            .stderr(Stdio::null())
            .status();
    }
}

// -- CoopScenario -------------------------------------------------------------

/// A coop process running a claudeless scenario.
///
/// Starts coop as a child process, provides methods to query the HTTP API,
/// and kills the process on drop.
pub struct CoopScenario {
    child: std::process::Child,
    addr: String,
    stderr_path: std::path::PathBuf,
    /// Isolated `CLAUDE_CONFIG_DIR` for the child; dropped with the scenario.
    _config_dir: tempfile::TempDir,
}

impl CoopScenario {
    /// Start coop with the given scenario file and prompt.
    ///
    /// Finds a free port, starts coop listening on it, and waits for the
    /// health endpoint to respond.
    pub fn start(scenario: &str, prompt: &str, cols: u16, rows: u16) -> anyhow::Result<Self> {
        ensure_crypto_provider();
        let port = find_free_port()?;
        let addr = format!("127.0.0.1:{port}");

        let coop_bin = coop_binary_path()?;
        let scenario_file = scenario_path(scenario);

        // Capture stderr to a temp file for debugging startup failures.
        let stderr_path = std::env::temp_dir().join(format!("coop-test-{port}.stderr"));
        let stderr_file = std::fs::File::create(&stderr_path)?;

        // Run against a throwaway Claude config dir with no inherited coop
        // configuration, so the scenario is the only thing driving the screen.
        let config_dir = tempfile::tempdir()?;

        let mut command = Command::new(&coop_bin);
        command
            .args([
                "--host",
                "127.0.0.1",
                "--port",
                &port.to_string(),
                "--cols",
                &cols.to_string(),
                "--rows",
                &rows.to_string(),
                "--",
                "claudeless",
                "--scenario",
                &scenario_file,
                prompt,
            ])
            .stdout(Stdio::null())
            .stderr(stderr_file);
        isolate_env(&mut command, config_dir.path());

        let child = command.spawn()?;

        let coop = Self { child, addr: addr.clone(), stderr_path, _config_dir: config_dir };

        // Wait for health endpoint.
        coop.wait_for_health(Duration::from_secs(15))?;

        Ok(coop)
    }

    /// Base URL for HTTP API requests.
    pub fn base_url(&self) -> String {
        format!("http://{}", self.addr)
    }

    /// Fetch the current screen via HTTP API.
    pub fn fetch_screen(&self) -> anyhow::Result<ScreenApiResponse> {
        let url = format!("{}/api/v1/screen", self.base_url());
        let resp = reqwest::blocking::get(&url)?;
        let screen: ScreenApiResponse = resp.json()?;
        Ok(screen)
    }

    /// Fetch the current screen text lines via HTTP API.
    pub fn fetch_screen_lines(&self) -> anyhow::Result<Vec<String>> {
        let screen = self.fetch_screen()?;
        Ok(screen.lines)
    }

    /// Poll until screen contains sentinel text.
    pub fn wait_for_screen_text(
        &self,
        sentinel: &str,
        timeout: Duration,
    ) -> anyhow::Result<Vec<String>> {
        let start = Instant::now();
        loop {
            match self.fetch_screen_lines() {
                Ok(lines) => {
                    let text = lines.join("\n");
                    if text.contains(sentinel) {
                        return Ok(lines);
                    }
                }
                Err(_) => {} // Server not ready yet, keep polling.
            }
            if start.elapsed() > timeout {
                anyhow::bail!("timed out waiting for {sentinel:?} on coop screen");
            }
            std::thread::sleep(Duration::from_millis(200));
        }
    }

    /// Wait for the health endpoint to respond.
    fn wait_for_health(&self, timeout: Duration) -> anyhow::Result<()> {
        let start = Instant::now();
        let url = format!("{}/api/v1/livez", self.base_url());
        loop {
            if reqwest::blocking::get(&url).is_ok() {
                return Ok(());
            }
            if start.elapsed() > timeout {
                let stderr = std::fs::read_to_string(&self.stderr_path).unwrap_or_default();
                let tail: String = stderr
                    .lines()
                    .rev()
                    .take(30)
                    .collect::<Vec<_>>()
                    .into_iter()
                    .rev()
                    .collect::<Vec<_>>()
                    .join("\n");
                anyhow::bail!(
                    "timed out waiting for coop health endpoint at {url}\ncoop stderr (last 30 lines):\n{tail}"
                );
            }
            std::thread::sleep(Duration::from_millis(100));
        }
    }
}

impl Drop for CoopScenario {
    fn drop(&mut self) {
        let _ = self.child.kill();
        let _ = self.child.wait();
    }
}

/// Minimal screen response for test comparison.
#[derive(Debug, serde::Deserialize)]
pub struct ScreenApiResponse {
    pub lines: Vec<String>,
    pub ansi: Vec<String>,
    pub cols: u16,
    pub rows: u16,
    pub alt_screen: bool,
    pub seq: u64,
}

// -- Helpers ------------------------------------------------------------------

/// Find a free TCP port.
fn find_free_port() -> anyhow::Result<u16> {
    let listener = std::net::TcpListener::bind("127.0.0.1:0")?;
    let port = listener.local_addr()?.port();
    drop(listener);
    Ok(port)
}

/// Find the coop binary. First check COOP_BIN env, then try cargo-built path.
fn coop_binary_path() -> anyhow::Result<String> {
    if let Ok(path) = std::env::var("COOP_BIN") {
        return Ok(path);
    }

    // Try the cargo target directory.
    let manifest_dir = PathBuf::from(env!("CARGO_MANIFEST_DIR"));
    let target_debug =
        manifest_dir.parent().and_then(|p| p.parent()).map(|p| p.join("target/debug/coop"));

    if let Some(path) = target_debug {
        if path.exists() {
            return Ok(path.display().to_string());
        }
    }

    // Fallback to PATH.
    if has_binary("coop") {
        return Ok("coop".to_owned());
    }

    anyhow::bail!("coop binary not found — set COOP_BIN or run `cargo build` first")
}

/// Normalize lines for comparison: trim trailing whitespace per-line, then
/// strip trailing empty lines.
pub fn normalize_lines(lines: &[String]) -> Vec<String> {
    let trimmed: Vec<String> = lines.iter().map(|l| l.trim_end().to_owned()).collect();
    let last_non_empty = trimmed.iter().rposition(|l| !l.is_empty()).map(|i| i + 1).unwrap_or(0);
    trimmed[..last_non_empty].to_vec()
}

/// Compare two line sets with allowance for shell prompt differences.
///
/// Tmux sessions include a shell prompt line that coop doesn't have.
/// This helper finds the intersection of non-empty, non-prompt lines.
pub fn compare_output_region(
    label_a: &str,
    lines_a: &[String],
    label_b: &str,
    lines_b: &[String],
    sentinel: &str,
) -> anyhow::Result<()> {
    let norm_a = normalize_lines(lines_a);
    let norm_b = normalize_lines(lines_b);

    // Find sentinel in both outputs.
    let a_idx = norm_a.iter().position(|l| l.contains(sentinel));
    let b_idx = norm_b.iter().position(|l| l.contains(sentinel));

    let a_start = a_idx.ok_or_else(|| {
        anyhow::anyhow!("{label_a} doesn't contain sentinel {sentinel:?}: {norm_a:?}")
    })?;
    let b_start = b_idx.ok_or_else(|| {
        anyhow::anyhow!("{label_b} doesn't contain sentinel {sentinel:?}: {norm_b:?}")
    })?;

    // Compare from sentinel line onwards, skipping empty/prompt lines.
    let a_content: Vec<&String> =
        norm_a[a_start..].iter().filter(|l| !l.is_empty() && !is_shell_prompt(l)).collect();
    let b_content: Vec<&String> =
        norm_b[b_start..].iter().filter(|l| !l.is_empty() && !is_shell_prompt(l)).collect();

    let min_len = a_content.len().min(b_content.len());
    if min_len == 0 {
        anyhow::bail!("no comparable content found after sentinel");
    }

    for i in 0..min_len {
        if a_content[i].trim() != b_content[i].trim() {
            anyhow::bail!(
                "line {i} differs:\n  {label_a}: {:?}\n  {label_b}: {:?}",
                a_content[i],
                b_content[i]
            );
        }
    }

    Ok(())
}

/// Heuristic: does this line look like a shell prompt?
fn is_shell_prompt(line: &str) -> bool {
    let trimmed = line.trim();
    trimmed.ends_with('$')
        || trimmed.ends_with('#')
        || trimmed.ends_with('%')
        || trimmed.starts_with("bash-")
        || trimmed.starts_with("sh-")
        || trimmed.contains("$ ")
}
