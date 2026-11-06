#!/usr/bin/env bash
# build-seed-config_test.sh — Unit tests for the box-config seed BUILD pipeline
# (MVC M3, ka-0000003598). Home epic ka-0000000011.
#
# The full pipeline needs a reference beads DB (kd), so it can't run end-to-end in
# CI. But its two load-bearing pure functions ARE testable without kd or network:
#   - redact_text  — genericizes a keep-redacted body (ACME specifics → placeholders)
#   - guard_scan   — fail-closed deny-list re-scan (the defense that makes the seed honest)
# We SOURCE the SUT with SOURCED_FOR_TEST=1 (main() is guarded off) and exercise
# both against synthetic bodies + a synthetic deny-list. No kd, no network, no root.
#
# Also validates the data files ship in a consistent, parseable shape.
#
# Usage:  bash scripts/inbox/build-seed-config_test.sh
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "$0")" && pwd)"
SUT="${SCRIPT_DIR}/build-seed-config.sh"
KEEPLIST="${SCRIPT_DIR}/seed-config-keeplist.tsv"
DENYLIST="${SCRIPT_DIR}/seed-redaction-denylist.txt"

PASS=0; FAIL=0
TMP="$(mktemp -d)"; trap 'rm -rf "$TMP"' EXIT
pass() { echo "PASS: $1"; PASS=$((PASS + 1)); }
fail() { echo "FAIL: $1: $2"; FAIL=$((FAIL + 1)); }

# Source the SUT for its pure functions (main() is guarded by SOURCED_FOR_TEST).
# shellcheck disable=SC1090
SOURCED_FOR_TEST=1 . "$SUT"

command -v redact_text >/dev/null 2>&1 || { echo "FATAL: redact_text not exposed" >&2; exit 2; }
command -v guard_scan  >/dev/null 2>&1 || { echo "FATAL: guard_scan not exposed"  >&2; exit 2; }

# ── redact_text ──────────────────────────────────────────────────────────────
# Every ACME-internal specific a keep-redacted body might name must genericize AND
# leave the body clean against the real deny-list.
r_hits_deny() { # <text> — 0 if the text still trips the SHIPPED deny-list
  printf '%s' "$1" | guard_scan "$DENYLIST" >/dev/null 2>&1 && return 1 || return 0
}

t_redact_host() {
  local out; out="$(printf 'ssh to bastion.e2e.dev.acme.example now' | redact_text)"
  case "$out" in *e2e.dev.acme.example*) fail redact-host "host survived: $out"; return;; esac
  pass redact-host
}
t_redact_account() {
  local out; out="$(printf 'account 123456789012 is prod' | redact_text)"
  case "$out" in *123456789012*) fail redact-account "account survived: $out"; return;; esac
  pass redact-account
}
t_redact_system() {
  local out; out="$(printf 'push to gardenio-next and check monorepo-dark' | redact_text)"
  case "$out" in *gardenio-next*|*monorepo-dark*) fail redact-system "system survived: $out"; return;; esac
  pass redact-system
}
t_redact_clean_vs_deny() {
  # A body naming several internal specifics, after redaction, must NOT trip the
  # shipped deny-list — redact_text and the deny-list are the same boundary.
  local body out
  body='Deploy via acme-helm-chart to gardenio-prod-agent (acct 123456789012); logs in mezmo; ticket DEVOPS-42; host x.e2e.dev.acme.example; jira at acme.atlassian.net'
  out="$(printf '%s' "$body" | redact_text)"
  if printf '%s' "$out" | guard_scan "$DENYLIST" >/dev/null 2>&1; then
    pass redact-clean-vs-deny
  else
    fail redact-clean-vs-deny "redacted body still trips the deny-list: $(printf '%s' "$out" | guard_scan "$DENYLIST")"
  fi
}
t_redact_keep_verbatim() {
  # A generic body naming no internal specifics must pass through byte-identical.
  local body='Link every MR to its bead with gb mr link — generic beads hygiene.'
  local out; out="$(printf '%s' "$body" | redact_text)"
  [ "$out" = "$body" ] && pass redact-keep-verbatim || fail redact-keep-verbatim "mutated a clean body: $out"
}

