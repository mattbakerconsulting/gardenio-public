#!/usr/bin/env bash
# tarboat-provision-box_test.sh — Unit tests for the E4 ephemeral-box provisioner
# (Test-in-box E4, ka-0000002700, epic ka-0000003645).
#
# The provisioner sequences `aws ec2` calls (allocate-address, run-instances,
# associate-address, terminate-instances, release-address, describe-*) around a
# render-user-data.sh invocation, tags the box for the reaper, and emits a
# tarboat-box/v1 descriptor. We drive it end-to-end against a STUB `aws` on PATH
# (no real cloud, no network): the stub records every invocation to a log and
# answers the read-shaped calls (allocate-address / describe-*) from canned
# values, so we can lock the call sequence, the descriptor shape, the EIP
# ownership accounting, the lifecycle=keep teardown guard, and the reaper's
# expiry/keep decision — without ever touching AWS.
#
# Locks:
#   - provision --dry-run emits a valid tarboat-box/v1 descriptor with the
#     derived sslip.io apex + hosts and the calver/namespace passed through;
#   - provision tags the box ephemeral + expiry + calver and IMDSv2-locks it;
#   - tearidown reads instance/allocation/region from a descriptor and only
#     releases an EIP it owned (eip_allocated_by_us);
#   - teardown refuses a lifecycle=keep box without --force;
#   - reap terminates an expired ephemeral box but spares a keep-tagged one and
#     a not-yet-due one;
#   - required-flag / unknown-subcommand precondition errors exit 2.
#
# Usage:  bash scripts/inbox/tarboat-provision-box_test.sh

set -uo pipefail

SCRIPT_DIR="$(cd "$(dirname "$0")" && pwd)"
SUT="${SCRIPT_DIR}/tarboat-provision-box.sh"

PASS=0
FAIL=0
TMP="$(mktemp -d)"
trap 'rm -rf "$TMP"' EXIT
pass() { echo "PASS: $1"; PASS=$((PASS + 1)); }
fail() { echo "FAIL: $1: $2"; FAIL=$((FAIL + 1)); }

# ── stub `aws` ───────────────────────────────────────────────────────────────
# Records the full argv (TAB-joined) of every call to $AWS_LOG, and answers the
# read-shaped subcommands with deterministic canned text so the SUT can proceed
# without a real cloud. Anything not explicitly answered prints nothing + exit 0.
BIN="$TMP/bin"
mkdir -p "$BIN"
cat > "$BIN/aws" <<'STUB'
#!/usr/bin/env bash
# Log the call (skip a leading --region R so query matching is positional-stable).
printf '%s\n' "$*" >> "$AWS_LOG"
args=("$@")
# Strip a leading "--region X" for dispatch matching.
if [ "${args[0]:-}" = "--region" ]; then args=("${args[@]:2}"); fi
svc="${args[0]:-}"; sub="${args[1]:-}"
case "$svc $sub" in
  "ec2 allocate-address")
    # --query '[AllocationId,PublicIp]' --output text
    printf 'eipalloc-0123456789\t198.51.100.23\n' ;;
  "ec2 describe-addresses")
    # may be queried for PublicIp or AllocationId; answer both shapes.
    if grep -q 'AllocationId' <<<"$*"; then echo "eipalloc-0123456789"; else echo "198.51.100.23"; fi ;;
  "ec2 run-instances")
    echo "i-0123456789abcdef0" ;;
  "ec2 wait")
    exit 0 ;;
  "ec2 associate-address")
    echo "eipassoc-0123" ;;
  "ec2 describe-instances")
    # lifecycle-tag probe (teardown) → answer per $STUB_LIFECYCLE; reap rows → $STUB_REAP_ROWS.
    if grep -q "Tags\[?Key=='lifecycle'\].Value | \[0\]" <<<"$*"; then
      echo "${STUB_LIFECYCLE:-ephemeral}"
    elif grep -q "InstanceId,LaunchTime" <<<"$*"; then
      printf '%b' "${STUB_REAP_ROWS:-}"
    elif grep -q 'PublicIpAddress' <<<"$*"; then
      echo "198.51.100.23"
    else
      echo "None"
    fi ;;
  "ec2 terminate-instances"|"ec2 release-address")
    : ;;
  *) : ;;
esac
exit 0
STUB
chmod +x "$BIN/aws"

# ── stub render-user-data.sh so provision (non-dry-run) doesn't need the real one
RUD_DIR="$TMP/scripts"
mkdir -p "$RUD_DIR"
# The SUT computes RENDER_USER_DATA relative to its own location, so for the
# non-dry-run path we rely on --user-data to bypass rendering instead.
echo "#cloud-config" > "$TMP/user-data.yaml"

run_sut() { PATH="$BIN:$PATH" AWS_LOG="$TMP/aws.log" "$@"; }

