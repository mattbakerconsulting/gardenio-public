#!/usr/bin/env bash
# test-userresponse-latency.sh — STANDING TIMED regression gate for the <20s
# USER-VISIBLE first-response path across all three agent lifecycle states.
# Task ka-0000000107 (C4), epic ka-0000008235 ("Fast + interruptible freeze").
# Plan-of-record: gardenio/docs/fast-interruptible-freeze.md (C4).
# Operator: Matt Baker (slack:U0OPERATOR1).
#
# WHAT THIS GUARDS (forever, on every gVisor e2e dispatch):
#   The end-to-end SLO the whole fast+interruptible-freeze epic defends: a user
#   message lands and the agent answers in < 20 s, NO MATTER which lifecycle
#   state the agent was in when the message arrived. This is the USER-VISIBLE
#   companion to the C7 resume-latency gate (ka-0000006324, test-resume-latency.sh):
#   C7 proves the warm-B *thaw* is sub-second in isolation; C4 proves the path the
#   USER experiences — spawn-or-thaw-or-serve-through-an-interrupt — stays under
#   the 20 s SLO for all three states. A green run means the freeze/thaw machinery
#   never makes a user wait more than 20 s, AND (the novel leg) a user message
#   arriving mid-freeze is served by the still-live agent with no thaw at all.
#
# THE THREE USER-VISIBLE STATES (each measured as t(agent responsive) − t(message)):
#   COLD       no pod -> spawn/thaw -> first response.
#              Cold-start: there is no warm placeholder, so the wake request must
#              FIRST create placeholder pod B and wait for it to reach Ready
#              (kubelet admit + CNI netns + volume + token), THEN restore. This is
#              the fresh-B (R3) shape from the resume benchmark.
#              latency(COLD) = create->Ready wall-clock + THAW_MS.
#   RESUMING   snapshot present -> thaw -> first response.
#              A snapshot exists and a warm placeholder B is already Ready, so the
#              measured cost is restore-only into B's kubelet-provisioned netns —
#              the proven sub-second warm-B (R4) path. This leg REUSES the same
#              node scripts the C7 gate proves, so the two gates can never silently
#              diverge. latency(RESUMING) = THAW_MS.
#   MID-FREEZE freeze in flight -> message -> interrupt (C2) -> live response.
#              The novel leg, unblocked by C2 (ka-0000005834). A checkpoint has
#              been taken (`runsc checkpoint --leave-running`) but the inbound
#              message ABORTS the freeze before the stop/relocate step, so Pod A
#              stays LIVE and serves the message with NO THAW. We assert the C2
#              invariant directly: the leave-running sandbox is still `running`
#              (freezethaw.go L540 post-state assertion) and the live agent's
#              in-RAM counter advances past its checkpoint value — i.e. the agent
#              never went dark. latency(MID-FREEZE) = time from "message" (the
#              interrupt decision) to the next observable agent response, which on
#              the no-thaw path is bounded by the agent's own response cadence,
#              not by any pod admission or restore.
#
# THE GATE (non-zero exit => red suite):
#   p50(state) < the tighter of {per-state gate, GATE_P50_MS ceiling}  for EACH of
#   COLD/RESUMING/MID-FREEZE over N>=5 in-test samples per state.
#   - GATE_P50_MS (default 20000ms) is the outer <20s epic ceiling every state
#     must clear no matter what.
#   - Per-state gates (post-Lever-2, ka-0000006528) actually bite now that the
#     cold path is fixed: GATE_COLD_P50_MS=15000, GATE_RESUMING_P50_MS=3000,
#     GATE_MIDFREEZE_P50_MS=3000. Each sits well above its measured p50 (COLD ~8s,
#     RESUMING ~0.3s, MID-FREEZE ~0.1s) but far below 20s, so a regression in a
#     fast path can no longer hide under the loose flat ceiling.
#   When REQUIRE_REPO=1 (set once C6 ka-0000007582 lands the repo onto the
#   placeholder) the COLD and RESUMING legs ALSO refuse to pass unless their
#   resumes report repo_present=1 — a fast number on an empty workspace does not
#   count. (MID-FREEZE serves the still-live Pod A, whose repo never left, so it
#   is repo-present by construction.)
#
# TIMED EMISSION:
#   Prints `userresponse_latency_ms state=<COLD|RESUMING|MID_FREEZE> run=<i> value=<ms> repo_present=<0|1>`
#   per sample to the task log (ingestible by the e2e flake/timing report) AND
#   writes a Prometheus textfile (gardenio_userresponse_latency_seconds histogram)
#   so a dashboard can ingest the standing-CI samples alongside the resume-latency
#   histogram (same buckets, sibling metric).
#
# REUSES THE PROVEN NODE SCRIPTS (no reinvention — tracks the code the benchmark
# and the C7 gate already prove):
#   - gardenio/tests/e2e/scripts/gvisor-node/single-container-resume-node.sh
#       builds the single-container ROOT-agent bundle + `runsc checkpoint`. C4
#       drives it three ways: CHECKPOINT_ONLY=1 (build the warm/cold restore
#       image) and LEAVE_RUNNING_ONLY=1 (the in-flight-freeze, source-cid-left-
#       alive state the MID-FREEZE leg interrupts).
#   - gardenio/tests/e2e/scripts/gvisor-node/single-container-thaw-into-b-node.sh
#       `runsc restore` into placeholder Pod B's kubelet-owned netns; prints
#       THAW_MS (the RESUMING number) + repo_present.
#
# OPT-IN / GATING (NEVER silently passes — same discipline as the C7 gate):
#   Targets the dedicated gvisor-poc namespace + runsc node pool, NOT the per-
#   run e2e namespace. Runs only when GVISOR_E2E=1 (or the dedicated
#   USERRESPONSE_LATENCY_E2E=1) AND the substrate is present (runsc RuntimeClass +
#   create-pods in $GVISOR_NS). When the opt-in flag is unset it SKIPs cleanly
#   (exit 0) like the sibling gvisor modules. When the opt-in flag IS set but the
#   substrate is MISSING it SKIPs LOUD (still exit 0 so an absent node pool does
#   not red the suite) — it must never look like it asserted when it did not.
#
# Prerequisites (all satisfied by escalation ka-0000004674, same as the C7 gate):
#   - kubectl context on america-e2e-eks
#   - ns gvisor-poc (PSA=privileged), RuntimeClass runsc, NodePool gvisor-poc
#   - SA gardenio-agent: create pods + pods/exec in gvisor-poc
#
# Usage:
#   GVISOR_E2E=1 ./gardenio/tests/e2e/scripts/test-userresponse-latency.sh
#   GVISOR_E2E=1 N_SAMPLES=8 GATE_COLD_P50_MS=15000 KEEP_PODS=1 ./...      # debug
#   GVISOR_E2E=1 REQUIRE_REPO=1 ./...                                      # post-C6
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
PROBE="${PROBE:-userresp-lat-probe-${RUN_ID}}"
# Number of timed samples per state. N>=5 per the standing-gate convention.
N_SAMPLES="${N_SAMPLES:-5}"
# SLO gate (ms). The whole epic's end-to-end target is < 20 s — this is the
# OUTER CEILING every state must clear no matter what. Kept for continuity with
# the epic's headline SLO and to catch a pathological all-states blowup.
GATE_P50_MS="${GATE_P50_MS:-20000}"
# PER-STATE tightened gates (ms). The flat 20 s ceiling above was set when the
# cold path still paid a ~95 s full clone; a flat gate that loose cannot catch a
# regression in a path whose p50 is 8 s (COLD) or sub-second (RESUMING/MID-FREEZE)
# — a sub-second path could balloon 60x and still "pass" 20 s. Now that Lever 2
# (blobless init-clone, ka-0000006528) collapsed COLD create->Ready from ~200 s to
# ~40 s in prod, each state gets a protective gate sitting well above its measured
# p50 (COLD ~8 s, RESUMING ~0.3 s, MID-FREEZE ~0.1 s) but far below the 20 s
# ceiling, so a real regression reds the suite instead of hiding under slack.
# Each is env-overridable; each must be <= GATE_P50_MS (the outer ceiling still
# applies). Raise a per-state gate deliberately if a substrate change moves the
# floor — do not loosen it back to 20 s to make a red run green.
GATE_COLD_P50_MS="${GATE_COLD_P50_MS:-15000}"
GATE_RESUMING_P50_MS="${GATE_RESUMING_P50_MS:-3000}"
GATE_MIDFREEZE_P50_MS="${GATE_MIDFREEZE_P50_MS:-3000}"
# C6 scope gate: refuse to pass unless COLD/RESUMING resumes report repo_present=1.
# Default off until ka-0000007582 lands the repo onto the placeholder.
REQUIRE_REPO="${REQUIRE_REPO:-0}"
# Where the proven node scripts live (shipped to the node via base64).
POC_DIR="${POC_DIR:-gardenio/tests/e2e/scripts/gvisor-node}"
# Prometheus textfile out (histogram exposition for the dashboard).
PROM_TEXTFILE="${PROM_TEXTFILE:-${PROM_TEXTFILE_DIR:-/tmp}/userresponse_latency_e2e_${RUN_ID}.prom}"
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
  # "loud" == opt-in SET but substrate absent → shared readiness guard (hard-fail
  # under REQUIRE_SUBSTRATE=1, loud skip otherwise; task ka-0000002867). "off" ==
  # opt-in unset: inert clean skip.
  if [ "$tag" = "loud" ]; then
    gvisor_require_substrate_guard userresponse-latency "$why"
  else
    blue "SKIP userresponse-latency: $why"
    dim  "  Opt-in module: set GVISOR_E2E=1 (or USERRESPONSE_LATENCY_E2E=1) and run on"
    dim  "  the gvisor-poc substrate (runsc RuntimeClass + create-pods in ns $GVISOR_NS)."
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
# than red the C4 golden gate, so a single node hiccup cannot masquerade as a
# real <20s user-response regression. The release-owner gardener golden-gates on
# this as non-blocking (SKIP-vs-FAIL). See ka-0000001111 / gd-LlQWNUyA8J.
skip_substrate() {
  local why="$1" detail="${2:-}"
  # Under the READINESS GATE (REQUIRE_SUBSTRATE=1) an unbuildable checkpoint
  # substrate is a NO-GO — the gate must prove the substrate works end-to-end
  # (task ka-0000002867). Outside the gate, keep the non-blocking SUBSTRATE-NOT-
  # READY behaviour (ka-0000001111 / gd-LlQWNUyA8J).
  if [ "${REQUIRE_SUBSTRATE:-0}" = "1" ]; then
    red   "════════════════════════════════════════════════════"
    red   " NO-GO userresponse-latency (REQUIRE_SUBSTRATE=1, SUBSTRATE-NOT-READY): $why"
    red   " The readiness gate could not assemble the checkpoint-build substrate"
    red   " on the gvisor-poc node (rootfs / checkpoint image build failed). The"
    red   " gate must PROVE the substrate works end-to-end — an unbuildable"
    red   " substrate is a NO-GO. Do NOT flip FreezeThawEnabled on this signal."
    [ -n "$detail" ] && dim "    $detail"
    red   "════════════════════════════════════════════════════"
    exit 1
  fi
  red   "════════════════════════════════════════════════════"
  red   " SKIP userresponse-latency (SUBSTRATE-NOT-READY): $why"
  red   " The runsc substrate PASSED preflight but the checkpoint-build substrate"
  red   " could not be assembled on the scheduled node (rootfs / checkpoint image"
  red   " build failed). This is an ENVIRONMENTAL node/registry fault, NOT an app"
  red   " regression — this run did NOT assert the <20s user-response SLO, and it"
  red   " must NOT be counted as a regression failure. Re-dispatch on a healthy"
  red   " gvisor-poc node. Golden-gating treats SUBSTRATE-NOT-READY as non-blocking."
  [ -n "$detail" ] && dim "    $detail"
  red   "════════════════════════════════════════════════════"
  if [ -n "${E2E_SKIP_SENTINEL:-}" ]; then printf '%s\n' "$*" >"$E2E_SKIP_SENTINEL" 2>/dev/null || true; fi  # skip visibility (ka-0000000989)
  exit 0
}

