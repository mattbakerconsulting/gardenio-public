# In-pod authenticated fetch of PROD beads-viewer (agent self-service)

Quick reference for an agent that needs a **screenshot or DOM** of a
page on the **prod** beads-viewer
(`https://beads-viewer.gardenio.e2e.example.com`) — e.g. for CDQR
(canonical-page visual QA) or to eyeball a rendered bead/agent card.

> **TL;DR**
> ```bash
> # DOM of a canonical page:
> gardenio/tests/e2e/scripts/fetch-beads-viewer-prod.sh /p/gardenio
>
> # PNG screenshot of a bead page:
> gardenio/tests/e2e/scripts/fetch-beads-viewer-prod.sh \
>   --screenshot --out /tmp/bead.png /ka-0000002064
> ```
> Then attach the PNG to a bead with `gb image upload /tmp/bead.png`.

Bead: `ka-0000002064` (prod variant of Track B, `ka-0000008133`).

> **⚠️ Auth path being removed (T6 Keycloak permadelete, ka-0000006953).** The
> `client_credentials` mint flow this doc relies on (steps below, and the linked
> [`agent-pod-keycloak-auth.md`](agent-pod-keycloak-auth.md) +
> `lib-keycloak-client-creds.sh` helper) depends on the in-chart Keycloak realm +
> confidential client, which have been removed from the fleet chart
> (`templates/keycloak/*`), and the `lib-keycloak-token.sh`/minting helpers were
> retired in T6b (ka-0000001463). Do **not** treat this as the current supported
> path for the fleet viewer — see `agent-pod-keycloak-auth.md` (banner'd historical)
> for the deprecation context. Historical reference until the fleet in-pod fetch
> path is re-documented against the GitLab/Entra-gated viewer.

## Why you can't just curl the public host

Prod beads-viewer is OIDC-gated and **correctly** 403s unauthenticated
browser traffic (this is by design — see bug `ka-0000002285`, closed
working-as-intended). You cannot complete the interactive forwardauth
code flow from a pod, and presenting a bearer to the public ingress
fails anyway because the AWS ALB 308-redirects bearer requests before
they reach Traefik.

The supported path (the same one the scheduled info-density audit uses
against prod — epic `ka-0000005684`) is:

1. **Mint** a viewer-audience bearer via the `client_credentials`
   grant (no human, no password). The agent ServiceAccount already has
   narrow `get` RBAC on the operator-managed client secret. See
   [`agent-pod-keycloak-auth.md`](agent-pod-keycloak-auth.md) and
   advice `ka-0000006483`.
2. **Port-forward** straight to the beads-viewer ClusterIP, bypassing
   the ALB entirely.
3. **Present** the bearer to beads-viewer's own OIDC middleware on
   `localhost`, which accepts an `Authorization: Bearer …` header.

The helper script
[`fetch-beads-viewer-prod.sh`](../tests/e2e/scripts/fetch-beads-viewer-prod.sh)
packages all three steps into one command.

## The helper

```
gardenio/tests/e2e/scripts/fetch-beads-viewer-prod.sh [options] <path>
```

| Option | Meaning |
|---|---|
| *(default)* / `--dom` | Fetch rendered HTML. Writes to `--out` or stdout. |
| `--screenshot` | Capture a PNG. Requires `--out <file.png>`. |
| `--out <file>` | Output path (DOM file, or required for screenshot). |
| `--port <n>` | Local port for the forward (default `8095`). |
| `--no-pf` | Skip port-forward; use an existing `BEADS_VIEWER_URL`. Still mints + presents the bearer. |
| `-h`, `--help` | Usage. |

`<path>` is the beads-viewer route, leading slash optional:
`/p/gardenio`, `/ka-0000002064`, `/inbox`, `/agents?project=gardenio`,
`/advice`, …

### Requirements

- `bash`, `curl`, `jq`, `kubectl` — present in agent pods. `kubectl` is
  used for the port-forward and the K8s-Secret read (the agent SA can
  already `get` the one client secret it needs).
- **Screenshot mode** additionally needs the Playwright harness under
  `gardenio/tests/beads-viewer-screenshots`:
  ```bash
  (cd gardenio/tests/beads-viewer-screenshots && npm ci && npx playwright install chromium)
  ```
  DOM mode needs none of that.

### Auth knobs (rarely needed)

All Keycloak env from
[`lib-keycloak-client-creds.sh`](../tests/e2e/scripts/lib-keycloak-client-creds.sh)
passes through. Defaults already resolve to the prod `gardenio` realm and
the `gardenio-info-density-audit` confidential client, so in a prod
agent pod you normally pass **nothing** extra. `POD_NAMESPACE` is read
from the downward-API service-account file when unset.

| Env | Default |
|---|---|
| `KC_BASE_URL` | `https://auth.e2e.dev.acme.example` |
| `KC_REALM` | `gardenio` |
| `KC_CLIENT_ID` | `gardenio-info-density-audit` |
| `KC_CLIENT_SECRET` | *(unset — read from the operator-managed K8s Secret)* |
| `BEADS_VIEWER_SVC` | `svc/gardenio-beads-viewer` |
| `BEADS_VIEWER_PORT` | `8095` |
| `BEADS_VIEWER_SHOT_FULLPAGE` | *(unset; set to capture the full scrollable page)* |

## Worked example: screenshot a bead for a CDQR comment

```bash
# From inside a prod gardenio agent pod, in the repo root:
shot=/tmp/ka-0000002064.png
gardenio/tests/e2e/scripts/fetch-beads-viewer-prod.sh \
  --screenshot --out "$shot" /ka-0000002064

# Push it to the viewer's whitelisted S3 bucket and get a URL
# (see advice ka-0000002145):
gb image upload "$shot"
```

For a quick DOM grep (no browser needed):

```bash
gardenio/tests/e2e/scripts/fetch-beads-viewer-prod.sh /p/gardenio \
  | grep -o '<title>[^<]*</title>'
```

## Security posture / low-risk notes

- **No secrets committed.** The client secret is auto-rotated by the
  keycloak-operator and read at runtime from a K8s Secret the agent SA
  already has narrow `get` access to. The script never writes the token
  to disk.
- **Viewer-only.** The `gardenio-info-density-audit` client maps only
  the `gardenio-viewer` realm role — read-only against beads-viewer.
- **No change to prod OIDC posture.** Unauthenticated requests to the
  public ingress still 403; this path authenticates against the
  viewer's own middleware over a localhost port-forward.

## See also

- [`agent-pod-keycloak-auth.md`](agent-pod-keycloak-auth.md) — the
  Keycloak client + `client_credentials` helper in detail.
- [`info-density-audit-harness.md`](info-density-audit-harness.md) — the
  scheduled prod audit that proves this path end-to-end.
- Advice `ka-0000006483` — bearer-minting helper.
- Advice `ka-0000002145` — `gb image upload` to attach screenshots to beads.
- Origin bug `ka-0000002285`; operator decision `ka-0000005883`;
  parent epic `ka-0000008133`.
