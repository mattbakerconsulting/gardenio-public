#!/usr/bin/env bash
# lint-deploy-values-deletion.sh
#
# Guards the recurring "stranded bead" mechanism (bug ka-0000002710): a change
# lands on a file under deploy-values/, that overlay is later DELETED as legacy,
# and every open bead whose remediation text names that file becomes silently
# unactionable — the bead still reads as a live, well-specified task while its
# named surface no longer exists. Nothing goes red. It has happened three times:
#
#   1. deploy-values/gardenio.yaml            deleted bdf67c368 (2026-07-08)
#      -> stranded ka-0000001165 (MR !4000 target file).
#   2. deploy-values/gardenio-rwx.yaml +
#      deploy-values/gardenio-rwx-ephemeral.yaml.tmpl  deleted d6f728709 (2026-08-15)
#      -> stranded ka-0000008192 (staging soak surface) AND ka-0000001165 again.
#
# THE GUARD (deliberately narrow — this must NOT become a noisy gate):
# This lint is a NO-OP on every MR that does not DELETE a file under
# deploy-values/. When (and only when) an MR deletes one, it requires the MR
# description to carry a structured DEPLOY-VALUES-DELETION receipt that
#   (a) names every deleted deploy-values path, and
#   (b) records the open-bead corpus sweep result (which beads it stranded, and
#       what was done about them).
# The receipt content is produced by the companion daemon-capable sweep:
#   scripts/sweep-deploy-values-refs.sh
# so the author cannot delete an overlay without first looking at what open
# beads it invalidates. That is option (a)+(c) of the bug's suggested fixes; it
# would have forced acknowledgement of all three historical occurrences.
#
# Blast radius: deleting a deploy-values file is a rare, deliberate act. This
# job runs on every MR but only ever HARD-fails on that specific event; an MR
# that touches no deploy-values deletion is untouched.
#
# Off-ramp (rare, audited): a line `Deploy-Values-Deletion-Exempt: <reason>`
# with a real reason. Same shape as lint-mr-proof's `Proof-Exempt:`.
#
# Fail-open on infra uncertainty: if the deleted-file set cannot be computed
# (no target branch, no merge-base, shallow clone) the guard WARNs and passes —
# a git blip must never redden a legitimate MR. The companion sweep and the
# reviewer are the backstop for that rare miss.
#
# Usage:
#   ./scripts/lint-deploy-values-deletion.sh                 # CI: reads env
#   ./scripts/lint-deploy-values-deletion.sh <desc-file>     # lint a desc file
#   ./scripts/lint-deploy-values-deletion.sh -               # desc from stdin
#
# In GitLab CI the description is CI_MERGE_REQUEST_DESCRIPTION and the deleted
# set is computed from the MR diff (target branch CI_MERGE_REQUEST_TARGET_BRANCH_NAME).
#
# Test hook (hermetic — bypasses git): set DEPLOY_VALUES_DELETED to a
# newline-separated list of deleted paths. An empty value means "no deletions".
#
# Companion sweep:    scripts/sweep-deploy-values-refs.sh
# Companion test:     scripts/lint-deploy-values-deletion_test.sh
# Bug: ka-0000002710.

set -uo pipefail

PROG="$(basename "$0")"

err() { echo "${PROG}: $*" >&2; }

DEPLOY_VALUES_DIR="deploy-values"

# Marker names for the receipt block.
MARKER_BEGIN_RE='<!--[[:space:]]*DEPLOY-VALUES-DELETION:BEGIN[[:space:]]*-->'
MARKER_END_RE='<!--[[:space:]]*DEPLOY-VALUES-DELETION:END[[:space:]]*-->'

# ── Read the MR description ───────────────────────────────────────────
# From: explicit file arg, stdin ("-"), or CI_MERGE_REQUEST_DESCRIPTION.
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
    # No description available. Only a problem if there ARE deletions; the
    # caller decides. Return empty.
    printf ''
  fi
}

# ── Compute the set of DELETED deploy-values files ────────────────────
# Honors the DEPLOY_VALUES_DELETED test hook first (hermetic). Otherwise
# computes from the MR diff against the target branch. Prints one path per
# line. Sets DELETION_UNKNOWN=1 (via global) when the set could not be
# determined from git (fail-open signal).
DELETION_UNKNOWN=0
deleted_deploy_values_files() {
  # Test hook / explicit override. `set` (even to empty) short-circuits git.
  if [[ "${DEPLOY_VALUES_DELETED+set}" == "set" ]]; then
    # Filter to deploy-values/ paths, drop blanks.
    printf '%s\n' "$DEPLOY_VALUES_DELETED" \
      | grep -E "^${DEPLOY_VALUES_DIR}/" 2>/dev/null || true
    return 0
  fi

  # Not in a git repo -> cannot compute -> fail open.
  if ! git rev-parse --git-dir >/dev/null 2>&1; then
    DELETION_UNKNOWN=1
    return 0
  fi

  local target base
  target="${CI_MERGE_REQUEST_TARGET_BRANCH_NAME:-}"
  if [[ -z "$target" ]]; then
    # Not an MR pipeline (push/tag build) — there is no MR diff to gate. Treat
    # as "no deletions to acknowledge": a push to main already merged; the gate
    # belongs on the MR, not the post-merge push.
    return 0
  fi

  git fetch --no-tags --depth=200 origin "$target" >/dev/null 2>&1 \
    || git fetch --no-tags origin "$target" >/dev/null 2>&1 || true
  base="$(git merge-base "origin/${target}" HEAD 2>/dev/null || echo '')"
  if [[ -z "$base" ]]; then
    DELETION_UNKNOWN=1
    return 0
  fi

  git diff --diff-filter=D --name-only "$base" HEAD -- "${DEPLOY_VALUES_DIR}/" \
    2>/dev/null || { DELETION_UNKNOWN=1; return 0; }
}

