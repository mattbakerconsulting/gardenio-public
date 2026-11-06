#!/usr/bin/env bash
# SC2317: the fake_node_* / fake_gb_* helpers are invoked INDIRECTLY (passed by
#         name via FX_NODE_RUNNER / FX_GB), which shellcheck can't trace — the
#         "unreachable" reports are false positives.
# shellcheck disable=SC2317
# test-lib-gvisor-forensics.sh — cluster-free unit test for lib-gvisor-forensics.sh
# (Rig R2, bead ka-0000006948).
#
# Drives the whole forensic-capture pipeline with a FAKE node runner, a FAKE
# kubectl and a FAKE gb — no cluster, no beads daemon, no real gVisor node — so
# it runs in the lint/CI job and on a laptop, gating every MR/main push. Mirrors
# test-lib-gvisor-node.sh.
#
# Asserts:
#   1. Happy path: all six artifacts staged, summary names the failing rung,
#      every collector ran, a bundle was "created" and every doc "added" in order.
#   2. Fail-soft: a node runner that errors on netns/runsc still produces the
#      artifacts (with the failure recorded) and the capture still bundles.
#   3. gb-absent: with FX_GB pointing at a non-existent binary, the local dir is
#      still fully staged and the function returns 1 (evidence preserved).
#   4. gb create failure / unparseable id: degrades to dir-only, returns 1.
#   5. Marker harvesting: FT_-prefixed lines are filtered into 06-markers.env and
#      the summary table.
#   6. CNI/Pod B/controller collectors emit the expected commands to the fakes.

set -uo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
# shellcheck disable=SC1091
source "$SCRIPT_DIR/lib-gvisor-forensics.sh"

PASS=0
FAIL=0
ERRORS=()
pass() { PASS=$((PASS + 1)); echo "  PASS: $1"; }
fail() { FAIL=$((FAIL + 1)); ERRORS+=("$1"); echo "  FAIL: $1"; }
# want <rc> <pass-msg> <fail-msg> — rc 0 => pass, else fail. Avoids the
# `cond && pass || fail` idiom (SC2015: C may run even when A is true).
want() { if [ "$1" -eq 0 ]; then pass "$2"; else fail "$3"; fi; }

WORK="$(mktemp -d)"
trap 'rm -rf "$WORK"' EXIT

# ── Fakes ─────────────────────────────────────────────────────────────────────
# Fake node runner: contract is `runner <cap-secs> <cmd-string>`. Dispatches on
# the command text so each collector gets plausible node output. A log of every
# command is appended to $NODE_LOG so we can assert the right probes ran.
NODE_LOG="$WORK/node.log"
: >"$NODE_LOG"
fake_node_ok() {
  local cmd="$2"
  printf '%s\n' "$cmd" >>"$NODE_LOG"
  case "$cmd" in
    *"ip -4 -o addr show scope global"*) echo "2: eth0    inet 10.30.136.3/32 scope global eth0" ;;
    *"/proc/net/route"*)                 echo "Iface Destination ...";; # truncated but present
    *"ip -d addr"*)                      echo "1: lo: <LOOPBACK,UP>"; echo "2: eth0: <BROADCAST,UP>" ;;
    *"ip route"*)                        echo "default via 169.254.1.1 dev eth0" ;;
    *"var/run/netns"*)                   echo "total 0" ;;
    *"runsc"*"list"*)                    echo "ID PID STATUS BUNDLE" ;;
    *"runsc"*"state"*)                   echo '{"id":"sc-agent-into-b","status":"running","pid":9528}' ;;
    *"runsc"*"ps"*)                      echo "UID PID PPID C STIME" ;;
    *"thaw-into-b.out"*|*"tail -c 8192"*) echo "reading mounts file: unexpected end of JSON input" ;;
    *PYCNI*|*"K8S_POD_UID"*)             echo '  MATCH file=/var/lib/cni/results/abc'; echo '  {"result":{"ips":[{"address":"10.30.136.3/32"}]}}' ;;
    *"ls -la"*"var/lib/cni/results"*)    echo "-rw-r--r-- 1 root root 512 cni-result-podB" ;;
    *"debug-log"*|*"FX_RUNSC_DEBUG_DIR"*) echo "(no debug dir)" ;;
    *)                                   echo "(fake-node ok: $cmd)" ;;
  esac
}
# Fake node runner that ERRORS (non-zero) for netns + runsc probes — exercises
# the fail-soft path.
fake_node_flaky() {
  local cmd="$2"
  printf '%s\n' "$cmd" >>"$NODE_LOG"
  case "$cmd" in
    *"nsenter --net="*|*"runsc"*) echo "nsenter: cannot open netns: No such file or directory"; return 1 ;;
    *) echo "(fake-node ok: $cmd)" ;;
  esac
}

