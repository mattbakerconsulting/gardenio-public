#!/bin/bash
# Run one shard of the gardenio E2E module suite.
#
# Required env:
#   E2E_SHARD        Shard index (0..TOTAL_SHARDS-1)
#   TOTAL_SHARDS     Total number of shards (e.g. 4)
#   NAMESPACE        K8s namespace (e.g. gardenio-e2e-0)
#   BEADS_TOKEN      Beads auth token (loaded by the CI job that runs the shard)
#
# Optional env:
#   ONLY             Comma-separated module names; run only these
#   SKIP             Comma-separated module names to skip
#   KEEP             Keep namespace after tests (unused here, passed through)
#   SMOKE_ONLY       If "1", run only the 5-module smoke gating subset
#                    (tls-certs, stack-health, agent-crew-lifecycle,
#                    beads-crud, mail-ready) regardless of E2E_SHARD/
#                    TOTAL_SHARDS. Used by e2e.yml's live-smoke job
#                    (ka-0000004434).
#
# Shard assignment strategy:
#   1. Heavy modules (>50s historical runtime) are explicitly pinned via
#      the `shard_for()` case statement so bin-packing is predictable.
#   2. All other modules are assigned by cksum(name) % TOTAL_SHARDS.
#      This is deterministic, distributes evenly over the long tail,
#      and needs no maintenance when modules are added or removed.
#
# Hermetic scripts NOT wired into run-shard.sh (intentional — they
# don't talk to kubectl/beads/network and are run separately):
#   [shard-isolation-lint: exclusion-list BEGIN — the shard-isolation lint
#    asserts every `- test-...sh` bullet between these markers exists on disk
#    and that every scripts/ test script on disk is either wired below or
#    listed here. Keep bullets machine-parseable: `#   - test-<name>.sh <note>`.]
#   - test-lib-retry.sh      Unit test for lib-retry.sh.
#   - test-lib-kd-create.sh  Unit test for lib-kd-create.sh (gd-hLx8BizDTr).
#   - test-lib-gvisor-node.sh  Cluster-free unit test for lib-gvisor-node.sh
#                              (gVisor-poc warm-node gate + bounded retry,
#                              decision ka-0000000122). Run in CI by
#                              checks.yml script-selftests.
#   - test-lib-gvisor-forensics.sh  Cluster-free unit test for
#                              lib-gvisor-forensics.sh (Rig R2 canary forensic
#                              auto-capture, bead ka-0000006948). Run in CI by
#                              checks.yml script-selftests.
#   - test-shard-isolation.sh  Partition-invariant lint for this file
#                              (gd-UHoecPuYe0). See e2e/README.md.
#   - test-shard-budget-guard.sh  Hermetic unit test for the budget-aware
#                              retry guard in this file (ka-0000003834): lifts
#                              shard_seconds_remaining()/retry_fits_budget() and
#                              drives them across the wall-clock budget boundary,
#                              no kubectl/beads. Run in CI by checks.yml
#                              script-selftests.
#   - test-skip-visibility-lint.sh  Hermetic gate for the skip-sentinel
#                              protocol (ka-0000000989): asserts run_module still
#                              classifies a self-skip (exit 0 + E2E_SKIP_SENTINEL)
#                              as SKIPPED not PASSED, and that every skip-family
#                              exit-0 site writes the sentinel — so green-by-skip
#                              cannot silently return. Lifts run_module and drives
#                              it against fake modules; no kubectl/beads. Run in
#                              CI by checks.yml script-selftests.
#   - test-claudeless-scenario-isolated.sh   Standalone claudeless
#                              lifecycle repro (ka-0000002034); local
#                              mock PTY env, no kubectl/beads/namespace.
#   - test-claudeless-multi-nudge-isolated.sh  Companion multi-nudge
#                              diagnostic (ka-0000002034); standalone,
#                              no kubectl/beads.
#   - test-concurrency-isolation.sh  Static $NAMESPACE-hygiene lint over
#                              the other test scripts; no kubectl/beads.
#   - test-outbox-shape-selftest.sh  Hermetic, credless self-test of the 1:1
#                              GBFSM outbox CREATE-shape (ka-0000003147); sources
#                              lib-outbox-shape.sh, no kubectl/beads. Run in CI
#                              by checks.yml script-selftests.
#   - test-lib-outbox-rig.sh   Cluster-free, credless unit test for
#                              lib-outbox-rig.sh's outbox_rig_guard (ka-0000003147);
#                              no kubectl/beads. Run in CI by checks.yml
#                              script-selftests.
#   - test-terraform-prewarm-cache.sh  Sanity check that the agent-image
#                              terraform provider prewarm (terraform-providers.sh
#                              -> /opt/terraform-plugin-cache) populates a
#                              non-empty, CONSUMABLE cache (ka-0000002834). No
#                              kubectl/beads. --contract-only runs credless on
#                              bare ubuntu; the full populate+consume proof
#                              runs against a freshly-built cache. No CI job
#                              runs it today (39s, pulls providers over the
#                              network; see checks.yml script-selftests).
#   - test-hermetic-set-lint.sh  (static lint over HERMETIC_MODULES; runs
#     standalone from the shell-hermetic job, never inside a shard)
#   - test-lane-lib.sh         Hermetic unit test for lane.sh, the live-namespace
#                              e2e lane driver (gd-WBz2qt9ADZ): lease staleness,
#                              calver pick, SKIP= derivation, report merge and
#                              acquire/sweep against a fake kubectl. Run in CI by
#                              checks.yml script-selftests.
# The E2E direction gardener's drift check (gardenio/docs/e2e-direction.md)
# subtracts these from the script count. If you add another hermetic
# script, update the gardener doc's drift command alongside this list.
#
# Cluster scripts wired OUT-OF-BAND (intentional — real E2Es that DO talk
# to kubectl/beads, but are dispatched by another harness, not by a
# run_module call here). They are NOT hermetic; the drift audit must still
# account for them so they are not re-flagged as orphans:
#   - test-gvisor-substrate-ready.sh        ┐ gVisor suite — run by hand against
#   - test-gvisor-freeze-interrupt.sh       │ the gvisor-poc node, gated on
#   - test-gvisor-renudge-thaw.sh           │ GVISOR_E2E. No CI job dispatches
#   - test-gvisor-abort-races-completion.sh ┘ them today.
#   - test-pf-supervisor.sh    Port-forward supervisor check, run via the
#                              `test-pf-supervisor` target in gardenio/Makefile.
#   - test-litellm-budget-cap.sh  Deliberate-runaway acceptance for the per-CI
#                              scoped LiteLLM virtual key (ka-0000000823,
#                              R4-COST-1/SEC-1): mints a tiny-budget key, drives
#                              real /v1/messages turns until a budget 429, then
#                              deletes it. NOT sharded — needs a real LiteLLM
#                              proxy + LITELLM_MASTER_KEY and spends real (tiny)
#                              money, so it is run on demand against a live proxy,
#                              not in the credless sharded suite.
#   [shard-isolation-lint: exclusion-list END]
# These don't belong in the sharded suite (gvisor needs a dedicated runsc
# substrate; pf-supervisor is a Makefile-driven unit-ish check). If you wire
# one INTO a shard or add another out-of-band cluster script, update this
# list and the e2e-direction.md drift command together.

set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
NAMESPACE="${NAMESPACE:-gardenio-e2e}"
BEADS_SVC="${BEADS_SVC:-${NAMESPACE}-beads}"
# In SMOKE_ONLY (and MIGRATION_ACCEPTANCE, and HERMETIC_ONLY) mode the runner
# is single-shard by design (one task runs the full named subset), so
# E2E_SHARD/TOTAL_SHARDS default to 0/1 and shard gating is disabled below. In
# normal sharded e2e runs both must be set. Each subset mode must be honored
# HERE (not only in the module-selection block far below) or the
# `${E2E_SHARD:?...}` guard aborts the whole gate before the subset logic runs
# (ka-0000003481 review finding).
if [ "${SMOKE_ONLY:-}" = "1" ] || [ "${MIGRATION_ACCEPTANCE:-}" = "1" ] || [ "${HERMETIC_ONLY:-}" = "1" ]; then
  E2E_SHARD="${E2E_SHARD:-0}"
  TOTAL_SHARDS="${TOTAL_SHARDS:-1}"
else
  E2E_SHARD="${E2E_SHARD:?E2E_SHARD must be set}"
  TOTAL_SHARDS="${TOTAL_SHARDS:-4}"
fi

echo "════════════════════════════════════════════════════"
echo " E2E Shard $E2E_SHARD / $TOTAL_SHARDS  (namespace=$NAMESPACE)"
echo "════════════════════════════════════════════════════"

# ── Shard wall-clock budget (ka-0000003834) ─────────────────────────
# Captured as early as possible so the setup below (context banner, the
# deployment/controller waits, port-forward establishment) all counts against
# the same budget the shard's job timeout measures. The budget-aware retry guard
# in run_module() uses this to DECLINE an in-shard retry that cannot fit in the
# time remaining before the job timeout SIGKILLs the shard (exit 137).
#
# WHY: run_module()'s connectivity-flap retry re-runs the WHOLE module (plus a
# ~180s connectivity-recovery poll) with no awareness of how much wall-clock the
# shard has left. On run 024eb6ea (tag 2026.190.4, shard 1) litellm-cost-
# tracking (~455s) failed late after a mid-module flap; the harness then waited
# ~2min for the recycled litellm pod + kd to recover and STARTED a full re-run —
# at which point the 20m task timeout fired MID-RETRY. The SIGKILL killed the
# shard before its remaining 6 modules ran OR its results were recorded, and the
# release gate saw a timed-out shard → red. Declining a retry that can't fit lets
# the ONE slow module's failure stand — with per-module attribution — while the
# rest of the shard finishes cleanly and records results, instead of SIGKILLing
# the whole shard and losing every not-yet-run module's result. (The failing
# module still fails the run -- there is no automatic quarantine -- but a clean
# per-module verdict beats a shard-wide timeout that blames every module.)
#
# SHARD_BUDGET_SECONDS defaults to 1500 (=25m), the budget the shard timeout
# was sized to (the 20m→25m bump — ka-0000008024 — moved both from 1200/20m
# together). Keep it below any wall-clock timeout the shard's CI job carries.
# Overridable via env so the timeout bump and the hermetic test can drive it
# without edits.
SHARD_START_TS=$(date +%s)
SHARD_BUDGET_SECONDS="${SHARD_BUDGET_SECONDS:-1500}"
# Wall-clock reserved for post-module work (JUnit XML generation, staging pod-log
# dump on failure, port-forward teardown) plus the few seconds between job
# start and SHARD_START_TS. Never start a retry that would leave less than this.
SHARD_BUDGET_MARGIN_SECONDS="${SHARD_BUDGET_MARGIN_SECONDS:-90}"
# Worst-case wall-clock the connectivity-recovery poll inside a retry can burn
# before the module even re-runs (the 36×5s loop below). Budgeted in full so a
# never-recovering flap cannot make the retry overrun the wall.
SHARD_RETRY_POLL_BUDGET_SECONDS="${SHARD_RETRY_POLL_BUDGET_SECONDS:-180}"
# Percent of SHARD_BUDGET_SECONDS at which shard_budget_warn() emits a one-shot
# "approaching the wall" line (ka-0000008024). A creeping per-module regression
# then shows up in the log BEFORE the job timeout SIGKILLs the shard (exit
# 137) rather than only being visible as an after-the-fact timeout. Purely
# informational — it never fails a module or the shard. Overridable for the
# hermetic test. SHARD_BUDGET_WARNED gates it to a single emission per shard.
SHARD_BUDGET_WARN_PCT="${SHARD_BUDGET_WARN_PCT:-80}"
SHARD_BUDGET_WARNED=0

if [ "${HERMETIC_ONLY:-}" = "1" ]; then
  echo ""
  echo "HERMETIC_ONLY=1: skipping cluster preflight (deployment waits, port-forwards,"
  echo "  token load, kd connectivity gate). Modules run against local processes only."
  PF_SUPERVISED=0
  PF_PID=""
  BRIDGE_PF_PID=""
  BRIDGE_AVAILABLE=0
  # Owned by the guarded block below, so it must be seeded here too. Hermetic
  # mode reached ensure_bridge_port_forward's `[ "$BRIDGE_AVAILABLE" = 1 ] ||
  # return 0` before the read at its next branch, so this was load-bearing by
  # accident: under `set -u`, anyone adding a branch above that early return
  # turned HERMETIC_ONLY into an unbound-variable abort that reads as a module
  # bug rather than a preamble bug. Seed all five, not four.
  BRIDGE_PF_SUPERVISED=0
  export BRIDGE_HTTP_URL=""
  export BEADS_TOKEN=""
  # The guarded block also exports these three. Clearing them is not reachable
  # today -- no module in HERMETIC_MODULES references any of them -- but an
  # ambient BEADS_HTTP_URL in a developer's shell would otherwise survive into
  # a run advertised as daemon-less, and a module that later grows a beads call
  # would silently write to whatever that points at instead of failing loudly.
  export BEADS_HTTP_URL=""
  export BEADS_AUTH_TOKEN=""
  export BEADS_DAEMON_TOKEN=""
fi

if [ "${HERMETIC_ONLY:-}" != "1" ]; then

# ── Context banner (informational; never fail the shard on banner errors) ──
( set +e; set +o pipefail
echo ""
echo "════════════════════════════════════════════════════════════════════"
echo "  E2E TEST CONTEXT  (shard $E2E_SHARD)"
echo "════════════════════════════════════════════════════════════════════"
DEPLOYED_IMAGE=$(kubectl -n "$NAMESPACE" get deploy "${NAMESPACE}-beads" \
  -o jsonpath='{.spec.template.spec.containers[0].image}' 2>/dev/null || echo "unknown")
DEPLOYED_TAG="${DEPLOYED_IMAGE##*:}"
CURRENT_SHA=$(git rev-parse HEAD 2>/dev/null || echo "unknown")
TAG_ON_SHA=$(git tag --points-at "$CURRENT_SHA" 2>/dev/null | awk '/^20[0-9]{2}\./ { print; exit }')
if [ -n "$TAG_ON_SHA" ]; then
  EXPECTED_TAG="${TAG_ON_SHA}-rc"
else
  EXPECTED_TAG="main-${CURRENT_SHA:0:7}"
fi
echo "  $NAMESPACE running:    $DEPLOYED_TAG"
echo "  this dispatch targets:  $EXPECTED_TAG  (commit ${CURRENT_SHA:0:7})"
if [ "$DEPLOYED_TAG" = "$EXPECTED_TAG" ]; then
  echo "  ✓ APP CODE MATCHES — tests exercise exactly what this commit builds."
else
  echo "  ⚠ APP CODE DOES NOT MATCH THIS DISPATCH."
  echo "  Tests will run against APP CODE from: $DEPLOYED_TAG"
  echo "  Tests use TEST SCRIPTS from:          $EXPECTED_TAG  ($CURRENT_SHA)"
fi
echo "════════════════════════════════════════════════════════════════════"
echo ""
true ) || true

# ── Wait for backing deployments to be Available ────────────────────
# e2e-setup declares the stack Ready, but pods can churn (restart/reschedule)
# between that check and shard start — on run ea3d230f17a645c2b9a89a03bbf1cd9e
# (release 2026.114.6) both the beads and slack-bridge pods went Pending ~2:50
# after stack-Ready, and the first module in each shard failed with exit 1 in
# ~40ms because the port-forward pointed at a non-ready pod. See gd-vAZehAPNS4.
kubectl -n "$NAMESPACE" wait --for=condition=Available \
  "deployment/${BEADS_SVC}" --timeout=90s 2>&1 || \
  echo "WARNING: beads deployment not Available within 90s; continuing"

# ── Wait for the agent controller (pool reconciler) to be Available ──
# The warm-pool modules (pool-recycle, pool-manager, pool-assign-attribution)
# assert the controller mints/recycles prewarmed agents inside fixed windows
# (e.g. pool-recycle's 180s mint clock). Those clocks assume the controller's
# pool reconciler is already live when the module starts. In a COLD per-instance
# ephemeral namespace the *-agents Deployment can still be rolling at shard
# start (image pull + DB connect + first project-cache refresh), so the mint
# clock starts against a controller that has not yet reconciled — 0/5 minted,
# module times out, shard burns its 20m wall. e2e-setup's wait-for-stack-ready
# gate covers this ONCE before shards start, but the controller can churn
# between that gate and shard start exactly like beads does (gd-vAZehAPNS4), so
# we re-confirm it here. Best-effort like the beads wait: a warm singleton ns
# satisfies it instantly; a genuinely-wedged controller still surfaces its own
# module failures downstream. Only shard 1 hosts the warm-pool modules, but the
# wait is cheap and the controller is a core dependency, so all shards run it.
# ka-0000006169.
CONTROLLER_DEPLOY="${NAMESPACE}-agents"
if kubectl -n "$NAMESPACE" get "deployment/${CONTROLLER_DEPLOY}" >/dev/null 2>&1; then
  kubectl -n "$NAMESPACE" wait --for=condition=Available \
    "deployment/${CONTROLLER_DEPLOY}" --timeout=120s 2>&1 || \
    echo "WARNING: controller deployment ${CONTROLLER_DEPLOY} not Available within 120s; continuing"
else
  echo "(controller deployment ${CONTROLLER_DEPLOY} not present — skipping readiness wait)"
fi

fi # HERMETIC_ONLY != 1 (context banner + deployment waits)

# ── Supervised port-forward primitive (ka-0000001942) ───────────────
# Bare `kubectl port-forward &` dies whenever kubectl itself exits — backing
# pod restart, SPDY stream error, transient API blip — and the old design
# only health-checked/repaired the tunnel at MODULE BOUNDARIES (ensure_*_
# port_forward in run_module, before each test runs). A kubectl death DURING
# a module — e.g. inside bridge-fleet-token-rotation's 23s TTL sleep — was
# therefore unrecoverable: the in-flight curl got HTTP 000 and the module
# failed even though the daemon was healthy. This is the confirmed shared
# root cause of the rotating single-module flake that quarantined 4 modules
# (decision gd-nU0jPKE3Za): on run f5899e1066d4 the beads tunnel logged
# "error: lost connection to pod" mid-sleep, bridge-fleet went 000×4, and the
# SAME module passed on captain retry. See bug ka-0000001639.
#
# pf-supervisor.sh (already used by test-local-mode.sh, gd-FtiTtZyuj2) keeps
# kubectl alive in a respawn loop with 1s backoff, self-healing WITHIN a
# module rather than only between modules. We back BOTH shared tunnels with
# it. The supervisor exits when its sentinel file is removed (cleanup trap).
PF_SUPERVISOR="${SCRIPT_DIR}/pf-supervisor.sh"
PF_SENTINEL="/tmp/pf-supervisor-beads-${E2E_SHARD}.sentinel"
BRIDGE_PF_SENTINEL="/tmp/pf-supervisor-bridge-${E2E_SHARD}.sentinel"

# spawn_supervised_pf <svc> <local-port> <target-port> <log> <sentinel>
# Launches pf-supervisor.sh detached (setsid so it survives signal cascades)
# and returns once it is running. Returns non-zero if the supervisor script
# is missing/non-executable so callers can fall back to a bare port-forward.
spawn_supervised_pf() {
  local svc="$1" lport="$2" tport="$3" logf="$4" sentinel="$5"
  [ -x "$PF_SUPERVISOR" ] || return 1
  # Clear any stale sentinel from a previous run so the supervisor starts fresh.
  rm -f "$sentinel"
  if command -v setsid >/dev/null 2>&1; then
    setsid -f bash "$PF_SUPERVISOR" "$NAMESPACE" "$svc" "$lport" "$logf" "$sentinel" "$tport"
  else
    bash "$PF_SUPERVISOR" "$NAMESPACE" "$svc" "$lport" "$logf" "$sentinel" "$tport" &
    disown 2>/dev/null || true
  fi
}

# ── Cleanup trap ────────────────────────────────────────────────────
# Backgrounded port-forwards hold the parent CI job open as child
# processes — if the script exits early (e.g. failed kd connectivity check
# below) without killing them, the job hangs for the full timeout
# before SIGKILL (exit 137). Observed on 2026.144.13 shards 0 and 1
# (gd-DwtVLCcT1g). Removing each supervisor's sentinel makes it kill its
# inner kubectl and exit cleanly; the PF_PID fallbacks cover the
# unsupervised path (and the legacy /tmp/*-pf-pid files).
cleanup_port_forwards() {
  local rc=$?
  rm -f "$PF_SENTINEL" "$BRIDGE_PF_SENTINEL" 2>/dev/null || true
  if [ -n "${PF_PID:-}" ]; then
    kill "$PF_PID" 2>/dev/null || true
  fi
  if [ -n "${BRIDGE_PF_PID:-}" ]; then
    kill "$BRIDGE_PF_PID" 2>/dev/null || true
  fi
  return "$rc"
}
trap cleanup_port_forwards EXIT

# dump_beads_pf_diag — surface why the beads tunnel never came up OR why LIST
# 500s while /v1/health is 200. The former harness fell straight from a silent
# PF poll-exhaustion into "kd connectivity check failed after 5 attempts" with
# zero diagnostics (observed on smoke of 2b846fff, gd-Rd0MfBgkae), making a
# transient tunnel/daemon stall impossible to triage from the CI log alone.
# This prints the pf-supervisor's own kubectl log, a direct /v1/health probe
# with the HTTP status, and the beads pod/endpoint state so the next failure is
# self-explaining.
#
# gd-tTgnUW3iy8: a NEW shape appeared where the tunnel is fine and /v1/health
# returns 200 (handleHealth only Ping()s the DB) but GET /v1/beads (LIST) 500s
# with "failed to list beads". /v1/health never runs the heavy LIST query, so a
# cold-emptyDir first-touch stall / statement-timeout / connection reset on the
# ephemeral pgvector backend surfaces on LIST while Ping sails through. The real
# SQL cause is logged ONLY in the beads pod (the daemon's "http 5xx root cause"
# WARN — kbeads http.go loggingMiddleware) and the ephemeral is reaped at
# TTL=45m, so it was previously unrecoverable. Capture the three artifacts that
# discriminate the failure families on the NEXT occurrence:
#   1. beads pod logs (the "http 5xx root cause" WARN carries handler +
#      sanitized error: "column ... does not exist" vs "context deadline
#      exceeded"/"statement timeout" vs "connection reset by peer").
#   2. a raw LIST probe with status + time_total (slow => cold-I/O/timeout;
#      fast + PG error => schema/SQL).
#   3. a pg_stat_activity snapshot (transient-vs-persistent: a lock / running
#      ANALYZE/autovacuum/backfill held for the retry window vs a clean DB).
dump_beads_pf_diag() {
  echo "──── beads port-forward diagnostics (gd-Rd0MfBgkae / gd-tTgnUW3iy8) ────"
  echo "[diag] pf-supervisor log (/tmp/kd-pf-beads-${E2E_SHARD}.log):"
  tail -n 30 "/tmp/kd-pf-beads-${E2E_SHARD}.log" 2>/dev/null | sed 's/^/[diag]   /' \
    || echo "[diag]   (no pf-supervisor log found)"
  echo "[diag] direct probe http://localhost:19090/v1/health:"
  local code
  code=$(curl -s --max-time 3 -o /dev/null -w '%{http_code}' \
    "http://localhost:19090/v1/health" 2>/dev/null || echo "000")
  echo "[diag]   HTTP $code (000 = tunnel not listening / connection refused)"

  # Raw LIST probe (gd-tTgnUW3iy8): the connectivity check IS a LIST, so probe
  # /v1/beads directly to record BOTH the HTTP status AND time_total. Duration
  # alone splits cold-I/O/timeout (slow) from schema/SQL (fast). The response
  # body is the same sanitized "failed to list beads" the client already saw —
  # the discriminating SQL cause is only in the pod log below — so we keep just
  # the status + timing here. An X-Request-Id lets the client 500 be
  # cross-referenced to the exact pod-side WARN.
  echo "[diag] direct probe http://localhost:19090/v1/beads?status=open (the LIST that 500s):"
  local list_req_id list_metrics
  list_req_id="smoke-diag-${E2E_SHARD}-$$"
  list_metrics=$(curl -s --max-time 10 -o /dev/null \
    -w 'HTTP %{http_code} in %{time_total}s' \
    -H "Authorization: Bearer ${BEADS_TOKEN:-}" \
    -H "X-Request-Id: ${list_req_id}" \
    "http://localhost:19090/v1/beads?status=open&limit=1" 2>/dev/null \
    || echo "HTTP 000 (probe failed)")
  echo "[diag]   ${list_metrics}  (X-Request-Id=${list_req_id})"

  echo "[diag] beads pods:"
  kubectl -n "$NAMESPACE" get pods -l app.kubernetes.io/component=beads \
    -o wide 2>&1 | sed 's/^/[diag]   /' || true
  echo "[diag] beads service endpoints:"
  kubectl -n "$NAMESPACE" get endpoints "$BEADS_SVC" 2>&1 | sed 's/^/[diag]   /' || true

  # Beads pod logs (gd-tTgnUW3iy8) — the LOAD-BEARING artifact. The daemon
  # emits one WARN "http 5xx root cause" per 5xx with handler + the sanitized
  # wrapped error chain. grep for it (and the LIST handler / error prefixes) so
  # a busy access log cannot push the discriminating line out of a tail window.
  # --prefix labels each pod (there is normally one replica, but be robust),
  # and a plain tail fallback prints recent lines when no 5xx WARN is present
  # (e.g. the daemon never saw the request → a tunnel/routing problem instead).
  echo "[diag] beads pod logs — 5xx root cause / LIST errors (last 5m):"
  local beads_5xx
  beads_5xx=$(kubectl -n "$NAMESPACE" logs -l app.kubernetes.io/component=beads \
    --prefix --tail=2000 --since=5m 2>/dev/null \
    | grep -iE 'http 5xx root cause|handleListBeads|list beads:|count beads:|scan beads:' \
    | tail -n 40 || true)
  if [ -n "$beads_5xx" ]; then
    echo "$beads_5xx" | sed 's/^/[diag]   /'
  else
    echo "[diag]   (no 5xx-root-cause WARN in beads logs — daemon may not have"
    echo "[diag]    seen the request; last 15 log lines follow)"
    kubectl -n "$NAMESPACE" logs -l app.kubernetes.io/component=beads \
      --tail=15 2>/dev/null | sed 's/^/[diag]   /' || true
  fi

  # Postgres activity snapshot (gd-tTgnUW3iy8) — transient-vs-persistent
  # discriminator. A lock / long-running ANALYZE / autovacuum / backfill held
  # for the ~10s retry window explains a deterministic 5/5 failure; a clean
  # snapshot points at a per-query cold-I/O stall. Best-effort: skips silently
  # when the embedded postgres pod / psql is unavailable (e.g. external RDS).
  local pg_pod
  pg_pod=$(kubectl -n "$NAMESPACE" get pods -l app.kubernetes.io/component=postgres \
    -o jsonpath='{.items[0].metadata.name}' 2>/dev/null || true)
  if [ -n "$pg_pod" ]; then
    echo "[diag] postgres pg_stat_activity (non-idle) on $pg_pod:"
    kubectl -n "$NAMESPACE" exec "$pg_pod" -c postgres -- \
      psql -U postgres -d beads -c \
      "SELECT state, wait_event_type, wait_event, now()-query_start AS run_for, left(query,80) AS query FROM pg_stat_activity WHERE state <> 'idle' AND pid <> pg_backend_pid() ORDER BY query_start;" \
      2>&1 | sed 's/^/[diag]   /' || echo "[diag]   (pg_stat_activity query failed)"
  fi
  echo "────────────────────────────────────────────────────────"
}

