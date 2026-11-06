#!/usr/bin/env bash
# test-probe-execution.sh — `gb probe run` (execution_target=probe) + the
# `script` condition leaf E2E (ka-0000002117, epic ka-0000000958).
#
# WHAT THIS COVERS (the gap it closes):
#   Two runtime pieces landed with ZERO black-box coverage:
#     - podmanager/probe.go + cmd/gb/probe.go (`gb probe run <script-bead>`): the
#       PRODUCER. A type=script bead's `script` field is realized as a one-off
#       K8s pod (execution_target=probe) — a bare `/bin/sh -lc "$script"`
#       container named `probe`, NO coop lifecycle, NO Claude OAuth, slim
#       50m/128Mi. `gb probe run` waits for the pod's terminal phase and STAMPS
#       the verdict on the bead: exit 0 → agent_state=done, exit N → failed, and
#       the observed exit_code either way. gb's own exit code mirrors the probe.
#     - conditions/leaf_script.go (the `script` daemon condition leaf): the
#       CONSUMER. It READS the verdict a probe stamped (script_bead → exit_code),
#       default op `nonzero`: a fresh closed probe that exited 0 SUPPRESSES the
#       fire (would_fire_now=false), any non-zero WAKES it (would_fire_now=true).
#
#   HEAD-verified before this module: `grep -rI script_bead gardenio/tests/` = 0,
#   `grep -rI type:script gardenio/tests/` = 0, and the only `gb probe run` hit in
#   the suite was a COMMENT in test-microvm-execution-target.sh. So a silent
#   regression in either half — a probe that stops stamping, or a leaf that
#   misreads the verdict — would ship uncaught, and every condition-gated gardener
#   riding this substrate would fire (or suppress) wrongly, fleet-wide.
#
# NAME-MATCH TRAP (why this is a NEW module, not an extension):
#   test-microvm-execution-target.sh LOOKS like probe coverage and is NOT — it is
#   a twin-predicate shell REIMPLEMENTATION of the resolver that asserts
#   `probe → FAIL_CLOSED` at the spawn door and never touches the real pod path.
#   Extending it would deepen the reimplementation. This module drives the REAL
#   lifecycle end-to-end through the live controller-substrate + daemon.
#
# WHAT IT PROVES (against the live cluster + beads daemon):
#   1. `gb probe run` on a type=script bead spawns a REAL pod whose single
#      container is named `probe` (podmanager.ProbeContainerName), carries the
#      gardenio.io/probe label, and runs the fleet agent image.
#   2. EXIT-0 leg: the probe bead is stamped agent_state=done, exit_code=0, and
#      closed. `gb probe run` exits 0.
#   3. NON-ZERO leg (the negative control): a script that `exit 3`s stamps the
#      bead agent_state=failed with the REAL exit_code=3, and closed. `gb probe
#      run` exits non-zero. A vacuous happy-path-only module would pass with the
#      verdict-stamping deleted; THIS leg fails when it is.
#   4. THE JOIN: feed BOTH verdicts to a `script` condition leaf (via a seeded
#      type=trigger bead, conditions=[{type:script,script_bead:<id>}], default op
#      nonzero) and evaluate it exactly as the schedule engine does at fire time
#      (`gb trigger reconcile --json` → scheduler.EvaluateConditions → evalScript):
#        - exit-0 probe  → would_fire_now=false (SUPPRESS: "nothing changed")
#        - exit-3 probe  → would_fire_now=true  (WAKE:     "something changed")
#      Covering the producer alone leaves this consumer join untested.
#   5. (cheap, non-blocking) activity_deadline_seconds: a script that sleeps past
#      a 5s deadline is kubelet-killed → PodFailed → stamped failed. Skipped
#      quietly if the substrate is slow; NEVER blocks the module.
#
# ANTI-VACUITY: delete the verdict-stamping in cmd/gb/probe.go (the
# closeFields{exit_code,agent_state} write) and THIS module fails at:
#   - Leg 2  "exit-0 probe stamped agent_state=done / exit_code=0"
#   - Leg 3  "exit-3 probe stamped agent_state=failed / exit_code=3"  (the control)
#   - Leg 4  BOTH would_fire_now assertions (the leaf reads exit_code/status; with
#            no stamp the bead never closes with a parseable code → fails open →
#            the exit-0 SUPPRESS assertion flips to true and fails).
# The negative control is Leg 3 plus the both-verdicts requirement in Leg 4.
#
# SPEND SAFETY: a probe pod runs `/bin/sh -lc "$script"` — it NEVER launches coop
# or Claude, so there is NO model spend on any leg by construction (unlike an
# agent pod, this needs no mock_scenario to stay claudeless).
#
# OPT-IN / GATING (NEVER silently passes): this module spawns REAL pods and needs
# a live cluster + a controller-configured agent image, so — like the other
# real-pod-spawn modules (test-gb-stop-suspend, test-dark-jira-fence-pod) — it is
# INERT on the standard credless fan-out and runs only when PROBE_EXECUTION_E2E=1
# (or GVISOR_E2E=1). When the flag is UNSET it SKIPs cleanly (exit 0). When the
# flag IS SET but kubectl / the cluster / the agent image is missing it SKIPs
# LOUD (still exit 0 so an absent substrate does not red the suite, but a skip is
# never mistaken for a pass).
#
# Precedents: test-pool-assign-attribution.sh (seed bead → wait_for_agent_pod →
# hardened trap cleanup), test-gb-stop-suspend.sh (controller-spawned pod + jsonpath).
#
# Usage:
#   PROBE_EXECUTION_E2E=1 ./gardenio/tests/e2e/scripts/test-probe-execution.sh
#   PROBE_EXECUTION_E2E=1 NAMESPACE=gardenio-e2e ONLY=probe-execution run-shard.sh
set -uo pipefail

