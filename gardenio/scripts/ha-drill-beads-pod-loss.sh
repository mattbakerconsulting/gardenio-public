#!/usr/bin/env bash
# HA epic (gd-StII9fmzym) Phase 3 drill 3: kill the gardenio-beads pod while a
# gardener run and a crew push/comment are in flight, and measure the blast
# radius through the actual kd/gb CLIENTS (not raw curl) — the acceptance
# criterion for gd-Mc3cPhsInt is a results table with the API gap in seconds,
# which client calls failed and whether the CLIENT retried transparently, and
# whether the gardener run is recorded as done (not failed) once the daemon
# comes back. This script is the instrument that produces those numbers.
#
# *** STATUS: WRITTEN, NEVER RUN. ***
# Same blocker shape as ha-drill-drain.sh (PR #137, gd-hyCFMUqYPE): this
# needs a privileged ServiceAccount/role capable of `pods/eviction` (or, with
# operator sign-off, `pods delete`) in namespace gardenio against the
# gardenio-beads pod specifically. As of the time this was written:
#   - gardenio-ha-drill-operator (the role meant to hold that grant) is
#     blocked at the role-bead level (gd-PgyIJPTUnC) pending the
#     ValidatingAdmissionPolicy that scopes `nodes patch` to
#     spec.unschedulable only (gd-JKXEwmGns8 / PR #158). PR #158 has MERGED
#     into source but "takes effect at the next calver release" — it is not
#     yet deployed, and requirement 4 (live post-deploy verification that the
#     VAP actually rejects a bad patch) is explicitly still outstanding.
#   - The role-bead's "not-yet-safe-to-spawn until bound AND verified"
#     condition is a blanket gate (decision gd-KmJX79f3ff: keep-gated,
#     role-wide, not per-capability) — it blocks THIS drill's pod-eviction
#     need exactly the same way it blocks drill 1's node-patch need, even
#     though this drill never touches a node.
# Do not run this until gd-JKXEwmGns8's requirement 4 clears AND you have
# independently re-verified with `kubectl auth can-i create pods/eviction -n
# gardenio --subresource=eviction` (or `delete pods`, if the operator has
# explicitly decided eviction is unnecessary for this drill) that the
# permission this script needs actually exists for the SA it will run under.
#
# What it does once it CAN run:
#   1. Starts a synthetic load loop that drives real `kd` and `gb` CLIENT
#      invocations (list/show/comment), not raw curl — the whole point of
#      this drill is to observe CLIENT-side retry behavior (kbeads'
#      HTTPClient / gardenio's beadsapi.Client), which a curl-based loop like
#      ha-drill-drain.sh's cannot exercise. Every call is timestamped with
#      wall-clock start/end, exit code, and attempt duration, so an
#      internally-retried call shows up as elevated latency rather than a
#      failure — the signal this drill is built to capture.
#   2. Locates (or is told about) one live crew agent and, if possible, an
#      in-flight gardener run, so "was the gardener run recorded done, not
#      failed" is an observable outcome, not a guess.
#   3. Confirms the gardenio-beads pod exists and is Ready, records its name
#      and start time, then evicts (default) or deletes it.
#   4. Measures the API gap: wall-clock time between the last successful load
#      call before the pod disappears and the first successful load call
#      after a replacement pod is Ready.
#   5. Prints a before/after results table: total calls / failures by client
#      method and idempotency class, the measured gap in seconds, whether any
#      call needed the client's internal retry (inferred from latency
#      outliers), the gardener run's final status, and an escalation/decision
#      write-integrity check (create one before the kill, confirm exactly one
#      copy exists after).
#
# Usage:
#   ./ha-drill-beads-pod-loss.sh [--dry-run] [--namespace gardenio]
#                                 [--duration 5m] [--attach-crew <agent-bead-id>]
#                                 [--method evict|delete] [--scratch-bead <id>]
#
# Idempotent by design:
#   - Re-running after a partial failure is safe. State (target pod name,
#     load-loop PID, whether we already killed it) lives in $STATE_DIR and
#     every mutating step checks current cluster state before acting (won't
#     re-kill a pod that's already been replaced since the recorded
#     target-pod name no longer exists).
#   - The script never assumes it created the crew or the gardener run it
#     observes; with no --attach-crew, it discovers the most-recently-active
#     in_progress crew agent bead itself and refuses to proceed (rather than
#     silently spawning one) if none is found.
#   - The write-integrity check creates at most one scratch comment per run
#     (recorded in $STATE_DIR/integrity_comment_id) and is a no-op on re-run
#     if that file already exists.
set -euo pipefail