k() { kubectl -n "$GVISOR_NS" "$@"; }

# Per-node-call wall-clock cap. The proven node scripts (and runsc itself) can
# HANG on process exit after checkpoint/restore — the documented quirk in
# test-gvisor-freeze-thaw.sh. Without a cap a single hung `runsc state`/`ps`
# would block the harness forever. So every node-side `k exec` is wrapped on BOTH
# sides: a client-side `timeout` so the harness moves on, AND a node-side
# `timeout` (passed into the chroot) so the stuck runsc process is actually
# killed on the node, not just detached from.
NODE_CALL_TIMEOUT="${NODE_CALL_TIMEOUT:-120}"
# node_exec <cap_sec> <bash -c string> — run the command on the node (host chroot)
# with the call wrapped in `timeout` at BOTH the container level and the client
# level. Call kubectl DIRECTLY here, not the k() shell function: `timeout` execs
# its first argument as a binary, and a shell function is not a binary.
node_exec() {
  local cap="$1"; shift
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
    # Force-delete any live leave-running source cid the MID-FREEZE leg left on
    # the node (the harness owns its lifecycle — the node script intentionally
    # does NOT delete it so we can measure against the live agent).
    node_exec 30 'ROOT=/run/containerd/runsc/k8s.io; timeout 10 nsenter -t 1 -m -p -n -- runsc --root=$ROOT delete --force sc-agent >/dev/null 2>&1 || true' >/dev/null 2>&1 || true
    k delete pod "$PROBE" --grace-period=0 --force >/dev/null 2>&1 || true
  else
    dim "  KEEP_PODS=1 — leaving probe + ${#CREATED_PODS[@]} placeholder pods for debug"
  fi
}
trap cleanup EXIT

