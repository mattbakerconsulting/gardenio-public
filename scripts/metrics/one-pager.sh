#!/usr/bin/env bash
# one-pager.sh — the headline fleet numbers for the launch one-pager
# (gd-UFdSDSsdvt; pasted into docs/launch/one-pager.md by gd-gCMMBGUGAJ).
#
# Prints a markdown table for a date window (default: the last 30 days):
#
#   merged PRs, PRs merged per day   gh pr list --state merged (mergedAt in window)
#   token cost, cost per merged PR   token counts on session beads x list price
#   decisions per week               decision beads created in window
#   median decision latency          responded_at - created_at
#   captain no-op rate               captain runs whose wrap-up progress_score is 0
#                                    (plan-gardener, which took over from
#                                    plan-captain on 2026-10-24, plan-captain,
#                                    release-captain)
#
# COST IS TOKENS x LIST PRICE, NEVER LITELLM SPEND DOLLARS. Every request goes
# through the LiteLLM proxy; gb sums the usage it returns (input, output, cache
# write, cache read) onto the session bead at session close. This script prices
# those counts at Anthropic's published per-MTok rates for the model the
# session's role runs. LiteLLM's own spend figures ("API-equivalent $", gb
# usage, live_cost_usd, settled_cost_usd) are synthetic for subscription
# accounts and are deliberately never read here.
#
# Caveats the output also states:
#   - Session beads do not record their model. A session is priced at the model
#     its role bead names TODAY; a role with no model is priced as Opus 5.5.
#   - Cache writes carry no TTL split. Claude Code on the fleet uses the 1-hour
#     prompt-cache TTL (2x input); --cache-write-ttl 5m prices them at 1.25x.
#   - MOST SESSIONS CARRY NO TOKEN COUNTS (gd-88NFBy95l7). gb writes usage only
#     when it closes its own session; the many sessions the controller's
#     session reaper closes as "retired" (pod gone before gb's close landed)
#     have none, and neither do live ones. So the script prints two costs:
#     RECORDED (a floor: only sessions with counts) and EXTRAPOLATED (recorded,
#     plus each role's mean recorded cost per session for every ended session
#     of that role without counts). Coverage is printed beside both.
#   - Decision beads carry no project label, so decisions are fleet-wide.
#
# Read-only. Needs kd, gh, jq and the fleet token; nothing else.
#
# Usage:
#   scripts/metrics/one-pager.sh [--days N | --since T [--until T]]
#                                [--repo OWNER/REPO] [--project NAME]
#                                [--cache-write-ttl 1h|5m]
#                                [--save-dir DIR] [--from-dir DIR]
#
#   --save-dir DIR   also write the fetched JSON to DIR (to re-run offline)
#   --from-dir DIR   skip fetching; read prs.json, sessions.json, roles.json,
#                    decisions.json, captains.json from DIR (the self-test)
#
# Self-test: bash scripts/metrics/one-pager_test.sh
set -euo pipefail

DAYS=30
SINCE=""
UNTIL=""
REPO=""
PROJECT="gardenio"
CACHE_TTL="1h"
SAVE_DIR=""
FROM_DIR=""

die() { echo "one-pager: $*" >&2; exit 2; }

usage() { sed -n '/^# Usage:/,/^# Self-test:/p' "$0" | sed 's/^# \{0,1\}//'; }

while [ $# -gt 0 ]; do
  case "$1" in
    --days) DAYS="${2:?--days needs a value}"; shift 2 ;;
    --since) SINCE="${2:?--since needs a value}"; shift 2 ;;
    --until) UNTIL="${2:?--until needs a value}"; shift 2 ;;
    --repo) REPO="${2:?--repo needs a value}"; shift 2 ;;
    --project) PROJECT="${2:?--project needs a value}"; shift 2 ;;
    --cache-write-ttl) CACHE_TTL="${2:?--cache-write-ttl needs a value}"; shift 2 ;;
    --save-dir) SAVE_DIR="${2:?--save-dir needs a value}"; shift 2 ;;
    --from-dir) FROM_DIR="${2:?--from-dir needs a value}"; shift 2 ;;
    -h|--help) usage; exit 0 ;;
    *) die "unknown argument: $1 (see --help)" ;;
  esac
done

command -v jq >/dev/null 2>&1 || die "jq is required"
case "$DAYS" in ''|*[!0-9]*|0) die "--days must be a positive integer, got '$DAYS'" ;; esac
case "$CACHE_TTL" in 1h|5m) ;; *) die "--cache-write-ttl must be 1h or 5m, got '$CACHE_TTL'" ;; esac

