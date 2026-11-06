#!/usr/bin/env bash
# test-gvisor-renudge-thaw.sh — E2E: re-nudge mid-THAW coordination on the REAL
# gvisor-poc substrate. Task ka-0000001841 (epic ka-0000004667 "gVisor production
# readiness"; impl ka-0000005830 / design ka-0000005125, MR !4719). The mid-THAW
# companion to test-gvisor-rehydrate-ack.sh — and structurally a mirror of it.
#
# THE GAP THIS CLOSES:
#   MR !4719 landed the re-nudge mid-THAW coordination: a user message arriving
#   WHILE a thaw of the target agent is in flight (Pod B restoring/rehydrating)
#   must NOT race a half-restored agent or mint a second Pod B. Unlike freeze,
#   thaw has NO live fallback Pod A, so the message is BUFFERED (bridge
#   waitForThawAndNudge, keyed on the controller's IsThawing / GET
#   /api/v1/thaw/status) until the rehydrated Pod B is live, then delivered
#   EXACTLY ONCE — and a concurrent spawn / reconciler pass during the thaw must
#   JOIN the in-flight thaw (single-flight in FreezeThawManager.Thaw) rather than
#   drive a second `runsc restore` (DEDUP, the ka-0000005680 admit-fail class).
#
#   The contract is asserted at the unit layer (freezethaw_singleflight_test.go,
#   bot_mentions_thaw_buffer_test.go, thaw_status_endpoint_test.go) — but NOTHING
#   drove the REAL manager single-flight + IsThawing against a concurrent-thaw
#   round-trip on the ACTUAL substrate while a real runsc-restored agent is ALIVE
#   in Pod B's netns. THIS module is that missing piece.
#
# WHY THE REAL Go CODE, NOT A BASH RE-IMPLEMENTATION:
#   This module builds + runs cmd/renudge-thaw-probe, which constructs the
#   GENUINE internal/podmanager.FreezeThawManager and drives the REAL
#   FreezeThawManager.Thaw single-flight + FreezeThawManager.IsThawing — the
#   exact primitives MR !4719 added and the bridge buffer keys on. A regression
#   in the single-flight (a second concurrent thaw minting a duplicate Pod B) or
#   in IsThawing (the buffer's hold/release signal) fails THIS e2e, not just the
#   next prod canary. The probe intercepts only the restore SCRIPT exec (Pod B is
#   already restored by this harness; the restore MECHANICS are proven by the
#   freeze-thaw / rehydrate-ack siblings) so it isolates the COORDINATION.
#
# HOW IT WORKS (reuses the proven freeze/thaw + rehydrate-ack node mechanism):
#   1. Schedule a privileged hostPID+hostPath probe onto the gvisor-poc node.
#   2. Build a single-container runsc agent bundle + `runsc checkpoint` (FREEZE)
#      via the proven single-container-resume-node.sh (CHECKPOINT_ONLY=1).
#   3. Create a REAL kubelet-owned runsc placeholder Pod B, wait Ready, resolve
#      its netns, and `runsc restore` (THAW) the checkpoint INTO Pod B's netns
#      via single-container-thaw-into-b-node.sh. Pod B is now ALIVE with the
#      restored agent task running in its kubelet-provisioned netns.
#   4. With Pod B demonstrably ALIVE, run cmd/renudge-thaw-probe on the node:
#        - dedup  : two concurrent Thaw calls for the same agent — the genuine
#                   single-flight must JOIN them (one restore, IsThawing true
#                   in-flight, same Pod B, exactly-once), then clear IsThawing.
#        - clears : a sequential second wake runs a FRESH restore (negative
#                   control — the single-flight is join-WHILE-in-flight, not a
#                   degenerate always-one-restore that would suppress a later
#                   legitimate thaw).
#   5. Re-assert Pod B is STILL Running after the probe — the coordination ran
#      WITH Pod B alive (the mid-thaw scenario), not after a teardown.
#
# runsc QUIRK: `runsc checkpoint`/`restore` complete the op then hang on process
#   EXIT (documented in test-gvisor-freeze-thaw.sh). Every runsc call is wrapped
#   in `timeout` and asserted on OBSERVED post-state, never on rc.
#
# OPT-IN / GATING (NEVER silently passes):
#   Targets the dedicated gvisor-poc namespace + runsc node pool. Runs only when
#   GVISOR_E2E=1 (or RENUDGE_THAW_E2E=1) AND the substrate is present (runsc
#   RuntimeClass + create-pods in $GVISOR_NS). Opt-in unset → SKIP clean (exit 0,
#   inert in the standard shards). Opt-in SET but substrate ABSENT → SKIP LOUD
#   (still exit 0 so an absent node pool does not red the suite, but a skip is
#   never mistaken for a pass).
#   Wire it in via:  GVISOR_E2E=1 ONLY=gvisor-renudge-thaw run-shard.sh
#
# Prerequisites (same as test-gvisor-rehydrate-ack.sh, escalation ka-0000004674):
#   - kubectl context on the gvisor-poc cluster
#   - ns gvisor-poc (PSA=privileged), RuntimeClass runsc, NodePool gvisor-poc
#   - SA gardenio-agent: create pods + pods/exec in gvisor-poc
#   - a Go toolchain in the task (to build cmd/renudge-thaw-probe from source)
#
# Usage:
#   GVISOR_E2E=1 ./gardenio/tests/e2e/scripts/test-gvisor-renudge-thaw.sh
#   GVISOR_E2E=1 KEEP_PODS=1 ./...                                  # debug
set -uo pipefail

