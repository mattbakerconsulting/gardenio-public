#!/usr/bin/env bash
# kubectl-ssm.sh -- reach the k3s API through an AWS SSM port-forward.
#
# There is no public 6443 (owner decision gd-PlIleJE1J2 = ssm). AWS IAM is the
# gate: `ssm:StartSession` on a server instance with document
# AWS-StartPortForwardingSession. The same path is the break-glass route when
# the edge, Keycloak and ziti are all down.
#
# A server's own SSM agent needs a route out (the NAT once servers lose their
# public IPv4). The relay path does not: a session on the ziti edge instance
# (public address, IGW route, same `nodes` SG) with document
# AWS-StartPortForwardingSessionToRemoteHost forwards to the server's private
# IP:6443, so kubectl survives a NAT outage. GARDENIO_SSM_VIA picks the path.
#
#   kubectl-ssm.sh start    open the forward (idempotent), write the context
#   kubectl-ssm.sh status   exit 0 if the forward is live and answering, else 1
#   kubectl-ssm.sh stop     kill only the recorded session, never pkill -f
#
# Agent-usable: no prompts, no TTY. `start` returns once 127.0.0.1:<port>
# answers and leaves the session running detached, so a later, separate
# command can run plain `kubectl --context gardenio-ssm get nodes`.
#
# Needs: aws CLI + session-manager-plugin, kubectl, profile `gardenio` allowed
# ssm:StartSession, and the source kubeconfig ~/.kube/gardenio.yaml (see
# gardenio/docs/k3s-cluster.md "Getting a kubeconfig").
#
# Certificate and key data are never printed.
#
# Environment (all optional):
#   AWS_PROFILE / AWS_REGION        default gardenio / us-east-1
#   GARDENIO_SSM_LOCAL_PORT         default 16443
#   GARDENIO_SSM_CONTEXT            default gardenio-ssm
#   GARDENIO_SSM_SOURCE_KUBECONFIG  default ~/.kube/gardenio.yaml
#   GARDENIO_SSM_TARGET_KUBECONFIG  default ~/.kube/config (context is merged here)
#   GARDENIO_SSM_STATE_DIR          default ~/.local/state/gardenio
#   GARDENIO_SSM_WAIT_SECS          default 30
#   GARDENIO_SSM_VIA                server, edge or auto (default auto: server
#                                   first, then edge if the port does not
#                                   answer within WAIT_SECS); edge relays
#                                   through the ziti edge instance

set -euo pipefail

PROFILE="${AWS_PROFILE:-gardenio}"
REGION="${AWS_REGION:-us-east-1}"
PORT="${GARDENIO_SSM_LOCAL_PORT:-16443}"
CONTEXT="${GARDENIO_SSM_CONTEXT:-gardenio-ssm}"
SRC_KUBECONFIG="${GARDENIO_SSM_SOURCE_KUBECONFIG:-$HOME/.kube/gardenio.yaml}"
DST_KUBECONFIG="${GARDENIO_SSM_TARGET_KUBECONFIG:-$HOME/.kube/config}"
STATE_DIR="${GARDENIO_SSM_STATE_DIR:-$HOME/.local/state/gardenio}"
WAIT_SECS="${GARDENIO_SSM_WAIT_SECS:-30}"
VIA="${GARDENIO_SSM_VIA:-auto}"
PIDFILE="$STATE_DIR/kubectl-ssm.pid"
LOGFILE="$STATE_DIR/kubectl-ssm.log"
TMP_DIR=""

# write_context's scratch dir (decoded CA and client key). A single-quoted
# trap on a global covers every exit, including die().
trap 'rm -rf "${TMP_DIR:-}"' EXIT

die() {
	printf 'kubectl-ssm: %s\n' "$*" >&2
	exit 1
}

# Checked before anything else, so a typo fails the same way with or without
# aws and kubectl installed.
case "$VIA" in
auto) VIAS="server edge" ;;
server | edge) VIAS="$VIA" ;;
*) die "GARDENIO_SSM_VIA must be server, edge or auto (got $VIA)" ;;
esac

aws_() { aws --profile "$PROFILE" --region "$REGION" "$@"; }

port_answers() { (exec 3<>"/dev/tcp/127.0.0.1/$PORT") 2>/dev/null; }

