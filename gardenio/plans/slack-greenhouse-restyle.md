# Slack "Greenhouse" Visual Restyle

Tracking: gd-pALXjDaPK8, implementation epic gd-XFizxz9y3Y
Slack ask: "Im ready to restyle and refresh the slack interface of gardenio
... I want people to say 'this doesn't look like it comes from the same
harness' ... and also for them to say that the refresh is better." — Matthew
Baker, thread gd-ZU1mxiQkKz

## Status

**Design locked; implementation in progress.** This doc is the plan-of-record
for the visual system. Phase 1 (the `Theme` struct + `card_theme` project
field + beads-viewer editor, `gardenio/controller/internal/bridge/theme.go`)
has landed. Phase 2 (card rendering wired through `themeFor`, below) has
landed. Phase 4 (the Semantic / Tool / Reaction vocabularies, below) extends
the vocabulary to the ~200 hard-coded emoji outside the agent cards; its
tracker is gd-Z3Sgg8W5Ih under gd-XFizxz9y3Y.

Live mockup: https://claude.ai/artifact/TxNUAbK4JJaKnpqswC3ZzK

## Why "Greenhouse"

Three directions were mocked up (Signal: neutral/geometric, Greenhouse:
growth-stage theme tied to the Gardenio name, Field Notes: minimal dev-log
style). Greenhouse was picked because it's the one that actually brands the
bot as *ours* rather than reskinning generic chat-ops chrome — Slack fixes
the font in every direction, so the real design space is icon set, bot
avatar, accent color, and layout, and Greenhouse is the direction that uses
all four coherently.

## What's locked

### Lifecycle states (replaces ✅⛔⏳↗️🔗⚠️🪦↻ one-for-one)

| Glyph | State | Meaning |
|---|---|---|
| 🫘 | Spawning | agent just started |
| 🍃 | Working | in progress |
| 🌿 | Continued | resumed / relayed |
| 🧑‍🌾 | Needs a decision | waiting on a human |
| 🪴 | Handed off | moved to someone else |
| 🌳 | Done | finished clean |
| 🥀 | Blocked | stuck, needs input |
| 🍄 | Aborted | something broke |
| 🍂 | Reaped | cleaned up, went stale |
| 🌾 | Restarted | regrown from the same root |

### Bead types (link-unfurl icons)

| Glyph | Type | Meaning |
|---|---|---|
| 🌱 | Task | a sprout — small, discrete |
| 🐛 | Bug | a pest in the bed (free win — already a pest) |
| 🌸 | Feature | a bloom — new and visible |
| 🌻 | Epic | the big bloom, several tasks under it |
| 🪧 | Decision | a signpost, pick a path |
| 🗺️ | Plan | the plot map |
| 🪺 | Bundle | a nest of things gathered together |
| 🪵 | Doc | a log: the written record |
| 🦜 | Message | the parrot that talks |
| 🦉 | Advice | the wise owl |
| 🥕 | Dashboard | the harvest you measure |
| 🪹 | any other type | an empty nest (`Theme.BeadTypeFallback`) |

The last six were added by gd-vQvHXp4Rl3 so that no type glyph is a heraldry
charge (spec R4); each was a charge no owner-accepted round-2 arms used.

**Glyph-collision fix (this revision):** v1 had Task reusing 🌱 from the
lifecycle state "Working," and Epic on 🌲 — close enough to Done's 🌳 to
misread at a glance. Epic moved to 🌻. The v2 fix moved Task off 🌱 onto 🌰
(acorn), but acorn didn't land — Task is kept on 🌱 (sprout, its natural
read) and Working moved instead, to 🍃 (a single leaf in motion — active,
distinct from 🌿's static herb-sprig and from 🍂's fallen/reaped leaf). All
16 glyphs across both tables are still unique.

### Palette

| Name | Hex |
|---|---|
| Seedling | `#A9C77C` |
| Moss | `#6E8F42` |
| Forest | `#3F6B3A` |
| Harvest | `#C9A13B` |
| Clay | `#B5652E` |
| Wilt | `#8B4C39` |

### Bot avatar

**Leaf mark** (white leaf silhouette on Forest `#3F6B3A`) — picked over the
terracotta pot, trowel+sprout, and watering-can options.

