# Stale-Namespace Reaper Scope — Decision Artifact

**Bead:** ka-0000000105 · **Parent epic:** ka-0000005701 (infra-cost gardener) · **Operator:** Matt (slack:U0OPERATOR1)
**Status:** awaiting operator decision

## Problem

The `prod-gardenio-eks` cluster (acct `123456789012`) is shared by multiple apps.
gardenio ships an ephemeral-namespace reaper (Helm `rwxReaper`, a `*/30` CronJob),
but it is **deliberately hard-scoped to `gardenio-rwx-*` namespaces only** — it
refuses any other prefix by design. Other apps' CI ephemerals therefore have **no
reaper at all** and accumulate indefinitely.

### Live evidence (re-verified 2026-08-13, read-only `kubectl`)

- **22 non-gardenio CI ephemeral namespaces Active**, all `mr-*` prefixed (the
  original `site-api-mr-*` / `sponsor-api-mr-*` shells have since aged out; the
  `mr-*` population persists and has grown).
- Ages span **~2d to ~36d**; oldest `mr-test-ka-0000005324-deploymr` (2026-07-08),
  `mr-forge-preview` (2026-07-11), `mr-main` (2026-07-25).
- Cost-gardener measured these at **~104 reserved pods / ~20.4 reserved vCPU** at
  ~2–3% utilization — the single largest reserved-core block in the cluster (larger
  than the ~12.3 cores gardenio itself reserves), against cluster-wide *actual* CPU
  use of only ~7.6 cores.
- Only two reaper CronJobs exist cluster-wide and **both are gardenio's own**
  (`gardenio-rwx/gardenio-rwx-rwx-reaper`, `gardenio-system/gardenio-rwx-reaper`), each
  scoped to `gardenio-rwx-*`. Nothing reaps `mr-*`.

## Why this is NOT a gardenio-chart fix

The reaper's hard scope is a load-bearing safety property, not an oversight. Widening
`NS_PREFIX` beyond `gardenio-rwx-` in gardenio's chart would be actively wrong:

1. **Ownership.** `mr-*` namespaces belong to *other apps'* CI (the monorepo /
   site-api / sponsor-api MR-preview pipelines). Deleting another team's CI
   environments from gardenio's release chart is a cross-team blast-radius gardenio
   must not own. The bead has said "do NOT delete via gardenio chart" across three
   cost-gardener cycles.
2. **The reaper is coupled to gardenio's naming contract.** Beyond deleting the
   namespace it also deletes gardenio-specific **per-instance cluster-scoped RBAC**
   (`<ns>-agent-cluster-admin`, `<ns>-agents-cluster-manager`, …) whose names are
   *derived from* the `gardenio-rwx-<id>` convention (see
   `templates/rwx-reaper/rbac.yaml`, `_helpers.tpl`). Those grants don't exist for
   `mr-*` namespaces; the cleanup logic has no meaning there.
3. **Fail-closed guards assume the prefix.** The `protectedNamespaces` denylist, the
   `REAPER_NSGUARD` invariant marker, and the finalizer-clearing path all sit inside
   the prefix-gated loop and are tuned for gardenio ephemerals. Generalizing the
   sweep re-opens exactly the fail-open surface those guards were added to close
   (ka-0000001816, ka-0000004129's 223-namespace leak).
4. **RBAC scope.** gardenio's reaper ServiceAccount is granted namespace + CRB
   delete rights derived from the `gardenio-rwx-*` contract. It has no mandate (and
   should get none) to delete arbitrary cluster namespaces.

The `gardenio-rwx-*` reaper *pattern* generalizes well; the *gardenio chart instance*
of it does not. This is a shared-cluster hygiene gap that belongs to the cluster
owner, not to gardenio's release artifact.

## Options

### Option A — Escalate to cluster owner (kubernetes-team): stand up a generalized shared reaper ⭐ RECOMMENDED

Hand the cluster owner a cluster-scoped, prefix/label-driven reaper (a small
platform-owned CronJob, not part of any single app's chart) that covers **all**
ephemeral CI namespace conventions (`mr-*`, `site-api-mr-*`, `sponsor-api-mr-*`,
`local-mr-*`, and future ones), keyed on a TTL and an opt-out `lifecycle=keep`
label. gardenio keeps its own `gardenio-rwx-*` reaper for the gardenio-specific RBAC
cleanup it must own.

- **Pros:** Correct owner. Covers every app uniformly, including future prefixes.
  Reuses gardenio's battle-tested reaper design (Replace policy, deadlines,
  finalizer clearing, count alert) as a reference. gardenio's chart stays scoped and
  safe. Reclaims the ~20 reserved vCPU immediately once live.
- **Cons:** Requires kubernetes-team to accept and operate a new platform component;
  not something gardenio can merge itself. Needs the CI-preview owners to agree a TTL
  that won't kill an MR env someone is mid-review on (mitigated by `lifecycle=keep`).

### Option B — Cluster-wide TTL-label convention for ephemeral namespaces

Instead of (or alongside) a central sweeper, standardize a `ttl`/`expires-at` label
that every app's CI stamps on its ephemeral namespaces at creation, enforced by a
generic controller (kube-janitor / a small label-driven reaper). Each app owns its
own TTL; the platform owns the enforcer.

- **Pros:** Decentralizes the *policy* (each team sets its own TTL) while
  centralizing *enforcement*. Self-documenting per-namespace expiry. No hard-coded
  prefix list to maintain as new apps appear.
- **Cons:** Requires every app's CI pipeline to adopt the label (a rollout, not a
  one-shot). Un-labeled legacy namespaces (like the current 22) still need a
  one-time manual sweep or a fallback age rule. More moving parts than A.

### Option C — Status quo: gardenio stays hard-scoped, file-and-handoff only

gardenio does nothing beyond flagging; the 22 (and growing) namespaces persist until
a human manually deletes them or the cluster owner acts independently.

- **Pros:** Zero gardenio work; no risk to gardenio's release path.
- **Cons:** The waste is real and recurring — three cost-gardener cycles have
  re-confirmed it unchanged. ~20 reserved vCPU stays stranded; the count only grows.
  "Flag and hope" has already run for weeks without resolution.

## Recommendation

**Option A**, with **Option B as the label mechanism inside it**: escalate to the
cluster owner to stand up a *platform-owned, label-driven* generalized reaper that
covers all app CI-ephemeral prefixes, using gardenio's existing reaper as the
reference design and a `lifecycle=keep` opt-out. gardenio retains its own
`gardenio-rwx-*` reaper for the RBAC cleanup only it can do.

This is the option that (a) puts the fix with the correct owner, (b) actually
reclaims the ~20 stranded vCPU, and (c) keeps gardenio's chart scoped and safe. C is
rejected because three cycles of file-and-hope have not moved the number. A pure
gardenio-chart widening is rejected for the four coupling/ownership reasons above.

## Next action if approved

File a `gb escalation` to kubernetes-team (the cluster owner) requesting a
generalized shared reaper / TTL-label enforcer for `prod-gardenio-eks`, referencing
this doc and gardenio's `templates/rwx-reaper/` as the reference implementation, and
attaching the current 22-namespace inventory for the one-time backlog sweep.
