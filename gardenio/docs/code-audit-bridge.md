# Code audit: `gardenio/controller/internal/bridge`

Rubric: [`gardenio/docs/code-audit-rubric.md`](code-audit-rubric.md). Reachability run:
https://github.com/mattbakerconsulting/gardenio/actions/runs/40721937722 (2026-10-28; this
crew could not dispatch a fresh run, the token lacks `actions:write`, so every deletion
below was re-checked by grep against today's `main`). main @ `209f3abf9`. Task:
gd-Hhc0CEf98E. Owner ruling on the off-in-prod services: decision gd-rZJdGewUse
(`dead-only`, 2026-10-31).

| package | Q1 for | Q2 reachable | Q3 on in prod | Q4 shim | Q5 dup | Q6 comment % | verdict | rule | notes |
|---|---|---|---|---|---|---|---|---|---|
| internal/bridge | slack-bridge's Slack bot, decision and agent cards, trigger engine, gardeners; also linked into controller, gb, beads-viewer, grafana-bridge, trigger-engine | partly (671 funcs dead from every shipped main: 55 reached by no main at all, 297 only by off bridges, 319 only by e2e probes) | partly: Jira, GitLab, wasteland, alertmanager-bridge and push code is dormant (k3s sets no `GITLAB_API_TOKEN`, no `JIRA_BASE_URL`; those bridges are off) | yes: RWX metrics client and the `gardenio-rwx-tester` role seed (RWX left 2026-09-16) | yes: 10 `truncate*` helpers, 3 `firstNonEmpty*`, 3 `writeJSON*` | 29% | trim | 7 | per-file list below |

Size: 129,784 source + 207,010 test lines before this PR, 129,155 + 206,640 after.

## Deleted in this PR

Every item has no non-test caller on today's `main` (grep), and the reachability run
listed it as dead from every main, shipped or not.

- `Bot.fieldsFromPriorAgentBead` (wrapper; its tests now target `priorAgentBead`),
  `Bot.agentTaskTitle` (wrapper; tests now target `agentTask`).
- `formatAgentContextTurnsLegacy` and `legacyShortcodeTheme`: the migration to themed
  usage chips finished, nothing called the legacy form. The emoji ratchet in
  `theme_guard_test.go` drops the `bot_agents_usage.go` entry.
- `formatAttachmentsSection`, `formatFileSize` (bot_thread_forward.go).
- `ComposeChainHandoffNudge` (gardener_chain.go): the agent-side chain-handoff nudge was
  never wired.
- `MultiNotifier` (multi_notifier.go, whole file): nothing constructs one.
- `ImageToRepo`, `GitHubClient.CompareSHAs`, `GitHubClient.GetCommitForDigest`.
- `GitLabClient.CreatePipeline`, `GetMergeRequest`, `GetPipeline`, `ListMRDiscussions`,
  `ListPipelineJobs`, and the `GitLabDiscussion` type.
- `DefaultGardenioImageConfigs`, `ExtractImageTag` (unreleased.go).
- `GenerateWebhookToken`: `docs/triggers.md` now says `openssl rand -hex 16`.

Moved from production files into `export_test.go`, because only tests call them: the
thin wrappers `viaCommons`, `BeadUnbackedGardenerProse`, `BeadUnreachableRepoPaths`,
`GitLabWebhookHandler`, `GrafanaBridgeConfigKeys`, `NewGitLabTriggerSource`,
`expandIssueTemplate`, `matchConditions`, `rewriteFormBody`, `sanitizeTriggerAgentName`,
and the `memorySlackLinkStore` test fake. `SlackBridgeConfigKeys` stays in production
code because `cmd/slack-bridge` tests use it.

Kept on purpose: one-line test accessors on live types (`ClaudeStatusPoller.Verdict`,
`seenSet.len`, `ThreadTaskAutoCloser.Window`, `CompiledExpr.Source`).

## Handed to other steps

- **gd-Y4ZCGr2m2t** (deletes wl-bridge, alertmanager-bridge and scif-portal under
  gd-rZJdGewUse): the bridge files only those binaries use, `wasteland*.go` (4),
  `alert_*.go` except `alert_theme.go` (6), and `scif_gate_alert.go` /
  `scif_release_alert.go`, which are no-ops in k3s (`scifGateAlert.channel: ""`) and lose
  their producer with scif-portal.
- **gd-eoRWKciZec** (zero-reference symbols): `AllowlistGate.Mode`,
  `MRTerminalizeDriftReconciler.ScanOnce`, `WastelandPoller.TrackedCount` / `IsTracked`.
- **gd-97QPTzVoOK** (new): RWX leftovers. `rwx.go`, the RWX half of `cicd_metrics.go`,
  gitlab-bridge's `CICD_RWX_*` wiring, the `gardenio-rwx-tester` role seed in `init.go`
  / `init_prompts.go` / `formula_step_dispatch.go`, and the chart's RWX reaper and
  `values-rwx.yaml`.

## Kept by owner ruling, dormant in prod

gd-rZJdGewUse keeps jira-bridge, gitlab-bridge and push-bridge for now. Their code here
is 67 files and about 27k source lines (`jira_*` 37, `gitlab_*` 22, `trigger_source_jira`,
`trigger_source_gitlab`, `cicd_metrics`, `trustmetrics_adapter`, `release_drought*`,
`escalation_reaper`, `push_notifier`). slack-bridge links much of it, but k3s sets no
GitLab token and no Jira URL, so it does not run. The e2e probes (jira-conv,
jira-syncback, gitlab-pipeline-*, gitlab-prov, mr-reopen, escalation-reap) drive it, so
R1 holds it until a service is deleted. If Jira goes, these `JiraClient` methods already
have no production caller: `AddLabels`, `SetAffectsVersions`, `ReadOnly()`, and
`CreateOutboxIntent` (dormant by design).

