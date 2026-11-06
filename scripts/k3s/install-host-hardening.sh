#!/usr/bin/env bash
# install-host-hardening.sh — host-layer public-internet hardening for the
# single-EC2 Gardenio-in-a-Box node.
#
# Phase 3 P3.5 (ka-0000006978), epic ka-0000005904. Source:
# gardenio/docs/research/inbox-public-internet-exposure-hardening.md
# (host firewall §"Sketch nftables ruleset", SSH §(c), threat-model F4–F6,F12).
#
# This is the HOST half of "only 80/443/22 reachable; control plane unreachable
# from outside". The chart/NetworkPolicy half is P3.4 (ka-0000005564). It does
# four things, all fail-closed, all nftables-native (never iptables-legacy, to
# avoid the legacy-vs-nft split that silently no-ops on a k3s host):
#
#   1. nftables: a SEPARATE `inet gardenio_fw` table that default-DROPs INPUT
#      except 22/80/443 + established/related + loopback + the CNI interfaces.
#      FORWARD/NAT are LEFT TO k3s (kube-proxy + flannel own them); we never
#      flush k3s's own ip/ip6 tables. This drops :6443 (apiserver), :10250
#      (kubelet), the NodePort range, and every in-cluster datastore port from
#      the public NIC — the real control for F4 (k3s cannot bind 6443 to
#      loopback; the firewall drop is the enforcement).
#   2. SSH: key-only, no root, single operator account, no tunneling — via an
#      sshd_config.d drop-in (F6). Crypto algorithm selection is LEFT to the
#      distro crypto-policy (do NOT pin Ciphers/MACs/Kex: a list the running
#      OpenSSH doesn't advertise makes sshd refuse to start — ka-0000006491).
#   3. fail2ban with the NFTABLES banaction + journald backend (the real gotcha:
#      the default iptables-multiport banaction writes to a legacy table that
#      never matches on a pure-nft host, so fail2ban looks healthy but bans
#      nothing). Plus an always-on nftables :22 new-connection rate-limit floor.
#   4. IMDSv2: enforce hop-limit=1 + tokens-required via the IMDS HTTP API
#      reachable from the box itself (best-effort; the authoritative knob is the
#      launch-time MetadataOptions — see the cloud-config template). (F12)
#
# Idempotent: re-running re-writes the same ruleset/config and reloads. Safe to
# run from cloud-init AND by hand. Run AFTER k3s is up so its tables exist; the
# script only ADDS its own table and never touches k3s's.
#
# Env / flags:
#   SSH_PORT          operator SSH port                       (default: 22)
#   OPERATOR_USER     the single allowed SSH user             (default: gardenio-op)
#   PUBLIC_IFACE      public NIC to restrict                  (default: auto-detect)
#   SSH_RATE          new SSH conns/min/src before drop       (default: 10)
#   NO_FAIL2BAN       set 1 to skip fail2ban (nft rate-limit still applies)
#   NO_IMDS           set 1 to skip the IMDSv2 hop-limit enforcement attempt
#   DRY_RUN           set 1 to print what would change without applying
set -euo pipefail

SSH_PORT="${SSH_PORT:-22}"
OPERATOR_USER="${OPERATOR_USER:-gardenio-op}"
PUBLIC_IFACE="${PUBLIC_IFACE:-}"
SSH_RATE="${SSH_RATE:-10}"
NO_FAIL2BAN="${NO_FAIL2BAN:-0}"
NO_IMDS="${NO_IMDS:-0}"
DRY_RUN="${DRY_RUN:-0}"

while [ $# -gt 0 ]; do
  case "$1" in
    --ssh-port) SSH_PORT="$2"; shift 2;;
    --operator-user) OPERATOR_USER="$2"; shift 2;;
    --public-iface) PUBLIC_IFACE="$2"; shift 2;;
    --ssh-rate) SSH_RATE="$2"; shift 2;;
    --no-fail2ban) NO_FAIL2BAN=1; shift;;
    --no-imds) NO_IMDS=1; shift;;
    --dry-run) DRY_RUN=1; shift;;
    -h|--help) sed -n '2,55p' "$0"; exit 0;;
    *) echo "unknown arg: $1" >&2; exit 2;;
  esac
done