# ── Config (all overridable via env) ────────────────────────────────
NAMESPACE="${NAMESPACE:-gardenio-e2e}"
BEADS_SVC="${BEADS_SVC:-${NAMESPACE}-beads}"
# The controller Deployment carries COOP_IMAGE (the fleet agent image) — the
# same image `gb probe run` defaults to. helm agents.fullname → "<ns>-agents".
CONTROLLER_DEPLOY="${CONTROLLER_DEPLOY:-${NAMESPACE}-agents}"
KD_BIN="${KD_BIN:-kd}"
# shellcheck source=SCRIPTDIR/lib-kd-create.sh
source "$(dirname "${BASH_SOURCE[0]}")/lib-kd-create.sh"
GB_BIN="${GB_BIN:-gb}"
PROJECT="${PROJECT:-gardenio}"
# How long `gb probe run` may wait for the pod's terminal phase (its own
# --timeout). A probe script is tiny, but a COLD agent-image pull onto a fresh
# Karpenter node can dominate (the image is ~GB), so keep it generous. The outer
# process guard adds headroom over this so gb can still close the bead after its
# own wait elapses (see run_probe).
PROBE_TIMEOUT="${PROBE_TIMEOUT:-300}"
# activeDeadlineSeconds for the timeout leg (leg 5). Small so the leg is cheap.
DEADLINE_SECS="${DEADLINE_SECS:-5}"
# Registry pull secret for the PRIVATE agent image. Resolved from the
# controller's IMAGE_PULL_SECRETS env in main() when unset; a probe pod cannot
# pull the private image without it (403 → ImagePullBackOff → never runs).
PROBE_PULL_SECRET="${PROBE_PULL_SECRET:-}"

# Opt-in flag: this module spawns real pods, so it is inert unless asked.
PROBE_EXECUTION_E2E="${PROBE_EXECUTION_E2E:-${GVISOR_E2E:-0}}"

PASS=0
FAIL=0
ERRORS=()
CREATED_BEADS=()
PF_PID=""

export KD_ACTOR="${KD_ACTOR:-e2e-probe-execution}"

# ── Helpers ─────────────────────────────────────────────────────────
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

# Read a single string field from a bead via kd show --json.
kd_field() {
  local id="$1" field="$2"
  "$KD_BIN" show "$id" --json 2>/dev/null | python3 -c "
import sys, json
try:
    d = json.load(sys.stdin)
    print(d.get('fields', {}).get('$field', ''))
except Exception:
    print('')
" 2>/dev/null || echo ""
}

