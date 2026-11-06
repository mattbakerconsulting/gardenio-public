# Short titles (`short_title`) Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Every bead carries a `short_title`: a human display name of at most 40 characters that Slack prose, the References footer, beads-viewer lists and agent cards all show instead of the `gd-*` id or a 100-character title. Agents and in-repo creators author it at create time; a scheduled Haiku-class Claude Code groomer fills in any that are missing and backfills the existing beads; the daemon validates it and, after a warn window, refuses agent-created task/bug/feature/epic/decision/escalation beads that omit it.

**Owner direction (seed bead gd-V5nGseRS5f, thread gd-8Gxghn6hkt):**
- 2026-10-11 16:01Z: a new MANDATORY field with a character limit is acceptable. `short_title` required (<=40) for task/bug/feature/epic/decision/escalation, server-enforced; internal types (agent/session/message/bundle) derive it automatically and are not gated. Two-step rollout: (1) accept-missing + generate + warn per creator; (2) flip to reject once every in-repo creator supplies it. One-time backfill of open beads.
- 16:07Z: the name is "short title" (field `short_title`). REQUIREMENT: `kd create --short-title` help text, the daemon's missing-field error and the agent advice line must all explain the purpose (the name humans see for this bead in Slack prose and the References footer, in beads-viewer lists and agent cards) and show a rendered example, e.g. `🌱 forward duration_ms through gb hook relay`.
- 16:40Z (REVISES item 3 of the bead body): **no LLM client in kbeads.** Fleet model access is OAuth tokens, not a daemon-usable API key. Generation and backfill are a scheduled Claude Code agent (trigger bead, Haiku-class model via the normal agent path): each run lists beads with empty `short_title` (task/bug/feature/epic/decision/escalation, open first, then recent closed), writes <=40-char titles with `kd update --short-title` in batches of ~50, idempotent, never overwrites an authored value. The daemon keeps only column + validation + kd flag + warn-then-reject.
- 2026-10-14 03:43Z (laptop-claude): display side is done. PR #229 merged (`bead_display_name.go` prefers `fields.short_title`, then heuristics); #280 (bounded cache, Block Kit limits, `linkHydrate: true` in k3s) goes out in the next roll.
- 2026-10-14 ~04:20Z (operator, Slack): "we use that instead of the gd-* bead reference for text, so we don't have any more gd-* nonsense in chat." The short title IS the link text; the id lives only in the link URL.

**Architecture:**
- **kbeads** grows a nullable `short_title` column (migration 052), a 40-rune validator, `short_title` on the HTTP/batch/gRPC bead shape, a `short_title_missing=true` list filter, an `only_if_short_title_empty` update guard, and a staged gate `BEADS_SHORT_TITLE_GATE=off|warn|enforce` modelled on `BEADS_AUTHORING_HUMAN_GATE` (`kbeads/internal/server/authoring_human_gate.go`). Warnings reach the caller through a new `X-Beads-Warning` response header.
- **kd** gets `--short-title` on create and update, prints `Short title:` in `kd show`, a `SHORT` column in `kd list`, a `--missing-short-title` list filter, and echoes `X-Beads-Warning` to stderr.
- **controller** (`beadsapi`) carries `short_title` on `beadJSON`/`BeadDetail`/`CreateBeadRequest`; the bridge resolver prefers the column, then the legacy fields entry, then heuristics. A new `internal/shorttitle` package owns the clip + de-noise rules (lifted from the bridge), and the single `beadsapi.CreateBeadDetail` choke point fills the field for every in-repo creator, with better deterministic values for schedule runs (`<trigger title> HH:MM`), agent beads (handle) and plan epics (`plan: <goal>`).
- **groomer**: a `type=role` bead pinned to `anthropic/claude-haiku-4-5` and a `type=trigger` / `trigger:schedule` bead (cron, `action=spawn_agent`, `bead_query` pre-flight on `short_title_missing`) whose prompt is the generation + backfill playbook. No daemon code calls a model.
- **beads-viewer** list rows and cards show `short_title` when present.

**Tech stack:** Go monorepo. kbeads (`kbeads/`, Postgres migrations via golang-migrate, protoc via `make proto`), controller (`gardenio/controller/`), Helm chart `gardenio/helm/gardenio` (chart tests are a separate module: `make test-helm`). Agent pods have a 2Gi limit: run package-scoped `go test ./internal/<pkg>/`, never `go test ./...` at a module root (advice gd-QLhMjEsBsR).

**Plan bead:** gd-YZATAIOhfW (labels `plan`, `project:gardenio`, `plan:short-titles`; `plan_state=ready`; related to thread gd-8Gxghn6hkt and seed gd-V5nGseRS5f). Flat plan, no phases. Captain bound but DISABLED until the owner approves the build.

**Step beads (minted 2026-10-14 04:00Z, bodies are the task sections below verbatim):**

| Task | Bead | Blocked by |
|---|---|---|
| 1 kbeads column/validation/HTTP | gd-5zUHp6GobN | — |
| 2 kbeads proto/gRPC | gd-WSb4yxXFI6 | 1 |
| 3 kd flags/show/list | gd-6Z98We2bq9 | 1 |
| 4 gate warn→enforce + chart value | gd-ADoKjJ5149 | 1 |
| 5 controller beadsapi + shorttitle + bridge | gd-iwFpVSCo4i | — |
| 6 every in-repo creator supplies it | gd-QypEmywEjJ | 5 |
| 7 beads-viewer | gd-1DvGBBCE31 | 5 |
| 8 advice + preamble + docs | gd-tBIIJcbHas | 3 |
| 9 groomer role/trigger/pre-flight/prompt | gd-F3n1hBqhCt | 3, 5 |
| 10 backfill + enforce flip | gd-wNjWuZQK5Q | 4, 6, 9 |

**Anchors:** verified against `origin/main` at `4dfdad92` (2026-10-14, "fix(kbeads): machine-gate row for agent_identity_authz.go"). Lines drift as PRs land. If a cited line has moved, find it by the quoted text and say so in the PR.

**Live facts (read-only, 2026-10-14 ~04:10Z, `kd list --all-projects` totals):**

| type | open+in_progress | closed |
|---|---|---|
| task | 373 | 904 |
| bug | 77 | 53 |
| feature | 3 | 1 |
| epic | 89 | 52 |
| decision | 1 | 162 |
| escalation | 16 | 308 |

