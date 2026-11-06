#!/bin/bash
# hq-bootstrap [step ...] -- provision the Gas City HQ host (Ubuntu 24.04).
#
# Installed to /usr/local/sbin/hq-bootstrap by cloud-init user-data, which runs
# `hq-bootstrap all` once at first boot. Every step is idempotent, so the whole
# thing (or one step) can be re-run later through SSM:
#   aws ssm send-command --profile gardenio --region us-east-1 \
#     --targets Key=tag:Name,Values=gardenio-hq --document-name AWS-RunShellScript \
#     --parameters commands='hq-bootstrap supervisor'
#
# Pins come from /etc/hq/pins.env (written by user-data from terraform vars).
# Logs to /var/log/hq-bootstrap.log. The idle-stop timer treats a running
# hq-bootstrap as activity, so a long first boot is never powered off midway.
#
# Steps, in `all` order:
#   base        apt packages, awscli v2, user gc (+ systemd linger), dirs
#   go          Go toolchain at GO_VERSION under /usr/local/go
#   node        Node LTS major NODE_MAJOR from NodeSource
#   gascity     clone upstream at GC_COMMIT as gc; dolt + bd via upstream's
#               pinned-SHA installer scripts; `go build ./cmd/gc`
#   claude      Claude Code native install as gc
#   ziti        ziti-edge-tunnel binary + root unit (host mode, /var/lib/ziti)
#   idle-stop   hq-idle-stop script + 5-minute timer
#   backup      gc-dolt-backup script + daily 02:30 UTC timer (DOLT_BACKUP of
#               every db on the city's Dolt server, mirrored to S3)
#   ziti-enroll enrol with the JWT in SSM /<env>/hq/ziti-enroll-jwt (skips if
#               no parameter or already enrolled)
#   claude-creds copy SSM /<env>/hq/claude-credentials to ~gc/.claude (skips
#               if no parameter)
#   supervisor  ~gc/.gc/supervisor.toml + `gc supervisor install` (skips if
#               gc is not built yet)
set -uo pipefail
exec > >(tee -a /var/log/hq-bootstrap.log) 2>&1
export DEBIAN_FRONTEND=noninteractive
export AWS_PAGER=""

# pins.env is materialized by user-data at boot time and doesn't exist in the
# repo, so shellcheck can't follow it; point it at /dev/null instead of
# disabling the check (quench's escapes gate forbids `shellcheck disable=`
# comments outright).
# shellcheck source=/dev/null
. /etc/hq/pins.env
: "${ENV_NAME:=gardenio}" "${GC_USER:=gc}" "${GO_VERSION:?}" "${NODE_MAJOR:?}"
: "${DOLT_VERSION:?}" "${BD_VERSION:?}" "${GC_COMMIT:?}" "${ZITI_TUNNEL_VERSION:?}"
: "${ZITI_IDENTITY_NAME:=gardenio-hq}" "${GC_REPO_URL:=https://github.com/gastownhall/gascity.git}"

GC_HOME_DIR="/home/${GC_USER}"
GC_UID=""
GC_PATH="${GC_HOME_DIR}/.local/bin:/usr/local/go/bin:/usr/local/bin:/usr/bin:/bin"
ts() { date -u +%Y-%m-%dT%H:%M:%SZ; }
log() { echo "[$(ts)] hq-bootstrap: $*"; }
fail() { log "FAIL: $*"; exit 1; }

# run_gc CMD... : run a command as the gc user with its login PATH and the
# systemd user-manager socket (needed by `gc supervisor install`).
run_gc() {
  sudo -u "${GC_USER}" -H env \
    PATH="${GC_PATH}" HOME="${GC_HOME_DIR}" USER="${GC_USER}" LOGNAME="${GC_USER}" \
    XDG_RUNTIME_DIR="/run/user/${GC_UID}" \
    DBUS_SESSION_BUS_ADDRESS="unix:path=/run/user/${GC_UID}/bus" \
    "$@"
}

