// SPDX-License-Identifier: BUSL-1.1
// Copyright (c) 2026 Matt Baker <fimbaz@gmail.com>

//! Shared test infrastructure: builders, mocks, and assertion helpers.

use std::future::Future;
use std::path::PathBuf;
use std::pin::Pin;
use std::sync::atomic::{AtomicBool, AtomicI32, AtomicU32, AtomicU64};
use std::sync::Arc;
use std::time::{Duration, Instant};

use bytes::Bytes;
use tokio::sync::{broadcast, mpsc, RwLock};
use tokio_util::sync::CancellationToken;

use crate::backend::Backend;
use crate::config::GroomLevel;
use crate::driver::{
    AgentState, AgentType, Detector, DetectorEmission, ExitStatus, NudgeEncoder, NudgeStep,
    RespondEncoder,
};
use crate::event::{
    InputEvent, OutputEvent, PromptOutcome, RawHookEvent, RawMessageEvent, TransitionEvent,
};
use crate::event_log::EventLog;
use crate::profile::ProfileState;
use crate::ring::RingBuffer;
use crate::screen::Screen;
use crate::start::{StartConfig, StartState};
use crate::stop::{StopConfig, StopState};
use crate::switch::{SwitchRequest, SwitchState};
use crate::transcript::TranscriptState;
use crate::transport::state::{
    DetectionInfo, DriverState, LifecycleState, SessionSettings, Store, TerminalState,
    TransportChannels,
};
use crate::usage::UsageState;

/// Test-only handle returned by [`StoreBuilder::build`], bundling the shared
/// store with all receiver ends that would normally be consumed by the session
/// loop.
pub struct StoreCtx {
    pub store: Arc<Store>,
    pub input_rx: mpsc::Receiver<InputEvent>,
    pub switch_rx: mpsc::Receiver<SwitchRequest>,
}

/// Builder for constructing `AppState` in tests with sensible defaults.
pub struct StoreBuilder {
    ring_size: usize,
    child_pid: u32,
    auth_token: Option<String>,
    hook_token: Option<String>,
    agent_state: AgentState,
    nudge_encoder: Option<Arc<dyn NudgeEncoder>>,
    respond_encoder: Option<Arc<dyn RespondEncoder>>,
    stop_config: Option<StopConfig>,
    start_config: Option<StartConfig>,
    transcript_state: Option<Arc<TranscriptState>>,
    groom: GroomLevel,
    session_dir: Option<PathBuf>,
    record_state: Option<Arc<crate::record::RecordingState>>,
    event_log_state: Option<Arc<EventLog>>,
    resume_report: Option<crate::transport::state::ResumeReport>,
    agent: AgentType,
}

impl Default for StoreBuilder {
    fn default() -> Self {
        Self::new()
    }
}

impl StoreBuilder {
    pub fn new() -> Self {
        Self {
            ring_size: 4096,
            child_pid: 0,
            auth_token: None,
            hook_token: None,
            agent_state: AgentState::Starting,
            nudge_encoder: None,
            respond_encoder: None,
            stop_config: None,
            start_config: None,
            transcript_state: None,
            groom: GroomLevel::Manual,
            session_dir: None,
            record_state: None,
            event_log_state: None,
            resume_report: None,
            agent: AgentType::Unknown,
        }
    }

    pub fn agent(mut self, agent: AgentType) -> Self {
        self.agent = agent;
        self
    }

    pub fn ring_size(mut self, n: usize) -> Self {
        self.ring_size = n;
        self
    }

    pub fn child_pid(mut self, pid: u32) -> Self {
        self.child_pid = pid;
        self
    }

    pub fn auth_token(mut self, t: impl Into<String>) -> Self {
        self.auth_token = Some(t.into());
        self
    }

    pub fn hook_token(mut self, t: impl Into<String>) -> Self {
        self.hook_token = Some(t.into());
        self
    }

    pub fn agent_state(mut self, s: AgentState) -> Self {
        self.agent_state = s;
        self
    }

    pub fn nudge_encoder(mut self, e: Arc<dyn NudgeEncoder>) -> Self {
        self.nudge_encoder = Some(e);
        self
    }

    pub fn respond_encoder(mut self, e: Arc<dyn RespondEncoder>) -> Self {
        self.respond_encoder = Some(e);
        self
    }

    pub fn stop_config(mut self, c: StopConfig) -> Self {
        self.stop_config = Some(c);
        self
    }

    pub fn start_config(mut self, c: StartConfig) -> Self {
        self.start_config = Some(c);
        self
    }

    pub fn transcript(mut self, t: Arc<TranscriptState>) -> Self {
        self.transcript_state = Some(t);
        self
    }

