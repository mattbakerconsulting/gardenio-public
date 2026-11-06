// SPDX-License-Identifier: BUSL-1.1
// Copyright (c) 2026 Matt Baker <fimbaz@gmail.com>

use std::future::Future;
use std::pin::Pin;
use std::sync::Arc;
use std::time::Duration;

use tokio::sync::mpsc;
use tokio_util::sync::CancellationToken;

use tracing::{debug, trace};

use crate::config::Config;
use crate::driver::{AgentState, Detector, DetectorEmission, PromptContext, PromptKind};
use crate::screen::ScreenTextSnapshot;

/// Tier 5 detector: classifies Claude's rendered terminal screen.
///
/// Detects the idle prompt (`❯`) on the last non-empty line, anti-matching
/// known startup prompts that should be handled separately.
pub struct ClaudeScreenDetector {
    snapshot_fn: Arc<dyn Fn() -> Arc<ScreenTextSnapshot> + Send + Sync>,
    poll: Duration,
}

impl ClaudeScreenDetector {
    pub fn new(
        config: &Config,
        snapshot_fn: Arc<dyn Fn() -> Arc<ScreenTextSnapshot> + Send + Sync>,
    ) -> Self {
        Self { snapshot_fn, poll: config.screen_poll() }
    }
}

impl Detector for ClaudeScreenDetector {
    fn run(
        self: Box<Self>,
        state_tx: mpsc::Sender<DetectorEmission>,
        shutdown: CancellationToken,
    ) -> Pin<Box<dyn Future<Output = ()> + Send>> {
        Box::pin(async move {
            let mut interval = tokio::time::interval(self.poll);
            let mut last_state: Option<AgentState> = None;

            loop {
                tokio::select! {
                    _ = shutdown.cancelled() => break,
                    _ = interval.tick() => {}
                }

                let snapshot = (self.snapshot_fn)();
                let non_empty: Vec<_> = snapshot
                    .lines
                    .iter()
                    .enumerate()
                    .filter(|(_, l)| !l.trim().is_empty())
                    .collect();
                trace!(
                    alt_screen = snapshot.alt_screen,
                    seq = snapshot.sequence,
                    non_empty_lines = non_empty.len(),
                    "screen poll"
                );
                let classified = classify_claude_screen(&snapshot);

                if let Some((ref state, ref cause)) = classified {
                    if last_state.as_ref() != Some(state) {
                        debug!(cause, state = state.as_str(), "screen detected");
                        let _ = state_tx.send((state.clone(), cause.clone(), None)).await;
                        last_state = Some(state.clone());
                    }
                } else if last_state.is_some() {
                    last_state = None;
                } else if !non_empty.is_empty() {
                    // Unclassified non-empty screen — log first few lines for diagnosis.
                    let preview: Vec<_> = non_empty
                        .iter()
                        .take(5)
                        .map(|(i, l)| format!("{i}:{}", l.trim()))
                        .collect();
                    debug!(lines = ?preview, "screen: unclassified");
                }
            }
        })
    }

    fn tier(&self) -> u8 {
        5
    }
}

