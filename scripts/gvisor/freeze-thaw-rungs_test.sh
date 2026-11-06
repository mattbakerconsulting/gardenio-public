#!/usr/bin/env bash
# freeze-thaw-rungs_test.sh — hermetic unit tests for the freeze→thaw rung
# classifier (Rig R1, bead ka-0000005639).
#
# Fully offline: it sources freeze-thaw-rungs.sh and feeds synthetic FT_*
# sentinel streams (the exact shape the freeze + thaw node scripts emit, plus
# the canary harness's rung-5/6/7 signals), asserting the FT_RUNG_* verdicts,
# the FT_FIRST_FAILED_RUNG classification, and the function's exit status. No
# live daemon, no gVisor node, no kd/gb, no network.
#
# Usage:  bash scripts/gvisor/freeze-thaw-rungs_test.sh

set -uo pipefail

SCRIPT_DIR="$(cd "$(dirname "$0")" && pwd)"
# shellcheck source=scripts/gvisor/freeze-thaw-rungs.sh
source "${SCRIPT_DIR}/freeze-thaw-rungs.sh"

PASS=0
FAIL=0
pass() { echo "PASS: $1"; PASS=$((PASS + 1)); }
fail() { echo "FAIL: $1: $2"; FAIL=$((FAIL + 1)); }

# ----------------------------------------------------------------------------
# Sentinel-stream fixtures. Each helper echoes a freeze/thaw stdout stream.
# ----------------------------------------------------------------------------

# A complete, all-passing freeze stream (rung 1 ok).
freeze_ok() {
  cat <<'EOF'
log: freezing agent
FT_AGENT_CID=ag123
FT_SANDBOX_CID=sb123
FT_POD_UID=uid-a
FT_CHECKPOINT_RC=0
FT_IMAGE_PRESENT=yes
FT_IMAGE_SIZE=3.0G
FT_CONTAINER_COUNT=2
FT_SANDBOX_STATE=running
FT_BUNDLE_OK=yes
FT_IMAGE_OK=yes
FT_SUBC=agent|ag123
EOF
}

# A complete, all-passing thaw stream through rung 4 (routable + cni confirmed).
thaw_ok() {
  cat <<'EOF'
log: restoring into Pod B
FT_B_SANDBOX_CID=bsb
FT_B_POD_UID=uid-b
FT_B_NETNS_PATH=/var/run/netns/cni-b
FT_B_NETNS=net:[4026532540]
FT_CNI_CONFIRMED=yes
FT_CNI_POD_IP=10.0.1.5/24
FT_NETNS_PINNED=/var/run/netns/cni-b
FT_SUB_RC=0
FT_ROOT_RC=0
FT_RESTORED_PID=37737
FT_RESTORED_NETNS=net:[4026532540]
FT_NETNS_L3_REASSERTED=yes
FT_NETNS_SELF_IPAM=yes
FT_NETNS_IFACES=1
FT_NETNS_DEFROUTE=yes
FT_CONTAINERS_RUNNING=yes
EOF
}

# The canary harness's rung-5/6/7 signals, all passing.
harness_ok() {
  cat <<'EOF'
FT_REHYDRATE_GB_ACK=ok
FT_REHYDRATE_COOP_ACK=ok
FT_WORK_TOKEN=intact
FT_RAM_RESUMED=yes
EOF
}

# rung_verdict <marker-block> <rung-name> -> echoes the FT_RUNG_<name> value.
rung_verdict() {
  printf '%s\n' "$1" | sed -n "s/^FT_RUNG_${2}=//p" | tail -1
}
# first_failed <marker-block> -> echoes FT_FIRST_FAILED_RUNG.
first_failed() {
  printf '%s\n' "$1" | sed -n 's/^FT_FIRST_FAILED_RUNG=//p' | tail -1
}
# rung_reason <marker-block> <rung-name> -> echoes FT_RUNG_<name>_REASON.
rung_reason() {
  printf '%s\n' "$1" | sed -n "s/^FT_RUNG_${2}_REASON=//p" | tail -1
}

# run_classify <stream> -> sets OUT (marker block) and RC (exit status).
run_classify() {
  OUT="$(printf '%s\n' "$1" | ft_classify_rungs -)"
  RC=$?
}

