#!/usr/bin/env bash
set -euo pipefail

# release-deploy.sh -- roll a calver to a helm release, verify it landed, and
# roll back automatically if it didn't.
#
# Gates (evaluated in order, first match wins):
#   1. Pause switch    -- ConfigMap release-deployer-pause exists     -> skip
#   2. Helm busy       -- `helm status` .info.status != "deployed"    -> skip
#   3. Not newer       -- target calver <= the release's current tag  -> skip
#   3b. Values drift    -- live values (minus global.imageTag) differ
#                          from <values> as it was at the deployed
#                          tag (a hand edit), or from <values> when
#                          that cannot be read; a key whose live value
#                          equals <values> at the new tag is reconciled,
#                          not drift                                  -> skip
#                          (but ALSO reported as a failure -- see rule 8 below)
#   4. Tag not green    -- the tag's own push runs of images.yml,
#                          ci.yml and checks.yml at its commit are not
#                          all success (a ci.yml run must really have
#                          built; owner 2026-11-03)               -> skip
#                          (a RED tag -- a completed run that did not
#                          succeed and no green one -- ALSO files one
#                          escalation per workflow; see rule 10 below)
# then:
#   4b. render the Keycloak theme ConfigMap from the checkout
#      (gardenio/scripts/keycloak-theme-configmap.py) and `kubectl apply
#      --server-side` it (--dry-run=server under --dry-run), then
#   4c. server-side apply the edge manifests from the checkout
#      (gardenio-ziti/public-edge.yaml, viewer-authproxy.yaml): ConfigMaps,
#      `nginx -t` in every edge pod, reload, then the rest; a failed test
#      rolls the nginx ConfigMap back (--dry-run=server under --dry-run;
#      only for --namespace gardenio), then
#   5. pack the chart (scripts/pack-gardenio-chart.sh), then
#      `helm upgrade --atomic --wait` on the .tgz (or --dry-run=server
#      under --dry-run)
#   6. `verify-image-tags.sh` -- on failure, `helm rollback` to the previous
#      revision
#   9. record the deployment on the release (POST
#      /v1/releases/<tag>/deployments, the route `kd release deploy` calls):
#      <calver> at the new helm revision after a verified roll; the previous
#      tag at the rollback's revision, by=rollback, after a rollback that left
#      the release `deployed`. Best effort, like rule 8, and never under
#      --dry-run (releases-design.md §4.3, plan gd-UZKZ9H25UD step 4).
#   10. on a rule 4 skip, file one escalation (POST /v1/beads, type
#      escalation, idempotency_key release-red:<calver>:<workflow>) for each
#      required workflow that is red at the tag, naming its runs, failed jobs
#      and the quarantine-or-revert recovery (owner 2026-11-03). Best effort,
#      like rule 8, and never under --dry-run.
#
# Usage:
#   release-deploy.sh <calver> [--namespace <ns>] [--release <name>] \
#                      [--values <file>] [--dry-run]
#
# Exit codes:
#   0 - deployed and verified (or, under --dry-run, every gate + the
#       server-side dry run passed)
#   3 - skipped: paused, helm busy, not newer, values drift, or the tag's
#       images.yml / ci.yml / checks.yml not green
#   1 - failed before anything changed (bad calver, pause/helm/gh/jq error,
#       release missing), or failed and the release is back on `deployed`
#       (atomic rollback or verify-failure rollback succeeded), or the
#       release was left `failed` and this run rolled it back to its newest
#       completed revision (no upgrade this run)
#   2 - failed AND the rollback also failed (or the release is not
#       `deployed` after a failed --atomic upgrade, or an edge ConfigMap
#       rollback failed after a failed `nginx -t`) -- manual intervention
#
# Fail closed: every probe that cannot give a definite answer (kubectl/helm/
# gh erroring, output that is not parseable JSON) is exit 1 with a report,
# never a skip and never a crash with an exit code outside the list above.
#
# Env (optional -- enables reporting, rules 8, 9 and 10):
#   KD_URL           e.g. https://kd.gardenio.internal
#   KD_TOKEN         bearer token (legacy shared token; used only when the
#                    machine-principal trio below is not set)
#   KD_OPERATED_BY   value for the X-Operated-By header (static token only)
#   KD_OIDC_TOKEN_URL, KD_OIDC_CLIENT_ID, KD_OIDC_CLIENT_SECRET_FILE
#                    machine principal (gd-5QYwXtV7IA): when all three are
#                    set, reporting authenticates as the Keycloak service
#                    account (client-credentials grant) and neither KD_TOKEN
#                    nor X-Operated-By is sent. The secret is read from the
#                    file by curl and never appears in argv or the log.
#   KD_OIDC_CA_FILE  optional PEM CA curl trusts for KD_OIDC_TOKEN_URL (the
#                    keycloak-runtime in-cluster CA, gd-lYPZlslOM7)
#
#   RELEASE_DEPLOY_PROJECT, RELEASE_DEPLOY_TARGET
#                    the project and release target rule 9 records the
#                    deployment on (default gardenio, prod)
#
# Companion test: release-deploy_test.sh

REPO="mattbakerconsulting/gardenio"

usage() {
  echo "Usage: $0 <calver> [--namespace <ns>] [--release <name>] [--values <file>] [--dry-run]" >&2
}

CALVER="${1:-}"
if [ -z "$CALVER" ] || [[ "$CALVER" == --* ]]; then
  usage
  exit 1
fi
shift

# Validate the calver before it goes anywhere near `--set` (a value like
# `2026.99.1,global.foo=bar` would otherwise inject extra helm values).
if ! [[ "$CALVER" =~ ^[0-9]{4}\.[0-9]+\.[0-9]+$ ]]; then
  echo "release-deploy.sh: invalid calver (want YYYY.N.N): ${CALVER}" >&2
  exit 1
fi

NAMESPACE="gardenio"
RELEASE="gardenio"
VALUES=""
DRY_RUN=false

while [ $# -gt 0 ]; do
  case "$1" in
    --namespace)
      [ $# -ge 2 ] || { echo "release-deploy.sh: --namespace requires a value" >&2; exit 1; }
      NAMESPACE="$2"
      shift 2
      ;;
    --release)
      [ $# -ge 2 ] || { echo "release-deploy.sh: --release requires a value" >&2; exit 1; }
      RELEASE="$2"
      shift 2
      ;;
    --values)
      [ $# -ge 2 ] || { echo "release-deploy.sh: --values requires a value" >&2; exit 1; }
      VALUES="$2"
      shift 2
      ;;
    --dry-run)
      DRY_RUN=true
      shift
      ;;
    *)
      echo "release-deploy.sh: unknown flag: $1" >&2
      usage
      exit 1
      ;;
  esac
done

SCRIPT_DIR="$(cd "$(dirname "$0")" && pwd)"

if ! TOPLEVEL="$(git -C "$SCRIPT_DIR" rev-parse --show-toplevel 2>&1)"; then
  echo "release-deploy.sh: git rev-parse --show-toplevel failed: ${TOPLEVEL}" >&2
  exit 1
fi

CHART_DIR="${TOPLEVEL}/gardenio/helm/gardenio"
[ -n "$VALUES" ] || VALUES="${TOPLEVEL}/deploy-values/gardenio-k3s.yaml"

OUTLOG="$(mktemp)"
# ERRF -- stderr of commands whose stdout is parsed as JSON. Kept apart from
# stdout so a warning (helm v4 prints flag deprecations on stderr) can never
# corrupt the JSON handed to jq.
ERRF="$(mktemp)"
# PACK_DIR -- holds the comment-stripped chart .tgz built at rule 5.
PACK_DIR="$(mktemp -d)"
# PREV_VALUES -- the overlay at the previously deployed tag (rule 3b).
PREV_VALUES="$(mktemp)"
trap 'rm -rf "$OUTLOG" "$ERRF" "$PACK_DIR" "$PREV_VALUES"' EXIT

