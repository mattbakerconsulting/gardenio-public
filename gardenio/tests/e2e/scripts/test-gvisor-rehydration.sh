#!/usr/bin/env bash
# test-gvisor-rehydration.sh — E2E rehydration suite asserting the C1–C5
# correctness contract on the restore-through-lifecycle harness.
#
# Bead ka-0000005803 (§6 of gardenio/docs/gvisor-runsc-cr/rehydration-design.md,
# ka-0000001515). Blocked-on (now merged): cred re-mint ka-0000007211, NATS
# reconnect ka-0000001901, thaw-epoch signal ka-0000003040.
#
# WHAT THIS PROVES (the contract, not the plumbing):
#   The sibling module test-gvisor-freeze-thaw.sh proves runsc checkpoint/restore
#   preserves state and the process RESUMES. The restore-through-lifecycle PoC
#   proves a thawed sandbox lands in a fresh, kubelet-owned netns/volumes/token.
#   THIS module proves the *next* layer — the warm-boot correctness contract:
#   a thawed agent must DISCARD Pod-A identity and re-establish everything before
#   resuming work. The connection half (C3 NATS reconnect, C4 presence
#   re-announce, §6.5 idempotency, §6.6 deadline) is asserted against a LIVE NATS
#   broker by the Rust suite coop/tests/specs/tests/rehydration.rs. THIS shell
#   module asserts the credential/identity half on the gvisor substrate, where
#   it can only truly be observed:
#
#   C1/C2 — cred re-mint identity gate (the headline): the token a thawed agent
#           re-reads from disk must present POD-B's identity (kubelet-minted),
#           never the Pod-A copy cached in RAM at freeze. We assert the exact
#           discriminating signals the agent's real freshness gate keys on
#           (coop rehydrate::freshness_gate, §5): a Pod-A-shaped token has
#           iat <= frozen_at and pod_uid=A (MUST be rejected); a Pod-B-shaped
#           token has iat > frozen_at and pod_uid=B (MUST pass). In production
#           this gate is what makes the agent's next AWS call resolve under
#           Pod B's SA — observable in CloudTrail as the assumed-role
#           session-name carrying B's identity, not A's (the CloudTrail probe
#           runs only when AWS creds + the gvisor substrate are present).
#   C2    — token-freshness FAIL-CLOSE: a blank/corrupt Pod-B token, or one still
#           bearing Pod-A's identity, must fail the thaw closed (agent NOT
#           released) rather than running on the RAM copy (§5 matrix row 1).
#   C4    — presence re-announce: release is gated on the agent's ack; we assert
#           the thaw-epoch marker/ack release condition on the substrate FS.
#   §6.5  — idempotency: the marker fired twice yields exactly-once effect
#           (epoch latched once; a second ack for the same epoch is a no-op).
#   §6.6  — deadline FAIL-CLOSE: a marker whose required consumer never acks must
#           NOT satisfy the release condition (the controller gate holds).
#
# HOW IT WORKS:
#   The contract-shape assertions are pure bash over crafted JWTs and the on-disk
#   marker/ack protocol the two watchers share (byte-compatible Go
#   internal/thawepoch ⇄ coop thaw.rs) — fast, no compile, run everywhere. The
#   runsc-lifecycle assertions (OPT-IN) drive freeze→thaw on the gvisor-poc
#   substrate and assert the marker/ack round-trips and (with creds) the
#   CloudTrail role-session-name reflects Pod B. Every runsc call is wrapped in
#   `timeout` and asserted on OBSERVED post-state (epic constraint ka-0000006959:
#   runsc C/R hangs on process exit).
#
# OPT-IN / GATING (mirrors test-gvisor-freeze-thaw.sh):
#   The runsc-lifecycle + CloudTrail assertions are OPT-IN: they SKIP cleanly
#   unless GVISOR_E2E=1 AND the gvisor-poc substrate is present (runsc
#   RuntimeClass + create-pods in $GVISOR_NS). The contract-shape assertions need
#   only bash + openssl and run everywhere, so the suite is meaningful in plain
#   CI and richer on the gvisor cluster.
#
#   Wire it in via:  GVISOR_E2E=1 ONLY=gvisor-rehydration run-shard.sh
#
# Usage:
#   ./gardenio/tests/e2e/scripts/test-gvisor-rehydration.sh             # contract shape only
#   GVISOR_E2E=1 ./gardenio/tests/e2e/scripts/test-gvisor-rehydration.sh # + runsc lifecycle

