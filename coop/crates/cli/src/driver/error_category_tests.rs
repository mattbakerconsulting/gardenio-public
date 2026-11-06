// SPDX-License-Identifier: BUSL-1.1
// Copyright (c) 2026 Matt Baker <fimbaz@gmail.com>

use super::{
    classify_error_detail, is_hard_auth_403, is_login_required, is_refusal_stop_reason,
    ErrorCategory, HardAuth403Watchdog, LoginRequiredWatchdog, HARD_AUTH_403_THRESHOLD,
    LOGIN_REQUIRED_THRESHOLD,
};

#[yare::parameterized(
    auth_error = { "authentication_error", ErrorCategory::Unauthorized },
    invalid_key = { "invalid api key", ErrorCategory::Unauthorized },
    invalid_key_snake = { "invalid_api_key", ErrorCategory::Unauthorized },
    permission = { "permission_error", ErrorCategory::Unauthorized },
    auth_failed = { "authentication_failed", ErrorCategory::Unauthorized },
    budget_exceeded = { "budget_exceeded", ErrorCategory::BudgetExceeded },
    budget_exceeded_sentence = { "Budget has been exceeded for project X", ErrorCategory::BudgetExceeded },
    budget_exceeded_type = { "budget_exceeded: Budget limit reached", ErrorCategory::BudgetExceeded },
    billing = { "billing", ErrorCategory::OutOfCredits },
    insufficient_credits = { "insufficient_credits", ErrorCategory::OutOfCredits },
    insufficient_credits_space = { "insufficient credits", ErrorCategory::OutOfCredits },
    out_of_credits = { "out of credits", ErrorCategory::OutOfCredits },
    payment = { "payment_required", ErrorCategory::OutOfCredits },
    rate_limit_error = { "rate_limit_error", ErrorCategory::RateLimited },
    rate_limit_space = { "rate limit exceeded", ErrorCategory::RateLimited },
    too_many = { "too many requests", ErrorCategory::RateLimited },
    http_429 = { "429 Too Many Requests", ErrorCategory::RateLimited },
    // A 429 carrying a NO-DEPLOYMENTS marker is a MODEL-group (pool-exhaustion)
    // throttle — routed to ModelRateLimited, NOT RateLimited: every pooled
    // profile serves the same model on the same upstream, so rotation cannot
    // help and must not fire (ka-0000001718). See the model_rate_limited_*
    // parameterized cases and model_rate_limited_is_not_auth_exhaustion below.
    litellm_temp_limiting = {
        "API Error: Server is temporarily limiting requests (not your usage limit) · No deployments available for selected model, Try again in 5 seconds.",
        ErrorCategory::ModelRateLimited
    },
    litellm_temp_limiting_short = { "Server is temporarily limiting requests", ErrorCategory::RateLimited },
    // A BARE account monthly-spend 429 with NO no-fallback / no-deployments
    // marker stays RateLimited — legitimate single-account failover (profile
    // rotation) is preserved.
    monthly_spend_limit_bare = {
        "This request would exceed your account's monthly spend limit. Please try again later.",
        ErrorCategory::RateLimited
    },
    // The canonical production incident string contains BOTH account-spend prose
    // AND "Available Model Group Fallbacks=None". The no-fallback marker MUST
    // win — this is the exact P0 string that a "spend-wins → keep rotating"
    // split would misfire on (ka-0000001718). → ModelRateLimited.
    monthly_spend_limit_wrapped = {
        "API Error: Server is temporarily limiting requests (not your usage limit) · {\"type\":\"error\",\"error\":{\"type\":\"rate_limit_error\",\"message\":\"This request would exceed your account's monthly spend limit. Please try again later.\"},\"request_id\":\"req_011Cc3voD3iFhq3b3Km6Rpxd\"}. Received Model Group=claude-opus-4-8-oauth Available Model Group Fallbacks=None",
        ErrorCategory::ModelRateLimited
    },
    spend_limit_phrase = { "spend limit reached", ErrorCategory::RateLimited },
    // No-deployments / no-fallback markers → ModelRateLimited (pool exhausted).
    litellm_no_deployments = { "no deployments available", ErrorCategory::ModelRateLimited },
    litellm_deployments_for_model = { "deployments available for selected model", ErrorCategory::ModelRateLimited },
    no_fallback_marker = {
        "Received Model Group=claude-opus-4-8-oauth Available Model Group Fallbacks=None",
        ErrorCategory::ModelRateLimited
    },
    no_fallback_marker_mixed_case = {
        "AVAILABLE MODEL GROUP FALLBACKS=NONE",
        ErrorCategory::ModelRateLimited
    },
    // Transient API-wrapped connection bounces classify as ServerError (not
    // RateLimited): they recover via the controller's in-place dismiss+re-nudge
    // with NO profile rotation / auth-failover (ka-0000006962). A fresh
    // connection works immediately, so rotating accounts cannot help.
    api_error_terminated = { " API Error: terminated", ErrorCategory::ServerError },
    api_error_terminated_lower = { "api error: terminated", ErrorCategory::ServerError },
    api_error_socket_closed = {
        "API Error: The socket connection was closed unexpectedly. For more information, pass `verbose: true` in the second argument to fetch()",
        ErrorCategory::ServerError
    },
    api_error_socket_closed_short = { "socket connection was closed", ErrorCategory::ServerError },
    api_error_conn_closed_unexpectedly = { "connection was closed unexpectedly", ErrorCategory::ServerError },
    api_error_connection_error = {
        "API Error: Connection error. For more information, pass `verbose: true` in the second argument to fetch()",
        ErrorCategory::ServerError
    },
    api_error_connection_error_short = { "api error: connection error", ErrorCategory::ServerError },
    // "API Error: Connection closed mid-response. The response above may be
    // incomplete." — a server-initiated FIN while the HTTP/2 response was still
    // streaming (same failure family as the socket-closed banner). Recovers via
    // the controller's in-place dismiss + re-nudge, NO profile rotation.
    api_error_conn_closed_mid_response = {
        "API Error: Connection closed mid-response. The response above may be incomplete.",
        ErrorCategory::ServerError
    },
    api_error_conn_closed_mid_response_short = { "connection closed mid-response", ErrorCategory::ServerError },
    api_error_response_may_be_incomplete = { "The response above may be incomplete.", ErrorCategory::ServerError },
    conn_refused = { "connection refused", ErrorCategory::NoInternet },
    conn_reset = { "connection reset", ErrorCategory::NoInternet },
    dns_fail = { "dns resolution failed", ErrorCategory::NoInternet },
    timeout = { "request timeout", ErrorCategory::NoInternet },
    timed_out = { "timed out", ErrorCategory::NoInternet },
    no_internet = { "no internet", ErrorCategory::NoInternet },
    network = { "network error", ErrorCategory::NoInternet },
    econnrefused = { "ECONNREFUSED", ErrorCategory::NoInternet },
    enotfound = { "ENOTFOUND", ErrorCategory::NoInternet },
    api_error = { "api_error", ErrorCategory::ServerError },
    overloaded = { "overloaded_error", ErrorCategory::ServerError },
    overloaded_plain = { "overloaded", ErrorCategory::ServerError },
    internal = { "internal_error", ErrorCategory::ServerError },
    internal_space = { "internal server error", ErrorCategory::ServerError },
    server_error = { "server_error", ErrorCategory::ServerError },
    http_500 = { "500 Internal Server Error", ErrorCategory::ServerError },
    http_502 = { "502 Bad Gateway", ErrorCategory::ServerError },
    http_503 = { "503 Service Unavailable", ErrorCategory::ServerError },
    // 529 is Anthropic's *overloaded* status. Matched numerically because the
    // textual markers are not always what reaches us — a proxy/gateway can
    // flatten the JSON error body to a bare status. Before this it fell through
    // to Other, the one category with no recovery branch (ka-0000003723).
    http_529 = { "529 Overloaded", ErrorCategory::ServerError },
    api_error_529 = { "API Error: 529", ErrorCategory::ServerError },
    bare_529 = { "529", ErrorCategory::ServerError },
    unknown_string = { "something went wrong", ErrorCategory::Other },
    empty = { "", ErrorCategory::Other },
    random = { "xyzzy", ErrorCategory::Other },
)]
fn classify(detail: &str, expected: ErrorCategory) {
    assert_eq!(classify_error_detail(detail), expected);
}