## For Phase 4 (refactor, not cut here)

- Split the big files: `triggers.go` 4,936, `bot_agents.go` 3,431, `bot_mentions.go`
  3,256, `gitlab_sync.go` 1,942, `gardener_evaluator.go` 1,844, `bot.go` 1,842,
  `agentstream.go` 1,814, `bot_agent_kill.go` 1,762.
- Gardener second life: `gardenerGrantSecondLife` has no caller, so the revive path, its
  metric (`recordGardenerSecondLife`) and the `gardener-second-life-exhausted` label are
  unreachable. The `GardenerSpec` fields live in the kbeads schema too, so removing it
  needs its own change.
- Comments: 67 files are over 35% comment (about 19k source lines). The worst are
  `standing_gardener.go` 75%, `dark_gardener.go` 74%, `notification_config.go` 68%,
  `bot_cross_operator_notify.go` 67%, `model_switch_metrics.go` 65%.
- Duplicates (Q5): fold the 10 `truncate*` helpers onto one rune-safe `truncateRunes`.

## Per-file list

`dead` counts functions dead from every shipped main in the reachability run, split as
reached by no main / only by an off-in-prod bridge / only by an e2e probe. `tests` counts
the lines of `<file>_test.go` and `<file>_*_test.go`. Line counts are after this PR.

