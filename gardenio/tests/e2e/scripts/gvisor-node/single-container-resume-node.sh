#!/usr/bin/env bash
# single-container-resume-node.sh — runs ON THE NODE (via gvisor-probe chroot /host).
# Proves fix path (c): the agent as runsc sandbox ROOT (single container, no pause sub-container) so
# raw `runsc checkpoint|restore` round-trips the agent task with no mounts-fd sub-container handoff.
# Driven by single-container-resume.sh (bead ka-0000000845). See single-container-resume.md.
set -uo pipefail
ROOT=/run/containerd/runsc/k8s.io     # reuse the CRI runsc root so the runtime config matches
# BDIR/CKPT/SC_CID default to the legacy fixed paths so every existing caller
# (resume-latency, rehydrate-ack, renudge-thaw, freeze-interrupt, userresponse-
# latency) is byte-for-byte unchanged. The resume-into-pool acceptance gate
# (ka-0000003295) overrides them per VARIANT so its claudeless + active-Claude
# checkpoints live in disjoint dirs and never clobber one another.
BDIR="${BDIR:-/var/tmp/sc}"            # bundle dir on the node
CKPT="${CKPT:-/var/tmp/ckSC}"          # checkpoint image dir on the node
SC_CID="${SC_CID:-sc-agent}"
SC_CID2="${SC_CID2:-sc-agent-restored}"
# WORKING_SET_ENTRIES sizes the agent's in-RAM heap cache — the proxy for an
# agent's loaded working set. Default 2000 (the legacy PoC value) so existing
# callers are unchanged; the acceptance gate drives WORKING_SET_ENTRIES=0 for its
# claudeless variant and the full heap for its active-Claude variant. AGENT_SENTINEL
# lets a caller stamp a per-run /work file token it can later assert survived.
WORKING_SET_ENTRIES="${WORKING_SET_ENTRIES:-2000}"
AGENT_SENTINEL="${AGENT_SENTINEL:-GVISOR-POC-SC-7f3a9c}"
# Cap every runsc call. checkpoint/restore + the post-restore state/ps calls can
# HANG on process exit (the documented runsc quirk, see test-gvisor-freeze-thaw.sh).
# Unbounded, the final restore self-proof would wedge any caller that invokes this
# via a node exec (the bench harness and the standing C7 gate). Capping each call
# lets the script finish naturally so its cleanup trap fires (no orphaned cids /
# mounts left for the next run). The C7 gate only needs the checkpoint artifacts
# this writes (ckSC + sc bundle), which exist well before the proof restore.
RUNSC_TIMEOUT="${RUNSC_TIMEOUT:-45}"
RUNSC() { timeout "$RUNSC_TIMEOUT" nsenter -t 1 -m -p -n -- runsc --root=$ROOT "$@"; }
# Mounts and rootfs checks MUST happen in host-init's mount namespace (where runsc's nsenter'd gofer
# re-exec runs) — a mount made in the probe's own mount ns is invisible to `nsenter -t 1 -m runsc`.
HMNT() { nsenter -t 1 -m -- "$@"; }

# SUBSTRATE-NOT-READY exit: the OCI rootfs / checkpoint substrate could not be
# assembled on THIS node — a registry/overlay/runtime environment fault, NOT an
# app regression. Emit a stable, machine-classifiable sentinel and exit with a
# distinct code (42) so callers (the standing C7/C4 latency gates) can SKIP
# inconclusively instead of reding the golden gate. This is the failure shape
# ka-0000001111 hit on 2026.160.8: overlay fast-path AND registry pull fallback
# both failed because the gvisor-poc node could not pull/assemble the rootfs.
SUBSTRATE_NOT_READY_RC=42
substrate_not_ready() {
  echo "    SUBSTRATE-NOT-READY: $*"
  exit "$SUBSTRATE_NOT_READY_RC"
}

cleanup() {
  RUNSC delete --force "$SC_CID" >/dev/null 2>&1 || true
  RUNSC delete --force "$SC_CID2" >/dev/null 2>&1 || true
  HMNT umount "$BDIR/rootfs" 2>/dev/null || true
}
trap cleanup EXIT

