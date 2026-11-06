#!/usr/bin/env bash
# tarboat-file-regressions.sh — Test-in-box E2 (ka-0000001077), epic ka-0000003645.
# Operator: Matt Baker (oidc:matthew.baker@acme.example).
#
# Consume a `tarboat-validate/v1` results document (emitted by
# scripts/inbox/tarboat-validate.sh --results-json, E1 ka-0000001107) and, for
# every gate with status=="fail", auto-file a typed regression *bug* parented to
# the test-in-box regression epic, capturing the gate key, the box image/calver,
# and the failing assertion message.
#
# This is the E2 step in the standing test-in-box regression suite: the gardener
# / CI cron runs tarboat-validate.sh against a fresh box, then pipes the results
# JSON here so each regressed gate becomes a tracked bug under the suite epic.
#
# De-dupe: an open (open|in_progress) bug for the SAME gate+calver is never
# duplicated — re-running the suite on the same box just no-ops on already-filed
# regressions.
#
# Unlike tarboat-validate.sh (which runs ON the air-gapped box and is therefore
# jq/python-free), this filer runs on the orchestration side — the gardener/CI
# host where `kd` already lives — so it leans on `jq` for robust JSON parsing.
# Its only dependencies are `kd` (to file/query beads) and `jq`.
#
# Usage:
#   tarboat-file-regressions.sh RESULTS_JSON [flags]
#   tarboat-validate.sh ... --results-json - | tarboat-file-regressions.sh -
#
#   RESULTS_JSON       path to a tarboat-validate/v1 doc, or "-" for stdin.
#   --epic ID          parent epic for filed bugs (default: ka-0000003645).
#   --label L          dedupe/marker label on filed bugs (default:
#                      tarboat-regression; repeatable additional labels via -l).
#   -l L               extra label to add to filed bugs (repeatable).
#   --priority N       bug priority (default: 1).
#   --operated-by ID   operator id stamped on filed bugs (default: from the
#                      results doc's operated_by, else env BOAT_OPERATED_BY).
#   --dry-run          print what WOULD be filed; do not call `kd create`.
#   -h, --help         this help.
#
# Exit status: 0 when all failing gates are handled (filed or already-present),
# even when bugs were created — filing a regression is the success path here.
# Non-zero only on usage / parse / kd errors.

set -euo pipefail

# ── args ─────────────────────────────────────────────────────────────────────
RESULTS_JSON=""
EPIC="ka-0000003645"
DEDUPE_LABEL="tarboat-regression"
EXTRA_LABELS=()
PRIORITY="1"
OPERATED_BY_OVERRIDE=""
DRY_RUN=0

usage() { sed -n '2,/^set -euo/p' "$0" | sed '$d; s/^# \{0,1\}//'; }

while [ "$#" -gt 0 ]; do
  case "$1" in
    --epic)        EPIC="$2"; shift 2;;
    --label)       DEDUPE_LABEL="$2"; shift 2;;
    -l)            EXTRA_LABELS+=("$2"); shift 2;;
    --priority)    PRIORITY="$2"; shift 2;;
    --operated-by) OPERATED_BY_OVERRIDE="$2"; shift 2;;
    --dry-run)     DRY_RUN=1; shift;;
    -h|--help)     usage; exit 0;;
    --)            shift; break;;
    -)             [ -z "$RESULTS_JSON" ] || { echo "extra arg: $1" >&2; exit 2; }
                   RESULTS_JSON="-"; shift;;
    -*)            echo "unknown flag: $1" >&2; usage >&2; exit 2;;
    *)             [ -z "$RESULTS_JSON" ] || { echo "extra arg: $1" >&2; exit 2; }
                   RESULTS_JSON="$1"; shift;;
  esac
done

[ -n "$RESULTS_JSON" ] || { echo "error: RESULTS_JSON path (or -) required" >&2; usage >&2; exit 2; }

# ── deps ─────────────────────────────────────────────────────────────────────
command -v jq >/dev/null 2>&1 || { echo "error: jq is required (filer runs on the orchestration host)" >&2; exit 2; }
KD="${KD_BIN:-kd}"
if [ "$DRY_RUN" = 0 ]; then
  command -v "$KD" >/dev/null 2>&1 || { echo "error: kd not found (set KD_BIN or use --dry-run)" >&2; exit 2; }
fi

# ── read + validate the results doc ──────────────────────────────────────────
if [ "$RESULTS_JSON" = "-" ]; then
  DOC="$(cat)"
else
  [ -s "$RESULTS_JSON" ] || { echo "error: results file not found or empty: $RESULTS_JSON" >&2; exit 2; }
  DOC="$(cat "$RESULTS_JSON")"
fi

echo "$DOC" | jq -e . >/dev/null 2>&1 || { echo "error: results is not valid JSON" >&2; exit 2; }

SCHEMA="$(echo "$DOC" | jq -r '.schema // ""')"
case "$SCHEMA" in
  tarboat-validate/v1) ;;
  "") echo "error: results doc has no .schema (expected tarboat-validate/v1)" >&2; exit 2;;
  *)  echo "error: unsupported schema '$SCHEMA' (expected tarboat-validate/v1)" >&2; exit 2;;
esac

CALVER="$(echo "$DOC" | jq -r '.calver // "" | if . == "" then "unknown" else . end')"
TARBOAT_IP="$(echo "$DOC" | jq -r '.ip // "unknown"')"
RELEASE="$(echo "$DOC" | jq -r '.release // "unknown"')"
NAMESPACE="$(echo "$DOC" | jq -r '.namespace // "unknown"')"
FINISHED_AT="$(echo "$DOC" | jq -r '.finished_at // "unknown"')"
RELEASE_GRADE="$(echo "$DOC" | jq -r '.release_grade // false')"
OPERATED_BY="${OPERATED_BY_OVERRIDE:-$(echo "$DOC" | jq -r '.operated_by // ""')}"
[ -n "$OPERATED_BY" ] || OPERATED_BY="${BOAT_OPERATED_BY:-}"

