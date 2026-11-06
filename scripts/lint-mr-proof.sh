#!/usr/bin/env bash
# lint-mr-proof.sh
#
# Enforces the gardenio MR proof-packet norm (W2, ka-0000004403): every MR
# description must carry a structured PROOF block with claim-matched
# evidence, per gardenio/docs/mr-proof-packet.md. Green CI proves the change
# compiles and its internal assertions hold; the PROOF block is where the
# author shows evidence for the USER-VISIBLE claim.
#
# An MR description must contain, between the markers
#   <!-- PROOF:BEGIN --> ... <!-- PROOF:END -->
# a bullet line for each required field, each with a non-placeholder value:
#   - Claim            (the user/operator-visible behavior delivered)
#   - Surface          (one of the SURFACES enum below)
#   - Evidence         (links/output exercising the claim itself)
#   - Not established  (what the evidence does NOT prove)
#
# Each field's value is the WHOLE bullet — its first line plus every
# continuation line (blank, indented, nested sub-bullet, or fenced block) up
# to the next `- **<Field>:**` bullet. Evidence presented below the first line
# therefore counts (ka-0000006457).
#
# Claim/evidence match (mechanical part): the Surface field routes a
# per-surface evidence heuristic — a viewer-ui claim needs a screenshot/image
# link, a pipeline-ci claim needs a run/job link. The *semantic* judgment
# (does the evidence really exercise the claim?) belongs to the reviewer;
# see the reviewer checklist in gardenio/docs/mr-proof-packet.md.
#
# Skips (each one is an explicit, auditable off-ramp):
#   - Draft MRs (title starts with "Draft:"/"WIP:") — not yet claiming anything.
#   - Trickle MRs (real <!-- PROVENANCE:BEGIN --> markers) — they carry their
#     own receipt, enforced by scripts/lint-trickle-mr.sh.
#   - A line `Proof-Exempt: <reason>` — rare escape hatch; exemptions are
#     visible in the description and counted by the KPI pass.
#
# The description is read from the GitLab API when the job has an MR context
# (CI_MERGE_REQUEST_IID + a token + curl/jq), because GitLab truncates the
# CI_MERGE_REQUEST_DESCRIPTION variable at ~2700 chars. If that read fails and
# the snapshot IS truncated, the lint validates the visible part of the block
# instead of skipping: required-field presence still fails the job, while the
# shape heuristics a mid-value cut could false-fail soften to warnings
# (ka-0000001603 — the old skip made the gate silently unenforced).
#
# Usage:
#   ./scripts/lint-mr-proof.sh <description-file>   # lint a file
#   ./scripts/lint-mr-proof.sh -                    # lint stdin
#   ./scripts/lint-mr-proof.sh                      # lint the GitLab MR
#                                                     description from CI env
#
# In GitLab CI the description is CI_MERGE_REQUEST_DESCRIPTION and the title
# is CI_MERGE_REQUEST_TITLE. When run with no args and the description var is
# set, the script lints it.
#
# Companion template: .gitlab/merge_request_templates/Default.md
# Companion test:     scripts/lint-mr-proof_test.sh
# Norm + KPI:         gardenio/docs/mr-proof-packet.md
# Task: ka-0000004403 (plan ka-0000001183, W2).

set -euo pipefail

PROG="$(basename "$0")"
SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
# The companion template ships the canonical placeholder text for each field.
# We read it at runtime (rather than hardcoding) so the guard tracks whatever
# the template actually ships and never drifts from it.
TEMPLATE_PATH="${SCRIPT_DIR}/../.gitlab/merge_request_templates/Default.md"

# Required proof fields. Each entry is "label|regex" where the regex matches
# the key portion of the bullet line (case-insensitive, after the leading
# "- **"). Keep these in sync with the Default MR template.
REQUIRED_FIELDS=(
  "Claim|claim"
  "Surface|surface"
  "Evidence|evidence"
  "Not established|not established"
)

# Valid Surface values. Keep in sync with the template and the norm doc.
SURFACES="viewer-ui pipeline-ci daemon-api agent-behavior chart-infra docs-tooling"

