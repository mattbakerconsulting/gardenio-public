// SPDX-License-Identifier: BUSL-1.1
// Copyright (c) 2026 Matt Baker <fimbaz@gmail.com>

use serde::{Deserialize, Serialize};

/// Categorized error type for agent error states.
#[derive(Debug, Clone, Copy, PartialEq, Eq, Serialize, Deserialize)]
#[serde(rename_all = "snake_case")]
pub enum ErrorCategory {
    Unauthorized,
    OutOfCredits,
    BudgetExceeded,
    RateLimited,
    ModelRateLimited,
    NoInternet,
    ServerError,
    Other,
}

impl ErrorCategory {
    /// Returns `true` for error categories that indicate the account's
    /// credentials are exhausted and auth failover should be triggered.
    ///
    /// `ModelRateLimited` is deliberately **excluded**: it is a MODEL-group
    /// (pool-exhaustion) 429 carrying a no-fallback / no-deployments marker,
    /// where every pooled profile serves the SAME throttled model on the SAME
    /// upstream. Rotating credentials cannot clear it, so it must NOT trip
    /// `handle_rate_limit` → `try_auto_rotate` → the exit-10 credential-switch
    /// restart. Doing so cold-restarts the pod every ~277s (a fresh session
    /// each cycle, `resumed=False`) and never recovers (ka-0000001718, residual
    /// of P0 ka-0000003464). Instead the controller parks in place (dismiss +
    /// backoff + re-nudge, same session) — see gb init's `model_rate_limited`
    /// branch in monitorExit.
    pub fn is_auth_exhaustion(&self) -> bool {
        matches!(self, Self::BudgetExceeded | Self::RateLimited)
    }

    /// Returns `true` for **terminal, non-failover** credential categories: an
    /// error where the agent's key/account cannot serve the request AND
    /// rotating credentials cannot fix it, so no auth-failover event fires and
    /// the session is marked a zombie (see `transition.rs`).
    ///
    /// These are exactly the categories that produce NO NATS event today:
    /// [`maybe_publish_auth_exhausted`] maps only [`Self::BudgetExceeded`] /
    /// [`Self::RateLimited`] (the failover categories) to an event and drops
    /// everything else. When an agent's FIRST inference turn fails with one of
    /// these — e.g. a `401 key not allowed to access model` (`Unauthorized`) or
    /// an out-of-credits billing error (`OutOfCredits`) — nothing is ever
    /// written to the agent bead: the real error string lives only in a live
    /// poll of `/api/v1/agent`, which is gone the moment the pod restarts. The
    /// 2026-07-25 opus-5 outage ran ~4.5h fleet-wide with `total_errors=0` on
    /// every affected bead for exactly this reason (ka-0000003962).
    ///
    /// A DIAGNOSTIC-only event is published on a distinct subject for these
    /// categories so the controller can persist `error_category` +
    /// `error_detail` onto the agent bead. This must NOT route into
    /// `is_auth_exhaustion` / failover: adding `Unauthorized` there would drive
    /// `handle_rate_limit` → `try_auto_rotate` → the exit-10 credential-switch
    /// restart, which cannot fix a key ACL and would cold-restart the pod
    /// forever (see the `is_auth_exhaustion` note above and ka-0000001718).
    pub fn is_terminal_auth_failure(&self) -> bool {
        matches!(self, Self::Unauthorized | Self::OutOfCredits)
    }

    /// Wire-format string for this category.
    pub fn as_str(&self) -> &'static str {
        match self {
            Self::Unauthorized => "unauthorized",
            Self::OutOfCredits => "out_of_credits",
            Self::BudgetExceeded => "budget_exceeded",
            Self::RateLimited => "rate_limited",
            Self::ModelRateLimited => "model_rate_limited",
            Self::NoInternet => "no_internet",
            Self::ServerError => "server_error",
            Self::Other => "other",
        }
    }
}

impl std::fmt::Display for ErrorCategory {
    fn fmt(&self, f: &mut std::fmt::Formatter<'_>) -> std::fmt::Result {
        f.write_str(self.as_str())
    }
}

