#!/usr/bin/env bash
# discriminating-restore-check.sh — bug ka-0000000357, gardener req-1 (steer ka-0000008062)
#                                   v2 (plural_fix, ka-0000001913)
#
# THE ZERO-CANARY ON-NODE DISCRIMINATING CHECK (LIVE half).
#
# Settles, empirically and at ZERO canary cost, whether the dir-medium plural_fix
# (freeze-side full-skeleton mkdir + image-lowerdir-stacking, freezethaw_node.go [3a])
# yields a thawable, NON-HOLLOW rootfs — or whether the restore still rc=128's on a
# sparse lower (FAIL) or green-lights a HOLLOW rootfs that ENOENTs the instant the
# agent execs anything (the cycle-14 trap). v2 RESULT ladder (see the foot of file):
#
#   PASS  => `runsc --overlay2=root:dir restore` of the persisted freeze bundle
#            completes (sandbox+agent rc=0, no 9p-walk ENOENT) AND the restored sentry
#            has a POPULATED cwd AND a NON-HOLLOW rootfs (a base binary 9p-walks + a
#            base file reads inside the sentry). => safe to open the gated MR / canary.
#   FAIL  => restore rc!=0 with a 'failed to walk "X" of type 040000' ENOENT (the
#            dir-medium sparse-lower rc=128 — EXPECTED on a PRE-fix bundle, proving the
#            gate discriminates), OR the cwd is absent/empty post-restore (real loss).
#   HOLLOW=> the walk + cwd passed but a base-image binary/file is ABSENT inside the
#            sentry (empty /usr). NO-GO TRIPWIRE: the cp-source/stacking fix is
#            MANDATORY before any canary (skeleton-only is hollow — ka-0000000259).
#   INCONCLUSIVE => an in-sentry exec flaked (sentry not joinable / timeout). Never PASS.
#   SETUP-ERROR  => a precondition failed (DEFECT-A parse/mount, bundle/image absent,
#            sandbox-id repoint, runsc missing). NOT a Fix verdict — fix setup, re-run.
#
# v2 fixes three confirmed harness defects (operator on-node run 2026-06-28 20:41Z):
#   DEFECT-A — .overlay-opts parse was wrong (literal `lowerdir=` left in) + mount
#              failure was NON-fatal -> unmounted rootfs -> opaque rc=128 script
#              artifact. Now uses thaw's build_lowers parser; mount failure is FATAL.
#   DEFECT-B — PHASE-1 poll waited for mere registration, not the count-barrier PARK
#              state, so PHASE-2 could race the sandbox. Now polls for restoring-
#              unstarted/created. (PHASE-2 keeps --detach: a non-detached sub restore
#              hangs on the agent init -> rc=124, never the real restore rc.)
#   DEFECT-C — liveness was CWD-only, blind to a hollow /usr. Now adds ASSERT-2
#              ROOTFS_NOT_HOLLOW (base-binary exec + base-file read).
#
# WHY A RAW `runsc restore` IS THE CHECK. The assertion that fail-closed prod is
# controller-side BASH (remount_rootfs POST-MOUNT CWD-dentry test, freezethaw_node.go
# ~1090-1140). A standalone `runsc restore` invoked directly NEVER runs that bash, so
# it inherently "bypasses the assertion" — there is nothing to patch out. The only
# question this answers is the load-bearing one: does runsc itself rebuild the
# rootfs-overlay upper (incl. /home/agent/workspace) from checkpoint.img under
# --overlay2=root:dir? If yes, the lower-only pre-restore gate was wrong to fail.
#
# COUNT-GATED N-CONTAINER BARRIER (ka-0000007097 / ka-0000003561). The real thaw
# (thawTmpl [5], gardenio/controller/internal/podmanager/freezethaw_node.go) is NOT a
# standalone agent restore: a persisted pod checkpoint bakes container_count=N into
# checkpoint.img, so the SANDBOX-root must be restored FIRST (it parks at
# restoringUnstarted reading that count), and then the AGENT sub-container restore
# satisfies the count and unblocks both, linked by the agent's io.kubernetes.cri.
# sandbox-id annotation. Restoring the agent sub ALONE makes runsc's sentry block
# looking for the parent sandbox .state file that was never written under our $ROOT,
# and it rc=128s with "cannot load sandbox: open <cid>_sandbox:<cid>.state: no such
# file or directory" — a SCRIPT-TOPOLOGY artifact, NOT a Fix-B verdict. So this check
# mirrors the proven two-phase barrier (sandbox-first, then agent sub; see also the
# PoC multi-container-resume-node.sh, archive/clutter-pre-delete-2026-10-28; write-up
# gardenio/docs/gvisor-runsc-cr/multi-container-resume.md). NOTE: the barrier
# target is read by runsc FROM checkpoint.img — it is NOT a CLI flag in this gVisor
# build (neither thawTmpl nor the PoC passes --container-count); do not add one.
#
# WHERE THIS RUNS. Inside the privileged probe pod (probe-pod.yaml: privileged,
# hostPID, hostPath / mounted at /host), on the dir-medium gVisor node that holds the
# persisted bundle. It chroots /host and drives the node's own runsc, exactly like
# the controller's node-restore script. It is READ-ONLY w.r.t. prod: it restores into
# SCRATCH cids (sandbox + agent) + a COPY of the bundle/overlay under /var/tmp, never
# touches the live checkpoint image, never deletes a pod, never reads/flips
# gvisor.freezeThaw. Cleanup deletes only its own scratch cids + /var/tmp working dirs.
#
# PREREQS (operator / privileged probe):
#   - kubectl ctx on the prod cluster (america-e2e-eks, acct 234567890123); the
#     bundle node for canary ka-0000005316 was ip-10-30-136-242.
#   - the persisted freeze bundle + checkpoint image must still exist on that node:
#       BUNDLE = /var/lib/gardenio/bundle/<agent>     (BundleHostBase)
#       IMG    = /var/lib/gardenio/ckpt/<agent>       (CheckpointHostBase)
#       OVER   = /var/lib/gardenio/overlay            (dir-medium filestore root)
#     The pk8 canary agent was "gardenio-uuae"; pass it via --agent or $AGENT. If the
#     5-day-old bundle was reaped, re-drive ONE freeze on a fresh gvisor-poc agent
#     to regenerate a persisted bundle, then run this against it (still zero-thaw).
#
# USAGE (from inside the probe pod, as root):
#   chroot /host /bin/bash -c '/path/to/discriminating-restore-check.sh --agent gardenio-uuae'
# or, having copied it onto the node:
#   AGENT=gardenio-uuae ./discriminating-restore-check.sh
#
# FLAGS:
#   --agent <name>     agent whose persisted bundle to restore (or $AGENT).
#   --bundle <path>    override BUNDLE (default /var/lib/gardenio/bundle/<agent>).
#   --img <path>       override IMG    (default /var/lib/gardenio/ckpt/<agent>).
#   --overlay <path>   override dir-medium filestore root (default /var/lib/gardenio/overlay).
#   --cwd <path>       checkpointed container CWD to assert (default: read from
#                      <bundle>/agent/config.json process.cwd; falls back to
#                      /home/agent/workspace).
#   --keep             do NOT clean up the scratch cids + /var/tmp work (debug).
#   --root <path>      runsc --root (default /run/containerd/runsc/k8s.io).
#
# EXIT: 0 = PASS (restore rc=0 AND cwd populated AND rootfs NON-HOLLOW).
#       1 = FAIL / HOLLOW / INCONCLUSIVE (the RESULT= line distinguishes them).
#       2 = SETUP-ERROR (DEFECT-A parse/mount, bundle/image absent, repoint, runsc
#           missing) — NOT a Fix verdict; fix the setup and re-run.
set -uo pipefail

