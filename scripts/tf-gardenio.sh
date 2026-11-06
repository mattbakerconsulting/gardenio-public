#!/usr/bin/env bash
# tf-gardenio.sh
#
# THE way to run terraform against composition/gardenio (gd-F0DwR8UEtV).
#
# It is a thin wrapper: it runs scripts/check-tf-tree-freshness.sh, and only if
# that passes does it hand the arguments to terraform in
# gardenio/terraform/composition/gardenio. See that script's header for the
# measured incident this exists to prevent -- in short, a plan from a two-hour
# old checkout proposed destroying the entire ziti public edge node and
# re-adding a world-open 0.0.0.0/0 security-group rule, because neither change
# existed yet in that tree.
#
# The guard covers PLAN as well as apply, deliberately. The stale plan above was
# nearly read as evidence about main; a plan that describes a tree nobody is
# going to apply is worse than no plan, because it looks like information.
#
# Usage:
#   ./scripts/tf-gardenio.sh plan
#   ./scripts/tf-gardenio.sh plan -target=module.k3s
#   ./scripts/tf-gardenio.sh apply -target=module.k3s
#   ./scripts/tf-gardenio.sh --allow-unpushed plan     # local commits, deliberate
#
# --allow-unpushed must come FIRST if used; everything after the first argument
# is passed to terraform untouched (terraform's own first argument is always a
# subcommand, so there is no ambiguity).
#
# NOTE, not enforced here: `apply` on this composition is expected to be
# `-target`ed. That is a live operational judgement about capacity and the etcd
# voters rather than an invariant, so it is documented in
# gardenio/terraform/README.md and not encoded as a check -- a guard should
# assert things that are true by construction, not things that are true today.
#
# Exit status: the guard's, if it refuses; otherwise terraform's.
#
# Companion test: scripts/tf-gardenio_test.sh
# Guard: scripts/check-tf-tree-freshness.sh
# Task: gd-F0DwR8UEtV.

set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="$(cd "$SCRIPT_DIR/.." && pwd)"
COMPOSITION="$REPO_ROOT/gardenio/terraform/composition/gardenio"

GUARD_ARGS=(--repo "$REPO_ROOT")
if [ "${1:-}" = "--allow-unpushed" ]; then
	GUARD_ARGS+=(--allow-unpushed)
	shift
fi

if [ $# -eq 0 ]; then
	sed -n '/^# Usage:/,/^# Exit status:/p' "${BASH_SOURCE[0]}" | sed 's/^# \{0,1\}//' >&2
	exit 2
fi

[ -d "$COMPOSITION" ] || {
	printf 'tf-gardenio: composition not found: %s\n' "$COMPOSITION" >&2
	exit 2
}
command -v terraform >/dev/null 2>&1 || {
	printf 'tf-gardenio: terraform is not on PATH\n' >&2
	exit 2
}

# No `|| true`, no pipeline: the guard's exit status IS the gate, and `set -e`
# plus a bare call is the only form where that cannot be silently discarded.
bash "$SCRIPT_DIR/check-tf-tree-freshness.sh" "${GUARD_ARGS[@]}"

cd "$COMPOSITION"
exec terraform "$@"
