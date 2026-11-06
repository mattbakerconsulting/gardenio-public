#!/usr/bin/env bash
# HA epic (gd-StII9fmzym) Phase 3 drill 1: cordon+drain the spot node hosting
# the gardenio control plane and one live crew, under synthetic beads-API
# load, and measure the blast radius. Acceptance criterion for gd-hyCFMUqYPE
# is a results table with real numbers -- this script is the instrument that
# produces them.
#
# Run it as the ha-drill-operator role (gd-PgyIJPTUnC, SA
# gardenio-ha-drill-operator). That identity is deliberately narrow
# (templates/controller/ha-drill-rbac.yaml, #140): nodes get/list/watch/patch
# cluster-wide, and pods get/list + pods/eviction create in the release
# namespace only. The admission policy (#158) further limits the node patch to
# spec.unschedulable on gardenio.io/lifecycle=spot, non-control-plane nodes.
# So this script does NOT use `kubectl drain` (which lists pods and reads
# DaemonSets cluster-wide, and 403s under that grant): it cordons, then evicts
# the release namespace's pods on the node one by one through the eviction
# API. Pods of other namespaces on the node are invisible to it and stay put.
#
# What it does:
#   1. Starts a synthetic load loop against the beads API: ~60 req/min of
#      mixed list/get/comment traffic, every request timestamped, status-
#      logged and tagged with the drill phase (baseline / drain / recovery),
#      so failures during the drain are a delta against a measured baseline.
#   2. Picks the spot node hosting the most critical gardenio pods
#      (controller / beads daemon / coopmux) and one live crew whose pod is on
#      that same node (--attach-crew must name a crew on that node).
#   3. Refuses (fails closed) if the node is not lifecycle=spot, is a
#      control-plane node, hosts a protected pod (postgres/nats/vmsingle/
#      keycloak-runtime/minio), hosts a non-agent pod with a node-local or
#      unverifiable PVC, or if any of those lookups errors.
#   4. Cordons, evicts, and samples readiness WHILE the evictions run,
#      tracking each evicted pod by its owner (ReplicaSet/StatefulSet/agent),
#      not by its old name.
#   5. Prints a results table: per-phase request counts and non-2xx statuses,
#      time-to-Ready per rescheduled owner, whether the crew resumed, and how
#      many agent beads went agent_state=failed during the drill.
#
# Usage:
#   ./ha-drill-drain.sh                         # dry run (the default): read-only
#                                               # discovery + plan, no HTTP load,
#                                               # no cordon, no eviction
#   ./ha-drill-drain.sh --live --node <name>    # the real drill, pinned to <name>
#   ./ha-drill-drain.sh --uncordon-only         # recover an interrupted run
#
#   Options: [--namespace gardenio] [--baseline 2m] [--duration 10m]
#            [--attach-crew <agent-bead-id>] [--leave-cordoned]
#
#   A live run needs BEADS_AUTH_TOKEN (and BEADS_HTTP_ADDR if not in-cluster).
#   LOAD_SCRATCH_BEAD_ID, if set, receives the comment slot of the load mix;
#   otherwise that slot reads comments instead of writing them.
#
# Idempotent by design:
#   - State (target node, load-loop PID, whether this script cordoned) lives in
#     $STATE_DIR, outside the repo tree. Every mutating step checks cluster
#     state first: an already-cordoned node is not re-cordoned, a live load
#     loop is not relaunched, an already-gone pod is not re-evicted.
#   - The node is uncordoned only if this script cordoned it (we_cordoned
#     names the node). A node someone else cordoned is never uncordoned.
#   - The EXIT trap stops the load loop and, on any error, uncordons the node
#     if this run cordoned it. `--uncordon-only` replays that cleanup.
#   - The script never spawns a crew; it refuses if none is live on the node.
set -euo pipefail

STATE_DIR="${HA_DRILL_STATE_DIR:-${XDG_STATE_HOME:-$HOME/.local/state}/ha-drill-drain}"
RUN_ID="$(date -u +%Y%m%dT%H%M%SZ)"
RESULTS_DIR="$STATE_DIR/results/$RUN_ID"

NAMESPACE="gardenio"
BASELINE="2m"
DURATION="10m"
LIVE=0
NODE_PIN=""
LEAVE_CORDONED=0
UNCORDON_ONLY=0
ATTACH_CREW=""
LOAD_TARGET_OPM=60
EVICT_TIMEOUT_S=300
READY_TIMEOUT_S=600

