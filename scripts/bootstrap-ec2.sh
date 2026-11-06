#!/usr/bin/env bash
# bootstrap-ec2.sh — Gardenio in a Box: scripted single-EC2 bootstrap.
#
# Brings a *fresh* EC2 instance up running k3s (embedded containerd) with the
# gardenio Helm chart installed via the BYO deploy path. This is Phase 0 of the
# "Gardenio in a Box" build (epic ka-0000007690; synthesis
# gardenio/docs/gardenio-in-a-box-synthesis.md §4). Source research:
# gardenio/docs/research/inbox-ec2-sizing-bootstrap.md §5 and
# gardenio/docs/research/inbox-k8s-distro-choice.md "install sketch".
#
# WHERE THIS RUNS: ON the EC2 instance, as root — either invoked from the
# #cloud-config user-data template (scripts/cloud-config/gardenio-inbox-user-data.yaml.tmpl)
# or run by hand after SSH'ing in. It does NOT call any AWS API; it assumes the
# instance already exists (the operator launches it with the scenario's
# instance type + gp3 root size — see the scenario table below).
#
# STAGES (each is idempotent / re-runnable):
#   1. OS prep            — swapoff, kernel modules + sysctls, base packages.
#   2. k3s install        — single-node k3s with embedded containerd.
#                           (P0.2 ka-0000002781 hardens this with a pinned,
#                            durable /etc/rancher/k3s/config.yaml.)
#   3. gVisor (OPTIONAL)  — runsc RuntimeClass-only sandbox runtime.
#                           (P0.3 ka-0000002716 wires the containerd template +
#                            AdmissionConfiguration. Default OFF here.)
#   4. image import       — load any air-gapped image tarballs into containerd.
#   4b. air-gap (OPTIONAL) — stand up the in-box zot registry (P4.1), seed it
#                           from the crane OCI-layout bundle, and redirect every
#                           containerd pull to it (P4.2). Default OFF; --airgap
#                           enables it so the box pulls ZERO external images.
#                           --airgap-import-only is the registry-FREE variant:
#                           it SKIPS this stage entirely and relies on stage 4
#                           (ctr-import + k3s auto-load dir) as the whole image
#                           story. See "AIR-GAP MODES" below.
#   4c. operators         — install the prerequisite operators/CRDs the in-box
#                           overlays' rendered CRs require (cert-manager,
#                           prometheus-operator ServiceMonitor CRD, EDP
#                           keycloak-operator) BEFORE the helm deploy, so the
#                           chart's Certificate/ClusterIssuer/ServiceMonitor/
#                           ClusterKeycloak/Keycloak* CRs have their CRDs +
#                           controllers in place. Default AUTO: installed when an
#                           in-box overlay is layered (which turns keycloak.runtime
#                           / tls / serviceMonitor ON); force on/off with
#                           --with-operators / --no-operators. Pinned versions
#                           live in the OP_* constants below.
#   5. deploy             — delegate secret-seed + helm install + smoke to the
#                           repo's existing scripts/deploy-byo.sh.
#
# AIR-GAP MODES (mutually exclusive):
#   --airgap             (zot mode, the air-gap DEFAULT) — in-box zot registry +
#                        containerd mirror. agentPullPolicy IfNotPresent; a pull
#                        miss falls back to the loopback registry. Survives pod
#                        reschedule / in-place upgrade (the registry re-serves).
#   --airgap-import-only (registry-FREE mode, OPT-IN; epic ka-0000002730) — NO
#                        zot, NO mirror. Every image is pre-loaded into k3s's
#                        embedded containerd via stage 4 (ctr-import from
#                        IMAGE_DIR + the k3s auto-load dir
#                        /var/lib/rancher/k3s/agent/images/ for reboot survival).
#                        Forces agents.agentPullPolicy=Never via the
#                        deploy-values/gardenio-inbox-import-only.yaml overlay so
#                        the kubelet never round-trips a (nonexistent) registry.
#                        CAVEAT: a containerd GC under DiskPressure or a pod
#                        reschedule onto an un-seeded state requires a manual
#                        re-import. NOT recommended for boxes that reschedule
#                        pods or upgrade in place — use --airgap (zot) there.
#
# Usage:
#   sudo scripts/bootstrap-ec2.sh [--scenario A|B|C] [--namespace NS]
#        [--release NAME] [--values FILE] [--extra-values FILE]... [--repo-dir DIR]
#        [--app-version TAG]
#        [--gvisor] [--no-gvisor] [--harden] [--no-harden] [--operator-user USER]
#        [--airgap] [--airgap-strict] [--airgap-import-only] [--bundle-dir DIR]
#        [--offline-assets DIR]
#        [--with-operators] [--no-operators] [--operators-namespace NS]
#        [--no-smoke] [--print-scenarios]
#
# --offline-assets DIR (Tarboat T1/G1.1, default /opt/gardenio/tarboat): a
#   directory of staged install-time assets (bin/{k3s,helm,crane,zot},
#   manifests/{cert-manager,servicemonitor-crd}.yaml,
#   charts/keycloak-operator-<ver>.tgz). Each fetch site prefers the staged copy
#   and only downloads when absent — UNLESS --airgap-strict, where a missing
#   staged asset is a HARD FAIL. This is what makes a true (internet-free)
#   air-gap install possible: combine with --airgap-strict and a staged image
#   bundle for ZERO public-internet fetches.
#
# PUBLIC-SAFE one-shot (the Phase 5 capstone profile — see
# gardenio/docs/gardenio-in-a-box-install.md):
#   sudo scripts/bootstrap-ec2.sh --scenario A --harden --airgap \
#        --extra-values gardenio/helm/gardenio/values-inbox.yaml \
#        --extra-values deploy-values/gardenio-inbox.yaml
#
# Environment overrides (flags win over env):
#   SCENARIO       sizing scenario A|B|C            (default: A)
#   NAMESPACE      target namespace                 (default: gardenio)
#   RELEASE        helm release name                (default: gardenio)
#   VALUES_FILE    base chart values overlay        (default: deploy-values/gardenio-byo.yaml)
#   EXTRA_VALUES   space-separated overlays layered ON TOP of VALUES_FILE, in
#                  order (also via repeatable --extra-values FILE). The
#                  public-safe profile is VALUES_FILE + the two in-box overlays.
#   REPO_DIR       checked-out gardenio repo root   (default: auto-detected)
#   K3S_VERSION    pin INSTALL_K3S_VERSION          (default: "" → P0.2 pin / k3s stable)
#   TLS_SANS       extra STABLE apiserver cert SANs (EIP / DNS) so the cert
#                  survives an EC2 stop/start IP change (default: none)
#   BIND_ADDRESS   apiserver bind address           (default: 0.0.0.0)
#   GVISOR         1=install runsc sandbox runtime  (default: 0)
#   IMAGE_DIR      dir of air-gapped *.tar images   (default: /opt/gardenio/images)
#   AIRGAP         1=in-box zot registry + mirror   (default: 0; Phase 4)
#   AIRGAP_STRICT  1=--disable-default-registry-endpoint (no upstream fallback)
#   AIRGAP_IMPORT_ONLY 1=registry-FREE ctr-import mode (skip zot; agentPullPolicy
#                  Never). Also AIRGAP_MODE=import. Mutually exclusive with AIRGAP.
#   BUNDLE_DIR     dir holding gardenio-bundle-*.oci.tar.zst (default: /opt/gardenio/bundle)
#   WITH_OPERATORS 1=install / 0=skip prerequisite operators+CRDs; unset=AUTO
#                  (install iff an in-box overlay is layered). Default: auto.
#   OPERATORS_NAMESPACE  ns for the keycloak-operator helm install (default: operators)
#   RUN_SMOKE      1=run smoke after deploy         (default: 1)
#
# NOTE on the values overlay: the default (deploy-values/gardenio-byo.yaml)
# deliberately runs keycloak / forwardauth / TLS OFF and exposes ClusterIP only
# — it satisfies Phase 0's goal ("it installs and runs") but is NOT yet
# public-internet-safe. The PUBLIC-SAFE profile keeps that base and layers the
# two in-box overlays on top via --extra-values (helm precedence: later wins),
# AND enables --harden (host firewall + SSH + IMDSv2):
#   --extra-values gardenio/helm/gardenio/values-inbox.yaml  (storage/secrets/air-gap)
#   --extra-values deploy-values/gardenio-inbox.yaml        (edge hardening + Keycloak)
# Canonical layering order + per-file scope: gardenio/docs/inbox-overlay-layering.md.
# Full one-shot runbook: gardenio/docs/gardenio-in-a-box-install.md.
set -euo pipefail

