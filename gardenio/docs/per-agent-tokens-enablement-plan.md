# Per-Agent Tokens (PAT) — Enablement Plan

**Status:** Draft / parked. Code is shipped; enablement is deferred until the
public-internet exposure work (ka-0000001861, parent hardening epic
ka-0000000956 Tier 3) resumes.

**Scope:** Flip `agents.perAgentTokens=true` in prod so each agent pod
authenticates to the beads daemon with its own project-scoped fleet token
instead of the shared primary-static `BEADS_DAEMON_TOKEN`.

## Background

Today, every agent pod mounts `BEADS_DAEMON_TOKEN` — the primary static bearer
secret. That token:

- has full write access across all projects,
- bypasses per-identity rate limits,
- is indistinguishable from any other "static" caller in the audit trail.

The per-agent-tokens feature (commit `1273789f`, controller env
`PER_AGENT_TOKENS`) replaces this shared secret with a per-agent fleet token:

1. On agent spawn, the controller generates a random token, registers it with
   the beads daemon as a project-scoped fleet token, and creates a K8s Secret
   named `agent-token-<beadID>`.
2. The agent pod mounts its own secret instead of the global one.
3. On agent done/stop/kill, the controller revokes the fleet token and deletes
   the K8s Secret.

The code path is already implemented and exercised by unit tests. What is NOT
done is **enabling it in the real deploy values**.

## Why it is still off

The feature was intentionally deferred alongside the public-internet exposure
epic (ka-0000001861) on 2026-04-17. Rationale, at the time:

- Without public-internet exposure, the blast radius of the shared token is
  internal-cluster only, so the hardening is less urgent.
- Enabling PAT touches every agent-bearing namespace; we wanted to batch it
  with the rest of the Tier 3 hardening so the rollout plan could be tested
  once.
- It requires widening the controller's RBAC (Secrets create/delete in the
  agent namespace) — see `helm/gardenio/templates/controller/rbac.yaml`.

The shipped X-Actor attribution fix (see
`internal/beadsapi/client.go` SetActor + `cmd/gb/main.go`) solved the specific
symptom that prompted re-opening this file — **agent-created decisions showing
`created_by=static`** — without needing PAT, because X-Actor rewrites the
Subject on the existing shared-token flow. PAT remains the right long-term
answer for **authorization** (project scope, revocation), not just
**attribution**.

## When to re-open

Trigger a PAT enablement pass when any of these become true:

- The public-internet exposure epic (ka-0000001861) resumes — the shared-token
  blast radius gets much larger the moment the beads daemon is reachable
  outside the cluster.
- We need hard project isolation between fleets (e.g. a tenant or external
  contractor running their own agents against the same daemon).
- A credential leak incident turns the "internal only" assumption into a
  regret.
- Audit requirements demand per-agent credential attribution beyond the
  X-Actor audit record.

## Prerequisites

- [ ] Parent hardening epic ka-0000000956 re-opened and scheduled.
- [ ] Public-internet epic ka-0000001861 status reviewed — PAT should land
      **before** or **at the same time as** public exposure, never after.
- [ ] Rate-limit exemption semantics for fleet tokens are reconfirmed against
      `kbeads/internal/server/rate_limit.go` (see the earlier regression
      ka-0000005257 — fleet tokens hit per-identity limits unless explicitly
      exempted).
- [ ] Controller RBAC widened to allow Secrets CRUD in the agent namespace
      (the helm chart already conditionally adds this when
      `agents.perAgentTokens=true`; verify the deployed ServiceAccount picks
      it up).
- [ ] Confirm every agent image path reads the token from the env var, not a
      hard-coded mount path (grep `BEADS_DAEMON_TOKEN` across `images/agent`
      and `coop/`).

## Rollout steps

The rollout is a single deploy-value flip; the complexity is in the staging
and verification, not the flip itself.

### Phase 1 — Staging (gardenio-rwx)

1. Set `agents.perAgentTokens=true` in the acme-helm-chart
   `gardenio-rwx` overlay.
2. Release a calver, deploy to gardenio-rwx.
3. Dispatch the standard e2e formula; verify:
   - `kubectl get secrets -n gardenio-rwx -l gardenio.io/type=agent-token` shows
     one secret per active agent pod.
   - Fleet tokens appear in `kd fleet list` scoped to the correct project.
   - Agents complete their tasks normally (no auth regressions on
     bead CRUD / decision creation / squawk).
4. Let at least one agent cycle run to completion and confirm the revoke path:
   the Secret and fleet token disappear when the agent is done/stopped.
