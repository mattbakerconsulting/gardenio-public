# Heraldry: arms for a bead (design spec)

**Status:** **shipped**, live in 2026.307.2 (k3s prod rev 272) and accepted by the owner on decision gd-dmNV26eh5D (GO, 23:59Z 11-03; step 5.1, gd-sXppd9dFcU). Beyond §11, the 5.1 walk added the line's arms on agent beads (5.8, gd-ASLjQPkHJz, PR #1612), gardeners (5.9, gd-QboM9sPYCg, PR #1611) and goats (5.10, gd-WUqr6IkgkM, PR #1622). Release note: [`release-notes/2026-11-04-heraldry.md`](../../release-notes/2026-11-04-heraldry.md); user page: [`heraldry/README.md`](../../heraldry/README.md). Open: the Slack agent card's visual grade (gd-qBbHYCbw3u, waiting on gd-fWeCNSRdCd). Before that, it was **accepted 2026-10-26, and revised the same day on the owner's verdict of 20:45Z** (thread gd-lvbRHMaXID, "final answer"): arms are **three to five emoji in a row**, shown the same way everywhere; the shield, its five places and the tincture are gone (§9, row "Verdict 10-26"). Badges from arms (plan gd-pQZO9ie11D, step 10, gd-XWocW0x1fq, 2026-10-28) changes the badge row of §5, a path in §6.4, and §13 (Tier-2 badges from arms and `og:image` through the proxy are now in scope; a public image host for Slack unfurls is still not). The verdict's changes (step 5.7, gd-vQobmSXmne) are the status, §0, §1, R2, R5, R9–R11, R13, §3.2, §4, §5, §6, §7, §8, §9, §10, §11 and §13; struck text is kept struck, and nothing is renumbered. Before the verdict it was accepted (step 0.5, gd-AhHh5abzNq) on the owner's GO on round 2 (decision gd-bC2HJmTCl3, 09:32Z): §9 holds the answers to Q1–Q16, and the round-2 recommendations (the eight tinctures as drawn, the keyline, the 24 px gate without the 16 px stake, rubric v2) are the rules. The step 0.5 changes are the status, §0, R13, §6.2, §6.3, §6.6, §9, §10, §11, §12. History: draft for the owner, 2026-10-25; **revised 2026-10-25 (round 0)** on the owner's first note in the thread (large emoji library; one standard shield, drawn as a small image rather than text emoji; the blazon and the rest of the notation internal only); **revised 2026-10-26 (round 1)** on the owner's five notes after the round-1 sheet (the picture is drawn at high resolution and only displayed small; the pixellated tier is thrown away; the field colour is load-bearing, with contrast checked; agents, not humans, pick arms; surfaces with room come first; a rubric and then a large sample of real beads; epics first, because they last). The round-0 changes are §0, §1, R3, R10, §4, §5, §6, §9 Q1 and Q9–Q11, §11, §12. The round-1 changes are §0, R1, R3, R5, R6, R7, R13, §4.1, §4.3, §5, §6, §7, §8, §9 Q2, Q6 and Q12–Q16, §10, §11, §12, §13. Origin: Slack thread gd-zhMrQAlMhS (owner, 2026-10-25
19:16Z). Plan bead: gd-N6I3Oa7uDg (phases P0–P5, gardener gd-HWYPK0WMCX). The owner asked for the
system and its initial rules, support in every surface that shows a bead, a place in the
`/garden` graphics regime, and **samples before implementation** ("we will want to see samples
before we implement, both for semantic and graphic confirmation; we can iterate for a while and
look at real beads"). So this spec has two halves: the rules (§1–§9), which are the proposal, and
the sample loop (§10), which is the first phase of the plan and the only thing that gets built
before the owner says GO.

Every code claim was read on `main` at 82778ea7. Paths are relative to
`gardenio/controller/internal/` unless they start with `gardenio/`, `kbeads/`, `lands/` or
`.claude/`.

---

## 0. Summary

**Heraldry** is an optional mark a bead can bear: **three to five emoji in a row**, read left to
right (`🔥🧭📦`), the first saying what the bead is and a later one what it is for. Each emoji is a
**charge**; the whole is the bead's **arms**. Arms are **unique within a project**: no two beads of
one project ever bear the same run. Arms are a visual identity, read in a glance the way a short
title is read in a breath. The short title stays the name; the arms are the face.

> **The verdict, 2026-10-26 20:45Z.** Until then arms were up to five emoji placed like the pips
> on the five face of a die on a shield of one tincture, drawn as a picture wherever an image could
> go. The owner saw that form on the surfaces and the run in Slack prose, and ruled: "the shield
> form is terrible, but linear is great; so let's just make it like that everywhere; 3-5 linear
> symbols per epic". The text below describes the run; struck passages and the round sheets
> record the first form.

Four things make it a system and not a decoration:

1. **One field, one canonical form** (§3, §4.1). `heraldry` is a column on the bead, like
   `short_title`, holding the run. kbeads validates it, keeps it unique per project, and exposes it
   on HTTP, gRPC, `kd` and the Lands contract.
2. **The run is the form** (§4). A person types arms as emoji and sees them as the same emoji in
   the same order on every surface. The internal words never reach a surface.
3. **Every surface that names a bead shows its arms** (§5), right before the short title: Slack
   prose and cards, `kd show` and `kd list`, `gb` output, the beads-viewer, the Workbench, the
   Commons cards and the World.
4. **Text where text emoji can go, a picture of the row where they cannot** (§6). `/garden` is
   emoji-free by four written contracts (rubric C7, `GOLDENS.md`, the harvest spec, the glyph
   tests), so there the run is drawn as one row from **one pinned emoji artwork set** (Noto Emoji,
   Q10), at device resolution, at 24 and 64 px. The library is **large** (~1,400 charges: every
   base emoji the artwork set draws, minus the theme's glyphs, Q1).
5. ~~**The field colour is part of the arms** (R13).~~ Struck by the verdict: there is no field.

Three further round-1 notes shape the rest. **Epics first** (R6): "heraldry will be more
important for epics than tasks; quite possibly only epic level things should get heraldry to
start … like a plan, or a phase … since it sticks around longer than e.g. a decision or an
escalation". So v1 arms belong to the long-lived beads, plans, phases and other epics (and
projects, Q7); tasks, decisions, escalations and agents come later, if at all. **Agents, not
humans, pick arms** in the normal case (R7, §7: the agent that files or holds a bead chooses
them; the human picker is a correction tool). **Surfaces with room come first** (§5, §6.3, §11:
the plan page, the Workbench header, viewer cards and Slack cards at 64 and 40 px, then the
Commons card and the farm's plot gate at 24; the 16 px stake is out of v1 (Q16) and the 8 px task
cell is gone with task arms; since the verdict, Slack and the viewer show the run as text and only
`/garden` draws the row picture).

**What gets built first is a sample sheet, not code** (§10): arms proposed for ~40 real beads,
rendered in every form (the shield picture at the slot sizes at device resolution, day and night,
on each tincture; a Slack card; `kd show`; the farm cells), posted to the thread and the plan's
plot, iterated until the owner says GO on the semantics and the look. Round 1 was posted
2026-10-26 00:48Z; round 2 is on the owner's eight notes (§10): first a **rubric** for what good
arms are, then "a rather large list of sample heraldry with pictures based on real beads", the
epics. Phase 1 (the field) started early
on the owner's override (plan comment 22:36Z) and takes the round-1 changes as a follow-up step.

**Plan bead:** gd-N6I3Oa7uDg, phases P0 gd-ggcYcNjsLA, P1 gd-cZf66OsB0d, P2 gd-9VNuzbkVim, P3
gd-vY1ddrMCzQ, P4 gd-lAcJ6d687D, P5 gd-sA2jOarxCh; the step ids are on the plan.

## 1. Vocabulary

| Word | Meaning | Example |
|---|---|---|
| **arms** / **heraldry** | the whole mark one bead bears: three to five charges in a row | `🔥🧭📦` |
| **run** | the charges in order, left to right; the canonical, stored and shown form (§4) | `🔥🧭📦` |
| **charge** | one emoji in the run | `🧭` |
| **row picture** | the run drawn as one image, for the surfaces that cannot draw text emoji (§4.3, §6) | 24 to 64 px high |
| **library** | the list of charges the system can draw (§2 R3) | ~1,400 charges at v1 |
| **register** | a project's set of arms in use; the uniqueness scope (§2 R5) | per `project:<x>` |
| ~~shield, place, tincture, blazon~~ | *(struck by the verdict)* the first form's five-place field, its places TL TR C BL BR, its colour, and its six-token stored form, which Parse still reads (§4.1) | `blue 🔥 . 🧭 . 📦` |

The plain words a surface may use are **arms** and **emoji**. No surface, help text or error
message prints the struck words (the owner's round-0 note: the notation "and such should be
internal only").

## 2. Rules (v1)

- **R1 Optional.** A bead with no arms is normal and stays normal. Nothing is derived, generated
  in bulk or backfilled (contrast `short_title`, which is required and groomed). Arms are chosen,
  in the normal case by the agent that files or holds the bead (R7).
- **R2 Three to five charges, in a row.** Arms are three to five charges in order, read left to
  right; the first says what the bead is (rubric S1, S3). Fewer than three or more than five is
  refused, and zero charges is "no arms" (field absent). A charge is one emoji: one grapheme
  cluster; a VS16 presentation selector is part of its charge, never a charge of its own. v1's
  library (R3) holds no skin-tone or ZWJ sequences and no flags, so those never reach the parser
  as valid. ~~One to five charges, one per place, any places.~~ (Struck by the verdict.)
- **R3 Charges come from the library.** The owner's round-0 answer to Q1: "a large library of
  emojis to pick from". The library is **every base emoji that the pinned artwork set draws**
  (Noto Emoji at a pinned release, Q10): single-codepoint emoji with or without VS16, about 1,400
  at Unicode 15.1, **minus** the bridge theme's glyphs (R4), skin-tone and ZWJ sequences and
  flags. (Round 0 also dropped the charges that collided at 8 px; round 1 threw the pixellated
  tier away, so nothing is dropped for size any more, §6.6.) Each charge has codepoints, a plain-words name
  (from the Unicode name, lower case, its accessible name) and an image asset; nothing is drawn
  by hand. The library is a generated Go table in kbeads (the validator's source of truth,
  names and codepoints only) and the artwork assets embedded in the controller
  (`internal/heraldry/assets/`, §6.4); `gardenio/docs/heraldry/library.md` lists it. The
  library moves when the artwork set is re-pinned, by PR, additions only; a charge is never
  removed once any register holds it. A value whose charge is outside the library is refused
  (R11): without an asset there is no picture, and the picture is the form.
- **R4 The library is not the theme.** The bridge theme (`bridge/theme.go:142-220`) already uses
  ~55 emoji for bead types, lifecycle states, semantic marks and tools, and C7 forbids one glyph
  meaning two things on a card. So the library **excludes** every emoji the Garden and Business
  themes use, and a theme change may not take an emoji the library holds. A test in the bridge
  asserts the two sets are disjoint. The type glyph (🌱 task, 🐛 bug, 🌻 epic …) therefore never
  appears in arms, and a run next to a type glyph is never ambiguous.
- **R5 Unique per project.** The identity of arms is the run. Two beads in one project
  (`project:<x>` label) may not bear the same run: the daemon refuses it (409 `heraldry_taken`).
  The same charges in another order are other arms as data, but they are hard to tell apart, so
  the daemon warns (`X-Beads-Warning`) and the rubric fails them (G4). Arms are **not released at
  close**: a closed bead keeps its arms and the run stays taken in that project (Q3). A bead with
  no project label has no register and may not bear arms. ~~The identity is the tincture plus the
  ordered 5-tuple of places.~~ (Struck by the verdict.)
- **R6 Which beads.** **v1: epics**, that is plans, phases and any other `type=epic` bead, and
  projects (Q7). The owner's round-1 note: arms matter more for epics than tasks, because an
  epic "sticks around longer than e.g. a decision or an escalation"; a plan is on the farm for
  weeks and in dozens of cards, so its face is worth learning, where a task's is gone in an
  afternoon. Tasks, bugs, features, chores, wishes, decisions, escalations and agents may not
  bear arms in v1 (the validator refuses them with plain words); widening R6 is a later rule
  once the epics have taught us what arms are for. Data and internal kinds (session, message,
  doc, bundle, evidence, report, card, trigger, role) never bear arms. (Q2, answered.)
- **R7 Who assigns.** The agent that files or holds the bead, at create (`kd create
  --heraldry`) or later (`kd update --heraldry`). The owner's round-1 note: "humans aren't
  really going to be in charge of picking this", so the normal path is an agent reading the bead
  and choosing charges under the advice line (§7), the way round 1's arms were proposed; a human
  may set or change arms through `kd` or the Workbench as a correction, not as the design. A
  later change replaces the arms (the old arms are released back to the register, because a bead
  has one arms at a time and the change was deliberate); `kd update --heraldry none` removes
  them. No groomer assigns arms in v1 (Q15 asks whether beads that humans author, from Slack or
  the Workbench, should get arms from the same groomer that fills their short title). A plan's
  steps do not inherit the plan's arms (Q4 holds the one inheritance idea worth having:
  differencing). **Amended 2026-10-27 by the owner** (Commons thread gd-Rf8rJQhkRE, gd-PUTYxm5BtX):
  a bead that bears no arms *shows its line's*, the nearest armed ancestor's along its parent-child
  edges (an epic winning at the same distance, `badge.ResolveArms`, the rule `gb badge` draws by),
  before its title, so a growing task in the Yard names its plan at a glance. It still bears none:
  the register, the validator and R6 are unchanged. In `/garden` so far: the Yard's Mailbox and
  Growing rows (`gardenhub/line_arms.go`, the hub's batched walk), a Yard plant's accessible name,
  the bead view's title (`line_arms`, `line_from`), and the gardeners (step 5.9, gd-QboM9sPYCg): the
  farm document gives each live agent its task's line arms (`FarmGardener.task_arms`, resolved over
  the rows the farm already holds, no daemon read), which the Porch row draws
  before the step it tends (not a plan's roster in the bead view: budget B1, decision gd-C5WGUm5SLu), its link says in words, and its figure wears as a small badge over its
  head at the Plan and Bead depths (`HERALDRY.badge`; not at Project or World, and not while its
  speech bubble is up); the Yard's agent rows and the barn's figures' names wear the same through the
  hub's batched walk. An agent bead (gd-ASLjQPkHJz) works a task
  through an `assigned` edge (`fields.task_id`), not under it, and bears no arms: its line runs
  through its task (`badge.Node`), so it shows its task's line arms in the bead view's title and
  before its task on the Tending row, on the beads-viewer agent page before the task's link, and on
  the Slack agent card before the task's short title (`badge.ResolveLine`, the one walk all three read).
- **R8 Shown everywhere, never alone.** Every surface that shows a bead's name shows its arms
  (§5), and the arms never carry meaning the words do not: the short title is always present,
  and the accessible name of any rendered arms is the plain-words name (§8).
- **R9 Precedence over derived marks.** Where a surface draws a derived identity for a bead (the
  Tier-0 plan-badge monogram in the viewer, `cmd/beads-viewer/plan_badge.go`, since retired with
  its flag in badges step 9, gd-5qEMPflr4z; the type glyph
  before a short title in Slack prose, `bridge/slack_link_hydrate.go:410`), authored arms **take
  its place**. A plan with arms shows its run, not its monogram; a bead with arms shows its run,
  not its type glyph, in prose. In card headers, lists and tables where the type has its own badge
  or column, the type glyph stays there and the run sits with the title.
- **R10 Budget.** C7 caps a card at 6 distinct glyphs. The run counts as **one glyph glued to
  the short title** (rubric C7 note): it names one bead, like the type glyph it replaces, and a
  line that names several beads shows the run only for its subject. The row picture is an image
  and spends none of the six. The farm's label tiers (cursor-grid spec §6.1) apply unchanged:
  arms add no text.
- **R11 Validation is server-side.** kbeads refuses a value that is not three to five library
  charges (R2, R3), that names a charge twice, that is set on a bead kind outside R6, or that
  collides in the register (R5). `kd` validates the same way before sending, for a fast error,
  with the same error text, and the text uses plain words ("arms are three to five emoji", "🐝 is
  used twice", "🏴 is not in the library"), never the notation.
- **R12 Stable text, stable bytes.** The canonical stored form is §4.1's after
  normalisation (NFC; VS16 added to charges whose library entry carries it, so `⚙️` never becomes
  `⚙`). Equality for R5 is on the canonical form.
- ~~**R13 The field is load-bearing.**~~ **Struck by the verdict** (2026-10-26 20:45Z). The
  first form named a tincture, one of eight, in the arms and counted it in the identity, and
  chose one by contrast when none was given. The run has no field, so there is nothing to colour;
  the palette table and contrast matrix are in `gardenio/docs/heraldry/round-2-sheet.md` as
  history.

## 3. Storage and API

As built in Phase 1 for the first form. Step 5.3 (gd-InkEAAqGNM) changes the column's value to the
run (§4.1) and the library package to match: `Parse` takes the run and reads the legacy form,
`Canonical` returns the run, and the place, set and block helpers go. The rest of this section
stands.

The field follows `short_title` touch for touch (plan `gardenio/docs/superpowers/plans/2026-10-14-short-titles.md`,
migration `056_beads_short_title`, column at `kbeads/internal/model/bead.go:233`).

### 3.1 kbeads

| Piece | Change |
|---|---|
| Column | `beads.heraldry TEXT NULL`, the canonical blazon, next to `short_title` (`kbeads/internal/model/bead.go:233`). Migration `057_beads_heraldry` (idempotent DDL, paired down file, no backfill). Column lists, insert, update and the five scan sites follow `short_title`'s (`store/postgres/queries.go:30,39,381,410,1582,1601`; `scan.go:48-496`). |
| Project on the row | the project is a label today (`labels` table, `migrations/001_initial.up.sql:29`; `projectFromLabels`, `server/http_metrics.go:277`), so no index can scope by it. Step 1.1 adds `beads.project TEXT NULL`, written from the `project:` label on create and kept in step by `reconcileLabels` (`server/beads.go:2373`) on a label change. As built, the store rewrites it on every `project:` label add or remove and on every bead update; it holds the project when the bead has exactly one `project:` label and is NULL otherwise, so a bead with arms must carry exactly one. It is the register key and nothing else reads it yet. |
| Uniqueness | `CREATE UNIQUE INDEX idx_beads_heraldry_register ON beads (project, heraldry) WHERE heraldry IS NOT NULL AND deleted_at IS NULL` (as built in 1.1: not `fleet, project, heraldry` as first drafted, because `fleet` is audit attribution only and one project's beads span many fleets, so a fleet-scoped index would let two beads of a project bear the same arms), the pattern of `idx_beads_idempotency_key` (`029_idempotency_key.up.sql:32`) and `idx_beads_fields_mr_url_unique` (`042_…:66`). The daemon checks the register before the write for a 409 that names the colliding bead (id + short title), and maps the 23505 race the way `beads_idempotency.go:109-120` and `beads.go:812-828` do. A project move through `reconcileLabels` re-runs the check. |
| Library | `kbeads/internal/heraldry/library.go`: the ~1,400 charges as a **generated** table (`Charge{Emoji, Name, Codepoints, VS16 bool}`, names and codepoints only, no images; the generator reads the pinned artwork set's manifest and the exclusion lists, R3), `Parse(input) (Arms, error)` for the emoji, placed and canonical input forms (§4.1), `Canonical(Arms) string`, `Set(Arms)` for the near-duplicate warning, served as `GET /v1/heraldry/library` (JSON, a day's cache; as built in step 1.2 it sits under `/v1/` like every other daemon route, carries the palette, the place names and `HeraldryHelp` beside the charges, and an ETag) for the bridge, the viewer's picker and `kd`. The artwork assets live in the controller (`internal/heraldry/assets/`, §6.4), not in kbeads. Grapheme splitting uses `uniseg`, already an indirect dependency (`gardenio/controller/go.mod:196`); nothing in the repo splits graphemes today, and the rune clips (`model.ClipShortTitle`, `validate.go:73`; `truncateRunes`, `bridge/bot_agents_wrapup.go:46`; `truncTitle`, `gb/news.go:598`) must never be applied to the canonical form. |
| Validator | `kbeads/internal/model/validate.go` next to `ShortTitleHelp:42-46`: `HeraldryHelp`, one text for the `kd` flag help, the daemon error and the advice line. Hooked into `createBead` beside `ValidateBead` (`server/beads.go:353`) and `updateBead` (`:1880`), which covers HTTP, gRPC, escalation replay, formula pour and undo. |
| HTTP / batch / gRPC | `heraldry` on the bead shape, create and update inputs (`server/beads.go:27,1153-1168`; `convert.go:24`; `undo.go:120,147`); proto `Bead` field 28, `CreateBeadRequest` 17, `UpdateBeadRequest` 15 (`proto/beads/v1/types.proto:58`, `beads.proto:43,167-171`), then `make proto`; `heraldry=<canonical>` as an exact-match list filter (`http_beads.go:314`); `kd search` indexes the charge names. The kbeads client carries it (`client/client.go:317,513,527`; `grpc.go:128,313,954`; `http.go:351`). |
| Warnings | the near-duplicate warning rides `X-Beads-Warning` (short-titles D3). |
| Gate | none. The field is optional (R1), so there is no warn/enforce lifecycle. |

### 3.2 kd

`--heraldry "<emoji…>"` on `create` (`cmd/kd/create.go:153,277,365`) and `update` (`update.go:71-79`; `none` clears), `Arms:` in `kd show` (`output.go:47-51`), one line with
the run (~~the three-line terminal block under it~~, struck by the verdict, step 5.3), an `ARMS` column (the emoji run) in `kd list` (`output.go:229-245`) and `kd search`
(`search.go:226-240`) tables when any row has arms (`text/tabwriter` counts a rune per column and an emoji is two, so the column is padded by grapheme width, the one place `kd` needs `uniseg`), `kd heraldry library [--search <word>]` (print the library by name), `kd heraldry check "<emoji…>"
[--project x]` (validate, print the run, say in plain words whether the register has it or the same emoji in another order).

### 3.3 Controller, gb, Lands contract

`beadsapi` carries `Heraldry` on `BeadDetail` (`beadsapi/client.go:1475`), the wire struct (`:2993,3102`), the create request (`:1122`) and the update request (`client_query.go:469-501`); `gb` shows the
emoji run before the title in `gb news` (`gb/news.go:636-712,1097`), `gb ready` (`ready.go:141`) and `gb plan show` (`plan_show.go:66-109`, whose `plan_read.go:126-254` rows gain `Heraldry` and `ShortTitle` together, since they carry neither today); the Lands contract
(`lands/contract/openapi.yaml`) gains an optional `heraldry` string (the canonical form, documented as opaque) on `Ticket`,
`TicketCreate`, `TicketUpdate`, `Plan` and `PlanStep`, additive per the contract's rules
(`openapi.yaml:10-34`), with fixtures for each schema touched, `ticketKeys` and `ticketWrites`
entries in `kbeads/internal/landmap/tables.go:174-199`, a version bump of `info.version` and
`landsv1.ContractVersion` together, and `make generate-lands-contract`. `gl` prints the emoji run in
its ticket list.

## 4. Forms: the run, and the internal names behind it

The owner's verdict of 2026-10-26 20:45Z (§9, row "Verdict 10-26"): "the shield form is terrible,
but linear is great; so let's just make it like that everywhere; 3-5 linear symbols per epic, and
we can display it all sorts of places. final answer". So **the run is the form**: three to five
emoji in a row, read left to right, shown the same way on every surface. Where a surface can draw
text emoji it shows the run as text; where it cannot (`/garden`, §6.1) it shows a picture of the
same row. The round-0 rule stands: the internal words never reach a surface.

### 4.1 The canonical form (stored, wired, never shown)

The **canonical form is the run itself**: the charges in order, nothing between them, after R12's
normalisation. It is the column value, the wire value on HTTP, gRPC and the Lands contract, and
the equality for R5.

```example
🔥🧭📦      three charges: fire, compass, package
🐐🌠🪦🌾    four charges
🔥🧭        INVALID: fewer than three (R2)
🐝🐝🔥      INVALID: a charge may appear once (R11)
```

A person types the same thing: `kd update gd-XXXX --heraldry "🔥🧭📦"`. ~~The placed input
(`tl:🌱 c:🔥 br:📦`) and `on <colour>`~~ are struck from the help text; step 5.3 may keep them
accepted silently for one release.

**Legacy, read-only.** Arms stored before the verdict are in the first form's six-token blazon
(`blue 🔥 . 🧭 . 📦`: a tincture, then the five places TL TR C BL BR, `.` for an empty place).
Parse still reads it, as the charges in place order with the colour dropped (`🔥🧭📦`), so every
stored value keeps rendering; nothing writes it any more, and step 5.6 (gd-LGIxGuLZ9y) rewrites
every stored value to the run.

### 4.2 The emoji run (the form)

The charges in order: `🔥🧭📦`. Internally the **crest** in earlier revisions; now simply the
arms. It is always followed by the short title, with one space, and it counts as **one glyph glued
to the short title** (Commons rubric C7, `gardenio/docs/short-titles.md`). Where the type glyph
would have gone in prose, the run goes (R9). In a card header the type badge and state glyph stay
and the run sits before the short title.

### 4.3 The row picture (where text emoji cannot go)

A raster or SVG image of the same run: the charges drawn from the library's artwork (§2 R3) in one
row, produced by one renderer (`internal/heraldry`, §6.4). It is used only where text emoji are not
allowed: the Workbench, the Commons cards and the World (§6.1). It carries `alt` / `aria-label`
text in plain words (§8).

~~In a terminal, `kd show` prints the places as three lines.~~ Struck: `kd show` prints one line,
`Arms:        🔥🧭📦`.

## 5. Surfaces

Where a bead is shown and what its arms look like there. The rule: **the text run (§4.2) wherever
text emoji can go; the row picture (§4.3, §6) only where they cannot.** A surface never prints the
canonical form of the first form or any internal word.

| Surface | Today | With arms (step) |
|---|---|---|
| Slack prose, References footer, link hydration | type glyph + short title (`bridge/slack_link_hydrate.go:399-402,491-498`; `bridge/bead_display_name.go:33`) | run + short title (R9, Q5); `beadDisplayName` carries `Heraldry` next to `ShortTitle`, read through `heraldry.Parse` so a stored value of either form gives the run (2.2, 5.5a) |
| Slack agent, decision, escalation, thread and wrap-up cards (`bridge/theme.go:392` `BeadTypeBadge`; `buildAgentCardBlocks`, `bot_agents.go:2391`; `bot_decisions.go`, `bot_escalations.go`) | type badge in the header | the run before the short title in the header line, through `beadTitleLinkArms(id, title, heraldry)`; the type badge and state glyph stay. The `slack_file` image block of step 2.3 goes (5.5a) |
| Slack unfurl (`bridge/link_unfurl.go:314-333`) | type emoji + title | run + short title |
| `kd show` / `kd list` / `kd search` | `Short title:` line; `SHORT` column | `Arms:` line with the run (5.3); `ARMS` column with the run |
| `gb news`, `gb ready`, `gb plan show`, `gb bundle show` | short title | run + short title |
| beads-viewer lists, bead page, plan cards and decks, plan peek, cmd-k results (`cmd/beads-viewer/templates/partials/*.html`) | ASCII type letter (the Tier-0 monogram and `viewer.show_plan_badges` were retired in gd-5qEMPflr4z) | the run as text, `<span class="arms" role="img" aria-label="<plain name>">`; the monogram slot shows the run when arms exist (R9) (5.5a) |
| Workbench bead view in `/garden` (`gardenhub/bead_face.go`, `web/workbench.js`) | title, status, Harvest section | a **Heraldry** section: the row picture at 64 px (SVG) with the plain-words name beside it, and the **Change arms** control (§7) (5.4, 5.5b) |
| Commons cards and timeline (`internal/commons/cards.go`) | inline-SVG state marks | the row picture as inline SVG before the card header's title, 24 px (5.4, 5.5b) |
| The World (`/garden` canvas) | sprites (`gardenhub/web/art.js`) | the row picture at 24 px at a plan's plot gate, from a device-resolution bitmap; the plot's hot speaks the plain-words name; no phase stake, no task cell (Q16, R6) (5.4, 5.5b) |
| Lands contract, `gl` | `Ticket` has no visual field | `heraldry` string (the run, documented as opaque); `gl` prints it (5.3) |
| ~~Plan badge (`gb plan badge`, `internal/planbadge/`)~~ Badge (`gb badge`, `internal/badge/`) | ~~Tier-0 monogram or Tier-2 model SVG~~ | a picture drawn from the arms, for any bead (badges plan gd-pQZO9ie11D); shown only where there is room (the bead page and plan detail headers at 64 px, the deck card cover, `og:image`), never in a slot the run fills (R9); contract in [`badge-art-spec.md`](../../badge-art-spec.md) |
| coop mux web UI | claimed task `title` only | out of v1; the Glass (Lands 6.x) replaces it |
| Grafana plugin | renders no beads | nothing |

## 6. The picture: one row, drawn small

### 6.1 Why a picture at all

Only because `/garden` cannot draw a text emoji. Rubric C7 (`gardenio/docs/commons-ux-rubric.md`)
glues every glyph to a word; the goldens pin Noto Sans subsets with **no emoji font**
(`gardenio/tests/gardenhub-browser/GOLDENS.md`); `TestPageTextInGoldenFonts`
(`gardenhub/glyphs_test.go`) and `TestRenderedTextInGoldenFonts` (`internal/commons/glyphs_test.go`)
fail on any character outside the subset; and the harvest spec says "No emoji anywhere". A picture
is pixels, not text, so it keeps those contracts. Everywhere else (Slack, the beads-viewer, a
terminal) the run is text, which the owner saw in Slack prose and called great.

~~One standard heater shield with five places and a tincture, drawn at full resolution and
displayed small, wherever an image could go.~~ Struck by the verdict: the owner found the shield
form terrible. The drawing is kept as history in `gardenio/docs/heraldry/shield.md`.

### 6.2 The row

One row: the three to five charges side by side, in run order, each a square of the slot height,
with a gap of one eighth of the height between charges, on a **transparent ground**. Width is
`n × h + (n − 1) × gap`. There is **no shield outline, no field and no tincture**. The one design
decision left is the **keyline**: a 1-unit line in the opposite luminance to the surface background
around every charge, so a charge keeps its edge on the day and the night surface (Q14 stands). Day
and night differ only in the keyline; the charges are the same.

~~**The tincture palette (Q13).**~~ Struck by the verdict: there is no field to colour. The palette
table that stood here is in the git history of this file and in `round-2-sheet.md` finding 7.

### 6.3 Sizes

The same row at every size, **rendered at device resolution and displayed small** (owner, round 1).
The size key is the **height**: a slot below is a CSS height; the bitmap behind it is the slot
times the device pixel ratio (at least 2×), resampled smoothly, or it is the SVG. Nothing is
nearest-neighbour scaled and nothing is quantised.

| Slot height (CSS px) | Where |
|---|---|
| 64 | Workbench bead view header, the sample sheet |
| 40 | a `/garden` slot between the two, when one needs it (Slack and the viewer show text) |
| 24 | Commons card header, World plan plot gate |

~~16~~ and ~~8~~ are out of v1, as before (Q16, R6). State is never colour alone
(`homestead-world-design.md:325-343` rule 1): the picture carries identity, not state, and the
plot's hot carries the plain-words name (§8).

### 6.4 The renderer

`internal/heraldry` (controller) is the one place arms become pixels. Input: the run (Parse also
takes the legacy form, §4.1), a bitmap height in device pixels, and day or night. Output: PNG
bytes (content-addressed, cached as `badge/cache.go` caches badges) and an SVG of the same row,
which is the preferred form wherever a browser draws it. The charges come from the pinned artwork
set as **PNG assets at 128 px**, composited and resampled with `golang.org/x/image/draw` (no CGo,
no SVG rasteriser, no text), embedded with `go:embed` from `internal/heraldry/assets/`. The World
draws a renderer PNG sized `slot × devicePixelRatio` with canvas `drawImage`, fetched once per arms;
the label DOM uses the SVG.

Serving: gardenhub serves `/garden/arms.svg|png` with `h=` (admin-only, as step 4.1 left it). The
beads-viewer's `/arms/<key>.png` route may stay for the renderer's test harness or go (5.5a); the
viewer shows the run as text. ~~A Slack card carries the picture as a `slack_file` image block.~~
Struck: Slack cards show the run as text (§5).

### 6.5 Read model

`FarmTask` (`gardenhub/farm_model.go`), `FarmPlan` and the phase row carry `heraldry` (the run,
omitted when absent); the farm fetches the picture from the renderer by it. The plan and phase rows
also carry `heraldry_name`, the plain-words name (§8), because the canvas has no library to name a
charge with. Bounds: the farm document grows by at most 5 emoji per bead; no new cap is needed.

### 6.6 Goldens and tests

Pictures are canvas pixels, `<img>` elements and inline SVG, so the two font tests stay green.
Goldens carry one fixture plan and one fixture task with arms (`gardenhub/testdata`), shot at phone
and desktop, light and dark (C5); the `commons-ux-acceptance` skill grades the Workbench Heraldry
section and the Commons card header, shot at device pixel ratio 1 and 2. Unit tests: every library
charge has an asset; the renderer is deterministic (golden PNGs for a three- and a five-charge row
at 24, 40 and 64 px at 2×, both themes); the row geometry holds and no shield path is emitted; and
the keyline gives every charge **3:1** (WCAG 2, 1.4.11) against both surface backgrounds, measured
at the charge's median colour and its darkest and lightest deciles. ~~The contrast matrix of every
charge against every tincture~~ is struck with the tincture.

## 7. Authoring

The owner's round-1 note: "humans aren't really going to be in charge of picking this." So the
authoring surface that matters is the one an agent uses, and the human controls are for looking
and correcting.

- **The agent advice line** (gd-Nk91bVbdnp, a sibling of advice gd-jRzC8diZBD) is the main
  authoring surface. It says: arms are optional and chosen, not generated; three to five emoji
  from the library in a row, the first saying what the epic is and a later one what it is for;
  unique in the project; and it shows `kd show`'s one `Arms:` line. Its text is in
  `gardenio/docs/heraldry/README.md`.
- `kd create … --heraldry "🔥🧭📦"` or `kd update … --heraldry "🔥🧭📦"`; the help text is
  `HeraldryHelp` (R11, one text in three places, as `ShortTitleHelp` is).
- `kd heraldry check "🔥🧭📦"` before committing to arms: it prints the run, the register's arms
  with the same emoji in another order, and whether the run is free, in plain words.
- The Workbench bead view has a **Change arms** control: a row of three to five slots, a
  searchable library grid (by name and keyword), the register's collisions shown live and the
  picture updating. It is a correction tool for an operator who dislikes what an agent chose,
  not the design's front door. (~~Five slots on the shield and eight swatches~~, struck.)
- A plan's narrative bundle and the plan page show the plan's arms in their header.
- How to choose good arms: `gardenio/docs/heraldry/rubric.md` §4.

## 8. Accessibility and words

The plain-words name of arms is `arms: fire, compass, package`: the word `arms`, then the
library's names of the charges in run order, joined by commas. It is the `aria-label` of every run
and picture, the `alt` of any raster, and what the Slack card's accessible text carries. A run in
prose carries its plain-words name in a `title` where the surface supports one. Arms are never the
only carrier of a fact (R8). Where `/garden` wants arms as text (a search result, a tooltip) it
uses the plain-words name, never the notation. (~~`arms on blue: <name> top-left, <name> centre,
<name> bottom-right`~~ and the terminal block's middle dot are struck with the places.)

## 9. Questions for the owner

**All answered** (step 0.5, gd-AhHh5abzNq, 2026-10-26). Q1, Q2, Q6 and Q12 were answered by the
owner's round-0 and round-1 notes. Q13, Q14, Q16 and rubric v2 were accepted explicitly by the GO
option of the round-2 decision gd-bC2HJmTCl3 (owner, 2026-10-26 09:32Z). Q3–Q5, Q7–Q11 and Q15
were not put to the owner one by one: the GO option says step 0.5 writes the recommendations as
the answers unless the owner says otherwise, and the owner's standing override is that open
questions take the spec's recommendations. Each of those is marked *recommendation, accepted under GO gd-bC2HJmTCl3; owner may amend*. An amendment is a PR
to this table and the rules it touches.

**The owner's verdict of 2026-10-26 20:45Z** (thread gd-lvbRHMaXID, row "Verdict 10-26" at the
end of the table) answers the form questions again, and it overrides every answer below that it
touches: Q11 and Q13 are struck, Q4, Q5, Q9 and Q14 are read for the run.

| # | Question | Answer |
|---|---|---|
| Q1 | **Closed roll or any emoji?** | **Answered: a large library.** "I'd like a large library of emojis to pick from." So R3 is the pinned artwork set minus the theme's glyphs, ~1,400 charges, not 64. Not literally *any* emoji: skin-tone and ZWJ sequences and flags are out of v1 (R2/R3), and the library is still a list the system knows, because every charge needs an asset to be drawn as a picture. |
| Q2 | Which bead kinds may bear arms (R6)? | **Answered by the owner's round-1 note: epics first.** Plans, phases and other epics, and projects (Q7), in v1; tasks, bugs, decisions, escalations and agents later if at all. |
| Q3 | Do closed beads release their arms? | **No** (*recommendation, accepted under GO gd-bC2HJmTCl3; owner may amend*). Arms stay taken in the project forever; `kd update --heraldry none` on the closed bead is the one release path, deliberate. |
| Q4 | Differencing: may a child bear its parent's arms with one charge changed, as a mark of lineage? | **Not in v1** (*recommendation, accepted under GO gd-bC2HJmTCl3; owner may amend*); a later rule if wanted. Round 1 showed a differenced sample, and it cost the phase its own face; a plan and its phases read as a family by sharing a charge, never the first, instead (rubric S6). |
| Q5 | In Slack prose, where only text can go, does the emoji run replace the type glyph (R9) or sit after it? | **Replace** (*recommendation, accepted under GO gd-bC2HJmTCl3; owner may amend*). The type glyph is derivable and the arms are authored; two glyph runs before a name reads as noise, and C7's budget is tight. In cards the run sits before the short title and the type badge stays (verdict 10-26). |
| Q6 | Where the 8-px shield goes in a task cell when the top-left is taken by the sheaf (§6.3) | **Moot in v1**: tasks bear no arms (R6, Q2), so there is no cell picture. Kept for the day R6 widens. |
| Q7 | Does a project (land) bear arms, drawn on its biome sign? | **Yes** (*recommendation, accepted under GO gd-bC2HJmTCl3; owner may amend*): same field, same register (a project is in its own register). |
| Q8 | The number of sample rounds before GO | **Two** (*recommendation, accepted under GO gd-bC2HJmTCl3; owner may amend*): round 1 on the recommendations (gd-HKbQ2d25hI), round 2 on the owner's notes (gd-bC2HJmTCl3), GO on round 2. Phase 0's sample loop is closed. |
| Q9 | **Slack prose and unfurls cannot carry an image.** A `slack_file` block works in a posted card but not in `chat.unfurl` (`plan_badge_unfurl.go`), and the viewer's image URLs are OIDC-gated. Accept the emoji run as the fallback there, or stand up a public, anonymous image host for arms (an S3/CloudFront bucket of content-addressed PNGs is the smallest) so every Slack surface shows the picture? | **Moot after the verdict:** the run is the form in all of Slack, cards included, so Slack needs no image. (Before: emoji run as fallback in v1, picture in every card, *recommendation, accepted under GO gd-bC2HJmTCl3*.) A public host is a later step if the fallback grates; it is a privileged change (bucket + DNS), so it would be an escalation, not crew work. |
| Q10 | **Which artwork set?** Noto Emoji (Google, Apache 2.0 for the images, matches the Noto Sans the goldens already pin, ships 32/72/128/512 px PNGs and SVGs per codepoint), Twemoji (CC BY 4.0, needs attribution in the UI), or OpenMoji (CC BY-SA 4.0, outline style)? | **Noto Emoji** (*recommendation, accepted under GO gd-bC2HJmTCl3; owner may amend*). Same family as the pinned text fonts, the most permissive licence, a flat style with its own outlines that holds its edge on a coloured field. |
| ~~Q11~~ | ~~**The shield shape.**~~ | **Struck by the verdict 10-26:** there is no shield. (Before: heater, *recommendation, accepted under GO gd-bC2HJmTCl3*.) |
| Q12 | (From the round-1 sheet.) The 8 px uniqueness test dropped 582 of 1,333 charges; cut the library, treat 8 px as a likeness, or warn on 8 px collisions? | **Answered by the owner's round-1 note: moot.** The pixellated tier is thrown away, so there is no 8 px tile to collide and nothing is dropped for size. |
| ~~Q13~~ | ~~**The tincture palette**~~ | **Struck by the verdict 10-26:** there is no field to colour. (Before: eight plain-word tinctures with the round-2 day and night values, accepted by GO gd-bC2HJmTCl3; `round-2-sheet.md` finding 7 keeps them.) |
| Q14 | **The contrast rule** (§6.6): when a charge and a tincture fall below 3:1, (a) draw a 1-unit keyline in the opposite luminance around every charge so every pair passes, or (b) refuse the pair in the register and let `kd heraldry check` say so? | **Accepted by the owner (GO gd-bC2HJmTCl3): (a), the keyline, not refusal.** Round 2 measured the cost of (b): 7,027 of 21,328 cells fail, gold and sand refuse two thirds of the library, and 58 of 137 sampled arms would have been sent back. The keyline passes every pair and is barely visible at 24 px. **Verdict 10-26:** the keyline stays, drawn against the surface background instead of a field (§6.2). |
| Q15 | **Epics humans author.** With agents picking arms (R7), a plan a human creates from Slack or the Workbench has none unless someone gives it some. Leave it (arms are optional), or let the groomer that fills short titles propose arms for it too, per project? | **Leave it in v1** (*recommendation, accepted under GO gd-bC2HJmTCl3; owner may amend*), and record the count of unarmed human-authored epics after Phase 5; if it grates, the groomer is a later step, opt-in per project, and R1 is amended then. |
| Q16 | **The farm's 16 px phase stake** (§6.3): keep it if a 3× bitmap in that slot reads, or show arms on the farm only at the 24 px plan plot gate? (The 8 px task cell is already out with task arms, R6.) | **Accepted by the owner (GO gd-bC2HJmTCl3): the 24 px plot gate only; the 16 px stake is dropped from v1.** At 2× and 3× the stake is a likeness, not the arms: no charge can be named, and at 1× it is a coloured blot. Phase 4 may revisit it at 3× once the gate is live. |
| Rubric v2 | (From the round-2 sheet.) Accept the four proposals for `gardenio/docs/heraldry/rubric.md`? | **Accepted by the owner (GO gd-bC2HJmTCl3).** (1) S2 for phases: a phase passes S2 when its plan's arms carry the purpose, or it may bear four charges with the shared one at TL. (2) G3's objective part: "at least as large as any corner" is replaced by "not a thin or low-contrast shape next to a larger, brighter corner", with the eye as the judge. (3) G4: flag pairs whose mean 24 px redmean distance is under 40, and judge those by eye. (4) Phases taking their plan's tincture is kept, noting it costs about one G4 fix in ten phases, so a sibling needs a centre of a clearly different colour. |
| Verdict 10-26 | (Owner, thread gd-lvbRHMaXID, 2026-10-26 20:45Z, before the 5.1 walk.) | **"okay so i learned that the shield form is terrible, but linear is great; so let's just make it like that everywhere; 3-5 linear symbols per epic, and we can display it all sorts of places. final answer".** Arms are three to five emoji in a row (R2), canonical as the run (§4.1), unique as the run (R5), shown as text wherever text emoji can go and as a row picture in `/garden` (§5, §6). The shield, the die places and the tincture go (R13, Q11, Q13 struck). Rubric v3 (`gardenio/docs/heraldry/rubric.md`) follows: the run reads left to right, what it is first, what it is for next. Built in Phase 5, steps 5.3 to 5.7 (§11). |

## 10. The sample loop (Phase 0)

Nothing in §3–§7 is built until the owner has seen arms on real beads and said GO. Phase 0 is:

1. **Round 1 sheet.** Pick ~40 live beads: every open plan (Lands Track A gd-vmw5s07A1F and
   Track B gd-X07JyIHEzl, the Homestead skin gd-JaMxRKsc8z, the birds gd-gAKb8Bf0Nm, the goats
   gd-EGpbDpqsec, costs gd-obL67RJ1td, plans-first-class gd-QjJScyWs36), six phases of Track A,
   fifteen recent tasks and bugs across projects, three decisions, three escalations, three
   agents, two projects. Propose arms for each by hand (a Fable-tier agent reading the bead,
   under R2–R6 and the library), and record the reasoning in one line per bead so the
   semantics can be judged ("🌱🔥📦: a seed of a feature, hot path, ships a package").
2. **Render every form** on a static sheet, with the renderer prototype (§11 0.1): the picture
   at the 64, 40, 24, 16 and 8 px slots, drawn at 1× and 2× and (round 2) 3× device resolution,
   day and night, on each tincture; the same arms in the four
   candidate shield shapes (Q11) and three artwork sets (Q10), once; `kd show`'s terminal block;
   a Slack agent card with the picture as an image block, and a References footer with the emoji
   run, posted to the Slack TEST workspace and shot with `gb slack view` (the
   `slack-ux-acceptance` skill's harness); the picture at the plot, stake and cell sizes, day
   and night, on the golden harness's fixture farm (the `commons-ux-acceptance` skill's harness,
   so the shots are real Chromium on the real canvas); the Workbench bead view header mock with
   the control. The library is on the sheet as a contact sheet of every charge at 16 px and
   (round 2) the contrast matrix of §6.6 with its failing pairs marked.
3. **Post** the sheet as a `type=report` bead on the plan (the Homestead skin plan's P3 report
   gd-QDql2hSmTn is the precedent) and the PNGs in the Slack thread gd-zhMrQAlMhS, with a
   `gb decision` whose options are GO, iterate (with the owner's notes), or stop.
4. **Iterate.** Each "iterate" answer mints the next round as a new step under Phase 0 (the
   plan gardener does this); rounds change the library, the rules in §2 and §9, the shield drawing, or the
   proposed arms, and the spec is amended by PR per round so the spec always says what the sheet
   shows.
5. **GO** closes Phase 0 and unblocks Phase 1. The accepted sheet's arms become the first entries
   in the registers when Phase 1 lands (one `kd update --heraldry` per sampled bead, by hand).

**Round 1, posted 2026-10-26 00:48Z** (step 0.3, PR #1099, `gardenio/docs/heraldry/round-1-sheet.md`,
decision gd-HKbQ2d25hI). The owner's notes in the thread, 01:43Z to 01:46Z, in order:

1. "i propose we render small but higher res images in the glyph, also humans aren't really going
   to be in charge of picking this"
2. "yeah the pixellated versions can pretty much be thrown away, there's no way to make that work"
3. "also, clearly, the background color needs to be load bearing as well or we are throwing a lot
   away?"
4. "but we need to be sure the contrast works out"
5. "and it looks like we should prioritize showing heraldry in places where we actually have some
   room, right?"
6. "let's produce a rather large list of sample heraldry with pictures based on real beads once
   we have a rubric ok?"
7. "also it's becoming clear heraldry will be more important for epics than tasks; quite possibly
   only epic level things should get heraldry to start"
8. "like a plan, or a phase, you know? since it sticks around longer than e.g. a decision or an
   escalation"

These are the **round-2 brief**, in two steps. **0.4a, the rubric** (`gardenio/docs/heraldry/
rubric.md`, the `commons-ux-rubric.md` pattern): what good arms are, semantically (the charges
say what the epic is and what it is for, in the order a reader scans them; no charge that is a
pun only its author gets; no charge that says the type, which the type glyph already says; a plan
and its phases read as a family without differencing) and graphically (the tincture suits the
charges and the contrast matrix passes; the centre charge is the one that must survive at 24 px;
no two arms in a project share a silhouette at 24 px), each as a numbered check an agent can
apply and a reviewer can score. **0.4b, the large sample**: once the rubric is merged, arms for
**every open epic on the farm**, plans and phases first (the farm holds about 130 open
epics, 32 of them plans; the sheet takes every open plan and its phases and every other open
epic, well over a hundred arms, each with its one-line reason and its rubric score), rendered with the
round-1 changes: the picture at device resolution in every slot and no quantised anything (§6.3,
§6.4); the same arms on all eight tinctures, day and night (R13, Q13); the contrast matrix with
the keyline and the refusal shown side by side (§6.6, Q14); the 16 px stake at 2× and 3× so Q16
can be answered; the sheet ordered by room, the 64 and 40 px pictures first. Round 1's 47 arms
are kept where the bead is an epic and dropped where it is a task, decision, escalation or
agent (R6). Round 1's Q12 is moot. Round 2 is posted the same way (report bead, thread,
decision).

**Round 2, posted 2026-10-26** (step 0.4b, PR #1115, `gardenio/docs/heraldry/round-2-sheet.md`, report gd-ax0mMd1BAz, decision gd-bC2HJmTCl3, data in
`samples/round-2/`). 137 open epics (33 plans, 92 phases, 12 other epics) scored against the rubric:
43 accept, 75 accept with notes and 19 revise with the keyline; refusing pairs instead (Q14 (b)) would
send 58 arms back on G2 alone. The sheet proposes the eight tinctures' day and night values (Q13), and
recommends the keyline (Q14 (a)) and the 24 px gate without the 16 px stake (Q16). It also proposes
four rubric v2 changes: S2 for phases, G3's size rule, a G4 threshold of 40, and the cost of phases
taking their plan's tincture.

**GO, 2026-10-26 09:32Z** (decision gd-bC2HJmTCl3). The owner accepted round 2's arms, its look,
and its recommendations: the eight tinctures as drawn (Q13), the keyline (Q14), the 24 px gate
only (Q16) and rubric v2. Phase 0's sample loop is closed; step 0.5 wrote the answers into §9
and set this spec to accepted. The round-1 decision gd-HKbQ2d25hI is superseded by round 2.

**Verdict, 2026-10-26 20:45Z** (thread gd-lvbRHMaXID). By evening the shield was on the
surfaces (Phases 2 to 4) and the run was in Slack prose. Before the 5.1 walk the owner gave the
form a final answer: the shield form is terrible, the linear form is great, three to five symbols
per epic, everywhere (§9, row "Verdict 10-26"). No new sample round: Phase 5 steps 5.3 to 5.7
rebuild the form in place (§11), and the 5.1 walk grades the run. The round-1 and round-2 sheets
stay in `gardenio/docs/heraldry/` as history.

## 11. Build steps

Phases map one to one onto the farm plan. Each step is one PR unless noted. Phase 1 started on
the owner's override of the Phase 0 GO gate (plan comment 22:36Z); Phases 2 and 3 wait on Phase
1 and run in parallel; **Phase 4 (the World) is last** and draws only the 24 px plot gate
(Q16: no 16 px stake in v1; owner, round 1: room first); Phase 5 waits on 2–4.

**Phase 0, Samples and rules (no product code).**
0.1 This spec merged as a draft (PR #1068) and revised on the owner's round-0 note (PR
#1076); done as PR #1093, with the quantised path round 1 then removed. The **library** as `gardenio/docs/heraldry/library.md`: the pinned artwork set and
its version, the exclusion lists (theme glyphs, modifiers, ZWJ sequences, flags, the few that
fail the 8-px uniqueness test), the count, and the standard shield drawing (§6.2) as an SVG with
its design grid. A **renderer prototype** (a throwaway `go run` under `scripts/`, not product
code) that composites the shield from the artwork PNGs at 64/40/24/16/8, smooth and quantised,
day and night, so the sheet is made by the same drawing rule the product will use; the prototype
also tries the artwork SVGs through `oksvg` and records which path the product takes. 0.2 Round
1: arms proposed for ~40 real beads with reasons. 0.3 Round 1 sheet rendered in every form on the
two harnesses and posted (report bead + thread + decision; done, PR #1099). 0.4 Round 2 on the
owner's eight round-1 notes (§10), in two steps: 0.4a the rubric (`gardenio/docs/heraldry/
rubric.md`) as its own PR, so the owner can correct it before it is applied; 0.4b the large
sample, arms for every open plan and phase and a sample of the other epics, scored against the
rubric, with the prototype grown to the tincture, 2× and 3× output and the contrast matrix and
shorn of the quantised path, re-rendered and re-posted with a decision (further rounds as
minted). 0.5 GO check-in: the owner's answers to Q2–Q16 written into
§9, spec status set to accepted (done on the round-2 GO gd-bC2HJmTCl3).

**Phase 1, The field.** 1.1 kbeads: column, migration, library package (the charge list with
codepoints and names, generated from the artwork set's manifest; no images in kbeads), parser and
canonical form, validator, register uniqueness (index + 409), near-duplicate warning, HTTP/batch
shape, list filter (in flight as PR #1096 on the five-token form). 1.1b (round 1): the tincture
token in the canonical form and the parser, the palette table in the library package, the
`on <colour>` input, the system's default tincture choice (R13), the register key widened to
tincture + tuple, the warning for the same tuple on another tincture, and R6 narrowed to epics
and projects; lands after 1.1 so #1096 is not reopened for it. 1.2 kbeads: proto and gRPC. 1.3 kd: `--heraldry` on create/update taking
emoji (bare or placed), `kd show` terminal block, `ARMS` column (emoji run), `kd heraldry
check` (validates, prints the terminal block and the register's collisions; no notation in the
output). 1.4 controller `beadsapi` + `gb` emoji run in `news`, `ready`, `plan show`, `bundle
show`. 1.5 Lands contract v1: `heraldry` on Ticket, TicketCreate, TicketUpdate, Plan, PlanStep;
fixtures; landmap keys; version bump; `gl` prints the emoji run. 1.6 Advice line, `HeraldryHelp`
(plain words: "one to five emoji, placed like the five on a die, unique in the project"), docs
page, the sampled beads given their arms.

**Phase 2, The renderer and Slack.** 2.1 `internal/heraldry`: embedded artwork assets for the
library (128 px), the shield drawing with the tincture palette, PNG output at any device-pixel
size and SVG output, day and night, no quantised path and no sprite sheet, content-addressed
cache, golden tests and the contrast matrix test (§6.6); the keyline per Q14. 2.2 Bridge: `beadDisplayName` and link hydration show the emoji run in
place of the type glyph (R9, Q5); the library/theme disjointness test. 2.3 Slack cards: the
picture as a `slack_file` image block beside the header on agent, decision, escalation, thread
and wrap-up cards, uploaded at 3× the 40 px slot once per arms and the file id kept by content
hash; unfurls show the emoji run (Q9). 2.4 `slack-ux-acceptance` pass on every changed card, before/after PNGs on the
step.

**Phase 3, Web surfaces (viewer, Workbench, Commons).** 3.1 beads-viewer: `/arms/<hash>.png|svg`
route (SVG preferred, PNG with `srcset` at 2× and 3×); `<img class="heraldry">` at 64 and 40
first, then 24 and 16, in the bead page, plan cards/decks/peek/cmd-k and lists; arms displace
the Tier-0 monogram; `alt` in plain words. 3.2 Workbench bead view: Heraldry section with the
picture at 64 px (SVG) and the plain-words name; then the **Change arms** control (five slots on
the shield, eight swatches, a searchable library grid by name and keyword, register collisions
shown live), a correction tool (§7). 3.3 Commons cards: the picture at 24 px in the card header as inline SVG; font
tests stay green. 3.4 `commons-ux-acceptance` pass and goldens for 3.1–3.3 (phone, desktop,
light, dark).

**Phase 4, The World (last; owner, round 1: room first).** 4.1 `art.js` and a `heraldry.js`:
the 24 px plot-gate picture drawn with `drawImage` from a renderer PNG sized by
`devicePixelRatio`, fetched once per arms; no sprite sheet, no quantised path. 4.2 Read model:
`heraldry` on `FarmTask`, `FarmPlan` and the phase row; bounds doc updated. 4.3 Placement: plan
plot gate (24) only; no phase stake (Q16 drops it from v1), no task cell or rollup stamp
(tasks bear no arms, R6); the plot's hot `aria-label` in plain words.
4.4 Goldens with the fixture plan and task bearing arms, at device pixel ratio 1 and 2;
looking-glass gate green.

**Phase 5, Acceptance.** 5.1 The owner opens Slack, the viewer, `/garden` on laptop and phone,
day and night, finds the sampled beads wearing their arms, and says GO or what to change.
5.2 Release note and the `gardenio/docs/heraldry/` page finished; spec status set to shipped.
After the verdict of 10-26 (§9), Phase 5 also rebuilds the form as the run, and 5.1 waits on all
of it: 5.3 (gd-InkEAAqGNM) the model in kbeads and the Lands contract: Parse takes the run and
reads the legacy form, Canonical is the run, uniqueness on the run, `HeraldryHelp` and
`PlainName` rewritten, `kd show` one line. 5.4 (gd-yYpzqd0YJG) the renderer draws the row
(§6.2), keyed by height. 5.5a (gd-nLVyw2i9fN) Slack cards and the beads-viewer show the text run;
the `slack_file` image block goes. 5.5b (gd-vuc7csqyeL) the `/garden` surfaces draw the row. 5.6
(gd-LGIxGuLZ9y) the register: a third charge for the twelve two-emoji arms and every stored value
rewritten to the run. 5.7 (gd-vQobmSXmne) this spec, the rubric, the README and the advice line.

## 12. Risks

Written for the first form. After the verdict of 10-26 the risks about Slack's missing image slot,
the eight tinctures, the contrast matrix and the register key's widening are moot; the rest stand.

- **Artwork size in the repo and the binary.** ~1,400 PNGs at 72 px from Noto Emoji are ~6 MB
  embedded in the controller; the SVGs would be ~8 MB. Acceptable for one binary, but the
  library is pruned to what the exclusion lists leave, the assets live in one directory with a
  generator script and a pinned upstream version, and gitleaks/quench budgets are checked in
  2.1 before the assets land.
- **Licence and attribution.** Noto Emoji images are Apache 2.0; the LICENSE file is vendored
  beside the assets and the docs page names the source. If the owner picks Twemoji (Q10) the UI
  needs a visible attribution line, which is a design cost.
- **Slack prose shows text emoji after all.** The owner asked not to rely on text emoji; Slack
  prose and unfurls have no image slot the bridge can fill without a public host (Q9). The
  fallback is honest and small, but it means the shield's shape is lost in one surface. If that
  grates, the public host is the fix and it is an escalation.
- **Register collisions in a busy project.** ~1,400 charges on 5 places and 8 tinctures make
  collisions vanishingly rare on the key, and with epics only (R6) a register holds tens of arms,
  not thousands; the near-duplicate warning (same set, different places; same tuple, other
  tincture) is what matters. The sheet shows how often two proposers reach for the same three
  charges, and the rubric's silhouette check (0.4a) is the real guard.
- **Contrast across 1,333 charges × 8 tinctures.** Noto's artwork spans every hue, so some
  charge will sit on a field of nearly its own colour. §6.6 measures every pair; Q14 settled it
  with a keyline on every charge (one drawing rule, no refusals), so an agent has no refusal
  matrix to learn. The round-2 sheet showed the failing pairs both ways.
- **A smooth picture in a pixel-art World.** The arms are the one thing on the canvas not drawn
  in the palette or at the farm's pixel size. The owner chose this knowingly (round 1: the
  pixellated versions cannot work). The plot gate is a 24 px slot on a sign, which is where a
  notice board carries a photograph; the sheet shows it in place so the clash is judged, not
  argued. The canvas must honour `devicePixelRatio` for the `drawImage` to be sharp; the
  prototype's farm shots check this on the golden harness at ratio 1 and 2.
- **The register key widens.** Phase 1's 1.1 (PR #1096) keys the register on the five-token
  form; 1.1b widens it to tincture + tuple with a migration that gives existing rows the system's
  default tincture. Nothing is in a register before Phase 1 ships, so the migration is formal.
- **Glyph budget on cards (C7).** An image block is not a glyph, so a card with the picture
  spends none of the six; the emoji run in prose spends up to five. Any prose line that already
  shows two state glyphs drops one or shows the run trimmed to the centre charge. 2.4's
  acceptance pass catches this per surface.
- **The 16×16 cell is full.** Priority flag, harvest badges, beetle, frost and drops compete
  for it, and 8 px is the slot with the least room. R6 settles it for v1: tasks bear no arms, so
  nothing is added to the cell. If R6 ever widens, Q6 and the crowded-cell shots come back.
- **Slack cards hold a title, not a bead.** `beadTitleLink(id, title)` is called from ~20 sites
  in the decision and escalation builders with no bead in hand. Step 2.3 either threads the bead
  through or adds a `beadTitleLinkArms(id, title, heraldry)` and leaves the plain form for sites
  that have only an id; the sweep found the call sites (`bot_escalations.go:186-744`,
  `bot_decisions.go:92-629`, `bot_beadactivity.go`, `bot_formula.go`, `sla_nudge_notify.go`).
- **`garden.js` is near its quench cap** (`quench.toml:85`); all World picture code goes in
  `art.js` and a new `heraldry.js` for the DOM picture, as the harvest spec did.

## 13. Not in this spec

Differencing and cadency (Q4) beyond a sample; arms for people (user beads); a heraldry
generator or groomer; ~~Tier-2 plan badges drawn from arms~~; arms in Grafana; ~~OpenGraph images with
arms~~; a public image host for Slack unfurls (Q9) beyond naming it; the farm drawing charges by any
means but the renderer's pictures; any pixellated or palette-quantised form of the arms. Since the
verdict of 10-26: the shield, its five places and the tincture, in any surface.

Since badges from arms (plan gd-pQZO9ie11D, decision gd-eThga7vg3I; step 10, gd-XWocW0x1fq), two
of the items struck above are **in scope**, under their own contract,
[`badge-art-spec.md`](../../badge-art-spec.md):

- **Tier-2 badges drawn from arms, for any bead.** `gb badge <bead-id>` draws a picture from the
  bead's arms, or from the nearest arms in its line. The arms stay the mark; the badge is a
  derived picture and never fills a slot the run fills (R9).
- **`og:image` through the viewer's image proxy.** The bead page and plan detail page carry the
  uploaded badge PNG as `og:image`, an absolute URL through the viewer's `/image/` proxy. The
  proxy is admin-only, so the image loads for a signed-in reader.

Still **not** in scope: a public image host for Slack unfurls (Q9). Slack's unfurler cannot load
the proxied `og:image`; a badge reaches Slack only as a file upload (`gb slack upload`).
