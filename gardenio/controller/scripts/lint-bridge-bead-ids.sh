#!/usr/bin/env bash
# lint-bridge-bead-ids.sh
#
# Forbids bare bead-ID interpolation in Slack-bound format strings within
# the gardenio bridge package. Patterns like
#
#     fmt.Sprintf("foo `%s` bar", bead.ID)
#
# must instead route through the b.beadLink(id, label) or
# b.beadTitleLink(id, title) helpers so beads-viewer hyperlinks render in
# Slack. See docs in gardenio/controller/internal/bridge/bot.go.
#
# Usage:
#   ./scripts/lint-bridge-bead-ids.sh                 # scan default bridge dir
#   ./scripts/lint-bridge-bead-ids.sh path/to/file    # scan explicit file(s)
#
# Run from gardenio/controller/. Exits non-zero on first violation.
#
# Companion to the integration test in
# gardenio/controller/internal/bridge/beads_viewer_links_test.go (ka-0000000442).
# Part of epic ka-0000003402 / task ka-0000006452.

set -euo pipefail

scan_dir="${SCAN_DIR:-internal/bridge}"

# Exempt basenames. These files either define the helpers themselves, are
# unrelated to Slack-bound text, or hold a legacy notifier path that does
# not have access to *Bot and is tracked separately for conversion.
exempt_files=(
  bot.go               # defines b.beadLink / b.beadTitleLink helpers
  bead_link_test.go    # tests for the helpers
  slack_linkify.go     # transport-level linkify wrapper (post-format)
  slack.go             # legacy SlackNotifier — pending conversion
)

is_exempt_basename() {
  local base="$1"
  case "$base" in
    *_test.go) return 0 ;;
  esac
  for ex in "${exempt_files[@]}"; do
    [[ "$base" == "$ex" ]] && return 0
  done
  return 1
}

# Resolve target files: positional args take precedence so callers (tests)
# can scan a single fixture; otherwise scan the bridge dir top level.
candidate_files=()
if (( $# > 0 )); then
  for f in "$@"; do
    candidate_files+=("$f")
  done
else
  while IFS= read -r f; do
    base="$(basename "$f")"
    is_exempt_basename "$base" && continue
    candidate_files+=("$f")
  done < <(find "$scan_dir" -maxdepth 1 -type f -name '*.go' | sort)
fi

if (( ${#candidate_files[@]} == 0 )); then
  exit 0
fi

# A line is a violation when it contains a backtick-wrapped `%s` (Slack code
# style) AND a bare bead.ID / .BeadID reference, AND does not pass the ID
# through one of the helpers (beadLink / beadTitle / beadTitleLink).
violations=$(grep -HnE '`%s`' "${candidate_files[@]}" 2>/dev/null \
  | grep -E '(bead\.ID|\.BeadID\b)' \
  | grep -vE 'beadLink|beadTitle|beadTitleLink' \
  || true)

if [[ -n "$violations" ]]; then
  cat >&2 <<MSG
lint-bridge-bead-ids: bare bead-ID interpolation in Slack format string.

Format strings like \`%s\` rendering bead.ID directly in Slack-bound text
must go through b.beadLink(id, label) or b.beadTitleLink(id, title) so the
beads-viewer URL renders as a clickable hyperlink.

Violations:
$violations
MSG
  exit 1
fi
