// SPDX-License-Identifier: BUSL-1.1
// Copyright (c) 2026 Matt Baker <fimbaz@gmail.com>

//! `coop attach` — interactive terminal client for a running coop server.
//!
//! Connects to a coop server via WebSocket, puts the local terminal in raw
//! mode, and proxies I/O between the user's terminal and the remote session.
//! Detach with Ctrl+] (0x1d).
//!
//! When a statusline is configured (via `--statusline-cmd` or the default
//! built-in), the bottom row of the terminal is reserved for a status bar
//! using DECSTBM scroll region margins.

use std::io::Write;
use std::sync::{Mutex, Once};
use std::time::{Duration, Instant};

use base64::Engine;
use futures_util::future::Either;
use futures_util::{SinkExt, StreamExt};
use nix::sys::termios;
use tokio::sync::mpsc;

use crate::replay_gate::ReplayGate;
use crate::transport::ws::{ClientMessage, ServerMessage};

/// CLI arguments for `coop attach`.
#[derive(Debug, clap::Args)]
pub struct AttachArgs {
    /// Server URL (e.g. http://127.0.0.1:8080).
    #[arg(env = "COOP_URL")]
    url: Option<String>,

    /// Unix socket path for local connection.
    #[arg(long, env = "COOP_SOCKET")]
    socket: Option<String>,

    /// Auth token for the coop server.
    #[arg(long, env = "COOP_AUTH_TOKEN")]
    auth_token: Option<String>,

    /// Disable the statusline.
    #[arg(long)]
    no_statusline: bool,

    /// Shell command for statusline content (default: built-in).
    #[arg(long, env = "COOP_STATUSLINE_CMD")]
    statusline_cmd: Option<String>,

    /// Statusline refresh interval in seconds.
    #[arg(long, env = "COOP_STATUSLINE_INTERVAL", default_value_t = DEFAULT_STATUSLINE_INTERVAL)]
    statusline_interval: u64,

    /// Maximum reconnection attempts (0 = disable).
    #[arg(long, default_value_t = 10)]
    max_reconnects: u32,
}

/// Detach key: Ctrl+] (ASCII 0x1d), same as telnet / docker attach.
const DETACH_KEY: u8 = 0x1d;

/// Refresh key: Ctrl+L (ASCII 0x0c), traditional terminal clear/redraw.
const REFRESH_KEY: u8 = 0x0c;

/// Enter alternate screen buffer (SMCUP). Isolates coop's display from the
/// user's shell scrollback so detaching cleanly restores the original content.
const SMCUP: &[u8] = b"\x1b[?1049h";

/// Exit alternate screen buffer (RMCUP). Restores the primary screen buffer
/// and cursor position, bringing back the user's original shell content.
const RMCUP: &[u8] = b"\x1b[?1049l";

/// Clear screen and move cursor home. Used after entering the alternate screen buffer to start with a blank slate.
const CLEAR_HOME: &[u8] = b"\x1b[2J\x1b[H";

/// Begin synchronized update (DEC private mode 2026). Tells the terminal to
/// batch subsequent output and render it atomically, preventing flicker during large redraws.
/// Supported by Ghostty, kitty, iTerm2, WezTerm, foot.
/// Unsupported terminals ignore the sequence harmlessly.
const SYNC_START: &[u8] = b"\x1b[?2026h";
const SYNC_END: &[u8] = b"\x1b[?2026l";

/// Saved terminal state for panic-time restoration.
/// Populated when entering raw mode, cleared on drop.
static PANIC_TERMIOS: Mutex<Option<nix::libc::termios>> = Mutex::new(None);
static PANIC_HOOK_INSTALLED: Once = Once::new();

const DEFAULT_STATUSLINE_INTERVAL: u64 = 5;
const PING_INTERVAL: Duration = Duration::from_secs(30);

/// WebSocket stream over a TCP (possibly TLS) connection.
type TcpWs =
    tokio_tungstenite::WebSocketStream<tokio_tungstenite::MaybeTlsStream<tokio::net::TcpStream>>;

/// WebSocket stream over a Unix domain socket.
type UnixWs = tokio_tungstenite::WebSocketStream<tokio::net::UnixStream>;

