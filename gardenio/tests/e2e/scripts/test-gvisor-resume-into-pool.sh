#!/usr/bin/env bash
# test-gvisor-resume-into-pool.sh — EPIC ACCEPTANCE GATE (ka-0000003295, epic
# ka-0000007532 "gVisor freeze→thaw RESUME into a prewarmed pool").
#
# THE ONE END-TO-END PROOF the whole resume-into-pool epic converges on:
#   FREEZE an active agent  →  RESUME its snapshot INTO a CLAIMED prewarmed POOL
#   member (NOT a fresh on-demand Pod B)  →  assert, on the real gvisor-poc
#   substrate, that ALL of the following hold together for the SAME resumed agent:
#     (1) /work SURVIVED  — the on-disk file token (state.bin sentinel) written
#         before the freeze is byte-identical after the resume.
#     (2) RAM SURVIVED    — the in-RAM monotonic counter climbs PAST its frozen
#         value, proving the restored process is the SAME live process with its
#         memory (heap working-set included), not a cold restart from zero.
#     (3) gb + coop REHYDRATE-ACK  — the REAL controller gate
#         (thawepoch.WaitForAcks, driven by cmd/thawepoch-probe) RELEASES the
#         resumed agent because BOTH required consumers (ConsumerCoop + ConsumerGB)
#         ack the current thaw epoch WITHIN THAW_REHYDRATE_DEADLINE — keyed to the
#         CLAIMED POOL MEMBER's pod-uid, not a synthetic one.
#     (4) fail-close negative control — with coop silent the gate HOLDS (never
#         releases a half-rehydrated agent). Proves (3) is a real gate, not a
#         rubber stamp.
#     (5) the pool member stays ALIVE across the whole gate window, and the pool
#         is OBSERVABLY CONSUMED (the claimed member is no longer an idle standby).
#
# TWO VARIANTS (both asserted in one run — the task's explicit deliverable):
#   • claudeless    — a minimal agent with WORKING_SET_ENTRIES=0 (no loaded heap).
#                     The floor case: proves the mechanism with a trivial RAM
#                     footprint, the fastest/cheapest signal.
#   • active-Claude — the rich agentsim proxy for a live Claude session:
#                     WORKING_SET_ENTRIES=2000 (a multi-MB loaded heap standing in
#                     for a Claude working set) + an open append-log FD + a UDP
#                     socket. This is the case the epic's HARD GATE cares about —
#                     the first clean active-Claude freeze→resume with acks in
#                     deadline (epic ka-0000007532). Its RAM assertion (2) is what
#                     proves a loaded working set survives, not just a counter.
#
# WHY "INTO A POOL MEMBER" AND NOT JUST "INTO POD B":
#   The sibling gates prove the pieces: test-gvisor-freeze-thaw.sh proves freeze/
#   thaw state fidelity; test-resume-latency.sh proves <1s restore into a warm
#   placeholder; test-gvisor-rehydrate-ack.sh proves the gb+coop ack RELEASE on a
#   live Pod B. NONE proves the CONVERGED path the epic is about: the warm target
#   is a CLAIMED PREWARMED POOL MEMBER (a runsc placeholder built to the
#   warm-placeholder shape — /work at podmanager.WarmPlaceholderWorkPath, the
#   passive sleep body that never races the restore), the restore GRAFTS onto that
#   claimed member's kubelet-owned netns, AND /work + RAM + acks are ALL asserted
#   on that one resumed agent. That convergence is the acceptance gate.
#
# HOW THE POOL MEMBER IS MODELED (deterministic, spend-safe, no controller needed):
#   A prewarmed pool member, at the moment a resume claims it, IS a Ready runsc
#   placeholder pod carrying the warm-placeholder shape (BuildWarmPlaceholderPod:
#   runsc RuntimeClass, /work emptyDir at WarmPlaceholderWorkPath, the
#   `mkdir -p /work/repo; sleep` body). We build that member with a POOL label
#   (role=warm-placeholder, gardenio.io/pool-standby=true) and a "claim" step that
#   stamps the claiming agent onto it (the ClaimWarmPlaceholderForThaw promotion),
#   then restore INTO it. We assert the claim consumed the standby (label flips) so
#   a resume observably draws down the pool. This mirrors the exact placeholder the
#   controller pool mints (warm_placeholder.go) without requiring a live
#   WARM_PLACEHOLDER_POOL_ENABLED controller on the e2e cluster — the graft target's
#   shape is what the restore actually depends on. (The controller-minted pool has
#   its own unit+e2e coverage: poolmanager/*_test.go + warm_placeholder_pool_e2e_test.go.)
#
# runsc QUIRK: `runsc checkpoint`/`restore` complete the op then HANG on process
#   EXIT (documented in test-gvisor-freeze-thaw.sh). Every runsc call is wrapped in
#   `timeout` and asserted on OBSERVED post-state, never on rc.
#
# OPT-IN / GATING (NEVER silently passes):
#   Targets the dedicated gvisor-poc namespace + runsc node pool, NOT the
#   per-run e2e namespace. Runs only when GVISOR_E2E=1 (or
#   RESUME_INTO_POOL_E2E=1) AND the substrate is present (runsc RuntimeClass +
#   create-pods in $GVISOR_NS). Opt-in unset → SKIP clean (exit 0, inert in the
#   standard shards). Opt-in SET but substrate ABSENT → the shared readiness guard
#   (hard-fail under REQUIRE_SUBSTRATE=1, loud skip otherwise). Needs a Go
#   toolchain to build cmd/thawepoch-probe from source.
#   Wire it in via:  GVISOR_E2E=1 ONLY=gvisor-resume-into-pool run-shard.sh
#
# Prerequisites (all satisfied by escalation ka-0000004674):
#   - kubectl context on the gvisor-poc-capable cluster
#   - ns gvisor-poc (PSA=privileged), RuntimeClass runsc, NodePool gvisor-poc
#   - SA gardenio-agent: create pods + pods/exec in gvisor-poc
#   - a Go toolchain in the task (to build cmd/thawepoch-probe)
#
# Usage:
#   GVISOR_E2E=1 ./gardenio/tests/e2e/scripts/test-gvisor-resume-into-pool.sh
#   GVISOR_E2E=1 ONLY_VARIANT=claudeless KEEP_PODS=1 ./...              # one variant, debug
#   GVISOR_E2E=1 REHYDRATE_DEADLINE_MS=8000 ./...                       # tighten the ack budget
set -uo pipefail