/// Classify an error detail string into an [`ErrorCategory`].
///
/// Uses case-insensitive substring matching against known Claude API error
/// strings and common screen-scraped patterns.
pub fn classify_error_detail(detail: &str) -> ErrorCategory {
    let lower = detail.to_lowercase();

    // Codex tags its typed error class up front (`[codex:usageLimitExceeded] ...`);
    // that is authoritative and must not be second-guessed by prose in the message.
    if let Some(category) = codex_tag_category(&lower) {
        return category;
    }
    // Gemini has no typed classes; `driver::gemini::parse::tag_message` derives
    // one from the message and prefixes it the same way.
    if let Some(category) = gemini_tag_category(&lower) {
        return category;
    }

    // Unauthorized / authentication errors
    if lower.contains("authentication_error")
        || lower.contains("invalid api key")
        || lower.contains("invalid_api_key")
        || lower.contains("permission_error")
        || lower.contains("authentication_failed")
    {
        return ErrorCategory::Unauthorized;
    }

    // Budget exceeded (provider-side budget limit, e.g. LiteLLM proxy)
    if lower.contains("budget_exceeded")
        || lower.contains("budget has been exceeded")
        || lower.contains("budget exceeded")
    {
        return ErrorCategory::BudgetExceeded;
    }

    // Out of credits / billing errors
    if lower.contains("billing")
        || lower.contains("insufficient_credits")
        || lower.contains("insufficient credits")
        || lower.contains("out of credits")
        || lower.contains("credit")
        || lower.contains("payment_required")
    {
        return ErrorCategory::OutOfCredits;
    }

    // Model-group rate limiting (pool exhaustion) — MUST be checked BEFORE the
    // account-level RateLimited block below.
    //
    // A 429 that carries a NO-FALLBACK / NO-DEPLOYMENTS model-group marker means
    // the requested model group has no upstream deployment available AND no
    // fallback group to spill onto, e.g.
    //   "… rate_limit_error … This request would exceed your account's monthly
    //    spend limit … Received Model Group=claude-opus-4-8-oauth
    //    Available Model Group Fallbacks=None"
    //   "… No deployments available for selected model …"
    // Every pooled credential PROFILE for such an account serves the SAME model
    // on the SAME upstream throttle, so rotating credentials CANNOT clear it.
    // Classifying this as RateLimited (below) trips is_auth_exhaustion() →
    // handle_rate_limit() → try_auto_rotate() → a forced Switch → coop exit-10 →
    // gb init mints a fresh session bead and cold-restarts. The fresh coop
    // re-registers all profiles Available and its switch_history starts empty,
    // so BOTH in-memory anti-flap guards reset — it immediately 429s on the same
    // model and rotates again, looping every ~277s (=COOP_ROTATE_COOLDOWN_SECS
    // minus teardown), resumed=False each cycle, never progressing
    // (ka-0000001718, residual of P0 ka-0000003464).
    //
    // ModelRateLimited is NOT is_auth_exhaustion (see the impl above), so
    // handle_rate_limit / try_auto_rotate never fire and the session is
    // preserved. The controller's gb init `model_rate_limited` branch parks in
    // place (dismiss + backoff + re-nudge, same session, liveness probe stays
    // up) exactly as the account-level rate-limit branch does, but without any
    // profile rotation / auth failover. Operator intent (gardenio/docs/
    // litellm-hardening-ha-plan.md, Matt 2026-06-16): a 429 on a model group
    // with "Fallbacks=None" SHOULD PARK, not rotate — cross-role Bedrock
    // spillover is forbidden (policy ka-0000002660).
    //
    // CRITICAL — the discriminator is the NO-FALLBACK marker, not "account" /
    // "spend" prose. The canonical production incident string (the
    // monthly_spend_limit_wrapped fixture) contains BOTH the account-spend prose
    // AND "Available Model Group Fallbacks=None", so a "spend-wins → keep
    // rotating" split would MISFIRE on the exact P0 string. Only the
    // no-fallback / no-deployments model-group markers are matched here; a bare
    // account 429 WITHOUT them falls through to RateLimited below (legitimate
    // single-account failover preserved).
    if lower.contains("available model group fallbacks=none")
        || lower.contains("no deployments available")
        || lower.contains("deployments available for selected model")
    {
        return ErrorCategory::ModelRateLimited;
    }

    // Rate limiting
    //
    // Includes LiteLLM proxy cooldown messages emitted by the Claude Code CLI
    // when all upstream deployments are temporarily unavailable, e.g.
    //   "Server is temporarily limiting requests (not your usage limit) ·
    //    No deployments available for selected model, Try again in 5 seconds."
    //   NOTE: the no-deployments/no-fallback shapes are now routed to
    //   ModelRateLimited by the block ABOVE (they can never be helped by
    //   rotation); only the account-level shapes below reach RateLimited.
    //
    // Also includes per-account monthly spend-limit rate_limit_errors, e.g.
    //   "API Error: Server is temporarily limiting requests (not your usage
    //    limit) · {"type":"error","error":{"type":"rate_limit_error",
    //    "message":"This request would exceed your account's monthly spend
    //    limit. Please try again later."}}"
    // The full wrapped form already matches via "rate_limit_error" /
    // "temporarily limiting", but Claude Code sometimes surfaces only the inner
    // human-readable message ("This request would exceed your account's monthly
    // spend limit. Please try again later.") with no type field. The
    // "spend limit" / "monthly spend" patterns catch that bare form. This is an
    // account-level cap, so it is recoverable by failing over to another account
    // — exactly what handle_rate_limit() does (profile rotation), unlike a
    // BudgetExceeded which marks the session a zombie (ka-0000007363).
    //
    // NOTE: transient API-wrapped *connection* bounces ("API Error: terminated",
    // "API Error: The socket connection was closed unexpectedly", "API Error:
    // Connection error.") were previously matched HERE and routed through
    // RateLimited. That was wrong (ka-0000006962): RateLimited fires
    // handle_rate_limit() → profile rotation → SIGHUP/full Claude restart and
    // trips is_auth_exhaustion() → an auth-failover NATS event + the 1m→30m
    // rate-limit backoff. Rotating accounts cannot fix a server-initiated
    // FIN-mid-stream (the Anthropic-side issue corpus — anthropics/claude-code
    // #67766, #62146 — shows a fresh connection opened immediately afterwards
    // works fine), so the agent was torn down and re-launched instead of picking
    // up where it left off. Those three shapes now classify as ServerError
    // below, which dismisses + re-nudges IN PLACE (no rotation, no failover).
    if lower.contains("rate_limit_error")
        || lower.contains("rate limit")
        || lower.contains("rate_limit")
        || lower.contains("too many requests")
        || lower.contains("429")
        || lower.contains("temporarily limiting")
        || lower.contains("spend limit")
        || lower.contains("monthly spend")
        || lower.contains("no deployments available")
        || lower.contains("deployments available for selected model")
    {
        return ErrorCategory::RateLimited;
    }

    // Network / connectivity errors
    if lower.contains("connection refused")
        || lower.contains("connection reset")
        || lower.contains("dns")
        || lower.contains("timeout")
        || lower.contains("timed out")
        || lower.contains("no internet")
        || lower.contains("network")
        || lower.contains("econnrefused")
        || lower.contains("enotfound")
    {
        return ErrorCategory::NoInternet;
    }

    // Server errors — provider-side failures that clear on a quick in-place
    // retry, recovered by the controller's `server_error` branch in monitorExit
    // (dismiss the error screen → short backoff → re-inject the work prompt,
    // SAME session, NO profile rotation / auth-failover).
    //
    // This also covers transient API-wrapped *connection* bounces:
    //   * "API Error: terminated" — the response stream was terminated
    //     mid-flight.
    //   * "API Error: The socket connection was closed unexpectedly. For more
    //     information, pass `verbose: true` …" — the upstream sent a clean FIN
    //     while the HTTP/2 response was still streaming (server-initiated, not a
    //     local outage; a fresh connection opened immediately afterwards works).
    //   * "API Error: Connection error." — the CLI's fetch() could not establish
    //     a TCP connection to the upstream/proxy (canonically a transient
    //     ECONNREFUSED while the LiteLLM proxy is mid-restart).
    //   * "API Error: Connection closed mid-response. The response above may be
    //     incomplete." — the upstream FIN'd the HTTP/2 stream *after* some tokens
    //     had already been emitted (server-initiated mid-stream close). Same
    //     failure family as the socket-closed banner; the phrasing differs
    //     because Claude Code (>= 2.x) surfaces the partial-response variant with
    //     its own message. Neither "api_error" (underscore) nor the socket-closed
    //     substrings match this wording, so before this rule it fell through to
    //     ErrorCategory::Other — a category with NO recovery branch in the
    //     controller's monitorExit — and the agent sat on the error screen until
    //     the stale reaper killed it, losing all conversation context
    //     (ka-0000006871). The "connection closed mid-response" and "response
    //     above may be incomplete" substrings route it to ServerError → in-place
    //     dismiss + short backoff + re-nudge, same session.
    // These are matched here — NOT under the connectivity branch above
    // (a *bare* "connection refused"/ECONNREFUSED is a genuine local outage →
    // NoInternet) and NOT under rate-limiting (rotating accounts can't fix a
    // transient upstream bounce; see the rate-limit comment above and
    // ka-0000006962). The "api error:" prefix is what distinguishes the
    // recoverable upstream bounce from a bare local connectivity failure.
    //
    // "529" is Anthropic's *overloaded* status — the single most likely way the
    // upstream fails during a capacity incident. It is matched numerically
    // because the textual markers ("overloaded" / "overloaded_error") are not
    // always what reaches us: a proxy or gateway can flatten the JSON error body
    // to a bare status, surfacing as "API Error: 529" with no prose to key on.
    // Before this it fell through every arm to ErrorCategory::Other — the one
    // category with NO recovery branch in monitorExit — so the agent sat on the
    // error screen until the stale reaper killed it, the same failure mode as
    // ka-0000006871 / ka-0000002780. Classified ServerError, not RateLimited:
    // 529 is provider-side capacity, so the correct response is the short
    // in-place backoff, and mapping it to RateLimited would trip
    // is_auth_exhaustion() → spurious failover + account churn that cannot
    // possibly clear a provider overload (ka-0000003723).
    //
    // KEEP IN SYNC with the gardenio post-mortem classifier —
    // gardenio/controller/internal/deathreport/classify.go
    // (matchesUpstreamErrorShape / upstreamErrorSubstrings). That Go side
    // must agree with THIS live verdict about what counts as an upstream
    // outage, or an "upstream_api_error" death would be classified as some
    // other family after the fact (ka-0000000202). The two substring sets are
    // intentionally identical; if you add a shape here, add it there too.
    if lower.contains("api_error")
        || lower.contains("overloaded_error")
        || lower.contains("overloaded")
        || lower.contains("internal_error")
        || lower.contains("internal server error")
        || lower.contains("server_error")
        || lower.contains("500")
        || lower.contains("502")
        || lower.contains("503")
        || lower.contains("529")
        || lower.contains("api error: terminated")
        || lower.contains("socket connection was closed")
        || lower.contains("connection was closed unexpectedly")
        || lower.contains("api error: connection error")
        || lower.contains("connection closed mid-response")
        || lower.contains("response above may be incomplete")
    {
        return ErrorCategory::ServerError;
    }

    ErrorCategory::Other
}

