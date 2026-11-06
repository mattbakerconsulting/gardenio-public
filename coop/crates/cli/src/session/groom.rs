// SPDX-License-Identifier: BUSL-1.1
// Copyright (c) 2026 Matt Baker <fimbaz@gmail.com>

//! Prompt enrichment and auto-dismiss logic.
//!
//! These tasks run as detached spawned futures, polling the screen buffer
//! for option labels and auto-dismissing disruption prompts.

use std::sync::Arc;
use std::time::Duration;

use bytes::Bytes;
use tracing::{debug, warn};

use crate::config::{Config, GroomLevel};
use crate::driver::{
    disruption_option, AgentState, NudgeStep, OptionParser, PromptContext, PromptKind,
    ScreenClassifier,
};
use crate::event::{InputEvent, PromptOutcome, TransitionEvent};
use crate::transport::Store;

/// Spawn deferred option enrichment for Permission/Plan prompts.
///
/// Hook events fire before the screen renders numbered options,
/// so we wait briefly for the PTY output to catch up, then parse
/// options from the screen and re-broadcast the enriched state.
pub(super) fn spawn_enrichment(store: &Arc<Store>, state_seq: u64, parser: &OptionParser) {
    let app = Arc::clone(store);
    let parser = Arc::clone(parser);
    tokio::spawn(enrich_prompt_options(app, state_seq, parser));
}

/// Wait for the screen to render prompt options, parse them, and re-broadcast the enriched prompt context.
///
/// Retries up to `MAX_ATTEMPTS` times, then falls back to universal Accept/Cancel options
/// that encode to Enter/Esc.
async fn enrich_prompt_options(app: Arc<Store>, expected_seq: u64, parser: OptionParser) {
    const MAX_ATTEMPTS: u32 = 10;
    const POLL_INTERVAL: Duration = Duration::from_millis(200);

    let mut last_snap_lines = 0usize;

    for _ in 0..MAX_ATTEMPTS {
        tokio::time::sleep(POLL_INTERVAL).await;

        // Bail if the state has changed since we spawned.
        let current_seq = app.driver.state_seq.load(std::sync::atomic::Ordering::Acquire);
        if current_seq != expected_seq {
            return;
        }

        let screen = app.terminal.screen.read().await;
        let snap = screen.snapshot_text();
        drop(screen);
        last_snap_lines = snap.lines.len();

        let options = parser(&snap.lines);
        if !options.is_empty() {
            let mut agent = app.driver.agent_state.write().await;

            // Re-check seq under the write lock.
            let current_seq = app.driver.state_seq.load(std::sync::atomic::Ordering::Acquire);
            if current_seq != expected_seq {
                return;
            }

            if let AgentState::Prompt { ref mut prompt } = *agent {
                if matches!(prompt.kind, PromptKind::Permission | PromptKind::Plan) {
                    prompt.options = options;
                    prompt.ready = true;

                    let next = agent.clone();
                    drop(agent);

                    let last_message = app.driver.last_message.read().await.clone();
                    let _ = app.channels.state_tx.send(TransitionEvent {
                        prev: next.clone(),
                        next,
                        seq: expected_seq,
                        cause: "ready".to_owned(),
                        last_message,
                    });
                }
            }
            return;
        }
    }

    // All retries exhausted — set fallback options so API consumers have
    // something to present (Enter for accept, Esc for cancel).
    debug!(last_snap_lines, "prompt option enrichment: setting fallback options");

    let mut agent = app.driver.agent_state.write().await;
    let current_seq = app.driver.state_seq.load(std::sync::atomic::Ordering::Acquire);
    if current_seq != expected_seq {
        return;
    }

    if let AgentState::Prompt { ref mut prompt } = *agent {
        if matches!(prompt.kind, PromptKind::Permission | PromptKind::Plan) {
            prompt.options = vec!["Accept".to_string(), "Cancel".to_string()];
            prompt.options_fallback = true;
            prompt.ready = true;

            let next = agent.clone();
            drop(agent);

            let last_message = app.driver.last_message.read().await.clone();
            let _ = app.channels.state_tx.send(TransitionEvent {
                prev: next.clone(),
                next,
                seq: expected_seq,
                cause: "ready".to_owned(),
                last_message,
            });
        }
    }
}