/// Classification of an interactive dialog screen.
#[derive(Debug, Clone, Copy, PartialEq, Eq)]
enum DialogKind {
    /// Tool permission dialog — normally suppressed (handled by Tier 1 hooks).
    /// The bypass-immune `rm`-of-home/root circuit breaker shares this shape but
    /// leaks past the hooks; it is detected via [`screen_shows_rm_of_home_or_root`]
    /// and emitted as `Prompt(Permission)` with subtype `"rm_circuit_breaker"`.
    ToolPermission,
    /// Edit permission for protected directories (.claude/, .git/) — emitted as
    /// `Prompt(Permission)` with subtype `"edit_protected"` and auto-dismissed.
    /// This dialog bypasses Tier 1 hooks in Claude Code v2.1.78+.
    EditPermission,
    /// Workspace trust — emitted as `Prompt(Permission)` with subtype `"trust"`.
    Permission,
    /// Onboarding/setup dialog — emitted as `Prompt(Setup)` with a subtype string.
    Setup(&'static str),
    /// Fable usage-credit consent — emitted as `Prompt(Setup)` with the subtype
    /// [`fable_dialog_kind`] derives from the confirm label on screen.
    FableConsent,
    /// Fable weekly-included-usage limit dialog ("You've reached your Fable 5
    /// limit" / "What do you want to do?") — a *different* screen from the
    /// consent gate (ka-0000003007). Emitted as `Prompt(Setup)` with subtype
    /// `"fable_limit"`. It is answered toward the STOP/WAIT option — the one
    /// that neither spends nor upgrades — by scanning the on-screen labels
    /// ([`fable_limit_safe_option`]); its spend/upgrade options (e.g. "Add
    /// funds…", "Upgrade to Max") must NEVER be auto-pressed.
    FableLimit,
}

/// Classify Claude's screen, returning the state and a cause string.
///
/// Emits `Prompt(Setup)` for onboarding dialogs, `Prompt(Permission)` for
/// workspace trust, and `Idle` for the idle prompt. Tool
/// permission dialogs and startup text prompts are suppressed (`None`).
pub fn classify_claude_screen(snapshot: &ScreenTextSnapshot) -> Option<(AgentState, String)> {
    // Classify interactive dialogs first — they take priority over the
    // simple startup text prompts which match more broadly.
    match classify_interactive_dialog(&snapshot.lines) {
        Some(DialogKind::ToolPermission) => {
            // Normal tool-permission dialogs are surfaced (and auto-approved
            // under `--dangerously-skip-permissions`) via the Tier-1 hook path,
            // so the screen classifier suppresses them to avoid double-handling.
            //
            // The one exception is Claude Code's bypass-immune *deletion circuit
            // breaker*: an `rm`/`rmdir` targeting the filesystem root or the home
            // directory still prompts even in bypassPermissions mode, and — like
            // the protected-dir edit prompt (`EditPermission`) — it leaks past the
            // hooks straight to the screen. Left unhandled it stalls the session.
            // Detect that specific case and emit a Permission prompt so groom can
            // decline it (answer "No"); a model bug (e.g. `rm -rf $EMPTYVAR` ->
            // `rm -rf /`) can then never wipe the pod home. See ka-0000003153.
            if screen_shows_rm_of_home_or_root(&snapshot.lines) {
                let options = parse_options_from_screen(&snapshot.lines);
                return Some((
                    AgentState::Prompt {
                        prompt: PromptContext::new(PromptKind::Permission)
                            .with_subtype("rm_circuit_breaker")
                            .with_options(options)
                            .with_ready(),
                    },
                    "screen:permission".to_owned(),
                ));
            }
            return None;
        }
        Some(DialogKind::EditPermission) => {
            let options = parse_options_from_screen(&snapshot.lines);
            return Some((
                AgentState::Prompt {
                    prompt: PromptContext::new(PromptKind::Permission)
                        .with_subtype("edit_protected")
                        .with_options(options)
                        .with_ready(),
                },
                "screen:permission".to_owned(),
            ));
        }
        Some(DialogKind::Permission) => {
            let options = parse_options_from_screen(&snapshot.lines);
            return Some((
                AgentState::Prompt {
                    prompt: PromptContext::new(PromptKind::Permission)
                        .with_subtype("trust")
                        .with_options(options)
                        .with_ready(),
                },
                "screen:permission".to_owned(),
            ));
        }
        Some(DialogKind::FableConsent) => {
            // 2.1.257 (ka-0000006026) turned this picker UNNUMBERED, so the
            // `N. `-prefixed parser returns nothing for it. Use the picker parser
            // that handles both numbered and unnumbered options and reports the
            // cursor line, so groom can answer by LABEL (arrow-nav) rather than a
            // digit that selects nothing.
            let (options, selected) = parse_picker(&snapshot.lines);
            return Some((
                AgentState::Prompt {
                    prompt: PromptContext::new(PromptKind::Setup)
                        .with_subtype(fable_dialog_kind(&snapshot.lines))
                        .with_options(options)
                        .with_selected(selected)
                        .with_ready(),
                },
                "screen:setup".to_owned(),
            ));
        }
        Some(DialogKind::FableLimit) => {
            // Classify + carry the on-screen options so the policy layer can
            // pick the safe stop/wait option by label (or fall back to Esc).
            // The subtype is distinct from the consent subtypes precisely so
            // this never routes through the fixed-option auto-confirm path.
            let options = parse_options_from_screen(&snapshot.lines);
            return Some((
                AgentState::Prompt {
                    prompt: PromptContext::new(PromptKind::Setup)
                        .with_subtype("fable_limit")
                        .with_options(options)
                        .with_ready(),
                },
                "screen:setup".to_owned(),
            ));
        }
        Some(DialogKind::Setup(subtype)) => {
            let options = parse_options_from_screen(&snapshot.lines);
            let auth_url =
                if subtype == "oauth_login" { extract_auth_url(&snapshot.lines) } else { None };
            let mut ctx = PromptContext::new(PromptKind::Setup)
                .with_subtype(subtype)
                .with_options(options)
                .with_ready();
            ctx.input = auth_url;
            return Some((AgentState::Prompt { prompt: ctx }, "screen:setup".to_owned()));
        }
        None => {}
    }

    // Detect Claude's thinking indicator: `… thinking` or `… thought for 9s`.
    // The ellipsis (U+2026) appears on a line while the model is actively
    // thinking, optionally followed by timing/token metadata.
    for line in &snapshot.lines {
        let trimmed = line.trim();
        if trimmed.starts_with('\u{2026}')
            && (trimmed.contains("thinking") || trimmed.contains("thought for"))
        {
            return Some((AgentState::Working, "screen:thinking".to_owned()));
        }
    }

    // Look for Claude's idle prompt indicator anywhere in the visible lines.
    // Claude Code renders `❯` (U+276F) at the start of its input line.
    // Status text like "ctrl+t to hide tasks" may appear below the prompt,
    // so we scan all non-empty lines rather than only the last.
    //
    // IMPORTANT: this check runs BEFORE startup prompt detection because the
    // startup detector uses broad substring matches (e.g. "bypass permissions")
    // that false-positive on status-bar text like "⏵⏵ bypass permissions on".
    // Once the idle prompt is visible the agent is past startup.
    //
    // CAVEAT: Claude's TUI also uses `❯` to mark the *selected* option in
    // interactive dialogs (e.g. `❯ 1. No, exit`). We distinguish the idle
    // prompt from dialog option markers by checking for a digit after the
    // leading `❯ `.  The idle prompt is `❯ ` (space then free text or empty);
    // a dialog option is `❯ N.` (space, digit, dot).
    for line in snapshot.lines.iter().rev() {
        let trimmed = line.trim();
        if !trimmed.is_empty() && trimmed.starts_with('\u{276f}') {
            // Skip dialog option markers like "❯ 1. No, exit"
            let after = trimmed['\u{276f}'.len_utf8()..].trim_start();
            let is_dialog_option = after.as_bytes().first().is_some_and(|b| b.is_ascii_digit());
            if !is_dialog_option {
                return Some((AgentState::Idle, "screen:idle".to_owned()));
            }
        }
    }

    // Emit text-based startup prompts as Prompt(Setup) for API visibility.
    // These are NOT auto-dismissed (no reliable keystroke encoding for text
    // prompts). Checked after dialog classification because the startup
    // detector matches broadly (e.g. "trust this folder" appears in both the
    // simple y/n prompt and the interactive Accessing workspace dialog).
    if let Some(startup) = detect_startup_prompt(&snapshot.lines) {
        let subtype = match startup {
            StartupPrompt::WorkspaceTrust => "startup_trust",
            StartupPrompt::BypassPermissions => "startup_bypass",
            StartupPrompt::LoginRequired => "startup_login",
        };
        return Some((
            AgentState::Prompt {
                prompt: PromptContext::new(PromptKind::Setup).with_subtype(subtype).with_ready(),
            },
            "screen:setup".to_owned(),
        ));
    }

    None
}

/// Signal phrases for a dialog screen, paired with its classification.
/// Each screen defines 2-3 signal phrases; a match requires 2+ signals.
/// Signals are `(phrase, case_insensitive)`.
type DialogScreen = (DialogKind, &'static [(&'static str, bool)]);

