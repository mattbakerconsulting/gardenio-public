#!/usr/bin/env bash
# test-gvisor-freeze-interrupt.sh — E2E: the C2 interruptible-freeze (re-nudge
# mid-FREEZE) RELEASE path on the REAL gvisor-poc substrate. Bead ka-0000004367
# (parent gVisor production-readiness epic ka-0000004667; ingress task
# ka-0000005834).
#
# THE GAP THIS CLOSES:
#   The C2 freeze-interrupt path (ka-0000005834) is UNIT-tested only — the
#   in-flight registry / abort defer / discardAbortedFreeze / handler contract
#   are asserted at the unit layer (freezethaw_interrupt_test.go +
#   freeze_interrupt_endpoint_test.go) but NOTHING drove a REAL freeze on real
#   runsc, fired a REAL POST /api/v1/freeze/interrupt through the genuine
#   handler mid-checkpoint, and asserted the five C2 guarantees end-to-end on
#   the live substrate:
#     (a) the freeze ABORTS — Freeze returns ErrFreezeAborted, the node exec
#         ctx was cancelled by the interrupt;
#     (b) Pod A's analogue (the live runsc sandbox) is STILL RUNNING —
#         `--leave-running` kept it alive across the checkpoint + abort;
#     (c) the message is SERVED by the live agent — its in-RAM monotonic
#         counter advanced past its checkpoint-time value (never went dark),
#         and the endpoint reported aborted=true;
#     (d) NO orphaned checkpoint image is left on the node — discardAbortedFreeze
#         reaped the image at the controller-owned safe path (FreezeImagePath);
#     (e) NO snapshot was persisted — the store has no thaw target.
#
#   The standing user-response-latency gate's MID-FREEZE leg only times a
#   LEAVE_RUNNING_ONLY checkpoint window — it never injects a message, never
#   hits the abort endpoint, and never asserts (a)–(e). This module is the
#   missing severe e2e the production-readiness epic demands.
#
# WHY THE REAL Go CODE, NOT A BASH RE-IMPLEMENTATION:
#   The shell harness cannot drive the in-process Freeze/AbortFreeze registry,
#   the abort-vs-completion finalize defer, or the discardAbortedFreeze image
#   reap — those are Go control-plane invariants of FreezeThawManager. So this
#   module builds and runs cmd/freeze-interrupt-probe from the controller
#   source tree (same pattern as cmd/thawepoch-probe / test-gvisor-rehydrate-
#   ack.sh): the probe drives the GENUINE Freeze / AbortFreeze / abort defer /
#   reap and the SHARED podmanager.FreezeInterruptHandler that backs the
#   controller's real endpoint, against a REAL `runsc checkpoint --leave-running`
#   of the live PoC sandbox. A regression inside any of those Go internals (or
#   the endpoint's wire contract) fails THIS e2e, not just the next prod canary.
#
# HOW IT WORKS (reuses the proven freeze-interrupt-probe + node script):
#   1. Schedule a privileged hostPID+hostPath probe pod onto the gvisor-poc node.
#   2. Build the single-container runsc agent + RUN it (do NOT checkpoint) via
#      the proven single-container-resume-node.sh RUN_ONLY=1 mode — leaves a
#      live `sc-agent` sandbox + a counter-advancing python agent in
#      $BDIR/work/agent.log. This is the freeze TARGET.
#   3. Cross-compile cmd/freeze-interrupt-probe (linux/amd64 static) from the
#      controller source, ship it to the node.
#   4. Run the probe on the node: it drives the GENUINE FreezeThawManager.Freeze
#      with a NodeExecutor that performs a REAL `runsc checkpoint --leave-
#      running` of sc-agent into the controller-owned image path, blocks until
#      the freeze ctx is cancelled, then fires a real POST to the genuine
#      FreezeInterruptHandler. Asserts (a)–(e) on real substrate.
#   5. Re-assert from the harness: status of the runsc sandbox is `running`,
#      the controller-owned image path is empty, and the counter advanced.
#
# runsc QUIRK: `runsc checkpoint`/`state`/`ps` can HANG on process exit
# (documented in test-gvisor-freeze-thaw.sh). Every runsc call is wrapped on
# both sides in `timeout` and asserted on OBSERVED post-state, never on rc.
#
# OPT-IN / GATING (NEVER silently passes):
#   Targets the dedicated gvisor-poc namespace + runsc node pool, NOT the
#   per-run e2e namespace. Runs only when GVISOR_E2E=1 (or
#   FREEZE_INTERRUPT_E2E=1) AND the substrate is present (runsc RuntimeClass +
#   create-pods in $GVISOR_NS). Opt-in unset → SKIP clean (exit 0, inert in
#   the standard shards). Opt-in SET but substrate ABSENT → SKIP LOUD (still
#   exit 0 so an absent node pool does not red the suite, but a skip is never
#   mistaken for a pass).
#   Wire it in via:  GVISOR_E2E=1 ONLY=gvisor-freeze-interrupt run-shard.sh
#
# Prerequisites (all satisfied by escalation ka-0000004674):
#   - kubectl context on america-e2e-eks
#   - ns gvisor-poc (PSA=privileged), RuntimeClass runsc, NodePool gvisor-poc
#   - SA gardenio-agent: create pods + pods/exec in gvisor-poc
#   - a Go toolchain in the task (to build cmd/freeze-interrupt-probe from source)
#
# Usage:
#   GVISOR_E2E=1 ./gardenio/tests/e2e/scripts/test-gvisor-freeze-interrupt.sh
#   GVISOR_E2E=1 KEEP_PODS=1 ./...                                  # debug
set -uo pipefail

