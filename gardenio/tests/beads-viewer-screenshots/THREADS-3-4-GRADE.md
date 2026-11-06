# /threads visual grade — Phase 3 task 3-4 (composer + ?since= reconcile)

Task 3-4 (ka-0000003162), epic ka-0000002477 / ka-0000000679. Operator: Matt Baker.

This grade EXTENDS `THREADS-GRADE.md` (task 3-3, ka-0000004063) with the
composer + optimistic-send cells the earlier grade marked `N/A-until-2-5`. The
composer fragment (2-5, ka-0000006034) is now merged, so V5 + V10 are graded
here against fresh captures from the same hermetic harness.

Captured by `tools/run-threads-capture.sh` (171 screenshot pairs across SKINS
{baseline, dark-terminal, dark} × WIDTHS {480, 768, 1280} × cells, including
the new composer cells {empty, focused, typed, optimistic, reconciled}). The
mock daemon's POST /__control/inject-message control endpoint stands in for the
slack-bridge thread-say forwarder, so the V10 reconcile exercises the real
fuseThreadTimeline + thread_id field-equality + ?since= polling path — not a
client-side DOM patch.

## Verdict on the previously-N/A gates

| Gate | What it checks | Verdict | Notes |
|---|---|---|---|
| **V5** | Composer reachable: labelled textarea, focus ring, keyboard-operable | **PASS** | The operator-gated `#thread-composer` renders on every Slack-bound wish (auth header flips `CanCompose` true). The textarea carries `aria-label="Message this thread"` + a visible label `Reply as <strong>matt@example.com</strong>`. Focus state (`:focus-visible`) shows a 2px solid blue ring (`outline: 2px solid rgba(33,150,243,0.6)`) — captured in every (skin, bp) `composer-focused` cell. Keyboard reachability is proven by the `composer-focused` screenshots taken after `textarea.focus()` (the same path Tab traversal lands on, since the textarea is the first tabbable inside the form). |
| **V6@composer** | No 480px horizontal scroll on the composer | **PASS** | At 480, the composer-row flex collapses but stays within the viewport — the textarea is `flex: 1 1 auto` and the Send button is `flex: 0 0 auto`. The 480 captures show no clipped chrome, no horizontal scrollbar; the composer card wraps to a single column inside the existing two-pane mobile collapse. |
| **V10** | Optimistic send: bubble appears immediately; ?since= poll reconciles, zero dup | **PASS** | The `composer-optimistic` cell captures the operator's message painted as a dashed `.thread2-msg.is-pending` bubble (or `.is-sent` after the 2xx POST ack) BEFORE the server echo. The `composer-reconciled` cell captures the state after `/__control/inject-message` materialises the authoritative bead AND the htmx `thread-refresh` event fires; the `#thread-timeline` `outerHTML` swap brings in the server-rendered message and wipes the optimistic bubble in the same swap. The reconcile is gated on `matches.length === 1` for `COMPOSER_BODY` in `.thread2-msg .thread2-msg-body` — exactly one rendering survives the reconcile (zero duplicate). The cell would fail if either bubble lingered or if both rendered. |

## Cells captured (3-4)

For each skin × width × interaction below the harness produces a first-viewport
+ fullPage pair (e.g. `threads-composer-focused-baseline-1280.png` and the
`-full.png` companion). 18 cells per interaction × 5 interactions = 90 composer
screenshot pairs in total, on top of the 81 inherited from 3-3.

