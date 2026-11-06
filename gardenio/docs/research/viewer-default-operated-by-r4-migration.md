# V-R4: Migration & sunset of "all corpus is Matt"

**Status:** research only — no implementation, no test code.
**Parent epic:** ka-0000002754 (research-only).
**Bead:** ka-0000008030.

**Sibling docs (all merged):**

- `viewer-default-operated-by-r1-inventory.md` (V-R1, ka-0000000145) — surface inventory; **only `/active` is default-on**.
- `viewer-default-operated-by-r2-ux.md` (V-R2, ka-0000003485) — chip + override semantics; forwarded the migration banner's sunset trigger to this doc.
- `viewer-default-operated-by-r3-server.md` (V-R3, ka-0000001699) — server-side default-injection + cache key implications.

**Transitivity workstream (the upstream that makes Phase 1 possible):**

- `operated-by-transitivity-r1-inventory.md` (T-R1, ka-0000005258) — current state of `operated_by` propagation.
- `operated-by-transitivity-r2-propagation.md` (T-R2, ka-0000000973, MR !2014) — push-at-spawn + pull-at-resolver hybrid model.
- `operated-by-transitivity-r3-dibs.md` (T-R3, ka-0000006715) — "last modifier becomes operator" + operator-class lattice.
- `operated-by-transitivity-r4-audit.md` (T-R4, ka-0000001481) — audit-trail design.

## Why this exists

V-R1 settled *where* the default applies (one surface: `/active`). V-R2 settled *how the chip behaves and what URLs it emits*, and explicitly forwarded **two** questions to this doc:

1. When does the migration banner disappear? (V-R2 §5.5)
2. How do "operator unknown" beads — the long tail of pre-cutover rows with no `operated_by` annotation — interact with the default-me filter? (V-R2 §1, V-R1 open question 4.)

V-R3 settled the *implementation shape* of the default-injection. None of V-R1 / V-R2 / V-R3 settle the **time dimension**: the corpus today is overwhelmingly Matt-owned, so a default-on `/active` for non-Matt OIDC users would render a near-empty page through no fault of theirs. The transitivity epic (T-R1 → T-R4) is the project that *fixes the input* by propagating real operator identity through every spawn / formula / dibs path. V-R4 is where the viewer side decides:

- What "Phase 0" (right now, pre-transitivity) looks like, and why we are still willing to ship default-on through it.
- What "Phase 1" (post-transitivity) looks like, and what the viewer learns to do once the corpus diversifies.
- The single, non-derived **sunset trigger** that flips the migration banner off.
- Whether the legitimate "no operator at all" case (formula expansion, scheduler-spawned, pre-backfill rows) ever gets folded into default-me. Spoiler: no.

The output is a phase plan + one decision (sunset trigger) + one decision (backfill semantics), with the threading back through V-R2's chip and banner contracts.

## 1. Phase 0 — "all corpus is Matt"

### 1.1 What Phase 0 means

Phase 0 is the state of the world *until* the transitivity epic's outputs land in production. Specifically:

- Today (2026-05-03) the only OIDC-authenticated human in the beads-viewer is Matt (`oidc:matt@…`). Every other "operator" identity attached to a bead is a sentinel (`system:scheduler`, `sched-…`, `static`) or a `slack:U…` from the bridge, none of which the viewer's `me` alias maps to.
- A second OIDC user logging in *today* would see `/active` with `operated_by=oidc:them@…` applied → the daemon returns near-zero rows because no bead has been stamped with their email yet. The chip would correctly say "Showing your work" and the page would correctly be empty. Both are technically correct; together they read as a broken page.
- The transitivity epic fixes the *input*: T-R2's hybrid push-at-spawn + pull-at-resolver model means a logged-in non-Matt human will start *making* beads stamped with their email, including beads spawned by their schedules and operated on by their agents.

### 1.2 Cardinality verification (the "kd list cardinality" check)

The bead description directs the V-R4 author to verify Phase 0 by listing the cardinality of `operated_by=oidc:<email>` and confirming that today's distribution is degenerate.

Today's daemon does not surface `operated_by` as a column on the bead-list JSON (see §1.3). The verification therefore takes one of two shapes:

