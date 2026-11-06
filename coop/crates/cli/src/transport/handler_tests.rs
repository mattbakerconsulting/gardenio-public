// SPDX-License-Identifier: BUSL-1.1
// Copyright (c) 2026 Matt Baker <fimbaz@gmail.com>

use std::sync::Arc;

use crate::driver::{AgentState, ExitStatus};
use crate::event::InputEvent;
use crate::test_support::{StoreBuilder, StoreCtx, StubNudgeEncoder, StubRespondEncoder};
use crate::transport::handler::{
    compute_health, compute_status, handle_input, handle_input_raw, handle_keys, handle_nudge,
    handle_resize, handle_respond, handle_signal, session_state_str, to_domain_answers,
    TransportQuestionAnswer,
};

#[test]
fn session_state_exited() {
    let state = AgentState::Exited { status: ExitStatus { code: Some(0), signal: None } };
    assert_eq!(session_state_str(&state, 1234), "exited");
}

#[test]
fn session_state_starting_when_pid_zero() {
    assert_eq!(session_state_str(&AgentState::Starting, 0), "starting");
    assert_eq!(session_state_str(&AgentState::Working, 0), "starting");
}

#[test]
fn session_state_uses_agent_state_when_pid_nonzero() {
    assert_eq!(session_state_str(&AgentState::Starting, 1), "starting");
    assert_eq!(session_state_str(&AgentState::Working, 42), "working");
    assert_eq!(session_state_str(&AgentState::Idle, 100), "idle");
}

#[test]
fn to_domain_answers_empty() {
    let result = to_domain_answers(&[]);
    assert!(result.is_empty());
}

#[test]
fn to_domain_answers_converts_fields() {
    let input = vec![
        TransportQuestionAnswer { option: Some(1), text: None },
        TransportQuestionAnswer { option: None, text: Some("custom".to_owned()) },
        TransportQuestionAnswer { option: Some(3), text: Some("both".to_owned()) },
    ];
    let result = to_domain_answers(&input);
    assert_eq!(result.len(), 3);
    assert_eq!(result[0].option, Some(1));
    assert!(result[0].text.is_none());
    assert!(result[1].option.is_none());
    assert_eq!(result[1].text.as_deref(), Some("custom"));
    assert_eq!(result[2].option, Some(3));
    assert_eq!(result[2].text.as_deref(), Some("both"));
}

#[tokio::test]
async fn compute_health_fields() -> anyhow::Result<()> {
    let StoreCtx { store: state, .. } = StoreBuilder::new().child_pid(1234).build();
    state.ready.store(true, std::sync::atomic::Ordering::Release);

    let h = compute_health(&state).await;
    assert_eq!(h.status, "running");
    assert_eq!(h.pid, Some(1234));
    assert!(h.uptime_secs >= 0);
    assert_eq!(h.terminal_cols, 80);
    assert_eq!(h.terminal_rows, 24);
    assert_eq!(h.ws_clients, 0);
    assert!(h.ready);
    Ok(())
}

#[tokio::test]
async fn compute_health_pid_zero_is_none() -> anyhow::Result<()> {
    let StoreCtx { store: state, .. } = StoreBuilder::new().build();
    let h = compute_health(&state).await;
    assert!(h.pid.is_none());
    assert!(!h.ready);
    Ok(())
}

#[tokio::test]
async fn compute_status_returns_agent_state() -> anyhow::Result<()> {
    let StoreCtx { store: state, .. } =
        StoreBuilder::new().child_pid(5678).agent_state(AgentState::Working).build();
    let st = compute_status(&state).await;
    assert_eq!(st.state, "working");
    assert_eq!(st.pid, Some(5678));
    assert!(st.uptime_secs >= 0);
    assert!(st.exit_code.is_none());
    Ok(())
}

#[tokio::test]
async fn compute_status_exited() -> anyhow::Result<()> {
    let StoreCtx { store: state, .. } = StoreBuilder::new()
        .child_pid(100)
        .agent_state(AgentState::Exited { status: ExitStatus { code: Some(1), signal: None } })
        .build();
    *state.terminal.exit_status.write().await = Some(ExitStatus { code: Some(1), signal: None });
    let st = compute_status(&state).await;
    assert_eq!(st.state, "exited");
    assert_eq!(st.exit_code, Some(1));
    Ok(())
}

