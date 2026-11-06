#!/usr/bin/env bash
# test-gvisor-rehydrate-ack.sh — E2E: the post-restore rehydrate-ack RELEASE path
# on the REAL gvisor-poc substrate. Bead ka-0000006321 (blocks ka-0000007916;
# related ka-0000001492 / ka-0000002058 / ka-0000004330).
#
# THE GAP THIS CLOSES:
#   4 open P1 bugs all live on the post-restore rehydrate-ack RELEASE path. The
#   contract is asserted at the unit layer (thawepoch/gate_test.go), at the
#   byte-compat layer (test-gvisor-rehydration.sh RE-IMPLEMENTS the gate in
#   bash), and in Rust (rehydration.rs vs live NATS) — but NOTHING drove a REAL
#   frozen→thawed agent on the gvisor-poc substrate and asserted, using the REAL
#   controller gate + REAL agent watchers:
#     (a) both required consumers (coop ConsumerCoop + gb ConsumerGB) WRITE their
#         Ack for the current thaw epoch,
#     (b) the controller's thawepoch.WaitForAcks RELEASES the agent, and
#     (c) it does so WITHIN deadline — NOT fail-closing with Pod B alive the full
#         window (the exact ka-0000007916 symptom on prod 2026.160.7, where Pod B
#         survived the whole 60s deadline yet neither consumer acked).
#
# WHY THE REAL Go CODE, NOT A BASH RE-IMPLEMENTATION:
#   The sibling test-gvisor-rehydration.sh asserts the on-disk marker/ack release
#   CONDITION by re-implementing it in shell (a `released()` function). That
#   proves the protocol SHAPE but not the production gate: a regression inside
#   thawepoch.WaitForAcks or thawepoch.Watcher (the code the 4 bugs actually
#   live in) would pass the bash re-implementation untouched. THIS module builds
#   and runs cmd/thawepoch-probe, which drives the genuine thawepoch.Watcher
#   (the agent-side coop+gb consumers' arm→handle→ack loop) feeding the genuine
#   thawepoch.WaitForAcks (the controller-side deadline gate) — against a marker/
#   ack round-trip on the ACTUAL node thaw-state FS, while a REAL runsc-restored
#   agent is ALIVE in Pod B's netns. A regression in either fails THIS e2e.
#
# HOW IT WORKS (reuses the proven freeze/thaw + resume-latency node mechanism):
#   1. Schedule a privileged hostPID+hostPath probe onto the gvisor-poc node.
#   2. Build a single-container runsc agent bundle + `runsc checkpoint` (FREEZE)
#      via the proven single-container-resume-node.sh (CHECKPOINT_ONLY=1) — the
#      same node driver the C7 resume-latency gate proves.
#   3. Create a REAL kubelet-owned runsc placeholder Pod B, wait Ready, resolve
#      its netns, and `runsc restore` (THAW) the checkpoint INTO Pod B's netns
#      via single-container-thaw-into-b-node.sh. Pod B is now ALIVE with the
#      restored agent task running in its kubelet-provisioned netns.
#   4. With Pod B demonstrably ALIVE, run cmd/thawepoch-probe on the node FS:
#        - release   : both consumers arm + ack the next epoch; the REAL gate
#                      RELEASES within deadline (asserts a,b,c above).
#        - failclose : coop stays silent; the REAL gate HOLDS (negative control —
#                      proves the gate genuinely fail-closes, the ka-0000007916
#                      shape, rather than always passing).
#   5. Re-assert Pod B is STILL Running after the gate released — proving the
#      ack/release happened WITH Pod B alive (the distinction the bug breaks).
#
# runsc QUIRK: `runsc checkpoint`/`restore` complete the op then hang on process
#   EXIT (documented in test-gvisor-freeze-thaw.sh). Every runsc call is wrapped
#   in `timeout` and asserted on OBSERVED post-state, never on rc.
#
# OPT-IN / GATING (NEVER silently passes):
#   Targets the dedicated gvisor-poc namespace + runsc node pool, NOT the
#   per-run e2e namespace. Runs only when GVISOR_E2E=1 (or
#   REHYDRATE_ACK_E2E=1) AND the substrate is present (runsc RuntimeClass +
#   create-pods in $GVISOR_NS). Opt-in unset → SKIP clean (exit 0, inert in the
#   standard shards). Opt-in SET but substrate ABSENT → SKIP LOUD (still exit 0
#   so an absent node pool does not red the suite, but a skip is never mistaken
#   for a pass).
#   Wire it in via:  GVISOR_E2E=1 ONLY=gvisor-rehydrate-ack run-shard.sh
#
# Prerequisites (all satisfied by escalation ka-0000004674):
#   - kubectl context on america-e2e-eks
#   - ns gvisor-poc (PSA=privileged), RuntimeClass runsc, NodePool gvisor-poc
#   - SA gardenio-agent: create pods + pods/exec in gvisor-poc
#   - a Go toolchain in the task (to build cmd/thawepoch-probe from source)
#
# Usage:
#   GVISOR_E2E=1 ./gardenio/tests/e2e/scripts/test-gvisor-rehydrate-ack.sh
#   GVISOR_E2E=1 KEEP_PODS=1 REHYDRATE_DEADLINE_MS=8000 ./...    # debug
set -uo pipefail

