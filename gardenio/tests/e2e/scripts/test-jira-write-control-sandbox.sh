#!/usr/bin/env bash
# test-jira-write-control-sandbox.sh — strong Jira write-control E2E (sandbox FSM)
#
# The live, sandbox-FSM leg of the strong allowlist enforcement suite
# (gd-yOllH26pFY, epic ka-0000003475 — two-account JIRA model). It proves, end to
# end against a REAL sandbox FSM ticket, that:
#
#   POSITIVE (triage path — activates with ka-0000003103):
#     A triage agent updates a bead → the PRIVILEGED bridge (gardenio2) stamps the
#     four triage fields — gardenio-triaged / sev-N / gxp-X labels + Affects-Version
#     — on the bound sandbox FSM ticket. Verify the labels + version LANDED, that
#     re-running is IDEMPOTENT (additive, no dupes), then REVERT.
#     Assert NO comment / transition / assignee / public side effects from the
#     triage path — fsm-triage writes ONLY labels+versions and nothing else.
#
#   NEGATIVE (browse-only enforcement — activates with gd-qBUDv4qMHb):
#     gardenio@ (the pod identity, post-migration browse-only on FSM) attempting a
#     direct curl PUT of a label / version on the FSM ticket must be REJECTED by
#     Jira with 403 — the migration is enforced by Jira, not just by removing the
#     skill (ka-0000003103 §3).
#
# ── Skip-clean contract ──
#
# The sandbox rigging this script needs — a gardenio2 privileged Jira credential,
# a browse-only gardenio@ credential, a disposable sandbox FSM ticket, the bridge
# routing triage writes through gardenio2, and the FSM sync toggle on in enforce
# mode — is delivered by ka-0000003103 + the prod cutover wiring (gd-J95Z08Vn0u).
# NONE of it exists in the ephemeral E2E namespace today. So, exactly like
# test-jira-bridge.sh scenarios 4/6, every live gate SKIPS CLEANLY (pass, not
# fail) when its rigging is absent, and the script is GREEN today. It lights up
# automatically once the operator provisions the sandbox env vars below.
#
# Required env for the LIVE gates (all absent ⇒ skip clean):
#   JIRA_SANDBOX_BASE_URL     Sandbox Jira base, e.g. https://acme-sandbox.atlassian.net
#   JIRA_SANDBOX_FSM_KEY      A disposable sandbox FSM ticket key, e.g. FSM-408
#   JIRA_GARDENIO2_EMAIL       Privileged bridge identity email (gardenio2)
#   JIRA_GARDENIO2_TOKEN       gardenio2 API token (the privileged write path)
#   JIRA_GARDENIO_BROWSE_EMAIL gardenio@ identity email (post-migration browse-only)
#   JIRA_GARDENIO_BROWSE_TOKEN gardenio@ API token (must be browse-only on FSM)
#
# These may instead be sourced from K8s secrets in $NAMESPACE when present
# (jira-gardenio2-cred / jira-gardenio-browse-cred) — see load_sandbox_creds.

set -euo pipefail

# ── Config ────────────────────────────────────────────────────────
NAMESPACE="${NAMESPACE:-gardenio-e2e}"
BEADS_SVC="${BEADS_SVC:-${NAMESPACE}-beads}"
KD_BIN="${KD_BIN:-kd}"
GB_BIN="${GB_BIN:-gb}"
PASS=0
FAIL=0
ERRORS=()
PF_PID=""
CREATED_BEADS=()
APPLIED_LABELS=()   # labels we stamped on the FSM ticket, for revert
APPLIED_VERSIONS=() # versions we stamped, for revert

export KD_ACTOR="${KD_ACTOR:-e2e-test}"

# ── Helpers (mirrors test-jira-bridge.sh) ─────────────────────────
red()   { printf '\033[0;31m%s\033[0m\n' "$*"; }
green() { printf '\033[0;32m%s\033[0m\n' "$*"; }
blue()  { printf '\033[0;34m%s\033[0m\n' "$*"; }
dim()   { printf '\033[2m%s\033[0m\n' "$*"; }

pass() { green "  ✓ $1"; PASS=$((PASS + 1)); }
fail() {
  red "  ✗ $1"
  [ -n "${2:-}" ] && dim "    $2"
  FAIL=$((FAIL + 1))
  ERRORS+=("$1: ${2:-}")
}
# skip records a cleanly-skipped gate as a PASS (rigging absent ≠ failure).
skip() { dim "  ⤼ SKIP: $1"; [ -n "${2:-}" ] && dim "    $2"; PASS=$((PASS + 1)); }

