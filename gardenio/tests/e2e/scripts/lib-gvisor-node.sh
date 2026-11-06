#!/usr/bin/env bash
# SC2034: GVISOR_*_DIAG / _ATTEMPTS are an inter-script return channel (set here,
#         read by the sourcing module) — not unused.
# shellcheck disable=SC2034
# lib-gvisor-node.sh — shared hardening helpers for the dedicated gVisor PoC
# substrate E2E modules (test-gvisor-*.sh, test-resume-latency.sh).
#
# WHY THIS EXISTS (decision ka-0000000122, chosen=harden; root flake ka-0000007234):
#   The 5 dedicated gardenio-gvisor-* dispatches HARD-FAIL on the gvisor-poc node,
#   but they fail INTERMITTENTLY on a rotating set of node-side signatures:
#     node-asset-ship / thaw-state-FS / restore-into-B / FREEZE-checkpoint /
#     Pod-B-netns-resolve.
#   The common cause is Karpenter cold-start: a freshly provisioned gvisor=poc
#   node reports its privileged probe pod Ready while the runsc install +
#   containerd-shim registration (nodeadm NodeConfig userdata, escalation
#   ka-0000004674) is still settling. A single node-side step fired in that
#   early-boot window can miss — `runsc --version` returns empty, a base64 copy
#   to /var/tmp races a not-yet-writable tmpfs, the runsc state root isn't
#   reachable yet — even though seconds later the same call SUCCEEDS (the exact
#   .22-vs-.21 transient in ka-0000007698).
#
#   This lib turns those one-shot node-side steps into:
#     1. gvisor_warm_node_gate  — a bounded readiness poll that asserts the node
#        is actually WARM (runsc present + executable, /var/tmp writable, runsc
#        state root reachable) before any real work, rather than trusting the
#        pod's Ready condition.
#     2. gvisor_retry_step      — a bounded, signature-keyed retry wrapper for the
#        individual flaky node-side steps, so a single cold-start miss self-heals
#        instead of red-ing the dedicated dispatch.
#
#   Both are deliberately BOUNDED: they ride out a transient cold-start, but a
#   genuine regression (missing binary, broken FS, real protocol break) still
#   exhausts the budget and HARD-FAILS with captured diagnostics — these helpers
#   never silently pass.
#
# USAGE:
#   # shellcheck disable=SC1090
#   source "$(dirname "${BASH_SOURCE[0]}")/lib-gvisor-node.sh"
#
#   # 1. After the probe pod is Ready, gate on the node being genuinely warm.
#   #    node_runner is YOUR module's node-exec function: it takes a wall-clock
#   #    cap (seconds) + a command string and runs it in the host chroot, e.g.
#   #    test-resume-latency.sh's node_exec or freeze-thaw's node_run.
#   if gvisor_warm_node_gate node_exec; then
#     pass "gvisor-poc node warm (runsc present, /var/tmp writable, runsc-root reachable)"
#   else
#     fail "gvisor-poc node warm-gate" "$GVISOR_WARM_DIAG"
#   fi
#
#   # 2. Wrap a flaky node-side step in a bounded, signature-keyed retry.
#   if gvisor_retry_step node-asset-ship 4 3 ship_assets_to_node; then
#     pass "node assets shipped"
#   else
#     fail "ship node assets" "$GVISOR_STEP_DIAG"
#   fi
#
# TUNABLES (override via env before sourcing, or per-call args):
#   GVISOR_WARM_ATTEMPTS   warm-gate poll attempts (default 12)
#   GVISOR_WARM_DELAY      seconds between warm-gate attempts (default 3)
#   GVISOR_WARM_CAP        per-probe node-exec wall-clock cap, seconds (default 30)
#   GVISOR_RETRY_ATTEMPTS  default step-retry attempts (default 3)
#   GVISOR_RETRY_DELAY     default seconds between step retries (default 3)
#   RUNTIME_CLASS          runsc handler binary name to probe (default runsc)
#   GVISOR_WARM_REQUIRE_ROOT  assert the k8s runsc state root in the warm gate
#                          (default 1). Set 0 for modules that build their OWN
#                          private runsc root (own --root=, no k8s runsc pod) —
#                          freeze-thaw / armed-guard — where the containerd
#                          /run/containerd/runsc/k8s.io dir is legitimately ABSENT
#                          on a fresh dedicated node and would false-negative the
#                          gate. Modules that schedule a k8s runsc pod
#                          (runtimeClassName: runsc) — rehydrate-ack /
#                          resume-latency / rehydration — keep the default 1.

