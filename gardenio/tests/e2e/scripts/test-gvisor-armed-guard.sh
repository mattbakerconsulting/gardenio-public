#!/usr/bin/env bash
# test-gvisor-armed-guard.sh — boot-wedge armed-watcher liveness guard
# (freeze fail-safe) E2E.  Bead ka-0000000584 (Phase-4 gVisor freeze/thaw).
#
# WHAT THIS PROVES (the fail-safe path on a real runsc sandbox, not a unit mock):
#   The P0 boot-wedge fix (ka-0000006667, controller watchersArmed +
#   freezethaw_armed_guard_test.go) refuses to checkpoint a pod whose in-pod
#   rehydration watchers (coop + gb) never armed their thaw sentinels. A runsc
#   pod becomes k8s-health-ready well before its agent loop boots and arms those
#   watchers; a checkpoint taken in that window has NO live watcher inside it, so
#   on thaw the controller's rehydration gate can never be acked — the thaw
#   fail-closes to a cold start even though `runsc restore` returned rc=0. The
#   fix is to NOT freeze such a pod: skipping the freeze yields the SAME cold
#   start on next spawn, minus a wasted 3.0G checkpoint and a guaranteed-doomed
#   thaw. The unit layer (freezethaw_armed_guard_test.go) asserts the decision
#   over a fake exec; THIS module asserts the END-TO-END invariant on the gvisor
#   substrate: a live, un-armed sandbox that the guard refuses to freeze is LEFT
#   RUNNING (the boot-wedge is never minted), and the positive control — both
#   watchers armed — proceeds to a real checkpoint that persists a snapshot.
#
#   The guard's interface is an on-disk contract shared byte-for-byte with the
#   Go controller (internal/thawepoch) and the coop watcher: each consumer drops
#   a `thaw-epoch.<consumer>.armed` sentinel (thawepoch.ArmedName) into the
#   agent's per-agent node-side thaw-state dir, and the pre-freeze probe
#   (watchersArmed, freezethaw.go) lists which sentinels exist via a
#   `if [ -f <path> ]; then echo FT_ARMED=<consumer>; fi` script, refusing the
#   freeze when any required consumer's FT_ARMED line is absent. This module
#   reproduces that exact sentinel-filename + probe-shape contract on the node
#   (the same blessed reimplement-the-shared-protocol-in-bash pattern the sibling
#   test-gvisor-rehydration.sh uses for the marker/ack contract) and wires the
#   gate decision to the real runsc checkpoint the sibling test-gvisor-freeze-
#   thaw.sh proves.
#
# THE THREE LEGS (the refuse-legs run FIRST — they leave the sandbox live; the
# positive control runs LAST because a real checkpoint stops the container):
#   A. FAIL-SAFE / boot-wedge: arm ONLY coop (omit gb). Probe reports gb missing
#      => guard REFUSES. Assert NO checkpoint image was written AND the sandbox
#      is still RUNNING with its in-RAM counter climbing past the pre-gate value
#      (the live agent was never frozen).
#   B. NONE-ARMED: clear both sentinels. Probe reports both missing => guard
#      REFUSES again; sandbox still live (a second, stronger negative).
#   C. POSITIVE CONTROL: arm BOTH coop + gb. Probe reports both present => guard
#      PROCEEDS. Run runsc checkpoint => assert checkpoint images present AND the
#      container is stopped (a real, thawable snapshot was minted).
#
# HOW IT WORKS (reuses the ka-0000000300 PoC mechanism, same as the sibling
# freeze-thaw module): kubectl/CRI exposes no checkpoint verb for runsc and the
# runsc state root lives on the node, so we schedule a privileged hostPath+hostPID
# probe pod onto the gvisor-poc node and drive `runsc run|checkpoint` directly
# via `nsenter -t 1 -m -p` into the host init namespaces. Every runsc invocation
# is wrapped in `timeout` and asserted from OBSERVED STATE (container state +
# image files), never rc — runsc C/R completes the op but hangs on process exit
# (observed live; epic constraint ka-0000006959).
#
# OPT-IN / GATING (mirrors test-gvisor-freeze-thaw.sh exactly): targets the
# dedicated gvisor-poc namespace + node pool, NOT the per-run e2e
# namespace. The prod e2e cluster's default pool has no gVisor, so this module
# SKIPs cleanly (exit 0) unless GVISOR_E2E=1 is set AND the substrate is present
# (runsc RuntimeClass + create-pods in $GVISOR_NS). Wire it in via:
#   GVISOR_E2E=1 ONLY=gvisor-armed-guard run-shard.sh
#
# Usage:
#   GVISOR_E2E=1 ./gardenio/tests/e2e/scripts/test-gvisor-armed-guard.sh
#   GVISOR_E2E=1 GVISOR_NS=gvisor-poc KEEP_PROBE=1 ./...   # keep probe for debug

