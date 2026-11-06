#!/usr/bin/env bash
# sentry-egress-check.sh — zero-canary on-node proof that a restored gVisor SENTRY
# netstack is ROUTABLE and can EGRESS, BEFORE the scarce operator-authorized canary
# is re-driven (bug ka-0000000514, task ka-0000008207). The NO-GO tripwire for the
# restored-SENTRY-link-only frontier that b714bf11b did NOT close.
#
# WHY THIS GATE EXISTS
# --------------------
# On prod 2026.180.13 a thaw "restore verified" rc=0 but BOTH rehydrate consumers
# (coop NATS reconnect, gb heartbeat HTTP POST) failed "network is unreachable" for
# the full 180s deadline -> "rehydrate: not released; failed consumers: coop, gb" ->
# fail-close. The restored SENTRY netstack came up LINK-ONLY. The controller's [6a]
# netns-content check went GREEN because it inspects the HOST-KERNEL netns view
# (nsenter --net=/proc/<RPID>/ns/net) — but runsc restore's setupNetwork() SCRAPES
# the netns into a FRESH sentry netstack DURING the restore RPC and discards the
# checkpoint's, so the SENTRY can be link-only even when the host's view of that
# inode is routable. The host-side check is structurally BLIND to it.
#
# WHAT THIS CHECKS (the SENTRY's OWN view, via runsc exec — NOT the host kernel's)
# -------------------------------------------------------------------------------
# After a count-gated barrier restore into SCRATCH cids (never the live pod), this
# `runsc exec`s INTO the restored sandbox and asserts, from inside the sentry:
#   (1) the sentry netstack carries a DEFAULT ROUTE (a 00000000 dest in its own
#       /proc/net/route) and >=1 non-loopback interface — the DIRECT link-only
#       detector, DNS- and service-independent; AND
#   (2) (when --nats / --beads targets are given) a TCP dial to those endpoints —
#       the SAME ones coop + gb reach — succeeds, OR fails with a NON-network errno
#       (refused/timeout/DNS). A dial that returns ENETUNREACH/EHOSTUNREACH is the
#       prod-.13 link-only signature and FAILs.
#
# This is the EXACT logic the controller thaw [6c] runs (freezethaw_node.go), so a
# PASS here predicts a thaw that releases the rehydrate-ack gate, and a FAIL here
# reproduces the canary fail OFFLINE — without burning a 5th canary.
#
# VERDICT LADDER (seg_verdict, the pure CI-tested core):
#   PASS          sentry has a default route + non-lo iface, AND every configured
#                 dial is routable (ok, or refused/timeout — the netstack routed it).
#   FAIL          sentry has NO default route / no non-lo iface (link-only), OR a
#                 dial returned network-unreachable. The prod-.13 bug — do NOT canary.
#   INCONCLUSIVE  an in-sentry exec flaked (sentry not joinable / 'python3' missing /
#                 timeout). NEVER a PASS.
#
# Usage (on-node, inside the privileged probe via `chroot /host`):
#   sentry-egress-check.sh --agent <name> [--nats host:4222] [--beads host:8080] \
#       [--bundle DIR] [--img DIR] [--overlay root:dir=DIR] [--root DIR] [--keep]
#
# Fully offline half: the seg_verdict ladder is exercised hermetically by
# sentry-egress-check_test.sh (checks.yml script-selftests, cluster-free). The LIVE half
# (the count-gated barrier restore that PRODUCES these measurements) still requires
# the on-node operator run.

set -uo pipefail

# ════════════════════════════════════════════════════════════════════════════
# PURE SOURCEABLE CORE — exercised hermetically by sentry-egress-check_test.sh
# ════════════════════════════════════════════════════════════════════════════

# seg_dial_verdict maps ONE in-sentry dial outcome to a class. Mirrors the
# controller's sentry_dial python (freezethaw_node.go [6c-2]) byte-for-byte in
# intent: a connect that SUCCEEDS or fails with a NON-network errno is "routable"
# (the netstack delivered the SYN — a refused/timeout/DNS error is a service/name
# problem, never a link-only verdict); only ENETUNREACH/EHOSTUNREACH/ENETDOWN is
# "link-only". "skip" means no target was configured. Args: $1 = one of
# ok|refused|timeout|dns|netunreach|skip (the raw probe outcome).
seg_dial_verdict() { # $1=raw outcome -> echoes routable|link-only|skip
  case "$1" in
    ok|refused|timeout|dns|other) echo routable ;;
    netunreach|hostunreach|netdown) echo link-only ;;
    skip|"") echo skip ;;
    *) echo skip ;;
  esac
}