# to_epoch <iso8601> — accepts fractional seconds, which fromdateiso8601 does not.
to_epoch() {
  jq -nr --arg t "$1" '$t | sub("\\.[0-9]+"; "") | fromdateiso8601' 2>/dev/null \
    || die "not an RFC3339 UTC time (want 2026-10-01T00:00:00Z): '$1'"
}
to_iso() { jq -nr --argjson e "$1" '$e | todate'; }

if [ -n "$UNTIL" ]; then UNTIL_E="$(to_epoch "$UNTIL")"; else UNTIL_E="$(jq -n 'now | floor')"; fi
if [ -n "$SINCE" ]; then SINCE_E="$(to_epoch "$SINCE")"; else SINCE_E=$((UNTIL_E - DAYS * 86400)); fi
[ "$SINCE_E" -lt "$UNTIL_E" ] || die "--since must be before --until"
SINCE="$(to_iso "$SINCE_E")"
UNTIL="$(to_iso "$UNTIL_E")"

REPO_ROOT="$(git -C "$(dirname "$0")" rev-parse --show-toplevel 2>/dev/null || true)"
COMMIT="$(git -C "${REPO_ROOT:-.}" rev-parse --short=12 HEAD 2>/dev/null || echo unknown)"

# ── fetch ────────────────────────────────────────────────────────────────────

# kd_pages <slim-jq-filter> <kd list args...> — every page of a kd list, each
# bead projected through the filter, as one JSON array. kd caps a page at
# --limit and says has_more, so loop rather than trust one call.
kd_pages() {
  local slim="$1" offset=0 page
  shift
  local pages; pages="$(mktemp)"
  while :; do
    page="$(kd list "$@" --limit 2000 --offset "$offset" --json-envelope)" \
      || die "kd list $* failed"
    jq -c "[.beads[] | $slim]" <<<"$page" >>"$pages"
    [ "$(jq -r '.has_more' <<<"$page")" = "true" ] || break
    offset=$((offset + $(jq -r '.count' <<<"$page")))
  done
  jq -cs 'add // []' "$pages"
  rm -f "$pages"
}

# merged_prs — merged PRs in the window. GitHub search returns at most 1000
# results per query, so ask one day at a time and refuse a day that hits the
# cap rather than silently undercount. The day ranges are inclusive at both
# ends, so dedupe by number and keep [since, until).
merged_prs() {
  local from="$SINCE_E" to day
  local days; days="$(mktemp)"
  while [ "$from" -lt "$UNTIL_E" ]; do
    to=$((from + 86400)); [ "$to" -gt "$UNTIL_E" ] && to="$UNTIL_E"
    day="$(gh pr list --repo "$REPO" --state merged --limit 1000 \
      --search "merged:$(to_iso "$from")..$(to_iso "$to")" \
      --json number,mergedAt)" || die "gh pr list failed for $(to_iso "$from")"
    [ "$(jq 'length' <<<"$day")" -lt 1000 ] \
      || die "gh search hit its 1000-result cap for the day from $(to_iso "$from")"
    printf '%s\n' "$day" >>"$days"
    from="$to"
  done
  jq -cs --argjson s "$SINCE_E" --argjson u "$UNTIL_E" \
    'add // [] | unique_by(.number) | map(select((.mergedAt | fromdateiso8601) as $m | $m >= $s and $m < $u))' "$days"
  rm -f "$days"
}

fetch() {
  local dir="$1" r
  command -v kd >/dev/null 2>&1 || die "kd is required"
  command -v gh >/dev/null 2>&1 || die "gh is required"
  [ -n "$REPO" ] || REPO="$(gh repo view --json nameWithOwner -q .nameWithOwner)" \
    || die "cannot work out the repo; pass --repo OWNER/REPO"

  merged_prs >"$dir/prs.json"

  kd_pages '{id, role: .fields.role, status: .fields.status,
             input_tokens: .fields.input_tokens, output_tokens: .fields.output_tokens,
             cache_write_tokens: .fields.cache_write_tokens,
             cache_read_tokens: .fields.cache_read_tokens}' \
    -t session --all-types --all-projects -l "project:$PROJECT" \
    --created-after "$SINCE" --created-before "$UNTIL" >"$dir/sessions.json"

  kd_pages '{role: .title, model: (.fields.model // "")}' \
    -t role --all-types --all-projects >"$dir/roles.json"

  kd_pages '{id, created_at, created_by, responded_at: .fields.responded_at}' \
    -t decision --all-types --all-projects \
    --created-after "$SINCE" --created-before "$UNTIL" >"$dir/decisions.json"

  : >"$dir/captains.ndjson"
  for r in plan-gardener plan-captain release-captain; do
    kd_pages '{id, role: .fields.role, created_at,
               progress_score: (.fields.wrapup // "" | fromjson? // {} | .progress_score)}' \
      -t agent --all-types --all-projects -l "project:$PROJECT" -f "role=$r" \
      --created-after "$SINCE" --created-before "$UNTIL" >>"$dir/captains.ndjson"
  done
  jq -cs 'add' "$dir/captains.ndjson" >"$dir/captains.json"
  rm -f "$dir/captains.ndjson"
}