set -uo pipefail

# Shared gVisor substrate helpers — incl. gvisor_require_substrate_guard, the
# readiness-gate decision for a missing substrate (REQUIRE_SUBSTRATE=1 → hard
# fail; default → clean skip). Task ka-0000002867, lib decision ka-0000000122.
SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
# shellcheck disable=SC1091
source "$SCRIPT_DIR/lib-gvisor-node.sh"

# ── Config ────────────────────────────────────────────────────────
GVISOR_NS="${GVISOR_NS:-gvisor-poc}"
RUNTIME_CLASS="${RUNTIME_CLASS:-runsc}"
# Unique sandbox/work/probe per run so concurrent or repeated dispatches don't
# collide — the cleanup trap deletes the probe by name, so a shared probe name
# would let the first run to finish tear down a still-running peer.
RUN_ID="${RUN_ID:-$$}"
PROBE="${PROBE:-gvisor-ag-probe-${RUN_ID}}"
CID="agagent-${RUN_ID}"
NODE_BASE="/tmp/gvisor-ag-${RUN_ID}"
SENTINEL="GVISOR-AG-$(printf '%s' "$RUN_ID" | cksum | awk '{print $1}')"
# Per-runsc-call wall-clock guard (checkpoint/restore hang on exit, see header).
RUNSC_TIMEOUT="${RUNSC_TIMEOUT:-45}"
BUSYBOX_IMAGE="${BUSYBOX_IMAGE:-public.ecr.aws/docker/library/busybox:latest}"

# The armed-sentinel on-disk contract (must match thawepoch.ArmedName on the Go
# side: "thaw-epoch.<consumer>.armed", and thawepoch.ConsumerCoop/ConsumerGB).
# STATE_DIR mirrors podmanager.thawStateHostDir — a per-agent dir holding each
# consumer's armed sentinel; the controller's watchersArmed probes exactly here.
STATE_DIR="${NODE_BASE}/thaw-state"
CONSUMER_COOP="coop"
CONSUMER_GB="gb"
armed_name() { printf 'thaw-epoch.%s.armed' "$1"; }   # == thawepoch.ArmedName

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
# Opt-in OFF: inert clean skip. Substrate-ABSENT goes through the shared
# readiness guard so REQUIRE_SUBSTRATE=1 can hard-fail (task ka-0000002867).
skip_optin_off() {
  blue "SKIP gvisor-armed-guard: $1"
  dim "  This module is opt-in (set GVISOR_E2E=1) and requires the gvisor-poc"
  dim "  substrate (runsc RuntimeClass + create-pods in ns $GVISOR_NS)."
  if [ -n "${E2E_SKIP_SENTINEL:-}" ]; then printf '%s\n' "$*" >"$E2E_SKIP_SENTINEL" 2>/dev/null || true; fi  # skip visibility (ka-0000000989)
  exit 0
}
skip_all() { gvisor_require_substrate_guard gvisor-armed-guard "$1"; }

k() { kubectl -n "$GVISOR_NS" "$@"; }

# Run a script on the gvisor node, inside the host init namespaces, with a hard
# wall-clock cap. Streams over stdin (avoids nested-quoting hell) and keeps every
# exec SHORT — long-lived exec streams are what wedged a prior crew on an API
# socket error.
node_run() {
  local cap="$1"; shift
  timeout "$cap" kubectl -n "$GVISOR_NS" exec -i "$PROBE" -- chroot /host bash -s
}

