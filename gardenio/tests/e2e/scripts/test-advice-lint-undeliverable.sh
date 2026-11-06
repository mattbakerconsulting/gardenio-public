#!/usr/bin/env bash
# test-advice-lint-undeliverable.sh — E2E for `gb advice lint`'s
# undeliverable-capability rule (ka-0000003515).
#
# `gb advice lint` is the guard that catches advice which can never reach
# anyone — the failure mode is silent by construction: the bead exists, looks
# authored, and is delivered to nobody. The undeliverable-capability rule is the
# doubly-important case: a bead whose TITLE asserts a capability ("agents CAN do
# X" / "no escalation needed") while its labels make it undeliverable, so the
# fleet's correction to a false "I can't do this" belief is stored exactly where
# no agent will ever be shown it (the real defect behind ka-0000006777 and
# ka-0000005080 — both carried scope:on-demand ONLY, so role-matching never
# delivered them while dozens of escalations cited the exact denial they refute).
#
# Runtime under test:
#   controller/cmd/gb/advice_lint.go                 (the `gb advice lint` verb)
#   controller/internal/advice/lint.go LintAdvice    (LintUndeliverableCap)
# Go unit coverage exists in controller/internal/advice/lint_test.go; this is the
# wired-into-the-daemon e2e companion (there was none — grep 'advice lint' and
# 'undeliverable' over gardenio/tests/ both returned 0 before this module).
#
# Scenarios (BOTH legs are required — without the PASS leg the test cannot
# distinguish "lint flags undeliverable capability advice" from "lint flags
# everything"; the PASS leg is the known-good control):
#   1. FLAGS  — a capability-asserting advice bead that is undeliverable
#               (scope:on-demand, the escape hatch NO other check catches)
#               yields an undeliverable-capability finding at ERROR severity
#               naming that exact bead id.
#   2. PASSES — the SAME capability-asserting title, but correctly targeted
#               (deliverable), yields ZERO findings. Proves the flag tracks
#               TARGETING, not the title text.
#   3. EXIT    — with an undeliverable-capability error present in the corpus,
#               `gb advice lint --errors-only` exits non-zero (the gate signal).
#   4. EXEMPT  — an undeliverable capability advice bead (scope:on-demand, the
#               same shape as Scenario 1) whose id is named in an OPEN `prerun`
#               config bead's `replaces_advice` is NOT flagged: the capability
#               correction is delivered LIVE per-pod by the boot probe, so its
#               on-demand label is deliberate, not a delivery gap. This is the
#               `prerunReplacedAdvice` exemption (advice_lint.go:171 →
#               LintAdvice replacedByPrerun). The Scenario-1 bead — identical
#               but with NO prerun replacement — MUST still be flagged in the
#               SAME lint run, or the exemption would pass vacuously against a
#               globally-broken lint (the anti-vacuity control for this leg).
#               Regression guard for decision D2 (ka-0000005100): without the
#               exemption, lint pushes agents to retarget a probe-delivered
#               scope:on-demand bead back to project:/role:, the exact churn D2
#               had to undo after the 2026-07-30 revert.
#
# Assertions are per-seed-bead-id against `gb advice lint --json`, NOT the
# global exit code alone: the shared corpus routinely already contains
# error-severity findings, so a bare "exit != 0" would pass even if our seed
# bead were mis-linted. We assert on OUR bead's findings specifically.

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
CREATED=()

export KD_ACTOR="${KD_ACTOR:-e2e-test}"

# ── Helpers ───────────────────────────────────────────────────────
red()   { printf '\033[0;31m%s\033[0m\n' "$*"; }
green() { printf '\033[0;32m%s\033[0m\n' "$*"; }
blue()  { printf '\033[0;34m%s\033[0m\n' "$*"; }
dim()   { printf '\033[2m%s\033[0m\n' "$*"; }

pass() { green "  ✓ $1"; PASS=$((PASS + 1)); }
fail() { red "  ✗ $1"; [ -n "${2:-}" ] && dim "    $2"; FAIL=$((FAIL + 1)); ERRORS+=("$1: ${2:-}"); }

# ── Setup ─────────────────────────────────────────────────────────
setup_portforward() {
  if [ -n "${BEADS_HTTP_URL:-}" ]; then
    blue "Using BEADS_HTTP_URL=$BEADS_HTTP_URL"
    return
  fi
  blue "Port-forwarding ${NAMESPACE} kd server..."
  local port=19144
  kubectl -n "$NAMESPACE" port-forward "svc/${BEADS_SVC}" "${port}:8080" \
    >/tmp/kd-pf-advice-lint-undeliverable.log 2>&1 &
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
  if [ -n "$secret" ]; then
    export BEADS_TOKEN="$secret"
    dim "  Token loaded from secret kd-beads-token"
  fi
}

