#!/usr/bin/env bash
# tarboat-provision-box.sh — Test-in-box E4 (ka-0000002700), epic ka-0000003645.
# Operator: Matt Baker (oidc:matthew.baker@acme.example).
#
# Stand up — and tear down — the FRESH ephemeral EC2 box that the standing
# Phase-B regression sweep validates. This is the front door of the suite:
#
#     provision  →  tarboat-validate.sh  →  tarboat-file-regressions.sh  →  teardown
#       (E4)              (E1)                      (E2)                      (E4)
#
# Phase A (E1 validate + E2 file-regressions, both merged) runs the four gates
# against an ALREADY-provisioned box — acceptance-smoke.sh itself says "It does
# NOT launch the instance — provisioning is an operator action." That operator
# step (spend / account / EIP / SSH key) is exactly what blocks a *standing*,
# unattended Phase-B sweep from standing up its own box on a cadence. E4 wires
# that missing step: a non-interactive provisioner the gardener / CI cron drives.
#
# It is the EC2-box-level analogue of the namespace-ephemeral lifecycle the
# RWX-era release pipeline had: provision a per-instance namespace, tear it
# down, and run a reaper CronJob as the periodic safety net. This script gives the
# boxed artifact the same three verbs — `provision`, `teardown`, `reap` — and
# the same `lifecycle=keep` exemption contract, so a sweep never leaks a box.
#
# WHERE THIS RUNS: on the ORCHESTRATION side (the gardener / CI host where the
# AWS CLI + credentials live), NOT on the air-gapped box. Like the E2 filer it
# leans on tooling that only the orchestration host has — here, `aws`. It does
# NOT need jq (the descriptor is emitted by hand, the way tarboat-validate.sh
# builds its results doc) so it runs anywhere the AWS CLI is configured.
#
# It reuses, never rebuilds, the launch contract already documented in
# gardenio/docs/gardenio-in-a-box-install.md §A.3: render-user-data.sh emits the
# #cloud-config, `aws ec2 run-instances ... --user-data` launches it, and
# `aws ec2 associate-address` attaches the EIP whose dashed form is the sslip.io
# apex. The provisioner just sequences those calls, tags the box for the reaper,
# waits for the edge to answer, and emits a machine-readable descriptor.
#
# ── SUBCOMMANDS ──────────────────────────────────────────────────────────────
#
#   provision [flags]              launch a fresh box, wait for its edge, emit a
#                                  tarboat-box/v1 descriptor for tarboat-validate.
#   teardown  (DESCRIPTOR|-|flags) terminate the instance + release its EIP.
#   reap      [flags]              terminate ephemeral-tagged boxes past expiry —
#                                  the periodic safety net (box-level reaper).
#
# ── provision flags ──────────────────────────────────────────────────────────
#   --calver VER        box image/calver, stamped into the descriptor so E2 can
#                       record which build regressed (also tagged on the box).
#   --region R          AWS region (default $AWS_REGION / $AWS_DEFAULT_REGION).
#   --ami ID            AMI id (AL2023 / Ubuntu). Default from $TARBOAT_AMI.
#   --instance-type T   default r7i.2xlarge (the §A.1 RAM-bound default).
#   --subnet-id ID      subnet to launch in (default $TARBOAT_SUBNET_ID).
#   --security-group-ids "sg-a sg-b"   the 22/80/443-only SG(s)
#                       (default $TARBOAT_SECURITY_GROUP_IDS).
#   --key-name NAME     EC2 keypair for operator SSH (default $TARBOAT_KEY_NAME).
#   --volume-size GiB   root gp3 size (default 150).
#   --namespace NS      gardenio namespace on the box (default gardenio).
#   --scenario A|B|C    sizing scenario passed to render-user-data (default A).
#   --harden 0|1        host hardening (default 1 — public-safe).
#   --app-version TAG   pin the chart appVersion (default = --calver).
#   --extra-values "F1 F2"  values overlays for render-user-data (default the two
#                       in-box overlays — the public-safe layering).
#   --offline-assets DIR    tarball offline-assets dir (true air-gap; passed
#                       through to render-user-data --offline-assets).
#   --repo-token TOK    read-only clone token for the PRIVATE repo (or env
#                       GARDENIO_REPO_TOKEN); unneeded in the air-gap/AMI path.
#   --allocation-id ID  reuse a pre-allocated EIP instead of allocating one.
#   --no-eip            do not allocate/associate an EIP (use the box's public IP).
#   --ttl-minutes N     stamp an expiry tag N minutes out for `reap` (default 120).
#   --keep              tag lifecycle=keep so `reap` never touches this box.
#   --wait-timeout S    seconds to wait for the box's edge to answer (default
#                       1800 = 30m; cloud-init + bootstrap is minutes). 0 = no wait.
#   --descriptor PATH   write the tarboat-box/v1 descriptor here ("-" = stdout,
#                       the default). Feed it to teardown and to tarboat-validate.
#   --user-data FILE    use a pre-rendered user-data file instead of invoking
#                       render-user-data.sh (advanced / testing).
#   --dry-run           print the AWS calls that WOULD run; launch nothing.
#
# ── teardown ─────────────────────────────────────────────────────────────────
#   teardown DESCRIPTOR_JSON     read instance-id/allocation-id/region from a
#                                tarboat-box/v1 descriptor ("-" = stdin), or:
#   teardown --instance-id ID [--allocation-id AID] [--region R] [--dry-run]
#   Terminates the instance and (if an allocation-id is known and was allocated
#   by us, or --release-eip is given) releases the EIP. Honors lifecycle=keep:
#   a box tagged keep is refused unless --force.
#
# ── reap ─────────────────────────────────────────────────────────────────────
#   reap [--region R] [--ttl-minutes N] [--dry-run] [--release-eip]
#   Terminates every running/pending box tagged gardenio.io/tarboat-ephemeral=true
#   whose gardenio.io/expiry is in the past (or, with --ttl-minutes, older than N
#   minutes by launch time), skipping any tagged lifecycle=keep. Same contract as
#   the namespace reaper: keep-labeled survive, everything stale is collected.
#
# ── EXIT STATUS ──────────────────────────────────────────────────────────────
#   0  provision: box up + descriptor emitted. teardown/reap: done (incl. no-op).
#   1  an AWS call failed, or the box never came up within --wait-timeout.
#   2  usage / precondition error (missing required flag, bad subcommand).
#
# RESULTS SCHEMA (tarboat-box/v1):
#   { "schema": "tarboat-box/v1", "instance_id", "region", "ip", "dashed_ip",
#     "apex", "hosts", "namespace", "calver", "app_version", "allocation_id",
#     "eip_allocated_by_us" (bool), "ssh_user", "instance_type", "scenario",
#     "ttl_minutes", "expiry_epoch", "started_at", "ready_at", "ready" (bool) }
#   `apex` is <dashed-ip>.sslip.io; `hosts` is the space-joined sslip.io host set
#   acceptance-smoke / tarboat-validate expect. Pass `ip`+`namespace` straight to
#   `tarboat-validate.sh --ip <ip> --namespace <namespace>`.

