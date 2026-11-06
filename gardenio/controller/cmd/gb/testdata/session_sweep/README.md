# Session sweep fixtures

Regression fixtures for `gb session sweep` (gardenio/docs/session-sweep.md), one directory
per problem class from the survey (gardenio/docs/session-sweep-survey.md, task
gd-JCoNrApHwa). Each fixture is a real excerpt from an archived agent session, redacted, and
`TestSweepDetectsSurveyedClasses` (session_sweep_fixtures_test.go) runs `sweepDetect` with
the default thresholds over every one of them, with no network and no daemon.

## Layout

```text
session_sweep/<class>/<case>/
  case.json      what the sweep must report (required)
  sessions.json  session beads, as `kd show <id> --json` returns them (a JSON array)
  agents.json    agent beads, the same shape (a JSON array)
  hooks.jsonl    HOOK_EVENTS bus events, one per line, as /v1/bus/events streams them
                 (without the SSE "data:" prefix); the test feeds each through
                 parseSweepBusLine, so only PostToolUse/PostToolUseFailure and the
                 stop-gate's Stop verdicts (stop_gate_decision) are kept
  comments.json  task comments: {"<task bead id>": [{"id","author","text","created_at"}]}
```

Every file but `case.json` is optional. `<case>` starts with `positive` (the class is
present and the sweep must flag it), `negative` (a real session that looks similar but is
healthy; the sweep must report nothing) or `known-miss` (the class is present but the sweep
cannot see it yet; the test skips it, naming the bead that tracks the gap). Every class has
at least one positive and one negative case.

`case.json`:

```json
{
  "class": "ci-poll",
  "sources": ["gd-J9BgbJwiZ4"],
  "note": "one line: what the excerpt shows and why it is (or is not) the class",
  "now": "2026-10-29T17:00:00Z",
  "since": "24h",
  "skip": "",
  "want": [{"pattern": "slow-call", "kind": "ci-wait", "agent": "gardenio-…-g0p", "count": 4800}]
}
```

`now` and `since` are the sweep's clock and window. `want` lists every finding the sweep
must return for the fixture, compared as a multiset on pattern, kind, agent and count; a
negative has `"want": []`, or only another class's findings that the excerpt also shows. `skip` is set only on a known miss.

## Capturing a new fixture from a session id

1. The session bead: `kd show <session-bead> --json`. Keep `id`, `type`, `status`,
   `created_at`, `updated_at` and `fields`; drop `description`, `operated_by_email` and
   the other display fields. Its `agent_bead_id` is the agent bead.
2. The agent bead: `kd show <agent-bead> --json`, the same trim. Keep `wrapup`,
   `task_id` and the wake fields.
3. Tool calls. If the session is still inside the bus retention (a few hours), replay it:
   `curl -sN -H "Authorization: Bearer $TOKEN" "$BEADS_HTTP/v1/bus/events?stream=hooks&since=<RFC3339>"`
   and keep that session's `data:` lines (strip the prefix). Otherwise rebuild them from
   the transcript:

   ```sh
   gb audit fetch <agent-bead> --output /tmp/audit          # ~1 s; every session under the bead
   # pick the session dir whose meta.json started_at matches the bead's (within 10 min)
   python3 transcript_to_bus.py --meta DIR/meta.json --match '<evidence regex>' DIR/*.jsonl > hooks.jsonl
   ```

   `transcript_to_bus.py` emits the relay's shape: tool_input and tool_response redacted
   and cut to 1024 JSON bytes, a cut response's error lines and last line in
   `tool_response_excerpt`, failures in `error`, `duration_ms` from the transcript
   timestamps. `--match` keeps only the calls that carry the evidence.
4. Task comments, for dup-comment: `kd show <task> --json` (its `comments`), as
   `{"<task>": [...]}`.
5. Redact: no tokens, emails or secrets (the script redacts tool payloads; check the bead
   fields by hand). Keep real tool inputs, error text, timestamps and agent names. Keep each
   fixture under 50 KB: cut irrelevant tool calls and unrelated bead fields, not the
   evidence.
6. Write `case.json`, run `go test ./cmd/gb/ -run TestSweepDetectsSurveyedClasses` from
   `gardenio/controller`, and fix `want` or the detector until it is green.