5. Run a revoke-under-crash test: kill an agent pod mid-task (SIGKILL) and
   confirm the reconciler revokes the token within one reconcile cycle.
   This is the failure mode we most want to catch before prod.

### Phase 2 — gardenio-next

gardenio-next is the config-mirrored release-candidate landing zone (see
`gardenio-next-mirror-deny-list.md`). It carries real agent traffic but
without e2e coverage, so it is a good second-stage canary.

1. Flip `agents.perAgentTokens=true` in the gardenio-next overlay.
2. Soak for at least 48 hours.
3. Watch the usual Slack channels for agent failures, auth errors, audit
   anomalies. Compare `kd audit` output for mutations before/after the flip —
   `created_by` should now show `agent-<beadID>` (fleet identity) in addition
   to the X-Actor audit line.

### Phase 3 — Prod

1. Flip `agents.perAgentTokens=true` in the prod acme-helm-chart overlay.
2. Release a calver; watch the release-pipeline e2e gate.
3. Verify the first wave of real agent pods mounts their per-agent Secret
   (spot-check 2–3 with `kubectl describe`).
4. Leave the fallback path (shared `BEADS_DAEMON_TOKEN` still provisioned) in
   place for one release cycle. Only strip it once we are confident no code
   path quietly relies on the global token.

### Phase 3a — k3s cluster flip (P7 Task 2, bead gd-kLRIbndGNp)

The k3s overlay flips `agents.perAgentTokens=true` and splits the agent-facing
coopmux token off `kd-beads-token` (`coopmux.coopmuxTokenSecret:
coopmux-agent-token`). Order matters; the whole sequence is one maintenance
window, because from the moment coopmux rolls it stops accepting the beads token
and every live agent pod (which still has it) loses its coopmux registration
until the pod is recreated.

Before the window, all of these must be merged and the release built from a main
that contains them: the controller hardening PR (#354: name validation,
lowercase Secret name, re-mint updates the Secret, revoke on post-mint failure,
reconciler pods mint their own token) and the kbeads parity PR (#356: agent
tokens carry taint context and grants, auto-parent, spawn-chain, project stamp,
mint-subject validation) and the configurable ownership bypass (PR #360: role-bead
field `bypass_ownership`, replacing the hard-coded gardener/captain list). Both
kbeads and the controller must be on that release BEFORE the flip.

**Step 0 — role beads (admin, before step 2).** An agent on its own token may
mutate beads it did not create only when a role element in its `role` names an open
`type:role` bead with `bypass_ownership: true` (default false; only an admin can set
it). The live role beads must be updated, otherwise the flip 403s these roles. The
roles seen on agent beads and what to do (checked 2026-10-14):

| Role | Live role bead | Action |
|---|---|---|
| `reviewer` (merge desk) | gd-MYJTpBdVnw exists | `kd update gd-MYJTpBdVnw -f bypass_ownership=true` |
| `claimlayer-reviewer` | gd-IL5Pcz6CkC exists | set the same, only if that merge desk needs it |
| `gardener`, `plan-gardener` | none | create `type:role` beads with `-f bypass_ownership=true` |
| `plan-captain`, `captain`, `release-captain` | none | create `type:role` beads with `-f bypass_ownership=true` |
| `crew`, `cicd`, `infra`, `thread`, `crew-*` | exist/none | leave unset (default false) |

These are the roles the old hard-coded list matched (any `-`-separated segment
`gardener`/`captain`) plus the reviewer. A new role bead with only that field
changes nothing else about the pod (every other field defaults). Verify with
`kd list --type role --all-types -f bypass_ownership=true`.

1. `kubectl -n gardenio create secret generic coopmux-agent-token
   --from-literal=token="$(openssl rand -hex 32)"` (create, never apply). It must
   exist before the roll or coopmux, controller and viewer pods fail on a missing
   Secret.
2. Roll through the normal release: `helm upgrade -f deploy-values/gardenio-k3s.yaml
   --set global.imageTag=<tag>` (a `release-cut.sh` tag never bumps `appVersion`;
   dropping the override is a rollback).
3. Re-apply the viewer edge so mux.clonk.tv carries the new coopmux token:
   `kubectl apply -f gardenio/helm/gardenio-ziti/viewer-authproxy.yaml`, then
   `kubectl -n gardenio rollout restart deploy/viewer-authproxy` (the token is
   substituted into nginx config at container start). Until you do, mux.clonk.tv
   returns 401.
