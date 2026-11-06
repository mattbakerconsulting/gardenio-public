# beads-viewer: rich hover popover preview for bead links

**Status:** Plan / design — not yet implemented
**Tracking:** ka-0000005676 (this plan), epic TBD on creation
**Scope:** beads-viewer = Go htmx server-side viewer at
`gardenio/controller/cmd/beads-viewer/`. **NOT** beads3d.
**Author:** thread-1777240755-165099 (planning agent), 2026-04-26
**Origin:** Matt's Slack request 2026-04-26 — "when the mouse is over a link to
a bead in beads viewer, the type, title, and first part of description for
that bead should show in a rich popover preview of that bead. Get it all
planned and put some stuff up that the beads viewer gardener can run with;
don't forget about tests"

---

## TL;DR

Today, every bead-ID reference in rendered descriptions, comments, design,
notes, AC, and dep lists is wrapped in a plain `<a href="/<id>">ka-XXXX</a>`
by `linkify.go::LinkifyHTML` (and via the goldmark inline pipeline for
markdown links). On hover, the browser shows nothing useful — just the
default URL tooltip. Users have to click through to learn anything about the
referenced bead.

We will add a **hover-triggered rich popover** that fetches a small preview
fragment from a new `GET /b/{id}/preview` endpoint and renders **type, title,
status badge, priority badge, and the first ~280 chars of the description**.
The preview piggy-backs on the existing linkify path so every existing
in-page bead link picks up the behavior automatically — no per-call-site
opt-in needed.

We will lean on **htmx + a tiny vanilla JS hover controller** (matches the
codebase's existing pattern in `comment_links.js`, `copy_buttons.js`,
`drafts.js`). No new JS framework, no popover library.

---

## Goals

1. Hovering any bead-ID link inside the beads-viewer UI shows a popover
   within ~250 ms with: type chip, status chip, priority chip, title,
   description excerpt.
2. The popover follows accessibility / keyboard expectations: focus → show,
   blur → hide, Escape → hide.
3. Each preview is fetched at most once per page load per bead ID and is
   cached client-side. Revisiting a link does not re-fetch.
4. The fetch is on hover-intent (~150 ms hover delay), so casual mouse
   transit across a list of links doesn't flood the daemon.
5. Works for every bead-link in the viewer: descriptions, comments,
   long-form fields, dep lists, similar-list, search results, active feed.
6. Robust under churn — htmx swaps and SSE comment refreshes don't leave
   stale popovers attached.
7. Standard Playwright + Go unit-test coverage; no flake.

## Non-goals

- Inline editing from the popover (priority/assignee popovers are separate
  features — ka-0000007368, ka-0000002946).
- Recursive preview (popover-on-popover).
- Touch-device long-press fallback. **Decision:** out of scope for v1.
  Touch users tap through to the bead — same as today. If demand emerges,
  follow-up bead.
- Preview for cross-project Jira IDs or external URLs. Bead-ID-only.
- Replacing the existing `data-comments-latest` polling hover/click flows.
- beads3d (3D viewer) parity. beads3d already opens an inline detail panel
  on click; if we want hover preview there it would be a separate, much
  bigger UI change against `detail-panel.js`. Out of scope for this epic.

## Acceptance criteria (verifiable)

A1. Hovering `<a href="/ka-abc123">ka-abc123</a>` for ≥150 ms triggers a
    `GET /b/ka-abc123/preview` request and renders a popover near the link.
A2. The popover contains: type, title, status, priority, and a description
    excerpt of at most 280 chars (with ellipsis if truncated).
A3. Hovering the same link a second time within the same page session does
    NOT re-issue the request.
A4. Moving the cursor onto the popover keeps it open. Moving off both link
    and popover closes it within ~150 ms.
A5. Focusing a bead link with the keyboard shows the popover; Escape or
    blur closes it.
A6. If the preview request 404s (bead not found, e.g. `ka-tombstone`), the
    popover renders a single-line "Not found" state — does not break the
    page.
A7. Popover never overlaps off-screen; it flips above/below the link as
    needed.
A8. New endpoint is rate-limit-friendly: a single page with N links, after
    hovering each once, makes exactly N preview calls — independent of how
    many times the user wiggles the mouse.
A9. New Go unit tests pass; new Playwright spec passes; existing tests
    unaffected.

---

## Architecture

