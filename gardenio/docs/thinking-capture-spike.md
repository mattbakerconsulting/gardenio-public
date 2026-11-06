# Thinking-trace PLAINTEXT capture spike (ka-0000004553)

> **Moved (gd-Wtw4k6fKwr).** This was `gardenio/spikes/thinking-capture/README.md`.
> The shipped callback is
> [`gardenio/helm/gardenio/files/litellm/litellm_thinking_callback.py`](../helm/gardenio/files/litellm/litellm_thinking_callback.py),
> which has moved on from the spike copy. The spike's own files (the original
> callback, `verify_e2e.py`, `test_extract.py`) were deleted from `main`; recover them
> from tag `archive/clutter-pre-delete-2026-10-28`.

**Deliverable:** a working capture that emits **non-empty thinking plaintext**
onto a NATS subject the beads-viewer consumer (ka-0000007528) can read, plus
documentation of which source was chosen and why.

**Status: ✅ proven end-to-end on real infra.** A live thinking completion
through the production LiteLLM proxy this agent uses
(`https://litellm.app.devops.acme.example`) was captured by the callback and
delivered as **779 chars of reasoning plaintext** on
`coop.events.messages.<actor>.thinking`. See "Verification" below.

---

## TL;DR — chosen source

**Source A: a LiteLLM logging callback on the proxy.** It is the only source
that (a) sees the thinking *plaintext*, (b) requires **zero changes** to coop,
Claude Code, or agent pods, and (c) is the intentional interrelation with the
litellm-in-cluster epic (ka-0000000560). The callback
(`litellm_thinking_callback.py`) publishes an envelope byte-compatible with
coop's `NatsForwarder`, so the consumer needs no new code path.

---

## The problem this spike corrects

The telepathy v1 plan (epic ka-0000007154) assumed coop already forwards
thinking plaintext and the only work was lifting the 512-byte truncation cap
(`coop/crates/cli/src/transport/nats_forward.rs:30`). **That is wrong.**

coop's `.thinking` NATS events are fed by the **Tier-2 LogWatcher of Claude
Code's persisted session-log JSONL** (`nats_forward.rs:121`, `extract_thinking`
at `:225`, fed from `driver/claude/stream.rs` `LogDetector`). Anthropic
**strips reasoning plaintext on persist**, keeping only the crypto
`signature`. Every persisted thinking block looks like:

```json
{"type": "thinking", "thinking": "", "signature": "EpICCmMIDhgC..."}
```

Un-truncating an empty string yields nothing. Lifting the cap is a no-op for
thinking.

### Empirical confirmation (reproduced in this spike)

- Report ka-0000001698 / decision ka-0000001028: 24/24 persisted thinking blocks
  signature-only across prod logs; 0 non-empty matches anywhere.
- Reproduced here against **this agent's own live session log**
  (`~/.claude/projects/.../<session>.jsonl`): **7/7 thinking blocks
  signature-only, `thinking=""`, all carrying a signature.**

The plaintext exists only transiently in the **live model response stream**
(Anthropic SSE `thinking_delta`), before persistence.

---

## Source comparison

| Source | Sees plaintext? | Changes needed | Verdict |
|---|---|---|---|
| **(A) LiteLLM logging callback** | **Yes** — proxy sees the raw streamed response incl. `thinking_delta`; LiteLLM flattens it to `message.reasoning_content` / `message.thinking_blocks` | **None** to coop/Claude/pods — add one callback + NATS env on the proxy | **CHOSEN** |
| (B) Claude Code `--print --output-format stream-json` stdout (coop Tier-3) | Yes — verified: stdout carries `thinking_delta` + a full `thinking` block (1064 chars in a test run) | **Large** — prod runs **interactive PTY** (`claude --dangerously-skip-permissions`, Tier-2; `init_agent_loop.go:462`), not `--print`. Would need a sidecar/print path or a second non-interactive Claude invocation | Viable fallback, high cost |
| (C) A Claude Code hook/config that persists thinking text | — | — | **Does not exist today**; no setting re-injects stripped reasoning plaintext |

### Why A over B