cleanup() {
  for id in "${CREATED[@]}"; do
    "$KD_BIN" delete "$id" 2>/dev/null || true
  done
  if [ -n "$PF_PID" ]; then
    kill "$PF_PID" 2>/dev/null || true
  fi
}
trap cleanup EXIT

# Create an advice bead with the given title and labels; echo its id (empty on
# failure). Labels are passed as extra args, e.g. mk_advice "title" -l role:x.
#
# NOTE: this echoes the id and does NOT itself track it for cleanup. Callers run
# it in a command substitution ($(...)), which is a SUBSHELL — an array append
# inside here would never reach the parent CREATED[] the EXIT trap deletes from,
# so the seed bead would leak into the (possibly shared) corpus. Every caller
# MUST `track "$id"` in the parent shell right after capturing the id.
mk_advice() {
  local title="$1"; shift
  "$KD_BIN" create "$title" --type=advice "$@" --json 2>/dev/null \
    | python3 -c "import sys,json; print(json.load(sys.stdin).get('id',''))" 2>/dev/null || echo ""
}

# Create a `prerun` config bead whose `replaces_advice` names the given advice
# bead id(s); echo its id (empty on failure). This is the boot-time prerun that
# delivers the capability correction LIVE per-pod, so the undeliverable-capability
# check must exempt any advice id it lists (advice_lint.go prerunReplacedAdvice →
# LintAdvice replacedByPrerun). Global-scoped so prerunReplacedAdvice's
# all-layers config scan finds it (it does not filter by project). The command is
# a harmless read-only echo — a prerun re-fires on every boot, so it must never
# mutate. Same subshell caveat as mk_advice: caller MUST `track` the id.
mk_prerun_config() {
  local replaces_id="$1"
  local body
  body=$(python3 - "$replaces_id" <<'PY'
import sys, json
print(json.dumps({"prerun": [{
    "id": "e2e-lint-undeliv-probe",
    "heading": "e2e prerun exemption probe",
    "cmd": "echo e2e-prerun-exemption-probe",
    "replaces_advice": [sys.argv[1]],
}]}))
PY
)
  "$KD_BIN" create "prerun" --type=config -l global -d "$body" --json 2>/dev/null \
    | python3 -c "import sys,json; print(json.load(sys.stdin).get('id',''))" 2>/dev/null || echo ""
}

# Register a bead id for deletion by the EXIT trap. Runs in the parent shell
# (never a subshell) so the append actually sticks.
track() { [ -n "${1:-}" ] && CREATED+=("$1"); }

# Count of lint findings for a given bead id with a given check name in the
# lint --json output captured in $1 (a file path).
findings_for() {
  local lint_json_file="$1" bead_id="$2" check="$3"
  python3 - "$lint_json_file" "$bead_id" "$check" <<'PY'
import sys, json
with open(sys.argv[1]) as fh:
    data = json.load(fh)
bead_id, check = sys.argv[2], sys.argv[3]
n = sum(1 for f in data if f.get("id") == bead_id and f.get("check") == check)
print(n)
PY
}

# Total count of lint findings (any check) for a given bead id.
total_findings_for() {
  local lint_json_file="$1" bead_id="$2"
  python3 - "$lint_json_file" "$bead_id" <<'PY'
import sys, json
with open(sys.argv[1]) as fh:
    data = json.load(fh)
bead_id = sys.argv[2]
n = sum(1 for f in data if f.get("id") == bead_id)
print(n)
PY
}

# Severity of the undeliverable-capability finding for a bead id (empty if none).
severity_for() {
  local lint_json_file="$1" bead_id="$2" check="$3"
  python3 - "$lint_json_file" "$bead_id" "$check" <<'PY'
import sys, json
with open(sys.argv[1]) as fh:
    data = json.load(fh)
bead_id, check = sys.argv[2], sys.argv[3]
for f in data:
    if f.get("id") == bead_id and f.get("check") == check:
        print(f.get("severity", ""))
        break
PY
}