```
┌──────────── browser ────────────┐         ┌──────── beads-viewer (Go) ───────┐
│  preview-popover.js             │         │                                  │
│   - delegated mouseover/focus   │  GET    │  GET /b/{id}/preview             │
│   - 150 ms hover-intent         │ ──────► │   handleBeadPreview              │
│   - cache by bead id            │         │     daemon.GetBead(id)           │
│   - fetch /b/{id}/preview       │ ◄────── │     render bead_preview.html     │
│   - render returned HTML into   │  HTML   │       (type/title/status/        │
│     a positioned <div role=     │ fragment│        priority/desc excerpt)    │
│     "tooltip">                  │         │                                  │
│   - dismiss on out / Escape     │         └──────────────────────────────────┘
└─────────────────────────────────┘
```

The popover is purely a hover preview — no nested htmx, no live updates.
We render a static HTML fragment server-side and inject it into a
single floating `<aside id="bead-preview-popover">` element that is created
once and reused across hovers.

### Why server-rendered HTML, not JSON?

- Matches the existing beads-viewer pattern: every fragment endpoint
  (`/b/{id}/comments`, `/b/{id}/deps`, `/b/{id}/rating`) returns rendered
  HTML, not JSON.
- Uses the same template machinery, the same status/priority badge styles,
  the same linkify rules — no client-side template duplication.
- The preview is plain text content with a couple of badges; the savings of
  JSON + client-side templating do not pay for the new code.

### Why a singleton popover, not htmx targets per link?

htmx's `hx-trigger="mouseenter"` + `hx-get` works for one-shot replacements
but does not handle: hover-intent debounce, dismissal, repositioning,
keyboard a11y, or shared cache across N links. We get all of that essentially
free by writing ~120 lines of vanilla JS, modeled on `comment_links.js`.

---

## Server side (Go)

### New route

`GET /b/{id}/preview` → `s.handleBeadPreview` (mounted in `server.go`
between the existing `/b/{id}/deps` and bead-action routes).

### Handler outline (`bead_preview.go`)

```go
func (s *Server) handleBeadPreview(w http.ResponseWriter, r *http.Request) {
    id := r.PathValue("id")
    if id == "" {
        http.Error(w, "missing id", http.StatusBadRequest)
        return
    }
    bead, err := s.daemon.GetBead(r.Context(), id)
    if err != nil {
        var apiErr *beadsapi.APIError
        if errors.As(err, &apiErr) && apiErr.StatusCode == http.StatusNotFound {
            s.renderPartial(w, "templates/partials/bead_preview.html",
                "bead_preview_notfound", map[string]any{"ID": id})
            return
        }
        s.logger.Error("preview: get bead", "id", id, "error", err)
        http.Error(w, "preview unavailable", http.StatusInternalServerError)
        return
    }
    s.renderPartial(w, "templates/partials/bead_preview.html", "bead_preview",
        map[string]any{
            "Bead":      bead,
            "Excerpt":   excerpt(bead.Description, 280),
            "BasePath":  s.basePath,
        })
}
```

### Excerpt helper

```go
// excerpt returns the first n runes of s, collapsing internal whitespace,
// and appending an ellipsis when truncated. Markdown is reduced to
// plaintext via a stripping pass before counting (so users don't see raw
// asterisks in the preview).
func excerpt(s string, n int) string { ... }
```

Implementation detail: re-use the existing `mdInline` goldmark instance to
render to HTML, then strip tags, then truncate. Tested in
`bead_preview_test.go`.

### Caching headers

- Set `Cache-Control: private, max-age=30` on 200 responses. The browser
  already short-circuits during the same page load via the JS cache; this
  lets back-button revisits and refresh-during-debugging stay snappy.
- 404s use `Cache-Control: no-store`.

### Existing handler reuse — none

The handler is small; we **do not** thread preview rendering through
`renderFullDetail` because preview fields differ (no comments, no deps,
truncated description, no metadata). One template, one handler.

### Where the route fits

`server.go::registerRoutes` (around line 257):

```go
mux.HandleFunc("GET /b/{id}/deps", s.handleBeadDeps)
mux.HandleFunc("GET /b/{id}/preview", s.handleBeadPreview)   // ← new
mux.HandleFunc("POST /b/{id}/decision/resolve", ...)
```

### Auth

Same auth chain as every other `/b/{id}/*` route — wrapped by the existing
OIDC middleware. No special handling required.

---

## Templates

New file: `templates/partials/bead_preview.html`. Defines two named
templates: `bead_preview` and `bead_preview_notfound`.

