#!/usr/bin/env bash
# configure-mirror.sh — Gardenio in a Box: redirect ALL containerd image pulls
# to the in-box zot registry on 127.0.0.1:5000.
#
# Phase 4 P4.2 (ka-0000006135), epic ka-0000007881. Source research:
# gardenio/docs/research/inbox-image-distribution.md (ka-0000005069) and
# synthesis gardenio/docs/gardenio-in-a-box-synthesis.md §2.G / Phase 4.
#
# WHY a NODE-LEVEL containerd mirror (not repointing global.imageRegistry):
#   The three most important runtime refs do NOT flow through Helm — the agent
#   image is COOP_IMAGE on the controller env, and init-clone + DinD are Go
#   constants (manager.go: InitCloneImage=alpine/git, DindImage=docker:dind).
#   Repointing global.imageRegistry moves NONE of them. A containerd mirror
#   rewrites EVERY pull regardless of who set the ref (Go const, env, Helm, or
#   stray ghcr.io/public.ecr.aws refs). Helm/Go refs stay byte-identical to
#   prod; the mirror does the redirection. This is the single deciding factor.
#
# WHAT THIS WRITES:
#   /etc/rancher/k3s/registries.yaml — a `mirrors:` entry for EVERY distinct
#   upstream host the chart/Go/values reference (registry.gitlab.com,
#   docker.io, ghcr.io, public.ecr.aws, quay.io) → http://127.0.0.1:5000,
#   each with capabilities ["pull","resolve"] and insecure_skip_verify (plain
#   HTTP on loopback). Because seed-bundle.sh seeded the zot under the
#   IDENTICAL repo paths, NO path rewrite is needed.
#
#   For STRICT air-gap, also adds `--disable-default-registry-endpoint` to the
#   k3s config so containerd NEVER falls back to the upstream endpoint when the
#   mirror lacks a blob (a missing image fails fast instead of leaking a pull).
#   Off by default (--strict to enable) so the box can still fall back to
#   upstream in the non-air-gapped convenience install.
#
# Idempotent: re-running rewrites registries.yaml + (optionally) the k3s
# config flag and restarts k3s so containerd reloads.
#
# Usage:
#   sudo scripts/inbox/configure-mirror.sh [--registry 127.0.0.1:5000]
#        [--hosts "registry.gitlab.com docker.io ghcr.io public.ecr.aws quay.io"]
#        [--strict] [--no-strict] [--k3s-config /etc/rancher/k3s/config.yaml]
#        [--registries /etc/rancher/k3s/registries.yaml] [--no-restart]
#
# Env / flags (flags win over env):
#   REGISTRY       in-box registry endpoint        (default: 127.0.0.1:5000)
#   MIRROR_HOSTS   space list of upstream hosts     (default: the 5 below)
#   STRICT         1=add --disable-default-registry-endpoint (default: 0)
#   K3S_CONFIG     k3s server config path           (default: /etc/rancher/k3s/config.yaml)
#   REGISTRIES     registries.yaml path             (default: /etc/rancher/k3s/registries.yaml)
#   RESTART        1=restart k3s after writing       (default: 1)
set -euo pipefail

REGISTRY="${REGISTRY:-127.0.0.1:5000}"
# Every distinct upstream host gardenio refs (verified against values.yaml +
# the Go image constants in manager.go):
#   registry.gitlab.com  — 13 first-party + the /mirror/* images
#   docker.io            — alpine/git, docker:dind (Go constants)
#   ghcr.io              — kiwigrid/k8s-sidecar, berriai/litellm (values.yaml)
#   public.ecr.aws       — docker/library/ubuntu (values.yaml:914)
#   quay.io              — oauth2-proxy (mirror sources; defensive)
MIRROR_HOSTS="${MIRROR_HOSTS:-registry.gitlab.com docker.io ghcr.io public.ecr.aws quay.io}"
STRICT="${STRICT:-0}"
K3S_CONFIG="${K3S_CONFIG:-/etc/rancher/k3s/config.yaml}"
REGISTRIES="${REGISTRIES:-/etc/rancher/k3s/registries.yaml}"
RESTART="${RESTART:-1}"

while [ $# -gt 0 ]; do
  case "$1" in
    --registry)    REGISTRY="$2"; shift 2;;
    --hosts)       MIRROR_HOSTS="$2"; shift 2;;
    --strict)      STRICT=1; shift;;
    --no-strict)   STRICT=0; shift;;
    --k3s-config)  K3S_CONFIG="$2"; shift 2;;
    --registries)  REGISTRIES="$2"; shift 2;;
    --no-restart)  RESTART=0; shift;;
    -h|--help)     sed -n '2,52p' "$0"; exit 0;;
    *) echo "unknown arg: $1" >&2; exit 2;;
  esac
