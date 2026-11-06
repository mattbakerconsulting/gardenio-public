#!/usr/bin/env python3
"""Rebuild HOOK_EVENTS bus lines from an archived Claude transcript.

The bus keeps only a few hours, so a fixture for an older session is rebuilt
from its transcript (`gb audit fetch <agent-bead>`). Each tool_use and its
tool_result become one PostToolUse (or PostToolUseFailure, when the result is
an error) line in the shape `gb hook relay` publishes and `gb session sweep`
replays (parseSweepBusLine): tool_input and tool_response are redacted and cut
to 1024 JSON bytes the way the relay cuts them, a cut response's error lines
and last line go in tool_response_excerpt, a failure's text goes in
`error`, and duration_ms is derived from the call and result timestamps (the
relay forwards Claude's own measurement; the two agree to about a second).

Usage:
  transcript_to_bus.py --meta DIR/meta.json [--match REGEX] [--max N] DIR/*.jsonl > hooks.jsonl

session_id is the records' sessionId (what the relay sends), not the archive
directory name.

--match keeps only calls whose input, response or error matches REGEX (case
sensitive), so a fixture holds the evidence and not the whole session.
"""

import argparse
import json
import re
import sys
from datetime import datetime, timezone

# Mirrors internal/toollatency/redact.go plus emails, which the fixtures also drop.
_REDACT = [
    (re.compile(r"\b(?:gh[pousr]_[A-Za-z0-9]{20,}|github_pat_[A-Za-z0-9_]{20,}|glpat-[A-Za-z0-9_-]{20,}"
                r"|sk-(?:ant-)?[A-Za-z0-9_-]{20,}|xox[abprs]-[A-Za-z0-9-]{10,}|AKIA[0-9A-Z]{16}"
                r"|eyJ[A-Za-z0-9_-]{10,}\.[A-Za-z0-9_-]{10,}\.[A-Za-z0-9_-]{10,})"), "[REDACTED]"),
    (re.compile(r"(?i)(bearer\s+)[A-Za-z0-9._~+/=-]{8,}"), r"\1[REDACTED]"),
    (re.compile(r"(?i)((?:token|secret|password|passwd|api[_-]?key|credential)s?[\"']?\s*[=:]\s*[\"']?)[^\s\"'&]{6,}"),
     r"\1[REDACTED]"),
    (re.compile(r"[A-Za-z0-9._%+-]+@[A-Za-z0-9.-]+\.[A-Za-z]{2,}"), "[email]"),
]


def redact(v):
    if isinstance(v, str):
        for rx, rep in _REDACT:
            v = rx.sub(rep, v)
        return v
    if isinstance(v, list):
        return [redact(x) for x in v]
    if isinstance(v, dict):
        return {k: redact(x) for k, x in v.items()}
    return v


def go_json(v):
    """json.Marshal: sorted keys, no spaces, HTML-escaped <, > and &."""
    s = json.dumps(v, sort_keys=True, separators=(",", ":"), ensure_ascii=False)
    return s.replace("<", "\\u003c").replace(">", "\\u003e").replace("&", "\\u0026")


def truncate_any(v, max_bytes=1024):
    """hook_relay.go truncateAny: the value, or its JSON cut to a string."""
    if v is None:
        return None
    data = go_json(v).encode()
    if len(data) <= max_bytes:
        return v
    return data[:max_bytes].decode("utf-8", "ignore") + "..."


# hook_relay.go toolResponseErrorLine and toolResponseExcerptSkip.
_ERROR_LINE = re.compile(r"(?i)\b(?:error|fatal|forbidden|denied|refused|unauthori[sz]ed|cannot|can't|failed)\b"
                         r"|not accessible|permission|\bHTTP 4\d\d\b|\bexit (?:code|status) [1-9]")
_EXCERPT_SKIP = {"Read", "Grep", "Glob", "NotebookRead", "WebFetch", "WebSearch", "Edit", "Write", "MultiEdit",
                 "NotebookEdit", "read_file", "write_file", "replace"}


def strings_of(v, out):
    if isinstance(v, str):
        out.append(v)
    elif isinstance(v, list):
        for x in v:
            strings_of(x, out)
    elif isinstance(v, dict):
        for k in sorted(v):
            strings_of(v[k], out)


def clip(s, max_bytes):
    return s.encode()[:max_bytes].decode("utf-8", "ignore")