echo "==> [1/5] build single-container OCI bundle (agent process as ROOT, no pause sub-container)"
RUNSC delete --force "$SC_CID" >/dev/null 2>&1 || true
RUNSC delete --force "$SC_CID2" >/dev/null 2>&1 || true
HMNT umount "$BDIR/rootfs" 2>/dev/null || true
rm -rf "$BDIR"; mkdir -p "$BDIR/rootfs" "$BDIR/work" "$BDIR/upper" "$BDIR/owork"

# Assemble a python rootfs. AGENT_PY_DIR is where we drop /agent.py so it appears
# at the merged rootfs root (overlay upperdir on the fast path, the flat rootfs on
# the fallback path).
PYIMG="${PYIMG:-public.ecr.aws/docker/library/python:3.11-slim}"
AGENT_PY_DIR=""

# FAST PATH: resolve the python image's overlay lowerdirs via a throwaway
# snapshot-mounts query against a KNOWN snapshot key (the original PoC node's live
# agent snapshot), strip the writable upper, and overlay-mount the read-only image
# layers. Overridable via AGENT_SNAP. This only works on a node where that exact
# snapshot is present, so it is a best-effort fast path — NOT a hard requirement.
# The overlay MUST be mounted in host-init's mount ns so runsc's nsenter'd gofer
# can see it.
AGENT_SNAP="${AGENT_SNAP:-995b3039f052b7720ed96eee0086fee0d787de116f9a662cccd4b92f51f5153c}"
LAYERS=$(ctr -n k8s.io snapshot mounts /tmp/__q "$AGENT_SNAP" 2>/dev/null | grep -oE 'lowerdir=[^ ,]*' | sed 's/lowerdir=//')
if [ -n "$LAYERS" ] \
   && HMNT mount -t overlay overlay "$BDIR/rootfs" -o "lowerdir=$LAYERS,upperdir=$BDIR/upper,workdir=$BDIR/owork,index=off" 2>/dev/null \
   && HMNT test -x "$BDIR/rootfs/usr/local/bin/python3"; then
  AGENT_PY_DIR="$BDIR/upper"   # upper is visible through the merged rootfs at /
  echo "    rootfs python: OK (overlay fast path, snapshot ${AGENT_SNAP:0:12})"
else
  # PORTABLE FALLBACK: the hardcoded snapshot is absent on this node (e.g. a
  # freshly-provisioned gvisor-poc node, or any node that never ran the PoC pod).
  # Pull the python image via ctr and flatten its layers into a plain rootfs dir —
  # the same mechanism test-gvisor-freeze-thaw.sh uses, so the gate is portable to
  # any gvisor node, not just the original PoC host. No overlay, no snapshot
  # dependency: a flat directory rootfs is a first-class runsc root.
  HMNT umount "$BDIR/rootfs" 2>/dev/null || true
  rm -rf "$BDIR/oci" "$BDIR/img.tar"; mkdir -p "$BDIR/oci"
  # All three of these are SUBSTRATE faults (registry unreachable / image GC'd /
  # disk pressure), NOT app regressions — emit the machine-classifiable sentinel.
  ctr -n gvisor-sc images pull "$PYIMG" >/dev/null 2>&1 || substrate_not_ready "ctr pull $PYIMG failed (registry unreachable or image absent)"
  ctr -n gvisor-sc images export "$BDIR/img.tar" "$PYIMG" >/dev/null 2>&1 || substrate_not_ready "ctr export $PYIMG failed"
  tar -xf "$BDIR/img.tar" -C "$BDIR/oci" || substrate_not_ready "untar exported image failed"
  # Extract every layer (in manifest order) into the flat rootfs.
  for L in $(python3 -c 'import json;print("\n".join(json.load(open("'"$BDIR"'/oci/manifest.json"))[0]["Layers"]))' 2>/dev/null); do
    tar -xf "$BDIR/oci/$L" -C "$BDIR/rootfs" 2>/dev/null || true
  done
  if HMNT test -x "$BDIR/rootfs/usr/local/bin/python3"; then
    AGENT_PY_DIR="$BDIR/rootfs"
    echo "    rootfs python: OK (portable pull+flatten fallback, $PYIMG)"
  fi