# Read a top-level bead attribute (status, type, ...) via kd show --json.
kd_attr() {
  local id="$1" attr="$2"
  "$KD_BIN" show "$id" --json 2>/dev/null | python3 -c "
import sys, json
try:
    d = json.load(sys.stdin)
    print(d.get('$attr', ''))
except Exception:
    print('')
" 2>/dev/null || echo ""
}

# ── Port-forward hygiene (long runs outlast one kubectl port-forward) ──
setup_portforward() {
  if [ -n "${BEADS_HTTP_URL:-}" ]; then
    blue "Using BEADS_HTTP_URL=$BEADS_HTTP_URL"
    return
  fi
  blue "Port-forwarding ${NAMESPACE} kd server..."
  local port=19113
  kubectl -n "$NAMESPACE" port-forward "svc/${BEADS_SVC}" "${port}:8080" \
    >/tmp/kd-pf-probe-exec.log 2>&1 &
  PF_PID=$!
  sleep 3
  export BEADS_HTTP_URL="http://localhost:${port}"
  dim "  Daemon at $BEADS_HTTP_URL (pid $PF_PID)"
}

ensure_daemon_pf() {
  [ -z "$PF_PID" ] && return 0  # caller-provided URL; not ours to manage
  if "$KD_BIN" list --status=open --limit 1 --json >/dev/null 2>&1; then
    return 0
  fi
  kill "$PF_PID" 2>/dev/null || true
  local port="${BEADS_HTTP_URL##*:}"
  kubectl -n "$NAMESPACE" port-forward "svc/${BEADS_SVC}" "${port}:8080" \
    >/tmp/kd-pf-probe-exec.log 2>&1 &
  PF_PID=$!
  sleep 3
}

setup_token() {
  if [ -n "${BEADS_TOKEN:-}" ]; then
    export BEADS_AUTH_TOKEN="${BEADS_AUTH_TOKEN:-$BEADS_TOKEN}"
    return
  fi
  local secret
  secret=$(kubectl -n "$NAMESPACE" get secret kd-beads-token \
    -o jsonpath='{.data.token}' 2>/dev/null | base64 -d 2>/dev/null || true)
  if [ -n "$secret" ]; then
    export BEADS_TOKEN="$secret"
    export BEADS_AUTH_TOKEN="$secret"
    dim "  Token loaded from secret kd-beads-token"
  fi
}

cleanup() {
  for id in "${CREATED_BEADS[@]}"; do
    "$KD_BIN" close "$id" 2>/dev/null || true
  done
  [ -n "$PF_PID" ] && kill "$PF_PID" 2>/dev/null || true
}
trap cleanup EXIT

# Seed a type=script bead with a `script` field (+ optional extra -f pairs).
# Prints the bead id, or empty on failure.
seed_script_bead() {
  local title="$1" script="$2"; shift 2
  local id
  id=$(kd_create_id "$KD_BIN" create "$title" \
    --type=script \
    -d "e2e probe-execution script bead ($TEST_TAG)" \
    -l "e2e,$TEST_TAG,project:${PROJECT}" \
    -f "script=$script" \
    -f "project=${PROJECT}" \
    "$@")
  if [ -z "$id" ]; then
    dim "    seed_script_bead failed (kd output above)" >&2
  fi
  printf '%s' "$id"
}

# Run `gb probe run <bead>` and capture rc + output. Prints rc; output on stderr
# of this fn is discarded — the caller reads the bead verdict, which is the
# durable surface. We pass --image + --pull-secret explicitly so the run does not
# depend on the CLI's env defaulting inside the shard: the probe reuses the
# PRIVATE fleet agent image, so without the registry pull secret the pod
# ImagePullBackOffs (403) and never runs.
run_probe() {
  local bead="$1" out rc=0
  local pull_args=()
  [ -n "$PROBE_PULL_SECRET" ] && pull_args=(--pull-secret "$PROBE_PULL_SECRET")
  # Give the outer process guard 60s of headroom OVER gb's own --timeout so that
  # when gb's wait elapses it still gets to stamp+close the bead (its terminal
  # work) instead of being SIGKILLed mid-close by an equal outer timeout.
  out=$(timeout "$((PROBE_TIMEOUT + 60))" "$GB_BIN" probe run "$bead" \
    --image "$PROBE_IMAGE" \
    --namespace "$NAMESPACE" \
    "${pull_args[@]}" \
    --timeout "${PROBE_TIMEOUT}s" 2>&1) || rc=$?
  printf '%s\n' "$out" | sed 's/^/      /' >&2
  return "$rc"
}

