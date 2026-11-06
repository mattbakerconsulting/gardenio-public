#!/usr/bin/env bash
# test-gitlab-pipeline-refpattern.sh — gitlab-pipeline trigger pipeline_ref_pattern
# gate E2E (task ka-0000002595, parent ka-0000000958 — E2E coverage-gap sprint).
#
# Backfills the behavioral e2e gap MR !6601 left open. pipeline_ref_pattern — the
# optional Go regexp a gitlab-pipeline trigger applies to each pipeline ref AFTER
# the GitLab fetch and BEFORE a match is returned, so a non-matching ref never
# spawns an agent — shipped with strong UNIT coverage (trigger_source_gitlab_test
# .go EvaluatePipelines_RefPattern/_InvalidRefPattern, init_gitlab_pipeline_test
# .go, cmd/gb/trigger_test.go, beads-viewer/triggers_test.go) but ZERO end-to-end
# assertion that drives the whole evaluation→fireAction→recordTriggerAudit loop
# and proves a non-matching ref actually SUPPRESSES the fire. The negative leg is
# the one that protects against a runaway trigger.
#
# ── Two independent surfaces, two mechanisms ─────────────────────────────────
#
#   A. THE FIRING GATE (hermetic from-source probe — the HARD core).
#      The gate lives in GitLabTriggerSource.evaluatePipelines, which needs a live
#      GitLabClient HTTP API. The deployed trigger-engine does NOT register the
#      gitlab source (only gitlab-bridge does, and gitlab-bridge is enabled:false
#      in the ephemeral e2e namespace), so there is no live process to
#      observe — the exact constraint that makes test-gitlab-dark-provenance.sh
#      and test-jira-conversational.sh from-source probes. Driving it against the
#      shared daemon via `trigger-probe --source gitlab-pipeline` would fan out to
#      unrelated production gitlab triggers and leave audit beads on the shared
#      store. So cmd/gitlab-pipeline-trigger-probe wires the REAL source + REAL
#      TriggerEngine against a stub multi-ref GitLab httptest server + an in-memory
#      mock daemon and asserts the pattern GATES firing: matching refs fire (one
#      allow audit each), non-matching refs fire NOTHING, an all-filtered ref set
#      fires zero (with a no-pattern control proving the pattern is the cause), an
#      invalid pattern fails closed, and the authored pattern round-trips verbatim
#      through the production ParseTriggerDef path. Hermetic; a HARD gate.
#
#   B. THE CLI AUTHORING ROUND-TRIP (live daemon, when present).
#      `gb trigger create gitlab-pipeline --ref-pattern <re>` is the production
#      chokepoint the probe deliberately bypasses (it seeds the bead directly).
#      When a live daemon is reachable this leg authors a real trigger through the
#      CLI, asserts the pipeline_ref_pattern field persists on the bead (kd show)
#      AND that an INVALID --ref-pattern is rejected at authoring time (compiled
#      in buildGitLabPipelineFields so a typo fails loud, not fail-closed-at-
#      runtime). Skips CLEANLY (never fails) when no daemon/gb is available.
#
# ── Skip-clean contract ──────────────────────────────────────────────────────
#
# The Go toolchain + cmd/gitlab-pipeline-trigger-probe source ride every e2e
# shard (the shard `use:` list includes build-gitlab-pipeline-trigger-probe). The
# probe legs are HARD assertions when the binary/toolchain is present; if BOTH a
# pre-built binary is absent AND no Go toolchain/source is available, the whole
# module SKIPS CLEANLY (exit 0) with a LOUD banner — a missing toolchain must
# never red the suite, mirroring gitlab-dark-provenance's skip_all("loud").
#
# Usage:
#   ./gardenio/tests/e2e/scripts/test-gitlab-pipeline-refpattern.sh

set -uo pipefail

# ── Config ────────────────────────────────────────────────────────
CONTROLLER_DIR="${CONTROLLER_DIR:-gardenio/controller}"
GB_BIN="${GB_BIN:-gb}"
KD_BIN="${KD_BIN:-kd}"
RUN_ID="${RUN_ID:-$$}"
PROBE_BIN=""
PROBE_BUILT=0   # 1 iff we built a temp probe that cleanup() must rm

