# Heraldry: arms for an epic

An epic can bear **arms**: three to five emoji in a row, read left to right, such as 🔥🧭📦. Arms
are a face for a bead that lasts, read in a glance the way a short title is read in a breath. The
short title stays the name, and the arms are the face. Arms are optional, they are unique within a
project, and in v1 only epics (plans, phases, any other `type=epic` bead) and projects bear them.

Arms are **chosen, not generated**. The agent that files or holds an epic picks its arms. No
groomer fills them in and nothing is backfilled. A human changes arms only to correct them.

Spec: [`2026-10-25-heraldry-design.md`](../superpowers/specs/2026-10-25-heraldry-design.md)
(plan gd-N6I3Oa7uDg). What good arms are: [`rubric.md`](rubric.md). What shipped:
[the release note](../release-notes/2026-11-04-heraldry.md).

> **Status: shipped.** Live in 2026.307.2 (k3s prod rev 272), and accepted by the owner on
> decision gd-dmNV26eh5D (GO, 23:59Z 11-03). Every stored value is in the run form this page
> describes.

## Giving an epic arms

1. **Read the epic.** For a phase, read its plan and the plan's arms too. Write one sentence that
   says what the epic is and what it is for.
2. **Choose three to five emoji, in order.** The first says what the epic is. A later one says
   what it is for. Three is the usual count; use four or five only for an epic with that many
   parts. [`rubric.md` §4](rubric.md#4-choosing-arms-how-an-agent-applies-the-rubric) gives the
   full order, with worked examples.
3. **Find them by name**, if you need to: `kd heraldry library --search fire`.
4. **Check them**: `kd heraldry check "🔥🧭📦" --project gardenio`. This prints the run, its
   plain-words name, and whether another bead of the project already bears it or bears the same
   emoji in another order. It exits non-zero when the arms are invalid or taken.

   ```text
   Arms:       🔥🧭📦
   In words:   arms: fire, compass, package
   Project:    gardenio: free, no other bead bears these arms
   ```
5. **Give them**: `kd update gd-XXXX --heraldry "🔥🧭📦"`, or `kd create … --heraldry "🔥🧭📦"`
   for a new epic. `kd update gd-XXXX --heraldry none` removes them.

`kd show` then prints the arms on one line:

```text
Arms:        🔥🧭📦
```

Give arms when the epic is worth learning by sight: a plan, a phase, a long-lived epic. A task,
bug, decision, escalation or agent is refused, because those beads do not last. They show their
line's arms instead (see [The line's arms](#the-lines-arms)).

To correct arms, give the new run with `kd update`. In `/garden`, the Workbench bead view's
**Change arms** control does the same, for a person fixing an agent's pick.

## The help text

The `kd create --heraldry` and `kd update --heraldry` flag help, the `kd heraldry` help, the
daemon's refusals, `GET /v1/heraldry/library` and the advice line all quote one constant,
`model.HeraldryHelp` (`kbeads/internal/model/validate.go`). It reads:

```text
heraldry is a bead's arms: an optional mark of three to five emoji in a row, unique in the project, for epics (plans and phases) and projects. Type them in order (🔥🧭📦). Emoji that Slack cards already use for bead types and states are not in the library. 'none' removes the arms.
```

`kbeads/internal/model/validate_test.go` checks that this page quotes `HeraldryHelp` verbatim in a
`text` block, so update the two together. The tests in `kbeads/internal/heraldry` check that
every example in it parses and that it uses no internal word.

`kd heraldry --help` puts the steps above in front of the help text (`heraldryLong`,
`kbeads/cmd/kd/heraldry.go`).

### What the daemon says

Every message uses plain words. The stored form and the internal terms never reach a person.

| The value | The answer |
|---|---|
| fewer than three or more than five library emoji | 400 `invalid arms: arms are three to five emoji. <HeraldryHelp>`, and `kd` says the same before it sends |
| an emoji twice | 400 `invalid arms: 🔥 is used twice. <HeraldryHelp>` |
| set on a task, bug, decision, escalation, agent … | 400 `a task bead cannot bear arms yet, because arms are for the beads that last. <HeraldryHelp>` |
| on a bead with no `project:` label, or with two | 400, naming the labels |
| the same run as another bead of the project | 409 `heraldry_taken`, naming the bead that bears it (gRPC `AlreadyExists`) |
| the same emoji as another bead's arms in another order | allowed, with an `X-Beads-Warning` that they may be hard to tell apart |
| no arms on an epic create (`BEADS_HERALDRY_GATE`, below) | allowed, with an `X-Beads-Warning` that `kd create` prints as `warning: heraldry missing (kd update <id> --heraldry "...")`; under `enforce`, a 400 for an agent's plan or phase epic |

### The create gate

`kbeads/internal/server/heraldry_gate.go` (gd-fn7uPujOqj) nudges an epic create that carries no
arms, staged warn then enforce like the short-title gate. It reads nothing from the register.

| Env | Chart value | Default | Applies to |
|---|---|---|---|
| `BEADS_HERALDRY_GATE` | `beads.heraldryGate.home` | `warn` | epics in the home garden |
| `BEADS_HERALDRY_GATE_TENANT_LANDS` | `beads.heraldryGate.tenantLands` | `off` | epics in a project with `land_kind=tenant` |

`warn` allows the create, returns the warning and counts
`gardenio_beads_heraldry_gate_total{action="allowed_warn"}`. `enforce` refuses, with the same
words, a non-human caller's epic labelled `plan` or created with a `parent_bead_id` that is a plan;
a human, and any other epic, is still only warned. `off`, or any value it does not know, turns it
off. `kd create --parent` and `gb plan`'s phase create send the parent as `parent_bead_id`, so the
daemon sees a new phase as one.

## The advice line

Agents learn to pick arms from one advice bead, **gd-Nk91bVbdnp**, a sibling of the short-title
advice gd-jRzC8diZBD, labelled `global` and `project:gardenio` like it. Editing advice needs
`gardenio-admin`, so a change to it goes through a `beads_admin_edit` escalation (as step 1.6b
did, gd-RMkrgcsAkZ). Its text:

```text
You pick the arms for an epic you file or hold (a plan, a phase, any type=epic bead). Nobody else will: arms are chosen, not generated. Read the epic, choose three to five emoji in a row that say what it is (first) and what it is for, check them, then give them:

  kd heraldry library --search <word>
  kd heraldry check "🔥🧭📦" --project gardenio
  kd update gd-XXXX --heraldry "🔥🧭📦"      (or kd create ... --heraldry "🔥🧭📦")

kd show then prints the arms on one line:

Arms:        🔥🧭📦

<model.HeraldryHelp>

How to choose good arms: gardenio/docs/heraldry/rubric.md §4. Doc: gardenio/docs/heraldry/README.md.

Plan: gd-N6I3Oa7uDg (heraldry).
```

## Where arms show

The rule: the **text run** (🔥🧭📦) wherever text emoji can go, and the **row picture** (the same
emoji drawn in one row from the pinned Noto artwork) only where they cannot, because `/garden` is
emoji-free by contract (Commons rubric C7, the goldens' fonts). Either way the run sits right
before the short title, as one glyph glued to it.

| Surface | Form |
|---|---|
| `kd show` | an `Arms:` line with the run |
| `kd list`, `kd search` | an `ARMS` column (the run) when a row has arms |
| `gb news`, `gb ready`, `gb plan show`, `gb bundle show` | the run before the short title |
| Lands contract, `gl` | an optional `heraldry` string on tickets, plans and plan steps (the run); `gl` prints it |
| Slack prose, References footer, unfurls | the run in place of the type glyph |
| Slack cards (agent, thread, wrap-up, decision, escalation) | the run before the short title in the header line; the type badge and state glyph stay |
| beads-viewer rows, cards, plan peek, cmd-k, bead page | the run as text, with the plain-words name as its accessible name |
| Workbench, Commons cards | the row picture, 64 px in the Workbench and 24 px in a Commons card header |
| `/garden` World | the row picture at 24 px at a plan's plot gate |

The plain-words name, the accessible name of every run and picture, is `arms: fire, compass,
package`.

### The line's arms

A bead that bears no arms of its own shows its **line's arms**: the run of its nearest armed
ancestor (task to phase to plan to project). It never bears them, so nothing is written on it and
the register is not touched.

| Who | Where it shows the line's arms |
|---|---|
| A task or bug in `/garden` | the Yard's Mailbox and Growing rows, the Yard plant's accessible name, the bead view header |
| An agent bead | the `/garden` bead view and the viewer's agent page, before the assigned task; the Slack agent card, before the task's short title. The line is the assigned task's (`fields.task_id`) |
| A gardener on the map | the gardener card, tip and plot roster, before "Tending" or "Assigned"; a small badge over the figure's head at the Plan and Bead levels |
| A goat (an open wish) | the goat card and the pen gate hover, before the wish title: its kin's lines, two runs at most. The sprite and the headstone carry none |

A bead with no armed line shows nothing, and leaves no gap.

## Badges

A **badge** is a picture drawn from a bead's arms (plan gd-pQZO9ie11D). The arms are the mark,
and the badge is the picture: the run fills every small slot, and the badge shows only where there
is room for a picture someone remembers, the bead page and plan detail headers (64 px, beside the
run), the cover of a deck card, `og:image`, a Slack upload. A badge is derived, never chosen: give
the epic arms, and draw the badge from them. Contract: [`badge-art-spec.md`](../badge-art-spec.md).

`gb badge` works on any bead. A bead with no arms of its own is drawn from the nearest arms in its
line (task to phase to plan to project), and a line with no arms is drawn from the title and
description only; `gb badge` says which on stderr.

```sh
gb badge gd-XXXX                                   # the poster, as SVG on stdout
gb badge gd-XXXX --png --size 512 --out badge.png  # the poster at 512 px
gb badge gd-XXXX --upload --size 512               # upload, write icon_svg_url / icon_png_url onto the bead
gb badge gd-XXXX --refresh --upload --size 512     # redraw after the picture went wrong
gb badge gd-XXXX --style line-art-bold --png --size 40 --out small.png   # a slot under 128 px
gb badge gd-XXXX --json                            # tier, cache key, arms, arms_from
```

- **Styles.** `poster` (the default, drawn by `claude-opus-5-5`) is judged at 128 px and above,
  and `gb badge` refuses a smaller `--size` for it. `line-art-bold` and `line-art-subtle` read at
  24 px. The owner picked the defaults from [`badges-round-1.md`](badges-round-1.md).
- **New arms, new picture.** The cache is keyed on the bead id, title, arms, style and prompt
  version, so changing the arms draws a new badge on the next `gb badge`. Re-run
  `gb badge gd-XXXX --upload --size 512` to put it on the bead; the viewer shows it on the next
  page load.
- **Always a badge.** With no model reachable, `gb badge` falls back to a deterministic monogram
  (Tier-0) and says so; `--tier0` asks for it.
- **Who can see it.** The upload goes through the viewer's admin-only `/image/` proxy, so the
  headers and `og:image` show it to a signed-in reader. Slack's unfurler cannot load it; to show a
  badge in Slack, upload the PNG with `gb slack upload`.

## In this directory

| File | What it is |
|---|---|
| [`rubric.md`](rubric.md) | what good arms are: the checks, and how an agent applies them |
| [`library.md`](library.md) | the emoji arms can be made of, and the ones left out |
| [`badges-round-1.md`](badges-round-1.md) | the badge sample sheet the owner picked the `gb badge` defaults from |
| [`round-1.md`](round-1.md), [`round-1-sheet.md`](round-1-sheet.md), [`round-2-sheet.md`](round-2-sheet.md) | history: the sample rounds of the first form |
| [`shield.md`](shield.md), [`shield.svg`](shield.svg), [`shield-grid.svg`](shield-grid.svg) | history: the first form's drawing |
| [`samples/`](samples/) | the renders behind the sheets |

## History: the first form

Until the owner's verdict of 2026-10-26 20:45Z, arms were one to five emoji placed like the five
on a die on a shield of one colour (a tincture), and `kd show` drew that shield on three lines.
The round sheets and the shield files above record that form; they are kept as history, not
guidance. Step 5.6 (gd-LGIxGuLZ9y) rewrote every stored value to the run; a value in the old
form (`blue 🔥 . 🧭 . 📦`) still reads as the run, the emoji in their old place order. Until step
5.3 (gd-InkEAAqGNM), `kd` quoted this help text:

```text
heraldry is a bead's arms: an optional mark of one to five emoji, placed like the five on a die on a shield of one colour, unique in the project, for epics (plans and phases) and projects. Type the emoji in order (🔥🧭📦) and they are placed for you: one goes in the centre, two on the diagonal, three top-left, centre and bottom-right, four in the corners, five on every place. To choose the places yourself, name them: tl:🔥 c:🧭 br:📦 (tl, tr, c, bl, br). Add the shield's colour last, 🔥🧭📦 on blue (blue, red, green, gold, purple, brown, slate or sand), or leave it out and the colour that shows the emoji best is chosen for you. The same emoji in the same places on another colour are other arms. Emoji that Slack cards already use for bead types and states are not in the library. 'none' removes the arms.
```
