# claudemore transparency proof (A1)

> Task ka-0000007533 (Phase 1.3, epic ka-0000006108). Instantiates acceptance
> **A1** from the R5 synthesis (`gardenio/docs/research/claudemore-r5-synthesis.md`
> §4 A1, §5.2 cost fence).

## The property

**With agents pointed at `ANTHROPIC_BASE_URL=claudemore`, the gardenio spend
pipeline produces a `total_cost_usd` / `gardenio_agent_spend_usd_total` that is
byte-identical to a litellm-direct baseline. claudemore meters no spend itself.**

claudemore *augments* litellm on the Messages-API request path (record/replay,
CC-semantic awareness); it does **not** reimplement litellm's cost layer. litellm
remains the single source of truth for spend. That is the cost fence (R5 §5.2).

## Why the property is not automatic — the two paths

The property holds only because the **write path** and the **spend read path**
resolve the account's LLM endpoint *differently*, and diverge exactly on
`proxy_url`:

| Path | Who | Resolver | `proxy_url` (claudemore) | `base_url` (litellm) |
|---|---|---|---|---|
| **Write** — agent LLM traffic | pod-spawn (`applyAccountConfig` → `ANTHROPIC_BASE_URL`), coopmux sync | `resolveAccountBaseURL(proxyURL, baseURL, default)` | **honored** (front the proxy) | fallback |
| **Read** — spend reconciliation | controller `spend_sync.go` + `reconcile_session_spend.go` | `resolveSpendReadBaseURL(baseURL, default)` | **ignored** (no proxy param exists) | used directly |

- The **write path** routes agent traffic *through* claudemore when `proxy_url`
  is set — that is what makes claudemore CC-aware and recordable. claudemore
  forwards the `x-litellm-tags: agent:<name>` header and the virtual-key auth
  unchanged, so litellm still sees, tags, and meters every request
  (`TestProxy_PassthroughTransparent`, `TestApplyAccountConfig_ProxyURLOverridesBaseURL`).
- The **read path** queries litellm **directly** at `base_url` — never at the
  proxy. claudemore is a Messages-API proxy; it does not serve litellm's
  `/key/info` or `/spend/tags`. Routing the read through claudemore would make
  spend reconciliation depend on the proxy correctly proxying litellm's admin
  endpoints, silently breaking `total_cost_usd` the moment a proxy is attached.

Because the read path *ignores* `proxy_url`, an account fronted by claudemore and
the same account after the **A2 rollback** (`proxy_url` cleared) read spend from
the **identical** endpoint. That is why metering is byte-identical to a
litellm-direct baseline whether or not claudemore is in front.

## The regression guard

The read-path rule was previously an unnamed, uncommented `return acct.BaseURL`
inside a closure in `main.go` and a base-URL trim in `spend_sync.go`. A
well-meaning cleanup that "unified" the read path onto the write-path resolver
(`resolveAccountBaseURL`) would route spend reads through claudemore and break
A1 silently. It is now a single-sourced, named, tested contract:

- **`resolveSpendReadBaseURL(baseURL, defaultURL)`** (`podspec.go`) — the read
  path's only resolver. It has **no `proxyURL` parameter**, so it *cannot* route
  through the proxy by construction. Both spend readers call it:
  - `spendSync.accountBaseURL` (`spend_sync.go`)
  - `spendReconciler.resolveBaseURL` closure (`main.go`)

## What the tests prove

| Test | File | Proves |
|---|---|---|
| `TestResolveSpendReadBaseURL` | `spend_read_baseurl_test.go` | Read resolver returns `base_url` (litellm) / global fallback, trimmed — and has no proxy rung. |
| `TestSpendTransparency_ReadPathIgnoresProxyURL` | `spend_read_baseurl_test.go` | For one account: write path → claudemore, read path → litellm; they **diverge on `proxy_url`**; post-rollback write == read. |
| `TestSpendSync_TransparencyReadsLiteLLMNotProxy` | `spend_sync_test.go` | End-to-end `syncAll` on a `proxy_url`-fronted account reads spend from litellm, **never hits the claudemore stub**, and yields a `total_cost_usd` identical to the no-proxy baseline. |
| `TestProxy_PassthroughTransparent` | `cmd/claudemore/proxy_test.go` | Write path: claudemore forwards `x-litellm-tags`, auth, body, and response byte-for-byte, so litellm meters unchanged. |

Together these close A1: **litellm sees and meters every request the same, and
the controller reads that spend from litellm directly — claudemore adds zero to
the metering path.**
