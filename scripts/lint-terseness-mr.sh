#!/usr/bin/env bash
# lint-terseness-mr.sh
#
# The verbosity-budget / terseness gate for monorepo-dark trickle MRs
# (peel-on-demand deliveries to real main), per R5 bundle ka-0000008221 §6
# task5. It enforces the two *leading* review-cost metrics on the delivery
# surface so rationale walls and oversized diffs never reach real main:
#
#   • Metric #6 — Diff size (added+removed lines). Baseline p50 174 / p90 733
#     LOC (ka-0000000929); target p50 ≤ 50, p90 ≤ 200. Quick-win gate from the
#     baseline: warn > 200, block > 500 without override.
#   • Metric #7 — Verbosity ratio = description prose ÷ diff substance,
#     `prose_chars / (1 + diff_chars)`, with the structured PROVENANCE block
#     excluded from the prose count. Baseline p50 0.166 / p90 0.865.
#
# The verbosity ratio is only enforced once the prose clears an absolute floor
# (TERSENESS_MIN_PROSE_CHARS). A tiny diff with a short, terse summary has a
# naturally high ratio (small denominator) but is exactly what we WANT — the
# floor makes the ratio bite only on genuine rationale WALLS, not on terse
# small MRs. This directly guards the R5 U4 failure mode ("over-indexing on the
# ratio could push agents to underexplain genuinely-needed context").
#
# This gate applies ONLY to trickle MRs — the same opt-in signal as
# scripts/lint-trickle-mr.sh: the real `<!-- PROVENANCE:BEGIN -->` marker in
# the description, or TRICKLE_MR=1. Ordinary MRs are a no-op pass (exit 0);
# verbose-in-lab is fine, terse-on-delivery is what we gate.
#
# Off-ramps (each an explicit, auditable escape hatch):
#   - A line `Terseness-Exempt: <reason>` with a real reason. Mirrors the
#     baseline's "block > 500 WITHOUT override" language; exemptions stay
#     visible in the description.
#
# All thresholds are env-overridable so they can be tuned as the baseline
# sample grows (R5 U2 — targets are directional until the sample is large):
#   TERSENESS_MAX_DIFF_LINES        (default 500)  hard block on diff size
#   TERSENESS_WARN_DIFF_LINES       (default 200)  soft warn on diff size
#   TERSENESS_MAX_VERBOSITY_RATIO   (default 0.5)  hard block on ratio
#   TERSENESS_MIN_PROSE_CHARS       (default 1200) prose floor for the ratio
#
# Usage:
#   ./scripts/lint-terseness-mr.sh <description-file>   # lint a file
#   ./scripts/lint-terseness-mr.sh -                    # lint stdin
#   ./scripts/lint-terseness-mr.sh                      # lint the GitLab MR
#                                                         from CI env
#
# Description source: explicit file arg, '-' for stdin, or
# CI_MERGE_REQUEST_DESCRIPTION (GitLab CI).
# Diff source (resolved only for trickle MRs, after the opt-in check):
#   1. TERSENESS_DIFF_FILE — a unified-diff file (used by the unit test).
#   2. In CI — computed from git against CI_MERGE_REQUEST_DIFF_BASE_SHA.
# If neither is available for a trickle MR, the gate warns and passes rather
# than redding the MR on an infra gap (mirrors lint-mr-proof's truncation
# off-ramp).
#
# Companion template: .gitlab/merge_request_templates/trickle.md
# Companion lint:     scripts/lint-trickle-mr.sh (provenance completeness, #8)
# Companion test:     scripts/lint-terseness-mr_test.sh
# Task: ka-0000007571. Source: R5 bundle ka-0000008221 §6 task5.

set -euo pipefail

PROG="$(basename "$0")"

# ── Thresholds (env-overridable; defaults wired from the R5 baseline) ──
MAX_DIFF_LINES="${TERSENESS_MAX_DIFF_LINES:-500}"
WARN_DIFF_LINES="${TERSENESS_WARN_DIFF_LINES:-200}"
MAX_VERBOSITY_RATIO="${TERSENESS_MAX_VERBOSITY_RATIO:-0.5}"
MIN_PROSE_CHARS="${TERSENESS_MIN_PROSE_CHARS:-1200}"