# ── Config (all overridable via env) ────────────────────────────────
GVISOR_NS="${GVISOR_NS:-gvisor-poc}"
RUNTIME_CLASS="${RUNTIME_CLASS:-runsc}"
RUN_ID="${RUN_ID:-$$}"
PROBE="${PROBE:-resume-into-pool-probe-${RUN_ID}}"
# Rehydration deadline (ms) the gate is asserted against — assertion (3). Default
# mirrors thawepoch.DefaultDeadline (10s), the cfg.ThawRehydrateDeadline effective
# budget. The whole point is RELEASE WITHIN this budget.
REHYDRATE_DEADLINE_MS="${REHYDRATE_DEADLINE_MS:-10000}"
# Which variants to run. Default: both (the task deliverable). Override to a single
# variant for a fast local iteration.
ONLY_VARIANT="${ONLY_VARIANT:-claudeless active-claude}"
# Where the proven node scripts live (shipped to the node via base64).
POC_DIR="${POC_DIR:-gardenio/tests/e2e/scripts/gvisor-node}"
# Controller module dir (to build cmd/thawepoch-probe from source).
CONTROLLER_DIR="${CONTROLLER_DIR:-gardenio/controller}"
POD_READY_TIMEOUT="${POD_READY_TIMEOUT:-180s}"
# Working-set size (in-RAM heap entries, ~512B each) per variant — the RAM proxy.
CLAUDELESS_WORKING_SET="${CLAUDELESS_WORKING_SET:-0}"
ACTIVE_CLAUDE_WORKING_SET="${ACTIVE_CLAUDE_WORKING_SET:-2000}"
# /work path on the pool member — must match podmanager.WarmPlaceholderWorkPath so
# the restored container's /work bind lines up with the claimed member's mount.
POOL_WORK_PATH="${POOL_WORK_PATH:-/work}"

# Shared gvisor-poc substrate helpers: gvisor_require_substrate_guard (readiness
# gate for a missing substrate), gvisor_warm_node_gate (bounded warm-node poll),
# gvisor_retry_step (bounded signature-keyed retry for the flaky node steps).
# Decision ka-0000000122; root flake ka-0000007234.
# shellcheck disable=SC1091
source "$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)/lib-gvisor-node.sh"

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
# Opt-in OFF (flags unset): inert clean skip, always exit 0 — this module is
# simply not selected. Substrate ABSENT (opt-in on, no node pool) goes through the
# shared readiness guard so REQUIRE_SUBSTRATE=1 can hard-fail rather than pass.
skip_optin_off() {
  blue "SKIP gvisor-resume-into-pool: $1"
  dim "  Opt-in module: set GVISOR_E2E=1 (or RESUME_INTO_POOL_E2E=1) and run on the"
  dim "  gvisor-poc substrate (runsc RuntimeClass + create-pods in ns $GVISOR_NS)."
  if [ -n "${E2E_SKIP_SENTINEL:-}" ]; then printf '%s\n' "$*" >"$E2E_SKIP_SENTINEL" 2>/dev/null || true; fi  # skip visibility (ka-0000000989)
  exit 0
}
skip_all() { gvisor_require_substrate_guard gvisor-resume-into-pool "$1"; }