fi
if [ -z "$AGENT_PY_DIR" ]; then
  substrate_not_ready "could not assemble a python rootfs (overlay fast path + pull fallback both failed)"; fi

# The warm-state agent program (same observable state as agentsim-pod.yaml): monotonic counter,
# a WORKING_SET_ENTRIES-entry heap cache (default 2000), 3 open FDs (log/state/udp),
# cwd /work/repo, sentinel in /work/state.bin.
# Written to $AGENT_PY_DIR so it appears at /agent.py inside the merged/flat rootfs
# (overlay upperdir on the fast path, the flat rootfs dir on the portable fallback).
cat > "$AGENT_PY_DIR/agent.py" <<'PY'
import os, time, json, socket
# SENTINEL (the /work file token) and the heap working-set size are read from the
# environment so a caller can drive per-variant runs (claudeless: 0 entries;
# active-Claude: a large loaded heap). Defaults preserve the legacy PoC values so
# existing callers are unchanged.
SENTINEL = os.environ.get("AGENT_SENTINEL", "GVISOR-POC-SC-7f3a9c")
WORKING_SET = int(os.environ.get("WORKING_SET_ENTRIES", "2000"))
cache = {"sentinel": SENTINEL, "repo_files": {}, "ctx": []}
for i in range(WORKING_SET):
    cache["repo_files"][f"file_{i}.go"] = ("x"*512)
wd = "/work/repo"; os.makedirs(wd, exist_ok=True); os.chdir(wd)
logf = open("/work/agent.log","a",buffering=1)
statef = open("/work/state.bin","w+b")
statef.write(SENTINEL.encode()); statef.flush()
s = socket.socket(socket.AF_INET, socket.SOCK_DGRAM)
start = time.time(); counter = 0
logf.write(f"BOOT pid={os.getpid()} sentinel={SENTINEL} cwd={os.getcwd()}\n")
while True:
    counter += 1
    cache["ctx"].append(counter)
    if len(cache["ctx"]) > 5: cache["ctx"].pop(0)
    logf.write(json.dumps({"counter": counter, "uptime_s": round(time.time()-start,2),
          "sentinel": cache["sentinel"], "fds":{"log":logf.fileno(),"state":statef.fileno(),"sock":s.fileno()},
          "state_offset": statef.tell(), "cwd": os.getcwd(),
          "repo_loaded": len(cache["repo_files"]), "ctx_tail": cache["ctx"][-1]})+"\n")
    time.sleep(1)
PY

