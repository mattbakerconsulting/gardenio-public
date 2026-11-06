#!/usr/bin/env bash
# kubectl-ssm_test.sh -- self-test for scripts/k3s/kubectl-ssm.sh.
#
# `aws` is a stub on PATH that records its arguments, answers the instance
# lookup, and for `ssm start-session` opens a real listener on the local port
# (so the script's wait-for-port and idempotency paths run for real). The
# kubeconfig cases use the real kubectl and skip loudly when it is absent; the
# GARDENIO_SSM_VIA cases after them use a kubectl stub and always run.
#
# Run: bash scripts/k3s/kubectl-ssm_test.sh
# `A && note ok || note FAIL` is the idiom here: note never fails, so C running
# after a true A is not a concern.
# shellcheck disable=SC2015
set -uo pipefail

SRC_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
SCRIPT="$SRC_DIR/kubectl-ssm.sh"
WORK="$(mktemp -d)"
PASS=0
FAIL=0
note() {
	if [ "$1" = ok ]; then
		PASS=$((PASS + 1))
		printf 'ok   %s\n' "$2"
	else
		FAIL=$((FAIL + 1))
		printf 'FAIL %s\n' "$2"
		[ $# -gt 2 ] && printf '%s\n' "$3" | sed 's/^/       | /'
	fi
}
HELPERS=()
# The EXIT trap is this shell's alone. A `cmd &` child is a fork of this shell
# until it execs, and killed in that window it runs the inherited trap itself:
# on CI the bystander's copy did, and its rm -rf took $WORK out from under the
# rest of the run (gd-uMfSuaQlCV). /proc/self tells such a copy from this
# shell; $BASHPID does not (its first read in the copy can be this shell's pid).
cleanup() {
	local h self
	read -r self _ </proc/self/stat
	[ "$self" = "$$" ] || return 0
	for h in "${HELPERS[@]}"; do kill "$h" 2>/dev/null || true; done
	GARDENIO_SSM_STATE_DIR="$WORK/state" bash "$SCRIPT" stop >/dev/null 2>&1 || true
	rm -rf "$WORK"
}
trap cleanup EXIT

# kill -0 succeeds on a zombie; in a container with no init to reap orphans a
# killed session lingers as one, so count state Z as dead.
alive() {
	local st
	kill -0 "$1" 2>/dev/null || return 1
	st="$(awk '{print $3}' "/proc/$1/stat" 2>/dev/null)" || return 1
	[ "$st" != Z ]
}

mkdir -p "$WORK/bin" "$WORK/state" "$WORK/home"
CALLS="$WORK/aws-calls"
PORT="$((20000 + RANDOM % 20000))"

cat >"$WORK/bin/aws" <<STUB
#!/usr/bin/env bash
printf '%s\n' "\$*" >> "$CALLS"
case "\$*" in
*"ec2 describe-instances"*) printf 'i-0aaa111\ti-0bbb222\n'; exit 0 ;;
*"ssm start-session"*)
	python3 -c '
import socket, sys, time
s = socket.socket(); s.setsockopt(socket.SOL_SOCKET, socket.SO_REUSEADDR, 1)
s.bind(("127.0.0.1", $PORT)); s.listen(5)
print("Starting session with SessionId: stub", flush=True)
time.sleep(600)
' </dev/null
	exit 0
	;;
esac
exit 0
STUB
chmod +x "$WORK/bin/aws"
export PATH="$WORK/bin:$PATH"
export HOME="$WORK/home" AWS_PROFILE=gardenio AWS_REGION=us-east-1
export GARDENIO_SSM_LOCAL_PORT="$PORT" GARDENIO_SSM_STATE_DIR="$WORK/state"
export GARDENIO_SSM_SOURCE_KUBECONFIG="$WORK/src.yaml" GARDENIO_SSM_TARGET_KUBECONFIG="$WORK/dst.yaml"
export GARDENIO_SSM_WAIT_SECS=10

have_kubectl=1
command -v kubectl >/dev/null || have_kubectl=0

b64() { printf '%s' "$1" | base64 | tr -d '\n'; }
CA_MARK="CAMARKER-$$"
CERT_MARK="CERTMARKER-$$"
KEY_MARK="KEYMARKER-$$"
cat >"$WORK/src.yaml" <<YAML
apiVersion: v1
kind: Config
clusters:
- name: default
  cluster:
    server: https://k3s.clonk.tv:6443
    certificate-authority-data: $(b64 "$CA_MARK")
users:
- name: default
  user:
    client-certificate-data: $(b64 "$CERT_MARK")
    client-key-data: $(b64 "$KEY_MARK")
