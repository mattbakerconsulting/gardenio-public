#!/usr/bin/env bash
# test-gvisor-multicycle-refreeze.sh — multi-cycle freeze→thaw→re-freeze story.
# Bead ka-0000001878 (gVisor production-readiness epic ka-0000004667).
#
# THE DECISION THIS TEST DEFENDS (option b):
#   freezethaw_lifecycle.go documents SINGLE-CYCLE scope — one freeze→thaw per
#   agent; re-freezing a thawed Pod B is out of scope, left to the
#   snapshot-transport follow-up (criu-image-streamer). Production readiness does
#   NOT require multi-cycle re-freeze (a perf feature); it requires that a SECOND
#   freeze of an already-thawed agent is handled SAFELY and the agent degrades to
#   a clean cold start, never clobbering the live thawed pod's node-local bundle.
#
#   The controller mechanism (asserted by the Go unit tests
#   freezethaw_refreeze_guard_test.go): a verified thaw reaps the 3.0G image but
#   the snapshot RECORD survives (its bundle backs the live Pod B), so Thaw marks
#   it Consumed=true. Freeze REFUSES + DISCARDS a consumed snapshot
#   (ErrFreezeAlreadyThawed); HasSnapshot/SnapshotNode/SnapshotTopology/
#   WarmBuckets treat consumed as absent → the next spawn cold-starts directly.
#
# WHAT THIS E2E PROVES on the live gvisor-poc substrate (node-level runsc, the
# same harness mechanism as test-gvisor-freeze-thaw.sh):
#
#   1. SUBSTRATE SUPPORTS THE FULL CYCLE: a live runsc agent can be
#      checkpoint→restore→re-checkpoint→restore'd — i.e. a THAWED process is
#      itself re-checkpointable at the runsc layer. This is what makes option (a)
#      *possible in principle* and proves the substrate is not the blocker.
#
#   2. SINGLE-SHOT NODE-LOCAL HAZARD IS REAL: the second checkpoint, written to
#      the SAME node-local image path the controller uses
#      (/var/lib/gardenio/ckpt/<agent>), OVERWRITES the first — exactly why the
#      controller must NOT silently re-freeze a thawed agent over the live
#      bundle. We assert the overwrite is observable (the cycle-2 image differs
#      from cycle-1's), making the design rationale concrete on real hardware.
#
#   3. CLEAN DEGRADATION / FIDELITY: after each restore the agent RESUMES (the
#      in-RAM counter climbs past the freeze value), proving the cycle does not
#      corrupt state — so the controller's "degrade to cold start" is a safety
#      choice, not a forced one. A second restore from the re-checkpoint resumes
#      too, confirming a re-frozen image is itself thawable at the runsc layer.
#
# This is the live-substrate companion to the unit-level
# TestFreezeThaw_MultiCycle_DegradesToColdStart. The controller GUARD semantics
# (Consumed marker, ErrFreezeAlreadyThawed, predicate skipping) are asserted by
# the Go unit tests against the fake clientset — they do not need a live cluster;
# this script proves the runsc substrate underneath behaves as the design assumes.
#
# OPT-IN / GATING (identical to test-gvisor-freeze-thaw.sh):
#   Targets ns gvisor-poc + the gvisor node pool. SKIPs cleanly (exit 0) unless
#   GVISOR_E2E=1 AND the substrate is present (runsc RuntimeClass + create-pods
#   in $GVISOR_NS). Wire in via:
#     GVISOR_E2E=1 ONLY=gvisor-multicycle-refreeze run-shard.sh
#
# Usage:
#   GVISOR_E2E=1 ./gardenio/tests/e2e/scripts/test-gvisor-multicycle-refreeze.sh
#   GVISOR_E2E=1 GVISOR_NS=gvisor-poc KEEP_PROBE=1 ./...   # keep probe for debug

set -uo pipefail

