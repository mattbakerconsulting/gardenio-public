# Legacy spec-less gardener manifest — derived run counts

> **Status: Historical snapshot (2026-06-15).** Point-in-time derived run-counts
> for legacy spec-less gardeners (backfill input). Regenerate with
> `gardenio/scripts/enumerate-legacy-gardeners.py`. System map:
> [`gardener-system-reference.md`](./gardener-system-reference.md). Kept for
> provenance.

**Task:** [ka-0000000351] · **Epic:** [ka-0000000380] (Backfill GardenerSpec engine blobs onto legacy spawn_agent gardeners) · **Generated:** 2026-06-15

> **READ-ONLY analysis.** Producing this manifest mutated no gardener bead. The
> actual backfill is the later, currently-blocked execute bead **ka-0000001186**.
> Regenerate this table any time with `gardenio/scripts/enumerate-legacy-gardeners.py`.

## What a "legacy spec-less gardener" is

A gardener bead — `role:gardener` by LABEL or by the `role` field — whose
`fields.gardener` GardenerSpec blob is empty/absent. The engine only bumps
`RunsSpawned`/`RunsCompleted` when `trig.Gardener != nil`
(`gardenio/controller/internal/bridge/gardener_evaluator.go`), so a spec-less
gardener never accumulates a native tally. beads-viewer therefore DERIVES the run
count at render time from the gardener's iteration agent beads (bug
[ka-0000005661]) — a display-only repair. This manifest is the authoritative
target list that drives the root-cause backfill.

## Method (mirrors the viewer exactly)

- **Target predicate.** `role:gardener` (label OR `role` field) AND no non-empty
  `gardener` blob. Enumerated programmatically across the daemon — NOT trusted from
  the epic's hand-listed ~21 examples.
- **Type scope.** `loadGardenerRows` (`gardeners.go:551`) queries BOTH `type=trigger`
  (modern) and `type=schedule` (legacy), by the label *and* by the `role` field,
  deduped by id. This manifest mirrors that union, so every gardener the viewer can
  render is accounted for.
- **Run-count derivation.** Identical to `deriveGardenerRunCount`
  (`gardeners_enrich.go:115`): union of the dual ownership keys `trigger_id` /
  `schedule_id` over `type=agent` iteration beads, taking **MAX (not SUM)** per key
  (legacy iterations carry exactly one key, so MAX yields the real count without
  double-counting a theoretical dual-keyed bead).
  - `runs_spawned` = total agent beads keyed to the gardener
  - `runs_completed` = those with `status=closed`
  - `last_agent_id` = the most-recently-created (`created_at`) keyed agent bead
- **Validation.** Dormant gardeners reproduce the epic's quoted counts exactly
  (ka-0000007400 = 94, ka-0000003150 = 55, ka-0000004625 = 107, ka-0000006590 = 97);
  active ones have grown as expected since the epic was written (ka-0000007301
  256 → 280, ka-0000005569 450 → 644). Two named examples (ka-0000004249,
  ka-0000007575) are `type=schedule`, surfaced in set B below.

## Totals

- **164** gardener beads total (120 `type=trigger` + 44 `type=schedule`); **79**
  carry a GardenerSpec blob.
- **85** spec-less gardeners: **41** `type=trigger` + **44** `type=schedule`.
- The acceptance criterion's "~21 expected" corresponds to the OPEN, gardenio-and-
  monorepo `type=trigger` `action=spawn_agent` subset; the full sweep surfaces more
  (closed iterations, schedule-typed, and `action=apply_formula`).

## Backfill scoping notes for the execute bead (ka-0000001186)

- **Set A (`type=trigger`, 41)** is the real backfill target — these still fire.
  40 are `action=spawn_agent`; **ka-0000001426** is `action=apply_formula` (still
  spec-less, flagged separately). **ka-0000007764** ("[scratch] cron create test")
  and **ka-0000008052** ("Schedule trigger: 23,53 * * * *") look like test/throwaway
  beads — confirm before backfilling. Beads with 0 derived runs
  (ka-0000000599, ka-0000001526, ka-0000001426, ka-0000005671, ka-0000000891,
  ka-0000007084, ka-0000005670, ka-0000001580, ka-0000008167, plus the two test
  beads) have never spawned an iteration — seed `runs_spawned=runs_completed=0`.
- **Set B (`type=schedule`, 44)** is the legacy dead primitive — `type=schedule`
  firing was removed in Phase 3b/D10 (only `type=trigger` beads fire now). All but
  three (ka-0000007575, ka-0000001897) are already closed. They are viewer-visible
  and still derive a tally, so they belong in the enumeration, but the execute bead
  should confirm each is genuinely inert before deciding whether to backfill or skip.