```html
{{define "bead_preview"}}
<article class="bead-preview" data-bead-id="{{.Bead.ID}}">
  <header class="bead-preview-meta">
    <span class="tag tag-type">{{.Bead.Type}}</span>
    {{if .Bead.Status}}<span class="tag tag-{{.Bead.Status}}">{{.Bead.Status}}</span>{{end}}
    {{if .Bead.PriorityLabel}}<span class="tag">{{.Bead.PriorityLabel}}</span>{{end}}
    <code class="bead-preview-id">{{.Bead.ID}}</code>
  </header>
  <h3 class="bead-preview-title">{{.Bead.Title}}</h3>
  {{if .Excerpt}}<p class="bead-preview-excerpt">{{.Excerpt}}</p>{{end}}
</article>
{{end}}

{{define "bead_preview_notfound"}}
<article class="bead-preview bead-preview-missing" data-bead-id="{{.ID}}">
  <p>Bead <code>{{.ID}}</code> not found.</p>
</article>
{{end}}
```

CSS lives in `templates/static/style.css` (or wherever the existing badge
styles are; the gardener should check and match). The popover container
`#bead-preview-popover` gets fixed positioning, max-width 380 px, drop
shadow, border, matching the existing detail-panel style language.

---

## Client side (JS)

New file: `templates/static/preview_popover.js`, loaded once via
`layout.html` after the other static scripts.

### Behavior

```text
on document:
  mouseover[a[href^="/<basePath>/"]]   → schedule show after 150 ms
  mouseout[a[href^=...]]               → cancel show; if showing, schedule hide after 150 ms
  focusin[a[href^=...]]                → show immediately
  focusout[a[href^=...]]               → hide immediately
  keydown[Escape]                      → hide
  click on a link                      → hide (navigation will rebuild)
on popover element:
  mouseenter                           → cancel hide
  mouseleave                           → schedule hide
```

### Pseudo-implementation

```js
(() => {
  const BASE = window.BEADS_VIEWER_BASE_PATH || '';
  const HOVER_MS = 150;
  const HIDE_MS = 150;
  const cache = new Map();   // id → Promise<string|null>  (HTML fragment, or null on 404)
  const inflight = new Map(); // id → AbortController
  let showT = null, hideT = null;
  let popover = null, currentLink = null;

  function ensurePopover() {
    if (popover) return popover;
    popover = document.createElement('aside');
    popover.id = 'bead-preview-popover';
    popover.setAttribute('role', 'tooltip');
    popover.hidden = true;
    popover.addEventListener('mouseenter', cancelHide);
    popover.addEventListener('mouseleave', scheduleHide);
    document.body.appendChild(popover);
    return popover;
  }

  function isBeadLink(a) {
    if (!a || !a.href) return false;
    // Accept /ka-XXX or /<base>/ka-XXX, no trailing path/query.
    const href = new URL(a.href, location.origin);
    if (href.origin !== location.origin) return false;
    const path = href.pathname.replace(BASE, '').replace(/^\/+/, '');
    return /^kd-[A-Za-z0-9]+$/.test(path);
  }

  function beadIdOf(a) {
    const path = new URL(a.href, location.origin).pathname.replace(BASE, '').replace(/^\/+/, '');
    return path;
  }

  async function fetchPreview(id) {
    if (cache.has(id)) return cache.get(id);
    const ac = new AbortController();
    inflight.set(id, ac);
    const p = fetch(`${BASE}/b/${id}/preview`, { signal: ac.signal })
      .then(async (r) => {
        if (r.status === 404) return await r.text();   // notfound HTML fragment
        if (!r.ok) throw new Error(`preview ${r.status}`);
        return await r.text();
      })
      .catch((e) => {
        if (e.name === 'AbortError') return null;
        return null;
      })
      .finally(() => inflight.delete(id));
    cache.set(id, p);
    return p;
  }

  async function show(link) {
    currentLink = link;
    const id = beadIdOf(link);
    const html = await fetchPreview(id);
    if (currentLink !== link || !html) return;       // user moved on
    const el = ensurePopover();
    el.innerHTML = html;
    position(el, link);
    el.hidden = false;
  }

  function position(el, anchor) { /* viewport-aware flip; standard */ }
  function scheduleShow(link) { clearTimeout(showT); cancelHide(); showT = setTimeout(() => show(link), HOVER_MS); }
  function cancelShow() { clearTimeout(showT); showT = null; }
  function scheduleHide() { clearTimeout(hideT); hideT = setTimeout(hide, HIDE_MS); }
  function cancelHide() { clearTimeout(hideT); hideT = null; }
  function hide() {
    if (popover) popover.hidden = true;
    currentLink = null;
  }

  document.addEventListener('mouseover', (e) => {
    const a = e.target.closest('a');
    if (isBeadLink(a)) scheduleShow(a);
  });
  document.addEventListener('mouseout', (e) => {
    const a = e.target.closest('a');
    if (!isBeadLink(a)) return;
    cancelShow();
    if (popover && !popover.hidden) scheduleHide();
  });
  document.addEventListener('focusin', (e) => {
    const a = e.target.closest('a');
    if (isBeadLink(a)) show(a);
  });
  document.addEventListener('focusout', (e) => {
    const a = e.target.closest('a');
    if (isBeadLink(a)) hide();
  });
  document.addEventListener('keydown', (e) => { if (e.key === 'Escape') hide(); });
  document.addEventListener('click', (e) => { if (e.target.closest('a')) hide(); });
})();
```