# Guard against multiple sourcing within the same shell.
# shellcheck disable=SC2317
if [ -n "${__GARDENIO_LIB_GVISOR_NODE_LOADED:-}" ]; then
  return 0 2>/dev/null || true
fi
__GARDENIO_LIB_GVISOR_NODE_LOADED=1

: "${GVISOR_WARM_ATTEMPTS:=12}"
: "${GVISOR_WARM_DELAY:=3}"
: "${GVISOR_WARM_CAP:=30}"
: "${GVISOR_WARM_REQUIRE_ROOT:=1}"
: "${GVISOR_RETRY_ATTEMPTS:=3}"
: "${GVISOR_RETRY_DELAY:=3}"

# Diagnostics from the most recent helper call (read by callers on failure so a
# genuine regression is debuggable, not a bare "<empty>"). These are the lib's
# return channel — set here and inside the functions, READ by the sourcing
# module, so shellcheck's "appears unused" (SC2034) is a false positive; the
# file-level directive below silences it.
GVISOR_WARM_DIAG=""
GVISOR_STEP_DIAG=""
# Number of attempts the most recent gvisor_retry_step actually consumed (1 ==
# first try; >1 == it rode out a transient). Lets a module log self-heals.
GVISOR_STEP_ATTEMPTS=0

# gvisor_retry_step — run a node-side step with a bounded, signature-keyed retry.
#
#   gvisor_retry_step <signature> <attempts> <delay> <cmd> [cmd-args...]
#
# Runs `<cmd> [args...]`; on non-zero exit it retries up to <attempts> total
# (incl. the first try), sleeping <delay> seconds between tries. <signature> is a
# short stable label for the failure family (node-asset-ship, FREEZE-checkpoint,
# restore-into-B, Pod-B-netns-resolve, thaw-state-FS) — it is echoed on each
# retry and folded into GVISOR_STEP_DIAG so a triager can see WHICH node-side
# step was flaky. Returns the final exit code; on exhaustion GVISOR_STEP_DIAG
# carries the signature + attempt count.
#
# Pass 0 (or empty) for attempts/delay to fall back to the GVISOR_RETRY_* env
# defaults. The command is invoked via its argv (no eval) so callers should pass
# a function name + args, not a shell string.
gvisor_retry_step() {
  local sig="$1"; shift
  local attempts="${1:-0}"; shift || true
  local delay="${1:-0}"; shift || true
  [ -z "$attempts" ] || [ "$attempts" -lt 1 ] 2>/dev/null && attempts="${GVISOR_RETRY_ATTEMPTS}"
  [ -z "$delay" ] 2>/dev/null && delay="${GVISOR_RETRY_DELAY}"
  local i rc=0
  GVISOR_STEP_DIAG=""
  GVISOR_STEP_ATTEMPTS=0
  for (( i = 1; i <= attempts; i++ )); do
    GVISOR_STEP_ATTEMPTS="$i"
    "$@"
    rc=$?
    if [ "$rc" -eq 0 ]; then
      return 0
    fi
    if [ "$i" -lt "$attempts" ]; then
      printf '    [gvisor-retry] step=%s attempt=%d/%d rc=%d — transient? retrying in %ds\n' \
        "$sig" "$i" "$attempts" "$rc" "$delay" >&2
      sleep "$delay"
    fi
  done
  GVISOR_STEP_DIAG="step '${sig}' failed after ${attempts} attempts (last rc=${rc}) — exhausted the cold-start retry budget; treat as a genuine substrate regression"
  return "$rc"
}