struct StatuslineConfig {
    /// Shell command to run for statusline content. None = built-in.
    cmd: Option<String>,
    /// Refresh interval.
    interval: Duration,
    /// Whether statusline is enabled at all.
    enabled: bool,
}

impl From<&AttachArgs> for StatuslineConfig {
    fn from(args: &AttachArgs) -> Self {
        Self {
            cmd: args.statusline_cmd.clone(),
            interval: Duration::from_secs(args.statusline_interval),
            enabled: !args.no_statusline,
        }
    }
}

enum SessionResult {
    Exited(i32),
    Detached,
    Disconnected(String),
}

/// State tracked across reconnects.
struct AttachState {
    agent_state: String,
    /// Current tool name + label parsed from the cause string (e.g. "Bash: git status").
    /// Cleared when state transitions away from Working.
    tool_info: Option<String>,
    cols: u16,
    rows: u16,
    started: Instant,
    /// Offset-gated dedup for interleaved Replay/Pty messages.
    gate: ReplayGate,
    /// True while waiting for the first Replay response after (re)connect.
    /// Used to end the synchronized update that wraps the initial redraw.
    sync_pending: bool,
}

impl AttachState {
    fn new(cols: u16, rows: u16) -> Self {
        Self {
            agent_state: "unknown".to_owned(),
            tool_info: None,
            cols,
            rows,
            started: Instant::now(),
            gate: ReplayGate::new(),
            sync_pending: false,
        }
    }

    fn uptime_secs(&self) -> u64 {
        self.started.elapsed().as_secs()
    }

    /// Prepare the gate for a fresh (re)connect replay and return the offset to
    /// request from the server.
    ///
    /// This MUST reset the gate. The `ReplayGate` high-water mark persists in
    /// `AttachState` across reconnects, but each new upstream connection starts
    /// a fresh replay stream — after an upstream restart the new server's ring
    /// begins at a low offset, far below a carried-over high-water mark. Without
    /// a reset, `on_replay`/`on_pty` would drop the restarted server's replay
    /// and every subsequent live frame as "already seen", leaving `sync_pending`
    /// set forever (SYNC_END never emitted) — a connected-but-frozen blank
    /// terminal with all output silently discarded. Resetting and replaying from
    /// 0 mirrors the Ctrl+L / resize redraw paths (`begin_sync_redraw`) and
    /// forces a correct full redraw. See ka-0000003224.
    fn begin_reconnect_replay(&mut self) -> u64 {
        self.gate.reset();
        self.sync_pending = true;
        0
    }

    /// Update state from a Transition message. Extracts tool info from the
    /// cause string when the state is Working.
    fn apply_transition(&mut self, next: String, cause: &str) {
        self.agent_state = next;
        self.tool_info =
            if self.agent_state == "working" { parse_tool_from_cause(cause) } else { None };
    }
}

/// Parse tool name and optional label from a cause string.
///
/// Cause format: `"hook:tool(Bash: git status)"` or `"log:tool(Read: main.rs)"`
/// Returns the parenthesized content (e.g. `"Bash: git status"`, `"Read: main.rs"`).
/// Returns `None` for non-tool causes or empty tool names.
fn parse_tool_from_cause(cause: &str) -> Option<String> {
    // Find ":tool(" — the prefix varies (hook, log, stdout).
    let start = cause.find(":tool(")? + 6;
    let end = cause.rfind(')')?;
    if end <= start {
        return None;
    }
    let content = &cause[start..end];
    if content.is_empty() {
        return None;
    }
    Some(content.to_owned())
}

/// RAII guard that restores terminal attributes on drop.
struct RawModeGuard {
    original: termios::Termios,
}

impl RawModeGuard {
    fn enter() -> anyhow::Result<Self> {
        let stdin = std::io::stdin();
        let original = termios::tcgetattr(&stdin)?;
        let mut raw = original.clone();
        termios::cfmakeraw(&mut raw);
        termios::tcsetattr(&stdin, termios::SetArg::TCSAFLUSH, &raw)?;
        Ok(Self { original })
    }
}

impl Drop for RawModeGuard {
    fn drop(&mut self) {
        // Clear the panic hook's termios state — we're restoring normally.
        if let Ok(mut guard) = PANIC_TERMIOS.lock() {
            *guard = None;
        }
        let _ = termios::tcsetattr(std::io::stdin(), termios::SetArg::TCSAFLUSH, &self.original);
    }
}

