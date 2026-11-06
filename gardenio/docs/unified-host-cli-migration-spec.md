# Unified-host CLI migration — spec

Status: **draft / spec** (2026-06-01). Parent epic: ka-0000006723
(unified-URL-path final cutover lap). This spec covers moving **CLI and
local-config consumers** off the legacy per-app hosts onto the unified
AVA host `gardenio.<env>` (path-fanned `/beads`, `/kd`, `/mux`, …) plus the
non-AVA shim `cli.gardenio.<env>`.

It exists because "just point everything at the unified host" has a hard
auth-model ordering constraint that is easy to miss and will break the
primary CLI (`kd`) if ignored.

## The constraint (measured 2026-06-01)

Same daemon-token bearer, three hosts:

| Host / path | daemon-token | what it actually wants |
| --- | --- | --- |
| `beads.gardenio.app/v1/health` (legacy) | **200** | honors the daemon token out-of-band (bypasses AVA) |
| `gardenio.app/beads/v1/health` (unified) | **303** | AVA OIDC cookie — token is rejected, 303→GitLab |
| `cli.gardenio.app/kd/v1/health` (shim) | **401** | a **GitLab PAT**, not the daemon token |

Consequence: a consumer's ability to move to the unified host is gated by
**which credential it can present**, not by config alone.

- **`gb`** reads the credstore cookie (post-`gb login`) → **can move to
  the unified host now** (cookie clears AVA).
- **`kd`** does **not** read the credstore — it authenticates with
  `BD_DAEMON_TOKEN` + `BEADS_OPERATED_BY`. Pointed at the unified host it
  gets 303'd; pointed at the shim it gets 401'd. **`kd` cannot leave the
  legacy host until it can present a cookie or a PAT.** This is the long
  pole.

## Inventory of legacy-host consumers

### A. Local operator config (this machine)
- `~/.bashrc`: `BD_DAEMON_HOST`, `BEADS_HTTP_URL` → `beads.gardenio.app`
  (drive `kd`); `BD_DAEMON_TOKEN` / `BEADS_DAEMON_TOKEN` (the bypass token).
- `~/.beads/config.yaml`: `daemon-host` → `beads.gardenio.app`.
- `BEADS_VIEWER_URL` / `BEADS_CLI_EXCHANGE_URL` (added 2026-06-01) → viewer
  + shim, already correct for `gb login`.

These **cannot** be flipped to the unified host while `kd` is token-based —
doing so 303s every `kd` call. They move in lockstep with the `kd` long pole.

### B. CI / automation — tracked: ka-0000003829 (blocked on PAT ka-0000007619)
- `.gitlab-ci.yml:1033`, `.rwx/coverage-ratchet.yml:65,72`,
  `.rwx/dictation-corpus-nightly.yml:35,41` → `beads.gardenio.app` + daemon
  token. Target: `cli.gardenio.app/kd` + `GARDENIO_GITLAB_PAT`. Staged in
  MR !3290 (must-not-merge until the PAT exists).

### C. Slack-bridge / beads-viewer default URLs — tracked: ka-0000004747
- Permalinks fall through to the legacy AVA viewer host. Operator-gated
  (couples with the legacyRedirects flip).

### D. Legacy-host deprecation (turn the old hosts off) — tracked: ka-0000007283
- Blast radius already mapped: daemon host (CI token caller), jira webhook,
  audit-host coupling. Hard-gated: 48h soak + Mezmo per runbook ka-0000000026.

## The long pole: make `kd` cookie-aware

`gb`'s working auth (credstore cookie + AVA→GitLab silent refresh) lives in
`gardenio/controller/internal/{credstore,avareauth}` — inside the **gardenio**
Go module. `kd` is in the **kbeads** module and cannot import it. To make
`kd` clear AVA the same way `gb` does:

1. Extract `credstore` + the cookie-replay/refresh HTTP transport out of
   `gardenio/controller/internal/` into a **shared module** both `gardenio`
   and `kbeads` import (go.work already links them).
2. Wire `kd`'s HTTP client to prefer the credstore cookie when present,
   falling back to `BD_DAEMON_TOKEN` for legacy/in-cluster use.
3. `kd` then targets `gardenio.app/beads` with the cookie (or, for
   headless/CI, a PAT against `cli.gardenio.app/kd`).

This is the gate for migrating both **A (local config)** and the human side
of **B**. It is a real refactor (module extraction + client rewire +
silent-refresh parity + tests), not a config flip.

## Sequenced plan

1. **gb default-path polish (unblocked, small).** Zero-config `gb login`
   (bake/derive viewer+shim hosts) + point `gb`'s own daemon client at the
   unified host (cookie). Update `gb-login-runbook.md` for the carve-out +
   add a first-time-setup section. *No auth-model change — gb already does
   cookies.*
2. **Provision `GARDENIO_GITLAB_PAT`** (escalation ka-0000007619, operator).
   Unblocks CI repoint (ka-0000003829 / MR !3290).
3. **`kd` cookie-awareness** (long pole): credstore→shared module, rewire
   `kd` client. Unblocks local-config + human CLI migration.
4. **Flip local config** (A) to the unified host once step 3 lands.
5. **Default-URL switch** (ka-0000004747) + **legacy deprecation**
   (ka-0000007283) — operator-gated, 48h soak + Mezmo, last.

## What is safe to do today vs gated

- **Safe now:** step 1 (gb polish + docs); writing this spec; `gb login`
  env vars (done); the carve-out itself (merged + live in rwx + prod).
- **Gated/blocked:** step 2 (operator must mint the PAT); step 3 (sizeable
  refactor — needs a green light); steps 4–5 (operator soak gates).

Do **not** repoint `~/.bashrc`/`~/.beads/config.yaml` `kd` hosts to the
unified host before step 3 — it silently 303s every `kd` call.
