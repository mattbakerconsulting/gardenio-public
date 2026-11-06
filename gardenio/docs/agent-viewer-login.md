# Agent login to beads-viewer (pod-side, for troubleshooting)

**Task:** ka-0000000118 · **Identity mechanism:** ka-0000006288 / epic ka-0000007323

This doc explains how an in-cluster **agent pod** authenticates to beads-viewer on
new prod (`gardenio.example.com`) so an agent can navigate beads and logs while
troubleshooting — without a human, a browser, or an AWS Verified Access session.

## TL;DR (agent, inside a pod)

```bash
gb viewer-login            # mint an audience-pinned SA token, persist a bearer session
gb viewer-login --check    # additionally probe /health and fail loudly if rejected
gb viewer-login --status   # show the current session + expiry

# raw token for ad-hoc curl against the in-cluster viewer:
TOKEN=$(gb viewer-login --print-token 2>/dev/null)
curl -H "Authorization: Bearer $TOKEN" \
  http://gardenio-beads-viewer.gardenio-system.svc.cluster.local:8095/health
```

Re-run `gb viewer-login` any time to refresh (the token TTL is short; the command
is idempotent). Subsequent `kd` / `gb` / curl calls replay the stored bearer.

## On k3s: the read-only verifier (P9.2, gd-6ujfeTZfNL)

The SA-token path below is the EKS design. On the k3s deploy the viewer runs
`devAuthBypass` behind the edge, the apiserver refuses anonymous OIDC discovery,
and every viewer daemon read needs a user bearer. So there a fleet agent checks
a page **on its own per-agent token** instead:

```bash
curl -s -o /dev/null -w '%{http_code} %{redirect_url}\n' \
  -H "Authorization: Bearer $BEADS_AUTH_TOKEN" "$BEADS_VIEWER_INTERNAL_URL/decisions"
```

`verifierMiddleware` (`cmd/beads-viewer/verifier.go`) admits such a direct call
only when it is a GET or HEAD, the daemon's `/v1/me` names the token an
**unrestricted** per-agent token, and the agent bead's role is on the viewer
tunable `viewer.verifier_roles` (comma-separated, default empty = off). Reads
go out on the agent's bearer, so kbeads authorizes them. The live farm is also
open (gd-TRdxFvBjW2): `GET /garden/farm`, `/garden/farm/events` and
`/garden/live/events`, plus the two POSTs that only steer the caller's own
stream (`/garden/farm/events/view`, `/garden/live/sub`). A shared connection
carries only the farm stream, and the farm view opens no transcript. Agent
terminals and transcripts, the Grafana and chart proxies, and the Lands GitHub
and accept callbacks stay refused. Turn it on with
`kd update gd-LceChJ7VXe -f viewer.verifier_roles=crew,infra` (an admin write).

Read-only kubectl for crews is the role field `verifier=true`: the controller
runs that role's pods as the chart's `gardenio-verifier` ServiceAccount
(`agents.verifier`, the infra-reader roles: no secrets, no writes) with its
token mounted, only when the chart sets `GARDENIO_VERIFIER_SERVICE_ACCOUNT`.

## Why not `gb login`?

`gb login` is for **humans**: a Keycloak device-grant sign-in that a person
completes in their own browser. An agent pod has no browser and no human session
— only a projected Kubernetes ServiceAccount token — and must never ask a human to
confirm a device code for it. So agents need a different door.

## How it works

beads-viewer accepts **two** verified token issuers (`internal/oidcauth`):

1. **Human path** — AVA / Microsoft Entra, at the public edge (`gardenio.example.com`).
2. **Machine path** — an audience-pinned **projected EKS ServiceAccount token**
   (`oidcauth.K8sSAConfig`, `cmd/beads-viewer/k8ssa.go`). Real verification: JWKS
   discovery + `iss` + `aud` + `exp` + signature against the EKS OIDC issuer. It is
   **not** a trusted header.

`gb viewer-login` drives the machine path:

1. Mints an audience-pinned token for the pod's own ServiceAccount via the
   Kubernetes **TokenRequest API** (`aud = gardenio-beads-viewer`). The token is
   signed by the cluster's EKS OIDC issuer — exactly the issuer the viewer trusts —
   and never touches node disk.