#[test]
fn zombie_flag_defaults_to_false() {
    let StoreCtx { store: state, .. } = StoreBuilder::new().build();
    assert!(!state.lifecycle.zombie.load(std::sync::atomic::Ordering::Acquire));
}

#[test]
fn zombie_flag_makes_livez_report_zombie() {
    let StoreCtx { store: state, .. } = StoreBuilder::new().child_pid(100).build();
    state.lifecycle.zombie.store(true, std::sync::atomic::Ordering::Release);
    assert!(state.lifecycle.zombie.load(std::sync::atomic::Ordering::Acquire));
}

#[tokio::test]
async fn nudge_not_ready_returns_error() -> anyhow::Result<()> {
    let StoreCtx { store: state, .. } =
        StoreBuilder::new().nudge_encoder(Arc::new(StubNudgeEncoder)).build();
    // ready defaults to false
    let result = handle_nudge(&state, "hello").await;
    assert!(result.is_err());
    assert_eq!(result.unwrap_err(), crate::error::ErrorCode::NotReady);
    Ok(())
}

#[tokio::test]
async fn nudge_no_driver_returns_error() -> anyhow::Result<()> {
    let StoreCtx { store: state, .. } = StoreBuilder::new().build();
    state.ready.store(true, std::sync::atomic::Ordering::Release);
    let result = handle_nudge(&state, "hello").await;
    assert!(result.is_err());
    assert_eq!(result.unwrap_err(), crate::error::ErrorCode::NoDriver);
    Ok(())
}

#[tokio::test]
async fn nudge_working_delivers() -> anyhow::Result<()> {
    let StoreCtx { store: state, mut input_rx, .. } = StoreBuilder::new()
        .agent_state(AgentState::Working)
        .nudge_encoder(Arc::new(StubNudgeEncoder))
        .build();
    state.ready.store(true, std::sync::atomic::Ordering::Release);

    let result = handle_nudge(&state, "hello").await.map_err(|e| anyhow::anyhow!("{e}"))?;
    assert!(result.delivered);
    assert_eq!(result.state_before.as_deref(), Some("working"));
    assert!(result.reason.is_none());

    let event = input_rx.recv().await;
    assert!(matches!(event, Some(InputEvent::Write(_))));
    Ok(())
}

#[tokio::test]
async fn nudge_waiting_delivers() -> anyhow::Result<()> {
    let StoreCtx { store: state, mut input_rx, .. } = StoreBuilder::new()
        .agent_state(AgentState::Idle)
        .nudge_encoder(Arc::new(StubNudgeEncoder))
        .build();
    state.ready.store(true, std::sync::atomic::Ordering::Release);

    let result = handle_nudge(&state, "hello").await.map_err(|e| anyhow::anyhow!("{e}"))?;
    assert!(result.delivered);
    assert_eq!(result.state_before.as_deref(), Some("idle"));
    assert!(result.reason.is_none());

    let event = input_rx.recv().await;
    assert!(matches!(event, Some(InputEvent::Write(_))));
    Ok(())
}

#[tokio::test]
async fn nudge_plan_delegates_to_respond() -> anyhow::Result<()> {
    let plan_state = AgentState::Prompt {
        prompt: crate::driver::PromptContext::new(crate::driver::PromptKind::Plan)
            .with_options(vec![
                "Approve".to_owned(),
                "Auto-approve".to_owned(),
                "Deny".to_owned(),
                "Feedback".to_owned(),
            ])
            .with_ready(),
    };
    let StoreCtx { store: state, mut input_rx, .. } = StoreBuilder::new()
        .agent_state(plan_state)
        .nudge_encoder(Arc::new(StubNudgeEncoder))
        .respond_encoder(Arc::new(StubRespondEncoder))
        .build();
    state.ready.store(true, std::sync::atomic::Ordering::Release);

    let result = handle_nudge(&state, "looks good but rename Foo")
        .await
        .map_err(|e| anyhow::anyhow!("{e}"))?;
    assert!(result.delivered);
    assert_eq!(result.state_before.as_deref(), Some("prompt"));

    // StubRespondEncoder::encode_plan returns "{option}\r"; resolve_plan_option(None,None) → 4.
    let event = input_rx.recv().await;
    assert!(matches!(event, Some(InputEvent::Write(data)) if data == &b"4\r"[..]));
    Ok(())
}