log() { printf '%s\n' "$*" | tee -a "$OUTLOG"; }

# errtext -- the captured stderr of the last command, for messages.
errtext() { cat "$ERRF" 2>/dev/null || true; }

REV_BEFORE="unknown"
REV_AFTER=""

# get_helm_revision -- best-effort current revision number of $RELEASE, for
# the before/after fields in rule 8's report. Never fails the script: a
# read-only status probe going wrong is not itself a deploy failure.
get_helm_revision() {
  local out
  if out="$(helm status "$RELEASE" -n "$NAMESPACE" -o json 2>/dev/null | jq -r '.version // empty' 2>/dev/null)" && [ -n "$out" ]; then
    printf '%s' "$out"
  else
    printf 'unknown'
  fi
}

# kd_auth_args -- fills KD_AUTH_ARGS with the curl header args for a daemon call.
# Machine principal when KD_OIDC_* is set (token fetched once per run; a failed
# fetch leaves no Authorization header, so the call fails and reporting stays
# best-effort -- the static KD_TOKEN is never substituted); legacy KD_TOKEN +
# X-Operated-By otherwise.
KD_MACHINE_TOKEN=""
kd_auth_args() {
  KD_AUTH_ARGS=()
  if [ -n "${KD_OIDC_TOKEN_URL:-}" ] && [ -n "${KD_OIDC_CLIENT_ID:-}" ] && [ -n "${KD_OIDC_CLIENT_SECRET_FILE:-}" ]; then
    if [ -z "$KD_MACHINE_TOKEN" ]; then
      # KD_OIDC_CA_FILE: the token URL is the keycloak-runtime https port,
      # signed by the chart's in-cluster CA (gd-lYPZlslOM7).
      local ca_args=()
      [ -n "${KD_OIDC_CA_FILE:-}" ] && ca_args=(--cacert "$KD_OIDC_CA_FILE")
      KD_MACHINE_TOKEN="$(curl -sf ${ca_args[@]+"${ca_args[@]}"} -X POST "$KD_OIDC_TOKEN_URL" \
        --data-urlencode "grant_type=client_credentials" \
        --data-urlencode "client_id=${KD_OIDC_CLIENT_ID}" \
        --data-urlencode "client_secret@${KD_OIDC_CLIENT_SECRET_FILE}" 2>/dev/null \
        | jq -r '.access_token // empty' 2>/dev/null)" || KD_MACHINE_TOKEN=""
    fi
    [ -n "$KD_MACHINE_TOKEN" ] && KD_AUTH_ARGS=(-H "Authorization: Bearer ${KD_MACHINE_TOKEN}")
    return 0
  fi
  KD_AUTH_ARGS=(-H "Authorization: Bearer ${KD_TOKEN:-}" -H "X-Operated-By: ${KD_OPERATED_BY:-}")
}

# post_bead <type> <priority-or-empty> <title> <description> <status-or-empty>
# [short-title] -- POSTs one bead to $KD_URL/v1/beads. kbeads refuses a
# machine-filed bug with no short_title (BEADS_SHORT_TITLE_GATE=enforce), so
# a bug must pass one (gd-440OOVBHi7). Returns non-zero on failure; callers must never
# let that change the script's own exit code (rule 8: "reporting failure
# never changes the exit code").
post_bead() {
  local type="$1" priority="$2" title="$3" desc="$4" status_field="$5" short_title="${6:-}"
  local body
  body="$(jq -nc \
    --arg title "$title" \
    --arg type "$type" \
    --arg desc "$desc" \
    --arg status "$status_field" \
    --arg short "$short_title" \
    '{title: $title, type: $type, description: $desc, labels: ["project:gardenio", "release-deploy"]}
     | if $status != "" then . + {status: $status} else . end
     | if $short != "" then . + {short_title: $short} else . end')" || return 1
  if [ -n "$priority" ]; then
    body="$(printf '%s' "$body" | jq -c --argjson priority "$priority" '. + {priority: $priority}')" || return 1
  fi
  kd_auth_args
  curl -sf -X POST "${KD_URL%/}/v1/beads" \
    ${KD_AUTH_ARGS[@]+"${KD_AUTH_ARGS[@]}"} \
    -H "Content-Type: application/json" \
    -d "$body" >/dev/null 2>&1
}

# open_bug_exists <title> -- true when an open release-deploy bug with this
# exact title is already filed. Fails OPEN (returns false) on any query or
# parse error, so a flaky read can only cause a duplicate, never a lost bug.
open_bug_exists() {
  local title="$1" resp
  kd_auth_args
  resp="$(curl -sf "${KD_URL%/}/v1/beads?type=bug&status=open&labels=release-deploy&limit=500" \
    ${KD_AUTH_ARGS[@]+"${KD_AUTH_ARGS[@]}"} 2>/dev/null)" || return 1
  printf '%s' "$resp" | jq -e --arg t "$title" 'any(.beads[]?; .title == $t)' >/dev/null 2>&1
}

# report_bead <outcome> <file-bug: true|false> -- rule 8. Files a type=event
# ledger bead (closed -- no lifecycle) when KD_URL is set; ALSO files an open
# type=bug bead at priority 1 when file-bug is true.
#
# The CronJob runs this every 15 minutes, so two cases stay quiet:
#   - steady-state skips ("paused", "not newer") file nothing at all;
#   - a bug whose exact title is already open (a persistent values drift or
#     error re-hit every tick) files nothing -- the open bug is the record.
report_bead() {
  local outcome="$1" file_bug="$2"
  [ -n "${KD_URL:-}" ] || return 0
  case "$outcome" in
    paused | "not newer") return 0 ;;
  esac

  local tail_out title desc short_title
  tail_out="$(tail -n 40 "$OUTLOG" 2>/dev/null || true)"
  # Titles carry only the first line of the outcome (which may embed
  # multi-line error text); the description keeps all of it.
  title="release-deploy ${CALVER}: ${outcome%%$'\n'*}"
  # kbeads rejects titles over 500 characters (400 "title: must be 500
  # characters or fewer"), and a broad values drift lists every drifted key
  # here, so cap it; the description keeps the full outcome. The cap is
  # deterministic, so the open-bug dedup below still matches across ticks.
  if [ "${#title}" -gt 480 ]; then
    title="${title:0:477}..."
  fi
  # The bug's short_title (kbeads: at most 40 characters, no ids) names
  # the kind of failure; the title carries the tag and detail.
  case "$outcome" in
    "values drift"*) short_title="reconcile hand-edited release values" ;;
    *) short_title="fix the release deployer run" ;;
  esac
  desc="$(printf 'outcome: %s\nhelm revision before: %s\nhelm revision after: %s\n\nlast 40 lines of output:\n%s' \
    "$outcome" "$REV_BEFORE" "${REV_AFTER:-$REV_BEFORE}" "$tail_out")"

  if [ "$file_bug" = "true" ] && open_bug_exists "$title"; then
    log "open bug already filed for this outcome; not re-reporting"
    return 0
  fi

  post_bead "event" "" "$title" "$desc" "closed" \
    || log "WARN: failed to report event bead to ${KD_URL}"

  if [ "$file_bug" = "true" ]; then
    post_bead "bug" "1" "$title" "$desc" "" "$short_title" \
      || log "WARN: failed to report bug bead to ${KD_URL}"
  fi
}