set -uo pipefail

# ── shared helpers ───────────────────────────────────────────────────────────
log()  { printf '\033[1;36m[provision]\033[0m %s\n' "$*" >&2; }
warn() { printf '\033[1;33m[provision] WARN:\033[0m %s\n' "$*" >&2; }
err()  { printf '\033[1;31m[provision] ERROR:\033[0m %s\n' "$*" >&2; }
die()  { err "$*"; exit 1; }
die2() { err "$*"; exit 2; }

SELF_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
RENDER_USER_DATA="$SELF_ROOT/scripts/render-user-data.sh"

# Tag keys — the box-level mirror of the namespace reaper's labels.
TAG_EPHEMERAL="gardenio.io/tarboat-ephemeral"
TAG_CALVER="gardenio.io/calver"
TAG_EXPIRY="gardenio.io/expiry"            # unix epoch seconds; box is reapable past it
TAG_LIFECYCLE="lifecycle"                 # value "keep" exempts from reap, like the ns reaper

# now_epoch — seconds since epoch. Factored out so the test can stub a clock.
now_epoch() { date +%s; }
iso_now()   { date -u +%Y-%m-%dT%H:%M:%SZ; }

# dash_ip 203.0.113.7 -> 203-0-113-7   (sslip.io apex form, per §A.4 / acceptance-smoke)
dash_ip() { printf '%s' "$1" | tr '.' '-'; }

# aws_cli — single chokepoint for every AWS call so --dry-run and the unit-test
# stub both intercept here. In dry-run we echo the call and succeed.
DRY_RUN=0
REGION=""
aws_cli() {
  local args=()
  [ -n "$REGION" ] && args+=(--region "$REGION")
  if [ "$DRY_RUN" = 1 ]; then
    printf '[dry-run] aws %s %s\n' "${args[*]}" "$*" >&2
    return 0
  fi
  command aws "${args[@]}" "$@"
}