if [ -n "$FROM_DIR" ]; then
  DATA="$FROM_DIR"
  [ -n "$REPO" ] || REPO="(from $FROM_DIR)"
else
  DATA="$(mktemp -d)"
  trap 'rm -rf "$DATA"' EXIT
  fetch "$DATA"
  if [ -n "$SAVE_DIR" ]; then mkdir -p "$SAVE_DIR" && cp "$DATA"/*.json "$SAVE_DIR"/; fi
fi
for f in prs sessions roles decisions captains; do
  [ -r "$DATA/$f.json" ] || die "missing $DATA/$f.json"
done

# ── compute + print ──────────────────────────────────────────────────────────

jq -nr \
  --slurpfile prs "$DATA/prs.json" \
  --slurpfile sessions "$DATA/sessions.json" \
  --slurpfile roles "$DATA/roles.json" \
  --slurpfile decisions "$DATA/decisions.json" \
  --slurpfile captains "$DATA/captains.json" \
  --arg since "$SINCE" --arg until "$UNTIL" \
  --argjson since_e "$SINCE_E" --argjson until_e "$UNTIL_E" \
  --arg repo "$REPO" --arg project "$PROJECT" --arg commit "$COMMIT" \
  --arg ttl "$CACHE_TTL" '
# Anthropic list prices, USD per million tokens (claude-api skill model table,
# cached 2026-09-25). Cache writes are input x 2 (1-hour TTL) or x 1.25 (5-min).
def prices: {
  "fable-5-1":  {name: "Fable 5.1",  in: 10, out: 50, read: 0.25},
  "opus-5-5":   {name: "Opus 5.5",   in: 4,  out: 20, read: 0.20},
  "sonnet-5-5": {name: "Sonnet 5.5", in: 2,  out: 10, read: 0.20},
  "haiku-4-5":  {name: "Haiku 4.5",  in: 1,  out: 5,  read: 0.10}
};
def write_mult: if $ttl == "1h" then 2 else 1.25 end;
def model_key($m): (prices | keys | map(select(. as $k | $m | contains($k))) | first) // "opus-5-5";
def r2: . * 100 | round / 100;
def r1: . * 10 | round / 10;
def money: r2 | tostring | if test("\\.") then (if test("\\.[0-9]$") then . + "0" else . end) else . + ".00" end | "$" + .;
def pct: . * 100 | r1 | tostring + "%";
def ts: sub("\\.[0-9]+"; "") | fromdateiso8601;
def n: . // 0 | tonumber;
def median: sort | length as $l | if $l == 0 then null
  elif $l % 2 == 1 then .[($l - 1) / 2] else (.[$l / 2 - 1] + .[$l / 2]) / 2 end;
def dur: if . == null then "n/a" elif . < 3600 then (. / 60 | r1 | tostring) + " min"
  else (. / 3600 | r1 | tostring) + " h" end;
def e2e: . == "escalation-analyze" or . == "e2e-agent-v2" or startswith("e2e-v");

(($until_e - $since_e) / 86400) as $days
| ($prs[0] | length) as $merged
| ($roles[0] | map({(.role): .model}) | add // {}) as $role_model

# sessions -> priced rows
| [$sessions[0][] | select(.input_tokens != null or .output_tokens != null)
    | (model_key($role_model[.role // ""] // "")) as $k
    | prices[$k] as $p
    | {role: (.role // ""), k: $k, in: (.input_tokens | n), out: (.output_tokens | n),
       cw: (.cache_write_tokens | n), cr: (.cache_read_tokens | n)}
    | .cost = ((.in * $p.in + .out * $p.out + .cw * $p.in * write_mult + .cr * $p.read) / 1e6)
  ] as $costed
| [$sessions[0][] | select(.input_tokens == null and .output_tokens == null)] as $nocount
| ($nocount | map(select(.status == "active")) | length) as $live
| ($nocount | map(select(.status != "active"))) as $uncosted
| (($costed | length) + ($uncosted | length)) as $ended
| ($costed | map(.cost) | add // 0) as $cost
| ($costed | group_by(.role) | map({(.[0].role): (map(.cost) | add / length)}) | add // {}) as $role_mean
| ($uncosted | map($role_mean[.role // ""] // null)) as $fill
| ($fill | map(select(. == null)) | length) as $unfillable
| ($cost + ($fill | map(. // 0) | add // 0)) as $cost_x
| (if $ended > 0 then ($costed | length) / $ended else 0 end) as $coverage
| ($costed | group_by(.k) | map({k: .[0].k, sessions: length,
     in: (map(.in) | add), out: (map(.out) | add), cw: (map(.cw) | add),
     cr: (map(.cr) | add), cost: (map(.cost) | add)})) as $by_model

# decisions
| [$decisions[0][] | select(.created_by // "" | e2e | not)] as $dec
| ([$decisions[0][] | select(.created_by // "" | e2e)] | length) as $dec_e2e
| [$dec[] | select(.responded_at != null and .responded_at != "")
    | (.responded_at | ts) - (.created_at | ts) | select(. >= 0)] as $lat

# captains
| [$captains[0][] | select(.progress_score != null)] as $scored
| ([$captains[0][] | select(.progress_score == null)] | length) as $unscored
| ($scored | map(select(.progress_score | tonumber == 0)) | length) as $noop
| ($scored | group_by(.role) | map("\(.[0].role) \(map(select(.progress_score | tonumber == 0)) | length)/\(length)") | join(", ")) as $noop_by_role

| "## Fleet one-pager metrics",
  "",
  "- Window: \($since) .. \($until) (\($days | r1) days)",
  "- Commit: \($commit)",
  "- Repo: \($repo); project: \($project)",
  "",
  "| Metric | Value | Source |",
  "|---|---|---|",
  "| Merged PRs | \($merged) | gh pr list --state merged on \($repo), mergedAt in window |",
  "| PRs merged per day | \(if $days > 0 then $merged / $days | r1 else "n/a" end) | merged PRs / \($days | r1) days |",
  "| Token cost at list price, recorded | \($cost | money) | token counts on the \($costed | length) of \($ended) ended session beads (\($coverage | pct); project:\($project), created in window) that carry them, x Anthropic list price; not LiteLLM spend |",
  "| Token cost at list price, extrapolated | \($cost_x | money) | recorded, plus each role mean recorded cost per session x its \($uncosted | length) ended sessions with no token counts |",
  "| Cost per merged PR | \(if $merged > 0 then "\($cost / $merged | money) recorded, \($cost_x / $merged | money) extrapolated" else "n/a" end) | token cost / merged PRs |",
  "| Decisions per week | \(if $days > 0 then ($dec | length) / ($days / 7) | r1 else "n/a" end) | \($dec | length) decision beads created in window, fleet-wide, e2e fixtures excluded / \($days / 7 | r2) weeks |",
  "| Median decision latency | \($lat | median | dur) (n=\($lat | length)) | responded_at - created_at on responded decision beads |",
  "| Captain no-op rate | \(if ($scored | length) > 0 then $noop / ($scored | length) | pct else "n/a" end) (\($noop)/\($scored | length)) | captain runs (plan-gardener, plan-captain, release-captain; project:\($project)) created in window whose wrap-up progress_score is 0 (\($noop_by_role)) |",
  "",
  "Recorded token cost by model (priced at the model each session role runs today; a role with no model is priced as Opus 5.5):",
  "",
  "| Model | Sessions | Input | Output | Cache write | Cache read | Cost |",
  "|---|---|---|---|---|---|---|",
  ($by_model[] | "| \(prices[.k].name) | \(.sessions) | \(.in) | \(.out) | \(.cw) | \(.cr) | \(.cost | money) |"),
  "",
  "Notes:",
  "- List prices per MTok (in/out/cache read): " + ([prices[] | "\(.name) $\(.in)/$\(.out)/$\(.read)"] | join(", ")) + "; cache writes at \(if $ttl == "1h" then "2x input (1-hour TTL)" else "1.25x input (5-minute TTL)" end).",
  "- \($uncosted | length) ended session beads carry no token counts (mostly closed as retired by the session reaper) and \($live) are still live; \($unfillable) of the ended ones belong to a role with no recorded session to extrapolate from.",
  "- \($dec_e2e) e2e-fixture decision beads excluded; \(($dec | length) - ($lat | length)) decisions unanswered or without responded_at.",
  "- \($unscored) captain runs have no progress_score and are left out of the no-op rate."
'