# report_deployment <tag> <revision> [by] -- rule 9. Records on the release
# <tag> of $RELEASE_DEPLOY_PROJECT that it runs on $RELEASE_DEPLOY_TARGET at
# helm revision <revision>. by defaults to the caller's principal on the
# daemon. Best effort: a release that was never recorded (404), a refused
# write or a daemon that is down is logged and never changes the exit code.
# The release-deploy event bead (rule 8) stays the log of the run.
report_deployment() {
  local tag="$1" revision="$2" by="${3:-}" body
  [ -n "${KD_URL:-}" ] || return 0
  [ -n "$tag" ] || return 0
  [ "$revision" = "unknown" ] && revision=""
  body="$(jq -nc \
    --arg target "${RELEASE_DEPLOY_TARGET:-prod}" \
    --arg by "$by" \
    --arg revision "$revision" \
    --arg tag "$tag" \
    '{target: $target, via: "script", by: $by, revision: $revision, image_tag: $tag}
     | with_entries(select(.value != ""))')" || {
    log "WARN: could not build the deployment body for ${tag}; not recorded"
    return 0
  }
  kd_auth_args
  if curl -sf -X POST "${KD_URL%/}/v1/releases/${tag}/deployments?project=${RELEASE_DEPLOY_PROJECT:-gardenio}" \
    ${KD_AUTH_ARGS[@]+"${KD_AUTH_ARGS[@]}"} \
    -H "Content-Type: application/json" \
    -d "$body" >/dev/null 2>&1; then
    log "recorded deployment: ${tag} on ${RELEASE_DEPLOY_TARGET:-prod} at helm revision ${revision:-unknown}${by:+ by ${by}}"
  else
    log "WARN: failed to record the deployment of ${tag} on ${KD_URL} (is the release recorded? gb release record ${tag})"
  fi
}

# report_and_exit <code> <outcome> [force-bug] -- the single exit path.
# Files bugs unconditionally on 1/2 (rule 8); force-bug=true additionally
# files one on an exit-3 (rule 3b's values-drift exception).
report_and_exit() {
  local code="$1" outcome="$2" force_bug="${3:-false}"
  local file_bug=false
  if [ "$code" = "1" ] || [ "$code" = "2" ]; then
    file_bug=true
  fi
  [ "$force_bug" = "true" ] && file_bug=true
  log "$outcome"
  report_bead "$outcome" "$file_bug"
  exit "$code"
}

# jq_get <var> <label> <json> <jq-args...> -- runs jq over <json> and stores
# stdout in <var>. <json> must parse as a JSON object (or null); anything
# else, or jq itself failing, is exit 1 with a report -- never a set -e crash.
jq_get() {
  local __var="$1" __label="$2" __in="$3"
  shift 3
  local __out
  if ! jq -e 'type == "object" or type == "null"' >/dev/null 2>&1 <<<"$__in"; then
    report_and_exit 1 "error: ${__label}: output is not a JSON object: ${__in}"
  fi
  if ! __out="$(jq "$@" <<<"$__in" 2>"$ERRF")"; then
    report_and_exit 1 "error: ${__label}: jq failed: $(errtext)"
  fi
  printf -v "$__var" '%s' "$__out"
}

# --- Rule 1: pause switch ----------------------------------------------

# Only an explicit NotFound means "not paused". Any other failure (API
# unreachable, RBAC, kubectl missing) is unknown -> fail closed.
if kubectl get configmap release-deployer-pause -n "$NAMESPACE" >/dev/null 2>"$ERRF"; then
  report_and_exit 3 "paused"
fi
PAUSE_ERR="$(errtext)"
if ! grep -qE '\(NotFound\)|configmaps? "release-deployer-pause" not found' <<<"$PAUSE_ERR"; then
  report_and_exit 1 "error: cannot check pause switch: ${PAUSE_ERR:-kubectl failed with no stderr}"
fi

# --- Rule 2: helm busy ---------------------------------------------------

# A failing `helm status` is never a skip: cluster unreachable / RBAC is an
# unknown state, and a missing release is an error for this script (it only
# ever upgrades an existing release).
if ! STATUS_JSON="$(helm status "$RELEASE" -n "$NAMESPACE" -o json 2>"$ERRF")"; then
  STATUS_ERR="$(errtext)"
  if grep -qiE 'not found' <<<"$STATUS_ERR"; then
    report_and_exit 1 "error: helm release ${RELEASE} not found in ${NAMESPACE}: ${STATUS_ERR}"
  fi
  report_and_exit 1 "error: helm status failed: ${STATUS_ERR:-no stderr}"
fi

jq_get HELM_STATUS "helm status" "$STATUS_JSON" -r '.info.status // empty'
if [ -z "$HELM_STATUS" ]; then
  report_and_exit 1 "error: helm status returned no .info.status"
fi
# A `failed` release is what an upgrade leaves when its --atomic rollback
# also failed (2026-11-05: a spot reclaim took the cert-manager webhook
# mid-roll, so the 2026.309.18 upgrade and its rollback both failed on
# webhook.cert-manager.io). Unlike pending-*, nothing is running, so waiting
# changes nothing: every later run skipped as "helm busy" until a person
# rolled it back. Retry that rollback here, to the newest revision that
# completed, and stop; the next run upgrades from a deployed release.
heal_failed_release() {
  local hist good good_tag
  jq_get REV_BEFORE "helm status" "$STATUS_JSON" -r '.version // "unknown"'
  if ! hist="$(helm history "$RELEASE" -n "$NAMESPACE" --max 20 -o json 2>"$ERRF")"; then
    report_and_exit 2 "helm release failed and helm history failed: $(errtext) -- manual intervention needed"
  fi
  good="$(jq -r '[.[] | select(.status == "superseded" or .status == "deployed")
                      | select(.description | test("^(Install complete|Upgrade complete|Rollback to [0-9]+)$"))
                      | .revision] | max // empty' <<<"$hist" 2>/dev/null)" || good=""
  if [ -z "$good" ]; then
    report_and_exit 2 "helm release failed and no completed revision in its last 20 to roll back to -- manual intervention needed"
  fi
  good_tag="$(jq -r --argjson r "$good" '.[] | select(.revision == $r) | .app_version // empty' <<<"$hist" 2>/dev/null)" || good_tag=""
  if $DRY_RUN; then
    report_and_exit 3 "helm release failed; would roll back to revision ${good} (dry run)"
  fi
  if ! ROLLBACK_OUT="$(helm rollback "$RELEASE" "$good" -n "$NAMESPACE" --wait --timeout 20m 2>&1)"; then
    log "$ROLLBACK_OUT"
    REV_AFTER="$(get_helm_revision)"
    report_and_exit 2 "helm release failed and the rollback to revision ${good} failed -- manual intervention needed"
  fi
  log "$ROLLBACK_OUT"
  REV_AFTER="$(get_helm_revision)"
  report_deployment "$good_tag" "$REV_AFTER" rollback
  report_and_exit 1 "helm release was failed; rolled back to revision ${good} (${good_tag:-tag unknown}); the next run upgrades from there"
}

if [ "$HELM_STATUS" = "failed" ]; then
  heal_failed_release
fi
if [ "$HELM_STATUS" != "deployed" ]; then
  report_and_exit 3 "helm busy: ${HELM_STATUS}"
fi

jq_get REV_BEFORE "helm status" "$STATUS_JSON" -r '.version // "unknown"'

# --- Rule 3: not newer ----------------------------------------------------

if ! CURRENT_VALUES_JSON="$(helm get values "$RELEASE" -n "$NAMESPACE" -o json 2>"$ERRF")"; then
  report_and_exit 1 "error: helm get values failed: $(errtext)"
fi

