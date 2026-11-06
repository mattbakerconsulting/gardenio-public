# Context compaction: settings, per-agent customization, and the resume nudge

> Origin: Slack thread `ka-0000001338` — *"problem: agent went idle after compacting;
> needs nudge. Also research compaction settings so we can customize in agent bead"*.
> Fix bead: `ka-0000007525`.

Two things live here:

1. **The bug** — why a fleet agent goes idle after a compaction, and the coop-side
   auto-resume that fixes it (§1).
2. **The research** — every compaction knob Claude Code actually exposes, how to set
   each one per fleet / project / role / **agent** today, and what a first-class
   agent-bead field would look like (§2–§5).

---

## 1. Why an agent goes idle after compacting

Claude Code finishes a compaction by returning the session to an **empty prompt**.
The pre-compaction turn is gone, no new user turn is pending, so the driver settles
into `AgentState::Idle` and stays there. For an agent that was already done that is
correct. For an agent that was mid-work it is a stall that only a human nudge clears —
exactly the operator report.

Three existing mechanisms all miss it:

| Mechanism | Why it doesn't recover the agent |
| --- | --- |
| beads-viewer **Compact** button (`cmd/beads-viewer/agent_compact.go`) | Sends `/compact` via `coopmux.Nudge` and returns. No follow-up nudge, by design — it only asked for a compaction. |
| `PreCompact` hook → `/tmp/inject-queue.jsonl` (`cmd/gb/hook.go`) | The re-primed context is drained by `drain-queue.sh` on the **next `PostToolUse`**. An idle agent never takes another tool call, so the queue is never drained. |
| `gb init-agent` `monitorExit` (`cmd/gb/init_agent.go`) | Recovers `rate_limited` / `model_rate_limited` / `server_error` / `no_internet` exits. Idle-after-compaction is not an exit, so no branch fires. |

### The fix: `coop::compact_resume`

Coop already sees every compaction — Claude Code fires `SessionStart` with
`source=compact`, which is what triggers coop's transcript snapshot. The resume
watcher hangs off that same signal (`coop/crates/cli/src/compact_resume.rs`, armed
from `transport/http/hooks.rs`):

1. Arm on `SessionStart(source=compact)`, recording `lifecycle.bytes_written` as a
   baseline.
2. Poll the agent state. Compaction itself renders as `Working`, so the watcher waits
   for the agent to settle into `Idle` and **stay** idle for a grace period (45s), so a
   compaction that flows straight into the next turn is never interrupted.
3. Deliver exactly one nudge, then disarm.

It stands down **without** nudging if any of these happen first: other input reaches
the PTY (`bytes_written` moved off the baseline — an operator or `gb` already resumed
the agent), the agent exits, the session shuts down, or the window (10m) elapses.

Because it keys off the hook and not the trigger, it covers auto-compaction, `/compact`
typed by the agent, and the viewer's Compact button alike.

| Env | Default | Meaning |
| --- | --- | --- |
| `COOP_COMPACT_RESUME` | `1` | `0`/`false`/`off`/`no` disables the resume nudge |
| `COOP_COMPACT_RESUME_GRACE_MS` | `45000` | idle dwell required before nudging |
| `COOP_COMPACT_RESUME_WINDOW_MS` | `600000` | give up this long after the compaction |
| `COOP_COMPACT_RESUME_POLL_MS` | `2000` | agent-state poll interval |
| `COOP_COMPACT_RESUME_MESSAGE` | see `DEFAULT_MESSAGE` | the text typed into the agent |

The default message tells the agent to `gb prime`, re-read its claimed bead, and either
continue or close out — which also papers over the never-drained inject queue above.

> The message must stay a **single line** and must not start with `/` or `@`: the nudge
> encoder types it and presses Enter, so a newline submits a half-written prompt and a
> leading `/`/`@` opens a completion menu instead of being read as prose. A unit test
> enforces this.

---

## 2. What Claude Code exposes (2.1.236)

Derived from the shipped binary's internals, so treat exact numbers as
**observed-not-documented** and re-check after a Claude Code upgrade.

### settings.json keys

| Key | Type | Effect |
| --- | --- | --- |
| `autoCompactEnabled` | bool | Master switch for automatic compaction. `false` = the session runs to the hard context limit instead of compacting. |
| `autoCompactWindow` | number \| `"auto"` | Token budget compaction is measured against. `"auto"` = use the model's own window. |
| `precomputeCompactionEnabled` | bool | Precomputes the compaction summary in the background so the actual compaction stalls the turn less. |

### Environment variables

| Env | Effect |
| --- | --- |
| `CLAUDE_CODE_AUTO_COMPACT_WINDOW` | Same as `autoCompactWindow`, and **wins over the setting**. Clamped to `[100_000, 1_000_000]` and then min'd with the model's real window — you can lower the effective window, never raise it past the model. |
| `DISABLE_COMPACT` | Disables compaction *and* the `/compact` command. Only when this is set is `CLAUDE_CODE_MAX_CONTEXT_TOKENS` honored. |
| `CLAUDE_CODE_MAX_CONTEXT_TOKENS` | Hard context cap; ignored unless `DISABLE_COMPACT` is set. |
| `CLAUDE_CODE_COLD_COMPACT` | "Cold" compaction — strips non-essential content more aggressively. |
| `CLAUDE_AUTOCOMPACT_PCT_OVERRIDE` | Moves the trigger percentage. It can only **lower** the threshold (compact earlier), never raise it. |
| `CLAUDE_CODE_MAX_OUTPUT_TOKENS` | Not a compaction knob per se, but it feeds the threshold math below. |
| `CLAUDE_CODE_BLOCKING_LIMIT_OVERRIDE` | Adjusts the hard blocking limit that sits above the compaction threshold. |