# ── Main ──────────────────────────────────────────────────────────────
mapfile -t DELETED < <(deleted_deploy_values_files)

if (( DELETION_UNKNOWN )); then
  echo "${PROG}: WARNING: could not compute the deleted-file set (no target branch / merge-base / shallow clone) — failing OPEN. The companion sweep (scripts/sweep-deploy-values-refs.sh) and the reviewer are the backstop."
  exit 0
fi

if (( ${#DELETED[@]} == 0 )); then
  echo "${PROG}: no deploy-values/ file deleted by this MR — nothing to check."
  exit 0
fi

echo "${PROG}: this MR deletes ${#DELETED[@]} file(s) under ${DEPLOY_VALUES_DIR}/:"
for f in "${DELETED[@]}"; do echo "  - ${f}"; done

desc="$(read_description "$@")"

# Off-ramp: explicit, audited exemption.
exempt_line="$(grep -iE '^[[:space:]]*Deploy-Values-Deletion-Exempt:' <<<"$desc" | head -n1 || true)"
if [[ -n "$exempt_line" ]]; then
  reason="${exempt_line#*:}"
  reason="$(sed -E 's/^[[:space:]]+//; s/[[:space:]]+$//' <<<"$reason")"
  if [[ -z "$reason" || "$reason" == *"<reason>"* ]]; then
    err "Deploy-Values-Deletion-Exempt line present but has no reason. Give a real reason or add the receipt block."
    exit 1
  fi
  echo "${PROG}: deletion exempt (reason: ${reason})."
  exit 0
fi

# Require the receipt block.
if ! grep -qE "$MARKER_BEGIN_RE" <<<"$desc" || ! grep -qE "$MARKER_END_RE" <<<"$desc"; then
  err "this MR deletes a deploy-values/ overlay but the description carries no"
  err "DEPLOY-VALUES-DELETION receipt (bug ka-0000002710)."
  err ""
  err "Deleting an overlay silently strands every OPEN bead whose remediation"
  err "text names that file. Before merging, run the corpus sweep:"
  err ""
  err "    scripts/sweep-deploy-values-refs.sh ${DELETED[*]}"
  err ""
  err "then paste its output into a receipt block in the MR description:"
  err ""
  err "    <!-- DEPLOY-VALUES-DELETION:BEGIN -->"
  err "    ## Deploy-values deletion receipt"
  err "    - **Deleted paths:** ${DELETED[*]}"
  err "    - **Stranded open beads:** <kd-ids from the sweep, or 'none'>"
  err "    - **Action taken:** <commented / re-homed / closed / none needed>"
  err "    <!-- DEPLOY-VALUES-DELETION:END -->"
  err ""
  err "Or, if a receipt genuinely does not apply, add a line:"
  err "    Deploy-Values-Deletion-Exempt: <reason>"
  exit 1
fi

# Extract the block strictly between the markers.
block="$(awk '
  /<!--[[:space:]]*DEPLOY-VALUES-DELETION:BEGIN[[:space:]]*-->/ { inblk=1; next }
  /<!--[[:space:]]*DEPLOY-VALUES-DELETION:END[[:space:]]*-->/   { inblk=0 }
  inblk                                                        { print }
' <<<"$desc")"

# field_value <keyre>: trimmed value of the "- **<key>:** value" bullet, or "".
field_value() {
  local keyre="$1" line value
  line="$(grep -iE "^[[:space:]]*-[[:space:]]*\*\*[[:space:]]*${keyre}" <<<"$block" | head -n1 || true)"
  [[ -n "$line" ]] || return 0
  value="${line#*:\*\*}"
  if [[ "$value" == "$line" ]]; then
    value="${line#*\*\*}"; value="${value#*\*\*}"
  fi
  sed -E 's/<!--.*-->//g' <<<"$value" | sed -E 's/^[[:space:]]+//; s/[[:space:]]+$//'
}

# 1. Every deleted path must be named in the "Deleted paths" field. This is
#    what forces the author to have looked at exactly what they are removing.
deleted_field="$(field_value 'deleted paths')"
if [[ -z "$deleted_field" ]]; then
  err "receipt block present but the 'Deleted paths' field is empty."
  err "List every deleted deploy-values path: ${DELETED[*]}"
  exit 1
fi
missing_paths=()
for f in "${DELETED[@]}"; do
  # Match on the basename too, so a bare filename in the receipt still counts.
  if ! grep -qF "$f" <<<"$deleted_field" && ! grep -qF "$(basename "$f")" <<<"$deleted_field"; then
    missing_paths+=("$f")
  fi
done
if (( ${#missing_paths[@]} > 0 )); then
  err "the receipt's 'Deleted paths' field does not name every file this MR deletes."
  err "Missing from the receipt:"
  for f in "${missing_paths[@]}"; do err "  - ${f}"; done
  err "The receipt must acknowledge each deletion so the corpus sweep covered it."
  exit 1
fi

# 2. The corpus-sweep result must be recorded (non-empty, non-placeholder).
stranded_field="$(field_value 'stranded open beads')"
if [[ -z "$stranded_field" || "$stranded_field" == *"<"*">"* ]]; then
  err "receipt block present but the 'Stranded open beads' field is empty or still a placeholder."
  err "Run scripts/sweep-deploy-values-refs.sh and record the open beads it found"
  err "(or the literal word 'none' if the sweep came back clean)."
  exit 1
fi

echo "${PROG}: deploy-values deletion receipt complete (deleted: ${#DELETED[@]}, stranded field: '${stranded_field}')."
exit 0