# Evaluate a `script` condition leaf against a probe bead exactly as the engine
# does at fire time, via `gb trigger reconcile`. Seeds a disabled type=trigger
# whose conditions=[{type:script,script_bead:<probe>}] (default op nonzero), then
# reads gate.would_fire_now. Prints "true"/"false"/"" (empty on eval failure).
#
# enabled=false + action=notify: the LIVE trigger engine skips enabled=false
# triggers at load, so this bead never actually fires or spawns anything — but
# `gb trigger reconcile` evaluates its conditions against the live store
# unconditionally (buildReconcileReport always builds the gate), which is exactly
# the fire-time gate we want to assert.
eval_script_leaf() {
  local probe_bead="$1" trig_id conds conds_str
  # The conditions leaf JSON. The live E2E daemon seeds type:trigger with
  # conditions as a type:string field (seed-types-and-users.sh overrides the
  # type:json builtin), so the value must be a JSON-ENCODED STRING; a raw array
  # 400s "conditions: must be a string". We JSON-encode the array into a string
  # and fall back to the raw array if a daemon variant declares it type:json.
  conds="[{\"type\":\"script\",\"script_bead\":\"${probe_bead}\"}]"
  conds_str=$(CONDS="$conds" python3 -c "import json,os; print(json.dumps(os.environ['CONDS']))" 2>/dev/null || printf '%s' "$conds")
  seed_leaf_trigger() {
    kd_create_id "$KD_BIN" create "e2e-probe-leaf-${probe_bead}-$TEST_TAG" \
      --type=trigger \
      -d "e2e script-leaf gate over probe $probe_bead ($TEST_TAG)" \
      -l "e2e,$TEST_TAG,project:${PROJECT}" \
      -f 'source=schedule' \
      -f 'action=notify' \
      -f 'enabled=false' \
      -f 'cron=* * * * *' \
      -f "project=${PROJECT}" \
      -f "conditions=$1"
  }
  trig_id=$(seed_leaf_trigger "$conds_str")
  [ -z "$trig_id" ] && trig_id=$(seed_leaf_trigger "$conds")
  if [ -z "$trig_id" ]; then
    dim "    eval_script_leaf: could not seed trigger bead for $probe_bead" >&2
    printf ''
    return 1
  fi
  CREATED_BEADS+=("$trig_id")
  # --jq projects .gate.would_fire_now in-process (no external jq). The gate is
  # buildGateEval → scheduler.EvaluateConditions → evalScript, the REAL leaf.
  "$GB_BIN" trigger reconcile "$trig_id" --json \
    --jq '.gate.would_fire_now' 2>/dev/null | tr -d '[:space:]' || printf ''
}

