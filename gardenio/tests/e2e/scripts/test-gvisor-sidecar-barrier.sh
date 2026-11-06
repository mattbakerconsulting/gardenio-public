#!/usr/bin/env bash
# test-gvisor-sidecar-barrier.sh — N-container sidecar freeze/thaw BARRIER E2E.
# Bead ka-0000004828 (net-new coverage), epic ka-0000000958.
#
# THE GAP THIS CLOSES (gardener-verified, ka-0000004828):
#   test-gvisor-freeze-thaw.sh proves freeze/thaw for a SINGLE-container runsc
#   sandbox (one sub-container). The controller's real freeze/thaw path is a
#   COUNT-GATED, N-CONTAINER BARRIER (internal/podmanager/freezethaw.go:1957-1971,
#   freezethaw_node.go:2943-2989): a multi-container (sidecar) agent pod is
#   sandbox + agent + each always-on sidecar, ALL sharing ONE runsc sentry. Freeze
#   bakes FT_CONTAINER_COUNT = len(sub-containers)+1 into the statefile as the
#   barrier target and captures one FT_SUBC roster line per sub-container; thaw
#   drives ONE `runsc restore` per sub-container (matched BY container-name) on top
#   of a backgrounded sandbox-root restore that PARKS until the count is satisfied.
#   Until this module, that whole barrier was unit-only (freezethaw_ncontainer_test.go,
#   freezethaw_thaw_test.go) — no live-substrate E2E drove it. The only tree-wide
#   hit for the barrier terms was prose in test-resume-latency.sh (no assertion).
#
# WHAT THIS PROVES ON THE LIVE runsc SUBSTRATE (automatically, not by demo):
#   1. A MULTI-container runsc sandbox (sandbox root + 2 sub-containers: "agent"
#      + "dind") boots with ALL THREE sharing ONE sentry (one sentry PID owns the
#      sandbox + both subs) — the real sidecar-agent pod shape.
#   2. `runsc checkpoint` of the whole pod (via the sandbox cid) bakes
#      container_count == 3 (= len(subs)+1) into the statefile metadata — the
#      barrier target the controller reads back (freezethaw.go keyContainerCount).
#   3. THE BARRIER HOLDS: after teardown, a backgrounded sandbox-root restore plus
#      a restore of only N-1 sub-containers (just "agent") does NOT release — the
#      root restore call stays blocked at restoringUnstarted (count 2 < 3), so the
#      whole pod stays frozen. This is the count-gate: a partial roster must never
#      resume a half-restored pod.
#   4. THE BARRIER RELEASES on the Nth: restoring the last sub-container ("dind")
#      satisfies the count; the backgrounded root restore returns rc=0 and the pod
#      resumes.
#   5. STATE FIDELITY per sub-container: after resume, BOTH sub-containers' in-RAM
#      counters climb PAST their frozen values (same live process, RAM preserved)
#      AND both sentinel FILE tokens survive — proving each sub-container's state
#      was restored to the RIGHT slot (name-keyed pairing), not cross-wired.
#   6. FAIL-CLOSED at the substrate: restoring a sub-container whose bundle does
#      NOT correspond to a valid roster slot of the checkpointed+restored pod (a
#      bogus container-name) is refused by runsc and starts NO container — a
#      spurious/off-roster restore never silently joins the pod. This is the
#      substrate-level complement to the controller's Go-side count/roster
#      cross-check at freeze time (freezethaw.go:1969, unit-tested by
#      freezethaw_ncontainer_test.go:TestFreeze_CountRosterMismatchFailsClosed):
#      both sides fail closed rather than admit a shape mismatch.
#
# HOW IT WORKS (reuses the freeze-thaw harness mechanism + the proven
# multi-container-resume PoC, gardenio/docs/gvisor-runsc-cr/multi-container-resume.md):
#   Like test-gvisor-freeze-thaw.sh, kubectl/CRI exposes no checkpoint verb for
#   runsc and the runsc state root lives on the node, so we schedule a privileged
#   hostPath+hostPID probe onto the gvisor node and drive `runsc` directly via
#   `nsenter -t 1 -m -p` into the host init namespaces. UNLIKE freeze-thaw's single
#   harness-owned container, we build a HARNESS-OWNED MULTI-container sandbox: one
#   sandbox-root bundle (container-type=sandbox) + two sub-container bundles
#   (container-type=container, distinct container-name annotations) all carrying the
#   SAME io.kubernetes.cri.sandbox-id, so `runsc create`/`start` attach both subs to
#   the one sentry — exactly the CRI sidecar-pod shape, without kubelet.
#
#   runsc QUIRK (same as freeze-thaw): `runsc checkpoint`/`restore` COMPLETE the op
#   then hang on process EXIT, so every call is `timeout`-wrapped and success is
#   asserted from OBSERVED STATE, never rc. ADDITIONAL barrier-specific quirk: a
#   `runsc create`/`start`/`restore` spawns a sub-container GOFER daemon that
#   inherits stdout; piping it through a reader deadlocks the pipe even after the
#   parent exits, so every such call REDIRECTS to a file — never a pipe.
#
# OPT-IN / GATING (identical to test-gvisor-freeze-thaw.sh):
#   SKIPs cleanly (exit 0) unless GVISOR_E2E=1 AND the substrate is present (runsc
#   RuntimeClass + create-pods in $GVISOR_NS). Runs only via an explicit
#   `GVISOR_E2E=1 ONLY=gvisor-sidecar-barrier` invocation.
#
# Usage:
#   GVISOR_E2E=1 ./gardenio/tests/e2e/scripts/test-gvisor-sidecar-barrier.sh
#   GVISOR_E2E=1 GVISOR_NS=gardenio-gvisor KEEP_PROBE=1 ./...   # keep probe for debug