# cloud-init swallows stdout/stderr silently — be loud and traceable. When run
# under user-data, everything lands in /var/log/cloud-init-output.log.
log()  { printf '\033[1;36m[bootstrap]\033[0m %s\n' "$*"; }
warn() { printf '\033[1;33m[bootstrap] WARN:\033[0m %s\n' "$*" >&2; }
die()  { printf '\033[1;31m[bootstrap] ERROR:\033[0m %s\n' "$*" >&2; exit 1; }

# ── Scenario sizing table (inbox-ec2-sizing-bootstrap.md §2) ─────────────────
# These are the shapes the OPERATOR launches the instance with; the script only
# records them for provenance + sanity-checks the running box against them.
#   SCENARIO  AGENTS  INSTANCE       vCPU/RAM      GP3_ROOT
scenario_row() {
  case "$1" in
    A) echo "1-2   r7i.2xlarge  8/64GiB   150" ;;
    B) echo "4-6   r7i.4xlarge  16/128GiB 250" ;;
    C) echo "8-12  r7i.8xlarge  32/256GiB 400" ;;
    *) return 1 ;;
  esac
}

print_scenarios() {
  cat <<'EOF'
Gardenio in a Box — EC2 sizing scenarios (inbox-ec2-sizing-bootstrap.md §2)
RAM-bound: every concurrent agent reserves a hard 16 GiB. Use memory-optimized r7i.

  SCENARIO  AGENTS  INSTANCE       vCPU / RAM    GP3 ROOT (GiB)
  A         1-2     r7i.2xlarge    8  / 64 GiB   150
  B         4-6     r7i.4xlarge    16 / 128 GiB  250   (4-5 steady, 6 burst)
  C         8-12    r7i.8xlarge    32 / 256 GiB  400

Launch the instance, then run this script ON it. Example launch:
  aws ec2 run-instances --instance-type r7i.2xlarge \
    --block-device-mappings 'DeviceName=/dev/xvda,Ebs={VolumeSize=150,VolumeType=gp3}' \
    --user-data file://<rendered-user-data.yaml>
EOF
}

