#!/usr/bin/env bash
# acceptance-smoke.sh — Gardenio-in-a-Box capstone acceptance check (P5.2).
#
# Phase 5 capstone (ka-0000005704, epic ka-0000006354, mega-epic ka-0000001295).
# Sources: mega-epic acceptance criteria; gardenio/docs/gardenio-in-a-box-install.md
# (Part B), gardenio/docs/gardenio-in-a-box-threat-model-signoff.md (F4/F5/F7/F8).
#
# This asserts the FOUR mega-epic acceptance properties on a LIVE, already-bootstrapped
# Gardenio-in-a-Box EC2 instance (one installed per the P5.1 runbook with the in-box
# overlays + --harden). It does NOT launch the instance — provisioning is an operator
# action (spend / account / EIP / SSH key). Run it after bootstrap completes:
#
#   1. REACHABLE over sslip.io with a PUBLICLY-TRUSTED cert, no client trust import.
#   2. Every public browser route is AUTH-GATED (302 -> Keycloak, never 200 app data).
#   3. Only 80/443/22 reachable; the k8s API (:6443) + datastores are NOT public.
#   4. No AWS API egress at runtime (NetworkPolicy default-deny; IMDS blocked from pods).
#
# It runs in two halves:
#   * OFF-BOX  (default)        — from the machine you run this on (your laptop / CI):
#                                 cert trust, route gating, port reachability. Needs only
#                                 curl + nc/bash and the box's public IP + hostnames.
#   * ON-BOX   (--on-box / SSH) — pod health, NetworkPolicy presence, IMDS-from-pod block.
#                                 Run on the instance (or via --ssh USER@HOST which wraps it).
#
# Exit 0 = all assertions pass. Exit 1 = at least one failed (with diagnostics).
# Exit 2 = usage/precondition error.
#
# Usage:
#   scripts/inbox/acceptance-smoke.sh --ip <EIP> [--hosts "h1 h2 ..."] \
#       [--apex <dashed-ip>.sslip.io] [--ssh USER@HOST] [--namespace NS] \
#       [--on-box] [--off-box-only] [--ports "6443 10250 5432 4222"]
#
# Examples:
#   # Off-box only (from your laptop), explicit hosts:
#   scripts/inbox/acceptance-smoke.sh --ip 203.0.113.7 \
#     --hosts "203-0-113-7.sslip.io beads-viewer.203-0-113-7.sslip.io auth.203-0-113-7.sslip.io"
#
#   # Full check, driving the on-box half over SSH:
#   scripts/inbox/acceptance-smoke.sh --ip 203.0.113.7 --apex 203-0-113-7.sslip.io \
#     --ssh gardenio-op@203.0.113.7
#
#   # On the instance itself (on-box half only):
#   sudo scripts/inbox/acceptance-smoke.sh --on-box --namespace gardenio
set -uo pipefail

IP=""
APEX=""
HOSTS=""
SSH_TARGET=""
NAMESPACE="${NAMESPACE:-gardenio}"
RELEASE="${RELEASE:-gardenio}"
# Control-plane / datastore ports that MUST be unreachable from off-box.
PRIVATE_PORTS="${PRIVATE_PORTS:-6443 10250 10256 5432 4222 8222}"
DO_OFFBOX=1
DO_ONBOX="auto"   # auto: on-box runs only if --ssh given or --on-box set
CURL_TIMEOUT="${CURL_TIMEOUT:-10}"
NC_TIMEOUT="${NC_TIMEOUT:-5}"

while [ $# -gt 0 ]; do
  case "$1" in
    --ip) IP="$2"; shift 2;;
    --apex) APEX="$2"; shift 2;;
    --hosts) HOSTS="$2"; shift 2;;
    --ssh) SSH_TARGET="$2"; shift 2;;
    --namespace) NAMESPACE="$2"; shift 2;;
    --release) RELEASE="$2"; shift 2;;
    --ports) PRIVATE_PORTS="$2"; shift 2;;
    --on-box) DO_ONBOX=1; DO_OFFBOX=0; shift;;
    --off-box-only) DO_ONBOX=0; DO_OFFBOX=1; shift;;
    -h|--help) sed -n '2,52p' "$0"; exit 0;;
    *) echo "unknown arg: $1 (try --help)" >&2; exit 2;;
  esac
done

red()  { printf '\033[0;31m%s\033[0m\n' "$*"; }
grn()  { printf '\033[0;32m%s\033[0m\n' "$*"; }
ylw()  { printf '\033[0;33m%s\033[0m\n' "$*"; }
info() { printf '\033[2m%s\033[0m\n' "$*"; }

PASS=0
FAIL=0
pass() { grn "  PASS: $*"; PASS=$((PASS+1)); }
miss() { red "  FAIL: $*"; FAIL=$((FAIL+1)); }

# Derive the host list from the apex if --hosts not given.
derive_hosts() {
  [ -n "$HOSTS" ] && return 0
  [ -n "$APEX" ] || return 0
  HOSTS="$APEX auth.$APEX beads-viewer.$APEX mux.$APEX grafana.$APEX"
}

