#!/usr/bin/env bash
# lib-retry.sh — shared retry helpers for gardenio E2E tests.
#
# kubectl port-forward can briefly drop mid-module, which makes otherwise
# stable kd / curl calls fail with connection refused or empty output. The
# outer harness (run-shard.sh) probes /v1/health and
# restarts the tunnel BETWEEN modules, but a drop WITHIN a module still
# bubbles out as a hard failure. These helpers absorb the single-retry
# transient and let the harness's between-module restart recover the rest.
#
# Usage:
#   # shellcheck disable=SC1090
#   source "$(dirname "${BASH_SOURCE[0]}")/lib-retry.sh"
#
#   kd_retry "$KD_BIN" list --status=open --json
#   curl_retry -sf "$BEADS_HTTP_URL/v1/health"
#   val=$(retry_on_empty "$KD_BIN" show "$ID" --json)
#
# Tunables (override via env before sourcing, or per-call):
#   RETRY_ATTEMPTS   total attempts incl. first try (default 3)
#   RETRY_DELAY      seconds between attempts (default 2)

# Guard against multiple sourcing within the same shell.
# shellcheck disable=SC2317
if [ -n "${__GARDENIO_LIB_RETRY_LOADED:-}" ]; then
  return 0 2>/dev/null || true
fi
__GARDENIO_LIB_RETRY_LOADED=1

: "${RETRY_ATTEMPTS:=3}"
: "${RETRY_DELAY:=2}"

# retry_cmd — run a command; on failure retry up to RETRY_ATTEMPTS times
# with RETRY_DELAY between attempts. Returns the final exit code.
#
# Stdout/stderr pass through. If an early attempt writes partial stdout
# and a later one succeeds, both are emitted — this matches `kd`'s actual
# behaviour (on a PF drop it writes nothing to stdout; its error text
# goes to stderr and is typically redirected by the caller).
retry_cmd() {
  local attempts="${RETRY_ATTEMPTS:-3}"
  local delay="${RETRY_DELAY:-2}"
  local i rc=0
  for (( i = 1; i <= attempts; i++ )); do
    "$@"
    rc=$?
    if [ "$rc" -eq 0 ]; then
      return 0
    fi
    if [ "$i" -lt "$attempts" ]; then
      sleep "$delay"
    fi
  done
  return "$rc"
}

# kd_retry — retry wrapper for the kd CLI.
#
# Usage: kd_retry "$KD_BIN" <subcommand> [args...]
#
# Drop-in for bare "$KD_BIN" invocations whose failure on a port-forward
# drop shouldn't kill the whole module.
kd_retry() {
  retry_cmd "$@"
}

# curl_retry — retry wrapper for curl. Combines our shell-level retry
# with curl's own --retry so we catch both "tunnel gone at connect" and
# "tunnel half-dead mid-request".
#
# Usage: curl_retry -sf "$URL" [curl-args...]
curl_retry() {
  retry_cmd curl --retry 2 --retry-delay 1 --max-time 10 "$@"
}

# retry_on_empty — run a command; if stdout is empty, wait and retry.
# Useful for queries whose "success" signal is non-empty output (gate
# status, filtered list, etc.). Does NOT inspect the exit code.
#
# Usage: val=$(retry_on_empty "$KD_BIN" show "$ID" --json)
retry_on_empty() {
  local attempts="${RETRY_ATTEMPTS:-3}"
  local delay="${RETRY_DELAY:-2}"
  local i output=""
  for (( i = 1; i <= attempts; i++ )); do
    output=$("$@" 2>/dev/null || true)
    if [ -n "$output" ]; then
      printf '%s' "$output"
      return 0
    fi
    if [ "$i" -lt "$attempts" ]; then
      sleep "$delay"
    fi
  done
  printf '%s' "$output"
}
