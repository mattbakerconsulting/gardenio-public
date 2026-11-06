#!/usr/bin/env bash
# tarboat-box-remediate.sh — make a BYO tarboat box's gardenio deploy succeed.
#
# The public-safe in-box overlay layering (values-inbox.yaml + gardenio-inbox.yaml)
# runs REAL edge-auth (edgeAuth.dryRun=false) and perHost TLS (tls.mode=perHost),
# both of which the chart FAIL-FASTS on unless two prerequisites are seeded first:
#
#   (a) an edge-auth HMAC Secret (edgeAuth.hmacKey.secretName), and
#   (b) a non-empty tls.hosts list + a cert source for it.
#
# The one-shot BYO cloud-init deploy seeds NEITHER, so `deploy-byo.sh` fail-fasts
# at `helm lint` and cloud-init exits with the deploy incomplete. This is exactly
# the two-pass seed flow TARBOAT-SETUP.md §5-6 documents for the tarball path; for
# the standing Phase-B sweep against a BYO box we do it here, unattended:
#
#   1. seed a random edge-auth HMAC Secret (key hmac-key),
#   2. mint a self-signed local-CA leaf cert covering every served sslip.io host
#      and load it as a TLS Secret (no Let's Encrypt / ACME dependency), and
#   3. re-run deploy-byo.sh with a tls overlay (existingSecretName + ClusterIssuer
#      off) plus --set edgeAuth.hmacKey.secretName + the enumerated tls.hosts.
#
# Run ON the box as root (the sweep scp's it from an up-to-date main checkout and
# runs it over SSH). Idempotent: re-running recreates the secrets and re-applies
# the deploy.
#
# Usage:
#   sudo tarboat-box-remediate.sh --namespace gardenio --dashed-ip 1-2-3-4 \
#        --app-version 2026.175.11 --hosts "a.sslip.io b.sslip.io ..." \
#        [--repo-dir /opt/gardenio/repo]
set -uo pipefail

NAMESPACE="gardenio"
DASHED_IP=""
APP_VERSION=""
HOSTS=""
REPO_DIR="/opt/gardenio/repo"
HMAC_SECRET="gardenio-edge-auth-hmac"
TLS_SECRET="gardenio-tarboat-tls"
OPERATORS_NAMESPACE="operators"
KC_ADMIN_SECRET="gardenio-keycloak-runtime-admin"
KO_VERSION="1.32.0"                                        # matches bootstrap-ec2.sh OP_KEYCLOAK_OPERATOR_VERSION
KO_REPO="https://epam.github.io/edp-helm-charts/stable"

while [ "$#" -gt 0 ]; do
  case "$1" in
    --namespace) NAMESPACE="$2"; shift 2;;
    --dashed-ip) DASHED_IP="$2"; shift 2;;
    --app-version) APP_VERSION="$2"; shift 2;;
    --hosts) HOSTS="$2"; shift 2;;
    --repo-dir) REPO_DIR="$2"; shift 2;;
    -h|--help) sed -n '2,32p' "$0" | sed 's/^# \{0,1\}//'; exit 0;;
    *) echo "unknown flag: $1" >&2; exit 2;;
  esac
done

[ -n "$DASHED_IP" ]   || { echo "FATAL: --dashed-ip required" >&2; exit 2; }
[ -n "$HOSTS" ]       || { echo "FATAL: --hosts required (space-separated served hostnames)" >&2; exit 2; }
[ -n "$APP_VERSION" ] || { echo "FATAL: --app-version required" >&2; exit 2; }
[ -x "$REPO_DIR/scripts/deploy-byo.sh" ] || { echo "FATAL: repo not staged at $REPO_DIR" >&2; exit 2; }

export KUBECONFIG=/etc/rancher/k3s/k3s.yaml
APEX="${DASHED_IP}.sslip.io"

echo "== remediate: ns=$NAMESPACE apex=$APEX app-version=$APP_VERSION =="
kubectl create namespace "$NAMESPACE" --dry-run=client -o yaml | kubectl apply -f -

# (a) edge-auth HMAC secret (key hmac-key) — satisfies edgeAuth.hmacKey.secretName.
echo "== seed $HMAC_SECRET =="
kubectl -n "$NAMESPACE" create secret generic "$HMAC_SECRET" \
  --from-literal=hmac-key="$(openssl rand -hex 32)" \
  --dry-run=client -o yaml | kubectl apply -f -