    pub fn groom(mut self, level: GroomLevel) -> Self {
        self.groom = level;
        self
    }

    pub fn session_dir(mut self, path: PathBuf) -> Self {
        self.session_dir = Some(path);
        self
    }

    pub fn record(mut self, r: Arc<crate::record::RecordingState>) -> Self {
        self.record_state = Some(r);
        self
    }

    pub fn event_log(mut self, e: Arc<EventLog>) -> Self {
        self.event_log_state = Some(e);
        self
    }

    pub fn resume_report(mut self, r: crate::transport::state::ResumeReport) -> Self {
        self.resume_report = Some(r);
        self
    }

    /// Build state and return a `StoreCtx` with all receiver handles.
    pub fn build(self) -> StoreCtx {
        let (input_tx, input_rx) = mpsc::channel(64);
        let (switch_tx, switch_rx) = mpsc::channel::<SwitchRequest>(1);
        let (output_tx, _) = broadcast::channel::<OutputEvent>(256);
        let (screen_tx, _) = broadcast::channel::<Arc<crate::screen::ScreenTextSnapshot>>(16);
        let (state_tx, _) = broadcast::channel::<TransitionEvent>(64);
        let (prompt_tx, _) = broadcast::channel::<PromptOutcome>(64);
        let (hook_tx, _) = broadcast::channel::<RawHookEvent>(64);
        let (message_tx, _) =
            broadcast::channel::<RawMessageEvent>(crate::event::MESSAGE_TX_CAPACITY);

        let store = Arc::new(Store {
            terminal: Arc::new(TerminalState {
                screen: RwLock::new(Screen::new(80, 24)),
                ring: RwLock::new(RingBuffer::new(self.ring_size)),
                ring_total_written: Arc::new(AtomicU64::new(0)),
                child_pid: AtomicU32::new(self.child_pid),
                exit_status: RwLock::new(None),
            }),
            driver: Arc::new(DriverState {
                agent_state: RwLock::new(self.agent_state),
                state_seq: AtomicU64::new(0),
                detection: RwLock::new(DetectionInfo { tier: u8::MAX, cause: String::new() }),
                error: RwLock::new(None),
                last_message: Arc::new(RwLock::new(None)),
            }),
            channels: TransportChannels {
                input_tx,
                output_tx,
                screen_tx,
                state_tx,
                prompt_tx,
                hook_tx,
                message_tx,
            },
            config: SessionSettings {
                started_at: Instant::now(),
                agent: self.agent,
                auth_token: self.auth_token,
                hook_token: self.hook_token,
                nudge_encoder: self.nudge_encoder,
                respond_encoder: self.respond_encoder,
                nudge_timeout: Duration::ZERO,
                nudge_enter_max_retries: 5,
                groom: self.groom,
            },
            lifecycle: LifecycleState {
                shutdown: CancellationToken::new(),
                ws_client_count: AtomicI32::new(0),
                bytes_written: AtomicU64::new(0),
                zombie: AtomicBool::new(false),
                last_transition_ms: AtomicU64::new(0),
                idle_suspended: AtomicBool::new(false),
            },
            session_id: RwLock::new(uuid::Uuid::new_v4().to_string()),
            ready: Arc::new(AtomicBool::new(false)),
            input_gate: Arc::new(crate::transport::state::InputGate::new(Duration::ZERO)),
            stop: Arc::new(StopState::new(
                self.stop_config.unwrap_or_default(),
                "http://127.0.0.1:0/api/v1/stop/resolve".to_owned(),
            )),
            start: Arc::new(StartState::new(self.start_config.unwrap_or_default())),
            switch: Arc::new(SwitchState {
                switch_tx,
                session_log_path: RwLock::new(None),
                base_settings: None,
                mcp_config: None,
            }),
            usage: Arc::new(UsageState::new()),
            profile: Arc::new(ProfileState::new()),
            transcript: self.transcript_state.unwrap_or_else(|| {
                Arc::new({
                    let dir = std::env::temp_dir().join("coop-test-transcripts");
                    // OK to panic in test-only code — infra setup failure is fatal.
                    #[allow(clippy::expect_used)]
                    TranscriptState::new(dir, None).expect("create transcript state")
                })
            }),
            input_activity: Arc::new(tokio::sync::Notify::new()),
            event_log: self.event_log_state.unwrap_or_else(|| Arc::new(EventLog::new(None))),
            record: self
                .record_state
                .unwrap_or_else(|| Arc::new(crate::record::RecordingState::new(None, 80, 24))),
            session_dir: self.session_dir,
            resume_report: self.resume_report,
        });

        StoreCtx { store, input_rx, switch_rx }
    }
}

