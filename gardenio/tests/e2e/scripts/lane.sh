#!/usr/bin/env bash
# lane.sh — driver for the live-namespace e2e lane (gd-WBz2qt9ADZ).
#
# Design: docs/superpowers/specs/2026-10-13-e2e-live-namespace-lane-design.md.
# Called by .github/workflows/e2e.yml's live-* jobs; every subcommand is also
# runnable by hand against a kubeconfig that holds the gardenio-e2e-tenant Role.
#
#   lane.sh acquire                 -> prints the pool namespace it leased
#   lane.sh assert-tenant <ns>      -> kubectl auth can-i: ceiling + escalate/bind verbs denied, tenant
#                                      verbs granted, and a Role granting update resourcequotas is Forbidden
#   lane.sh install <ns> [<tag>]    -> assert-tenant, wipe, mint run secrets, pack + helm install, wait
#   lane.sh seed <ns>               -> port-forward + seed-types-and-users.sh
#   lane.sh token <ns>              -> prints the run's beads token (masked by caller)
#   lane.sh teardown <ns>           -> helm uninstall, wipe, release the lease
#   lane.sh sweep                   -> wipe every pool namespace whose lease expired
#   lane.sh newest-tag              -> newest calver tag in the checkout
#   lane.sh skip-list               -> HERMETIC_MODULES as a SKIP= csv
#   lane.sh report <dir>...         -> merge e2e-results.jsonl files; exit 1 on any fail
#
# Pool model (why fixed namespaces and a Lease, not `kubectl create ns`):
# namespaces are cluster-scoped and RBAC cannot restrict `create` by name, so
# a create-per-run runner needs a ClusterRole. With a pool the runner holds one
# Role per pool namespace and nothing cluster-scoped. A run owns a namespace by
# creating coordination.k8s.io/Lease `e2e-run-lock` in it (create is atomic:
# AlreadyExists == someone else holds it). A lease older than its
# leaseDurationSeconds is a run whose teardown never ran (spot reclaim); it is
# swept, not respected.
#
# "Disposable" is preserved by wiping, on BOTH ends: install wipes first (the
# previous run may have died mid-shard), teardown wipes after. The wipe never
# touches the operator's objects (Role, RoleBinding, ResourceQuota, LimitRange,
# the `ecr` pull Secret): it deletes workloads, PVCs, ConfigMaps, run Secrets
# (labelled gardenio.io/e2e-run) and Helm-labelled Secrets/SAs/Roles. The
# runner could not touch the quota or limit range anyway: the tenant Role
# enumerates core resources and holds no verb on either, and names its rbac
# verbs so it holds neither `escalate` nor `bind` and cannot write itself a
# Role that does (assert-tenant proves both from the runner's own token before
# every install, including a positive control that tries exactly that).
#
# Secrets are created with `kubectl create secret generic`, never `kubectl
# apply` (scripts/security/lint-kubectl-apply-secret.py).
#
# Testable seams: KUBECTL / HELM / LANE_PACK are overridable, and the pure helpers
# (lane_lease_expired, lane_pick_newest_calver, lane_report_merge,
# lane_skip_list) take their inputs as arguments or stdin.
# test-lane-lib.sh drives them with a fake kubectl; set LANE_SOURCE_ONLY=1 to
# source this file without dispatching.
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="${REPO_ROOT:-$(cd "$SCRIPT_DIR/../../../.." && pwd)}"
KUBECTL="${KUBECTL:-kubectl}"
HELM="${HELM:-helm}"
CHART_DIR="${CHART_DIR:-$REPO_ROOT/gardenio/helm/gardenio}"
LANE_PACK="${LANE_PACK:-$REPO_ROOT/scripts/pack-gardenio-chart.sh}"
LANE_VALUES="${LANE_VALUES:-$REPO_ROOT/gardenio/tests/e2e/lane/values-e2e.yaml}"
LANE_POOL="${LANE_POOL:-gardenio-e2e-0 gardenio-e2e-1}"
LANE_LEASE_NAME="${LANE_LEASE_NAME:-e2e-run-lock}"
LANE_LEASE_SECONDS="${LANE_LEASE_SECONDS:-7200}"
LANE_HOLDER="${LANE_HOLDER:-${GITHUB_RUN_ID:-local}-${GITHUB_RUN_ATTEMPT:-0}}"
LANE_ACQUIRE_TIMEOUT="${LANE_ACQUIRE_TIMEOUT:-1200}"
LANE_ACQUIRE_POLL="${LANE_ACQUIRE_POLL:-30}"
LANE_HELM_TIMEOUT="${LANE_HELM_TIMEOUT:-12m}"
LANE_RUN_LABEL="gardenio.io/e2e-run=true"
LANE_TOKEN_SECRET="gardenio-e2e-beads-token"
LANE_COOP_SECRET="coop-credential-key"
LANE_PF_PORT="${LANE_PF_PORT:-19090}"

