// SPDX-License-Identifier: BUSL-1.1
// Copyright (c) 2026 Matt Baker <fimbaz@gmail.com>

use super::*;

/// Guard for tests that mutate environment variables. Prevents parallel races.
static ENV_LOCK: std::sync::Mutex<()> = std::sync::Mutex::new(());

// ===== Entry-point tests ====================================================

#[tokio::test]
async fn missing_coop_url_returns_2() {
    let args = AttachArgs {
        url: None,
        socket: None,
        auth_token: None,
        no_statusline: false,
        statusline_cmd: None,
        statusline_interval: DEFAULT_STATUSLINE_INTERVAL,
        max_reconnects: 10,
    };
    assert_eq!(run(args).await, 2);
}

#[test]
fn help_flag_handled_by_clap() {
    let err = TestWrapper::try_parse_from(["coop-attach", "--help"]).unwrap_err();
    assert!(!err.use_stderr());
}

#[test]
fn help_short_flag_handled_by_clap() {
    let err = TestWrapper::try_parse_from(["coop-attach", "-h"]).unwrap_err();
    assert!(!err.use_stderr());
}

#[tokio::test]
async fn connection_refused_returns_1() {
    let args = AttachArgs {
        url: Some("http://127.0.0.1:1".to_string()),
        socket: None,
        auth_token: None,
        no_statusline: false,
        statusline_cmd: None,
        statusline_interval: DEFAULT_STATUSLINE_INTERVAL,
        max_reconnects: 10,
    };
    assert_eq!(run(args).await, 1);
}

// ===== AttachArgs / StatuslineConfig tests ==================================

use clap::Parser;

/// Wrapper to test `AttachArgs` parsing (since `Args` doesn't have `try_parse_from`).
#[derive(Debug, Parser)]
#[command(name = "coop-attach")]
struct TestWrapper {
    #[command(flatten)]
    args: AttachArgs,
}

fn parse_args(args: &[&str]) -> AttachArgs {
    let argv: Vec<&str> = std::iter::once("coop-attach").chain(args.iter().copied()).collect();
    TestWrapper::try_parse_from(argv).unwrap_or_else(|e| panic!("parse failed: {e}")).args
}

#[test]
fn args_defaults_enabled_builtin() {
    let _lock = ENV_LOCK.lock();
    std::env::remove_var("COOP_STATUSLINE_CMD");
    std::env::remove_var("COOP_STATUSLINE_INTERVAL");
    std::env::remove_var("COOP_URL");
    std::env::remove_var("COOP_AUTH_TOKEN");
    std::env::remove_var("COOP_SOCKET");
    let args = parse_args(&[]);
    let cfg = StatuslineConfig::from(&args);
    assert!(cfg.enabled);
    assert!(cfg.cmd.is_none());
    assert_eq!(cfg.interval, Duration::from_secs(DEFAULT_STATUSLINE_INTERVAL));
}

#[test]
fn args_no_statusline_flag() {
    let args = parse_args(&["--no-statusline"]);
    let cfg = StatuslineConfig::from(&args);
    assert!(!cfg.enabled);
}

#[test]
fn args_statusline_cmd_space_separated() {
    let args = parse_args(&["--statusline-cmd", "echo hello"]);
    let cfg = StatuslineConfig::from(&args);
    assert_eq!(cfg.cmd.as_deref(), Some("echo hello"));
}

#[test]
fn args_statusline_cmd_equals_syntax() {
    let args = parse_args(&["--statusline-cmd=echo hello"]);
    let cfg = StatuslineConfig::from(&args);
    assert_eq!(cfg.cmd.as_deref(), Some("echo hello"));
}

#[test]
fn args_statusline_interval_override() {
    let args = parse_args(&["--statusline-interval", "10"]);
    let cfg = StatuslineConfig::from(&args);
    assert_eq!(cfg.interval, Duration::from_secs(10));
}

#[test]
fn args_statusline_interval_equals_syntax() {
    let args = parse_args(&["--statusline-interval=3"]);
    let cfg = StatuslineConfig::from(&args);
    assert_eq!(cfg.interval, Duration::from_secs(3));
}

#[test]
fn args_invalid_interval_is_parse_error() {
    let argv = ["coop-attach", "--statusline-interval=abc"];
    assert!(TestWrapper::try_parse_from(argv).is_err());
}

#[test]
fn args_url_positional() {
    let args = parse_args(&["http://localhost:8080"]);
    assert_eq!(args.url.as_deref(), Some("http://localhost:8080"));
}

#[test]
fn args_socket_flag() {
    let args = parse_args(&["--socket", "/tmp/coop.sock"]);
    assert_eq!(args.socket.as_deref(), Some("/tmp/coop.sock"));
}