# ── Test body ─────────────────────────────────────────────────────
main() {
  blue "═══════════════════════════════════════════════════"
  blue " advice lint undeliverable-capability E2E"
  blue "═══════════════════════════════════════════════════"

  setup_portforward
  setup_token

  if ! "$KD_BIN" list --status=open --json >/dev/null 2>&1; then
    red "ERROR: Cannot reach kd server at $BEADS_HTTP_URL"
    exit 1
  fi
  green "  Server reachable: $BEADS_HTTP_URL"
  echo

  local TAG; TAG="e2e-lint-undeliv-$$-$(date +%s)"
  # A title that trips assertsCapability's capabilityCanRE (`\bCAN\b +[a-z]`).
  # The all-caps standalone CAN + lowercase verb is the exact "I CAN do this"
  # correction shout the rule keys on (ka-0000006777's real title shape).
  local CAP_TITLE_UNDELIV="$TAG agents CAN read pod logs cluster-wide"
  local CAP_TITLE_OK="$TAG agents CAN read pod logs cluster-wide (deliverable)"

  local lint_json; lint_json=$(mktemp)

  # ── Scenario 1: FLAGS — undeliverable capability advice ──────────
  # scope:on-demand is the escape hatch that no OTHER lint check flags: it
  # suppresses no-targeting (on-demand is a legit "not role-matched" doc) yet
  # is never delivered by role matching. Pair it with a role: label so the
  # bead trips ONLY undeliverable-capability, isolating the rule under test
  # (a bare on-demand bead also trips denied-broadcast via the auto-stamped
  # project label, which would muddy the assertion).
  blue "Scenario 1: undeliverable capability advice is FLAGGED (error)"
  local undeliv_id
  undeliv_id=$(mk_advice "$CAP_TITLE_UNDELIV" -l scope:on-demand -l "role:$TAG-fakerole")
  track "$undeliv_id"
  if [ -z "$undeliv_id" ]; then
    fail "create undeliverable capability advice" "no id returned"
    blue "═══════════════════════════════════════════════════"
    red " ✗ setup failed"
    return 1
  fi
  pass "undeliverable capability advice created ($undeliv_id)"

  "$GB_BIN" advice lint --json >"$lint_json" 2>/dev/null || true
  if ! python3 -c "import json,sys; json.load(open('$lint_json'))" 2>/dev/null; then
    fail "gb advice lint --json is valid JSON" "output not parseable"
    return 1
  fi
  pass "gb advice lint --json returned valid JSON"

  local n_undeliv sev
  n_undeliv=$(findings_for "$lint_json" "$undeliv_id" "undeliverable-capability")
  if [ "$n_undeliv" -ge 1 ]; then
    pass "lint flags our bead with undeliverable-capability"
  else
    fail "lint flags undeliverable-capability" \
      "expected >=1 undeliverable-capability finding for $undeliv_id, got $n_undeliv"
  fi

  sev=$(severity_for "$lint_json" "$undeliv_id" "undeliverable-capability")
  if [ "$sev" = "error" ]; then
    pass "undeliverable-capability finding is severity=error"
  else
    fail "undeliverable-capability severity=error" "got severity='$sev'"
  fi
  echo

  # ── Scenario 2: PASSES — same title, correctly targeted (control) ─
  # This is the known-good control. The ONLY difference from Scenario 1 is the
  # targeting: a role: label WITHOUT scope:on-demand is deliverable, so the
  # identical capability-asserting title yields NO findings. Without this leg
  # the suite cannot tell "lint flags undeliverable capability advice" apart
  # from "lint flags every capability-titled bead".
  #
  # The role MUST be a LIVE role (a builtin, a type:role bead, or an open
  # trigger's role) — the control asserts ZERO *total* findings, and the
  # dead-role-target check (WARN) fires against the live daemon for any role:
  # target outside the live-role union. An invented placeholder like
  # `role:crew-e2e` is NOT live, so on the real E2E daemon (where liveRoleSet is
  # populated, unlike the nil-registry unit tests) it trips dead-role-target and
  # the "zero total findings" assertion fails — the exact false negative that
  # reddened releases .16/.17. `crew` is a first-class builtin (role.go), so the
  # control bead is genuinely, fully deliverable with no hygiene problems.
  blue "Scenario 2: same capability title, correctly targeted → PASSES (control)"
  local ok_id
  ok_id=$(mk_advice "$CAP_TITLE_OK" -l "role:crew")
  track "$ok_id"
  if [ -z "$ok_id" ]; then
    fail "create deliverable capability advice" "no id returned"
  else
    pass "deliverable capability advice created ($ok_id)"
    "$GB_BIN" advice lint --json >"$lint_json" 2>/dev/null || true

    local n_ok_undeliv n_ok_total
    n_ok_undeliv=$(findings_for "$lint_json" "$ok_id" "undeliverable-capability")
    if [ "$n_ok_undeliv" -eq 0 ]; then
      pass "correctly-targeted capability advice is NOT flagged undeliverable-capability"
    else
      fail "control bead wrongly flagged" \
        "expected 0 undeliverable-capability findings for $ok_id, got $n_ok_undeliv"
    fi

    n_ok_total=$(total_findings_for "$lint_json" "$ok_id")
    if [ "$n_ok_total" -eq 0 ]; then
      pass "correctly-targeted capability advice has zero lint findings"
    else
      fail "control bead has unexpected findings" \
        "expected 0 total findings for $ok_id, got $n_ok_total"
    fi
  fi
  echo

  # ── Scenario 3: EXIT — error present ⇒ --errors-only exits non-zero ─
  # The undeliverable bead from Scenario 1 is still open, so the corpus carries
  # at least one error-severity finding; the lint gate must reflect that in its
  # exit code so it can red an advice-hygiene workflow.
  blue "Scenario 3: --errors-only exits non-zero while the error bead is open"
  local exit_rc=0
  "$GB_BIN" advice lint --errors-only >/dev/null 2>&1 || exit_rc=$?
  if [ "$exit_rc" -ne 0 ]; then
    pass "gb advice lint --errors-only exits non-zero (exit $exit_rc)"
  else
    fail "lint exit code" "expected non-zero with an open error bead, got 0"
  fi
  echo

  # ── Scenario 4: EXEMPT — prerun-replaced capability advice is NOT flagged ─
  # The undeliverable-capability check exempts a bead whose id is named in an
  # open `prerun` config bead's `replaces_advice`: the boot probe delivers the
  # capability correction LIVE per-pod, so scope:on-demand is deliberate, not a
  # delivery gap (advice_lint.go prerunReplacedAdvice → LintAdvice
  # replacedByPrerun). Seed a bead with the EXACT undeliverable shape of
  # Scenario 1, then a global `prerun` config bead that replaces it, and assert
  # the bead earns ZERO undeliverable-capability findings.
  #
  # The Scenario-1 bead ($undeliv_id) is still open and NOT prerun-replaced, so
  # its assertion below (re-checked against the SAME fresh lint run) is the
  # anti-vacuity control: if the exemption were implemented by globally
  # disabling the check, BOTH beads would go unflagged and the suite would pass
  # vacuously. Requiring $undeliv_id to STAY flagged while $exempt_id is exempt
  # proves the exemption is keyed on the prerun replacement, not blanket-off.
  blue "Scenario 4: prerun-replaced capability advice is EXEMPT (not flagged)"
  local exempt_id prerun_cfg_id
  exempt_id=$(mk_advice "$TAG agents CAN read pod logs cluster-wide (prerun)" \
    -l scope:on-demand -l "role:$TAG-fakerole")
  track "$exempt_id"
  if [ -z "$exempt_id" ]; then
    fail "create prerun-exempt capability advice" "no id returned"
  else
    pass "prerun-exempt capability advice created ($exempt_id)"
    prerun_cfg_id=$(mk_prerun_config "$exempt_id")
    track "$prerun_cfg_id"
    if [ -z "$prerun_cfg_id" ]; then
      fail "create prerun config bead" "no id returned"
    else
      pass "prerun config bead created, replaces_advice=[$exempt_id] ($prerun_cfg_id)"

      "$GB_BIN" advice lint --json >"$lint_json" 2>/dev/null || true
      if ! python3 -c "import json,sys; json.load(open('$lint_json'))" 2>/dev/null; then
        fail "gb advice lint --json is valid JSON (exempt run)" "output not parseable"
      else
        local n_exempt n_ctrl
        n_exempt=$(findings_for "$lint_json" "$exempt_id" "undeliverable-capability")
        if [ "$n_exempt" -eq 0 ]; then
          pass "prerun-replaced capability advice is NOT flagged undeliverable-capability"
        else
          fail "prerun-exempt bead wrongly flagged" \
            "expected 0 undeliverable-capability findings for $exempt_id (prerun-replaced), got $n_exempt"
        fi

        # Anti-vacuity control: the non-exempt Scenario-1 bead MUST stay flagged
        # in this same run, proving the exemption tracks the prerun replacement
        # and did not globally disable the check.
        n_ctrl=$(findings_for "$lint_json" "$undeliv_id" "undeliverable-capability")
        if [ "$n_ctrl" -ge 1 ]; then
          pass "non-exempt capability advice STILL flagged in same run (anti-vacuity)"
        else
          fail "anti-vacuity control failed" \
            "expected the non-prerun bead $undeliv_id to stay flagged (>=1), got $n_ctrl — exemption may be globally disabling the check"
        fi
      fi
    fi
  fi
  echo

  rm -f "$lint_json"

  # ── Summary ─────────────────────────────────────────────────────
  blue "═══════════════════════════════════════════════════"
  local total=$((PASS + FAIL))
  if [ "$FAIL" -eq 0 ]; then
    green " ✓ All $total assertions passed"
  else
    red " ✗ $FAIL/$total assertions failed"
    echo
    for err in "${ERRORS[@]}"; do
      red "   - $err"
    done
  fi
  blue "═══════════════════════════════════════════════════"

  [ "$FAIL" -eq 0 ]
}

main "$@"