log() { printf '[lane] %s\n' "$*" >&2; }
die() { printf '[lane] FATAL: %s\n' "$*" >&2; exit 1; }

# ── pure helpers ──────────────────────────────────────────────────────────────

# lane_lease_expired <acquireTime RFC3339> <leaseDurationSeconds> [<now epoch>]
# exit 0 when the lease is stale, 1 when still live. Empty acquireTime == stale
# (a lease nobody stamped is a lease nobody is renewing).
lane_lease_expired() {
  local acquired="$1" dur="$2" now="${3:-$(date -u +%s)}"
  [ -n "$acquired" ] || return 0
  local acq_epoch
  acq_epoch=$(date -u -d "$acquired" +%s 2>/dev/null) || return 0
  [ $((acq_epoch + dur)) -lt "$now" ]
}

# lane_pick_newest_calver: stdin = tag names, stdout = the newest YYYY.DDD.N
# calver (numeric by field), or exit 1 if none. Not `sort -V`: it treats
# 2026.286.10 < 2026.286.9 as strings would not, but version-sort is locale-
# and implementation-dependent for a leading-zero-free calver; be explicit.
lane_pick_newest_calver() {
  grep -E '^20[0-9]{2}\.[0-9]{1,3}\.[0-9]+$' | sort -t. -k1,1n -k2,2n -k3,3n | tail -1 | grep .
}

# lane_newest_tag: newest calver tag present in this checkout (the workflow
# checks out with fetch-depth 0 so tags are there). Overridable by E2E_IMAGE_TAG.
lane_newest_tag() {
  if [ -n "${E2E_IMAGE_TAG:-}" ]; then echo "$E2E_IMAGE_TAG"; return; fi
  git -C "$REPO_ROOT" tag --list | lane_pick_newest_calver \
    || die "no calver tag (20YY.DDD.N) in this checkout; pass E2E_IMAGE_TAG"
}

# lane_skip_list: HERMETIC_MODULES from run-shard.sh as a SKIP= csv. Those
# modules already run in the shell-hermetic job; a shard that re-ran them would
# double-count their time and their verdicts.
lane_skip_list() {
  local f="${1:-$SCRIPT_DIR/run-shard.sh}"
  sed -n 's/^HERMETIC_MODULES="\(.*\)"$/\1/p' "$f" | head -1 | grep .
}

# lane_report_merge <jsonl>... : one markdown table over every module row;
# exit 1 if any row is a fail. Skips are listed with their reason, never hidden.
lane_report_merge() {
  local f fails=0 passes=0 skips=0
  echo '| status | time | shard | module | reason |'
  echo '|---|---|---|---|---|'
  for f in "$@"; do
    [ -s "$f" ] || continue
    jq -r '"| \(.status) | \(.duration_s)s | \(.shard // "-") | \(.name) | \(.skip_reason // .fail_reason // "") |"' "$f"
    passes=$((passes + $(jq -r 'select(.status=="pass")|.name' "$f" | wc -l)))
    fails=$((fails + $(jq -r 'select(.status=="fail")|.name' "$f" | wc -l)))
    skips=$((skips + $(jq -r 'select(.status=="skip")|.name' "$f" | wc -l)))
  done
  echo
  echo "**pass ${passes} · fail ${fails} · skip ${skips}**"
  [ "$fails" -eq 0 ]
}

