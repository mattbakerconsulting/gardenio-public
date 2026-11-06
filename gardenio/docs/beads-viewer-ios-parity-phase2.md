# beads-viewer iOS-parity Phase 2 — design doc

**Status:** research / design — no code changes in this MR.
**Bead:** [`ka-0000006123`](https://github.com/mattbakerconsulting/gardenio) (parent epic `ka-0000002538`).
**Scope:** the Go htmx server-side viewer at `gardenio/controller/cmd/beads-viewer/` (templates `templates/*.html`, server `server.go` / `main.go` / `search.go`). **Not** beads3d.

This doc exists so the follow-on decision bead (menu/IA fork) can be answered unambiguously. It maps the iOS Gardenio app's data views to the current beads-viewer surface area, proposes three menu/IA forks with concrete handler+template+route impact, sketches per-type renderers for `decision`/`schedule`/`mr`/`advice`, lays out a commenting write-path UX, and ends with a draftable `gb decision create` invocation.

---

## 0. TL;DR

- The current beads-viewer is a thin generic browser: index → project list → bead show, plus a search page and two htmx fragment routes (`/b/{id}/comments`, `/b/{id}/deps`). Every bead, regardless of `Type`, renders through the same `bead_show.html` field-list dump.
- The iOS app, by contrast, is a *typed* browser. It segregates bead families into purpose-built tabs (Beads, Active, Decisions, Agents, Advice) and renders each type with its own card layout. Decisions inline option buttons. Schedules surface cron + enabled. Advice has a scope sidebar (Global/Projects/Roles/Agents).
- Three forks for the IA refactor are realistic:
  - **Fork A — tab-strip in `layout.html`** (Beads / Active / Decisions / Agents / Advice). Mirrors iOS most directly. Each tab has its own list handler + template; `bead_show.html` becomes a dispatcher to per-type detail templates.
  - **Fork B — single list + per-type lens.** Most incremental. `bead_show.html` switches on `bead.Type` to a per-type partial; list views stay as today (project page + search). No new top-level routes.
  - **Fork C — split list+detail (htmx-driven shared selection).** Two-pane layout: filtered list on left, detail rendered into the right pane via `hx-target`. Deep-linking via query params (`?id=…&filter=…`). Browser back/forward needs `hx-push-url`.
- Comment write-path: a small `<form hx-post="/b/{id}/comments" hx-swap="outerHTML" hx-target="#comments-section">` lives at the bottom of the existing comments partial; the POST handler runs `daemon.AddComment(ctx, id, EmailFromContext(ctx), text)` (already present in `internal/beadsapi`) and re-renders the partial. No new daemon RPC needed.
- Decision-bead options are sharpened in §6 with `artifact_type` per option.

---

## 1. iOS inventory

Reverse-engineered from `usr/Gardenio/Gardenio/`:

- `BeadsTab.swift` (757 lines) — main bead browser + `BeadDetailView`.
- `InProgressTab.swift` (366 lines) — "Active" overview screen.
- `DecisionsTab.swift` (206 lines) — standalone pending-decisions list.
- `AdviceTab.swift` (700+ lines) + `AdviceDetailView.swift` — hierarchical advice browser.
- `AgentsTab.swift` (~1000 lines) — running-agents fleet view.

Top-level navigation is a SwiftUI `TabView` (defined in `GardenioApp.swift`); `AppTab` enum values include `.beads`, `.active`, `.agents`, `.decisions`, `.advice`, `.costs`. Each tab is its own `NavigationStack` (advice uses `NavigationSplitView`).

### 1.1 `BeadsTab` — generic bead browser

```
┌─────────────────────────────────────────────────┐
│ Beads                                  ⟲  [42]  │  ← nav title + refresh + count
├─────────────────────────────────────────────────┤
│ [Issues] [All] [Tasks] [Features] [Bugs]   …    │  ← horizontal type-filter chips
│                                                 │
│ Project: ▾ [All Projects ▾]      42 beads      │  ← project picker + count
├─────────────────────────────────────────────────┤
│ ●  Title of bead — first line wraps             │  ← StatusDot + title
│    Description preview, two lines max…          │
│    [task] 👤 assignee  📁 project   ka-xxx 5m   │  ← TypeBadge, assignee, project, id, ago
│                                                 │
│ ●  Next bead…                                   │
│    [bug] [P0] 👤 agent-name 📁 gardenio ka-yyy 1h│
└─────────────────────────────────────────────────┘
       ◀ swipe leading: Spawn   trailing: Close ▶
```

**Data shown per row:** `status` (dot color), `title`, `description` (truncated), `type` badge, `priority` badge if ≤1, `assignee` (with `cpu` icon if assignee matches an active agent bead), `project` derived from `project:<name>` label, `id`, `updatedAt` "ago".

**Interactions:** type-filter chip switches the in-memory predicate; project picker filters; `searchable` text field does substring match on title/id/assignee/type/description; tap → push `BeadDetailView`; swipe-left → close (destructive); swipe-right → spawn-agent sheet; pull-to-refresh.

**Type taxonomy on iOS:** `BeadsTab` recognizes `task / feature / bug / epic / chore` as the "issues" macro-filter. Other types (`schedule`, `decision`, `agent`, `advice`, `formula`, `mr`, `jack`) are *not* surfaced through filter chips here — the operator switches to the dedicated tab (`.decisions`, `.advice`, `.agents`) for those, with `BeadsTab` as the catch-all when "All" is selected.

**Why it works:** the filter strip is the affordance that says "this view is segregated by type." The user's first interaction is choosing a lens. Project filter lives below the lens because users primarily slice *by project within a type*, not the other way around.

### 1.2 `BeadDetailView` — generic bead detail

```
┌──────────────────────────────────────────────┐
│  ●  IN_PROGRESS                       ka-xxx │  ← status pill + id
│                                              │
│  Title of the bead, large bold                │
│                                              │
│  [task] [P1] [issue]                          │  ← type / priority / kind chips
└──────────────────────────────────────────────┘

  ┌─[Assigned Agent → console]─────────────────┐
  │ 🖥  Assigned Agent                          │  ← only when assignee is a live agent bead
  │     agent-name-1abc                       › │     (NavigationLink to AgentDetailView)
  └────────────────────────────────────────────┘

  People       ← assignee / owner / created-by
  Description  ← plain text (no markdown rendering yet on iOS)
  Notes
  Labels       ← FlowLayout chips
  Fields       ← BeadFieldsSection (curated subset, friendly labels)
  Timestamps   ← created / updated / closed / due, "ago" formatted
  ───
  [Spawn Agent]      ← only when status != closed
  [Close Bead]
```

**Critical detail:** `BeadFieldsSection` (`BeadsTab.swift:639`) does NOT dump every field. It walks a hard-coded ordered list and only renders the keys it recognizes:

```
project, role, cron, enabled (bool→Yes/No), timezone,
agent_state, provider, status, model, fallback,
throttle_status, overage_status, consecutive_failures,
last_run (formatted "ago"), prompt (clipped to 200 chars).
```

Two consequences:
1. **The display list is curated.** Unknown keys are silently hidden. This is the iOS app's main editorial decision — it implicitly chooses which fields are user-relevant and how they should be labeled ("Agent State" not `agent_state`).
2. **Type-specific rendering is *de facto* per-key, not per-type.** A `schedule` bead shows `cron / enabled / timezone / last_run` because those keys exist; an `agent` bead shows `agent_state / provider / model / fallback / throttle_status / consecutive_failures`. The framework is "pull a small whitelist of fields and label them nicely," and the result is that each type gets a recognizably different detail card without any explicit type dispatch.

**Why this works:** the curated whitelist gives the user a calm, scannable detail page where "the right things show up" without burying them under `Fields:` JSON dumps. It is far more readable than a generic `range $k, $v := .Bead.Fields` template.

**Why it has limits:** because dispatch is implicit, complex types (`decision`, `mr`) cannot show non-trivial widgets (option buttons, pipeline state badges). Decisions in particular get pushed to a separate tab (§1.4) where they *can* render specialized UI.

### 1.3 `InProgressTab` — "Active" dashboard

```
┌──────────────────────────────────────────────────┐
│ Active                                       ✅  │  ← title + connection badge
├──────────────────────────────────────────────────┤
│ ┌──────────────────────────────────────────────┐ │
│ │  👥 3      ▶ 12       ❓ 2        📥 7      │ │  ← SummaryBar: counts, each tappable
│ │  Agents    Active     Decisions   Queued    │ │     and routes to a different tab
│ └──────────────────────────────────────────────┘ │
│                                                  │
│ ⚠ 2 pending decisions  Switch to Agents tab  ›   │  ← DecisionBanner (only if decisions>0)
│                                                  │
│  gardenio (5)                                     │  ← Section per project
│   ● Title…             agent-name [edit] [bash] 30s
│      Description preview                         │
│      [task] ka-xxx 5m                            │
│   ● Next bead…                                   │
│                                                  │
│  gardenio-next (3)                                │
│   …                                              │
└──────────────────────────────────────────────────┘
```

**Data shown:** four headline counts (`active agents` / `inProgressBeads.count` / `decisions.count` / `unclaimedTasks.count`), per-project sections of in-progress beads, each card shows agent activity (current tool, last event chip, idle duration with color: green<5min, orange>5min, red if reaped), bead title/desc, type/priority chips, id, "ago".

**Why it works:** This is the operator's home screen — at a glance, "what is happening RIGHT NOW, and which projects are alive." The summary bar doubles as cross-tab navigation. The agent activity row tells you whether each agent is *making progress* (running a tool) or *stuck* (idle for many minutes).

**htmx implication:** server-side beads-viewer cannot easily replicate live tool/event chips without an SSE wire-up. The InProgress dashboard's live cards are likely a *Phase 3* feature; Phase 2 should focus on the static parts (counts, in-progress list grouped by project, decision count).

### 1.4 `DecisionsTab` + `DecisionsTabCard` — bespoke renderer

This is the clearest example of why a generic `bead_show.html` doesn't cut it. Decision beads carry a `DecisionEntry` with `prompt`, `requestedBy`, `options[]` (each `{id, short, label, artifact_type}`). The iOS card renders:

```
┌────────────────────────────────────────────────┐
│ ●  ka-xxx [decision]                    👤 by  │  ← header
│                                                │
│  Bead title                                    │  ← title
│                                                │
│  Decision prompt body, multi-line, may include │
│  why-context paragraphs, recommendation, etc.  │
│                                                │
│  ┌──────────────────────────────────────────┐  │
│  │ Reasoning (optional)                     │  │  ← TextField (sticky)
│  └──────────────────────────────────────────┘  │
│                                                │
│  ━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━ │
│                                                │
│  ┌──────────[Approve]──────────┐               │  ← per-option full-width buttons
│  │                              │               │     ≥44pt touch target;
│  │  (subhead under big label    │               │     option.id 'approve' tints green,
│  │   if option.short ≠ label)   │               │     'reject/deny' red, 'skip/defer' orange
│  └──────────────────────────────┘               │
│  ┌──────────[Option B]──────────┐               │
│  │ short hint                   │               │
│  └──────────────────────────────┘               │
│  ┌──────────[Dismiss]───────────┐               │  ← always present, secondary
│  └──────────────────────────────┘               │
│                                                │
│  …context: bead description preview (3 lines)  │
└────────────────────────────────────────────────┘
```

**Interactions:** picking a button → `store.resolveDecision(id, optionId, reasoning)`. `Dismiss` → `store.dismissDecision(id, reason)`. While resolving, the chosen button shows a `ProgressView` and all buttons are disabled (single-flight resolution).

**Why it works:** putting the resolution affordance in the same card as the prompt collapses the read-then-act loop. No drill-down. No "open in another app." The reasoning field is right there because *most decisions deserve a why*. The dismiss button is always present because *some decisions deserve to disappear*.

**Beads-viewer parity note:** beads-viewer today does not surface options *at all*; it just dumps `Fields: prompt: …, options: [...]` as a blob. There's no resolve POST endpoint either. Comment write-path (§5) would need to land first to validate the POST + middleware + audit story; decision-resolution can follow the same shape.

### 1.5 `AdviceTab` + `AdviceDetailView` — split scope sidebar

```
┌────────────┐  ┌──────────────────────────────────────────┐
│ Advice     │  │ Advice                          ✏ + ⟲    │
├────────────┤  │   Filter: scope=Global  q=…              │
│ ▢ All      │  ├──────────────────────────────────────────┤
│ 🌐 Global  │  │ • Title of advice                         │
│ ⚙  Advanced│  │   first paragraph preview…                │
│            │  │   [P1] [global] ka-aaa     5m            │
│ Projects   │  │ • …                                        │
│  📁 gardenio│  │                                            │
│  📁 next   │  │     [in detail column on iPad/Mac]:       │
│            │  │  ┌────────────────────────────────────┐   │
│ Roles      │  │  │ ★ P1  🌐 Global             ka-aaa │   │
│  🛡 gardener│  │  │ Title                              │   │
│            │  │  │ ── ADVICE ──                       │   │
│ Agents     │  │  │ Markdown body…                     │   │
│  🖥 agent-1│  │  │ ── HOOK ──   (if any)              │   │
└────────────┘  │  │ Trigger / Command / Timeout / On-fail│  │
                │  │ ── TARGETING ── scope description    │  │
                │  │ ── SUBSCRIPTION FILTERS ──           │  │
                │  │ created / updated / by               │  │
                │  └────────────────────────────────────┘   │
                └──────────────────────────────────────────┘
```

**Data shown:** scope sidebar enumerates discovered scopes (rigs/projects, roles, agents) by inspecting label sets; the detail card slots data from `bead.Fields` into typed cards (`Hook`, `Targeting`, `Subscription filters`) only when the relevant subfields are non-empty.

**Why it works:** advice has high cardinality — without a sidebar, the list is unreadable. Scope is the natural axis (the operator usually wants "show me advice that targets *this project*", not "all advice ever"). The detail view's *conditional* sub-cards keep simple advice short and complex advice readable.

### 1.6 `AgentsTab` — fleet view

```
┌─────────────────────────────────────────────────┐
│ Agents                                  ✅  ⟲   │
├─────────────────────────────────────────────────┤
│ [All 27]  [gardenio 18]  [gardenio-next 9]   …    │  ← project chips
├─────────────────────────────────────────────────┤
│ gardenio (18)                                     │  ← Section per project
│   ● agent-foo-1abc       running                 │
│     [task] tool: bash   12s idle                 │
│   …                                              │
│                                                  │
│ Decisions — gardenio (2)                          │  ← inline DecisionRow
│   ● ka-xxx Title…       [Approve][Reject][…]    │
│                                                  │
│ Unclaimed Tasks (5)                              │  ← bare list
│   [P1] Title…                            ka-yyy  │
│                                                  │
│ Recent (12)                                      │  ← terminated agents
│   ● agent-old-1xyz   reaped 5m ago               │  ← swipe-left → Respawn
└─────────────────────────────────────────────────┘
```

**Why it works:** "what's running, what wants my attention, what's queued, what just finished" all on one screen. Decision rows in the agents tab are the *compact* version of `DecisionsTab`'s full card — same actions, less chrome. This is the "I'm in a meeting and need to triage in 30 seconds" view.

### 1.7 Per-type taxonomy on iOS — summary table

| Type        | Where it surfaces (primary)        | Specialized rendering?                                                                                  |
|-------------|------------------------------------|---------------------------------------------------------------------------------------------------------|
| `task` / `feature` / `bug` / `epic` / `chore` | `BeadsTab` (Issues / All)                       | No — generic `BeadDetailView`, but the curated `BeadFieldsSection` whitelist surfaces relevant fields.                                       |
| `decision`  | `DecisionsTab` + `AgentsTab` decision rows | YES — full inline option-resolution card; reasoning input; tinted buttons by `option.id`.                |
| `schedule`  | `BeadsTab` (filter) → `BeadDetailView`     | Implicit — `cron / enabled / timezone / last_run` keys surface via the curated whitelist.               |
| `formula`   | `BeadsTab` (under "All")           | None — falls through to generic detail. iOS has no formula-runner UI today.                              |
| `mr`        | `BeadsTab` (under "All")           | None — falls through. (`mr_state` is not in the whitelist, so even basic MR status is invisible on iOS.) |
| `agent`     | `AgentsTab` → `AgentDetailView`             | YES — bespoke agent console with live activity, tool, idle, respawn.                                     |
| `advice`    | `AdviceTab` → `AdviceDetailView`           | YES — Targeting / Hook / Subscription cards conditional on fields; markdown body.                       |
| `jack` / `account` | `BeadsTab` (under "All")    | None — generic detail.                                                                                   |

The takeaway: iOS uses **dedicated tabs** for the three types where bespoke UI carries its weight (`decision`, `agent`, `advice`), and a **curated-whitelist generic detail** for everything else. Beads-viewer does not yet have either.

---

## 2. beads-viewer current-state audit

### 2.1 File map

```
gardenio/controller/cmd/beads-viewer/
├── main.go            (215 lines)  — config, OIDC wiring, mux, server lifecycle
├── server.go          (411 lines)  — Server struct, RegisterRoutes, render(), 5 handlers
├── search.go          (149 lines)  — handleSearch (semantic→lexical fallback)
├── dev_auth.go        (66 lines)   — devBypassMiddleware (BEADS_VIEWER_DEV_AUTH_BYPASS=1)
├── middleware.go      (67 lines)   — ipWhitelistMiddleware (ALLOWED_CIDRS)
├── linkify.go         (47 lines)   — ka-id → href rewriter
├── templates.go       (9 lines)    — //go:embed templates and templates/static
└── templates/
    ├── layout.html              (837 lines) — 6 themes via html[data-skin], <nav>, htmx wired
    ├── index.html               (20 lines)
    ├── project.html             (40 lines)
    ├── bead_show.html           (79 lines)  — generic field-list dump + similar + comments
    ├── search.html              (78 lines)  — form + results table
    ├── partials/comments.html   (17 lines)  — read-only render, auto-poll every 30s if in_progress
    └── partials/deps.html       (17 lines)  — read-only deps list
```

### 2.2 Routes and dispatch (`server.go:136`)

```
GET /{$}                   → handleIndex        → index.html       data: {Projects: []string}
GET /search                → handleSearch       → search.html      data: {Query, *Filter, Results, Mode, …}
GET /p/{project}           → handleProject      → project.html     data: {Project, Beads: []projectBeadView, Truncated}
GET /b/{id}/comments       → handleBeadComments → partials/comments.html (template "comments")
GET /b/{id}/deps           → handleBeadDeps     → partials/deps.html (template "deps")
GET /{id}                  → handleBeadShow     → bead_show.html   data: {Bead, TitleHTML, DescriptionHTML, Dependencies, Comments, Project, Similar}
GET /assets/static/        → static FileServer
GET /healthz, /readyz      → bypass auth
```

The fragment routes use the `/b/{id}/...` prefix because Go 1.22 `net/http.ServeMux` rejects overlapping patterns like `/{id}/comments` vs `/p/{project}` (commented in `server.go:142-144`).

### 2.3 Template dispatch (`server.go:97-106`)

```go
pageNames := []string{"index.html", "bead_show.html", "search.html", "project.html"}
// each page parses: layout.html + <name> + partials/comments.html + partials/deps.html
```

So **every page already has access to `comments` and `deps` partials**. Adding new partials (e.g. per-type detail blocks) means appending to the `partials/` slice and they immediately become callable as `{{template "name" .}}` from any page.

### 2.4 Data flowing into `bead_show.html`

The handler (`server.go:245`) populates a single map for the template:

```text
Bead             *beadsapi.BeadDetail   // ID, Title, Type, Status, Priority, Assignee, Owner,
                                         // CreatedBy, ClosedBy, Kind, Slug, Fleet,
                                         // Labels []string, Description string,
                                         // Notes string, Fields map[string]any,
                                         // CreatedAt/UpdatedAt time.Time,
                                         // ClosedAt/DueAt/DeferUntil string
TitleHTML        template.HTML          // linkified title
DescriptionHTML  template.HTML          // goldmark GFM → linkified
Dependencies     []beadsapi.Dependency  // best-effort; nil on daemon error
Comments         []commentView          // already markdown-rendered + linkified
Project          string                 // first "project:<x>" label, else ""
Similar          []similarBeadView      // best-effort; nil hides the panel
```

`Bead.Fields` is a `map[string]any` of arbitrary scalars / slices / nested maps as returned by the daemon. The template currently just dumps it via `{{range $k, $v := .Bead.Fields}} <pre>{{$v}}</pre>` — no curation, no friendly labels, no per-type behavior.

### 2.5 htmx swap targets in use today

| Element                    | Where           | Trigger              | Target / swap                       |
|----------------------------|-----------------|----------------------|-------------------------------------|
| Bead-id link prefetch      | project.html, search.html, bead_show.html similar | `mouseover once`     | `target=none, swap=none` (warms server cache) |
| Comments auto-poll         | partials/comments.html | `every 30s` (only when status=in_progress) | `outerHTML` of `#comments-section` |
| Deps fragment              | partials/deps.html | (no client trigger today; route exists for future) | `outerHTML` of `#deps-section`      |

So beads-viewer is htmx-aware but has very few real swap interactions today. The two `id` anchors that already exist (`#comments-section`, `#deps-section`) are the natural targets for a write-path post.

### 2.6 Auth and identity

- OIDC middleware (`gardenio/controller/internal/oidcauth/oidcauth.go`) verifies the bearer / `X-Auth-Request-Email` and injects email into `r.Context()`. Accessor: `oidcauth.EmailFromContext(ctx)` / `oidcauth.EmailFromRequest(r)`.
- Dev bypass (`dev_auth.go`) injects a static email when `BEADS_VIEWER_DEV_AUTH_BYPASS=1` is set, refusing to start in anything that looks like a real cluster.
- IP whitelist (`middleware.go`) gates the OIDC step; not relevant to write-path correctness.
- The current handlers do NOT read identity. None of them need to — they're all reads. Any write handler must read `EmailFromContext` and pass it as `author` to the daemon.

### 2.7 Daemon write API already available

`gardenio/controller/internal/beadsapi/client_comments.go:22`:

```go
func (c *Client) AddComment(ctx context.Context, beadID, author, text string) error
```

Posts `{"author": …, "text": …}` to `/v1/beads/{id}/comments`. **No new daemon RPC is required for the comment write path.** Decision-resolution would need to be checked separately (out of scope for this doc; flagged in §6).

### 2.8 Dead / minimal code

- `partials/deps.html` route is reachable but has no client-side caller (no element does `hx-get` against it). Not dead — it's just there for future "refresh deps" buttons. Worth flagging because Fork A/C would actually start using it.
- `Similar` panel only renders when the kbeads embedder is enabled; otherwise hidden silently.
- `linkify.go` is well-scoped; nothing dead there.

---

## 3. Menu / IA forks

Three forks. Each has: annotated mockup, handler changes, template impact, route impact, deep-linking, keyboard-nav, tradeoffs.

### Fork A — tab-strip in `layout.html` (mirror iOS)

**Mockup:**
```
┌──────────────────────────────────────────────────────────────────┐
│ beads │ active │ decisions │ agents │ advice │  [search…]   logout│
├──────────────────────────────────────────────────────────────────┤
│ ┌── tab content varies ───────────────────────────────────────┐ │
│ │  Beads tab → /  : project picker + filtered list            │ │
│ │  Active tab → /active : grouped in_progress beads           │ │
│ │  Decisions  → /decisions : open decision beads with options │ │
│ │  Agents     → /agents : agent beads grouped by project      │ │
│ │  Advice     → /advice : scope sidebar + list + detail       │ │
│ └─────────────────────────────────────────────────────────────┘ │
└──────────────────────────────────────────────────────────────────┘
```

**Handler changes (Go):**
- New handlers in `server.go`: `handleActive`, `handleDecisions`, `handleAgents`, `handleAdvice` (5 reads against `daemon.ListBeadsFiltered` with type and status filters).
- `handleBeadShow` becomes a dispatcher: switch on `bead.Type`, render the per-type partial inside the existing `bead_show.html` shell.
- `handleIndex` repurposed: `/` either redirects to `/beads` or becomes the "Beads" tab landing.

**Template impact:**
- `layout.html` grows a `<nav class="tabstrip">` block above the existing search bar; the active tab is highlighted by checking a `.ActiveTab` data key.
- New page templates: `active.html`, `decisions.html`, `agents.html`, `advice.html` (each renders inside the layout).
- New partials per type: `partials/decision.html`, `partials/schedule.html`, `partials/mr.html`, `partials/agent.html`, `partials/advice.html`. `bead_show.html` becomes a tiny dispatcher (`{{ if eq .Bead.Type "decision" }}{{template "decision" .}}{{else}}…`).
- Existing routes (`/p/{project}`, `/{id}`, `/search`) keep working — they're orthogonal to the tab strip.

**Route impact (URL surface):**
```
GET /             → Beads tab (project picker + recent beads)
GET /active       → Active tab
GET /decisions    → Decisions tab
GET /agents       → Agents tab
GET /advice       → Advice tab
GET /advice?scope=role:gardener   (deep-link into a scope)
GET /p/{project}  (unchanged)
GET /search       (unchanged)
GET /{id}         (unchanged) — type dispatch happens inside the handler
```

**Deep-linking & back button:** every tab is its own URL → browser back/forward "just works"; Cmd+L → paste a `/decisions` URL works; sharing a tab URL works. No `hx-push-url` needed.

**Keyboard nav:** standard browser tab order. Optional `accesskey` letters (`b/a/d/g/v` → tabs) are cheap to add.

**Pros:**
- Feels iOS-shaped.
- Clear mental model — "I'm in the Decisions tab".
- Each tab is a single handler with a single query, easy to test (already a server_test.go pattern in `server_test.go`).

**Cons:**
- Most code-and-template surface area of the three forks. Five new pages, five new partials, layout change.
- Cross-tab consistency (filters, theming) requires discipline.

### Fork B — single list + per-type lens

**Mockup:**
```
┌──────────────────────────────────────────────────────────────────┐
│ beads │   [search…]                                       logout │   ← unchanged nav
├──────────────────────────────────────────────────────────────────┤
│  Project: <gardenio>  All projects                                │
│  Open beads (showing first 100)                                  │
│  ┌────────┬───────────────────────────────────┬────────┬──────┐  │
│  │ Bead   │ Title                             │ Type   │ Stat │  │
│  ├────────┼───────────────────────────────────┼────────┼──────┤  │
│  │ ka-xxx │ Title…                            │ task   │ open │  │
│  │ ka-yyy │ Decision title…                   │decision│ open │  │
│  └────────┴───────────────────────────────────┴────────┴──────┘  │
└──────────────────────────────────────────────────────────────────┘

   click ka-yyy ↓
┌──────────────────────────────────────────────────────────────────┐
│  decision  ●  open                                       ka-yyy  │
│  Decision title…                                                  │
│  Prompt… (markdown)                                              │
│  ┌─[Option A]─┐  ┌─[Option B]─┐  ┌─[Dismiss]─┐                  │
│  Reasoning: ____________________  Submit                          │
│  ─── DEPS ───   ─── COMMENTS ───                                 │
└──────────────────────────────────────────────────────────────────┘
```

**Handler changes (Go):**
- `handleBeadShow` switches on `bead.Type` and chooses a per-type partial. No new top-level handlers.
- Optional: `handleProject` learns a `?type=<t>` query param to filter the list view by type.

**Template impact:**
- Same per-type partials as Fork A (`partials/decision.html`, etc.).
- `bead_show.html` becomes the dispatcher.
- `layout.html` unchanged.
- `project.html` and `search.html` optionally surface the `Type` column more prominently and add a `<select>` filter chip.

**Route impact:**
- Zero new top-level routes.
- One new query param: `/p/{project}?type=decision` (optional; no breakage if absent).

**Deep-linking:** unchanged — every bead is at `/{id}` already; types are filters, not paths.

**Keyboard nav:** unchanged.

**Pros:**
- Smallest delta. Every existing route keeps working. CSS surface area unchanged. Per-type partials carry all the wins.
- Cheap to ship and revert. Per-type partials can land one at a time (decision first, schedule next, etc.).
- Doesn't *preclude* Fork A — partials built here are reusable verbatim under tabs later.

**Cons:**
- Operator still has no top-level "where are my decisions" view. They have to go via search (`type:decision status:open`) or Slack.
- "Active" / "Agents" overview screens (the iOS `InProgressTab` + `AgentsTab`) are not addressed.

### Fork C — split list+detail (htmx-driven shared selection)

**Mockup:**
```
┌──────────────────────────────────────────────────────────────────┐
│ beads │  [search…]                                        logout │
├────────────────────┬─────────────────────────────────────────────┤
│ Open beads         │  decision  ●  open                  ka-yyy  │
│  ┌─────────────┐   │  Decision title…                            │
│  │ ka-xxx task │←  │  …                                          │
│  │ ka-yyy dec  │   │  ┌─[Approve]─┐ ┌─[Reject]─┐                │
│  │ ka-zzz adv  │   │  ─── DEPS ───   ─── COMMENTS ───            │
│  └─────────────┘   │                                              │
│                    │                                              │
│  filter: [type ▾]  │                                              │
│  filter: [proj ▾]  │                                              │
└────────────────────┴─────────────────────────────────────────────┘
```

Left rail uses `<a hx-get="/b/{id}" hx-target="#detail" hx-push-url="/{id}?list=…&type=…">`; right pane is `<main id="detail">`.

**Handler changes (Go):**
- `handleBeadShow` learns to render *fragment-only* when the request has `HX-Request: true` (no layout shell).
- New thin handler (or extend `handleProject`) renders the *list pane only* on full-page load and on filter change (`hx-target="#list"`).

**Template impact:**
- `layout.html` adds a flex split (`<aside id="list">` + `<main id="detail">`).
- `bead_show.html` template is split into two: an outer wrapper (renders only on full-page load) and an inner fragment used for htmx swaps.
- Per-type partials still apply (Forks A/B).

**Route impact:**
- Same routes; `HX-Request` header is the dispatcher.
- Deep-link to `/{id}?list=type:decision&proj=gardenio` reconstructs both panes on full-page load.

**Deep-linking & back button:** **only works if** every htmx click sets `hx-push-url` to the canonical `/{id}?…` URL. Browser back/forward then triggers full-page load; the handler must re-render both panes for the URL.

**Keyboard nav:** keyboard arrows on the left list (`hx-trigger="keyup[key==='ArrowDown']"`) are the natural extension; tab order across the split is fine.

**Pros:**
- Densest information per pixel. One screen does what two clicks do today.
- Filters in the list pane apply *without* losing the open detail.

**Cons:**
- Highest complexity. Two render modes per page (full vs fragment), `hx-push-url` discipline, possible state-sync bugs.
- Mobile: split-view forces ugly responsive collapse. Probably needs media-query down to a stacked layout, which then looks like Fork B anyway.
- Browser bookmarks stop being self-evident (which list filters were active?).

### 3.x Fork comparison

| Axis                                      | A — tabstrip          | B — lens                                   | C — split-view                            |
|-------------------------------------------|-----------------------|--------------------------------------------|-------------------------------------------|
| New top-level routes                      | 4–5                   | 0                                          | 0                                         |
| New page templates                        | 4–5                   | 0                                          | (split bead_show into 2)                  |
| New partials (per-type detail)            | 4–6                   | 4–6                                        | 4–6                                       |
| Layout.html change                        | yes (tabstrip)        | no                                         | yes (split)                               |
| Deep-linking complexity                   | trivial               | trivial                                    | needs `hx-push-url` everywhere            |
| Mobile-friendliness                       | OK (tabstrip wraps)   | best (single column already)               | needs responsive collapse                 |
| iOS resemblance                           | highest               | medium                                     | lowest                                    |
| Reverse-out cost                          | medium                | low                                        | high                                      |
| Lands per-type renderers                  | yes                   | yes                                        | yes                                       |
| Lands "Active / Decisions overview"       | yes                   | no                                         | partial (filterable list)                 |

The forks are *not* mutually exclusive: Fork B is a strict subset of Fork A, and Fork A's tabs can later be relayouted into Fork C's split-view pane. The decision is mostly about *how much surface area to commit to first*.

---

## 4. Per-type renderer sketches

This section sketches three specific per-type detail templates. Naming convention: `templates/partials/type_<name>.html` defining `{{define "type_<name>"}}`. The dispatcher in `bead_show.html` becomes:

```html
{{ if eq .Bead.Type "decision" }}     {{template "type_decision" .}}
{{ else if eq .Bead.Type "schedule" }}{{template "type_schedule" .}}
{{ else if eq .Bead.Type "mr" }}      {{template "type_mr" .}}
{{ else if eq .Bead.Type "advice" }}  {{template "type_advice" .}}
{{ else }}                            {{template "type_generic" .}}    ← today's field-list dump
{{ end }}
```

Each per-type partial renders the *type-specific cards*; the generic header (status / id / title / labels / description / timestamps / deps / comments) lives in `bead_show.html` *outside* the dispatch so it's shared.

### 4.1 `decision`

Field source map (from observed `Bead.Fields` keys on existing decision beads):

| Display element              | Source                                                                 |
|------------------------------|------------------------------------------------------------------------|
| Prompt body (markdown)       | `bead.Fields["prompt"]` (string) — render via `goldmark` like description |
| Requested-by                 | `bead.Fields["requested_by"]` or fallback `bead.CreatedBy`              |
| Options list                 | `bead.Fields["options"]` (array of `{id, short, label, artifact_type}`) |
| Per-option button tint       | derived: `id == "approve"` → green, `reject/deny` → red, `skip/defer` → orange |
| Resolution (if resolved)     | `bead.Fields["resolution"]` (`{option_id, response_text, resolved_by, resolved_at}`) — show as a "Resolved" banner instead of buttons |
| Required-artifact submit box | only when *this* operator is the resolver-in-flight; out of scope for v1 |
| `is_resolved`                | derived: `bead.Status == "closed"` OR `Fields["resolution"]` present    |

Sketch:

```html
{{define "type_decision"}}
<section class="decision-card">
  {{ with .Bead.Fields.prompt }}<div class="decision-prompt">{{markdown .}}</div>{{ end }}
  {{ with .Bead.Fields.requested_by }}<p class="meta">Requested by <strong>{{.}}</strong></p>{{ end }}

  {{ if .Resolved }}
    <div class="banner banner-resolved">
      Resolved <strong>{{ .Resolution.OptionID }}</strong> by {{ .Resolution.ResolvedBy }}
      {{ if .Resolution.ResponseText }}— {{ .Resolution.ResponseText }}{{ end }}
    </div>
  {{ else if .Options }}
    <form hx-post="{{.BasePath}}/b/{{.Bead.ID}}/decision/resolve"
          hx-target="#decision-card-{{.Bead.ID}}"
          hx-swap="outerHTML">
      <textarea name="reasoning" placeholder="Reasoning (optional)"></textarea>
      <div class="decision-options">
        {{ range .Options }}
          <button class="opt opt-{{.ID}}" name="option_id" value="{{.ID}}">
            <span class="opt-label">{{.Label}}</span>
            {{ if and .Short (ne .Short .Label) }}<span class="opt-short">{{.Short}}</span>{{ end }}
            {{ if .ArtifactType }}<span class="opt-artifact">→ {{.ArtifactType}}</span>{{ end }}
          </button>
        {{ end }}
        <button class="opt opt-dismiss" formaction="{{.BasePath}}/b/{{.Bead.ID}}/decision/dismiss">Dismiss</button>
      </div>
    </form>
  {{ end }}
</section>
{{end}}
```

Notes:
- The handler shape (`POST /b/{id}/decision/resolve`) is *out of scope for this MR* but wants a sibling daemon RPC (`ResolveDecision`); the `partials/comments.html` write-path is the cheaper validator (§5).
- `option.artifact_type` is the field name on the existing decision-bead schema; surfacing it in the button gives the operator a "if I pick this, I owe a report/plan/checklist next" hint matching the Slack flow.

### 4.2 `schedule`

Field source map:

| Display element       | Source                                          |
|-----------------------|-------------------------------------------------|
| Cron string           | `bead.Fields["cron"]` (string)                   |
| Enabled               | `bead.Fields["enabled"]` (bool)                  |
| Timezone              | `bead.Fields["timezone"]` (string, default UTC)  |
| Next-fire (derived)   | computed serverside from cron + tz; nil if cannot parse |
| Last-run              | `bead.Fields["last_run"]` (RFC3339; render "ago") |
| Project / Role        | `bead.Fields["project"]` / `["role"]`            |
| Prompt template       | `bead.Fields["prompt"]` (truncate to 400 chars; `<details>` toggles full) |

Sketch:

```html
{{define "type_schedule"}}
<section class="schedule-card">
  <dl class="field-list">
    <dt>Cron</dt><dd><code>{{.Bead.Fields.cron}}</code></dd>
    <dt>Enabled</dt><dd>{{ if .Bead.Fields.enabled }}<span class="ok">Yes</span>{{ else }}<span class="warn">No</span>{{ end }}</dd>
    <dt>Timezone</dt><dd>{{ or .Bead.Fields.timezone "UTC" }}</dd>
    {{ with .NextFire }}<dt>Next fire</dt><dd>{{ . }} ({{ .Ago }})</dd>{{ end }}
    {{ with .Bead.Fields.last_run }}<dt>Last run</dt><dd>{{ . }} ({{ago .}})</dd>{{ end }}
    {{ with .Bead.Fields.project }}<dt>Project</dt><dd>{{.}}</dd>{{ end }}
    {{ with .Bead.Fields.role }}<dt>Role</dt><dd>{{.}}</dd>{{ end }}
  </dl>
  {{ with .Bead.Fields.prompt }}
    <details class="schedule-prompt"><summary>Prompt</summary><pre>{{.}}</pre></details>
  {{ end }}
</section>
{{end}}
```

The handler can compute `.NextFire` server-side using `github.com/robfig/cron/v3` (already a transitive dep from kbeads scheduler code) and inject it alongside the `Bead`. A *future* affordance: a "Pause / Resume" button that toggles `enabled` via a new daemon RPC — out of scope for this MR.

### 4.3 `mr`

Field source map (best-guess from existing MR beads in the gardenio project):

| Display element              | Source                                                   |
|------------------------------|----------------------------------------------------------|
| MR URL                       | `bead.Fields["mr_url"]` or `["url"]` (string)            |
| MR state                     | `bead.Fields["mr_state"]` (`opened` / `merged` / `closed`) — derived to color & label |
| Pipeline status              | `bead.Fields["pipeline_status"]` (`success` / `failed` / `running`) |
| Author                       | `bead.Fields["author"]` or `bead.CreatedBy`              |
| Branch / Target              | `bead.Fields["source_branch"]` / `["target_branch"]`     |
| Approvals required / given   | `bead.Fields["approvals_required"]` / `["approvals_given"]` |
| Last commit                  | `bead.Fields["last_commit_sha"]` (short) + `["last_commit_message"]` |
| Discussion / comment counts  | `bead.Fields["unresolved_threads"]`                       |

Sketch:

```html
{{define "type_mr"}}
<section class="mr-card">
  <p class="mr-state mr-state-{{.Bead.Fields.mr_state}}">
    {{ if eq .Bead.Fields.mr_state "merged" }}● merged
    {{ else if eq .Bead.Fields.mr_state "closed" }}✗ closed
    {{ else }}● open
    {{ end }}
  </p>
  <p>
    <a href="{{.Bead.Fields.mr_url}}" target="_blank" rel="noopener">{{.Bead.Fields.mr_url}}</a>
  </p>
  <dl class="field-list">
    {{ with .Bead.Fields.pipeline_status }}<dt>Pipeline</dt><dd class="pipe-{{.}}">{{.}}</dd>{{ end }}
    {{ with .Bead.Fields.source_branch }}<dt>Source</dt><dd><code>{{.}}</code></dd>{{ end }}
    {{ with .Bead.Fields.target_branch }}<dt>Target</dt><dd><code>{{.}}</code></dd>{{ end }}
    {{ if .Bead.Fields.approvals_required }}
      <dt>Approvals</dt><dd>{{.Bead.Fields.approvals_given}} / {{.Bead.Fields.approvals_required}}</dd>
    {{ end }}
    {{ with .Bead.Fields.unresolved_threads }}<dt>Unresolved</dt><dd>{{.}}</dd>{{ end }}
    {{ with .Bead.Fields.last_commit_sha }}<dt>Last commit</dt><dd><code>{{.}}</code> {{$.Bead.Fields.last_commit_message}}</dd>{{ end }}
  </dl>
</section>
{{end}}
```

Notes:
- **Field availability is uncertain.** Before cutting a real implementation MR, run `kd list --type mr -f json | jq '.[].fields | keys'` against the live daemon to confirm which keys actually populate. If `mr_state` isn't there, fall back to inferring from labels (`mr-merged`, `mr-failed`).
- A small live-refresh affordance (`hx-get` every 60s targeting the card) could keep pipeline status fresh; lower priority than getting a static rendering right.

### 4.4 `advice` (bonus — included because it's the highest-cardinality type)

Field source map:

| Display element     | Source                                                     |
|---------------------|------------------------------------------------------------|
| Scope               | derived from `bead.Labels` (`global` / `rig:<v>` / `role:<v>` / `agent:<v>` / `g{N}:…`) |
| Hook trigger        | `bead.Fields["hook_trigger"]` (string)                      |
| Hook command        | `bead.Fields["hook_command"]` (string)                      |
| Hook timeout        | `bead.Fields["hook_timeout"]` (int, seconds)                |
| Hook on-failure     | `bead.Fields["hook_on_failure"]` (`block` / `warn` / `ignore`) |
| Subscriptions       | `bead.Fields["subscriptions"]` (string list)                |
| Subscriptions exclude | `bead.Fields["subscriptions_exclude"]`                    |
| Markdown body       | `bead.Description`                                          |

Render order: scope chip + body-markdown card; hook card only if any hook field is non-empty; subscription card only if either subs list is non-empty; targeting card always (even if `global`). This mirrors `AdviceDetailView.swift` line-for-line.

### 4.5 Why three forks all need the same partials

The per-type partials (`type_decision.html` etc.) are content; the menu/IA fork (Section 3) is chrome. Whichever fork wins, **the same per-type partials carry the iOS-parity wins.** That's why this section is fork-independent.

---

## 5. Commenting UX sketch (write-path)

### 5.1 Affordance

A `<form>` lives at the bottom of `partials/comments.html`, *inside* `<section id="comments-section">` so it gets swapped together with the list:

```html
{{define "comments"}}
<section id="comments-section" class="comments-section"
  {{if eq .Bead.Status "in_progress"}}hx-get="{{.BasePath}}/b/{{.Bead.ID}}/comments" hx-trigger="every 30s" hx-swap="outerHTML"{{end}}>
  <h2>Comments ({{len .Comments}})</h2>
  {{ if .Comments }}
    <div class="comment-list">
      {{ range .Comments }}
        <div class="comment">
          <div class="comment-meta"><strong>{{.Author}}</strong> · {{.CreatedAt.Format "2006-01-02 15:04 MST"}}</div>
          <div class="comment-body">{{.TextHTML}}</div>
        </div>
      {{ end }}
    </div>
  {{ else }}
    <p class="empty">—</p>
  {{ end }}

  {{ if .CanComment }}                              ← set true by handler when EmailFromContext != ""
  <form class="comment-form"
        hx-post="{{.BasePath}}/b/{{.Bead.ID}}/comments"
        hx-target="#comments-section"
        hx-swap="outerHTML"
        hx-disabled-elt="find textarea, find button">
    <label for="comment-text">Comment as <strong>{{.AuthorEmail}}</strong></label>
    <textarea id="comment-text" name="text" rows="3" required
              placeholder="Add a comment… (markdown supported)"></textarea>
    <button type="submit">Post</button>
  </form>
  {{ end }}
</section>
{{end}}
```

Affordance lives in *exactly one place* (the partial). All forks (A/B/C) inherit it for free because the partial is mounted under every page.

### 5.2 POST handler shape

```go
// In server.go's RegisterRoutes:
mux.HandleFunc("POST /b/{id}/comments", s.handleAddComment)

// handleAddComment posts a comment as the authenticated user and re-renders
// the comments fragment.
func (s *Server) handleAddComment(w http.ResponseWriter, r *http.Request) {
    id := r.PathValue("id")
    if id == "" {
        http.Error(w, "Missing bead id", http.StatusBadRequest); return
    }
    email := oidcauth.EmailFromRequest(r)
    if email == "" {
        http.Error(w, "Unauthenticated", http.StatusUnauthorized); return
    }
    if err := r.ParseForm(); err != nil {
        http.Error(w, "Bad form", http.StatusBadRequest); return
    }
    text := strings.TrimSpace(r.PostForm.Get("text"))
    if text == "" {
        http.Error(w, "Empty comment", http.StatusBadRequest); return
    }
    if len(text) > 10_000 {
        http.Error(w, "Comment too long", http.StatusRequestEntityTooLarge); return
    }
    if err := s.daemon.AddComment(r.Context(), id, email, text); err != nil {
        s.logger.Error("adding comment", "id", id, "author", email, "error", err)
        http.Error(w, "Failed to post comment", http.StatusBadGateway); return
    }
    // Re-render the comments fragment so the swap puts the new comment in place.
    s.handleBeadComments(w, r)
}
```

Implementation notes:
- The handler reuses `handleBeadComments` for the response — that handler already builds the `commentView` slice with markdown + linkification, so the post-write render path is a single line.
- `EmailFromRequest` (not `EmailFromContext`) so we honor a `X-Auth-Request-Email` ForwardAuth header when present.
- Rate limiting: not in v1. If the operator wants it, a token-bucket per-email middleware fits in 30 lines and lives behind the OIDC middleware.

### 5.3 htmx swap behavior

- `hx-target="#comments-section"` + `hx-swap="outerHTML"` → on success the whole comments section, including the form, is replaced. Form input clears naturally because the new form has empty `<textarea>`.
- `hx-disabled-elt="find textarea, find button"` → during the request, the textarea + Post button are disabled, preventing double-submits.
- On 4xx/5xx response, htmx by default does NOT swap (status outside `200-299`). The user sees a transient error toast (or, simpler, a banner inserted via `hx-on::response-error`). We can lean on htmx's `htmx:responseError` event to flash a message; out-of-scope for the design doc.

### 5.4 Optimistic update story

Three options, in order of complexity:

1. **No optimism** (recommended for v1). The Post button shows "Posting…" via `hx-indicator`; on response, the swap happens. RTT to the in-cluster daemon is <50ms — there's no UX win from optimism worth the complexity of reconciling on error.
2. **Append-on-submit, swap-on-response.** Use `hx-on::before-request` to append a temporary `<div class="comment pending">` to the list; the response swap replaces it with the canonical render. Risk: error path needs to remove the pending node.
3. **Full client-side optimistic + reconcile.** Out of scope.

Pick (1).

### 5.5 Auth interaction with Keycloak / OIDC middleware

The auth middleware chain is `ipWhitelistMiddleware → authMiddleware (OIDC | dev bypass) → appMux` (`main.go:91-94`). Both the `GET` and `POST` routes go through the same chain, so:

- An unauthenticated POST is already blocked by the OIDC middleware (302 to login). Dead code in the handler? Not quite — defense in depth is cheap: the `if email == ""` 401 in §5.2 catches a future regression where the middleware order changes.
- The author of record is `EmailFromContext` / `EmailFromRequest`, not a user-controlled form field. There is **no** `<input name="author">` in the form — the email is shown as a label only (`Comment as <strong>{{.AuthorEmail}}</strong>`).
- CSRF: htmx requests carry the `Cookie` header; `oauth2-proxy` sessions are typically cookie-based. SameSite=Lax cookies are CSRF-safe for POST from same-origin forms. If the deployment uses `oauth2-proxy --cookie-samesite=none`, CSRF mitigation needs an explicit token (out of scope for this doc; flag in the decision bead).
- Audit: `daemon.AddComment` already logs author server-side; nothing new needed.

### 5.6 Where the affordance lives in each fork

- **Fork A:** identical — the form is in `partials/comments.html` which is mounted on every per-type detail template. Each tab's detail screen carries the form.
- **Fork B:** identical — `bead_show.html` dispatches by type but always renders comments at the bottom.
- **Fork C:** in the right pane only; `hx-target="#comments-section"` continues to work because that DOM id lives inside `#detail`.

---

## 6. Open questions for the decision bead

The decision bead (to be created as a child of `ka-0000002538`) selects the IA fork. Below is the exact `gb decision create` invocation. **Not yet executed** — this MR is the artifact that gates it.

### 6.1 Sharpened option labels

- **A — tabstrip-mirror-iOS.** Land the full per-tab UI: `/active`, `/decisions`, `/agents`, `/advice`, plus per-type partials and tabstrip in `layout.html`. Highest scope; iOS users feel at home.
- **B — lens-only.** Land per-type partials behind `bead_show.html` dispatch, no new top-level routes. Smallest delta; most reversible.
- **C — split-view.** Split list+detail with htmx-driven shared selection. Densest UI; deep-linking via `hx-push-url`.
- **D — defer / scope down.** Land *only* the comment write-path (§5) and the `decision`-type per-type partial; postpone every other fork until the operator sees the comment loop in production.

### 6.2 Drafted invocation

```bash
gb decision create --no-wait \
  --prompt="beads-viewer iOS-parity Phase 2 — design doc landed (gardenio/docs/beads-viewer-ios-parity-phase2.md). Pick the IA fork to implement next. Recommendation: B (lens-only) → ship per-type partials and the comment write-path first; promote to A (tabs) in a follow-up if/when overview screens (Active/Decisions) earn their keep." \
  --options='[
    {"id":"a","short":"A — tabstrip","label":"Mirror iOS — add /active /decisions /agents /advice tabs in layout.html, per-type partials, full IA refactor","artifact_type":"plan"},
    {"id":"b","short":"B — lens-only","label":"Smallest delta — per-type partials behind bead_show.html dispatch + comment write-path; no new top-level routes","artifact_type":"plan"},
    {"id":"c","short":"C — split-view","label":"Split list+detail with htmx-driven selection and hx-push-url; densest UI","artifact_type":"plan"},
    {"id":"d","short":"D — defer","label":"Land only comments write-path + decision-type partial; postpone all other forks until the comment loop is validated in production","artifact_type":"plan"}
  ]'
gb yield
```

Each option produces `artifact_type=plan` because the chosen fork's next artifact is a concrete implementation plan (with task beads + partial naming + handler stubs), not code.

### 6.3 Secondary forks the implementation plan must answer

These are *not* in the IA decision bead — they belong in the chosen plan:

- **Per-type template naming.** `partials/type_decision.html` vs `partials/decision.html` vs `bead_decision.html`. Recommendation: `partials/type_<name>.html` to keep the `partials/` directory single-purpose.
- **Generic fallback name.** `type_generic` vs keeping today's `bead_show.html` body. Recommendation: extract today's body into `type_generic` so the dispatcher is uniform.
- **Decision-resolution write-path.** Comments first; decision-resolve POST is a follow-up that needs a daemon-side `ResolveDecision` RPC (the `kd decision resolve` CLI exists; verify whether the kbeads HTTP API exposes it).
- **CSRF posture.** Confirm whether oauth2-proxy cookies are SameSite=Lax (no token needed) or None (token needed). Block on the cookie config.
- **Live-refresh per type.** The current 30s comment poll is conditional on `status=in_progress`. Should `decision` beads poll every 30s too (so a Slack-resolved decision disappears from the page)? Probably yes; cheap.

---

## Appendix A — Handler-test scaffolding pointer

`gardenio/controller/cmd/beads-viewer/server_test.go` (424 lines) and `bead_show_test.go` (539 lines) already use a fake `beadAPI` interface (defined at `server.go:62`). Any new handler — `handleAddComment`, `handleDecisions`, etc. — slots into the same testing pattern: define a fake `beadAPI` method, exercise the route, assert HTML content.

## Appendix B — Style/screenshot MR formula

The visual surface of this work is significant once Forks A or C land. The beads-viewer style/UX MR formula `ka-0000002540` exists for that. This MR (docs only) does not invoke it — there are no visual changes to screenshot. The implementation MR(s) following the decision should apply the formula.

## Appendix C — File touches for each fork (if implemented)

| File                                                            | Fork A | Fork B | Fork C |
|-----------------------------------------------------------------|--------|--------|--------|
| `cmd/beads-viewer/server.go` — RegisterRoutes + new handlers     | ++     | +      | +      |
| `cmd/beads-viewer/main.go`                                       | —      | —      | —      |
| `cmd/beads-viewer/templates/layout.html`                         | + tabs | —      | + split|
| `cmd/beads-viewer/templates/bead_show.html` — dispatcher         | +      | +      | + (split into wrap+frag) |
| `cmd/beads-viewer/templates/partials/type_*.html` (new, 4–6)     | +      | +      | +      |
| `cmd/beads-viewer/templates/partials/comments.html` — write form | +      | +      | +      |
| `cmd/beads-viewer/templates/active.html` (new)                   | +      | —      | —      |
| `cmd/beads-viewer/templates/decisions.html` (new)                | +      | —      | —      |
| `cmd/beads-viewer/templates/agents.html` (new)                   | +      | —      | —      |
| `cmd/beads-viewer/templates/advice.html` (new)                   | +      | —      | —      |
| `internal/beadsapi/client_comments.go`                           | — (already supports AddComment) | — | — |
| `internal/beadsapi/` — new ResolveDecision client (decision write-path, follow-up) | + (later) | + (later) | + (later) |

`++` = significant new code, `+` = small new code, `—` = unchanged.