### Commands and hooks

- `/compact [custom summarization instructions]` — manual compaction; the trailing text
  is passed to the summarizer, so you can steer what survives.
- `/autocompact` — interactive configuration of the auto-compact window.
- `PreCompact` hook payload carries `trigger` (`manual` | `auto`) and
  `custom_instructions`. gardenio already uses it to enqueue re-prime context.
- `SessionStart` fires with `source=compact` after a compaction — the signal §1 uses.
- A `microcompact` path exists internally but exposes no user-facing knob today.

### Threshold math

```
threshold = min(configuredWindow, modelWindow)
          − min(maxOutputTokens, 20_000)
          − 13_000            # reserve for the compaction summary itself
```

For a 200k window with the usual ≥20k max-output that is **167k ≈ 83.5%** of the raw
window.

> ✅ gardenio's context gauge (`controller/internal/contextwindow/contextwindow.go`)
> now models this **absolute** reserve directly — `EffectiveWindowTokens = window −
> OutputReserveTokens(20k) − SummaryReserveTokens(13k)` — instead of the old
> `AutoCompactFraction = 0.92`. That fraction was wrong in both directions because
> the reserve is absolute, not proportional: on a 200k window 0.92 read "fine" up to
> 184k while compaction fires at ~167k (overstated headroom by ~17k — `ka-0000001225`);
> on a 1M window it read hot at 920k when the real wall is ~967k. The reserves are
> named constants — retune them here if a Claude Code upgrade moves the formula.

---

## 3. Setting these per fleet / project / role / agent — today

No new code is needed. The `claude-settings` config-bead category merges **arbitrary
top-level keys** onto `DefaultUserSettings()` (`MergeOverride`, one level deep), so
every settings.json key and any `env` entry above is already expressible, at any scope.

Precedence (later wins): `global (0:) < project (1:) < role (2:) < project-inline (2~:)
< agent (3:)`.

```bash
# Fleet-wide: compact a bit earlier than default.
kd create claude-settings --type config -d '{"autoCompactWindow": 150000}' \
  -l global

# One role: never auto-compact (long-context model, short sessions).
kd create claude-settings --type config -d '{"autoCompactEnabled": false}' \
  -l role:reviewer

# One agent: env form, which beats the settings key.
kd create claude-settings --type config \
  -d '{"env": {"CLAUDE_CODE_AUTO_COMPACT_WINDOW": "150000"}}' \
  -l agent:my-agent-name
```

> **`COOP_*` is not in scope for this mechanism.** The `env` block in a
> `claude-settings` bead lands in **Claude Code's** environment. The resume-nudge
> knobs in §1 are read by **coop**, which is Claude Code's parent process, so a
> config bead cannot reach them. They are agent-container env vars set by the
> controller's pod spec — fleet-wide today, per-agent only if we do §4.

Check what an agent actually resolves to with the effective-settings view
(`claudeconfig.Effective*`) or the beads-viewer config panel, which walks the same
sorted layers as the merge — **but only when the viewer resolves with the same
`(project, role)` subscriptions as the pod**. The viewer's `/claude-config` page
resolves per-project (filtered by `?project=`, added in Task 3), and a viewer scoped
to the wrong project — or to no project at all — drops every `project:`-labelled bead
the pod itself receives (`internal/advice/matching.go:218-220`) and can disagree with
the merge silently.

---

## 4. If we want a first-class agent-bead *field*

Config beads are the right answer for policy. A named field on the agent bead is the
right answer for a knob operators flip constantly per spawn — that is the bar
`ultracode` cleared. The pattern to copy is `applyUltracode` / `resolveUltracode` in
`controller/cmd/controller/podspec.go`:

1. Read the field off the agent bead (`compaction`, `auto_compact_window`, …).
2. Resolve with the same precedence the other matrix knobs use
   (`ultracode|effort|workflows|model`).
3. Project it into a `BOAT_*` env var on the pod spec.
4. Have `gb setup claude` translate that env var into the settings.json key, forcing it
   over whatever the config-bead layers produced.

Suggested minimal surface, if we do it:

| Agent-bead field | Projects to | settings.json / env |
| --- | --- | --- |
| `auto_compact` (`on`\|`off`) | `BOAT_AUTO_COMPACT` | `autoCompactEnabled` |
| `compact_window` (tokens) | `BOAT_COMPACT_WINDOW` | `CLAUDE_CODE_AUTO_COMPACT_WINDOW` |
| `compact_resume` (`on`\|`off`) | `BOAT_COMPACT_RESUME` | `COOP_COMPACT_RESUME` |

Cost/benefit: three more matrix knobs to thread through podspec, viewer, and the spawn
UI, versus one `kd create claude-settings -l agent:<name>` that works right now. Worth
doing only once we see operators actually flipping these per spawn.

---

## 5. Quick reference

| Want | Do |
| --- | --- |
| Stop agents idling after a compaction | Nothing — on by default in coop (§1). |
| Turn that off | `COOP_COMPACT_RESUME=0` on the agent container (controller pod spec). Not a config bead — see the note in §3. |
| Compact earlier | `autoCompactWindow` (or `CLAUDE_CODE_AUTO_COMPACT_WINDOW`) below the model window. |
| Never compact | `autoCompactEnabled: false`, or `DISABLE_COMPACT=1` to also kill `/compact`. |
| Steer what survives a compaction | `/compact <instructions>`, or a `PreCompact` hook that sets `custom_instructions`. |
| See what an agent resolved | beads-viewer config panel / `claudeconfig` effective settings. |
