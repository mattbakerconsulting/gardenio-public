# LiteLLM Stage-2 Migration Runbook (per-account base_url cutover)

Operator-gated cutover of **live prod account beads** from the EXTERNAL LiteLLM
proxy (`https://litellm.app.devops.acme.example`) to the in-cluster proxy
(`http://gardenio-litellm:4000`).

- **Epic:** ka-0000000560 (SENSITIVE — due caution)
- **Stage-1 (done):** ka-0000004799 — stood the in-cluster proxy up in prod +
  pointed the GLOBAL controller `LITELLM_URL` at it. Decision gate ka-0000006287.
- **Stage-2 (this doc):** ka-0000000158 — flip each account bead's per-account
  `base_url` + `litellm_key`, because per-account `base_url` takes precedence
  over the global URL (`podspec.go:970-977`, fix ka-0000003221). Until the
  per-account flip, live inference / spend_sync / `gb usage` still go external.

Tooling: [`gardenio/scripts/litellm-stage2-migrate.sh`](../scripts/litellm-stage2-migrate.sh)
(`snapshot` / `seed` / `verify` / `flip` / `rollback`).

---

## Why this is a hard gate

The controller treats an account with `base_url` set but an **empty**
`litellm_key` as UNUSABLE (`accountInfoUnusable`, `main.go:1754`) — every agent
on that account 401s and parks idle. The in-cluster proxy ENFORCES auth (401 on
no-key / bogus-key / even a forwarded OAuth bearer; only a master/virtual key →
200). So the flip and the rollback **must each write `base_url` AND
`litellm_key` together, atomically, per bead.** A half-write strands a whole
team's agents.

One pod becomes the sole inference egress for any migrated account. A
CrashLoop on that pod takes down all migrated traffic at once (external absorbed
that risk in a separate cluster). Hence: green non-prod bake + tested rollback +
fresh operator decision-gate before any prod account flip.

## Current prod state (verified 2026-06-14, this run)

| Check | Result |
|---|---|
| `gardenio-litellm` Deployment | **1/1 Running**, 0 restarts; Service :4000 |
| `/health/readiness` (in-pod) | `{"status":"connected","db":"connected",...}` v1.82.0 |
| Master-key match | `litellm-master-key` keys `master-key` == `token` == controller's `LITELLM_MASTER_KEY`. Admin API auth works. |
| Salt cycle | throwaway `/key/generate` → `/key/info` round-trip → `/key/delete` clean (404 after). No orphan. |
| Controller (`gardenio-agents`) `LITELLM_URL` | `http://gardenio-litellm:4000` (in-cluster, from Stage-1) |
| In-cluster served models | OAuth groups only: `claude-opus-4-{6,7,8}-oauth`, `claude-sonnet-4-6-oauth`, `anthropic/claude-haiku-4-5`. **No bedrock.** |
| Account beads | **9 open, ALL external** (see scope table). |
| `BEADS_EMBED_URL` (gardenio-beads) | external, `bedrock-titan-embed-v2`. Unchanged. |

## Scope: which accounts migrate this stage

| Bead | Title | Model | This stage? |
|---|---|---|---|
| ka-0000006686 | gardenio | claude-opus-4-8-oauth | ✅ flip |
| ka-0000006008 | gardenio2 | claude-opus-4-8-oauth | ✅ flip |
| ka-0000001761 | gardenio3 | claude-opus-4-8-oauth | ✅ flip |
| ka-0000002464 | first-mate-haiku | claude-haiku-4-5 | ✅ flip (canary — lowest traffic) |
| ka-0000003499 | matthewcbaker0122 | claude-opus-4-8-oauth | ✅ flip |
| ka-0000000611 | matthewbaker-acme | claude-opus-4-8-oauth | ✅ flip |
| ka-0000006759 | matthewbaker1110 | claude-opus-4-8-oauth | ✅ flip |
| ka-0000005691 | acme-shared | claude-opus-4-6-oauth | ⚠️ flip-with-care — already keyless+base_url (pre-existing UNUSABLE); seeding it actually *fixes* that |
| ka-0000003488 | litellm-bedrock | bedrock-claude-opus-4-7 | ❌ **stays external** — in-cluster proxy serves no bedrock model group |

**Embeddings** (`BEADS_EMBED_URL`, `bedrock-titan-embed-v2`) also **stay external**
this stage — same reason (in-cluster proxy needs `litellm.embedding.enabled=true`
+ a bedrock Titan model + Bedrock creds, not yet enabled in prod values; rwx-green
under ka-0000007591 but a separate stage).