# ── cluster helpers ───────────────────────────────────────────────────────────

lane_lease_manifest() {
  local ns="$1"
  cat <<EOF
apiVersion: coordination.k8s.io/v1
kind: Lease
metadata:
  name: ${LANE_LEASE_NAME}
  namespace: ${ns}
  labels:
    gardenio.io/e2e-run: "true"
spec:
  holderIdentity: "${LANE_HOLDER}"
  leaseDurationSeconds: ${LANE_LEASE_SECONDS}
  acquireTime: "$(date -u +%Y-%m-%dT%H:%M:%S.000000Z)"
EOF
}

# lane_lease_state <ns> -> "free" | "stale <holder>" | "held <holder>"
lane_lease_state() {
  local ns="$1" json
  json=$("$KUBECTL" -n "$ns" get lease "$LANE_LEASE_NAME" -o json 2>/dev/null) || { echo free; return; }
  local holder acquired dur
  holder=$(jq -r '.spec.holderIdentity // ""' <<<"$json")
  acquired=$(jq -r '.spec.acquireTime // ""' <<<"$json")
  dur=$(jq -r '.spec.leaseDurationSeconds // 0' <<<"$json")
  if lane_lease_expired "$acquired" "$dur"; then echo "stale $holder"; else echo "held $holder"; fi
}

lane_lease_release() {
  local ns="$1"
  "$KUBECTL" -n "$ns" delete lease "$LANE_LEASE_NAME" --ignore-not-found >/dev/null
}

# lane_wipe <ns>: everything a run put there, nothing the operator did.
lane_wipe() {
  local ns="$1"
  log "wipe $ns"
  if "$HELM" -n "$ns" status "$ns" >/dev/null 2>&1; then
    "$HELM" -n "$ns" uninstall "$ns" --wait --timeout 5m || log "helm uninstall $ns returned non-zero; continuing with the raw wipe"
  fi
  # Agent pods the controller spawned are not Helm-managed; neither are the
  # per-run Secrets. Delete by kind, not by label, for workloads.
  "$KUBECTL" -n "$ns" delete jobs,cronjobs,deployments,statefulsets,replicasets,pods,services,persistentvolumeclaims,configmaps,poddisruptionbudgets,networkpolicies \
    --all --ignore-not-found --wait=true --timeout=5m >/dev/null 2>&1 || true
  # Operator-owned objects share the namespace: Role/RoleBinding/ResourceQuota
  # (label gardenio.io/e2e-pool) and the `ecr`/`ghcr` pull Secrets (no label).
  # Everything Helm created carries app.kubernetes.io/managed-by=Helm, and
  # everything this script created carries gardenio.io/e2e-run. Secrets only,
  # never the Lease: install wipes right after acquire, and deleting the lease
  # there freed the namespace for the next run, which then wiped this run's
  # install mid-wait (gd-mS5EZeWhZF). lane_lease_release is the only way out.
  "$KUBECTL" -n "$ns" delete secrets,serviceaccounts,roles,rolebindings -l app.kubernetes.io/managed-by=Helm --ignore-not-found >/dev/null 2>&1 || true
  "$KUBECTL" -n "$ns" delete secrets -l "$LANE_RUN_LABEL" --ignore-not-found >/dev/null 2>&1 || true
  # Helm's own release-history Secrets (sh.helm.release.v1.*) survive a failed
  # uninstall; they carry owner=helm, not managed-by.
  "$KUBECTL" -n "$ns" delete secrets -l owner=helm --ignore-not-found >/dev/null 2>&1 || true
}

