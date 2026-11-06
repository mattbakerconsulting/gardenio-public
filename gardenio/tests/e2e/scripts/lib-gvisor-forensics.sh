#!/usr/bin/env bash
# SC2034: several FX_* return-channel vars (FT_FORENSIC_*) and the per-collector
#         status vars are set here and READ by the sourcing canary driver — not
#         unused. The file-level directive silences the false positives.
# shellcheck disable=SC2034
# lib-gvisor-forensics.sh — Rig R2: auto-capture a forensic evidence bundle on
# ANY gVisor freeze→thaw canary rung failure, BEFORE the HARD HOLD.
#
# ── WHY THIS EXISTS (bead ka-0000006948, rig epic ka-0000000867) ──────────────
#   The gvisor-poc freeze→thaw canary (role ka-0000003266, redrive task
#   ka-0000001355) climbs an assertion ladder of named rungs (R1, ka-0000005639):
#     snapshot_persisted, restore_rc0, netns_routable, cni_confirmed,
#     gb_ack+coop_ack, work_intact, ram_intact.
#   When a rung fails the canary must HARD HOLD (one canary, zero orphans,
#   prod/freezeThaw-flag untouched). But the evidence that makes the failure
#   debuggable — Pod B's netns L3 state, the runsc restore stderr, the committed
#   CNI result, Pod B's phase+events, the controller log tail, and the full FT_*
#   marker set — is EPHEMERAL: kubelet GCs Pod B, the AWS VPC CNI DELs the netns,
#   the 3.0G checkpoint image is reaped, and the controller log rolls. The last
#   canary (bundle ka-0000006433 / doc ka-0000007065) was only debuggable because
#   a human captured all of that BY HAND, pre-GC, in the failing freeze window —
#   which is exactly how the dir-medium freeze-sync regression (bug ka-0000000357)
#   was root-caused on-node.
#
#   This library reproduces that hand-capture AUTOMATICALLY: on the first failing
#   rung the canary driver calls gvisor_forensic_capture, which snapshots every
#   piece of evidence into a local staging dir and assembles a `gb bundle`
#   (kind=report) parented to the canary task, then prints the bundle id so:
#     - Rig R3 (ka-0000004124) links the bundle into the deduped next-layer bug,
#     - Rig R4 (ka-0000006231) attaches it to its HARD HOLD / gb decision.
#
# ── DESIGN PRINCIPLES ────────────────────────────────────────────────────────
#   1. FAIL-SOFT, ALWAYS. The whole point is to grab evidence before it vanishes.
#      A collector that errors (Pod B already gone, netns unreadable, runsc state
#      hung) writes a "(capture failed: …)" note into ITS artifact and the
#      capture proceeds with the rest. A single missing source must NEVER lose
#      the others, and must NEVER abort the canary's HARD HOLD path. The whole
#      capture runs best-effort; the only hard requirement is that it returns.
#   2. DEPENDENCY-INJECTED RUNNERS. The node-side, kubectl and gb accessors are
#      indirected through FX_NODE_RUNNER / FX_KUBECTL / FX_GB function-or-binary
#      names, so the companion hermetic unit test (test-lib-gvisor-forensics.sh)
#      drives the whole pipeline with FAKES — no cluster, no daemon, gated on
#      every MR/main push (mirrors lib-gvisor-node.sh + test-lib-gvisor-node.sh).
#   3. NODE-RUNNER CONTRACT (more forgiving than lib-gvisor-node.sh): the runner
#      takes a wall-clock cap (seconds) as $1; the command is delivered on BOTH
#      positional $2 AND stdin, so EITHER node-exec idiom in the tree works
#      WITHOUT a wrapper — resume-latency's `node_exec` (reads $2) and
#      freeze-thaw's `node_run` (`kubectl exec -i … bash -s`, reads stdin). The
#      canary driver passes the SAME shim it already uses for the lifecycle, as-is.
#   4. BOUNDED. Every node-side / kubectl read is wrapped in a wall-clock cap
#      (runsc state/ps can hang on exit — the documented quirk) so the capture
#      itself can never wedge the driver.
#
# ── USAGE (from the canary driver / Rig R4 loop) ─────────────────────────────
#   # shellcheck disable=SC1090
#   source "$(dirname "${BASH_SOURCE[0]}")/lib-gvisor-forensics.sh"
#
#   # node_run: YOUR module's node-exec shim (freeze-thaw's `node_run` OR
#   #           resume-latency's `node_exec` — both work unwrapped, see #3).
#   # markers:  the harvested FT_*/FT_RUNG_* stdout the lifecycle emitted (a file
#   #           path OR a literal blob in FX_MARKERS).
#   # NOTE: call it BARE, not `if gvisor_forensic_capture; then`. It returns 1 on
#   # the COMMON degraded path (gb absent / bundle create failed) — which is NOT
#   # an error: the local evidence dir (FT_FORENSIC_DIR) is still fully staged.
#   # Only rc=2 (no writable staging dir at all) is a real failure. Read the
#   # FT_FORENSIC_* sentinels, don't branch on the exit code.
#   FX_NODE_RUNNER=node_run \
#   FX_ORIGIN_BEAD=ka-0000001355 \
#   FX_RUNG=netns_routable FX_REASON="restored sentry landed in lo-only netns" \
#   FX_NS=gvisor-poc FX_PROBE="$PROBE" \
#   FX_POD_B=agentsim-b FX_POD_B_UID="$BPOD" FX_BNETNS="$BNETNS_PATH" \
#   FX_RUNSC_ROOT=/run/containerd/runsc/k8s.io FX_RESTORE_CID="$RCID" \
#   FX_RESTORE_STDERR=/var/tmp/thaw-into-b.out \
#   FX_CONTROLLER_NS=gardenio FX_MARKERS="$MARKER_BLOB" \
#     gvisor_forensic_capture
#
#   echo "$FT_FORENSIC_BUNDLE"   # bundle id, also printed as FT_FORENSIC_BUNDLE=<id>
#
# ── RETURN CHANNEL (stdout sentinels + shell vars) ───────────────────────────
#   FT_FORENSIC_DIR=<dir>     local staging dir with all raw artifacts (always set)
#   FT_FORENSIC_BUNDLE=<id>   created bundle bead id ("" if bundling unavailable)
#   FT_FORENSIC_URL=<url>     /bundle/<id> viewer path ("" if no bundle)
#   gvisor_forensic_capture returns 0 if a bundle was created (even partial),
#   1 if only the local dir could be staged (evidence preserved, no bundle — the
#   common degraded path, NOT an error), 2 only if no writable staging dir could
#   be created at all (nothing preserved — a real failure).
#
# ── TUNABLES (env; override before calling) ──────────────────────────────────
#   FX_NODE_RUNNER      node-exec shim fn: `fn <cap-secs> <cmd>` (REQUIRED for
#                       node-side collectors; absent => those write a skip note)
#   FX_KUBECTL          kubectl binary/fn (default: kubectl)
#   FX_GB               gb binary/fn (default: gb)
#   FX_ORIGIN_BEAD      bundle parent task (default: ka-0000001355, the redrive)
#   FX_RUNG / FX_REASON failing rung name + short reason (for the summary doc)
#   FX_NS               k8s ns holding Pod B + events (default: gvisor-poc)
#   FX_PROBE            privileged probe pod for node-side chroot exec
#   FX_POD_B            Pod B name; FX_POD_B_UID its uid (for the CNI result match)
#   FX_BNETNS           Pod B CNI netns path (ip addr/route, /proc/net/route)
#   FX_RUNSC_ROOT       runsc state root (default: /run/containerd/runsc/k8s.io)
#   FX_RESTORE_CID      restore-target container id (runsc state/ps)
#   FX_RUNSC_DEBUG_DIR  node dir holding runsc --debug-log output (optional)
#   FX_RESTORE_STDERR   node path to the restore stderr capture (e.g.
#                       /var/tmp/thaw-into-b.out from the thaw-into-b harness)
#   FX_CONTROLLER_NS    controller ns for the log tail (default: gardenio)
#   FX_CONTROLLER_SELECTOR  controller pod selector (default:
#                       app.kubernetes.io/component=controller)
#   FX_CONTROLLER_TAIL  controller log lines to tail (default: 400)
#   FX_MARKERS          harvested FT_* marker set: a file path OR a literal blob
#   FX_STAGE_DIR        staging dir (default: mktemp -d)
#   FX_NODE_CAP         per-node-read wall-clock cap, seconds (default: 30)
#   FX_KUBECTL_CAP      per-kubectl-read wall-clock cap, seconds (default: 30)