/// Spawn re-classification of a prompt whose context carries no subtype.
///
/// The Tier-1 hook path emits a bare `Prompt(Permission)` with an empty
/// context for `Notification(permission_prompt)`. When the thing actually on
/// screen is a *screen-classified* dialog (a Fable consent gate, a weekly-limit
/// dialog, a protected-dir edit prompt), that bare context has no subtype, so
/// `dismiss_plan` finds no policy for it and the session wedges with the dialog
/// still up — and the Tier-5 screen detector will not rescue it, because it
/// dedupes on its own last-emitted state and the screen is static (ka-0000002659).
///
/// So re-run the classifier against the current screen on a slow tick for as
/// long as the prompt stays unanswered. If it classifies a subtyped dialog,
/// replace the prompt context in place, re-broadcast it, and spawn auto-dismiss
/// for the *classified* prompt. Polling (rather than one shot) covers the hook
/// firing before the dialog has finished rendering, and retries a static wedge.
pub(super) fn spawn_reclassify(
    store: &Arc<Store>,
    config: &Config,
    state_seq: u64,
    classifier: &ScreenClassifier,
) {
    let store = Arc::clone(store);
    let classifier = Arc::clone(classifier);
    tokio::spawn(reclassify_prompt(
        store,
        config.groom_dismiss_delay(),
        config.groom_reclassify_poll(),
        state_seq,
        classifier,
    ));
}

/// How many polls to attempt before giving up (30 × 1s by default = 30s,
/// comfortably inside the ~3 min `switch_timeout` restart that used to be the
/// only escape from this wedge).
const RECLASSIFY_ATTEMPTS: u32 = 30;

async fn reclassify_prompt(
    store: Arc<Store>,
    dismiss_delay: Duration,
    poll: Duration,
    expected_seq: u64,
    classifier: ScreenClassifier,
) {
    for _ in 0..RECLASSIFY_ATTEMPTS {
        tokio::time::sleep(poll).await;

        // Someone answered the prompt (or the agent moved on) — nothing to do.
        if store.driver.state_seq.load(std::sync::atomic::Ordering::Acquire) != expected_seq {
            return;
        }

        let Some(fresh) = classify_current_screen(&store, &classifier).await else {
            continue;
        };
        // Only a *subtyped* classification is actionable: it is the subtype that
        // selects a dismiss policy. An unsubtyped re-classification would leave
        // us exactly where we started.
        if fresh.subtype.is_none() {
            continue;
        }

        {
            let mut agent = store.driver.agent_state.write().await;
            // Re-check seq under the write lock.
            if store.driver.state_seq.load(std::sync::atomic::Ordering::Acquire) != expected_seq {
                return;
            }
            let AgentState::Prompt { ref mut prompt } = *agent else {
                return;
            };
            // Never clobber a context that already has a subtype — a
            // higher-confidence detector got there first.
            if prompt.subtype.is_some() {
                return;
            }
            warn!(
                subtype = fresh.subtype.as_deref().unwrap_or_default(),
                kind = fresh.kind.as_str(),
                "re-classified subtype-less prompt from the current screen — the dialog on \
                 screen is answerable, the hook context was not"
            );
            *prompt = fresh.clone();
            let next = agent.clone();
            drop(agent);

            let last_message = store.driver.last_message.read().await.clone();
            let _ = store.channels.state_tx.send(TransitionEvent {
                prev: next.clone(),
                next,
                seq: expected_seq,
                cause: "screen:reclassify".to_owned(),
                last_message,
            });
        }

        let classifier = Some(Arc::clone(&classifier));
        spawn_auto_dismiss_inner(&store, &fresh, dismiss_delay, expected_seq, &classifier);
        return;
    }
}