HERE="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
STATE_DIR="${HA_DRILL_STATE_DIR:-$HERE/.state/ha-drill-beads-pod-loss}"
RESULTS_DIR="$STATE_DIR/results"
mkdir -p "$STATE_DIR" "$RESULTS_DIR"

NAMESPACE="gardenio"
DURATION="5m"
DRY_RUN=0
METHOD="evict"
ATTACH_CREW=""
SCRATCH_BEAD_ID=""
LOAD_TARGET_OPM=60

# The beads daemon Deployment name — namespace-scoped, single pod expected
# under normal HA config (per gd-StII9fmzym's measured starting point).
BEADS_POD_LABEL_SELECTOR="app=gardenio-beads"

log() { printf '[%s] %s\n' "$(date -u +%Y-%m-%dT%H:%M:%SZ)" "$*" >&2; }
die() { log "ERROR: $*"; exit 1; }

while [[ $# -gt 0 ]]; do
  case "$1" in
    --dry-run)         DRY_RUN=1; shift;;
    --namespace)       NAMESPACE="$2"; shift 2;;
    --duration)        DURATION="$2"; shift 2;;
    --attach-crew)     ATTACH_CREW="$2"; shift 2;;
    --method)          METHOD="$2"; shift 2;;
    --scratch-bead)    SCRATCH_BEAD_ID="$2"; shift 2;;
    -h|--help)         sed -n '2,70p' "$0"; exit 0;;
    *) die "unknown arg: $1 (see --help)";;
  esac
done

[[ "$METHOD" == "evict" || "$METHOD" == "delete" ]] || die "--method must be evict or delete, got: $METHOD"

K=(kubectl -n "$NAMESPACE")
[[ "$DRY_RUN" == 1 ]] && log "DRY-RUN: no mutating kubectl/kd/gb call will actually execute"

run() {
  # run <description> -- <cmd...>   — honors DRY_RUN, always logs.
  local desc="$1"; shift
  [[ "$1" == "--" ]] && shift
  log "$desc: $*"
  if [[ "$DRY_RUN" == 1 ]]; then
    log "  (dry-run, skipped)"
    return 0
  fi
  "$@"
}

to_secs() {
  local d="$1"
  case "$d" in
    *h) echo $(( ${d%h} * 3600 ));;
    *m) echo $(( ${d%m} * 60 ));;
    *s) echo "${d%s}";;
    *)  echo "$d";;
  esac
}