err() { echo "${PROG}: $*" >&2; }

# ── Description source ────────────────────────────────────────────────
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

# ── Opt-in: only trickle MRs are gated ────────────────────────────────
# Only the real HTML-comment marker counts (a prose mention of the token must
# not opt an MR in), matching lint-trickle-mr.sh.
MARKER_BEGIN_RE='<!--[[:space:]]*PROVENANCE:BEGIN[[:space:]]*-->'
MARKER_END_RE='<!--[[:space:]]*PROVENANCE:END[[:space:]]*-->'
has_markers=0
if grep -qE "$MARKER_BEGIN_RE" <<<"$desc" && grep -qE "$MARKER_END_RE" <<<"$desc"; then
  has_markers=1
fi
if (( ! has_markers )) && [[ "${TRICKLE_MR:-}" != "1" ]]; then
  # Not a trickle MR — verbose-in-lab is fine; nothing to gate.
  exit 0
fi

# ── Off-ramp: audited exemption ───────────────────────────────────────
exempt_line="$(grep -iE '^[[:space:]]*Terseness-Exempt:' <<<"$desc" | head -n1 || true)"
if [[ -n "$exempt_line" ]]; then
  reason="${exempt_line#*:}"
  reason="$(sed -E 's/^[[:space:]]+//; s/[[:space:]]+$//' <<<"$reason")"
  if [[ -z "$reason" || "$reason" == *"<reason>"* ]]; then
    err "Terseness-Exempt line present but has no reason. Give a real reason or trim the MR."
    exit 1
  fi
  echo "${PROG}: terseness-exempt MR (reason: ${reason})."
  exit 0
fi

# ── Prose char count (metric #7 numerator, provenance excluded) ───────
# Strip the structured PROVENANCE block, then every remaining HTML comment
# (template scaffolding + the machine-readable Security-Scan receipt). What
# remains is the author's human-facing prose. Collapse whitespace runs so the
# count is neither gameable by padding nor by minifying.
prose_chars_of() {
  local text="$1"
  # 1. Drop the PROVENANCE block (inclusive of its markers). Done first, and
  #    by its marker LINES, so the block's inner bullets are removed before the
  #    general HTML-comment strip below would only remove the marker comments.
  text="$(awk '
    /<!--[[:space:]]*PROVENANCE:BEGIN[[:space:]]*-->/ { skip=1; next }
    /<!--[[:space:]]*PROVENANCE:END[[:space:]]*-->/   { skip=0; next }
    !skip                                             { print }
  ' <<<"$text")"
  # 2. Drop every remaining HTML comment (template scaffolding + the
  #    machine-readable Security-Scan receipt), including comments that span
  #    multiple lines and those inline within a line. State machine so a
  #    <!-- ... --> that opens on one line and closes on another is handled.
  text="$(awk '
    { line=$0; out="" }
    {
      while (length(line) > 0) {
        if (incomment) {
          pos = index(line, "-->")
          if (pos == 0) { line="" }
          else { line = substr(line, pos+3); incomment=0 }
        } else {
          pos = index(line, "<!--")
          if (pos == 0) { out = out line; line="" }
          else { out = out substr(line, 1, pos-1); line = substr(line, pos+4); incomment=1 }
        }
      }
      print out
    }
  ' <<<"$text")"
  # 3. Collapse all whitespace to single spaces, trim, and count characters.
  text="$(tr '\n\t' '  ' <<<"$text" | sed -E 's/[[:space:]]+/ /g; s/^ //; s/ $//')"
  printf '%s' "${#text}"
}

prose_chars="$(prose_chars_of "$desc")"

# ── Diff source + parse (metrics #6 lines and #7 diff_chars) ──────────
# Resolve a unified diff for a trickle MR: explicit file (tests) or git in CI.
read_diff() {
  if [[ -n "${TERSENESS_DIFF_FILE:-}" ]]; then
    [[ -f "$TERSENESS_DIFF_FILE" ]] || { err "TERSENESS_DIFF_FILE not found: $TERSENESS_DIFF_FILE"; exit 2; }
    cat "$TERSENESS_DIFF_FILE"
    return 0
  fi
  local base="${CI_MERGE_REQUEST_DIFF_BASE_SHA:-}"
  local head="${CI_COMMIT_SHA:-HEAD}"
  if [[ -n "$base" ]]; then
    # Ensure the base commit is present under a shallow clone; best-effort.
    if ! git cat-file -e "${base}^{commit}" 2>/dev/null; then
      git fetch -q --depth=200 origin "$base" 2>/dev/null || true
    fi
    if git cat-file -e "${base}^{commit}" 2>/dev/null; then
      git diff --no-color "$base" "$head" 2>/dev/null && return 0
    fi
  fi
  return 1
}