# ── Args / env ───────────────────────────────────────────────────────────────
SCENARIO="${SCENARIO:-A}"
NAMESPACE="${NAMESPACE:-gardenio}"
RELEASE="${RELEASE:-gardenio}"
VALUES_FILE="${VALUES_FILE:-deploy-values/gardenio-byo.yaml}"
# Additional chart values overlays layered ON TOP of --values, in order (helm
# precedence: later wins). Repeatable via --extra-values; passed straight through
# to deploy-byo.sh. This is how the PUBLIC-SAFE in-box profile is assembled in
# one shot: the canonical layering is the base profile (--values) then the two
# in-box overlays (gardenio/helm/gardenio/values-inbox.yaml = storage/secrets/air-gap,
# then deploy-values/gardenio-inbox.yaml = edge hardening + in-box Keycloak). See
# gardenio/docs/inbox-overlay-layering.md and gardenio/docs/gardenio-in-a-box-install.md.
# Space-separated in the EXTRA_VALUES env; repeatable as --extra-values FILE flags.
EXTRA_VALUES_RAW="${EXTRA_VALUES:-}"
EXTRA_VALUES=()
[ -n "$EXTRA_VALUES_RAW" ] && read -r -a EXTRA_VALUES <<<"$EXTRA_VALUES_RAW"
REPO_DIR="${REPO_DIR:-}"
# Pin the image tag (Chart.appVersion) the chart renders to a KNOWN-PUBLISHED
# calver. The chart's appVersion can outpace the newest image actually promoted
# to the registry (release bumps appVersion before the images for that tag are
# built/promoted), so a fresh box that defaults the tag to appVersion ends up
# ErrImagePull/NotFound on every gardenio image. Pin to the latest promoted
# non-rc calver that passed E2E (see gardenio/docs/gardenio-in-a-box-install.md
# "Pinning a known-good image tag"). Empty = chart default (appVersion). Passed
# straight through to deploy-byo.sh --app-version. (ka-0000001967)
APP_VERSION="${APP_VERSION:-}"
# Per-box Slack channel id (C0…). When set (alongside layering the
# deploy-values/gardenio-inbox-slack.yaml overlay via --extra-values), it is
# passed through to deploy-byo.sh --slack-channel so the box's channel comes
# from the bootstrap inputs, not a hand-edited overlay placeholder. Empty =
# leave the overlay's own value. (ka-0000005575)
SLACK_CHANNEL="${SLACK_CHANNEL:-}"
K3S_VERSION="${K3S_VERSION:-}"
TLS_SANS="${TLS_SANS:-}"
BIND_ADDRESS="${BIND_ADDRESS:-0.0.0.0}"
GVISOR="${GVISOR:-0}"
IMAGE_DIR="${IMAGE_DIR:-/opt/gardenio/images}"
RUN_SMOKE="${RUN_SMOKE:-1}"
# ── Air-gap image distribution (Phase 4, OFF by default) ─────────────────────
# AIRGAP=1 stands up the in-box zot registry (P4.1), seeds it from the crane
# OCI-layout bundle, and redirects every containerd pull to it (P4.2) BEFORE
# deploy — so the box pulls zero images from any external registry at runtime.
# Default OFF: the BYO/cloud path keeps pulling from the GitLab registry.
AIRGAP="${AIRGAP:-0}"
BUNDLE_DIR="${BUNDLE_DIR:-/opt/gardenio/bundle}"
AIRGAP_STRICT="${AIRGAP_STRICT:-0}"
# ── Registry-FREE import-only mode (epic ka-0000002730, OFF by default) ───────
# AIRGAP_IMPORT_ONLY=1 (or AIRGAP_MODE=import, or --airgap-import-only) opts the
# box into a registry-FREE air-gap: skip airgap_setup() (no zot, no mirror) and
# rely entirely on import_images() (stage 4) + the k3s auto-load dir for image
# delivery, with agents.agentPullPolicy forced to Never via the import-only
# deploy overlay. Mutually exclusive with AIRGAP (zot mode).
AIRGAP_IMPORT_ONLY="${AIRGAP_IMPORT_ONLY:-0}"
[ "${AIRGAP_MODE:-}" = "import" ] && AIRGAP_IMPORT_ONLY=1
# ── Box-config seed (M3, ka-0000003598, OFF by default) ──────────────────────
# SEED_CONFIG=1 restores the pre-baked, already-redacted box-config seed
# (gardenio-seed-<calver>.jsonl under $SEED_DIR) into the box's fresh beads DB
# AFTER deploy, so a friend's first agent already has its minimal, honest advice.
# Default OFF because it needs `kd` pointed at the in-box daemon (usually via a
# port-forward), which a fully hands-off bootstrap can't assume; the documented
# post-install path (gardenio/docs/box-config-seeding.md) runs it explicitly.
# Under AIRGAP_STRICT=1 a SEED_CONFIG=1 run hard-fails on a missing/corrupt seed.
SEED_CONFIG="${SEED_CONFIG:-0}"
SEED_DIR="${SEED_DIR:-/opt/gardenio/seed}"
# ── Prerequisite operators / CRDs (4c, ka-0000000953) ────────────────────────
# The two in-box overlays (gardenio/helm/gardenio/values-inbox.yaml +
# deploy-values/gardenio-inbox.yaml) turn keycloak.runtime + keycloak.forwardAuth
# + tls(perHost LE) + ServiceMonitors ON. The chart therefore renders CRs whose
# CRDs+controllers are NOT part of stock k3s and are NOT installed by the deploy
# path: cert-manager.io/v1 Certificate+ClusterIssuer, monitoring.coreos.com/v1
# ServiceMonitor, and v1.edp.epam.com/v1 ClusterKeycloak/KeycloakRealm/
# KeycloakClient/… So a hands-off `helm upgrade` aborts at apply with
#   no matches for kind "ClusterKeycloak"/"ServiceMonitor"/"Certificate" …
# WITH_OPERATORS controls the 4c stage:
#   1     → always install (force, even on a bare base profile)
#   0     → never install (operator pre-installed them, or knows the overlay
#           does not need them)
#   unset → AUTO: install iff an in-box overlay is layered via --extra-values
#           (that is what flips runtime/tls/serviceMonitor on).
# Versions are PINNED — confirmed against the live P5.2 acceptance run.
WITH_OPERATORS="${WITH_OPERATORS:-}"            # "", 0, or 1
OPERATORS_NAMESPACE="${OPERATORS_NAMESPACE:-operators}"
OP_CERT_MANAGER_VERSION="v1.16.2"               # cert-manager full manifest
OP_SERVICEMONITOR_CRD_VERSION="v0.76.0"         # prometheus-operator ServiceMonitor CRD
OP_KEYCLOAK_OPERATOR_VERSION="1.32.0"           # epamedp/keycloak-operator helm chart
OP_KEYCLOAK_OPERATOR_REPO="https://epam.github.io/edp-helm-charts/stable"
# The two in-box overlays whose presence turns the operator-requiring CRs on.
# Matched by basename so an absolute or repo-relative path both trigger AUTO.
INBOX_OVERLAY_BASENAMES=("values-inbox.yaml" "gardenio-inbox.yaml")
# ── Host-layer hardening (Phase 3 P3.5, OFF by default) ──────────────────────
# HARDEN=1 applies the public-internet host firewall (nftables default-drop to
# 22/80/443), SSH key-only + fail2ban (nft backend), and IMDSv2 hop-limit=1 via
# scripts/k3s/install-host-hardening.sh AFTER k3s is up. Default OFF so a
# private/dev BYO box is unchanged; the public-box cloud-config enables it.
# OPERATOR_USER is the single SSH account the drop-in allows (must already
# exist with an authorized_keys injected at launch — never bake a shared key).
HARDEN="${HARDEN:-0}"
OPERATOR_USER="${OPERATOR_USER:-gardenio-op}"

# ── Offline assets (Tarboat T1 / G1.1, ka-0000005747) ────────────────────────
# A true air-gap install must fetch ZERO binaries/manifests/charts from the
# public internet. The tarboat tarball stages every install-time external asset
# under OFFLINE_ASSETS (default /opt/gardenio/tarboat) in this layout:
#   bin/{k3s,helm,crane,zot}                 — pinned, executable
#   manifests/cert-manager.yaml              — OP_CERT_MANAGER_VERSION
#   manifests/servicemonitor-crd.yaml        — OP_SERVICEMONITOR_CRD_VERSION
#   charts/keycloak-operator-<ver>.tgz       — OP_KEYCLOAK_OPERATOR_VERSION (helm pull)
# (k3s system images are staged under IMAGE_DIR / the k3s auto-load dir as today.)
# Each fetch site prefers the staged copy and falls back to the network ONLY
# when AIRGAP_STRICT=0. Under --airgap-strict a missing staged asset is a HARD
# FAIL (never a silent internet fallback) — that is what makes "true air-gap" true.
OFFLINE_ASSETS="${OFFLINE_ASSETS:-/opt/gardenio/tarboat}"

# offline_asset <relative-path> — echo the absolute staged path iff it exists
# and is non-empty, else return non-zero. Callers test the result.
offline_asset() {
  local p="${OFFLINE_ASSETS%/}/$1"
  [ -n "$OFFLINE_ASSETS" ] || return 1
  [ -s "$p" ] || return 1
  printf '%s\n' "$p"
}

# offline_required <human-name> — under --airgap-strict, abort when a staged
# asset is missing instead of falling back to a network fetch.
offline_required() {
  if [ "$AIRGAP_STRICT" = "1" ]; then
    die "--airgap-strict: required offline asset missing ($1). Stage it under $OFFLINE_ASSETS, or drop --airgap-strict to allow a network fetch."
  fi
}