# ── Config (all overridable via env) ────────────────────────────────
GVISOR_NS="${GVISOR_NS:-gvisor-poc}"
RUNTIME_CLASS="${RUNTIME_CLASS:-runsc}"
RUN_ID="${RUN_ID:-$$}"
PROBE="${PROBE:-freeze-int-probe-${RUN_ID}}"
# Where the proven node scripts live (shipped to the node via base64).
POC_DIR="${POC_DIR:-gardenio/tests/e2e/scripts/gvisor-node}"
# Controller module dir (to build cmd/freeze-interrupt-probe from source).
CONTROLLER_DIR="${CONTROLLER_DIR:-gardenio/controller}"
POD_READY_TIMEOUT="${POD_READY_TIMEOUT:-180s}"
# How long the probe lets the live agent settle between the in-flight checkpoint
# and the interrupt — gives the counter a real delta for (c).
CHECKPOINT_SETTLE_MS="${CHECKPOINT_SETTLE_MS:-1500}"
# Agent identity the probe uses; doubles as the sanitized key for FreezeImagePath
# under the controller-owned safe path /var/lib/gardenio/ckpt/<agent>.
PROBE_AGENT="${PROBE_AGENT:-freeze-int-probe-agent-${RUN_ID}}"

# Shared gvisor-poc substrate hardening helpers (warm-node + signature-keyed
# retries). Decision ka-0000000122 (chosen=harden), root flake ka-0000007234.
# shellcheck disable=SC1091
source "$(dirname "${BASH_SOURCE[0]}")/lib-gvisor-node.sh"

PASS=0
FAIL=0
ERRORS=()
CREATED_PODS=()

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
# SKIP loud — opt-in unset OR substrate missing. Always exit 0 (an absent node
# pool must NOT red the suite) but print a banner so a skip is never a pass.
skip_all() {
  local why="$1" tag="${2:-substrate}"
  if [ "$tag" = "loud" ]; then
    red "════════════════════════════════════════════════════"
    red " SKIP gvisor-freeze-interrupt (LOUD): $why"
    red " OPT-IN WAS SET but the gvisor-poc substrate is absent — this run did"
    red " NOT assert the interruptible-freeze C2 path. A green suite here proves"
    red " NOTHING about the mid-checkpoint abort + endpoint contract. Provision"
    red " the gvisor-poc node pool + runsc RuntimeClass (escalation ka-0000004674)."
    red "════════════════════════════════════════════════════"
  else
    blue "SKIP gvisor-freeze-interrupt: $why"
    dim  "  Opt-in module: set GVISOR_E2E=1 (or FREEZE_INTERRUPT_E2E=1) and run on the"
    dim  "  gvisor-poc substrate (runsc RuntimeClass + create-pods in ns $GVISOR_NS)."
  fi
  if [ -n "${E2E_SKIP_SENTINEL:-}" ]; then printf '%s\n' "$*" >"$E2E_SKIP_SENTINEL" 2>/dev/null || true; fi  # skip visibility (ka-0000000989)
  exit 0
}