# (b) local-CA leaf cert covering every served host -> a TLS Secret. No ACME/LE.
echo "== mint local-CA TLS cert for: $HOSTS =="
WD="$(mktemp -d)"; trap 'rm -rf "$WD"' EXIT
(
  cd "$WD"
  openssl req -x509 -newkey rsa:4096 -nodes -keyout ca.key -out ca.crt -days 3650 \
    -subj "/CN=gardenio-tarboat-local-ca/O=gardenio" >/dev/null 2>&1
  san=""; for h in $HOSTS; do san="${san}DNS:${h},"; done; san="${san%,}"
  openssl req -newkey rsa:2048 -nodes -keyout tls.key -out tls.csr \
    -subj "/CN=${APEX}" >/dev/null 2>&1
  openssl x509 -req -in tls.csr -CA ca.crt -CAkey ca.key -CAcreateserial -days 825 \
    -extfile <(printf 'subjectAltName=%s' "$san") -out tls.crt >/dev/null 2>&1
  cat tls.crt ca.crt > tls.fullchain.crt
)
kubectl -n "$NAMESPACE" create secret tls "$TLS_SECRET" \
  --cert="$WD/tls.fullchain.crt" --key="$WD/tls.key" \
  --dry-run=client -o yaml | kubectl apply -f -

# (c) keycloak-operator must reconcile the realm/client CRs, or oauth2-proxy
#     CrashLoopBackOffs on OIDC discovery 404 "Realm does not exist"
#     (ka-0000005282). Two on-box bridges the box's cloned tag predates:
#
#   c1. CLUSTER-SCOPE the operator. bootstrap-ec2.sh (from the box's OLD tag)
#       installs epamedp/keycloak-operator with the chart default
#       clusterReconciliationEnabled=false -> WATCH_NAMESPACE=operators -> the
#       operator's cache only watches ns/operators, so the KeycloakRealm /
#       KeycloakClient CRs the gardenio chart creates in ns/$NAMESPACE are NEVER
#       reconciled. helm upgrade the operator to clusterReconciliationEnabled=true
#       (WATCH_NAMESPACE="" + ClusterRole) so it sees CRs in every namespace.
#       Idempotent: on a box whose bootstrap already did this (future release),
#       the upgrade is a no-op. (main's bootstrap-ec2.sh carries this durably.)
#
#   c2. MIRROR the admin Secret into the operator namespace. The EDP operator
#       resolves a ClusterKeycloak's .spec.secret in its OWN namespace
#       (OPERATOR_NAMESPACE=operators), NOT in the CR's namespace. The chart
#       creates $KC_ADMIN_SECRET in ns/$NAMESPACE only (the box's OLD chart has
#       no operator-ns copy), so the operator can't authenticate ->
#       ClusterKeycloak connected:false. We pre-create the admin Secret in BOTH
#       namespaces with the SAME password and stamp the release-namespace copy
#       for Helm adoption, so the re-deploy's chart-managed Secret adopts it (the
#       chart's lookup-based password helper then reuses this value) and the
#       operator authenticates during the helm --wait window. (main's chart
#       carries the operator-ns copy durably via
#       keycloak.runtime.clusterKeycloak.operatorNamespace.)
echo "== c1: cluster-scope the keycloak-operator (clusterReconciliationEnabled=true) =="
kubectl create namespace "$OPERATORS_NAMESPACE" --dry-run=client -o yaml | kubectl apply -f -
ko_chart="epamedp/keycloak-operator"
ko_args=(--version "$KO_VERSION")
staged_ko="/opt/gardenio/tarboat/charts/keycloak-operator-${KO_VERSION}.tgz"   # air-gap default (bootstrap OFFLINE_ASSETS)
if [ -s "$staged_ko" ]; then
  ko_chart="$staged_ko"
  ko_args=()   # --version is for repo charts; the local tgz IS the version
else
  # BYO sweep boxes are internet-connected; fetch the pinned chart from upstream.
  helm repo add epamedp "$KO_REPO" >/dev/null 2>&1 || true
  helm repo update epamedp >/dev/null 2>&1 || helm repo update >/dev/null 2>&1 || true