while [ $# -gt 0 ]; do
  case "$1" in
    --scenario) SCENARIO="$2"; shift 2;;
    --namespace) NAMESPACE="$2"; shift 2;;
    --release) RELEASE="$2"; shift 2;;
    --values) VALUES_FILE="$2"; shift 2;;
    --extra-values) EXTRA_VALUES+=("$2"); shift 2;;
    --repo-dir) REPO_DIR="$2"; shift 2;;
    --app-version) APP_VERSION="$2"; shift 2;;
    --slack-channel) SLACK_CHANNEL="$2"; shift 2;;
    --k3s-version) K3S_VERSION="$2"; shift 2;;
    --tls-san) TLS_SANS="$TLS_SANS $2"; shift 2;;
    --bind-address) BIND_ADDRESS="$2"; shift 2;;
    --image-dir) IMAGE_DIR="$2"; shift 2;;
    --offline-assets) OFFLINE_ASSETS="$2"; shift 2;;
    --gvisor) GVISOR=1; shift;;
    --no-gvisor) GVISOR=0; shift;;
    --airgap) AIRGAP=1; shift;;
    --no-airgap) AIRGAP=0; shift;;
    --airgap-strict) AIRGAP=1; AIRGAP_STRICT=1; shift;;
    --airgap-import-only) AIRGAP_IMPORT_ONLY=1; shift;;
    --bundle-dir) BUNDLE_DIR="$2"; shift 2;;
    --with-operators) WITH_OPERATORS=1; shift;;
    --no-operators) WITH_OPERATORS=0; shift;;
    --operators-namespace) OPERATORS_NAMESPACE="$2"; shift 2;;
    --harden) HARDEN=1; shift;;
    --no-harden) HARDEN=0; shift;;
    --operator-user) OPERATOR_USER="$2"; shift 2;;
    --no-smoke) RUN_SMOKE=0; shift;;
    --print-scenarios) print_scenarios; exit 0;;
    -h|--help) sed -n '2,124p' "$0"; exit 0;;
    *) die "unknown arg: $1 (try --help)";;
  esac
done

SCENARIO="$(printf '%s' "$SCENARIO" | tr '[:lower:]' '[:upper:]')"
scenario_row "$SCENARIO" >/dev/null || die "unknown --scenario '$SCENARIO' (expected A, B, or C; see --print-scenarios)"

# Air-gap modes are mutually exclusive: --airgap stands up the in-box zot
# registry + mirror; --airgap-import-only is the registry-FREE ctr-import path
# that explicitly SKIPS that. Setting both is contradictory — fail loudly.
if [ "$AIRGAP" = "1" ] && [ "$AIRGAP_IMPORT_ONLY" = "1" ]; then
  die "--airgap (zot mode) and --airgap-import-only (registry-free) are mutually exclusive — pick one"
fi

[ "$(id -u)" -eq 0 ] || die "must run as root (use sudo). This installs k3s + system packages."

# ── Locate the checked-out repo (for deploy-byo.sh + chart + values) ──────────
# The repo tree is found by the presence of scripts/deploy-byo.sh — NOT by a
# .git dir. A tarball-staged tree (Tarboat T1/G1.2, ka-0000008214) has no .git,
# so every probe below tests for the sentinel file, never for .git. This is what
# lets a friend untar the tarboat bundle and run with NO private-repo clone.
detect_repo_dir() {
  # 1. explicit --repo-dir / REPO_DIR
  if [ -n "$REPO_DIR" ]; then echo "$REPO_DIR"; return; fi
  # 2. the dir this script lives in (scripts/), walk up to repo root
  local self_root; self_root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
  if [ -f "$self_root/scripts/deploy-byo.sh" ]; then echo "$self_root"; return; fi
  # 3. the tarball-staged repo tree under the offline-assets dir (G1.2). The
  #    tarboat bundle unpacks the gardenio subset to OFFLINE_ASSETS/repo/ with
  #    NO .git — accepted purely on the sentinel scripts/deploy-byo.sh.
  if [ -n "$OFFLINE_ASSETS" ] && [ -f "${OFFLINE_ASSETS%/}/repo/scripts/deploy-byo.sh" ]; then
    echo "${OFFLINE_ASSETS%/}/repo"; return
  fi
  # 4. the AMI-baked / cloud-init-staged location (also no .git required: the
  #    cloud-init step symlinks the staged tree here, or an AMI bakes a checkout).
  if [ -f /opt/gardenio/repo/scripts/deploy-byo.sh ]; then echo /opt/gardenio/repo; return; fi
  return 1
}
REPO_DIR="$(detect_repo_dir)" || die "cannot find the gardenio repo (scripts/deploy-byo.sh). Pass --repo-dir."
[ -f "$REPO_DIR/scripts/deploy-byo.sh" ] || die "no scripts/deploy-byo.sh under repo-dir: $REPO_DIR"

read -r S_AGENTS S_INSTANCE S_CPURAM S_GP3 <<<"$(scenario_row "$SCENARIO")"
log "Scenario $SCENARIO → ${S_AGENTS} agents · ${S_INSTANCE} (${S_CPURAM}) · ${S_GP3} GiB gp3 root"
_airgap_mode="off"
[ "$AIRGAP" = "1" ] && _airgap_mode="zot"
[ "$AIRGAP_IMPORT_ONLY" = "1" ] && _airgap_mode="import-only"
log "Repo: $REPO_DIR · namespace: $NAMESPACE · release: $RELEASE · values: $VALUES_FILE · gvisor: $GVISOR · airgap: $_airgap_mode"

# ── Sanity-check the running box against the scenario (warn-only) ────────────
check_box_size() {
  local want_gib have_kib have_gib
  want_gib="$(echo "$S_CPURAM" | sed 's#.*/##; s#GiB##')"
  have_kib="$(awk '/MemTotal/{print $2}' /proc/meminfo 2>/dev/null || echo 0)"
  have_gib=$(( have_kib / 1024 / 1024 ))
  if [ "$have_gib" -gt 0 ] && [ "$have_gib" -lt $(( want_gib * 80 / 100 )) ]; then
    warn "this box has ~${have_gib} GiB RAM but scenario $SCENARIO expects ~${want_gib} GiB."
    warn "agent pods reserve a hard 16 GiB each — undersized box → scheduling failures."
  fi
}
check_box_size

# ── 1. OS prep ───────────────────────────────────────────────────────────────
os_prep() {
  log "[1/5] OS prep (swapoff, kernel modules, sysctls, base packages)"
  swapoff -a || true
  sed -i.bak '/ swap / s/^\([^#]\)/#\1/' /etc/fstab 2>/dev/null || true

  modprobe overlay      || warn "modprobe overlay failed (may be built-in)"
  modprobe br_netfilter || warn "modprobe br_netfilter failed (may be built-in)"
  printf 'overlay\nbr_netfilter\n' >/etc/modules-load.d/gardenio-k8s.conf
  printf 'net.bridge.bridge-nf-call-iptables=1\nnet.bridge.bridge-nf-call-ip6tables=1\nnet.ipv4.ip_forward=1\n' \
    >/etc/sysctl.d/99-gardenio-k8s.conf
  sysctl --system >/dev/null 2>&1 || true

  if command -v dnf >/dev/null 2>&1; then          # Amazon Linux 2023 / RHEL-like
    dnf install -y curl-minimal openssl jq tar git >/dev/null 2>&1 \
      || dnf install -y curl openssl jq tar git >/dev/null 2>&1 || warn "dnf package install had issues"
  elif command -v apt-get >/dev/null 2>&1; then    # Ubuntu / Debian
    apt-get update -y >/dev/null 2>&1 || true
    DEBIAN_FRONTEND=noninteractive apt-get install -y curl ca-certificates openssl jq tar git >/dev/null 2>&1 \
      || warn "apt-get package install had issues"
  else
    warn "no dnf/apt-get — assuming curl/openssl/jq/git already present"
  fi
}