# beads_schema_lost — detect the emptyDir-postgres-restart schema-wipe shape
# (gd-JLL0DzwFWC). Ephemeral E2E postgres runs with persistence.enabled=false, so
# its data dir is an emptyDir (helm/gardenio/templates/postgres/statefulset.yaml).
# The schema is created ONCE by the Helm pre-upgrade migrate Job. If the postgres
# pod restarts mid-E2E (OOM at its 1Gi limit / spot eviction / Karpenter
# consolidation) it comes back with an EMPTY data dir — no `configs` table, no
# schema at all — and the migrate Job (a Helm hook) does NOT re-run. Every bead
# LIST then 500s with `relation "configs" does not exist` because
# validateFilterTypes -> resolveTypeConfig reads `configs` on every list. This
# reds every module and the whole release gate (measured: golden tag dark ~12h,
# 231.1/.2/.3 all gate=false).
#
# The tell that discriminates this from a tunnel flap: /v1/health is 200 (it only
# Ping()s the DB, which connects fine to an EMPTY db) while GET /v1/beads 500s.
# Returns 0 (true) when that exact shape holds so the caller can trigger a beads
# restart — the daemon re-runs migrations at startup (postgres.go New ->
# runMigrations), rebuilding the schema on the reset emptyDir with no operator or
# migrate-Job involvement.
beads_schema_lost() {
  # /v1/health must be UP (tunnel + DB reachable) …
  curl -sf --max-time 3 -o /dev/null "http://localhost:19090/v1/health" 2>/dev/null || return 1
  # … while a LIST 500s specifically on the missing schema. Match the sanitized
  # daemon error rather than any non-2xx so a genuine transient (timeout, 502)
  # does NOT masquerade as schema loss and trigger an unnecessary restart.
  local body
  body=$(curl -s --max-time 8 \
    -H "Authorization: Bearer ${BEADS_TOKEN:-}" \
    "http://localhost:19090/v1/beads?status=open&limit=1" 2>/dev/null || true)
  case "$body" in
    *"does not exist"*|*"failed to validate type filter"*|*"failed to list beads"*) return 0 ;;
    *) return 1 ;;
  esac
}

# recover_beads_schema — restart the beads deployment so its startup
# runMigrations rebuilds the schema wiped by an emptyDir-postgres restart
# (gd-JLL0DzwFWC), then wait until a LIST actually succeeds. Bounded by
# SHARD_RETRY_POLL_BUDGET_SECONDS so it can never overrun the retry allotment
# retry_fits_budget() reserves. Best-effort: returns 0 as soon as a LIST works,
# 1 if the schema never comes back within budget (caller still retries the
# module, preserving prior "never silently mask a down environment" behaviour).
recover_beads_schema() {
  echo "Beads schema lost (postgres emptyDir restart, gd-JLL0DzwFWC): /v1/health 200 but LIST 500s on missing schema — restarting beads to re-run startup migrations"
  kubectl -n "$NAMESPACE" rollout restart "deployment/${BEADS_SVC}" 2>&1 | sed 's/^/  /' || true
  kubectl -n "$NAMESPACE" rollout status "deployment/${BEADS_SVC}" --timeout=120s 2>&1 | sed 's/^/  /' || true
  ensure_port_forward
  local _deadline _i
  _deadline=$(( $(date +%s) + SHARD_RETRY_POLL_BUDGET_SECONDS ))
  _i=0
  while :; do
    _i=$((_i + 1))
    if kd list --status=open --limit 1 --json >/dev/null 2>&1; then
      echo "Beads schema recovered after ${_i} probe(s); a LIST now succeeds"
      return 0
    fi
    if [ "$(date +%s)" -ge "$_deadline" ]; then
      echo "WARNING: beads schema still missing after ~${SHARD_RETRY_POLL_BUDGET_SECONDS}s of restart+poll; retrying module anyway"
      return 1
    fi
    sleep 5
    ensure_port_forward
  done
}

if [ "${HERMETIC_ONLY:-}" != "1" ]; then

# ── Port-forward to beads daemon ────────────────────────────────────
# Poll-until-ready instead of a fixed sleep. Fixed `sleep 3` was insufficient
# on 2026.144.13 (gd-DwtVLCcT1g): port-forward did not finish establishing
# until ~5s, so the kd connectivity check below saw "connection refused" and
# exited the script before any module ran.
PF_SUPERVISED=0
PF_PID=""
if spawn_supervised_pf "$BEADS_SVC" 19090 8080 \
     "/tmp/kd-pf-beads-${E2E_SHARD}.log" "$PF_SENTINEL"; then
  PF_SUPERVISED=1
  echo "Beads port-forward supervised via pf-supervisor.sh (sentinel $PF_SENTINEL)"
else
  echo "WARNING: pf-supervisor.sh unavailable; falling back to bare port-forward"
  kubectl -n "$NAMESPACE" port-forward "svc/${BEADS_SVC}" 19090:8080 &
  PF_PID=$!
  echo "$PF_PID" > /tmp/pf-pid
fi
# Poll until /v1/health answers. The beads Deployment can report Available at
# almost the same instant this loop starts (observed on smoke of 2b846fff,
# gd-Rd0MfBgkae: beads "condition met" and the PF spawn logged 7ms apart), and
# /v1/health pings the DB — so a freshly-Available pod behind a still-
# establishing kubectl tunnel can need more than the former 15s before it
# answers 200. Widen to 30 attempts (~30s) and, critically, make exhaustion
# LOUD: the previous loop fell through silently, so a never-ready tunnel only
# surfaced downstream as "kd connectivity check failed" with no clue whether
# the tunnel or the daemon was at fault.
PF_BEADS_READY=0
for _i in $(seq 1 30); do
  if curl -sf --max-time 2 -o /dev/null "http://localhost:19090/v1/health" 2>/dev/null; then
    echo "Port-forward established on :19090 (attempt $_i)"
    PF_BEADS_READY=1
    break
  fi
  if [ "$PF_SUPERVISED" = 0 ] && [ -n "$PF_PID" ] && ! kill -0 "$PF_PID" 2>/dev/null; then
    echo "WARNING: port-forward pid $PF_PID died before tunnel came up; restarting"
    kubectl -n "$NAMESPACE" port-forward "svc/${BEADS_SVC}" 19090:8080 &
    PF_PID=$!
    echo "$PF_PID" > /tmp/pf-pid
  fi
  sleep 1
done
if [ "$PF_BEADS_READY" = 0 ]; then
  echo "WARNING: beads /v1/health never answered 200 on :19090 after 30 attempts"
  echo "         the kd connectivity check below will likely fail — dumping PF state:"
  dump_beads_pf_diag || true
fi

# ── Port-forward to slack-bridge (escalation web API) ───────────────
# Per-commit ephemeral overlays don't always include slack-bridge (the prod
# overlay does, but the test overlay may not). When the Service is absent
# the kubectl port-forward errors immediately with NotFound, and without
# the short-circuit below ensure_bridge_port_forward+run_module's 3-attempt
# retry burns ~12s per module on a tunnel that can never come up.
# Observed: shard-3 spent ~6-8min of its 15-min budget on this churn and
# timed out before reaching the final modules. See gd-I7Y7AfZAtU.
BRIDGE_SVC="${NAMESPACE}-slack-bridge"
BRIDGE_PF_PID=""
BRIDGE_PF_SUPERVISED=0
BRIDGE_AVAILABLE=0
if kubectl -n "$NAMESPACE" get "svc/${BRIDGE_SVC}" >/dev/null 2>&1; then
  BRIDGE_AVAILABLE=1
  kubectl -n "$NAMESPACE" wait --for=condition=Available \
    "deployment/${BRIDGE_SVC}" --timeout=90s 2>&1 || \
    echo "WARNING: slack-bridge deployment not Available within 90s; continuing"
  # Supervised like the beads tunnel (ka-0000001942): a bridge kubectl death
  # mid-module would otherwise leave BRIDGE_HTTP_URL dead until the next
  # module boundary (HTTP 000 for escalation-*/slack-* tests, gd-3RxELxfbCe).
  if spawn_supervised_pf "$BRIDGE_SVC" 19091 8090 \
       "/tmp/bridge-pf-${E2E_SHARD}.log" "$BRIDGE_PF_SENTINEL"; then
    BRIDGE_PF_SUPERVISED=1
    echo "Bridge port-forward supervised via pf-supervisor.sh (sentinel $BRIDGE_PF_SENTINEL)"
  else
    echo "WARNING: pf-supervisor.sh unavailable; falling back to bare bridge port-forward"
    kubectl -n "$NAMESPACE" port-forward "svc/${BRIDGE_SVC}" 19091:8090 &
    BRIDGE_PF_PID=$!
    echo "$BRIDGE_PF_PID" > /tmp/bridge-pf-pid
  fi
  # Poll-until-ready (see beads-PF rationale above). gd-DwtVLCcT1g.
  for _i in 1 2 3 4 5 6 7 8 9 10 11 12 13 14 15; do
    if curl -sf --max-time 2 -o /dev/null "http://localhost:19091/healthz" 2>/dev/null; then
      echo "Bridge port-forward established on :19091 (attempt $_i)"
      break
    fi
    sleep 1
  done
  export BRIDGE_HTTP_URL="http://localhost:19091"
else
  echo "WARNING: slack-bridge Service '${BRIDGE_SVC}' not found in namespace '${NAMESPACE}'"
  echo "  Skipping bridge port-forward setup and disabling bridge tunnel checks."
  echo "  Bridge-dependent modules (escalation-*, slack-*) will run without BRIDGE_HTTP_URL."
  export BRIDGE_HTTP_URL=""
fi

# Token is either in env (passed by caller) or on disk from load-token task
if [ -z "${BEADS_TOKEN:-}" ] && [ -f /tmp/beads-token ]; then
  BEADS_TOKEN="$(cat /tmp/beads-token)"
fi
: "${BEADS_TOKEN:?BEADS_TOKEN must be set or /tmp/beads-token must exist}"
export BEADS_TOKEN
export BEADS_AUTH_TOKEN="$BEADS_TOKEN"
export BEADS_DAEMON_TOKEN="$BEADS_TOKEN"
export BEADS_HTTP_URL="http://localhost:19090"

# kbeads server enforces non-empty operated_by since ka-0000005772 (commit
# 310a72b48). Without this, every test script that calls `kd create` /
# `gb agent start` without an explicit --operated-by 400s with "operator
# could not be resolved". Set once at the shard entrypoint so all child
# kd/gb invocations inherit it. See gd-804pz4vYOA.
#
# Since ka-0000002601 the resolver requires a well-formed human-shape
# operator id — "slack:U<alnum>" or "oidc:<local>@<domain>" — so the
# previous "ci:e2e-shard-N" value is rejected (HTTP 400) and every write
# fails. Use an oidc-shape fixture address that survives the sanitizer.
# See ka-0000006816.
export BEADS_OPERATED_BY="${BEADS_OPERATED_BY:-oidc:e2e-shard-${E2E_SHARD}@example.com}"

echo "kd: $(which kd), gb: $(which gb)"
echo "BEADS_HTTP_URL=$BEADS_HTTP_URL, token length: ${#BEADS_TOKEN}"
# Retry the connectivity check briefly: even with the tunnel poll above,
# the first kd call can transiently 502 if the daemon is still warming up.
# gd-DwtVLCcT1g.
_kd_ok=0
for _i in 1 2 3 4 5; do
  if kd list --status=open --json >/dev/null 2>&1; then
    _kd_ok=1
    kd list --status=open --json 2>&1 | head -3
    break
  fi
  echo "kd connectivity check attempt $_i failed; retrying..."
  sleep 2
done
if [ "$_kd_ok" = 0 ] && beads_schema_lost; then
  # The db lost its schema before this shard even started running modules
  # (emptyDir-postgres restart between deploy and shard start, gd-JLL0DzwFWC).
  # Rebuild it by restarting beads rather than hard-failing the whole shard.
  recover_beads_schema || true
  if kd list --status=open --json >/dev/null 2>&1; then
    _kd_ok=1
    kd list --status=open --json 2>&1 | head -3
  fi
fi
if [ "$_kd_ok" = 0 ]; then
  echo "kd connectivity check failed after 5 attempts"
  # Surface the actual kd error (the loop above swallowed it) plus the full
  # tunnel/daemon state so this stops being an undebuggable transient.
  echo "  last kd error:"
  kd list --status=open --json 2>&1 | head -5 | sed 's/^/    /' || true
  dump_beads_pf_diag || true
  exit 1
fi

fi # HERMETIC_ONLY != 1 (beads/bridge port-forwards, token load, kd connectivity gate)

export GB_BIN="gb"
export KD_BIN="kd"
# "e2e-rwx" is a stable identifier, not a reference to RWX CI:
# beadsapi/e2e_account.go matches it to classify e2e accounts. Do not rename.
export KD_ACTOR="e2e-rwx"
export NAMESPACE
export BEADS_SVC

JUNIT_DIR="$PWD/junit-results-${E2E_SHARD}"
mkdir -p "$JUNIT_DIR"
RESULTS_JSONL="$JUNIT_DIR/e2e-results.jsonl"
: > "$RESULTS_JSONL"

TESTS_PASSED=0
TESTS_FAILED=0
TESTS_SKIPPED=0

# ── Port-forward hygiene ────────────────────────────────────────────
# Under pf-supervisor.sh the inner kubectl is respawned automatically with 1s
# backoff, so repair here just means "wait for the tunnel to come back" — we
# must NOT kill/relaunch kubectl ourselves (that would race the supervisor and
# double-bind the port). Only the unsupervised fallback path does the old
# kill+restart dance. ka-0000001942.
ensure_port_forward() {
  if curl -sf --max-time 3 -o /dev/null "http://localhost:19090/v1/health" 2>/dev/null; then
    return 0
  fi
  if [ "$PF_SUPERVISED" = 1 ]; then
    echo "Beads tunnel unreachable; waiting up to 10s for pf-supervisor to heal..."
    for _ in 1 2 3 4 5 6 7 8 9 10; do
      if curl -sf --max-time 2 -o /dev/null "http://localhost:19090/v1/health" 2>/dev/null; then
        echo "Beads tunnel recovered (supervisor)"
        return 0
      fi
      sleep 1
    done
    echo "WARNING: beads tunnel still unreachable after supervisor wait"
    return 0
  fi
  # Unsupervised fallback: kill and relaunch the bare port-forward ourselves.
  echo "Port-forward unreachable (pid $PF_PID), restarting..."
  kill "$PF_PID" 2>/dev/null || true
  wait "$PF_PID" 2>/dev/null || true
  kubectl -n "$NAMESPACE" port-forward "svc/${BEADS_SVC}" 19090:8080 &
  PF_PID=$!
  echo "$PF_PID" > /tmp/pf-pid
  sleep 2
  echo "Port-forward restarted (pid $PF_PID)"
}

# Same health-check + restart pattern for the slack-bridge tunnel (19091).
# Separate from the beads tunnel so either can be repaired independently.
# Tests that use BRIDGE_HTTP_URL (escalation-slack-notify, etc.) fail with
# HTTP 000 when this tunnel dies silently mid-suite — see gd-3RxELxfbCe.
ensure_bridge_port_forward() {
  # No-op when slack-bridge was not present at startup. Avoids the 267-cycle
  # NotFound churn observed in shard-3 timeouts (gd-I7Y7AfZAtU).
  [ "$BRIDGE_AVAILABLE" = 1 ] || return 0
  if curl -sf --max-time 3 -o /dev/null "http://localhost:19091/healthz" 2>/dev/null; then
    return 0
  fi
  if [ "$BRIDGE_PF_SUPERVISED" = 1 ]; then
    echo "Bridge tunnel unreachable; waiting up to 10s for pf-supervisor to heal..."
    for _ in 1 2 3 4 5 6 7 8 9 10; do
      if curl -sf --max-time 2 -o /dev/null "http://localhost:19091/healthz" 2>/dev/null; then
        echo "Bridge tunnel recovered (supervisor)"
        return 0
      fi
      sleep 1
    done
    echo "WARNING: bridge tunnel still unreachable after supervisor wait"
    return 0
  fi
  # Unsupervised fallback: kill and relaunch the bare port-forward ourselves.
  echo "Bridge port-forward unreachable (pid $BRIDGE_PF_PID), restarting..."
  kill "$BRIDGE_PF_PID" 2>/dev/null || true
  wait "$BRIDGE_PF_PID" 2>/dev/null || true
  kubectl -n "$NAMESPACE" port-forward "svc/${BRIDGE_SVC}" 19091:8090 &
  BRIDGE_PF_PID=$!
  echo "$BRIDGE_PF_PID" > /tmp/bridge-pf-pid
  sleep 2
  echo "Bridge port-forward restarted (pid $BRIDGE_PF_PID)"
}

# shard_seconds_remaining — wall-clock left in the shard's job budget.
# Echoes SHARD_BUDGET_SECONDS - (now - SHARD_START_TS); can go negative (already
# over budget), which retry_fits_budget treats as "no room". ka-0000003834.
shard_seconds_remaining() {
  local now
  now=$(date +%s)
  echo $(( SHARD_BUDGET_SECONDS - (now - SHARD_START_TS) ))
}

# retry_fits_budget <first-attempt-duration-seconds> — decide whether an in-shard
# retry can complete before the job timeout SIGKILLs the shard (exit 137).
# Returns 0 (fits) or 1 (does not fit). A retry costs, worst case, the ~180s
# connectivity-recovery poll PLUS a full module re-run (estimated at the first
# attempt's own duration), and we must still leave SHARD_BUDGET_MARGIN_SECONDS
# for the post-module JUnit/log/teardown work. If that sum exceeds the wall-clock
# remaining, DECLINE the retry — the single module failure stands (with clean
# per-module attribution) rather than cascading into a shard-wide timeout that
# loses every not-yet-run module and reds the release gate anyway. ka-0000003834.
retry_fits_budget() {
  local first_duration="$1"
  local remaining need
  remaining=$(shard_seconds_remaining)
  need=$(( SHARD_RETRY_POLL_BUDGET_SECONDS + first_duration + SHARD_BUDGET_MARGIN_SECONDS ))
  if [ "$remaining" -lt "$need" ]; then
    return 1
  fi
  return 0
}

# shard_budget_warn — emit a ONE-SHOT warning the first time cumulative shard
# wall-clock crosses SHARD_BUDGET_WARN_PCT of SHARD_BUDGET_SECONDS. Called after
# every module completes so a creeping per-module regression is visible in the
# log BEFORE the job timeout fires (exit 137), not only as an after-the-fact
# SIGKILL. Purely informational: never fails a module or the shard, and never
# emits more than once (SHARD_BUDGET_WARNED). Complements retry_fits_budget,
# which reacts to the SAME budget only at a failure's retry decision — this
# fires on the happy path too. ka-0000008024.
shard_budget_warn() {
  [ "$SHARD_BUDGET_WARNED" = 0 ] || return 0
  local elapsed threshold remaining
  elapsed=$(( $(date +%s) - SHARD_START_TS ))
  threshold=$(( SHARD_BUDGET_SECONDS * SHARD_BUDGET_WARN_PCT / 100 ))
  if [ "$elapsed" -ge "$threshold" ]; then
    SHARD_BUDGET_WARNED=1
    remaining=$(shard_seconds_remaining)
    echo "⚠ SHARD BUDGET: shard $E2E_SHARD has used ${elapsed}s of its ${SHARD_BUDGET_SECONDS}s wall-clock budget (≥${SHARD_BUDGET_WARN_PCT}%, ~${remaining}s left before the job timeout SIGKILLs it). If modules remain, this shard is at risk of an exit-137 timeout — rebalance heavy modules (ka-0000001993) or bump the timeout (ka-0000008024)."
  fi
}

# json_str — emit a JSON string literal (quoted, with the six characters JSON
# requires escaping handled) for an arbitrary shell value. Used to embed a
# free-text skip reason into the results JSONL without a python round-trip.
# ka-0000000989.
json_str() {
  local s="$1"
  s="${s//\\/\\\\}"   # backslash first
  s="${s//\"/\\\"}"   # double-quote
  s="${s//	/\\t}"    # tab
  s="${s//$'\r'/\\r}"
  s="${s//$'\n'/\\n}"
  printf '"%s"' "$s"
}

