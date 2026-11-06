# ADR 0006 — Recording OpenAI/Codex traffic: a claudemore-style capture path that respects the `auth.json` contract

> Status: **Proposed** (design note, nothing built; §8 lists what the operator may overturn)
> Bead: gd-sssOUVd1g6, a scope-out of ADR 0004 §11 (originally gd-0D8aGbRwYp)
> Plan: gd-8s8FdZu8UA "Bring Codex to gardenio"; epic gd-J9Wkw5HjF6 "4. Rollout". Operator decision gd-MxiDBDi9UM chose "start".
> Builds on: [ADR 0004](0004-codex-harness.md) §5 (credential contract), §9 (persistence); [codex-subscription-keeper.md](../../gardenio/docs/codex-subscription-keeper.md); the claudemore research ([R5 synthesis](../../gardenio/docs/research/claudemore-r5-synthesis.md)).

Evidence tags as in ADR 0004: **[SPIKE]** observed by running the real `codex` binary (here: `codex-cli 0.162.1` against a local mock, never a real ChatGPT turn). **[BIN]** read from the binary's strings, never executed. **[REPO]** read in this repo. **[UNVERIFIED]** inference; each has a row in §7.

---

## 1. Context

claudemore ([REPO] `gardenio/controller/cmd/claudemore`, `internal/claudemore`) is a thin Anthropic-Messages reverse proxy that sits behind `ANTHROPIC_BASE_URL` (account `proxy_url`), forwards to litellm, and in RECORD mode tees each exchange into a redacted, canonicalised, S3-stored cassette. REPLAY serves cassettes at $0 for e2e. It gives gardenio three things Claude agents rely on: a **wire audit** (reasoning plaintext, `AuditCassette`), a **drift canary** (`CompareToLive`), and **deterministic replay**.

Codex agents get none of it. With ChatGPT-subscription auth (ADR 0004 §5) Codex talks straight to `chatgpt.com/backend-api/...` with the access token from `$CODEX_HOME/auth.json`; there is no litellm, no `proxy_url`, no base URL gardenio controls. ADR 0004 §11 listed this as a v1 non-goal. This ADR decides how to close it without breaking the credential contract that ADR 0004 §5 and the keeper fixed.

The hard constraint, restated: **the refresh token never leaves the controller, the access token lives only in the pod's `auth.json` and in flight to OpenAI, and nothing that persists (cassette, log, metric, bead) may contain either.**

## 2. Decisions at a glance

| § | Decision |
|---|---|
| A | Capture point: a **reverse proxy selected by Codex's `chatgpt_base_url`**, not a TLS-intercepting egress proxy. |
| B | Placement: an **in-pod, loopback-only tap** that redacts before anything leaves the pod, not an in-cluster Service. |
| C | Credentials: the tap is a **pure pass-through for `Authorization` / `chatgpt-account-id`**. It holds no `auth.json`, no refresh token, no Secret, and never injects credentials. |
| D | Scope: **RECORD only** in v1 (audit + drift). REPLAY is a separate decision after a real-turn corpus exists. |
| E | Redaction: the shared chokepoint must gain **JWT and `Bearer <token>` rules first**. Today it has neither. |
| F | Failure and rollback: recording is **fail-open**, the tap is supervised, and rollback is "stop writing `chatgpt_base_url`". |
| G | Phasing: a small build is split out (§6). This note ships alone. |

## 3. A. Where to capture

Three candidates.

