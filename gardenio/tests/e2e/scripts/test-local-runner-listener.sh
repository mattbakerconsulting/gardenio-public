#!/usr/bin/env bash
# test-local-runner-listener.sh — the public runner listener serves only the
# runner allowlist and refuses the shared token (local-agents spec §4.3, §11 f;
# plan gd-fwFefkDP4H Task 18, gd-jW7KJ7ZdNy).
#
# Two layers, each negative paired with a positive control:
#   daemon  (BEADS_HTTP_URL) a request stamped X-Gardenio-Listener: runner with
#           a non-runner bearer is 401 (runner_auth.go); the same request
#           without the header is 200. Runs wherever the daemon is reachable.
#   ingress (RUNNER_PUBLIC_URL) the chart's runner IngressRoutes
#           (beads.ingress.runnerRoute.enabled=true, or the k3s public edge)
#           route only kbeads runnerAllowedPatterns, and only for a gr_
#           bearer, plus POST /v1/runners/register. Everything else is
#           Traefik's 404. Without RUNNER_PUBLIC_URL this layer is one skip.
#
# The ingress layer registers a runner through the public URL (needs an admin
# token for the invite), or uses E2E_RUNNER_HOME, a GARDENIO_HOME that already
# holds runner.json and runner.token. Tokens go to curl through a 0600 header
# file, never argv, and are never printed.
#
# Usage:
#   NAMESPACE=gardenio-e2e RUNNER_PUBLIC_URL=https://runners.example.com ./test-local-runner-listener.sh

set -euo pipefail

# shellcheck source=SCRIPTDIR/lib-local-runner.sh
source "$(dirname "${BASH_SOURCE[0]}")/lib-local-runner.sh"

HDR_DIR=""

# bearer_file <name> <token> — a curl header file carrying the token.
bearer_file() {
  local f="$HDR_DIR/$1.hdr"
  ( umask 077; printf 'Authorization: Bearer %s\n' "$2" >"$f" )
  printf '%s' "$f"
}

# code [curl args...] — HTTP status only.
code() { curl -s -o /dev/null -w '%{http_code}' --max-time 15 "$@" || true; }

# body [curl args...] — response body only.
body() { curl -s --max-time 15 "$@" || true; }

listener_cleanup() {
  [ -n "$HDR_DIR" ] && rm -rf "$HDR_DIR"
  lr_cleanup
}

