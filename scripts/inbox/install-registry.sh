#!/usr/bin/env bash
# install-registry.sh — Gardenio in a Box: in-box zot OCI registry as a host
# systemd unit on 127.0.0.1:5000.
#
# Phase 4 P4.1 (ka-0000003363), epic ka-0000007881. Source research:
# gardenio/docs/research/inbox-image-distribution.md (ka-0000005069) and
# synthesis gardenio/docs/gardenio-in-a-box-synthesis.md §2.G / Phase 4.
#
# WHY a host systemd unit (not a k8s pod):
#   The agent container is hardcoded ImagePullPolicy: PullAlways
#   (manager.go:982) and ~11 first-party Deployments default to Always. Every
#   on-demand agent spawn forces a registry round-trip, so the in-box registry
#   is HOT-PATH, not install-only: it must be always-on, reboot-surviving, and
#   started BEFORE kubelet (chicken-and-egg — it cannot itself be a k8s pod).
#   Hence: a host systemd unit (Restart=always, Before=k3s.service).
#
# WHY zot (not registry:2):
#   Each calver upgrade re-seeds a fresh multi-GB bundle; stale blobs
#   accumulate. zot does online GC + layer dedup; registry:2 needs
#   stop-the-world GC — a hot-path outage given PullAlways.
#
# This script does the HOST side: install the zot binary, write its config +
# the systemd unit, and start it. SEEDING the registry from the crane bundle is
# a SEPARATE step — see scripts/inbox/seed-bundle.sh. Redirecting pulls to this
# registry is P4.2 — see scripts/inbox/configure-mirror.sh (containerd mirror).
#
# Idempotent: re-running re-installs the (pinned) binary, rewrites the config +
# unit, and restarts. Data on a persistent host path survives restarts/reboots.
#
# Usage:
#   sudo scripts/inbox/install-registry.sh [--zot-version vX.Y.Z]
#        [--listen 127.0.0.1] [--port 5000] [--data-dir /var/lib/zot]
#        [--no-start] [--no-dedup]
#
# Env / flags (flags win over env):
#   ZOT_VERSION    pinned zot release tag           (default: v2.1.2)
#   LISTEN_ADDR    loopback listen address          (default: 127.0.0.1)
#   PORT           registry port                    (default: 5000)
#   DATA_DIR       persistent blob/metadata path    (default: /var/lib/zot)
#   START          1=enable+start the unit          (default: 1)
#   DEDUP          true=zot storage dedup on         (default: true)
set -euo pipefail

ZOT_VERSION="${ZOT_VERSION:-v2.1.2}"
LISTEN_ADDR="${LISTEN_ADDR:-127.0.0.1}"
PORT="${PORT:-5000}"
DATA_DIR="${DATA_DIR:-/var/lib/zot}"
START="${START:-1}"
DEDUP="${DEDUP:-true}"
# Tarboat T1/G1.1 (ka-0000005747): prefer a staged zot binary under
# OFFLINE_ASSETS/bin/zot over a GitHub download, so a true air-gap install
# fetches nothing. --airgap-strict (AIRGAP_STRICT=1) makes a missing staged
# binary a hard fail instead of a network fallback.
OFFLINE_ASSETS="${OFFLINE_ASSETS:-/opt/gardenio/tarboat}"
AIRGAP_STRICT="${AIRGAP_STRICT:-0}"

while [ $# -gt 0 ]; do
  case "$1" in
    --zot-version) ZOT_VERSION="$2"; shift 2;;
    --listen)      LISTEN_ADDR="$2"; shift 2;;
    --port)        PORT="$2"; shift 2;;
    --data-dir)    DATA_DIR="$2"; shift 2;;
    --offline-assets) OFFLINE_ASSETS="$2"; shift 2;;
    --no-start)    START=0; shift;;
    --no-dedup)    DEDUP="false"; shift;;
    -h|--help)     sed -n '2,40p' "$0"; exit 0;;
    *) echo "unknown arg: $1" >&2; exit 2;;
  esac
done

log()  { printf '\033[1;36m[zot]\033[0m %s\n' "$*"; }
warn() { printf '\033[1;33m[zot] WARN:\033[0m %s\n' "$*" >&2; }
die()  { printf '\033[1;31m[zot] FATAL:\033[0m %s\n' "$*" >&2; exit 1; }

[ "$(id -u)" = "0" ] || die "must run as root (writes /usr/local/bin, /etc, systemd)"

ZOT_BIN="/usr/local/bin/zot"
ZOT_CONFIG="/etc/zot/config.json"
ZOT_UNIT="/etc/systemd/system/zot.service"