contexts:
- name: default
  context: {cluster: default, user: default}
current-context: default
YAML

# usage text, unknown subcommand
out="$(bash "$SCRIPT" bogus 2>&1)" && rc=0 || rc=$?
if [ "$rc" -ne 0 ] && [[ "$out" == *usage* ]]; then note ok "unknown subcommand is rejected with usage"; else note FAIL "unknown subcommand is rejected with usage" "$out"; fi

# status before start: exit 1
if bash "$SCRIPT" status >/dev/null 2>&1; then note FAIL "status is 1 when nothing is running"; else note ok "status is 1 when nothing is running"; fi

if [ "$have_kubectl" = 1 ]; then
	out="$(bash "$SCRIPT" start 2>&1)" && rc=0 || rc=$?
	if [ "$rc" -eq 0 ]; then note ok "start succeeds and returns (detached)"; else note FAIL "start succeeds and returns (detached)" "$out"; fi

	calls="$(cat "$CALLS" 2>/dev/null || true)"
	[[ "$calls" == *"--profile gardenio"* && "$calls" == *"--region us-east-1"* ]] && note ok "aws called with profile gardenio and region us-east-1" || note FAIL "aws profile/region" "$calls"
	[[ "$calls" == *"tag:k3s:role,Values=server"* && "$calls" == *"instance-state-name,Values=running"* ]] && note ok "instance lookup filters k3s:role=server, running" || note FAIL "instance lookup filters" "$calls"
	[[ "$calls" == *"ssm start-session --target i-0aaa111 --document-name AWS-StartPortForwardingSession --parameters portNumber=6443,localPortNumber=$PORT"* ]] && note ok "start-session args: first server, port-forward doc, 6443 -> local port" || note FAIL "start-session args" "$calls"

	if [ -s "$WORK/state/kubectl-ssm.pid" ] && kill -0 "$(cat "$WORK/state/kubectl-ssm.pid")" 2>/dev/null; then note ok "session survives the end of start; pid recorded"; else note FAIL "session survives the end of start; pid recorded"; fi
	bash "$SCRIPT" status >/dev/null 2>&1 && note ok "status is 0 while the session is live" || note FAIL "status is 0 while the session is live"

	pid1="$(cat "$WORK/state/kubectl-ssm.pid")"
	out2="$(bash "$SCRIPT" start 2>&1)" && rc=0 || rc=$?
	pid2="$(cat "$WORK/state/kubectl-ssm.pid")"
	n="$(grep -c 'ssm start-session' "$CALLS")"
	if [ "$rc" -eq 0 ] && [ "$pid1" = "$pid2" ] && [ "$n" = 1 ]; then note ok "start is idempotent: reuses the live session"; else note FAIL "start is idempotent: reuses the live session" "$out2 (pids $pid1/$pid2, sessions $n)"; fi

	srv="$(kubectl --kubeconfig "$WORK/dst.yaml" config view --raw -o jsonpath="{.clusters[?(@.name=='gardenio-ssm')].cluster.server}")"
	tsn="$(kubectl --kubeconfig "$WORK/dst.yaml" config view --raw -o jsonpath="{.clusters[?(@.name=='gardenio-ssm')].cluster.tls-server-name}")"
	[ "$srv" = "https://127.0.0.1:$PORT" ] && [ "$tsn" = "k3s.clonk.tv" ] && note ok "context gardenio-ssm: server 127.0.0.1, tls-server-name k3s.clonk.tv" || note FAIL "context gardenio-ssm server/tls-server-name" "server=$srv tls-server-name=$tsn"
	kubectl --kubeconfig "$WORK/dst.yaml" config get-contexts gardenio-ssm >/dev/null 2>&1 && note ok "context gardenio-ssm exists in the target kubeconfig" || note FAIL "context gardenio-ssm exists"
	creds="$(kubectl --kubeconfig "$WORK/dst.yaml" config view --raw -o jsonpath="{.clusters[?(@.name=='gardenio-ssm')].cluster.certificate-authority-data}{.users[?(@.name=='gardenio-ssm')].user.client-certificate-data}{.users[?(@.name=='gardenio-ssm')].user.client-key-data}")"
	[ "$creds" = "$(b64 "$CA_MARK")$(b64 "$CERT_MARK")$(b64 "$KEY_MARK")" ] && note ok "CA and client credentials carried over into the new context" || note FAIL "credentials carried over"

	all="$out