k() { kubectl -n "$GVISOR_NS" "$@"; }

# Per-node-call wall-clock cap (runsc can HANG on process exit). Every node-side
# exec is wrapped on BOTH sides: a node-side `timeout` (kills the hung chroot) and
# a client-side `timeout` (so kubectl exec can't block the harness). Mirrors
# test-resume-latency.sh / test-gvisor-rehydrate-ack.sh node_exec.
NODE_CALL_TIMEOUT="${NODE_CALL_TIMEOUT:-120}"
node_exec() {
  local cap="$1"; shift
  timeout "$((cap + 20))" kubectl -n "$GVISOR_NS" exec "$PROBE" -- timeout "$cap" chroot /host bash -c "$1" 2>/dev/null
}

cleanup() {
  blue "── cleanup ──"
  # Best-effort node-side teardown of every per-variant cid / dir.
  node_exec 40 '
    ROOT=/run/containerd/runsc/k8s.io
    for cid in sc-agent-cl sc-agent-ac sc-into-pool-cl sc-into-pool-ac; do
      timeout 15 nsenter -t 1 -m -p -n -- runsc --root=$ROOT delete --force "$cid" 2>/dev/null || true
    done
    rm -rf /var/tmp/rip-* /var/tmp/thawepoch-probe /var/tmp/single-container-*-node.sh 2>/dev/null || true
  ' >/dev/null 2>&1 || true
  if [ "${KEEP_PODS:-0}" != "1" ]; then
    for p in "${CREATED_PODS[@]:-}"; do
      [ -n "$p" ] && k delete pod "$p" --grace-period=0 --force >/dev/null 2>&1 || true
    done
    k delete pod "$PROBE" --grace-period=0 --force >/dev/null 2>&1 || true
  else
    dim "  KEEP_PODS=1 — leaving probe + ${#CREATED_PODS[@]} pool-member pods for debug"
  fi
}
trap cleanup EXIT

echo "════════════════════════════════════════════════════"
echo " gVisor freeze → resume-INTO-POOL — EPIC ACCEPTANCE GATE (ka-0000003295)"
echo " ns=$GVISOR_NS runtimeClass=$RUNTIME_CLASS deadline=${REHYDRATE_DEADLINE_MS}ms variants='$ONLY_VARIANT'"
echo "════════════════════════════════════════════════════"

# ── Gate 0: opt-in + substrate preflight ────────────────────────────
if [ "${GVISOR_E2E:-0}" != "1" ] && [ "${RESUME_INTO_POOL_E2E:-0}" != "1" ]; then
  skip_optin_off "GVISOR_E2E / RESUME_INTO_POOL_E2E != 1 (opt-in module)"
fi
command -v kubectl >/dev/null 2>&1 || skip_all "kubectl not installed"
command -v go >/dev/null 2>&1 || skip_all "go toolchain not present (need it to build cmd/thawepoch-probe)"
kubectl get runtimeclass "$RUNTIME_CLASS" >/dev/null 2>&1 \
  || skip_all "RuntimeClass '$RUNTIME_CLASS' not present on this cluster"
# Namespace-existence check BEFORE auth: the gvisor-poc ns is part of the substrate
# (escalation ka-0000004674). auth can-i can return "yes" (RBAC permits create) even
# when the ns is absent, so without this a genuinely absent substrate would hard-fail
# at probe scheduling instead of routing through the readiness guard (clean SKIP /
# REQUIRE_SUBSTRATE=1 hard-fail). Never let an absent node pool red the suite.
kubectl get namespace "$GVISOR_NS" >/dev/null 2>&1 \
  || skip_all "namespace '$GVISOR_NS' not present on this cluster (gvisor-poc substrate absent)"
[ "$(kubectl auth can-i create pods -n "$GVISOR_NS" 2>/dev/null)" = "yes" ] \
  || skip_all "cannot create pods in ns '$GVISOR_NS'"
[ -f "$POC_DIR/single-container-resume-node.sh" ] \
  || skip_all "node script single-container-resume-node.sh missing under $POC_DIR"
[ -f "$POC_DIR/single-container-thaw-into-b-node.sh" ] \
  || skip_all "node script single-container-thaw-into-b-node.sh missing under $POC_DIR"