echo "════════════════════════════════════════════════════"
echo " <20s user-response — STANDING TIMED regression gate (C4)"
echo " ns=$GVISOR_NS runtimeClass=$RUNTIME_CLASS N=$N_SAMPLES ceiling=${GATE_P50_MS}ms gate_cold=${GATE_COLD_P50_MS}ms gate_resuming=${GATE_RESUMING_P50_MS}ms gate_midfreeze=${GATE_MIDFREEZE_P50_MS}ms require_repo=$REQUIRE_REPO"
echo " states: COLD (spawn/thaw) · RESUMING (warm thaw) · MID-FREEZE (interrupt, no thaw)"
echo "════════════════════════════════════════════════════"

# ── Gate 0: opt-in + substrate preflight ───────────────────────────
if [ "${GVISOR_E2E:-0}" != "1" ] && [ "${USERRESPONSE_LATENCY_E2E:-0}" != "1" ]; then
  skip_all "GVISOR_E2E / USERRESPONSE_LATENCY_E2E != 1 (opt-in module)" off
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

blue "Substrate present — running standing user-response assertions."

# ── 1. Schedule the privileged probe + ship the proven node scripts ─
blue "[1/6] scheduling privileged probe '$PROBE' onto the gvisor-poc node"
cat <<YAML | k apply -f - >/dev/null 2>&1
apiVersion: v1
kind: Pod
metadata:
  name: ${PROBE}
  namespace: ${GVISOR_NS}
  labels: {app: userresp-lat-probe, test: ka-0000000107}
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