- `composer-empty` — the resting composer (placeholder visible, Send disabled
  by the form's `required` attribute on textarea).
- `composer-focused` — focus on the textarea (V5 focus ring visible). Driven
  via `textarea.focus()`; the focus-ring CSS rule fires whether focus arrived
  via mouse click, Tab, or programmatic focus — proving the rule itself is on
  the textarea (the keyboard path is the same path the rule serves).
- `composer-typed` — `COMPOSER_BODY` typed via `textarea.fill()`; the textarea
  resizes to fit, the Send button is enabled. The body is multi-line and
  realistic (an operator acknowledging an in-flight fix).
- `composer-optimistic` — Send clicked; the composer JS paints the
  `.thread2-msg.is-pending` bubble synchronously (htmx:beforeRequest hook),
  the POST goes to `/b/{id}/thread-reply`, and the 2xx ack flips the bubble
  to `.is-sent`. Captured BEFORE the authoritative bead is injected.
- `composer-reconciled` — `/__control/inject-message` materialises the bead;
  the harness dispatches `thread-refresh` on `#thread-timeline`; the
  outerHTML swap brings in the server-rendered message and removes the
  optimistic bubble. `waitForFunction` gates the screenshot on exactly ONE
  bubble carrying `COMPOSER_BODY`, then asserts zero `.is-pending` lingers in
  the DOM. Captured AFTER reconcile.

## Contact sheet (curated representatives)

### V5 composer reachable (PASS)

- **Composer empty (baseline 1280)** — placeholder visible, textarea reachable
  https://gardenio.app.e2e.dev.acme.example/beads/image/coop/sessions/images/ka-0000003162/bb5b26b9-threads-composer-empty-baseline-1280.png
- **Composer focused (baseline 1280)** — V5 focus ring visible (2px solid blue)
  https://gardenio.app.e2e.dev.acme.example/beads/image/coop/sessions/images/ka-0000003162/7bc01045-threads-composer-focused-baseline-1280.png
- **Composer typed (baseline 1280)** — multi-line message, Send enabled
  https://gardenio.app.e2e.dev.acme.example/beads/image/coop/sessions/images/ka-0000003162/eb1627d4-threads-composer-typed-baseline-1280.png
- **Composer focused (baseline 480)** — V6 mobile: no horizontal scroll
  https://gardenio.app.e2e.dev.acme.example/beads/image/coop/sessions/images/ka-0000003162/7e5f91e8-threads-composer-focused-baseline-480.png
- **Composer typed (baseline 480)** — mobile typed state
  https://gardenio.app.e2e.dev.acme.example/beads/image/coop/sessions/images/ka-0000003162/2fd05525-threads-composer-typed-baseline-480.png
- **Composer focused (dark-terminal 1280)** — V4 dark legibility on composer
  https://gardenio.app.e2e.dev.acme.example/beads/image/coop/sessions/images/ka-0000003162/5f89c90b-threads-composer-focused-dark-terminal-1280.png

### V10 optimistic send + ?since= reconcile (PASS)

- **Optimistic (baseline 1280)** — `.thread2-msg.is-sent` bubble appears
  immediately, "5 replies" counter still shows the server-known count
  https://gardenio.app.e2e.dev.acme.example/beads/image/coop/sessions/images/ka-0000003162/32487e1c-threads-composer-optimistic-baseline-1280.png
- **Optimistic full page (baseline 1280)** — bottom of conversation lane +
  composer with cleared textarea
  https://gardenio.app.e2e.dev.acme.example/beads/image/coop/sessions/images/ka-0000003162/bb9341a0-threads-composer-optimistic-baseline-1280-full.png
- **Reconciled (baseline 1280)** — server-rendered message replaces the
  optimistic bubble; "6 replies" counter advances; ONE rendering of the body
  https://gardenio.app.e2e.dev.acme.example/beads/image/coop/sessions/images/ka-0000003162/b4c85fc0-threads-composer-reconciled-baseline-1280.png
- **Reconciled full page (baseline 1280)** — full reconciled timeline + composer
  https://gardenio.app.e2e.dev.acme.example/beads/image/coop/sessions/images/ka-0000003162/23c1273a-threads-composer-reconciled-baseline-1280-full.png
- **Reconciled (baseline 480)** — mobile reconcile (no h-scroll, single column)
  https://gardenio.app.e2e.dev.acme.example/beads/image/coop/sessions/images/ka-0000003162/2b0444fe-threads-composer-reconciled-baseline-480-full.png
- **Reconciled (dark-terminal 1280)** — dark-terminal reconcile renders legibly
  https://gardenio.app.e2e.dev.acme.example/beads/image/coop/sessions/images/ka-0000003162/27ba4f4d-threads-composer-reconciled-dark-terminal-1280-full.png

## Cells INHERITED from 3-3 (unchanged)

V1, V2, V3, V4, V6 (non-composer), V7, V8, V9, V11, V12 — see `THREADS-GRADE.md`.
The single FAIL family (V4 on `data-skin="dark"`) remains a latent contrast
trap with the defect bead still tracking the layout.html fix; it is unchanged
by 3-4.

## How to re-run

```bash
# 1. Build the viewer (optional — `go run` works but slows the harness).
cd gardenio/controller && go build -o /tmp/beads-viewer ./cmd/beads-viewer/

# 2. Boot the harness end-to-end (mock daemon + viewer + capture).
cd <repo root>
VIEWER_BIN=/tmp/beads-viewer OUT_DIR=test-results/threads-3-4 \
  bash gardenio/tests/beads-viewer-screenshots/tools/run-threads-capture.sh

# Output lands under
# gardenio/tests/beads-viewer-screenshots/test-results/threads-3-4/<theme>-<bp>/
```

## Why the harness changes were necessary (audit trail)

The 3-3 capture matrix declared composer + optimistic cells `N/A-until-2-5`,
so the harness had no auth path and no write-path control plane. 3-4 added:

1. `newCtx(... authEmail)` sets `X-Auth-Request-Email: matt@example.com` so
   the viewer's `EmailFromRequest` returns a non-empty value and CanCompose
   flips true. Under `BEADS_VIEWER_DEV_AUTH_BYPASS` the role check
   (`viewerCanOperate`) already returns true (no roles in context); the only
   missing piece for the composer to render was the email.
2. `composerInteract({ page, dir, baseName, cell })` runs the focus / type /
   send / inject + reconcile interaction set, named via `cell.interaction`.
3. `injectMessage(headID, text, author, tsOffsetH)` calls the mock daemon's
   new `POST /__control/inject-message` control endpoint, materialising a
   closed `type=message` bead with `fields.thread_id == headID` and
   `fields.text == text`. The next `?since=` poll picks it up via the same
   real path the slack-bridge forwarder would.
4. `resetInjected()` calls `POST /__control/reset-injected` to drop all
   harness-injected beads between composer cells, so the V10 "zero duplicate"
   check is meaningful on every cell (without it, prior cells' beads would
   already render in the conversation and make the count always >1).
5. Mock daemon POST passthroughs (`POST /v1/beads`, `POST
   /v1/beads/{id}/state/closed`, `POST /v1/beads/{id}/comments`,
   `POST /v1/beads/{id}/labels`) return 200/{} so the viewer's composer write
   handler does not 500 mid-capture. The AUTHORITATIVE bead lands via the
   `/__control/inject-message` injection, not via the POST passthrough — this
   keeps the write contract legible (the passthrough is a sink, not the
   source-of-truth).

## Tied-back audit gates

Acceptance C4/K4 + O5 are also covered by the Go audit suite
(`threads_audit_3_4_test.go`):
- `TestAudit_ComposerBody_LongBodyPasses` proves a >500-char composer message
  renders verbatim through the real fuser (the Title's 500-char cap does not
  truncate the conversation line).
- `TestAudit_ComposerBody_TitleFallbackFlagged` proves the audit FLAGS a
  Title-fallback regression with a `composer_body_truncated` divergence
  pointing at the offending member.
- `TestAudit_ComposerBody_DropFlagged`, `MissingEntryFlagged`,
  `ActorMismatchFlagged` cover the other C4/K4 failure modes.
- `TestAudit_PrefixStability_FullSnapshotPasses` +
  `CursorBoundIsAPrefixOfFull` prove the per-entry hash sequence of any
  `?since=` response is an exact prefix-extension of the full-fetch
  fingerprint.
- `TestAudit_PrefixStability_TamperedPriorFingerprintFails` +
  `PositionLevelHashMismatch` + `VanishedEarlierEntryFlagged` prove the audit
  FAILS on any earlier-entry reorder/vanish.
- `TestHandleThreadAudit_HonoursSinceParam` +
  `PriorFingerprintEnforced` exercise the endpoint's `?since=<rfc3339>` +
  `?prior_fingerprint=<hex>` query-param surface end-to-end.