## A. type=trigger (backfill-execute scope, ka-0000001186)

| bead-id | type | gardener name | spawned | completed | last_agent_id | status | action | cron | project |
|---|---|---|--:|--:|---|---|---|---|---|
| ka-0000003783 | trigger | Triage-intake gardener: sweep orphaned/abandoned beads into n | 677 | 677 | ka-0000005912 | open | spawn_agent | `17,47 * * * *` | gardenio |
| ka-0000005569 | trigger | AWS-POC currency gardener (ka-0000005495) | 644 | 644 | ka-0000002188 | open | spawn_agent | `23,53 * * * *` | gardenio |
| ka-0000007301 | trigger | MR Relevance Gardener (epic ka-0000004957) | 280 | 280 | ka-0000001891 | open | spawn_agent | `11 5,11,17,23 * * *` | monorepo |
| ka-0000006629 | trigger | Fleet-liveness guard: alert when janitor/triage gardeners go  | 168 | 168 | ka-0000004015 | open | spawn_agent | `23 */2 * * *` | gardenio |
| ka-0000007744 | trigger | Beads-Viewer Info-Density Gardener: drive open molecules of k | 155 | 155 | ka-0000004649 | open | spawn_agent | `23,53 * * * *` | gardenio |
| ka-0000007624 | trigger | Resumer Watchdog (gardener-of-gardeners): revive stuck-but-unf | 139 | 139 | ka-0000004032 | open | spawn_agent | `17 * * * *` | gardenio |
| ka-0000004625 | trigger | Gardener for ka-0000006278 (Gardener: make gardenio.app the AVA | 107 | 107 | ka-0000006161 | open | spawn_agent | `7,37 * * * *` | gardenio |
| ka-0000006590 | trigger | Gardener for ka-0000007702 (PE-10133 / FSM-380 visit-instance | 97 | 97 | ka-0000000221 | open | spawn_agent | `12,42 * * * *` | monorepo |
| ka-0000007400 | trigger | Firehose Gardener: build push-based CW in parallel, verify pa | 94 | 94 | ka-0000001351 | open | spawn_agent | `7,37 * * * *` | monorepo |
| ka-0000007583 | trigger | Gardener: AWS POC litellm enablement (ka-0000004609) | 90 | 90 | ka-0000002482 | open | spawn_agent | `19,49 * * * *` | gardenio |
| ka-0000007403 | trigger | Gardener: drive LOL public-release follow-ups to done (epic k | 77 | 77 | ka-0000006236 | open | spawn_agent | `7,37 * * * *` | gardenio |
| ka-0000003150 | trigger | Soup-to-nuts gardener: Gardener Page (epic ka-0000001579) | 55 | 55 | ka-0000001396 | open | spawn_agent | `11,41 * * * *` | gardenio |
| ka-0000004603 | trigger | Coverage Gardener: re-anchor coverage/head-of-main + publish  | 39 | 39 | ka-0000001364 | open | spawn_agent | `23 8 * * *` | monorepo |
| ka-0000005727 | trigger | Gardener operated_by-multi-user: drive epic ka-0000004247 to  | 35 | 35 | ka-0000000110 | open | spawn_agent | `17,47 * * * *` | gardenio |
| ka-0000008171 | trigger | Gardener: Phase 3 kube/CRI freeze-thaw (ka-0000006959) | 25 | 25 | ka-0000003945 | closed | spawn_agent | `6,36 * * * *` | gardenio |
| ka-0000002589 | trigger | Gardener: dynamic-workflows pilot ka-0000006835 (sanitize gas | 17 | 17 | ka-0000005318 | open | spawn_agent | `7,37 * * * *` | gardenio |
| ka-0000008247 | trigger | deploy-mr Login & Data-Viewing Validation Gardener (epic ka-H | 16 | 16 | ka-0000006855 | open | spawn_agent | `13 13 * * *` | monorepo |
| ka-0000005478 | trigger | DEVOPS-312 Stacked-MR Gardener: drive epic ka-0000005938 (9-s | 15 | 15 | ka-0000004475 | open | spawn_agent | `17 11,13,15,17,19,21 * * 1-5` | monorepo |
| ka-0000001037 | trigger | Gardener: thread-cards impl epic ka-0000001048 | 13 | 13 | ka-0000005897 | closed | spawn_agent | `9,29,49 * * * *` | gardenio |
| ka-0000000498 | trigger | First Mate (gardener-of-gardeners): plan-trajectory supervisor | 12 | 12 | ka-0000006209 | open | spawn_agent | `41 */7 * * *` | gardenio |
| ka-0000000119 | trigger | Gardener: Phase 6 fast + interruptible freeze (<10s SLO) | 8 | 8 | ka-0000005002 | closed | spawn_agent | `11,41 * * * *` | gardenio |
| ka-0000006496 | trigger | Gardener: auto-link impl epic ka-0000000933 (M0-M5 ship) | 8 | 8 | ka-0000006308 | open | spawn_agent | `13,43 * * * *` | gardenio |
| ka-0000000775 | trigger | Gardener: LiteLLM-only cost estimation epic (ka-0000000758) | 7 | 7 | ka-0000000888 | closed | spawn_agent | `11,41 * * * *` | gardenio |
| ka-0000007826 | trigger | Gardener: URL-unification research epic (ka-0000006413) | 4 | 4 | ka-0000000740 | closed | spawn_agent | `7 * * * *` | gardenio |
| ka-0000008052 | trigger | Schedule trigger: 23,53 * * * * | 3 | 3 | ka-0000007325 | closed | spawn_agent | `23,53 * * * *` | gardenio |
| ka-0000002056 | trigger | OSS-Sanitization Audit Gardener: weekly internal-string + sec | 2 | 2 | ka-0000005705 | open | spawn_agent | `13 9 * * 1` | gardenio |
| ka-0000005781 | trigger | Weekly Gardener-Audit Roll-up: fail%/no-op% trends per captai | 2 | 2 | ka-0000001586 | open | spawn_agent | `11 8 * * 1` | gardenio |
| ka-0000000636 | trigger | customer-poc migration gardener (gardenio-rwx+ephemeral → new  | 2 | 2 | ka-0000007700 | open | spawn_agent | `17,47 * * * *` | gardenio |
| ka-0000007481 | trigger | Investigative-janitor gardener: verify-done / why-not-closed  | 2 | 2 | ka-0000002177 | open | spawn_agent | `11,41 * * * *` | gardenio |
| ka-0000001972 | trigger | Gardener: drive trigger-test coverage epic ka-0000004568 | 1 | 1 | ka-0000003695 | open | spawn_agent | `15 */3 * * *` | gardenio |
| ka-0000000196 | trigger | Inbox-densify shipping gardener (ka-0000000894) | 1 | 1 | ka-0000003659 | open | spawn_agent | `*/12 * * * *` | gardenio |
| ka-0000000599 | trigger | Activity-Feed Delivery Gardener (continuous): drive build epi | 0 | 0 | - | open | spawn_agent | `23 */2 * * *` | gardenio |
| ka-0000001526 | trigger | Gardener: CI-minute spend watchdog (monthly regression guard) | 0 | 0 | - | open | spawn_agent | `17 9 1 * *` | monorepo |
| ka-0000001426 | trigger | Continual gardener: Grafana GRADE Audit (daily, disabled unti | 0 | 0 | - | open | apply_formula | `23 7 * * *` | gardenio |
| ka-0000007764 | trigger | [scratch] cron create test | 0 | 0 | - | closed | spawn_agent | `0 9 * * *` | gardenio |
| ka-0000005671 | trigger | operated-by visibility epic gardener spawn (ka-0000006357) | 0 | 0 | - | open | spawn_agent | `-` | gardenio |
| ka-0000000891 | trigger | Gardenio gardener-perf: analyze pg_stat_statements on america- | 0 | 0 | - | open | spawn_agent | `37 * * * *` | gardenio |
| ka-0000007084 | trigger | Gardener: trigger-bead validation epic ka-0000008095 | 0 | 0 | - | open | spawn_agent | `17 */2 * * *` | gardenio |
| ka-0000005670 | trigger | Inbox Revamp Gardener: drive epic ka-0000002474 to prod + ope | 0 | 0 | - | open | spawn_agent | `17,47 * * * *` | gardenio |
| ka-0000001580 | trigger | DEVOPS-460 Gardener: hourly triage + dispatch for DevOps Supp | 0 | 0 | - | open | spawn_agent | `13 * * * *` | monorepo |
| ka-0000008167 | trigger | Gardenio Bug-Fix Gardener: drive open bug beads to MR + merge  | 0 | 0 | - | open | spawn_agent | `3,23,43 * * * *` | gardenio |

## B. type=schedule (legacy dead primitive)

| bead-id | type | gardener name | spawned | completed | last_agent_id | status | action | cron | project |
|---|---|---|--:|--:|---|---|---|---|---|
| ka-0000004432 | schedule | Gardenio Feature Delivery Gardener: drive open beads to MR + r | 748 | 748 | ka-0000007804 | closed | - | `2,17,32,47 * * * *` | gardenio |
| ka-0000000154 | schedule | Gardener: customer-poc AWS POC implementation (umbrella ka-uc | 425 | 425 | ka-0000002115 | closed | - | `19,49 * * * *` | gardenio |
| ka-0000004416 | schedule | Coverage Implementation+Monitoring Gardener: drive Phase-2 ep | 363 | 363 | ka-0000001614 | closed | - | `23 * * * *` | gardenio |
| ka-0000000663 | schedule | Monorepo Gardener: advance engineering bug tickets | 335 | 335 | ka-0000002561 | closed | - | `7,22,37,52 0-4,11-23 * * *` | monorepo |
| ka-0000003531 | schedule | Gardenio Log Monitor Gardener: inspect pods, file bugs, prosec | 315 | 315 | ka-0000000224 | closed | - | `23 * * * *` | gardenio |
| ka-0000000677 | schedule | Gardenio gardener-inbox-density: drive epic ka-0000005534 to r | 237 | 237 | ka-0000005915 | closed | - | `19,49 * * * *` | gardenio |
| ka-0000002931 | schedule | Beads-Viewer Gardener: single-threaded owner, UX enrichment + | 236 | 236 | ka-0000004368 | closed | - | `3,23,43 * * * *` | gardenio |
| ka-0000007431 | schedule | Per-User Implementation Gardener (Option B): drive operators  | 216 | 216 | ka-0000005338 | closed | - | `7,37 * * * *` | gardenio |
| ka-0000001667 | schedule | scan-vm Skill Gardener: drive epic ka-0000006453 from scaffol | 136 | 136 | ka-0000005479 | closed | - | `19,49 * * * *` | monorepo |
| ka-0000001052 | schedule | Code Coverage Gardener: improve MR !1000 (DEVOPS-312, RWX por | 113 | 113 | ka-0000001645 | closed | - | `13 11,13,15,17,19,21 * * 1-5` | monorepo |
| ka-0000001455 | schedule | Per-Project Coverage Gardener: drive research epic ka-3ag7iMQ | 106 | 106 | ka-0000000807 | closed | - | `47 * * * *` | gardenio |
| ka-0000003483 | schedule | Phase 3b Convergence Gardener: drive type=trigger absorbs typ | 77 | 77 | ka-0000007964 | closed | - | `7 * * * *` | gardenio |
| ka-0000007761 | schedule | Monorepo Gardener-Observability: fix Grafana dashboards and c | 76 | 76 | ka-0000001919 | closed | - | `13 8-18 * * 1-5` | monorepo |
| ka-0000005515 | schedule | Gardener Free-Htmx Widget: drive epic ka-0000006879 from rese | 69 | 69 | ka-0000005017 | closed | - | `17,47 * * * *` | gardenio |
| ka-0000005678 | schedule | Gardener: centralized observability backplane (epic ka-CLEbbs | 45 | 45 | ka-0000004537 | closed | - | `17,47 * * * *` | gardenio |
| ka-0000003132 | schedule | DEVOPS-460 Gardener: hourly triage + dispatch for DevOps Supp | 41 | 41 | ka-0000005659 | closed | - | `13 * * * *` | monorepo |
| ka-0000003361 | schedule | Gardenio Gardener Weekend (off-peak promo) | 41 | 41 | ka-0000007705 | closed | - | `7,37 8-20 * * 0,6` | gardenio |
| ka-0000001017 | schedule | Bug Tracker Update: refresh Confluence page (every 2h) | 38 | 38 | ka-0000006397 | closed | - | `17 */2 * * *` | monorepo |
| ka-0000000222 | schedule | Beads-Viewer Umbrella Gardener: ka-0000002474 primary + 6 sec | 29 | 29 | ka-0000004741 | closed | - | `11,31,51 * * * *` | gardenio |
| ka-0000007575 | schedule | Coverage Gardener: keep MR !1000 fresh (re-anchor) + raise co | 22 | 22 | ka-0000001665 | open | - | `23 * * * *` | monorepo |
| ka-0000007738 | schedule | Inbox Revamp Gardener: drive epic ka-0000002474 to prod + ope | 11 | 11 | ka-0000002298 | closed | - | `17,47 * * * *` | gardenio |
| ka-0000006273 | schedule | Escalation-beads Design Gardener: drive inbox-constellation d | 9 | 9 | ka-0000001198 | closed | - | `47 * * * *` | gardenio |
| ka-0000004177 | schedule | Gardener Trigger-Bead Validation (LEGACY-SCHEDULED): drive ep | 8 | 8 | ka-0000007620 | closed | - | `23 */2 * * *` | gardenio |
| ka-0000006391 | schedule | Monorepo Gardener Weekend (off-peak promo, expires Mar 27) | 7 | 7 | ka-0000006192 | closed | - | `7,37 8-20 * * 0,6` | monorepo |
| ka-0000003200 | schedule | Operated_By Multi-User Readiness Gardener: drive epic ka-xLPZ | 6 | 6 | ka-0000005965 | closed | - | `19 * * * *` | gardenio |
| ka-0000000918 | schedule | Stage-B Free-Htmx Gardener: drive epic ka-0000001505 (rendere | 6 | 6 | ka-0000006964 | closed | - | `7,37 * * * *` | gardenio |
| ka-0000001185 | schedule | User-Mgmt & Attribution Research Gardener: drive epic ka-dHRc | 5 | 5 | ka-0000003572 | closed | - | `13,43 * * * *` | gardenio |
| ka-0000002602 | schedule | Gardener: auto-link incoming work beads epic ka-0000003873 | 4 | 4 | ka-0000002539 | closed | - | `17,47 * * * *` | gardenio |
| ka-0000006076 | schedule | Gardener: S3 images epic ka-0000001945 (e2e+unit tests) | 4 | 4 | ka-0000000795 | closed | - | `13,43 * * * *` | gardenio |
| ka-0000004249 | schedule | Gardener for ka-0000003108 (Research: better e2e coverage for | 3 | 3 | ka-0000000491 | closed | - | `7,37 * * * *` | gardenio |
| ka-0000002451 | schedule | Per-user homepage research gardener (epic ka-0000003855) | 3 | 3 | ka-0000008002 | closed | - | `23 */2 * * *` | gardenio |
| ka-0000005593 | schedule | Tree-hugger gardener schedule (epic ka-0000004697) | 3 | 3 | ka-0000001105 | closed | - | `13,43 * * * *` | monorepo |
| ka-0000001829 | schedule | Gardener for ka-0000004118 (Direct GitLab→coopmux: skip Keycl | 2 | 2 | ka-0000004736 | closed | - | `7,37 * * * *` | gardenio |
| ka-0000004941 | schedule | Gardener for ka-0000002532 (beads-viewer image rendering: fix | 2 | 2 | ka-0000007264 | closed | - | `7,37 * * * *` | gardenio |
| ka-0000002629 | schedule | Operated_by Option B Rollout Gardener: drive P1-P7 ADR phases | 2 | 2 | ka-0000003371 | closed | - | `17 */4 * * *` | gardenio |
| ka-0000001140 | schedule | E2E Gardener Weekend (off-peak promo, expires Mar 27) | 2 | 2 | ka-0000004090 | closed | - | `23 10,14,18,22 * * 0,6` | monorepo |
| ka-0000007468 | schedule | Coverage Gardener Weekend (off-peak promo, expires Mar 27) | 2 | 2 | ka-0000002914 | closed | - | `13 10,14,18 * * 0,6` | monorepo |
| ka-0000002091 | schedule | Tree-hugger v2 platform-hardening gardener (epic ka-zcDSl9Pa1 | 1 | 1 | ka-0000002942 | closed | - | `7,37 * * * *` | gardenio |
| ka-0000006924 | schedule | Gardener for ka-0000006835 (Pilot: dynamic workflows + beads  | 0 | 0 | - | closed | - | `7,37 * * * *` | gardenio |
| ka-0000001897 | schedule | E2E Coverage-Sprint Gardener (ka-0000000958) | 0 | 0 | - | open | - | `7,37 * * * *` | gardenio |
| ka-0000007163 | schedule | Phase 2 Triggers UI Gardener: drive first-class triggers in b | 0 | 0 | - | closed | - | `17 * * * *` | gardenio |
| ka-0000002336 | schedule | Gardener: Formulas-to-full-potential (ka-0000007507) | 0 | 0 | - | closed | - | `23,53 * * * *` | gardenio |
| ka-0000002592 | schedule | Gardenio Bug-Fix Gardener: drive open bug beads to MR + merge  | 0 | 0 | - | closed | - | `3,23,43 * * * *` | gardenio |
| ka-0000004053 | schedule | Gardener Incident Intelligence: drive epic ka-0000003459 from | 0 | 0 | - | closed | - | `13,43 * * * *` | gardenio |

