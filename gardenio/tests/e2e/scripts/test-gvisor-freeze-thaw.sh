#!/usr/bin/env bash
# test-gvisor-freeze-thaw.sh — automated runsc checkpoint/restore (freeze/thaw)
# state-fidelity E2E test.  Bead ka-0000001895 (Phase 2 epic ka-0000002167).
#
# WHAT THIS PROVES (automatically, not by manual demo):
#   1. A live "agent" container running under the runsc RuntimeClass on the
#      gvisor-poc node pool can be `runsc checkpoint`ed (frozen) and
#      `runsc restore`d (thawed) with full state intact.
#   2. A verifiable state TOKEN survives the freeze/thaw:
#        - a FILE token  (sentinel written to /work/state.bin), and
#        - an IN-MEMORY token (a monotonic counter held only in the agent's
#          RAM, flushed to /work/counter each tick).
#   3. The agent RESUMES after thaw: the in-RAM counter keeps climbing PAST
#      the value it held at freeze time — i.e. the restored process is the
#      same live process with its memory, not a cold restart from zero.
#   4. FAILURE PATH: thawing a runsc checkpoint onto a runtime WITHOUT runsc
#      (the node's `runc`) fails with a clear non-zero error and starts no
#      container — a corrupt restore must surface loudly, never silently.
#
# HOW IT WORKS (reuses the ka-0000000300 PoC harness mechanism, MR !3464):
#   kubectl/CRI exposes no checkpoint verb for runsc and the runsc state root
#   lives on the node, so we schedule a privileged hostPath+hostPID "probe"
#   pod onto the gvisor-poc node (nodeSelector gvisor=enabled + matching
#   toleration), then drive `runsc run|checkpoint|restore` directly against a
#   harness-owned sandbox via `nsenter -t 1 -m -p` into the host init
#   namespaces (the gofer fork + netns open require host init context).
#
#   Unlike the PoC's kubelet-managed agentsim pod — whose cold-restore process
#   is short-lived because kubelet GCs the pod's emptyDir bind source (PoC
#   report §5) — this test runs the agent as a HARNESS-OWNED runsc container
#   with its own bundle, runsc-root and a HOST bind-mount for /work. Nothing
#   is kubelet-managed, so the restore is deterministic and the state token is
#   readable straight from the host (no fragile `runsc exec` round-trip).
#
#   runsc QUIRK (observed live on release-20260525.0): `runsc checkpoint` and
#   `runsc restore` COMPLETE the operation but then hang on process EXIT. So
#   every runsc invocation is wrapped in `timeout`, its return code is ignored,
#   and success is asserted from OBSERVED STATE (container stopped + image
#   files present after freeze; counter climbing after thaw) rather than rc.
#
# OPT-IN / GATING:
#   This module targets the dedicated `gvisor-poc` namespace + node pool, NOT
#   the per-run e2e namespace the rest of the suite runs against.
#   The prod e2e cluster's default pool has no gVisor, so this module is
#   OPT-IN: it SKIPs cleanly (exit 0) unless GVISOR_E2E=1 is set AND the
#   substrate is present (runsc RuntimeClass + create-pods in $GVISOR_NS).
#   Wire it in via:  GVISOR_E2E=1 ONLY=gvisor-freeze-thaw run-shard.sh
#
# Prerequisites (all satisfied by escalation ka-0000004674):
#   - kubectl context on america-e2e-eks
#   - ns gvisor-poc (PSA=privileged), RuntimeClass runsc, NodePool gvisor-poc
#   - SA gardenio-agent: create pods + pods/exec in gvisor-poc
#
# Usage:
#   GVISOR_E2E=1 ./gardenio/tests/e2e/scripts/test-gvisor-freeze-thaw.sh
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
PROBE="${PROBE:-gvisor-ft-probe-${RUN_ID}}"
CID="ftagent-${RUN_ID}"
NODE_BASE="/tmp/gvisor-ft-${RUN_ID}"
SENTINEL="GVISOR-FT-$(printf '%s' "$RUN_ID" | cksum | awk '{print $1}')"
# Per-runsc-call wall-clock guard (checkpoint/restore hang on exit, see header).
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
# Opt-in OFF (GVISOR_E2E unset): inert clean skip, always exit 0 — this module
# is simply not selected. Substrate-ABSENT (opt-in on, no node pool) goes through
# gvisor_require_substrate_guard instead, so the readiness gate (REQUIRE_SUBSTRATE=1)
# can hard-fail rather than silently pass.
skip_optin_off() {
  blue "SKIP gvisor-freeze-thaw: $1"
  dim "  This module is opt-in (set GVISOR_E2E=1) and requires the gvisor-poc"
  dim "  substrate (runsc RuntimeClass + create-pods in ns $GVISOR_NS)."
  if [ -n "${E2E_SKIP_SENTINEL:-}" ]; then printf '%s\n' "$*" >"$E2E_SKIP_SENTINEL" 2>/dev/null || true; fi  # skip visibility (ka-0000000989)
  exit 0
}
skip_all() { gvisor_require_substrate_guard gvisor-freeze-thaw "$1"; }