run_module() {
  local name="$1"
  shift
  # The harness is not hermetic even when the module is: without this guard a
  # hermetic module still pays two port-forward acquisitions and a 3x2s curl
  # of a tunnel that will never exist. _pf_ok=1 also disables the
  # environmental-retry gate below, which is correct -- there is no
  # environment to flap.
  local _pf_ok=1
  if [ "${HERMETIC_ONLY:-}" != "1" ]; then
    ensure_port_forward
    ensure_bridge_port_forward
    _pf_ok=0
    for _pf_i in 1 2 3; do
      if curl -sf --max-time 2 -o /dev/null "http://localhost:19090/v1/health" 2>/dev/null; then
        _pf_ok=1
        break
      fi
      echo "Tunnel unreachable before $name (attempt $_pf_i/3), restarting..."
      ensure_port_forward
    done
    if [ "$_pf_ok" = 0 ]; then
      echo "WARNING: tunnel still unreachable after 3 attempts, running $name anyway"
    fi
  fi
  # Same for the bridge tunnel: some modules (escalation-*, slack-*) need
  # it and would otherwise fail with HTTP 000 if the tunnel drops here.
  # When the bridge Service is absent from the overlay, skip the check
  # entirely so we don't waste ~6s per module hammering a missing service
  # (gd-I7Y7AfZAtU). _bpf_ok=1 disables the preflight retry below.
  local _bpf_ok=1
  if [ "$BRIDGE_AVAILABLE" = 1 ]; then
    _bpf_ok=0
    for _bpf_i in 1 2 3; do
      if curl -sf --max-time 2 -o /dev/null "http://localhost:19091/healthz" 2>/dev/null; then
        _bpf_ok=1
        break
      fi
      echo "Bridge tunnel unreachable before $name (attempt $_bpf_i/3), restarting..."
      ensure_bridge_port_forward
    done
    if [ "$_bpf_ok" = 0 ]; then
      echo "WARNING: bridge tunnel still unreachable after 3 attempts, running $name anyway"
    fi
  fi
  echo ""
  echo "════════════════════════════════════════════════════"
  echo " Module: $name  (shard $E2E_SHARD)"
  echo "════════════════════════════════════════════════════"
  local rc=0
  local start_ts
  start_ts=$(date +%s)
  # ── Skip-sentinel protocol (ka-0000000989) ──────────────────────────
  # A module that self-skips (prerequisite binary absent, opt-in unset, …)
  # exits 0 — which is load-bearing (the gvisor suite and other modules are
  # also run DIRECTLY, where exit!=0 == failure, so a skip
  # MUST stay exit 0). But a bare exit 0 was previously recorded as MODULE
  # PASSED, indistinguishable from a real pass: 0 assertions ran, yet the
  # module counted toward the green tally the release gate is built on. The
  # confirmed live instance: test-claudemore-hooks (a HARD shard-0 gate)
  # skip_cleans on EVERY CI run because no `claude` binary is ever installed
  # (install-coop-claudeless provisions coop+claudeless, not claude), so it
  # has been green-by-skip — asserting nothing — while prod deploy is E2E-gated.
  #
  # Fix: hand each module a per-invocation sentinel path via E2E_SKIP_SENTINEL.
  # The module's skip helpers (skip_clean/skip_all/skip_optin_off/…) write their
  # reason to that path right before `exit 0`; run_module then classifies an
  # exit-0-with-sentinel as SKIPPED rather than PASSED. The sentinel is a side
  # channel, so the exit code the direct dispatchers read is unchanged, and the
  # env var is unset outside run_module so standalone module runs are unaffected.
  local skip_sentinel
  skip_sentinel="$(mktemp "${TMPDIR:-/tmp}/e2e-skip-${name}.XXXXXX")"
  rm -f "$skip_sentinel"   # absence == "did not skip"; the module (re)creates it iff it skips
  export E2E_SKIP_SENTINEL="$skip_sentinel"
  # ── Captain targeted-retry verification hook (ka-0000000510) ─────────
  # When CAPTAIN_POISON_MODULE=<module> is set, that module fails the
  # FIRST time it runs in this shell, then passes on every subsequent
  # attempt. Used to prove the Captain retry path end-to-end without
  # depending on a real flake. The sentinel lives in /tmp which persists
  # across retry attempts within the same CI job. No-op when the env
  # var is unset (default).
  if [ -n "${CAPTAIN_POISON_MODULE:-}" ] && [ "$name" = "$CAPTAIN_POISON_MODULE" ]; then
    local poison_sentinel="/tmp/captain-poison-${name}.fired"
    if [ ! -f "$poison_sentinel" ]; then
      echo "CAPTAIN_POISON_MODULE=$name set and sentinel missing — planting failure" >&2
      touch "$poison_sentinel"
      rc=1
    else
      echo "CAPTAIN_POISON_MODULE=$name set but sentinel exists — running normally (retry path)" >&2
      bash "$@" || rc=$?
    fi
  else
    bash "$@" || rc=$?
  fi
  local end_ts
  end_ts=$(date +%s)
  local duration=$((end_ts - start_ts))
  # Connectivity-aware retry. Two distinct flake shapes both warrant one
  # bounded retry against a recovered environment — distinguished only by
  # WHEN the tunnel was unhealthy:
  #
  #   (a) PRE-FLIGHT flap (gd-vAZehAPNS4): the tunnel was already known-bad
  #       going in (_pf_ok/_bpf_ok == 0) AND the module died fast (<3s) — it
  #       never ran an assertion, it just hit a closed port-forward. On
  #       run ea3d230f17a6 the beads pod went Pending between deploy-ready and
  #       shard start; mail-nudge-delivery (shard 3) and stack-health (shard 0)
  #       both failed in ~46ms with no assertions run.
  #
  #   (b) MID-MODULE flap (gd-BaFiyhnLUV): the pre-flight passed, the module
  #       ran for >3s, then the single-replica beads pod's readinessProbe
  #       flapped (a slow /v1/health DB-ping under cold POC I/O briefly drops
  #       the ONLY backing address out of the Service EndpointSlice) or the
  #       kubectl port-forward SPDY stream blipped — so an in-flight `kd`
  #       call mid-test got "kd reachable ✗" / HTTP 000 and the module exited
  #       non-zero. The pre-flight snapshot (_pf_ok=1) looks healthy, so the
  #       (a) guard never fires and the flap is misclassified as an assertion
  #       failure. This was the shared signature that flapped 7 shard-1
  #       modules on RC 2026.166.6/.7 (trigger-engine-smoke, secret-reconciler,
  #       escalation-crud, pool-recycle, …). MR !4331 (beads readinessProbe
  #       failureThreshold 3->6) hardens the pod, but a flap inside the 60s
  #       tolerance window still surfaces as a transient in-test failure that
  #       a single retry against the recovered tunnel absorbs.
  #
  # Detect (b) with a POST-mortem probe: on any module failure, re-check the
  # beads tunnel + the actual kd endpoint (the bridge too, when present). If
  # connectivity is unhealthy AFTER the run, the failure is environmental, not
  # an assertion — retry once regardless of duration. We deliberately bound
  # this to ONE retry so a genuinely broken assertion that also happens to race
  # a flap cannot loop.
  local _post_conn_ok=1
  if [ "${HERMETIC_ONLY:-}" != "1" ] && [ "$rc" -ne 0 ]; then
    if ! curl -sf --max-time 3 -o /dev/null "http://localhost:19090/v1/health" 2>/dev/null \
       || ! kd list --status=open --limit 1 --json >/dev/null 2>&1; then
      _post_conn_ok=0
    elif [ "$BRIDGE_AVAILABLE" = 1 ] \
       && ! curl -sf --max-time 3 -o /dev/null "http://localhost:19091/healthz" 2>/dev/null; then
      _post_conn_ok=0
    fi
  fi
  local retried_after_preflight=0
  local retried_mid_module=0
  local retry_declined_budget=0
  # Only consider a retry when the failure looks environmental (fast pre-flight
  # flap, or beads/kd unreachable post-run). ka-0000003834 adds a THIRD gate on
  # top of that classification: even a genuine flap only earns a retry if the
  # retry can finish inside the shard's wall-clock budget. A slow module failing
  # late (litellm ~455s on shard 1) would otherwise trigger a full re-run that
  # the job timeout SIGKILLs MID-RETRY — losing every not-yet-run module and
  # reding the release gate. Declining here lets that one failure stand (with
  # clean per-module attribution) while the remaining modules run and record
  # results. The retry-shape classification (pre-flight vs mid-module) is
  # computed first so it can label the declined-retry log line accurately.
  if [ "$rc" -ne 0 ] && { { [ "$duration" -lt 3 ] && { [ "$_pf_ok" = 0 ] || [ "$_bpf_ok" = 0 ]; }; } || [ "$_post_conn_ok" = 0 ]; } \
     && ! retry_fits_budget "$duration"; then
    retry_declined_budget=1
    local _remaining
    _remaining=$(shard_seconds_remaining)
    echo "Module $name failed in ${duration}s (looks environmental) but only ${_remaining}s of the ${SHARD_BUDGET_SECONDS}s shard budget remain — a retry needs ~$(( SHARD_RETRY_POLL_BUDGET_SECONDS + duration + SHARD_BUDGET_MARGIN_SECONDS ))s (poll+rerun+margin), which would overrun the job timeout (exit 137)."
    echo "Declining the in-shard retry so the remaining modules still run and record results (ka-0000003834). The single failure stands with per-module attribution and reds the release unless quarantined (ka-0000000440)."
  fi
  if [ "$rc" -ne 0 ] && [ "$retry_declined_budget" = 0 ] && { { [ "$duration" -lt 3 ] && { [ "$_pf_ok" = 0 ] || [ "$_bpf_ok" = 0 ]; }; } || [ "$_post_conn_ok" = 0 ]; }; then
    [ "$_post_conn_ok" = 0 ] && retried_mid_module=1
    if [ "$_post_conn_ok" = 0 ]; then
      echo "Module $name failed in ${duration}s and beads/kd is unreachable post-run (mid-module connectivity flap) — waiting for deployments + retrying once"
    else
      echo "Module $name failed in ${duration}s with unhealthy pre-flight — waiting for deployments + retrying once"
    fi
    kubectl -n "$NAMESPACE" wait --for=condition=Available \
      "deployment/${BEADS_SVC}" --timeout=60s 2>&1 || true
    if [ "$BRIDGE_AVAILABLE" = 1 ]; then
      kubectl -n "$NAMESPACE" wait --for=condition=Available \
        "deployment/${BRIDGE_SVC}" --timeout=60s 2>&1 || true
    fi
    ensure_port_forward
    ensure_bridge_port_forward
    # Gate the retry on ACTUAL kd reachability, not just deployment-Available
    # (ka-0000006320). The .166.19–.167.2 flake storm was dominated by a node
    # going NoExecute-tainted mid-run: the backing single-replica beads pod
    # reschedules onto a fresh node, which routinely takes >60s — longer than
    # the wait above + the 10s ensure_port_forward budget. The old code then
    # re-ran the module UNCONDITIONALLY into a still-dead tunnel, so the retry
    # aborted on "Cannot reach kd server" and got logged MODULE FAILED —
    # turning a pure infra flap into a release-gating red. .167.3's
    # trigger-engine-smoke proved a retry against a RECOVERED tunnel heals, so
    # poll /v1/health (and the bridge healthz when present) up to ~180s before
    # re-running. If it never recovers within budget we still run anyway (prior
    # behaviour), so a genuinely-down environment is not silently masked.
    #
    # ka-0000003834: bound the poll by WALL-CLOCK (SHARD_RETRY_POLL_BUDGET_SECONDS)
    # rather than a fixed 36-iteration count. The old `seq 1 36` + `sleep 5` gave
    # 180s of *sleeps* but the per-iteration ensure_port_forward/curl overhead
    # pushed real wall-clock past 180s — which would blow the exact allotment
    # retry_fits_budget() reserves for this poll. A wall-clock deadline makes the
    # poll cost provably ≤ SHARD_RETRY_POLL_BUDGET_SECONDS, so the budget guard's
    # arithmetic is sound.
    _poll_deadline=$(( $(date +%s) + SHARD_RETRY_POLL_BUDGET_SECONDS ))
    _conn_i=0
    while :; do
      _conn_i=$((_conn_i + 1))
      if curl -sf --max-time 3 -o /dev/null "http://localhost:19090/v1/health" 2>/dev/null \
         && { [ "$BRIDGE_AVAILABLE" != 1 ] \
              || curl -sf --max-time 3 -o /dev/null "http://localhost:19091/healthz" 2>/dev/null; }; then
        echo "Connectivity recovered after ${_conn_i} probe(s); retrying $name"
        break
      fi
      if [ "$(date +%s)" -ge "$_poll_deadline" ]; then
        echo "WARNING: kd still unreachable after ~${SHARD_RETRY_POLL_BUDGET_SECONDS}s; retrying $name anyway"
        break
      fi
      sleep 5
      ensure_port_forward
      ensure_bridge_port_forward
    done
    # The poll above gates on /v1/health, which is a bare DB Ping — it returns
    # 200 even against a SCHEMA-LESS db after an emptyDir-postgres restart, so it
    # would happily "recover" and retry the module straight into a db where every
    # LIST 500s (gd-JLL0DzwFWC). Before retrying, detect that specific shape and
    # rebuild the schema by restarting beads (its startup runMigrations re-applies
    # every migration). No-op on a normal tunnel flap where the schema is intact.
    if beads_schema_lost; then
      recover_beads_schema || true
    fi
    retried_after_preflight=1
    rc=0
    start_ts=$(date +%s)
    bash "$@" || rc=$?
    end_ts=$(date +%s)
    duration=$((end_ts - start_ts))
  fi
  # Did the module self-skip? (exit 0 AND it wrote the skip sentinel, ka-0000000989)
  local _skipped=0
  local _skip_reason=""
  if [ "$rc" -eq 0 ] && [ -f "$skip_sentinel" ]; then
    _skipped=1
    _skip_reason="$(head -c 500 "$skip_sentinel" 2>/dev/null | tr '\n' ' ')"
  fi
  rm -f "$skip_sentinel" 2>/dev/null || true
  unset E2E_SKIP_SENTINEL
  if [ "$_skipped" = 1 ]; then
    # SKIPPED is NOT a pass: 0 assertions ran, so it must not inflate the green
    # tally the release gate rests on. Recorded as a distinct status:skip row
    # (the JUnit generator already renders it as <skipped/> and excludes it from
    # the pass count; the shard summary prints TESTS_SKIPPED). The "MODULE
    # SKIPPED:" stdout line lets an aggregate summary report skips separately
    # from passes.
    echo "MODULE SKIPPED: $name${_skip_reason:+ (${_skip_reason})}"
    TESTS_SKIPPED=$((TESTS_SKIPPED + 1))
    echo "{\"name\":\"$name\",\"status\":\"skip\",\"rc\":0,\"duration_s\":$duration,\"shard\":$E2E_SHARD,\"retried_after_preflight\":$retried_after_preflight,\"skip_reason\":$(json_str "$_skip_reason")}" >> "$RESULTS_JSONL"
    shard_budget_warn
    return 0
  fi
  if [ "$rc" -eq 0 ]; then
    echo "MODULE PASSED: $name"
    TESTS_PASSED=$((TESTS_PASSED + 1))
    echo "{\"name\":\"$name\",\"status\":\"pass\",\"rc\":0,\"duration_s\":$duration,\"shard\":$E2E_SHARD,\"retried_after_preflight\":$retried_after_preflight}" >> "$RESULTS_JSONL"
  else
    local fail_reason="assertion"
    if [ "$retry_declined_budget" = 1 ]; then
      echo "MODULE FAILED: $name (exit $rc, environmental-looking retry DECLINED — insufficient shard budget, ka-0000003834)"
      fail_reason="retry_declined_budget"
    elif [ "$retried_mid_module" = 1 ]; then
      echo "MODULE FAILED: $name (exit $rc, retried after mid-module connectivity flap)"
      fail_reason="midmodule_connectivity"
    elif [ "$retried_after_preflight" = 1 ]; then
      echo "MODULE FAILED: $name (exit $rc, retried after pre-flight failure)"
      fail_reason="preflight_failed"
    else
      echo "MODULE FAILED: $name (exit $rc)"
    fi
    TESTS_FAILED=$((TESTS_FAILED + 1))
    echo "{\"name\":\"$name\",\"status\":\"fail\",\"rc\":$rc,\"duration_s\":$duration,\"shard\":$E2E_SHARD,\"retried_after_preflight\":$retried_after_preflight,\"retried_mid_module\":$retried_mid_module,\"retry_declined_budget\":$retry_declined_budget,\"fail_reason\":\"$fail_reason\"}" >> "$RESULTS_JSONL"
  fi
  # Surface an approaching-the-wall warning once cumulative shard time crosses
  # the budget threshold, regardless of this module's pass/fail (ka-0000008024).
  shard_budget_warn
  return "$rc"
}

# ── Shard assignment ────────────────────────────────────────────────
# Heavy modules are explicitly pinned so bin-packing is predictable.
# Everything else hashes to a shard via cksum(name) % TOTAL_SHARDS.
#
# RUNTIME-WEIGHT REBALANCE (ka-0000001993). The pins below are bin-packed by
# MEASURED per-module wall-clock, not module COUNT. The prior count-balance
# (36/35/37/28 modules) hid a 5× runtime spread: shard 1 hit 836s (13.9m) while
# shard 2 idled at 166s (2.8m). Because run_module retries an environmentally-
# flaky module by re-running the WHOLE module, a single retry of the 363s
# pool-assign-attribution on the old shard 1 (13.9m base) would overrun the 20m
# shard timeout → exit-137 (the shard-timeout flake this rebalance fixes).
#
# Provenance: duration_s parsed from the e2e-results.jsonl / stdout of five
# 2026-08-04 release RCs (release runs 604d701e, d0283f7d, 8b32eea6,
# 429f8067, f3fe3da3, a486b553; pipelines 2026.216.9–.15). Weight = per-module
# MAX across those RCs (worst-case, absorbs cold-cache RCs). Only modules that
# ACTUALLY RUN on the release/scheduled fan-out carry weight; slack-*, the gVisor
# gates, local-mode, semantic-search all self-SKIP there (weight ~0).
#
# Result (release-path weights): base p95 per shard 8.9–9.5m (was 12.8–13.9m);
# worst single shard + a full heaviest-module retry ≤ 15.0m — ≥5m under the 25m
# task timeout on every shard. The parser + raw per-RC duration_s data live on
# bead ka-0000001993; after any re-pin, verify the partition is still exhaustive
# and disjoint with gardenio/tests/e2e/scripts/test-shard-isolation.sh.
#
# THREE hard constraints survive the rebalance and are NOT free to move:
#   1. config-* → shard 3 (atomic). Every config-* test creates unscoped
#      "<category>:global" beads (claude-settings/mcp/hooks/instructions) on the
#      shared daemon; ResolveConfigBeads merges across shards, so concurrent
#      config-* on two shards cross-pollute (gd-xG5dJItFru, gd-zzuzGsgOma).
#   2. slack-* → shard 2 (atomic + serialized). One test Slack app fans each
#      inbound event to EVERY connected bridge; two bridges on two shards would
#      cross-fire (ka-0000006398). Weight ~0 today (rig absent ⇒ SKIP-pass).
#   3. from-source Go probes → shard 0. Keeping the probes there confines their
#      build coupling to one shard and keeps the Go toolchain warm.
# credential-rotation + escalation-* are NO LONGER pinned with config-* — they
# never create unscoped global beads (credential-rotation uses project-scoped
# config:auth), so they are free to move for balance. Light modules not listed
# here hash — cksum(name)%4 spreads the sub-5s long tail evenly by construction.
shard_for() {
  local name="$1"
  case "$name" in
    # ── Shard 0 ───────────────────────────────────────────────────────
    # Runtime drivers: pool-recycle (247s) + session-spend-attribution (148s).
    # Stale-image prewarmed recycle / cycle-on-upgrade (gd-ppSVTZDNLt). Drives
    # the live pool reconciler through a simulated image-rev change against an
    # EPHEMERAL per-test project, so it never touches the real pool and is free
    # to sit apart from pool-assign-attribution (its old shard-1 co-tenant) —
    # splitting the two heaviest warm-pool modules across shards halves the
    # per-shard retry tax.
    pool-recycle)               echo 0; return ;;
    # Session spend attribution (gd-NU1IbgLk5v): a from-source `go test -run`
    # against the real controller reconciler + bridge usage consumer (hermetic —
    # httptest + embedded NATS, no pods). From-source Go module → shard 0 with
    # the other probes so the toolchain stays warm.
    session-spend-attribution)  echo 0; return ;;
    # MR-merge flow (~22s). Moved off shard 3 (was pinned there for convenience,
    # not pollution) to fill shard 0.
    mr-merge-flow)              echo 0; return ;;
    # advice-budget (~20s). Light-heavy filler for shard 0.
    advice-budget)              echo 0; return ;;
    # From-source Go probes (build-*-probe tasks ride shard 0's `use:` list):
    # jira-conversational (ka-0000003999), gitlab-dark-provenance (ka-0000005448),
    # grafana-bridge (ka-0000002319). Pinned together so the Go build toolchain
    # stays warm and the build tasks stay confined to one shard's use: list.
    jira-conversational)        echo 0; return ;;
    # jira-syncback-denybydefault (ka-0000005719): another from-source Go probe
    # (cmd/jira-syncback-probe, build-jira-syncback-probe rides shard 0's `use:`
    # list) exercising the sync-back deny-by-default allowlist + source:jira-dark
    # early-return. Pinned with the other jira probes to keep the Go toolchain
    # warm and the jira modules co-located.
    jira-syncback-denybydefault) echo 0; return ;;
    gitlab-dark-provenance)     echo 0; return ;;
    grafana-bridge)             echo 0; return ;;
    # MR-reopen drift sweep (ka-0000006088): another from-source Go probe
    # (cmd/mr-reopen-probe, build-mr-reopen-probe rides shard 0's `use:` list),
    # so it belongs with the probes that keep the Go toolchain warm. Light
    # (~5s): five sweep passes against the live daemon, no pods, no waits.
    mr-reopen-drift)            echo 0; return ;;
    # grafana-render-cli (ka-0000007821): drives the REAL `gb` binary
    # (build-gb rides EVERY shard's use: list) against Python-stdlib stub
    # daemon + renderer servers — hermetic, no pods, no tunnel. Pinned on
    # shard 0 with the other gb/toolchain-warm modules for a stable partition;
    # it needs no build-*-probe, only build-gb which is already present.
    grafana-render-cli)         echo 0; return ;;
    # verify-prod-images (ka-0000003773): drives the REAL `gb` binary (build-gb
    # rides EVERY shard's use: list) with --skip-cluster over hand-written
    # deploy-values fixtures — a PURE file scan, NO stub servers, NO cluster, NO
    # beads tunnel. Pinned on shard 0 with the other gb/toolchain-warm hermetic
    # modules (release-status, grafana-render-cli); it needs no build-*-probe,
    # only build-gb which is already present. Light (~1s): four deterministic
    # `gb verify-prod-images` runs. Self-SKIPs (loud) only if gb / python3 absent.
    verify-prod-images)         echo 0; return ;;
    # claudemore-replay-volatile-hash (ka-0000006222): from-source, curl-driven
    # probe of the REPLAY request-hash volatile-field stripping (cassette.go). It
    # builds `./cmd/claudemore/` from source and overlay-builds mutants in its
    # PROOF, so it pins on shard 0 with claudemore-hooks (cksum-lands on 0 and
    # builds the SAME cmd/claudemore) to keep that Go build cache warm and the
    # two claudemore-from-source modules co-located. Light (~5s incl. the record→
    # replay round-trip); needs no cluster/beads/tunnel, self-SKIPs (loud) if the
    # Go toolchain / curl / python3 is absent.
    claudemore-replay-volatile-hash) echo 0; return ;;
    # hook-prime-assignment (ka-0000007410): drives the REAL `gb` binary (build-gb
    # rides EVERY shard's use: list) against a REAL `kd show` over the shard's live
    # kbeads daemon, asserting the `gb hook prime` ## Assignment field-elision on a
    # seeded-then-CLOSED fixture agent bead (closed → never spawned by the
    # reconciler, zero-churn). Pinned on shard 0 with the other gb/toolchain-warm
    # modules: its anti-vacuity legs overlay-build ceiling mutants, so the warm Go
    # toolchain here is load-bearing. Light (~15s incl.
    # two mutant builds). Self-SKIPs (loud) if gb / kd / python3 / daemon absent.
    hook-prime-assignment)      echo 0; return ;;
    # jira-reply-mirror-link (ka-0000006808): drives the REAL `gb jira reply`
    # binary against Python-stdlib stub daemon + jira-bridge servers to assert
    # the MR !6704 reply->ticket-mirror triggered-by edge — hermetic, no pods,
    # no tunnel. Like grafana-render-cli it needs only build-gb (already on
    # every shard's use: list), so it pins on shard 0 with the gb/toolchain-warm
    # modules for a stable partition.
    jira-reply-mirror-link)     echo 0; return ;;
    # jira-backlog-pick (gd-LQcsEs2WQT): drives the REAL `gb jira backlog-pick`
    # binary against a Python-stdlib stub Jira JQL-search server to assert the
    # oldest-first pick, the live gardenio-triaged dedup (incl. the refinement-
    # label non-skip), MISSING surfacing, the > 50-key JQL batching, and the
    # read-only contract — hermetic, no pods, no tunnel. Like grafana-render-cli /
    # jira-reply-mirror-link it needs only build-gb (already on every shard's use:
    # list), so it pins on shard 0 with the gb/toolchain-warm jira modules.
    jira-backlog-pick)          echo 0; return ;;
    # grafana-directive (ka-0000007821): boots the REAL beads-viewer binary
    # (build-beads-viewer, added to shard 0's use: list) against a Python-stdlib
    # stub daemon to assert the :::grafana markdown directive renders an iframe
    # (GRAFANA_BASE_URL set) / a degraded note (unset). Hermetic — no pods, no
    # tunnel. Pinned to shard 0 because its build-beads-viewer prebuild rides
    # ONLY shard 0's use: list (like the from-source probes).
    grafana-directive)          echo 0; return ;;
    # plans-mr-overflow (ka-0000000730): boots the REAL beads-viewer binary
    # (build-beads-viewer, on shard 0's use: list) against the shard's live
    # kbeads daemon and renders seeded step->produces->merge-request graphs to
    # assert the /plans "+N MR · ✕ K" overflow FailedHiddenCount attention
    # signal. Pinned to shard 0 for the SAME reason grafana-directive is — the
    # build-beads-viewer prebuild rides ONLY shard 0's use: list, so the module
    # finds /usr/local/bin/beads-viewer instead of building from source.
    plans-mr-overflow)          echo 0; return ;;
    # escalation-auto-reaper (ka-0000006112): from-source cmd/escalation-reap-probe
    # driving the REAL EscalationReaper against stub GitLab + a mock daemon.
    # Hermetic, so it is free to sit anywhere — pinned here with the other
    # from-source probes to keep the Go toolchain warm and to let it pick up a
    # pre-built binary if a build task is ever added to shard 0's use: list.
    escalation-auto-reaper)     echo 0; return ;;
    # gitlab-pipeline-refpattern (ka-0000002595): from-source
    # cmd/gitlab-pipeline-trigger-probe driving the REAL GitLabTriggerSource +
    # TriggerEngine against a stub multi-ref GitLab server + a mock daemon, so a
    # gitlab-pipeline trigger's pipeline_ref_pattern is proven to GATE firing
    # (matching refs fire, non-matching refs do not — the runaway-guard leg).
    # Hermetic; pinned with the other from-source probes (build-gitlab-pipeline-
    # trigger-probe rides shard 0's use: list) to keep the Go toolchain warm.
    gitlab-pipeline-refpattern) echo 0; return ;;
    # gitlab-pipeline-minduration (ka-0000001474): from-source
    # cmd/gitlab-pipeline-minduration-probe driving the REAL GitLabTriggerSource +
    # TriggerEngine against a stub GitLab server (LIST omits duration, DETAIL
    # carries it — prod-accurate) + a mock daemon, so a gitlab-pipeline trigger's
    # min_duration ("slow pipeline") threshold is proven to GATE firing (at/over
    # fires, under does not — the runaway-guard leg; null/unknown fails closed).
    # Hermetic; pinned with the other from-source probes (build-gitlab-pipeline-
    # minduration-probe rides shard 0's use: list) to keep the Go toolchain warm.
    gitlab-pipeline-minduration) echo 0; return ;;
    # gitlab-webhook-deadman (ka-0000005729): a from-source `go test -run` against
    # the REAL GitLabWebhookMonitor.evaluate + the REAL webhook handler over a REAL
    # promhttp /metrics endpoint (hermetic — httptest only, no pods). Asserts the
    # served gardenio_gitlab_webhook_dark 0→1 flip with a real-delivery anti-vacuity
    # control, plus the served-names ↔ shipped-alert-expr coupling. From-source Go
    # module → shard 0 with the other probes to keep the Go toolchain warm.
    gitlab-webhook-deadman)     echo 0; return ;;
    # jira-webhook-deadman (ka-0000004554): the exact Jira mirror of the GitLab
    # sibling above — a from-source `go test -run` against the REAL
    # JiraWebhookMonitor.evaluate + the REAL JiraWebhookHandler over a REAL promhttp
    # /metrics endpoint (hermetic — httptest only, no pods). Asserts the served
    # gardenio_jira_webhook_dark 0→1 flip with a real-delivery anti-vacuity control,
    # plus the served-names ↔ shipped-alert-expr coupling. From-source Go module →
    # shard 0 with the other probes to keep the Go toolchain warm.
    jira-webhook-deadman)       echo 0; return ;;
    # slackbridge-respawn-phase-metrics (ka-0000003422): a from-source `go test -run`
    # against the REAL recordRespawnPhase over a REAL promhttp /metrics endpoint —
    # the same served-exposition + name-match-shipped-alert shape as the two deadman
    # siblings above (a live-bridge scrape is impossible; the metrics endpoint is the
    # un-forwarded :9093 and the *Vec is empty until a real respawn fires). Hermetic
    # (httptest only, no pods). From-source Go module → shard 0 with the other probes
    # to keep the Go toolchain warm.
    slackbridge-respawn-phase-metrics) echo 0; return ;;
    # slackmock-loop (gd-RhOuCeCYUC, epic gd-L1HXcTGzDV §C1): closes the slackmock
    # VALIDATION LOOP — builds + boots the ACTUAL slackmock binary, round-trips a
    # message through lib-slackmock.sh (SLACK_API / slackmock_find_message /
    # _call_count / _reset), then runs the in-process Go leg that drives the
    # bridge's REAL production Slack client (buildWebhookSlackClient honouring
    # SLACK_API_URL) against the fixture. Hermetic (localhost mock + httptest, no
    # pods, no cluster, no egress) so it is a HARD gate. NOT a slack-* live module
    # (needs no bridge/Socket-Mode/test-app), so it does NOT belong on shard 2's
    # single-bridge fan-out lane — it is a from-source Go probe → shard 0 with the
    # other probes to keep the Go toolchain warm.
    slackmock-loop)             echo 0; return ;;
    # stranded-recovered-sweep (gd-DMvno7uS3J, coverage-backfill epic
    # ka-0000000958): a from-source `go test -tags e2e -run` that builds the REAL
    # podmanager.K8sManager against the LIVE apiserver, plants husk pods in an
    # ISOLATED SCRATCH NAMESPACE, and calls ReconcileStrandedRecovered directly —
    # deterministic proof of the 5-min level-triggered stranded-pod recovery sweep
    # without waiting on (or depending on the version of) the deployed controller.
    # Needs the Go toolchain + cluster access, so it rides shard 0 with the other
    # from-source probes and the cluster-touching pool-recycle module. Light
    # (~3s) — the husks are unschedulable, so no pod ever runs. SKIPs LOUD without
    # a toolchain / reachable cluster.
    stranded-recovered-sweep)   echo 0; return ;;
    # jira-bridge + formula-dedup: co-located with the from-source jira probe so
    # the jira modules stay together and the toolchain is warm.
    jira-bridge)                echo 0; return ;;
    formula-dedup)              echo 0; return ;;
    # gVisor dedicated-dispatch gates. Weight-0 self-SKIPs on the standard
    # fan-out (GVISOR_E2E is never set here); each dedicated gardenio-gvisor-*
    # dispatch runs its script DIRECTLY, bypassing shard gating, so these pins
    # are runtime-neutral and kept on shard 0 purely for a stable partition.
    #   resume-latency (ka-0000006324/C7), userresponse-latency (ka-0000000107/C4),
    #   gvisor-rehydrate-ack (ka-0000006321), gvisor-freeze-interrupt (ka-0000004367),
    #   gb-stop-suspend (ka-0000000910).
    resume-latency)             echo 0; return ;;
    userresponse-latency)       echo 0; return ;;
    gvisor-rehydrate-ack)       echo 0; return ;;
    gvisor-resume-into-pool)    echo 0; return ;;
    gvisor-freeze-interrupt)    echo 0; return ;;
    gb-stop-suspend)            echo 0; return ;;
    # dark-jira-fence-pod (ka-0000000981): the POD leg of the dark-Jira write-token
    # fence — spawns a DARK + a NON-DARK agent pod against a token-injecting
    # ephemeral project and asserts scrubDarkForbiddenEnv strips JIRA_API_TOKEN
    # from the dark pod only. OPT-IN (DARK_FENCE_POD_E2E) like the other real-pod-
    # spawn modules, so it self-SKIPs on the standard fan-out (weight ~0) — pinned
    # to shard 0 with them purely for a stable partition.
    dark-jira-fence-pod)        echo 0; return ;;
    # probe-execution (ka-0000002117): `gb probe run` + script condition leaf.
    # OPT-IN (PROBE_EXECUTION_E2E / GVISOR_E2E) real-pod-spawn module, so it
    # self-SKIPs on the standard fan-out (weight ~0) — pinned to shard 0 with the
    # other opt-in pod-spawn modules purely for a stable partition.
    probe-execution)            echo 0; return ;;
    # ── Shard 1 ───────────────────────────────────────────────────────
    # The two heaviest cred modules, isolated together: cred-rotation-resume
    # (283s) + credential-rotation (225s). Both spend most of their time waiting
    # on SSE/NATS failover + coop resume polling (I/O-bound, not CPU), so pairing
    # them on a dedicated shard leaves the other three shards free to run the
    # CPU/pod-bound heavies (pool-*, claudeless) in parallel.
    cred-rotation-resume)       echo 1; return ;;
    # credential-rotation drives config:auth failover (A→B) — PROJECT-scoped, so
    # it does NOT create the unscoped "<category>:global" beads that force the
    # config-* cluster onto one shard. Free to live here for balance.
    credential-rotation)        echo 1; return ;;
    # ── Shard 2 ───────────────────────────────────────────────────────
    # pool-assign-attribution (363s) is the single heaviest module in the suite.
    # It seeds a 1-slot prewarmed pool on an EPHEMERAL per-test project and drives
    # a REAL warm pod through attributed work (gd-FZXGGpGROh). Given its own shard
    # with only light fillers so that even a full retry (2×363s ≈ 12m) stays under
    # the 20m task timeout.
    pool-assign-attribution)    echo 2; return ;;
    # pvc-gc-ttl (75s) + controller-metrics (45s) + trigger-engine-smoke (18s):
    # hermetic-ish fillers that top shard 2 up toward the ~535s target.
    pvc-gc-ttl)                 echo 2; return ;;
    controller-metrics)         echo 2; return ;;
    # idle-active-metrics (ka-0000003235, ~45s): scrapes the SAME controller :9092
    # registry as controller-metrics for the two Phase-0 idle/active gauges. Pinned
    # beside it so the pair of :9092 scrape modules share one shard's port-forward
    # idiom and the partition stays stable.
    idle-active-metrics)        echo 2; return ;;
    trigger-engine-smoke)       echo 2; return ;;
    # trigger-action-fidelity (ka-0000006362, ~15s): probe-driven bead-source
    # action assertions. Pinned beside trigger-engine-smoke — both drive the
    # pre-built trigger-probe against the live daemon, so co-locating keeps the
    # trigger E2E surface on one shard and the probe binary warm.
    trigger-action-fidelity)    echo 2; return ;;
    # trigger-cooldown-event-race (ka-0000002850, ~20s): drives the bead-source
    # EventReady cooldown-bypass (triggers.go:2093 "pending event") via the same
    # pre-built trigger-probe — a two-probe recipe (non-match stamps last_checked,
    # then a match inside the window must fire through the bypass). Pinned beside
    # the other trigger-probe modules so the binary stays warm on one shard.
    trigger-cooldown-event-race) echo 2; return ;;
    # trigger-multi-cooldown-event-race (ka-0000000405, ~20s): the multi-source
    # sibling of trigger-cooldown-event-race — drives MultiTriggerSource.EventReady
    # (-> scheduler.HasPendingEventMatch) through the SAME engine cooldown gate via
    # cmd/trigger-probe --source=multi. Same two-probe recipe. Pinned beside the
    # other trigger-probe modules so the binary stays warm on one shard.
    trigger-multi-cooldown-event-race) echo 2; return ;;
    # slack-* cluster (constraint #2): atomic + serialized for Socket-Mode
    # fan-out safety (ka-0000006398, epic ka-0000006991). A single test Slack app
    # delivers each inbound event to EVERY connected bridge, so two ephemeral
    # bridges on two shards would cross-fire; keeping all slack-* on ONE shard
    # guarantees only one bridge drives the test app. Weight ~0 today — every
    # slack-* SKIP-passes when the real-user rig / SLACK_BOT_TOKEN is absent.
    slack-threads)              echo 2; return ;;
    slack-cards)                echo 2; return ;;
    slack-ui)                   echo 2; return ;;
    slack-dashboard)            echo 2; return ;;
    group-dm)                   echo 2; return ;;
    # agent-feedback-writepath (ka-0000008183): a from-source probe wiring the
    # REAL AgentFeedbackAPI handler to a beadsapi.Client pointed at the LIVE
    # daemon, then asserting the persisted beads (handler shape + the daemon
    # (rater,rated_bead_id) upsert end-to-end). Shard 0 with the other from-source
    # Go probes so the toolchain stays warm and it can ride a pre-built binary
    # from shard 0's use: list.
    agent-feedback-writepath)   echo 0; return ;;
    # subagent-stop (ka-0000004963): from-source Go probe — builds gb, embeds a
    # NATS server, drives the REAL `gb hook relay` SubagentStop → real HookStream
    # subscriber → HandleSubagentStop, and asserts the subagent's FINAL message
    # reaches the stream chunk Output (the drop the gd-TdQnFDzJdo fix repaired).
    # Shard 0 with the other from-source Go probes so the toolchain stays warm.
    # Self-SKIPs clean if the Go toolchain is absent.
    subagent-stop)              echo 0; return ;;
    # gardener-start-argv-contract (ka-0000001242): from-source go-test EMITTER —
    # an //go:build e2e test (internal/bridge/gardener_start_argv_emit_e2e_test.go)
    # calls the UNEXPORTED gardenerStartArgs (the exact symbol the Slack
    # epic-mention entrypoint shells out with) and prints the argv; the module
    # then runs `gb <that argv>` against the live daemon on a FRESH epic and
    # asserts it reaches a create, plus a negative control (same argv, ack flag
    # stripped, second fresh epic → gate refuses, no schedule). Rides shard 0's
    # SAME warm e2e-tagged ./internal/bridge/ build as subagent-stop (build-gb is
    # on every shard's use: list; no new probe binary). Self-SKIPs clean if the
    # Go toolchain / daemon is absent.
    gardener-start-argv-contract) echo 0; return ;;
    # claudemore-hooks (ka-0000004452): the REPLAY-tier hooks e2e — builds
    # claudemore + gb + a NATS probe from source, then drives the REAL `claude`
    # binary through claudemore record→replay ($0, hermetic, no cluster) to
    # assert T5 (SessionStart prime consumption), T6 (Stop-gate BLOCK→ALLOW
    # loop), and PreToolUse/PostToolUse → NATS. Shard 0 with the other
    # from-source Go probes so the toolchain stays warm. Self-SKIPs clean if the
    # claude binary / Go toolchain is absent.
    claudemore-hooks)           echo 0; return ;;
    # claudemore-replay-fail-on-miss (ka-0000006183): the REPLAY-tier fail_on_miss
    # HTTP contract — a from-source boot probe (NO `claude` binary needed, unlike
    # the rest of the claudemore cluster). Builds cmd/claudemore, boots it in
    # REPLAY over a filesystem cassette store with the upstream at a CLOSED port,
    # and asserts a cassette miss is a LOUD 424 + X-Claudemore-Replay: miss (never
    # a silent LIVE fall-back), plus a HEAD preflight=200 leg, a real
    # record→replay HIT control (200+hit, proves the 424 is miss-specific), and a
    # populated-store per-hash miss control. Shard 0 with the other from-source Go
    # probes so the toolchain stays warm. Self-SKIPs clean if the Go toolchain /
    # curl / python3 is absent.
    claudemore-replay-fail-on-miss) echo 0; return ;;
    # claudemore-agentchoice (ka-0000005944): the SCHEDULED/OPT-IN-tier agent-choice
    # e2e — sibling of claudemore-hooks. Builds claudemore + gb + kd from source,
    # then drives the REAL `claude` binary through claudemore record→replay against
    # a stub beads daemon to assert the model's semantic CHOICES: T2 right-bead
    # claim, T3 decision divergence (fork→decision / status→none), T4 wrap-up
    # blockers omission.
    #
    # SKIP ACCEPTED — do NOT re-file this as a coverage gap (decision ka-0000004333,
    # operator Matt Baker, 2026-08-06, option `split`). Unlike claudemore-hooks —
    # which is $0/hermetic (records against a deterministic scripted stub upstream,
    # replays with NO upstream) and was fixed to RUN by installing a pinned claude
    # on shard 0 — claudemore-agentchoice is gated on CLAUDEMORE_AGENTCHOICE_E2E=1
    # AND records against a REAL litellm upstream using REAL tokens (it asserts
    # genuine model reasoning, which no scripted stub can stand in for). That real-
    # spend, real-upstream requirement is why it is SCHEDULED/opt-in tier, not a
    # per-MR/main gate: absent the opt-in it self-SKIPs clean (also without the
    # claude binary / Go toolchain / upstream), and that skip is the intended,
    # accepted default for the release suite. It is pinned to shard 0 alongside the
    # other from-source Go probes so the toolchain stays warm WHEN it does run.
    claudemore-agentchoice)     echo 0; return ;;
    # ── Shard 3 ───────────────────────────────────────────────────────
    # claudeless (252s) heads the shard; the config-* cluster (constraint #1,
    # ~131s total, config-bead-hotreload=65s dominant) fills the rest. config-*
    # MUST stay atomic here: every config-* test creates unscoped
    # "<category>:global" beads (claude-settings/mcp/hooks/instructions) on the
    # shared daemon and ResolveConfigBeads merges them across shards, so running
    # config-* on two shards at once silently cross-pollutes assertions
    # (gd-xG5dJItFru, gd-zzuzGsgOma). Serializing them on one shard eliminates
    # the whole class.
    claudeless)                 echo 3; return ;;
    # sse-reliability (54s) + sse-events (22s) + bridge-fleet-token-rotation
    # (23s) + gvisor-ns-sweep (17s): heavy fillers balancing shard 3 to ~535s.
    sse-reliability)            echo 3; return ;;
    sse-events)                 echo 3; return ;;
    bridge-fleet-token-rotation) echo 3; return ;;
    # gvisor-ns-sweep: hermetic `go test` wrapper (no live cluster), kept as a
    # shard-3 filler (gd-obdTf3fIcu).
    gvisor-ns-sweep)            echo 3; return ;;
    config-advice)              echo 3; return ;;
    config-bead-hotreload)      echo 3; return ;;
    config-beads)               echo 3; return ;;
    config-beads-crud)          echo 3; return ;;
    config-categories)          echo 3; return ;;
    config-dump-load)           echo 3; return ;;
    config-edge-cases)          echo 3; return ;;
    config-instructions)        echo 3; return ;;
    config-label-specificity)   echo 3; return ;;
    config-materialization)     echo 3; return ;;
    config-mcp)                 echo 3; return ;;
    config-merge-concat)        echo 3; return ;;
    config-merge-override)      echo 3; return ;;
    config-migrate)             echo 3; return ;;
    config-multi-role)          echo 3; return ;;
    config-project-inline)      echo 3; return ;;
    config-role-model)          echo 3; return ;;
    config-setup-claude)        echo 3; return ;;
    config-subscriptions)       echo 3; return ;;
    config-user-settings)       echo 3; return ;;
  esac
  # Hash-based fallback for all other (light, sub-~13s) modules. cksum(name)%4
  # spreads the long tail evenly and needs no maintenance as modules are added.
  local hash
  hash=$(echo -n "$name" | cksum | awk '{print $1}')
  echo $((hash % TOTAL_SHARDS))
}