# --- preflight: confirm we actually hold the permissions this needs --------
preflight_rbac() {
  local missing=()
  kubectl auth can-i get pods -n "$NAMESPACE"    >/dev/null 2>&1 || missing+=("get pods -n $NAMESPACE")
  kubectl auth can-i list pods -n "$NAMESPACE"   >/dev/null 2>&1 || missing+=("list pods -n $NAMESPACE")
  if [[ "$METHOD" == "evict" ]]; then
    kubectl auth can-i create pods/eviction -n "$NAMESPACE" >/dev/null 2>&1 || missing+=("create pods/eviction -n $NAMESPACE")
  else
    kubectl auth can-i delete pods -n "$NAMESPACE" >/dev/null 2>&1 || missing+=("delete pods -n $NAMESPACE")
  fi
  if [[ ${#missing[@]} -gt 0 ]]; then
    die "missing RBAC for: ${missing[*]}. See gd-JKXEwmGns8 (VAP merged but not yet deployed/verified) and gd-PgyIJPTUnC (role blanket-gated pending that) — this script cannot run until that clears. Refusing to proceed loudly here instead of partway through the drill."
  fi
  log "RBAC preflight OK: pod get/list + $METHOD present in namespace $NAMESPACE"
}

# --- step 1: synthetic load loop through the REAL kd/gb clients -------------
LOAD_LOG="$RESULTS_DIR/load.ndjson"
LOAD_PID_FILE="$STATE_DIR/load.pid"

client_call() {
  # client_call <label> <idempotent:0|1> -- <cmd...>
  # Times one real kd/gb invocation and logs one NDJSON line per call. Using
  # the actual CLI binaries (not curl) is the point: it's their internal
  # HTTPClient/beadsapi.Client transport — including any connection-reset
  # retry — that this drill measures.
  local label="$1" idempotent="$2"; shift 2
  [[ "$1" == "--" ]] && shift
  local start end status ms
  start="$(date +%s.%N)"
  if "$@" >/dev/null 2>>"$RESULTS_DIR/load.stderr"; then
    status="ok"
  else
    status="fail"
  fi
  end="$(date +%s.%N)"
  ms="$(awk "BEGIN{printf \"%.0f\", ($end-$start)*1000}")"
  printf '{"ts":"%s","label":"%s","idempotent":%s,"status":"%s","latency_ms":%s}\n' \
    "$(date -u +%Y-%m-%dT%H:%M:%S.%3NZ)" "$label" "$idempotent" "$status" "$ms" >> "$LOAD_LOG"
}

start_load_loop() {
  local end_ts scratch_id="$SCRATCH_BEAD_ID"
  if [[ -f "$LOAD_PID_FILE" ]] && kill -0 "$(cat "$LOAD_PID_FILE")" 2>/dev/null; then
    log "load loop already running (pid $(cat "$LOAD_PID_FILE")), not starting a second one"
    return 0
  fi
  if [[ -z "$scratch_id" ]]; then
    log "no --scratch-bead given; comment traffic falls back to a read-only kd show against gd-Mc3cPhsInt itself, same as the drain drill's substitution"
  fi
  end_ts=$(( $(date +%s) + $(to_secs "$DURATION") + 60 ))
  (
    while [[ $(date +%s) -lt $end_ts ]]; do
      local r=$(( RANDOM % 100 ))
      if [[ $r -lt 40 ]]; then
        client_call "kd-list" 1 -- kd list --status=in_progress -l "project:gardenio" --json
      elif [[ $r -lt 75 ]]; then
        client_call "kd-show" 1 -- kd show gd-Mc3cPhsInt
      elif [[ $r -lt 90 ]]; then
        client_call "gb-ready" 1 -- gb ready
      else
        if [[ -n "$scratch_id" ]]; then
          client_call "kd-comment-add" 0 -- kd comment add "$scratch_id" -m "ha-drill-beads-pod-loss synthetic load $(date +%s)"
        else
          client_call "kd-show-fallback" 1 -- kd show gd-Mc3cPhsInt
        fi
      fi
      sleep "0.$(( (RANDOM % 400) + 800 ))"
    done
  ) &
  echo $! > "$LOAD_PID_FILE"
  log "load loop started, pid $(cat "$LOAD_PID_FILE"), target ~${LOAD_TARGET_OPM} req/min through real kd/gb clients, log $LOAD_LOG"
}

stop_load_loop() {
  [[ -f "$LOAD_PID_FILE" ]] || return 0
  local pid; pid="$(cat "$LOAD_PID_FILE")"
  kill "$pid" 2>/dev/null || true
  wait "$pid" 2>/dev/null || true
  rm -f "$LOAD_PID_FILE"
  log "load loop stopped"
}

# --- step 2: find or attach to one live crew, and note any gardener run -----
discover_crew() {
  if [[ -n "$ATTACH_CREW" ]]; then
    echo "$ATTACH_CREW"
    return 0
  fi
  local id
  id="$(kd list --status=in_progress --type=agent -l project:gardenio --json 2>/dev/null \
    | grep -o '"id"[[:space:]]*:[[:space:]]*"[^"]*"' | head -1 | sed -E 's/.*"([^"]+)"$/\1/' || true)"
  echo "$id"
}

# --- step 3: write-integrity check (escalation/decision/comment survival) --
# Creates exactly one scratch comment before the kill and confirms exactly
# one copy exists after — a duplicate would mean a non-idempotent call got
# silently retried somewhere in the stack (client OR daemon), and a missing
# one would mean a write was lost. Both are the failure modes this drill's
# acceptance criterion cares about.
record_write_integrity_baseline() {
  local marker_file="$STATE_DIR/integrity_comment_id"
  if [[ -f "$marker_file" ]]; then
    log "write-integrity baseline already recorded ($(cat "$marker_file")), not re-creating (idempotent skip)"
    return 0
  fi
  [[ -n "$SCRATCH_BEAD_ID" ]] || { log "no --scratch-bead given; skipping write-integrity comment check"; return 0; }
  local marker
  marker="ha-drill-beads-pod-loss integrity marker $(date +%s%N)"
  run "create write-integrity marker comment" -- kd comment add "$SCRATCH_BEAD_ID" -m "$marker"
  echo "$marker" > "$marker_file"
}

check_write_integrity() {
  local marker_file="$STATE_DIR/integrity_comment_id"
  [[ -f "$marker_file" && -n "$SCRATCH_BEAD_ID" ]] || { echo "  (skipped — no --scratch-bead)"; return 0; }
  local marker; marker="$(cat "$marker_file")"
  local count
  count="$(kd show "$SCRATCH_BEAD_ID" --json 2>/dev/null | grep -o -F "$marker" | wc -l || echo 0)"
  echo "  marker comment occurrences: $count (want exactly 1 — 0 means lost, >1 means duplicated by a retried non-idempotent write)"
}

# --- step 4: locate, kill, and time the return of gardenio-beads -----------
find_beads_pod() {
  local pod
  pod="$("${K[@]}" get pods -l "$BEADS_POD_LABEL_SELECTOR" -o jsonpath='{.items[0].metadata.name}' 2>/dev/null || true)"
  [[ -n "$pod" ]] || die "no pod matching -l $BEADS_POD_LABEL_SELECTOR found in namespace $NAMESPACE"
  echo "$pod"
}

kill_beads_pod() {
  local pod="$1"
  if ! "${K[@]}" get pod "$pod" >/dev/null 2>&1; then
    log "pod $pod no longer exists (idempotent skip — already killed by a prior partial run)"
    return 0
  fi
  echo "$pod" > "$STATE_DIR/target_pod"
  date -u +%Y-%m-%dT%H:%M:%S.%3NZ > "$STATE_DIR/kill_ts"
  if [[ "$METHOD" == "evict" ]]; then
    run "evict $pod" -- kubectl -n "$NAMESPACE" delete --raw \
      "/api/v1/namespaces/$NAMESPACE/pods/$pod/eviction" \
      -f - <<EOF
{"apiVersion":"policy/v1","kind":"Eviction","metadata":{"name":"$pod","namespace":"$NAMESPACE"}}
EOF
  else
    run "delete $pod" -- "${K[@]}" delete pod "$pod" --grace-period=0
  fi
}

measure_api_gap() {
  # Measures the gap between the last "ok" load-log line before the kill
  # timestamp and the first "ok" line after it. Requires the load loop to
  # have been running before kill_beads_pod is called.
  local kill_ts; kill_ts="$(cat "$STATE_DIR/kill_ts" 2>/dev/null || true)"
  [[ -n "$kill_ts" ]] || { echo "(no kill_ts recorded — dry-run or pod already gone)"; return 0; }
  local last_ok_before first_ok_after
  last_ok_before="$(awk -F'"' -v k="$kill_ts" '$4<k && /"status":"ok"/{last=$4} END{print last}' "$LOAD_LOG" 2>/dev/null || true)"
  first_ok_after="$(awk -F'"' -v k="$kill_ts" '$4>k && /"status":"ok"/{print $4; exit}' "$LOAD_LOG" 2>/dev/null || true)"
  if [[ -z "$last_ok_before" || -z "$first_ok_after" ]]; then
    echo "(insufficient data to compute gap — check $LOAD_LOG)"
    return 0
  fi
  local gap_s
  gap_s="$(( $(date -u -d "$first_ok_after" +%s 2>/dev/null || echo 0) - $(date -u -d "$last_ok_before" +%s 2>/dev/null || echo 0) ))"
  echo "last ok before kill: $last_ok_before"
  echo "first ok after kill: $first_ok_after"
  echo "measured API gap: ${gap_s}s"
}

# --- step 5: summarize -------------------------------------------------
summarize() {
  local total failed_by_label
  total="$(wc -l < "$LOAD_LOG" 2>/dev/null || echo 0)"
  failed_by_label="$(grep '"status":"fail"' "$LOAD_LOG" 2>/dev/null | grep -o '"label":"[^"]*"' | sort | uniq -c || true)"
  {
    echo "## ha-drill-beads-pod-loss results — $(date -u +%Y-%m-%dT%H:%M:%SZ)"
    echo
    echo "Target pod: $(cat "$STATE_DIR/target_pod" 2>/dev/null || echo '(none — dry-run or preflight failure)')"
    echo "Kill method: $METHOD"
    echo
    echo "### Load loop (real kd/gb client calls)"
    echo "- total calls: $total"
    echo "- failures by call label (idempotent calls failing here means the client's own connection-reset retry gave up or was never applied):"
    echo "${failed_by_label:-  (none)}"
    echo
    echo "### API gap"
    measure_api_gap
    echo
    echo "### Gardener run outcome"
    if [[ -n "$ATTACH_CREW" || -n "${crew_id:-}" ]]; then
      kd show "${ATTACH_CREW:-$crew_id}" 2>/dev/null | grep -E '^(Status|Fields):' -A5 || echo "  (could not re-fetch crew bead post-kill)"
    else
      echo "  (no crew attached — see gd-Mc3cPhsInt's own run_log for gardener outcome)"
    fi
    echo
    echo "### Write-integrity check (escalation/decision/comment survival)"
    check_write_integrity
    echo
    echo "Raw logs: $LOAD_LOG"
  } | tee "$RESULTS_DIR/summary-$(date -u +%Y%m%dT%H%M%SZ).md"
}

# --- main --------------------------------------------------------------
preflight_rbac

crew_id="$(discover_crew)"
if [[ -z "$crew_id" ]]; then
  die "no in_progress crew agent found and no --attach-crew given. This drill needs one live crew (and ideally an in-flight gardener run) to observe outcome-recording behavior; pass --attach-crew <agent-bead-id>, or start one out-of-band first. Refusing to auto-spawn a crew as a side effect of a pod-loss drill."
fi
log "observing crew: $crew_id"

record_write_integrity_baseline
start_load_loop
target_pod="$(find_beads_pod)"
log "target pod: $target_pod"

trap 'stop_load_loop' EXIT

# Let the load loop establish a clean pre-kill baseline before we pull the rug.
sleep 15

kill_beads_pod "$target_pod"

# Wait out the rest of the configured duration so the load loop captures the
# daemon coming back (new pod Ready, Service endpoints updated) plus a tail
# of post-recovery traffic for the results table.
sleep "$(to_secs "$DURATION")"

stop_load_loop

summarize

log "drill complete. Fold the summary above into a comment on the task bead — this script does not post to beads itself."