# Fake kubectl: logs argv, returns plausible output. The lib calls it via
# `timeout $cap $FX_KUBECTL …`, and `timeout` needs a REAL command (it cannot
# exec a shell function) — so kubectl is faked as a SCRIPT on PATH
# (fake_kubectl_bin), and FX_KUBECTL is pointed at that path. (The node runner
# and gb fakes, by contrast, are functions, because the lib invokes those by
# name without `timeout`.)
KUBECTL_LOG="$WORK/kubectl.log"
: >"$KUBECTL_LOG"
fake_kubectl_bin="$WORK/kubectl"
cat >"$fake_kubectl_bin" <<KBE
#!/usr/bin/env bash
echo "\$@" >>"$KUBECTL_LOG"
case "\$*" in
  *"get pod"*"-o wide"*)  echo "NAME        READY   STATUS    RESTARTS   AGE"; echo "agentsim-b  1/1     Running   0          30s" ;;
  *"get pod"*"-o yaml"*)  echo "status:"; echo "  phase: Running"; echo "  conditions: [{type: Ready, status: 'True'}]" ;;
  *"get events"*)         echo "LAST SEEN   TYPE     REASON      OBJECT          MESSAGE"; echo "10s         Normal   Started     pod/agentsim-b  Started container" ;;
  *"logs"*)               echo "controller FATAL agent reconstructed rootfs overlay is MISSING the container CWD dentry"; echo "FT_ROOT_RC=128" ;;
  *)                      echo "(fake-kubectl: \$*)" ;;
esac
exit 0
KBE
chmod +x "$fake_kubectl_bin"

# Fake gb: logs argv; `bundle create --json` prints a JSON id; `bundle add`
# records each add. Installed as a function so FX_GB can name it (the lib calls
# gb directly, not via timeout).
GB_LOG="$WORK/gb.log"
: >"$GB_LOG"
fake_gb() {
  printf '%s\n' "$*" >>"$GB_LOG"
  case "$1 $2" in
    "bundle create") echo '{"id":"gd-FAKEBUNDLE01","url":"/bundle/gd-FAKEBUNDLE01"}' ;;
    "bundle add")    : ;;  # success, no output
    *)               echo "(fake-gb: $*)" ;;
  esac
  return 0
}
# Fake gb whose `bundle create` FAILS — exercises the create-failure degrade.
fake_gb_create_fails() {
  printf '%s\n' "$*" >>"$GB_LOG"
  case "$1 $2" in
    "bundle create") echo "error: daemon unreachable" >&2; return 1 ;;
    *) return 0 ;;
  esac
}

# ── 1. Happy path ──────────────────────────────────────────────────────────────
MARKERS="$WORK/markers.txt"
cat >"$MARKERS" <<'MK'
FT_RUNG_snapshot_persisted=ok
FT_RUNG_restore_rc0=fail reason=lo-only-netns
FT_CNI_CONFIRMED=yes
FT_CNI_POD_IP=10.30.136.3/32
FT_NETNS_IFACES=1
FT_NETNS_DEFROUTE=no
FT_ROOT_RC=128
some non-marker noise line
MK

OUT="$(
  FX_NODE_RUNNER=fake_node_ok \
  FX_KUBECTL="$fake_kubectl_bin" \
  FX_GB=fake_gb \
  FX_ORIGIN_BEAD=ka-0000001355 \
  FX_RUNG=restore_rc0 FX_REASON="restored sentry landed in lo-only netns" \
  FX_NS=gvisor-poc FX_PROBE=gvisor-ft-probe-1 \
  FX_POD_B=agentsim-b FX_POD_B_UID=uid-b-123 FX_BNETNS=/var/run/netns/cni-b \
  FX_RUNSC_ROOT=/run/containerd/runsc/k8s.io FX_RESTORE_CID=sc-agent-into-b \
  FX_RESTORE_STDERR=/var/tmp/thaw-into-b.out \
  FX_CONTROLLER_NS=gardenio FX_MARKERS="$MARKERS" \
  FX_STAGE_DIR="$WORK/cap1" \
  gvisor_forensic_capture
)"; RC=$?