const DIALOG_SCREENS: &[DialogScreen] = &[
    // Security notes
    (
        DialogKind::Setup("security_notes"),
        &[
            ("Security notes:", false),
            ("Claude can make mistakes", false),
            ("Press Enter to continue", false),
        ],
    ),
    // Login success
    (
        DialogKind::Setup("login_success"),
        &[("Login successful", false), ("Logged in as", false), ("Press Enter to continue", false)],
    ),
    // OAuth login
    (
        DialogKind::Setup("oauth_login"),
        &[("Paste code here if prompted", false), ("oauth/authorize", false)],
    ),
    // Login method picker
    (
        DialogKind::Setup("login_method"),
        &[
            ("Select login method:", false),
            ("Claude account with subscription", false),
            ("Anthropic Console account", false),
        ],
    ),
    // Workspace trust
    (
        DialogKind::Permission,
        &[
            ("Accessing workspace:", false),
            ("Yes, I trust this folder", false),
            ("enter to confirm", true),
        ],
    ),
    // Terminal setup
    (
        DialogKind::Setup("terminal_setup"),
        &[
            ("Use Claude Code's terminal setup?", false),
            ("Yes, use recommended settings", false),
            ("enter to confirm", true),
        ],
    ),
    // Theme picker
    (
        DialogKind::Setup("theme_picker"),
        &[("Choose the text style", false), ("Dark mode", false), ("enter to confirm", true)],
    ),
    // Settings error
    (
        DialogKind::Setup("settings_error"),
        &[
            ("Settings Error", false),
            ("Continue without these settings", false),
            ("Exit and fix manually", false),
        ],
    ),
    // Bypass permissions dialog (--dangerously-skip-permissions)
    (
        DialogKind::Setup("bypass_permissions"),
        &[
            ("Bypass Permissions mode", false),
            ("you accept all responsibility", false),
            ("enter to confirm", true),
        ],
    ),
    // Fable usage-credit consent — a one-time gate Claude Code raises the first
    // time a session runs on Fable 5 (ka-0000001044). Unhandled it wedges every
    // fable-pinned agent at startup, because the harness persists consent
    // per-org in `~/.claude.json` (`fableOverageConsentV2`) keyed by an OAuth
    // account UUID that is unknowable at image-build time — so it cannot be
    // preseeded, and the screen is the only place to answer it.
    //
    // Signals are drawn only from the dialog's TITLE and its OPTION LABELS.
    // Both are short enough to always render on one line, whereas the body
    // copy ("Fable 5 runs on usage credits…") hard-wraps at the terminal width
    // and would defeat a per-line `contains` match.
    //
    // Every variant renders exactly one title and one confirm label, and the
    // two sets are disjoint — so every variant lands exactly on threshold. The
    // confirm label is also what separates a free consent from a purchase; see
    // [`fable_dialog_kind`].
    //
    // The confirm label has FOUR branches, not three (ka-0000001616). Reading
    // the shipped harness (2.1.236) it is:
    //
    //   credits usable                  -> "Continue with Fable 5"
    //   credits off, balance remains    -> "Yes, re-enable and continue"
    //   no credits, self-serve purchase -> "Yes, buy usage credits" / "Buy usage credits"
    //   otherwise (web- or admin-managed credits, no in-CLI purchase):
    //       "Set up usage credits on claude.ai" / "Manage usage credits on claude.ai"
    //       "Request usage credits from your admin" / "Request more from your admin"
    //
    // That last branch is what an org-managed account gets, and its labels were
    // missing here — so those screens reached only their title, one hit, and
    // fell below threshold: unclassified, unanswered, wedged. The decline label
    // "No, keep my current model" is listed too, so a future confirm-label swap
    // on the picker variant still lands on threshold.
    //
    // "Buy usage credits" is the one signal not unique to this dialog: it is
    // also the title of the standalone purchase flow, a multi-step
    // card-charging dialog coop must never answer. That screen never names
    // Fable, so it can only ever reach one hit and stays below threshold. The
    // same holds for the admin-request confirmation ("Request usage credits
    // from your admin" as a *title*, options "Send request" / "Cancel") — it
    // too names no Fable title and no second label, so it stays unanswered.
    (
        DialogKind::FableConsent,
        &[
            // Titles.
            ("Switch to Fable 5?", false),
            ("Fable 5 limit", false),
            // Model-version-agnostic: matches "Fable 5 now uses usage credits"
            // AND "Fable 5.1 now uses usage credits" (and any future minor).
            // Pinning the literal "Fable 5 …" left a 5.1-titled screen one hit
            // short of threshold, so it wedged unclassified (ka-0000001501). The
            // phrase is unique to this consent gate's title — the standalone
            // purchase flow is titled "Buy usage credits", never "… now uses …".
            ("now uses usage credits", false),
            // Confirm labels — credits usable, or a self-serve purchase.
            ("Continue with Fable 5", false),
            ("Yes, re-enable and continue", false),
            ("Yes, buy usage credits", false),
            ("Buy usage credits", false),
            // Confirm labels — credits managed on the web or by an org admin.
            ("Set up usage credits on claude.ai", false),
            ("Manage usage credits on claude.ai", false),
            ("Request usage credits from your admin", false),
            ("Request more from your admin", false),
            // Decline label (picker + credits-off entry).
            ("No, keep my current model", false),
        ],
    ),
    // Fable weekly-included-usage limit dialog (ka-0000003007). NOT the consent
    // gate: it fires when a fable-pinned session exhausts its *weekly included*
    // Fable usage mid-run, and asks "What do you want to do?" over a variable
    // option set — some safe (Stop / Wait for limit to reset / Don't continue
    // automatically), some emphatically not (Add funds…, Upgrade to Max/Team,
    // Adjust monthly limit). Option 2 in particular walks into the card-charging
    // purchase flow and must never be auto-pressed.
    //
    // Two signals pin it, and both must be present to classify: the question
    // "What do you want to do?" (which the consent gate never asks) and the
    // "Fable 5 limit" title. The consent *limit* variant shares that title, but
    // it is checked FIRST (above) and reaches threshold on its own confirm label
    // ("Continue with Fable 5", …) before this entry is ever considered — so a
    // consent screen classifies as FableConsent, never as FableLimit. This
    // entry only wins when the question is present, i.e. the true limit dialog.
    //
    // Classification is deliberately decoupled from the answer: which key is
    // pressed is decided later by scanning the parsed option LABELS for a safe
    // one ([`fable_limit_safe_option`]) — never a fixed option number — so a
    // spend/upgrade label can never be selected by position.
    (DialogKind::FableLimit, &[("What do you want to do?", false), ("Fable 5 limit", false)]),
    // Edit permission — protected directory write (.claude/, .git/)
    // Claude Code v2.1.78+ prompts for these even in bypassPermissions mode.
    // Must appear before ToolPermission to avoid mis-classification.
    (
        DialogKind::EditPermission,
        &[
            ("Do you want to make this edit", false),
            ("allow all edits during this session", true),
            ("Esc to cancel", false),
        ],
    ),
    // Tool permission
    (
        DialogKind::ToolPermission,
        &[
            ("Do you want to proceed?", false),
            ("Yes, and don't ask again", false),
            ("Esc to cancel", false),
        ],
    ),
];