### Accessibility rule (added this revision)

Inline Slack unicode emoji have no settable tooltip or alt text — a screen
reader announces Slack's generic Unicode name ("seedling"), not the state it
stands for ("Working"). Rule, applied everywhere above and in every mockup:

- **Inline unicode** (card headers, status lines, chips): the glyph always
  appears glued to its verbal label in the same text run — `🍃 Working`,
  never a bare 🍃.
- **Real Block Kit icons** (an `image_url` context element, if a
  custom-drawn icon ever replaces a unicode glyph): set `alt_text` to that
  same word, so the accessible name matches the visual pairing either way.

This is also the actual mechanism that catches future collisions: once
glyph+word is mandatory everywhere, a reused glyph reads as a duplicate
*label* immediately, instead of hiding as a lookalike icon.

## Selectable: garden mode vs. business mode

"okay, im into this; but! We need to make it selectable; with a 'business
mode' and a 'garden mode'" — Matthew Baker. Garden mode (above) stays the
default; Business mode is a second, parallel vocabulary for channels/orgs
that want the same structural upgrade (color, accessible pairing, unique
glyph-per-state) without the plant theme.

### Business mode vocabulary

Lifecycle states — plain geometric Unicode, no emoji, same "glyph glued to
verbal label" rule:

| Glyph | State |
|---|---|
| ○ | Spawning |
| ◐ | Working |
| ↻ | Continued |
| ◆ | Needs a decision |
| → | Handed off |
| ● | Done |
| ■ | Blocked |
| ✕ | Aborted |
| ◻ | Reaped |
| ↺ | Restarted |

`↻` (Continued, clockwise) and `↺` (Restarted, counter-clockwise) are
distinct code points on purpose — same relationship as Garden's static 🌿
vs. regrown 🌾.

Bead types — bracket tags, not icons (this is the "Field Notes" direction
from the first round of mockups): `[TASK]` `[BUG]` `[FEAT]` `[EPIC]`
`[DECISION]` `[PLAN]` (and `[BUNDLE]` `[DOC]` `[MESSAGE]` `[ADVICE]` `[DASHBOARD]`, any other type as its word in brackets). Plain text is inherently screen-reader-correct, so
the accessibility rule is satisfied for free in Business mode — no
emoji-name mismatch is possible when there's no emoji.

Palette — one neutral + four semantic accents, replacing Garden's six named
greens/earth tones:

| Name | Hex | Used for |
|---|---|---|
| Slate | `#6B7280` | Spawning / neutral chrome |
| Info | `#2563EB` | Working / Continued / Restarted |
| Success | `#16A34A` | Done |
| Warning | `#D97706` | Blocked / Needs a decision |
| Danger | `#DC2626` | Aborted |
| Muted | `#9CA3AF` | Reaped |

Bot avatar: a plain mid-grey rounded square with a single white chevron
(`›`) — no plant imagery, no leaf mark.

### Selection mechanism

**Revised per operator decision (gd-nzzCKYHhc0, 2026-10-10):** "The flag
should be per project and configurable in beads viewer." A deploy-values /
env-var toggle (the `linkUnfurlChannels` precedent, originally proposed
above) is operator-only and requires a redeploy to flip — too heavy for a
per-project cosmetic preference. The corrected mechanism:

- **Scope: per-project, not per-channel.** `card_theme` is a new scalar
  field on the `type:project` config bead (same bead that already carries
  `git_url`, `image`, `account`, `model`, resource knobs, and the `secrets`
  JSON list — see `gardenio/controller/cmd/beads-viewer/project_config.go`).
  Values: `"garden"` (default) or `"business"`.
- **Configurable in beads-viewer**, not just CLI/YAML: the existing
  admin-gated project editor (`GET /p/{project}/edit`, `POST
  /p/{project}/config` in `project_config.go` +
  `templates/partials/project_config.html`) already edits scalar project
  knobs inline with layered validation (pre-validate via
  `projectValidityReport`, then PATCH the typed field, daemon re-validates
  against `TypeConfig`). `card_theme` is one more field in that same form —
  a `<select>` next to `model`/`image`, no new editor, no new auth path
  (still `gardenio-admin`, global, not project-scoped, matching every other
  knob on that bead).
