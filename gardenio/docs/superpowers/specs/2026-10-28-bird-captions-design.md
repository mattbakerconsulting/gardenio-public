# Bird captions: what each call is for (design spec)

**Status:** built, 2026-10-30: every step has merged or is in review, and §15 records what shipped and where it
differs from this design. Drafted 2026-10-28 as step 1 (task gd-4coyNrCKd9) of plan gd-jMl4oF2cJO, asked in
thread gd-JmsR3RWvVd. Steps 2-10 are already filed as the plan's children; §13 says which section each one
implements and what this spec changes in its scope.
**Ask (owner, in the thread):** a bird says only "shell" for most calls. Most calls already carry a description,
and Haiku could write one cheaply when they don't. Draw it readably on a backdrop, leave it up longer, make a
long press open the agent's terminal, and keep all of them as a transcript.

Every code claim was re-read on `main` at 44405251. Paths are relative to `gardenio/controller/` unless they
start with `gardenio/`. The figures in §2 come from the live `HOOK_EVENTS` stream, read on 2026-10-28.

---

## 0. Summary

1. **A caption is one line of words, at most 80 characters** (§5), made **in the hub** (beads-viewer), never in
   the agent pod (§6). Most captions are the Bash tool's own `description`. 92% of Bash calls have one, and
   their median length is 36 characters. Next come a file's repo-relative path, a search pattern, a URL's host,
   and a subagent's description. Haiku writes a caption only for a Bash call with no description, about 8% of
   calls (§8).
2. **The relay keeps the description from being cut** (§6.2). Today `gb hook relay` truncates `tool_input` to
   1024 bytes of JSON. `description` sorts after `command`, so it is lost on 14% of Bash calls. The relay will
   copy it into its own field before truncating.
3. **The wire gains one field, `cap`**, on events that start a call and on `doing` (§7). Nothing else from
   `tool_input` leaves the server. The scrubbing rules are in §5.3, and `TestActivityNoToolInputOnTheWire`
   keeps checking them. Estimated cost: the Everyone page goes from 6.9 KB/s to about 9.7 KB/s. The ceiling
   is 12 KB/s.
4. **Haiku is fenced in on every side** (§8): `claude-haiku-4-5` through LiteLLM, an LRU of 4096 entries, at
   most 30 calls a minute and 4 at once across the farm, a 3 s timeout, 5000 calls a day, and a kill switch.
   Estimated spend is about $0.40 a day. A bird never waits on the model: Haiku's caption arrives later as a
   `doing` update.
5. **The label is a plate** of at most two lines that **lingers 6 s** after its bird. There is one per gardener
   and at most 24 on the farm (§9).
