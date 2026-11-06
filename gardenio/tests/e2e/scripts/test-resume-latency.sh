#!/usr/bin/env bash
# test-resume-latency.sh — STANDING TIMED regression gate for <1s gVisor agent resume.
# Task ka-0000006324 (C7), epic ka-0000002590 ("Phase 5: <1s agent resume").
# Operator: Matthew Baker (mandate, thread 1780804147.560279).
#
# WHAT THIS GUARDS (forever, on every gVisor e2e dispatch):
#   The R4 thaw-onto-WARM-placeholder-B resume path stays sub-second. The
#   overnight convergence benchmark (ka-0000003577, bench-resume.sh) PROVED the
#   number once; this module is the permanent CI gate that FAILS the suite if the
#   number ever regresses past the SLO. A green run here means a real runsc agent
#   still wakes in <1s on the warm-B path AND strictly faster than a fresh-Pod-B
#   control.
#
# RESUME LATENCY (measured EXACTLY as the benchmark defines it):
#   resume_latency = t(agent responsive) − t(wake request)
#   For the warm-B (R4) path the wake request lands on an ALREADY-Ready
#   placeholder pod B, so the measured cost is the `runsc restore` into B's
#   kubelet-provisioned netns (proof-of-life: the restored single-container agent
#   re-schedules and its in-RAM counter climbs past the freeze value, the
#   self-contained analogue of coop-ready / first NATS pong). This is the same
#   THAW_MS the benchmark's node driver records for R4.
#   For the fresh-B (R3) CONTROL the wake request must FIRST create placeholder
#   pod B and wait for it to reach Ready (kubelet admit + CNI netns + volume +
#   token), THEN restore — so its latency includes the multi-second pod-admission
#   cost the warm-B pool removes. resume_latency(R3) = create→Ready wall-clock +
#   THAW_MS.
#
# THE GATE (non-zero exit ⇒ red suite):
#   p50(R4 warm-B) < GATE_P50_MS (default 1000ms)   over N≥5 in-test resumes
#   AND  p50(R4 warm-B) STRICTLY < p50(R3 fresh-B)   (proves the pool removes the
#                                                     Pod-B Ready wait)
#   When REQUIRE_REPO=1 (set once C6 ka-0000007582 lands the repo onto the
#   placeholder) the gate ALSO refuses to pass unless the warm-B resumes report
#   repo_present=1 — a sub-1s number on an empty workspace does not count.
#
# TIMED EMISSION:
#   Prints `resume_latency_ms regime=R4 path=thaw-warm run=<i> value=<ms> repo_present=<0|1>`
#   per resume to the task log (ingestible by the e2e flake/timing report) AND
#   writes a Prometheus textfile (gardenio_resume_latency_seconds histogram, same
#   name+buckets+labels as controller/internal/metrics/resume_latency.go and
#   bench-resume.sh) so the ka-0000003577 dashboard histogram can ingest the
#   standing-CI samples alongside harness + production ones.
#
# REUSES THE PROVEN NODE SCRIPTS (no reinvention — tracks the code the benchmark
# proved):
#   - gardenio/tests/e2e/scripts/gvisor-node/single-container-resume-node.sh
#       builds the single-container ROOT-agent bundle (fix path (c), no mounts-fd
#       sub-container handoff) + `runsc checkpoint` → /var/tmp/ckSC + /var/tmp/sc.
#   - gardenio/tests/e2e/scripts/gvisor-node/single-container-thaw-into-b-node.sh
#       `runsc restore` that image into placeholder Pod B's kubelet-owned netns;
#       prints THAW_MS (restore-only, the R4 number) + repo_present once C6 lands.
#
# OPT-IN / GATING (NEVER silently passes — see scope item 3 of ka-0000006324):
#   This module targets the dedicated gvisor-poc namespace + runsc node pool, NOT
#   the per-run e2e namespace. It runs only when GVISOR_E2E=1 (or
#   RESUME_LATENCY_E2E=1) AND the substrate is present (runsc RuntimeClass +
#   create-pods in $GVISOR_NS). When the opt-in flag is unset it SKIPs cleanly
#   (exit 0) like the sibling gvisor modules. When the opt-in flag IS set but the
#   substrate is MISSING it SKIPs with a LOUD warning (still exit 0 so an absent
#   node pool does not red the suite) — it must never look like it asserted when
#   it did not.
#
# Prerequisites (all satisfied by escalation ka-0000004674):
#   - kubectl context on america-e2e-eks
#   - ns gvisor-poc (PSA=privileged), RuntimeClass runsc, NodePool gvisor-poc
#   - SA gardenio-agent: create pods + pods/exec in gvisor-poc
#
# Usage:
#   GVISOR_E2E=1 ./gardenio/tests/e2e/scripts/test-resume-latency.sh
#   GVISOR_E2E=1 N_RESUMES=8 GATE_P50_MS=1000 KEEP_PODS=1 ./...      # debug
#   GVISOR_E2E=1 REQUIRE_REPO=1 ./...                                # post-C6
set -uo pipefail