region() {
  local tok
  tok=$(curl -sf -X PUT "http://169.254.169.254/latest/api/token" -H "X-aws-ec2-metadata-token-ttl-seconds: 300")
  curl -sf -H "X-aws-ec2-metadata-token: ${tok}" "http://169.254.169.254/latest/meta-data/placement/region"
}

# ssm_get NAME : print a SecureString parameter, or return 1 when absent.
ssm_get() {
  aws ssm get-parameter --region "$(region)" --name "$1" --with-decryption \
    --query Parameter.Value --output text 2>/dev/null
}

step_base() {
  log "base: packages"
  apt-get update -q
  apt-get install -y -q tmux git curl jq unzip libicu-dev build-essential ca-certificates gnupg \
    acl rsync less htop
  if ! command -v aws >/dev/null; then
    curl -sSL https://awscli.amazonaws.com/awscli-exe-linux-x86_64.zip -o /tmp/awscliv2.zip
    unzip -q -o /tmp/awscliv2.zip -d /tmp && /tmp/aws/install --update >/dev/null
    rm -rf /tmp/aws /tmp/awscliv2.zip
  fi
  if ! id "${GC_USER}" >/dev/null 2>&1; then
    log "base: creating user ${GC_USER}"
    useradd --create-home --shell /bin/bash "${GC_USER}"
  fi
  GC_UID=$(id -u "${GC_USER}")
  loginctl enable-linger "${GC_USER}"
  # install -d creates missing parents as root; own ~/.local and friends
  # explicitly or the Claude Code installer cannot mkdir ~/.local/state.
  install -d -o "${GC_USER}" -g "${GC_USER}" -m 0755 \
    "${GC_HOME_DIR}/.local" "${GC_HOME_DIR}/.local/bin" "${GC_HOME_DIR}/.local/share" \
    "${GC_HOME_DIR}/.local/state" "${GC_HOME_DIR}/src"
  install -d -o "${GC_USER}" -g "${GC_USER}" -m 0700 "${GC_HOME_DIR}/.gc" "${GC_HOME_DIR}/.claude"
  install -d -m 0755 /var/lib/hq
  # Nothing listens on the public address: no sshd (SSM Session Manager only).
  systemctl disable --now ssh.socket ssh.service >/dev/null 2>&1 || true
  cat > /etc/profile.d/hq.sh <<'EOF'
# Gas City HQ host: toolchain paths (see gardenio/docs/hq-host.md).
export PATH="$HOME/.local/bin:/usr/local/go/bin:$PATH"
EOF
  # Wait for the user manager that linger starts, so run_gc can talk to it.
  for _ in $(seq 1 30); do
    [ -S "/run/user/${GC_UID}/bus" ] && break
    sleep 1
  done
  [ -S "/run/user/${GC_UID}/bus" ] || log "base: warning: no user bus at /run/user/${GC_UID}/bus yet"
}

step_go() {
  if [ -x /usr/local/go/bin/go ] && /usr/local/go/bin/go version | grep -q "go${GO_VERSION} "; then
    log "go: ${GO_VERSION} already installed"; return
  fi
  log "go: installing ${GO_VERSION}"
  curl -fsSL "https://go.dev/dl/go${GO_VERSION}.linux-amd64.tar.gz" -o /tmp/go.tgz
  rm -rf /usr/local/go && tar -C /usr/local -xzf /tmp/go.tgz && rm -f /tmp/go.tgz
  /usr/local/go/bin/go version
}

step_node() {
  if command -v node >/dev/null && node -v | grep -q "^v${NODE_MAJOR}\."; then
    log "node: $(node -v) already installed"; return
  fi
  log "node: installing ${NODE_MAJOR}.x from NodeSource"
  curl -fsSL "https://deb.nodesource.com/setup_${NODE_MAJOR}.x" | bash -
  apt-get install -y -q nodejs
  node -v
}