6. **Opening the Glass from a label** (§10): long-press a label or a bird for 500 ms, moving less than 10 px
   (the Commons' gesture), or right-click it. The keyboard has `w` (watch) and `t` (transcript) for the
   gardener under the cursor. All three need `data-glass`.
7. **The transcript is an in-memory ring of 200 calls per gardener** (§11). It is cut per viewer, served at
   `/garden/farm/activity/...`, and pushed live only to a stream that asked for it. It is not stored as beads:
   that would be about 14k beads a day.
8. **Viewers** (§12): captions go wherever the activity event goes, which is admin-only today. Under the
   per-viewer cut, a scoped viewer gets them for its own gardeners, with bead ids masked.

## 1. Decisions

| # | Question | Answer |
|---|---|---|
| D1 | Caption shape and cap | One line of words, ≤ 80 runes, no path outside the repo, no secret, no URL beyond its host (§5). |
| D2 | Where captions are made | In the hub (`internal/gardenhub`, in beads-viewer). The relay only copies `description` and `file_path` before it truncates (§6). |
| D3 | Which sources | Deterministic first: Bash/Monitor `description`, the file path, the Grep/Glob pattern, the WebFetch host, the WebSearch query, the Agent `description`, built-in tool phrases, the MCP tool name in words. **Haiku only for Bash with no description** (§5.2, §8). |
| D4 | Viewer tiers | Captions go with the activity cut. Admin-only today. Under gd-QPIHOFzCXc a scoped viewer gets captions for its own gardeners, with bead ids masked (§12). |
| D5 | Haiku budget | `AGENT_DEFAULT_HAIKU_MODEL` (`anthropic/claude-haiku-4-5`) via LiteLLM; LRU 4096; 30/min; 4 concurrent; 3 s timeout; 5000/day ceiling; `max_tokens` 32; kill switch `FARM_CAPTION_HAIKU=off` (§8). |
| D6 | Linger | 6 s after the flight ends, fading over the last 300 ms. One per gardener, 24 on the farm (§9.2). |
| D7 | Press gesture | Long press of 500 ms that moves under 10 px, on any pointer, or right-click/`contextmenu`. Keyboard: `w` opens the Glass and `t` the transcript for the gardener at the cursor (§10). |
| D8 | Transcript storage | An in-memory ring, 200 rows per gardener, forgotten with the agent. No turn beads (§11). |

## 2. Research: what the hooks carry (measured)

### 2.1 The sample

On 2026-10-28 the `HOOK_EVENTS` JetStream stream held 10,018 events from 09:03Z to 16:44Z. Its limit is
10,000 messages, which today is about 7.7 hours. I read every event with an ordered consumer from the agent
pod. That throwaway probe was deleted and is not in this PR.

- **4,402 PreToolUse events from 235 agents**, all from the `claude` harness. The window has no Codex or
  Gemini calls.
- **Tool mix:** Bash 4,243 (96.4%), Read 71, Agent 32, ToolSearch 21, Monitor 13, Write 7, Edit 4, TaskStop 4,
  ScheduleWakeup 4, and Skill, SendFeedback, ReportFindings 1 each. There were **no MCP calls**, and no Grep,
  Glob, WebFetch or WebSearch. The fleet's pods expose no Grep/Glob tools, and their system prompt sends reads
  through Bash. That is why "shell" is nearly everything the owner sees.
- **Bash classes** (`toollatency.ClassifyCmd`, of the Bash inputs that parse): `other` 52.8%, `kd_show` 15.3%,
  `git_other` 10.5%, `gb_other` 8.7%, `kd_other` 5.2%, then a tail. **About 59% of all birds say "shell"**:
  the `other` class plus every truncated input, which classifies as `other`.
- **Peak rate:** 77 PreToolUse events a minute farm-wide (16:23Z), and 3 a second for one agent. The average
  is about 11 a minute.
- **Payload:** the median PreToolUse is 861 bytes on the bus, p90 1,512 and max 1,665.

### 2.2 `tool_input` per tool, after the relay

`cmd/gb/hook_relay.go` runs `tool_input` through `redactToolInput` (`toollatency.RedactJSON`) and then
`truncateAny(…, 1024)`. **When the JSON is over 1024 bytes, `tool_input` becomes a string**: the first 1024
bytes plus `...`. `json.Marshal` of a map sorts its keys, so whatever sorts late is lost first.

| Tool | Keys seen | Caption field | Survives truncation? |
|---|---|---|---|
| Bash | `command`, `description`, `timeout`, `run_in_background` | `description` | **No.** `command` sorts first. 583 of 4,243 inputs (13.7%) were truncated, and only 30 of those still held `"description":` in the kept prefix. |
| Monitor | `command`, `description`, `timeout_ms` | `description` | As for Bash (2 of 13 truncated). |
| Read | `file_path` (+ `offset`, `limit`) | `file_path` | Yes: it is small. |
| Edit | `file_path`, `new_string`, `old_string`, `replace_all` | `file_path` | Yes: it sorts first. |
| Write | `content`, `file_path` | `file_path` | **No.** `content` sorts first. All 7 were truncated. |
| Agent / Task | `description`, `prompt`, `subagent_type` | `description` | Yes: it sorts first. All 32 were truncated, and all still began `{"description":`. |
| Grep / Glob | `pattern`, `path`, `glob`, … | `pattern` | Usually (none in the window). |
| WebFetch / WebSearch | `prompt`, `url` / `query` | the `url` host / `query` | WebFetch: not when `prompt` is long. |
| ToolSearch, Skill, TaskStop, ScheduleWakeup | `query`, `skill`, `task_id`, `stop` | a fixed phrase per tool (§5.2) | n/a |
| `mcp__<server>__<tool>` | anything | the tool name in words | n/a |

### 2.3 Descriptions

- **3,382 of 3,660 Bash inputs that parse (92.4%) have a non-empty `description`.** Counting the truncated
  inputs, only 80.4% of Bash calls carry a description the hub can read today. The fix in §6.2 brings this
  back to about 92%.
- **Length:** p50 36 characters, p90 48, p99 59, max 105. Only 6 of 3,382 are over 80, so an 80-character cap
  almost never cuts one.
- **Hygiene:** none had a 32-or-longer token run, 3 already carried the relay's `REDACTED` marker, and 4 had a
  `/`-led token. All four were URL routes such as "Find /w/ handling in beads-viewer on main", not file paths.
  Typical examples: "Show task, claim it, and read team news", "Poll CI status for three refreshed PRs",
  "Trial-merge main into #1282 in a detached worktree".
- **Bash calls with no description** (278): by class, `other` 188, `git_other` 43, `kd_show` 20, `gb_other` 16.
  111 of them began with `cd`, and 26 began with a shell variable set to the agent's scratch path. These are
  what Haiku sees (§8).

### 2.4 The contract today (fixed by this spec's neighbours)

- `internal/gardenhub/activity.go`: a hook event becomes a **class** and a **tool word** (`ParseHookEvent`).
  `tool_input` is read only to classify a Bash command, and is never kept. Events are at most 4 a second per
  gardener (`activityMinGap` 250 ms). Each stream is folded over 250 ms, 500 ms, 1 s, 2 s or 4 s so that it gets
  at most **64 events a second** (`activityStreamRate`, gd-iAq4va9XDt). A viewer's queue holds 64 and drops
  the newest when full. Events are cut per viewer (`keep`), then to the viewport (`setView`, gd-fBGGNdgrLc).
- `gardenio/docs/homestead-farm-read-model.md`, "Activity": the `activity` event is about 60 bytes,
  `{id, doing, tool, at, dur, n?}`. `TestActivityViewportBytes` (60 gardeners at 4 calls/s) measures 6.9 KB/s
  for the Everyone page, 5.6 KB/s for a worst-case 10-plot Plan view, and 25 B/s at the Project level.
  **Privacy rule:** "`tool_input` is read only to classify a Bash command and is never kept"
  (`TestActivityNoToolInputOnTheWire`).
- `web/activity.js`: holds each gardener's doing and writes the card's line, `doing: go test · 4 s ago · 212
  calls this session`. `toolWord(t, short)` turns `other` into "shell".
- `web/birds.js`: the newest call's word flies beside its bird (the flock leader's), in 10 px bold with a 1 px
  halo (`TAG_PX`, `TAG`). It fades over the bird's last 300 ms (`TAG_FADE`), and is placed clear of every face,
  bubble, sign tip, cursor ring and bird (`place`, `sides`, `keepOff`). At most 24 birds are live (`MAX`), at
  most 3 in a flock (`FLOCK`). `TAG_CAP` (128) is the size of the baked-label cache, not a length in characters.