# ── Module selection ────────────────────────────────────────────────
ONLY="${ONLY:-}"
SKIP="${SKIP:-}"
SMOKE_ONLY="${SMOKE_ONLY:-}"
MIGRATION_ACCEPTANCE="${MIGRATION_ACCEPTANCE:-}"
HERMETIC_ONLY="${HERMETIC_ONLY:-}"

# 5-module smoke subset (ka-0000004434). Order matches the bead so the
# log read-out is predictable for operators triaging smoke failures.
SMOKE_MODULES="tls-certs,stack-health,agent-crew-lifecycle,beads-crud,mail-ready,trigger-engine-smoke"

# P4.4 migration-acceptance subset (epic ka-0000003481). The named subset that
# gates the fresh-prod cutover (ka-0000002205): a post-auth authenticated viewer
# 200 (ka-0000008176), coopmux reachability (ka-0000004091), and — opt-in via
# AGENT_MR_E2E=1 — a real agent opening an MR end-to-end (ka-0000002701). Also
# includes stack-health (base deployment readiness) and tls-certs (edge certs)
# so a single MIGRATION_ACCEPTANCE=1 run is a self-contained cutover gate.
# Order = operator read-out order.
MIGRATION_ACCEPTANCE_MODULES="stack-health,tls-certs,migration-postauth-viewer,migration-coop-reachable,migration-agent-opens-mr"

# Hermetic subset (gd-3VrngvYd5x). Every module here runs to completion with
# NO cluster, NO beads daemon and NO port-forwards: pure `go test` over fake
# clientsets and httptest servers, local probe binaries, and static linters.
# This is what .github/workflows/e2e.yml's `shell-hermetic` job runs, and it is
# the first thing to execute any of these modules in GitHub Actions since
# 8a3eb29e deleted the old CI pipelines on 2026-09-16.
#
# Derived empirically (not by static categorisation alone): each candidate was
# run directly with no KUBECONFIG and no cluster reachable, and only those that
# passed or cleanly (LOUD) skipped made the cut.
#
# test-hermetic-set-lint.sh WILL guard this list once T6 wires it into a CI
# job -- it reds if any listed module acquires a kubectl/kd/gb/helm
# dependency. It is NOT wired as of this commit, so today this list is
# protected by nothing but this comment. Run it by hand if you change the
# list: ./gardenio/tests/e2e/scripts/test-hermetic-set-lint.sh
#
# Do not shrink this list to make a red go away without saying so on a bead --
# quiet shrinkage is precisely the failure mode this whole lane exists to fix.
HERMETIC_MODULES="edge-auth-box-auth,claudemore-replay-fail-on-miss,claudemore-replay-volatile-hash,grafana-directive,pvc-gc-ttl,gvisor-ns-sweep,gitlab-dark-provenance,gitlab-webhook-deadman,jira-webhook-deadman,slackbridge-respawn-phase-metrics,slackmock-loop,grafana-bridge,jira-conversational,jira-syncback-denybydefault,outbox-rules-json,escalation-auto-reaper,session-spend-attribution"

# apply_subset <label> <subset-csv> — narrow ONLY to <subset> (intersected with
# any caller-supplied ONLY) and disable shard gating so a single shard runs the
# whole subset end-to-end. Shared by SMOKE_ONLY and MIGRATION_ACCEPTANCE.
apply_subset() {
  local label="$1" subset="$2"
  if [ -n "$ONLY" ]; then
    # Intersect: keep only modules that appear in BOTH ONLY and <subset>.
    local intersect="" mod
    IFS=',' read -ra _ONLY <<< "$ONLY"
    for mod in "${_ONLY[@]}"; do
      mod=$(echo "$mod" | xargs)
      case ",$subset," in
        *",$mod,"*) intersect="${intersect:+$intersect,}$mod" ;;
      esac
    done
    ONLY="$intersect"
  else
    ONLY="$subset"
  fi
  echo "${label}=1: running only ${label} subset (ONLY=$ONLY)"
}

# When SMOKE_ONLY=1, narrow ONLY to the smoke subset.
if [ "$SMOKE_ONLY" = "1" ]; then
  apply_subset "SMOKE_ONLY" "$SMOKE_MODULES"
fi

# When MIGRATION_ACCEPTANCE=1, narrow ONLY to the migration-acceptance subset
# and disable shard gating (single-shard run). Mutually usable with SMOKE_ONLY:
# if both are set, SMOKE_ONLY has already narrowed ONLY, and this intersects
# further — but the intended use is MIGRATION_ACCEPTANCE=1 alone against prod.
if [ "$MIGRATION_ACCEPTANCE" = "1" ]; then
  SMOKE_ONLY=1   # reuse the shard-gating bypass in should_run
  apply_subset "MIGRATION_ACCEPTANCE" "$MIGRATION_ACCEPTANCE_MODULES"
fi

# When HERMETIC_ONLY=1, narrow ONLY to the hermetic subset. Like
# MIGRATION_ACCEPTANCE it borrows SMOKE_ONLY's shard-gating bypass rather than
# adding a third one to should_run.
if [ "$HERMETIC_ONLY" = "1" ]; then
  SMOKE_ONLY=1   # reuse the shard-gating bypass in should_run
  apply_subset "HERMETIC_ONLY" "$HERMETIC_MODULES"
fi

should_run() {
  local name="$1"
  # Shard gating: only run modules assigned to this shard.
  # Disabled in SMOKE_ONLY mode so a single shard runs the full subset.
  if [ "$SMOKE_ONLY" != "1" ]; then
    local assigned
    assigned=$(shard_for "$name")
    if [ "$assigned" != "$E2E_SHARD" ]; then
      return 1
    fi
  fi
  # --only filter
  if [ -n "$ONLY" ]; then
    IFS=',' read -ra ONLY_MODULES <<< "$ONLY"
    local found=false
    for mod in "${ONLY_MODULES[@]}"; do
      mod=$(echo "$mod" | xargs)
      if [ "$mod" = "$name" ]; then
        found=true
        break
      fi
    done
    if [ "$found" = false ]; then
      echo "SKIP (--only=$ONLY): $name"
      TESTS_SKIPPED=$((TESTS_SKIPPED + 1))
      echo "{\"name\":\"$name\",\"status\":\"skip\",\"rc\":0,\"duration_s\":0,\"shard\":$E2E_SHARD}" >> "$RESULTS_JSONL"
      return 1
    fi
  fi
  # --skip filter
  if [ -n "$SKIP" ]; then
    IFS=',' read -ra SKIP_MODULES <<< "$SKIP"
    for mod in "${SKIP_MODULES[@]}"; do
      mod=$(echo "$mod" | xargs)
      if [ "$mod" = "$name" ]; then
        echo "SKIP (--skip): $name"
        TESTS_SKIPPED=$((TESTS_SKIPPED + 1))
        echo "{\"name\":\"$name\",\"status\":\"skip\",\"rc\":0,\"duration_s\":0,\"shard\":$E2E_SHARD}" >> "$RESULTS_JSONL"
        return 1
      fi
    done
  fi
  return 0
}

MODULE_RC=0

# ── Module registry (same 85 entries as serial e2e) ────────────────
if should_run "tls-certs"; then
  run_module "tls-certs" "./gardenio/tests/e2e/scripts/test-tls-certs.sh" || MODULE_RC=1
fi
if should_run "stack-health"; then
  run_module "stack-health" "./gardenio/tests/e2e/scripts/test-stack-health.sh" || MODULE_RC=1
fi
if should_run "agent-crew-lifecycle"; then
  run_module "agent-crew-lifecycle" "./gardenio/tests/e2e/scripts/test-agent-crew-lifecycle.sh" || MODULE_RC=1
fi
if should_run "beads-crud"; then
  run_module "beads-crud" "./gardenio/tests/e2e/scripts/test-beads-crud.sh" || MODULE_RC=1
fi
if should_run "atomic-relabel"; then
  run_module "atomic-relabel" "./gardenio/tests/e2e/scripts/test-atomic-relabel.sh" || MODULE_RC=1
fi
if should_run "mail-nudge-delivery"; then
  run_module "mail-nudge-delivery" "./gardenio/tests/e2e/scripts/test-mail-nudge-delivery.sh" || MODULE_RC=1
fi
if should_run "decisions-yield"; then
  run_module "decisions-yield" "./gardenio/tests/e2e/scripts/test-decisions-yield.sh" || MODULE_RC=1
fi
if should_run "gate-system"; then
  run_module "gate-system" "./gardenio/tests/e2e/scripts/test-gate-system.sh" || MODULE_RC=1
fi
if should_run "decision-reports"; then
  run_module "decision-reports" "./gardenio/tests/e2e/scripts/test-decision-reports.sh" || MODULE_RC=1
fi
if should_run "account-auth"; then
  run_module "account-auth" "./gardenio/tests/e2e/scripts/test-account-auth.sh" || MODULE_RC=1
fi
if should_run "admin-strip-primary"; then
  # `gb admin strip-primary` Phase 6 (epic ka-0000008240, the LAST/irreversible
  # phase). Proves the RFC 7386 null-delete makes `primary` ABSENT (not "" or
  # "null") + the config primary_account strip preserves unknown keys, plus the
  # --dry-run/--snapshot-out operator-safety surface. Runs the REAL command
  # READ-ONLY (dry-run + snapshot-out, zero blast radius) and drives the apply
  # semantic via scoped throwaway beads — the fleet-wide --apply is NOT run here
  # (it would clobber sibling cred/session modules' primary_account on the shared
  # daemon; see the module header). Light (~3s), hash-placed shard — no pin.
  run_module "admin-strip-primary" "./gardenio/tests/e2e/scripts/test-admin-strip-primary.sh" || MODULE_RC=1
fi
if should_run "oidc-auth"; then
  run_module "oidc-auth" "./gardenio/tests/e2e/scripts/test-oidc-auth.sh" || MODULE_RC=1
fi
if should_run "forwardauth-403-force-logout"; then
  run_module "forwardauth-403-force-logout" "./gardenio/tests/e2e/scripts/test-forwardauth-403-force-logout.sh" || MODULE_RC=1
fi
if should_run "edge-auth"; then
  # Stage 3 AVA CLI scaffold — gates the gardenio-edge-auth shim deployed
  # by W-B-12 (ka-0000007194) under the W-E-6 POC env (ka-0000005102).
  # Archive per-gate curl transcripts into the shard's junit-results dir
  # so CI preserves them. REQUIRE_DEPLOYMENT defaults to 0 — gates A-C
  # SKIP cleanly when the deploy or PAT is absent. Operator flips it to
  # 1 in a follow-up once the staging soak proves the chart is stable.
  #
  # Shim discovery (ka-0000007461): point edge-auth deployment/service
  # discovery at the per-instance ${NAMESPACE}-edge-auth names so the script
  # port-forwards the right Service when one exists. In the ephemeral e2e ns
  # there is none (see split below), so Gates A–P + R SKIP cleanly.
  export EDGE_AUTH_DEPLOYMENT="${NAMESPACE}-edge-auth"
  export EDGE_AUTH_SERVICE="${NAMESPACE}-edge-auth"
  #
  # EPHEMERAL-vs-SINGLETON SPLIT (architecture fact, confirmed ka-0000007461):
  # The CI E2E suite deploys each run into a pool namespace (gardenio-e2e-N)
  # that deploys kbeads + controller but NOT edgeAuth / unifiedHost, and the
  # CI job carries no GitLab PAT secret. So:
  #   - Shim gates (E–P) and Gate R inherently SKIP here (no edge-auth deploy,
  #     no unified host, no member/non-member PAT). They run only against a
  #     long-lived deploy with the same rigging as prod.
  #   - Gate Q is THE green-able GATING assertion in the ephemeral ns: it runs
  #     UNCONDITIONALLY against the kbeads daemon this shard already
  #     port-forwards (BEADS_HTTP_URL, exported above) and proves the
  #     forwardauth-identity WRITE attribution contract (operated_by=oidc:<email>)
  #     — the server-side equivalent of the gb-login cookie write path.
  # BUG#1 regression gates (ka-0000004502, Gate M/N) and Gate R run only when
  # UNIFIED_HOST_URL is exported (the singleton); left unset here so they SKIP.
  mkdir -p "$JUNIT_DIR/edge-auth-logs" 2>/dev/null || true
  export LOG_ARCHIVE_DIR="$JUNIT_DIR/edge-auth-logs"
  run_module "edge-auth" "./gardenio/tests/e2e/edge-auth/test-edge-auth.sh" || MODULE_RC=1
  unset LOG_ARCHIVE_DIR
  unset EDGE_AUTH_DEPLOYMENT EDGE_AUTH_SERVICE
fi
if should_run "edge-auth-box-auth"; then
  # BOX-AUTH profile (EDGE_AUTH_PAT_DISABLED, ka-0000006087 §6 / ka-0000005667).
  # A from-source local-binary probe: it builds + boots the real cmd/edge-auth
  # under the box-auth env and asserts the readiness contract (Ready-with-cookie,
  # NotReady-without, PAT-bearing 503, ZERO outbound gitlab.com calls) with a
  # KNOWN-GOOD control proving the no-call assertion is non-vacuous. Needs no
  # cluster — the box-auth profile is not deployed in any e2e ns (only the
  # portable Gasbox flips patDisabled=true) — so it runs GREEN in every shard
  # container with a Go toolchain + python3 and SKIPs CLEANLY (loud) without them.
  run_module "edge-auth-box-auth" "./gardenio/tests/e2e/scripts/test-edge-auth-box-auth.sh" || MODULE_RC=1
fi
if should_run "airgap-image-distribution"; then
  # P4.3 air-gap image-distribution knobs (ka-0000007260, parent ka-0000000958).
  # A hermetic helm-RENDER probe: it runs `helm template` against the real
  # gardenio chart and asserts the four global.airgap knobs — AGENT_PULL_POLICY
  # =IfNotPresent, DIND_IMAGE override, IMAGE_PULL_SECRETS dropped, and (the
  # headline) latestMode forced off so a pinned digest survives — each paired
  # with a known-good airgap=false control. global.airgap is NEVER true in any
  # e2e namespace (only the portable Gasbox flips it), so there is no live
  # deployment to assert against; the knobs live entirely at Helm render time.
  # Needs no cluster/beads/tunnel — runs GREEN in every shard container with the
  # helm binary + the chart on disk, and SKIPs CLEANLY (loud) without them.
  run_module "airgap-image-distribution" "./gardenio/tests/e2e/scripts/test-airgap-image-distribution.sh" || MODULE_RC=1
fi
if should_run "claudemore-replay-fail-on-miss"; then
  # claudemore REPLAY fail_on_miss HTTP contract (ka-0000006183, parent
  # ka-0000000958, feature ka-0000005558). A from-source boot probe of the ONE
  # load-bearing safety property of the whole $0/deterministic REPLAY tier: a
  # cassette miss is a LOUD 424 + X-Claudemore-Replay: miss, NEVER a silent
  # fall-back to LIVE (which would quietly spend real tokens against litellm
  # while CI reports green). Unlike the rest of the claudemore cluster this is
  # testable with NO `claude` binary — it is a plain HTTP request/response
  # contract — so it builds cmd/claudemore, boots it in REPLAY over a filesystem
  # cassette store (CLAUDEMORE_CASSETTE_DIR, no S3/MinIO) with the upstream at a
  # CLOSED port (the anti-vacuity control: a fall-through hits connection-refused
  # instead of silently succeeding), and drives it with curl. Four legs: the
  # headline uncassetted-POST→424+miss, a HEAD preflight→200+preflight, a REAL
  # record→replay HIT control (200+hit+recorded body, proving the 424 is
  # miss-specific not a blanket outage), and a populated-store per-hash miss
  # (proving the miss is keyed to the request hash). claudemore is test-lane
  # only (the chart no longer ships it, gd-Y4ZCGr2m2t), so there is no live
  # Deployment; the contract lives entirely at the binary's HTTP surface.
  # Needs no cluster/beads/tunnel — runs GREEN in every shard with the Go
  # toolchain + curl + python3, SKIPs CLEANLY (loud) without them.
  run_module "claudemore-replay-fail-on-miss" "./gardenio/tests/e2e/scripts/test-claudemore-replay-fail-on-miss.sh" || MODULE_RC=1
fi
if should_run "claudemore-replay-volatile-hash"; then
  # claudemore REPLAY request-hash VOLATILE-FIELD STRIPPING (ka-0000006222,
  # parent ka-0000000958, related ka-0000000029). A from-source, curl-driven boot
  # probe of stripVolatileForHash / volatileNestedKeys / ccVersionRE
  # (cassette.go): two launches whose request bodies differ ONLY in a per-launch-
  # volatile field (nested cache_control breakpoint PLACEMENT, or the
  # cc_version=2.1.211.<suffix> billing token) MUST hash identically and
  # REPLAY-HIT, while a launch differing in a NON-volatile field (real
  # system-prompt content) MUST MISS. The one module that mentions these fields
  # (claudemore-hooks) deliberately FORCES byte-identical bodies (fixed re-seeded
  # HOME + --setting-sources "") and so structurally cannot exercise the strip —
  # gut stripVolatileForHash and its legs stay green. This module STOPS forcing
  # identity: it hand-authors the two bodies (mirroring the real `claude` wire —
  # cc_version header on system[0], cache_control on system[]/messages[].content[])
  # and varies ONLY the field under test, so a HIT proves the strip and a MISS
  # (real-content control) proves it strips volatile-ONLY. Three legs: volatile-HIT
  # (200+hit), nonvolatile-MISS negative control (424+miss), and stored-fidelity
  # (the on-disk cassette KEEPS cache_control+cc_version — strip is hash-only).
  # Same toolchain as claudemore-replay-fail-on-miss, no `claude` binary needed;
  # claudemore is NEVER enabled on any e2e namespace, so the contract lives
  # entirely at the binary's HTTP surface. Runs GREEN in every shard with the Go
  # toolchain + curl + python3, SKIPs CLEANLY (loud) via E2E_SKIP_SENTINEL without.
  run_module "claudemore-replay-volatile-hash" "./gardenio/tests/e2e/scripts/test-claudemore-replay-volatile-hash.sh" || MODULE_RC=1
fi
# ── P4.4 migration-acceptance smoke checks (epic ka-0000003481) ─────
# Codified post-cutover smoke gating the fresh-prod migration (ka-0000002205).
# All three self-SKIP cleanly when their substrate is absent, so they are inert
# in minimal ephemeral e2e namespaces and only assert against the long-lived
# singleton / prod. Grouped as the MIGRATION_ACCEPTANCE subset (see below).
#
#   migration-postauth-viewer  ka-0000008176 — authenticated viewer GET → 200 +
#     real daemon-backed body (NOT the pre-auth 303 that masked ka-0000007004).
#   migration-coop-reachable   ka-0000004091 — coopmux /api/v1/health healthy.
#   migration-agent-opens-mr   ka-0000002701 — a real agent opens an MR e2e
#     (opt-in: AGENT_MR_E2E=1; loud SKIP otherwise — never in the fast smoke).
if should_run "migration-postauth-viewer"; then
  run_module "migration-postauth-viewer" "./gardenio/tests/e2e/scripts/test-migration-postauth-viewer.sh" || MODULE_RC=1
