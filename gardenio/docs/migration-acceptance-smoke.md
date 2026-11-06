# P4.4 migration-acceptance smoke runbook

Codified post-cutover smoke checks that gate the fresh-prod migration
(Phase-4 epic `ka-0000002205`, smoke epic `ka-0000003481`). Run this
**before declaring a cutover good**.

## Why this exists

The P4.3b prod beads-viewer **502** (`ka-0000007004`) slipped through
because every prior "proof of life" on the new prod cluster only validated
the **pre-auth `303`→GitLab-OAuth redirect**. That redirect is issued by
the AVA edge / oauth2-proxy *before any backend is touched*, so a mis-wired
viewer→daemon edge (AVA forwarding to the controller NLB instead of a
viewer-edge NLB) read as "green." No check ever exercised an **authenticated
page load** or the **agent→MR loop**.

These three checks close that gap. They are codified as harness modules (not
manual curls) and grouped into a named `MIGRATION_ACCEPTANCE` subset runnable
against the fresh prod cluster.

## The three checks

| Module | Bead | Proves |
|---|---|---|
| `migration-postauth-viewer` | `ka-0000008176` | An **authenticated** beads-viewer GET returns **HTTP 200 with real, daemon-backed body** — traversing AVA-trusted bearer → viewer pod → gardenio-beads daemon. A `303/302/307` (the pre-auth redirect that masked the 502) is a **hard fail**. |
| `migration-coop-reachable` | `ka-0000004091` | The **coop/coopmux** terminal path is reachable + healthy: coopmux Deployment ready and `GET /api/v1/health` → `{"status":"running"}`. |
| `migration-agent-opens-mr` | `ka-0000002701` | A **real agent** spawned on the cluster opens an MR end-to-end (claim bead → branch → push → `glab mr create` → `gb mr link`), proving controller + coopmux + daemon + GitLab post-cutover. **Opt-in** (`AGENT_MR_E2E=1`) — slow + costs API tokens. |

### How the post-auth check distinguishes 200 from the pre-auth 303

The public edge 308-redirects bearer traffic before Traefik, so you **cannot**
curl the public viewer host with a token and get 200. The check port-forwards
straight to the viewer ClusterIP (`svc/<release>-beads-viewer:8095`), bypassing
the edge, and presents a Keycloak-minted bearer to the viewer's own OIDC
middleware on localhost. It uses `curl` **without `-L`** (a redirect surfaces
as the status, not silently followed) and treats any `3xx` as a hard fail —
mirroring edge-auth Gate M. It then creates a fixture bead via the daemon and
asserts that bead renders in the viewer's `/{id}` page (`handleBeadShow` →
`s.daemon.GetBead`), so a 200 with the fixture's marker in the body proves the
**full AVA→viewer→daemon** path, not a static shell.

Auth helpers reused (no new ones invented): both `mint_user_token` (default)
and `mint_client_credentials_token` (`VIEWER_TOKEN_MODE=client`, for in-pod
runs) now resolve a GitLab-direct transport bearer via `lib-gitlab-token.sh`
(T6b, ka-0000001463 — the Keycloak-minting helpers were retired). Against a
beads-viewer still verifying Keycloak tokens the module SKIPs until the
viewer-login migration lands (ka-0000002083); in the ephemeral CI namespaces
the viewer Service is absent, so it SKIPs there regardless.
The thrice-duplicated `setup_viewer_portforward` / `viewer_get` helpers were
extracted into `lib-viewer.sh` (honoring the standing code TODO); the new
module is its fourth caller.

## Running it

### As a named subset via run-shard.sh

```sh
# All three (agent-opens-MR self-skips unless AGENT_MR_E2E=1):
MIGRATION_ACCEPTANCE=1 NAMESPACE=<ns> ./gardenio/tests/e2e/scripts/run-shard.sh

# Include the real-agent MR-loop check:
MIGRATION_ACCEPTANCE=1 AGENT_MR_E2E=1 NAMESPACE=<ns> \
  ./gardenio/tests/e2e/scripts/run-shard.sh
```