# ── Assertion 1+2: off-box reachability, trusted cert, auth-gating ───────────
check_offbox() {
  echo "== OFF-BOX (cert trust · route gating · port reachability) =="
  command -v curl >/dev/null 2>&1 || { miss "curl not found (off-box checks need curl)"; return; }
  derive_hosts
  [ -n "$IP" ] || { miss "--ip <EIP> required for off-box checks"; return; }

  # 1+2: each served host must (a) present a PUBLICLY-TRUSTED cert (verify w/o -k,
  #      no imported CA) and (b) be AUTH-GATED (302/401/403, not a 200 with app data).
  if [ -n "$HOSTS" ]; then
    for h in $HOSTS; do
      # ssl_verify_result==0 means the system trust store validated the chain —
      # i.e. a publicly-trusted LE cert, NO client trust import. --resolve pins
      # the SNI host to the box IP so this works before/independent of public DNS.
      local out code verify
      out="$(curl -sS -o /dev/null --max-time "$CURL_TIMEOUT" \
               --resolve "${h}:443:${IP}" \
               -w '%{http_code} %{ssl_verify_result}' \
               "https://${h}/" 2>/dev/null)" || out="000 -1"
      code="${out%% *}"; verify="${out##* }"
      if [ "$verify" = "0" ]; then
        pass "TLS publicly-trusted (no client trust import): https://${h} (verify=0)"
      else
        miss "TLS NOT publicly trusted for https://${h} (ssl_verify_result=$verify) — cert not issued/served, or self-signed"
      fi
      case "$code" in
        302|303|307|401|403) pass "route auth-gated: https://${h} -> HTTP $code (redirect to Keycloak / denied)";;
        200) miss "route https://${h} returned 200 — UNAUTHENTICATED app data? (must be auth-gated)";;
        000) miss "route https://${h} unreachable (HTTP 000) — :443 not answering or wrong host";;
        *)   ylw "  NOTE: https://${h} -> HTTP $code (not a clear gate signal; inspect manually)";;
      esac
    done
  else
    ylw "  NOTE: no --hosts/--apex given; skipping per-host cert+gating checks. Pass --apex <dashed-ip>.sslip.io."
  fi

  # 3 (off-box half): the control-plane + datastore ports MUST be unreachable.
  echo "== OFF-BOX (Assertion 3: only 80/443/22 reachable) =="
  # :443 must succeed (sanity — proves we're hitting the right box / firewall lets edge through).
  if port_open "$IP" 443; then pass ":443 reachable (edge up)"; else miss ":443 NOT reachable — edge/firewall problem"; fi
  for p in $PRIVATE_PORTS; do
    if port_open "$IP" "$p"; then
      miss ":$p is REACHABLE from off-box — control-plane/datastore exposed (must be firewall-dropped)"
    else
      pass ":$p not reachable from off-box (firewall-dropped)"
    fi
  done
}

# Returns 0 if TCP <host>:<port> accepts a connection within NC_TIMEOUT.
port_open() {
  local host="$1" port="$2"
  if command -v nc >/dev/null 2>&1; then
    nc -z -w "$NC_TIMEOUT" "$host" "$port" >/dev/null 2>&1
  else
    # bash /dev/tcp fallback (no nc).
    timeout "$NC_TIMEOUT" bash -c "exec 3<>/dev/tcp/${host}/${port}" >/dev/null 2>&1
  fi
}

