# Controller → Coopmux Credential Push: Trust Boundary and Transport Posture

Source: secrets-research W9 ([ka-0000005140](https://beads.gardenio.e2e.example.com/i/ka-0000005140)).
Parent synthesis: [secrets-synthesis.md](../secrets-synthesis.md) Theme A
(encryption by default, fail-closed at startup).

## Scope

This doc covers the in-cluster channel:

| Side | Component | Source |
|---|---|---|
| Caller (client) | `gardenio-controller` | `gardenio/controller/internal/coopmux/client.go` |
| Callee (server) | `gardenio-coopmux`  | `coop/crates/mux/src/transport/http_cred.rs` |
| Service URL | `http://<release>-coopmux:<port>` | helper `gardenio.coopmux.serviceURL` in `_helpers.tpl` |
| Auth | Bearer token (`COOP_MUX_AUTH_TOKEN`) | `coopmuxTokenSecret` Helm value |

**Not in scope:** the public ingress in front of the coopmux dashboard
(`coopmux.ingress.host`). That path is TLS-terminated at the cluster edge
(Traefik) and gated by Keycloak ForwardAuth + OIDC. This doc is exclusively
about the **internal pod-to-pod credential-push hop**, which is plaintext
HTTP today.

## Endpoints carrying credential material

The controller calls coopmux on these paths (see `client.go`):

- `POST /api/v1/credentials/new`         — register an account.
- `POST /api/v1/credentials/set`         — push the raw API key / OAuth bearer token.
- `POST /api/v1/credentials/distribute`  — redistribute an existing key.
- `DELETE /api/v1/credentials/{account}` — drop an account.
- `GET  /api/v1/credentials/status`      — read-only.

The `set` payload carries the **raw provider credential** in the JSON body
(`{"account": "...", "token": "<plaintext-anthropic-key-or-oauth-token>",
"litellm_key": "<plaintext-litellm-virtual-key>"}`). Every other field on
the wire is metadata; `token` and `litellm_key` are the secret material.

## Current posture (production today)

- **Transport**: plain HTTP over the cluster pod network.
- **AuthN**: HTTP `Authorization: Bearer <COOP_MUX_AUTH_TOKEN>` (shared
  secret in K8s `Secret`).
- **AuthZ**: any caller that holds the bearer token can write to all
  credential endpoints — there is no per-path or per-method scoping.
- **Network restrictions**: a `NetworkPolicy` admits coopmux ingress only
  from labeled pods (controller, agents, Traefik) in the release namespace
  — see `gardenio/helm/gardenio/templates/networkpolicies/networkpolicies.yaml`
  block "Ingress from: controller, bridges, beads daemon, coopmux, agent pods".
- **Encryption-at-rest** on the coopmux side: AES-256-GCM if
  `COOP_CREDENTIAL_KEY` is set; plaintext fallback otherwise (W1, separate
  bead).

### What the attacker model assumes

In-cluster plaintext is acceptable today **only** under all of:

1. The pod network is treated as an isolation boundary (no untrusted
   workloads share the cluster).
2. The `NetworkPolicy` is installed and enforced by the CNI.
3. No host-network or hostPath workloads have read access to the pod-to-pod
   traffic.
4. The cluster does not span trust zones (e.g. a multi-tenant cluster where
   another team's namespace could ARP-spoof or run a privileged debugging
   pod).

If any of those degrades, the bearer token + the raw Anthropic key on the
wire are recoverable by anyone with on-cluster packet capture.

## Recommended posture

There are two paths that materially raise the bar without rewriting the
HTTP client and Axum server. Pick **one per environment** and declare it
in the deploy-values overlay (`coopmux.transport.posture`, see below).

### Posture 1 (preferred): service-mesh sidecar mTLS

Deploy the release namespace into a service mesh that does **STRICT mTLS**
at the pod-to-pod hop. No application change required.

- **Istio**: set `PeerAuthentication` mode `STRICT` on the namespace; the
  Envoy sidecar terminates / originates mTLS transparently. Workloads still
  speak plain HTTP to `localhost`.
- **Linkerd**: inject the namespace with the Linkerd proxy; meshed-to-meshed
  traffic is mTLS by default.

In either case the controller continues to `POST` plain HTTP to
`http://<release>-coopmux:<port>`, but the actual wire bytes are encrypted
and identity-bound by the mesh. SPIFFE workload identities replace the
shared bearer for in-mesh authN; the bearer can stay as a belt-and-braces
authZ check.

**Operator pre-conditions** (block enabling Posture 1 if any fail):

- Mesh control plane installed cluster-wide.
- Namespace labeled / annotated for injection.
- Mesh CA rotation policy in place (separate from the bearer rotation).

### Posture 2 (fallback): native TLS + client cert via cert-manager

Used when there is no service mesh in the cluster. Substantially more
moving parts:

- Provision a server cert for the coopmux Service via `cert-manager`
  `Certificate` with the in-cluster `Issuer`.
- Have coopmux's Axum server bind TLS using that cert (requires a Rust-side
  change — `axum-server` `bind_rustls`).
- Provision a client cert for the controller (separate `Certificate`).
- Have the Go client load the cert + a custom `tls.Config` with
  `Certificates` and `RootCAs` pinned (requires a Go-side change in
  `coopmux.Client`).
- Rotate both certs on the cert-manager renewal cadence; the controller
  must reload `tls.Config` without a pod restart (or accept a rolling
  restart on each renewal).

The chart MUST NOT silently flip from Posture 1/3 to Posture 2 — the
posture toggle in values fails closed if the matching `Certificate`
resources are absent.

### Posture 3 (development / single-tenant clusters): plaintext + NetPol

Plain HTTP + bearer + `NetworkPolicy`, as today. **Acceptable for**:
ephemeral `gardenio-rwx-*` namespaces, single-developer dev clusters, and
the `gardenio-rwx` staging environment. **Not acceptable for** any cluster
that hosts third-party workloads or carries customer data.

## Chart guidance

The chart declares the **intended posture** on a per-release basis. Wiring
up Posture 1 or 2 happens at the cluster level (mesh install, cert-manager
issuer) and is **out of scope for the chart** — the chart only carries
the declaration and the supporting comments/notes.

```yaml
# values.yaml
coopmux:
  # ...
  transport:
    # Required for production-class deployments.
    #   "service-mesh-mtls" — Istio / Linkerd terminates mTLS at the sidecar.
    #                        See gardenio/docs/security/controller-coopmux-mtls.md.
    #   "native-mtls"       — coopmux Axum server + Go client speak TLS
    #                        directly via cert-manager. NOT YET IMPLEMENTED
    #                        in code; declaring this posture without the
    #                        accompanying Certificate resources will fail.
    #   "dev-plaintext"     — plaintext HTTP + bearer + NetworkPolicy.
    #                        Acceptable for dev / ephemeral RWX namespaces only.
    posture: "dev-plaintext"
```

### Production overlays MUST declare `posture != "dev-plaintext"`

Production-class overlays (anything that runs against shared infra or
hosts customer credentials) MUST set `coopmux.transport.posture` to one of
`service-mesh-mtls` or `native-mtls`. Reviewing operators reject
production overlays that leave the value at the default.

The chart cannot fully enforce this — service-mesh injection is a
cluster-level concern — but it can:

- Emit a `NOTES.txt` warning when `posture: dev-plaintext` is rendered
  against a non-dev release name (best-effort hint only).
- Reject rendering when `posture: native-mtls` is set but the referenced
  `Certificate` secrets do not exist in the namespace (`required` template
  guard — to be added alongside the Rust/Go TLS implementation work).

## Open follow-ups (not done in W9)

W9 is scoped to **documentation + chart-level guidance**. Code-level work
to make Posture 2 actually function is tracked separately and is not
required to close this bead:

- Rust: `bind_rustls` on the coopmux credential router (`coop/crates/mux/src/transport/`).
- Go: `tls.Config` + cert reload on `coopmux.Client` (`gardenio/controller/internal/coopmux/client.go`).
- Helm: `Certificate` + `Secret` plumbing under `templates/coopmux/`.
- Cert-manager `Issuer` provisioning per cluster (operator runbook).

If/when those land, replace the "NOT YET IMPLEMENTED" note in `values.yaml`
with a pointer to the cert-manager issuer the operator must provision.

## Related

- [secrets-synthesis.md](../secrets-synthesis.md) — W9 row, Theme A.
- [secret-rotation.md](../secret-rotation.md) — bearer-token rotation cadence.
- `gardenio/helm/gardenio/templates/networkpolicies/networkpolicies.yaml` — the
  NetworkPolicy that Posture 3 relies on.
- `gardenio/controller/internal/coopmux/client.go` — credential-push client.
- `coop/crates/mux/src/transport/http_cred.rs` — credential endpoints on
  the coopmux side.