# Minimal OCI runtime spec: SINGLE container, agent IS the init process (no pause, no sub-container).
cat > "$BDIR/config.json" <<JSON
{
  "ociVersion": "1.0.2",
  "process": {
    "terminal": false,
    "user": {"uid": 0, "gid": 0},
    "args": ["python3", "/agent.py"],
    "env": ["PATH=/usr/local/bin:/usr/local/sbin:/usr/sbin:/usr/bin:/sbin:/bin", "PYTHONUNBUFFERED=1", "AGENT_SENTINEL=__AGENT_SENTINEL__", "WORKING_SET_ENTRIES=__WORKING_SET_ENTRIES__"],
    "cwd": "/",
    "capabilities": {
      "bounding": ["CAP_NET_BIND_SERVICE","CAP_CHOWN","CAP_DAC_OVERRIDE","CAP_FOWNER","CAP_SETUID","CAP_SETGID"],
      "effective": ["CAP_NET_BIND_SERVICE","CAP_CHOWN","CAP_DAC_OVERRIDE","CAP_FOWNER","CAP_SETUID","CAP_SETGID"],
      "permitted": ["CAP_NET_BIND_SERVICE","CAP_CHOWN","CAP_DAC_OVERRIDE","CAP_FOWNER","CAP_SETUID","CAP_SETGID"]
    },
    "rlimits": [{"type": "RLIMIT_NOFILE", "hard": 65536, "soft": 65536}]
  },
  "root": {"path": "rootfs", "readonly": false},
  "hostname": "sc-agent",
  "mounts": [
    {"destination": "/proc", "type": "proc", "source": "proc"},
    {"destination": "/dev", "type": "tmpfs", "source": "tmpfs", "options": ["nosuid","strictatime","mode=755","size=65536k"]},
    {"destination": "/dev/pts", "type": "devpts", "source": "devpts", "options": ["nosuid","noexec","newinstance","ptmxmode=0666","mode=0620"]},
    {"destination": "/dev/shm", "type": "tmpfs", "source": "shm", "options": ["nosuid","noexec","nodev","mode=1777","size=65536k"]},
    {"destination": "/tmp", "type": "tmpfs", "source": "tmpfs", "options": ["nosuid","nodev","mode=1777"]},
    {"destination": "/work", "type": "bind", "source": "BDIR_WORK", "options": ["rbind","rw"]}
  ],
  "linux": {
    "namespaces": [
      {"type": "pid"},
      {"type": "ipc"},
      {"type": "uts"},
      {"type": "mount"},
      {"type": "network"}
    ]
  }
}
JSON
sed -i "s#BDIR_WORK#$BDIR/work#" "$BDIR/config.json"
sed -i "s#__AGENT_SENTINEL__#$AGENT_SENTINEL#" "$BDIR/config.json"
sed -i "s#__WORKING_SET_ENTRIES__#$WORKING_SET_ENTRIES#" "$BDIR/config.json"
echo "    bundle ready at $BDIR (single-container, agent=init; working_set=$WORKING_SET_ENTRIES sentinel=$AGENT_SENTINEL)"

echo "==> [2/5] runsc run --detach the single-container sandbox; accrue warm state"
RUNSC run --bundle "$BDIR" --detach "$SC_CID"
sleep 6
ST=$(RUNSC state "$SC_CID" 2>&1)
echo "    status=$(echo "$ST" | python3 -c 'import sys,json;d=json.load(sys.stdin);print(d["status"],"pid",d["pid"])' 2>/dev/null || echo "$ST" | head -c 200)"
echo "    runsc ps (expect python3 as init pid 1):"; RUNSC ps "$SC_CID" 2>&1 | head
echo -n "    pre-freeze counter: "; tail -1 "$BDIR/work/agent.log" 2>&1

# RUN_ONLY: stop here with the single-container sandbox BUILT + RUNNING, and do
# NOT checkpoint. This is the live freeze TARGET the interruptible-freeze e2e
# (test-gvisor-freeze-interrupt.sh / cmd/freeze-interrupt-probe, ka-0000004367)
# checkpoints itself: the probe drives the GENUINE controller Freeze (which execs
# its OWN `runsc checkpoint --leave-running "$SC_CID"`) and a real
# POST /api/v1/freeze/interrupt that ABORTS it mid-checkpoint, then asserts the
# C2 invariants (sandbox still running, no orphaned image, no snapshot) against
# this same live cid. We leave $SC_CID alive and untouched; the harness owns its
# cleanup (force-delete after the proof). The agent's monotonic counter in
# $BDIR/work/agent.log is the liveness signal the probe samples before/after the
# abort to prove the agent never went dark.
if [ "${RUN_ONLY:-0}" = "1" ]; then
  echo "    RUN_ONLY=1 — single-container sandbox '$SC_CID' built + running; skipping checkpoint."
  echo "    SC_CID=$SC_CID  BDIR=$BDIR"
  RUN_STATUS=$(RUNSC state "$SC_CID" 2>/dev/null | python3 -c 'import sys,json;print(json.load(sys.stdin)["status"])' 2>/dev/null)
  echo "    RUN_STATUS=${RUN_STATUS:-unknown}"
  if [ "$RUN_STATUS" = "running" ]; then
    echo "    RUN_ONLY_PROVEN=1 (sandbox running; ready as a freeze target)"
  else
    echo "    RUN_ONLY_PROVEN=0 (status='${RUN_STATUS:-unknown}')"
  fi
  exit 0