- Routes: `GET /garden/farm`, `/garden/farm/events` and `POST /garden/farm/events/view` are `routeAdminOnly`
  (`cmd/beads-viewer/route_classes.go`).
- The Glass: `/garden?glass=<agent bead id>`. The page shows it only when `body[data-glass="1"]`, that is, when
  the viewer may watch (`TierViewer` in `internal/glass/tiers.go`).
- beads-viewer has **no model credentials** today. `internal/namegen` (slack-bridge) is the only Haiku client
  in Go: a raw `POST /v1/messages` with a 10 s timeout.

## 3. What a caption is for

The tool word answers "what kind of thing is it doing", and the class and the species of the bird already say
that. The caption answers "what is it doing it **for**", in the agent's own words where it gave them. Here are a
caption and the tool word it stands beside:

| Today | With a caption |
|---|---|
| shell | Trial-merge main into #1282 in a detached worktree |
| kd show | Show task, claim it, and read team news |
| Read | Read web/birds.js |
| subagent | Explore gardenhub transcript endpoints |

## 4. Non-goals

- Showing a command, a file's content, an output, or a diff. The Glass is for that.
- Captions for a sidechain call (a call inside a subagent). The hawk stands for the subagent, and its caption is
  the Agent `description`.
- Summarising a run of calls into a sentence ("refactoring the fold"). That is a later idea: it needs a model
  over many calls and has its own privacy questions.

## 5. The caption

### 5.1 Shape

- **One line of words, at most 80 runes** after scrubbing. Whitespace is collapsed, and control characters and
  surrounding quotes or backticks are dropped. A longer caption is cut at a word boundary and ends in `…`.
- Sentence case as given, with no trailing period added or removed. It is imperative, the way the harness
  writes descriptions ("Show plan ready frontier").
- It may be empty. The plate then shows the tool word as it does today (§9.1).

### 5.2 Sources, in order (`captionOf(tool, in)`, pure)