- **Per-bead introspection**: `kd show <id> --json` does not include `operated_by` as a top-level field. The annotation lives on bead *events* (creates / updates / comments) — see T-R1 §3 ("operated_by is event-shaped, not row-shaped"). Counting requires a daemon-side aggregation, not a CLI loop.
- **Daemon filter**: `ListBeadsFiltered(OperatedBy="oidc:matt@…")` returns the cohort. Calling it with `OperatedBy="oidc:other@…"` for any other email returns approximately zero. The viewer already drives this exact path on `/active?operated_by=<email>`.

I executed the closest-available CLI proxy — created-by cardinality across 5,000 issue beads (`kd list --limit 5000 --all-projects --json | jq -r '.[].created_by' | sort | uniq -c | sort -rn`) — as a sanity check on the population shape. The top creators are `static` (3,164 of 5,000), `null` (164), then a long tail of `thread-…` and `sched-…` IDs. **Zero `oidc:` creators appear** in that sample, which is consistent with the claim that the OIDC viewer is brand-new and no bead row was *created* under an OIDC identity. Operated-by is event-shaped, not creator-shaped (T-R1), so this is suggestive rather than dispositive — but the directional answer is unambiguous: no non-Matt OIDC operator has any beads to find.

The implication for V-R4: **we do not gate Phase 0 on a cardinality check at runtime**. We accept that the page will look near-empty for any non-Matt OIDC user during Phase 0 and we mitigate that with the migration banner (V-R2 §5), not with a runtime threshold that flips the default off.

### 1.3 Why default-on is still acceptable in Phase 0

Two reasons this is the right call even with the corpus skewed:

1. **The chip + banner together are honest.** V-R2 §4.2 makes the default explicit ("Showing your work" with a `(default)` pill) and V-R2 §5 wraps it in a migration banner that explains *why* the page may be empty and points at the `[Show all]` escape. The user is never told the system is broken; they are told the corpus is mid-migration.
2. **Reverting to default-off is more expensive than enduring Phase 0.** If we ship default-off "until the corpus diversifies", every non-Matt user is going to learn to use `?operated_by=me` manually, and we have to *un-train* that habit later. Defaulting-on now (with the banner) sets the right reflex from day one: "Active is your queue; click `Show all` to expand."

### 1.4 Phase 0 affordances for Matt himself

Matt is the one OIDC user for whom default-me already works correctly today. The chip on `/active` for Matt will say "Showing your work (default)" and show the actual list. **The migration banner does not render for Matt** (V-R2 §5.3 rule 2: "the resolved viewer email is not Matt's") — it would be redundant from his vantage. This is hard-coded against Matt's email until the sunset trigger fires; see §3.

## 2. Phase 1 — post-transitivity, default-me is meaningful

### 2.1 What Phase 1 means

Phase 1 begins when the transitivity epic's outputs are deployed:

- T-R2's push-at-spawn means schedules / agents / formulas inherit the spawning operator's identity at every step (covered: scheduler chains, formula expansion, dibs-rebound chains).
- T-R3's dibs rule means an operator who *modifies* a bead becomes its current `operated_by` (with sentinel-class chain-stop semantics so `system:scheduler` doesn't transfer to humans by accident).
- T-R4's audit table records every operator transition for retroactive forensics.

Once those land, a second OIDC human (call them Bob) starts producing beads stamped with `oidc:bob@…` immediately on first interaction:

- Slack threads they reply to → the bridge stamps `oidc:bob@…` (via OIDC linkback or the existing `slack:U…` → email mapping).
- Schedules they own → the spawned agent's beads carry `oidc:bob@…`.
- `/active` for Bob now actually returns rows because real beads exist with his identity.

Phase 1 is the steady-state world the viewer was designed for. The migration banner is the only piece of the V-R2 contract that should *change* between phases; everything else — chip, redirect, override semantics, persistence story — is identical.

### 2.2 What changes in the viewer between Phase 0 and Phase 1

