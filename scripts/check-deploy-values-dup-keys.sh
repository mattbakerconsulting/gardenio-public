#!/usr/bin/env bash
# check-deploy-values-dup-keys.sh
#
# LOUD duplicate-YAML-key guard for the helm deploy overlays under
# deploy-values/. A duplicate mapping key in a YAML file is silently
# resolved "last key wins" by virtually every parser (helm, yq, PyYAML),
# so a bad "keep both" merge resolution can neutralize a security control
# WITHOUT any tool complaining.
#
# This exact footgun nearly defeated the C7 grafana-llm-app toolset
# allow-list: a CA (certificate-authority) key collision in an overlay
# silently last-key-won and almost dropped the explicit-deny toolset block
# (C7 QC, ka-0000007430 item 6). This guard makes that class of mistake
# fail the build LOUDLY instead of shipping a half-applied overlay.
#
# It runs `yamllint` with ONLY the key-duplicates rule enabled (mikefarah
# `yq` does NOT detect duplicate keys — it last-key-wins silently, verified
# 2026-06-28 — so it is unsuitable here). yamllint's key-duplicates rule
# exits non-zero on the first duplicate mapping key.
#
# Usage:
#   ./scripts/check-deploy-values-dup-keys.sh                 # lint deploy-values/*.yaml
#   ./scripts/check-deploy-values-dup-keys.sh <file> [file..] # lint specific files
#
# Exit status: 0 if all files are duplicate-key-free, 1 if any file has a
# duplicate mapping key (or if yamllint is unavailable).
#
# Companion test: scripts/check-deploy-values-dup-keys_test.sh
# Task: ka-0000007430 (C7-followup hardening). Parent epic: ka-0000002011.

set -euo pipefail

# yamllint config: enable ONLY key-duplicates. We deliberately do not enable
# the cosmetic rules (line-length, indentation, comments) — this guard is a
# correctness gate, not a style gate, and the overlays are hand-tuned.
YAMLLINT_CONFIG='{extends: default, rules: {
  key-duplicates: enable,
  line-length: disable,
  indentation: disable,
  comments: disable,
  comments-indentation: disable,
  document-start: disable,
  trailing-spaces: disable,
  empty-lines: disable,
  brackets: disable,
  braces: disable,
  truthy: disable,
  octal-values: disable,
  new-line-at-end-of-file: disable,
  hyphens: disable,
  empty-values: disable,
  commas: disable,
  colons: disable,
  float-values: disable,
  quoted-strings: disable,
  key-ordering: disable
}}'

# Resolve `yamllint` — prefer the binary on PATH (how CI installs it: apt-get
# install yamllint), fall back to `python3 -m yamllint` for pip-based installs.
yamllint_run() {
  if command -v yamllint >/dev/null 2>&1; then
    yamllint "$@"
  elif command -v python3 >/dev/null 2>&1 && python3 -m yamllint --version >/dev/null 2>&1; then
    python3 -m yamllint "$@"
  else
    echo "ERROR: yamllint not found. Install with: pip install yamllint" >&2
    return 127
  fi
}

# Determine the file list. With no args, lint every *.yaml in deploy-values/
# (NOT the *.yaml.tmpl ephemeral templates — they carry ${VAR} placeholders
# and envsubst markers that are not valid standalone YAML).
SCRIPT_DIR="$(cd "$(dirname "$0")" && pwd)"
REPO_ROOT="$(cd "${SCRIPT_DIR}/.." && pwd)"

declare -a FILES
if [[ "$#" -gt 0 ]]; then
  FILES=("$@")
else
  # Glob may legitimately match nothing if run outside the repo; guard it.
  shopt -s nullglob
  FILES=("${REPO_ROOT}"/deploy-values/*.yaml)
  shopt -u nullglob
  if [[ "${#FILES[@]}" -eq 0 ]]; then
    echo "ERROR: no deploy-values/*.yaml files found under ${REPO_ROOT}" >&2
    exit 1
  fi
fi

rc=0
for f in "${FILES[@]}"; do
  if [[ ! -f "$f" ]]; then
    echo "ERROR: not a file: $f" >&2
    rc=1
    continue
  fi
  # Distinguish three states explicitly:
  #   0   → clean, no dup keys
  #   127 → yamllint is missing/broken (yamllint_run emits ERROR; do NOT mask
  #         this as a dup-key finding — they have different remediations)
  #   *   → yamllint exited non-zero with a real dup-key finding
  out=""
  out="$(yamllint_run -d "$YAMLLINT_CONFIG" "$f" 2>&1)"; lint_rc=$?
  case "$lint_rc" in
    0)
      echo "OK    $f"
      ;;
    127)
      # yamllint_run already printed the install instruction to stderr.
      echo "TOOL-MISSING (yamllint unavailable, cannot lint): $f" >&2
      rc=127
      ;;
    *)
      echo "DUPLICATE-KEY in: $f" >&2
      echo "$out" >&2
      rc=1
      ;;
  esac
done

case "$rc" in
  0) ;;
  127)
    echo "" >&2
    echo "FAIL: yamllint not available — install with 'pip install yamllint'." >&2
    echo "(This is NOT a duplicate-key finding; it is a missing-tool failure.)" >&2
    echo "(ka-0000007430)" >&2
    ;;
  *)
    echo "" >&2
    echo "FAIL: duplicate YAML key(s) detected in deploy overlay(s) above." >&2
    echo "A duplicate mapping key silently last-key-wins and can neutralize a" >&2
    echo "security control (e.g. the C7 toolset allow-list). Resolve the merge" >&2
    echo "so each key appears once. (ka-0000007430)" >&2
    ;;
esac

exit "$rc"
