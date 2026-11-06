# shellcheck shell=bash
# release-role-guard.sh -- sourced by release-cut.sh and release.sh.
#
# Release tags are cut only by the standing release captain (trigger
# gd-FgKpNn3A02, role `release-captain`) or by the operator, never by a task
# crew (gd-CVotEQ3vk9: a check-in crew cut 2026.298.1 itself, outside the
# ~04:00Z / ~16:00Z release windows; every cut builds every image).
#
# An agent pod carries its role in BOAT_ROLE (comma-separated when an agent has
# several roles). The operator's laptop has no BOAT_ROLE, so it passes. Any other
# agent role is refused, before anything is fetched, tagged or pushed. A
# --dry-run is never refused: it pushes nothing.
#
# GARDENIO_RELEASE_OPERATOR=1 is the operator's explicit override, for a laptop
# shell that happens to export BOAT_ROLE. An agent must never set it. A crew that
# needs a build files a helm_deploy escalation naming the next window.
#
# This does not stop a raw `git push origin <tag>`: every pod pushes as the same
# GitHub identity, so there is no server-side tag ruleset yet (see
# gardenio/docs/release-pipeline.md, "Who cuts a release").

# release_role_guard <script-name> <dry-run:true|false> -- returns when the
# caller may cut; otherwise prints why and exits 4.
release_role_guard() {
  local script="$1" dry_run="$2" role
  local -a roles=()
  if [ "$dry_run" = true ] || [ "${GARDENIO_RELEASE_OPERATOR:-}" = 1 ] || [ -z "${BOAT_ROLE:-}" ]; then
    return 0
  fi
  IFS=',' read -ra roles <<<"$BOAT_ROLE"
  for role in "${roles[@]}"; do
    if [ "$role" = release-captain ]; then
      return 0
    fi
  done
  cat >&2 <<EOF
${script}: refused: release tags are cut only by the release captain or the operator (BOAT_ROLE=${BOAT_ROLE}).
${script}: need a build? File a helm_deploy escalation and name the next release window (~04:00Z or ~16:00Z):
${script}:   gb escalation create --operation-type helm_deploy --requesting-task <your-task> ...
${script}: --dry-run is allowed and pushes nothing. See gardenio/docs/release-pipeline.md, "Who cuts a release".
EOF
  exit 4
}
