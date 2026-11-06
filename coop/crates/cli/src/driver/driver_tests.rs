// SPDX-License-Identifier: BUSL-1.1
// Copyright (c) 2026 Matt Baker <fimbaz@gmail.com>

use super::{
    disruption_option, fable_consent_confirm_index, fable_limit_safe_option,
    picker_navigate_and_confirm, PromptContext, PromptKind,
};

fn prompt(kind: PromptKind, subtype: Option<&str>) -> PromptContext {
    let ctx = PromptContext::new(kind).with_ready();
    match subtype {
        Some(s) => ctx.with_subtype(s),
        None => ctx,
    }
}

// -- Disruptions (should return Some(option)) --

#[test]
fn disruption_security_notes() {
    assert_eq!(disruption_option(&prompt(PromptKind::Setup, Some("security_notes"))), Some(1));
}

#[test]
fn disruption_login_success() {
    assert_eq!(disruption_option(&prompt(PromptKind::Setup, Some("login_success"))), Some(1));
}

#[test]
fn disruption_terminal_setup() {
    assert_eq!(disruption_option(&prompt(PromptKind::Setup, Some("terminal_setup"))), Some(1));
}

#[test]
fn disruption_theme_picker() {
    assert_eq!(disruption_option(&prompt(PromptKind::Setup, Some("theme_picker"))), Some(1));
}

#[test]
fn disruption_settings_error() {
    assert_eq!(disruption_option(&prompt(PromptKind::Setup, Some("settings_error"))), Some(2));
}

#[test]
fn disruption_trust() {
    assert_eq!(disruption_option(&prompt(PromptKind::Permission, Some("trust"))), Some(1));
}

#[test]
fn disruption_edit_protected() {
    assert_eq!(disruption_option(&prompt(PromptKind::Permission, Some("edit_protected"))), Some(2));
}

// -- Elicitations (should return None) --

#[test]
fn elicitation_oauth_login() {
    assert_eq!(disruption_option(&prompt(PromptKind::Setup, Some("oauth_login"))), None);
}

#[test]
fn elicitation_login_method() {
    assert_eq!(disruption_option(&prompt(PromptKind::Setup, Some("login_method"))), None);
}

#[test]
fn elicitation_startup_trust() {
    assert_eq!(disruption_option(&prompt(PromptKind::Setup, Some("startup_trust"))), None);
}

#[test]
fn elicitation_startup_bypass() {
    assert_eq!(disruption_option(&prompt(PromptKind::Setup, Some("startup_bypass"))), None);
}

#[test]
fn elicitation_startup_login() {
    assert_eq!(disruption_option(&prompt(PromptKind::Setup, Some("startup_login"))), None);
}

#[test]
fn elicitation_tool_permission() {
    assert_eq!(disruption_option(&prompt(PromptKind::Permission, Some("tool"))), None);
}

#[test]
fn elicitation_plan() {
    assert_eq!(disruption_option(&prompt(PromptKind::Plan, None)), None);
}

#[test]
fn elicitation_question() {
    assert_eq!(disruption_option(&prompt(PromptKind::Question, None)), None);
}

#[test]
fn elicitation_setup_no_subtype() {
    assert_eq!(disruption_option(&prompt(PromptKind::Setup, None)), None);
}

#[test]
fn elicitation_permission_no_subtype() {
    assert_eq!(disruption_option(&prompt(PromptKind::Permission, None)), None);
}

#[test]
fn fable_consent_uses_label_not_fixed_option() {
    // 2.1.257 (ka-0000006026): the consent picker is UNNUMBERED and the confirm
    // option moved off position 1, so a fixed digit selects nothing / the wrong
    // option. It must be answered by label (arrow-nav in groom), never here.
    assert_eq!(disruption_option(&prompt(PromptKind::Setup, Some("fable_consent"))), None);
}

