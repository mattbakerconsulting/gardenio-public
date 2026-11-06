#!/bin/sh
# SPDX-License-Identifier: BUSL-1.1
# Copyright (c) 2026 Matt Baker <fimbaz@gmail.com>
#
# Fake `gemini --acp` for coop's integration tests: speaks the ACP frames
# captured in the phase 0 spike (acp/*.jsonl) instead of calling a model.
#
#   fake-gemini.sh --acp     JSON-RPC over stdio (one object per line).
#
# The prompt text picks the behaviour:
#   *slow*   streams a fragment and then waits; only session/cancel ends the
#            turn (stopReason cancelled), like acp/cancel.jsonl
#   *fail*   answers the prompt with a JSON-RPC error carrying the quota text
#   *        streams "pong: <prompt>" and ends the turn with usage
#
# Everything the client sends is appended to $FAKE_GEMINI_LOG.
set -eu

log=${FAKE_GEMINI_LOG:-/dev/null}
sid=b70b593d-8f2c-4dba-84ab-1cce54577642
slow_id=

[ "${1:-}" = "--acp" ] || {
	echo "fake-gemini: unsupported args '$*'" >&2
	exit 2
}

reply() { printf '{"jsonrpc":"2.0","id":%s,"result":%s}\n' "$1" "$2"; }
chunk() {
	printf '{"jsonrpc":"2.0","method":"session/update","params":{"sessionId":"%s","update":{"sessionUpdate":"agent_message_chunk","content":{"type":"text","text":"%s"}}}}\n' "$sid" "$1"
}

while IFS= read -r line; do
	printf '%s\n' "$line" >>"$log"
	id=$(printf '%s' "$line" | sed -n 's/.*"id":\([0-9][0-9]*\).*/\1/p')
	method=$(printf '%s' "$line" | sed -n 's/.*"method":"\([^"]*\)".*/\1/p')
	case "$method" in
	initialize)
		reply "$id" '{"protocolVersion":1,"authMethods":[{"id":"gemini-api-key","name":"Gemini API key"}],"agentCapabilities":{"loadSession":true}}'
		;;
	authenticate) reply "$id" '{}' ;;
	session/new)
		reply "$id" "{\"sessionId\":\"$sid\",\"modes\":{\"availableModes\":[{\"id\":\"default\"},{\"id\":\"yolo\"}]}}"
		;;
	session/load)
		chunk "replayed history"
		reply "$id" '{}'
		;;
	session/set_mode | session/set_model) reply "$id" '{}' ;;
	session/prompt)
		case "$line" in
		*slow*)
			slow_id=$id
			chunk "working..."
			;;
		*fail*)
			printf '{"jsonrpc":"2.0","id":%s,"error":{"code":-32603,"message":"[API Error: Resource has been exhausted (e.g. check quota).]"}}\n' "$id"
			;;
		*)
			text=$(printf '%s' "$line" | sed -n 's/.*"text":"\([^"]*\)".*/\1/p')
			chunk "pong: $text"
			reply "$id" '{"stopReason":"end_turn","usage":{"inputTokens":100,"outputTokens":3,"totalTokens":103}}'
			;;
		esac
		;;
	session/cancel)
		if [ -n "$slow_id" ]; then
			reply "$slow_id" '{"stopReason":"cancelled"}'
			slow_id=
		fi
		;;
	esac
done
