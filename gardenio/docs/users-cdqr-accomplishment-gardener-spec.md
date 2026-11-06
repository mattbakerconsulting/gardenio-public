# Per-User Homepage (`/u/{slug}`) CDQR+A Gardener — Spec

**Tracking epic:** ka-0000000581
**Formula:** ka-0000007015 (`Users-Page CDQR+A Audit`)
**Schedule trigger:** ka-0000004802 (`Users-Page CDQR+A Gardener`)
**Formula JSON (committed):** `gardenio/docs/users-cdqr-accomplishment-formula.json`
**Operator:** Matt Baker (`oidc:matthew.baker@acme.example`)
**Origin:** Slack — *"the 'Users' page for an individual user does a terrible job of
showing what they've actually done and such; create a gardener w a rubric and formula
to improve this w playwright screenshots and iteration as we've done w other things."*

---

## 1. Which page?

The **per-user homepage** served at **`GET /u/{slug}`**
(`gardenio/controller/cmd/beads-viewer/user_home.go`, template
`templates/user_home.html`) — the page you land on for an *individual* user.

> ⚠️ This is **not** the `/people` operator directory (handler `handlePeople`,
> `people.go`). That directory has its own gardener (people-directory CDQR,
> spec `people-directory-cdqr-gardener-spec.md`). Matt's phrase "for an individual
> user" pins this at `/u/{slug}`.

The homepage renders ~11 sections (`buildUserHomeSections`): pinned, inbox,
active-agents, open-decisions, recent-comments, open-mrs, recent-commits,
recent-merged, their-automations, their-gardeners, forwarded-mentions.

## 2. The complaint, grounded

*"Does a terrible job of showing what they've actually done."* The page leans
heavily on **in-flight** work (open decisions, active agents, open MRs) and
buries **completed** work: `recent-merged` and `recent-commits` are mid/low-page
lists with no aggregate, no recency framing, and no separation from the backlog.
A stranger landing on `/u/{slug}` cannot answer *"what has this person actually
accomplished, and recently?"* in five seconds. That is the gap this gardener drives.

## 3. Why a fresh gardener (not a revival)

A `/u/{slug}` CDQR gardener already existed (formula ka-0000000967 / epic
ka-0000007423) but was **dead**: no enabled schedule, and a botched `kd formula
apply` had left **190 orphaned step beads** with literal unexpanded
`{{fire_time_iso}}` titles and no parent. Those were closed; the old trigger
(ka-0000006082) was disabled and the old epic closed as superseded. The new
gardener pours via a `type=trigger` schedule (the proven gardener-page pattern,
ka-0000006085) so `{{fire_time_iso}}` actually expands at fire time.

## 4. The rubric — **CDQR + A**

Standard four CDQR dimensions plus one **headline** dimension that encodes the
operator's exact complaint, so the score cannot go green while accomplishments
are illegible.

| Dim | Name | Scores on `/u/{slug}` |
|---|---|---|
| **C** | Correctness | Counts/links accurate; no template leaks / 5xx; htmx sections resolve; rendered page matches DOM. |
| **D** | Density | Tufte signal-per-pixel: `near_white_ratio`, `data_fields`, `dom_element_count`. |
| **Q** | Quality | Alignment, hierarchy, card styling, empty-states, narrow-viewport layout. |
| **R** | Relevance | Does the page answer *"what is mine and what needs me, right now?"* — high-value sections above the fold, urgency-ordered. |
| **A** | **Accomplishment Legibility** | **Headline.** Can a stranger answer *"what has this person actually DONE, recently?"* in ~5s without scrolling? Scores on: is there a top-of-page done-work summary band (counts + recency windows)? Are done sections distinct from in-flight? Is any done signal above the fold? **Score ≤ 3 while there is no accomplishment summary and nothing done above the fold** — which drives the bug that fixes Matt's complaint. |

`overall.score` = mean of the five. Each dimension scoring **≤ 6** files one bug
bead parented to the epic with concrete recommendations + before-screenshot
evidence. The **A** bug is filed at priority 1 (others at 2) so it sorts to the
top of the epic.

### Objective metrics `measure` adds for A
- `accomplishment_summary_present` (bool) — top-of-page done-work aggregate band?
- `done_signal_fields` — rendered data points conveying *completed* work
  (recent-merged rows + recent-commits rows + closed/merged/shipped/resolved chips).
- `recency_windows_present` — list of time windows surfaced (`7d`/`14d`/`today`/…).
- `done_vs_inflight_separated` (bool) — done sections structurally distinct from backlog?
- `accomplishment_top_fold` (bool) — ≥1 done signal in the first ~900px of DOM order?

## 5. The gardener shape

Six-step pour `boot-and-warmup → capture → measure → judge → file-issues → report`,
modeled on the info-density audit (ka-0000005684) and gardener-page CDQR
(ka-0000005307). Fired by schedule trigger ka-0000004802:

- **cron** `37 8 * * *` UTC (daily), plus the preflight gate below.
- **preflight bead-query** (self-block, advice ka-0000000974): fires only when
  ≥1 open/in-progress/blocked `kind:users-cdqr` issue exists in `gardenio`.
  Closing epic ka-0000000581 is therefore the gardener's kill switch.
- `max_concurrent: 1`, `cooldown: 30m` — no same-cycle double-fire.
- **vars:** `TARGET_URL=http://localhost:8095`, `USER_SLUG=` (empty →
  boot auto-discovers the busiest live operator), `ITERATION={{fire_time_iso}}`,
  `PARENT_EPIC=ka-0000000581`.

## 6. Verified routing facts (2026-06-17, in-cluster viewer)

- User homepages serve at **`/u/<slug>`** at the service root; a `200` is the
  only proof a slug is valid.
- `/people` hrefs carry a **`/beads` reverse-proxy prefix**
  (`href="/beads/u/sam.rivers"`). Strip `/beads` before hitting
  `{{TARGET_URL}}` — `/beads/u/<slug>` 404s against the port-forward.
- Real human slugs are **dotted full names** (`sam.rivers`, `al.kent`).
  Guesses like `matt-baker` / `matthew.baker` 404. `op-<id>` slugs are thin
  operator sentinels. `/agents` rows render **no** per-user links — discover
  slugs from `/people`, ranked by live homepage signal.
- The agent pod reaches the viewer via `kubectl port-forward
  svc/gardenio-beads-viewer:8095` + a Keycloak `client_credentials` bearer
  (advice ka-0000006483, harness `gardenio/docs/info-density-audit-harness.md`).

## 7. Output per iteration

`docs/users-cdqr/{{ITERATION}}/` — `captures/{html,screenshots}/`, `metrics.json`,
`verdicts.json`, `REPORT.md` (5-row scorecard, A first) — landed on `main` via a
self-merged MR so future iterations can diff.