#[test]
fn classify_is_case_insensitive() {
    assert_eq!(classify_error_detail("AUTHENTICATION_ERROR"), ErrorCategory::Unauthorized);
    assert_eq!(classify_error_detail("Rate_Limit_Error"), ErrorCategory::RateLimited);
}

/// Documents the arm precedence for a 529 that also carries connectivity prose
/// (ka-0000003723). Matching is ordered and first-wins, and the NoInternet arm
/// sits *before* ServerError, so a composite like "529 … connection reset"
/// resolves to NoInternet rather than ServerError.
///
/// This is deliberate and safe: NoInternet has its own in-place recovery branch
/// in the controller's monitorExit (dismiss + backoff capped at 5m + re-nudge,
/// same session, no profile rotation), so such a message still recovers — it
/// just backs off on the wider connectivity schedule instead of the 2m
/// server-error one. The property that matters is that neither arm is Other,
/// the one category with NO recovery branch. Pinned as a test so a future
/// reorder of these arms is a deliberate choice rather than a silent
/// regression.
#[test]
fn classify_529_arm_precedence_against_connectivity_prose() {
    // Bare 529, and 529 with non-connectivity prose → ServerError.
    assert_eq!(classify_error_detail("API Error: 529"), ErrorCategory::ServerError);
    assert_eq!(classify_error_detail("529 Overloaded"), ErrorCategory::ServerError);

    // 529 composited with connectivity wording → NoInternet wins (earlier arm).
    assert_eq!(
        classify_error_detail("API Error: 529 — connection reset by peer"),
        ErrorCategory::NoInternet,
        "the NoInternet arm precedes ServerError, so connectivity prose wins a \
         composite; both recover in place, so this is a backoff-schedule \
         difference and not a lost-agent regression"
    );

    // Whichever arm wins, the outcome must never be Other — that is the
    // category with no recovery branch, where the agent sits on the error
    // screen until the stale reaper kills it.
    for detail in [
        "API Error: 529",
        "529 Overloaded",
        "API Error: 529 — connection reset by peer",
        "{\"type\":\"error\",\"error\":{\"type\":\"overloaded_error\"}}",
    ] {
        assert_ne!(
            classify_error_detail(detail),
            ErrorCategory::Other,
            "{detail:?} must classify into a category with a recovery branch"
        );
    }
}