# Shared gVisor substrate helpers — incl. gvisor_require_substrate_guard, the
# readiness-gate decision for a missing substrate (REQUIRE_SUBSTRATE=1 → hard
# fail; default → clean skip). Task ka-0000002867, lib decision ka-0000000122.
# shellcheck disable=SC1091
source "$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)/lib-gvisor-node.sh"

# ── Config (all overridable via env) ───────────────────────────────
GVISOR_NS="${GVISOR_NS:-gvisor-poc}"
RUNTIME_CLASS="${RUNTIME_CLASS:-runsc}"
RUN_ID="${RUN_ID:-$$}"
PROBE="${PROBE:-resume-lat-probe-${RUN_ID}}"
# Number of timed resumes per path. N≥5 per the C7 mandate; warm-B is the gated
# set, fresh-B the control. Default 5; bump for tighter tails.
N_RESUMES="${N_RESUMES:-5}"
# SLO gate (ms). The R4 warm-B p50 must be strictly below this AND below the
# fresh-B p50.
GATE_P50_MS="${GATE_P50_MS:-1000}"
# C6 scope gate: refuse to pass unless warm-B resumes report repo_present=1.
# Default off until ka-0000007582 lands the repo onto the placeholder.
REQUIRE_REPO="${REQUIRE_REPO:-0}"
# Where the proven node scripts live (shipped to the node via base64).
POC_DIR="${POC_DIR:-gardenio/tests/e2e/scripts/gvisor-node}"
# Prometheus textfile out (histogram exposition for the dashboard).
PROM_TEXTFILE="${PROM_TEXTFILE:-${PROM_TEXTFILE_DIR:-/tmp}/resume_latency_e2e_${RUN_ID}.prom}"
ROLE="${ROLE:-crew}"
RUNTIME="${RUNTIME:-runsc}"
BUSYBOX_IMAGE="${BUSYBOX_IMAGE:-public.ecr.aws/docker/library/python:3.11-slim}"
# Per-pod Ready timeout — kubelet admit budget for the placeholder pods.
POD_READY_TIMEOUT="${POD_READY_TIMEOUT:-180s}"

PASS=0
FAIL=0
ERRORS=()
CREATED_PODS=()

# ── Helpers ────────────────────────────────────────────────────────
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
# pool must NOT red the suite) but print a banner so a skip is never mistaken for
# a pass. The "$2" tag distinguishes "opt-in off" (inert, expected in standard
# shards) from "substrate missing" (loud — the gated dispatch expected it).
skip_all() {
  local why="$1" tag="${2:-substrate}"
  # "loud" == opt-in was SET but the substrate is absent. Defer to the shared
  # readiness guard: hard-fail under REQUIRE_SUBSTRATE=1 (the go/no-go), loud
  # skip otherwise (task ka-0000002867). "off" == opt-in unset: inert clean skip.
  if [ "$tag" = "loud" ]; then
    gvisor_require_substrate_guard resume-latency "$why"
  else
    blue "SKIP resume-latency: $why"
    dim  "  Opt-in module: set GVISOR_E2E=1 (or RESUME_LATENCY_E2E=1) and run on the"
    dim  "  gvisor-poc substrate (runsc RuntimeClass + create-pods in ns $GVISOR_NS)."
    if [ -n "${E2E_SKIP_SENTINEL:-}" ]; then printf '%s\n' "$*" >"$E2E_SKIP_SENTINEL" 2>/dev/null || true; fi  # skip visibility (ka-0000000989)
    exit 0
  fi
}