[ -d "$CONTROLLER_DIR/cmd/thawepoch-probe" ] \
  || skip_all "cmd/thawepoch-probe missing under $CONTROLLER_DIR"

blue "Substrate present — running resume-into-pool acceptance assertions."

# ── 1. Build cmd/thawepoch-probe (the REAL gate+watcher driver) ─────
# Cross-compile static linux/amd64 so it runs in the host chroot regardless of the
# task's libc. This is the GENUINE thawepoch.WaitForAcks + thawepoch.Watcher
# production code — NOT a re-implementation.
blue "[1/4] building cmd/thawepoch-probe (real thawepoch.WaitForAcks + Watcher)"
PROBE_BIN="$(mktemp /tmp/thawepoch-probe.XXXXXX)"
if ( cd "$CONTROLLER_DIR" && CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -o "$PROBE_BIN" ./cmd/thawepoch-probe/ ) >"/tmp/thawepoch-build-${RUN_ID}.log" 2>&1; then
  pass "thawepoch-probe built ($(wc -c < "$PROBE_BIN" | tr -d ' ') bytes, linux/amd64 static)"
else
  fail "build thawepoch-probe" "go build failed: $(tail -4 "/tmp/thawepoch-build-${RUN_ID}.log")"
  echo "════════ Summary: $PASS passed, $FAIL failed ════════"
  exit 1
fi

# ── 2. Schedule the privileged probe + warm the node + ship assets ──
blue "[2/4] scheduling privileged probe '$PROBE' onto the gvisor-poc node"
cat <<YAML | k apply -f - >/dev/null 2>&1
apiVersion: v1
kind: Pod
metadata:
  name: ${PROBE}
  namespace: ${GVISOR_NS}
  labels: {app: resume-into-pool-probe, test: ka-0000003295}
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
# ka-0000007234): a freshly Karpenter-provisioned node reports its probe Ready
# while runsc + /var/tmp + the runsc state root are still settling.
if gvisor_warm_node_gate node_exec; then
  pass "gvisor-poc node warm (runsc present + /var/tmp writable + runsc-root reachable)"
else
  fail "gvisor-poc node warm-gate" "$GVISOR_WARM_DIAG"
  echo "════════ Summary: $PASS passed, $FAIL failed ════════"
  exit 1
fi

