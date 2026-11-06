# Gardenio in a Box — End-to-End Install Runbook

> **Phase 5 capstone deliverable — P5.1 (ka-0000002846), epic ka-0000006354, mega-epic ka-0000001295.**
> **Operator:** Matt Baker (oidc:matthew.baker@acme.example)
>
> The single authoritative runbook for the **scripted, one-shot, single-EC2** install
> that brings up the full gardenio stack on its own address, with **no AWS API at runtime**,
> **every public route auth-gated**, **only 80/443/22 reachable**, and **the k8s API not
> public**. This doc ties Phases 0–4 together into one documented procedure.

## What "Gardenio in a Box" is

A single memory-optimized EC2 instance running **k3s** (bundled Traefik + ServiceLB +
local-path-provisioner + CoreDNS) with the gardenio Helm chart installed on top via two
**in-box values overlays** that flip every AWS-coupled default to its in-box equivalent.
The box is reachable only on 80/443 (app) + 22 (operator SSH); the control plane, datastores,
and IMDS are all off the public interface.

Architecture and the chosen substitution for every external dependency:
[`gardenio-in-a-box-synthesis.md`](gardenio-in-a-box-synthesis.md) (§1 topology, §2
substitutions). Security sign-off against the fail-closed checklist:
[`gardenio-in-a-box-threat-model-signoff.md`](gardenio-in-a-box-threat-model-signoff.md) (P5.3).

### The phases this runbook integrates

| Phase | What it gave the box | Key artifacts |
|---|---|---|
| 0 — substrate | k3s + (optional) gVisor + scripted EC2 bootstrap | `scripts/bootstrap-ec2.sh`, `scripts/k3s/*`, `scripts/cloud-config/gardenio-inbox-user-data.yaml.tmpl` |
| 1 — storage & secrets | local-path SC, Retain for crown jewels, pg_dumpall DR, in-box-seeded secrets (no ESO/IRSA) | `gardenio/helm/gardenio/values-inbox.yaml`, `scripts/deploy-byo.sh` |
| 2 — object store | in-box MinIO instead of S3 | `templates/minio/*`, `inBox.objectStore.*` |
| 3 — edge hardening | forwardAuth on every route, no AVA holes, NetworkPolicy default-deny, CrowdSec, rate/body caps, sslip.io + HTTP-01 TLS, host firewall + SSH + IMDSv2 | `deploy-values/gardenio-inbox.yaml`, `scripts/k3s/install-host-hardening.sh` |
| 4 — air-gap images | in-box zot registry + containerd mirror; box pulls zero external images | `scripts/inbox/*`, `global.airgap` |

## The two overlay files (read this first)

The in-box profile is **not one file**. It is two values overlays with complementary
scopes, layered **both** on top of the base chart, in this canonical order — full detail in
[`inbox-overlay-layering.md`](inbox-overlay-layering.md) (ka-0000007248):

```
1. gardenio/helm/gardenio/values.yaml          base chart defaults
2. gardenio/helm/gardenio/values-inbox.yaml    storage + secrets + air-gap + TLS(sslip.io/HTTP-01)
3. deploy-values/gardenio-inbox.yaml          edge hardening + in-box Keycloak
```

Helm deep-merges later `-f` files over earlier ones. The handful of overlapping keys
(`postgres`, `storageClassRetain`, `backup`, `keycloak.runtime.persistence`) carry
**byte-identical** values in both files, so the merge is a no-op rather than a conflict; this
is regression-guarded by `gardenio/helm/gardenio/tests/inbox_overlay_layering_test.go`.

> **Applying only one overlay is not public-safe.** Only `values-inbox.yaml` → cloud edge
> (AVA trust signer, no NetworkPolicy, landing public). Only `deploy-values/gardenio-inbox.yaml`
> → AWS/EBS storage defaults + unpinned `externalSecrets` (may render `ExternalSecret` CRs
> with no backing store on a stock k3s box). **You need both.**

---

## Part A — The one-shot scripted install (the happy path)

This is the capstone deliverable: one `aws ec2 run-instances` call whose user-data brings the
box all the way up, public-safe, with no manual steps on the instance.

### A.0 Prerequisites the operator provides (off-box, once)

These are deployment-time **inputs the box cannot synthesize for itself**. None is a
fail-open software default — each is an operator action, and the install fails loudly (not
silently insecure) if one is missing.

1. **An AWS account + VPC subnet** to launch the instance in. (AWS is used only to *create*
   the instance and its EIP — there is **no AWS API call at runtime**.)
2. **A pinned Elastic IP.** sslip.io hostnames and the Let's Encrypt certs embed the IP; an
   IP change reissues every hostname + cert. Allocate the EIP *before* install and associate
   it at launch.
3. **An operator SSH keypair.** Inject the **public** key at launch (EC2 key pair or cloud-init
   `ssh_authorized_keys`) into the `gardenio-op` account. **Never bake a shared key into an
   AMI** — a leaked image would unlock every box.
4. **A security group** allowing inbound 22/80/443 only (defense-in-depth; the host nftables
   firewall is the source of truth — see A.4).
