# Heraldry round 1: arms proposed for 47 real beads

> **History.** This records the first form of arms: one to five emoji placed like the five on a
> die on a shield of one tincture. The owner's verdict of 2026-10-26 20:45Z (thread gd-lvbRHMaXID)
> replaced it with three to five emoji in a row, shown the same way everywhere. Kept as history,
> not guidance: for the current form see [`README.md`](README.md) and [`rubric.md`](rubric.md).

Phase 0 step 0.2 (gd-2XAx90BUGC) of the heraldry plan gd-N6I3Oa7uDg. Spec:
[`2026-10-25-heraldry-design.md`](../superpowers/specs/2026-10-25-heraldry-design.md) §10 step 1.
Step 0.3 (gd-NF8PUEH9K4) turns these into the round-1 sheet for the owner. Nothing here is
stored on a bead yet. The accepted arms go onto their beads in step 1.6, by hand.

| File | What it is |
|---|---|
| [`samples/round-1/proposals.tsv`](samples/round-1/proposals.tsv) | the source: one row per bead, with arms in the placed input form (`tl:🎲 c:🛡️ br:🖼️`) |
| [`samples/round-1/proposals-check.png`](samples/round-1/proposals-check.png) | a working render of every proposal at 64, 24 and 16 px smooth, plus 8 px quantised at 1× and 3×, by day and by night, on each project's field. This is a check, not the round-1 sheet |
| [`samples/round-1/proposals-tinctures.txt`](samples/round-1/proposals-tinctures.txt) | for each project and each field colour, the charges that sink into the field |

To regenerate, run `scripts/heraldry-proto`: `go run . proposals -noto DIR -out OUT` (see its README).

## How the arms were checked

- **R2–R4, the library.** Every row went through kbeads' own `heraldry.Parse` (1.1a, the
  validator's code) in a throwaway test. All 47 parse, so each charge is in the library and
  none is a theme glyph. No shield holds one charge twice.
- **R5, unique per project.** There are no tuple collisions and no near-duplicate warnings
  (same emoji, different places) in either register. 111 distinct charges are used. Twenty
  of them appear on more than one bead, always with different company, for example
  🪟 for the Glass four times.
- **R6, kinds.** The sample covers plan epics, phase epics, tasks, bugs, decisions,
  escalations, agents and projects. All of them are kinds R6 allows.
- **Rule of tincture.** Each charge's artwork was measured against each candidate field
  colour (the share of its pixels near the field colour). On **vert**, gardenio's arms have
  no clashing charge, after one swap (🧩 → 🧮; the puzzle piece is green). On
  **azure**, claimlayer's have none. The proposed fields are vert for gardenio and azure
  for claimlayer. Or and argent would each sink 21–23 of gardenio's 109 charges.
- **Seen at size.** In the check render, 🕸️ (grey on green, lost by night) became 🍝, and
  🎚️ (dark) became ☑️.

## A habit, not a rule

Nothing in R1–R12 says what a place means, and nothing should. These proposals still follow one
reading habit, so that a set of arms can be read:

- **centre**: the thing itself (a goat, a shield, a purse);
- **top-left**: where it lives, or what sets it off (the Commons hall, a wish, the cluster);
- **bottom-right**: what it does, or how it ends (a picture, a graveyard, a stop sign).