# assert_rung <stream> <rung> <expected-verdict> <test-name>
assert_rung() {
  local stream="$1" rung="$2" want="$3" name="$4" got
  run_classify "$stream"
  got="$(rung_verdict "$OUT" "$rung")"
  if [ "$got" = "$want" ]; then
    pass "$name"
  else
    fail "$name" "FT_RUNG_${rung} expected '$want', got '$got'. Block:\n$OUT"
  fi
}

# assert_first <stream> <expected-first-failed> <expected-rc> <test-name>
assert_first() {
  local stream="$1" wantrung="$2" wantrc="$3" name="$4"
  run_classify "$stream"
  local gotrung; gotrung="$(first_failed "$OUT")"
  if [ "$gotrung" = "$wantrung" ] && [ "$RC" = "$wantrc" ]; then
    pass "$name"
  else
    fail "$name" "first_failed expected '$wantrung' rc=$wantrc, got '$gotrung' rc=$RC. Block:\n$OUT"
  fi
}

echo "── freeze-thaw-rungs classifier tests ──────────────────────────────"

# ── 1. Full green cycle: every measured rung ok, none failed ────────────
FULL_OK="$(freeze_ok; thaw_ok; harness_ok)"
assert_first "$FULL_OK" none 0 "full green cycle classifies as no-failure (rc 0)"
assert_rung "$FULL_OK" snapshot_persisted ok "rung1 ok on green cycle"
assert_rung "$FULL_OK" restore_rc0        ok "rung2 ok on green cycle"
assert_rung "$FULL_OK" netns_routable     ok "rung3 ok on green cycle"
assert_rung "$FULL_OK" cni_confirmed      ok "rung4 ok on green cycle (cni=yes)"
assert_rung "$FULL_OK" rehydrate_acks     ok "rung5 ok on green cycle"
assert_rung "$FULL_OK" work_intact        ok "rung6 ok on green cycle"
assert_rung "$FULL_OK" ram_intact         ok "rung7 ok on green cycle"

# ── 2. Rung 1 failures (freeze side) ────────────────────────────────────
F_BADCRC="$(freeze_ok | sed 's/FT_CHECKPOINT_RC=0/FT_CHECKPOINT_RC=137/'; thaw_ok; harness_ok)"
assert_first "$F_BADCRC" snapshot_persisted 1 "checkpoint rc!=0 => rung1 fail + classified mode"
assert_rung  "$F_BADCRC" restore_rc0 skip "downstream rungs skipped after rung1 fail"

F_BADBUNDLE="$(freeze_ok | sed 's/FT_BUNDLE_OK=yes/FT_BUNDLE_OK=no/'; thaw_ok; harness_ok)"
assert_first "$F_BADBUNDLE" snapshot_persisted 1 "incomplete bundle => rung1 fail"

F_NOCOUNT="$(freeze_ok | sed '/FT_CONTAINER_COUNT=/d'; thaw_ok; harness_ok)"
assert_first "$F_NOCOUNT" snapshot_persisted 1 "absent FT_CONTAINER_COUNT => rung1 fail (not baked into statefile)"

F_NOFREEZE="$(thaw_ok; harness_ok)"
assert_first "$F_NOFREEZE" snapshot_persisted 1 "no freeze sentinels at all => rung1 fail (absent FT_CHECKPOINT_RC)"

# Each remaining rung-1 conjunct has its own fail-path (a fail-closed forensic
# classifier must lock in every conjunct, not just a representative one).
F_NOIMG="$(freeze_ok | sed 's/FT_IMAGE_PRESENT=yes/FT_IMAGE_PRESENT=no/'; thaw_ok; harness_ok)"
assert_first "$F_NOIMG" snapshot_persisted 1 "FT_IMAGE_PRESENT!=yes => rung1 fail"