# seg_verdict — the verdict ladder, pure (positional args, touches no globals).
# Args:
#   $1 DEFROUTE   sentry default route: yes|no|unknown (unknown == exec flaked)
#   $2 NONLO      sentry non-loopback iface count (integer; -1 == exec flaked)
#   $3 NATS_RAW   raw NATS dial outcome (ok|refused|timeout|dns|netunreach|skip)
#   $4 BEADS_RAW  raw beads dial outcome (same vocabulary)
# Prints RESULT=/VERDICT block on stdout; returns 0 ONLY for PASS, 1 for FAIL,
# 2 for INCONCLUSIVE (a flaked in-sentry exec — never a PASS, but distinct from a
# real link-only FAIL so a redrive is suggested rather than a NO-GO declared).
seg_verdict() {
  local DEFROUTE="$1" NONLO="$2" NATS_RAW="$3" BEADS_RAW="$4"
  local nats beads
  nats=$(seg_dial_verdict "$NATS_RAW")
  beads=$(seg_dial_verdict "$BEADS_RAW")

  # INCONCLUSIVE: the in-sentry route read flaked (could not join the sentry / no
  # python3 / timeout). Never PASS — but not the link-only FAIL either.
  if [ "$DEFROUTE" = unknown ] || [ "$NONLO" = "-1" ]; then
    echo "RESULT=INCONCLUSIVE  sentry_defroute=$DEFROUTE  sentry_nonlo=$NONLO  nats=$nats  beads=$beads"
    echo "VERDICT: INCONCLUSIVE — the in-sentry netstack read flaked (sentry not joinable / 'python3'"
    echo "  missing / runsc-exec timeout). NOT a PASS and NOT a link-only verdict. Re-run with a"
    echo "  longer RUNSC_TIMEOUT, or --keep and 'runsc exec <cid> cat /proc/net/route' manually."
    return 2
  fi

  # FAIL (link-only): no default route, OR no non-loopback iface, OR a dial that
  # NO-ROUTED (network-unreachable). This is the prod-.13 bug reproduced offline.
  if [ "$DEFROUTE" = no ] || [ "${NONLO:-0}" -lt 1 ] || [ "$nats" = link-only ] || [ "$beads" = link-only ]; then
    echo "RESULT=FAIL  sentry_defroute=$DEFROUTE  sentry_nonlo=$NONLO  nats=$nats  beads=$beads"
    echo "VERDICT: FAIL — the restored SENTRY netstack is LINK-ONLY (no default route / no non-lo iface,"
    echo "  or an in-sentry dial returned network-unreachable). This is the prod-.13 rehydrate-ack"
    echo "  fail (coop+gb 'network is unreachable' for 180s) reproduced OFFLINE. The fix did NOT make"
    echo "  the restore-target netns routable at setupNetwork() scrape-time — do NOT re-drive the"
    echo "  canary. Check FT_NETNS_PRERESTORE_ROUTABLE / the pre-restore reassert_l3+self_ipam ordering."
    return 1
  fi

  # PASS: sentry routable AND every configured dial routable (ok / refused / timeout —
  # the netstack delivered the SYN; an unconfigured target is "skip", which is fine).
  echo "RESULT=PASS  sentry_defroute=$DEFROUTE  sentry_nonlo=$NONLO  nats=$nats  beads=$beads"
  echo "VERDICT: PASS — the restored SENTRY netstack carries a default route + a non-loopback"
  echo "  interface, AND every configured egress dial (NATS/beads) is ROUTABLE from INSIDE the"
  echo "  sentry. The restore-target netns was routable when runsc setupNetwork() scraped it into"
  echo "  the sentry netstack — the fix holds OFFLINE. Safe to proceed to the operator-gated canary."
  return 0
}