# Ship the proven node scripts + the thawepoch-probe binary (base64, to dodge exec
# stdin/quoting pitfalls). Bounded signature-keyed retry so a single cold-start
# copy miss self-heals; a genuinely broken copy still HARD-FAILS on exhaustion.
ship_node_assets() {
  for s in single-container-resume-node.sh single-container-thaw-into-b-node.sh; do
    local b64; b64="$(base64 -w0 "$POC_DIR/$s")"
    k exec "$PROBE" -- chroot /host bash -c \
      "echo $b64 | base64 -d > /var/tmp/$s && chmod +x /var/tmp/$s" >/dev/null 2>&1 || return 1
  done
  base64 -w0 "$PROBE_BIN" | k exec -i "$PROBE" -- chroot /host bash -c \
    'base64 -d > /var/tmp/thawepoch-probe && chmod +x /var/tmp/thawepoch-probe' >/dev/null 2>&1 || return 1
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
  fail "ship node assets" "base64 copy to /var/tmp failed or binary not executable — $GVISOR_STEP_DIAG"
  echo "════════ Summary: $PASS passed, $FAIL failed ════════"
  exit 1
fi

# ── Node-side helpers reused per variant ────────────────────────────
# Resolve a claimed pool member (runsc placeholder) pod UID → "SENTRY_PID NETNS".
# Identical resolver to test-resume-latency.sh / test-gvisor-rehydrate-ack.sh.
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

# Create a PREWARMED POOL MEMBER: a Ready runsc placeholder carrying the
# warm-placeholder shape (BuildWarmPlaceholderPod) AND the pool-standby labels a
# real controller-minted member carries, so we can assert the claim consumes it.
make_pool_member() {
  local name="$1"
  cat <<YAML | k apply -f - >/dev/null 2>&1
apiVersion: v1
kind: Pod
metadata:
  name: ${name}
  namespace: ${GVISOR_NS}
  labels:
    app: resume-into-pool-member
    test: ka-0000003295
    role: warm-placeholder
    gardenio.io/pool-standby: "true"
spec:
  runtimeClassName: ${RUNTIME_CLASS}
  restartPolicy: Never
  nodeSelector: {gvisor: enabled}
  tolerations:
  - {key: gvisor, operator: Equal, value: enabled, effect: NoSchedule}
  containers:
  - name: agent
    image: ${BUSYBOX_IMAGE:-public.ecr.aws/docker/library/python:3.11-slim}
    command: ["/bin/sh","-c"]
    args: ["mkdir -p ${POOL_WORK_PATH}/repo 2>/dev/null; exec sleep 86400"]
    volumeMounts: [{name: work, mountPath: ${POOL_WORK_PATH}}]
  volumes: [{name: work, emptyDir: {}}]
YAML
  CREATED_PODS+=("$name")
}

# ── 3. Per-variant acceptance ladder ────────────────────────────────
# For each variant: FREEZE (checkpoint an active single-container agent with the
# variant's working set) → build a Ready pool member → CLAIM it (stamp the agent,
# flip the standby label) → RESUME the checkpoint INTO the claimed member's netns
# → assert /work + RAM + gb/coop ack RELEASE + fail-close + member-still-alive +
# pool-consumed.
run_variant() {
  local variant="$1" working_set="$2"
  local slug bdir ckpt cid rcid statedir sentinel member uid
  slug=$(printf '%s' "$variant" | tr -c 'a-z0-9' '-')
  bdir="/var/tmp/rip-${slug}-sc"
  ckpt="/var/tmp/rip-${slug}-ck"
  cid="sc-agent-${slug:0:2}"
  rcid="sc-into-pool-${slug:0:2}"
  statedir="/var/tmp/rip-${slug}-thawstate"
  sentinel="RIP-${slug}-${RUN_ID}"
  member="resume-into-pool-member-${slug}-${RUN_ID}"

  echo ""
  blue "════ VARIANT: ${variant} (working_set=${working_set} entries) ════"

  # ── FREEZE ── build + checkpoint the active agent with this variant's RAM.
  # CHECKPOINT_ONLY=1 stops after the checkpoint is written (the artifacts the
  # resume consumes); its own self-proof restore is skipped. Bounded retry on the
  # cold-start race; assert on OBSERVED checkpoint artifacts, not the script rc.
  blue "  [freeze] checkpoint an active agent (sentinel=$sentinel, ws=$working_set)"
  local build_out=""
  do_freeze() {
    build_out=$(node_exec 240 "CHECKPOINT_ONLY=1 BDIR='$bdir' CKPT='$ckpt' SC_CID='$cid' WORKING_SET_ENTRIES='$working_set' AGENT_SENTINEL='$sentinel' /var/tmp/single-container-resume-node.sh 2>&1; true")
    local have
    have=$(node_exec 30 "if [ -f '$ckpt/checkpoint.img' ] && [ -f '$ckpt/pages.img' ] && [ -f '$bdir/config.json' ]; then echo OK; else echo MISSING; fi" | tr -d '\r')
    [ "$have" = OK ]
  }
  if gvisor_retry_step "FREEZE-${slug}" 3 5 do_freeze; then
    local ck_kib pre_counter
    ck_kib=$(node_exec 30 "du -sk '$ckpt' 2>/dev/null | awk '{print \$1}'" | tr -d '\r')
    # The frozen counter is the agent's in-RAM value at checkpoint — assertion (2)
    # baseline. Read from the host-visible /work/agent.log the checkpoint captured.
    pre_counter=$(node_exec 30 "tail -1 '$bdir/work/agent.log' 2>/dev/null | python3 -c 'import sys,json;print(json.load(sys.stdin)[\"counter\"])' 2>/dev/null" | tr -d '\r')
    RIP_FROZEN_COUNTER="$pre_counter"
    pass "[$variant] agent FROZEN — checkpoint ${ck_kib:-?}KiB + bundle present (frozen counter=${pre_counter:-?}; attempts=$GVISOR_STEP_ATTEMPTS)"
  else
    fail "[$variant] FREEZE checkpoint" "artifacts absent on node after retries: $(printf '%s\n' "$build_out" | tail -6)"
    return 1
  fi
  # /work file token must exist in the frozen snapshot's /work (assertion (1)
  # baseline — it must be present BEFORE we prove it survived the resume).
  local pre_token
  pre_token=$(node_exec 30 "cat '$bdir/work/state.bin' 2>/dev/null" | tr -d '\r')
  if [ "$pre_token" = "$sentinel" ]; then
    pass "[$variant] /work file token seeded pre-freeze (state.bin = $sentinel)"
  else
    fail "[$variant] /work token pre-freeze" "got '$pre_token' want '$sentinel'"
  fi

  # ── POOL MEMBER ── build a Ready prewarmed member + CLAIM it.
  blue "  [pool] mint a Ready prewarmed pool member + claim it for the resume"
  make_pool_member "$member"
  if ! k wait --for=condition=Ready "pod/$member" --timeout="$POD_READY_TIMEOUT" >/dev/null 2>&1; then
    fail "[$variant] pool member Ready" "member $member did not become Ready in $POD_READY_TIMEOUT"
    return 1
  fi
  # Standby precondition: the member is an IDLE standby before the claim.
  local standby_pre
  standby_pre=$(k get pod "$member" -o jsonpath='{.metadata.labels.gardenio\.io/pool-standby}' 2>/dev/null)
  [ "$standby_pre" = "true" ] \
    && pass "[$variant] pool member Ready + idle standby (gardenio.io/pool-standby=true)" \
    || fail "[$variant] pool member idle standby" "pool-standby label pre-claim = '$standby_pre' (want true)"

  # CLAIM: stamp the resuming agent onto the member and flip the standby label —
  # the observable of ClaimWarmPlaceholderForThaw promoting a standby to a claimed
  # graft target. A real resume MUST consume a standby, not run on an idle one.
  local agent_name="rip-agent-${slug}-${RUN_ID}"
  k label pod "$member" gardenio.io/pool-standby=false --overwrite >/dev/null 2>&1
  k annotate pod "$member" gardenio.io/claimed-by="$agent_name" --overwrite >/dev/null 2>&1
  local standby_post claimed_by
  standby_post=$(k get pod "$member" -o jsonpath='{.metadata.labels.gardenio\.io/pool-standby}' 2>/dev/null)
  claimed_by=$(k get pod "$member" -o jsonpath='{.metadata.annotations.gardenio\.io/claimed-by}' 2>/dev/null)
  if [ "$standby_post" = "false" ] && [ "$claimed_by" = "$agent_name" ]; then
    pass "[$variant] pool member CLAIMED (standby true→false, claimed-by=$agent_name) — resume consumes a standby, not an idle member"
  else
    fail "[$variant] pool member claimed" "standby='$standby_post' claimed-by='$claimed_by' (want false / $agent_name)"
  fi

  # Resolve the claimed member's sentry pid + kubelet-owned netns (bounded retry —
  # the runsc sandbox can be briefly unresolvable right after Ready).
  uid=$(k get pod "$member" -o jsonpath='{.metadata.uid}' 2>/dev/null)
  local b_pid b_netns
  resolve_member() { read -r b_pid b_netns <<<"$(resolve_b "$uid")"; [ -n "${b_pid:-}" ]; }
  if ! gvisor_retry_step "member-netns-${slug}" 4 3 resolve_member; then
    fail "[$variant] resolve pool member netns" "could not resolve sandbox sentry pid/netns for $member (uid=$uid) — $GVISOR_STEP_DIAG"
    return 1
  fi

  # ── RESUME ── restore the checkpoint INTO the claimed member's netns.
  blue "  [resume] runsc restore the frozen agent INTO the claimed pool member's netns"
  local thaw_out thaw_ms
  thaw_out=$(node_exec "$NODE_CALL_TIMEOUT" \
    "B_NETNS='$b_netns' B_SENTRY_PID='$b_pid' BDIR='$bdir' CKPT='$ckpt' RCID='$rcid' /var/tmp/single-container-thaw-into-b-node.sh 2>&1")
  thaw_ms=$(printf '%s\n' "$thaw_out" | sed -n 's/.*THAW_MS=\([0-9][0-9]*\).*/\1/p' | head -1)
  RIP_THAW_MS="$thaw_ms"
  if grep -q 'PROVEN\|netns inode' <<<"$thaw_out"; then
    pass "[$variant] agent RESUMED into the CLAIMED pool member's netns (restore≈${thaw_ms:-?}ms; restored task shares the member's kubelet-owned netns)"
  else
    fail "[$variant] resume into pool member" "restore did not prove netns graft: $(printf '%s\n' "$thaw_out" | tail -6)"
  fi

  # Member must be ALIVE right after the restore — the gate below runs on a live
  # member (the ka-0000007916 scenario: the ack/release happens with it alive).
  local phase_pre
  phase_pre=$(k get pod "$member" -o jsonpath='{.status.phase}' 2>/dev/null)
  [ "$phase_pre" = "Running" ] \
    && pass "[$variant] pool member ALIVE post-resume (phase=Running)" \
    || fail "[$variant] pool member alive post-resume" "phase='$phase_pre' (want Running)"

  # ── ASSERT (1) /work SURVIVED ── file token byte-identical after the resume.
  local post_token
  post_token=$(node_exec 30 "cat '$bdir/work/state.bin' 2>/dev/null" | tr -d '\r')
  if [ "$post_token" = "$sentinel" ]; then
    pass "[$variant] (1) /work INTACT — state.bin survived freeze→resume ($sentinel)"
  else
    fail "[$variant] (1) /work intact" "post-resume state.bin='$post_token' want '$sentinel'"
  fi

  # ── ASSERT (2) RAM SURVIVED ── in-RAM counter climbed PAST the frozen value.
  # Poll the host-visible counter (the restored process flushes it each tick).
  local resumed=0 c
  for _ in 1 2 3 4 5 6 7 8; do
    c=$(node_exec 20 "tail -1 '$bdir/work/agent.log' 2>/dev/null | python3 -c 'import sys,json;print(json.load(sys.stdin)[\"counter\"])' 2>/dev/null" | tr -d '\r')
    if [ -n "$c" ] && [ "$c" -gt "${RIP_FROZEN_COUNTER:-0}" ] 2>/dev/null; then resumed="$c"; break; fi
    sleep 1
  done
  if [ "$resumed" != 0 ] && [ "$resumed" -gt "${RIP_FROZEN_COUNTER:-0}" ] 2>/dev/null; then
    pass "[$variant] (2) RAM INTACT — in-RAM counter climbed $RIP_FROZEN_COUNTER → $resumed past freeze (same live process, working_set=$working_set preserved)"
  else
    fail "[$variant] (2) RAM intact" "counter '$resumed' not > frozen '$RIP_FROZEN_COUNTER' (restore did not resume the live process)"
  fi
  # For active-Claude, also confirm the loaded heap working set is reported intact
  # by the resumed process (repo_loaded == working_set) — a RAM number on an empty
  # heap would not prove a Claude working set survived.
  if [ "$working_set" -gt 0 ] 2>/dev/null; then
    local repo_loaded
    repo_loaded=$(node_exec 20 "tail -1 '$bdir/work/agent.log' 2>/dev/null | python3 -c 'import sys,json;print(json.load(sys.stdin)[\"repo_loaded\"])' 2>/dev/null" | tr -d '\r')
    if [ "$repo_loaded" = "$working_set" ]; then
      pass "[$variant] (2b) loaded working set INTACT — resumed process reports repo_loaded=$repo_loaded (=$working_set)"
    else
      fail "[$variant] (2b) working set intact" "resumed repo_loaded='$repo_loaded' want '$working_set'"
    fi
  fi

  # ── ASSERT (3) gb + coop REHYDRATE-ACK RELEASE within deadline ──
  # The REAL controller gate (thawepoch.WaitForAcks) fed by the REAL agent
  # watchers, keyed to the CLAIMED MEMBER's pod-uid. Both consumers ack the current
  # epoch and the gate releases within THAW_REHYDRATE_DEADLINE.
  blue "  [rehydrate-ack] real thawepoch.WaitForAcks over coop+gb acks (deadline ${REHYDRATE_DEADLINE_MS}ms, pod-uid=$uid)"
  local rel_out rel_line rel_released rel_elapsed rel_epoch
  rel_out=$(node_exec "$NODE_CALL_TIMEOUT" \
    "/var/tmp/thawepoch-probe -mode=release -state-dir='$statedir' -deadline-ms=$REHYDRATE_DEADLINE_MS -pod-uid='$uid' 2>&1")
  rel_line=$(printf '%s\n' "$rel_out" | grep '^THAWEPOCH_PROBE' | head -1)
  dim "    $rel_line"
  rel_released=$(printf '%s' "$rel_line" | sed -n 's/.*released=\([a-z]*\).*/\1/p')
  rel_elapsed=$(printf '%s' "$rel_line" | sed -n 's/.*elapsed_ms=\([0-9]*\).*/\1/p')
  rel_epoch=$(printf '%s' "$rel_line" | sed -n 's/.*epoch=\([0-9]*\).*/\1/p')
  if [ "$rel_released" = "true" ]; then
    pass "[$variant] (3) gb+coop acked epoch ${rel_epoch:-?}; WaitForAcks RELEASED the resumed agent"
  else
    fail "[$variant] (3) rehydrate-ack RELEASE" "gate did NOT release (released='$rel_released'): $(printf '%s\n' "$rel_out" | tail -4)"
  fi
  if [ -n "$rel_elapsed" ] && [ "$rel_elapsed" -lt "$REHYDRATE_DEADLINE_MS" ] 2>/dev/null; then
    pass "[$variant] (3b) RELEASE within deadline: ${rel_elapsed}ms < ${REHYDRATE_DEADLINE_MS}ms (not fail-closing the full window)"
  else
    fail "[$variant] (3b) RELEASE within deadline" "elapsed_ms='${rel_elapsed:-NA}' not < ${REHYDRATE_DEADLINE_MS}ms — the ka-0000007916 fail-close symptom"
  fi
  # Armed + ack + marker files present on the substrate FS (thawepoch.ArmedPath /
  # AckPath) — the round-trip actually landed on disk, not just in memory.
  local fs_ok
  fs_ok=$(node_exec 30 "
    ok=1
    for c in coop gb; do
      [ -f \"$statedir/thaw-epoch.\$c.armed\" ] || ok=0
      [ -f \"$statedir/thaw-epoch.\$c.done\" ]  || ok=0
    done
    [ -f '$statedir/thaw-epoch.json' ] || ok=0
    echo FS_OK=\$ok" | tr -d '\r' | sed -n 's/.*FS_OK=//p')
  [ "$fs_ok" = "1" ] \
    && pass "[$variant] (3c) ArmedPath + Ack + marker files present on substrate FS ($statedir)" \
    || fail "[$variant] (3c) thaw-state FS artifacts" "expected coop/gb .armed + .done + thaw-epoch.json under $statedir"

  # ── ASSERT (4) fail-close negative control ── coop silent → gate HOLDS.
  local fc_out fc_line fc_released fc_failed
  fc_out=$(node_exec 60 \
    "/var/tmp/thawepoch-probe -mode=failclose -state-dir='${statedir}-fc' -pod-uid='$uid' 2>&1")
  fc_line=$(printf '%s\n' "$fc_out" | grep '^THAWEPOCH_PROBE' | head -1)
  dim "    $fc_line"
  fc_released=$(printf '%s' "$fc_line" | sed -n 's/.*released=\([a-z]*\).*/\1/p')
  fc_failed=$(printf '%s' "$fc_line" | sed -n 's/.*failed=\[\([^]]*\)\].*/\1/p')
  if [ "$fc_released" = "false" ] && [ "$fc_failed" = "coop" ]; then
    pass "[$variant] (4) fail-close proven — coop silent → gate HELD (released=false, failed=[coop])"
  else
    fail "[$variant] (4) fail-close negative control" "released='$fc_released' failed='[$fc_failed]' (want released=false, failed=[coop])"
  fi

  # ── ASSERT (5) member alive across the gate + pool consumed ──
  local phase_post standby_final
  phase_post=$(k get pod "$member" -o jsonpath='{.status.phase}' 2>/dev/null)
  standby_final=$(k get pod "$member" -o jsonpath='{.metadata.labels.gardenio\.io/pool-standby}' 2>/dev/null)
  [ "$phase_post" = "Running" ] \
    && pass "[$variant] (5) pool member STILL Running after RELEASE — ack/release completed with it alive" \
    || fail "[$variant] (5) member alive across gate" "phase='$phase_post' after release (want Running)"
  [ "$standby_final" = "false" ] \
    && pass "[$variant] (5b) pool CONSUMED — the claimed member is no longer an idle standby (standby=false)" \
    || fail "[$variant] (5b) pool consumed" "pool-standby='$standby_final' after resume (want false — a resume must draw the member out of the standby pool)"

  echo "    ── ${variant} summary: frozen=${RIP_FROZEN_COUNTER} resumed=${resumed} thaw≈${RIP_THAW_MS:-?}ms release=${rel_elapsed:-NA}ms/${REHYDRATE_DEADLINE_MS}ms"
}

# ── Drive the requested variants ────────────────────────────────────
blue "[3/4] driving variant ladders"
for v in $ONLY_VARIANT; do
  case "$v" in
    claudeless)     run_variant "claudeless"    "$CLAUDELESS_WORKING_SET" || true ;;
    active-claude)  run_variant "active-claude" "$ACTIVE_CLAUDE_WORKING_SET" || true ;;
    *) fail "unknown variant '$v'" "ONLY_VARIANT must be a subset of 'claudeless active-claude'" ;;
  esac
done

# ── 4. Summary ──────────────────────────────────────────────────────
blue "[4/4] acceptance gate summary"
echo ""
echo "════════════════════════════════════════════════════"
echo " resume-into-pool ACCEPTANCE GATE Summary: $PASS passed, $FAIL failed"
echo "   variants: $ONLY_VARIANT   deadline=${REHYDRATE_DEADLINE_MS}ms"
echo "════════════════════════════════════════════════════"
if [ "$FAIL" -gt 0 ]; then
  for e in "${ERRORS[@]}"; do red "  - $e"; done
  exit 1
fi
green "ACCEPTANCE GATE GREEN — freeze→resume-into-pool holds /work + RAM + gb/coop acks for every variant."
exit 0
