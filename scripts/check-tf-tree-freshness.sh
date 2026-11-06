#!/usr/bin/env bash
# check-tf-tree-freshness.sh
#
# Refuse to plan or apply Terraform from a checkout that is not exactly
# origin/main.
#
# WHY THIS EXISTS (gd-F0DwR8UEtV, measured 2026-09-23): a `terraform plan` of
# composition/gardenio run from a checkout ~2h old reported
#
#     Plan: 1 to add, 3 to change, 6 to destroy.
#
# The 6 destroys were the ENTIRE ziti public edge node -- aws_eip, aws_instance,
# aws_network_interface, aws_route53_record, aws_security_group and its ingress
# rule, all `.ziti_edge` -- because that node had been ADDED to main at 18:20Z
# and the stale tree simply did not contain it. The 1 add was
# `aws_vpc_security_group_ingress_rule.ziti_router_public` coming BACK: a
# superseded world-open `0.0.0.0/0` rule on the shared nodes SG, the exact
# landmine `5412373e` had just removed (gd-59AbgRyxPC, comment 3559). So one
# untargeted apply from a two-hour-old tree would have taken down public ziti
# AND re-opened the hole, in the same run. Same tree from origin/main:
# `0 to add, 3 to change, 0 to destroy`.
#
# A RESOURCE-SIDE GUARD CANNOT CATCH THIS. `prevent_destroy` lives on the
# resource, and the whole point is that a stale tree does not contain the
# resource -- there is nothing for the lifecycle block to be attached to.
# The check has to be on the TREE, before Terraform is handed the plan.
#
# IT EXITS. It does not warn. The ad-hoc check this replaces printed
#     *** STALE -- refusing ***
# and then ran the plan anyway, because `cmd || echo ...` does not exit and
# nothing checked the status afterwards. I read the resulting 6-destroy plan as
# evidence about main before catching it. A guard that warns without exiting is
# decoration (cf. scripts/README.md, the ci-guards-that-cannot-fail family).
#
# WHAT IT CHECKS, and what each check is for:
#
#   0. The repo is not shallow. `merge-base --is-ancestor` gives unreliable
#      answers when history is truncated, and an unreliable answer here is
#      indistinguishable from a pass.
#   1. `git fetch` SUCCEEDS. Without this the whole guard is theatre: it would
#      compare HEAD against whatever `origin/main` happened to say at the last
#      fetch, so a stale tree with an equally stale remote ref passes cleanly.
#      A fetch failure is a refusal, not a warning -- "I could not verify" must
#      never render as "verified".
#   2. `git merge-base --is-ancestor <remote> HEAD` -- HEAD contains origin/main.
#      This is the check that catches the measured hazard (behind), and it also
#      catches diverged.
#   3. HEAD == origin/main -- no unpushed local commits. This is the INVERSE
#      hazard: applying infrastructure that no one has reviewed and that nothing
#      else can reproduce. Overridable with --allow-unpushed, because testing a
#      local change against real state is a legitimate operator workflow; it is
#      not overridable silently.
#
# DELIBERATELY NOT CHECKED: a dirty working tree. Editing and re-planning in a
# loop is normal Terraform work, and blocking it would get this wrapper routed
# around within a day -- which would cost more than it saves. The measured
# hazard was a stale COMMIT, not uncommitted edits.
#
# This guard makes the sanctioned path safe; it is not a cage. `terraform` is
# still on everyone's PATH. That is the intended level of protection: the
# failure being prevented was an operator not thinking to check, not an operator
# deciding to proceed.
#
# Usage:
#   ./scripts/check-tf-tree-freshness.sh                  # check the repo containing cwd
#   ./scripts/check-tf-tree-freshness.sh --repo <dir>     # check another checkout
#   ./scripts/check-tf-tree-freshness.sh --branch <name>  # compare against a branch other than main
#   ./scripts/check-tf-tree-freshness.sh --allow-unpushed # permit local commits on top of origin/main
#
# Exit status: 0 if the tree is exactly origin/<branch> (or ahead of it with
# --allow-unpushed), non-zero otherwise. Never 0 on an unverifiable tree.
#
# Companion test: scripts/check-tf-tree-freshness_test.sh
# Wrapper that calls it: scripts/tf-gardenio.sh
# Task: gd-F0DwR8UEtV.

set -euo pipefail

REPO=""
BRANCH="main"
REMOTE="origin"
ALLOW_UNPUSHED=0

die() {
	printf '%s\n' "$*" >&2
	exit 2
}