step_gascity() {
  local src="${GC_HOME_DIR}/src/gascity"
  GC_UID=$(id -u "${GC_USER}")
  if [ ! -d "${src}/.git" ]; then
    log "gascity: cloning ${GC_REPO_URL} at ${GC_COMMIT}"
    run_gc git init -q "${src}"
    run_gc git -C "${src}" remote add origin "${GC_REPO_URL}"
  fi
  if ! run_gc git -C "${src}" cat-file -e "${GC_COMMIT}^{commit}" 2>/dev/null; then
    run_gc git -C "${src}" fetch --depth 1 origin "${GC_COMMIT}" \
      || run_gc git -C "${src}" fetch origin
  fi
  run_gc git -C "${src}" checkout -q --detach "${GC_COMMIT}"
  local head; head=$(run_gc git -C "${src}" rev-parse HEAD)
  log "gascity: HEAD=${head}"

  # dolt + bd through upstream's own installers (pinned SHA-256 per version).
  if ! command -v dolt >/dev/null || ! dolt version | grep -q " ${DOLT_VERSION}$"; then
    log "gascity: installing dolt ${DOLT_VERSION}"
    DOLT_INSTALL_BIN_DIR=/usr/local/bin bash "${src}/.github/scripts/install-dolt-archive.sh" "${DOLT_VERSION}"
  fi
  if ! command -v bd >/dev/null || ! bd version | grep -q "${BD_VERSION#v}"; then
    log "gascity: installing bd ${BD_VERSION}"
    BD_INSTALL_BIN_DIR=/usr/local/bin bash "${src}/.github/scripts/install-bd-archive.sh" "${BD_VERSION}"
  fi

  local stamp="${GC_HOME_DIR}/.local/share/gc-build.txt"
  if [ -x "${GC_HOME_DIR}/.local/bin/gc" ] && grep -qs "^commit=${head}$" "${stamp}"; then
    log "gascity: gc already built from ${head}"; return
  fi
  log "gascity: building gc (go build ./cmd/gc) -- takes several minutes"
  run_gc env GOFLAGS=-trimpath GOTOOLCHAIN=local \
    bash -c "cd '${src}' && go build -o '${GC_HOME_DIR}/.local/bin/gc' ./cmd/gc" || fail "go build ./cmd/gc"
  install -d -o "${GC_USER}" -g "${GC_USER}" "$(dirname "${stamp}")"
  printf 'commit=%s\nbuilt=%s\ngo=%s\n' "${head}" "$(ts)" "$(/usr/local/go/bin/go version)" > "${stamp}"
  chown "${GC_USER}:${GC_USER}" "${stamp}"
  run_gc gc version
}

step_claude() {
  GC_UID=$(id -u "${GC_USER}")
  if [ -x "${GC_HOME_DIR}/.local/bin/claude" ]; then
    log "claude: already installed ($(run_gc claude --version 2>/dev/null | head -1))"; return
  fi
  log "claude: native install as ${GC_USER}"
  run_gc bash -c 'curl -fsSL https://claude.ai/install.sh | bash'
  run_gc claude --version
}