5. **A way to deliver the `gardenio` repo to the box.** The box needs a small subset of the
   `gardenio` tree at install time (`scripts/`, `gardenio/helm/gardenio/`, `deploy-values/`, and
   the install docs) so cloud-init can run `bootstrap-ec2.sh` and `deploy-byo.sh`. By default
   cloud-init *clones* the repo to `/opt/gardenio/repo` — but `gardenio` is **PRIVATE**, so on a
   truly fresh box with no credential and no staged tree that clone fails
   (`fatal: could not read Username for https://gitlab.com`) and the **entire** bootstrap
   `runcmd` aborts — nothing installs (ka-0000001441). Pick **exactly one** of:

   - **(a) Tarboat tarball — untar, no clone (TRUE air-gap default; Tarboat T1/G1.2,
     ka-0000008214).** The signed `gardenio-tarboat-<calver>.tar.zst` *carries* the repo subset
     at `repo/`, unpacked on the box under the offline-assets dir (default
     `/opt/gardenio/tarboat/repo`). cloud-init detects the staged tree on the sentinel
     `scripts/deploy-byo.sh` (**not** `.git` — a tarball tree never has one), symlinks it to
     `/opt/gardenio/repo`, and **never reaches the clone step**. This is the only path a friend
     with no GitLab access can use, and the right default for a real air-gap. No token, no
     network, no private-repo access required. The same offline-assets dir also holds the
     staged install-time binaries/manifests/charts (G1.1) and is passed through to
     `bootstrap-ec2.sh --offline-assets`; pin a non-default location with
     `render-user-data.sh --offline-assets DIR`.
   - **(b) AMI-bake / pre-stage the repo.** Bake a checkout into the AMI at
     `/opt/gardenio/repo`, or `scp`/copy it there before bootstrap. The user-data clone step
     **short-circuits** when `/opt/gardenio/repo/scripts/deploy-byo.sh` exists (an AMI-baked
     checkout's `.git` also satisfies the check) — no token, no network needed.
   - **(c) Pass a read-only clone token at render time.** Supply a GitLab **read-only deploy
     token** (or a PAT scoped to `read_repository`) so cloud-init can clone the private repo.
     Prefer the env var so the secret stays out of `ps`/shell history:

     ```sh
     GARDENIO_REPO_TOKEN=glpat-xxxxxxxx scripts/render-user-data.sh --scenario A ...   # preferred
     # or, equivalently:
     scripts/render-user-data.sh --repo-token glpat-xxxxxxxx --scenario A ...
     ```

     The token is written on the box to a `0600` git credential store consumed once during
     the clone (shell xtrace is disabled so it never lands in
     `/var/log/cloud-init-output.log`) and the store is deleted immediately after. **The token
     is baked into the rendered user-data file** — treat that file as a secret and delete it
     after launch. For a GitLab *deploy token* (username ≠ `oauth2`), also pass
     `--repo-token-user <deploy-token-username>`. This path requires the box to reach GitLab,
     so it is **not** an air-gap option — use (a) for a true air-gap.

   Do **not** hardcode any token into the repo or the template — it is supplied only at
   render time.
6. **(Air-gap only)** the OCI image bundle `gardenio-bundle-*.oci.tar.zst` staged on the box
   under `/opt/gardenio/bundle` (baked into the AMI or copied in before bootstrap). See Part D.

### A.1 Pick a sizing scenario

`scripts/bootstrap-ec2.sh --print-scenarios` (source: `inbox-ec2-sizing-bootstrap.md` §2):

| Scenario | Agents | Instance | vCPU/RAM | gp3 root |
|---|---|---|---|---|
| A | 1–2 | `r7i.2xlarge` | 8 / 64 GiB | 150 GiB |
| B | 4–6 | `r7i.4xlarge` | 16 / 128 GiB | 250 GiB |
| C | 8–12 | `r7i.8xlarge` | 32 / 256 GiB | 400 GiB |

The workload is RAM-bound (each agent pod reserves a hard 16 GiB), hence the `r7i` family.

### A.2 Render the public-safe user-data

```sh
GARDENIO_REPO_TOKEN=glpat-xxxxxxxx \
scripts/render-user-data.sh \
  --scenario A \
  --repo-ref main \
  --harden 1 \
  --operator-user gardenio-op \
  --app-version 2026.160.13 \
  --extra-values "gardenio/helm/gardenio/values-inbox.yaml deploy-values/gardenio-inbox.yaml" \
  > /tmp/user-data.yaml
```

`GARDENIO_REPO_TOKEN` is the read-only clone credential for the **private** `gardenio` repo
(prerequisite [§A.0](#a0-prerequisites-the-operator-provides-off-box-once) item 5 option (c)).
Omit it whenever the repo tree is already on the box — either tarball-staged under
`/opt/gardenio/tarboat/repo` (Tarboat air-gap default, item 5 option (a)) or AMI-baked at
`/opt/gardenio/repo` (option (b)). cloud-init short-circuits the clone on the sentinel
`scripts/deploy-byo.sh` (no `.git` needed), so a token is required **only** for the
internet-connected clone path (option (c)). When you do pass a token, the rendered
`/tmp/user-data.yaml` embeds it; treat that file as a secret and delete it after launch.

`--extra-values` (space-separated, repo-relative) is what layers the two in-box overlays in
order; `--harden 1` enables the host firewall + SSH + IMDSv2. The base `--values` stays
`deploy-values/gardenio-byo.yaml` (the chart's self-contained base) and the overlays flip it to
public-safe on top.

`--app-version` **pins the image tag** to a known-published calver instead of letting it
default to the chart's `appVersion` — **do not skip this on a fresh box.** See
[§A.2a](#a2a-pinning-a-known-good-image-tag-required-on-a-fresh-box) for how to pick the tag
and why it matters.

### A.2a Pinning a known-good image tag (required on a fresh box)

The chart's `appVersion` (in `gardenio/helm/gardenio/Chart.yaml`) is **not** a safe default for
the image tag on a fresh install. Every image reference floats to `appVersion` when no explicit
tag is set (`gardenio.imageTag` helper → `.tag | default .Chart.AppVersion`), but `appVersion`
is bumped at release time *before* the images for that tag are built and promoted to the
registry. So a hands-off install that defaults the tag to `appVersion` can reference a tag the
registry **does not have yet** → every gardenio-image pod `ErrImagePull` / `manifest unknown` on
a brand-new box. (Observed in P5.2 acceptance, ka-0000005704: `Chart.yaml appVersion` was
`2026.161.6` while the newest published tag was `2026.160.13`.)

**Pick the tag like this:** use the **latest promoted, non-`-rc` calver** — i.e. one the release
pipeline actually built, deployed to gardenio-rwx, and passed full E2E on (release steps 1–5;
golden promotion turns `<calver>-rc` into the bare `<calver>` tag only after E2E is green).
Confirm the tag exists in the registry before launch:

```sh
# what prod is running (a known-good, E2E-passed tag) — the safest single choice:
kubectl --context <prod-ctx> -n gardenio get deploy gardenio-controller \
  -o jsonpath='{.spec.template.spec.containers[0].image}'; echo
# or list promoted (bare, non-rc) tags in the registry and take the newest calver:
glab api "projects/Acme%2FPlatform%2Fgardenio/registry/repositories" \
  # then .../<repo-id>/tags and filter to YYYY.DDD.N (no -rc suffix)
```

**Where to override the tag** (three layers, pick the one that fits your entry point):

| Entry point | How to pin |
|---|---|
| One-shot user-data (A.2 above) | `render-user-data.sh --app-version <calver>` → flows to bootstrap → `deploy-byo.sh` |
| Manual bootstrap on the box | `bootstrap-ec2.sh --app-version <calver>` (or `APP_VERSION=<calver>` env) |
| Direct helm deploy | `deploy-byo.sh --app-version <calver>` (rewrites a temp chart's `appVersion`; tracked `Chart.yaml` is untouched) |
| Per-component override (advanced) | set `agents.image.tag` / `<component>.image.tag` in a values overlay passed via `--extra-values` |

`--app-version` is the blunt, whole-stack pin and the right default for in-box: it sets the tag
for **every** first-party image at once. The per-component `image.tag` keys exist for the rare
case where you need to mix tags (e.g. a hotfixed agent image on an otherwise-pinned stack).

> **Air-gap interaction:** in `--airgap` mode (Part D) the digest pins in `values-inbox.yaml`
> win over the tag, and `global.airgap: true` hard-disables `latestMode`, so the bundle's
> content is what runs regardless of `--app-version`. Pin `--app-version` to the **same calver
> the OCI bundle was built for** so the human-visible tag matches the bytes actually seeded into
> the in-box zot registry.

> The Phase-0 default (`--harden 0`, no `--extra-values`) renders an "it installs and runs"
> box that is **NOT public-safe**. Use the invocation above for any internet-facing box.

### A.3 Launch the instance

```sh
aws ec2 run-instances \
  --image-id <AL2023-or-Ubuntu-AMI> \
  --instance-type r7i.2xlarge \
  --key-name <your-keypair> \
  --security-group-ids <sg-22-80-443-only> \
  --subnet-id <subnet> \
  --block-device-mappings 'DeviceName=/dev/xvda,Ebs={VolumeSize=150,VolumeType=gp3,Encrypted=true}' \
  --metadata-options 'HttpTokens=required,HttpPutResponseHopLimit=1' \
  --user-data file:///tmp/user-data.yaml
# then associate the pre-allocated EIP:
aws ec2 associate-address --instance-id <id> --allocation-id <eipalloc-...>
```

`HttpTokens=required,HttpPutResponseHopLimit=1` is the authoritative IMDSv2 lockdown (the
hardening script also enforces hop-limit=1 from inside the box as belt-and-suspenders).

cloud-init runs `bootstrap-ec2.sh`, which executes (each stage idempotent):

```
os_prep → install_k3s → install_gvisor(opt) → harden_host → import_images
        → airgap_setup(opt) → install_operators(auto/opt) → deploy
```

`install_operators` (stage 4c) installs the A.5 prerequisite operators/CRDs (cert-manager,
ServiceMonitor CRD, EDP keycloak-operator) **before** the deploy — AUTO whenever an in-box
overlay is layered. See A.5.

Logs land in `/var/log/cloud-init-output.log` and `/var/log/gardenio-bootstrap.log`.

### A.4 One install prerequisite the chart fail-fasts on

The in-box overlay runs the **real** (non-dry-run) edge with **real** per-host TLS. Most of the
auth secrets are now **auto-seeded by the chart** (ka-0000000567) — only the env-specific TLS
host list still has to be supplied or `helm` aborts the render loudly (by design — fail-closed):

1. **Auth secrets — auto-seeded, no action needed (ka-0000003741).** When ExternalSecrets is off
   (the in-box default), the chart self-seeds the three load-bearing auth Secrets with
   `lookup`-then-`randAlphaNum` (preserved across `helm upgrade`, kept across `helm uninstall`),
   exactly like the postgres password and the keycloak admin Secret:
   - `gardenio-forwardauth-secrets` (`client-secret` + `cookie-secret`, for oauth2-proxy) —
     `templates/forwardauth/secrets.yaml`. The in-box KeycloakClient CR references the same
     `client-secret`, so the realm and oauth2-proxy agree with no hand-step.
   - `gardenio-edge-auth-hmac` (`hmac-key`, the shim↔daemon trust boundary) — same template.

   To supply your own HMAC key instead, pre-create the Secret and pass its name (this suppresses
   the self-seed): `--set edgeAuth.hmacKey.secretName=gardenio-edge-auth-hmac`. With
   `externalSecrets.enabled=true` (the cloud path) ESO owns all three and the chart self-seed is a
   no-op.
2. **TLS hosts** (`tls.mode: perHost` — Let's Encrypt HTTP-01 cannot issue a wildcard). Enumerate
   every served hostname so the single shared cert satisfies `sniStrict`, with `<dashed-ip>`
   being the EIP in dashed form (e.g. `203.0.113.7` → `203-0-113-7`):
   ```sh
   --set tls.mode=perHost \
   --set 'tls.hosts={auth.203-0-113-7.sslip.io,203-0-113-7.sslip.io,beads-viewer.203-0-113-7.sslip.io,mux.203-0-113-7.sslip.io,grafana.203-0-113-7.sslip.io}'
   ```
   (Without it: `tls.mode=perHost requires a non-empty tls.hosts list`.)

Set the matching IngressRoute hosts (`keycloak.runtime.host`, `forwardAuth` cookie/host,
`beadsViewer`/`coopmux`/`grafana` hosts) to the same `<dashed-ip>.sslip.io` names so the cert
SAN set and the routes agree. The TLS block in `values-inbox.yaml` documents the full host
list and the static-cert / air-gapped fallback.

> **Why the routes render even though `traefik.enabled: false` (ka-0000001357).** The base
> profile (`deploy-values/gardenio-byo.yaml`) correctly sets `traefik.enabled: false` because the
> box must **not** deploy a second Traefik — it uses the **k3s-bundled Traefik in `kube-system`**
> (the NetworkPolicy ingress selector in `gardenio-inbox.yaml` points at it). But the chart deploys
> **no** Traefik workload of its own, and the bundled Traefik still needs the chart's
> `IngressRoute` / `Middleware` / `TLSOption` CRs — without them it routes nothing, serves its
> default self-signed cert, never binds the Let's Encrypt perHost cert, and acceptance properties
> 1 (trusted cert) + 2 (auth-gating) are unsatisfiable. The chart therefore splits the flag:
> **`traefik.ingressRoutes.enabled`** gates CR emission independently of the workload flag, and the
> in-box edge overlay sets it `true`. You do **not** need a `--set traefik.enabled=true` workaround
> any more — the overlay handles it. (Leaving `traefik.ingressRoutes.enabled` unset falls back to
> `traefik.enabled`, so cloud / customer-poc overlays that front traffic with a different ingress
> controller are unchanged: `traefik.enabled: false` still emits zero Traefik CRs.)

> **Wiring this into the one-shot path:** the auth Secrets self-seed, so the only env-specific
> bit left is the TLS host list — add the `--set tls.domainName` / `--set tls.hosts` (and the
> matching IngressRoute hosts) to an env-overlay file passed via `--extra-values`. For a first
> install, filling those in by hand after SSH'ing in (then re-running the idempotent deploy stage)
> is the lower-risk path. If you want to pin your own HMAC key rather than the chart-generated one,
> pre-create `gardenio-edge-auth-hmac` and add `--set edgeAuth.hmacKey.secretName=...`.

### A.5 Prerequisite operators + CRDs (cert-manager, ServiceMonitor CRD, EDP keycloak-operator)

The two in-box overlays turn `keycloak.runtime` + `keycloak.forwardAuth` + `tls.mode=perHost`
(Let's Encrypt) + the per-component `serviceMonitor.enabled` toggles **ON**. The chart
therefore renders Custom Resources whose **CRDs + controllers are NOT part of stock k3s** and
are **NOT installed by `scripts/deploy-byo.sh`**. Apply all three **before** the gardenio
`helm upgrade`, or apply aborts at the build step with:

```
unable to build kubernetes objects from release manifest: ...
  no matches for kind "ClusterKeycloak"/"KeycloakClient"/"ServiceMonitor"/"Certificate" ...
  ensure CRDs are installed first
```

| Prerequisite | Provides the CR kinds | Pinned version |
|---|---|---|
| **cert-manager** (full manifest) | `cert-manager.io/v1` `Certificate` + `ClusterIssuer` | `v1.16.2` |
| **prometheus-operator ServiceMonitor CRD** (server-side apply) | `monitoring.coreos.com/v1` `ServiceMonitor` | `v0.76.0` |
| **epamedp/keycloak-operator** (helm, into `operators` ns) | `v1.edp.epam.com/v1` `ClusterKeycloak` / `KeycloakRealm` / `KeycloakClient` / `KeycloakRealmRole` / `KeycloakRealmGroup` / `KeycloakRealmIdentityProvider` | `1.32.0` |

> **The one-shot path does this for you.** `scripts/bootstrap-ec2.sh` runs an idempotent
> **stage 4c** that installs exactly these three, pinned, **before** the deploy stage. It is
> **AUTO**: it fires whenever an in-box overlay (`values-inbox.yaml` /
> `deploy-values/gardenio-inbox.yaml`) is layered via `--extra-values` — i.e. exactly when the
> CRs get rendered. Force it on a bare base profile with `--with-operators`; skip it (operator
> pre-installed them) with `--no-operators`; retarget the keycloak-operator namespace with
> `--operators-namespace NS`. So the public-safe invocation in the **Manual install** section
> below already installs all prerequisites with no extra steps.

The explicit `kubectl`/`helm` commands (for a by-hand install, or to pre-seed before a
`--no-operators` bootstrap) — these are exactly what stage 4c runs:

```sh
# 1. cert-manager v1.16.2 (full manifest), then wait for the webhook to be live
kubectl apply -f https://github.com/cert-manager/cert-manager/releases/download/v1.16.2/cert-manager.yaml
kubectl -n cert-manager rollout status deploy/cert-manager-webhook --timeout=180s

# 2. prometheus-operator ServiceMonitor CRD v0.76.0 (server-side — the schema
#    exceeds the client-side last-applied-annotation size limit)
kubectl apply --server-side -f \
  https://raw.githubusercontent.com/prometheus-operator/prometheus-operator/v0.76.0/example/prometheus-operator-crd/monitoring.coreos.com_servicemonitors.yaml

# 3. EDP keycloak-operator 1.32.0 into the operators namespace
kubectl create namespace operators --dry-run=client -o yaml | kubectl apply -f -
helm repo add epamedp https://epam.github.io/edp-helm-charts/stable && helm repo update epamedp
helm upgrade --install keycloak-operator epamedp/keycloak-operator \
  --version 1.32.0 --namespace operators --wait --timeout 5m
```

#### cert-manager HTTP-01 ClusterIssuer (TLS)

The perHost cert is issued by a cert-manager **HTTP-01 ClusterIssuer** named
`letsencrypt-http01` (P3.6 / P3.6b ships the in-box ClusterIssuer template, rendered by the
chart). With cert-manager installed per the table above, ensure:
- `:80` is reachable from the internet (it is — host firewall allows 80/443/22), and
- the catch-all `:80→:443` redirect does **not** swallow `/.well-known/acme-challenge/`
  (cert-manager's solver route outranks it; this is handled by the chart).

The in-box HTTP-01 ClusterIssuer needs **no DNS-provider credential** (that's the whole point
of sslip.io + HTTP-01 — no Route53, no ACM, no external-dns).

### A.6 CrowdSec + Traefik header pairing (edge prerequisites)

Two host-side Traefik static-config items the chart cannot inject (it manages only
IngressRoute/Middleware CRs). Apply via a `HelmChartConfig` on the k3s-bundled Traefik
(`kube-system/traefik`):

```yaml
apiVersion: helm.cattle.io/v1
kind: HelmChartConfig
metadata: { name: traefik, namespace: kube-system }
spec:
  valuesContent: |-
    # F12: register the CrowdSec bouncer yaegi plugin (pin the version).
    experimental:
      plugins:
        crowdsec:
          moduleName: github.com/maxlerebourg/crowdsec-bouncer-traefik-plugin
          version: v1.4.2
    # F10: direct exposure => never trust forwarded headers.
    additionalArguments:
      - "--entrypoints.web.forwardedHeaders.insecure=false"
      - "--entrypoints.websecure.forwardedHeaders.insecure=false"
```

(`edge.crowdsec.*` and `keycloak.forwardAuth.trustForwardHeader: false` in the overlay flip
the chart-side knobs; this is the Traefik-side pairing.)

---

## Part B — Verify the box (acceptance probes)

Run these after bootstrap completes. They are the operational gate before public exposure and
are the same assertions the P5.2 acceptance smoke (ka-0000005704) automates.

### B.1 Stack is up

```sh
ssh gardenio-op@<eip>
sudo KUBECONFIG=/etc/rancher/k3s/k3s.yaml k3s kubectl -n gardenio get pods   # all Running
sudo k3s kubectl -n gardenio get svc                                          # all ClusterIP
```

### B.2 Reachable over sslip.io with a publicly-trusted cert (no client trust import)

```sh
# from any machine — NOT the box, and with no custom CA in the trust store:
curl -sSI https://beads-viewer.203-0-113-7.sslip.io | head -1   # expect 302 -> Keycloak (auth-gated)
curl -sS  https://203-0-113-7.sslip.io/ -o /dev/null -w '%{http_code} %{ssl_verify_result}\n'
#   ssl_verify_result must be 0 (publicly-trusted LE cert; no -k needed)
```

A `302` to the Keycloak login (not a `200` with app data) proves the route is **auth-gated**
(F7/F8). The cert verifying with no `-k` / no imported CA proves the publicly-trusted TLS
requirement.

### B.3 Only 80/443/22 reachable; control plane + datastores private

```sh
# from off-box — these MUST fail:
for p in 6443 10250 5432 4222 8222; do nc -vz <eip> $p 2>&1 | tail -1; done   # all refused/timeout
nc -vz <eip> 443 2>&1 | tail -1                                               # succeeds
# on-box — pod networking/DNS still works (proves FORWARD/CNI weren't broken):
sudo k3s kubectl get pods -A | grep -v Running | grep -v Completed || echo "all healthy"
```

### B.4 No AWS API egress at runtime

```sh
# on-box: nothing should be dialing AWS endpoints. With defaultDenyEgress on, agent pods
# cannot reach IMDS / RFC1918 / the control plane at all. Spot-check:
sudo k3s kubectl -n gardenio get networkpolicy            # default-deny-ingress + -egress present
# from inside an agent pod (if any), IMDS must be blocked:
#   curl -m 3 http://169.254.169.254/latest/meta-data/   -> times out (NetworkPolicy excludes 169.254/16)
```

There is no ESO, no IRSA, no S3/SM/Route53/ACM client on the box: storage is local-path,
secrets are in-box-seeded, the object store is in-box MinIO, TLS is HTTP-01, and images are
served from the in-box zot registry (`--airgap`). The only AWS touch is launch-time
(`run-instances`, `associate-address`) — never at runtime.

### B.5 k8s API not public

```sh
# off-box: refused (B.3 covers :6443). Operator kubectl is over an SSH tunnel only:
ssh -L 6443:127.0.0.1:6443 gardenio-op@<eip>
# locally, with kubeconfig server https://127.0.0.1:6443:
kubectl get nodes
```

---

## Part C — Operator access after install

- **kubectl:** SSH tunnel to loopback `:6443` only (B.5). Never copy `/etc/rancher/k3s/k3s.yaml`
  to a laptop and repoint it at a public IP — `:6443` is firewall-dropped on the public NIC.
- **Browser:** `https://beads-viewer.<dashed-ip>.sslip.io` etc. — every route 302s to the in-box
  Keycloak login. First login uses the chart-generated first-admin (Secret
  `<release>-keycloak-first-admin`, see `deploy-values/gardenio-inbox.yaml` `keycloak.runtime.firstAdmin`).
- **`gb`/`kd` CLI:** `gb login` / `kd login`, the Keycloak device grant against the box's
  Keycloak. It needs the public `gardenio-cli` client (device grant only) in `keycloak.clients`,
  as `deploy-values/gardenio-k3s.yaml` declares it, and a daemon route that accepts the Keycloak
  bearer. The old cookie path (`gb login` capturing the `_gardenio_oauth2` cookie through the
  viewer's `/auth/cli-handoff`) was retired in gd-5FThTGbOOP.

---

## Part C.5 — In-box inference (in-cluster LiteLLM)

Epic ka-0000007994. The box runs a **first-class in-cluster LiteLLM proxy** so
agents do inference with NO external dependency beyond a single
LiteLLM→provider egress. `values-inbox.yaml` sets `litellm.enabled: true`; the
chart deploys `gardenio-litellm` (Deployment + ClusterIP Service on `:4000` +
ConfigMap for model routing), a `db-init` initContainer creates the `litellm`
database on the in-cluster postgres, and the controller auto-wires
`LITELLM_URL`/`LITELLM_ADMIN_SECRET` from `litellm.enabled`.

**Why a proxy-held provider key (not OAuth forwarding).** A fresh EC2 egress IP
is OAuth-IP-throttled by Anthropic — every `/v1/messages` returns
`rate_limit_error` — and 403'd by the corp LiteLLM WAF (ka-0000004450). A real
Anthropic API key (`sk-ant-api…`, sent as `x-api-key`) is **not** subject to
that Max-subscription OAuth IP throttle. So the in-box LiteLLM holds the
provider key itself (`litellm.extraEnv` → Secret
`<release>-litellm-provider-key`, key `anthropic-api-key`) and its models set
`api_key: os.environ/ANTHROPIC_API_KEY`. Agents never carry a provider key.

### Secrets (seeded by `scripts/deploy-byo.sh`, ExternalSecrets OFF in-box)

| Secret | Keys | Purpose |
|---|---|---|
| `<release>-litellm-master-key` | `master-key`, `salt-key`, `token` | LiteLLM master key (`token` → `LITELLM_MASTER_KEY`); also the controller alias-sync admin key and the value an account bead's `litellm_key` carries. Random `openssl` value, create-if-missing. |
| `<release>-litellm-provider-key` | `anthropic-api-key` | The upstream provider key the proxy authenticates with. Seeded from the deploy-time `ANTHROPIC_API_KEY` env; a placeholder is seeded if unset. |

**Supply the real provider key at install** (NEVER hardcode it in a tracked file):

```sh
# Operator supplies the sk-ant-api… key via env; deploy-byo.sh seeds it.
ANTHROPIC_API_KEY=sk-ant-api... scripts/deploy-byo.sh ...
# Or, on an already-installed box, recreate the Secret and roll the pod:
kubectl -n gardenio create secret generic gardenio-litellm-provider-key \
  --from-literal=anthropic-api-key=sk-ant-api... --dry-run=client -o yaml \
  | kubectl apply -f -
kubectl -n gardenio rollout restart deploy/gardenio-litellm
```

### Point a project's agents at the in-cluster endpoint (account bead)

`agents.litellmURL` already defaults the controller + agent pods to
`http://gardenio-litellm.gardenio.svc.cluster.local:4000`. To make `gb spawn`
resolve a model end-to-end, seed an in-box **account bead** (helper:
`scripts/inbox/seed-litellm-account.sh`):

```sh
# token == the litellm master key (gardenio-litellm-master-key / key `token`)
LITELLM_KEY=$(kubectl -n gardenio get secret gardenio-litellm-master-key \
  -o jsonpath='{.data.token}' | base64 -d)
kd create --type account --title claude-inbox-litellm \
  --field base_url=http://gardenio-litellm.gardenio.svc.cluster.local:4000 \
  --field model=claude-opus-5 \
  --field litellm_key="$LITELLM_KEY" \
  --label project:<your-project>
```

`applyAccountConfig` (gardenio/controller/cmd/controller/podspec.go) then bakes
`ANTHROPIC_BASE_URL` + `ANTHROPIC_MODEL` + `ANTHROPIC_CUSTOM_HEADERS`
(`x-litellm-api-key: Bearer <key>`) onto every agent pod for that project.
Requires `agents.enabled: true` (the controller); the public-box base profile
ships it off.

### NetworkPolicy (default-deny respected; minimal egress)

With `networkPolicies.enabled` + `defaultDenyEgress`, the chart renders:
- `<release>-litellm-ingress` — agents + controller may reach the proxy on `:4000`.
- `<release>-litellm-egress` — the proxy may reach the in-cluster postgres
  (`:5432`), kube-dns, and **only** `api.anthropic.com:443` (the single provider
  egress: `0.0.0.0/0` minus RFC1918/IMDS/CGNAT). Agent pods stay default-deny to
  the internet — they reach inference **only** via the in-cluster Service.

The single remaining operator action for live inference is supplying a real
`sk-ant-api…` key (above) **or** allowlisting the box EIP on whichever provider
endpoint the proxy targets.

---

## Part D — Air-gap mode (optional, the "truly air-gappable" capstone)

Add `--airgap` (or `--airgap-strict`) to the bootstrap and stage the OCI bundle:

```sh
# build the bundle at release time and copy it to /opt/gardenio/bundle on the box:
#   gardenio-bundle-<calver>.oci.tar.zst
# then bootstrap stands up zot (127.0.0.1:5000), seeds it, and redirects containerd:
sudo scripts/bootstrap-ec2.sh --scenario A --harden --airgap \
  --app-version <calver-the-bundle-was-built-for> \
  --extra-values gardenio/helm/gardenio/values-inbox.yaml \
  --extra-values deploy-values/gardenio-inbox.yaml
```

(Pin `--app-version` to the calver the OCI bundle was built for — see the air-gap note in
[§A.2a](#a2a-pinning-a-known-good-image-tag-required-on-a-fresh-box). The digest pins still win
at runtime; matching the tag just keeps the human-visible tag honest.)

`global.airgap: true` (set in `values-inbox.yaml`) hard-disables `latestMode` (digest pins
survive), flips the agent pull policy to `IfNotPresent`, and neutralizes `imagePullSecrets`.
`--airgap-strict` adds `--disable-default-registry-endpoint` so there is no upstream fallback.
Full air-gap mechanics: `scripts/inbox/*` and `inbox-image-distribution.md`.

### Two bundle artifacts: zot path vs `ctr`-import path

`.rwx/bundle.yml` emits **two** image artifacts from one dispatch (epic ka-0000002730):

| Artifact | Shape | Consumed by |
|---|---|---|
| `gardenio-bundle-<calver>.oci.tar.zst` | per-image OCI **layouts** + `manifest.tsv` | the zot path (`--airgap`) — `scripts/inbox/seed-bundle.sh` `crane push`es each into the in-box registry |
| `gardenio-bundle-<calver>.images.oci-archive.tar.zst` | one combined OCI **image-archive** (a *tarred* OCI layout) | the import-only path (`--airgap-import-only`) — `k3s ctr images import` |

The two shapes are **not interchangeable**: `ctr images import` wants a tarred OCI layout, not
the layout *directory* the zot path ships, and the archive deliberately preserves each image's
exact registry manifest digest (it carries the unmodified registry blobs, with each image's
`org.opencontainers.image.ref.name` annotation set to its original prod ref) — a `docker save`
tarball would recompute the manifest and break digest-pin resolution under `agentPullPolicy:
Never`. To load it on the box:

```sh
# strict-offline / minimal-footprint box (no zot, no mirror):
# `ctr` auto-decompresses zstd (containerd DetectCompression), so the .tar.zst
# imports directly — the archive is compressed WITHOUT --long precisely so it
# stays within containerd's zstd decoder window:
k3s ctr images import gardenio-bundle-<calver>.images.oci-archive.tar.zst
# (or drop it under /var/lib/rancher/k3s/agent/images/ for k3s auto-load on start)
```

`bootstrap-ec2.sh import_images()` already runs `k3s ctr images import` over `*.tar`/`*.tar.zst`
under `$IMAGE_DIR` (default `/opt/gardenio/images`), so staging the archive there is enough — no
manual `zstd -d` needed. The `--airgap-import-only` flag (ka-0000003856) makes this the *whole*
load path. The re-import-on-reschedule / GC-pinning recovery runbook is the next section.
Background + tradeoffs:
[`gardenio-in-a-box-ctr-import-eval.md`](gardenio-in-a-box-ctr-import-eval.md) §4.

### `--airgap-import-only`: registry-free mode (opt-in, OFF by default)

> **`--airgap` (zot) is the air-gap default and the right choice for almost every box.**
> `--airgap-import-only` is a strict-offline / minimal-footprint *alternative* for operators
> who want zero registry process at runtime and accept the reschedule/GC caveat below. The two
> flags are **mutually exclusive** — `bootstrap-ec2.sh` rejects passing both.

#### How import-only differs from zot mode

| | `--airgap` (zot, default) | `--airgap-import-only` |
|---|---|---|
| Runtime registry process | host `zot.service` (≈80 MB) on `127.0.0.1:5000` | **none** |
| containerd mirror redirect | yes (`configure-mirror.sh`) | none |
| Where images live | containerd store **+** durable zot registry | containerd store **only** |
| Bootstrap stage | `airgap_setup()` (4b) seeds zot | **skipped** — `import_images()` (4) + k3s auto-load dir only |
| Agent pull policy | `IfNotPresent` (miss → re-pull from loopback zot) | **`Never`** (no server to fall back to; miss → fail fast) |
| First-party pull policy | `IfNotPresent` | **`Never`** (via `deploy-values/gardenio-inbox-import-only.yaml`) |
| Pod reschedule / after GC | re-pulls from loopback zot ✅ | **breaks** — image gone, no server to re-pull from ❌ |
| Recovery | automatic | **manual** — re-run import + re-pin (below) |

In zot mode the containerd store is a *cache* in front of a durable local registry. In
import-only mode the containerd store is the *only* copy of every image — there is no server to
re-pull from — so a containerd image GC reclaim (under `DiskPressure`) or a pod reschedule onto a
freshly-pruned store wedges every affected pod into `ErrImageNeverPull` / `ImagePullBackOff` with
**no automatic recovery**. Two mitigations make this survivable; both are wired into
`bootstrap-ec2.sh --airgap-import-only` and re-applied on every (re-)import:

1. **GC pinning** — `scripts/inbox/pin-images.sh` labels every imported image
   `io.cri.containerd.pinned=pinned` (+ `io.cattle.k3s.pinned=pinned`). containerd's CRI image GC
   skips pinned images regardless of `DiskPressure`, so a routine GC sweep will not reclaim them.
2. **`agentPullPolicy: Never` + first-party `imagePullPolicy: Never`** — the kubelet never
   round-trips the (absent) registry, so a misconfigured box fails fast and visibly instead of
   hanging indefinitely on a dead endpoint. (`deploy-values/gardenio-inbox-import-only.yaml`,
   layered last so it wins precedence.)

#### ⚠️ NOT recommended for boxes that reschedule pods or upgrade in place

Pinning prevents *routine* GC, but it is not a substitute for a durable registry. Use
`--airgap-import-only` **only** for a box that is:

- **Never rescheduled** — a single-node appliance/demo where pods are not drained, evicted, or
  moved. (A node reboot is fine: the k3s auto-load dir re-imports on start, and bootstrap re-pins.)
- **Rebuilt, not upgraded in place** — a new image set means a new bundle + a fresh import + a
  re-pin + a manual prune of the old images. In-place `helm upgrade` to new digests will pull
  images the containerd store does not have, and under `Never` those pods will not start until
  you import the new bundle first.

If the box reschedules pods, autoscales, or takes in-place image upgrades, **use `--airgap`
(zot) instead** — its loopback registry makes all three recover automatically. This is the
explicit recommendation of the ctr-import eval
([`gardenio-in-a-box-ctr-import-eval.md`](gardenio-in-a-box-ctr-import-eval.md) §4).

#### Recovery: re-import on reschedule / after GC

Symptom: one or more pods stuck in `ErrImageNeverPull` / `ImagePullBackOff`, events showing
`Container image "…" is not present with pull policy of Never`. The image was reclaimed (GC) or
the pod landed on a store that never had it. Recover by re-importing the bundle and re-pinning —
the same two steps bootstrap runs, executable standalone on the box:

```sh
# 1. confirm what's missing (pinned images survive; reclaimed ones are gone):
sudo k3s ctr -n k8s.io images ls | grep -i pinned   # should list every bundle image

# 2. re-import the bundle (idempotent; ctr auto-decompresses .tar.zst):
#    the bundle should still be staged under $IMAGE_DIR (default /opt/gardenio/images);
#    if it was pruned, copy gardenio-bundle-<calver>.images.oci-archive.tar.zst back first.
sudo k3s ctr -n k8s.io images import \
  /opt/gardenio/images/gardenio-bundle-<calver>.images.oci-archive.tar.zst

# 3. RE-PIN — a fresh import lands UNPINNED and is immediately GC-eligible again:
sudo scripts/inbox/pin-images.sh

# 4. nudge the wedged pods to retry (delete → recreate picks up the now-present image):
kubectl -n gardenio delete pod <stuck-pod>            # or: kubectl rollout restart deploy/<name>
```

> **Always re-pin after any re-import (step 3).** `k3s ctr images import` does not preserve the
> pin labels; skipping the re-pin leaves the images GC-eligible and you will be back here after
> the next `DiskPressure` sweep. `pin-images.sh` is idempotent — running it when everything is
> already pinned is a no-op.

The k3s auto-load dir (`/var/lib/rancher/k3s/agent/images/`) re-imports staged tarballs on every
k3s start, so a **reboot** recovers GC'd images automatically (and bootstrap's
`import_images()` re-pins on the next run) — but a live **reschedule** without a restart needs
the manual steps above.

---

## Part E — Day-2: DR, restore, patching

- **DR floor:** an out-of-band EC2/EBS snapshot of the k3s data dir (ops action, not a runtime
  AWS dependency) + the in-cluster `pg_dumpall` CronJob (every 6h, 7-day retention, written to
  a Retain PVC). The crown-jewel stores (postgres/nats/keycloak) bind a `reclaimPolicy: Retain`
  StorageClass so a `helm uninstall` / PVC delete orphans rather than erases.
- **Restore:** whole-box EBS-snapshot + pg_dumpall + the NATS delete-then-restore caveat —
  [`gardenio-in-a-box-restore.md`](gardenio-in-a-box-restore.md).
- **Patching:** OS security packages auto-apply with a scheduled reboot window (k3s recovers
  cleanly with `--node-ip`/`--tls-san` pinned); k3s + image upgrades stay on a controlled
  cadence. Details in `inbox-public-internet-exposure-hardening.md` §"Patching".

---

## Manual install (without the one-shot user-data)

If you bootstrap by hand (already-running instance, or a dev box), the same scripts apply:

```sh
git clone https://github.com/mattbakerconsulting/gardenio.git
cd gardenio
# public-safe in-box install (run as root on the instance):
# --app-version pins the image tag to a known-published calver (see §A.2a) —
# omit it only if Chart.yaml appVersion is already known to be published.
sudo scripts/bootstrap-ec2.sh --scenario A --harden \
  --app-version 2026.160.13 \
  --extra-values gardenio/helm/gardenio/values-inbox.yaml \
  --extra-values deploy-values/gardenio-inbox.yaml
# Stage 4c AUTO-installs the A.5 prerequisite operators/CRDs (cert-manager v1.16.2,
# ServiceMonitor CRD v0.76.0, EDP keycloak-operator 1.32.0) before the deploy because an
# in-box overlay is layered. Then satisfy A.4 (edge-auth HMAC secret + tls.hosts) and the A.6
# Traefik HelmChartConfig, and re-run the deploy stage (idempotent) if you set them after the
# first deploy. (Pre-installed the operators yourself? Add --no-operators to skip 4c.)
```

For a **private** BYO cluster (not internet-facing), the Phase-0 base profile alone is correct
and simpler — see [`byo-cluster.md`](byo-cluster.md). That profile runs auth/TLS off and reaches
services via `kubectl port-forward`; it is explicitly **not** the public-box posture.

---

## Reference index

| Topic | Doc |
|---|---|
| Architecture + substitutions | [`gardenio-in-a-box-synthesis.md`](gardenio-in-a-box-synthesis.md) |
| Overlay layering (the two files) | [`inbox-overlay-layering.md`](inbox-overlay-layering.md) |
| Threat-model sign-off (F1–F14) | [`gardenio-in-a-box-threat-model-signoff.md`](gardenio-in-a-box-threat-model-signoff.md) |
| Public-exposure hardening research | [`research/inbox-public-internet-exposure-hardening.md`](research/inbox-public-internet-exposure-hardening.md) |
| Ingress / edge access control research | [`research/gardenio-in-a-box-ingress-edge-access-control.md`](research/gardenio-in-a-box-ingress-edge-access-control.md) |
| Restore / DR | [`gardenio-in-a-box-restore.md`](gardenio-in-a-box-restore.md) |
| In-box Keycloak | [`inbox-keycloak.md`](inbox-keycloak.md) |
| In-box object store (MinIO) | [`inbox-object-store-runbook.md`](inbox-object-store-runbook.md) |
| BYO (private cluster) | [`byo-cluster.md`](byo-cluster.md) |

Operator: Matt Baker