# ── Config (all overridable via env) ────────────────────────────────
GVISOR_NS="${GVISOR_NS:-gvisor-poc}"
RUNTIME_CLASS="${RUNTIME_CLASS:-runsc}"
RUN_ID="${RUN_ID:-$$}"
PROBE="${PROBE:-rehydrate-ack-probe-${RUN_ID}}"
# Rehydration deadline (ms) the gate is asserted against. Default mirrors
# thawepoch.DefaultDeadline (10s). The whole point of this gate is RELEASE
# WITHIN this budget — assertion (c).
REHYDRATE_DEADLINE_MS="${REHYDRATE_DEADLINE_MS:-10000}"
# Where the proven node scripts live (shipped to the node via base64).
POC_DIR="${POC_DIR:-gardenio/tests/e2e/scripts/gvisor-node}"
# Controller module dir (to build cmd/thawepoch-probe from source).
CONTROLLER_DIR="${CONTROLLER_DIR:-gardenio/controller}"
BUSYBOX_IMAGE="${BUSYBOX_IMAGE:-public.ecr.aws/docker/library/python:3.11-slim}"
POD_READY_TIMEOUT="${POD_READY_TIMEOUT:-180s}"
# Node-side thaw-state dir for the gate round-trip (analogue of
# podmanager.ThawStateHostBase/<agent>; per-run so concurrent dispatches don't
# collide).
NODE_STATE_DIR="${NODE_STATE_DIR:-/var/tmp/thaw-ack-${RUN_ID}}"

# Shared gvisor-poc substrate hardening helpers: gvisor_warm_node_gate (bounded
# warm-node readiness poll) + gvisor_retry_step (bounded signature-keyed retry
# for the flaky node-side steps). Decision ka-0000000122 (chosen=harden), root
# flake ka-0000007234.
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
  # "loud" == opt-in SET but substrate absent → shared readiness guard (hard-fail
  # under REQUIRE_SUBSTRATE=1, loud skip otherwise; task ka-0000002867). "off" ==
  # opt-in unset: inert clean skip.
  if [ "$tag" = "loud" ]; then
    gvisor_require_substrate_guard gvisor-rehydrate-ack "$why"
  else
    blue "SKIP gvisor-rehydrate-ack: $why"
    dim  "  Opt-in module: set GVISOR_E2E=1 (or REHYDRATE_ACK_E2E=1) and run on the"
    dim  "  gvisor-poc substrate (runsc RuntimeClass + create-pods in ns $GVISOR_NS)."
    if [ -n "${E2E_SKIP_SENTINEL:-}" ]; then printf '%s\n' "$*" >"$E2E_SKIP_SENTINEL" 2>/dev/null || true; fi  # skip visibility (ka-0000000989)
    exit 0
  fi
}

k() { kubectl -n "$GVISOR_NS" "$@"; }

# Per-node-call wall-clock cap (runsc can HANG on process exit). Every node-side
# exec is wrapped on BOTH sides: a node-side `timeout` (kills the hung chroot)
# and a client-side `timeout` (so kubectl exec can't block the harness). Mirrors
# test-resume-latency.sh node_exec.
NODE_CALL_TIMEOUT="${NODE_CALL_TIMEOUT:-120}"
node_exec() {
  local cap="$1"; shift
  timeout "$((cap + 20))" kubectl -n "$GVISOR_NS" exec "$PROBE" -- timeout "$cap" chroot /host bash -c "$1" 2>/dev/null
}