/// A fake PTY backend for deterministic, sub-millisecond session tests.
pub struct MockPty {
    output: Vec<Bytes>,
    chunk_delay: Duration,
    exit_status: ExitStatus,
    drain_input: bool,
    captured_input: Arc<parking_lot::Mutex<Vec<Bytes>>>,
}

impl Default for MockPty {
    fn default() -> Self {
        Self::new()
    }
}

impl MockPty {
    pub fn new() -> Self {
        Self {
            output: Vec::new(),
            chunk_delay: Duration::ZERO,
            exit_status: ExitStatus { code: Some(0), signal: None },
            drain_input: false,
            captured_input: Arc::new(parking_lot::Mutex::new(Vec::new())),
        }
    }

    pub fn with_output(chunks: Vec<Bytes>) -> Self {
        Self { output: chunks, ..Self::new() }
    }

    pub fn exit_status(mut self, s: ExitStatus) -> Self {
        self.exit_status = s;
        self
    }

    pub fn chunk_delay(mut self, d: Duration) -> Self {
        self.chunk_delay = d;
        self
    }

    pub fn drain_input(mut self) -> Self {
        self.drain_input = true;
        self
    }

    pub fn captured_input(&self) -> Arc<parking_lot::Mutex<Vec<Bytes>>> {
        Arc::clone(&self.captured_input)
    }
}

impl Backend for MockPty {
    fn run(
        &mut self,
        output_tx: mpsc::Sender<Bytes>,
        mut input_rx: mpsc::Receiver<crate::backend::BackendInput>,
        _resize_rx: mpsc::Receiver<(u16, u16)>,
    ) -> Pin<Box<dyn Future<Output = anyhow::Result<ExitStatus>> + Send + '_>> {
        let output = std::mem::take(&mut self.output);
        let chunk_delay = self.chunk_delay;
        let exit_status = self.exit_status;
        let drain_input = self.drain_input;
        let captured_input = Arc::clone(&self.captured_input);

        Box::pin(async move {
            for chunk in output {
                if output_tx.send(chunk).await.is_err() {
                    break;
                }
                if chunk_delay > Duration::ZERO {
                    tokio::time::sleep(chunk_delay).await;
                }
            }
            if drain_input {
                while let Some(msg) = input_rx.recv().await {
                    match msg {
                        crate::backend::BackendInput::Write(data) => {
                            captured_input.lock().push(data);
                        }
                        crate::backend::BackendInput::Drain(tx) => {
                            let _ = tx.send(());
                        }
                    }
                }
            }
            Ok(exit_status)
        })
    }

    fn resize(&self, _cols: u16, _rows: u16) -> anyhow::Result<()> {
        Ok(())
    }

    fn child_pid(&self) -> Option<u32> {
        None
    }
}

/// Extension trait to convert any `Display` error into `anyhow::Error`.
/// Replaces `.map_err(|e| anyhow::anyhow!("{e}"))` with `.anyhow()`.
pub trait AnyhowExt<T> {
    fn anyhow(self) -> anyhow::Result<T>;
}

impl<T, E: std::fmt::Display> AnyhowExt<T> for Result<T, E> {
    fn anyhow(self) -> anyhow::Result<T> {
        self.map_err(|e| anyhow::anyhow!("{e}"))
    }
}

/// Stub nudge encoder that passes through message bytes unchanged.
pub struct StubNudgeEncoder;
impl NudgeEncoder for StubNudgeEncoder {
    fn encode(&self, message: &str) -> Vec<NudgeStep> {
        vec![NudgeStep { bytes: message.as_bytes().to_vec(), delay_after: None }]
    }
}

/// Stub respond encoder that returns fixed byte sequences.
pub struct StubRespondEncoder;
impl RespondEncoder for StubRespondEncoder {
    fn encode_permission(&self, option: u32) -> Vec<NudgeStep> {
        vec![NudgeStep { bytes: format!("{option}\r").into_bytes(), delay_after: None }]
    }
    fn encode_plan(&self, option: u32, _feedback: Option<&str>) -> Vec<NudgeStep> {
        vec![NudgeStep { bytes: format!("{option}\r").into_bytes(), delay_after: None }]
    }
    fn encode_question(
        &self,
        _answers: &[crate::driver::QuestionAnswer],
        _total_questions: usize,
    ) -> Vec<NudgeStep> {
        vec![NudgeStep { bytes: b"q\r".to_vec(), delay_after: None }]
    }
    fn encode_setup(&self, option: u32) -> Vec<NudgeStep> {
        vec![NudgeStep { bytes: format!("{option}\r").into_bytes(), delay_after: None }]
    }
}

