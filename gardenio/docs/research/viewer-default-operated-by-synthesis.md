# V-Synthesis: beads-viewer default `operated_by=me` — research synthesis (retrospective)

**Status:** synthesis of merged research docs. No implementation in this doc.
**Parent epic:** ka-0000002754 (research-only).
**Decision checkpoint task:** ka-0000001589.

**Sibling research docs (all merged):**

- V-R1 inventory — `viewer-default-operated-by-r1-inventory.md` (ka-0000000145, MR !2005)
- V-R2 UX — `viewer-default-operated-by-r2-ux.md` (ka-0000003485)
- V-R3 server — `viewer-default-operated-by-r3-server.md` (ka-0000001699)
- V-R4 migration/sunset — `viewer-default-operated-by-r4-migration.md` (ka-0000008030)

**Already-resolved decision and downstream:**

- Decision ka-0000007227 (filed 2026-05-03, closed same day, `approve` option chosen).
- Implementation epic ka-0000005389 — **closed**. P1–P7 merged on main (commits `36583808`, `0a9390ab`, `7a05fd19`, `d685cfcb`, `12d70882`, `a571d970`, `72556fc0`).
- Verification — `viewer-default-operated-by-d-verification.md` (ka-0000005487).

This synthesis exists for the audit trail. The umbrella gardener auto-spawned ka-0000001589 before noticing that the decision checkpoint had already fired and the implementation had shipped; the doc captures what R1–R4 collectively decided, what the operator approved, and what is left to chase.

## 1. The problem in one paragraph

Matt asked (Slack, 2026-05-03): *"make the operated by filter the default for a beads viewer user (all beads created so far are by me for migration purposes)"*. Current behaviour was to render the unfiltered firehose; an OIDC-authenticated user landing on `/active` should see "their" world by default, with the URL still able to override. The migration caveat — most rows are Matt-owned today — is real and time-bounded; whatever ships must not assume it.

## 2. R1–R4 decisions, distilled

| # | Question | Answer | Source |
|---|---|---|---|
| 1 | Which surfaces default-on? | **Only `/active` (incl. `?project=` variant).** Every other list (search, decisions, agents, advice, schedules, costs, health, views, projects, ratings, p/{project}, cmdk) stays default-off; some get an opt-in `me` chip. | V-R1 |
| 2 | What is the cold-load contract? | OIDC viewer + no `operated_by` key + default-on surface → server applies `operated_by=me` (alias resolves to viewer email). URL always wins; explicit empty `?operated_by=` is "show all" and must never auto-promote. | V-R2 §1, V-R3 §1 |
| 3 | What about anonymous / static-token / dev viewers? | No email → no default; chip surfaces a "Sign in" CTA. `me` from anonymous folds to "no filter". | V-R2 §6, V-R3 §2 |
| 4 | URL aliases | `me` → viewer email, `null`/`(none)` → IS NULL, empty → all, other → literal. Daemon never sees `me`; alias expansion lives in the viewer. | V-R2 §2 |
| 5 | Per-user persistence? | **None in V1.** URL is the only state. Cookie/`user_pref` bead deferred to a follow-on epic with its own decision. Precedence locked: URL > (future) cookie/bead > computed default. | V-R2 §3 |
| 6 | Indication chip | Single calm header chip with five states (default, all, unowned, other, anonymous). `(default)` pill marks cold-load. Reuses R8 operator-chip rendering. | V-R2 §4 |
| 7 | Migration banner | Separate from chip; rendered only for non-Matt OIDC viewers on default-on surfaces; cookie-dismissible. | V-R2 §5 |
| 8 | Sunset trigger | **Config bead flag** (`viewer.show_migration_banner`) **with calendar backstop `2026-08-01`.** Cardinality threshold rejected (gameable, hand-wave N, hot-path cost). Calendar-only rejected as primary (blind to slips). | V-R4 §3 |
| 9 | Server-side insertion point | In the handler, between URL parse and `ListBeadsFiltered`. Trigger on `r.URL.Query().Has("operated_by") == false` (NOT `raw == ""`). Flip the *raw* param to `"me"` and let `resolveOperatedByParam` resolve as today; `MyWorkSummaryMode` lights up unchanged. | V-R3 §1 |
| 10 | Composition with `?project=` / `?status=` / `?type=` | AND-by-construction at the daemon. No OR paths anywhere. | V-R3 §3 |
| 11 | Saved views | Default-injection lives in the route handler, **not** in the daemon client. `handleViewShow` consumes spec verbatim. | V-R1 / V-R3 §6 |
| 12 | Sibling decision/queued counts on `/active` | Stay fleet-wide. Don't propagate `me` into them — the banner is supposed to surface fleet-wide attention even for items the viewer didn't initiate. | V-R3 §7 |
| 13 | Cache implications | Viewer responses on default-on surfaces are now viewer-dependent. Emit `Cache-Control: private, no-store`; if a future LRU lands, key off the *outbound* daemon URL, not the inbound viewer URL. | V-R3 §5 |
| 14 | Unowned beads under default-me | **Strict reading.** `operated_by IS NULL` rows reachable only via `?operated_by=null`; never folded into default-me. Chip's `[ Unowned ]` button is the one-click path. | V-R4 §4 |
| 15 | Auto-claim on view? | No. Dibs is a write-time rule (T-R3). Mere viewing does not operate. | V-R4 §4.5 |