/// Minimum number of signals that must match to identify a dialog screen.
const DIALOG_SIGNAL_THRESHOLD: usize = 2;

/// Distinguish a free Fable consent from one that authorizes spending.
///
/// The dialog always renders two options — confirm (1) then decline (2) — but
/// the confirm label differs by what confirming actually *does*:
///
/// - `"Continue with Fable 5"` — credits are already provisioned and enabled;
///   confirming only records the one-time consent. Costs nothing extra, so
///   `"fable_consent"` is auto-confirmed.
/// - `"Yes, re-enable and continue"` / `"…buy usage credits"` — confirming turns
///   overage billing on or charges a card. That is a spending decision no agent
///   should make on an operator's behalf, so `"fable_credits"` is auto-*declined*
///   instead: option 2 keeps the session alive on another model rather than
///   wedging it, and an operator can enable credits out of band.
/// - `"…on claude.ai"` / `"…from your admin"` — credits are managed off-CLI.
///   Confirming spends nothing, but it only opens a URL or files a request with
///   the org's admins and then dismisses *without* making Fable usable, so the
///   agent gains nothing and has spoken to a human on the operator's behalf.
///   These are `"fable_credits"` too, and declining still keeps the session
///   alive on another model.
fn fable_dialog_kind(lines: &[String]) -> &'static str {
    let free_consent = lines.iter().any(|line| line.contains("Continue with Fable 5"));
    if free_consent {
        "fable_consent"
    } else {
        "fable_credits"
    }
}