1. **Reverse proxy via `chatgpt_base_url` (chosen).** `chatgpt_base_url` is a top-level `config.toml` key [REPO: it is in `codexDeniedKeys`, `cmd/gb/setup_codex.go`, because it "redirects requests or credentials"]. [SPIKE] with `chatgpt_base_url = "http://127.0.0.1:18099/backend-api/"`, every ChatGPT-backend call moved to that host: `GET wham/accounts/check`, `GET wham/settings/user`, `GET plugins/featured`, `GET ps/plugins/{installed,list,suggested/codex}`, `POST ps/mcp`, `POST codex/analytics-events/events`. Each carried `authorization`, `chatgpt-account-id`, `originator: codex_exec` and `user-agent: codex_exec/0.162.1`. Codex refreshes tokens at `auth.openai.com` [BIN], a different host, so the tap never sees the refresh exchange. This is exactly claudemore's shape: a base-URL override that keeps the real client, the real hooks and the real tool loop.
2. **TLS-intercepting egress proxy** (`HTTPS_PROXY` + a gardenio CA via `CODEX_CA_CERTIFICATE`, which Codex honours [BIN] and ADR 0004's env table already lists). Sees everything including endpoints that ignore the base URL, and websockets, with no dependence on Codex config keys. Rejected for v1: it needs a CA private key in the cluster, decrypts the bearer for **all** of the pod's egress (git, package indexes, other APIs), and widens the blast radius of a compromised proxy from "ChatGPT calls" to "everything the pod says over TLS". Keep as the fallback if A leaves a hole that matters (§7 R3).
3. **Protocol-level tap in coop** (record what the app-server driver already sees: `item/*`, `turn/*`, `thread/tokenUsage/updated`; ADR 0004 §9 already persists the rollout and normalised events). Costs nothing new and carries no wire credential at all. It is **not** a replacement: it is Codex's view, not the wire, so it cannot back REPLAY, cannot show a request Codex built but the harness never surfaced, and cannot detect backend drift. **If the only need is human audit of what an agent said and did, this already exists and this ADR is unnecessary**; the proxy earns its keep only for wire-level drift detection and replay. Stated so the operator can stop here.

### A.1 What the proxy must handle

- **Path allowlist for recording**, pass-through for everything else. Only the model-turn endpoint (`/backend-api/codex/responses`, incl. a compact variant if the binary has one [UNVERIFIED R1]) is recorded. `wham/*`, `plugins/*`, `ps/*` and analytics are forwarded and never stored; they carry account metadata, not reasoning.
- **SSE streaming.** The Responses API streams; reuse claudemore's `FlushInterval: -1` tee ([REPO] `proxy.go`) so deltas are never buffered ahead of the client. Capture is a copy, never a gate.
- **Websockets.** [SPIKE] on a path where the socket fails, Codex retries `wss` five times (~7 s) per attempt and then prints "Falling back from WebSockets to HTTPS transport". The binary contains a `ResponsesWsRequest` / `response.create` message with `previous_response_id` [BIN], i.e. the websocket transport sends **incremental** requests, which are poor cassette keys and a second wire format to record. v1 therefore answers the upgrade with a fast refusal so Codex settles on HTTPS, and measures the cost (§7 R4). The `responses_websockets*` feature flags now report `removed` in `codex features list` [SPIKE], so there is no supported switch to turn the socket off; the refusal is on us.
- **Routing discovery.** [SPIKE] the model call is gated on `wham/accounts/check`: a 401 produced "workspace routing discovery unauthorized (401)" and an empty 200 produced "selected workspace missing from routing discovery"; no `codex/responses` request was ever made. A recording proxy must therefore be a true forwarder to the real backend, not a mock, and the first build step is a real-turn capture to learn the actual model-call path and headers (§7 R1). This note does not assume them.

## 4. B. Where the tap runs

**Decision: in the agent pod, listening on `127.0.0.1` only.** `gb init` starts it before coop; `gb setup codex` writes `chatgpt_base_url = "http://127.0.0.1:<port>/backend-api/"` into the generated `config.toml` when `CODEX_RECORD=1` (env set by the controller from role/project config, off by default). The key is written by gardenio's own generator; the `codexDeniedKeys` strip on `codex-config` beads stays, so an agent-writable config bead still cannot redirect Codex.

Why not an in-cluster Service the way claudemore runs: the Authorization header on this hop is a **ChatGPT account access token**, which unlike a litellm virtual key grants access to the owner's account, and the claudemore hop is plain HTTP inside the cluster. Loopback keeps the bearer inside the pod, where it already lives in `auth.json`. The tap needs no listener exposure, no NetworkPolicy and no Service.

The tap redacts and canonicalises **in the pod**, then ships only finished cassettes. Shipping path, in order of preference (implementation question, §7 R6): (1) write cassette files beside the rollout and let coop's existing snapshot upload carry them (same bucket/prefix scheme as ADR 0004 §9, no new pod credential); (2) POST them to an in-cluster claudemore ingest endpoint, which is safe in cleartext because they are already redacted. Not chosen: giving the agent ServiceAccount write access to the cassette bucket, which widens agent-pod IAM and is a lane (helm/IRSA) escalation this note should not trigger.

Reuse: `internal/claudemore` (`Cassette`, `CassetteStore`, `RedactBytes`, `AuditCassette`) is importable from `gb`; `cmd/claudemore/proxy.go` is the reverse-proxy to lift. The Anthropic-specific part is `CanonicalizeRequest` (extracts `model`, strips volatile fields, header allowlist for the hash); Codex needs its own canonicaliser (§5.3).

## 5. C/E. The credential contract, concretely

### 5.1 What the tap may and may not hold

| Item | Tap |
|---|---|
| `gardenio-codex-auth` (real login, refresh token) | **Never.** Controller-only RBAC, unchanged. The tap runs in the agent pod, which cannot read it. |
| `gardenio-codex-auth-session` / `$CODEX_HOME/auth.json` (placeholder refresh token) | **Does not read the file.** It sees the access token only as the `Authorization` header Codex sends. |
| Access token in flight | Forwarded byte-for-byte. Not stored, not logged, not in a metric label, not in an error string. |
| `chatgpt-account-id` | Forwarded. Not stored (it is an account identifier). |
| Token rotation | No tap action. The keeper pushes a fresh session document into the pod; Codex reads it and sends the new header. A stateless tap needs no reload, which is why we do not inject credentials. |

Rejected: **the tap injecting the bearer itself** (so Codex would not need `auth.json`). It would make the tap a credential holder, require it to track rotation, and turn a recorder bug into an auth outage.

### 5.2 Redaction gap that must be fixed before the first cassette

[REPO] `internal/claudemore/redact.go` redacts `key=value` shapes and bare `sk-`/`ghp_`/`glpat-`/`github_pat_`/`vk-` tokens. It has **no JWT rule**, and the keyword rule requires `:` or `=` after `bearer`, so `Authorization: Bearer eyJ...` is not caught by the body pass. A ChatGPT access token is a JWT (`eyJ...` three dot-separated segments; the keeper reads its `exp` [REPO]). Verified in this task by running `RedactBytes` and `ContainsSecretShape` on `Authorization: Bearer eyJ...`: the bytes come back unchanged and the shape check returns false. Today that would survive `RedactBytes` verbatim if it ever appeared in a captured body, a model-visible tool output, or a header value. (The k3s log shipper already redacts JWTs and `Bearer`, per `gardenio/docs/k3s-cluster.md`; the cassette chokepoint has not caught up.) Required, as the **first** build step and a standalone fix independent of Codex:

- JWT shape: `\beyJ[A-Za-z0-9_-]{8,}\.[A-Za-z0-9_-]{8,}\.[A-Za-z0-9_-]*`.
- `Bearer\s+<token>` with whitespace as the delimiter.
- A matching `ContainsSecretShape` update, since the recorder's belt-and-braces check uses it.

### 5.3 What a Codex cassette stores

Allowlist, not denylist (as claudemore already does for response headers):

- **Request headers kept:** `content-type`, `accept`, `originator`. Dropped: `authorization`, `chatgpt-account-id`, `cookie`, `session-id`, `x-codex-turn-metadata` (contains `installation_id`, `thread_id`, `turn_id` [SPIKE]), `user-agent` beyond the product/version token.
- **Request body:** redacted whole through `RedactBytes`. Hash key drops volatile fields (`prompt_cache_key`, `client_metadata`, per-turn ids, and, for the websocket transport if ever recorded, `previous_response_id`).
- **Response:** status, `content-type`, and the SSE body redacted whole. Reasoning items arrive as `encrypted_content` blobs [UNVERIFIED R2]; store them as-is only if redaction cannot alter them, otherwise drop the field and record that it was dropped, so replay cannot silently differ.
- **Identity for audit:** the `harness=codex` agent name, resolved from the pod, not from headers.

## 6. D/F/G. Scope, failure, rollback, phasing

- **RECORD only.** REPLAY needs a Codex request canonicaliser whose hash is stable across `previous_response_id`, `prompt_cache_key` and turn metadata, plus a corpus recorded from real subscription turns. Neither exists. Decide REPLAY after the recorder has produced real cassettes.
- **Cost fence.** Subscription auth means `total_cost_usd` stays 0 ([REPO] ADR 0004 §5); the tap meters no spend and must not touch `reconcile_session_spend.go`'s skip of `provider=openai`. The claudemore transparency property (spend identical with or without the proxy) holds trivially.
- **Fail-open recording.** A cassette write error, queue overflow or oversize body (claudemore's 8 MiB cap) drops the cassette and logs a count; it never delays or alters the forwarded bytes. A failed upstream is forwarded as-is and not recorded, as claudemore already does for non-2xx.
- **The tap is a single point of failure for model calls**, which claudemore's in-cluster form is not for Claude (clearing `proxy_url` is instant). Mitigations: supervised by `gb init` with restart; `codex doctor` stays the readiness probe; the listener binds before coop starts.
- **Rollback:** unset `CODEX_RECORD` and restart the pod; `config.toml` is regenerated wholesale at every start (ADR 0004 §E, `codex-config-injection.md`), so `chatgpt_base_url` simply disappears and Codex goes direct. No credential state to unwind.
- **Build slices**, each small and independently reviewable:
  1. Redaction: JWT + `Bearer` rules and tests in `internal/claudemore` (Codex-independent, can land now).
  2. Real-turn capture to settle R1/R2/R4 (needs a logged-in pod; an `infra`/operator step, not CI).
  3. `gb` loopback tap: lifted proxy, path allowlist, Codex canonicaliser, local cassette files; `setup_codex.go` writes `chatgpt_base_url` when `CODEX_RECORD=1`.
  4. Shipping path (§4) and an `AuditCassette` equivalent for Responses SSE.
  5. Optional: REPLAY, as its own ADR.
- **Acceptance test shape (slice 3).** A hermetic forwarder test in the style of `TestProxy_PassthroughTransparent`: feed a canary JWT as `Authorization` and a canary string in a tool output; assert the upstream received both byte-for-byte, and that the canary JWT appears in **no** cassette, log line or error. This is the test that enforces "never logs credentials".

## 7. Open risks (UNVERIFIED), each with the step that closes it

| # | Risk | Closed by |
|---|---|---|
| R1 | The real model-call path and headers (`codex/responses`?), whether it honours `chatgpt_base_url` and not a separate base, and what `wham/accounts/check` must return. [SPIKE] only reached routing discovery; `CODEX_APP_SERVER_CHATGPT_BASE_URL` [BIN] suggests app-server mode has its own override. | Slice 2, a forwarder run against the real backend from a logged-in pod. |
| R2 | Whether `encrypted_content` and other opaque fields survive redaction unchanged, and what a Responses SSE audit can extract (reasoning summaries). | Slice 2 corpus, then slice 4. |
| R3 | Endpoints that ignore `chatgpt_base_url` (e.g. `api.openai.com` websocket seen in the older spike, realtime). If any carries model traffic, A has a hole and option 2 is back on the table. | Slice 2: egress log of a full turn with the tap in place. |
| R4 | Websocket refusal: does a fast `426`/close make Codex fall back to HTTPS in well under the ~7 s flaky-path cost, and is the fallback sticky for the session or retried per turn. | Slice 2, measured. |
| R5 | `chatgpt_base_url` over `http://127.0.0.1`: does Codex require https for this key, or attach credentials differently to a non-OpenAI host. [SPIKE] it sent the bearer to plain-http loopback, so likely fine. | Slice 2. |
| R6 | Whether coop's snapshot uploader can carry an extra file kind, or the in-cluster ingest path is needed. | Slice 4 code read. |
| R7 | Codex versions: `chatgpt_base_url` and the endpoint set may change on a `codex.sh` bump. | A contract test in slice 3 that loads the generated config with `codex --strict-config` (already done for the golden) and a recorded-path allowlist that fails closed on unknown paths. |

## 8. What the operator may still overturn

- **Whether to build at all**: option 3 in §3 already gives audit. The proxy is justified only by wire-level drift detection and replay.
- **B**: in-pod loopback tap over an in-cluster claudemore mode. The latter reuses the deployed binary and S3 wiring but puts a ChatGPT bearer on a cleartext in-cluster hop.
- **A**: a TLS-intercepting egress proxy if the owner prefers complete coverage over a small credential surface.
- **D**: starting with REPLAY-first, if a deterministic Codex e2e tier is the real motivation.

## 9. Consequences

- ADR 0004 §11's claudemore non-goal is superseded by this ADR for the design; nothing is built, so the v1 non-goal stays true in practice.
- The JWT/`Bearer` redaction gap in §5.2 is real today for the existing Anthropic cassettes (a JWT in a captured body would persist) and is worth fixing regardless of Codex. Filed as gd-A0vrgLrg0U.
- No helm, RBAC, deploy-values or terraform change is required by slices 1 to 4 under the preferred options; the cluster-side alternative in §4 (ingest endpoint) and any bucket/IAM change are lane escalations to be filed from the slice that needs them.
