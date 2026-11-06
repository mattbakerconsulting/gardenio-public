# slack-e2e

Real-user Playwright Slack E2E suite. Drives the gardenio Slack surface **the way
a human does** — types and clicks in the real `app.slack.com` web client,
authenticated as a dedicated **test human user** — against a dedicated test
Slack workspace/app wired to the ephemeral `slack-bridge` under test, and
asserts the controller/bridge react end-to-end over **Socket Mode**.

- **Epic:** `ka-0000006991` (Real-user Playwright Slack E2E testing)
- **Plan:** [`gardenio/plans/slack-e2e-playwright.md`](../../plans/slack-e2e-playwright.md)
- **Provisioning + session-refresh runbook:** [`gardenio/docs/slack-e2e-provisioning.md`](../../docs/slack-e2e-provisioning.md)
- **Module runner (CI):** [`gardenio/tests/e2e/scripts/test-slack-ui.sh`](../e2e/scripts/test-slack-ui.sh) (E2E module `slack-ui`)

It complements — does not replace — the outbound-only `slackmock` + bash Slack
scripts (`test-slack-{threads,cards,dashboard}.sh`), which post as the bot and
read back but never render the real client UI or flow a **human-authored** event
inbound.

## How it differs from `beads-viewer-e2e`

| | beads-viewer-e2e | slack-e2e (here) |
| --- | --- | --- |
| App under test | boots it itself (`webServer:`) | already-running ephemeral `slack-bridge` |
| Auth | dev-bypass | pre-captured test-user `storageState` (`d`/`d-s` cookies) |
| Fixtures | seeds a local daemon | none seeded — asserts against live bridge + daemon |
| Parallelism | parallel | serialized (`workers: 1`) for Socket Mode fan-out safety |
| Browsers | chromium + scoped firefox | chromium only |

## Run

This suite is the `slack-ui` E2E module — do not run it directly. No CI
workflow runs it today (`.github/workflows/e2e.yml`'s `shell-hermetic` job runs
only the cluster-free subset). The runner gates the whole rig and **skips
cleanly** when any piece is absent (local dev, fork CI, un-opted-in namespaces),
mirroring the `SLACK_BOT_TOKEN` guard on the sibling bash Slack scripts. Against
a live namespace, run it through the shard runner:

```sh
# from the repo root; slack-ui is pinned to shard 2 (Socket Mode serialized)
ONLY=slack-ui E2E_SHARD=2 ./gardenio/tests/e2e/scripts/run-shard.sh
```

The runner ([`test-slack-ui.sh`](../e2e/scripts/test-slack-ui.sh)) extracts the
captured session out of the ESO-synced K8s Secret `slack-e2e-session` into a
private temp file, then exports the env contract below and runs `npm ci` +
`npx playwright install --with-deps chromium` + `npm test`.

**Locally against a live rig** (rare — needs a real test session), export the
contract by hand and run:

```sh
cd gardenio/tests/slack-e2e
npm ci
npx playwright install --with-deps chromium
SLACK_E2E_STORAGE_STATE=/path/to/storage-state.json \
SLACK_E2E_BRIDGE_URL=http://localhost:8099 \
SLACK_E2E_CHANNEL=C0XXXXXXX \
BEADS_HTTP_URL=http://localhost:19090 \
  npm test
```

## Env contract

Handed in by the runner; consumed by `playwright.config.js`, `globalSetup.mjs`,
and the fixtures (see [`tests/_slack.js`](tests/_slack.js)).

| Env | Required | Meaning |
| --- | --- | --- |
| `SLACK_E2E_STORAGE_STATE` | yes (real run) | Path to the captured test-user `storageState` JSON. Applied via `use.storageState`. |
| `SLACK_E2E_BRIDGE_URL` | yes | Base URL of the ephemeral `slack-bridge` under test (Socket Mode attach point; exposes `/healthz` + `/readyz`). |
| `SLACK_E2E_CHANNEL` | yes | `C0…` id of the test channel the app was invited to. |
| `BEADS_HTTP_URL` | no | Beads daemon HTTP base URL (default `http://localhost:19090`) — assert bead reactions. |
| `BEADS_AUTH_TOKEN` | no | Bearer for the daemon (empty under auth-bypass). |
| `SLACK_E2E_TEAM_ID` | no | Slack team id for a fully-qualified `/client/<team>/<channel>` deep link. |
| `SLACK_E2E_WORKSPACE_URL` | no | Slack web client base (default `https://app.slack.com`). |
| `SLACK_E2E_BOT_HANDLE` | no | Bot's Slack display name typed after `@` to summon it (default `gardenio`). Set when the test app was installed under a different name. |