// ── Model-group rate limiting (pool exhaustion, no fallback) — ka-0000001718 ──

/// A 429 carrying a no-fallback / no-deployments model-group marker classifies
/// as ModelRateLimited and is NOT an auth-exhaustion category, so it never
/// drives handle_rate_limit → try_auto_rotate → the exit-10 cold-restart loop.
/// Rotating credentials cannot clear a model-level throttle (every pooled
/// profile serves the same throttled model), so the controller parks in place.
#[test]
fn model_rate_limited_is_not_auth_exhaustion() {
    assert!(
        !ErrorCategory::ModelRateLimited.is_auth_exhaustion(),
        "ModelRateLimited must NOT be an auth-exhaustion category — it must not \
         trip try_auto_rotate / exit-10 (ka-0000001718)"
    );
    // The account-level rate limit stays an auth-exhaustion category so a bare
    // single-account 429 still fails over via profile rotation.
    assert!(
        ErrorCategory::RateLimited.is_auth_exhaustion(),
        "bare account RateLimited must remain auth-exhaustion (failover preserved)"
    );
}

/// The canonical production incident string (monthly_spend_limit_wrapped) — the
/// exact string that hung plan-assistants in the 2026-07-10 recurrence —
/// contains BOTH account-spend prose AND "Available Model Group Fallbacks=None".
/// It MUST classify as ModelRateLimited (park), NOT RateLimited (rotate): the
/// no-fallback marker is the discriminator, not the "account"/"spend" prose.
#[test]
fn canonical_incident_string_parks_does_not_rotate() {
    let canonical = "API Error: Server is temporarily limiting requests (not your usage limit) · \
        {\"type\":\"error\",\"error\":{\"type\":\"rate_limit_error\",\"message\":\"This request \
        would exceed your account's monthly spend limit. Please try again later.\"},\
        \"request_id\":\"req_011Cc3voD3iFhq3b3Km6Rpxd\"}. Received Model \
        Group=claude-opus-4-8-oauth Available Model Group Fallbacks=None";
    let category = classify_error_detail(canonical);
    assert_eq!(
        category,
        ErrorCategory::ModelRateLimited,
        "canonical incident string must park (ModelRateLimited), not rotate (RateLimited)"
    );
    assert!(
        !category.is_auth_exhaustion(),
        "canonical incident string must NOT trip auth-exhaustion rotation"
    );
}