So the open backfill is ~560 beads (~12 groomer runs at 50/run); closed is ~1480 more. No `short_title` exists anywhere in kbeads today (Go, SQL, proto, docs). The fleet's Haiku route is the LiteLLM group `anthropic/claude-haiku-4-5` (`gardenio/helm/gardenio/values.yaml:4762`, `defaultHaikuModel`). Role beads pin a model through `fields.model` (e.g. `crew-fable` gd-czleroHlgq = `claude-fable-5-1-oauth`; `gardenio/controller/internal/beadsapi/roles.go:25`). Scheduled triggers look like the cicd watchdog gd-OxGR1jvjZ9: labels `trigger:schedule`, `role:<role>`, `project:gardenio`; fields `action=spawn_agent`, `cron`, `role`, `allowed_roles`, `allowed_projects`, `prompt`, `wake_intent`, `max_concurrent=1`, `cooldown_minutes`, `timezone`, `enabled=true` (JSON bool), optional `conditions` (JSON array of `bead_query` leaves).

## Design decisions (settled here; reopen only via `gb decision create`)

- **D1 — a column, not `slug`.** `beads.short_title TEXT NULL`. `slug` stays reserved for a URL-safe handle (`kbeads/docs/design-bead-references.md:237-239`). Limit is **40 runes** (not bytes), after trimming and collapsing whitespace; newlines are rejected. Empty string means "unset" and marshals as absent (`omitempty`).
- **D2 — the gate.** Env `BEADS_SHORT_TITLE_GATE` with values `off` / `warn` / `enforce`, parsed exactly like `authoringHumanGateModeFromEnv` (`kbeads/internal/server/authoring_human_gate.go:50-58`; a typo is `off`). **Code default is `warn`** so the observation window starts the moment the daemon rolls, with no chart change. Gated types: `task`, `bug`, `feature`, `epic`, `decision`, `escalation` with empty `short_title` at create. In `warn` every such create records the metric and returns an `X-Beads-Warning`. In `enforce` the create is **refused only for non-human callers** (`AuthIdentity.Type` in `static`, `fleet`, `machine`: agent pods, bridges, services — `kbeads/internal/server/audit.go:150-170`); `oidc` and `edge` callers (a human at kd or the viewer) still only get the warning, because the groomer fills what a human skips and the operator asked for agents to be held to it, not people. The chart exposes `beads.shortTitleGate` (env on the kbeads Deployment); the flip to `enforce` is a deploy-values change after Task 10's audit.
- **D3 — the warning channel.** The create path has no warning mechanism today (only `X-Kbeads-Version`/`X-Kbeads-Commit` headers, `kbeads/internal/server/http.go:377-378`). Add `X-Beads-Warning` (one header value per warning) set by the HTTP create handler; `kd` prints each as `warning: …` on stderr. gRPC callers get a `slog.Warn` only.
- **D4 — one text, three places.** A single exported constant `model.ShortTitleHelp` is the help, the error and the advice line:

  ```
  short_title is the name humans see for this bead: in Slack prose and the References footer, in beads-viewer lists and on agent cards. Up to 40 characters, imperative, no ids, timestamps, type words or prefixes. Rendered example: 🌱 forward duration_ms via gb hook relay (the glyph is added at render time; store only the words).
  ```

  The example is `model.ShortTitleExample` (37 runes). It was first drafted as "forward duration_ms through gb hook relay", which is 41 runes and fails the 40-rune limit; tests assert the constant, not a literal.