# ════════════════════════════════════════════════════════════════════════════
# LIVE MAIN — privileged on-node restore + in-sentry probe (NOT exercised by CI)
# ════════════════════════════════════════════════════════════════════════════
# Wrapped in seg_main(), invoked ONLY by the positive BASH_SOURCE guard at the foot
# of the file, so the test can SOURCE the pure core with zero live side effects.
seg_main() {
AGENT="${AGENT:-}"
BUNDLE_OVR=""; IMG_OVR=""; OVER="${OVER:-/var/lib/gardenio/overlay}"
NATS_TGT=""; BEADS_TGT=""; KEEP=0
ROOT="${ROOT:-/run/containerd/runsc/k8s.io}"
RUNSC_TIMEOUT="${RUNSC_TIMEOUT:-120}"

while [ $# -gt 0 ]; do
  case "$1" in
    --agent)   AGENT="$2"; shift 2 ;;
    --bundle)  BUNDLE_OVR="$2"; shift 2 ;;
    --img)     IMG_OVR="$2"; shift 2 ;;
    --overlay) OVER="$2"; shift 2 ;;
    --nats)    NATS_TGT="$2"; shift 2 ;;
    --beads)   BEADS_TGT="$2"; shift 2 ;;
    --root)    ROOT="$2"; shift 2 ;;
    --keep)    KEEP=1; shift ;;
    -h|--help) sed -n '2,60p' "$0"; exit 0 ;;
    *) echo "unknown flag: $1" >&2; exit 2 ;;
  esac
done

[ -z "$AGENT" ] && { echo "SETUP-ERROR: --agent <name> (or \$AGENT) is required" >&2; exit 2; }

BUNDLE="${BUNDLE_OVR:-/var/lib/gardenio/bundle/$AGENT}"
IMG="${IMG_OVR:-/var/lib/gardenio/ckpt/$AGENT}"

say() { echo "[sentry-egress] $*"; }
fail_setup() { echo "SETUP-ERROR: $*" >&2; exit 2; }

# HOST-NAMESPACE WRAPPERS (ka-0000002536 invariant; PoC + discriminating-restore-check
# parity): runsc under `nsenter -t 1 -m -p -n` (host mount+pid+net) and mounts under
# `nsenter -t 1 -m` so this gate exercises the EXACT restore path prod uses.
RUNSC() { timeout "$RUNSC_TIMEOUT" nsenter -t 1 -m -p -n -- runsc --root="$ROOT" "$@"; }
HMNT()  { nsenter -t 1 -m -- "$@"; }

command -v runsc >/dev/null 2>&1 || fail_setup "runsc not on PATH — run inside the probe pod via 'chroot /host'."
[ -d "$BUNDLE" ]             || fail_setup "bundle dir absent: $BUNDLE (reaped? pass --bundle, or re-drive ONE freeze to regenerate)."
[ -s "$IMG/checkpoint.img" ] || fail_setup "checkpoint image absent/empty: $IMG/checkpoint.img (reaped? pass --img, or re-drive ONE freeze)."

say "agent=$AGENT  bundle=$BUNDLE  img=$IMG  overlay=$OVER  nats='${NATS_TGT:-<none>}'  beads='${BEADS_TGT:-<none>}'"

# ── Isolate from prod: restore into SCRATCH cids against COPIES, never the live state ──
# Build a routable scratch netns (eth0 + a /32 + default route via the synthetic AWS
# VPC CNI link-scope gateway) so a CORRECT fix restores into it routable — and so a
# DELIBERATELY-stripped netns reproduces the link-only FAIL. The scratch netns is the
# offline stand-in for Pod B's pinned CNI netns; the controller's reassert_l3/self_ipam
# populate the real one pre-restore, this gate populates the scratch one the same way.
WORK="/var/tmp/gb-sentry-egress-$AGENT-$$"
SCRATCH_BUNDLE="$WORK/bundle"
SCRATCH_OVER="$WORK/over"
RCID="segchk-${AGENT}-$$"
SBCID="segchk-sb-${AGENT}-$$"
SCAFFOLD_NETNS="gb-seg-$$"
SCAFFOLD_NETNS_PATH="/var/run/netns/$SCAFFOLD_NETNS"
mkdir -p "$SCRATCH_OVER/agent/upper" "$SCRATCH_OVER/agent/work" \
         "$SCRATCH_OVER/sandbox/upper" "$SCRATCH_OVER/sandbox/work"

