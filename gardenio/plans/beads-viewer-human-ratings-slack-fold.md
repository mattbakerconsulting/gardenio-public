# Beads-Viewer Human Ratings — Slack-fold Plan of Record (F3: sunset `human_survey`)

**Status:** Plan of record. Follow-on to T1b / [ka-0000007419](../../) of epic [ka-0000007308](../../).
**Decision:** [ka-0000007600](../../) — operator chose **F3** (sunset `human_survey`; bridge writes only `rating`; one-time data migration; port beads3d).
**Predecessors:**
- T1b research: [`gardenio/docs/research/beads-viewer-human-ratings-slack-fold.md`](../docs/research/beads-viewer-human-ratings-slack-fold.md) (MR !1783)
- T3 plan-of-record (model-A): [`gardenio/plans/beads-viewer-human-ratings.md`](./beads-viewer-human-ratings.md)
- T4a (in-progress, registers `type=rating`): [ka-0000004715](../../)

---

## 0. Why this plan exists

T1b inventoried two parallel rating mechanisms:

- `type=human_survey` — bridge-side, Slack `gb stop` exit-survey (`-1 / 0 / +1`, `rater_slack_user`, parent-child to agent bead). Source: `gardenio/controller/internal/bridge/bot_human_survey.go`. Existing reader: `beads3d/src/ratings-page.js`.
- `type=rating` — beads-viewer, OIDC POST (`1..5` star, optional axes, `rates` edge, `rater` email). T4a registration in flight.

The operator chose F3 — keep one canonical record (`type=rating`) and migrate the Slack flow onto it. This plan tells implementers exactly what to change, in what order, with what fallbacks.

---

## 1. Scope

In scope:

1. **Type-config amendments** to `type=rating` so it can carry Slack-shaped data without lossy round-tripping (`score_native`, `source`, `slack_*` fields, optional `feedback`).
2. **Bridge write-side rewrite** — the `human_survey` button-click handler creates a `rating` bead instead, with score mapping, identity resolution, and a `rates` edge to the *agent bead*.
3. **Manual-trigger and feedback-modal paths** — `rate <bead>` keyword, `:bar_chart:` reaction, "Add feedback" overflow are all rewired against `rating`.
4. **One-time data migration** of existing `human_survey` beads to `rating`, idempotent, with provenance preserved.
5. **Beads-viewer** — `/ratings` and the per-bead widget read the new fields; surfaces a `source` filter; renders the native scale next to stars where applicable.
6. **Beads3d port** — `ratings-page.js` queries `type=rating` filtered to Slack-source rows.
7. **Decommission** — the `human_survey` type registration stays for one release after the migration completes (read-only safety net), then is dropped.

Out of scope:

- Cross-tab rollups (decisions / costs / advice into `/ratings`).
- iOS Gardenio parity.
- Promoting high-rated work into advice / training pipelines.
- Any change to `human-survey-config` semantics (the dark-launch flag, role allowlists, expire window). The bridge still loads the same config; only the destination record type changes.

---

## 2. Data model — amendments to `type=rating`

T3 / T4a already lock the `model-A` shape (`rated_bead_id`, `score 1..5`, `rater` email, optional `axes`/`tags`, free-text rationale in description). F3 needs three additions and one redefinition. **All four are optional fields** — beads-viewer-authored ratings continue to land unchanged.

| Field | Type | Required | Purpose |
|---|---|---|---|
| `score_native` | integer | optional | The original-scale value before normalization. For Slack rows this is `-1`, `0`, or `+1`. For viewer-authored rows it is unset (or equal to `score`). Authoritative when reading aggregates that should not collapse the scale. |
| `source` | string | optional | Provenance. Enum-like: `web` (beads-viewer POST), `slack-stop` (exit-survey click), `slack-manual` (manually-triggered prompt), `slack-feedback-only` (feedback-modal without a rating click), `slack-backfill` (one-time migration). Defaults to `web` at the viewer write-site if absent. |
| `slack_channel`, `slack_thread_ts`, `slack_user_id` | strings | optional | Slack-side coordinates preserved verbatim from the originating click for traceability and for resolution-failure fallback. Empty for `source=web`. |
| `feedback` | string | optional | Slack free-text feedback captured via the modal. Mirrors the original `human_survey.feedback` field 1:1 — kept distinct from the bead description so existing modal copy and submission semantics do not change. |

