# Gardener Page — implementation design doc

**Bead:** ka-0000000062 (research+design task) · **Epic:** ka-0000001579
**Operator:** Matt Baker (oidc:matthew.baker@acme.example)
**Origin:** Slack thread 1780247286.508479 — "keep the trigger page, ADD a gardener page that captures everything an agent card shows but folds every iteration of that gardener into one thread representing the gardener's whole life. Gardeners must be per-user; backfill is in scope."
**Prior art:** `gardenio/docs/research/beads-viewer-gardeners-view.md` (ka-0000004722, 2026-04-27) — the *strategic* fork research (Fork A/B/C). This doc is the *implementation-ready* refinement that the epic commissioned: it picks concrete structs, queries, routes, and a backfill procedure for the eight implementation children.

**Status:** design only — no Go/template/schema change in this MR. Implementation lands in the eight children listed in §10.

---

## 0. TL;DR

- **What we build:** a `/gardener/{id}` page that renders a gardener *header* (terminal goal, run budget, counters, stop/progress, owning user) plus an ordered **life-thread** of iteration cards — each iteration card reusing the existing agent-card view-model so it "captures everything an agent card shows" for free.
- **The one load-bearing fact (verified against source):** `stampGardenerLoopContext` (`internal/bridge/triggers.go:1115-1145`) stamps `schedule_id = <gardener trigger bead ID>` onto **every** agent bead a gardener spawns, plus `schedule_prior_agent_id`, `schedule_runs_spawned/completed`, `schedule_terminal_goal`, `slack_user_id`, and `user_bead_id`. `schedule_id` is **written on the bridge side but never read in beads-viewer.** Wiring that read is the heart of this work.
- **Eight decisions** (one per open question), each mapped to one implementation child in §10. Headline recommendations:
  1. **View model:** `gardenerView` (header, from the trigger bead's `GardenerSpec`) + `gardenerIterationView` (embeds `agentRowView`).
  2. **Lookup:** daemon-side indexed `schedule_id` field filter — already half-built; add the index + one client param. Recommended over the `Limit:500` client-side scan.
  3. **Route:** `GET /gardener/{id}` eager page + `GET /gardener/{id}/runs` lazy section, mirroring `/u/{slug}`.
  4. **Ordering:** `schedule_prior_agent_id` chain (authoritative) with `created_at` fallback when the chain breaks.
  5. **Ownership:** three-tier resolve `UserBeadID → SlackUserID → operated_by_id`; surface owner chip; add "Their gardeners" to `/u/{slug}`.
  6. **Backfill:** a `kd admin backfill gardener-attribution` subcommand (sibling of the existing `backfill user-bead-id`), idempotent, `--dry-run`.
  7. **Live updates:** extend the agents SSE filter to also match `schedule_id`. Recommended over `liveAttrs` polling.
  8. **Trigger relationship:** keep `/triggers` as-is; ADD a link from each `role:gardener` row → `/gardener/{id}`, and from each agent card with `schedule_id` → `/gardener/{schedule_id}`.

---

## 1. Ground truth — what the code actually does today

All anchors verified by reading source in this MR's base tree.

### 1.1 The gardener trigger bead and its `GardenerSpec`

A gardener is a `type=trigger` bead carrying `trigger:schedule` + `role:gardener` labels. Its gardener state lives in a **single JSON-encoded `gardener` field** on the bead, parsed into `GardenerSpec`:

- `ParseTriggerDef` reads it: `if raw := strings.TrimSpace(bead.Fields["gardener"]); raw != "" { json.Unmarshal([]byte(raw), &cap); t.Gardener = &cap }` — `internal/bridge/triggers.go` (parse path, ~line 1651).
- `GardenerSpec` (`internal/bridge/triggers.go:218-270`) carries: `Name`, `TerminalGoal`, `ExitCondition`, `OnExit`, `Stop`/`Progress` (`json.RawMessage`), `RunBudget`, `MaxRuns`, `RetryOnFailure`, `MaxRetries`, `RunTimeout`, `Timeout`, `OneSpawnAtATime`, `RunsSpawned`, `RunsCompleted`, `ConsecutiveFailures`, `LastAgentID`, `LastCheckedAgent`, `RunLog`, `ProgressState`, `ForceClose`, `AssignBead`, `SlackChannel`, **`SlackUserID`**, **`UserBeadID`**, plus deprecated legacy `Watch*` fields.

### 1.2 The per-iteration stamp (the load-bearing fact)

`stampGardenerLoopContext(fields, trig)` (`internal/bridge/triggers.go:1115-1145`) writes these custom fields onto the agent bead **of each spawned iteration**:

| Field stamped | Source | Notes |
|---|---|---|
| `schedule_id` | `trig.ID` | **= the gardener trigger bead's ID.** The FK that links iteration → gardener. Always set. |
| `schedule_title` | `trig.Title` | |
| `schedule_runs_spawned` | `spec.RunsSpawned + 1` | snapshot counter at spawn |
| `schedule_runs_completed` | `spec.RunsCompleted` | snapshot counter |
| `schedule_max_runs` | `spec.MaxRuns` | |
| `schedule_terminal_goal` | `spec.TerminalGoal` | when set |
| `schedule_name` | `spec.Name` | when set |
| `schedule_slack_channel` | `spec.SlackChannel` | when set |
| `slack_user_id` | `spec.SlackUserID` | **NB: stamped as `slack_user_id`, NOT `schedule_slack_user_id`** |
| `user_bead_id` | `spec.UserBeadID` | when set |
| `schedule_prior_agent_id` | `spec.LastAgentID` | back-pointer to the **previous** iteration's agent bead; forms the chain. Set when non-empty (i.e. on every run after the first). |
| `spawn_source` | `"schedule"` | |

> **Correction to the epic/task text.** The epic and task copy refer to a `schedule_slack_user_id` field. The code stamps the owner as **`slack_user_id`** (and `user_bead_id`). The whole design below uses the real field names. The backfill in §7 reads/writes these real names.

### 1.3 The agent-card view-model we reuse

`agentRowView` (`cmd/beads-viewer/agents.go:151-197`) is the canonical agent-card model: `ID, Name, Role, Project, AgentState, PodPhase, Status, TaskID, TaskTitle, SpawnedBy, OperatedByDisplay, OperatedBySlug, Desc, Ago, UpdatedAtRFC, Uptime, CreatedAtRFC, LongRunning, Stale, TerminalURL, SlackURL, WrapUpSummary, WrapUpMRs`.

It is populated by `agentRowFromBeadCtx(ctx, b *beadsapi.BeadDetail, project, now)` (`agents.go:1510-1550`), reading `b.Fields["agent"/"role"/"task_id"/"task_title"/"spawned_by"/"wrapup"]`, plus timestamps and computed `LongRunning`/`Stale`. **Reusing this for each iteration card is what gives "everything an agent card shows" for free.** Caveat: `WrapUpSummary`/`WrapUpMRs` are populated only for closed rows, and MR resolution costs a daemon round-trip per closed iteration.

### 1.4 The existing per-user homepage (the pattern to mirror)

`/u/{slug}` (`cmd/beads-viewer/user_home.go`):
- `handleUserHome` (line 115) resolves slug → operator via `s.daemon.LookupOperatorBySlug(ctx, slug)` → `operatorID`.
- `buildUserHomeSections` (line 289) builds eager sections in parallel (`sync.WaitGroup`); lazy sections get placeholder view-models with `LazyURL` set and the template renders an HTMX shell `hx-trigger="revealed once"` (`templates/partials/user_home_section.html:9`).
- "Their automations" (lines 366-380) is the per-user hook: `runUserHomeSection(... ListBeadsQuery{OperatedByID: operatorID, Types: []string{"schedule"}, Statuses: {"open","in_progress"}, Limit: userHomeListLimit}, slug, "No active automations.", "schedule", scheduleEnabledFilter())`.
- Lazy section handlers (e.g. `handleUserHomeSectionForwarded`, line 573) extract the path value, build the section, and `renderPartial(... "templates/partials/user_home_section.html", "user_home_section", ...)`.

### 1.5 Route / template / auth wiring

`cmd/beads-viewer/server.go`:
- Routes: `mux.HandleFunc("GET /u/{slug}", s.handleUserHome)` (line 833); lazy section routes at 834-838.
- Auth gate factories: `operatorOnly := oidcauth.RequireRole(oidcauth.RoleOperator, s.logger)` (~859), `viewerOnly := ...RoleViewer...` (~860). Reads use plain `mux.HandleFunc` (viewer-scoped by the ambient auth middleware); writes wrap `operatorOnly(http.HandlerFunc(...))` (e.g. `POST /create-gardener`, ~957).
- Page templates: a `pageNames` slice (~648) is looped (~650-654) to `ParseFS` each page with `layout.html` + page template + partials. **A new page = add its name to `pageNames` AND create `templates/<name>`.**
- Partials: a `fragmentPartials` list (~461-517, includes `user_home_section.html` at ~513) registered into the `partials` map (~697-700).

### 1.6 The current "all iterations" lookup (the thing we replace)

`gb gardener audit` (`cmd/gb/gardener_audit.go:116-136`) lists `type=agent` with `Limit:500`, then **client-side** filters `b.Fields["schedule_id"] == scheduleID`. This breaks for gardeners with >500 lifetime iterations (the busy delivery/release gardeners already churn dozens of runs/day).

### 1.7 Daemon-side field filtering already exists (verified)

- Proto: `ListBeadsRequest.field_filters map<string,string>` (field 11) — `kbeads/proto/beads/v1/beads.proto`, generated at `kbeads/gen/beads/v1/beads.pb.go:345`.
- SQL: `internal/store/postgres/queries.go:589-606` turns each `filter.Fields[key]=val` into `fields->>'key' = $n` (with `=*` presence and `=''` absence sentinels).
- HTTP: `?field=key=value` → `filter.Fields` (`internal/server/http_beads.go`), and `kd list -f key=value` → `req.FieldFilters` (`cmd/kd/list.go:124-134`).
- **Index status:** migration `025_fields_hot_indexes.up.sql` adds partial B-tree expression indexes for `agent`, `agent_state`, `project`, `role`, `slack_user_id` — but **NOT `schedule_id`**. So a `schedule_id` filter works today but degrades to a sequential scan.

The **controller-side** client (`internal/beadsapi/client_query.go:13`, `ListBeadsQuery`) does **not** yet expose a generic `Fields` map to forward to `field_filters` (it has typed params: `Types`, `Statuses`, `OperatedByID`, `ExcludeScheduled`, etc.). Adding it is a small, well-precedented change (§4).

---

## 2. Decision 1 — Gardener view model

**Child:** ka-0000005461 (Gardener view model + helper).

Two structs, mirroring the existing `triggerRowView` (trigger metadata) / `agentRowView` (agent card) split.

```go
// cmd/beads-viewer/gardener.go

// gardenerView is the gardener-page header. Sourced from the gardener trigger
// bead's `gardener` JSON field (GardenerSpec) plus bead identity/ownership.
type gardenerView struct {
	// Identity (from the trigger bead)
	ID    string // the trigger bead ID == the schedule_id stamped on iterations
	Title string

	// Goal + exit (GardenerSpec)
	Name          string
	TerminalGoal  string
	ExitCondition string
	OnExit        string

	// Budget + retry (GardenerSpec)
	RunBudget       int
	MaxRuns         int
	RetryOnFailure  bool
	MaxRetries      int
	RunTimeout      string
	Timeout         string
	OneSpawnAtATime bool

	// Runtime counters (GardenerSpec — engine-owned)
	RunsSpawned         int
	RunsCompleted       int
	ConsecutiveFailures int
	LastAgentID         string

	// Opaque structured JSON, pretty-printed (engine parser deferred to B6).
	StopJSON     string
	ProgressJSON string

	// Ownership (resolved — see Decision 5)
	OwnerDisplay string // human label
	OwnerSlug    string // -> /u/{slug} when resolvable; "" => no link
	SlackUserID  string // GardenerSpec.SlackUserID (raw)
	UserBeadID   string // GardenerSpec.UserBeadID (raw)

	// Trigger-bead surface
	Enabled      bool
	IsClosed     bool
	Ago          string
	UpdatedAtRFC string

	// Derived from the iteration set (filled by the runs section)
	IterationCount int // total agents with schedule_id == ID
	LiveCount      int // open/in_progress iterations
}

// gardenerIterationView is one run in the gardener's life-thread. It EMBEDS
// agentRowView so each card renders identically to an /agents card, then
// adds the schedule_* context stamped by stampGardenerLoopContext.
type gardenerIterationView struct {
	agentRowView // by value: ID, Name, Role, Status, Uptime, WrapUpSummary, WrapUpMRs, ...

	// Per-run context (from the agent bead's stamped fields)
	RunNumber    int    // canonical run number (Decision 4)
	RunsSpawned  int    // schedule_runs_spawned snapshot (fallback ordering signal)
	MaxRuns      int    // schedule_max_runs
	PriorAgentID string // schedule_prior_agent_id (chain back-pointer)
	TerminalGoal string // schedule_terminal_goal (per-run snapshot)

	// Presentation
	IsLive      bool   // status != closed — render "in progress" badge, no fixed number
	RunLabel    string // "Run #3 of 10", "Run #3 (in progress)", "Run #3 (chain broken — by time)"
}
```

**Mapping rules**
- `gardenerView` fields come straight from `GardenerSpec` (decode `bead.Fields["gardener"]`) + trigger-bead status/timestamps. `Stop`/`Progress`/`ProgressState` are `json.RawMessage` today; render pretty-printed JSON in a `<details>` block — do **not** block on the B6 stop-spec parser.
- `gardenerIterationView.agentRowView` is produced by the **existing** `agentRowFromBeadCtx` — no change to that function. The gardener helper just wraps each returned row and overlays the parsed `schedule_*` ints/strings.
- `IterationCount`/`LiveCount` on the header are computed from the iteration slice after the runs lookup (Decision 2), so the header and the thread stay consistent in one render.

**Helper signature**
```go
// gardenerViewFromBead builds the header from a gardener trigger bead.
func (s *Server) gardenerViewFromBead(ctx context.Context, b *beadsapi.BeadDetail) (gardenerView, error)

// gardenerIterations fetches + orders the life-thread (Decisions 2 + 4).
func (s *Server) gardenerIterations(ctx context.Context, scheduleID string, now time.Time) ([]gardenerIterationView, error)
```

---

## 3. Decision 2 — Iteration lookup (RECOMMEND: daemon-side indexed `schedule_id` filter)

**Child:** ka-0000000412 (Daemon/query: efficient lookup by schedule_id).

**Recommendation: daemon-side indexed field filter.** Reject the client-side `Limit:500` scan: long-lived gardeners already exceed 500 lifetime iterations, and the scan grows with the *whole* agent population, not the gardener's slice.

The daemon already supports `field_filters` end-to-end (§1.7); two small additions complete the path:

### 3.1 Add the index (one migration)

`kbeads/internal/store/postgres/migrations/031_fields_schedule_id_index.up.sql` (next free number — base tree has up to 030):

```sql
-- Gardener Page (epic ka-0000001579): index beads.fields->>'schedule_id' so
-- "all iterations of gardener X" is an O(log n) indexed lookup, not the
-- sequential scan that gb gardener audit does today (cmd/gb/gardener_audit.go).
-- Mirrors migration 025's partial-B-tree approach for hot single-key equality.
CREATE INDEX IF NOT EXISTS idx_beads_fields_schedule_id
    ON beads ((fields->>'schedule_id'))
    WHERE fields ? 'schedule_id';
```
Down migration drops it. Partial (`WHERE fields ? 'schedule_id'`) keeps the index small — only scheduler/gardener-spawned beads carry the key. Single-key equality is a textbook B-tree fit; no GIN needed (same rationale as 025's header comment).

### 3.2 Forward `Fields` from the viewer client

Extend `beadsapi.ListBeadsQuery` (`internal/beadsapi/client_query.go:13`) with a generic map and forward it in `ListBeadsFiltered`:

```go
// Fields filters on custom-field equality (fields->>'key' = 'value').
// Forwarded to the daemon's field_filters. Each entry adds one predicate.
Fields map[string]string
```
```go
// in ListBeadsFiltered's param builder:
for k, v := range q.Fields {
	params.Add("field", k+"="+v) // daemon parses ?field=key=value (http_beads.go)
}
```
(Use `params.Add`, not `Set`, so multiple field predicates compose. The daemon's HTTP layer already splits each `field=` value on the first `=`.)

### 3.3 The lookup call

```go
res, err := s.daemon.ListBeadsFiltered(ctx, beadsapi.ListBeadsQuery{
	Types:  []string{"agent"},
	Fields: map[string]string{"schedule_id": gardenerID},
	Sort:   "created_at",
	Limit:  500, // page size, not a correctness cap — see pagination note
})
```

**Pagination:** keep `Limit` as a page size and use `Offset` to walk if `res.Total > len(res.Beads)`. For the eager page render, cap the *rendered* thread at the most recent N (e.g. 50) and lazy-load older runs via `/gardener/{id}/runs?before=<cursor>` (Decision 3). The header's `IterationCount` uses `res.Total` so the count is honest even when the thread is truncated.

**Migration ordering:** the index migration is independent and can land first (idempotent `IF NOT EXISTS`, applied via `kd migrate`). The client `Fields` param can land before the index — it just runs un-indexed until the migration applies. No deploy coupling.

---

## 4. Decision 3 — Route + render

**Child:** ka-0000001207 (Add /gardener/{id} page).

Mirror `/u/{slug}`: an eager page plus a lazy "older runs" section.

### 4.1 Routes (`server.go`, in the route block ~833)

```go
mux.HandleFunc("GET /gardener/{id}",       s.handleGardenerDetail)       // eager page
mux.HandleFunc("GET /gardener/{id}/runs",  s.handleGardenerRunsSection)  // lazy older-runs section
```
Both are **reads → viewer-scoped** (plain `HandleFunc`, gated by ambient auth like `/u/{slug}` and `/agents`). No operator-only control actions on the gardener page in v1 (toggling enable/disable stays on `/triggers`, which already has `operatorOnly` bulk actions). If a future "pause this gardener" button is added, wrap only that POST in `operatorOnly`.

### 4.2 Handlers

```go
func (s *Server) handleGardenerDetail(w http.ResponseWriter, r *http.Request) {
	id := strings.TrimSpace(r.PathValue("id"))
	if id == "" { http.Error(w, "missing gardener id", http.StatusBadRequest); return }
	ctx := r.Context()
	bead, err := s.daemon.GetBead(ctx, id)
	if err != nil { s.logger.Error("gardener detail: get bead", "id", id, "err", err); http.Error(w, "internal error", 500); return }
	if bead == nil { http.NotFound(w, r); return }

	now := time.Now()
	header, err := s.gardenerViewFromBead(ctx, bead)
	if err != nil { s.logger.Error("gardener detail: build header", "id", id, "err", err); http.Error(w, "internal error", 500); return }
	iters, err := s.gardenerIterations(ctx, id, now) // capped to recent N for eager render
	if err != nil { s.logger.Error("gardener detail: iterations", "id", id, "err", err); /* render header + empty thread, surface a soft error */ }
	header.IterationCount = len(iters) // or res.Total when paginating

	s.render(w, r, "gardener_detail.html", map[string]any{
		"Gardener":    header,
		"Iterations": iters,
		"BasePath":   s.basePath,
		"RunsLazyURL": s.basePath + "/gardener/" + id + "/runs",
	})
}

func (s *Server) handleGardenerRunsSection(w http.ResponseWriter, r *http.Request) {
	id := strings.TrimSpace(r.PathValue("id"))
	if id == "" { http.NotFound(w, r); return }
	// fetch older runs (Offset/cursor), render the section partial
	s.renderPartial(w, r, "templates/partials/gardener_runs_section.html", "gardener_runs_section",
		map[string]any{"Iterations": older, "BasePath": s.basePath})
}
```

### 4.3 Templates

- New page `templates/gardener_detail.html` — add `"gardener_detail.html"` to the `pageNames` slice (~648). Layout: header block (terminal goal, budget, counters, owner chip, enabled badge, stop/progress `<details>`) + the life-thread (iteration cards) + the lazy "older runs" shell.
- New partial `templates/partials/gardener_runs_section.html` (section, mirrors `user_home_section.html` shape with `hx-trigger="revealed once"`) — register it in `fragmentPartials` (~513) and the `partials` map (~700).
- Iteration cards **reuse the existing agent-row partial**. Each `gardenerIterationView` embeds `agentRowView`, so the card markup (`templates/partials/...` used by `/agents`) renders unchanged; the gardener template adds the run-number ribbon (`{{.RunLabel}}`) and live badge around it.

### 4.4 Nav

Add a link in the existing `/triggers` rows (Decision 8) rather than a new top-level nav entry for v1 — keeps IA changes minimal and is consistent with "ADD, don't replace." A top-level `/gardeners` roster (prior-art Fork A) is a follow-up, not in this epic's children.

---

## 5. Decision 4 — Ordering authority (RECOMMEND: chain primary, `created_at` fallback)

**Child:** ka-0000004816 (Iteration life-thread timeline).

**Primary — `schedule_prior_agent_id` chain.** It is the gardener's own causal record: each iteration's `schedule_prior_agent_id` points at the previous run's agent bead (`triggers.go:1143`). Reconstruct the ordered list by indexing iterations by ID and walking the chain from the head:
1. The head is the iteration whose ID == `GardenerSpec.LastAgentID` (the latest), or — if that is stale — the iteration with no other iteration pointing at it via `schedule_prior_agent_id`.
2. Walk `schedule_prior_agent_id` backward to the root (the run with empty `schedule_prior_agent_id`).
3. Assign run numbers from the root forward: 1, 2, 3, … N.

**Fallback — `created_at` sort.** The chain can break: a missed run, a manually edited/deleted bead, or a gardener restart that resets `LastAgentID`. Detect a break when (a) a `schedule_prior_agent_id` references an ID not in the set, or (b) the walked chain length < total iterations. On break, **fall back to `created_at` ascending** for the whole set and number sequentially. Mark affected cards `RunLabel = "Run #k (by time)"` so the operator knows ordering is heuristic, not causal.

**Why not the counter?** `schedule_runs_spawned` is a *snapshot at spawn* and is the right thing to *display* ("Run 7 of 10") but is unreliable as a *sort key* across restarts (counters can reset/duplicate). Use it only to enrich the label when present, never as the sort authority.

**In-flight iteration.** The latest run is typically open/in_progress. Identify it as `status != closed` (or == `LastAgentID`). Render it at the top of the thread with `IsLive=true` and `RunLabel = "Run #N (in progress)"` — no dependence on a final number. `agentRowFromBeadCtx` degrades gracefully for in-flight rows (no uptime/MR resolution); the card simply shows fewer pills, which is fine.

**Display:** newest-first in the thread (operators care about the live/last run first); the lazy section appends older runs. Run number is computed root-forward but displayed on each card regardless of thread order.

---

## 6. Decision 5 — Per-user ownership model

**Child:** ka-0000002818 (Per-user gardener ownership).

**Resolution (priority order):**
1. `GardenerSpec.UserBeadID` — resolve the user bead → operator → slug.
2. `GardenerSpec.SlackUserID` — `LookupOperatorBySlackID` (or alias `slack:<id>`).
3. Trigger bead `operated_by_id` (FK) / `operated_by` — resolve via `operator_aliases` (`OperatedByAliases` query param or `OperatedByID`).

Return `(ownerDisplay, ownerSlug)`; empty slug ⇒ render the label without a link (same convention as `agentRowView.OperatedBySlug`). Reuse the existing slug resolver (`operator_slug_cache.go: resolveOperatorSlug` / `populateOperatedBySlug`) so the owner chip matches `/agents` and `/u` styling.

**Surface owner on the gardener page:** the header's owner chip links to `/u/{ownerSlug}` (via the existing `{{userLink}}` template func used by agent rows).

**List a user's gardeners on `/u/{slug}`:** add a sibling to "Their automations". The cleanest filter is by **label + ownership**, since gardeners are `type=trigger` + `role:gardener`:
```go
theirGardeners = s.runUserHomeSection(ctx, sectionTheirGardeners, "Their gardeners",
	beadsapi.ListBeadsQuery{
		OperatedByID: operatorID,
		Types:        []string{"trigger"},
		Labels:       []string{"role:gardener"},
		Statuses:     []string{"open", "in_progress"},
		Limit:        userHomeListLimit,
	}, slug, "No gardeners.", "trigger", nil)
```
Each row links to `/gardener/{id}`. (Ownership here keys on `operated_by_id`; the backfill in §7 ensures legacy gardeners have it. For gardeners owned via `UserBeadID`/`SlackUserID` only, the backfill also writes `operated_by_id` so this one filter suffices.)

---

## 7. Decision 6 — Backfill plan

**Child:** ka-0000008108 (Backfill owner attribution).

**Precedent:** `kd admin backfill user-bead-id` (`kbeads/cmd/kd/admin_backfill_user_bead_id.go`) already implements the exact shape we need — a cobra subcommand under `kd admin backfill`, a pure `plan()` step, idempotent `UpdateBead`/field merge, and a `--dry-run` flag (lines 38-60, 88-210). Model the new command on it directly rather than inventing a `gb` command.

**Command:** `kd admin backfill gardener-attribution [--dry-run] [--limit N]`

**Phase A — gardener trigger beads.** List `type=trigger` + `role:gardener`. For each whose `gardener` JSON lacks both `user_bead_id` and `slack_user_id`:
1. Resolve owner from the **trigger bead's** `operated_by_id` (FK) or `operated_by` / `operator_aliases` (`kind:subject`, via `LookupOperatorByAlias`).
2. From the resolved operator, derive `slack_user_id` (operator's slack subject) and `user_bead_id` (operator's user bead).
3. Decode `gardener` JSON, set `SlackUserID`/`UserBeadID`, re-encode, write back via the field-merge update. Also ensure the trigger bead's `operated_by_id` is set (so Decision 5's `/u` filter works).
4. **Idempotency guard:** skip if `gardener.user_bead_id`/`slack_user_id` already non-empty (and `operated_by_id` already set). The merge update is a no-op anyway, but the guard makes the plan/report accurate.

**Phase B — historical agent iterations.** List `type=agent` with `schedule_id` present (uses the new index from §3.1) but missing `slack_user_id`/`user_bead_id`. For each:
1. Group by `schedule_id`; look up the gardener trigger once per group.
2. Inherit the owner from the (now-backfilled) gardener: stamp `slack_user_id` + `user_bead_id` onto the iteration to match `stampGardenerLoopContext`'s field names.
3. **Idempotency guard:** skip iterations that already carry `slack_user_id`. Skip groups whose gardener trigger no longer exists (dangling `schedule_id`) — log and report, don't fail.

**Dry-run output** (tab-separated, no writes):
```text
PHASE  BEAD_ID     FIELD          OLD        NEW        STATUS
A      ka-abc…     slack_user_id  (unset)    U07K1RLQ   WOULD_UPDATE
A      ka-def…     slack_user_id  U07K1RLQ   U07K1RLQ   SKIP_IDEMPOTENT
B      ka-ghi…     slack_user_id  (unset)    U07K1RLQ   WOULD_UPDATE
B      ka-jkl…     —              —          —          SKIP_NO_GARDENER
```
Exit non-zero if any `ERROR` rows (unresolvable operator, malformed `gardener` JSON). Re-running after an apply produces an all-`SKIP_IDEMPOTENT` plan — that is the idempotency acceptance test.

---

## 8. Decision 7 — Live updates (RECOMMEND: extend agents SSE filter)

**Child:** ka-0000000633 (Live updates).

**Recommendation: extend the agents SSE filter to also match `schedule_id`.** The agents SSE stream (`cmd/beads-viewer/agents_events.go`) already subscribes to bead events, filters `type=agent` + optional `?project=`, and pushes rendered agent rows. Adding one predicate gives the gardener page a live thread for free, with no per-gardener polling allowlist entry and no fixed interval.

```go
// handleAgentsEvents: read the new filter alongside projectFilter.
scheduleIDFilter := strings.TrimSpace(r.URL.Query().Get("schedule_id"))

// writeAgentsIndexEvent: after the project check (~line 91-98), add:
if scheduleIDFilter != "" && fields["schedule_id"] != scheduleIDFilter {
	return
}
```
The gardener page opens `EventSource('/agents/events?schedule_id=' + gardenerID)` and reuses the same client-side row-insert handler `/agents` uses. New/updated/closed iterations stream in as they happen.

**Why not `liveAttrs` polling?** It requires a slug allowlist entry (`widgets_live.go:43-48`) + a new template polling func + a fixed interval, and it re-fetches the whole section each tick. The SSE path is one server-side conditional, event-driven, and has no artificial cap (unlike the constellation's 6-child limit). The only cost is a tiny bit of client JS, which already exists for `/agents`.

**Caveat:** iterations spawned outside the gardener loop (manual agents) won't carry `schedule_id` and correctly won't stream — that's the desired behavior.

---

## 9. Decision 8 — Trigger page relationship (ADD, don't replace)

**Child:** ka-0000002277 (Cross-links).

**Confirmed: `/triggers` stays exactly as-is.** We only ADD two cross-links.

**(a) Trigger row → gardener page.** `triggerRowView` (`triggers.go:66-139`) already has `Role` (populated from `fields["role"]`). The gardener's own page is keyed by the trigger bead ID, which *is* the row's bead ID — so no new field is needed: link `role == "gardener"` rows to `/gardener/{.ID}`. In `trigger_row.html`, replace the plain role span with a conditional link:
```html
{{if eq .Role "gardener"}}
  <a class="trig-meta-bit" href="{{$.BasePath}}/gardener/{{.ID}}">gardener →</a>
{{else if .Role}}
  <span class="trig-meta-bit">{{.Role}}</span>
{{end}}
```
(Use the row's existing bead `ID`; do not invent a `ScheduleID` field — for a gardener the trigger bead ID and the stamped `schedule_id` are the same value.)

**(b) Agent card → its gardener.** Expose `schedule_id` to the agent detail/card context (add `ScheduleID string` to `agentRowView`, populated in `agentRowFromBeadCtx` from `b.Fields["schedule_id"]`), and in the agent card template render a "gardener" link when set:
```html
{{if .ScheduleID}}<a class="agent-meta" href="{{$.BasePath}}/gardener/{{.ScheduleID}}">gardener</a>{{end}}
```
This also gives the iteration cards on the gardener page a self-link back to the gardener (harmless/consistent).

**Dependency note:** both links target `/gardener/{id}` (Decision 3). If the cross-link child lands before the route child, the links 404 until the route exists — a transparent dead-end, not a breakage. Sequence the route child (ka-0000001207) before or with the cross-link child (ka-0000002277).

---

## 10. Implementation children — mapping & sequencing

| Open question | Decision | Child bead | Dep notes |
|---|---|---|---|
| Q1 view model | §2 | ka-0000005461 | foundation; many children import the structs |
| Q2 iteration lookup | §3 | ka-0000000412 | index migration + client `Fields` param; unblocks the page + backfill Phase B |
| Q3 route + render | §4 | ka-0000001207 | depends on Q1 (structs) + Q2 (lookup) |
| Q4 ordering | §5 | ka-0000004816 | helper inside Q1/Q3; pure logic + tests |
| Q5 ownership | §6 | ka-0000002818 | depends on Q1; extends `/u/{slug}` |
| Q6 backfill | §7 | ka-0000008108 | depends on Q2 index (Phase B query); `kd admin` sibling |
| Q7 live updates | §8 | ka-0000000633 | independent; one SSE predicate |
| Q8 cross-links | §9 | ka-0000002277 | depends on Q3 (route target) |

**Suggested order:** Q1 → Q2 → (Q4, Q5 in parallel) → Q3 → (Q6, Q7, Q8 in parallel). Q2's index can land first standalone.

---

## 11. Risks & open notes

- **Field-name correction is load-bearing.** Everything keys on the real stamped names `schedule_id`, `slack_user_id`, `user_bead_id`, `schedule_prior_agent_id` (NOT `schedule_slack_user_id`). The epic/task copy is wrong on this one name; this doc is the source of truth. Update child descriptions accordingly (done — see §12).
- **`Stop`/`Progress` stay opaque.** Render pretty JSON; do not block the page on the B6 stop-spec parser (ka-0000001562).
- **MR/wrap-up resolution cost.** Each closed iteration's `WrapUpMRs` is a daemon round-trip. For very long threads, resolve MRs only for the eager (recent N) cards; the lazy older-runs section can render wrap-up text without MR pills, or resolve on reveal.
- **Chain drift.** The `created_at` fallback covers edited/deleted/restarted gardeners; surface the "(by time)" label so operators aren't misled by heuristic numbering.
- **Index footprint.** The partial `schedule_id` index only covers scheduler/gardener beads; small and bounded, consistent with migration 025's stance.
- **Legacy `type=schedule` gardeners.** Older gardeners may be `type=schedule` rather than `type=trigger`+`trigger:schedule`. The page route should accept both: try `GetBead`; if it's a `schedule` bead, read counters from its native fields rather than a `gardener` JSON blob. Most live gardeners are already `type=trigger`; treat `type=schedule` as a compatibility branch in Q3.

---

## 12. Acceptance check (this task)

- [x] Design doc bundle attached to epic ka-0000001579 (this file + `gb bundle`).
- [x] Each of the 8 open questions has a recorded decision (§2–§9).
- [x] Implementation children (ka-0000005461 … ka-0000002277) enriched with enough detail to start (§12 edits below).
- [x] Field-name correction (`slack_user_id`, not `schedule_slack_user_id`) recorded so children don't chase a non-existent field.
