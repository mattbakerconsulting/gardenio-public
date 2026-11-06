# Slack Bridge Security & Code Quality Audit

**Date:** 2026-04-05
**Scope:** `gardenio/controller/internal/bridge/` (~120 Go source files, ~70,824 lines)
**Entry point:** `gardenio/controller/cmd/slack-bridge/main.go` (729 lines)

---

## Executive Summary

The Slack bridge is a well-structured service that bridges beads lifecycle events to Slack
notifications and handles Slack interaction webhooks. The codebase demonstrates solid Go
conventions, good use of interfaces for testability, and comprehensive deduplication logic
for SSE reconnect resilience. Test coverage is excellent (86+ test files).

However, the audit identified **4 critical**, **3 high**, **5 medium**, and **4 low**
severity findings. The most urgent issues are the complete lack of authentication on
HTTP API endpoints and potential SSRF vectors through attacker-controlled URLs.

---

## Critical Findings

### C1: No Authentication on HTTP API Endpoints

**Files:** `cmd/slack-bridge/main.go:83-116`, `api_decisions.go`, `bot_threads.go`, `bot_channels.go`

All HTTP endpoints share a single `http.ServeMux` with **no authentication middleware**.
Any network client that can reach the bridge HTTP port can:

- **Resolve/dismiss decisions** via `POST /api/decisions/{id}/resolve` and `/api/decisions/{id}/dismiss`
  (`api_decisions.go`) — an attacker can resolve agent decision checkpoints with arbitrary values
- **Read Slack thread messages** via `GET /api/slack/threads` (`bot_threads.go`)
- **Post replies to Slack threads** via `POST /api/slack/threads` (`bot_threads.go`)
- **List Slack channels and history** via `GET /api/slack/channels` (`bot_channels.go`)
- **Search Slack messages** via `/api/slack/channels?action=search` (`bot_channels.go`)
- **Proxy file downloads** via `GET /api/slack/files/{id}` (`bot_threads.go`, 50MB max)
- **View unreleased changes** via `GET /api/unreleased` (`main.go:117`, uses GitLab API)

**Impact:** Complete compromise of the decision workflow. An attacker on the same network can
silently resolve decisions, read all Slack channel content the bot can access, and post as the bot.

**Recommendation:** Add authentication middleware (e.g., bearer token from env var, mTLS, or
network policy) to all `/api/*` endpoints. At minimum, the decision resolve/dismiss endpoints
must require authentication immediately.

### C2: Unauthenticated Permission Callback Endpoint

**File:** `bot_permission.go:~line 30`

`/api/permission/callback` accepts POST requests with no authentication. Any HTTP client can
submit permission requests that are displayed as Slack interactive buttons. When a human clicks
"Allow" or "Deny", the verdict is sent back to the `channelURL` embedded in the button value.

**Impact:** An attacker can inject fake permission prompts into Slack channels, potentially
tricking operators into approving malicious actions.

**Recommendation:** Require a shared secret or HMAC signature on permission callback requests.
Validate that the request originates from a known channel server.

### C3: SSRF via Permission Callback channelURL

**File:** `bot_permission.go`, `nudge.go:SendPermissionVerdict`

The Slack button value contains `requestID|channelURL`. When a human clicks Allow/Deny, the
bridge makes an HTTP POST to `channelURL` with the verdict. This URL is set when the permission
callback is created and is stored in the Slack button value.

If an attacker can create permission callbacks (see C2), they can set `channelURL` to any
internal URL, turning the bridge into an SSRF proxy when an operator clicks the button.

Even without C2, if the channel server is compromised, the `channelURL` controls where the
bridge sends HTTP requests.

**Impact:** SSRF — an attacker can make the bridge send POST requests to arbitrary internal
services (cloud metadata endpoints, internal APIs, etc.).

**Recommendation:**
1. Validate `channelURL` against an allowlist of known coop/channel server URL patterns
2. Reject URLs pointing to cloud metadata endpoints (169.254.169.254, etc.)
3. Require the URL scheme to be http/https and the host to match expected patterns

### C4: Optional Webhook Signature Verification (Silent Bypass)

**Files:** `slack.go:308`, `jira_webhook.go:152`

Both Slack and Jira HMAC-SHA256 signature verification are **silently bypassed** when the
respective signing secrets are empty:

```go
// slack.go:308
if s.signingSecret != "" {
    // ... verify HMAC ...
}
```

```go
// jira_webhook.go (similar pattern)
if cfg.WebhookSecret != "" {
    // ... verify HMAC ...
}
```

When `SLACK_SIGNING_SECRET` or the Jira webhook secret is not configured, **all webhook
payloads are accepted without verification**. There is no warning log, no startup error,
and no health check failure.