- **D5 — the groomer owns generation.** No daemon background job. The trigger fires only when `bead_query{short_title_missing:true} > 0`, so a quiet fleet costs nothing. Each run: open beads first, then closed beads updated in the last 30 days; 50 per run; `kd update --short-title … --only-if-short-title-empty` so a concurrent author always wins. The groomer never edits `title`.
- **D6 — deterministic fills at one choke point.** Every controller/gb/viewer/bridge create passes through `beadsapi.CreateBeadDetail` (`gardenio/controller/internal/beadsapi/client.go:1129`); a gated type with no `ShortTitle` gets `shorttitle.Derive(type, title)` there, so the enforce flip cannot strand an in-repo creator. Targeted overrides give the known-good shapes: schedule runs `<trigger title> HH:MM` (what the bridge already derives, `bead_display_name.go:91-99`), agent beads the handle, plan epics `plan: <goal>`. Escalations and decisions come out of `Derive` as the `<ref>: …` prefix (`Merge PR #295`) or the clipped title/question. Everything clips at a word boundary with a trailing `…` only when something was cut. `title` is never edited. The trade-off is explicit: a machine-created bead keeps a de-noised title as its short title and the groomer will not overwrite it; the groomer's Haiku quality goes to `kd create` beads where an agent forgot the flag, and to the backfill.
- **D7 — bridge compatibility.** `beadDisplayName` prefers `bead.ShortTitle`, then `bead.Fields["short_title"]` (what PR #229 reads, so any value written before the column rolls keeps working), then heuristics. The fields path is dead on task/bug/feature/epic today anyway: their schemas reject unknown keys (`kbeads/internal/model/validate_fields.go:114-126`), so nothing is lost by making the column canonical.

## Global Constraints

1. **No model calls from kbeads or the controller.** Generation happens in an agent pod. A PR that adds an LLM client to either module is out of scope and will be sent back.
2. **Merge gates.** kbeads and controller PRs: normal crew PR, self-merge on green is NOT allowed for a daemon change; file `gb escalation create --operation-type github_pr_merge` as usual. A chart change (`gardenio/helm/gardenio/**`, Task 4's values key) and a deploy-values change (Task 10's flip) are privileged lanes: open as DRAFT with label `merge-desk:hold` and file a `merge_window` escalation (advice gd-3wArIbnnWn).
3. **Never overwrite an authored `short_title`.** The groomer uses the only-if-empty guard; deterministic fills only set the field on create; `kd update --short-title` is the one path that replaces, and it is a human/author action.
4. **Memory.** Run `go test ./internal/server/ -run <Name>` style, package-scoped. `make test` at the repo root OOM-kills the pod.
5. **Migration policy** (`kbeads/CLAUDE.md`, "Migration policy"): idempotent DDL (`IF NOT EXISTS`), no eager backfill, paired down file.
6. **Commit style:** `feat(kbeads): …`, `feat(gardenio/bridge): …`, `feat(gardenio/gb): …`. Co-author line per the session attribution.
7. **Release.** kbeads and controller land through `release-cut.sh` + `helm upgrade` by the operator. Tasks 9 and 10 need the daemon with Task 1 live; say so in their escalation context.

## Dependency line

- Task 1 (kbeads column) unblocks Task 2 (proto/gRPC), Task 3 (kd) and Task 4 (gate).
- Task 3 unblocks Task 8 (advice + docs) and Task 9 (groomer).
- Task 5 (controller beadsapi + bridge) starts in parallel with Task 1 and unblocks Task 6 (creator fills), Task 7 (viewer) and Task 9.
- Task 10 (backfill + enforce flip) waits on Task 4, Task 6 and Task 9.

Blocks edges at mint time: T2←T1, T3←T1, T4←T1, T6←T5, T7←T5, T8←T3, T9←T3, T9←T5, T10←T4, T10←T6, T10←T9. T1 and T5 start immediately and in parallel.

---

### Task 1: kbeads — column, model, store, validation, HTTP and batch exposure

**Files:**
- Create: `kbeads/internal/store/postgres/migrations/052_beads_short_title.up.sql`, `…/052_beads_short_title.down.sql`
- Modify: `kbeads/internal/model/bead.go:225-232` (struct), `kbeads/internal/model/validate.go:36-45` (ValidateBead), `kbeads/internal/store/postgres/queries.go:29-31,38-40,292-334,1436-1471`, `kbeads/internal/store/postgres/queries_embedding.go:318-321,413-415`, `kbeads/internal/store/postgres/scan.go` (all five scanners: :19, :106, :200, :298, :403), `kbeads/internal/server/beads.go:25-89,93-96,935-953,1067-1069,1132-1136,1146-1151,1314-1328`, `kbeads/internal/server/http_beads.go:285-320` (list filter), `kbeads/internal/server/undo.go:116-133`, `kbeads/internal/store/postgres/postgres_test.go:48,69,80` (sqlmock column lists)
- Test: `kbeads/internal/model/validate_test.go` (beside `TestValidate_TitleTooLong` :62), `kbeads/internal/server/beads_short_title_test.go` (new), `kbeads/internal/store/postgres/migration_052_integration_test.go` (new, `//go:build integration`)

**Interfaces:**
- Produces `model.Bead.ShortTitle string \`json:"short_title,omitempty"\`` (after `Slug`, :227).
- Produces `model.ShortTitleMax = 40` and `model.ShortTitleHelp` (D4) in `kbeads/internal/model/validate.go`.
- Produces `createBeadInput.ShortTitle string \`json:"short_title"\`` and `updateBeadInput.ShortTitle *string \`json:"short_title"\`` plus `OnlyIfShortTitleEmpty bool \`json:"only_if_short_title_empty"\``.
- Produces list filter `short_title_missing=true` (HTTP query param → `store.ListBeadsFilter.ShortTitleMissing`), SQL `(short_title IS NULL OR short_title = '')`.

- [ ] **Step 1: Migration 052**

`052_beads_short_title.up.sql`:
```sql
-- short_title: the <=40-char human display name (plan 2026-10-14-short-titles, gd-V5nGseRS5f).
-- Nullable; no backfill here — the groomer agent fills it. Length is enforced in Go (runes).
ALTER TABLE beads ADD COLUMN IF NOT EXISTS short_title TEXT;
```
`052_beads_short_title.down.sql`:
```sql
ALTER TABLE beads DROP COLUMN IF EXISTS short_title;
```
Pattern: `039_beads_search_tsv.up.sql:48`. Integration test beside `migration_0NN_integration_test.go`: up, insert a bead with `short_title`, read it back, down.

- [ ] **Step 2: Model + validation (test first)**

In `validate_test.go` add `TestValidate_ShortTitleTooLong` (41 runes → `short_title: must be 40 characters or fewer`), `TestValidate_ShortTitleExactly40` (40 multibyte runes pass), `TestValidate_ShortTitleNewlineRejected`. Run `go test ./internal/model/ -run ShortTitle` → FAIL. Then add to `ValidateBead` beside the title rule at `validate.go:39-44`:
```go
// ShortTitle: optional, at most ShortTitleMax runes, single line.
if st := b.ShortTitle; st != "" {
	if len([]rune(st)) > ShortTitleMax {
		ve.Errors = append(ve.Errors, FieldError{Field: "short_title", Message: "must be 40 characters or fewer"})
	}
	if strings.ContainsAny(st, "\r\n") {
		ve.Errors = append(ve.Errors, FieldError{Field: "short_title", Message: "must be a single line"})
	}
}
```
Normalise once in `createBead`/`updateBead` (`strings.Join(strings.Fields(st), " ")`) before validation so the error is about length, not stray spaces.

- [ ] **Step 3: Store**

Add `short_title` to `beadColumns`/`beadColumnsBeads` (`queries.go:29-31,38-40`) and the two hand-written lists in `queries_embedding.go`; add `nullString(b.ShortTitle)` to `queryCreateBead` (:301-304, :311) and a `short_title = $N` to `queryUpdateBead` (:1436-1471); add a `sql.NullString` scan slot after `slug` in all five scanners in `scan.go`. Fix the sqlmock column lists (`postgres_test.go:48,69,80`, `queries_embedding_test.go`, `subfield_edits_test.go:18`, `scrub_taint_test.go:16`). `go test ./internal/store/postgres/` must be green before moving on (it is a sqlmock suite, cheap).

- [ ] **Step 4: Server inputs, create and update**

`createBeadInput` (`beads.go:25-89`): add `ShortTitle`. In `createBead` after the title check (:94-96) normalise and copy into the `model.Bead` literal (:255+). `updateBeadInput` (:935-953): add `ShortTitle *string` and `OnlyIfShortTitleEmpty bool`. In `updateBead` beside the title branch (:1314-1328):
```go
if in.ShortTitle != nil {
	if in.OnlyIfShortTitleEmpty && strings.TrimSpace(bead.ShortTitle) != "" {
		// Groomer guard: an authored value always wins (plan D5).
	} else {
		oldValues["short_title"] = bead.ShortTitle
		bead.ShortTitle = normalizeShortTitle(*in.ShortTitle)
		changes["short_title"] = bead.ShortTitle
	}
}
```
Teach `patchOnlyChangesFields` (:1067-1069), `hasAnyChange` (:1132-1136), `patchOnlyChangesAssignee` (:1146-1151) and the undo whitelist (`undo.go:116-133`) about the new pointer. The HTTP `handleUpdateBead` (`http_beads.go:837-846`) needs no set-flag: a nil pointer is "unchanged".

- [ ] **Step 5: List filter**

`http_beads.go` beside `no_open_deps` (:299): `if q.Get("short_title_missing") == "true" { filter.ShortTitleMissing = true }`. Store: add the predicate where `NoParent`/`ExcludeScheduled` are applied in the list query builder. Test: `TestListBeads_ShortTitleMissing` in `beads_short_title_test.go` using `newTestServer` (`http_test.go:3513`).

- [ ] **Step 6: Server tests**

`beads_short_title_test.go`: create with a 40-rune value round-trips through `GET /v1/beads/{id}` and `GET /v1/batch/beads` (`http_batch.go:43-124` needs no change: it marshals `*model.Bead`); 41 runes → 400 `invalid bead: validation failed: short_title: must be 40 characters or fewer`; update with `only_if_short_title_empty` leaves an authored value alone and fills an empty one. Run `go test ./internal/server/ -run ShortTitle`.

- [ ] **Step 7: Docs + commit**

Add `short_title` to the bead field table in `kbeads/CLAUDE.md`. Commit: `feat(kbeads): short_title column, 40-rune validation, HTTP/batch exposure, missing filter (gd-V5nGseRS5f)`. Open the PR; escalate the merge.

---

### Task 2: kbeads — proto and gRPC round-trip

**Files:**
- Modify: `kbeads/proto/beads/v1/types.proto:8-55` (Bead: `string short_title = 27;`), `kbeads/proto/beads/v1/beads.proto:10-39` (CreateBeadRequest: `string short_title = 16;`), `:145-158` (UpdateBeadRequest: `optional string short_title = 13; optional bool only_if_short_title_empty = 14;`), `:56-` (ListBeadsRequest: `bool short_title_missing = 27;`), `kbeads/internal/server/convert.go:16-64` (`beadToProto`, beside `Slug: b.Slug` :23), `kbeads/internal/server/beads.go:799-853` (gRPC CreateBead), `:1887-1931` (gRPC UpdateBead), `kbeads/internal/client/grpc.go:113-141,275-317,935-` (client mapping)
- Regenerate: `make proto` (`kbeads/Makefile:28-29` → `scripts/gen-proto.sh`; plugin versions must match `go.mod`)
- Test: `kbeads/internal/server/grpc_test.go` (beside the existing CreateBead cases)

- [ ] **Step 1:** Add the four proto fields with the numbers above (27 is the first free Bead number; 16, 13/14, 27 are the next free in their messages; no `reserved` blocks exist). Run `make proto` from `kbeads/`; commit the generated `gen/beads/v1/*.pb.go` together with the `.proto`.
- [ ] **Step 2:** Wire `beadToProto`, the gRPC `CreateBead`/`UpdateBead` handlers (copy the optional exactly like `if req.Title != nil { in.Title = req.Title }`), `ListBeads` filter, and the client (`protoBeadToModel` at `grpc.go:941`, `CreateBead`/`UpdateBead` request mapping). Note `SawOpenHash` skipped the proto with a "protoc v6 regen required" comment (`beads.go:86-87`): if `make proto` fails on plugin skew, fix the toolchain in this PR rather than skipping — `kd --transport grpc` must carry the field.
- [ ] **Step 3:** gRPC test: create with `short_title`, get, assert; 41 runes → `codes.InvalidArgument`. `go test ./internal/server/ -run 'GRPC.*ShortTitle'`.
- [ ] **Step 4:** Commit `feat(kbeads): short_title on the gRPC surface (gd-V5nGseRS5f)`.

---

### Task 3: kd — `--short-title` on create/update, show/list, missing filter, warning echo

**Files:**
- Modify: `kbeads/cmd/kd/create.go:145-161,265-280,352-373`, `kbeads/cmd/kd/update.go:63-70,140-142,234-253`, `kbeads/cmd/kd/output.go:46-49,207-260,388-398`, `kbeads/cmd/kd/list.go:118,373`, `kbeads/internal/client/client.go:310-351,501-513`, `kbeads/internal/client/http.go` (response header read, near :1663 where `X-Actor` is sent)
- Test: `kbeads/cmd/kd/create_short_title_test.go` (new, beside `create_idempotency_test.go`), `kbeads/cmd/kd/update_test.go`, `kbeads/cmd/kd/list_json_description_test.go` pattern for the list column

**Interfaces:**
- `kd create --short-title "<text>"`; help text = `model.ShortTitleHelp` (D4).
- `kd update <id> --short-title "<text>" [--only-if-short-title-empty]`.
- `kd list --missing-short-title` → `short_title_missing=true`.
- `kd show` prints `Short title: …` directly under `Title:`; `kd list` gains a `SHORT` column (40 wide) between `TYPE` and `TITLE`; the JSON shapes pick the field up from `model.Bead` unchanged.

- [ ] **Step 1 (test first):** `TestCreate_ShortTitleFlagSent` using the existing fake-server pattern in `create_idempotency_test.go`: assert the POST body has `"short_title":"forward duration_ms through gb hook relay"`. `TestUpdate_ShortTitleOnlyIfEmpty` asserts `only_if_short_title_empty:true`. `TestCreate_WarningHeaderEchoed`: a fake server that answers 201 with `X-Beads-Warning: short_title missing: …` → stderr contains `warning: short_title missing`. Run → FAIL.
- [ ] **Step 2:** Flags: `createCmd.Flags().String("short-title", "", model.ShortTitleHelp)` at :352-373; read at :145-161; set `CreateBeadRequest.ShortTitle` (:265-280). Update: `--short-title` + `--only-if-short-title-empty` at :234-253, `Changed()` wiring at :63-70, `reqHasChange` at :140-142. Client structs: `ShortTitle string \`json:"short_title,omitempty"\`` on `CreateBeadRequest`, `ShortTitle *string` + `OnlyIfShortTitleEmpty bool` on `UpdateBeadRequest`.
- [ ] **Step 3:** HTTP client: after a successful create/update response, read `resp.Header.Values("X-Beads-Warning")` and return them on the result (a `Warnings []string` on the create return, or a package-level hook the commands drain); `kd create`/`kd update` print each as `warning: %s` to stderr. Keep stdout unchanged so `--json` consumers are unaffected.
- [ ] **Step 4:** Output: `printBeadTable` (`output.go:48-49`) adds `Short title:` after `Title:` when non-empty; `printBeadTableFiltered` rows (:388-398) add `{"Short title", "short_title", bead.ShortTitle}`; `printBeadListTable` header (:228) becomes `ID\tSTATUS\tTYPE\tSHORT\tPRIORITY\tTITLE\tASSIGNEE`, the title cut (:234-236) stays at 50. `kd list --missing-short-title` maps to `ShortTitleMissing` beside `NoOpenDeps` (`list.go:373`).
- [ ] **Step 5:** `go test ./cmd/kd/ -run 'ShortTitle|Warning'`. Commit `feat(kd): --short-title on create/update, show/list columns, --missing-short-title (gd-V5nGseRS5f)`.

---

### Task 4: kbeads — `BEADS_SHORT_TITLE_GATE` warn → enforce, metric, warning header, chart value

**Files:**
- Create: `kbeads/internal/server/short_title_gate.go`, `kbeads/internal/server/short_title_gate_test.go`
- Modify: `kbeads/internal/server/beads.go:178-193` (apply the gate right after the authoring gate), `kbeads/internal/server/http_beads.go:49-112` (`handleCreateBead`: set `X-Beads-Warning`), `kbeads/internal/server/operated_by_metrics.go:159-165,236-251` (metric beside `gardenio_beads_authoring_human_gate_total`), `kbeads/CLAUDE.md` (Configuration table), `gardenio/helm/gardenio/values.yaml` + the kbeads Deployment template (new `beads.shortTitleGate` → env `BEADS_SHORT_TITLE_GATE`), `gardenio/helm/gardenio/tests` (render assertion)

**Interfaces:**
- `shortTitleGateModeFromEnv() shortTitleGateMode` (`off`/`warn`/`enforce`; default **warn**; typo → off is NOT the default here — an unset env means warn, an unrecognised value means off, both documented).
- `isShortTitleGatedType(t model.BeadType) bool` → task, bug, feature, epic, decision, escalation.
- `isNonHumanCaller(id AuthIdentity) bool` → `Type` in static/fleet/machine (`AuthIdentityFromContext`, `kbeads/internal/server/audit.go:181`).
- Metric `gardenio_beads_short_title_gate_total{bead_type, caller, action}` with `action` in `supplied` / `allowed_warn` / `refused`, `caller` in `human` / `agent`.
- `createBead` returns warnings: change its signature to `(*model.Bead, []string, error)` OR stash them on the context via a small `warningsFromContext` helper set by the HTTP handler. Prefer the context helper: three call sites (gRPC, HTTP, escalation replay) stay untouched.

- [ ] **Step 1 (tests first, `short_title_gate_test.go`, pattern `authoring_human_gate_test.go:49-115`):** mode parse table (unset→warn, `enforce`/`1`/`true`→enforce, `off`→off, `bogus`→off); `TestCreateBead_ShortTitleGate_WarnAddsHeader` (static identity, type task, no short_title → 201 + `X-Beads-Warning` starting `short_title missing:` and containing the rendered example); `TestCreateBead_ShortTitleGate_EnforceRefusesAgent` (static/fleet/machine → 400 whose body is `model.ShortTitleHelp` prefixed by `short_title is required for type task:`); `TestCreateBead_ShortTitleGate_EnforceWarnsHuman` (oidc identity → 201 + header); `TestCreateBead_ShortTitleGate_InternalTypesUntouched` (agent/session/message/bundle/trigger → no header, no metric). Use `newTestServer` and the identity helpers in `operated_by_test.go:384`.
- [ ] **Step 2:** Implement `short_title_gate.go` mirroring `authoring_human_gate.go`; apply in `createBead` immediately after the authoring gate block (:178-193):
```go
if mode := shortTitleGateModeFromEnv(); mode != shortTitleGateOff && isShortTitleGatedType(beadType) && strings.TrimSpace(in.ShortTitle) == "" {
	caller := "human"
	if isNonHumanCaller(AuthIdentityFromContext(ctx)) { caller = "agent" }
	if mode == shortTitleGateEnforce && caller == "agent" {
		recordShortTitleGate(string(beadType), caller, "refused")
		return nil, inputError("short_title is required for type " + string(beadType) + ": " + model.ShortTitleHelp)
	}
	recordShortTitleGate(string(beadType), caller, "allowed_warn")
	addWarning(ctx, "short_title missing: "+model.ShortTitleHelp)
	slog.Warn("short_title missing", "type", beadType, "actor", actorFromContext(ctx), "mode", mode.String())
}
```
and `recordShortTitleGate(type, caller, "supplied")` when the field is present, so the ratio is observable.
- [ ] **Step 3:** `handleCreateBead` (`http_beads.go:49-112`): wrap `ctx` with a warnings collector before calling `createBead`; after success, `for _, w := range collected { w.Header().Add("X-Beads-Warning", w) }` before `writeJSON` (:111). `writeJSON` writes headers on first write, so the `Add` must come first.
- [ ] **Step 4:** Chart: `beads.shortTitleGate: ""` in `values.yaml` (comment: empty = daemon default `warn`; set `enforce` after the Task 10 audit, `off` is the kill switch) rendered as `BEADS_SHORT_TITLE_GATE` on the kbeads container, beside the existing `BEADS_AUTHORING_HUMAN_GATE` env. Add a render assertion in `gardenio/helm/gardenio/tests`. Run `make test-helm` (separate module; ~5 min) and `helm lint gardenio/helm/gardenio/`. **This chart hunk is a privileged lane**: put it in its own PR, DRAFT + `merge-desk:hold`, with a `merge_window` escalation; the kbeads code PR merges independently (it is inert until the daemon rolls, and `warn` is zero-refusal).
- [ ] **Step 5:** `go test ./internal/server/ -run ShortTitleGate`. Commits: `feat(kbeads): BEADS_SHORT_TITLE_GATE warn/enforce with X-Beads-Warning (gd-V5nGseRS5f)` and `feat(chart): beads.shortTitleGate value (gd-V5nGseRS5f)`.

---

### Task 5: controller — `short_title` on beadsapi, a shared `shorttitle` package, bridge resolver prefers the column

**Files:**
- Create: `gardenio/controller/internal/shorttitle/shorttitle.go`, `…/shorttitle_test.go`
- Modify: `gardenio/controller/internal/beadsapi/client.go:1085-1112` (`CreateBeadRequest`: add `ShortTitle string \`json:"short_title,omitempty"\``), `:1417-1430` (`BeadDetail`: `ShortTitle` after `Slug` :1419), `:2928-2958` (`beadJSON`: `ShortTitle` after `Slug` :2930), `:3033-3036` (`toDetail` copies it beside slug), `gardenio/controller/internal/beadsapi/client_query.go:110` (`ShortTitleMissing bool` beside `NoOpenDeps`; `:240-241` sets `short_title_missing=true`), `:403` (`UpdateBeadRequest`: `ShortTitle *string`, `OnlyIfShortTitleEmpty bool`), `gardenio/controller/internal/bridge/bead_display_name.go:26-99` (resolver delegates its heuristics to `shorttitle`)
- Test: `gardenio/controller/internal/bridge/bead_display_name_test.go:92-103` (extend), `gardenio/controller/internal/beadsapi/*_test.go` (decode round-trip)

**Interfaces:**
- `shorttitle.Max = 40`.
- `shorttitle.Clip(s string, max int) string`: collapse whitespace, cut at the last word boundary that fits in `max` runes, append `…` only when something was cut; never more than `max` runes; empty in → empty out. (Lift `truncateWords`/`collapseSpaces` from `bead_display_name.go`.)
- `shorttitle.Derive(beadType, title string) string`: the de-noising rules that live in `heuristicDisplayName` / `scheduleRunDisplayName` today (`bead_display_name.go:91-134`: schedule-run `<name> HH:MM`, agent `@handle`, bracket tags, `P0:` ordinals, component prefixes, trailing parentheticals, `<ref>: …` prefix, clause split, trailing question), moved here verbatim with their tests, clipped to `Max`. One source of truth for "a short name from a title": the bridge's render-time fallback and Task 6's create-time fills call the same function.
- `beadDisplayName` order: `bead.ShortTitle` → `bead.Fields["short_title"]` → `shorttitle.Derive(bead.Type, bead.Title)` → id. The bridge keeps its own `displayNameMax = 42` bound on whatever it returns.

- [ ] **Step 1 (test first):** `shorttitle_test.go`: `Clip("forward duration_ms through gb hook relay", 40)` unchanged; a 60-rune sentence cuts at a word boundary and ends in `…` within 40 runes; multibyte runes count as one; `Clip("", 40) == ""`. Move every table case of `TestBeadDisplayName` that exercises a heuristic (`bead_display_name_test.go:14-130`) into `TestDerive`, unchanged expectations except the 42→40 bound. Bridge test: add `"column short_title wins over fields and heuristics"` (`BeadDetail{ShortTitle: "x", Fields: {"short_title": "y"}, Title: "P0: …"}` → `x`) beside the existing case at :92. beadsapi: decode `{"id":"gd-x","title":"t","short_title":"s"}` → `BeadDetail.ShortTitle == "s"`; `BatchGetBeads` carries it; `CreateBeadRequest{ShortTitle:"s"}` marshals `"short_title":"s"`. Run package tests → FAIL.
- [ ] **Step 2:** Implement the package; make `bead_display_name.go` a thin wrapper (column → fields → `shorttitle.Derive`). Update the comment at :28-31: the column is canonical, the fields read is the compatibility path for anything written before the roll. Add the struct fields and the `toDetail` copy.
- [ ] **Step 3:** Hydrator test: for a bead with `ShortTitle`, the rendered mrkdwn (`slack_link_hydrate.go:320` inline, `:415` footer) contains the short title and the `gd-*` id appears only inside the `<url|…>` link target. (Unresolved ids — daemon miss — still fall to the regex linkifier, `slack_linkify.go:96`; that is the documented residual of the operator's "no gd-* in chat" ask.)
- [ ] **Step 4:** `go test ./internal/shorttitle/ ./internal/bridge/ -run 'DisplayName|Hydrate|Clip|Derive'` and `go test ./internal/beadsapi/ -run ShortTitle`. Commit `feat(gardenio/bridge): shorttitle package; resolver prefers the short_title column (gd-V5nGseRS5f)`.

---

### Task 6: controller + gb — every in-repo creator supplies `short_title` (one choke point, targeted overrides)

**Files:**
- Modify: `gardenio/controller/internal/beadsapi/client.go:1129-1143` (`CreateBeadDetail`, the single create choke point every `CreateBead` / `SpawnAgent` / viewer / bridge / gb path funnels through — it already stamps `gardener_id` at :1138), `gardenio/controller/internal/bridge/triggers.go:4020-4067` (`createTaskBead`: schedule runs and the `create_bead` action), `gardenio/controller/internal/bridge/trigger_source_schedule.go:665,713` (`fireKey`/`Schedule: <trigger title>`), `gardenio/controller/internal/beadsapi/client.go:1320-1323` (`SpawnAgent` agent bead), `gardenio/controller/cmd/gb/plan_write.go:256-265` (`createPlanEpicDaemon`), `gardenio/controller/cmd/gb/escalation_create.go:263-268`, `gardenio/controller/cmd/gb/decision.go:261-271`, `gardenio/controller/internal/bridge/thread_bead.go:169-188` (Slack thread head, `type=task`)
- Test: `gardenio/controller/internal/beadsapi/client_test.go` (choke-point default), `gardenio/controller/internal/bridge/triggers_test.go`, `gardenio/controller/cmd/gb/plan_create_test.go:21`, escalation/decision create tests, `thread_bead_test.go`

**Interfaces:**
- **Choke-point default** in `CreateBeadDetail`, right after the gardener stamp (:1138): `if req.ShortTitle == "" && shorttitle.Gated(req.Type) { req.ShortTitle = shorttitle.Derive(req.Type, req.Title) }` where `Gated` = task, bug, feature, epic, decision, escalation (the Task 4 set) plus `agent` (handle). This alone makes every controller/gb/viewer creator compliant under `enforce`: thread heads (`thread_bead.go:180`, `thread_compose.go:272`), plan steps/phases (`plan_write.go:355-358,420-424`, `plans.go:4852`, `plan_edit.go:305`), JIRA/GitLab mention beads (`jira_poller.go:943`, `jira_mentions.go:665`, `gitlab_mentions.go:349`, `gitlab_mr_backlink.go:430`), `agent_start.go:166`, decisions, escalations. A caller that sets `ShortTitle` explicitly is never overridden.
- **Targeted overrides** where the derived form is known to be better than the de-noised title:
  - schedule runs (`createTaskBead`): `ShortTitle = shorttitle.Clip(trig.Title, 40-6) + " " + <HH:MM of m.Key>` — the `fireKey` is `2006-01-02T15:04`, so the clock is `m.Key[11:16]`; this is exactly what the bridge renders today from the title regex (`bead_display_name.go:37,91-99`), now authored instead of guessed.
  - agent beads (`SpawnAgent` :1320 and `Derive` for the direct `Type:"agent"` sites): the handle without `@`.
  - plan epics (`createPlanEpicDaemon` :256): `shorttitle.Clip("plan: "+goal, 40)` (owner's "plan: <name>" shape, seed bead item 2).
  - escalations (`escalation_create.go:263`): `Derive` already yields the `<ref>: …` prefix (`Merge PR #295`) or the clipped title; no override, but a test pins `Merge PR #295: first-admin hook brute-force flag (gd-DAy1iWvO4y…)` → `Merge PR #295`.
  - decisions (`decision.go:261`): `Derive` keeps a trailing question clause; a test pins one real title.
- Nothing here edits `Title`. Nothing here applies to `session`, `message`, `trigger`, `schedule`, `mail`, `bundle`, `config`: not gated, bridge heuristics render them.

- [ ] **Step 1 (tests first):** `TestCreateBeadDetail_DefaultsShortTitle` (fake server captures the POST body: gated type with empty `ShortTitle` → derived value present; explicit value untouched; `type=message` → absent). `TestCreateTaskBead_ScheduleRunShortTitle` (`m.Key="2026-10-11T15:07"`, trigger title `cicd watchdog: 20-min GitHub Actions stall` → `cicd watchdog: 20-min GitHub… 15:07`, ≤40 runes). Plan epic → `plan: …`. Agent → handle. Run → FAIL.
- [ ] **Step 2:** Implement the default and the overrides at the anchored lines.
- [ ] **Step 3:** Package-scoped `go test` for `internal/beadsapi`, `internal/bridge -run 'Trigger|Thread|ShortTitle'`, `cmd/gb -run 'Plan|Escalation|Decision'`. Commit `feat(gardenio): every in-repo creator supplies short_title; schedule runs, agents and plan epics fill it deterministically (gd-V5nGseRS5f)`.
- [ ] **Step 4 (live, after the roll):** a fresh cicd-watchdog run bead shows `Short title: cicd watchdog… HH:MM` in `kd show`; a fresh escalation shows its ref prefix; `gardenio_beads_short_title_gate_total{caller="agent",action="allowed_warn"}` stops growing from bridge/gb sources (the residual is `kd create` by agents, which Task 8's advice and Task 9's groomer cover).

---

### Task 7: beads-viewer — show `short_title` in list rows, cards and plan steps

**Files:**
- Modify: `gardenio/controller/cmd/beads-viewer/server.go:1114` (template `FuncMap`: add `displayTitle`), `views.go:317` (`row.RowTitle = b.Title` → keep, add `row.RowShortTitle = b.ShortTitle`), `bead_actions.go:300` (same), `project_bulk.go:271-275` (`projectViewFromBead`), `inbox_stream.go:248`, `plan_phases.go:578-580` (`planStepView`), `plans.go:2588` (`planCardStep`), templates `templates/view_show.html:41`, `templates/partials/active_row.html:9`, `templates/partials/project_row.html:23`, `templates/partials/inbox_row.html:13`, `templates/partials/plan_steps.html:185`, `templates/partials/plan_card.html:54,623,631`, `templates/partials/deck_card.html:37`, `templates/partials/cmdk_results.html:4`, `templates/search.html:117`
- Test: `cmd/beads-viewer/views_test.go` (or the nearest render test for each partial)

**Interfaces:**
- Row/step/card view structs gain `ShortTitle string`; templates render `{{if .ShortTitle}}<span title="{{.Title}}">{{.ShortTitle}}</span>{{else}}{{.Title}}{{end}}` through a shared partial or the `displayTitle` func so the full title stays available on hover and on the bead page header (`partials/title_block.html:15-17`, unchanged: the detail page shows the full title and a `Short title` line under it).

- [ ] **Step 1 (test first):** render the active-row partial for one bead with `ShortTitle` and one without; assert the short title is the visible text for the first (with the full title in `title=`) and the full title for the second. Same for a plan step row. Run → FAIL.
- [ ] **Step 2:** Add the struct fields, the func, and the template changes at the anchored sites. The bead page gets a `Short title` line in `title_block.html` under the title.
- [ ] **Step 3:** `go test ./cmd/beads-viewer/ -run 'Row|Card|PlanStep|ShortTitle'`. Commit `feat(beads-viewer): show short_title in lists, cards and plan steps (gd-V5nGseRS5f)`.

---

### Task 8: agent advice line, prime/preamble text, docs

**Files:**
- Modify: `gardenio/controller/internal/bridge/init_prompts.go:117` (global Essential Commands line: `kd create "..." --type=task|bug|feature|epic -l project:<name>` gains `--short-title "<=40 chars, the name humans see>"`), `:230` (thread-role variant). These are `config:claude-instructions:*` beads upserted by `init_ensure.go:78-120`, so the text goes live on the next bridge start.
- Create: `gardenio/docs/short-titles.md` (the field, the gate and its env/chart value, the warning header, the groomer, the rendered example, how to audit with the metric)
- Live: one global advice bead via `gb advice add` (`cmd/gb/advice.go:96`; no targeting label → `global` at :184-186)

**Interfaces:**
- Advice title `Supply --short-title when you create a bead`; body = `model.ShortTitleHelp` (D4) verbatim, then: "Under `BEADS_SHORT_TITLE_GATE=enforce` an agent create without it is refused; in `warn` you get `warning: short_title missing`. Example: `kd create "P0: forward duration_ms + tool_use_id through gb hook relay" --type=task --short-title "forward duration_ms via gb hook relay"`."
- The three texts (kd help, daemon error, advice/preamble) are the same sentence: Task 4's test asserts the error contains the rendered example; this task adds a Go test over `init_prompts.go` asserting the preamble line contains `--short-title` and the example, and the doc quotes `ShortTitleHelp` in a fenced block.

- [ ] **Step 1:** Patch the two preamble lines, add the doc, add the test. `go test ./internal/bridge/ -run InitPrompts`. Commit `docs(gardenio): short_title purpose, gate and groomer; preamble mentions --short-title (gd-V5nGseRS5f)`.
- [ ] **Step 2 (live, after Task 3 is released so the flag exists):** `gb advice add` the global advice; put its id on the plan epic. `gb advice lint` must stay clean.

---

### Task 9: the short-title groomer — role bead, trigger bead, scheduler pre-flight, prompt

**Files:**
- Modify: `gardenio/controller/internal/scheduler/conditions/leaf_daemon.go:285-320` (`evalBeadQueryCount` passes `ShortTitleMissing` through to `ListBeadsQuery`), `gardenio/controller/internal/scheduler/conditions/expression.go:106,241` (the two leaf structs beside `ExcludeBlockedByOpenTypes`: add `ShortTitleMissing bool \`json:"short_title_missing,omitempty"\``), `gardenio/controller/cmd/gb/trigger_schedule.go:136-150` (`scheduleConditionJSON`) and `:160-215` (`parseBeadQueryConditions`: new key `short_title_missing=true`), `gardenio/docs/short-titles.md` (groomer section)
- Create: `gardenio/docs/groomers/short-title-groomer.prompt.md` (the prompt, committed so it is reviewable; the trigger bead's `prompt` field is this file verbatim)
- Live (daemon writes, after Tasks 3 and 5 are released): one `type=role` bead, one `type=trigger` bead

**Interfaces:**
- Role bead: `kd create "short-title-groomer" --type=role -l project:gardenio -f model=anthropic/claude-haiku-4-5 -d "Haiku-pinned groomer that writes short_title on beads missing one (plan 2026-10-14-short-titles)."`.
- Trigger bead, through the house verb (`gb trigger create schedule`, `cmd/gb/trigger_schedule.go:31`; result has the shape of gd-OxGR1jvjZ9): `gb trigger create schedule --title "Short-title groomer: fill missing short_title (50/run)" --cron "*/30 * * * *" --tz UTC --action spawn_agent --role short-title-groomer --agent-project gardenio --allowed-role short-title-groomer --allowed-project gardenio --max-concurrent 1 --cooldown 10m --enabled=false --bead-query 'project=gardenio,types=task|bug|feature|epic|decision|escalation,statuses=open|in_progress|blocked|closed,short_title_missing=true,op=>,value=0' --prompt "$(cat gardenio/docs/groomers/short-title-groomer.prompt.md)"`. Then `kd update <trigger> -f wake_intent="Every 30 minutes, if any task/bug/feature/epic/decision/escalation lacks short_title, spawn one Haiku groomer that fills up to 50 of them (open first, then closed updated in the last 30 days) and exits."`. It stays disabled until Task 10 (`gb trigger enable <id>`).
- Prompt contract (the file): list with `kd list --missing-short-title --type task,bug,feature,epic,decision,escalation --status open,in_progress,blocked --limit 50 --json`, then if fewer than 50, `--status closed --sort updated_at --limit <rest>` and drop rows older than 30 days; for each bead read `title` and the first 300 chars of `description`; write `kd update <id> --short-title "<text>" --only-if-short-title-empty`; rules: <=40 characters, imperative, no ids, timestamps, type words, bracket tags or `P0:` ordinals; schedule-run and agent beads are skipped (deterministic creators own them); never edit `title`; finish with one `gb squawk` line `short-title groomer: filled N, skipped M, remaining R` and `gb stop` with the counts in the wrap-up. Budget: one pass, no retries beyond one re-list.

- [ ] **Step 1 (test first):** `leaf_daemon_test.go`: a `bead_query` leaf with `short_title_missing: true` calls `ListBeadsFiltered` with `ShortTitleMissing: true`; `trigger_schedule_test.go`: `parseBeadQueryConditions([]string{"…,short_title_missing=true"})` sets the field and an unknown value errors. Run → FAIL. Implement the pass-through next to the `labels`/`statuses` handling in `evalBeadQueryCount` (:285-320) and the parser case beside `case "statuses":`. Commit `feat(gardenio/scheduler): bead_query short_title_missing pre-flight (gd-V5nGseRS5f)`.
- [ ] **Step 2:** Write the prompt file and the doc section; PR together with Step 1.
- [ ] **Step 3 (live, needs Task 3 + Task 5 + this PR released):** create the role bead and the trigger bead (DISABLED). Verify with `kd show <trigger> --json` that `enabled` is a JSON `false` and `conditions[0].short_title_missing` is `true`. Put both ids on the plan epic.
- [ ] **Step 4:** Run the playbook once by hand without the cron: `gb spawn --role short-title-groomer --prompt "$(cat gardenio/docs/groomers/short-title-groomer.prompt.md)"`; confirm ~50 beads gained a `short_title`, none with an existing value changed (`kd list --type task --status open --limit 200 --json` before/after diff on `short_title`), and that the bridge renders one of them by name in a Slack message.

---

### Task 10: backfill to zero, audit the creators, decide the enforce flip

**Files:**
- Modify (privileged lane, DRAFT + `merge-desk:hold`): `deploy-values/gardenio-k3s.yaml` (`beads.shortTitleGate: enforce`)
- Live: trigger bead `enabled=true`; metric dashboards

**Interfaces:**
- Done when: `kd list --missing-short-title --status open,in_progress,blocked --type task,bug,feature,epic,decision,escalation --limit 1` reports 0 total; closed beads updated in the last 30 days also 0; `sum by (caller) (increase(gardenio_beads_short_title_gate_total{action="allowed_warn"}[24h]))` for `caller="agent"` is 0 for a full day (every in-repo creator supplies the field); the flip PR is merged and a deliberate `kd create --type task` from an agent pod without `--short-title` returns the Task 4 error text.

- [ ] **Step 1:** Enable the trigger (`gb trigger enable <trigger>`); watch runs (`kd show <trigger>` `last_outcome`, `last_run`); the open backlog (~560) takes ~12 runs, about six hours at the 30-minute cron.
- [ ] **Step 2:** After 24h of `warn` with the groomer at zero backlog, pull the metric. Any `caller="agent"` residual names a creator this plan missed (bridge thread/session beads are NOT gated; JIRA poller / GitLab sync — none deployed on k3s today, so expect none): file a follow-up step on the plan for each.
- [ ] **Step 3:** File `gb decision create --requesting-task <this step> --title "Flip BEADS_SHORT_TITLE_GATE to enforce" --option A "flip now" --option B "hold one more week"`. On A: open the deploy-values PR (DRAFT, `merge-desk:hold`) and a `merge_window` escalation. On B: schedule a re-check.
- [ ] **Step 4:** After the flip rolls: the deliberate refusal check above; comment the result on the plan epic; close.
