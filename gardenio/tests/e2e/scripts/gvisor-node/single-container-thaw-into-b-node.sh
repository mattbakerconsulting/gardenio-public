#!/usr/bin/env bash
# single-container-thaw-into-b-node.sh — runs ON THE NODE (gvisor-probe chroot /host).
# THROUGH-LIFECYCLE proof for the single-container shape (bead ka-0000000845, fix path c):
# restore the single-container agent checkpoint (from single-container-resume.sh) into placeholder
# Pod B's FRESH, kubelet-provisioned CNI netns — proving fix (c) composes with the MR!3488
# placeholder-pod pattern. Combines:
#   - fix (c): agent is the runsc ROOT container -> single `runsc restore`, no mounts-fd handoff
#   - through-lifecycle: restored sandbox joins B's kubelet-owned netns (old netns GC'd)
# Args (env): B_NETNS (B's CNI netns path), expects /var/tmp/ckSC image + /var/tmp/sc bundle present.
set -uo pipefail
ROOT=/run/containerd/runsc/k8s.io
# BDIR/CKPT/RCID default to the legacy fixed paths so every existing caller is
# byte-for-byte unchanged. The resume-into-pool acceptance gate (ka-0000003295)
# overrides them to point at its per-variant checkpoint/bundle dirs (claudeless
# vs active-Claude) and a per-variant restored cid, so the two variants' restores
# never collide on a shared cid/root.
BDIR="${BDIR:-/var/tmp/sc}"
CKPT="${CKPT:-/var/tmp/ckSC}"
B_NETNS="${B_NETNS:?set B_NETNS to placeholder pod B CNI netns path}"
B_SENTRY_PID="${B_SENTRY_PID:?set B_SENTRY_PID}"
RCID="${RCID:-sc-agent-into-b}"
# Cap every runsc call: checkpoint/restore + the post-restore state/ps calls can
# HANG on process exit (the documented runsc quirk, see test-gvisor-freeze-thaw.sh).
# An unbounded `runsc state`/`ps` here would wedge any caller (the bench harness
# and the standing C7 gate both invoke this via a node exec). THAW_MS is printed
# BEFORE the proof section, so a capped proof call never loses the timing number.
RUNSC_TIMEOUT="${RUNSC_TIMEOUT:-45}"
RUNSC() { timeout "$RUNSC_TIMEOUT" nsenter -t 1 -m -p -n -- runsc --root=$ROOT "$@"; }

RUNSC delete --force "$RCID" >/dev/null 2>&1 || true

echo "==> repoint the SAME single-container bundle's network ns -> B's CNI netns (in place)"
# Reuse the identical bundle dir (/var/tmp/sc) whose rootfs overlay the checkpoint's gofer was rooted
# against — a `cp -a` of an overlay mountpoint diverges the FS tree and breaks the 9p gofer re-walk.
# We only rewrite the network-namespace path; everything else (rootfs overlay, /work bind, agent.py)
# is the single-container bundle's own — no sub-container, no mounts-fd handoff.
python3 - "$B_NETNS" "$BDIR/config.json" <<'PY'
import json, sys
bnet, p = sys.argv[1], sys.argv[2]
d = json.load(open(p))
for n in d["linux"]["namespaces"]:
    if n["type"] == "network":
        n["path"] = bnet
json.dump(d, open(p, "w"))
print(f"    repointed network ns -> {bnet}")
PY

echo "==> runsc restore single-container checkpoint into B's netns"
# IMPORTANT: do NOT capture the --detach restore via OUT=$(...). The detached
# sandbox daemon INHERITS the command-substitution's stdout pipe and keeps it
# open, so `$(...)` blocks until the daemon exits — wedging any programmatic
# caller (the bench harness and the standing C7 gate both invoke this via a node
# exec). Redirect the restore's output to a FILE instead so nothing waits on the
# daemon's inherited pipe; the runsc CLI itself returns promptly and t1 captures
# the true restore latency.
RLOG="/var/tmp/thaw-into-b-${RCID}.out"   # per-cid so parallel variant restores don't share a log
t0=$(date +%s%3N)
RUNSC restore --image-path="$CKPT" --bundle "$BDIR" --detach "$RCID" >"$RLOG" 2>&1; RC=$?
t1=$(date +%s%3N)
echo "    THAW_MS=$((t1-t0))  rc=$RC"
OUT="$(head -c 600 "$RLOG" 2>/dev/null)"
[ -n "$OUT" ] && echo "    runsc output: $OUT"
if [ "$RC" -ne 0 ]; then echo "    ❌ restore-into-B failed"; exit "$RC"; fi

echo "==> PROOF: restored single-container agent shares B's kubelet-owned netns inode"
sleep 4
RPID=$(RUNSC state "$RCID" 2>/dev/null | python3 -c 'import sys,json;print(json.load(sys.stdin)["pid"])' 2>/dev/null)
echo "    runsc ps:"; RUNSC ps "$RCID" 2>&1 | head
R_NETNS=$(readlink /proc/$RPID/ns/net 2>/dev/null)
B_NETNS_INODE=$(readlink /proc/$B_SENTRY_PID/ns/net 2>/dev/null)
echo "    restored agent sentry pid=$RPID netns=$R_NETNS"
echo "    placeholder B sentry  pid=$B_SENTRY_PID netns=$B_NETNS_INODE"
echo -n "    counter after thaw-into-B: "; tail -1 "$BDIR/work/agent.log" 2>/dev/null
if [ -n "$R_NETNS" ] && [ "$R_NETNS" = "$B_NETNS_INODE" ]; then
  echo "    ✅ single-container thaw-through-lifecycle PROVEN: restored agent task runs in B's"
  echo "       kubelet-provisioned netns inode ($R_NETNS); single restore, no mounts-fd handoff."
else
  echo "    ⚠ netns mismatch (restored=$R_NETNS vs B=$B_NETNS_INODE)"
fi
RUNSC delete --force "$RCID" >/dev/null 2>&1 || true
