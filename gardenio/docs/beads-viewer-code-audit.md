# beads-viewer code audit (2026-10-31)

> **Task:** gd-R6phmQsoxD, phase 2 of plan gd-eRFLdnAEeB. **Rubric:**
> [`code-audit-rubric.md`](code-audit-rubric.md). **Reachability run:**
> [40721937722](https://github.com/mattbakerconsulting/gardenio/actions/runs/40721937722)
> (main @ `3e9e6fbba`, 2026-10-28; the crew token cannot re-dispatch it, gd-eOb1nyEaXC).
> **Audited at:** main @ `6b58fd20b`. **File splits:** gd-Wl6LuCnvlH.

`gardenio/controller/cmd/beads-viewer` is SHIPPED (`beads-viewer` image, `beadsViewer.enabled:
true` in k3s) and protected by **R2**: every page, route and template stays. This audit
only trims Go code behind them.

| package | Q1 for | Q2 reachable | Q3 on in prod | Q4 shim | Q5 dup | Q6 comment % | verdict | rule | notes |
|---|---|---|---|---|---|---|---|---|---|
| cmd/beads-viewer | the operator admin web UI (beads, plans, agents, feed, gardeners) | partly: 22 funcs test-only, plus 6 template methods and 2 exported methods only tests call | on; 10 dormant knob families (below) | yes, small: GitLab-only MR rail and coverage enrichment | small: inline-edit handler preamble ×10, three reactor loops | 32% | **keep, trim** | 1, 7 | this PR removes the dead parts; dormant families are **ask** |
| cmd/beads-viewer/viewer | per-request operator-id resolver for inbox and view telemetry | yes | on | no | no | — | **keep** | 8 | `WithTTL`/`WithClock`/`WithEmailExtractor` are test-only options; kept as idiomatic |

Size: 114,410 source lines and 166,138 test lines before, 114,105 and 166,011 after this
PR. Every route is unchanged: no handler, template or `RegisterRoutes` line was touched.

## Method

1. **deadcode list.** `dead/cmd-beads-viewer.tsv` names 22 beads-viewer functions; all 22
   are also in `test-only/`, so tests are their only callers.
2. **Template methods.** deadcode (RTA) treats every exported method of a type that reaches
   an interface as live, so it cannot see view-model methods that no template calls. A
   token scan of the package counted, for every func and method, its identifier uses in
   non-test Go, in tests, and as a word in `templates/**` (html, js, css). 95 names have no
   second use in non-test Go. Checked by hand: those implementing an interface from another
   package (`commons.Files`, `gardenhub` farm cost, `plans` seams, goldmark `ast.Node`,
   prometheus `Collector`) stay. The rest are template-only methods (kept) or listed below.
3. **Dormant knobs.** `helm template -f deploy-values/gardenio-k3s.yaml` of the
   beads-viewer Deployment, compared with every env var the package reads.
4. **Copy-paste.** `dupl -t 60` over non-test files; a family count of the rubric's prefixes.

## Done in this PR (low-risk trims)

**Deleted, no production caller:**

| symbol | file | note |
|---|---|---|
| `resolveOperatedByParamForViewer`, `resolveDefaultOperatedByForViewer`, `operatorIDsMatchViewerWithResolver` | `operator_chip_resolver.go` (whole file) | "future inbox" layer never wired; its test file went with it. `fakeAliasLookup` moved to `view_telemetry_test.go` |
| `reorderStep`, `reorderFailure`, `reorderPhase`, `reorderPhaseResolved` | `plan_edit.go`, `plan_phases.go` | wrappers over `internal/plans`; the pure tests moved to `internal/plans/fields_test.go` |
| `planTranscriptToolTurnBead` | `plan_transcript_spike.go` | test calls `plantranscript.ToolTurnBead` |
| `userHomeProfilePref`, `parseUserHomeProfilePref` | `user_home.go` | legacy alias; test calls `parseProfilePref` |
| `planBlockerInfo.Cleared`, `planCrewRollup.HasStuckTally`, `threadDetailView.HasTimeline`, `costsSummaryView.PrimaryBillHas` | `plans_blocked.go`, `plans_crew.go`, `threads_routes.go`, `costs.go` | no template or Go caller; the test assertions on them were dropped or rewritten against the fields |

**Moved to `test_seams_test.go`** (test-only compositions; their tests cover the
production functions they wrap, so the rubric's "rewrite against the production path"
applies, done with a test-local wrapper so 100+ call sites stay untouched):
`buildRoleHub`, `feedSeedCards`, `viewerVibeMode`, `auditThreadTimeline`,
`codeHighlighter.stats`, `registeredWidgets`, `renderFeedCard` (+ `renderFeedCardWithProject`),
`resetHtmxRendererCache`, `threadMembers`, `Server.SetNow`, `Server.MintHtmxWidgetToken`.
Doc comments that explained real behaviour moved onto the production function
(`threadMembersWithFieldSnapshot`, `renderFeedCardFull`).

## Dormant: built, not deployed (ask the owner)

k3s never sets these, so the code behind them never runs in prod. They are verdict **ask**:
no deletion until the owner says the feature has no planned use.

| knob(s) | code | size (non-test) | question |
|---|---|---:|---|
| `MEZMO_VIEWER_BASE_URL`, `MEZMO_VIEWER_SERVICE_KEY` | `widgets_mezmo.go`, `widgets_mezmo_http.go`; `noopMezmoClient` is what prod runs | ~715 | keep the live Mezmo widget feed, or reduce to the no-op client? |
| `LANDS_APPLY_ENABLED`, `LANDS_APPLY_*`, `LANDS_APPROVER` | `lands_apply.go`, `lands_apply_reactor.go` | ~1,220 | Lands Track B work in progress? (likely keep) |
| `LANDS_DEMO_*` | `lands_demo.go` | ~340 | demo land still planned? |
| `BEADS_VIEWER_PLAN_CRITIQUE_FINDINGS` | `plan_critique_findings.go` + branches in `plans.go`, `plans_critique.go` | ~450+ | turn on, or delete? |
| `GITLAB_API_TOKEN`, `GITLAB_BASE_URL` (Q4: gardenio left GitLab 2026-09-16) | `index_mr_status.go` MR rail, GitLab enrichment of `/coverage` drill-in | ~180 + branches | GitLab-only shim: port to GitHub or delete |
| `LOGIN_PICKER_*` | `login.go` picker | ~150 | keep (auth.clonk.tv work) or delete? |
| `OIDC_*`, `AVA_*`, `K8S_SA_*`, `GARDENIO_ZITI_*`, `ALLOWED_CIDRS`, `BEADS_VIEWER_EDGE_AUTH_HMAC_KEY` | `main.go` config + `middleware.go`; most logic lives in `internal/oidcauth`, `internal/edgeauth` | small here | auth modes for other deploy shapes; belongs to the auth audit |
| `BEADS_VIEWER_JIRA_*` | `linkify.go`, `jira_timeline.go`, `extern.go` | ~250 + branches | Jira linkify still wanted? |
| `GARDENIO_VIEWER_GARDEN_*_BANNER` / `_REDIRECT` | `migration_banner.go`, `garden_retire.go` | ~480 | **keep:** this is the R2 `?old=1` retire mechanism |
| `COVERAGE_PER_PROJECT_ENABLED`, `ESCALATION_REVIEW_URL` | `coverage_per_project.go`, escalation links | ~545 + branches | turn on or delete? |

## Copy-paste (Q5), for a follow-up trim

- **Inline-edit preamble**, ~10 handlers in `priority_edit.go`, `assignee_edit.go`,
  `owner_edit.go`, `title_edit.go`, `labels_edit.go`, `description_edit.go`,
  `comment_edit.go`: each repeats `PathValue("id")` → 400, `EmailFromContext` → 401,
  `GetBead` → `*LoadError` (49 copies of `"Missing bead id"` in the package). One
  `s.loadEditableBead(w, r, requireAuth, onErr)` helper saves ~100 lines.
- **Reactor loops:** `bead_feed_hub.go:121`, `plan_label_reactor.go:176`,
  `plan_step_reactor.go:145` share a ~25-line subscribe-and-dispatch loop.
- **Near-pairs:** `plans.go` `handlePlanCloseStep` / `handlePlanReopenStep`,
  `user_home.go:928`/`954`, `project_hub_sections.go:106`/`327`, `lands.go:521` /
  `window.go:839`, `feed.go:2670`/`2703` (each ~20-30 lines).
- **Not duplicates:** `humanizeTime` vs `formatAgo` and `shortLifetime` vs
  `shortDuration` render different formats; `firstNonEmpty` (bead_preview.go) and
  `firstNonEmptyField` (gardener_owner.go) differ by whitespace trimming. Folding them
  would change output.

dupl found no clone over 120 tokens beyond these, so copy-paste is not where the size is.
The size is in big single-purpose functions and data literals (`newTemplateAssets` 782
lines, `RegisterRoutes` 835, `planRowView` 427, `handlePlans` 360, `planIndexEnrichTimed`
535, `handleAgents` 403, `handleBeadShow` 385).

## Split plan for gd-Wl6LuCnvlH

Same package, move-only, one file per PR, each new file under 1,500 lines.

**plans.go (5,400)**
- `plans_view.go`: consts, `planRowView`, `planCardStep`, `planStepView` and their
  methods (lines 38-969, ~930).
- `plans_index.go`: `handlePlans`, `planProjectOptions`, `planIndexStepData`,
  `renderedStepRollup`, the `count*` and time helpers, `pick*CurrentOrNextStep`,
  `planCreatorChip*` (970-1416 and 1986-2676, ~1,140).
- `plans_index_enrich.go`: `planIndexEnrich`, `planIndexEnrichTimed` (1417-1985, ~570).
- `plans_detail.go`: `handlePlanDetail`, `planTimeline`, `enrichPlanStepBlockers`,
  `planProgress` and friends (2677-2924, 3881-4119, ~490).
- `plans_events.go`: `handlePlanEvents` through `renderPlanCrewFragment` (2925-3880, ~950).
- `plans_decisions.go`: `planDecisionBeads` through `planProse` (4120-4781, ~660).
- `plans.go` keeps the operator mutations (4782-5400, ~620).

**server.go (4,487)**
- `template_funcs.go`: `templateAssets`, `newTemplateAssets` (the FuncMap, ~800).
- `routes.go`: `RegisterRoutes` and `routeRegistrar` (~840).
- `index.go`: `indexStatsView` through `handleIndex` (2996-3538, ~540).
- `project_page.go`: `projectBeadView` filters and `handleProject` (2740-2995, 3539-3660, ~380).
- `bead_show.go` (exists): add `handleBeadShow`, `handleBundleShow`, `handleBeadDeps`,
  `similarViews`, `injectTypeView` (~600).
- `comments.go`: `commentView`, `renderCommentMarkdown*`, `linkifyComments*`,
  `handleBeadComments`, `handleAddComment`, `renderCommentError` (~250).
- `server.go` keeps `beadAPI`, `Server`, setters, `NewServer`, `render` (~1,000).

**feed.go (2,715)**
- `feed_collapse.go`: dedup and cluster collapsing (509-1167, ~660).
- `feed_squawk.go`: squawk and scheduler classifiers and their regexps (1509-2194, ~690).
- `feed_card.go`: `classifyFeedEvent` through `formatFeedAgo` (2195-2715, ~520).
- `feed.go` keeps the handlers, SSE, seed cache (~840).

**agents.go (2,149)**
- `agents_view.go`: UI prefs, `agentRowView` and the summary types (32-576, ~550).
- `agents_summary.go`: recent runs, fleet tile, chips, sparkline, wrap-up parsing and
  TLDR, produced MRs (578-1494, ~920).
- `agents.go` keeps `handleAgents`, terminal and screen helpers, `agentRowFromBeadCtx` (~650).

## Not covered

- **Unused struct fields** on view models (template-only reads hide them from vet and
  deadcode). A field-level template grep is the next cheapest cut.
- **Comment trim:** the package is at 32%, under the 35% flag, so no bulk comment pass.