- **Bridge resolution**: the bridge already resolves an incoming Slack
  channel to its owning project bead for other per-project behavior
  (`bot_notifications.go`, `bot_commands.go`, `jira_webhook.go` all do a
  channel → project lookup today). Card rendering reuses that same lookup:
  `themeFor(channel string) Theme` reads the resolved project's
  `card_theme` field (cached the same way the project's other config is
  cached), falling back to `"garden"` when the field is unset or the
  channel has no resolvable project.
- Still no slash command / per-user override in v1 — a future option, not
  required to ship this.

Both Garden and Business become named `Theme` structs (glyph table +
palette + avatar) rather than literals scattered across `bot_agents.go` /
`bot_agents_wrapup.go` / `link_unfurl.go` / `agentstream.go` — this is the
same "one theme module" shape already called for in the very first
recommendation on this thread (gd-Hdjmzm7stH), now with two members instead
of one, selected by project config instead of a deploy-time env var.

## Implementation

Real build touches, at minimum (now carrying both themes, not just Garden):

- `gardenio/controller/internal/bridge/theme.go` — the `Theme` struct, the
  `Garden`/`Business` instances, and the `themeFor(channel)` resolver.
  **Landed.**
- `gardenio/controller/internal/bridge/bot_agents.go` — `buildAgentCardBlocks`,
  `buildCompactWorkingCardBlocks`, `buildCompactAgentCardBlocks`,
  `outcomeHeaderGlyph`.
- `gardenio/controller/internal/bridge/bot_agents_wrapup.go` —
  `buildWrapUpAgentCardBlocks`, `wrapUpOutcome`, `restartConditionLine`.
- `gardenio/controller/internal/bridge/link_unfurl.go` — `buildUnfurlAttachment`,
  `beadTypeBadge`, `beadStatusEmoji`, `beadStatusColor` (the one place today
  using classic `slack.Attachment` for a colored left bar — color bars don't
  natively exist on Block Kit sections/headers, so extending them to every
  card type, not just unfurls, is part of this work).
- `gardenio/controller/internal/bridge/agentstream.go` — `buildThreadStatusText`.

Test files that assert exact block/emoji content and will need updating
alongside: `agents_card_test.go`, `agents_card_wrapup_edit_test.go`,
`agents_card_ownership_test.go`, `bot_agents_closed_card_test.go`,
`bot_agents_pool_card_test.go`, `thread_cards_post_test.go`,
`thread_cards_terminal_test.go`, `gitlab_mrdesc_test.go`,
`jira_completion_ack_test.go`.

### Phase 2 implementation notes

Resolutions made while wiring, where the spec was silent:

- **State mapping.** The old shortcodes map onto the 10 lifecycle states:
  done→Done, failed/platform-failure→Aborted, blocked/rate-limited/
  scheduling-stuck→Blocked, decision-pending→Needs a decision,
  respawn-armed ("progressing")→Restarted (same identity regrown),
  `successor=new`→Continued, handoff/"moved"→Handed off, reaped→Reaped,
  idle→Spawning (the neutral "not yet active" state; the verbal label says
  "idle"). Platform failure has no dedicated glyph of its own.
- **Glyph glued to label.** Wrap-up and compact cards used to render
  `glyph *name* · label`; they now render `glyph label · name`, and the
  wrap-up header banner reads `glyph label — tldr`. Unfurl metadata pairs the
  type glyph with the type word (`🌻 \`epic\``; Business's `[EPIC]` tag is
  its own label) and the status glyph with the status word.
- **Plan badge.** An unfurled epic carrying the `plan` label takes the plan
  badge (`🗺️` / `[PLAN]`) instead of the epic one.
- **Types outside the themed six** (bundle, doc, agent, …) keep their legacy
  emoji under Garden and get an uppercase bracket tag under Business.
  *Superseded (gd-vQvHXp4Rl3):* the legacy emoji (📦 📄 💬 💡 📊 📍) were
  heraldry charges, which spec R4 forbids for a theme glyph. Garden's
  `BeadType` now carries bundle 🪺, doc 🪵, message 🦜, advice 🦉 and
  dashboard 🥕, and `Theme.BeadTypeFallback` (Garden 🪹; Business empty,
  meaning a bracket tag of the type word) covers every other type. No glyph
  is hard-coded in bridge code any more.
