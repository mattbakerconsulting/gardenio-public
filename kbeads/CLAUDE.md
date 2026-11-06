# Beads

Beads is an event-driven work-tracking system for hierarchical work items ("beads") with
dependencies, labels, comments, and event history. It exposes gRPC + REST APIs and ships a
Cobra-based CLI client (`kd`).

## kd vs gb

`kd` handles **data operations**: bead CRUD (create, show, list, close), deps, labels,
comments, views, jacks, config, remotes, and `kd serve`. Agent **orchestration** commands
(agent lifecycle, decisions, gates, hooks, bus emit, mail, prime, yield, ready, setup,
advice, news) moved to `gb` (the gardenio CLI). Deprecated `kd` commands print a notice
pointing to `gb`.

## Label filters: `-l/--label` is AND, `--label-any` is OR

This is the **#1 footgun**. `kd list` / `kd search` accept two label filters with **opposite**
semantics:

- **`-l` / `--label` ANDs.** Repeating the flag *or* comma-joining both require **ALL** the
  labels: `-l a,b,c` ≡ `-l a -l b -l c` ≡ "carries a AND b AND c". For distinct
  single-kind labels (e.g. three different escalation kinds) this is **always 0** — no one
  bead carries all three. A comma in `-l` is AND, not a convenience OR.
- **`--label-any` ORs.** A bead matches when it carries **AT LEAST ONE** of the listed
  labels: `--label-any a,b,c` ≡ "a OR b OR c" (the union). It ANDs with `-l`, status, type,
  etc., and (server-side) narrows *within* OIDC project scope — it can never widen it.

```sh
kd list -l release-stuck,preflight-broken          # AND → 0 (no bead has both)
kd list --label-any release-stuck,preflight-broken # OR  → union of both kinds
```

This matters for **safety-gate queries**: a release cut-gate asking "is any `release-stuck` /
`preflight-broken` / `release-pipeline-broken` escalation open?" MUST use `--label-any` — the
`-l a,b,c` form returns 0 unconditionally and reads as a false all-clear (ka-0000003307).

## Key concepts

- **Bead** — the core work item. Has a kind (`issue`, `data`, `config`), a type (`epic`,
  `task`, `feature`, `chore`, `bug`, `formula`, `molecule`, or custom), a status (`open`,
  `in_progress`, `deferred`, `closed`), and optional metadata, labels, dependencies, comments.
- **`short_title`** — optional human display name, at most 40 runes, single line
  (`model.ShortTitleMax`, `model.ShortTitleHelp`; migration 056). The daemon collapses
  whitespace before the length check. `PATCH` with `"only_if_short_title_empty": true` only
  fills an unset value, so a groomer never overwrites an authored one. Any agent on its
  per-agent token may fill an EMPTY `short_title` on a non-decision bead in its token's
  project that it does not own, when the patch changes nothing else; the emptiness check
  is in the write itself (`store.FillShortTitle`; `internal/server/short_title_fill_authz.go`,
  gd-JOOXA7UqLh). `GET
  /v1/beads?short_title_missing=true` lists beads with none (NULL or `''`).
  gRPC carries the same fields: `Bead.short_title`, `CreateBeadRequest.short_title`,
  `UpdateBeadRequest.short_title` + `only_if_short_title_empty`,
  `ListBeadsRequest.short_title_missing`.
