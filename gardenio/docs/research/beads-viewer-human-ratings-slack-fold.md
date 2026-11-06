# Beads-Viewer Human Ratings — fold in Slack `human_survey` mechanism

**Status:** Research note. T1b of epic [ka-0000007308](https://gitlab.com/example-org/gardenio/-/issues?search=ka-0000007308).
**Author:** `gardenio-more-work-to-add-around-this-f-6192` (crew, project:gardenio).
**Source:** Operator follow-up on the parent epic — *"fold in the existing rating mechanism that's present in slack, so ratings made in slack are visible in the ratings tab. do research for this now."*
**Predecessor docs:**
- T1 research: [`beads-viewer-human-ratings.md`](./beads-viewer-human-ratings.md) (recommended `type=rating` design)
- T3 plan-of-record: [`gardenio/plans/beads-viewer-human-ratings.md`](../../plans/beads-viewer-human-ratings.md) (data model, routes, view models)

No code in this task. Output is this note + a `gb decision` raising the design forks below.

---

## 1. Problem statement

After T1+T3 landed, the epic shipped a `type=rating` design (1–5 stars, optional axes, OIDC-email rater, `rates` dependency edge). Implementation of `type=rating` in kbeads is in flight under [T4a / ka-0000004715](https://gitlab.com/example-org/gardenio/-/issues?search=ka-0000004715).

**A separate rating mechanism already exists** in the Slack bridge and was missed by both T1 and T3. The `gb stop` exit-survey path posts a block-kit prompt (`"Was this agent helpful? −1 / 0 / +1"`) into the agent's Slack thread; clicks create a `type=human_survey` bead (`gardenio/controller/internal/bridge/bot_human_survey.go`). That data already drives the `beads3d` ratings page (`beads3d/src/ratings-page.js`) — it is not vapor.

Result: two parallel rating record types with different schemas, scales, identity, and edges. The new beads-viewer `/ratings` tab queries only `type=rating` (`gardenio/controller/cmd/beads-viewer/ratings.go:122`), so existing Slack ratings are invisible there.

This note inventories both, names the seams, and recommends one of three integration strategies. Per epic gate, no code changes are proposed here — the chosen path will be raised as a `gb decision` to unblock a future T4-tier child task.

---

## 2. Inventory

### 2.1 `type=human_survey` (Slack-side, today)

Origin: `gardenio/controller/internal/bridge/bot_human_survey.go` + `gardenio/controller/cmd/gb/human_survey.go`.

| Aspect | Value |
|---|---|
| Bead type | `human_survey` |
| Bead kind | `data` |
| Required fields | none enforced; in practice `rating`, `agent_bead_id`, `rater_slack_user`, `answered_at` are set on every write |
| Rating scale | configurable; default **−1 / 0 / +1** (`HumanSurveyConfig.MinRating = -1`, `RatingScaleMax = 1`) |
| Title convention | `Human survey: <agent_bead_id> rating=<n>` |
| Edge to subject | parent-child to **agent bead** (`AddDependency(newID, agentBeadID, "parent-child", …)`) |
| Labels | `agent:<agent-bead-id>`, `project:<project>` (mirrored from the agent bead) |
| Idempotence | one row per (`agent_bead_id`, `rater_slack_user`); button re-click updates in place |
| Rater identity | Slack user id (e.g. `U0OPERATOR1`) — not resolved to email |
| Free-text feedback | optional `feedback` field, captured via a Slack modal |
| Source provenance | `source` field: `"stop"` (exit prompt) or `"manual"` (manually-triggered prompt) |
| Trigger gate | dark-launched: `human-survey-config.enabled` defaults to `false`; some roles flip it on |
| Existing read surface | `beads3d/src/ratings-page.js` aggregates by role/project/schedule_id |

Field schema in `gardenio/controller/internal/bridge/init.go:253-265`:

```go
"type:human_survey": TypeConfig{
    Kind: "data",
    Fields: []FieldDef{
        {Name: "agent_bead_id",     Type: "string"},
        {Name: "slack_channel",     Type: "string"},
        {Name: "slack_thread_ts",   Type: "string"},
        {Name: "rater_slack_user",  Type: "string"},
        {Name: "rating",            Type: "integer"},
        {Name: "source",            Type: "string"},
        {Name: "answered_at",       Type: "string"},
        {Name: "feedback",          Type: "string"},
    },
},
```

Sample bead today (`ka-0000005073`):

```json
{
  "type": "human_survey",
  "title": "Human survey: ka-0000001442 rating=1",
  "fields": {
    "rating": 1, "source": "stop", "answered_at": "2026-04-28T17:56:07Z",
    "agent_bead_id": "ka-0000001442",
    "slack_channel": "C0AHE7C0E1G", "slack_thread_ts": "1777391466.478799",
    "rater_slack_user": "U0OPERATOR1"
  },
  "labels": ["agent:ka-0000001442", "project:gardenio"]
}
```

### 2.2 `type=rating` (beads-viewer, T3 plan-of-record)

Origin: `gardenio/controller/cmd/beads-viewer/ratings.go`. Type registration is the responsibility of T4a (ka-0000004715, in progress).

| Aspect | Value |
|---|---|
| Bead type | `rating` |
| Bead kind | `issue` (per T3 §2.1 it lives in the issue plane so it shows in standard list views) |
| Required fields | `rated_bead_id`, `score` |
| Rating scale | **1–5 stars** (`score` integer 1..5) |
| Title convention | `rating: <short-rated-title-or-id> · <score>★` |
| Edge to subject | `rates` dependency edge (rating → rated bead) |
| Labels | mirrored `project:<project>` |
| Idempotence | one row per (`rater`, `rated_bead_id`); update in place on re-rate |
| Rater identity | OIDC email (server-set from `oidcauth.EmailFromRequest`) |
| Free-text feedback | bead **description** as markdown rationale (not a `feedback` field) |
| Optional axes | JSON object: `correctness`/`scope`/`efficiency`/`communication`, each 1..5 |
| Optional tags | string[] |
| Source provenance | none today; implicit "beads-viewer web UI" |

### 2.3 The seams (where they don't line up)

| Seam | `human_survey` | `rating` |
|---|---|---|
| Type | `human_survey` | `rating` |
| Subject anchor | `agent_bead_id` (parent-child) | `rated_bead_id` (`rates` edge) |
| Subject scope | always the agent bead | any non-rating bead |
| Scale | −1 / 0 / +1 (configurable) | 1..5 |
| Rater | Slack user id | OIDC email |
| Free text | `feedback` field | bead description |
| Title prefix | `Human survey: …` | `rating: …` |
| Source | `source: stop|manual` | (none) |
| Display today | beads3d ratings page | beads-viewer `/ratings` tab |

Two genuinely different signals are encoded:

- `human_survey` is "**how was this agent session?**" — fast triage from the human reading the Slack thread. Designed for low-friction rapid taps; the −1 option exists specifically so operators can flag net-harmful sessions.
- `rating` is "**how good is this work output?**" — considered review against a stable target (an MR, a bead resolution, an artifact). Designed for axis-level critique with rationale.

Both deserve to live on the ratings tab. Neither subsumes the other.

---

## 3. Strategy options

Three integration paths. Each has a concrete implementation sketch (no code in this note — paths are sized for a follow-up T-task).

### Option F1 — Read-side adapter (recommended)

**Both bead types stay as-is on the write side.** `/ratings` and the per-bead widget read from both and present a unified view-model. Slack continues to write `human_survey`; beads-viewer continues to write `rating`. Beads3d already reads `human_survey` — no churn there.

**Data flow:**

```text
Slack click ──▶ bridge ──▶ human_survey bead  ┐
                                              ├─▶ /ratings list & per-bead widget
beads-viewer POST ──▶ rating bead ────────────┘
```

**Read path changes (size-of-task estimate, scoped here only — implementation is a separate T-task):**

1. `(*Server).handleRatings` issues two `ListBeadsFiltered` calls (`Types: ["rating"]` and `Types: ["human_survey"]`) and merges + sorts by `created_at`. Adds a small "Source" column to each row (`web` / `slack`).
2. New view-model adapter — `surveyRowView(bead)` — that renders a `human_survey` bead as a `ratingRowView`-shaped struct: `Score` is the raw integer; `Stars` becomes a thumbs glyph (`👍 / · / 👎`) instead of star-fill; `RatedBeadID` is `agent_bead_id`; `Rater` is the Slack user id (or display name once §3.2 below resolves it).
3. The `(*Server).buildRatingWidget` per-bead widget already walks `rates` reverse-deps — extend it to also walk parent-child reverse-deps and pull surveys when the bead has `type=agent`. Aggregate panes are kept *separate*: `Stars (1-5)` aggregate next to `Slack survey (-1..+1)` aggregate. Don't smash them into one mean.
4. Filters: `?source=` (`web` / `slack` / both) and the existing `?project=`, `?type=`, `?rater=` get OR'd across both sources where shapes align.

**Pros:**
- No data migration. No backfill. No risk to beads3d.
- Both authoring flows keep their natural ergonomics: low-friction Slack tap vs. considered web review.
- Decision-bead authority (ka-0000000502 chose `model-A`) is honoured; we are *not* changing the data model.
- Read-side change is small and testable in isolation (a Playwright round-trip per source).

**Cons:**
- Sort/pagination across two streams is awkward when totals diverge (we will end up doing local merge of two `Limit: ratingsListLimit/2` lists).
- Dual aggregates — readers must understand "two scales, two means". Mitigation: clear column headers and per-source histograms.
- `human_survey` will not appear in a rated bead's *incoming-edges* panel via the existing `rates` fanout (it has no `rates` edge). Mitigation: surface them via the per-bead widget's parent-child walk (§3 step 3 above), which already runs server-side.

### Option F2 — Write-side adapter on the bridge (migrate Slack writes)

The Slack bridge is taught to write **both** records on a click: a `human_survey` bead (kept for backward compat with beads3d) **and** a sibling `rating` bead (with `score`, `rated_bead_id=agent_bead_id`, `rater=<email-resolved-from-slack-id>`, and a `rates` edge). The viewer reads only `type=rating` as planned.

This requires:

1. A −1/0/+1 → 1..5 mapping (e.g. `-1→1`, `0→3`, `+1→5`). The mapping is lossy at the extremes but interpretable. `score` is required 1..5 in the type config so we cannot store the native value in `score`.
2. A new optional `score_native` field on `type=rating` to preserve the original Slack value. The plan-of-record does not currently allow this; it would need amendment.
3. Slack-user-id → OIDC-email resolution at write time (Slack `users.info` API call) so `rater` is an email. Failures fall back to `rater = slack:<user-id>` and are flagged in a `source` field.
4. A new `source` field on `type=rating` (`web` / `slack`) to preserve provenance.
5. Backfill for existing `human_survey` beads — either skip (lossy) or run a one-time backfill that creates corresponding `rating` beads with `source=slack-backfill`.

**Pros:**
- Single read query (`Types: ["rating"]`); one aggregation, one sort.
- Per-bead widget gets `human_survey` ratings "for free" via the `rates` reverse-dep walk that already exists.
- Long-term cleaner — only one canonical rating record, with `source` carrying provenance.

**Cons:**
- **Two records per click is duplicative state**, with the usual drift hazards: a re-click that updates one bead but not the other becomes a coherence bug. Idempotence keys must be tracked on both sides.
- Forces a scale conversion that loses information (`-1 → 1` is the *worst* star — but a Slack `-1` carries the explicit "worse than nothing" semantics that 1★ does not).
- Forces email resolution on the bridge for rater identity, adding a Slack API dependency to the rating-write hot path.
- Backfill is non-trivial: `human_survey` beads from before the new code path land have no `rates` edge and no `rater` email; either we skip, or we backfill and accept identity loss.
- Plan-of-record amendment (new `score_native`, new `source` field) requires re-opening ka-0000000502.

### Option F3 — Sunset `human_survey`, migrate Slack to write `rating`

Hard cutover: bridge stops writing `human_survey`, writes only `rating`. Existing `human_survey` beads are migrated (one-time) to `rating` beads. Beads3d's ratings page is updated to read `type=rating` instead of `type=human_survey`. The `human_survey` type is left registered but unused, or dropped.

**Pros:**
- One canonical type. Cleanest end state.
- No dual-aggregate UX confusion.

**Cons:**
- All the F2 cons, *plus* a hard breakage of an in-flight surface (beads3d) that is not part of this epic's scope.
- One-time migration touches every existing `human_survey` bead (~unknown count, many in the open list), with risk of identity loss for raters whose Slack id we cannot resolve to email.
- Coordination cost across two epics (this one + a beads3d follow-up).
- Highest-risk path; the one most likely to land tag churn.

### Comparison summary

| Criterion | F1 (read-side) | F2 (write-side dual) | F3 (sunset survey) |
|---|---|---|---|
| Lossless rating data | ✅ | ⚠️ (scale collapse) | ⚠️ (scale collapse) |
| No data migration | ✅ | ❌ (backfill needed) | ❌ (full migration) |
| Beads3d compatibility | ✅ (untouched) | ✅ | ❌ (must port) |
| Plan-of-record amendment | ❌ (none) | ⚠️ (`score_native`, `source` fields) | ⚠️ (and beads3d epic) |
| Identity resolution required | ❌ (display-only) | ✅ | ✅ |
| Implementation surface | beads-viewer only | bridge + beads-viewer | bridge + beads-viewer + beads3d |
| Reversibility | ✅ (read code) | ⚠️ (dual writes are sticky) | ❌ (migrated data) |
| **Total effort** | **small** | medium | large |

**Recommendation: F1 (read-side adapter).** It honours the existing decision (ka-0000000502 chose `model-A` `type=rating`), preserves the two semantically-distinct signals at full fidelity, leaves beads3d alone, and is the smallest reversible step. F2/F3 may be revisited later after we have run F1 long enough to know whether dual-aggregate UX confuses operators in practice.

---

## 4. Sub-decisions inside F1

If F1 is chosen, three sub-questions remain. None are blocking enough to need a separate decision bead — they should be answered in the implementation T-task — but they are called out here so the operator can override at decision time.

### 4.1 Rater identity rendering for Slack rows

The merged `/ratings` list shows a `Rater` column. `rating` rows have an OIDC email; `human_survey` rows have a Slack user id (`U0OPERATOR1`). Three options:

- **D1-a (recommended for F1):** Display Slack user id with a `slack:` prefix (`slack:U0OPERATOR1`). Zero infra work. Add a follow-up T-task to enrich with display name via Slack `users.info` (cached) when the bridge has a token.
- **D1-b:** Resolve to email via Slack `users.info` at write time on the bridge, and persist a new `rater_email` field on `human_survey`. Adds a Slack-API dependency to the rating-write hot path; requires `users:read.email` scope.
- **D1-c:** Resolve at *render* time in beads-viewer with a server-side cache. Avoids hot-path cost but puts a Slack-API dependency in beads-viewer (which currently has none).

D1-a now, D1-b later, is the cheap reversible path.

### 4.2 Per-bead widget surfacing of surveys

The `rating` widget on `bead_show.html` walks `rates` reverse-deps. `human_survey` beads have no `rates` edge — they are parent-child of the agent bead. Two options:

- **D2-a (recommended):** Walk parent-child reverse-deps additionally on `bead.Type == "agent"`. Survey rows render in the widget alongside ratings, with a clear "Slack" source badge. Cost: one extra reverse-dep call when rendering an agent bead show page.
- **D2-b:** Backfill `rates` edges retroactively on every existing `human_survey` (one-shot script + bridge change to add the edge on new writes). Less branching in the read path; one-time data migration.

D2-a keeps F1's "no data migration" property. D2-b may be worth doing later as a tidying-up if the parent-child walk becomes hot.

### 4.3 Aggregation on the per-bead widget

The widget today computes a single mean and 1..5 histogram from `rating` beads. With surveys folded in, the choice is:

- **D3-a (recommended):** Two independent aggregate panes: `Ratings: 4.2 ★ (n=7)` and `Slack surveys: +0.6 (n=12)`. No fake unification.
- **D3-b:** Normalize survey to 1..5 (`-1→1`, `0→3`, `+1→5`) and roll into a single mean. Loses fidelity; see F2 cons.

D3-a respects the two distinct signals and matches research §2.3.

---

## 5. Backfill strategy (under F1)

`human_survey` beads created before F1 ships are already-readable by F1 (the read-side merge is type-only — no schema migration). No backfill action is required for them to appear in `/ratings`.

The only backfill question is whether to retroactively add `rates` edges so `human_survey` beads also appear in the rated agent bead's incoming-edges panel. Under D2-a (parent-child walk in the widget), this is unnecessary. Under D2-b, it is a one-shot script.

**Recommendation:** No backfill in the F1 implementation T-task. Add an explicit "no backfill needed" line to the T-task acceptance criteria so a future agent does not invent one.

---

## 6. Things this note explicitly defers

- The `dark-launched` flag (`human-survey-config.enabled` default false) is a separate axis — F1 is independent of how widely surveys are turned on. The viewer just renders whatever is in the daemon.
- The Slack-user-id → email lookup (D1-b/D1-c) is treated as a future enrichment, not an F1 prerequisite.
- Folding **decisions** or **agent costs** into `/ratings` (other adjacent feedback signals named in T1 §2) is out of scope — those have their own tabs.
- iOS Gardenio parity is still out of scope per the parent epic.
- Promoting high-rated work into advice/training pipelines — out of scope; future epic.

---

## 7. Decision fork (to be raised via `gb decision`)

One decision, three options, each with `artifact_type=plan`:

- **F1** — Read-side adapter (recommended). Beads-viewer reads both `rating` and `human_survey` and renders unified rows + dual aggregates. No write-side changes; no data migration.
- **F2** — Write-side dual records on the bridge. Slack click writes both `human_survey` and `rating`, with `score` mapped from the −1/0/+1 scale and `rater` resolved to email. Plan-of-record amendments needed for `score_native` and `source` fields.
- **F3** — Sunset `human_survey`, migrate to `type=rating` only. Bridge writes only `rating`; existing `human_survey` beads migrated; beads3d updated.

Human picks one; chosen option's artifact (a fold-in plan) is then committed under `gardenio/plans/beads-viewer-human-ratings-slack-fold.md` as a follow-up task. Implementation is a new T-task slotted between T5 and T6 of the parent epic, *after* T4a (ka-0000004715) lands the `type=rating` registration.

---

## 8. References

- Source thread: Slack 2026-04-25 — Matt Baker, parent epic ka-0000007308.
- Operator follow-up: 2026-04-28 — *"fold in the existing rating mechanism that's present in slack"*.
- Slack-side rating: `gardenio/controller/internal/bridge/bot_human_survey.go`, `gardenio/controller/cmd/gb/human_survey.go`, type-config in `gardenio/controller/internal/bridge/init.go:253-265`.
- Beads-viewer ratings: `gardenio/controller/cmd/beads-viewer/ratings.go`, plan-of-record `gardenio/plans/beads-viewer-human-ratings.md`.
- Existing read surface for surveys: `beads3d/src/ratings-page.js`, `beads3d/tests/ratings-page.spec.js`.
- T4a (in-progress, type=rating registration): ka-0000004715.
- Predecessor decisions: ka-0000000502 (data model, chose `model-A`), ka-0000001726 (UX surface, chose wide).