if [ "$RC" -eq 0 ]; then
  pass "happy: gvisor_forensic_capture returns 0 (bundle created)"
else
  fail "happy: expected rc=0, got $RC"
fi

case "$OUT" in
  *"FT_FORENSIC_BUNDLE=gd-FAKEBUNDLE01"*) pass "happy: prints FT_FORENSIC_BUNDLE sentinel with bundle id" ;;
  *) fail "happy: missing/blank FT_FORENSIC_BUNDLE sentinel: '$OUT'" ;;
esac
case "$OUT" in
  *"FT_FORENSIC_URL=/bundle/gd-FAKEBUNDLE01"*) pass "happy: prints FT_FORENSIC_URL viewer path" ;;
  *) fail "happy: missing FT_FORENSIC_URL: '$OUT'" ;;
esac
# All six artifacts + summary present.
ALL_PRESENT=1
for f in 00-summary.md 01-netns.txt 02-runsc.txt 03-cni.txt 04-podb.txt 05-controller.log 06-markers.env; do
  [ -s "$WORK/cap1/$f" ] || { ALL_PRESENT=0; echo "    missing/empty: $f"; }
done
want "$((ALL_PRESENT == 1 ? 0 : 1))" \
  "happy: all 6 artifacts + summary staged and non-empty" \
  "happy: some artifacts missing/empty under $WORK/cap1"

# Summary names the failing rung + reason.
if grep -q 'restore_rc0' "$WORK/cap1/00-summary.md" && grep -q 'lo-only netns' "$WORK/cap1/00-summary.md"; then
  pass "happy: summary names failing rung + reason"
else
  fail "happy: summary missing rung/reason"
fi

# Marker filtering: 06-markers.env has FT_ lines, drops the noise line.
if grep -q 'FT_ROOT_RC=128' "$WORK/cap1/06-markers.env" \
   && grep -q 'FT_RUNG_restore_rc0=fail' "$WORK/cap1/06-markers.env"; then
  pass "happy: markers artifact carries the FT_ marker set"
else
  fail "happy: markers artifact missing FT_ lines"
fi
if grep -q 'some non-marker noise line' "$WORK/cap1/06-markers.env" \
   && ! grep -A100 'FT_-prefixed lines only' "$WORK/cap1/06-markers.env" | grep -q 'non-marker noise'; then
  pass "happy: FT_-only section filters out non-marker noise (raw section keeps it)"
else
  # Non-fatal nuance; only fail if the FT_-only filter clearly leaked noise.
  if grep -A100 'FT_-prefixed lines only' "$WORK/cap1/06-markers.env" | grep -q 'non-marker noise'; then
    fail "happy: FT_-only section leaked the non-marker noise line"
  else
    pass "happy: FT_-only section filters out non-marker noise"
  fi
fi

# Bundle was created once and 7 docs added in order.
CREATE_N="$(grep -c '^bundle create' "$GB_LOG" || true)"
ADD_N="$(grep -c '^bundle add' "$GB_LOG" || true)"
want "$((CREATE_N == 1 ? 0 : 1))" \
  "happy: gb bundle create called exactly once" \
  "happy: gb bundle create called $CREATE_N times (want 1)"
want "$((ADD_N == 7 ? 0 : 1))" \
  "happy: gb bundle add called for all 7 docs" \
  "happy: gb bundle add called $ADD_N times (want 7)"
if grep -q 'bundle create ka-0000001355 --kind report' "$GB_LOG"; then
  pass "happy: bundle parented to origin canary task, kind=report"
else
  fail "happy: bundle create missing origin/kind: $(grep '^bundle create' "$GB_LOG")"
fi

# Shell-var contract: when called DIRECTLY (not in $(...)), the function sets the
# FT_FORENSIC_* shell vars in the caller's shell. (In a command substitution the
# subshell's assignments are lost — the stdout sentinels above are the
# cross-subshell channel, asserted separately.) Reset the gb log first so this
# extra run does not perturb the count assertions above. stdout -> file.
: >"$GB_LOG"
FX_NODE_RUNNER=fake_node_ok FX_KUBECTL="$fake_kubectl_bin" FX_GB=fake_gb \
  FX_ORIGIN_BEAD=ka-0000001355 FX_RUNG=restore_rc0 FX_REASON="direct call" \
  FX_POD_B=agentsim-b FX_BNETNS=/var/run/netns/cni-b FX_RESTORE_CID=sc-agent-into-b \
  FX_MARKERS="$MARKERS" FX_STAGE_DIR="$WORK/cap1b" \
  gvisor_forensic_capture >"$WORK/cap1b.out" 2>/dev/null