fn terminal_size() -> Option<(u16, u16)> {
    let ws = rustix::termios::tcgetwinsize(std::io::stdout()).ok()?;
    if ws.ws_col > 0 && ws.ws_row > 0 {
        Some((ws.ws_col, ws.ws_row))
    } else {
        None
    }
}

fn enter_alt_screen(stdout: &mut std::io::Stdout) {
    let _ = stdout.write_all(SMCUP);
    let _ = stdout.write_all(SYNC_START);
    let _ = stdout.write_all(CLEAR_HOME);
    let _ = stdout.flush();
}

fn exit_alt_screen(stdout: &mut std::io::Stdout) {
    let _ = stdout.write_all(RMCUP);
    let _ = stdout.flush();
}

fn set_scroll_region(stdout: &mut std::io::Stdout, content_rows: u16) {
    let _ = write!(stdout, "\x1b[1;{content_rows}r\x1b[H");
    let _ = stdout.flush();
}

fn reset_scroll_region(stdout: &mut std::io::Stdout) {
    let _ = write!(stdout, "\x1b[r");
    let _ = stdout.flush();
}

/// Render statusline on the bottom row (save cursor, reverse video, restore).
fn render_statusline(stdout: &mut std::io::Stdout, content: &str, cols: u16, rows: u16) {
    let max = cols as usize;
    let truncated =
        if content.len() > max { &content[..content.floor_char_boundary(max)] } else { content };
    let _ =
        write!(stdout, "\x1b7\x1b[{rows};1H\x1b[7m{truncated:<width$}\x1b[0m\x1b8", width = max);
    let _ = stdout.flush();
}

fn builtin_statusline(state: &AttachState) -> String {
    let mut line = format!(
        " [coop] {} | {}s | {}x{}",
        state.agent_state,
        state.uptime_secs(),
        state.cols,
        state.rows
    );
    if let Some(ref tool) = state.tool_info {
        line.push_str(" | ");
        line.push_str(tool);
    }
    line
}

/// Run a shell command with template expansion ({state}, {cols}, {rows}, {uptime}, {tool}).
async fn run_statusline_cmd(cmd: &str, state: &AttachState) -> String {
    let tool_str = state.tool_info.as_deref().unwrap_or("");
    let expanded = cmd
        .replace("{state}", &state.agent_state)
        .replace("{cols}", &state.cols.to_string())
        .replace("{rows}", &state.rows.to_string())
        .replace("{uptime}", &state.uptime_secs().to_string())
        .replace("{tool}", tool_str);
    match tokio::process::Command::new("sh")
        .args(["-c", &expanded])
        .stdout(std::process::Stdio::piped())
        .stderr(std::process::Stdio::null())
        .output()
        .await
    {
        Ok(out) if out.status.success() => String::from_utf8_lossy(&out.stdout).trim().to_owned(),
        _ => format!(" [coop] statusline cmd failed: {cmd}"),
    }
}

/// Run `coop attach`. Returns a process exit code.
pub async fn run(args: AttachArgs) -> i32 {
    if args.url.is_none() && args.socket.is_none() {
        eprintln!("error: COOP_URL is not set and no URL or --socket argument provided");
        return 2;
    }

    let sl_cfg = StatuslineConfig::from(&args);
    attach(
        args.url.as_deref(),
        args.socket.as_deref(),
        args.auth_token.as_deref(),
        &sl_cfg,
        args.max_reconnects,
    )
    .await
}

fn build_ws_url(base_url: &str) -> String {
    let base = base_url.trim_end_matches('/');
    let scheme = if base.starts_with("https://") { "wss" } else { "ws" };
    let host =
        base.strip_prefix("https://").or_else(|| base.strip_prefix("http://")).unwrap_or(base);
    format!("{scheme}://{host}/ws?subscribe=pty,state")
}

