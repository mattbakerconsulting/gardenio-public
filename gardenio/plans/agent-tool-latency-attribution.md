# Attributing agent slowness to specific tool calls and commands

**Status: PLAN, not started.** Epic `gd-Oc3GVm9gc6` (label `plan`, project
`gardenio`). Origin: Slack thread `gd-P4HTdNsNLx` — "gardenio should have a way
to figure out what commands/tool calls are taking longest for agents, so we can
fix/work around slowness."

Written 2026-09-22 by thread agent `thread-1790106263-729189` against
`main@b2e0f33`. Every claim below is either a file:line in this tree or a live
measurement taken from the running `gardenio-k3s` fleet during the research
session; the live measurements are labelled as such and dated.

Sibling plan: [`claude-session-subagent-tracking.md`](claude-session-subagent-tracking.md)
(PARKED) — same hook pipeline, different question (lineage and forensics, not
latency). Its **P3 durable sink** is this plan's P4; neither plan should build
two sinks. Where they touch, this document defers to it.

---

## 1. The question, and why today's answer is wrong

An operator wants to ask: *what is actually making my agents slow — which tool,
which shell command, on which project?* Today gardenio can answer only the first
half of the first clause, and the number it gives is probably wrong.

What exists (live, 2026-09-22, `gardenio-vmsingle` in namespace `gardenio`):

```promql
histogram_quantile(0.99, sum by (le) (rate(gardenio_agent_tool_call_latency_seconds_bucket[1h])))
→ 44.2 s          # p50 0.64 s, p95 5.36 s
sum(increase(gardenio_agent_tool_call_latency_seconds_count[24h]))
→ 10,646 tool calls / day, fleet-wide
```

That family is emitted per agent pod by coop
(`coop/crates/cli/src/transport/http/metrics.rs:86`) and carries labels
`agent_name`, `project`, `account` — **and no `tool` label**. So the board at
`gardenio/helm/gardenio/dashboards/agent-latency-distribution.json` can draw a
44-second p99 and cannot say whether that is `go build`, `gh run watch`, or a
`grep` over the monorepo. There is no command dimension at all.

Worse, the 44 s is not trustworthy. coop derives each duration by pairing
`ToolBefore`→`ToolAfter` in a FIFO queue **keyed on tool name**
(`coop/crates/cli/src/driver/hook_detect.rs:41-67`, `ToolTimer::finish`). The
code comment calls this "a reasonable approximation". Measured against live
fleet traffic it is not:

> **Live measurement, 2026-09-22 17:00–19:50Z.** Replayed 1,354 hook events from
> the `HOOK_EVENTS` stream (637 `PreToolUse`). **233 of them — 37% — started
> while another call of the same tool name in the same session was still in
> flight**, with up to **7** `Bash` calls overlapping at once. Every one of those
> is a duration attributed to the wrong command: a parallel block of one `grep`
> and one `go build` reports the grep as taking the build's time.

That is the real state: one aggregate number, mis-attributed by construction,
with no drill-down.

## 2. What already exists (do not rebuild any of this)

**The events are already produced and already durable.** This was the first
thing checked, and it is the reason this plan is cheap.

| Link in the chain | Where | State |
|---|---|---|
| Claude Code fires `Pre`/`PostToolUse` per tool call | `.claude/settings.local.json` in every agent pod, from `cmd/gb` setup | live |
| `gb hook relay` transforms and publishes | `gardenio/controller/cmd/gb/hook_relay.go:206-260` | live |
| kbeads publishes to JetStream | `kbeads/internal/server/http_hooks.go` | live |
| `HOOK_EVENTS` stream, subject `hooks.<agent>.<event>` | `kbeads/internal/eventbus/streams.go:12` | live, FileStorage |
| Replay/read API | `GET /v1/bus/events?stream=hooks&since=<RFC3339>` (`kbeads/internal/server/http_bus_sse.go:66-97`) | live, bearer-authed |
| coop archives raw hook JSON per session | `.state/coop/sessions/<uuid>/hook_events.jsonl` | live on-pod; S3 upload **off** in `gardenio-k3s` (`deploy-values/gardenio-k3s.yaml:503,774`) |
| Agent pods are scraped for `/metrics` | vmagent `kubernetes-pods` job, `templates/vmagent/configmap.yaml:35` | live |

**Claude Code already measures the number we want.** Verified in this session's
own `hook_events.jsonl`: every `PostToolUse` payload carries