/// Classify the screen as an interactive dialog, returning the dialog kind
/// if recognized, or `None` if no dialog is detected.
///
/// Each known dialog screen defines 2-3 signal phrases; a match requires
/// at least [`DIALOG_SIGNAL_THRESHOLD`] signals present on screen.
fn classify_interactive_dialog(lines: &[String]) -> Option<DialogKind> {
    for (kind, signals) in DIALOG_SCREENS {
        let mut hits = 0;
        for &(phrase, ci) in *signals {
            let found = lines.iter().any(|line| {
                let trimmed = line.trim();
                if ci {
                    trimmed.to_lowercase().contains(phrase)
                } else {
                    trimmed.contains(phrase)
                }
            });
            if found {
                hits += 1;
                if hits >= DIALOG_SIGNAL_THRESHOLD {
                    return Some(*kind);
                }
            }
        }
    }
    None
}

/// Extract an OAuth authorization URL from screen lines.
///
/// Looks for `https://<any-domain>/oauth/authorize?` and may span
/// multiple terminal lines.  Continuation lines start at column 0
/// (no leading whitespace) while surrounding UI text is indented.
fn extract_auth_url(lines: &[String]) -> Option<String> {
    let suffix = "/oauth/authorize?";
    let start_idx = lines.iter().position(|line| {
        let t = line.trim_start();
        t.starts_with("https://") && t.contains(suffix)
    })?;

    let mut url = lines[start_idx].trim().to_string();

    // Concatenate hard-wrapped continuation lines.
    for line in &lines[start_idx + 1..] {
        let trimmed = line.trim_end();
        if trimmed.is_empty() || trimmed.starts_with(' ') {
            break;
        }
        url.push_str(trimmed);
    }

    Some(url)
}