if ! diff_text="$(read_diff)"; then
  echo "${PROG}: WARNING: could not compute the MR diff (no TERSENESS_DIFF_FILE and no reachable CI_MERGE_REQUEST_DIFF_BASE_SHA) — skipping diff-size/ratio checks."
  exit 0
fi

# Count added/removed LINES (#6) and their content CHARS (#7 denominator) from
# the unified diff. Content lines start with a single +/- ; file headers
# (+++/---) and hunk headers (@@) are excluded. The leading +/- is not counted
# toward chars (it is diff syntax, not code substance).
read -r diff_lines diff_chars < <(awk '
  /^\+\+\+/ || /^---/ { next }             # file headers
  /^[+-]/ {
    lines++
    chars += length($0) - 1               # drop the leading +/-
  }
  END { printf "%d %d\n", lines+0, chars+0 }
' <<<"$diff_text")

# ── Compute the verbosity ratio (#7): prose / (1 + diff_chars) ────────
# awk for the float division; formatted to 3 dp for the message.
ratio="$(awk -v p="$prose_chars" -v d="$diff_chars" 'BEGIN { printf "%.3f", p / (1 + d) }')"

echo "${PROG}: trickle MR — diff ${diff_lines} lines (${diff_chars} code chars), prose ${prose_chars} chars, verbosity ratio ${ratio}."

fail=0

# ── Enforce #6: diff size ─────────────────────────────────────────────
if (( diff_lines > MAX_DIFF_LINES )); then
  err "diff too large: ${diff_lines} lines > hard limit ${MAX_DIFF_LINES} (R5 metric #6)."
  err "  A trickle MR is a tiny, single-purpose slice (baseline target p90 ≤ 200 LOC)."
  err "  Split the peel into smaller slices, or add 'Terseness-Exempt: <reason>' if this"
  err "  slice is genuinely atomic and cannot be split."
  fail=1
elif (( diff_lines > WARN_DIFF_LINES )); then
  echo "${PROG}: WARNING: diff is ${diff_lines} lines (> ${WARN_DIFF_LINES}); target is p90 ≤ 200 LOC — consider a smaller slice."
fi

# ── Enforce #7: verbosity ratio (only above the prose floor) ──────────
# The ratio only bites when the prose is itself large — a rationale WALL — so
# terse small-diff MRs (naturally high ratio, tiny prose) are never penalized.
if (( prose_chars >= MIN_PROSE_CHARS )); then
  over="$(awk -v r="$ratio" -v m="$MAX_VERBOSITY_RATIO" 'BEGIN { print (r > m) ? 1 : 0 }')"
  if [[ "$over" == "1" ]]; then
    err "verbosity ratio ${ratio} > ${MAX_VERBOSITY_RATIO} with ${prose_chars} chars of prose (R5 metric #7)."
    err "  The description is a rationale wall relative to the diff. Trim it to a one-paragraph"
    err "  summary — move trust-earning detail into the structured PROVENANCE block (it is"
    err "  excluded from this count) and drop defensive re-derivation of correctness."
    fail=1
  fi
else
  # Prose under the floor: the ratio is not enforced (terse by construction).
  if awk -v r="$ratio" -v m="$MAX_VERBOSITY_RATIO" 'BEGIN { exit (r > m) ? 0 : 1 }'; then
    echo "${PROG}: verbosity ratio ${ratio} exceeds ${MAX_VERBOSITY_RATIO} but prose is only ${prose_chars} chars (< ${MIN_PROSE_CHARS} floor) — terse, not a wall; OK."
  fi
fi

if (( fail )); then
  err "terseness gate failed (R5 §6 task5 / ka-0000007571). Verbose-in-lab, terse-on-delivery."
  exit 1
fi

echo "${PROG}: terseness gate passed."