log()  { printf '\033[1;36m[harden]\033[0m %s\n' "$*"; }
warn() { printf '\033[1;33m[harden] WARN:\033[0m %s\n' "$*" >&2; }
die()  { printf '\033[1;31m[harden] ERROR:\033[0m %s\n' "$*" >&2; exit 1; }

[ "${DRY_RUN}" = "1" ] || [ "$(id -u)" -eq 0 ] || die "must run as root (installs nftables/fail2ban + writes sshd drop-in)"

NFT_RULES="/etc/nftables/gardenio-fw.nft"
SSHD_DROPIN="/etc/ssh/sshd_config.d/10-gardenio-hardening.conf"
F2B_JAIL="/etc/fail2ban/jail.local"
OP_SUDOERS="/etc/sudoers.d/90-gardenio-op"

# Cloud-init default accounts, in the order we prefer them when hunting for the
# bootstrapping keypair. The launch keypair lands in exactly one of these on a
# fresh image (ec2-user on AL2023/AL2, ubuntu on Ubuntu, etc.).
CLOUD_DEFAULT_USERS="ec2-user ubuntu admin fedora centos rocky almalinux cloud-user debian arch bitnami"

# ── 0. Install prerequisite packages (nftables, fail2ban) ────────────────────
# Fresh AL2023 (al2023-ami-kernel-default) and minimal Ubuntu ship WITHOUT the
# nftables package, so the `command -v nft || die` sanity check below would abort
# the whole bootstrap. Install nftables (and fail2ban unless --no-fail2ban) up
# front, detected via dnf/apt-get. The `command -v nft || die` in write_nftables
# stays as the post-install sanity check.
install_packages() {
  local -a pkgs=(nftables)
  [ "$NO_FAIL2BAN" = "1" ] || pkgs+=(fail2ban)
  if [ "$DRY_RUN" = "1" ]; then
    log "[dry-run] would install packages: ${pkgs[*]} (dnf/apt-get)"
    return
  fi
  if command -v dnf >/dev/null 2>&1; then          # Amazon Linux 2023 / RHEL-like
    log "installing packages via dnf: ${pkgs[*]}"
    dnf install -y "${pkgs[@]}" >/dev/null 2>&1 || warn "dnf install (${pkgs[*]}) had issues; relying on post-install nft sanity check"
  elif command -v apt-get >/dev/null 2>&1; then    # Ubuntu / Debian
    log "installing packages via apt-get: ${pkgs[*]}"
    apt-get update -y >/dev/null 2>&1 || true
    DEBIAN_FRONTEND=noninteractive apt-get install -y "${pkgs[@]}" >/dev/null 2>&1 \
      || warn "apt-get install (${pkgs[*]}) had issues; relying on post-install nft sanity check"
  else
    warn "no dnf/apt-get — cannot install ${pkgs[*]}; relying on post-install nft sanity check"
  fi
}

# ── 0b. Disable firewalld (AL2023 ships it active) ───────────────────────────
# AL2023 ships firewalld enabled+running. firewalld installs its own nftables
# tables (filter_FORWARD with a default-drop posture) that, after a reboot,
# block k3s pod->service / hairpin (intra-cluster) traffic — which broke
# forwardauth OIDC discovery on a live box (ka-0000006491, i-0123456789abcdef0).
# Our host firewall is the standalone `inet gardenio_fw` table written below
# (INPUT-only, leaves FORWARD/CNI to k3s); firewalld is redundant with it and
# actively hostile to k3s forwarding, so disable+stop it before we lay down our
# ruleset. No-op (and harmless) on images that don't ship firewalld.
disable_firewalld() {
  if [ "$DRY_RUN" = "1" ]; then
    if systemctl list-unit-files firewalld.service >/dev/null 2>&1; then
      log "[dry-run] would 'systemctl disable --now firewalld' (it blocks k3s pod->service forwarding)"
    else
      log "[dry-run] firewalld not present; nothing to disable"
    fi
    return
  fi
  if systemctl list-unit-files firewalld.service >/dev/null 2>&1; then
    log "disabling firewalld (it installs FORWARD-drop nft tables that break k3s pod->service traffic)"
    systemctl disable --now firewalld >/dev/null 2>&1 || warn "could not disable firewalld; k3s pod->service traffic may be blocked after reboot"
  else
    log "firewalld not installed; nothing to disable"
  fi
}