# SUBSTRATE-NOT-READY skip — the gvisor-poc RuntimeClass + create-pods preflight
# PASSED (so this is NOT an absent node pool) but the checkpoint-build substrate
# could not be assembled on the scheduled node: the agent OCI rootfs failed to
# build (overlay fast-path AND registry pull fallback both failed), or the
# checkpoint artifacts never materialized. This is an ENVIRONMENTAL fault — a
# node/registry hiccup — NOT an app regression. We exit 0 INCONCLUSIVE rather
# than red the C7 golden gate, so a single node hiccup cannot masquerade as a
# real <1s-resume regression. The release-owner gardener golden-gates on this as
# non-blocking (SKIP-vs-FAIL). See ka-0000001111 / gd-LlQWNUyA8J.
skip_substrate() {
  local why="$1" detail="${2:-}"
  # Under the READINESS GATE (REQUIRE_SUBSTRATE=1) the gate's job is to prove the
  # substrate stands up AND a checkpoint can be built on it — so an unbuildable
  # checkpoint substrate is itself a NO-GO, not an absorbed environmental flake
  # (task ka-0000002867). Outside the gate, keep the long-standing non-blocking
  # SUBSTRATE-NOT-READY behaviour (ka-0000001111 / gd-LlQWNUyA8J).
  if [ "${REQUIRE_SUBSTRATE:-0}" = "1" ]; then
    red   "════════════════════════════════════════════════════"
    red   " NO-GO resume-latency (REQUIRE_SUBSTRATE=1, SUBSTRATE-NOT-READY): $why"
    red   " The readiness gate could not assemble the checkpoint-build substrate"
    red   " on the gvisor-poc node (rootfs / checkpoint image build failed). The"
    red   " gate must PROVE the substrate works end-to-end — an unbuildable"
    red   " substrate is a NO-GO. Do NOT flip FreezeThawEnabled on this signal."
    [ -n "$detail" ] && dim "    $detail"
    red   "════════════════════════════════════════════════════"
    exit 1
  fi
  red   "════════════════════════════════════════════════════"
  red   " SKIP resume-latency (SUBSTRATE-NOT-READY): $why"
  red   " The runsc substrate PASSED preflight but the checkpoint-build substrate"
  red   " could not be assembled on the scheduled node (rootfs / checkpoint image"
  red   " build failed). This is an ENVIRONMENTAL node/registry fault, NOT an app"
  red   " regression — this run did NOT assert the <1s resume SLO, and it must NOT"
  red   " be counted as a regression failure. Re-dispatch on a healthy gvisor-poc"
  red   " node. Golden-gating treats SUBSTRATE-NOT-READY as non-blocking."
  [ -n "$detail" ] && dim "    $detail"
  red   "════════════════════════════════════════════════════"
  if [ -n "${E2E_SKIP_SENTINEL:-}" ]; then printf '%s\n' "$*" >"$E2E_SKIP_SENTINEL" 2>/dev/null || true; fi  # skip visibility (ka-0000000989)
  exit 0
}

k() { kubectl -n "$GVISOR_NS" "$@"; }