# ── 1. Install the pinned zot binary ─────────────────────────────────────────
install_binary() {
  local arch goarch url
  arch="$(uname -m)"
  case "$arch" in
    x86_64|amd64)  goarch="amd64";;
    aarch64|arm64) goarch="arm64";;
    *) die "unsupported arch: $arch";;
  esac

  if [ -x "$ZOT_BIN" ] && "$ZOT_BIN" --version 2>/dev/null | grep -q "${ZOT_VERSION#v}"; then
    log "zot ${ZOT_VERSION} already installed at $ZOT_BIN"
    return
  fi

  # Offline: a staged zot binary short-circuits the GitHub download.
  local staged="${OFFLINE_ASSETS%/}/bin/zot"
  if [ -n "$OFFLINE_ASSETS" ] && [ -s "$staged" ]; then
    log "installing zot from staged binary ($staged) — no download"
    install -m 0755 "$staged" "$ZOT_BIN"
    log "installed $($ZOT_BIN --version 2>/dev/null | head -1 || echo zot)"
    return
  fi
  if [ "$AIRGAP_STRICT" = "1" ]; then
    die "--airgap-strict: staged zot binary missing (${staged}). Stage it under $OFFLINE_ASSETS, or drop --airgap-strict."
  fi

  url="https://github.com/project-zot/zot/releases/download/${ZOT_VERSION}/zot-linux-${goarch}-minimal"
  log "downloading zot ${ZOT_VERSION} (${goarch}) from ${url}"
  local tmp
  tmp="$(mktemp)"
  curl -fsSL "$url" -o "$tmp" || die "download failed: $url"
  install -m 0755 "$tmp" "$ZOT_BIN"
  rm -f "$tmp"
  log "installed $($ZOT_BIN --version 2>/dev/null | head -1 || echo zot)"
}

# ── 2. Write the zot config ──────────────────────────────────────────────────
# Plain HTTP on loopback (127.0.0.1) — pragmatic, no certs (research open-q 6).
# The registry is only ever reached by the node's containerd over loopback;
# it is never exposed off-box (the systemd unit binds LISTEN_ADDR only, and
# the box's security group leaves only 80/443/SSH open — Phase 3).
write_config() {
  install -d -m 0755 "$(dirname "$ZOT_CONFIG")"
  install -d -m 0755 "$DATA_DIR"
  log "writing $ZOT_CONFIG (listen ${LISTEN_ADDR}:${PORT}, data ${DATA_DIR}, dedup=${DEDUP})"
  cat > "$ZOT_CONFIG" <<EOF
{
  "distSpecVersion": "1.1.0",
  "storage": {
    "rootDirectory": "${DATA_DIR}",
    "dedupe": ${DEDUP},
    "gc": true,
    "gcDelay": "1h",
    "gcInterval": "24h"
  },
  "http": {
    "address": "${LISTEN_ADDR}",
    "port": "${PORT}"
  },
  "log": {
    "level": "info"
  }
}
EOF
}

# ── 3. Write + load the systemd unit ─────────────────────────────────────────
# Ordering is the crux: zot MUST be up before k3s/kubelet so the very first
# image pull (PullAlways) resolves against a live registry. Before=k3s.service
# orders us ahead of k3s; Restart=always keeps the hot path alive.
write_unit() {
  log "writing $ZOT_UNIT (Before=k3s.service, Restart=always)"
  cat > "$ZOT_UNIT" <<EOF
[Unit]
Description=zot OCI registry (gardenio-in-a-box in-box image distribution)
Documentation=https://zotregistry.dev
After=network-online.target
Wants=network-online.target
# Start before k3s so the first (PullAlways) image pull resolves locally.
Before=k3s.service k3s-agent.service

[Service]
Type=simple
ExecStart=${ZOT_BIN} serve ${ZOT_CONFIG}
Restart=always
RestartSec=2
# Hardening — the registry needs nothing but its data dir + loopback.
NoNewPrivileges=true
ProtectSystem=strict
ProtectHome=true
PrivateTmp=true
ReadWritePaths=${DATA_DIR}
LimitNOFILE=65536

[Install]
WantedBy=multi-user.target
EOF
  systemctl daemon-reload
}

# ── 4. Enable + start ────────────────────────────────────────────────────────
start_unit() {
  systemctl enable zot.service >/dev/null 2>&1 || true
  systemctl restart zot.service
  log "waiting for registry to answer on ${LISTEN_ADDR}:${PORT} ..."
  local _i
  for _i in $(seq 1 30); do
    if curl -fsS "http://${LISTEN_ADDR}:${PORT}/v2/" >/dev/null 2>&1; then
      log "registry is up: http://${LISTEN_ADDR}:${PORT}/v2/"
      return 0
    fi
    sleep 1
  done
  systemctl status zot.service --no-pager -l || true
  die "registry did not come up within 30s"
}

# ── main ─────────────────────────────────────────────────────────────────────
install_binary
write_config
write_unit
if [ "$START" = "1" ]; then
  start_unit
else
  log "--no-start: unit written but not started"
fi

log "done. Next: seed it with scripts/inbox/seed-bundle.sh, then redirect"
log "containerd pulls with scripts/inbox/configure-mirror.sh (P4.2)."
