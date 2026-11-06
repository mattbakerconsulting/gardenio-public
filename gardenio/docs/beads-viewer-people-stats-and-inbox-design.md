# beads-viewer: per-user stats set + agent-message inbox — research & design

> Gate task: **ka-0000003767** · Epic: **ka-0000005590** · Operator: Matt Baker
>
> This document is the design-of-record for two beads-viewer "people surface"
> enhancements. **Part A** enriches the `/people` directory list view with a
> per-row + header stat set. **Part B** adds a per-user agent-message **inbox**
> to the `/u/{slug}` homepage. Implementation is tracked by the beads that
> depend on this one (see §6).

## Table of contents

- [1. Context & current state](#1-context--current-state)
- [2. Part A — richer per-user stats on /people](#2-part-a--richer-per-user-stats-on-people)
- [3. Part B — agent-message inbox on /u/{slug}](#3-part-b--agent-message-inbox-on-uslug)
- [4. Daemon query surface (shared)](#4-daemon-query-surface-shared)
- [5. Privacy, performance & failure modes](#5-privacy-performance--failure-modes)
- [6. Implementation beads](#6-implementation-beads)

---

## 1. Context & current state

### 1.1 Source map

| Surface | File | What it does today |
|---|---|---|
| `/people` list | `gardenio/controller/cmd/beads-viewer/people.go` | Renders the operator directory. Each `peopleRow` shows: OpenBeads count, last-active "ago", and ≤5 recently-created chips. Header (`peopleHeader`) shows People / Identities / OpenBeads / ActiveNow. |
| `/people` stats source | `kbeads/internal/server/http_operators.go` `handleOperatorStats` → `store.OperatorDirectoryStats` (`kbeads/internal/store/postgres/operators_stats.go`) | Two SQL queries: (1) full-table `GROUP BY operated_by_id` → open count + `MAX(updated_at)` last-active; (2) windowed `ROW_NUMBER()` recent-created chips. |
| `/u/{slug}` homepage | `gardenio/controller/cmd/beads-viewer/user_home.go` | 8+ sections (pinned, active agents, open decisions, recent comments, open MRs, recent commits, recent merged, their automations, their gardeners, forwarded mentions). Eager sections fan out in parallel; heavy ones lazy-load via HTMX. |
| Chat rendering | `bead_chat.go` + `maps_chat.go` | `mapChatView` / `chatMessage` view-models + `projectChatMessage` projector. Renders `type=message` beads as chat lines (author, kind badge, linkified body, relative + absolute timestamps). Capped at `mapChatMaxMessages = 200`. |

### 1.2 Bead shapes that matter

- **Mail** (`gb mail send`, `gb mail.go`): `type=mail`, `kind=data`,
  `Assignee = <recipient>`, `Labels = ["from:<sender>", "delivery:interrupt"?]`,
  `Priority 0..3`, body in `Description`. **Recipient is the `Assignee`
  string** (an agent/operator name), NOT an `operated_by_id` FK. Read state =
  `status` (`open` → unread, `closed` → read; `gb mail` closes on read).
- **Squawk / message** (`gb squawk`): `type=message`, `kind=data`,
  `Labels = ["squawk", "from:<actor>"]`, `Fields.source_agent = <actor>`,
  `Fields.text = <body>`. Created then **immediately closed** (fire-and-forget),
  with `triggered-by` lineage onto the agent/trigger/user bead. @-mentions of a
  bead are surfaced via the text-reference extractor (`bead_references`) — a
  squawk that names `ka-X` produces a `mentioned-by` edge on `ka-X`.
- **Operator identity**: an `operators(id)` FK. The viewer resolves
  slug→operator_id (`LookupOperatorBySlug`) and email→operator_id
  (`resolveInboxViewerOperator`). One human folds many identities (oidc, slack,
  gitlab, agent handles) via `operator_aliases` + render-time `dedupOperators`.

### 1.3 The addressing gap (central design problem for Part B)

There is **no `operated_by_id` FK on the recipient of a mail/squawk**. Mail is
addressed to an `Assignee` *string*; a squawk @-mentions a *bead*, not an
operator. So "messages sent to operator X by agents" cannot be a single
`WHERE recipient_operator_id = X` query today. §3.2 defines the resolution
strategy (recipient-handle set per operator) and §4 the daemon query.

---

## 2. Part A — richer per-user stats on /people

### 2.1 Selection principles

The directory is a **check-in surface**, not a dashboard: each row must scan in
under a second, and the whole page is one un-paginated render of ≤500 operators.
Every candidate stat is graded on **(a) signal** ("does this tell me who is
active / productive right now?") and **(b) cost** (cheap = derivable from the
existing two-query `OperatorDirectoryStats`; new-query = needs a new daemon
aggregate). We ship the cheap high-signal set now and gate the new-query set
behind a single additional aggregate query (§4.1) that stays O(operators).

### 2.2 Final per-row stat set

| Stat | Signal | Cost | Decision |
|---|---|---|---|
| **Open / in-progress count** (existing `OpenBeads`) | high — current load | cheap (already computed) | **KEEP** |
| **Last-active "ago"** (existing) | high — recency | cheap | **KEEP** |
| **Recently-created chips** (existing ≤5) | medium — what they're starting | cheap | **KEEP** (collapse to count + hover when row is dense — see §2.4) |
| **Created last 7d** | high — throughput in | new-query (one windowed `COUNT … FILTER`) | **ADD** |
| **Closed last 7d** | high — throughput out | new-query (same query, `status='closed' AND closed_at > now-7d`) | **ADD** |
| **In-progress count** (split out of OpenBeads) | medium — actively-touched vs merely-open | cheap (add a second `FILTER` to query 1) | **ADD** |
| **MRs open / merged** | high for engineers | new-query (`type='merge-request'` FILTERs) | **ADD** |
| **Decisions pending** | high — "is someone waiting on this person?" | new-query (`type='decision' AND status='open'` FILTER, keyed on operated_by_id) | **ADD** |
| **Agents + gardeners owned (active)** | medium — fleet footprint | new-query (`type IN ('agent','trigger')` FILTER, `role:gardener` for gardeners) | **ADD** (single combined "🤖 N" chip; gardeners broken out only in header) |
| **Messages received + unread badge** | high — ties Part A to Part B | new-query (§4.2 per-operator message counts) | **ADD** (badge only; full list lives on `/u/{slug}`) |
| **Activity sparkline** (created/closed events per day, last 14d) | medium — trend at a glance | new-query (one `date_trunc('day') … GROUP BY` bucketed count) **OR** derive cheaply from a 14×count array | **ADD as v2** — ship a 14-int array endpoint; render as inline `<svg>` polyline. Defer if it doesn't fit the §4.1 single-query budget. |
| Decisions resolved (lifetime) | low — vanity, unbounded | new-query | **DROP** — not a recency signal; resolved decisions live on the user page. |

**Rendering note:** the row already carries Display / identity chips / email /
status-message / recent chips. To avoid overflowing the row we render the new
numeric stats as a compact **stat-chip strip** (`◷ 12 open · ↑7 created · ↓5
closed · ⇄ 3 MR · ⚑ 2 dec · 🤖 4 · ✉ 9 (2)`) with tooltips, right-aligned, one
line, em-dash for zero. The sparkline (when shipped) sits at the far right.

### 2.3 Final header (aggregate banner) stat set

`peopleHeader` currently has People / Identities / OpenBeads / ActiveNow. Add,
summed over the deduped row set (so no extra query — fold the per-row stats):

- **Created last 7d** (fleet throughput in)
- **Closed last 7d** (fleet throughput out)
- **Open MRs** (review backlog across humans)
- **Pending decisions** (operator-action backlog — the most actionable fleet number)
- **Total unread messages** (sum of per-row unread)
- **Gardeners active** (broken out here even though the row folds it into 🤖)

Header counts are **render-time sums of the per-row stats** — never a separate
fleet-wide query — so the header can never disagree with the rows beneath it.

### 2.4 Sort & affordances

- Default sort stays **activity-recency DESC** (keep current behavior). Add
  client-side column sort (created7d, closed7d, open, unread) as a progressive
  enhancement — the data is already in the DOM, so it's a JS-only add, no
  re-fetch.
- Dense-row guard: when the new stat strip + recent chips would wrap, collapse
  recent chips to "📝 N" with the existing hover-card surfacing the list.

### 2.5 View-model changes (people.go)

Extend `peopleStats` (per-operator aggregate) and `peopleRow`:

```go
type peopleStats struct {
    OpenBeads      int
    InProgress     int          // NEW
    Created7d      int          // NEW
    Closed7d       int          // NEW
    MRsOpen        int          // NEW
    MRsMerged7d    int          // NEW (merged in window; lifetime is unbounded)
    DecisionsPending int        // NEW
    AgentsActive   int          // NEW
    GardenersActive int          // NEW
    MessagesRecv   int          // NEW
    MessagesUnread int          // NEW
    Spark          []int        // NEW v2 — 14-day created/closed bucket counts
    LastActive     time.Time
    Recent         []peopleRecentBead
}
```

`peopleRow` gains the same fields (or embeds `peopleStats`), aggregated across
the operator group's members exactly like `OpenBeads` is today (sum counts; for
unread, sum; for sparkline, element-wise sum). `peopleHeader` gains the §2.3
sums.

---

## 3. Part B — agent-message inbox on /u/{slug}

### 3.1 Definition of "messages sent to them by agents"

> **Superseded by §3.8 (bidirectional).** §3.1–§3.3 define the original
> agent → user half. The shipped feed also captures user → agent messages; read
> §3.8 for the merged two-sided model.

The inbox is the union of two bead populations, both authored by agents and
*directed at this operator*:

1. **Mail** — `type=mail` beads whose **`Assignee`** resolves to one of the
   operator's recipient handles (§3.2). `status=open` ⇒ unread; `status=closed`
   ⇒ read. Carries `from:<sender>` label, subject (`Title`), body
   (`Description`), priority.
2. **@-mention squawks** — `type=message` beads (squawks / Slack drops /
   delivery reports) whose **prose @-mentions this operator** — i.e. the
   message text references a bead the operator owns, OR references the
   operator's user bead / agent beads directly. v1 scopes to the
   strongest, cheapest signal: squawks that mention the operator's **user
   bead** or are `triggered-by`-linked to an agent the operator owns. (A
   fuller "@-name in free text" match is deferred to v2 — see §3.7.)

### 3.2 Recipient-handle resolution (the addressing bridge)

Because mail is addressed to an `Assignee` string, the viewer must build the
operator's **recipient-handle set**: every name an agent might `gb mail send`
to. Sources, in resolution order:

- The operator's identity subjects/handles from the directory feed
  (`OperatorDirectoryEntry`: oidc subject/email, slack U-id, gitlab handle,
  agent names folded into the human via aliases).
- The operator's slug and `op-<id>` fallback.
- The operator's display name.

This set is computed once per page render (reusing the `dedupOperators` group
the directory already builds) and passed to the daemon query (§4.2) as an
`assignee IN (...)` list. **Alias-awareness matters**: a mail sent to
`matthew.baker` and one sent to a slack U-id both belong in the same human's
inbox.

### 3.3 Per-operator daemon query

See §4.2 for the endpoint. Conceptually two bounded queries (mirroring
`OperatorDirectoryStats`'s two-query shape):

- **Mail**: `type=mail AND assignee = ANY($handles) ORDER BY created_at DESC
  LIMIT N` — returns mail beads with status (read/unread), sender label,
  subject, body, priority, created_at.
- **Mention squawks**: `type=message` beads `mentioned-by` the operator's user
  bead + owned agent beads (reverse-reference lookup, the same
  `GetMentionedBy` plumbing the constellation uses), `ORDER BY created_at DESC
  LIMIT N`.

Both capped (`LIMIT N`, default 100, hard ceiling 500 mirroring existing
patterns). The two streams are merged + sorted newest-first viewer-side.

### 3.4 Chat-style render (reuse bead_chat / maps_chat)

The inbox renders with the **existing `chatMessage` / `mapChatView` shapes** and
`projectChatMessage` projector — verbatim reuse, no new chat CSS:

- `messageBodyText` already handles `Fields.text` → `Description` → `Title`.
- `messageAuthor` already handles `source_agent` → `created_by` →
  `operated_by`. Mail's `from:<sender>` label maps to the same Author slot (add
  a tiny `from:`-label reader to `messageAuthor`, or pre-stamp Author when
  projecting mail).
- `messageKind` gains a `"mail"` badge case (label/`type=mail`) alongside
  `squawk` / `slack` / `message`.
- Ordering oldest→newest within the panel (chat-log order) is already the
  `buildBeadChat` contract; we feed it the merged id set.

New file `gardenio/controller/cmd/beads-viewer/user_inbox.go` builds a
`userInboxView` wrapping `mapChatView` plus the affordance state (§3.5). It is a
**lazy HTMX section** on `/u/{slug}` (like forwarded-mentions) so it never slows
first paint, plus a dedicated full-page route `/u/{slug}/inbox` for the
expanded view.

### 3.5 Affordances (v1 vs v2)

| Affordance | Mechanism | Phase |
|---|---|---|
| **Read / unread** | Mail: `status` (open=unread). Squawks are always-closed, so model read-state via a per-operator **last-visit timestamp** stored on the user bead `Fields.inbox_last_visit_at` — anything created after it is "unread". | v1 |
| **Unread-since-last-visit badge** | Count of inbox items newer than `inbox_last_visit_at`; same number feeds the `/people` row badge (§2.2). Bump the timestamp on inbox view. | v1 |
| **Jump-to-source** | Each `chatMessage.Href` already → `/{beadID}`. For mention squawks add a secondary "in: ka-X" chip linking the mentioned bead. | v1 |
| **Sender filter** | Distinct `from:<sender>` values → filter pills; client-side filter over rendered rows (no re-fetch). | v1 |
| **Mark read / reply inline** | Reply = `gb mail send` back to sender (POST to a viewer handler that calls `daemon.CreateBead` type=mail). Mark-read = close the mail bead (`CloseBead`). Self-only (viewer must be the page owner — reuse `isViewerSelf`). | v2 |
| **Group by gardener / thread** | Group rows by `triggered-by` lineage root (gardener/agent) — the squawk lineage already links to the gardener. | v2 |
| **Search** | Client-side substring over body/author for v1; daemon `Search` param for v2 when volume warrants. | v1 client / v2 server |

### 3.6 Self-vs-other visibility

The full inbox (bodies, reply, mark-read) renders **only for the page owner**
(`isViewerSelf`). For a cross-user view, show counts + subjects but not bodies
(privacy — an operator's mail is not public). This mirrors the existing
self-only inline editors on the homepage.

### 3.7 Brainstorm — additional inbox ideas (candidate backlog)

- **Priority lane**: pin `priority ≤ 1` (critical/high) mail to the top with a
  red rail, matching the `delivery:interrupt` nudge semantics.
- **"Needs reply" smart-filter**: mail still `open` + older than 24h.
- **Digest collapse**: fold N squawks from the same gardener in the same hour
  into one expandable group (gardeners can be chatty).
- **Cross-link to decisions**: a decision addressed to the operator is already
  on the homepage; surface an inbox affordance "you have 2 pending decisions"
  linking the existing section rather than duplicating.
- **Keyboard nav** (`j`/`k`/`o`) for power users, matching the inbox density mode.
- **Unread count in the nav/tab title** so it's visible from any page.
- **Notification opt-in**: tie `inbox_last_visit_at` to an optional email/slack
  ping when unread crosses a threshold (far-future, controller-side).

### 3.8 Bidirectional feed (scope expansion — Matt Baker, 2026-06-10)

The original §3.1 inbox was one-directional (agent → user). The shipped feed is
**bidirectional**: `/u/{slug}` is a two-sided conversation log that also captures
messages the operator **sent**. This makes the page a true dialogue rather than a
half-conversation. Implemented in
`gardenio/controller/cmd/beads-viewer/user_inbox.go`.

**Four populations, one merged log.** The feed unions the §3.1 incoming pair with
two outgoing populations, all projected through the same `chatMessage` /
`mapChatView` shapes (§3.4):

| Population | Direction | Bead shape | Keyed on |
|---|---|---|---|
| Mail | agent → user (other-side) | `type=mail`, `from:<sender>` label | `assignee = ANY(handles)` (§3.2) |
| Mention squawks | agent → user (other-side) | `type=message` mentioning the user bead | reverse-reference (`GetMentionedBy`) |
| Slack thread replies | user → agent (own-side) | `type=task` label `slack-thread-reply` | `operated_by_id = viewer` |
| Bot @-mentions | user → agent (own-side) | `type=task` label `slack-mention` | `operated_by_id = viewer` |

**No new daemon primitive.** The outgoing side reuses the existing
`ListBeadsQuery.OperatedByID` FK filter (`client_query.go`) — the bridge stamps
`fields.operated_by` on both `slack-thread-reply` (`bot_thread_forward.go`) and
`slack-mention` (`bot_mentions.go`) beads, and the daemon resolves that to the
`operators(id)` FK. The incoming mail side keeps the merged `Assignees` filter
(ka-0000007555). Because the daemon `Labels` filter is AND-combined, the two
outgoing labels are issued as two bounded queries rather than one OR query.

**Direction styling.** `chatMessage.Direction` is stamped per population:
`from-user` lines render as own-side bubbles (right-aligned, accent background);
`from-agent` lines render as other-side bubbles (left-aligned). The maps /
bead-show chat panels leave `Direction` empty and are unchanged. `messageAuthor`
already resolves `operated_by`, so user-authored lines attribute to the operator
correctly; mail lines read their author from the `from:<sender>` label.

**Affordances (v1).** Read/unread (open mail OR created after
`fields.inbox_last_visit_at`), an unread badge, a client-side sender filter
(pills over the distinct `from:` / author set), client-side substring search,
and jump-to-source (`chatMessage.Href` → `/{beadID}`). Viewing your own inbox
bumps `inbox_last_visit_at` so only newer arrivals stay unread. Outgoing lines
are never marked unread (they are the operator's own messages).

**Privacy (§3.6, extended).** A cross-user viewer still sees the merged feed —
senders, timestamps, counts — but mail bodies and the operator's own outgoing
reply bodies are redacted to subject-only. Agent squawk bodies (already public on
their bead page) survive. The full self-only affordance set (reply, mark-read)
remains v2 per §3.5.

**Caps.** Each of the four populations is fetched at `userInboxFetchLimit`; the
merged log is then capped at `mapChatMaxMessages` (200) newest-first, with the
dropped older count surfaced via `mapChatView.OlderHidden` (§5 cap honesty).

---

## 4. Daemon query surface (shared)

### 4.1 Part A — extend OperatorDirectoryStats (one added query)

Add the new per-row counts to `store.OperatorStat` and compute them in
**one additional `GROUP BY operated_by_id` query** alongside the existing two,
so the directory stays at **3 queries total regardless of operator count**:

```sql
SELECT operated_by_id,
  COUNT(*) FILTER (WHERE status IN ('open','in_progress'))                       AS open_beads,
  COUNT(*) FILTER (WHERE status = 'in_progress')                                  AS in_progress,
  COUNT(*) FILTER (WHERE created_at > now() - interval '7 days')                  AS created_7d,
  COUNT(*) FILTER (WHERE status='closed' AND closed_at > now() - interval '7 days') AS closed_7d,
  COUNT(*) FILTER (WHERE type='merge-request' AND status IN ('open','in_progress')) AS mrs_open,
  COUNT(*) FILTER (WHERE type='merge-request' AND status='closed'
                   AND closed_at > now() - interval '7 days')                     AS mrs_merged_7d,
  COUNT(*) FILTER (WHERE type='decision' AND status='open')                       AS decisions_pending,
  COUNT(*) FILTER (WHERE type='agent'   AND status IN ('open','in_progress'))     AS agents_active,
  COUNT(*) FILTER (WHERE type='trigger' AND status IN ('open','in_progress'))     AS gardeners_active,
  MAX(updated_at)                                                                 AS last_active
FROM beads
WHERE operated_by_id IS NOT NULL AND deleted_at IS NULL
GROUP BY operated_by_id;
```

This **replaces** the existing query (1) — it's a superset. `gardeners_active`
counts `type=trigger`; the row UI can fold agents+gardeners into one 🤖 chip and
the header breaks gardeners out. The sparkline (§2.2 v2) is a separate optional
`date_trunc('day')` bucket query — ship only if it stays O(operators·14).

JSON: extend `OperatorStatEntry` (`http_operators.go`) and the
`OperatorStats` client (`client_operators.go`) with the new int fields. All
additive — existing fields untouched.

### 4.2 Part B — per-operator message counts + lists

Two needs: (a) **counts** for the `/people` unread badge (cheap, all
operators); (b) **the message list** for one operator's `/u/{slug}` inbox.

- **(a) Counts** — fold into §4.1's query is *not* possible (mail recipient is
  `assignee` string, not `operated_by_id`). Instead add a small dedicated
  aggregate: `SELECT assignee, COUNT(*), COUNT(*) FILTER (WHERE status='open')
  FROM beads WHERE type='mail' GROUP BY assignee`. The viewer maps each
  operator's recipient-handle set (§3.2) onto these buckets. Bounded by the
  mail table; returned as a `map[assignee]→{total,unread}`.
- **(b) List** — a new endpoint `GET /v1/operators/{id}/messages?limit=N` (or
  reuse `ListBeadsFiltered` with `Types:["mail"]` + a new `Assignees []string`
  query param, plus the existing `GetMentionedBy` for squawks). Prefer the
  latter (no new endpoint) — add an **`Assignees []string`** filter to
  `ListBeadsQuery` / daemon `field_filter` so the viewer can issue
  `type=mail AND assignee = ANY($handles)`.

**Recommendation:** add `Assignees []string` to `ListBeadsQuery` (daemon-side
`assignee = ANY(...)`). It's the minimal, reusable primitive that serves both
the count fold and the per-user list, and it avoids a bespoke endpoint.

---

## 5. Privacy, performance & failure modes

- **Privacy**: mail bodies render only for the page owner (§3.6); the directory
  badge is a count only. Metric/aggregate rows never carry message bodies.
- **Performance**: `/people` stays at **3 bounded queries** (§4.1) + 1 mail-count
  aggregate (§4.2a) regardless of operator count — no per-row fan-out. `/u/{slug}`
  inbox is a **lazy HTMX section**, so it never blocks first paint; its two
  queries are `LIMIT`-capped.
- **Graceful degradation**: every new stat/section follows the existing
  "return empty on error" rule (`lookupOperatorStats`, `runUserHomeSection`) —
  a daemon hiccup drops the stat strip / inbox to em-dash / empty-state, never
  500s the page.
- **Alias correctness**: counts key on canonical `operated_by_id`; the mail
  recipient set folds every alias handle so a renamed/multi-identity human sees
  one unified inbox.
- **Cap honesty**: inbox `LIMIT` and the squawk `mapChatMaxMessages=200` cap are
  surfaced as "N older not shown", reusing `mapChatView.OlderHidden`.

---

## 6. Implementation beads

This gate unblocks the two umbrella impl beads already filed by the scoping
agent, plus the granular daemon-primitive beads filed from this design. All
depend (`blocks`) on this research bead and are parented to epic ka-0000005590.

- **ka-0000006741** — impl: richer per-user stats on /people (view-model +
  template + header). Consumes §4.1.
- **ka-0000005615** — impl: agent-message inbox + affordances on /u/{slug}.
  Consumes §3 + §4.2.
- **(new) daemon: extend OperatorDirectoryStats with the §4.1 counts** — the
  shared primitive ka-0000006741 depends on.
- **(new) daemon: add `Assignees []string` filter + mail-count aggregate
  (§4.2)** — the shared primitive ka-0000005615 depends on.

See the bead graph under ka-0000005590 for live status.