# Guard against multiple sourcing within the same shell.
# shellcheck disable=SC2317
if [ -n "${__GARDENIO_LIB_GVISOR_FORENSICS_LOADED:-}" ]; then
  return 0 2>/dev/null || true
fi
__GARDENIO_LIB_GVISOR_FORENSICS_LOADED=1

: "${FX_KUBECTL:=kubectl}"
: "${FX_GB:=gb}"
: "${FX_ORIGIN_BEAD:=ka-0000001355}"
: "${FX_NS:=gvisor-poc}"
: "${FX_RUNSC_ROOT:=/run/containerd/runsc/k8s.io}"
: "${FX_CONTROLLER_NS:=gardenio}"
: "${FX_CONTROLLER_SELECTOR:=app.kubernetes.io/component=controller}"
: "${FX_CONTROLLER_TAIL:=400}"
: "${FX_NODE_CAP:=30}"
: "${FX_KUBECTL_CAP:=30}"

# Return channel — populated by gvisor_forensic_capture, read by the caller.
FT_FORENSIC_DIR=""
FT_FORENSIC_BUNDLE=""
FT_FORENSIC_URL=""

# ── small internal helpers ───────────────────────────────────────────────────

# _fx_log — diagnostics to stderr so they don't pollute the FT_FORENSIC_* stdout
# sentinels the caller parses.
_fx_log() { printf '    [gvisor-forensics] %s\n' "$*" >&2; }