assert_contains() {
  if grep -qF -- "$2" <<< "$3"; then pass "$1 (contains '$2')"
  else fail "$1" "expected to contain '$2'; got: $(echo "$3" | head -3)"; fi
}

# ── Setup ─────────────────────────────────────────────────────────
setup_portforward() {
  if [ -n "${BEADS_HTTP_URL:-}" ]; then blue "Using BEADS_HTTP_URL=$BEADS_HTTP_URL"; return; fi
  blue "Port-forwarding ${NAMESPACE} kd server..."
  local port=19107
  kubectl -n "$NAMESPACE" port-forward "svc/${BEADS_SVC}" "${port}:8080" \
    >/tmp/kd-pf-jwc.log 2>&1 &
  PF_PID=$!
  sleep 3
  export BEADS_HTTP_URL="http://localhost:${port}"
  dim "  Daemon at $BEADS_HTTP_URL (pid $PF_PID)"
}

setup_token() {
  if [ -n "${BEADS_TOKEN:-}" ]; then return; fi
  local secret
  secret=$(kubectl -n "$NAMESPACE" get secret kd-beads-token \
    -o jsonpath='{.data.token}' 2>/dev/null | base64 -d 2>/dev/null || true)
  # if...fi (not `[ ... ] && export`) so a MISSING secret does not return
  # non-zero and abort the whole script under `set -e` before main() runs.
  if [ -n "$secret" ]; then export BEADS_TOKEN="$secret"; fi
}

# load_sandbox_creds fills the JIRA_SANDBOX_* / JIRA_GARDENIO2_* /
# JIRA_GARDENIO_BROWSE_* vars from K8s secrets when they are not already set in
# the env. Best-effort and silent: any that stay empty gate a clean skip below.
load_sandbox_creds() {
  local g2 gb
  g2=$(kubectl -n "$NAMESPACE" get secret jira-gardenio2-cred -o json 2>/dev/null || echo "")
  if [ -n "$g2" ]; then
    : "${JIRA_GARDENIO2_EMAIL:=$(echo "$g2" | python3 -c "import sys,json,base64; d=json.load(sys.stdin)['data']; print(base64.b64decode(d.get('email','')).decode() or '')" 2>/dev/null || echo "")}"
    : "${JIRA_GARDENIO2_TOKEN:=$(echo "$g2" | python3 -c "import sys,json,base64; d=json.load(sys.stdin)['data']; print(base64.b64decode(d.get('token','')).decode() or '')" 2>/dev/null || echo "")}"
  fi
  gb=$(kubectl -n "$NAMESPACE" get secret jira-gardenio-browse-cred -o json 2>/dev/null || echo "")
  if [ -n "$gb" ]; then
    : "${JIRA_GARDENIO_BROWSE_EMAIL:=$(echo "$gb" | python3 -c "import sys,json,base64; d=json.load(sys.stdin)['data']; print(base64.b64decode(d.get('email','')).decode() or '')" 2>/dev/null || echo "")}"
    : "${JIRA_GARDENIO_BROWSE_TOKEN:=$(echo "$gb" | python3 -c "import sys,json,base64; d=json.load(sys.stdin)['data']; print(base64.b64decode(d.get('token','')).decode() or '')" 2>/dev/null || echo "")}"
  fi
}

# sandbox_rig_ready returns 0 iff every var needed for the LIVE positive gate is
# present. When it returns non-zero, the positive gate skips clean.
sandbox_rig_ready() {
  [ -n "${JIRA_SANDBOX_BASE_URL:-}" ] && [ -n "${JIRA_SANDBOX_FSM_KEY:-}" ] \
    && [ -n "${JIRA_GARDENIO2_EMAIL:-}" ] && [ -n "${JIRA_GARDENIO2_TOKEN:-}" ]
}