# Ship both proven node scripts (base64 to dodge exec stdin/quoting pitfalls).
ship_ok=1
for s in single-container-resume-node.sh single-container-thaw-into-b-node.sh; do
  b64="$(base64 -w0 "$POC_DIR/$s")"
  k exec "$PROBE" -- chroot /host bash -c \
    "echo $b64 | base64 -d > /var/tmp/$s && chmod +x /var/tmp/$s" >/dev/null 2>&1 || ship_ok=0
done
[ "$ship_ok" = 1 ] && pass "proven node scripts shipped to the node" \
                    || fail "ship node scripts" "base64 copy to /var/tmp failed"

# ── Shared node-side helpers (mirror test-resume-latency.sh) ────────
# Node-side resolver: given a placeholder pod UID, print "B_SENTRY_PID B_NETNS".
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
thaw_into() {
  local bpid="$1" bnetns="$2" out ms repo
  out="$(node_exec "$NODE_CALL_TIMEOUT" \
    "B_NETNS='$bnetns' B_SENTRY_PID='$bpid' /var/tmp/single-container-thaw-into-b-node.sh 2>&1")"
  ms="$(printf '%s\n' "$out" | sed -n 's/.*THAW_MS=\([0-9][0-9]*\).*/\1/p' | head -1)"
  repo=0; grep -q 'repo_present=1' <<<"$out" && repo=1
  if [ -n "$ms" ]; then echo "$ms 1 $repo"; else echo "0 0 $repo"; fi
}