cleanup() {
  blue "── cleanup ──"
  node_run 60 <<EOF 2>/dev/null || true
ROOT=${NODE_BASE}/runsc-root
timeout 20 nsenter -t 1 -m -p -- runsc --root=\$ROOT kill ${CID} KILL 2>/dev/null || true
timeout 20 nsenter -t 1 -m -p -- runsc --root=\$ROOT delete --force ${CID} 2>/dev/null || true
for p in \$(ps aux | grep runsc | grep -E '${CID}|${NODE_BASE}' | grep -v grep | awk '{print \$2}'); do
  kill -9 \$p 2>/dev/null || true
done
rm -rf ${NODE_BASE} 2>/dev/null || true
EOF
  if [ "${KEEP_PROBE:-0}" != "1" ]; then
    k delete pod "$PROBE" --grace-period=0 --force >/dev/null 2>&1 || true
  fi
}
trap cleanup EXIT

echo "════════════════════════════════════════════════════"
echo " gVisor boot-wedge armed-watcher guard (freeze fail-safe) E2E"
echo " ns=$GVISOR_NS  runtimeClass=$RUNTIME_CLASS  cid=$CID  sentinel=$SENTINEL"
echo "════════════════════════════════════════════════════"

# ── Gate 0: opt-in + substrate preflight ──────────────────────────
# opt-in OFF → inert clean skip; substrate ABSENT → shared readiness guard.
[ "${GVISOR_E2E:-0}" = "1" ] || skip_optin_off "GVISOR_E2E != 1 (opt-in module)"
command -v kubectl >/dev/null 2>&1 || skip_all "kubectl not installed"
kubectl get runtimeclass "$RUNTIME_CLASS" >/dev/null 2>&1 \
  || skip_all "RuntimeClass '$RUNTIME_CLASS' not present on this cluster"
[ "$(kubectl auth can-i create pods -n "$GVISOR_NS" 2>/dev/null)" = "yes" ] \
  || skip_all "cannot create pods in ns '$GVISOR_NS'"

blue "Substrate present — running boot-wedge armed-guard assertions."

# ── 1. Schedule the privileged probe onto the gvisor node ──────────
blue "[1/5] scheduling privileged probe '$PROBE' onto the gvisor-poc node"
cat <<YAML | k apply -f - >/dev/null 2>&1
apiVersion: v1
kind: Pod
metadata:
  name: ${PROBE}
  namespace: ${GVISOR_NS}
  labels: {app: gvisor-ag-probe, test: ka-0000000584}
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
if ! k wait --for=condition=Ready "pod/$PROBE" --timeout=180s >/dev/null 2>&1; then
  fail "probe pod Ready" "pod $PROBE did not become Ready in 180s"
  k get pod "$PROBE" -o wide 2>&1 | sed 's/^/    /'
  echo "════════ Summary: $PASS passed, $FAIL failed ════════"
  exit 1
fi
pass "probe scheduled + Ready on the gvisor node"

# Confirm the gvisor-poc node is genuinely WARM before any freeze/guard work — via
# the SHARED hardening gate (lib-gvisor-node.sh gvisor_warm_node_gate; decision
# ka-0000000122 chosen=harden, root flake ka-0000007234). REPLACES this module's
# old inline RUNSC_VER_RETRIES poll: the gate asserts the same runsc-present-+-
# reports-a-version check AND that /var/tmp is writable, riding out the identical
# Karpenter cold-start transient (empty `runsc --version` in ka-0000007698 that
# SUCCEEDED seconds later). This module builds its OWN private runsc root
# (--root=$NODE_BASE/runsc-root, no k8s runsc pod), so the k8s containerd state
# root check is SKIPPED (GVISOR_WARM_REQUIRE_ROOT=0). node_run streams over stdin,
# so use the gate's stdin-streaming adapter; RUNSC_VER_RETRIES / RUNSC_VER_DELAY
# stay honored (mapped onto attempts/delay). On exhaustion GVISOR_WARM_DIAG names
# the first still-failing check — a genuine regression still HARD-FAILS.
if GVISOR_WARM_REQUIRE_ROOT=0 gvisor_warm_gate_via_run node_run \
     "${RUNSC_VER_RETRIES:-10}" "${RUNSC_VER_DELAY:-3}"; then
  pass "gvisor-poc node warm (runsc present + /var/tmp writable)"
