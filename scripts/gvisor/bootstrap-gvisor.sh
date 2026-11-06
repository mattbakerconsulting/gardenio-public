#!/usr/bin/env bash
# bootstrap-gvisor.sh — install gVisor (runsc) as a RuntimeClass-only sandbox
# runtime on a single-node k3s box.
#
# Phase 0 P0.3 (ka-0000002716), epic ka-0000007690. Sources:
# gardenio/docs/research/inbox-ec2-sizing-bootstrap.md §4 and
# gardenio/docs/research/inbox-k8s-distro-choice.md §4.
#
# "RuntimeClass-only mode": a pod runs under gVisor purely by carrying
# `runtimeClassName: runsc` — NO Karpenter node pool, NO dedicated-pool node
# selector/taint, NO freeze/thaw, NO PSA-privileged namespace. The controller's
# only routing is pod.spec.runtimeClassName from the agent's `runtime_class`
# metadata. On one node we just need: (1) runsc + the containerd shim installed,
# (2) the runsc handler registered in containerd via the k3s config TEMPLATE
# (the rendered config.toml is overwritten on every restart), (3) the node
# labelled so the chart's RuntimeClass scheduling matches (or scheduling={} —
# see the gvisor-inbox overlay), (4) an apiserver AdmissionConfiguration that
# exempts runtimeClass=runsc from PodSecurity so the chart need not carve out a
# privileged namespace.
#
# This script does the HOST side (1,2,3,4). The RuntimeClass object itself is
# owned by the Helm chart (gvisor.runtimeClass.create=true,
# scheduling={}) — do NOT hand-apply a second `runsc` RuntimeClass; the chart's
# carries helm.sh/resource-policy: keep and a hand-applied one collides
# (research §4 correction #2).
#
# Usage:
#   sudo scripts/gvisor/bootstrap-gvisor.sh [--runsc-release YYYYMMDD]
#        [--platform systrap|kvm|ptrace] [--node-label gvisor=enabled]
#        [--no-admission-config] [--no-restart]
#
# Env / flags:
#   RUNSC_RELEASE   pinned gVisor release (default: 20260525.0 — matches the
#                   audited/deployed runsc on the gvisor-poc pool; "latest"
#                   allowed but discouraged — pin a tested release per research §4)
#   RUNSC_PLATFORM  runsc platform (default: systrap — correct for EC2 Nitro
#                   guests; kvm only works on .metal bare-metal)
#   NODE_LABEL      node label the chart RuntimeClass scheduling expects, when
#                   you keep the default scheduling (default: gvisor=enabled).
#                   Harmless to set even with scheduling={} (the overlay path).
#   ADMISSION_CONFIG 1=drop apiserver AdmissionConfiguration exempting runsc (default 1)
set -euo pipefail

RUNSC_RELEASE="${RUNSC_RELEASE:-20260525.0}"
RUNSC_PLATFORM="${RUNSC_PLATFORM:-systrap}"
NODE_LABEL="${NODE_LABEL:-gvisor=enabled}"
ADMISSION_CONFIG="${ADMISSION_CONFIG:-1}"
DO_RESTART=1

while [ $# -gt 0 ]; do
  case "$1" in
    --runsc-release) RUNSC_RELEASE="$2"; shift 2;;
    --platform) RUNSC_PLATFORM="$2"; shift 2;;
    --node-label) NODE_LABEL="$2"; shift 2;;
    --no-admission-config) ADMISSION_CONFIG=0; shift;;
    --no-restart) DO_RESTART=0; shift;;
    -h|--help) sed -n '2,45p' "$0"; exit 0;;
    *) echo "unknown arg: $1" >&2; exit 2;;
  esac
done

log()  { printf '\033[1;36m[gvisor]\033[0m %s\n' "$*"; }
warn() { printf '\033[1;33m[gvisor] WARN:\033[0m %s\n' "$*" >&2; }
die()  { printf '\033[1;31m[gvisor] ERROR:\033[0m %s\n' "$*" >&2; exit 1; }

[ "$(id -u)" -eq 0 ] || die "must run as root (installs binaries + edits containerd template)"
command -v k3s >/dev/null 2>&1 || die "k3s not found — install k3s first (scripts/bootstrap-ec2.sh stage 2)"

K3S_CONTAINERD_DIR="/var/lib/rancher/k3s/agent/etc/containerd"