### Why event delegation on `document`?

Existing scripts (`comment_links.js`, `copy_buttons.js`) already do this
because htmx swaps replace fragments at runtime — direct binding to specific
links would bind to elements that vanish. Delegation handles freshly
swapped-in markup automatically with zero re-binding.

### Excluding non-bead anchors

The viewer renders many non-bead anchors: `/active`, `/agents/...`,
`/p/<project>`, `/views/...`, `/advice`, etc. The `isBeadLink` check enforces
"the path under base is exactly `ka-XXX`" so the popover only fires for
actual bead links.

A more aggressive alternative is to add `data-bead-link="true"` in
`linkify.go` and gate on the data attribute. **Decision:** start with the
URL-shape check — zero server changes, simpler regression surface. If the
shape check produces false positives in practice, switch to the data
attribute in a follow-up.

---

## Testing

### Go unit tests (`bead_preview_test.go`)

- `TestExcerpt_TruncatesAtRuneBoundary` — multi-byte runes, 280-rune cap.
- `TestExcerpt_StripsMarkdown` — `**bold** _it_` becomes `bold it`.
- `TestExcerpt_AppendsEllipsisOnTruncate` / `TestExcerpt_NoEllipsisWhenShort`.
- `TestHandleBeadPreview_Renders` — fake daemon returns a bead; assert the
  rendered HTML contains type, title, excerpt, status tag, id.
- `TestHandleBeadPreview_NotFound` — daemon returns 404 → `bead_preview_notfound`
  template, status 200 (we render an HTML fragment, not a 404 page,
  because htmx-style fragment endpoints conventionally return 200 with the
  state markup; consistent with `handleBeadComments`).
- `TestHandleBeadPreview_DaemonError` — daemon returns 500 → handler returns
  500.
- `TestHandleBeadPreview_RouteMounted` — exercise the `mux` registered in
  `server.go` to confirm `/b/{id}/preview` is reachable.
- `TestHandleBeadPreview_AuthRequired` — same shape as existing route auth
  tests; preview should follow the same OIDC chain.

### Playwright e2e (`tests/preview_popover_test.go` style — match existing
   `htmx_test.go` pattern, OR a new spec under wherever the gardener
   already runs Playwright for this server. Gardener to confirm.)

- `popover_appears_on_hover` — hover a bead link, assert popover visible
  with type+title+excerpt within 500 ms.
- `popover_dismisses_on_mouseout` — move mouse off both link and popover,
  assert hidden within 500 ms.
- `popover_persists_when_moved_onto_popover` — hover link → move onto
  popover → popover stays open.
- `popover_dismisses_on_escape` — show, press Escape, assert hidden.
- `popover_keyboard_focus_shows_preview` — tab to link, assert popover.
- `popover_404_renders_notfound_state` — link to a non-existent id,
  assert "Not found" text.
- `popover_caches_within_page` — hover link twice, assert exactly one
  network request to `/b/<id>/preview` (use `page.on('request')`).
- `popover_position_clamps_to_viewport` — link near right edge → popover
  flips left.

### Existing tests we must not break

- `linkify_test.go` — unchanged.
- `bead_show_test.go` — page rendering unchanged; only adds the new script
  tag in layout. Add a single assertion that
  `templates/static/preview_popover.js` is included.
- `htmx_test.go` — unaffected.

---

## Out-of-scope but worth noting

