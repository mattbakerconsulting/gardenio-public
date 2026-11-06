#!/usr/bin/env bash
# seed-config-dump.sh — Box-config seed INSTALL (MVC M3, ka-0000003598).
#
# Sibling to seed-bundle.sh (images) and seed-litellm-account.sh (account bead).
# Home epic: ka-0000000011. Plan of record: gardenio/docs/box-config-seeding.md.
#
# WHAT THIS DOES: on a fresh air-gapped box, after the in-box beads daemon is up
# and BEFORE the first agent spawn, restore the pre-baked box-config seed into the
# box's fresh beads DB so a friend's first agent already has its (minimal, honest)
# advice — the air-gap equivalent of how prod gets its seeded advice.
#
# The seed is the JSON-lines artifact `gardenio-seed-<calver>.jsonl` that
# build-seed-config.sh produced at release time and assemble-tarboat.sh packaged
# under `tarboat/seed/`. Each line is `{seed_kind, id, type, title, body, labels}`.
# This script verifies the .sha256, then `kd create`s one advice bead per line.
#
# IDEMPOTENT: a seed-marker bead (title "box-config-seed applied <calver>", a
# type=config bead) records that the seed ran. On a second invocation the marker
# is present → this is a no-op (re-running bootstrap never double-seeds). Pass
# --force to re-seed regardless (e.g. after wiping the DB).
#
# --airgap-strict: a MISSING or CORRUPT seed is a HARD FAIL (same posture as the
# G1.1 staged-asset rule) — never a silent empty-config boot on a strict box.
#
# Usage:
#   seed-config-dump.sh [--seed PATH] [--kd-bin kd] [--operated-by WHO]
#       [--airgap-strict] [--force] [--no-verify-sha] [--dry-run]
#
# Env / flags (flags win): SEED, KD_BIN, OPERATED_BY, AIRGAP_STRICT, FORCE,
# VERIFY_SHA, DRY_RUN. Defaults find the newest seed under /opt/gardenio/seed/.
#
# bash -n + shellcheck clean. Companion unit test: seed-config-dump_test.sh.
set -euo pipefail

SEED="${SEED:-}"
KD_BIN="${KD_BIN:-kd}"
# OPERATED_BY satisfies the beads server-side operator-resolution guard on create
# (same reason seed-litellm-account.sh sets it). Override per box.
OPERATED_BY="${OPERATED_BY:-oidc:matthew.baker@acme.example}"
AIRGAP_STRICT="${AIRGAP_STRICT:-0}"
FORCE="${FORCE:-0}"
VERIFY_SHA="${VERIFY_SHA:-1}"
DRY_RUN="${DRY_RUN:-0}"

while [ $# -gt 0 ]; do
  case "$1" in
    --seed)          SEED="$2"; shift 2;;
    --kd-bin)        KD_BIN="$2"; shift 2;;
    --operated-by)   OPERATED_BY="$2"; shift 2;;
    --airgap-strict) AIRGAP_STRICT=1; shift;;
    --force)         FORCE=1; shift;;
    --no-verify-sha) VERIFY_SHA=0; shift;;
    --dry-run)       DRY_RUN=1; shift;;
    -h|--help)       sed -n '2,34p' "$0"; exit 0;;
    *) echo "unknown arg: $1" >&2; exit 2;;
  esac
done

log()  { printf '\033[1;36m[seed-config]\033[0m %s\n' "$*"; }
warn() { printf '\033[1;33m[seed-config] WARN:\033[0m %s\n' "$*" >&2; }
die()  { printf '\033[1;31m[seed-config] FATAL:\033[0m %s\n' "$*" >&2; exit 1; }

# strict_or_skip — under --airgap-strict a problem is fatal; otherwise a soft skip
# (exit 0) so a non-strict box that shipped without a config seed still boots.
strict_or_skip() {
  if [ "$AIRGAP_STRICT" = "1" ]; then die "$1 (--airgap-strict)"; fi
  warn "$1 — skipping config seed (not --airgap-strict)"; exit 0
}