# ── Config ────────────────────────────────────────────────────────
GVISOR_NS="${GVISOR_NS:-gvisor-poc}"
RUNTIME_CLASS="${RUNTIME_CLASS:-runsc}"
RUN_ID="${RUN_ID:-$$}"
PROBE="${PROBE:-gvisor-mcrf-probe-${RUN_ID}}"
CID="mcrfagent-${RUN_ID}"
NODE_BASE="/tmp/gvisor-mcrf-${RUN_ID}"
SENTINEL="GVISOR-MCRF-$(printf '%s' "$RUN_ID" | cksum | awk '{print $1}')"
RUNSC_TIMEOUT="${RUNSC_TIMEOUT:-45}"
BUSYBOX_IMAGE="${BUSYBOX_IMAGE:-public.ecr.aws/docker/library/busybox:latest}"

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
skip_all() {
  blue "SKIP gvisor-multicycle-refreeze: $1"
  dim "  This module is opt-in (set GVISOR_E2E=1) and requires the gvisor-poc"
  dim "  substrate (runsc RuntimeClass + create-pods in ns $GVISOR_NS)."
  if [ -n "${E2E_SKIP_SENTINEL:-}" ]; then printf '%s\n' "$*" >"$E2E_SKIP_SENTINEL" 2>/dev/null || true; fi  # skip visibility (ka-0000000989)
  exit 0
}

k() { kubectl -n "$GVISOR_NS" "$@"; }

# Run a script on the gvisor node, inside the host init namespaces, with a hard
# wall-clock cap. Streams over stdin (avoids nested-quoting hell).
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
echo " gVisor multi-cycle freeze→thaw→re-freeze story"
echo " ns=$GVISOR_NS  runtimeClass=$RUNTIME_CLASS  cid=$CID  sentinel=$SENTINEL"
echo "════════════════════════════════════════════════════"

# ── Gate 0: opt-in + substrate preflight (SKIP, never fail, when absent) ──
[ "${GVISOR_E2E:-0}" = "1" ] || skip_all "GVISOR_E2E != 1 (opt-in module)"
command -v kubectl >/dev/null 2>&1 || skip_all "kubectl not installed"
kubectl get runtimeclass "$RUNTIME_CLASS" >/dev/null 2>&1 \
  || skip_all "RuntimeClass '$RUNTIME_CLASS' not present on this cluster"
[ "$(kubectl auth can-i create pods -n "$GVISOR_NS" 2>/dev/null)" = "yes" ] \
  || skip_all "cannot create pods in ns '$GVISOR_NS'"

blue "Substrate present — running multi-cycle re-freeze assertions."

# ── 1. Schedule the privileged probe onto the gvisor node ──────────
blue "[1/7] scheduling privileged probe '$PROBE' onto the gvisor-poc node"
cat <<YAML | k apply -f - >/dev/null 2>&1
apiVersion: v1
kind: Pod
metadata:
  name: ${PROBE}
  namespace: ${GVISOR_NS}
  labels: {app: gvisor-mcrf-probe, test: ka-0000001878}
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

# Confirm runsc is on the node (bounded retry — same early-boot transient the
# sibling freeze-thaw module rides out, ka-0000007698).
RUNSC_VER_RETRIES="${RUNSC_VER_RETRIES:-10}"
RUNSC_VER_DELAY="${RUNSC_VER_DELAY:-3}"
RUNSC_VER=""
RUNSC_VER_ERR=""
for _i in $(seq 1 "$RUNSC_VER_RETRIES"); do
  RUNSC_VER=$(node_run 30 <<'EOF' 2>/dev/null | tr -d '\r'
nsenter -t 1 -m -p -- runsc --version 2>/dev/null | head -1
EOF
)
  grep -q 'runsc version' <<<"$RUNSC_VER" && break
  RUNSC_VER_ERR=$(node_run 30 <<'EOF' 2>&1 | tr -d '\r' | head -5
nsenter -t 1 -m -p -- runsc --version; echo "[rc=$?]"
EOF
)
  sleep "$RUNSC_VER_DELAY"
done
if grep -q 'runsc version' <<<"$RUNSC_VER"; then
  pass "runsc present on node ($RUNSC_VER)"
else
  fail "runsc present on node" "runsc --version returned: '${RUNSC_VER:-<empty>}' after ${RUNSC_VER_RETRIES} attempts; stderr/rc: ${RUNSC_VER_ERR:-<none>}"
  echo "════════ Summary: $PASS passed, $FAIL failed ════════"
  exit 1
fi