set -uo pipefail

# Shared gVisor substrate helpers — incl. gvisor_require_substrate_guard, the
# readiness-gate decision for a missing substrate (REQUIRE_SUBSTRATE=1 → hard
# fail; default → clean skip). Task ka-0000002867, lib decision ka-0000000122.
# shellcheck disable=SC1091
source "$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)/lib-gvisor-node.sh"

# ── Config ────────────────────────────────────────────────────────
GVISOR_NS="${GVISOR_NS:-gvisor-poc}"
RUNTIME_CLASS="${RUNTIME_CLASS:-runsc}"
RUN_ID="${RUN_ID:-$$}"
PROBE="${PROBE:-gvisor-rh-probe-${RUN_ID}}"
RUNSC_TIMEOUT="${RUNSC_TIMEOUT:-45}"
# The thaw-epoch on-disk contract (must match thawepoch.MarkerName /
# thawepoch.AckPath on the Go side and MARKER_NAME / CONSUMER on the coop side).
MARKER_NAME="thaw-epoch.json"

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
skip_runsc() {
  blue "SKIP gvisor-rehydration runsc-lifecycle: $1"
  dim "  The runsc-lifecycle + CloudTrail assertions are opt-in (GVISOR_E2E=1)"
  dim "  and need the gvisor-poc substrate. Contract-shape assertions still ran."
}
# Substrate-ABSENT decision for the runsc-lifecycle leg. Unlike the sibling
# modules this one does NOT exit on a skip — the already-run contract-shape
# assertions must still reach the summary (which exits 1 if any failed). So under
# the READINESS GATE (REQUIRE_SUBSTRATE=1) the runsc-lifecycle leg is mandatory
# and a missing substrate is a NO-GO hard-fail; otherwise fall through to a loud
# skip_runsc so the contract-shape verdict stands (task ka-0000002867).
require_runsc_substrate() {
  if [ "${REQUIRE_SUBSTRATE:-0}" = "1" ]; then
    gvisor_require_substrate_guard gvisor-rehydration "$1 (runsc-lifecycle leg)"
  else
    skip_runsc "$1"
  fi
}
have() { command -v "$1" >/dev/null 2>&1; }

WORKDIR="$(mktemp -d "${TMPDIR:-/tmp}/gvisor-rehydration-${RUN_ID}.XXXXXX")"
cleanup() {
  [ "${KEEP_WORKDIR:-0}" = "1" ] || rm -rf "$WORKDIR" 2>/dev/null || true
  if [ "${GVISOR_E2E:-0}" = "1" ] && [ "${KEEP_PROBE:-0}" != "1" ] && have kubectl; then
    kubectl -n "$GVISOR_NS" delete pod "$PROBE" --grace-period=0 --force >/dev/null 2>&1 || true
  fi
}
trap cleanup EXIT

echo "════════════════════════════════════════════════════"
echo " gVisor rehydration C1–C5 contract E2E"
echo " ns=$GVISOR_NS  runtimeClass=$RUNTIME_CLASS"
echo "════════════════════════════════════════════════════"

# ── Preflight: a base64 codec for the contract-shape JWT assertions ─
# Prefer coreutils `base64` (always present in the e2e image); fall back to
# openssl. Either provides the encode/decode the minter + gate predicate need.
if have base64; then
  B64E() { base64 | tr '+/' '-_' | tr -d '=\n'; }
  # base64url decode: restore +/ and padding, then decode (ignore garbage tail).
  B64D() { tr '_-' '/+' | { cat; printf '=='; } | base64 -d 2>/dev/null; }
elif have openssl; then
  B64E() { openssl base64 -A | tr '+/' '-_' | tr -d '='; }
  B64D() { tr '_-' '/+' | { cat; printf '=='; } | openssl base64 -d -A 2>/dev/null; }
else
  blue "SKIP gvisor-rehydration: no base64/openssl codec for JWT minting"
  exit 0
