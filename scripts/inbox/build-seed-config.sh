#!/usr/bin/env bash
# build-seed-config.sh — Box-config seed BUILD pipeline (MVC M3, ka-0000003598).
#
# Home epic: ka-0000000011 (minimum-viable config/prompt subset for the boxed
# gardenio). Plan of record: gardenio/docs/box-config-seeding.md. Q4 resolution
# (decision ka-0000002935): the box restores a pre-baked, ALREADY-REDACTED seed
# produced deterministically from reviewable inputs — never hand-rolled.
#
# WHAT THIS DOES (extract → redact → guard → manifest → package):
#   1. EXTRACT  — read seed-config-keeplist.tsv (the allow-list of kept config).
#                 For each advice row, pull the bead from a reference beads DB via
#                 `kd show <id> --json`. role-context/type/prefix/skill rows are
#                 metadata the box already carries in-binary/in-image; they ride in
#                 the manifest for audit but seed no advice body.
#   2. REDACT   — for keep-redacted rows, genericize the body via REDACT_SUBS
#                 (ACME host/account/system names → friend-grade placeholders).
#                 keep rows ship verbatim.
#   3. GUARD    — re-scan every FINAL body against seed-redaction-denylist.txt
#                 (guard_scan). ANY match = build FAILS. The seed can never ship a
#                 leaked internal ref. This is the defense that makes Q4's chosen
#                 "opaque blob" honest.
#   4. MANIFEST — emit a reviewable seed-manifest.tsv (one row per selected artifact:
#                 id, family, disposition) + a seed-redaction-report.md (human audit
#                 of WHAT shipped + what genericization fired), so a human audits the
#                 seed without cracking it.
#   5. PACKAGE  — write the seed itself as JSON-lines (gardenio-seed-<calver>.jsonl,
#                 one bead per line: what seed-config-dump.sh restores) + a .sha256.
#
# The seed is JSON-lines, NOT a pg_dump binary: reviewable as a diff, restorable
# with `kd create` on ANY beads DB (the box's fresh in-box daemon), no pg tooling.
#
# Usage:
#   build-seed-config.sh --version <calver> [--out-dir DIR]
#       [--keeplist FILE] [--denylist FILE] [--kd-bin kd] [--allow-missing]
#
# In CI/release this runs against a clean reference beads DB (kd pointed at it).
# --allow-missing tolerates a keeplist advice id absent from the reference DB
# (logs a WARN + a `missing` manifest row) instead of hard-failing — for building
# a partial seed in an env that lacks every prod advice bead.
#
# bash -n + shellcheck clean. Companion unit test: build-seed-config_test.sh
# exercises redact_text + guard_scan as pure functions (no kd, no network).
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "$0")" && pwd)"

log()  { printf '\033[1;36m[seed-build]\033[0m %s\n' "$*"; }
warn() { printf '\033[1;33m[seed-build] WARN:\033[0m %s\n' "$*" >&2; }
die()  { printf '\033[1;31m[seed-build] FATAL:\033[0m %s\n' "$*" >&2; exit 1; }