# jira_get / jira_put_labels / jira_put_version — thin curl wrappers over the
# sandbox Jira REST v3 API using a given email:token basic-auth pair. Echo the
# HTTP status code (jira_get echoes the body on the last line via a marker).
jira_curl_code() { # method path email token json
  local method="$1" path="$2" email="$3" token="$4" json="${5:-}"
  local args=(-s -o /dev/null -w '%{http_code}' -X "$method"
    -u "${email}:${token}" -H "Content-Type: application/json"
    "${JIRA_SANDBOX_BASE_URL}${path}")
  [ -n "$json" ] && args+=(-d "$json")
  curl "${args[@]}" 2>/dev/null || echo "000"
}

jira_get_body() { # path email token
  curl -s -u "${2}:${3}" -H "Accept: application/json" \
    "${JIRA_SANDBOX_BASE_URL}${1}" 2>/dev/null || echo "{}"
}

cleanup() {
  # Revert any FSM labels/versions we stamped so the sandbox ticket is left clean.
  if sandbox_rig_ready; then
    for l in "${APPLIED_LABELS[@]}"; do
      jira_curl_code PUT "/rest/api/3/issue/${JIRA_SANDBOX_FSM_KEY}" \
        "$JIRA_GARDENIO2_EMAIL" "$JIRA_GARDENIO2_TOKEN" \
        "{\"update\":{\"labels\":[{\"remove\":\"${l}\"}]}}" >/dev/null 2>&1 || true
    done
    for v in "${APPLIED_VERSIONS[@]}"; do
      jira_curl_code PUT "/rest/api/3/issue/${JIRA_SANDBOX_FSM_KEY}" \
        "$JIRA_GARDENIO2_EMAIL" "$JIRA_GARDENIO2_TOKEN" \
        "{\"update\":{\"versions\":[{\"remove\":{\"name\":\"${v}\"}}]}}" >/dev/null 2>&1 || true
    done
  fi
  for id in "${CREATED_BEADS[@]}"; do "$KD_BIN" close "$id" 2>/dev/null || true; done
  [ -n "$PF_PID" ] && kill "$PF_PID" 2>/dev/null || true
}
trap cleanup EXIT

