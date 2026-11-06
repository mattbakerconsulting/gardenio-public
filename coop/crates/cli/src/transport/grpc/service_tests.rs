// SPDX-License-Identifier: BUSL-1.1
// Copyright (c) 2026 Matt Baker <fimbaz@gmail.com>

use super::*;
use crate::event::PtySignal;
use crate::test_support::{StoreBuilder, StoreCtx};
use crate::transport::encode_key;

#[yare::parameterized(
    enter = { "Enter", b"\r" as &[u8] },
    return_key = { "Return", b"\r" },
    tab = { "Tab", b"\t" },
    escape = { "Escape", b"\x1b" },
    esc = { "Esc", b"\x1b" },
    backspace = { "Backspace", b"\x7f" },
    delete = { "Delete", b"\x1b[3~" },
    up = { "Up", b"\x1b[A" },
    down = { "Down", b"\x1b[B" },
    right = { "Right", b"\x1b[C" },
    left = { "Left", b"\x1b[D" },
    home = { "Home", b"\x1b[H" },
    end = { "End", b"\x1b[F" },
    pageup = { "PageUp", b"\x1b[5~" },
    pagedown = { "PageDown", b"\x1b[6~" },
    space = { "Space", b" " },
    ctrl_c = { "Ctrl-C", b"\x03" },
    ctrl_d = { "Ctrl-D", b"\x04" },
    ctrl_z = { "Ctrl-Z", b"\x1a" },
    f1 = { "F1", b"\x1bOP" },
    f12 = { "F12", b"\x1b[24~" },
)]
fn encode_key_known(name: &str, expected: &[u8]) {
    let result = encode_key(name);
    assert!(result.is_some(), "encode_key({name:?}) returned None");
    assert_eq!(result.as_deref(), Some(expected));
}

#[test]
fn encode_key_unknown_returns_none() {
    assert!(encode_key("SuperKey").is_none());
    assert!(encode_key("").is_none());
    assert!(encode_key("Ctrl-?").is_none());
}

#[test]
fn encode_key_case_insensitive() {
    assert_eq!(encode_key("enter"), encode_key("ENTER"));
    assert_eq!(encode_key("ctrl-c"), encode_key("Ctrl-C"));
}

#[yare::parameterized(
    sigint = { "SIGINT", PtySignal::Int },
    int = { "INT", PtySignal::Int },
    bare_2 = { "2", PtySignal::Int },
    sigterm = { "SIGTERM", PtySignal::Term },
    term = { "TERM", PtySignal::Term },
    bare_15 = { "15", PtySignal::Term },
    sighup = { "SIGHUP", PtySignal::Hup },
    sigkill = { "SIGKILL", PtySignal::Kill },
    sigusr1 = { "SIGUSR1", PtySignal::Usr1 },
    sigusr2 = { "SIGUSR2", PtySignal::Usr2 },
    sigcont = { "SIGCONT", PtySignal::Cont },
    sigstop = { "SIGSTOP", PtySignal::Stop },
    sigtstp = { "SIGTSTP", PtySignal::Tstp },
    sigwinch = { "SIGWINCH", PtySignal::Winch },
)]
fn pty_signal_from_name_known(name: &str, expected: PtySignal) {
    let result = PtySignal::from_name(name);
    assert_eq!(result, Some(expected), "PtySignal::from_name({name:?})");
}

#[test]
fn pty_signal_from_name_unknown_returns_none() {
    assert!(PtySignal::from_name("SIGFOO").is_none());
    assert!(PtySignal::from_name("").is_none());
    assert!(PtySignal::from_name("99").is_none());
}

#[test]
fn pty_signal_from_name_case_insensitive() {
    assert_eq!(PtySignal::from_name("sigint"), Some(PtySignal::Int));
    assert_eq!(PtySignal::from_name("int"), Some(PtySignal::Int));
}

#[test]
fn service_instantiation_compiles() {
    let StoreCtx { store: state, .. } = StoreBuilder::new().build();
    let service = CoopGrpc::new(state);
    // Verify we can construct a tonic server from the service
    let _router = service.into_router();
}

#[test]
fn service_with_auth_compiles() {
    let StoreCtx { store: state, .. } =
        StoreBuilder::new().child_pid(1234).auth_token("secret").build();
    let service = CoopGrpc::new(state);
    // Verify we can build the router with auth interceptor
    let _router = service.into_router();
}

#[tokio::test]
async fn send_input_raw_writes_bytes() -> anyhow::Result<()> {
    use crate::event::InputEvent;
    let StoreCtx { store: state, mut input_rx, .. } = StoreBuilder::new().child_pid(1234).build();
    let svc = CoopGrpc::new(state);

    let req = tonic::Request::new(proto::SendInputRawRequest { data: b"hello".to_vec() });
    let resp = proto::coop_server::Coop::send_input_raw(&svc, req).await?;
    assert_eq!(resp.into_inner().bytes_written, 5);

    let event = input_rx.recv().await;
    assert!(matches!(event, Some(InputEvent::Write(_))));
    Ok(())
}

#[tokio::test]
async fn get_ready_returns_readiness() -> anyhow::Result<()> {
    let StoreCtx { store: state, .. } = StoreBuilder::new().child_pid(1234).build();
    let svc = CoopGrpc::new(state.clone());

    let req = tonic::Request::new(proto::GetReadyRequest {});
    let resp = proto::coop_server::Coop::get_ready(&svc, req).await?;
    assert!(!resp.into_inner().ready, "default ready is false");

    state.ready.store(true, std::sync::atomic::Ordering::Release);
    let req = tonic::Request::new(proto::GetReadyRequest {});
    let resp = proto::coop_server::Coop::get_ready(&svc, req).await?;
    assert!(resp.into_inner().ready);
    Ok(())
}