#[tokio::test]
async fn nudge_question_delegates_to_respond() -> anyhow::Result<()> {
    let question_state = AgentState::Prompt {
        prompt: crate::driver::PromptContext::new(crate::driver::PromptKind::Question)
            .with_questions(vec![crate::driver::QuestionContext {
                question: "What framework?".to_owned(),
                options: vec!["React".to_owned(), "Vue".to_owned()],
            }])
            .with_ready(),
    };
    let StoreCtx { store: state, mut input_rx, .. } = StoreBuilder::new()
        .agent_state(question_state)
        .nudge_encoder(Arc::new(StubNudgeEncoder))
        .respond_encoder(Arc::new(StubRespondEncoder))
        .build();
    state.ready.store(true, std::sync::atomic::Ordering::Release);

    let result = handle_nudge(&state, "Svelte").await.map_err(|e| anyhow::anyhow!("{e}"))?;
    assert!(result.delivered);
    assert_eq!(result.state_before.as_deref(), Some("prompt"));

    // StubRespondEncoder::encode_question returns "q\r".
    let event = input_rx.recv().await;
    assert!(matches!(event, Some(InputEvent::Write(data)) if data == &b"q\r"[..]));
    Ok(())
}

#[tokio::test]
async fn nudge_returns_immediately_for_large_message() -> anyhow::Result<()> {
    // Use an encoder that simulates the per-byte typing delay used in
    // production (claude_nudge with `delay_after` between steps).  If
    // `handle_nudge` awaited delivery, a 12 KB message at 1 ms/byte would
    // take >12 s; detached delivery must return in well under 50 ms.
    use crate::driver::{NudgeEncoder, NudgeStep};
    struct SlowPerByteEncoder;
    impl NudgeEncoder for SlowPerByteEncoder {
        fn encode(&self, message: &str) -> Vec<NudgeStep> {
            let mut steps: Vec<NudgeStep> = message
                .as_bytes()
                .iter()
                .map(|b| NudgeStep {
                    bytes: vec![*b],
                    delay_after: Some(std::time::Duration::from_millis(1)),
                })
                .collect();
            steps.push(NudgeStep { bytes: b"\r".to_vec(), delay_after: None });
            steps
        }
    }

    let StoreCtx { store: state, .. } = StoreBuilder::new()
        .agent_state(AgentState::Idle)
        .nudge_encoder(Arc::new(SlowPerByteEncoder))
        .build();
    state.ready.store(true, std::sync::atomic::Ordering::Release);

    let big = "x".repeat(12_000);
    let start = std::time::Instant::now();
    let result = handle_nudge(&state, &big).await.map_err(|e| anyhow::anyhow!("{e}"))?;
    let elapsed = start.elapsed();

    assert!(result.delivered);
    assert!(
        elapsed < std::time::Duration::from_millis(50),
        "handle_nudge returned in {elapsed:?}, expected <50ms (delivery should be detached)"
    );
    Ok(())
}