# ── FT_IMAGE_PRESENT truncation-vs-absence disambiguation (bug ka-0000004931) ──
# The freeze-side snapshot_persisted failure that filed this bug was NOT a genuine
# image absence — it was a WEDGED host-ns probe that hung the freeze until it was
# killed, TRUNCATING the sentinel stream so FT_IMAGE_PRESENT was ABSENT (or
# present-but-empty), never a real "=no". All three cases still FAIL rung 1
# (fail-closed is correct), but the REASON must distinguish them so an auto-filed
# signature (Rig R3) does not label a hang+truncate as "image absent". These lock
# the three-way split ftr_seen enables.
#
# (a) FT_CHECKPOINT_RC present + FT_IMAGE_PRESENT ABSENT => truncation, not =no.
F_TRUNC_ABSENT="$(freeze_ok | sed '/FT_IMAGE_PRESENT=/d'; thaw_ok; harness_ok)"
assert_first "$F_TRUNC_ABSENT" snapshot_persisted 1 "checkpoint ran but FT_IMAGE_PRESENT absent => rung1 fail (truncation)"
run_classify "$F_TRUNC_ABSENT"
r="$(rung_reason "$OUT" snapshot_persisted)"
if grep -qi "TRUNCATED" <<<"$r" && grep -qi "NOT an observed image absence" <<<"$r"; then
  pass "rung1 absent-FT_IMAGE_PRESENT reason names TRUNCATION, not image absence (bug ka-0000004931)"
else
  fail "rung1 truncation-absent reason" "expected a TRUNCATED / not-an-observed-absence reason, got: $r"
fi
# It must NOT be mislabeled with the genuine-absence wording.
if grep -qi "positively observed no image" <<<"$r"; then
  fail "rung1 truncation-absent must not claim genuine absence" "got: $r"
else
  pass "rung1 absent-FT_IMAGE_PRESENT reason does NOT claim a positively-observed absence"
fi

# (b) FT_IMAGE_PRESENT PRESENT-BUT-EMPTY (partial line) => truncation, not =no.
F_TRUNC_EMPTY="$(freeze_ok | sed 's/FT_IMAGE_PRESENT=yes/FT_IMAGE_PRESENT=/'; thaw_ok; harness_ok)"
assert_first "$F_TRUNC_EMPTY" snapshot_persisted 1 "present-but-empty FT_IMAGE_PRESENT => rung1 fail (truncation)"
run_classify "$F_TRUNC_EMPTY"
r="$(rung_reason "$OUT" snapshot_persisted)"
if grep -qi "TRUNCATED" <<<"$r" && ! grep -qi "positively observed no image" <<<"$r"; then
  pass "rung1 empty-FT_IMAGE_PRESENT reason names TRUNCATION, not image absence"
else
  fail "rung1 truncation-empty reason" "expected TRUNCATED (not genuine absence), got: $r"
fi

# (c) FT_IMAGE_PRESENT=no GENUINE absence => the probe positively observed no
# image. This is the ONLY case that should carry the observed-absence wording; it
# must NOT be labeled truncation (that would be the pre-fix conflation).
F_GENUINE_NO="$(freeze_ok | sed 's/FT_IMAGE_PRESENT=yes/FT_IMAGE_PRESENT=no/'; thaw_ok; harness_ok)"
run_classify "$F_GENUINE_NO"
r="$(rung_reason "$OUT" snapshot_persisted)"
if grep -qi "positively observed no image" <<<"$r" && ! grep -qi "TRUNCATED" <<<"$r"; then
  pass "rung1 genuine FT_IMAGE_PRESENT=no reason names an OBSERVED absence, not truncation (bug ka-0000004931)"
else
  fail "rung1 genuine-absence reason" "expected an observed-absence reason (not TRUNCATED), got: $r"
fi

# FT_SANDBOX_STATE!=running is the high-signal "--leave-running killed the live
# pod" mode — assert both the classification AND its distinctive reason.
F_SBDEAD="$(freeze_ok | sed 's/FT_SANDBOX_STATE=running/FT_SANDBOX_STATE=stopped/'; thaw_ok; harness_ok)"
assert_first "$F_SBDEAD" snapshot_persisted 1 "FT_SANDBOX_STATE!=running => rung1 fail (checkpoint killed live pod)"
run_classify "$F_SBDEAD"
if grep -q "killed the live pod" <<<"$(rung_reason "$OUT" snapshot_persisted)"; then
  pass "rung1 sandbox-dead reason is the distinctive 'killed the live pod' mode"
else
  fail "rung1 sandbox-dead reason" "got: $(rung_reason "$OUT" snapshot_persisted)"
fi

F_NOIMGOK="$(freeze_ok | sed 's/FT_IMAGE_OK=yes/FT_IMAGE_OK=no/'; thaw_ok; harness_ok)"
assert_first "$F_NOIMGOK" snapshot_persisted 1 "freeze-side FT_IMAGE_OK!=yes => rung1 fail (image not relocated)"

