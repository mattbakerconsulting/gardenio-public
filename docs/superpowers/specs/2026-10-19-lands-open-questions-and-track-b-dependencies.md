# Lands: open questions and Track B dependencies

Plan gd-vmw5s07A1F (Gardenio: Lands). Task gd-lyIZyN6wof (Phase 0, step 0d). Narrative: bundle
gd-uj7BxrMQsH, doc gd-NcaFgzREzt (draft 3). Written 2026-10-19.

This records two things Phase 0 owes the plan: one owner decision per open question, and the
exact steps of other plans that Track B (phases 7 to 10) waits on. Nothing here is decided by
this document; each question's answer is whatever the owner picks on its decision bead.

## 1. Open questions, one decision each

The authoritative list is the narrative's "Open questions for Matthew", items 1 to 8. Item 8
bundles five questions from the parity inventories (product definition gd-jXfrmBe6Yy section
8), so it is filed as five decisions. Every decision is filed with `--requesting-task
gd-lyIZyN6wof` and carries its background, the reasons for the recommended default, and what
changes per option in its context.

| # | Question | Decision | Options (recommended first) |
|---|---|---|---|
| 1 | Money | gd-l13fjWA0iW | **credits** (operator-set prepaid, no processor) · stripe |
| 2 | Identity | gd-2Yb3mGI6QM | **github-only** (Keycloak GitHub IdP, repo control via App install) · github-plus-email · email-only |
| 3 | Approval | gd-N7jCRnzwtz | **manual** (operator approves every plot) · auto-cap · invite-only |
| 4 | Isolation | gd-prDX5Xhr8n | **namespace-per-plot** (NetworkPolicy + quota + SA) · shared-restricted |
| 5 | Program stance | gd-p1yI8kaI4q | **unchanged** (showcase, few real tenants, no SLA) · track-a-first · product |
| 6 | Repo hosts | gd-PojrrBbCo4 | **github-only** · github-and-gitlab · any-git |
| 7 | Track A order | gd-Iy3tH8tL0y | **commons-first** (narrative order) · workbench-first (product definition) · interleaved |
| 8a | Slack's future | gd-78Tb2kbQrR | **permanent-mirror** (per land) · full-cutover |
| 8b | Reach | gd-t4lzcEOEFT | **web-push** (PWA + email digest, Slack DM fallback) · slack-dm-until-native · native-push |
| 8c | Spawn rights in a public land | gd-fPXoeHDQS3 | **land-role** (owner + member, not guest, within the purse) · owner-and-steward · per-channel |
| 8d | Glass for project humans | gd-kAgWFTwF3U | **operator-only** in v1 · scoped-read-only · scoped-redacted |
| 8e | `gb squawk` / `gb slack` vs `gb say` | gd-FuxIp9SMtV | **alias-forever** · scheduled-sweep |

The recommendations hang together: no payments and manual approval follow from an unchanged
program stance (5); GitHub-only identity (2) and repo hosts (6) share the GitHub App as the one
credential path; Slack as a permanent mirror (8a) is what the launch plan gd-CGO0GT7Wp6 asks
for ("emphasise the Slack integration") and also provides the DM fallback that makes web push
enough (8b).

### 1.1 Answers (all twelve resolved by the owner on 2026-10-19)

| # | Answer | Matches recommendation? |
|---|---|---|
| 1 | credits | yes |
| 2 | github-only | yes |
| 3 | manual | yes |
| 4 | namespace-per-plot | yes |
| 5 | **product** | no (recommended unchanged) |
| 6 | github-only | yes |
| 7 | commons-first | yes |
| 8a | permanent-mirror | yes |
| 8b | **native-push** | no (recommended web-push) |
| 8c | land-role | yes |
| 8d | operator-only | yes |
| 8e | alias-forever | yes |

Two answers differ from the recommendation, and each one changes what the plan must build:

- **Q5 = product.** Lands is now a product: tenants, an SLA, support and terms. The reasons given
  for Q1 (credits) and Q3 (manual approval) assumed the stance stayed unchanged. Both were
  answered as recommended anyway (Q1 credits, Q3 manual), so v1 is a product with prepaid,
  operator-set credits and no payment processor, and the operator approves every plot by hand.
  Stripe is therefore not v1 work; taking money is a later step of the product. What the
  product stance does add is uptime/SLA, support and terms work, which the plan does not have
  yet. The owner noted this on gd-vmw5s07A1F at 05:52Z.