/// Returns `true` if `stop_reason` is Anthropic's `"refusal"` value — the
/// Messages API stop reason emitted when the model declines to generate a
/// response for content-policy / safety reasons (as opposed to `end_turn`,
/// `tool_use`, `max_tokens`, or `stop_sequence`).
///
/// This is deliberately a standalone predicate rather than a new
/// [`ErrorCategory`] variant: a refusal is **not** an agent *error* state (the
/// request succeeded — the model simply chose not to answer), so it must not
/// ripple a new category through the error/serde/transport surfaces or trip any
/// of the error-recovery branches in the controller's `monitorExit`. Instead it
/// is detected on the assistant message response and forwarded as its own
/// classified NATS event (`{prefix}.messages.{actor}.refusal`) so the controller
/// can persist a durable log of every refusal for the surveys section
/// (ka-0000004364 → ka-0000006122), exactly as [`is_hard_auth_403`] /
/// [`is_login_required`] are matched without a new [`ErrorCategory`].
///
/// Matching is an exact, case-insensitive, whitespace-trimmed comparison against
/// the single canonical token `"refusal"` — NOT a substring scan — so ordinary
/// assistant prose that merely mentions the word "refusal" cannot masquerade as
/// an API-level refusal signal.
/// Map a leading `[codex:<CodexErrorInfo>]` tag (lower-cased) onto a category.
///
/// The class names are the app-server protocol's `CodexErrorInfo` variants
/// (`coop/tests/fixtures/codex`, `codex app-server generate-json-schema`).
/// `contextWindowExceeded` and the other unmapped classes are deliberately
/// [`ErrorCategory::Other`]: no existing category matches, and rotating
/// credentials cannot fix them.
fn codex_tag_category(lower: &str) -> Option<ErrorCategory> {
    let rest = lower
        .strip_prefix(crate::driver::codex::parse::CODEX_TAG_PREFIX.to_lowercase().as_str())?;
    let (class, message) = rest.split_once(']')?;
    // A connection-class tag wrapping an HTTP 401/403 is an auth failure, not a
    // connectivity drop: the backend answered and refused the credential. Treating
    // it as `NoInternet` made gb init dismiss + re-inject forever (gd-fr3xjSxIws).
    if matches!(class, "httpconnectionfailed" | "responsestreamconnectionfailed")
        && is_codex_auth_rejection(message)
    {
        return Some(ErrorCategory::Unauthorized);
    }
    Some(match class {
        "unauthorized" => ErrorCategory::Unauthorized,
        "usagelimitexceeded" | "ratelimitexceeded" => ErrorCategory::RateLimited,
        "sessionbudgetexceeded" => ErrorCategory::BudgetExceeded,
        "serveroverloaded"
        | "internalservererror"
        | "responsestreamdisconnected"
        | "responsetoomanyfailedattempts" => ErrorCategory::ServerError,
        "httpconnectionfailed" | "responsestreamconnectionfailed" => ErrorCategory::NoInternet,
        _ => ErrorCategory::Other,
    })
}