# Per-node-call wall-clock cap. The proven node scripts (and runsc itself) can
# HANG on process exit after checkpoint/restore — the documented quirk in
# test-gvisor-freeze-thaw.sh. Without a cap a single hung `runsc state`/`ps`
# would block the harness forever (observed: a 13min+ stall on run 3). So every
# node-side `k exec` is wrapped on BOTH sides: a client-side `timeout` so the
# harness moves on, AND a node-side `timeout` (passed into the chroot) so the
# stuck runsc process is actually killed on the node, not just detached from.
NODE_CALL_TIMEOUT="${NODE_CALL_TIMEOUT:-120}"
# node_exec <cap_sec> <bash -c string> — run the command on the node (host chroot)
# with the call wrapped in `timeout` at BOTH the container level (`timeout` runs
# inside the probe container, killing a hung chroot) and the client level (the
# outer `timeout` so kubectl exec itself can't block the harness). The command
# string is passed as a single argv element to `bash -c` — kubectl exec execs
# argv directly (no intervening shell), so no extra quoting is needed. The real
# anti-hang guarantee is the in-script RUNSC_TIMEOUT cap on every `runsc` call in
# the proven node scripts; this wrapper is the backstop that unblocks the harness.
node_exec() {
  local cap="$1"; shift
  # Call kubectl DIRECTLY here, not the k() shell function: `timeout` execs its
  # first argument as a binary, and a shell function is not a binary (it fails
  # with "timeout: failed to run command 'k'", rc 127). The node-side `timeout`
  # caps the chroot; the client-side `timeout` caps kubectl exec itself.
  timeout "$((cap + 20))" kubectl -n "$GVISOR_NS" exec "$PROBE" -- timeout "$cap" chroot /host bash -c "$1" 2>/dev/null
}

# nearest-rank p50 of a whitespace/newline list of integers.
p50() {
  printf '%s\n' "$@" | tr ' ' '\n' | grep -E '^[0-9]+$' | sort -n | awk '
    { v[n++]=$1 }
    END { if(n==0){print ""; exit} r=int((50*n+99)/100); if(r<1)r=1; print v[r-1] }'
}