PASS=0
FAIL=0
ERRORS=()
CREATED_BEADS=()

# ── Helpers ───────────────────────────────────────────────────────
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

# skip_all exits 0 (a missing toolchain must NOT red the suite) with a LOUD
# banner so a skip is never silently read as a pass.
skip_all() {
  local why="$1"
  red "════════════════════════════════════════════════════"
  red " SKIP gitlab-pipeline-refpattern (LOUD): $why"
  red " This run did NOT assert that pipeline_ref_pattern gates gitlab-pipeline"
  red " trigger firing (match + the negative/no-match leg that guards against a"
  red " runaway trigger). A green suite here proves NOTHING about MR !6601. The"
  red " e2e shard ships a Go toolchain + cmd/gitlab-pipeline-trigger-probe"
  red " (build-gitlab-pipeline-trigger-probe) — if this fires in CI, that wiring"
  red " regressed."
  red "════════════════════════════════════════════════════"
  if [ -n "${E2E_SKIP_SENTINEL:-}" ]; then printf '%s\n' "$*" >"$E2E_SKIP_SENTINEL" 2>/dev/null || true; fi  # skip visibility (ka-0000000989)
  exit 0
}

PATH_INSTALLED=0
cleanup() {
  for id in "${CREATED_BEADS[@]:-}"; do
    [ -n "$id" ] && "$KD_BIN" close "$id" >/dev/null 2>&1 || true
  done
  # Only remove a temp binary we built; never a PATH-installed one.
  [ "$PROBE_BUILT" = "1" ] && [ "$PATH_INSTALLED" != "1" ] && [ -n "$PROBE_BIN" ] \
    && rm -f "$PROBE_BIN" 2>/dev/null || true
}
trap cleanup EXIT

echo "════════════════════════════════════════════════════"
echo " gitlab-pipeline pipeline_ref_pattern gate E2E (ka-0000002595)"
echo " match+negative → all-filtered → fail-closed → field-roundtrip"
echo "════════════════════════════════════════════════════"

# ── 1. Resolve the probe binary ─────────────────────────────────────
# Prefer a binary pre-built +
# smoke-checked onto PATH (the gitlab-prov-probe precedent). Fall back to an
# in-shell build from source for local dev / robustness. Only when BOTH the
# pre-built binary is absent AND no Go toolchain/source is available do we skip.
if command -v gitlab-pipeline-trigger-probe >/dev/null 2>&1; then
  PROBE_BIN="$(command -v gitlab-pipeline-trigger-probe)"
  PATH_INSTALLED=1
  pass "using pre-built gitlab-pipeline-trigger-probe ($PROBE_BIN)"
elif [ -x /usr/local/bin/gitlab-pipeline-trigger-probe ]; then
  PROBE_BIN=/usr/local/bin/gitlab-pipeline-trigger-probe
  PATH_INSTALLED=1
  pass "using gitlab-pipeline-trigger-probe ($PROBE_BIN)"
else
  PATH_INSTALLED=0
  command -v go >/dev/null 2>&1 \
    || skip_all "no pre-built gitlab-pipeline-trigger-probe on PATH and no go toolchain to build it"
  [ -d "$CONTROLLER_DIR/cmd/gitlab-pipeline-trigger-probe" ] \
    || skip_all "no pre-built probe and cmd/gitlab-pipeline-trigger-probe missing under $CONTROLLER_DIR"
  blue "[build] cmd/gitlab-pipeline-trigger-probe (from source)"
  PROBE_BIN="$(mktemp "/tmp/gitlab-pipeline-trigger-probe.${RUN_ID}.XXXXXX")"
  BUILD_LOG="/tmp/gitlab-pipeline-trigger-probe-build-${RUN_ID}.log"
  if ( cd "$CONTROLLER_DIR" && CGO_ENABLED=0 go build -o "$PROBE_BIN" ./cmd/gitlab-pipeline-trigger-probe/ ) >"$BUILD_LOG" 2>&1; then
    PROBE_BUILT=1
    pass "cmd/gitlab-pipeline-trigger-probe built from source"
  else
    fail "build gitlab-pipeline-trigger-probe" "go build failed: $(tail -8 "$BUILD_LOG")"
    # A build failure IS a hard failure — the probe is the whole core gate.
    blue "════════════════════════════════════════════════════"
    red " ✗ 1/1 assertions failed (probe build)"
    for err in "${ERRORS[@]}"; do red "   - $err"; done
    blue "════════════════════════════════════════════════════"
    exit 1
  fi