command -v "$KD_BIN" >/dev/null 2>&1 || die "kd binary '$KD_BIN' not on PATH"
command -v jq        >/dev/null 2>&1 || die "jq not on PATH"

# ── Resolve the seed path ────────────────────────────────────────────────────
if [ -z "$SEED" ]; then
  SEED="$(find /opt/gardenio/seed -maxdepth 1 -name 'gardenio-seed-*.jsonl' 2>/dev/null \
            | sort -V | tail -1)"
  [ -n "$SEED" ] || strict_or_skip "no gardenio-seed-*.jsonl under /opt/gardenio/seed/"
fi
[ -f "$SEED" ] || strict_or_skip "seed not found: $SEED"
log "seed: $SEED"

# ── Verify the sha256 sidecar (offline integrity) ────────────────────────────
if [ "$VERIFY_SHA" = "1" ]; then
  if [ -f "${SEED}.sha256" ]; then
    ( cd "$(dirname "$SEED")" && sha256sum -c "$(basename "$SEED").sha256" >/dev/null ) \
      || strict_or_skip "seed sha256 mismatch — refusing a corrupt config seed"
    log "sha256 OK"
  else
    [ "$AIRGAP_STRICT" = "1" ] && die "no ${SEED}.sha256 sidecar (--airgap-strict)"
    warn "no ${SEED}.sha256 sidecar — skipping integrity check"
  fi
fi

# CALVER from the filename (gardenio-seed-<calver>.jsonl) for the marker bead.
CALVER="$(basename "$SEED" .jsonl)"; CALVER="${CALVER#gardenio-seed-}"
MARKER_TITLE="box-config-seed applied ${CALVER}"

# ── Idempotency: bail if the marker bead already exists ──────────────────────
if [ "$FORCE" != "1" ]; then
  if "$KD_BIN" search "$MARKER_TITLE" --json 2>/dev/null \
       | jq -e --arg t "$MARKER_TITLE" 'any(.[]; .title == $t)' >/dev/null 2>&1; then
    log "marker '$MARKER_TITLE' already present — config already seeded, no-op."
    exit 0
  fi
fi

# ── Restore: one kd create per JSON-lines record ─────────────────────────────
seeded=0 failed=0
while IFS= read -r line; do
  [ -n "$line" ] || continue
  kind="$(printf '%s' "$line" | jq -r '.seed_kind // "advice"')"
  [ "$kind" = "advice" ] || { warn "skip unknown seed_kind '$kind'"; continue; }

  title="$(printf '%s' "$line" | jq -r '.title')"
  body="$(printf '%s' "$line" | jq -r '.body')"
  # labels[] → repeated --label args.
  mapfile -t labels < <(printf '%s' "$line" | jq -r '.labels[]?')
  label_args=(); for l in "${labels[@]}"; do label_args+=(--label "$l"); done

  if [ "$DRY_RUN" = "1" ]; then
    log "[dry-run] would seed advice: ${title:0:70}"
    seeded=$((seeded + 1)); continue
  fi

  if "$KD_BIN" create "$title" --type advice \
       --operated-by "$OPERATED_BY" \
       --description "$body" \
       "${label_args[@]}" >/dev/null 2>&1; then
    seeded=$((seeded + 1))
  else
    warn "failed to seed advice: ${title:0:70}"
    failed=$((failed + 1))
  fi
done < "$SEED"

[ "$failed" -eq 0 ] || strict_or_skip "${failed} advice bead(s) failed to seed"

# ── Drop the idempotency marker ──────────────────────────────────────────────
if [ "$DRY_RUN" != "1" ]; then
  "$KD_BIN" create "$MARKER_TITLE" --type config \
    --operated-by "$OPERATED_BY" \
    --description "Box-config seed ${CALVER} applied by seed-config-dump.sh (${seeded} advice beads). Idempotency marker — see gardenio/docs/box-config-seeding.md." \
    >/dev/null 2>&1 || warn "could not write seed marker (seed still applied)"
fi

log "done. ${seeded} advice bead(s) seeded from ${SEED}."
