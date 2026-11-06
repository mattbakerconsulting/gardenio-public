#!/usr/bin/env bash
# install-k3s-durability.sh — wire the k3s pinning + backup durability units.
#
# Phase 0 P0.2 (ka-0000002781), epic ka-0000007690. Source:
# gardenio/docs/research/inbox-k8s-distro-choice.md §"install sketch"/(c)/§"Backup".
#
# Installs two pieces of restart durability on a single-node k3s box:
#
#  1. k3s-config-refresh.service — a oneshot unit, ordered BEFORE k3s.service,
#     that re-runs write-k3s-config.sh on every boot. An EC2 stop/start changes
#     the private IP; this re-pins --node-ip/--advertise-address to the current
#     IP and rewrites the cert SAN list before the apiserver comes up, so the
#     cert always matches a reachable address (the original failure this task
#     guards against).
#
#  2. k3s-datastore-backup.timer — a systemd timer that runs the kine SQLite
#     backup on a schedule (default twice daily, mirroring k3s's etcd-snapshot
#     default of 00:00/12:00). NOTE: we use a timer + sqlite .backup rather than
#     the k3s `etcd-snapshot-schedule-cron` flag because that flag only works on
#     the embedded-etcd datastore, not SQLite/kine (k3s-io/k3s#8960).
#
# This script installs the units; it does NOT start k3s. bootstrap-ec2.sh calls
# write-k3s-config.sh + this installer BEFORE the k3s install so the first boot
# already has the pinned config.
#
# Usage:
#   sudo scripts/k3s/install-k3s-durability.sh [--tls-san SAN]... [--bind-address ADDR]
#        [--backup-schedule "ONCAL"] [--backup-retention N] [--backup-s3 s3://...]
#        [--no-backup-timer]
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
TLS_SANS="${TLS_SANS:-}"
BIND_ADDRESS="${BIND_ADDRESS:-0.0.0.0}"
BACKUP_SCHEDULE="${BACKUP_SCHEDULE:-*-*-* 00,12:00:00}"   # systemd OnCalendar
BACKUP_RETENTION="${BACKUP_RETENTION:-10}"
BACKUP_S3="${BACKUP_S3:-}"
INSTALL_BACKUP_TIMER=1

while [ $# -gt 0 ]; do
  case "$1" in
    --tls-san) TLS_SANS="$TLS_SANS $2"; shift 2;;
    --bind-address) BIND_ADDRESS="$2"; shift 2;;
    --backup-schedule) BACKUP_SCHEDULE="$2"; shift 2;;
    --backup-retention) BACKUP_RETENTION="$2"; shift 2;;
    --backup-s3) BACKUP_S3="$2"; shift 2;;
    --no-backup-timer) INSTALL_BACKUP_TIMER=0; shift;;
    -h|--help) sed -n '2,40p' "$0"; exit 0;;
    *) echo "unknown arg: $1" >&2; exit 2;;
  esac
done

log() { printf '\033[1;36m[k3s-durability]\033[0m %s\n' "$*"; }
[ "$(id -u)" -eq 0 ] || { echo "must run as root" >&2; exit 1; }

# Install the helper scripts to a stable location so the units reference a fixed
# path independent of the (possibly ephemeral) repo checkout.
install -d /usr/local/lib/gardenio
install -m 0755 "$SCRIPT_DIR/write-k3s-config.sh"     /usr/local/lib/gardenio/write-k3s-config.sh
install -m 0755 "$SCRIPT_DIR/k3s-datastore-backup.sh" /usr/local/lib/gardenio/k3s-datastore-backup.sh
log "installed helper scripts to /usr/local/lib/gardenio/"

# Persist the SAN/bind config the refresh unit should apply each boot.
{
  echo "# Gardenio in a Box k3s durability config (P0.2 ka-0000002781)."
  echo "TLS_SANS=\"$(echo "$TLS_SANS" | xargs)\""
  echo "BIND_ADDRESS=\"$BIND_ADDRESS\""
} >/etc/gardenio-k3s-durability.env
chmod 0644 /etc/gardenio-k3s-durability.env

# Run it once now so the config exists before first k3s start.
log "generating initial /etc/rancher/k3s/config.yaml"
# The env file is generated above at runtime; shellcheck can't follow it, so
# point it at /dev/null (treat as empty) rather than suppressing the warning.
# shellcheck source=/dev/null
set -a; . /etc/gardenio-k3s-durability.env; set +a
/usr/local/lib/gardenio/write-k3s-config.sh

# ── 1. Boot-time config refresh, ordered before k3s ──────────────────────────
cat >/etc/systemd/system/k3s-config-refresh.service <<'EOF'
[Unit]
Description=Refresh k3s pinned node-ip/tls-san before k3s starts (Gardenio in a Box P0.2)
# Run after the network is up (we read the current private IP) but BEFORE k3s,
# so the apiserver comes up with a cert/advertise-address matching the live IP.
After=network-online.target
Wants=network-online.target
Before=k3s.service

[Service]
Type=oneshot
RemainAfterExit=yes
EnvironmentFile=-/etc/gardenio-k3s-durability.env
ExecStart=/usr/local/lib/gardenio/write-k3s-config.sh

[Install]
WantedBy=multi-user.target
# Also pull it in as a dependency of k3s so enabling k3s enables the refresh.
WantedBy=k3s.service
EOF

systemctl daemon-reload
systemctl enable k3s-config-refresh.service >/dev/null 2>&1 || true
log "installed + enabled k3s-config-refresh.service (Before=k3s.service)"

# ── 2. Datastore backup timer ────────────────────────────────────────────────
if [ "$INSTALL_BACKUP_TIMER" = "1" ]; then
  cat >/etc/systemd/system/k3s-datastore-backup.service <<EOF
[Unit]
Description=k3s SQLite/kine datastore backup (Gardenio in a Box P0.2)
After=k3s.service

[Service]
Type=oneshot
Environment=RETENTION=${BACKUP_RETENTION}
$( [ -n "$BACKUP_S3" ] && echo "Environment=S3_URI=${BACKUP_S3}" )
ExecStart=/usr/local/lib/gardenio/k3s-datastore-backup.sh
EOF

  cat >/etc/systemd/system/k3s-datastore-backup.timer <<EOF
[Unit]
Description=Schedule k3s SQLite/kine datastore backups (Gardenio in a Box P0.2)

[Timer]
OnCalendar=${BACKUP_SCHEDULE}
Persistent=true

[Install]
WantedBy=timers.target
EOF

  systemctl daemon-reload
  systemctl enable k3s-datastore-backup.timer >/dev/null 2>&1 || true
  log "installed + enabled k3s-datastore-backup.timer (OnCalendar='${BACKUP_SCHEDULE}', retention=${BACKUP_RETENTION})"
else
  log "skipping backup timer (--no-backup-timer)"
fi

log "✅ k3s durability units installed"
