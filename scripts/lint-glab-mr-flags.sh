#!/usr/bin/env bash
# lint-glab-mr-flags.sh
#
# Forbids the gh-style `glab mr list --state=<value>` invocation. glab is NOT
# gh: it has NO `--state` flag — `glab mr list --state=opened` errors
#
#     ERROR  Unknown flag: --state.
#
# The real state filters are bare `glab mr list` (open), `-M`/`--merged`,
# `-c`/`--closed`, `-A`/`--all`. The deprecated `--opened` still works but warns.
#
# This has recurred four times (ka-0000000464 → ka-0000005306 → ka-0000006493
# → ka-0000003906); each prior fix patched a single call site and left the rest
# to re-rot. This guard makes datapoint #5 fail CI instead of burning another
# audit cycle.
#
# Usage:
#   ./scripts/lint-glab-mr-flags.sh                 # scan the tracked tree (git grep)
#   ./scripts/lint-glab-mr-flags.sh path/to/file    # scan explicit file(s) (tests)
#
# Run from the repo root. Exits non-zero on the first violation.
#
# A line is EXEMPT when it is teaching against the pattern rather than invoking
# it — detected by the presence of `Unknown flag`, `deprecated`, or an explicit
# auditable inline suppression token `glab-lint-ok`. Companion unit test:
# scripts/lint-glab-mr-flags_test.sh.

set -euo pipefail

# The erroring form: `mr list` … a gh-style `--state` flag with a state value
# (either `--state=opened` or `--state opened`). Also catches a real
# `--opened` flag invocation. Kept deliberately narrow so it matches commands,
# not the word "state" in prose.
pattern='mr list[^`]*(--state[= ](opened|closed|merged|all)|--opened([[:space:]]|$))'

# Lines that discuss the flag to warn against it, or carry an explicit
# suppression, are not violations.
exempt_line='Unknown flag|deprecated|glab-lint-ok|NO `--state`|is NOT gh'

# Paths that legitimately contain the pattern as a frozen fixture / snapshot
# or as this guard's own source and tests.
exempt_path='scripts/lint-glab-mr-flags\.sh|scripts/lint-glab-mr-flags_test\.sh|gardenio/controller/cmd/gb/testdata/advice_catalog_snapshot\.json|gardenio/controller/internal/advice/lint_test\.go|gardenio/docs/audits/'

if (( $# > 0 )); then
  raw="$(grep -HnE "$pattern" "$@" 2>/dev/null || true)"
else
  # git grep scans only tracked files and respects the repo root.
  raw="$(git grep -nE "$pattern" -- \
        ':(exclude)scripts/lint-glab-mr-flags.sh' \
        ':(exclude)scripts/lint-glab-mr-flags_test.sh' \
        ':(exclude)gardenio/controller/cmd/gb/testdata/advice_catalog_snapshot.json' \
        ':(exclude)gardenio/controller/internal/advice/lint_test.go' \
        ':(exclude)gardenio/docs/audits/**' \
        2>/dev/null || true)"
fi

violations="$(printf '%s\n' "$raw" \
  | grep -vE "$exempt_path" \
  | grep -vE "$exempt_line" \
  | sed '/^$/d' || true)"

if [[ -n "$violations" ]]; then
  cat >&2 <<MSG
lint-glab-mr-flags: gh-style \`glab mr list --state=...\` invocation found.

glab has NO \`--state\` flag — it errors "Unknown flag: --state". Use:
  glab mr list            # open MRs (the default)
  glab mr list --merged   # or -M
  glab mr list --closed   # or -c
  glab mr list --all      # or -A

If a line legitimately discusses the flag (e.g. to warn against it), it is
already exempt when it contains "Unknown flag" or "deprecated"; otherwise add
an auditable inline \`glab-lint-ok\` marker.

Violations:
$violations
MSG
  exit 1
fi