/// Known startup prompts that Claude Code may present.
#[derive(Debug, Clone, Copy, PartialEq, Eq)]
pub enum StartupPrompt {
    /// "Do you trust the files in this folder?"
    WorkspaceTrust,
    /// "Allow tool use without prompting?" / --dangerously-skip-permissions
    BypassPermissions,
    /// "Please sign in" / login / onboarding flow
    LoginRequired,
}

/// Classify a screen snapshot as a startup prompt.
///
/// Scans the last non-empty lines of the screen for known prompt patterns.
pub fn detect_startup_prompt(screen_lines: &[String]) -> Option<StartupPrompt> {
    // Work backwards through lines to find the last non-empty content.
    let trimmed: Vec<&str> =
        screen_lines.iter().map(|l| l.trim()).filter(|l| !l.is_empty()).collect();

    if trimmed.is_empty() {
        return None;
    }

    // Check the last few lines for known patterns.
    let tail = if trimmed.len() >= 5 { &trimmed[trimmed.len() - 5..] } else { &trimmed };
    let combined = tail.join(" ");
    let lower = combined.to_lowercase();

    // Workspace trust prompt
    if lower.contains("trust the files")
        || lower.contains("do you trust")
        || lower.contains("trust this folder")
        || lower.contains("trust this workspace")
    {
        return Some(StartupPrompt::WorkspaceTrust);
    }

    // Permission bypass prompt
    if lower.contains("skip permissions")
        || lower.contains("dangerously-skip-permissions")
        || lower.contains("allow tool use without prompting")
        || lower.contains("bypass permissions")
    {
        return Some(StartupPrompt::BypassPermissions);
    }

    // Login / onboarding prompt
    if lower.contains("please sign in")
        || lower.contains("please log in")
        || lower.contains("login required")
        || lower.contains("sign in to continue")
        || lower.contains("authenticate")
        || lower.contains("not logged in")
        || lower.contains("please run /login")
    {
        return Some(StartupPrompt::LoginRequired);
    }

    None
}

/// Parse numbered option labels from terminal screen lines.
///
/// Scans lines bottom-up looking for patterns like `❯ 1. Yes` or `  2. Don't ask again`.
/// Handles Claude's real TUI format:
/// - Selected option: `❯ 1. Label`
/// - Unselected: `  2. Label`
/// - Description lines indented under options (skipped)
/// - Separator lines `────...` and footer hints (skipped)
///
/// Collects matches and stops at the first non-option, non-skippable line above
/// the block. Returns options in ascending order (option 1 first).
/// Returns true if the screen shows a destructive `rm`/`rmdir` command whose
/// target is the filesystem root or a home directory — the pattern that trips
/// Claude Code's bypass-immune deletion circuit breaker.
///
/// From the docs: "Removals targeting the filesystem root or home directory,
/// such as `rm -rf /` and `rm -rf ~`, still prompt as a circuit breaker against
/// model error." The check is deliberately narrow — only the home/root directory
/// *itself*, never a subpath under it (e.g. `rm -rf /home/agent/build` is left
/// suppressed) — so it never mis-handles an ordinary tool-permission dialog.
fn screen_shows_rm_of_home_or_root(lines: &[String]) -> bool {
    for line in lines {
        let tokens: Vec<&str> = line.split_whitespace().collect();
        // Only inspect lines that invoke rm/rmdir (the command-preview line).
        if !tokens.iter().any(|&w| w == "rm" || w == "rmdir") {
            continue;
        }
        for tok in tokens {
            // Strip surrounding quotes an agent may have added (e.g. "$HOME").
            let tok = tok.trim_matches(|c| c == '"' || c == '\'');
            if is_home_or_root_target(tok) {
                return true;
            }
        }
    }
    false
}