err() { echo "${PROG}: $*" >&2; }

# fetch_api_description: echo the AUTHORITATIVE, untruncated MR description
# from the GitLab API, or nothing (return 1) if it cannot be read.
#
# GitLab truncates the CI_MERGE_REQUEST_DESCRIPTION predefined variable at
# ~2700 chars, which used to leave this gate unenforceable on any long
# description (ka-0000001603). The API returns the whole thing, so truncation
# stops being a hole rather than being worked around. Same root cause and same
# fix as the monorepo's check-jira-ticket gate (ka-0000003640 / ka-0000003085).
fetch_api_description() {
  local url scheme cred val code out body attempts=""
  [[ -n "${CI_API_V4_URL:-}" && -n "${CI_PROJECT_ID:-}" && -n "${CI_MERGE_REQUEST_IID:-}" ]] || return 1
  command -v curl >/dev/null 2>&1 || return 1
  command -v jq >/dev/null 2>&1 || return 1
  url="${CI_API_V4_URL}/projects/${CI_PROJECT_ID}/merge_requests/${CI_MERGE_REQUEST_IID}"

  # Two auth schemes, in the order an MR pipeline actually holds them:
  #   GITLAB_CLONE_TOKEN — project variable with api scope; the per-MR pipeline
  #     holds it (see the .gitlab-ci.yml header) and it reads MRs reliably.
  #   CI_JOB_TOKEN — only works where the MR-read endpoint is job-token-allowed;
  #     free to try as a fallback, and harmless when it 401s.
  #
  # Each attempt's HTTP code is recorded and reported on failure. A silent
  # fallback to the truncated snapshot is undiagnosable from the job log — you
  # cannot tell "no token in this pipeline" from "token lacks api scope" from
  # "endpoint rejects JOB-TOKEN" without re-running blind. The codes say which.
  body="$(mktemp)"
  for spec in "PRIVATE-TOKEN:GITLAB_CLONE_TOKEN" "JOB-TOKEN:CI_JOB_TOKEN"; do
    scheme="${spec%%:*}"; cred="${spec##*:}"
    val="${!cred:-}"
    [[ -n "$val" ]] || continue   # no token for this scheme
    code="$(curl -sS -o "$body" -w '%{http_code}' --max-time 20 --retry 2 \
              --header "${scheme}: ${val}" "$url" 2>/dev/null || echo "000")"
    # `--retry` writes the -w output once PER ATTEMPT, so a retried request
    # yields e.g. "500500200"; the LAST three chars are the final code.
    code="${code: -3}"
    attempts+="${attempts:+, }${cred}→HTTP ${code}"
    if [[ "$code" == "200" ]]; then
      # .description is a string, so jq's `//` alternative is safe here (it is a
      # footgun only on a boolean, where `false // x` yields x — ka-0000006874).
      out="$(jq -r '.description // empty' <"$body" 2>/dev/null || true)"
      if [[ -n "$out" ]]; then
        rm -f "$body"
        printf '%s' "$out"
        return 0
      fi
      attempts+=" (empty description)"
    fi
  done
  rm -f "$body"
  err "MR API description read failed: ${attempts:-no credential set (neither GITLAB_CLONE_TOKEN nor CI_JOB_TOKEN)}."
  return 1
}

# resolve_description: set the globals `desc`, `DESC_SOURCE` and
# `DESC_TRUNCATED` from an explicit file arg, stdin ("-"), the API, or the
# CI_MERGE_REQUEST_DESCRIPTION env var (in that order of preference).
#
# DESC_TRUNCATED is "no" whenever we hold the full text; it is "yes" only when
# we are reading a snapshot GitLab flagged as cut off. Every downstream check
# that has to soften does so on THAT flag, never on the raw env var.
desc=""
DESC_SOURCE=""
DESC_TRUNCATED="no"