/// Assert that an expression evaluates to `Err` whose Display output
/// contains the given substring.
#[macro_export]
macro_rules! assert_err_contains {
    ($expr:expr, $substr:expr) => {{
        let result = $expr;
        let err = result.expect_err(concat!("expected Err for: ", stringify!($expr)));
        let msg = err.to_string();
        assert!(msg.contains($substr), "expected error containing {:?}, got: {msg:?}", $substr);
    }};
}

/// A configurable detector for testing [`CompositeDetector`] tier resolution.
///
/// Emits a sequence of `(delay, state)` pairs, then waits for shutdown.
/// Each emission can optionally include a tier override.
pub struct MockDetector {
    tier_val: u8,
    states: Vec<(Duration, AgentState, Option<u8>)>,
}

impl MockDetector {
    pub fn new(tier: u8, states: Vec<(Duration, AgentState)>) -> Self {
        Self { tier_val: tier, states: states.into_iter().map(|(d, s)| (d, s, None)).collect() }
    }

    pub fn with_overrides(tier: u8, states: Vec<(Duration, AgentState, Option<u8>)>) -> Self {
        Self { tier_val: tier, states }
    }
}

impl Detector for MockDetector {
    fn run(
        self: Box<Self>,
        state_tx: mpsc::Sender<DetectorEmission>,
        shutdown: CancellationToken,
    ) -> Pin<Box<dyn Future<Output = ()> + Send>> {
        Box::pin(async move {
            for (delay, state, tier_override) in self.states {
                tokio::select! {
                    _ = shutdown.cancelled() => return,
                    _ = tokio::time::sleep(delay) => {
                        if state_tx.send((state, String::new(), tier_override)).await.is_err() {
                            return;
                        }
                    }
                }
            }
            shutdown.cancelled().await;
        })
    }

    fn tier(&self) -> u8 {
        self.tier_val
    }
}

/// Spawn an HTTP server on a random port for integration testing.
///
/// Returns the bound address and a join handle for the server task.
pub async fn spawn_http_server(
    store: Arc<Store>,
) -> anyhow::Result<(std::net::SocketAddr, tokio::task::JoinHandle<()>)> {
    let router = crate::transport::build_router(store);
    let listener = tokio::net::TcpListener::bind("127.0.0.1:0").await?;
    let addr = listener.local_addr()?;
    let handle = tokio::spawn(async move {
        let _ = axum::serve(listener, router).await;
    });
    Ok((addr, handle))
}

/// Spawn an HTTP server on a Unix domain socket for integration testing.
///
/// Returns a join handle for the server task. Uses the same `hyper_util` UDS
/// serving approach as `run.rs`.
pub async fn spawn_uds_server(
    store: Arc<Store>,
    socket_path: &std::path::Path,
) -> anyhow::Result<tokio::task::JoinHandle<()>> {
    // Remove stale socket if present.
    let _ = std::fs::remove_file(socket_path);
    let uds_listener = tokio::net::UnixListener::bind(socket_path)?;
    let router = crate::transport::build_router(store);
    let handle = tokio::spawn(async move {
        let mut make_svc = router.into_make_service();
        while let Ok((stream, _)) = uds_listener.accept().await {
            let svc_future = <_ as tower::Service<_>>::call(&mut make_svc, ());
            tokio::spawn(async move {
                let Ok(svc) = svc_future.await;
                let io = hyper_util::rt::TokioIo::new(stream);
                let hyper_svc = hyper_util::service::TowerToHyperService::new(svc);
                let _ = hyper_util::server::conn::auto::Builder::new(
                    hyper_util::rt::TokioExecutor::new(),
                )
                .serve_connection_with_upgrades(io, hyper_svc)
                .await;
            });
        }
    });
    Ok(handle)
}

/// Spawn a gRPC server on a random port for integration testing.
///
/// Returns the bound address and a join handle for the server task.
pub async fn spawn_grpc_server(
    store: Arc<Store>,
) -> anyhow::Result<(std::net::SocketAddr, tokio::task::JoinHandle<()>)> {
    let listener = tokio::net::TcpListener::bind("127.0.0.1:0").await?;
    let addr = listener.local_addr()?;
    let grpc = crate::transport::grpc::CoopGrpc::new(store);
    let incoming = tokio_stream::wrappers::TcpListenerStream::new(listener);
    let handle = tokio::spawn(async move {
        let _ = grpc.into_router().serve_with_incoming(incoming).await;
    });
    Ok((addr, handle))
}