/// True if `tok` names the filesystem root or a home directory *itself*.
fn is_home_or_root_target(tok: &str) -> bool {
    // Filesystem root, and home via shorthand / env var (with or without a
    // single trailing slash).
    if matches!(tok, "/" | "~" | "~/" | "$HOME" | "${HOME}" | "$HOME/" | "${HOME}/") {
        return true;
    }
    // For absolute paths, strip a single trailing slash before comparing.
    let path = tok.strip_suffix('/').unwrap_or(tok);
    if path == "/root" {
        return true;
    }
    // `/home/<user>` or `/Users/<user>` — the home directory itself, i.e. exactly
    // one non-empty path segment after the base (a deeper subpath does not trip
    // the circuit breaker and must stay suppressed).
    for base in ["/home/", "/Users/"] {
        if let Some(rest) = path.strip_prefix(base) {
            if !rest.is_empty() && !rest.contains('/') {
                return true;
            }
        }
    }
    false
}

pub fn parse_options_from_screen(lines: &[String]) -> Vec<String> {
    let mut options: Vec<(u32, String)> = Vec::new();
    let mut found_any = false;

    for line in lines.iter().rev() {
        let trimmed = line.trim();

        // Skip blank lines
        if trimmed.is_empty() {
            continue;
        }

        // Skip hint/footer lines (e.g. "Esc to cancel · Tab to amend")
        if is_hint_line(trimmed) {
            continue;
        }

        // Skip separator lines (e.g. "────────────")
        if is_separator_line(trimmed) {
            if found_any {
                // Separator above the options block can appear between groups
                // (e.g. question dialog splits options 1-3 from option 4)
                continue;
            }
            continue;
        }

        // Try to parse as a numbered option
        if let Some((num, label)) = parse_numbered_option(trimmed) {
            options.push((num, label));
            found_any = true;
        } else if found_any {
            // Non-option, non-skippable line. Could be a description line
            // indented under a previous option, or the end of the block.
            // Description lines are deeply indented (5+ spaces) with no
            // leading digit — skip those.
            if is_description_line(line) {
                continue;
            }
            // Otherwise we've hit content above the options block — stop.
            break;
        }
    }

    // Sort by option number ascending and return just the labels
    options.sort_by_key(|(num, _)| *num);
    options.into_iter().map(|(_, label)| label).collect()
}

/// Parse an arrow-select picker's option block, returning the option labels
/// (top-to-bottom) and the 0-based index of the cursor line (`❯`), if any.
///
/// Unlike [`parse_options_from_screen`], which requires an `N. ` prefix, this
/// handles the UNNUMBERED picker Claude Code 2.1.257 introduced for the Fable
/// consent gate (ka-0000006026) as well as the older numbered form. Because
/// unnumbered option lines are indistinguishable from body prose on their own,
/// the block is located structurally: it is the contiguous run of non-blank
/// lines immediately above the footer, bounded by the blank line Claude always
/// renders between the body copy and the options. A leading `❯` marks the
/// cursor; a leading `N. ` (numbered form) is stripped so labels are clean.
pub fn parse_picker(lines: &[String]) -> (Vec<String>, Option<usize>) {
    // Collect the option block bottom-up: skip trailing blank/footer lines,
    // then take the contiguous run of option lines until a blank/separator.
    let mut block: Vec<(bool, String)> = Vec::new();
    let mut started = false;
    for line in lines.iter().rev() {
        let trimmed = line.trim();

        if trimmed.is_empty() {
            if started {
                break; // blank line above the options ends the block
            }
            continue; // trailing blanks below/around the footer
        }
        if is_hint_line(trimmed) {
            if started {
                break;
            }
            continue; // footer, e.g. "Enter to confirm · Esc to cancel"
        }
        if is_separator_line(trimmed) {
            break; // box-drawing rule = top boundary
        }

        let is_cursor = trimmed.starts_with('\u{276f}');
        block.push((is_cursor, normalize_picker_label(trimmed)));
        started = true;
    }

    block.reverse();
    let selected = block.iter().position(|(cursor, _)| *cursor);
    let options = block.into_iter().map(|(_, label)| label).collect();
    (options, selected)
}