Both A and B were **empirically confirmed to carry the plaintext** (see
Evidence). A wins because prod already routes 100% of agent inference through a
LiteLLM proxy (`ANTHROPIC_BASE_URL`, `podspec.go` / this agent's env), so the
capture point already exists and is shared by every agent. B would require
changing how every agent runs Claude (PTY → print) or running a parallel
non-interactive Claude, which is invasive and doubles inference.

The one coupling to note: A's capture lives on the proxy, so it lands naturally
as part of the **litellm-in-cluster migration (ka-0000000560)**. It works on the
**external** proxy today too (this spike proves it), so it is not blocked on
that migration — it just becomes a first-class in-cluster feature once the proxy
is ours.

---

## Files

Spike files, kept only in tag `archive/clutter-pre-delete-2026-10-28`:

| File | What |
|---|---|
| `litellm_thinking_callback.py` | The capture. A LiteLLM `CustomLogger` that extracts reasoning plaintext on `async_log_success_event` and publishes to `{prefix}.messages.{actor}.thinking`. Import-safe; lazy NATS connect. |
| `verify_e2e.py` | End-to-end proof: live thinking call → proxy → callback → NATS → subscriber reads back non-empty text. |
| `test_extract.py` | Offline unit tests (no network) for extraction, actor resolution, the signature-only failure shape, and envelope parity. 8/8 pass. |

---

## Output envelope (consumer-compatible)

Matches coop `NatsForwarder.publish_event` (`nats_forward.rs:165-198`) so the
beads-viewer subscriber (ka-0000007528) reads it with no new code:

```json
{
  "event_type": "thinking",
  "text":        "<full reasoning plaintext>",   // NON-EMPTY — the whole point
  "full_length": 779,
  "actor":       "gardenio-...",                  // from x-litellm-tags agent:<name>
  "session_id":  "<litellm call id>",
  "ts":          1749680000000,
  "source":      "litellm-callback"              // provenance vs coop's log path
}
```

`actor` comes from the `x-litellm-tags: agent:<name>` header gardenio already
injects on every request (see this agent's `ANTHROPIC_CUSTOM_HEADERS`), so
per-agent routing is identical to coop's.

---

## Verification

### Offline unit tests
```sh
python3 test_extract.py     # 8/8 passed (from the archive tag's spike dir)
```

### End-to-end (run in an agent pod)
```sh
SSL_CERT_FILE=$(python3 -c 'import certifi;print(certifi.where())') \
  python3 verify_e2e.py      # from the archive tag's spike dir
```
Observed result:
```text
subject:        coop.events.messages.<actor>.thinking
event_type:     thinking
source:         litellm-callback
full_length:    779
text non-empty: True
text sample:    'A prime number is a natural number greater than 1 that has no positive
                 divisors other than 1 and itself.\n\nFor 13: ...'
PASS: non-empty thinking PLAINTEXT delivered on the consumer subject.
```
For the identical turn the persisted-log path emits `text=""` — this is the win.

---

## Integration path (for the in-cluster litellm work, ka-0000000560)

1. Mount `litellm_thinking_callback.py` on the proxy's `PYTHONPATH` and add
   `nats-py` to the proxy image.
2. Proxy config:
   ```yaml
   litellm_settings:
     callbacks: ["litellm_thinking_callback.instance"]
   ```
3. Proxy pod env (same NATS coords coop gets via `podspec.go:631`):
   `COOP_NATS_URL`, `COOP_NATS_PREFIX` (default `coop.events`),
   `COOP_NATS_TOKEN_FILE` / `COOP_NATS_TOKEN`.
4. The beads-viewer consumer (ka-0000007528) already reads
   `coop.events.messages.*.thinking` — non-empty events now flow.

**Open follow-ups** (out of scope for this spike, file as tasks):
- NATS max-payload: chunk or cap very long traces (coop caps at 512B; this
  callback sends full text — set a generous cap or chunk for the consumer).
- Decide coexistence with coop's existing (empty-text) `.thinking` events —
  the `source` field disambiguates; the consumer may prefer
  `source:litellm-callback` and ignore empty `source` (coop/log) thinking.
- When litellm goes in-cluster, fold this callback into the proxy chart.