fi
echo

# ── 2. Run the probe; map each LEG line to a pass/fail assertion ─────
# The probe prints one "LEG <name>: PASS|FAIL <detail>" line per leg and exits
# non-zero if any leg failed. We require ALL expected legs to be present (a
# missing leg is a failure, so a probe that silently drops a leg cannot pass).
blue "── Firing gate (hermetic probe): pipeline_ref_pattern suppresses non-matching refs ──"
PROBE_OUT="/tmp/gitlab-pipeline-trigger-probe-${RUN_ID}.out"
PROBE_ERR="/tmp/gitlab-pipeline-trigger-probe-${RUN_ID}.err"
probe_rc=0
"$PROBE_BIN" >"$PROBE_OUT" 2>"$PROBE_ERR" || probe_rc=$?

EXPECTED_LEGS="match-and-negative all-filtered-suppressed invalid-pattern-fails-closed field-roundtrip"
for leg in $EXPECTED_LEGS; do
  line=$(grep -E "^LEG ${leg}: " "$PROBE_OUT" 2>/dev/null | head -1 || true)
  if [ -z "$line" ]; then
    fail "leg $leg" "no result line emitted by the probe (leg dropped)"
    continue
  fi
  detail="${line#LEG "${leg}": }"
  case "$detail" in
    PASS*) pass "leg $leg — ${detail#PASS }" ;;
    FAIL*) fail "leg $leg" "${detail#FAIL }" ;;
    *)     fail "leg $leg" "unrecognized result: $detail" ;;
  esac
done

# Belt-and-suspenders: the probe's own exit code must agree with our parse. If
# it exited non-zero but every parsed leg passed (should be impossible), treat
# it as a failure so a probe-level error is never masked.
if [ "$probe_rc" -ne 0 ] && [ "$FAIL" -eq 0 ]; then
  fail "probe exit code" "probe exited $probe_rc but all parsed legs passed — probe-level error (stderr: $(tail -3 "$PROBE_ERR" 2>/dev/null))"
fi
echo

# ── 3. CLI authoring round-trip (live daemon, when present) ─────────
# `gb trigger create gitlab-pipeline --ref-pattern` is the production authoring
# chokepoint the probe bypasses. When a live daemon is reachable, assert the
# field persists on the bead AND that an invalid --ref-pattern is rejected at
# authoring (compiled in buildGitLabPipelineFields). Skips cleanly otherwise.
blue "── CLI authoring round-trip: gb trigger create gitlab-pipeline --ref-pattern ──"
if [ -z "${BEADS_HTTP_URL:-}" ] || ! command -v "$GB_BIN" >/dev/null 2>&1 \
   || ! command -v "$KD_BIN" >/dev/null 2>&1 \
   || ! "$KD_BIN" list --status=open --json >/dev/null 2>&1; then
  dim "  no live daemon / gb / kd reachable — skipping CLI round-trip leg (probe legs above are the HARD gate)"