/// Map a leading `[gemini:<class>]` tag (lower-cased) onto a category.
///
/// A quota/rate-limit failure rotates credentials like any other rate limit
/// (ADR 0005 R8); a context-window overflow does not, so it is `Other`.
fn gemini_tag_category(lower: &str) -> Option<ErrorCategory> {
    let rest = lower
        .strip_prefix(crate::driver::gemini::parse::GEMINI_TAG_PREFIX.to_lowercase().as_str())?;
    let (class, _) = rest.split_once(']')?;
    Some(match class {
        "unauthorized" => ErrorCategory::Unauthorized,
        "quotaexhausted" => ErrorCategory::RateLimited,
        _ => ErrorCategory::Other,
    })
}

/// `true` when a Codex connection-failure message carries an auth rejection from
/// the backend: a standalone `401`/`403` status token or the usual bearer /
/// unauthorized phrasing. Status tokens must not be embedded in a longer number
/// (ports, byte counts, ids).
fn is_codex_auth_rejection(lower: &str) -> bool {
    if lower.contains("missing bearer")
        || lower.contains("unauthorized")
        || lower.contains("invalid_api_key")
        || lower.contains("token_expired")
    {
        return true;
    }
    ["401", "403"].iter().any(|code| {
        lower.match_indices(code).any(|(i, _)| {
            let before = lower[..i].chars().next_back();
            let after = lower[i + code.len()..].chars().next();
            !before.is_some_and(|c| c.is_ascii_digit())
                && !after.is_some_and(|c| c.is_ascii_digit())
        })
    })
}

