# Slack Greenhouse restyle: Phase 4 alert/sync post triage

Task gd-lq9aP7GcoQ (tracker gd-Z3Sgg8W5Ih, plan gd-XFizxz9y3Y). Companion to the
Phase 4 section of [`slack-greenhouse-restyle.md`](slack-greenhouse-restyle.md).

**Rule.** A post is themed when it lands in a Slack channel that a project can
own, so `themeFor(channel)` is meaningful. It is left as-is when it is operator
chrome (a channel-less operator notice, a bead, mail or another system's
comment) or when it is not a Slack surface at all.

## Themed

Watchers that are not `*Bot`s take an optional `ThemeFor ThemeResolver`
(`alert_theme.go`, wired from `(*Bot).ThemeResolver()` in `cmd/slack-bridge`);
nil renders Garden. A watcher that posts to two channels (agent thread + ops)
resolves the theme per target channel.

| Surface | Legacy | Key |
|---|---|---|
| `coverage_alert.go` header | `:warning:` | `warn` |
| `scif_gate_alert.go` Gate-1 opening, Gate-2 opening | `:lock:` | `review` (an approval/release request, not a refusal) |
| `scif_gate_alert.go` Gate-1 approved / denied | `:white_check_mark:` / `:no_entry:` | `ok` / `denied` |
| `model_fallback.go` flag header | `:warning:` | `warn` |
| `refusal_log.go` header, silent-fallback bullet | `:no_entry:`, `:warning:` | `denied`, `warn` |

## Left as operator-alert chrome

| Surface | Why |
|---|---|
| `gardener_liveness`, `gardener_pouring_stall`, `gardener_epic_reaper`, `trigger_check_liveness`, `trigger_dark_lane`, `trigger_credential_wedge`, `closed_enabled_drain`, `stale_revival_review_escalate`, `uncovered_open_lane_rollup`, `release_drought` (🚨 🪝 🧹 ⏫ 📋 ✅) | Sent through `TriggerNotifier(ctx, title, msg, "")` with no channel: the operator notice relay. The same string is also written to a bead description or notice bead, so there is no channel to theme and the text is read as plain data. |
| `sla_nudge_notify.go` (`:rotating_light:`, `:alarm_clock:`) | Posts to the escalation queue channel (operator queue), not a project channel. |
| `dark_agent_budget`, `gardener_gate_guard`, `gardener_chain` | Bead comments, CLI output and agent prompt text. |
| `jira_*` (✅ ❌ ⚠️ 👋) | Jira comments. Slack themes do not apply to another system. |
| `gitlab_*` (✅ ❌ 🔄 ⏳ 🚫 ⚙️ 🏃) | GitLab MR comments and descriptions, same reason. |

## Not triaged here (not alert/sync posts)

`squawk.go`, `comment_forward.go`, `chat.go`, `dm_delivery.go`,
`bot_unreleased.go`, `slack.go`, `bot.go` and `spawn_vector.go` still carry
legacy shortcodes. They are agent-thread forwards, DMs, command replies or card
chrome rather than automated alert/sync posts, so they belong to the
command/card wiring steps or a follow-up.