## Prep (non-destructive — done this run, repeatable)

```bash
# 1. Capture exact prior base_url + litellm_key for every open account bead.
LEDGER=/path/to/litellm-stage2-rollback-ledger.json \
  gardenio/scripts/litellm-stage2-migrate.sh snapshot

# 2. Mint a per-account virtual key in the IN-CLUSTER litellm DB and store the
#    plaintext in K8s secret gardenio-litellm-stage2-vkeys (one data key per
#    account-bead-id). Skips bedrock. Round-trips each key via /key/info.
#    DOES NOT touch any account bead — live traffic stays external.
gardenio/scripts/litellm-stage2-migrate.sh seed

# 3. Re-verify the seeded keys resolve.
gardenio/scripts/litellm-stage2-migrate.sh verify
```

Prep result this run: **8 OAuth keys minted + verified, spend=0; secret
`gardenio-litellm-stage2-vkeys` holds all 8; all 9 beads still external.**
Do **not** rotate `LITELLM_SALT_KEY` after this — it orphans the encrypted key
rows (ka-0000006378).

## Staged flip (GATED — requires resolved `go` decision)

The `flip` subcommand refuses unless `--i-have-operator-approval=<decision-id>`
names the resolved `go` decision bead for this stage, and requires `--account`
(one bead per invocation).

**Order — canary first, watch ~5–10 min between each:**

1. `first-mate-haiku` (ka-0000002464) — lowest traffic canary.
2. `gardenio3`, `gardenio2`, `gardenio` (ka-0000001761, ka-0000006008, ka-0000006686).
3. `matthewcbaker0122`, `matthewbaker-acme`, `matthewbaker1110`.
4. `acme-shared` last (it's keyless today → currently unusable; flip fixes it).

```bash
gardenio/scripts/litellm-stage2-migrate.sh flip \
    --account first-mate-haiku \
    --i-have-operator-approval=<decision-bead-id>
```

Per account this writes, **atomically**:
`base_url=http://gardenio-litellm:4000` + the seeded `litellm_key`. The controller
credential-sync loop propagates to running agents within ~60s; new pods inherit
immediately.

**Watch after each flip (5–10 min):**

| Signal | Healthy | Unhealthy → roll back this account |
|---|---|---|
| Agent API errors (`kubectl logs <agent-pod> \| grep -i error`) | none new | 401/403/5xx from litellm |
| `gardenio-litellm` pod | Running, 0 new restarts, `/health/readiness` 200 | restarts climbing / non-200 |
| spend_sync (`kubectl logs deploy/gardenio-agents \| grep -i spend`) | `spend sync completed`, no 400s | `update failed` / `unknown field` |
| account-cost write | bead `total_cost_usd`/`shadow_*` advancing each ~5 min cycle | stale >15 min |
| `gb usage` for that account | resolves in-cluster | errors |

## Rollback (tested escape hatch — instant, per account or all)

```bash
# Single account (atomic restore of prior external base_url + litellm_key):
LEDGER=/path/to/litellm-stage2-rollback-ledger.json \
  gardenio/scripts/litellm-stage2-migrate.sh rollback --account first-mate-haiku

# All accounts:
gardenio/scripts/litellm-stage2-migrate.sh rollback --ledger /path/...
```

Rollback restores EXACTLY the snapshot values (external host + the original
external virtual key), so agents return to the proven external path within ~60s.
**Keep the external proxy reachable until in-cluster is fully trusted (≥24 h).**
Dry-run both flip and rollback first (`--dry-run`) — done this run, both target
the right beads with atomic two-field writes.

## Declaring success

Wait ≥24 h after the full flip: zero litellm-caused agent API errors, cost fields
updating on every migrated account, no `gardenio-litellm` restarts, spend_sync
clean. Only then consider the embeddings + bedrock stage — **Stage-3**
(ka-0000002573), runbook [`litellm-stage3-runbook.md`](litellm-stage3-runbook.md).

## Key references

- `gardenio/controller/cmd/controller/main.go:1754` — `accountInfoUnusable`
- `gardenio/controller/cmd/controller/podspec.go:970-1010` — base_url precedence + headers
- `gardenio/docs/litellm-production-cutover.md` — the broader cutover runbook
- ka-0000006378 — non-prod seed findings (salt-rotation hazard, image has no curl)