# lane_acquire: lease a pool namespace; prints its name. Sweeps stale leases on
# the way. Polls up to LANE_ACQUIRE_TIMEOUT when every namespace is held.
lane_acquire() {
  local deadline=$(( $(date +%s) + LANE_ACQUIRE_TIMEOUT )) ns state
  while :; do
    for ns in $LANE_POOL; do
      state=$(lane_lease_state "$ns")
      case "$state" in
        stale*) log "$ns: lease ${state#stale } expired; sweeping"; lane_wipe "$ns"; lane_lease_release "$ns" ;;
        held*)  log "$ns: held by ${state#held }"; continue ;;
      esac
      if lane_lease_manifest "$ns" | "$KUBECTL" create -f - >/dev/null 2>&1; then
        log "$ns: leased as $LANE_HOLDER"
        echo "$ns"
        return 0
      fi
      log "$ns: lost the create race; trying the next namespace"
    done
    [ "$(date +%s)" -lt "$deadline" ] || die "no free pool namespace after ${LANE_ACQUIRE_TIMEOUT}s (pool: $LANE_POOL)"
    sleep "$LANE_ACQUIRE_POLL"
  done
}

lane_sweep() {
  local ns state
  for ns in $LANE_POOL; do
    state=$(lane_lease_state "$ns")
    case "$state" in
      stale*) log "$ns: lease ${state#stale } expired; sweeping"; lane_wipe "$ns"; lane_lease_release "$ns" ;;
      *) log "$ns: $state" ;;
    esac
  done
}

# lane_assert_tenant <ns>: prove, from inside the runner, that the identity
# this job carries is the tenant Role and nothing more. Every line of
# LANE_TENANT_DENY must come back "no": the operator's ResourceQuota and
# LimitRange are the ceiling the runner must not be able to raise or delete,
# and the Namespace carries the Pod Security "baseline" label the runner must
# not be able to remove (gd-LXD71HHC8n). The direct verbs alone are a blind
# spot: `escalate roles` lets a subject write a Role granting rules it does
# not hold and `bind roles`/`bind clusterroles` lets it reference one, so a
# Role with verbs ["*"] on roles/rolebindings could lift the ceiling while
# every direct can-i printed "no" (operator hold on gd-LvbhX557UD). Those
# three are in the deny list, and lane_assert_tenant_positive_control then
# tries the attack itself and demands Forbidden. Every line of
# LANE_TENANT_ALLOW must be "yes", or helm install is about to 403 anyway.
# Mirrors the operator's `--as=system:serviceaccount:arc-runners:gardenio-e2e-runner`
# check in rbac/namespace-pool.yaml; the chart test
# gardenio/helm/gardenio/tests/e2e_lane_namespace_pool_test.go proves the same
# over the manifest offline.
LANE_TENANT_DENY="${LANE_TENANT_DENY:-update resourcequotas,delete resourcequotas,create resourcequotas,update limitranges,delete limitranges,create limitranges,patch namespaces,delete namespaces,create clusterroles,create clusterrolebindings,escalate roles,bind roles,escalate clusterroles,bind clusterroles}"
LANE_TENANT_ALLOW="${LANE_TENANT_ALLOW:-create pods,create secrets,create roles,create leases.coordination.k8s.io}"
LANE_TENANT_PROBE_ROLE="${LANE_TENANT_PROBE_ROLE:-gardenio-e2e-escalation-probe}"