/// A bare account monthly-spend 429 WITHOUT any no-fallback / no-deployments
/// marker stays RateLimited (auth-exhaustion), so legitimate single-account
/// failover via profile rotation is preserved and unchanged by this fix.
#[test]
fn bare_account_429_still_rotates() {
    let bare = "This request would exceed your account's monthly spend limit. \
                Please try again later.";
    let category = classify_error_detail(bare);
    assert_eq!(
        category,
        ErrorCategory::RateLimited,
        "a bare account 429 (no no-fallback marker) must stay RateLimited"
    );
    assert!(
        category.is_auth_exhaustion(),
        "a bare account 429 must remain an auth-exhaustion category (failover preserved)"
    );
}

// ── Terminal, non-failover credential failures — ka-0000003962 ──────────────

/// `Unauthorized` and `OutOfCredits` are terminal, non-failover credential
/// categories: rotating credentials cannot fix a key ACL or an out-of-credits
/// account. They are the categories `maybe_publish_auth_exhausted` silently
/// drops (it only maps `BudgetExceeded`/`RateLimited`), so they get a
/// DIAGNOSTIC-only NATS event on a distinct subject instead. Crucially they
/// must NEVER be auth-exhaustion categories — routing them into failover would
/// cold-restart the pod forever.
#[test]
fn terminal_auth_failures_are_not_auth_exhaustion() {
    for cat in [ErrorCategory::Unauthorized, ErrorCategory::OutOfCredits] {
        assert!(
            cat.is_terminal_auth_failure(),
            "{cat:?} must be a terminal, non-failover credential failure"
        );
        assert!(
            !cat.is_auth_exhaustion(),
            "{cat:?} must NOT be an auth-exhaustion category — a terminal auth \
             failure must never trigger failover/rotation (ka-0000003962)"
        );
    }
}

/// The failover categories (`BudgetExceeded`/`RateLimited`) and the park
/// category (`ModelRateLimited`) are NOT terminal auth failures — they already
/// emit their own events / park in place and must not be double-reported on the
/// diagnostic subject.
#[test]
fn non_terminal_categories_are_not_terminal_auth_failures() {
    for cat in [
        ErrorCategory::BudgetExceeded,
        ErrorCategory::RateLimited,
        ErrorCategory::ModelRateLimited,
        ErrorCategory::NoInternet,
        ErrorCategory::ServerError,
        ErrorCategory::Other,
    ] {
        assert!(
            !cat.is_terminal_auth_failure(),
            "{cat:?} must NOT be classified as a terminal auth failure"
        );
    }
}

#[test]
fn serde_roundtrip() -> anyhow::Result<()> {
    let categories = [
        ErrorCategory::Unauthorized,
        ErrorCategory::OutOfCredits,
        ErrorCategory::BudgetExceeded,
        ErrorCategory::RateLimited,
        ErrorCategory::ModelRateLimited,
        ErrorCategory::NoInternet,
        ErrorCategory::ServerError,
        ErrorCategory::Other,
    ];
    for cat in &categories {
        let json =
            serde_json::to_string(cat).map_err(|e| anyhow::anyhow!("serialize {cat:?}: {e}"))?;
        let back: ErrorCategory =
            serde_json::from_str(&json).map_err(|e| anyhow::anyhow!("deserialize {json}: {e}"))?;
        assert_eq!(*cat, back);
    }
    Ok(())
}

#[test]
fn as_str_matches_serde() -> anyhow::Result<()> {
    let categories = [
        ErrorCategory::Unauthorized,
        ErrorCategory::OutOfCredits,
        ErrorCategory::BudgetExceeded,
        ErrorCategory::RateLimited,
        ErrorCategory::ModelRateLimited,
        ErrorCategory::NoInternet,
        ErrorCategory::ServerError,
        ErrorCategory::Other,
    ];
    for cat in &categories {
        let json =
            serde_json::to_string(cat).map_err(|e| anyhow::anyhow!("serialize {cat:?}: {e}"))?;
        // serde produces `"snake_case"`, as_str should match without quotes
        let expected = json.trim_matches('"');
        assert_eq!(cat.as_str(), expected);
        assert_eq!(cat.to_string(), expected);
    }
    Ok(())
}

