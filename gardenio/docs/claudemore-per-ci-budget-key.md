# Per-CI scoped LiteLLM virtual key (budget cap)

**Scope:** Phase 0 claudemore guardrail (task ka-0000000823, epic ka-0000000278).
Closes **R4-COST-1** (unbounded per-run model spend) and **R4-SEC-1** (first real
model credential in the CI trust boundary). Design source:
[`research/claudemore-r4-derisking.md`](research/claudemore-r4-derisking.md) §2/§5.

## TL;DR

A per-CI run gets a LiteLLM virtual key **minted with a `max_budget` +
`budget_duration`** at namespace-create, and **deleted** at reap. LiteLLM
hard-`429`s that key the moment its cumulative spend crosses the budget, so the
worst-case spend of a runaway real-Claude-Code run is **bounded regardless of
agent behavior** — an in-run hard stop at the proxy, not the lagging 5-min spend
reconcile. Deleting the key at teardown makes a leaked tracking credential
short-lived and revocable.

The write path is the new **`gb litellm-key`** command
(`gardenio/controller/cmd/gb/litellm_key.go`) — the mint/update/delete counterpart
to the pre-existing read-only `/key/info` path (`gb usage`, `spend_sync.go`).

## Background — what existed vs. what was missing

Before this change the controller only ever **read** LiteLLM keys: `spend_sync`
polls `GET /key/info` for spend, and `gb usage` renders it. `max_budget` was
purely informational in Go — there was **no `/key/generate` or `/key/update`
write path anywhere in gardenio/kbeads/coop** (R4 §2). So an ephemeral CI run had
no way to obtain a spend-capped, revocable key; a runaway agent could spend
without an in-run ceiling.

This wires the three LiteLLM admin verbs that give a per-CI run a scoped key:

| Verb | LiteLLM endpoint | Used at |
|---|---|---|
| `gb litellm-key mint`   | `POST /key/generate` | namespace-create |
| `gb litellm-key update` | `POST /key/update`   | budget adjustment |
| `gb litellm-key delete` | `POST /key/delete`   | reap / teardown |
| `gb litellm-key info`   | `GET /key/info`      | verify (reuses read path) |

The master/admin key stays **controller/operator-side only** — mint out-of-band
and inject only the resulting scoped virtual key into the CI agent path. The
master key must never enter the agent pod (R4-SEC).

## Lifecycle

```bash
# ── at namespace-create — mint a $5 / 24h key scoped to Haiku ──
export LITELLM_URL=http://gardenio-litellm:4000
export LITELLM_MASTER_KEY=sk-master-...     # controller-side secret
VKEY=$(gb litellm-key mint \
  --alias ci-<instance-id> \
  --max-budget 5 \
  --budget-duration 24h \
  --models claude-haiku-4-5)
# inject $VKEY into the run as the agent's virtual key (NOT the master key)

# ── during the run — LiteLLM enforces the cap; 429 once spend >= max_budget ──

# ── at reap/teardown — revoke the key ──
gb litellm-key delete --key "$VKEY" --ignore-missing
```

- `--max-budget` is **required and must be > 0**: an unbudgeted key defeats the
  guardrail, so `mint` refuses it.
- `--budget-duration` (e.g. `24h`, `30d`) sets a rolling reset window; omit it for
  a lifetime-cumulative cap.
- `--models` restricts the key to an allowlist so a runaway cannot reach a pricier
  model group than intended.
- `--ignore-missing` on `delete` makes teardown idempotent: an already-reaped key
  (404 from LiteLLM) is treated as success, so a re-run never fails.

`mint` prints the raw `sk-...` key on **stdout** and a summary on **stderr**, so
`VKEY=$(gb litellm-key mint ...)` captures only the key.

## Acceptance — deliberate runaway 429s at the cap

The bead's acceptance is: *"a deliberate runaway test confirms litellm 429s at the
cap."* Two levels assert it:

1. **Unit** — `TestBudgetCap429AtCap` (`litellm_key_test.go`): mints a capped key
   against a fake proxy that 429s once modeled spend crosses `max_budget`, drives
   calls until the 429, and asserts it lands at the expected finite point. Locks
   the contract that the cap is enforced at the proxy, in-run.

2. **End-to-end** — `tests/e2e/scripts/test-litellm-budget-cap.sh`: mints a
   tiny-budget key against a **real** LiteLLM proxy, drives real `/v1/messages`
   turns until a **budget-shaped** HTTP 429, then deletes the key. Run it against
   the in-cluster or devops proxy:

   ```bash
   LITELLM_URL=http://gardenio-litellm:4000 \
   LITELLM_MASTER_KEY=sk-master-... \
   gardenio/tests/e2e/scripts/test-litellm-budget-cap.sh --model claude-haiku-4-5 --budget 0.05
   ```

   Exit 0 = the key 429'd at the cap and was cleaned up. Non-zero = the cap did
   not engage (a COST guardrail regression) or setup/teardown failed. The script
   always deletes its key on exit (even on failure), so a runaway never leaks a
   live credential.
