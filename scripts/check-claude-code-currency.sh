#!/usr/bin/env bash
# check-claude-code-currency.sh
#
# Guard: CLAUDE_CODE_VERSION in gardenio/images/agent/install/versions.env must
# track the npm `stable` dist-tag of @anthropic-ai/claude-code.
#
# WHY THIS EXISTS (ka-0000007531): the agent image's Claude Code pin was bumped
# only by ad-hoc one-off task beads (ka-0000006725 2.1.154, ka-0000000133
# 2.1.187, ka-0000001055 2.1.211). Nothing watched the pin between those beads,
# so the fleet silently ran an older Claude Code than the one shipping upstream —
# measured 2026-08-25: pinned 2.1.211, `stable` already 2.1.231. Operator
# direction (Matt Baker, Slack thread ka-0000001535) is that keeping Claude Code
# current is the RELEASE MANAGER's standing responsibility, so the check has to
# be a command the release-owner gardener can run every cycle, not tribal memory.
#
# `stable` — not `latest`. npm's `latest` for this package tracks the newest
# build (2.1.246 on the day this was written) while `stable` is the vetted
# channel; every past bump bead chose `stable`, so that is what this guard
# compares against.
#
# The guard reports drift; it does NOT bump. Bumping is two coordinated edits
# (versions.env and the THREE sha512s in install/checksums.env) plus an image
# rebuild, which belongs in a reviewed MR — see the "next steps" block this
# prints on drift.
#
# Usage:
#   ./scripts/check-claude-code-currency.sh            # human-readable
#   ./scripts/check-claude-code-currency.sh --json     # machine-readable
#   REPO_ROOT=/tmp/fixture ./scripts/check-claude-code-currency.sh
#   CLAUDE_CODE_UPSTREAM=2.1.231 ./scripts/check-claude-code-currency.sh
#       # skip the network read (tests, air-gapped runners, pre-fetched value)
#
# Exit status:
#   0  pinned == upstream stable (in sync), or pinned is AHEAD of stable
#      (a deliberate pre-release pin — reported, not failed).
#   1  operational failure (missing/unparseable versions.env, registry
#      unreachable, unparseable registry response). Fails LOUD: a currency
#      alarm that silently passes on error is worse than no alarm.
#   2  drift — pinned is BEHIND the upstream stable dist-tag.
#
# Companion test: scripts/check-claude-code-currency_test.sh

set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "$0")" && pwd)"
REPO_ROOT="${REPO_ROOT:-$(cd "${SCRIPT_DIR}/.." && pwd)}"

REGISTRY_URL="${CLAUDE_CODE_REGISTRY_URL:-https://registry.npmjs.org/@anthropic-ai/claude-code}"
DIST_TAG="${CLAUDE_CODE_DIST_TAG:-stable}"
VERSIONS_ENV="${REPO_ROOT}/gardenio/images/agent/install/versions.env"

JSON=0
for arg in "$@"; do
  case "$arg" in
    --json) JSON=1 ;;
    -h | --help)
      sed -n '2,40p' "$0"
      exit 0
      ;;
    *)
      echo "check-claude-code-currency: unknown argument: $arg" >&2
      exit 1
      ;;
  esac
done

die() {
  echo "check-claude-code-currency: $1" >&2
  exit 1
}

# ── the pin ──────────────────────────────────────────────────────────
# Read by sed rather than `source` so the guard never executes the pin file.
[[ -f "$VERSIONS_ENV" ]] || die "missing ${VERSIONS_ENV}"
PINNED="$(sed -n 's/^export CLAUDE_CODE_VERSION=//p' "$VERSIONS_ENV" | head -1 | tr -d '"'"'"' \t\r')"
[[ -n "$PINNED" ]] || die "CLAUDE_CODE_VERSION not set in ${VERSIONS_ENV}"