#[test]
fn fable_credits_uses_esc_not_fixed_option() {
    // Confirming would enable overage billing or charge a card. It is declined
    // with Esc in groom (order-independent), never a fixed digit — the old '2'
    // selected nothing in the unnumbered picker (ka-0000006026).
    assert_eq!(disruption_option(&prompt(PromptKind::Setup, Some("fable_credits"))), None);
}

#[test]
fn fable_consent_confirm_index_matches_by_label() {
    // Located by label regardless of position or version suffix.
    let opts =
        vec!["Switch to Sonnet 5 and continue".to_owned(), "Continue with Fable 5.1".to_owned()];
    assert_eq!(fable_consent_confirm_index(&opts), Some(1));

    let opts2 = vec!["Continue with Fable 5".to_owned(), "No, keep my current model".to_owned()];
    assert_eq!(fable_consent_confirm_index(&opts2), Some(0));

    // No confirm label present (spend/credits variant) — returns None.
    let credits = vec!["Yes, buy usage credits".to_owned(), "Not now".to_owned()];
    assert_eq!(fable_consent_confirm_index(&credits), None);
    assert_eq!(fable_consent_confirm_index(&[]), None);
}

#[test]
fn picker_navigate_and_confirm_emits_arrows_then_enter() {
    // Down when the target is below the cursor.
    let down = picker_navigate_and_confirm(0, 1);
    assert_eq!(down.len(), 1, "one write so the loop can't strand the Enter");
    assert_eq!(down[0].bytes, b"\x1b[B\r");
    assert!(down[0].delay_after.is_none());

    // Up when the target is above the cursor.
    assert_eq!(picker_navigate_and_confirm(2, 0)[0].bytes, b"\x1b[A\x1b[A\r");

    // Already on the target — just Enter.
    assert_eq!(picker_navigate_and_confirm(1, 1)[0].bytes, b"\r");
}

#[test]
fn fable_limit_never_uses_fixed_option() {
    // The weekly-limit dialog is answered by label, not position — it must not
    // route through the fixed-option auto-confirm path (that would press a
    // number blind to whether it spends or upgrades).
    assert_eq!(disruption_option(&prompt(PromptKind::Setup, Some("fable_limit"))), None);
}

#[test]
fn fable_limit_safe_option_selects_only_stop_or_wait() {
    // Stop/wait options are safe (their position is returned, 1-based).
    assert_eq!(fable_limit_safe_option(&["Stop and wait for limit to reset".to_owned()]), Some(1));
    assert_eq!(fable_limit_safe_option(&["Wait for limit to reset".to_owned()]), Some(1));
    assert_eq!(fable_limit_safe_option(&["Stop".to_owned()]), Some(1));
    assert_eq!(fable_limit_safe_option(&["Don't continue automatically".to_owned()]), Some(1));
    // Curly-apostrophe rendering is matched too (matches on "continue automatically").
    assert_eq!(
        fable_limit_safe_option(&["Don\u{2019}t continue automatically".to_owned()]),
        Some(1)
    );

    // Spend/upgrade options are NEVER selected.
    assert_eq!(fable_limit_safe_option(&["Upgrade to Max".to_owned()]), None);
    assert_eq!(fable_limit_safe_option(&["Upgrade to Team plan".to_owned()]), None);
    assert_eq!(
        fable_limit_safe_option(&["Add funds to continue with usage credits".to_owned()]),
        None
    );
    assert_eq!(fable_limit_safe_option(&["Adjust monthly limit".to_owned()]), None);

    // Mixed set: the safe option's real position is returned, skipping the
    // upgrade (1) and the card-charging purchase (2).
    let opts = [
        "Upgrade to Max".to_owned(),
        "Add funds to continue with usage credits".to_owned(),
        "Stop and wait for limit to reset".to_owned(),
    ];
    assert_eq!(fable_limit_safe_option(&opts), Some(3));

    // No safe option present → nothing selected (caller falls back to Esc).
    let unsafe_only = ["Upgrade to Max".to_owned(), "Adjust monthly limit".to_owned()];
    assert_eq!(fable_limit_safe_option(&unsafe_only), None);
    assert_eq!(fable_limit_safe_option(&[]), None);
}
