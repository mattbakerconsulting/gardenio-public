#!/usr/bin/env bash
# test-grafana-bridge.sh — grafana-bridge dashboard-sync + conflict-policy E2E
# (task ka-0000002319, coverage-backfill epic ka-0000000958).
#
# Closes the gap the grafana-bridge W2 landing left open (commits d2d8e2f /
# 7421e20, W2-impl-b): the Grafana<->beads dashboard sync — the beads→Grafana
# ConfigMap write path, the one-time backfill, the SSE-driven sync, and the
# Grafana→beads read-back with its three ConflictPolicy branches
# (beads-wins / grafana-wins / manual) — shipped with strong UNIT coverage
# (grafana_sync_test.go, grafana_readback_test.go, grafana_configmap_test.go,
# grafana_backfill_test.go) but NO end-to-end assertion wiring the pieces
# together the way production does. The only live grafana e2e that existed —
# test-grafana-iframe.sh — asserts X-Frame-Options embedding, and
# test-charts-hub.sh asserts the viewer's read proxies; neither touches the
# bridge's sync or conflict-resolution logic.
#
# ── Why a built-from-source probe, not a live-bridge port-forward ─────────────
#
# Two hard constraints shape this module — identical to test-gitlab-dark-
# provenance / test-jira-conversational:
#
#   1. The grafana-bridge read-back poll is OFF on the deployed fleet: on
#      staging (and prod) grafanaBridge.grafana.url is empty, so the bridge
#      runs WRITE-ONLY (beads→ConfigMap only) — there is no live Grafana→beads
#      reconcile loop to observe and the conflict-policy branches never fire
#      in-cluster. A port-forward can assert nothing about them.
#   2. The sync + conflict logic under test is all in-process Go (GrafanaSync,
#      BackfillDashboards, GrafanaReadback's three policy branches). Driving it
#      against a real shared daemon would require a live bridge + a live Grafana;
#      `kd serve` needs Postgres, so an isolated daemon is not cheap in a shard.
#
# So this module follows the EXACT precedent of cmd/gitlab-prov-probe /
# cmd/jira-conv-probe / cmd/trigger-probe: it builds and runs
# cmd/grafana-bridge-probe, a TEST-ONLY binary that wires the REAL bridge
# components — the REAL GrafanaConfigMapWriter against a fake k8s clientset, the
# REAL GrafanaSync through the REAL SSE stream, and the REAL GrafanaReadback
# against a STUB Grafana httptest server — plus an in-process mock daemon that
# RECORDS every bead write. It is hermetic: ZERO pods, ZERO shared-daemon writes,
# ZERO live Grafana — so it is green today and is a HARD gate.
#
# ── Legs (each a hard sub-gate; any FAIL reddens the shard) ───────────────────
#
#   1  write-sync    A dashboard bead + model doc, driven through the REAL SSE
#                    stream, provisions a grafana_dashboard="1" ConfigMap carrying
#                    the model + stamps dashboard_hash/source=beads; an unchanged
#                    re-sync is a no-op (the hash loop-breaker). [acceptance:
#                    "the bridge writes the Grafana ConfigMap"]
#   2  backfill      BackfillDashboards imports N JSON models as type=dashboard
#                    beads (idempotent on uid), each with a model doc, then a
#                    catch-up sync provisions each into Grafana.
#   3  grafana-wins  A TRUE conflict (both sides diverged) resolves grafana-wins:
#                    the bead model becomes the Grafana model, the dropped beads
#                    model is archived, source=grafana. [acceptance: "asserts
#                    read-back reconciles per … grafana-wins"]
#   4  beads-wins    The same conflict under beads-wins keeps the beads model and
#                    archives the divergent Grafana model. [acceptance:
#                    "… beads-wins"]
#   5  manual        The same conflict under manual opens a type=decision bead
#                    (blocks-linked to the dashboard) and touches NEITHER live
#                    model. [acceptance: "manual conflict policy (manual opening a
#                    decision bead)"]
#   6  loop-breaker  A read-back poll where Grafana's model matches the bead hash
#                    (only a volatile version bump) is a no-op — the loop-breaker
#                    that stops a beads→Grafana write echoing back as a phantom edit.
#
# ── Skip-clean contract ──────────────────────────────────────────────────────
#
# The Go toolchain + cmd/grafana-bridge-probe source are present in every e2e
# shard container (the shard `use:` list includes build-grafana-bridge-probe, and
# go is installed). If either is somehow absent the module SKIPS CLEANLY (exit 0)
# with a LOUD banner — an absent toolchain must never red the suite — mirroring
# the gitlab-dark-provenance / jira-conversational skip_all("loud") contract.
# When present, every leg is a HARD assertion.
#
# Usage:
#   ./gardenio/tests/e2e/scripts/test-grafana-bridge.sh

set -uo pipefail

# ── Config ────────────────────────────────────────────────────────
CONTROLLER_DIR="${CONTROLLER_DIR:-gardenio/controller}"
RUN_ID="${RUN_ID:-$$}"
PROBE_BIN=""

