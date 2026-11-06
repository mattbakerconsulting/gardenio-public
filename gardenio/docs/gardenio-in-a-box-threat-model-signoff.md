# Gardenio-in-a-Box — Threat-Model Sign-Off (Fail-Closed Checklist)

> **Phase 5 capstone deliverable — P5.3 (ka-0000006635), epic ka-0000006354, mega-epic ka-0000001295.**
> **Operator:** Matt Baker (oidc:matthew.baker@acme.example)
> **Source threat model:** [`research/inbox-public-internet-exposure-hardening.md`](research/inbox-public-internet-exposure-hardening.md) (ka-0000003100)
> **Edge layer:** [`research/gardenio-in-a-box-ingress-edge-access-control.md`](research/gardenio-in-a-box-ingress-edge-access-control.md) (ka-0000002581)
> **Install runbook:** [`gardenio-in-a-box-install.md`](gardenio-in-a-box-install.md) (P5.1, ka-0000002846)

## Purpose

This document is the security **sign-off** for exposing a Gardenio-in-a-Box single-EC2
install to the public internet. It verifies that the **fail-closed checklist** from the
hardening threat model (`inbox-public-internet-exposure-hardening.md` §"Public-profile
fail-closed checklist", items **F1–F14**) holds on the **built artifact** — the merged
Phase 1–4 code, overlays, and bootstrap scripts as they exist on `main` — not on a design
sketch.

The threat model's core finding frames the whole exercise:

> The box is **fail-closed by *omission*** today (no app route renders without explicit
> enablement) — but "in a box" means turning services **on**, so it must become
> **fail-closed *by gate***.

So the sign-off question for each item is precise: **when an operator turns the box ON
with the in-box profile, does the dangerous-by-default behaviour invert to the safe
posture by configuration that ships in the repo — not by the operator remembering to set
it?** A control that exists only as prose in a research doc does **not** pass. A control
that is wired into `gardenio/helm/gardenio/values-inbox.yaml`,
`deploy-values/gardenio-inbox.yaml`, `scripts/k3s/install-host-hardening.sh`, or
`scripts/bootstrap-ec2.sh` **does**.

## Verdict

**SIGNED OFF — conditional on the documented operator prerequisites.**

All 14 fail-closed defaults (F1–F14) are inverted to their public-safe posture by
artifacts that ship on `main`. The box is safe to expose on the public internet **when
installed per the P5.1 runbook with the in-box overlay applied and host hardening enabled**
(`bootstrap-ec2.sh --harden` + the in-box values overlay). The residual risk is bounded and
carried by a short list of **operator preconditions** (§"Operator preconditions") and
**accepted residual risks** (§"Accepted residual risk"), none of which is a fail-*open*
default — each is either a launch-time action the operator must take (pin an EIP, inject an
SSH key, register the CrowdSec Traefik plugin) or a deliberately-deferred lower-severity
item.

The box must **not** be exposed publicly if installed with only the Phase-0 BYO profile
(`deploy-values/gardenio-byo.yaml`, no hardening) — that profile is "it installs and runs",
**not** public-safe, and the runbook and this sign-off both say so explicitly.

---

## The five trust boundaries (sign-off spine)

The threat model frames the box as five concentric boundaries an adversary must cross
(synthesis §1.2, hardening doc §"Threat Model"):

```
INTERNET → EDGE (Traefik/forwardauth) → CLUSTER (k3s services) → AGENT SANDBOX (gVisor) → NODE (k3s host + control plane + IMDS)
```

The fail-closed checklist maps onto these boundaries:

| Boundary | Controls | Checklist items |
|---|---|---|
| Internet → Edge | host firewall (only 22/80/443); TLS; forced HTTPS | F5, F6, F14 |
| Edge → Cluster | forwardauth on every route; no AVA holes; no header spoofing; rate-limit/body-cap; WAF-lite | F7, F8, F9, F10, F11, F12 |
| Cluster → Agent sandbox | gVisor runsc; NetworkPolicy default-deny | F1, F2, F3 |
| Agent → Node / control plane / IMDS | apiserver not public; NetworkPolicy egress exclusions; probe off | F1, F2, F4, F13 |

A pass on all four boundaries is the sign-off.

---

## Fail-closed checklist — line-by-line verification

For each item: **Today** (the stock chart/terraform default the threat model flags),
**In-box required** (the safe posture), and **Verified in** (the exact artifact on `main`
that enforces it, with the verdict).

### F1 — `networkPolicies.enabled`: `false` → **`true`** ✅

- **Verified in** `deploy-values/gardenio-inbox.yaml` → `networkPolicies.enabled: true`.
- Renders the `default-deny-ingress` policy + per-service allow rules + the agent-to-agent
  isolation that answers "a compromised agent pod cannot reach the control plane / IMDS /
  postgres / other agents." This is the single most important flip in the threat model
  (crown-jewel attack tree, hops 2–5). **PASS.**

### F2 — `networkPolicies.defaultDenyEgress`: `false` → **`true`** ✅

- **Verified in** `deploy-values/gardenio-inbox.yaml` → `networkPolicies.defaultDenyEgress: true`.
- Without it, any pod with no explicit `*-egress` policy has unrestricted egress (IMDS
  `169.254.169.254`, in-cluster postgres, arbitrary internet). With it on, the chart
  re-enables exactly what each component needs and the agent-egress policy keeps its
  IMDS/CGNAT/RFC1918 exclusions — making every agent-escape egress hop fail closed. **PASS.**

### F3 — `networkPolicies.traefik.{namespaceSelector,podSelector}`: empty → **k3s `kube-system`/traefik** ✅

- **Verified in** `deploy-values/gardenio-inbox.yaml` →
  `networkPolicies.traefik.namespaceSelector: {kubernetes.io/metadata.name: kube-system}`,
  `podSelector: {app.kubernetes.io/name: traefik}`; `kubeDns` selectors pinned to
  `kube-system`/`k8s-app: kube-dns`.
- This is the **correctness pairing** for F1/F2: with default-deny on but these selectors
  empty, every public route would 503 at the NetworkPolicy layer (Traefik→service ingress
  dropped) even though the IngressRoute is correct. The overlay sets them to the
  **k3s-bundled** Traefik's real labels, so the policy is both fail-closed *and* functional.
  **PASS.**

### F4 — kube-apiserver public exposure: EKS `0.0.0.0/0` → **bound loopback/private; never public** ✅

- **Verified in** `scripts/k3s/install-host-hardening.sh` (nftables INPUT default-drop;
  `:6443`, `:10250`, `:10256`, NodePort range all fall through to `policy drop` on the
  public NIC) **and** `scripts/bootstrap-ec2.sh` / `scripts/k3s/write-k3s-config.sh`
  (`--node-ip`/`--tls-san`/`--bind-address` pinning).
- The threat model is explicit that k3s cannot bind `:6443` to loopback (the supervisor
  needs it), so the **host-firewall drop on `:6443` from the public interface IS the
  control**. The hardening script implements exactly that; the operator reaches kubectl over
  an SSH tunnel to loopback. The overlay's NetworkPolicy comment confirms the controller
  reaches the apiserver via the in-cluster `kubernetes.default` Service, not a public
  endpoint. This is the deliberate **inversion** of the POC's `endpoint_public_access_cidrs
  = ["0.0.0.0/0"]`. **PASS** (firewall-enforced; see residual note on `--bind-address`).

### F5 — Host firewall: SG-only (cloud) → **nftables default-drop; only 22/80/443** ✅

- **Verified in** `scripts/k3s/install-host-hardening.sh`: a **separate** `inet gardenio_fw`
  table, `policy drop` on INPUT, accepting only loopback + established/related + the CNI
  interfaces (`cni0`/`flannel.1`) + `tcp dport {80, 443}` + rate-limited `:22`. FORWARD/NAT
  are left to k3s (never flushed) — the exact pattern the threat model prescribes to avoid
  breaking pod networking/DNS. Wired into bootstrap via `--harden`. **PASS.**
- The self-test the script prints is the sign-off's acceptance probe: *off-box `nc -vz <eip>
  6443 / 10250 / 5432` must FAIL while `443` succeeds; on-box `kubectl get pods -A` + pod DNS
  must still work.*

### F6 — SSH: (host) → **key-only, no password** ✅

- **Verified in** `scripts/k3s/install-host-hardening.sh`: writes
  `/etc/ssh/sshd_config.d/10-gardenio-hardening.conf` (`PasswordAuthentication no`,
  `PermitRootLogin no`, `AuthenticationMethods publickey`, single `AllowUsers <operator>`,
  `MaxAuthTries 3`, no TCP/agent/X11 forwarding, modern crypto), validates with `sshd -t`
  before reload, and adds fail2ban with the **nftables** banaction + journald backend (the
  legacy-vs-nft gotcha the threat model calls out) plus an always-on nftables `:22`
  rate-limit floor. **PASS.**
- **Precondition:** the operator account (`gardenio-op` by default) must exist with an
  `authorized_keys` injected at launch — **never bake a shared key into an AMI** (carried as
  an operator precondition below).

### F7 — `keycloak.forwardAuth.enabled` covers **all** routes: per-route gaps → **on + every browser route** ✅

- **Verified in** `deploy-values/gardenio-inbox.yaml` → `keycloak.forwardAuth.enabled: true`,
  `provider: oidc` (flipped back from the base overlays' `gitlab`), gated on the in-box
  Keycloak realm issuer; `landing.public: false` (landing page itself auth-gated);
  `unifiedHost` block documented to require `forwardAuth.enabled: true` if turned on.
- **Coverage is regression-guarded** by the P3.7 chart test
  `gardenio/helm/gardenio/tests/forwardauth_coverage_test.go` (ka-0000005775), which asserts
  every rendered non-allowlisted IngressRoute carries the forwardauth middleware. This is the
  mechanism that keeps F7 from silently regressing when a new route is added. **PASS.**

### F8 — `beads.ingress.skipForwardAuth`: `true` → **`false`** ✅

- **Verified in** `deploy-values/gardenio-inbox.yaml` → `beads.ingress.skipForwardAuth: false`.
- Prod sets it `true` because AVA/edge-auth fronts the beads daemon route; on the box there
  is no upstream gate, so the browser route MUST carry forwardauth. The daemon's own
  bearer-token/HMAC envelope still protects programmatic (`gb`/`kd`, CI) callers; this adds
  the oauth2-proxy gate in front for browsers. **PASS.**

### F9 — All `*.avaIngress.enabled` (AVA carve-outs): varies → **`false`** ✅

- **Verified in** `deploy-values/gardenio-inbox.yaml` → `beads.ingress.avaIngress.enabled:
  false`, `beadsViewer.avaIngress.enabled: false`, and `edgeAuth.ava.trustSigner: ""`
  (compiles the AVA verification branch out entirely). AVA terminates auth *upstream*; on a
  box with no AVA these routes would be **unauthenticated holes**. They default OFF in the
  base chart and are **pinned OFF** here so no base overlay can re-enable a hole. **PASS.**

### F10 — `trustForwardHeader` / Traefik `forwardedHeaders.trustedIPs`: trust on → **off / unset** ✅

- **Verified in** `deploy-values/gardenio-inbox.yaml` → `keycloak.forwardAuth.trustForwardHeader:
  false` (and the documented `unifiedHost.forwardAuth.trustForwardHeader: false`).
- With Traefik directly internet-facing (no upstream ALB), a client can forge
  `X-Forwarded-Proto: https` / spoof `X-Forwarded-For`; trusting those would corrupt the
  auth/redirect decision, the per-IP rate-limit, and HSTS scheme detection. Flipped off so
  the auth decision uses the real connection only. **PASS.**
- **Pairing (operator precondition):** the k3s-bundled Traefik's *static* config must set
  `forwardedHeaders.trustedIPs: []` and per-IP middlewares at depth 0 (RemoteAddr). The chart
  cannot inject Traefik static config; this is a `HelmChartConfig` the operator applies (see
  preconditions). The overlay flips the *chart-side* knob; the Traefik-side pairing is host-layer.

### F11 — `inFlightReq` + `buffering` middleware: absent → **add + default-on** ✅

- **Verified in** `deploy-values/gardenio-inbox.yaml` → `edge.inFlightReq.enabled: true`
  (`amount: 50`), `edge.buffering.enabled: true` (`maxRequestBodyBytes: 2000000`). Both are
  **new** Traefik Middlewares shipped by the chart (P3.2, ka-0000006519), gated default-OFF
  in the base chart (they would wrongly throttle a multi-replica ALB-fronted prod) and turned
  **on** for the single small box. **PASS.**

### F12 — CrowdSec WAF-lite: absent → **ship default-on, fail-open** ✅

- **Verified in** `deploy-values/gardenio-inbox.yaml` → `edge.crowdsec.enabled: true`,
  `mode: stream`, `failOpen: true` (P3.3, ka-0000001941). Ships the CrowdSec agent (LAPI)
  in-cluster + the Traefik bouncer Middleware on every route, streaming + fail-open so a
  CrowdSec hiccup never blackholes the box. **PASS.**
- **Precondition:** the bouncer is a Traefik **yaegi plugin** that must be registered in the
  k3s-bundled Traefik's *static* config (`experimental.plugins`) via a `HelmChartConfig`
  (the chart cannot inject Traefik static config). The overlay documents the exact
  `HelmChartConfig` with a pinned plugin version. Carried as an operator precondition.

### F13 — `gvisor.probe.create`: gated off (`name=""`) → **keep off unless freeze/thaw needed; pin probe image by digest** ✅

- **Verified in** chart defaults: `gvisor.namespace.name: ""` (`values.yaml:163`) renders the
  entire `gvisor-namespace.yaml` empty — the node-root probe DaemonSet
  (`hostNetwork`+`hostPID`+`privileged`+hostPath `/`) does **not** exist by default. The
  in-box bootstrap leaves gVisor OFF unless `--gvisor` is explicitly passed, and even then the
  `deploy-values/gardenio-gvisor-inbox.yaml` overlay enables only the **RuntimeClass-only**
  sandbox (single-node `scheduling: {}`), not the probe. The probe is the single most
  dangerous object in the chart (NetworkPolicy cannot protect a `hostNetwork` pod); keeping
  it absent is the control. **PASS** (off by default; see residual note on digest-pinning the
  probe image *if* an operator ever enables it).

### F14 — k3s ServiceLB `type:LoadBalancer` exposure: Klipper hostPort-like → **no LB services, or rely on F5 default-drop** ✅

- **Verified in** chart templates: a full grep of `gardenio/helm/gardenio/templates/` for
  `type: NodePort`, `nodePort:`, `hostPort`, `type: LoadBalancer` returns **nothing** — every
  Gardenio Service is `ClusterIP`. The only `hostNetwork` object is the gated-off gvisor-probe
  (F13). The **only** `type=LoadBalancer` on the box is the k3s-bundled Traefik (publishes
  :80/:443 via `svclb-traefik` hostPorts) — which is exactly the surface F5's default-drop
  intends. Nothing else flips to LoadBalancer, so the ServiceLB surface is bounded to
  80/443. **PASS.**

---

## Crown-jewel attack tree — post-hardening status

The threat model's central attack (agent escape → node / control-plane / IMDS) and the
control that blocks each hop, **as enforced by the merged in-box artifact**:

| Hop | Blocking control | Status with in-box profile |
|---|---|---|
| Agent code → break out of pod | gVisor `runsc` (userspace syscall interception) | Active when `--gvisor` (RuntimeClass-only); the primary isolation boundary |
| Escaped pod → node IMDS | agent-egress NetworkPolicy excludes `169.254.0.0/16` | **Active** (F1/F2 on); belt-and-suspenders IMDSv2 hop-limit=1 via hardening script (F4-adjacent) |
| Escaped pod → control plane (:6443) | agent-egress excludes RFC1918+CGNAT; apiserver not public | **Active** (F1/F2 on + F4 host-firewall drop) |
| Escaped pod → postgres / other services | postgres ingress allows beads only; agent-egress allow-list | **Active** (F1/F2 on) |
| Escaped pod → another agent | no agent↔agent ingress/egress | **Active** (F1/F2 on) |
| Escaped pod → gvisor-probe (node-root) | probe has no listener + controller-only `pods/exec` RBAC | **Best** — probe is **absent** by default (F13) |

**Net:** the fatal weakness the threat model identified — "the entire NetworkPolicy layer is
disabled by default" — is closed by F1/F2/F3 in the in-box overlay, and the most dangerous
object (the probe) is absent by default (F13). Every hop in the crown-jewel tree fails closed
with the in-box profile applied.

---

## Operator preconditions (carried risk, not fail-open defaults)

These are launch-time actions the operator MUST take. None is a software default that ships
open; each is a deployment-time input the box cannot synthesize for itself. They are the
explicit conditions of this sign-off.

1. **Apply the in-box overlay + enable hardening.** Install per the P5.1 runbook with
   `bootstrap-ec2.sh --harden` and the in-box values overlay. The plain BYO profile is **not**
   public-safe and must not be exposed. *(Without this, F1–F12 do not flip — this is the
   master precondition.)*
2. **Pin an Elastic IP** before issuing certs. An IP change reissues every sslip.io hostname
   and re-triggers HTTP-01 (TLS/DNS, F-adjacent). *(`values-inbox.yaml` TLS block precondition.)*
3. **Inject the operator SSH public key at launch** (EC2 key pair / cloud-init
   `ssh_authorized_keys`) into the `gardenio-op` account. **Never bake a shared key into an
   AMI** — a leaked image would unlock every box. *(F6 precondition.)*
4. **Register the CrowdSec bouncer plugin** in the k3s-bundled Traefik static config via the
   `HelmChartConfig` documented in `values-inbox.yaml` (pinned plugin version). Without it the
   `edge.crowdsec` Middleware references a plugin that isn't loaded. *(F12 precondition.)*
5. **Set Traefik `forwardedHeaders.trustedIPs: []`** in the same Traefik `HelmChartConfig`,
   pairing with `trustForwardHeader: false`. *(F10 host-side pairing.)*
6. **Install a cert-manager HTTP-01 ClusterIssuer** (`letsencrypt-http01`) and cert-manager
   CRDs before the chart's Certificate applies; ensure `:80` reaches the box and the
   `:80→:443` redirect does not swallow `/.well-known/acme-challenge/`. *(TLS precondition;
   the in-box HTTP-01 ClusterIssuer ships via P3.6b, ka-0000005963.)*
7. **Run the apiserver-exposure self-test** after install: from off-box, `nc -vz <eip> 6443 /
   10250 / 5432` must FAIL while `443` succeeds; on-box, `kubectl get pods -A` + pod DNS must
   still work. This is the F4/F5 acceptance probe and is the same assertion P5.2 (acceptance
   smoke, ka-0000005704) automates. *(Verification, not configuration.)*

---

## Accepted residual risk (deliberately deferred / out of scope)

These are known, lower-severity gaps the threat model itself flagged as non-blocking for a
single-tenant box on an isolated account. They do **not** block the sign-off; they are
recorded so the acceptance is honest.

- **`--bind-address=127.0.0.1` is not viable on k3s** (the supervisor needs `:6443`). The
  control for F4 is therefore the **host-firewall drop on `:6443`** from the public NIC, not a
  loopback bind. Accepted: the firewall drop is the threat model's prescribed enforcement and
  is verified (F4/F5). A second layer (AWS SG also restricting to 22/80/443) is recommended
  defense-in-depth but the box must be correct without it.
- **No container-image CVE scanner in CI** (no Trivy/Grype gate in `.rwx/docker.yml`). First-
  party images are tag-based; the air-gap path digest-pins via `global.airgap`. Accepted for
  the capstone; tracked as a hardening follow-up (patch-posture §, hardening doc). Not a
  runtime-exposure default.
- **gvisor-probe image (`public.ecr.aws/.../ubuntu:22.04`) is an external pull + patch
  surface** *if the probe is ever enabled*. Mitigated by F13 (probe absent by default) and the
  air-gap bundle path (pulls served from in-box zot). If an operator enables freeze/thaw, they
  must pin the probe image by digest and include it in the bundle. Accepted because the probe
  is off by default.
- **HSTS preload** must NOT be submitted for a single box (irreversible; tied to a possibly-
  ephemeral sslip.io IP). Accepted: the chart emits HSTS headers but the operator does not
  preload. Documented in the edge research (G9).
- **`'unsafe-inline'` in the default CSP `script-src`** (the Vite SPAs bundle inline JS). Low
  severity, tracked in the edge research (audit F6); not an exposure default.

---

## Sign-off statement

On the basis of the line-by-line verification above — every fail-closed default F1–F14
inverted to its public-safe posture by an artifact merged on `main`, the crown-jewel attack
tree closed at every hop with the in-box profile applied, and the residual risk bounded by a
documented set of operator preconditions and accepted lower-severity deferrals — the
Gardenio-in-a-Box single-EC2 install is **signed off as safe to expose on the public
internet** when, and only when, installed per the P5.1 runbook with the in-box overlay and
`--harden`.

This sign-off is **conditional**: it is void if the box is exposed with only the Phase-0 BYO
profile, if host hardening is skipped, or if the in-box values overlay is not applied. The
P5.2 acceptance smoke (ka-0000005704) is the operational gate that confirms these conditions
hold on a live box before public exposure.

**Verification artifacts** (all on `main` at sign-off):
`deploy-values/gardenio-inbox.yaml`, `gardenio/helm/gardenio/values-inbox.yaml`,
`scripts/k3s/install-host-hardening.sh`, `scripts/bootstrap-ec2.sh`,
`scripts/deploy-byo.sh`, `gardenio/helm/gardenio/tests/forwardauth_coverage_test.go`,
chart `templates/` (host-port grep clean), `gvisor.namespace.name` default empty.

Operator: Matt Baker