// -- Terminal hard-auth-403 watchdog -----------------------------------------

#[yare::parameterized(
    canonical = { "API Error: 403 key not allowed to access model" },
    bare = { "key not allowed to access model" },
    call_variant = { "403 key not allowed to call model claude-opus-4-8" },
    wrapped = {
        "API Error: 403 {\"error\":{\"message\":\"key not allowed to access model: claude-opus-4-8\",\"type\":\"auth_error\"}}"
    },
    mixed_case = { "403 Key Not Allowed To Access Model" },
)]
fn hard_auth_403_matches(detail: &str) {
    assert!(is_hard_auth_403(detail), "expected hard-auth-403 match: {detail}");
}

#[yare::parameterized(
    generic_403 = { "403 Forbidden" },
    rate_limited = { "rate_limit_error: too many requests" },
    auth_error = { "authentication_error: invalid api key" },
    server_error = { "500 Internal Server Error" },
    empty = { "" },
    unrelated = { "model not found" },
)]
fn hard_auth_403_does_not_match(detail: &str) {
    assert!(!is_hard_auth_403(detail), "did not expect hard-auth-403 match: {detail}");
}

/// The detector fires only after exactly `HARD_AUTH_403_THRESHOLD` *consecutive*
/// hard-auth-403 errors — not before.
#[test]
fn watchdog_fires_after_threshold_consecutive() {
    let mut wd = HardAuth403Watchdog::new();
    let detail = "403 key not allowed to access model";

    // First THRESHOLD-1 errors must NOT trip.
    for i in 1..HARD_AUTH_403_THRESHOLD {
        assert!(!wd.record_error(detail), "tripped early at #{i}");
        assert_eq!(wd.consecutive(), i);
        assert!(!wd.tripped());
    }

    // The THRESHOLD-th consecutive error trips it.
    assert!(wd.record_error(detail), "did not trip at threshold");
    assert_eq!(wd.consecutive(), HARD_AUTH_403_THRESHOLD);
    assert!(wd.tripped());
}

/// A transient/mixed error sequence (403s interleaved with other errors, or
/// 403s cleared by a successful turn) must NOT trip the watchdog.
#[test]
fn watchdog_does_not_fire_on_transient_or_mixed() {
    let hard403 = "403 key not allowed to access model";

    // Mixed: a different error in the middle resets the run.
    let mut wd = HardAuth403Watchdog::new();
    assert!(!wd.record_error(hard403));
    assert!(!wd.record_error("rate_limit_error: slow down")); // resets to 0
    assert_eq!(wd.consecutive(), 0);
    // Even THRESHOLD-1 more 403s after the reset stay under the bar.
    for _ in 1..HARD_AUTH_403_THRESHOLD {
        assert!(!wd.record_error(hard403));
    }
    assert!(!wd.tripped(), "mixed sequence must not trip");

    // Recovery: a successful turn between 403s prevents tripping.
    let mut wd = HardAuth403Watchdog::new();
    for _ in 0..(HARD_AUTH_403_THRESHOLD - 1) {
        assert!(!wd.record_error(hard403));
    }
    wd.record_recovery(); // productive turn clears the count
    assert_eq!(wd.consecutive(), 0);
    assert!(!wd.record_error(hard403), "single 403 after recovery must not trip");
    assert!(!wd.tripped());
}

/// A single hard-auth-403 never trips (the threshold is > 1, by design, to
/// tolerate the post-credential-switch propagation blip).
#[test]
fn watchdog_single_403_is_transient() {
    let mut wd = HardAuth403Watchdog::new();
    assert!(HARD_AUTH_403_THRESHOLD > 1, "threshold must tolerate a transient blip");
    assert!(!wd.record_error("403 key not allowed to access model"));
    assert!(!wd.tripped());
}

/// Once tripped, the watchdog stays tripped under continued 403s (idempotent
/// re-fire is fine — the caller parks on the first trip).
#[test]
fn watchdog_stays_tripped() {
    let mut wd = HardAuth403Watchdog::new();
    let detail = "403 key not allowed to access model";
    for _ in 0..HARD_AUTH_403_THRESHOLD {
        wd.record_error(detail);
    }
    assert!(wd.tripped());
    assert!(wd.record_error(detail), "should remain tripped");
    assert!(wd.tripped());
}

