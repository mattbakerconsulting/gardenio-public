// SPDX-License-Identifier: BUSL-1.1
// Copyright (c) 2026 Matt Baker <fimbaz@gmail.com>

//! Regression tests for the Fable-consent auto-answer race (ka-0000002659).
//!
//! The wedge these pin down: the consent dialog is classified from the screen
//! at `state_seq=N`, and ~4 ms later the injected initial prompt lands in the
//! session log, so Tier 2 emits `log:user` → `Working` at `state_seq=N+1`. The
//! dialog is still up — the agent is emphatically not working behind a modal —
//! but `auto_dismiss` bailed on the bare seq check and nothing retried, so
//! every unattended Fable spawn hung on the first turn until the ~3 min
//! `switch_timeout` restart happened to re-classify it cleanly.
//!
//! These run the real `Session` select-loop over a mock PTY holding the
//! prod-shape 2.1.257 consent screen, so classification, the seq bump, and
//! keystroke delivery all go through the production codepath.

use std::sync::Arc;
use std::time::Duration;

use bytes::Bytes;
use tokio_util::sync::CancellationToken;

use crate::config::{Config, GroomLevel};
use crate::driver::claude::screen::classify_claude_screen;
use crate::driver::{AgentState, PromptContext, PromptKind, ScreenClassifier};
use crate::session::{Session, SessionConfig};
use crate::test_support::{MockDetector, MockPty, StoreBuilder, StoreCtx, StubRespondEncoder};

/// The 2.1.257 Fable 5.1 consent picker exactly as prod rendered it: unnumbered
/// options, confirm SECOND, cursor defaulting to the decline option.
const CONSENT_SCREEN: &str =
    include_str!("../driver/claude/fixtures/fable_consent_51_title.screen.txt");

/// The arrow-nav burst that answers the picker above: one Down to move the
/// cursor from "Switch to Sonnet 5 and continue" onto "Continue with Fable 5.1",
/// then Enter — a single write (see `picker_navigate_and_confirm`).
const EXPECTED_BURST: &[u8] = b"\x1b[B\r";

/// Render the fixture as PTY output (CRLF line endings, as a real terminal
/// emits) so the screen emulator lays it out on separate lines.
fn consent_screen_bytes() -> Bytes {
    Bytes::from(CONSENT_SCREEN.replace('\n', "\r\n"))
}

/// The prompt context the screen classifier produces for the screen above.
fn classified_consent_prompt() -> AgentState {
    AgentState::Prompt {
        prompt: PromptContext::new(PromptKind::Setup)
            .with_subtype("fable_consent")
            .with_options(vec![
                "Switch to Sonnet 5 and continue".to_owned(),
                "Continue with Fable 5.1".to_owned(),
            ])
            .with_selected(Some(0))
            .with_ready(),
    }
}

/// A bare `Prompt(Permission)` with no subtype — what the Tier-1 hook path
/// emits for `Notification(permission_prompt)`, and what the wedged pod was
/// sitting in at `seq=5`.
fn subtypeless_permission_prompt() -> AgentState {
    AgentState::Prompt { prompt: PromptContext::new(PromptKind::Permission) }
}

/// Run the session with the consent screen on the PTY and the given detector
/// emissions, returning every byte the session wrote to the backend.
async fn run_with_screen(
    states: Vec<(Duration, AgentState)>,
    classifier: Option<ScreenClassifier>,
) -> anyhow::Result<Vec<Bytes>> {
    let mut config = Config::test();
    config.drain_timeout_ms = Some(0);

    let StoreCtx { store, mut input_rx, .. } = StoreBuilder::new()
        .ring_size(65536)
        .groom(GroomLevel::Auto)
        .respond_encoder(Arc::new(StubRespondEncoder))
        .build();

    let backend = MockPty::with_output(vec![consent_screen_bytes()]).drain_input();
    let captured = backend.captured_input();

    let shutdown = CancellationToken::new();
    let mut session_config = SessionConfig::new(Arc::clone(&store), backend)
        .with_shutdown(shutdown.clone())
        .with_detectors(vec![Box::new(MockDetector::new(1, states))]);
    if let Some(classifier) = classifier {
        session_config = session_config.with_screen_classifier(classifier);
    }
    let session = Session::new(&config, session_config);

    let sd = shutdown.clone();
    tokio::spawn(async move {
        tokio::time::sleep(Duration::from_millis(500)).await;
        sd.cancel();
    });

    let _ = session.run_to_exit(&config, &mut input_rx).await?;
    let input = captured.lock().clone();
    Ok(input)
}

fn contains_burst(input: &[Bytes]) -> bool {
    input.iter().any(|b| b.as_ref() == EXPECTED_BURST)
}