# ── Detect the public-facing interface (the default route's dev) ─────────────
detect_iface() {
  [ -n "$PUBLIC_IFACE" ] && { echo "$PUBLIC_IFACE"; return; }
  ip -4 route show default 2>/dev/null | awk '{for(i=1;i<=NF;i++) if($i=="dev"){print $(i+1); exit}}'
}

# ── 1. nftables host firewall ─────────────────────────────────────────────────
# A standalone `inet gardenio_fw` table so we coexist with — never flush — k3s's
# own ip/ip6 nat+filter tables. INPUT default-drop; FORWARD is NOT governed here
# (left default-accept for kube-proxy/flannel). Loopback + CNI ifaces (cni0,
# flannel.1, veth*) are trusted wholesale so intra-cluster traffic on ClusterIPs
# is never filtered. The public NIC admits only 22/80/443 + established/related.
write_nftables() {
  local iface; iface="$(detect_iface)"
  [ -n "$iface" ] || warn "could not detect public interface; ruleset will rate-limit :22 on all ifaces"
  if [ "$DRY_RUN" = "1" ]; then
    log "[dry-run] would write $NFT_RULES (public iface: ${iface:-<all>}, ssh :$SSH_PORT rate ${SSH_RATE}/min), then nft -c -f / nft -f / enable nftables"
    return
  fi
  log "writing nftables ruleset $NFT_RULES (public iface: ${iface:-<all>}, ssh :$SSH_PORT rate ${SSH_RATE}/min)"

  mkdir -p "$(dirname "$NFT_RULES")"
  cat >"$NFT_RULES" <<EOF
#!/usr/sbin/nft -f
# /etc/nftables/gardenio-fw.nft — Gardenio-in-a-Box host firewall (P3.5).
# Governs INPUT only. FORWARD/NAT are left to k3s (kube-proxy + flannel).
# DO NOT flush k3s's tables — this is a SEPARATE 'inet gardenio_fw' table; k3s
# manages its own ip/ip6 nat/filter tables independently. Generated by
# scripts/k3s/install-host-hardening.sh — do not hand-edit.

table inet gardenio_fw {
    # Per-source SSH connection-rate meter (new conns only).
    set ssh_meter {
        type ipv4_addr
        flags dynamic, timeout
        timeout 1m
    }

    chain input {
        type filter hook input priority 0; policy drop;

        # Loopback + already-established flows.
        iif "lo" accept
        ct state established,related accept
        ct state invalid drop

        # Trust the CNI/cluster interfaces wholesale so intra-cluster traffic
        # (kube-dns, apiserver, datastore ClusterIPs) is never filtered.
        iifname { "cni0", "flannel.1", "cilium*", "veth*" } accept

        # ICMP (PMTU, diagnostics) — rate-limited.
        ip protocol icmp icmp type { echo-request, destination-unreachable, time-exceeded } limit rate 10/second accept
        ip6 nexthdr icmpv6 accept

        # SSH (:$SSH_PORT) — rate-limit NEW conns per source, then accept.
        tcp dport $SSH_PORT ct state new add @ssh_meter { ip saddr limit rate over ${SSH_RATE}/minute } drop
        tcp dport $SSH_PORT accept

        # The only other public ports: HTTP + HTTPS (Traefik web/websecure).
        tcp dport { 80, 443 } accept

        # Everything else (6443 apiserver, 10250 kubelet, 30000-32767 NodePort,
        # 5432 postgres, 4222/8222/6222 NATS, vm*/grafana) is DROPPED by policy.
    }
}
EOF

  command -v nft >/dev/null 2>&1 || die "nftables (nft) not installed; install it before hardening"
  nft -c -f "$NFT_RULES" || die "nftables ruleset failed validation ($NFT_RULES)"
  # Load our table now (k3s tables remain untouched).
  nft -f "$NFT_RULES"
  # Persist across reboots: include our file from the system nftables config and
  # enable the service. We append our include rather than overwrite the distro
  # default so a vendor-managed /etc/nftables.conf keeps working.
  if [ -f /etc/nftables.conf ] && ! grep -q "$NFT_RULES" /etc/nftables.conf 2>/dev/null; then
    printf '\ninclude "%s"\n' "$NFT_RULES" >>/etc/nftables.conf
  fi
  systemctl enable --now nftables 2>/dev/null || warn "could not enable nftables.service (ruleset still loaded for this boot)"
  log "nftables host firewall active (INPUT default-drop; 22/80/443 only)"
}

