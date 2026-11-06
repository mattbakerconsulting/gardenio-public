#!/usr/bin/env bash
# write-k3s-config.sh — generate a pinned, restart-durable k3s server config.
#
# Phase 0 P0.2 (ka-0000002781), epic ka-0000007690. Source:
# gardenio/docs/research/inbox-k8s-distro-choice.md "Single-EC2 install sketch".
#
# Writes /etc/rancher/k3s/config.yaml BEFORE k3s first starts so the install
# picks it up. Two durability concerns it solves:
#
#  1. EC2 stop/start changes the private IP. If the apiserver TLS cert was
#     signed for the OLD IP and kubeconfig/--node-ip point at it, the apiserver
#     comes back with a cert that doesn't match the new address and kubectl/agents
#     break. We pin --node-ip / --advertise-address to the CURRENT private IP at
#     each boot (re-derived from IMDS), and add the STABLE addresses (a fixed EIP,
#     a DNS name, 127.0.0.1) to --tls-san so the cert always covers a reachable,
#     non-changing SAN. (Per the research §9 + open-question 9.)
#  2. Single-node datastore = SQLite/kine (the research recommendation for one
#     node). NB: `k3s etcd-snapshot` does NOT apply to kine — see
#     scripts/k3s/k3s-datastore-backup.sh for the correct kine backup path.
#
# Idempotent: re-running rewrites the config with the current IP. Pair with a
# systemd boot unit (install-k3s-durability.sh) so the IP is refreshed before
# k3s starts on every boot, surviving a stop/start IP change.
#
# Env / flags:
#   NODE_IP        override the auto-detected private IPv4 (default: IMDS / route)
#   TLS_SANS       space/comma list of EXTRA stable SANs (EIP, DNS) for the cert
#   BIND_ADDRESS   apiserver bind address (default: 0.0.0.0; loopback-only later phase)
#   K3S_CONFIG     output path (default: /etc/rancher/k3s/config.yaml)
#   DISABLE        space list of bundled components to disable (default: none —
#                  keep traefik/servicelb/local-storage, the reuse win)
set -euo pipefail

NODE_IP="${NODE_IP:-}"
TLS_SANS="${TLS_SANS:-}"
BIND_ADDRESS="${BIND_ADDRESS:-0.0.0.0}"
K3S_CONFIG="${K3S_CONFIG:-/etc/rancher/k3s/config.yaml}"
DISABLE="${DISABLE:-}"

while [ $# -gt 0 ]; do
  case "$1" in
    --node-ip) NODE_IP="$2"; shift 2;;
    --tls-san) TLS_SANS="$TLS_SANS $2"; shift 2;;
    --bind-address) BIND_ADDRESS="$2"; shift 2;;
    --config) K3S_CONFIG="$2"; shift 2;;
    --disable) DISABLE="$DISABLE $2"; shift 2;;
    -h|--help) sed -n '2,40p' "$0"; exit 0;;
    *) echo "unknown arg: $1" >&2; exit 2;;
  esac
done

log() { printf '\033[1;36m[k3s-config]\033[0m %s\n' "$*"; }

# ── Detect the current private IPv4 ──────────────────────────────────────────
# IMDSv2 first (token-gated, hop-limit-1 friendly), falling back to the route
# table for non-EC2 / metadata-disabled hosts.
detect_node_ip() {
  [ -n "$NODE_IP" ] && { echo "$NODE_IP"; return; }
  local tok ip
  tok="$(curl -sf -X PUT 'http://169.254.169.254/latest/api/token' \
        -H 'X-aws-ec2-metadata-token-ttl-seconds: 60' 2>/dev/null || true)"
  if [ -n "$tok" ]; then
    ip="$(curl -sf -H "X-aws-ec2-metadata-token: $tok" \
         'http://169.254.169.254/latest/meta-data/local-ipv4' 2>/dev/null || true)"
  fi
  [ -z "${ip:-}" ] && ip="$(ip -4 route get 1.1.1.1 2>/dev/null | awk '{for(i=1;i<=NF;i++) if($i=="src"){print $(i+1); exit}}')"
  echo "${ip:-}"
}

NODE_IP="$(detect_node_ip)"
[ -n "$NODE_IP" ] || { echo "ERROR: could not determine node private IPv4 (pass --node-ip)" >&2; exit 1; }

# Build the TLS-SAN list: current node IP + 127.0.0.1 + localhost + any stable
# extras. Stable extras (a fixed EIP, a DNS name) are what keep the cert valid
# across an IP-changing stop/start; the current node IP is added so the cert
# also matches the address k3s advertises right now.
declare -a sans=("$NODE_IP" "127.0.0.1" "localhost")
# normalize comma/space-separated extras
for s in ${TLS_SANS//,/ }; do [ -n "$s" ] && sans+=("$s"); done

# de-dup while preserving order
declare -A seen; declare -a uniq_sans
for s in "${sans[@]}"; do [ -n "${seen[$s]:-}" ] && continue; seen[$s]=1; uniq_sans+=("$s"); done

log "node-ip=$NODE_IP  bind-address=$BIND_ADDRESS  tls-san=[${uniq_sans[*]}]"

mkdir -p "$(dirname "$K3S_CONFIG")"
{
  echo "# Managed by scripts/k3s/write-k3s-config.sh (Gardenio in a Box P0.2, ka-0000002781)."
  echo "# Regenerated on every boot before k3s starts — DO NOT hand-edit."
  echo ""
  echo "write-kubeconfig-mode: \"0644\""
  echo ""
  echo "# Pin advertised/node IP to the CURRENT private IPv4 (re-derived each boot)."
  echo "node-ip: \"${NODE_IP}\""
  echo "advertise-address: \"${NODE_IP}\""
  echo "bind-address: \"${BIND_ADDRESS}\""
  echo ""
  echo "# Stable + current SANs so the apiserver cert survives an EC2 stop/start IP change."
  echo "tls-san:"
  for s in "${uniq_sans[@]}"; do echo "  - \"${s}\""; done
  echo ""
  echo "# Single-node datastore: SQLite/kine (k3s default; no embedded etcd)."
  echo "# Explicit so an operator never accidentally flips to etcd via --cluster-init."
  echo "# (kine state lives in /var/lib/rancher/k3s/server/db — backed up by"
  echo "#  scripts/k3s/k3s-datastore-backup.sh, NOT by k3s etcd-snapshot.)"
  if [ -n "$(echo "$DISABLE" | tr -d '[:space:]')" ]; then
    echo ""
    echo "# Disabled bundled components (operator opted out of the reuse win):"
    echo "disable:"
    for d in $DISABLE; do echo "  - \"${d}\""; done
  fi
} >"$K3S_CONFIG"

log "wrote $K3S_CONFIG"
