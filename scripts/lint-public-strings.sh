#!/usr/bin/env bash
# lint-public-strings.sh — fails when the tree reintroduces dead internal
# hosts / the old GitLab group path (--strings) or infra identifiers
# (--infra-ids). Spec: docs/superpowers/specs/2026-10-11-public-ready-scrub-design.md §4
# (plan epic gd-uNpaF2zvVH; this mode is Task 2, gd-WZyNtc0X1f).
#
# Usage: scripts/lint-public-strings.sh (--strings|--infra-ids|--all) [ROOT]
# Exit: 0 clean, 1 findings (one "path:line: text   [label]" per line), 2 usage.
#
# --strings flags the dead internal hosts and the old GitLab group path outside
# tests, fixtures and docs/archive. It never matches bare "acme": acmeServer
# and acme-challenge are Let's Encrypt, and Acme / acme.example stay as
# redaction placeholders in tests (spec §0). Strings take no allowlist entries.
#
# --infra-ids flags AWS resource ids (sg-/i-/vpc-/subnet-/ami-/snap-/vol-/eni-)
# that are not a fixture spelling, and the owner's AWS account id anywhere
# outside the operational sites in docs/public-readiness/allowlist.txt.
set -euo pipefail

MODE="${1:-}"; ROOT="${2:-$(cd "$(dirname "$0")/.." && pwd)}"
case "$MODE" in --strings|--infra-ids|--all) ;; *) echo "usage: $0 (--strings|--infra-ids|--all) [ROOT]" >&2; exit 2;; esac
cd "$ROOT"

EXCLUDES=(--exclude-dir=.worktrees --exclude-dir=.gb --exclude-dir=node_modules --exclude-dir=vendor
          --exclude-dir=gen --exclude-dir=.git --exclude-dir=dist --exclude-dir=target)
ALLOWLIST="docs/public-readiness/allowlist.txt"
# Split so this file never matches itself.
ACCOUNT_ID='360190''250083'
# Fixture spellings that are never real ids: zero-led, the 0123456789abcdef
# family (plus one trailing hex digit, so a doc can name distinct fixtures),
# the descending fedcba98765432 family, deadbeef and abcdef.
FIXTURE_IDS='-(0{6,}[0-9a-f]*|0123456789abcdef[0-9a-f]?|0?fedcba98765432[0-9a-f]*|deadbeef[0-9a-f]*|abcdef[0-9a-f]*)$'

# Tests, fixtures and the archive keep the old spellings on purpose.
STRING_EXEMPT=(--exclude='*_test.go' --exclude-dir=tests --exclude-dir=testdata --exclude-dir=fixtures
               --exclude-dir=fixture --exclude-dir=e2e --exclude-dir=archive)
# Split so this file never matches itself.
STRING_PATTERN='prod\.gardenio\.acme\.example|cli\.gardenio\.acme\.example|app\.e2e\.dev\.acme\.example|gitlab\.com/[Aa]cme|registry\.gitlab\.com/[Aa]cme|Acme''/Platform'

raw_strings() {
  { grep -rInE "${EXCLUDES[@]}" "${STRING_EXEMPT[@]}" -e "$STRING_PATTERN" . || true; } \
    | sed 's#^\./##; s#$#\tdead-string#'
}

# raw findings, one "path:line:text<TAB>label" per line
raw_infra_ids() {
  { grep -rInoE "${EXCLUDES[@]}" -e '\b(sg|i|vpc|subnet|ami|snap|vol|eni)-[0-9a-f]{8,17}\b' . || true; } \
    | { grep -vE -- "$FIXTURE_IDS" || true; } | sed 's#^\./##; s#$#\tinfra-id#'
  { grep -rInE "${EXCLUDES[@]}" -e "\\b${ACCOUNT_ID}\\b" . || true; } \
    | sed 's#^\./##; s#$#\taccount-id#'
}

# An allowlist entry is "path" (every line of a file whose occurrences are all
# load-bearing) or "path:line". It drops each finding whose "path:line:" begins
# with "entry:". An entry that drops nothing is stale and is itself a finding.
apply_allowlist() { # $1: raw findings; stdout: findings to report
  local raw="$1" kept="$1" keys entry
  [ -f "$ALLOWLIST" ] || { printf '%s\n' "$raw"; return; }
  keys="$(awk -F: 'NF > 2 { print $1 ":"; print $1 ":" $2 ":" }' <<<"$raw")"
  while IFS= read -r entry || [ -n "$entry" ]; do
    entry="${entry%%#*}"; entry="${entry%"${entry##*[![:space:]]}"}"
    [ -n "$entry" ] || continue
    if ! grep -qxF -- "$entry:" <<<"$keys"; then
      printf '%s:0:stale allowlist entry %s matches nothing\tstale-allowlist\n' "$ALLOWLIST" "'$entry'"
      continue
    fi
    kept="$(awk -v p="$entry:" 'index($0, p) != 1' <<<"$kept")"
  done < "$ALLOWLIST"
  printf '%s\n' "$kept"
}

findings=0
report() { # $1: findings
  local line
  while IFS= read -r line; do
    [ -n "$line" ] || continue
    printf '%s   [%s]\n' "${line%%$'\t'*}" "${line##*$'\t'}"
    findings=$((findings+1))
  done <<<"$1"
}

infra_ids() {
  report "$(apply_allowlist "$(raw_infra_ids)")"
}

strings_mode() {
  report "$(raw_strings)"
}

case "$MODE" in
  --infra-ids) infra_ids ;;
  --strings)   strings_mode ;;
  --all)       strings_mode; infra_ids ;;
esac

if [ "$findings" -gt 0 ]; then echo "lint-public-strings: $findings finding(s)" >&2; exit 1; fi
echo "lint-public-strings: clean ($MODE)"