k() { kubectl -n "$GVISOR_NS" "$@"; }

# Per-node-call wall-clock cap (runsc can HANG on process exit). Every node-side
# exec is wrapped on BOTH sides: a node-side `timeout` (kills the hung chroot)
# and a client-side `timeout` (so kubectl exec can't block the harness). Mirrors
# test-resume-latency.sh / test-gvisor-rehydrate-ack.sh.
NODE_CALL_TIMEOUT="${NODE_CALL_TIMEOUT:-180}"
node_exec() {
  local cap="$1"; shift
  timeout "$((cap + 20))" kubectl -n "$GVISOR_NS" exec "$PROBE" -- timeout "$cap" chroot /host bash -c "$1" 2>/dev/null
}

cleanup() {
  blue "── cleanup ──"
  # Force-delete the live sc-agent the harness owns (the node script intentionally
  # does NOT delete it so the probe can measure against the live sandbox).
  node_exec 30 'ROOT=/run/containerd/runsc/k8s.io; timeout 10 nsenter -t 1 -m -p -n -- runsc --root=$ROOT delete --force sc-agent >/dev/null 2>&1 || true' >/dev/null 2>&1 || true
  # Reap any leftover image under the controller-owned safe path (best-effort).
  node_exec 30 "timeout 10 nsenter -t 1 -m -- rm -rf /var/lib/gardenio/ckpt/${PROBE_AGENT//[^a-zA-Z0-9_-]/_} 2>/dev/null || true" >/dev/null 2>&1 || true
  if [ "${KEEP_PODS:-0}" != "1" ]; then
    for p in "${CREATED_PODS[@]:-}"; do
      [ -n "$p" ] && k delete pod "$p" --grace-period=0 --force >/dev/null 2>&1 || true
    done
    k delete pod "$PROBE" --grace-period=0 --force >/dev/null 2>&1 || true
  else
    dim "  KEEP_PODS=1 — leaving probe + ${#CREATED_PODS[@]} placeholder pods for debug"
  fi
}
trap cleanup EXIT

echo "════════════════════════════════════════════════════"
echo " gVisor C2 interruptible-freeze — real substrate E2E (ka-0000004367)"
echo " ns=$GVISOR_NS runtimeClass=$RUNTIME_CLASS agent=$PROBE_AGENT settle=${CHECKPOINT_SETTLE_MS}ms"
echo "════════════════════════════════════════════════════"

# ── Gate 0: opt-in + substrate preflight ────────────────────────────
if [ "${GVISOR_E2E:-0}" != "1" ] && [ "${FREEZE_INTERRUPT_E2E:-0}" != "1" ]; then
  skip_all "GVISOR_E2E / FREEZE_INTERRUPT_E2E != 1 (opt-in module)" off
fi
command -v kubectl >/dev/null 2>&1 || skip_all "kubectl not installed" loud
command -v go >/dev/null 2>&1 || skip_all "go toolchain not present (need it to build cmd/freeze-interrupt-probe)" loud
kubectl get runtimeclass "$RUNTIME_CLASS" >/dev/null 2>&1 \
  || skip_all "RuntimeClass '$RUNTIME_CLASS' not present on this cluster" loud
[ "$(kubectl auth can-i create pods -n "$GVISOR_NS" 2>/dev/null)" = "yes" ] \
  || skip_all "cannot create pods in ns '$GVISOR_NS'" loud
[ -f "$POC_DIR/single-container-resume-node.sh" ] \
  || skip_all "node script single-container-resume-node.sh missing under $POC_DIR" loud
[ -d "$CONTROLLER_DIR/cmd/freeze-interrupt-probe" ] \
  || skip_all "cmd/freeze-interrupt-probe missing under $CONTROLLER_DIR" loud

blue "Substrate present — running C2 interruptible-freeze assertions."