# _fx_node — run a command in the gvisor node host chroot via the injected
# runner, capped. Echoes a uniform skip line (never errors) when no runner is
# wired, so a collector degrades to a note instead of aborting.
#
# The command is fed on BOTH channels so EITHER node-exec idiom in the tree works
# WITHOUT a wrapper shim:
#   - positional $2 — resume-latency's `node_exec` (runs `bash -c "$1"` after a
#     shift), and the hermetic test's fakes, read the command from $2.
#   - stdin (here-string) — freeze-thaw's `node_run` (`kubectl exec -i … chroot
#     /host bash -s`) IGNORES $2 and reads the script from stdin. A here-string
#     (`<<<`) is a kernel-buffered/temp-file fd, so a runner that does NOT consume
#     stdin (node_exec) just leaves it unread — no SIGPIPE, no hang.
# This is deliberately MORE forgiving than lib-gvisor-node.sh, which requires
# node_run callers to hand-wrap it in a 2-arg shim; here the canary's existing
# node_run OR node_exec shim works as-is.
_fx_node() {
  local cmd="$1" cap="${2:-$FX_NODE_CAP}"
  if [ -z "${FX_NODE_RUNNER:-}" ]; then
    echo "(skipped: no FX_NODE_RUNNER wired — node-side capture unavailable)"
    return 0
  fi
  # The runner is responsible for the chroot + nsenter into host init ns (and,
  # for the real cluster runner, exec'ing through FX_PROBE).
  "$FX_NODE_RUNNER" "$cap" "$cmd" <<<"$cmd" 2>&1 \
    || echo "(node command exited non-zero — partial/empty output above)"
}

# _fx_kubectl — capped kubectl read against the cluster. Best-effort: a failure
# yields its stderr in-band rather than aborting the collector.
_fx_kubectl() {
  timeout "$FX_KUBECTL_CAP" "$FX_KUBECTL" "$@" 2>&1 || echo "(kubectl exited non-zero — partial/empty output above)"
}

# _fx_section — write a labelled section header into the current artifact.
_fx_section() { printf '\n===== %s =====\n' "$*"; }

# ── collectors — each writes ONE artifact file; each is individually fail-soft ─
# Every collector takes the output file path as $1 and returns 0 even on capture
# failure (the failure is recorded IN the artifact). This is deliberate: the
# capture must survive any single missing source.

# gvisor_forensic_collect_netns — Pod B netns L3: ip addr / ip route /
# /proc/net/route inside FX_BNETNS, plus host-ns ip addr/route for context.
# This is the evidence that localizes a lo-only / link-only restore failure
# (the recurring netns-strip class: ka-0000001790 → ka-0000001156 → ka-0000001099).
gvisor_forensic_collect_netns() {
  local out="$1"
  {
    echo "# netns + routing forensics (Pod B CNI netns: ${FX_BNETNS:-<unset>})"
    if [ -n "${FX_BNETNS:-}" ]; then
      _fx_section "ip -d addr  (in Pod B netns)"
      _fx_node "nsenter --net=${FX_BNETNS} -- ip -d addr 2>&1; echo rc=\$?"
      _fx_section "ip route  (in Pod B netns)"
      _fx_node "nsenter --net=${FX_BNETNS} -- ip route 2>&1; echo rc=\$?"
      _fx_section "ip -4 -o addr scope global  (in Pod B netns — the netns_routable rung signal)"
      _fx_node "nsenter --net=${FX_BNETNS} -- ip -4 -o addr show scope global 2>&1; echo rc=\$?"
      _fx_section "/proc/net/route  (in Pod B netns — default route => CNI route step ran)"
      _fx_node "nsenter --net=${FX_BNETNS} -- cat /proc/net/route 2>&1; echo rc=\$?"
    else
      echo "(FX_BNETNS unset — no Pod B netns to inspect; recording host-ns context only)"
    fi
    _fx_section "host-ns ip addr (node context)"
    _fx_node "ip -d addr 2>&1; echo rc=\$?"
    _fx_section "host-ns ip route (node context)"
    _fx_node "ip route 2>&1; echo rc=\$?"
    _fx_section "host /var/run/netns listing"
    _fx_node "ls -la /var/run/netns 2>&1; echo rc=\$?"
  } >"$out" 2>&1 || _fx_log "netns collector wrote with errors (recorded in $out)"
  return 0
}