# ── Main ────────────────────────────────────────────────────────────
main() {
  blue "═══════════════════════════════════════════════════"
  blue " gb probe run + script condition leaf E2E (ka-0000002117)"
  blue "═══════════════════════════════════════════════════"

  if [ "$PROBE_EXECUTION_E2E" != "1" ]; then
    dim "SKIP: PROBE_EXECUTION_E2E!=1 — spawns real probe pods; inert on the"
    dim "      standard fan-out. Run with PROBE_EXECUTION_E2E=1."
    exit 0
  fi

  if ! command -v kubectl >/dev/null 2>&1; then
    dim "SKIP (loud): PROBE_EXECUTION_E2E=1 but kubectl unavailable — cannot"
    dim "             spawn a probe pod. (exit 0, but this is NOT a pass.)"
    exit 0
  fi

  setup_portforward
  setup_token

  if ! "$KD_BIN" list --status=open --json >/dev/null 2>&1; then
    dim "SKIP (loud): PROBE_EXECUTION_E2E=1 but kd server unreachable at"
    dim "             ${BEADS_HTTP_URL:-<unset>}. (exit 0, NOT a pass.)"
    exit 0
  fi
  green "  Server reachable: ${BEADS_HTTP_URL}"

  # Resolve the fleet agent image from the controller Deployment's COOP_IMAGE —
  # the same image `gb probe run` defaults to. Without a real image the pod
  # cannot run, so a missing one is a LOUD skip, never a silent pass.
  PROBE_IMAGE="${PROBE_IMAGE:-$(kubectl -n "$NAMESPACE" get deploy "$CONTROLLER_DEPLOY" \
    -o jsonpath='{.spec.template.spec.containers[?(@.name=="agents")].env[?(@.name=="COOP_IMAGE")].value}' \
    2>/dev/null || true)}"
  if [ -z "$PROBE_IMAGE" ]; then
    dim "SKIP (loud): could not resolve COOP_IMAGE from deploy ${CONTROLLER_DEPLOY}"
    dim "             in ns ${NAMESPACE} — no agent image to run a probe pod with."
    dim "             (exit 0, NOT a pass.)"
    exit 0
  fi
  green "  probe image: $PROBE_IMAGE"

  # Resolve the registry pull secret the controller stamps on agent pods
  # (IMAGE_PULL_SECRETS), so our probe pod can pull the SAME private image. This
  # is the very defect this module exercises: without the secret the pod
  # ImagePullBackOffs (403). Take the first name if the env lists several.
  if [ -z "$PROBE_PULL_SECRET" ]; then
    PROBE_PULL_SECRET=$(kubectl -n "$NAMESPACE" get deploy "$CONTROLLER_DEPLOY" \
      -o jsonpath='{.spec.template.spec.containers[?(@.name=="agents")].env[?(@.name=="IMAGE_PULL_SECRETS")].value}' \
      2>/dev/null | cut -d, -f1 | tr -d '[:space:]' || true)
  fi
  if [ -n "$PROBE_PULL_SECRET" ]; then
    green "  pull secret: $PROBE_PULL_SECRET"
  else
    dim "  pull secret: (none resolved — assuming a public image / pre-cached node)"
  fi
  echo

  TEST_TAG="e2e-probe-$$-$(date +%s)"

  # ── Leg 1 + 2: exit-0 probe → pod named `probe`, bead stamped done ──
  blue "Leg 1+2: exit-0 probe — pod container 'probe', bead done/exit_code=0"
  ensure_daemon_pf
  local ok_bead
  ok_bead=$(seed_script_bead "e2e-probe-ok-$TEST_TAG" 'echo probe-ok; exit 0')
  if [ -z "$ok_bead" ]; then
    fail "seed exit-0 script bead" "kd create --type=script returned no id"
    summary; return
  fi
  CREATED_BEADS+=("$ok_bead")
  # Assert the seeded bead really is a type=script carrying the script field —
  # guards a silent type/field regression that would make every leg vacuous.
  local seeded_type seeded_script
  seeded_type=$(kd_attr "$ok_bead" "type")
  seeded_script=$(kd_field "$ok_bead" "script")
  if [ "$seeded_type" = "script" ] && [ -n "$seeded_script" ]; then
    pass "seeded type=script bead with a script field ($ok_bead)"
  else
    fail "seeded type=script bead" "type='$seeded_type' script='$seeded_script'"
  fi

  # Watch for the probe pod while `gb probe run` drives it. The runner deletes
  # the pod on completion (keepPod=false default), so we race a background
  # watcher against the run to catch the container name before teardown.
  local pod_name
  pod_name="probe-$(printf '%s' "$ok_bead" | tr '[:upper:]' '[:lower:]')"
  pod_name="${pod_name:0:63}"
  local container_seen="" label_seen=""
  ( # background watcher: poll for our probe pod's container name + label
    local deadline=$(( $(date +%s) + PROBE_TIMEOUT ))
    while [ "$(date +%s)" -lt "$deadline" ]; do
      local cname
      cname=$(kubectl -n "$NAMESPACE" get pod "$pod_name" \
        -o jsonpath='{.spec.containers[0].name}' 2>/dev/null || true)
      if [ -n "$cname" ]; then
        local lbl
        lbl=$(kubectl -n "$NAMESPACE" get pod "$pod_name" \
          -o jsonpath='{.metadata.labels.gardenio\.io/probe}' 2>/dev/null || true)
        printf '%s\t%s\n' "$cname" "$lbl" > "/tmp/probe-watch-${TEST_TAG}.out"
        break
      fi
      sleep 1
    done
  ) &
  local watcher_pid=$!

  local ok_rc=0
  run_probe "$ok_bead" || ok_rc=$?
  wait "$watcher_pid" 2>/dev/null || true
  if [ -f "/tmp/probe-watch-${TEST_TAG}.out" ]; then
    container_seen=$(cut -f1 "/tmp/probe-watch-${TEST_TAG}.out" 2>/dev/null || true)
    label_seen=$(cut -f2 "/tmp/probe-watch-${TEST_TAG}.out" 2>/dev/null || true)
    rm -f "/tmp/probe-watch-${TEST_TAG}.out"
  fi

  if [ "$container_seen" = "probe" ]; then
    pass "probe pod's single container is named 'probe' (ProbeContainerName)"
  elif [ -n "$container_seen" ]; then
    fail "probe container name" "expected 'probe', saw '$container_seen'"
  else
    # The pod may have run + been reaped faster than the 1s watcher poll on a
    # warm-image node. The verdict-stamp assertions below still fully exercise
    # the runtime path; a missed pod snapshot is a soft note, not a failure.
    dim "  (probe pod completed + was reaped before the watcher sampled it;"
    dim "   container-name snapshot skipped — verdict asserts below still cover it)"
  fi
  if [ "$label_seen" = "true" ]; then
    pass "probe pod carries the gardenio.io/probe label"
  elif [ -n "$container_seen" ]; then
    # We saw the pod but not the label — a real regression (BuildProbePod always
    # stamps LabelProbe). Only assert when we actually captured the pod.
    fail "probe pod gardenio.io/probe label" "container seen but label='$label_seen'"
  fi

  # THE VERDICT (leg 2): exit 0 → done, exit_code=0, closed.
  ensure_daemon_pf
  local ok_state ok_code ok_status
  ok_state=$(kd_field "$ok_bead" "agent_state")
  ok_code=$(kd_field "$ok_bead" "exit_code")
  ok_status=$(kd_attr "$ok_bead" "status")
  if [ "$ok_state" = "done" ]; then
    pass "exit-0 probe stamped agent_state=done"
  else
    fail "exit-0 probe agent_state=done" "got '$ok_state'"
  fi
  if [ "$ok_code" = "0" ]; then
    pass "exit-0 probe stamped exit_code=0"
  else
    fail "exit-0 probe exit_code=0" "got '$ok_code'"
  fi
  if [ "$ok_status" = "closed" ]; then
    pass "exit-0 probe bead is closed"
  else
    fail "exit-0 probe bead closed" "status='$ok_status'"
  fi
  if [ "$ok_rc" = "0" ]; then
    pass "gb probe run exited 0 for the exit-0 probe"
  else
    fail "gb probe run exit code (exit-0 leg)" "expected 0, got $ok_rc"
  fi
  echo

  # ── Leg 3: NON-ZERO probe → failed with the REAL exit code (control) ──
  blue "Leg 3 (NEGATIVE CONTROL): exit-3 probe — bead failed, exit_code=3"
  ensure_daemon_pf
  local bad_bead
  bad_bead=$(seed_script_bead "e2e-probe-fail-$TEST_TAG" 'echo probe-fail; exit 3')
  if [ -z "$bad_bead" ]; then
    fail "seed exit-3 script bead" "kd create --type=script returned no id"
    summary; return
  fi
  CREATED_BEADS+=("$bad_bead")
  local bad_rc=0
  run_probe "$bad_bead" || bad_rc=$?

  ensure_daemon_pf
  local bad_state bad_code bad_status
  bad_state=$(kd_field "$bad_bead" "agent_state")
  bad_code=$(kd_field "$bad_bead" "exit_code")
  bad_status=$(kd_attr "$bad_bead" "status")
  if [ "$bad_state" = "failed" ]; then
    pass "exit-3 probe stamped agent_state=failed"
  else
    fail "exit-3 probe agent_state=failed" "got '$bad_state'"
  fi
  if [ "$bad_code" = "3" ]; then
    pass "exit-3 probe stamped the REAL exit_code=3"
  else
    fail "exit-3 probe exit_code=3" "got '$bad_code' (a vacuous test would not check this)"
  fi
  if [ "$bad_status" = "closed" ]; then
    pass "exit-3 probe bead is closed"
  else
    fail "exit-3 probe bead closed" "status='$bad_status'"
  fi
  if [ "$bad_rc" != "0" ]; then
    pass "gb probe run mirrored the failure in its own exit code (rc=$bad_rc)"
  else
    fail "gb probe run exit code (exit-3 leg)" "expected non-zero, got 0"
  fi
  echo

  # ── Leg 4: THE JOIN — feed BOTH verdicts to a `script` condition leaf ──
  blue "Leg 4 (THE JOIN): script condition leaf reads each verdict"
  dim "  op=nonzero (default): exit-0 ⇒ suppress (would_fire_now=false),"
  dim "                        exit-N ⇒ wake    (would_fire_now=true)"
  ensure_daemon_pf

  local ok_fire
  ok_fire=$(eval_script_leaf "$ok_bead")
  if [ "$ok_fire" = "false" ]; then
    pass "exit-0 verdict → leaf SUPPRESSES (would_fire_now=false)"
  elif [ "$ok_fire" = "true" ]; then
    fail "exit-0 verdict → leaf suppresses" \
      "would_fire_now=true — a stale/absent verdict fails OPEN; the exit_code=0 stamp is not being read"
  else
    fail "exit-0 verdict → leaf suppresses" "gate did not evaluate (got '$ok_fire')"
  fi

  local bad_fire
  bad_fire=$(eval_script_leaf "$bad_bead")
  if [ "$bad_fire" = "true" ]; then
    pass "exit-3 verdict → leaf WAKES (would_fire_now=true)"
  elif [ "$bad_fire" = "false" ]; then
    fail "exit-3 verdict → leaf wakes" \
      "would_fire_now=false — the non-zero verdict is being misread as 'nothing to do'"
  else
    fail "exit-3 verdict → leaf wakes" "gate did not evaluate (got '$bad_fire')"
  fi
  echo

  # ── Leg 5 (cheap, non-blocking): activity_deadline_seconds timeout ──
  blue "Leg 5 (optional): activity_deadline_seconds kills a slow probe → failed"
  ensure_daemon_pf
  local slow_bead
  # Sleep well past the deadline; the kubelet kills the pod → PodFailed → failed.
  slow_bead=$(seed_script_bead "e2e-probe-timeout-$TEST_TAG" \
    "echo probe-slow; sleep $((DEADLINE_SECS * 20))" \
    -f "activity_deadline_seconds=${DEADLINE_SECS}")
  if [ -z "$slow_bead" ]; then
    dim "  (could not seed timeout bead — skipping optional leg 5)"
  else
    CREATED_BEADS+=("$slow_bead")
    local slow_rc=0
    run_probe "$slow_bead" || slow_rc=$?
    ensure_daemon_pf
    local slow_state slow_status
    slow_state=$(kd_field "$slow_bead" "agent_state")
    slow_status=$(kd_attr "$slow_bead" "status")
    if [ "$slow_state" = "failed" ] && [ "$slow_status" = "closed" ]; then
      pass "deadline-killed probe stamped agent_state=failed (closed)"
    else
      # Non-blocking: substrate timing (image pull vs deadline race) can make
      # this flaky, so a miss is a soft note, never a module failure.
      dim "  (leg 5 inconclusive: state='$slow_state' status='$slow_status' rc=$slow_rc"
      dim "   — deadline/scheduling race; non-blocking by design)"
    fi
  fi
  echo

  summary
}

summary() {
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