#[tokio::test]
async fn nudge_trailing_cr_delivered_after_caller_drops() -> anyhow::Result<()> {
    // Simulate the bug from ka-0000000936: the caller's HTTP timeout fires
    // while delivery is still in flight, dropping the response future.
    // With detached delivery, the trailing \r must still reach the PTY.
    use crate::driver::{NudgeEncoder, NudgeStep};
    struct SlowEncoder;
    impl NudgeEncoder for SlowEncoder {
        fn encode(&self, message: &str) -> Vec<NudgeStep> {
            // Two steps: payload, then a small delay, then trailing \r.
            vec![
                NudgeStep {
                    bytes: message.as_bytes().to_vec(),
                    delay_after: Some(std::time::Duration::from_millis(50)),
                },
                NudgeStep { bytes: b"\r".to_vec(), delay_after: None },
            ]
        }
    }

    let StoreCtx { store: state, mut input_rx, .. } = StoreBuilder::new()
        .agent_state(AgentState::Idle)
        .nudge_encoder(Arc::new(SlowEncoder))
        .build();
    state.ready.store(true, std::sync::atomic::Ordering::Release);

    // Run handle_nudge inside a `select!` that drops it after 1 ms — far
    // shorter than the 50 ms delay between payload and trailing \r.
    let state_clone = Arc::clone(&state);
    let nudge_fut = async move {
        let _ = handle_nudge(&state_clone, "hello").await;
    };
    tokio::select! {
        _ = nudge_fut => {}
        _ = tokio::time::sleep(std::time::Duration::from_millis(1)) => {
            // Drop the nudge future mid-flight.
        }
    }

    // Drain WaitForDrain markers and collect Write payloads with a generous
    // budget for the detached task to complete the full sequence.
    let deadline = std::time::Instant::now() + std::time::Duration::from_secs(2);
    let mut writes: Vec<bytes::Bytes> = Vec::new();
    while std::time::Instant::now() < deadline {
        match tokio::time::timeout(std::time::Duration::from_millis(200), input_rx.recv()).await {
            Ok(Some(InputEvent::Write(data))) => {
                let is_cr = data.as_ref() == b"\r";
                writes.push(data);
                if is_cr {
                    break;
                }
            }
            Ok(Some(InputEvent::WaitForDrain(tx))) => {
                let _ = tx.send(());
            }
            Ok(Some(_)) => continue,
            Ok(None) | Err(_) => continue,
        }
    }

    assert!(
        writes.iter().any(|w| w.as_ref() == b"hello"),
        "expected payload in writes, got: {writes:?}"
    );
    assert!(
        writes.iter().any(|w| w.as_ref() == b"\r"),
        "expected trailing \\r in writes, got: {writes:?}"
    );
    Ok(())
}

#[tokio::test]
async fn nudge_back_to_back_serialise_via_gate() -> anyhow::Result<()> {
    // Two back-to-back nudges must serialise: the second handle_nudge call
    // must wait for the first detached delivery to complete (i.e. for the
    // first guard to drop) before it is allowed to proceed past the gate.
    use crate::driver::{NudgeEncoder, NudgeStep};
    struct DelayedEncoder;
    impl NudgeEncoder for DelayedEncoder {
        fn encode(&self, message: &str) -> Vec<NudgeStep> {
            // Tag the payload with the message itself so we can verify
            // ordering downstream.
            vec![
                NudgeStep {
                    bytes: message.as_bytes().to_vec(),
                    delay_after: Some(std::time::Duration::from_millis(150)),
                },
                NudgeStep { bytes: b"\r".to_vec(), delay_after: None },
            ]
        }
    }

    let StoreCtx { store: state, mut input_rx, .. } = StoreBuilder::new()
        .agent_state(AgentState::Idle)
        .nudge_encoder(Arc::new(DelayedEncoder))
        .build();
    state.ready.store(true, std::sync::atomic::Ordering::Release);

    // Drain the input channel concurrently to keep the spawned deliveries
    // moving — without a consumer the WaitForDrain barriers would stall.
    let writes: Arc<tokio::sync::Mutex<Vec<bytes::Bytes>>> =
        Arc::new(tokio::sync::Mutex::new(Vec::new()));
    let writes_clone = Arc::clone(&writes);
    let drainer = tokio::spawn(async move {
        while let Some(ev) = input_rx.recv().await {
            match ev {
                InputEvent::Write(data) => {
                    writes_clone.lock().await.push(data);
                }
                InputEvent::WaitForDrain(tx) => {
                    let _ = tx.send(());
                }
                _ => {}
            }
        }
    });

    // First nudge returns quickly (detached) but holds the gate while
    // delivery runs.  Second must wait until the first guard drops.
    let r1 = handle_nudge(&state, "first").await.map_err(|e| anyhow::anyhow!("{e}"))?;
    assert!(r1.delivered);

    let start = std::time::Instant::now();
    let r2 = handle_nudge(&state, "second").await.map_err(|e| anyhow::anyhow!("{e}"))?;
    let elapsed = start.elapsed();
    assert!(r2.delivered);
    // Encoder delay is 150 ms; serialisation forces second acquire to wait
    // for the first delivery to finish.  Allow generous slack.
    assert!(
        elapsed >= std::time::Duration::from_millis(100),
        "expected second nudge to wait for first delivery (>=100ms), got {elapsed:?}"
    );

    // Give the second delivery a moment to flush, then snapshot.
    tokio::time::sleep(std::time::Duration::from_millis(400)).await;
    drop(state);
    let _ = drainer.await;
    let final_writes = writes.lock().await.clone();

    // Verify ordering: "first" appears before "second" in the write stream.
    let first_idx = final_writes.iter().position(|w| w.as_ref() == b"first");
    let second_idx = final_writes.iter().position(|w| w.as_ref() == b"second");
    assert!(first_idx.is_some(), "missing first payload: {final_writes:?}");
    assert!(second_idx.is_some(), "missing second payload: {final_writes:?}");
    assert!(first_idx < second_idx, "expected first before second: {final_writes:?}");
    Ok(())
}