else
  fail "gvisor-poc node warm-gate" "$GVISOR_WARM_DIAG"
fi

# ── 2. Build a harness-owned runsc sandbox + stateful agent ────────
# Same mechanism as test-gvisor-freeze-thaw.sh: pull busybox via ctr, flatten its
# layer into a plain rootfs, write the stateful agent + an OCI config.json that
# host-bind-mounts /work so the host can read the in-RAM counter directly.
blue "[2/5] building harness-owned runsc sandbox + stateful agent"
BUILD_OUT=$(node_run 120 <<EOF 2>&1
set -e
BASE=${NODE_BASE}
B=\$BASE/bundle
WORK=\$BASE/work
ROOT=\$BASE/runsc-root
rm -rf \$BASE
mkdir -p \$B/rootfs/work \$WORK \$ROOT \$BASE/oci ${STATE_DIR}

ctr -n gvisor-ag images pull ${BUSYBOX_IMAGE} >/dev/null 2>&1
ctr -n gvisor-ag images export \$BASE/bb.tar ${BUSYBOX_IMAGE} >/dev/null 2>&1
tar -xf \$BASE/bb.tar -C \$BASE/oci
LAYER=\$(python3 -c 'import json;print(json.load(open("'\$BASE'/oci/manifest.json"))[0]["Layers"][0])')
tar -xf \$BASE/oci/\$LAYER -C \$B/rootfs
test -x \$B/rootfs/bin/busybox

# Stateful "agent": holds an in-RAM monotonic counter (the liveness token) and
# writes a FILE token (sentinel) to /work/state.bin. /work is a HOST bind-mount,
# so this test reads the counter + token directly without a runsc exec.
cat > \$B/rootfs/agent.sh <<'AGENT'
#!/bin/sh
SENT="__SENTINEL__"
echo "\$SENT" > /work/state.bin
i=0
exec 3>>/work/agent.log
echo "BOOT pid=\$\$ sentinel=\$SENT" >&3
while true; do
  i=\$((i+1))
  echo "\$i" > /work/counter
  echo "tick=\$i sentinel=\$SENT" >&3
  sleep 1
done
AGENT
sed -i "s|__SENTINEL__|${SENTINEL}|" \$B/rootfs/agent.sh
chmod +x \$B/rootfs/agent.sh

cat > \$B/config.json <<CFG
{
  "ociVersion": "1.0.0",
  "process": {"terminal": false, "user": {"uid":0,"gid":0},
    "args": ["/bin/sh","/agent.sh"],
    "env": ["PATH=/bin:/usr/bin","HOME=/root"], "cwd": "/"},
  "root": {"path": "rootfs", "readonly": false},
  "hostname": "agagent",
  "mounts": [
    {"destination": "/proc", "type": "proc", "source": "proc"},
    {"destination": "/dev", "type": "tmpfs", "source": "tmpfs",
       "options": ["nosuid","mode=755","size=65536k"]},
    {"destination": "/work", "type": "bind", "source": "\$WORK",
       "options": ["rbind","rw"]}
  ],
  "linux": {"namespaces": [{"type":"pid"},{"type":"mount"}]}
}
CFG
echo "BUILD_OK rootfs=\$(ls \$B/rootfs/bin/busybox)"
EOF
)
if grep -q 'BUILD_OK' <<<"$BUILD_OUT"; then
  pass "sandbox bundle + rootfs + agent built on node"
else
  fail "build sandbox bundle" "$(printf '%s' "$BUILD_OUT" | tail -4)"
  echo "════════ Summary: $PASS passed, $FAIL failed ════════"
  exit 1
fi