# ── 2. k3s install ───────────────────────────────────────────────────────────
# Single-node install with the P0.2 (ka-0000002781) durability layer: a pinned,
# restart-durable /etc/rancher/k3s/config.yaml (node-ip/tls-san/bind-address,
# SQLite/kine) written + a boot-refresh unit installed BEFORE k3s first starts,
# plus a datastore-backup timer.
install_k3s() {
  log "[2/5] Installing k3s (single-node, embedded containerd)"

  # P0.2 durability: write the pinned config + install the boot-refresh +
  # backup units BEFORE the k3s install, so the very first apiserver start
  # already uses the pinned node-ip/tls-san and survives a later stop/start.
  local dur="$REPO_DIR/scripts/k3s/install-k3s-durability.sh"
  if [ -x "$dur" ]; then
    log "  installing k3s durability layer (pinned config + backup timer)"
    TLS_SANS="${TLS_SANS:-}" BIND_ADDRESS="${BIND_ADDRESS:-0.0.0.0}" "$dur" \
      || warn "k3s durability install reported an issue (continuing)"
  else
    warn "scripts/k3s/install-k3s-durability.sh not present — k3s will use defaults (no IP pinning)"
  fi

  if command -v k3s >/dev/null 2>&1 && systemctl is-active --quiet k3s 2>/dev/null; then
    log "  k3s already installed and active — skipping install"
  else
    # config.yaml (written above) supplies node-ip/tls-san/bind-address; only
    # pass through the version pin via INSTALL_K3S_VERSION. The installer reads
    # /etc/rancher/k3s/config.yaml automatically.
    if [ -n "$K3S_VERSION" ]; then export INSTALL_K3S_VERSION="$K3S_VERSION"; fi
    # Offline: stage the k3s binary + the install.sh so INSTALL_K3S_SKIP_DOWNLOAD
    # makes the installer self-contained (the official k3s air-gap procedure).
    local _k3s_bin _k3s_inst
    if _k3s_bin="$(offline_asset bin/k3s)"; then
      log "  installing k3s from staged binary ($_k3s_bin) — no download"
      install -m 0755 "$_k3s_bin" /usr/local/bin/k3s
      _k3s_inst="$(offline_asset bin/k3s-install.sh)" \
        || die "staged bin/k3s but not bin/k3s-install.sh under $OFFLINE_ASSETS"
      INSTALL_K3S_SKIP_DOWNLOAD=true sh "$_k3s_inst" \
        || die "k3s offline install failed"
    else
      offline_required "bin/k3s + bin/k3s-install.sh"
      curl -sfL https://get.k3s.io | sh - \
        || die "k3s install failed"
    fi
  fi

  export KUBECONFIG=/etc/rancher/k3s/k3s.yaml
  log "  waiting for the node to become Ready..."
  local _
  for _ in $(seq 1 60); do
    if k3s kubectl get nodes 2>/dev/null | grep -q ' Ready'; then break; fi
    sleep 3
  done
  k3s kubectl get nodes 2>/dev/null | grep -q ' Ready' || die "k3s node did not become Ready in time"
  log "  k3s node Ready ($(k3s --version 2>/dev/null | head -1))"
}

# ── 3. gVisor (OPTIONAL) ─────────────────────────────────────────────────────
# RuntimeClass-only sandbox runtime (P0.3 ka-0000002716): runsc + shim install,
# config-v3.toml.tmpl injection (containerd 1.7 vs 2.x schema), PodSecurity
# AdmissionConfiguration exempting runtimeClass=runsc. The matching chart
# overlay (deploy-values/gardenio-gvisor-inbox.yaml, scheduling={}) is added to
# the deploy stage when --gvisor is set. Default OFF.
install_gvisor() {
  if [ "$GVISOR" != "1" ]; then
    log "[3/5] gVisor disabled (--gvisor to enable) — skipping"
    return 0
  fi
  log "[3/5] gVisor requested — installing runsc RuntimeClass-only mode"
  local gv="$REPO_DIR/scripts/gvisor/bootstrap-gvisor.sh"
  if [ -x "$gv" ]; then
    "$gv" || die "gVisor host bootstrap failed"
  else
    die "scripts/gvisor/bootstrap-gvisor.sh not found under $REPO_DIR"
  fi
}

# ── 3b. Host-layer hardening (OPTIONAL, P3.5 ka-0000006978) ───────────────────
# Public-internet host firewall + SSH + IMDSv2. Runs AFTER k3s is up (so k3s's
# own nftables tables already exist and we only ADD a separate table, never
# flush k3s's). Default OFF; --harden (or HARDEN=1) enables it. The matching
# control-plane half is the NetworkPolicy overlay (P3.4) + the perHost TLS
# overlay (P3.6). See gardenio/docs/research/inbox-public-internet-exposure-hardening.md.
harden_host() {
  if [ "$HARDEN" != "1" ]; then
    log "[3b] host hardening disabled (--harden to enable) — skipping"
    return 0
  fi
  log "[3b] host hardening requested — nftables 22/80/443 + SSH key-only + fail2ban + IMDSv2"
  local hs="$REPO_DIR/scripts/k3s/install-host-hardening.sh"
  if [ -x "$hs" ]; then
    "$hs" --operator-user "$OPERATOR_USER" \
      || die "host hardening failed (scripts/k3s/install-host-hardening.sh)"
  else
    die "scripts/k3s/install-host-hardening.sh not found/executable under $REPO_DIR"
  fi
}