/// THE REGRESSION: a `state_seq` bump between classification and delivery — the
/// `log:user` race — must NOT drop the auto-dismiss burst while the dialog is
/// still on screen. The picker still gets Down + Enter.
#[tokio::test]
async fn state_seq_bump_still_answers_the_consent_picker() -> anyhow::Result<()> {
    let input = run_with_screen(
        vec![
            // Dialog classified from the screen (`screen:setup`).
            (Duration::from_millis(20), classified_consent_prompt()),
            // 10 ms later the injected initial prompt lands in the session log
            // and Tier 2 emits `log:user` → Working, bumping state_seq. The
            // 50 ms test dismiss delay has NOT elapsed yet, so the burst is
            // still pending when the bump lands — exactly the prod ordering.
            (Duration::from_millis(10), AgentState::Working),
        ],
        Some(Arc::new(classify_claude_screen)),
    )
    .await?;

    assert!(
        contains_burst(&input),
        "seq bump while the dialog is still on screen must not cancel the answer; \
         expected {EXPECTED_BURST:?} in captured input: {input:?}"
    );
    Ok(())
}

/// Control for the test above: with no screen classifier wired (the pre-fix
/// behaviour, and still the behaviour for drivers that have no classifier) the
/// seq bump drops the burst. This is the mechanism the fix removes — it is here
/// so a future change that stops plumbing the classifier fails loudly rather
/// than silently reintroducing the wedge.
#[tokio::test]
async fn state_seq_bump_without_a_classifier_drops_the_burst() -> anyhow::Result<()> {
    let input = run_with_screen(
        vec![
            (Duration::from_millis(20), classified_consent_prompt()),
            (Duration::from_millis(10), AgentState::Working),
        ],
        None,
    )
    .await?;

    assert!(
        !contains_burst(&input),
        "without a classifier there is nothing to re-validate against, so the \
         burst must stay cancelled: {input:?}"
    );
    Ok(())
}

/// No seq bump at all: the ordinary path still answers the picker (guards
/// against the refactor breaking the common case).
#[tokio::test]
async fn undisturbed_consent_prompt_is_answered() -> anyhow::Result<()> {
    let input = run_with_screen(
        vec![(Duration::from_millis(20), classified_consent_prompt())],
        Some(Arc::new(classify_claude_screen)),
    )
    .await?;

    assert!(contains_burst(&input), "expected {EXPECTED_BURST:?} in captured input: {input:?}");
    Ok(())
}

/// The second half of the wedge: after the race the dialog came back only as
/// `hook:prompt(permission)` — a bare Permission prompt with no subtype, which
/// carries no dismiss policy — and the Tier-5 screen detector never re-emitted
/// it because it dedupes on its own last-emitted state and the screen is
/// static. Re-classifying the current screen must recover the subtype and
/// answer the dialog.
#[tokio::test]
async fn subtypeless_prompt_is_reclassified_from_the_screen_and_answered() -> anyhow::Result<()> {
    let input = run_with_screen(
        vec![(Duration::from_millis(20), subtypeless_permission_prompt())],
        Some(Arc::new(classify_claude_screen)),
    )
    .await?;

    assert!(
        contains_burst(&input),
        "a subtype-less prompt over a classifiable dialog must be re-classified \
         and answered; expected {EXPECTED_BURST:?} in captured input: {input:?}"
    );
    Ok(())
}

/// The re-classified prompt context is published to API consumers, not just
/// used internally to pick keystrokes.
#[tokio::test]
async fn reclassified_prompt_context_is_published() -> anyhow::Result<()> {
    let mut config = Config::test();
    config.drain_timeout_ms = Some(0);

    let StoreCtx { store, mut input_rx, .. } = StoreBuilder::new()
        .ring_size(65536)
        .groom(GroomLevel::Auto)
        .respond_encoder(Arc::new(StubRespondEncoder))
        .build();

    let backend = MockPty::with_output(vec![consent_screen_bytes()]).drain_input();
    let shutdown = CancellationToken::new();
    let session = Session::new(
        &config,
        SessionConfig::new(Arc::clone(&store), backend)
            .with_shutdown(shutdown.clone())
            .with_detectors(vec![Box::new(MockDetector::new(
                1,
                vec![(Duration::from_millis(20), subtypeless_permission_prompt())],
            ))])
            .with_screen_classifier(Arc::new(classify_claude_screen)),
    );

    // Poll the live store while the session runs — reading it after the loop
    // exits would only see the terminal Exited state.
    let probe = Arc::clone(&store);
    let sd = shutdown.clone();
    let observed = tokio::spawn(async move {
        let mut found = None;
        for _ in 0..100 {
            tokio::time::sleep(Duration::from_millis(5)).await;
            if let AgentState::Prompt { ref prompt } = *probe.driver.agent_state.read().await {
                if prompt.subtype.is_some() {
                    found = Some(prompt.clone());
                    break;
                }
            }
        }
        sd.cancel();
        found
    });

    let _ = session.run_to_exit(&config, &mut input_rx).await?;

    let prompt = observed.await?.ok_or_else(|| {
        anyhow::anyhow!("re-classification never published a subtyped prompt context")
    })?;
    assert_eq!(
        prompt.subtype.as_deref(),
        Some("fable_consent"),
        "re-classification must publish the recovered subtype"
    );
    assert_eq!(prompt.selected, Some(0), "cursor position must come from the screen");
    Ok(())
}