| # | Tool | Caption | `src` (metric label) |
|---|---|---|---|
| 1 | Bash, Monitor | `description` | `description` |
| 2 | Read, Edit, Write, MultiEdit, NotebookEdit, NotebookRead | "Read" / "Edit" / "Write" + the path (§5.3) | `path` |
| 3 | Grep | `Search for "<pattern>"`, pattern ≤ 40 runes | `pattern` |
| 3 | Glob | `Find files <pattern>` | `pattern` |
| 4 | WebFetch | `Fetch <host>` (host only) | `host` |
| 4 | WebSearch | `Search the web: <query>`, query ≤ 50 runes | `host` |
| 5 | Agent, Task | `description` (the hawk's caption) | `agent` |
| 6 | ToolSearch, Skill, TaskStop, TaskCreate, ScheduleWakeup, Monitor without a description, SendMessage, TodoWrite… | a fixed phrase per tool: "Load tools", "Use skill <name>" (the name ≤ 40 runes), "Stop a background task", "Schedule a wake-up", … | `builtin` |
| 7 | `mcp__<server>__<tool>` | `<server>: <tool>` in words ("slack: post message") | `mcp` |
| 8 | Bash with no description | **Haiku** (§8), and until it answers, empty | `haiku` / `cache` |
| 9 | anything else | empty: the tool word stands | `none` |

**MCP does not go to Haiku** (this changes step 3's scope). An MCP input can be any payload: a Slack message, a
document, a ticket body. That is much riskier to send anywhere than a shell command. Its tool name in words
already says what it does, and the window had no MCP calls. This can be revisited with numbers.

**Codex and Gemini.** Codex's shell call has no description, so it falls to Haiku (row 8). Gemini's
`run_shell_command` has a `description` and maps to row 1. Neither appeared in the sample, so step 2 tests both
with fixtures.

### 5.3 Scrubbing (`scrubCaption`, applied to every caption, Haiku's output included)

1. `toollatency.RedactText` (the same patterns as the relay's first layer and the ledger's second).
2. **Paths.** A token that looks like an absolute or `~/` path:
   - inside the agent's repo checkout (`…/work/<rest>`, or under the event's `cwd` when that is in the
     checkout): shown as `<rest>` (`gardenio/controller/internal/gardenhub/web/birds.js` is shortened to
     `web/birds.js`, the last two segments, when the full form would overflow);
   - anywhere else (scratch dirs, `/tmp`, `$HOME`, `/proc`, `/etc`): **the basename only** (`desc_samples.txt`).
   A URL route such as `/w/` or `/api/slack` is short (≤ 2 segments, no dot in the last one), is not a file
   path, and is kept.
3. **URLs:** host only (`https://github.com/x/y/pull/9` becomes `github.com`). An `#1282` or a `PR 1287` stays.
4. **Opaque tokens:** any run of 32 or more `[A-Za-z0-9_\-+/=]` becomes `…`. Email addresses are removed.
5. **Bead ids** (`[a-z]{2}-[A-Za-z0-9]{10}`, `ka-\d{10}`) are kept for an unrestricted viewer and masked for a
   scoped one (§12).
6. Then the 80-rune cut of §5.1.

The rules are pure and run in the hub, so they live beside `toolWord` in `activity.go` (or `caption.go`), with
a table test built from the §2.3 samples and planted secrets.

## 6. Where captions are made

### 6.1 The hub, not the relay

| | Relay (in the pod) | Hub (beads-viewer) |
|---|---|---|
| Sees the full `tool_input` | yes | no: only what survives 1024 bytes |
| Can call Haiku without slowing the agent | **no**: a hook blocks the tool call | yes: asynchronous |
| One cache, one rate cap, one kill switch, one key | no: one per pod, so 235 caches and 235 keys | yes |
| Scrubbing rules change without an agent image | no: a release window plus pod restarts | yes: one deploy |
| Old pods during rollout | n/a | must tolerate missing fields |

So **captions are made in the hub**. The relay does only what the hub cannot: it rescues the two fields that
truncation destroys.

### 6.2 The relay change (in step 2)

`hookRelayEvent` gains two fields, filled from the raw `tool_input` **before** `truncateAny`, each run through
`toollatency.RedactText` and cut at 256 bytes:

```go
ToolDescription string `json:"tool_description,omitempty"` // Bash/Monitor/Agent/Task "description"
ToolFilePath    string `json:"tool_file_path,omitempty"`   // Read/Edit/Write/MultiEdit/Notebook* "file_path"
```

For PreToolUse only, since the caption is made when the call starts. That adds at most 512 bytes to an event
that is already up to about 1.7 KB, with no new secret surface: both values are already inside `tool_input`.
`ParseHookEvent` prefers these fields and falls back to parsing `tool_input`. Until the agent image carrying
the new `gb` rolls out, captions work for the inputs that are not truncated (about 80% of Bash calls).

## 7. On the wire

### 7.1 Shapes

```jsonc
// event: activity: unchanged, plus cap on an event that starts a call
{ "id": "gd-gWork1", "doing": "shell", "tool": "other", "at": 1792870452, "dur": 0,
  "cap": "Trial-merge main into #1282 in a detached worktree" }
// event: doing: gardeners[].doing, plus cap; also sent when a late (Haiku) caption lands, see 7.2
{ "id": "gd-gWork1", "class": "shell", "tool": "other", "at": 1792870452, "calls": 213, "cap": "…" }
```

- `cap` is `omitempty`. It is set only on an event whose calls started (`started()` ≥ 1, or `n` ≥ 1). A finish,
  a thought and the hawk's landing carry none. A fold carries the cap of **its newest start**, not of its newest
  event.
- `FarmDoing.Cap` is the cap of the gardener's newest started call. It is baked into `gardeners[].doing` like
  the rest. Old clients ignore it, because the key is added under `meta.schema=1`.
- `HookActivity` gains `Cap` and `Src`. **It still has no field for `tool_input`.**
  `TestActivityNoToolInputOnTheWire` is rewritten to plant secrets, absolute paths, session ids and emails in
  every `tool_input` field and in `tool_description` / `tool_file_path`. It asserts that none reach the stream
  or the document, and that a planted plain description does reach them, scrubbed.

### 7.2 A late caption (Haiku)

The start event goes out at once with the deterministic caption, which is empty for a Bash call with no
description. When Haiku answers, the hub sets the open call's caption and the doing's `cap`, and sends one
`event: doing` for that gardener. It follows the activity event's rules: the cut, the view (Plan level and
below, the gardener in `ids`, or no view yet) and the queue. The page puts the caption on that gardener's
newest plate if its `at` matches, and otherwise only on the doing and the transcript. Haiku is limited to 30 a
minute farm-wide, so these updates cost nothing next to the budget.

### 7.3 Budget