fi
if should_run "migration-coop-reachable"; then
  run_module "migration-coop-reachable" "./gardenio/tests/e2e/scripts/test-migration-coop-reachable.sh" || MODULE_RC=1
fi
if should_run "migration-agent-opens-mr"; then
  run_module "migration-agent-opens-mr" "./gardenio/tests/e2e/scripts/test-migration-agent-opens-mr.sh" || MODULE_RC=1
fi
# ── T4 GitLab-JWT end-to-end gate (gd-8EL2X1CJEw, epic ka-0000004118) ─────
# Green-light gate before the T5 prod Keycloak-permadelete cutover: asserts the
# beads daemon + coopmux JWT verifiers are pointed DIRECTLY at gitlab.com (not a
# Keycloak realm), a full kd create→close session leg succeeds with the GitLab
# `sub`/`email` recorded as the audit actor, and NO Keycloak JWT is observed on
# either verifier. LOUD-SKIPs cleanly in the ephemeral e2e ns (which stays on the
# Keycloak realm broker until the cutover proves out) — inert
# in the fast fan-out, asserts only against the GitLab-cutover singleton / prod.
if should_run "gitlab-jwt-e2e"; then
  run_module "gitlab-jwt-e2e" "./gardenio/tests/e2e/scripts/test-gitlab-jwt-e2e.sh" || MODULE_RC=1
fi
if should_run "viewer-migration-banner"; then
  run_module "viewer-migration-banner" "./gardenio/tests/e2e/scripts/test-viewer-migration-banner.sh" || MODULE_RC=1
fi
if should_run "viewer-bead-activity"; then
  run_module "viewer-bead-activity" "./gardenio/tests/e2e/scripts/test-viewer-bead-activity.sh" || MODULE_RC=1
fi
# beads-viewer /maps + /gardeners server-side selection correctness (gd-VEanc11wVQ,
# epic ka-0000000958). Asserts the rendered HTML honors the maps2 live/all scope,
# the crossfilter facet rail, the /gardeners fixture + schedule-mirror exclusions,
# field-only role=gardener inclusion, and the live goal-state badge. Self-SKIPs
# when the viewer Service is absent, so it is inert in the ephemeral e2e
# namespaces and asserts only against a long-lived viewer (singleton/prod).
if should_run "viewer-maps"; then
  run_module "viewer-maps" "./gardenio/tests/e2e/scripts/test-viewer-maps.sh" || MODULE_RC=1
fi
# beads-viewer /agents live-view correctness: the SSE event-driven row render
# must stay column-congruent with the static page render (epic ka-0000007652).
# Self-SKIPs when the viewer Service is absent, so it is inert in the ephemeral
# e2e namespaces and asserts only against a long-lived viewer (singleton/prod).
if should_run "agents-live-view"; then
  run_module "agents-live-view" "./gardenio/tests/e2e/scripts/test-agents-live-view.sh" || MODULE_RC=1
fi
# /plans "Wake" button (plan-assistant prompt POST): spawn on first message,
# wake a parked assistant with session_resume + prior-name continuity, reject an
# empty message (gd-R7wAWZ0E8T). Self-SKIPs when the viewer Service is absent, so
# it is inert in the ephemeral e2e namespaces and asserts only against a
# long-lived viewer (singleton/prod).
if should_run "plans-wake-button"; then
  run_module "plans-wake-button" "./gardenio/tests/e2e/scripts/test-plans-wake-button.sh" || MODULE_RC=1
fi
# /plans MR overflow pill FailedHiddenCount attention signal (ka-0000000730,
# related gd-VHwFGOoKDV / ka-0000000387): seeds step->produces->merge-request
# bead graphs in the live daemon and renders them through the REAL beads-viewer
# binary (dev-auth bypass) to assert the "+N MR · ✕ K" overflow — inline pills
# under the cap, a calm all-green overflow, a hidden-failed MR tinting the pill +
# folding a ✕ K facet (THE POINT), and a promoted inline failure NOT inflating
# the hidden count. Self-SKIPs when no daemon is reachable or no viewer binary /
# go toolchain is available, so it is inert where it cannot run.
if should_run "plans-mr-overflow"; then
  run_module "plans-mr-overflow" "./gardenio/tests/e2e/scripts/test-plans-mr-overflow.sh" || MODULE_RC=1
fi
# /plans ⚠ stalled-badge render: a dead-agent orphan past the 1h grace renders the
# badge with the dead-agent BadgeLabel ("· 1 dead step") + frozen-age ("· 3h"),
# loud-tints past the 24h stale bar, and is SUPPRESSED by a live crew on the step
# (ka-0000000734) — seeds bead state + SQL-backdated updated_at and asserts the
# rendered HTML on both the cockpit + index card (ka-0000006935). Self-SKIPs when
# the viewer Service is absent OR the in-chart postgres cannot be exec'd to
# backdate updated_at (external RDS / viewer-only), so it is inert where it cannot
# host the fixture and asserts only against a long-lived viewer (singleton/prod).
if should_run "plans-stall-badge"; then
  run_module "plans-stall-badge" "./gardenio/tests/e2e/scripts/test-plans-stall-badge.sh" || MODULE_RC=1
fi
# /plans "last change" stamp folds crew heartbeats (ka-0000003623; runtime fix
# MR !6757 / ka-0000004310): both freshness surfaces fold the plan's crew
# heartbeats into the "updated Xago" stamp so a step claimed a month ago whose
# crew is heartbeating now reads FRESH (not "1 month ago" beside a live crew
# chip). Seeds a plan whose epic + step are SQL-backdated −45d plus a crew wired
# by an `assigned` edge, and asserts the rendered stamp on BOTH the cockpit
# header + index card: fresh + agreeing with a live crew, falling back to the
# step/epic time when the crew's heartbeat ages (load-bearing negative control),
# and re-freshening the within-tier sort rank. The temporal twin of
# plans-stall-badge (that asserts the STALL predicate; this the freshness stamp).
# Self-SKIPs when the viewer Service is absent OR the in-chart postgres cannot be
# exec'd to backdate updated_at (external RDS / viewer-only), so it is inert
# where it cannot host the fixture and asserts only against a long-lived viewer.
if should_run "plans-last-change-crew"; then
  run_module "plans-last-change-crew" "./gardenio/tests/e2e/scripts/test-plans-last-change-crew.sh" || MODULE_RC=1
fi
# LIVE beadsapi contract: `gb plan show` resolves a plan + phases + steps
# straight from the daemon with only BEADS_AUTH_TOKEN (both BEADS_VIEWER_*
# unset) — the exact env where the plan WRITE verbs 401 (ka-0000004277, epic
# ka-0000003451). Seeds a phased plan via kd (token-only), drives the real gb
# binary, and proves the write path still needs a viewer (contrast). Self-SKIPs
# when the daemon is unreachable, so it is inert where it cannot run.
if should_run "plan-read-daemon-alone"; then
  run_module "plan-read-daemon-alone" "./gardenio/tests/e2e/scripts/test-plan-read-daemon-alone.sh" || MODULE_RC=1
fi
# LIVE beadsapi contract: `gb bundle update` rewrites a doc bead's body IN PLACE
# (no forked duplicate doc) straight from the daemon with only BEADS_AUTH_TOKEN
# (both BEADS_VIEWER_* unset) — the converge verb shipped in MR !6651
# (coverage-backfill ka-0000004386, epic ka-0000000958). Seeds origin+bundle+doc
# via the real gb binary (token-only), drives `gb bundle update`, and asserts the
# mutation persisted + the bundle still holds exactly one doc. Self-SKIPs when the
# daemon is unreachable, so it is inert where it cannot run. Light (~5s, hash-
# placed shard) — no shard_for pin needed.
if should_run "bundle-update"; then
  run_module "bundle-update" "./gardenio/tests/e2e/scripts/test-bundle-update.sh" || MODULE_RC=1
fi
# `gb pool enable` / `gb pool disable` read-modify-write semantics
# (coverage-backfill ka-0000005192, epic ka-0000000958). Drives the REAL
# gb binary against a real daemon (token-only) and asserts the RMW-preserve
# contract at cmd/gb/pool.go:115-188: disable/enable flip ONLY `enabled` while
# preserving role/min/max/mode, enable fills the K8s defaults (thread/2/5/crew)
# on a brand-new pool, and never clobbers an explicit value on a partial config.
# The existing pool e2es all seed via a raw whole-object `kd update -f` write, so
# this is the sole end-to-end cover of the CLI's field-preserving path. Self-SKIPs
# when the daemon is unreachable, so it is inert where it cannot run. Light
# (~3s, hash-placed shard) — no shard_for pin needed.
if should_run "pool-enable-disable-rmw"; then
  run_module "pool-enable-disable-rmw" "./gardenio/tests/e2e/scripts/test-pool-enable-disable-rmw.sh" || MODULE_RC=1
fi
# Embedded Grafana dashboard iframe loads (no X-Frame-Options refusal) —
# gd-opukem7ufe, epic gd-OY6hl3Qh9l. Guards the allow_embedding fix
# (ka-0000003842). Self-SKIPs when the Grafana Service is absent, so it is
# inert in the per-instance ephemeral e2e namespaces (grafana.enabled=false
# there) and asserts only against a long-lived deploy / prod.
if should_run "grafana-iframe"; then
  run_module "grafana-iframe" "./gardenio/tests/e2e/scripts/test-grafana-iframe.sh" || MODULE_RC=1
fi
# `gb grafana render` CLI (bead→uid resolution, d/ vs d-solo routing, and the
# placeholder / 404 renderer-missing failure paths) — coverage-backfill
# ka-0000007821, epic ka-0000000958. HERMETIC: drives the REAL gb binary against
# Python-stdlib stub daemon + renderer servers, so it needs neither a cluster
# nor the beads tunnel and is a HARD gate on every shard-0 run. Self-SKIPs
# (loud) only if the gb binary or python3 is somehow absent.
if should_run "grafana-render-cli"; then
  run_module "grafana-render-cli" "./gardenio/tests/e2e/scripts/test-grafana-render-cli.sh" || MODULE_RC=1
fi
# `gb verify-prod-images` operator drill (ka-0000003773, epic ka-0000000958,
# related ka-0000007820): the THIRD line of defense against a coverage-instrumented
# (-cover) image reaching prod — the release-runbook post-deploy step whose doc
# says the runbook "can branch on $? alone". The top-level orchestrator
# runVerifyProdImages had ZERO e2e (and zero unit) coverage, so its exit-code
# contract, the STABLE JSON verdict, and the --skip-cluster file-scan compose-order
# were entirely unexercised. HERMETIC: drives the REAL gb binary with
# --skip-cluster over hand-written deploy-values fixtures (a pure file scan — no
# stub servers, no cluster, no beads tunnel). Offender legs assert exit NON-ZERO +
# pass=false on BOTH detection signals (repo -cover suffix AND cover tag scheme);
# clean legs assert exit 0 + pass=true + offenders=[], incl. a near-miss fixture
# (coverage-patch/recover/discovery — 'cover' mid-word) proving the happy verdict is
# a REAL clean scan, not a matcher that never fires. ANTI-VACUITY: if
# errCoverImagesFound (verify_prod_images.go:361) regressed to `return nil` the two
# offender legs go RED (verified against an overlay-built mutant). HARD gate on
# every shard-0 run; self-SKIPs (loud) only if the gb binary or python3 is absent.
if should_run "verify-prod-images"; then
  run_module "verify-prod-images" "./gardenio/tests/e2e/scripts/test-verify-prod-images.sh" || MODULE_RC=1
fi
# `gb hook prime` ## Assignment field-elision (ka-0000007410, epic ka-0000000958,
# runtime ka-0000006516): the fleet-wide SessionStart hot path that collapses an
# agent bead's oversized `prompt:` field to a preview + `kd show <id>` pointer. Had
# ZERO e2e — every prior grep hit was a settings.json WIRING assertion, and the 30
# unit tests STUB `runKD` so they never see the real `kd show` TEXT the elision
# parses. Drives the REAL gb binary against a REAL `kd show` over the shard's live
# daemon, on a seeded-then-CLOSED fixture agent bead (a closed agent bead is never
# spawned by the reconciler → zero-churn, no pod). Legs: the over-ceiling prompt is
# elided (preview + pointer, deep body gone), the under-ceiling
# schedule_terminal_goal survives INTACT (anti-vacuity), and Depends On/Children/
# Mentions still render (the kd text-drift leg). ANTI-VACUITY: two overlay-built
# ceiling mutants (→∞ stops eliding → the elision leg reds; →1 elides everything →
# the survives-intact leg reds) machine-check the legs are load-bearing. Pinned to
# shard 0 with the other gb/toolchain-warm modules (it needs build-gb + a Go
# toolchain for the mutant overlay, both already on shard 0). Self-SKIPs (loud) if
# gb / kd / python3 or the daemon is absent; the mutant leg additionally skips loud
# with no Go toolchain.
if should_run "hook-prime-assignment"; then
  run_module "hook-prime-assignment" "./gardenio/tests/e2e/scripts/test-hook-prime-assignment.sh" || MODULE_RC=1
fi
# `gb jira reply` → ticket-mirror triggered-by link (ka-0000006808, MR !6704,
# epic ka-0000000958): the outbound reply message bead gets a triggered-by edge
# to the ticket's jira:<KEY> mirror bead (cmd/gb/jira.go linkJiraReplyToMirror) —
# a CLI-side behavior the conversational probe's bridge-driven reply leg cannot
# reach. HERMETIC: drives the REAL gb binary against a Python-stdlib stub daemon
# (records create/list/dependency/close) + stub jira-bridge, asserting exactly
# one edge to the mirror (leg 1) and zero edges when no mirror is synced (leg 2,
# negative control). Needs neither a cluster nor the beads tunnel and is a HARD
# gate on every shard-0 run. Self-SKIPs (loud) only if the gb binary or python3
# is somehow absent.
if should_run "jira-reply-mirror-link"; then
  run_module "jira-reply-mirror-link" "./gardenio/tests/e2e/scripts/test-jira-reply-mirror-link.sh" || MODULE_RC=1
fi
# `gb jira backlog-pick` CLI (task gd-LQcsEs2WQT, epic ka-0000000958): the
# backlog-mode ticket picker that walks a released wave's explicit FSM keys,
# skips anything already carrying the gardenio-triaged Jira label (read LIVE), and
# returns the oldest un-triaged key. HERMETIC: drives the REAL gb binary against
# a Python-stdlib stub of the Jira JQL-search endpoint, asserting oldest-first
# pick, the live-label dedup (incl. the gardenio-triaged-sev-1 refinement-label
# non-skip), MISSING surfacing, the > 50-key `key IN (...)` JQL batching (all
# keys walked, never truncated), and the read-only contract (0 writes recorded).
# A truncated fetch or a broken dedup here is the "false wave-drained vs. double-
# spend" failure the bead is about. Needs neither a cluster nor the beads tunnel
# and is a HARD gate on every shard-0 run. Self-SKIPs (loud) only if the gb
# binary or python3 is somehow absent (or a PATH gb predates backlog-pick and no
# toolchain can rebuild it).
if should_run "jira-backlog-pick"; then
  run_module "jira-backlog-pick" "./gardenio/tests/e2e/scripts/test-jira-backlog-pick.sh" || MODULE_RC=1
fi
# `gb grafana users list` / `gb grafana users bump` CLI (list role filtering,
# bump from/to/idempotency selection, --dry-run no-write, and the
# unmatched-name / invalid-role hard errors) — coverage-backfill ka-0000004197,
# epic ka-0000000958. HERMETIC: drives the REAL gb binary against a stateful
# Python-stdlib stub of the Grafana org-users API (list-after-bump and
# idempotent re-bump verified end-to-end via the stub's PATCH log), so it needs
# neither a cluster nor the beads tunnel and is a HARD gate on every shard-0
# run. Self-SKIPs (loud) only if the gb binary or python3 is somehow absent.
if should_run "grafana-users-cli"; then
  run_module "grafana-users-cli" "./gardenio/tests/e2e/scripts/test-grafana-users-cli.sh" || MODULE_RC=1
fi
# The `:::grafana <uid>` markdown embed directive (LinkifyHTML →
# embedGrafanaDirectives): renders a sandboxed Grafana iframe when
# GRAFANA_BASE_URL is set, a quiet "not configured" note when it is not —
# coverage-backfill ka-0000007821, epic ka-0000000958. HERMETIC: boots the REAL
# beads-viewer binary against a Python-stdlib stub daemon TWICE (configured +
# unconfigured), asserting both render paths a curl of the bead-show page can
# see. No cluster, no tunnel. Self-SKIPs (loud) only if the viewer binary
# cannot be resolved/built or python3 is absent.
if should_run "grafana-directive"; then
  run_module "grafana-directive" "./gardenio/tests/e2e/scripts/test-grafana-directive.sh" || MODULE_RC=1
fi
# /charts Observability Hub + its two browser-facing daemon proxies:
# /api/v1/charts (read-only proxy to the kbeads chart catalog, E6.1
# ka-0000005063) and /api/v1/query_range (read-only proxy to the in-cluster
# vmsingle, ka-0000002747) — coverage-backfill gd-irTNYKQtA5, epic
# ka-0000000958. Asserts the deployed viewer→daemon / viewer→vmsingle data path
# the June freeze shipped with only httptest-unit coverage. Self-SKIPs when the
# viewer Service is absent, so it is inert in the per-instance ephemeral e2e
# namespaces (viewer OIDC surface is singleton/prod-only) and asserts only
# against a long-lived deploy / prod.
if should_run "charts-hub"; then
  run_module "charts-hub" "./gardenio/tests/e2e/scripts/test-charts-hub.sh" || MODULE_RC=1
fi
# /costs spend page + its htmx cost-explorer fragment (coverage-backfill
# ka-0000005996, epic ka-0000000958). The operator's primary spend surface
# shipped with ZERO e2e — the three cost-adjacent modules all skip the page
# itself (litellm-cost-tracking asserts LiteLLM per-key spend, usage-tracking
# asserts SESSION-bead usage, agent-cost-context asserts the per-agent chips on
# /agents + /{id}). Beyond marker asserts it pins a BEHAVIORAL invariant that
# discriminates the rolling-window headline $/h from the avg-since-midnight
# computation it replaced (both render as "$N/h", so a string grep cannot tell
# them apart): on a young UTC day with a $0.00 settled total, a midnight-average
# rate is necessarily $0.00/h while a 24h-rolling rate stays positive. Also
# cross-checks projected ≈ rate × 24, and asserts the inline-ECharts wiring
# stays gone after the June Grafana swap (costs_test.go pins the same absence
# in-tree; this proves it on a DEPLOYED viewer). Read-only — creates no beads.
# Self-SKIPs when the viewer Service is absent or no bearer is mintable, so it
# is inert in the per-instance ephemeral e2e namespaces (viewer OIDC surface is
# singleton/prod-only) and asserts only against a long-lived deploy / prod.
if should_run "costs-spend-page"; then
  run_module "costs-spend-page" "./gardenio/tests/e2e/scripts/test-costs-spend-page.sh" || MODULE_RC=1
fi
if should_run "multi-user-e2e"; then
  run_module "multi-user-e2e" "./gardenio/tests/e2e/scripts/test-multi-user-e2e.sh" || MODULE_RC=1
fi
if should_run "dependencies"; then
  run_module "dependencies" "./gardenio/tests/e2e/scripts/test-dependencies.sh" || MODULE_RC=1
fi
if should_run "list-with-blockers"; then
  run_module "list-with-blockers" "./gardenio/tests/e2e/scripts/test-list-with-blockers.sh" || MODULE_RC=1
fi
if should_run "labels"; then
  run_module "labels" "./gardenio/tests/e2e/scripts/test-labels.sh" || MODULE_RC=1
fi
if should_run "config-advice"; then
  run_module "config-advice" "./gardenio/tests/e2e/scripts/test-config-advice.sh" || MODULE_RC=1
fi
if should_run "advice-budget"; then
  run_module "advice-budget" "./gardenio/tests/e2e/scripts/test-advice-budget.sh" || MODULE_RC=1
fi
if should_run "advice-supersession"; then
  run_module "advice-supersession" "./gardenio/tests/e2e/scripts/test-advice-supersession.sh" || MODULE_RC=1
fi
if should_run "advice-lint-undeliverable"; then
  run_module "advice-lint-undeliverable" "./gardenio/tests/e2e/scripts/test-advice-lint-undeliverable.sh" || MODULE_RC=1
fi
if should_run "config-beads"; then
  run_module "config-beads" "./gardenio/tests/e2e/scripts/test-config-beads.sh" || MODULE_RC=1
fi
if should_run "config-instructions"; then
  run_module "config-instructions" "./gardenio/tests/e2e/scripts/test-config-instructions.sh" || MODULE_RC=1
fi
if should_run "config-materialization"; then
  run_module "config-materialization" "./gardenio/tests/e2e/scripts/test-config-materialization.sh" || MODULE_RC=1
fi
if should_run "config-dump-load"; then
  run_module "config-dump-load" "./gardenio/tests/e2e/scripts/test-config-dump-load.sh" || MODULE_RC=1
fi
if should_run "config-merge-concat"; then
  run_module "config-merge-concat" "./gardenio/tests/e2e/scripts/test-config-merge-concat.sh" || MODULE_RC=1
fi
if should_run "config-label-specificity"; then
  run_module "config-label-specificity" "./gardenio/tests/e2e/scripts/test-config-label-specificity.sh" || MODULE_RC=1
fi
if should_run "config-beads-crud"; then
  run_module "config-beads-crud" "./gardenio/tests/e2e/scripts/test-config-beads-crud.sh" || MODULE_RC=1
fi
if should_run "config-categories"; then
  run_module "config-categories" "./gardenio/tests/e2e/scripts/test-config-categories.sh" || MODULE_RC=1
fi
if should_run "config-merge-override"; then
  run_module "config-merge-override" "./gardenio/tests/e2e/scripts/test-config-merge-override.sh" || MODULE_RC=1
fi
if should_run "config-mcp"; then
  run_module "config-mcp" "./gardenio/tests/e2e/scripts/test-config-mcp.sh" || MODULE_RC=1
fi
if should_run "config-user-settings"; then
  run_module "config-user-settings" "./gardenio/tests/e2e/scripts/test-config-user-settings.sh" || MODULE_RC=1
fi
if should_run "config-setup-claude"; then
  run_module "config-setup-claude" "./gardenio/tests/e2e/scripts/test-config-setup-claude.sh" || MODULE_RC=1
fi
if should_run "config-multi-role"; then
  run_module "config-multi-role" "./gardenio/tests/e2e/scripts/test-config-multi-role.sh" || MODULE_RC=1
fi
if should_run "multirepo-skill-discovery"; then
  run_module "multirepo-skill-discovery" "./gardenio/tests/e2e/scripts/test-multirepo-skill-discovery.sh" || MODULE_RC=1
fi
if should_run "role-skill-toggle"; then
  run_module "role-skill-toggle" "./gardenio/tests/e2e/scripts/test-role-skill-toggle.sh" || MODULE_RC=1
fi
if should_run "config-project-inline"; then
  run_module "config-project-inline" "./gardenio/tests/e2e/scripts/test-config-project-inline.sh" || MODULE_RC=1
fi
if should_run "config-subscriptions"; then
  run_module "config-subscriptions" "./gardenio/tests/e2e/scripts/test-config-subscriptions.sh" || MODULE_RC=1
fi
if should_run "config-edge-cases"; then
  run_module "config-edge-cases" "./gardenio/tests/e2e/scripts/test-config-edge-cases.sh" || MODULE_RC=1
fi
if should_run "config-migrate"; then
  run_module "config-migrate" "./gardenio/tests/e2e/scripts/test-config-migrate.sh" || MODULE_RC=1
fi
if should_run "config-bead-hotreload"; then
  run_module "config-bead-hotreload" "./gardenio/tests/e2e/scripts/test-config-bead-hotreload.sh" || MODULE_RC=1
fi
if should_run "prime-setup"; then
  run_module "prime-setup" "./gardenio/tests/e2e/scripts/test-prime-setup.sh" || MODULE_RC=1
fi
if should_run "mail-ready"; then
  run_module "mail-ready" "./gardenio/tests/e2e/scripts/test-mail-ready.sh" || MODULE_RC=1
fi
if should_run "mail-system"; then
  run_module "mail-system" "./gardenio/tests/e2e/scripts/test-mail-system.sh" || MODULE_RC=1
fi
if should_run "epics-children"; then
  run_module "epics-children" "./gardenio/tests/e2e/scripts/test-epics-children.sh" || MODULE_RC=1
fi
# kbeads daemon auto-close cascade (ka-0000003089). Sits next to epics-children
# and mr-merge-flow because it covers the seam BOTH of them stop short of:
# epics-children closes its epic by hand (so last-child auto-close never fires)
# and mr-merge-flow asserts only the jira-bridge Review transition. This module
# asserts the transitions the DAEMON performs on its own — epic drain, leaf
# close-on-merge, decision-gate park, and the live-gardener defer — keying every
# positive assertion on closed_by=system:auto-close so a bead closed by anything
# else cannot masquerade as a cascade.
if should_run "auto-close-cascade"; then
  run_module "auto-close-cascade" "./gardenio/tests/e2e/scripts/test-auto-close-cascade.sh" || MODULE_RC=1
fi
# auto-parent-suppression (ka-0000005752, parent ka-0000000958): backfills the
# ZERO e2e coverage of the daemon's agent/session auto-parent SKIP (fix MR !6699
# / ka-0000002199). Acts AS a synthetic gardener whose task_id is a goal-bearing
# epic and asserts a type=agent and type=session bead it creates get NO
# parent-child edge to the epic, while a type=task control by the same gardener
# DOES (the skip is type-scoped, not a dead auto-parent). Daemon-only; every
# agent fixture is fenced (project=e2e-test + agent_state=failed) so it never
# cold-starts a pod. Self-SKIPs when no daemon is reachable.
if should_run "auto-parent-suppression"; then
  run_module "auto-parent-suppression" "./gardenio/tests/e2e/scripts/test-auto-parent-suppression.sh" || MODULE_RC=1
fi
# agent-state-enum (ka-0000000270, parent ka-0000000958): backfills the ZERO
# live-daemon coverage of the three type:agent agent_state enum values merge
# 568969352 added — queued / model_rate_limited / stop_requested — the third
# recurrence of the silent-PATCH-drop class (ka-0000000585 → ka-0000006367 →
# this). The only prior "coverage" was test-microvm-execution-target.sh's
# state_accepted() shell-literal loop, which asserts a hardcoded string against
# itself and passes with kbeads uninstalled. This drives the LIVE daemon: reads
# the resolved type:agent schema, writes+reads-back each value on an OPEN,
# project=e2e-test-fenced agent bead, and asserts a bogus value is REJECTED
# (negative control). Daemon-only; the fence keeps it from cold-starting a pod;
# self-SKIPs when no daemon is reachable or when a store override masks the enum
# as a plain string (the prod shape — unobservable there, meaningful on the
# fresh e2e daemon that does not seed type:agent).
if should_run "agent-state-enum"; then
  run_module "agent-state-enum" "./gardenio/tests/e2e/scripts/test-agent-state-enum.sh" || MODULE_RC=1
fi
if should_run "mr-merge-flow"; then
  run_module "mr-merge-flow" "./gardenio/tests/e2e/scripts/test-mr-merge-flow.sh" --namespace "$NAMESPACE" --simulate || MODULE_RC=1
fi
# mr-dispatch-gate (ka-0000006726, parent ka-0000000958): backfills the per-MR
# dispatch COOLDOWN (ka-0000001101, MR !6600) and the one-shot aging NUDGE on a
# persistently operator-gated MR (ka-0000007496, MR !6599) — both merged with
# unit-only coverage. Builds cmd/gb from source and drives `gb mr dispatch-gate`
# against the shared daemon + a loopback mock GitLab (the cooldown reads a head
# SHA): asserts same-SHA-in-window suppress, expiry + head-moved re-dispatch, and
# the aging-nudge fire-once (via the GB_DISPATCH_GATE_NUDGE_AGE test seam). This
# is the module that caught the undeclared one-shot-marker field (fixed alongside
# in kbeads config.go). Self-SKIPs LOUD without a Go toolchain / python3 / daemon.
if should_run "mr-dispatch-gate"; then
  run_module "mr-dispatch-gate" "./gardenio/tests/e2e/scripts/test-mr-dispatch-gate-e2e.sh" || MODULE_RC=1