cleanup() {
  blue "── cleanup ──"
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
echo " <1s gVisor agent resume — STANDING TIMED regression gate (C7)"
echo " ns=$GVISOR_NS runtimeClass=$RUNTIME_CLASS N=$N_RESUMES gate_p50=${GATE_P50_MS}ms require_repo=$REQUIRE_REPO"
echo "════════════════════════════════════════════════════"

# ── Gate 0: opt-in + substrate preflight ───────────────────────────
if [ "${GVISOR_E2E:-0}" != "1" ] && [ "${RESUME_LATENCY_E2E:-0}" != "1" ]; then
  skip_all "GVISOR_E2E / RESUME_LATENCY_E2E != 1 (opt-in module)" off
fi
command -v kubectl >/dev/null 2>&1 || skip_all "kubectl not installed" loud
kubectl get runtimeclass "$RUNTIME_CLASS" >/dev/null 2>&1 \
  || skip_all "RuntimeClass '$RUNTIME_CLASS' not present on this cluster" loud
[ "$(kubectl auth can-i create pods -n "$GVISOR_NS" 2>/dev/null)" = "yes" ] \
  || skip_all "cannot create pods in ns '$GVISOR_NS'" loud
[ -f "$POC_DIR/single-container-resume-node.sh" ] \
  || skip_all "node script single-container-resume-node.sh missing under $POC_DIR" loud
[ -f "$POC_DIR/single-container-thaw-into-b-node.sh" ] \
  || skip_all "node script single-container-thaw-into-b-node.sh missing under $POC_DIR" loud

blue "Substrate present — running standing resume-latency assertions."

# ── 1. Schedule the privileged probe + ship the proven node scripts ─
blue "[1/5] scheduling privileged probe '$PROBE' onto the gvisor-poc node"
cat <<YAML | k apply -f - >/dev/null 2>&1
apiVersion: v1
kind: Pod
metadata:
  name: ${PROBE}
  namespace: ${GVISOR_NS}
  labels: {app: resume-lat-probe, test: ka-0000006324}
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

# Gate on the gvisor-poc node being genuinely WARM before the warm-B pool build +
# node-asset ship — via the SHARED hardening gate (lib-gvisor-node.sh
# gvisor_warm_node_gate; decision ka-0000000122 chosen=harden, root flake
# ka-0000007234). A freshly Karpenter-provisioned node reports its probe Ready
# while runsc + /var/tmp + the runsc state root are still settling — the rotating
# node-asset-ship / restore cold-start cause. This module SCHEDULES a k8s runsc
# pod (runtimeClassName: runsc), so the default warm gate — incl. the k8s
# containerd state root check (c) — applies (no GVISOR_WARM_REQUIRE_ROOT override).
# node_exec is the 2-arg (cap, cmd) runner the gate expects directly. On a node
# that never warms (a true substrate regression) the gate HARD-FAILS with diag.
if gvisor_warm_node_gate node_exec; then
  pass "gvisor-poc node warm (runsc present + /var/tmp writable + runsc-root reachable)"
else
  fail "gvisor-poc node warm-gate" "$GVISOR_WARM_DIAG"
  echo "════════ Summary: $PASS passed, $FAIL failed ════════"
  exit 1
fi

# Ship both proven node scripts (base64 to dodge exec stdin/quoting pitfalls).
# Wrapped in a bounded, signature-keyed retry: a single cold-start base64-copy
# miss to a not-yet-settled /var/tmp self-heals instead of red-ing the dedicated
# dispatch (the ka-0000007234 node-asset-ship signature). On exhaustion it still
# HARD-FAILS — a genuinely broken copy is NOT masked.
ship_node_scripts() {
  for s in single-container-resume-node.sh single-container-thaw-into-b-node.sh; do
    local b64; b64="$(base64 -w0 "$POC_DIR/$s")"
    k exec "$PROBE" -- chroot /host bash -c \
      "echo $b64 | base64 -d > /var/tmp/$s && chmod +x /var/tmp/$s" >/dev/null 2>&1 || return 1
  done
}
if gvisor_retry_step node-asset-ship 4 3 ship_node_scripts; then
  pass "proven node scripts shipped to the node (attempts=$GVISOR_STEP_ATTEMPTS)"
else
  fail "ship node scripts" "$GVISOR_STEP_DIAG"
fi

# ── 2. Build the single-container agent bundle + checkpoint (R4 image) ─
# Reuse the PROVEN single-container-resume-node.sh: it assembles the ROOT-agent
# bundle (fix path (c)) and `runsc checkpoint`s it to /var/tmp/ckSC + leaves the
# /var/tmp/sc bundle in place — exactly the inputs single-container-thaw-into-b
# consumes. We need ONLY its checkpoint artifacts, NOT its own restore self-proof:
# that script's final `runsc restore` is known to hang on process EXIT (the runsc
# checkpoint/restore quirk documented in test-gvisor-freeze-thaw.sh) and is not
# wrapped in `timeout` there, so we cap the whole invocation and IGNORE its rc.
# Success is asserted from OBSERVED STATE — the checkpoint images on disk — not
# from the script's exit code (which the hang/cap would poison). This is the same
# "assert by observed state, ignore runsc rc" discipline the freeze-thaw module
# uses.
blue "[2/5] building single-container agent bundle + checkpoint (/var/tmp/ckSC)"
# CHECKPOINT_ONLY=1: build the bundle + write the checkpoint, then STOP. We do NOT
# want the node script's own self-proof restore (steps 4-5) — that is a `--detach`
# restore captured in a command substitution, which blocks on the detached
# daemon's stdout pipe (the runsc exit quirk) and would wedge this exec. We only
# need the checkpoint artifacts; the timed restores happen in steps 3-4 below.
BUILD_OUT=$(node_exec 240 "CHECKPOINT_ONLY=1 /var/tmp/single-container-resume-node.sh 2>&1; true")
# Assert the checkpoint images exist (checkpoint.img + pages.img) AND the bundle
# config the thaw step repoints is present — the actual inputs, not a log line.
HAVE=$(node_exec 30 'if [ -f /var/tmp/ckSC/checkpoint.img ] && [ -f /var/tmp/ckSC/pages.img ] && [ -f /var/tmp/sc/config.json ]; then echo OK; else echo MISSING; fi' | tr -d '\r')
if [ "$HAVE" = OK ]; then
  ck_kib=$(node_exec 30 'du -sk /var/tmp/ckSC 2>/dev/null | awk "{print \$1}"' | tr -d '\r')
  pass "checkpoint image + bundle built (/var/tmp/ckSC ${ck_kib:-?}KiB + /var/tmp/sc/config.json)"
elif grep -q 'SUBSTRATE-NOT-READY' <<<"$BUILD_OUT"; then
  # The node script could not assemble the rootfs (overlay + registry pull both
  # failed) — an environmental fault, not an app regression. Skip inconclusive
  # instead of reding the gate. The node emits its own SUBSTRATE-NOT-READY line.
  skip_substrate "node could not assemble the checkpoint-build substrate" \
    "$(printf '%s\n' "$BUILD_OUT" | grep 'SUBSTRATE-NOT-READY' | head -1 | sed 's/^[[:space:]]*//')"
else
  fail "build checkpoint image" "checkpoint artifacts absent on node (ckSC/checkpoint.img+pages.img or sc/config.json missing): $(printf '%s\n' "$BUILD_OUT" | tail -6)"
  echo "════════ Summary: $PASS passed, $FAIL failed ════════"
  exit 1
fi

# Node-side resolver: given a placeholder pod UID, print "B_SENTRY_PID B_NETNS".
# Mirrors restore-through-lifecycle.sh — find the runsc sandbox cid whose
# sandbox-uid == the pod UID, read its sentry pid, and use /proc/<pid>/ns/net as
# B's kubelet-owned netns path (runsc restore accepts a /proc/PID/ns/net path).
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

# Run ONE thaw-into-B against an already-resolved placeholder. Echoes
# "<thaw_ms> <success 0|1> <repo_present 0|1>" (THAW_MS = restore-only cost).
# Capped via node_exec so a hung restore/state degrades to a failed resume (0 0)
# rather than blocking the harness.
thaw_into() {
  local bpid="$1" bnetns="$2" out ms repo
  out="$(node_exec "$NODE_CALL_TIMEOUT" \
    "B_NETNS='$bnetns' B_SENTRY_PID='$bpid' /var/tmp/single-container-thaw-into-b-node.sh 2>&1")"
  ms="$(printf '%s\n' "$out" | sed -n 's/.*THAW_MS=\([0-9][0-9]*\).*/\1/p' | head -1)"
  repo=0; grep -q 'repo_present=1' <<<"$out" && repo=1
  if [ -n "$ms" ]; then echo "$ms 1 $repo"; else echo "0 0 $repo"; fi
}

# Create a placeholder Pod B (the warm-B / fresh-B target) and return its name.
# Same shape as placeholder-pod-b.yaml: real kubelet runsc pod, emptyDir /work,
# default SA token — so the restored agent's mount/netns expectations line up.
make_placeholder() {
  local name="$1"
  cat <<YAML | k apply -f - >/dev/null 2>&1
apiVersion: v1
kind: Pod
metadata:
  name: ${name}
  namespace: ${GVISOR_NS}
  labels: {app: resume-lat-placeholder, test: ka-0000006324, role: restore-placeholder}
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
  CREATED_PODS+=("$name")
}

# ── 3. Warm-B (R4): pre-Ready pool → timed restore-only resumes ─────
# Build the warm pool FIRST (kubelet admit happens off the hot path), wait all
# Ready, THEN time N restores. The measured latency excludes pod admission —
# that is the whole point of the warm-B pool.
blue "[3/5] warm-B (R4): pre-warming a pool of $N_RESUMES Ready placeholder pods"
warm_pods=()
for i in $(seq 1 "$N_RESUMES"); do
  nm="resume-lat-warmb-${RUN_ID}-${i}"
  make_placeholder "$nm"
  warm_pods+=("$nm")
done
warm_ready=1
for nm in "${warm_pods[@]}"; do
  k wait --for=condition=Ready "pod/$nm" --timeout="$POD_READY_TIMEOUT" >/dev/null 2>&1 || warm_ready=0
done
[ "$warm_ready" = 1 ] && pass "warm-B pool of $N_RESUMES placeholders Ready (kubelet admit done OFF the hot path)" \
                      || fail "warm-B pool Ready" "not all $N_RESUMES placeholders reached Ready in $POD_READY_TIMEOUT"

blue "    timing $N_RESUMES warm-B resumes (restore into an already-Ready B)"
warm_samples=()
warm_repo_all=1
warm_ok=0
i=0
for nm in "${warm_pods[@]}"; do
  i=$((i + 1))
  uid=$(k get pod "$nm" -o jsonpath='{.metadata.uid}' 2>/dev/null)
  read -r bpid bnetns <<<"$(resolve_b "$uid")"
  if [ -z "${bpid:-}" ]; then
    echo "resume_latency_ms regime=R4 path=thaw-warm run=$i value=NA repo_present=0 (resolve failed for $nm)"
    continue
  fi
  read -r ms ok repo <<<"$(thaw_into "$bpid" "$bnetns")"
  echo "resume_latency_ms regime=R4 path=thaw-warm run=$i value=${ms} repo_present=${repo}"
  if [ "$ok" = 1 ]; then warm_samples+=("$ms"); warm_ok=$((warm_ok + 1)); [ "$repo" = 0 ] && warm_repo_all=0; fi
done
if [ "$warm_ok" -ge "$N_RESUMES" ]; then
  pass "warm-B resumes: $warm_ok/$N_RESUMES succeeded"
else
  fail "warm-B resumes succeeded" "only $warm_ok/$N_RESUMES warm-B restores succeeded"
fi

# ── 4. Fresh-B (R3) CONTROL: create→Ready→restore, full wall-clock ──
blue "[4/5] fresh-B (R3) control: $N_RESUMES resumes that PAY the Pod-B Ready wait"
fresh_samples=()
fresh_ok=0
for i in $(seq 1 "$N_RESUMES"); do
  nm="resume-lat-freshb-${RUN_ID}-${i}"
  t0=$(date +%s%3N)
  make_placeholder "$nm"
  if ! k wait --for=condition=Ready "pod/$nm" --timeout="$POD_READY_TIMEOUT" >/dev/null 2>&1; then
    echo "resume_latency_ms regime=R3 path=thaw-fresh run=$i value=NA (pod $nm not Ready)"
    continue
  fi
  uid=$(k get pod "$nm" -o jsonpath='{.metadata.uid}' 2>/dev/null)
  read -r bpid bnetns <<<"$(resolve_b "$uid")"
  if [ -z "${bpid:-}" ]; then
    echo "resume_latency_ms regime=R3 path=thaw-fresh run=$i value=NA (resolve failed)"
    continue
  fi
  read -r ms ok _repo <<<"$(thaw_into "$bpid" "$bnetns")"
  t1=$(date +%s%3N)
  if [ "$ok" = 1 ]; then
    total=$((t1 - t0))   # create→Ready→restore wall-clock = the R3 cost
    fresh_samples+=("$total")
    fresh_ok=$((fresh_ok + 1))
    echo "resume_latency_ms regime=R3 path=thaw-fresh run=$i value=${total} (restore=${ms}ms + pod-admit)"
  else
    echo "resume_latency_ms regime=R3 path=thaw-fresh run=$i value=NA (restore failed)"
  fi
done
[ "$fresh_ok" -ge 1 ] && pass "fresh-B control resumes: $fresh_ok/$N_RESUMES succeeded" \
                      || fail "fresh-B control resumes" "no fresh-B control restores succeeded — cannot form the R3 baseline"

# ── 5. GATE + timed emission ───────────────────────────────────────
blue "[5/5] evaluating the <1s SLO gate + emitting timed samples"
WARM_P50=$(p50 "${warm_samples[@]:-}")
FRESH_P50=$(p50 "${fresh_samples[@]:-}")
echo "    R4 warm-B  p50 = ${WARM_P50:-NA}ms  (n=${#warm_samples[@]}, samples: ${warm_samples[*]:-none})"
echo "    R3 fresh-B p50 = ${FRESH_P50:-NA}ms (n=${#fresh_samples[@]}, samples: ${fresh_samples[*]:-none})"

# Emit the Prometheus histogram textfile (same name/buckets/labels as
# controller/internal/metrics/resume_latency.go) so the ka-0000003577 dashboard
# can ingest these standing-CI samples. Values are SECONDS (Prom convention).
emit_prom() {
  {
    echo "# HELP gardenio_resume_latency_seconds Agent resume latency (standing e2e gate, task ka-0000006324)."
    echo "# TYPE gardenio_resume_latency_seconds histogram"
    printf '%s\n' "R4 thaw-warm ${warm_samples[*]:-}" "R3 thaw-fresh ${fresh_samples[*]:-}" | awk -v role="$ROLE" -v rt="$RUNTIME" '
      BEGIN { nb=split("0.25 0.5 0.75 1 1.25 1.5 2 3 5 10 30 60 120 300", B, " ") }
      {
        regime=$1; path=$2; cnt=0; sum=0; for(i=1;i<=nb;i++) bk[i]=0
        for(j=3;j<=NF;j++){ if($j !~ /^[0-9]+$/) continue; s=$j/1000.0; cnt++; sum+=s
          for(i=1;i<=nb;i++) if(s<=B[i]+0) bk[i]++ }
        if(cnt==0) next
        lbl=sprintf("regime=\"%s\",role=\"%s\",runtime=\"%s\",path=\"%s\",env=\"e2e\"", regime, role, rt, path)
        for(i=1;i<=nb;i++) printf "gardenio_resume_latency_seconds_bucket{%s,le=\"%s\"} %d\n", lbl, B[i], bk[i]
        printf "gardenio_resume_latency_seconds_bucket{%s,le=\"+Inf\"} %d\n", lbl, cnt
        printf "gardenio_resume_latency_seconds_sum{%s} %.6f\n", lbl, sum
        printf "gardenio_resume_latency_seconds_count{%s} %d\n", lbl, cnt
      }'
  } > "$PROM_TEXTFILE" 2>/dev/null && dim "    wrote histogram textfile: $PROM_TEXTFILE"
}
emit_prom

# Hard gate 1 — R4 warm-B p50 strictly below the SLO.
if [ -n "$WARM_P50" ] && [ "$WARM_P50" -lt "$GATE_P50_MS" ] 2>/dev/null; then
  pass "R4 warm-B p50 ${WARM_P50}ms < SLO ${GATE_P50_MS}ms (sub-second resume holds)"
else
  fail "R4 warm-B p50 < SLO" "p50='${WARM_P50:-NA}ms' not < ${GATE_P50_MS}ms — sub-second resume REGRESSED"
fi

# Hard gate 2 — R4 warm-B strictly faster than the R3 fresh-B control (proves
# the warm pool actually removed the Pod-B Ready wait, not just a fast node).
if [ -n "$WARM_P50" ] && [ -n "$FRESH_P50" ] && [ "$WARM_P50" -lt "$FRESH_P50" ] 2>/dev/null; then
  pass "R4 warm-B p50 ${WARM_P50}ms STRICTLY < R3 fresh-B p50 ${FRESH_P50}ms (pool removes pod-admit)"
else
  fail "R4 warm-B faster than R3 control" "warm p50='${WARM_P50:-NA}' not < fresh p50='${FRESH_P50:-NA}' — warm-B pool gives no speedup"
fi

# Hard gate 3 — C6 scope: warm-B resumes must carry the repo (post-ka-0000007582).
if [ "$REQUIRE_REPO" = 1 ]; then
  if [ "${#warm_samples[@]}" -gt 0 ] && [ "$warm_repo_all" = 1 ]; then
    pass "REQUIRE_REPO=1 satisfied: all warm-B resumes reported repo_present=1"
  else
    fail "REQUIRE_REPO=1 repo present" "a warm-B resume reported repo_present=0 — a sub-1s number on an empty workspace does not count (C6 ka-0000007582)"
  fi
else
  dim  "    REQUIRE_REPO=0 — repo-present not gated yet (flip to 1 once C6 ka-0000007582 lands the repo onto the placeholder)"
fi

# ── Summary ────────────────────────────────────────────────────────
echo ""
echo "════════════════════════════════════════════════════"
echo " resume-latency gate Summary: $PASS passed, $FAIL failed"
echo "   R4 warm-B p50=${WARM_P50:-NA}ms  R3 fresh-B p50=${FRESH_P50:-NA}ms  SLO=${GATE_P50_MS}ms"
echo "════════════════════════════════════════════════════"
if [ "$FAIL" -gt 0 ]; then
  for e in "${ERRORS[@]}"; do red "  - $e"; done
  exit 1
fi
green "All resume-latency assertions passed — <1s warm-B resume SLO holds."
exit 0