Captions are made once per call in the hub, not per viewer. The stream's **events a second do not change**:
the fold tiers and the 64/s rate still decide them. **Bytes** grow. With the measured lengths (median 36,
p90 48) a start event gains about 49 bytes (`,"cap":"…"`), and about half of the events in the 60×4 test are
starts:

| Page (`TestActivityViewportBytes`, 60 gardeners × 4 calls/s) | Today | Estimate | Ceiling (step 2 asserts it) |
|---|---|---|---|
| Everyone, at the Plan level | 6.9 KB/s | ~9.7 KB/s | **12 KB/s** |
| 10-plot Plan view, worst case | 5.6 KB/s | ~7.9 KB/s | **10 KB/s** |
| Project level (`busy` only) | 25 B/s | 25 B/s | unchanged |

If the measured figure is over its ceiling, the wire cap drops to 48 runes (the p90) before anything else
changes. The plate never needs more (§9.1), and the transcript keeps the full 80.

## 8. The Haiku fallback

**When:** a Bash PreToolUse whose caption is empty after §5.2 rows 1-7, from a gardener in the current set
(never a pool shell, never a sidechain call). Nothing else goes to Haiku.

**What it is sent:** only the **scrubbed command**: `RedactText`, paths reduced as in §5.3, at most 400 bytes,
with a leading `cd <dir> &&` and scratch-variable assignments stripped. It is never sent other `tool_input`
fields, cwd, the session, the agent's name or its task. The prompt is fixed:

> System: You write the caption for a shell command an AI coding agent is running, for a status display. Reply
> with only the caption: an imperative phrase of at most eight words, no quotes, no trailing period, no file
> paths, no secrets. User: `<command>`

Haiku's reply is untrusted and passes through the same scrubber (§5.3). A reply that is empty, over 80 runes
after scrubbing, or multi-line is dropped (`src="none"`).

**Bounds:**

| Knob | Value | Why |
|---|---|---|
| Model | `AGENT_DEFAULT_HAIKU_MODEL` (default `anthropic/claude-haiku-4-5`), via LiteLLM | The dated id is refused by the fleet's LiteLLM ACL (config.go). Spend goes to `x-litellm-tags: component:beads-viewer,feature:bird-captions`. |
| `max_tokens` / temperature | 32 / 0 | Eight words. Deterministic for the cache. |
| Cache | in-process LRU, 4096 entries, keyed by SHA-256 of the scrubbed command, 24 h TTL | Repeats are free. Haiku 4.5's minimum cacheable prompt is 4096 tokens, so prompt caching cannot help a ~300-token prompt. This LRU is the cache. |
| Rate | token bucket, 30 a minute farm-wide | The measured peak is 77 calls a minute × 8% ≈ 6 a minute, so this is 5× headroom. |
| Concurrency | 4 in flight | |
| Timeout | 3 s, no retry | After 3 s the bird is gone and the plate has lingered half its time. A late caption still lands in the transcript (§11). |
| Daily ceiling | 5000 calls (UTC day) | About $2 a day at most. |
| Over a bound | no call, `src="none"`, counted | A bird never waits and is never queued. |
| Kill switch | `FARM_CAPTION_HAIKU=off` (Helm `farm.captions.haiku.enabled`) | Deterministic captions only. A second switch, `FARM_CAPTIONS=off`, drops `cap` entirely and restores today's wire. |
| Credentials | a LiteLLM virtual key mounted into beads-viewer | beads-viewer has none today. That is a secret and Helm-lane change, so step 3 files the escalation. Without the key, Haiku is simply off. |