fi

echo "==> [3/5] runsc checkpoint --leave-running the single cid"
rm -rf "$CKPT" && mkdir -p "$CKPT"
t0=$(date +%s%3N)
RUNSC checkpoint --leave-running --image-path="$CKPT" "$SC_CID"
CRC=$?
t1=$(date +%s%3N)
echo "    checkpoint rc=$CRC  size=$(du -sh "$CKPT"|cut -f1)  freeze_ms=$((t1-t0))"
echo -n "    counter at checkpoint: "; tail -1 "$BDIR/work/agent.log"
sleep 4
echo -n "    counter after (leave-running still advancing): "; tail -1 "$BDIR/work/agent.log"

# CHECKPOINT_ONLY: stop here with the checkpoint written (+ the /var/tmp/sc bundle
# left in place) — the artifacts the thaw-into-B path consumes. Callers that only
# need the checkpoint (the standing C7 resume-latency gate) set this to skip the
# self-proof restore in steps 4-5. That restore is a `--detach` call captured in a
# command substitution, which BLOCKS the substitution on the detached daemon's
# stdout pipe (the runsc exit quirk) — fine for an interactive PoC run, but it
# would wedge a programmatic caller. We delete the leave-running source cid first
# so the next thaw-into-B restore starts from a clean runsc root.
if [ "${CHECKPOINT_ONLY:-0}" = "1" ]; then
  RUNSC delete --force "$SC_CID" >/dev/null 2>&1 || true
  echo "    CHECKPOINT_ONLY=1 — checkpoint ready at $CKPT (+ bundle $BDIR); skipping self-proof restore."
  exit 0
fi

# LEAVE_RUNNING_ONLY: stop here with the checkpoint written AND the source cid
# left ALIVE (`--leave-running` above did not stop it). This is the in-flight
# freeze state the C2 interrupt path (ka-0000005834) defends: a checkpoint has
# been taken but the controller, on an inbound user message, ABORTS before the
# stop/relocate step so Pod A stays live to serve the message — no thaw needed.
# The standing C4 <10s gate uses this to time the MID-FREEZE user-visible leg:
# checkpoint-in-flight -> interrupt -> the still-live agent's counter must keep
# advancing (proof the agent never went dark). The plan-of-record invariant this
# guards: `runsc checkpoint --leave-running` MUST leave the sandbox `running`
# (freezethaw.go L540 post-state assertion). We assert exactly that here and emit
# the live cid + a pre/post counter pair the harness compares for liveness. We do
# NOT delete the source cid — the caller measures against the live agent and is
# responsible for cleanup (the C4 harness force-deletes it after the proof).
if [ "${LEAVE_RUNNING_ONLY:-0}" = "1" ]; then
  # Emit the in-flight-checkpoint duration (freeze_ms) — t0/t1 still hold the
  # `runsc checkpoint --leave-running` timing from the block above. This IS the
  # user-visible mid-freeze window the C2 interrupt bounds: the worst case a
  # message waits is for the in-flight checkpoint to be aborted/complete, after
  # which the still-live sandbox serves with NO thaw.
  LR_FREEZE_MS=$((t1 - t0))
  # Assert the C2 leave-running INVARIANT (plan-of-record / freezethaw.go L540):
  # `runsc checkpoint --leave-running` MUST leave the sandbox `running`. This is
  # the load-bearing precondition for the interrupt path — if the sandbox is not
  # running after the checkpoint, the agent went dark and "serve without a thaw"
  # is impossible.
  LR_STATUS=$(RUNSC state "$SC_CID" 2>/dev/null | python3 -c 'import sys,json;print(json.load(sys.stdin)["status"])' 2>/dev/null)
  # Best-effort liveness corroboration: the still-running agent's counter at the
  # moment we confirm `running` (informational — the gating invariant is STATUS).
  LR_COUNTER=$(tail -1 "$BDIR/work/agent.log" 2>/dev/null | python3 -c 'import sys,json;print(json.load(sys.stdin)["counter"])' 2>/dev/null)
  echo "    LEAVE_RUNNING_ONLY=1 — checkpoint taken, source cid '$SC_CID' left running."
  echo "    LR_STATUS=${LR_STATUS:-unknown}  LR_FREEZE_MS=${LR_FREEZE_MS}  LR_COUNTER=${LR_COUNTER:-NA}"
  if [ "$LR_STATUS" = "running" ]; then
    echo "    LEAVE_RUNNING_PROVEN=1 (sandbox still running after leave-running checkpoint; no thaw needed to serve)"
  else
    echo "    LEAVE_RUNNING_PROVEN=0 (status='${LR_STATUS:-unknown}' — leave-running did NOT keep the sandbox running)"
  fi
  exit 0