```json
{"hook_event_name":"PostToolUse","tool_name":"Bash","tool_use_id":"toolu_014m2…",
 "duration_ms":415,"prompt_id":"…","tool_input":{…},"tool_response":{…}}
```

`duration_ms` is Claude's own measurement of the tool execution, and
`tool_use_id` is the exact pairing key. **`gb hook relay` reads neither**
(`hook_relay.go:230-236` extracts only `tool_name`, `tool_input`,
`tool_response`), and coop's detector reads neither
(`coop/crates/cli/src/driver/hook_recv.rs:166,185` parse `tool_name` only;
`HookEvent::ToolAfter` at `coop/crates/cli/src/driver/mod.rs:266` has no room
for them). Both ends throw away the authoritative value and then guess it.

## 3. The gaps, in the order they matter

1. **The measurement is discarded.** `duration_ms` + `tool_use_id` exist on the
   payload and are dropped at both consumers. Everything else follows from this.
2. **No tool dimension.** The only latency family has no `tool` label, so no
   drill-down of any kind is possible.
3. **No command dimension, and no arguments.** `Bash` is one label value
   covering `sed -n 1,40p` and `make preflight`. The operator's question is
   mostly *about Bash*, and it is about the **whole command line**: `go test
   ./...` and `go test ./cmd/x -run TestY -v` are different problems wearing the
   same name. A finer label does not fix this — see §4.1.
4. **Nothing consumes the stream.** `bridge.HookStream`
   (`controller/internal/bridge/hookstream.go:229`) is the only subscriber and
   ships in the disabled slack-bridge. The events reach no consumer in prod.
5. **Retention is volume-dependent, not time-bounded.** `HOOK_EVENTS` takes the
   default 10,000 messages / 100 MB, `DiscardOld`, **no `MaxAge`, no per-subject
   floor** (`streams.go:183-189`). Measured fleet volume on 2026-09-22 was ~480
   hook events/hour, so the ring currently holds roughly a day — but that window
   shrinks with fleet size and is shared with the 5-minute synthetic
   `PreToolUse` heartbeats (`agent_start.go:727-733`). It is not a window you
   can ask "what was slow last Tuesday" against.
6. **No exemplars.** A histogram can say p99 = 44 s. It can never say *which
   command* took 44 s. Answering the operator's question requires keeping some
   actual command text, which requires redaction.

## 4. The one real design decision: where to aggregate

A `tool` label has to be added somewhere. **Where** decides whether this costs
900 series or 49,000.

Live TSDB census, 2026-09-22 (`/api/v1/status/tsdb` on `gardenio-vmsingle`):