$out2"
	leak=0
	for m in "$CA_MARK" "$CERT_MARK" "$KEY_MARK" "$(b64 "$CA_MARK")" "$(b64 "$CERT_MARK")" "$(b64 "$KEY_MARK")"; do
		[[ "$all" == *"$m"* ]] && leak=1
	done
	[ "$leak" = 0 ] && note ok "start never prints certificate or key data" || note FAIL "start never prints certificate or key data" "$all"
else
	echo "SKIP kubeconfig/start cases: kubectl not on PATH"
fi

# stop kills only the recorded pid
# The helper must not inherit the command substitution's stdout, or $(...)
# would block until the sleep exits.
sleep 600 >/dev/null 2>&1 &
bystander=$!
HELPERS+=("$bystander")
# Until it execs, the bystander is a copy of this shell, not a sleep: wait for
# the exec, so the cases below kill and spare a real unrelated process.
for _ in $(seq 50); do
	[ "$(cat "/proc/$bystander/comm" 2>/dev/null)" = sleep ] && break
	sleep 0.1
done
if [ "$have_kubectl" = 1 ]; then
	pid="$(cat "$WORK/state/kubectl-ssm.pid")"
	bash "$SCRIPT" stop >/dev/null 2>&1
	sleep 0.5
	if ! alive "$pid" && [ ! -f "$WORK/state/kubectl-ssm.pid" ]; then note ok "stop kills the recorded session and clears the pidfile"; else note FAIL "stop kills the recorded session and clears the pidfile"; fi
	bash "$SCRIPT" status >/dev/null 2>&1 && note FAIL "status is 1 after stop" || note ok "status is 1 after stop"
fi
if kill -0 "$bystander" 2>/dev/null; then note ok "stop leaves unrelated processes alone"; else note FAIL "stop leaves unrelated processes alone"; fi

# a stale pidfile pointing at an unrelated live process is never signalled
echo "$bystander" >"$WORK/state/kubectl-ssm.pid"
bash "$SCRIPT" stop >/dev/null 2>&1
if kill -0 "$bystander" 2>/dev/null; then note ok "stale pidfile naming a non-ssm process is not killed"; else note FAIL "stale pidfile naming a non-ssm process is not killed"; fi
kill "$bystander" 2>/dev/null || true

# From here on kubectl is a stub too, so the cases below run on a runner with
# no kubectl. They check which session is opened, not the kubeconfig (the real
# kubectl cases above do that): the stub answers the server and token lookups
# in write_context and touches the kubeconfig it is told to write.
cat >"$WORK/bin/kubectl" <<'STUB'
#!/usr/bin/env bash
kc=""
prev=""
for a in "$@"; do
	[ "$prev" = --kubeconfig ] && kc="$a"
	prev="$a"
done
case "$*" in
*"config view"*".server}"*) printf 'https://k3s.clonk.tv:6443' ;;
*"config view"*".token}"*) printf 'stub-token' ;;
*"config set-"*) [ -n "$kc" ] && touch "$kc" ;;
esac
exit 0
STUB
chmod +x "$WORK/bin/kubectl"

# no server instance -> clear error
cat >"$WORK/bin/aws" <<'STUB'
#!/usr/bin/env bash
case "$*" in *describe-instances*) echo None; exit 0 ;; esac
exit 0
STUB
out="$(bash "$SCRIPT" start 2>&1)" && rc=0 || rc=$?
[ "$rc" -ne 0 ] && [[ "$out" == *"no running instance"* ]] && note ok "no running server instance fails clearly" || note FAIL "no running server instance fails clearly" "$out"

# Edge relay and auto fallback. This stub tells the edge filter from the
# server filter, answers the private-IP lookup, and listens only for the
# RemoteHost document unless STUB_SERVER=listen; with STUB_SERVER=stall the
# server session never opens the port and records its pid.
cat >"$WORK/bin/aws" <<STUB
#!/usr/bin/env bash
printf '%s\n' "\$*" >> "$CALLS"
listen() {
	python3 -c '
import socket, sys, time
s = socket.socket(); s.setsockopt(socket.SOL_SOCKET, socket.SO_REUSEADDR, 1)
s.bind(("127.0.0.1", $PORT)); s.listen(5)
print("Starting session with SessionId: stub", flush=True)
time.sleep(600)
' </dev/null
}
case "\$*" in
*"tag:Name,Values=gardenio-k3s-ziti-edge"*) printf 'i-0edge999\n' ;;
*"tag:k3s:role,Values=server"*) printf 'i-0aaa111\ti-0bbb222\n' ;;
*"--instance-ids i-0aaa111"*PrivateIpAddress*) printf '10.0.1.23\n' ;;
*"ssm start-session"*AWS-StartPortForwardingSessionToRemoteHost*) listen ;;
*"ssm start-session"*)
	if [ "\${STUB_SERVER:-listen}" = stall ]; then
		echo "\$\$" > "$WORK/stalled.pid"
		sleep 600 </dev/null; exit 0
	fi
	listen
	;;