// ── login-required predicate + watchdog (ka-0000007394) ─────────────────────

#[yare::parameterized(
    canonical = { "Not logged in · Please run /login" },
    bare = { "Not logged in" },
    slash_login = { "Please run /login to authenticate" },
    sign_in = { "Please sign in to continue" },
    login_required = { "Login required" },
    continue_phrase = { "Sign in to continue" },
    mixed_case = { "NOT LOGGED IN" },
)]
fn login_required_matches(detail: &str) {
    assert!(is_login_required(detail), "expected login-required match: {detail}");
}

#[yare::parameterized(
    hard_403 = { "403 key not allowed to access model" },
    rate_limit = { "rate_limit_error: slow down" },
    server_error = { "API Error: 500 Internal server error" },
    normal_work = { "Reading the file and applying the patch." },
    empty = { "" },
)]
fn login_required_does_not_match(detail: &str) {
    assert!(!is_login_required(detail), "did not expect login-required match: {detail}");
}

/// The login-required watchdog trips only after THRESHOLD consecutive
/// not-logged-in turns — not before.
#[test]
fn login_watchdog_fires_after_threshold_consecutive() {
    let mut wd = LoginRequiredWatchdog::new();
    let detail = "Not logged in · Please run /login";

    for i in 1..LOGIN_REQUIRED_THRESHOLD {
        assert!(!wd.record_error(detail), "tripped early at #{i}");
        assert_eq!(wd.consecutive(), i);
        assert!(!wd.tripped());
    }

    assert!(wd.record_error(detail), "did not trip at threshold");
    assert_eq!(wd.consecutive(), LOGIN_REQUIRED_THRESHOLD);
    assert!(wd.tripped());
}

/// A transient/mixed sequence (login banners interleaved with other states, or
/// cleared by a successful turn) must NOT trip the login watchdog — this is the
/// 25/26-recover credential-switch calibration (ka-0000006636).
#[test]
fn login_watchdog_does_not_fire_on_transient_or_mixed() {
    let banner = "Not logged in · Please run /login";

    // Mixed: a different error in the middle resets the run.
    let mut wd = LoginRequiredWatchdog::new();
    assert!(!wd.record_error(banner));
    assert!(!wd.record_error("rate_limit_error: slow down")); // resets to 0
    assert_eq!(wd.consecutive(), 0);
    for _ in 1..LOGIN_REQUIRED_THRESHOLD {
        assert!(!wd.record_error(banner));
    }
    assert!(!wd.tripped(), "mixed sequence must not trip");

    // Recovery: a successful turn between banners prevents tripping.
    let mut wd = LoginRequiredWatchdog::new();
    for _ in 0..(LOGIN_REQUIRED_THRESHOLD - 1) {
        assert!(!wd.record_error(banner));
    }
    wd.record_recovery(); // productive turn clears the count
    assert_eq!(wd.consecutive(), 0);
    assert!(!wd.record_error(banner), "single banner after recovery must not trip");
    assert!(!wd.tripped());
}

/// A single not-logged-in observation never trips (threshold > 1 by design, to
/// tolerate the post-credential-switch propagation blip).
#[test]
fn login_watchdog_single_observation_is_transient() {
    let mut wd = LoginRequiredWatchdog::new();
    assert!(LOGIN_REQUIRED_THRESHOLD > 1, "threshold must tolerate a transient blip");
    assert!(!wd.record_error("Not logged in · Please run /login"));
    assert!(!wd.tripped());
}

/// The two watchdogs are independent: alternating 403 and login-required
/// errors resets each other (neither reaches a consecutive run), so a mixed
/// auth-failure storm does not spuriously trip either terminal park.
#[test]
fn login_and_403_watchdogs_are_independent() {
    let mut login = LoginRequiredWatchdog::new();
    let mut h403 = HardAuth403Watchdog::new();
    let banner = "Not logged in · Please run /login";
    let h = "403 key not allowed to access model";

    for _ in 0..(LOGIN_REQUIRED_THRESHOLD + HARD_AUTH_403_THRESHOLD) {
        // Feed BOTH watchdogs both details (mirrors the transition wiring,
        // where every Error detail is offered to both record_error calls).
        login.record_error(banner);
        login.record_error(h);
        h403.record_error(banner);
        h403.record_error(h);
    }
    assert!(!login.tripped(), "alternating details must not trip login watchdog");
    assert!(!h403.tripped(), "alternating details must not trip 403 watchdog");
}