# ── Config (all overridable via env) ────────────────────────────────
GVISOR_NS="${GVISOR_NS:-gvisor-poc}"
RUNTIME_CLASS="${RUNTIME_CLASS:-runsc}"
RUN_ID="${RUN_ID:-$$}"
PROBE="${PROBE:-renudge-thaw-probe-${RUN_ID}}"
# Where the proven node scripts live (shipped to the node via base64).
POC_DIR="${POC_DIR:-gardenio/tests/e2e/scripts/gvisor-node}"
# Controller module dir (to build cmd/renudge-thaw-probe from source).
CONTROLLER_DIR="${CONTROLLER_DIR:-gardenio/controller}"
BUSYBOX_IMAGE="${BUSYBOX_IMAGE:-public.ecr.aws/docker/library/python:3.11-slim}"
POD_READY_TIMEOUT="${POD_READY_TIMEOUT:-180s}"
# Agent name the probe keys the thaw single-flight on (per-run so concurrent
# dispatches don't collide).
PROBE_AGENT="${PROBE_AGENT:-renudge-agent-${RUN_ID}}"

# Shared gvisor-poc substrate hardening helpers: gvisor_warm_node_gate +
# gvisor_retry_step (decision ka-0000000122, root flake ka-0000007234).
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
    red " SKIP gvisor-renudge-thaw (LOUD): $why"
    red " OPT-IN WAS SET but the gvisor-poc substrate is absent — this run did"
    red " NOT assert the re-nudge mid-THAW coordination. A green suite here proves"
    red " NOTHING about the single-flight DEDUP or the IsThawing buffer signal."
    red " Provision the gvisor-poc node pool + runsc RuntimeClass (ka-0000004674)."
    red "════════════════════════════════════════════════════"
  else
    blue "SKIP gvisor-renudge-thaw: $why"
    dim  "  Opt-in module: set GVISOR_E2E=1 (or RENUDGE_THAW_E2E=1) and run on the"
    dim  "  gvisor-poc substrate (runsc RuntimeClass + create-pods in ns $GVISOR_NS)."
  fi
  if [ -n "${E2E_SKIP_SENTINEL:-}" ]; then printf '%s\n' "$*" >"$E2E_SKIP_SENTINEL" 2>/dev/null || true; fi  # skip visibility (ka-0000000989)
  exit 0
}

k() { kubectl -n "$GVISOR_NS" "$@"; }

# Per-node-call wall-clock cap (runsc can HANG on process exit). Mirrors
# test-gvisor-rehydrate-ack.sh node_exec.
NODE_CALL_TIMEOUT="${NODE_CALL_TIMEOUT:-120}"
node_exec() {
  local cap="$1"; shift
  timeout "$((cap + 20))" kubectl -n "$GVISOR_NS" exec "$PROBE" -- timeout "$cap" chroot /host bash -c "$1" 2>/dev/null
}