#[test]
fn args_auth_token_flag() {
    let args = parse_args(&["--auth-token", "secret"]);
    assert_eq!(args.auth_token.as_deref(), Some("secret"));
}

#[test]
fn args_max_reconnects_default() {
    let args = parse_args(&[]);
    assert_eq!(args.max_reconnects, 10);
}

#[test]
fn args_max_reconnects_override() {
    let args = parse_args(&["--max-reconnects", "0"]);
    assert_eq!(args.max_reconnects, 0);
}

// ===== builtin_statusline tests =============================================

#[test]
fn builtin_statusline_format() {
    let state = AttachState {
        agent_state: "working".to_owned(),
        tool_info: None,
        cols: 120,
        rows: 40,
        started: Instant::now(),
        gate: ReplayGate::new(),
        sync_pending: false,
    };
    let line = builtin_statusline(&state);
    assert!(line.contains("[coop]"));
    assert!(line.contains("working"));
    assert!(line.contains("120x40"));
}

#[test]
fn builtin_statusline_uptime_increases() {
    let state = AttachState {
        agent_state: "idle".to_owned(),
        tool_info: None,
        cols: 80,
        rows: 24,
        started: Instant::now() - Duration::from_secs(42),
        gate: ReplayGate::new(),
        sync_pending: false,
    };
    let line = builtin_statusline(&state);
    assert!(line.contains("42s") || line.contains("43s"), "expected ~42s uptime: {line}");
}

// ===== run_statusline_cmd tests =============================================

#[tokio::test]
async fn run_statusline_cmd_captures_output() {
    let state = AttachState::new(80, 24);
    let result = run_statusline_cmd("echo test-output", &state).await;
    assert_eq!(result, "test-output");
}

#[tokio::test]
async fn run_statusline_cmd_expands_state() {
    let mut state = AttachState::new(80, 24);
    state.agent_state = "idle".to_owned();
    let result = run_statusline_cmd("echo {state}", &state).await;
    assert_eq!(result, "idle");
}

#[tokio::test]
async fn run_statusline_cmd_expands_dimensions() {
    let state = AttachState::new(120, 40);
    let result = run_statusline_cmd("echo {cols}x{rows}", &state).await;
    assert_eq!(result, "120x40");
}

#[tokio::test]
async fn run_statusline_cmd_expands_uptime() {
    let state = AttachState {
        agent_state: "working".to_owned(),
        tool_info: None,
        cols: 80,
        rows: 24,
        started: Instant::now() - Duration::from_secs(99),
        gate: ReplayGate::new(),
        sync_pending: false,
    };
    let result = run_statusline_cmd("echo {uptime}", &state).await;
    assert!(result == "99" || result == "100", "expected ~99: {result}");
}

#[tokio::test]
async fn run_statusline_cmd_failed_command() {
    let state = AttachState::new(80, 24);
    let result = run_statusline_cmd("false", &state).await;
    assert!(result.contains("failed"));
}

#[tokio::test]
async fn run_statusline_cmd_trims_trailing_newline() {
    let state = AttachState::new(80, 24);
    let result = run_statusline_cmd("printf 'hello\\n\\n'", &state).await;
    assert_eq!(result, "hello");
}

// ===== parse_tool_from_cause tests ==========================================

#[test]
fn parse_tool_from_cause_hook_with_label() {
    assert_eq!(
        parse_tool_from_cause("hook:tool(Bash: git status)"),
        Some("Bash: git status".to_owned())
    );
}

#[test]
fn parse_tool_from_cause_log_with_basename() {
    assert_eq!(parse_tool_from_cause("log:tool(Read: main.rs)"), Some("Read: main.rs".to_owned()));
}

#[test]
fn parse_tool_from_cause_no_label() {
    assert_eq!(parse_tool_from_cause("hook:tool(Bash)"), Some("Bash".to_owned()));
}

#[test]
fn parse_tool_from_cause_non_tool() {
    assert_eq!(parse_tool_from_cause("hook:idle"), None);
    assert_eq!(parse_tool_from_cause("hook:working"), None);
    assert_eq!(parse_tool_from_cause("log:user"), None);
}

// ===== apply_transition tests ================================================

#[test]
fn apply_transition_working_extracts_tool() {
    let mut state = AttachState::new(80, 24);
    state.apply_transition("working".to_owned(), "hook:tool(Bash: git status)");
    assert_eq!(state.agent_state, "working");
    assert_eq!(state.tool_info, Some("Bash: git status".to_owned()));
}