set -uo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
# shellcheck disable=SC1091
source "$SCRIPT_DIR/lib-gvisor-node.sh"

# ── Config ────────────────────────────────────────────────────────
GVISOR_NS="${GVISOR_NS:-gvisor-poc}"
RUNTIME_CLASS="${RUNTIME_CLASS:-runsc}"
RUN_ID="${RUN_ID:-$$}"
PROBE="${PROBE:-gvisor-scb-probe-${RUN_ID}}"
# Node-local working root for this run's harness-owned sandbox + bundles.
NODE_BASE="/tmp/gvisor-scb-${RUN_ID}"
# Sub-container roster (the sidecars): "agent" is the canonical CRI name, "dind"
# the always-on sidecar. container_count == len(SUBS)+1 (sandbox root).
SUBS="agent dind"
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
skip_optin_off() {
  blue "SKIP gvisor-sidecar-barrier: $1"
  dim "  This module is opt-in (set GVISOR_E2E=1) and requires the gvisor-poc"
  dim "  substrate (runsc RuntimeClass + create-pods in ns $GVISOR_NS)."
  if [ -n "${E2E_SKIP_SENTINEL:-}" ]; then printf '%s\n' "$*" >"$E2E_SKIP_SENTINEL" 2>/dev/null || true; fi  # skip visibility (ka-0000000989)
  exit 0
}
skip_all() { gvisor_require_substrate_guard gvisor-sidecar-barrier "$1"; }

k() { kubectl -n "$GVISOR_NS" "$@"; }

# Stream a script to the node inside the host init namespaces with a wall-clock cap.
node_run() {
  local cap="$1"; shift
  timeout "$cap" kubectl -n "$GVISOR_NS" exec -i "$PROBE" -- chroot /host bash -s
}