/// Normalize a picker option line to its bare label: strip a leading cursor
/// (`❯`) and whitespace, an optional `N. ` numbered prefix, and any trailing
/// active-selection marker (`✔`/`✓`).
fn normalize_picker_label(trimmed: &str) -> String {
    let s = trimmed.strip_prefix('\u{276f}').unwrap_or(trimmed);
    let s = s.trim_start_matches([' ', '\u{00A0}']);

    // Strip an optional "N. " numbered prefix (older format).
    let s = match s.find(|c: char| !c.is_ascii_digit()) {
        Some(digit_end) if digit_end > 0 => s[digit_end..].strip_prefix(". ").unwrap_or(s),
        _ => s,
    };

    s.trim_end().trim_end_matches(['✔', '✓']).trim_end().to_string()
}

/// Extract plan prompt context from the terminal screen.
///
/// Plan prompts are detected via the screen rather than the session log,
/// so context is built from the visible screen lines.
pub fn extract_plan_context(_screen: &ScreenTextSnapshot) -> PromptContext {
    PromptContext::new(PromptKind::Plan)
}

/// Try to parse a line as a numbered option: `[❯ ] N. label`.
///
/// Strips leading selection indicator (`❯`) and whitespace before matching.
/// The `❯` may be followed by a regular space or a non-breaking space (U+00A0).
/// Returns `(number, label)` if the line matches.
fn parse_numbered_option(trimmed: &str) -> Option<(u32, String)> {
    // Strip the selection indicator (❯) if present, then any mix of
    // regular spaces and non-breaking spaces (U+00A0).
    let s = trimmed.strip_prefix('❯').unwrap_or(trimmed);
    let s = s.trim_start_matches([' ', '\u{00A0}']);

    // Must start with one or more digits
    let digit_end = s.find(|c: char| !c.is_ascii_digit())?;
    if digit_end == 0 {
        return None;
    }

    let num: u32 = s[..digit_end].parse().ok()?;

    // Must be followed by ". "
    let rest = s[digit_end..].strip_prefix(". ")?;

    // Label must be non-empty
    if rest.is_empty() {
        return None;
    }

    // Strip trailing selection indicators (e.g. " ✔" or " ✓") that Claude
    // renders after the currently-active option in picker dialogs.
    let label = rest.trim_end().trim_end_matches(['✔', '✓']).trim_end().to_string();

    if label.is_empty() {
        return None;
    }

    Some((num, label))
}

/// Separator lines are composed entirely of box-drawing characters.
fn is_separator_line(trimmed: &str) -> bool {
    !trimmed.is_empty() && trimmed.chars().all(|c| matches!(c, '─' | '╌' | '━' | '═' | '│' | '┃'))
}

/// Hint/footer lines contain navigation instructions.
fn is_hint_line(trimmed: &str) -> bool {
    // Common Claude TUI footer patterns
    trimmed.contains("Esc to cancel")
        || trimmed.contains("Enter to select")
        || trimmed.contains("Enter to confirm")
        || trimmed.contains("Tab to amend")
        || trimmed.contains("Arrow keys to navigate")
}

/// Description lines are indented continuation text under a numbered option.
/// They start with 5+ spaces (deeper than option indentation) and don't begin
/// with a digit (ruling out numbered options themselves).
fn is_description_line(raw_line: &str) -> bool {
    let leading = raw_line.len() - raw_line.trim_start().len();
    if leading < 5 {
        return false;
    }
    let first_non_space = raw_line.trim_start().chars().next();
    !matches!(first_non_space, Some('0'..='9') | Some('❯') | None)
}

#[cfg(test)]
#[path = "screen_tests.rs"]
mod tests;

#[cfg(test)]
#[path = "startup_tests.rs"]
mod startup_tests;