2. The viewer verifies the token, checks the `sub`
   (`system:serviceaccount:<ns>:<sa>`) against its **explicit allow-list**, maps it
   to a synthetic `<ns>.<sa>@<domain>` operator identity + a role floor, and serves
   the request.
3. `gb viewer-login` persists the token as a bearer session in the shared credstore,
   so it survives across `kd` / `gb` / curl invocations until it expires.

### Reach the viewer at the in-cluster Service, not the AVA edge

The machine path lives at the pod **behind** AVA. The public edge
(`gardenio.example.com/beads`) is fronted by AWS Verified Access, which `303`-redirects
any request lacking its own human session cookie — a machine bearer cannot traverse it.
So `gb viewer-login` defaults to the **in-cluster Service**:

```
http://gardenio-beads-viewer.gardenio-system.svc.cluster.local:8095
```

Override with `--viewer-url` only for a non-AVA host (e.g. a `cli.` carve-out).

### Which pods can do this

The pod's ServiceAccount must (1) be able to call the TokenRequest API and (2) be on
the viewer's `K8S_SA_ALLOWED_SUBJECTS` allow-list.

- gardenio-project agent pods run as **`gardenio-agent-cluster-admin`** with an
  automounted token → (1) holds today.
- Minting always succeeds for (1); an un-allow-listed subject mints a valid token
  and still gets **403** from the viewer. `gb viewer-login --check` surfaces that
  clearly.

## Operator enablement (one-time, privileged)

The verifier code + Helm wiring ship **default-OFF**. Turning it on for prod is two
operator actions:

### 1. Enable the SA-token branch on the prod deploy (terraform)

In `gardenio/terraform/composition/canonical-prod` set:

```hcl
beads_viewer_sa_login_enabled = true
# defaults are already prod-correct:
#   beads_viewer_sa_allowed_subjects       = ["system:serviceaccount:gardenio-system:gardenio-agent-cluster-admin"]
#   beads_viewer_sa_synthetic_email_domain = "acme.example"
```

then apply the gardenio module (prod deploy is `terraform apply -target=module.gardenio`,
STS-account-guarded to `123456789012`). This renders the `K8S_SA_*` env onto the
beads-viewer Deployment (issuer = the cluster's `module.eks.oidc_issuer_url`). With
`beads_viewer_sa_login_enabled = false` (the default) **no** `K8S_SA_*` env is rendered
and the deploy is byte-identical to today.

### 2. Provision the synthetic operator identity (beads daemon)

The viewer maps a verified subject to `<ns>.<sa>@<domain>`, e.g.
`gardenio-system.gardenio-agent-cluster-admin@acme.example`. That operator row must
exist on the daemon (kind `oidc`), or the resolver returns not-found and `/inbox`
shows the unprovisioned-operator banner. Provision it via the admin link-aliases
endpoint (requires an admin bearer):

```bash
curl -X POST -H "Authorization: Bearer $ADMIN_BEARER" \
  -H 'Content-Type: application/json' \
  --data '{
    "aliases": [
      {"kind":"oidc","subject":"gardenio-system.gardenio-agent-cluster-admin@acme.example"}
    ],
    "source":"agent-viewer-login-provision"
  }' \
  https://<daemon>/v1/admin/operators/link-aliases
```

> Negative operator resolutions are cached ~60s with no production `Invalidate`
> caller — allow a minute after provisioning before judging a 403.

## Verification

From a gardenio agent pod, after enablement + provisioning:

```bash
gb viewer-login --check
# → Minted audience-pinned token for system:serviceaccount:gardenio-system:gardenio-agent-cluster-admin ...
# → Viewer accepted the token (GET /health -> 200).
```

Before enablement, `--check` fails with `HTTP 401` (branch disabled) or `HTTP 403`
(subject not allow-listed) — both carry an actionable message pointing back here.

## Files

- `gardenio/controller/cmd/gb/viewer_login.go` — the `gb viewer-login` command.
- `gardenio/controller/cmd/beads-viewer/k8ssa.go` — the viewer-side SA-token verifier.
- `gardenio/helm/gardenio/templates/beads-viewer/deployment.yaml` — `K8S_SA_*` env (default-OFF).
- `gardenio/terraform/modules/gardenio-chart/` + `composition/canonical-prod/` — prod wiring.