# ── guard_scan ───────────────────────────────────────────────────────────────
t_guard_clean_pass() {
  local dl="$TMP/dl.txt"
  printf '# comment\n\nsecret-token\n123456789012\n' > "$dl"
  if printf 'a perfectly generic advice body about gb mr link' | guard_scan "$dl" >/dev/null; then
    pass guard-clean-pass
  else
    fail guard-clean-pass "clean text was rejected"
  fi
}
t_guard_catches_leak() {
  local dl="$TMP/dl.txt"
  printf 'secret-token\n123456789012\n' > "$dl"
  if printf 'oops account 123456789012 leaked' | guard_scan "$dl" >/dev/null; then
    fail guard-catches-leak "leak passed the guard"
  else
    pass guard-catches-leak
  fi
}
t_guard_case_insensitive() {
  local dl="$TMP/dl.txt"; printf 'mezmo\n' > "$dl"
  if printf 'logs go to MEZMO today' | guard_scan "$dl" >/dev/null; then
    fail guard-case-insensitive "uppercase MEZMO slipped the guard"
  else
    pass guard-case-insensitive
  fi
}
t_guard_skips_comments() {
  # A deny-list of ONLY comments/blanks must match nothing (never false-positive).
  local dl="$TMP/dl.txt"; printf '# just a comment\n\n#another\n' > "$dl"
  if printf 'anything at all 123456789012' | guard_scan "$dl" >/dev/null; then
    pass guard-skips-comments
  else
    fail guard-skips-comments "comment/blank line was treated as a pattern"
  fi
}
t_guard_reports_pattern() {
  local dl="$TMP/dl.txt"; printf 'gardenio-next\n' > "$dl"
  local out; out="$(printf 'mirror to gardenio-next' | guard_scan "$dl" || true)"
  case "$out" in DENY*gardenio-next*) pass guard-reports-pattern;;
    *) fail guard-reports-pattern "no DENY row for the match: $out";; esac
}

# ── data-file integrity ──────────────────────────────────────────────────────
t_denylist_eres_valid() {
  # Every non-comment deny-list line must be a valid ERE (grep -E accepts it).
  local bad=0 pat
  while IFS= read -r pat; do
    case "$pat" in ''|'#'*) continue;; esac
    printf '' | grep -iEn -- "$pat" >/dev/null 2>&1 || { [ $? -gt 1 ] && { echo "  bad ERE: $pat"; bad=1; }; }
  done < "$DENYLIST"
  [ "$bad" = "0" ] && pass denylist-eres-valid || fail denylist-eres-valid "an ERE failed to compile"
}
t_keeplist_shape() {
  # Every non-comment keeplist row is 3+ tab fields with a known family + disposition.
  local bad=0 family selector disposition rest
  while IFS=$'\t' read -r family selector disposition rest; do
    case "$family" in ''|'#'*) continue;; esac
    case "$family" in advice|role-context|type|prefix|skill) ;; *) echo "  bad family: $family"; bad=1;; esac
    case "$disposition" in keep|keep-redacted) ;; *) echo "  bad disposition: $disposition ($selector)"; bad=1;; esac
    [ -n "$selector" ] || { echo "  empty selector"; bad=1; }
  done < "$KEEPLIST"
  [ "$bad" = "0" ] && pass keeplist-shape || fail keeplist-shape "malformed keeplist row"
}
t_keeplist_advice_ids() {
  # advice selectors must be gd-IDs (kd-<alnum>); the build passes them to kd show.
  local bad=0 family selector rest
  while IFS=$'\t' read -r family selector rest; do
    case "$family" in advice) ;; *) continue;; esac
    case "$selector" in kd-*) ;; *) echo "  advice selector not a gd-ID: $selector"; bad=1;; esac
  done < "$KEEPLIST"
  [ "$bad" = "0" ] && pass keeplist-advice-ids || fail keeplist-advice-ids "non-gd-ID advice selector"
}
t_keeplist_no_dropped_id() {
  # Belt-and-suspenders: no keeplist advice row may name an id the inventory marks
  # DROP (ACME-internal). Guards against a copy-paste of a drop-row into the allow-list.
  local dropped="ka-0000005439 ka-0000004810 ka-0000002875 ka-0000003745 ka-0000004130 ka-0000007974 ka-0000006428 ka-0000007878 ka-0000006856 ka-0000007102 ka-0000007027"
  local bad=0 family selector rest d
  while IFS=$'\t' read -r family selector rest; do
    case "$family" in advice) ;; *) continue;; esac
    for d in $dropped; do [ "$selector" = "$d" ] && { echo "  DROP id in keeplist: $selector"; bad=1; }; done
  done < "$KEEPLIST"
  [ "$bad" = "0" ] && pass keeplist-no-dropped-id || fail keeplist-no-dropped-id "a DROP id is in the allow-list"
}

for t in t_redact_host t_redact_account t_redact_system t_redact_clean_vs_deny \
         t_redact_keep_verbatim t_guard_clean_pass t_guard_catches_leak \
         t_guard_case_insensitive t_guard_skips_comments t_guard_reports_pattern \
         t_denylist_eres_valid t_keeplist_shape t_keeplist_advice_ids \
         t_keeplist_no_dropped_id; do
  "$t"
done

echo
echo "build-seed-config_test: ${PASS} passed, ${FAIL} failed"
[ "$FAIL" -eq 0 ]