# Numeric guard: a present-but-NON-numeric FT_CONTAINER_COUNT (garbage node
# output) is coerced to 0 and fails — distinct from the absent case (F_NOCOUNT).
F_BADCOUNT="$(freeze_ok | sed 's/FT_CONTAINER_COUNT=2/FT_CONTAINER_COUNT=abc/'; thaw_ok; harness_ok)"
assert_first "$F_BADCOUNT" snapshot_persisted 1 "non-numeric FT_CONTAINER_COUNT => rung1 fail (numeric guard)"

# SECONDARY HAZARD: image reaped between freeze and thaw. Freeze emits
# FT_IMAGE_OK=yes; the thaw pre-restore re-verify emits FT_IMAGE_OK=no and the
# barrier fail-closes FT_ROOT_RC=128 with NO FT_RESTORED_PID. Last-wins makes the
# thaw `no` win, so rung 1 must peel this off as a snapshot durability fault
# BEFORE rung 2 ever interprets the overloaded FT_ROOT_RC=128.
F_REAPED="$(freeze_ok; cat <<'EOF'
FT_IMAGE_OK=no
FT_SUB_RC=0
FT_ROOT_RC=128
EOF
harness_ok)"
assert_first "$F_REAPED" snapshot_persisted 1 "image reaped (freeze IMAGE_OK=yes, thaw IMAGE_OK=no + ROOT_RC=128) classifies at rung1, not rung2"

# ── 3. Rung 2 failures (barrier restore) ────────────────────────────────
# True rung-2 failure: barrier exits before [6], NO FT_RESTORED_PID, FT_ROOT_RC=128.
T_NOPID="$(freeze_ok; cat <<'EOF'
FT_B_NETNS=net:[4026532540]
FT_SUB_RC=0
FT_ROOT_RC=128
EOF
harness_ok)"
assert_first "$T_NOPID" restore_rc0 1 "barrier fail (no FT_RESTORED_PID, ROOT_RC=128) => rung2 fail"
assert_rung  "$T_NOPID" netns_routable skip "rung3 skipped after rung2 fail"

# INVERSE ORDERING HAZARD: a true barrier failure whose FIRST FT_ROOT_RC emit is 0
# but which dies before printing FT_RESTORED_PID. A regression to a naive
# 'FT_ROOT_RC==0 => rung2 ok' predicate would WRONGLY pass this — it is exactly the
# misclassification rung 2's PID-keying exists to prevent. Locks PID-keying in.
T_NOPID_RC0="$(freeze_ok; cat <<'EOF'
FT_B_NETNS=net:[4026532540]
FT_SUB_RC=0
FT_ROOT_RC=0
EOF
harness_ok)"
assert_first "$T_NOPID_RC0" restore_rc0 1 "barrier fail with stale FT_ROOT_RC=0 + no PID => rung2 fail (PID-keying, not rc-keying)"

# Sub-container restore failure: PID present but FT_SUB_RC!=0.
T_SUBRC="$(freeze_ok; thaw_ok | sed 's/FT_SUB_RC=0/FT_SUB_RC=1/'; harness_ok)"
assert_first "$T_SUBRC" restore_rc0 1 "sub-container restore rc!=0 => rung2 fail even with PID"

# Netns inode mismatch: PID present, rcs 0, but restored netns != Pod B netns.
T_INODE="$(freeze_ok; thaw_ok | sed 's/FT_RESTORED_NETNS=net:\[4026532540\]/FT_RESTORED_NETNS=net:[4026539999]/'; harness_ok)"
assert_first "$T_INODE" restore_rc0 1 "restored netns inode != Pod B netns => rung2 fail"

# Netns inode EMPTY arm (distinct from MISMATCH): restored netns absent entirely.
# Exercises the `[ -z "$rns" ]` guard, reported with the <empty> marker.
T_INODE_EMPTY="$(freeze_ok; thaw_ok | sed '/FT_RESTORED_NETNS=/d'; harness_ok)"
assert_first "$T_INODE_EMPTY" restore_rc0 1 "restored netns absent (empty arm) => rung2 fail"
run_classify "$T_INODE_EMPTY"
if grep -q "<empty>" <<<"$(rung_reason "$OUT" restore_rc0)"; then
  pass "rung2 empty-inode reason carries the <empty> marker"