## 3. What was approved and shipped

Decision ka-0000007227 approved the **`approve` option** on 2026-05-03. The verbatim scope from the decision option was: *"default `operated_by=me` on `/active` and `/search`, the migration banner with config-flag sunset + 2026-08-01 calendar backstop, no-auto-claim, unowned reachable only via `?operated_by=null`. Banner copy from V-R2 §5.2."*

P1–P7 from ka-0000005389 landed on main and the implementation epic is closed. The verification doc (ka-0000005487) confirms code-level wiring matches the synthesis above; the seven UI-bound smoke checks were escalated to the operator because the agent fleet cannot impersonate an OIDC session from inside the cluster.

Note: the shipped scope included `/search` as default-on, which differs from V-R1's recommendation of *"`/search` default-off, with an opt-in chip"*. That divergence is recorded in the decision option text and was the operator's call. The synthesis flags this as a feed-forward question if a future reversal is desired (see §5).

## 4. Why this synthesis exists

The umbrella gardener's automation noticed R1–R4 were merged and queued ka-0000001589 to "open the decision checkpoint". It did not check whether the decision had already been filed in May. Filing a verbatim `approve-full / approve-minimal / revise / defer` decision now would (a) confuse the operator, (b) produce a stale audit trail, and (c) bypass the already-resolved approval. The synthesis doc captures the closed loop instead.

## 5. Outstanding feed-forward items (not blocking)

These were surfaced by R1–R4 and are *not* covered by the shipped implementation. Each is a candidate for a follow-on bead under a new epic, **only if the operator wants to chase it**:

1. **`oidc:<email>` vs raw-email row-match gap** (V-R3 §2). The viewer's chip recognises both forms; the daemon's `OperatedBy=` filter today matches the bare email exactly. Default-on amplifies the surprise. Resolution is either (a) widen the daemon match to the canonical id set, or (b) viewer-side multi-id queries. Belongs in the transitivity work, not viewer.
2. **`/search` default-on reversal** (V-R1 vs shipped scope). V-R1 argued search should be default-off because users searching `q="canonical-prod"` expect global recall. The shipped scope flipped this on. If the operator observes "I know this bead exists but search can't find it" surprise, a small revert + opt-in chip is the V-R1-aligned fix.
3. **Saved-view `OperatedBy` field** (V-R1 / V-R3 §6). If saved views should ever encode "scope to viewer", `savedViewSpec` needs a new field; the R3 boundary holds (spec-set value wins, default-injection is bypassed).
4. **Per-user persistence (V2 cookie / V3 `user_pref` bead)** (V-R2 §3). Defer until telemetry shows users escape the default repeatedly. The precedence slot is reserved.
5. **Mobile / narrow-viewport chip layout** (V-R2 §9 / V-R3). The three-action chip row will not fit on a 320 px viewport. Needs a collapsed `▾` menu form if mobile is in scope.
6. **Telemetry on banner dismissal and on default-escape rate** (V-R3 / V-R4). Cheap to add, useful to drive the V2 persistence decision and to validate the banner is doing real work.
7. **R8's `[ My beads ]` hero nav link** (V-R2 §9). Now redundant on `/active`. Either remove, or repurpose to deep-link a default-off surface (e.g. `/search?operated_by=me`).

## 6. Decision artifact

Filed under ka-0000001589 as a `gb decision create --no-wait` with options reframed for the post-shipment reality:

- `ack-shipped` (report): acknowledge the loop is closed; mark ka-0000001589 as a no-op against already-shipped scope.
- `followup` (epic): operator picks one or more §5 items and authorises a follow-on epic.
- `revise` (report): operator wants a specific shipped behaviour reconsidered (e.g. `/search` default-off reversal).
- `defer` (report): park the §5 items entirely.

The decision is `--no-wait` and the task does not yield — this is a notice + record-keeping checkpoint, not a blocker.

## 7. Links

- Parent epic: ka-0000002754 (research-only).
- Implementation epic (closed): ka-0000005389.
- Decision (closed): ka-0000007227.
- Verification: ka-0000005487 / `viewer-default-operated-by-d-verification.md`.
- This task: ka-0000001589.
- R8 prior art: `docs/research/human-attribution-ux.md`.
- Code touchpoints (read-only refs): `gardenio/controller/cmd/beads-viewer/{operator_chip.go, active.go, search.go, oidcauth/oidcauth.go}`, `gardenio/controller/internal/beadsapi/client_query.go`.