# ════════════════════════════════════════════════════════════════════════════
# PURE, SOURCEABLE CORE — hermetically tested by discriminating-restore-check_test.sh
# ════════════════════════════════════════════════════════════════════════════
# These two functions carry the LOAD-BEARING OFFLINE logic this gate exists to
# protect against silent regression (bug ka-0000000357 req-1 / task ka-0000002792):
#   - build_lowers  — the DEFECT-A `.overlay-opts` parser (a wrong parse left the
#                     rootfs unmounted and made the restore rc=128 a SCRIPT artifact,
#                     mis-readable as a real data-loss verdict).
#   - drc_verdict   — the PRE-FIX=FAIL / skeleton-only=HOLLOW / skeleton+stacking
#                     =PASS verdict ladder, the durable-regression artifact named by
#                     the task. Given the measured restore rcs + in-sentry assertions
#                     it classifies the cycle and chooses the exit code.
# Both are PURE: they run NO runsc, mount, kubectl, or network, so a cluster-free CI
# gate (checks.yml script-selftests runs discriminating-restore-check_test.sh) can assert the ladder's
# classification + the parser can never silently re-break. The LIVE half (count-gated
# barrier restore + the asserts that PRODUCE those measurements) still requires the
# on-node operator run — see discriminating-restore-check.md "What CI covers vs node".

# build_lowers — EXACT parity with the controller's thaw build_lowers
# (freezethaw_node.go remount_rootfs). DEFECT-A (ka-0000000357): freeze writes
# .overlay-opts as a SINGLE `lowerdir=<seg>[:<seg>...]` token (NO `:`-separated upper
# prefix, NO comma) — and, in the cp-FAILURE fallback, the raw live opts
# `rw,...,lowerdir=A:B,upperdir=C,...`. The prior `sed 's/^[^:]*://'` left the literal
# `lowerdir=` in the value for the freeze-written form (there is no `:` before it) ->
# mount got `lowerdir=lowerdir=/...` garbage and (worse) failed NON-FATALLY (return 0),
# leaving the rootfs unmounted so the later restore rc=128'd as a SCRIPT artifact. The
# robust fix is thaw's own parser: split on ',', pull the lowerdir + upperdir tokens,
# and (only if an upper exists — the fallback case) prepend it so written files win.
# This ALSO correctly handles the plural_fix multi-segment `lowerdir=$LOWER:img.0:img.1`.
build_lowers() { # $1=opts
  local opts="$1" upper lower
  upper=$(printf '%s' "$opts" | tr ',' '\n' | sed -n 's/^upperdir=//p')
  lower=$(printf '%s' "$opts" | tr ',' '\n' | sed -n 's/^lowerdir=//p')
  if [ -n "$upper" ]; then printf '%s:%s\n' "$upper" "$lower"; else printf '%s\n' "$lower"; fi
}