// ── refusal stop_reason predicate (ka-0000004364) ───────────────────────────

#[yare::parameterized(
    canonical = { "refusal" },
    upper = { "REFUSAL" },
    mixed_case = { "Refusal" },
    leading_ws = { "  refusal" },
    trailing_ws = { "refusal\n" },
    surrounding_ws = { "\t refusal \t" },
)]
fn refusal_stop_reason_matches(stop_reason: &str) {
    assert!(is_refusal_stop_reason(stop_reason), "expected refusal match: {stop_reason:?}");
}

#[yare::parameterized(
    end_turn = { "end_turn" },
    tool_use = { "tool_use" },
    max_tokens = { "max_tokens" },
    stop_sequence = { "stop_sequence" },
    empty = { "" },
    // A SUBSTRING containing the word must NOT match — detection is an exact
    // token comparison, so ordinary assistant prose mentioning "refusal" (or a
    // compound stop reason) can never masquerade as an API-level refusal signal.
    prose = { "the model issued a refusal here" },
    compound = { "refusal_pending" },
    pause_turn = { "pause_turn" },
)]
fn refusal_stop_reason_does_not_match(stop_reason: &str) {
    assert!(!is_refusal_stop_reason(stop_reason), "did not expect refusal match: {stop_reason:?}");
}

/// A refusal is NOT an error category: it must never be produced by
/// `classify_error_detail` (a successful-but-declined turn is not an agent error
/// state). The word "refusal" as an error detail falls through to `Other`, and
/// the predicate is the only refusal signal.
#[test]
fn refusal_is_not_an_error_category() {
    assert_eq!(classify_error_detail("refusal"), ErrorCategory::Other);
    assert_eq!(classify_error_detail("stop_reason: refusal"), ErrorCategory::Other);
}

/// Codex tags its typed error class up front; the tag decides the category even
/// when the prose would match a different rule (here "credits" → OutOfCredits).
#[yare::parameterized(
    unauthorized = { "[codex:unauthorized] token expired", ErrorCategory::Unauthorized },
    usage_limit = { "[codex:usageLimitExceeded] Buy more credits", ErrorCategory::RateLimited },
    rate_limit = { "[codex:rateLimitExceeded] slow down", ErrorCategory::RateLimited },
    budget = { "[codex:sessionBudgetExceeded] over", ErrorCategory::BudgetExceeded },
    overloaded = { "[codex:serverOverloaded] busy", ErrorCategory::ServerError },
    disconnected = { "[codex:responseStreamDisconnected] eof", ErrorCategory::ServerError },
    connection = { "[codex:httpConnectionFailed] refused", ErrorCategory::NoInternet },
    connection_401 = { "[codex:httpConnectionFailed] unexpected status 401 Unauthorized: Missing bearer", ErrorCategory::Unauthorized },
    connection_missing_bearer = { "[codex:httpConnectionFailed] Missing bearer or basic authentication", ErrorCategory::Unauthorized },
    connection_403 = { "[codex:responseStreamConnectionFailed] http 403 forbidden", ErrorCategory::Unauthorized },
    connection_port_not_status = { "[codex:httpConnectionFailed] connect to 10.0.0.1:14013 failed", ErrorCategory::NoInternet },
    context_window = { "[codex:contextWindowExceeded] too long", ErrorCategory::Other },
    unknown_class = { "[codex:somethingNew] 429 rate limit", ErrorCategory::Other },
    mixed_case = { "[Codex:UsageLimitExceeded] x", ErrorCategory::RateLimited },
)]
fn codex_tagged_errors_classify_by_class(detail: &str, expected: ErrorCategory) {
    assert_eq!(classify_error_detail(detail), expected);
}

/// An untagged detail (or a tag-like string not at the start) is classified by
/// the generic substring rules exactly as before.
#[test]
fn codex_tag_only_counts_as_a_prefix() {
    assert_eq!(classify_error_detail("oops [codex:unauthorized]"), ErrorCategory::Other);
    assert_eq!(classify_error_detail("rate_limit_error"), ErrorCategory::RateLimited);
}
