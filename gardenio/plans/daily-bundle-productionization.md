# The Daily Bundle — Productionization & Viewer Remediation

*Scoping note. The newspaper has outgrown a one-off demo; this captures the
viewer gaps to remediate and the open question of where the project should live.*

The inaugural issue (bundle `ka-0000007321`, design in
`daily-bundle-newspaper.md`) proved the format. Operator review surfaced two
viewer-side defects and a structural observation: "the newspaper production
process increasingly looks like its own project, perhaps with its own Acme
GitLab repo and project bead." This doc roots-causes the defects and frames the
project-home decision.

## Root cause: the two viewer defects

Both defects trace to one design choice in `bundle_renderers.go`: the `html`
doc format renders author content into a **bare-`sandbox` `<iframe srcdoc>`**.
That sandbox is correct for *untrusted* author HTML (XSS-safe by construction),
but it isolates the newspaper pages from the viewer's chrome in two ways that
hurt a *trusted, first-party* newspaper.

### Defect A — bundle links get no hover preview

- **How previews work today:** `templates/static/preview_popover.js` attaches a
  single delegated `mouseover` listener on the **parent** document. It fires for
  anchors whose href resolves to `/{ka-id}` (see `linkKindOf`), then fetches the
  `/b/{id}/preview` fragment (`handleBeadPreview` in `bead_preview.go`) and shows
  a popover. `LinkifyHTML` (`linkify.go:284`) emits the anchors it recognises as
  `class="mention" data-bead-id="…"`.
- **Why bundle html links miss out:** (1) the anchors live *inside* the
  sandboxed iframe — a separate document the parent's delegated listener never
  sees, and with a bare sandbox the iframe is an *opaque origin* the parent JS
  cannot reach at all; (2) the newspaper authors plain `<a href="/ka-X">` without
  the `mention`/`data-bead-id` markup the classifier keys on.
- **Note:** bundle **md** docs render *inline* through `LinkifyHTML`
  (`bundle_renderers.go:88`), so their bead links *already* get previews. Only
  the `html` (and any future iframe-isolated) format is affected.

### Defect B — each html page shows its own scrollbar

- **How it renders today:** `renderBundleHTML` emits
  `<iframe class="bundle-html" sandbox srcdoc="…" style="…min-height:720px;">`
  (`bundle_renderers.go`, `bundleHTMLDefaultMinHeight = 720`). The height is
  fixed, so any page taller than 720px gets an *inner* scrollbar — a scrollbar
  inside a scrollbar, which reads as broken on a reading surface.
- **Why auto-resize is impossible today:** the standard fixes both fail under a
  *bare* `sandbox`. The iframe cannot `postMessage` its height (needs
  `allow-scripts`, which would reintroduce script-execution XSS), and the parent
  cannot measure `contentDocument.body.scrollHeight` (a bare sandbox forces an
  opaque origin, blocking same-origin DOM access).

## Remediation options (viewer)

| Defect | Option | Sketch | Trade-off |
|---|---|---|---|
| B (resize) | **B1 — `allow-same-origin`** | Change `sandbox` → `sandbox="allow-same-origin"` (scripts STILL disabled). A small parent script sets `iframe.style.height = contentDoc.body.scrollHeight` via a `ResizeObserver`. | Keeps script-XSS off (no `allow-scripts`); grants same-origin DOM read so the parent can measure + resize. **Recommended.** Must keep `allow-scripts` OUT — `allow-same-origin`+`allow-scripts` together defeats the sandbox. |
| B (resize) | B2 — trusted inline render | Add a `newspaper`/`trusted-html` format that sanitizes and renders inline (no iframe), so it flows like md. | Bigger security-model change; needs a vetted sanitizer. Heavier. |
| A (preview) | **A1 — same-origin + inject** | With B1's `allow-same-origin`, the parent popover script also binds to each bundle iframe's document; the renderer rewrites `/ka-X` anchors to carry `class="mention" data-bead-id`. | Reuses the whole existing preview path. Pairs naturally with B1. **Recommended.** |
| A (preview) | A2 — author-side cards | Newspaper emits its own CSS-only `:hover` cards with excerpts baked in at build time. | No viewer change, but excerpts go stale and duplicate the daemon. |

**Recommended pairing: B1 + A1.** One sandbox-attribute change unlocks both —
parent-driven auto-resize *and* parent-driven previews — while preserving the
no-`allow-scripts` XSS guarantee. Net change is localized to
`bundle_renderers.go` (the iframe attrs + anchor rewrite) and a small parent JS
resize/preview-bind helper on the bundle page.

## The project-home question (operator decision)

Three plausible homes, in rising order of ceremony:

1. **Epic inside the gardenio monorepo.** The newspaper *renders* via beads-viewer
   (gardenio) and *reads* from kbeads — both already here. An epic under the
   existing `gardenio` project bead keeps the viewer remediation next to the code
   it touches; the issue-generation tooling lives under `gardenio/` (or a small
   `cmd/dailybundle`).
2. **New project bead, code still in monorepo.** A first-class `daily-bundle`
   project bead for tracking/roster/scheduling, but the code stays in the
   monorepo. Gives the newspaper its own backlog and a scheduled "Editor"
   gardener without a repo split.
3. **New Acme GitLab repo + project bead.** Full separation: its own repo,
   CI, project bead. Justified only if the generator grows beyond a thin
   kbeads-reader (templating engine, asset pipeline, multi-edition publishing).

**Recommendation:** start at **(2)** — a `daily-bundle` project bead for the
backlog and the scheduled Editor gardener, with code in the monorepo under
`gardenio/`. The viewer remediation (B1+A1) belongs to gardenio regardless. Defer
the repo split (3) until the generator's surface area justifies its own CI — the
viewer changes don't, and a premature split would scatter the beads-viewer work.

### Decision (ka-0000001108): option 3 — independent repo

Operator chose option **3** — a separate GitLab repo + project bead — on the
principle that *"journalism requires independence."* Status:

- **Project bead:** `ka-0000001121` (`type=project`, `daily-bundle`).
- **GitLab repo:** scaffolded and pushed to `example-org/daily-bundle`
  (Go skeleton: `cmd/dailybundle` + `internal/mine` + `internal/compose`,
  builds + tests green; `README.md` + `STYLEBOOK.md` capture the house style).
- **The Editor-in-Chief** (`ka-0000001468`) re-homes to this repo as a scheduled
  CI job, not a gardener pod.
- **Viewer remediation** (`ka-0000004212`, `ka-0000006557`) stays in **gardenio** —
  those are beads-viewer fixes, cross-linked only.

**Access caveat (decision ka-0000002191):** operator wants the repo private to
"just me and gardenio." A project under the `example-org` *group* inherits
all ~61 group members; GitLab has no project-level exclusion. True isolation
requires the repo in the operator's **personal namespace**
(`matthewbaker/daily-bundle`), which a group bot cannot transfer into (403 —
needs the namespace owner's perms). Pending operator action to transfer or
re-create in the personal namespace.

## Productionization (the "Editor-in-Chief")

Once the viewer renders cleanly, a scheduled gardener wakes once daily, runs the
mining query for the prior UTC day (created/closed beads, bugs, epics, release
tags, obituary candidates), composes the four sections, assembles the bundle,
and posts `/bundle/<id>` to Slack. This is the natural home for the issue
generator regardless of which project-home option is chosen.