else
  fail "rung2 empty-inode reason" "got: $(rung_reason "$OUT" restore_rc0)"
fi

# Present-but-EMPTY FT_RESTORED_PID (not absent) must take the 'no restored sentry
# pid' branch (FT_SUB_RC/FT_ROOT_RC seen), NOT the 'no thaw sentinels' branch —
# proving ftr_seen's absent-vs-empty three-way split.
T_PID_EMPTY="$(freeze_ok; cat <<'EOF'
FT_B_NETNS=net:[4026532540]
FT_SUB_RC=0
FT_ROOT_RC=128
FT_RESTORED_PID=
EOF
harness_ok)"
assert_first "$T_PID_EMPTY" restore_rc0 1 "present-but-empty FT_RESTORED_PID => rung2 fail (not the all-absent branch)"
run_classify "$T_PID_EMPTY"
if grep -q "no restored sentry pid" <<<"$(rung_reason "$OUT" restore_rc0)"; then
  pass "rung2 empty-PID takes the 'no restored sentry pid' branch (ftr_seen split)"
else
  fail "rung2 empty-PID branch" "got: $(rung_reason "$OUT" restore_rc0)"
fi

# ── 4. THE ORDERING HAZARD: lo-only netns must classify as rung3, NOT rung2 ──
# The node script emits FT_ROOT_RC=0, then FT_RESTORED_PID, then re-emits
# FT_ROOT_RC=128 on the lo-only fail-close. Last-wins parse sees ROOT_RC=128, but
# rung2 keys on FT_RESTORED_PID (present) so the restore is correctly ok and the
# lo-only fault localizes to rung3.
T_LOONLY="$(freeze_ok; cat <<'EOF'
FT_B_NETNS=net:[4026532540]
FT_CNI_CONFIRMED=yes
FT_CNI_POD_IP=10.0.1.5/24
FT_SUB_RC=0
FT_ROOT_RC=0
FT_RESTORED_PID=37737
FT_RESTORED_NETNS=net:[4026532540]
FT_NETNS_L3_REASSERTED=no
FT_NETNS_SELF_IPAM=no
FT_NETNS_IFACES=0
FT_NETNS_DEFROUTE=no
FT_ROOT_RC=128
EOF
harness_ok)"
assert_rung  "$T_LOONLY" restore_rc0    ok   "lo-only: rung2 ok despite last-wins FT_ROOT_RC=128 (keys on PID)"
assert_first "$T_LOONLY" netns_routable 1    "lo-only netns classifies as rung3 (netns_routable), not rung2"
# The rung-3 reason must carry the netns-graft localizers for a forensic bundle.
run_classify "$T_LOONLY"
if grep -q "cni-confirmed=yes" <<<"$(rung_reason "$OUT" netns_routable)"; then
  pass "rung3 lo-only reason embeds the cni/L3/self-ipam localizers"
else
  fail "rung3 lo-only reason localizers" "reason missing cni-confirmed diag: $(rung_reason "$OUT" netns_routable)"
fi

# A present-but-EMPTY FT_NETNS_IFACES (seen, but value '') is NOT the absent/skip
# case — the numeric guard coerces it to 0 and the rung FAILS. Distinguishes
# absent (=> skip, the LEGACY case below) from present-empty (=> fail).
T_IFACE_EMPTY="$(freeze_ok; thaw_ok | sed 's/FT_NETNS_IFACES=1/FT_NETNS_IFACES=/'; harness_ok)"
assert_first "$T_IFACE_EMPTY" netns_routable 1 "present-but-empty FT_NETNS_IFACES => rung3 fail (not skip)"
# Present-but-NON-numeric FT_NETNS_IFACES (garbage) also coerces to 0 and fails.
T_IFACE_GARBAGE="$(freeze_ok; thaw_ok | sed 's/FT_NETNS_IFACES=1/FT_NETNS_IFACES=many/'; harness_ok)"
assert_first "$T_IFACE_GARBAGE" netns_routable 1 "non-numeric FT_NETNS_IFACES => rung3 fail (numeric guard)"