# gvisor_forensic_collect_runsc — runsc state/ps for the restore-target cid,
# the restore stderr capture, and any runsc --debug-log output on the node.
# THIS is the layer that distinguishes a freeze-side loss (bug ka-0000000357,
# rc=128 CWD-dentry ENOENT) from an L3-strip thaw failure.
gvisor_forensic_collect_runsc() {
  local out="$1"
  {
    echo "# runsc restore forensics (root=${FX_RUNSC_ROOT}; restore-cid=${FX_RESTORE_CID:-<unset>})"
    _fx_section "runsc list"
    _fx_node "timeout 20 nsenter -t 1 -m -p -- runsc --root=${FX_RUNSC_ROOT} list 2>&1; echo rc=\$?"
    if [ -n "${FX_RESTORE_CID:-}" ]; then
      _fx_section "runsc state ${FX_RESTORE_CID}"
      _fx_node "timeout 20 nsenter -t 1 -m -p -- runsc --root=${FX_RUNSC_ROOT} state ${FX_RESTORE_CID} 2>&1; echo rc=\$?"
      _fx_section "runsc ps ${FX_RESTORE_CID}"
      _fx_node "timeout 20 nsenter -t 1 -m -p -- runsc --root=${FX_RUNSC_ROOT} ps ${FX_RESTORE_CID} 2>&1; echo rc=\$?"
    fi
    _fx_section "restore stderr capture (${FX_RESTORE_STDERR:-<unset>})"
    if [ -n "${FX_RESTORE_STDERR:-}" ]; then
      _fx_node "tail -c 8192 ${FX_RESTORE_STDERR} 2>&1; echo rc=\$?"
    else
      echo "(FX_RESTORE_STDERR unset — driver did not record the restore stderr path)"
    fi
    _fx_section "runsc --debug-log output (${FX_RUNSC_DEBUG_DIR:-<unset>})"
    if [ -n "${FX_RUNSC_DEBUG_DIR:-}" ]; then
      # Use an if/then on the node (not `[ -n "$f" ] && tail`) so the trailing
      # `echo rc=$?` reports the TAIL's status, not the empty-glob test's rc=1.
      _fx_node "ls -la ${FX_RUNSC_DEBUG_DIR} 2>&1; echo '--- newest debug log tail ---'; f=\$(ls -1t ${FX_RUNSC_DEBUG_DIR}/* 2>/dev/null | head -1); if [ -n \"\$f\" ]; then tail -c 16384 \"\$f\" 2>&1; echo rc=\$?; else echo '(no debug-log files present)'; fi"
    else
      echo "(FX_RUNSC_DEBUG_DIR unset — runsc was not run with --debug-log, or the dir was not passed)"
    fi
  } >"$out" 2>&1 || _fx_log "runsc collector wrote with errors (recorded in $out)"
  return 0
}

# gvisor_forensic_collect_cni — /var/lib/cni/results listing + Pod B's committed
# CNI result JSON (matched by K8S_POD_UID, the authoritative fix-C source of
# truth, ka-0000001099). Confirms whether the CNI ADD ever committed an IP.
gvisor_forensic_collect_cni() {
  local out="$1"
  {
    echo "# CNI result forensics (/var/lib/cni/results; Pod B uid=${FX_POD_B_UID:-<unset>} name=${FX_POD_B:-<unset>})"
    _fx_section "/var/lib/cni/results listing"
    _fx_node "ls -la /var/lib/cni/results 2>&1; echo rc=\$?"
    _fx_section "Pod B committed CNI result (matched by K8S_POD_UID / K8S_POD_NAME)"
    # Single-quoted python heredoc body runs ON THE NODE; FX_* expand here into
    # the argv. Mirrors read_cni_pod_ip in freezethaw_node.go but dumps the WHOLE
    # matched result (not just the IP) for forensics.
    _fx_node "python3 - '${FX_POD_B_UID:-}' '${FX_POD_B:-}' <<'PYCNI'
import json,glob,sys
uid,name=(sys.argv[1] if len(sys.argv)>1 else ''),(sys.argv[2] if len(sys.argv)>2 else '')
hit=False
for fp in sorted(glob.glob('/var/lib/cni/results/*')):
    try: d=json.load(open(fp))
    except Exception as e:
        print('  (unreadable %s: %s)'%(fp,e)); continue
    args=dict((kv[0],kv[1]) for kv in d.get('cniArgs',[]) if isinstance(kv,(list,tuple)) and len(kv)==2)
    if (uid and args.get('K8S_POD_UID')==uid) or (name and args.get('K8S_POD_NAME')==name):
        hit=True
        print('  MATCH file=%s'%fp)
        print(json.dumps(d,indent=2)[:8000])
if not hit:
    print('  (no committed CNI result matched Pod B uid=%r name=%r — CNI ADD likely never committed an IP)'%(uid,name))
PYCNI
echo rc=\$?"
  } >"$out" 2>&1 || _fx_log "cni collector wrote with errors (recorded in $out)"
  return 0
}