/// Classify the current screen, returning the prompt context if it classifies
/// as a prompt. `None` for anything else (no classification, or a non-prompt
/// state such as Idle/Working).
async fn classify_current_screen(
    store: &Arc<Store>,
    classifier: &ScreenClassifier,
) -> Option<PromptContext> {
    let snapshot = {
        let screen = store.terminal.screen.read().await;
        screen.snapshot_text()
    };
    match classifier(&snapshot) {
        Some((AgentState::Prompt { prompt }, _)) => Some(prompt),
        _ => None,
    }
}

/// Spawn auto-dismiss of a disruption prompt in groom=auto mode.
///
/// The prompt state is broadcast BEFORE auto-dismiss so API clients see
/// the action transparently.
pub(super) fn spawn_auto_dismiss(
    store: &Arc<Store>,
    prompt: &PromptContext,
    config: &Config,
    state_seq: u64,
    classifier: &Option<ScreenClassifier>,
) {
    spawn_auto_dismiss_inner(store, prompt, config.groom_dismiss_delay(), state_seq, classifier);
}

fn spawn_auto_dismiss_inner(
    store: &Arc<Store>,
    prompt: &PromptContext,
    dismiss_delay: Duration,
    state_seq: u64,
    classifier: &Option<ScreenClassifier>,
) {
    if store.config.groom != GroomLevel::Auto {
        return;
    }

    let Some((steps, groom_option)) = dismiss_plan(store, prompt) else {
        return;
    };

    tokio::spawn(auto_dismiss(AutoDismiss {
        store: Arc::clone(store),
        steps,
        dismiss_delay,
        expected_seq: state_seq,
        prompt: prompt.clone(),
        groom_option,
        classifier: classifier.clone(),
    }));
}

/// The keystroke burst that answers a prompt, plus the option number to report
/// in the resulting [`PromptOutcome`].
type DismissPlan = (Vec<NudgeStep>, Option<u32>);