T4a's open MR must accept these as **append-only optional fields**. If T4a has already merged before this plan lands, a follow-up T4a-2 task amends the type-config; no data migration is required for the amendment itself (existing `rating` rows stay valid).

**Score mapping** (`score_native -> score`, when writing from Slack):

| `score_native` | `score` | Rationale |
|---|---|---|
| `-1` | `1` | "Worse than nothing" → 1★ (worst). Lossy at the floor, but the only value in `1..5` that is unambiguously negative. |
| `0` | `3` | Neutral mid-point. |
| `+1` | `5` | "Helpful" → 5★ (best). |

Loss is one-way: aggregates that need the native scale read `score_native`; aggregates that need stars read `score`. The viewer-side write path leaves `score_native` unset; readers treat absent `score_native` as equal to `score`.

**Rater identity** (Slack-source rows):

- The bridge resolves `slack_user_id` → email via Slack `users.info` (`users:read.email` scope) at write time, with a small in-process LRU (`max=1024`, `ttl=24h`).
- Resolution success → `rater` is the email; `slack_user_id` is also persisted for traceability.
- Resolution failure (API error, no email scope, deleted user) → `rater` is `slack:<user-id>` (sentinel form); `source` is unchanged. No bead is dropped; failure surfaces only as a debug log line.
- The `(rater, rated_bead_id)` idempotency key still holds; a rater whose email later resolves on a re-click *will* create a second rating bead (different `rater` value). This is rare; the migration script (§5) addresses the legacy case but the live write-path accepts the duplicate as a known limitation.

**Edge: `rates`** to the agent bead. Slack ratings rate the *agent session* (per T1b §2.3 — the human_survey signal is "how was this session"), so `rated_bead_id = agent_bead_id`. The bridge adds the `rates` dependency immediately after creating the bead, mirroring `gardenio/controller/cmd/beads-viewer/ratings.go:301-314`. The previous `parent-child` edge is dropped — the `rates` edge is sufficient and matches the canonical model.

**Labels.** The bridge mirrors the agent bead's `project:` label and adds a fresh `agent:<agent-bead-id>` label, exactly as `bot_human_survey.go:319-322` does today. Unchanged.

---

## 3. Write-path — bridge rewrite

Three entry points exist today; all three move from `human_survey` to `rating`.

### 3.1 Exit-survey click (`bot_human_survey.go:handleHumanSurveyRate`)

Today (`bot_human_survey.go:243-347`): button click → `findExistingHumanSurvey` → upsert `human_survey` bead → add parent-child edge.

After F3:

1. Resolve `rater_email := resolveSlackEmail(slack_user_id)` (best-effort; sentinel on failure).
2. `score_native := rating` (the integer from the button value); `score := mapNativeToStars(rating)`.
3. `existing := findExistingRating(agent_bead_id, rater_email)` — same idempotency lookup as the viewer (`ratings.go:findExistingRating`), but scoped to the rater-and-target pair.
4. **Upsert path** (existing found): `UpdateBeadFieldsTyped` with the new field values, preserving `created_at`. Title is rewritten to `rating: <agent-bead-id> · <stars>★`.
5. **Create path** (no existing): `CreateBead{Type:"rating", ...}` then `AddDependency(newID, agent_bead_id, "rates", "slack-bridge")`. Drop the parent-child call.
6. Confirmation message logic (`rewriteHumanSurveyConfirmation`) is unchanged in shape; only the success copy mentions stars: *"Rating ★★★★☆ recorded — thanks!"* (or the canonical −1/0/+1 phrasing — see §3.5).

### 3.2 Manual-trigger and reaction paths (`bot_human_survey_manual.go`)