# ── 1. Build cmd/freeze-interrupt-probe (the REAL Freeze + handler driver) ─
# Cross-compile for the node (linux/amd64, static) so it runs in the host chroot
# regardless of the task's libc. This is the genuine FreezeThawManager + abort
# defer + discardAbortedFreeze + FreezeInterruptHandler — NOT a re-implementation.
blue "[1/6] building cmd/freeze-interrupt-probe (real Freeze + AbortFreeze + endpoint)"
PROBE_BIN="$(mktemp /tmp/freeze-interrupt-probe.XXXXXX)"
if ( cd "$CONTROLLER_DIR" && CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -o "$PROBE_BIN" ./cmd/freeze-interrupt-probe/ ) >"/tmp/freeze-interrupt-build-${RUN_ID}.log" 2>&1; then
  pass "freeze-interrupt-probe built ($(wc -c < "$PROBE_BIN" | tr -d ' ') bytes, linux/amd64 static)"
else
  fail "build freeze-interrupt-probe" "go build failed: $(tail -8 "/tmp/freeze-interrupt-build-${RUN_ID}.log")"
  echo "════════ Summary: $PASS passed, $FAIL failed ════════"
  exit 1
fi

# ── 2. Schedule the privileged probe + ship node script + binary ────
blue "[2/6] scheduling privileged probe '$PROBE' onto the gvisor-poc node"
cat <<YAML | k apply -f - >/dev/null 2>&1
apiVersion: v1
kind: Pod
metadata:
  name: ${PROBE}
  namespace: ${GVISOR_NS}
  labels: {app: freeze-int-probe, test: ka-0000004367}
spec:
  restartPolicy: Never
  nodeSelector: {gvisor: enabled}
  tolerations:
  - {key: gvisor, operator: Equal, value: enabled, effect: NoSchedule}
  hostPID: true
  hostNetwork: true
  containers:
  - name: probe
    image: public.ecr.aws/docker/library/ubuntu:22.04
    command: ["sleep", "1800"]
    securityContext: {privileged: true}
    volumeMounts:
    - {name: host, mountPath: /host}
    - {name: run, mountPath: /run}
    - {name: gardenio, mountPath: /var/lib/gardenio}
  volumes:
  - {name: host, hostPath: {path: /}}
  - {name: run, hostPath: {path: /run}}
  - {name: gardenio, hostPath: {path: /var/lib/gardenio, type: DirectoryOrCreate}}
YAML
if ! k wait --for=condition=Ready "pod/$PROBE" --timeout="$POD_READY_TIMEOUT" >/dev/null 2>&1; then
  fail "probe pod Ready" "pod $PROBE did not become Ready in $POD_READY_TIMEOUT"
  k get pod "$PROBE" -o wide 2>&1 | sed 's/^/    /'
  echo "════════ Summary: $PASS passed, $FAIL failed ════════"
  exit 1
fi
pass "probe scheduled + Ready on the gvisor node"

# Warm-node readiness gate: a freshly Karpenter-provisioned gvisor=poc node
# reports its probe pod Ready while runsc + /var/tmp + the runsc state root are
# still settling. Bounded-poll until the node is genuinely warm before any real
# work (decision ka-0000000122, root flake ka-0000007234).
if gvisor_warm_node_gate node_exec; then
  pass "gvisor-poc node warm (runsc present + /var/tmp writable + runsc-root reachable)"
else
  fail "gvisor-poc node warm-gate" "$GVISOR_WARM_DIAG"
  echo "════════ Summary: $PASS passed, $FAIL failed ════════"
  exit 1
fi