# ── 4. air-gapped image import ───────────────────────────────────────────────
# Loads any image tarballs under IMAGE_DIR into k3s's embedded containerd. In
# the zot air-gap path this is belt-and-suspenders (airgap_setup() serves pulls
# from the in-box registry); in --airgap-import-only mode this is the WHOLE
# image story — there is no registry, so the seeded images must be present.
#
# REBOOT SURVIVAL: a bare `ctr images import` populates the containerd content
# store but does NOT survive a k3s datastore wipe and is reclaimable by
# containerd GC. k3s additionally auto-imports any *.tar/*.tar.zst dropped into
# /var/lib/rancher/k3s/agent/images/ on every start. In import-only mode we
# stage the tarballs THERE (preferred per inbox-ec2-sizing-bootstrap.md §3) so
# k3s re-imports them after a reboot/restart, then still do the explicit import
# for immediate availability this run.
K3S_AUTOLOAD_DIR="/var/lib/rancher/k3s/agent/images"
import_images() {
  log "[4/5] Importing air-gapped image tarballs from $IMAGE_DIR (if any)"

  # Import-only mode: stage tarballs in the k3s auto-load dir so they re-import
  # on every k3s start (reboot survival). Do this BEFORE the explicit import so
  # a copy exists even if the explicit import below is skipped.
  if [ "$AIRGAP_IMPORT_ONLY" = "1" ] && [ -d "$IMAGE_DIR" ]; then
    mkdir -p "$K3S_AUTOLOAD_DIR"
    local s staged=0
    for s in "$IMAGE_DIR"/*.tar "$IMAGE_DIR"/*.tar.zst; do
      [ -e "$s" ] || continue
      # Skip if IMAGE_DIR already IS the auto-load dir (don't copy onto self).
      [ "$(cd "$IMAGE_DIR" && pwd)" = "$K3S_AUTOLOAD_DIR" ] && { staged=1; break; }
      if cp -f "$s" "$K3S_AUTOLOAD_DIR/" 2>/dev/null; then
        staged=1
        log "  staged $(basename "$s") → $K3S_AUTOLOAD_DIR/ (k3s auto-load, reboot-survivable)"
      else
        warn "  could not stage $(basename "$s") into $K3S_AUTOLOAD_DIR (continuing)"
      fi
    done
    [ "$staged" = "1" ] || warn "  import-only: no tarballs staged into $K3S_AUTOLOAD_DIR — box will have NO images and pods will fail (agentPullPolicy=Never)"
  fi

  if [ -d "$IMAGE_DIR" ]; then
    local t found=0
    for t in "$IMAGE_DIR"/*.tar "$IMAGE_DIR"/*.tar.zst; do
      [ -e "$t" ] || continue
      found=1
      log "  importing $t"
      k3s ctr images import "$t" 2>/dev/null || warn "  import failed for $t (continuing)"
    done
    if [ "$found" != "1" ]; then
      if [ "$AIRGAP_IMPORT_ONLY" = "1" ]; then
        warn "  no tarballs found under $IMAGE_DIR — import-only box has NO seeded images (pods will ImagePullBackOff under agentPullPolicy=Never)"
      else
        log "  no tarballs found — chart will pull images from their registries"
      fi
    fi
  else
    if [ "$AIRGAP_IMPORT_ONLY" = "1" ]; then
      warn "  $IMAGE_DIR absent — import-only box has NO seeded images (pods will ImagePullBackOff under agentPullPolicy=Never)"
    else
      log "  $IMAGE_DIR absent — chart will pull images from their registries"
    fi
  fi

  # Import-only: pin every imported image so containerd's CRI image GC will not
  # reclaim it under DiskPressure. There is no registry to re-pull from in this
  # mode (agentPullPolicy=Never), so a GC reclaim would wedge pods into
  # ErrImageNeverPull with no recovery path. Re-runs after every (re-)import;
  # the helper is idempotent. (ka-0000006466)
  if [ "$AIRGAP_IMPORT_ONLY" = "1" ]; then
    local pin="$REPO_DIR/scripts/inbox/pin-images.sh"
    if [ -x "$pin" ]; then
      log "  pinning imported images (io.cri.containerd.pinned) so GC won't reclaim them"
      "$pin" || warn "  pin-images.sh failed — imported images are GC-eligible under DiskPressure"
    else
      warn "  $pin not present/executable — imported images are GC-eligible under DiskPressure"
    fi
  fi
}

# ── 5. deploy (delegate to the repo's existing BYO path) ─────────────────────
deploy() {
  log "[5/5] Deploying gardenio chart via scripts/deploy-byo.sh"
  export KUBECONFIG=/etc/rancher/k3s/k3s.yaml
  # k3s ships helm? No — but it ships `kubectl`. deploy-byo.sh needs helm +
  # kubectl + openssl on PATH. Provide a k3s-backed kubectl shim if absent, and
  # install helm if missing.
  ensure_kubectl
  ensure_helm

  local smoke_flag=()
  [ "$RUN_SMOKE" = "1" ] || smoke_flag=(--no-smoke)

  # Layer any operator-supplied overlays first (e.g. the public-safe in-box
  # profile: --extra-values gardenio/helm/gardenio/values-inbox.yaml
  # --extra-values deploy-values/gardenio-inbox.yaml). Relative paths resolve
  # against the repo root. deploy-byo.sh applies them in order after --values.
  local extra_values=()
  local ev ev_path
  for ev in "${EXTRA_VALUES[@]:-}"; do
    [ -n "$ev" ] || continue
    case "$ev" in /*) ev_path="$ev";; *) ev_path="$REPO_DIR/$ev";; esac
    [ -f "$ev_path" ] || die "extra values overlay not found: $ev_path"
    extra_values+=(--extra-values "$ev_path")
    log "  layering overlay: $ev"
  done

  # When gVisor is on, layer the RuntimeClass-only overlay so the chart renders
  # the runsc RuntimeClass with single-node scheduling={} (P0.3).
  if [ "$GVISOR" = "1" ]; then
    local gv_overlay="$REPO_DIR/deploy-values/gardenio-gvisor-inbox.yaml"
    [ -f "$gv_overlay" ] || die "gVisor overlay not found: $gv_overlay"
    extra_values+=(--extra-values "$gv_overlay")
    log "  layering gVisor overlay: deploy-values/gardenio-gvisor-inbox.yaml"
  fi

  # In --airgap-import-only mode, force agents.agentPullPolicy=Never (and
  # global.airgap=true) via the import-only overlay so the kubelet NEVER tries
  # to round-trip the (nonexistent) registry for an agent spawn — there is no
  # zot to fall back to, so a pull must hard-fail fast rather than hang in
  # ImagePullBackOff. Layered LAST so it wins precedence over any operator
  # overlay that set IfNotPresent/Always.
  if [ "$AIRGAP_IMPORT_ONLY" = "1" ]; then
    local io_overlay="$REPO_DIR/deploy-values/gardenio-inbox-import-only.yaml"
    [ -f "$io_overlay" ] || die "import-only overlay not found: $io_overlay"
    extra_values+=(--extra-values "$io_overlay")
    log "  layering import-only overlay: deploy-values/gardenio-inbox-import-only.yaml (agentPullPolicy=Never)"
  fi

  # Pin the image tag to a known-published calver when APP_VERSION is set, so the
  # box never references an image the registry doesn't have yet (ka-0000001967).
  local appver_flag=()
  [ -n "$APP_VERSION" ] && { appver_flag=(--app-version "$APP_VERSION"); log "  pinning image tag (appVersion) → $APP_VERSION"; }

  # Inject the per-box Slack channel id when slack-enabling, so the reusable
  # gardenio-inbox-slack.yaml overlay needs no hand-edit (ka-0000005575).
  local slack_flag=()
  [ -n "$SLACK_CHANNEL" ] && { slack_flag=(--slack-channel "$SLACK_CHANNEL"); log "  setting slackBridge.slack.channel → $SLACK_CHANNEL"; }

  # Flags carry namespace/release; RUN_SMOKE is also passed as --no-smoke when 0.
  "$REPO_DIR/scripts/deploy-byo.sh" \
      --namespace "$NAMESPACE" \
      --release "$RELEASE" \
      --values "$REPO_DIR/$VALUES_FILE" \
      "${extra_values[@]}" \
      "${appver_flag[@]}" \
      "${slack_flag[@]}" \
      "${smoke_flag[@]}" \
    || die "deploy-byo.sh failed"
}

ensure_kubectl() {
  command -v kubectl >/dev/null 2>&1 && return 0
  log "  installing kubectl shim → /usr/local/bin/kubectl (wraps 'k3s kubectl')"
  cat >/usr/local/bin/kubectl <<'KSH'
#!/usr/bin/env bash
exec k3s kubectl "$@"
KSH
  chmod 0755 /usr/local/bin/kubectl
}

ensure_helm() {
  command -v helm >/dev/null 2>&1 && return 0
  local staged
  if staged="$(offline_asset bin/helm)"; then
    log "  installing helm from staged binary ($staged) — no download"
    install -m 0755 "$staged" /usr/local/bin/helm
    return 0
  fi
  offline_required "bin/helm"
  log "  installing helm"
  curl -sfL https://raw.githubusercontent.com/helm/helm/main/scripts/get-helm-3 | bash >/dev/null 2>&1 \
    || die "helm install failed"
}

ensure_crane() {
  command -v crane >/dev/null 2>&1 && return 0
  local staged
  if staged="$(offline_asset bin/crane)"; then
    log "  installing crane from staged binary ($staged) — no download"
    install -m 0755 "$staged" /usr/local/bin/crane
    return 0
  fi
  offline_required "bin/crane"
  log "  installing crane (needed to seed the in-box registry)"
  local arch goarch
  arch="$(uname -m)"
  case "$arch" in
    x86_64|amd64)  goarch="x86_64";;
    aarch64|arm64) goarch="arm64";;
    *) die "unsupported arch for crane: $arch";;
  esac
  curl -fsSL "https://github.com/google/go-containerregistry/releases/download/v0.20.2/go-containerregistry_Linux_${goarch}.tar.gz" \
    | tar xz -C /tmp crane || die "crane download failed"
  install -m 0755 /tmp/crane /usr/local/bin/crane
}

# ── 4b. air-gap image distribution (Phase 4, OFF by default) ─────────────────
# Stand up the in-box zot registry (P4.1), seed it from the crane OCI-layout
# bundle, then redirect every containerd pull to it (P4.2). Runs AFTER k3s is
# installed (configure-mirror restarts k3s so containerd reloads the mirror)
# and BEFORE deploy so the very first chart pull resolves against the in-box
# registry. The box then performs zero external registry pulls at runtime.
airgap_setup() {
  if [ "$AIRGAP_IMPORT_ONLY" = "1" ]; then
    log "[airgap] IMPORT-ONLY mode — skipping zot registry + mirror entirely (images loaded via ctr-import in stage 4; agentPullPolicy forced to Never via overlay)"
    return
  fi
  [ "$AIRGAP" = "1" ] || { log "[airgap] OFF (default) — chart pulls from external registries"; return; }
  log "[airgap] ON — standing up in-box zot registry + containerd mirror (Phase 4)"
  ensure_crane

  local ib="$REPO_DIR/scripts/inbox"
  [ -x "$ib/install-registry.sh" ] || die "missing $ib/install-registry.sh (P4.1)"
  [ -x "$ib/seed-bundle.sh" ]      || die "missing $ib/seed-bundle.sh (P4.1)"
  [ -x "$ib/configure-mirror.sh" ] || die "missing $ib/configure-mirror.sh (P4.2)"

  log "[airgap] 1/3 install zot registry (host systemd unit, 127.0.0.1:5000)"
  # Pass the offline-assets dir + strict flag through so the staged zot binary
  # is used (G1.1) and a missing one hard-fails under --airgap-strict.
  OFFLINE_ASSETS="$OFFLINE_ASSETS" AIRGAP_STRICT="$AIRGAP_STRICT" \
    "$ib/install-registry.sh" --offline-assets "$OFFLINE_ASSETS" \
    || die "install-registry.sh failed"

  log "[airgap] 2/3 seed registry from bundle under $BUNDLE_DIR"
  BUNDLE_DIR="$BUNDLE_DIR" "$ib/seed-bundle.sh" --bundle \
    "$(find "$BUNDLE_DIR" -maxdepth 1 -name 'gardenio-bundle-*.oci.tar.zst' 2>/dev/null | sort -V | tail -1)" \
    || die "seed-bundle.sh failed (is the bundle present under $BUNDLE_DIR?)"

  log "[airgap] 3/3 redirect containerd pulls (strict=$AIRGAP_STRICT)"
  local strict_flag=()
  [ "$AIRGAP_STRICT" = "1" ] && strict_flag=(--strict)
  "$ib/configure-mirror.sh" "${strict_flag[@]}" || die "configure-mirror.sh failed"
}

# ── 4c. prerequisite operators / CRDs (ka-0000000953) ────────────────────────
# Decide whether the in-box overlays are in play. AUTO (WITH_OPERATORS unset)
# installs the operators iff one of the in-box overlays is layered via
# --extra-values / EXTRA_VALUES — those overlays are exactly what turn
# keycloak.runtime + tls(perHost) + serviceMonitor on and thus render the CRs.
inbox_overlay_layered() {
  local ev base want
  for ev in "${EXTRA_VALUES[@]:-}"; do
    [ -n "$ev" ] || continue
    base="$(basename "$ev")"
    for want in "${INBOX_OVERLAY_BASENAMES[@]}"; do
      [ "$base" = "$want" ] && return 0
    done
  done
  return 1
}

# Resolve whether stage 4c runs: explicit WITH_OPERATORS wins; else AUTO.
operators_wanted() {
  case "$WITH_OPERATORS" in
    1) return 0;;
    0) return 1;;
    *) inbox_overlay_layered;;
  esac
}

# Install the prerequisite operators/CRDs the in-box CRs need, with PINNED
# versions, idempotently, BEFORE the helm deploy. Each sub-step is a no-op when
# its CRD/release is already present, so re-running the bootstrap is safe.
install_operators() {
  if ! operators_wanted; then
    if [ "$WITH_OPERATORS" = "0" ]; then
      log "[4c] operators install disabled (--no-operators) — skipping"
    else
      log "[4c] no in-box overlay layered — chart renders no operator-requiring CRs; skipping (use --with-operators to force)"
    fi
    return 0
  fi
  log "[4c] installing prerequisite operators/CRDs (cert-manager $OP_CERT_MANAGER_VERSION, ServiceMonitor CRD $OP_SERVICEMONITOR_CRD_VERSION, EDP keycloak-operator $OP_KEYCLOAK_OPERATOR_VERSION)"
  export KUBECONFIG=/etc/rancher/k3s/k3s.yaml
  ensure_kubectl
  ensure_helm

  # 1/3 cert-manager (Certificate + ClusterIssuer). Full manifest install +
  # wait for the controllers/webhook to be Available before the chart's
  # Certificate/ClusterIssuer apply (the webhook must be live or apply 500s).
  if kubectl get crd certificates.cert-manager.io >/dev/null 2>&1; then
    log "  cert-manager CRDs already present — skipping manifest apply"
  else
    local cm_src
    if cm_src="$(offline_asset manifests/cert-manager.yaml)"; then
      log "  applying cert-manager $OP_CERT_MANAGER_VERSION from staged manifest ($cm_src)"
    else
      offline_required "manifests/cert-manager.yaml"
      cm_src="https://github.com/cert-manager/cert-manager/releases/download/${OP_CERT_MANAGER_VERSION}/cert-manager.yaml"
      log "  applying cert-manager $OP_CERT_MANAGER_VERSION full manifest (download)"
    fi
    kubectl apply -f "$cm_src" \
      || die "cert-manager manifest apply failed"
  fi
  log "  waiting for cert-manager controllers to become Available"
  for d in cert-manager cert-manager-webhook cert-manager-cainjector; do
    kubectl -n cert-manager rollout status "deploy/$d" --timeout=180s \
      || warn "  cert-manager deploy/$d not Available in time (continuing)"
  done

  # 2/3 prometheus-operator ServiceMonitor CRD (server-side apply — the CRD
  # schema exceeds the client-side last-applied-annotation size limit).
  if kubectl get crd servicemonitors.monitoring.coreos.com >/dev/null 2>&1; then
    log "  ServiceMonitor CRD already present — skipping"
  else
    local sm_src
    if sm_src="$(offline_asset manifests/servicemonitor-crd.yaml)"; then
      log "  applying ServiceMonitor CRD $OP_SERVICEMONITOR_CRD_VERSION from staged manifest ($sm_src) (server-side)"
    else
      offline_required "manifests/servicemonitor-crd.yaml"
      sm_src="https://raw.githubusercontent.com/prometheus-operator/prometheus-operator/${OP_SERVICEMONITOR_CRD_VERSION}/example/prometheus-operator-crd/monitoring.coreos.com_servicemonitors.yaml"
      log "  applying prometheus-operator ServiceMonitor CRD $OP_SERVICEMONITOR_CRD_VERSION (server-side, download)"
    fi
    kubectl apply --server-side -f "$sm_src" \
      || die "ServiceMonitor CRD apply failed"
  fi

  # 3/3 EDP keycloak-operator (ClusterKeycloak/KeycloakRealm/KeycloakClient/…).
  # helm install into the operators namespace, pinned chart version.
  #
  # clusterReconciliationEnabled=true is REQUIRED (ka-0000005282): the operator
  # lives in ns/$OPERATORS_NAMESPACE but the gardenio chart creates its
  # KeycloakRealm / KeycloakClient CRs in the gardenio namespace. With the chart
  # default (clusterReconciliationEnabled=false) the operator sets
  # WATCH_NAMESPACE=$OPERATORS_NAMESPACE and its controller-runtime cache only
  # watches that namespace, so the realm/client CRs in ns/gardenio are NEVER
  # reconciled — ClusterKeycloak stays connected:false, the gardenio-inbox realm
  # is never created, and oauth2-proxy CrashLoopBackOffs on OIDC discovery 404
  # ("Realm does not exist"). Cluster-wide reconciliation (WATCH_NAMESPACE="",
  # ClusterRole) makes the operator see CRs in every namespace — this mirrors the
  # cloud acme-helm-chart operators values (clusterReconciliationEnabled: true).
  if kubectl get crd clusterkeycloaks.v1.edp.epam.com >/dev/null 2>&1 \
     || kubectl get crd keycloaks.v1.edp.epam.com >/dev/null 2>&1; then
    log "  EDP keycloak-operator CRDs already present — skipping helm install"
  else
    log "  installing epamedp/keycloak-operator $OP_KEYCLOAK_OPERATOR_VERSION into ns/$OPERATORS_NAMESPACE"
    kubectl create namespace "$OPERATORS_NAMESPACE" --dry-run=client -o yaml | kubectl apply -f -
    # Prefer a staged chart tgz (helm installs from a local path with no repo
    # add/update — fully offline); else add the upstream repo and pull.
    local ko_chart staged_chart
    local ko_version_args=(--version "$OP_KEYCLOAK_OPERATOR_VERSION")
    if staged_chart="$(offline_asset "charts/keycloak-operator-${OP_KEYCLOAK_OPERATOR_VERSION}.tgz")"; then
      log "    using staged chart $staged_chart — no repo fetch"
      ko_chart="$staged_chart"
      ko_version_args=()   # --version is for repo charts; the local tgz IS the version
    else
      offline_required "charts/keycloak-operator-${OP_KEYCLOAK_OPERATOR_VERSION}.tgz"
      helm repo add epamedp "$OP_KEYCLOAK_OPERATOR_REPO" >/dev/null 2>&1 || true
      helm repo update epamedp >/dev/null 2>&1 || helm repo update >/dev/null 2>&1 || true
      ko_chart="epamedp/keycloak-operator"
    fi
    helm upgrade --install keycloak-operator "$ko_chart" \
      "${ko_version_args[@]}" \
      --namespace "$OPERATORS_NAMESPACE" \
      --set clusterReconciliationEnabled=true \
      --wait --timeout 5m \
      || die "keycloak-operator helm install failed"
  fi
  log "[4c] prerequisite operators/CRDs ready"
}

# ── 6. box-config seed (M3, ka-0000003598) ───────────────────────────────────
# Restore the pre-baked, already-redacted box-config seed into the box's fresh
# beads DB, AFTER deploy (daemon up) and — conceptually — before the first agent
# spawn. Delegates to scripts/inbox/seed-config-dump.sh (idempotent, marker-gated).
# OFF unless SEED_CONFIG=1: the seed needs `kd` reachable against the in-box
# daemon, which a hands-off bootstrap can't assume. When on, AIRGAP_STRICT flows
# through so a missing/corrupt seed hard-fails instead of a silent empty-config boot.
seed_config() {
  [ "$SEED_CONFIG" = "1" ] || { log "[6/6] box-config seed OFF (SEED_CONFIG unset) — see gardenio/docs/box-config-seeding.md to seed post-install"; return; }
  local sc="$REPO_DIR/scripts/inbox/seed-config-dump.sh"
  [ -x "$sc" ] || die "SEED_CONFIG=1 but missing $sc"
  log "[6/6] seeding box-config from $SEED_DIR (strict=$AIRGAP_STRICT)"
  local strict_flag=()
  [ "$AIRGAP_STRICT" = "1" ] && strict_flag=(--airgap-strict)
  local seed_path
  seed_path="$(find "$SEED_DIR" -maxdepth 1 -name 'gardenio-seed-*.jsonl' 2>/dev/null | sort -V | tail -1)"
  if [ -n "$seed_path" ]; then
    "$sc" --seed "$seed_path" "${strict_flag[@]}" || die "seed-config-dump.sh failed"
  else
    "$sc" "${strict_flag[@]}" || die "seed-config-dump.sh failed (no seed under $SEED_DIR)"
  fi
}

# ── main ─────────────────────────────────────────────────────────────────────
os_prep
install_k3s
install_gvisor
harden_host
import_images
airgap_setup
install_operators
deploy
seed_config

log "✅ Gardenio in a Box is up (scenario $SCENARIO, namespace $NAMESPACE)."
log "   kubeconfig: /etc/rancher/k3s/k3s.yaml"
log "   pods:       k3s kubectl -n $NAMESPACE get pods"
log "   beads:      k3s kubectl -n $NAMESPACE port-forward svc/${RELEASE}-beads 8080:8080"