# ── 5. Rung 4 is NON-GATING: a routable thaw with cni=no must NOT fail ───
T_CNINO="$(freeze_ok; thaw_ok | sed 's/FT_CNI_CONFIRMED=yes/FT_CNI_CONFIRMED=no/'; harness_ok)"
assert_first "$T_CNINO" none 0 "routable thaw with FT_CNI_CONFIRMED=no is NOT a failure (rung4 non-gating)"
assert_rung  "$T_CNINO" cni_confirmed skip "rung4 cni=no on a routable thaw => skip, never fail"
assert_rung  "$T_CNINO" rehydrate_acks ok "rungs after a non-gating rung4 still evaluate"

# ── 6. Rung 5 ack failures + the per-consumer fail-closed contract ──────
T_GBNACK="$(freeze_ok; thaw_ok; harness_ok | sed 's/FT_REHYDRATE_GB_ACK=ok/FT_REHYDRATE_GB_ACK=fail/')"
assert_first "$T_GBNACK" rehydrate_acks 1 "gb ack fail => rung5 fail"

# Asymmetric: gb=ok, coop=fail. Reason must name coop but NOT gb (locks the
# asymmetric reason builder, which T_BOTHNACK covers only transitively).
T_COOPNACK="$(freeze_ok; thaw_ok; harness_ok | sed 's/FT_REHYDRATE_COOP_ACK=ok/FT_REHYDRATE_COOP_ACK=fail/')"
assert_first "$T_COOPNACK" rehydrate_acks 1 "coop ack fail (gb ok) => rung5 fail"
run_classify "$T_COOPNACK"
if grep -q "coop" <<<"$(rung_reason "$OUT" rehydrate_acks)" \
   && ! grep -q "gb(" <<<"$(rung_reason "$OUT" rehydrate_acks)"; then
  pass "rung5 asymmetric reason names only the failing consumer (coop), not gb"
else
  fail "rung5 asymmetric reason" "got: $(rung_reason "$OUT" rehydrate_acks)"
fi

# PARTIAL MEASUREMENT (the contract case): exactly ONE ack present, the other
# entirely absent => FAIL (fail-closed per consumer), NOT skip. Only when BOTH are
# absent does it skip (the LEGACY case below). Locks the "emit both or neither"
# harness contract.
T_GBONLY="$(freeze_ok; thaw_ok; cat <<'EOF'
FT_REHYDRATE_GB_ACK=ok
FT_WORK_TOKEN=intact
FT_RAM_RESUMED=yes
EOF
)"
assert_first "$T_GBONLY" rehydrate_acks 1 "only gb ack present (coop absent) => rung5 fail (fail-closed per consumer)"
run_classify "$T_GBONLY"
if grep -q "coop(<absent>)" <<<"$(rung_reason "$OUT" rehydrate_acks)"; then
  pass "rung5 partial-measurement reason flags the absent consumer as coop(<absent>)"
else
  fail "rung5 partial-measurement reason" "got: $(rung_reason "$OUT" rehydrate_acks)"
fi

T_BOTHNACK="$(freeze_ok; thaw_ok; cat <<'EOF'
FT_REHYDRATE_GB_ACK=fail
FT_REHYDRATE_COOP_ACK=fail
FT_WORK_TOKEN=intact
FT_RAM_RESUMED=yes
EOF
)"
run_classify "$T_BOTHNACK"
if [ "$(first_failed "$OUT")" = "rehydrate_acks" ] && grep -q "coop" <<<"$(rung_reason "$OUT" rehydrate_acks)"; then
  pass "both acks fail => rung5 fail, reason names both consumers"
else
  fail "both acks fail" "got first=$(first_failed "$OUT") reason=$(rung_reason "$OUT" rehydrate_acks)"
fi

# Uppercase / wrong-spelling ack value is treated as failure (case-sensitive
# exact-token contract).
T_GBUPPER="$(freeze_ok; thaw_ok; harness_ok | sed 's/FT_REHYDRATE_GB_ACK=ok/FT_REHYDRATE_GB_ACK=OK/')"
assert_first "$T_GBUPPER" rehydrate_acks 1 "case-sensitive: FT_REHYDRATE_GB_ACK=OK (uppercase) => rung5 fail"