# ── the upstream stable dist-tag ─────────────────────────────────────
# CLAUDE_CODE_UPSTREAM lets a caller inject the value (tests, air-gapped
# runners, or a release cycle that already fetched it) and skip the network.
if [[ -n "${CLAUDE_CODE_UPSTREAM:-}" ]]; then
  UPSTREAM="$CLAUDE_CODE_UPSTREAM"
  SOURCE="injected"
else
  command -v curl >/dev/null 2>&1 || die "curl not found and CLAUDE_CODE_UPSTREAM unset"
  command -v jq >/dev/null 2>&1 || die "jq not found and CLAUDE_CODE_UPSTREAM unset"
  # The abbreviated-metadata Accept header keeps this to a few KB instead of the
  # multi-MB full packument.
  BODY="$(curl -fsSL --max-time 30 \
    -H 'Accept: application/vnd.npm.install-v1+json' \
    "$REGISTRY_URL" 2>/dev/null)" ||
    die "registry unreachable: ${REGISTRY_URL}"
  UPSTREAM="$(printf '%s' "$BODY" | jq -r --arg t "$DIST_TAG" '.["dist-tags"][$t] // empty')"
  [[ -n "$UPSTREAM" ]] || die "dist-tag '${DIST_TAG}' absent from ${REGISTRY_URL} response"
  SOURCE="$REGISTRY_URL"
fi

SEMVER_RE='^[0-9]+\.[0-9]+\.[0-9]+'
[[ "$PINNED" =~ $SEMVER_RE ]] || die "pinned version is not semver-shaped: '${PINNED}'"
[[ "$UPSTREAM" =~ $SEMVER_RE ]] || die "upstream version is not semver-shaped: '${UPSTREAM}'"

# ── compare ──────────────────────────────────────────────────────────
# sort -V orders versions; equal strings short-circuit first so "behind" is
# only ever reported for a genuine ordering difference.
if [[ "$PINNED" == "$UPSTREAM" ]]; then
  STATUS=in-sync
  RC=0
elif [[ "$(printf '%s\n%s\n' "$PINNED" "$UPSTREAM" | sort -V | head -1)" == "$PINNED" ]]; then
  STATUS=behind
  RC=2
else
  STATUS=ahead
  RC=0
fi

if ((JSON)); then
  jq -n \
    --arg pinned "$PINNED" \
    --arg upstream "$UPSTREAM" \
    --arg tag "$DIST_TAG" \
    --arg status "$STATUS" \
    --arg source "$SOURCE" \
    '{pinned:$pinned, upstream:$upstream, dist_tag:$tag, status:$status, source:$source, drift:($status=="behind")}'
else
  case "$STATUS" in
    in-sync)
      echo "check-claude-code-currency: OK — pinned ${PINNED} == npm ${DIST_TAG} ${UPSTREAM}"
      ;;
    ahead)
      echo "check-claude-code-currency: OK — pinned ${PINNED} is AHEAD of npm ${DIST_TAG} ${UPSTREAM} (deliberate pre-release pin)"
      ;;
    behind)
      cat >&2 <<EOF
check-claude-code-currency: DRIFT — agent image is BEHIND upstream Claude Code
  pinned   : ${PINNED}   (gardenio/images/agent/install/versions.env)
  npm ${DIST_TAG}: ${UPSTREAM}

To bump (all in ONE MR — a partial bump ships a broken image):
  1. gardenio/images/agent/install/versions.env
       export CLAUDE_CODE_VERSION=${UPSTREAM}
  2. gardenio/images/agent/install/checksums.env — refresh BOTH sha512s:
       curl -fsSL https://registry.npmjs.org/@anthropic-ai/claude-code/-/claude-code-${UPSTREAM}.tgz | sha512sum
       curl -fsSL https://registry.npmjs.org/@anthropic-ai/claude-code-linux-arm64/-/claude-code-linux-arm64-${UPSTREAM}.tgz | sha512sum
EOF
      ;;
  esac
fi

exit "$RC"