/// Connect over Unix socket (preferred) or TCP. The `ws://localhost` URI over
/// UDS is a formality — tungstenite needs it for the HTTP upgrade handshake.
async fn connect_ws(
    url: Option<&str>,
    socket: Option<&str>,
) -> Result<Either<TcpWs, UnixWs>, String> {
    if let Some(path) = socket {
        let stream =
            tokio::net::UnixStream::connect(path).await.map_err(|e| format!("{path}: {e}"))?;
        let ws_url = "ws://localhost/ws?subscribe=pty,state";
        let (ws_stream, _response) =
            tokio_tungstenite::client_async(ws_url, stream).await.map_err(|e| format!("{e}"))?;
        return Ok(Either::Right(ws_stream));
    }

    let base_url = url.ok_or("no URL or socket provided")?;
    let ws_url = build_ws_url(base_url);
    let (stream, _response) =
        tokio_tungstenite::connect_async(&ws_url).await.map_err(|e| format!("{e}"))?;
    Ok(Either::Left(stream))
}

async fn attach(
    url: Option<&str>,
    socket: Option<&str>,
    auth_token: Option<&str>,
    sl_cfg: &StatuslineConfig,
    max_reconnects: u32,
) -> i32 {
    // Try the initial connection BEFORE entering raw mode so a connection
    // failure doesn't disturb the terminal.
    let initial_ws = match connect_ws(url, socket).await {
        Ok(s) => s,
        Err(e) => {
            eprintln!("error: WebSocket connection failed: {e}");
            return 1;
        }
    };

    // Enter raw mode (persists across reconnects).
    let raw_guard = match RawModeGuard::enter() {
        Ok(g) => g,
        Err(e) => {
            eprintln!("error: failed to enter raw mode: {e}");
            return 1;
        }
    };

    // Install a panic hook (once) to restore the terminal even on unwind.
    {
        let raw_termios: nix::libc::termios = raw_guard.original.clone().into();
        if let Ok(mut guard) = PANIC_TERMIOS.lock() {
            *guard = Some(raw_termios);
        }
    }
    PANIC_HOOK_INSTALLED.call_once(|| {
        let prev_hook = std::panic::take_hook();
        std::panic::set_hook(Box::new(move |info| {
            if let Ok(mut guard) = PANIC_TERMIOS.lock() {
                if let Some(ref termios) = *guard {
                    // Exit alternate screen before restoring termios so the
                    // user's original shell content reappears.
                    // SAFETY: Writing to stdout fd 1 in panic hook; fd remains
                    // valid for the lifetime of the process.
                    #[allow(unsafe_code)]
                    unsafe {
                        nix::libc::write(1, RMCUP.as_ptr().cast(), RMCUP.len());
                    }
                    // SAFETY: Restoring terminal attributes in panic hook; stdin
                    // fd 0 remains valid for the lifetime of the process.
                    #[allow(unsafe_code)]
                    unsafe {
                        nix::libc::tcsetattr(0, nix::libc::TCSAFLUSH, termios);
                    }
                    *guard = None;
                }
            }
            prev_hook(info);
        }));
    });

    let mut stdout = std::io::stdout();

    // Enter alternate screen buffer — isolates coop display from shell scrollback.
    enter_alt_screen(&mut stdout);

    // Determine initial terminal size.
    let (init_cols, init_rows) = terminal_size().unwrap_or((80, 24));
    let mut state = AttachState::new(init_cols, init_rows);
    let mut sl_active = sl_cfg.enabled && init_rows > 2;

    // Spawn a blocking thread to read stdin (lives across reconnects).
    let (stdin_tx, mut stdin_rx) = mpsc::channel::<Vec<u8>>(64);
    std::thread::spawn(move || {
        use std::io::Read;
        let stdin = std::io::stdin();
        let mut handle = stdin.lock();
        let mut buf = [0u8; 4096];
        loop {
            match handle.read(&mut buf) {
                Ok(0) => break,
                Ok(n) => {
                    if stdin_tx.blocking_send(buf[..n].to_vec()).is_err() {
                        break;
                    }
                }
                Err(_) => break,
            }
        }
    });

    // SIGWINCH handler for terminal resize (lives across reconnects).
    let mut sigwinch =
        tokio::signal::unix::signal(tokio::signal::unix::SignalKind::window_change()).ok();

    let mut attempt: u32 = 0;
    let mut pending_ws = Some(initial_ws);
    let exit_code;

    loop {
        // Use the pre-connected stream on first iteration, reconnect after.
        let ws_stream = if let Some(ws) = pending_ws.take() {
            ws
        } else {
            match connect_ws(url, socket).await {
                Ok(s) => s,
                Err(e) => {
                    attempt += 1;
                    if attempt > max_reconnects {
                        if sl_active {
                            reset_scroll_region(&mut stdout);
                        }
                        exit_alt_screen(&mut stdout);
                        drop(raw_guard);
                        eprintln!("coop attach: max reconnects reached, giving up.");
                        return 1;
                    }
                    let backoff = reconnect_backoff(attempt);
                    let _ = write!(
                        stdout,
                        "\r\ncoop attach: connection failed ({e}), retrying in {:.1}s...\r\n",
                        backoff.as_secs_f64()
                    );
                    let _ = stdout.flush();
                    tokio::time::sleep(backoff).await;
                    continue;
                }
            }
        };

        let (mut ws_tx, mut ws_rx) = ws_stream.split();

        // Post-connect handshake: Auth → Resize → Replay → GetAgent.
        if let Some(token) = auth_token {
            let _ = send_msg(&mut ws_tx, &ClientMessage::Auth { token: token.to_owned() }).await;
        }

        let content_rows = if sl_active && state.rows > 2 {
            set_scroll_region(&mut stdout, state.rows - 1);
            state.rows - 1
        } else {
            state.rows
        };
        let _ =
            send_msg(&mut ws_tx, &ClientMessage::Resize { cols: state.cols, rows: content_rows })
                .await;

        // Begin synchronized redraw for initial replay. Reset the gate so the
        // upcoming replay is treated as first and a stale high-water mark from
        // the previous connection cannot suppress a restarted server's output
        // (ka-0000003224).
        let _ = stdout.write_all(SYNC_START);
        let _ = stdout.write_all(CLEAR_HOME);
        let _ = stdout.flush();
        let replay_offset = state.begin_reconnect_replay();
        let _ =
            send_msg(&mut ws_tx, &ClientMessage::GetReplay { offset: replay_offset, limit: None })
                .await;

        let mut ctx = AttachContext {
            state: &mut state,
            sl_active: &mut sl_active,
            sl_cfg,
            stdin_rx: &mut stdin_rx,
            sigwinch: &mut sigwinch,
            stdout: &mut stdout,
        };
        if *ctx.sl_active {
            let _ = send_msg(&mut ws_tx, &ClientMessage::GetAgent {}).await;
        }
        ctx.refresh_statusline().await;

        let result = connect_and_run(&mut ws_tx, &mut ws_rx, &mut ctx).await;
        let _ = ws_tx.send(tokio_tungstenite::tungstenite::Message::Close(None)).await;

        match result {
            SessionResult::Exited(code) => {
                exit_code = code;
                break;
            }
            SessionResult::Detached => {
                exit_code = 0;
                break;
            }
            SessionResult::Disconnected(reason) => {
                attempt += 1;
                let give_up = max_reconnects == 0 || attempt > max_reconnects;
                if sl_active {
                    reset_scroll_region(&mut stdout);
                }
                if give_up {
                    exit_alt_screen(&mut stdout);
                    drop(raw_guard);
                    let why = if max_reconnects == 0 {
                        reason
                    } else {
                        "max reconnects reached".to_owned()
                    };
                    eprintln!("coop attach: {why}");
                    return 1;
                }
                let backoff = reconnect_backoff(attempt);
                let _ = write!(
                    stdout,
                    "\r\ncoop attach: reconnecting ({attempt}/{max_reconnects}) in {:.1}s...\r\n",
                    backoff.as_secs_f64()
                );
                let _ = stdout.flush();
                tokio::time::sleep(backoff).await;
            }
        }
    }

    if sl_active {
        reset_scroll_region(&mut stdout);
    }
    exit_alt_screen(&mut stdout);
    drop(raw_guard);
    eprintln!("detached from coop session.");
    exit_code
}