# ── JSON descriptor emission (jq-free; mirrors tarboat-validate.sh) ───────────
# json_str — escape a value for embedding in a JSON string.
json_str() {
  local s="$1"
  s="${s//\\/\\\\}"; s="${s//\"/\\\"}"
  s="${s//$'\n'/\\n}"; s="${s//$'\t'/\\t}"; s="${s//$'\r'/}"
  printf '%s' "$s"
}

# ── tiny field reader for a tarboat-box/v1 descriptor (jq-free) ───────────────
# Reads a flat "key": "value" or "key": value (number/bool) from a descriptor.
# Used by teardown when handed a descriptor instead of explicit flags. We keep
# this deliberately small — descriptors are emitted by this same script, flat.
desc_get() {
  local doc="$1" key="$2" v
  # string form:  "key": "value"
  v="$(printf '%s' "$doc" | sed -n "s/.*\"$key\"[[:space:]]*:[[:space:]]*\"\\([^\"]*\\)\".*/\\1/p" | head -n1)"
  if [ -n "$v" ]; then printf '%s' "$v"; return 0; fi
  # scalar form:  "key": value   (number / true / false / null)
  v="$(printf '%s' "$doc" | sed -n "s/.*\"$key\"[[:space:]]*:[[:space:]]*\\([^,}\"[:space:]]*\\).*/\\1/p" | head -n1)"
  printf '%s' "$v"
}