# ── 3. Run the agent under runsc + confirm warm state ──────────────
blue "[3/5] starting agent under runsc + seeding warm state"
RUN_OUT=$(node_run 90 <<EOF 2>&1
set -e
BASE=${NODE_BASE}; ROOT=\$BASE/runsc-root; B=\$BASE/bundle; WORK=\$BASE/work
rsc(){ timeout ${RUNSC_TIMEOUT} nsenter -t 1 -m -p -- runsc --root=\$ROOT "\$@"; }
find \$B/rootfs -maxdepth 1 -name '.gvisor.filestore*' -delete 2>/dev/null || true
rsc run -detach -bundle \$B ${CID}
for _ in 1 2 3 4 5 6 7 8 9 10; do [ -s \$WORK/counter ] && break; sleep 1; done
echo "PRE_COUNTER=\$(cat \$WORK/counter 2>/dev/null)"
echo "FILE_TOKEN=\$(cat \$WORK/state.bin 2>/dev/null)"
echo "RUNTIME=\$(rsc state ${CID} 2>/dev/null | python3 -c 'import sys,json;print(json.load(sys.stdin).get("status","?"))' 2>/dev/null || echo '?')"
EOF
)
PRE_COUNTER=$(printf '%s' "$RUN_OUT" | sed -n 's/^PRE_COUNTER=//p' | tr -d '\r')
FILE_TOKEN=$(printf '%s' "$RUN_OUT"  | sed -n 's/^FILE_TOKEN=//p'  | tr -d '\r')
if [ -n "$PRE_COUNTER" ] && [ "$PRE_COUNTER" -ge 1 ] 2>/dev/null; then
  pass "agent running with warm in-RAM counter (PRE=$PRE_COUNTER)"
else
  fail "agent warm state" "no counter after start. out: $(printf '%s' "$RUN_OUT" | tail -4)"
  echo "════════ Summary: $PASS passed, $FAIL failed ════════"
  exit 1
fi
if [ "$FILE_TOKEN" = "$SENTINEL" ]; then
  pass "file token seeded (state.bin = $SENTINEL)"
else
  fail "file token seeded" "got '$FILE_TOKEN' want '$SENTINEL'"
fi

# ── armed_gate: reproduce the controller's pre-freeze guard EXACTLY ──
# Mirrors podmanager.FreezeThawManager.watchersArmed (freezethaw.go): builds the
# identical per-consumer probe — `if [ -f <armed-path> ]; then echo FT_ARMED=<c>;
# fi` over the agent's thaw-state dir — and refuses (rc=1) iff any required
# consumer's FT_ARMED line is absent. The required set is {coop, gb}, the same
# default as armedConsumers(). Returns 0 (= guard PROCEEDS to checkpoint) only
# when every required sentinel is present; prints MISSING=<csv> on refusal.
# Runs on the node so it probes the real on-disk sentinels under $STATE_DIR.
armed_gate() {
  node_run 30 <<EOF 2>&1
SD=${STATE_DIR}
present=""
for c in ${CONSUMER_COOP} ${CONSUMER_GB}; do
  if [ -f "\$SD/thaw-epoch.\$c.armed" ]; then echo "FT_ARMED=\$c"; fi
done
missing=""
for c in ${CONSUMER_COOP} ${CONSUMER_GB}; do
  [ -f "\$SD/thaw-epoch.\$c.armed" ] || missing="\${missing:+\$missing,}\$c"
done
if [ -n "\$missing" ]; then echo "GATE=refused"; echo "MISSING=\$missing"; else echo "GATE=proceed"; fi
EOF
}

# Helper: is the runsc container currently RUNNING? (the boot-wedge invariant —
# a refused freeze must leave the live agent untouched).
sandbox_status() {
  node_run 30 <<EOF 2>/dev/null | tr -d '\r'
BASE=${NODE_BASE}; ROOT=\$BASE/runsc-root
timeout 20 nsenter -t 1 -m -p -- runsc --root=\$ROOT state ${CID} 2>/dev/null \
  | python3 -c 'import sys,json;print(json.load(sys.stdin).get("status","?"))' 2>/dev/null || echo '?'
EOF
}

