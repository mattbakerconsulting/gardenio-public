#!/usr/bin/env python3
"""Deterministic stub Anthropic Messages API for the claudemore hooks e2e.

This stub stands in for litellm/Anthropic during the RECORD phase of the hooks
e2e (bead ka-0000004452). claudemore runs in LIVE+record mode with this stub as
its upstream, so the REAL `claude` binary drives a full multi-turn tool loop
with **zero real model spend** ($0) and fully deterministic responses. The
cassettes recorded here are then served back by claudemore in REPLAY mode with
no upstream at all.

Why a stub and not a recorded real session: the opus system prompt embeds the
current date (`Today's date is <date>`), which is in claudemore's hashed request
body. A checked-in cassette recorded on one day would MISS on the next
(fail_on_miss → daily-red CI). Recording against this stub *within the same run*
(same clock) sidesteps that entirely — cassettes are ephemeral, never committed.

Scripted conversation (keyed off the request the model sends, not a call count,
so it is robust to retries). Branches are evaluated in this order:

  * haiku model (the title/quota side-call) → a trivial final text.
  * opus, a tool_result present in the conversation → a final text ("Done.").
    (The model just ran a tool; wrap up so it tries to Stop.)
  * opus, the BLOCK_MARKER present in the conversation (the Stop hook blocked and
    injected its reason) AND a DECISION_COMMAND is set → a `tool_use` that runs
    the DECISION_COMMAND, modeling the agent ACTING on the stop-gate block (T6).
  * opus, otherwise (the initial turn) →
      - if STUB_SEED_COMMAND is non-empty: a `tool_use` running it (so
        PreToolUse/PostToolUse fire and a marker-carrying turn is recorded — T5).
      - if STUB_SEED_COMMAND is empty: a final text, so the model's first action
        is to try to Stop (which the T6 gate then blocks before any tool ran).
"""

import http.server
import json
import os
import sys

PORT = int(os.environ.get("STUB_PORT", sys.argv[1] if len(sys.argv) > 1 else "8300"))
SEED_COMMAND = os.environ.get("STUB_SEED_COMMAND", "echo CLAUDEMORE_HOOKS_SEED_TOKEN")
DECISION_COMMAND = os.environ.get("STUB_DECISION_COMMAND", "")
BLOCK_MARKER = os.environ.get("STUB_BLOCK_MARKER", "")


def sse(events):
    return "".join(f"event: {e}\ndata: {json.dumps(d)}\n\n" for e, d in events).encode()


def tool_use_stream(command, tool_use_id):
    return sse(
        [
            ("message_start", {"type": "message_start", "message": {"id": "msg_stub_tooluse", "type": "message", "role": "assistant", "model": "stub", "content": [], "stop_reason": None, "usage": {"input_tokens": 10, "output_tokens": 1}}}),
            ("content_block_start", {"type": "content_block_start", "index": 0, "content_block": {"type": "tool_use", "id": tool_use_id, "name": "Bash", "input": {}}}),
            ("content_block_delta", {"type": "content_block_delta", "index": 0, "delta": {"type": "input_json_delta", "partial_json": json.dumps({"command": command})}}),
            ("content_block_stop", {"type": "content_block_stop", "index": 0}),
            ("message_delta", {"type": "message_delta", "delta": {"stop_reason": "tool_use", "stop_sequence": None}, "usage": {"output_tokens": 10}}),
            ("message_stop", {"type": "message_stop"}),
        ]
    )


def final_stream(text):
    return sse(
        [
            ("message_start", {"type": "message_start", "message": {"id": "msg_stub_final", "type": "message", "role": "assistant", "model": "stub", "content": [], "stop_reason": None, "usage": {"input_tokens": 20, "output_tokens": 1}}}),
            ("content_block_start", {"type": "content_block_start", "index": 0, "content_block": {"type": "text", "text": ""}}),
            ("content_block_delta", {"type": "content_block_delta", "index": 0, "delta": {"type": "text_delta", "text": text}}),
            ("content_block_stop", {"type": "content_block_stop", "index": 0}),
            ("message_delta", {"type": "message_delta", "delta": {"stop_reason": "end_turn", "stop_sequence": None}, "usage": {"output_tokens": 5}}),
            ("message_stop", {"type": "message_stop"}),
        ]
    )


def flatten_conversation(messages):
    """Concatenate all human-readable text in the conversation so the stub can
    branch on whether the stop-gate block reason has been injected."""
    parts = []
    for m in messages:
        content = m.get("content")
        if isinstance(content, str):
            parts.append(content)
        elif isinstance(content, list):
            for blk in content:
                if not isinstance(blk, dict):
                    continue
                if blk.get("type") == "text":
                    parts.append(blk.get("text", ""))
                elif blk.get("type") == "tool_result":
                    rc = blk.get("content")
                    if isinstance(rc, str):
                        parts.append(rc)
                    elif isinstance(rc, list):
                        for x in rc:
                            if isinstance(x, dict):
                                parts.append(x.get("text", ""))
    return "\n".join(parts)


def has_tool_result(messages):
    for m in messages:
        if isinstance(m.get("content"), list):
            for blk in m["content"]:
                if isinstance(blk, dict) and blk.get("type") == "tool_result":
                    return True
    return False


class Handler(http.server.BaseHTTPRequestHandler):
    def do_POST(self):
        length = int(self.headers.get("Content-Length", 0))
        body = self.rfile.read(length) if length else b""
        try:
            d = json.loads(body)
        except json.JSONDecodeError:
            d = {}
        model = d.get("model", "")
        messages = d.get("messages", [])
        stream = bool(d.get("stream", False))
        convo = flatten_conversation(messages)

        if "haiku" in model:
            payload = final_stream("t")
        elif has_tool_result(messages):
            payload = final_stream("Done.")
        elif BLOCK_MARKER and BLOCK_MARKER in convo and DECISION_COMMAND:
            # The Stop hook blocked and injected its reason; act on the block.
            payload = tool_use_stream(DECISION_COMMAND, "toolu_stub_decision")
        elif SEED_COMMAND:
            # Initial turn (T5): run the seed command so the tool hooks fire and a
            # marker-carrying turn is recorded.
            payload = tool_use_stream(SEED_COMMAND, "toolu_stub_seed")
        else:
            # Initial turn with no seed (T6): try to stop immediately, so the
            # Stop-gate blocks BEFORE any tool ran.
            payload = final_stream("Task complete.")

        self.send_response(200)
        self.send_header("Content-Type", "text/event-stream" if stream else "application/json")
        self.send_header("Content-Length", str(len(payload)))
        self.end_headers()
        self.wfile.write(payload)

    def do_GET(self):
        self.send_response(200)
        p = b'{"ok":true}'
        self.send_header("Content-Length", str(len(p)))
        self.end_headers()
        self.wfile.write(p)

    def log_message(self, *args):
        pass  # quiet


if __name__ == "__main__":
    http.server.HTTPServer(("127.0.0.1", PORT), Handler).serve_forever()