leaked=""; leakns=""
trap '
  if [ "$KEEP" = 1 ]; then
    say "--keep: leaving $WORK, scratch cids $RCID + $SBCID, netns $SCAFFOLD_NETNS"
  else
    RUNSC delete --force "$RCID"  >/dev/null 2>&1 || true
    RUNSC delete --force "$SBCID" >/dev/null 2>&1 || true
    sleep 1
    HMNT umount -l "$SCRATCH_BUNDLE/sandbox/rootfs" 2>/dev/null || true
    HMNT umount -l "$SCRATCH_BUNDLE/agent/rootfs"   2>/dev/null || true
    HMNT ip netns delete "$SCAFFOLD_NETNS" 2>/dev/null || true
    rm -rf "$WORK" 2>/dev/null || true
    leaked=$(RUNSC list -q 2>/dev/null | grep -E "segchk-(sb-)?.*-$$\$" || true)
    [ -n "$leaked" ] && say "LEAKED: scratch cid(s) survived teardown: $leaked"
    leakns=$(HMNT ip netns list 2>/dev/null | grep -E "gb-seg-$$" || true)
    [ -n "$leakns" ] && say "LEAKED: scratch netns survived teardown: $leakns"
  fi
' EXIT

cp -a "$BUNDLE" "$SCRATCH_BUNDLE"

# Repoint the original sandbox cid -> SBCID across BOTH scratch configs (parity with
# discriminating-restore-check.sh / thawTmpl [3]).
python3 - "$SCRATCH_BUNDLE" "$SBCID" <<'PYSBID'
import json,os,sys
sb,sbcid=sys.argv[1],sys.argv[2]
def annval(cfg,key):
    try: return json.load(open(cfg)).get("annotations",{}).get(key,"")
    except Exception: return ""
asb=annval(os.path.join(sb,"sandbox","config.json"),"io.kubernetes.cri.sandbox-id") \
    or annval(os.path.join(sb,"agent","config.json"),"io.kubernetes.cri.sandbox-id")
for dstn in ("sandbox","agent"):
    p=os.path.join(sb,dstn,"config.json")
    if not os.path.exists(p): continue
    c=json.load(open(p))
    ann=c.setdefault("annotations",{})
    for k,v in list(ann.items()):
        if asb and asb in v: ann[k]=v.replace(asb,sbcid)
    if dstn=="agent": ann["io.kubernetes.cri.sandbox-id"]=sbcid
    json.dump(c,open(p,"w"))