jq_get CURRENT_TAG "helm get values" "$CURRENT_VALUES_JSON" -r '.global.imageTag // empty'
if [ -n "$CURRENT_TAG" ]; then
  TOP="$(printf '%s\n%s\n' "$CURRENT_TAG" "$CALVER" | sort -V | tail -1)"
  if [ "$CURRENT_TAG" = "$CALVER" ] || [ "$TOP" != "$CALVER" ]; then
    report_and_exit 3 "not newer"
  fi
fi

# --- Rule 3b: values drift -------------------------------------------------

# Drifted leaves of three values objects [live, baseline, new overlay],
# global.imageTag set aside: a leaf is drifted when live differs from the
# baseline, reconciled when live also equals the new overlay (review adopted
# the hand edit, gd-440OOVBHi7), and parked otherwise. Leaves are compared by
# path, so key order and absent-vs-null both count. Prints
# {"drift": [...], "reconciled": [...]} with dotted key names.
# Quoted heredoc: the jq program's $p etc. must reach jq unexpanded.
DRIFT_FILTER="$(cat <<'JQ'
def leaves: del(.global.imageTag) | [paths(scalars) as $p | {key: ($p | tojson), value: [getpath($p)]}] | from_entries;
(.[0] | leaves) as $cur | (.[1] | leaves) as $base | (.[2] | leaves) as $tgt
| [($cur + $base) | keys[] | select($cur[.] != $base[.])] as $drifted
| {drift: [$drifted[] | select($cur[.] != $tgt[.])], reconciled: [$drifted[] | select($cur[.] == $tgt[.])]}
| map_values([.[] | fromjson | join(".")] | unique)
JQ
)"

# values_json <var> <file> -- the YAML <file> as JSON in <var>, via yq or
# python3+PyYAML. Neither, or a parse failure, is exit 1 (fail closed).
values_json() {
  local __var="$1" __file="$2" __out
  if command -v yq >/dev/null 2>&1; then
    if ! __out="$(yq -o=json "$__file" 2>"$ERRF")"; then
      report_and_exit 1 "cannot check values drift: yq failed: $(errtext)"
    fi
  elif command -v python3 >/dev/null 2>&1 && python3 -c 'import yaml' >/dev/null 2>&1; then
    if ! __out="$(python3 -c 'import sys, json, yaml
with open(sys.argv[1]) as f:
    json.dump(yaml.safe_load(f) or {}, sys.stdout)' "$__file" 2>"$ERRF")"; then
      report_and_exit 1 "cannot check values drift: python3 yaml failed: $(errtext)"
    fi
  else
    report_and_exit 1 "cannot check values drift"
  fi
  printf -v "$__var" '%s' "$__out"
}

# previous_overlay -- writes <values> as it was at CURRENT_TAG, the overlay
# the live release was rolled with, to $PREV_VALUES. On any doubt it returns
# 1 with the reason in BASELINE_NOTE, and the caller compares with the new
# overlay instead. The deployer's checkout is a --depth 1 clone of the new
# tag, so a missing CURRENT_TAG is fetched from origin.
BASELINE_NOTE=""
previous_overlay() {
  local values_dir top_real rel shallow=()
  if [ -z "$CURRENT_TAG" ]; then
    BASELINE_NOTE="the release has no global.imageTag"
    return 1
  fi
  # CURRENT_TAG comes from the live release: never put a non-calver in a ref.
  if ! [[ "$CURRENT_TAG" =~ ^[0-9]{4}\.[0-9]+\.[0-9]+$ ]]; then
    BASELINE_NOTE="previous tag ${CURRENT_TAG} is not a calver"
    return 1
  fi
  if ! values_dir="$(cd "$(dirname "$VALUES")" 2>/dev/null && pwd -P)" \
    || ! top_real="$(cd "$TOPLEVEL" && pwd -P)"; then
    BASELINE_NOTE="cannot resolve ${VALUES}"
    return 1
  fi
  rel="${values_dir}/$(basename "$VALUES")"
  if [ "${rel#"${top_real}/"}" = "$rel" ]; then
    BASELINE_NOTE="${VALUES} is outside the checkout"
    return 1
  fi
  rel="${rel#"${top_real}/"}"
  if ! git -C "$TOPLEVEL" rev-parse -q --verify "refs/tags/${CURRENT_TAG}^{commit}" >/dev/null 2>&1; then
    [ "$(git -C "$TOPLEVEL" rev-parse --is-shallow-repository 2>/dev/null)" = "true" ] && shallow=(--depth 1)
    if ! git -C "$TOPLEVEL" fetch -q --no-tags ${shallow[@]+"${shallow[@]}"} origin \
      "+refs/tags/${CURRENT_TAG}:refs/tags/${CURRENT_TAG}" >/dev/null 2>&1; then
      BASELINE_NOTE="cannot fetch previous tag ${CURRENT_TAG}"
      return 1
    fi
  fi
  if ! git -C "$TOPLEVEL" show "refs/tags/${CURRENT_TAG}:${rel}" >"$PREV_VALUES" 2>/dev/null; then
    BASELINE_NOTE="${rel} is not in previous tag ${CURRENT_TAG}"
    return 1
  fi
}

# The baseline is the overlay at the previously deployed tag (P-R1,
# gd-foTtSpeOrc): live values equal to it mean nobody hand-edited the
# release, so the new tag's reviewed overlay may replace them; a difference
# is a hand edit and parks, unless the new overlay already carries the live
# value (reconciled, gd-440OOVBHi7). Without that proof, compare with the new overlay
# as before, so any overlay change parks too.
values_json TARGET_VALUES_JSON "$VALUES"
BASELINE_VALUES_JSON="$TARGET_VALUES_JSON"
DRIFT_SUFFIX=""
if previous_overlay; then
  values_json BASELINE_VALUES_JSON "$PREV_VALUES"
  log "values baseline: the overlay at ${CURRENT_TAG}"
else
  DRIFT_SUFFIX=" (compared with the new overlay: ${BASELINE_NOTE})"
  log "values baseline: the new overlay (${BASELINE_NOTE})"
fi

# The three objects go to jq on stdin: the overlay alone is larger than one
# argv string may be (MAX_ARG_STRLEN, 128 KiB).
if ! DRIFT_JSON="$(printf '%s\n%s\n%s\n' "$CURRENT_VALUES_JSON" "$BASELINE_VALUES_JSON" "$TARGET_VALUES_JSON" \
  | jq -s -c "if length == 3 and all(.[]; type == \"object\" or type == \"null\") then . else error(\"not three JSON objects\") end | ${DRIFT_FILTER}" 2>"$ERRF")"; then
  report_and_exit 1 "cannot check values drift: jq failed: $(errtext)"
fi
jq_get DRIFT_KEYS "values drift" "$DRIFT_JSON" -r '.drift | join(",")'
jq_get RECONCILED_KEYS "values drift" "$DRIFT_JSON" -r '.reconciled | join(",")'
if [ -n "$RECONCILED_KEYS" ]; then
  log "values drift reconciled (live equals the new overlay): ${RECONCILED_KEYS}"
fi
if [ -n "$DRIFT_KEYS" ]; then
  report_and_exit 3 "values drift: ${DRIFT_KEYS}${DRIFT_SUFFIX}" true
fi

# Tags deploy-values pins per component (e.g. beads.image.tag) are expected
# on those pods, so verify must not fail on them.
VERIFY_ARGS=("$CALVER" "--namespace=${NAMESPACE}")
# Quoted heredoc: the jq program's $p must reach jq unexpanded.
PINNED_FILTER="$(cat <<'JQ'
[paths(scalars) as $p
 | select(($p | length) >= 3 and $p[-1] == "tag" and $p[-2] == "image" and $p[0] != "global")
 | getpath($p) | tostring] | unique | .[]