`@gardenio rate <bead-id>` and the `:bar_chart:` reaction post a fresh prompt and store `human_survey_prompt_ts` on the agent bead for dedup. Neither path writes a rating bead by itself — they post the block-kit message; the resulting click goes through §3.1. The only change here is to drop the `humanSurveyAgentLabelPrefix` from the *prompt-message* bookkeeping (the prompt itself doesn't write a bead). The `human_survey_prompt_ts` agent-bead field is retained — it stores the *Slack message ts*, not a bead id, and the dedup logic is independent of the destination type.

### 3.3 Feedback-modal submission (`bot_human_survey_manual.go:265-360`)

Today: modal submit looks up the existing `human_survey` bead by `(agent_bead_id, rater_slack_user)` and updates the `feedback` field; if no rating yet, it creates a feedback-only `human_survey` bead.

After F3:

1. Lookup is now on `type=rating` by `(agent_bead_id, rater_email)` (with `slack_user_id` fallback when email is unresolved — see §3.4).
2. Update path: `UpdateBeadFieldsTyped` sets `feedback`. The existing `feedback` field on `rating` (§2) holds the Slack modal text verbatim, distinct from the description.
3. Feedback-only create path (no rating yet): `CreateBead{Type:"rating", source:"slack-feedback-only", score:0, score_native:0, ...}`. **`score=0`** is below the registered minimum (`1..5`); we therefore relax the `score` field to `min:0` only when `source=slack-feedback-only`. Two equivalent implementations:
    - **(a)** Keep the `1..5` constraint and require a sentinel `score=1` for feedback-only beads. Readers must special-case `source=slack-feedback-only` to suppress the 1★ rendering.
    - **(b)** Lower the constraint to `score >= 0` in T4a-2, with a runtime-side rule that `score=0` is only legal for `source=slack-feedback-only`.
    - **Recommended: (b)** — readers do not need to special-case scale interpretation, and `score=0` is a clearer "no rating" sentinel than 1★.
4. The "Add feedback" follow-up button (`rewriteHumanSurveyConfirmation`) keeps its current behaviour; only the lookup type changes.

### 3.4 Idempotency under identity resolution failures

If a rater's first click succeeded with `rater=slack:U0OPERATOR1` (sentinel) and a later click resolves the same Slack id to `alice@acme.example`, a naive lookup would miss the existing bead and create a duplicate. The bridge looks up by **(`agent_bead_id`, *either* `rater=email` *or* `slack_user_id=U…`)**, picking the first match deterministically. This degrades gracefully when:

- All clicks fail to resolve → all clicks land as `rater=slack:U…`; lookup matches; idempotent.
- All clicks succeed → all clicks land as `rater=email`; lookup matches; idempotent.
- A click succeeds after sentinel writes → the bridge updates the existing sentinel-rater bead in place and **rewrites `rater` to the email**. This is the only place we mutate a `rater` value after creation; tests cover it explicitly.

### 3.5 Slack confirmation copy

Confirmation messages stay native to the −1/0/+1 scale ("Rating *+1* recorded — thanks!", etc.). We do not show stars in the Slack thread — the click was on a thumb scale, not a star scale, and surfacing the conversion would confuse the rater. The star value is an internal storage detail. The viewer renders both (§6.1).

### 3.6 Type-config registration cleanup

`gardenio/controller/internal/bridge/init.go:253-265` registers `type:human_survey`. Keep the registration through one release (defence-in-depth: legacy unmigrated beads still validate during the migration window) and remove it in the same release that drops the migration tooling (§7).

---

## 4. Read-path — beads-viewer changes

### 4.1 `/ratings` list

`(*Server).handleRatings` (`ratings.go:108`) already filters by `Types: ["rating"]`. After F3 this is unchanged in shape, with three additions:

1. New `?source=` filter (`web`, `slack`, or unset). `source=slack` matches `slack-stop|slack-manual|slack-feedback-only|slack-backfill`. Implemented as a post-list filter — `ListBeadsQuery` does not support OR-of-field-values today; the filter rejects rows whose `source` field does not match. Cost: one extra string-compare per row over the existing `ratingsListLimit` cap.
2. New "Source" column in `templates/pages/ratings.html` showing a small badge — `web` / `slack`. Slack rows additionally show the `score_native` value next to the stars (e.g. `★☆☆☆☆ (−1)`).
3. `ratingRowView` gains `Source string`, `ScoreNative *int`, and `Feedback string` (snippet, when no description is present).

### 4.2 Per-bead rating widget

`buildRatingWidget` (`ratings.go`) walks `rates` reverse-deps. After F3 this picks up Slack ratings *for free* — they carry the `rates` edge. The widget renders mixed rows, with a "Slack" badge on `source=slack-*` rows.

The per-bead aggregate continues to compute `mean ± n` over `score`. We add a *secondary* aggregate over `score_native` for beads where any row has a non-null `score_native`, rendered as `Slack scale: +0.6 (n=12)` underneath the star aggregate. This is the only place the dual-scale UX surfaces in the new world; if operator feedback says "just show stars", the secondary line is removable in one commit.

### 4.3 Rated-bead semantics (agent vs work)

Slack ratings rate the *agent session* (`rated_bead_id = agent_bead_id`). Viewer ratings rate any bead. This is intentional — see T1b §2.3. The widget on an *agent* bead's show page will surface both Slack and viewer ratings of that agent. The widget on, say, an MR bead surfaces only viewer ratings (because the bridge has no current path that targets MRs).

A future enhancement (out of scope) could let the bridge target the agent's *work-output* bead when the agent attached one — that requires a stable convention for agent → work-output discovery and is deferred.

### 4.4 Self-rating guard

`isSelfRating` (`ratings.go:363, 412`) compares `rater` email to the rated bead's owner agent identity. For Slack rows the rater is the *Slack-resolved email of a human reviewer*, so the guard naturally rejects an agent rating itself — agents do not have Slack OIDC emails. No change needed; the existing guard is sufficient.

---

## 5. One-time data migration

A new tool: `gardenio/controller/cmd/migrate-human-survey/` (Go binary, runs against the kbeads HTTP API). One-shot, idempotent, batch-paginated. Driven by an operator from a workstation or a one-shot `Job` in the cluster — not in the controller hot path.

### 5.1 Inputs and outputs

- Input: every bead with `type=human_survey` and `status=open`. (Closed `human_survey` beads, if any, are migrated in the same pass — there is no semantic difference for ratings.)
- Output: a corresponding `type=rating` bead per `(agent_bead_id, rater_slack_user)` pair, with provenance preserved.
- Side effects: adds a `rates` edge from the new rating to the agent bead; **does not** delete the source `human_survey` bead in the same pass (see §5.4).

### 5.2 Field mapping

| `human_survey` field | `rating` field | Notes |
|---|---|---|
| `agent_bead_id` | `rated_bead_id` | Identical semantics. |
| `rating` | `score_native`, `score` | Native preserved verbatim; `score = map(native)` per §2. |
| `rater_slack_user` | `slack_user_id` | Always preserved. |
| (resolved email) | `rater` | Slack `users.info` lookup at migration time. Sentinel `slack:<user-id>` on failure. |
| `slack_channel` | `slack_channel` | Verbatim. |
| `slack_thread_ts` | `slack_thread_ts` | Verbatim. |
| `feedback` | `feedback` | Verbatim. |
| `source` (`stop`/`manual`) | `source` | `stop → slack-backfill-stop`; `manual → slack-backfill-manual`. The `-backfill-` infix lets us tell migrated rows apart from live rows for at least one observability pass. |
| `answered_at` | `created_at` of the new bead | Best-effort: passed via the `--created-at` flag if the daemon supports overriding it; otherwise the migration accepts `created_at = now` and stores `answered_at` in a new optional `original_answered_at` field on `rating`. **Decision deferred to implementation:** if the daemon does not support `created_at` override, ship with `original_answered_at` and document it. |
| labels (`agent:…`, `project:…`) | labels (mirrored) | Verbatim. |

### 5.3 Idempotency

Each pass:

1. Reads all `human_survey` beads in batches (`ListBeadsFiltered{Types:["human_survey"], Limit:200}` with cursor).
2. For each, computes the target `rater` via the same resolver as the live write path (§3.4).
3. Looks up an existing `rating` by `(agent_bead_id, rater)` *and* by `(agent_bead_id, slack_user_id)` (handles sentinel-then-resolved drift).
4. If found and the existing rating already has matching `score_native` + `source` + `slack_user_id`, **skip**.
5. If found but missing fields, **update** (idempotent fix-up — covers the case where a previous partial migration crashed).
6. If not found, **create + edge**.

Runs are safe to re-execute. The tool prints a one-line summary per row and exits 0 on success, 1 if any row failed (with row IDs for retry).

### 5.4 Source-bead disposition

After a successful migration pass, source `human_survey` beads remain in place (read-only). One release later, the cleanup pass closes them with a `migrated-to:<rating-id>` field. Two releases later, when no consumer is reading `type=human_survey` (verified via `kd list --type=human_survey` count), the type registration is dropped.

Rationale: leaves a forensic trail through one observability cycle; avoids the irreversible-delete failure mode where a botched migration is discovered post-deletion.

### 5.5 Pre-flight and dry-run

The tool ships `--dry-run` and `--limit N` flags. The first production pass runs as `--dry-run` with `--limit 0` to print the migration plan to stdout (counts per source-type, resolution-failure count, per-rater duplicate-key collisions). Operator reviews. Second pass drops `--dry-run`.

---

## 6. Beads3d port

`beads3d/src/ratings-page.js` queries `type=human_survey` (line 339) to drive the aggregate dashboard. F3 ports it to `type=rating` filtered to Slack-source rows.

### 6.1 Query change

Before:

```js
_api.list({ type: 'human_survey', limit: 500 })
```

After:

```js
_api.list({ type: 'rating', source: 'slack', limit: 500 })
```

If the daemon's list endpoint does not support a `source=` filter, the page lists `type=rating, limit=500` and post-filters in JS for `fields.source.startsWith('slack')` (or `endsWith('-backfill-…')`). The filter is one line of JS; daemon-side support is a nice-to-have, not a blocker.

### 6.2 Field-name changes

`ratings-page.js:aggregateRatings` reads `fields.rating`. Rename callers to `fields.score_native ?? fields.score` so the dashboard keeps showing the canonical −1/0/+1 scale. The renderer in `ratingColor()` already handles values outside that range.

`fields.agent_bead_id` is no longer authoritative — the canonical agent reference is now the `rates` edge. Two options:

- **(a)** Keep reading `fields.agent_bead_id` (which the bridge continues to write, §3.1 step 5 above) — zero JS work.
- **(b)** Walk the `rates` edge in JS to discover the agent bead — requires an extra `_api.show()` per row, or a server-side join.

**Recommended: (a)** — the field is denormalized but cheap, and the dashboard already depends on it.

### 6.3 Test coverage

`beads3d/tests/ratings-page.spec.js` and `detail-panel-ratings.spec.js` exercise the existing surface. Update fixtures to use `type=rating` with `source=slack-stop` and `score_native`. The aggregate math is identical; only the input shape changes.

### 6.4 Coordination

This change happens in the same MR series as the bridge rewrite — once the bridge writes `rating` rows, the beads3d page must read them, otherwise the dashboard goes stale. The cutover order in §7 sequences this so beads3d is updated *before* the bridge stops writing `human_survey`.

---

## 7. Phasing and rollout

The phasing is designed to keep both readers (beads-viewer, beads3d) seeing data at every step. No flag day.

### Phase 0 — type-config amendments (T4a / T4a-2)

- T4a-2 (or T4a if not yet merged): append optional fields to `type=rating` (`score_native`, `source`, `slack_channel`, `slack_thread_ts`, `slack_user_id`, `feedback`); relax `score` to `>= 0`.
- No data changes; existing `rating` rows stay valid.
- Acceptance: tests in T4a continue to pass; new tests cover validation of the optional fields and the `score=0` + `source=slack-feedback-only` rule.

**Gate before Phase 1:** Phase 0 must be merged and deployed to gardenio-rwx.

### Phase 1 — bridge dual-write (transitional)

- Bridge continues writing `human_survey` *and* now also writes `rating` per §3, behind a feature flag (`HumanSurveyConfig.WriteRatingType bool`, default `false`).
- Identity resolver, score mapper, idempotent upsert all land here.
- Tests cover: dual-write, identity resolver fallback, score mapping, manual-trigger and feedback-modal paths.
- Acceptance: with the flag on in gardenio-rwx, every Slack click produces both a `human_survey` and a `rating`; with it off, behaviour is unchanged.

### Phase 2 — beads-viewer reads new fields

- `/ratings` list and per-bead widget render `source` + `score_native` + native-scale aggregate (§4).
- Beads3d page is updated to read `type=rating` with `source=slack`; can run against either the still-empty (live writes off) or partially-populated (live writes on) state because it filters explicitly on `source`.
- Acceptance: Playwright tests in `gardenio/tests/beads-viewer-e2e/` cover the new filter and column. Beads3d Playwright fixtures in `beads3d/tests/ratings-page.spec.js` use the new shape.

### Phase 3 — flip the bridge flag in gardenio-rwx

- Set `HumanSurveyConfig.WriteRatingType = true` in the rwx config bead.
- Smoke-test the exit-survey path against an rwx agent.
- Watch dual-write rows appear in `/ratings`.

### Phase 4 — one-time data migration (rwx)

- Run `migrate-human-survey --dry-run` against rwx; review counts.
- Run live; verify migrated rows in `/ratings`.
- Beads3d dashboard now shows the full historical aggregate plus dual-written live rows. The dual-write window means a small set of `human_survey` rows have *both* a legacy reader path and a migrated `rating` shadow — both readers point at the same data, so this is benign.

### Phase 5 — flip in prod

- Phase 1–4 cycle repeated against prod beads-viewer / prod gardenio. Same migration tool, same dry-run posture.

### Phase 6 — single-write cutover

- One release after Phase 5: bridge stops writing `human_survey`. The dual-write flag is removed.
- The `human_survey` type registration is left in place for read-back compatibility for one more release.

### Phase 7 — type registration cleanup

- One release after Phase 6: drop the `type:human_survey` registration in `init.go`. Drop the migration tool. Close any leftover `human_survey` beads with `migrated-to:<rating-id>` populated.

**Total elapsed time:** roughly four weekly releases from Phase 0 to Phase 7. The *user-visible* cutover (Slack ratings appear on `/ratings`) lands at Phase 3.

---

## 8. Risks and rollback

| Risk | Severity | Mitigation |
|---|---|---|
| Score mapping is lossy at the floor (`-1 → 1★` collapses "worse than nothing" semantics). | medium | `score_native` preserves the original; aggregates that need the native scale read it. The viewer renders both. Slack confirmation copy stays native. |
| Slack `users.info` rate limits or scope failures cause `rater` sentinel writes. | low | LRU cache; debug logging; idempotent self-healing on later success (§3.4). Migration tool exposes a resolution-failure count up front. |
| Migration crashes mid-run, leaving a partial mirror. | medium | Tool is idempotent (§5.3); re-runs converge. Source `human_survey` beads are never deleted in the same pass. |
| `score=0` sentinel for feedback-only rows confuses readers expecting `1..5`. | low | Type config relaxed to `>= 0` *only* when paired with `source=slack-feedback-only`; viewer suppresses the star rendering for `score=0`. |
| Beads3d dashboard goes stale between Phase 3 and Phase 6 because it still queries `type=human_survey`. | high (if Phase 2 slips after Phase 3) | Phase ordering enforces beads3d port *before* the bridge writes `rating`-only. The single-write cutover in Phase 6 is gated on the beads3d port being deployed. |
| T4a's existing tests reject the new optional fields. | low | Phase 0 amends T4a/T4a-2 with explicit unit tests for the optional fields before any bridge change lands. |
| Deleted Slack users (Slack `users.info` returns `user_not_found`) leak as sentinel-rater rows forever. | low | Acceptable; sentinel rows are queryable, do not break aggregates, and surface the user id verbatim if a human ever wants to backfill identity manually. |

**Rollback per phase:**

- Phase 1: flip `WriteRatingType=false`; behaviour reverts to `human_survey`-only.
- Phase 2: revert the viewer/3d MR; readers fall back to the existing `type=rating` shape.
- Phase 3: same as Phase 1 — flip the flag off.
- Phase 4: migrated rows are additive (`human_survey` rows still present); a faulty run is rolled back by listing rows whose `source` starts with `slack-backfill-` and bulk-closing them; a re-run produces them again from the still-live source.
- Phase 5: same shape as Phase 3 in prod.
- Phase 6: re-introduce the `human_survey` write path with a hotfix release; some Slack clicks during the gap have no `human_survey` row but do have a `rating` row — readers that still expect both will need adjusting (this is exactly why Phase 7 is one release later).
- Phase 7: irreversible only in the sense that the type registration is gone; data is unaffected.

---

## 9. Acceptance

- Bridge writes `type=rating` for every Slack click, with `source` set to `slack-stop` / `slack-manual` / `slack-feedback-only`, `score_native` preserved, `rates` edge to the agent bead.
- Manual-trigger (`@gardenio rate`, `:bar_chart:`) and feedback-modal paths produce `type=rating` rows; "Add feedback" follow-up updates the existing `rating`.
- `findExistingRating` lookup honours both email and `slack_user_id`; sentinel-then-resolved transitions update the same row.
- One-time migration tool is idempotent, supports `--dry-run`, and emits a per-row summary.
- `/ratings` page shows a `Source` column and a `?source=` filter; per-bead widget surfaces both star and native-scale aggregates.
- Beads3d ratings page reads `type=rating` filtered to Slack source; aggregate values match the pre-migration legacy values within the rounding error of the score mapping.
- After Phase 7, `kd list --type=human_survey` returns zero rows and the type registration is gone.
- `make test`, `make lint`, `quench check`, and `helm lint helm/gardenio/` all pass at every phase.
- New Playwright behavioural coverage: round-trip on the `/ratings` page for both `web` and `slack` source rows.

---

## 10. Implementation breakdown (proposed new T-tasks)

The epic [ka-0000007308](../../) is currently sequenced T1 → T2 → T3 → T4 → T5 → T6 → T7. F3 introduces a new branch between T4a and T6:

| Task | Title | Phase | Depends on |
|---|---|---|---|
| T4a-2 | Amend `type=rating` schema with optional fields (`score_native`, `source`, `slack_*`, `feedback`); relax `score>=0`. | Phase 0 | T4a |
| T5b | Bridge dual-write — exit-survey, manual-trigger, feedback-modal write `rating` behind `WriteRatingType` flag. | Phase 1 | T4a-2 |
| T5c | Beads-viewer renders `source`, `score_native`, dual-aggregate; new `?source=` filter. | Phase 2 | T4a-2 |
| T5d | Beads3d port — `ratings-page.js` queries `type=rating, source=slack`; fixture updates. | Phase 2 | T4a-2 |
| T5e | One-time migration tool `migrate-human-survey/` with `--dry-run` and idempotent re-run. | Phase 4 | T5b |
| T6 | Existing — Playwright e2e covers `/ratings` round-trip for `web` and `slack` rows. | Phase 2 | T5c, T5d |
| T7 | Existing — release. | Phase 3, 5 | T6 |
| T7b | Single-write cutover — flip `WriteRatingType=true`, drop dual-write code path. | Phase 6 | T7 + observability cycle |
| T7c | Drop `type:human_survey` registration; close leftover beads. | Phase 7 | T7b + observability cycle |

Implementation should not begin on T5b until T4a-2 has merged. T5e (the migration tool) can be drafted in parallel with T5b and T5c but must not be run until the dual-write window has produced enough rows for the migrator to test against in rwx.

---

## 11. References

- Decision: ka-0000007600 (chose F3).
- Research: `gardenio/docs/research/beads-viewer-human-ratings-slack-fold.md` (MR !1783).
- Predecessor decisions: ka-0000000502 (data model — `model-A`), ka-0000001726 (UX surface — `ux-I-wide`).
- T4a (in flight): ka-0000004715.
- Bridge code touched: `gardenio/controller/internal/bridge/bot_human_survey.go`, `bot_human_survey_manual.go`, `init.go`.
- Viewer code touched: `gardenio/controller/cmd/beads-viewer/ratings.go`, `templates/pages/ratings.html`, `templates/partials/rating_widget.html`.
- Beads3d code touched: `beads3d/src/ratings-page.js`, `beads3d/tests/ratings-page.spec.js`, `beads3d/tests/detail-panel-ratings.spec.js`.
- Migration tool (new): `gardenio/controller/cmd/migrate-human-survey/`.