#[tokio::test]
async fn nudge_permission_prompt_rejected() -> anyhow::Result<()> {
    let perm_state = AgentState::Prompt {
        prompt: crate::driver::PromptContext::new(crate::driver::PromptKind::Permission)
            .with_subtype("tool")
            .with_tool("Bash")
            .with_ready(),
    };
    let StoreCtx { store: state, .. } = StoreBuilder::new()
        .agent_state(perm_state)
        .nudge_encoder(Arc::new(StubNudgeEncoder))
        .build();
    state.ready.store(true, std::sync::atomic::Ordering::Release);

    let result = handle_nudge(&state, "hello").await.map_err(|e| anyhow::anyhow!("{e}"))?;
    assert!(!result.delivered);
    assert_eq!(result.state_before.as_deref(), Some("prompt"));
    assert!(result.reason.as_deref().unwrap_or("").contains("agent is prompt"));
    Ok(())
}

#[tokio::test]
async fn respond_not_ready_returns_error() -> anyhow::Result<()> {
    let StoreCtx { store: state, .. } =
        StoreBuilder::new().respond_encoder(Arc::new(StubRespondEncoder)).build();
    let result = handle_respond(&state, None, None, None, &[]).await;
    assert!(result.is_err());
    assert_eq!(result.unwrap_err(), crate::error::ErrorCode::NotReady);
    Ok(())
}

#[tokio::test]
async fn respond_setup_prompt_delivers_option_to_pty() -> anyhow::Result<()> {
    let setup_state = AgentState::Prompt {
        prompt: crate::driver::PromptContext::new(crate::driver::PromptKind::Setup)
            .with_subtype("theme_picker")
            .with_options(vec!["Dark mode".to_owned(), "Light mode".to_owned()])
            .with_ready(),
    };
    let StoreCtx { store: state, mut input_rx, .. } = StoreBuilder::new()
        .agent_state(setup_state)
        .respond_encoder(Arc::new(StubRespondEncoder))
        .build();
    state.ready.store(true, std::sync::atomic::Ordering::Release);

    let result = handle_respond(&state, None, Some(2), None, &[])
        .await
        .map_err(|e| anyhow::anyhow!("{e}"))?;
    assert!(result.delivered);
    assert_eq!(result.prompt_type.as_deref(), Some("setup"));

    // Verify PTY received the encoded bytes ("2\r" from StubRespondEncoder).
    let event = input_rx.recv().await;
    assert!(matches!(event, Some(InputEvent::Write(data)) if data == &b"2\r"[..]));
    Ok(())
}

#[tokio::test]
async fn respond_no_prompt_returns_soft_failure() -> anyhow::Result<()> {
    let StoreCtx { store: state, .. } = StoreBuilder::new()
        .agent_state(AgentState::Working)
        .respond_encoder(Arc::new(StubRespondEncoder))
        .build();
    state.ready.store(true, std::sync::atomic::Ordering::Release);

    let result = handle_respond(&state, Some(true), None, None, &[])
        .await
        .map_err(|e| anyhow::anyhow!("{e}"))?;
    assert!(!result.delivered);
    assert!(result.prompt_type.is_none());
    assert_eq!(result.reason.as_deref(), Some("no prompt active"));
    Ok(())
}