PASS=0
FAIL=0
ERRORS=()

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
  red " SKIP grafana-bridge (LOUD): $why"
  red " This run did NOT assert the grafana-bridge sync + conflict-policy stack"
  red " (beads→ConfigMap write path, backfill, SSE-driven sync, and the"
  red " beads-wins / grafana-wins / manual read-back branches). A green suite"
  red " here proves NOTHING about that stack. The e2e shard is built with a Go"
  red " toolchain + the cmd/grafana-bridge-probe source (build-grafana-bridge-probe"
  red " task) — if this fires in CI, that wiring regressed."
  red "════════════════════════════════════════════════════"
  if [ -n "${E2E_SKIP_SENTINEL:-}" ]; then printf '%s\n' "$*" >"$E2E_SKIP_SENTINEL" 2>/dev/null || true; fi  # skip visibility (ka-0000000989)
  exit 0
}

PATH_INSTALLED=0
cleanup() {
  # Only remove a temp binary we built; never a PATH-installed one.
  [ "$PATH_INSTALLED" != "1" ] && [ -n "$PROBE_BIN" ] && rm -f "$PROBE_BIN" 2>/dev/null || true
}
trap cleanup EXIT

echo "════════════════════════════════════════════════════"
echo " grafana-bridge dashboard-sync + conflict-policy E2E (ka-0000002319)"
echo " write-sync -> backfill -> grafana-wins -> beads-wins -> manual -> loop-breaker"
echo "════════════════════════════════════════════════════"

# ── 1. Resolve the probe binary ─────────────────────────────────────
# Prefer a binary pre-built +
# smoke-checked onto PATH (the gitlab-prov-probe / jira-conv-probe precedent,
# with per-source caching). Fall back to an in-shell build from source for local
# dev and for robustness if the pre-built binary is absent. Only when BOTH the
# pre-built binary is absent AND no Go toolchain/source is available do we skip
# clean.
if command -v grafana-bridge-probe >/dev/null 2>&1; then
  PROBE_BIN="$(command -v grafana-bridge-probe)"
  # PROBE_BIN is on PATH, not a temp file — do not let cleanup() rm it.
  PATH_INSTALLED=1
  pass "using pre-built grafana-bridge-probe ($PROBE_BIN)"
else
  PATH_INSTALLED=0
  command -v go >/dev/null 2>&1 \
    || skip_all "no pre-built grafana-bridge-probe on PATH and no go toolchain to build it"
  [ -d "$CONTROLLER_DIR/cmd/grafana-bridge-probe" ] \
    || skip_all "no pre-built grafana-bridge-probe on PATH and cmd/grafana-bridge-probe missing under $CONTROLLER_DIR"
  blue "[build] cmd/grafana-bridge-probe (from source)"
  PROBE_BIN="$(mktemp "/tmp/grafana-bridge-probe.${RUN_ID}.XXXXXX")"
  BUILD_LOG="/tmp/grafana-bridge-probe-build-${RUN_ID}.log"
  if ( cd "$CONTROLLER_DIR" && CGO_ENABLED=0 go build -o "$PROBE_BIN" ./cmd/grafana-bridge-probe/ ) >"$BUILD_LOG" 2>&1; then
    pass "cmd/grafana-bridge-probe built from source"
  else
    fail "build grafana-bridge-probe" "go build failed: $(tail -8 "$BUILD_LOG")"
    # A build failure IS a hard failure — the probe is the whole module.
    blue "════════════════════════════════════════════════════"
    red " ✗ 1/1 assertions failed (probe build)"
    for err in "${ERRORS[@]}"; do red "   - $err"; done
    blue "════════════════════════════════════════════════════"
    exit 1
  fi
fi
echo

# ── 2. Run the probe; capture per-leg lines ─────────────────────────
# The probe prints one "LEG <name>: PASS|FAIL <detail>" line per leg and exits
# non-zero if any leg failed. We tee its output, map each leg line to a pass/fail
# assertion, and require ALL six expected legs to be present (a missing leg is a
# failure, so a probe that silently drops a leg cannot pass).
blue "[run] driving write-sync -> backfill -> grafana-wins -> beads-wins -> manual -> loop-breaker"
PROBE_OUT="/tmp/grafana-bridge-probe-${RUN_ID}.out"
probe_rc=0
"$PROBE_BIN" >"$PROBE_OUT" 2>/tmp/grafana-bridge-probe-${RUN_ID}.err || probe_rc=$?

EXPECTED_LEGS="write-sync backfill grafana-wins beads-wins manual loop-breaker"
for leg in $EXPECTED_LEGS; do
  line=$(grep -E "^LEG ${leg}: " "$PROBE_OUT" 2>/dev/null | head -1 || true)
  if [ -z "$line" ]; then
    fail "leg $leg" "no result line emitted by the probe (leg dropped)"
    continue
  fi
  detail="${line#LEG ${leg}: }"
  case "$detail" in
    PASS*) pass "leg $leg — ${detail#PASS }" ;;
    FAIL*) fail "leg $leg" "${detail#FAIL }" ;;
    *)     fail "leg $leg" "unrecognized result: $detail" ;;
  esac
done

# Belt-and-suspenders: the probe's own exit code must agree with our parse. If
# the probe exited non-zero but every leg line parsed PASS (should be
# impossible), treat it as a failure so a probe-level error is never masked.
if [ "$probe_rc" -ne 0 ] && [ "$FAIL" -eq 0 ]; then
  fail "probe exit code" "probe exited $probe_rc but all parsed legs passed — probe-level error (stderr: $(tail -3 /tmp/grafana-bridge-probe-${RUN_ID}.err 2>/dev/null))"
fi
echo

# ── Summary ─────────────────────────────────────────────────────────
blue "════════════════════════════════════════════════════"
total=$((PASS + FAIL))
if [ "$FAIL" -eq 0 ]; then
  green " ✓ All $total assertions passed"
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