# Ship the proven node script + the freeze-interrupt-probe binary. Wrapped in a
# bounded signature-keyed retry: a single cold-start base64-copy miss self-heals
# (the exact ka-0000007234 'ship node assets' signature).
ship_node_assets() {
  local b64
  b64="$(base64 -w0 "$POC_DIR/single-container-resume-node.sh")"
  k exec "$PROBE" -- chroot /host bash -c \
    "echo $b64 | base64 -d > /var/tmp/single-container-resume-node.sh && chmod +x /var/tmp/single-container-resume-node.sh" >/dev/null 2>&1 || return 1
  # The probe binary may be a few MB — stream via base64 to avoid an over-long argv.
  base64 -w0 "$PROBE_BIN" | k exec -i "$PROBE" -- chroot /host bash -c \
    'base64 -d > /var/tmp/freeze-interrupt-probe && chmod +x /var/tmp/freeze-interrupt-probe' >/dev/null 2>&1 || return 1
  # Sanity: the shipped binary actually runs on the node (rejects an unknown flag).
  # Capture, then match -- do NOT pipe node_exec into `grep -q`. This function's
  # status drives `gvisor_retry_step node-asset-ship 4 3`, these scripts run
  # under `set -o pipefail`, and grep -q exits at the first match without
  # draining. kubectl exec is then writing to a pipe with no reader, takes
  # EPIPE, and pipefail promotes its failure to the pipeline's -- so a match
  # reads as a miss and costs a retry that re-ships a multi-MB binary, or on
  # four losses reds the whole module. (The INNER pipeline inside the quoted
  # node-side script is fine: `bash -c` has pipefail OFF, so `$?` is grep's
  # status by design and the probe's EPIPE death is invisible there.)
  local out; out="$(node_exec 30 '/var/tmp/freeze-interrupt-probe -nonexistent-flag 2>&1 | grep -q "flag provided but not defined"; echo RC=$?')" || return 1
  grep -q 'RC=0' <<<"$out"
}
if gvisor_retry_step node-asset-ship 4 3 ship_node_assets; then
  pass "node script + freeze-interrupt-probe binary shipped + executable (attempts=$GVISOR_STEP_ATTEMPTS)"
else
  fail "ship node assets" "base64 copy to /var/tmp failed or binary not executable on node — $GVISOR_STEP_DIAG"
  echo "════════ Summary: $PASS passed, $FAIL failed ════════"
  exit 1
fi

# ── 3. STAND UP THE LIVE FREEZE TARGET ──────────────────────────────
# Build the single-container OCI bundle + RUN the runsc sandbox (no checkpoint).
# Leaves sc-agent ALIVE with a counter-advancing python agent in /var/tmp/sc/work
# /agent.log — the freeze target the probe checkpoints + aborts. (The proven
# node script's RUN_ONLY=1 mode, ka-0000004367 addition.)
blue "[3/6] standing up live sc-agent sandbox (the freeze target)"
do_run_only() {
  local out
  out=$(node_exec 240 "RUN_ONLY=1 /var/tmp/single-container-resume-node.sh 2>&1; true")
  grep -q 'RUN_ONLY_PROVEN=1' <<<"$out"
}
if gvisor_retry_step run-only-sandbox 3 5 do_run_only; then
  status=$(node_exec 15 "ROOT=/run/containerd/runsc/k8s.io; timeout 10 nsenter -t 1 -m -p -n -- runsc --root=\$ROOT state sc-agent 2>/dev/null | python3 -c 'import sys,json;print(json.load(sys.stdin)[\"status\"])' 2>/dev/null" | tr -d '\r')
  pass "sc-agent running on the node (status=${status:-running}; attempts=$GVISOR_STEP_ATTEMPTS)"
else
  fail "stand up live sc-agent" "RUN_ONLY did not prove the sandbox running — $GVISOR_STEP_DIAG"
  echo "════════ Summary: $PASS passed, $FAIL failed ════════"
  exit 1
fi

# ── 4. RUN THE PROBE: real checkpoint + real interrupt ──────────────
# The probe drives the GENUINE FreezeThawManager.Freeze (registry + abort defer
# + discardAbortedFreeze image reap) with a NodeExecutor that performs a REAL
# `runsc checkpoint --leave-running` of sc-agent into the controller-owned safe
# path, then fires a real POST to the genuine FreezeInterruptHandler. Asserts
# (a)–(e) on real substrate; exits 0 iff every invariant held.
blue "[4/6] driving real Freeze + real POST /api/v1/freeze/interrupt against live sc-agent"
RES_LINE=""
PROBE_OUT="/tmp/freeze-interrupt-probe-${RUN_ID}.out"
node_exec "$NODE_CALL_TIMEOUT" \
  "/var/tmp/freeze-interrupt-probe -agent='$PROBE_AGENT' -sc-cid=sc-agent -bundle-dir=/var/tmp/sc -checkpoint-settle-ms=$CHECKPOINT_SETTLE_MS 2>&1" \
  > "$PROBE_OUT" || true
RES_LINE=$(grep '^FREEZE_INTERRUPT_PROBE' "$PROBE_OUT" | head -1)
dim "    $RES_LINE"