- **No colour bars yet.** `chat.unfurl` rejects an unfurl that mixes blocks
  with legacy attachment fields (`cannot_parse_attachment`), so unfurls stay
  blocks-only and `beadStatusColor` no longer exists. Extending the palette to
  agent cards means moving them to attachments (`chat.update` must also clear
  the old top-level blocks); that is deliberately left as a follow-up.
- **Resolver caching.** `themeFor` runs on every card flush and stream status
  tick, so successful resolutions are cached per channel for 30s.
- **Thread status.** The assistant thread status leads with the Working glyph
  (`Starting up…` with the Spawning glyph).

## Phase 4: Semantic, Tool and Reaction vocabularies

Tracking: gd-Z3Sgg8W5Ih (epic), gd-XJvAqYEBBR (this vocabulary step).

Phases 1-2 themed only the agent-card lifecycle states and bead types. About
200 other hard-coded Slack `:shortcodes:` / unicode emoji remain in command
replies, alert posts, the live-stream summary and message reactions
(`:white_check_mark:`, `:x:`, `:warning:`, `:rocket:`, `:zap:`, `:link:`,
`:pause_button:`, the `agentstream.go` tool table, the `eyes` /
`hourglass_flowing_sand` reactions, …). Phase 4 gives each theme three more
glyph maps so those call sites can resolve through `themeFor` instead. This
section only **defines** the vocabulary; wiring each call site is a separate
step per file group (steps 2-6 of the epic), and a final regression guard
(step 7) fails the build on a new bare shortcode.

Rules carried over unchanged:

- **Task stays 🌱** (`Garden.BeadType["task"]`). No Phase 4 glyph may reuse any
  Lifecycle or BeadType glyph, and no glyph repeats within a map.
- **Accessibility.** Garden glyphs in Semantic and Tool are inline unicode, so
  each call site keeps the glyph glued to its verbal label (`🌼 Spawned`, not a
  bare 🌼). Business glyphs are plain bracket text or plain geometric/ASCII
  marks, never color emoji, so they are their own accessible label.
- **Reactions are exempt from the pairing rule** (a reaction has no text run)
  and are limited to **stock Slack emoji names** — custom emoji are not
  available in every workspace. Names carry no colons.

### Semantic

Cross-cutting meanings that recur across messages. The legacy column is what
the key replaces today.

| Key | Meaning | Legacy | Garden | Business |
|---|---|---|---|---|
| `ok` | succeeded / confirmed | ✅ `:white_check_mark:` | 🌼 | `[OK]` |
| `fail` | failed / error | ❌ `:x:` | 🪨 | `[FAIL]` |
| `warn` | caution / degraded | ⚠️ `:warning:` | 🌵 | `[WARN]` |
| `info` | neutral notice | ℹ️ `:information_source:` | 💧 | `[INFO]` |
| `denied` | permission refused | 🚫 `:no_entry_sign:`, 🔒 `:lock:` | 🚧 | `[DENIED]` |
| `blocked_by` | waiting on another bead | ⛔ `:no_entry:` | 🧱 | `[BLOCKED BY]` |
| `handoff` | work passed on | 🤝 `:handshake:` | 🧤 | `[HANDOFF]` |
| `schedule` | scheduled / timed | ⏰ `:clock1:`, `:alarm_clock:` | ☀️ | `[SCHEDULE]` |
| `trigger` | trigger fired | ⚡ `:zap:` | 🌩️ | `[TRIGGER]` |
| `spawn` | agent spawning | 🚀 `:rocket:` | 🌰 | `[SPAWN]` |
| `release` | release / deploy | 🚀 `:rocket:` | 🚜 | `[RELEASE]` |
| `idle` | agent idle | 💤 `:zzz:` | 🐌 | `[IDLE]` |
| `link` | linked / chained bead | 🔗 `:link:` | 🪢 | `[LINK]` |
| `assignee` | who owns it | 👤 `:bust_in_silhouette:` | 🐝 | `[ASSIGNEE]` |
| `paused` | paused | ⏸ `:pause_button:` | 🌙 | `[PAUSED]` |
| `resumed` | resumed / running | ▶️ `:arrow_forward:` | 🌅 | `[RESUMED]` |
| `stop` | stopped | ⏹ `:black_square_for_stop:` | 🪓 | `[STOP]` |
| `note` | annotation / summary | 📝 `:memo:` | 🪶 | `[NOTE]` |
| `question` | asking something | ❓ `:question:` | 🪻 | `[QUESTION]` |
| `review` | review request | 🔍 `:mag:` | 🐞 | `[REVIEW]` |
| `cost` | spend | 💰 `:moneybag:` | 🍯 | `[COST]` |
| `context` | context window | 🧠 `:brain:` | 🧺 | `[CTX]` |
| `turns` | turn / iteration count | 🔁 `:repeat:` | ♻️ | `[TURNS]` |
| `latency` | time-to-first-token | ⚡ `:zap:` | 🐇 | `[TTFT]` |
| `report` | stats / report | 📊 `:bar_chart:` | 🍅 | `[REPORT]` |