# ── REDACT: genericize a keep-redacted body ──────────────────────────────────
# redact_text — read a body on stdin, write the genericized body on stdout.
# The substitutions map ACME-internal specifics named in a keep-redacted body to
# friend-grade placeholders so the SHAPE of the advice survives while the internal
# reference does not. Every sub here must leave the body CLEAN against the guard's
# deny-list — REDACT_SUBS and the deny-list are two views of the same boundary.
# Kept deliberately narrow: substitute what a keep-redacted advice body actually
# names; anything else the guard fail-closes (drops) rather than silently ships.
redact_text() {
  sed -E \
    -e 's#[a-z0-9.-]*\.e2e\.dev\.acme\.ai#your-gardenio.example.internal#g' \
    -e 's#[a-z0-9.-]*\.acme\.ai#example.internal#g' \
    -e 's#acme\.atlassian\.net#your-issue-tracker.example#g' \
    -e 's#registry\.gitlab\.com/acme[a-zA-Z0-9/._-]*#your-registry.example/gardenio#g' \
    -e 's#123456789012#<your-aws-account-id>#g' \
    -e 's#arn:aws:[a-z0-9:_/-]+#<your-aws-arn>#g' \
    -e 's#gardenio-agents?-[a-z0-9-]+#<your-agent-iam-role>#g' \
    -e 's#gardenio-prod-agent#<your-agent-iam-role>#g' \
    -e 's#monorepo-dark#<your-private-repo-workflow>#g' \
    -e 's#gardenio-next#<your-config-mirror>#g' \
    -e 's#perf[ -]bastion#<your-perf-host>#g' \
    -e 's#acme-helm-chart#<your-app-chart>#g' \
    -e 's#RDS master#<your-db-admin>#g' \
    -e 's#[Mm]ezmo#<your-log-pipeline>#g' \
    -e 's#DEVOPS-[0-9]+#<TICKET-N>#g' \
    -e 's#PE-[0-9]+#<TICKET-N>#g' \
    -e 's#PE-epic#<your-epic-workflow>#g' \
    -e 's#customer[ -]PROD([ -]tenant)?#<your-customer-env>#g' \
    -e 's#jira-mention#<issue-mention-role>#g' \
    -e 's#fsm-triage#<triage-role>#g' \
    -e 's#RWX_ACCESS_TOKEN#<your-ci-token>#g' \
    -e 's#JIRA_(API_)?TOKEN#<your-issue-tracker-token>#g' \
    -e 's#SLACK_[A-Z_]*TOKEN#<your-chat-token>#g' \
    -e 's#CLAUDE_CODE_OAUTH_TOKEN#<your-model-oauth-token>#g' \
    -e 's#BEADS_OPERATED_BY#<your-operator-env-var>#g' \
    -e 's#xox[baprs]-[a-z0-9-]+#<your-chat-token>#g' \
    -e 's#[Kk]eycloak[ -]?admin#<your-sso-admin>#g'
}

# ── GUARD: fail-closed deny-list re-scan ─────────────────────────────────────
# guard_scan <denylist-file> — read candidate text on stdin, scan it against every
# ERE pattern in <denylist-file> (case-insensitive). Prints each matched pattern +
# the offending line on stdout. EXIT 0 = clean (no match); EXIT 1 = at least one
# match (the caller DROPS/ FAILS on this). Comment/blank lines in the deny-list are
# skipped. Pure: no kd, no network, no globals.
guard_scan() {
  local denylist="$1" text pattern hit=0
  text="$(cat)"
  while IFS= read -r pattern; do
    case "$pattern" in ''|'#'*) continue;; esac
    if printf '%s\n' "$text" | grep -iEn -- "$pattern" >/dev/null 2>&1; then
      printf 'DENY\t%s\t%s\n' "$pattern" \
        "$(printf '%s\n' "$text" | grep -iEn -- "$pattern" | head -1)"
      hit=1
    fi
  done < "$denylist"
  return "$hit"
}