| | |
|---|---|
| Total series in vmsingle | **23,304** |
| `gardenio_agent_tool_call_latency_seconds_bucket` | **2,449** (already the #2 metric by series count) |
| …of which live right now | 130 |
| Buckets per series | 13 (`0.01 … 120, +Inf`) |

The family is already 10% of the whole TSDB because it is **per pod**, and agent
pods are ephemeral — every respawn mints a fresh `agent_name`/`pod`/`instance`
triple that lives out the retention window.

| Option | Series cost | Verdict |
|---|---|---|
| **A. Add `tool` to the existing per-pod histogram** | ~20 tools × 15 series × every pod that ever ran → **~49k**, i.e. 2× the entire current TSDB | **Rejected** |
| **B. Aggregate per-pod, then `labeldrop` pod identity at scrape** | Collapses distinct targets onto one label set → colliding counters, `rate()` garbage | **Rejected** |
| **C. Emit the tool-labelled histogram from ONE central consumer of `HOOK_EVENTS`**, labels `{tool, project}` | 20 × 3 × 15 ≈ **900** series, stable, zero churn | **Recommended** |

Option C also fixes gaps 1, 2, 3, 4 and 6 with one component, and it is the same
consumer the sibling plan's P3 needs — so it is built once, not twice. Per-pod
metrics stay exactly as they are for "which *agent* is slow"; the central
consumer answers "which *tool/command* is slow".

### 4.1 Full argv is the deliverable; the class label is only the roll-up

**The operator's question is "which command", with its arguments — not "was it
Bash".** `go test ./...` and `go test ./cmd/beads-viewer/ -run TestX -v` are the
same class and completely different problems; `grep -rn <pat> .` over the
monorepo and `grep -n <pat> one-file.go` differ by three orders of magnitude in
the live sample. A design that ends at a `cmd_class` label has not answered the
question.

Prometheus cannot be that answer: argv is unbounded, high-entropy text, and a
metric label is the single worst place to put it (it explodes cardinality and
persists for the full retention window — the same rule the
`observability/pii-gateway` catalog already enforces on operator labels). So the
system has **two planes, and the plan builds both**:

| Plane | Holds | Answers |
|---|---|---|
| **Metric** (`{tool, cmd_class, project}` histograms) | bounded counts + buckets, no argv | "is tool latency regressing?", alerting, trend |
| **Record** (slow-call ledger, §P2) | **full argv, redacted**, duration, tool_use_id, agent/session/project, exit status | **"what exactly was slow, and what were its arguments?"** |

The class label exists only so the metric plane stays cheap and so the ledger
can be grouped; it is never the drill-down. The normalizer that produces it
strips leading `cd … &&` and `VAR=…` prefixes, takes the binary basename, and
appends the subcommand for a fixed allowlist (`git`, `go`, `make`, `kd`, `gb`,
`cargo`, `npm`, `helm`, `kubectl`, `aws`, `docker`, `gh`), with everything else
falling into `other` — a hard-capped (~40 value) domain so a novel command can
never widen the series space.

Sizing the record plane, from the live sample: ~10,600 tool calls/day fleet-wide,
of which the ledger keeps only calls over a threshold (say 2 s — 22% of the
sample) plus a per-session top-N. At ~500 bytes of redacted argv per row that is
under 2 MB/day, which is nothing next to the bead store. **Store every slow call
whole rather than sampling percentiles of it.**

## 5. Phases

Each ships alone, with its own bead and PR.

### P0 — Forward what Claude already measures (one file, hours)

Add `duration_ms`, `tool_use_id`, `prompt_id` and `permission_mode` to
`hookRelayEvent` and `buildRelayEvent` (`cmd/gb/hook_relay.go`), stamp
`tool_use_id` on `PreToolUse` too, mirror the fields into `bridge.hookEvent`
(`internal/bridge/hookstream.go`) so they survive unmarshal, and document them
in `gardenio/docs/hook-event-schema.md`.

- **Acceptance:** `TestBuildRelayEvent_PostToolUse` asserts both fields; a live
  `GET /v1/bus/events?stream=hooks` on a rolled pod shows `duration_ms` on real
  traffic.
- **Note:** `duration_ms` is Claude-Code-version dependent. Treat it as
  optional; consumers fall back to derived Pre→Post timing and set a
  `source="derived"` label so the two are never silently mixed.

### P1 — Fix the existing histogram's correctness (coop, small)

Thread `tool_use_id` and `duration_ms` through
`HookEvent::ToolBefore`/`ToolAfter` (`driver/mod.rs:266`, `driver/hook_recv.rs`)
and key `ToolTimer` on `tool_use_id` instead of tool name
(`driver/hook_detect.rs:41-67`), preferring Claude's `duration_ms` when present.
No new labels here — this is purely the 37%-misattribution fix.

- **Acceptance:** a unit test with two overlapping same-name calls of 1 s and
  10 s records 1 s and 10 s, not 10 s and 1 s. Fleet p99 on
  `gardenio_agent_tool_call_latency_seconds` is expected to **drop** once rolled;
  record the before/after in the bead.

### P2 — The consumer: bounded metrics **and** a full-argv slow-call ledger

One `HOOK_EVENTS` durable consumer (controller-side, alongside the existing
`hookstream.go` machinery), two outputs from each `PostToolUse`.

**(a) Metric plane** — the trend/alert surface:

- `gardenio_tool_latency_seconds{tool, project, source}` — histogram, 13 buckets
- `gardenio_bash_cmd_latency_seconds{cmd_class, project, source}` — histogram

**(b) Record plane** — the answer surface. Every call whose duration exceeds a
configurable threshold (default 2 s), plus each session's top-N, is written as a
row with the **complete argument vector**:

```text
slow_tool_calls(
  id, ts, agent_bead_id, session_id, project, role,
  tool_name, tool_use_id, duration_ms, source,          -- 'claude' | 'derived'
  cmd_class,                                            -- normalized, for grouping
  command_redacted TEXT,                                -- FULL argv, secrets scrubbed
  tool_input_redacted JSONB,                            -- non-Bash tools: the real params
  exit_status, output_bytes, is_sidechain)
  index (ts), (cmd_class, ts), (agent_bead_id, ts), (duration_ms DESC)
```

Non-Bash tools keep their real parameters too — a slow `Read` wants its path, a
slow `Agent` call wants its prompt head, a slow MCP call wants its arguments —
under the same truncation and redaction rules.

**Redaction is a precondition of this phase, not a follow-up.** Command text
routinely carries tokens: `GH_TOKEN="$GIT_TOKEN" gh run list …` appears verbatim
in the live sample. Scrub in the pod, before the text leaves it, reusing
`controller/internal/obspii` plus the `observability/pii-gateway/catalog.yaml`
rules; the consumer scrubs again on write (neither end trusts the other, the
same posture vmagent's `metric_relabel_configs` already takes at
`templates/vmagent/configmap.yaml:97-110`). Argv never becomes a metric label.

Both writes must be best-effort log-and-continue: a metrics or DB hiccup must
never touch an agent's tool call. The consumer is durable (not ephemeral) so a
controller restart resumes rather than losing the window.

- **Acceptance:** (1) `histogram_quantile(0.95, sum by (le,tool) (rate(gardenio_tool_latency_seconds_bucket[1h])))`
  returns a per-tool p95 on live data, total new series < 2,000 checked against
  `/api/v1/status/tsdb` a day after rollout; (2) **given any time window, the
  ledger returns the N slowest calls with their full command lines**, e.g.
  "`go test ./cmd/beads-viewer/ -run TestHandleClaudeConfig -v` — 1,235 s,
  agent X, 2026-09-22T18:41Z"; (3) a seeded row containing a token comes back
  redacted.

### P3 — The harness tax (the gardenio-specific question)

With P0+P1 landed we hold two clocks per call: Claude's `duration_ms` (the tool
itself) and the Pre→Post wall clock (which additionally contains gardenio's own
`PreToolUse` hooks — `gb hook relay`, `/hooks/drain-queue.sh` — plus any
permission gate). **Their delta is gardenio's overhead per tool call.** Emit it
as `gardenio_tool_harness_overhead_seconds{phase}` and time each hook command
itself in `gb hook`.