# lane_assert_tenant_positive_control <ns>: the can-i lines say what RBAC
# believes; this says what the API does. Create (server-side dry run, so
# nothing is ever persisted even if the fence is down) a Role granting
# `update resourcequotas` as the runner. RBAC's grant-time escalation check
# must refuse it with Forbidden ("attempting to grant RBAC permissions not
# currently held"). A success means the runner holds `escalate` on roles and
# the ceiling is decorative; any other error (API unreachable, no `create
# roles`) is also a refusal to install, because the control proved nothing.
lane_assert_tenant_positive_control() {
  local ns="$1" out rc=0
  out=$(printf 'apiVersion: rbac.authorization.k8s.io/v1\nkind: Role\nmetadata:\n  name: %s\n  namespace: %s\nrules:\n  - apiGroups: [""]\n    resources: [resourcequotas]\n    verbs: [update]\n' \
    "$LANE_TENANT_PROBE_ROLE" "$ns" | "$KUBECTL" -n "$ns" create -f - --dry-run=server 2>&1) || rc=$?
  if [ "$rc" -eq 0 ]; then
    # Dry run: nothing to clean up. Delete defensively anyway in case a
    # kubectl ignored --dry-run; the name is ours and label-free deletes of a
    # single named object never reach operator-owned RBAC.
    "$KUBECTL" -n "$ns" delete role "$LANE_TENANT_PROBE_ROLE" --ignore-not-found >/dev/null 2>&1 || true
    log "TENANT BOUNDARY BROKEN: the runner CREATED a Role granting update resourcequotas in $ns (RBAC escalation check did not refuse it); it holds escalate on roles"
    return 1
  fi
  case "$out" in
    *[Ff]orbidden*) log "$ns: positive control held (Role granting update resourcequotas refused: Forbidden)"; return 0 ;;
    *) log "TENANT BOUNDARY UNPROVEN: creating the probe Role in $ns failed for a reason other than Forbidden: ${out:-<empty>}"; return 1 ;;
  esac
}

lane_assert_tenant() {
  local ns="$1" entry verb res answer bad=0
  IFS=, read -r -a deny <<<"$LANE_TENANT_DENY"
  IFS=, read -r -a allow <<<"$LANE_TENANT_ALLOW"
  for entry in "${deny[@]}"; do
    verb="${entry%% *}"; res="${entry#* }"
    answer=$("$KUBECTL" -n "$ns" auth can-i "$verb" "$res" 2>/dev/null || true)
    case "$answer" in
      no) ;;
      *) log "TENANT BOUNDARY BROKEN: can-i $verb $res in $ns answered '${answer:-<empty>}' (want no)"; bad=1 ;;
    esac
  done
  for entry in "${allow[@]}"; do
    verb="${entry%% *}"; res="${entry#* }"
    answer=$("$KUBECTL" -n "$ns" auth can-i "$verb" "$res" 2>/dev/null || true)
    case "$answer" in
      yes) ;;
      *) log "tenant Role missing: can-i $verb $res in $ns answered '${answer:-<empty>}' (want yes)"; bad=1 ;;
    esac
  done
  lane_assert_tenant_positive_control "$ns" || bad=1
  [ "$bad" -eq 0 ] || die "runner identity in $ns is not the gardenio-e2e-tenant boundary; refusing to install (re-apply rbac/namespace-pool.yaml)"
  log "$ns: tenant boundary holds (${#deny[@]} denied, ${#allow[@]} allowed, escalation probe Forbidden)"
}

lane_mint_secrets() {
  local ns="$1"
  # 48 hex chars for the daemon token; 32 raw bytes base64 for coop (persist.rs
  # accepts base64 or raw key material).
  "$KUBECTL" -n "$ns" create secret generic "$LANE_TOKEN_SECRET" \
    --from-literal="token=$(openssl rand -hex 24)" >/dev/null
  "$KUBECTL" -n "$ns" create secret generic "$LANE_COOP_SECRET" \
    --from-literal="key=$(openssl rand -base64 32)" >/dev/null
  "$KUBECTL" -n "$ns" label secret "$LANE_TOKEN_SECRET" "$LANE_COOP_SECRET" "$LANE_RUN_LABEL" --overwrite >/dev/null
}

