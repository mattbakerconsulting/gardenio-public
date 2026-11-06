# LiteLLM Stage-3 Migration Runbook (embeddings + bedrock account cutover)

Operator-gated cutover of the **two items deliberately left external in Stage-2**
to the in-cluster LiteLLM proxy (`http://gardenio-litellm:4000`):

1. **Embeddings** — kbeads `BEADS_EMBED_URL` (model `bedrock-titan-embed-v2`),
   currently the EXTERNAL proxy (`https://litellm.app.devops.acme.example`).
2. **`litellm-bedrock` account** (ka-0000003488, model `bedrock-claude-opus-4-7`)
   — the OAuth-exhaustion safety-net account.

- **Epic:** ka-0000000560 (SENSITIVE — due caution)
- **Stage-1 (done):** ka-0000004799 — in-cluster proxy up in prod + global
  `LITELLM_URL` repointed. Decision gate ka-0000006287.
- **Stage-2 (done):** ka-0000000158 — 8 OAuth account beads flipped in-cluster +
  verified. Rollback ledger in Secret `gardenio-litellm-stage2-rollback-ledger`.
- **Stage-3 (this doc):** ka-0000002573.

Tooling: [`gardenio/scripts/litellm-stage3-migrate.sh`](../scripts/litellm-stage3-migrate.sh)
(`snapshot` / `seed` / `verify` / `embed-verify` / `flip` / `rollback`).

Both items were blocked in Stage-2 for the SAME reason: the in-cluster proxy
served only the OAuth chat groups — **no bedrock model group at all** (validation
ka-0000007591). Stage-3 closes that by adding bedrock support to the in-cluster
proxy config, then flipping each item behind its own bake + gate.

---

## What changed in the chart (this MR)

The in-cluster litellm chart already supported the Titan **embedding** model
(`litellm.embedding.enabled`, rwx-green under ka-0000007591). This MR adds a
parallel, default-off **bedrock CHAT** path so item 2 has somewhere to land:

| Key | Purpose |
|---|---|
| `litellm.bedrock.enabled` | Gate. `false` by default → model_list unchanged. |
| `litellm.bedrock.region` | AWS region for the chat InvokeModel calls. |
| `litellm.bedrock.models[]` | `{modelName, bedrockModel[, region]}` list. Defaults mirror the external proxy's `bedrock-claude-opus-4-7` + `bedrock-claude-haiku-4-5` (the account's model + probe_model). |

**Credentials are shared**: both the embedder and the chat models run in the
SAME litellm pod and use the SINGLE `litellm.embedding.{irsaRoleArn,staticCredsSecret}`
credential strategy. A helper (`gardenio.litellm.needsBedrockCreds`) drives the
ServiceAccount, `automountServiceAccountToken`, the AWS env block, and the
render-time guard (`templates/litellm/validate-embedding.yaml`) off "any bedrock
model enabled" rather than just embeddings. **The IRSA role (prod:
`arn:aws:iam::234567890123:role/gardenio-litellm-bedrock`) MUST allow
`bedrock:InvokeModel` on the chat inference profiles**, not just the Titan model
— see the escalation below.

Underlying Bedrock model ids (confirmed live 2026-06-14 via `/model/info` on the
external proxy):

| Routed name | Bedrock model id |
|---|---|
| `bedrock-titan-embed-v2` | `bedrock/amazon.titan-embed-text-v2:0` |
| `bedrock-claude-opus-4-7` | `bedrock/us.anthropic.claude-opus-4-7` |
| `bedrock-claude-haiku-4-5` | `bedrock/us.anthropic.claude-haiku-4-5-20251001-v1:0` |

## Why this is a hard gate

Same blast radius as Stage-2 plus two extras:

- **Embeddings**: if `BEADS_EMBED_URL` repoints in-cluster while the proxy can't
  serve `bedrock-titan-embed-v2` at **1024 dims**, every `kd embed sync` /
  semantic search 400s (validation ka-0000007591). The pgvector column is
  1024-wide (migration `005_bead_embeddings`); a dim mismatch corrupts search.
- **bedrock account**: the controller treats `base_url`-set + empty
  `litellm_key` as UNUSABLE (`accountInfoUnusable`, `main.go:1754`). So the flip
  and rollback **must write `base_url` AND `litellm_key` together, atomically**.
  This account is the OAuth-exhaustion safety net — if it 401s, exhausted teams
  have no fallback.

Hence: green non-prod bake + tested rollback + fresh operator decision-gate
before either prod flip.

## Prep (non-destructive — run freely, no gate)

Snapshot + seed are read-mostly and never touch live traffic.

