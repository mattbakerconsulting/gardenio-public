#!/usr/bin/env bash
# lint-trickle-mr.sh
#
# Enforces that a trickle MR (monorepo-dark peel-on-demand delivery to real
# main) carries a complete structured PROVENANCE block, per R5 bundle
# ka-0000008221 §2.3. This is the lint that drives metric #8
# (provenance-completeness) to 100%.
#
# A trickle MR description must contain, between the markers
#   <!-- PROVENANCE:BEGIN --> ... <!-- PROVENANCE:END -->
# a bullet line for each required field, each with a non-placeholder value:
#   - Source dark commits/MRs
#   - Bead IDs
#   - Affordance results
#   - Dwell time in dark
#   - Beads-viewer link
#
# A field is considered MISSING if its key line is absent, or if the only
# content after the key is empty / still the template placeholder / an HTML
# comment.
#
# Usage:
#   ./scripts/lint-trickle-mr.sh <description-file>   # lint a file
#   ./scripts/lint-trickle-mr.sh -                    # lint stdin
#   ./scripts/lint-trickle-mr.sh                      # lint the GitLab MR
#                                                       description from CI env
#
# In GitLab CI the MR description is available as CI_MERGE_REQUEST_DESCRIPTION.
# When run with no args and that variable is set, the script lints it.
#
# This lint only applies to *trickle* MRs. An MR opts into the check by using
# the trickle template (which carries the PROVENANCE markers) or by setting
# TRICKLE_MR=1. If no PROVENANCE markers are present and TRICKLE_MR is unset,
# the script is a no-op (exit 0) — ordinary MRs are not gated.
#
# Companion template: .gitlab/merge_request_templates/trickle.md
# Companion test:     scripts/lint-trickle-mr_test.sh
# Task: ka-0000003961. Source: R5 bundle ka-0000008221 §2.3.

set -euo pipefail

PROG="$(basename "$0")"

# Required provenance fields. Each entry is "label|regex" where the regex
# matches the key portion of the bullet line (case-insensitive, after the
# leading "- **"). Keep these in sync with the trickle template.
REQUIRED_FIELDS=(
  "Source dark commits/MRs|source dark commits"
  "Bead IDs|bead ids"
  "Affordance results|affordance results"
  "Dwell time in dark|dwell time"
  "Beads-viewer link|beads-viewer link"
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

# Decide whether this MR is a trickle MR subject to the check.
#
# Only the real HTML-comment markers count — `<!-- PROVENANCE:BEGIN -->` and
# `<!-- PROVENANCE:END -->`. A bare or backtick-quoted mention of the token
# `PROVENANCE:BEGIN` in prose (e.g. an MR that *describes* this template) must
# NOT opt the MR into the check, or the lint false-positives on any MR that
# merely talks about provenance. The markers must therefore appear wrapped in
# `<!-- ... -->`.
MARKER_BEGIN_RE='<!--[[:space:]]*PROVENANCE:BEGIN[[:space:]]*-->'
MARKER_END_RE='<!--[[:space:]]*PROVENANCE:END[[:space:]]*-->'
has_markers=0
if grep -qE "$MARKER_BEGIN_RE" <<<"$desc" && grep -qE "$MARKER_END_RE" <<<"$desc"; then
  has_markers=1
fi

if (( ! has_markers )) && [[ "${TRICKLE_MR:-}" != "1" ]]; then
  # Not a trickle MR — nothing to enforce.
  exit 0
fi

if (( ! has_markers )); then
  err "TRICKLE_MR=1 but no PROVENANCE block found."
  err "A trickle MR must include a provenance block delimited by"
  err "  <!-- PROVENANCE:BEGIN --> ... <!-- PROVENANCE:END -->"
  err "Use .gitlab/merge_request_templates/trickle.md."
  exit 1
fi

# Extract the content strictly between the real HTML-comment markers.
block="$(awk '
  /<!--[[:space:]]*PROVENANCE:BEGIN[[:space:]]*-->/ { inblk=1; next }
  /<!--[[:space:]]*PROVENANCE:END[[:space:]]*-->/   { inblk=0 }
  inblk                                             { print }
' <<<"$desc")"

# Strip an HTML comment ( <!-- ... --> ) from a single line, then trim.
strip_comment_and_trim() {
  sed -E 's/<!--.*-->//g' <<<"$1" | sed -E 's/^[[:space:]]+//; s/[[:space:]]+$//'
}

missing=()
for entry in "${REQUIRED_FIELDS[@]}"; do
  label="${entry%%|*}"
  keyre="${entry##*|}"

  # Find the bullet line for this field. Format: "- **<Key>:** <value>"
  line="$(grep -iE "^[[:space:]]*-[[:space:]]*\*\*[[:space:]]*${keyre}" <<<"$block" | head -n1 || true)"
  if [[ -z "$line" ]]; then
    missing+=("$label (field line absent)")
    continue
  fi

  # Value is everything after the first "**" closing the key, i.e. after ":**".
  value="${line#*:\*\*}"
  # If the regex matched a key without the ":**" shape, fall back to after "**...**".
  if [[ "$value" == "$line" ]]; then
    value="${line#*\*\*}"; value="${value#*\*\*}"
  fi
  value="$(strip_comment_and_trim "$value")"

  # Reject empty or obvious placeholder values.
  case "$value" in
    "" )
      missing+=("$label (no value)") ;;
    *"e.g."*|*"goes here"*|*"XXXX"*|*"<!--"* )
      missing+=("$label (placeholder value: '${value}')") ;;
    _*|"…"|"..." )
      missing+=("$label (placeholder value: '${value}')") ;;
  esac
done

if (( ${#missing[@]} > 0 )); then
  err "trickle MR provenance block is incomplete (R5 §2.3 / ka-0000003961):"
  for m in "${missing[@]}"; do
    err "  - missing or placeholder: ${m}"
  done
  err "Fill in every field in the PROVENANCE block of the trickle template."
  exit 1
fi

echo "${PROG}: provenance block complete (${#REQUIRED_FIELDS[@]} fields present)."