done

log()  { printf '\033[1;36m[mirror]\033[0m %s\n' "$*"; }
warn() { printf '\033[1;33m[mirror] WARN:\033[0m %s\n' "$*" >&2; }
die()  { printf '\033[1;31m[mirror] FATAL:\033[0m %s\n' "$*" >&2; exit 1; }

[ "$(id -u)" = "0" ] || die "must run as root (writes /etc/rancher/k3s)"

# ── 1. Write registries.yaml — mirror every host → the in-box zot ────────────
# k3s reads /etc/rancher/k3s/registries.yaml and renders it into containerd's
# hosts.toml config_path at startup. capabilities pull+resolve covers both
# manifest resolution and blob pulls; insecure_skip_verify allows plain HTTP
# on the loopback endpoint (the registry never leaves the box).
write_registries() {
  install -d -m 0755 "$(dirname "$REGISTRIES")"
  log "writing $REGISTRIES (hosts: $MIRROR_HOSTS → http://$REGISTRY)"
  {
    echo "# Managed by scripts/inbox/configure-mirror.sh (Gardenio in a Box P4.2)."
    echo "# Redirects every image pull to the in-box zot registry so the box"
    echo "# performs ZERO external registry pulls at runtime. Pod/Go image refs"
    echo "# stay byte-identical to prod; this node config does the redirection."
    echo "mirrors:"
    for host in $MIRROR_HOSTS; do
      echo "  \"${host}\":"
      echo "    endpoint:"
      echo "      - \"http://${REGISTRY}\""
    done
    echo "configs:"
    echo "  \"${REGISTRY}\":"
    echo "    tls:"
    echo "      insecure_skip_verify: true"
  } > "$REGISTRIES"
}

# ── 2. (strict) add --disable-default-registry-endpoint to the k3s config ────
# Without this, containerd falls back to the UPSTREAM endpoint when the mirror
# can't serve a blob — defeating air-gap (it would leak a pull to the dead
# GitLab registry). With it, a missing image fails fast and visibly.
enable_strict() {
  [ "$STRICT" = "1" ] || { log "strict mode OFF (upstream fallback allowed) — pass --strict for air-gap"; return; }
  install -d -m 0755 "$(dirname "$K3S_CONFIG")"
  touch "$K3S_CONFIG"
  if grep -q '^disable-default-registry-endpoint:' "$K3S_CONFIG" 2>/dev/null; then
    log "strict flag already present in $K3S_CONFIG"
  else
    log "adding 'disable-default-registry-endpoint: true' to $K3S_CONFIG (strict air-gap)"
    printf '\n# Gardenio in a Box P4.2: strict air-gap — no upstream fallback.\ndisable-default-registry-endpoint: true\n' \
      >> "$K3S_CONFIG"
  fi
}

# ── 3. restart k3s so containerd reloads registries.yaml ─────────────────────
restart_k3s() {
  [ "$RESTART" = "1" ] || { warn "--no-restart: registries.yaml written but k3s NOT restarted (containerd won't reload until next restart)"; return; }
  if systemctl list-unit-files 2>/dev/null | grep -q '^k3s.service'; then
    log "restarting k3s.service so containerd reloads the mirror config"
    systemctl restart k3s.service
  elif systemctl list-unit-files 2>/dev/null | grep -q '^k3s-agent.service'; then
    log "restarting k3s-agent.service"
    systemctl restart k3s-agent.service
  else
    warn "no k3s systemd unit found — restart k3s manually so containerd reloads $REGISTRIES"
  fi
}

# ── 4. verify the mirror serves a known image ────────────────────────────────
verify() {
  log "verifying registry ${REGISTRY} is reachable"
  curl -fsS "http://${REGISTRY}/v2/" >/dev/null 2>&1 \
    || warn "registry ${REGISTRY} not answering — run install-registry.sh + seed-bundle.sh first"
  if command -v k3s >/dev/null 2>&1; then
    log "current containerd images (k3s ctr images ls | head):"
    k3s ctr images ls 2>/dev/null | head -5 || true
  fi
}

write_registries
enable_strict
restart_k3s
verify

log "done. Every pull from [$MIRROR_HOSTS] now redirects to http://${REGISTRY}."
[ "$STRICT" = "1" ] && log "STRICT: --disable-default-registry-endpoint set — no upstream fallback."
log "Smoke-test: 'k3s ctr images pull <registry>/gardenio/controller:<tag>'"
log "should resolve against the in-box zot with NO external network."