PYSBID
GOT_SBID=$(python3 -c 'import json,sys
try: print(json.load(open(sys.argv[1])).get("annotations",{}).get("io.kubernetes.cri.sandbox-id",""))
except Exception: print("")' "$SCRATCH_BUNDLE/agent/config.json" 2>/dev/null)
[ "$GOT_SBID" = "$SBCID" ] || fail_setup "sandbox-id repoint did not apply to the scratch agent config (got '$GOT_SBID', want '$SBCID')."

# ── Build a ROUTABLE scratch netns (the offline stand-in for Pod B's pinned CNI netns) ──
# A correct fix scrapes THIS into the sentry. We give it a veth (eth0), a /32, the
# synthetic link-scope gateway route, and a default via it — exactly what self_ipam
# installs in the controller. A peer end lives in the host ns so the netstack has a
# real L2 partner (the dial half then exercises true egress when targets are given).
HMNT ip netns add "$SCAFFOLD_NETNS" 2>/dev/null || true
HMNT ip -n "$SCAFFOLD_NETNS" link set lo up 2>/dev/null || true
HMNT ip link add "segh$$" type veth peer name "segp$$" 2>/dev/null || true
HMNT ip link set "segp$$" netns "$SCAFFOLD_NETNS" 2>/dev/null || true
HMNT ip -n "$SCAFFOLD_NETNS" link set "segp$$" name eth0 2>/dev/null || true
HMNT ip link set "segh$$" up 2>/dev/null || true
HMNT ip -n "$SCAFFOLD_NETNS" link set eth0 up 2>/dev/null || true
HMNT ip -n "$SCAFFOLD_NETNS" addr add 169.254.99.2/32 dev eth0 2>/dev/null || true
HMNT ip -n "$SCAFFOLD_NETNS" route add 169.254.1.1 dev eth0 scope link 2>/dev/null || true
HMNT ip -n "$SCAFFOLD_NETNS" route add default via 169.254.1.1 dev eth0 2>/dev/null || true

# Repoint every GC'd host-path mount source to a scratch stand-in + the netns to the
# scratch one (parity with discriminating-restore-check.sh scaffold).
python3 - "$SCRATCH_BUNDLE" "$SCAFFOLD_NETNS_PATH" "$WORK/scaffold" <<'PYSCAF'
import json,os,sys
sb,netns_path,scafroot=sys.argv[1],sys.argv[2],sys.argv[3]
os.makedirs(scafroot,exist_ok=True)
def standin(src,typ):
    rel=src.strip("/").replace("/","_"); dst=os.path.join(scafroot,rel)
    if typ in ("tmpfs","cgroup") or src.rstrip("/").endswith(("shm","mqueue")):
        os.makedirs(dst,exist_ok=True)
    else:
        os.makedirs(os.path.dirname(dst),exist_ok=True)
        if not os.path.exists(dst): open(dst,"w").close()
    return dst
for dstn in ("sandbox","agent"):
    p=os.path.join(sb,dstn,"config.json")
    if not os.path.exists(p): continue
    c=json.load(open(p))
    for n in c.get("linux",{}).get("namespaces",[]):
        if n.get("type")=="network": n["path"]=netns_path
    for m in c.get("mounts",[]):
        src=m.get("source",""); typ=m.get("type","")
        if not src.startswith("/"): continue
        if any(src.startswith(x) for x in ("/proc","/sys","/dev")) and typ not in ("bind",): continue
        if os.path.exists(src): continue
        m["source"]=standin(src,typ)
    json.dump(c,open(p,"w"))
PYSCAF
say "[scaffold] routable scratch netns $SCAFFOLD_NETNS_PATH (eth0 + /32 + default route) + host-mount stand-ins"

# Reconstruct each rootfs overlay (lowerdir from .overlay-opts + fresh upper/work).
remount() { # $1=dstn
  local dstn="$1" opts up wk
  opts=$(cat "$SCRATCH_BUNDLE/$dstn/.overlay-opts" 2>/dev/null)
  [ -z "$opts" ] && fail_setup "no .overlay-opts for $dstn in the scratch bundle."
  local lower upper lowers
  upper=$(printf '%s' "$opts" | tr ',' '\n' | sed -n 's/^upperdir=//p')
  lower=$(printf '%s' "$opts" | tr ',' '\n' | sed -n 's/^lowerdir=//p')
  if [ -n "$upper" ]; then lowers="${upper}:${lower}"; else lowers="$lower"; fi
  [ -z "$lowers" ] && fail_setup "could not parse a lowerdir for $dstn from .overlay-opts='$opts'."
  up="$SCRATCH_OVER/$dstn/upper"; wk="$SCRATCH_OVER/$dstn/work"
  HMNT umount "$SCRATCH_BUNDLE/$dstn/rootfs" 2>/dev/null || true
  HMNT mount -t overlay overlay "$SCRATCH_BUNDLE/$dstn/rootfs" \
        -o "lowerdir=$lowers,upperdir=$up,workdir=$wk,index=off" 2>"$WORK/$dstn.mnterr" \
    || fail_setup "$dstn rootfs overlay mount failed: $(cat "$WORK/$dstn.mnterr" 2>/dev/null)"
}
remount sandbox; remount agent

# ── COUNT-GATED N-CONTAINER BARRIER restore (sandbox-first, then agent sub) ──
say "[restore] count-gated barrier — sandbox (scratch=$SBCID) then agent sub (scratch=$RCID)"
ROOT_OUT="$WORK/restore-sandbox.out"; RESTORE_OUT="$WORK/restore-agent.out"
( RUNSC --overlay2="root:dir=$OVER" \
    restore --image-path="$IMG" --bundle "$SCRATCH_BUNDLE/sandbox" --detach "$SBCID" \
    >"$ROOT_OUT" 2>&1; echo "SBRC=$?" >>"$ROOT_OUT" ) &
SBPID=$!
for _ in $(seq 1 40); do
  if ! kill -0 "$SBPID" 2>/dev/null; then break; fi
  st=$(RUNSC state "$SBCID" 2>/dev/null \
       | python3 -c 'import sys,json
try: print(json.load(sys.stdin).get("status",""))
except Exception: print("")' 2>/dev/null)
  case "$st" in restoring-unstarted|restoringUnstarted|created|paused) break ;; esac
  sleep 0.5
done
( RUNSC --overlay2="root:dir=$OVER" \
    restore --image-path="$IMG" --bundle "$SCRATCH_BUNDLE/agent" --detach "$RCID" \
    >"$RESTORE_OUT" 2>&1 )
RC=$?
wait "$SBPID" 2>/dev/null
SBRC=$(sed -n 's/^SBRC=//p' "$ROOT_OUT" | tail -1); [ -z "$SBRC" ] && SBRC=1
say "[restore] sandbox_rc=$SBRC  agent_rc=$RC"
[ -s "$ROOT_OUT" ]    && sed 's/^/    runsc(sandbox): /' "$ROOT_OUT"
[ -s "$RESTORE_OUT" ] && sed 's/^/    runsc(agent): /'   "$RESTORE_OUT"
if [ "$RC" -ne 0 ] || [ "$SBRC" -ne 0 ]; then
  echo "RESULT=FAIL  restore_rc=$RC  sandbox_rc=$SBRC"
  echo "VERDICT: FAIL — the count-gated barrier restore did NOT complete (agent_rc=$RC sandbox_rc=$SBRC)."
  echo "  Egress cannot be asserted on a sandbox that did not restore. Inspect the runsc output above."
  exit 1
fi

# ── ASSERT: read the restored SENTRY's OWN netstack + dial the consumers' endpoints ──
# EXACTLY the controller [6c] probe (freezethaw_node.go), run here against the scratch
# sentry. python3 in the agent image reads the sentry's own /proc/net/{route,dev}.
say "[assert] reading restored SENTRY netstack via 'runsc exec' (its OWN view, not the host's)"
SENTRY_NET=$(RUNSC exec "$RCID" python3 -c '
defroute="no"; nonlo=0
try:
    for i,l in enumerate(open("/proc/net/route")):
        if i==0: continue
        f=l.split()
        if len(f)>1 and f[1]=="00000000": defroute="yes"
except Exception:
    defroute="unknown"
try:
    for i,l in enumerate(open("/proc/net/dev")):
        if i<2: continue
        n=l.split(":")[0].strip()
        if n and n!="lo": nonlo+=1
except Exception:
    nonlo=-1
print("%s %d" % (defroute,nonlo))
' 2>/dev/null)
DEFROUTE=$(printf '%s' "$SENTRY_NET" | awk '{print $1}'); [ -z "$DEFROUTE" ] && DEFROUTE=unknown
NONLO=$(printf '%s' "$SENTRY_NET" | awk '{print $2}'); [ -z "$NONLO" ] && NONLO=-1
say "[assert] sentry_defroute=$DEFROUTE  sentry_nonlo=$NONLO"

# In-sentry TCP dial — same vocabulary seg_dial_verdict consumes.
seg_dial_raw() { # $1=host:port -> echoes ok|refused|timeout|dns|netunreach|other|skip
  [ -z "$1" ] && { echo skip; return 0; }
  local hp="$1" host port
  host="${hp%:*}"; port="${hp##*:}"
  RUNSC exec "$RCID" python3 -c '
import socket,sys,errno
host,port=sys.argv[1],int(sys.argv[2])
try:
    s=socket.create_connection((host,port),timeout=4); s.close(); print("ok")
except OSError as e:
    en=getattr(e,"errno",None)
    if en in (errno.ENETUNREACH,): print("netunreach")
    elif en in (errno.EHOSTUNREACH,): print("hostunreach")
    elif en in (errno.ENETDOWN,): print("netdown")
    elif en==errno.ECONNREFUSED: print("refused")
    elif en in (errno.ETIMEDOUT,) or isinstance(e,socket.timeout): print("timeout")
    elif isinstance(e,socket.gaierror): print("dns")
    else: print("other")
except Exception:
    print("other")
' "$host" "$port" 2>/dev/null
}
NATS_RAW=$(seg_dial_raw "$NATS_TGT")
BEADS_RAW=$(seg_dial_raw "$BEADS_TGT")
say "[assert] in-sentry dial: nats($NATS_TGT)=$NATS_RAW  beads($BEADS_TGT)=$BEADS_RAW"

seg_verdict "$DEFROUTE" "$NONLO" "$NATS_RAW" "$BEADS_RAW"
exit $?
}

# Run the live on-node main ONLY on direct execution. When SOURCED (the hermetic
# test sentry-egress-check_test.sh), seg_main is merely defined and the pure core
# (seg_dial_verdict + seg_verdict) is exercised with zero live side effects.
if [ "${BASH_SOURCE[0]}" = "${0}" ]; then
  seg_main "$@"
fi