/// Exponential backoff: 500ms * 2^attempt, capped at 10s.
fn reconnect_backoff(attempt: u32) -> Duration {
    Duration::from_millis(500u64.saturating_mul(1u64 << attempt.min(20)).min(10_000))
}

/// Mutable context passed to `connect_and_run`, grouping resources that
/// persist across reconnects.
struct AttachContext<'a> {
    state: &'a mut AttachState,
    sl_active: &'a mut bool,
    sl_cfg: &'a StatuslineConfig,
    stdin_rx: &'a mut mpsc::Receiver<Vec<u8>>,
    sigwinch: &'a mut Option<tokio::signal::unix::Signal>,
    stdout: &'a mut std::io::Stdout,
}

impl AttachContext<'_> {
    /// Process a Replay message through the gate and write the unseen suffix.
    fn write_replay_data(&mut self, data: &str, offset: u64, next_offset: u64) {
        if let Ok(decoded) = base64::engine::general_purpose::STANDARD.decode(data) {
            let Some(action) = self.state.gate.on_replay(decoded.len(), offset, next_offset) else {
                return;
            };
            let _ = self.stdout.write_all(&decoded[action.skip..]);
            if self.state.sync_pending {
                self.state.sync_pending = false;
                let _ = self.stdout.write_all(SYNC_END);
            }
            let _ = self.stdout.flush();
        }
    }

    /// Process a Pty broadcast message through the gate and write the unseen suffix.
    fn write_pty_data(&mut self, data: &str, offset: u64) {
        if let Ok(decoded) = base64::engine::general_purpose::STANDARD.decode(data) {
            let Some(skip) = self.state.gate.on_pty(decoded.len(), offset) else {
                return;
            };
            let _ = self.stdout.write_all(&decoded[skip..]);
            if self.state.sync_pending {
                self.state.sync_pending = false;
                let _ = self.stdout.write_all(SYNC_END);
            }
            let _ = self.stdout.flush();
        }
    }

    /// Refresh the statusline bar (no-op if statusline is inactive).
    async fn refresh_statusline(&mut self) {
        if !*self.sl_active {
            return;
        }
        let content = match &self.sl_cfg.cmd {
            Some(cmd) => run_statusline_cmd(cmd, self.state).await,
            None => builtin_statusline(self.state),
        };
        render_statusline(self.stdout, &content, self.state.cols, self.state.rows);
    }

    /// Begin a synchronized redraw: sync-start + clear + optional scroll region.
    /// Resets the replay gate so the upcoming replay is treated as first.
    fn begin_sync_redraw(&mut self) {
        self.state.gate.reset();
        let _ = self.stdout.write_all(SYNC_START);
        let _ = self.stdout.write_all(CLEAR_HOME);
        if *self.sl_active {
            set_scroll_region(self.stdout, self.state.rows - 1);
        }
        let _ = self.stdout.flush();
        self.state.sync_pending = true;
    }
}