fi
# trickle-gate (ka-0000001182, follow-up to ka-0000004967; bundle ka-0000005053
# §7 #4): behavioral proof of `gb mr trickle-gate`'s eligibility bar. Builds
# cmd/gb from source and drives it against the shared daemon + a loopback mock
# GitLab that serves fixture MRs: a fully-satisfied MR exits 0 (eligible), and a
# fixture missing each HARD item (provenance/small-diff/rationale/coverage/
# code-review/security/perf) exits 3 under --enforce with the blocking id named
# in the reason; a control asserts the advisory default (no --enforce) exits 0.
# The unit tests reach the pure aggregator; this reaches the resolve→decide→exit
# CLI path. Self-SKIPs LOUD without a Go toolchain / python3 / daemon.
if should_run "trickle-gate"; then
  run_module "trickle-gate" "./gardenio/tests/e2e/scripts/test-trickle-gate-e2e.sh" || MODULE_RC=1
fi
if should_run "slack-threads"; then
  run_module "slack-threads" "./gardenio/tests/e2e/scripts/test-slack-threads.sh" || MODULE_RC=1
fi
if should_run "slack-cards"; then
  run_module "slack-cards" "./gardenio/tests/e2e/scripts/test-slack-cards.sh" || MODULE_RC=1
fi
# group-dm (gd-bYT2TXgbbj, epic ka-0000003709): mpim participate/start/invite
# happy-path smoke. Pinned to shard 2 with the other slack-* modules and
# serialized there for Socket Mode fan-out safety. SKIP-passes when the real
# Slack rig (SLACK_BOT_TOKEN / BRIDGE_HTTP_URL / GROUP_DM_USERS) is absent —
# the mock has no conversations.open/members, so there is no mpim to drive.
if should_run "group-dm"; then
  run_module "group-dm" "./gardenio/tests/e2e/scripts/test-group-dm.sh" || MODULE_RC=1
fi
# Real-user Playwright Slack surface (ka-0000006398, epic ka-0000006991). Pinned
# to shard 2 with the other slack modules and serialized there for Socket Mode
# fan-out safety (see shard_for). Not in the smoke subset — slower, auth-fragile.
# SKIP-passes (never fails) until the test-Slack rig is provisioned: the module
# is inert without the storageState secret / scaffold / live bridge (mirrors the
# SLACK_BOT_TOKEN guard). SLACK_UI_REQUIRE=true (via the require-slack-ui
# dispatch param) flips an absent rig to a HARD FAIL on scheduled readiness runs.
if should_run "slack-ui"; then
  run_module "slack-ui" "./gardenio/tests/e2e/scripts/test-slack-ui.sh" || MODULE_RC=1
fi
if should_run "claim-workflow"; then
  run_module "claim-workflow" "./gardenio/tests/e2e/scripts/test-claim-workflow.sh" || MODULE_RC=1
fi
if should_run "search-filter"; then
  run_module "search-filter" "./gardenio/tests/e2e/scripts/test-search-filter.sh" || MODULE_RC=1
fi
if should_run "semantic-search"; then
  run_module "semantic-search" "./gardenio/tests/e2e/scripts/test-semantic-search.sh" || MODULE_RC=1
fi
if should_run "workspace"; then
  run_module "workspace" "./gardenio/tests/e2e/scripts/test-workspace.sh" || MODULE_RC=1
fi
if should_run "dm-news"; then
  run_module "dm-news" "./gardenio/tests/e2e/scripts/test-dm-news.sh" || MODULE_RC=1
fi
if should_run "news-alive-freshness"; then
  run_module "news-alive-freshness" "./gardenio/tests/e2e/scripts/test-news-alive-freshness.sh" || MODULE_RC=1
fi
if should_run "formula"; then
  run_module "formula" "./gardenio/tests/e2e/scripts/test-formula.sh" || MODULE_RC=1
fi
if should_run "formula-dedup"; then
  run_module "formula-dedup" "./gardenio/tests/e2e/scripts/test-formula-dedup.sh" || MODULE_RC=1
fi
if should_run "formula-lifecycle"; then
  run_module "formula-lifecycle" "./gardenio/tests/e2e/scripts/test-formula-lifecycle.sh" || MODULE_RC=1
fi
if should_run "formula-gate-pour"; then
  run_module "formula-gate-pour" "./gardenio/tests/e2e/scripts/test-formula-gate-pour.sh" || MODULE_RC=1
fi
if should_run "priority-sorting"; then
  run_module "priority-sorting" "./gardenio/tests/e2e/scripts/test-priority-sorting.sh" || MODULE_RC=1
fi
if should_run "session-resume"; then
  run_module "session-resume" "./gardenio/tests/e2e/scripts/test-session-resume.sh" || MODULE_RC=1
fi
if should_run "credential-rotation"; then
  run_module "credential-rotation" "./gardenio/tests/e2e/scripts/test-credential-rotation.sh" || MODULE_RC=1
fi
if should_run "cred-rotation-resume"; then
  run_module "cred-rotation-resume" "./gardenio/tests/e2e/scripts/test-cred-rotation-resume.sh" || MODULE_RC=1
fi
if should_run "thread-respawn-resume"; then
  run_module "thread-respawn-resume" "./gardenio/tests/e2e/scripts/test-thread-respawn-resume.sh" || MODULE_RC=1
fi
if should_run "threadtask-idle-hint"; then
  run_module "threadtask-idle-hint" "./gardenio/tests/e2e/scripts/test-threadtask-idle-hint.sh" || MODULE_RC=1
fi
# trigger-changed-to-seam (ka-0000002238, parent ka-0000000958): from-source
# go:build e2e test that drives the declarative bead-trigger changed_to /
# field_changed operators across the four-process daemon->SSE->bridge seam. It
# wires the REAL bridge.SSEStream + BeadStateTriggerSource (prod's
# trigger-engine wiring) to the LIVE daemon, PATCHes a watched trigger bead's
# `enabled` field, and asserts the per-field `changes` overlay (perFieldChanges
# -> SSE -> sse.go:398 -> matchOneCondition) fires changed_to/field_changed for
# real — a seam whose only prior tests all start downstream of the wire (pure
# perFieldChanges, hand-stuffed src.pending, or changes["title"] column). Keys
# every assertion on its own seeded bead; the seed carries fixture:e2e: so the
# shared shard engine never fires it. Self-SKIPs clean without the Go toolchain /
# daemon.
if should_run "trigger-changed-to-seam"; then
  run_module "trigger-changed-to-seam" "./gardenio/tests/e2e/scripts/test-trigger-changed-to-seam.sh" || MODULE_RC=1
fi
if should_run "dynworkflow-resume"; then
  run_module "dynworkflow-resume" "./gardenio/tests/e2e/scripts/test-dynworkflow-resume.sh" || MODULE_RC=1
fi
if should_run "pvc-gc-ttl"; then
  run_module "pvc-gc-ttl" "./gardenio/tests/e2e/scripts/test-pvc-gc-ttl.sh" || MODULE_RC=1
fi
if should_run "gvisor-ns-sweep"; then
  run_module "gvisor-ns-sweep" "./gardenio/tests/e2e/scripts/test-gvisor-ns-sweep.sh" || MODULE_RC=1
fi
if should_run "sidecar-injection"; then
  run_module "sidecar-injection" "./gardenio/tests/e2e/scripts/test-sidecar-injection.sh" || MODULE_RC=1
fi
if should_run "comments-events"; then
  run_module "comments-events" "./gardenio/tests/e2e/scripts/test-comments-events.sh" || MODULE_RC=1
fi
if should_run "graph-stats"; then
  run_module "graph-stats" "./gardenio/tests/e2e/scripts/test-graph-stats.sh" || MODULE_RC=1
fi
if should_run "orphans"; then
  run_module "orphans" "./gardenio/tests/e2e/scripts/test-orphans.sh" || MODULE_RC=1
fi
if should_run "bead-transfer"; then
  run_module "bead-transfer" "./gardenio/tests/e2e/scripts/test-bead-transfer.sh" || MODULE_RC=1
fi
if should_run "jack-system"; then
  run_module "jack-system" "./gardenio/tests/e2e/scripts/test-jack-system.sh" || MODULE_RC=1
fi
if should_run "molecule-system"; then
  run_module "molecule-system" "./gardenio/tests/e2e/scripts/test-molecule-system.sh" || MODULE_RC=1
fi
if should_run "triggers"; then
  run_module "triggers" "./gardenio/tests/e2e/scripts/test-triggers.sh" || MODULE_RC=1
fi
if should_run "sse-events"; then
  run_module "sse-events" "./gardenio/tests/e2e/scripts/test-sse-events.sh" || MODULE_RC=1
fi
if should_run "sse-reliability"; then
  run_module "sse-reliability" "./gardenio/tests/e2e/scripts/test-sse-reliability.sh" || MODULE_RC=1
fi
# Telepathy NATS "thinking" SSE feed (coverage-backfill gd-uWRZ9ia43N, epic
# ka-0000000958). The viewer NATS subscriber (thinking_hub.go) + unified
# /telepathy SSE page (telepathy.go) shipped with only httptest-unit coverage;
# the sibling sse-events module covers only kbeads /v1/events/stream, not this
# viewer feed. Asserts the page render + FeedEnabled state, a clean SSE degrade
# with no 500 (both branches), and — when the deployed viewer has NATS wired —
# a published coop.events.messages.<agent>.thinking event streaming as an
# `event: thinking` row plus its backfill-ring replay on a fresh connect.
# Self-SKIPs when the viewer Service is absent, so it is inert in the ephemeral
# e2e namespaces and asserts only against the long-lived singleton / prod.
if should_run "telepathy-feed"; then
  run_module "telepathy-feed" "./gardenio/tests/e2e/scripts/test-telepathy-feed.sh" || MODULE_RC=1
fi
if should_run "hooks-system"; then
  run_module "hooks-system" "./gardenio/tests/e2e/scripts/test-hooks-system.sh" || MODULE_RC=1
fi
# claudemore-hooks (ka-0000004452): REPLAY-tier hooks e2e — drives the REAL
# claude binary through claudemore record→replay ($0, hermetic) to assert T5
# (SessionStart prime consumption), T6 (Stop-gate BLOCK→ALLOW loop), and
# PreToolUse/PostToolUse → NATS. Self-SKIPs clean without the claude/Go toolchain.
if should_run "claudemore-hooks"; then
  run_module "claudemore-hooks" "./gardenio/tests/e2e/scripts/test-claudemore-hooks.sh" || MODULE_RC=1
fi
# subagent-stop (ka-0000004963, parent ka-0000000958): from-source Go probe —
# builds gb, embeds NATS, drives the REAL `gb hook relay` SubagentStop → real
# HookStream subscriber → HandleSubagentStop, and asserts the subagent's FINAL
# message reaches the stream chunk Output (the exact drop the gd-TdQnFDzJdo fix
# repaired; fails against pre-fix empty Output). Self-SKIPs clean without the Go
# toolchain.
if should_run "subagent-stop"; then
  run_module "subagent-stop" "./gardenio/tests/e2e/scripts/test-subagent-stop.sh" || MODULE_RC=1
fi
# gardener-start-argv-contract (ka-0000001242, parent ka-0000000958; related
# ka-0000006141): from-source go-test EMITTER — an //go:build e2e test calls the
# UNEXPORTED gardenerStartArgs (the exact symbol the @gardenio <epic-id> Slack
# entrypoint shells out with) and prints the argv; the module runs `gb <that
# argv>` against the live daemon on a FRESH epic and asserts a create lands, plus
# a negative control (same argv w/o the ack flag, second fresh epic → gate
# refuses, no schedule). Closes the gap where the hardcoded-argv replica in
# claudeless/gardener-mention-entrypoint.toml stays GREEN across an argv drift.
# Self-SKIPs clean without the Go toolchain / daemon.
if should_run "gardener-start-argv-contract"; then
  run_module "gardener-start-argv-contract" "./gardenio/tests/e2e/scripts/test-gardener-start-argv-contract.sh" || MODULE_RC=1
fi
# claudemore-agentchoice (ka-0000005944): REPLAY-tier agent-choice e2e — drives
# the REAL claude binary through claudemore record→replay against a stub beads
# daemon to assert the model's semantic CHOICES the claudeless tier structurally
# cannot: T2 right-bead claim, T3 decision divergence (fork→decision /
# status→none), T4 wrap-up blockers omission. OPT-IN (CLAUDEMORE_AGENTCHOICE_E2E=1,
# records real tokens) — self-SKIPs clean without the opt-in / claude / Go toolchain.
if should_run "claudemore-agentchoice"; then
  run_module "claudemore-agentchoice" "./gardenio/tests/e2e/scripts/test-claudemore-agentchoice.sh" || MODULE_RC=1
fi
if should_run "fleet-management"; then
  run_module "fleet-management" "./gardenio/tests/e2e/scripts/test-fleet-management.sh" || MODULE_RC=1
fi
if should_run "watch-remote"; then
  run_module "watch-remote" "./gardenio/tests/e2e/scripts/test-watch-remote.sh" || MODULE_RC=1
fi
if should_run "slack-dashboard"; then
  run_module "slack-dashboard" "./gardenio/tests/e2e/scripts/test-slack-dashboard.sh" || MODULE_RC=1
fi
if should_run "usage-tracking"; then
  run_module "usage-tracking" "./gardenio/tests/e2e/scripts/test-usage-tracking.sh" || MODULE_RC=1
fi
if should_run "agent-lifecycle"; then
  run_module "agent-lifecycle" "./gardenio/tests/e2e/scripts/test-agent-lifecycle.sh" || MODULE_RC=1
fi
if should_run "gitlab-bridge"; then
  run_module "gitlab-bridge" "./gardenio/tests/e2e/scripts/test-gitlab-bridge.sh" || MODULE_RC=1
fi
# MR-reopen drift sweep (ka-0000006088, coverage-backfill epic ka-0000000958).
# Builds + runs cmd/mr-reopen-probe: the REAL MRReopenDriftReconciler over the
# REAL beadsapi client against the LIVE shard daemon, with a STUB GitLab. Asserts
# the reopen leg (closed bead + open MR ⇒ re-opened) plus the merged /
# mr_close_override / type=merge-request / re-verify-race negatives — the
# direction test-mr-merge-flow.sh never covered. HARD gate; skips LOUD without a
# Go toolchain or a reachable daemon. Shares shard 0 with the other Go probes.
if should_run "mr-reopen-drift"; then
  run_module "mr-reopen-drift" "./gardenio/tests/e2e/scripts/test-mr-reopen-drift.sh" || MODULE_RC=1
fi
# gd-close merge→close race live re-check (ka-0000007123, coverage-backfill epic
# ka-0000000958, fix bug ka-0000004113). Drives the REAL `kd close` binary
# against the LIVE shard daemon (seeded like the bridge would: an issue bead + a
# canonical merge-request bead reading mr_state=opened) with GitLab replaced by a
# Python-stdlib stub the seeded mr_url points at. Asserts the acquittal leg (live
# `merged` acquits a lagging `opened` projection + emits the "has not caught up
# yet" note), the refusal-preserved negative (live `opened` still refuses), the
# three degradation legs (no-token / HTTP-500 / unparseable-body all fall back to
# refusal, never acquit), and the --allow-open-mr override stamp — the acquittal
# path test-mr-reopen-drift.sh never covered. HARD gate; skips LOUD without kd,
# python3, or a reachable daemon. No probe binary (kd + python3 only).
if should_run "mr-close-live"; then
  run_module "mr-close-live" "./gardenio/tests/e2e/scripts/test-mr-close-live-recheck.sh" || MODULE_RC=1
fi
# E-PROV forward-provenance + host-scoped dark bridge (ka-0000005448). Builds +
# runs cmd/gitlab-prov-probe (REAL GitLabSync host-scope / webhook merge gate /
# backstop poller / acceptance reconciler / peel-backlink guard, wired against
# STUB GitLab httptest servers + an in-process mock daemon). Hermetic (no pods,
# no shared-daemon writes) so it is a HARD gate — any leg failure reddens the
# shard. Co-located with the jira-conv-probe build on shard 0 (see shard_for).
if should_run "gitlab-dark-provenance"; then
  run_module "gitlab-dark-provenance" "./gardenio/tests/e2e/scripts/test-gitlab-dark-provenance.sh" || MODULE_RC=1
fi
# GitLab inbound-webhook dark-hook deadman (ka-0000005729, coverage-backfill epic
# ka-0000000958). Wraps a from-source `go test -run TestGitLabWebhookDeadmanE2E`
# against the REAL GitLabWebhookMonitor.evaluate + the REAL
# GitLabWebhookHandlerWithConfig over a REAL promhttp /metrics endpoint. Proves
# the served gardenio_gitlab_webhook_dark flips 0→1 (with a real-delivery
# anti-vacuity control reading 0 while deliveries flow) AND that the emitted
# series names still match the metric names the SHIPPED PrometheusRule expr keys
# off — the rename-either-side-silently-unfires guard the unit + helm render tests
# each miss. Hermetic (httptest only, no pods, no shared-daemon writes) so it is a
# HARD gate. Co-located with the other from-source probes on shard 0 (see
# shard_for). SKIPs LOUD without a Go toolchain / controller source.
if should_run "gitlab-webhook-deadman"; then
  run_module "gitlab-webhook-deadman" "./gardenio/tests/e2e/scripts/test-gitlab-webhook-deadman.sh" || MODULE_RC=1
fi
# JIRA inbound-webhook dark-hook deadman (ka-0000004554, coverage-backfill epic
# ka-0000000958). The exact Jira mirror of the GitLab sibling above: wraps a
# from-source `go test -run TestJiraWebhookDeadmanE2E` against the REAL
# JiraWebhookMonitor.evaluate + the REAL JiraWebhookHandler over a REAL promhttp
# /metrics endpoint. Proves the served gardenio_jira_webhook_dark flips 0→1 (with a
# real-delivery anti-vacuity control reading 0 while deliveries flow, and
# counter.calls==0 proving that 0 is the healthy early-out not a quiet-upstream 0)
# AND that the emitted series names still match the metric names the SHIPPED
# PrometheusRule expr keys off — the rename-either-side-silently-unfires guard the
# unit + helm render tests each miss. Hermetic (httptest only, no pods, no
# shared-daemon writes) so it is a HARD gate. Co-located with the other from-source
# probes on shard 0 (see shard_for). SKIPs LOUD without a Go toolchain / controller
# source.
if should_run "jira-webhook-deadman"; then
  run_module "jira-webhook-deadman" "./gardenio/tests/e2e/scripts/test-jira-webhook-deadman.sh" || MODULE_RC=1
fi
# slack-bridge cold-respawn PHASE-BREAKDOWN metrics (ka-0000003422, coverage-backfill
# epic ka-0000000958; feature merge 8edcbe2e1 / task ka-0000001457). Same shape as the
# two deadman siblings above: wraps a from-source `go test -run
# TestSlackBridgeRespawnPhaseE2E` against the REAL recordRespawnPhase over a REAL
# promhttp /metrics endpoint (the DefaultGatherer buildMetricsServer mounts on the
# bridge's :9093). A live-bridge port-forward is impossible here — run-shard's
# BRIDGE_HTTP_URL tunnel is the :8090 escalation mux (/metrics is 404 there; the real
# endpoint is the un-forwarded :9093), and the two series are a promauto *Vec that
# marshals NOTHING until a real cold respawn fires, which no e2e module drives. Proves
# the served gardenio_slackbridge_respawn_phase_seconds/_slow_total exposure (with an
# absent-before/present-after anti-vacuity control — the registry-regression guard),
# phase/role label bounding (cardinality guard), slow-counter monotonicity by value,
# AND that the emitted series names still match the strings the SHIPPED vmalert rule
# (SlackBridgeRespawnPhaseSlow) keys off — the rename-either-side-silently-unfires
# guard the unit + helm render tests each miss. Hermetic (httptest only, no pods, no
# shared-daemon writes) so it is a HARD gate. Co-located with the other from-source
# probes on shard 0 (see shard_for). SKIPs LOUD without a Go toolchain / controller
# source.
if should_run "slackbridge-respawn-phase-metrics"; then
  run_module "slackbridge-respawn-phase-metrics" "./gardenio/tests/e2e/scripts/test-slackbridge-respawn-phase-metrics.sh" || MODULE_RC=1
fi
# slackmock-loop (gd-RhOuCeCYUC, epic gd-L1HXcTGzDV §C1). Closes the slackmock
# validation loop end-to-end: compiled fixture + lib-slackmock.sh round-trip +
# the bridge's REAL production Slack client honouring the SLACK_API_URL override
# (set in NO ci/e2e/helm flow before this — the redirect that makes slackmock a
# fixture was never exercised). Hermetic (localhost mock + httptest, no pods) so
# it is a HARD gate. Co-located with the from-source probes on shard 0 (see
# shard_for). SKIPs LOUD without a Go toolchain / the slackmock + controller source.
if should_run "slackmock-loop"; then
  run_module "slackmock-loop" "./gardenio/tests/e2e/scripts/test-slackmock-loop.sh" || MODULE_RC=1
fi
# stranded-recovered-sweep (gd-DMvno7uS3J, coverage-backfill epic ka-0000000958).
# Wraps a from-source `go test -tags e2e -run TestReconcileStrandedRecoveredE2E`
# that builds the REAL podmanager.K8sManager against the LIVE apiserver, plants
# husk pods in an ISOLATED SCRATCH NAMESPACE, and calls ReconcileStrandedRecovered
# directly — deterministic proof of the level-triggered (every-5-min) stranded-pod
# recovery sweep that the sibling stale-vkey/keyless legs (ka-0000001464 Scenario
# 5) did not cover. Positive (project pool regained a healthy sibling → husk
# deleted) + negative control (pool still fully exhausted → husk left alone).
# From-source Go + live-cluster → shard 0 with the other probes and pool-recycle.
# SKIPs LOUD without a Go toolchain / reachable cluster.
if should_run "stranded-recovered-sweep"; then
  run_module "stranded-recovered-sweep" "./gardenio/tests/e2e/scripts/test-stranded-recovered-sweep.sh" || MODULE_RC=1
fi
# grafana-bridge dashboard-sync + conflict-policy (ka-0000002319, coverage-backfill
# epic ka-0000000958). Builds + runs cmd/grafana-bridge-probe (REAL
# GrafanaConfigMapWriter against a fake k8s clientset / REAL GrafanaSync through
# the REAL SSE stream / REAL GrafanaReadback against a STUB Grafana, wired to an
# in-process mock daemon). Exercises the beads→ConfigMap write path, the backfill,
# and all three read-back ConflictPolicy branches (beads-wins / grafana-wins /
# manual) — the read-back poll is OFF on the fleet (grafana.url empty → write-only)
# so a port-forward can assert none of it. Hermetic (no pods, no shared-daemon
# writes, no live Grafana) so it is a HARD gate — any leg failure reddens the
# shard. Co-located with the other from-source probes on shard 0 (see shard_for).
if should_run "grafana-bridge"; then
  run_module "grafana-bridge" "./gardenio/tests/e2e/scripts/test-grafana-bridge.sh" || MODULE_RC=1
fi
if should_run "jira-bridge"; then
  run_module "jira-bridge" "./gardenio/tests/e2e/scripts/test-jira-bridge.sh" || MODULE_RC=1
fi
# Dark-pod Jira write-token fence, NETWORK leg (ka-0000007263, coverage-backfill
# epic ka-0000000958; fence bead ka-0000005242, R7 epic ka-0000004670). Asserts
# the DEPLOYED NetworkPolicies actually deny a dark pod egress to real upstream
# Jira: the chart render tests prove the TEMPLATE emits Atlassian CIDRs, but
# nothing proved those CIDRs still cover acme.atlassian.net or that no OTHER
# policy re-grants what this one subtracts (NetworkPolicy is allow-UNION). Pure
# kubectl + set arithmetic — no pod spawn, no live connect() (a timeout would be
# indistinguishable from a healthy fence), so it is fast and deterministic.
# SKIPS LOUD on a namespace deployed before the fence (2026.217.x); a present
# but malformed policy is a HARD failure.
if should_run "dark-jira-fence"; then
  run_module "dark-jira-fence" "./gardenio/tests/e2e/scripts/test-dark-jira-fence.sh" || MODULE_RC=1
fi
# dark-jira-fence-pod (ka-0000000981): the POD leg of the same fence, the
# companion to the NETWORK leg above. Proves scrubDarkForbiddenEnv (podspec.go)
# strips the plaintext JIRA_API_TOKEN out of a DEPLOYED dark agent pod. Unlike
# the network leg (pure kubectl set-arithmetic, no pod spawn), the pod leg needs
# a token-injection rig: an ephemeral project whose EnvVars carry a DUMMY
# JIRA_API_TOKEN, then a DARK pod (token must be ABSENT) AND a NON-DARK control
# pod (token must be PRESENT) against the SAME project — without the control the
# "absent" is vacuous (the e2e namespace injects no Jira token at all). Two real pod
# spawns → OPT-IN like test-gb-stop-suspend: gated on DARK_FENCE_POD_E2E=1 so it
# is inert in the standard shards (which spawn NO agent pods). It SKIPs LOUD —
# never silently passes — when the flag is set but the fence is not deployed (a
# namespace older than 2026.217.x lacks the dark-agent-egress NP the scrub ships
# with) or the substrate is missing. Run via the dedicated dispatch or
# `DARK_FENCE_POD_E2E=1 ONLY=dark-jira-fence-pod`.
if [ "${DARK_FENCE_POD_E2E:-0}" = "1" ]; then
  if should_run "dark-jira-fence-pod"; then
    run_module "dark-jira-fence-pod" "./gardenio/tests/e2e/scripts/test-dark-jira-fence-pod.sh" || MODULE_RC=1
  fi
fi
# Conversational round-trip gate (ka-0000003999, epic ka-0000007401): builds +
# runs cmd/jira-conv-probe (REAL webhook/mention/reply/squawk-forward/resolver
# wired against a STUB Jira + in-process mock daemon, jiraWebhookDevMode) to walk
# mention->spawn->reply->squawk->follow-up + the FSM negative. Hermetic (no pods,
# no shared-daemon writes, no sandbox rig) so it is a HARD gate — any leg failure
# reddens the shard. Co-located with jira-bridge on shard 0 (see shard_for).
if should_run "jira-conversational"; then
  run_module "jira-conversational" "./gardenio/tests/e2e/scripts/test-jira-conversational.sh" || MODULE_RC=1
fi
# Sync-back deny-by-default gate (ka-0000005719, coverage-backfill epic
# ka-0000000958): builds + runs cmd/jira-syncback-probe (REAL bridge.JiraSync +
# real SSEStream against a STUB Jira) to walk the deny-by-default project
# allowlist (non-allowlisted + FSM-block deny) and the source:jira-dark
# early-return, in BOTH handleUpdated and handleClosed. Each leg pairs its
# deny/dark target with an allowlisted PE positive-control so a 0-writes pass can
# never be vacuous (ka-0000005143). Hermetic (no pods, no shared-daemon writes,
# no sandbox rig) so it is a HARD gate. Co-located with the jira probes on shard 0.
if should_run "jira-syncback-denybydefault"; then
  run_module "jira-syncback-denybydefault" "./gardenio/tests/e2e/scripts/test-jira-syncback-denybydefault.sh" || MODULE_RC=1
fi
if should_run "jira-write-control-sandbox"; then
  run_module "jira-write-control-sandbox" "./gardenio/tests/e2e/scripts/test-jira-write-control-sandbox.sh" || MODULE_RC=1
fi
if should_run "jira-outbox-negative"; then
  run_module "jira-outbox-negative" "./gardenio/tests/e2e/scripts/test-jira-outbox-negative.sh" || MODULE_RC=1