/// Decide how to answer a disruption prompt, or `None` to leave it alone.
///
/// Pure policy over the prompt context: it reads only `store.config`, so it can
/// be re-run against a freshly classified context (see [`ready_to_deliver`]).
fn dismiss_plan(store: &Store, prompt: &PromptContext) -> Option<DismissPlan> {
    // The bypass-immune `rm`-of-home/root circuit breaker (subtype
    // `rm_circuit_breaker`) is declined with Esc ("Esc to cancel"), which
    // unblocks the session while refusing the destructive rm. Esc is robust to
    // the dialog's option numbering — unlike a numbered selection — and matches
    // the operator decision to auto-answer No/Esc (ka-0000001287). It needs
    // neither the respond encoder nor `disruption_option`, so handle it first.
    if prompt.subtype.as_deref() == Some("rm_circuit_breaker") {
        warn!("auto-declining rm-of-home/root circuit-breaker dialog with Esc");
        return Some((vec![NudgeStep { bytes: b"\x1b".to_vec(), delay_after: None }], None));
    }

    if prompt.subtype.as_deref() == Some("fable_consent") {
        // Free Fable consent gate. 2.1.257 (ka-0000006026) made this an
        // UNNUMBERED arrow-select picker whose confirm option ("Continue with
        // Fable N") is no longer at a fixed position and whose cursor defaults
        // to the DECLINE option — so the old digit '1' selected nothing and the
        // pod wedged. Answer by LABEL: arrow-navigate from the cursor line to
        // the confirm line, then Enter. Falls back to Esc (keeps the session
        // alive on another model) if the confirm label or cursor is missing.
        return match (crate::driver::fable_consent_confirm_index(&prompt.options), prompt.selected)
        {
            (Some(target), Some(cursor)) => {
                warn!(
                    "confirming free Fable consent by label — navigating picker from option \
                     {cursor} to {target} ({:?}) then Enter",
                    prompt.options.get(target)
                );
                Some((
                    crate::driver::picker_navigate_and_confirm(cursor, target),
                    Some(target as u32 + 1),
                ))
            }
            _ => {
                warn!(
                    "Fable consent picker offers no locatable confirm label / cursor \
                     (options={:?}, selected={:?}) — declining with Esc to keep the session alive \
                     rather than wedging it",
                    prompt.options, prompt.selected
                );
                Some((vec![NudgeStep { bytes: b"\x1b".to_vec(), delay_after: None }], None))
            }
        };
    }

    if prompt.subtype.as_deref() == Some("fable_credits") {
        // Fable usage-credit gate — confirming would enable overage billing, buy
        // credits, or file a request with this org's admins. Decline with Esc:
        // order-independent (2.1.257 made the picker unnumbered, so the old digit
        // '2' selected nothing) and it dismisses without any spending decision,
        // continuing on another model.
        warn!(
            "declining Fable usage-credit dialog with Esc — confirming would enable overage \
             billing, buy credits, or ask this org's admins for them; continuing on another \
             model. Enable usage credits for this account to run on Fable 5."
        );
        return Some((vec![NudgeStep { bytes: b"\x1b".to_vec(), delay_after: None }], None));
    }

    if prompt.subtype.as_deref() == Some("fable_limit") {
        // Fable weekly-limit dialog ("What do you want to do?", ka-0000003007).
        // Answer the STOP/WAIT option — the one that neither spends nor upgrades
        // — selected by LABEL, never by a fixed position: its option set is
        // conditional and one option ("Add funds…") walks into a card-charging
        // purchase flow that must never be auto-pressed. If no safe option is on
        // screen, decline with Esc, which dismisses without any spending
        // decision. Either way the session stays alive without spending.
        return match crate::driver::fable_limit_safe_option(&prompt.options) {
            Some(option) => {
                warn!(
                    "answering Fable weekly-limit dialog with safe stop/wait option {option} \
                     ({:?}) — neither spends nor upgrades; the limit resets on its weekly window",
                    prompt.options.get((option as usize).saturating_sub(1))
                );
                let encoder = store.config.respond_encoder.as_ref()?;
                Some((encoder.encode_setup(option), Some(option)))
            }
            None => {
                warn!(
                    "Fable weekly-limit dialog offers no safe stop/wait option ({:?}) — declining \
                     with Esc rather than pressing a spend/upgrade option",
                    prompt.options
                );
                Some((vec![NudgeStep { bytes: b"\x1b".to_vec(), delay_after: None }], None))
            }
        };
    }

    let option = disruption_option(prompt)?;
    if prompt.subtype.as_deref() == Some("settings_error") {
        warn!("auto-dismissing settings error dialog (option {option})");
    }
    let encoder = store.config.respond_encoder.as_ref()?;

    // "Press Enter to continue" screens have no numbered
    // options — just send Enter instead of "N" + delay + Enter.
    let steps = if prompt.options.is_empty() {
        vec![NudgeStep { bytes: b"\r".to_vec(), delay_after: None }]
    } else if prompt.kind == PromptKind::Permission {
        encoder.encode_permission(option)
    } else {
        encoder.encode_setup(option)
    };
    let groom_option = if prompt.options.is_empty() { None } else { Some(option) };
    Some((steps, groom_option))
}

/// A pending auto-dismiss burst.
struct AutoDismiss {
    store: Arc<Store>,
    steps: Vec<NudgeStep>,
    dismiss_delay: Duration,
    expected_seq: u64,
    prompt: PromptContext,
    groom_option: Option<u32>,
    classifier: Option<ScreenClassifier>,
}

async fn auto_dismiss(mut task: AutoDismiss) {
    tokio::time::sleep(task.dismiss_delay).await;

    if !ready_to_deliver(&mut task).await {
        return;
    }
    let _delivery = task.store.input_gate.acquire().await;
    // Re-check after gate acquisition.
    if !ready_to_deliver(&mut task).await {
        return;
    }

    deliver(&task.store, std::mem::take(&mut task.steps)).await;

    let _ = task.store.channels.prompt_tx.send(PromptOutcome {
        source: "groom".to_owned(),
        r#type: task.prompt.kind.as_str().to_owned(),
        subtype: task.prompt.subtype.clone(),
        option: task.groom_option,
    });
}