# Parse the result line for the per-invariant booleans the probe emitted.
field() { printf '%s' "$RES_LINE" | sed -n "s/.*${1}=\([^ ]*\).*/\1/p"; }
A_ABORTED_ERR=$(field aborted_err)
A_EXEC_CANCEL=$(field exec_canceled)
A_SNAP_NIL=$(field snap_nil)
ENDPOINT_ABORTED=$(field endpoint_aborted)
STATUS_AFTER=$(field status_after)
IMG_BEFORE=$(field img_before)
IMG_AFTER=$(field img_after)
COUNTER_PRE=$(field counter_pre)
COUNTER_MID=$(field counter_mid)
COUNTER_POST=$(field counter_post)
NO_SNAPSHOT=$(field no_snapshot)

if [ -z "$RES_LINE" ]; then
  fail "probe ran to completion" "no FREEZE_INTERRUPT_PROBE line in stdout — $(tail -20 "$PROBE_OUT" | sed 's/^/      /')"
  echo "════════ Summary: $PASS passed, $FAIL failed ════════"
  exit 1
fi

# (a) the freeze ABORTS — Freeze err is ErrFreezeAborted, exec ctx cancelled,
#     no snapshot returned from the call, AND the endpoint returned aborted=true.
if [ "$A_ABORTED_ERR" = "true" ] && [ "$A_EXEC_CANCEL" = "true" ] && [ "$A_SNAP_NIL" = "true" ] && [ "$ENDPOINT_ABORTED" = "true" ]; then
  pass "(a) freeze aborted: Freeze→ErrFreezeAborted, node-exec ctx cancelled, snap=nil, endpoint aborted=true"
else
  fail "(a) freeze aborted" "ErrFreezeAborted=$A_ABORTED_ERR exec_canceled=$A_EXEC_CANCEL snap_nil=$A_SNAP_NIL endpoint_aborted=$ENDPOINT_ABORTED"
fi

# (b) Pod A's analogue is STILL RUNNING after the abort — --leave-running kept
#     the live sandbox alive across the checkpoint + abort.
if [ "$STATUS_AFTER" = "running" ]; then
  pass "(b) Pod A analogue still running: sc-agent status=$STATUS_AFTER after abort (--leave-running invariant)"
else
  fail "(b) Pod A analogue still running" "sc-agent status='$STATUS_AFTER' after abort (expected 'running')"
fi

# (c) the message is served by the LIVE agent — its counter advanced past the
#     checkpoint-time value while the freeze was in flight and again after the
#     abort. The endpoint returned aborted=true → the message would route to
#     this live agent with no thaw.
if [ -n "$COUNTER_PRE" ] && [ -n "$COUNTER_MID" ] && [ -n "$COUNTER_POST" ] \
   && [ "$COUNTER_MID" -gt "$COUNTER_PRE" ] 2>/dev/null \
   && [ "$COUNTER_POST" -gt "$COUNTER_MID" ] 2>/dev/null; then
  pass "(c) message served by live agent: counter advanced $COUNTER_PRE → $COUNTER_MID → $COUNTER_POST (agent never went dark)"
else
  fail "(c) message served by live agent" "counter did NOT advance: pre=$COUNTER_PRE mid=$COUNTER_MID post=$COUNTER_POST"
fi

# (d) NO orphaned checkpoint image is left on the node — the real checkpoint
#     wrote one (img_before=true), discardAbortedFreeze reaped it (img_after=false).
if [ "$IMG_BEFORE" != "true" ]; then
  fail "(d-pre) real checkpoint wrote an image" "img_before=$IMG_BEFORE — the checkpoint substrate failed before the abort, (d) would be vacuous"
elif [ "$IMG_AFTER" = "false" ]; then
  pass "(d) no orphaned checkpoint image: discardAbortedFreeze reaped FreezeImagePath($PROBE_AGENT) after abort"
else
  # Cross-check from the harness — host-namespaced rm probe of the controller-owned safe path.
  SANITIZED_AGENT="${PROBE_AGENT//[^a-zA-Z0-9_-]/_}"
  STILL=$(node_exec 30 "nsenter -t 1 -m -- test -f /var/lib/gardenio/ckpt/${SANITIZED_AGENT}/checkpoint.img && echo PRESENT || echo ABSENT" | tr -d '\r')
  fail "(d) no orphaned checkpoint image" "image_after_abort=$IMG_AFTER; host-ns probe of /var/lib/gardenio/ckpt/${SANITIZED_AGENT}: $STILL"