# True only for a live process that is really our aws ssm session, so a stale
# pidfile whose PID was recycled is never signalled.
session_alive() {
	local pid cmdline
	[ -f "$PIDFILE" ] || return 1
	pid="$(cat "$PIDFILE")"
	[[ "$pid" =~ ^[0-9]+$ ]] || return 1
	kill -0 "$pid" 2>/dev/null || return 1
	cmdline="$(tr '\0' ' ' <"/proc/$pid/cmdline" 2>/dev/null)" || return 1
	[[ "$cmdline" == *start-session* ]]
}

find_server() {
	local id
	id="$(aws_ ec2 describe-instances \
		--filters 'Name=tag:k3s:role,Values=server' 'Name=instance-state-name,Values=running' \
		--query 'Reservations[].Instances[].InstanceId' --output text)"
	id="${id%%[[:space:]]*}" # first of possibly several
	if [ -z "$id" ] || [ "$id" = None ]; then
		die "no running instance tagged k3s:role=server in $REGION (profile $PROFILE)"
	fi
	printf '%s\n' "$id"
}

find_edge() {
	local id
	id="$(aws_ ec2 describe-instances \
		--filters 'Name=tag:Name,Values=gardenio-k3s-ziti-edge' 'Name=instance-state-name,Values=running' \
		--query 'Reservations[].Instances[].InstanceId' --output text)"
	id="${id%%[[:space:]]*}"
	if [ -z "$id" ] || [ "$id" = None ]; then
		die "no running edge instance tagged Name=gardenio-k3s-ziti-edge in $REGION (profile $PROFILE)"
	fi
	printf '%s\n' "$id"
}

server_private_ip() {
	local ip
	ip="$(aws_ ec2 describe-instances --instance-ids "$1" \
		--query 'Reservations[0].Instances[0].PrivateIpAddress' --output text)"
	if [ -z "$ip" ] || [ "$ip" = None ]; then
		die "no private IP for server $1"
	fi
	printf '%s\n' "$ip"
}

# open_session <via> <server-id>: start one detached forward, record its pid.
open_session() {
	local via=$1 sid=$2 target doc params
	if [ "$via" = edge ]; then
		target="$(find_edge)"
		doc=AWS-StartPortForwardingSessionToRemoteHost
		params="host=$(server_private_ip "$sid"),portNumber=6443,localPortNumber=$PORT"
	else
		target="$sid"
		doc=AWS-StartPortForwardingSession
		params="portNumber=6443,localPortNumber=$PORT"
	fi
	printf 'kubectl-ssm: forwarding via %s (%s)\n' "$via" "$target"
	printf -- '--- via %s (%s)\n' "$via" "$target" >>"$LOGFILE"
	# setsid: own session/process group, so the forward outlives this shell
	# and `stop` can take down aws plus session-manager-plugin together.
	setsid aws --profile "$PROFILE" --region "$REGION" ssm start-session --target "$target" \
		--document-name "$doc" --parameters "$params" </dev/null >>"$LOGFILE" 2>&1 &
	printf '%s\n' "$!" >"$PIDFILE"
}

# wait_port <pid>: true once the local port answers; false if the session
# exits or WAIT_SECS pass first.
wait_port() {
	local i
	for ((i = 0; i < WAIT_SECS * 5; i++)); do
		port_answers && return 0
		kill -0 "$1" 2>/dev/null || break
		sleep 0.2
	done
	port_answers
}