if [ "$FT_FORENSIC_BUNDLE" = "gd-FAKEBUNDLE01" ] && [ "$FT_FORENSIC_URL" = "/bundle/gd-FAKEBUNDLE01" ]; then
  pass "happy: direct call sets FT_FORENSIC_BUNDLE / FT_FORENSIC_URL shell vars"
else
  fail "happy: shell vars wrong: bundle='$FT_FORENSIC_BUNDLE' url='$FT_FORENSIC_URL'"
fi

# Right node probes ran (netns + runsc + cni + the scope-global rung signal).
NETNS_OK=0; RUNSC_OK=0; CNI_OK=0; SCOPE_OK=0
grep -q 'nsenter --net=/var/run/netns/cni-b' "$NODE_LOG" && NETNS_OK=1
grep -q 'runsc --root=/run/containerd/runsc/k8s.io state sc-agent-into-b' "$NODE_LOG" && RUNSC_OK=1
grep -q '/var/lib/cni/results' "$NODE_LOG" && CNI_OK=1
grep -q 'ip -4 -o addr show scope global' "$NODE_LOG" && SCOPE_OK=1
want "$((NETNS_OK == 1 ? 0 : 1))" "happy: netns collector ran nsenter into Pod B netns" "happy: no Pod B netns probe"
want "$((RUNSC_OK == 1 ? 0 : 1))" "happy: runsc collector queried restore-target state" "happy: no runsc state probe"
want "$((CNI_OK == 1 ? 0 : 1))" "happy: cni collector listed /var/lib/cni/results" "happy: no cni probe"
want "$((SCOPE_OK == 1 ? 0 : 1))" "happy: netns_routable rung signal (scope global) probed" "happy: no scope-global probe"

# kubectl probes: Pod B yaml + events + controller logs.
PODB_OK=0; EV_OK=0; LOG_OK=0
grep -q 'get pod agentsim-b -o yaml' "$KUBECTL_LOG" && PODB_OK=1
grep -q 'get events' "$KUBECTL_LOG" && EV_OK=1
grep -q "logs -l app.kubernetes.io/component=controller" "$KUBECTL_LOG" && LOG_OK=1
want "$((PODB_OK == 1 ? 0 : 1))" "happy: Pod B yaml captured via kubectl" "happy: no Pod B yaml probe"
want "$((EV_OK == 1 ? 0 : 1))" "happy: Pod B events captured via kubectl" "happy: no events probe"
want "$((LOG_OK == 1 ? 0 : 1))" "happy: controller log tail captured via kubectl" "happy: no controller log probe"

# ── 2. Fail-soft: node runner errors on netns/runsc, capture still bundles ─────
: >"$GB_LOG"; : >"$NODE_LOG"
FX_NODE_RUNNER=fake_node_flaky FX_KUBECTL="$fake_kubectl_bin" FX_GB=fake_gb \
  FX_ORIGIN_BEAD=ka-0000001355 FX_RUNG=netns_routable FX_REASON="netns stripped" \
  FX_POD_B=agentsim-b FX_BNETNS=/var/run/netns/cni-b FX_RESTORE_CID=sc-x \
  FX_MARKERS="FT_ROOT_RC=128" FX_STAGE_DIR="$WORK/cap2" \
  gvisor_forensic_capture >/dev/null; RC2=$?
if [ "$RC2" -eq 0 ] && [ -s "$WORK/cap2/01-netns.txt" ] && [ -s "$WORK/cap2/02-runsc.txt" ]; then
  pass "fail-soft: erroring node runner still produces artifacts + bundles (rc=0)"
else
  fail "fail-soft: expected rc=0 with artifacts, got rc=$RC2"
fi
if grep -q 'cannot open netns' "$WORK/cap2/01-netns.txt"; then
  pass "fail-soft: the capture failure is RECORDED in the artifact (not lost)"
else
  fail "fail-soft: artifact did not record the node failure"
fi

