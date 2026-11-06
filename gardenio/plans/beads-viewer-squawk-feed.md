# Squawk Feed in beads-viewer

Tracking: ka-0000003088
Slack ask: "let's make a plan to get all `gb squawk` output in one place
with nice links and such in beads-viewer; just every gb squawk." — Matt Baker

## Goal

A first-class page in `beads-viewer` that streams every `gb squawk` across
the fleet — chronological, linkified, and grouped enough to be skimmable.
The operator should be able to read what every agent is saying without
scrolling through Slack.

## What a squawk already is

- `gb squawk <text>` creates a bead and immediately closes it
  (`gardenio/controller/cmd/gb/squawk.go`).
- Shape:
  - `type=message`, `kind=data`, `status=closed`
  - `labels=["squawk", "from:<actor>"]`
  - `fields={"source_agent": <actor>, "text": <full text>}`
  - `title` = `text` (truncated to 500 chars)
  - `created_by` = actor
- Bridge relay: `gardenio/controller/internal/bridge/squawk.go` matches on
  `type=message` AND label `squawk`/`say`, looks up the agent's Slack
  thread, and posts `:speech_balloon: <agent>: <text>`.

The squawk bead itself **does not** persist a Slack permalink — the bridge
posts in the moment and forgets the message TS. (See "Open question 1".)

## What beads-viewer already has

- Linkifier (`linkify.go`) wraps bare `kd-…` IDs in anchor tags, skipping
  text already inside `<a>` so we won't double-wrap.
- Active list / project / view pages all share the partial layout in
  `templates/partials/`.
- Agents page (`/agents`, `/agents/{id}`) already knows agent identity →
  thread, screen, events.
- `beadsapi.ListBeadsQuery` supports `Types`, `Labels`, `Statuses`,
  `Sort`, `Limit`, `Offset` — sufficient for `type=message&labels=squawk`.

There is **no** existing concept of "message bead" anywhere in the viewer
— `grep type=message gardenio/controller/cmd/beads-viewer` is empty.

## Design

### Route

- `GET /squawks` — paginated chronological feed (newest first).
- `GET /squawks?agent=<name>` — same feed filtered to one agent.
- `GET /squawks?project=<name>` — filtered to one project (label
  `project:<name>` on the squawk bead, set by `gb squawk` when present).
- `GET /squawks?since=<duration>` — `30m`, `2h`, `24h` quick filter.
- Add `Squawks` to the top nav next to `Active` / `Decisions` / `Agents`.

### Server

Add `squawks.go` next to `active.go`:

```go
type squawkRow struct {
    ID         string
    Agent      string         // resolved from fields.source_agent or from: label
    AgentURL   string         // /agents/<agent> if agent is known
    Project    string         // first project:* label, if any
    Text       template.HTML  // linkified
    CreatedAt  time.Time
    Ago        string
    SlackURL   string         // optional, see Open question 1
}

func (s *Server) handleSquawks(w http.ResponseWriter, r *http.Request) { … }
```

Implementation notes:
- Query: `ListBeadsFiltered({Types:[]string{"message"}, Labels:[]string{"squawk"}, Statuses:[]string{"closed"}, Sort:"-created_at", Limit:200})`.
- For an `agent=` filter, AND in `from:<agent>` to the labels slice.
- For a `project=` filter, AND in `project:<name>` to the labels slice.
- Use `LinkifyText(bead.fields.text || bead.title, "")` so bare bead IDs
  in squawks become clickable.
- Resolve agent → URL via `/agents/<name>` (already a route).
- Group consecutive squawks from the same agent within 60s into one card
  (visual coalesce — the underlying rows stay distinct for permalinks).

### Template

`templates/squawks.html` extending `layout.html`. Each card renders:

```
[time-ago] [agent chip → /agents/<id>] [project chip → /p/<project>]
<linkified text>
[permalink → /b/<id>] [Slack ↗ if SlackURL present]
```

Reuse `agent_chip` / project chip patterns from `partials/active_row.html`
where possible.

### Pagination & filters

- Cursor pagination using `created_at < ?` (older).
- Top-of-page filter bar: agent text input (autocomplete from active
  agents), project dropdown, since dropdown (`30m / 2h / 24h / 7d`).