pub fn is_refusal_stop_reason(stop_reason: &str) -> bool {
    stop_reason.trim().eq_ignore_ascii_case("refusal")
}

/// Number of *consecutive* terminal hard-auth-403 errors after which a coop
/// session is declared permanently bricked and is parked/killed by
/// [`HardAuth403Watchdog`].
///
/// Why 3 (and not 1):
///
/// A single `403 key not allowed to access model` can be a transient blip —
/// e.g. allowlist propagation lag in the seconds right after a credential
/// switch, before the LiteLLM/bedrock key picks up the new model grant. The
/// bedrock failover root-cause fix (ka-0000001014) also rebuilds the baked-in
/// `--model` flag on the *next* restart, so an agent that 403s once may well
/// recover on the following turn. Giving the session two grace turns avoids
/// nuking an agent that would have self-healed.
///
/// Three consecutive 403s with **zero** intervening successful inference,
/// however, is unambiguously a stuck key/model-allowlist mismatch, not a
/// blip: the request fails identically every turn and waiting or rotating
/// accounts will not help. At that point the agent cannot run a single
/// inference turn — it cannot even process its own `gb stop` — and would
/// otherwise sit as a zombie pod holding capacity indefinitely (17h in the
/// original incident, ka-0000006562). Three is a small constant chosen to
/// trip fast while tolerating the known transient-recovery window.
pub const HARD_AUTH_403_THRESHOLD: u32 = 3;