# Derive $CONTEXT in $DST_KUBECONFIG from $SRC_KUBECONFIG: same CA and
# credentials, server https://127.0.0.1:$PORT, tls-server-name = the original
# API host so verification works whatever the certificate's SANs include.
write_context() {
	[ -f "$SRC_KUBECONFIG" ] || die "source kubeconfig $SRC_KUBECONFIG not found (gardenio/docs/k3s-cluster.md, 'Getting a kubeconfig')"
	local tmp server host
	TMP_DIR="$(mktemp -d)"
	tmp="$TMP_DIR"
	chmod 700 "$tmp"
	local view=(kubectl --kubeconfig "$SRC_KUBECONFIG" config view --raw --minify)
	server="$("${view[@]}" -o jsonpath='{.clusters[0].cluster.server}')"
	[ -n "$server" ] || die "$SRC_KUBECONFIG has no usable current-context"
	host="${server#*://}"
	host="${host%%[:/]*}"

	local ca cert key token
	ca="$("${view[@]}" -o jsonpath='{.clusters[0].cluster.certificate-authority-data}')"
	cert="$("${view[@]}" -o jsonpath='{.users[0].user.client-certificate-data}')"
	key="$("${view[@]}" -o jsonpath='{.users[0].user.client-key-data}')"
	token="$("${view[@]}" -o jsonpath='{.users[0].user.token}')"

	mkdir -p "$(dirname "$DST_KUBECONFIG")"
	local k=(kubectl --kubeconfig "$DST_KUBECONFIG" config)
	local cl=("${k[@]}" set-cluster "$CONTEXT" --server "https://127.0.0.1:$PORT" --tls-server-name "$host")
	if [ -n "$ca" ]; then
		printf '%s' "$ca" | base64 -d >"$tmp/ca"
		cl+=(--certificate-authority "$tmp/ca" --embed-certs=true)
	fi
	"${cl[@]}" >/dev/null
	if [ -n "$cert" ] && [ -n "$key" ]; then
		printf '%s' "$cert" | base64 -d >"$tmp/cert"
		printf '%s' "$key" | base64 -d >"$tmp/key"
		"${k[@]}" set-credentials "$CONTEXT" --client-certificate "$tmp/cert" --client-key "$tmp/key" --embed-certs=true >/dev/null
	elif [ -n "$token" ]; then
		# kubectl takes the token as an argument, so it is visible to `ps` for
		# the instant this runs; the gardenio kubeconfig uses a client cert.
		"${k[@]}" set-credentials "$CONTEXT" --token "$token" >/dev/null
	else
		die "$SRC_KUBECONFIG user has neither client cert/key nor token"
	fi
	"${k[@]}" set-context "$CONTEXT" --cluster "$CONTEXT" --user "$CONTEXT" >/dev/null
	chmod 600 "$DST_KUBECONFIG"
	printf 'kubectl-ssm: context %s written to %s\n' "$CONTEXT" "$DST_KUBECONFIG"
}

cmd_start() {
	command -v aws >/dev/null || die "aws CLI not found"
	command -v kubectl >/dev/null || die "kubectl not found"
	mkdir -p "$STATE_DIR"
	if session_alive && port_answers; then
		echo "kubectl-ssm: reusing live session (pid $(cat "$PIDFILE"))"
	else
		# A dead or non-answering recorded session: clear it, then start fresh.
		session_alive && cmd_stop
		rm -f "$PIDFILE"
		local id via pid=""
		id="$(find_server)"
		: >"$LOGFILE"
		chmod 600 "$LOGFILE"
		for via in $VIAS; do
			open_session "$via" "$id"
			pid="$(cat "$PIDFILE")"
			wait_port "$pid" && break
			cmd_stop >/dev/null 2>&1 || true
			pid=""
		done
		if [ -z "$pid" ]; then
			printf 'kubectl-ssm: 127.0.0.1:%s never answered; last session output:\n' "$PORT" >&2
			tail -n 20 "$LOGFILE" >&2 || true
			die "is session-manager-plugin installed and profile $PROFILE allowed ssm:StartSession for server $id? (tried: $VIAS)"
		fi
		echo "kubectl-ssm: forwarding 127.0.0.1:$PORT -> $id:6443 via $via (pid $pid)"
	fi
	write_context
	echo "kubectl-ssm: use: kubectl --context $CONTEXT get nodes"
}

cmd_status() {
	if session_alive && port_answers; then
		echo "kubectl-ssm: up (pid $(cat "$PIDFILE"), 127.0.0.1:$PORT)"
		return 0
	fi
	echo "kubectl-ssm: down"
	return 1
}

cmd_stop() {
	if session_alive; then
		local pid
		pid="$(cat "$PIDFILE")"
		# The session leader's pgid is its pid: this takes the aws CLI and its
		# session-manager-plugin child, and nothing else.
		kill -- "-$pid" 2>/dev/null || kill "$pid" 2>/dev/null || true
		echo "kubectl-ssm: stopped session $pid"
	else
		echo "kubectl-ssm: no live recorded session"
	fi
	rm -f "$PIDFILE"
}

case "${1:-}" in
start) cmd_start ;;
status) cmd_status ;;
stop) cmd_stop ;;
*) die "usage: $(basename "$0") start|status|stop" ;;
esac