# drc_verdict — the verdict ladder, extracted VERBATIM from the prior inline block so
# the RESULT=/VERDICT lines and exit code are byte-identical (behavior-preserving).
# Args (all positional, so it stays pure / touches no globals):
#   $1 RC            agent-sub restore rc        $5 CWDRC        `test -e $CWD` rc
#   $2 SBRC          sandbox restore rc          $6 NENTRIES     entry count under CWD
#   $3 WALKMISS      9p-walk-ENOENT signature    $7 HOLLOWRC     base-binary exec rc
#   $4 CWD           checkpointed CWD (display)  $8 HOLLOW_BYTES base-file bytes read
# Prints the RESULT= + VERDICT block on stdout; returns 0 ONLY for PASS, 1 for
# FAIL / HOLLOW / INCONCLUSIVE. On a restore failure ($1!=0 || $2!=0) the in-sentry
# args ($5..$8) are not read, so the caller may pass 0s there.
drc_verdict() {
  local RC="$1" SBRC="$2" WALKMISS="$3" CWD="$4" CWDRC="$5" NENTRIES="$6" HOLLOWRC="$7" HOLLOW_BYTES="$8"
  if [ "$RC" -ne 0 ] || [ "$SBRC" -ne 0 ]; then
    echo "RESULT=FAIL  restore_rc=$RC  sandbox_rc=$SBRC  walk_miss='${WALKMISS:-}'"
    if [ -n "$WALKMISS" ]; then
      echo "VERDICT: FAIL — the restore 9p-walk ENOENT'd: $WALKMISS. This is the dir-medium"
      echo "  sparse-lower rc=128 (ka-0000000357). On a PRE-fix bundle this is EXPECTED and"
      echo "  PROVES the gate discriminates. On a POST-fix bundle it means the freeze-side"
      echo "  skeleton/stacking did not seed that dentry — do NOT open the gated MR; widen"
      echo "  the [3a] skeleton set or fix the stacking, then re-run."
    else
      echo "VERDICT: FAIL — the count-gated barrier restore did NOT complete (agent_rc=$RC,"
      echo "  sandbox_rc=$SBRC) under --overlay2=root:dir, with no 9p-walk-ENOENT signature."
      echo "  Capture the restore stdout (rc=124 => timeout/hang; 'cannot load sandbox'"
      echo "  => a barrier-ordering miss, re-run) and inspect before any release decision."
    fi
    return 1
  fi
  # ── RESULT LADDER (success-restore branch) ──
  # PASS          iff WALK_SURVIVES (rc==0) && CWD live (entries>0) && ROOTFS_NOT_HOLLOW
  #               (base exec rc=0 AND base file byte read).
  # HOLLOW        walk + CWD passed but base binary/file absent (rc=91, or rc=0 + 0 bytes)
  #               — the cycle-14 catch; the cp-source/stacking fix is MANDATORY.
  # FAIL          CWD absent (CWDRC==1) or present-but-empty (NENTRIES==0): real upper loss.
  # INCONCLUSIVE  an in-sentry exec flaked (rc>=125) — never PASS.
  if [ "$CWDRC" = 0 ] && [ "$NENTRIES" -gt 0 ] && [ "$HOLLOWRC" = 0 ] && [ "$HOLLOW_BYTES" -gt 0 ]; then
    echo "RESULT=PASS  restore_rc=0  cwd='$CWD'  entries=$NENTRIES  base_exec_rc=0  base_file_bytes=$HOLLOW_BYTES"
    echo "VERDICT: PASS — restore completed, '$CWD' is POPULATED ($NENTRIES entries), AND the"
    echo "  rootfs is NON-HOLLOW (a base binary 9p-walked + a base file read inside the"
    echo "  restored sentry). The dir-medium plural_fix (freeze-side skeleton + image"
    echo "  lowerdir-stacking) holds OFFLINE. Safe to open the gated MR / proceed to the"
    echo "  operator-gated canary."
    return 0
  elif [ "$HOLLOWRC" = 91 ] || { [ "$HOLLOWRC" = 0 ] && [ "$HOLLOW_BYTES" = 0 ]; }; then
    echo "RESULT=HOLLOW  restore_rc=0  cwd='$CWD'  entries=$NENTRIES  base_exec_rc=$HOLLOWRC  base_file_bytes=$HOLLOW_BYTES"
    echo "VERDICT: HOLLOW — the directory walk + CWD passed, but a base-image binary/file is"
    echo "  ABSENT inside the restored sentry (empty /usr). This is the cycle-14 hollow-rootfs"
    echo "  failure: execve(coop/gb/bash) would ENOENT the instant the agent runs anything."
    echo "  NO-GO TRIPWIRE: the cp-source / image-lowerdir-stacking fix is MANDATORY — do NOT"
    echo "  open the gated MR. (Skeleton-only is HOLLOW — ka-0000000259.)"
    return 1
  elif [ "$CWDRC" = 1 ] || { [ "$CWDRC" = 0 ] && [ "$NENTRIES" = 0 ]; }; then
    echo "RESULT=FAIL  restore_rc=0  cwd='$CWD'  cwd_test_rc=$CWDRC  entries=$NENTRIES"
    echo "VERDICT: FAIL — restore completed but '$CWD' is ABSENT or EMPTY inside the restored"
    echo "  sentry. The rootfs-overlay upper did NOT round-trip through checkpoint.img — REAL"
    echo "  workspace data loss (FT_DIRMEDIUM_CWD_LIVE=no parity). Do NOT open the gated MR."
    return 1
  else
    echo "RESULT=INCONCLUSIVE  restore_rc=0  cwd='$CWD'  cwd_test_rc=$CWDRC  entries=$NENTRIES  base_exec_rc=$HOLLOWRC  base_file_bytes=$HOLLOW_BYTES"
    echo "VERDICT: INCONCLUSIVE — restore rc=0 but an in-sentry exec flaked (rc>=125: sentry"
    echo "  not joinable / 'sh' missing / timeout). NOT a PASS. Re-run with a longer"
    echo "  RUNSC_TIMEOUT, or --keep and inspect the restored sentry manually."
    return 1
  fi
}