fi
helm upgrade --install keycloak-operator "$ko_chart" \
  "${ko_args[@]}" \
  --namespace "$OPERATORS_NAMESPACE" \
  --reuse-values \
  --set clusterReconciliationEnabled=true \
  --wait --timeout 5m 2>&1 | tail -5 \
  || echo "  WARN: operator upgrade non-zero (continuing; may already be cluster-scoped)"

echo "== c2: seed matching $KC_ADMIN_SECRET in ns/$NAMESPACE + ns/$OPERATORS_NAMESPACE =="
# Reuse an already-seeded password if one exists in either namespace (idempotent
# re-runs / a prior successful deploy), else mint a fresh one.
KC_ADMIN_PW="$(kubectl -n "$NAMESPACE" get secret "$KC_ADMIN_SECRET" -o jsonpath='{.data.password}' 2>/dev/null | base64 -d 2>/dev/null || true)"
[ -n "$KC_ADMIN_PW" ] || KC_ADMIN_PW="$(kubectl -n "$OPERATORS_NAMESPACE" get secret "$KC_ADMIN_SECRET" -o jsonpath='{.data.password}' 2>/dev/null | base64 -d 2>/dev/null || true)"
[ -n "$KC_ADMIN_PW" ] || KC_ADMIN_PW="$(openssl rand -base64 24 | tr -dc 'A-Za-z0-9' | head -c 32)"
for ns in "$NAMESPACE" "$OPERATORS_NAMESPACE"; do
  kubectl create namespace "$ns" --dry-run=client -o yaml | kubectl apply -f - >/dev/null
  kubectl -n "$ns" create secret generic "$KC_ADMIN_SECRET" \
    --from-literal=username=admin \
    --from-literal=password="$KC_ADMIN_PW" \
    --dry-run=client -o yaml | kubectl apply -f - >/dev/null
  # Stamp BOTH copies for Helm adoption so the re-deploy's chart-managed Secret
  # (resource-policy: keep, no owner metadata on our hand-created one) ADOPTS it
  # instead of aborting with "invalid ownership metadata". The gardenio helm
  # release owns both: the release-ns copy always, and — on a future calver
  # whose chart carries keycloak.runtime.clusterKeycloak.operatorNamespace — the
  # operator-ns copy too. Stamping the operator-ns copy now keeps that later
  # `helm upgrade` conflict-free. The chart's lookup-based password helper reuses
  # this value, so both namespaces stay in sync.
  kubectl -n "$ns" label  secret "$KC_ADMIN_SECRET" app.kubernetes.io/managed-by=Helm --overwrite >/dev/null 2>&1 || true
  kubectl -n "$ns" annotate secret "$KC_ADMIN_SECRET" \
    meta.helm.sh/release-name="$NAMESPACE" \
    meta.helm.sh/release-namespace="$NAMESPACE" --overwrite >/dev/null 2>&1 || true
done

# (d) beads-viewer ServiceAccount. beads-viewer (enabled by the inbox overlay)
#     reuses the agents ServiceAccount ($RELEASE-agents-sa) for its IRSA-or-plain
#     identity, but the box's OLD cloned chart gates that SA on agents.enabled —
#     which is false on BYO — so the beads-viewer ReplicaSet FailedCreate with
#     "serviceaccount \"gardenio-agents-sa\" not found" and the login gate never
#     gets a backend (verdict=no-form). Pre-create the SA (no IRSA annotation:
#     BYO uses plain in-cluster RBAC) and stamp it for Helm adoption so the
#     re-deploy adopts it. (main's chart carries the durable gate widening —
#     serviceaccount.yaml renders the SA when agents.enabled OR beadsViewer.enabled.)
AGENTS_SA="${NAMESPACE}-agents-sa"   # gardenio.agents.serviceAccountName = <release>-agents-sa; release==NAMESPACE on the box
echo "== d: ensure beads-viewer's agents ServiceAccount $AGENTS_SA in ns/$NAMESPACE =="
kubectl -n "$NAMESPACE" create serviceaccount "$AGENTS_SA" --dry-run=client -o yaml | kubectl apply -f - >/dev/null
kubectl -n "$NAMESPACE" label  serviceaccount "$AGENTS_SA" app.kubernetes.io/managed-by=Helm --overwrite >/dev/null 2>&1 || true
kubectl -n "$NAMESPACE" annotate serviceaccount "$AGENTS_SA" \
  meta.helm.sh/release-name="$NAMESPACE" \
  meta.helm.sh/release-namespace="$NAMESPACE" --overwrite >/dev/null 2>&1 || true