**Impact:** If the secret env vars are accidentally omitted (misconfigured Helm chart,
missing K8s secret), the bridge silently accepts forged webhooks.

**Recommendation:**
1. Log a prominent warning at startup when signing secrets are empty
2. Consider making signing secrets required (fail-closed) with an explicit opt-out flag
3. Add a health check that reports signing verification status

---

## High Severity Findings

### H1: Wildcard CORS on Decision SSE Proxy

**File:** `api_decisions_sse.go:47`

```go
w.Header().Set("Access-Control-Allow-Origin", "*")
```

The decision SSE event proxy sets `Access-Control-Allow-Origin: *`, allowing any website
to subscribe to the real-time decision event stream.

**Impact:** Any website a user visits can silently open an EventSource to the bridge and
receive all decision lifecycle events in real time, including decision titles, options,
and resolution details.

**Recommendation:** Restrict the CORS origin to the known web UI origin(s), or require
authentication on the SSE endpoint.

### H2: Unbounded Request Body Reads

**Files:** `slack.go:302`, `jira_webhook.go:152`, `bot_autodestruct.go:176`,
`bot_unreleased.go:200`, `gitlab_sync.go:468`, `jira.go:354`, `github.go:411`,
`gitlab.go:297,352,612`

Multiple HTTP handlers use `io.ReadAll(r.Body)` without `http.MaxBytesReader` or
`io.LimitReader`:

| File | Line | Context |
|------|------|---------|
| `slack.go` | 302 | Incoming Slack webhook body |
| `jira_webhook.go` | 152 | Incoming Jira webhook body |
| `gitlab_sync.go` | 468 | Incoming GitLab webhook body |
| `bot_autodestruct.go` | 176 | Controller API response |
| `bot_unreleased.go` | 200 | Controller /version API response |
| `jira.go` | 354 | Jira API response |
| `github.go` | 411 | GitHub API response (deprecated — migrating to GitLab) |
| `gitlab.go` | 297,352,612 | GitLab API responses |

Only two call sites use `LimitReader`: `wasteland_join.go:194` (1KB) and `sse.go:166` (512B).

**Impact:** A malicious client can send an arbitrarily large request body, exhausting the
bridge's memory. For incoming webhook endpoints (slack.go, jira_webhook.go, gitlab_sync.go),
this is directly exploitable by external callers.

**Recommendation:** Wrap all incoming `r.Body` reads with `http.MaxBytesReader(w, r.Body, maxSize)`
or `io.LimitReader(r.Body, maxSize)`. Suggested limits:
- Webhook bodies: 1MB
- API responses (outgoing calls): 10MB
- File proxy: already limited to 50MB (good)

### H3: SQL Injection Risk in Wasteland Dolt Queries

**File:** `wasteland.go:82-140`

SQL queries are constructed via `fmt.Sprintf` with manual escaping:

```go
query := fmt.Sprintf(
    `SELECT ... FROM wanted WHERE status = '%s'`,
    escapeSQLString(status),
)
```

The `escapeSQLString` function only escapes single quotes (`'` → `''`). This is the
minimum escaping for SQL string literals but does not handle all edge cases (backslash
escaping in some SQL dialects, multi-byte character attacks).

While Dolt's SQL dialect may not be vulnerable to these specific attacks, the pattern
is fragile and error-prone.

**Impact:** If any caller passes unvalidated user input (e.g., from Slack slash commands)
to `QueryWanted`, `ClaimWanted`, or `SubmitCompletion`, SQL injection is possible.

**Recommendation:** Use parameterized queries if Dolt supports them, or add strict input
validation (alphanumeric + limited punctuation) before the SQL construction.

---

## Medium Severity Findings

### M1: Unbounded In-Memory Map Growth

**Files:** `dm_delivery.go` (seen, questions maps), `agents.go` (seen, taskAssignees maps),
`squawk.go` (seen map)

Several watchers maintain dedup maps that grow without bound:

| Component | Map | Growth |
|-----------|-----|--------|
| `DMDelivery` | `seen` | One entry per DM bead, never cleaned |
| `DMDelivery` | `questions` | One entry per question, never cleaned |
| `Agents` | `seen` | One entry per crash notification, never cleaned |
| `Agents` | `taskAssignees` | Cleaned on close, but grows during operation |
| `Squawk` | `seen` | One entry per squawk bead, never cleaned |

In contrast, `Dedup` (`dedup.go`) correctly implements TTL-based cleanup every 10 minutes,
and `Decisions.escalated` has hourly TTL cleanup. These are good patterns to follow.

**Impact:** Over time (days/weeks of continuous operation), these maps consume increasing
memory. In a long-running bridge pod, this could lead to OOM.