resolve_description() {
  # The env flag is how CI reports a cut-off snapshot; the test suite uses it
  # the same way alongside a file argument.
  if [[ "${CI_MERGE_REQUEST_DESCRIPTION_IS_TRUNCATED:-}" == "true" ]]; then
    DESC_TRUNCATED="yes"
  fi

  if (( $# >= 1 )); then
    if [[ "$1" == "-" ]]; then
      desc="$(cat)"
      DESC_SOURCE="stdin"
    else
      [[ -f "$1" ]] || { err "no such file: $1"; exit 2; }
      desc="$(cat "$1")"
      DESC_SOURCE="file ${1}"
    fi
    return
  fi

  local api
  if api="$(fetch_api_description)" && [[ -n "$api" ]]; then
    desc="$api"
    DESC_SOURCE="GitLab API (untruncated)"
    DESC_TRUNCATED="no"
    echo "${PROG}: read the full description from the MR API (${#desc} chars) — truncation cannot hide the packet."
    return
  fi

  if [[ -n "${CI_MERGE_REQUEST_DESCRIPTION:-}" ]]; then
    desc="$CI_MERGE_REQUEST_DESCRIPTION"
    DESC_SOURCE="CI_MERGE_REQUEST_DESCRIPTION variable"
    if [[ "$DESC_TRUNCATED" == "yes" ]]; then
      echo "${PROG}: WARNING: could not read the MR description from the API; falling back to the TRUNCATED CI snapshot."
    fi
    return
  fi

  err "no description provided (pass a file, '-' for stdin, or set CI_MERGE_REQUEST_DESCRIPTION)"
  exit 2
}

resolve_description "$@"

# ── Off-ramps ────────────────────────────────────────────────────────

# Draft MRs are not yet claiming anything user-visible.
title="${CI_MERGE_REQUEST_TITLE:-}"
if [[ "$title" =~ ^([Dd]raft|WIP): ]]; then
  echo "${PROG}: draft MR — proof packet not required yet (required before marking ready)."
  exit 0
fi

# Trickle MRs carry their own structured receipt (PROVENANCE block), enforced
# by scripts/lint-trickle-mr.sh. Only the real HTML-comment markers count, so
# a prose mention of the token does not trigger the deferral.
if grep -qE '<!--[[:space:]]*PROVENANCE:BEGIN[[:space:]]*-->' <<<"$desc"; then
  echo "${PROG}: trickle MR (PROVENANCE block) — deferring to lint-trickle-mr.sh."
  exit 0
fi

# Explicit exemption: `Proof-Exempt: <reason>` on its own line, with a real
# reason. Exemptions stay visible in the description for the KPI pass.
exempt_line="$(grep -iE '^[[:space:]]*Proof-Exempt:' <<<"$desc" | head -n1 || true)"
if [[ -n "$exempt_line" ]]; then
  reason="${exempt_line#*:}"
  reason="$(sed -E 's/^[[:space:]]+//; s/[[:space:]]+$//' <<<"$reason")"
  if [[ -z "$reason" || "$reason" == *"<reason>"* ]]; then
    err "Proof-Exempt line present but has no reason. Give a real reason or add a PROOF block."
    exit 1
  fi
  echo "${PROG}: proof-exempt MR (reason: ${reason})."
  exit 0
fi

# ── PROOF block ──────────────────────────────────────────────────────

MARKER_BEGIN_RE='<!--[[:space:]]*PROOF:BEGIN[[:space:]]*-->'
MARKER_END_RE='<!--[[:space:]]*PROOF:END[[:space:]]*-->'

# `cmd && flag=1` would abort the script under `set -e` when cmd fails, so use
# explicit ifs.
has_begin=0; has_end=0
if grep -qE "$MARKER_BEGIN_RE" <<<"$desc"; then has_begin=1; fi
if grep -qE "$MARKER_END_RE" <<<"$desc"; then has_end=1; fi

# PARTIAL VALIDATION. When the API read failed and GitLab handed us a snapshot
# whose PROOF:END fell past the cut, we hold the START of a real block. This
# used to warn-and-exit-0, which made the gate silently unenforced on exactly
# the long descriptions most likely to need it: a packet missing a required
# field passed green twice on !8121 (ka-0000001603). Instead, validate what IS
# visible — required-field PRESENCE is checked for real (a field that is absent
# here is either missing or below the cut, and both are fixed the same way: move
# the block up), while the shape heuristics that a mid-value cut could
# false-fail (Surface enum, per-surface evidence) soften to warnings.
partial=0
if (( has_begin )) && (( ! has_end )) && [[ "$DESC_TRUNCATED" == "yes" ]]; then
  partial=1
  echo "${PROG}: WARNING: the CI description snapshot is truncated and PROOF:END fell past the cut."
  echo "${PROG}:          Validating the VISIBLE part of the block: a required field that is not"
  echo "${PROG}:          visible here FAILS this job. Move the PROOF block near the TOP of the"
  echo "${PROG}:          description to have the whole packet validated."
fi

if (( ! partial )) && { (( ! has_begin )) || (( ! has_end )); }; then
  err "no PROOF block found (gardenio MR proof-packet norm, ka-0000004403)."
  # A truncated CI snapshot is the most common reason a block that IS in the
  # description (and passes this script locally) is invisible here. Name it
  # first so the author does not re-verify a correct block and re-trigger CI
  # to find out (ka-0000004000).
  if [[ "$DESC_TRUNCATED" == "yes" ]]; then
    err "LIKELY CAUSE: GitLab truncated this description at ~2700 chars for CI,"
    err "  so a PROOF block further down is NOT in the snapshot this job sees."
    err "  Move the PROOF block near the TOP of the description (right after the"
    err "  one-paragraph Summary, as in the template) so it survives the cut."
  fi
  err "Every gardenio MR must include a proof block delimited by"
  err "  <!-- PROOF:BEGIN --> ... <!-- PROOF:END -->"
  err "with Claim / Surface / Evidence / Not established fields."
  err "Use .gitlab/merge_request_templates/Default.md, or add"
  err "'Proof-Exempt: <reason>' if a proof packet genuinely does not apply."
  err "Norm: gardenio/docs/mr-proof-packet.md"
  exit 1
fi

# Extract the content strictly between the real HTML-comment markers.
block="$(awk '
  /<!--[[:space:]]*PROOF:BEGIN[[:space:]]*-->/ { inblk=1; next }
  /<!--[[:space:]]*PROOF:END[[:space:]]*-->/   { inblk=0 }
  inblk                                        { print }
' <<<"$desc")"

# Strip HTML comments ( <!-- ... --> ) line-by-line, then trim each line.
# Safe on multi-line input: sed applies the substitutions per line, so a
# bullet's continuation lines keep their line structure.
strip_comments_and_trim() {
  sed -E 's/<!--.*-->//g; s/^[[:space:]]+//; s/[[:space:]]+$//' <<<"$1"
}

# field_lines <keyre>: echo the RAW lines of the whole bullet for field
# <keyre> — the first matching "- **<Key>:**" line plus every following line
# that markdown would render as part of that bullet.
#
# A following line belongs to the bullet unless it is a sibling list item at
# the same-or-shallower indent, or a heading at the same-or-shallower indent.
# Blank lines, more-indented lines (nested bullets, fenced blocks, quoted
# output) and lazy continuations all belong. Lines inside a fenced code block
# always belong, so a fence containing "- " or "# " cannot end the value.
#
# This is what the surface heuristics must grep: an Evidence bullet that opens
# with a sentence of framing and puts its run links and before/after output on
# the continuation lines is still evidence (ka-0000006457).
field_lines() {
  local keyre="$1"
  awk -v keyre="$keyre" '
    function indent_of(s,   t) {
      t = s; sub(/[^ \t].*$/, "", t); gsub(/\t/, "    ", t); return length(t)
    }
    !started {
      if (tolower($0) ~ ("^[ \t]*-[ \t]*\\*\\*[ \t]*" keyre)) {
        started = 1; base = indent_of($0); print
      }
      next
    }
    /^[ \t]*(```|~~~)/ { fence = !fence; print; next }
    fence              { print; next }
    /^[ \t]*$/         { print; next }
    {
      if (indent_of($0) <= base &&
          ($0 ~ /^[ \t]*([-*+][ \t]|[0-9]+[.)][ \t])/ || $0 ~ /^[ \t]*#/)) exit
      print
    }
  ' <<<"$block"
}

# bullet_value <line>: echo the text after the bolded key on a field line.
bullet_value() {
  local line="$1" value
  value="${line#*:\*\*}"
  if [[ "$value" == "$line" ]]; then
    value="${line#*\*\*}"; value="${value#*\*\*}"
  fi
  printf '%s' "$value"
}

# field_value <keyre>: echo the trimmed FIRST-LINE value of the field, or
# nothing. Used where the value is inherently single-line (the Surface enum).
field_value() {
  local keyre="$1" line
  line="$(grep -iE "^[[:space:]]*-[[:space:]]*\*\*[[:space:]]*${keyre}" <<<"$block" | head -n1 || true)"
  [[ -n "$line" ]] || return 0
  strip_comments_and_trim "$(bullet_value "$line")"
}

# field_body <keyre>: echo the trimmed value of the WHOLE bullet — the
# first-line value plus every continuation line — or nothing. Used for
# presence/placeholder detection and for the per-surface evidence heuristics,
# both of which must see evidence that lives below the first line.
field_body() {
  local keyre="$1" lines first rest
  lines="$(field_lines "$keyre")"
  [[ -n "$lines" ]] || return 0
  first="$(head -n1 <<<"$lines")"
  rest="$(tail -n +2 <<<"$lines")"
  # Command substitution strips trailing newlines, so a bullet whose only
  # content is blank continuation lines still reads as an empty value.
  strip_comments_and_trim "$(printf '%s\n%s' "$(bullet_value "$first")" "$rest")"
}

# template_placeholder <keyre>: echo the literal placeholder sentence the
# template ships inside the HTML comment for the field <keyre>, or nothing if
# the template is unavailable / the field is not found. This is the exact
# unfilled-field text — e.g. for Claim it is
#   "the user/operator-visible behavior this MR delivers, in one sentence".
# Matching the WHOLE placeholder sentence (not a lone substring like "e.g.")
# is what distinguishes a genuinely untouched field from substantive prose
# that merely contains a common token.
template_placeholder() {
  local keyre="$1" line
  [[ -f "$TEMPLATE_PATH" ]] || return 0
  line="$(grep -iE "^[[:space:]]*-[[:space:]]*\*\*[[:space:]]*${keyre}" "$TEMPLATE_PATH" | head -n1 || true)"
  [[ -n "$line" ]] || return 0
  # Pull the text between the <!-- and --> markers and trim it.
  sed -nE 's/.*<!--[[:space:]]*(.*[^[:space:]])[[:space:]]*-->.*/\1/p' <<<"$line"
}

# is_placeholder_value <keyre> <value>: return 0 (true) if <value> is an
# unfilled / template-shaped stub for field <keyre>, echoing a short reason
# naming the rule that matched so the author can act without reading this
# script. Detects by SHAPE — an untouched or stub value — never by the mere
# presence of a common substring ("e.g.", "one of:") inside otherwise
# substantive prose (ka-0000000685).
is_placeholder_value() {
  local keyre="$1" value="$2" ph
  case "$value" in
    "" )
      echo "no value"; return 0 ;;
    *"<!--"* )
      # An unterminated / left-in HTML comment marker — the field was not
      # filled in past the template scaffold.
      echo "unfilled template comment left in value"; return 0 ;;
    "_"*|"…"|"..." )
      echo "stub value ('${value}')"; return 0 ;;
  esac
  # The field still carries (or is dominated by) the template's own
  # placeholder sentence. Match the full sentence, so real prose that only
  # happens to include a token like "e.g." is never flagged.
  ph="$(template_placeholder "$keyre")"
  if [[ -n "$ph" && "$value" == *"$ph"* ]]; then
    echo "still contains the template placeholder text"; return 0
  fi
  return 1
}

missing=()
for entry in "${REQUIRED_FIELDS[@]}"; do
  label="${entry%%|*}"
  keyre="${entry##*|}"

  line="$(grep -iE "^[[:space:]]*-[[:space:]]*\*\*[[:space:]]*${keyre}" <<<"$block" | head -n1 || true)"
  if [[ -z "$line" ]]; then
    missing+=("$label (field line absent)")
    continue
  fi

  value="$(field_body "$keyre")"

  # Reject empty or template-shaped placeholder values. is_placeholder_value
  # detects an UNFILLED field by shape (empty, stub, or still carrying the
  # template's own placeholder sentence) and echoes the rule that matched — it
  # does NOT flag substantive prose that merely contains a common token such as
  # "e.g." or "one of:" (ka-0000000685).
  if reason="$(is_placeholder_value "$keyre" "$value")"; then
    missing+=("$label (${reason})")
  fi
done

if (( ${#missing[@]} > 0 )); then
  err "PROOF block is incomplete (norm: gardenio/docs/mr-proof-packet.md):"
  for m in "${missing[@]}"; do
    err "  - missing or placeholder: ${m}"
  done
  if (( partial )); then
    err "This is a TRUNCATED snapshot: a field that exists further down the real"
    err "description is invisible to this job, so it counts as missing. Move the"
    err "PROOF block near the TOP of the description and re-run."
  fi
  err "Fill in every field in the PROOF block (template: .gitlab/merge_request_templates/Default.md)."
  exit 1
fi

# ── Surface enum + claim-matched evidence heuristics ─────────────────

# soft_fail: report a SHAPE check that a mid-value truncation could false-fail
# (the Surface enum, the per-surface evidence heuristics). A hard error
# normally; under partial validation it degrades to a warning, because the value
# we are judging may itself be cut in half. Field PRESENCE is never softened.
soft_fail() {
  local line
  if (( partial )); then
    for line in "$@"; do
      echo "${PROG}: WARNING (not validated — truncated snapshot): ${line}"
    done
    return 0
  fi
  for line in "$@"; do err "$line"; done
  exit 1
}

# Surface is an enum on a single line; Evidence is the whole bullet, so links
# and output on continuation lines count (ka-0000006457).
surface="$(field_value 'surface' | tr '[:upper:]' '[:lower:]')"
evidence="$(field_body 'evidence')"

surface_ok=0
for s in $SURFACES; do
  [[ "$surface" == "$s" ]] && surface_ok=1
done
if (( ! surface_ok )); then
  soft_fail "Surface '${surface}' is not a recognized value." \
            "Use one of: ${SURFACES// / | }"
fi

# Mechanical claim/evidence match: the two surfaces whose evidence has an
# unambiguous shape are checked here; the rest are the reviewer's judgment.
case "$surface" in
  viewer-ui)
    # A UI claim needs visual evidence: an uploaded image (gb image upload
    # proxied /image/ URL), a markdown image, or an image/video file link.
    if ! grep -qiE '(/image/|!\[|\.(png|jpe?g|gif|webp|webm|mp4)\b|screenshot)' <<<"$evidence"; then
      soft_fail "claim/evidence mismatch: Surface is viewer-ui but Evidence has no" \
                "screenshot or image link. Capture the change and upload it with" \
                "'gb image upload <png>' — unit tests alone do not show a UI claim."
    fi
    ;;
  pipeline-ci)
    # A CI/pipeline claim needs a link to a run that exercised it.
    if ! grep -qiE 'https?://[^[:space:]]*(pipeline|job|/runs?/)' <<<"$evidence"; then
      soft_fail "claim/evidence mismatch: Surface is pipeline-ci but Evidence has no" \
                "pipeline/job/run link. Link the run that exercised the change."
    fi
    ;;
esac

if (( partial )); then
  echo "${PROG}: proof packet fields all present in the VISIBLE part of a truncated snapshot (surface: ${surface})."
  echo "${PROG}: PARTIAL validation — move the PROOF block near the top to have the whole packet checked."
else
  echo "${PROG}: proof packet complete (surface: ${surface}) [description source: ${DESC_SOURCE}]."
fi
