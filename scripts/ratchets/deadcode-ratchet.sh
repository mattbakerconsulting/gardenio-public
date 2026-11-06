#!/usr/bin/env bash
# deadcode-ratchet.sh — no new function that no shipped main reaches
# (gd-d29n0m678l).
#
# Usage:
#   deadcode-ratchet.sh check  <module-dir>   fail on a dead func not in the baseline
#   deadcode-ratchet.sh update <module-dir>   drop baseline entries that are no longer dead
#   deadcode-ratchet.sh init   <module-dir>   write the baseline from scratch
#
# Runs golang.org/x/tools/cmd/deadcode once, without -test, with every
# shipped main of the module as a root: the "shipped-union" list of
# scripts/reachability/deadcode.sh, the deletion set the audit-and-shrink plan
# works from. A func on that list that is not in <module-dir>/.deadcode-baseline.tsv
# fails the check: new code no shipped binary calls, or a call removed so that
# a func went dead. Fix it by calling it from shipped code or deleting it; a
# helper only tests use belongs in a _test.go file.
#
# THE BASELINE MAY ONLY SHRINK. `update` keeps the entries that are still
# dead and adds none, so deleting dead code never turns the check red and
# never needs a baseline edit. `init` is for the first baseline only.
#
# Baseline: "package<TAB>func" rows, sorted, "#" lines are comments. The
# shipped mains are the defaults of .github/workflows/reachability.yml (the
# self-test holds the two lists equal); SHIPPED_MAINS overrides them.
#
# Not on an agent pod for the controller: one run type-checks and builds SSA
# for seven mains, well past a pod's memory (advice gd-QLhMjEsBsR). checks.yml
# runs it in the lint-go job; kbeads (cmd/kd) peaks at about 2.1 GB.
#
# Env: DEADCODE (binary, default "deadcode"), SHIPPED_MAINS.

set -euo pipefail
export LC_ALL=C # sort and comm must agree on order

MODE=${1:-}
MODULE_DIR=${2:-}
DEADCODE=${DEADCODE:-deadcode}

case "$MODE" in
  check | update | init) ;;
  *) echo "usage: $0 <check|update|init> <module-dir>" >&2; exit 2 ;;
esac
if [[ -z "$MODULE_DIR" || ! -f "$MODULE_DIR/go.mod" ]]; then
  echo "deadcode-ratchet: '$MODULE_DIR' is not a Go module dir" >&2
  exit 2
fi

if [[ -z "${SHIPPED_MAINS:-}" ]]; then
  case "$(cd "$MODULE_DIR" && pwd)" in
    */gardenio/controller) SHIPPED_MAINS="cmd/controller cmd/slack-bridge cmd/grafana-bridge cmd/github-bridge cmd/trigger-engine cmd/landgate cmd/beads-viewer cmd/gb" ;;
    */kbeads) SHIPPED_MAINS="cmd/kd" ;;
    *) echo "deadcode-ratchet: no shipped mains known for $MODULE_DIR; set SHIPPED_MAINS" >&2; exit 2 ;;
  esac
fi

BASELINE="$MODULE_DIR/.deadcode-baseline.tsv"
if [[ "$MODE" != init && ! -f "$BASELINE" ]]; then
  echo "deadcode-ratchet: no baseline at $BASELINE" >&2
  exit 2
fi

TMP=$(mktemp -d)
trap 'rm -rf "$TMP"' EXIT

read -r -a mains <<<"$SHIPPED_MAINS"
roots=()
for m in "${mains[@]}"; do roots+=("./$m"); done

# The format of deadcode.sh plus the position, which only the report uses.
FMT='{{range .Funcs}}{{$.Path}}	{{.Name}}	{{.Position}}{{println}}{{end}}'
if ! (cd "$MODULE_DIR" && "$DEADCODE" -f "$FMT" "${roots[@]}") >"$TMP/raw" 2>"$TMP/err"; then
  cat "$TMP/err" >&2
  echo "deadcode-ratchet: deadcode failed in $MODULE_DIR" >&2
  exit 1
fi
cut -f1,2 "$TMP/raw" | sort -u >"$TMP/now"
grep -v '^#' "$BASELINE" 2>/dev/null | sort -u >"$TMP/base" || true

write() { # write <rows-file>
  {
    cat <<EOF
# Funcs no shipped main reaches, as deadcode (no -test) reported them when the
# ratchet landed (gd-d29n0m678l). scripts/ratchets/deadcode-ratchet.sh check
# fails on a dead func not listed here. The list may only shrink: run
# \`scripts/ratchets/deadcode-ratchet.sh update $MODULE_DIR\` after deleting
# dead code, and never add a row by hand.
# Shipped mains: $SHIPPED_MAINS
# package	func
EOF
    cat "$1"
  } >"$BASELINE"
}

case "$MODE" in
  init)
    write "$TMP/now"
    echo "deadcode-ratchet: wrote $(wc -l <"$TMP/now" | tr -d ' ') dead funcs to $BASELINE"
    exit 0
    ;;
  update)
    comm -12 "$TMP/base" "$TMP/now" >"$TMP/keep"
    write "$TMP/keep"
    echo "deadcode-ratchet: $BASELINE keeps $(wc -l <"$TMP/keep" | tr -d ' ') of $(wc -l <"$TMP/base" | tr -d ' ') entries"
    exit 0
    ;;
esac

comm -13 "$TMP/base" "$TMP/now" >"$TMP/new"
gone=$(comm -23 "$TMP/base" "$TMP/now" | wc -l | tr -d ' ')
if [[ ! -s "$TMP/new" ]]; then
  echo "deadcode-ratchet: $MODULE_DIR: $(wc -l <"$TMP/now" | tr -d ' ') dead funcs, none new (baseline $(wc -l <"$TMP/base" | tr -d ' '))"
  if [[ "$gone" -gt 0 ]]; then
    echo "  $gone baseline entries are no longer dead; \`$0 update $MODULE_DIR\` shrinks the list"
  fi
  exit 0
fi

echo "deadcode-ratchet: $MODULE_DIR: $(wc -l <"$TMP/new" | tr -d ' ') funcs that no shipped main ($SHIPPED_MAINS) reaches are new:"
# Join back to the positions for the report.
awk -F '\t' 'NR == FNR { want[$1 "\t" $2] = 1; next } ($1 "\t" $2) in want { print }' "$TMP/new" "$TMP/raw" | sort -u |
  while IFS=$'\t' read -r pkg fn pos; do
    echo "  $pkg  $fn  ($pos)"
    if [[ -n "${GITHUB_ACTIONS:-}" ]]; then
      echo "::error::unreachable from every shipped main: $pkg.$fn ($pos)"
    fi
  done
cat <<'EOF'

Call each one from shipped code or delete it. A helper only tests use belongs
in a _test.go file. Do not add these to the baseline: it may only shrink. See
"Ratchets" in gardenio/docs/code-reduction-targets.md.
EOF
exit 1