# One remediation overlay carrying ALL box-specific overrides as values (not
# --set, so it works with any deploy-byo version — older tags parse
# --extra-values but not --set):
#   - edgeAuth.hmacKey.secretName -> the seeded HMAC Secret,
#   - tls: existingSecretName (local-CA) + ClusterIssuer off + enumerated hosts,
#   - PER-BOX AUTH HOSTS: the base overlay deploy-values/gardenio-inbox.yaml pins
#     the auth host/cookie domain/callback to the CLOUD placeholder
#     auth.gardenio.e2e.example.com (§6/§7.4 — a real per-env value the
#     operator is meant to override at install). On a torn-from-scratch sweep box
#     nothing overrides it, so oauth2-proxy does OIDC discovery against that cloud
#     host — which public DNS resolves to a FOREIGN endpoint that 404s the
#     discovery URL, and oauth2-proxy has no discovery retry -> immediate exit ->
#     steady CrashLoopBackOff, blocking helm --wait and the login gate
#     (ka-0000000068). Repoint every auth host at this box's own sslip.io apex.
#     Because discovery then hairpins through traefik serving the local-CA leaf
#     cert minted above (NOT publicly trusted), oauth2-proxy must skip TLS verify
#     on the discovery/token calls — add --ssl-insecure-skip-verify as an
#     extraArg. This is the box-sweep posture only (the operator TARBOAT-SETUP
#     path imports the local CA into client trust instead); it never reaches a
#     cloud deploy because it lives in this box-remediation overlay.
OVERLAY=/tmp/tarboat-remediation-overlay.yaml
{
  echo "edgeAuth:"
  echo "  hmacKey:"
  echo "    secretName: \"${HMAC_SECRET}\""
  echo "    secretKey: \"hmac-key\""
  echo "tls:"
  echo "  enabled: true"
  echo "  mode: \"perHost\""
  echo "  existingSecretName: \"${TLS_SECRET}\""
  echo "  clusterIssuer:"
  echo "    create: false"
  echo "  hosts:"
  for h in $HOSTS; do echo "    - \"${h}\""; done
  # DISABLE the CrowdSec bouncer on the sweep box (ka-0000003256). The base inbox
  # overlay sets edge.crowdsec.enabled=true, so gardenio.edgeMiddlewares attaches
  # the <release>-edge-crowdsec Middleware to every gated edge route — the
  # beads-viewer forwardauth route (Host beads-viewer.<apex>) and the apex
  # /oauth2/* route. That Middleware references a yaegi PLUGIN that must be
  # registered in the k3s-bundled Traefik static config AND, once registered,
  # must reach the LAPI Service cross-namespace from kube-system. On a from-
  # scratch sweep box NEITHER holds: the plugin is unregistered (Traefik drops
  # the whole route -> beads-viewer.<apex>/ 404, verdict=no-form), and even after
  # registering it the bouncer 500s because crowdsecLapiHost is a bare
  # gardenio-edge-crowdsec:8080 (not an FQDN, and no NetworkPolicy admits
  # kube-system->LAPI:8080 under the box's default-deny). CrowdSec is fail-open
  # WAF-lite that adds ZERO value to the login/acceptance/beads gates this
  # ephemeral single-node CI box validates, and it is the sole thing breaking
  # every gated route. Turn it OFF here so the edge chain is
  # buffering->inflightreq->forwardauth->Keycloak with no plugin/LAPI dependency.
  # SCOPE: this remediation overlay is applied LAST (--extra-values) and is used
  # ONLY by the sweep box + the unattended BYO path — it never reaches a cloud
  # deploy (prod uses its own deploy-values overlay, which sets its own edge posture).
  # The base inbox overlay and its crowdsec chart tests are untouched, so the
  # documented operator crowdsec-on posture (plugin registered per install-doc
  # §A.6) is unchanged for real operator boxes.
  echo "edge:"
  echo "  crowdsec:"
  echo "    enabled: false"
  echo "keycloak:"
  echo "  runtime:"
  # KC_HOSTNAME + the IngressRoute Host() match + the auto-derived OIDC issuer.
  echo "    host: \"auth.${APEX}\""
  echo "  forwardAuth:"
  # Front-door SSO cookie scoped to this box's apex (a parent of every served
  # subdomain) and the oauth2 callback host = the apex (the forwardAuth ingress).
  echo "    cookieDomain: \".${APEX}\""
  echo "    ingress:"
  echo "      host: \"${APEX}\""
  echo "    extraArgs:"
  echo "      - \"--cookie-csrf-per-request-limit=4\""
  # Local-CA discovery/token hairpin through traefik — skip TLS verify (box only).
  echo "      - \"--ssl-insecure-skip-verify=true\""
  # Per-box gated beads-viewer host — the login gate (validate.sh) follows
  # https://beads-viewer.<apex>/, so the viewer route MUST render on that host or
  # the gate 404s (verdict=no-form). enabled=true comes from the base inbox
  # overlay; the host is only known at install so it is injected here.
  echo "beadsViewer:"
  echo "  ingress:"
  echo "    host: \"beads-viewer.${APEX}\""
} > "$OVERLAY"
echo "== remediation overlay =="; cat "$OVERLAY"