/// Decide whether the pending burst should still be delivered.
///
/// The cheap check is `state_seq == expected_seq`: nothing has happened since
/// the prompt was classified. But a seq bump is NOT evidence that the dialog was
/// answered — any detector emission bumps it. The startup case that motivated
/// this (ka-0000002659): the Fable consent dialog is classified from the screen
/// at seq=N, then 4 ms later the injected initial prompt lands in the session log
/// and Tier 2 emits `log:user` → `Working` at seq=N+1. The dialog is still up —
/// the agent is emphatically NOT working behind a modal — but the burst was
/// dropped on the seq check and never retried, so every Fable spawn wedged until
/// the ~3 min `switch_timeout` restart happened to re-classify it cleanly.
///
/// So on drift, ask the SCREEN. If the same dialog is still rendered, re-derive
/// the plan from that fresh context (option positions and cursor may have moved)
/// and deliver. Only prompts carrying a subtype are re-validated: those are the
/// screen-classified dialogs, for which the screen is the source of truth.
async fn ready_to_deliver(task: &mut AutoDismiss) -> bool {
    let current = task.store.driver.state_seq.load(std::sync::atomic::Ordering::Acquire);
    if current == task.expected_seq {
        return true;
    }

    let Some(ref classifier) = task.classifier else {
        return false;
    };
    let Some(subtype) = task.prompt.subtype.clone() else {
        return false;
    };
    let Some(fresh) = classify_current_screen(&task.store, classifier).await else {
        return false;
    };
    if fresh.kind != task.prompt.kind || fresh.subtype.as_deref() != Some(subtype.as_str()) {
        return false;
    }
    let Some((steps, groom_option)) = dismiss_plan(&task.store, &fresh) else {
        return false;
    };

    warn!(
        subtype = %subtype,
        expected_seq = task.expected_seq,
        current_seq = current,
        "state_seq advanced after the dialog was classified, but it is STILL on screen — \
         delivering the auto-dismiss burst anyway instead of wedging"
    );
    task.steps = steps;
    task.groom_option = groom_option;
    task.prompt = fresh;
    // Adopt the observed seq so the post-gate re-check compares against it.
    task.expected_seq = current;
    true
}

/// Deliver a keystroke burst to the PTY, stopping early if the screen already
/// moved on.
///
/// Between steps where a delay is present, check whether the screen already
/// transitioned (e.g. a number key auto-confirmed in a picker dialog) and skip
/// the remaining steps, to prevent keystrokes bleeding into the next screen.
async fn deliver(store: &Arc<Store>, steps: Vec<NudgeStep>) {
    let step_count = steps.len();
    for (i, step) in steps.into_iter().enumerate() {
        // Snapshot screen before steps with a delay when
        // more steps follow — we compare after the delay.
        let pre_lines = if step.delay_after.is_some() && i + 1 < step_count {
            store.terminal.screen.try_read().ok().map(|s| s.snapshot_text().lines.clone())
        } else {
            None
        };

        if store.channels.input_tx.send(InputEvent::Write(Bytes::from(step.bytes))).await.is_err() {
            break;
        }
        if let Some(delay) = step.delay_after {
            let (drain_tx, drain_rx) = tokio::sync::oneshot::channel();
            let _ = store.channels.input_tx.send(InputEvent::WaitForDrain(drain_tx)).await;
            let _ = drain_rx.await;
            tokio::time::sleep(delay).await;
        }

        // If the screen content changed after the delay,
        // the keystroke already took full effect — stop
        // before sending the next step (e.g. Enter).
        if let Some(pre) = pre_lines {
            if let Ok(screen) = store.terminal.screen.try_read() {
                if screen.snapshot_text().lines != pre {
                    break;
                }
            }
        }
    }
}

#[cfg(test)]
#[path = "groom_race_tests.rs"]
mod groom_race_tests;