# Critical control-plane pods, in priority order -- the node hosting the most
# of these is the drain target. Names match the epic's measured-starting-point
# Deployment names (gd-StII9fmzym).
CRITICAL_POD_PATTERNS=("gardenio-agents" "gardenio-beads" "gardenio-coopmux")

# Never touch a node carrying any of these: on-demand-pinned StatefulSets with
# node-local PVs (postgres/nats/vmsingle/minio per deploy-values/
# gardenio-k3s.yaml); keycloak-runtime is not pinned (gd-vekJWxnNGG), which is
# why it stays on this list.
PROTECTED_POD_PATTERNS=("postgres" "nats" "vmsingle" "keycloak-runtime" "minio")

# The admission policy (#158) only lets this SA cordon nodes carrying this
# label value; the script refuses the same nodes up front.
LIFECYCLE_LABEL="gardenio.io/lifecycle"
CONTROL_PLANE_LABEL="node-role.kubernetes.io/control-plane"
SPOT_VALUE="spot"

# Agent pods carry this label (podmanager.LabelAgent); the controller
# recreates them, so they are tracked by it instead of by an ownerReference.
AGENT_LABEL="gardenio.io/agent"

log() { printf '[%s] %s\n' "$(date -u +%Y-%m-%dT%H:%M:%SZ)" "$*" >&2; }
die() { log "ERROR: $*"; exit 1; }

while [[ $# -gt 0 ]]; do
  case "$1" in
    --live)           LIVE=1; shift;;
    --dry-run)        LIVE=0; shift;;
    --node)           NODE_PIN="$2"; shift 2;;
    --namespace)      NAMESPACE="$2"; shift 2;;
    --baseline)       BASELINE="$2"; shift 2;;
    --duration)       DURATION="$2"; shift 2;;
    --attach-crew)    ATTACH_CREW="$2"; shift 2;;
    --leave-cordoned) LEAVE_CORDONED=1; shift;;
    --uncordon-only)  UNCORDON_ONLY=1; shift;;
    -h|--help)        sed -n '2,61p' "$0"; exit 0;;
    *) die "unknown arg: $1 (see --help)";;
  esac
done

mkdir -p "$STATE_DIR" "$RESULTS_DIR"
K=(kubectl -n "$NAMESPACE")
KC=(kubectl)
WE_CORDONED_FILE="$STATE_DIR/we_cordoned"
TARGET_FILE="$STATE_DIR/target_node"
PHASE_FILE="$STATE_DIR/phase"
LOAD_LOG="$RESULTS_DIR/load.ndjson"
LOAD_PID_FILE="$STATE_DIR/load.pid"
READY_LOG="$RESULTS_DIR/reschedule.ndjson"
EVICT_LOG="$RESULTS_DIR/evictions.ndjson"
CREW_LOG="$RESULTS_DIR/crew.json"
DRILL_START=""

if [[ "$LIVE" == 1 ]]; then
  [[ -n "$NODE_PIN" ]] || die "--live needs --node <name>: pin the node you mean to drain (run without --live first to see the discovered target)"
  [[ -n "${BEADS_AUTH_TOKEN:-}" ]] || die "--live needs BEADS_AUTH_TOKEN: without it every load-loop request fails and the failure count measures nothing"
else
  log "DRY-RUN (default): read-only discovery and plan only; no load traffic, no cordon, no eviction. Pass --live --node <name> for the real drill."
