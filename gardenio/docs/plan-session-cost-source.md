# Plan: fix session cost source (F1/F2/F3) — ka-0000004228

**Decision:** ka-0000000562 — **RESOLVED** by @matthew.baker (Slack):
> "Find a way to scrape LiteLLM or use its API to get session spend totals; that would be most accurate."

**Chosen source-of-truth:** LiteLLM is authoritative. Pull real per-session
spend from LiteLLM's API rather than estimating in the agent.
**Audit:** gardenio/docs/audit-session-spend.md (ka-0000006509) · **Operator:** Matt Baker

## Problem (root cause)

All session/agent cost derives from a `response_cost` field that LiteLLM emits
only as an HTTP **header**; Claude Code does not persist it into the session
JSONL, so the parser keys on a field that is never present and cost reads `$0`
(F1). The same missing field makes coop's per-turn usage events carry no cost,
so the spend counter drops every turn (F2). At session close the controller
already fetches authoritative spend but writes it to the **account** bead, never
the session/agent (F3). LiteLLM, however, **does** know the true cost of every
request — so the fix is to read it from LiteLLM and attribute it to the session.

## The attribution crux — and how it's solved

LiteLLM's `/spend/logs` is indexed by the **virtual key = account**, which is
**shared across all concurrent sessions** on that account
(`podspec.go:955` sets one `x-litellm-api-key` per account;
`spend_sync.go:186` hashes the key). A naive window sum would mix sessions.

**Solution — per-request session tagging (LiteLLM v1.82 native):** attach a
session/agent identifier to every request as a LiteLLM spend tag, then filter
`/spend/logs` by it. LiteLLM supports this and **spend tracking from tags is
unaffected by the known tag-budget-enforcement bug** (tracking runs post-call):