cleanup() {
  blue "── cleanup ──"
  node_exec 30 "rm -rf '$NODE_STATE_DIR' /var/tmp/thawepoch-probe 2>/dev/null || true" >/dev/null 2>&1 || true
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
echo " gVisor thaw rehydrate-ack RELEASE — real substrate E2E (ka-0000006321)"
echo " ns=$GVISOR_NS runtimeClass=$RUNTIME_CLASS deadline=${REHYDRATE_DEADLINE_MS}ms"
echo "════════════════════════════════════════════════════"

# ── Gate 0: opt-in + substrate preflight ────────────────────────────
if [ "${GVISOR_E2E:-0}" != "1" ] && [ "${REHYDRATE_ACK_E2E:-0}" != "1" ]; then
  skip_all "GVISOR_E2E / REHYDRATE_ACK_E2E != 1 (opt-in module)" off
fi
command -v kubectl >/dev/null 2>&1 || skip_all "kubectl not installed" loud
command -v go >/dev/null 2>&1 || skip_all "go toolchain not present (need it to build cmd/thawepoch-probe)" loud
kubectl get runtimeclass "$RUNTIME_CLASS" >/dev/null 2>&1 \
  || skip_all "RuntimeClass '$RUNTIME_CLASS' not present on this cluster" loud
[ "$(kubectl auth can-i create pods -n "$GVISOR_NS" 2>/dev/null)" = "yes" ] \
  || skip_all "cannot create pods in ns '$GVISOR_NS'" loud
[ -f "$POC_DIR/single-container-resume-node.sh" ] \
  || skip_all "node script single-container-resume-node.sh missing under $POC_DIR" loud
[ -f "$POC_DIR/single-container-thaw-into-b-node.sh" ] \
  || skip_all "node script single-container-thaw-into-b-node.sh missing under $POC_DIR" loud
[ -d "$CONTROLLER_DIR/cmd/thawepoch-probe" ] \
  || skip_all "cmd/thawepoch-probe missing under $CONTROLLER_DIR" loud

blue "Substrate present — running rehydrate-ack RELEASE assertions."

# ── 1. Build cmd/thawepoch-probe (the REAL gate+watcher driver) ─────
# Cross-compile for the node (linux/amd64, static) so it runs in the host chroot
# regardless of the task's libc. This is the genuine thawepoch.WaitForAcks +
# thawepoch.Watcher production code — NOT a re-implementation.
blue "[1/6] building cmd/thawepoch-probe (real thawepoch.WaitForAcks + Watcher)"
PROBE_BIN="$(mktemp /tmp/thawepoch-probe.XXXXXX)"
if ( cd "$CONTROLLER_DIR" && CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -o "$PROBE_BIN" ./cmd/thawepoch-probe/ ) >"/tmp/thawepoch-build-${RUN_ID}.log" 2>&1; then
  pass "thawepoch-probe built ($(wc -c < "$PROBE_BIN" | tr -d ' ') bytes, linux/amd64 static)"
else
  fail "build thawepoch-probe" "go build failed: $(tail -4 "/tmp/thawepoch-build-${RUN_ID}.log")"
  echo "════════ Summary: $PASS passed, $FAIL failed ════════"
  exit 1
fi

# ── 2. Schedule the privileged probe + ship node scripts + the binary ─
blue "[2/6] scheduling privileged probe '$PROBE' onto the gvisor-poc node"
cat <<YAML | k apply -f - >/dev/null 2>&1
apiVersion: v1
kind: Pod
metadata:
  name: ${PROBE}
  namespace: ${GVISOR_NS}
  labels: {app: rehydrate-ack-probe, test: ka-0000006321}
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
  volumes:
  - {name: host, hostPath: {path: /}}
  - {name: run, hostPath: {path: /run}}
YAML
if ! k wait --for=condition=Ready "pod/$PROBE" --timeout="$POD_READY_TIMEOUT" >/dev/null 2>&1; then
  fail "probe pod Ready" "pod $PROBE did not become Ready in $POD_READY_TIMEOUT"
  k get pod "$PROBE" -o wide 2>&1 | sed 's/^/    /'
  echo "════════ Summary: $PASS passed, $FAIL failed ════════"
  exit 1
fi
pass "probe scheduled + Ready on the gvisor node"

# Warm-node readiness gate (HARDEN, decision ka-0000000122; root flake
# ka-0000007234). A freshly Karpenter-provisioned gvisor=poc node reports its
# probe pod Ready while runsc + /var/tmp + the runsc state root are still
# settling — the rotating cold-start cause behind node-asset-ship /
# FREEZE-checkpoint / restore-into-B / Pod-B-netns-resolve flakes. Bounded-poll
# until the node is genuinely warm before any real work; a node that never warms
# (a true substrate regression) still HARD-FAILS with diagnostics.
if gvisor_warm_node_gate node_exec; then
  pass "gvisor-poc node warm (runsc present + /var/tmp writable + runsc-root reachable)"
else
  fail "gvisor-poc node warm-gate" "$GVISOR_WARM_DIAG"
  echo "════════ Summary: $PASS passed, $FAIL failed ════════"
  exit 1
fi

# Ship the proven node scripts + the thawepoch-probe binary (base64, to dodge
# exec stdin/quoting pitfalls — same idiom as test-resume-latency.sh). Wrapped in
# a bounded signature-keyed retry: a single cold-start base64-copy miss to a
# not-yet-settled /var/tmp self-heals instead of red-ing the dedicated dispatch
# (the exact ka-0000007234 'ship node assets' signature). On exhaustion it still
# HARD-FAILS — a genuinely broken copy is NOT masked.
ship_node_assets() {
  for s in single-container-resume-node.sh single-container-thaw-into-b-node.sh; do
    local b64; b64="$(base64 -w0 "$POC_DIR/$s")"
    k exec "$PROBE" -- chroot /host bash -c \
      "echo $b64 | base64 -d > /var/tmp/$s && chmod +x /var/tmp/$s" >/dev/null 2>&1 || return 1
  done
  # The binary may be a few MB — ship it through a streamed base64 over stdin to
  # avoid an over-long argv.
  base64 -w0 "$PROBE_BIN" | k exec -i "$PROBE" -- chroot /host bash -c \
    'base64 -d > /var/tmp/thawepoch-probe && chmod +x /var/tmp/thawepoch-probe' >/dev/null 2>&1 || return 1
  # Sanity: the shipped binary actually runs on the node (rejects a missing flag).
  # Capture, then match -- do NOT pipe node_exec into `grep -q`. This function's
  # status drives `gvisor_retry_step node-asset-ship 4 3`, these scripts run
  # under `set -o pipefail`, and grep -q exits at the first match without
  # draining. kubectl exec is then writing to a pipe with no reader, takes
  # EPIPE, and pipefail promotes its failure to the pipeline's -- so a match
  # reads as a miss and costs a retry that re-ships a multi-MB binary, or on
  # four losses reds the whole module. (The INNER pipeline inside the quoted
  # node-side script is fine: `bash -c` has pipefail OFF, so `$?` is grep's
  # status by design and the probe's EPIPE death is invisible there.)
  local out; out="$(node_exec 30 '/var/tmp/thawepoch-probe -mode=release 2>&1 | grep -q "state-dir is required"; echo RC=$?')" || return 1
  grep -q 'RC=0' <<<"$out"
}
if gvisor_retry_step node-asset-ship 4 3 ship_node_assets; then
  pass "node scripts + thawepoch-probe binary shipped + executable on the node (attempts=$GVISOR_STEP_ATTEMPTS)"
else
  fail "ship node assets" "base64 copy to /var/tmp failed or binary not executable on node — $GVISOR_STEP_DIAG"
  echo "════════ Summary: $PASS passed, $FAIL failed ════════"
  exit 1
fi

# ── 3. FREEZE: build the single-container agent bundle + checkpoint ──
# Reuse the PROVEN single-container-resume-node.sh (CHECKPOINT_ONLY=1): assembles
# the ROOT-agent bundle (fix path c) and `runsc checkpoint`s it to /var/tmp/ckSC
# + leaves the /var/tmp/sc bundle. We need ONLY its checkpoint artifacts; its own
# self-proof restore hangs on exit, so cap + assert by OBSERVED STATE.
blue "[3/6] FREEZE: build single-container agent + runsc checkpoint (/var/tmp/ckSC)"
# Bounded signature-keyed retry (HARDEN, ka-0000007234 'FREEZE-checkpoint'
# signature). The runsc checkpoint build can transiently lose a race on a node
# still finishing its cold start (the gd-2XhgsAZ8m0 'checkpoint artifacts absent
# on node' shape); retry the whole build+assert a bounded number of times. Each
# attempt re-runs the build and re-asserts the OBSERVED artifacts, so a genuine
# build break still exhausts the budget and HARD-FAILS with the captured tail.
BUILD_OUT=""
do_freeze_checkpoint() {
  BUILD_OUT=$(node_exec 240 "CHECKPOINT_ONLY=1 /var/tmp/single-container-resume-node.sh 2>&1; true")
  local have
  have=$(node_exec 30 'if [ -f /var/tmp/ckSC/checkpoint.img ] && [ -f /var/tmp/ckSC/pages.img ] && [ -f /var/tmp/sc/config.json ]; then echo OK; else echo MISSING; fi' | tr -d '\r')
  [ "$have" = OK ]
}
if gvisor_retry_step FREEZE-checkpoint 3 5 do_freeze_checkpoint; then
  ck_kib=$(node_exec 30 'du -sk /var/tmp/ckSC 2>/dev/null | awk "{print \$1}"' | tr -d '\r')
  pass "agent frozen — checkpoint image + bundle present (/var/tmp/ckSC ${ck_kib:-?}KiB + /var/tmp/sc/config.json; attempts=$GVISOR_STEP_ATTEMPTS)"
else
  fail "FREEZE checkpoint image" "checkpoint artifacts absent on node after retries: $(printf '%s\n' "$BUILD_OUT" | tail -6)"
  echo "════════ Summary: $PASS passed, $FAIL failed ════════"
  exit 1
fi

# Node-side resolver: given a placeholder pod UID, print "B_SENTRY_PID B_NETNS".
# Identical to test-resume-latency.sh resolve_b.
resolve_b() {
  local uid="$1"
  node_exec 45 "
    ROOT=/run/containerd/runsc/k8s.io
    RUNSC() { timeout 10 nsenter -t 1 -m -p -n -- runsc --root=\$ROOT \"\$@\"; }
    for c in \$(ctr -n k8s.io containers ls 2>/dev/null | awk 'NR>1{print \$1}'); do
      RUNSC state \$c >/dev/null 2>&1 || continue
      u=\$(RUNSC state \$c 2>/dev/null | python3 -c 'import sys,json;print(json.load(sys.stdin)[\"annotations\"].get(\"io.kubernetes.cri.sandbox-uid\",\"\"))' 2>/dev/null)
      [ \"\$u\" = \"$uid\" ] || continue
      sb=\$(RUNSC state \$c 2>/dev/null | python3 -c 'import sys,json;print(json.load(sys.stdin)[\"annotations\"][\"io.kubernetes.cri.sandbox-id\"])' 2>/dev/null)
      pid=\$(RUNSC state \$sb 2>/dev/null | python3 -c 'import sys,json;print(json.load(sys.stdin)[\"pid\"])' 2>/dev/null)
      [ -n \"\$pid\" ] && { echo \"\$pid /proc/\$pid/ns/net\"; break; }
    done
  " | tr -d '\r' | head -1
}

# ── 4. THAW: create a REAL kubelet Pod B + restore the checkpoint into it ─
blue "[4/6] THAW: create real runsc Pod B + runsc restore the frozen agent into its netns"
POD_B="rehydrate-ack-podb-${RUN_ID}"
cat <<YAML | k apply -f - >/dev/null 2>&1
apiVersion: v1
kind: Pod
metadata:
  name: ${POD_B}
  namespace: ${GVISOR_NS}
  labels: {app: rehydrate-ack-podb, test: ka-0000006321, role: restore-placeholder}
spec:
  runtimeClassName: ${RUNTIME_CLASS}
  restartPolicy: Never
  nodeSelector: {gvisor: enabled}
  tolerations:
  - {key: gvisor, operator: Equal, value: enabled, effect: NoSchedule}
  containers:
  - name: agent
    image: ${BUSYBOX_IMAGE}
    command: ["/bin/sh","-c"]
    args: ["mkdir -p /work/repo && exec sleep 36000"]
    volumeMounts: [{name: work, mountPath: /work}]
  volumes: [{name: work, emptyDir: {}}]
YAML
CREATED_PODS+=("$POD_B")
if ! k wait --for=condition=Ready "pod/$POD_B" --timeout="$POD_READY_TIMEOUT" >/dev/null 2>&1; then
  fail "Pod B Ready" "placeholder Pod B $POD_B did not become Ready in $POD_READY_TIMEOUT"
  echo "════════ Summary: $PASS passed, $FAIL failed ════════"
  exit 1
fi
B_UID=$(k get pod "$POD_B" -o jsonpath='{.metadata.uid}' 2>/dev/null)
# Bounded signature-keyed retry (HARDEN, ka-0000007234 'Pod-B-netns-resolve'
# signature). Right after Pod B reports Ready the runsc sandbox sentry can be
# briefly unresolvable (ctr/runsc state still registering), so resolve_b returns
# empty on the first call yet succeeds a few seconds later. Retry the resolve a
# bounded number of times; a Pod B whose netns never resolves (a real fault)
# still exhausts the budget and HARD-FAILS.
resolve_pod_b() {
  read -r B_PID B_NETNS <<<"$(resolve_b "$B_UID")"
  [ -n "${B_PID:-}" ]
}
if ! gvisor_retry_step Pod-B-netns-resolve 4 3 resolve_pod_b; then
  fail "resolve Pod B netns" "could not resolve sandbox sentry pid/netns for $POD_B (uid=$B_UID) — $GVISOR_STEP_DIAG"
  echo "════════ Summary: $PASS passed, $FAIL failed ════════"
  exit 1
fi
THAW_OUT=$(node_exec "$NODE_CALL_TIMEOUT" \
  "B_NETNS='$B_NETNS' B_SENTRY_PID='$B_PID' /var/tmp/single-container-thaw-into-b-node.sh 2>&1")
THAW_MS=$(printf '%s\n' "$THAW_OUT" | sed -n 's/.*THAW_MS=\([0-9][0-9]*\).*/\1/p' | head -1)
if grep -q 'PROVEN\|netns inode' <<<"$THAW_OUT"; then
  pass "agent THAWED into live Pod B netns (restore≈${THAW_MS:-?}ms; restored task shares B's kubelet-owned netns)"
else
  fail "THAW into Pod B" "restore did not prove netns graft: $(printf '%s\n' "$THAW_OUT" | tail -6)"
fi

# Assert Pod B is ALIVE right after the restore — the precondition the
# ka-0000007916 symptom holds (Pod B alive the full window). The whole point is
# that the ack/release below happens WITH Pod B alive, not after a teardown.
B_PHASE_PRE=$(k get pod "$POD_B" -o jsonpath='{.status.phase}' 2>/dev/null)
if [ "$B_PHASE_PRE" = "Running" ]; then
  pass "Pod B ALIVE post-restore (phase=Running) — rehydrate-ack runs on a live netns (the ka-0000007916 scenario)"
else
  fail "Pod B alive post-restore" "Pod B phase='$B_PHASE_PRE' (expected Running); cannot assert ack/release on a live netns"
fi

# ── 5. RELEASE: real WaitForAcks gate releases within deadline ──────
# Run cmd/thawepoch-probe on the node FS: both consumers arm a REAL Watcher,
# the controller writes the marker, both watchers ack the current epoch, and the
# REAL thawepoch.WaitForAcks RELEASES within deadline. Assertions (a)+(b)+(c).
blue "[5/6] RELEASE: real thawepoch.WaitForAcks gate over coop+gb acks (deadline ${REHYDRATE_DEADLINE_MS}ms)"
REL_OUT=$(node_exec "$NODE_CALL_TIMEOUT" \
  "/var/tmp/thawepoch-probe -mode=release -state-dir='$NODE_STATE_DIR' -deadline-ms=$REHYDRATE_DEADLINE_MS -pod-uid='$B_UID' 2>&1")
REL_LINE=$(printf '%s\n' "$REL_OUT" | grep '^THAWEPOCH_PROBE' | head -1)
dim "    $REL_LINE"
REL_RELEASED=$(printf '%s' "$REL_LINE" | sed -n 's/.*released=\([a-z]*\).*/\1/p')
REL_ELAPSED=$(printf '%s' "$REL_LINE" | sed -n 's/.*elapsed_ms=\([0-9]*\).*/\1/p')
REL_EPOCH=$(printf '%s' "$REL_LINE" | sed -n 's/.*epoch=\([0-9]*\).*/\1/p')

if [ "$REL_RELEASED" = "true" ]; then
  pass "(a)+(b) both consumers (coop, gb) acked epoch ${REL_EPOCH:-?}; WaitForAcks RELEASED the agent"
else
  fail "rehydrate-ack RELEASE" "gate did NOT release (released='$REL_RELEASED'): $(printf '%s\n' "$REL_OUT" | tail -4)"
fi
# (c) within deadline — released can only be true within deadline, but assert
# the elapsed number explicitly so a stale-state regression is caught.
if [ -n "$REL_ELAPSED" ] && [ "$REL_ELAPSED" -lt "$REHYDRATE_DEADLINE_MS" ] 2>/dev/null; then
  pass "(c) RELEASE within deadline: ${REL_ELAPSED}ms < ${REHYDRATE_DEADLINE_MS}ms (not fail-closing the full window)"
else
  fail "RELEASE within deadline" "elapsed_ms='${REL_ELAPSED:-NA}' not < ${REHYDRATE_DEADLINE_MS}ms — the ka-0000007916 fail-close symptom"
fi
# Confirm the armed sentinels + ack files actually exist on the substrate FS
# (thawepoch.ArmedPath / AckPath), per the task's "assert against ArmedPath/Ack
# files on the substrate FS" scope.
FS_CHECK=$(node_exec 30 "
  ok=1
  for c in coop gb; do
    [ -f \"$NODE_STATE_DIR/thaw-epoch.\$c.armed\" ] || ok=0
    [ -f \"$NODE_STATE_DIR/thaw-epoch.\$c.done\" ]  || ok=0
  done
  [ -f '$NODE_STATE_DIR/thaw-epoch.json' ] || ok=0
  echo FS_OK=\$ok" | tr -d '\r' | sed -n 's/.*FS_OK=//p')
if [ "$FS_CHECK" = "1" ]; then
  pass "ArmedPath + Ack + marker files present on substrate FS ($NODE_STATE_DIR: *.armed, *.done, thaw-epoch.json)"
else
  fail "thaw-state FS artifacts" "expected coop/gb .armed + .done + thaw-epoch.json under $NODE_STATE_DIR on the node"
fi

# Negative control — coop silent → gate HOLDS. Proves the gate genuinely
# fail-closes (the ka-0000007916 shape) rather than always passing. Separate
# state dir so it doesn't see the release-mode acks.
blue "    negative control: coop silent → WaitForAcks must HOLD (fail-close)"
FC_OUT=$(node_exec 60 \
  "/var/tmp/thawepoch-probe -mode=failclose -state-dir='${NODE_STATE_DIR}-fc' -pod-uid='$B_UID' 2>&1")
FC_LINE=$(printf '%s\n' "$FC_OUT" | grep '^THAWEPOCH_PROBE' | head -1)
dim "    $FC_LINE"
FC_RELEASED=$(printf '%s' "$FC_LINE" | sed -n 's/.*released=\([a-z]*\).*/\1/p')
FC_FAILED=$(printf '%s' "$FC_LINE" | sed -n 's/.*failed=\[\([^]]*\)\].*/\1/p')
if [ "$FC_RELEASED" = "false" ] && [ "$FC_FAILED" = "coop" ]; then
  pass "negative control: coop never acked → gate HELD (released=false, failed=[coop]) — fail-close proven"
else
  fail "fail-close negative control" "released='$FC_RELEASED' failed='[$FC_FAILED]' (expected released=false, failed=[coop])"
fi

# ── 6. Pod B still alive after release — the ka-0000007916 distinction ─
blue "[6/6] Pod B liveness across the gate window"
B_PHASE_POST=$(k get pod "$POD_B" -o jsonpath='{.status.phase}' 2>/dev/null)
if [ "$B_PHASE_POST" = "Running" ]; then
  pass "Pod B STILL Running after RELEASE — ack/release completed WITH Pod B alive (not a fail-close teardown)"
else
  fail "Pod B alive across gate" "Pod B phase='$B_PHASE_POST' after release (expected Running)"
fi

# ── Summary ─────────────────────────────────────────────────────────
echo ""
echo "════════════════════════════════════════════════════"
echo " gVisor rehydrate-ack RELEASE Summary: $PASS passed, $FAIL failed"
echo "   thaw≈${THAW_MS:-?}ms  release_elapsed=${REL_ELAPSED:-NA}ms  deadline=${REHYDRATE_DEADLINE_MS}ms  epoch=${REL_EPOCH:-?}"
echo "════════════════════════════════════════════════════"
if [ "$FAIL" -gt 0 ]; then
  for e in "${ERRORS[@]}"; do red "  - $e"; done
  exit 1
fi
green "All gVisor rehydrate-ack RELEASE assertions passed — coop+gb ack + WaitForAcks releases within deadline on a live Pod B."
exit 0