# ── 7. Rung 6 / 7: state-fidelity failures ──────────────────────────────
T_WORKLOST="$(freeze_ok; thaw_ok; harness_ok | sed 's/FT_WORK_TOKEN=intact/FT_WORK_TOKEN=lost/')"
assert_first "$T_WORKLOST" work_intact 1 "/work token lost => rung6 fail"
assert_rung  "$T_WORKLOST" ram_intact skip "rung7 skipped after rung6 fail"

T_RAMNO="$(freeze_ok; thaw_ok; harness_ok | sed 's/FT_RAM_RESUMED=yes/FT_RAM_RESUMED=no/')"
assert_first "$T_RAMNO" ram_intact 1 "in-RAM counter did not resume => rung7 fail (deepest rung)"

# ── 8. Legacy / absent sentinels: rungs 3-7 SKIP, not fail ──────────────
# A legacy node script that predates Fix A/Fix C and a harness that measured no
# acks/tokens: rungs 1-2 pass, 3-7 skip, overall no-failure.
LEGACY="$(freeze_ok; cat <<'EOF'
FT_B_NETNS=net:[4026532540]
FT_SUB_RC=0
FT_ROOT_RC=0
FT_RESTORED_PID=37737
FT_RESTORED_NETNS=net:[4026532540]
FT_CONTAINERS_RUNNING=yes
EOF
)"
assert_first "$LEGACY" none 0 "legacy/older script (no Fix-A/C sentinels, no harness tokens) => no failure"
assert_rung  "$LEGACY" netns_routable skip "absent FT_NETNS_IFACES => rung3 skip (not fail)"
assert_rung  "$LEGACY" cni_confirmed  skip "absent FT_CNI_CONFIRMED => rung4 skip"
assert_rung  "$LEGACY" rehydrate_acks skip "absent ack sentinels => rung5 skip"
assert_rung  "$LEGACY" work_intact    skip "absent FT_WORK_TOKEN => rung6 skip"
assert_rung  "$LEGACY" ram_intact     skip "absent FT_RAM_RESUMED => rung7 skip"

# ── 9. Marker block shape: every rung emits both markers + the two summary keys ─
run_classify "$FULL_OK"
shape_ok=1
for r in snapshot_persisted restore_rc0 netns_routable cni_confirmed rehydrate_acks work_intact ram_intact; do
  grep -q "^FT_RUNG_${r}=" <<<"$OUT" || { shape_ok=0; echo "   missing FT_RUNG_${r}="; }
  grep -q "^FT_RUNG_${r}_REASON=" <<<"$OUT" || { shape_ok=0; echo "   missing FT_RUNG_${r}_REASON="; }
done
grep -q "^FT_FIRST_FAILED_RUNG=" <<<"$OUT" || { shape_ok=0; echo "   missing FT_FIRST_FAILED_RUNG="; }
grep -q "^FT_FIRST_FAILED_REASON=" <<<"$OUT" || { shape_ok=0; echo "   missing FT_FIRST_FAILED_REASON="; }
if [ "$shape_ok" = 1 ]; then
  pass "marker block emits all 7 rung pairs + both summary keys"
else
  fail "marker block shape" "missing keys (see above)"
fi

# ── 10. Non-FT_ noise (log lines, blanks) is ignored, not parsed as sentinels ──
NOISY="$(printf 'random log line\n\nFT_NOT_A_REAL_KEY but no equals\n'; freeze_ok; echo "another log"; thaw_ok; harness_ok)"
assert_first "$NOISY" none 0 "interleaved log/blank/noise lines are ignored"

# ── 11. Reading from a FILE path (not just stdin) ───────────────────────
TMP_T="$(mktemp)"
trap 'rm -f "$TMP_T"' EXIT
printf '%s\n' "$FULL_OK" > "$TMP_T"
OUT="$(ft_classify_rungs "$TMP_T")"; RC=$?
if [ "$RC" = 0 ] && [ "$(first_failed "$OUT")" = none ]; then
  pass "ft_classify_rungs reads a file-path argument"
else
  fail "file-path read" "rc=$RC first=$(first_failed "$OUT")"
fi

# ── summary ─────────────────────────────────────────────────────────────
echo "────────────────────────────────────────────────────────────────────"
echo "freeze-thaw-rungs tests: ${PASS} passed, ${FAIL} failed"
[ "$FAIL" -eq 0 ]