fi

# (e) NO snapshot was persisted — the in-process snapshot store has no thaw target.
if [ "$NO_SNAPSHOT" = "true" ]; then
  pass "(e) no snapshot persisted: store has no thaw target for $PROBE_AGENT (no dangling freeze→thaw on a live agent)"
else
  fail "(e) no snapshot persisted" "no_snapshot=$NO_SNAPSHOT — store still carries a thaw target for the live agent"
fi

# ── 5. Live sc-agent is STILL serving from the harness's vantage point ─
# Cross-check the controller-side invariants from outside the probe: the runsc
# sandbox must still be running AND its agent log counter must STILL be moving
# after the probe exits, proving the abort really left the agent live.
blue "[5/6] cross-check live sandbox liveness from the harness (post-probe)"
LIVE_STATUS=$(node_exec 15 "ROOT=/run/containerd/runsc/k8s.io; timeout 10 nsenter -t 1 -m -p -n -- runsc --root=\$ROOT state sc-agent 2>/dev/null | python3 -c 'import sys,json;print(json.load(sys.stdin)[\"status\"])' 2>/dev/null" | tr -d '\r')
if [ "$LIVE_STATUS" = "running" ]; then
  pass "post-probe sc-agent status=running (the abort truly left the live agent up; not an orphaned shell)"
else
  fail "post-probe sc-agent still running" "status='$LIVE_STATUS' (expected 'running')"
fi

LIVE_COUNTER_1=$(node_exec 15 "tail -1 /var/tmp/sc/work/agent.log 2>/dev/null | python3 -c 'import sys,json;print(json.load(sys.stdin)[\"counter\"])' 2>/dev/null" | tr -d '\r')
sleep 2
LIVE_COUNTER_2=$(node_exec 15 "tail -1 /var/tmp/sc/work/agent.log 2>/dev/null | python3 -c 'import sys,json;print(json.load(sys.stdin)[\"counter\"])' 2>/dev/null" | tr -d '\r')
if [ -n "$LIVE_COUNTER_1" ] && [ -n "$LIVE_COUNTER_2" ] && [ "$LIVE_COUNTER_2" -gt "$LIVE_COUNTER_1" ] 2>/dev/null; then
  pass "post-probe agent counter advancing: $LIVE_COUNTER_1 → $LIVE_COUNTER_2 (live agent is still serving)"
else
  fail "post-probe agent counter advancing" "counter did NOT advance over 2s: $LIVE_COUNTER_1 → $LIVE_COUNTER_2"
fi

# ── 6. Cross-check the image-path reap from the harness ─────────────
blue "[6/6] cross-check the controller-owned image path is empty from the harness"
SANITIZED_AGENT="${PROBE_AGENT//[^a-zA-Z0-9_-]/_}"
IMG_PATH_STATE=$(node_exec 15 "nsenter -t 1 -m -- test -e /var/lib/gardenio/ckpt/${SANITIZED_AGENT} && echo PRESENT || echo ABSENT" | tr -d '\r')
if [ "$IMG_PATH_STATE" = "ABSENT" ]; then
  pass "/var/lib/gardenio/ckpt/${SANITIZED_AGENT} fully reaped (host-ns probe confirms)"
else
  fail "image path reaped from harness" "/var/lib/gardenio/ckpt/${SANITIZED_AGENT} state=$IMG_PATH_STATE (expected ABSENT — discardAbortedFreeze did not reap)"
fi

# ── Summary ─────────────────────────────────────────────────────────
echo ""
echo "════════════════════════════════════════════════════"
echo " gVisor C2 interruptible-freeze Summary: $PASS passed, $FAIL failed"
echo "   counter=$COUNTER_PRE→$COUNTER_MID→$COUNTER_POST  sandbox_post=$LIVE_STATUS  img_path=$IMG_PATH_STATE"
echo "════════════════════════════════════════════════════"
if [ "$FAIL" -gt 0 ]; then
  for e in "${ERRORS[@]}"; do red "  - $e"; done
  exit 1
fi
green "All C2 interruptible-freeze assertions passed — real freeze on real runsc aborted by real endpoint; agent left live, no image orphaned, no snapshot persisted."
exit 0