fi
# POSITIVE/apply sibling of jira-outbox-negative (gd-gYv0BhZIjc): asserts a VALID
# intent reaching Synced is STRICTLY corroborated by the effect LANDING on the
# FSM ticket (browse-read) — closing the "Synced != applied" false-green that hid
# the P0 silent no-op (gd-d7U7jd6cyH / gd-l0ffwDFvQ8). Skips clean when the rig or
# the MANDATORY gardenio@ browse seat is absent. Hash-assigned (NOT in the smoke
# subset), so it gates the scheduled/full + release E2E, not main-push smoke.
if should_run "jira-outbox-positive"; then
  run_module "jira-outbox-positive" "./gardenio/tests/e2e/scripts/test-jira-outbox-positive.sh" || MODULE_RC=1
fi
# FAIL-CLOSED outbox rule-JSON reconciliation gate (gd-dq6raBnxLV). Unlike the
# jira-outbox-negative sibling (which skips clean when its live rig is absent),
# this gate FAILS CLOSED when the operator Export-JSON is absent from
# gardenio/tests/e2e/fixtures/outbox-rules/ — a RED "awaiting operator artifact"
# signal, by design. Hash-assigned (NOT in the smoke subset), so it gates the
# scheduled/full E2E without blocking main-push smoke or per-MR pipelines.
if should_run "outbox-rules-json"; then
  run_module "outbox-rules-json" "./gardenio/tests/e2e/scripts/verify-outbox-rules-json.sh" || MODULE_RC=1
fi
if should_run "reconciler"; then
  run_module "reconciler" "./gardenio/tests/e2e/scripts/test-reconciler.sh" || MODULE_RC=1
fi
if should_run "pool-manager"; then
  run_module "pool-manager" "./gardenio/tests/e2e/scripts/test-pool-manager.sh" || MODULE_RC=1
fi
if should_run "pool-assign-attribution"; then
  run_module "pool-assign-attribution" "./gardenio/tests/e2e/scripts/test-pool-assign-attribution.sh" || MODULE_RC=1
fi
if should_run "pool-recycle"; then
  run_module "pool-recycle" "./gardenio/tests/e2e/scripts/test-pool-recycle.sh" || MODULE_RC=1
fi
# lambda_microvm execution backend (ka-0000006937, parent ka-0000000958, anchor
# epic ka-0000005010): the agent runtime that landed + prod-cutover'd in
# 2026-07-17..07-31 had ZERO black-box coverage — ~30 Go unit files but no e2e
# module and no claudeless scenario. Asserts the seven decision tables a
# regression would silently break: execution_target resolution (agent>role>
# project>default, unknown→kubernetes so no agent is ever stranded), spawn
# fail-closed (a microvm-targeted spawn NEVER falls through to a pod), native
# suspend-on-stop eligibility + its spawn/teardown twin-predicate lockstep,
# resume-on-message routing (resume the SAME VM in place; closing the bead would
# fire AgentDone→Terminate and destroy the resume target), AWS-state liveness
# (SUSPENDED is Live, not Terminal — the structural root of standby≠leak), GC
# standby≠leak + the max-standby-age cap, and the warm-pool cost opt-in +
# IdlePolicy AWS-min-60 floor (ka-0000005755: a 0 there 400-blocked EVERY prod
# launch). The CONTRACT leg needs no substrate and runs in every shard —
# required here because the backend is default-off everywhere and, when on,
# needs a cross-account AWS launcher role plus real MicroVM spend, so launch/
# suspend/resume/GC are structurally unobservable in CI (both existing
# "E2E validation" docs are hand-run operator runbooks whose §Status records the
# live run was never executed). The SUBSTRATE leg self-SKIPs unless
# MICROVM_SUBSTRATE_E2E=1; it asserts the live fail-closed routing invariant
# (agent_state=failed + NO pod) on a cluster with no MicroVM backend.
if should_run "microvm-execution-target"; then
  run_module "microvm-execution-target" "./gardenio/tests/e2e/scripts/test-microvm-execution-target.sh" || MODULE_RC=1
fi
if should_run "scheduler"; then
  run_module "scheduler" "./gardenio/tests/e2e/scripts/test-scheduler.sh" || MODULE_RC=1
fi
if should_run "trigger-system"; then
  run_module "trigger-system" "./gardenio/tests/e2e/scripts/test-trigger-system.sh" || MODULE_RC=1
fi
if should_run "trigger-engine-smoke"; then
  run_module "trigger-engine-smoke" "./gardenio/tests/e2e/scripts/test-trigger-engine-smoke.sh" || MODULE_RC=1
fi
# trigger-action-fidelity (ka-0000006362): drives the REAL bead-source
# evaluation→fireAction loop via cmd/trigger-probe (build-trigger-probe rides
# every shard's use: list) to assert three action behaviors with unit-only
# coverage — create_bead field fidelity + engine allowlist (ka-0000003075),
# role→role: LABEL mirroring (ka-0000002971), and the assert fire-on-violation
# path incl. fail-closed (ka-0000008157). Skips clean (LOUD) with no daemon/probe.
if should_run "trigger-action-fidelity"; then
  run_module "trigger-action-fidelity" "./gardenio/tests/e2e/scripts/test-trigger-action-fidelity.sh" || MODULE_RC=1
fi
# trigger-cooldown-event-race (ka-0000002850, parent ka-0000000958): drives the
# bead-source EventReady cooldown-bypass (triggers.go:2093 "pending event",
# bug ka-0000001376) via cmd/trigger-probe against the LIVE shared daemon with
# NO --ignore-cooldown — the one path test-trigger-action-fidelity deliberately
# routes AROUND via the cooldownIgnored test seam. Two-probe recipe: a non-match
# stamps last_checked (real non-firing eval, prod-faithful), then a MATCH inside
# the window must fire through the EventReady bypass (positive leg), while a
# NON-match in the same window must NOT bypass (negative leg — the false-positive
# direction). Skips clean (LOUD) with no daemon/probe.
if should_run "trigger-cooldown-event-race"; then
  run_module "trigger-cooldown-event-race" "./gardenio/tests/e2e/scripts/test-trigger-cooldown-event-race.sh" || MODULE_RC=1
fi
# trigger-multi-cooldown-event-race (ka-0000000405, parent ka-0000000958): the
# MULTI-source sibling of trigger-cooldown-event-race. That module covers the
# engine "pending event" bypass end-to-end through BeadStateTriggerSource; this
# one drives the SAME gate through MultiTriggerSource.EventReady (which delegates
# to scheduler.HasPendingEventMatch) via cmd/trigger-probe --source=multi, so the
# multi wrapper's own body is covered on a live daemon (it was unit-only before).
# Same two-probe recipe, NO --ignore-cooldown: positive (match in window fires via
# bypass) + negative (non-match in window stays silent). Skips clean (LOUD) with
# no daemon/probe, or when a stale probe image predates the --source=multi mode.
if should_run "trigger-multi-cooldown-event-race"; then
  run_module "trigger-multi-cooldown-event-race" "./gardenio/tests/e2e/scripts/test-trigger-multi-cooldown-event-race.sh" || MODULE_RC=1
fi
# gitlab-pipeline-refpattern (ka-0000002595, parent ka-0000000958): drives the
# REAL GitLabTriggerSource + TriggerEngine via cmd/gitlab-pipeline-trigger-probe
# (build-gitlab-pipeline-trigger-probe rides shard 0's use: list) to assert that
# a gitlab-pipeline trigger's pipeline_ref_pattern GATES firing — matching refs
# fire, non-matching refs do NOT (the negative/runaway-guard leg MR !6601 left
# with unit-only coverage). Hermetic (stub GitLab httptest + mock daemon, no
# pods, no shared-daemon writes); a HARD gate. Skips clean (LOUD) with no
# toolchain/probe.
if should_run "gitlab-pipeline-refpattern"; then
  run_module "gitlab-pipeline-refpattern" "./gardenio/tests/e2e/scripts/test-gitlab-pipeline-refpattern.sh" || MODULE_RC=1
fi
# gitlab-pipeline-minduration (ka-0000001474, parent ka-0000000958): drives the
# REAL GitLabTriggerSource + TriggerEngine via cmd/gitlab-pipeline-minduration-
# probe (build-gitlab-pipeline-minduration-probe rides shard 0's use: list) to
# assert that a gitlab-pipeline trigger's min_duration ("slow pipeline") GATES
# firing — pipelines at/over the threshold fire, pipelines under it do NOT (the
# runaway-guard leg), and null/unknown or detail-fetch-failed durations NEVER
# fire (fail closed). Hermetic (stub GitLab httptest + mock daemon, no pods, no
# shared-daemon writes); a HARD gate. Skips clean (LOUD) with no toolchain/probe.
if should_run "gitlab-pipeline-minduration"; then
  run_module "gitlab-pipeline-minduration" "./gardenio/tests/e2e/scripts/test-gitlab-pipeline-minduration.sh" || MODULE_RC=1
fi
if should_run "session-archival"; then
  run_module "session-archival" "./gardenio/tests/e2e/scripts/test-session-archival.sh" || MODULE_RC=1
fi
if should_run "secret-reconciler"; then
  run_module "secret-reconciler" "./gardenio/tests/e2e/scripts/test-secret-reconciler.sh" || MODULE_RC=1
fi
if should_run "claudeless"; then
  run_module "claudeless" "./gardenio/tests/e2e/scripts/test-claudeless.sh" || MODULE_RC=1
fi
if should_run "local-mode"; then
  # Archive per-scenario gb-agent-start logs into the shard's junit-results
  # dir so CI preserves them as artifacts. Without this, the only surviving
  # diagnostic is whatever the script printed inline. See ka-0000003076.
  mkdir -p "$JUNIT_DIR/local-mode-logs" 2>/dev/null || true
  export LOG_ARCHIVE_DIR="$JUNIT_DIR/local-mode-logs"
  run_module "local-mode" "./gardenio/tests/e2e/scripts/test-local-mode.sh" || MODULE_RC=1
  unset LOG_ARCHIVE_DIR
fi
# Pours each gardenio/formulas/onboarding formula and has a claudeless local
# agent (`gb agent start --local`, as local-mode) work it to its first gate.
# Self-skips without gb/kd/coop/claudeless (gd-VJoT78CBjV).
if should_run "onboarding-formulas"; then
  run_module "onboarding-formulas" "./gardenio/tests/e2e/scripts/test-onboarding-formulas.sh" || MODULE_RC=1
fi
if should_run "coop-local-mode"; then
  run_module "coop-local-mode" "./gardenio/tests/e2e/scripts/test-coop-local-mode.sh" || MODULE_RC=1
fi
# Local runner scenarios (local-agents spec §11 a, b+e, c+d, f; gd-jW7KJ7ZdNy).
# The runner ones self-skip without an admin token (type=runner_invite is
# admin-gated); listener's ingress half needs RUNNER_PUBLIC_URL.
if should_run "local-runner-mail"; then
  run_module "local-runner-mail" "./gardenio/tests/e2e/scripts/test-local-runner-mail.sh" || MODULE_RC=1
fi
if should_run "local-runner-lifecycle"; then
  mkdir -p "$JUNIT_DIR/local-runner-logs" 2>/dev/null || true
  export LOG_ARCHIVE_DIR="$JUNIT_DIR/local-runner-logs"
  run_module "local-runner-lifecycle" "./gardenio/tests/e2e/scripts/test-local-runner-lifecycle.sh" || MODULE_RC=1
  unset LOG_ARCHIVE_DIR
fi
if should_run "local-runner-escalation"; then
  mkdir -p "$JUNIT_DIR/local-runner-logs" 2>/dev/null || true
  export LOG_ARCHIVE_DIR="$JUNIT_DIR/local-runner-logs"
  run_module "local-runner-escalation" "./gardenio/tests/e2e/scripts/test-local-runner-escalation.sh" || MODULE_RC=1
  unset LOG_ARCHIVE_DIR
fi
if should_run "local-runner-listener"; then
  run_module "local-runner-listener" "./gardenio/tests/e2e/scripts/test-local-runner-listener.sh" || MODULE_RC=1
fi
# coop live-usage seam: the dedicated Prometheus metrics port (:9093 in gardenio
# pods) + the /api/v1/agent live-usage fields statusreporter mirrors onto agent
# beads (coverage-backfill ka-0000005996, epic ka-0000000958). NOTHING scraped
# coop's metrics port before this — controller-metrics scrapes the CONTROLLER's
# :9092 and fleet-metrics scrapes kbeads' :9091; the two apparent ":9093" hits
# elsewhere in the suite are `local port=19093`, an unrelated forward port. It
# also closes the other half of the hole: agent-cost-context SEEDS
# context_tokens/live_cost_usd by hand via kd and asserts the read path, but
# nothing proved anything COMPUTES them — this drives the producer end from a
# live coop session. Asserts an EXACT tool-call histogram count (3 then 4, not
# merely "> 0" — the shape that would have caught ka-0000004093, where a
# restricted hook matcher left the histogram registered-but-dead fleet-wide),
# the unauthenticated-/metrics-only auth contract, and /metrics vs /api/v1/agent
# agreement on request_count. Also asserts the llm_request latency histogram BY
# VALUE — EXACTLY session_request_count-1 observations (ka-0000005556 fixed, so
# claudeless's +00:00 timestamps now parse). One fixture limit remains asserted
# as SHAPE and skipped by VALUE: cost_usd (needs a LiteLLM response_cost key
# claudeless never emits).
# Hermetic: launches its own coop+claudeless on loopback, needs no cluster or
# daemon, and self-SKIPs when coop/claudeless are not on PATH.
if should_run "coop-live-usage"; then
  run_module "coop-live-usage" "./gardenio/tests/e2e/scripts/test-coop-live-usage.sh" || MODULE_RC=1
fi
if should_run "escalation-crud"; then
  run_module "escalation-crud" "./gardenio/tests/e2e/scripts/test-escalation-crud.sh" || MODULE_RC=1
fi
if should_run "escalation-approval"; then
  run_module "escalation-approval" "./gardenio/tests/e2e/scripts/test-escalation-approval.sh" || MODULE_RC=1
fi
if should_run "escalation-execution"; then
  run_module "escalation-execution" "./gardenio/tests/e2e/scripts/test-escalation-execution.sh" || MODULE_RC=1
fi
if should_run "escalation-slack-notify"; then
  run_module "escalation-slack-notify" "./gardenio/tests/e2e/scripts/test-escalation-slack-notify.sh" || MODULE_RC=1
fi
if should_run "escalation-v2"; then
  run_module "escalation-v2" "./gardenio/tests/e2e/scripts/test-escalation-v2.sh" || MODULE_RC=1
fi
# Escalation AUTO-REAPER (ka-0000006112, parent ka-0000000958): the sweep that
# auto-closes an escalation satisfied OUT-OF-BAND (011b9e33b/ka-0000004105
# MR-witness, 00b86ed3c/ka-0000007203 orchestration witness) plus the
# never-closing MR-landed review flag (ccfc8411a/ka-0000002563). The five
# escalation modules above exercise create/approve/execute/resolve and NEVER
# assert auto-close, so a regression is invisible in both directions: under-reap
# re-nudges operators with dead asks (~15% phantom rate), over-reap silently
# closes a live P0. A live test is structurally impossible — gitlab-bridge (the
# reaper's host) is `enabled: false` in the e2e namespace, the reaper HARD-SKIPS
# any e2e- actor's escalations (isE2EEscalation, and KD_ACTOR defaults to
# e2e-test), and the bridge has no sweep-trigger endpoint + a 10m interval. So
# this follows the gitlab-dark-provenance precedent: a from-source probe drives
# the REAL EscalationReaper against stub GitLab + a mock daemon. Hermetic (no
# pods, no shared-daemon writes) and a HARD gate in every shard; it self-SKIPs
# LOUD only if the Go toolchain/source is missing. Shard 0 like the other
# from-source probes, so it can ride a pre-built binary when one is on PATH.
if should_run "escalation-auto-reaper"; then
  run_module "escalation-auto-reaper" "./gardenio/tests/e2e/scripts/test-escalation-auto-reaper.sh" || MODULE_RC=1
fi
# agent-feedback-writepath (ka-0000008183): backfills the always-on feedback
# endpoint (de29900/ka-0000002210) — POST /api/slack/agent-feedback → a stable
# type=rating feedback bead, plus the daemon-enforced (rater, rated_bead_id)
# idempotent upsert that the feedback→bug source (ka-0000007151) relies on. Like
# the escalation-auto-reaper backfill above, a black-box HTTP test is impossible:
# the slack-bridge (the handler's host) is `enabled: false` in the EPHEMERAL e2e
# namespace — only a long-lived deploy runs it — so there is no bridge to
# POST to. So this drives a
# from-source probe that wires the REAL AgentFeedbackAPI handler to a
# beadsapi.Client pointed at the LIVE daemon, asserting BOTH the handler's
# CreateBeadRequest shape AND the real preCreateRating upsert end-to-end (the
# unit test's mock daemon implements neither). SKIPs LOUD-clean if the Go
# toolchain/source or the live daemon is absent.
if should_run "agent-feedback-writepath"; then
  run_module "agent-feedback-writepath" "./gardenio/tests/e2e/scripts/test-agent-feedback-writepath.sh" || MODULE_RC=1
fi
if should_run "litellm-cost-tracking"; then
  run_module "litellm-cost-tracking" "./gardenio/tests/e2e/scripts/test-litellm-cost-tracking.sh" || MODULE_RC=1
fi
# litellm-fallbacks (ka-0000003835, epic ka-0000000958): asserts the DEPLOYED
# LiteLLM router carries the router_settings.fallbacks arms — specifically the
# crew-e2e opus-4-8 -> opus-5 spill (ka-0000007458) — nested under
# router_settings (not a silently-ignored top-level key, ka-0000005911) with
# every primary+target resolving to a live model group. The chart-render unit
# test litellm_router_alias_test.go renders DEFAULT values and CANNOT catch a
# stale deploy / overlay override that drops the arm; this reads the live
# /get/config/callbacks. Read-only, light; self-SKIPs when litellm.enabled=false
# like litellm-cost-tracking beside it.
if should_run "litellm-fallbacks"; then
  run_module "litellm-fallbacks" "./gardenio/tests/e2e/scripts/test-litellm-fallbacks.sh" || MODULE_RC=1
fi
if should_run "agent-cost-context"; then
  run_module "agent-cost-context" "./gardenio/tests/e2e/scripts/test-agent-cost-context.sh" || MODULE_RC=1
fi
if should_run "config-role-model"; then
  run_module "config-role-model" "./gardenio/tests/e2e/scripts/test-config-role-model.sh" || MODULE_RC=1
fi
if should_run "fleet-metrics"; then
  run_module "fleet-metrics" "./gardenio/tests/e2e/scripts/test-fleet-metrics.sh" || MODULE_RC=1
fi
# Controller :9092 registry scrape (gd-ckUXI6EOld, epic ka-0000000958). Port-forwards
# svc/${NAMESPACE}-agents :9092 and asserts the controller-emitted families that NO
# other module covers: reconcile_total (HARD, always-on reconcile-loop counter) and —
# after a spend-safe seed→close spawn/stop cycle — lifecycle_events / session_duration
# with their documented labels (pod_phase_transitions asserted observationally). Distinct
# surface from fleet-metrics (kbeads :9091 gardenio_beads_*) and reconciler (log-JSON
# sync_runs). Self-SKIPs when the controller Service is absent; the event-family "appear"
# asserts are non-fatal PENDING unless CONTROLLER_METRICS_STRICT=1 (flip once a live run
# confirms they populate reliably). Hashes to shard 1 alongside the warm-pool modules.
if should_run "controller-metrics"; then
  run_module "controller-metrics" "./gardenio/tests/e2e/scripts/test-controller-metrics.sh" || MODULE_RC=1
fi
# Controller idle/active measurement scrape (ka-0000003235, epic ka-0000000958).
# Closes the HEAD-verified gap that NO e2e scrapes the two Phase-0 idle/active
# gauges idle_metrics.go emits (ka-0000006778) — gardenio_agents_by_activity{project,
# activity} and gardenio_agent_reserved_memory_bytes{project,activity}; coverage was
# only the in-process classifier unit test. Port-forwards svc/${NAMESPACE}-agents
# :9092 and HARD-asserts both gauges are present with {project,activity} labels +
# activity∈{active,idle}, plus two structural invariants that hold on any warm
# controller (pre-seed symmetry: every project carries BOTH activity buckets;
# value-join: reserved>0 ⇒ count>0, and reserved is a byte count ≥1 MiB). A
# spend-safe seed (image=registry.invalid → ImagePullBackOff, zero spend) then
# drives OUR ephemeral project into both series and asserts reserved-memory>0 and
# MOVES with agent count — non-fatal PENDING unless IDLE_METRICS_STRICT=1 (flip
# once a live run confirms the drive lands). Distinct surface from
# controller-metrics (reconcile_total/lifecycle/session_duration on the SAME :9092)
# and fleet-metrics (kbeads :9091). Self-SKIPs when the controller Service is
# absent. Pinned to shard 2 beside controller-metrics — same :9092 scrape idiom.
if should_run "idle-active-metrics"; then
  run_module "idle-active-metrics" "./gardenio/tests/e2e/scripts/test-idle-active-metrics.sh" || MODULE_RC=1
fi
# Session spend attribution (gd-NU1IbgLk5v): wraps targeted `go test -run` against
# the REAL controller reconciler (captureBaseline/reconcileAtClose + the served
# gardenio_agent_spend_usd_total counter, over a real promhttp /metrics scrape) and
# the REAL bridge usage consumer (coop cost-free usage NATS model, over an embedded
# NATS server). Hermetic HARD gate — no pods; SKIPs LOUD only if the Go toolchain
# is absent. Distinct surface from fleet-metrics / litellm-cost-tracking /
# agent-cost-context / usage-tracking (see the module header).
if should_run "session-spend-attribution"; then
  run_module "session-spend-attribution" "./gardenio/tests/e2e/scripts/test-session-spend-attribution.sh" || MODULE_RC=1
fi
if should_run "bridge-fleet-token-rotation"; then
  run_module "bridge-fleet-token-rotation" "./gardenio/tests/e2e/scripts/test-bridge-fleet-token-rotation.sh" || MODULE_RC=1
fi
# Per-agent token identity (P7 Task 2): agent-class token acts as the agent
# (created_by = agent name, X-Actor ignored) and cannot mint bridge tokens.
# Self-SKIPs (exit 0) when the admin token is unreachable.
if should_run "per-agent-token-identity"; then
  run_module "per-agent-token-identity" "./gardenio/tests/e2e/scripts/test-per-agent-token-identity.sh" || MODULE_RC=1
fi
# Per-agent token SPAWN (P7 Task 2): spawns a real (spend-safe, never-starting)
# agent and asserts its pod and its reconciler-rebuilt pod mount agent-token-<bead>
# and never kd-beads-token, that the token acts as the agent, and that closing the
# agent revokes it. Self-SKIPs (exit 0) unless the controller runs PER_AGENT_TOKENS=true;
# set REQUIRE_PER_AGENT_TOKENS=1 to make the flag-off case a failure (rollout check).
if should_run "per-agent-token-spawn"; then
  run_module "per-agent-token-spawn" "./gardenio/tests/e2e/scripts/test-per-agent-token-spawn.sh" || MODULE_RC=1
fi
# Cross-owner MR→bead linking via privileged bridge fleet token (gd-XOcerNXFV2,
# epic gd-v9gFo1rRU1). Self-SKIPs (exit 0) until T1's privileged-fleet
# capability (ka-0000001946) is deployed to the namespace, so it is inert in
# the suite ahead of the fix. Force the assertions with CROSSOWNER_E2E=1.
if should_run "bridge-crossowner-mr-linking"; then
  run_module "bridge-crossowner-mr-linking" "./gardenio/tests/e2e/scripts/test-bridge-crossowner-mr-linking.sh" --namespace "$NAMESPACE" || MODULE_RC=1
fi
if should_run "stale-reaper"; then
  run_module "stale-reaper" "./gardenio/tests/e2e/scripts/test-stale-reaper.sh" || MODULE_RC=1
fi
# Stale-reaper grace-gate + run-instance reconcile (gd-lbGpeO0u1R, parent
# ka-0000000958): assert the two fixes that closed the "gardeners killed mid-loop /
# crew killed mid-wrap-up" incident — the grace-gated orphan reap (7a44b0b5 /
# ka-0000000018: busy pods with a fresh updated_at survive their task closing; only
# idle-past-grace orphans are reaped, crew 5m / gardener 15m) and stranded
# run-instance reconciliation (977d6f2b / ka-0000000889: a schedule/cron
# run-instance left in_progress by a gone assignee is CLOSED — immediately if
# reaped this pass, else past the 15m grace). test-stale-reaper.sh only asserts the
# reaper is enabled + logs a pass; this asserts both decision tables. The CONTRACT
# leg — the two reaper decision tables reimplemented over bash predicates — needs
# no substrate and runs in every shard (like thaw-reaper-exemption's contract leg,
# because the 5-15m grace windows exceed the shard budget and updated_at can't be
# backdated on the live daemon). The SUBSTRATE leg self-SKIPs unless
# REAPER_SUBSTRATE_E2E=1, so this is safe in every shard.
if should_run "stale-reaper-grace-reconcile"; then
  run_module "stale-reaper-grace-reconcile" "./gardenio/tests/e2e/scripts/test-stale-reaper-grace-reconcile.sh" || MODULE_RC=1
fi
# Thaw-race reaper exemptions (gd-2T9JI3wsw5, parent gd-JJIph7y3Ns): assert the
# reconciler reaper LEAVES ALONE an in-flight thaw placeholder Pod B (b0b44f25 /
# ka-0000002058) and a frozen-awaiting-thaw agent (bb1c0bba). The CONTRACT leg —
# the reaper exemption truth table reimplemented over bash predicates — needs no
# substrate and runs in every shard (like gvisor-rehydration's contract-shape
# leg). The SUBSTRATE leg inside the module self-SKIPs unless GVISOR_E2E=1 and
# the gvisor-poc freeze/thaw substrate is present, so this is safe in plain
# e2e shards (freeze/thaw disabled there → exemption checkers nil).
if should_run "thaw-reaper-exemption"; then
  run_module "thaw-reaper-exemption" "./gardenio/tests/e2e/scripts/test-thaw-reaper-exemption.sh" || MODULE_RC=1
fi
# Duplicate / concurrent spawn DEDUP during in-flight thaw (ka-0000007641, parent
# epic ka-0000004667). E2E acceptance for the re-nudge mid-THAW design's DEDUP
# decision (gardenio/docs/renudge-mid-thaw-coordination.md §DEDUP, decision child
# ka-0000005125; single-flight impl child ka-0000005830): a 2nd wake racing a
# thaw ⇒ exactly ONE runsc restore, ONE Pod B (the ka-0000007549 double-Pod-B
# class). Like thaw-reaper-exemption, the CONTRACT leg — the full DEDUP
# guard-chain decision table over bash predicates — needs no substrate and runs
# in every shard; the SUBSTRATE leg self-SKIPs unless GVISOR_E2E=1 and the
# gvisor-poc substrate is present, so this is safe in plain e2e shards.
if should_run "gvisor-duplicate-concurrent-spawn"; then
  run_module "gvisor-duplicate-concurrent-spawn" "./gardenio/tests/e2e/scripts/test-gvisor-duplicate-concurrent-spawn.sh" || MODULE_RC=1
fi
# Project-level runtime_class inheritance (ka-0000001145, parent E2E-gap sprint
# ka-0000000958). Coverage backfill for the merged capability (commit e0de71cb,
# task ka-0000006969, epic ka-0000007417): a type=project bead's `runtime_class`
# field pins EVERY agent in that project to that RuntimeClass (e.g. runsc) with
# precedence global<project<role<agent. Like gvisor-duplicate-concurrent-spawn,
# the CONTRACT leg — the runtime_class precedence + gVisor-routing decision table
# over bash predicates — needs no substrate and runs in every shard; the
# SUBSTRATE leg (a real project bead → runsc pod, asserted via kubectl) self-SKIPs
# unless GVISOR_E2E=1 and the gvisor-poc substrate + controller are present, so
# this is safe in plain e2e shards.
if should_run "project-runtime-class"; then
  run_module "project-runtime-class" "./gardenio/tests/e2e/scripts/test-project-runtime-class.sh" || MODULE_RC=1