# ── 1. Install runsc + the containerd shim (pinned, checksum-verified) ───────
install_runsc() {
  if command -v runsc >/dev/null 2>&1 && [ "${FORCE_RUNSC:-0}" != "1" ]; then
    log "runsc already installed ($(runsc --version 2>/dev/null | head -1)) — skipping download"
    return 0
  fi
  local arch url tmp
  arch="$(uname -m)"   # x86_64 / aarch64 — gVisor uses these names directly
  url="https://storage.googleapis.com/gvisor/releases/release/${RUNSC_RELEASE}/${arch}"
  tmp="$(mktemp -d)"; trap 'rm -rf "$tmp"' RETURN
  log "downloading runsc + shim (release ${RUNSC_RELEASE}, arch ${arch})"
  local f
  for f in runsc containerd-shim-runsc-v1; do
    curl -sfL "${url}/${f}"        -o "${tmp}/${f}"        || die "download failed: ${url}/${f}"
    curl -sfL "${url}/${f}.sha512" -o "${tmp}/${f}.sha512" || die "checksum download failed: ${url}/${f}.sha512"
  done
  # Verify checksums (the .sha512 files reference the bare filename, so cd in).
  ( cd "$tmp" && sha512sum -c runsc.sha512 containerd-shim-runsc-v1.sha512 ) \
    || die "runsc/shim checksum verification FAILED"
  install -m 0755 "${tmp}/runsc" "${tmp}/containerd-shim-runsc-v1" /usr/local/bin/
  log "installed runsc + containerd-shim-runsc-v1 to /usr/local/bin ($(runsc --version 2>/dev/null | head -1))"
}

# ── 2. Register the runsc handler in containerd via the k3s template ─────────
# CRITICAL (research §4 correction #1): the template filename + CRI plugin path
# depend on the containerd MAJOR version k3s ships. containerd 2.x →
# config-v3.toml.tmpl with [plugins.'io.containerd.cri.v1.runtime'...]; containerd
# 1.7 → config.toml.tmpl with io.containerd.grpc.v1.cri. Pairing the v3 schema
# with the v2 filename silently registers NOTHING. Detect first.
inject_containerd_runsc() {
  mkdir -p "$K3S_CONTAINERD_DIR"
  local cver major tmpl
  cver="$(detect_containerd_major)"
  major="${cver%%.*}"
  log "detected containerd major version: ${cver:-unknown}"

  if [ "$major" = "2" ] || [ "$major" = "3" ]; then
    tmpl="$K3S_CONTAINERD_DIR/config-v3.toml.tmpl"
    log "writing $tmpl (containerd ${major}.x schema)"
    cat >"$tmpl" <<EOF
{{ template "base" . }}

# gVisor (runsc) runtime handler — Gardenio in a Box P0.3 (ka-0000002716).
# containerd ${major}.x CRI v1 schema. Pod runs under gVisor via
# runtimeClassName: runsc (RuntimeClass handler = "runsc" = this block name).
[plugins.'io.containerd.cri.v1.runtime'.containerd.runtimes.runsc]
  runtime_type = 'io.containerd.runsc.v1'
[plugins.'io.containerd.cri.v1.runtime'.containerd.runtimes.runsc.options]
  TypeUrl = 'io.containerd.runsc.v1.options'
  ConfigPath = '/etc/containerd/runsc.toml'
EOF
  else
    # containerd 1.7 (older k3s) — config.toml.tmpl + grpc.v1.cri schema.
    tmpl="$K3S_CONTAINERD_DIR/config.toml.tmpl"
    log "writing $tmpl (containerd 1.7 schema)"
    cat >"$tmpl" <<EOF
{{ template "base" . }}

# gVisor (runsc) runtime handler — Gardenio in a Box P0.3 (ka-0000002716).
# containerd 1.7 grpc.v1.cri schema.
[plugins."io.containerd.grpc.v1.cri".containerd.runtimes.runsc]
  runtime_type = "io.containerd.runsc.v1"
[plugins."io.containerd.grpc.v1.cri".containerd.runtimes.runsc.options]
  TypeUrl = "io.containerd.runsc.v1.options"
  ConfigPath = "/etc/containerd/runsc.toml"
EOF
  fi

  # runsc per-runtime options: pin the platform. systrap is correct on EC2 Nitro
  # guests (no nested virt / no /dev/kvm); kvm only on .metal.
  cat >/etc/containerd/runsc.toml <<EOF
# runsc options — Gardenio in a Box P0.3 (ka-0000002716).
[runsc_config]
  platform = "${RUNSC_PLATFORM}"
EOF
  log "wrote /etc/containerd/runsc.toml (platform=${RUNSC_PLATFORM})"
}

detect_containerd_major() {
  # k3s embeds containerd; `k3s ctr version` prints the server version.
  local v
  v="$(k3s ctr version 2>/dev/null | awk '/Version/{print $2; exit}' | sed 's/^v//')"
  [ -n "$v" ] || v="$(containerd --version 2>/dev/null | grep -oE 'v[0-9]+\.[0-9]+\.[0-9]+' | head -1 | sed 's/^v//')"
  echo "$v"
}