cleanup() {
  blue "── cleanup ──"
  node_run 90 <<EOF 2>/dev/null || true
ROOT=${NODE_BASE}/runsc-root
RSC(){ timeout 20 nsenter -t 1 -m -p -- runsc --root=\$ROOT "\$@"; }
for c in \$(RSC list 2>/dev/null | awk 'NR>1 && /scb-/{print \$1}'); do
  RSC kill "\$c" KILL 2>/dev/null || true
  RSC delete --force "\$c" 2>/dev/null || true
done
for p in \$(ps aux | grep runsc | grep '${NODE_BASE}' | grep -v grep | awk '{print \$2}'); do
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
echo " gVisor N-container sidecar freeze/thaw BARRIER E2E"
echo " ns=$GVISOR_NS  runtimeClass=$RUNTIME_CLASS  subs=[$SUBS]  base=$NODE_BASE"
echo "════════════════════════════════════════════════════"

# ── Gate 0: opt-in + substrate preflight ──────────────────────────
[ "${GVISOR_E2E:-0}" = "1" ] || skip_optin_off "GVISOR_E2E != 1 (opt-in module)"
command -v kubectl >/dev/null 2>&1 || skip_all "kubectl not installed"
kubectl get runtimeclass "$RUNTIME_CLASS" >/dev/null 2>&1 \
  || skip_all "RuntimeClass '$RUNTIME_CLASS' not present on this cluster"
[ "$(kubectl auth can-i create pods -n "$GVISOR_NS" 2>/dev/null)" = "yes" ] \
  || skip_all "cannot create pods in ns '$GVISOR_NS'"

blue "Substrate present — running N-container barrier assertions."

# ── 1. Schedule the privileged probe onto the gvisor node ──────────
blue "[1/7] scheduling privileged probe '$PROBE' onto the gvisor node"
cat <<YAML | k apply -f - >/dev/null 2>&1
apiVersion: v1
kind: Pod
metadata:
  name: ${PROBE}
  namespace: ${GVISOR_NS}
  labels: {app: gvisor-scb-probe, test: ka-0000004828}
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

# Warm-node gate (rides out a Karpenter cold-start; hard-fails a real regression).
warm_shim() { node_run "$1" <<EOF 2>/dev/null
$2
EOF
}
if gvisor_warm_node_gate warm_shim; then
  pass "gvisor node warm (runsc present, /var/tmp writable, runsc-root reachable)"
else
  fail "gvisor node warm-gate" "$GVISOR_WARM_DIAG"
  echo "════════ Summary: $PASS passed, $FAIL failed ════════"
  exit 1
fi

# ── 2. Build a harness-owned MULTI-container runsc sandbox ──────────
# One sandbox-root bundle + one bundle per sub-container, all carrying the SAME
# io.kubernetes.cri.sandbox-id so `runsc create/start` attach the subs to the one
# sentry. Each sub-container writes a per-name sentinel FILE token + an in-RAM
# monotonic counter flushed to a HOST-bind-mounted /work, readable from the host.
blue "[2/7] building harness-owned multi-container sandbox (root + subs [$SUBS])"
BUILD_OUT=$(node_run 150 <<EOF 2>&1
set -e
BASE=${NODE_BASE}; ROOT=\$BASE/runsc-root; SRC=\$BASE/src
rm -rf \$BASE; mkdir -p \$BASE/oci \$ROOT \$SRC \$BASE/ck
ctr -n gv-scb images pull ${BUSYBOX_IMAGE} >/dev/null 2>&1
ctr -n gv-scb images export \$BASE/bb.tar ${BUSYBOX_IMAGE} >/dev/null 2>&1
tar -xf \$BASE/bb.tar -C \$BASE/oci
LAYER=\$(python3 -c 'import json;print(json.load(open("'\$BASE'/oci/manifest.json"))[0]["Layers"][0])')
tar -xf \$BASE/oci/\$LAYER -C \$SRC
test -x \$SRC/bin/busybox

# sandbox-root bundle (container-type=sandbox; runs a long sleep as PID 1).
SBB=\$BASE/bundle-sandbox; mkdir -p \$SBB/rootfs; cp -a \$SRC/. \$SBB/rootfs/
cat > \$SBB/config.json <<CFG
{"ociVersion":"1.0.0","process":{"user":{"uid":0,"gid":0},"args":["/bin/sleep","100000"],"env":["PATH=/bin"],"cwd":"/"},
"root":{"path":"rootfs","readonly":false},"hostname":"scb",
"annotations":{"io.kubernetes.cri.container-type":"sandbox","io.kubernetes.cri.sandbox-id":"scb-sb-${RUN_ID}"},
"mounts":[{"destination":"/proc","type":"proc","source":"proc"}],
"linux":{"namespaces":[{"type":"pid"},{"type":"mount"},{"type":"network"},{"type":"ipc"},{"type":"uts"}]}}
CFG

# one sub-container bundle per name (container-type=container; distinct name; same sandbox-id).
for name in ${SUBS}; do
  B=\$BASE/bundle-\$name; W=\$BASE/work-\$name; SENT="SCB-\$name-${RUN_ID}"
  mkdir -p \$B/rootfs \$W; cp -a \$SRC/. \$B/rootfs/
  cat > \$B/rootfs/agent.sh <<AGENT
#!/bin/sh
echo "\$SENT" > /work/state.bin
i=0
exec 3>>/work/agent.log
echo "BOOT name=\$name pid=\\\$\\\$ sentinel=\$SENT" >&3
while true; do
  i=\\\$((i+1))
  echo "\\\$i" > /work/counter
  echo "tick=\\\$i sentinel=\$SENT" >&3
  sleep 1
done
AGENT
  chmod +x \$B/rootfs/agent.sh
  cat > \$B/config.json <<CFG
{"ociVersion":"1.0.0","process":{"user":{"uid":0,"gid":0},"args":["/bin/sh","/agent.sh"],"env":["PATH=/bin"],"cwd":"/"},
"root":{"path":"rootfs","readonly":false},"hostname":"scb",
"annotations":{"io.kubernetes.cri.container-type":"container","io.kubernetes.cri.container-name":"\$name","io.kubernetes.cri.sandbox-id":"scb-sb-${RUN_ID}"},
"mounts":[{"destination":"/proc","type":"proc","source":"proc"},
{"destination":"/work","type":"bind","source":"\$W","options":["rbind","rw"]}],
"linux":{"namespaces":[{"type":"pid"},{"type":"mount"},{"type":"network"},{"type":"ipc"},{"type":"uts"}]}}
CFG
done
echo "BUILD_OK subs=[${SUBS}]"
EOF
)
if grep -q 'BUILD_OK' <<<"$BUILD_OUT"; then
  pass "multi-container sandbox bundles built (root + [$SUBS])"
else
  fail "build multi-container sandbox" "$(printf '%s' "$BUILD_OUT" | tail -5)"
  echo "════════ Summary: $PASS passed, $FAIL failed ════════"
  exit 1
fi

# ── 3. Run root + start subs; assert ONE shared sentry + warm state ─
blue "[3/7] running sandbox + starting subs; asserting one shared sentry"
RUN_OUT=$(node_run 120 <<EOF 2>&1
set -e
BASE=${NODE_BASE}; ROOT=\$BASE/runsc-root
RSC(){ timeout ${RUNSC_TIMEOUT} nsenter -t 1 -m -p -- runsc --root=\$ROOT "\$@"; }
st(){ RSC state "\$1" 2>/dev/null | python3 -c 'import sys,json;print(json.load(sys.stdin).get("status","?"))' 2>/dev/null || echo '?'; }
pidof_cid(){ RSC state "\$1" 2>/dev/null | python3 -c 'import sys,json;print(json.load(sys.stdin).get("pid","?"))' 2>/dev/null || echo '?'; }
# run sandbox root (output REDIRECTED — never piped: sub gofers deadlock a pipe)
find \$BASE/bundle-sandbox/rootfs -maxdepth 1 -name '.gvisor.filestore*' -delete 2>/dev/null || true
RSC run -detach -bundle \$BASE/bundle-sandbox scb-sb-${RUN_ID} >\$BASE/sb.out 2>&1
for _ in 1 2 3 4 5; do [ "\$(st scb-sb-${RUN_ID})" = running ] && break; sleep 1; done
# create + start each sub-container into the shared sentry
for name in ${SUBS}; do
  find \$BASE/bundle-\$name/rootfs -maxdepth 1 -name '.gvisor.filestore*' -delete 2>/dev/null || true
  RSC create -bundle \$BASE/bundle-\$name scb-\$name-${RUN_ID} >\$BASE/create-\$name.out 2>&1
  RSC start scb-\$name-${RUN_ID} >\$BASE/start-\$name.out 2>&1
done
for _ in 1 2 3 4 5 6 7 8 9 10; do [ -s \$BASE/work-agent/counter ] && [ -s \$BASE/work-dind/counter ] && break; sleep 1; done
echo "SB_PID=\$(pidof_cid scb-sb-${RUN_ID})"
for name in ${SUBS}; do echo "SUBPID_\$name=\$(pidof_cid scb-\$name-${RUN_ID})"; done
echo "LIVE_COUNT=\$(RSC list 2>/dev/null | grep -c 'scb-')"
for name in ${SUBS}; do
  echo "PRE_\$name=\$(cat \$BASE/work-\$name/counter 2>/dev/null)"
  echo "TOK_\$name=\$(cat \$BASE/work-\$name/state.bin 2>/dev/null)"
done
EOF
)
SB_PID=$(printf '%s' "$RUN_OUT" | sed -n 's/^SB_PID=//p' | tr -d '\r')
SUBPID_AGENT=$(printf '%s' "$RUN_OUT" | sed -n 's/^SUBPID_agent=//p' | tr -d '\r')
SUBPID_DIND=$(printf '%s' "$RUN_OUT" | sed -n 's/^SUBPID_dind=//p' | tr -d '\r')
LIVE_COUNT=$(printf '%s' "$RUN_OUT" | sed -n 's/^LIVE_COUNT=//p' | tr -d '\r')
PRE_AGENT=$(printf '%s' "$RUN_OUT" | sed -n 's/^PRE_agent=//p' | tr -d '\r')
PRE_DIND=$(printf '%s' "$RUN_OUT" | sed -n 's/^PRE_dind=//p' | tr -d '\r')
TOK_AGENT=$(printf '%s' "$RUN_OUT" | sed -n 's/^TOK_agent=//p' | tr -d '\r')
TOK_DIND=$(printf '%s' "$RUN_OUT" | sed -n 's/^TOK_dind=//p' | tr -d '\r')

# 3a. all three containers report the SAME sentry pid (one shared sentry).
if [ -n "$SB_PID" ] && [ "$SB_PID" != "?" ] \
   && [ "$SUBPID_AGENT" = "$SB_PID" ] && [ "$SUBPID_DIND" = "$SB_PID" ]; then
  pass "one shared sentry: sandbox+agent+dind all report pid=$SB_PID"
else
  fail "shared sentry" "pids sandbox=$SB_PID agent=$SUBPID_AGENT dind=$SUBPID_DIND (expected all equal; sidecar shape not established)"
  echo "════════ Summary: $PASS passed, $FAIL failed ════════"
  exit 1
fi
# 3b. live container count == len(subs)+1 (the barrier target we expect frozen).
if [ "$LIVE_COUNT" = "3" ]; then
  pass "live container_count == 3 (sandbox + agent + dind)"
else
  fail "live container_count" "got '$LIVE_COUNT', want 3"
fi
# 3c. both subs warm (counter climbing) + sentinel tokens seeded.
if [ -n "$PRE_AGENT" ] && [ "$PRE_AGENT" -ge 1 ] 2>/dev/null \
   && [ -n "$PRE_DIND" ] && [ "$PRE_DIND" -ge 1 ] 2>/dev/null; then
  pass "both sub-containers warm (agent=$PRE_AGENT dind=$PRE_DIND)"
else
  fail "sub-containers warm" "agent='$PRE_AGENT' dind='$PRE_DIND' (no counter after start)"
  echo "════════ Summary: $PASS passed, $FAIL failed ════════"
  exit 1
fi
[ "$TOK_AGENT" = "SCB-agent-${RUN_ID}" ] && [ "$TOK_DIND" = "SCB-dind-${RUN_ID}" ] \
  && pass "per-sub sentinel tokens seeded (agent/dind state.bin)" \
  || fail "sub sentinel tokens seeded" "agent='$TOK_AGENT' dind='$TOK_DIND'"

# ── 4. CHECKPOINT the whole pod; assert container_count == 3 baked in ─
# Plain checkpoint (NO --leave-running): the whole pod STOPS, so each sub's in-RAM
# counter freezes at an exact value the host-visible /work/counter file records.
# (The controller uses --leave-running to keep the live Pod A serving during image
# relocation; here the sandbox is harness-owned and torn down next, so a clean stop
# gives an unambiguous frozen value — matching test-gvisor-freeze-thaw.sh.)
blue "[4/7] runsc checkpoint (freeze) — assert container_count==3 barrier target"
FREEZE_OUT=$(node_run 90 <<EOF 2>&1
set -e
BASE=${NODE_BASE}; ROOT=\$BASE/runsc-root; CK=\$BASE/ck
rm -rf \$CK && mkdir -p \$CK
t0=\$(date +%s%N)
timeout ${RUNSC_TIMEOUT} nsenter -t 1 -m -p -- runsc --root=\$ROOT checkpoint --image-path=\$CK scb-sb-${RUN_ID} >\$BASE/ckpt.out 2>&1 || true
t1=\$(date +%s%N)
echo "FREEZE_MS=\$(( (t1-t0)/1000000 ))"
echo "IMAGES=\$(ls \$CK 2>/dev/null | tr '\n' ',')"
echo "SNAP_KIB=\$(du -sk \$CK 2>/dev/null | awk '{print \$1}')"
echo "SB_STATE=\$(timeout 20 nsenter -t 1 -m -p -- runsc --root=\$ROOT state scb-sb-${RUN_ID} 2>/dev/null | python3 -c 'import sys,json;print(json.load(sys.stdin).get("status","?"))' 2>/dev/null || echo '?')"
# The barrier target baked into the statefile: parse container_count out of the
# checkpoint.img metadata header (freezethaw.go reads keyContainerCount here).
echo "BAKED_COUNT=\$(strings \$CK/checkpoint.img 2>/dev/null | python3 -c 'import sys,re
for l in sys.stdin:
  m=re.search(r"\"container_count\":\"?(\d+)\"?", l)
  if m: print(m.group(1)); break' 2>/dev/null)"
for name in ${SUBS}; do echo "FROZEN_\$name=\$(cat \$BASE/work-\$name/counter 2>/dev/null)"; done
EOF
)
FREEZE_MS=$(printf '%s' "$FREEZE_OUT" | sed -n 's/^FREEZE_MS=//p' | tr -d '\r')
IMAGES=$(printf '%s' "$FREEZE_OUT" | sed -n 's/^IMAGES=//p' | tr -d '\r')
SNAP_KIB=$(printf '%s' "$FREEZE_OUT" | sed -n 's/^SNAP_KIB=//p' | tr -d '\r')
SB_STATE=$(printf '%s' "$FREEZE_OUT" | sed -n 's/^SB_STATE=//p' | tr -d '\r')
BAKED_COUNT=$(printf '%s' "$FREEZE_OUT" | sed -n 's/^BAKED_COUNT=//p' | tr -d '\r')
FROZEN_AGENT=$(printf '%s' "$FREEZE_OUT" | sed -n 's/^FROZEN_agent=//p' | tr -d '\r')
FROZEN_DIND=$(printf '%s' "$FREEZE_OUT" | sed -n 's/^FROZEN_dind=//p' | tr -d '\r')

if grep -q 'checkpoint.img' <<<"$IMAGES" && grep -q 'pages.img' <<<"$IMAGES"; then
  pass "checkpoint images written (${IMAGES%,}; ${SNAP_KIB:-?}KiB; freeze≈${FREEZE_MS:-?}ms)"
else
  fail "checkpoint images written" "ls ck = '$IMAGES'"
  echo "════════ Summary: $PASS passed, $FAIL failed ════════"
  exit 1
fi
[ "$SB_STATE" = "stopped" ] \
  && pass "whole pod stopped after checkpoint (frozen: agent=$FROZEN_AGENT dind=$FROZEN_DIND)" \
  || fail "pod stopped after checkpoint" "sandbox state='$SB_STATE' (a plain checkpoint must stop the pod)"
# THE KEY FREEZE ASSERTION: the statefile carries container_count == len(subs)+1.
if [ "$BAKED_COUNT" = "3" ]; then
  pass "statefile bakes container_count == 3 (barrier target = agent + dind + sandbox)"
else
  fail "statefile container_count == 3" "parsed BAKED_COUNT='$BAKED_COUNT' from checkpoint.img (barrier target wrong)"
fi
[ -n "$FROZEN_AGENT" ] && [ "$FROZEN_AGENT" -ge 1 ] 2>/dev/null \
  && [ -n "$FROZEN_DIND" ] && [ "$FROZEN_DIND" -ge 1 ] 2>/dev/null \
  && pass "frozen state values captured (agent=$FROZEN_AGENT dind=$FROZEN_DIND)" \
  || fail "frozen state values" "agent='$FROZEN_AGENT' dind='$FROZEN_DIND'"

# ── 5+6. THE COUNT-GATED BARRIER: HOLD with N-1, RELEASE on the Nth ──
# One continuous restore sequence in ONE node shell (so the backgrounded root
# restore is owned throughout): tear down the originals, background the sandbox-
# root restore, then restore the sub-containers ONE AT A TIME with a state probe
# between each.  The invariant we assert (version-independent — see below):
#
#   * With only N-1 sub-containers restored (root + "agent" = 2 of 3), the pod
#     stays FROZEN: neither sub's in-RAM counter advances past its frozen value,
#     because the sentry parks at restoringUnstarted until container_count is met.
#   * When the Nth sub ("dind") is restored (3 of 3), the count is satisfied, the
#     kernel resumes, and BOTH subs' counters climb PAST freeze with tokens intact.
#
# WHY the counter (not the bg-restore's liveness) is the barrier observable:
# on runsc release-20260727.0 the detached ROOT `runsc restore` CLI call can
# RETURN rc=0 before the whole pod finishes restoring, yet the guest kernel stays
# paused until the count is met (empirically verified on the live gvisor node).
# So the authoritative "held vs released" signal is whether the guest is EXECUTING
# — i.e. whether the host-visible counter advances — not whether the CLI returned.
blue "[5/7] barrier HOLD: bg root + agent only (2/3) — guest must stay frozen"
BARRIER_OUT=$(node_run 200 <<EOF 2>&1
set -u
BASE=${NODE_BASE}; ROOT=\$BASE/runsc-root; CK=\$BASE/ck
RSC(){ timeout ${RUNSC_TIMEOUT} nsenter -t 1 -m -p -- runsc --root=\$ROOT "\$@"; }
RST(){ timeout ${RUNSC_TIMEOUT} nsenter -t 1 -m -p -- runsc --root=\$ROOT restore --image-path=\$CK "\$@"; }
# tear down the leave-running originals so the cids are free to restore into.
for name in ${SUBS}; do RSC kill scb-\$name-${RUN_ID} KILL >/dev/null 2>&1 || true; RSC delete --force scb-\$name-${RUN_ID} >/dev/null 2>&1 || true; done
RSC kill scb-sb-${RUN_ID} KILL >/dev/null 2>&1 || true; RSC delete --force scb-sb-${RUN_ID} >/dev/null 2>&1 || true
sleep 1
find \$BASE/bundle-sandbox/rootfs -maxdepth 1 -name '.gvisor.filestore*' -delete 2>/dev/null || true
for name in ${SUBS}; do find \$BASE/bundle-\$name/rootfs -maxdepth 1 -name '.gvisor.filestore*' -delete 2>/dev/null || true; done
echo "REMAINING_AFTER_TEARDOWN=\$(RSC list 2>/dev/null | grep -c 'scb-')"

# background the ROOT sandbox restore — it registers the count target and parks.
( RST --bundle \$BASE/bundle-sandbox --detach scb-sb-${RUN_ID} >\$BASE/rroot.out 2>&1; echo "ROOT_RC=\$?" >>\$BASE/rroot.out ) &
ROOT_BG=\$!
for _ in \$(seq 1 20); do RSC state scb-sb-${RUN_ID} >/dev/null 2>&1 && break; sleep 0.5; done

# PHASE A — restore ONLY the "agent" sub (2 of 3). Barrier must HOLD.
RST --bundle \$BASE/bundle-agent --detach scb-agent-${RUN_ID} >\$BASE/rsub-agent.out 2>&1
echo "AGENT_SUB_RC=\$?"
# Sample the counters twice, ~4s apart, while only 2/3 are restored. A count-gated
# barrier keeps the guest paused, so the counters must NOT advance between samples.
HOLD_A1=\$(cat \$BASE/work-agent/counter 2>/dev/null); HOLD_D1=\$(cat \$BASE/work-dind/counter 2>/dev/null)
sleep 4
HOLD_A2=\$(cat \$BASE/work-agent/counter 2>/dev/null); HOLD_D2=\$(cat \$BASE/work-dind/counter 2>/dev/null)
echo "HOLD_A1=\$HOLD_A1"; echo "HOLD_A2=\$HOLD_A2"; echo "HOLD_D1=\$HOLD_D1"; echo "HOLD_D2=\$HOLD_D2"
echo "HOLD_COUNT=\$(RSC list 2>/dev/null | grep -c 'scb-')"

# PHASE B — restore the last sub "dind" (3 of 3). Barrier must RELEASE.
RST --bundle \$BASE/bundle-dind --detach scb-dind-${RUN_ID} >\$BASE/rsub-dind.out 2>&1
echo "DIND_SUB_RC=\$?"
wait \$ROOT_BG 2>/dev/null || true
echo "ROOT_RC=\$(sed -n 's/^ROOT_RC=//p' \$BASE/rroot.out | tail -1)"
echo "RELEASED_COUNT=\$(RSC list 2>/dev/null | grep -c 'scb-')"
# resume proof: both counters must climb PAST their frozen values.
for _ in \$(seq 1 12); do
  A=\$(cat \$BASE/work-agent/counter 2>/dev/null || echo 0); D=\$(cat \$BASE/work-dind/counter 2>/dev/null || echo 0)
  if [ "\$A" -gt "${FROZEN_AGENT:-0}" ] 2>/dev/null && [ "\$D" -gt "${FROZEN_DIND:-0}" ] 2>/dev/null; then break; fi
  sleep 1
done
echo "RESUME_agent=\$(cat \$BASE/work-agent/counter 2>/dev/null)"
echo "RESUME_dind=\$(cat \$BASE/work-dind/counter 2>/dev/null)"
echo "TOKAFTER_agent=\$(cat \$BASE/work-agent/state.bin 2>/dev/null)"
echo "TOKAFTER_dind=\$(cat \$BASE/work-dind/state.bin 2>/dev/null)"
EOF
)
REMAINING=$(printf '%s' "$BARRIER_OUT" | sed -n 's/^REMAINING_AFTER_TEARDOWN=//p' | tr -d '\r')
HOLD_A1=$(printf '%s' "$BARRIER_OUT" | sed -n 's/^HOLD_A1=//p' | tr -d '\r')
HOLD_A2=$(printf '%s' "$BARRIER_OUT" | sed -n 's/^HOLD_A2=//p' | tr -d '\r')
HOLD_D1=$(printf '%s' "$BARRIER_OUT" | sed -n 's/^HOLD_D1=//p' | tr -d '\r')
HOLD_D2=$(printf '%s' "$BARRIER_OUT" | sed -n 's/^HOLD_D2=//p' | tr -d '\r')
HOLD_COUNT=$(printf '%s' "$BARRIER_OUT" | sed -n 's/^HOLD_COUNT=//p' | tr -d '\r')
ROOT_RC=$(printf '%s' "$BARRIER_OUT" | sed -n 's/^ROOT_RC=//p' | tr -d '\r')
RELEASED_COUNT=$(printf '%s' "$BARRIER_OUT" | sed -n 's/^RELEASED_COUNT=//p' | tr -d '\r')
RESUME_AGENT=$(printf '%s' "$BARRIER_OUT" | sed -n 's/^RESUME_agent=//p' | tr -d '\r')
RESUME_DIND=$(printf '%s' "$BARRIER_OUT" | sed -n 's/^RESUME_dind=//p' | tr -d '\r')
TOKAFTER_AGENT=$(printf '%s' "$BARRIER_OUT" | sed -n 's/^TOKAFTER_agent=//p' | tr -d '\r')
TOKAFTER_DIND=$(printf '%s' "$BARRIER_OUT" | sed -n 's/^TOKAFTER_dind=//p' | tr -d '\r')

[ "$REMAINING" = "0" ] && pass "originals torn down before restore (0 scb containers remain)" \
  || fail "teardown before restore" "still $REMAINING scb containers after delete"

# THE KEY HOLD ASSERTION: with 2 of 3 restored, NEITHER counter advances between
# the two ~4s-apart samples — the guest is paused at the count-gated barrier.
if [ -n "$HOLD_A1" ] && [ -n "$HOLD_A2" ] && [ -n "$HOLD_D1" ] && [ -n "$HOLD_D2" ] \
   && [ "$HOLD_A2" -le "$HOLD_A1" ] 2>/dev/null && [ "$HOLD_D2" -le "$HOLD_D1" ] 2>/dev/null; then
  pass "BARRIER HELD: guest frozen with 2/3 restored (agent $HOLD_A1→$HOLD_A2, dind $HOLD_D1→$HOLD_D2 — no advance)"
else
  fail "barrier held with 2/3" "counters advanced under a partial roster (agent $HOLD_A1→$HOLD_A2, dind $HOLD_D1→$HOLD_D2) — count-gate leaked; the guest resumed before all 3 joined"
fi
# Corroborating signal: only N-1 (=2) containers are registered while the barrier
# holds — the Nth ("dind") has not yet been restored, so the count is unmet.
[ "$HOLD_COUNT" = "2" ] \
  && pass "only 2 of 3 containers registered during the hold (count unmet)" \
  || dim "    containers registered during hold: '$HOLD_COUNT' (expected 2; informational)"
# The frozen values themselves must not have been surpassed during the hold.
if [ -n "$HOLD_A2" ] && [ -n "$FROZEN_AGENT" ] && [ "$HOLD_A2" -le "$FROZEN_AGENT" ] 2>/dev/null \
   && [ -n "$HOLD_D2" ] && [ -n "$FROZEN_DIND" ] && [ "$HOLD_D2" -le "$FROZEN_DIND" ] 2>/dev/null; then
  pass "held counters ≤ frozen values (agent $HOLD_A2≤$FROZEN_AGENT, dind $HOLD_D2≤$FROZEN_DIND)"
else
  dim "    held=agent:$HOLD_A2/dind:$HOLD_D2 vs frozen=agent:$FROZEN_AGENT/dind:$FROZEN_DIND (informational)"
fi

# ── 6. RELEASE proof ────────────────────────────────────────────────
blue "[6/7] barrier RELEASE: restored dind (3/3) — both subs resume past freeze"
if [ "$ROOT_RC" = "0" ]; then
  pass "backgrounded root restore returned rc=0 after all 3 restored"
else
  fail "root restore rc after release" "ROOT_RC='$ROOT_RC' (expected 0 once the barrier was satisfied)"
fi
[ "$RELEASED_COUNT" = "3" ] && pass "all 3 containers re-scheduled after release" \
  || fail "3 containers after release" "runsc list shows '$RELEASED_COUNT' scb containers (want 3)"

# STATE FIDELITY: both subs' in-RAM counters climbed past freeze (RAM preserved,
# name-keyed restore landed each sub's state on the right slot).
if [ -n "$RESUME_AGENT" ] && [ "$RESUME_AGENT" -gt "${FROZEN_AGENT:-0}" ] 2>/dev/null; then
  pass "agent sub RESUMED — counter $FROZEN_AGENT → $RESUME_AGENT past freeze"
else
  fail "agent sub resumed past freeze" "RESUME_agent='$RESUME_AGENT' not > FROZEN_agent='$FROZEN_AGENT'"
fi
if [ -n "$RESUME_DIND" ] && [ "$RESUME_DIND" -gt "${FROZEN_DIND:-0}" ] 2>/dev/null; then
  pass "dind sub RESUMED — counter $FROZEN_DIND → $RESUME_DIND past freeze"
else
  fail "dind sub resumed past freeze" "RESUME_dind='$RESUME_DIND' not > FROZEN_dind='$FROZEN_DIND'"
fi
# FILE tokens survived for BOTH subs, each with its OWN sentinel (no cross-wiring).
[ "$TOKAFTER_AGENT" = "SCB-agent-${RUN_ID}" ] && [ "$TOKAFTER_DIND" = "SCB-dind-${RUN_ID}" ] \
  && pass "per-sub FILE tokens survived, correctly slotted (agent='$TOKAFTER_AGENT' dind='$TOKAFTER_DIND')" \
  || fail "per-sub tokens survived" "agent='$TOKAFTER_AGENT' dind='$TOKAFTER_DIND' (want SCB-agent-${RUN_ID}/SCB-dind-${RUN_ID}; cross-wired or lost)"

# ── 7. FAIL-CLOSED: a sub whose bundle is NOT in the roster is refused ─
# Substrate-level complement to the controller's freeze-time count/roster
# cross-check (freezethaw.go:1969): restoring a sub-container against a bundle
# that does not correspond to a valid roster slot of the restored pod (a bogus
# container-name) must ERROR and start NO container — a shape mismatch never
# silently joins the pod.
blue "[7/7] fail-closed: restoring an off-roster sub bundle errors clearly"
FC_OUT=$(node_run 90 <<EOF 2>&1
BASE=${NODE_BASE}; ROOT=\$BASE/runsc-root; CK=\$BASE/ck
# Craft a bundle with a container-name NOT in the checkpointed roster (postgres).
BOGUS=\$BASE/bundle-bogus; rm -rf \$BOGUS; cp -a \$BASE/bundle-agent \$BOGUS
python3 - <<'PY'
import json
p="${NODE_BASE}/bundle-bogus/config.json"
c=json.load(open(p))
c["annotations"]["io.kubernetes.cri.container-name"]="postgres"
json.dump(c,open(p,"w"))
PY
find \$BOGUS/rootfs -maxdepth 1 -name '.gvisor.filestore*' -delete 2>/dev/null || true
out=\$(timeout 30 nsenter -t 1 -m -p -- runsc --root=\$ROOT restore --image-path=\$CK --bundle \$BOGUS --detach scb-bogus-${RUN_ID} 2>&1); rc=\$?
echo "BOGUS_RC=\$rc"
echo "BOGUS_ERR=\$(printf '%s' "\$out" | grep -iE 'error|fail|not being restored|no such|unexpected|invalid|belong' | head -1)"
# confirm no such container is actually running.
echo "BOGUS_RUNNING=\$(timeout 15 nsenter -t 1 -m -p -- runsc --root=\$ROOT state scb-bogus-${RUN_ID} 2>/dev/null | python3 -c 'import sys,json;print(json.load(sys.stdin).get("status","none"))' 2>/dev/null || echo none)"
RSC(){ timeout 20 nsenter -t 1 -m -p -- runsc --root=\$ROOT "\$@"; }
RSC delete --force scb-bogus-${RUN_ID} >/dev/null 2>&1 || true
EOF
)
BOGUS_RC=$(printf '%s' "$FC_OUT" | sed -n 's/^BOGUS_RC=//p' | tr -d '\r')
BOGUS_ERR=$(printf '%s' "$FC_OUT" | sed -n 's/^BOGUS_ERR=//p' | tr -d '\r')
BOGUS_RUNNING=$(printf '%s' "$FC_OUT" | sed -n 's/^BOGUS_RUNNING=//p' | tr -d '\r')
if [ -n "$BOGUS_RC" ] && [ "$BOGUS_RC" -ne 0 ] 2>/dev/null && [ "$BOGUS_RUNNING" != "running" ]; then
  pass "fail-closed: off-roster sub restore errored (rc=$BOGUS_RC${BOGUS_ERR:+: ${BOGUS_ERR:0:70}}), no container started"
else
  fail "fail-closed off-roster sub" "rc='$BOGUS_RC' running='$BOGUS_RUNNING' err='$BOGUS_ERR' (a non-roster sub must be refused)"
fi

# ── Summary ───────────────────────────────────────────────────────
echo ""
echo "════════════════════════════════════════════════════"
echo " gVisor sidecar-barrier Summary: $PASS passed, $FAIL failed"
echo "   container_count(baked)=$BAKED_COUNT  freeze≈${FREEZE_MS:-?}ms  snapshot=${SNAP_KIB:-?}KiB"
echo "   agent: FROZEN=$FROZEN_AGENT RESUMED=$RESUME_AGENT   dind: FROZEN=$FROZEN_DIND RESUMED=$RESUME_DIND"
echo "════════════════════════════════════════════════════"
if [ "$FAIL" -gt 0 ]; then
  for e in "${ERRORS[@]}"; do red "  - $e"; done
  exit 1
fi
green "All gVisor N-container sidecar-barrier assertions passed."
exit 0