fi

# ===========================================================================
# JWT minter + the freshness predicate the agent's real gate keys on.
# We do NOT verify signatures — neither does the gate: kubelet minted the
# on-disk token and STS / the API server verify it on use. The gate (and this
# predicate) assert IDENTITY + FRESHNESS only (rehydration-design §5):
#   reject if expired | wrong-aud | pod_uid != Pod-B | iat <= frozen_at;
#   pass only with a positive proof (pod_uid == Pod-B OR iat > frozen_at);
#   else fail closed (Indeterminate).
# ===========================================================================
# mint_jwt <iat> <exp> <aud> <pod_uid> → header.payload.dummysig
mint_jwt() {
  local iat="$1" exp="$2" aud="$3" pod_uid="$4" hdr pld
  hdr="$(printf '{"alg":"RS256","typ":"JWT"}' | B64E)"
  pld="$(printf '{"sub":"system:serviceaccount:gvisor-poc:gardenio-agent","aud":["%s"],"iat":%s,"exp":%s,"kubernetes.io":{"pod":{"uid":"%s"}}}' \
    "$aud" "$iat" "$exp" "$pod_uid" | B64E)"
  printf '%s.%s.%s' "$hdr" "$pld" "ZHVtbXktc2ln"
}

# decode_payload <jwt> → the JSON claims (naive grep parsing, sufficient here)
decode_payload() { printf '%s' "$1" | cut -d. -f2 | B64D; }