- **Plan-scoped writes** — a plan's own agents may update and close the plan epic and the
  beads whose parent chain reaches it (nearest plan wins), though they filed none of them:
  the plan's assignee, the thread (or dm) agent whose `thread_id` matches the plan's, and
  iterations of an open gardener trigger labelled `gardener-epic:<plan>`. Per-agent token
  only, in the token's project; epic, task, bug, feature, chore, bundle and doc only;
  never delete. Each admission writes an `authz.plan_scope` mutation_history row naming
  the rule and the plan. An agent token may not change `thread_id`, `slack_thread_bead_id`
  or `trigger_id` on an agent bead, since those links decide the binding
  (`internal/server/plan_scope_authz.go`, gd-Mk64Bko3XO). The one machine principal admitted is
  a `gardenio-machine-*` role with `PlanScope` (the beads-viewer's plan answer reactor, which
  applies an owner's plan ruling server-side, gd-Dl88Htipuq): the same beads in any project,
  plus update of the plan's gardener (`gardener-epic:<plan>`), whose enable it turns on is
  sealed from the owner's approval like the asking thread's
  (`spawn_lineage_plan_approval.go`). A home-garden plan its owner asked for in their own
  thread has no approval: the gardener the thread agent binds is sealed to the home owner
  when the owner opened the thread (the head's `created_by`) and the building plan's
  `thread_id` is that thread (`spawn_lineage_thread_ask.go`, P8.7).
- **Ownership grants** — a caller may update, close or delete an existing non-config bead
  only when a row of the grant table admits it (`internal/server/ownership_grants.go`,
  design and runbook `docs/ownership-grants.md`). The base rule (created_by, assignee or
  owner is the caller) is the row `owner`; every exception is a row, code rows and
  machine rows pinned by `testdata/ownership_grants.golden.json`. A role's write reach is
  rows in its role bead's `ownership_grants` field, set by an OIDC admin or an approved
  escalation, never by code. A denial names the bead's owners, the caller and the rows
  that would admit the write (`allowing_grants`). Every admit counts in
  `gardenio_authz_ownership_admits_total{grant,principal_kind,action,bead_type}`.
  A crew may update and close the task its agent bead names in `task_id`, and that task's
  direct children (row `dispatched-task`; task, bug, feature, chore; only while the bead is
  unassigned, the caller's or a gone agent's; `task_id` is fixed after spawn for an agent
  token), and the controller may claim an open, unassigned bug, feature
  or chore for the single-claim agent dispatched onto it (`DispatchClaim`;
  `internal/server/dispatched_task_authz.go`, gd-GrJRP7kLBY).
- **`heraldry`** — a bead's optional arms: a run of three to five library emoji in order,
  stored as the run itself, `🔥🧭📦` (`internal/heraldry`, spec
  `gardenio/docs/superpowers/specs/2026-10-25-heraldry-design.md`, migration 057; the owner's
  2026-10-26 verdict replaced the shield). Only epics (plans, phases) and projects may bear
  them (R6). Create and update take the run, with or without spaces, and `"none"` or `""`
  removes them; fewer than three emoji, more than five, or one twice is a 400 carrying
  `model.HeraldryHelp`. The old shield forms (`blue 🔥 . 🧭 . 📦`, the five places alone,
  `tl:🔥 c:🧭 br:📦`, a trailing `on <colour>`) are still read, as their charges in place order
  with the colour dropped: `heraldry.Parse` for input, `heraldry.Read` (no three-emoji floor)
  for values already stored, so beads not yet rewritten (step 5.6) keep reading. Arms are
  unique within a project among live beads, closed ones included, compared by run: the
  register is `beads.project` (the bead's one `project:` label) plus the partial unique index
  `idx_beads_heraldry_register`. A collision is a 409 `{"code":"heraldry_taken","bead_id":…}`
  (gRPC `AlreadyExists`); the same emoji in another order is an `X-Beads-Warning`; a bead with
  arms needs exactly one `project:` label (`internal/server/heraldry.go`). `GET
  /v1/beads?heraldry=<arms>` matches the run, and shield-form values whose places read as it.
  gRPC carries the same: `Bead.heraldry` (28), `CreateBeadRequest.heraldry` (17),
  `UpdateBeadRequest.heraldry` (15, optional), `ListBeadsRequest.heraldry` (28); a refused
  value is `InvalidArgument`, a collision `AlreadyExists`. An undo of a `heraldry` mutation
  puts the old arms back through the register (409 when another bead took them since). `GET
  /v1/heraldry/library` (`internal/server/http_heraldry.go`, viewer role, a day's cache with an
  ETag) serves the charges (emoji, name, codepoints, vs16) and `model.HeraldryHelp`, for
  clients that cannot import the package. `?heraldry_project=<p>` lists the project's register.
  `kd` takes `--heraldry` on `create` and `update`, prints one `Arms:        🔥🧭📦` line in `kd
  show`, an `ARMS` column (the run, laid out by width on screen through `wideCells`,
  `cmd/kd/heraldry.go`) in `kd list` / `kd search` when a row bears arms, and has `kd heraldry
  library [--search]` and `kd heraldry check <emoji…> [--project]`. `tincture.go`'s `palette`
  stays only so `Read` knows the old colour words and because the controller's `kbeadssrc`
  parses it; do not rename or reorder it.
- **Release** — `type=release` (kind `data`, born closed): one tag of one project, parented to
  its project bead and unique on `(project, tag)` through a daemon-stamped `idempotency_key`
  (`internal/server/releases.go`, design `gardenio/docs/releases-design.md`). The tag must
  match the project bead's `release_tag_pattern`. `status` (`cut`, `built`, `live`,
  `superseded`) is derived from `built_at` and `deployments`, never written by a client.
  A create with no tag is the project's **open release** (`releases_open.go`, plan gd-lgN4U7X5Tb):
  status `open`, kept open, keyed `release:<project>/next` (a second is a 409), stamped
  `opened_at`/`opened_by`; `POST /v1/releases/{id}/contents` appends rows to it, idempotent on
  `(merge_sha, pr)`; one PATCH writing `tag` (with `sha`, `cut_at`, `cut_by`, `source`) cuts it:
  the bead closes, its key becomes the tag's, and those fields are fixed after. A row appended
  while the cut is in flight is kept: a cut writing no `contents` takes the stored ones, and one
  writing `contents` is a 409 to re-read and retry (gd-k8Z0vMaohu). It cannot be
  closed or deployed before the cut.
  `deployments` is written only by `POST /v1/releases/{id}/deployments`, which closes the
  previous holder's row on that target and refuses a target the project's `release_targets` does
  not declare, except with `via=github` (github-bridge's `deployment_status`), where it appends the
  environment as kind `github-environment` in the same transaction. A helm revision is one
  roll per target: a report of a revision the release already records there changes nothing,
  and one another release of the project records is a 409. `POST
  /v1/releases/repair-deployments` (`releases_repair.go`, `kd release repair-deployments
  --project P [--dry-run]`) collapses a release's same-revision rows into the earliest and
  re-chains each target's rows in since order. Writers: an OIDC
  `gardenio-admin`, an agent whose role names `release-captain` on its own token, a machine
  granted the type. HTTP-only
  (`http_releases.go`): `kd release create|show|list|deploy|repair-deployments`. A newly written deployment row
  also closes the project's open tasks and escalations with `awaits_release=true` whose PRs
  the release or a release on its `previous` chain carries in `contents`; close-on-merge and
  close-on-execute leave such a task open (`releases_awaits.go`, gd-ZM7NEBPAqZ). An open
  release's gardener (a trigger labelled `release-gardener` and `gardener-release:<release>`,
  spawning `release-captain`) that a run of the previous release's open, sealed gardener binds on
  its own token is sealed to that gardener's author with its `iat` kept
  (`spawn_lineage_release_succession.go`, decision gd-cDG0zTiNSk); one sealed gardener per release.
- **Formula** — a reusable work definition (data-kind bead) with variable substitution,
  instantiated as a molecule. The DSL supports `extends`, `expand`, `loop`, `branch`, `gate`,
  `aspect`, `children`, `on_complete`, `requires_skills`, `waits_for`, `phase`, and file-based
  loading. Full DSL reference: [`docs/formula-reference.md`](../docs/formula-reference.md)
  (repo root). Preview expansion without creating beads via `kd formula cook`.
- **Molecule** — an instantiated formula (issue-kind bead) acting as an epic with child beads
  per step. Managed via `kd mol` (list, show, pour, wisp, burn, squash, bond, gc). Ephemeral
  wisps GC via `kd mol gc --age <duration>`.
- **Store** — persistence interface (`internal/store/store.go`), PostgreSQL implementation.
  All mutations are wrapped in transactions and recorded as events.
  > **Gotcha:** per-row batch sub-queries **must** use `WHERE col = ANY($1::text[])` with
  > `pq.Array(ids)` — a single param regardless of result size. Never build `IN ($1,…,$N)`
  > from N ids: lib/pq overflows past **65535** parameters and the query 500s silently on
  > large result sets (ka-0000002155). Copy the pattern from `queryGetLabelsForBeads` /
  > `queryGetBlockedByForBeads` in `internal/store/postgres/batch_queries.go`.
- **Events** — every mutation records an event row in Postgres and publishes to an event bus
  via the `Publisher` interface (`internal/events`); publishing is optional (no-op publisher
  when no bus is configured).
- **IDs** — nanoid format, prefixed `kd-` (`internal/idgen`).
- **Type configuration** — bead types are extensible. Built-ins in `internal/server/config.go`;
  custom types via `SetConfig` with key `type:<name>` (`TypeConfig` / `FieldDef` in
  `internal/model/type_config.go`). The legacy type name `template` is aliased to `formula`
  at the API layer (`model.TypeAliases`).

## Directory structure

```text
kbeads/
├── cmd/kd/              # CLI client (Cobra); one file per command
├── internal/
│   ├── config/          # Env-var configuration (BEADS_DATABASE_URL, etc.)
│   ├── events/          # Publisher interface and implementations
│   ├── idgen/           # Nanoid-based ID generation
│   ├── model/           # Domain types: Bead, Dependency, Comment, Event, Filter
│   ├── references/      # Text-reference extractor (typed + untyped mentions)
│   ├── server/          # gRPC + HTTP server, proto ↔ model conversion, interceptors
│   └── store/           # Store interface + postgres/ implementation with migrations
├── proto/beads/v1/      # Protobuf service and message definitions
└── gen/beads/v1/        # Generated Go code from proto (do not edit)
```

## Build & test

```sh
go build ./cmd/kd                # build the CLI
go test ./...                    # all tests — uses go-sqlmock, no running Postgres needed
make proto                       # regenerate protobuf Go code (or ./scripts/gen-proto.sh)
```

## Protobuf generation

Generated Go lives in `gen/beads/v1/`, produced from `proto/beads/v1/`. The `go_package`
option in each `.proto` controls the output module path
(`gardenio/kbeads/gen/beads/v1;beadsv1`).

**Regenerate** (`make proto`) after modifying any `.proto`, or after cherry-picking from
upstream (`alfredjeanlab/beads`) which uses a different module path — the proto files already
carry this fork's `go_package`, so regenerating fixes the generated code.

> **Gotcha:** `protoc-gen-go` / `protoc-gen-go-grpc` plugin versions **must match** the
> `google.golang.org/protobuf` and `google.golang.org/grpc` versions in `go.mod`, or the
> generated code fails to compile. (protoc v6.x; see `go.mod` for the exact plugin versions.)
> The committed `gen/` was built with protoc 3.21.12 (Debian `protobuf-compiler`). Without
> `libprotobuf-dev` that protoc has no well-known types (`google/protobuf/timestamp.proto:
> File not found`): set `PROTOC_INCLUDE` to a directory holding
> `google/protobuf/{timestamp,wrappers}.proto` from the matching protobuf release.

## Configuration (environment variables)

| Variable | Default | Purpose |
|---|---|---|
| `BEADS_DATABASE_URL` | *(required)* | Postgres connection string |
| `BEADS_GRPC_ADDR` | `:9090` | gRPC listen address |
| `BEADS_HTTP_ADDR` | `:8080` | HTTP listen address |
| `BEADS_METRICS_ADDR` | `:9091` | Metrics-only HTTP listen address (internal) |
| `BEADS_NATS_URL` | *(optional)* | Event bus URL |
| `BEADS_SERVER` | `localhost:9090` | CLI client target address |
| `BEADS_BRIDGE_TOKEN_RESYNC_INTERVAL` | `30s` | How often the daemon re-reads persisted `bridge_token:*` rows and adds any live token it does not hold (`0` disables). Covers a per-agent token registered on the old pod during a rolling update after the new pod loaded the store, which otherwise 401s until the next restart (gd-PPzLrS2k8e). Tokens this daemon revoked are never re-added. |
| `BEADS_HISTORY_RETENTION_DAYS` | `14` | Window for the hourly history-retention sweep (`internal/server/history_retention.go`, gd-1F8R0SQtiQ): it deletes `beads.bead.updated` events and `field='fields'` mutation_history rows older than this on `trigger` and `agent` beads, in batches of 500, at most 10 min a tick. Create, close, status, assignee and all other history is kept, as is every row of other types. `0` disables it. A `fields` history row holds only the changed keys (`old_value` the reverse merge patch, `new_value` the forward one), and a trigger or agent `bead.updated` event is stored without `bead.fields` (`fields_omitted: true`, `changes.fields` the forward patch, `fields_old` the reverse); the live stream still carries the whole bead. |
| `BEADS_EMBED_BACKEND` | `noop` | Embedding backend (`noop`, `ollama`) |
| `BEADS_EMBED_URL` | `http://localhost:11434` | Embedding backend base URL |
| `BEADS_EMBED_API_KEY` | *(optional)* | Embedding backend API key |
| `BEADS_EMBED_MODEL` | *(optional)* | Embedding model (e.g. `nomic-embed-text`) |
| `BEADS_REFERENCES_EXTRACT_COMMENTS` | `0` | When `1`, extracts ka-id refs from comment bodies. Off by default (decision ka-0000000868); opt in, run `kd admin reextract-references`, then flip on. |
| `BEADS_DECISION_AGING_ENABLED` | `0` (off) | When `1`/`true`, a daily sweep auto-DEFAULTS stale open decisions to their pre-declared default option so the operator queue stops accruing unbounded (ka-0000003434). Opt-in per decision: only decisions carrying a `default_option` field (or an option with `"default": true`) are touched — one with no declared default is left alone (no ping). Auto-defaults stamp `auto_defaulted: true` + `responded_by: system:decision-aging` and run the same close cascade as an operator resolve. |
| `BEADS_DECISION_AGING_DAYS` | `14` | Global staleness threshold in days (floored at 1) for the aging sweep above. A decision may extend (not shorten) its own window with an `auto_default_after_days` field. |
| `BEADS_SPAWN_POLICY_ENABLED` | `0` (off) | When `1`/`true`, the system:spawn-policy evaluator runs on every privileged-role agent-spawn escalation (create-time, plus a ~30s sweep) and auto-approves those matched by a declared, capped `type:spawn_policy` rule for that role (gd-vmLhgqydEa). Opt-in per role: a role with no `type:spawn_policy` bead is left untouched. First-match rules: an `auto` rule within its `max_per_day`/`max_concurrent` caps auto-approves (`approved_by: system:spawn-policy`, `approval_method: policy`, `policy_rule` stamped); over cap falls through to the next rule; a matching `human` rule leaves the escalation for an operator; if a policy bead exists but no rule matches, the escalation is denied and closed. A `type:spawn_policy` rule with `mode: auto` and no positive caps is refused at write time regardless of this flag. |
| `BEADS_SHORT_TITLE_GATE` | `warn` | Create-time `short_title` gate for `task`/`bug`/`feature`/`epic`/`decision`/`escalation` (gd-V5nGseRS5f, `internal/server/short_title_gate.go`). Unset or `warn`: a create with no `short_title` is allowed, returns an `X-Beads-Warning: short_title missing: …` header (HTTP only; gRPC logs a `slog.Warn`) and counts `gardenio_beads_short_title_gate_total{action="allowed_warn"}`. `enforce` (or `1`/`true`): refuses that create with a 400 for a non-human caller (static, fleet or machine credentials) and still only warns an oidc/edge human. `off` — or any unrecognised value — disables it. Flip to `enforce` once `caller="agent",action="allowed_warn"` is zero (plan Task 10). |
| `BEADS_HERALDRY_GATE` | `warn` | Create-time heraldry gate for `epic` in the home garden (gd-fn7uPujOqj, `internal/server/heraldry_gate.go`). Unset or `warn`: an epic created with no arms is allowed, returns `X-Beads-Warning: heraldry missing (kd update <id> --heraldry "...")` and counts `gardenio_beads_heraldry_gate_total{action="allowed_warn"}`. `enforce`: refuses (400, same words) a non-human caller's epic labelled `plan` or whose `parent_bead_id` is a plan; humans and other epics are only warned. `off`, or any unrecognised value, disables it. Reads nothing from the register. |
| `BEADS_HERALDRY_GATE_TENANT_LANDS` | `off` | The same gate's mode for epics in a tenant land (a project bead with `land_kind=tenant`); same values. |
| `BEADS_OPERATION_TYPE_GATE` | `warn` | The escalation `operation_type` registry (agent-access P7.4, gd-OXdyPW15fC, `internal/server/escalation_op_registry.go`): the closed set of types, folded into about 25 classes that `escalation_policy` rules match with `when.operation_classes` (a class the registry lacks is a 400 at policy write). Unset or `warn`: create rewrites an alias to its registered type (`helm_upgrade` → `helm_deploy`) and files an unregistered type with an `X-Beads-Warning` and a `slog.Warn`. `enforce`: an unregistered type is a 400 listing the registered ones. `off`, or any unrecognised value: no rewrite, no check. `gardenio_beads_operation_type_gate_total{class,action}` counts `registered`, `aliased`, `unregistered` and `refused`; flip to `enforce` once a release shows `action="unregistered"` at zero. Policy rules naming `operation_types` match a type and its aliases alike. |
| `BEADS_PROJECT_LABEL_MISMATCH` | *(refuse)* | Project-label invariant at create (gd-YdsEpnN3Qo, `internal/server/project_label_invariant.go`). A project-bearing bead (its type declares a `project` field; `plot_application` excepted) with `fields.project` and no `project:` label gets `project:<fields.project>`; a message with no `project:` label takes its thread head's (`fields.thread_id`). A bead whose `project:` labels all name another project is refused (400, after the authz checks); `warn` logs and allows it instead. `gardenio_beads_project_label_invariant_total{bead_type,action}` counts what it did, `gardenio_beads_project_label_missing{type}` (hourly) counts beads still without the label, and `kd admin backfill-project-labels [--apply]` labels the old ones. |
| `BEADS_ESCALATION_RESULT_REQUIRES_APPROVAL` | `false` | The D6 gate on `POST /v1/escalations/{id}/result` (escalation-routing T6, `internal/server/escalation_result_gate.go`), read per request. Off: any caller that passes write-authz may record a result (the old behaviour). On: a result is a 403, leaving the bead untouched, unless (a) the escalation has a server-stamped approval (`approved_at`) and the caller has its `execute_by` lane's authority (empty means `operator`; `callerHasLaneAuthority`: `operator` is `gardenio-admin` on any OIDC client, `kd login` included, and `land-owner:<land>` the land's owners group; presence was proven at approve, and the result is stamped `result_client=<azp>`, gd-JvbxHtR6bx), (b) `execute_by=server` and the write is the daemon's own, (c) the verified requester withdraws with `result_status=failure`, (d) the caller is a machine in `BEADS_ESCALATION_RESULT_MACHINES`, or (e) a member of a merge escalation's coordinator `execute_by` lane sends it back with `result_status=failure` while that lane holds it (`held_by`; gd-fs9lGoSLDd) or after a hand-up moved it off (`held_at` equal to `handed_up_at`; gd-0v8Ftju0z0). |
| `BEADS_ESCALATION_RESULT_MACHINES` | *(empty)* | Comma-separated machine client ids whose escalation results the gate above accepts without an approval (D6(d)). Empty at rollout. |
| `BEADS_ESCALATION_LANE_PRESENCE_WINDOW` | `30m` | Presence staffing for machine coordinator lanes (`internal/server/escalation_lane_presence.go`, gd-t41kYFbLUt): a presence-tracked client in `BEADS_ESCALATION_COORDINATOR_CLIENTS` staffs its lane only if it made an authenticated request this recently. Last-seen is the config row `escalation_lane_seen:<client>` (written at most once a minute, read through a 30s cache), so every replica and a restart agree. An away lane is never picked as a route or hand-up target (`laneStaffed`), but keeps what it holds and executes (`laneMayHold`), and D5 holds with it rather than with an owner lane, so an offline laptop never adds owner asks. GET/list `/v1/escalations` carry `held_by_presence` `{lane, client, online, last_seen_at, window}`; an item held while its lane is away is stamped `held_by_offline_at` once, and the bridge posts one notice in its card's thread. `0` turns presence off. |
| `BEADS_ESCALATION_PRESENCE_CLIENTS` | `gardenio-laptop-operator` | The machine coordinator clients staffed by presence (comma-separated; `-` for none). `gardenio-cluster-coordinator` is left out: it is spawned on demand, so a quiet window is its normal state. |
| `BEADS_ESCALATION_LOCAL_TARGET` | *(enforce)* | Create-time check that an `execution_target=local` escalation names where it runs, `target_runner` (a runner bead id, spec §8.1; P7 Task 13 briefly took an agent bead id) or `target_owner` (an operator id; their one online runner is resolved at approval), read per request (`checkEscalationTargets`, `internal/server/escalation_targets.go`, the one create guard; P4 Task 16). Unset: a local escalation naming neither is refused with a 400. `warn`: allowed with an `X-Beads-Warning`. A `target_runner` that is not an open, unrevoked runner is refused in both modes, whatever the `execution_target`. `execution_target`, `target_runner`, `target_owner` and `requesting_runner` are immutable after filing. On approval of a local dispatch (`execution_dispatch=local`) that names a runner, the runner is checked again and only its owner (or `target_owner` itself) may approve, else 403 with nothing written; a coordinator lane is refused too (`checkLocalApproval`). `startEscalationExecutor` records the agent joined on the runner as `executor` (the identity `POST /result` accepts), and after the status flip `emitEscalationApprovedMail` sends it a `control:escalation-approved` mail pointing at `gb escalation close`, or parks the mail as `runner:<id>` when none is joined. `executor_mail_id` records it and the response carries `executor_notified`. A server replay or a verdict is never mailed. A failed mail never fails the approval. On the laptop that mail is the only trigger: `gb agent start --runner` hands `control:escalation-approved` to its escalation executor, and its mail subscriber re-addresses parked `runner:<id>` mail to itself before delivering it. |
| `BEADS_LANDS_DOOR_MESSAGES_PER_MINUTE` | `20` | Messages one caller may create in one land through its land door, per minute (`internal/server/land_door_message_rate.go`, gd-i74esUHCTg): the daemon-side copy of beads-viewer's `COMMONS_MESSAGES_PER_MINUTE`, since the door reaches `POST /v1/beads` past the viewer. Past it a `type=message` create is a 429 with `Retry-After`. `0` turns it off; an unparseable value is the default. In memory, so a restart refills it. |
| `COOP_S3_BUCKET` | *(unset)* | The picture bucket the land door serves decision evidence from (`GET /v1/lands/v1/lands/{land}/decisions/{decision}/evidence/{image}`, `internal/landmap/evidence.go`, gd-wRXlwYH2GI): the one `gb image upload` writes and the viewer's `/image/` proxy reads. Unset: that route answers `upstream_unavailable` and `Decision.evidence` stays absent. `COOP_S3_PREFIX` (default `coop/sessions`; only `<prefix>/images/` is served), `COOP_S3_REGION` and `COOP_S3_ENDPOINT` (path-style, e.g. MinIO) as for the viewer. Needs read on `<prefix>/images/*` for the daemon's ServiceAccount. |

`BEADS_AUTH_TOKEN` and `BEADS_PREVIOUS_TOKEN` (the shared static bearer and its rotation twin) are no longer read by the daemon (gd-ke6PC7bRto): it logs a warning if they are set and accepts only fleet tokens, OIDC JWTs and machine principals. `X-Actor` and `X-Fleet` are honoured for no caller. `kd` still reads `BEADS_AUTH_TOKEN` as one of its client-side bearer env var names.

`BEADS_TAINT_PRIMARY_TOKEN_GRANTS` (ADR docs/adr/taints.md §13 OQ1, decision gd-KUlo9gSoRh) was the taint read/write gate's migration-window escape for the primary static bearer token. Removed in phase 3 (gd-3KUNlqC1SQ) once bridges held fleet-token grants of their own — the env var is no longer read anywhere in the server; the primary token is gated like any other principal, no bypass.

## Typed mention syntax

The text-reference extractor (`internal/references`) recognizes bare `ka-0000001713` mentions
and **typed** mentions that declare the relationship kind. Design-of-record — full verb→
`DependencyType` vocabulary, storage schema (`bead_references.relationship_type`, migration
013), and query surface (`kd refs`, `/v1/graph`):
[`docs/research-typed-mention-syntax.md`](docs/research-typed-mention-syntax.md);
implementation epic `ka-0000003840`, parser in `internal/references/typed.go`.

**Two equivalent surface forms** parse to the same edge:

- Function-call: `parent(ka-0000007196)` — inline, primary syntax.
- Colon-line: `parent: ka-0000007196` — line-anchored, list-friendly, optional Markdown bullet.

Bare `ka-XXX` outside either form stays an untyped flat mention.

> **Gotcha:** only the closed-core verb set (`parent`, `blocks`, `related`, `duplicates`,
> `supersedes`, `triggered-by`, `chains_to`, and their inverses — see the design doc for the
> full 1:1 mapping) produces a typed edge. Any verb **outside this set degrades to an untyped
> mention** (`relationship_type = NULL`) — not an error, silently ignored as a typed edge (the
> raw verb is preserved for future promotion analysis).

## Commits

Short imperative subject; scope in parentheses when it helps:
`fix(store): handle nil metadata on update`.

## Landing the plane

1. **Test** — `go test ./...` must pass.
2. **Quench** — `quench check` must pass (cloc max 1000 LOC, escapes = panic needs a
   `// PANIC:` comment, agents = CLAUDE.md required, docs TOC + links, tests, git commits).
3. **Keep generated code in sync** — modifying `.proto` under `proto/` means regenerate
   `gen/` and commit both.
4. **Follow existing patterns** — the server layer converts proto ↔ model via
   `internal/server/convert.go`; new CLI commands go in their own file under `cmd/kd/`.
5. **Record events** — any new mutation must call both `store.RecordEvent` and
   `publisher.Publish`.
6. **Migrations** — schema changes need a new numbered migration pair in
   `internal/store/postgres/migrations/` (see the policy below).

## Migration policy

- **Idempotent DDL only.** Every `ALTER TABLE` / `CREATE TRIGGER` / `CREATE INDEX` /
  `CREATE TABLE` uses `IF NOT EXISTS` (or `DROP … IF EXISTS` before `CREATE`). A
  partially-applied migration that left `schema_migrations.dirty=true` must recover on retry
  without manual intervention. See migrations 019 / 020 / 021 for the canonical pattern.
- **No eager full-table backfills.** Schema migrations run inside a single transaction that
  holds row/table locks for the duration. On a 313K-row prod env this stalls every read AND
  write for the whole window (ka-0000006380). Backfills go in: (a) a controller cron, (b) a
  one-shot `kd admin` command, or (c) a `UPDATE … LIMIT N` loop in psql so each batch is its
  own transaction. The schema migration installs the trigger for new writes; legacy rows are
  filled separately.
- **Migrations run via `kd migrate`, not the daemon.** Prod deploys invoke `kd migrate` from
  a Helm pre-upgrade Job (`gardenio/helm/gardenio/templates/beads/migrate-job.yaml`) so
  migrations have no startup-probe deadline. The daemon's own `m.Up()` after the Job is a
  sub-second `ErrNoChange` no-op (epic ka-0000005716).
- **`BEADS_MIGRATE_AUTO_RECOVER=last-clean`** is the Job default — auto-clears a stale
  `dirty=true` and retries. Idempotent migrations make this safe.