# ── enumerate already-open regression bugs for cheap de-dupe ─────────────────
# One query, filtered locally by the typed (gate,calver) marker fields we stamp.
EXISTING="[]"
if [ "$DRY_RUN" = 0 ]; then
  EXISTING="$("$KD" list --type=bug --status open --status in_progress \
                  --label "$DEDUPE_LABEL" --limit 1000 --json 2>/dev/null || echo '[]')"
  echo "$EXISTING" | jq -e . >/dev/null 2>&1 || EXISTING="[]"
fi

# already_open <gate-key> <calver> → 0 if an open bug already covers it.
# Dedup keys on the tarboat-gate:<key> + tarboat-calver:<calver> LABELS (the
# beads server rejects arbitrary typed -f fields — only a fixed allowlist is
# accepted — so the (gate,calver) identity is carried as labels, which are
# freeform and index cleanly).
already_open() {
  local key="$1" calver="$2"
  echo "$EXISTING" | jq -e --arg g "tarboat-gate:$key" --arg c "tarboat-calver:$calver" \
    'any(.[]?; (.labels | index($g)) and (.labels | index($c)))' \
    >/dev/null 2>&1
}

# ── walk the failing gates ───────────────────────────────────────────────────
FAIL_COUNT="$(echo "$DOC" | jq '[.gates[]? | select(.status == "fail")] | length')"
if [ "$FAIL_COUNT" -eq 0 ]; then
  echo "tarboat-file-regressions: no failing gates in results (release_grade=$RELEASE_GRADE, calver=$CALVER) — nothing to file."
  exit 0
fi

echo "tarboat-file-regressions: $FAIL_COUNT failing gate(s) on calver=$CALVER (release_grade=$RELEASE_GRADE); epic=$EPIC"

FILED=0
SKIPPED=0
# Iterate failing gates by index so assertion messages with spaces/newlines
# survive intact (no fragile field splitting on the shell side).
FAIL_RECS="$(echo "$DOC" | jq -c '[.gates[]? | select(.status == "fail")]')"
i=0
while [ "$i" -lt "$FAIL_COUNT" ]; do
  rec="$(echo "$FAIL_RECS" | jq -c ".[$i]")"
  i=$((i + 1))
  key="$(echo "$rec"  | jq -r '.key // "unknown" | if . == "" then "unknown" else . end')"
  name="$(echo "$rec" | jq -r '.name // ""')"
  msg="$(echo "$rec"  | jq -r '.message // "" | if . == "" then "(no assertion message captured)" else . end')"
  dur="$(echo "$rec"  | jq -r '.duration_seconds // 0')"

  if already_open "$key" "$CALVER"; then
    echo "  • gate '$key' @ $CALVER — open regression bug already exists; skipping."
    SKIPPED=$((SKIPPED + 1))
    continue
  fi

  title="tarboat regression: gate '$key' FAILED on $CALVER"
  desc="$(cat <<EOF
Auto-filed by tarboat-file-regressions.sh (Test-in-box E2, ka-0000001077) from a
tarboat-validate/v1 results doc — a standing regression of the **boxed** gardenio
artifact.

- **Gate:** \`$key\`${name:+ — $name}
- **Status:** fail (duration ${dur}s)
- **Box calver:** $CALVER
- **Release:** $RELEASE
- **Namespace:** $NAMESPACE
- **Box IP:** $TARBOAT_IP
- **Validated at:** $FINISHED_AT

**Failing assertion:**
\`\`\`
$msg
\`\`\`

Re-run \`scripts/inbox/tarboat-validate.sh\` against a fresh box for this calver
to reproduce. Close when the gate is green again on a re-validated box.
EOF
)"

  # The (gate,calver) identity is carried as LABELS, not typed -f fields: the
  # beads server enforces a fixed typed-field allowlist and rejects unknown ones
  # (tarboat_gate/tarboat_calver 400 out). Labels are freeform, dedupe on them,
  # and render in the viewer. The operator is stamped via --operated-by (a
  # first-class flag), NOT -f operated_by= (also not in the allowlist).
  create_args=( create "$title"
    --type=bug
    --parent-task "$EPIC"
    -d "$desc"
    --priority "$PRIORITY"
    -l "$DEDUPE_LABEL"
    -l "tarboat-gate:$key"
    -l "tarboat-calver:$CALVER" )
  for L in "${EXTRA_LABELS[@]:-}"; do [ -n "$L" ] && create_args+=( -l "$L" ); done
  [ -n "$OPERATED_BY" ] && create_args+=( --operated-by "$OPERATED_BY" )

  if [ "$DRY_RUN" = 1 ]; then
    echo "  • [dry-run] would file bug for gate '$key' @ $CALVER:"
    echo "      kd ${create_args[*]}"
    FILED=$((FILED + 1))
    continue
  fi

  if out="$("$KD" "${create_args[@]}" 2>&1)"; then
    new_id="$(echo "$out" | grep -oE '(gd|ka)-[A-Za-z0-9]+' | head -1)"
    echo "  • filed regression bug ${new_id:-?} for gate '$key' @ $CALVER (parent $EPIC)"
    FILED=$((FILED + 1))
  else
    echo "  ! failed to file bug for gate '$key' @ $CALVER:" >&2
    echo "$out" >&2
    exit 1
  fi
done

echo "tarboat-file-regressions: done — filed $FILED, skipped (already open) $SKIPPED."
exit 0