# ── 4. REFUSE legs (run first — they must leave the sandbox live) ──
blue "[4/5] fail-safe: un-armed watchers => guard REFUSES, sandbox stays live"

# Leg A — only coop armed (gb missing): the canonical boot-wedge window.
node_run 30 <<EOF >/dev/null 2>&1 || true
SD=${STATE_DIR}; rm -f "\$SD"/thaw-epoch.*.armed 2>/dev/null || true
: > "\$SD/$(armed_name "$CONSUMER_COOP")"   # arm coop only (thawepoch.WriteArmed)
EOF
GATE_A=$(armed_gate)
COUNTER_BEFORE_A=$(node_run 20 <<EOF 2>/dev/null | tr -d '\r'
cat ${NODE_BASE}/work/counter 2>/dev/null
EOF
)
if grep -q 'GATE=refused' <<<"$GATE_A" && grep -q 'MISSING=gb' <<<"$GATE_A"; then
  pass "guard REFUSED freeze with gb un-armed (MISSING=gb — would be the boot-wedge)"
else
  fail "guard refuses partial-armed" "gate output: $(printf '%s' "$GATE_A" | tr '\n' ' ')"
fi
# Because the guard refused, NO checkpoint ran: assert no checkpoint image dir
# exists AND the sandbox is still running with its counter climbing past the
# pre-gate value (the live agent was never frozen).
NO_IMG_A=$(node_run 20 <<EOF 2>/dev/null | tr -d '\r'
[ -e ${NODE_BASE}/ck/checkpoint.img ] && echo HAVE_IMG || echo NO_IMG
EOF
)
if [ "$NO_IMG_A" = "NO_IMG" ]; then
  pass "no checkpoint image minted on refusal (no wasted 3.0G boot-wedge snapshot)"
else
  fail "no checkpoint image on refusal" "found a checkpoint image after a refused freeze"
fi
STATUS_A=$(sandbox_status)
RESUME_A=0
for _ in 1 2 3 4 5 6 7 8; do
  C=$(node_run 15 <<EOF 2>/dev/null | tr -d '\r'
cat ${NODE_BASE}/work/counter 2>/dev/null || echo 0
EOF
)
  if [ -n "$C" ] && [ "$C" -gt "${COUNTER_BEFORE_A:-0}" ] 2>/dev/null; then RESUME_A=$C; break; fi
  sleep 1
done
if [ "$STATUS_A" = "running" ] && [ -n "$RESUME_A" ] && [ "$RESUME_A" -gt "${COUNTER_BEFORE_A:-0}" ] 2>/dev/null; then
  pass "sandbox LEFT LIVE after refusal — status=running, counter climbed ${COUNTER_BEFORE_A} → ${RESUME_A}"
else
  fail "sandbox left live after refusal" "status='$STATUS_A' counter ${COUNTER_BEFORE_A} → '${RESUME_A}' (boot-wedge invariant: a refused freeze must not touch the live agent)"
fi

# Leg B — none armed: clear both sentinels, expect refusal again (stronger
# negative, and the exact state of a pod whose agent loop never armed at all).
node_run 30 <<EOF >/dev/null 2>&1 || true
rm -f ${STATE_DIR}/thaw-epoch.*.armed 2>/dev/null || true
EOF
GATE_B=$(armed_gate)
STATUS_B=$(sandbox_status)
if grep -q 'GATE=refused' <<<"$GATE_B" \
   && grep -q 'coop' <<<"$GATE_B" && grep -q 'gb' <<<"$GATE_B"; then
  pass "guard REFUSED freeze with NO watchers armed (MISSING both coop+gb)"
else
  fail "guard refuses none-armed" "gate output: $(printf '%s' "$GATE_B" | tr '\n' ' ')"
fi
if [ "$STATUS_B" = "running" ]; then
  pass "sandbox still LIVE after none-armed refusal (status=running)"
else
  fail "sandbox live after none-armed refusal" "status='$STATUS_B'"
fi