```bash
# 1. Capture the bedrock account's (and every account's) prior base_url +
#    litellm_key into a rollback ledger.
LEDGER=/path/to/litellm-stage3-rollback-ledger.json \
  gardenio/scripts/litellm-stage3-migrate.sh snapshot

# 2. Mint a bedrock-scoped virtual key in the IN-CLUSTER litellm DB and store the
#    plaintext in K8s secret gardenio-litellm-stage3-vkeys. Defaults to the
#    litellm-bedrock account. Skips if the proxy isn't serving bedrock yet —
#    enable litellm.bedrock first (see bake). Round-trips via /key/info.
gardenio/scripts/litellm-stage3-migrate.sh seed

# 3. Re-verify the seeded key resolves AND the proxy serves model + probe_model.
gardenio/scripts/litellm-stage3-migrate.sh verify
```

Do **not** rotate `LITELLM_SALT_KEY` after seeding — it orphans the encrypted key
rows (ka-0000006378).

## Non-prod bake (gardenio-rwx / POC)

The chart support is enabled in non-prod first. gardenio-rwx already runs
`litellm.embedding.enabled=true`; add `litellm.bedrock.enabled=true` to the
overlay and bake.

```bash
# Embeddings already green in rwx; re-confirm after any change:
NAMESPACE=gardenio-rwx LITELLM_SVC=gardenio-rwx-litellm \
  gardenio/scripts/litellm-stage3-migrate.sh embed-verify

# Bedrock chat: seed + verify against the rwx proxy.
NAMESPACE=gardenio-rwx LITELLM_SVC=gardenio-rwx-litellm \
  VKEY_SECRET=gardenio-litellm-stage3-vkeys \
  gardenio/scripts/litellm-stage3-migrate.sh seed
NAMESPACE=gardenio-rwx LITELLM_SVC=gardenio-rwx-litellm \
  gardenio/scripts/litellm-stage3-migrate.sh verify
```

Bake long enough to trust the bedrock chat path (a real agent inference round
against the seeded key, no `InvokeModel` AccessDenied, spend recorded).

## Prod values flip — Item 1 (embeddings)

This is a **helm values change**, deployed via the release pipeline — not a bead
write and not a script `flip`.

In `deploy-values/gardenio.yaml`:

```yaml
litellm:
  enabled: true
  embedding:
    enabled: true                       # adds bedrock-titan-embed-v2 in-cluster
    region: "us-east-1"
    irsaRoleArn: "arn:aws:iam::234567890123:role/gardenio-litellm-bedrock"

beads:
  embedding:
    enabled: true
    url: "http://gardenio-litellm:4000"  # was https://litellm.app.devops.acme.example
    model: "bedrock-titan-embed-v2"
    apiKeySecret:
      name: "litellm-master-key"
      key: "master-key"                 # in-cluster proxy's own master key
```

**Before** declaring success, run `embed-verify` against PROD to confirm the
in-cluster proxy serves the model at 1024 dims:

```bash
gardenio/scripts/litellm-stage3-migrate.sh embed-verify      # NAMESPACE defaults to gardenio
```

**Rollback (item 1):** revert `beads.embedding.url` to
`https://litellm.app.devops.acme.example` (+ `apiKeySecret` back to the external
key) and redeploy. The external proxy still serves the Titan model. Optionally
also set `litellm.embedding.enabled=false` (leaving it enabled is harmless — it
just keeps the model in the list).

## Prod flip — Item 2 (bedrock account, GATED)

Requires `litellm.bedrock.enabled=true` in `deploy-values/gardenio.yaml`:

```yaml
litellm:
  bedrock:
    enabled: true
    region: "us-east-1"
    # models default to bedrock-claude-opus-4-7 + bedrock-claude-haiku-4-5
```

Then, **only after** a resolved `go` decision for this stage:

```bash
# Prereqs: snapshot done, key seeded + verified against PROD.
gardenio/scripts/litellm-stage3-migrate.sh seed       # against prod
gardenio/scripts/litellm-stage3-migrate.sh verify     # model + probe_model served

gardenio/scripts/litellm-stage3-migrate.sh flip \
    --account litellm-bedrock \
    --i-have-operator-approval=<decision-bead-id>
```

This writes, atomically: `base_url=http://gardenio-litellm:4000` + the seeded
`litellm_key`. The controller credential-sync loop propagates within ~60s.

**Watch after the flip (5–10 min):**

| Signal | Healthy | Unhealthy → roll back |
|---|---|---|
| agents on the bedrock account | no new 401/403/5xx | litellm auth errors |
| `gardenio-litellm` pod | Running, 0 new restarts, `/health/readiness` 200 | restarts / non-200 |
| Bedrock InvokeModel | succeeds | `AccessDenied` (IRSA role missing chat invoke) |
| spend_sync | `spend sync completed` | `update failed` |
| account-cost write | `total_cost_usd` advancing | stale >15 min |

**Rollback (item 2):**

```bash
LEDGER=/path/to/litellm-stage3-rollback-ledger.json \
  gardenio/scripts/litellm-stage3-migrate.sh rollback --account litellm-bedrock
```