fi

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
  kubectl auth can-i get nodes            >/dev/null 2>&1 || missing+=("get nodes")
  kubectl auth can-i list nodes           >/dev/null 2>&1 || missing+=("list nodes")
  kubectl auth can-i patch nodes          >/dev/null 2>&1 || missing+=("patch nodes (cordon)")
  kubectl auth can-i get pods -n "$NAMESPACE"             >/dev/null 2>&1 || missing+=("get pods -n $NAMESPACE")
  kubectl auth can-i list pods -n "$NAMESPACE"            >/dev/null 2>&1 || missing+=("list pods -n $NAMESPACE")
  kubectl auth can-i create pods/eviction -n "$NAMESPACE" >/dev/null 2>&1 || missing+=("create pods/eviction -n $NAMESPACE")
  if [[ ${#missing[@]} -gt 0 ]]; then
    die "missing RBAC for: ${missing[*]}. Run as the ha-drill-operator role (gd-PgyIJPTUnC). Refusing to proceed."
  fi
  log "RBAC preflight OK: node get/list/patch + pod get/list + pods/eviction create in $NAMESPACE"
}

# --- step 1: synthetic load loop against the beads API ---------------------
set_phase() {
  echo "$1" > "$PHASE_FILE"
  log "phase: $1"
}

beads_call() {
  # beads_call <method> <path> [body] -- logs one NDJSON line per call.
  local method="$1" path="$2" body="${3:-}"
  local url="${BEADS_HTTP_ADDR:-http://gardenio-beads.gardenio.svc.cluster.local:8080}${path}"
  local start status latency_ms end phase
  phase="$(cat "$PHASE_FILE" 2>/dev/null || echo unknown)"
  start="$(date +%s.%N)"
  if [[ -n "$body" ]]; then
    status="$(curl -sS -o /dev/null -w '%{http_code}' -m 5 -X "$method" "$url" \
      -H "Authorization: Bearer ${BEADS_AUTH_TOKEN}" -H 'Content-Type: application/json' \
      -d "$body" 2>/dev/null)" || true
  else
    status="$(curl -sS -o /dev/null -w '%{http_code}' -m 5 -X "$method" "$url" \
      -H "Authorization: Bearer ${BEADS_AUTH_TOKEN}" 2>/dev/null)" || true
  fi
  # curl already prints 000 when it gets no response; only default if empty.
  status="${status:-000}"
  end="$(date +%s.%N)"
  latency_ms="$(awk "BEGIN{printf \"%.0f\", ($end-$start)*1000}")"
  printf '{"ts":"%s","phase":"%s","method":"%s","path":"%s","status":"%s","latency_ms":%s}\n' \
    "$(date -u +%Y-%m-%dT%H:%M:%S.%3NZ)" "$phase" "$method" "$path" "$status" "$latency_ms" >> "$LOAD_LOG"
}

start_load_loop() {
  local end_ts scratch_id="${LOAD_SCRATCH_BEAD_ID:-}"
  if [[ "$LIVE" != 1 ]]; then
    log "dry-run: would start a ~${LOAD_TARGET_OPM} req/min load loop against ${BEADS_HTTP_ADDR:-the in-cluster beads service} (comment target: ${scratch_id:-read-only})"
    return 0
  fi
  if [[ -f "$LOAD_PID_FILE" ]] && kill -0 "$(cat "$LOAD_PID_FILE")" 2>/dev/null; then
    log "load loop already running (pid $(cat "$LOAD_PID_FILE")), not starting a second one"
    return 0
  fi
  if [[ -z "$scratch_id" ]]; then
    log "no LOAD_SCRATCH_BEAD_ID given: the comment slot reads gd-StII9fmzym's comments instead of writing"
  fi
  # Upper bound only: the main flow stops the loop when recovery ends.
  end_ts=$(( $(date +%s) + $(to_secs "$BASELINE") + $(to_secs "$DURATION") + EVICT_TIMEOUT_S + READY_TIMEOUT_S ))
  (
    while [[ $(date +%s) -lt $end_ts ]]; do
      r=$(( RANDOM % 100 ))
      if [[ $r -lt 50 ]]; then
        beads_call GET "/v1/beads?limit=25"
      elif [[ $r -lt 85 ]]; then
        beads_call GET "/v1/beads/gd-StII9fmzym"
      else
        if [[ -n "$scratch_id" ]]; then
          beads_call POST "/v1/beads/${scratch_id}/comments" '{"text":"ha-drill-drain synthetic load '"$(date +%s)"'"}'
        else
          beads_call GET "/v1/beads/gd-StII9fmzym/comments"
        fi
      fi
      # ~60 req/min == 1s between calls; jitter 0.800-1.199s so it isn't lockstep.
      sleep "$(awk -v r="$RANDOM" 'BEGIN{printf "%.3f", 0.8+(r%400)/1000}')"
    done
  ) &
  echo $! > "$LOAD_PID_FILE"
  log "load loop started, pid $(cat "$LOAD_PID_FILE"), target ~${LOAD_TARGET_OPM} req/min, log $LOAD_LOG"
}

stop_load_loop() {
  [[ -f "$LOAD_PID_FILE" ]] || return 0
  local pid; pid="$(cat "$LOAD_PID_FILE")"
  kill "$pid" 2>/dev/null || true
  wait "$pid" 2>/dev/null || true
  rm -f "$LOAD_PID_FILE"
  log "load loop stopped"
}

# --- step 2: discover target node and guard it (fail closed) ---------------
node_guard() {
  # node_guard <node> -- dies unless the node is a spot, non-control-plane
  # node. Any lookup error is a refusal, never "not protected".
  local node="$1" labels lifecycle
  labels="$("${KC[@]}" get node "$node" -o json | jq -ce '.metadata.labels // {}')" \
    || die "refusing $node: could not read its labels"
  lifecycle="$(jq -r --arg k "$LIFECYCLE_LABEL" '.[$k] // ""' <<<"$labels")"
  [[ "$lifecycle" == "$SPOT_VALUE" ]] \
    || die "refusing $node: $LIFECYCLE_LABEL='${lifecycle}', not '$SPOT_VALUE'. This drill only targets spot nodes."
  if jq -e --arg k "$CONTROL_PLANE_LABEL" 'has($k)' <<<"$labels" >/dev/null; then
    die "refusing $node: control-plane node ($CONTROL_PLANE_LABEL)"
  fi
}

pods_on_node_json() {
  # pods_on_node_json <node> -- this namespace's pods on <node>, as a JSON
  # array. Dies on a lookup error.
  local node="$1" all
  all="$("${K[@]}" get pods -o json)" || die "could not list pods in $NAMESPACE"
  jq --arg n "$node" '[.items[] | select(.spec.nodeName == $n)]' <<<"$all"
}

owner_key_jq='(if (.metadata.labels // {})["'"$AGENT_LABEL"'"] then "agent/" + .metadata.labels["'"$AGENT_LABEL"'"]
               elif (.metadata.ownerReferences // []) | length > 0 then .metadata.ownerReferences[0].kind + "/" + .metadata.ownerReferences[0].name
               else "bare/" + .metadata.name end)'

pod_guard() {
  # pod_guard <node> <pods-json> -- dies if the node hosts a protected pod, or
  # a non-agent pod with a PVC that is node-local or cannot be verified.
  # Agent pods with PVCs are reported, not refused: they are the crew this
  # drill observes, and a crew that stays Pending is a result.
  local node="$1" pods="$2" pat names claim sc
  names="$(jq -r '.[].metadata.name' <<<"$pods")"
  for pat in "${PROTECTED_POD_PATTERNS[@]}"; do
    if grep -qi "$pat" <<<"$names"; then
      die "refusing $node: hosts a protected pod matching '$pat'. See gd-vekJWxnNGG before ever touching keycloak-runtime."
    fi
  done
  while IFS=$'\t' read -r pod is_agent claim; do
    [[ -n "$claim" ]] || continue
    sc="$("${K[@]}" get pvc "$claim" -o 'jsonpath={.spec.storageClassName}' 2>/dev/null)" || sc="<unreadable>"
    if [[ "$is_agent" == "true" ]]; then
      log "NOTE: agent pod $pod mounts PVC $claim (storageClass ${sc:-<none>}); a node-local volume leaves it Pending after eviction until the controller replaces it"
      echo "$pod $claim ${sc:-<none>}" >> "$RESULTS_DIR/agent-pvcs.txt"
      continue
    fi
    case "$sc" in
      local-path|local-storage|""|"<unreadable>")
        die "refusing $node: pod $pod mounts PVC $claim with storageClass '${sc:-<none>}', which is node-local or unverifiable; evicting it leaves the pod Pending (the 2026-09-22 beads outage)";;
    esac
  done < <(jq -r --arg a "$AGENT_LABEL" '.[] | . as $p
      | (.spec.volumes // [])[] | select(.persistentVolumeClaim)
      | [$p.metadata.name, ((($p.metadata.labels // {})[$a]) != null | tostring), .persistentVolumeClaim.claimName] | @tsv' <<<"$pods")
}

find_target_node() {
  local all best
  all="$("${K[@]}" get pods -o json)" || die "could not list pods in $NAMESPACE"
  best="$(jq -r --argjson pats "$(printf '%s\n' "${CRITICAL_POD_PATTERNS[@]}" | jq -R . | jq -s .)" '
      [.items[] | select(.spec.nodeName != null) | . as $p
       | select(any($pats[]; . as $x | $p.metadata.name | startswith($x))) | .spec.nodeName]
      | group_by(.) | map({node: .[0], n: length}) | sort_by(-.n) | .[0].node // empty' <<<"$all")"
  [[ -n "$best" ]] || die "no critical pods (${CRITICAL_POD_PATTERNS[*]}) found in $NAMESPACE; re-check the pod name patterns against live Deployment names"
  echo "$best"
}

# --- step 3: find the live crew on the target node --------------------------
crew_bead_json() {
  # crew_bead_json <agent-bead-id>
  kd show "$1" --json
}

discover_crew() {
  # discover_crew <node> <pods-json> -- an agent bead id whose live pod is on
  # <node>. Live agent beads are mostly status=open, so match on the pod, not
  # on bead status.
  local node="$1" pods="$2" agent id pod_node
  if [[ -n "$ATTACH_CREW" ]]; then
    agent="$(crew_bead_json "$ATTACH_CREW" | jq -r '.fields.agent // empty')" \
      || die "could not read crew bead $ATTACH_CREW"
    pod_node="$(jq -r --arg a "$AGENT_LABEL" --arg v "$agent" \
      '[.[] | select((.metadata.labels // {})[$a] == $v)] | .[0].spec.nodeName // empty' <<<"$pods")"
    [[ "$pod_node" == "$node" ]] \
      || die "--attach-crew $ATTACH_CREW (agent '$agent') has no pod on target node $node; the drill must drain the crew's own node"
    echo "$ATTACH_CREW"
    return 0
  fi
  while read -r agent; do
    [[ -n "$agent" ]] || continue
    id="$(kd list --all-types --type=agent -l project:gardenio -s open -s in_progress \
          -f "agent=$agent" -f agent_state=working --json --limit 1 | jq -r '.[0].id // empty')" \
      || die "kd list failed while looking up crew '$agent'"
    if [[ -n "$id" ]]; then echo "$id"; return 0; fi
  done < <(jq -r --arg a "$AGENT_LABEL" \
      '.[] | select((.metadata.labels // {})["gardenio.io/mode"] == "crew") | (.metadata.labels // {})[$a] // empty' <<<"$pods")
  echo ""
}

crew_snapshot() {
  # crew_snapshot <label> <agent-bead-id> -- records state, session, respawn
  # count and the pod's uid/node, so "resumed" is a comparison, not a guess.
  local label="$1" id="$2" bead agent pod
  bead="$(crew_bead_json "$id")" || { log "WARN: could not read crew bead $id ($label)"; return 0; }
  agent="$(jq -r '.fields.agent // empty' <<<"$bead")"
  pod="$("${K[@]}" get pods -l "$AGENT_LABEL=$agent" -o json 2>/dev/null \
    | jq -c '[.items[] | {name: .metadata.name, uid: .metadata.uid, node: .spec.nodeName, phase: .status.phase,
                          created: .metadata.creationTimestamp}] | sort_by(.created) | .[-1] // null' 2>/dev/null || echo null)"
  jq -c --arg l "$label" --arg ts "$(date -u +%Y-%m-%dT%H:%M:%SZ)" --argjson pod "${pod:-null}" \
    '{label: $l, ts: $ts, agent_state: .fields.agent_state, last_session_id: .fields.last_session_id,
      respawn_count: .fields.respawn_count, status: .status, pod: $pod}' <<<"$bead" >> "$CREW_LOG"
}

wait_crew_resumed() {
  # wait_crew_resumed <agent-bead-id> -- polls until the crew has a new,
  # Running pod and its bead says agent_state=working (or a terminal state),
  # or READY_TIMEOUT_S passes. The bead alone is not enough: it still says
  # "working" for a while after the old pod is gone.
  local id="$1" deadline old_uid agent state uid
  old_uid="$(jq -rs '.[0].pod.uid // empty' "$CREW_LOG" 2>/dev/null || true)"
  agent="$(crew_bead_json "$id" | jq -r '.fields.agent // empty' 2>/dev/null || true)"
  deadline=$(( $(date +%s) + READY_TIMEOUT_S ))
  while [[ $(date +%s) -lt $deadline ]]; do
    state="$(crew_bead_json "$id" 2>/dev/null | jq -r '.fields.agent_state // empty' 2>/dev/null || true)"
    uid="$("${K[@]}" get pods -l "$AGENT_LABEL=$agent" -o json 2>/dev/null \
      | jq -r '[.items[] | select(.status.phase == "Running")] | sort_by(.metadata.creationTimestamp) | .[-1].metadata.uid // empty' 2>/dev/null || true)"
    case "$state" in done|failed|stopped) break;; esac
    if [[ -n "$uid" && "$uid" != "$old_uid" && "$state" == "working" ]]; then break; fi
    sleep 5
  done
  crew_snapshot after "$id"
}

# Agent beads that went agent_state=failed during the drill: the measurable
# form of "was any captain/gardener run marked failed".
failed_agents_since() {
  local since="$1"
  kd list --all-types --type=agent -l project:gardenio -s open -s closed -s in_progress \
    -f agent_state=failed --json --limit 200 \
    | jq -c --arg s "$since" '[.[] | select(.updated_at >= $s) | {id, agent: .fields.agent, role: .fields.role, updated_at}]'
}

evictable_json() {
  # evictable_json <pods-json> -- the pods drain_node evicts: not DaemonSet,
  # not mirror, and not bare non-agent pods (nothing would recreate them).
  jq -c "[.[]
      | select(((.metadata.ownerReferences // [])[0].kind // \"\") != \"DaemonSet\")
      | select(((.metadata.annotations // {})[\"kubernetes.io/config.mirror\"]) == null)
      | {name: .metadata.name, uid: .metadata.uid, key: $owner_key_jq}
      | select(.key | startswith(\"bare/\") | not)]" <<<"$1"
}

# --- step 4: cordon, evict, sample ------------------------------------------
cordon_node() {
  local node="$1" unschedulable
  echo "$node" > "$TARGET_FILE"
  unschedulable="$("${KC[@]}" get node "$node" -o 'jsonpath={.spec.unschedulable}')" \
    || die "could not read $node's cordon state"
  if [[ "$unschedulable" == "true" ]]; then
    if [[ "$(cat "$WE_CORDONED_FILE" 2>/dev/null)" == "$node" ]]; then
      log "node $node already cordoned by an earlier run of this script (idempotent skip)"
    else
      die "node $node is already cordoned by someone else (spot handling or ops work); not draining a node this drill does not own"
    fi
    return 0
  fi
  log "cordon $node"
  "${KC[@]}" cordon "$node"
  echo "$node" > "$WE_CORDONED_FILE"
}

evict_pod() {
  # evict_pod <pod> -- one eviction through the API, retried while a PDB says
  # 429. Returns non-zero if the pod is still there at the deadline.
  local pod="$1" deadline out
  deadline=$(( $(date +%s) + EVICT_TIMEOUT_S ))
  while [[ $(date +%s) -lt $deadline ]]; do
    if out="$(printf '{"apiVersion":"policy/v1","kind":"Eviction","metadata":{"name":"%s","namespace":"%s"}}' "$pod" "$NAMESPACE" \
        | "${KC[@]}" create --raw "/api/v1/namespaces/$NAMESPACE/pods/$pod/eviction" -f - 2>&1)"; then
      printf '{"ts":"%s","pod":"%s","result":"evicted"}\n' "$(date -u +%Y-%m-%dT%H:%M:%S.%3NZ)" "$pod" >> "$EVICT_LOG"
      return 0
    fi
    case "$out" in
      *NotFound*|*"not found"*)
        printf '{"ts":"%s","pod":"%s","result":"already-gone"}\n' "$(date -u +%Y-%m-%dT%H:%M:%S.%3NZ)" "$pod" >> "$EVICT_LOG"
        return 0;;
      *TooManyRequests*|*"disruption budget"*|*429*)
        log "eviction of $pod blocked by a PDB, retrying"; sleep 5;;
      *)
        log "eviction of $pod failed: $out"; return 1;;
    esac
  done
  log "eviction of $pod still blocked after ${EVICT_TIMEOUT_S}s"
  return 1
}

sample_readiness() {
  # sample_readiness <node> <tracked-json> <since> -- runs in the background
  # while the evictions do. For each evicted pod's owner, records the first
  # replacement pod (created at or after <since>) that is Ready off <node>,
  # using the Ready condition's own transition time. Old names are never
  # matched: replacement pods do not keep them.
  local node="$1" tracked="$2" since="$3" deadline snap found
  tracked="$(jq -c 'unique_by(.key)' <<<"$tracked")"
  deadline=$(( $(date +%s) + EVICT_TIMEOUT_S + READY_TIMEOUT_S ))
  found='[]'
  while [[ $(date +%s) -lt $deadline ]]; do
    snap="$("${K[@]}" get pods -o json 2>/dev/null)" || { sleep 2; continue; }
    found="$(jq -c --arg n "$node" --arg s "$since" --argjson t "$tracked" --argjson f "$found" "
      [.items[] | {key: $owner_key_jq, name: .metadata.name, node: .spec.nodeName,
                   created: .metadata.creationTimestamp,
                   ready: ([(.status.conditions // [])[] | select(.type == \"Ready\" and .status == \"True\") | .lastTransitionTime][0])}
       | select(.ready != null and .node != \$n and .created >= \$s)] as \$new
      | \$f + [\$t[] | . as \$o | select(any(\$f[]; .key == \$o.key) | not)
               | ([\$new[] | select(.key == \$o.key)] | sort_by(.ready) | .[0]) as \$r
               | select(\$r != null)
               | {key: \$o.key, old_pod: \$o.name, new_pod: \$r.name, rescheduled_to: \$r.node, ready_at: \$r.ready}]
    " <<<"$snap")"
    if [[ "$(jq length <<<"$found")" -ge "$(jq length <<<"$tracked")" ]]; then
      break
    fi
    sleep 2
  done
  jq -c '.[]' <<<"$found" > "$READY_LOG.tmp"
  mv "$READY_LOG.tmp" "$READY_LOG"
}

drain_node() {
  # drain_node <node> <pods-json> -- evicts this namespace's pods on <node>,
  # skipping DaemonSet and mirror pods (they stay by design) and bare non-agent
  # pods (nothing would recreate them). Samples readiness concurrently.
  local node="$1" pods="$2" tracked sampler pod since failed=0
  tracked="$(evictable_json "$pods")"
  jq -r '.[] | select(((.metadata.ownerReferences // []) | length) == 0 and ((.metadata.labels // {})["'"$AGENT_LABEL"'"]) == null) | .metadata.name' <<<"$pods" \
    | while read -r pod; do [[ -n "$pod" ]] && log "skipping bare pod $pod (nothing recreates it)"; done
  echo "$tracked" > "$RESULTS_DIR/tracked.json"
  log "evicting $(jq length <<<"$tracked") pod(s) from $node"

  since="$(date -u +%Y-%m-%dT%H:%M:%SZ)"
  sample_readiness "$node" "$tracked" "$since" &
  sampler=$!
  SAMPLER_PID=$sampler
  while read -r pod; do
    [[ -n "$pod" ]] || continue
    evict_pod "$pod" || failed=1
  done < <(jq -r '.[].name' <<<"$tracked")
  # A failed eviction ends the drill now; on_exit stops the sampler.
  [[ "$failed" == 0 ]] || die "one or more evictions failed (see $EVICT_LOG); the trap uncordons $node"
  wait "$sampler" || true
  SAMPLER_PID=""
}

uncordon_target() {
  # uncordon_target [force] -- uncordons only a node this script cordoned.
  # Without force, --leave-cordoned is honored.
  local node
  node="$(cat "$WE_CORDONED_FILE" 2>/dev/null || true)"
  if [[ -z "$node" ]]; then
    log "this script did not cordon a node (no $WE_CORDONED_FILE); nothing to uncordon"
    return 0
  fi
  if [[ "${1:-}" != force && "$LEAVE_CORDONED" == 1 ]]; then
    log "--leave-cordoned set, not uncordoning $node"
    return 0
  fi
  log "uncordon $node"
  "${KC[@]}" uncordon "$node"
  rm -f "$WE_CORDONED_FILE"
}

on_exit() {
  local rc=$?
  trap - EXIT INT TERM
  [[ -z "${SAMPLER_PID:-}" ]] || kill "$SAMPLER_PID" 2>/dev/null || true
  stop_load_loop
  if [[ "$rc" -ne 0 && "$LIVE" == 1 ]]; then
    log "exiting with status $rc; uncordoning the node if this run cordoned it"
    uncordon_target force || log "ERROR: uncordon failed; run --uncordon-only"
  fi
  exit "$rc"
}

# --- step 5: measure -------------------------------------------------------
summarize() {
  local evicted_at
  {
    echo "## ha-drill-drain results -- run $RUN_ID"
    echo
    echo "Target node: $(cat "$TARGET_FILE" 2>/dev/null || echo '(none)')  ·  drill start $DRILL_START"
    echo
    echo "### Load loop (per phase)"
    echo
    echo "| phase | requests | non-2xx | statuses | p50 ms | max ms |"
    echo "|---|---|---|---|---|---|"
    if [[ -s "$LOAD_LOG" ]]; then
      jq -rs 'group_by(.phase)[] | . as $g
        | ($g | map(.latency_ms) | sort) as $l
        | [$g[0].phase, ($g | length),
           ($g | map(select(.status | startswith("2") | not)) | length),
           ($g | map(select(.status | startswith("2") | not) | .status) | group_by(.) | map("\(.[0])×\(length)") | join(" ")),
           $l[(($l | length) / 2 | floor)], $l[-1]]
        | "| " + (map(tostring) | join(" | ")) + " |"' "$LOAD_LOG"
      echo
      echo "Non-2xx timestamps: $(jq -rs '[.[] | select(.status | startswith("2") | not) | "\(.ts) \(.status) \(.method) \(.path)"] | if length == 0 then "(none)" else join("; ") end' "$LOAD_LOG")"
    else
      echo "| (no load data) | | | | | |"
    fi
    echo
    echo "### Time to Ready (by owner)"
    echo
    echo "| owner | old pod | new pod | rescheduled to | evicted at | ready at | seconds |"
    echo "|---|---|---|---|---|---|---|"
    if [[ -s "$READY_LOG" ]]; then
      while read -r line; do
        evicted_at="$(jq -r --arg p "$(jq -r .old_pod <<<"$line")" 'select(.pod == $p) | .ts' "$EVICT_LOG" | head -1)"
        jq -r --arg e "$evicted_at" '[.key, .old_pod, .new_pod, .rescheduled_to, $e, .ready_at,
            (if $e == "" then "?" else ((.ready_at | fromdateiso8601) - ($e | sub("\\.[0-9]+Z$"; "Z") | fromdateiso8601) | tostring) end)]
          | "| " + join(" | ") + " |"' <<<"$line"
      done < "$READY_LOG"
    fi
    jq -r --slurpfile r <(cat "$READY_LOG" 2>/dev/null) 'unique_by(.key)[] | . as $o | select(any($r[]; .key == $o.key) | not)
        | "| \(.key) | \(.name) | (not Ready within the timeout) | | | | |"' "$RESULTS_DIR/tracked.json" 2>/dev/null || true
    if [[ -s "$RESULTS_DIR/agent-pvcs.txt" ]]; then
      echo
      echo "Agent pods with PVCs on the node (pod claim storageClass): $(tr '\n' ';' < "$RESULTS_DIR/agent-pvcs.txt")"
    fi
    echo
    echo "### Crew"
    echo
    if [[ -s "$CREW_LOG" ]]; then
      jq -rs '(.[0]) as $b | (.[-1]) as $a
        | "- before: state=\($b.agent_state) session=\($b.last_session_id) respawns=\($b.respawn_count) pod=\($b.pod.name // "-")@\($b.pod.node // "-")",
          "- after:  state=\($a.agent_state) session=\($a.last_session_id) respawns=\($a.respawn_count) pod=\($a.pod.name // "-")@\($a.pod.node // "-")",
          "- resumed: " + (if ($a.pod.uid // "") != "" and $a.pod.uid != ($b.pod.uid // "") and $a.agent_state == "working"
              then "yes (new pod, back to working; session " + (if $a.last_session_id == $b.last_session_id then "same, conversation resumed" else "changed" end) + ")"
              else "no" end)' "$CREW_LOG"
    else
      echo "- (no crew data)"
    fi
    echo
    echo "### Agent runs marked failed during the drill"
    echo
    if [[ -n "$DRILL_START" ]]; then
      failed_agents_since "$DRILL_START" | jq -r 'if length == 0 then "- none" else (.[] | "- \(.id) \(.role) \(.agent) at \(.updated_at)") end' \
        || echo "- (kd list failed; not measured)"
    else
      echo "- (not measured)"
    fi
    echo
    echo "Raw logs: $RESULTS_DIR"
  } | tee "$RESULTS_DIR/summary.md"
}

# --- main --------------------------------------------------------------
if [[ "$UNCORDON_ONLY" == 1 ]]; then
  uncordon_target force
  exit 0
fi

trap on_exit EXIT
# A signal must reach on_exit with a non-zero status, so it uncordons.
trap 'exit 130' INT
trap 'exit 143' TERM

preflight_rbac

target_node="$(find_target_node)"
log "discovered target (most critical pods): $target_node"
if [[ -n "$NODE_PIN" && "$NODE_PIN" != "$target_node" ]]; then
  log "--node $NODE_PIN overrides the discovered target $target_node"
  target_node="$NODE_PIN"
fi
node_guard "$target_node"
node_pods="$(pods_on_node_json "$target_node")"
pod_guard "$target_node" "$node_pods"

crew_id="$(discover_crew "$target_node" "$node_pods")"
[[ -n "$crew_id" ]] || die "no live crew (agent_state=working) has a pod on $target_node, and no --attach-crew given. Start a crew there out-of-band first; this drill never spawns one."
log "observing crew: $crew_id"

if [[ "$LIVE" != 1 ]]; then
  log "dry-run plan: cordon $target_node, evict these $NAMESPACE pods (DaemonSet, mirror and bare non-agent pods skipped), observe crew $crew_id"
  evictable_json "$node_pods" | jq -r '.[] | "  would evict: \(.name) (\(.key))"' >&2
  start_load_loop
  exit 0
fi

DRILL_START="$(date -u +%Y-%m-%dT%H:%M:%SZ)"
set_phase baseline
start_load_loop
crew_snapshot before "$crew_id"
sleep "$(to_secs "$BASELINE")"

set_phase drain
cordon_node "$target_node"
drain_node "$target_node" "$node_pods"

set_phase recovery
wait_crew_resumed "$crew_id"
sleep "$(to_secs "$DURATION")"
stop_load_loop

summarize
uncordon_target

log "drill complete. Fold the summary above into a comment on the task bead -- this script does not post to beads itself."