# Install the PACKED chart, never the source dir (gd-mS5EZeWhZF). Helm stores
# the whole chart in the release Secret, which may not exceed 1 MiB: the raw
# dir's template comments put sh.helm.release.v1.gardenio-e2e-0.v1 at ~1.14 MB
# and the install failed before applying anything. Packed the way
# release-deploy.sh packs it (comments stripped, render self-checked) it is
# ~612 KB (scripts/check-helm-release-size.sh --values <this overlay>).
lane_helm_install() {
  local ns="$1" tag="$2" pack_dir pack_out tgz
  pack_dir="$(mktemp -d)"
  # Expanded now (pack_dir is local) and cleared on first fire: a RETURN trap
  # set in a function stays set after it returns (see lane_seed).
  # shellcheck disable=SC2064
  trap "rm -rf '$pack_dir'; trap - RETURN" RETURN
  pack_out="$(bash "$LANE_PACK" "$CHART_DIR" "$pack_dir" --version "$tag" --app-version "$tag")" \
    || die "chart pack failed ($LANE_PACK); refusing to install the unpacked chart, it overflows the release Secret"
  tgz="$(tail -n 1 <<<"$pack_out")"
  [ -f "$tgz" ] || die "chart pack produced no file (got '$tgz')"
  log "helm upgrade --install $ns $(basename "$tgz") (imageTag=$tag)"
  "$HELM" upgrade --install "$ns" "$tgz" -n "$ns" \
    -f "$LANE_VALUES" --set "global.imageTag=$tag" \
    --wait --timeout "$LANE_HELM_TIMEOUT"
}

lane_install() {
  local ns="$1" tag="${2:-$(lane_newest_tag)}"
  [ -f "$LANE_VALUES" ] || die "overlay not found: $LANE_VALUES"
  lane_assert_tenant "$ns"
  lane_wipe "$ns"
  lane_mint_secrets "$ns"
  lane_helm_install "$ns" "$tag"
  NAMESPACE="$ns" "$SCRIPT_DIR/wait-for-stack-ready.sh"
}

lane_token() {
  "$KUBECTL" -n "$1" get secret "$LANE_TOKEN_SECRET" -o jsonpath='{.data.token}' | base64 -d
}

lane_seed() {
  local ns="$1" pf_pid
  "$KUBECTL" -n "$ns" port-forward "svc/${ns}-beads" "${LANE_PF_PORT}:8080" >/tmp/lane-seed-pf.log 2>&1 &
  pf_pid=$!
  # Expanded now and cleared on first fire. A RETURN trap outlives the
  # function that set it, so the old single-quoted form fired again when
  # lane_main returned, where pf_pid is unbound: `pf_pid: unbound variable`
  # failed live-setup after a good seed (gd-mS5EZeWhZF, PR #801).
  # shellcheck disable=SC2064
  trap "kill $pf_pid 2>/dev/null || true; trap - RETURN" RETURN
  for _ in $(seq 1 30); do
    curl -sf --max-time 2 -o /dev/null "http://localhost:${LANE_PF_PORT}/v1/health" && break
    sleep 1
  done
  curl -sf --max-time 2 -o /dev/null "http://localhost:${LANE_PF_PORT}/v1/health" \
    || { cat /tmp/lane-seed-pf.log >&2; die "beads not reachable on :${LANE_PF_PORT} for seeding"; }
  BEADS_HTTP_URL="http://localhost:${LANE_PF_PORT}" BEADS_TOKEN="$(lane_token "$ns")" \
    "$SCRIPT_DIR/seed-types-and-users.sh"
}

lane_teardown() {
  local ns="$1"
  lane_wipe "$ns"
  lane_lease_release "$ns"
  log "$ns released"
}

lane_main() {
  local cmd="${1:-}"; shift || true
  case "$cmd" in
    acquire)    lane_acquire ;;
    install)    lane_install "${1:?namespace}" "${2:-}" ;;
    seed)       lane_seed "${1:?namespace}" ;;
    token)      lane_token "${1:?namespace}" ;;
    teardown)   lane_teardown "${1:?namespace}" ;;
    assert-tenant) lane_assert_tenant "${1:?namespace}" ;;
    sweep)      lane_sweep ;;
    newest-tag) lane_newest_tag ;;
    skip-list)  lane_skip_list "$@" ;;
    report)     lane_report_merge "$@" ;;
    *) sed -n '2,20p' "${BASH_SOURCE[0]}" >&2; exit 2 ;;
  esac
}

if [ "${LANE_SOURCE_ONLY:-}" != "1" ]; then
  lane_main "$@"
fi