echo "== re-run deploy-byo (edge-auth HMAC + local-CA TLS + tls.hosts overlay) =="
cd "$REPO_DIR"
bash scripts/deploy-byo.sh \
  --namespace "$NAMESPACE" \
  --values deploy-values/gardenio-byo.yaml \
  --extra-values gardenio/helm/gardenio/values-inbox.yaml \
  --extra-values deploy-values/gardenio-inbox.yaml \
  --extra-values "$OVERLAY" \
  --app-version "$APP_VERSION" \
  --no-smoke
rc=$?
echo "deploy-byo re-run rc=$rc"

echo "== pods after re-deploy =="
kubectl -n "$NAMESPACE" get pods -o wide 2>&1 | head -50 || true

# ── Forwardauth crashloop diagnostics (ka-0000005282) ────────────────────────
# The oauth2-proxy crashloop persisted across the .22/.23 sweeps WITH the
# per-box auth hosts applied above, and the box is always torn down before
# anyone can read pod logs (agent-side kubectl is RBAC-denied; only this
# on-box root shell can see them). Capture the discriminating evidence into
# the sweep log: pod logs + live args + secret sanity + the OIDC discovery
# URL probed from every relevant vantage point — host via public DNS (the
# IGW-hairpin path a pod effectively takes), host via loopback traefik
# (Host-routing minus hairpin), host via the keycloak ClusterIP (realm
# existence minus the edge), and an in-cluster probe pod that carries the
# forwardauth labels so the exact same NetworkPolicies select it. Plus the
# KeycloakRealm/Client CR status + operator logs (CRD-skew hypothesis).
# Best-effort throughout: never alters the deploy exit code.
echo "== DIAG: forwardauth pod state + logs =="
kubectl -n "$NAMESPACE" describe pods -l app.kubernetes.io/name=gardenio-forwardauth 2>&1 \
  | sed -n '/^Containers:/,/^Conditions:/p' | head -50 || true
kubectl -n "$NAMESPACE" describe pods -l app.kubernetes.io/name=gardenio-forwardauth 2>&1 \
  | sed -n '/^Events:/,$p' | head -25 || true
echo "-- current logs --"
kubectl -n "$NAMESPACE" logs deploy/gardenio-forwardauth --tail=80 2>&1 || true
echo "-- previous-container logs --"
kubectl -n "$NAMESPACE" logs deploy/gardenio-forwardauth --previous --tail=80 2>&1 || true

echo "== DIAG: live oauth2-proxy args =="
kubectl -n "$NAMESPACE" get deploy gardenio-forwardauth \
  -o jsonpath='{range .spec.template.spec.containers[0].args[*]}{@}{"\n"}{end}' 2>&1 || true

ISSUER="$(kubectl -n "$NAMESPACE" get deploy gardenio-forwardauth \
  -o jsonpath='{range .spec.template.spec.containers[0].args[*]}{@}{"\n"}{end}' 2>/dev/null \
  | sed -n 's/^--oidc-issuer-url=//p' | head -1)"
