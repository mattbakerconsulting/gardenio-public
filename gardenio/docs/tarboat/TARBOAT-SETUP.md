# Tarboat Setup — Deterministic Single-Prompt Runbook

> **Tarboat T2 / G2.2 deliverable — task ka-0000004969, epic ka-0000007759.**
> **Operator:** Matt Baker (oidc:matthew.baker@acme.example)
>
> This is the **agent-targeted, deterministic, single-prompt** setup runbook for the
> **gardenio-in-a-tar ("tarboat")** air-gapped distribution. It takes an operator (or the
> on-box driving agent) from an **unpacked, signature-verified tarball** to a **running,
> auth-gated gardenio box with working in-box inference** — with **zero ambiguous branches**,
> **zero public-internet fetches**, and **zero private-repo clones**.
>
> It is the inference-shaped sibling of the human-interactive
> [`gardenio-in-a-box-install.md`](../gardenio-in-a-box-install.md). Where that doc says
> "decide based on your judgment" or "do A.4 by hand then re-run," **this doc decides for you**
> and every command is copy-pasteable and idempotent.
>
> The single prompt that kicks off the on-box driving agent is
> [`TARBOAT-AGENT-PROMPT.md`](TARBOAT-AGENT-PROMPT.md) (G2.5). That prompt points the agent at
> **this file** and tells it to execute the steps below in order.

---

## 0. The contract this runbook honors (read once, then never branch)

The operator already settled the four forks the [gap analysis](../tarboat-gap-analysis.md)
left open. This runbook bakes those answers in so the agent never has to choose:

| Fork | Decision baked into this runbook |
|---|---|
| **Q1 — where the driving agent runs** | **On the box.** The agent runs as a normal shell user on the EC2 instance and drives the install locally. No laptop-over-SSH variant appears below. |
| **Q2 — internet-connected vs true air-gap** | **True air-gap.** Every install-time asset is staged in the tarball; the box makes **no** outbound fetch. TLS is a **self-signed local CA** (not Let's Encrypt), so no inbound `:80` ACME challenge and no outbound `:443` to LE. |
| **Q3 — `--airgap` (zot) vs `--airgap-import-only`** | **`--airgap` (zot).** The in-box loopback registry recovers automatically on pod reschedule / GC. Import-only is **not** used here. |
| **Q4 — AGPL-v3 MinIO in a redistributed tarball** | **Carried as-is.** MinIO ships AGPL-v3; its license travels in `LICENSES/` (see §1.3). No SeaweedFS swap for this release. |

The **one** runtime egress hole is the LiteLLM→Anthropic provider hop for inference (§7). A
fully-offline box with no inference is possible (skip §7), but the headline tarboat proof
is "an agent on the box completes one inference round-trip," so §7 is part of the happy path.

**Determinism rules this runbook follows (and the agent must too):**

1. **Every step is idempotent.** Re-running any step is safe; the bootstrap stages are
   individually idempotent and the `kubectl`/`helm` calls use apply/`--install`.
2. **Every step has an explicit success assertion.** A step is "done" only when its assertion
   prints the expected token. If an assertion fails, **stop and report** — do not improvise a
   workaround and do not proceed to the next step.
3. **No version is chosen at runtime.** The image tag, the staged binaries, and the chart are
   all pinned by the tarball's `<calver>` (§1.2). The agent reads `<calver>` from the tarball,
   never invents it.
4. **No secret is baked into the tarball.** The three operator-supplied secrets (§2) are
   generated on-box or passed in at setup time, never shipped.

---

## 1. What the operator unpacked (the tarball, verified)

The tarboat artifact is one signed file: `gardenio-tarboat-<calver>.tar.zst` (built by G1.3,
bead ka-0000000192). Before setup begins it must be **unpacked and integrity-verified**. The
operator (or the bootstrap that staged the tarball into the AMI) has already run:

```sh
# 1.1 — verify + unpack (run wherever the tarball landed; shown at /opt for the on-box case)
cd /opt
zstd -d -c gardenio-tarboat-<calver>.tar.zst | tar -x          # → gardenio-tarboat-<calver>/
cd gardenio-tarboat-<calver>
sha256sum -c SHA256SUMS                                        # MUST print "OK" for every line
```

`sha256sum -c SHA256SUMS` printing `OK` for every line is the **integrity gate**. If any line
prints `FAILED`, the tarball is corrupt or tampered — **stop**; do not install.

### 1.1 Tarball layout (what each directory is for)

```text
gardenio-tarboat-<calver>/
  README.md                 friend-grade front page (what this is, sizing, support)
  TARBOAT-SETUP.md          THIS runbook (the deterministic setup steps)
  TARBOAT-AGENT-PROMPT.md   the single prompt for the on-box driving agent (G2.5)
  MANIFEST                  every file + sha256 + provenance (calver, commit, build run id)
  SHA256SUMS                top-level checksum (verified in §1)
  LICENSES/                 bundled-image + component licenses + NOTICE (MinIO AGPL-v3 incl.)
  repo/                     the gardenio repo subset (scripts/ + gardenio/helm/ + deploy-values/)
  bin/                      k3s, k3s-install.sh, helm, crane, zot (pinned, checksummed)
  manifests/                cert-manager.yaml, servicemonitor-crd.yaml (pinned versions)
  charts/                   keycloak-operator-1.32.0.tgz
  images/                   gardenio-bundle-<calver>.oci.tar.zst (first-party + dependency images)
  k3s-airgap-images-<arch>.tar.zst   k3s system images (Traefik/CoreDNS/etc.)
```

> The `repo/`, `bin/`, `manifests/`, and `charts/` paths are exactly what the bootstrap's
> `--offline-assets` consumer (G1.1, bead ka-0000005747) expects. Do not rename them.

### 1.2 The one version you read, never choose

The tarball pins **one `<calver>`** across images + chart + scripts. The agent reads it from
the directory name (or `MANIFEST`) and uses it verbatim wherever this runbook says `<calver>`:

```sh
# from inside gardenio-tarboat-<calver>/ :
CALVER="$(sed -n 's/^calver: *//p' MANIFEST | head -1)"        # provenance field
test -n "$CALVER" || CALVER="$(basename "$PWD" | sed 's/^gardenio-tarboat-//')"
echo "tarboat calver: $CALVER"                                 # e.g. 2026.161.18
```

This is the answer to gap **G3.2** (implicit version pin): the friend never picks a tag.
`<calver>` here is `$CALVER` from this snippet in every command below.

### 1.3 Licensing (G3.3 / Q4)

`LICENSES/` carries each bundled image's license plus a top-level `NOTICE`. MinIO is
**AGPL-v3** and that license is included; it ships **off by network exposure** (in-cluster,
self-hosted, not offered to third parties). No action is required at setup — this section
exists so the operator knows the redistribution posture is already satisfied by the tarball
contents.

---

## 2. Operator-supplied inputs (the only things not in the tarball)

These four inputs the box **cannot synthesize for itself**. Capture them up front; every
later step references them. None is shipped in the tarball (§0 rule 4).

| # | Input | Why | How the agent gets it |
|---|---|---|---|
| 1 | **The box's reachable IP** (`$EIP`) | TLS SANs and the served hostnames embed it. On EC2 this is the associated Elastic IP. | Operator states it in the launch prompt, **or** the agent derives it (§2.1). |
| 2 | **An operator SSH public key** | So the operator can reach the box after install. | Injected at launch (EC2 keypair / cloud-init `ssh_authorized_keys`); **not** a setup step here. |
| 3 | **An edge-auth HMAC key** | The public edge runs real auth (`edgeAuth.dryRun: false`); the chart fails fast without it. | **Generated on-box** in §5 — never shipped, never chosen. |
| 4 | **A Claude OAuth token** (`sk-ant-oat01…`) | The on-box agent carries it as `CLAUDE_CODE_OAUTH_TOKEN`; the in-box LiteLLM forwards it upstream so on-box agents can think (§7). This org has no `sk-ant-api…` key — OAuth is the only credential type. | Operator pastes it into the setup prompt as the agent's OAuth token. **Never** baked into the tarball; nothing is seeded as a Secret. |

### 2.1 Derive the box IP deterministically

```sh
# Prefer the operator-provided value; else derive the primary public IPv4.
EIP="${EIP:-$(curl -s --max-time 3 http://169.254.169.254/latest/meta-data/public-ipv4 2>/dev/null)}"
test -n "$EIP" || EIP="$(ip -4 -o addr show scope global | awk '{print $4}' | cut -d/ -f1 | head -1)"
test -n "$EIP" || { echo "FATAL: no box IP — pass EIP=<ip> in the prompt"; exit 1; }
DASHED_IP="${EIP//./-}"                                        # 203.0.113.7 -> 203-0-113-7
echo "box IP: $EIP   dashed: $DASHED_IP"
```

> On a true air-gap box with no IMDS, `EIP` **must** be supplied in the prompt. The
> `sslip.io` hostnames are still used as stable local names (resolvable on the box via
> `/etc/hosts` — see §6.1); they need not be publicly resolvable for a local-CA install.

---

## 3. Preconditions check (assert before touching the box)

Run this block first. It asserts the box is the shape the runbook assumes. Any `FATAL`
means stop and report — do not proceed.

```sh
set -u
cd /opt/gardenio-tarboat-"$CALVER" || { echo "FATAL: unpacked tarball not found"; exit 1; }

# 3.1 — running as root (bootstrap installs k3s + writes /usr/local/bin + nftables)
[ "$(id -u)" = "0" ] || { echo "FATAL: run setup as root (sudo -i)"; exit 1; }

# 3.2 — the staged repo tree is present (G1.2: no private-repo clone needed)
[ -f repo/scripts/bootstrap-ec2.sh ]   || { echo "FATAL: repo/scripts/bootstrap-ec2.sh missing"; exit 1; }
[ -f repo/scripts/deploy-byo.sh ]      || { echo "FATAL: repo/scripts/deploy-byo.sh missing"; exit 1; }

# 3.3 — the offline assets are present (G1.1: no public-internet fetch needed)
for f in bin/k3s bin/k3s-install.sh bin/helm bin/crane bin/zot \
         manifests/cert-manager.yaml manifests/servicemonitor-crd.yaml \
         charts/keycloak-operator-1.32.0.tgz \
         images/gardenio-bundle-"$CALVER".oci.tar.zst ; do
  [ -s "$f" ] || { echo "FATAL: required offline asset missing: $f"; exit 1; }
done

# 3.4 — enough RAM for scenario A (each agent pod reserves a hard 16 GiB; box wants 64 GiB)
mem_gib="$(awk '/MemTotal/{printf "%d", $2/1024/1024}' /proc/meminfo)"
[ "$mem_gib" -ge 60 ] || echo "WARN: ${mem_gib} GiB RAM < 64 GiB (scenario A target r7i.2xlarge)"

echo "PRECONDITIONS OK"
```

**Assertion:** the block prints `PRECONDITIONS OK` (a `WARN` on RAM is acceptable for a demo
box; a `FATAL` is not).

---

## 4. Stage the offline assets where the bootstrap consumes them

The bootstrap's `--offline-assets DIR` defaults to `/opt/gardenio/tarboat` and expects the
`bin/ manifests/ charts/` layout there; the staged **repo tree** must land at the auto-detected
`/opt/gardenio/repo`; the **zot image bundle** goes under `/opt/gardenio/images`, which §6 passes
to the bootstrap as `--bundle-dir` so `airgap_setup()` → `seed-bundle.sh` seeds the in-box zot
registry from it. Stage all three deterministically:

```sh
SRC="/opt/gardenio-tarboat-$CALVER"
mkdir -p /opt/gardenio/tarboat /opt/gardenio/images

# 4.1 — install-time binaries/manifests/charts → the --offline-assets dir
cp -a "$SRC"/bin       /opt/gardenio/tarboat/
cp -a "$SRC"/manifests /opt/gardenio/tarboat/
cp -a "$SRC"/charts    /opt/gardenio/tarboat/
chmod 0755 /opt/gardenio/tarboat/bin/*

# 4.2 — the gardenio repo subset → the repo dir (no .git; bootstrap accepts a staged tree)
rm -rf /opt/gardenio/repo && cp -a "$SRC"/repo /opt/gardenio/repo

# 4.3 — the OCI image bundle → IMAGE_DIR (bootstrap's airgap_setup seeds zot from here)
cp -a "$SRC"/images/gardenio-bundle-"$CALVER".oci.tar.zst /opt/gardenio/images/
# k3s system images → the k3s auto-load dir so k3s has Traefik/CoreDNS offline
mkdir -p /var/lib/rancher/k3s/agent/images
cp -a "$SRC"/k3s-airgap-images-*.tar.zst /var/lib/rancher/k3s/agent/images/ 2>/dev/null || true

# 4.4 — assert the staging is complete
[ -x /opt/gardenio/repo/scripts/bootstrap-ec2.sh ] || { echo "FATAL: repo not staged"; exit 1; }
[ -s /opt/gardenio/tarboat/bin/k3s ]               || { echo "FATAL: bin/ not staged"; exit 1; }
[ -s /opt/gardenio/images/gardenio-bundle-"$CALVER".oci.tar.zst ] || { echo "FATAL: image bundle not staged"; exit 1; }
echo "STAGING OK"
```

**Assertion:** prints `STAGING OK`.

> Why these exact paths: `bootstrap-ec2.sh` auto-detects the repo at `/opt/gardenio/repo`,
> defaults `--offline-assets` to `/opt/gardenio/tarboat`, and defaults `IMAGE_DIR` to
> `/opt/gardenio/images`. `seed-bundle.sh`'s own `BUNDLE_DIR` default is `/opt/gardenio/bundle`,
> so §6 passes `--bundle-dir /opt/gardenio/images` to point `airgap_setup()` at the staged zot
> bundle here. (The `k3s-airgap-images-*.tar.zst` staged into the k3s auto-load dir is a
> separate, k3s-internal set — Traefik/CoreDNS — not the gardenio image bundle.)

---

## 5. Seed the two install-time secrets the public edge requires

The in-box edge runs **real** auth and **real** per-host TLS, so the chart fail-fasts unless
both are present **before** the deploy. Seed them now (idempotent — recreate-if-exists), so
the single bootstrap invocation in §6 deploys clean on the first pass (no "do A.4 by hand,
re-run" branch).

```sh
export KUBECONFIG=/etc/rancher/k3s/k3s.yaml      # exists after §6.1 install_k3s; for a pre-seed,
                                                  # run §5 AFTER §6's k3s stage — see note below.
kubectl create namespace gardenio --dry-run=client -o yaml | kubectl apply -f -

# 5.1 — edge-auth HMAC key (input #3): generated on-box, never shipped
kubectl -n gardenio create secret generic gardenio-edge-auth-hmac \
  --from-literal=hmac-key="$(openssl rand -hex 32)" \
  --dry-run=client -o yaml | kubectl apply -f -

# 5.2 — (REMOVED) No LiteLLM provider-key Secret. This org has no sk-ant-api… key;
#        inference uses OAuth-token forwarding (forwardClientHeaders: true in
#        values-inbox.yaml), so the in-box proxy holds NO upstream credential. The
#        agent's own Claude OAuth token (input #4, §7) is the sole credential and is
#        forwarded upstream at request time — there is nothing to seed here.

kubectl -n gardenio get secret gardenio-edge-auth-hmac && echo "SECRETS OK"
```

**Assertion:** both secrets list and the block prints `SECRETS OK`.

> **Ordering note (deterministic):** these secrets need a live k3s API. The bootstrap in §6
> installs k3s as its first stage and is **idempotent**, so the deterministic order is:
> run §6 **once** (it installs k3s, then tries to deploy and may fail-fast on the missing
> secrets — that is expected and harmless), then run §5, then re-run the §6 deploy stage.
> To avoid the fail-fast entirely, the agent runs §6 with `--no-deploy`-style staging by
> invoking bootstrap up to k3s, but the simplest deterministic path is: **§6 (install_k3s
> completes) → §5 (seed secrets) → §6 again (idempotent; now deploys clean).** The §6 block
> below encodes exactly this two-pass flow so the agent does not have to reason about it.

---

## 6. Bootstrap the box (true air-gap, two idempotent passes)

This is the heart of the runbook. One script, one canonical invocation, run twice (the second
run is a no-op for everything already done and completes the deploy). It performs, in order:

```
os_prep → install_k3s → harden_host → import_images → airgap_setup(zot)
        → install_operators(cert-manager/ServiceMonitor/keycloak-operator) → deploy
```

### 6.1 Pass 1 — bring up k3s + registry + operators (deploy may fail-fast; that's expected)

```sh
cd /opt/gardenio/repo
export OFFLINE_ASSETS=/opt/gardenio/tarboat       # belt-and-suspenders; --offline-assets sets it too

sudo scripts/bootstrap-ec2.sh \
  --scenario A \
  --harden \
  --airgap \
  --airgap-strict \
  --offline-assets /opt/gardenio/tarboat \
  --bundle-dir /opt/gardenio/images \
  --app-version "$CALVER" \
  --extra-values gardenio/helm/gardenio/values-inbox.yaml \
  --extra-values deploy-values/gardenio-inbox.yaml \
  --extra-values /opt/gardenio/tarboat-tls.yaml \
  || echo "pass-1 ended (a deploy fail-fast on first run is expected before §5 secrets exist)"

export KUBECONFIG=/etc/rancher/k3s/k3s.yaml
kubectl get nodes | grep -q ' Ready' && echo "K3S READY"
```

**Wait — `/opt/gardenio/tarboat-tls.yaml` does not exist yet.** Create the **true-air-gap TLS
overlay** before pass 1. This is the one deterministic deviation from the internet-connected
box: **local-CA static cert instead of Let's Encrypt** (Q2). It disables the HTTP-01 issuer
and points the chart at a self-signed cert the box mints for itself:

```sh
# 6.0 — generate a self-signed local CA + leaf cert covering every served sslip.io host,
#        and write the TLS overlay that wires the chart to it (no Let's Encrypt, no inbound :80).
HOSTS="$DASHED_IP.sslip.io auth.$DASHED_IP.sslip.io beads.$DASHED_IP.sslip.io \
beads-viewer.$DASHED_IP.sslip.io coopmux.$DASHED_IP.sslip.io grafana.$DASHED_IP.sslip.io \
landing.$DASHED_IP.sslip.io"

workdir="$(mktemp -d)"; cd "$workdir"
# CA
openssl req -x509 -newkey rsa:4096 -nodes -keyout ca.key -out ca.crt -days 3650 \
  -subj "/CN=gardenio-tarboat-local-ca/O=gardenio" >/dev/null 2>&1
# leaf with all hosts as SANs
san="$(for h in $HOSTS; do printf 'DNS:%s,' "$h"; done)IP:$EIP"
openssl req -newkey rsa:2048 -nodes -keyout tls.key -out tls.csr \
  -subj "/CN=$DASHED_IP.sslip.io" >/dev/null 2>&1
openssl x509 -req -in tls.csr -CA ca.crt -CAkey ca.key -CAcreateserial -days 825 \
  -extfile <(printf 'subjectAltName=%s' "$san") -out tls.crt >/dev/null 2>&1
# full chain for the leaf secret (leaf + CA) so clients that trust the CA verify the chain
cat tls.crt ca.crt > tls.fullchain.crt

export KUBECONFIG=/etc/rancher/k3s/k3s.yaml
kubectl create namespace gardenio --dry-run=client -o yaml | kubectl apply -f - 2>/dev/null || true
kubectl -n gardenio create secret tls gardenio-tarboat-tls \
  --cert=tls.fullchain.crt --key=tls.key --dry-run=client -o yaml | kubectl apply -f - 2>/dev/null || true

# the box-override overlay: static existing cert (HTTP-01 issuer OFF, true
# air-gap) PLUS the per-box auth hosts. The gardenio-inbox.yaml edge overlay pins
# the auth host / cookie domain / oauth2 callback to the CLOUD placeholder
# auth.gardenio.e2e.example.com (a real per-env value it expects the operator
# to override here). Leave it unset and oauth2-proxy discovers OIDC against that
# cloud host — which resolves to a foreign endpoint that 404s the discovery URL,
# and oauth2-proxy has no discovery retry -> forwardauth CrashLoopBackOff blocks
# the whole deploy (ka-0000000068). Point every auth host at THIS box's apex.
# --ssl-insecure-skip-verify: discovery now hairpins through Traefik serving the
# local-CA leaf (not publicly trusted), so oauth2-proxy must skip verify on its
# server-to-server discovery/token calls (browsers still verify via the CA import
# in §9). The chart's forwardauth KeycloakClient auto-whitelists
# https://<forwardAuth.ingress.host>/oauth2/callback, so no hand-edited client
# redirectUris are needed.
cat > /opt/gardenio/tarboat-tls.yaml <<YAML
tls:
  enabled: true
  existingSecretName: "gardenio-tarboat-tls"
  clusterIssuer:
    create: false
keycloak:
  runtime:
    host: "auth.$DASHED_IP.sslip.io"
  forwardAuth:
    cookieDomain: ".$DASHED_IP.sslip.io"
    ingress:
      host: "$DASHED_IP.sslip.io"
    extraArgs:
      - "--cookie-csrf-per-request-limit=4"
      - "--ssl-insecure-skip-verify=true"
# Per-box gated beads-viewer host. The inbox overlay turns beadsViewer.enabled on
# (without a rendered viewer route a browser hits Traefik with no route -> 404);
# the host is only known at install, so pin it here to one of the tls.hosts SANs.
beadsViewer:
  ingress:
    host: "beads-viewer.$DASHED_IP.sslip.io"
YAML
# keep the CA where the operator can grab it for client trust import (§9)
cp ca.crt /opt/gardenio/tarboat-local-ca.crt
cd /opt/gardenio/repo
echo "BOX OVERRIDE OVERLAY OK (local CA at /opt/gardenio/tarboat-local-ca.crt)"
```

> The static-cert path is first-class in the chart: `tls.existingSecretName` makes the chart
> skip the HTTP-01 `ClusterIssuer` and bind your secret to every IngressRoute
> (`templates/_helpers.tpl` resolves the cert name to `existingSecretName`;
> `tls-clusterissuer.yaml` and `tls.yaml` both gate off when it is set). The
> `gardenio-inbox.yaml` edge overlay still provides forwardAuth-on-every-route, default-deny
> NetworkPolicy, and the host firewall — the cert *source* and the per-box auth *hosts*
> are what this overlay pins.

> **Note — keycloak realm reconciliation (ka-0000005282).** For the in-box realm to be
> created (and thus for oauth2-proxy/forwardauth to pass OIDC discovery instead of
> CrashLoopBackOff-ing on a 404 `"Realm does not exist"`), the EDP keycloak-operator must
> both (a) *see* the `KeycloakRealm`/`KeycloakClient` CRs the chart creates in the `gardenio`
> namespace, and (b) *authenticate* to the in-cluster Keycloak. Two things make that work,
> already wired into the canonical path — no manual step:
> 1. `bootstrap-ec2.sh` installs the operator with `clusterReconciliationEnabled=true`
>    (`WATCH_NAMESPACE=""` + ClusterRole), so its cache watches **all** namespaces — not just
>    the `operators` namespace it runs in. Without this the realm CRs in `gardenio` are never
>    reconciled (`ClusterKeycloak … CONNECTED=false`, empty CR status).
> 2. The chart mirrors the runtime admin `Secret` into the operator namespace via
>    `keycloak.runtime.clusterKeycloak.operatorNamespace: "operators"` (set in
>    `deploy-values/gardenio-inbox.yaml`). The operator resolves a `ClusterKeycloak`'s
>    `.spec.secret` in **its own** namespace, so the copy there is what lets it log in.
>
> If you ever see forwardauth crashlooping on `"Realm does not exist"`, verify both:
> `kubectl -n operators get deploy keycloak-operator -o jsonpath='{..env[?(@.name=="WATCH_NAMESPACE")].value}'`
> should be empty, and `kubectl -n operators get secret gardenio-keycloak-runtime-admin`
> must exist and match `kubectl -n gardenio get secret gardenio-keycloak-runtime-admin`.

> **Note on `kubectl` before §5:** §6.0 already creates the `gardenio` namespace, so you can run
> §5 immediately after §6.0 (k3s is up after pass-1's `install_k3s`). Deterministic order:
> **§6.0 (TLS overlay + namespace) → §6.1 pass 1 (k3s + registry + operators; deploy fail-fasts)
> → §5 (seed the two secrets) → §6.2 pass 2 (deploy completes).**

### 6.2 Pass 2 — complete the deploy (idempotent; now that §5 secrets exist)

```sh
cd /opt/gardenio/repo
export OFFLINE_ASSETS=/opt/gardenio/tarboat

sudo scripts/bootstrap-ec2.sh \
  --scenario A \
  --harden \
  --airgap \
  --airgap-strict \
  --offline-assets /opt/gardenio/tarboat \
  --bundle-dir /opt/gardenio/images \
  --app-version "$CALVER" \
  --extra-values gardenio/helm/gardenio/values-inbox.yaml \
  --extra-values deploy-values/gardenio-inbox.yaml \
  --extra-values /opt/gardenio/tarboat-tls.yaml \
  --set edgeAuth.hmacKey.secretName=gardenio-edge-auth-hmac

export KUBECONFIG=/etc/rancher/k3s/k3s.yaml
echo "DEPLOY PASS-2 EXIT: $?"
```

**Assertion:** pass 2 exits `0` and prints `DEPLOY PASS-2 EXIT: 0`. (`--airgap-strict` guarantees
that if any staged asset were missing the run would have hard-failed in §4's assertions, not
here.)

> **Why two passes and not one:** the chart fail-fasts on the missing edge-auth HMAC secret,
> and the secret needs a live API server, which only exists after `install_k3s`. Rather than
> interleave a `kubectl` step into the middle of the bootstrap (a branch), this runbook runs the
> idempotent bootstrap twice with the secret-seed (§5) between them. Every bootstrap stage is a
> no-op on the second pass except the deploy, which now succeeds. This is fully deterministic.

---

## 7. In-box inference (the headline: an on-box agent can think)

The box ships a **first-class in-cluster LiteLLM proxy** (`litellm.enabled: true` in
`values-inbox.yaml`). The proxy holds **no upstream credential** — this org has no `sk-ant-api…`
provider key. Instead it runs with `forward_client_headers_to_llm_api: true`, so it forwards the
on-box agent's **Claude OAuth bearer token** (input #4) straight through to `api.anthropic.com`.
The agent authenticates *to the proxy* with the LiteLLM virtual key via the `x-litellm-api-key`
header (set by the account bead in §7.2), which leaves the `Authorization: Bearer` slot free to
carry the OAuth token the proxy relays upstream. The **single** runtime egress hole is the
proxy → `api.anthropic.com:443` hop; agent pods stay default-deny to the internet and reach
inference only via the in-cluster Service.

```sh
export KUBECONFIG=/etc/rancher/k3s/k3s.yaml

# 7.1 — provider key: NONE. There is nothing to seed (no gardenio-litellm-provider-key Secret).
#        Inference authenticates with the agent's forwarded OAuth token. Just confirm the proxy
#        rolled out with header-forwarding enabled.
kubectl -n gardenio rollout status deploy/gardenio-litellm --timeout=180s
OAUTH_TOKEN="${CLAUDE_CODE_OAUTH_TOKEN:?input #4: a Claude OAuth token (sk-ant-oat01…) is required}"

# 7.2 — seed an in-box account bead so `gb spawn` resolves a model end-to-end. The agent reaches
#        the proxy with the LiteLLM virtual key in the x-litellm-api-key header (custom_headers),
#        and carries its OAuth token as CLAUDE_CODE_OAUTH_TOKEN (forwarded as Authorization: Bearer).
LITELLM_KEY="$(kubectl -n gardenio get secret gardenio-litellm-master-key -o jsonpath='{.data.token}' | base64 -d)"
kd create --type account --title claude-inbox-litellm \
  --field base_url=http://gardenio-litellm.gardenio.svc.cluster.local:4000 \
  --field model=claude-opus-4-8 \
  --field custom_headers="x-litellm-api-key:${LITELLM_KEY}" \
  --field oauth_token="$OAUTH_TOKEN" \
  --label project:tarboat 2>/dev/null || echo "(account bead may already exist — ok)"

# 7.3 — prove one inference round-trip THROUGH the in-cluster proxy. The OAuth token rides in
#        Authorization: Bearer (forwarded upstream); the LiteLLM virtual key authenticates the
#        client to the proxy via x-litellm-api-key. anthropic-beta:oauth-* + the Claude-Code system
#        prompt are required by Anthropic for OAuth tokens (verified working under ka-0000007708).
kubectl -n gardenio run litellm-probe --rm -i --restart=Never --env="OAUTH_TOKEN=$OAUTH_TOKEN" --env="LITELLM_KEY=$LITELLM_KEY" \
  --image="$(kubectl -n gardenio get deploy gardenio-litellm -o jsonpath='{.spec.template.spec.containers[0].image}')" \
  --command -- /bin/sh -c '
    curl -s -m 30 http://gardenio-litellm.gardenio.svc.cluster.local:4000/v1/messages \
      -H "Authorization: Bearer $OAUTH_TOKEN" \
      -H "x-litellm-api-key: $LITELLM_KEY" \
      -H "anthropic-beta: oauth-2025-04-20" \
      -H "anthropic-version: 2023-06-01" \
      -H "Content-Type: application/json" \
      -d "{\"model\":\"claude-opus-4-8\",\"max_tokens\":16,\"system\":\"You are Claude Code, Anthropic'\''s official CLI for Claude.\",\"messages\":[{\"role\":\"user\",\"content\":\"reply with the single word: tarboat\"}]}"
  ' 2>/dev/null | grep -qi tarboat && echo "INFERENCE OK" || echo "INFERENCE FAILED (check OAuth token + egress allowlist)"
```

**Assertion:** prints `INFERENCE OK`. A `FAILED` here usually means the OAuth token is expired /
rejected, the `x-litellm-api-key` virtual key is wrong, or the box EIP has been OAuth-IP-throttled
by Anthropic (`rate_limit_error`) — in which case request an EIP allowlist from Anthropic (there is
no static key to fall back to). The proxy → `api.anthropic.com:443` hop is the **only** legitimate
egress dependency of the whole box.

> The `<release>-litellm-egress` NetworkPolicy already allows the proxy to reach
> `api.anthropic.com:443` (and only that, plus in-cluster postgres + kube-dns). Nothing else on
> the box dials the internet.

---

## 8. Acceptance — the box is up and correctly locked down

These are the operational gate. They are the same posture assertions the box's acceptance
smoke (`scripts/inbox/acceptance-smoke.sh`) automates; run that script as the single-command
form, or the explicit probes below.

```sh
export KUBECONFIG=/etc/rancher/k3s/k3s.yaml

# 8.1 — every pod Running/Completed
kubectl -n gardenio get pods --no-headers | grep -vE ' (Running|Completed) ' \
  && echo "ACCEPTANCE: pods not all healthy — FAIL" || echo "ACCEPTANCE: all pods healthy — OK"

# 8.2 — routes are auth-gated (302 to Keycloak), TLS verifies against the LOCAL CA (§9 import)
curl --cacert /opt/gardenio/tarboat-local-ca.crt -sSI \
  "https://beads-viewer.$DASHED_IP.sslip.io" --resolve "beads-viewer.$DASHED_IP.sslip.io:443:$EIP" \
  | head -1 | grep -qE ' 30[12] ' && echo "ACCEPTANCE: auth-gated + local-CA TLS — OK" \
                                  || echo "ACCEPTANCE: route/cert — FAIL"

# 8.3 — only 80/443/22 reachable; control plane + datastores private (run from OFF-box ideally)
for p in 6443 10250 5432 4222; do
  timeout 3 bash -c "echo > /dev/tcp/$EIP/$p" 2>/dev/null && echo "ACCEPTANCE: port $p OPEN — FAIL" || true
done; echo "ACCEPTANCE: private ports closed — OK"

# 8.4 — default-deny NetworkPolicy present
kubectl -n gardenio get networkpolicy | grep -q default-deny \
  && echo "ACCEPTANCE: default-deny netpol — OK" || echo "ACCEPTANCE: netpol — FAIL"

# 8.5 — the canonical single-command form (preferred; wraps the above + more):
sudo /opt/gardenio/repo/scripts/inbox/acceptance-smoke.sh --eip "$EIP" --ca /opt/gardenio/tarboat-local-ca.crt \
  && echo "ACCEPTANCE SMOKE: GREEN"
```

**Release gate:** §8.1–8.4 each print `OK`, **and** §8.5 prints `ACCEPTANCE SMOKE: GREEN`, **and**
§7.3 printed `INFERENCE OK`. That conjunction is "the agent drove a fresh box from the tarball
to a running, locked-down, inference-capable gardenio." This is the from-scratch proof the
tarboat program gates release on (G2.3/G2.4 — the agent-executable validation harness wraps
exactly these assertions and its exit code is the gate).

---

## 9. Operator access after install (post-setup, for the human)

- **Trust the local CA** (so browsers/CLIs verify the box cert without `-k`): import
  `/opt/gardenio/tarboat-local-ca.crt` into the operator's trust store
  (macOS Keychain / `update-ca-certificates` / browser CA import). This is the one-time cost of
  the true-air-gap local-CA choice (Q2) — there is no publicly-trusted cert because there is no
  Let's Encrypt.
- **Resolve the hostnames:** the `sslip.io` names encode the IP, so `<dashed-ip>.sslip.io`
  resolves to `$EIP` from any machine that can reach the box; for a fully-offline operator
  laptop, add the hosts to `/etc/hosts` pointing at `$EIP`.
- **Browser:** `https://beads-viewer.<dashed-ip>.sslip.io` etc. — every route 302s to the in-box
  Keycloak login. First login uses the chart-generated first-admin (Secret
  `gardenio-keycloak-first-admin`).
- **`kubectl`:** SSH tunnel to loopback `:6443` only — `ssh -L 6443:127.0.0.1:6443 gardenio-op@$EIP`,
  then a local kubeconfig pointed at `https://127.0.0.1:6443`. `:6443` is firewall-dropped on the
  public NIC.

---

## 10. If a step fails (the only "branch," and it's not a fork)

There is exactly one decision the agent ever makes: **did the current step's assertion pass?**

- **Pass** → proceed to the next step.
- **Fail** → **stop**. Do not improvise. Capture the failing assertion's output plus the
  relevant log and report it. The logs to attach:
  - bootstrap: `/var/log/gardenio-bootstrap.log` and `/var/log/cloud-init-output.log`
  - a wedged pod: `kubectl -n gardenio describe pod <name>` + `kubectl -n gardenio logs <name>`
  - a helm render abort: the `helm` stderr the bootstrap printed (it names the missing
    secret/value verbatim — e.g. the HMAC key or `tls.hosts`).

The agent **never** edits tracked files, **never** chooses a version, and **never** reaches the
public internet to work around a missing asset — under `--airgap-strict` a missing staged asset
is a hard fail by design (that is what makes the air-gap true). A failure is a report, not a
fix-it-yourself.

---

## Reference index

| Topic | Where |
|---|---|
| The single prompt for the on-box driving agent | [`TARBOAT-AGENT-PROMPT.md`](TARBOAT-AGENT-PROMPT.md) |
| Gap analysis (why tarboat exists, the four forks) | [`../tarboat-gap-analysis.md`](../tarboat-gap-analysis.md) |
| Human-interactive in-box install (the sibling doc) | [`../gardenio-in-a-box-install.md`](../gardenio-in-a-box-install.md) |
| Tarball artifact + layout (G1.3) | bead ka-0000000192 |
| Offline-staged assets (`--offline-assets`, G1.1) | bead ka-0000005747, `scripts/bootstrap-ec2.sh` |
| Carried repo tree (G1.2) | bead ka-0000008214 |
| Architecture + AWS-dependency substitutions | [`../gardenio-in-a-box-synthesis.md`](../gardenio-in-a-box-synthesis.md) |

Operator: matthew.baker@acme.example