JQ
)"
jq_get PINNED_TAGS "values file ${VALUES}" "$TARGET_VALUES_JSON" -r "$PINNED_FILTER"
while IFS= read -r pin; do
  [ -n "$pin" ] && VERIFY_ARGS+=("--allow-tag=${pin}")
done <<<"$PINNED_TAGS"

# --- Rule 4: the tag's own CI is green ------------------------------------
#
# Owner 2026-11-03: "we don't care for ci on main, rather on the release
# branch". ci.yml and checks.yml no longer run on main pushes (19 of the last
# 30 ci.yml main runs were cancelled by the next merge); they run on the tag
# push, beside images.yml, and this rule rolls a tag only once ALL THREE have
# a successful push run of the tag at the commit it points at. Until then
# every tick skips here (exit 3), naming the workflow that is not green. A red
# tag is never rolled, and it does not stay quiet either: rule 10 files one
# escalation per red workflow (owner 2026-11-03), whose recovery is a
# quarantine row on main plus a re-run of the failed jobs (same tag), or a
# revert on main plus a new cut.

# A green run only counts if it built the commit the tag points at (a tag
# that was moved, or a same-named branch, must not satisfy the gate).
if ! TAG_COMMIT_JSON="$(gh api "repos/${REPO}/commits/${CALVER}" 2>"$ERRF")"; then
  report_and_exit 1 "error: gh api cannot resolve tag ${CALVER}: $(errtext)"
fi
jq_get TAG_SHA "gh api commits/${CALVER}" "$TAG_COMMIT_JSON" -r '.sha // empty'
if [ -z "$TAG_SHA" ]; then
  report_and_exit 1 "error: gh api returned no commit sha for tag ${CALVER}"
fi

# ci.yml concludes `success` without building when its supersession check
# fires (its "HOW TO ASK 'DID MAIN ACTUALLY BUILD?'" note). That check answers
# false on a tag, so this should never trip; it is release-cut.sh's old rule,
# kept so a regression there cannot roll an untested tag. A run built only if
# some `heavy work starting` step succeeded, none was skipped, and the gate's
# `superseded -- run-wide skip` step did not succeed.
SENTINEL_STEP="heavy work starting"
SUPERSESSION_FIRED_STEP="superseded -- run-wide skip"
# Quoted heredocs: the jq programs' $sha, $s and $f must reach jq unexpanded.
GREEN_IDS_FILTER="$(cat <<'JQ'
.workflow_runs[]? | select(.conclusion == "success" and .head_sha == $sha) | .id // "?" | tostring
JQ
)"
NEWEST_FILTER="$(cat <<'JQ'
[.workflow_runs[]? | select(.head_sha == $sha)] | first
| if . == null then "none" else "\(.status // "?")/\(.conclusion // "-")" end
JQ
)"
BUILT_FILTER="$(cat <<'JQ'
[.jobs[]?.steps[]? | select(.name == $s) | .conclusion] as $c
| "\([$c[] | select(. == "success")] | length) \([$c[] | select(. == "skipped")] | length) \([.jobs[]?.steps[]? | select(.name == $f and .conclusion == "success")] | length)"
JQ
)"

# ci_run_built <run-id> -- 0 if that ci.yml run really built, else 1.
ci_run_built() {
  local id="$1" jobs counts built skipped fired
  if ! jobs="$(gh api "repos/${REPO}/actions/runs/${id}/jobs?filter=latest&per_page=100" 2>"$ERRF")"; then
    report_and_exit 1 "error: gh api ci.yml run ${id} jobs failed: $(errtext)"
  fi
  jq_get counts "gh api ci.yml run ${id} jobs" "$jobs" -r --arg s "$SENTINEL_STEP" --arg f "$SUPERSESSION_FIRED_STEP" "$BUILT_FILTER"
  read -r built skipped fired <<<"$counts"
  [ "${skipped:-1}" -eq 0 ] && [ "${fired:-1}" -eq 0 ] && [ "${built:-0}" -gt 0 ]
}