This is the phase that can tell us our own gates are the slowdown, which no
amount of tool-level data can. Measured in-session, `gb squawk` cost 415 ms and
a trivial `sed` 26–40 ms, so the tax is real but small at rest; it has never been
tracked under load.

- **Acceptance:** a board panel showing overhead p50/p95 per hook command;
  an alert if overhead p95 exceeds 1 s.

### P4 — Where the ledger lives, and per-session self-service

P2 defines the ledger's shape and write path; this phase settles its home and
its second reader.

1. **Fleet store.** The `agent_hook_events` table proposed in
   `claude-session-subagent-tracking.md` §P3 is the same sink. **Do not design a
   second one** — this plan only adds the requirement that `duration_ms`,
   `tool_use_id`, `cmd_class` and the redacted argv be first-class indexed
   columns, and that slow-call rows survive independently of the `HOOK_EVENTS`
   ring's eviction. If that plan stays parked, P2's table ships standalone and
   the sibling plan adopts it.
2. **Per session, no cluster needed.** coop already writes every hook payload to
   `.state/coop/sessions/<uuid>/hook_events.jsonl` on the pod, `duration_ms` and
   argv included. A bounded top-N heap over that, written to the session bead at
   `gb session close`, gives every agent and operator "the 10 slowest commands
   this session, with their arguments" on the beads-viewer session page — with
   no dependency on NATS, Postgres, or the S3 archive (which is off in
   `gardenio-k3s`: `deploy-values/gardenio-k3s.yaml:503,774`).

- **Acceptance:** a finished agent's session page lists its slowest calls with
  full (redacted) command lines; the same rows are queryable fleet-wide by
  `cmd_class` and by time window.
### P5 — Surface it