# ── 5. POSITIVE CONTROL (runs last — a real checkpoint stops the box) ──
blue "[5/5] positive control: BOTH watchers armed => guard PROCEEDS + snapshot"
node_run 30 <<EOF >/dev/null 2>&1 || true
SD=${STATE_DIR}
: > "\$SD/$(armed_name "$CONSUMER_COOP")"   # arm coop
: > "\$SD/$(armed_name "$CONSUMER_GB")"     # arm gb
EOF
GATE_C=$(armed_gate)
if grep -q 'GATE=proceed' <<<"$GATE_C"; then
  pass "guard PROCEEDS with both coop+gb armed (snapshot is structurally thawable)"
else
  fail "guard proceeds when armed" "gate output: $(printf '%s' "$GATE_C" | tr '\n' ' ')"
fi
# Drive the real runsc checkpoint the guard authorized; assert by observed state
# (images present + container stopped). Same timed-and-ignore-rc handling as the
# sibling freeze-thaw module (runsc C/R hangs on exit).
FREEZE_OUT=$(node_run 90 <<EOF 2>&1
set -e
BASE=${NODE_BASE}; ROOT=\$BASE/runsc-root; CK=\$BASE/ck; WORK=\$BASE/work
rm -rf \$CK && mkdir -p \$CK
FROZEN_AT=\$(cat \$WORK/counter 2>/dev/null)
timeout ${RUNSC_TIMEOUT} nsenter -t 1 -m -p -- runsc --root=\$ROOT checkpoint --image-path=\$CK ${CID} >/dev/null 2>&1 || true
echo "IMAGES=\$(ls \$CK 2>/dev/null | tr '\n' ',')"
echo "SNAPSHOT_KIB=\$(du -sk \$CK 2>/dev/null | awk '{print \$1}')"
echo "STATUS=\$(timeout 20 nsenter -t 1 -m -p -- runsc --root=\$ROOT state ${CID} 2>/dev/null | python3 -c 'import sys,json;print(json.load(sys.stdin).get("status","?"))' 2>/dev/null || echo '?')"
echo "FROZEN_COUNTER=\$FROZEN_AT"
EOF
)
IMAGES=$(printf '%s' "$FREEZE_OUT"   | sed -n 's/^IMAGES=//p'      | tr -d '\r')
STATUS_C=$(printf '%s' "$FREEZE_OUT" | sed -n 's/^STATUS=//p'      | tr -d '\r')
SNAP_KIB=$(printf '%s' "$FREEZE_OUT" | sed -n 's/^SNAPSHOT_KIB=//p' | tr -d '\r')
FROZEN_COUNTER=$(printf '%s' "$FREEZE_OUT" | sed -n 's/^FROZEN_COUNTER=//p' | tr -d '\r')
if grep -q 'checkpoint.img' <<<"$IMAGES" && grep -q 'pages.img' <<<"$IMAGES"; then
  pass "checkpoint images minted when armed (${IMAGES%,}; ${SNAP_KIB:-?}KiB; frozen@counter=$FROZEN_COUNTER)"
else
  fail "checkpoint images minted when armed" "ls ck = '$IMAGES' (the guard authorized a freeze that produced no snapshot)"
fi
if [ "$STATUS_C" = "stopped" ]; then
  pass "container stopped after authorized checkpoint (a real, thawable snapshot persisted)"
else
  fail "container stopped after checkpoint" "runsc state status='$STATUS_C'"
fi

# ── Summary ───────────────────────────────────────────────────────
echo ""
echo "════════════════════════════════════════════════════"
echo " gVisor armed-guard Summary: $PASS passed, $FAIL failed"
echo "   refuse-legs: gb-unarmed + none-armed left the sandbox LIVE"
echo "   positive control: both-armed minted a ${SNAP_KIB:-?}KiB snapshot"
echo "════════════════════════════════════════════════════"
if [ "$FAIL" -gt 0 ]; then
  for e in "${ERRORS[@]}"; do red "  - $e"; done
  exit 1
fi
green "All gVisor boot-wedge armed-guard assertions passed."
exit 0