# ════════════════════════════════════════════════════════════════════════════
# LIVE MAIN — privileged on-node count-gated barrier restore (NOT exercised by CI)
# ════════════════════════════════════════════════════════════════════════════
# Wrapped in drc_main(), invoked ONLY by the positive BASH_SOURCE guard at the foot
# of the file. When the test SOURCES this script, drc_main is merely DEFINED (never
# called), so the pure core above is testable with zero live side effects. The
# positive-guard form (vs an early `... || return 0`) is also what keeps a sourcing
# `shellcheck -x` clean: an early top-level return makes shellcheck treat everything
# after the `source` in the TEST as unreachable (SC2317). Mirrors freeze-thaw-rungs.sh.
drc_main() {
AGENT="${AGENT:-}"
BUNDLE_OVR=""; IMG_OVR=""; OVER="${OVER:-/var/lib/gardenio/overlay}"
CWD_OVR=""; KEEP=0
ROOT="${ROOT:-/run/containerd/runsc/k8s.io}"
RUNSC_TIMEOUT="${RUNSC_TIMEOUT:-120}"

while [ $# -gt 0 ]; do
  case "$1" in
    --agent)   AGENT="$2"; shift 2 ;;
    --bundle)  BUNDLE_OVR="$2"; shift 2 ;;
    --img)     IMG_OVR="$2"; shift 2 ;;
    --overlay) OVER="$2"; shift 2 ;;
    --cwd)     CWD_OVR="$2"; shift 2 ;;
    --root)    ROOT="$2"; shift 2 ;;
    --keep)    KEEP=1; shift ;;
    -h|--help) sed -n '2,80p' "$0"; exit 0 ;;
    *) echo "unknown flag: $1" >&2; exit 2 ;;
  esac
done

[ -z "$AGENT" ] && { echo "SETUP-ERROR: --agent <name> (or \$AGENT) is required" >&2; exit 2; }

BUNDLE="${BUNDLE_OVR:-/var/lib/gardenio/bundle/$AGENT}"
IMG="${IMG_OVR:-/var/lib/gardenio/ckpt/$AGENT}"

say() { echo "[discriminating-check] $*"; }
fail_setup() { echo "SETUP-ERROR: $*" >&2; exit 2; }

# HOST-NAMESPACE WRAPPERS (ka-0000002536 invariant; PoC parity). The probe pod runs
# in its OWN mount/pid/net namespaces, so a bare `runsc restore` forks its gofer in the
# probe's restricted ns and EPERMs ('cannot create gofer process: fork/exec
# /proc/self/exe: operation not permitted'), and a bare `mount` lands in the probe's ns
# where runsc's nsenter'd gofer can't see it. The controller's thaw + every proven PoC
# harness (single-/multi-container-resume-node.sh) therefore run runsc under
# `nsenter -t 1 -m -p -n` (host mount+pid+net) and mounts under `nsenter -t 1 -m`
# (HMNT). We do the SAME so this gate exercises the EXACT restore path prod uses. If
# PID 1 is not the host init (e.g. run outside a hostPID probe), nsenter is a harmless
# no-op shim that still finds runsc on PATH.
RUNSC() { timeout "$RUNSC_TIMEOUT" nsenter -t 1 -m -p -n -- runsc --root="$ROOT" "$@"; }
HMNT()  { nsenter -t 1 -m -- "$@"; }

command -v runsc >/dev/null 2>&1 || fail_setup "runsc not on PATH — run inside the probe pod via 'chroot /host'."
[ -d "$BUNDLE" ]            || fail_setup "bundle dir absent: $BUNDLE (reaped? pass --bundle, or re-drive ONE freeze to regenerate)."
[ -s "$IMG/checkpoint.img" ] || fail_setup "checkpoint image absent/empty: $IMG/checkpoint.img (reaped? pass --img, or re-drive ONE freeze)."

