# /threads polish grade — Phase 3 task 3-5 (ka-0000001087)

Task 3-5 (ka-0000001087), epic ka-0000002477 / ka-0000000679. Operator: Matt Baker.

This grade records the SHOULD-have polish gates 3-3/3-4 grading uncovered:
empty/loading/error chrome, markdown parity, accessible composer, mobile
tolerance, and optimistic-reconcile hardening + the `data-skin="dark"` contrast
fix. Each gate below was verified against the real `beads-viewer` binary booted
over the hermetic `threads-mock-daemon.mjs` (the same harness 3-3/3-4 use), plus
Go unit/contract tests in `threads_polish_test.go`.

## Verdicts

| Gate | What it checks | Verdict | Evidence |
|---|---|---|---|
| **(a) empty** | `/threads` `.threads-empty` + per-lane `.thread2-empty` in both lanes, full chrome | **PASS** | `/threads/gd-uvwwymdTju` renders nav + breadcrumbs + wish rail + "opened this wish" marker + "No agent work yet" line — not a blank pane. Index empty panel unchanged from 3-3. |
| **(a) loading** | A refresh affordance, no flash-of-unstyled | **PASS** | The `#thread-timeline` poll region flips `aria-busy` true/false via `hx-on:htmx:before-request`/`after-request` and shows an aria-hidden `.thread2-refreshing` "Updating…" pill (CSS `.htmx-request` gated, `prefers-reduced-motion` honoured). Page CSS is inline SSR — no FOUC. |
| **(a) error** | `.banner.banner-error` keeps full page chrome both themes | **PASS** | Forcing `mode=timeline` keeps the head + nav + crumbs + h1 and shows "Failed to load thread timeline."; `mode=list` keeps the index h1 + intro + banner. Verified via `/__control/fail`. |
| **(b) markdown parity** | message bodies render with the same goldmark treatment bead comments use (code/links/lists) | **PASS** | Conversation bubbles render via the new `threadMessageMarkdown` template func = `renderCommentMarkdown(s.mdInline, …)` — byte-identical pipeline to bead comments. Live render shows `<code>make test</code>`, `<a href="…">signup PR</a>`, `<li>…</li>`. Bubble carries `.thread2-msg-body comment-body` for per-skin code/pre styling. `TestThreadDetail_MarkdownParity`. |
| **(c) accessible composer** | labelled textarea, visible focus ring, Enter/Cmd-Enter submit, ARIA on the log | **PASS** | `<label for="thread-composer-text">` is the accessible name (redundant `aria-label` removed); `aria-describedby="thread-composer-hint"` wires the shortcut hint; textarea `:focus-visible` = 2px solid blue (computed-style verified); Send button now has its own focus ring; **Cmd/Ctrl+Enter** submits (keydown → `requestSubmit`), documented in the hint; `#thread2-convo` is `role="log" aria-live="polite" aria-relevant="additions text"`. `TestThreadComposer_AccessibleMarkup`. |
| **(d) mobile** | single-column `thread2-grid` ≤480px, wrapped chips, full-width cards, no h-scroll; 768 clean | **PASS** | At 480: `scrollWidth == clientWidth` (no h-scroll); the composer row computed `flex-direction: column` (textarea + full-width Send stacked); chips wrap; `.thread2-msg-body pre { overflow-x:auto; max-width:100% }` keeps code blocks in-bounds. 768 keeps the single-column grid with desktop spacing. |
| **(e) optimistic de-dup** | optimistic bubble is **replaced, not duplicated** by its server echo under `?since=`, incl. slow-network | **PASS** | Content-derived de-dup key (`threadMsgKey` = FNV-1a/32 of trimmed `author\0text`, byte-identical to `msgKey()` in `thread_composer.js`) stamped as `data-msg-key`. The JS snapshots optimistic bubbles in `htmx:beforeSwap` and, in `afterSwap`, drops the optimistic twin iff a non-optimistic bubble with the same key arrived, else re-appends it. **Live: afterOptimistic=1 bubble; afterReconcile=0 optimistic + exactly 1 body (zero dup). Slow-network: poll-before-echo PRESERVES the bubble (no flicker-out), echo then reconciles to exactly 1.** `TestThreadMsgKey_GoldenVectors`, `TestConversation_CarriesDedupKey`, `TestThreadDetail_RendersDedupKeyAttr`. |
| **(e) dark contrast** | `data-skin="dark"` muted greys legible | **PASS** | The filed defect was that `dark` had no token block → light fallback. Fixed by aliasing every thread + layout `[data-skin="dark-terminal"]` selector to `[data-skin^="dark"]`, so `dark` inherits the full dark-terminal `--bg`/`--text`/muted-grey tokens + thread overrides. Live `data-skin="dark"` render is fully legible (dark bg, `#9aa7b4`-class greys). `TestThreadTemplates_DarkSkinAliased`. |

## The de-dup key (cross-language contract)

`threadMsgKey(author, text)` (Go, `threads_curation.go`) and `msgKey(author,
text)` (JS, `thread_composer.js`) MUST agree byte-for-byte: both trim author and
text, join with a single `0x00` byte, run FNV-1a/32 over the UTF-8 bytes, render
base-16. Golden vectors pinned in `TestThreadMsgKey_GoldenVectors` (also produced
by running the JS function over the same inputs):

| author | text | key |
|---|---|---|
| `matt@example.com` | `hello world` | `b7a4fbbe` |
| `` | `x` | `59770925` |
| `a` | `  spaced  ` | `aa5dfe9a` |
| `u` | a markdown line w/ code-span + link (see test) | `ba50fec8` |

(The fourth vector's exact text is in `TestThreadMsgKey_GoldenVectors` —
`threads_polish_test.go` — kept verbatim there so the key stays pinned.)

If either side drifts, the optimistic bubble stops matching its server echo and
the de-dup silently breaks — hence the pinned vectors guard both halves.

## How to re-run

```bash
# 1. build the viewer
cd gardenio/controller && go build -o /tmp/beads-viewer ./cmd/beads-viewer/

# 2. boot mock daemon + viewer (dev-auth bypass) and drive with Playwright
cd gardenio/tests/beads-viewer-screenshots
MOCK_DAEMON_PORT=18080 node tools/threads-mock-daemon.mjs &
BEADS_HTTP_ADDR=http://localhost:18080 BEADS_VIEWER_DEV_AUTH_BYPASS=1 \
  BEADS_VIEWER_DEV_AUTH_BYPASS_ACK_AGENT_POD=1 BEADS_VIEWER_PORT=8095 /tmp/beads-viewer &
# composer cells need the X-Auth-Request-Email header (CanCompose gate).

# 3. Go contract tests
cd gardenio/controller && go test ./cmd/beads-viewer/ -run 'Thread|Curation|Composer|MsgKey|Markdown|DarkSkin|Loading' -count=1
```

## Inherited gates (unchanged from 3-3/3-4)

V1–V3, V7–V9, V11–V12 (THREADS-GRADE.md) and the composer-reachable / optimistic
cells (THREADS-3-4-GRADE.md) remain green; 3-5 closes the V4 `data-skin="dark"`
FAIL the earlier grades tracked as a latent defect.
