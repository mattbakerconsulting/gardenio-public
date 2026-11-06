# Lands: the Workbench and the Glass

**Status:** design spec, phase 0b of plan gd-vmw5s07A1F (Gardenio: Lands). Task gd-ywo8KxkQy6.
**Scope:** the two Lands surfaces that stand in front of beads-viewer (the **Workbench**) and
coopmux's dashboard (the **Glass**) for Lands users, both opened from a thing in `/garden`,
never as a separate app. beads-viewer and `mux.html` themselves stay, whole, as the
administrative interface (W-D7, G-D6).
**Inputs:** plan narrative draft 3 (gd-NcaFgzREzt); bundle docs under gd-uj7BxrMQsH: product
definition (gd-jXfrmBe6Yy, cited `PD`), beads-viewer inventory (gd-HU66xXpuS0, `BV`),
coop/coopmux inventory (gd-8rKFAEbjqT, `CM`). Every code claim below was re-read on `main`
at 5afbdddf.
**Siblings:** Commons spec `2026-10-19-lands-commons-design.md` (0a, gd-dAsgFTJ2Se), C1–C12
rubric `gardenio/docs/commons-ux-rubric.md` (0c, gd-dedrS0LKTA), open questions (0d,
gd-lyIZyN6wof).
**Amended 2026-10-25** (gd-PM9m0yQKkc, owner instruction in Commons thread gd-TjHWOgmQat):
Grafana and the costs rollup move out of §2.3's DROP and LATER lists into plan gd-gh5J0vVbJP
(Grafana in the garden); a Costs panel kind is added (W-D5, WB-17) and WB-10's cost tab
embeds Grafana solo panels.
**Amended 2026-10-25, second** (gd-XpUMDQOWhZ, owner instruction in Commons thread
gd-BGtml5qKgd: "need to be sure we don't ultimately deprecate beads viewer and mux; leaving
them as a powerful administrative interface that lands users don't necessarily interact
with"): beads-viewer and coopmux's `mux.html` are **not deprecated**. Nothing in this spec
deletes a template, a DROP page, `mux.html` or the `/mux` edge route any more. Changes: W-D7
and G-D6 added; W-D6, G-D5, §2.3, §2.6, §3.1, §3.2, §3.4, §3.7, §5 step 13, §6 steps 8 and
10, §7 Q5 reworded; §7 Q6 opened.
**Amended 2026-10-25, third** (decision gd-KaJD2ZEAE9, owner, thread gd-BGtml5qKgd): §7 Q6
answered **admins-too**. A flipped §2.6 redirect applies to everyone who can reach `/garden`,
the operator tier included; `?old=1` and the Old viewer menu are the way back. The dated flips
gd-zTQIrRbcse (R1/R2, >= 2026-11-01) and gd-yTZdHuUTKM (R3–R10, >= 2026-11-02) proceed as
dated with no tier check. Changes: §2.6, §5 step 13, §7 Q6.
**Amended 2026-11-05** (gd-r1IReMfQyW, plan gd-gO1fTT7oGC, owner thread gd-Rrf36anbQj): one bead
view. The bead panel (`?bead=`) and the Commons workbench node (`/garden/c/n/{id}`) are one
view; the panel draws a thread's whole conversation and the node's tree as its Work section.
Changes: §1's bead panel row, WB-1 (the wish face), WB-3 (the conversation), WB-19 added. The
views this folded, and why: `gardenio/docs/adr-one-bead-view.md`.

Paths are relative to `gardenio/controller/` unless they start with `coop/` or `gardenio/`.

---

## 0. Decisions taken in this spec

Say so on the plan thread if any is wrong; each is load-bearing for the step lists in §5–§6.

| # | Decision | Choice | Why |
|---|---|---|---|
| W-D1 | Where the Workbench lives | **A panel family inside `/garden`**, grown out of the existing in-garden bead panel (`internal/gardenhub/web/bead.js`). No new route tree, no new binary, no build step. | The bead panel, the Mailbox and the plans drawer already prove the shape; `gardenhub` is mounted in the beads-viewer binary, so every viewer handler is on the same mux. |
| W-D2 | Write path | **Reuse the viewer's own POST handlers**, exactly as the Mailbox does today (`web/garden.js` `act()` posts to `/b/{id}/decision/resolve`). Add JSON response negotiation to those handlers; never fork a second write implementation. | Permission, audit and attribution stay identical. "These are the viewer's own endpoints … so permission and audit are unchanged" is already the rule in `garden.js`. |
| W-D3 | Read path | **`/garden/*` JSON endpoints in `gardenhub`**, one per panel kind, reading the daemon with the viewer's own bearer (as `GET /garden/bead/{id}` does in `bead.go`), so kbeads' fail-closed scope cuts every answer. | One read model per panel; scope enforced where it already is. |
| W-D4 | Rendering | Vanilla JS in `gardenhub/web`, text through `textContent`, server-sanitized markdown through the same `tidy()` re-pointing that `web/chat.js` uses. Per-type **faces** are JS renderers over a typed `face` block in the JSON. | Matches gardenhub's idiom (no framework, Go tests run the pure logic under node). |
| W-D5 | URL scheme | **`/garden?<kind>=<id>`**, one open panel at a time, history via `pushState` like `bead.js`. Kinds: `bead`, `plan`, `project`, `queue`, `q`, `bundle`, `gardener`, `glass`, `costs` (WB-17, added 2026-10-25), `settings` (WB-13), `pic` (WB-18, added 2026-10-26: `/garden?pic=<path>`, the picture's path on this site; local, no fetch). `?bead=` keeps working unchanged. | A reload, a new tab, a Slack link and a Commons card all land on the same panel. |
| W-D6 | What retires | **GET pages retire; POST endpoints never do.** The `/b/{id}/*`, `/plans/{id}/*`, `/gardener/{id}/*` POSTs are the Workbench's write API. A retired page becomes a 302 to its `/garden?…` URL, with `?old=1` keeping the page. | Retirement is a redirect, never a deleted template (W-D7) and never a broken write. |
| W-D7 | What stays | **beads-viewer stays, whole, as the administrative interface.** No template, page or route is deleted, the DROP list included. "Retire" in this spec means only that `/garden` becomes the default for that page's job and the old page is one click away (`?old=1`, the Old viewer menu, §2.3). Lands users (scoped principals) are steered to `/garden`; admins keep every page. | Owner instruction 2026-10-25 (thread gd-BGtml5qKgd). The viewer is the power tool; the game is the product. Lands users need never see the viewer, and the operator never loses it. |
| G-D1 | How the browser reaches a session | **Through beads-viewer, never directly to coopmux.** A Glass relay in `gardenhub` resolves agent bead → project → viewer scope → `last_session_id`, then talks to coopmux server-side with the service token and an edge-auth envelope that names the human. | coopmux has role tiers but **no project concept** (`coop/crates/mux/src/transport/auth.rs` `edge_required_role` keys on method and path only). Per-land scope already lives in Go (`viewerScope`, kbeads fail-closed); a second scope implementation in Rust is a second wall to keep in sync. |
| G-D2 | Browser transport | **SSE down, POST up, same origin.** No browser WebSocket. | It is coop's own fallback (`coop/crates/web/src/lib/fallback-socket.ts`, `coop/crates/mux/src/transport/sse.rs`) promoted to the only path, because the public edge drops WS upgrades; it also kills the `coopmux.<host>` host derivation in `templates/static/agent_terminal.js:48-62`. |
| G-D3 | Watch vs attach | **Watch never writes to the PTY, not even a resize.** Attach (typing, keys, resize) is a separate, operator-tier, explicit act. | A watcher on a phone must not re-wrap the agent's terminal for everyone. |
| G-D4 | One nudge path | The Glass nudges through **`bridge.NudgeAgent`** (MCP channel first, coop PTY fallback, busy vs pod-gone classification, `internal/bridge/nudge.go:160`), not coopmux's PTY nudge that `/agents/{id}/nudge` uses today (`cmd/beads-viewer/agent_phase3.go:271`). | Two nudge paths behave differently for the same agent; the bridge's is the one agents are built for. beads-viewer already imports `internal/bridge`. |
| G-D5 | coopmux's role | **Backend for the Glass.** It keeps the session table, fanout, health, credential broker and NATS relay. `mux.html` stays as the operator-tier admin dashboard (G-D6; until 2026-10-25 this row said "becomes a redirect"); coop's single-session `terminal.html` stays as a pod-local debug page that is never routed at the edge. | `PD §2`: "coopmux itself stays as the backend"; the Glass is its Lands-facing UI, `mux.html` its admin one. |
| G-D6 | What stays (Glass) | **`mux.html` and the `/mux` edge route stay, operator-tier, as the admin dashboard.** The Glass is what a Lands user gets; coopmux's own dashboard is what the operator keeps. Nothing redirects off `mux.html` and nothing removes `/mux` from the edge. | Owner instruction 2026-10-25 (thread gd-BGtml5qKgd), same as W-D7. |

---

## 1. What is already inside `/garden`

Phase 5 and 6 extend these; neither starts from zero.

| Seed | Code | What it does | What it lacks |
|---|---|---|---|
| Bead panel | `internal/gardenhub/bead.go`, `web/bead.js` (gd-BdhucNqv0i) | Any bead the garden shows opens in a panel; deep link `/garden?bead=<id>`; description, fields, parents/children, comments; cross-refs stay in the garden. | As written at 5afbdddf: read-only. Built since: faces, edits, comments and lifecycle verbs (WB-1 to WB-4, phase 5), then the thread's conversation and the work tree (WB-3, WB-19, plan gd-gO1fTT7oGC). |
| Mailbox act-in-place | `web/garden.js:75-120` | Answer a decision (option or free text), approve/reject an escalation, from the dock or from a bead panel holding the row. | Only the two verbs; parses HTML `[role=alert]` for errors. |
| Plans drawer | `web/plans.js`, `farm.go` | Reads plans, phases, waiting-on lines, MR links. | No cockpit, no writes. |
| Porch chat | `web/chat.js` | Reads a conversation (`/agents/chat/conversation/{id}?format=json`), answers decisions in it. | Being replaced by the Commons (0a). |
| Route classes | `cmd/beads-viewer/route_classes.go:177-182` | All six `/garden*` routes are classified. | All `routeAdminOnly` until per-viewer `/garden` (gd-MnQlEmbUq3). |

---

## 2. The Workbench

### 2.1 Inventory of beads-viewer pages

Only pages with a Workbench verdict are listed; observability and governance pages are in
§2.3. "Opens from" is how a person reaches it in Lands. Scope column is today's
`route_classes.go` class for the GET page.

| Page | Routes (GET page + its writes) | Scope today | In `/garden` today | Verdict | Opens from |
|---|---|---|---|---|---|
| Bead detail | `GET /{id}`; fragments `/b/{id}/{description,priority,assignee,labels,owner,deps,comments,activity,preview,ownership}`; writes `POST /b/{id}/{close,reopen,claim,release,parent,comments,description,priority,assignee,labels,owner}` | scoped read, admin write | read-only panel | **MUST** | any bead on the map; any card title in the Commons; Cmd-K |
| Search + Cmd-K | `GET /search`, `GET /cmdk/suggest` | scoped | none | **MUST** | Cmd-K / `/` anywhere in `/garden`; search field in the panel header |
| Activity feed | `GET /feed`, `/feed/events`, `/feed/backfill` | scoped | none (the World is the ambient feed) | **MUST** (as a panel tab, not a page) | the plot's "recent" tab; the land header |
| Active work | `GET /active` | scoped | the Growing zone draws it | **MUST** | the plot's "growing" tab; default-to-mine toggle |
| Inbox | `GET /inbox`, `/inbox/{id}`; `POST /b/{id}/inbox-{pin,worthy}/toggle` | admin | none | **MUST** | the Mailbox (`queue=inbox`) |
| Decisions queue | `GET /decisions`, `/decisions/{id}`; `POST /b/{id}/decision/{resolve,dismiss,reflag}` | scoped read, admin write | Mailbox resolve only | **MUST** | the Mailbox (`queue=decisions`); a decision card |
| Escalations | `GET /escalations`; `POST /b/{id}/escalation/{approve,reject}` | admin | Mailbox approve/reject | **MUST** | the Mailbox (`queue=escalations`); an escalation card |
| Plans index | `GET /plans` | scoped | plans drawer (read) | **MUST** | the plot's plan crates; the land header |
| Plan cockpit | `GET /plans/{id}` and `/plans/{id}/{card,comments,events,narrative,agent,peek}`; ~40 writes `server.go:2162-2295` | scoped read, admin write | none | **MUST** | a plan field on a plot; a plan card in the Commons |
| Pour a formula | `GET /plans/{id}/pours/new`, `GET/POST /formulas/{id}/pour` | admin | none | **MUST** (from a plan only) | the cockpit's "pour" action |
| Bundles | `GET /bundle/{id}` | scoped | none | **MUST** | a bundle link in a card, a plan narrative, a bead panel |
| Project hub | `GET /p/{project}`, `/p/{project}/edit`; `POST /p/{project}/{config,bulk/status,bulk/labels,bulk/assignee}` | scoped read, admin write | the plot itself | **MUST** (read + config), bulk LATER | click the plot's sign |
| Gardeners | `GET /gardeners`, `/gardener/{id}{,/runs,/invocations,/beads,/edit,/successor}`; writes `enable/disable/pause-toggle/run-now/cancel-run/retire/mode/config/successor`; `/create-gardener` | admin | gardeners drawn on plots | **MUST** | a gardener figure's "Run" tab |
| Quick-spawn | `GET /quick-spawn{,-inline}`, POST | admin | none | **MUST** | a bead panel's "Put an agent on this"; superseded for chat by Commons mention-to-spawn |
| Settings | `GET /settings{,/edit,/view}` | scoped | none | **MUST** (default project, spawn prefs) | the avatar menu |
| Agents list | `GET /agents` + `/agents/events` | scoped | Porch + figures | **Glass** (§3) | — |
| `/agents/chat`, `/threads*` | | admin | Porch chat | **Commons** (0a) | — |

### 2.2 Workbench MUST list

Each row is a test case: it is done when it works in `/garden` on the operator's land, at
phone and desktop width, and a golden screenshot covers it (phase 1 gate, rubric C1–C12).
IDs are cited by the phase-5 steps.

| ID | Requirement | Built on |
|---|---|---|
| WB-1 | Bead panel shows a **typed face** for: task/bug/feature/chore, epic, plan (epic + `plan` label), decision (options, chosen, requester), escalation (ask, operation type, approver, result), agent (state, task, project/role, cost, Glass link), message, bundle, report, gardener, MR (status, pipeline), and **wish** (a thread head: any bead but a message or channel that carries the `thread` label, `kin.IsWishHead`; added 2026-11-05, gd-zn6iUnPd9Q): the ask itself as the lead (`slack_root_text` or the Commons wish's `text`, which the fields table then leaves out), who asked, where (the Slack channel, linked to the thread when the bridge recorded its permalink, or the Commons #general) and when, and one counts line from the thread's kin (replies, agents live or left, work open or closed, PRs merged) that links to the sections below and never repeats the conversation. A member of a thread (a message, an agent, a task with `thread_id`) carries `Part of wish <head>` under its header, opening the head's panel, not the Commons node. Any other type falls back to today's generic fields table. | `BV: Bead detail page`; the 17 `type_*` partials in `templates/bead_show.html` are the reference rendering |
| WB-2 | **Inline edit** of title, description (markdown preview), priority, assignee, labels, owner, status from the panel. | `POST /b/{id}/*` (W-D2) |
| WB-3 | **The conversation** (amended 2026-11-05, gd-tx2wMH48MV, gd-MpqfrPhLZP; was **Comments**: read, compose with markdown preview, newest-first with "older"). A bead in a thread (a wish head, or a bead whose `fields.thread_id` names one) shows one Conversation section: the thread's messages (Slack replies, squawks, cards, Commons messages, `gb say` lines, and the messages out of the thread that name the bead, via `mention`) and the bead's own kd comments, **one timeline, oldest first**, the latest 12 shown and the rest behind Show older, the newest line in view on a phone. Each line names its speaker in its voice (`internal/kin` `Speaker`; the rules are the rubric's, `commons-ux-rubric.md` C11 Voices); a card is one state chip, never prose. Its foot is the thread's reply composer where the Commons offers one (the thread pane's write path, `commons/write.go`), and a writer's kd comment composer (markdown preview), whose comment joins the timeline in the comment voice. Live: the panel listens to the Commons stream for its head's thread and patches the one line in place, with no refetch of the bead document. A bead in no thread that no message names keeps the Comments section: composer first, newest first, Show older comments. | `GET /garden/bead/{id}` `kin` (`internal/kin`); `web/beadtalk.js`; `POST /b/{id}/comments{,/preview}`; the Commons thread's reply route and stream |
| WB-4 | **Lifecycle**: close (with reason), reopen, claim, release; set parent; deps shown as "waits on / holds up" (exists) with add/remove. | `POST /b/{id}/{close,reopen,claim,release,parent}`; deps write via the daemon client the fragments use |
| WB-5 | **Cmd-K and search** inside `/garden`: id jump, lexical, semantic with lexical fallback, scoped; results open panels, not pages. | `search.go`, `cmdk.go` lifted behind a JSON shape |
| WB-6 | **Queues** panel (`?queue=`) with three tabs, Inbox, Decisions, Escalations, sorted like the coopmux sidebar (needs-you first, then age); pin and worthy toggles; resolve/dismiss/reflag; approve/reject; default to mine. | `inbox.go`, `decisions.go`, `escalations.go`; Mailbox `act()` |
| WB-7 | **Plans index** and **plan cockpit** (`?plan=`): steps with state, waiting-on, crew, narrative (rendered, editable), decisions and escalations of the plan, events live; writes: add/close/reopen/dispatch/retitle/reorder steps, phases, narrative/doc edit, mark, state/build/complete/abandon/reopen. | the lifted `internal/plans` service (§2.5); `farm.go` for structure |
| WB-8 | **Pour** a formula from a plan. | `formulas.go` pour handlers |
| WB-9 | **Bundles** (`?bundle=`): per-format renderers (markdown doc, report, screenshots), source-MR link, table of contents on desktop. | `bundle.go`; `templates/static/bundle_toc.js` |
| WB-10 | **Project panel** (`?project=`, opened from the plot sign): agents, gardeners, LLM accounts, cost and purse (`budget_usd`/`remaining_usd` when gd-SAPwFE4FyW lands), recent activity, open decisions; project config edit. The **cost tab embeds Grafana solo panels** for the project (the `gardenio-garden-costs` dashboard with `project` set), built and owned by plan gd-gh5J0vVbJP step 2.2; this row keeps the purse figures, that plan supplies the charts. | `project_hub.go`, `project_config.go`; Grafana embed per gd-gh5J0vVbJP |
| WB-11 | **Gardener panel** (`?gardener=`): config, mode, runs, invocations, beads; enable/disable/pause/run-now/cancel/retire/successor; create from the project panel. | `gardener_page.go` and the `/gardener/{id}/*` POSTs |
| WB-12 | **Quick-spawn** from a bead panel (role, model alias), honouring the user's spawn preferences. | `quick_spawn.go` |
| WB-13 | **Settings** from the avatar menu: default project, spawn preferences. | `settings.go` |
| WB-14 | **Live**: an open panel refreshes when its bead moves (id-only SSE event, client refetches; Commons D8), never a stale snapshot. | the gardenhub SSE stream (`stream.go`); `BeadFeedHub` |
| WB-15 | **Soft degradation**: daemon error renders what the map already knows (`window.GARDEN.known`) with an honest "could not load" line, never a blank panel. | `bead.js` already does this for reads |
| WB-16 | **Redirects**: every retired GET page 302s to its `/garden?…` URL with the id and query preserved; old Slack permalinks keep working. | W-D6 |
| WB-17 | **Costs panel** (kind `costs`, `/garden?costs=fleet` or `/garden?costs=<project>`): Grafana solo panels of fleet or per-project spend, admin-only (`routeAdminOnly`) through that plan's phase 2; a tenant credits-only view follows in its phase 3 and never hands a tenant a Grafana session. Built by plan gd-gh5J0vVbJP (phase 2, step 2.1), not by phase 5; phase 5 only provides the panel host (5.1) it registers on. | plan gd-gh5J0vVbJP: shared embed builder, `GET /garden/charts/{set}`, `gardenio-garden-costs` dashboard |
| WB-18 | **Picture preview and viewer** (owner ask, Commons thread gd-5JBvgy02rf and Slack gd-0UcZIloW6M): a mouse resting on any picture in `/garden` (a Commons image attachment, a grade's picture, a markdown figure, a bundle's screenshot), or a keyboard focus on it, raises a bigger preview beside it, inside the viewport, pointer-transparent; none on touch or for a picture data saver holds. A click opens the panel kind `pic` (`/garden?pic=<path>`) on the picture's set, read from the page around it (the grade, the Commons message, the markdown document, the bundle): the picture at the panel's width (the map window's width on a desktop), Previous/Next, "N of M", Open whole (the file, as the link did), a filmstrip; ArrowLeft/ArrowRight/Home/End and a swipe cycle it, wrapping. A reload shows the picture alone. One helper, `web/pictures.js`. | 5.15 (gd-uEEeNMlk1a) |
| WB-19 | **One bead view** (added 2026-11-05, plan gd-gO1fTT7oGC, owner thread gd-Rrf36anbQj). The bead panel (`/garden?bead=<id>`) and the Commons workbench node (`/garden/c/n/<id>`) are **one view**: the node draws the selected bead's document with the panel's own renderer (`web/workbench.js` `BEAD.into`, from the same `GET /garden/bead/<id>`: head, face, conversation, composer), and keeps only its tree rail and its Jobs, Plan and Open rails as its own part (`commons/node.go` has no face of its own; gd-0GnBi7SRfK). The node's tree is the panel's **Work** section: the panel embeds the node fragment's tree (two levels, each row with its state, PR, open ask and jobs) and its Open rail (`GET /garden/c/n/<id>?embed=1`, `web/beadwork.js`), and once it is in hides the rows of its own Children, Related, Waits on and Holds up lists that the tree draws, so each bead is listed once (those lists stand as the fallback when the Commons is off or the read fails; gd-efoAiLlzRC). A Work row in the panel opens that bead's panel, not the node. There is no "Open in workbench" button: the panel is the workbench. The steward's node (`/garden/c/s/<land>`) stays its own document, and lists the land's open wishes as a Wishes tree under its plans, each row opening `?bead=<head>` (gd-ak4P2qXMss). beads-viewer's bead page, `/threads` and the bead chat stay whole (W-D7), reached by the Menu's Old viewer. | `bead.go` `readKin`; `web/workbench.js`, `web/beadtalk.js`, `web/beadwork.js`, `web/commons2.js`; `commons/node.go`, `commons/stewardnode.go`; ADR `gardenio/docs/adr-one-bead-view.md` |

### 2.3 LATER and DROP (Workbench side)

Taken from `PD §4` and `BV §2`; listed so nobody builds them in phase 5.

- **LATER** (stay on beads-viewer, reachable from an "Old viewer" link in the avatar menu
  until they earn a row, and stay served there afterwards as admin pages, W-D7): saved views; plan critique, assistant prompt, gardener bind/wake;
  plans bulk and master switch; formulas registry browsing; triggers; agent lifecycle events;
  telepathy; taints; people directory and `/u/{slug}`; teams and forwarding; ratings; advice
  and prime; roles and Claude-config matrix; health tiles; live widgets; project bulk edits.
- **DROP** (**not deleted**, amended 2026-10-25, W-D7: these get no Workbench row and stay
  served on beads-viewer for admins; the list exists so nobody ports them. Removing one from
  the Old viewer menu is the owner's per-page call, `PD §8` Q8): constellation, maps board, deck and `/up`, coverage, extern, peel
  catalog, cassettes, tool-latency, trust, incident-intel, fleet sentinels, legacy redirects
  (`/index`, `/captains`, `/ui`, `/operator-inbox`).
- **Moved to plan gd-gh5J0vVbJP** (Grafana in the garden, amended 2026-10-25): the **costs
  rollup** (was LATER) and **charts/dashboards/Grafana proxy** (was DROP: `/costs*`,
  `/charts*`, `/dashboards`, `/grafana*`, `grafana_proxy.go`, `dashboard_view.go`). Grafana
  becomes a Workbench panel kind (WB-17) and the Project panel's cost tab (WB-10). Phase 5
  neither builds nor deletes any of these. **DONE, and the `/costs` embeds do not retire**
  (step 4.3, gd-0WYDQUDLFf, 2026-10-27): the owner decided beads-viewer stays (Commons
  thread gd-RRCiOkQB5N), so `/costs` keeps its chart cards and `/charts` its hub, with no
  §2.6 banner, redirect flag or Old viewer entry. Each links the garden's Costs panel
  (`/garden?costs=fleet`) for a viewer who reaches `/garden`. Parity is recorded on that bead
  and in `gardenio/docs/garden-grafana-panels.md` (Parity with `/costs`) as a record, not a
  gate. The page's cost explorer, trust strip and account cards stay as they are.

### 2.4 How the Workbench opens

From the **world** (every hot spot already has a bead id or a project; `garden.js beadHref`):

| Thing in the world | Opens |
|---|---|
| A crop, weed, rock or signpost on a bed (a task, bug, decision, escalation) | `?bead=<id>` with its typed face (WB-1) |
| A plan field or crate | `?plan=<id>` cockpit (WB-7) |
| A plot's sign | `?project=<name>` (WB-10) |
| The Mailbox / Yard signpost | `?queue=decisions` (or the tab that has something for me) (WB-6) |
| A gardener figure | agent bead face with a live Glass thumbnail; "Watch" → `?glass=<agent>` (§3) |
| A gardener's shed / run board | `?gardener=<id>` (WB-11) |
| Cmd-K or `/` | `?q=<text>` (WB-5) |

From a **Commons card** (0a owns the card; this spec owns what the links do):

- The card **title** opens `?bead=<subject>` over the Commons pane (desktop: beside it;
  phone: a sheet over it, Back returns to the thread).
- Card **buttons** act in place through the same POSTs (W-D2); they never navigate.
- A plan card's "Open plan" → `?plan=`; a bundle link → `?bundle=`; an agent card's "Watch"
  → `?glass=`.
- Every bead-id link inside a message body goes through `chat.js tidy()` and stays in the
  garden.

### 2.5 Architecture

1. **Panel host.** `bead.js` becomes `workbench.js`: one panel element, a `kind` dispatcher,
   the `?<kind>=` parser it already has (`param`, `withBead` generalised to `withPanel`), and
   one renderer module per kind. Desktop: a right-hand panel beside the world (and beside
   the Commons pane when both are open). Phone: a full-screen sheet with a Back bar.
2. **Reads.** New gardenhub routes, all JSON, all reading with the request's bearer:
   `GET /garden/plan/{id}`, `/garden/project/{name}`, `/garden/queue/{inbox|decisions|escalations}`,
   `/garden/search?q=`, `/garden/bundle/{id}`, `/garden/gardener/{id}`. `GET /garden/bead/{id}`
   gains a `face` object keyed by type. Each handler calls the same functions the viewer page
   handler calls (e.g. `inbox.go`'s builders), extracted where they are tangled with templates.
3. **Writes.** The panel posts to the viewer's existing POST routes with
   `Accept: application/json`. A small helper, `respondAction(w, r, result, err)`, answers
   `{"ok":true,"bead":<id>}` or `{"ok":false,"error":"…","status":409}` when JSON is asked
   for and keeps today's HTML/redirect otherwise. The Mailbox's `[role=alert]` scraping goes
   away. No handler's permission or audit code is touched.
4. **Plans service.** `PD §6` #5: lift the cockpit POST handlers' bodies out of
   `cmd/beads-viewer/plans*.go` into `internal/plans` (functions over the daemon client, no
   `http`), call them from the old handlers unchanged, then from the Workbench. This is the
   one large refactor in phase 5 and it ships alone, behind the existing tests, before any
   cockpit UI.
5. **Scope.** Every new `/garden/*` route is added to `route_classes.go` in the same MR
   (the build-failing coverage test already enforces it). Reads are `routeScoped` from the
   start where the underlying viewer page is scoped (bead, search, plans, bundle, project,
   decisions read); they cannot be reached by a scoped principal until `/garden` itself is
   (gd-MnQlEmbUq3), so classifying them now costs nothing and avoids a second pass. Writes
   inherit the POST's existing class.
6. **Live.** The gardenhub stream already emits id-level patches; a panel subscribes to the
   ids it shows and refetches on change (Commons D8). The cockpit additionally listens to
   `/plans/{id}/events` until the plans service emits onto the hub.

### 2.6 Retirement order for the old pages

A page retires when all four hold: (a) its MUST rows pass in `/garden`; (b) a golden covers
the panel; (c) for one week the old page shows an "Open in the garden" banner (the
`migration_banner.go` pattern) and the redirect is live behind a flag; (d) the flag flips and
the GET becomes a 302 to its `/garden?…` URL (WB-16), with `?old=1` keeping the page and the
Old viewer menu listing it. **The template is never deleted** (W-D7, amended 2026-10-25;
until then this sentence read "deleted one release later"). The 302 applies to every
principal who can reach `/garden`, the operator tier included (§7 Q6, answered admins-too):
an admin lands in `/garden` and gets the old page back with `?old=1` or the Old viewer menu.
Scoped principals are not redirected while `/garden` is admin-only for them
(gd-MnQlEmbUq3).

Two escapes keep a retired page reachable after its flip (step 5.13): `?old=1` on the page's
GET keeps the page for that request (`garden_retire.go`, as `?bead=1` keeps a plan's bead
page), and an htmx swap keeps it too. That is how the LATER parts of a retired page stay in
reach: a plan's critique, assistant and gardener on `/plans/{id}?old=1` (linked from the
cockpit panel for a viewer who may write), the plans bulk edits and master switch on
`/plans?old=1`. A scoped principal is never redirected today (it cannot reach `/garden`
until gd-MnQlEmbUq3), and an agent's bead page is not retired until the Glass.

Order, easiest-and-most-used first:

| # | Retire | To | Gate rows | Why this slot |
|---|---|---|---|---|
| R1 | `/decisions`, `/decisions/{id}`, `/escalations` | `?queue=decisions`, `?queue=escalations`, `?bead=` | WB-1 (decision, escalation faces), WB-6 | The Mailbox already does the writes; smallest gap. |
| R2 | `/inbox`, `/inbox/{id}` | `?queue=inbox` | WB-6 | Same panel; the operator's daily loop moves in one step. |
| R3 | `/{id}` bead page | `?bead=` | WB-1–WB-4, WB-12, WB-14, WB-15 | Most-linked page; needs edits, comments and lifecycle first. Slack and bundle permalinks redirect. |
| R4 | `/search`, Cmd-K | `?q=` | WB-5 | Depends on R3 so results land on a finished panel. |
| R5 | `/active`, `/feed` | project panel tabs | WB-10, WB-14 | The World is already the ambient feed; these become tabs. |
| R6 | `/bundle/{id}` | `?bundle=` | WB-9 | Self-contained renderer. |
| R7 | `/plans`, `/plans/{id}` and its GET partials | `?plan=` | WB-7, WB-8 | Largest; waits for the plans service lift. |
| R8 | `/p/{project}` | `?project=` | WB-10 | After R5 and R7 supply its tabs. |
| R9 | `/gardeners`, `/gardener/{id}*`, `/create-gardener`, `/quick-spawn*` | `?gardener=`, bead panel action | WB-11, WB-12 | Admin-only, low traffic. |
| R10 | `/settings*` | avatar menu | WB-13 | Last; harmless to leave. |
| — | `/agents`, `/agents/{id}` terminal | `?glass=` | GL rows | Phase 6 (§3.7). |
| — | `/agents/chat*`, `/threads*` | Commons | 0a | Commons phases 2–3. |

`/` already redirects admins to `/garden` (`server.go:717-727`); once R3 is done it does so
for every principal that can see `/garden`.

---

## 3. The Glass

### 3.1 Inventory

What exists for watching and steering a live session today, across four components.

| Piece | Code | What it does | Verdict |
|---|---|---|---|
| coopmux dashboard `mux.html` | `coop/crates/web/src/roots/mux/` (React + Vite + Bun; `App.tsx`, `SessionSidebar.tsx`, `ExpandedSession.tsx`, `CredentialPanel.tsx`, `LaunchDialog.tsx`; `components/Tile.tsx`, `SessionDetails.tsx`, `inspector/*`) | Grid of live ANSI thumbnails (`screen_batch` over `/ws/mux`), attention-sorted sidebar, expand to xterm.js with replay, actions panel (text, keys, nudge, respond), credential pool, launch dialog. Deep link `#<pod>`. | **Stays** as the operator-tier admin dashboard (G-D6); the Glass is the Lands-facing UI. Launch dialog and raw bytes are not ported to the Glass |
| coop web terminal `terminal.html` | `coop/crates/web/src/roots/coop/`, built single-file into `coop/crates/web/dist/terminal.html`, served by coop at `/` (`coop/crates/cli/src/transport/mod.rs:418-456`) | One session, in-pod. | **Stays** as a pod-local debug page; never routed at the edge (G-D5) |
| Transport | coop `/ws` and `/api/v1/*` (`crates/cli/src/transport/`); coopmux `/ws/{session}`, `/ws/mux`, `/sse/*` twins (`crates/mux/src/transport/{ws_mux,sse,http}.rs`); `fallback-socket.ts` (WS → SSE+POST); `base64.ts`, `ansi-render.ts`, `replay-gate.ts` | Per-session PTY + state + usage + hooks subscription; replay; one upstream fanned out to N browsers (`upstream/bridge.rs`). | **Kept** as backend; the Glass consumes it server-side |
| coopmux auth | `transport/auth.rs` `edge_required_role` and the per-frame `edge_session_frame_required_role`; `transport/edge_auth.rs` (HMAC v1/v2 envelope: operator, mode, roles, audit id) | viewer / operator / admin per route **and per frame**; credentials admin-only; unknown mutation → admin. **No project scope.** | Tier table **ported** into the relay; coopmux keeps enforcing it as defence in depth |
| beads-viewer agent terminal | `templates/static/agent_terminal.js` (xterm over a **browser-direct** WS to `coopmux.<host>/ws/{session}`, `:48-62`); `agents.go` `getScreenSnapshot` (cached-screen first paint via `coopmuxapi.GetScreen`), `coopmuxTerminalURL*` (`:1916-1964`); `agent_input.go`, `agent_phase3.go`, `agent_compact.go` (input, keys with destructive confirm, nudge, respond, compact; audit lines `agent_terminal_write`) | The only place a Go service already proxies session control with audit. | **Seam reused**; browser-direct WS replaced by the relay (G-D1) |
| coopmux Go client | `internal/coopmuxapi/client.go`: `GetScreen`, `GetAgentState`, `SendKeys`, `Nudge`, `Respond`, `SendInput` | Server-side session verbs with the service token. | **Reused**; gains a streaming subscribe |
| Bridge | `internal/bridge/nudge.go` `NudgeAgent` (`:160`, MCP `:8789` first, coop `/api/v1/agent/nudge` fallback, terminal-state and `stop_requested` skips); `coop_url:` note written by `internal/statusreporter/reporter.go:534`, read by bridge and poolmanager; `bot.go:1760` `coopmuxAgentLink` → `COOPMUX_PUBLIC_URL#<pod>` in every Slack agent card | The fleet's one nudge path and the link Slack history carries. | **Kept**; link target moves to `/garden?glass=` |
| Edge | unified host `/mux` path (`gardenio/helm/gardenio/values.yaml:362-374`, `templates/coopmux/ingress.yaml`); Traefik ForwardAuth (oauth2-proxy → Keycloak realm `gardenio`, `gardenio/docs/beads-viewer-public-internet.md`); `cmd/edge-auth/forwardauth.go` emits the same `X-Auth-Request-*` headers coopmux reads | coopmux is browser-exposed today behind its own route. | `/mux` **stays** at the edge, operator-tier (G-D6; until 2026-10-25: "becomes a redirect, then is removed") |

### 3.2 Glass MUST list

Three verbs, in increasing power. **Watch** is look-only; **nudge** puts a sentence in the
agent's next turn; **attach** is hands on the keyboard.

| ID | Verb | Requirement | Tier | Built on |
|---|---|---|---|---|
| GL-1 | watch | Open a live terminal for one agent (`?glass=<agent>`): cached screen paints first, then replay, then live PTY; xterm.js lazy-loaded from the viewer's `static/xterm.bundle.js` only when a Glass opens. | viewer (see GL-14 for scoped) | `getScreenSnapshot`; coopmux `/ws/{session}?subscribe=pty,state,usage` |
| GL-2 | watch | Watch never sends resize or input (G-D3): the terminal renders at the PTY's own geometry and scales to fit; a phone gets pinch-zoom and horizontal pan, not a rewrap. | viewer | `ExpandedSession.tsx` fit logic, inverted |
| GL-3 | watch | Header: agent, project, role, task title (agent bead `task_id` → title, the same resolver the agent card uses), pod, account, cost, context size, state glyph. | viewer | `SessionDetails.tsx` fields; `crates/mux/src/lib.rs:300-345` enrichment; `PD §6` #9 |
| GL-4 | watch | State machine shown everywhere the agent appears (figure, card, Glass header): Starting/Working/Idle/Prompt/Error/Parked/Restarting/Exited, plus `rate_limited` from `error_category`; Prompt and Error pulse. | viewer | coop `driver/mod.rs:64-82` transitions via the relay |
| GL-5 | watch | **Wall** (`?glass=` with no agent, or the land header's "Glass" button): the land's live sessions as text-grid thumbnails (DOM, not canvas, `PD §6` #4), attention-sorted (prompt, error, rate-limited, working, idle), batched updates; click a tile to watch. | viewer | `/ws/mux` `screen_batch`, `SessionSidebar.tsx` ordering, `Tile.tsx` + `ansi-render.ts` ported |
| GL-6 | watch | Reconnect without loss: SSE `Last-Event-ID` resumes; on a gap the relay re-sends a screen snapshot instead of partial bytes. | — | `replay-gate.ts` semantics |
| GL-7 | nudge | Nudge box under the terminal and on the agent card: free text → `bridge.NudgeAgent` (G-D4); also writes the message bead so the Commons thread shows it (Commons D6); busy vs pod-gone vs stopped answered honestly. | operator | `nudge.go`; `agent_phase3.go` handler re-pointed |
| GL-8 | nudge | Respond to a prompt: Permission, Plan, Question, Setup (incl. `oauth_login`, `fable_consent`) rendered from coop's prompt schema with explicit option labels, never bare indices; one shared component used by the Glass and the Commons tool-permission card. | operator | `ActionsPanel.tsx:159-350`; `coopmuxapi.Respond`; `PD §6` #8 |
| GL-9 | attach | **Take the keyboard**: a click or tap on the terminal (or Enter on it) claims control (`POST /agents/{id}/control/claim`), shows "attached as <you>" to every other watcher, and only then sends typed input, resize and named keys (Esc, Tab, arrows, Ctrl chords) with the destructive-key confirmation `agent_phase3.go:22-24` already has (Ctrl-C, Ctrl-D, Ctrl-Z, and Esc, which Claude Code reads as interrupt; gd-bWtNrk4snt; not Ctrl-\, which coop cannot type, gd-HCcFZAc9Rj). Those keys ask first however they arrive, typed straight into the terminal too: the question opens under the terminal with Cancel focused (Esc in it cancels), and Send is in the danger style; beads-viewer's agent page asks for the same set (`internal/glass` destructiveKeys). The terminal is the input: no text field and no key buttons; attached, it wears the accent frame and a blinking cursor, and the PTY echoes what is typed. A click or tap in the prompt moves the cursor there (Up/Down, then Left/Right, inside the prompt box only). Taking the keyboard from someone else stays an explicit button. Detach on Esc-Esc, blur timeout or close. (Operator, decision gd-vRWdWdROSa; gd-FpaO6Pb9bR.) | operator | `agent_input.go`, `agent_phase3.go` keys and claim; coopmux per-frame tiers |
| GL-10 | attach | Compact (with cancel) beside the attached terminal. | operator | `agent_compact.go` |
| GL-11 | all | Every write (input, keys, nudge, respond, compact, claim) emits today's `agent_terminal_write` audit line with the operator's `oidc:` email and the coopmux audit id. | — | `BV §5`; `PD` cross-cutting |
| GL-12 | all | Opens from: agent card "Watch" (Commons), gardener figure on a plot, agent bead face (WB-1), Wall tile, and every legacy link (GL-13). Close returns to where it was opened. | — | §3.3 |
| GL-13 | all | Links: Slack's `coopmuxAgentLink` emits `/garden?glass=<agent>`; `/agents/{id}` terminal → `?glass=` per §2.6 (redirect at most, `?old=1` keeps it). `/mux#<pod>` keeps working as is (G-D6): nothing redirects off `mux.html`. | — | `bot.go:1760` |
| GL-14 | all | Per-land scope: a principal sees and acts on sessions only of agents whose project is in their scope (§3.4). | — | G-D1 |
| GL-15 | admin | Credential pool panel (accounts, health, session counts, 5h/7d utilisation, strategy) read-only, admin tier, opened from the operator land's header. | admin | `CredentialPanel.tsx`; `http_cred.rs:233-329` via the relay |

**LATER** (`CM`, `PD §4`): HTTP screen fallback as a separate path (GL-1's cached paint covers
first load); last-message preview on tiles; inspector state tab; signal/shutdown/restart;
file upload; hook config; transcripts and recordings viewer; push/rebalance and account CRUD;
OAuth toast; WebSocket upgrade as an optimisation where the edge allows it; hotkeys.
**DROP** (not ported to the Glass; they stay on `mux.html`, G-D6): raw byte injection, launch
dialog. Still DROP outright: serving `terminal.html` at the edge.

### 3.3 How the Glass opens from a card or a plot

- **Plot**: every gardener figure is a hot spot (`garden.js` `worldHot`). Hover/tap shows its
  card with a 6-line live thumbnail (the GL-5 tile at small size) and the state glyph; "Watch"
  opens `?glass=<agent>`. Prompt and Error states are already the attention colours on the
  figure.
- **Commons card**: the agent card's "Watch" button (today the Slack card's agent-name link)
  opens the Glass beside the thread on desktop and as a sheet on phone; the nudge box in the
  Glass and the thread composer write to the same conversation.
- **Decision / tool-permission card**: "See the screen" opens the Glass scrolled to the
  prompt; responding in either place resolves both.
- **Land header**: "Glass" opens the Wall for the land.

### 3.4 Auth: the same edge, per-land scope

```diagram
browser ──(cookie: oauth2-proxy session)──► public edge (Keycloak + oauth2-proxy, gd-P78ygCUgA9)
   │                                            │ X-Auth-Request-{User,Email,Groups}
   ▼                                            ▼
/garden/glass/*  ───────────────────────► beads-viewer (gardenhub Glass relay)
                                              1. principal + roles from oidcauth (viewer/operator/admin)
                                              2. agent bead → project → viewerScope(principal) ∋ project?  else 404
                                              3. frame/route tier check (ported table)                     else 403
                                              4. agent bead → last_session_id
                                              ▼  service bearer + edge-auth v2 envelope
                                              │  (X-Operator=<human>, roles, audit id)
                                         coopmux /ws/{session}, /ws/mux, /api/v1/sessions/{id}/*
                                              (re-checks the tier per frame; no project knowledge)
```

1. **One edge.** The Glass is served under `/garden`, so it rides the cookie and
   ForwardAuth the rest of `/garden` already has. No second login, no `coopmux.<host>` host
   derivation in the Glass. The `/mux` route stays at the edge for the operator tier (G-D6).
2. **Per-land scope is decided in Go** (G-D1), by the same `viewerScope` the bead panel and
   the daemon use. A session whose agent is out of scope is a 404, identical to a missing
   one, so ids do not leak. The Wall's upstream `/ws/mux` subscription is **one** server-side
   feed; the relay filters `sessions` and `screen_batch` per principal before fanning out,
   exactly like the per-principal `BeadFeedHub`.
3. **Tiers are checked twice.** The relay applies a Go port of `edge_required_role` and
   `edge_session_frame_required_role` before forwarding; coopmux applies the original after.
   A shared fixture (method, path or frame type → tier) is asserted by a Go test and a Rust
   test so the two tables cannot drift.
4. **The human is named upstream.** The relay signs the edge-auth v2 envelope with the
   human's email and roles, not the service identity, so coopmux's audit and metrics name the
   person. The service bearer only proves the relay is the relay.
5. **Scoped humans and PTYs.** A PTY can show a secret. v1: watch and the Wall are
   **operator-tier** on every land; a per-land setting `glass_watch: operators|members`
   (default `operators`) lets a land owner open watch to members later. Nudge and respond
   stay operator-tier; attach stays operator-tier on the operator's land and admin-only on
   tenant lands until Fences (phase 10). This answers `PD §8` Q5 provisionally; 0d
   (gd-lyIZyN6wof) owns the final answer.
6. **Route classes.** Every `/garden/glass/*` route lands in `route_classes.go` in the same
   MR. Reads `routeScoped` (the scope check above is the real wall; the class keeps the
   build-failing table honest), writes `routeAdminOnly` until the Foundation action table
   gives them a scoped class.

### 3.5 Relay shape

| Route | Method | Does |
|---|---|---|
| `/garden/glass/{agent}` | GET | JSON: identity header (GL-3), state, cached screen text, `glass_watch` policy, whether the caller may nudge/attach. |
| `/garden/glass/{agent}/stream` | GET (SSE) | `pty` (base64 bytes), `state`, `usage`, `screen` (resync) events from one upstream subscription per session, shared by every watcher. |
| `/garden/glass/{agent}/send` | POST | One control frame (`input`, `keys`, `resize`, `respond`, `nudge`, `compact`), tier-checked, audit-logged, forwarded. `input`/`keys`/`resize` require a live control claim (GL-9). |
| `/garden/glass/wall` | GET (SSE) | The land's filtered `sessions` + `screen_batch` feed (GL-5). |
| `/garden/glass/credentials` | GET | Admin-only pool snapshot (GL-15). |

Upstream connections are opened lazily on the first watcher, shared, and closed 30 s after
the last one leaves. Back-pressure: a slow SSE client drops `pty` frames and is sent a
`screen` resync instead (the replay-gate rule), so one phone on a bad network cannot stall
the upstream for others.

### 3.6 What the Glass does not change

The bridge's `NudgeAgent`, the `coop_url:` bead note, controller → coopmux account sync,
credential distribution, throttle failover, health eviction and the NATS relay for
outbound-only pods are all untouched. The Glass is a reader and a caller of them.

### 3.7 Retirement order (Glass side)

| # | Retire | To | Gate |
|---|---|---|---|
| G-R1 | beads-viewer `/agents/{id}` terminal and `agent_terminal.js` browser-direct WS | `?glass=<agent>` | GL-1–GL-4, GL-7–GL-11 |
| G-R2 | Slack card agent link (`coopmuxAgentLink`) | `/garden?glass=<agent>` | GL-13 |
| G-R3 | ~~`mux.html`~~ | **struck 2026-10-25 (G-D6)**: `mux.html` stays as the admin dashboard; the Wall is the Lands-facing twin, not a replacement | — |
| G-R4 | `/agents` list page | the Wall and the project panel's agents tab | GL-5, WB-10 |
| G-R5 | ~~`/mux` edge route~~ | **struck 2026-10-25 (G-D6)**: `unifiedHost.paths.mux` and `templates/coopmux/ingress.yaml` stay | — |

G-R1 and G-R4 follow §2.6 as amended: a redirect at most, `?old=1` keeps the page, the
template is never deleted (W-D7).

---

## 4. Shared with the Commons

- **URL scheme.** The Commons owns its own params (channel/thread, 0a); Workbench and Glass
  params (W-D5) compose with them: `/garden?thread=<id>&glass=<agent>` shows the thread with
  the Glass beside it on desktop.
- **One transport library.** The Glass's SSE-down/POST-up client is the same small module the
  Commons uses for its live stream (`PD §6` #4), written once in `gardenhub/web`.
- **Visual regression.** Goldens (phase 1, gd-D953GccBTY) for: each WB face, the queues
  panel, the cockpit, the project panel, the Glass at watch and attach, the Wall, at phone
  and desktop, light and dark. The Glass goldens use a fixture session that replays a
  recorded `recording.jsonl` with a frozen clock, so the terminal pixels are deterministic.
- **Rubric.** C1–C12 (0c) grades every panel; the Glass adds one check the rubric should
  carry: the terminal is legible at phone width without rewrapping (GL-2).

---

## 5. Phase 5 — Workbench: step list

Each step is one MR, sized for one crew agent, in dependency order. Steps marked ∥ can run
in parallel with the step before them.

1. **5.1 Panel host.** Rename `bead.js` → `workbench.js`; generalise `?bead=` to the `kind`
   dispatcher (W-D5) with `?bead=` behaviour byte-identical; desktop side panel and phone
   sheet. Tests: the existing `bead_test.go` node tests pass unchanged; new tests for
   `withPanel` and Back behaviour.
2. **5.2 JSON actions.** `respondAction` helper; `Accept: application/json` on the
   `/b/{id}/*` POSTs, the decision and escalation POSTs, and the gardener POSTs; Mailbox
   switched off HTML scraping. Tests: each handler's HTML and JSON branch, permission
   unchanged.
3. **5.3 Typed faces (WB-1).** `face` block in `GET /garden/bead/{id}` and one JS renderer per
   MUST type, generic fallback for the rest. Goldens for every face.
4. **5.4 Bead writes (WB-2, WB-3, WB-4, WB-12).** Inline edit, comment compose, lifecycle,
   parent and deps, quick-spawn, in the panel.
5. **5.5 Queues panel (WB-6).** `GET /garden/queue/{kind}` reusing `inbox.go`,
   `decisions.go`, `escalations.go` builders; three tabs; attention sort; Mailbox opens it.
   **Retire R1, R2** behind the banner flag.
6. **5.6 Live panels (WB-14, WB-15).** Panel subscribes to the gardenhub stream by id;
   degraded state copy. ∥ with 5.5.
7. **5.7 Search (WB-5).** `GET /garden/search`, Cmd-K and `/` in the garden. **Retire R3**
   (bead page) and **R4** once 5.3–5.7 are green.
8. **5.8 Plans service lift.** Move the cockpit POST bodies into `internal/plans`; old
   handlers call it; no UI change. Tests: the existing plans handler tests unchanged and
   green. ∥ with 5.3–5.7.
9. **5.9 Cockpit (WB-7, WB-8).** `GET /garden/plan/{id}`; steps, waiting-on, crew,
   narrative edit, plan decisions/escalations, all writes via the service-backed POSTs; pour.
   **Retire R7.**
10. **5.10 Bundles (WB-9).** `GET /garden/bundle/{id}`, per-format renderers. **Retire R6.**
    ∥ with 5.9.
11. **5.11 Project panel (WB-10).** Plot sign opens it; tabs for agents, gardeners,
    accounts, cost/purse, activity (feed + active), decisions; config edit. **Retire R5, R8.**
12. **5.12 Gardener panel and settings (WB-11, WB-13).** **Retire R9, R10.**
13. **5.13 Redirect sweep (WB-16).** Flip remaining flags (they apply to the operator tier
    too, §7 Q6), add the Old viewer link for LATER pages. ~~Delete retired templates one release
    after their flip~~: struck 2026-10-25 (W-D7), templates are never deleted.
    *As built (gd-JFIdRRlyvl):* the Menu's Old viewer item and `/old-viewer`, which lists the
    LATER pages, the retired pages (each by `?old=1`) and the DROP pages (§7 Q5); the
    `?old=1` escape (§2.6); and a sweep test that every retired GET page 302s with every flag
    on while every page the Old viewer lists stays. The flips themselves are config writes,
    each gated on a week of its banner in a rolled-out tag and on Q6, so they ride the dated
    follow-ups gd-zTQIrRbcse (R1, R2) and gd-yTZdHuUTKM (R3–R10).
14. **5.14 Check-in.** Harness screenshots of every panel at both widths posted to
    #gardenio-homestead; parity check against §2.2 recorded on the plan.
15. **5.15 Picture preview and viewer (WB-18, gd-uEEeNMlk1a, added 2026-10-26).** `web/pictures.js`:
    `PICTURES.wire(root)` on each Commons fragment and each painted panel; the shared
    `div.pic-preview` tooltip; the `pic` kind registered on the panel host as its first local
    kind (`local(id)`: painted from what the page has, never fetched). The old 5.16 (the viewer
    alone, gd-lUmmh4COaO) was folded into this step: one PR. Goldens `picpreview`, `picviewer`.

## 6. Phase 6 — Glass: step list

1. **6.1 Tier table port.** Go port of `edge_required_role` and
   `edge_session_frame_required_role` in `internal/glass`, with a shared fixture file asserted
   by a Go test and a Rust test in `coop/crates/mux`. No behaviour change anywhere.
2. **6.2 Upstream client.** `coopmuxapi` gains `Subscribe(ctx, session, topics)` (one WS to
   coopmux `/ws/{session}` with the service token and a v2 envelope naming the human) and
   `SubscribeMux(ctx)` for `/ws/mux`. Tests against a fake coopmux.
3. **6.3 Relay, read side (GL-1, GL-3, GL-4, GL-6, GL-14).** `GET /garden/glass/{agent}` and
   `/stream`; shared per-session upstream; scope check; resync on gap; route classes.
   Tests: out-of-scope agent is 404; two watchers share one upstream; slow client gets a
   resync.
4. **6.4 Glass panel, watch (GL-1, GL-2, GL-12).** `?glass=` panel kind in `workbench.js`;
   lazy xterm; no-resize scaling; header; opened from the agent face and the gardener figure.
   Goldens from a recorded session fixture.
5. **6.5 Nudge and respond (GL-7, GL-8, GL-11).** `POST /garden/glass/{agent}/send` for
   `nudge` and `respond`; nudge via `bridge.NudgeAgent` plus the message bead; one respond
   component shared with the Commons tool-permission card; audit lines.
   `POST /agents/{id}/nudge` re-pointed at the same path.
6. **6.6 Attach (GL-9, GL-10).** Control claim, "attached as" banner to other watchers,
   input/keys/resize frames, destructive-key confirm, compact; detach rules.
   **Retire G-R1.**
7. **6.7 Wall (GL-5).** `/garden/glass/wall` filtered fanout; text-grid tiles; attention
   sort; small thumbnails on gardener figure cards. ∥ with 6.5–6.6.
8. **6.8 Links (GL-13).** `coopmuxAgentLink` emits `/garden?glass=`; `/agents/{id}` gets
   the §2.6 banner and flag. No coopmux change: `mux.html` and its `#<pod>` deep link stay
   (G-D6). **Retire G-R2, G-R4** (G-R3 struck).
   *As built (gd-STJOoSnUAW):* the bridge resolves a card's pod to its agent bead
   (`resolveAgentPod`, the `pod_name` read it already made) and `agentCardHref` links
   `<viewer>/garden?glass=<agent bead>`, keeping `<coopmux>#<pod>` when the viewer URL or the
   agent bead is unknown (`coopmuxAgentLink` is now `agentCardLink`). `/agents` (G-R4) has its
   own banner and redirect pair, `viewer.garden_agents_banner` / `_redirect`, to the Wall
   (`?glass=wall`) or, with `?project=`, that project panel's Agents tab; the Old viewer lists it
   as `/agents?old=1`. The agent's bead page (`/{id}` of an agent, G-R1) already carried its
   banner and flag from 6.6.
9. **6.9 Credential pool (GL-15).** Admin panel off the operator land header.
10. **6.10 Edge cleanup.** Struck 2026-10-25 (G-D6): the `/mux` route stays at the edge.
11. **6.11 Check-in.** Watch, nudge, attach and the Wall screenshotted at both widths;
    parity check against §3.2 recorded on the plan.

---

## 7. Open questions (routed to 0d, gd-lyIZyN6wof)

1. **Glass for members** (`PD §8` Q5): is the §3.4 #5 default (`operators`, per-land opt-in
   to `members`) acceptable, or must members never see a PTY?
2. **Attach on tenant lands**: admin-only until Fences, or never?
3. **Audit sink** (`PD §8` Q9): GL-11 keeps the process log line; should phase 6 also write an
   audit bead per attach session so the land owner can see who typed into their agent?
4. **DROP list confirmation** (`PD §8` Q8): which DROP pages does an SLO or runbook link?
5. **Old viewer link**: ~~keep beads-viewer's LATER pages reachable indefinitely, or set a
   date?~~ Answered 2026-10-25 by the owner (thread gd-BGtml5qKgd): indefinitely. beads-viewer
   is the administrative interface and is not deprecated (W-D7); same for `mux.html` (G-D6).
   Built in step 5.13 (gd-JFIdRRlyvl): the garden's Menu item Old viewer opens `/old-viewer`
   (`old_viewer.go` `oldViewerGroups`), and it stays for as long as the viewer does.
6. **Redirect tier** (opened 2026-10-25, gd-XpUMDQOWhZ): ~~once a page's §2.6 flag flips,
   does the 302 apply to the operator tier too (admins land in `/garden` and use `?old=1` or
   the Old viewer menu to get back), or only to non-admin principals who can reach `/garden`,
   with admins landing on the old page by default?~~ Answered 2026-10-25 by the owner
   (decision gd-KaJD2ZEAE9): **admins too.** The 302 applies to everyone who can reach
   `/garden`; admins use `?old=1` or the Old viewer menu to get the beads-viewer page. This is
   the 5.13 design as built (PR #1069): `garden_retire.go` gains no tier check, and the
   dated flips gd-zTQIrRbcse and gd-yTZdHuUTKM proceed as dated.

## 8. Risks

- **Relay load.** Every PTY byte now crosses beads-viewer. Mitigation: one upstream per
  session shared by all watchers, lazy open, 30 s linger, drop-to-resync for slow clients;
  `world_perf_test.go`-style budget test for 30 sessions × 3 watchers.
- **Scope bug = leak.** A scope mistake in the relay shows another land's terminal.
  Mitigation: 404-not-403, the route class table, a negative-matrix test (scoped principal ×
  out-of-scope agent × every Glass route and frame), coopmux tiers as the second check.
- **Plans lift.** The cockpit refactor touches the most-used write surface. Mitigation: it
  ships alone with no UI change and the existing handler tests as the net (step 5.8).
- **Two UIs, for good (W-D7).** Old pages and panels coexist permanently. Mitigation: the
  writes are literally the same handlers (W-D2), so they cannot disagree; only rendering
  differs.
- **Respond indexing.** A wrong option auto-approves the wrong thing. Mitigation: GL-8 labels
  options from coop's schema, replay-tested against coop's e2e fixtures before 6.5 merges.