# ============================================================================
# 1. provision --dry-run emits a valid descriptor (drives the real
#    render-user-data.sh, but against the stub `aws` — no real cloud/network).
#
# Root-cause note (ka-0000004694): the rc=1 / empty-stdout failure seen in CI
# was NOT a "transient parallel-load flake" (the earlier gd-0vP21gMx3Z framing
# was a mis-diagnosis). The real cause was that the RWX-era test-scripts task's
# file allow-list staged tarboat-provision-box.sh but NOT
# scripts/render-user-data.sh (nor its cloud-config template), so provision
# --dry-run died "render-user-data.sh not found". Defensively, the SUT also now requires only
# readability and invokes the renderer via `bash` (mode-bit independent). We
# capture via a temp file (clearer FAIL diagnostics than a $(...) subshell) and
# keep stderr so a real failure surfaces its message.
# ============================================================================
: > "$TMP/aws.log"
PATH="$BIN:$PATH" AWS_LOG="$TMP/aws.log" bash "$SUT" provision \
      --calver 2026.162.1 --namespace gardenio --region us-east-2 \
      --dry-run >"$TMP/dryrun.out" 2>"$TMP/dryrun.err"; rc=$?
out="$(cat "$TMP/dryrun.out")"
err="$(cat "$TMP/dryrun.err")"
if [ "$rc" = 0 ] \
   && grep -q '"schema": "tarboat-box/v1"' <<<"$out" \
   && grep -q '"calver": "2026.162.1"' <<<"$out" \
   && grep -q '"namespace": "gardenio"' <<<"$out" \
   && grep -q '"apex": "203-0-113-7.sslip.io"' <<<"$out" \
   && grep -q 'beads-viewer.203-0-113-7.sslip.io' <<<"$out"; then
  pass "provision --dry-run emits tarboat-box/v1 with derived apex+hosts+calver"
else
  fail "provision dry-run descriptor" "rc=$rc out=<<<$out>>> err=<<<$err>>>"
fi

# ============================================================================
# 2. provision requires --calver.
# ============================================================================
PATH="$BIN:$PATH" AWS_LOG="$TMP/aws.log" bash "$SUT" provision --dry-run >/dev/null 2>&1; rc=$?
if [ "$rc" = 2 ]; then pass "provision without --calver exits 2"
else fail "provision missing --calver" "rc=$rc (want 2)"; fi

# ============================================================================
# 3. provision (real path, stub aws + --user-data) tags the box + IMDSv2-locks.
#    --wait-timeout 0 so it doesn't try to curl an edge.
# ============================================================================
: > "$TMP/aws.log"
out="$(PATH="$BIN:$PATH" AWS_LOG="$TMP/aws.log" bash "$SUT" provision \
        --calver 2026.162.2 --region us-east-2 \
        --ami ami-0test --subnet-id subnet-0test --security-group-ids sg-0test \
        --key-name kp-test --user-data "$TMP/user-data.yaml" \
        --ttl-minutes 90 --wait-timeout 0 2>/dev/null)"; rc=$?
log="$(cat "$TMP/aws.log")"
if [ "$rc" = 0 ] \
   && grep -q 'run-instances' <<<"$log" \
   && grep -q 'HttpTokens=required,HttpPutResponseHopLimit=1' <<<"$log" \
   && grep -q 'gardenio.io/tarboat-ephemeral' <<<"$log" \
   && grep -q 'gardenio.io/calver' <<<"$log" \
   && grep -q 'gardenio.io/expiry' <<<"$log" \
   && grep -q '"instance_id": "i-0123456789abcdef0"' <<<"$out" \
   && grep -q '"eip_allocated_by_us": true' <<<"$out" \
   && grep -q '"ip": "198.51.100.23"' <<<"$out"; then
  pass "provision launches+tags(ephemeral/calver/expiry)+IMDSv2-locks, emits real descriptor"
else
  fail "provision real path" "rc=$rc log=<<<$log>>> out=<<<$out>>>"
fi

# capture that descriptor for teardown tests
echo "$out" > "$TMP/desc.json"

# ============================================================================
# 4. provision associates the EIP it allocated (allocate → run → associate).
# ============================================================================
if grep -q 'allocate-address' "$TMP/aws.log" \
   && grep -q 'associate-address' "$TMP/aws.log"; then
  # ordering: allocate must precede associate
  a="$(grep -n 'allocate-address' "$TMP/aws.log" | head -n1 | cut -d: -f1)"
  s="$(grep -n 'associate-address' "$TMP/aws.log" | head -n1 | cut -d: -f1)"
  if [ "$a" -lt "$s" ]; then pass "provision allocates EIP before associating it"
  else fail "provision EIP order" "allocate(line $a) not before associate(line $s)"; fi
else
  fail "provision EIP calls" "missing allocate/associate in log"
fi

# ============================================================================
# 5. teardown reads the descriptor and releases the EIP we owned.
# ============================================================================
: > "$TMP/aws.log"
PATH="$BIN:$PATH" AWS_LOG="$TMP/aws.log" STUB_LIFECYCLE=ephemeral \
  bash "$SUT" teardown "$TMP/desc.json" >/dev/null 2>&1; rc=$?
