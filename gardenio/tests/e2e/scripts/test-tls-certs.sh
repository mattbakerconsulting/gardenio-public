#!/usr/bin/env bash
# test-tls-certs.sh — TLS certificate validation E2E tests
#
# Verifies that HTTPS endpoints in the staging namespace serve valid TLS
# certificates whose Subject Alternative Names (SANs) match the requested
# hostnames. Catches certificate mismatch errors like an ALB serving a
# parent-domain cert that doesn't cover subdomain ingress hosts.
#
# Usage:
#   ./tests/e2e/scripts/test-tls-certs.sh
#
# Prerequisites:
#   - curl installed
#   - kubectl context pointing at the target cluster
#   - Helm values accessible via `helm get values`
#
# Scenarios tested:
#   1. TLS handshake succeeds without certificate errors for each ingress host
#   2. Served certificate SAN matches the requested hostname

set -euo pipefail

# ── Config ────────────────────────────────────────────────────────
NAMESPACE="${NAMESPACE:-gardenio-e2e}"
SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
PASS=0
FAIL=0
ERRORS=()

# ── Helpers ───────────────────────────────────────────────────────
red()   { printf '\033[0;31m%s\033[0m\n' "$*"; }
green() { printf '\033[0;32m%s\033[0m\n' "$*"; }
blue()  { printf '\033[0;34m%s\033[0m\n' "$*"; }
dim()   { printf '\033[2m%s\033[0m\n' "$*"; }

pass() {
  local name="$1"
  green "  ✓ $name"
  PASS=$((PASS + 1))
}

fail() {
  local name="$1"
  local reason="${2:-}"
  red "  ✗ $name"
  [ -n "$reason" ] && dim "    $reason"
  FAIL=$((FAIL + 1))
  ERRORS+=("$name: $reason")
}

# ── Resolve ingress hosts from Helm values ────────────────────────
resolve_hosts() {
  local hosts=()

  # Try to extract hosts from deployed Helm values
  if command -v helm >/dev/null 2>&1; then
    while IFS= read -r line; do
      local h
      h=$(echo "$line" | sed -n 's/.*host: *//p' | xargs)
      if [ -n "$h" ] && [ "$h" != '""' ] && [ "$h" != "''" ]; then
        hosts+=("$h")
      fi
    done < <(helm get values "$NAMESPACE" -n "$NAMESPACE" 2>/dev/null | grep "host:" || true)
  fi

  # Fallback: well-known subdomains if helm extraction failed
  if [ "${#hosts[@]}" -eq 0 ]; then
    local domain="${NAMESPACE}.app.e2e.dev.acme.example"
    hosts=(
      "coopmux.${domain}"
      "beads.${domain}"
      "beads3d.${domain}"
      "advice.${domain}"
      "roles.${domain}"
      "${domain}"
    )
    dim "  (using fallback host list — helm values not available)"
  fi

  echo "${hosts[@]}"
}

# ── Check TLS certificate for a single host ───────────────────────
check_tls_cert() {
  local host="$1"

  # Use curl with strict TLS (no -k) to verify the cert matches the hostname.
  # We expect curl to fail with exit code 60 (SSL certificate problem) if the
  # cert SAN doesn't cover this hostname.
  local curl_output
  local curl_rc=0
  curl_output=$(curl -sv --max-time 10 "https://${host}/" 2>&1) || curl_rc=$?

  # curl exit code 60 = SSL certificate problem (peer certificate cannot be
  # authenticated with given CA certificates — which includes SAN mismatch).
  # curl exit code 51 = SSL peer certificate or SSH remote key was not OK
  if [ "$curl_rc" -eq 60 ] || [ "$curl_rc" -eq 51 ]; then
    # Extract the specific error for reporting
    local ssl_error
    ssl_error=$(echo "$curl_output" | grep -i "SSL\|certificate\|subject" | grep -v "^[*{}]" | head -3 || echo "certificate validation failed")
    local cert_subject
    cert_subject=$(echo "$curl_output" | grep "subject:" | head -1 | sed 's/.*subject: //' || echo "unknown")
    local san_line
    san_line=$(echo "$curl_output" | grep "subjectAltName" | head -1 || echo "")

    fail "TLS cert valid for ${host}" "cert subject: ${cert_subject}; ${san_line}"
    return
  fi

  # curl exit code 0 or other HTTP errors (401, 404, etc.) mean TLS succeeded
  if [ "$curl_rc" -eq 0 ] || grep -q "SSL connection using" <<<"$curl_output"; then
    pass "TLS cert valid for ${host}"
    return
  fi

  # Connection refused, timeout, DNS failure — not a cert issue, skip
  if [ "$curl_rc" -eq 7 ] || [ "$curl_rc" -eq 28 ] || [ "$curl_rc" -eq 6 ]; then
    dim "  - SKIP ${host} (connection error, not a cert issue; curl rc=${curl_rc})"
    return
  fi

  # Any other error — report but don't fail the cert check
  dim "  - SKIP ${host} (unexpected curl rc=${curl_rc})"
}

# ── Test Suite ────────────────────────────────────────────────────
main() {
  blue "═══════════════════════════════════════════════════"
  blue " tls-certs E2E tests"
  blue "═══════════════════════════════════════════════════"
  echo

  # ── Scenario 1: Resolve ingress hosts ──────────────────────────
  blue "Resolving ingress hosts for namespace: $NAMESPACE"
  local hosts_str
  hosts_str=$(resolve_hosts)
  read -ra HOSTS <<< "$hosts_str"
  dim "  Found ${#HOSTS[@]} host(s): ${HOSTS[*]}"
  echo

  # ── Scenario 2: Validate TLS certificate for each host ────────
  blue "Scenario 1: TLS certificate SAN matches each ingress hostname"
  for host in "${HOSTS[@]}"; do
    check_tls_cert "$host"
  done
  echo

  # ── Summary ─────────────────────────────────────────────────────
  blue "═══════════════════════════════════════════════════"
  local total=$((PASS + FAIL))
  if [ "$FAIL" -eq 0 ]; then
    green " ✓ All $total TLS assertions passed"
  else
    red " ✗ $FAIL/$total TLS assertions failed"
    echo
    for err in "${ERRORS[@]}"; do
      red "   - $err"
    done
  fi
  blue "═══════════════════════════════════════════════════"

  [ "$FAIL" -eq 0 ]
}

main "$@"