DISCOVERY="${ISSUER%/}/.well-known/openid-configuration"
ISSUER_HOST="$(printf '%s' "$ISSUER" | sed -E 's#https?://##; s#/.*##')"
echo "DIAG issuer=$ISSUER"

echo "== DIAG: forwardauth secret sanity (lengths only) =="
for k in client-secret cookie-secret; do
  n="$(kubectl -n "$NAMESPACE" get secret gardenio-forwardauth-secrets \
        -o jsonpath="{.data.${k}}" 2>/dev/null | base64 -d 2>/dev/null | wc -c)" || n='?'
  echo "  gardenio-forwardauth-secrets/${k}: ${n} bytes"
done

if [ -n "$ISSUER" ]; then
  echo "== DIAG: discovery from host vantage points =="
  echo "-- (a) public DNS + IGW hairpin (the pod's effective path) --"
  curl -m 15 -sk -o /tmp/diag-disc-a.json -w 'code=%{http_code} time=%{time_total}s\n' \
    "$DISCOVERY" || echo "curl rc=$? (conn-fail/timeout)"
  head -c 250 /tmp/diag-disc-a.json 2>/dev/null; echo
  echo "-- (b) loopback traefik (Host-routed, no hairpin) --"
  curl -m 15 -sk --resolve "${ISSUER_HOST}:443:127.0.0.1" \
    -o /tmp/diag-disc-b.json -w 'code=%{http_code} time=%{time_total}s\n' \
    "$DISCOVERY" || echo "curl rc=$?"
  head -c 250 /tmp/diag-disc-b.json 2>/dev/null; echo
  echo "-- (c) keycloak-runtime ClusterIP (realm existence, no edge) --"
  KC_SVC_IP="$(kubectl -n "$NAMESPACE" get svc \
    -l app.kubernetes.io/component=keycloak-runtime \
    -o jsonpath='{.items[0].spec.clusterIP}' 2>/dev/null)" || KC_SVC_IP=""
  KC_PATH="$(printf '%s' "$DISCOVERY" | sed -E 's#https?://[^/]+##')"
  echo "kc_svc_ip=${KC_SVC_IP:-none} path=$KC_PATH"
  if [ -n "$KC_SVC_IP" ]; then
    curl -m 15 -s -o /tmp/diag-disc-c.json \
      -w 'code=%{http_code} time=%{time_total}s\n' \
      "http://${KC_SVC_IP}:8080${KC_PATH}" || echo "curl rc=$?"
    head -c 250 /tmp/diag-disc-c.json 2>/dev/null; echo
  fi

  echo "== DIAG: in-cluster probe with forwardauth labels (same netpols) =="
  kubectl -n "$NAMESPACE" delete pod fa-oidc-probe --ignore-not-found --now >/dev/null 2>&1 || true
  PROBE=/tmp/fa-oidc-probe.yaml
  cat > "$PROBE" <<EOF
apiVersion: v1
kind: Pod
metadata:
  name: fa-oidc-probe
  namespace: ${NAMESPACE}
  labels:
    app.kubernetes.io/name: gardenio-forwardauth
    app.kubernetes.io/instance: gardenio
spec:
  restartPolicy: Never
  securityContext:
    runAsNonRoot: true
    runAsUser: 65534
    seccompProfile: { type: RuntimeDefault }
  containers:
    - name: probe
      image: curlimages/curl:8.8.0
      command: ["sh", "-c"]
      args:
        - |
          echo '--- dns (cluster resolver) ---'
          nslookup ${ISSUER_HOST} 2>&1 || true
          echo '--- discovery (pod vantage, verbose tail) ---'
          curl -m 20 -skv -o /tmp/d.json -w 'code=%{http_code} time=%{time_total}s\n' '${DISCOVERY}' 2>&1 | tail -30
          head -c 200 /tmp/d.json 2>/dev/null; echo
      securityContext:
        allowPrivilegeEscalation: false
        capabilities: { drop: [ALL] }
EOF
  kubectl apply -f "$PROBE" 2>&1 || true
  kubectl -n "$NAMESPACE" wait --for=jsonpath='{.status.phase}'=Succeeded \
    pod/fa-oidc-probe --timeout=120s 2>&1 || true
  kubectl -n "$NAMESPACE" logs fa-oidc-probe 2>&1 || true
  kubectl -n "$NAMESPACE" delete pod fa-oidc-probe --ignore-not-found --now >/dev/null 2>&1 || true
