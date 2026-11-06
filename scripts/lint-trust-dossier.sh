#!/usr/bin/env bash
# lint-trust-dossier.sh
#
# Enforces that a monorepo-dark TRUST-DOSSIER MR carries a complete, non-trivial
# dossier, per R3 catalog /bundle/ka-0000005053 §2.6 (rationale HARD-on-presence)
# and §4 (the dossier template). Task ka-0000005561; master epic ka-0000005560.
#
# The dossier is the RSI handoff artifact: prose that demonstrates UNDERSTANDING
# (Why / Alternatives / Risk) plus claim-matched Evidence and Provenance. This
# lint checks the machine-checkable half — PRESENCE + NON-TRIVIALITY of the
# required sections — exactly as §2.6 specifies. Qualitative depth stays a human
# judgment (ADVISORY) the lint does not adjudicate.
#
# A dossier MR description must contain, between the markers
#   <!-- DOSSIER:BEGIN --> ... <!-- DOSSIER:END -->
# a `## <Section>` heading for each required section, each followed by
# non-placeholder prose:
#   Summary, Why, Alternatives considered, Risk assessment, Evidence, Provenance
#
# The core non-triviality rule (§2.6, §4): the lint REJECTS if `Why` is empty or
# byte-equal to `Summary` — a dossier that restates the summary as its rationale
# demonstrates nothing.
#
# Escape-hatch trailers (§2.5) are validated when PRESENT: a
# `Trickle-Size-Exception:` or `Multi-Concern-Ack:` trailer must carry a
# non-trivial reason (mirroring the `Coverage-Bypass:` organic-trailer path — a
# bare trailer with no justification is rejected). Absence is fine; the bounds
# themselves (LOC/files/concern) are the trickle-gate's job, not this lint's.
#
# Usage:
#   ./scripts/lint-trust-dossier.sh <description-file>   # lint a file
#   ./scripts/lint-trust-dossier.sh -                    # lint stdin
#   ./scripts/lint-trust-dossier.sh                      # lint the GitLab MR
#                                                          description from CI env
#
# In GitLab CI the MR description is available as CI_MERGE_REQUEST_DESCRIPTION.
# When run with no args and that variable is set, the script lints it.
#
# This lint only applies to *trust-dossier* MRs. An MR opts into the check by
# using the trust-dossier template (which carries the DOSSIER markers) or by
# setting DOSSIER_MR=1. If no DOSSIER markers are present and DOSSIER_MR is
# unset, the script is a no-op (exit 0) — ordinary MRs are not gated.
#
# Companion template: .gitlab/merge_request_templates/trust-dossier.md
# Companion test:     scripts/lint-trust-dossier_test.sh

set -euo pipefail

PROG="$(basename "$0")"

# Required dossier sections, in canonical order. Each entry is "label|regex"
# where the regex matches the heading text after the leading "## "
# (case-insensitive). Keep these in sync with the trust-dossier template.
REQUIRED_SECTIONS=(
  "Summary|summary"
  "Why|why"
  "Alternatives considered|alternatives considered"
  "Risk assessment|risk assessment"
  "Evidence|evidence"
  "Provenance|provenance"
)

err() { echo "${PROG}: $*" >&2; }

