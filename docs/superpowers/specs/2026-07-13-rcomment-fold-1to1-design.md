# R-COMMENT′ fold into the 1:1 GBFSM model — design

> **⛔ DE-SCOPED 2026-07-16 — plan ka-0000002637 is CLOSED.** Per operator
> directive (ka-0000000112, "we're no longer integrating directly with the jira
> bridge in gardenio… especially this plan") and scope decision ka-0000006158
> ("only de-scoping the parts that touch the actual gardenio jira bridge;
> everything else is very much live"), the **1:1 direct-FSM-write outbound
> reshape** described below is no longer being pursued. This doc is retained for
> history only. The inbound jira-bridge (poller→beads, sync-back), the sibling
> FSM Triage Automation epic **ka-0000000137** (live gardener), and the shared
> outbox lineage (ka-0000002067 et al.) are unaffected — any still-live outbound
> behavior now lives in the monorepo `gfo-outbox` skill, tracked by that lane.

**Date:** 2026-07-13
**Operator:** Matt Baker (slack:U0OPERATOR1)
**Epic:** ka-0000002637 (1:1 gardenio↔FSM reshape) · **Plan-of-record:** bundle ka-0000004087 (narrative doc ka-0000001904)
**Rule-build task:** ka-0000004008 · **Rule-adapt phase:** P4 (ka-0000003716) · **Writer phase:** P2 (ka-0000001491)
**Security substrate inherited:** ka-0000002067

> **REVISION 2026-07-14 (b) — note is OPTIONAL + full-length (B with optional A).**
> The operator asked us to examine the note critically: if the comment were just a
> deterministic pointer, R-LINK already conveys it. Resolution: the internal
> comment is now **opt-in per triage, and when posted carries the FULL analysis**,
> not a short pointer. Mechanics: the field changed from `triage_internal_note`
> (Short text, `customfield_10155`, 255-cap) to **`triage_internal_note_full`
> (`customfield_10156`, a Paragraph/rich-text field, no cap)**; the writer (the
> `gfo-outbox` skill) sets it as ADF to post a full comment (**mode B**), or omits
> it to rely on labels + R-LINK only (**mode A**). The live R-COMMENT′ rule was
> repointed to `customfield_10156` in all three places (trigger field, IF
> condition, comment action) and live-verified (a 770-char note propagated as one
> internal comment on FSM-442, `jsdPublic=false`, Automation actor). The gate
> invariant (j), its self-test, and the `R-COMMENT.json` fixture were updated to
> `customfield_10156` (51/51 green). §3.1's field row and §3.3's trigger field are
> superseded by this banner; every security invariant (§4) is unchanged — the note
> stays internal-only free-text applied by the deputy, now simply optional and
> longer.

> **REVISION 2026-07-14 — write half moved to the monorepo skill.** After the rule
> was built + live-verified, the operator decided the GBFSM outbox *write* is
> monorepo-specific business logic and does not belong in the shared gardenio
> controller. **§3.2 (writer P2 code) and the controller parts of §7 are
> SUPERSEDED:** the fsm-triage agent now upserts the whole Analysis ticket
> (incl. `triage_internal_note`) via the monorepo `gfo-outbox` skill
> (`~/monorepo/.claude/skills/gfo-outbox/SKILL.md`), and the T1–T4 controller
> commits were reverted. **Still valid as built:** the `triage_internal_note`
> field (customfield_10155), the folded R-COMMENT′ rule design (§3.3), the
> security invariants (§4–§5), and the gate/testing (§6, invariant (j) +
> `R-COMMENT.json` fixture). Follow-on: retire the controller's `syncGBFSMTicket`
> outbox writer entirely (separate reviewed change).

## 1. Problem

The 1:1 reshape folded the append-only *intent* tickets into a single GBFSM *Analysis* ticket whose **fields** carry the ops: R-LABEL′, R-VERSION′, and R-LINK were adapted to trigger on the Analysis ticket's create/edit and read `triage_labels` / `triage_version` / `fsm_target_key`. **R-COMMENT was not folded.** The live sandbox rule `019f4018-3240-7ff5-be14-1bc4f487b935` still fires on the **retired** trigger — a separate `Internal-Comment` work item (issue type 10042) being *created* — and posts `triggerIssue.description` onto the FSM target.

This is a genuine gap, and the plan-of-record contradicts itself about the intended end state:

- Rule-build task **ka-0000004008** title: `… R-LINK/R-CANARY; **R-COMMENT retired**`.
- Plan **§3**: the clickable FSM→GBFSM link *"replaces the plain-text internal-comment link-back."*
- Plan **§4 invariant #6** and **§P4**: *"Comment visibility hardcoded INTERNAL rule-side"* / *"same INTERNAL visibility"* — i.e. an internal comment keeps flowing.

Consequence today: a comment added to the reshaped Analysis ticket does **not** propagate (verified 2026-07-13 — a probe comment on GFO-53 produced nothing on FSM-440); an internal note only lands if a separate `Internal-Comment` intent work item is created via the old path (verified: GFO-56 → FSM-440 internal note, `jsdPublic=false`, author "Automation for Jira", R-CANARY silent).

## 2. Decision

**Keep the inline internal note, folded into the single Analysis ticket** (operator decision, 2026-07-13). The link-back comment stays retired (superseded by R-LINK), but the **triage internal note** continues to reach the FSM ticket — now sourced from the one Analysis ticket, not a separate intent. This resolves the contradiction in favor of invariant #6, and both spec lines get reconciled to match (§3's "replaces the *link-back*" is correct and narrow; the *analysis* note is a distinct thing that stays).

**Append, not overwrite (operator refinement, 2026-07-13).** Re-triage that *changes* the note **appends a new** internal note on FSM rather than editing the prior one in place. Because the notes are internal-only, a growing thread is acceptable and is in fact **better for auditability** — the FSM ticket carries the full gardenio-triage history, latest-authoritative. This is chosen over both edit-in-place (needs a fragile `Send web request` PUT inside a security rule) and single-snapshot (goes stale). The append is keyed on **content change**, not on every edit event, so replay and no-op edits do not duplicate (see §3.3).

## 3. Mechanism

### 3.1 Schema (P2)

Add one custom field to the GBFSM *Analysis* issue type:

| Field | Type | Purpose |
|---|---|---|
| `triage_internal_note` | text (multi-line) | The exact text destined for the FSM ticket as an internal note. Distinct from Description (the human-readable hub), so hub content — smart-links, formatting — never bleeds into a customer ticket's comment. |

The writer populates it with the note text plus a **content-keyed marker** — the FSM key and an 8-char hash of the note body, computed in Go:

```
[gardenio-note:FSM-440:a1b2c3d4] 🤖 Gardenio triage — sev-3 / GxP-med; labels + Affects-Version applied. Full current analysis: GBFSM-440.
```

The note may carry as much or as little triage content as the writer chooses; the marker's hash changes iff the note body changes. This is what makes the append idempotent (§3.3): a re-fire with identical content reuses the same marker and is skipped, while a genuine re-triage produces a new marker and a new appended note. The always-current full analysis still lives on the GBFSM Description, one click away via the R-LINK link.

### 3.2 Writer (P2 — `syncGBFSMTicket`)

Populate `triage_internal_note` on create/upsert alongside the existing fields (Description, `triage_labels`, `triage_version`, `fsm_target_key`, `idempotency_key`). No new FSM write path — the field lives on the GBFSM Analysis ticket only. Invariant #2 (create hardcoded to GBFSM project id) unchanged.

### 3.3 Rule R-COMMENT′ (P4 — adapt live `019f4018` in place; do not rebuild)

- **Trigger:** Analysis ticket **created or edited** (same trigger family as R-LABEL′/R-VERSION′) — replaces the "Internal-Comment work-item created" trigger.
- **Conditions (all fail-closed):**
  - `reporter-is-bot` (confused-deputy guard on the open project);
  - `fsm_target_key` (customfield_10148) matches `^FSM-[0-9]+$`;
  - `triage_internal_note` non-empty.
- **Append-once-per-version guard:** branch by JQL to the FSM target; scan its comments; **proceed only if none already contains the current marker** `[gardenio-note:FSM-N:<hash8>]`. Append-if-this-version-absent ⇒ a genuine re-triage (new hash) appends a new note; SSE replay / no-op edit (same hash) is skipped. The prior notes are left in place — the thread is the audit log.
- **Action:** add `triage_internal_note` as an **internal** comment — `jsdPublic=false` **hardcoded rule-side** (never from payload) — executed as the **Automation actor**, never a bot account.
- **Retire:** disable the old Internal-Comment trigger on this rule; retire the `Internal-Comment` issue type (10042) from the GBFSM schema.

### 3.4 Data flow

```
syncGBFSMTicket()  →  ONE Analysis ticket
     fields: Description + triage_labels + triage_version + fsm_target_key
             + idempotency_key + triage_internal_note
                         │
     ┌───────────┬───────────┬───────────┬─────────────┐
  R-LABEL′    R-VERSION′    R-LINK     R-COMMENT′    (R-CANARY watches)
  labels →FSM  version→FSM  link →FSM  internal note →FSM (once, guarded)
```

FSM receives labels, Affects-Version, a clickable link, and — on first triage — one internal note; subsequent re-triage that changes the note appends another, forming an internal-only, append-only triage log (latest-authoritative). All applied by the Automation actor.

## 4. Security invariants (§4 of plan-of-record) — all preserved

1. **Zero bot FSM write** — R-COMMENT′ writes as Automation actor. ✅ (verified live)
2. **Create hardcoded to GBFSM** — writer only adds a field to the GBFSM Analysis ticket. ✅
3. **Allowlist on applied ops** — unchanged for labels/version; the internal note is free-text by design (see §5). ✅
4. **reporter-is-bot on every rule** — R-COMMENT′ keeps it. ✅
5. **GBFSM create/edit restricted to bots+admins** — unchanged. ✅
6. **Comment visibility hardcoded INTERNAL, never from payload** — `jsdPublic=false` pinned rule-side; only the *text* is data. ✅ (this is the load-bearing invariant for the fold)
7. **R-CANARY on direct bot FSM write** — unchanged; deputy-applied note keeps it silent. ✅ (verified live)
8. **GBFSM site-public ≠ customer-visible** — unchanged. ✅
9. **Idempotency** — the content-keyed marker is the comment's idempotency key; append-once-per-version makes SSE replay and no-op edits no-ops, while a genuine content change appends exactly one new note. ✅ (strengthens #9 for the comment leg)

## 5. Explicit non-goal: no allowlist on the note text

Unlike labels/version op_values, `triage_internal_note` is **bot-controlled free-text with no allowlist**. Accepted because the note is **internal-only, non-customer-visible, applied by the deputy** — the worst case is unhelpful text in an internal note, not a disclosure or a privilege path. The allowlist stays scoped to the ops that mutate customer-visible/queryable FSM state (labels, version).

## 6. Testing

- **Rule-JSON gate:** export `R-COMMENT′.json` to `gardenio/tests/e2e/fixtures/outbox-rules/`. `verify-outbox-rules-json.sh` invariant **(c)** is a *corpus-wide* check — **no** rule may carry a PUBLIC/"Share with customer" comment marker (`jsdPublic=true`/`visibility:public`), and "any optional internal note must be INTERNAL." A folded R-COMMENT′ that posts an **internal** note (`jsdPublic=false`) satisfies (c) directly. **Caveat:** that script's header narrative currently says *"R-COMMENT retired"* (it was written toward the retire-branch); this plan must update that narrative to describe the folded R-COMMENT′ so the fixture and its documentation agree.
- **E2E (folds into P5 gate ka-0000000600, alongside T-1to1 / T-upsert):**
  - **positive:** triage an FSM ticket → one internal note on FSM, marker present, `jsdPublic=false`, author = Automation;
  - **idempotency (unchanged note / replay):** re-triage with an unchanged note, or SSE replay → still exactly one note (same-hash marker match → skip);
  - **append (changed note):** re-triage that changes the note → a second note appended, both present, latest last;
  - **negatives:** non-bot reporter → no note; empty `triage_internal_note` → no note; R-CANARY silent throughout.
- **Live sandbox verification:** adapt the rule, drive a triage, confirm exactly-one note + idempotency on re-triage, capture proof, and replace the bundle's R-COMMENT canvas + FSM-internal-comment shots with the folded-model versions.

## 7. Scope & ownership

An **adaptation within** epic ka-0000002637 — not a new epic. Touches:

- **P2 (ka-0000001491):** add `triage_internal_note` custom field + one writer line in `syncGBFSMTicket`. Code, gardenio repo, behind `JIRA_OUTBOX_ENABLED`.
- **P4 (ka-0000003716):** adapt the live rule in the sandbox (operator/browser), export JSON, retire the Internal-Comment issue type.
- **Docs:** reconcile the two contradictory lines in ka-0000001904 and the "R-COMMENT retired" wording in ka-0000004008 to "R-COMMENT′ folded (internal note from the Analysis ticket)."
- **Agent-facing doc:** state the behavior where agents will see it (`gardenio/docs/` outbox/triggers reference and/or an advice bead): *the FSM internal notes are an **append-only** gardenio-triage log — each meaningful re-triage adds a note, the **latest is authoritative**, and the current full analysis lives on the linked GBFSM ticket.* So agents reading an FSM ticket don't mistake the oldest note for current or expect edit-in-place.

## 8. Out of scope

- Prod build (P6) — inherits the folded shape once sandbox P5 is green; no separate design.
- Any change to R-LABEL′/R-VERSION′/R-LINK/R-CANARY beyond leaving them untouched.
- **Edit-in-place upsert** (one note kept always-current via `Send web request` PUT) — rejected: adds credential surface + a fragile step inside a load-bearing security rule, and discards audit history. The append log (§2) gives current-state (latest note) *and* history without any of that.
- **Single-snapshot create-if-absent** (one note, never updated) — rejected: goes stale on re-triage.
- Pruning/capping the append log — not in scope; internal-only notes are cheap, and the thread *is* the audit trail. Revisit only if a real volume problem appears.