| file | src lines | comment % | test lines | dead (none/off/e2e) | verdict | rule | notes |
|---|---|---|---|---|---|---|---|
| advice_nudge.go | 189 | 14% | 336 | 0/0/0 | keep | 8 |  |
| agent_cards.go | 399 | 20% | 408 | 0/0/0 | keep | 8 |  |
| agent_notices.go | 179 | 22% | 213 | 0/0/0 | keep | 8 |  |
| agent_view_events.go | 100 | 49% | 0 | 0/0/0 | trim (comments) | 7 |  |
| agents.go | 354 | 28% | 3229 | 0/0/0 | keep | 8 |  |
| agentstream.go | 1814 | 27% | 2250 | 0/0/0 | keep | 8 |  |
| alert_loop.go | 196 | 20% | 166 | 0/10/0 | delete (gd-Y4ZCGr2m2t) | owner |  |
| alert_mute.go | 231 | 22% | 130 | 0/6/0 | delete (gd-Y4ZCGr2m2t) | owner |  |
| alert_route.go | 367 | 28% | 462 | 0/9/0 | delete (gd-Y4ZCGr2m2t) | owner |  |
| alert_rules_configmap.go | 138 | 28% | 106 | 0/2/0 | delete (gd-Y4ZCGr2m2t) | owner |  |
| alert_rules_render.go | 239 | 26% | 208 | 0/5/0 | delete (gd-Y4ZCGr2m2t) | owner |  |
| alert_rules_sync.go | 336 | 17% | 206 | 0/10/0 | delete (gd-Y4ZCGr2m2t) | owner |  |
| alert_theme.go | 33 | 27% | 151 | 0/0/0 | keep | 8 | theme for alert cards; shipped mains reach it |
| api_conversation.go | 434 | 16% | 0 | 0/0/0 | keep | 8 |  |
| api_conversation_caller.go | 329 | 22% | 422 | 0/0/0 | keep | 8 |  |
| api_conversation_dm.go | 226 | 12% | 0 | 0/0/0 | keep | 8 |  |
| api_conversation_say.go | 983 | 18% | 718 | 0/0/0 | keep | 8 |  |
| api_decisions.go | 359 | 11% | 520 | 0/0/0 | keep | 8 |  |
| api_decisions_sse.go | 127 | 10% | 0 | 0/0/0 | keep | 8 |  |
| api_escalations.go | 215 | 14% | 287 | 0/0/0 | keep | 8 |  |
| api_escalations_sse.go | 121 | 7% | 0 | 0/0/0 | keep | 8 |  |
| assistant_event_metrics.go | 151 | 48% | 117 | 0/0/0 | trim (comments) | 7 |  |
| bare_id_preview.go | 233 | 35% | 334 | 0/0/0 | keep | 8 |  |
| bead_display_name.go | 52 | 42% | 102 | 0/0/0 | trim (comments) | 7 |  |
| beadactivity.go | 250 | 25% | 505 | 0/0/0 | keep | 8 |  |
| bot.go | 1842 | 34% | 12477 | 0/0/0 | keep | 8 |  |
| bot_agent_death.go | 291 | 38% | 515 | 0/0/0 | trim (comments) | 7 |  |
| bot_agent_feedback.go | 206 | 28% | 213 | 0/0/0 | keep | 8 |  |
| bot_agent_kill.go | 1762 | 39% | 558 | 1/0/0 | trim (done here) | 7 |  |
| bot_agents.go | 3431 | 33% | 3311 | 1/0/0 | trim (done here) | 7 | 3,372 lines; agentTaskTitle deleted here |
| bot_agents_usage.go | 251 | 40% | 424 | 1/0/0 | trim (done here) | 7 |  |
| bot_agents_wrapup.go | 735 | 43% | 0 | 0/0/0 | trim (comments) | 7 |  |
| bot_assistant.go | 421 | 39% | 988 | 0/0/0 | trim (comments) | 7 |  |
| bot_autodestruct.go | 203 | 8% | 474 | 0/0/0 | keep | 8 |  |
| bot_beadactivity.go | 341 | 19% | 534 | 0/0/0 | keep | 8 |  |
| bot_channel_invite.go | 154 | 18% | 318 | 0/0/0 | keep | 8 |  |
| bot_channels.go | 637 | 11% | 1096 | 0/0/0 | keep | 8 |  |
| bot_channels_manage.go | 322 | 13% | 514 | 0/0/0 | keep | 8 |  |
| bot_codex_login.go | 164 | 11% | 211 | 0/0/0 | keep | 8 |  |
| bot_commands.go | 1462 | 16% | 846 | 0/0/0 | keep | 8 |  |
| bot_concierge.go | 283 | 12% | 346 | 0/0/0 | keep | 8 |  |
| bot_cross_operator_notify.go | 55 | 67% | 0 | 0/0/0 | trim (comments) | 7 |  |
| bot_decisions.go | 1050 | 16% | 1972 | 0/0/0 | keep | 8 |  |
| bot_decisions_dm.go | 105 | 52% | 326 | 0/0/0 | trim (comments) | 7 |  |
| bot_decisions_modal.go | 1031 | 19% | 1076 | 0/0/0 | keep | 8 |  |
| bot_decisions_operated_by.go | 87 | 49% | 117 | 0/0/0 | trim (comments) | 7 |  |
| bot_decisions_permission.go | 100 | 56% | 188 | 0/0/0 | trim (comments) | 7 |  |
| bot_dm.go | 208 | 18% | 310 | 0/0/0 | keep | 8 |  |
| bot_escalation_operator_dm.go | 50 | 42% | 0 | 0/0/0 | trim (comments) | 7 |  |
| bot_escalations.go | 828 | 18% | 579 | 0/0/0 | keep | 8 |  |
| bot_formula.go | 741 | 15% | 2563 | 0/0/0 | keep | 8 |  |
| bot_gate.go | 111 | 5% | 393 | 0/0/0 | keep | 8 |  |
| bot_group_invite.go | 362 | 24% | 344 | 0/0/0 | keep | 8 |  |
| bot_group_open.go | 264 | 22% | 253 | 0/0/0 | keep | 8 |  |
| bot_human_survey.go | 416 | 24% | 731 | 0/0/0 | keep | 8 |  |
| bot_human_survey_manual.go | 353 | 22% | 364 | 0/0/0 | keep | 8 |  |
| bot_human_survey_rating.go | 345 | 26% | 722 | 0/0/0 | keep | 8 |  |
| bot_kill_owner.go | 89 | 19% | 214 | 0/0/0 | keep | 8 |  |
| bot_lands_support.go | 65 | 12% | 0 | 0/0/0 | keep | 8 |  |
| bot_mention_summarize.go | 301 | 16% | 221 | 0/0/0 | keep | 8 |  |
| bot_mentions.go | 3256 | 34% | 4808 | 0/0/1 | keep | R1 | 3,256 lines; Slack mention router, live. parseQuietFlag reached only from e2e probes (R1) |
| bot_mentions_harness.go | 271 | 30% | 248 | 0/0/0 | keep | 8 |  |
| bot_mentions_model.go | 120 | 47% | 366 | 0/0/0 | trim (comments) | 7 |  |
| bot_message_subtype.go | 143 | 45% | 141 | 0/0/0 | trim (comments) | 7 |  |
| bot_notifications.go | 673 | 18% | 1163 | 0/0/0 | keep | 8 |  |
| bot_permission.go | 448 | 18% | 461 | 0/0/0 | keep | 8 |  |
| bot_permission_answerer.go | 151 | 23% | 369 | 0/0/0 | keep | 8 |  |
| bot_person_identity.go | 366 | 21% | 393 | 0/0/0 | keep | 8 |  |
| bot_reaction_trigger.go | 122 | 43% | 153 | 0/0/0 | trim (comments) | 7 |  |
| bot_reactions.go | 364 | 26% | 582 | 0/0/0 | keep | 8 |  |
| bot_reactions_lifecycle.go | 181 | 19% | 420 | 0/0/0 | keep | 8 |  |
| bot_reactions_wish_close.go | 234 | 49% | 334 | 0/0/0 | trim (comments) | 7 |  |
| bot_recover.go | 94 | 52% | 127 | 0/0/0 | trim (comments) | 7 |  |
| bot_reopen_debounce.go | 84 | 45% | 768 | 0/0/0 | trim (comments) | 7 |  |
| bot_runner.go | 334 | 16% | 214 | 0/0/0 | keep | 8 |  |
| bot_schedule_cmd.go | 424 | 15% | 357 | 0/0/0 | keep | 8 |  |
| bot_thread_forward.go | 671 | 37% | 1608 | 2/0/0 | trim (done here) | 7 |  |
| bot_thread_validate.go | 139 | 13% | 113 | 0/0/0 | keep | 8 |  |
| bot_threads.go | 421 | 20% | 380 | 0/0/0 | keep | 8 |  |
| bot_threads_dedup.go | 104 | 36% | 234 | 0/0/0 | trim (comments) | 7 |  |
| bot_trigger_cmd.go | 387 | 7% | 380 | 0/0/0 | keep | 8 |  |
| bot_unreleased.go | 242 | 6% | 260 | 0/0/0 | keep | 8 |  |
| bot_user_resolver.go | 62 | 11% | 0 | 0/0/0 | keep | 8 |  |
| bot_users.go | 230 | 10% | 135 | 0/0/0 | keep | 8 |  |
| canvas.go | 95 | 21% | 70 | 0/0/0 | keep | 8 |  |
| card_subject.go | 84 | 31% | 114 | 0/0/0 | keep | 8 |  |
| channel_stamp.go | 247 | 24% | 323 | 0/0/0 | keep | 8 |  |
| chat.go | 225 | 17% | 325 | 0/0/0 | keep | 8 |  |
| cicd_metrics.go | 808 | 24% | 485 | 0/22/0 | keep (owner, for now) | owner | GitLab + RWX pipeline metrics, gitlab-bridge only; RWX half goes with gd-97QPTzVoOK |
| claimed.go | 236 | 20% | 775 | 0/0/0 | keep | 8 |  |
| claude_status.go | 598 | 27% | 821 | 1/0/0 | keep | 8 | Verdict() is a test-only accessor; kept |
| claude_status_metrics.go | 137 | 33% | 0 | 0/0/0 | keep | 8 |  |
| closed_enabled_drain.go | 821 | 37% | 750 | 0/0/0 | trim (comments) | 7 |  |
| comment_forward.go | 293 | 23% | 390 | 0/0/0 | keep | 8 |  |
| commons_files.go | 203 | 18% | 274 | 0/0/0 | keep | 8 |  |
| commons_mirror.go | 1170 | 22% | 1280 | 1/0/0 | trim (done here) | 7 |  |
| commons_persona.go | 158 | 19% | 0 | 0/0/0 | keep | 8 |  |
| commons_spawn_rate.go | 97 | 26% | 138 | 0/0/0 | keep | 8 |  |
| controller_api_auth.go | 25 | 36% | 26 | 0/0/0 | trim (comments) | 7 |  |
| conversation.go | 242 | 25% | 200 | 0/0/0 | keep | 8 |  |
| cors.go | 24 | 21% | 82 | 0/0/0 | keep | 8 |  |
| coverage_alert.go | 231 | 18% | 176 | 0/0/0 | keep | 8 |  |
| cred_exhaustion_alert.go | 262 | 32% | 0 | 0/0/0 | keep | 8 |  |
| cross_operator_notify.go | 849 | 33% | 1276 | 0/0/0 | keep | 8 |  |
| dark_agent_budget.go | 193 | 39% | 330 | 0/0/0 | trim (comments) | 7 |  |
| dark_gardener.go | 47 | 74% | 0 | 0/0/0 | trim (comments) | 7 |  |
| dark_mr_cap.go | 260 | 44% | 484 | 0/0/0 | trim (comments) | 7 |  |
| dashboard.go | 1026 | 14% | 1201 | 0/0/0 | keep | 8 |  |
| decision_cards.go | 257 | 27% | 288 | 0/0/0 | keep | 8 |  |
| decision_message_mirrors.go | 161 | 32% | 249 | 0/0/0 | keep | 8 |  |
| decisions.go | 1373 | 33% | 4030 | 0/0/0 | keep | 8 |  |
| dedup.go | 232 | 16% | 311 | 0/0/0 | keep | 8 |  |
| dispatch_classifier.go | 330 | 37% | 268 | 0/0/0 | trim (comments) | 7 |  |
| dm_channels.go | 124 | 29% | 327 | 0/0/0 | keep | 8 |  |
| dm_delivery.go | 586 | 17% | 1475 | 0/0/0 | keep | 8 |  |
| dm_project.go | 55 | 31% | 101 | 0/0/0 | keep | 8 |  |
| dm_replies.go | 183 | 19% | 0 | 0/0/0 | keep | 8 |  |
| escalation_audience.go | 19 | 21% | 75 | 0/0/0 | keep | 8 |  |
| escalation_cards.go | 169 | 34% | 193 | 0/0/0 | keep | 8 |  |
| escalation_operator_dm.go | 520 | 29% | 546 | 0/0/0 | keep | 8 |  |
| escalation_pingback_loop.go | 300 | 26% | 555 | 0/0/0 | keep | 8 |  |
| escalation_reaper.go | 1016 | 41% | 1606 | 0/0/16 | keep (owner, for now) | owner | GitLab MR-merge reaper; k3s comment says the GitLab reaper is off. Driven by escalation-reap-probe (R1) |
| escalation_review_flag.go | 309 | 39% | 473 | 0/0/4 | trim (comments) | R1, 7 | 4 funcs reached only from escalation-reap-probe (R1) |
| escalation_staleness_digest.go | 336 | 27% | 278 | 0/0/0 | keep | 8 |  |
| escalations.go | 1209 | 23% | 1764 | 0/0/0 | keep | 8 |  |
| event_wake.go | 142 | 44% | 179 | 0/0/0 | trim (comments) | 7 |  |
| formula_instantiate.go | 329 | 26% | 526 | 0/0/0 | keep | 8 |  |
| formula_step_dispatch.go | 1012 | 42% | 1966 | 0/0/0 | trim (comments) | 7 |  |
| formula_step_dispatch_metrics.go | 432 | 58% | 356 | 0/0/0 | trim (comments) | 7 |  |
| gardener_block_on_mr.go | 250 | 46% | 531 | 0/0/0 | trim (comments) | 7 |  |
| gardener_bounds_guard.go | 289 | 43% | 333 | 0/0/0 | trim (comments) | 7 |  |
| gardener_chain.go | 216 | 33% | 498 | 1/0/0 | trim (done here) | 7 | ComposeChainHandoffNudge (never wired) deleted here |
| gardener_chain_spawn.go | 488 | 32% | 594 | 0/0/0 | keep | 8 |  |
| gardener_cron_gating.go | 193 | 53% | 193 | 0/0/0 | trim (comments) | 7 |  |
| gardener_d1.go | 192 | 51% | 233 | 0/0/0 | trim (comments) | 7 |  |
| gardener_d2.go | 173 | 57% | 95 | 0/0/0 | trim (comments) | 7 |  |
| gardener_d6.go | 296 | 42% | 282 | 0/0/0 | trim (comments) | 7 |  |
| gardener_dormancy_probe.go | 192 | 55% | 203 | 0/0/0 | trim (comments) | 7 |  |
| gardener_dormancy_probe_readonly.go | 129 | 18% | 71 | 0/0/0 | keep | 8 |  |
| gardener_dormant_sweep.go | 323 | 20% | 318 | 0/0/0 | keep | 8 |  |
| gardener_epic_reaper.go | 360 | 43% | 369 | 0/0/0 | trim (comments) | 7 |  |
| gardener_evaluator.go | 1844 | 49% | 2380 | 0/0/0 | trim (comments) | 7 |  |
| gardener_gate_guard.go | 109 | 39% | 225 | 1/0/0 | trim (done here) | 7 |  |
| gardener_hooks.go | 636 | 34% | 409 | 0/0/0 | keep | 8 |  |
| gardener_iteration_cache.go | 223 | 23% | 265 | 0/0/0 | keep | 8 |  |
| gardener_liveness.go | 638 | 37% | 853 | 0/0/0 | trim (comments) | 7 |  |
| gardener_operator_reopen.go | 165 | 45% | 359 | 0/0/0 | trim (comments) | 7 |  |
| gardener_pouring_stall.go | 608 | 32% | 711 | 0/0/0 | keep | 8 |  |
| gardener_probe.go | 71 | 59% | 144 | 0/0/0 | trim (comments) | 7 |  |
| gardener_resume_metrics.go | 141 | 48% | 83 | 1/0/0 | trim (Phase 4) | 7 | recordGardenerSecondLife reached only from the unwired second-life grant |
| gardener_second_life.go | 265 | 48% | 348 | 2/0/0 | trim (Phase 4) | 7 | grant path unwired (no caller of gardenerGrantSecondLife); schema fields live in kbeads. Phase 4 refactor, not cut here |
| gardener_selfblock.go | 216 | 42% | 164 | 0/0/0 | trim (comments) | 7 |  |
| gardener_standing_guard.go | 167 | 56% | 281 | 0/0/0 | trim (comments) | 7 |  |
| gate.go | 206 | 5% | 0 | 0/0/0 | keep | 8 |  |
| github.go | 354 | 12% | 507 | 1/0/0 | trim (done here) | 7 | GitHubClient for /unreleased. CompareSHAs, GetCommitForDigest, ImageToRepo deleted here |
| github_pr_url.go | 27 | 22% | 39 | 0/0/0 | keep | 8 |  |
| gitlab.go | 1277 | 23% | 2507 | 0/0/0 | keep (owner, for now), trimmed here | owner | GitLabClient. 5 uncalled methods deleted here. slack-bridge builds it only with GITLAB_API_TOKEN (unset in k3s) |
| gitlab_agent_resolver.go | 393 | 22% | 905 | 0/1/8 | keep (owner, for now) | owner |  |
| gitlab_ai_approval.go | 164 | 24% | 288 | 0/2/2 | keep (owner, for now) | owner |  |
| gitlab_branch_ownership.go | 526 | 26% | 455 | 0/0/0 | keep (owner, for now) | owner |  |
| gitlab_comment_mrlink.go | 174 | 25% | 216 | 0/4/0 | keep (owner, for now) | owner |  |
| gitlab_mention_catchup.go | 702 | 43% | 931 | 0/12/0 | keep (owner, for now) | owner |  |
| gitlab_mention_metrics.go | 38 | 53% | 24 | 0/0/1 | keep (owner, for now) | owner |  |
| gitlab_mentions.go | 1050 | 33% | 1923 | 0/1/27 | keep (owner, for now) | owner |  |
| gitlab_mr_backlink.go | 460 | 25% | 1060 | 0/0/9 | keep (owner, for now) | owner |  |
| gitlab_mr_backlink_keys.go | 131 | 42% | 0 | 0/0/5 | keep (owner, for now) | owner |  |
| gitlab_mr_reopen_drift.go | 340 | 28% | 326 | 0/1/7 | keep (owner, for now) | owner |  |
| gitlab_mr_schema_flag.go | 351 | 25% | 381 | 1/8/0 | keep (owner, for now) | owner |  |
| gitlab_mr_stats.go | 155 | 32% | 113 | 0/0/0 | keep (owner, for now) | owner |  |
| gitlab_mr_terminalize_drift.go | 290 | 29% | 289 | 1/5/0 | keep (owner, for now) | owner |  |
| gitlab_mrdesc.go | 184 | 16% | 249 | 0/0/7 | keep (owner, for now) | owner |  |
| gitlab_squawk_forward.go | 240 | 28% | 396 | 0/5/0 | keep (owner, for now) | owner |  |
| gitlab_sync.go | 1942 | 22% | 3688 | 1/4/23 | keep (owner, for now), trimmed here | owner | 1,952 lines. GitLabWebhookHandler moved to export_test.go here |
| gitlab_uploads.go | 215 | 33% | 365 | 0/0/0 | keep (owner, for now) | owner |  |
| gitlab_upstream_acceptance.go | 226 | 31% | 368 | 0/0/5 | keep (owner, for now) | owner |  |
| gitlab_upstream_poller.go | 210 | 31% | 303 | 0/0/4 | keep (owner, for now) | owner |  |
| gitlab_webhook_deadman.go | 285 | 49% | 537 | 0/4/0 | keep (owner, for now) | owner |  |
| gitlab_webhook_metrics.go | 102 | 57% | 385 | 0/1/1 | keep (owner, for now) | owner |  |
| grafana_backfill.go | 151 | 18% | 144 | 0/0/0 | keep | 8 |  |
| grafana_client.go | 178 | 24% | 160 | 0/0/0 | keep | 8 |  |
| grafana_configmap.go | 291 | 29% | 369 | 0/0/0 | keep | 8 |  |
| grafana_dashboard_store.go | 224 | 20% | 0 | 0/0/0 | keep | 8 |  |
| grafana_model.go | 80 | 34% | 67 | 0/0/0 | keep | 8 |  |
| grafana_readback.go | 395 | 22% | 304 | 0/0/0 | keep | 8 |  |
| grafana_sync.go | 230 | 30% | 614 | 0/0/0 | keep | 8 |  |
| heraldry_cards.go | 177 | 28% | 293 | 0/0/0 | keep | 8 |  |
| hookstream.go | 601 | 19% | 1307 | 0/0/0 | keep | 8 |  |
| human_operator.go | 215 | 37% | 124 | 0/0/0 | trim (comments) | 7 |  |
| inbound_action.go | 571 | 15% | 575 | 0/0/0 | keep | 8 |  |
| inbound_compose.go | 782 | 18% | 1155 | 0/0/0 | keep | 8 |  |
| inbound_react.go | 125 | 25% | 172 | 0/0/0 | keep | 8 |  |
| inbound_reply.go | 624 | 26% | 835 | 0/0/0 | keep | 8 |  |
| init.go | 1475 | 40% | 1675 | 0/0/0 | trim (comments) | 7 | seeds Jira/GitLab config beads and the gardenio-rwx-tester role (gd-97QPTzVoOK) |
| init_decision.go | 44 | 39% | 0 | 0/0/0 | trim (comments) | 7 |  |
| init_ensure.go | 213 | 26% | 0 | 2/0/0 | trim (done here) | 7 |  |
| init_message.go | 107 | 36% | 0 | 0/0/0 | trim (comments) | 7 |  |
| init_prompts.go | 689 | 25% | 636 | 0/0/0 | keep | 8 |  |
| init_types.go | 63 | 14% | 0 | 0/0/0 | keep | 8 |  |
| jacks.go | 245 | 23% | 402 | 0/0/0 | keep | 8 |  |
| jira.go | 1106 | 21% | 9491 | 0/2/4 | keep (owner, for now) | owner | JiraClient. AddLabels, SetAffectsVersions, ReadOnly() have no non-test caller but are pinned by the read-only/no-bypass tests; CreateOutboxIntent is dormant by design. Left while Jira is kept |
| jira_adf_markdown.go | 399 | 25% | 286 | 0/0/0 | keep (owner, for now) | owner |  |
| jira_age_metrics.go | 482 | 27% | 547 | 0/16/0 | keep (owner, for now) | owner |  |
| jira_agent_resolver.go | 455 | 37% | 1200 | 1/0/8 | keep (owner, for now) | owner |  |
| jira_allowlist.go | 380 | 41% | 1100 | 4/2/9 | keep (owner, for now) | owner |  |
| jira_attachments.go | 162 | 30% | 239 | 0/0/0 | keep (owner, for now) | owner |  |
| jira_attribution.go | 67 | 64% | 209 | 0/0/2 | keep (owner, for now) | owner |  |
| jira_backfill.go | 279 | 46% | 545 | 0/3/0 | keep (owner, for now) | owner |  |
| jira_backlog_pick.go | 404 | 46% | 349 | 0/0/0 | keep (owner, for now) | owner |  |
| jira_completion_ack.go | 313 | 41% | 515 | 0/7/0 | keep (owner, for now) | owner |  |
| jira_config.go | 319 | 11% | 453 | 0/12/0 | keep (owner, for now) | owner |  |
| jira_conversation_metrics.go | 319 | 44% | 380 | 0/1/9 | keep (owner, for now) | owner |  |
| jira_inbound_comment_dlq.go | 268 | 34% | 340 | 0/5/2 | keep (owner, for now) | owner |  |
| jira_inbound_comment_metrics.go | 113 | 51% | 0 | 0/0/0 | keep (owner, for now) | owner |  |
| jira_inbound_comment_retry.go | 53 | 38% | 237 | 0/0/1 | keep (owner, for now) | owner |  |
| jira_issue_read.go | 211 | 24% | 209 | 0/0/6 | keep (owner, for now) | owner |  |
| jira_mention_metrics.go | 47 | 62% | 65 | 0/0/1 | keep (owner, for now) | owner |  |
| jira_mentions.go | 1402 | 37% | 2874 | 0/0/34 | keep (owner, for now) | owner |  |
| jira_metrics.go | 334 | 27% | 626 | 0/11/0 | keep (owner, for now) | owner |  |
| jira_outbox.go | 443 | 56% | 436 | 0/0/1 | keep (owner, for now) | owner |  |
| jira_outbox_intents.go | 165 | 39% | 0 | 0/0/0 | keep (owner, for now) | owner | writer kept dormant by design (see jira_outbox.go) |
| jira_poller.go | 1340 | 22% | 1270 | 2/24/0 | keep (owner, for now) | owner |  |
| jira_poller_stall.go | 183 | 56% | 159 | 0/3/0 | keep (owner, for now) | owner |  |
| jira_project_gate.go | 181 | 51% | 136 | 2/0/2 | keep (owner, for now) | owner |  |
| jira_reply.go | 357 | 36% | 603 | 0/0/7 | keep (owner, for now) | owner |  |
| jira_reply_dedup.go | 107 | 38% | 153 | 0/0/5 | keep (owner, for now) | owner |  |
| jira_scope.go | 60 | 55% | 83 | 0/0/2 | keep (owner, for now) | owner |  |
| jira_spawn_ack.go | 203 | 49% | 320 | 0/0/4 | keep (owner, for now) | owner |  |
| jira_spawn_role.go | 95 | 33% | 239 | 0/0/2 | keep (owner, for now) | owner |  |
| jira_squawk_forward.go | 317 | 30% | 692 | 0/0/9 | keep (owner, for now) | owner |  |
| jira_status_transition.go | 344 | 33% | 412 | 0/0/8 | keep (owner, for now) | owner |  |
| jira_sync.go | 957 | 35% | 1637 | 0/3/20 | keep (owner, for now) | owner |  |
| jira_thread.go | 151 | 22% | 203 | 0/0/2 | keep (owner, for now) | owner |  |
| jira_wake_retry.go | 114 | 47% | 291 | 0/0/1 | keep (owner, for now) | owner |  |
| jira_webhook.go | 1109 | 46% | 2979 | 0/0/19 | keep (owner, for now) | owner |  |
| jira_webhook_deadman.go | 312 | 45% | 622 | 0/5/1 | keep (owner, for now) | owner |  |
| jira_write_policy.go | 408 | 45% | 831 | 1/1/14 | keep (owner, for now) | owner |  |
| keycloak.go | 474 | 10% | 222 | 0/0/0 | keep | 8 |  |
| lands_support.go | 444 | 16% | 311 | 0/0/0 | keep | 8 |  |
| link_unfurl.go | 350 | 29% | 809 | 0/0/0 | keep | 8 |  |
| mail.go | 112 | 20% | 84 | 0/0/0 | keep | 8 |  |
| mention_reasons.go | 94 | 15% | 258 | 0/0/0 | keep | 8 |  |
| model_fallback.go | 408 | 27% | 403 | 0/0/0 | keep | 8 |  |
| model_switch_metrics.go | 138 | 65% | 175 | 0/0/0 | trim (comments) | 7 |  |
| mr_dispatch_cooldown.go | 224 | 44% | 236 | 0/0/0 | trim (comments) | 7 |  |
| mr_dispatch_gate.go | 781 | 55% | 917 | 0/0/0 | trim (comments) | 7 |  |
| mr_peel_scaffold.go | 234 | 29% | 194 | 0/0/0 | keep | 8 |  |
| mr_relevance_gate.go | 125 | 38% | 139 | 0/0/0 | trim (comments) | 7 |  |
| mr_trickle_gate.go | 710 | 29% | 583 | 0/0/0 | keep | 8 |  |
| notice.go | 212 | 20% | 339 | 0/0/0 | keep | 8 |  |
| notification_config.go | 117 | 68% | 73 | 0/0/0 | trim (comments) | 7 |  |
| nudge.go | 553 | 31% | 510 | 0/0/0 | keep | 8 |  |
| operator_activity_metrics.go | 77 | 36% | 67 | 0/0/0 | trim (comments) | 7 |  |
| placement.go | 347 | 14% | 480 | 0/0/0 | keep | 8 |  |
| plan_badge_unfurl.go | 33 | 45% | 29 | 0/0/0 | trim (comments) | 7 |  |
| plan_map_link.go | 107 | 26% | 154 | 0/0/0 | keep | 8 |  |
| pronouns.go | 75 | 21% | 93 | 0/0/0 | keep | 8 |  |
| push_notifier.go | 407 | 25% | 356 | 0/16/0 | keep (owner, for now), trimmed here | owner | APNs; push-bridge only |
| refusal_log.go | 339 | 23% | 264 | 0/0/0 | keep | 8 |  |
| release_drought.go | 442 | 19% | 427 | 0/10/0 | keep (owner, for now) | owner | GitLab-tag release watchdog; reached only by off binaries |
| release_drought_metrics.go | 69 | 20% | 0 | 0/0/0 | keep (owner, for now) | owner |  |
| repoless_gitpath_guard.go | 223 | 45% | 312 | 1/0/0 | trim (done here) | 7 |  |
| respawn_phase_metrics.go | 145 | 56% | 159 | 0/0/0 | trim (comments) | 7 |  |
| retry.go | 111 | 31% | 0 | 0/0/0 | keep | 8 |  |
| rotation.go | 178 | 13% | 96 | 0/0/0 | keep | 8 |  |
| route_explain.go | 333 | 17% | 327 | 0/0/0 | keep | 8 |  |
| router.go | 221 | 22% | 176 | 0/0/0 | keep | 8 |  |
| rwx.go | 238 | 26% | 412 | 0/7/0 | delete (gd-97QPTzVoOK) | 5 | RWX left 2026-09-16; only gitlab-bridge cicd_metrics uses it. Dead even though gitlab-bridge stays |
| scif_gate_alert.go | 460 | 28% | 207 | 0/0/0 | delete (gd-Y4ZCGr2m2t) | owner | no-op in k3s (scifGateAlert.channel empty); its producer scif-portal is being deleted |
| scif_release_alert.go | 372 | 31% | 363 | 0/0/0 | delete (gd-Y4ZCGr2m2t) | owner | no-op in k3s; reads reports only scif-portal writes |
| security_gate.go | 382 | 31% | 407 | 0/0/0 | keep | 8 |  |
| seenset.go | 142 | 32% | 85 | 1/0/0 | keep | 8 | len() is a test-only accessor; kept |
| sla_nudge_metrics.go | 35 | 34% | 0 | 0/0/0 | keep | 8 |  |
| sla_nudge_notify.go | 89 | 10% | 0 | 0/0/0 | keep | 8 |  |
| sla_nudge_policy.go | 131 | 31% | 125 | 0/0/0 | keep | 8 |  |
| sla_nudge_sweep.go | 473 | 27% | 402 | 0/0/0 | keep | 8 |  |
| slack.go | 523 | 8% | 447 | 0/0/0 | keep | 8 |  |
| slack_event_dedup.go | 113 | 38% | 90 | 0/0/0 | trim (comments) | 7 |  |
| slack_link.go | 1053 | 16% | 1081 | 5/0/0 | trim (done here) | 7 | memorySlackLinkStore (test fake) moved to export_test.go |
| slack_link_hydrate.go | 641 | 17% | 522 | 0/0/0 | keep | 8 |  |
| slack_linkify.go | 362 | 25% | 371 | 1/0/0 | trim (done here) | 7 |  |
| slack_oidc_operator_alias_resync.go | 258 | 20% | 211 | 0/0/0 | keep | 8 |  |
| slack_operator_resync.go | 123 | 20% | 120 | 0/0/0 | keep | 8 |  |
| slash_thread.go | 46 | 33% | 40 | 0/0/0 | keep | 8 |  |
| spawn_vector.go | 256 | 23% | 209 | 0/0/0 | keep | 8 |  |
| squawk.go | 135 | 19% | 690 | 0/0/0 | keep | 8 |  |
| sse.go | 430 | 20% | 858 | 0/0/0 | keep | 8 |  |
| stale_revival_review_escalate.go | 316 | 40% | 289 | 0/0/0 | trim (comments) | 7 |  |
| standing_gardener.go | 83 | 75% | 0 | 0/0/0 | trim (comments) | 7 |  |
| state.go | 1085 | 23% | 693 | 0/0/0 | keep | 8 |  |
| subgraph_resolver.go | 288 | 23% | 331 | 0/0/0 | keep | 8 |  |
| surface_slack.go | 379 | 11% | 411 | 0/0/0 | keep | 8 |  |
| theme.go | 507 | 21% | 1064 | 0/0/0 | keep | 8 |  |
| theme_slash.go | 55 | 29% | 474 | 0/0/0 | keep | 8 |  |
| thread_anchor.go | 336 | 20% | 0 | 0/0/0 | keep | 8 |  |
| thread_bead.go | 212 | 48% | 527 | 0/0/0 | trim (comments) | 7 |  |
| thread_message_forward.go | 280 | 38% | 935 | 0/0/0 | trim (comments) | 7 |  |
| thread_reply_dlq.go | 261 | 24% | 364 | 0/0/0 | keep | 8 |  |
| thread_reply_metrics.go | 124 | 47% | 46 | 0/0/0 | trim (comments) | 7 |  |
| thread_task_autoclose.go | 1079 | 41% | 2663 | 1/0/0 | trim (comments) | 7 | Window() is a test-only accessor; kept |
| trigger_check_liveness.go | 213 | 44% | 286 | 0/0/0 | trim (comments) | 7 |  |
| trigger_credential_wedge.go | 349 | 34% | 347 | 0/0/0 | keep | 8 |  |
| trigger_dark_lane.go | 849 | 30% | 727 | 0/0/0 | keep | 8 |  |
| trigger_edge_events.go | 150 | 19% | 169 | 0/0/0 | keep | 8 |  |
| trigger_expr.go | 295 | 39% | 612 | 1/0/0 | trim (comments) | 7 | Source() is a test-only accessor; kept |
| trigger_expr_cache.go | 39 | 36% | 0 | 0/0/0 | trim (comments) | 7 |  |
| trigger_permissions.go | 256 | 21% | 1195 | 0/0/0 | keep | 8 |  |
| trigger_prompt_render.go | 63 | 27% | 57 | 0/0/0 | keep | 8 |  |
| trigger_source_account.go | 365 | 8% | 728 | 0/0/0 | keep | 8 |  |
| trigger_source_bead.go | 1311 | 31% | 2317 | 1/0/0 | trim (done here) | 7 |  |
| trigger_source_bead_reconcile.go | 292 | 26% | 289 | 0/0/0 | keep | 8 |  |
| trigger_source_gitlab.go | 576 | 28% | 1428 | 1/0/15 | keep (owner, for now), trimmed here | owner | NewGitLabTriggerSource moved to export_test.go |
| trigger_source_jira.go | 393 | 23% | 452 | 2/5/10 | keep (owner, for now), trimmed here | owner | expandIssueTemplate moved to export_test.go |
| trigger_source_multi.go | 535 | 27% | 914 | 0/0/0 | keep | 8 |  |
| trigger_source_schedule.go | 800 | 36% | 1887 | 0/0/0 | trim (comments) | 7 |  |
| trigger_source_schedule_mirror.go | 327 | 20% | 370 | 0/0/0 | keep | 8 |  |
| trigger_source_slack_reaction.go | 376 | 22% | 1004 | 0/0/0 | keep | 8 |  |
| trigger_source_webhook.go | 675 | 18% | 1557 | 1/0/0 | trim (done here) | 7 |  |
| trigger_starve.go | 249 | 38% | 351 | 0/0/0 | trim (comments) | 7 |  |
| trigger_sweep_roster.go | 63 | 35% | 56 | 0/0/0 | keep | 8 |  |
| trigger_watched.go | 152 | 30% | 172 | 0/0/0 | keep | 8 |  |
| triggers.go | 4936 | 45% | 4306 | 1/0/0 | trim (done here) | 7 | 4,935 lines; split is Phase 4. sanitizeTriggerAgentName moved to export_test.go |
| trustmetrics_adapter.go | 227 | 16% | 0 | 0/12/0 | keep (owner, for now) | owner | adapts the dark GitLabClient; reached only by off binaries |
| uncovered_open_lane_rollup.go | 460 | 30% | 277 | 0/0/0 | keep | 8 |  |
| unreleased.go | 334 | 13% | 723 | 2/0/0 | trim (done here) | 7 | DefaultGardenioImageConfigs, ExtractImageTag deleted here |
| user_directory.go | 523 | 20% | 990 | 0/0/0 | keep | 8 |  |
| vibe_mode.go | 186 | 35% | 551 | 0/0/0 | keep | 8 |  |
| wasteland.go | 440 | 17% | 124 | 0/17/0 | delete (gd-Y4ZCGr2m2t) | owner |  |
| wasteland_join.go | 258 | 13% | 0 | 0/5/0 | delete (gd-Y4ZCGr2m2t) | owner |  |
| wasteland_poller.go | 301 | 10% | 0 | 2/8/0 | delete (gd-Y4ZCGr2m2t) | owner |  |
| wasteland_sync.go | 289 | 25% | 319 | 0/9/0 | delete (gd-Y4ZCGr2m2t) | owner |  |
| web.go | 18 | 17% | 0 | 0/0/0 | keep | 8 |  |
| webhook_latency.go | 105 | 64% | 297 | 0/1/1 | trim (comments) | R1, 7 | observeWebhookHandling reached only from off bridges / probes |