Restores EXACTLY the snapshot values (external host + the original external
virtual key). Keep the external proxy reachable ≥24 h after each flip.

## Declaring success

Wait ≥24 h after each item's flip: zero litellm-caused agent API errors, embed
sync clean, cost fields updating on the bedrock account, no `gardenio-litellm`
restarts, spend_sync clean. Item 1 and item 2 can be flipped independently —
embeddings first (lower risk, no decision needed beyond the values deploy) then
the account flip behind its decision gate.

## Operator escalation — Bedrock IAM (item 2)

The prod IRSA role `gardenio-litellm-bedrock` was provisioned for the Titan
**embedding** model. Before item 2's flip, an operator MUST confirm/extend its
policy to allow `bedrock:InvokeModel` on the chat inference profiles
`us.anthropic.claude-opus-4-7` and `us.anthropic.claude-haiku-4-5-20251001-v1:0`
(and the cross-region inference-profile resource ARNs). Without it the flip
yields `AccessDenied` at the Bedrock boundary. File via `gb escalation create`.
The `embed-verify` / `verify` bake catches this before any prod flip if run
against a proxy whose role lacks the chat permission.

## 133-prod bring-up notes (2026-07-05)

When the litellm-bedrock account was first exercised on new prod
(prod-gardenio-eks, acct 133), the IAM escalation above was NOT the blocker —
the prod IRSA role is `gardenio-prod-litellm-embed` and its policy already
granted `bedrock:InvokeModel` on the claude chat inference profiles +
foundation models (admin `invoke-model` of `us.anthropic.claude-opus-4-7`
succeeded, profile ACTIVE). Instead, three separate migration gaps had to be
closed before an fsm-triage agent could complete a bedrock turn. All three are
prerequisites for the flip on any fresh cluster:

1. **Account credential secret (K8s).** litellm-bedrock's `credential =
   secret:litellm-gardenio-key`. Its ExternalSecret pulls from the shared
   133-account SM name `gardenio/litellm-gardenio-key` (property `token`, a
   25-char `sk-…` litellm virtual key). On 133 the SM secret did not exist —
   the value lived only as a live K8s secret in the legacy e2e cluster
   (`gardenio` ns). Migrate it: read the e2e secret's `token`, write
   `{"token": "<value>"}` to SM `gardenio/litellm-gardenio-key` (mode-600 temp,
   `file://`, never echoed). Without it the controller fail-closes every
   fsm-triage spawn ("refusing to launch a loginless pod — secret
   litellm-gardenio-key not found").

2. **ESO IRSA read grant (IAM).** The external-secrets role
   `gardenio-prod-external-secrets` must be allowed to read `gardenio/litellm-*`
   or the ExternalSecret above gets `AccessDenied` (it allowed
   `gardenio/gardenio-*` etc. but not `gardenio/litellm-*`). Codified in
   `composition/canonical-prod/platform.tf` (`p44_oauth_bootstrap_secret_arns`);
   do NOT re-add it by hand — `terraform apply` now owns it.

3. **Virtual keys in the litellm DB (RDS).** The account's `litellm_key` (and
   the `litellm-gardenio-key` credential value, if the agent presents it) must
   exist as rows in `LiteLLM_VerificationToken` in the prod litellm RDS,
   scoped to `bedrock-claude-opus-4-7` + `bedrock-claude-haiku-4-5`, else the
   agent 401s `token_not_found_in_db`. This is the `seed` step — normally mints
   a fresh key + `flip`s it onto the bead, but if the bead already carries the
   correct in-cluster `base_url` + `litellm_key` (as on 133) you can instead
   seed that exact key value via `/key/generate {"key": "<vkey>", "models":
   [...]}` and skip the bead write. **DB-rebuild hazard:** these rows are
   durable in RDS but are NOT recreated automatically — after any litellm DB
   reinit (see the Stage-2 empty-vkey outage) re-run `seed` for this account.

Verified end-to-end on 133 2026-07-05: fsm-triage agent spawned on
litellm-bedrock, authenticated, litellm invoked bedrock via SigV4, agent
completed its turn (`agent_state=done`). 200 OK confirmed for streaming,
extended thinking, tools, and `max_tokens=32000`.

## Key references

- `gardenio/controller/cmd/controller/main.go:1754` — `accountInfoUnusable`
- `gardenio/controller/cmd/controller/podspec.go:970-1010` — base_url precedence
- `gardenio/helm/gardenio/templates/litellm/configmap.yaml` — model_list render
- `gardenio/helm/gardenio/templates/litellm/validate-embedding.yaml` — cred guard
- `gardenio/docs/litellm-stage2-runbook.md` — the Stage-2 account cutover
- ka-0000007591 — the in-cluster-no-bedrock blocker (item-1 origin)
- ka-0000006378 — salt-rotation hazard; image has no curl (use the script)