**Cost:** about 13,800 PreToolUse a day (the window's 573 an hour, an upper bound) × 7.6% with no description ≈
1,050 candidates a day, about 800 after cache hits. At about 300 input tokens and up to 24 output tokens, a call
costs $0.0003 + $0.00012 ≈ **$0.0004**. That comes to **about $0.35-0.45 a day**, or roughly $13 a month.
Without the relay fix, 19.6% of Bash calls have no readable description, which roughly doubles the spend and
puts the 30/min cap closer to the busy minutes.

**Metrics:** `gardenio_farm_caption_total{src}` (`description`, `path`, `pattern`, `host`, `agent`, `builtin`,
`mcp`, `haiku`, `cache`, `none`), `gardenio_farm_caption_haiku_seconds` (histogram),
`gardenio_farm_caption_haiku_skipped_total{why="rate"|"concurrency"|"daily"|"off"|"timeout"|"error"}`.

## 9. On the page

> Amended 2026-11-03 (gd-CykqEHeEpX, thread gd-7pPL5Ef3VS): the owner asked that "captions don't float with the birds, but the
> birds float up and just leave a caption on the calling agent's head". The plate now perches over the gardener's head from
> the call, bird or no bird, for LINGER (solid at once, fading out over its last 300 ms), and the bird floats up off the head and away; a gardener at home flies its birds from
> its figure in its home strip's barn. 9.1's "the flock leader carries it" and 9.2's "when a bird's flight ends" are
> superseded; `web/birds.js`'s header states the current rule.

### 9.1 The plate (step 4)

- The caption goes on a **rounded plate**: radius 4 px, padding 3 × 5 px. The plate is filled with the label's
  halo colour (`TAG.day[1]` / `TAG.night[1]`) at 92% alpha and ringed with 1 px of its ink at 35%. The text is
  `TAG_PX` (10 px) bold in the ink colour, so contrast is as high as today's halo.
- **Width:** at most 200 CSS px on a phone (viewport < 600 px) and 240 px otherwise. It wraps at word boundaries
  to **at most two lines**. The second line ends in `…` if it still overflows (about 64 characters on a phone).
  The full caption is in the transcript.
- **With no caption**, the plate shows the tool word ("shell"), so every bird keeps a word.
- The flock leader carries it, and the hawk carries the Agent `description`. Placement (`place`, `sides`,
  `keepOff`) uses the plate's box instead of the text's box. A plate that cannot be placed is dropped, as a
  word is today, and is never drawn over a face.
- Baked once per caption, theme and DPR, as today. `TAG_CAP` (the bake cache size) goes to 256, because
  captions vary far more than tool words.

### 9.2 Lingering (step 5)

- When a bird's flight ends, its plate **perches by its gardener's head for LINGER = 6 s**, fading over the last
  300 ms. It is placed each frame by the same keep-off logic, so a figure walking under it is not covered.
- **One plate per gardener:** the next call's plate replaces it at once. At most `MAX` (24) plates are live
  across the farm, and the oldest lingering one goes first.
- A late Haiku caption (§7.2) replaces the text of the plate that is still up.
- Reduced motion: the still bird perches for `STILL` (1 s) as today, and its plate stays for LINGER and then
  goes with no fade. No plates at the World and Project levels, as there are no birds there.

## 10. From a label to the Glass (step 6)

- **Targets:** a plate (its box plus 4 px), a lingering plate, and a bird (its sprite box). The hit test runs
  against `BIRDS.tags()`, which gains `gid` (the gardener) and `linger`.
- **Long press:** pointerdown on a target, held **500 ms**, moving **under 10 px** (Manhattan distance). These are
  the Commons' `PRESS_MS` and threshold (`web/commons.js`), so the farm and the Commons feel the same. It works
  for every pointer type. At 250 ms the plate's ring brightens, so the press shows it is working. A second
  pointer (a pinch), a camera move, or `pointercancel` cancels it. **Right-click** (`contextmenu`) on a target
  does the same at once on a desktop. A short press keeps today's meaning (the cursor).
- **Action:** if `body[data-glass="1"]`, navigate to `/garden?glass=<gardener id>`. Otherwise toast "The Glass is
  not open to you here". The route checks again (`internal/glass` tiers), so the page check is only for UX.
- **Keyboard:** `w` ("watch") opens the Glass, and `t` the transcript (§11.4), for the gardener standing on the
  cursor's Thing. If several stand there, it is the one with the newest call. They become `CURNAV.action` acts
  `watch` and `transcript`, are said in `#cursay` ("Watching gd-… in the Glass"), and are listed in the help
  legend. Neither key is bound on the board today (`s` and `i` are, in `garden.js`).
- The gardener's card gets a "watch" link next to its doing line, and the doing line itself opens the transcript.

## 11. The transcript

### 11.1 Storage: an in-memory ring (step 7)

Each gardener in the current set has **a ring of the last 200 calls**, next to its `actState` in `activity.go`.
The ring is forgotten with the agent (`activityForget`) and cleared on a new session. A row is:

```jsonc
{ "at": 1792870452, "class": "shell", "tool": "other", "cap": "Trial-merge main into #1282…", "dur": 5120, "ok": true }
```

- **A row is written when the call finishes**, so it has `dur` and `ok`: `ok` is false for
  `PostToolUseFailure`. A call with a Haiku request in flight waits for it, up to the 3 s timeout, before its row
  is written. A call that never finishes is written when `activityOpenForget` lets it go, with `dur` 0 and `ok`
  absent. The call in progress is the gardener's doing, which the panel shows as "now".
- Rows are about 120-160 bytes. 235 gardeners × 200 rows is about 7.5 MB at worst. The bound is the gardener
  set, which is itself capped.
- **Why not turn beads** (`internal/plantranscript`): the sample is about 14k calls a day. Turn beads are for
  messages written at human pace. Writing a bead per call would load the daemon, the bead stores and every
  bead consumer, all for "what happened in the last few minutes". A durable record already exists:
  `HOOK_EVENTS` keeps the last 10k events, and the toollatency ledger keeps slow calls with their argv.
- **A restart empties the rings.** Later, not in v1: on start, refill them from `HOOK_EVENTS` (the last hour,
  through an ordered consumer) with deterministic captions only.

### 11.2 Endpoints

- `GET /garden/farm/activity/{id}?since=<unix>&limit=<n ≤ 200>`: one gardener's rows, newest first. 404 for an id
  the viewer's cut does not keep (never a 403 that confirms it exists).
- `GET /garden/farm/activity?scope=farm|project:<p>|plot:<task id>&since=&limit=<n ≤ 100>`: the rows of every
  gardener in the scope that the cut keeps, merged by `at`. Each row adds `id`. Paginate with `since` on the
  oldest row's `at`.
- Both get the same route class as `/garden/farm` (`routeAdminOnly` today) and the same cut once it is
  per-viewer.

### 11.3 Live rows

`POST /garden/farm/events/view` gains `"transcript": "<id>" | "farm" | "project:<p>" | "plot:<task id>"`, one
per stream, cleared with `""`. Only such a stream gets `event: call`, carrying the row plus `id`, for gardeners
in that scope and in its cut. The farm page's budget is untouched for everyone else. A subscribed stream is
batched to one `calls` event a second, with at most **20 rows a second**. Past that, the batch carries
`"skipped": n`, and the panel says "n more calls" and loads them on scroll. At about 150 B a row, that is at most
**about 3 KB/s** extra, and only while the panel is open.

### 11.4 The panel for one gardener (step 8)

Rows are newest first: time, class glyph (the species' colour), caption (or the tool word), duration, and a
failure mark. A filter by class, a "watch" link, and new rows arrive live. You reach it from the card's doing line, the
`t` key, and the Glass page. A long press stays a single action (the Glass), with no menu.

### 11.5 Plot and farm (step 9)

The same panel over a scope (§11.2), with each row headed by the gardener's name and arms. The first screen is
one request. Bound: 100 rows a page, 20 live rows a second (§11.3). The test from step 9's bead (60 gardeners at
4 calls a second) asserts the 3 KB/s.

## 12. Who sees captions

- **Today:** whoever reaches `/garden/farm/events`, which is `routeAdminOnly`. That is unrestricted viewers
  only.
- **Under the per-viewer cut** (gd-QPIHOFzCXc, built in gd-MnQlEmbUq3): `cap` goes with its event, and the event
  already goes only to a viewer whose cut keeps that gardener (its own projects). Following the "fail closed"
  rule (homestead read model, Authorization), **bead-id-shaped tokens in a caption are masked to "a bead" for a
  scoped viewer**, because the id may name another project's bead. The transcript endpoints and the `call` event
  follow the same rules. Since the §14 re-grade (gd-Kvh3fdmFg0), a scoped viewer's captions and say rows also lose
  dollar amounts, ARNs, AWS hosts, cluster hosts, IPv4 addresses and 12-digit AWS accounts (`maskScoped`): the
  §5.3 scrubber keeps a host and any plain word, which is right for an admin and too much for a project member.
- **The Glass** is gated separately (§10): a caption never gives anyone more than they could already watch.

## 13. Steps: where each is specified

| Step | Bead | Implements | Changes to the bead's scope |
|---|---|---|---|
| 2 Caption contract on the wire | gd-gCHKhIROvB | §5, §6.2, §7 | **Adds the relay fields** (§6.2, `cmd/gb/hook_relay.go`). `cap` only on starting events. Byte ceilings from §7.3. Gemini and Codex fixtures. |
| 3 Haiku fallback | gd-TBAYacR31x | §8 | **Bash only, not MCP** (§5.2). Needs a LiteLLM key escalation for beads-viewer. Ships with Haiku off until the key exists. |
| 4 Plate | gd-TmYAAOBQEH | §9.1 | `TAG_CAP` is the bake cache. Width and line limits from §9.1. |
| 5 Linger | gd-2mYRxayl9b | §9.2 | none |
| 6 Press to Glass | gd-jt6OwmV4Jk | §10 | 10 px threshold (the Commons'), not 8. Keys `w` / `t`. |
| 7 Transcript store | gd-MsTDi7JYVs | §11.1-§11.3 | Ring, not beads (D8). Rows written at finish. The `call` event is batched. |
| 8 One gardener's transcript | gd-kl2CL3X9jQ | §11.4 | none |
| 9 Plot and farm transcript | gd-X5dM2VRgDd | §11.5 | Budget is 20 rows/s, about 3 KB/s per subscribed stream. |
| 10 Docs, dashboards, rollout | gd-FOzMlrXga6 | §8 (switches), §14 | Helm values listed in §14. |

## 14. Flags, rollout, risks

**Helm values (step 10):** `farm.captions.enabled` (true), `farm.captions.haiku.enabled` (false until the key
exists), `farm.captions.haiku.ratePerMinute` (30), `.concurrency` (4), `.timeout` (3s), `.dailyMax` (5000),
`.cacheSize` (4096), `farm.captions.wireCap` (80), `farm.transcript.rows` (200). LINGER stays a constant in
`birds.js`: it is presentation, not cost.

**Rollout:** step 2's hub half works on its own (about 80% of Bash calls get a description). The relay half
rides the next release window's agent image. Haiku switches on after its key is mounted.

**Risks:**
- *A description that leaks.* An agent can write anything in `description`: a path, a hostname, a customer's
  name. Mitigation: the §5.3 scrubber, two redaction layers, admin-only routes today, and masked ids for scoped
  viewers. Residual: free text the scrubber cannot recognise as sensitive. I accept that for an admin-only
  surface. Before the route opens to scoped viewers, re-sample descriptions and grade them. **Re-graded
  2026-10-31 (gd-Kvh3fdmFg0):** 4,461 PreToolUse calls (the whole `HOOK_EVENTS` window, 17:20Z-19:03Z, 3,750
  distinct captions) and 62 live say rows. Nothing sensitive got through: no secret value, email, host,
  address, AWS account or dollar amount; 10 captions carried the relay's `[REDACTED]`. Planted inputs showed the
  scrubber keeps bare cluster hosts, addresses, AWS accounts and dollar amounts, so `maskScoped` now takes those
  out for a scoped viewer (§12).
- *The bird cap hides calls.* At 24 birds and one plate per gardener, a busy farm shows only the newest caption
  per gardener. The transcript is where you read everything.
- *Haiku drift or outage.* Captions fall back to the tool word, the counters show it, and the switch turns it off.
- *Restart loses the transcript.* This is accepted for v1 (§11.1), with replay from `HOOK_EVENTS` as the later
  remedy.

## 15. What shipped (step 10, 2026-10-30)

| Step | Bead | PR | What landed |
|---|---|---|---|
| 2 Caption contract | gd-gCHKhIROvB | #1366 | `captionOf` and `scrubCaption` (`internal/gardenhub/caption.go`), `cap` on `activity`, `doing` and `gardeners[].doing`, the relay's `tool_description` / `tool_file_path`. Measured 9.7 KB/s (Everyone) and 7.9 KB/s (10-plot view), under the 12 and 10 KB/s ceilings. |
| 3 Haiku fallback | gd-TBAYacR31x, gd-tGAvOOGeoM | #1451, #1431 | `internal/gardenhub/captioner` with every §8 bound, its counters, and the late caption as one `event: doing`. The key Secret is mounted on beads-viewer (`beadsViewer.captions.haiku.secretName`, escalation gd-25uXUE3HJJ, live since 2026.303.1). |
| 4 Plate | gd-TmYAAOBQEH | #1425 | §9.1 as written. |
| 5 Linger | gd-2mYRxayl9b | #1437 | §9.2 as written. LINGER stays a constant in `birds.js`. |
| 6 Press to Glass | gd-jt6OwmV4Jk | #1453 | §10 as written. |
| 7 Transcript store | gd-MsTDi7JYVs, gd-LEmM2G7iaa | #1467, #1474 | The ring, both endpoints and `event: call`. Paging is by an opaque per-row cursor (`next` / `prev`), not by `since` on the oldest `at` (§11.2), so a page cut inside one second loses no row. |
| 8 One gardener's transcript | gd-kl2CL3X9jQ | #1485 | §11.4. |
| 9 Plot and farm transcript | gd-X5dM2VRgDd | #1519 | §11.5; `TestTranscriptCallBytes` measures 2.7 KB/s per subscribed stream against the 3 KB/s ceiling. |
| 10 Docs, dashboards, rollout | gd-FOzMlrXga6 | this PR | The switches, Helm values, panels and release note below. |

**Differences from §8 and §14:**

- **Helm values live under `beadsViewer`**, not `farm`: `beadsViewer.captions.{enabled, wireCap}`,
  `beadsViewer.captions.haiku.{enabled, secretName, secretKey, ratePerMinute, concurrency, timeout, dailyMax,
  cacheSize}` and `beadsViewer.transcript.rows`, with the §14 defaults. The viewer is what makes captions, and step 3
  had already put the key under `beadsViewer.captions.haiku`.
- **`FARM_CAPTIONS=off`** (`captions.enabled: false`) drops `cap` from the wire **and** from the transcript, and
  asks Haiku nothing. **`FARM_CAPTION_HAIKU`** renders `on` only when captions are on, `haiku.enabled` is true and
  the key Secret is set. **`FARM_CAPTION_WIRE_CAP`** (1-80) and **`FARM_TRANSCRIPT_ROWS`** (1-1000) are env-driven,
  not constants. A value that does not parse, or is out of range, keeps the default.
- **Haiku spend** is a new counter, `gardenio_farm_caption_haiku_tokens_total{type}`, read from each reply's
  `usage`. LiteLLM's tag spend has no `feature` dimension (`gardenio_tag_cost_usd` carries role, project and
  model), so the dashboard prices the tokens at Haiku 4.5's list price ($1 / $5 per million). The bill of record
  stays LiteLLM's.
- **Dashboards:** a board-only row "Farm: bird captions" on Gardenio Garden Health (panel ids 14-18): caption sources,
  Haiku latency against the 3 s timeout, Haiku spend over the window, Haiku calls skipped by `why`, and transcript
  bytes per viewer against 3 KB/s. The boards are baked into the grafana-bridge image, so the panels appear with the
  release that carries them.
- **Rollout:** the k3s deploy values set `haiku.enabled: true`. The relay half of step 2 is already in the agent
  image. Kill switches: `beadsViewer.captions.haiku.enabled=false` (deterministic captions only) or
  `beadsViewer.captions.enabled=false` (no captions), each a `helm upgrade` away. Release note:
  `gardenio/docs/release-notes/2026-10-30-bird-captions.md`.