while [ $# -gt 0 ]; do
	case "$1" in
	--repo)
		[ $# -ge 2 ] || die "check-tf-tree-freshness: --repo needs a value"
		REPO="$2"
		shift 2
		;;
	--branch)
		[ $# -ge 2 ] || die "check-tf-tree-freshness: --branch needs a value"
		BRANCH="$2"
		shift 2
		;;
	--remote)
		[ $# -ge 2 ] || die "check-tf-tree-freshness: --remote needs a value"
		REMOTE="$2"
		shift 2
		;;
	--allow-unpushed)
		ALLOW_UNPUSHED=1
		shift
		;;
	-h | --help)
		sed -n '/^# Usage:/,/^# Exit status:/p' "$0" | sed 's/^# \{0,1\}//'
		exit 0
		;;
	*)
		die "check-tf-tree-freshness: unknown argument: $1"
		;;
	esac
done

if [ -n "$REPO" ]; then
	[ -d "$REPO" ] || die "check-tf-tree-freshness: no such directory: $REPO"
	cd "$REPO"
fi

git rev-parse --git-dir >/dev/null 2>&1 ||
	die "check-tf-tree-freshness: not a git repository: $(pwd)"

# 0. Shallow history makes the ancestry check unreliable, and an unreliable
#    answer must not be allowed to read as a pass.
if [ "$(git rev-parse --is-shallow-repository)" = "true" ]; then
	die "check-tf-tree-freshness: REFUSING -- this is a shallow clone.
  Ancestry against $REMOTE/$BRANCH cannot be established from truncated history.
  Fix: git fetch --unshallow"
fi

HEAD_SHA="$(git rev-parse --verify HEAD 2>/dev/null)" ||
	die "check-tf-tree-freshness: REFUSING -- HEAD does not resolve to a commit."

# 1. Fetch, and treat a failure as a refusal. Comparing against a cached
#    remote-tracking ref would let a stale tree pass against a stale ref.
if ! FETCH_ERR="$(git fetch --quiet "$REMOTE" "$BRANCH" 2>&1)"; then
	die "check-tf-tree-freshness: REFUSING -- could not fetch $REMOTE/$BRANCH.
  The tree cannot be shown to be current, and 'could not verify' is not 'verified'.
  git said: ${FETCH_ERR:-(no output)}"
fi

# Resolve what we JUST fetched, rather than refs/remotes/$REMOTE/$BRANCH, so the
# answer does not depend on the remote's configured refspec.
REMOTE_SHA="$(git rev-parse --verify FETCH_HEAD 2>/dev/null)" ||
	die "check-tf-tree-freshness: REFUSING -- fetch succeeded but FETCH_HEAD does not resolve."

short() { git rev-parse --short "$1"; }

# 2. HEAD must contain origin/<branch>.
if ! git merge-base --is-ancestor "$REMOTE_SHA" "$HEAD_SHA"; then
	BEHIND="$(git rev-list --count "$HEAD_SHA".."$REMOTE_SHA")"
	AHEAD="$(git rev-list --count "$REMOTE_SHA".."$HEAD_SHA")"
	if [ "$AHEAD" -eq 0 ]; then
		die "check-tf-tree-freshness: REFUSING -- tree is STALE.
  HEAD              $(short "$HEAD_SHA")
  $REMOTE/$BRANCH   $(short "$REMOTE_SHA")  ($BEHIND commit(s) ahead of HEAD)
  Planning or applying from here can destroy resources added to $BRANCH since
  this checkout, and resurrect ones deleted from it.
  Fix: git merge --ff-only $REMOTE/$BRANCH"
	fi
	die "check-tf-tree-freshness: REFUSING -- tree has DIVERGED from $REMOTE/$BRANCH.
  HEAD              $(short "$HEAD_SHA")  ($AHEAD commit(s) not on $BRANCH)
  $REMOTE/$BRANCH   $(short "$REMOTE_SHA")  ($BEHIND commit(s) not in HEAD)
  Fix: rebase onto $REMOTE/$BRANCH, or check out $BRANCH."
fi

# 3. No unpushed local commits, unless explicitly allowed.
if [ "$HEAD_SHA" != "$REMOTE_SHA" ]; then
	AHEAD="$(git rev-list --count "$REMOTE_SHA".."$HEAD_SHA")"
	if [ "$ALLOW_UNPUSHED" -eq 0 ]; then
		die "check-tf-tree-freshness: REFUSING -- $AHEAD unpushed commit(s) on top of $REMOTE/$BRANCH.
  HEAD              $(short "$HEAD_SHA")
  $REMOTE/$BRANCH   $(short "$REMOTE_SHA")
  Applying these means applying infrastructure nobody has reviewed and that no
  other checkout can reproduce.
  Fix: push them, or pass --allow-unpushed if this is a deliberate local test."
	fi
	printf 'check-tf-tree-freshness: OK (--allow-unpushed) -- HEAD %s is %s commit(s) ahead of %s/%s %s\n' \
		"$(short "$HEAD_SHA")" "$AHEAD" "$REMOTE" "$BRANCH" "$(short "$REMOTE_SHA")"
	exit 0
fi

printf 'check-tf-tree-freshness: OK -- HEAD %s == %s/%s\n' \
	"$(short "$HEAD_SHA")" "$REMOTE" "$BRANCH"