| Concern                                           | Phase 0 (now)                                      | Phase 1 (post-transitivity)                       |
|---------------------------------------------------|----------------------------------------------------|---------------------------------------------------|
| Default applied on `/active` for OIDC viewers     | yes (V-R2 §1)                                      | yes (unchanged)                                   |
| Chip on `/active`                                 | "Showing your work (default)" (V-R2 §4)            | "Showing your work" — the `(default)` pill stays  |
| Migration banner for non-Matt OIDC viewers        | rendered (V-R2 §5)                                 | **not rendered** (sunset)                         |
| Migration banner for Matt                         | never rendered                                     | never rendered                                    |
| Backfill ("unknown operator") rows                | reachable only via `?operated_by=null` (§4)        | unchanged                                         |
| Server-side default-injection logic (V-R3)        | unchanged across the phase boundary                | unchanged                                         |

The single bit that flips at the phase boundary is the migration banner's visibility. Everything else is invariant.

### 2.3 What does not change

The default-injection contract is invariant across phases. We are emphatically *not* gating the default itself on cardinality. The reason is in §1.3: the user-experience contract should be the same whether the corpus is 99% Matt or 33% Matt or 5% Matt.

## 3. Sunset trigger for the migration banner

V-R2 §5.5 enumerated three options and forwarded the final call here. V-R4 picks **one** and writes down the justification.

### 3.1 The three options on the table

| Option              | Mechanism                                                                                  | Pros                                                          | Cons                                                                                                                |
|---------------------|--------------------------------------------------------------------------------------------|---------------------------------------------------------------|---------------------------------------------------------------------------------------------------------------------|
| **A. Calendar date**| Banner self-removes after a fixed date (e.g. `2026-08-01`).                                | Cheapest; one constant in the viewer.                         | Brittle if transitivity slips. Date passes regardless of whether the corpus has actually diversified.               |
| **B. Cardinality threshold** | Banner self-removes once ≥ N distinct non-Matt `operated_by` values exist on beads created in the last 30 days. | Self-tuning; ties banner lifecycle to actual diversification. | Adds a daemon-side count call on every viewer page-load (or a cache layer). Gameable by spawn churn (one human running 5 schedules looks like 5 operators). N is a hand-wave. |
| **C. Config flag**  | Operator flips a boolean in viewer config (env var, flag bead, or `gardenio-config` bead).  | Simplest mental model; matches Matt's "we'll know when we see it" preference; fully under operator control. | Requires a human to remember to flip it. If the operator forgets, the banner persists indefinitely.                  |

### 3.2 The pick: **C (config flag) with A (calendar) as a backstop**

Concretely:

- Primary trigger: a single boolean — `BeadsViewer.ShowMigrationBanner` — defaulted to `true` in Phase 0 and intended to be flipped to `false` by the operator when transitivity work has visibly landed and a second OIDC human has started accruing beads.
- Backstop trigger: a hard-coded date (`2026-08-01`, ~three months from V-R2's draft). If the flag has not been touched by then, the banner self-removes. The flag still wins if explicitly set to `true` after the backstop date — the date is a "we forgot" failsafe, not a hard ceiling.
- The viewer renders the banner iff `BeadsViewer.ShowMigrationBanner == true && time.Now().Before(BeadsViewer.MigrationBannerBackstop)`. Both knobs live in viewer config; neither requires a deploy if they are config-bead-driven (T-R3-style config bead consumed at viewer startup + reload).

### 3.3 Why C beats A and B

**vs. A (calendar-only):** A pure calendar trigger is cheaper but blind. If transitivity slips by a month and we pick `2026-08-01`, non-Matt users see a near-empty `/active` with no banner explaining why for the entirety of August. Calendar-as-primary inverts the safety relationship: the failure mode is "user is silently confused", which is the exact failure mode the banner exists to prevent. Calendar is fine as a backstop because by then the failure mode has flipped — operators not flipping the flag means we are now showing a *stale* banner long after the corpus diversified, which is annoying but not confusing.

**vs. B (cardinality threshold):** Cardinality is the option that *sounds* correct but bakes in a runtime metric the viewer was not built to compute. Three concrete problems:

1. **Defining N is a hand-wave.** Is "diversified" 2 humans? 5? The corpus is a small team; any specific N is a guess and will need tuning. The flag form skips this question.
2. **Spawn churn looks like diversification.** T-R2's hybrid model means each scheduled gardener a human owns produces beads with that human's identity. A single Matt-launched gardener orchestrating five schedules under five different sub-bead chains can produce five distinct `operated_by` values that all trace back to Matt. Cardinality cannot tell the difference; the flag form does not need to.
3. **Cost.** A `count(distinct operated_by)` over the last 30 days is a real query. To run it on every viewer request would add a hot-path daemon call; to cache it would add a TTL/invalidation layer. The flag form is a constant-time read.

We dismiss B even as a backstop — its failure modes (gameable, hand-wave N) are in the wrong direction for a backstop role.

**vs. flag-only:** The pure flag with no backstop has the "operator forgets" failure mode. Three months feels like enough time for the team to notice the banner is stale; if not, the calendar quietly removes it.

### 3.4 What "the operator flips the flag" actually means

The `BeadsViewer.ShowMigrationBanner` boolean is a config knob, not a deploy-gated constant. Two viable shapes for the runtime read; V-R3's implementation epic picks one:

- **Env var**: `GARDENIO_VIEWER_SHOW_MIGRATION_BANNER=true|false`, read at viewer startup. Cheapest; forces a viewer restart to flip.
- **Config bead**: a `gardenio-config` bead with a `viewer.show_migration_banner` field, polled by the viewer on a 60s interval (matches the existing `gardenio-config` consumption pattern from T-R3 / scheduler config). No restart; auditable via bead history; matches the project's "configuration is also a bead" reflex.

Recommendation: **config bead**. It's the natural shape — the migration banner lifecycle is a project-wide event, the config bead is the project-wide config primitive, and bead history captures the flip for forensics ("when did we declare migration done?").

The backstop date is a constant in the viewer code (`viewerCmd.MigrationBannerBackstop`) — there is no value in making it config-bead-driven, since the backstop's whole purpose is to fire when the operator has *forgotten* the config bead.

### 3.5 What happens when the trigger fires

- The banner stops rendering on the next page-load. There is no transition animation, no "migration complete" celebration banner, no toast. The page silently goes back to chip-only.
- Existing dismissal cookies (V-R2 §5.4) are left alone — they are no-ops once the banner is gone, and clearing them on the trigger would require code that runs once and then never again, which is not worth the complexity. They will eventually expire on their own (1-year TTL per V-R2).
- Bookmarked URLs are unaffected. The chip's behavior is identical pre- and post-trigger.

## 4. Backfill — should "operator unknown" beads surface under default-me?

### 4.1 The question

A non-trivial fraction of the corpus has no `operated_by` annotation at all:

- Pre-cutover beads created before the transitivity work introduced operator stamping.
- Schedule-spawned beads whose chain terminates at a sentinel (`system:scheduler`) before reaching a human (T-R3 §5: sentinel-class chain-stop is intentional).
- Formula expansions that ran with no caller `X-Operated-By` header (T-R2 §4 explicitly fingers this case as a "no operator" terminus).
- Bridge-emitted beads (jira-bridge / gitlab-bridge) where the emitting source had no resolvable identity.

These rows should be reachable, but the question is whether the viewer's default-me filter should *include* them. Two readings:

- **"My work" should mean only the beads stamped to me.** Period. Unknown-operator rows live behind the `?operated_by=null` URL (V-R2 §1.1, line 4 of the contract table). This is the strict reading.
- **"My work" should mean the beads I can plausibly act on, including the un-attributed ones I'd otherwise miss.** This is the broad reading: the default would mean "operated_by IS NULL OR operated_by = <viewerEmail>".

### 4.2 The pick: **strict reading — keep unknowns under `?operated_by=null` only**

Concretely:

- Default-me on `/active` resolves to `OperatedBy="oidc:<viewerEmail>"` exactly. Unknowns (`operated_by IS NULL`) do **not** appear.
- Unknowns are reachable only via `?operated_by=null` (which renders V-R2 §4.2's "Showing unowned work" chip state).
- The chip's `[ Unowned ]` button in the default-applied state (V-R2 §4.2 row 1) is the one-click escape into that view.

### 4.3 Why this is right

Three reasons:

1. **The default's promise is "yours".** A user who lands on `/active` and sees "Showing your work (default)" is making one assumption: every row below is theirs. Folding in unknowns silently breaks that contract — the page now shows their work *plus* a heap of un-attributed scheduler droppings, formula expansions, and pre-cutover noise that the user has no particular reason to care about. The default's authority comes from being a tight, predictable filter; widening it dilutes the value.
2. **Unknowns are a pre-cutover migration artifact, not a category of work.** By Phase 1, new `operated_by IS NULL` rows are rare — only formula expansions with no caller identity and a few sentinel-stopped chains. The rump of "real" unknowns is the historical pre-cutover corpus, which is exactly what the migration banner already directs the user toward (`[ Show all ]` and the project structural views like `/p/<project>`). We do not need a second back-door into that data through default-me.
3. **It compounds the cardinality skew.** In Phase 0, "operated_by IS NULL OR operated_by = <viewerEmail>" for a non-Matt user is essentially "everything that wasn't created by Matt's agents", which is a ~uniform random sample of the corpus. That is *not* "their work" by any honest definition. The strict reading gives them an honestly-empty `/active` that the banner explains; the broad reading would silently misrepresent old un-attributed Matt activity as "theirs".

### 4.4 Affordances that compensate for the strict reading

The cost of the strict reading is that a user staring at an empty `/active` may not realize there is unowned work that they could plausibly claim. We address this *through V-R2's existing affordances*, not by changing the default-me semantics:

- The **chip's `[ Unowned ]` button** is one click away from default-me. V-R2 §4.2 row 1 makes it a primary action. This is the canonical path to unowned-work claiming.
- The **migration banner** explicitly mentions `[ Show all ]` and the `/active?operated_by=` URL during Phase 0 — that link surfaces both Matt-owned and unknown rows in one place.
- The **dibs rule** (T-R3) makes "claiming" an unowned bead happen by side-effect: a user who comments on, edits, or otherwise modifies an unowned bead becomes its `operated_by`, and on the next visit it shows up in their default-me page. The viewer doesn't need to do anything special; the transitivity layer handles the "I touched it, now it's mine" semantics.

### 4.5 What we are *not* deciding

- We are not adding a new chip state for `me OR null`. V-R2's chip vocabulary stays as drafted; no new alias.
- We are not adding a "include unowned" toggle to `/active`. The `[ Unowned ]` chip button is already a discoverable one-click switch.
- We are not changing `?operated_by=null` semantics. It stays "operated_by IS NULL" exactly (R8 §5.2; V-R2 §1.1).
- We are not auto-claiming unowned beads on viewer-side mere-view. Dibs is a write-time rule (T-R3 §3 "modifying = operating"); merely reading `/active` does not operate anything. This is correct and we are not relitigating it.

## 5. Decision summary (table form)

| Decision                                              | V-R4 contract                                                                              | Locked here? | Forwarded to                |
|-------------------------------------------------------|--------------------------------------------------------------------------------------------|--------------|------------------------------|
| Default-on acceptable in Phase 0 with skewed corpus   | Yes — chip + migration banner together make it honest                                      | yes          | implementation epic          |
| Phase boundary trigger for default-me's *behavior*    | None — default behavior is invariant across phases                                         | yes          | —                            |
| Migration banner sunset trigger                       | **Config flag** (config bead `viewer.show_migration_banner`) **with calendar backstop** (`2026-08-01`) | yes | V-R3 / impl epic             |
| Banner sunset by cardinality threshold                | **Rejected** (gameable; hand-wave N; hot-path cost)                                        | yes          | —                            |
| Banner sunset by calendar-only                        | **Rejected as primary**, accepted as backstop                                              | yes          | —                            |
| Default-me includes unowned beads (`operated_by IS NULL`) | **No**. Unknowns reachable only via `?operated_by=null`.                               | yes          | —                            |
| Chip path to unowned                                  | `[ Unowned ]` button in V-R2 §4.2 default-applied chip state                               | (V-R2)       | —                            |
| Auto-claim on view                                    | **No**. Dibs is a write-time rule (T-R3); mere viewing does not operate.                   | yes (cross-ref T-R3) | —                    |
| Phase 0 verification mechanism                        | Daemon-side filter cohort (`OperatedBy=oidc:<email>`); CLI proxy is `created_by` (suggestive only) | yes  | T-R1 follow-up if a true `kd list --operated-by` flag is wanted |
| Banner copy approval                                  | V-R2 §5.2 draft is the starting point; final copy lands in the implementation epic         | (V-R2)       | impl epic + operator review  |
| Existing dismissal cookies on trigger fire            | Leave them; 1-year TTL is sufficient                                                       | yes          | —                            |

## 6. Open questions surfaced (forwarded to decision checkpoint or impl epic)

1. **Backstop date specifics (impl epic).** The recommended `2026-08-01` is ~three months from V-R2's draft. The implementation epic confirms or shifts the date based on the actual transitivity epic delivery schedule. Locking 2026-08-01 here is a *recommendation*, not a binding date.
2. **`gardenio-config` bead schema for the flag (impl epic).** What is the exact field path? Is it `viewer.show_migration_banner` or `beads_viewer.migration_banner.enabled`? The viewer's existing config-bead consumer (T-R3-style polling) already has a schema; this is a small addition, not a redesign.
3. **Emit a deprecation log when the backstop fires (impl epic).** When the calendar backstop kicks in, log "migration banner backstop reached on $date — flag was never flipped" exactly once on viewer startup. Cheap; gives operators a useful nudge to clean up the now-redundant config-bead field.
4. **Operator-class banner suppression for sentinels (impl epic).** A non-Matt human logged in via OIDC sees the banner. A `static` bearer-token or anonymous viewer does not (V-R2 §6 already covers this). What about an OIDC viewer whose email *does* resolve to a sentinel-class operator (`system:`)? This is a hypothetical today (no such viewers exist), but should be written down: same rule as anonymous — no banner, no default-me, sign-in CTA in the chip is moot. Forwarded to impl epic to encode the case explicitly.
5. **A real `kd list --operated-by <id>` CLI flag (T-R1 follow-up).** The `kd list` JSON does not surface `operated_by` today (§1.2). A follow-up would expose it as a column and as a server-side filter, which would make the Phase 0 cardinality check trivially auditable from the CLI for future migrations of the same shape. Not a V-R4 deliverable, but a natural T-R1 follow-on bead.
6. **Telemetry on banner dismissal (impl epic).** If the banner cookie is set on a meaningful number of viewers per week, that is signal that the banner is doing its job. If it's set near zero, that is signal that non-Matt users are not actually visiting, and the banner is theatre. Adding a counter is a small implementation question; deferred.

## 7. Cross-references

- This bead: ka-0000008030 (V-R4)
- Parent epic: ka-0000002754
- V-R1 inventory: `gardenio/docs/research/viewer-default-operated-by-r1-inventory.md` (ka-0000000145, merged)
- V-R2 UX: `gardenio/docs/research/viewer-default-operated-by-r2-ux.md` (ka-0000003485, merged) — see §5 for the migration banner contract this doc closes.
- V-R3 server: `gardenio/docs/research/viewer-default-operated-by-r3-server.md` (ka-0000001699, merged)
- T-R1 inventory: `gardenio/docs/research/operated-by-transitivity-r1-inventory.md` (ka-0000005258)
- T-R2 propagation: `gardenio/docs/research/operated-by-transitivity-r2-propagation.md` (ka-0000000973, MR !2014)
- T-R3 dibs: `gardenio/docs/research/operated-by-transitivity-r3-dibs.md` (ka-0000006715)
- T-R4 audit: `gardenio/docs/research/operated-by-transitivity-r4-audit.md` (ka-0000001481)
- R8 prior art: `docs/research/human-attribution-ux.md`
- Code touchpoints (read-only references): `gardenio/controller/cmd/beads-viewer/{operator_chip.go, active.go}`
- Decision checkpoint: filed by epic owner once V-R1 → V-R4 land as merged docs.