**Recommendation:** Add TTL-based cleanup to all dedup maps, following the pattern in
`dedup.go:StartCleanup`.

### M2: Goroutine Lifecycle Issues in DM Delivery

**File:** `dm_delivery.go`

`scheduleReminder` uses `time.AfterFunc` with `context.Background()`:

```go
time.AfterFunc(delay, func() {
    ctx := context.Background()
    // ... posts to Slack ...
})
```

These goroutines are not tracked and outlive any parent context. If the bridge is
shutting down, reminder goroutines may fire after the Slack client has been closed.

**Impact:** Potential panics or errors during shutdown. Goroutine leaks if reminders
are created faster than they fire.

**Recommendation:** Use a tracked goroutine pattern (e.g., `errgroup` or `sync.WaitGroup`)
and pass the parent context to enable cancellation on shutdown.

### M3: God Object: Bot Struct

**File:** `bot.go`

The `Bot` struct has 40+ fields and a single `sync.Mutex` protecting most of them.
It implements at minimum: `Notifier`, `AgentNotifier`, `DecisionRespawner`, and
`RotationNotifier` interfaces.

Key concerns:
- **Lock contention:** A single mutex for all in-memory state means unrelated operations
  (agent cards, thread mappings, decisions) contend on the same lock
- **Cognitive complexity:** Understanding which fields the mutex protects requires reading
  every method
- **Testing surface:** The Bot is hard to unit test in isolation due to its many dependencies

**Impact:** Maintenance burden and risk of deadlocks or missed lock acquisitions as the
codebase grows.

**Recommendation:** Extract subsystems into dedicated structs:
- `AgentCardManager` — agent card state and Slack updates
- `ThreadManager` — thread-agent bindings and routing
- `InteractionHandler` — Slack button/modal callbacks

### M4: Autodestruct Authorization via Environment Variable

**File:** `bot_autodestruct.go:42-53`

The autodestruct allowlist is read from `AUTODESTRUCT_ALLOWED_USERS` environment variable
on every invocation (not cached):

```go
func (b *Bot) isAutoDestructAllowed(userID string) bool {
    allowed := os.Getenv("AUTODESTRUCT_ALLOWED_USERS")
    // ...
}
```

While this works, the `AUTODESTRUCT_TOKEN` used for the controller API call is also read
from env on every call (`bot_autodestruct.go:156`). The authorization model is sound
(Slack user ID check + bearer token for controller), but env var reads on every call
add unnecessary overhead and log noise.

**Impact:** Low security impact (defense in depth is present), but configuration errors
are hard to diagnose since the allowlist is invisible to health checks.

**Recommendation:** Parse the allowlist once at startup, log the configured user IDs,
and expose the count in the readiness endpoint.

### M5: Event Dedup Across SSE Reconnect is Inconsistent

**Files:** Various watchers

Different watchers use different dedup strategies:

| Component | Dedup | TTL Cleanup | Cross-Session |
|-----------|-------|-------------|---------------|
| `Dedup` (central) | key→time | Yes (10min) | Via CatchUp |
| `Agents.seen` | key→bool | No | No |
| `Squawk.seen` | key→bool | No | No |
| `DMDelivery.seen` | key→bool | No | No |
| `Decisions.escalated` | key→time | Yes (1hr) | No |
| `SSEStream` (created/closed) | key→bool | No | Via LastEventID |

**Impact:** Inconsistent dedup means some watchers may re-fire on long-running
reconnects where the central `Dedup` has cleaned up entries but per-watcher maps
have not.

**Recommendation:** Consolidate dedup into the central `Dedup` struct with namespaced
keys (e.g., `squawk:BEAD_ID`, `dm:BEAD_ID`). This provides uniform TTL cleanup and
simplifies reasoning about dedup behavior.

---

## Low Severity Findings

### L1: Secrets Loaded from Environment Variables at Runtime

**File:** `cmd/slack-bridge/main.go`

Multiple secrets are loaded from environment variables:
`BEADS_DAEMON_TOKEN`, `SLACK_BOT_TOKEN`, `SLACK_APP_TOKEN`, `SLACK_SIGNING_SECRET`,
`GITHUB_TOKEN`, `COOP_NATS_TOKEN`, `NAMEGEN_API_KEY`, `AUTODESTRUCT_TOKEN`

This is standard K8s practice (via Secret refs in the Helm chart), but:
- Secrets are not validated at startup (empty values silently degrade)
- No secret rotation mechanism is documented

**Recommendation:** Validate required secrets at startup and fail fast with clear error
messages for missing critical secrets.

### L2: State File Performance at Scale

**File:** `state.go`