fi
# gVisor runsc checkpoint/restore freeze/thaw state-fidelity (ka-0000001895,
# Phase 2 epic ka-0000002167). OPT-IN: the test self-SKIPs (exit 0) unless
# GVISOR_E2E=1 is set AND the gvisor-poc substrate is present (runsc
# RuntimeClass + create-pods in ns gvisor-poc). It is therefore inert in the
# standard e2e shards (which have no gVisor) and runs only via an
# explicit `GVISOR_E2E=1 ONLY=gvisor-freeze-thaw` invocation. Unlike the rest
# of the suite it targets ns gvisor-poc on the gvisor node pool, not the
# per-run e2e namespace.
if [ "${GVISOR_E2E:-0}" = "1" ] && should_run "gvisor-freeze-thaw"; then
  run_module "gvisor-freeze-thaw" "./gardenio/tests/e2e/scripts/test-gvisor-freeze-thaw.sh" || MODULE_RC=1
fi
# gVisor N-container sidecar freeze/thaw BARRIER (ka-0000004828, coverage-gap epic
# ka-0000000958). test-gvisor-freeze-thaw covers a SINGLE-container sandbox; the
# controller's real path is a COUNT-GATED N-container barrier (freezethaw.go:1957,
# freezethaw_node.go:2943) that was UNIT-only until this module. Builds a
# harness-owned MULTI-container runsc sandbox (sandbox root + agent + dind subs, one
# shared sentry — the sidecar-agent pod shape), checkpoints the whole pod, and drives
# the barrier restore on the live substrate: proves container_count==len(subs)+1 is
# baked into the statefile, the barrier HOLDS the guest frozen with N-1 subs restored
# and RELEASES only when the Nth satisfies the count, both subs' RAM+FILE state
# survive name-keyed, and an off-roster sub is refused. OPT-IN exactly like the
# sibling gvisor-freeze-thaw module: self-SKIPs (exit 0) unless GVISOR_E2E=1 AND the
# gvisor-poc substrate is present, so it is inert in the standard e2e shards
# and runs only via an explicit `GVISOR_E2E=1 ONLY=gvisor-sidecar-barrier` invocation.
if [ "${GVISOR_E2E:-0}" = "1" ] && should_run "gvisor-sidecar-barrier"; then
  run_module "gvisor-sidecar-barrier" "./gardenio/tests/e2e/scripts/test-gvisor-sidecar-barrier.sh" || MODULE_RC=1
fi
# gVisor multi-cycle freeze→thaw→re-freeze story (ka-0000001878, production-readiness
# epic ka-0000004667). Proves on the live substrate that a THAWED runsc process is
# itself re-checkpointable (substrate supports multi-cycle), that a node-local
# re-freeze to the controller's deterministic per-agent path DESTROYS the prior
# snapshot (the single-shot hazard the Consumed guard prevents), and that state
# fidelity holds across both cycles. The controller GUARD semantics (Snapshot.Consumed,
# ErrFreezeAlreadyThawed, predicate skipping → clean cold-start degradation) are
# asserted by the Go unit tests freezethaw_refreeze_guard_test.go; this is their
# live-substrate companion. OPT-IN exactly like the sibling gvisor-freeze-thaw
# module: self-SKIPs (exit 0) unless GVISOR_E2E=1 AND the gvisor-poc substrate is
# present, so it is inert in the standard e2e shards and runs only via an
# explicit `GVISOR_E2E=1 ONLY=gvisor-multicycle-refreeze` invocation.
if [ "${GVISOR_E2E:-0}" = "1" ] && should_run "gvisor-multicycle-refreeze"; then
  run_module "gvisor-multicycle-refreeze" "./gardenio/tests/e2e/scripts/test-gvisor-multicycle-refreeze.sh" || MODULE_RC=1
fi
# gVisor boot-wedge armed-watcher liveness guard / freeze fail-safe (ka-0000000584,
# Phase-4 gVisor freeze/thaw). Asserts the END-TO-END fail-safe path the P0 fix
# (ka-0000006667, controller watchersArmed + freezethaw_armed_guard_test.go)
# guarantees: an un-armed sandbox the guard refuses to freeze is LEFT RUNNING
# (the boot-wedge is never minted), and the both-armed positive control proceeds
# to a real checkpoint that persists a snapshot. OPT-IN exactly like the sibling
# gvisor-freeze-thaw module: self-SKIPs (exit 0) unless GVISOR_E2E=1 AND the
# gvisor-poc substrate is present (runsc RuntimeClass + create-pods in ns
# gvisor-poc), so it is inert in the standard e2e shards and runs only
# via an explicit `GVISOR_E2E=1 ONLY=gvisor-armed-guard` invocation.
if [ "${GVISOR_E2E:-0}" = "1" ] && should_run "gvisor-armed-guard"; then
  run_module "gvisor-armed-guard" "./gardenio/tests/e2e/scripts/test-gvisor-armed-guard.sh" || MODULE_RC=1
fi
# gVisor rehydration C1–C5 correctness contract (ka-0000005803, Phase 3 epic
# ka-0000006959; §6 of rehydration-design ka-0000001515). UNLIKE freeze-thaw this
# module is NOT fully gated on GVISOR_E2E: its contract-shape assertions (the
# cred re-mint freshness gate over crafted Pod-A/Pod-B JWTs, and the thaw-epoch
# marker/ack release condition) need only bash + a base64 codec, so they run in
# every shard. The runsc-lifecycle + CloudTrail assertions inside it self-SKIP
# unless GVISOR_E2E=1 and the gvisor-poc substrate is present. The connection
# half of the contract (C3 NATS reconnect, C4 re-announce, idempotency, deadline
# against a live broker) is asserted by coop/tests/specs/tests/rehydration.rs.
if should_run "gvisor-rehydration"; then
  run_module "gvisor-rehydration" "./gardenio/tests/e2e/scripts/test-gvisor-rehydration.sh" || MODULE_RC=1
fi
# <1s gVisor agent resume — STANDING TIMED regression gate (ka-0000006324 / C7,
# Phase 5 epic ka-0000002590). Operator mandate (Matt Baker, thread
# 1780804147.560279): the <1s resume result must be guarded by a permanent timed
# e2e test, not only proven once by the overnight benchmark (ka-0000003577).
# OPT-IN like the sibling gvisor modules: gated on GVISOR_E2E=1 (or
# RESUME_LATENCY_E2E=1) so it is inert in the standard e2e shards (no
# gVisor node pool there) and runs only via an explicit
# `GVISOR_E2E=1 ONLY=resume-latency`. It targets
# ns gvisor-poc on the gvisor node pool. The module measures wall-clock resume
# latency over N≥5 warm-B (R4) resumes, asserts p50<1s AND strictly faster than
# the fresh-B (R3) control, prints resume_latency_ms per run + a Prometheus
# histogram textfile, and (per scope item 3) SKIPs LOUD — never silently passes —
# when the opt-in flag is set but the substrate is absent.
if [ "${GVISOR_E2E:-0}" = "1" ] || [ "${RESUME_LATENCY_E2E:-0}" = "1" ]; then
  if should_run "resume-latency"; then
    run_module "resume-latency" "./gardenio/tests/e2e/scripts/test-resume-latency.sh" || MODULE_RC=1
  fi
fi
# <20s USER-VISIBLE first-response — STANDING TIMED regression gate (ka-0000000107 /
# C4, epic ka-0000008235 "Fast + interruptible freeze"). Operator: Matt Baker.
# The user-visible companion to the C7 resume-latency gate: where C7 proves the
# warm thaw is sub-second in isolation, C4 proves the path the USER experiences —
# spawn-or-thaw-or-serve-through-an-interrupt — stays under the 20s SLO for ALL
# THREE lifecycle states (COLD spawn/thaw, RESUMING warm thaw, MID-FREEZE serve
# the still-live agent via the C2 interrupt, no thaw). OPT-IN like the sibling
# gvisor modules: gated on GVISOR_E2E=1 (or USERRESPONSE_LATENCY_E2E=1) so it is
# inert in the standard e2e shards (no gVisor node pool there) and runs
# only via an explicit `GVISOR_E2E=1 ONLY=userresponse-latency`. It targets ns gvisor-poc on the
# gvisor node pool, reuses the proven node scripts the C7 gate proves, prints
# userresponse_latency_ms per sample + a gardenio_userresponse_latency_seconds
# histogram textfile, and SKIPs LOUD — never silently passes — when the opt-in
# flag is set but the substrate is absent. The MID-FREEZE leg depends on the C2
# interrupt path (ka-0000005834) — it asserts the leave-running invariant
# (freezethaw.go L540) the interrupt relies on.
if [ "${GVISOR_E2E:-0}" = "1" ] || [ "${USERRESPONSE_LATENCY_E2E:-0}" = "1" ]; then
  if should_run "userresponse-latency"; then
    run_module "userresponse-latency" "./gardenio/tests/e2e/scripts/test-userresponse-latency.sh" || MODULE_RC=1
  fi
fi
# gVisor thaw rehydrate-ack RELEASE on the real substrate (ka-0000006321; blocks
# ka-0000007916). Closes the gap the 4 open P1 rehydrate-ack bugs share: NO e2e
# drove a REAL frozen→thawed agent on the gvisor-poc substrate and asserted the
# REAL controller gate (thawepoch.WaitForAcks) RELEASES the agent — both required
# consumers (coop ConsumerCoop + gb ConsumerGB) ack the current epoch — WITHIN
# deadline, with Pod B alive (the ka-0000007916 symptom is a fail-close while Pod
# B stays alive the full window). Unlike the bash-re-implemented gate in
# test-gvisor-rehydration.sh, this module builds + runs cmd/thawepoch-probe — the
# genuine thawepoch.Watcher feeding the genuine WaitForAcks — so a regression in
# either fails this e2e. OPT-IN like the sibling gvisor modules: gated on
# GVISOR_E2E=1 (or REHYDRATE_ACK_E2E=1) so it is inert in the standard
# e2e shards (no gVisor node pool there) and runs only via an explicit
# `GVISOR_E2E=1 ONLY=gvisor-rehydrate-ack`. It targets ns gvisor-poc on the
# gvisor node pool and SKIPs LOUD — never silently passes — when the opt-in flag
# is set but the substrate is absent.
if [ "${GVISOR_E2E:-0}" = "1" ] || [ "${REHYDRATE_ACK_E2E:-0}" = "1" ]; then
  if should_run "gvisor-rehydrate-ack"; then
    run_module "gvisor-rehydrate-ack" "./gardenio/tests/e2e/scripts/test-gvisor-rehydrate-ack.sh" || MODULE_RC=1
  fi
fi
# EPIC ACCEPTANCE GATE: freeze → resume-INTO-POOL (ka-0000003295, epic
# ka-0000007532). The one end-to-end proof the resume-into-pool epic converges on:
# freeze an active agent, resume its snapshot INTO a CLAIMED prewarmed pool member
# (not a fresh Pod B), and assert /work + RAM + gb/coop rehydrate-ack RELEASE all
# hold on that one resumed agent — for BOTH a claudeless and an active-Claude
# variant. Sits above its three building-block siblings (freeze-thaw state
# fidelity, resume-latency warm-B restore, rehydrate-ack RELEASE), asserting the
# converged path none of them cover. OPT-IN like the sibling gvisor modules: gated
# on GVISOR_E2E=1 (or RESUME_INTO_POOL_E2E=1) so it is inert in the standard
# e2e shards (no gVisor node pool there) and runs only via an explicit
# `GVISOR_E2E=1 ONLY=gvisor-resume-into-pool`. Targets ns gvisor-poc; SKIPs LOUD —
# never silently passes — when the opt-in flag is set but the substrate is absent.
if [ "${GVISOR_E2E:-0}" = "1" ] || [ "${RESUME_INTO_POOL_E2E:-0}" = "1" ]; then
  if should_run "gvisor-resume-into-pool"; then
    run_module "gvisor-resume-into-pool" "./gardenio/tests/e2e/scripts/test-gvisor-resume-into-pool.sh" || MODULE_RC=1
  fi
fi
# gb salvage dirty-worktree → S3 → restore (ka-0000000188, parent ka-0000000958):
# the rescue that bundles an involuntarily-killed agent's uncommitted work to S3
# (MR !5359 / ka-0000004335, Tier-2 push !5613) had ZERO e2e coverage of the real
# round-trip — a data-integrity gap that fails SILENTLY, because salvage runs on
# the teardown path where it always exits 0 and production discards its stderr.
# This module runs the REAL `gb salvage` and `gb salvage restore` binaries against
# a local stdlib-python3 S3 stub (salvage honours COOP_S3_ENDPOINT and forces
# path-style, the same override the in-box MinIO overlay uses), asserting a
# tracked edit, an untracked file, and a committed-but-unpushed commit
# (ka-0000005216) all survive kill → S3 → restore into a fresh clone, while a
# .gitignore'd secret does NOT. Fully HERMETIC — git + python3 + gb only, no
# kubectl/namespace/daemon/AWS creds/network — so it runs unconditionally in
# every shard and self-SKIPs loud if a prerequisite binary is missing.
if should_run "gb-salvage"; then
  run_module "gb-salvage" "./gardenio/tests/e2e/scripts/test-gb-salvage.sh" || MODULE_RC=1
fi
# AUTOMATIC salvage auto-restore on respawn (gd-WDgPrzYp2N, parent ka-0000000958):
# the SIBLING of gb-salvage above and its DISJOINT complement. gb-salvage covers
# the MANUAL round-trip (`gb salvage restore` run by hand); this covers the
# AUTOMATIC hook at init_agent_loop.go:107 (autoRestoreSalvageOnRespawn) — the
# leg that actually runs in a production respawn, fetching the salvage bundle
# back into the re-cloned repo and writing salvage-restore-note.md into the pod's
# system prompt so the resumed agent recovers its lost WIP WITHOUT any operator
# action. That path had ZERO e2e coverage (grep gardenio/tests/ for
# "auto-restore"/"salvage-restore-note" = 0). It drives the REAL `gb init --mock`
# binary — the exact call site — against the SAME stdlib-python3 S3 stub, fully
# HERMETIC (git + python3 + gb only; env -i + sandboxed HOME + dead daemon/NATS +
# COOP_MAX_RESTARTS=0 so the hook fires but coop never launches and no model
# spend occurs). Runs unconditionally in every shard and self-SKIPs loud if a
# prerequisite binary is missing.
if should_run "gb-salvage-autorestore"; then
  run_module "gb-salvage-autorestore" "./gardenio/tests/e2e/scripts/test-gb-salvage-autorestore.sh" || MODULE_RC=1
fi
# gb whoami capability introspection (ka-0000001750, parent ka-0000000958): the
# read-only "what can I actually do?" self-diagnostic (AWS/K8s/GitLab/secrets
# probing) merged by ka-0000003630 shipped with 998 lines of net-new runtime
# code and UNIT tests only — `grep -rI whoami gardenio/tests/` was ZERO before
# this. A regression is high-blast-radius and quiet: it is the agent-facing input
# to the escalate/do-not decision (advice ka-0000000752) and load-bearing for the
# boot-time capability block, yet every probe section is fail-open so the command
# never exits non-zero on a broken probe. This module runs the REAL `gb whoami`
# binary with the four external tools (aws/kubectl/glab/git) shadowed on PATH by
# deterministic fixture stubs, asserting the JSON + human report carry the known
# identities, the K8s auth path is probed INDEPENDENTLY of AWS (the ka-0000003844
# miss), derivations (30→Developer, secret-prefix aggregation) are correct, each
# section fails open in isolation, --probe verifies assume-role targets WITHOUT
# minting a real STS credential (task constraint), and a second identity fixture
# yields a different report (not hardcoded). Fully HERMETIC — gb + python3 only,
# no kubectl/namespace/daemon/AWS creds/network — so it runs unconditionally in
# every shard and self-SKIPs loud if a prerequisite is missing.
if should_run "gb-whoami"; then
  run_module "gb-whoami" "./gardenio/tests/e2e/scripts/test-gb-whoami.sh" || MODULE_RC=1
fi
# gb spawn SPAWNED success-token contract (ka-0000004253, parent ka-0000000958):
# the machine-greppable first line `SPAWNED agent=… bead=… status=… task=…` that
# a gardener loop scripts against to confirm a dispatch (SHA 283942476,
# ka-0000000982) shipped with UNIT tests only — no e2e drove the REAL `gb spawn`
# binary's stdout. The token exists to STOP duplicate-spawn storms: its absence
# once made a gardener grepping '^Spawned' read a SUCCESSFUL spawn as failure and
# re-dispatch three times (bug ka-0000001243), so a silent regression to prose
# (or the token drifting off line 1) re-opens that fleet-wide failure mode. This
# module points the real binary at a tiny STATEFUL localhost mock kbeads daemon
# and asserts: the token anchors at stdout LINE 1 with parseable agent=/bead=/
# status=/task= (exit 0); status is a MEMBER of {created,existing}; a re-spawn of
# the same task is the single-claim no-op → status=existing returning the exact
# bead the first spawn created (dedup is spawn.go's real guard, not the mock);
# and the NEGATIVE CONTROL — a failing spawn prints NO `^SPAWNED` line and exits
# non-zero, proving the grep discriminates. Fully HERMETIC — gb + python3 only,
# no kubectl/namespace/real daemon/network — so it runs unconditionally in every
# shard and self-SKIPs loud (pre-2026.218 gb, or a missing prerequisite).
if should_run "gb-spawn-success-token"; then
  run_module "gb-spawn-success-token" "./gardenio/tests/e2e/scripts/test-gb-spawn-success-token.sh" || MODULE_RC=1
fi
# gb audit fetch bare-bead expansion + empty-fetch guard (ka-0000004272, parent
# ka-0000000958): `gb audit` — the forensic session-log download an operator runs
# to pull an agent's archived transcripts out of S3 — had ZERO e2e coverage of
# any kind (0 of ~190 test-*.sh touched it). Two behaviors merged ab1d5b6a2
# shipped unguarded at the CLI level: (1) `gb audit fetch <bare-bead-id>` expands
# to EVERY session under the bead newest-first with a `resolved to N session(s)`
# note, and (2) a fetch that downloads NOTHING now EXITS NON-ZERO
# (audit.go:433, ka-0000002531) instead of exit-0-with-empty-dir. The pure
# resolver has a Go unit test but the RunE (expansion loop, note line, --json
# `sessions` key, exit-code guard) is untested — the half where a regression is
# invisible, since gb audit is human-driven with no alerting. This module drives
# the REAL `gb audit fetch` against a local stdlib-python3 S3 stub (audit honours
# COOP_S3_ENDPOINT + forces path-style, same override the in-box MinIO overlay
# uses), asserting bare-bead expansion (newest-first), compound-id passthrough,
# and — the headline — that an empty fetch exits non-zero and writes nothing.
# Fully HERMETIC — gb + python3 only, no kubectl/namespace/daemon/AWS creds/
# network — so it runs unconditionally in every shard and self-SKIPs loud if a
# prerequisite binary is missing.
if should_run "gb-audit-fetch"; then
  run_module "gb-audit-fetch" "./gardenio/tests/e2e/scripts/test-gb-audit-fetch.sh" || MODULE_RC=1
fi
# gb stop → SUSPEND (freeze) on a freeze-eligible runsc agent (ka-0000000910;
# fast-hot-resume epic ka-0000003080; follow-up from decision ka-0000003208 on
# the merged gb-stop-suspend change ka-0000005754 / MR !4327). UNLIKE the direct-
# runsc siblings, this drives the REAL chain MR !4327 introduced: plain `gb stop`
# inside a freeze-eligible runsc agent → agent_state=stopping (the sole AgentStop
# trigger, NOT done, NOT stop_requested) → controller `runsc checkpoint
# --leave-running` → persisted snapshot + a gardenio_freeze_latency_seconds sample;
# negative control: `gb stop --done` takes the unchanged terminal done path.
# OPT-IN like the sibling gvisor modules: gated on GVISOR_E2E=1 (or
# GB_STOP_SUSPEND_E2E=1) so it is inert in the standard e2e shards (no
# gVisor + no freeze/thaw controller there) and runs only via an explicit
# `GVISOR_E2E=1 ONLY=gb-stop-suspend`. It SKIPs LOUD — never silently passes —
# when the opt-in flag is set but the substrate/controller is absent.
if [ "${GVISOR_E2E:-0}" = "1" ] || [ "${GB_STOP_SUSPEND_E2E:-0}" = "1" ]; then
  if should_run "gb-stop-suspend"; then
    run_module "gb-stop-suspend" "./gardenio/tests/e2e/scripts/test-gb-stop-suspend.sh" || MODULE_RC=1
  fi
fi
# probe-execution (ka-0000002117, epic ka-0000000958): `gb probe run`
# (execution_target=probe) PRODUCER + the `script` condition leaf CONSUMER. Seeds
# a type=script bead, runs it as a one-off probe pod (container named `probe`, no
# coop/Claude → zero model spend by construction), asserts the verdict stamping
# (exit 0 → done/exit_code=0; exit N → failed/real exit code — the negative
# control), then feeds BOTH verdicts to a `script` leaf via `gb trigger reconcile`
# and asserts would_fire_now (exit-0 ⇒ suppress, exit-N ⇒ wake). OPT-IN like the
# other real-pod-spawn modules: gated on PROBE_EXECUTION_E2E=1 (or GVISOR_E2E=1)
# so it is inert in the standard shards (which spawn NO agent pods) and SKIPs LOUD
# when the flag is set but kubectl / cluster / agent image is missing. Run via a
# dedicated dispatch or `PROBE_EXECUTION_E2E=1 ONLY=probe-execution`.
if [ "${PROBE_EXECUTION_E2E:-0}" = "1" ] || [ "${GVISOR_E2E:-0}" = "1" ]; then
  if should_run "probe-execution"; then
    run_module "probe-execution" "./gardenio/tests/e2e/scripts/test-probe-execution.sh" || MODULE_RC=1
  fi
fi
# Controller RESTART mid-freeze / mid-thaw durability (ka-0000002295, production-
# readiness epic ka-0000004667). The CONTROLLER-PROCESS durability concern, distinct
# from the node-runsc siblings. Builds + runs cmd/ftrestart-probe — the GENUINE
# ConfigMapSnapshotStore (ka-0000001694) + FreezeThawManager DrainThaws/InflightThaw/
# WithoutCancel-detached dctx (main.go:909, ka-0000001355 / ka-0000004088). Case (a)
# drives a REAL process boundary against the LIVE API server (proc1 persists a
# complete snapshot then exits; a FRESH proc2 recovers + trusts it, rejects a half
# one); case (b) parks a real Thaw mid-restore at an inbound-ctx cancel and asserts
# the slot survives + the detached ctx is NOT cancelled + DrainThaws drains in budget.
# OPT-IN like the sibling gvisor modules: gated on GVISOR_E2E=1 (or
# CONTROLLER_RESTART_E2E=1) so it is inert in the standard e2e shards (no
# gVisor + no configmap-RBAC there) and runs only via an explicit
# `GVISOR_E2E=1 ONLY=gvisor-controller-restart`. NO privileged node pod — durability

# is a controller↔API-server property. SKIPs LOUD — never silently passes — when the
# opt-in flag is set but the substrate/RBAC is absent.
if [ "${GVISOR_E2E:-0}" = "1" ] || [ "${CONTROLLER_RESTART_E2E:-0}" = "1" ]; then
  if should_run "gvisor-controller-restart"; then
    run_module "gvisor-controller-restart" "./gardenio/tests/e2e/scripts/test-gvisor-controller-restart.sh" || MODULE_RC=1
  fi
fi

# ── Generate JUnit XML (per-shard) ──────────────────────────────────
python3 - "$RESULTS_JSONL" "$JUNIT_DIR/e2e-summary.xml" <<'PYEOF'
import json, sys
from xml.sax.saxutils import escape

results_file, output_file = sys.argv[1], sys.argv[2]
results = []
with open(results_file) as f:
    for line in f:
        line = line.strip()
        if line:
            results.append(json.loads(line))

tests = sum(1 for r in results if r["status"] != "skip")
failures = sum(1 for r in results if r["status"] == "fail")
skipped = sum(1 for r in results if r["status"] == "skip")
total_time = sum(r.get("duration_s", 0) for r in results)

lines = ['<?xml version="1.0" encoding="UTF-8"?>']
lines.append(f'<testsuites tests="{tests}" failures="{failures}" skipped="{skipped}" time="{total_time}">')
lines.append(f'  <testsuite name="gardenio-e2e-shard" tests="{tests}" failures="{failures}" skipped="{skipped}" time="{total_time}">')
for r in results:
    name = escape(r["name"])
    dur = r.get("duration_s", 0)
    if r["status"] == "skip":
        lines.append(f'    <testcase name="{name}" classname="gardenio.e2e" time="{dur}"><skipped/></testcase>')
    elif r["status"] == "fail":
        rc = r.get("rc", 1)
        lines.append(f'    <testcase name="{name}" classname="gardenio.e2e" time="{dur}">')
        lines.append(f'      <failure message="exit code {rc}">Module {name} failed with exit code {rc}</failure>')
        lines.append(f'    </testcase>')
    else:
        lines.append(f'    <testcase name="{name}" classname="gardenio.e2e" time="{dur}" />')
lines.append('  </testsuite>')
lines.append('</testsuites>')

with open(output_file, 'w') as f:
    f.write('\n'.join(lines) + '\n')
print(f"JUnit XML written (shard): {tests} tests, {failures} failures, {skipped} skipped")
PYEOF

# ── Staging pod log dump on shard failure ────────────────────────────
if [ "${HERMETIC_ONLY:-}" != "1" ] && [ "$TESTS_FAILED" -gt 0 ]; then
  echo ""
  echo "════════════════════════════════════════════════════"
  echo " STAGING POD LOGS (last 40 lines per pod, shard $E2E_SHARD)"
  echo " namespace=$NAMESPACE"
  echo "════════════════════════════════════════════════════"
  for LABEL in \
    "app.kubernetes.io/component=agents" \
    "app.kubernetes.io/component=slack-bridge" \
    "app.kubernetes.io/component=beads"; do
    PODS=$(kubectl -n "$NAMESPACE" get pod -l "$LABEL" -o name 2>/dev/null || true)
    for POD in $PODS; do
      echo ""
      echo "──── $POD ($LABEL) ────"
      kubectl -n "$NAMESPACE" logs "$POD" --tail=40 --all-containers=true 2>&1 | sed 's/^/    /' || echo "    (log fetch failed)"
    done
  done
  echo ""
  echo "════════════════════════════════════════════════════"
  echo " END STAGING POD LOGS (shard $E2E_SHARD)"
  echo "════════════════════════════════════════════════════"
fi

echo ""
echo "════════════════════════════════════════════════════"
echo " E2E Shard $E2E_SHARD Summary: $TESTS_PASSED passed, $TESTS_FAILED failed, $TESTS_SKIPPED skipped"
echo "════════════════════════════════════════════════════"

# ── Cleanup port-forwards ───────────────────────────────────────────
# Remove the supervisor sentinels so pf-supervisor.sh kills its inner kubectl
# and exits cleanly; the pid-file kills cover the unsupervised fallback path.
# (The EXIT trap also does this, but doing it explicitly here lets the tunnels
# tear down before the JUnit/log work above has fully flushed.) ka-0000001942.
rm -f "$PF_SENTINEL" "$BRIDGE_PF_SENTINEL" 2>/dev/null || true
if [ -f /tmp/pf-pid ]; then
  kill "$(cat /tmp/pf-pid)" 2>/dev/null || true
fi
if [ -f /tmp/bridge-pf-pid ]; then
  kill "$(cat /tmp/bridge-pf-pid)" 2>/dev/null || true
fi

exit "$MODULE_RC"