/// Returns `true` if `detail` matches a *terminal hard-auth-403* shape: an
/// inference request rejected because the API key is not permitted to use the
/// requested model (canonically `403 key not allowed to access model`, as
/// emitted by the LiteLLM proxy / AWS Bedrock when a key's model allowlist
/// does not include the requested model).
///
/// This is deliberately distinct from [`ErrorCategory::RateLimited`] and
/// [`ErrorCategory::BudgetExceeded`]: those are recoverable by waiting or by
/// rotating to another account. A hard-auth-403 is **not** — every turn fails
/// identically until the key's model allowlist or the requested model itself
/// changes, neither of which coop can do mid-session. It is matched as a
/// standalone predicate (rather than a new [`ErrorCategory`] variant) so it
/// can be tracked across turns by [`HardAuth403Watchdog`] without rippling a
/// new category through every transport/serde surface.
///
/// Matching is intentionally specific so a generic `403 Forbidden` (which may
/// be a transient gateway hiccup) does NOT trip the terminal watchdog — the
/// match requires the distinctive "(not) allowed to access/call model"
/// phrasing.
pub fn is_hard_auth_403(detail: &str) -> bool {
    let lower = detail.to_lowercase();
    lower.contains("not allowed to access model")
        || lower.contains("not allowed to call model")
        || lower.contains("key not allowed to access")
}

/// Tracks consecutive terminal hard-auth-403 errors within a single coop
/// session so the session can be parked/killed once the error proves
/// persistent rather than transient (see [`HARD_AUTH_403_THRESHOLD`]).
///
/// Semantics — fed one error detail per [`AgentState::Error`] observation:
///   * a hard-auth-403 detail increments the consecutive counter;
///   * any other error detail resets it to zero (the failure is not a
///     persistent hard-auth-403);
///   * [`record_recovery`](Self::record_recovery) resets it to zero on a
///     genuinely successful, productive turn.
///
/// The watchdog never resets on the `Working` state that precedes each turn's
/// error — otherwise the Working→Error→Working→Error cycle of a perpetually
/// 403ing agent would clear the counter every turn and never trip.
///
/// [`AgentState::Error`]: crate::driver::AgentState::Error
#[derive(Debug, Clone, Default)]
pub struct HardAuth403Watchdog {
    consecutive: u32,
}

impl HardAuth403Watchdog {
    /// Create a fresh watchdog with a zero consecutive count.
    pub fn new() -> Self {
        Self::default()
    }

    /// Feed an error detail observed on entry to `AgentState::Error`.
    ///
    /// Increments the consecutive counter on a hard-auth-403 match and resets
    /// it otherwise. Returns `true` once the count reaches
    /// [`HARD_AUTH_403_THRESHOLD`] (i.e. the session should be terminally
    /// parked/killed).
    pub fn record_error(&mut self, detail: &str) -> bool {
        if is_hard_auth_403(detail) {
            self.consecutive = self.consecutive.saturating_add(1);
        } else {
            self.consecutive = 0;
        }
        self.tripped()
    }

    /// Reset the counter after a successful, productive turn (no 403).
    pub fn record_recovery(&mut self) {
        self.consecutive = 0;
    }

    /// Whether the consecutive count has reached the terminal threshold.
    pub fn tripped(&self) -> bool {
        self.consecutive >= HARD_AUTH_403_THRESHOLD
    }

    /// Current consecutive hard-auth-403 count (for logging/telemetry).
    pub fn consecutive(&self) -> u32 {
        self.consecutive
    }
}

/// Number of *consecutive* "Not logged in" / login-required observations after
/// which a coop session is declared terminally credential-starved and is
/// parked/killed by [`LoginRequiredWatchdog`].
///
/// Why a threshold > 1 (and 3 specifically):
///
/// A single "Not logged in · Please run /login" is almost always TRANSIENT —
/// the coopmux credential broker swaps the active profile (exit-code-10
/// credential switch) and Claude Code briefly reports not-logged-in before it
/// re-auths and continues. The OSHA-300 audit (ka-0000006636) measured this
/// empirically: 25/26 login-mentioning sessions recovered, with the LAST
/// assistant message being real work, not a login error. Terminating on the
/// first observation would nuke those recoverable agents.
///
/// THREE consecutive login-required turns with **zero** intervening successful
/// inference, however, is the genuine credential-not-ready-at-spawn failure
/// (ka-0000007394): the credential was never distributed/validated for this
/// pod, every turn no-ops on the same banner, and waiting will not help. At
/// that point the agent cannot run a single inference turn — it cannot even
/// process its own `gb stop` — and would otherwise burn cron wake-up after
/// cron wake-up doing zero work (33 in the original incident). Three mirrors
/// [`HARD_AUTH_403_THRESHOLD`]: small enough to trip fast, large enough to
/// tolerate the known transient-switch window.
pub const LOGIN_REQUIRED_THRESHOLD: u32 = 3;