- Extend `dashboards/agent-latency-distribution.json` with a per-tool fan, a
  per-`cmd_class` top-10 bar, and a harness-overhead row (ship via the
  grafana-bridge image, never UI edits — the board's own description says so).
- `gb tools top [--session|--fleet] [--by class|command] [--since 24h]`: the CLI
  an agent or operator runs to get the table in §7 directly, defaulting to **full
  command lines** and offering `--by class` for the rolled-up view. Fleet mode
  queries the P2 ledger (falling back to the replay API); session mode reads the
  local `hook_events.jsonl` and needs no cluster at all.
- A beads-viewer view over the ledger: slowest commands for a window, grouped by
  class, each row expandable to the full argv and the agent/session it ran in.

### P6 — Retention (optional, only if §3.5 bites)

`HOOK_EVENTS` keeps the default caps. If "what was slow last week" becomes a
real question, give it `MaxAge` + `MaxMsgsPerSubject` the way `MESSAGE_EVENTS`
was fixed (`streams.go:221-223`) — or rely on P4's table and leave the ring
alone. Cheaper to decide after P2 shows the real event volume.

## 6. Risks

| Risk | Mitigation |
|---|---|
| **Secrets in command text** | Redaction is a precondition of P2, not a follow-up: scrub in the pod, scrub again on write, never put argv in a metric label. |
| **Cardinality blowup** | Central emitter (§4 option C), hard-capped `cmd_class` domain with an `other` bucket, and a post-rollout `status/tsdb` check in P2's acceptance. |
| **`duration_ms` disappears in a Claude Code upgrade** | Optional field + `source` label + derived fallback (P0). |
| **Ring eviction during a busy day** | P6, or P4's table. Known and bounded, not silent: a durable consumer's lag is observable. |
| **Relay adds per-tool-call latency** | It already runs on every call today; P3 is what finally measures it. |
| **Subagent calls double-count** | Subagent tool calls fire hooks too. Tag with the `isSidechain`/subagent id already carried on the envelope and exclude by default in fleet rollups. |

## 7. What the answer looks like (live, 2026-09-22)

Produced during research by replaying `HOOK_EVENTS` and pairing Pre→Post — i.e.
with today's flawed pairing, no new code. This is the shape the plan makes
trustworthy, per-project, and continuous:

```text
class                    n    sum_s   p50      max    share of tool wall-clock
Bash:grep               75   5246.2   2.93   1343.3   23.6%
Bash:go test            11   3857.3  40.21   1903.4   17.4%
Bash:go build            5   3629.7 499.22   2030.2   16.4%
Bash (unclassified)     17   1339.7  11.24   1103.6    6.0%
Bash:gh                 20   1007.3   3.25    765.4    4.5%
Bash:kd show            39    643.3   1.62    195.8    2.9%
Edit                    30    209.2   0.93    124.4    0.9%
```

And the rows that actually answer the question — the same replay, kept whole
instead of bucketed (truncated here for width; the ledger keeps the full line):

```text
 2030 s  Bash   cd gardenio/controller && go build ./... 2>&1 | tail -30
 1903 s  Bash   cd gardenio/controller && go test ./cmd/beads-viewer/ -run 'TestHandleClaudeConfig_(MultiProject…'
 1343 s  Bash   grep -n "RUN   TestArrayFieldPrimitives_RealPostgres\|PASS: …" …
 1235 s  Bash   go test ./cmd/beads-viewer/ -run TestHandleClaudeConfig -v 2>&1 | tail -100
 1039 s  Bash   go build ./... 2>&1 | tail -30 && echo "BUILD OK" && go test ./cmd/gb/... 2>&1 | tail -60
  777 s  Bash   sleep 15 && GH_TOKEN="$GIT_TOKEN" gh run list --repo … --branch agent/e2e…
  765 s  Bash   GH_TOKEN="$GIT_TOKEN" gh run watch 35765432327 --repo … --interval 20
```

That is the shape §4.1 commits to keeping: the whole command, its arguments, its
duration, and the agent that ran it. Note also what the last two rows expose —
agents burning tens of minutes polling CI from inside a tool call, and doing it
with a token on the command line. Neither is visible in any percentile.

Read the table above with the §1 caveat in mind — the maxima are exactly the mis-attributed
values P1 fixes. Even so the headline is plausible and actionable: **grep, `go
test` and `go build` are ~57% of all agent tool wall-clock**, which points at
concrete fixes (a warm Go build cache in the agent image, steering agents off
repo-wide `grep -rn` onto indexed search) that nobody could have justified
before because nobody could see it.

## 8. Bead anchoring

| Bead | Scope |
|---|---|
| `gd-Oc3GVm9gc6` | Plan epic (label `plan`, project `gardenio`) |
| P0–P6 | `task` children of the epic, one per phase above |

Sequencing: P0 → P1 (independent of P2) and P0 → P2 → {P3, P4, P5}. P4's fleet
half coordinates with the sibling plan's sink decision but is not blocked by it;
its per-session half depends on nothing but P0. P6 is decided after P2.