cleanup() {
  blue "── cleanup ──"
  node_exec 30 "rm -rf /var/tmp/renudge-thaw-probe 2>/dev/null || true" >/dev/null 2>&1 || true
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
echo " gVisor re-nudge mid-THAW coordination — real substrate E2E (ka-0000001841)"
echo " ns=$GVISOR_NS runtimeClass=$RUNTIME_CLASS agent=$PROBE_AGENT"
echo "════════════════════════════════════════════════════"

# ── Gate 0: opt-in + substrate preflight ────────────────────────────
if [ "${GVISOR_E2E:-0}" != "1" ] && [ "${RENUDGE_THAW_E2E:-0}" != "1" ]; then
  skip_all "GVISOR_E2E / RENUDGE_THAW_E2E != 1 (opt-in module)" off
fi
command -v kubectl >/dev/null 2>&1 || skip_all "kubectl not installed" loud
command -v go >/dev/null 2>&1 || skip_all "go toolchain not present (need it to build cmd/renudge-thaw-probe)" loud
kubectl get runtimeclass "$RUNTIME_CLASS" >/dev/null 2>&1 \
  || skip_all "RuntimeClass '$RUNTIME_CLASS' not present on this cluster" loud
[ "$(kubectl auth can-i create pods -n "$GVISOR_NS" 2>/dev/null)" = "yes" ] \
  || skip_all "cannot create pods in ns '$GVISOR_NS'" loud
[ -f "$POC_DIR/single-container-resume-node.sh" ] \
  || skip_all "node script single-container-resume-node.sh missing under $POC_DIR" loud
[ -f "$POC_DIR/single-container-thaw-into-b-node.sh" ] \
  || skip_all "node script single-container-thaw-into-b-node.sh missing under $POC_DIR" loud
[ -d "$CONTROLLER_DIR/cmd/renudge-thaw-probe" ] \
  || skip_all "cmd/renudge-thaw-probe missing under $CONTROLLER_DIR" loud

blue "Substrate present — running re-nudge mid-THAW coordination assertions."

# ── 1. Build cmd/renudge-thaw-probe (the REAL single-flight + IsThawing driver) ─
# Cross-compile for the node (linux/amd64, static). This is the genuine
# FreezeThawManager.Thaw single-flight + IsThawing production code — NOT a
# re-implementation.
blue "[1/6] building cmd/renudge-thaw-probe (real FreezeThawManager.Thaw single-flight + IsThawing)"
PROBE_BIN="$(mktemp /tmp/renudge-thaw-probe.XXXXXX)"
if ( cd "$CONTROLLER_DIR" && CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -o "$PROBE_BIN" ./cmd/renudge-thaw-probe/ ) >"/tmp/renudge-build-${RUN_ID}.log" 2>&1; then
  pass "renudge-thaw-probe built ($(wc -c < "$PROBE_BIN" | tr -d ' ') bytes, linux/amd64 static)"
else
  fail "build renudge-thaw-probe" "go build failed: $(tail -4 "/tmp/renudge-build-${RUN_ID}.log")"
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
  labels: {app: renudge-thaw-probe, test: ka-0000001841}
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
# ka-0000007234). Same rationale as test-gvisor-rehydrate-ack.sh.
if gvisor_warm_node_gate node_exec; then
  pass "gvisor-poc node warm (runsc present + /var/tmp writable + runsc-root reachable)"
else
  fail "gvisor-poc node warm-gate" "$GVISOR_WARM_DIAG"
  echo "════════ Summary: $PASS passed, $FAIL failed ════════"
  exit 1
fi

# Ship the proven node scripts + the renudge-thaw-probe binary (base64), wrapped
# in a bounded signature-keyed retry (ka-0000007234 self-heal). Same idiom as
# test-gvisor-rehydrate-ack.sh's ship_node_assets.
ship_node_assets() {
  for s in single-container-resume-node.sh single-container-thaw-into-b-node.sh; do
    local b64; b64="$(base64 -w0 "$POC_DIR/$s")"
    k exec "$PROBE" -- chroot /host bash -c \
      "echo $b64 | base64 -d > /var/tmp/$s && chmod +x /var/tmp/$s" >/dev/null 2>&1 || return 1
  done
  base64 -w0 "$PROBE_BIN" | k exec -i "$PROBE" -- chroot /host bash -c \
    'base64 -d > /var/tmp/renudge-thaw-probe && chmod +x /var/tmp/renudge-thaw-probe' >/dev/null 2>&1 || return 1
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
  local out; out="$(node_exec 30 '/var/tmp/renudge-thaw-probe -mode=dedup 2>&1 | grep -q "pod-b is required"; echo RC=$?')" || return 1
  grep -q 'RC=0' <<<"$out"
}
if gvisor_retry_step node-asset-ship 4 3 ship_node_assets; then
  pass "node scripts + renudge-thaw-probe binary shipped + executable on the node (attempts=$GVISOR_STEP_ATTEMPTS)"
else
  fail "ship node assets" "base64 copy to /var/tmp failed or binary not executable on node — $GVISOR_STEP_DIAG"
  echo "════════ Summary: $PASS passed, $FAIL failed ════════"
  exit 1
fi

# ── 3. FREEZE: build the single-container agent bundle + checkpoint ──
blue "[3/6] FREEZE: build single-container agent + runsc checkpoint (/var/tmp/ckSC)"
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
# Identical to test-gvisor-rehydrate-ack.sh resolve_b.
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
POD_B="renudge-thaw-podb-${RUN_ID}"
cat <<YAML | k apply -f - >/dev/null 2>&1
apiVersion: v1
kind: Pod
metadata:
  name: ${POD_B}
  namespace: ${GVISOR_NS}
  labels: {app: renudge-thaw-podb, test: ka-0000001841, role: restore-placeholder}
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
# Bounded signature-keyed retry for the netns resolve (ka-0000007234).
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

# Assert Pod B is ALIVE right after the restore — the mid-thaw scenario holds Pod
# B alive while the coordination runs.
B_PHASE_PRE=$(k get pod "$POD_B" -o jsonpath='{.status.phase}' 2>/dev/null)
if [ "$B_PHASE_PRE" = "Running" ]; then
  pass "Pod B ALIVE post-restore (phase=Running) — re-nudge coordination runs on a live netns (the mid-THAW scenario)"
else
  fail "Pod B alive post-restore" "Pod B phase='$B_PHASE_PRE' (expected Running); cannot assert mid-thaw coordination on a live netns"
fi

# ── 5. COORDINATION: real Thaw single-flight + IsThawing on the node ────
# Run cmd/renudge-thaw-probe on the node beside the live Pod B. dedup proves the
# headline invariant (join → one Pod B, IsThawing true→false, exactly-once);
# clears is the negative control (sequential wake runs a fresh restore).
blue "[5/6] COORDINATION: real FreezeThawManager single-flight + IsThawing (dedup + clears)"

DEDUP_OUT=$(node_exec "$NODE_CALL_TIMEOUT" \
  "/var/tmp/renudge-thaw-probe -mode=dedup -agent='$PROBE_AGENT' -pod-b='$POD_B' -pod-b-uid='$B_UID' -namespace='$GVISOR_NS' 2>&1")
DEDUP_LINE=$(printf '%s\n' "$DEDUP_OUT" | grep '^THAW_RENUDGE_PROBE' | head -1)
dim "    $DEDUP_LINE"
D_INFLIGHT=$(printf '%s' "$DEDUP_LINE" | sed -n 's/.*inflight_thawing=\([a-z]*\).*/\1/p')
D_RESTORES=$(printf '%s' "$DEDUP_LINE" | sed -n 's/.*restores=\([0-9]*\).*/\1/p')
D_SAMEPODB=$(printf '%s' "$DEDUP_LINE" | sed -n 's/.*same_podb=\([a-z]*\).*/\1/p')
D_CLEARED=$(printf '%s' "$DEDUP_LINE" | sed -n 's/.*cleared_thawing=\([a-z]*\).*/\1/p')
D_OK=1; grep -q 'ok=false' <<<"$DEDUP_LINE" && D_OK=0

# (a) IsThawing true in-flight — the buffer signal that holds the mid-thaw nudge.
if [ "$D_INFLIGHT" = "true" ]; then
  pass "(a) IsThawing=true while thaw in flight — bridge buffers the mid-thaw nudge (does not race a half-restored agent)"
else
  fail "IsThawing in-flight signal" "inflight_thawing='$D_INFLIGHT' (expected true) — the buffer would NOT hold a mid-thaw nudge"
fi
# (b) exactly one restore — the DEDUP single-flight invariant (no duplicate Pod B).
if [ "$D_RESTORES" = "1" ]; then
  pass "(b) exactly 1 restore for 2 concurrent thaws — single-flight JOINED the 2nd (no duplicate Pod B, ka-0000005680 class)"
else
  fail "single-flight DEDUP" "restores='$D_RESTORES' (expected 1) — a concurrent thaw started a 2nd runsc restore → duplicate Pod B"
fi
# (c) joiner adopted the owner's Pod B + no error — exactly-once delivery target.
if [ "$D_SAMEPODB" = "true" ] && [ "$D_OK" = "1" ]; then
  pass "(c) joiner adopted the owner's Pod B, both returned no error — the buffered nudge lands exactly-once on one rehydrated agent"
else
  fail "single-flight join result" "same_podb='$D_SAMEPODB' ok=$D_OK — joiner did not share the owner's Pod B/outcome: $(printf '%s\n' "$DEDUP_OUT" | tail -4)"
fi
# (d) IsThawing cleared after the thaw resolved — the buffer's release condition.
if [ "$D_CLEARED" = "true" ]; then
  pass "(d) IsThawing cleared after thaw resolved — the buffer releases → the nudge is delivered (not stranded at the deadline)"
else
  fail "IsThawing release signal" "cleared_thawing='$D_CLEARED' (expected true) — the buffer would never release"
fi

# Negative control — a sequential second wake must run a FRESH restore.
blue "    negative control: sequential second wake → a FRESH restore (single-flight clears on return)"
CLEARS_OUT=$(node_exec "$NODE_CALL_TIMEOUT" \
  "/var/tmp/renudge-thaw-probe -mode=clears -agent='${PROBE_AGENT}-seq' -pod-b='$POD_B' -pod-b-uid='$B_UID' -namespace='$GVISOR_NS' 2>&1")
CLEARS_LINE=$(printf '%s\n' "$CLEARS_OUT" | grep '^THAW_RENUDGE_PROBE' | head -1)
dim "    $CLEARS_LINE"
C_RESTORES=$(printf '%s' "$CLEARS_LINE" | sed -n 's/.*restores=\([0-9]*\).*/\1/p')
C_BETWEEN=$(printf '%s' "$CLEARS_LINE" | sed -n 's/.*between_thawing=\([a-z]*\).*/\1/p')
if [ "$C_RESTORES" = "2" ] && [ "$C_BETWEEN" = "false" ]; then
  pass "negative control: two sequential wakes ⇒ two restores, IsThawing false between — join is WHILE-in-flight, not stuck-forever"
else
  fail "single-flight clears-on-return" "restores='$C_RESTORES' between_thawing='$C_BETWEEN' (expected 2 / false): $(printf '%s\n' "$CLEARS_OUT" | tail -4)"
fi

# ── 6. Pod B still alive after the coordination ────────────────────
blue "[6/6] Pod B liveness across the coordination window"
B_PHASE_POST=$(k get pod "$POD_B" -o jsonpath='{.status.phase}' 2>/dev/null)
if [ "$B_PHASE_POST" = "Running" ]; then
  pass "Pod B STILL Running after the coordination — single-flight + IsThawing ran WITH Pod B alive (the mid-THAW scenario)"
else
  fail "Pod B alive across coordination" "Pod B phase='$B_PHASE_POST' after the probe (expected Running)"
fi

# ── Summary ─────────────────────────────────────────────────────────
echo ""
echo "════════════════════════════════════════════════════"
echo " gVisor re-nudge mid-THAW Summary: $PASS passed, $FAIL failed"
echo "   thaw≈${THAW_MS:-?}ms  dedup_restores=${D_RESTORES:-?}  clears_restores=${C_RESTORES:-?}  agent=$PROBE_AGENT"
echo "════════════════════════════════════════════════════"
if [ "$FAIL" -gt 0 ]; then
  for e in "${ERRORS[@]}"; do red "  - $e"; done
  exit 1
fi
green "All re-nudge mid-THAW assertions passed — single-flight DEDUP (1 restore, exactly-once) + IsThawing buffer signal (hold→release) on a live Pod B."
exit 0