# ── 2a. Ensure the operator account exists with the bootstrap key ─────────────
# The sshd drop-in below pins `AllowUsers $OPERATOR_USER` + key-only + no-root.
# On a fresh cloud image the launch keypair lands in the cloud-init DEFAULT
# account (ec2-user on AL2023/AL2, ubuntu on Ubuntu, …) — NOT $OPERATOR_USER.
# If we reload sshd admitting only a nonexistent / key-less account, the
# operator is locked out (recourse: detach root volume or SSM). So, BEFORE the
# drop-in is written/reloaded, make sure $OPERATOR_USER exists, is a passwordless
# sudoer, and carries a copy of the bootstrap account's authorized_keys.
# Idempotent: re-running adds nothing it already has. (ka-0000002090)
ensure_operator_user() {
  local op="$OPERATOR_USER" op_home src_user="" src_keys="" op_ssh op_keys

  # Find the bootstrap account: the cloud default user (in preference order) that
  # actually has a populated ~/.ssh/authorized_keys. Skip $OPERATOR_USER itself
  # and never source from root.
  for cand in $CLOUD_DEFAULT_USERS; do
    [ "$cand" = "$op" ] && continue
    local h; h="$(getent passwd "$cand" 2>/dev/null | cut -d: -f6)"
    [ -n "$h" ] || continue
    [ -s "$h/.ssh/authorized_keys" ] || continue
    src_user="$cand"; src_keys="$h/.ssh/authorized_keys"; break
  done
  # Fallback: scan every non-root /home account for a populated authorized_keys.
  if [ -z "$src_keys" ]; then
    while IFS=: read -r u _ uid _ _ h _; do
      [ "$u" = "$op" ] && continue
      [ "$uid" -ge 1000 ] 2>/dev/null || continue
      case "$h" in /home/*|/Users/*) ;; *) continue;; esac
      [ -s "$h/.ssh/authorized_keys" ] || continue
      src_user="$u"; src_keys="$h/.ssh/authorized_keys"; break
    done < <(getent passwd)
  fi

  if [ "$DRY_RUN" = "1" ]; then
    if getent passwd "$op" >/dev/null 2>&1; then
      log "[dry-run] operator user '$op' already exists"
    else
      log "[dry-run] would create operator user '$op' (useradd -m -s /bin/bash + NOPASSWD sudoers $OP_SUDOERS)"
    fi
    if [ -n "$src_keys" ]; then
      log "[dry-run] would seed $op ~/.ssh/authorized_keys from bootstrap account '$src_user' ($src_keys)"
    else
      warn "[dry-run] no bootstrap account with a populated authorized_keys found; '$op' would have NO keys (lockout risk)"
    fi
    return
  fi

  # Create the account if missing.
  if getent passwd "$op" >/dev/null 2>&1; then
    log "operator user '$op' already exists"
  else
    log "creating operator user '$op' (useradd -m -s /bin/bash)"
    useradd -m -s /bin/bash "$op" || die "useradd $op failed"
  fi
  op_home="$(getent passwd "$op" | cut -d: -f6)"
  [ -n "$op_home" ] || die "could not resolve home dir for '$op'"

  # Passwordless sudo via a dedicated drop-in (validated with visudo -c).
  if [ ! -f "$OP_SUDOERS" ]; then
    log "granting passwordless sudo to '$op' ($OP_SUDOERS)"
    printf '%s ALL=(ALL) NOPASSWD:ALL\n' "$op" >"$OP_SUDOERS"
    chmod 0440 "$OP_SUDOERS"
    if command -v visudo >/dev/null 2>&1 && ! visudo -cf "$OP_SUDOERS" >/dev/null 2>&1; then
      rm -f "$OP_SUDOERS"; die "sudoers drop-in $OP_SUDOERS failed visudo validation"
    fi
  fi

  # Seed authorized_keys from the bootstrap account (700 dir, 600 file, owned by op).
  op_ssh="$op_home/.ssh"; op_keys="$op_ssh/authorized_keys"
  mkdir -p "$op_ssh"; chmod 700 "$op_ssh"
  if [ -n "$src_keys" ]; then
    if [ -s "$op_keys" ] && cmp -s "$src_keys" "$op_keys"; then
      log "operator '$op' authorized_keys already matches bootstrap account '$src_user'"
    else
      log "seeding $op authorized_keys from bootstrap account '$src_user' ($src_keys)"
      install -m 600 "$src_keys" "$op_keys"
    fi
  elif [ -s "$op_keys" ]; then
    warn "no cloud bootstrap account with keys found, but '$op' already has authorized_keys — leaving as-is"
  else
    warn "no bootstrap account with a populated authorized_keys found AND '$op' has none — refusing to reload sshd would lock you out. Inject a key into $op_keys (or pass --operator-user <existing-key-holder>) before sshd reload."
  fi
  chmod 600 "$op_keys" 2>/dev/null || true
  chown -R "$op":"$op" "$op_ssh"
}

# ── 2. SSH hardening drop-in ──────────────────────────────────────────────────
write_sshd_dropin() {
  if [ "$DRY_RUN" = "1" ]; then log "[dry-run] would write $SSHD_DROPIN (key-only, no root, user=$OPERATOR_USER) and 'sshd -t && systemctl reload sshd'"; return; fi
  log "writing sshd hardening drop-in $SSHD_DROPIN (key-only, no root, user=$OPERATOR_USER)"
  mkdir -p "$(dirname "$SSHD_DROPIN")"
  cat >"$SSHD_DROPIN" <<EOF
# Managed by scripts/k3s/install-host-hardening.sh (Gardenio-in-a-Box P3.5).
# Auth
PasswordAuthentication no
KbdInteractiveAuthentication no
ChallengeResponseAuthentication no
PubkeyAuthentication yes
PermitRootLogin no
AuthenticationMethods publickey
PermitEmptyPasswords no
UsePAM yes

# Scope to the single operator account
AllowUsers $OPERATOR_USER
Port $SSH_PORT

# Brute-force pressure
MaxAuthTries 3
MaxSessions 4
LoginGraceTime 20

# Lateral-movement / tunneling surface (the box is a destination, not a jump host)
X11Forwarding no
AllowAgentForwarding no
AllowTcpForwarding no
PermitTunnel no
GatewayPorts no

# Idle hygiene
ClientAliveInterval 300
ClientAliveCountMax 2

# NOTE: We deliberately do NOT pin Ciphers/MACs/KexAlgorithms here. AL2023
# governs OpenSSH crypto via the system-wide crypto-policy (update-crypto-policies,
# /etc/crypto-policies/back-ends/opensshserver.config), which is already a
# strong, FIPS-aware default. A drop-in that lists algorithms the running
# OpenSSH build doesn't advertise (e.g. sntrup761x25519-sha512@openssh.com on
# the AL2023 OpenSSH) makes sshd reject its own config and fail to listen,
# locking the operator out and requiring EBS volume surgery to recover
# (ka-0000006491, found on box i-0123456789abcdef0). The security directives
# above (key-only, no root, no tunneling, rate limits) are what matter; leave
# algorithm selection to the distro crypto-policy.
EOF
  # Lockout guard: AllowUsers pins $OPERATOR_USER and we forbid passwords/root,
  # so reloading with no key for that account locks the operator out. Write the
  # drop-in (so it's ready) but refuse to reload until a key is present.
  local op_home op_keys=""
  op_home="$(getent passwd "$OPERATOR_USER" 2>/dev/null | cut -d: -f6)"
  [ -n "$op_home" ] && op_keys="$op_home/.ssh/authorized_keys"
  if [ -z "$op_keys" ] || [ ! -s "$op_keys" ]; then
    warn "operator '$OPERATOR_USER' has no authorized_keys — drop-in written but NOT reloading sshd (would lock you out). Seed $op_keys then reload sshd."
    return
  fi

  # Validate BEFORE reload so a typo fails loudly instead of locking us out.
  if sshd -t 2>/dev/null; then
    systemctl reload sshd 2>/dev/null || systemctl reload ssh 2>/dev/null || warn "could not reload sshd (drop-in written; reload manually)"
    log "sshd hardened + reloaded"
  else
    warn "sshd -t rejected the drop-in; leaving it in place but NOT reloading. Run 'sshd -t' to inspect."
  fi
}

# ── 3. fail2ban (nftables banaction) + recidive ───────────────────────────────
write_fail2ban() {
  if [ "$NO_FAIL2BAN" = "1" ]; then log "fail2ban skipped (--no-fail2ban); nftables :22 rate-limit is the floor"; return; fi
  if [ "$DRY_RUN" = "1" ]; then log "[dry-run] would write $F2B_JAIL (nftables banaction, journald backend) and enable fail2ban"; return; fi
  log "writing fail2ban jail.local $F2B_JAIL (nftables banaction, journald backend)"
  if ! command -v fail2ban-server >/dev/null 2>&1; then
    warn "fail2ban not installed; skipping (nftables :22 rate-limit remains the floor). Install fail2ban to enable IP-reputation banning."
    return
  fi
  mkdir -p "$(dirname "$F2B_JAIL")"
  cat >"$F2B_JAIL" <<EOF
# Managed by scripts/k3s/install-host-hardening.sh (Gardenio-in-a-Box P3.5).
# banaction MUST be nftables-* — the default iptables-multiport writes to a
# legacy table that never matches on a pure-nft k3s host (fail2ban would look
# healthy while banning nothing). backend=systemd because k3s hosts log sshd to
# journald, not /var/log/auth.log.
[DEFAULT]
banaction = nftables-multiport
banaction_allports = nftables-allports
backend = systemd
bantime  = 1h
findtime = 10m
maxretry = 4

[sshd]
enabled = true
port    = $SSH_PORT

[recidive]
enabled  = true
logpath  = /var/log/fail2ban.log
bantime  = 1w
findtime = 1d
maxretry = 5
EOF
  systemctl enable --now fail2ban 2>/dev/null || warn "could not enable fail2ban.service"
  systemctl restart fail2ban 2>/dev/null || true
  log "fail2ban active (sshd + recidive, nftables banaction)"
}

# ── 4. IMDSv2 hop-limit=1 (best-effort from the box) ──────────────────────────
# The AUTHORITATIVE control is the launch-time MetadataOptions
# (HttpTokens=required, HttpPutResponseHopLimit=1) — see the cloud-config
# template / run-instances. From inside the box we can only request it via the
# IMDS-modify endpoint if the instance role allows it; otherwise this is a no-op
# and we just log the residual gap. A hop-limit of 1 means a container/pod one
# network hop away cannot reach IMDS even if egress NetworkPolicy is off.
enforce_imdsv2() {
  if [ "$NO_IMDS" = "1" ]; then log "IMDSv2 enforcement skipped (--no-imds)"; return; fi
  if [ "$DRY_RUN" = "1" ]; then log "[dry-run] would attempt IMDSv2 hop-limit=1 enforcement"; return; fi
  local tok iid region
  tok="$(curl -sf -X PUT 'http://169.254.169.254/latest/api/token' -H 'X-aws-ec2-metadata-token-ttl-seconds: 60' 2>/dev/null || true)"
  [ -n "$tok" ] || { warn "IMDS unreachable / already locked down — cannot self-check hop-limit (set HttpPutResponseHopLimit=1 at launch)"; return; }
  iid="$(curl -sf -H "X-aws-ec2-metadata-token: $tok" 'http://169.254.169.254/latest/meta-data/instance-id' 2>/dev/null || true)"
  region="$(curl -sf -H "X-aws-ec2-metadata-token: $tok" 'http://169.254.169.254/latest/meta-data/placement/region' 2>/dev/null || true)"
  if command -v aws >/dev/null 2>&1 && [ -n "$iid" ] && [ -n "$region" ]; then
    if aws ec2 modify-instance-metadata-options --instance-id "$iid" --region "$region" \
         --http-tokens required --http-put-response-hop-limit 1 --http-endpoint enabled >/dev/null 2>&1; then
      log "IMDSv2 enforced: tokens required, hop-limit=1 (instance $iid)"
      return
    fi
  fi
  warn "could not enforce IMDSv2 from the box (no aws CLI / role perms). Set MetadataOptions at LAUNCH: HttpTokens=required, HttpPutResponseHopLimit=1."
}

install_packages
disable_firewalld
write_nftables
ensure_operator_user
write_sshd_dropin
write_fail2ban
enforce_imdsv2

log "✅ host hardening applied (nftables 22/80/443 default-drop; SSH key-only; fail2ban nft; IMDSv2)."
log "   verify off-box: nc -vz <eip> 6443 / 10250 / 5432 must FAIL; 443 must succeed."
log "   verify on-box:  k3s kubectl get pods -A  and pod DNS must still work (FORWARD/CNI intact)."