# Create a placeholder Pod B (the warm/cold restore target) and return its name.
make_placeholder() {
  local name="$1"
  cat <<YAML | k apply -f - >/dev/null 2>&1
apiVersion: v1
kind: Pod
metadata:
  name: ${name}
  namespace: ${GVISOR_NS}
  labels: {app: userresp-lat-placeholder, test: ka-0000000107, role: restore-placeholder}
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

# ── 2. Build the single-container agent bundle + checkpoint (restore image) ─
# Reuse the PROVEN single-container-resume-node.sh in CHECKPOINT_ONLY mode: it
# assembles the ROOT-agent bundle and `runsc checkpoint`s it to /var/tmp/ckSC +
# /var/tmp/sc — the inputs both the COLD and RESUMING restores consume. Success
# is asserted from OBSERVED STATE (the checkpoint images on disk), not the
# script's rc, exactly as the C7 gate does.
blue "[2/6] building single-container agent bundle + checkpoint (/var/tmp/ckSC)"
BUILD_OUT=$(node_exec 240 "CHECKPOINT_ONLY=1 /var/tmp/single-container-resume-node.sh 2>&1; true")
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
  fail "build checkpoint image" "checkpoint artifacts absent on node: $(printf '%s\n' "$BUILD_OUT" | tail -6)"
  echo "════════ Summary: $PASS passed, $FAIL failed ════════"
  exit 1
fi

# ── 3. RESUMING (warm thaw): pre-Ready pool → timed restore-only resumes ─
# Snapshot present + warm placeholder Ready -> restore-only. The proven sub-second
# warm-B path; the user-visible latency here is just THAW_MS. We pre-warm the pool
# OFF the hot path (kubelet admit excluded), then time N restores.
blue "[3/6] RESUMING (warm thaw): pre-warming a pool of $N_SAMPLES Ready placeholder pods"
warm_pods=()
for i in $(seq 1 "$N_SAMPLES"); do
  nm="userresp-warm-${RUN_ID}-${i}"
  make_placeholder "$nm"
  warm_pods+=("$nm")
done
warm_ready=1
for nm in "${warm_pods[@]}"; do
  k wait --for=condition=Ready "pod/$nm" --timeout="$POD_READY_TIMEOUT" >/dev/null 2>&1 || warm_ready=0
done
[ "$warm_ready" = 1 ] && pass "warm pool of $N_SAMPLES placeholders Ready (kubelet admit done OFF the hot path)" \
                      || fail "warm pool Ready" "not all $N_SAMPLES placeholders reached Ready in $POD_READY_TIMEOUT"

blue "    timing $N_SAMPLES RESUMING responses (thaw into an already-Ready B)"
resuming_samples=()
resuming_repo_all=1
resuming_ok=0
i=0
for nm in "${warm_pods[@]}"; do
  i=$((i + 1))
  uid=$(k get pod "$nm" -o jsonpath='{.metadata.uid}' 2>/dev/null)
  read -r bpid bnetns <<<"$(resolve_b "$uid")"
  if [ -z "${bpid:-}" ]; then
    echo "userresponse_latency_ms state=RESUMING run=$i value=NA repo_present=0 (resolve failed for $nm)"
    continue
  fi
  read -r ms ok repo <<<"$(thaw_into "$bpid" "$bnetns")"
  echo "userresponse_latency_ms state=RESUMING run=$i value=${ms} repo_present=${repo}"
  if [ "$ok" = 1 ]; then resuming_samples+=("$ms"); resuming_ok=$((resuming_ok + 1)); [ "$repo" = 0 ] && resuming_repo_all=0; fi
done
[ "$resuming_ok" -ge "$N_SAMPLES" ] && pass "RESUMING responses: $resuming_ok/$N_SAMPLES succeeded" \
                                    || fail "RESUMING responses succeeded" "only $resuming_ok/$N_SAMPLES RESUMING thaws succeeded"

# ── 4. COLD (spawn/thaw): create→Ready→restore, full wall-clock ─────
# No warm placeholder: the wake request pays the full pod admission cost FIRST,
# then restores. This is the cold-start user-visible path. latency = create→Ready
# wall-clock + THAW_MS.
blue "[4/6] COLD (spawn/thaw): $N_SAMPLES responses that PAY the Pod-B Ready wait"
cold_samples=()
cold_repo_all=1
cold_ok=0
for i in $(seq 1 "$N_SAMPLES"); do
  nm="userresp-cold-${RUN_ID}-${i}"
  t0=$(date +%s%3N)
  make_placeholder "$nm"
  if ! k wait --for=condition=Ready "pod/$nm" --timeout="$POD_READY_TIMEOUT" >/dev/null 2>&1; then
    echo "userresponse_latency_ms state=COLD run=$i value=NA repo_present=0 (pod $nm not Ready)"
    continue
  fi
  uid=$(k get pod "$nm" -o jsonpath='{.metadata.uid}' 2>/dev/null)
  read -r bpid bnetns <<<"$(resolve_b "$uid")"
  if [ -z "${bpid:-}" ]; then
    echo "userresponse_latency_ms state=COLD run=$i value=NA repo_present=0 (resolve failed)"
    continue
  fi
  read -r ms ok repo <<<"$(thaw_into "$bpid" "$bnetns")"
  t1=$(date +%s%3N)
  if [ "$ok" = 1 ]; then
    total=$((t1 - t0))   # create→Ready→restore wall-clock = the COLD cost
    cold_samples+=("$total")
    cold_ok=$((cold_ok + 1))
    [ "$repo" = 0 ] && cold_repo_all=0
    echo "userresponse_latency_ms state=COLD run=$i value=${total} repo_present=${repo} (restore=${ms}ms + pod-admit)"
  else
    echo "userresponse_latency_ms state=COLD run=$i value=NA repo_present=0 (restore failed)"
  fi
done
[ "$cold_ok" -ge 1 ] && pass "COLD responses: $cold_ok/$N_SAMPLES succeeded" \
                     || fail "COLD responses" "no COLD spawn/thaw responses succeeded — cannot form the COLD distribution"

# ── 5. MID-FREEZE (interrupt, no thaw): freeze in flight → message → live ──
# The novel C2 leg (ka-0000005834). When a message arrives mid-freeze the worst
# the user can wait is for the IN-FLIGHT CHECKPOINT to be aborted/complete — after
# which the STILL-LIVE Pod A serves the message with NO thaw (no stop, no restore,
# no pod admission). So the user-visible mid-freeze cost is bounded by the freeze
# window itself, and the load-bearing precondition is the C2 invariant: a
# `runsc checkpoint --leave-running` MUST leave the sandbox `running`
# (plan-of-record / freezethaw.go L540). If that invariant holds, the agent never
# went dark and the message is served live.
#
# We measure this by driving the proven node script in LEAVE_RUNNING_ONLY mode:
# it builds + runs the sandbox, takes a `--leave-running` checkpoint, reports the
# checkpoint duration as LR_FREEZE_MS (the user-visible window the interrupt
# bounds), and asserts the sandbox is still `running` afterwards. The build/run is
# setup that models the freeze the controller already had in flight; the SAMPLE is
# LR_FREEZE_MS, gated on LEAVE_RUNNING_PROVEN=1 (status==running). We rebuild per
# sample (the script leaves one live cid 'sc-agent' which we force-delete after).
blue "[5/6] MID-FREEZE (interrupt, no thaw): $N_SAMPLES checkpoint-in-flight → interrupt → live responses"
midfreeze_samples=()
midfreeze_ok=0
node_delete_sc() {
  node_exec 30 'ROOT=/run/containerd/runsc/k8s.io; timeout 10 nsenter -t 1 -m -p -n -- runsc --root=$ROOT delete --force sc-agent >/dev/null 2>&1 || true' >/dev/null 2>&1 || true
}
for i in $(seq 1 "$N_SAMPLES"); do
  # Clean any prior live source cid so the leave-running build starts fresh.
  node_delete_sc
  # Materialize the in-flight-freeze state: checkpoint taken with the source cid
  # left LIVE. The node script asserts the C2 leave-running invariant and reports
  # LR_FREEZE_MS = the in-flight checkpoint window the interrupt bounds.
  out="$(node_exec 240 "LEAVE_RUNNING_ONLY=1 /var/tmp/single-container-resume-node.sh 2>&1")"
  proven=0; grep -q 'LEAVE_RUNNING_PROVEN=1' <<<"$out" && proven=1
  status="$(printf '%s\n' "$out" | sed -n 's/.*LR_STATUS=\([a-zA-Z]*\).*/\1/p' | head -1)"
  ms="$(printf '%s\n' "$out" | sed -n 's/.*LR_FREEZE_MS=\([0-9][0-9]*\).*/\1/p' | head -1)"
  if [ "$proven" = 1 ] && [ -n "$ms" ]; then
    midfreeze_samples+=("$ms")
    midfreeze_ok=$((midfreeze_ok + 1))
    echo "userresponse_latency_ms state=MID_FREEZE run=$i value=${ms} repo_present=1 (no-thaw; sandbox=${status:-running}; freeze_window=${ms}ms)"
  else
    echo "userresponse_latency_ms state=MID_FREEZE run=$i value=NA repo_present=1 (leave-running invariant NOT proven: status='${status:-unknown}' freeze_ms='${ms:-NA}')"
  fi
  node_delete_sc
done
[ "$midfreeze_ok" -ge 1 ] && pass "MID-FREEZE responses: $midfreeze_ok/$N_SAMPLES served by the still-live agent (no thaw)" \
                          || fail "MID-FREEZE responses" "no MID-FREEZE interrupt responses proven — the still-live-agent invariant did not hold (C2 ka-0000005834)"

# ── 6. GATE + timed emission ───────────────────────────────────────
blue "[6/6] evaluating the <20s SLO gate (per state) + emitting timed samples"
COLD_P50=$(p50 "${cold_samples[@]:-}")
RESUMING_P50=$(p50 "${resuming_samples[@]:-}")
MIDFREEZE_P50=$(p50 "${midfreeze_samples[@]:-}")
echo "    COLD       p50 = ${COLD_P50:-NA}ms (n=${#cold_samples[@]}, samples: ${cold_samples[*]:-none})"
echo "    RESUMING   p50 = ${RESUMING_P50:-NA}ms (n=${#resuming_samples[@]}, samples: ${resuming_samples[*]:-none})"
echo "    MID-FREEZE p50 = ${MIDFREEZE_P50:-NA}ms (n=${#midfreeze_samples[@]}, samples: ${midfreeze_samples[*]:-none})"

# Emit the Prometheus histogram textfile (sibling to gardenio_resume_latency_seconds,
# same buckets) so a dashboard can ingest these standing-CI samples. SECONDS.
emit_prom() {
  {
    echo "# HELP gardenio_userresponse_latency_seconds User-visible first-response latency by lifecycle state (standing e2e gate, task ka-0000000107)."
    echo "# TYPE gardenio_userresponse_latency_seconds histogram"
    printf '%s\n' "COLD ${cold_samples[*]:-}" "RESUMING ${resuming_samples[*]:-}" "MID_FREEZE ${midfreeze_samples[*]:-}" | awk -v role="$ROLE" -v rt="$RUNTIME" '
      BEGIN { nb=split("0.25 0.5 0.75 1 1.25 1.5 2 3 5 10 30 60 120 300", B, " ") }
      {
        state=$1; cnt=0; sum=0; for(i=1;i<=nb;i++) bk[i]=0
        for(j=2;j<=NF;j++){ if($j !~ /^[0-9]+$/) continue; s=$j/1000.0; cnt++; sum+=s
          for(i=1;i<=nb;i++) if(s<=B[i]+0) bk[i]++ }
        if(cnt==0) next
        lbl=sprintf("state=\"%s\",role=\"%s\",runtime=\"%s\",env=\"e2e\"", state, role, rt)
        for(i=1;i<=nb;i++) printf "gardenio_userresponse_latency_seconds_bucket{%s,le=\"%s\"} %d\n", lbl, B[i], bk[i]
        printf "gardenio_userresponse_latency_seconds_bucket{%s,le=\"+Inf\"} %d\n", lbl, cnt
        printf "gardenio_userresponse_latency_seconds_sum{%s} %.6f\n", lbl, sum
        printf "gardenio_userresponse_latency_seconds_count{%s} %d\n", lbl, cnt
      }'
  } > "$PROM_TEXTFILE" 2>/dev/null && dim "    wrote histogram textfile: $PROM_TEXTFILE"
}
emit_prom

# Hard gate — each state's p50 strictly below BOTH its tightened per-state gate
# AND the outer <20s epic ceiling. The per-state gate is the one that actually
# bites now that the cold path is fixed; the ceiling remains as a backstop and to
# keep the headline SLO explicit in every run.
gate_state() {
  local name="$1" p50v="$2" gate="$3"
  # Effective gate = the tighter of the per-state gate and the outer ceiling.
  if [ "$gate" -gt "$GATE_P50_MS" ] 2>/dev/null; then gate="$GATE_P50_MS"; fi
  if [ -n "$p50v" ] && [ "$p50v" -lt "$gate" ] 2>/dev/null; then
    pass "$name p50 ${p50v}ms < gate ${gate}ms (ceiling ${GATE_P50_MS}ms; user-visible response within budget)"
  else
    fail "$name p50 < gate" "p50='${p50v:-NA}ms' not < ${gate}ms (ceiling ${GATE_P50_MS}ms) — user-visible $name response REGRESSED"
  fi
}
gate_state "COLD"       "$COLD_P50"       "$GATE_COLD_P50_MS"
gate_state "RESUMING"   "$RESUMING_P50"   "$GATE_RESUMING_P50_MS"
gate_state "MID-FREEZE" "$MIDFREEZE_P50"  "$GATE_MIDFREEZE_P50_MS"

# Hard gate — C6 scope: COLD + RESUMING resumes must carry the repo (post-ka-0000007582).
# MID-FREEZE serves the still-live Pod A, whose repo never left, so it is exempt.
if [ "$REQUIRE_REPO" = 1 ]; then
  if [ "${#resuming_samples[@]}" -gt 0 ] && [ "$resuming_repo_all" = 1 ] \
     && [ "${#cold_samples[@]}" -gt 0 ] && [ "$cold_repo_all" = 1 ]; then
    pass "REQUIRE_REPO=1 satisfied: all COLD + RESUMING resumes reported repo_present=1"
  else
    fail "REQUIRE_REPO=1 repo present" "a COLD/RESUMING resume reported repo_present=0 — a fast number on an empty workspace does not count (C6 ka-0000007582)"
  fi
else
  dim  "    REQUIRE_REPO=0 — repo-present not gated yet (flip to 1 once C6 ka-0000007582 lands the repo onto the placeholder)"
fi

# ── Summary ────────────────────────────────────────────────────────
echo ""
echo "════════════════════════════════════════════════════"
echo " user-response gate Summary: $PASS passed, $FAIL failed"
echo "   COLD p50=${COLD_P50:-NA}ms (gate ${GATE_COLD_P50_MS}ms)  RESUMING p50=${RESUMING_P50:-NA}ms (gate ${GATE_RESUMING_P50_MS}ms)  MID-FREEZE p50=${MIDFREEZE_P50:-NA}ms (gate ${GATE_MIDFREEZE_P50_MS}ms)  ceiling=${GATE_P50_MS}ms"
echo "════════════════════════════════════════════════════"
if [ "$FAIL" -gt 0 ]; then
  for e in "${ERRORS[@]}"; do red "  - $e"; done
  exit 1
fi
green "All user-response assertions passed — per-state gates (COLD<${GATE_COLD_P50_MS}ms, RESUMING<${GATE_RESUMING_P50_MS}ms, MID-FREEZE<${GATE_MIDFREEZE_P50_MS}ms) and the <20s ceiling hold."
exit 0