- Default limit 100, max 500. Add a `[load older]` link instead of
  infinite scroll — the page is meant for skimming, not endless scroll.

### Live updates (phase 2, optional)

The viewer already consumes the bead SSE stream for the active page.
Add a `beads.bead.closed` handler that, when `type=message && squawk`,
prepends a row to the feed via HTMX `hx-swap-oob`. Out of scope for
phase 1 — manual refresh / since-filter is good enough to start.

## Linkification rules (the "nice links" ask)

In addition to bead IDs (already covered by `linkify.go`):

1. `kd-…` → bead permalink (already done).
2. `MR !<n>` / `!<n>` → linked when `<n>` is numeric, point at the
   project bead's `gitlab_project` field if present, else leave plain.
3. URLs (`https://…`) → standard hyperlink, escaped.
4. `@<agent>` mentions → `/agents/<agent>` if the agent exists in the
   roster, else plain.
5. Slack-style backticks ``` `code` ``` → `<code>` (cheap regex).

Implement as a single `LinkifySquawk(text)` helper in `linkify.go` that
extends `LinkifyText` with these rules. Add a unit test per rule
(`linkify_test.go`).

## Open questions (decision points before we ship)

1. **Persist Slack permalink on the squawk bead?**
   The bridge currently posts and forgets. Persisting `slack_thread_ts`
   + `slack_channel` on the bead at relay time would let the feed link
   back into Slack. Two options:
   - (a) Bridge writes back via `beadsapi.UpdateBead` after `chat.postMessage`.
     Adds one daemon write per squawk. Cheap, useful.
   - (b) Skip — operators who want Slack already have Slack. Page is
     gardenio-only.
   **Recommendation: (a).** Consistency with how decisions/mail track
   their Slack messages, and the "nice links" ask explicitly contemplates
   Slack jump-back.

2. **Project tagging on squawk beads.**
   `gb squawk` does NOT currently put a `project:<name>` label on the
   bead. The agent's `KD_PROJECT` env is well-defined; we should add it
   in `runSquawk` so the project filter actually has data. One-line
   change in `gardenio/controller/cmd/gb/squawk.go`.

3. **Retention.**
   Squawks accumulate fast (every cycle, every gardener, every crew). Do
   we keep them forever? Suggested: leave bead retention alone (kbeads
   has its own GC story), the viewer just renders newest-first with
   pagination. Revisit if `messages` table balloons.

4. **Permissions / OIDC.**
   Same as `/active`: any logged-in operator. No write actions on this
   page so no `operatorOnly` middleware needed.

## Phasing

1. **P1 — feed page (3–4 hr)**
   - `squawks.go` handler, route, template
   - `LinkifySquawk` helper + tests
   - Top-nav link
   - One unit test that the handler queries
     `type=message&labels=squawk&status=closed`.

2. **P2 — project tagging on squawks (15 min)**
   - `gb squawk` adds `project:<KD_PROJECT>` label when set.
   - Doc in `gb squawk --help`.

3. **P3 — Slack permalink writeback (1 hr)**
   - Bridge stores `slack_thread_ts`/`slack_channel`/`slack_permalink` on
     the squawk bead post-send.
   - Feed renders `[Slack ↗]`.

4. **P4 — Live SSE prepend (1–2 hr, optional)**
   - HTMX OOB swap on new squawks.

Total: ~6 hr engineer-time across 4 PRs, each independently shippable.

## Files touched (estimate)

- `gardenio/controller/cmd/beads-viewer/squawks.go` (new)
- `gardenio/controller/cmd/beads-viewer/squawks_test.go` (new)
- `gardenio/controller/cmd/beads-viewer/templates/squawks.html` (new)
- `gardenio/controller/cmd/beads-viewer/templates/layout.html` (nav link)
- `gardenio/controller/cmd/beads-viewer/server.go` (route registration)
- `gardenio/controller/cmd/beads-viewer/linkify.go` (`LinkifySquawk`)
- `gardenio/controller/cmd/beads-viewer/linkify_test.go`
- `gardenio/controller/cmd/gb/squawk.go` (P2: project label)
- `gardenio/controller/internal/bridge/squawk.go` (P3: writeback)

No daemon / kbeads schema changes required.