- **Q8b = native-push.** The Commons does not take over decision and DM delivery from Slack
  until native push works. Web push is no longer enough. This is less new work than it sounds:
  an iOS Gardenio app exists (see `gardenio/docs/beads-viewer-ios-parity-phase2.md`), and
  `push-bridge` (`gardenio/controller/internal/bridge/push_notifier.go`, Helm `pushBridge`)
  already sends APNs alerts for decisions and escalations. It is disabled in the chart and has
  no credentials yet. What Commons III needs from it:
  1. Real APNs credentials (a `.p8` key secret, key id, team id, bundle id) and
     `pushBridge.enabled: true`. This is operator work (Apple developer account, a secret, a
     `deploy-values` change) and needs an escalation when Commons III is scheduled.
  2. Device registration per person. Today `/v1/devices` is the fleet's operator phones and
     every device receives every alert. Land members need devices keyed to the user, and
     alerts sent only to members of that land.
  3. Commons notification records (mentions, DMs, decision cards) routed to push-bridge. The
     storm roll-up and dedup policy applies here too.
  4. An app path that opens the Commons pane (deep link into `/garden`) and phone-answerable
     decision cards.
  Q8a = permanent-mirror means Slack DM remains a working fallback while this ships, so the
  Commons I and II order is unchanged.

Q8e = alias-forever: `gb squawk` and `gb slack *` stay documented aliases of `gb say`
permanently. No sweep task is filed. The Commons spec's line about a scheduled role-prompt
sweep is corrected in the same change as this section.

Not filed, on purpose: four product-definition questions that are design calls for the Phase 0
specs rather than owner forks. They go to the spec authors:

| Product definition §8 | Owner of the answer |
|---|---|
| Q3 conversation id: new bead type or the existing thread-head (wish) bead | 0a Commons spec (gd-dAsgFTJ2Se) |
| Q7 canvas World card with an embedded terminal, or the Glass as a page | 0b Workbench + Glass spec (gd-ywo8KxkQy6) |
| Q8 observability pages that must survive the DROP list | 0b Workbench + Glass spec (gd-ywo8KxkQy6) |
| Q9 audit sink for agent input (process log or audit bead) | 0b Workbench + Glass spec (gd-ywo8KxkQy6) |

## 2. Track B prerequisites: state on 2026-10-19

### gd-P78ygCUgA9 Keycloak at the public edge

Phases 1 and 2 are closed: the edge probes, Keycloak sign-in and the apps on public names
(including `mux.clonk.tv` behind the edge, S4b). Phase 3 "Cut over" (gd-RkrDwwtSwC) is open:

- S8 soak gd-Brrq3du7H5 is in progress, operator-run from the laptop; the dial count runs from
  2026-10-18T03:58:57Z, so 72 h ends around 2026-10-21T04:00Z.
- S9c gd-xNMLrsgajy (switch ziti off in the live stack) is open and waits on S8.
- S10 gd-K8d2mKf6Ey (stop depending on Vultr) is open and waits on S9c.
- Bug gd-RUhOpXWwmz (Grafana panels on `/costs` blank behind https) is open.

**Lands needs:** Phase 7 needs S9c, so the Keycloak edge is the only door into the stack before
registration opens. S10 and the Grafana bug are not needed by Track B.

### gd-507PTO0036 Project-scoped humans

Phases 0 and 1 are closed (fail-closed project scope in kbeads). The rest:

- Phase 2 gd-myIsTYHWX7 (realm groups and roles, no default role) is open, but its only task
  gd-zuV3EfwrzB closed when PR #373 merged on 2026-10-16. What is left is the operator rollout
  and closing the phase.
- Phase 3 gd-YF5fr57I4G (viewer reads as the user; edge admits project groups) is open, but all
  four of its tasks are closed.
- Phase 4 gd-tt1p2OMFFl: the Slack mapping task gd-9RrlPkQ8y3 is in progress, parked until the
  teammate confirms test decision gd-p3MzleZOnx. The negative matrix gd-6q0cRd9V7Q is closed.
  The live check gd-llFKMS8Pkc is open.
