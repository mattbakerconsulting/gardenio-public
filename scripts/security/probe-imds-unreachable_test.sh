#!/usr/bin/env bash
# Drives probe-imds-unreachable.sh with a fake curl on PATH so every outcome is
# exercised without touching the network.
set -euo pipefail
here="$(cd "$(dirname "$0")" && pwd)"
tmp="$(mktemp -d)"; trap 'rm -rf "$tmp"' EXIT
fail=0

# $1 = imds behaviour: open | blocked | settles (answers once, then blocks: the
# kube-router startup window). $2 = control code.
make_curl() {
  : >"$tmp/imds-calls"
  cat >"$tmp/curl" <<EOF2
#!/usr/bin/env bash
for a in "\$@"; do
  case "\$a" in
    *169.254.169.254*)
      echo x >>"$tmp/imds-calls"
      case "$1" in
        open) printf 'AQAEAFAKE-TOKEN'; exit 0;;
        settles) [ "\$(wc -l <"$tmp/imds-calls")" -le 1 ] && { printf 'AQAEAFAKE-TOKEN'; exit 0; }; exit 28;;
        *) exit 28;;
      esac;;
    *fd00:ec2::254*) exit 7;;
    http*) printf '%s' "$2"; exit 0;;
  esac
done
exit 2
EOF2
  chmod +x "$tmp/curl"
}

run() {
  set +e
  PATH="$tmp:$PATH" IMDS_PROBE_SETTLE_SECONDS="${1:-3}" IMDS_PROBE_RETRY_SLEEP=1 \
    bash "$here/probe-imds-unreachable.sh" >/dev/null 2>&1
  echo $?
  set -e
}

check() { # $1 name, $2 want, $3 got
  if [ "$3" = "$2" ]; then echo "PASS $1"; else echo "FAIL $1: rc=$3 want $2"; fail=1; fi
}

make_curl blocked 200; check "blocked IMDS + live control passes" 0 "$(run)"
make_curl open 200; check "planted reachable IMDS fails" 1 "$(run)"
make_curl blocked 000; check "dead network fails the control" 2 "$(run)"
make_curl settles 200; check "IMDS open only in the startup window passes after retry" 0 "$(run 5)"
make_curl open 200; check "IMDS still open after the settle window fails" 1 "$(run 0)"
exit "$fail"