# Resolve the checkpointed CWD exactly as the controller does (process.cwd from the
# agent sub-container config.json), so we assert the SAME path runsc restore 9p-walks.
CWD="$CWD_OVR"
if [ -z "$CWD" ]; then
  CWD=$(python3 -c 'import json,sys
try: print(json.load(open(sys.argv[1])).get("process",{}).get("cwd",""))
except Exception: print("")' "$BUNDLE/agent/config.json" 2>/dev/null)
fi
[ -z "$CWD" ] && CWD="/home/agent/workspace"
say "agent=$AGENT  bundle=$BUNDLE  img=$IMG  overlay=$OVER  cwd=$CWD"

# ── Isolate from prod: restore into a SCRATCH cid against COPIES, never the live state ──
WORK="/var/tmp/gb-discriminating-$AGENT-$$"
SCRATCH_BUNDLE="$WORK/bundle"
SCRATCH_OVER="$WORK/over"
# Two scratch cids — one for the sandbox root, one for the agent sub — so the barrier
# restore (sandbox-first, then agent) runs entirely against scratch ids and never
# touches the live pod's cids. SBCID is the parent the agent sub joins.
RCID="discrim-${AGENT}-$$"
SBCID="discrim-sb-${AGENT}-$$"
mkdir -p "$SCRATCH_OVER/agent/upper" "$SCRATCH_OVER/agent/work" \
         "$SCRATCH_OVER/sandbox/upper" "$SCRATCH_OVER/sandbox/work"

# Cleanup on EXIT. Installed HERE — before the bundle copy / sandbox-id repoint /
# barrier — so EVERY exit path (including the SETUP-ERROR assertions below) tears down
# the scratch cids + /var/tmp work and can never leak an orphan. Inlined as a trap
# command string (no helper function) so the linter does not flag the body as
# unreachable (SC2317) — the prior helper-fn form needed a forbidden inline lint-
# suppression to silence that false positive, which quench's escapes ratchet rejects.
# --keep skips teardown; otherwise delete both scratch cids, unmount the scratch
# rootfs overlays, and remove the working dir.
# Pre-declare the leaked-cid/netns audit vars BEFORE the trap string. They are
# assigned INSIDE the single-quoted trap body, which shellcheck does not scope into,
# so without this it false-positives SC2154 ("referenced but not assigned"). An inline
# `# shellcheck disable` is rejected by quench's escapes ratchet; pre-declaring is the
# clean fix that lets discriminating-restore-check.sh enter a `shellcheck -x` CI gate.
leaked=""; leakns=""
trap '
  if [ "$KEEP" = 1 ]; then
    say "--keep: leaving $WORK, scratch cids $RCID (agent) + $SBCID (sandbox), netns gb-drc-$$"
  else
    RUNSC delete --force "$RCID" >/dev/null 2>&1 || true
    RUNSC delete --force "$SBCID" >/dev/null 2>&1 || true
    # On a PASS the detached sentry is still releasing the rootfs mount + netns when
    # `delete --force` returns, so a bare umount/netns-delete/rm races it and leaks.
    # Settle briefly, then LAZY-umount (detaches even if the sentry still holds an fd),
    # delete the netns, and rm. The leaked self-audit below still reports any survivor.
    sleep 1
    HMNT umount -l "$SCRATCH_BUNDLE/sandbox/rootfs" 2>/dev/null || true
    HMNT umount -l "$SCRATCH_BUNDLE/agent/rootfs" 2>/dev/null || true
    HMNT ip netns delete "gb-drc-$$" 2>/dev/null || true
    rm -rf "$WORK" 2>/dev/null || true
    # Leaked-cid self-audit: after teardown, any scratch cid for THIS run ($$) that
    # still appears in runsc list is an orphan — make it VISIBLE (LEAKED) rather than
    # silently leave a half-restored sentry on the node. Scoped to this PID so a
    # concurrent run is never flagged.
    leaked=$(RUNSC list -q 2>/dev/null | grep -E "discrim-(sb-)?.*-$$\$" || true)
    [ -n "$leaked" ] && say "LEAKED: scratch cid(s) survived teardown: $leaked (runsc --root=$ROOT delete --force <cid> to remove)"
    leakns=$(HMNT ip netns list 2>/dev/null | grep -E "gb-drc-$$" || true)
    [ -n "$leakns" ] && say "LEAKED: scratch netns survived teardown: $leakns (ip netns delete gb-drc-$$ to remove)"
  fi
' EXIT

# COPY the bundle (config.json + rootfs mountpoint + .overlay-opts) so the live one is
# untouched. The persisted bundle carries BOTH sandbox/ and agent/ subdirs — the copy
# preserves both so the barrier can restore each.
cp -a "$BUNDLE" "$SCRATCH_BUNDLE"

# REPOINT the original sandbox cid -> SBCID across BOTH scratch configs (mirrors
# thawTmpl [3]'s sandbox-id annotation repoint). The persisted agent/config.json's
# io.kubernetes.cri.sandbox-id names the ORIGINAL live sandbox cid; restoring the
# agent sub against that makes runsc try to JOIN the original sandbox whose .state
# was never written under our $ROOT -> rc=128 'cannot load sandbox: ... .state: no
# such file or directory'. Substring-replace the original sandbox cid with SBCID in
# every annotation value of both the agent AND sandbox configs (same form as
# thawTmpl's `asb in v` rewrite) so the scratch agent joins the scratch sandbox.
# stderr is NOT swallowed (any traceback must reach the operator output) — and the
# rc is asserted below, so a silent repoint failure can never be misread as a Fix-B
# data-loss verdict at the barrier stage.
python3 - "$SCRATCH_BUNDLE" "$SBCID" <<'PYSBID'
import json,os,sys
sb,sbcid=sys.argv[1],sys.argv[2]
# Anchor: the original sandbox cid, read from the sandbox config's own sandbox-id
# annotation (falls back to the agent config's if the sandbox lacks it).
def annval(cfg,key):
    try: return json.load(open(cfg)).get("annotations",{}).get(key,"")
    except Exception: return ""
asb=annval(os.path.join(sb,"sandbox","config.json"),"io.kubernetes.cri.sandbox-id") \
    or annval(os.path.join(sb,"agent","config.json"),"io.kubernetes.cri.sandbox-id")
for dstn in ("sandbox","agent"):
    p=os.path.join(sb,dstn,"config.json")
    if not os.path.exists(p): continue
    c=json.load(open(p))
    # setdefault (NOT .get) so the dict is ATTACHED to c — otherwise the agent
    # force-set below would mutate a detached copy and never persist for an
    # annotation-sparse legacy bundle (the exact case the fallback claims to cover).
    ann=c.setdefault("annotations",{})
    for k,v in list(ann.items()):
        if asb and asb in v: ann[k]=v.replace(asb,sbcid)
    # Belt-and-suspenders: ensure the agent's sandbox-id is exactly SBCID even if the
    # original anchor could not be resolved (e.g. an annotation-sparse legacy bundle).
    if dstn=="agent": ann["io.kubernetes.cri.sandbox-id"]=sbcid
    json.dump(c,open(p,"w"))
PYSBID
# ASSERT the repoint actually landed on the scratch AGENT config. If it did not
# (Python aborted, schema surprise, write failure), a later barrier rc=128 would be a
# SCRIPT-TOPOLOGY/SETUP artifact — the header explicitly says that is NOT a Fix-B
# verdict — so fail as SETUP-ERROR (exit 2) HERE rather than let it surface downstream
# as a misleading "loss may be REAL" HALT.
GOT_SBID=$(python3 -c 'import json,sys
try: print(json.load(open(sys.argv[1])).get("annotations",{}).get("io.kubernetes.cri.sandbox-id",""))
except Exception: print("")' "$SCRATCH_BUNDLE/agent/config.json" 2>/dev/null)
[ "$GOT_SBID" = "$SBCID" ] || fail_setup "sandbox-id repoint did not apply to the scratch agent config (got '$GOT_SBID', want '$SBCID'). A subsequent restore rc=128 'cannot load sandbox' would be a topology/setup artifact, NOT a Fix-B verdict — fix the bundle/config and re-run."

# ── [scaffold] (ka-0000000357, defect-D) stand in for the GC'd Pod-A host mounts + netns ──
# WHY. The persisted bundle's configs reference kubelet/containerd HOST mount SOURCES
# (resolv.conf, /etc/hosts, hostname, /dev/shm, termination-log — under
# /var/lib/kubelet/pods/<uid>/… and …/sandboxes/<sb>/…) and a NETNS that were reaped
# when Pod A was torn down after the freeze. In the REAL thaw a kubelet-owned placeholder
# Pod B re-provides exactly these before runsc restore runs (thawTmpl). The standalone
# gate has no Pod B, so without this step the SANDBOX restore FATALs in setupNetwork /
# 'error setting up FS: opening …/resolv.conf: no such file or directory' BEFORE the agent
# sub ever 9p-walks the rootfs — a SCAFFOLD gap, not a Fix verdict, and the exact reason
# the operator found the harness non-functional. We materialize empty scratch stand-ins
# for every MISSING host-path mount source (a dir for tmpfs/dir mounts, a touch'd file for
# bind mounts) and a fresh scratch netns, then repoint both configs to them. We do NOT
# touch sources that still exist (none should, post-GC) and we change NO rootfs content,
# so the dir-medium rootfs the Fix is about is mounted exactly as thaw mounts it. All
# scaffold artifacts live under $WORK / a $$-scoped netns and are torn down by the EXIT
# trap (extended below).
SCAFFOLD_NETNS="gb-drc-$$"
HMNT ip netns add "$SCAFFOLD_NETNS" 2>/dev/null || true
# Bring lo up so the restored sentry's netstack has a usable loopback (the [6a]-class
# non-lo/default-route content the controller asserts is NOT checked by this offline
# gate — we only need the restore to not FATAL in setupNetwork).
HMNT ip -n "$SCAFFOLD_NETNS" link set lo up 2>/dev/null || true
SCAFFOLD_NETNS_PATH="/var/run/netns/$SCAFFOLD_NETNS"
python3 - "$SCRATCH_BUNDLE" "$SCAFFOLD_NETNS_PATH" "$WORK/scaffold" <<'PYSCAF'
import json,os,sys
sb,netns_path,scafroot=sys.argv[1],sys.argv[2],sys.argv[3]
os.makedirs(scafroot,exist_ok=True)
made=[]
def standin(src,typ):
    # Map a GC'd host source to an empty scratch stand-in under scafroot, preserving a
    # unique-ish basename so two sources never collide. dir/tmpfs -> dir; bind -> file
    # (unless the original looks like a dir mount). Idempotent.
    rel=src.strip("/").replace("/","_")
    dst=os.path.join(scafroot,rel)
    if typ in ("tmpfs","cgroup") or src.rstrip("/").endswith(("shm","mqueue")):
        os.makedirs(dst,exist_ok=True)
    else:
        os.makedirs(os.path.dirname(dst),exist_ok=True)
        if not os.path.exists(dst):
            open(dst,"w").close()
    return dst
for dstn in ("sandbox","agent"):
    p=os.path.join(sb,dstn,"config.json")
    if not os.path.exists(p): continue
    c=json.load(open(p))
    # Repoint the network namespace to the scratch netns.
    for n in c.get("linux",{}).get("namespaces",[]):
        if n.get("type")=="network":
            n["path"]=netns_path
    # Repoint every MISSING host-path mount source to an empty scratch stand-in.
    for m in c.get("mounts",[]):
        src=m.get("source",""); typ=m.get("type","")
        if not src.startswith("/"): continue
        if any(src.startswith(x) for x in ("/proc","/sys","/dev")) and typ not in ("bind",): continue
        if os.path.exists(src): continue
        new=standin(src,typ); m["source"]=new; made.append((dstn,src,new))
    json.dump(c,open(p,"w"))
print("SCAFFOLD_MADE=%d" % len(made))
for d,s,n in made: print("  %s: %s -> %s" % (d,s,n))
PYSCAF
say "[scaffold] materialized scratch stand-ins for GC'd Pod-A host mounts + netns $SCAFFOLD_NETNS_PATH"

# Reconstruct the rootfs overlay the SAME way remount_rootfs does — lowerdir from
# .overlay-opts + a FRESH empty upper/work — but WITHOUT the controller's pre-restore
# CWD assertion. This is the "assertion bypassed" mount the steer asks for. The
# .overlay-opts parser (build_lowers, the DEFECT-A fix) is defined ONCE in the pure
# sourceable core at the top of this file and is exercised hermetically by the CI gate.
remount() { # $1=dstn (agent|sandbox)
  local dstn="$1" opts lowers up wk d
  opts=$(cat "$SCRATCH_BUNDLE/$dstn/.overlay-opts" 2>/dev/null)
  # A dir-medium bundle ALWAYS carries .overlay-opts for every dstn (freeze writes it
  # per DSTN_LIST). An empty/absent one is a setup/bundle defect, never a Fix verdict.
  [ -z "$opts" ] && fail_setup "no .overlay-opts for $dstn in the scratch bundle (a dir-medium bundle must carry it) — fix the bundle/freeze and re-run; an unmounted rootfs is never a Fix verdict."
  lowers=$(build_lowers "$opts")
  [ -z "$lowers" ] && fail_setup "could not parse a lowerdir for $dstn from .overlay-opts='$opts' — fix the bundle and re-run."
  # Assert EVERY lower segment exists before mounting (thaw [2] parity) so a GC'd /
  # mis-preserved segment is named HERE as a setup error, not surfaced 2 steps later as
  # an opaque restore rc=128 that would be misread as a Fix-B data-loss verdict.
  local IFS=':'
  for d in $lowers; do
    HMNT test -d "$d" || fail_setup "$dstn overlay lower segment missing: $d (GC'd / not preserved). An unmounted rootfs is never a Fix verdict — fix the bundle and re-run."
  done
  unset IFS
  up="$SCRATCH_OVER/$dstn/upper"; wk="$SCRATCH_OVER/$dstn/work"
  HMNT umount "$SCRATCH_BUNDLE/$dstn/rootfs" 2>/dev/null || true
  # DEFECT-A: mount failure is now FATAL (exit 2 SETUP-ERROR). The prior `return 0`
  # left the rootfs UNMOUNTED so the subsequent runsc restore 9p-walked a garbage /
  # empty rootfs and rc=128'd — a SCRIPT artifact misreadable as a real data-loss
  # verdict. An unmounted reconstructed rootfs can never settle the Fix question.
  # The mount MUST land in host-init's mount ns (HMNT) — runsc's nsenter'd gofer reads
  # the rootfs from THERE, and a mount in the probe's own ns is invisible to it
  # (single-container-resume-node.sh:20-21).
  if ! HMNT mount -t overlay overlay "$SCRATCH_BUNDLE/$dstn/rootfs" \
        -o "lowerdir=$lowers,upperdir=$up,workdir=$wk,index=off" 2>"$WORK/$dstn.mnterr"; then
    say "$dstn overlay mount FAILED (lowerdir=$lowers): $(cat "$WORK/$dstn.mnterr" 2>/dev/null)"
    fail_setup "$dstn rootfs overlay mount failed — the reconstructed rootfs is unmounted, so a restore rc here would be a script artifact, not a Fix verdict. Fix the bundle/overlay and re-run."
  fi
}
remount sandbox; remount agent

# ── COUNT-GATED N-CONTAINER BARRIER restore (sandbox-first, then agent sub) ──
# Mirrors thawTmpl [5] / the PoC two-phase sequence. PHASE 1: background the SANDBOX
# root restore — with --detach it does NOT return until the baked container_count is
# satisfied (it parks at restoringUnstarted). PHASE 2: restore the AGENT sub; its
# sandbox-id annotation (repointed to SBCID above) makes it join the scratch sandbox,
# satisfies the count, and unblocks BOTH. We collect the sandbox rc via `wait`. The
# barrier target is read by runsc from checkpoint.img — there is NO --container-count
# CLI flag in this gVisor build, so none is passed (parity with thawTmpl + the PoC).
#
# DEFECT-B (ka-0000000357): the prior PHASE-1 poll waited only for `runsc state` to
# SUCCEED (i.e. the cid is REGISTERED) — but registration happens BEFORE the sandbox
# parks at the count barrier. If PHASE-2 joined in that window, the agent restore
# raced the sandbox and could rc=128 'cannot load sandbox' — a TOPOLOGY artifact, not a
# Fix verdict. Fix: poll for the sandbox to actually reach the PARK state
# (restoring-unstarted, or created) before PHASE-2 joins. We KEEP --detach on the agent
# sub (PHASE-2): a sub-container restore WITHOUT --detach attaches to the agent's
# long-running init and blocks until it EXITS — under `timeout` that returns rc=124 (a
# hang), never the real restore rc. The REAL thaw (thawTmpl L1459) uses --detach on the
# agent sub and THAT is exactly the call whose rc surfaced the cycle-14 'sub=128'
# discriminating signal — so --detach here is what makes the gate predict prod. PHASE-2
# runs in the FOREGROUND (not backgrounded) so RC=$? is the genuine agent restore rc.
say "[restore] count-gated barrier — sandbox-first (scratch cid=$SBCID), then agent sub (scratch cid=$RCID); controller bash assertion is structurally bypassed"
ROOT_OUT="$WORK/restore-sandbox.out"
RESTORE_OUT="$WORK/restore-agent.out"

# PHASE 1 — sandbox root, backgrounded (blocks at the barrier until the count is met).
# RUNSC bakes in `timeout … nsenter -t 1 -m -p -n -- runsc --root=$ROOT`; --overlay2 is
# a runsc GLOBAL flag so it precedes `restore` (ka-0000000621).
( RUNSC --overlay2="root:dir=$OVER" \
    restore --image-path="$IMG" --bundle "$SCRATCH_BUNDLE/sandbox" --detach "$SBCID" \
    >"$ROOT_OUT" 2>&1; echo "SBRC=$?" >>"$ROOT_OUT" ) &
SBPID=$!
# Wait for the sandbox to reach the count-barrier PARK state (restoring-unstarted /
# created), NOT mere registration, before the agent sub joins (DEFECT-B). Accept either
# spelling runsc may report (restoring-unstarted, restoringUnstarted, created). If the
# sandbox restore has already FAILED (the backgrounded subshell exited), stop polling —
# the rc is collected below and surfaced as FAIL, not masked by a 10s spin.
SB_PARKED=no
for _ in $(seq 1 40); do
  if ! kill -0 "$SBPID" 2>/dev/null; then break; fi   # sandbox restore already returned (likely failed)
  st=$(RUNSC state "$SBCID" 2>/dev/null \
       | python3 -c 'import sys,json
try: print(json.load(sys.stdin).get("status",""))
except Exception: print("")' 2>/dev/null)
  case "$st" in
    restoring-unstarted|restoringUnstarted|created|paused) SB_PARKED=yes; break ;;
  esac
  sleep 0.5
done
say "[restore] sandbox park state before agent join: parked=$SB_PARKED (status='${st:-}')"

# PHASE 2 — agent sub, FOREGROUND + --detach (real restore rc; see DEFECT-B note).
# Joining it makes len==container_count, firing the real restore so BOTH the sandbox
# boot and this sub call return.
( RUNSC --overlay2="root:dir=$OVER" \
    restore --image-path="$IMG" --bundle "$SCRATCH_BUNDLE/agent" --detach "$RCID" \
    >"$RESTORE_OUT" 2>&1 )
RC=$?
# Collect the backgrounded sandbox restore rc now that the barrier is satisfied.
wait "$SBPID" 2>/dev/null
SBRC=$(sed -n 's/^SBRC=//p' "$ROOT_OUT" | tail -1); [ -z "$SBRC" ] && SBRC=1
say "[restore] sandbox_rc=$SBRC  agent_rc=$RC"
[ -s "$ROOT_OUT" ]    && sed 's/^/    runsc(sandbox): /' "$ROOT_OUT"
[ -s "$RESTORE_OUT" ] && sed 's/^/    runsc(agent): /'   "$RESTORE_OUT"

if [ "$RC" -ne 0 ] || [ "$SBRC" -ne 0 ]; then
  # WALK-ENOENT signature: 'failed to walk "<X>" of type <T> in mount "<m>:/": no such
  # file or directory' is the dir-medium sparse-lower rc=128 (the cycle-6 / -14 signature
  # this gate is built to reproduce). <T> is 040000 for a missing DIRECTORY dentry (the
  # cycle-14 usr walk) OR 0100000 for a missing FILE leaf (e.g. usr/bin/sleep — what a
  # skeleton-ONLY lower fails on, the empirical proof skeleton-only is hollow/insufficient).
  # Match BOTH so a file-leaf miss is correctly classed as the sparse-lower FAIL, not
  # "no signature". Name the missing dentry <X> — the next thing the freeze fix must seed.
  WALKMISS=$(grep -hoE 'failed to walk "[^"]+" of type [0-9]+' "$ROOT_OUT" "$RESTORE_OUT" 2>/dev/null | head -1)
  # Classify the restore-failure rung via the pure ladder (the in-sentry args are unread
  # on a restore failure, so pass 0s). Both runsc outputs are already dumped inline above.
  drc_verdict "$RC" "$SBRC" "$WALKMISS" "$CWD" 0 0 0 0
  exit $?
fi

# ── ASSERT-1: CWD liveness (FT_DIRMEDIUM_CWD_LIVE parity) ───────────────────────────
# The checkpointed CWD must resolve AND carry content INSIDE the restored sentry (the
# only place the rebuilt upper is observable). entries>0 distinguishes a present-but-
# EMPTY dir (still a loss / hollow) from a populated workspace.
say "[assert-1] CWD liveness: runsc exec test -e '$CWD' + entry count inside restored sentry"
RUNSC exec "$RCID" test -e "$CWD" >/dev/null 2>&1; CWDRC=$?
NENTRIES=$(RUNSC exec "$RCID" \
             sh -c "ls -A1 '$CWD' 2>/dev/null | wc -l" 2>/dev/null | tr -dc '0-9')
NENTRIES="${NENTRIES:-0}"
say "[assert-1] cwd_test_rc=$CWDRC  entries_under_cwd=$NENTRIES"

# ── ASSERT-2: ROOTFS_NOT_HOLLOW (DEFECT-C, ka-0000000357) ───────────────────────────
# The cycle-14 blind spot: an empty-dir skeleton (and the thaw L1162 cwd mkdir) make
# the DIRECTORY walk pass and CWD exist, but leave /usr HOLLOW — the FILE leaf
# /usr/bin/bash and post-resume execve(coop/gb/bash) 9p-walk base binaries from the
# lower and ENOENT. CWD-only liveness is structurally BLIND to that. So force a fresh
# 9p-walk of a base-image BINARY (command -v bash — proves usr/bin populated + the
# bin->usr/bin symlink resolves) AND an open+read of a base-image FILE (head -c1 of an
# os-release; falls back to /usr/lib/os-release) — proving real base CONTENT round-
# tripped, not just the directory skeleton. The exec must return rc=0 AND non-empty
# stdout. rc=0 with EMPTY stdout is the HOLLOW catch (dir present, content absent).
say "[assert-2] ROOTFS_NOT_HOLLOW: fresh 9p-walk of a base binary + read of a base file inside the restored sentry"
HOLLOW_OUT=$(RUNSC exec "$RCID" /bin/sh -c \
  'command -v bash >/dev/null 2>&1 || exit 91; { head -c1 /etc/os-release 2>/dev/null || head -c1 /usr/lib/os-release 2>/dev/null; } ' 2>/dev/null); HOLLOWRC=$?
# Normalize: did we get a non-empty byte from a base file?
HOLLOW_BYTES=$(printf '%s' "$HOLLOW_OUT" | wc -c | tr -dc '0-9'); HOLLOW_BYTES="${HOLLOW_BYTES:-0}"
say "[assert-2] base_exec_rc=$HOLLOWRC  base_file_bytes=$HOLLOW_BYTES"

# ── RESULT LADDER ───────────────────────────────────────────────────────────────────
# The restore succeeded (rc==0 above), so the measured in-sentry assertions decide the
# verdict. Classification + the RESULT=/VERDICT block + exit code live in drc_verdict
# (the pure, CI-tested ladder at the top of this file); pass RC=SBRC=0 and an empty
# WALKMISS since this branch is only reached on a clean restore.
drc_verdict 0 0 "" "$CWD" "$CWDRC" "$NENTRIES" "$HOLLOWRC" "$HOLLOW_BYTES"
exit $?
}

# Run the live on-node main ONLY on direct execution. When SOURCED (the hermetic test
# discriminating-restore-check_test.sh), drc_main is merely defined — the pure core
# (build_lowers + drc_verdict) is exercised with zero live side effects.
if [ "${BASH_SOURCE[0]}" = "${0}" ]; then
  drc_main "$@"
fi