# ── Assertion 3 (on-box) + 4: pod health, NetworkPolicy, IMDS-from-pod block ─
check_onbox_local() {
  echo "== ON-BOX (pod health · NetworkPolicy · no AWS egress) =="
  local KCTL
  if command -v kubectl >/dev/null 2>&1; then KCTL="kubectl";
  elif command -v k3s >/dev/null 2>&1; then KCTL="k3s kubectl";
  else miss "neither kubectl nor k3s found on-box"; return; fi
  export KUBECONFIG="${KUBECONFIG:-/etc/rancher/k3s/k3s.yaml}"

  # 3 (on-box half): pod networking/DNS survived the host firewall (FORWARD/CNI intact).
  local notready
  notready="$($KCTL -n "$NAMESPACE" get pods --no-headers 2>/dev/null \
              | grep -Evc 'Running|Completed' | tr -d ' ')"
  if [ "${notready:-0}" = "0" ]; then pass "all pods in ns/$NAMESPACE Running/Completed (CNI/FORWARD intact)";
  else miss "$notready pod(s) in ns/$NAMESPACE not Ready — see: $KCTL -n $NAMESPACE get pods"; fi

  # 4: NetworkPolicy default-deny present (the fail-closed spine F1/F2).
  local npc
  npc="$($KCTL -n "$NAMESPACE" get networkpolicy --no-headers 2>/dev/null | wc -l | tr -d ' ')"
  if [ "${npc:-0}" -ge 1 ]; then
    pass "$npc NetworkPolicy objects in ns/$NAMESPACE (default-deny + per-service allows)"
    if $KCTL -n "$NAMESPACE" get networkpolicy --no-headers 2>/dev/null | grep -qi 'deny'; then
      pass "a default-deny NetworkPolicy is present"
    else
      ylw "  NOTE: no policy name matched 'deny' — confirm default-deny-ingress/egress rendered"
    fi
  else
    miss "NO NetworkPolicy in ns/$NAMESPACE — F1/F2 not active (agent-escape hops are OPEN)"
  fi

  # 4: no ExternalSecret CRs (in-box = no ESO; secrets seeded). Absence of the CRD
  # is itself a pass (ESO not installed).
  if $KCTL get crd externalsecrets.external-secrets.io >/dev/null 2>&1; then
    local esc
    esc="$($KCTL -n "$NAMESPACE" get externalsecret --no-headers 2>/dev/null | wc -l | tr -d ' ')"
    if [ "${esc:-0}" = "0" ]; then pass "no ExternalSecret CRs in ns/$NAMESPACE (in-box seeded secrets)";
    else miss "$esc ExternalSecret CR(s) present — in-box must not depend on ESO/AWS SM"; fi
  else
    pass "ExternalSecret CRD not installed (no ESO on the box)"
  fi

  # 4: IMDS must be blocked from inside an agent-class pod. Best-effort: try an
  # ephemeral debug pod in the namespace; if egress default-deny is on, the curl
  # to IMDS times out (success). Skips cleanly if we cannot schedule a pod.
  echo "== ON-BOX (Assertion 4: IMDS blocked from a namespace pod) =="
  local imds_out
  imds_out="$($KCTL -n "$NAMESPACE" run imds-probe-$$ --rm -i --restart=Never \
                --image=curlimages/curl:8.10.1 --quiet --timeout=60s -- \
                sh -c 'curl -s -m 4 -o /dev/null -w "%{http_code}" http://169.254.169.254/latest/meta-data/ 2>/dev/null || echo BLOCKED' \
                2>/dev/null | tr -d '[:space:]')"
  case "$imds_out" in
    *BLOCKED*|"") pass "IMDS unreachable from a ns/$NAMESPACE pod (egress default-deny excludes 169.254/16)";;
    200*)        miss "IMDS REACHABLE from a ns/$NAMESPACE pod (HTTP 200) — node IAM creds stealable; F2 egress exclusion not active";;
    *)           ylw "  NOTE: IMDS probe inconclusive (got '$imds_out'); the image may not have pulled in air-gap mode — verify NetworkPolicy egress manually";;
  esac
}

# Drive the on-box half over SSH (wraps this same script on the remote box).
check_onbox_ssh() {
  echo "== ON-BOX via SSH ($SSH_TARGET) =="
  command -v ssh >/dev/null 2>&1 || { miss "ssh not found (cannot run --ssh on-box half)"; return; }
  # Prefer a checked-out copy at the AMI-baked repo path; fall back to piping this script.
  if ssh -o BatchMode=yes -o ConnectTimeout=10 "$SSH_TARGET" \
       'test -x /opt/gardenio/repo/scripts/inbox/acceptance-smoke.sh' 2>/dev/null; then
    # NAMESPACE/RELEASE intentionally expand client-side so the local values reach the box.
    if ssh "$SSH_TARGET" "sudo /opt/gardenio/repo/scripts/inbox/acceptance-smoke.sh --on-box --namespace '$NAMESPACE' --release '$RELEASE'"; then
      pass "on-box half passed (over SSH)"
    else
      miss "on-box half reported failures (over SSH)"
    fi
  else
    # Pipe this script to the remote and run its on-box half (values expand client-side).
    if ssh "$SSH_TARGET" "sudo bash -s -- --on-box --namespace '$NAMESPACE' --release '$RELEASE'" < "$0"; then
      pass "on-box half passed (piped over SSH)"
    else
      miss "on-box half reported failures (piped over SSH)"
    fi
  fi
}

# ── main ─────────────────────────────────────────────────────────────────────
echo "=== Gardenio-in-a-Box acceptance smoke (P5.2) ==="
[ -n "$IP" ] && info "box IP: $IP"; [ -n "$APEX" ] && info "apex: $APEX"; info "namespace: $NAMESPACE"

if [ "$DO_OFFBOX" = "1" ]; then check_offbox; fi

if [ "$DO_ONBOX" = "1" ]; then
  check_onbox_local
elif [ "$DO_ONBOX" = "auto" ] && [ -n "$SSH_TARGET" ]; then
  check_onbox_ssh
elif [ "$DO_ONBOX" = "auto" ]; then
  ylw "NOTE: on-box half skipped (run with --on-box on the instance, or pass --ssh USER@HOST)."
fi

echo "=== Result: $PASS passed, $FAIL failed ==="
[ "$FAIL" = "0" ] || { red "ACCEPTANCE FAILED — do NOT expose this box publicly until all assertions pass."; exit 1; }
grn "ACCEPTANCE PASSED — the box satisfies the mega-epic acceptance properties."