log="$(cat "$TMP/aws.log")"
if [ "$rc" = 0 ] \
   && grep -q 'terminate-instances.*i-0123456789abcdef0' <<<"$log" \
   && grep -q 'release-address.*eipalloc-0123456789' <<<"$log"; then
  pass "teardown from descriptor terminates instance + releases the EIP it owned"
else
  fail "teardown descriptor" "rc=$rc log=<<<$log>>>"
fi

# ============================================================================
# 6. teardown does NOT release an EIP it didn't own (eip_allocated_by_us=false).
# ============================================================================
sed 's/"eip_allocated_by_us": true/"eip_allocated_by_us": false/' "$TMP/desc.json" > "$TMP/desc-notours.json"
: > "$TMP/aws.log"
PATH="$BIN:$PATH" AWS_LOG="$TMP/aws.log" STUB_LIFECYCLE=ephemeral \
  bash "$SUT" teardown "$TMP/desc-notours.json" >/dev/null 2>&1; rc=$?
log="$(cat "$TMP/aws.log")"
if [ "$rc" = 0 ] \
   && grep -q 'terminate-instances' <<<"$log" \
   && ! grep -q 'release-address' <<<"$log"; then
  pass "teardown spares an EIP it did not allocate"
else
  fail "teardown not-ours EIP" "rc=$rc log=<<<$log>>>"
fi

# ============================================================================
# 7. teardown refuses a lifecycle=keep box without --force.
# ============================================================================
: > "$TMP/aws.log"
PATH="$BIN:$PATH" AWS_LOG="$TMP/aws.log" STUB_LIFECYCLE=keep \
  bash "$SUT" teardown --instance-id i-keep0000 --region us-east-2 >/dev/null 2>&1; rc=$?
log="$(cat "$TMP/aws.log")"
if [ "$rc" = 1 ] && ! grep -q 'terminate-instances' <<<"$log"; then
  pass "teardown refuses lifecycle=keep box without --force (no terminate)"
else
  fail "teardown keep guard" "rc=$rc log=<<<$log>>>"
fi

# ============================================================================
# 8. teardown --force overrides the keep guard.
# ============================================================================
: > "$TMP/aws.log"
PATH="$BIN:$PATH" AWS_LOG="$TMP/aws.log" STUB_LIFECYCLE=keep \
  bash "$SUT" teardown --instance-id i-keep0000 --region us-east-2 --force >/dev/null 2>&1; rc=$?
log="$(cat "$TMP/aws.log")"
if [ "$rc" = 0 ] && grep -q 'terminate-instances.*i-keep0000' <<<"$log"; then
  pass "teardown --force terminates a keep-tagged box"
else
  fail "teardown --force" "rc=$rc log=<<<$log>>>"
fi

# ============================================================================
# 9. reap terminates an expired ephemeral box, spares keep + not-yet-due.
#    Rows: id \t launch \t expiry-epoch \t lifecycle
#      i-expired : expiry in the past             → reap
#      i-keep    : keep                            → spare
#      i-future  : expiry far in the future        → spare
# ============================================================================
PAST=$(( $(date +%s) - 600 ))
FUTURE=$(( $(date +%s) + 36000 ))
ROWS="i-expired\t2026-06-11T00:00:00Z\t${PAST}\tephemeral\ni-keep\t2026-06-11T00:00:00Z\t${PAST}\tkeep\ni-future\t2026-06-11T00:00:00Z\t${FUTURE}\tephemeral\n"
: > "$TMP/aws.log"
out="$(PATH="$BIN:$PATH" AWS_LOG="$TMP/aws.log" STUB_REAP_ROWS="$ROWS" \
        bash "$SUT" reap --region us-east-2 2>&1)"; rc=$?
log="$(cat "$TMP/aws.log")"
term_expired=0; term_keep=0; term_future=0
grep -q 'terminate-instances.*i-expired' "$TMP/aws.log" && term_expired=1
grep -q 'terminate-instances.*i-keep'    "$TMP/aws.log" && term_keep=1
grep -q 'terminate-instances.*i-future'  "$TMP/aws.log" && term_future=1
if [ "$rc" = 0 ] && [ "$term_expired" = 1 ] && [ "$term_keep" = 0 ] && [ "$term_future" = 0 ]; then
  pass "reap terminates expired ephemeral box, spares keep + not-yet-due"
else
  fail "reap decision" "rc=$rc expired=$term_expired keep=$term_keep future=$term_future log=<<<$log>>>"
fi

# ============================================================================
# 10. unknown subcommand exits 2.
# ============================================================================
PATH="$BIN:$PATH" AWS_LOG="$TMP/aws.log" bash "$SUT" frobnicate >/dev/null 2>&1; rc=$?
if [ "$rc" = 2 ]; then pass "unknown subcommand exits 2"
else fail "unknown subcommand" "rc=$rc (want 2)"; fi

echo "----"
echo "PASS=$PASS FAIL=$FAIL"
[ "$FAIL" -eq 0 ]