4. **Recreate every live agent pod** so each gets its own beads token and the new
   coopmux token. List first, then delete; the controller (and its reconciler)
   rebuild each pod from its agent bead with a per-agent token:
   ```sh
   kubectl get pods -A -l gardenio.io/agent -o custom-columns=NS:.metadata.namespace,POD:.metadata.name,AGENT:.metadata.labels.gardenio\.io/agent
   kubectl get pods -A -l gardenio.io/agent -o json \
     | jq -r '.items[] | "\(.metadata.namespace) \(.metadata.name)"' \
     | while read -r ns pod; do kubectl -n "$ns" delete pod "$pod" --wait=false; done
   ```
   `gardenio.io/agent` is on agent pods in both the main and the gVisor namespace.
   Do not run this before step 2 has finished (the replacement would be built by the
   old controller with the shared token).
5. Verify with the spawn e2e, which fails if the flag is not actually on:
   `REQUIRE_PER_AGENT_TOKENS=1 NAMESPACE=gardenio CONTROLLER_DEPLOY=<controller deployment>
   gardenio/tests/e2e/scripts/test-per-agent-token-spawn.sh`. Then list one real
   crew pod's secretKeyRef names: `BEADS_*` from `agent-token-<bead id, lowercased>`,
   `COOP_MUX_TOKEN`/`COOP_BROKER_TOKEN` from `coopmux-agent-token`, no line naming
   `kd-beads-token`.

Known effects of the flip, checked 2026-10-14 (details on gd-kLRIbndGNp):

- **Rate limit.** The daemon's per-identity limiter (BEADS_RATE_LIMIT_ENABLED,
  default on: 20 mutations/s burst 40, 100 reads/s burst 200) is keyed by
  type+subject. The shared static token is exempt; an agent token is not, so each
  agent name gets its own bucket (429 + `Retry-After: 1`, logged as `rate limit
  exceeded`). Normal agent traffic is far below it; a scripted loop over hundreds
  of beads is not. After the flip, grep the kbeads log for that line. Tune with
  BEADS_RATE_LIMIT_{MUTATE,READ}_{RPS,BURST} on the beads deployment.
- **Beads with no `project:` label are invisible to agent tokens** (reads and list
  results are ANDed with the token's project label). Of 18,224 beads: 27 live
  task/bug/epic beads, 8 bundles, 26 docs and 4 live `advice` beads carry none.
  The advice ones matter: `gb prime` lists advice with the agent's own token, so
  advice without a `project:` label (the Global working-rules bead gd-3wArIbnnWn,
  the Role: crew advice gd-L4MSFKLoV5) would stop reaching agents. Do NOT backfill
  a project label onto them: a bare `project:` broadcast without role targeting is
  dropped by the advice matcher (IsDeniedBroadcast). Tracked as a flip
  precondition on gd-kLRIbndGNp.

Rollback for 3a: set `agents.perAgentTokens: false` and `coopmux.coopmuxTokenSecret: ""`
in the overlay, release, revert `viewer-authproxy.yaml` to `kd-beads-token`
and re-apply it, then recreate the agent pods again (step 4).

## Rollback

- Flip `agents.perAgentTokens=false` in the overlay and redeploy.
- Existing per-agent Secrets and fleet tokens are left in place; the
  revoke-on-stop path will drain them as agents exit.
- Stragglers can be swept with `kubectl delete secret -n <ns> -l
  gardenio.io/type=agent-token` + `kd fleet revoke <name>` for each
  `agent-*` fleet.

## Open questions

- **Fleet token lifetime under restart**: the controller revokes on
  done/stop/kill, but what if the controller itself crashes mid-provision?
  The K8s Secret reconciler should be extended to detect orphaned
  `agent-token-*` Secrets whose bead is closed — owner is this rollout.
- **Cross-project contamination during gardenio-next mirror**: the mirror
  deny-list (see `gardenio-next-mirror-deny-list.md`) must NOT carry fleet
  tokens. Add a test that confirms `beads.daemonToken` + `agent-token-*`
  Secrets are never mirror candidates.
- **gb CLI behavior when pod mounts a fleet token**: confirm `gb` picks up
  the per-agent token from the same env var (`BEADS_DAEMON_TOKEN`) without
  change; the X-Actor fix is independent of which token is used.

## Related

- `gardenio/controller/cmd/controller/agenttoken.go` — provisioning / revoke.
- `gardenio/controller/internal/config/config.go` — `PER_AGENT_TOKENS` env.
- `gardenio/helm/gardenio/values.yaml` — `agents.perAgentTokens` (default `false`).
- `gardenio/helm/gardenio/templates/controller/rbac.yaml` — conditional
  Secrets RBAC.
- ka-0000000956 — parent Tier 3 hardening epic (deferred).
- ka-0000001861 — public-internet exposure epic (deferred).
- ka-0000005257 — prior rate-limit regression; fleet-token exemption
  semantics to reconfirm before flip.