# gvisor_warm_node_gate — bounded poll that the gvisor-poc node is genuinely WARM
# before any real freeze/thaw work runs.
#
#   gvisor_warm_node_gate <node_runner_fn> [attempts] [delay]
#
# <node_runner_fn> is the module's node-exec function — it MUST take a wall-clock
# cap (seconds) as $1 and a command string as $2 and run that command in the host
# chroot on the gvisor node, printing its stdout. (Both node-exec idioms in the
# tree are compatible: test-resume-latency.sh / test-gvisor-rehydrate-ack.sh use
# `node_exec cap "cmd"`; for the stdin-streaming `node_run cap <<<script` idiom
# (freeze-thaw / armed-guard / rehydration), call gvisor_warm_gate_via_run below,
# which wraps node_run in the 2-arg shim for you.)
#
# Asserts, polling up to <attempts> times with <delay> between:
#   (a) runsc handler binary present + reports a version (executable, PID-ns
#       reachable via nsenter -t 1) — the real handler, not just the RuntimeClass;
#   (b) /var/tmp on the node is writable (the base64-ship target; a not-yet-mounted
#       tmpfs is the ka-0000007234 node-asset-ship signature);
#   (c) the containerd/runsc state root dir exists (restore-into-B / netns-resolve
#       walk it). This check is SKIPPED when GVISOR_WARM_REQUIRE_ROOT=0 — modules
#       that build their OWN private runsc root (freeze-thaw / armed-guard: own
#       --root= under /tmp/gvisor-*, no k8s runsc pod) never populate
#       /run/containerd/runsc/k8s.io, so asserting it there would false-negative
#       the gate on a fresh dedicated node. Modules that schedule a k8s runsc pod
#       keep it (default 1).
# Returns 0 once the enabled checks hold; on exhaustion returns 1 and sets
# GVISOR_WARM_DIAG to the first still-failing check + its captured output.
gvisor_warm_node_gate() {
  local runner="$1"
  local attempts="${2:-$GVISOR_WARM_ATTEMPTS}"
  local delay="${3:-$GVISOR_WARM_DELAY}"
  local rc="${RUNTIME_CLASS:-runsc}"
  local require_root="${GVISOR_WARM_REQUIRE_ROOT:-1}"
  local i ver tmp_ok root_ok root_hold last=""
  GVISOR_WARM_DIAG=""
  for (( i = 1; i <= attempts; i++ )); do
    # (a) runsc present + executable in the host init namespaces.
    ver="$("$runner" "$GVISOR_WARM_CAP" "nsenter -t 1 -m -p -- ${rc} --version 2>&1 | head -1" 2>/dev/null | tr -d '\r')"
    # (b) /var/tmp writable (the base64-ship target). $$/$d MUST expand on the
    # node, not here — single quotes are intentional (SC2016 disabled).
    # shellcheck disable=SC2016
    tmp_ok="$("$runner" "$GVISOR_WARM_CAP" 'd=/var/tmp/.gvisor-warm-probe.$$; ( touch "$d" && rm -f "$d" && echo WRITABLE ) 2>/dev/null' 2>/dev/null | tr -d '\r')"
    # (c) the runsc / containerd state root is present (k8s-runsc-pod modules only).
    if [ "$require_root" = 1 ]; then
      root_ok="$("$runner" "$GVISOR_WARM_CAP" '[ -d /run/containerd/runsc/k8s.io ] && echo ROOT_OK || echo ROOT_MISSING' 2>/dev/null | tr -d '\r')"
      [ "$root_ok" = ROOT_OK ] && root_hold=1 || root_hold=0
    else
      root_ok=SKIPPED; root_hold=1
    fi

    if grep -q 'runsc version' <<<"$ver" \
       && [ "$tmp_ok" = WRITABLE ] \
       && [ "$root_hold" = 1 ]; then
      GVISOR_WARM_DIAG=""
      return 0
    fi
    # Record the first still-failing check for diagnostics on the final attempt.
    if ! grep -q 'runsc version' <<<"$ver"; then
      last="runsc handler not ready: '${rc} --version' => '${ver:-<empty>}'"
    elif [ "$tmp_ok" != WRITABLE ]; then
      last="/var/tmp not writable yet on node (tmpfs settling?): '${tmp_ok:-<empty>}'"
    else
      last="runsc state root /run/containerd/runsc/k8s.io absent: '${root_ok:-<empty>}'"
    fi
    if [ "$i" -lt "$attempts" ]; then
      printf '    [gvisor-warm] attempt=%d/%d not warm yet (%s) — retrying in %ds\n' \
        "$i" "$attempts" "$last" "$delay" >&2
      sleep "$delay"
    fi
  done
  GVISOR_WARM_DIAG="node not warm after ${attempts} attempts (${last}); the gvisor-poc node likely cold-started under Karpenter and never finished staging runsc — provision/keep-warm the gvisor=poc pool (escalation ka-0000004674)"
  return 1
}

# gvisor_warm_gate_via_run — adapt a STDIN-STREAMING node runner (the `node_run
# <cap>` idiom in freeze-thaw / armed-guard / rehydration, which reads the script
# body from stdin) to gvisor_warm_node_gate's 2-arg `(cap, cmd-string)` contract.
#
#   gvisor_warm_gate_via_run <node_run_fn> [attempts] [delay]
#
# It defines a private 2-arg shim that pipes the command string into <node_run_fn>
# over stdin, then calls gvisor_warm_node_gate with that shim. Use it instead of
# hand-rolling a shim in each module. GVISOR_WARM_DIAG is set exactly as by the
# underlying gate. (These modules build their own private runsc root, so they set
# GVISOR_WARM_REQUIRE_ROOT=0 before calling — check (c) is skipped there.)
gvisor_warm_gate_via_run() {
  local run_fn="$1"; shift
  # shellcheck disable=SC2329  # invoked indirectly by name via gvisor_warm_node_gate
  __gvisor_run_shim() { "$run_fn" "$1" <<< "$2"; }
  gvisor_warm_node_gate __gvisor_run_shim "$@"
}

