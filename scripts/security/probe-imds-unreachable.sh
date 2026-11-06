#!/usr/bin/env bash
# probe-imds-unreachable.sh: prove this pod cannot reach the EC2 instance
# metadata service (P7 Task 7, spec D6). The positive control proves the pod
# has working egress, so "IMDS unreachable" is not just "no network".
# Exit: 0 IMDS blocked + control OK; 1 IMDS reachable; 2 control failed.
#
# kube-router installs a new pod's firewall chain asynchronously, so a pod's
# first ~1s can still reach IMDS (gd-fetk49kFlz measured 200 at t=0, 000 from
# t=1s). A probe run as the first command would false-fail, so the IPv4 check
# retries until blocked for up to IMDS_PROBE_SETTLE_SECONDS (default 15).
set -euo pipefail
control="${IMDS_PROBE_CONTROL_URL:-https://api.github.com/zen}"
settle="${IMDS_PROBE_SETTLE_SECONDS:-15}"
retry_sleep="${IMDS_PROBE_RETRY_SLEEP:-1}"

code="$(curl -sS -m 10 -o /dev/null -w '%{http_code}' "$control" 2>/dev/null || true)"
if [ "$code" != "200" ]; then
  echo "control: $control answered '$code', want 200; the probe proves nothing without egress" >&2
  exit 2
fi
echo "control: $control answered 200"

imds_v4_answers() {
  curl -sS -m 3 -X PUT -H 'X-aws-ec2-metadata-token-ttl-seconds: 60' \
    -o /dev/null http://169.254.169.254/latest/api/token 2>/dev/null
}

deadline=$((SECONDS + settle))
while imds_v4_answers; do
  if [ "$SECONDS" -ge "$deadline" ]; then
    echo "IMDS (169.254.169.254) still answers the token request after ${settle}s: pod network can reach the node role" >&2
    exit 1
  fi
  echo "IMDS answered; retrying in ${retry_sleep}s in case the pod firewall is still being installed" >&2
  sleep "$retry_sleep"
done

if curl -sS -m 3 -g -X PUT -H 'X-aws-ec2-metadata-token-ttl-seconds: 60' \
     -o /dev/null 'http://[fd00:ec2::254]/latest/api/token' 2>/dev/null; then
  echo "IMDS (fd00:ec2::254) answered the token request" >&2
  exit 1
fi
echo "IMDS unreachable from this pod (IPv4 and IPv6)"