## slack-bridge fixtures (auth, channel access)

`tests/fixtures.js` extends Playwright's `test` so every spec asserts on
behavior instead of re-plumbing auth/reachability. All four fixtures are lazy —
instantiated only when a test names one.

| Fixture | Provides | Guarantee |
| --- | --- | --- |
| `bridge` | request context bound to `SLACK_E2E_BRIDGE_URL` | `/healthz` OK **and** `/readyz` ready before the spec body runs |
| `beads` | request context bound to `BEADS_HTTP_URL` + bearer | daemon reachable/authorized for read-back assertions |
| `slackChannel` | resolved `SLACK_E2E_CHANNEL` | asserted well-formed `C0…` id (channel access) |
| `workspace` | authenticated Slack web `Page`, scoped to the channel | session injected; workspace DOM mounted, not the signed-out screen |

## Session expiry (the fragile part)

The captured `d`/`d-s` cookies expire on a rolling cadence (weeks-to-months).
When they lapse, auth lands on the Slack **login screen**. Two layers turn that
into a precise pointer instead of an opaque selector flake:

1. `globalSetup.mjs` asserts the `storageState` carries both `d` and `d-s`
   cookies **and that neither is past its `expires`** before any spec runs — a
   stale capture fails fast with
   `SESSION EXPIRED — refresh per slack-e2e-provisioning.md Part B`. A cookie
   expiring within 14 days logs a warning instead of failing.
2. The `workspace` fixture asserts the workspace DOM mounted (not the signed-out
   screen) and throws the same runbook pointer if it did not.

### Standalone liveness check (outside the e2e runner)

Both layers live in [`session-guard.mjs`](session-guard.mjs), which also works as
a CLI so anything that browses Slack as the test user (e.g. a `gb slack view`
screenshot tool, a UX-acceptance loop) can gate on it with just an exit code:

```sh
cd gardenio/tests/slack-e2e
# offline: cookies present + not expired (no browser, no network)
SLACK_E2E_STORAGE_STATE=/path/to/storage-state.json npm run -s check-session
# online: also load app.slack.com in headless chromium and confirm the
# workspace mounts rather than the signed-out screen (catches revocation)
node session-guard.mjs --storage-state /path/to/storage-state.json --probe --channel C0XXXXXXX
```

Exit codes: `0` live · `2` `SESSION EXPIRED — refresh per slack-e2e-provisioning.md
Part B` · `3` misconfigured (no path / unreadable file / bad flag). `--json`
prints a machine-readable status line. From Node, import `assertStorageStateLive`,
`probeLiveness` and `SessionExpiredError` (match on `err.code === 'SESSION_EXPIRED'`).
A Slack outage or selector drift during `--probe` is deliberately *not* reported
as expiry. Unit tests: `npm run test:unit`.

**Fix:** re-capture and rewrite the one SM value per
[`slack-e2e-provisioning.md` Part B](../../docs/slack-e2e-provisioning.md).

## Layout

```
gardenio/tests/slack-e2e/
├── package.json
├── playwright.config.js     # storageState from env; chromium; serialized
├── globalSetup.mjs          # fail-fast env + session-liveness gate
├── session-guard.mjs        # reusable liveness check + CLI (+ session-guard.test.mjs)
├── tests/
│   ├── _slack.js            # env contract + centralized app.slack.com selectors (qa)
│   ├── _helpers.js          # reusable Slack interaction helpers
│   ├── fixtures.js          # slack-bridge fixtures: bridge / beads / slackChannel / workspace
│   ├── smoke.spec.js        # Tier-0 smoke — proves the harness pieces are reachable
│   └── mention-card.spec.js # Tier-0 behavior — @mention spawns agent → card renders (real DOM)
└── README.md
```

`mention-card.spec.js` is the harness's first end-to-end BEHAVIOR proof (task
`gd-QgcuQWQjhm`): a real test human user types `@gardenio <prompt>` in the real
`app.slack.com` client (via the composer autocomplete, so the token resolves to
a genuine `<@Uxxx>` mention — a literal "@gardenio" string spawns nothing), and
the spec asserts the bridge posted an agent status card back into the thread by
matching the card's DOM marker `Decisions thread below`
(`buildAgentCardBlocks`), plus a bot-posts-message read-back sanity.

Later tiers (decision buttons, modal submits, slash commands — plan §Coverage
roadmap) add one spec file per concern under `tests/`. Keep every
`app.slack.com` selector in `tests/_slack.js` so a Slack DOM change is a
one-file fix.