# gvisor_forensic_collect_podb — Pod B phase/conditions (get pod -o yaml) +
# the events scoped to Pod B. Shows whether the placeholder ever went Ready and
# what the kubelet/scheduler reported.
gvisor_forensic_collect_podb() {
  local out="$1"
  {
    echo "# Pod B phase + events (ns=${FX_NS}; pod=${FX_POD_B:-<unset>})"
    if [ -n "${FX_POD_B:-}" ]; then
      _fx_section "kubectl get pod ${FX_POD_B} -o wide"
      _fx_kubectl -n "$FX_NS" get pod "$FX_POD_B" -o wide
      _fx_section "kubectl get pod ${FX_POD_B} -o yaml (status/conditions)"
      _fx_kubectl -n "$FX_NS" get pod "$FX_POD_B" -o yaml
      _fx_section "kubectl get events --field-selector involvedObject.name=${FX_POD_B}"
      _fx_kubectl -n "$FX_NS" get events --field-selector "involvedObject.name=${FX_POD_B}" --sort-by=.lastTimestamp
    else
      echo "(FX_POD_B unset — no placeholder pod to inspect; dumping recent ns events instead)"
      _fx_section "kubectl get events (ns ${FX_NS}, recent)"
      _fx_kubectl -n "$FX_NS" get events --sort-by=.lastTimestamp
    fi
  } >"$out" 2>&1 || _fx_log "podb collector wrote with errors (recorded in $out)"
  return 0
}

# gvisor_forensic_collect_controller — controller log tail. The freezethaw.go
# post-state check + FATAL restore diagnostics land here; the prior canary's
# truncated-stderr observability gap (ka-0000001790) is why we grab the full tail.
gvisor_forensic_collect_controller() {
  local out="$1"
  {
    echo "# controller log tail (ns=${FX_CONTROLLER_NS}; selector=${FX_CONTROLLER_SELECTOR}; tail=${FX_CONTROLLER_TAIL})"
    _fx_section "kubectl logs -l ${FX_CONTROLLER_SELECTOR} --tail=${FX_CONTROLLER_TAIL}"
    _fx_kubectl -n "$FX_CONTROLLER_NS" logs -l "$FX_CONTROLLER_SELECTOR" --tail="$FX_CONTROLLER_TAIL" --prefix
  } >"$out" 2>&1 || _fx_log "controller collector wrote with errors (recorded in $out)"
  return 0
}

# gvisor_forensic_collect_markers — the full harvested FT_*/FT_RUNG_* marker set.
# FX_MARKERS is either a readable file path or a literal blob. We normalize it to
# the artifact verbatim AND filter the FT_-prefixed lines into a compact env tail
# so the marker set is greppable by R3/R4.
gvisor_forensic_collect_markers() {
  local out="$1"
  {
    echo "# harvested freeze/thaw markers (full FT_*/FT_RUNG_* set)"
    if [ -n "${FX_MARKERS:-}" ] && [ -f "${FX_MARKERS}" ]; then
      _fx_section "raw markers (from file ${FX_MARKERS})"
      cat "${FX_MARKERS}"
    elif [ -n "${FX_MARKERS:-}" ]; then
      _fx_section "raw markers (literal)"
      printf '%s\n' "${FX_MARKERS}"
    else
      echo "(FX_MARKERS unset — driver did not pass the harvested marker set)"
    fi
    _fx_section "FT_-prefixed lines only"
    # Read file / here-string (no `printf | grep` pipe) so a zero-match grep
    # exit under `set -o pipefail` reflects ONLY the grep, never a SIGPIPE on the
    # upstream printf — the `|| echo "(no FT_ lines found)"` then fires solely on
    # a genuine no-match. (This section is uncapped on purpose: 06-markers.env is
    # the complete machine-greppable set; the 60-line cap lives in the summary.)
    if [ -n "${FX_MARKERS:-}" ] && [ -f "${FX_MARKERS}" ]; then
      grep -E '^[[:space:]]*FT_' "${FX_MARKERS}" 2>/dev/null || echo "(no FT_ lines found)"
    elif [ -n "${FX_MARKERS:-}" ]; then
      grep -E '^[[:space:]]*FT_' <<<"${FX_MARKERS}" 2>/dev/null || echo "(no FT_ lines found)"
    fi
  } >"$out" 2>&1 || _fx_log "markers collector wrote with errors (recorded in $out)"
  return 0
}