`MIGRATION_ACCEPTANCE=1` narrows the run to
`stack-health,tls-certs,migration-postauth-viewer,migration-coop-reachable,migration-agent-opens-mr`
and runs single-shard (shard gating disabled), mirroring `SMOKE_ONLY=1`.

### Against the fresh prod cluster via RWX (the cutover gate)

`.rwx/migration-acceptance.yml` is a **dispatch-only, READ-ONLY** workflow.
`eks-cluster` is **required and fail-loud** — there is no default target, so a
mis-dispatch can never silently hit the wrong cluster (that wrong-target class
is exactly what produced `ka-0000007004`). It also fail-loud aborts if the
target namespace is absent, so the gate can never vacuously pass.

```sh
rwx dispatch gardenio-migration-acceptance \
  --param eks-cluster=prod-gardenio-eks \
  --param namespace=gardenio \
  --param host-domain=gardenio.example.com \
  --param commit-sha=$(git rev-parse HEAD) \
  --param ref-name=refs/heads/main
# add --param agent-mr=1 to also run the real-agent MR-loop check
```

`host-domain` selects the AWS vault that authenticates to the cluster,
mirroring how staging dispatches pass `GARDENIO_STAGING_HOST_DOMAIN`:

- `gardenio.example.com` / `aws-poc.gardenio.acme.example` → **gardenio-main** vault
  (acct `123456789012` — the fresh customer-poc/POC-lineage prod cluster
  `prod-gardenio-eks`).
- `gardenio.e2e.example.com` / `e2e.example.com` → the dedicated
  `PROD_*` project secrets (legacy `america-e2e-eks`, acct `234567890123`).
- any other value → **hard FATAL** (no silent fallback): only the two
  recognized routing classes are accepted, so a typo can never quietly
  authenticate with the wrong account.

After configuring creds the workflow asserts `aws sts get-caller-identity`
matches the account the host-domain routing expects, and refuses to run on a
mismatch — turning any (host-domain, eks-cluster) account mix-up into one clear
fail rather than a wrong-cluster run.

### Why the gate can't vacuously pass

Every module self-skips when its substrate is absent, and `run-shard.sh` exits
0 on an all-skip run — so a wrong-namespace/wrong-overlay cutover could
otherwise go green having tested nothing (the `ka-0000007004` class). The RWX
workflow guards against this: it fail-loud aborts when the target namespace is
absent, and after the run it parses `junit-results-0/e2e-results.jsonl` and
requires the two load-bearing modules (`migration-postauth-viewer`,
`migration-coop-reachable`) to have **`status==pass`** — a skip or fail on
either is a hard RED. (`tls-certs` and `stack-health` are included for
signal but are not gate-required, so their self-skip behavior on absent
substrate never false-greens the gate.)

## Safety / write posture

READ-ONLY apart from: creating + closing **one throwaway fixture bead**
(post-auth viewer), and — only under `AGENT_MR_E2E=1` — spawning **one agent**
that opens a **Draft** MR on a throwaway branch (never pushes to / merges main).
No AWS or infra writes. Any real AWS/data write on prod still requires a fresh
operator `gb decision` (program §8); this workflow performs none.

Every module **self-skips loudly** when its substrate is absent (viewer Service
missing, coopmux disabled, controller absent, `AGENT_MR_E2E` unset), so it is
inert in minimal ephemeral e2e namespaces and only asserts against the
long-lived singleton / prod.

## As the cutover acceptance gate

Before signing off the P4.4 cutover as good, run the RWX dispatch above against
the fresh prod cluster with `--param agent-mr=1` and require all modules green
(no `FAIL`; `SKIP`s are acceptable only for substrate genuinely not deployed).
This is the codified replacement for the manual "does the viewer load / can I
open a terminal / can an agent make an MR" checks that missed the 502.