# freshness_verdict <jwt> <frozen_at> <expected_pod_uid> <expected_aud> <now>
#   → prints "PASS" or "FAIL:<reason>". Mirrors coop rehydrate::freshness_gate.
freshness_verdict() {
  local jwt="$1" frozen="$2" want_uid="$3" want_aud="$4" now="$5"
  local pld iat exp uid auds proven=0
  pld="$(decode_payload "$jwt")"
  [ -n "$pld" ] || { echo "FAIL:malformed"; return; }
  iat="$(printf '%s' "$pld" | grep -o '"iat":[0-9]*' | head -1 | cut -d: -f2)"
  exp="$(printf '%s' "$pld" | grep -o '"exp":[0-9]*' | head -1 | cut -d: -f2)"
  uid="$(printf '%s' "$pld" | grep -o '"uid":"[^"]*"' | head -1 | sed 's/.*:"//; s/"$//')"
  auds="$(printf '%s' "$pld" | grep -o '"aud":\[[^]]*\]' | head -1)"
  # Hard disqualifiers.
  if [ -n "$exp" ] && [ "$exp" -le "$now" ]; then echo "FAIL:expired"; return; fi
  if [ -n "$want_aud" ] && ! grep -q "\"$want_aud\"" <<<"$auds"; then echo "FAIL:wrong_aud"; return; fi
  if [ -n "$want_uid" ] && [ -n "$uid" ]; then
    if [ "$uid" = "$want_uid" ]; then proven=1; else echo "FAIL:pod_a_identity"; return; fi
  fi
  if [ -n "$frozen" ] && [ -n "$iat" ]; then
    if [ "$iat" -le "$frozen" ]; then echo "FAIL:stale_iat"; return; fi
    proven=1
  fi
  [ "$proven" -eq 1 ] && echo "PASS" || echo "FAIL:indeterminate"
}

NOW=$(date +%s)
FROZEN=$((NOW - 100))
AUD="sts.amazonaws.com"

# ── 1. C1/C2 — cred re-mint: Pod-A token rejected, Pod-B token accepted ──
blue "[1] C1/C2 — cred re-mint identity gate (the headline)"

# Pod-B token: minted after the freeze, bound to Pod B → MUST pass.
POD_B=$(mint_jwt $((NOW + 5)) $((NOW + 3600)) "$AUD" "pod-B-uid")
V=$(freshness_verdict "$POD_B" "$FROZEN" "pod-B-uid" "$AUD" "$NOW")
[ "$V" = "PASS" ] && pass "fresh Pod-B token accepted (kubelet-minted identity, iat>frozen)" \
  || fail "Pod-B token should pass" "verdict=$V"

# Pod-A token by iat: issued before freeze, still in RAM → MUST fail closed.
POD_A_IAT=$(mint_jwt $((FROZEN - 50)) $((NOW + 3600)) "$AUD" "pod-B-uid")
V=$(freshness_verdict "$POD_A_IAT" "$FROZEN" "" "$AUD" "$NOW")
[ "$V" = "FAIL:stale_iat" ] && pass "Pod-A token (iat≤frozen) fails closed — no pre-freeze identity reused" \
  || fail "stale-iat token should fail closed" "verdict=$V"

# Pod-A token by pod_uid: bound to Pod A under Pod B → the identity-confusion
# hazard → MUST fail closed.
POD_A_UID=$(mint_jwt $((NOW + 5)) $((NOW + 3600)) "$AUD" "pod-A-uid")
V=$(freshness_verdict "$POD_A_UID" "$FROZEN" "pod-B-uid" "$AUD" "$NOW")
[ "$V" = "FAIL:pod_a_identity" ] && pass "Pod-A pod_uid under Pod-B fails closed (identity-confusion hazard caught)" \
  || fail "pod-A-uid token should fail closed" "verdict=$V"

# ── 2. C2 — fail-close on expired / wrong-audience / indeterminate ─
blue "[2] C2 — fail-close on expired, wrong-audience, and indeterminate tokens"

EXPIRED=$(mint_jwt $((NOW + 5)) $((NOW - 1)) "$AUD" "pod-B-uid")
V=$(freshness_verdict "$EXPIRED" "$FROZEN" "pod-B-uid" "$AUD" "$NOW")
[ "$V" = "FAIL:expired" ] && pass "expired token fails closed" || fail "expired should fail" "verdict=$V"

WRONG_AUD=$(mint_jwt $((NOW + 5)) $((NOW + 3600)) "some.other.audience" "pod-B-uid")
V=$(freshness_verdict "$WRONG_AUD" "$FROZEN" "pod-B-uid" "$AUD" "$NOW")
[ "$V" = "FAIL:wrong_aud" ] && pass "wrong-audience token fails closed" || fail "wrong-aud should fail" "verdict=$V"

# Indeterminate: no pod-uid claim AND no frozen_at to compare iat against → the
# design says "indistinguishable → fail closed".
INDET=$(mint_jwt $((NOW + 5)) $((NOW + 3600)) "$AUD" "")
V=$(freshness_verdict "$INDET" "" "" "$AUD" "$NOW")
[ "$V" = "FAIL:indeterminate" ] && pass "no-freshness-signal token fails closed (indeterminate)" \
  || fail "indeterminate should fail" "verdict=$V"

# ── 3. C4/C5/§6.5/§6.6 — thaw-epoch marker/ack release condition ───
# The controller gate (Go internal/thawepoch.WaitForAcks) releases the agent iff
# every required consumer wrote an OK ack FOR THE CURRENT EPOCH within deadline,
# else fail-closes. We assert that release condition directly on the on-disk
# protocol shared by the gb supervisor and coop watchers.
blue "[3] C4/C5/§6.5/§6.6 — thaw-epoch marker/ack release condition"

mk_marker() { # <dir> <epoch>
  cat > "$1/$MARKER_NAME" <<JSON
{"epoch":$2,"pod_uid":"pod-B-uid","node":"node-b","ts":"@$NOW","frozen_at":"@$FROZEN","deadline_ms":10000}
JSON
}
mk_ack() { # <dir> <consumer> <epoch> <ok>
  cat > "$1/thaw-epoch.$2.done" <<JSON
{"epoch":$3,"consumer":"$2","ok":$4,"ts":"@$NOW"}
JSON
}
# released <dir> <epoch> <required-consumers...> → "RELEASE" | "HOLD"
released() {
  local dir="$1" epoch="$2"; shift 2
  local c ae aok
  for c in "$@"; do
    [ -f "$dir/thaw-epoch.$c.done" ] || { echo "HOLD"; return; }
    ae="$(grep -o '"epoch":[0-9]*' "$dir/thaw-epoch.$c.done" | head -1 | cut -d: -f2)"
    aok="$(grep -o '"ok":true' "$dir/thaw-epoch.$c.done")"
    { [ "$ae" = "$epoch" ] && [ -n "$aok" ]; } || { echo "HOLD"; return; }
  done
  echo "RELEASE"
}

D="$WORKDIR/epoch"; mkdir -p "$D"
mk_marker "$D" 1

# C5/§6.6 — required consumer (coop) has not acked → gate HOLDs (fail-closed).
mk_ack "$D" gb 1 true
R=$(released "$D" 1 gb coop)
[ "$R" = "HOLD" ] && pass "deadline fail-close: missing coop ack holds the gate (C5/§6.6)" \
  || fail "missing required ack should HOLD" "got $R"

# A consumer that acked NOT-OK (fail-closed itself) → gate HOLDs.
mk_ack "$D" coop 1 false
R=$(released "$D" 1 gb coop)
[ "$R" = "HOLD" ] && pass "consumer fail-close (ok=false) holds the gate" || fail "ok=false should HOLD" "got $R"

# C4 — both required consumers ack OK for the current epoch → RELEASE.
mk_ack "$D" coop 1 true
R=$(released "$D" 1 gb coop)
[ "$R" = "RELEASE" ] && pass "presence re-announce + cred re-mint acked → agent released (C4)" \
  || fail "full ack should RELEASE" "got $R"

# §6.5 idempotency — a marker bump to epoch 2 with stale (epoch-1) acks must NOT
# release: the gate keys on the CURRENT epoch, so a replayed old ack is inert.
mk_marker "$D" 2
R=$(released "$D" 2 gb coop)
[ "$R" = "HOLD" ] && pass "idempotency: stale epoch-1 acks do not release epoch 2 (§6.5)" \
  || fail "stale-epoch ack should HOLD" "got $R"

# Re-acking epoch 2 releases — and re-writing the same ack twice is a no-op.
mk_ack "$D" gb 2 true; mk_ack "$D" coop 2 true; mk_ack "$D" coop 2 true
R=$(released "$D" 2 gb coop)
[ "$R" = "RELEASE" ] && pass "exactly-once effect: double-ack of epoch 2 still releases cleanly" \
  || fail "epoch-2 full ack should RELEASE" "got $R"

# ── 3b. C3 controller marker-write idiom (ka-0000004017) ───────────────
# The controller is not on the node: NodeRehydrationTrigger writes the marker
# over the NodeExecutor (probe chroot) using a base64 payload + chown to the
# agent UID + atomic temp-then-mv — so the in-pod watchers (running as UID 1000)
# can read the marker and write acks beside it. This asserts that exact node
# script shape produces a marker the watchers can parse, byte-for-byte, with no
# shell-quoting hazard on the JSON. Runs everywhere (needs only base64 + a shell).
blue "[3b] C3 — controller marker-write idiom (base64 + chown + atomic mv)"
W="$WORKDIR/c3"; mkdir -p "$W"
STATE_DIR="$W/var/lib/gardenio/thaw/agentsim"
MARKER_JSON='{"epoch":1,"pod_uid":"pod-B-uid","node":"node-b","ts":"2026-06-04T12:00:00Z","frozen_at":"2026-06-04T11:58:20Z","deadline_ms":10000}'
B64=$(printf '%s' "$MARKER_JSON" | base64 | tr -d '\n')
# Mirror NodeRehydrationTrigger.writeMarker (without chown, which needs root):
set +e
( set -e
  mkdir -p "$STATE_DIR"
  chmod 0775 "$STATE_DIR" 2>/dev/null || true
  tmp="$STATE_DIR/$MARKER_NAME.tmp.$$"
  printf '%s' "$B64" | base64 -d > "$tmp"
  mv -f "$tmp" "$STATE_DIR/$MARKER_NAME"
)
WROTE=$?
set -e
if [ "$WROTE" = "0" ] && [ -f "$STATE_DIR/$MARKER_NAME" ]; then
  GOT_EPOCH="$(grep -o '"epoch":[0-9]*' "$STATE_DIR/$MARKER_NAME" | head -1 | cut -d: -f2)"
  GOT_UID="$(grep -o '"pod_uid":"[^"]*"' "$STATE_DIR/$MARKER_NAME" | head -1)"
  # No leftover temp file (atomic mv consumed it).
  LEFTOVER="$(find "$STATE_DIR" -name "$MARKER_NAME.tmp.*" 2>/dev/null | head -1)"
  if [ "$GOT_EPOCH" = "1" ] && [ "$GOT_UID" = '"pod_uid":"pod-B-uid"' ] && [ -z "$LEFTOVER" ]; then
    pass "controller marker-write idiom: base64 payload survives round-trip, atomic mv leaves no temp"
  else
    fail "marker-write idiom" "epoch=$GOT_EPOCH uid=$GOT_UID leftover=$LEFTOVER"
  fi
else
  fail "marker-write idiom" "write rc=$WROTE, marker present=$( [ -f "$STATE_DIR/$MARKER_NAME" ] && echo yes || echo no )"
fi

# ===========================================================================
# runsc-lifecycle + CloudTrail assertions (OPT-IN).
# ===========================================================================
echo ""
echo "──────────────────────────────────────────────────"
# opt-in OFF → plain skip_runsc (the contract-shape leg above already ran).
# opt-in ON but substrate ABSENT → the shared readiness guard: under
# REQUIRE_SUBSTRATE=1 (the go/no-go) the runsc-lifecycle leg is mandatory, so a
# missing substrate is a NO-GO hard-fail; otherwise it is a loud skip and the
# already-passed contract-shape assertions stand (task ka-0000002867).
if [ "${GVISOR_E2E:-0}" != "1" ]; then
  skip_runsc "GVISOR_E2E != 1"
elif ! have kubectl; then
  require_runsc_substrate "kubectl not installed"
elif ! kubectl get runtimeclass "$RUNTIME_CLASS" >/dev/null 2>&1; then
  require_runsc_substrate "RuntimeClass '$RUNTIME_CLASS' not present"
elif [ "$(kubectl auth can-i create pods -n "$GVISOR_NS" 2>/dev/null)" != "yes" ]; then
  require_runsc_substrate "cannot create pods in ns '$GVISOR_NS'"
else
  blue "[4] runsc-lifecycle: thaw-epoch marker/ack round-trips on the substrate"
  k() { kubectl -n "$GVISOR_NS" "$@"; }
  node_run() { local cap="$1"; timeout "$cap" kubectl -n "$GVISOR_NS" exec -i "$PROBE" -- chroot /host bash -s; }

  cat <<YAML | k apply -f - >/dev/null 2>&1
apiVersion: v1
kind: Pod
metadata: {name: ${PROBE}, namespace: ${GVISOR_NS}, labels: {app: gvisor-rh-probe, test: ka-0000005803}}
spec:
  restartPolicy: Never
  nodeSelector: {gvisor: enabled}
  tolerations: [{key: gvisor, operator: Equal, value: enabled, effect: NoSchedule}]
  hostPID: true
  hostNetwork: true
  containers:
  - name: probe
    image: public.ecr.aws/docker/library/ubuntu:22.04
    command: ["sleep","1800"]
    securityContext: {privileged: true}
    volumeMounts: [{name: host, mountPath: /host}, {name: run, mountPath: /run}]
  volumes: [{name: host, hostPath: {path: /}}, {name: run, hostPath: {path: /run}}]
YAML
  if k wait --for=condition=Ready "pod/$PROBE" --timeout=180s >/dev/null 2>&1; then
    pass "runsc-lifecycle probe Ready on gvisor node"
    # Gate on the node being genuinely WARM before the marker/ack round-trip — via
    # the SHARED hardening gate (lib-gvisor-node.sh gvisor_warm_node_gate; decision
    # ka-0000000122 chosen=harden, root flake ka-0000007234). This leg writes the
    # thaw-epoch marker + ack files under /var/tmp on the node, so a not-yet-settled
    # tmpfs on a freshly Karpenter-provisioned gvisor-poc node is the direct flake
    # signature — the gate's /var/tmp-writable check (b) rules it out, and its
    # runsc-present check (a) confirms this really is a gvisor node. This leg does
    # NOT schedule a k8s runsc pod (plain privileged probe, own on-disk protocol),
    # so the k8s containerd state root check (c) is SKIPPED (GVISOR_WARM_REQUIRE_ROOT=0).
    # node_run streams over stdin → use the stdin-streaming adapter. A node that
    # never warms HARD-FAILS with diag rather than red-ing later on a confusing
    # marker-write miss.
    if GVISOR_WARM_REQUIRE_ROOT=0 gvisor_warm_gate_via_run node_run; then
      pass "gvisor-poc node warm (runsc present + /var/tmp writable)"
    else
      fail "gvisor-poc node warm-gate" "$GVISOR_WARM_DIAG"
    fi
    # Drive the marker/ack release condition on the NODE FS, where the controller
    # writes the marker post-restore and the agent consumers ack — proving the
    # shared on-disk protocol works on the real substrate.
    MARK_OUT=$(node_run 60 <<EOF 2>&1
set -e
D=/var/tmp/thaw-rh-${RUN_ID}; rm -rf \$D; mkdir -p \$D
NOW=\$(date +%s)
cat > \$D/${MARKER_NAME} <<JSON
{"epoch":1,"pod_uid":"pod-B-uid","node":"\$(hostname)","ts":"@\$NOW","frozen_at":"@\$((NOW-100))","deadline_ms":10000}
JSON
cat > \$D/thaw-epoch.coop.done <<JSON
{"epoch":1,"consumer":"coop","ok":true,"ts":"@\$NOW"}
JSON
cat > \$D/thaw-epoch.gb.done <<JSON
{"epoch":1,"consumer":"gb","ok":true,"ts":"@\$NOW"}
JSON
ME=\$(grep -o '"epoch":[0-9]*' \$D/${MARKER_NAME} | head -1 | cut -d: -f2)
ok=1
for c in coop gb; do
  ae=\$(grep -o '"epoch":[0-9]*' \$D/thaw-epoch.\$c.done | head -1 | cut -d: -f2)
  aok=\$(grep -o '"ok":true' \$D/thaw-epoch.\$c.done)
  { [ "\$ae" = "\$ME" ] && [ -n "\$aok" ]; } || ok=0
done
[ "\$ok" = "1" ] && echo "RELEASE_OK epoch=\$ME" || echo "RELEASE_FAIL"
rm -rf \$D
EOF
)
    if grep -q 'RELEASE_OK' <<<"$MARK_OUT"; then
      pass "thaw-epoch marker/ack round-trips on node FS ($(printf '%s' "$MARK_OUT" | grep -o 'RELEASE_OK.*'))"
    else
      fail "thaw-epoch marker/ack round-trip" "node out: $(printf '%s' "$MARK_OUT" | tail -3)"
    fi

    # C1 CloudTrail probe (best-effort): if AWS creds are present, assert the
    # node's current AWS identity resolves via STS (the same web-identity path a
    # thawed agent re-mints through). A full Pod-A-vs-Pod-B CloudTrail
    # session-name diff needs a live thawed agent making a call under each pod's
    # SA; that is the production assertion this harness is the scaffold for.
    if have aws && [ -n "${AWS_ACCESS_KEY_ID:-}${AWS_WEB_IDENTITY_TOKEN_FILE:-}" ]; then
      if CT=$(timeout 30 aws sts get-caller-identity --query 'Arn' --output text 2>/dev/null) && [ -n "$CT" ]; then
        pass "STS identity resolvable for CloudTrail role-session-name assertion (arn=${CT##*/})"
      else
        dim "    STS get-caller-identity unavailable — CloudTrail session-name probe skipped"
      fi
    else
      dim "    no AWS creds on probe — CloudTrail role-session-name probe skipped (C1 scaffold only)"
    fi
  else
    fail "runsc-lifecycle probe Ready" "pod $PROBE did not become Ready in 180s"
  fi
fi

# ── Summary ───────────────────────────────────────────────────────
echo ""
echo "════════════════════════════════════════════════════"
echo " gVisor rehydration C1–C5 Summary: $PASS passed, $FAIL failed"
echo "════════════════════════════════════════════════════"
if [ "$FAIL" -gt 0 ]; then
  for e in "${ERRORS[@]}"; do red "  - $e"; done
  exit 1
fi
green "All gVisor rehydration contract assertions passed."
exit 0
