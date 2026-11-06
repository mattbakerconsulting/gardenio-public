# /threads end-to-end seeded verification — Phase 3 task 3-6 (ka-0000006435)

Task 3-6 (ka-0000006435), the FINAL child of epic ka-0000002477 / ka-0000000679.
Operator: Matt Baker. Graded against rubrics **ka-0000007635** (Phase 3) +
**gd-ekEoieO8YE** (Phase 2).

This is the closing E2E proof: a single seeded **throwaway persisting** daemon
exercising the full first-class /threads surface against the **live HTTP path** —
`POST /b/{id}/thread-reply` (compose) → `?since=` incremental poll → `GET
/threads/{id}/audit` structured PASS — plus the full Playwright visual matrix
graded per the 12-item rubric. No new features; no controller code changed.

## What was run (all hermetic, no prod contact, no Postgres/Keycloak)

New reusable harness under `gardenio/tests/beads-viewer-screenshots/tools/`:

| File | Purpose |
|---|---|
| `e2e-seeded-daemon.mjs` | A **persisting** in-memory beads daemon (the 3-3 mock's write path is a no-op; this one stores `POST /v1/beads` + `/close` so the composed bead is the bead the audit reads back). Seeds the multi-member thread with pinned UTC timestamps. |
| `drive-threads-e2e.mjs` | Drives compose → `?since=` poll → audit and asserts structured PASS on every C/K/O gate + the cross-check. |
| `run-e2e-seeded.sh` | Orchestrator: boots the seeded daemon + the real `beads-viewer` (dev-auth bypass) + the driver, tears down on exit. |

Run it: `VIEWER_BIN=/tmp/beads-viewer OUT_DIR=test-results/threads-e2e \
gardenio/tests/beads-viewer-screenshots/tools/run-e2e-seeded.sh`

## Seeded thread `gd-wishE2E36` (mirrors threads_test.go / threads_audit_*_test.go)

Every membership + closure shape the acceptance criteria enumerate:

- **wish HEAD** — Slack-bound (`slack_thread_channel`/`slack_thread_ts`) so the composer can post.
- **field-only member** (`gd-0YttqNEzLP`) — `fields.thread_id`, no parent-child dep.
- **closure-only HALF-LINK** (`gd-rMd20UgxbS`) — parent-child dep under the epic, **no** `thread_id` field (the 1b-3 write window); reached only by the reverse-dep BFS.
- **closed member, VALID ClosedAt** (`gd-g9OfqUaeoF`) — clean `bead_closed`, not imputed.
- **closed member, EMPTY ClosedAt** (`gd-9m0U6OHUFv`) — `bead_closed` imputed to `created_at`, surfaced as an `unparseable_closed_at` **warning** (never a silent drop).
- **pre-seeded >500-char composer message** (`gd-msgSEED36`) — `type=message`, `fields.text` = 771 chars.
- comments on the head + a work member (fused conversation).
- **live-composed >500-char message** (`gd-live00000136`, 539 chars) added at runtime via the real write path.

## A. Log-correctness audit — structured PASS on every gate

Live path drove: baseline audit → `POST /b/gd-wishE2E36/thread-reply` (539-char
body, HTTP 204) → `?since=` poll (body rendered, **exactly once**, cursor
advanced) → full audit → cursor-bound audit. Result: **`audit.pass = true`**.

Post-compose audit (`test-results/threads-e2e/e2e-audit-post-compose.json`):
`rendered_total=13 == expected_total=13`, `conserved=true`,
by-kind `bead_created=6, comment=3, bead_closed=2, message=2`.

| Gate | Verdict | Evidence |
|---|---|---|
| **C1/C6** COUNT conservation (total + per-kind + per-member) | **PASS** | `rendered=13 == expected=13` (N=6 members-with-body, K=3 comments, M=2 closable, P=2 messages); no `member_count_mismatch` divergence. |
| **C4/K4** composer body completeness (>500 not truncated to Title) | **PASS** | `composer_body.conformant=true` over **2** message members (seeded 771-char + live 539-char); full `fields[text]` renders verbatim, operator email on `Author`. |
| **C5/K3** closed-event integrity | **PASS** | `gd-9m0U6OHUFv` empty ClosedAt → `unparseable_closed_at` **warning** (imputed to `created_at`, NOT dropped); `closable=2` (valid `gd-g9OfqUaeoF` + imputed both emit `bead_closed`). |
| **K1/K2/K5** hash multiset + attribution | **PASS** | `hash.matched=true`, zero fabrications, zero drops; no `title` (bleed) divergence. |
| **O1–O4** ORDER strict-monotonic + unique 4-tuple | **PASS** | `order.strictly_monotonic=true`, `order.unique=true`, fingerprint `cb24853b52df…`. |
| **O5** `?since=` prefix stability | **PASS** | within-snapshot `stable=true`; **cursor-bound across-snapshot** proof: fed the pre-compose fingerprint back at the pre-compose cursor after the compose appended a later entry → `stable=true`, `prefix_length=12`, prior fingerprint honoured (`test-results/threads-e2e/e2e-audit-cursor-bound.json`). |
| `/threads/{id}/audit` read-only + viewer-scoped | **PASS** | plain HandleFunc, no operator gate, no write surface (server.go:1075). |

## B. Visual grading matrix — 171 screenshot pairs, 0 failures

`capture-threads.mjs` across **{baseline, dark-terminal, dark} × {480, 768, 1280}**
including the now-live composer (V5) + optimistic (V10) cells. Graded by a
vision-capable agent from the contact sheets + individual full-res cells.

| Gate | Verdict | Evidence |
|---|---|---|
| **V1** no blank/white panes; error keeps chrome | **PASS** | every index/detail/composer cell renders full chrome; forced `list`/`timeline` failures keep nav + crumbs + banner. |
| **V2** conversation-first / work-collapsed default | **PASS** | detail leads with Conversation; "Agent activity" `<details>` CLOSED on load. |
| **V3** wish narrative leads; work tally demoted | **PASS** | index cards lead with the bold wish; tally is muted meta ("4 beads of work" / "no work yet"). |
| **V4** readable contrast BOTH themes | **PASS** | dark-terminal legible; **`data-skin="dark"` byte-identical to dark-terminal** — the 3-5 `[data-skin^="dark"]` alias fix holds; the former contrast trap is FIXED, not just filed. |
| **V5** composer reachable | **PASS** | labelled "Reply as <email>", placeholder, **visible blue focus ring**, Send button, ⌘/Ctrl+Enter hint. |
| **V6** no h-scroll / clipped cards at 480; mobile single-column | **PASS** | 480px single-column, full-width cards + stacked textarea/Send, text wraps, no overflow. |
| **V7** desktop rail + long-thread scroll | **PASS** | 16-reply + 9-work long thread scrolls cleanly; work-expand disclosure opens. |
| **V8** part-of-wish chip | **PASS** | resolved chip links to `/threads/{thread_id}`; fallback (missing head) + absent (on heads) cells captured. |
| **V9** needs-triage banner + fulfilled marker | **PASS** | amber needs-triage card + fulfilled close-marker both render. |
| **V10** optimistic send + zero-dup reconcile | **PASS** | optimistic "sent" bubble appears immediately; the capture driver's `waitForFunction` proves exactly ONE body + zero residual `.is-pending` after the `?since=` reconcile. |
| **V11** alignment/spacing sane | **PASS** | no overflow/overlap across 171 pairs at all 3 breakpoints. |
| **V12** every FAIL cell links its screenshot | **N/A** | zero FAIL cells; all screenshots attached (contact sheets below). |

### Contact sheets (gb image upload)

- baseline 1280: https://gardenio.app.e2e.dev.acme.example/beads/image/coop/sessions/images/2026-07-01/6362385b-contact-baseline-1280.png
- dark-terminal 1280: https://gardenio.app.e2e.dev.acme.example/beads/image/coop/sessions/images/2026-07-01/ae4ac499-contact-dark-terminal-1280.png
- dark (V4 contrast probe): https://gardenio.app.e2e.dev.acme.example/beads/image/coop/sessions/images/2026-07-01/99fa0956-contact-dark-1280.png

### Key individual cells

- composer focused (V5 focus ring): https://gardenio.app.e2e.dev.acme.example/beads/image/coop/sessions/images/2026-07-01/ecd44fc1-threads-composer-focused-baseline-1280.png
- composer optimistic (V10 bubble): https://gardenio.app.e2e.dev.acme.example/beads/image/coop/sessions/images/2026-07-01/11c5b828-threads-composer-optimistic-baseline-1280.png
- composer typed @480 (V6 mobile): https://gardenio.app.e2e.dev.acme.example/beads/image/coop/sessions/images/2026-07-01/6252d652-threads-composer-typed-dark-terminal-480.png
- index populated (V3/V9): https://gardenio.app.e2e.dev.acme.example/beads/image/coop/sessions/images/2026-07-01/2fc7fd58-threads-index-populated-dark-terminal-1280.png
- bead-chip resolved (V8): https://gardenio.app.e2e.dev.acme.example/beads/image/coop/sessions/images/2026-07-01/01f99b4b-threads-beadchip-resolved-baseline-1280.png

## C. Index MemberCount vs audit member-union cross-check — reconciled

- Audit member-union (`members[]`, field ∪ parent-child closure): **8** members.
- Index `MemberCount` fast-path (head + `fields.thread_id`-bound derivatives): **7**.
- **Delta = 1 = the half-link member `gd-rMd20UgxbS`** — a parent-child descendant
  the closure BFS reaches, but which does not yet carry the `thread_id` field
  (the 1b-3 write window between the durable dep and the field copy).

This is the **documented, by-design index-undercount** (`threads.go`
`MemberCount` doc: "half-links … are a sub-second window and are not reflected
here. The detail view's `threadMembers` computes the exact field ∪ closure
union"). Not a bug — the index is a cheap fast-path count; the detail/audit
compute the authoritative union. The delta is exactly the seeded half-link,
confirming the two paths agree modulo the documented window.

## D. Process gates

- `go build ./cmd/beads-viewer/` — **green**. `helm lint gardenio/helm/gardenio/` — **green** (0 failed).
- Threads Go suite (`go test ./cmd/beads-viewer/ -run 'Thread|Curation|Composer|MsgKey|Markdown|DarkSkin|Loading|Audit|Timeline'`) — **PASS** (37.7s).
- New files are JS/shell test harness only — **no controller code changed**, so the
  ≥60% patch-coverage gate does not apply (no Go diff). `node --check` + `bash -n` clean.
- Writes are bridge-mediated + operatorOnly-parity (the composer POSTs go through
  the already-merged `viewerOnly`-gated `POST /b/{id}/thread-reply`; the viewer
  holds no Slack token) — A6/A7 satisfied by construction, unchanged by this task.

## Verdict

**PASS.** The seeded throwaway-daemon E2E reproduces compose → poll → audit-PASS →
visual-green end to end. All C1/C4/C5/C6/K1–K5/O1–O5 audit gates return structured
PASS on a genuinely multi-member thread; the full visual matrix grades green on
all 11 substantive rubric gates (V12 N/A); the MemberCount/union cross-check
reconciles to the documented half-link window. Epic ka-0000002477's terminal goal
is met.