# _fx_write_summary — the human-facing forensic-doc.md analog: failing rung +
# reason, canary context, links, and the FT_ marker table. Reproduces the shape
# of the hand-written forensic-doc.md (ka-0000007065) so the auto-bundle reads
# the same as the canary that made the class debuggable.
_fx_write_summary() {
  local out="$1" dir="$2"
  {
    echo "# gVisor freeze→thaw canary — forensic auto-capture"
    echo
    echo "**Failing rung:** \`${FX_RUNG:-<unspecified>}\`"
    echo "**Reason:** ${FX_REASON:-<unspecified>}"
    echo
    echo "Auto-captured by Rig R2 (lib-gvisor-forensics.sh, bead ka-0000006948) on the"
    echo "FIRST failing rung, BEFORE the HARD HOLD, while the failing-window evidence"
    echo "was still live (pre-GC). This reproduces the by-hand capture that made the"
    echo "prior canary debuggable (bundle ka-0000006433 / doc ka-0000007065)."
    echo
    echo "## Context"
    echo
    echo "| field | value |"
    echo "|---|---|"
    echo "| canary task (origin) | ${FX_ORIGIN_BEAD} |"
    echo "| namespace | ${FX_NS} |"
    echo "| Pod B | ${FX_POD_B:-<unset>} (uid ${FX_POD_B_UID:-<unset>}) |"
    echo "| Pod B netns | ${FX_BNETNS:-<unset>} |"
    echo "| runsc root | ${FX_RUNSC_ROOT} |"
    echo "| restore-target cid | ${FX_RESTORE_CID:-<unset>} |"
    echo "| controller | -n ${FX_CONTROLLER_NS} -l ${FX_CONTROLLER_SELECTOR} |"
    echo
    echo "## Evidence artifacts (this bundle)"
    echo
    echo "| # | artifact | what it shows |"
    echo "|---|---|---|"
    echo "| 1 | 01-netns.txt | Pod B netns \`ip addr\`/\`ip route\`/\`/proc/net/route\` — the netns_routable rung |"
    echo "| 2 | 02-runsc.txt | \`runsc list/state/ps\` + restore stderr + \`--debug-log\` tail — the restore_rc0 rung |"
    echo "| 3 | 03-cni.txt | \`/var/lib/cni/results\` listing + Pod B committed CNI result — the cni_confirmed rung |"
    echo "| 4 | 04-podb.txt | Pod B phase/conditions + events |"
    echo "| 5 | 05-controller.log | controller log tail (FATAL restore diagnostics land here) |"
    echo "| 6 | 06-markers.env | full harvested FT_*/FT_RUNG_* marker set |"
    echo
    echo "## Harvested FT_ markers"
    echo
    echo '```'
    # Cap at 60 lines with `grep -m 60` (NOT `grep | head -60`): the pipe form
    # lets head close the pipe early, SIGPIPE-killing grep (or the upstream
    # printf), which under `set -o pipefail` (the canary driver + unit-test shell
    # opts) propagates as a pipeline failure and spuriously fires the `|| echo
    # "(no FT_ lines found)"` fallback right after 60 real markers. `grep -m`
    # stops itself after 60 matches and exits 0, and reading a file / here-string
    # (no live pipe writer) cannot SIGPIPE — so the fallback fires ONLY on a
    # genuine zero-match.
    if [ -n "${FX_MARKERS:-}" ] && [ -f "${FX_MARKERS}" ]; then
      grep -E -m 60 '^[[:space:]]*FT_' "${FX_MARKERS}" 2>/dev/null || echo "(no FT_ lines found)"
    elif [ -n "${FX_MARKERS:-}" ]; then
      grep -E -m 60 '^[[:space:]]*FT_' <<<"${FX_MARKERS}" 2>/dev/null || echo "(no FT_ lines found)"
    else
      echo "(FX_MARKERS unset)"
    fi
    echo '```'
    echo
    echo "## Cross-references"
    echo
    echo "- Rig R2 (this capture): ka-0000006948"
    echo "- Rig R3 (files the next-layer bug, links this bundle): ka-0000004124"
    echo "- Rig R4 (bounded self-loop driver, invokes this on failure): ka-0000006231"
    echo "- Canary redrive task: ${FX_ORIGIN_BEAD}"
    echo "- Reference (hand-captured) forensic bundle: ka-0000006433"
    echo
    echo "_Local staging dir on the capturing host: ${dir}_"
  } >"$out" 2>&1 || _fx_log "summary write had errors (recorded in $out)"
  return 0
}