#[tokio::test]
async fn input_writes_text() -> anyhow::Result<()> {
    let StoreCtx { store: state, mut input_rx, .. } = StoreBuilder::new().build();
    let len = handle_input(&state, "hello".to_owned(), false).await;
    assert_eq!(len, 5);
    let event = input_rx.recv().await;
    assert!(matches!(event, Some(InputEvent::Write(data)) if data == &b"hello"[..]));
    Ok(())
}

#[tokio::test]
async fn input_with_enter_appends_cr() -> anyhow::Result<()> {
    let StoreCtx { store: state, mut input_rx, .. } = StoreBuilder::new().build();
    let len = handle_input(&state, "hi".to_owned(), true).await;
    assert_eq!(len, 3); // "hi\r"
    let event = input_rx.recv().await;
    assert!(matches!(event, Some(InputEvent::Write(data)) if data == &b"hi\r"[..]));
    Ok(())
}

#[tokio::test]
async fn input_raw_writes_bytes() -> anyhow::Result<()> {
    let StoreCtx { store: state, mut input_rx, .. } = StoreBuilder::new().build();
    let len = handle_input_raw(&state, vec![0x1b, 0x5b, 0x41]).await;
    assert_eq!(len, 3);
    let event = input_rx.recv().await;
    assert!(matches!(event, Some(InputEvent::Write(data)) if data == &[0x1b, 0x5b, 0x41][..]));
    Ok(())
}

#[tokio::test]
async fn keys_valid() -> anyhow::Result<()> {
    let StoreCtx { store: state, mut input_rx, .. } = StoreBuilder::new().build();
    let len = handle_keys(&state, &["Enter".to_owned(), "Tab".to_owned()])
        .await
        .map_err(|e| anyhow::anyhow!("{e}"))?;
    assert_eq!(len, 2); // \r + \t
    let event = input_rx.recv().await;
    assert!(matches!(event, Some(InputEvent::Write(_))));
    Ok(())
}

#[tokio::test]
async fn keys_invalid_returns_error() -> anyhow::Result<()> {
    let StoreCtx { store: state, .. } = StoreBuilder::new().build();
    let result = handle_keys(&state, &["SuperKey".to_owned()]).await;
    assert_eq!(result.unwrap_err(), "SuperKey");
    Ok(())
}

#[tokio::test]
async fn resize_valid() -> anyhow::Result<()> {
    let StoreCtx { store: state, mut input_rx, .. } = StoreBuilder::new().build();
    handle_resize(&state, 120, 40).await.map_err(|e| anyhow::anyhow!("{e}"))?;
    let event = input_rx.recv().await;
    assert!(matches!(event, Some(InputEvent::Resize { cols: 120, rows: 40 })));
    Ok(())
}

#[tokio::test]
async fn resize_zero_cols_rejected() -> anyhow::Result<()> {
    let StoreCtx { store: state, .. } = StoreBuilder::new().build();
    let result = handle_resize(&state, 0, 24).await;
    assert!(result.is_err());
    Ok(())
}

#[tokio::test]
async fn resize_zero_rows_rejected() -> anyhow::Result<()> {
    let StoreCtx { store: state, .. } = StoreBuilder::new().build();
    let result = handle_resize(&state, 80, 0).await;
    assert!(result.is_err());
    Ok(())
}

#[tokio::test]
async fn signal_valid() -> anyhow::Result<()> {
    let StoreCtx { store: state, mut input_rx, .. } = StoreBuilder::new().build();
    handle_signal(&state, "SIGINT").await.map_err(|e| anyhow::anyhow!("{e}"))?;
    let event = input_rx.recv().await;
    assert!(matches!(event, Some(InputEvent::Signal(crate::event::PtySignal::Int))));
    Ok(())
}

#[tokio::test]
async fn signal_unknown_returns_error() -> anyhow::Result<()> {
    let StoreCtx { store: state, .. } = StoreBuilder::new().build();
    let result = handle_signal(&state, "SIGFOO").await;
    assert_eq!(result.unwrap_err(), "SIGFOO");
    Ok(())
}