# gvisor_require_substrate_guard — the readiness-gate decision for a substrate
# that is ABSENT (no runsc RuntimeClass / no create-pods / not warm). This is the
# single shared answer to the question "what happens when the gvisor-poc
# substrate is missing at test time?", so every gVisor module makes the SAME
# choice instead of each open-coding `exit 0`.
#
#   gvisor_require_substrate_guard <module> <why>
#
# It reads the REQUIRE_SUBSTRATE env var and decides the exit:
#
#   REQUIRE_SUBSTRATE unset/0  (DEFAULT — backward compatible):
#       prints a LOUD blue SKIP banner and EXITS 0. An absent/torn-down node pool
#       must NOT red the per-feature dispatches or the golden gate — the
#       long-standing operator mandate (ka-0000000847 / ka-0000003662). A skip is
#       made visible (loud) but is never a hard failure.
#
#   REQUIRE_SUBSTRATE=1  (the READINESS GATE — task ka-0000002867):
#       prints a LOUD red NO-GO banner and EXITS 1 (hard fail). The production-
#       readiness go/no-go (epic ka-0000004667, exit criterion 4) is the signal
#       that gates flipping FreezeThawEnabled for real agents. It CANNOT rest on a
#       test that silently skipped — "substrate absent" under the gate is itself a
#       NO-GO, because the gate's whole job is to prove the substrate stands up and
#       the gVisor lifecycle actually runs. So here an absent substrate fails the
#       gate loudly rather than passing green.
#
# This function ALWAYS exits the calling script — it never returns. Modules call
# it in place of their old `exit 0` on the substrate-absent path.
gvisor_require_substrate_guard() {
  local module="$1" why="$2"
  if [ "${REQUIRE_SUBSTRATE:-0}" = "1" ]; then
    printf '\033[0;31m%s\033[0m\n' "════════════════════════════════════════════════════"
    printf '\033[0;31m%s\033[0m\n' " NO-GO ${module} (REQUIRE_SUBSTRATE=1): ${why}"
    printf '\033[0;31m%s\033[0m\n' " This is the gVisor PRODUCTION-READINESS GATE (task ka-0000002867,"
    printf '\033[0;31m%s\033[0m\n' " epic ka-0000004667). The gate must PROVE the gvisor-poc substrate"
    printf '\033[0;31m%s\033[0m\n' " stands up and the freeze/thaw lifecycle actually runs — a silent"
    printf '\033[0;31m%s\033[0m\n' " SKIP is NOT a pass here. An absent/cold substrate is a NO-GO:"
    printf '\033[0;31m%s\033[0m\n' " do NOT flip FreezeThawEnabled for real agents on this signal."
    printf '\033[0;31m%s\033[0m\n' " Provision/keep-warm the gvisor=poc node pool + runsc RuntimeClass"
    printf '\033[0;31m%s\033[0m\n' " (escalation ka-0000004674) and re-run the readiness dispatch."
    printf '\033[0;31m%s\033[0m\n' "════════════════════════════════════════════════════"
    exit 1
  fi
  printf '\033[0;34m%s\033[0m\n' "════════════════════════════════════════════════════"
  printf '\033[0;34m%s\033[0m\n' " SKIP ${module} (substrate absent): ${why}"
  printf '\033[2m%s\033[0m\n'    "  Opt-in module: an absent/torn-down gvisor-poc pool is a clean skip"
  printf '\033[2m%s\033[0m\n'    "  here (ka-0000000847 / ka-0000003662) — it must NOT red this dispatch."
  printf '\033[2m%s\033[0m\n'    "  To make absence a HARD FAILURE (the readiness go/no-go), re-run with"
  printf '\033[2m%s\033[0m\n'    "  REQUIRE_SUBSTRATE=1 (dispatch gardenio-gvisor-readiness)."
  if [ -n "${E2E_SKIP_SENTINEL:-}" ]; then printf '%s\n' "$*" >"$E2E_SKIP_SENTINEL" 2>/dev/null || true; fi  # skip visibility (ka-0000000989)
  exit 0
}