# =============================================================================
# provision
# =============================================================================
cmd_provision() {
  local CALVER="" AMI="${TARBOAT_AMI:-}" INSTANCE_TYPE="r7i.2xlarge"
  local SUBNET_ID="${TARBOAT_SUBNET_ID:-}" SG_IDS="${TARBOAT_SECURITY_GROUP_IDS:-}"
  local KEY_NAME="${TARBOAT_KEY_NAME:-}" VOLUME_SIZE="150" NAMESPACE="gardenio"
  local SCENARIO="A" HARDEN="1" APP_VERSION="" EXTRA_VALUES="" OFFLINE_ASSETS=""
  local REPO_TOKEN="${GARDENIO_REPO_TOKEN:-}" ALLOCATION_ID="" WANT_EIP=1
  local TTL_MINUTES="120" KEEP=0 WAIT_TIMEOUT="1800" DESCRIPTOR="-"
  local USER_DATA_FILE="" SSH_USER="gardenio-op"
  local DEFAULT_EXTRA_VALUES="gardenio/helm/gardenio/values-inbox.yaml deploy-values/gardenio-inbox.yaml"

  while [ "$#" -gt 0 ]; do
    case "$1" in
      --calver) CALVER="$2"; shift 2;;
      --region) REGION="$2"; shift 2;;
      --ami) AMI="$2"; shift 2;;
      --instance-type) INSTANCE_TYPE="$2"; shift 2;;
      --subnet-id) SUBNET_ID="$2"; shift 2;;
      --security-group-ids) SG_IDS="$2"; shift 2;;
      --key-name) KEY_NAME="$2"; shift 2;;
      --volume-size) VOLUME_SIZE="$2"; shift 2;;
      --namespace) NAMESPACE="$2"; shift 2;;
      --scenario) SCENARIO="$2"; shift 2;;
      --harden) HARDEN="$2"; shift 2;;
      --app-version) APP_VERSION="$2"; shift 2;;
      --extra-values) EXTRA_VALUES="$2"; shift 2;;
      --offline-assets) OFFLINE_ASSETS="$2"; shift 2;;
      --repo-token) REPO_TOKEN="$2"; shift 2;;
      --allocation-id) ALLOCATION_ID="$2"; shift 2;;
      --no-eip) WANT_EIP=0; shift;;
      --ttl-minutes) TTL_MINUTES="$2"; shift 2;;
      --keep) KEEP=1; shift;;
      --wait-timeout) WAIT_TIMEOUT="$2"; shift 2;;
      --descriptor) DESCRIPTOR="$2"; shift 2;;
      --user-data) USER_DATA_FILE="$2"; shift 2;;
      --ssh-user) SSH_USER="$2"; shift 2;;
      --dry-run) DRY_RUN=1; shift;;
      -h|--help) sed -n '2,120p' "$0" | sed 's/^# \{0,1\}//'; exit 0;;
      *) die2 "provision: unknown flag: $1";;
    esac
  done

  [ -n "$CALVER" ] || die2 "provision: --calver is required (the box image/calver, for the descriptor + tags)"
  [ "$APP_VERSION" != "" ] || APP_VERSION="$CALVER"
  [ "$EXTRA_VALUES" != "" ] || EXTRA_VALUES="$DEFAULT_EXTRA_VALUES"

  if [ "$DRY_RUN" != 1 ]; then
    command -v aws >/dev/null 2>&1 || die2 "provision: aws CLI not found on PATH (this runs on the orchestration host)"
    [ -n "$AMI" ]     || die2 "provision: --ami (or \$TARBOAT_AMI) is required"
    [ -n "$SUBNET_ID" ] || die2 "provision: --subnet-id (or \$TARBOAT_SUBNET_ID) is required"
    [ -n "$SG_IDS" ]  || die2 "provision: --security-group-ids (or \$TARBOAT_SECURITY_GROUP_IDS) is required"
  fi

  # 1. Render the #cloud-config user-data (reuse render-user-data.sh — §A.2/§A.3).
  local user_data
  if [ -n "$USER_DATA_FILE" ]; then
    [ -r "$USER_DATA_FILE" ] || die2 "provision: --user-data file not readable: $USER_DATA_FILE"
    user_data="$USER_DATA_FILE"
    log "using pre-rendered user-data: $user_data"
  else
    # Require only readability and invoke through `bash` (the renderer is a
    # bash script, so it needs no exec bit). This keeps provision robust to a
    # CI checkout that does not preserve mode bits. NOTE: the actual CI
    # failure was the renderer being ABSENT (the RWX-era test-scripts task's
    # file filter didn't stage it), not a stripped exec bit; this guard is
    # defensive hygiene. (ka-0000004694.)
    [ -r "$RENDER_USER_DATA" ] || die "provision: render-user-data.sh not found/readable at $RENDER_USER_DATA"
    user_data="$(mktemp /tmp/tarboat-user-data.XXXXXX.yaml)"
    local rargs=(--scenario "$SCENARIO" --harden "$HARDEN" --namespace "$NAMESPACE"
                 --app-version "$APP_VERSION" --extra-values "$EXTRA_VALUES"
                 --operator-user "$SSH_USER")
    [ -n "$OFFLINE_ASSETS" ] && rargs+=(--offline-assets "$OFFLINE_ASSETS")
    log "rendering user-data (scenario=$SCENARIO harden=$HARDEN app-version=$APP_VERSION)"
    # Pass the clone token via env so it never lands in `ps`/xtrace.
    if ! GARDENIO_REPO_TOKEN="$REPO_TOKEN" bash "$RENDER_USER_DATA" "${rargs[@]}" > "$user_data"; then
      rm -f "$user_data"
      die "provision: render-user-data.sh failed"
    fi
  fi

  # 2. Allocate an EIP up front (so the SANs/hosts are known before launch) unless
  #    one was supplied or --no-eip. We record whether WE allocated it so teardown
  #    only releases EIPs it owns.
  local eip_allocated_by_us=0 box_ip="" dashed="" apex="" hosts=""
  if [ "$WANT_EIP" = 1 ] && [ -z "$ALLOCATION_ID" ]; then
    log "allocating Elastic IP"
    local alloc_out
    alloc_out="$(aws_cli ec2 allocate-address --domain vpc \
        --query '[AllocationId,PublicIp]' --output text)" \
      || die "provision: allocate-address failed"
    if [ "$DRY_RUN" = 1 ]; then
      ALLOCATION_ID="eipalloc-DRYRUN"; box_ip="203.0.113.7"
    else
      ALLOCATION_ID="$(printf '%s' "$alloc_out" | awk '{print $1}')"
      box_ip="$(printf '%s' "$alloc_out" | awk '{print $2}')"
    fi
    eip_allocated_by_us=1
    log "allocated EIP $box_ip ($ALLOCATION_ID)"
  elif [ -n "$ALLOCATION_ID" ]; then
    log "reusing pre-allocated EIP $ALLOCATION_ID"
    if [ "$DRY_RUN" = 1 ]; then
      box_ip="203.0.113.7"
    else
      box_ip="$(aws_cli ec2 describe-addresses --allocation-ids "$ALLOCATION_ID" \
          --query 'Addresses[0].PublicIp' --output text)" \
        || die "provision: describe-addresses for $ALLOCATION_ID failed"
    fi
  fi

  # 3. Build the tag spec. The reaper keys on gardenio.io/tarboat-ephemeral=true +
  #    gardenio.io/expiry; lifecycle=keep exempts (same contract as the ns reaper).
  local started_epoch expiry_epoch
  started_epoch="$(now_epoch)"
  expiry_epoch=$((started_epoch + TTL_MINUTES * 60))
  local name_tag="gardenio-tarboat-${CALVER}-$$"
  local lifecycle_val="ephemeral"
  [ "$KEEP" = 1 ] && lifecycle_val="keep"
  local tag_spec
  tag_spec="ResourceType=instance,Tags=["
  tag_spec+="{Key=Name,Value=${name_tag}},"
  tag_spec+="{Key=${TAG_EPHEMERAL},Value=true},"
  tag_spec+="{Key=${TAG_CALVER},Value=${CALVER}},"
  tag_spec+="{Key=${TAG_EXPIRY},Value=${expiry_epoch}},"
  tag_spec+="{Key=${TAG_LIFECYCLE},Value=${lifecycle_val}}]"

  # 4. Launch (run-instances) — exactly the §A.3 call, IMDSv2-locked.
  log "launching $INSTANCE_TYPE (ami=$AMI subnet=$SUBNET_ID)"
  # --security-group-ids takes a space-separated list; split SG_IDS robustly.
  local sg_arr=(); read -r -a sg_arr <<< "$SG_IDS"
  local run_args=(ec2 run-instances
    --image-id "$AMI"
    --instance-type "$INSTANCE_TYPE"
    --subnet-id "$SUBNET_ID"
    --security-group-ids "${sg_arr[@]}"
    --block-device-mappings "DeviceName=/dev/xvda,Ebs={VolumeSize=${VOLUME_SIZE},VolumeType=gp3,Encrypted=true}"
    --metadata-options "HttpTokens=required,HttpPutResponseHopLimit=1"
    --tag-specifications "$tag_spec"
    --user-data "file://${user_data}"
    --query 'Instances[0].InstanceId' --output text)
  [ -n "$KEY_NAME" ] && run_args+=(--key-name "$KEY_NAME")

  local instance_id
  instance_id="$(aws_cli "${run_args[@]}")" || {
    # Best-effort: don't strand an EIP we just allocated if the launch failed.
    if [ "$eip_allocated_by_us" = 1 ]; then
      warn "launch failed; releasing the EIP we allocated ($ALLOCATION_ID)"
      aws_cli ec2 release-address --allocation-id "$ALLOCATION_ID" >/dev/null 2>&1 || true
    fi
    die "provision: run-instances failed"
  }
  [ "$DRY_RUN" = 1 ] && instance_id="i-DRYRUN0000000000"
  log "launched instance $instance_id"

  # 5. Wait until it's running, then associate the EIP (§A.3).
  if [ "$WANT_EIP" = 1 ] && [ -n "$ALLOCATION_ID" ]; then
    log "waiting for instance to reach running before associating EIP"
    aws_cli ec2 wait instance-running --instance-ids "$instance_id" \
      || warn "instance-running wait returned non-zero (continuing)"
    log "associating EIP $box_ip"
    aws_cli ec2 associate-address --instance-id "$instance_id" \
      --allocation-id "$ALLOCATION_ID" >/dev/null \
      || die "provision: associate-address failed"
  elif [ "$WANT_EIP" = 0 ]; then
    # No EIP: read the auto-assigned public IP once running.
    log "no EIP requested; waiting for a public IP"
    aws_cli ec2 wait instance-running --instance-ids "$instance_id" \
      || warn "instance-running wait returned non-zero (continuing)"
    if [ "$DRY_RUN" = 1 ]; then
      box_ip="203.0.113.7"
    else
      box_ip="$(aws_cli ec2 describe-instances --instance-ids "$instance_id" \
          --query 'Reservations[0].Instances[0].PublicIpAddress' --output text)" \
        || die "provision: describe-instances for public IP failed"
    fi
  fi
  [ -n "$box_ip" ] && [ "$box_ip" != "None" ] || die "provision: could not determine box IP"
  dashed="$(dash_ip "$box_ip")"
  apex="${dashed}.sslip.io"
  hosts="$apex auth.$apex beads-viewer.$apex mux.$apex grafana.$apex"

  # 6. Wait for the box's edge to answer. cloud-init + bootstrap takes minutes;
  #    the off-box readiness signal (same as acceptance-smoke) is: traefik on
  #    :443 returns ANY HTTP status for the apex. 0 disables the wait.
  local ready=0 ready_at=""
  if [ "$WAIT_TIMEOUT" -gt 0 ] 2>/dev/null && [ "$DRY_RUN" != 1 ]; then
    log "waiting up to ${WAIT_TIMEOUT}s for the box edge to answer (https://${apex}/)"
    if wait_for_edge "$apex" "$WAIT_TIMEOUT"; then
      ready=1; ready_at="$(iso_now)"
      log "box edge is answering — box is up"
    else
      warn "box edge did not answer within ${WAIT_TIMEOUT}s — descriptor will say ready=false"
    fi
  elif [ "$DRY_RUN" = 1 ]; then
    ready=1; ready_at="$(iso_now)"
  fi

  # 7. Emit the descriptor.
  emit_descriptor \
    "$instance_id" "$box_ip" "$dashed" "$apex" "$hosts" "$NAMESPACE" \
    "$CALVER" "$APP_VERSION" "$ALLOCATION_ID" "$eip_allocated_by_us" "$SSH_USER" \
    "$INSTANCE_TYPE" "$SCENARIO" "$TTL_MINUTES" "$expiry_epoch" \
    "$(date -u -d "@$started_epoch" +%Y-%m-%dT%H:%M:%SZ 2>/dev/null || iso_now)" \
    "$ready_at" "$ready" | write_descriptor "$DESCRIPTOR"

  # Clean up a user-data temp file we rendered (it may carry a clone token).
  [ -z "$USER_DATA_FILE" ] && rm -f "$user_data"

  # exit 0 when the box is ready; 1 when it never came up (the descriptor is still
  # emitted either way, so the caller can tear it down).
  [ "$ready" = 1 ] || [ "$WAIT_TIMEOUT" -le 0 ] 2>/dev/null
}