k() { kubectl -n "$GVISOR_NS" "$@"; }

# Run a script on the gvisor node, inside the host init namespaces, with a
# hard wall-clock cap. Streams the script over stdin (avoids nested-quoting
# hell) and keeps every exec SHORT — long-lived exec streams are what wedged
# the prior crew on an API socket error.
node_run() {
  local cap="$1"; shift
  timeout "$cap" kubectl -n "$GVISOR_NS" exec -i "$PROBE" -- chroot /host bash -s
}

cleanup() {
  # Best-effort teardown of the sandbox + probe. Never fails the test.
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
echo " gVisor runsc freeze/thaw state-fidelity E2E"
echo " ns=$GVISOR_NS  runtimeClass=$RUNTIME_CLASS  cid=$CID  sentinel=$SENTINEL"
echo "════════════════════════════════════════════════════"

# ── Gate 0: opt-in + substrate preflight ──────────────────────────
# opt-in OFF → inert clean skip; substrate ABSENT → skip_all → the shared
# readiness guard (hard-fails under REQUIRE_SUBSTRATE=1, clean-skips otherwise).
[ "${GVISOR_E2E:-0}" = "1" ] || skip_optin_off "GVISOR_E2E != 1 (opt-in module)"
command -v kubectl >/dev/null 2>&1 || skip_all "kubectl not installed"
kubectl get runtimeclass "$RUNTIME_CLASS" >/dev/null 2>&1 \
  || skip_all "RuntimeClass '$RUNTIME_CLASS' not present on this cluster"
[ "$(kubectl auth can-i create pods -n "$GVISOR_NS" 2>/dev/null)" = "yes" ] \
  || skip_all "cannot create pods in ns '$GVISOR_NS'"

blue "Substrate present — running freeze/thaw assertions."

# ── 1. Schedule the privileged probe onto the gvisor node ──────────
blue "[1/6] scheduling privileged probe '$PROBE' onto the gvisor-poc node"
cat <<YAML | k apply -f - >/dev/null 2>&1
apiVersion: v1
kind: Pod
metadata:
  name: ${PROBE}
  namespace: ${GVISOR_NS}
  labels: {app: gvisor-ft-probe, test: ka-0000001895}
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

# Confirm the gvisor-poc node is genuinely WARM before any freeze/thaw work — via
# the SHARED hardening gate (lib-gvisor-node.sh gvisor_warm_node_gate; decision
# ka-0000000122 chosen=harden, root flake ka-0000007234). This REPLACES this
# module's old inline RUNSC_VER_RETRIES poll: the gate asserts the same thing —
# runsc present + reports a version — but ALSO that /var/tmp is writable (this
# module base64-ships its sandbox there), and it rides out the identical
# Karpenter cold-start transient (the .22-vs-.21 empty-`runsc --version` in
# ka-0000007698, where the same invocation SUCCEEDED seconds later).
#
# This module builds its OWN private runsc root (--root=$NODE_BASE/runsc-root, no
# k8s runsc pod), so the k8s containerd state root check is SKIPPED
# (GVISOR_WARM_REQUIRE_ROOT=0) — asserting /run/containerd/runsc/k8s.io here would
# false-negative on a fresh dedicated node. node_run streams its script over
# stdin, so use the gate's stdin-streaming adapter. The RUNSC_VER_RETRIES /
# RUNSC_VER_DELAY knobs remain honored (mapped onto the gate's attempts/delay) for
# backward compatibility. On exhaustion GVISOR_WARM_DIAG carries the first
# still-failing check (a genuine regression still HARD-FAILS, never a silent pass).
if GVISOR_WARM_REQUIRE_ROOT=0 gvisor_warm_gate_via_run node_run \
     "${RUNSC_VER_RETRIES:-10}" "${RUNSC_VER_DELAY:-3}"; then
  pass "gvisor-poc node warm (runsc present + /var/tmp writable)"
else
  fail "gvisor-poc node warm-gate" "$GVISOR_WARM_DIAG"
fi

# ── 2. Build a harness-owned runsc sandbox (own root/bundle/rootfs) ─
# Pull busybox via ctr, flatten its single layer into a plain rootfs, write
# the stateful agent + an OCI config.json that host-bind-mounts /work.
blue "[2/6] building harness-owned runsc sandbox + stateful agent"
BUILD_OUT=$(node_run 120 <<EOF 2>&1
set -e
BASE=${NODE_BASE}
B=\$BASE/bundle
WORK=\$BASE/work
ROOT=\$BASE/runsc-root
rm -rf \$BASE
mkdir -p \$B/rootfs/work \$WORK \$ROOT \$BASE/oci

# Pull + export busybox, extract its layer into a flat rootfs.
ctr -n gvisor-ft images pull ${BUSYBOX_IMAGE} >/dev/null 2>&1
ctr -n gvisor-ft images export \$BASE/bb.tar ${BUSYBOX_IMAGE} >/dev/null 2>&1
tar -xf \$BASE/bb.tar -C \$BASE/oci
LAYER=\$(python3 -c 'import json;print(json.load(open("'\$BASE'/oci/manifest.json"))[0]["Layers"][0])')
tar -xf \$BASE/oci/\$LAYER -C \$B/rootfs
test -x \$B/rootfs/bin/busybox

# Stateful "agent": holds an in-RAM monotonic counter (the IN-MEMORY token),
# an open append-log FD, and writes a FILE token (sentinel) to /work/state.bin.
# /work is a HOST bind-mount, so the host (and thus this test) can read the
# token + counter directly without a runsc exec.
cat > \$B/rootfs/agent.sh <<'AGENT'
#!/bin/sh
SENT="__SENTINEL__"
echo "\$SENT" > /work/state.bin
i=0
exec 3>>/work/agent.log          # held append-log FD
echo "BOOT pid=\$\$ sentinel=\$SENT" >&3
while true; do
  i=\$((i+1))                     # in-RAM counter (the in-memory token)
  echo "\$i" > /work/counter      # flush to host-visible file each tick
  echo "tick=\$i sentinel=\$SENT" >&3
  sleep 1
done
AGENT
sed -i "s|__SENTINEL__|${SENTINEL}|" \$B/rootfs/agent.sh
chmod +x \$B/rootfs/agent.sh

# Minimal OCI runtime spec with a host bind-mount for /work.
cat > \$B/config.json <<CFG
{
  "ociVersion": "1.0.0",
  "process": {"terminal": false, "user": {"uid":0,"gid":0},
    "args": ["/bin/sh","/agent.sh"],
    "env": ["PATH=/bin:/usr/bin","HOME=/root"], "cwd": "/"},
  "root": {"path": "rootfs", "readonly": false},
  "hostname": "ftagent",
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
blue "[3/6] starting agent under runsc + seeding warm state"
RUN_OUT=$(node_run 90 <<EOF 2>&1
set -e
BASE=${NODE_BASE}; ROOT=\$BASE/runsc-root; B=\$BASE/bundle; WORK=\$BASE/work
rsc(){ timeout ${RUNSC_TIMEOUT} nsenter -t 1 -m -p -- runsc --root=\$ROOT "\$@"; }
find \$B/rootfs -maxdepth 1 -name '.gvisor.filestore*' -delete 2>/dev/null || true
rsc run -detach -bundle \$B ${CID}
# wait for warm state
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

# ── 4. CHECKPOINT (freeze) — timed; assert by observed state ───────
blue "[4/6] runsc checkpoint (freeze)"
FREEZE_OUT=$(node_run 90 <<EOF 2>&1
set -e
BASE=${NODE_BASE}; ROOT=\$BASE/runsc-root; CK=\$BASE/ck; WORK=\$BASE/work
rm -rf \$CK && mkdir -p \$CK
t0=\$(date +%s%N)
# checkpoint completes the op but hangs on exit -> cap + ignore rc.
timeout ${RUNSC_TIMEOUT} nsenter -t 1 -m -p -- runsc --root=\$ROOT checkpoint --image-path=\$CK ${CID} >/dev/null 2>&1 || true
t1=\$(date +%s%N)
echo "FREEZE_MS=\$(( (t1-t0)/1000000 ))"
echo "IMAGES=\$(ls \$CK 2>/dev/null | tr '\n' ',')"
echo "SNAPSHOT_KIB=\$(du -sk \$CK 2>/dev/null | awk '{print \$1}')"
echo "STATUS=\$(timeout 20 nsenter -t 1 -m -p -- runsc --root=\$ROOT state ${CID} 2>/dev/null | python3 -c 'import sys,json;print(json.load(sys.stdin).get("status","?"))' 2>/dev/null || echo '?')"
echo "FROZEN_COUNTER=\$(cat \$WORK/counter 2>/dev/null)"
EOF
)
FREEZE_MS=$(printf '%s' "$FREEZE_OUT" | sed -n 's/^FREEZE_MS=//p' | tr -d '\r')
IMAGES=$(printf '%s' "$FREEZE_OUT"    | sed -n 's/^IMAGES=//p'    | tr -d '\r')
STATUS=$(printf '%s' "$FREEZE_OUT"    | sed -n 's/^STATUS=//p'    | tr -d '\r')
SNAP_KIB=$(printf '%s' "$FREEZE_OUT"  | sed -n 's/^SNAPSHOT_KIB=//p' | tr -d '\r')
FROZEN_COUNTER=$(printf '%s' "$FREEZE_OUT" | sed -n 's/^FROZEN_COUNTER=//p' | tr -d '\r')
if grep -q 'checkpoint.img' <<<"$IMAGES" && grep -q 'pages.img' <<<"$IMAGES"; then
  pass "checkpoint images written (${IMAGES%,}; ${SNAP_KIB:-?}KiB; freeze≈${FREEZE_MS:-?}ms)"
else
  fail "checkpoint images written" "ls ck = '$IMAGES'"
fi
if [ "$STATUS" = "stopped" ]; then
  pass "container stopped after checkpoint (frozen at counter=$FROZEN_COUNTER)"
else
  fail "container stopped after checkpoint" "runsc state status='$STATUS'"
fi

# Snapshot the frozen counter from the host as the authoritative freeze value.
[ -n "$FROZEN_COUNTER" ] && [ "$FROZEN_COUNTER" -ge 1 ] 2>/dev/null \
  && pass "frozen state token captured (counter=$FROZEN_COUNTER ≥ PRE=$PRE_COUNTER)" \
  || fail "frozen state token captured" "FROZEN_COUNTER='$FROZEN_COUNTER'"

# ── 5. RESTORE (thaw) — assert RESUME: counter climbs past freeze ──
blue "[5/6] runsc restore (thaw) + resume proof"
RESTORE_OUT=$(node_run 90 <<EOF 2>&1
set -e
BASE=${NODE_BASE}; ROOT=\$BASE/runsc-root; B=\$BASE/bundle; CK=\$BASE/ck; WORK=\$BASE/work
# delete the stopped container shell so we can restore fresh
timeout 20 nsenter -t 1 -m -p -- runsc --root=\$ROOT delete --force ${CID} 2>/dev/null || true
find \$B/rootfs -maxdepth 1 -name '.gvisor.filestore*' -delete 2>/dev/null || true
AT_RESTORE=\$(cat \$WORK/counter 2>/dev/null)
t2=\$(date +%s%N)
# restore completes the op but hangs on exit -> cap + ignore rc; prove liveness below.
timeout ${RUNSC_TIMEOUT} nsenter -t 1 -m -p -- runsc --root=\$ROOT restore -detach -bundle \$B --image-path=\$CK ${CID} >/dev/null 2>&1 || true
t3=\$(date +%s%N)
echo "THAW_MS=\$(( (t3-t2)/1000000 ))"
echo "AT_RESTORE=\$AT_RESTORE"
# resume proof: wait for the counter to advance past the frozen value
RESUMED=0
for _ in 1 2 3 4 5 6 7 8; do
  C=\$(cat \$WORK/counter 2>/dev/null || echo 0)
  if [ "\$C" -gt "${FROZEN_COUNTER:-0}" ] 2>/dev/null; then RESUMED=\$C; break; fi
  sleep 1
done
echo "RESUME_COUNTER=\$RESUMED"
echo "FILE_TOKEN_AFTER=\$(cat \$WORK/state.bin 2>/dev/null)"
EOF
)
THAW_MS=$(printf '%s' "$RESTORE_OUT"        | sed -n 's/^THAW_MS=//p'        | tr -d '\r')
RESUME_COUNTER=$(printf '%s' "$RESTORE_OUT" | sed -n 's/^RESUME_COUNTER=//p' | tr -d '\r')
FILE_TOKEN_AFTER=$(printf '%s' "$RESTORE_OUT" | sed -n 's/^FILE_TOKEN_AFTER=//p' | tr -d '\r')

# Thaw latency target: sub-second (PoC measured ~50-280ms). Informational
# unless wildly off; the hard assertion is RESUME below.
if [ -n "$THAW_MS" ] && [ "$THAW_MS" -lt 1000 ] 2>/dev/null; then
  pass "thaw latency sub-second (≈${THAW_MS}ms)"
else
  dim "    thaw latency ≈${THAW_MS:-?}ms (informational; not gating)"
fi

# HARD ASSERTION 1 — file token survived the freeze/thaw.
if [ "$FILE_TOKEN_AFTER" = "$SENTINEL" ]; then
  pass "FILE token survived thaw (state.bin = $SENTINEL)"
else
  fail "FILE token survived thaw" "got '$FILE_TOKEN_AFTER' want '$SENTINEL'"
fi

# HARD ASSERTION 2 — IN-MEMORY token resumed: the counter climbed PAST the
# frozen value, proving the restored process is the same live process (RAM
# preserved), not a cold restart from zero.
if [ -n "$RESUME_COUNTER" ] && [ "$RESUME_COUNTER" -gt "${FROZEN_COUNTER:-0}" ] 2>/dev/null; then
  pass "agent RESUMED — in-RAM counter climbed $FROZEN_COUNTER → $RESUME_COUNTER past freeze"
else
  fail "agent RESUMED past freeze" \
    "RESUME_COUNTER='$RESUME_COUNTER' not > FROZEN_COUNTER='$FROZEN_COUNTER' (restore did not resume the live process)"
fi

# ── 6. FAILURE PATH — thaw onto a node without runsc => clear error ─
# A runsc checkpoint must NOT be silently consumable by a non-runsc runtime.
# We attempt to restore the SAME checkpoint with the node's `runc` (the
# runtime present when runsc is absent) and assert: non-zero rc AND no
# container started. A clean exit or a started container would be silent
# corruption — the exact thing this gate exists to catch.
blue "[6/6] failure path: restore runsc checkpoint under runc => clear error"
FAILPATH_OUT=$(node_run 90 <<EOF 2>&1
BASE=${NODE_BASE}; B=\$BASE/bundle; CK=\$BASE/ck; RUNCROOT=\$BASE/runc-root
if ! command -v runc >/dev/null 2>&1 && [ ! -x /usr/sbin/runc ] && [ ! -x /usr/bin/runc ]; then
  echo "NO_RUNC=1"; exit 0
fi
RUNC=\$(command -v runc 2>/dev/null || echo /usr/sbin/runc)
rm -rf \$RUNCROOT && mkdir -p \$RUNCROOT
find \$B/rootfs -maxdepth 1 -name '.gvisor.filestore*' -delete 2>/dev/null || true
out=\$(timeout 30 nsenter -t 1 -m -p -- \$RUNC --root=\$RUNCROOT restore --image-path=\$CK --bundle=\$B failpath-${CID} 2>&1)
rc=\$?
echo "RUNC_RC=\$rc"
echo "RUNC_ERR=\$(printf '%s' "\$out" | grep -iE 'error|fail|panic|cannot|unable|invalid' | head -1)"
# Confirm no container actually came up under runc.
listed=\$(timeout 15 nsenter -t 1 -m -p -- \$RUNC --root=\$RUNCROOT list 2>/dev/null | grep -c 'failpath-${CID}.*running' || echo 0)
echo "RUNC_RUNNING=\$listed"
EOF
)
if grep -q 'NO_RUNC=1' <<<"$FAILPATH_OUT"; then
  dim "    no runc on node — failure-path uses an alternate negative below"
  # Alternate negative: restoring from a bogus/empty image-path must error.
  ALT_OUT=$(node_run 60 <<EOF 2>&1
BASE=${NODE_BASE}; ROOT=\$BASE/runsc-root; B=\$BASE/bundle
out=\$(timeout 30 nsenter -t 1 -m -p -- runsc --root=\$ROOT restore -detach -bundle \$B --image-path=/tmp/gvisor-ft-nonexistent-${RUN_ID} bogus-${CID} 2>&1)
echo "ALT_RC=\$?"
echo "ALT_ERR=\$(printf '%s' "\$out" | grep -iE 'error|fail|no such|cannot|unable' | head -1)"
EOF
)
  ALT_RC=$(printf '%s' "$ALT_OUT" | sed -n 's/^ALT_RC=//p' | tr -d '\r')
  ALT_ERR=$(printf '%s' "$ALT_OUT" | sed -n 's/^ALT_ERR=//p' | tr -d '\r')
  if [ -n "$ALT_RC" ] && [ "$ALT_RC" -ne 0 ] 2>/dev/null && [ -n "$ALT_ERR" ]; then
    pass "failure path (no-runsc-image): restore errored clearly (rc=$ALT_RC: ${ALT_ERR:0:80})"
  else
    fail "failure path clear error" "bogus restore rc='$ALT_RC' err='$ALT_ERR' (expected non-zero + message)"
  fi
else
  RUNC_RC=$(printf '%s' "$FAILPATH_OUT"      | sed -n 's/^RUNC_RC=//p'      | tr -d '\r')
  RUNC_ERR=$(printf '%s' "$FAILPATH_OUT"     | sed -n 's/^RUNC_ERR=//p'     | tr -d '\r')
  RUNC_RUNNING=$(printf '%s' "$FAILPATH_OUT" | sed -n 's/^RUNC_RUNNING=//p' | tr -d '\r')
  if [ -n "$RUNC_RC" ] && [ "$RUNC_RC" -ne 0 ] 2>/dev/null; then
    pass "failure path: runc restore of runsc checkpoint failed clearly (rc=$RUNC_RC: ${RUNC_ERR:0:80})"
  else
    fail "failure path clear error" "runc restore rc='$RUNC_RC' (expected non-zero; a clean exit would be silent corruption)"
  fi
  if [ "${RUNC_RUNNING:-0}" = "0" ]; then
    pass "failure path: NO container started under runc (no silent corruption)"
  else
    fail "failure path no container" "runc reports $RUNC_RUNNING running 'failpath' container(s) — silent corruption"
  fi
fi

# ── Summary ───────────────────────────────────────────────────────
echo ""
echo "════════════════════════════════════════════════════"
echo " gVisor freeze/thaw Summary: $PASS passed, $FAIL failed"
echo "   freeze≈${FREEZE_MS:-?}ms  thaw≈${THAW_MS:-?}ms  snapshot=${SNAP_KIB:-?}KiB"
echo "   counter: PRE=$PRE_COUNTER  FROZEN=$FROZEN_COUNTER  RESUMED=$RESUME_COUNTER"
echo "════════════════════════════════════════════════════"
if [ "$FAIL" -gt 0 ]; then
  for e in "${ERRORS[@]}"; do red "  - $e"; done
  exit 1
fi
green "All gVisor freeze/thaw assertions passed."
exit 0