# ── 2. Build a harness-owned runsc sandbox (stateful counter agent) ─
# Same mechanism as test-gvisor-freeze-thaw.sh: busybox rootfs, an agent that
# holds an in-RAM monotonic counter (the in-memory token) flushed to
# /work/counter each tick, plus a FILE token in /work/state.bin. /work is a host
# bind-mount so the host (this test) reads tokens directly.
blue "[2/7] building harness-owned runsc sandbox + stateful agent"
BUILD_OUT=$(node_run 120 <<EOF 2>&1
set -e
BASE=${NODE_BASE}
B=\$BASE/bundle
WORK=\$BASE/work
ROOT=\$BASE/runsc-root
rm -rf \$BASE
mkdir -p \$B/rootfs/work \$WORK \$ROOT \$BASE/oci
ctr -n gvisor-mcrf images pull ${BUSYBOX_IMAGE} >/dev/null 2>&1
ctr -n gvisor-mcrf images export \$BASE/bb.tar ${BUSYBOX_IMAGE} >/dev/null 2>&1
tar -xf \$BASE/bb.tar -C \$BASE/oci
LAYER=\$(python3 -c 'import json;print(json.load(open("'\$BASE'/oci/manifest.json"))[0]["Layers"][0])')
tar -xf \$BASE/oci/\$LAYER -C \$B/rootfs
test -x \$B/rootfs/bin/busybox
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
  "hostname": "mcrfagent",
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
blue "[3/7] starting agent under runsc + seeding warm state"
RUN_OUT=$(node_run 90 <<EOF 2>&1
set -e
BASE=${NODE_BASE}; ROOT=\$BASE/runsc-root; B=\$BASE/bundle; WORK=\$BASE/work
rsc(){ timeout ${RUNSC_TIMEOUT} nsenter -t 1 -m -p -- runsc --root=\$ROOT "\$@"; }
find \$B/rootfs -maxdepth 1 -name '.gvisor.filestore*' -delete 2>/dev/null || true
rsc run -detach -bundle \$B ${CID}
for _ in 1 2 3 4 5 6 7 8 9 10; do [ -s \$WORK/counter ] && break; sleep 1; done
echo "PRE_COUNTER=\$(cat \$WORK/counter 2>/dev/null)"
echo "FILE_TOKEN=\$(cat \$WORK/state.bin 2>/dev/null)"
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
[ "$FILE_TOKEN" = "$SENTINEL" ] \
  && pass "file token seeded (state.bin = $SENTINEL)" \
  || fail "file token seeded" "got '$FILE_TOKEN' want '$SENTINEL'"

# ── 4. CYCLE 1 — checkpoint (freeze) to the CONTROLLER node-local path ──
# Write the cycle-1 image to /var/lib/gardenio/ckpt/<agent> — the exact
# controller-owned safe path (CheckpointHostBase, ka-0000003702) — so the
# single-shot node-local overwrite hazard is exercised against the real path the
# controller's re-freeze would reuse.
CKPT_PATH="/var/lib/gardenio/ckpt/${CID}"
blue "[4/7] cycle-1 checkpoint (freeze) → ${CKPT_PATH}"
FREEZE1_OUT=$(node_run 90 <<EOF 2>&1
set -e
BASE=${NODE_BASE}; ROOT=\$BASE/runsc-root; CK=${CKPT_PATH}; WORK=\$BASE/work
rm -rf \$CK && mkdir -p \$CK
timeout ${RUNSC_TIMEOUT} nsenter -t 1 -m -p -- runsc --root=\$ROOT checkpoint --image-path=\$CK ${CID} >/dev/null 2>&1 || true
echo "IMAGES=\$(ls \$CK 2>/dev/null | tr '\n' ',')"
echo "STATUS=\$(timeout 20 nsenter -t 1 -m -p -- runsc --root=\$ROOT state ${CID} 2>/dev/null | python3 -c 'import sys,json;print(json.load(sys.stdin).get("status","?"))' 2>/dev/null || echo '?')"
echo "FROZEN1_COUNTER=\$(cat \$WORK/counter 2>/dev/null)"
# Fingerprint the cycle-1 image so we can prove cycle-2 OVERWRITES it.
echo "CKPT1_SUM=\$(md5sum \$CK/checkpoint.img 2>/dev/null | awk '{print \$1}')"
EOF
)
IMAGES1=$(printf '%s' "$FREEZE1_OUT" | sed -n 's/^IMAGES=//p' | tr -d '\r')
STATUS1=$(printf '%s' "$FREEZE1_OUT" | sed -n 's/^STATUS=//p' | tr -d '\r')
FROZEN1_COUNTER=$(printf '%s' "$FREEZE1_OUT" | sed -n 's/^FROZEN1_COUNTER=//p' | tr -d '\r')
CKPT1_SUM=$(printf '%s' "$FREEZE1_OUT" | sed -n 's/^CKPT1_SUM=//p' | tr -d '\r')
if grep -q 'checkpoint.img' <<<"$IMAGES1"; then
  pass "cycle-1 checkpoint images written (${IMAGES1%,}; frozen at counter=$FROZEN1_COUNTER)"
else
  fail "cycle-1 checkpoint images written" "ls = '$IMAGES1'"
fi
[ "$STATUS1" = "stopped" ] \
  && pass "cycle-1 container stopped after checkpoint" \
  || fail "cycle-1 container stopped" "runsc state status='$STATUS1'"

# ── 5. CYCLE 1 — restore (thaw) + resume proof ─────────────────────
blue "[5/7] cycle-1 restore (thaw) + resume proof"
RESTORE1_OUT=$(node_run 90 <<EOF 2>&1
set -e
BASE=${NODE_BASE}; ROOT=\$BASE/runsc-root; B=\$BASE/bundle; CK=${CKPT_PATH}; WORK=\$BASE/work
timeout 20 nsenter -t 1 -m -p -- runsc --root=\$ROOT delete --force ${CID} 2>/dev/null || true
find \$B/rootfs -maxdepth 1 -name '.gvisor.filestore*' -delete 2>/dev/null || true
timeout ${RUNSC_TIMEOUT} nsenter -t 1 -m -p -- runsc --root=\$ROOT restore -detach -bundle \$B --image-path=\$CK ${CID} >/dev/null 2>&1 || true
RESUMED=0
for _ in 1 2 3 4 5 6 7 8; do
  C=\$(cat \$WORK/counter 2>/dev/null || echo 0)
  if [ "\$C" -gt "${FROZEN1_COUNTER:-0}" ] 2>/dev/null; then RESUMED=\$C; break; fi
  sleep 1
done
echo "RESUME1_COUNTER=\$RESUMED"
echo "FILE_TOKEN_AFTER=\$(cat \$WORK/state.bin 2>/dev/null)"
EOF
)
RESUME1_COUNTER=$(printf '%s' "$RESTORE1_OUT" | sed -n 's/^RESUME1_COUNTER=//p' | tr -d '\r')
FILE_TOKEN_AFTER1=$(printf '%s' "$RESTORE1_OUT" | sed -n 's/^FILE_TOKEN_AFTER=//p' | tr -d '\r')
[ "$FILE_TOKEN_AFTER1" = "$SENTINEL" ] \
  && pass "cycle-1 FILE token survived thaw" \
  || fail "cycle-1 FILE token survived thaw" "got '$FILE_TOKEN_AFTER1' want '$SENTINEL'"
if [ -n "$RESUME1_COUNTER" ] && [ "$RESUME1_COUNTER" -gt "${FROZEN1_COUNTER:-0}" ] 2>/dev/null; then
  pass "cycle-1 RESUMED — counter climbed $FROZEN1_COUNTER → $RESUME1_COUNTER past freeze"
else
  fail "cycle-1 RESUMED past freeze" "RESUME1='$RESUME1_COUNTER' not > FROZEN1='$FROZEN1_COUNTER'"
fi

# ── 6. CYCLE 2 — RE-FREEZE the thawed process to the SAME path ─────
# THE CRUX. Re-checkpoint the now-THAWED process to the identical node-local
# path, modeling the controller's freeze EXACTLY: buildFreezeScript runs
# `HMNT rm -rf "$IMG" && HMNT mkdir -p "$IMG"` before every checkpoint
# (ka-0000003702). We do the same `rm -rf $CK && mkdir -p $CK` here — without it
# runsc REFUSES to checkpoint into a non-empty image-path (it errors and leaves
# the container running, the substrate's own anti-overwrite guard). That detail
# is itself instructive: the only way a node-local re-freeze can proceed is to
# FIRST destroy the prior snapshot. So this step proves (a) the runsc substrate
# CAN re-checkpoint a thawed process — the substrate is not the blocker — and
# (b) doing so DESTROYS cycle-1's image at the controller's deterministic
# per-agent path (no second slot exists), which is precisely why the controller
# must NOT silently re-freeze a thawed agent over its live bundle. The guard
# (ErrFreezeAlreadyThawed + Consumed marker) is what makes this safe by refusing.
blue "[6/7] cycle-2 RE-FREEZE the thawed process → same ${CKPT_PATH} (clear-then-checkpoint, as the controller does)"
FREEZE2_OUT=$(node_run 90 <<EOF 2>&1
set -e
BASE=${NODE_BASE}; ROOT=\$BASE/runsc-root; CK=${CKPT_PATH}; WORK=\$BASE/work
# Clear the path first — EXACTLY what the controller's buildFreezeScript does
# (rm -rf "\$IMG" && mkdir -p "\$IMG"). This destroys cycle-1's image: the
# node-local single-shot hazard, made concrete.
rm -rf \$CK && mkdir -p \$CK
timeout ${RUNSC_TIMEOUT} nsenter -t 1 -m -p -- runsc --root=\$ROOT checkpoint --image-path=\$CK ${CID} >/dev/null 2>&1 || true
echo "IMAGES2=\$(ls \$CK 2>/dev/null | tr '\n' ',')"
echo "STATUS2=\$(timeout 20 nsenter -t 1 -m -p -- runsc --root=\$ROOT state ${CID} 2>/dev/null | python3 -c 'import sys,json;print(json.load(sys.stdin).get("status","?"))' 2>/dev/null || echo '?')"
echo "FROZEN2_COUNTER=\$(cat \$WORK/counter 2>/dev/null)"
echo "CKPT2_SUM=\$(md5sum \$CK/checkpoint.img 2>/dev/null | awk '{print \$1}')"
EOF
)
IMAGES2=$(printf '%s' "$FREEZE2_OUT" | sed -n 's/^IMAGES2=//p' | tr -d '\r')
STATUS2=$(printf '%s' "$FREEZE2_OUT" | sed -n 's/^STATUS2=//p' | tr -d '\r')
FROZEN2_COUNTER=$(printf '%s' "$FREEZE2_OUT" | sed -n 's/^FROZEN2_COUNTER=//p' | tr -d '\r')
CKPT2_SUM=$(printf '%s' "$FREEZE2_OUT" | sed -n 's/^CKPT2_SUM=//p' | tr -d '\r')

# Substrate supports re-checkpoint of a thawed process.
if grep -q 'checkpoint.img' <<<"$IMAGES2" && [ "$STATUS2" = "stopped" ]; then
  pass "cycle-2 RE-FREEZE succeeded at runsc layer (substrate supports re-checkpoint; frozen at counter=$FROZEN2_COUNTER)"
else
  fail "cycle-2 re-freeze at runsc layer" "images='$IMAGES2' status='$STATUS2'"
fi
# The thawed counter advanced past cycle-1's freeze before re-freezing — i.e. it
# is genuinely the resumed process being re-checkpointed, not a stale image.
if [ -n "$FROZEN2_COUNTER" ] && [ "$FROZEN2_COUNTER" -gt "${FROZEN1_COUNTER:-0}" ] 2>/dev/null; then
  pass "cycle-2 re-froze the RESUMED process (counter $FROZEN1_COUNTER → $FROZEN2_COUNTER advanced before re-freeze)"
else
  fail "cycle-2 re-froze the resumed process" "FROZEN2='$FROZEN2_COUNTER' not > FROZEN1='$FROZEN1_COUNTER'"
fi
# OVERWRITE HAZARD: the node-local single-shot path now holds a DIFFERENT image
# than cycle-1 — the first snapshot is gone. This is the concrete reason the
# controller degrades to cold start instead of node-local re-freeze.
if [ -n "$CKPT1_SUM" ] && [ -n "$CKPT2_SUM" ] && [ "$CKPT1_SUM" != "$CKPT2_SUM" ]; then
  pass "node-local single-shot OVERWRITE confirmed (cycle-1 image ${CKPT1_SUM:0:8}… replaced by ${CKPT2_SUM:0:8}…) — why the controller must not silently re-freeze"
elif [ -z "$CKPT1_SUM" ] || [ -z "$CKPT2_SUM" ]; then
  dim "    image fingerprints unavailable (md5sum absent?) — overwrite assertion skipped"
else
  fail "node-local overwrite confirmed" "cycle-1 and cycle-2 checkpoint.img have the SAME md5 ($CKPT1_SUM) — expected an overwrite to a new image"
fi

# ── 7. CYCLE 2 — restore from the re-freeze + resume proof ─────────
# A re-frozen image is itself thawable: restore from cycle-2's checkpoint and
# confirm the process resumes again (counter climbs past the cycle-2 freeze).
# This proves fidelity is preserved across the full cycle — the controller's
# "cold start" is a deliberate single-shot safety choice, not forced by data loss.
blue "[7/7] cycle-2 restore (thaw from re-freeze) + resume proof"
RESTORE2_OUT=$(node_run 90 <<EOF 2>&1
set -e
BASE=${NODE_BASE}; ROOT=\$BASE/runsc-root; B=\$BASE/bundle; CK=${CKPT_PATH}; WORK=\$BASE/work
timeout 20 nsenter -t 1 -m -p -- runsc --root=\$ROOT delete --force ${CID} 2>/dev/null || true
find \$B/rootfs -maxdepth 1 -name '.gvisor.filestore*' -delete 2>/dev/null || true
timeout ${RUNSC_TIMEOUT} nsenter -t 1 -m -p -- runsc --root=\$ROOT restore -detach -bundle \$B --image-path=\$CK ${CID} >/dev/null 2>&1 || true
RESUMED=0
for _ in 1 2 3 4 5 6 7 8; do
  C=\$(cat \$WORK/counter 2>/dev/null || echo 0)
  if [ "\$C" -gt "${FROZEN2_COUNTER:-0}" ] 2>/dev/null; then RESUMED=\$C; break; fi
  sleep 1
done
echo "RESUME2_COUNTER=\$RESUMED"
echo "FILE_TOKEN_AFTER2=\$(cat \$WORK/state.bin 2>/dev/null)"
EOF
)
RESUME2_COUNTER=$(printf '%s' "$RESTORE2_OUT" | sed -n 's/^RESUME2_COUNTER=//p' | tr -d '\r')
FILE_TOKEN_AFTER2=$(printf '%s' "$RESTORE2_OUT" | sed -n 's/^FILE_TOKEN_AFTER2=//p' | tr -d '\r')
[ "$FILE_TOKEN_AFTER2" = "$SENTINEL" ] \
  && pass "cycle-2 FILE token survived re-freeze→thaw" \
  || fail "cycle-2 FILE token survived re-freeze→thaw" "got '$FILE_TOKEN_AFTER2' want '$SENTINEL'"
if [ -n "$RESUME2_COUNTER" ] && [ "$RESUME2_COUNTER" -gt "${FROZEN2_COUNTER:-0}" ] 2>/dev/null; then
  pass "cycle-2 RESUMED from re-freeze — counter climbed $FROZEN2_COUNTER → $RESUME2_COUNTER (re-frozen image is thawable)"
else
  fail "cycle-2 RESUMED from re-freeze" "RESUME2='$RESUME2_COUNTER' not > FROZEN2='$FROZEN2_COUNTER'"
fi

# Reap the controller-path image we wrote (it lives outside the harness NODE_BASE
# the cleanup trap removes).
node_run 30 <<EOF >/dev/null 2>&1 || true
rm -rf ${CKPT_PATH}
EOF

# ── Summary ───────────────────────────────────────────────────────
echo ""
echo "════════════════════════════════════════════════════"
echo " gVisor multi-cycle re-freeze Summary: $PASS passed, $FAIL failed"
echo "   counter: PRE=$PRE_COUNTER  FROZEN1=$FROZEN1_COUNTER  RESUME1=$RESUME1_COUNTER"
echo "            FROZEN2=$FROZEN2_COUNTER  RESUME2=$RESUME2_COUNTER"
echo "   image:   cycle-1 md5=${CKPT1_SUM:0:8}…  cycle-2 md5=${CKPT2_SUM:0:8}… (overwrite)"
echo ""
echo " DECISION (b): substrate SUPPORTS multi-cycle, but the controller is"
echo " single-shot node-local — a thawed agent's snapshot is marked Consumed,"
echo " Freeze refuses ErrFreezeAlreadyThawed, and the agent degrades to a clean"
echo " cold start (guard semantics asserted by freezethaw_refreeze_guard_test.go)."
echo "════════════════════════════════════════════════════"
if [ "$FAIL" -gt 0 ]; then
  for e in "${ERRORS[@]}"; do red "  - $e"; done
  exit 1
fi
green "All gVisor multi-cycle re-freeze assertions passed."
exit 0