# ── the entrypoint ───────────────────────────────────────────────────────────

# gvisor_forensic_capture — stage all evidence then assemble a gb bundle.
#
# Sets FT_FORENSIC_DIR (whenever a staging dir exists), FT_FORENSIC_BUNDLE /
# FT_FORENSIC_URL (when the bundle was created). Prints those three as stdout
# sentinels. Returns 0 if a bundle was created, 1 if only the local dir could be
# staged (no daemon / gb absent) — in BOTH cases the raw evidence is preserved
# under FT_FORENSIC_DIR — or 2 if no writable staging dir could be created at all
# (nothing to preserve; FT_FORENSIC_DIR is empty). Callers should read the
# FT_FORENSIC_* sentinels, not branch on rc 0-vs-1 (1 is a normal degraded run).
gvisor_forensic_capture() {
  local dir
  dir="${FX_STAGE_DIR:-$(mktemp -d "${TMPDIR:-/tmp}/gvisor-forensic.XXXXXX" 2>/dev/null)}"
  mkdir -p "$dir" 2>/dev/null || true
  # The ONE hard invariant: a usable staging dir. If the chosen dir can't be
  # created/written (mktemp failed, FX_STAGE_DIR under a regular file, read-only
  # FS), fall back to a fresh mktemp; if THAT also fails, fail loudly with rc=2
  # rather than silently "succeeding" with a green, evidence-free bundle pointing
  # at a nonexistent dir. The capture's whole purpose is to preserve evidence —
  # no dir means there is nothing to preserve, so we must not pretend otherwise.
  if [ -z "$dir" ] || ! { [ -d "$dir" ] && [ -w "$dir" ]; }; then
    dir="$(mktemp -d "${TMPDIR:-/tmp}/gvisor-forensic.XXXXXX" 2>/dev/null || true)"
  fi
  if [ -z "$dir" ] || ! { [ -d "$dir" ] && [ -w "$dir" ]; }; then
    _fx_log "FATAL: could not create a writable forensic staging dir (FX_STAGE_DIR='${FX_STAGE_DIR:-}', TMPDIR='${TMPDIR:-/tmp}') — no evidence can be preserved"
    FT_FORENSIC_DIR=""
    FT_FORENSIC_BUNDLE=""
    FT_FORENSIC_URL=""
    printf 'FT_FORENSIC_DIR=\n'
    printf 'FT_FORENSIC_BUNDLE=\n'
    printf 'FT_FORENSIC_URL=\n'
    return 2
  fi
  FT_FORENSIC_DIR="$dir"
  FT_FORENSIC_BUNDLE=""
  FT_FORENSIC_URL=""

  _fx_log "capturing forensic evidence for rung='${FX_RUNG:-?}' reason='${FX_REASON:-?}' -> $dir"

  # Stage every artifact. Each collector is fail-soft and records its own errors;
  # we run them unconditionally so one missing source never loses the rest.
  _fx_write_summary               "$dir/00-summary.md"   "$dir"
  gvisor_forensic_collect_netns       "$dir/01-netns.txt"
  gvisor_forensic_collect_runsc       "$dir/02-runsc.txt"
  gvisor_forensic_collect_cni         "$dir/03-cni.txt"
  gvisor_forensic_collect_podb        "$dir/04-podb.txt"
  gvisor_forensic_collect_controller  "$dir/05-controller.log"
  gvisor_forensic_collect_markers     "$dir/06-markers.env"

  # Assemble the bundle. If gb is unavailable (no daemon, hermetic test), we keep
  # the local dir and return 1 so the caller still has the evidence on disk.
  # FX_GB is a SINGLE binary path OR a function name (same contract as FX_KUBECTL,
  # invoked below as `"$FX_GB" bundle …` — one quoted word). Probe it the same way
  # it is invoked: `command -v "$FX_GB"` (binary on PATH or abs path) OR a defined
  # function. Do NOT word-split FX_GB — a value with embedded flags is unsupported,
  # so the probe and the invocation agree on what "available" means.
  if ! command -v "$FX_GB" >/dev/null 2>&1 && [ "$(type -t "$FX_GB" 2>/dev/null)" != function ]; then
    _fx_log "gb unavailable ('${FX_GB}') — evidence staged at $dir but NOT bundled"
    printf 'FT_FORENSIC_DIR=%s\n' "$dir"
    printf 'FT_FORENSIC_BUNDLE=\n'
    printf 'FT_FORENSIC_URL=\n'
    return 1
  fi

  local title headline bid
  title="gVisor canary forensic auto-capture — rung ${FX_RUNG:-?} (${FX_REASON:-failure})"
  headline="Auto-captured on first failing rung before HARD HOLD (Rig R2)"

  # `gb bundle create` prints JSON; pull the id robustly (jq if present, else sed).
  local create_out
  create_out="$("$FX_GB" bundle create "$FX_ORIGIN_BEAD" --kind report \
      --title "$title" --headline "$headline" --json 2>&1)" || {
    _fx_log "gb bundle create failed: $create_out — evidence preserved at $dir"
    printf 'FT_FORENSIC_DIR=%s\n' "$dir"
    printf 'FT_FORENSIC_BUNDLE=\n'
    printf 'FT_FORENSIC_URL=\n'
    return 1
  }
  bid="$(_fx_extract_bundle_id "$create_out" "$FX_ORIGIN_BEAD")"
  if [ -z "$bid" ]; then
    _fx_log "could not parse bundle id from: $create_out — evidence preserved at $dir"
    printf 'FT_FORENSIC_DIR=%s\n' "$dir"
    printf 'FT_FORENSIC_BUNDLE=\n'
    printf 'FT_FORENSIC_URL=\n'
    return 1
  fi
  FT_FORENSIC_BUNDLE="$bid"
  FT_FORENSIC_URL="/bundle/${bid}"

  # Add each artifact in TOC order. Per-doc failures are logged but non-fatal —
  # a partial bundle still beats no bundle.
  local pos=0 f fmt
  for f in 00-summary.md 01-netns.txt 02-runsc.txt 03-cni.txt 04-podb.txt 05-controller.log 06-markers.env; do
    [ -f "$dir/$f" ] || continue
    case "$f" in
      *.md)  fmt=md ;;
      *)     fmt=code ;;
    esac
    if ! "$FX_GB" bundle add "$bid" --doc "$dir/$f" --format "$fmt" --position "$pos" >/dev/null 2>&1; then
      _fx_log "WARN: gb bundle add failed for $f (continuing)"
    fi
    pos=$((pos + 1))
  done

  _fx_log "forensic bundle assembled: ${FT_FORENSIC_BUNDLE} (${FT_FORENSIC_URL})"
  printf 'FT_FORENSIC_DIR=%s\n' "$dir"
  printf 'FT_FORENSIC_BUNDLE=%s\n' "$FT_FORENSIC_BUNDLE"
  printf 'FT_FORENSIC_URL=%s\n' "$FT_FORENSIC_URL"
  return 0
}