# Read the MR description from: explicit file arg, stdin ("-"), or the
# CI_MERGE_REQUEST_DESCRIPTION env var (GitLab CI).
read_description() {
  if (( $# >= 1 )); then
    if [[ "$1" == "-" ]]; then
      cat
    else
      [[ -f "$1" ]] || { err "no such file: $1"; exit 2; }
      cat "$1"
    fi
  elif [[ -n "${CI_MERGE_REQUEST_DESCRIPTION:-}" ]]; then
    printf '%s' "$CI_MERGE_REQUEST_DESCRIPTION"
  else
    err "no description provided (pass a file, '-' for stdin, or set CI_MERGE_REQUEST_DESCRIPTION)"
    exit 2
  fi
}

desc="$(read_description "$@")"

# Decide whether this MR is a trust-dossier MR subject to the check. Only the
# real HTML-comment markers count — a bare or backtick-quoted mention of the
# token `DOSSIER:BEGIN` in prose (e.g. an MR that *describes* this template)
# must NOT opt the MR in, or the lint false-positives on any MR that merely
# talks about the dossier. The markers must appear wrapped in `<!-- ... -->`.
MARKER_BEGIN_RE='<!--[[:space:]]*DOSSIER:BEGIN[[:space:]]*-->'
MARKER_END_RE='<!--[[:space:]]*DOSSIER:END[[:space:]]*-->'
has_markers=0
if grep -qE "$MARKER_BEGIN_RE" <<<"$desc" && grep -qE "$MARKER_END_RE" <<<"$desc"; then
  has_markers=1
fi

if (( ! has_markers )) && [[ "${DOSSIER_MR:-}" != "1" ]]; then
  # Not a trust-dossier MR — nothing to enforce.
  exit 0
fi

if (( ! has_markers )); then
  err "DOSSIER_MR=1 but no dossier block found."
  err "A trust-dossier MR must include a block delimited by"
  err "  <!-- DOSSIER:BEGIN --> ... <!-- DOSSIER:END -->"
  err "Use .gitlab/merge_request_templates/trust-dossier.md."
  exit 1
fi

# Extract the content strictly between the real HTML-comment markers.
block="$(awk '
  /<!--[[:space:]]*DOSSIER:BEGIN[[:space:]]*-->/ { inblk=1; next }
  /<!--[[:space:]]*DOSSIER:END[[:space:]]*-->/   { inblk=0 }
  inblk                                          { print }
' <<<"$desc")"

# Extract the prose body of a `## <Section>` heading: every line after the
# heading up to (but not including) the next `## ` heading or end of block.
# Strips HTML comments and bullet markers, collapses whitespace, and trims — so
# a section that contains only a template comment / placeholder reads as empty.
section_body() {
  local keyre="$1"
  awk -v keyre="$keyre" '
    BEGIN { IGNORECASE=1; inSec=0 }
    /^[[:space:]]*##[[:space:]]+/ {
      # A new heading. Are we entering our section, or leaving it?
      hdr=$0
      sub(/^[[:space:]]*##[[:space:]]+/, "", hdr)
      sub(/[[:space:]]+$/, "", hdr)
      if (tolower(hdr) ~ ("^" keyre "$")) { inSec=1; next }
      if (inSec) { inSec=0 }
      next
    }
    inSec { print }
  ' <<<"$block" \
    | sed -E 's/<!--.*-->//g' \
    | sed -E 's/^[[:space:]]*[-*][[:space:]]+//' \
    | tr -s '[:space:]' ' ' \
    | sed -E 's/^[[:space:]]+//; s/[[:space:]]+$//'
}

# A value is a placeholder if it is empty or matches an obvious template stub.
is_placeholder() {
  local v="$1"
  case "$v" in
    "" ) return 0 ;;
    *"e.g."*|*"goes here"*|*"XXXX"*|*"<!--"* ) return 0 ;;
    _*|"…"|"..." ) return 0 ;;
  esac
  # A body that is nothing but italicized placeholder prose (the template's
  # `_..._` stubs) is also a placeholder. Strip a single wrapping pair of
  # underscores and re-check for known stub phrasing.
  local inner="$v"
  if [[ "$inner" == _*_ ]]; then
    inner="${inner#_}"; inner="${inner%_}"
    case "$inner" in
      *"summary of what this slice"*|*"problem this solves"*|\
      *"named alternative"*|*"rollback plan"*|*"claim-matched evidence"*|\
      *"peeled from"*|*"scope boundary"* ) return 0 ;;
    esac
  fi
  return 1
}

missing=()
summary_body=""
why_body=""
for entry in "${REQUIRED_SECTIONS[@]}"; do
  label="${entry%%|*}"
  keyre="${entry##*|}"

  body="$(section_body "$keyre")"

  # Remember Summary and Why for the byte-equality non-triviality check.
  [[ "$label" == "Summary" ]] && summary_body="$body"
  [[ "$label" == "Why" ]] && why_body="$body"

  # Heading present but body a placeholder?
  if ! grep -qiE "^[[:space:]]*##[[:space:]]+${keyre}[[:space:]]*$" <<<"$block"; then
    missing+=("${label} (heading absent)")
    continue
  fi
  if is_placeholder "$body"; then
    missing+=("${label} (empty or placeholder)")
  fi
done

# Core non-triviality rule: Why must not be empty or byte-equal to Summary.
if [[ -n "$why_body" && "$why_body" == "$summary_body" ]]; then
  missing+=("Why (byte-equal to Summary — restate the rationale, don't repeat)")
fi

# Escape-hatch trailer validation (only when present). A trailer must carry a
# non-trivial reason after the colon.
check_trailer() {
  local name="$1"
  local line
  line="$(grep -iE "^[[:space:]]*${name}:" <<<"$desc" | head -n1 || true)"
  [[ -z "$line" ]] && return 0   # absent — fine
  local reason="${line#*:}"
  reason="$(sed -E 's/<!--.*-->//g' <<<"$reason" | sed -E 's/^[[:space:]]+//; s/[[:space:]]+$//')"
  if is_placeholder "$reason"; then
    missing+=("${name} trailer present but reason is empty/placeholder")
  fi
}
check_trailer "Trickle-Size-Exception"
check_trailer "Multi-Concern-Ack"

if (( ${#missing[@]} > 0 )); then
  err "trust-dossier MR is incomplete (R3 §2.6/§4 / ka-0000005561):"
  for m in "${missing[@]}"; do
    err "  - ${m}"
  done
  err "Fill in every required section of the DOSSIER block in the trust-dossier template."
  exit 1
fi

echo "${PROG}: trust dossier complete (${#REQUIRED_SECTIONS[@]} sections present + non-trivial)."