- **Cross-page navigation cache.** The JS cache is per page load. Service
  Worker caching of `/b/{id}/preview` would survive navigation but is not
  worth the complexity for v1.
- **Hover-on-tag-chips.** A nice future polish: hover the type/status/
  priority chip inside a popover and get a "what does this mean" definition.
  Not part of this epic.
- **Touch / mobile.** Out of scope (see Non-goals). If we add it, the
  pattern is: long-press → popover with explicit close button.
- **Preview for `bd-XXX` legacy IDs.** `linkify.go` only matches `kd-`. If
  some legacy IDs still appear, the popover won't trigger — this matches
  current link behavior.

---

## Implementation breakdown — child tasks

The gardener (ka-0000000596 owner / beads-viewer gardener) should pull these
in order. Each is a single feature branch + MR, mirroring the close/reopen/
priority-edit cadence the team has been using.

1. **Server endpoint + template + Go tests.**
   - `bead_preview.go` (handler + excerpt helper)
   - `bead_preview_test.go`
   - `templates/partials/bead_preview.html`
   - Wire route in `server.go`.
   - Acceptance: route returns rendered HTML for a real id, `notfound`
     fragment for an unknown id, 500 for an upstream error. Go tests pass.

2. **Client JS hover controller + CSS + layout wiring.**
   - `templates/static/preview_popover.js`
   - CSS for `#bead-preview-popover` and `.bead-preview-*` classes.
   - Add `<script src="{{.BasePath}}/assets/static/preview_popover.js" defer></script>`
     to `layout.html` after the other static scripts.
   - Acceptance: load any bead-show page, hover a link in the description,
     popover appears. Manual smoke + a single Playwright "appears on hover"
     test.

3. **Playwright e2e suite for the popover.**
   - All eight scenarios listed under Testing.
   - Use the existing test harness (whichever runs `htmx_test.go`-style
     end-to-end tests; gardener to confirm — there's no existing Playwright
     setup for the Go viewer in `gardenio/controller/`, so the gardener may
     need to either (a) add a small Playwright harness pointed at the Go
     binary, or (b) replicate the spirit of these tests as Go-driven
     `httptest` + headless DOM assertions. Recommend (a) — the JS behavior
     is genuinely UI behavior and httptest can't exercise hover/focus.)
   - Acceptance: all scenarios pass in CI.

4. **Polish / regressions.**
   - Verify the popover does not break copy buttons, comment polling,
     priority edit popover, or keyboard navigation.
   - Tune timings (HOVER_MS, HIDE_MS) if QA flags churn.
   - Acceptance: existing test suite green; visual diff parity on the
     bead-show page.

Sequencing: 1 must land before 2 (JS calls the endpoint). 3 depends on 2.
4 wraps up.

---

## Risks / open questions

- **Where do Playwright tests for the Go beads-viewer live today?** I could
  not find a `playwright.config.js` under `gardenio/controller/` — the only
  Playwright setup is in `beads3d/`. Gardener should confirm whether to (a)
  introduce a new Playwright harness for `cmd/beads-viewer`, or (b)
  approximate hover behavior in Go integration tests using a headless
  browser library (`chromedp`?). My recommendation: introduce a small
  Playwright harness — UI hover behavior really should be tested as UI.
- **Style language.** The existing `.tag-<status>` and `.tag` classes need
  to be reused so the preview's chips look identical to the rest of the
  page. Gardener should grep for `.tag-` styles in the static CSS and
  confirm they are usable from a popover context (no specificity issues).
- **Title escaping.** `html/template` will escape `Bead.Title` and
  `.Excerpt` automatically — confirm by adding a test with `<script>` in a
  title.
- **Daemon load.** Worst case: a list page with 200 bead links, user hovers
  every one → 200 preview calls. Mitigation: client cache (already), and
  the preview handler is a single `GetBead` call which is already cached by
  the daemon's own LRU. If this becomes a hot path, add a `GetBeadsBatch`
  prefetch endpoint as a follow-up — not v1.

---

## Gardener handoff checklist

- [ ] Read this doc.
- [ ] Confirm Playwright harness decision for `cmd/beads-viewer`.
- [ ] Pull task 1 (`server endpoint + tests`) and ship.
- [ ] Pull task 2 (`client JS + CSS + layout`) and ship.
- [ ] Pull task 3 (`Playwright e2e`) and ship.
- [ ] Pull task 4 (`polish + regression sweep`) and ship.
- [ ] Close epic.