esac
exit 0
STUB
chmod +x "$WORK/bin/aws"
EDGE_CALL="ssm start-session --target i-0edge999 --document-name AWS-StartPortForwardingSessionToRemoteHost --parameters host=10.0.1.23,portNumber=6443,localPortNumber=$PORT"

out="$(GARDENIO_SSM_VIA=bogus bash "$SCRIPT" start 2>&1)" && rc=0 || rc=$?
[ "$rc" -ne 0 ] && [[ "$out" == *"GARDENIO_SSM_VIA must be"* ]] && note ok "unknown GARDENIO_SSM_VIA is rejected" || note FAIL "unknown GARDENIO_SSM_VIA is rejected" "$out"

# edge relay
: >"$CALLS"
out="$(GARDENIO_SSM_VIA=edge bash "$SCRIPT" start 2>&1)" && rc=0 || rc=$?
calls="$(cat "$CALLS")"
[ "$rc" -eq 0 ] && [[ "$calls" == *"tag:Name,Values=gardenio-k3s-ziti-edge"* && "$calls" == *"$EDGE_CALL"* ]] && note ok "VIA=edge: RemoteHost session on the edge instance to the server's private IP" || note FAIL "VIA=edge: RemoteHost session on the edge instance to the server's private IP" "$out
$calls"
n="$(grep -c 'ssm start-session' "$CALLS")"
[ "$n" = 1 ] && note ok "VIA=edge: no server-direct session is tried" || note FAIL "VIA=edge: no server-direct session is tried" "$calls"
bash "$SCRIPT" status >/dev/null 2>&1 && note ok "VIA=edge: status is 0 while the relay is live" || note FAIL "VIA=edge: status is 0 while the relay is live"
bash "$SCRIPT" stop >/dev/null 2>&1
sleep 0.5

# auto falls back to the edge when the server session never opens the port
: >"$CALLS"
rm -f "$WORK/stalled.pid"
out="$(STUB_SERVER=stall GARDENIO_SSM_WAIT_SECS=2 GARDENIO_SSM_VIA=auto bash "$SCRIPT" start 2>&1)" && rc=0 || rc=$?
sessions="$(grep 'ssm start-session' "$CALLS")"
first="$(sed -n 1p <<<"$sessions")"
second="$(sed -n 2p <<<"$sessions")"
[ "$rc" -eq 0 ] && [[ "$first" == *"--target i-0aaa111 --document-name AWS-StartPortForwardingSession --parameters"* && "$second" == *"$EDGE_CALL"* ]] && note ok "VIA=auto: server session first, then the edge relay" || note FAIL "VIA=auto: server session first, then the edge relay" "$out
$sessions"
stalled="$(cat "$WORK/stalled.pid" 2>/dev/null)"
[ -n "$stalled" ] && ! alive "$stalled" && note ok "VIA=auto: the stalled server session is stopped before the fallback" || note FAIL "VIA=auto: the stalled server session is stopped before the fallback" "stalled pid=$stalled"
bash "$SCRIPT" status >/dev/null 2>&1 && note ok "VIA=auto: status is 0 on the edge relay" || note FAIL "VIA=auto: status is 0 on the edge relay"
bash "$SCRIPT" stop >/dev/null 2>&1
sleep 0.5

# server only: no fallback, the failure names what was tried
: >"$CALLS"
rm -f "$WORK/stalled.pid"
out="$(STUB_SERVER=stall GARDENIO_SSM_WAIT_SECS=1 GARDENIO_SSM_VIA=server bash "$SCRIPT" start 2>&1)" && rc=0 || rc=$?
n="$(grep -c 'ssm start-session' "$CALLS")"
[ "$rc" -ne 0 ] && [ "$n" = 1 ] && [[ "$out" == *"tried: server"* ]] && note ok "VIA=server: a stalled session fails without the edge, naming what was tried" || note FAIL "VIA=server: a stalled session fails without the edge, naming what was tried" "$out (sessions $n)"
stalled="$(cat "$WORK/stalled.pid" 2>/dev/null)"
[ -n "$stalled" ] && kill "$stalled" 2>/dev/null

printf '\n%d passed, %d failed\n' "$PASS" "$FAIL"
[ "$FAIL" -eq 0 ]