Every mutation triggers a full JSON serialize + disk write of the entire state file.
The state file contains: DecisionMessages, ChatMessages, AgentCards, ThreadAgents,
ListenThreads, NudgeThrottles, Dashboard, LastEventID.

Atomic writes (write .tmp then rename) are correctly implemented, but the full-file
write pattern means write amplification scales with total state size.

**Impact:** At scale (hundreds of agents, thousands of tracked threads), state file
writes could become a performance bottleneck.

**Recommendation:** Consider a debounced write strategy (coalesce mutations within a
window) or migrate to a lightweight embedded database.

### L3: New HTTP Client Per SSE Proxy Connection

**File:** `api_decisions_sse.go`

Each SSE subscriber connection creates a new `beadsapi.Client`, which includes an
`http.Client` and establishes a new SSE connection to kbeads.

**Impact:** Under load (many concurrent SSE subscribers), this creates many upstream
connections. The impact is limited since the web UI has few concurrent users.

**Recommendation:** Consider a fan-out pattern where a single upstream SSE connection
multiplexes to multiple downstream subscribers.

### L4: File Proxy Without Content-Type Validation

**File:** `bot_threads.go`

The `/api/slack/files/{id}` endpoint proxies file downloads from Slack with a 50MB
size limit (good), but does not validate or restrict the Content-Type of proxied files.

**Impact:** Low — the endpoint is unauthenticated (covered by C1), so the file proxy
risk is subsumed by the broader auth issue.

**Recommendation:** After fixing authentication (C1), consider adding Content-Type
restrictions or Content-Disposition headers to prevent browser-based attacks.

---

## Positive Observations

The audit also identified several areas of strong engineering:

1. **Excellent test coverage:** 86+ test files covering most subsystems, including
   edge cases like concurrent thread operations (`bot_thread_concurrent_test.go`)
   and SSE reconnect (`sse_edge_test.go`)

2. **Atomic state file writes:** `state.go` uses write-tmp-then-rename pattern,
   preventing corruption on crash

3. **SSE reconnect with Last-Event-ID:** `sse.go` correctly persists the last event
   ID and replays on reconnect with exponential backoff (1s→30s cap)

4. **Central dedup with TTL:** `dedup.go` provides a well-designed dedup mechanism
   with periodic cleanup — a good pattern for other watchers to adopt

5. **HMAC verification (when enabled):** Both Slack and Jira HMAC implementations
   use `hmac.Equal` for constant-time comparison, correctly preventing timing attacks

6. **Autodestruct authorization:** Two-factor authorization (Slack user ID allowlist +
   bearer token for controller API) with confirmation UI

7. **Router pattern matching:** `router.go` implements clean priority-based channel
   routing with proper RWMutex protection

8. **Nudge delivery:** Two-tier delivery (MCP channel first, coop PTY fallback) with
   retry and dead-agent detection prevents futile nudges

9. **Decision spawn cooldown:** `decisions.go` prevents rapid respawn loops with a
   10-minute dedup cooldown per decision bead

10. **Dashboard content hashing:** `dashboard.go` prevents redundant Slack API calls
    by hashing the rendered content and only updating on changes

---

## Summary Table

| ID | Severity | Category | Finding |
|----|----------|----------|---------|
| C1 | Critical | AuthN | No authentication on HTTP API endpoints |
| C2 | Critical | AuthN | Unauthenticated permission callback endpoint |
| C3 | Critical | SSRF | SSRF via attacker-controlled channelURL |
| C4 | Critical | AuthN | Optional webhook signature verification (silent bypass) |
| H1 | High | CORS | Wildcard CORS on decision SSE proxy |
| H2 | High | DoS | Unbounded request body reads (io.ReadAll) |
| H3 | High | SQLi | SQL injection risk in wasteland Dolt queries |
| M1 | Medium | Resource | Unbounded in-memory map growth |
| M2 | Medium | Resource | Goroutine lifecycle issues in DM delivery |
| M3 | Medium | Design | God Object: Bot struct with 40+ fields |
| M4 | Medium | Config | Autodestruct authorization via env var |
| M5 | Medium | Logic | Inconsistent event dedup strategies |
| L1 | Low | Config | Secrets not validated at startup |
| L2 | Low | Perf | State file write amplification at scale |
| L3 | Low | Resource | New HTTP client per SSE proxy connection |
| L4 | Low | Security | File proxy without Content-Type validation |

---

## Recommended Priority

1. **Immediate (P0):** C1, C2, C3 — Add authentication to HTTP API endpoints
2. **Short-term (P1):** C4, H1, H2 — Fix signature bypass, CORS, body limits
3. **Medium-term (P2):** H3, M1, M2, M5 — SQL safety, memory leaks, goroutine cleanup
4. **Backlog:** M3, M4, L1-L4 — Structural improvements and hardening