Most arms use these three places. Bare emoji land on these places too: three bare emoji
fill top-left, centre and bottom-right (spec §4.1). Four or five charges are kept for beads
with that many parts: Track A's four surfaces, Track B's six stages, and AWS cost cuts' three
line items. Two charges sit on opposite corners when the bead is a pairing ("delivered is
not done", "the Barn and its label, kept apart"). Tasks and bugs carry two or three
charges, plans three to five, and decisions and escalations two or three.

## The proposals

The arms column is the emoji run. Places are top-left (TL), top-right (TR), centre (C),
bottom-left (BL) and bottom-right (BR).

### Plans (15)

These are every open plan that is `building`, plus the two drafts the spec names (plans
first-class, show costs). The Homestead skin plan named in §10 has since closed.

| Bead | Short title | Arms | Places | Why |
|---|---|---|---|---|
| gd-N6I3Oa7uDg | Heraldry: arms for a bead | 🎲🛡️🖼️ | TL C BR | a shield (centre), laid out like a die (top-left), drawn as a picture (bottom-right) |
| gd-vmw5s07A1F | Lands: game inside garden | 🌍💬🎮🛠️🪟 | all five | the game (centre) and its four parts, read like a map: the World and the Commons above, the Workbench and the Glass below |
| gd-X07JyIHEzl | Lands: public plots (Track B) | 🚪📜🏡🛂🎉 | all five | a public plot (centre) behind its Door and Deed, with the Gate's registration and the Open day below |
| gd-gAKb8Bf0Nm | gardeners animate by activity | 🔨🐦🤸 | TL C BR | a tool call (hammer) flies up as a bird, and the gardener strikes a pose for what it does |
| gd-EGpbDpqsec | wishes as goats in the garden | 🌠🐐🪦 | TL C BR | a wish (shooting star) becomes a goat; untended, it ends in the graveyard |
| gd-obL67RJ1td | Show costs on garden | 💵👛🧾 | TL C BR | the fleet's purse (centre), money going in, a receipt per plan and gardener |
| gd-gh5J0vVbJP | Grafana costs panels in the garden | 🌀📊🪙 | TL C BR | a Grafana (swirl) panel in the garden, starting with credits (coin) |
| gd-QjJScyWs36 | plans first-class in garden | 📋🪜🚦 | TL C BR | the plan line (clipboard), the "what moves this" ladder, the Approve and build gate |
| gd-aD7jOGmAWW | AWS cost cuts | ☁️📉🐳🌐 | TL C BL BR | the AWS bill (cloud) going down, from image pushes (whale) and public addresses (globe) |
| gd-QXq1fjYuRD | passkey login page | 📱🔑🪪 | TL C BR | a key held on your own device, shown at a branded sign-in page (ID card) |
| gd-507PTO0036 | Project-scoped human access | 🏷️🧑🔒 | TL C BR | one person, let in to the beads of one project label and locked out of the rest |
| gd-YZATAIOhfW | Short titles implementation | 📏✏️💇 | TL C BR | a name kept short (ruler), written by its author, groomed (haircut) by a Haiku agent |
| gd-y28dPqgbgg | Agents see Slack | 🧪👀📝 | TL C BR | agents look (eyes) at Slack as a test user and mark what they see against a rubric |
| gd-P78ygCUgA9 | Replace ziti with Keycloak | 🍝🗝️🧥 | TL C BR | ziti (a pasta) goes, and a key and a cloak (Keycloak) hold the edge |
| gd-5f5bJPKLhC | P7 pre-publish security close-out | 🕵️🔐🧯 | TL C BR | a reviewer's eye (detective), the doors locked, and the open holes put out before publishing |

### Track A phases (6)

| Bead | Short title | Arms | Places | Why |
|---|---|---|---|---|
| gd-E1TBc0lNdL | golden-screenshot visual regression | 📸🪞🥇 | TL C BR | the looking glass: screenshots held against a golden copy |
| gd-KoOR5SPlNE | Commons I: room model | 🏛️🧵📨 | TL C BR | the Commons (hall) and its room model: threads of messages |
| gd-xh6Ldnpjow | Commons II: crew in room | 🏛️🃏🗳️ | TL C BR | the same hall, now with the crew's cards in it and decisions put to a vote |
| gd-lDmTW3GkPq | Commons III: many people | 🏛️👪🔔 | TL C BR | the same hall, with many people in it and a bell when someone is called |
| gd-26faHO39t7 | Workbench: beads and cockpit | 🎛️🛠️📥 | TL C BR | the Workbench (tools), with the plans cockpit (controls) and the inbox |
| gd-o0I19hS1k1 | Glass: live sessions browser | 🪟🖥️👁️ | TL C BR | live agent terminals (screen) behind the Glass, to watch |

The three Commons phases share 🏛️ top-left. This is a family likeness, chosen by the author,
and R5 has no objection: the tuples differ and so do the sets. It is not inheritance (R7).
See the differencing sample below for what inheritance would look like.

### Tasks and bugs (15)

| Bead | Type | Short title | Arms | Places | Why |
|---|---|---|---|---|---|
| gd-INpa3m02bc | task | 1.1a heraldry library and parser | 📚🔣🧮 | TL C BR | a library of emoji (symbols), and the parser that counts them onto their places (abacus) |
| gd-QScdF6GUKr | task | light Glass live on k3s | 💡🪟☸️ | TL C BR | switch on (light) the Glass, live on the cluster (helm wheel) |
| gd-ODxztYntyd | task | draw the goat sprite | 🎨🐐 | TL BR | draw (palette) the goat |
| gd-wyucyyLuAz | task | make the butterflies visible | 🔆🦋 | TL C | butterflies you can see by bright day |
| gd-iAq4va9XDt | task | cut whole-farm activity SSE bytes | 🏞️📡🤏 | TL C BR | the whole farm in view, its live stream (antenna) made smaller |
| gd-Li0fBnupv2 | task | alert on stale postgres S3 backup | ⏰🐘💾 | TL C BR | an alarm when the Postgres (elephant) backup (disk) goes stale |
| gd-lnc0xuboeg | bug | keep the Barn's label off its figures | 🛖🏷️ | TL BR | the Barn (hut) and its label in opposite corners: the arms draw the fix |
| gd-tNvYsFiDb8 | bug | render raw bucket shot links via proxy | 🪣🔀🖼️ | TL C BR | a raw bucket link, sent round through the proxy, comes out a picture |
| gd-RL1uPwUqAF | bug | plan gardener unsealed after enable | ☑️🔓🛑 | TL C BR | the enable tick leaves the gardener unsealed (open lock), so its spawns are stopped |
| gd-X8q4N828KL | bug | keep escalations open on prompt delivery | 📬📂 | TL BR | delivered (mailbox) is not done: the escalation stays open (open folder) |
| gd-AteqeWk2or | bug | garden goldens hit the 10m test timeout | 🥇⏱️ | TL BR | the golden screenshots run out the stopwatch |
| gd-ZnKiMMTpko | bug | fix folded-bird gone check in perf test | 🐦⏲️💨 | TL C BR | a bird must be gone (dash) within the 2-second timer |
| gd-xP2TUS8iNJ | bug | Commons reply under agent card dropped | 🃏💬🕳️ | TL C BR | a reply under an agent card falls down a hole |
| gd-4nac3mPMgo | bug | Mentions queue behind one capacity slot | 📣🎰🐢 | TL C BR | every mention (megaphone) waits on one slot, slowly |
| gd-T2IrJR2hS6 | task | claimlayer-infra live proof | 🎥🧪🛑 | TL C BR | a live (camera) proof that the restricted agent works, and is stopped where it should be. **claimlayer register** |

### Decisions (3), escalations (3)

| Bead | Type | Short title | Arms | Places | Why |
|---|---|---|---|---|---|
| gd-9ZGh01W5ub | decision | Glass 6.3 | 🪟🔑 | TL BR | does the Glass get its own key? |
| gd-gMjUuvb3DT | decision | k3s alerts to Slack? | 📟📢 | TL BR | cluster alerts (pager) called out to Slack (loudspeaker)? |
| gd-imuxOvIxRo | decision | Flip BEADS_SHORT_TITLE_GATE to enforce? | 📏⛩️ | TL BR | close the short-title (ruler) gate? |
| gd-mF1e8qFR3c | escalation | Reseal the Heraldry plan gardener | 🛡️🔏 | TL BR | the Heraldry plan's (shield) trigger, sealed again (lock and pen) |
| gd-LLJvx6uY5z | escalation | Roll PR #1048 in the 04:00Z window | 🐘📍🕓 | TL C BR | the Postgres (elephant) backup pinned to its zone, rolled at four o'clock |
| gd-GoPqbwmNUf | escalation | Seed the alertmanager-slack-webhook Secret | 🤫🪝📟 | TL C BR | a secret (hush) for the webhook (hook) that the alerts (pager) ride |

A decision is a question, so its arms are two charges on opposite corners: the subject and
the question. The arms are the same whatever the answer, because arms carry identity, not
state (§6.3).

### Agents (3), projects (2)

| Bead | Type | Name | Arms | Places | Why |
|---|---|---|---|---|---|
| gd-zcU3tHqPgb | agent | merge desk | 🧐🖇️🛎️ | TL C BR | a desk (bell) that reviews closely (monocle) and joins the work (linked clips) |
| gd-U66ra82Klk | agent | plan gardener for Grafana costs | 📊👒🔁 | TL C BR | the gardener (straw hat) of the costs panels plan, back on every wake (loop) |
| gd-hzlWkr6DiY | agent | 1.1 crew: heraldry column | 🛡️👷🐘 | TL C BR | the crew (builder) putting arms (shield) into a Postgres (elephant) column |
| gd-Ui9jl00KfA | project | gardenio | 🌷📿☸️ | TL C BR | a garden (tulip) of beads (prayer beads) on Kubernetes (helm wheel) |
| gd-LpISt8IyHx | project | claimlayer | 🧾🥞 | TL BR | claims (receipt) in layers (a pancake stack). **claimlayer register** |

## Differencing sample (Q4)

The spec recommends no differencing in v1. To show the idea, here are three Track A phases
written as the plan's arms (🌍💬🎮🛠️🪟) with only the centre changed:

| Phase | Own arms (above) | Differenced: the plan's arms, centre changed |
|---|---|---|
| Commons I | 🏛️🧵📨 | 🌍💬🧵🛠️🪟 (centre 🎮 → 🧵) |
| Workbench | 🎛️🛠️📥 | 🌍💬🎛️🛠️🪟 (centre 🎮 → 🎛️) |
| Glass | 🪟🖥️👁️ | 🌍💬🖥️🛠️🪟 (centre 🎮 → 🖥️) |

What this shows: differenced arms are always full, five charges, and from 16 px down they
look like the plan's own. That is the point of differencing, but it also costs each step its
own face. Own arms with an authored family likeness (the Commons' shared 🏛️) give some of the
lineage without that cost.

## Findings for the next steps

1. **The two project beads have no register under R5 as written.** The gardenio project
   bead (gd-Ui9jl00KfA) has no labels at all. The claimlayer project bead (gd-LpISt8IyHx) is
   labelled `project:gardenio`. R5 says a bead with no project label may not bear arms, and
   Q7 (recommended yes) says a project is in its own register. So the new `beads.project`
   column (1.1) should come from the bead's own name for `type=project`, not from its label.
   Otherwise gardenio's arms are refused, and claimlayer's land in gardenio's register.
2. **One bead has two project labels.** gd-T2IrJR2hS6 carries both `project:claimlayer`
   and `project:gardenio`. This round puts it in claimlayer's register, but 1.1 needs a rule
   for which label wins: the first, the one that isn't the fleet default, or a refusal.
3. **8 px does not carry identity.** In the check render (3× column), every gardenio
   shield at 8 px reads as a green shield with two or three specks, as 0.1 found. 0.3
   should show the 2×2-cell rule beside the scaled drawing.
4. **Fields: vert for gardenio, azure for claimlayer.** These are the colours with no
   clashing charges in this sample. A green charge on gardenio arms would need a warning
   in the picker (§7); 🧩 was the only one here.
5. **Proposers reach for the same charges.** 🪟 (the Glass) appears four times, and 🛡️ and
   🐘 three times each. A surface's name tends to become its charge. That is useful (the
   Glass is a window everywhere), but it means the near-duplicate warning will matter more
   than the spec's §12 risk note suggests, once arms are authored per step.