fi

echo "==> [4/5] runsc restore the image into a NEW cid (single container = single restore, no mounts-fd)"
CKPT_COUNTER=$(tail -1 "$BDIR/work/agent.log" | python3 -c 'import sys,json;print(json.load(sys.stdin)["counter"])')
echo "    counter just before restore: $CKPT_COUNTER"
# stop the leave-running original first so only the restored cid writes the shared /work/agent.log
RUNSC delete --force "$SC_CID" >/dev/null 2>&1 || true
# Redirect the --detach restore to a FILE, not a command substitution: the
# detached daemon inherits the substitution's stdout pipe and holds it open,
# blocking $(...) until the daemon exits. See the same fix in
# single-container-thaw-into-b-node.sh.
# Derive the restore-log path from BDIR so parallel variant runs (each with its own
# BDIR) never share a log file. Only used on the self-proof restore path (not the
# CHECKPOINT_ONLY early-exit callers rely on).
RLOG="$BDIR-restore.out"
t0=$(date +%s%3N)
RUNSC restore --image-path="$CKPT" --bundle "$BDIR" --detach "$SC_CID2" >"$RLOG" 2>&1; RC=$?
t1=$(date +%s%3N)
echo "    THAW_MS=$((t1-t0))  rc=$RC"
OUT="$(head -c 600 "$RLOG" 2>/dev/null)"
[ -n "$OUT" ] && echo "    runsc output: $OUT"
if [ "$RC" -ne 0 ]; then echo "    ❌ restore failed (rc=$RC)"; exit "$RC"; fi

echo "==> [5/5] PROOF: restored single-container init task re-scheduled + counter advanced"
sleep 5
echo "    runsc ps restored cid:"; RUNSC ps "$SC_CID2" 2>&1 | head
RPID=$(RUNSC state "$SC_CID2" 2>/dev/null | python3 -c 'import sys,json;print(json.load(sys.stdin)["pid"])' 2>/dev/null)
echo "    restored sentry pid=$RPID  netns=$(readlink /proc/$RPID/ns/net 2>/dev/null)"
echo -n "    counter after restore: "; tail -1 "$BDIR/work/agent.log"
LAST=$(tail -1 "$BDIR/work/agent.log" | python3 -c 'import sys,json;print(json.load(sys.stdin)["counter"])' 2>/dev/null)
SENT=$(tail -1 "$BDIR/work/agent.log" | python3 -c 'import sys,json;print(json.load(sys.stdin)["sentinel"])' 2>/dev/null)
echo "    checkpoint_counter=$CKPT_COUNTER  post_restore_counter=$LAST  sentinel=$SENT"
if [ -n "$LAST" ] && [ "$LAST" -gt "$CKPT_COUNTER" ] 2>/dev/null; then
  echo "    ✅ single-container resume PROVEN: agent init task re-scheduled on raw runsc restore;"
  echo "       counter advanced $CKPT_COUNTER -> $LAST with RAM/FD/cwd/sentinel fidelity; NO mounts-fd handoff."
else
  echo "    ⚠ restored but counter did not advance past checkpoint — inspect $BDIR/work/agent.log"
fi