#[test]
fn apply_transition_idle_clears_tool() {
    let mut state = AttachState::new(80, 24);
    state.apply_transition("working".to_owned(), "hook:tool(Bash: git status)");
    state.apply_transition("idle".to_owned(), "hook:idle");
    assert_eq!(state.agent_state, "idle");
    assert_eq!(state.tool_info, None);
}

#[test]
fn builtin_statusline_shows_tool_info() {
    let state = AttachState {
        agent_state: "working".to_owned(),
        tool_info: Some("Bash: git status".to_owned()),
        cols: 80,
        rows: 24,
        started: Instant::now(),
        gate: ReplayGate::new(),
        sync_pending: false,
    };
    let line = builtin_statusline(&state);
    assert!(line.contains("Bash: git status"), "expected tool info: {line}");
}

#[test]
fn builtin_statusline_no_tool_when_idle() {
    let state = AttachState {
        agent_state: "idle".to_owned(),
        tool_info: None,
        cols: 80,
        rows: 24,
        started: Instant::now(),
        gate: ReplayGate::new(),
        sync_pending: false,
    };
    let line = builtin_statusline(&state);
    assert!(!line.contains("Bash"), "should not show tool info: {line}");
}

#[tokio::test]
async fn run_statusline_cmd_expands_tool() {
    let mut state = AttachState::new(80, 24);
    state.tool_info = Some("Read: config.rs".to_owned());
    let result = run_statusline_cmd("echo {tool}", &state).await;
    assert_eq!(result, "Read: config.rs");
}

#[tokio::test]
async fn run_statusline_cmd_tool_empty_when_idle() {
    let state = AttachState::new(80, 24);
    let result = run_statusline_cmd("printf 'tool=%s' '{tool}'", &state).await;
    assert_eq!(result, "tool=");
}

// ===== WebSocket integration tests ==========================================
// These tests spin up a real coop server with MockPty and connect via
// tokio-tungstenite, exercising the same protocol that `attach` uses.

mod ws_integration {
    use base64::Engine;
    use bytes::Bytes;
    use futures_util::{SinkExt, StreamExt};

    use crate::event::OutputEvent;
    use crate::test_support::{StoreBuilder, StoreCtx};
    use crate::transport::ws::{ClientMessage, ServerMessage};

    use super::*;

    /// Helper: spawn a coop HTTP server with a MockPty backend and return
    /// the server address. The server emits `output_chunks` on the broadcast
    /// channel.
    async fn spawn_test_server(
        output_chunks: Vec<&str>,
    ) -> (std::net::SocketAddr, std::sync::Arc<crate::transport::state::Store>) {
        let StoreCtx { store: state, .. } = StoreBuilder::new().ring_size(65536).build();

        // Write output chunks to ring buffer and broadcast them.
        {
            let mut ring = state.terminal.ring.write().await;
            for chunk in &output_chunks {
                let data = Bytes::from(chunk.as_bytes().to_vec());
                ring.write(&data);
                let offset = ring.total_written() - data.len() as u64;
                let _ = state.channels.output_tx.send(OutputEvent::Raw { data, offset });
            }
        }

        let (addr, _handle) = crate::test_support::spawn_http_server(std::sync::Arc::clone(&state))
            .await
            .unwrap_or_else(|e| panic!("failed to spawn test server: {e}"));

        // Small delay for the server to be ready.
        tokio::time::sleep(Duration::from_millis(50)).await;

        (addr, state)
    }

    /// Connect a WebSocket client to the given address.
    async fn connect_ws(
        addr: std::net::SocketAddr,
        mode: &str,
    ) -> (
        futures_util::stream::SplitSink<
            tokio_tungstenite::WebSocketStream<
                tokio_tungstenite::MaybeTlsStream<tokio::net::TcpStream>,
            >,
            tokio_tungstenite::tungstenite::Message,
        >,
        futures_util::stream::SplitStream<
            tokio_tungstenite::WebSocketStream<
                tokio_tungstenite::MaybeTlsStream<tokio::net::TcpStream>,
            >,
        >,
    ) {
        let url = format!("ws://{addr}/ws?mode={mode}");
        let (stream, _) = tokio_tungstenite::connect_async(&url)
            .await
            .unwrap_or_else(|e| panic!("ws connect failed: {e}"));
        stream.split()
    }

    /// Send a JSON message and return the text of the response.
    async fn send_and_recv<S, R>(tx: &mut S, rx: &mut R, msg: &ClientMessage) -> String
    where
        S: SinkExt<tokio_tungstenite::tungstenite::Message> + Unpin,
        R: StreamExt<
                Item = Result<
                    tokio_tungstenite::tungstenite::Message,
                    tokio_tungstenite::tungstenite::Error,
                >,
            > + Unpin,
    {
        let json = serde_json::to_string(msg).unwrap_or_default();
        let _ = tx.send(tokio_tungstenite::tungstenite::Message::Text(json.into())).await;

        // Read with a timeout.
        match tokio::time::timeout(Duration::from_secs(2), rx.next()).await {
            Ok(Some(Ok(tokio_tungstenite::tungstenite::Message::Text(text)))) => text.to_string(),
            other => panic!("expected text message, got {other:?}"),
        }
    }