- **`x-litellm-tags: agent:<BOAT_AGENT>`** — comma-separated tags that land in
  the spend-log `request_tags` field
  ([request_tags](https://docs.litellm.ai/docs/proxy/request_tags)).
- and/or **`x-litellm-spend-logs-metadata: {"session_id":"…","agent":"…"}`** —
  arbitrary JSON metadata persisted on each spend-log row
  ([request_headers](https://docs.litellm.ai/docs/proxy/request_headers)).

The agent pod already injects `ANTHROPIC_CUSTOM_HEADERS` (`podspec.go:955`), so
adding a second static header per request is a one-line spec change. With it,
`/spend/logs` filtered by `agent:<name>` + the session's `[started_at, ended_at]`
window yields **exactly that session's requests, authoritatively priced** — no
cross-session contamination.

### Validated live (2026-06-06, from an agent pod against `litellm.app.devops.acme.example`)

- **Agent pods have NO master key.** `/spend/logs` rejects the per-account
  virtual key as admin-only (`401 "Only proxy admin"`). `LITELLM_MASTER_KEY` is
  injected **only into the controller deployment** (`controller/deployment.yaml:333`).
- **Therefore reconciliation must run in the CONTROLLER, not `gb`.** The existing
  `gb` close-time `syncAccountSpendFromLiteLLM` (`session.go:572`) early-returns
  in every agent pod because the master key is empty — F3's "hook already
  present" is a **dead hook** in agent context. The controller has the master
  key, the `spendSync` daemon, and a session-close hook (`emitSessionCloseMetrics`,
  `main.go:1229`) already wired to AgentStop/Done/Kill.
- `/key/info` (virtual key works) confirms the crux: alias `gardenio3`,
  `metadata:{}`, **~$9210 cumulative account-wide spend**, no per-session split —
  tagging is the only way to slice it.
- **Not yet validated (needs controller master-key access — task T1 gate):**
  that `x-litellm-tags` round-trips into `/spend/logs.request_tags` AND is
  consumed by LiteLLM rather than forwarded to Anthropic (the proxy runs
  `forward_client_headers_to_llm_api: true`). Shipping the header blind to all
  prod agents is risky, so T1 gates the rollout.

## Implementation

### 1. Tag every request with the agent identity (`cmd/controller/podspec.go`)

Extend the `ANTHROPIC_CUSTOM_HEADERS` assignment (`podspec.go:954-955`) to append
the agent tag (Claude Code forwards `ANTHROPIC_CUSTOM_HEADERS` on every request,
and `forward_client_headers_to_llm_api: true` passes them through):

```go
hdrs := "x-litellm-api-key: Bearer " + acct.LiteLLMKey
// Tag every request so /spend/logs can be attributed per agent/session.
hdrs += "\nx-litellm-tags: agent:" + spec.AgentName
spec.Env["ANTHROPIC_CUSTOM_HEADERS"] = hdrs
```

(Confirm Claude Code's multi-header delimiter for `ANTHROPIC_CUSTOM_HEADERS`
during impl — newline vs comma — and adjust. Session-id is not known at pod-spec
time; agent-name + time-window is sufficient for attribution since one agent
runs one session at a time. If finer granularity is wanted later, coop can set a
per-session `x-litellm-spend-logs-metadata` header at session start.)

**Enable client tags on the key/config:** LiteLLM requires the issuing key to
allow client-supplied tags. Add `allow_client_tags: true` (or the config-level
equivalent) — wire via `litellm.extraSettings`/key metadata in
`helm/gardenio/templates/litellm/configmap.yaml:77-84` +
`values.yaml:5001`. Verify against the deployed v1.82.0 behavior.

### 2. Authoritative per-session reconciliation at close — IN THE CONTROLLER

> **Correction (validated live):** this CANNOT live in `gb`/`cmd/gb/session.go` —
> agent pods have no master key, so `/spend/logs` 401s there. It must run in the
> **controller**, hooked into `emitSessionCloseMetrics` (`main.go:1229`, fires on
> AgentStop/Done/Kill with `cfg.LiteLLMMasterKey` + `daemon` in scope).

Add a `reconcileSessionSpend` step in the controller session-close path. On
close:

1. Read the session bead's `started_at` and `ended_at` (controller fetches the
   bead already in `emitSessionCloseMetrics`).
2. Query `/spend/logs?api_key=<hashed key>&start_date&end_date` over the session
   window using `cfg.LiteLLMMasterKey` (mirror `spend_sync.go:192 queryPeriodSpend`).
3. Filter entries to `request_tags` containing `agent:<name>` (extend the
   `spendLogEntry` struct `spend_sync.go:178` with `RequestTags []string`).
4. Sum `spend` → authoritative session cost. Write it to **both** the session
   bead (`total_cost_usd`) and the agent rollup, replacing the JSONL estimate.
   This fixes **F1 + F3** with LiteLLM as the single source.

The gb-side `syncAccountSpendFromLiteLLM` (`/key/info`, account-level) stays as
the account number; the two now agree because both come from LiteLLM. The dead
gb close-time hook is removed/marked controller-only (task T5).

### 3. F2 — per-turn live counter / spend charts (honest scope)

`/spend/logs` is a **post-call batch** source — it gives accurate **session-close**
cost, not real-time per-turn cost. Under the chosen approach:

- **Option B-live (recommended for F2):** drive the spend counter from the
  session-close reconciled total. Replace the `response_cost`-gated increment in
  `spend_forwarder.go:159` with a session-close emission of the reconciled cost
  to `gardenio_agent_spend_usd_total` (one increment per session, labeled by
  agent/project/role/model/account). The three spend charts (`spend-by-agent`,
  `hourly-spend-rate`, `agent-cost-per-bead`) then populate from real LiteLLM
  spend, at session-close granularity rather than per-turn.
- **What is lost:** true intra-session per-turn cost resolution. If real-time
  per-turn cost is later required, that needs the proxy-callback path (audit
  option A) and is out of scope for this chosen direction. **Flag for operator:**
  confirm session-close granularity is acceptable for the spend charts (it
  matches the "most accurate totals" intent). If yes, F2 is resolved at
  session-close granularity; if per-turn is required, file a follow-up.

## Files touched

| File | Change | Task |
|---|---|---|
| `cmd/controller/podspec.go` | append `x-litellm-tags: agent:<name>` to `ANTHROPIC_CUSTOM_HEADERS` | T2 |
| `helm/gardenio/templates/litellm/configmap.yaml` + `values.yaml` | enable client tags / `allow_client_tags` | T2 |
| `cmd/controller/main.go` (`emitSessionCloseMetrics`) + new `reconcile_session_spend.go` | controller-side per-session reconciliation at close | T3 |
| `cmd/controller/spend_sync.go` | add `RequestTags` to `spendLogEntry`; tag-filtered window sum helper | T3 |
| session/agent bead write (via daemon) | write reconciled `total_cost_usd` to session bead + agent rollup | T3 |
| `cmd/controller/spend_forwarder.go` | emit reconciled session cost to counter at close (F2) | T4 |
| `cmd/gb/session.go` | remove/mark-dead `syncAccountSpendFromLiteLLM`; fix `session_usage.go:100` doc-comment | T5 |
| tests | `spend_sync_test.go`, `spend_forwarder_test.go`, `session_usage_test.go`, audit doc | T5 |

## How each finding resolves

| Finding | Resolution |
|---|---|
| **F1** session/agent cost = $0 | **Fixed** — authoritative per-session spend pulled from LiteLLM `/spend/logs` (tag-filtered) and written to session bead + agent rollup at close. |
| **F2** counter never increments / charts empty | **Fixed at session-close granularity** — counter driven by reconciled LiteLLM cost, not the missing per-turn field. Per-turn real-time resolution explicitly out of scope (would need proxy callback). |
| **F3** close-sync writes account not session | **Fixed** — new `syncSessionSpendFromLiteLLM` writes the session/agent number; `/key/info` account sync retained, now consistent (both from LiteLLM). |

## Validation

- Tag plumbing: dispatch a request through the deployed LiteLLM and confirm the
  `agent:<name>` tag appears in `/spend/logs` `request_tags` (curl with master
  key). This validates the load-bearing assumption before wiring the close path.
- `make build && make test` (controller + kbeads); `helm lint helm/gardenio/`.
- Unit: tag-filtered window sum isolates one agent's spend across a fixture with
  two concurrent agents on the same key; close path writes session + agent cost.
- E2E spot-check: a single agent session's reconciled `total_cost_usd` matches
  the LiteLLM UI / `/spend/logs` for that agent+window within rounding.

## Open items needing confirmation during impl

1. `ANTHROPIC_CUSTOM_HEADERS` multi-header delimiter in the deployed Claude Code.
2. Exact `allow_client_tags` enablement for v1.82.0 (key metadata vs config).
3. Operator sign-off that **session-close** spend granularity satisfies F2's
   charts (vs true per-turn) — see §3.