main() {
  lr_init "Public runner listener: allowlist only, no shared token (spec §11 f)" curl
  trap listener_cleanup EXIT
  HDR_DIR="$(mktemp -d -t lr-hdr-XXXXXX)"
  chmod 700 "$HDR_DIR"

  local shared="${BEADS_AUTH_TOKEN:-${BEADS_TOKEN:-}}" shared_hdr="" c
  if [ -n "$shared" ]; then
    shared_hdr="$(bearer_file shared "$shared")"
  fi
  unset shared

  # ── daemon layer ────────────────────────────────────────────────
  blue "(f) daemon: the listener header refuses a non-runner bearer"
  if [ -z "$shared_hdr" ]; then
    skip "daemon refuses a non-runner bearer on the listener" "no BEADS_AUTH_TOKEN/BEADS_TOKEN"
  else
    c="$(code -H "@$shared_hdr" "$BEADS_HTTP_URL/v1/beads?limit=1")"
    if [ "$c" = 200 ]; then
      pass "the shared token reads /v1/beads on the cluster listener (control)"
    else
      fail "the shared token reads /v1/beads on the cluster listener (control)" "HTTP $c"
    fi
    c="$(code -H "@$shared_hdr" -H 'X-Gardenio-Listener: runner' "$BEADS_HTTP_URL/v1/beads?limit=1")"
    if [ "$c" = 401 ]; then
      pass "the same request stamped X-Gardenio-Listener: runner is 401"
    else
      fail "the same request stamped X-Gardenio-Listener: runner is 401" "HTTP $c"
    fi
  fi

  # ── ingress layer ───────────────────────────────────────────────
  if [ -z "${RUNNER_PUBLIC_URL:-}" ]; then
    skip "(f) public listener routes" "RUNNER_PUBLIC_URL unset; needs an install with the runner route"
    lr_summary
  fi
  local pub="${RUNNER_PUBLIC_URL%/}"
  blue "(f) ingress: $pub"

  # register is the one bearer-less route: a bad invite reaches the daemon,
  # which answers 404 "invite not found" (not Traefik's bare 404).
  local reg_body
  reg_body="$(body -X POST -H 'Content-Type: application/json' \
    -d '{"invite_code":"GRI-NOPE-NOPE","hostname":"e2e","os_arch":"linux/amd64"}' \
    -w '\n%{http_code}' "$pub/v1/runners/register")"
  if [ "${reg_body##*$'\n'}" = 404 ] && grep -q 'invite not found' <<<"$reg_body"; then
    pass "register is routed: a bad invite is the daemon's 404 invite not found"
  else
    fail "register is routed: a bad invite is the daemon's 404 invite not found" "$(printf '%s' "$reg_body" | tail -c 300)"
  fi

  local home="${E2E_RUNNER_HOME:-}" rc=0
  if [ -z "$home" ]; then
    lr_register_runner "$pub" || rc=$?
    home="$RUNNER_HOME"
  else
    RUNNER_ID="$(jq -r '.runner_id // empty' "$home/runner.json" 2>/dev/null || true)"
  fi
  local runner_hdr=""
  if [ "$rc" -eq 0 ] && [ -n "$RUNNER_ID" ] && [ -r "$home/runner.token" ]; then
    runner_hdr="$(bearer_file runner "$(cat "$home/runner.token")")"
  fi
  local fake_hdr
  fake_hdr="$(bearer_file fake "gr_e2e-not-a-real-token")"

  if [ -z "$runner_hdr" ]; then
    skip "a real runner token on the public listener" "no runner: set E2E_RUNNER_HOME or run with an admin token"
  else
    c="$(code -H "@$runner_hdr" "$pub/v1/runners/$RUNNER_ID")"
    if [ "$c" = 200 ]; then
      pass "a real runner token reads its own runner (control)"
    else
      fail "a real runner token reads its own runner (control)" "HTTP $c"
    fi
    c="$(code -H "@$runner_hdr" "$pub/v1/admin/operators")"
    if [ "$c" = 404 ]; then
      pass "a real runner token gets no route to /v1/admin"
    else
      fail "a real runner token gets no route to /v1/admin" "HTTP $c"
    fi
    c="$(code -H "@$runner_hdr" -X POST "$pub/v1/escalations/gd-e2enope/approve")"
    if [ "$c" = 404 ]; then
      pass "a real runner token gets no route to escalation approve"
    else
      fail "a real runner token gets no route to escalation approve" "HTTP $c"
    fi
    if [ -n "$shared_hdr" ]; then
      c="$(code -H "@$shared_hdr" "$pub/v1/runners/$RUNNER_ID")"
      if [ "$c" = 404 ]; then
        pass "the shared token gets no route on the public listener"
      else
        fail "the shared token gets no route on the public listener" "HTTP $c"
      fi
    fi
  fi
  c="$(code "$pub/v1/beads")"
  if [ "$c" = 404 ]; then
    pass "no bearer gets no route to /v1/beads"
  else
    fail "no bearer gets no route to /v1/beads" "HTTP $c"
  fi
  c="$(code -H "@$fake_hdr" "$pub/v1/beads")"
  if [ "$c" = 401 ]; then
    pass "an unknown gr_ token is routed and the daemon answers 401"
  else
    fail "an unknown gr_ token is routed and the daemon answers 401" "HTTP $c"
  fi
  if [ -n "$shared_hdr" ]; then
    c="$(code -H "@$shared_hdr" "$pub/v1/beads")"
    if [ "$c" = 404 ]; then
      pass "the shared token gets no route to /v1/beads"
    else
      fail "the shared token gets no route to /v1/beads" "HTTP $c"
    fi
  else
    skip "the shared token on the public listener" "no BEADS_AUTH_TOKEN/BEADS_TOKEN"
  fi

  lr_summary
}

main "$@"