- Identity-audit bugs filed under the plan are still open: gd-rnklckVFKS (email_verified never
  checked), gd-W4FkF80BlX (Slack profile email grants capabilities), gd-fBBMiDHb07 (unmapped
  Jira commenter acts as the owner), gd-QV2WuUjely (an agent consents to a push as a human) and
  gd-b5drtT0ymT (any PR commenter's text reaches the owning agent).

**Lands needs:**

- Phase 7 needs Phase 2 (a self-registered user must hold no gardenio role).
- Phase 7 needs gd-rnklckVFKS. Self-registration is the exact precondition that bug names:
  identity must not rest on an unverified email.
- Phase 7 needs gd-llFKMS8Pkc, the live proof with a test user that the scope holds.
- Phase 8 needs Phase 2's group mechanism. See gap 1.
- Phase 10 needs gd-b5drtT0ymT (P1 once a repo is public, and tenants' repos may be public) and
  gd-QV2WuUjely (a tenant's agent must not be able to close a decision as its tenant).

### gd-MnQlEmbUq3 Per-viewer `/garden`

All five tasks are open and unassigned. `/garden*` is still `routeAdminOnly`:

- gd-l7yYxZpoZo reclassifies the routes.
- gd-6UG1OGlqYg cuts the farm document per viewer.
- gd-UKO0ZpXeha cuts the SSE patches per subscriber.
- gd-16UJtQh3Ua adds the leak tests.
- gd-GuZ3tbnTFI keeps writes admin-only.

**Lands needs:** Phase 8 needs all five, so a tenant lands on their own plot. The existing
epic-level edge gd-KiiqMwbZLu → gd-MnQlEmbUq3 is the right granularity and stays.

### gd-SAPwFE4FyW Per-project budget

All four tasks are open and unassigned:

- gd-9Tlqjf49Sm adds `budget_usd` and `budget_period` to the project bead.
- gd-T6zwDjWLBI computes `remaining_usd` in the hub.
- gd-GmJYMnSKGJ adds the ok/warn/over level and the period boundary.
- gd-jkBsCur9lO adds the tests.

The plan is explicitly display-only: "no spawn gating ... enforcement is a separate decision".

**Lands needs:** Phase 8 needs all four. It then adds the hard wall itself (refuse spawns and
park agents at `remaining_usd <= 0`). Phase 9 needs gd-T6zwDjWLBI, because the steward checks
the remaining budget before spawning. The epic-level edge gd-KiiqMwbZLu → gd-SAPwFE4FyW stays.

### gd-AbFYcF2BMr Agents as people + chat dock

Phases 1 to 3 are open and no task is closed:

- Phase 1 gd-FyB9OEPqcB has 1a to 1d, a check-in, and the owner's truck-ride feature
  gd-elQWxzYhdm.
- Phase 2 gd-CvrZY5cNf6 holds the reason model.
- Phase 3 gd-fjrpTC0pT3 holds the chat dock.

The epic already blocks Commons I (gd-KoOR5SPlNE).

**Lands needs:** Phase 9 needs three steps:

- gd-DKMSc1Y91I (1b): `gardeners[]` maps each agent to its plot, so the steward lives on its
  plot.
- gd-e4uXZs8coZ (1c): draws the gardeners there.
- gd-TvJosTKfAt (2a): the reason model, so "parked: purse empty" is a reason the World can show.

The Phase 3 chat dock is not needed: the steward talks in the Commons.

### gd-aR6UzS0HgM P9 Restricted agents

Five phases with ten tasks, all open, no code.

**Lands needs:**

- Phase 10 needs tiers: gd-Bcqj2r4m7E (`capability_tiers` on roles) and gd-Pcyz0ftcTg (the
  spawn gate and escalation authz read tiers).
- Phase 10 needs enforcement: gd-kLkxfyARXz (one shared restricted-ops check) and gd-hGfgjuviQy
  (untrusted-input taint on that same check; a stranger's repo is untrusted input).
- Phase 10 needs the ledger: gd-vvdOkF0bDL (the capability-grants ledger approvals write to).
- Phase 9 needs gd-Pcyz0ftcTg, because the steward spawns gardeners under a restricted tier.

Not needed:

- Grant expiry gd-cgywv7npBC.
- Ergonomics (Phase 4).
- The contributor-local role in Phase 5. Lands needs its own restricted role, which Phase 10
  models on gd-5UVkKGj9jJ.

### Also named by the narrative for Phase 10

- gd-ke6PC7bRto (kbeads stops accepting the shared static token) covers "no shared static token
  on the tenant path". It is in progress under P7 Phase 1.
- gd-mu3cEUKoAV (P7 Phase 3, public security notes) is the "gd-5f5bJPKLhC phase 3" the
  narrative cites.

## 3. Edges

Edges that already existed (thread agent, 2026-10-19):

| Lands phase | Blocked by |
|---|---|
| 7 Gate gd-pqJennUltH | gd-P78ygCUgA9 (epic), gd-507PTO0036 (epic) |
| 8 Deed and purse gd-KiiqMwbZLu | gd-MnQlEmbUq3 (epic), gd-SAPwFE4FyW (epic) |
| 9 Steward gd-G8lmByYEoM | none |
| 10 Fences gd-Gx6arHBvzJ | gd-aR6UzS0HgM (epic) |

Specific edges to add (`kd dep add <phase> <prerequisite> --type blocks`):

| Lands phase | Blocked by |
|---|---|
| 7 Gate gd-pqJennUltH | gd-xNMLrsgajy, gd-myIsTYHWX7, gd-rnklckVFKS, gd-llFKMS8Pkc |
| 8 Deed and purse gd-KiiqMwbZLu | gd-myIsTYHWX7 |
| 9 Steward gd-G8lmByYEoM | gd-DKMSc1Y91I, gd-e4uXZs8coZ, gd-TvJosTKfAt, gd-T6zwDjWLBI, gd-Pcyz0ftcTg |
| 10 Fences gd-Gx6arHBvzJ | gd-Bcqj2r4m7E, gd-Pcyz0ftcTg, gd-kLkxfyARXz, gd-hGfgjuviQy, gd-vvdOkF0bDL, gd-b5drtT0ymT, gd-QV2WuUjely, gd-ke6PC7bRto, gd-mu3cEUKoAV |

The phase epics were created by the thread agent, and kbeads only lets the creator, the
assignee, the owner or an admin edit their edges. So these edges are applied through an
escalation, not by this task's crew. The escalation id is in the plan comment on gd-vmw5s07A1F.

Recommended follow-up for the plan's owner (not in the escalation, because it removes edges):
once the specific edges exist, drop the epic-level edges gd-pqJennUltH → gd-P78ygCUgA9,
gd-pqJennUltH → gd-507PTO0036 and gd-Gx6arHBvzJ → gd-aR6UzS0HgM. As they stand, those edges
hold Track B behind work Lands does not need: Vultr retirement S10, the Grafana `/costs` bug,
the Jira and Slack-email identity bugs, restricted-agent ergonomics and the contributor-local
role.

## 4. Gaps found while pinning

1. **Provisioning a plot as code has no runtime group path.** Today a project group is a Helm
   list, `keycloak.projectGroups` (gd-zuV3EfwrzB, PR #373). Helm overrides replace lists, and
   changing the list is a deploy-values change. Phase 8's "project bead + `/project-<name>`
   group + membership" therefore needs a runtime path: the controller creating
   `KeycloakRealmGroup` objects, or the Keycloak admin API. That path needs RBAC, so it is an
   operator escalation. Phase 8's design must pick one.
2. **The steward needs the Commons.** It "greets tenants in `#general`" and posts digests, which
   needs Commons I (gd-KoOR5SPlNE) and `gb say`. Track B is described as parallel to Track A,
   but Phase 9 cannot finish before Commons I. The plan-gardener should add Phase 9 → Commons I
   when it sequences the phases. This document does not add that edge.
3. **Budget enforcement has no owner outside Lands.** gd-SAPwFE4FyW is display-only by its own
   text, so the hard wall (refusing spawns, parking agents, a margin for accounting lag) is
   entirely Phase 8's work. Phase 8 should not wait for another plan to build it.