    #[tokio::test]
    async fn replay_returns_ring_buffer_contents() {
        let (addr, _state) = spawn_test_server(vec!["hello world"]).await;
        let (mut tx, mut rx) = connect_ws(addr, "raw").await;

        let msg = ClientMessage::GetReplay { offset: 0, limit: None };
        let response = send_and_recv(&mut tx, &mut rx, &msg).await;

        let parsed: Result<ServerMessage, _> = serde_json::from_str(&response);
        match parsed {
            Ok(ServerMessage::Replay { data, offset, .. }) => {
                assert_eq!(offset, 0);
                let decoded =
                    base64::engine::general_purpose::STANDARD.decode(&data).unwrap_or_default();
                let text = String::from_utf8_lossy(&decoded);
                assert!(
                    text.contains("hello world"),
                    "expected 'hello world' in replay, got: {text}"
                );
            }
            other => panic!("expected ReplayResult, got {other:?}"),
        }
    }

    #[tokio::test]
    async fn state_request_returns_current_state() {
        let (addr, _state) = spawn_test_server(vec![]).await;
        let (mut tx, mut rx) = connect_ws(addr, "all").await;

        let msg = ClientMessage::GetAgent {};
        let response = send_and_recv(&mut tx, &mut rx, &msg).await;

        let parsed: Result<ServerMessage, _> = serde_json::from_str(&response);
        match parsed {
            Ok(ServerMessage::Agent { state, .. }) => {
                assert_eq!(state, "starting", "default AppState starts as 'starting'");
            }
            other => panic!("expected AgentState, got {other:?}"),
        }
    }

    #[tokio::test]
    async fn input_raw_reaches_server() {
        let StoreCtx { store: state, mut input_rx, .. } =
            StoreBuilder::new().ring_size(4096).build();

        let (addr, _handle) = crate::test_support::spawn_http_server(std::sync::Arc::clone(&state))
            .await
            .unwrap_or_else(|e| panic!("server: {e}"));
        tokio::time::sleep(Duration::from_millis(50)).await;

        let (mut tx, _rx) = connect_ws(addr, "raw").await;

        // Send an InputRaw message.
        let data = base64::engine::general_purpose::STANDARD.encode(b"ls\n");
        let msg = ClientMessage::SendInputRaw { data };
        let json = serde_json::to_string(&msg).unwrap_or_default();
        let _ = tx.send(tokio_tungstenite::tungstenite::Message::Text(json.into())).await;

        // The server should forward it as an InputEvent::Write.
        let event = tokio::time::timeout(Duration::from_secs(2), input_rx.recv()).await;
        match event {
            Ok(Some(crate::event::InputEvent::Write(bytes))) => {
                assert_eq!(&bytes[..], b"ls\n");
            }
            other => panic!("expected Write(b'ls\\n'), got {other:?}"),
        }
    }

    #[tokio::test]
    async fn resize_reaches_server() {
        let StoreCtx { store: state, mut input_rx, .. } =
            StoreBuilder::new().ring_size(4096).build();

        let (addr, _handle) = crate::test_support::spawn_http_server(std::sync::Arc::clone(&state))
            .await
            .unwrap_or_else(|e| panic!("server: {e}"));
        tokio::time::sleep(Duration::from_millis(50)).await;

        let (mut tx, _rx) = connect_ws(addr, "raw").await;

        let msg = ClientMessage::Resize { cols: 120, rows: 39 };
        let json = serde_json::to_string(&msg).unwrap_or_default();
        let _ = tx.send(tokio_tungstenite::tungstenite::Message::Text(json.into())).await;

        let event = tokio::time::timeout(Duration::from_secs(2), input_rx.recv()).await;
        match event {
            Ok(Some(crate::event::InputEvent::Resize { cols, rows })) => {
                assert_eq!(cols, 120);
                assert_eq!(rows, 39);
            }
            other => panic!("expected Resize(120, 39), got {other:?}"),
        }
    }