# wait_for_edge APEX TIMEOUT_SEC — poll https://APEX/ until any HTTP status (the
# edge is up) or timeout. Returns 0 ready, 1 timed out.
wait_for_edge() {
  local apex="$1" timeout="$2" deadline code
  deadline=$(( $(now_epoch) + timeout ))
  while [ "$(now_epoch)" -lt "$deadline" ]; do
    code="$(curl -k -s -o /dev/null -m 10 -w '%{http_code}' "https://${apex}/" 2>/dev/null || echo 000)"
    # Any non-000 code means traefik answered — the edge is up.
    [ "$code" != "000" ] && return 0
    sleep "${TARBOAT_EDGE_POLL_SECONDS:-15}"
  done
  return 1
}

# emit_descriptor ... — print the tarboat-box/v1 JSON to stdout.
emit_descriptor() {
  local instance_id="$1" ip="$2" dashed="$3" apex="$4" hosts="$5" namespace="$6"
  local calver="$7" app_version="$8" alloc="$9" eip_ours="${10}" ssh_user="${11}"
  local itype="${12}" scenario="${13}" ttl="${14}" expiry="${15}"
  local started="${16}" ready_at="${17}" ready="${18}"
  local ready_bool="false"; [ "$ready" = 1 ] && ready_bool="true"
  local eip_bool="false"; [ "$eip_ours" = 1 ] && eip_bool="true"
  cat <<EOF
{
  "schema": "tarboat-box/v1",
  "instance_id": "$(json_str "$instance_id")",
  "region": "$(json_str "${REGION:-}")",
  "ip": "$(json_str "$ip")",
  "dashed_ip": "$(json_str "$dashed")",
  "apex": "$(json_str "$apex")",
  "hosts": "$(json_str "$hosts")",
  "namespace": "$(json_str "$namespace")",
  "calver": "$(json_str "$calver")",
  "app_version": "$(json_str "$app_version")",
  "allocation_id": "$(json_str "$alloc")",
  "eip_allocated_by_us": $eip_bool,
  "ssh_user": "$(json_str "$ssh_user")",
  "instance_type": "$(json_str "$itype")",
  "scenario": "$(json_str "$scenario")",
  "ttl_minutes": $ttl,
  "expiry_epoch": $expiry,
  "started_at": "$(json_str "$started")",
  "ready_at": "$(json_str "$ready_at")",
  "ready": $ready_bool
}
EOF
}