fi

echo "== DIAG: keycloak realm/client CRs + operator =="
kubectl -n "$NAMESPACE" get keycloakrealms,keycloakclients,clusterkeycloaks 2>&1 || true
kubectl -n "$NAMESPACE" get keycloakrealms,keycloakclients -o yaml 2>/dev/null \
  | grep -E '^\s+(name:|available:|failureCount:|value:|reason:)' | head -30 || true
echo "-- operator (ns operators) --"
helm list -n operators 2>&1 || true
kubectl -n operators get pods 2>&1 || true
kubectl -n operators logs deploy/keycloak-operator --tail=50 2>&1 | tail -50 \
  || kubectl -n operators logs -l app.kubernetes.io/name=keycloak-operator --tail=50 2>&1 | tail -50 || true
echo "-- realm CRD schema: defaultGroups occurrences (0 = stale CRD) --"
kubectl get crd keycloakrealms.v1.edp.epam.com -o yaml 2>/dev/null | grep -c defaultGroups || true

echo "== DIAG: keycloak-runtime logs (realm import) =="
kubectl -n "$NAMESPACE" logs -l app.kubernetes.io/component=keycloak-runtime --tail=50 2>&1 | tail -50 || true

# ── beads-viewer crashloop diagnostics (ka-0000006204) ───────────────────────
# The .32 sweep found beads-viewer 0/1 CrashLoopBackOff (image pulls fine, then
# the container Started-then-exits repeatedly), which is the SOLE cause of the
# acceptance/login/beads_data gate reds — but the DIAG block only ever dumped
# forwardauth + keycloak, so the crash reason was never captured before the box
# was torn down (agent-side kubectl is RBAC-denied; only this on-box root shell
# can read pod logs). Mirror the forwardauth capture: pod state + Events + the
# CURRENT and --previous container logs (the previous-container logs hold the
# fatal line from the run the kubelet already killed) + the live OIDC/AVA env so
# a config crash-on-start (e.g. an empty OIDC_CLIENT_ID with AVA off, the
# ka-0000006204 root cause -> parseConfig os.Exit(1)) is self-evident in the
# sweep log. Best-effort throughout: never alters the deploy exit code.
echo "== DIAG: beads-viewer pod state + logs =="
kubectl -n "$NAMESPACE" describe pods -l app.kubernetes.io/component=beads-viewer 2>&1 \
  | sed -n '/^Containers:/,/^Conditions:/p' | head -50 || true
kubectl -n "$NAMESPACE" describe pods -l app.kubernetes.io/component=beads-viewer 2>&1 \
  | sed -n '/^Events:/,$p' | head -25 || true
echo "-- current logs --"
kubectl -n "$NAMESPACE" logs deploy/gardenio-beads-viewer --tail=80 2>&1 || true
echo "-- previous-container logs (holds the fatal crash-on-start line) --"
kubectl -n "$NAMESPACE" logs deploy/gardenio-beads-viewer --previous --tail=80 2>&1 || true
echo "== DIAG: beads-viewer live auth env (crash-on-start config) =="
kubectl -n "$NAMESPACE" get deploy gardenio-beads-viewer \
  -o jsonpath='{range .spec.template.spec.containers[0].env[?(@.name=="OIDC_ISSUER_URL")]}OIDC_ISSUER_URL={@.value}{"\n"}{end}{range .spec.template.spec.containers[0].env[?(@.name=="OIDC_CLIENT_ID")]}OIDC_CLIENT_ID={@.value}{"\n"}{end}{range .spec.template.spec.containers[0].env[?(@.name=="AVA_TRUST_SIGNER")]}AVA_TRUST_SIGNER={@.value}{"\n"}{end}{range .spec.template.spec.containers[0].env[?(@.name=="BEADS_VIEWER_DEV_AUTH_BYPASS")]}BEADS_VIEWER_DEV_AUTH_BYPASS={@.value}{"\n"}{end}' 2>&1 || true

echo "== DIAG: recent namespace events =="
kubectl -n "$NAMESPACE" get events --sort-by=.lastTimestamp 2>&1 | tail -25 || true
echo "== DIAG: done =="

exit "$rc"