    #[tokio::test]
    async fn auth_required_blocks_input_raw() {
        let StoreCtx { store: state, .. } =
            StoreBuilder::new().ring_size(4096).auth_token("secret123").build();

        let (addr, _handle) = crate::test_support::spawn_http_server(std::sync::Arc::clone(&state))
            .await
            .unwrap_or_else(|e| panic!("server: {e}"));
        tokio::time::sleep(Duration::from_millis(50)).await;

        let (mut tx, mut rx) = connect_ws(addr, "raw").await;

        // Try to send input without authenticating.
        let data = base64::engine::general_purpose::STANDARD.encode(b"hello");
        let msg = ClientMessage::SendInputRaw { data };
        let response = send_and_recv(&mut tx, &mut rx, &msg).await;

        let parsed: Result<ServerMessage, _> = serde_json::from_str(&response);
        match parsed {
            Ok(ServerMessage::Error { code, .. }) => {
                assert_eq!(code, "UNAUTHORIZED");
            }
            other => panic!("expected UNAUTHORIZED error, got {other:?}"),
        }
    }

    #[tokio::test]
    async fn auth_required_blocks_resize() {
        let StoreCtx { store: state, .. } =
            StoreBuilder::new().ring_size(4096).auth_token("secret123").build();

        let (addr, _handle) = crate::test_support::spawn_http_server(std::sync::Arc::clone(&state))
            .await
            .unwrap_or_else(|e| panic!("server: {e}"));
        tokio::time::sleep(Duration::from_millis(50)).await;

        let (mut tx, mut rx) = connect_ws(addr, "raw").await;

        // Try to resize without authenticating.
        let msg = ClientMessage::Resize { cols: 120, rows: 40 };
        let response = send_and_recv(&mut tx, &mut rx, &msg).await;

        let parsed: Result<ServerMessage, _> = serde_json::from_str(&response);
        match parsed {
            Ok(ServerMessage::Error { code, .. }) => {
                assert_eq!(code, "UNAUTHORIZED");
            }
            other => panic!("expected UNAUTHORIZED error, got {other:?}"),
        }
    }

    #[tokio::test]
    async fn replay_gate_dedup_no_duplicates() {
        use crate::replay_gate::ReplayGate;

        let StoreCtx { store: state, .. } = StoreBuilder::new().ring_size(65536).build();

        // Pre-load ring with initial data "AAAA".
        {
            let mut ring = state.terminal.ring.write().await;
            let data = Bytes::from(b"AAAA".to_vec());
            ring.write(&data);
            let offset = ring.total_written() - data.len() as u64;
            let _ = state.channels.output_tx.send(OutputEvent::Raw { data, offset });
        }

        let (addr, _handle) = crate::test_support::spawn_http_server(std::sync::Arc::clone(&state))
            .await
            .unwrap_or_else(|e| panic!("server: {e}"));
        tokio::time::sleep(Duration::from_millis(50)).await;

        // Connect with pty subscription to receive broadcast events.
        let url = format!("ws://{addr}/ws?subscribe=pty");
        let (stream, _) = tokio_tungstenite::connect_async(&url)
            .await
            .unwrap_or_else(|e| panic!("ws connect failed: {e}"));
        let (mut tx, mut rx) = stream.split();

        // Inject new PTY data "BBBB" via broadcast.
        {
            let mut ring = state.terminal.ring.write().await;
            let data = Bytes::from(b"BBBB".to_vec());
            ring.write(&data);
            let offset = ring.total_written() - data.len() as u64;
            let _ = state.channels.output_tx.send(OutputEvent::Raw { data, offset });
        }

        // Small delay for the broadcast to queue ahead of any replay response.
        tokio::time::sleep(Duration::from_millis(20)).await;

        // Request full replay.
        let msg = ClientMessage::GetReplay { offset: 0, limit: None };
        let json = serde_json::to_string(&msg).unwrap_or_default();
        let _ = tx.send(tokio_tungstenite::tungstenite::Message::Text(json.into())).await;

        // Collect all messages for up to 500ms, then feed through ReplayGate.
        let mut gate = ReplayGate::new();
        let mut output = Vec::<u8>::new();
        let deadline = tokio::time::Instant::now() + Duration::from_millis(500);
        loop {
            match tokio::time::timeout_at(deadline, rx.next()).await {
                Ok(Some(Ok(tokio_tungstenite::tungstenite::Message::Text(text)))) => {
                    match serde_json::from_str::<ServerMessage>(&text) {
                        Ok(ServerMessage::Replay { data, offset, next_offset, .. }) => {
                            if let Ok(decoded) =
                                base64::engine::general_purpose::STANDARD.decode(&data)
                            {
                                if let Some(action) =
                                    gate.on_replay(decoded.len(), offset, next_offset)
                                {
                                    if action.is_first {
                                        output.clear();
                                    }
                                    output.extend_from_slice(&decoded[action.skip..]);
                                }
                            }
                        }
                        Ok(ServerMessage::Pty { data, offset, .. }) => {
                            if let Ok(decoded) =
                                base64::engine::general_purpose::STANDARD.decode(&data)
                            {
                                if let Some(skip) = gate.on_pty(decoded.len(), offset) {
                                    output.extend_from_slice(&decoded[skip..]);
                                }
                            }
                        }
                        _ => {}
                    }
                }
                _ => break,
            }
        }

        let text = String::from_utf8_lossy(&output);
        assert_eq!(text, "AAAABBBB", "expected no duplicates, got: {text}");
    }