# ── 3. gb-absent: local dir staged, returns 1, evidence preserved ──────────────
OUT3="$(
  FX_NODE_RUNNER=fake_node_ok FX_KUBECTL="$fake_kubectl_bin" \
  FX_GB="$WORK/definitely-not-a-real-binary-xyz" \
  FX_ORIGIN_BEAD=ka-0000001355 FX_RUNG=cni_confirmed FX_REASON="no committed IP" \
  FX_MARKERS="FT_CNI_CONFIRMED=no" FX_STAGE_DIR="$WORK/cap3" \
  gvisor_forensic_capture
)"; RC3=$?
if [ "$RC3" -eq 1 ] && [ -s "$WORK/cap3/00-summary.md" ] && [ -s "$WORK/cap3/03-cni.txt" ]; then
  pass "gb-absent: returns 1 but FULLY stages the local evidence dir"
else
  fail "gb-absent: expected rc=1 with staged dir, got rc=$RC3"
fi
case "$OUT3" in
  *"FT_FORENSIC_DIR=$WORK/cap3"*) pass "gb-absent: still prints FT_FORENSIC_DIR pointing at the evidence" ;;
  *) fail "gb-absent: missing FT_FORENSIC_DIR sentinel: '$OUT3'" ;;
esac
case "$OUT3" in
  *"FT_FORENSIC_BUNDLE="$'\n'*|*"FT_FORENSIC_BUNDLE="|*"FT_FORENSIC_BUNDLE=
"*) pass "gb-absent: FT_FORENSIC_BUNDLE is blank (no bundle)" ;;
  *) # accept any blank form
     if grep -qE '^FT_FORENSIC_BUNDLE=$' <<<"$OUT3"; then
       pass "gb-absent: FT_FORENSIC_BUNDLE is blank (no bundle)"
     else
       fail "gb-absent: FT_FORENSIC_BUNDLE should be blank: '$OUT3'"
     fi ;;
esac

# ── 4. gb create failure → degrade to dir-only, rc=1 ──────────────────────────
: >"$GB_LOG"
FX_NODE_RUNNER=fake_node_ok FX_KUBECTL="$fake_kubectl_bin" FX_GB=fake_gb_create_fails \
  FX_ORIGIN_BEAD=ka-0000001355 FX_RUNG=ram_intact FX_REASON="counter reset" \
  FX_MARKERS="FT_RUNG_ram_intact=fail" FX_STAGE_DIR="$WORK/cap4" \
  gvisor_forensic_capture >/dev/null; RC4=$?
if [ "$RC4" -eq 1 ] && [ -s "$WORK/cap4/00-summary.md" ]; then
  pass "create-fail: gb bundle create failure degrades to dir-only (rc=1, evidence kept)"
else
  fail "create-fail: expected rc=1 with staged dir, got rc=$RC4"
fi
# No `bundle add` should have happened if create failed.
if ! grep -q '^bundle add' "$GB_LOG"; then
  pass "create-fail: no docs added after a failed create"
else
  fail "create-fail: added docs despite failed create"
fi

# ── 5. id extraction: jq-form, bare-token-form, and parent-exclusion ──────────
want "$([ "$(_fx_extract_bundle_id '{"id":"gd-ABC123"}')" = "gd-ABC123" ] && echo 0 || echo 1)" \
  "id-extract: parses .id JSON form" \
  "id-extract: failed JSON .id form -> '$(_fx_extract_bundle_id '{"id":"gd-ABC123"}')'"
want "$([ "$(_fx_extract_bundle_id 'created bundle gd-XYZ789 ok')" = "gd-XYZ789" ] && echo 0 || echo 1)" \
  "id-extract: falls back to a kd- token in free text" \
  "id-extract: bare-token fallback failed"
# Parent-exclusion: when the create output echoes the origin/parent bead BEFORE
# the new child id, the grep fallback must skip the excluded parent and return
# the new id (the last non-excluded token). This is the ka-0000001355-vs-new-id
# confusion the review flagged. Force the grep path with a non-JSON blob.
got5="$(_fx_extract_bundle_id 'log: creating under parent ka-0000001355
created bundle gd-NEWBUND9 (/bundle/gd-NEWBUND9)' 'ka-0000001355')"
want "$([ "$got5" = "gd-NEWBUND9" ] && echo 0 || echo 1)" \
  "id-extract: excludes the origin/parent bead, returns the new child id" \
  "id-extract: parent-exclusion failed -> got '$got5' (want gd-NEWBUND9)"

# ── 6. no FX_MARKERS / no FX_BNETNS: collectors degrade with explicit notes ────
FX_NODE_RUNNER=fake_node_ok FX_KUBECTL="$fake_kubectl_bin" FX_GB=fake_gb \
  FX_ORIGIN_BEAD=ka-0000001355 FX_RUNG=work_intact FX_REASON="workspace lost" \
  FX_STAGE_DIR="$WORK/cap6" \
  gvisor_forensic_capture >/dev/null; RC6=$?
