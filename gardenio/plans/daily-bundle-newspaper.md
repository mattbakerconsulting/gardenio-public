# The Daily Bundle — Design

*A daily newspaper, rendered as a `type=bundle` bead.*

## The concept

Every day the gardenio fleet opens and closes hundreds of beads, files bugs,
ships releases, and fights infrastructure fires. That activity is *legible* —
it all lives in kbeads — but it isn't *readable*. **The Daily Bundle** turns
the previous day's bead activity into a one-page newspaper: a front page with a
lead story, three columns of articles, a statistics ledger, and an "In Brief"
rail.

It exploits a happy accident of the bundle viewer: the `html` doc format renders
inside a **sandboxed `<iframe srcdoc>`** (XSS-safe by construction,
`ka-0000001859`). That means a single self-contained HTML document — masthead
typography, multi-column CSS, drop caps and all — renders as a true broadsheet
inside beads-viewer at `/bundle/<id>`. No new renderer, no schema change.

## Why a bundle bead is the right vessel

| Requirement | How the bundle delivers |
|---|---|
| One shareable artifact per day | The bundle bead *is* the issue. `/bundle/<id>` is the permalink. |
| Multiple sections | Each section is a `type=doc` child with a `position`. |
| Lives in the graph | Parented to an "origin" bead; every story cites real `kd-` IDs that linkify. |
| Browsable in sequence | The viewer's TOC + single-column reading view = the paper's running order. |
| Mix of rich + plain | Front page is `html`; editorial/colophon are `md`; a raw "wire" can be `csv`. |

## Anatomy of an issue (2nd edition)

```text
origin bead ──parent-child──▶  ☐ bundle:  "The Daily Bundle — <DATE>"   (kind=report)
                                   │
   pos 0  html  ──────────────────┤  📰 Page One    (ticker, masthead, lead, 3-col front; read-more)
   pos 1  html  ──────────────────┤  📈 Page Two    (Markets exchange, CI-min futures, infra desk)
   pos 2  md    ──────────────────┤  📖 Inside      (full long-form articles + ⚰ Obituaries)
   pos 3  csv   ──────────────────┘  🔌 The Wire    (machine-readable index of notable beads)
```

The TOC at `/bundle/<id>` lists the four sections, so the paper reads as a
genuine multi-page broadsheet — front page, an infrastructure-and-markets
inside page, the long-form section, and the appendix-of-record wire.

### Below-the-fold & links

- **Read more / below the fold.** Each front-page story shows a headline, deck,
  and lede, then a native `<details>`/`<summary>` ("Read more ▾") expands the
  full article below the fold. `<details>` needs no JS, so it works inside the
  viewer's bare-`sandbox` iframe.
- **Every bead is a link.** The html pages use *relative* `<a href="/ka-X">`
  anchors. The viewer wraps html docs in `srcdoc`, which resolves relative URLs
  against the parent page — so the links navigate the frame to the real bead on
  whatever host the viewer runs on (popups via `target=_blank` are blocked by
  the bare sandbox; relative in-frame navigation is not). The `md` "Inside"
  page additionally relies on the viewer's bare-`kd-` linkify path, so its prose
  is a fully clickable index. Net effect: every bead reference is live in both
  the rich and the plain surfaces, with **no viewer code change**.

### Markets & Obituaries columns

- **Markets — "The Fleet Exchange":** the day's release-tag ticker
  (e.g. 20 RC tags on Jun 13), prod-golden version, bead open/close volume as
  "indices," and a CI-minute "futures" box (e.g. the ~10k min/mo the release-DAG
  epic projects). Rendered as a stock-style table + callout boxes.
- **Obituaries:** beads retired, superseded, or stopped that day —
  decommissioned gardeners, superseded hypotheses, won't-fix bugs — each with a
  kind word and a link. Lives in the `md` Inside page.

## House style

- **Masthead:** *The Daily Bundle*, set large in a Times/Georgia serif, with a
  Latin motto (*"Omnia quae acta sunt, in fasciculo"* — "all that was done, in
  one bundle"), volume/number, dateline, and edition strap.
- **Sections:** Top of the Fold (lead), the three-column body, **The Fleet
  Ledger** (the day's open/closed/bug counts as standing statistics), **In
  Brief & Around the Fleet**, **Fleet Weather** (a tongue-in-cheek outlook), and
  a colophon foot.
- **Bylines** name the responsible desk (Release Desk, gVisor Bureau, CI-Green
  Watchdog, Monorepo Desk…) — i.e. the gardener/crew lineage that did the work —
  and cite the originating bead ID.
- **Tags:** colored pills — `Breaking` / `Resolved` / `Pattern` / `Watch` —
  classify each story.
- Every `kd-` reference in the prose is a real bead; in the viewer's md docs they
  linkify automatically, so the paper is a navigable index into the day.

## How an issue is produced

1. **Pick the day's window** (00:00–24:00 UTC of "yesterday").
2. **Mine kbeads:** beads created/closed in the window, grouped by type,
   priority, and creator; bugs and epics surfaced; recurring themes clustered.
3. **Assign the front page:** the highest-severity / highest-impact thread
   becomes the lead; 5–6 stories fill the columns; the rest become briefs.
4. **Compute the ledger:** opened / closed / bugs / epics.
5. **Compose** the `html` front page, the `md` editorial, and the `csv` wire.
6. **Assemble the bundle:** `gb bundle create` the issue, `gb bundle add` each
   section with a `position`, and hand `/bundle/<id>` to Slack.

This is a natural fit for a **scheduled gardener** ("The Editor-in-Chief") that
wakes once a day, runs the mining query, and files the issue automatically —
turning the fleet's exhaust into a daily front page.

## Future columns

- **Obituaries** — beads closed as `won't-fix` / superseded, with a kind word.
- **Markets** — CI-minute spend, token costs, the release version "ticker."
- **Letters to the Editor** — operator decisions and their outcomes.
- **The Crossword** — a dependency graph puzzle. (Aspirational.)