# ── main (skipped when SOURCED_FOR_TEST=1 so the test can extract the funcs) ──
main() {
  local VERSION="" OUT_DIR="" KEEPLIST="" DENYLIST="" KD_BIN="kd" ALLOW_MISSING=0
  while [ $# -gt 0 ]; do
    case "$1" in
      --version) VERSION="$2"; shift 2;;
      --out-dir) OUT_DIR="$2"; shift 2;;
      --keeplist) KEEPLIST="$2"; shift 2;;
      --denylist) DENYLIST="$2"; shift 2;;
      --kd-bin) KD_BIN="$2"; shift 2;;
      --allow-missing) ALLOW_MISSING=1; shift;;
      -h|--help) sed -n '2,52p' "$0"; exit 0;;
      *) die "unknown arg: $1";;
    esac
  done

  [ -n "$VERSION" ] || die "--version <calver> is required"
  KEEPLIST="${KEEPLIST:-$SCRIPT_DIR/seed-config-keeplist.tsv}"
  DENYLIST="${DENYLIST:-$SCRIPT_DIR/seed-redaction-denylist.txt}"
  OUT_DIR="${OUT_DIR:-$(pwd)/tarboat/seed}"
  [ -f "$KEEPLIST" ] || die "keeplist not found: $KEEPLIST"
  [ -f "$DENYLIST" ] || die "denylist not found: $DENYLIST"
  command -v "$KD_BIN" >/dev/null 2>&1 || die "kd binary '$KD_BIN' not on PATH"

  install -d -m 0755 "$OUT_DIR"
  local seed="$OUT_DIR/gardenio-seed-${VERSION}.jsonl"
  local manifest="$OUT_DIR/seed-manifest.tsv"
  local report="$OUT_DIR/seed-redaction-report.md"
  : > "$seed"
  printf 'id\tfamily\tselector\tdisposition\tstatus\n' > "$manifest"

  local kept=0 redacted=0 missing=0 dropped=0
  {
    echo "# Box-config seed — redaction report (calver ${VERSION})"
    echo
    echo "Generated by \`scripts/inbox/build-seed-config.sh\` from the allow-list"
    echo "\`seed-config-keeplist.tsv\` + deny-list \`seed-redaction-denylist.txt\`."
    echo "Every seeded body was re-scanned by the guard (\`guard_scan\`); the build"
    echo "FAILS on any deny-list match, so this seed carries zero leaked internal refs."
    echo
    echo "| family | selector | disposition | status |"
    echo "|---|---|---|---|"
  } > "$report"

  # Walk the keeplist. Only advice rows pull + seed a bead body; the rest are
  # metadata the box carries in-binary/in-image and only ride the manifest.
  while IFS=$'\t' read -r family selector disposition _note; do
    case "$family" in ''|'#'*) continue;; esac
    if [ "$family" != "advice" ]; then
      printf '%s\t%s\t%s\t%s\t%s\n' "-" "$family" "$selector" "$disposition" "meta" >> "$manifest"
      printf '| %s | %s | %s | meta |\n' "$family" "$selector" "$disposition" >> "$report"
      continue
    fi

    local id="$selector" raw body
    if ! raw="$("$KD_BIN" show "$id" --json 2>/dev/null)"; then
      if [ "$ALLOW_MISSING" = "1" ]; then
        warn "advice $id not in reference DB — recording 'missing', continuing"
        printf '%s\t%s\t%s\t%s\t%s\n' "$id" "$family" "$selector" "$disposition" "missing" >> "$manifest"
        printf '| %s | %s | %s | missing |\n' "$family" "$id" "$disposition" >> "$report"
        missing=$((missing + 1)); continue
      fi
      die "advice $id not found in reference DB (pass --allow-missing to tolerate)"
    fi

    # title + description reconstruct the seedable advice body.
    body="$(printf '%s' "$raw" | jq -r '.title + "\n\n" + (.description // "")')"
    if [ "$disposition" = "keep-redacted" ]; then
      body="$(printf '%s' "$body" | redact_text)"
      redacted=$((redacted + 1))
    fi

    # GUARD: fail-closed re-scan of the FINAL body.
    local diag
    if diag="$(printf '%s' "$body" | guard_scan "$DENYLIST")"; then
      : # clean
    else
      dropped=$((dropped + 1))
      printf '%s\t%s\t%s\t%s\t%s\n' "$id" "$family" "$selector" "$disposition" "DROPPED-leak" >> "$manifest"
      printf '| %s | %s | %s | **DROPPED (leak)** |\n' "$family" "$id" "$disposition" >> "$report"
      warn "GUARD: advice $id still matched the deny-list after redaction — DROPPED:"
      printf '%s\n' "$diag" | sed 's/^/    /' >&2
      die "guard rejected advice $id — a kept body leaks an internal ref; fix redact_text or drop it from the keeplist"
    fi

    # Seed a JSON-lines record the box restores with `kd create`.
    printf '%s' "$raw" | jq -c --arg body "$body" \
      '{seed_kind:"advice", id:.id, type:.type, title:.title,
        body:$body, labels:(.labels // [])}' >> "$seed"
    printf '%s\t%s\t%s\t%s\t%s\n' "$id" "$family" "$selector" "$disposition" "seeded" >> "$manifest"
    printf '| %s | %s | %s | seeded |\n' "$family" "$id" "$disposition" >> "$report"
    kept=$((kept + 1))
  done < "$KEEPLIST"

  {
    echo
    echo "**Tally:** ${kept} advice seeded · ${redacted} genericized · ${missing} missing · ${dropped} dropped-leak."
  } >> "$report"

  ( cd "$OUT_DIR" && sha256sum "$(basename "$seed")" > "$(basename "$seed").sha256" )

  log "seed:     $seed  (${kept} advice bodies)"
  log "manifest: $manifest"
  log "report:   $report"
  log "sha256:   ${seed}.sha256"
  log "done. ${kept} seeded / ${redacted} genericized / ${missing} missing / ${dropped} dropped."
}

# Only run main when executed directly. Sourcing (SOURCED_FOR_TEST=1) exposes
# redact_text + guard_scan to the unit test without running the pipeline.
if [ "${SOURCED_FOR_TEST:-0}" != "1" ]; then
  main "$@"
fi
