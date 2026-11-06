# GBFSM outbox — arming checklist (Phase-4 prerequisites)

> **⛔ DE-SCOPED 2026-07-16 — plan ka-0000002637 is CLOSED.** Per operator
> directive ka-0000000112 + scope decision ka-0000006158 ("only de-scoping the
> parts that touch the actual gardenio jira bridge; everything else is very much
> live"), the **1:1 direct-FSM-write outbound reshape** this checklist arms is no
> longer being pursued. Do NOT flip `JIRA_OUTBOX_ENABLED` for that plan. Retained
> for history. Inbound jira-bridge and the sibling FSM Triage Automation epic
> ka-0000000137 (live gardener) are unaffected; any live outbound behavior now
> lives in the monorepo `gfo-outbox` skill under that lane.

> Operator-facing checklist to run **before** flipping `JIRA_OUTBOX_ENABLED` on and
> scaffolding the GBFSM **Analysis** issue type + labels/version custom fields
> (epic [ka-0000002637], Phase 4 [ka-0000003716]). Surfaced by the adversarial security
> review of MR !5362 (Phase 2 collapse) and tracked in [ka-0000003401].

## Scope & risk

These items are **GBFSM-side quality**, **not** write-isolation. Every API-level
isolation invariant from [ka-0000002067] holds regardless: the bot has ZERO FSM write,
the create/edit project is hardcoded to the numeric GBFSM id, and `fsm_target_key` is
DATA that can never redirect a write. The **worst case** if an item here is skipped is a
duplicate GBFSM ticket in the internal outbox project (10037) — **never** an FSM write.

So this is not a merge gate; the Phase-2 writer merged inert (toggle off + unconfigured
issue type). It is the gate the operator clears at **arming time**.

## Checklist

### 1. Pin the `fsm_target_key` field TYPE to support exact `=` match

`SearchGBFSM` (the read half of the upsert) queries
`project = 10037 AND cf[10148] = "FSM-N"` using the **exact `=` operator**
(`gardenio/controller/internal/bridge/jira_outbox.go`). This was hardened from the fuzzy
`~` (contains) operator: `~` tokenizes the value and can suffer an index/tokenizer
**miss** that silently returns empty, which would fall through to a second
`CreateGBFSMTicket` — a duplicate GBFSM ticket for one FSM-N.

`=` only works if `customfield_10148` (`fsm_target_key`) is a field type whose searcher
supports the `=` operator — a **short / single-line text** field (or a URL/label field),
**not** a free-text *paragraph* field (whose only searcher is `~`). Note `=` on a text
searcher is *analyzed / case-insensitive*, not byte-literal; the byte-exact guarantee is
the writer's own client-side re-verification of `fsm_target_key`, so `=` is just the
"don't tokenizer-miss a present ticket" half. FSM keys are canonical (e.g. `FSM-408`), so
case-insensitivity is immaterial in practice.

- [ ] Confirm `customfield_10148` in the sandbox GBFSM project (10037) is a
      **single-line text** custom field (Jira admin → Custom fields → `fsm_target_key`).
- [ ] Confirm its **search template** is the *exact/text* searcher, not a free-text one.
- [ ] Smoke it: as gardenio2 (privileged on GBFSM), run the JQL
      `project = 10037 AND cf[10148] = "FSM-408"` in the Jira issue navigator and confirm
      it returns without a JQL error. A `400`/"not searchable with =" means the field type
      is wrong — fix it before arming.
- [ ] Repeat the type-pin when the **prod** GBFSM project is scaffolded at cutover
      ([ka-0000007073]) — prod gets its own project id and field ids.

> Fail-safe if this is missed: a mistyped field makes the `=` search **400**, and the
> caller (`syncGBFSMTicket`) fail-safes — it logs the search error and makes **no write**
> (no create, no edit). So a wrong field type degrades to "no GBFSM ticket written",
> never a silent duplicate. But the point of arming is to make the surface *work*, so pin
> the type up front.

### 2. Assert at-most-one GBFSM ticket per FSM-N

`SearchGBFSM` caps at `maxResults=5 ORDER BY created ASC` and returns the **oldest**
passing candidate, so re-triage converges on one canonical ticket. But the upsert cannot
**self-heal** if duplicates have already accrued for one FSM-N — it only ever edits the
oldest; any extras keep drifting.

The bridge now **warns** when `SearchGBFSM` sees `>1` match for one `fsm_target_key`
(`match_count`, `chosen_key` in the log line — grep the slack-bridge logs for
`multiple GBFSM tickets match`). Treat that warning as the trigger to reconcile.

- [ ] Before arming, run the dedupe JQL as gardenio2 and confirm **zero** FSM-N with more
      than one GBFSM ticket:
      ```
      project = 10037 ORDER BY cf[10148] ASC, created ASC
      ```
      (Group by `fsm_target_key`; any key with >1 row is a duplicate to close/merge.)
- [ ] After arming, watch for the `multiple GBFSM tickets match one fsm_target_key`
      bridge warning; if it fires, close all but the oldest ticket for that FSM-N.
- [ ] (Optional hardening) If duplicates recur, consider adding a Jira-side uniqueness
      guard or an Automation rule that rejects a second create for an existing
      `fsm_target_key`.

### 3. Round-trip the collapsed upsert against a real search field

The unit tests stub `/search/jql` verbatim (they echo a fixed payload regardless of the
JQL). Two extra guards close that gap so the create's field wiring and the search's
field/operator wiring cannot silently drift apart:

- **In-repo (already committed):** `jira_gbfsm_roundtrip_test.go` drives `handleUpdated`
  against a **stateful** Jira double that stores each create and answers `/search/jql` by
  actually evaluating the `cf[10148] = "…"` predicate against the stored tickets'
  `customfield_10148` — a genuine round-trip, not a canned echo. It proves create →
  re-triage → **edit** converges on one ticket, distinct FSM keys get distinct tickets,
  and the search uses `=` not `~`.
- [ ] **Live (at arming):** against the real sandbox, create one GBFSM Analysis ticket
      for a disposable FSM-N, then re-run the triage for the same FSM-N and confirm the
      bridge **edits** the same GBFSM key (no second ticket). The
      `gardenio/tests/e2e/scripts/test-jira-outbox-positive.sh` rig exercises the operator
      Automation apply; extend/confirm it round-trips the collapsed upsert (not just the
      retired per-op intents) once the Analysis issue type + labels/version fields are
      scaffolded.

## Preview the backfill before arming (dry-run)

Before arming, you can PREVIEW the one-time FSM→GBFSM backfill without writing
anything. Set `JIRA_BACKFILL_DRY_RUN=true` on the jira-bridge (epic
[ka-0000002637], Phase 5a [ka-0000000123]). At startup the bridge enumerates the
in-scope FSM backlog **read-only** and logs, for each FSM ticket, the ONE GBFSM
ticket it *would* upsert (`gbfsm-backfill: WOULD upsert GBFSM ticket …` — grep for
`gbfsm-backfill`), computed by the same writer the live sync uses. It makes **zero**
Jira writes (no create / edit / search), so it is safe to run at any time,
independent of `JIRA_OUTBOX_ENABLED`. The startup line reports whether the surface
it previews is actually armed (`outbox_analysis_type_configured`,
`outbox_writable_now`). The **live** backfill run (dry-run → live flip) is Phase 5
proper ([ka-0000006784]) and is gated on Phase 4 rules + a green E2E pipeline — it is
NOT wired by this toggle.

## R-COMMENT′ — the FSM internal-note append-log (folded rule)

The folded **R-COMMENT′** rule ([ka-0000002637] Phase 4, design spec
`docs/superpowers/specs/2026-07-13-rcomment-fold-1to1-design.md`) mirrors the
Analysis ticket's **optional** `triage_internal_note_full` field
(`customfield_10156`, a rich-text Paragraph field) onto the FSM ticket as an
**internal** comment (`publicComment:false`), applied by the Automation actor —
never the bot. Behaviour worth knowing when you read an FSM ticket:

- **The comment is opt-in per triage.** The writer sets
  `triage_internal_note_full` only when it wants an inline FSM comment; **omitting
  the field posts no comment** (the triage still reaches FSM via labels + the
  R-LINK back-link). When set, it carries the **full analysis** (no 255-char cap —
  it is a Paragraph field), so the comment stands on its own inline on FSM.
- **It is an append-only log, not an edit-in-place.** R-COMMENT′ triggers on the
  field *changing*, so each **meaningful re-triage appends a new** internal note.
  The **latest note (bottom of the thread) is authoritative**; earlier notes are
  history. Same-content re-triage (or SSE replay) does **not** fire, so it never
  duplicates.
- **The canonical analysis also lives on the linked GBFSM ticket** (its
  Description) — follow the FSM→GBFSM link (R-LINK). Don't treat the oldest FSM
  note as current, and don't expect a posted note's text to be rewritten in place.
- **Internal-only, always.** The comment is pinned `publicComment:false`
  rule-side (never from payload); no customer-visible comment is ever created. Gate
  invariant (j) in `verify-outbox-rules-json.sh` machine-checks this (internal-only
  comment, non-bot actor, reporter-is-bot guard, `^FSM-[0-9]+$` target pin).

## Related

- Writer: the monorepo **`gfo-outbox` skill**
  (`~/monorepo/.claude/skills/gfo-outbox/SKILL.md`) — the fsm-triage agent upserts
  the whole Analysis ticket (incl. the optional `triage_internal_note_full`) from
  there. (The former controller writer `jira_outbox.go` / `jira_sync.go`
  `syncGBFSMTicket` path was reverted — see the spec's 2026-07-14 revision banner;
  follow-on retires it entirely.)
- Backfill dry-run: `gardenio/controller/internal/bridge/jira_backfill.go`
  (`GBFSMBackfill`, `JIRA_BACKFILL_DRY_RUN`)
- Frozen ticket schema: [ka-0000002637] Phase-1 design-lock comment
- Scaffold artifact: [ka-0000001322] (GFO project 10037, field ids)
- Plan-of-record: [ka-0000004087]