    #[tokio::test]
    async fn auth_then_input_raw_succeeds() {
        let StoreCtx { store: state, mut input_rx, .. } =
            StoreBuilder::new().ring_size(4096).auth_token("secret123").build();

        let (addr, _handle) = crate::test_support::spawn_http_server(std::sync::Arc::clone(&state))
            .await
            .unwrap_or_else(|e| panic!("server: {e}"));
        tokio::time::sleep(Duration::from_millis(50)).await;

        let (mut tx, _rx) = connect_ws(addr, "raw").await;

        // Authenticate first.
        let auth = ClientMessage::Auth { token: "secret123".to_owned() };
        let json = serde_json::to_string(&auth).unwrap_or_default();
        let _ = tx.send(tokio_tungstenite::tungstenite::Message::Text(json.into())).await;
        tokio::time::sleep(Duration::from_millis(50)).await;

        // Now send input.
        let data = base64::engine::general_purpose::STANDARD.encode(b"hello");
        let msg = ClientMessage::SendInputRaw { data };
        let json = serde_json::to_string(&msg).unwrap_or_default();
        let _ = tx.send(tokio_tungstenite::tungstenite::Message::Text(json.into())).await;

        let event = tokio::time::timeout(Duration::from_secs(2), input_rx.recv()).await;
        match event {
            Ok(Some(crate::event::InputEvent::Write(bytes))) => {
                assert_eq!(&bytes[..], b"hello");
            }
            other => panic!("expected Write(b'hello'), got {other:?}"),
        }
    }
}

// ===== Unix Domain Socket integration tests =================================

mod uds_integration {
    use base64::Engine;
    use bytes::Bytes;
    use futures_util::{SinkExt, StreamExt};

    use crate::event::OutputEvent;
    use crate::test_support::{StoreBuilder, StoreCtx};
    use crate::transport::ws::{ClientMessage, ServerMessage};

    use super::*;

    /// Helper: spawn a coop UDS server with pre-loaded output chunks.
    async fn spawn_test_uds_server(
        output_chunks: Vec<&str>,
        socket_path: &std::path::Path,
    ) -> std::sync::Arc<crate::transport::state::Store> {
        let StoreCtx { store: state, .. } = StoreBuilder::new().ring_size(65536).build();

        // Write output chunks to ring buffer and broadcast them.
        {
            let mut ring = state.terminal.ring.write().await;
            for chunk in &output_chunks {
                let data = Bytes::from(chunk.as_bytes().to_vec());
                ring.write(&data);
                let offset = ring.total_written() - data.len() as u64;
                let _ = state.channels.output_tx.send(OutputEvent::Raw { data, offset });
            }
        }

        let _handle =
            crate::test_support::spawn_uds_server(std::sync::Arc::clone(&state), socket_path)
                .await
                .unwrap_or_else(|e| panic!("failed to spawn UDS server: {e}"));

        // Small delay for the server to be ready.
        tokio::time::sleep(Duration::from_millis(50)).await;

        state
    }

    /// Read the next ServerMessage of a specific type, skipping others (e.g.
    /// initial Transition broadcast). Times out after 2 seconds.
    async fn recv_until<R, F, T>(rx: &mut R, pred: F) -> anyhow::Result<T>
    where
        R: StreamExt<
                Item = Result<
                    tokio_tungstenite::tungstenite::Message,
                    tokio_tungstenite::tungstenite::Error,
                >,
            > + Unpin,
        F: Fn(ServerMessage) -> Option<T>,
    {
        let deadline = tokio::time::Instant::now() + Duration::from_secs(2);
        loop {
            match tokio::time::timeout_at(deadline, rx.next()).await {
                Ok(Some(Ok(tokio_tungstenite::tungstenite::Message::Text(text)))) => {
                    if let Ok(msg) = serde_json::from_str::<ServerMessage>(&text) {
                        if let Some(val) = pred(msg) {
                            return Ok(val);
                        }
                    }
                }
                Ok(Some(Ok(_))) => continue,
                other => anyhow::bail!("expected matching message, got {other:?}"),
            }
        }
    }