# ── 3. Label the node (for the chart RuntimeClass default scheduling) ────────
label_node() {
  [ -n "$NODE_LABEL" ] || return 0
  export KUBECONFIG=/etc/rancher/k3s/k3s.yaml
  local node
  node="$(k3s kubectl get nodes -o name 2>/dev/null | head -1 | cut -d/ -f2)"
  [ -n "$node" ] || { warn "no node found to label (k3s not ready?) — skipping"; return 0; }
  log "labelling node $node with $NODE_LABEL"
  k3s kubectl label node "$node" "$NODE_LABEL" --overwrite >/dev/null 2>&1 \
    || warn "node label failed (continuing — the gvisor-inbox overlay uses scheduling={} anyway)"
}

# ── 4. PodSecurity AdmissionConfiguration exempting runtimeClass=runsc ───────
# On the in-box single node WE own the apiserver, so instead of the prod chart's
# dedicated PSA-privileged namespace we drop a cluster-level AdmissionConfiguration
# exempting runtimeClasses: [runsc] and wire it via a k3s apiserver arg. Scope is
# broad (any runsc pod), so the agent namespace must stay locked down (Phase 3).
write_admission_config() {
  [ "$ADMISSION_CONFIG" = "1" ] || { log "skipping AdmissionConfiguration (--no-admission-config)"; return 0; }
  local dir="/var/lib/rancher/k3s/server/psa"
  mkdir -p "$dir"
  cat >"$dir/admission-config.yaml" <<'EOF'
# PodSecurity AdmissionConfiguration — Gardenio in a Box P0.3 (ka-0000002716).
# Exempts runtimeClass=runsc so runsc-pinned agent pods (and any privileged
# freeze/thaw substrate, if later enabled) admit without a per-namespace
# PSA-privileged label. The exemption is BROAD — keep the agent namespace tight.
apiVersion: apiserver.config.k8s.io/v1
kind: AdmissionConfiguration
plugins:
  - name: PodSecurity
    configuration:
      apiVersion: pod-security.admission.config.k8s.io/v1
      kind: PodSecurityConfiguration
      defaults:
        enforce: "baseline"
        enforce-version: "latest"
        audit: "restricted"
        audit-version: "latest"
        warn: "restricted"
        warn-version: "latest"
      exemptions:
        runtimeClasses: ["runsc"]
EOF
  log "wrote $dir/admission-config.yaml (PodSecurity exempts runtimeClass=runsc)"

  # Wire it into the apiserver via a k3s config drop-in. k3s merges
  # /etc/rancher/k3s/config.yaml.d/*.yaml into the server config; using a
  # drop-in keeps P0.2's main config.yaml (node-ip/tls-san) intact.
  local dropin_dir="/etc/rancher/k3s/config.yaml.d"
  mkdir -p "$dropin_dir"
  cat >"$dropin_dir/10-gvisor-psa.yaml" <<EOF
# Gardenio in a Box P0.3 (ka-0000002716) — wire the PSA AdmissionConfiguration.
kube-apiserver-arg:
  - "admission-control-config-file=$dir/admission-config.yaml"
EOF
  log "wrote $dropin_dir/10-gvisor-psa.yaml (apiserver --admission-control-config-file)"
}

restart_k3s() {
  [ "$DO_RESTART" = "1" ] || { log "skipping k3s restart (--no-restart) — restart manually to apply"; return 0; }
  log "restarting k3s to apply containerd template + apiserver admission config"
  systemctl restart k3s || die "k3s restart failed"
  export KUBECONFIG=/etc/rancher/k3s/k3s.yaml
  local _
  for _ in $(seq 1 60); do
    k3s kubectl get nodes 2>/dev/null | grep -q ' Ready' && break
    sleep 3
  done
  k3s kubectl get nodes 2>/dev/null | grep -q ' Ready' || die "k3s node did not return Ready after restart"
  log "k3s back up"
}

verify() {
  export KUBECONFIG=/etc/rancher/k3s/k3s.yaml
  # The containerd runtime handler is registered iff k3s re-rendered config.toml
  # from the template. Confirm the rendered config mentions runsc.
  if grep -rq 'runsc' "$K3S_CONTAINERD_DIR/config.toml" 2>/dev/null; then
    log "✅ containerd rendered config registers the runsc handler"
  else
    warn "runsc not found in rendered $K3S_CONTAINERD_DIR/config.toml — handler may not be active (check after restart)"
  fi
}

# ── main ─────────────────────────────────────────────────────────────────────
install_runsc
inject_containerd_runsc
write_admission_config
restart_k3s
label_node
verify

log "✅ gVisor RuntimeClass-only mode installed."
log "   Next: deploy the chart with the gVisor in-box overlay so the runsc"
log "   RuntimeClass exists with single-node scheduling={}:"
log "     scripts/bootstrap-ec2.sh ... --gvisor   (adds deploy-values/gardenio-gvisor-inbox.yaml)"
log "   Then a pod with runtimeClassName: runsc runs sandboxed on this node."