# write_descriptor PATH — "-" (or empty) to stdout, else to the file.
write_descriptor() {
  local path="$1"
  if [ -z "$path" ] || [ "$path" = "-" ]; then cat; else cat > "$path"; fi
}

# =============================================================================
# teardown
# =============================================================================
cmd_teardown() {
  local DESC_SRC="" INSTANCE_ID="" ALLOCATION_ID="" RELEASE_EIP="auto" FORCE=0
  local positional=""
  while [ "$#" -gt 0 ]; do
    case "$1" in
      --instance-id) INSTANCE_ID="$2"; shift 2;;
      --allocation-id) ALLOCATION_ID="$2"; shift 2;;
      --region) REGION="$2"; shift 2;;
      --release-eip) RELEASE_EIP="yes"; shift;;
      --no-release-eip) RELEASE_EIP="no"; shift;;
      --force) FORCE=1; shift;;
      --dry-run) DRY_RUN=1; shift;;
      -h|--help) sed -n '2,120p' "$0" | sed 's/^# \{0,1\}//'; exit 0;;
      -) DESC_SRC="-"; shift;;
      -*) die2 "teardown: unknown flag: $1";;
      *) [ -z "$positional" ] || die2 "teardown: extra arg: $1"; positional="$1"; shift;;
    esac
  done
  [ -n "$positional" ] && DESC_SRC="$positional"

  # If a descriptor was given (path or stdin), read instance/alloc/region from it
  # unless explicit flags already set them.
  if [ -n "$DESC_SRC" ]; then
    local doc
    if [ "$DESC_SRC" = "-" ]; then doc="$(cat)"; else
      [ -r "$DESC_SRC" ] || die2 "teardown: descriptor not readable: $DESC_SRC"
      doc="$(cat "$DESC_SRC")"
    fi
    [ -n "$INSTANCE_ID" ]   || INSTANCE_ID="$(desc_get "$doc" instance_id)"
    [ -n "$ALLOCATION_ID" ] || ALLOCATION_ID="$(desc_get "$doc" allocation_id)"
    [ -n "$REGION" ]        || REGION="$(desc_get "$doc" region)"
    # eip_allocated_by_us governs the default release decision.
    if [ "$RELEASE_EIP" = "auto" ]; then
      local ours; ours="$(desc_get "$doc" eip_allocated_by_us)"
      [ "$ours" = "true" ] && RELEASE_EIP="yes" || RELEASE_EIP="no"
    fi
  fi

  [ -n "$INSTANCE_ID" ] || die2 "teardown: need an instance id (descriptor or --instance-id)"
  if [ "$DRY_RUN" != 1 ]; then
    command -v aws >/dev/null 2>&1 || die2 "teardown: aws CLI not found on PATH"
  fi

  # Honor lifecycle=keep (same contract as the reaper) unless --force.
  if [ "$FORCE" != 1 ] && [ "$DRY_RUN" != 1 ]; then
    local life
    life="$(aws_cli ec2 describe-instances --instance-ids "$INSTANCE_ID" \
        --query "Reservations[0].Instances[0].Tags[?Key=='${TAG_LIFECYCLE}'].Value | [0]" \
        --output text 2>/dev/null || echo "")"
    if [ "$life" = "keep" ]; then
      die "teardown: $INSTANCE_ID is tagged ${TAG_LIFECYCLE}=keep — refusing without --force"
    fi
  fi

  log "terminating instance $INSTANCE_ID"
  aws_cli ec2 terminate-instances --instance-ids "$INSTANCE_ID" >/dev/null \
    || die "teardown: terminate-instances failed for $INSTANCE_ID"

  if [ "$RELEASE_EIP" = "yes" ] && [ -n "$ALLOCATION_ID" ] && [ "$ALLOCATION_ID" != "None" ]; then
    log "waiting for instance to terminate before releasing EIP"
    aws_cli ec2 wait instance-terminated --instance-ids "$INSTANCE_ID" \
      || warn "instance-terminated wait returned non-zero (continuing to release)"
    log "releasing EIP $ALLOCATION_ID"
    aws_cli ec2 release-address --allocation-id "$ALLOCATION_ID" >/dev/null \
      || warn "teardown: release-address failed for $ALLOCATION_ID (EIP may leak)"
  elif [ -n "$ALLOCATION_ID" ] && [ "$ALLOCATION_ID" != "None" ]; then
    log "leaving EIP $ALLOCATION_ID allocated (not ours / --no-release-eip)"
  fi
  log "teardown complete for $INSTANCE_ID"
}