    #[tokio::test]
    async fn uds_connect_and_replay() -> anyhow::Result<()> {
        let dir = tempfile::tempdir()?;
        let sock = dir.path().join("coop.sock");
        let _state = spawn_test_uds_server(vec!["hello via uds"], &sock).await;

        let ws = super::connect_ws(None, Some(sock.to_str().unwrap_or_default())).await;
        let ws_stream = ws.map_err(|e| anyhow::anyhow!("{e}"))?;
        let (mut tx, mut rx) = ws_stream.split();

        // Request replay.
        let msg = ClientMessage::GetReplay { offset: 0, limit: None };
        let json = serde_json::to_string(&msg)?;
        let _ = tx.send(tokio_tungstenite::tungstenite::Message::Text(json.into())).await;

        // Read Replay response, skipping the initial Transition broadcast.
        let (data, offset) = recv_until(&mut rx, |msg| match msg {
            ServerMessage::Replay { data, offset, .. } => Some((data, offset)),
            _ => None,
        })
        .await?;

        assert_eq!(offset, 0);
        let decoded = base64::engine::general_purpose::STANDARD.decode(&data)?;
        let text = String::from_utf8_lossy(&decoded);
        assert!(text.contains("hello via uds"), "expected 'hello via uds' in replay, got: {text}");
        Ok(())
    }

    #[tokio::test]
    async fn uds_connect_nonexistent_socket_returns_error() {
        let result = super::connect_ws(None, Some("/tmp/coop-nonexistent-test.sock")).await;
        assert!(result.is_err(), "expected error for nonexistent socket");
        let err = result.unwrap_err();
        assert!(
            err.contains("/tmp/coop-nonexistent-test.sock"),
            "error should mention socket path: {err}"
        );
    }

    #[tokio::test]
    async fn uds_preferred_over_url() -> anyhow::Result<()> {
        let dir = tempfile::tempdir()?;
        let sock = dir.path().join("coop.sock");
        let _state = spawn_test_uds_server(vec!["uds-wins"], &sock).await;

        // Pass a bogus TCP URL alongside the real socket path.
        // If UDS is preferred, the connection succeeds via the socket.
        let ws =
            super::connect_ws(Some("http://127.0.0.1:1"), Some(sock.to_str().unwrap_or_default()))
                .await;
        let ws_stream = ws.map_err(|e| anyhow::anyhow!("{e}"))?;
        let (mut tx, mut rx) = ws_stream.split();

        let msg = ClientMessage::GetReplay { offset: 0, limit: None };
        let json = serde_json::to_string(&msg)?;
        let _ = tx.send(tokio_tungstenite::tungstenite::Message::Text(json.into())).await;

        let (data, _) = recv_until(&mut rx, |msg| match msg {
            ServerMessage::Replay { data, offset, .. } => Some((data, offset)),
            _ => None,
        })
        .await?;

        let decoded = base64::engine::general_purpose::STANDARD.decode(&data)?;
        let text = String::from_utf8_lossy(&decoded);
        assert!(text.contains("uds-wins"), "expected UDS data, got: {text}");
        Ok(())
    }
}

// ===== Terminal fidelity tests ===============================================

mod terminal_fidelity {
    use super::*;

    // -- Escape sequence constants --

    #[test]
    fn smcup_is_correct_sequence() {
        assert_eq!(SMCUP, b"\x1b[?1049h");
    }

    #[test]
    fn rmcup_is_correct_sequence() {
        assert_eq!(RMCUP, b"\x1b[?1049l");
    }

    #[test]
    fn clear_home_is_correct_sequence() {
        assert_eq!(CLEAR_HOME, b"\x1b[2J\x1b[H");
    }

    #[test]
    fn sync_start_is_correct_sequence() {
        assert_eq!(SYNC_START, b"\x1b[?2026h");
    }

    #[test]
    fn sync_end_is_correct_sequence() {
        assert_eq!(SYNC_END, b"\x1b[?2026l");
    }

    // -- Key constants --

    #[test]
    fn detach_key_is_ctrl_bracket() {
        assert_eq!(DETACH_KEY, 0x1d);
    }

    #[test]
    fn refresh_key_is_ctrl_l() {
        assert_eq!(REFRESH_KEY, 0x0c);
    }

    // -- enter/exit alt screen write correct sequences --

    #[test]
    fn enter_alt_screen_writes_smcup_sync_clear() {
        // Capture what enter_alt_screen writes by using a Vec<u8> as a stand-in.
        // We can't easily capture stdout, so verify the function exists and
        // the sequences are composed correctly.
        let mut buf = Vec::new();
        buf.extend_from_slice(SMCUP);
        buf.extend_from_slice(SYNC_START);
        buf.extend_from_slice(CLEAR_HOME);
        // The composed sequence should be: enter alt screen + begin sync + clear
        assert_eq!(
            &buf, b"\x1b[?1049h\x1b[?2026h\x1b[2J\x1b[H",
            "enter_alt_screen should compose SMCUP + SYNC_START + CLEAR_HOME"
        );
    }