def response_excerpt(tool, resp, head, max_bytes=1024, line_max=320):
    """hook_relay.go toolResponseExcerpt: error lines and the last line past the cut."""
    if tool in _EXCERPT_SKIP or head is resp or not isinstance(head, str):
        return ""
    parts = []
    strings_of(resp, parts)
    lines = "\n".join(parts).split("\n")
    seen, picked, budget = set(), {}, max_bytes

    def keep(i):
        line = lines[i].rstrip(" \t\r")
        if not line.strip() or line in seen or go_json(line)[1:-1] in head:
            return None
        seen.add(line)
        return clip(line, line_max)

    last = len(lines) - 1
    while last >= 0 and not lines[last].strip():
        last -= 1
    if last >= 0:
        line = keep(last)
        if line is not None:
            picked[last] = line
            budget -= len(line.encode()) + 1
    for i in range(max(last, 0)):
        if not _ERROR_LINE.search(lines[i]):
            continue
        line = keep(i)
        if line is None:
            continue
        if len(line.encode()) + 1 > budget:
            break
        picked[i] = line
        budget -= len(line.encode()) + 1
    return "\n".join(picked[i] for i in sorted(picked))


def parse_ts(s):
    return datetime.fromisoformat(s.replace("Z", "+00:00")).astimezone(timezone.utc)


def text_of(content):
    if isinstance(content, str):
        return content
    out = []
    for c in content or []:
        if isinstance(c, dict) and c.get("type") == "text":
            out.append(c.get("text", ""))
    return "\n".join(out)


def main():
    ap = argparse.ArgumentParser()
    ap.add_argument("--meta", required=True, help="the session's meta.json from gb audit fetch")
    ap.add_argument("--match", help="keep only calls whose input/response/error matches this regex")
    ap.add_argument("--max", type=int, default=0, help="keep at most this many lines")
    ap.add_argument("--session-id", help="override the session id (default: the records' sessionId)")
    ap.add_argument("transcripts", nargs="+")
    a = ap.parse_args()
    meta = json.load(open(a.meta))
    # The bus carries Claude's own session id (the transcript records'
    # sessionId, the basename of the session bead's session_log), which is not
    # the archive directory's coop uuid; a resumed session keeps it.
    session_id = a.session_id or ""
    match = re.compile(a.match) if a.match else None

    calls, results = {}, {}
    for path in a.transcripts:
        for line in open(path):
            try:
                d = json.loads(line)
            except ValueError:
                continue
            if d.get("isSidechain"):
                continue
            if not session_id and d.get("sessionId"):
                session_id = d["sessionId"]
            msg = d.get("message") or {}
            content = msg.get("content")
            if not isinstance(content, list):
                continue
            for c in content:
                if not isinstance(c, dict):
                    continue
                if d.get("type") == "assistant" and c.get("type") == "tool_use":
                    calls[c["id"]] = (d.get("timestamp"), c.get("name"), c.get("input"))
                elif d.get("type") == "user" and c.get("type") == "tool_result":
                    results[c["tool_use_id"]] = (d.get("timestamp"), c, d.get("toolUseResult"))

    n = 0
    for tid, (cts, name, tin) in sorted(calls.items(), key=lambda kv: kv[1][0] or ""):
        if tid not in results:
            continue
        rts, res, structured = results[tid]
        is_err = bool(res.get("is_error"))
        text = text_of(res.get("content"))
        if not is_err and name == "Bash" and isinstance(structured, dict) and "stdout" in structured:
            resp = {k: structured.get(k) for k in ("stdout", "stderr", "interrupted", "isImage") if k in structured}
        else:
            resp = text
        if match and not (match.search(go_json(tin)) or match.search(text)):
            continue
        ts = parse_ts(rts)
        payload = {
            "agent": meta.get("agent_name", ""),
            "agent_bead_id": meta.get("bead_id", ""),
            "project": meta.get("project", ""),
            "role": meta.get("role", ""),
            "session_id": session_id,
            "harness": "claude",
            "event": "PostToolUseFailure" if is_err else "PostToolUse",
            "ts": ts.strftime("%Y-%m-%dT%H:%M:%S.%f000Z"),
            "tool_name": name,
            "tool_input": truncate_any(redact(tin)),
            "tool_use_id": tid,
        }
        if is_err:
            payload["error"] = redact(text)
        else:
            resp = redact(resp)
            payload["tool_response"] = truncate_any(resp)
            excerpt = response_excerpt(name, resp, payload["tool_response"])
            if excerpt:
                payload["tool_response_excerpt"] = excerpt
            if cts:
                payload["duration_ms"] = int((ts - parse_ts(cts)).total_seconds() * 1000)
                payload["duration_source"] = "claude"
        line = {"stream": "hooks", "type": payload["event"],
                "subject": "hooks.%s.%s" % (payload["agent"], payload["event"]),
                "ts": ts.strftime("%Y-%m-%dT%H:%M:%SZ"), "payload": payload}
        sys.stdout.write(go_json(line) + "\n")
        n += 1
        if a.max and n >= a.max:
            break


if __name__ == "__main__":
    main()
