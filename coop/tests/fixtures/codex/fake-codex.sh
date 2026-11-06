#!/bin/sh
# SPDX-License-Identifier: BUSL-1.1
# Copyright (c) 2026 Matt Baker <fimbaz@gmail.com>
#
# Fake `codex` for coop's integration tests: replays the committed spike
# fixtures instead of calling a model.
#
#   fake-codex.sh app-server        JSON-RPC over stdio. Every request id N is
#                                   answered with the slice of
#                                   app-server-session.jsonl that follows
#                                   `{"id":N,"result"` (up to the next response),
#                                   so a client issuing the same request
#                                   sequence as the capture gets the capture.
#   fake-codex.sh exec [...] -      one `exec --json` turn; the prompt arrives on
#                                   stdin. A prompt containing "fail" replays the
#                                   401 stream, anything else the tool-call stream.
#
# Everything the client sends is appended to $FAKE_CODEX_LOG (argv and stdin
# for exec) so tests can assert on the requests.
set -eu

dir=$(dirname "$0")
log=${FAKE_CODEX_LOG:-/dev/null}
mode=${1:-}

case "$mode" in
app-server)
	while IFS= read -r line; do
		printf '%s\n' "$line" >>"$log"
		id=$(printf '%s' "$line" | sed -n 's/^{"id":\([0-9][0-9]*\),"method".*/\1/p')
		[ -n "$id" ] || continue
		awk -v id="$id" '
			BEGIN { start = "{\"id\":" id ",\"result\""; next_id = "{\"id\":" (id + 1) ",\"result\"" }
			index($0, start) == 1 { on = 1 }
			index($0, next_id) == 1 { on = 0 }
			on { print; fflush() }
		' "$dir/app-server-session.jsonl"
	done
	;;
exec)
	prompt=$(cat)
	{
		printf 'argv: %s\n' "$*"
		printf 'stdin: %s\n' "$prompt"
	} >>"$log"
	case "$prompt" in
	*fail*) cat "$dir/exec-turn-failed-401.jsonl" ;;
	*) cat "$dir/exec-tool-stop-gate.jsonl" ;;
	esac
	;;
*)
	echo "fake-codex: unsupported mode '$mode'" >&2
	exit 2
	;;
esac