`spawn` and `release` split the legacy 🚀, and `trigger` and `latency` split
the legacy ⚡, because the two uses mean different things. The key set is
closed for Phase 4: a call site whose meaning is not here adds a key to this
table (and both theme maps) first, then uses it.

### Tool

Glyph per agent tool in the live-stream stop summary
(`agentstream.go` `toolEmoji`). The `default` key covers every tool without its
own entry (legacy 🔧 `:wrench:`).

| Tool | Legacy | Garden | Business |
|---|---|---|---|
| Read | 📖 | 📖 | `≡` |
| Grep | 🔍 | 🔍 | `/` |
| Glob | 📁 `:file_folder:` | 🗂️ | `*` |
| Edit | ✏️ | ✂️ (pruning shears) | `✎` |
| Write | 📄 | 🖋️ | `▪` |
| Bash | `:terminal:` | ⚙️ | `$` |
| WebFetch | 🌐 | 🐕 (fetch) | `↓` |
| WebSearch | 🔎 | 🔭 | `?` |
| Agent | 👥 | 👥 | `»` |
| NotebookEdit | 📓 | 📓 | `▦` |
| default | 🔧 | 🔧 | `·` |

The stop summary always prints the tool name beside the glyph
(`⚙️ Bash (44)`), which satisfies the pairing rule; Business marks are
deliberately terse for the same reason.

### Reaction

Emoji **names** for the agent lifecycle reactions on the originating message
or agent card (`bot_reactions_lifecycle.go`) and the refusal reaction
(`bot_mentions.go`). Stock Slack names only.

| State | Meaning | Legacy | Garden | Business |
|---|---|---|---|---|
| `seen` | spawn acknowledged | `eyes` | `seedling` | `white_circle` |
| `working` | in progress | `hourglass_flowing_sand` | `leaves` | `hourglass_flowing_sand` |
| `done` | finished clean | `white_check_mark` | `deciduous_tree` | `white_check_mark` |
| `failed` | failed | `x` | `wilted_flower` | `x` |
| `refused` | request refused | `no_entry_sign` | `fallen_leaf` | `no_entry_sign` |

Garden's reactions echo its lifecycle glyphs (seedling → leaves → tree;
wilted flower for a failure). Business keeps plain marks and leaves the
working / done / failed / refused names as they are today.

### Accessors and fallbacks

`theme.go` exposes `Theme.Semantic(key)`, `Theme.Tool(name)` and
`Theme.Reaction(state)`, backed by the `SemanticGlyphs`, `ToolGlyphs` and
`ReactionNames` maps. Unknown keys never panic or return a blank when a
sensible value exists:

- `Semantic` and `Reaction`: a key the theme lacks falls back to **Garden's**
  value, then `""` for a key neither theme knows.
- `Tool`: a tool without an entry takes the theme's own `default` glyph, then
  Garden's.

Tests assert key parity between the themes, glyph uniqueness per map, no
Garden collision with a Lifecycle or BeadType glyph, bracket-only Business
Semantic values, and the fallback behavior.

## Out of scope (separate plan)

Extending this same visual system (palette, glyph vocabulary, accessibility
rule) to the beads-viewer web UI is tracked as its own plan, since it's a
different surface with a different rendering stack (htmx/Go templates, not
Slack Block Kit).