step_ziti() {
  install -d -m 0700 /var/lib/ziti
  if [ -x /usr/local/bin/ziti-edge-tunnel ] && /usr/local/bin/ziti-edge-tunnel version 2>/dev/null | grep -q "${ZITI_TUNNEL_VERSION}"; then
    log "ziti: tunnel ${ZITI_TUNNEL_VERSION} already installed"
  else
    log "ziti: installing ziti-edge-tunnel ${ZITI_TUNNEL_VERSION}"
    curl -fsSL "https://github.com/openziti/ziti-tunnel-sdk-c/releases/download/${ZITI_TUNNEL_VERSION}/ziti-edge-tunnel-Linux_x86_64.zip" -o /tmp/zet.zip
    rm -rf /tmp/zet && mkdir -p /tmp/zet && unzip -q /tmp/zet.zip -d /tmp/zet
    install -m 0755 /tmp/zet/ziti-edge-tunnel /usr/local/bin/ziti-edge-tunnel
    rm -rf /tmp/zet /tmp/zet.zip
  fi
  cat > /etc/systemd/system/ziti-edge-tunnel.service <<'EOF'
[Unit]
Description=OpenZiti edge tunnel (host mode; identities in /var/lib/ziti)
After=network-online.target
Wants=network-online.target
# No identity yet (pre-enrolment) -> stay inactive instead of crash-looping.
ConditionPathExistsGlob=/var/lib/ziti/*.json

[Service]
# run-host binds hosted services only: no TUN device, no resolver changes.
# Switch to `run` (TUN) when this host must *dial* ziti services too.
ExecStart=/usr/local/bin/ziti-edge-tunnel run-host -I /var/lib/ziti
Restart=always
RestartSec=5s

[Install]
WantedBy=multi-user.target
EOF
  systemctl daemon-reload
  systemctl enable ziti-edge-tunnel.service >/dev/null 2>&1
}

step_idle_stop() {
  log "idle-stop: installing hq-idle-stop + timer"
  cat > /usr/local/sbin/hq-idle-stop <<'EOF'
#!/bin/bash
# hq-idle-stop [check] -- power the HQ host off when nothing is using it.
#
# Every 5 minutes (systemd timer) this decides whether the host is "active":
#   * any gc session in state active (or running) in any running city
#     (`gc session list --json` as the gc user, per city from /v0/cities)
#   * an SSM Session Manager shell is open (ssm-session-worker process)
#   * a tmux client is attached for the gc user (someone is in a pane)
#   * hq-bootstrap is running
#   * the hold file /var/lib/hq/hold exists (operator opt-out)
# Idle means none of those. After HQ_IDLE_MINUTES (30) of consecutive idle
# checks, and only once uptime exceeds 20 minutes, it runs `systemctl
# poweroff`. The instance has instance_initiated_shutdown_behavior=stop, so
# poweroff == EC2 stop: compute billing ends, the EBS root persists.
# `check` prints the decision inputs as JSON and never powers off.
set -uo pipefail
STATE_DIR=/run/hq-idle-stop
HOLD_FILE=/var/lib/hq/hold
GC_USER=${GC_USER:-gc}
GC_HOME_DIR=/home/${GC_USER}
IDLE_MINUTES=${HQ_IDLE_MINUTES:-30}
MIN_UPTIME_MINUTES=${HQ_MIN_UPTIME_MINUTES:-20}
SUPERVISOR_URL=${HQ_SUPERVISOR_URL:-http://127.0.0.1:8372}
mkdir -p "${STATE_DIR}"

# Under systemd stdout already lands in the journal; echo only when run by hand.
log() { logger -t hq-idle-stop -- "$*"; [ -n "${INVOCATION_ID:-}" ] || echo "hq-idle-stop: $*"; }
as_gc() { sudo -u "${GC_USER}" -H env PATH="${GC_HOME_DIR}/.local/bin:/usr/local/bin:/usr/bin:/bin" HOME="${GC_HOME_DIR}" "$@"; }

uptime_min=$(( $(cut -d. -f1 /proc/uptime) / 60 ))
ssm_session=false; pgrep -f '[s]sm-session-worker' >/dev/null && ssm_session=true
bootstrap=false; pgrep -f '[h]q-bootstrap' >/dev/null && bootstrap=true
hold=false; [ -e "${HOLD_FILE}" ] && hold=true
tmux_attached=0
tmux_attached=$(as_gc tmux list-clients 2>/dev/null | wc -l | tr -d ' ')

# gc sessions: per running city, count state=="active" or running==true.
# unknown=true when the supervisor answered but a city query failed; that is
# treated as active (never power off on a guess).
sessions=0; unknown=false
cities=$(curl -sf --max-time 5 "${SUPERVISOR_URL}/v0/cities" 2>/dev/null | jq -r '.items[]? | select(.running) | .path' 2>/dev/null || true)
for city in ${cities}; do
  out=$(as_gc bash -c "cd '${city}' && timeout 25 gc session list --json --state all" 2>/dev/null) || { unknown=true; continue; }
  n=$(printf '%s' "${out}" | jq '[.sessions[]? | select(.state == "active" or .running == true)] | length' 2>/dev/null) || { unknown=true; continue; }
  sessions=$(( sessions + n ))
done

active=false
if [ "${sessions}" -gt 0 ] || [ "${unknown}" = true ] || [ "${ssm_session}" = true ] || \
   [ "${tmux_attached}" -gt 0 ] || [ "${bootstrap}" = true ] || [ "${hold}" = true ]; then
  active=true
fi

if [ "${active}" = true ]; then
  rm -f "${STATE_DIR}/idle_since"
  idle_min=0
else
  [ -f "${STATE_DIR}/idle_since" ] || date +%s > "${STATE_DIR}/idle_since"
  idle_min=$(( ( $(date +%s) - $(cat "${STATE_DIR}/idle_since") ) / 60 ))
fi

if [ "${1:-}" = "check" ]; then
  jq -n --argjson s "${sessions}" --argjson u "${unknown}" --argjson ssm "${ssm_session}" \
    --argjson tm "${tmux_attached}" --argjson b "${bootstrap}" --argjson h "${hold}" \
    --argjson up "${uptime_min}" --argjson idle "${idle_min}" --argjson a "${active}" \
    --argjson lim "${IDLE_MINUTES}" \
    '{active:$a, sessions:$s, sessions_unknown:$u, ssm_session:$ssm, tmux_clients:$tm, bootstrap_running:$b, hold:$h, uptime_min:$up, idle_min:$idle, idle_limit_min:$lim}'
  exit 0
fi

if [ "${active}" = true ]; then
  log "sessions=${sessions} unknown=${unknown} ssm=${ssm_session} tmux_clients=${tmux_attached} bootstrap=${bootstrap} hold=${hold}: active, not stopping"
  exit 0
fi
if [ "${uptime_min}" -lt "${MIN_UPTIME_MINUTES}" ]; then
  log "sessions=0, idle for ${idle_min}m, uptime ${uptime_min}m < ${MIN_UPTIME_MINUTES}m: too early to stop"
  exit 0
fi
if [ "${idle_min}" -ge "${IDLE_MINUTES}" ]; then
  log "sessions=0, idle for ${idle_min}m >= ${IDLE_MINUTES}m, uptime ${uptime_min}m: powering off (EC2 stop)"
  # Last consistent Dolt backup before the stop, so a stop never loses more
  # than the last few minutes of work. A failure is loud but never blocks
  # the poweroff (the EBS root persists either way).
  if [ -x /usr/local/bin/gc-dolt-backup ]; then
    if timeout 900 /usr/local/bin/gc-dolt-backup >/dev/null 2>&1; then
      log "pre-stop dolt backup ok: $(cat /var/lib/hq/last-backup 2>/dev/null)"
    else
      logger -t hq-idle-stop -p user.err -- "PRE-STOP DOLT BACKUP FAILED (rc=$?), powering off anyway: $(cat /var/lib/hq/last-backup 2>/dev/null)"
      [ -n "${INVOCATION_ID:-}" ] || echo "hq-idle-stop: PRE-STOP DOLT BACKUP FAILED" >&2
    fi
  fi
  systemctl poweroff
  exit 0
fi
log "sessions=0, idle for ${idle_min}m (limit ${IDLE_MINUTES}m, uptime ${uptime_min}m)"
EOF
  chmod 0755 /usr/local/sbin/hq-idle-stop
  cat > /etc/systemd/system/hq-idle-stop.service <<'EOF'
[Unit]
Description=Power off the HQ host when idle (see hq-idle-stop)

[Service]
Type=oneshot
ExecStart=/usr/local/sbin/hq-idle-stop
EOF
  cat > /etc/systemd/system/hq-idle-stop.timer <<'EOF'
[Unit]
Description=Idle check every 5 minutes for the HQ host

[Timer]
OnBootSec=5min
OnUnitActiveSec=5min
AccuracySec=30s

[Install]
WantedBy=timers.target
EOF
  systemctl daemon-reload
  systemctl enable --now hq-idle-stop.timer >/dev/null 2>&1
}

step_backup() {
  log "backup: installing gc-dolt-backup + daily timer"
  cat > /usr/local/bin/gc-dolt-backup <<'EOF'
#!/bin/bash
# gc-dolt-backup -- consistent backup of every database on the city's managed
# Dolt server, mirrored to S3. Root runs it (timer daily 02:30 UTC, 10 min
# after boot, and hq-idle-stop right before poweroff); the Dolt and S3 work
# runs as the gc user.
#
#   1. port from <city>/.gc/runtime/packs/dolt/dolt-state.json (read only)
#   2. `show databases` minus information_schema/mysql
#   3. per db: CALL DOLT_BACKUP('add', NAME, file://<root>/<db>) if missing,
#      then CALL DOLT_BACKUP('sync', NAME)  -- online, consistent, incremental
#   4. aws s3 sync <root> s3://<bucket>/dolt/ --delete (instance role creds)
#   5. one line to /var/lib/hq/last-backup:
#        <utc ts> <ok|fail> dbs=<a,b> bytes=<n> <detail>
# Exit non-zero on any failure. `gc doctor bd-backup-freshness` knows nothing
# about this; the status file (scripts/hq backup-status) is the real signal.
set -uo pipefail
GC_USER=${GC_USER:-gc}
GC_HOME_DIR=/home/${GC_USER}
CITY=${GC_CITY:-${GC_HOME_DIR}/gascity-test}
STATE_JSON=${CITY}/.gc/runtime/packs/dolt/dolt-state.json
BACKUP_ROOT=${GC_BACKUP_ROOT:-${GC_HOME_DIR}/backups/dolt}
STATUS_FILE=${GC_BACKUP_STATUS:-/var/lib/hq/last-backup}
BACKUP_NAME=${GC_BACKUP_NAME:-hq-local}
export AWS_PAGER=""

imds() {
  local tok
  tok=$(curl -sf -m 3 -X PUT http://169.254.169.254/latest/api/token -H "X-aws-ec2-metadata-token-ttl-seconds: 60") || return 1
  curl -sf -m 3 -H "X-aws-ec2-metadata-token: ${tok}" "http://169.254.169.254/latest/$1"
}
if [ -z "${GC_BACKUP_BUCKET:-}" ]; then
  account=$(imds dynamic/instance-identity/document | jq -r .accountId 2>/dev/null)
  GC_BACKUP_BUCKET="${HQ_ENV_NAME:-gardenio}-hq-backups-${account:-unknown}"
fi
export AWS_DEFAULT_REGION=${AWS_DEFAULT_REGION:-$(imds meta-data/placement/region || echo us-east-1)}

as_gc() {
  if [ "$(id -un)" = "${GC_USER}" ]; then env "$@"
  else sudo -u "${GC_USER}" -H env HOME="${GC_HOME_DIR}" "$@"; fi
}
# dolt CLI against the running server (no repo dir needed). DOLT_CLI_PASSWORD
# must be set (empty) or the CLI prompts and hangs without a tty.
dsql() { # dsql DB QUERY
  as_gc DOLT_CLI_PASSWORD="" AWS_DEFAULT_REGION="${AWS_DEFAULT_REGION}" \
    dolt --host 127.0.0.1 --port "${PORT}" --user root --no-tls --use-db "$1" sql -r csv -q "$2"
}
log() { logger -t gc-dolt-backup -- "$*"; [ -n "${INVOCATION_ID:-}" ] || echo "gc-dolt-backup: $*"; }
status() { # status ok|fail detail
  local line
  line="$(date -u +%Y-%m-%dT%H:%M:%SZ) $1 dbs=${DBS_CSV:-none} bytes=${BYTES:-0} $2"
  if ! printf '%s\n' "${line}" > "${STATUS_FILE}" 2>/dev/null; then log "cannot write ${STATUS_FILE}"; fi
  if [ "$1" = ok ]; then log "${line}"; else logger -t gc-dolt-backup -p user.err -- "${line}"; [ -n "${INVOCATION_ID:-}" ] || echo "gc-dolt-backup: ${line}" >&2; fi
}
fail() { status fail "$*"; exit 1; }

DBS_CSV=""; BYTES=0
# One at a time: the timer, hq-idle-stop and a hand run may overlap.
exec 9>/run/lock/gc-dolt-backup
flock -w 600 9 || fail "another gc-dolt-backup has held the lock for 10 minutes"
[ -r "${STATE_JSON}" ] || fail "no ${STATE_JSON} (city not running?)"
PORT=$(jq -r '.port // empty' "${STATE_JSON}") || fail "cannot parse ${STATE_JSON}"
[ -n "${PORT}" ] || fail "no port in ${STATE_JSON}"
[ "$(jq -r .running "${STATE_JSON}")" = true ] || log "warning: dolt-state.json says running=false, trying port ${PORT} anyway"

dbs=$(dsql information_schema "show databases" 2>&1 | tail -n +2 | grep -Evx 'information_schema|mysql' ) || fail "show databases on :${PORT} failed: ${dbs}"
[ -n "${dbs}" ] || fail "no databases on :${PORT}"
DBS_CSV=$(printf '%s' "${dbs}" | paste -sd,)

as_gc install -d -m 0700 "${GC_HOME_DIR}/backups" "${BACKUP_ROOT}" || fail "cannot create ${BACKUP_ROOT}"
failed=""
for db in ${dbs}; do
  url="file://${BACKUP_ROOT}/${db}"
  have=$(dsql "${db}" "select url from dolt_backups where name = '${BACKUP_NAME}'" 2>/dev/null | tail -n +2)
  if [ -z "${have}" ]; then
    out=$(dsql "${db}" "CALL DOLT_BACKUP('add','${BACKUP_NAME}','${url}')" 2>&1) || case "${out}" in
      *"already exists"*) ;;
      *) log "${db}: backup add failed: ${out}"; failed="${failed} ${db}"; continue ;;
    esac
  elif [ "${have}" != "${url}" ]; then
    log "${db}: backup ${BACKUP_NAME} points at ${have}, expected ${url}"; failed="${failed} ${db}"; continue
  fi
  out=$(dsql "${db}" "CALL DOLT_BACKUP('sync','${BACKUP_NAME}')" 2>&1) || { log "${db}: backup sync failed: ${out}"; failed="${failed} ${db}"; continue; }
  log "${db}: synced to ${url}"
done
BYTES=$(du -sb "${BACKUP_ROOT}" 2>/dev/null | cut -f1)
[ -z "${failed}" ] || fail "dolt backup failed for:${failed}"

out=$(as_gc AWS_DEFAULT_REGION="${AWS_DEFAULT_REGION}" aws s3 sync "${BACKUP_ROOT}" "s3://${GC_BACKUP_BUCKET}/dolt/" --delete --only-show-errors 2>&1) \
  || fail "s3 sync to s3://${GC_BACKUP_BUCKET}/dolt/ failed: ${out}"
status ok "s3=s3://${GC_BACKUP_BUCKET}/dolt/"
EOF
  chmod 0755 /usr/local/bin/gc-dolt-backup
  install -d -m 0755 /var/lib/hq
  cat > /etc/systemd/system/gc-dolt-backup.service <<'EOF'
[Unit]
Description=Dolt backup of the Gas City HQ databases to local disk and S3
After=network-online.target
Wants=network-online.target

[Service]
Type=oneshot
ExecStart=/usr/local/bin/gc-dolt-backup
TimeoutStartSec=15min
EOF
  cat > /etc/systemd/system/gc-dolt-backup.timer <<'EOF'
[Unit]
Description=Daily 02:30 UTC (and 10 min after boot) Dolt backup of the HQ city

[Timer]
OnCalendar=*-*-* 02:30:00 UTC
OnBootSec=10min
Persistent=true
AccuracySec=1min

[Install]
WantedBy=timers.target
EOF
  systemctl daemon-reload
  systemctl enable --now gc-dolt-backup.timer >/dev/null 2>&1
}

step_ziti_enroll() {
  local ident="/var/lib/ziti/${ZITI_IDENTITY_NAME}.json" jwt
  if [ -s "${ident}" ]; then
    log "ziti-enroll: ${ident} exists, ensuring the service is up"
    systemctl start ziti-edge-tunnel.service; return
  fi
  if ! jwt=$(ssm_get "/${ENV_NAME}/hq/ziti-enroll-jwt") || [ -z "${jwt}" ]; then
    log "ziti-enroll: no /${ENV_NAME}/hq/ziti-enroll-jwt parameter; skipping"; return
  fi
  log "ziti-enroll: enrolling ${ZITI_IDENTITY_NAME}"
  local tmp; tmp=$(mktemp)
  printf '%s' "${jwt}" > "${tmp}"
  if ziti-edge-tunnel enroll -j "${tmp}" -i "${ident}"; then
    chmod 0600 "${ident}"
    systemctl restart ziti-edge-tunnel.service
    log "ziti-enroll: enrolled; delete the SSM parameter now (one-time JWT)"
  else
    rm -f "${ident}"; log "ziti-enroll: enrolment FAILED"
  fi
  rm -f "${tmp}"
}

step_claude_creds() {
  local dst="${GC_HOME_DIR}/.claude/.credentials.json" val
  if ! val=$(ssm_get "/${ENV_NAME}/hq/claude-credentials") || [ -z "${val}" ]; then
    log "claude-creds: no /${ENV_NAME}/hq/claude-credentials parameter; skipping"; return
  fi
  install -d -o "${GC_USER}" -g "${GC_USER}" -m 0700 "$(dirname "${dst}")"
  printf '%s\n' "${val}" > "${dst}"
  chown "${GC_USER}:${GC_USER}" "${dst}"; chmod 0600 "${dst}"
  log "claude-creds: installed ${dst} (delete the SSM parameter now)"
}

step_supervisor() {
  GC_UID=$(id -u "${GC_USER}")
  if [ ! -x "${GC_HOME_DIR}/.local/bin/gc" ]; then
    log "supervisor: gc not built yet; skipping"; return
  fi
  local toml="${GC_HOME_DIR}/.gc/supervisor.toml"
  if [ ! -f "${toml}" ]; then
    cat > "${toml}" <<'EOF'
# Machine-wide Gas City supervisor config (GC_HOME=/home/gc/.gc).
# Only allowed_hosts is set: bind stays 127.0.0.1 and port stays 8372
# (internal/supervisor/config.go BindOrDefault/PortOrDefault). The dashboard
# is reached over the ziti overlay as gc.dashboard.ziti; without this entry
# every non-loopback Host header is answered 421 Misdirected Request.
[supervisor]
allowed_hosts = ["gc.dashboard.ziti", "gc.dashboard.ziti:8372"]
EOF
    chown "${GC_USER}:${GC_USER}" "${toml}"; chmod 0600 "${toml}"
  fi
  if run_gc systemctl --user is-enabled gascity-supervisor.service >/dev/null 2>&1; then
    log "supervisor: unit already installed; (re)starting"
    run_gc systemctl --user start gascity-supervisor.service
  else
    log "supervisor: gc supervisor install"
    run_gc env GC_DISABLE_USAGE_METRICS=1 gc supervisor install || fail "gc supervisor install"
  fi
  sleep 3
  run_gc systemctl --user --no-pager status gascity-supervisor.service | head -5
  curl -sf -H 'Host: gc.dashboard.ziti' http://127.0.0.1:8372/v0/cities && echo
}

main() {
  local steps=("$@")
  [ ${#steps[@]} -eq 0 ] && steps=(all)
  [ "${steps[0]}" = all ] && steps=(base go node gascity claude ziti idle-stop backup ziti-enroll claude-creds supervisor)
  log "start: ${steps[*]} (pins: go=${GO_VERSION} node=${NODE_MAJOR} dolt=${DOLT_VERSION} bd=${BD_VERSION} gc=${GC_COMMIT} zet=${ZITI_TUNNEL_VERSION})"
  id "${GC_USER}" >/dev/null 2>&1 && GC_UID=$(id -u "${GC_USER}")
  for s in "${steps[@]}"; do
    case "${s}" in
      base) step_base ;;
      go) step_go ;;
      node) step_node ;;
      gascity) step_gascity ;;
      claude) step_claude ;;
      ziti) step_ziti ;;
      idle-stop) step_idle_stop ;;
      backup) step_backup ;;
      ziti-enroll) step_ziti_enroll ;;
      claude-creds) step_claude_creds ;;
      supervisor) step_supervisor ;;
      *) fail "unknown step ${s}" ;;
    esac
  done
  log "done: ${steps[*]}"
}
main "$@"