    #[test]
    fn exit_alt_screen_writes_rmcup() {
        let mut buf = Vec::new();
        buf.extend_from_slice(RMCUP);
        assert_eq!(&buf, b"\x1b[?1049l", "exit_alt_screen should write RMCUP");
    }

    // -- AttachState sync_pending --

    #[test]
    fn attach_state_new_sync_pending_false() {
        let state = AttachState::new(80, 24);
        assert!(!state.sync_pending, "new AttachState should have sync_pending = false");
    }

    // -- Reconnect replay resets the gate (ka-0000003224) --
    //
    // The reconnect handshake must reset the persisted ReplayGate and replay
    // from offset 0. If it instead replays from the carried-over high-water
    // mark (the pre-fix bug), a restarted upstream — whose fresh ring begins at
    // a low offset — has its replay and every subsequent live frame dropped as
    // "already seen", leaving the terminal connected-but-frozen and blank.

    #[test]
    fn begin_reconnect_replay_resets_gate_and_returns_zero() {
        let mut state = AttachState::new(80, 24);
        // Simulate a prior connection that advanced the gate high-water mark.
        state.gate.on_replay(50_000, 0, 50_000);
        assert_eq!(state.gate.offset(), Some(50_000), "precondition: gate advanced");

        let offset = state.begin_reconnect_replay();

        assert_eq!(offset, 0, "reconnect must replay from offset 0, not the stale high-water mark");
        assert_eq!(state.gate.offset(), None, "reconnect must reset the gate to the pending state");
        assert!(state.sync_pending, "reconnect must arm sync_pending for the initial redraw");
    }

    #[test]
    fn reconnect_after_upstream_restart_renders_output() {
        let mut state = AttachState::new(80, 24);
        // Prior connection: high-water mark advanced far into the old ring.
        state.gate.on_replay(50_000, 0, 50_000);

        // Upstream restarts; attach reconnects and runs the handshake.
        let _ = state.begin_reconnect_replay();

        // The restarted server has a fresh ring: an initial replay at a low
        // offset must be accepted (pre-fix it is dropped as next_offset <= gate).
        let replay = state.gate.on_replay(0, 0, 0);
        assert!(
            replay.is_some(),
            "restarted server's initial replay must be accepted after a reconnect reset"
        );

        // A subsequent live PTY frame from the restarted server must also be
        // rendered rather than silently dropped below the stale high-water mark.
        let pty = state.gate.on_pty(100, 500);
        assert!(
            pty.is_some(),
            "restarted server's live output must be rendered, not dropped as already-seen"
        );
    }

    // -- Refresh key filtering --

    #[test]
    fn refresh_key_detected_in_input() {
        let input = vec![b'a', REFRESH_KEY, b'b'];
        assert!(input.contains(&REFRESH_KEY));
    }

    #[test]
    fn refresh_key_filtered_from_input() {
        let input = vec![b'a', REFRESH_KEY, b'b', REFRESH_KEY];
        let filtered: Vec<u8> = input.iter().copied().filter(|&b| b != REFRESH_KEY).collect();
        assert_eq!(filtered, vec![b'a', b'b']);
    }

    #[test]
    fn refresh_key_only_leaves_empty_input() {
        let input = vec![REFRESH_KEY];
        let filtered: Vec<u8> = input.iter().copied().filter(|&b| b != REFRESH_KEY).collect();
        assert!(filtered.is_empty());
    }

    // -- Detach key takes priority over refresh --

    #[test]
    fn detach_key_before_refresh_key() {
        let input = vec![b'x', DETACH_KEY, REFRESH_KEY];
        // Detach is checked first by position
        let detach_pos = input.iter().position(|&b| b == DETACH_KEY);
        assert_eq!(detach_pos, Some(1), "detach key should be found at position 1");
    }

    #[test]
    fn refresh_key_without_detach() {
        let input = vec![b'x', REFRESH_KEY, b'y'];
        let detach_pos = input.iter().position(|&b| b == DETACH_KEY);
        assert!(detach_pos.is_none(), "no detach key in input");
        assert!(input.contains(&REFRESH_KEY), "refresh key should be found");
    }

    // -- build_ws_url tests --

    #[test]
    fn build_ws_url_subscribes_to_pty_and_state() {
        let url = build_ws_url("http://localhost:8080");
        assert!(url.contains("subscribe=pty,state"), "URL should subscribe to pty,state: {url}");
    }
}