# _fx_extract_bundle_id — pull the NEW bundle bead id (kd-…) from `gb bundle
# create` output.
#
#   _fx_extract_bundle_id <blob> [exclude-id]
#
# Prefers jq over the structured keys. If jq is absent OR the output is not clean
# JSON (e.g. a stderr log line folded in via the create call's 2>&1), it falls
# back to a grep for a kd-… token — but EXCLUDES <exclude-id> (the origin/parent
# bead, which `gb bundle create <origin> …` takes as its first arg and may echo
# back) so the fallback never mistakes the parent for the new bundle. Among the
# remaining tokens it takes the LAST one: `gb` create output conventionally
# prints the parent (if at all) before the freshly-minted child id.
_fx_extract_bundle_id() {
  local blob="$1" exclude="${2:-}" id=""
  if command -v jq >/dev/null 2>&1; then
    id="$(printf '%s' "$blob" | jq -r '.id // .ID // .bundle.id // .bundle.ID // .bead.id // .beadId // empty' 2>/dev/null | head -1)"
  fi
  if [ -z "$id" ]; then
    local toks
    toks="$(printf '%s' "$blob" | grep -oE '(gd|ka)-[A-Za-z0-9]+')"
    if [ -n "$exclude" ]; then
      toks="$(printf '%s\n' "$toks" | grep -vx "$exclude" || true)"
    fi
    # Last non-excluded token: the new child id is printed after any parent ref.
    id="$(printf '%s\n' "$toks" | grep -E '^(gd|ka)-' | tail -1)"
  fi
  printf '%s' "$id"
}