/// Event loop for a single WebSocket connection. Returns on session end,
/// user detach, or connection loss.
async fn connect_and_run<WsTx, WsRx>(
    ws_tx: &mut WsTx,
    ws_rx: &mut WsRx,
    ctx: &mut AttachContext<'_>,
) -> SessionResult
where
    WsTx: SinkExt<tokio_tungstenite::tungstenite::Message> + Unpin,
    WsRx: StreamExt<
            Item = Result<
                tokio_tungstenite::tungstenite::Message,
                tokio_tungstenite::tungstenite::Error,
            >,
        > + Unpin,
{
    // Statusline refresh timer.
    let mut sl_interval = tokio::time::interval(ctx.sl_cfg.interval);
    sl_interval.tick().await; // Consume the immediate first tick.

    // Ping keepalive timer.
    let mut ping_interval = tokio::time::interval(PING_INTERVAL);
    ping_interval.tick().await; // Consume the immediate first tick.

    loop {
        tokio::select! {
            // Incoming WebSocket messages.
            msg = ws_rx.next() => {
                match msg {
                    Some(Ok(tokio_tungstenite::tungstenite::Message::Text(text))) => {
                        match serde_json::from_str::<ServerMessage>(&text) {
                            Ok(ServerMessage::Replay { data, offset, next_offset, .. }) => {
                                ctx.write_replay_data(&data, offset, next_offset);
                            }
                            Ok(ServerMessage::Pty { data, offset, .. }) => {
                                ctx.write_pty_data(&data, offset);
                            }
                            Ok(ServerMessage::Exit { code, .. }) => {
                                let deadline = tokio::time::Instant::now() + Duration::from_millis(200);
                                while let Ok(Some(Ok(tokio_tungstenite::tungstenite::Message::Text(text)))) =
                                    tokio::time::timeout_at(deadline, ws_rx.next()).await
                                {
                                    match serde_json::from_str(&text) {
                                        Ok(ServerMessage::Replay { data, offset, next_offset, .. }) => {
                                            ctx.write_replay_data(&data, offset, next_offset);
                                        }
                                        Ok(ServerMessage::Pty { data, offset, .. }) => {
                                            ctx.write_pty_data(&data, offset);
                                        }
                                        _ => {}
                                    }
                                }
                                return SessionResult::Exited(code.unwrap_or(0));
                            }
                            Ok(ServerMessage::Error { code, message }) => {
                                return SessionResult::Disconnected(format!("[{code}] {message}"));
                            }
                            Ok(ServerMessage::Transition { next, cause, .. }) => {
                                ctx.state.apply_transition(next, &cause);
                                ctx.refresh_statusline().await;
                            }
                            _ => {}
                        }
                    }
                    Some(Ok(tokio_tungstenite::tungstenite::Message::Close(_))) | None => {
                        return SessionResult::Disconnected("connection closed".to_owned());
                    }
                    Some(Err(e)) => return SessionResult::Disconnected(format!("{e}")),
                    _ => {}
                }
            }

            // Local stdin input.
            data = ctx.stdin_rx.recv() => {
                let Some(bytes) = data else {
                    return SessionResult::Disconnected("stdin closed".to_owned());
                };
                if let Some(pos) = bytes.iter().position(|&b| b == DETACH_KEY) {
                    if pos > 0 {
                        let _ = send_raw(ws_tx, &bytes[..pos]).await;
                    }
                    return SessionResult::Detached;
                }
                if bytes.contains(&REFRESH_KEY) {
                    let filtered: Vec<u8> = bytes.iter().copied().filter(|&b| b != REFRESH_KEY).collect();
                    if !filtered.is_empty() {
                        let _ = send_raw(ws_tx, &filtered).await;
                    }
                    ctx.begin_sync_redraw();
                    let _ = send_msg(ws_tx, &ClientMessage::GetReplay { offset: 0, limit: None }).await;
                    continue;
                }
                if send_raw(ws_tx, &bytes).await.is_err() {
                    return SessionResult::Disconnected("send failed".to_owned());
                }
            }

            // Terminal resize — triggers full redraw.
            _ = async {
                match ctx.sigwinch.as_mut() {
                    Some(s) => { s.recv().await; }
                    None => std::future::pending::<()>().await,
                }
            } => {
                if let Some((cols, rows)) = terminal_size() {
                    ctx.state.cols = cols;
                    ctx.state.rows = rows;
                    let was_active = *ctx.sl_active;
                    *ctx.sl_active = ctx.sl_cfg.enabled && rows > 2;

                    if was_active || *ctx.sl_active {
                        reset_scroll_region(ctx.stdout);
                    }
                    let content_rows = if *ctx.sl_active { set_scroll_region(ctx.stdout, rows - 1); rows - 1 } else { rows };
                    let _ = send_msg(ws_tx, &ClientMessage::Resize { cols, rows: content_rows }).await;
                    ctx.begin_sync_redraw();
                    ctx.refresh_statusline().await;
                    let _ = send_msg(ws_tx, &ClientMessage::GetReplay { offset: 0, limit: None }).await;
                }
            }

            _ = sl_interval.tick(), if *ctx.sl_active => { ctx.refresh_statusline().await; }
            _ = ping_interval.tick() => { let _ = send_msg(ws_tx, &ClientMessage::Ping {}).await; }
        }
    }
}

async fn send_msg<S>(tx: &mut S, msg: &ClientMessage) -> Result<(), String>
where
    S: SinkExt<tokio_tungstenite::tungstenite::Message> + Unpin,
{
    let text = serde_json::to_string(msg).map_err(|e| e.to_string())?;
    tx.send(tokio_tungstenite::tungstenite::Message::Text(text.into()))
        .await
        .map_err(|_| "WebSocket send failed".to_owned())
}

async fn send_raw<S>(tx: &mut S, bytes: &[u8]) -> Result<(), String>
where
    S: SinkExt<tokio_tungstenite::tungstenite::Message> + Unpin,
{
    let data = base64::engine::general_purpose::STANDARD.encode(bytes);
    send_msg(tx, &ClientMessage::SendInputRaw { data }).await
}

#[cfg(test)]
#[path = "attach_tests.rs"]
mod tests;
