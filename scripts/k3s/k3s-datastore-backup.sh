#!/usr/bin/env bash
# k3s-datastore-backup.sh — back up the single-node k3s SQLite/kine datastore.
#
# Phase 0 P0.2 (ka-0000002781), epic ka-0000007690. Source:
# gardenio/docs/research/inbox-k8s-distro-choice.md §"Backup / DR" (step 7).
#
# IMPORTANT correctness note: the research doc's install-sketch step 7 says to
# enable `k3s etcd-snapshot save` "with the SQLite datastore". That is WRONG —
# `k3s etcd-snapshot` and the `etcd-snapshot-schedule-cron` server flag ONLY
# apply to the embedded ETCD datastore. On a single node running SQLite/kine
# (the recommended datastore — step 8), `k3s etcd-snapshot save` fails with
# "etcd database not found" (k3s-io/k3s#8960). The correct datastore backup is
# a consistent copy of the kine SQLite DB, which this script performs.
#
# What this backs up:
#   * The kine SQLite datastore under /var/lib/rancher/k3s/server/db, taken with
#     `sqlite3 .backup` (a live-consistent online backup, no service stop).
#   * The node token (/var/lib/rancher/k3s/server/token) — it ENCRYPTS the
#     datastore's secrets, so a datastore backup is useless to restore without
#     the matching token. Always captured alongside.
#
# What this does NOT back up (separate concern — research step 7 / open-q 6):
#   * local-path PV *contents* under /var/lib/rancher/k3s/storage (postgres,
#     NATS data). The datastore records PV *objects*, not their bytes. Use an
#     EBS/filesystem snapshot or Velero for those. See the runbook in
#     scripts/k3s/README.md.
#
# Usage:
#   sudo scripts/k3s/k3s-datastore-backup.sh [--out-dir DIR] [--retention N]
#                                            [--s3-uri s3://bucket/prefix]
#
# Env / flags:
#   OUT_DIR     local backup dir       (default: /var/lib/rancher/k3s/server/db-snapshots)
#   RETENTION   keep newest N backups  (default: 10; 0 = keep all)
#   S3_URI      optional off-box copy  (default: none; needs awscli + creds)
#   DATA_DIR    k3s data dir           (default: /var/lib/rancher/k3s)
set -euo pipefail

OUT_DIR="${OUT_DIR:-/var/lib/rancher/k3s/server/db-snapshots}"
RETENTION="${RETENTION:-10}"
S3_URI="${S3_URI:-}"
DATA_DIR="${DATA_DIR:-/var/lib/rancher/k3s}"

while [ $# -gt 0 ]; do
  case "$1" in
    --out-dir) OUT_DIR="$2"; shift 2;;
    --retention) RETENTION="$2"; shift 2;;
    --s3-uri) S3_URI="$2"; shift 2;;
    --data-dir) DATA_DIR="$2"; shift 2;;
    -h|--help) sed -n '2,45p' "$0"; exit 0;;
    *) echo "unknown arg: $1" >&2; exit 2;;
  esac
done

log()  { printf '\033[1;36m[k3s-backup]\033[0m %s\n' "$*"; }
die()  { printf '\033[1;31m[k3s-backup] ERROR:\033[0m %s\n' "$*" >&2; exit 1; }

DB_DIR="$DATA_DIR/server/db"
STATE_DB="$DB_DIR/state.db"
TOKEN_FILE="$DATA_DIR/server/token"

[ -f "$STATE_DB" ] || die "kine datastore not found at $STATE_DB — is this a SQLite/kine k3s node? (etcd nodes use etcd-snapshot instead)"
command -v sqlite3 >/dev/null 2>&1 || die "sqlite3 not on PATH — install it (dnf install -y sqlite / apt-get install -y sqlite3)"

# Timestamp without Date.now()-style surprises — `date` is fine in shell.
ts="$(date -u +%Y%m%dT%H%M%SZ)"
mkdir -p "$OUT_DIR"
stage="$(mktemp -d)"
trap 'rm -rf "$stage"' EXIT

log "online .backup of kine datastore → state.db"
# `.backup` takes a consistent snapshot of a live SQLite DB without stopping k3s.
sqlite3 "$STATE_DB" ".backup '$stage/state.db'" || die "sqlite3 .backup failed"

if [ -f "$TOKEN_FILE" ]; then
  cp "$TOKEN_FILE" "$stage/token"
  log "captured node token (required to decrypt datastore secrets on restore)"
else
  log "WARN: $TOKEN_FILE absent — restore may fail to decrypt secrets"
fi

cat >"$stage/RESTORE.md" <<EOF
# k3s SQLite/kine datastore restore ($ts)

This is a SQLite/kine datastore backup (NOT a k3s etcd snapshot).

To restore on a fresh/clean k3s node:
  1. Stop k3s:            sudo systemctl stop k3s
  2. Restore the token:   sudo cp token $DATA_DIR/server/token
  3. Restore the DB:      sudo cp state.db $DB_DIR/state.db
     (remove stale WAL/SHM): sudo rm -f $DB_DIR/state.db-wal $DB_DIR/state.db-shm
  4. Start k3s:           sudo systemctl start k3s

NB: local-path PV contents under $DATA_DIR/storage are NOT in this backup —
restore them separately (EBS/filesystem snapshot or Velero).
EOF

archive="$OUT_DIR/k3s-kine-${ts}.tar.gz"
tar -C "$stage" -czf "$archive" state.db token RESTORE.md 2>/dev/null \
  || tar -C "$stage" -czf "$archive" state.db RESTORE.md
log "wrote $archive ($(du -h "$archive" | cut -f1))"

# ── Retention (local) ────────────────────────────────────────────────────────
if [ "$RETENTION" -gt 0 ] 2>/dev/null; then
  # Timestamped names (k3s-kine-YYYYmmddTHHMMSSZ) sort lexically == chronologically,
  # so a plain glob (sorted ascending by the shell) gives oldest-first without
  # parsing `ls`. Reverse to newest-first, then drop the newest $RETENTION.
  shopt -s nullglob
  all=("$OUT_DIR"/k3s-kine-*.tar.gz)
  shopt -u nullglob
  old=()
  if [ "${#all[@]}" -gt "$RETENTION" ]; then
    # all[] is ascending (oldest first); everything except the last $RETENTION is stale.
    old=("${all[@]:0:${#all[@]}-RETENTION}")
  fi
  if [ "${#old[@]}" -gt 0 ]; then
    log "pruning ${#old[@]} backup(s) beyond retention=$RETENTION"
    rm -f "${old[@]}"
  fi
fi

# ── Optional off-box copy ────────────────────────────────────────────────────
if [ -n "$S3_URI" ]; then
  command -v aws >/dev/null 2>&1 || die "S3_URI set but awscli not on PATH"
  log "uploading to ${S3_URI%/}/$(basename "$archive")"
  aws s3 cp "$archive" "${S3_URI%/}/$(basename "$archive")" \
    || die "S3 upload failed"
fi

log "✅ datastore backup complete"