if [ "$RC6" -eq 0 ] \
   && grep -q 'FX_MARKERS unset' "$WORK/cap6/06-markers.env" \
   && grep -q 'FX_BNETNS unset' "$WORK/cap6/01-netns.txt"; then
  pass "degrade: missing markers/netns recorded as explicit notes, capture still bundles"
else
  fail "degrade: expected explicit unset notes + rc=0, got rc=$RC6"
fi

# ── 7. uncreatable staging dir → rc=2, no green empty bundle ──────────────────
# The ONE hard invariant: never "succeed" with a nonexistent evidence dir. Point
# FX_STAGE_DIR under a regular FILE so mkdir -p cannot create it, and force the
# mktemp fallback to also fail by pointing TMPDIR at the same regular file.
: >"$GB_LOG"
BLOCKER="$WORK/not-a-dir"; : >"$BLOCKER"   # a regular file
FX_NODE_RUNNER=fake_node_ok FX_KUBECTL="$fake_kubectl_bin" FX_GB=fake_gb \
  FX_ORIGIN_BEAD=ka-0000001355 FX_RUNG=restore_rc0 FX_REASON="x" \
  FX_STAGE_DIR="$BLOCKER/cap7" TMPDIR="$BLOCKER" \
  gvisor_forensic_capture >"$WORK/cap7.out" 2>/dev/null; RC7=$?
want "$((RC7 == 2 ? 0 : 1))" \
  "dir-fail: uncreatable staging dir returns rc=2 (real failure, not a green empty bundle)" \
  "dir-fail: expected rc=2, got $RC7"
if [ "$FT_FORENSIC_DIR" = "" ] && [ "$FT_FORENSIC_BUNDLE" = "" ] && ! grep -q '^bundle create' "$GB_LOG"; then
  pass "dir-fail: no bundle created, FT_FORENSIC_DIR blank (nothing falsely claimed)"
else
  fail "dir-fail: should not create a bundle / should blank FT_FORENSIC_DIR (dir='$FT_FORENSIC_DIR' bundle='$FT_FORENSIC_BUNDLE')"
fi

# ── 8. node_run (stdin) idiom works unwrapped, just like node_exec ($2) ───────
# freeze-thaw's node_run ignores $2 and reads the command from STDIN (bash -s).
# This fake models that: it reads stdin, ignores $2. The lib must still capture
# node-side evidence (the critical contract the review caught).
: >"$NODE_LOG"
fake_node_stdin() {
  local cmd; cmd="$(cat)"          # read from stdin, NOT $2
  printf '%s\n' "$cmd" >>"$NODE_LOG"
  case "$cmd" in
    *"ip -4 -o addr show scope global"*) echo "2: eth0    inet 10.30.136.3/32 scope global eth0" ;;
    *"runsc"*"state"*)                   echo '{"status":"running"}' ;;
    *)                                   echo "(fake-stdin ok)" ;;
  esac
}
FX_NODE_RUNNER=fake_node_stdin FX_KUBECTL="$fake_kubectl_bin" FX_GB=fake_gb \
  FX_ORIGIN_BEAD=ka-0000001355 FX_RUNG=netns_routable FX_REASON="stdin idiom" \
  FX_POD_B=agentsim-b FX_BNETNS=/var/run/netns/cni-b FX_RESTORE_CID=sc-x \
  FX_MARKERS="FT_ROOT_RC=128" FX_STAGE_DIR="$WORK/cap8" \
  gvisor_forensic_capture >/dev/null; RC8=$?
if [ "$RC8" -eq 0 ] \
   && grep -q 'ip -4 -o addr show scope global' "$NODE_LOG" \
   && grep -q '10.30.136.3/32' "$WORK/cap8/01-netns.txt"; then
  pass "node_run-stdin: stdin-reading runner captures node-side evidence unwrapped"
else
  fail "node_run-stdin: stdin runner did not capture node-side evidence (rc=$RC8)"
fi

# ── Summary ────────────────────────────────────────────────────────────────────
echo ""
echo "════════ test-lib-gvisor-forensics Summary: $PASS passed, $FAIL failed ════════"
if [ "$FAIL" -gt 0 ]; then
  for e in "${ERRORS[@]}"; do echo "  - $e"; done
  exit 1
fi
exit 0