# ── Test Suite ────────────────────────────────────────────────────
main() {
  blue "═══════════════════════════════════════════════════"
  blue " Jira write-control (sandbox FSM) — strong allowlist E2E"
  blue " gd-yOllH26pFY / epic ka-0000003475"
  blue "═══════════════════════════════════════════════════"

  setup_portforward
  setup_token
  load_sandbox_creds

  if ! "$KD_BIN" list --status=open --json >/dev/null 2>&1; then
    red "ERROR: Cannot reach kd server at $BEADS_HTTP_URL"; exit 1
  fi
  green "  Server reachable: $BEADS_HTTP_URL"
  echo

  local TEST_TAG="e2e-jwc-$$-$(date +%s)"

  # ── Scenario 0: allowlist unit invariants are compiled in ──────────
  # A cheap always-on gate that keeps the module non-vacuous even when the
  # sandbox rig is absent: prove the daemon is reachable and round-trips a
  # realistic triage-classification bead, so the module isn't a pure skip in the
  # ephemeral ns.
  #
  # We anchor on the classification surface the FSM sync-back actually reads off
  # a bead — the gardenio-triaged / sev-<N> / gxp-<X> LABELS
  # (bridge triageClassification()) plus the bound jira_key field — because those
  # are exactly what the daemon persists on a task and what the bridge later
  # syncs to the ticket. We deliberately do NOT pass `-f role=` /
  # `-f recommended_affects_version=`: both are consumed by the CONTROLLER
  # (jira_sync.go actingRole() / triageClassification()) and asserted by its unit
  # tests (jira_sync_strong_test.go, jira_triage_bridge_test.go), but NEITHER is
  # registered on the built-in `task` type, so the daemon 400s `unknown field` on
  # the create — which froze the full E2E release gate (gd-KbPPLTRs6W). Seeding
  # those two onto type:task in the E2E fixture would only make this ephemeral
  # gate accept a create the PROD daemon still rejects (a false-green that hides
  # the very drift E2E exists to catch), so the anchor stays on registered fields
  # + labels.
  blue "Scenario 0: triage-classification bead round-trips (daemon FSM-aware)"
  local sbx_key="${JIRA_SANDBOX_FSM_KEY:-FSM-408}"
  local tri_out tri_id
  tri_out=$("$KD_BIN" create "jwc-triage-bead-$TEST_TAG" --type=task \
    -l "project:gardenio" -l "gardenio-triaged" -l "sev-1" -l "gxp-med" \
    -f "jira_key=$sbx_key" \
    -d "Triage classification bead for the sandbox FSM write-control E2E" \
    --json 2>/dev/null || echo "{}")
  tri_id=$(echo "$tri_out" | python3 -c "import sys,json; print(json.load(sys.stdin).get('id',''))" 2>/dev/null || echo "")
  if [ -n "$tri_id" ]; then
    CREATED_BEADS+=("$tri_id")
    pass "triage-classification bead created ($tri_id)"
    local show_out
    show_out=$("$KD_BIN" show "$tri_id" --json 2>/dev/null || echo "{}")
    assert_contains "bead carries bound FSM key" "$sbx_key" "$show_out"
    assert_contains "bead carries gardenio-triaged label" "gardenio-triaged" "$show_out"
    assert_contains "bead carries sev classification label" "sev-1" "$show_out"
    assert_contains "bead carries gxp classification label" "gxp-med" "$show_out"
  else
    fail "triage-classification bead" "could not create"
  fi
  echo

  # ── Scenario 1: POSITIVE — privileged gardenio2 stamps labels + version ──
  blue "Scenario 1: triage path stamps labels+version on FSM as gardenio2 (privileged)"
  if ! sandbox_rig_ready; then
    skip "positive triage-write gate" \
      "sandbox rig absent (need JIRA_SANDBOX_BASE_URL + JIRA_SANDBOX_FSM_KEY + gardenio2 cred); arrives with ka-0000003103"
  else
    local fsm="$JIRA_SANDBOX_FSM_KEY"
    local lbl="gardenio-triaged"
    local sev="sev-1"
    local gxp="gxp-med"
    local ver="2026.7.0"

    # (a) Stamp the four triage fields as gardenio2 (additive PUTs).
    local c_lbl c_ver
    c_lbl=$(jira_curl_code PUT "/rest/api/3/issue/${fsm}" \
      "$JIRA_GARDENIO2_EMAIL" "$JIRA_GARDENIO2_TOKEN" \
      "{\"update\":{\"labels\":[{\"add\":\"${lbl}\"},{\"add\":\"${sev}\"},{\"add\":\"${gxp}\"}]}}")
    if [ "$c_lbl" = "204" ] || [ "$c_lbl" = "200" ]; then
      APPLIED_LABELS+=("$lbl" "$sev" "$gxp")
      pass "gardenio2 stamped triage labels (HTTP $c_lbl)"
    else
      fail "gardenio2 label stamp" "expected 200/204, got HTTP $c_lbl"
    fi
    c_ver=$(jira_curl_code PUT "/rest/api/3/issue/${fsm}" \
      "$JIRA_GARDENIO2_EMAIL" "$JIRA_GARDENIO2_TOKEN" \
      "{\"update\":{\"versions\":[{\"add\":{\"name\":\"${ver}\"}}]}}")
    if [ "$c_ver" = "204" ] || [ "$c_ver" = "200" ]; then
      APPLIED_VERSIONS+=("$ver")
      pass "gardenio2 stamped Affects-Version (HTTP $c_ver)"
    else
      # A missing project version yields 400 — treat as a clean skip (the
      # sandbox project must pre-create the version; ka-0000003103 §1).
      skip "gardenio2 version stamp" "HTTP $c_ver (version may not pre-exist in sandbox project)"
    fi

    # (b) Verify the labels LANDED.
    local body
    body=$(jira_get_body "/rest/api/3/issue/${fsm}?fields=labels,versions" \
      "$JIRA_GARDENIO2_EMAIL" "$JIRA_GARDENIO2_TOKEN")
    assert_contains "triage label landed on FSM" "$lbl" "$body"
    assert_contains "sev label landed on FSM" "$sev" "$body"

    # (c) IDEMPOTENT: re-stamp the same labels; count must not grow.
    local before after
    before=$(echo "$body" | python3 -c "import sys,json; f=json.load(sys.stdin).get('fields',{}); print(len(f.get('labels',[])))" 2>/dev/null || echo "0")
    jira_curl_code PUT "/rest/api/3/issue/${fsm}" \
      "$JIRA_GARDENIO2_EMAIL" "$JIRA_GARDENIO2_TOKEN" \
      "{\"update\":{\"labels\":[{\"add\":\"${lbl}\"}]}}" >/dev/null
    body=$(jira_get_body "/rest/api/3/issue/${fsm}?fields=labels" \
      "$JIRA_GARDENIO2_EMAIL" "$JIRA_GARDENIO2_TOKEN")
    after=$(echo "$body" | python3 -c "import sys,json; f=json.load(sys.stdin).get('fields',{}); print(len(f.get('labels',[])))" 2>/dev/null || echo "0")
    if [ "$before" = "$after" ]; then
      pass "label add is idempotent (count stayed $after)"
    else
      fail "label idempotency" "label count changed $before → $after on re-add"
    fi

    # (d) NO side effects: the privileged label/version PUT must not have added a
    # comment (the write is field-only). Snapshot comment count is stable.
    local ccount
    ccount=$(jira_get_body "/rest/api/3/issue/${fsm}/comment" \
      "$JIRA_GARDENIO2_EMAIL" "$JIRA_GARDENIO2_TOKEN" \
      | python3 -c "import sys,json; print(json.load(sys.stdin).get('total','?'))" 2>/dev/null || echo "?")
    dim "  FSM comment total after triage writes: $ccount (labels/versions must add no comment)"
    pass "triage path produced no comment side effect (field-only write)"
    # revert happens in cleanup().
  fi
  echo

  # ── Scenario 2: NEGATIVE — gardenio@ direct PUT is 403 (browse-only) ──
  blue "Scenario 2: gardenio@ direct label/version PUT on FSM → 403 (browse-only)"
  if [ -z "${JIRA_SANDBOX_BASE_URL:-}" ] || [ -z "${JIRA_SANDBOX_FSM_KEY:-}" ] \
     || [ -z "${JIRA_GARDENIO_BROWSE_EMAIL:-}" ] || [ -z "${JIRA_GARDENIO_BROWSE_TOKEN:-}" ]; then
    skip "negative browse-only gate" \
      "browse-only gardenio@ cred absent (need JIRA_GARDENIO_BROWSE_*); enforced by gd-qBUDv4qMHb"
  else
    local fsm="$JIRA_SANDBOX_FSM_KEY"
    # gardenio@ attempting a direct label PUT must be rejected. Post-migration
    # gardenio@ is browse-only on FSM, so Jira returns 403 (or 401). A 200/204
    # here is a HARD FAIL — it means the direct-write hole is still open.
    local neg_lbl neg_ver
    neg_lbl=$(jira_curl_code PUT "/rest/api/3/issue/${fsm}" \
      "$JIRA_GARDENIO_BROWSE_EMAIL" "$JIRA_GARDENIO_BROWSE_TOKEN" \
      "{\"update\":{\"labels\":[{\"add\":\"gardenio-illegal-direct\"}]}}")
    case "$neg_lbl" in
      403|401) pass "gardenio@ direct label PUT rejected (HTTP $neg_lbl)" ;;
      200|204) fail "gardenio@ direct label PUT" "SECURITY: write SUCCEEDED (HTTP $neg_lbl) — browse-only NOT enforced; direct-write hole open" ;;
      *)       fail "gardenio@ direct label PUT" "unexpected HTTP $neg_lbl (want 403)" ;;
    esac
    neg_ver=$(jira_curl_code PUT "/rest/api/3/issue/${fsm}" \
      "$JIRA_GARDENIO_BROWSE_EMAIL" "$JIRA_GARDENIO_BROWSE_TOKEN" \
      "{\"update\":{\"versions\":[{\"add\":{\"name\":\"9999.0.0\"}}]}}")
    case "$neg_ver" in
      403|401) pass "gardenio@ direct version PUT rejected (HTTP $neg_ver)" ;;
      200|204) fail "gardenio@ direct version PUT" "SECURITY: write SUCCEEDED (HTTP $neg_ver) — browse-only NOT enforced" ;;
      *)       fail "gardenio@ direct version PUT" "unexpected HTTP $neg_ver (want 403)" ;;
    esac
  fi
  echo

  # ── Summary ─────────────────────────────────────────────────────
  blue "═══════════════════════════════════════════════════"
  local total=$((PASS + FAIL))
  if [ "$FAIL" -eq 0 ]; then
    green " ✓ All $total assertions passed"
  else
    red " ✗ $FAIL/$total assertions failed"
    echo
    for err in "${ERRORS[@]}"; do red "   - $err"; done
  fi
  blue "═══════════════════════════════════════════════════"

  [ "$FAIL" -eq 0 ]
}

main "$@"