# =============================================================================
# reap — box-level safety net (mirror of a namespace reaper CronJob)
# =============================================================================
cmd_reap() {
  local TTL_MINUTES="" RELEASE_EIP="yes"
  while [ "$#" -gt 0 ]; do
    case "$1" in
      --region) REGION="$2"; shift 2;;
      --ttl-minutes) TTL_MINUTES="$2"; shift 2;;
      --no-release-eip) RELEASE_EIP="no"; shift;;
      --dry-run) DRY_RUN=1; shift;;
      -h|--help) sed -n '2,120p' "$0" | sed 's/^# \{0,1\}//'; exit 0;;
      *) die2 "reap: unknown flag: $1";;
    esac
  done
  if [ "$DRY_RUN" != 1 ]; then
    command -v aws >/dev/null 2>&1 || die2 "reap: aws CLI not found on PATH"
  fi

  local now; now="$(now_epoch)"
  # Enumerate running/pending ephemeral boxes with their expiry + lifecycle +
  # launch time. One TAB-separated row per instance.
  local rows
  rows="$(aws_cli ec2 describe-instances \
      --filters "Name=tag:${TAG_EPHEMERAL},Values=true" \
                "Name=instance-state-name,Values=pending,running,stopping,stopped" \
      --query "Reservations[].Instances[].[InstanceId,LaunchTime,Tags[?Key=='${TAG_EXPIRY}'].Value|[0],Tags[?Key=='${TAG_LIFECYCLE}'].Value|[0]]" \
      --output text 2>/dev/null)" || die "reap: describe-instances failed"

  [ -n "$rows" ] || { log "reap: no ephemeral tarboat boxes found"; return 0; }

  local reaped=0 kept=0 alive=0
  while IFS=$'\t' read -r iid launch expiry life; do
    [ -n "$iid" ] || continue
    if [ "$life" = "keep" ]; then
      log "[keep]   $iid (lifecycle=keep, exempt)"; kept=$((kept+1)); continue
    fi
    # Decide reapable: explicit --ttl-minutes overrides the tagged expiry.
    local reapable=0
    if [ -n "$TTL_MINUTES" ]; then
      local launch_epoch age
      launch_epoch="$(date -u -d "$launch" +%s 2>/dev/null || echo 0)"
      age=$(( now - launch_epoch ))
      [ "$launch_epoch" -gt 0 ] && [ "$age" -ge $((TTL_MINUTES * 60)) ] && reapable=1
    else
      case "$expiry" in
        ''|None) reapable=0;;
        *) [ "$expiry" -le "$now" ] 2>/dev/null && reapable=1;;
      esac
    fi
    if [ "$reapable" = 1 ]; then
      log "[reap]   $iid (expiry=${expiry:-none} launch=$launch)"
      aws_cli ec2 terminate-instances --instance-ids "$iid" >/dev/null \
        || { warn "reap: terminate failed for $iid"; continue; }
      reaped=$((reaped+1))
      # Best-effort release of any EIP still associated with this box.
      if [ "$RELEASE_EIP" = "yes" ] && [ "$DRY_RUN" != 1 ]; then
        local alloc
        alloc="$(aws_cli ec2 describe-addresses \
            --filters "Name=instance-id,Values=$iid" \
            --query 'Addresses[0].AllocationId' --output text 2>/dev/null || echo None)"
        if [ -n "$alloc" ] && [ "$alloc" != "None" ]; then
          aws_cli ec2 release-address --allocation-id "$alloc" >/dev/null 2>&1 \
            && log "         released EIP $alloc" \
            || warn "         release-address failed for $alloc"
        fi
      fi
    else
      log "[alive]  $iid (expiry=${expiry:-none}, not yet due)"; alive=$((alive+1))
    fi
  done <<< "$rows"

  log "reap done: reaped=$reaped kept=$kept alive=$alive"
  return 0
}

# =============================================================================
# dispatch
# =============================================================================
usage() {
  cat >&2 <<EOF
tarboat-provision-box.sh — provision/teardown/reap the ephemeral box for the
standing test-in-box (Tarboat) regression sweep. (E4 ka-0000002700, epic ka-0000003645)

  tarboat-provision-box.sh provision --calver VER [flags]   launch + emit descriptor
  tarboat-provision-box.sh teardown  (DESCRIPTOR|--instance-id ID) [flags]
  tarboat-provision-box.sh reap      [--ttl-minutes N] [--dry-run]

Run any subcommand with -h for its flags. Full header: head -n 130 "\$0".
EOF
}

main() {
  local sub="${1:-}"; shift || true
  case "$sub" in
    provision) cmd_provision "$@";;
    teardown)  cmd_teardown "$@";;
    reap)      cmd_reap "$@";;
    -h|--help|help|"") usage; [ -z "$sub" ] && exit 2 || exit 0;;
    *) die2 "unknown subcommand: $sub (want: provision|teardown|reap)";;
  esac
}

main "$@"
