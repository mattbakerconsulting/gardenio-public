#!/usr/bin/env bash
# assemble-admin-kubeconfig.sh — build a kubeconfig from a cluster-admin BEARER TOKEN
# and the in-pod API server endpoint + CA, then (optionally) drive the discriminating
# restore check via run-drc-via-admin-kubeconfig.sh.
#
# The easiest credential delivery is the operator pasting/DMing the raw cluster-admin
# token (from Secret gardenio-cluster-admin-token, ka-0000005378). This pod already
# has the prod cluster's API server endpoint (KUBERNETES_SERVICE_HOST) and its CA
# (the projected SA ca.crt) — the in-pod API server IS the prod cluster API server.
# A cluster-admin token presented to it grants full cluster access incl. gvisor-poc
# pods/exec, regardless of this pod's own (sandboxed, minimal) SA.
#
# USAGE:
#   # token via env (preferred — never on argv/ps):
#   ADMIN_TOKEN=<token> ./assemble-admin-kubeconfig.sh            # writes kubeconfig, prints path
#   ADMIN_TOKEN=<token> ./assemble-admin-kubeconfig.sh --run      # ...and runs the check
#   # or from a file:
#   ./assemble-admin-kubeconfig.sh --token-file /tmp/tok --run
#
# OUT: /home/agent/.kube/drc-admin-config (mode 600). Pass --run to immediately invoke
# run-drc-via-admin-kubeconfig.sh --kubeconfig <that> (forwards --agent/--node).
set -uo pipefail

OUT="${OUT:-/home/agent/.kube/drc-admin-config}"
TOKEN="${ADMIN_TOKEN:-}"
TOKEN_FILE=""
RUN=0
PASS_ARGS=()

while [ $# -gt 0 ]; do
  case "$1" in
    --token-file) TOKEN_FILE="$2"; shift 2 ;;
    --out)        OUT="$2"; shift 2 ;;
    --run)        RUN=1; shift ;;
    --agent|--node|--ns) PASS_ARGS+=("$1" "$2"); shift 2 ;;
    -h|--help)    sed -n '2,30p' "$0"; exit 0 ;;
    *) echo "unknown flag: $1" >&2; exit 2 ;;
  esac
done

[ -z "$TOKEN" ] && [ -n "$TOKEN_FILE" ] && TOKEN="$(cat "$TOKEN_FILE")"
TOKEN="$(printf '%s' "$TOKEN" | tr -d '[:space:]')"
[ -z "$TOKEN" ] && { echo "SETUP-ERROR: no token (set \$ADMIN_TOKEN or --token-file)." >&2; exit 2; }

CA="/var/run/secrets/kubernetes.io/serviceaccount/ca.crt"
SERVER="https://${KUBERNETES_SERVICE_HOST:-172.20.0.1}:${KUBERNETES_SERVICE_PORT_HTTPS:-443}"
[ -s "$CA" ] || { echo "SETUP-ERROR: in-pod CA missing at $CA" >&2; exit 2; }

mkdir -p "$(dirname "$OUT")"
( umask 077
  kubectl --kubeconfig="$OUT" config set-cluster prod \
    --server="$SERVER" --certificate-authority="$CA" --embed-certs=true >/dev/null
  kubectl --kubeconfig="$OUT" config set-credentials admin --token="$TOKEN" >/dev/null
  kubectl --kubeconfig="$OUT" config set-context prod --cluster=prod --user=admin >/dev/null
  kubectl --kubeconfig="$OUT" config use-context prod >/dev/null
)
chmod 600 "$OUT"
echo "wrote kubeconfig: $OUT (server=$SERVER)"

# Verify the token actually authenticates + has the exec right before declaring ready.
who="$(kubectl --kubeconfig="$OUT" auth whoami 2>&1 | awk '/Username/{print $2}')"
if [ -z "$who" ]; then
  echo "WARN: token did not authenticate (auth whoami failed). Check the token is the" >&2
  echo "  cluster-admin token (base64-decoded) from Secret gardenio-cluster-admin-token." >&2
  kubectl --kubeconfig="$OUT" auth whoami 2>&1 | head -4 >&2
  exit 1
fi
echo "authenticated as: $who"
if kubectl --kubeconfig="$OUT" auth can-i create pods/exec -n "${NS:-gvisor-poc}" >/dev/null 2>&1; then
  echo "exec-in-gvisor-poc: YES — ready to run the check."
else
  echo "exec-in-gvisor-poc: NO — token authenticates but lacks pods/exec in gvisor-poc (not cluster-admin?)." >&2
fi

if [ "$RUN" = 1 ]; then
  here="$(cd "$(dirname "$0")" && pwd)"
  echo "=== running discriminating restore check via remote exec ==="
  exec "$here/run-drc-via-admin-kubeconfig.sh" --kubeconfig "$OUT" "${PASS_ARGS[@]}"
fi