else
  TAG="glprefpat-${RUN_ID}-$(date +%s)"
  PATTERN='^(main|refs/merge-requests/[0-9]+/train)$'

  # (a) A valid --ref-pattern round-trips onto the bead's pipeline_ref_pattern.
  create_out=$("$GB_BIN" trigger create gitlab-pipeline \
    --title "e2e-glp-refpat-$TAG" \
    --project "Acme/Platform/gardenio" \
    --status failed \
    --ref-pattern "$PATTERN" \
    --action notify \
    --prompt "e2e ref-pattern round-trip $TAG" \
    --enabled=false \
    --label "e2e:gitlab-pipeline-refpattern" --label "e2e-tag:$TAG" \
    --json 2>&1) || true
  trig_id=$(printf '%s' "$create_out" | python3 -c "import sys,json
try:
  d=json.load(sys.stdin); print(d.get('id') or d.get('ID') or '')
except Exception:
  print('')" 2>/dev/null || echo "")

  if [ -z "$trig_id" ]; then
    fail "gb trigger create gitlab-pipeline --ref-pattern" "no trigger id in output: $(printf '%s' "$create_out" | head -3)"
  else
    CREATED_BEADS+=("$trig_id")
    pass "gitlab-pipeline trigger authored with --ref-pattern ($trig_id)"

    show_json=$("$KD_BIN" show "$trig_id" --json 2>/dev/null || echo "{}")
    got_pattern=$(printf '%s' "$show_json" | python3 -c "import sys,json; print((json.load(sys.stdin).get('fields') or {}).get('pipeline_ref_pattern',''))" 2>/dev/null || echo "")
    if [ "$got_pattern" = "$PATTERN" ]; then
      pass "pipeline_ref_pattern field round-trips onto the bead ($got_pattern)"
    else
      fail "pipeline_ref_pattern round-trip" "bead field = '$got_pattern', want '$PATTERN'"
    fi

    # The trigger-source:gitlab-pipeline label is stamped at the create chokepoint.
    if printf '%s' "$show_json" | python3 -c "import sys,json; sys.exit(0 if 'trigger-source:gitlab-pipeline' in (json.load(sys.stdin).get('labels') or []) else 1)" 2>/dev/null; then
      pass "trigger carries trigger-source:gitlab-pipeline label"
    else
      fail "trigger-source label" "trigger-source:gitlab-pipeline not on bead labels"
    fi
  fi

  # (b) An INVALID --ref-pattern is rejected at authoring time (compiled in
  # buildGitLabPipelineFields so a typo fails LOUD, not silently-fail-closed).
  bad_out=$("$GB_BIN" trigger create gitlab-pipeline \
    --title "e2e-glp-badpat-$TAG" \
    --project "Acme/Platform/gardenio" \
    --status failed \
    --ref-pattern '[unterminated' \
    --action notify \
    --prompt "should be rejected" \
    --enabled=false \
    --json 2>&1) || true
  bad_id=$(printf '%s' "$bad_out" | python3 -c "import sys,json
try:
  d=json.load(sys.stdin); print(d.get('id') or d.get('ID') or '')
except Exception:
  print('')" 2>/dev/null || echo "")
  if [ -n "$bad_id" ]; then
    CREATED_BEADS+=("$bad_id")
    fail "invalid --ref-pattern rejection" "authoring SUCCEEDED (id=$bad_id) — a bad regexp should be rejected at create time"
  elif grep -qiE 'ref-pattern|regexp|error parsing' <<<"$bad_out"; then
    pass "invalid --ref-pattern rejected at authoring time (compiled in buildGitLabPipelineFields)"
  else
    fail "invalid --ref-pattern rejection" "no trigger created but no clear regexp error: $(printf '%s' "$bad_out" | head -3)"
  fi
fi
echo

# ── Summary ─────────────────────────────────────────────────────────
blue "════════════════════════════════════════════════════"
total=$((PASS + FAIL))
if [ "$FAIL" -eq 0 ]; then
  green " ✓ All $total assertions passed — pipeline_ref_pattern gate verified"
else
  red " ✗ $FAIL/$total assertions failed"
  echo
  for err in "${ERRORS[@]}"; do red "   - $err"; done
  echo
  dim "  Probe output:"
  sed 's/^/    /' "$PROBE_OUT" 2>/dev/null || true
fi
blue "════════════════════════════════════════════════════"

[ "$FAIL" -eq 0 ]