/// Returns `true` if `detail` matches a *login-required* / not-authenticated
/// shape — canonically Claude Code's "Not logged in · Please run /login"
/// banner, emitted when the CLI has no valid credential at all (as opposed to
/// a credential that authenticates but is rate-limited / 403'd on a model).
///
/// This is deliberately distinct from [`ErrorCategory::Unauthorized`] (an
/// `authentication_error` from the API for a *present but rejected* key) and
/// from [`is_hard_auth_403`] (a present key not allowed a specific model).
/// A not-logged-in state means no usable credential reached the pod, so no
/// amount of waiting or account rotation recovers it — only a fresh spawn with
/// the credential actually provisioned can. It is matched as a standalone
/// predicate (rather than a new [`ErrorCategory`] variant) so it can be tracked
/// across turns by [`LoginRequiredWatchdog`] without rippling a new category
/// through every transport/serde surface — exactly as [`is_hard_auth_403`]
/// does.
///
/// Matching is intentionally specific to the login banner so ordinary prose
/// mentioning the words (e.g. an agent writing "the user is not logged in")
/// does not, on its own, trip the terminal watchdog — and the > 1 threshold
/// guards the residual risk further.
pub fn is_login_required(detail: &str) -> bool {
    let lower = detail.to_lowercase();
    lower.contains("not logged in")
        || lower.contains("please run /login")
        || lower.contains("please sign in")
        || lower.contains("login required")
        || lower.contains("sign in to continue")
}

/// Tracks consecutive "Not logged in" / login-required observations within a
/// single coop session so the session can be parked/killed once the condition
/// proves persistent rather than a transient credential-switch blip (see
/// [`LOGIN_REQUIRED_THRESHOLD`]).
///
/// Semantics mirror [`HardAuth403Watchdog`] exactly — it is fed one detail per
/// state observation that carries a banner (an `AgentState::Error` detail, or
/// the inline `Working`→`Idle` `last_message`):
///   * a login-required detail increments the consecutive counter;
///   * any other detail resets it to zero (the failure is not a persistent
///     login-required condition);
///   * [`record_recovery`](Self::record_recovery) resets it to zero on a
///     genuinely successful, productive turn.
///
/// Like the 403 watchdog, it never resets on the `Working` state that precedes
/// each turn — otherwise the Working→banner→Working→banner cycle of a
/// perpetually-not-logged-in agent would clear the counter every turn and never
/// trip.
#[derive(Debug, Clone, Default)]
pub struct LoginRequiredWatchdog {
    consecutive: u32,
}

impl LoginRequiredWatchdog {
    /// Create a fresh watchdog with a zero consecutive count.
    pub fn new() -> Self {
        Self::default()
    }

    /// Feed a detail observed on a banner-bearing state. Increments the
    /// consecutive counter on a login-required match and resets it otherwise.
    /// Returns `true` once the count reaches [`LOGIN_REQUIRED_THRESHOLD`] (the
    /// session should be terminally parked/killed).
    pub fn record_error(&mut self, detail: &str) -> bool {
        if is_login_required(detail) {
            self.consecutive = self.consecutive.saturating_add(1);
        } else {
            self.consecutive = 0;
        }
        self.tripped()
    }

    /// Reset the counter after a successful, productive turn (no banner).
    pub fn record_recovery(&mut self) {
        self.consecutive = 0;
    }

    /// Whether the consecutive count has reached the terminal threshold.
    pub fn tripped(&self) -> bool {
        self.consecutive >= LOGIN_REQUIRED_THRESHOLD
    }

    /// Current consecutive login-required count (for logging/telemetry).
    pub fn consecutive(&self) -> u32 {
        self.consecutive
    }
}

#[cfg(test)]
#[path = "error_category_tests.rs"]
mod tests;