# --- Rule 10: a red tag files one escalation -------------------------------
#
# Owner 2026-11-03: a red tag must not go quiet. While the tag's runs are
# pending or in progress, rule 4 skips quietly as before. Once a required
# workflow has a COMPLETED push run at the tag's commit that did not succeed
# (RED_CONCLUSIONS) and no successful one, this files ONE escalation for that
# workflow, routed by the escalation policy (operation_type
# github_actions_rerun: the coordinator:laptop lane today). It runs before
# every rule 4 skip and scans all three workflows, so a red ci.yml is named
# even while images.yml is still building. Best effort, like rules 8 and 9: a
# failure to file is logged and never changes the exit code; nothing is filed
# under --dry-run or without KD_URL.
RED_CONCLUSIONS='["failure","timed_out","cancelled","startup_failure","action_required"]'
# Quoted heredocs: the jq programs' $sha, $red, $repo and $id must reach jq
# unexpanded.
RED_RUNS_FILTER="$(cat <<'JQ'
[.workflow_runs[]? | select(.head_sha == $sha)] as $r
| if any($r[]; .conclusion == "success") then []
  else [$r[] | select(.status == "completed" and ((.conclusion // "") as $c | any($red[]; . == $c)))
        | {id, conclusion, url: (.html_url // "https://github.com/\($repo)/actions/runs/\(.id)")}]
  end
JQ
)"
FAILED_JOBS_FILTER="$(cat <<'JQ'
[.jobs[]? | select(.conclusion != "success" and .conclusion != "skipped")
 | {run_id: $id, name, conclusion: (.conclusion // .status // "?"), url: (.html_url // "")}
 | with_entries(select(.value != ""))]
JQ
)"
# WF_RUNS -- each workflow's runs JSON as rule 4 fetched it, so the red scan
# asks gh again only for the workflows rule 4 never reached.
declare -A WF_RUNS=()

# red_tag_escalations -- rule 10's scan. Never exits.
red_tag_escalations() {
  [ -n "${KD_URL:-}" ] || return 0
  local wf runs red
  for wf in images.yml ci.yml checks.yml; do
    runs="${WF_RUNS[$wf]:-}"
    if [ -z "$runs" ] && ! runs="$(gh api "repos/${REPO}/actions/workflows/${wf}/runs?branch=${CALVER}&event=push" 2>"$ERRF")"; then
      log "WARN: red-tag check: gh api ${wf} runs failed: $(errtext)"
      continue
    fi
    if ! red="$(jq -c --arg sha "$TAG_SHA" --arg repo "$REPO" --argjson red "$RED_CONCLUSIONS" "$RED_RUNS_FILTER" <<<"$runs" 2>"$ERRF")"; then
      log "WARN: red-tag check: ${wf} runs are not readable: $(errtext)"
      continue
    fi
    if [ -n "$red" ] && [ "$red" != "[]" ]; then
      file_red_escalation "$wf" "$red"
    fi
  done
  return 0
}

# file_red_escalation <workflow> <red-runs-json> -- POSTs the escalation for
# one red workflow of the tag.
#
# idempotency_key release-red:<calver>:<workflow>: kbeads matches a key
# against every non-deleted bead, closed ones included, which is why per-agent
# or per-cause keys need a time bucket (github_app_credentials.go). Here that
# match is what we want: a calver is never cut twice, so one key is one red
# workflow of one tag, every tick after the first gets the same bead back, and
# a closed escalation stays closed (a re-run that goes red again is the same
# tag and the same ask; the next cut is a new key).
file_red_escalation() {
  local wf="$1" red="$2" key ids id jobs one failed="[]" runs_text jobs_text rerun title short_title ctx body resp bead
  key="release-red:${CALVER}:${wf}"
  if $DRY_RUN; then
    log "red tag: ${wf} failed at ${CALVER}; dry run, escalation ${key} not filed"
    return 0
  fi
  ids="$(jq -r '.[].id' <<<"$red" 2>/dev/null)" || ids=""
  for id in $ids; do
    if jobs="$(gh api "repos/${REPO}/actions/runs/${id}/jobs?filter=latest&per_page=100" 2>"$ERRF")" \
      && one="$(jq -c --argjson id "$id" "$FAILED_JOBS_FILTER" <<<"$jobs" 2>"$ERRF")"; then
      failed="$(jq -c --argjson a "$failed" --argjson b "$one" -n '$a + $b' 2>/dev/null)" || failed="[]"
    else
      log "WARN: red-tag check: cannot list the jobs of ${wf} run ${id}: $(errtext)"
    fi
  done
  runs_text="$(jq -r '[.[] | "\(.url) (\(.conclusion))"] | join(", ")' <<<"$red" 2>/dev/null)" || runs_text="?"
  jobs_text="$(jq -r 'if length == 0 then "(none listed)" else [.[] | "\(.name) (\(.conclusion))"] | join("; ") end' <<<"$failed" 2>/dev/null)" || jobs_text="?"
  rerun="$(jq -r --arg repo "$REPO" '[.[] | "gh run rerun \(.id) --failed -R \($repo)"] | join(" ; ")' <<<"$red" 2>/dev/null)" || rerun="gh run rerun <run-id> --failed"
  title="Release ${CALVER} red: ${wf} failed"
  # kbeads' short_title: 40 runes at most, no ids (model.ShortTitleMax).
  short_title="triage red ${wf} on the release tag"
  ctx="$(printf '%s\n' \
    "Release tag ${CALVER} (commit ${TAG_SHA}) is red: its ${wf} push run concluded without success and no run of it succeeded, so release-deploy.sh will not roll it (rule 4 skips every tick). Filed once by release-deploy.sh (rule 10, owner 2026-11-03)." \
    "Run(s): ${runs_text}" \
    "Failed jobs: ${jobs_text}" \
    "" \
    "Recovery (owner 2026-11-03):" \
    "- Narrow failure (one or a few tests): add a row to the shared quarantine list on main (each row its own file in .github/quarantine.d/; the failing job's \"quarantine rows\" step prints it, and scripts/quarantine.sh is how CI reads main's copy), then re-run ONLY the failed jobs of the tag's run: ${rerun}. The tagged code is unchanged; only main's quarantine list changes. No new cut." \
    "- Obvious large regression attributable to one PR (build/compile broken, failures across many packages, a whole job down rather than a test): revert that PR's merge on main and cut a new tag (gardenio/scripts/release-cut.sh)." \
    "Record the path chosen (quarantine or revert) and the culprit PR in this escalation's result.")"
  body="$(jq -nc \
    --arg title "$title" --arg short "$short_title" --arg key "$key" --arg ctx "$ctx" \
    --arg repo "$REPO" --arg tag "$CALVER" --arg sha "$TAG_SHA" --arg wf "$wf" \
    --argjson runs "$red" --argjson failed "$failed" \
    '{title: $title, short_title: $short, type: "escalation", kind: "data", priority: 1,
      labels: ["project:gardenio", "release-deploy"], idempotency_key: $key,
      fields: {
        operation_type: "github_actions_rerun",
        operation_spec: {repo: $repo, tag: $tag, sha: $sha, workflow: $wf, runs: $runs, failed_jobs: $failed,
                         commands: [$runs[] | "gh run rerun \(.id) --failed -R \($repo)"]},
        context: $ctx,
        expected_outputs: {path: "quarantine (rows on main + failed jobs re-run, same tag) or revert (culprit merge reverted on main + new cut)",
                           culprit_pr: "the PR whose change failed, or none"}}}' 2>/dev/null)" || {
    log "WARN: could not build the red-tag escalation ${key}; not filed"
    return 0
  }
  kd_auth_args
  if resp="$(curl -sf -X POST "${KD_URL%/}/v1/beads" \
    ${KD_AUTH_ARGS[@]+"${KD_AUTH_ARGS[@]}"} \
    -H "Content-Type: application/json" \
    -d "$body" 2>/dev/null)"; then
    bead="$(jq -r '.id // empty' <<<"$resp" 2>/dev/null)" || bead=""
    log "red tag: escalation ${bead:-(id not returned)} for ${wf} at ${CALVER} (idempotency_key ${key})"
  else
    log "WARN: failed to file the red-tag escalation ${key} to ${KD_URL}"
  fi
  return 0
}

# images.yml first: its skip keeps the outcome "images not ready", which the
# release record's deployment parser and the release-trains notes know.
for wf in images.yml ci.yml checks.yml; do
  case "$wf" in
    images.yml) not_green="images not ready" ;;
    *) not_green="${wf} not green" ;;
  esac
  if ! RUNS_JSON="$(gh api "repos/${REPO}/actions/workflows/${wf}/runs?branch=${CALVER}&event=push" 2>"$ERRF")"; then
    report_and_exit 1 "error: gh api ${wf} runs failed: $(errtext)"
  fi
  jq_get GREEN_IDS "gh api ${wf} runs" "$RUNS_JSON" -r --arg sha "$TAG_SHA" "$GREEN_IDS_FILTER"
  jq_get NEWEST "gh api ${wf} runs" "$RUNS_JSON" -r --arg sha "$TAG_SHA" "$NEWEST_FILTER"
  WF_RUNS[$wf]="$RUNS_JSON"
  if [ -z "$GREEN_IDS" ]; then
    red_tag_escalations || true
    report_and_exit 3 "${not_green}: no successful ${wf} push run for ${CALVER} at ${TAG_SHA} (newest there: ${NEWEST})"
  fi
  [ "$wf" = ci.yml ] || continue
  BUILT=false
  while IFS= read -r id; do
    [ -n "$id" ] || continue
    if ci_run_built "$id"; then
      BUILT=true
      break
    fi
    log "ci.yml run ${id} at ${TAG_SHA} concluded success but built nothing; not green"
  done <<<"$GREEN_IDS"
  if ! $BUILT; then
    red_tag_escalations || true
    report_and_exit 3 "${not_green}: ci.yml concluded success for ${CALVER} at ${TAG_SHA} but built nothing (superseded)"
  fi
done

# --- Rule 4b: theme ConfigMap ------------------------------------------

# The Keycloak login theme is a ConfigMap the chart references but does NOT
# own (gd-qiUiXvTX0k: inside the chart the theme tree was stored twice in the
# 1 MiB Helm release Secret and tipped 2026.292.1 over the limit). Render it
# from the checkout (gardenio/keycloak-theme) and apply it BEFORE the upgrade:
# the StatefulSet's theme-unpack initContainer needs it, and the chart reads
# its gardenio.dev/theme-checksum annotation (lookup) into checksum/theme, so
# Keycloak restarts on a theme edit and on nothing else. After every gate, so
# a skip never touches the cluster. Server-side apply: no last-applied
# annotation doubling a ~250 KB object. Under --dry-run the apply is a
# server-side dry run (validated, not persisted).
if ! THEME_MANIFEST="$("${SCRIPT_DIR}/keycloak-theme-configmap.py" --release "$RELEASE" --namespace "$NAMESPACE" 2>"$ERRF")"; then
  report_and_exit 1 "theme configmap render failed: $(errtext)"
fi
THEME_APPLY_ARGS=(apply --server-side --field-manager=release-deploy --force-conflicts -n "$NAMESPACE" -f -)
if $DRY_RUN; then
  THEME_APPLY_ARGS+=(--dry-run=server)
fi
if ! THEME_OUT="$(printf '%s\n' "$THEME_MANIFEST" | kubectl "${THEME_APPLY_ARGS[@]}" 2>&1)"; then
  log "$THEME_OUT"
  report_and_exit 1 "theme configmap apply failed"
fi
log "$THEME_OUT"

# --- Rule 4c: edge manifests ------------------------------------------

# The two nginx edges in front of the release (gd-WHBtmG7K3Z) are plain
# manifests outside the chart: gardenio-ziti/public-edge.yaml (ns
# public-edge) and viewer-authproxy.yaml (ns gardenio). They ride the release
# from the tag's checkout. Per edge: server-side apply its ConfigMaps; when
# the nginx ConfigMap changed, wait until every running pod mounts the new
# file, `nginx -t` in each (viewer-authproxy first re-renders its envsubst
# template into conf.d, the way its entrypoint does at start), and on a
# failed test re-apply the live ConfigMap as it was before (exit 1; exit 2
# when that rollback apply fails, as a restart would then load the broken
# config). On a pass, `nginx -s reload` in each pod, then apply the rest of
# the manifest, so a Deployment change never starts a pod on an untested
# config. Before the upgrade, so a broken edge stops the release; a later
# failed upgrade leaves the edge on the new tag. Edge upstreams that name a
# chart Service must use the resolver + variable form (public-edge.yaml
# does): a static name nginx cannot resolve fails `nginx -t` before rule 5
# creates the Service. The manifests front the release in namespace gardenio,
# so any other --namespace skips this rule.
#
# Fields: manifest|namespace|nginx ConfigMap|pod selector|ConfigMap mount|render
EDGES=(
  "public-edge|public-edge|public-edge|app.kubernetes.io/name=public-edge|/etc/nginx/conf.d|"
  "viewer-authproxy|gardenio|viewer-authproxy|app=viewer-authproxy|/etc/nginx/templates|envsubst"
)
EDGE_RELEASE_NAMESPACE="gardenio"
EDGE_DIR="${TOPLEVEL}/gardenio/helm/gardenio-ziti"
EDGE_SYNC_TIMEOUT="${EDGE_SYNC_TIMEOUT:-300}"
EDGE_SYNC_INTERVAL="${EDGE_SYNC_INTERVAL:-5}"
# viewer-authproxy: keep conf.d, render the templates, test; restore on demand.
EDGE_RENDER_TEST='rm -rf /tmp/release-deploy-conf.d && cp -a /etc/nginx/conf.d /tmp/release-deploy-conf.d && /docker-entrypoint.d/20-envsubst-on-templates.sh >/dev/null && nginx -t'
EDGE_RENDER_RESTORE='rm -f /etc/nginx/conf.d/* && cp -a /tmp/release-deploy-conf.d/. /etc/nginx/conf.d/'
EDGE_APPLY_ARGS=(apply --server-side --field-manager=release-deploy --force-conflicts -f -)
if $DRY_RUN; then
  EDGE_APPLY_ARGS+=(--dry-run=server)
fi

# manifest_docs <var> <file> -- every object in the multi-document YAML
# <file> as one JSON array, via yq or python3+PyYAML (fail closed, exit 1).
manifest_docs() {
  local __var="$1" __file="$2" __out
  if [ ! -f "$__file" ]; then
    report_and_exit 1 "edge manifest missing from the checkout: ${__file}"
  fi
  if command -v yq >/dev/null 2>&1; then
    __out="$(yq -o=json "$__file" 2>"$ERRF")" || report_and_exit 1 "edge manifest ${__file}: yq failed: $(errtext)"
  elif command -v python3 >/dev/null 2>&1 && python3 -c 'import yaml' >/dev/null 2>&1; then
    __out="$(python3 -c 'import sys, json, yaml
with open(sys.argv[1]) as f:
    json.dump([d for d in yaml.safe_load_all(f) if d], sys.stdout)' "$__file" 2>"$ERRF")" \
      || report_and_exit 1 "edge manifest ${__file}: python3 yaml failed: $(errtext)"
  else
    report_and_exit 1 "edge manifest ${__file}: neither yq nor python3+PyYAML to read it"
  fi
  if ! __out="$(jq -cs '[.[] | if type == "array" then .[] else . end | select(type == "object")]' <<<"$__out" 2>"$ERRF")"; then
    report_and_exit 1 "edge manifest ${__file}: not YAML objects: $(errtext)"
  fi
  printf -v "$__var" '%s' "$__out"
}

# edge_apply <json-array> -- server-side applies the objects as a List.
edge_apply() {
  local docs="$1" out
  if ! out="$(jq -c '{apiVersion: "v1", kind: "List", items: .}' <<<"$docs" | kubectl "${EDGE_APPLY_ARGS[@]}" 2>&1)"; then
    log "$out"
    return 1
  fi
  log "$out"
}

# edge_exec <ns> <pod> <cmd...> -- runs <cmd> in the pod's nginx container.
edge_exec() {
  local ns="$1" pod="$2"
  shift 2
  kubectl exec -n "$ns" "$pod" -c nginx -- "$@"
}

# edge_roll <spec> -- rule 4c for one EDGES entry. Exits on any failure.
edge_roll() {
  local name ns cm selector mount render
  IFS='|' read -r name ns cm selector mount render <<<"$1"
  local file="${EDGE_DIR}/${name}.yaml" docs cm_docs rest_docs new_cm live_json old_cm pods pod key
  manifest_docs docs "$file"
  cm_docs="$(jq -c '[.[] | select(.kind == "ConfigMap")]' <<<"$docs")"
  rest_docs="$(jq -c '[.[] | select(.kind != "ConfigMap")]' <<<"$docs")"
  new_cm="$(jq -c --arg n "$cm" --arg ns "$ns" \
    'first(.[] | select(.metadata.name == $n and .metadata.namespace == $ns)) // empty' <<<"$cm_docs")"
  [ -n "$new_cm" ] || report_and_exit 1 "edge ${name}: ${file} has no ConfigMap ${ns}/${cm}"

  if $DRY_RUN; then
    edge_apply "$docs" || report_and_exit 1 "edge ${name}: dry-run apply failed"
    return 0
  fi

  if ! live_json="$(kubectl get configmap "$cm" -n "$ns" -o json 2>"$ERRF")"; then
    report_and_exit 1 "edge ${name}: cannot read the live ConfigMap ${ns}/${cm}: $(errtext)"
  fi
  jq_get old_cm "live ConfigMap ${ns}/${cm}" "$live_json" -c \
    '{apiVersion: "v1", kind: "ConfigMap", metadata: {name: .metadata.name, namespace: .metadata.namespace}, data: .data, binaryData: .binaryData}
     | with_entries(select(.value != null))'

  edge_apply "$cm_docs" || report_and_exit 1 "edge ${name}: ConfigMap apply failed"

  if [ "$(jq -cS '[.data, .binaryData]' <<<"$old_cm")" = "$(jq -cS '[.data, .binaryData]' <<<"$new_cm")" ]; then
    log "edge ${name}: nginx ConfigMap unchanged; no test, no reload"
  else
    if ! pods="$(kubectl get pods -n "$ns" -l "$selector" \
        -o jsonpath='{range .items[?(@.status.phase=="Running")]}{.metadata.name}{"\n"}{end}' 2>"$ERRF")"; then
      edge_rollback "$name" "$ns" "$old_cm" "" "$render" "cannot list pods: $(errtext)"
    fi
    pods="$(grep -v '^$' <<<"$pods" || true)"
    [ -n "$pods" ] || edge_rollback "$name" "$ns" "$old_cm" "" "$render" "no running pod to test the config in"
    local tested="" deadline out want got
    for pod in $pods; do
      deadline=$((SECONDS + EDGE_SYNC_TIMEOUT))
      for key in $(jq -r '.data // {} | keys[]' <<<"$new_cm"); do
        want="$(jq -r --arg k "$key" '.data[$k]' <<<"$new_cm")"
        while got="$(edge_exec "$ns" "$pod" cat "${mount}/${key}" 2>/dev/null)"; [ "$got" != "$want" ]; do
          if [ "$SECONDS" -ge "$deadline" ]; then
            edge_rollback "$name" "$ns" "$old_cm" "$tested" "$render" \
              "${pod} did not mount the new ${key} within ${EDGE_SYNC_TIMEOUT}s"
          fi
          sleep "$EDGE_SYNC_INTERVAL"
        done
      done
      tested+="${pod} "
      if [ "$render" = "envsubst" ]; then
        out="$(edge_exec "$ns" "$pod" sh -c "$EDGE_RENDER_TEST" 2>&1)" || {
          log "$out"
          edge_rollback "$name" "$ns" "$old_cm" "$tested" "$render" "nginx -t failed in ${pod}"
        }
      else
        out="$(edge_exec "$ns" "$pod" nginx -t 2>&1)" || {
          log "$out"
          edge_rollback "$name" "$ns" "$old_cm" "$tested" "$render" "nginx -t failed in ${pod}"
        }
      fi
      log "edge ${name}: ${pod}: nginx -t ok"
    done
    for pod in $pods; do
      if ! out="$(edge_exec "$ns" "$pod" nginx -s reload 2>&1)"; then
        log "$out"
        report_and_exit 1 "edge ${name}: nginx reload failed in ${pod} (the tested config stays mounted)"
      fi
      log "edge ${name}: ${pod}: reloaded"
    done
  fi

  if [ "$(jq 'length' <<<"$rest_docs")" -gt 0 ]; then
    edge_apply "$rest_docs" || report_and_exit 1 "edge ${name}: manifest apply failed (ConfigMaps applied and tested)"
  fi
}

# edge_rollback <name> <ns> <old-cm-json> <tested-pods> <render> <reason> --
# puts the nginx ConfigMap back as it was and, for a rendered edge, each
# tested pod's conf.d; then exits 1 (2 if the ConfigMap rollback failed).
edge_rollback() {
  local name="$1" ns="$2" old_cm="$3" tested="$4" render="$5" reason="$6" pod
  log "edge ${name}: ${reason}; rolling the ConfigMap back"
  if [ "$render" = "envsubst" ]; then
    for pod in $tested; do
      edge_exec "$ns" "$pod" sh -c "$EDGE_RENDER_RESTORE" >/dev/null 2>&1 \
        || log "WARN: edge ${name}: could not restore conf.d in ${pod}"
    done
  fi
  if ! edge_apply "[${old_cm}]"; then
    report_and_exit 2 "edge ${name}: ${reason}, and the ConfigMap rollback failed -- manual intervention needed (a pod restart loads the untested config)"
  fi
  report_and_exit 1 "edge ${name}: ${reason}; ConfigMap rolled back"
}

if [ "$NAMESPACE" = "$EDGE_RELEASE_NAMESPACE" ]; then
  for edge in "${EDGES[@]}"; do
    edge_roll "$edge"
  done
else
  log "edge manifests: they front namespace ${EDGE_RELEASE_NAMESPACE}, not ${NAMESPACE}; skipped"
fi

# --- Rule 5: upgrade ---------------------------------------------------

# Upgrade from the packed chart, never the source dir. Helm stores the whole
# chart in the release Secret (1 MiB limit): 2026.266.2's raw chart made a
# ~1,071,500 B Secret and the upgrade failed before applying anything; packed,
# it is ~826 KB. The pack strips template comments and fails if the render
# changes (ka-0000007585).
if ! PACK_OUT="$(bash "${TOPLEVEL}/scripts/pack-gardenio-chart.sh" "$CHART_DIR" "$PACK_DIR" \
    --version "$CALVER" --app-version "$CALVER" 2>"$ERRF")"; then
  report_and_exit 1 "chart pack failed: $(errtext)"
fi
CHART_TGZ="$(tail -n 1 <<<"$PACK_OUT")"
if [ ! -f "$CHART_TGZ" ]; then
  report_and_exit 1 "chart pack produced no file (got '${CHART_TGZ}')"
fi

HELM_UPGRADE_BASE_ARGS=(upgrade "$RELEASE" "$CHART_TGZ" -n "$NAMESPACE" -f "$VALUES" --set "global.imageTag=${CALVER}")

if $DRY_RUN; then
  # --hide-secret: the dry run prints every rendered manifest, and this
  # output lands in pod logs and (last 40 lines) in the report bead. helm
  # >= 3.13 and v4 support it with --dry-run; an older helm rejects the flag,
  # which fails closed (exit 1) rather than leaking Secrets.
  if ! UPGRADE_OUT="$(helm "${HELM_UPGRADE_BASE_ARGS[@]}" --dry-run=server --hide-secret 2>&1)"; then
    log "$UPGRADE_OUT"
    REV_AFTER="$(get_helm_revision)"
    report_and_exit 1 "dry-run helm upgrade failed"
  fi
  log "$UPGRADE_OUT"
  REV_AFTER="$(get_helm_revision)"
  report_and_exit 0 "dry-run ok"
fi

if ! UPGRADE_OUT="$(helm "${HELM_UPGRADE_BASE_ARGS[@]}" --atomic --wait --timeout 20m 2>&1)"; then
  log "$UPGRADE_OUT"
  # --atomic should have rolled the release back to its previous revision.
  # Do not assume it did: only a release back on `deployed` is exit 1;
  # anything else (pending-*, failed, status unreadable) is exit 2.
  POST_STATUS=""
  if POST_JSON="$(helm status "$RELEASE" -n "$NAMESPACE" -o json 2>"$ERRF")"; then
    POST_STATUS="$(jq -r '.info.status // empty' <<<"$POST_JSON" 2>/dev/null || true)"
    REV_AFTER="$(jq -r '.version // "unknown"' <<<"$POST_JSON" 2>/dev/null || echo unknown)"
  else
    log "helm status after failed upgrade failed: $(errtext)"
    REV_AFTER="unknown"
  fi
  if [ "$POST_STATUS" = "deployed" ]; then
    # --atomic rolled back to a new revision of the tag that ran before.
    report_deployment "$CURRENT_TAG" "$REV_AFTER" rollback
    report_and_exit 1 "helm upgrade failed (atomic rollback applied)"
  fi
  report_and_exit 2 "helm upgrade failed and the atomic rollback did not restore a deployed release (status: ${POST_STATUS:-unknown}) -- manual intervention needed"
fi
log "$UPGRADE_OUT"

# --- Rule 6: verify, roll back on failure -----------------------------

if ! VERIFY_OUT="$(bash "${SCRIPT_DIR}/verify-image-tags.sh" "${VERIFY_ARGS[@]}" 2>&1)"; then
  log "$VERIFY_OUT"
  if ! ROLLBACK_OUT="$(helm rollback "$RELEASE" 0 -n "$NAMESPACE" --wait --timeout 20m 2>&1)"; then
    log "$ROLLBACK_OUT"
    REV_AFTER="$(get_helm_revision)"
    report_and_exit 2 "verify failed and rollback failed -- manual intervention needed"
  fi
  log "$ROLLBACK_OUT"
  REV_AFTER="$(get_helm_revision)"
  report_deployment "$CURRENT_TAG" "$REV_AFTER" rollback
  report_and_exit 1 "verify failed, rolled back"
fi
log "$VERIFY_OUT"

# --- Rule 9: record the deployment ------------------------------------

REV_AFTER="$(get_helm_revision)"
report_deployment "$CALVER" "$REV_AFTER"
report_and_exit 0 "deployed and verified"
