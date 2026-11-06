#!/usr/bin/env bash
# verify-outbox-rules-json.sh — FAIL-CLOSED outbox rule-JSON reconciliation gate
#
# Defense-in-depth layer 2 of the OUTBOX security model (epic ka-0000002067,
# gate bead gd-dq6raBnxLV, rule build ka-0000004008). The outbox model rests on
# ONE load-bearing assumption: "the exported Jira Automation rule JSON, reviewed
# in-repo, IS the security-critical source of truth." Today that review is
# manual — the operator pastes the Export-JSON into the repo and we HOPE a human
# reviewer catches drift. This module converts that manual-review risk into a
# machine-enforced, FAIL-CLOSED reconciliation gate.
#
# ── The 1:1 reshape (Phase 4, ka-0000003716 / epic ka-0000002637) ──────
#
# Phase 2 (ka-0000001491) COLLAPSED the writer: instead of a fan-out of per-op
# intent tickets (Add-Label / Set-Version / Internal-Comment as separate issues),
# gardenio now upserts ONE GBFSM "Analysis" ticket per FSM ticket carrying every op
# as FIELDS — the N triage labels in a labels field, the recommended
# Affects-Version in a version field, the analysis in the Description, and
# fsm_target_key (customfield_10148) as the authoritative link + upsert key. Phase 4
# adapts the operator-owned Automation rules to read MULTIPLE ops from that ONE
# ticket. This gate reconciles the NEW single-ticket rule shape:
#
#   R-LABEL   : Analysis labels field → append EACH allowlisted label to FSM
#               (the op_value allowlist ALLOWLIST_RE is enforced element-wise
#               rule-side BEFORE apply — invariant #3).
#   R-VERSION : Analysis version field → ensure-then-set FSM Affects-Version.
#   R-LINK    : Analysis fsm_target_key → clickable FSM→GBFSM back-link on FSM-N
#               (the Phase-3 FSM half; issue-link / web-link field metadata, so it
#               fires NO customer email). REPLACES the retired plain-text
#               Internal-Comment link-back.
#   R-COMMENT′: Analysis triage_internal_note_full field (customfield_10156) → FSM
#               internal note, append-log (FOLDED 1:1, design spec
#               docs/superpowers/specs/2026-07-13-rcomment-fold-1to1-design.md,
#               epic ka-0000002637). Fires on
#               jira.issue.field.changed(triage_internal_note_full); posts
#               {{triggerIssue.customfield_10156}} to the fsm_target_key ticket via
#               jira.issue.comment with publicComment:false — ALWAYS internal,
#               never customer-facing. R-COMMENT′ is a COMMENT rule, not a
#               field-apply rule: like R-LINK it is EXEMPT from R-LABEL's op_value
#               allowlist (a) and the fail-on-apply-error sinks (f) — a comment-add
#               has no Synced/Failed apply-error path to gate.
#   R-CANARY  : alerts on ANY direct bot-account FSM write (unchanged; must never
#               fire = live structural proof).
#
# R-COMMENT′ supersedes the earlier "R-COMMENT retires" design: rather than leaving
# the internal note un-reachable from FSM (relying solely on the R-LINK back-link to
# the GBFSM Analysis ticket), the comment intent is FOLDED BACK IN as its own rule
# reading a dedicated triage_internal_note_full field on the SAME single Analysis ticket
# — not a revived separate per-op Internal-Comment ticket. Its invariants (j, below)
# pin: the comment stays INTERNAL (publicComment:false, never a public marker), the
# Automation actor is never a bot, the reporter-is-bot confused-deputy guard is
# present, and fsm_target_key is regex-pinned. The corpus-wide no-public-comment
# check (c) continues to enforce that NO rule — R-COMMENT′ included — can leak a
# customer-facing comment.
#
# ── Why this exists (the drift it defends) ────────────────────────────
#
# Open P0 escalation gd-YdG7ap12oB exists precisely because a live rule may carry
# the WRONG, no-'triaged-'-infix op_value allowlist regex
# (`^(gardenio-triaged|gardenio-sev-[0-3]|gardenio-gxp-[a-z0-9-]+)$`). Under exact-
# match semantics that regex REJECTS every real gardenio-triaged-sev-N /
# gardenio-triaged-gxp-* label the bridge emitter produces (isTriageLabel(),
# gardenio/controller/internal/bridge/jira_sync.go), silently dead-lettering
# EVERY severity/GxP classification. Live smoke (T1/T2) did NOT catch it — smoke
# used only the bare `gardenio-triaged` sentinel, which matches BOTH the correct and
# the drifted regex. The bug is invisible until a canonical `gardenio-triaged-sev-N`
# label is created and observed to Sync. Gardeners field-i7pu (2026-07-07 20:54) and
# field-h9k0 (2026-07-08 08:21) both mandated this gate exist BEFORE a prod export.
#
# ── The FAIL-CLOSED contract (load-bearing) ───────────────────────────
#
# Unlike the sibling test-jira-outbox-negative.sh — which SKIPS CLEAN (green)
# when its live sandbox rig is absent — this gate does the OPPOSITE when its
# artifact is absent: it FAILS. If the fixtures dir
# `gardenio/tests/e2e/fixtures/outbox-rules/` is EMPTY (README only) the gate
# exits NON-ZERO — a RED "awaiting operator export" signal, NOT a skip: while the
# dir stays empty the gate stays red until the operator commits a genuine
# `Actions → Export JSON` (per-rule, in Global automation) for
# R-LABEL / R-VERSION / R-LINK / R-CANARY. That operator export LANDED 2026-07-11:
# the four real rule JSONs (R-LABEL/R-VERSION/R-LINK/R-CANARY) are now committed
# and the gate reconciles GREEN against them. R-COMMENT.json (the folded R-COMMENT′
# internal-note rule, design spec
# docs/superpowers/specs/2026-07-13-rcomment-fold-1to1-design.md, epic
# ka-0000002637) LANDED 2026-07-13 as a fifth committed fixture. The fail-closed
# contract above is UNCHANGED — it is what would redden the gate again if the
# artifact were ever removed. See the fixtures README.
#
# It is registered as a HASH-ASSIGNED full-suite module (shard_for() fallback),
# NOT part of the fast main-push smoke subset (SMOKE_MODULES in run-shard.sh).
# So it gates the SCHEDULED / full E2E suite (holding it red whenever the export
# is absent) WITHOUT blocking main-push smoke or per-MR pipelines.
#
# ── What it asserts when the artifact IS present ──────────────────────
#
# All invariants below are ALREADY PINNED IN-REPO — this gate reconciles the
# exported JSON against them; it does NOT re-derive or invent them. Because the
# Jira Automation Export-JSON schema is not yet pinned in-repo, the assertions
# are TOLERANT lookups over the exported structure rather than a brittle exact-
# shape schema. Where a value-vs-key distinction matters for security (the
# op_value allowlist and its drift), the lookup scans JSON string LEAF VALUES via
# `jq '.. | strings'` — so a regex in a prose/description field cannot be
# mistaken for an enforced condition value (adversarial-review CRITICAL-1).
# Coarser presence signals (accountId, project-id, FSM scope) use whole-corpus
# literal greps. Each invariant's residual tolerant-matching limit is documented
# at its check. When the Export-JSON schema is later pinned, the lookups can be
# tightened to exact key paths.
#
#   (a) op_value allowlist  : ALLOWLIST_RE (the tight, 'triaged-'-infix-carrying
#                             allowlist, verbatim from the shared shape library
#                             lib-outbox-shape.sh) is PRESENT in the R-LABEL rule as a
#                             string leaf value, AND the no-'triaged-'-infix DRIFT
#                             literals (`gardenio-sev-`, `gardenio-gxp-`) are ABSENT
#                             everywhere (gd-YdG7ap12oB). NOTE (1:1 reshape): in the
#                             collapsed shape the allowlist is applied ELEMENT-WISE
#                             over the multi-value labels field via a smart-value
#                             expression, e.g.
#                             `{{issue.customfield_LABELS.match("<ALLOWLIST_RE>")…}}`,
#                             so ALLOWLIST_RE appears as a SUBSTRING of a condition/
#                             action leaf value (not a whole standalone leaf, as it
#                             did in the per-op `matches ALLOWLIST_RE` condition).
#                             (a) therefore accepts a substring-within-a-leaf match
#                             (grep -F, which also matches the whole-leaf per-op
#                             form), scoped to the R-LABEL file's leaf VALUES only
#                             (jq '.. | strings' — never keys/prose). Residual
#                             limit: a human reviewer eyeballs that the regex sits in
#                             the label apply/guard, not a comment body.
#   (b) R-CANARY bot ids    : the R-CANARY rule carries BOTH bot accountIds
#                             (gardenio@ 712020:66bb0993-fdcf-4652-8941-d4ac9a81b093
#                             + gardenio2 712020:a2f3e121-ea2e-48d1-aa6e-241713778a2b)
#                             — the direct-FSM-write alert must recognize both.
#   (c) no public comment   : NO rule carries a PUBLIC / "Share with customer"
#                             comment marker (jsdPublic=true / visibility:public /
#                             sd.public.comment.public) ANYWHERE in the corpus — the
#                             customer-notification-safety guarantee (invariant #6).
#                             R-COMMENT′ (folded 1:1, ka-0000002637) posts the
#                             Analysis triage_internal_note_full (customfield_10156) to
#                             FSM as a comment with publicComment:false — ALWAYS
#                             internal, never customer-facing; R-LINK posts a LINK
#                             instead (field metadata, no visibility concept, no
#                             customer email) for the back-reference. Any note
#                             posted to FSM (via R-COMMENT′ or otherwise) must stay
#                             INTERNAL, so ANY public marker fails the gate closed —
#                             invariant (j) below additionally pins R-COMMENT′'s own
#                             publicComment:false leaf directly, file-scoped.
#   (d) reporter-is-bot     : each apply rule (R-LABEL / R-VERSION / R-LINK)
#       + POLARITY            carries the reporter-is-bot CONDITION with the correct
#                             POLARITY — the v4 confused-deputy guard (T11, ka-0000004008
#                             delta 2026-07-08 23:32) for the site-public GBFSM project.
#                             The real Export-JSON schema is now PINNED in-repo
#                             (gd-zKkSOkK6df): the guard is a jira.user.condition whose
#                             value.conditions[] carries { field:reporter, check:USER_IS,
#                             criteria:[…both bot ids…] }. So (d) no longer merely greps
#                             both ids — it asserts the reporter condition's check is the
#                             POSITIVE $REPORTER_POS_CHECK (USER_IS) AND references BOTH
#                             bots, and FAILS CLOSED on any non-positive check. The
#                             dangerous inversion (check==USER_IS_NOT — "reporter is NOT a
#                             bot", same ids) is the confused-deputy guard turned inside
#                             out: it would propagate an UNPRIVILEGED reporter's Analysis
#                             ticket to FSM while BLOCKING the legitimate bot-reportered
#                             one. The old both-ids-present grep false-PASSED that
#                             inversion (adversarial-review HIGH-1); the polarity assertion
#                             closes it. RESIDUAL a human eyeballs on JSON commit: that the
#                             condition sits in the rule's active if-block guard (not a
#                             disabled/dangling component).
#   (e) scope {GFO,FSM}     : the rule SCOPE names GFO project id 10037 (source/
#       + no phantom          trigger side) AND the REAL FSM project id 10038 (apply/
#                             target side), AND the phantom id 10036 appears NOWHERE
#                             in the corpus. This is the gd-l0ffwDFvQ8 false-Synced
#                             defense: the prod defect scoped the apply rules {GFO
#                             10037, PHANTOM 10036} with FSM 10038 ABSENT — 10036 does
#                             NOT exist (the Jira scope UI rendered it 'Unavailable
#                             value') — so the JQL branch key={{customfield_10148}}=
#                             FSM-N found no in-scope target and every apply silently
#                             no-oped while the intent STILL transitioned Synced. The
#                             bare literal 'FSM' (a rule NAME / JQL branch / target
#                             regex) is NOT sufficient — it false-passes on the broken
#                             scope; only the real id 10038 proves the scope resolves.
#                             (e) also pins R-VERSION's jira.version.create target
#                             ('Space') to FSM (Matt reported that action was
#                             INDEPENDENTLY fat-fingered to 10036). CONSERVATIVE
#                             FAIL-CLOSED (see the '(e) ACCEPTED TOLERANT-MATCHING
#                             LIMIT' block): PASSES only on an unambiguously-correct
#                             {GFO 10037, FSM 10038} scope with no 10036; a GFO-only /
#                             phantom-scoped / no-10038 export stays RED.
#   (f) fail-on-apply-error : each FIELD-APPLY rule (R-LABEL / R-VERSION)
#                             transitions the Analysis ticket to FAILED on the apply
#                             action's ERROR path and does NOT unconditionally flip
#                             Pending->Synced regardless of apply outcome — the
#                             STRUCTURAL fix demanded by P0 gd-l0ffwDFvQ8 / bug
#                             gd-d7U7jd6cyH ("ADD AN ERROR BRANCH … so Synced can
#                             NEVER again be a false-success signal"). R-LINK is
#                             EXEMPT: an issue/web link is idempotent field metadata
#                             with no Synced/Failed transition and no "Synced !=
#                             applied" risk. CONSERVATIVE FAIL-CLOSED (see the '(f)
#                             ACCEPTED TOLERANT-MATCHING LIMIT' block): a rule PASSES
#                             (f) only when it has ≥2 Failed sinks and ≥1 Synced sink
#                             (validation-reject Failed + a SECOND apply-error Failed,
#                             vs the single happy-path Synced). The broken shape
#                             (1 Failed / 1 Synced) stays RED; an id-only export with
#                             no status names stays RED.
#   (g) R-LINK present      : the R-LINK rule EXISTS, references fsm_target_key
#                             (customfield_10148) + FSM, and carries a link action
#                             (issue-link / web-link) — so the FSM→GBFSM clickable
#                             back-link (Phase 3 ka-0000001070) is machine-verified
#                             present, applied by the Automation actor (never a bot),
#                             as field metadata (no customer email).
#   (h) target-key repoint  : each apply rule (R-LABEL / R-VERSION / R-LINK) PINS
#       defense              the fsm_target_key regex FSM_TARGET_RE (^FSM-[0-9]+$) as
#                             a MATCHES/comparison condition VALUE on customfield_10148
#                             — the T7 REPOINT DEFENSE (source of truth: sibling
#                             test-jira-outbox-negative.sh T7 case). Without
#                             it a crafted repoint of customfield_10148 to an arbitrary
#                             / cross-project key would be applied to FSM by the
#                             Automation actor. Invariant (e) only greps the bare
#                             literal 'FSM' corpus-wide, which a rule NAME ('GBFSM
#                             R-LABEL') or the JQL branch trivially satisfies — it does
#                             NOT prove the per-rule target constraint. (h) closes that
#                             hole: it scans the rule file's string LEAF VALUES
#                             (jq '.. | strings', like (a)) for the pinned regex literal
#                             — schema-tolerant grep -F, fail-closed if any apply rule
#                             lacks it. ACCEPTED TOLERANT-MATCHING LIMIT (like (a)): the
#                             leaf-scan proves the pinned regex is a condition/apply
#                             VALUE (not a key), but until the Export-JSON schema is
#                             pinned it cannot prove that leaf is specifically the
#                             `second` of a MATCHES condition against customfield_10148
#                             (vs a comment/prose body) — a human reviewer eyeballs that
#                             on JSON commit.
#   (i) actor never a bot   : each rule file's Automation ACTOR (.rules[].actor.value —
#       (confused-deputy /    the identity the rule RUNS AS) is NON-EMPTY and is NEITHER
#        outbox-isolation)    gardenio bot accountId (BOT_UUID_GARDENIO 66bb0993-… /
#                             BOT_UUID_GARDENIO2 a2f3e121-…). The whole outbox model rests
#                             on apply happening as the dedicated Automation actor, NEVER
#                             as a bot: the narrative asserts this throughout (e.g. "by the
#                             Automation actor (never a bot)") but nothing machine-checked
#                             it. If a rule ran AS a bot, its edits would write FSM under a
#                             bot identity (outbox-isolation break) AND R-CANARY — which
#                             alerts on ANY bot-account FSM write — would fire on the
#                             outbox's own applies (confused deputy). (i) FAILS CLOSED if
#                             actor.value is missing/empty OR contains either bot UUID
#                             (bare-UUID substring match, exactly like (h)/(b)). All four
#                             real fixtures share the single non-bot actor
#                             557058:f58131cb-… (type ACCOUNT_ID). ACCEPTED
#                             TOLERANT-MATCHING LIMIT: (i) is a DENYLIST of the known bot
#                             ids, not an allowlist of one blessed actor — it proves the
#                             actor is not a KNOWN bot, but a human reviewer eyeballs on
#                             JSON commit that the actor is the intended dedicated
#                             Automation service account (not some other privileged human).
#
# ── Always-on self-consistency (non-vacuous even with no artifact) ────
#
# Before the artifact gate, FIVE self-tests run.
#   1. self_test_allowlist  — asserts this script's literal (a) equals the sibling
#      source-of-truth value in lib-outbox-shape.sh (cross-file drift guard, extracted
#      by sourcing the library; gd-9fKNfehdbd), accepts the full canonical emitter
#      vocabulary, and rejects the historical no-infix drift shape (`gardenio-sev-3`).
#   2. self_test_apply_error — feeds the (f) discriminator a synthetic BROKEN-shape
#      rule (1 validation-else Failed + 1 unconditional Synced) and a synthetic
#      CORRECTED-shape rule (the added apply-error → Failed branch), proving the
#      former stays RED and the latter can PASS.
#   3. self_test_scope      — feeds the (e) scope_ok discriminator a synthetic BROKEN
#      scope corpus (the exact prod defect {GFO 10037, PHANTOM 10036}, FSM 10038 ABSENT)
#      and a synthetic CORRECTED scope corpus ({GFO 10037, FSM 10038}, no phantom),
#      proving the former stays RED and the latter can PASS — mirroring self_test_apply
#      _error on the same shared clause helpers the live check uses (gd-l0ffwDFvQ8). It
#      ALSO proves the whole-number-token anchoring: a correct scope that merely cites a
#      customfield whose id EMBEDS the phantom still PASSES (no substring false-fail), and
#      a broken scope where FSM 10038 appears only inside a longer customfield number
#      stays RED (no substring false-pass).
#   4. self_test_reporter_polarity — feeds the (d) reporter_is_bot_verdict discriminator
#      a synthetic POSITIVE reporter condition (check=USER_IS, both bot ids → OK) and the
#      INVERTED one (check=USER_IS_NOT, same ids → BAD_POLARITY), proving the former passes
#      and the latter reddens — the confused-deputy-inside-out hole the old both-ids grep
#      missed (gd-zKkSOkK6df). Also covers the legacy flat shape, a single-bot positive
#      condition (MISSING_BOTH), a no-reporter-condition file (NONE), and a SPLIT positive
#      shape (two OR'd single-id conditions → OK, proving the both-ids union does not
#      false-fail a faithful alternate form), on the SAME verdict fn the live (d) check calls.
#   5. self_test_newshape   — writes a synthetic, FAITHFUL single-Analysis-ticket
#      export (R-LABEL/R-VERSION/R-LINK/R-COMMENT′/R-CANARY, new shape) to a PRIVATE
#      mktemp dir and runs THIS SCRIPT recursively against it (OUTBOX_SELFTEST_INNER=1 +
#      OUTBOX_RULES_FIXTURES_DIR override), asserting the whole reconciliation goes
#      GREEN — then injects FIVE drifts and asserts each reddens: the gd-YdG7ap12oB
#      no-infix allowlist drift into R-LABEL (invariant (a) catch), the removal of
#      the ^FSM-[0-9]+$ target-key regex from ONE apply rule (invariant (h) catch, the
#      T7 repoint hole), a scope repoint from FSM 10038 to the phantom 10036 across
#      every rule (invariant (e) catch, the gd-l0ffwDFvQ8 false-Synced defect), an
#      INVERTED reporter condition (check→USER_IS_NOT, both bot ids kept) in ONE apply
#      rule (invariant (d) catch, the gd-zKkSOkK6df polarity hole), and an apply rule
#      whose Automation actor.value is repointed to a gardenio BOT accountId in ONE apply
#      rule (invariant (i) catch, the gd-RigNkSZKE6 confused-deputy / outbox-isolation
#      hole). This proves the (a)-(i) assertions accept a faithful new-shape export and
#      reject a drifted one END-TO-END through the live check, using its OWN synthetic
#      fixtures and WITHOUT touching the real fail-closed fixtures dir.
# All five PASS today; they keep the module meaningful and catch a regression in the
# pinned invariants independently of the fixtures dir — they still run (and hold the
# line) even if the dir were empty. With the operator export now committed, the
# artifact reconciliation gate below runs GREEN against the four real rule JSONs; an
# empty fixtures dir would still redden the overall run (the fail-closed contract).
#
# ── Hermetic ──────────────────────────────────────────────────────────
#
# Pure repo shell: filesystem + jq + grep (+ python3 only in a self-test text edit).
# NO network, NO kubectl, NO kd server, NO Jira admin creds, NO attempt to read SM
# gardenio/jira-sandbox-api-token (agent SA is AccessDenied — verified). run-shard.sh's
# run_module does a tunnel preflight before invoking us, but this script never touches it.

set -euo pipefail

# ── Path resolution (robust to CWD) ───────────────────────────────
# run-shard.sh invokes us from the repo root, but resolve relative to our own
# location so the gate also works when run standalone from anywhere.
SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
FIXTURES_DIR="${OUTBOX_RULES_FIXTURES_DIR:-$SCRIPT_DIR/../fixtures/outbox-rules}"
# lib-outbox-shape.sh is the sibling SOURCE OF TRUTH for ALLOWLIST_RE (the shared 1:1
# shape library sourced by test-jira-outbox-{positive,negative}.sh). The value used to
# live as a top-level `ALLOWLIST_RE='…'` line in test-jira-outbox-negative.sh but was
# factored into this library (Phase 5c, ka-0000003147); the drift guard in
# self_test_allowlist SOURCES it from here (gd-9fKNfehdbd).
SHAPE_LIB="$SCRIPT_DIR/lib-outbox-shape.sh"
SELF="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)/$(basename "${BASH_SOURCE[0]}")"

# ── ALLOWLIST_RE — the tight op_value allowlist (source of truth) ──
# VERBATIM from lib-outbox-shape.sh (the `: "${ALLOWLIST_RE:=…}"` default; originally
# the field-i7pu drop-in on ka-0000004008, since factored out of
# test-jira-outbox-negative.sh into the shared shape library). This is the CORRECT
# regex; the self-test below sources the library and asserts this literal still equals
# it, so the two files cannot silently diverge.
ALLOWLIST_RE='^(gardenio-triaged|gardenio-triaged-sev-[0-3]|gardenio-triaged-gxp-(none|low|med|high)|sev-[0-3]|gxp-(none|low|med|high))$'

# The no-'triaged-'-infix DRIFT discriminators (gd-YdG7ap12oB). These substrings
# appear in the WRONG regex (`gardenio-sev-[0-3]`, `gardenio-gxp-[a-z0-9-]+`) but
# NEVER in the correct one (which has `gardenio-triaged-sev-` / `-gxp-` and the
# back-compat bare `sev-` / `gxp-`). Their presence anywhere in the exported
# rule set is the drift.
DRIFT_LITERALS=('gardenio-sev-' 'gardenio-gxp-')

# Bot accountIds (ka-0000004008 R-CANARY spec + reporter-is-bot delta).
# The full accountId is "712020:<uuid>". We match on the <uuid> portion because
# R-CANARY's condition FACTORS the shared "712020:" prefix out of the regex
# alternation — the live rule is `{{comment.author.accountId}} matches
# 712020:(66bb0993-…|a2f3e121-…)` (ka-0000004008 R-CANARY build report) — so the
# full "712020:<uuid>" literal does NOT appear per-id there, while the reporter
# IN-lists on the apply rules carry the full form. The UUID is the discriminating
# part present in BOTH shapes; matching it is the tolerant, no-false-positive
# reconciliation (a 36-char UUID cannot plausibly collide). The full accountIds,
# for reference: gardenio@ 712020:66bb0993-… / gardenio2 712020:a2f3e121-…
BOT_UUID_GARDENIO='66bb0993-fdcf-4652-8941-d4ac9a81b093'   # gardenio@
BOT_UUID_GARDENIO2='a2f3e121-ea2e-48d1-aa6e-241713778a2b'  # gardenio2
BOT_ID_PREFIX='712020:'                                   # Atlassian accountId prefix

# The non-bot Automation actor accountId (invariant (i), gd-RigNkSZKE6). All four real
# fixtures set .rules[].actor.value to this single non-bot ACCOUNT_ID today. It is used
# ONLY by write_newshape_fixtures to give the synthetic export a faithful non-bot actor
# (so the (i) live check passes the faithful arm and reddens the bot-actor drift arm).
# The live (i) gate is a DENYLIST — it rejects the bot UUIDs above and never keys on this
# specific value, so a legitimately-different Automation actor accountId still passes.
AUTOMATION_ACTOR_ID='557058:f58131cb-b67d-43c7-b30d-6b58d40bd077'  # non-bot ACCOUNT_ID

# Reporter-is-bot condition POLARITY (invariant (d), gd-zKkSOkK6df). The real
# operator Export-JSON (now pinned in fixtures/outbox-rules/) models the confused-deputy
# guard as a `jira.user.condition` whose `value.conditions[]` carries
# `{ "field": "reporter", "check": "USER_IS", "criteria": [{…bot ids…}] }`. "USER_IS"
# is the POSITIVE sense — "reporter IS one of these bots". The dangerous inversion the
# gate must reject is "USER_IS_NOT" (reporter is NOT a bot), which would let an
# UNPRIVILEGED reporter's Analysis ticket propagate to FSM while a legitimately
# bot-reportered one is BLOCKED — the confused-deputy guard turned inside out. We accept
# ONLY the exact positive check the pinned schema uses; every other check value
# (USER_IS_NOT, a legacy/renamed check, or an absent check) fails the gate closed.
REPORTER_POS_CHECK='USER_IS'

# GFO project id (ka-0000004008: "GFO: project 10037"). The sandbox key is still
# literally GFO; prod GBFSM gets its own id at cutover (ka-0000007073). This is the
# SOURCE/trigger side of the rule scope (the Analysis ticket lives in GFO/GBFSM).
GFO_PROJECT_ID='10037'

# FSM project id — the REAL customer-facing FSM project (operator Matt, gd-l0ffwDFvQ8
# close 2026-07-11). This is the APPLY/target side of the rule scope: the apply rules'
# JQL branch key={{issue.customfield_10148}}=FSM-N can only resolve an IN-SCOPE target
# if the rule scope names FSM 10038. The prod defect scoped the 3 apply rules
# {GFO 10037, PHANTOM 10036} with FSM 10038 ABSENT, so every apply silently no-oped
# against an out-of-scope FSM-N while the intent STILL transitioned Pending->Synced —
# the false-Synced defect. Prod GBFSM/FSM ids are re-pinned at cutover (ka-0000007073).
FSM_PROJECT_ID='10038'

# PHANTOM project id — the NON-EXISTENT project the prod defect's rule scope named in
# FSM's place (10038). 10036 does not exist: the Jira scope UI rendered it as an
# "Unavailable value", and it appears NOWHERE else on the gardenio surface. Its presence
# anywhere in a committed export is the exact gd-l0ffwDFvQ8 false-Synced defect (a scope
# {GFO 10037, PHANTOM 10036}, or a R-VERSION version.create fat-fingered to it) — the
# invariant (e) phantom-absent check fails the gate closed on it.
PHANTOM_PROJECT_ID='10036'

# FSM target-key regex — the T7 REPOINT DEFENSE (source of truth: the sibling
# test-jira-outbox-negative.sh T7 case; also embedded in each apply-rule heredoc in
# this script's own faithful fixtures, write_newshape_fixtures). Each
# apply rule (R-LABEL / R-VERSION / R-LINK) must pin fsm_target_key
# (customfield_10148) with a `MATCHES ^FSM-[0-9]+$` condition, so a crafted repoint
# of customfield_10148 to an arbitrary / cross-project key is refused rule-side
# BEFORE any FSM write (no cross-project write). Not invented here — pinned in-repo.
FSM_TARGET_RE='^FSM-[0-9]+$'

# ── Helpers (mirror test-jira-outbox-negative.sh) ─────────────────
red()   { printf '\033[0;31m%s\033[0m\n' "$*"; }
green() { printf '\033[0;32m%s\033[0m\n' "$*"; }
blue()  { printf '\033[0;34m%s\033[0m\n' "$*"; }
dim()   { printf '\033[2m%s\033[0m\n' "$*"; }

PASS=0
FAIL=0
ERRORS=()

pass() { green "  ✓ $1"; PASS=$((PASS + 1)); }
fail() {
  red "  ✗ $1"
  [ -n "${2:-}" ] && dim "    $2"
  FAIL=$((FAIL + 1))
  ERRORS+=("$1: ${2:-}")
}

# assert_file_contains LABEL FILE FIXED_STRING
assert_file_contains() {
  if grep -qF -- "$3" "$2"; then pass "$1"
  else fail "$1" "expected file $(basename "$2") to contain '$3'"; fi
}

# Rule-class file slots (tolerant identification by apply-action / name literal).
R_LABEL_FILE=""
R_VERSION_FILE=""
R_LINK_FILE=""
R_COMMENT_FILE=""
R_CANARY_FILE=""

# classify_files populates the R_*_FILE slots from the parsed JSON corpus. In the
# 1:1-reshape shape ALL apply rules trigger on issueType.name = Analysis, so the
# per-op issue-type literals (Add-Label / Set-Version / Internal-Comment) are GONE
# — identification keys on the DISCRIMINATING APPLY ACTION each rule carries, which
# any faithful per-rule Export JSON includes and which do NOT cross-contaminate:
#   - R-CANARY : rule name contains 'CANARY', or a comment.author.accountId
#                condition carrying a bot uuid (the FSM-write alert). Classified
#                FIRST so the other slots never grab it (the apply rules carry
#                bot uuids too, in the reporter-is-bot condition, but never
#                'comment.author' and are never named CANARY).
#   - R-COMMENT′: the folded internal-note rule (ka-0000002637) — its TRIGGER reads
#                triage_internal_note_full and its comment ACTION posts
#                customfield_10156 via a jira.issue.comment action. Classified
#                SECOND (right after R-CANARY, before any apply-rule slot below) and
#                `continue`s, so it can NEVER fall through into R-LABEL/R-VERSION/
#                R-LINK. This is the fix for the exact false-classification bug this
#                gate shipped with: every exported rule's top-level Jira
#                rule-metadata carries a bare `"labels":[]` array (unrelated to any
#                labels APPLY), so a discriminator keyed on the bare `"labels"`
#                substring alone would grab R-COMMENT′ into the R-LABEL slot
#                whenever R-COMMENT′'s filename sorts first alphabetically — see the
#                R-LABEL bullet below for the fix on that side too.
#   - R-VERSION: a version apply — a `"versions"` fixVersions add and/or a
#                Create-version action (version.create / fixversion).
#   - R-LINK   : a link apply — an issue-link / web-link / remote-link action.
#   - R-LABEL  : the canonical triage vocabulary (`gardenio-triaged`) unique to the
#                label rule's op_value allowlist. (a) then independently verifies
#                the FULL tight ALLOWLIST_RE is present in this file, so keying on
#                `gardenio-triaged` here is not circular. Deliberately NOT a bare
#                `"labels"` substring match: every exported rule (R-COMMENT′
#                included) carries the unrelated top-level rule-metadata array
#                `"labels":[]`, which would false-match ANY rule and, combined with
#                first-match-wins alphabetical file ordering, silently steal the
#                R-LABEL slot out from under the real label rule.
# A single COMBINED rule (one file carrying several applies) fills every slot it
# matches — accepted; per-rule Export JSON keeps the assertions unambiguous (see
# fixtures README).
classify_files() {
  local f
  for f in "$@"; do
    # R-CANARY first (so the other slots below never capture it).
    if grep -qiF -- 'CANARY' "$f" \
       || { grep -qF -- 'comment.author' "$f" && grep -qF -- "$BOT_UUID_GARDENIO" "$f"; }; then
      R_CANARY_FILE="${R_CANARY_FILE:-$f}"
      continue
    fi
    # R-COMMENT′ second — before any apply-rule slot — so it can never fall through
    # into R-LABEL/R-VERSION/R-LINK. Discriminator: the trigger field
    # (triage_internal_note_full) AND the comment action's source field
    # (customfield_10156) AND the comment action type (jira.issue.comment) must ALL
    # be present — this combination is unique to the folded internal-note rule and
    # does not collide with R-LABEL/R-VERSION (which also use jira.issue.comment for
    # their own Failed-path notes, but never reference triage_internal_note_full or
    # customfield_10156).
    if grep -qF -- 'triage_internal_note_full' "$f" \
       && grep -qF -- 'customfield_10156' "$f" \
       && grep -qF -- 'jira.issue.comment' "$f"; then
      R_COMMENT_FILE="${R_COMMENT_FILE:-$f}"
      continue
    fi
    if grep -qiE -- '"versions"|version\.create|create version|fixversion' "$f"; then
      R_VERSION_FILE="${R_VERSION_FILE:-$f}"
    fi
    if grep -qiE -- 'issuelink|issue[._]link|jira\.issue\.link|relates|remotelink|web ?link|link work item' "$f"; then
      R_LINK_FILE="${R_LINK_FILE:-$f}"
    fi
    if grep -qF -- 'gardenio-triaged' "$f"; then
      R_LABEL_FILE="${R_LABEL_FILE:-$f}"
    fi
  done
}

# ── Self-consistency invariants (always-on, non-vacuous) ──────────
self_test_allowlist() {
  blue "Self-test: ALLOWLIST_RE consistency (drift guard vs sibling + canonical vocabulary)"

  # (1) Cross-file drift guard: our literal must equal the sibling source-of-truth
  # value in lib-outbox-shape.sh. That value moved OUT of a top-level
  # `ALLOWLIST_RE='…'` line in test-jira-outbox-negative.sh and INTO the shared shape
  # library as a `: "${ALLOWLIST_RE:=…}"` default (Phase 5c, ka-0000003147) — the old
  # sed extractor that pointed at test-jira-outbox-negative.sh therefore silently found
  # nothing and this guard went inert/RED (gd-9fKNfehdbd). We now extract by SOURCING
  # the library in a clean subshell (ALLOWLIST_RE + the load-guard sentinel unset, so
  # the `:=` default fires and no parent-env override leaks in) and reading the value
  # bash itself assigns. Sourcing — rather than re-parsing a fixed assignment syntax —
  # is deliberately robust: it keeps working no matter how the library later expresses
  # the assignment, which is the exact brittleness that broke the old sed guard. If the
  # library is absent (standalone run) this sub-check skips, but this self-test is not
  # vacuous because its sub-checks (2)/(3) below still run.
  if [ -f "$SHAPE_LIB" ]; then
    local sib_re
    # Extract by SOURCING the library in a clean child `bash -c` (ALLOWLIST_RE + the
    # load-guard sentinel unset so the `:=` default fires and no parent-env override
    # leaks in), reading back the value bash itself assigns. The lib path is passed as
    # a positional ($1) — a genuine dynamic source the linter cannot follow — so the
    # child interpreter, not this file, owns the source; that keeps this call site free
    # of SC1090/SC1091/SC2031 while staying robust to any future change in how the
    # library expresses the assignment.
    sib_re="$(bash -c 'unset ALLOWLIST_RE __GARDENIO_LIB_OUTBOX_SHAPE_LOADED; source "$1" >/dev/null 2>&1; printf "%s" "${ALLOWLIST_RE:-}"' _ "$SHAPE_LIB" 2>/dev/null || true)"
    if [ -z "$sib_re" ]; then
      fail "sibling ALLOWLIST_RE extractable" \
        "could not obtain ALLOWLIST_RE by sourcing $(basename "$SHAPE_LIB") — the source-of-truth definition moved again; re-point the drift guard (gd-9fKNfehdbd)"
    elif [ "$sib_re" = "$ALLOWLIST_RE" ]; then
      pass "ALLOWLIST_RE matches sibling source of truth ($(basename "$SHAPE_LIB"))"
    else
      fail "ALLOWLIST_RE matches sibling source of truth" \
        "this script and $(basename "$SHAPE_LIB") have DIVERGED — reconcile the op_value allowlist. ours='$ALLOWLIST_RE' sibling='$sib_re'"
    fi
  else
    dim "  ⤼ sibling shape library $(basename "$SHAPE_LIB") not found beside this script — cross-file drift sub-check skipped (this self-test's sub-checks 2/3 still run)"
  fi

  # (2) The full canonical emitter vocabulary MUST be accepted (field-i7pu guard:
  # a no-infix allowlist would REJECT these and silently dead-letter every
  # classification).
  local good ok=1
  for good in gardenio-triaged \
              gardenio-triaged-sev-0 gardenio-triaged-sev-3 \
              gardenio-triaged-gxp-none gardenio-triaged-gxp-high \
              sev-0 sev-3 gxp-none gxp-med; do
    if ! [[ "$good" =~ $ALLOWLIST_RE ]]; then
      fail "ALLOWLIST_RE accepts canonical label" "'$good' should MATCH"; ok=0
    fi
  done
  [ "$ok" = 1 ] && pass "ALLOWLIST_RE accepts ALL canonical triage labels (the 'triaged-' infix drift guard)"

  # (3) The historical no-infix drift shape MUST be rejected.
  ok=1
  for bad in gardenio-sev-3 gardenio-gxp-high gardenio-sev-9 evil-label ''; do
    if [[ "$bad" =~ $ALLOWLIST_RE ]]; then
      fail "ALLOWLIST_RE rejects drift/non-canonical" "'$bad' should NOT match"; ok=0
    fi
  done
  [ "$ok" = 1 ] && pass "ALLOWLIST_RE rejects the no-infix drift shape (gardenio-sev-3 / gardenio-gxp-*) and junk"
  echo
}

# self_test_apply_error PROVES invariant (f) is sound: a synthetic BROKEN-shape
# rule (1 validation-else Failed + 1 unconditional Synced — the historical live
# shape) must FAIL the (f) predicate, and a synthetic CORRECTED-shape rule (the
# added apply-error → Failed branch) must PASS it. This turns (f)'s soundness from
# an assertion into a hermetic, self-checking proof — mirroring self_test_allowlist.
# It writes the fixtures to a PRIVATE mktemp dir and removes it on return, so it
# NEVER pollutes the real fail-closed fixtures/outbox-rules/ dir. Always-on
# (runs even with no operator artifact), so the module stays non-vacuous.
self_test_apply_error() {
  blue "Self-test: invariant (f) discriminator (broken-shape FAILS, corrected-shape PASSES)"
  local tdir
  tdir="$(mktemp -d "${TMPDIR:-/tmp}/outbox-f-selftest.XXXXXX")" || {
    fail "(f) self-test scratch dir" "mktemp failed — cannot run the (f) soundness self-test"
    echo; return
  }
  # shellcheck disable=SC2064  # expand $tdir now so the trap cleans THIS dir.
  trap "rm -rf '$tdir'" RETURN

  # BROKEN shape: validation-ELSE Failed (present today) + UNCONDITIONAL Synced.
  # A prose comment literally saying 'FAILED' is included to prove it does NOT
  # inflate the Failed-sink count (we count transition ACTIONS, not strings).
  cat > "$tdir/broken.json" <<'BROKEN_JSON'
{ "rules": [ { "name": "GFO R-LABEL (broken shape)", "components": [
  { "component": "CONDITION_BLOCK", "type": "jira.condition.if.block", "children": [
    { "component": "BRANCH", "type": "jira.issue.related",
      "value": { "relatedType": "jql", "jql": "key = {{issue.customfield_10148}}" }, "children": [
      { "component": "ACTION", "type": "jira.issue.edit",
        "value": { "advancedJson": "{\"update\":{\"labels\":[{\"add\":\"{{triggerIssue.customfield_10160}}\"}]}}" } } ] },
    { "component": "ACTION", "type": "jira.issue.transition",
      "value": { "transition": { "id": "51", "name": "Synced" } } } ] },
  { "component": "CONDITION_BLOCK", "type": "jira.condition.else.block", "children": [
    { "component": "ACTION", "type": "jira.issue.comment",
      "value": { "body": "R-LABEL validation FAILED — op_value not in allowlist. status Failed." } },
    { "component": "ACTION", "type": "jira.issue.transition",
      "value": { "transition": { "id": "41", "name": "Failed" } } } ] } ] } ] }
BROKEN_JSON

  # CORRECTED shape: adds an apply-error branch (re-fetch + verify the edit landed)
  # whose error/false arm transitions to Failed — a SECOND Failed sink; Synced now
  # sits on the success arm. This is the P0 gd-l0ffwDFvQ8 structural fix.
  cat > "$tdir/corrected.json" <<'CORRECTED_JSON'
{ "rules": [ { "name": "GFO R-LABEL (corrected shape)", "components": [
  { "component": "CONDITION_BLOCK", "type": "jira.condition.if.block", "children": [
    { "component": "BRANCH", "type": "jira.issue.related",
      "value": { "relatedType": "jql", "jql": "key = {{issue.customfield_10148}}" }, "children": [
      { "component": "ACTION", "type": "jira.issue.edit",
        "value": { "advancedJson": "{\"update\":{\"labels\":[{\"add\":\"{{triggerIssue.customfield_10160}}\"}]}}" } },
      { "component": "ACTION", "type": "jira.issue.refetch", "value": {} },
      { "component": "CONDITION_BLOCK", "type": "jira.condition.if.block",
        "conditions": [ { "component": "CONDITION", "type": "jira.comparison.condition",
          "value": { "first": { "type": "SMART", "value": "{{issue.labels}}" }, "operator": "NOT_CONTAINS",
                     "second": { "type": "SMART", "value": "{{triggerIssue.customfield_10160}}" } } } ], "children": [
        { "component": "ACTION", "type": "jira.issue.comment",
          "value": { "body": "apply to FSM FAILED — label absent after edit (actor lacks EDIT_ISSUES / screen field); marking intent Failed" } },
        { "component": "ACTION", "type": "jira.issue.transition",
          "value": { "transition": { "id": "41", "name": "Failed" } } } ] },
      { "component": "CONDITION_BLOCK", "type": "jira.condition.else.block", "children": [
        { "component": "ACTION", "type": "jira.issue.transition",
          "value": { "transition": { "id": "51", "name": "Synced" } } } ] } ] } ] },
  { "component": "CONDITION_BLOCK", "type": "jira.condition.else.block", "children": [
    { "component": "ACTION", "type": "jira.issue.comment",
      "value": { "body": "R-LABEL validation FAILED — op_value not in allowlist" } },
    { "component": "ACTION", "type": "jira.issue.transition",
      "value": { "transition": { "id": "41", "name": "Failed" } } } ] } ] } ] }
CORRECTED_JSON

  local bc cc bf bs cf cs
  bc="$(transition_sink_counts "$tdir/broken.json")";    bf="${bc%% *}"; bs="${bc##* }"
  cc="$(transition_sink_counts "$tdir/corrected.json")"; cf="${cc%% *}"; cs="${cc##* }"

  if apply_error_ok "$bf" "$bs"; then
    fail "(f) self-test: broken shape stays RED" \
      "the synthetic broken rule (${bf} Failed / ${bs} Synced sink) PASSED (f) — the discriminator would FALSE-PASS the live buggy export; (f) is unsound, do NOT ship"
  else
    pass "(f) rejects the broken shape (${bf} Failed / ${bs} Synced sink → RED; validation-else Failed alone never satisfies (f))"
  fi

  if apply_error_ok "$cf" "$cs"; then
    pass "(f) accepts the corrected shape (${cf} Failed / ${cs} Synced sink → PASS; the apply-error Failed branch is honored)"
  else
    fail "(f) self-test: corrected shape can pass" \
      "the synthetic corrected rule (${cf} Failed / ${cs} Synced sink) did NOT pass (f) — the discriminator would FALSE-FAIL a faithful fixed export (bar 4 violated); loosen (f)"
  fi
  echo
}

# self_test_scope PROVES invariant (e)'s scope discriminator is sound: a synthetic
# BROKEN-scope corpus (the exact prod defect {GFO 10037, PHANTOM 10036} with FSM 10038
# ABSENT, gd-l0ffwDFvQ8) must FAIL scope_ok, and a synthetic CORRECTED-scope corpus
# ({GFO 10037, FSM 10038}, no phantom) must PASS it. This turns (e)'s soundness from an
# assertion into a hermetic, self-checking proof — mirroring self_test_apply_error. It
# feeds heredoc corpus STRINGS directly to the shared scope_ok predicate, which composes
# the SAME clause helpers (gfo_in_scope / fsm_in_scope / phantom_in_scope) the live (e)
# check calls for its three sub-verdicts, so the live check and the self-test cannot
# drift. Always-on (runs even with no operator artifact), so the module stays
# non-vacuous. No filesystem, no fixtures-dir touch.
self_test_scope() {
  blue "Self-test: invariant (e) scope discriminator (broken {10037,10036}/no-10038 FAILS, corrected {10037,10038} PASSES)"

  # BROKEN scope — the exact prod defect: GFO 10037 + PHANTOM 10036, FSM 10038 ABSENT.
  local broken corrected
  broken="$(cat <<BROKEN_SCOPE
{ "rules": [ { "name": "GBFSM R-VERSION (broken scope)",
  "ruleScope": { "resources": [
    "ari:cloud:jira:x:project/$GFO_PROJECT_ID",
    "ari:cloud:jira:x:project/$PHANTOM_PROJECT_ID" ] },
  "components": [ { "type": "jira.version.create",
    "value": { "name": "1.0", "projectKey": "FSM" } } ] } ] }
BROKEN_SCOPE
)"
  # CORRECTED scope — GFO 10037 + the REAL FSM 10038, no phantom.
  corrected="$(cat <<CORRECTED_SCOPE
{ "rules": [ { "name": "GBFSM R-VERSION (corrected scope)",
  "ruleScope": { "resources": [
    "ari:cloud:jira:x:project/$GFO_PROJECT_ID",
    "ari:cloud:jira:x:project/$FSM_PROJECT_ID" ] },
  "components": [ { "type": "jira.version.create",
    "value": { "name": "1.0", "projectKey": "FSM" } } ] } ] }
CORRECTED_SCOPE
)"

  if scope_ok "$broken"; then
    fail "(e) self-test: broken scope stays RED" \
      "the synthetic broken-scope corpus ({GFO $GFO_PROJECT_ID, PHANTOM $PHANTOM_PROJECT_ID}, FSM $FSM_PROJECT_ID absent) PASSED scope_ok — invariant (e) would FALSE-PASS the exact gd-l0ffwDFvQ8 false-Synced defect; (e) is unsound, do NOT ship"
  else
    pass "(e) rejects the broken prod scope ({GFO $GFO_PROJECT_ID, PHANTOM $PHANTOM_PROJECT_ID}, no FSM $FSM_PROJECT_ID → RED; a GFO-only / phantom-scoped export never satisfies (e))"
  fi

  if scope_ok "$corrected"; then
    pass "(e) accepts the corrected scope ({GFO $GFO_PROJECT_ID, FSM $FSM_PROJECT_ID}, no phantom → PASS; the real FSM id is honored)"
  else
    fail "(e) self-test: corrected scope can pass" \
      "the synthetic corrected-scope corpus ({GFO $GFO_PROJECT_ID, FSM $FSM_PROJECT_ID}, no phantom) did NOT pass scope_ok — (e) would FALSE-FAIL a faithful operator export; loosen (e)"
  fi

  # Whole-number-token anchoring proof (against the grep -F substring-collision hazard).
  # A CORRECT scope {GFO 10037, FSM 10038} that merely also references a customfield whose
  # numeric id EMBEDS the phantom 10036 (e.g. customfield_100361) must STILL pass — the
  # phantom clause anchors on 10036 as a whole token, so the embedding does not false-fail
  # a faithful export.
  local corrected_with_collision
  corrected_with_collision="$(cat <<COLLISION_OK
{ "rules": [ { "name": "GBFSM R-VERSION (corrected scope + colliding customfield)",
  "ruleScope": { "resources": [
    "ari:cloud:jira:x:project/$GFO_PROJECT_ID",
    "ari:cloud:jira:x:project/$FSM_PROJECT_ID" ] },
  "components": [ { "type": "jira.comparison.condition",
    "value": { "first": "{{issue.customfield_${PHANTOM_PROJECT_ID}1}}", "operator": "MATCHES", "second": "^FSM-[0-9]+\$" } } ] } ] }
COLLISION_OK
)"
  if scope_ok "$corrected_with_collision"; then
    pass "(e) whole-token anchoring: a correct scope that also cites customfield_${PHANTOM_PROJECT_ID}1 (embeds $PHANTOM_PROJECT_ID) still PASSES (no substring false-fail)"
  else
    fail "(e) self-test: substring collision false-fails a correct scope" \
      "a correct scope {GFO $GFO_PROJECT_ID, FSM $FSM_PROJECT_ID} that merely references customfield_${PHANTOM_PROJECT_ID}1 was REJECTED — the phantom-absent clause is matching $PHANTOM_PROJECT_ID as a substring instead of a whole token; anchor id_token_present"
  fi

  # And a BROKEN scope where the real FSM id 10038 appears ONLY as a substring of a
  # longer customfield number (customfield_${FSM_PROJECT_ID}0) — never as a whole token —
  # must stay RED: the FSM-present clause must not be satisfied by the embedding.
  local broken_fsm_substring
  broken_fsm_substring="$(cat <<COLLISION_BAD
{ "rules": [ { "name": "GBFSM R-VERSION (phantom scope, FSM id only as substring)",
  "ruleScope": { "resources": [
    "ari:cloud:jira:x:project/$GFO_PROJECT_ID",
    "ari:cloud:jira:x:project/$PHANTOM_PROJECT_ID" ] },
  "components": [ { "type": "jira.comparison.condition",
    "value": { "first": "{{issue.customfield_${FSM_PROJECT_ID}0}}", "operator": "EQUALS", "second": "x" } } ] } ] }
COLLISION_BAD
)"
  if scope_ok "$broken_fsm_substring"; then
    fail "(e) self-test: FSM id as substring false-passes a broken scope" \
      "a broken scope {GFO $GFO_PROJECT_ID, PHANTOM $PHANTOM_PROJECT_ID} where FSM $FSM_PROJECT_ID appears ONLY inside customfield_${FSM_PROJECT_ID}0 PASSED — the FSM-present clause is matching $FSM_PROJECT_ID as a substring; anchor id_token_present"
  else
    pass "(e) whole-token anchoring: a broken scope where FSM $FSM_PROJECT_ID appears only inside customfield_${FSM_PROJECT_ID}0 stays RED (substring does not satisfy the FSM-present clause)"
  fi
  echo
}

# self_test_reporter_polarity PROVES invariant (d)'s reporter_is_bot_verdict is sound: a
# synthetic POSITIVE reporter condition (check=$REPORTER_POS_CHECK, both bot ids) must
# yield OK, and the INVERTED one (check=USER_IS_NOT, same ids) must yield BAD_POLARITY —
# the exact confused-deputy-inside-out hole the old both-ids grep missed (adversarial-
# review HIGH-1, now closed by gd-zKkSOkK6df). It feeds heredoc rule STRINGS through a
# private mktemp file to the SAME reporter_is_bot_verdict the live (d) check calls, so the
# live check and the self-test cannot drift. Also covers the legacy flat shape, a
# single-bot positive condition (MISSING_BOTH), and a no-reporter-condition file (NONE).
# Always-on (runs even with no operator artifact), so the module stays non-vacuous.
self_test_reporter_polarity() {
  blue "Self-test: invariant (d) reporter polarity discriminator (USER_IS both → OK, USER_IS_NOT → BAD_POLARITY)"
  local tdir
  tdir="$(mktemp -d "${TMPDIR:-/tmp}/outbox-d-selftest.XXXXXX")" || {
    fail "(d) self-test scratch dir" "mktemp failed — cannot run the (d) polarity soundness self-test"
    echo; return
  }
  # shellcheck disable=SC2064  # expand $tdir now so the trap cleans THIS dir.
  trap "rm -rf '$tdir'" RETURN

  # POSITIVE — the real pinned nested shape: reporter USER_IS one-of {both bots}.
  cat > "$tdir/positive.json" <<POS_JSON
{ "rules": [ { "name": "R (positive reporter-is-bot)", "components": [
  { "component": "CONDITION", "type": "jira.user.condition",
    "value": { "operator": "OR", "conditions": [ { "field": "reporter", "check": "$REPORTER_POS_CHECK",
      "criteria": [ { "type": "ID", "value": "${BOT_ID_PREFIX}${BOT_UUID_GARDENIO}" },
                    { "type": "ID", "value": "${BOT_ID_PREFIX}${BOT_UUID_GARDENIO2}" } ] } ] } } ] } ] }
POS_JSON

  # INVERTED — same ids, but check=USER_IS_NOT (reporter is NOT a bot): the dangerous
  # inversion. The 36-char bot UUIDs are still PRESENT, so the historical both-ids grep
  # would false-PASS this; the polarity verdict must reject it.
  cat > "$tdir/inverted.json" <<NEG_JSON
{ "rules": [ { "name": "R (INVERTED reporter condition)", "components": [
  { "component": "CONDITION", "type": "jira.user.condition",
    "value": { "operator": "OR", "conditions": [ { "field": "reporter", "check": "USER_IS_NOT",
      "criteria": [ { "type": "ID", "value": "${BOT_ID_PREFIX}${BOT_UUID_GARDENIO}" },
                    { "type": "ID", "value": "${BOT_ID_PREFIX}${BOT_UUID_GARDENIO2}" } ] } ] } } ] } ] }
NEG_JSON

  # LEGACY flat positive shape — value.{field,check,value[]} (older synthetic form). The
  # verdict must still recognize it (kept working across schema forms). NOTE: the legacy
  # flat form used check=IS_ONE_OF, which is NOT the pinned positive check — so with the
  # schema now pinned this yields BAD_POLARITY (only the exact $REPORTER_POS_CHECK passes).
  cat > "$tdir/legacy_flat.json" <<FLAT_JSON
{ "rules": [ { "name": "R (legacy flat IS_ONE_OF)", "components": [
  { "component": "CONDITION", "type": "jira.user.condition",
    "value": { "field": "reporter", "check": "IS_ONE_OF",
      "value": [ "${BOT_ID_PREFIX}${BOT_UUID_GARDENIO}", "${BOT_ID_PREFIX}${BOT_UUID_GARDENIO2}" ] } } ] } ] }
FLAT_JSON

  # POSITIVE but only ONE bot id → MISSING_BOTH.
  cat > "$tdir/one_bot.json" <<ONE_JSON
{ "rules": [ { "name": "R (positive, one bot only)", "components": [
  { "component": "CONDITION", "type": "jira.user.condition",
    "value": { "operator": "OR", "conditions": [ { "field": "reporter", "check": "$REPORTER_POS_CHECK",
      "criteria": [ { "type": "ID", "value": "${BOT_ID_PREFIX}${BOT_UUID_GARDENIO}" } ] } ] } } ] } ] }
ONE_JSON

  # NO reporter user-condition at all → NONE.
  cat > "$tdir/none.json" <<NONE_JSON
{ "rules": [ { "name": "R (no reporter condition)", "components": [
  { "component": "CONDITION", "type": "jira.comparison.condition",
    "value": { "first": "{{issue.project.id}}", "operator": "EQUALS", "second": "$GFO_PROJECT_ID" } } ] } ] }
NONE_JSON

  # SPLIT positive: "reporter IS bot1 OR reporter IS bot2" expressed as TWO OR'd
  # single-id conditions[] entries (semantically identical to the pinned two-criteria
  # form). Must be OK — the both-ids requirement is a file-wide union over positive
  # reporter conditions, so this faithful alternate shape is NOT false-failed.
  cat > "$tdir/split_positive.json" <<SPLIT_JSON
{ "rules": [ { "name": "R (positive, split OR of two single-id conditions)", "components": [
  { "component": "CONDITION", "type": "jira.user.condition",
    "value": { "operator": "OR", "conditions": [
      { "field": "reporter", "check": "$REPORTER_POS_CHECK", "criteria": [ { "type": "ID", "value": "${BOT_ID_PREFIX}${BOT_UUID_GARDENIO}" } ] },
      { "field": "reporter", "check": "$REPORTER_POS_CHECK", "criteria": [ { "type": "ID", "value": "${BOT_ID_PREFIX}${BOT_UUID_GARDENIO2}" } ] } ] } } ] } ] }
SPLIT_JSON

  local v
  v="$(reporter_is_bot_verdict "$tdir/positive.json")"
  if [ "$v" = "OK" ]; then
    pass "(d) accepts a POSITIVE reporter condition ($REPORTER_POS_CHECK, both bot ids → OK)"
  else
    fail "(d) self-test: positive polarity passes" \
      "a synthetic positive reporter-is-bot condition ($REPORTER_POS_CHECK, both bot ids) yielded '$v' not OK — the verdict would FALSE-FAIL a faithful operator export; loosen (d)"
  fi

  v="$(reporter_is_bot_verdict "$tdir/inverted.json")"
  case "$v" in
    BAD_POLARITY:*)
      pass "(d) rejects the INVERTED reporter condition (USER_IS_NOT, both bot ids → $v; the confused-deputy-inside-out hole the old both-ids grep missed)" ;;
    *)
      fail "(d) self-test: inverted polarity stays RED" \
        "a synthetic INVERTED reporter condition (USER_IS_NOT, both bot ids present) yielded '$v' not BAD_POLARITY — the verdict would FALSE-PASS an unprivileged reporter being able to drive FSM writes (confused-deputy guard inverted); (d) is unsound, do NOT ship" ;;
  esac

  v="$(reporter_is_bot_verdict "$tdir/legacy_flat.json")"
  case "$v" in
    BAD_POLARITY:*)
      pass "(d) parses the legacy flat shape and rejects its non-pinned IS_ONE_OF check ($v; only the pinned $REPORTER_POS_CHECK passes)" ;;
    *)
      fail "(d) self-test: legacy flat shape parsed" \
        "the legacy flat reporter shape (value.field/check/value[]) yielded '$v' — with the schema pinned it should be BAD_POLARITY (IS_ONE_OF is not $REPORTER_POS_CHECK); the flat-shape parse arm regressed" ;;
  esac

  v="$(reporter_is_bot_verdict "$tdir/one_bot.json")"
  if [ "$v" = "MISSING_BOTH" ]; then
    pass "(d) flags a positive condition that references only ONE bot id (→ MISSING_BOTH)"
  else
    fail "(d) self-test: single-bot condition flagged" \
      "a positive reporter condition referencing only ONE bot id yielded '$v' not MISSING_BOTH — the both-ids requirement regressed"
  fi

  v="$(reporter_is_bot_verdict "$tdir/none.json")"
  if [ "$v" = "NONE" ]; then
    pass "(d) reports NONE when a rule carries no reporter user-condition (caller fails closed)"
  else
    fail "(d) self-test: no-reporter file → NONE" \
      "a rule with no reporter jira.user.condition yielded '$v' not NONE — the presence detection regressed"
  fi

  v="$(reporter_is_bot_verdict "$tdir/split_positive.json")"
  if [ "$v" = "OK" ]; then
    pass "(d) accepts the SPLIT positive shape (two OR'd single-id $REPORTER_POS_CHECK conditions → OK; both-ids is a file-wide union, so a faithful alternate shape is not false-failed)"
  else
    fail "(d) self-test: split positive shape passes" \
      "a faithful 'reporter IS bot1 OR reporter IS bot2' expressed as two OR'd single-id $REPORTER_POS_CHECK conditions yielded '$v' not OK — the both-ids union regressed to a per-condition check and would FALSE-FAIL this legitimate export"
  fi
  echo
}

# write_newshape_fixtures DIR — writes a synthetic, FAITHFUL single-Analysis-ticket
# export (the 1:1-reshape shape) satisfying invariants (a)-(j), used by
# self_test_newshape. Each apply rule triggers on issueType.name = Analysis, hardcodes
# project 10037, carries the reporter-is-bot condition (both bot accountIds), runs AS the
# non-bot Automation actor ($AUTOMATION_ACTOR_ID, so invariant (i) passes), targets
# FSM via ^FSM-[0-9]+$ / a JQL branch, and — for the field-apply rules — carries the
# validation-else Failed + apply-error Failed + happy Synced sinks. R-COMMENT′ (the
# folded internal-note rule, invariant (j)) is included too: it triggers on
# triage_internal_note_full, posts customfield_10156 via jira.issue.comment with
# publicComment:false, and carries the same reporter-is-bot / non-bot-actor /
# fsm_target_key-pin guards as the apply rules. Each rule also
# carries a ruleScope naming the CORRECT project ids ({GFO 10037, FSM 10038}; R-CANARY
# scopes {FSM 10038}) so invariant (e) sees the real FSM id 10038 and no phantom 10036 —
# the ari uuid is a fixed illustrative constant (the gate never keys on it). R-VERSION's
# jira.version.create pins projectKey "FSM". All comments are INTERNAL. This is HERMETIC
# synthetic input, NOT a substitute for the operator export (it never lands in
# fixtures/outbox-rules/). The synthetic customfield ids for the labels/version fields
# (10160/10161) are ILLUSTRATIVE — the gate never keys on them.
write_newshape_fixtures() { # dir
  local d="$1"

  # R-LABEL — Analysis labels field → FSM labels, allowlisted element-wise. The
  # ALLOWLIST_RE is embedded in the guard/apply smart value (substring of a leaf).
  cat > "$d/R-LABEL.json" <<LABEL_JSON
{ "rules": [ { "name": "GBFSM R-LABEL — Analysis labels to FSM (multi-op)",
  "actor": { "type": "ACCOUNT_ID", "value": "$AUTOMATION_ACTOR_ID" },
  "ruleScope": { "resources": [
    "ari:cloud:jira:a1b2c3d4-e5f6-7890-abcd-ef0123456789:project/$GFO_PROJECT_ID",
    "ari:cloud:jira:a1b2c3d4-e5f6-7890-abcd-ef0123456789:project/$FSM_PROJECT_ID" ] },
  "trigger": { "component": "TRIGGER", "type": "jira.issue.event.trigger:created" },
  "components": [
    { "component": "CONDITION", "type": "jira.comparison.condition",
      "value": { "first": "{{issue.issuetype.name}}", "operator": "EQUALS", "second": "Analysis" } },
    { "component": "CONDITION", "type": "jira.comparison.condition",
      "value": { "first": "{{issue.project.id}}", "operator": "EQUALS", "second": "$GFO_PROJECT_ID" } },
    { "component": "CONDITION", "type": "jira.user.condition",
      "value": { "operator": "OR", "conditions": [ { "field": "reporter", "check": "$REPORTER_POS_CHECK",
        "criteria": [ { "type": "ID", "value": "${BOT_ID_PREFIX}${BOT_UUID_GARDENIO}" },
                      { "type": "ID", "value": "${BOT_ID_PREFIX}${BOT_UUID_GARDENIO2}" } ] } ] } },
    { "component": "CONDITION", "type": "jira.comparison.condition",
      "value": { "first": "{{issue.customfield_10148}}", "operator": "MATCHES", "second": "^FSM-[0-9]+\$" } },
    { "component": "CONDITION_BLOCK", "type": "jira.condition.if.block",
      "conditions": [ { "component": "CONDITION", "type": "jira.comparison.condition",
        "value": { "first": "{{issue.customfield_10160.match(\"$ALLOWLIST_RE\").size}}",
                   "operator": "EQUALS", "second": "{{issue.customfield_10160.size}}" } } ],
      "children": [
        { "component": "BRANCH", "type": "jira.issue.related",
          "value": { "relatedType": "jql", "jql": "key = \"{{issue.customfield_10148}}\"" },
          "children": [
            { "component": "ACTION", "type": "jira.issue.edit",
              "value": { "advancedJson": "{\"update\":{\"labels\":[{\"add\":\"{{issue.customfield_10160}}\"}]}}" } },
            { "component": "ACTION", "type": "jira.issue.refetch", "value": {} },
            { "component": "CONDITION_BLOCK", "type": "jira.condition.if.block",
              "conditions": [ { "component": "CONDITION", "type": "jira.comparison.condition",
                "value": { "first": "{{issue.labels}}", "operator": "NOT_CONTAINS", "second": "{{triggerIssue.customfield_10160}}" } } ],
              "children": [
                { "component": "ACTION", "type": "jira.issue.comment",
                  "value": { "body": "apply to FSM FAILED — labels absent after edit; marking Failed", "visibility": { "type": "internal" } } },
                { "component": "ACTION", "type": "jira.issue.transition",
                  "value": { "transition": { "id": "41", "name": "Failed" } } } ] },
            { "component": "CONDITION_BLOCK", "type": "jira.condition.else.block", "children": [
                { "component": "ACTION", "type": "jira.issue.transition",
                  "value": { "transition": { "id": "51", "name": "Synced" } } } ] } ] } ] },
    { "component": "CONDITION_BLOCK", "type": "jira.condition.else.block", "children": [
        { "component": "ACTION", "type": "jira.issue.comment",
          "value": { "body": "R-LABEL validation FAILED — labels not all allowlisted", "visibility": { "type": "internal" } } },
        { "component": "ACTION", "type": "jira.issue.transition",
          "value": { "transition": { "id": "41", "name": "Failed" } } } ] } ] } ] }
LABEL_JSON

  # R-VERSION — Analysis version field → FSM Affects-Version (ensure-then-set).
  cat > "$d/R-VERSION.json" <<VERSION_JSON
{ "rules": [ { "name": "GBFSM R-VERSION — Analysis version to FSM Affects-Version",
  "actor": { "type": "ACCOUNT_ID", "value": "$AUTOMATION_ACTOR_ID" },
  "ruleScope": { "resources": [
    "ari:cloud:jira:a1b2c3d4-e5f6-7890-abcd-ef0123456789:project/$GFO_PROJECT_ID",
    "ari:cloud:jira:a1b2c3d4-e5f6-7890-abcd-ef0123456789:project/$FSM_PROJECT_ID" ] },
  "trigger": { "component": "TRIGGER", "type": "jira.issue.event.trigger:created" },
  "components": [
    { "component": "CONDITION", "type": "jira.comparison.condition",
      "value": { "first": "{{issue.issuetype.name}}", "operator": "EQUALS", "second": "Analysis" } },
    { "component": "CONDITION", "type": "jira.comparison.condition",
      "value": { "first": "{{issue.project.id}}", "operator": "EQUALS", "second": "$GFO_PROJECT_ID" } },
    { "component": "CONDITION", "type": "jira.user.condition",
      "value": { "operator": "OR", "conditions": [ { "field": "reporter", "check": "$REPORTER_POS_CHECK",
        "criteria": [ { "type": "ID", "value": "${BOT_ID_PREFIX}${BOT_UUID_GARDENIO}" },
                      { "type": "ID", "value": "${BOT_ID_PREFIX}${BOT_UUID_GARDENIO2}" } ] } ] } },
    { "component": "CONDITION", "type": "jira.comparison.condition",
      "value": { "first": "{{issue.customfield_10148}}", "operator": "MATCHES", "second": "^FSM-[0-9]+\$" } },
    { "component": "CONDITION_BLOCK", "type": "jira.condition.if.block",
      "conditions": [ { "component": "CONDITION", "type": "jira.comparison.condition",
        "value": { "first": "{{issue.customfield_10161}}", "operator": "MATCHES", "second": "^[A-Za-z0-9][A-Za-z0-9._-]{0,59}\$" } } ],
      "children": [
        { "component": "ACTION", "type": "jira.version.create",
          "value": { "name": "{{issue.customfield_10161}}", "projectKey": "FSM" } },
        { "component": "BRANCH", "type": "jira.issue.related",
          "value": { "relatedType": "jql", "jql": "key = \"{{issue.customfield_10148}}\"" },
          "children": [
            { "component": "ACTION", "type": "jira.issue.edit",
              "value": { "advancedJson": "{\"update\":{\"versions\":[{\"add\":{\"name\":\"{{issue.customfield_10161}}\"}}]}}" } },
            { "component": "ACTION", "type": "jira.issue.refetch", "value": {} },
            { "component": "CONDITION_BLOCK", "type": "jira.condition.if.block",
              "conditions": [ { "component": "CONDITION", "type": "jira.comparison.condition",
                "value": { "first": "{{issue.versions.name}}", "operator": "NOT_CONTAINS", "second": "{{triggerIssue.customfield_10161}}" } } ],
              "children": [
                { "component": "ACTION", "type": "jira.issue.comment",
                  "value": { "body": "apply to FSM FAILED — version absent after edit; marking Failed", "visibility": { "type": "internal" } } },
                { "component": "ACTION", "type": "jira.issue.transition",
                  "value": { "transition": { "id": "41", "name": "Failed" } } } ] },
            { "component": "CONDITION_BLOCK", "type": "jira.condition.else.block", "children": [
                { "component": "ACTION", "type": "jira.issue.transition",
                  "value": { "transition": { "id": "51", "name": "Synced" } } } ] } ] } ] },
    { "component": "CONDITION_BLOCK", "type": "jira.condition.else.block", "children": [
        { "component": "ACTION", "type": "jira.issue.comment",
          "value": { "body": "R-VERSION validation FAILED — bad version shape", "visibility": { "type": "internal" } } },
        { "component": "ACTION", "type": "jira.issue.transition",
          "value": { "transition": { "id": "41", "name": "Failed" } } } ] } ] } ] }
VERSION_JSON

  # R-LINK — Analysis fsm_target_key → clickable FSM→GBFSM back-link. Idempotent link
  # metadata; no Synced/Failed transition (exempt from (f)).
  cat > "$d/R-LINK.json" <<LINK_JSON
{ "rules": [ { "name": "GBFSM R-LINK — Analysis to FSM back-link",
  "actor": { "type": "ACCOUNT_ID", "value": "$AUTOMATION_ACTOR_ID" },
  "ruleScope": { "resources": [
    "ari:cloud:jira:a1b2c3d4-e5f6-7890-abcd-ef0123456789:project/$GFO_PROJECT_ID",
    "ari:cloud:jira:a1b2c3d4-e5f6-7890-abcd-ef0123456789:project/$FSM_PROJECT_ID" ] },
  "trigger": { "component": "TRIGGER", "type": "jira.issue.event.trigger:created" },
  "components": [
    { "component": "CONDITION", "type": "jira.comparison.condition",
      "value": { "first": "{{issue.issuetype.name}}", "operator": "EQUALS", "second": "Analysis" } },
    { "component": "CONDITION", "type": "jira.comparison.condition",
      "value": { "first": "{{issue.project.id}}", "operator": "EQUALS", "second": "$GFO_PROJECT_ID" } },
    { "component": "CONDITION", "type": "jira.comparison.condition",
      "value": { "first": "{{issue.customfield_10148}}", "operator": "MATCHES", "second": "^FSM-[0-9]+\$" } },
    { "component": "CONDITION", "type": "jira.user.condition",
      "value": { "operator": "OR", "conditions": [ { "field": "reporter", "check": "$REPORTER_POS_CHECK",
        "criteria": [ { "type": "ID", "value": "${BOT_ID_PREFIX}${BOT_UUID_GARDENIO}" },
                      { "type": "ID", "value": "${BOT_ID_PREFIX}${BOT_UUID_GARDENIO2}" } ] } ] } },
    { "component": "CONDITION", "type": "jira.comparison.condition",
      "value": { "first": "{{issue.key}}", "operator": "MATCHES", "second": "^GFO-" } },
    { "component": "ACTION", "type": "jira.issue.link",
      "value": { "linkType": "relates to", "inward": "{{issue.customfield_10148}}", "outward": "{{issue.key}}" } } ] } ] }
LINK_JSON

  # R-COMMENT′ — Analysis triage_internal_note_full field (customfield_10156) → FSM
  # internal note, append-log (folded 1:1, ka-0000002637). Idempotent comment-add;
  # no Synced/Failed transition (exempt from (f), same rationale as R-LINK). ALWAYS
  # internal (publicComment:false) — never customer-facing.
  cat > "$d/R-COMMENT.json" <<COMMENT_JSON
{ "rules": [ { "name": "GBFSM R-COMMENT' — Analysis triage_internal_note_full to FSM internal note",
  "actor": { "type": "ACCOUNT_ID", "value": "$AUTOMATION_ACTOR_ID" },
  "ruleScope": { "resources": [
    "ari:cloud:jira:a1b2c3d4-e5f6-7890-abcd-ef0123456789:project/$GFO_PROJECT_ID",
    "ari:cloud:jira:a1b2c3d4-e5f6-7890-abcd-ef0123456789:project/$FSM_PROJECT_ID" ] },
  "trigger": { "component": "TRIGGER", "type": "jira.issue.field.changed",
    "value": { "fields": [ { "value": "triage_internal_note_full", "type": "fieldName" } ] } },
  "components": [
    { "component": "CONDITION", "type": "jira.comparison.condition",
      "value": { "first": "{{issue.customfield_10148}}", "operator": "MATCHES", "second": "^FSM-[0-9]+\$" } },
    { "component": "CONDITION", "type": "jira.comparison.condition",
      "value": { "first": "{{issue.customfield_10156}}", "operator": "REGEX_MATCHES", "second": "(?s).+" } },
    { "component": "CONDITION", "type": "jira.user.condition",
      "value": { "operator": "OR", "conditions": [ { "field": "reporter", "check": "$REPORTER_POS_CHECK",
        "criteria": [ { "type": "ID", "value": "${BOT_ID_PREFIX}${BOT_UUID_GARDENIO}" },
                      { "type": "ID", "value": "${BOT_ID_PREFIX}${BOT_UUID_GARDENIO2}" } ] } ] } },
    { "component": "BRANCH", "type": "jira.issue.related",
      "value": { "relatedType": "jql", "jql": "key = \"{{issue.customfield_10148}}\"" },
      "children": [
        { "component": "ACTION", "type": "jira.issue.comment",
          "value": { "comment": "{{triggerIssue.customfield_10156}}", "publicComment": false,
                     "commentVisibility": null, "addCommentOnce": false } } ] } ] } ] }
COMMENT_JSON

  # R-CANARY — alerts on ANY direct bot-account FSM write (unchanged).
  cat > "$d/R-CANARY.json" <<CANARY_JSON
{ "rules": [ { "name": "GBFSM R-CANARY — alert on ANY bot-account comment on FSM (never fires)",
  "actor": { "type": "ACCOUNT_ID", "value": "$AUTOMATION_ACTOR_ID" },
  "ruleScope": { "resources": [
    "ari:cloud:jira:a1b2c3d4-e5f6-7890-abcd-ef0123456789:project/$FSM_PROJECT_ID" ] },
  "trigger": { "component": "TRIGGER", "type": "jira.issue.event.trigger:commented" },
  "components": [
    { "component": "CONDITION", "type": "jira.comparison.condition",
      "value": { "first": "{{comment.author.accountId}}", "operator": "MATCHES",
                 "second": "${BOT_ID_PREFIX}(${BOT_UUID_GARDENIO}|${BOT_UUID_GARDENIO2})" } },
    { "component": "ACTION", "type": "jira.issue.outgoing.email",
      "value": { "to": "matthew.baker@acme.example", "subject": "FSM canary", "body": "bot wrote FSM {{issue.key}}" } } ] } ] }
CANARY_JSON
}

# self_test_newshape PROVES the rewritten (a)-(j) assertions accept a FAITHFUL
# single-Analysis-ticket export and REJECT a drifted one — end-to-end through the
# real reconcile_artifact, using its OWN synthetic fixtures and WITHOUT touching the
# real fail-closed fixtures dir. It runs THIS script recursively in inner mode
# (OUTBOX_SELFTEST_INNER=1 skips the self-tests; OUTBOX_RULES_FIXTURES_DIR points at
# a private mktemp dir), so the inner run does ONLY reconcile_artifact against the
# synthetic fixtures. The recursion is one level deep (inner mode never re-enters
# the self-tests), so there is no unbounded recursion.
self_test_newshape() {
  blue "Self-test: single-Analysis-ticket reconciliation (faithful export PASSES, drift FAILS)"
  local tdir
  tdir="$(mktemp -d "${TMPDIR:-/tmp}/outbox-newshape-selftest.XXXXXX")" || {
    fail "new-shape self-test scratch dir" "mktemp failed — cannot run the new-shape reconciliation self-test"
    echo; return
  }
  # shellcheck disable=SC2064
  trap "rm -rf '$tdir'" RETURN

  # (1) FAITHFUL new-shape export → the whole reconciliation must go GREEN.
  local good="$tdir/good"; mkdir -p "$good"
  write_newshape_fixtures "$good"
  if OUTBOX_SELFTEST_INNER=1 OUTBOX_RULES_FIXTURES_DIR="$good" bash "$SELF" >/dev/null 2>&1; then
    pass "faithful single-Analysis-ticket export reconciles GREEN (invariants a-j accept the new shape)"
  else
    fail "new-shape self-test: faithful export PASSES" \
      "a synthetic FAITHFUL single-Analysis-ticket export (R-LABEL/R-VERSION/R-LINK/R-COMMENT′/R-CANARY) did NOT reconcile clean — the rewritten (a)-(j) assertions would FALSE-FAIL a correct operator export. Re-run: OUTBOX_SELFTEST_INNER=1 OUTBOX_RULES_FIXTURES_DIR=$good bash $SELF"
  fi

  # (2) DRIFTED export (gd-YdG7ap12oB: R-LABEL carries the no-'triaged-'-infix
  # allowlist) → the reconciliation must go RED (invariant (a) drift catch).
  local drift="$tdir/drift"; mkdir -p "$drift"
  write_newshape_fixtures "$drift"
  # Swap the correct allowlist for the drifted (no-infix) shape in R-LABEL only.
  local drift_re='^(gardenio-triaged|gardenio-sev-[0-3]|gardenio-gxp-[a-z0-9-]+)$'
  ALLOWLIST_RE_ESC="$ALLOWLIST_RE" DRIFT_RE="$drift_re" python3 - "$drift/R-LABEL.json" <<'PY' 2>/dev/null || true
import os, sys
p = sys.argv[1]
with open(p) as f:
    s = f.read()
s = s.replace(os.environ["ALLOWLIST_RE_ESC"], os.environ["DRIFT_RE"])
with open(p, "w") as f:
    f.write(s)
PY
  if OUTBOX_SELFTEST_INNER=1 OUTBOX_RULES_FIXTURES_DIR="$drift" bash "$SELF" >/dev/null 2>&1; then
    fail "new-shape self-test: drifted export FAILS" \
      "a synthetic export whose R-LABEL carries the no-'triaged-'-infix drift regex ($drift_re) reconciled GREEN — invariant (a) would FALSE-PASS the exact gd-YdG7ap12oB silent-dead-letter drift. Do NOT ship. Inspect: OUTBOX_SELFTEST_INNER=1 OUTBOX_RULES_FIXTURES_DIR=$drift bash $SELF"
  else
    pass "drifted export (no-'triaged-'-infix R-LABEL allowlist) reconciles RED — invariant (a) catches the gd-YdG7ap12oB drift on the new shape"
  fi

  # (3) T7-REPOINT drift (gd-dVR6VxDjT0): strip the pinned fsm_target_key regex
  # FSM_TARGET_RE (^FSM-[0-9]+$) from ONE apply rule (R-VERSION, so this drift is
  # isolated from the (a) R-LABEL drift above) → the reconciliation must go RED
  # (invariant (h) repoint-defense catch). Proves (h) is NON-VACUOUS: a repoint of
  # customfield_10148 to an arbitrary key would otherwise be applied to FSM. The
  # weakened condition (^.+$ — matches ANY target key) keeps the JSON structurally
  # valid; invariant (e)'s bare 'FSM' corpus grep still passes (R-VERSION keeps its
  # FSM project-key/JQL references), so ONLY (h) reddens — the exact hole (e) misses.
  local repoint="$tdir/repoint"; mkdir -p "$repoint"
  write_newshape_fixtures "$repoint"
  FSM_TARGET_RE_ESC="$FSM_TARGET_RE" REPOINT_RE='^.+$' python3 - "$repoint/R-VERSION.json" <<'PY' 2>/dev/null || true
import os, sys
p = sys.argv[1]
with open(p) as f:
    s = f.read()
s = s.replace(os.environ["FSM_TARGET_RE_ESC"], os.environ["REPOINT_RE"])
with open(p, "w") as f:
    f.write(s)
PY
  if OUTBOX_SELFTEST_INNER=1 OUTBOX_RULES_FIXTURES_DIR="$repoint" bash "$SELF" >/dev/null 2>&1; then
    fail "new-shape self-test: target-key repoint FAILS" \
      "a synthetic export whose R-VERSION drops the pinned fsm_target_key regex $FSM_TARGET_RE (weakened to ^.+\$) reconciled GREEN — invariant (h) would FALSE-PASS the T7 repoint hole: a crafted repoint of customfield_10148 to an arbitrary / cross-project key would be applied to FSM. Do NOT ship. Inspect: OUTBOX_SELFTEST_INNER=1 OUTBOX_RULES_FIXTURES_DIR=$repoint bash $SELF"
  else
    pass "target-key-repointed export (R-VERSION drops the ^FSM-[0-9]+\$ pin) reconciles RED — invariant (h) catches the T7 repoint hole that (e)'s bare 'FSM' grep misses"
  fi

  # (4) BROKEN-SCOPE drift (gd-l0ffwDFvQ8): repoint the scope from the REAL FSM id 10038
  # to the PHANTOM 10036 in EVERY rule file — the exact prod defect ({GFO 10037, PHANTOM
  # 10036}, FSM 10038 ABSENT). The whole reconciliation must go RED via invariant (e):
  # both the missing-10038 clause AND the present-10036 clause fire. Replacing across ALL
  # files removes 10038 from the corpus entirely (else a surviving scope leaves the FSM-id
  # signal present and this would not model an absent-FSM scope). This proves (e) is
  # NON-VACUOUS end-to-end through the real live check, not just via the scope_ok predicate
  # that self_test_scope exercises directly.
  local scopebreak="$tdir/scopebreak"; mkdir -p "$scopebreak"
  write_newshape_fixtures "$scopebreak"
  local sf
  for sf in "$scopebreak"/*.json; do
    FSM_PROJECT_ID_ESC="project/$FSM_PROJECT_ID" PHANTOM_PROJECT_ID_ESC="project/$PHANTOM_PROJECT_ID" \
      python3 - "$sf" <<'PY' 2>/dev/null || true
import os, sys
p = sys.argv[1]
with open(p) as f:
    s = f.read()
s = s.replace(os.environ["FSM_PROJECT_ID_ESC"], os.environ["PHANTOM_PROJECT_ID_ESC"])
with open(p, "w") as f:
    f.write(s)
PY
  done
  if OUTBOX_SELFTEST_INNER=1 OUTBOX_RULES_FIXTURES_DIR="$scopebreak" bash "$SELF" >/dev/null 2>&1; then
    fail "new-shape self-test: broken scope FAILS" \
      "a synthetic export whose rule scope names {GFO $GFO_PROJECT_ID, PHANTOM $PHANTOM_PROJECT_ID} with FSM $FSM_PROJECT_ID ABSENT reconciled GREEN — invariant (e) would FALSE-PASS the exact gd-l0ffwDFvQ8 false-Synced defect: the apply rules' JQL branch finds no in-scope FSM-N so the edit silently no-ops while the intent still transitions Synced. Do NOT ship. Inspect: OUTBOX_SELFTEST_INNER=1 OUTBOX_RULES_FIXTURES_DIR=$scopebreak bash $SELF"
  else
    pass "broken-scope export (scope {GFO $GFO_PROJECT_ID, PHANTOM $PHANTOM_PROJECT_ID}, FSM $FSM_PROJECT_ID absent) reconciles RED — invariant (e) catches the gd-l0ffwDFvQ8 false-Synced defect that the bare 'FSM' grep missed"
  fi

  # (5) REPORTER-POLARITY drift (gd-zKkSOkK6df): invert the reporter condition in ONE
  # apply rule (R-LINK, so this drift is isolated from the (a)/(h)/(e) drifts above) —
  # flip its check from the positive $REPORTER_POS_CHECK to USER_IS_NOT, leaving BOTH bot
  # ids in place → the reconciliation must go RED via invariant (d). The bot ids are
  # UNCHANGED, so the historical both-ids grep would have FALSE-PASSED this; only the
  # polarity check reddens. Proves (d) is NON-VACUOUS end-to-end through the real live
  # check — an inverted reporter guard (reporter is NOT a bot) would let an unprivileged
  # reporter's Analysis ticket drive FSM writes as the Automation actor.
  local polbreak="$tdir/polbreak"; mkdir -p "$polbreak"
  write_newshape_fixtures "$polbreak"
  POS_CHECK_ESC="\"check\": \"$REPORTER_POS_CHECK\"" INVERTED_ESC="\"check\": \"USER_IS_NOT\"" \
    python3 - "$polbreak/R-LINK.json" <<'PY' 2>/dev/null || true
import os, sys
p = sys.argv[1]
with open(p) as f:
    s = f.read()
# Flip ONLY the reporter condition's positive check to the inverted one. The synthetic
# R-LINK carries exactly one `"check": "<pos>"` (the reporter user-condition), so a plain
# replace is unambiguous here.
s = s.replace(os.environ["POS_CHECK_ESC"], os.environ["INVERTED_ESC"])
with open(p, "w") as f:
    f.write(s)
PY
  if OUTBOX_SELFTEST_INNER=1 OUTBOX_RULES_FIXTURES_DIR="$polbreak" bash "$SELF" >/dev/null 2>&1; then
    fail "new-shape self-test: inverted reporter polarity FAILS" \
      "a synthetic export whose R-LINK reporter condition is INVERTED (check USER_IS_NOT, both bot ids still present) reconciled GREEN — invariant (d) would FALSE-PASS the confused-deputy guard turned inside out: an unprivileged reporter's Analysis ticket would propagate to FSM (as the Automation actor) while the legitimate bot-reportered one is blocked. Do NOT ship. Inspect: OUTBOX_SELFTEST_INNER=1 OUTBOX_RULES_FIXTURES_DIR=$polbreak bash $SELF"
  else
    pass "reporter-inverted export (R-LINK reporter check flipped to USER_IS_NOT) reconciles RED — invariant (d) catches the polarity inversion the old both-ids grep missed"
  fi

  # (6) ACTOR-IS-BOT drift (gd-RigNkSZKE6): repoint ONE apply rule's Automation actor
  # (R-LABEL, so this drift is isolated from the (a)/(h)/(e)/(d) drifts above) from the
  # non-bot $AUTOMATION_ACTOR_ID to the gardenio bot accountId → the reconciliation must go
  # RED via invariant (i). This is the EXACT hole the adversarial panel proved live on
  # origin/main: with (i) absent, an export whose apply rule RUNS AS the bot passed 42/42.
  # Running as a bot is an outbox-isolation break (edits write FSM under a bot identity)
  # AND a confused deputy (R-CANARY — which alerts on ANY bot-account FSM write — would
  # fire on the outbox's own applies). Nothing ELSE in the rule changes, so ONLY (i)
  # reddens — proving (i) is NON-VACUOUS end-to-end through the real live check. Mutated
  # with jq (a first-class dependency, the same tool the proven repro used).
  local actorbreak="$tdir/actorbreak"; mkdir -p "$actorbreak"
  write_newshape_fixtures "$actorbreak"
  jq --arg bot "${BOT_ID_PREFIX}${BOT_UUID_GARDENIO}" '.rules[].actor.value = $bot' \
    "$actorbreak/R-LABEL.json" > "$actorbreak/R-LABEL.json.tmp" 2>/dev/null \
    && mv "$actorbreak/R-LABEL.json.tmp" "$actorbreak/R-LABEL.json" || true
  if OUTBOX_SELFTEST_INNER=1 OUTBOX_RULES_FIXTURES_DIR="$actorbreak" bash "$SELF" >/dev/null 2>&1; then
    fail "new-shape self-test: bot Automation actor FAILS" \
      "a synthetic export whose R-LABEL Automation actor (.rules[].actor.value) is a gardenio BOT accountId (${BOT_ID_PREFIX}${BOT_UUID_GARDENIO}) reconciled GREEN — invariant (i) would FALSE-PASS the exact confused-deputy / outbox-isolation break the adversarial panel proved live: the apply rule runs AS the bot, writing FSM under a bot identity and tripping R-CANARY on the outbox's own applies. Do NOT ship. Inspect: OUTBOX_SELFTEST_INNER=1 OUTBOX_RULES_FIXTURES_DIR=$actorbreak bash $SELF"
  else
    pass "bot-actor export (R-LABEL actor.value repointed to the gardenio bot accountId) reconciles RED — invariant (i) catches the confused-deputy / outbox-isolation break the narrative asserted but nothing machine-checked"
  fi
  echo
}

# ── (f) fail-on-apply-error discriminator ─────────────────────────
# transition_sink_counts FILE → prints "<failed_sinks> <synced_sinks>": the count
# of DISTINCT transition-ACTION components in the rule whose target status is
# Failed (exclusively) resp. Synced (exclusively). A Jira Automation
# 'Transition work item' action stores its target as a status NAME leaf under the
# component's value (`.value.transition.name` and kin); we scan every ACTION-ish
# object whose type carries 'issue.transition'/'issue_transition' and classify by
# the case-insensitive status-name leaves it contains. Exclusive matching (a sink
# counts as Failed only if it has a 'Failed' leaf and NO 'Synced' leaf, and vice
# versa) means an inline transition-comment mentioning the other word cannot flip
# a sink's class — this biases toward fail-closed. A transition TRIGGER or a
# status CONDITION is not an ACTION-transition and is excluded. jq errors (e.g.
# an export with no such structure) yield "0 0", which fails the check closed.
transition_sink_counts() {
  jq -rs '
    def sinks:
      [ .. | objects
        | select( (.type? // "") | ascii_downcase | test("issue[._]transition") )
        | { f: ([ (.value? // {}) | .. | strings | select(ascii_downcase == "failed") ] | length),
            s: ([ (.value? // {}) | .. | strings | select(ascii_downcase == "synced") ] | length) } ];
    sinks
    | "\([ .[] | select(.f > 0 and .s == 0) ] | length) \([ .[] | select(.s > 0 and .f == 0) ] | length)"
  ' "$1" 2>/dev/null || printf '0 0'
}

# fail_on_apply_error_check NAME FILE — invariant (f), CONSERVATIVE FAIL-CLOSED.
#
# ACCEPTED TOLERANT-MATCHING LIMIT (modeled on the '(d) reporter-is-bot polarity'
# limit): the Jira Automation Export-JSON schema is not pinned in-repo, so this
# gate CANNOT prove that the second Failed sink is structurally the child of the
# apply action's error/false branch (vs, say, a duplicated validation-else Failed).
# What it CAN prove soundly is the count relation that the structural fix
# necessarily produces and the broken shape necessarily lacks:
#
#   • the buggy rule (ka-0000004008 build report) had EXACTLY ONE
#     Transition->Failed (validation-ELSE) and an UNCONDITIONAL Transition->Synced
#     — 1 Failed sink, 1 Synced sink.
#   • the FIX (P0 gd-l0ffwDFvQ8: "ADD AN ERROR BRANCH: if the apply action fails,
#     transition the intent to FAILED") adds a SECOND, apply-error Failed sink —
#     2 Failed sinks, 1 Synced sink.
#
# So (f) PASSES a rule only when it has ≥2 Failed sinks AND ≥1 Synced sink. This is:
#   - NO-FALSE-PASS on the broken shape: 1 Failed / 1 Synced ⇒ RED (bar 2). A prose
#     comment saying 'FAILED' does not add a sink (we count transition ACTIONS, not
#     string occurrences), and an id-only export with no status names yields 0/0.
#   - CAPABLE OF PASSING a faithful corrected export (bar 4): the natural fix —
#     whether the apply-error Failed sits inside the JQL branch or in the main flow
#     after it — produces the 2-Failed/1-Synced shape and PASSES.
#   - RESIDUAL a human must still eyeball on JSON commit: that the extra Failed sink
#     is genuinely gated on the apply outcome, not a cosmetic duplicate.
# apply_error_ok FAILED_SINKS SYNCED_SINKS — the (f) pass predicate, in ONE place so
# the live check and the hermetic self-test cannot drift.
apply_error_ok() { [ "$1" -ge 2 ] && [ "$2" -ge 1 ]; }

fail_on_apply_error_check() {
  local name="$1" file="$2" counts failed synced
  counts="$(transition_sink_counts "$file")"
  failed="${counts%% *}"; synced="${counts##* }"
  if apply_error_ok "$failed" "$synced"; then
    pass "$name Fails on apply error (${failed} Transition→Failed sinks, ${synced} Transition→Synced sink — validation-reject + apply-error paths both present)"
  else
    fail "$name fail-on-apply-error branch" \
      "$name ($(basename "$file")) has ${failed} Transition→Failed sink(s) and ${synced} Transition→Synced sink(s) — a corrected rule needs a SECOND Failed sink (apply-error branch) beyond the validation-else one (≥2 Failed sinks and ≥1 Synced sink). The broken shape (1 Failed / 1 Synced, or an id-only export) transitions Pending→Synced regardless of whether the apply landed — the false-success P0 gd-l0ffwDFvQ8 / gd-d7U7jd6cyH. Add the apply-error → Failed branch, then eyeball that the extra Failed is gated on the apply outcome."
  fi
}

# ── (e) scope discriminator ───────────────────────────────────────
# id_token_present ID CORPUS — TRUE iff the numeric project id ID appears in CORPUS as
# a WHOLE number token (not embedded in a longer number). The scope ids (10036/10037/
# 10038) differ only in the last digit AND could otherwise collide with a customfield /
# version number that merely embeds them (e.g. a bare `grep -F 10038` also matches
# `customfield_100380`). Anchoring on a non-digit (or string edge) on both sides makes
# the membership check exact against that hazard while staying schema-tolerant (it does
# not require the id to sit in a specific JSON key path). This is the ONE matcher the
# live (e) check AND the hermetic self-test both use, so they cannot drift.
id_token_present() { grep -qE -- "(^|[^0-9])$1([^0-9]|\$)" <<< "$2"; }

# The (e) scope clauses, each in ONE place so the live check and self_test_scope cannot
# drift. gfo_in_scope / fsm_in_scope require the id as a whole token; phantom_in_scope
# is TRUE when the phantom id appears at all (its very presence is the defect).
gfo_in_scope()     { id_token_present "$GFO_PROJECT_ID"     "$1"; }
fsm_in_scope()     { id_token_present "$FSM_PROJECT_ID"     "$1"; }
phantom_in_scope() { id_token_present "$PHANTOM_PROJECT_ID" "$1"; }

# scope_ok CORPUS — the (e) pass predicate, composed from the shared clauses above so
# the live check and the hermetic self-test cannot drift (mirrors apply_error_ok).
# TRUE iff the corpus:
#   • names GFO 10037     (the SOURCE/trigger project — already required today), AND
#   • names FSM 10038     (the REAL apply/target project the JQL branch resolves), AND
#   • does NOT name 10036 (the phantom the prod defect scoped in FSM's place).
# The broken prod scope {GFO 10037, PHANTOM 10036} with FSM 10038 ABSENT (gd-l0ffwDFvQ8)
# is REJECTED by BOTH the missing-10038 clause AND the present-10036 clause — either
# alone reddens, so the two clauses are belt-and-suspenders against the exact defect.
scope_ok() {
  gfo_in_scope "$1" && fsm_in_scope "$1" && ! phantom_in_scope "$1"
}

# version_create_target_leaves FILE — prints the LEAF VALUES of every jira.version.create
# ("Create version") ACTION's own value object in FILE, one per line (empty if the rule
# has no such action). Both string leaves AND numeric leaves are emitted (numbers coerced
# to strings) so a target project expressed as a numeric projectId (e.g. projectId:10038)
# is seen, not just a projectKey string. This is the "Space"/target-project the version
# is created IN. Matt reported (gd-l0ffwDFvQ8) that action was INDEPENDENTLY fat-fingered
# to the phantom 10036 — separate from the rule scope — so (e) pins it to FSM directly.
# Scoping the leaf-scan to the version.create action's value (not the whole file) means
# the FSM project-id/key on OTHER conditions can't false-pass this specific check, and a
# phantom on a DIFFERENT action can't false-fail it.
version_create_target_leaves() {
  jq -r '
    [ .. | objects
      | select( (.type? // "") | ascii_downcase | test("version[._]create|create.?version|fixversion") )
      | (.value? // {}) | .. | (strings, (numbers|tostring)) ] | .[]
  ' "$1" 2>/dev/null
}

# ── (d) reporter-is-bot POLARITY discriminator ────────────────────
# reporter_is_bot_verdict FILE — the ONE place the (d) verdict is computed, so the live
# check and the hermetic self_test_reporter_polarity cannot drift. Prints exactly one of:
#   OK             — the file carries ≥1 reporter jira.user.condition whose check is the
#                    POSITIVE polarity ($REPORTER_POS_CHECK) AND references BOTH bot ids,
#                    and NO reporter user-condition carries a non-positive check.
#   BAD_POLARITY:X — a reporter user-condition carries a NON-positive check X (e.g.
#                    USER_IS_NOT) — the inverted confused-deputy guard. X is the sorted,
#                    comma-joined set of offending checks ('<empty>' for an absent check).
#                    Fail-closed: ANY non-positive reporter check reddens, even if a
#                    correct positive one is also present (a rule must not both include
#                    and invert the guard).
#   MISSING_BOTH   — every reporter condition is positive-polarity, but the UNION of all
#                    positive reporter conditions' criteria does not reference BOTH bot ids.
#   NONE           — the file carries NO reporter jira.user.condition at all.
# The scan handles BOTH the REAL pinned nested shape (value.conditions[].{field,check,
# criteria[].value}, schemaVersion 6 — the four committed fixtures) AND the legacy flat
# shape (value.{field,check,value[]}) the older synthetic fixtures used, so it keeps
# working if either form is exported. Bot-id membership tolerates the accountId prefix
# ('712020:') being present or factored out (substring contains on the 36-char UUID —
# a UUID cannot plausibly collide). The both-ids requirement is computed over the UNION
# of every POSITIVE reporter condition's criteria in the file, so a faithful guard that
# expresses "reporter IS bot1 OR reporter IS bot2" as TWO OR'd single-id conditions[]
# (semantically identical to the pinned two-criteria form) is accepted, not false-failed
# (adversarial-review false-fail). The polarity check stays PER-condition and fail-closed:
# ANY non-positive reporter check reddens BEFORE the both-ids union is considered, so a
# split shape where one arm is inverted is still rejected. jq errors (malformed export)
# yield NONE, which the caller treats as fail-closed. check is case-normalized
# (ascii_upcase) so a lowercase check leaf cannot slip past the polarity gate — the
# dangerous inversion 'user_is_not' still upcases to USER_IS_NOT != USER_IS; field is
# likewise ascii_downcase'd to match the canonical lowercase 'reporter' system-field key.
reporter_is_bot_verdict() {
  jq -rs --arg b1 "$BOT_UUID_GARDENIO" --arg b2 "$BOT_UUID_GARDENIO2" --arg pos "$REPORTER_POS_CHECK" '
    ($pos | ascii_upcase) as $POS
    | [ .. | objects
        | select( (.type? // "") == "jira.user.condition" )
        | (.value? // {}) as $v
        # NESTED shape: value.conditions[]. LEGACY flat shape: the value object itself
        # carries field/check/value (only when there is no nested conditions[]).
        | ( ($v.conditions? // [])
            + ( if (($v.conditions? // []) | length) == 0 and ($v.field? != null) then [ $v ] else [] end ) )
        | .[]
        | select( ((.field? // "") | ascii_downcase) == "reporter" )
        | { check: ((.check? // "") | ascii_upcase),
            crit: ( [ (.criteria? // [])[] | (.value? // "") | tostring ]
                    + [ (.value? // empty) | (if type == "array" then .[] else . end) | tostring ] ) }
      ] as $rc
    # UNION of criteria across every POSITIVE-polarity reporter condition.
    | ( [ $rc[] | select(.check == $POS) | .crit[] ] ) as $poscrit
    | if ($rc | length) == 0 then "NONE"
      elif ( $rc | any( .check != $POS ) )
        then ( "BAD_POLARITY:" + ( [ $rc[] | select(.check != $POS)
                                     | (if .check == "" then "<empty>" else .check end) ] | unique | join(",") ) )
      elif ( ($poscrit | any(contains($b1))) and ($poscrit | any(contains($b2))) ) then "OK"
      else "MISSING_BOTH" end
  ' "$1" 2>/dev/null || printf 'NONE'
}

# ── (i) Automation-actor-never-a-bot discriminator ────────────────
# actor_bot_verdict FILE — the ONE place the (i) verdict is computed. Examines EVERY
# rule's Automation actor (.rules[].actor.value — the identity the rule RUNS AS) and
# prints exactly one of:
#   OK        — every rule in the file has a NON-EMPTY actor.value and NONE contains a
#               gardenio bot UUID (BOT_UUID_GARDENIO / BOT_UUID_GARDENIO2).
#   MISSING   — at least one rule has a missing/empty actor.value (an actor-less rule is
#               unverifiable — fail closed).
#   BOT:<v>   — at least one rule's actor.value CONTAINS a bot UUID (the confused-deputy /
#               outbox-isolation break). <v> is the comma-joined offending actor value(s).
#   NONE      — the file carries no .rules[] at all (or jq errored) — fail closed.
# Bot-id membership is a bare-UUID substring `contains`, exactly like (b)/(h): the actor
# value is the full "712020:<uuid>" accountId, and a 36-char UUID cannot plausibly
# collide, so a substring match is the tolerant, no-false-positive test. `.actor.value?`
# suppresses a jq error if `actor` is not an object; a missing actor coalesces to "".
actor_bot_verdict() {
  jq -r --arg b1 "$BOT_UUID_GARDENIO" --arg b2 "$BOT_UUID_GARDENIO2" '
    [ .rules[]? | (.actor.value? // "") ] as $actors
    | if ($actors | length) == 0 then "NONE"
      elif ( $actors | any(. == "") ) then "MISSING"
      elif ( $actors | any( contains($b1) or contains($b2) ) )
        then ( "BOT:" + ( [ $actors[] | select( contains($b1) or contains($b2) ) ] | join(",") ) )
      else "OK" end
  ' "$1" 2>/dev/null || printf 'NONE'
}

# ── Artifact reconciliation (the fail-closed gate) ────────────────
reconcile_artifact() {
  shopt -s nullglob
  local json_files=("$FIXTURES_DIR"/*.json)
  shopt -u nullglob

  if [ "${#json_files[@]}" -eq 0 ]; then
    # FAIL CLOSED — this is a gate awaiting the operator export, NOT a skip.
    fail "operator rule-JSON export present in fixtures/outbox-rules/" \
      "no *.json committed yet — this gate stays RED until the operator commits the exported rule JSON (Actions → Export JSON, per-rule, in Global automation) for R-LABEL / R-VERSION / R-LINK / R-CANARY. See $(basename "$FIXTURES_DIR")/README.md. This is intended defense-in-depth (gd-dq6raBnxLV), not a bug."
    blue "  → AWAITING OPERATOR ARTIFACT. The fail-closed contract holds: gate is RED."
    echo
    return
  fi

  pass "operator rule-JSON export present (${#json_files[@]} file(s) in fixtures/outbox-rules/)"

  # Parseability — unparseable JSON fails closed (contract).
  local f parse_ok=1
  for f in "${json_files[@]}"; do
    if jq -e . "$f" >/dev/null 2>&1; then
      dim "    parsed: $(basename "$f")"
    else
      fail "rule JSON parseable" "$(basename "$f") is not valid JSON (jq parse failed) — a corrupt/truncated export fails the gate closed"
      parse_ok=0
    fi
  done
  if [ "$parse_ok" != 1 ]; then
    blue "  → at least one export unparseable; skipping deep assertions (fail-closed)"
    echo
    return
  fi
  pass "all committed rule JSON files parse as valid JSON"
  echo

  classify_files "${json_files[@]}"
  # show_slot echoes the basename of a populated slot, or "<not found>".
  show_slot() { if [ -n "$1" ]; then basename "$1"; else printf '<not found>'; fi; }
  blue "Rule identification (tolerant, by apply-action / name literal — single-Analysis-ticket shape):"
  dim  "    R-LABEL    ← $(show_slot "$R_LABEL_FILE")"
  dim  "    R-VERSION  ← $(show_slot "$R_VERSION_FILE")"
  dim  "    R-LINK     ← $(show_slot "$R_LINK_FILE")"
  dim  "    R-COMMENT′ ← $(show_slot "$R_COMMENT_FILE")"
  dim  "    R-CANARY   ← $(show_slot "$R_CANARY_FILE")"
  echo

  # A single normalized corpus string (all files) for set-wide literal lookups.
  local corpus
  corpus="$(cat "${json_files[@]}")"

  # String LEAF VALUES across all files (jq: recurse values, emit strings only —
  # object KEYS are not values in jq's model, so they are excluded). One leaf per
  # line. This is the precise, schema-agnostic substrate for (a): matching a rule's
  # actual condition/action VALUE, not a substring of a key or a prose comment
  # (adversarial-review CRITICAL-1). Every file already parsed clean, so jq cannot
  # error here.
  local leaves
  leaves="$(jq -r '.. | strings' "${json_files[@]}" 2>/dev/null)"

  # ── (a) op_value allowlist: ALLOWLIST_RE present in R-LABEL, drift absent ──
  blue "Invariant (a): op_value allowlist regex == ALLOWLIST_RE in R-LABEL (drift = gd-YdG7ap12oB)"
  if [ -z "$R_LABEL_FILE" ]; then
    fail "R-LABEL rule present" \
      "no R-LABEL export identified (need a rule with a labels apply / the canonical 'gardenio-triaged' vocabulary) — the op_value allowlist is unverifiable; failing closed"
  else
    # Presence: ALLOWLIST_RE must appear in a JSON string LEAF VALUE of the R-LABEL
    # file — as a substring of a leaf (the 1:1-reshape smart-value form, e.g.
    # `{{issue.customfield_LABELS.match("<ALLOWLIST_RE>")…}}`) OR the whole leaf (a
    # standalone `matches ALLOWLIST_RE` condition; grep -F matches both). Scoped to
    # the R-LABEL file's leaves only, so a benign mention in prose/keys elsewhere
    # can't false-pass.
    local label_leaves
    label_leaves="$(jq -r '.. | strings' "$R_LABEL_FILE" 2>/dev/null)"
    if grep -qF -- "$ALLOWLIST_RE" <<< "$label_leaves"; then
      pass "R-LABEL carries the exact ALLOWLIST_RE op_value allowlist as a condition/apply leaf value"
    else
      fail "ALLOWLIST_RE present in R-LABEL" \
        "no string-leaf value in $(basename "$R_LABEL_FILE") contains the tight op_value allowlist — R-LABEL does not enforce the canonical allowlist (or stores it somewhere this tolerant scan can't see). Expected leaf substring: $ALLOWLIST_RE"
    fi
  fi
  # Drift: no leaf value may CONTAIN a no-'triaged-'-infix literal. Case-INSENSITIVE
  # (adversarial-review HIGH-2). Scoped to leaf VALUES so a benign mention in prose/
  # keys can't false-fail, and a drifted regex embedded in a larger allowlist value
  # (e.g. '^(gardenio-triaged|gardenio-sev-[0-3]|...)$') is still caught.
  local d drift_found=0
  for d in "${DRIFT_LITERALS[@]}"; do
    if grep -qiF -- "$d" <<< "$leaves"; then
      fail "no-'triaged-'-infix drift absent" \
        "DRIFT DETECTED: a rule condition value contains '$d' — the no-'triaged-'-infix allowlist shape that silently dead-letters every sev/GxP classification (gd-YdG7ap12oB). The op_value regex must be ALLOWLIST_RE, not the historical gardenio-sev-/gardenio-gxp- shape."
      drift_found=1
    fi
  done
  [ "$drift_found" = 0 ] && pass "no no-'triaged-'-infix drift literals (gardenio-sev- / gardenio-gxp-, any case) in any rule condition value"
  echo

  # ── (b) R-CANARY carries BOTH bot accountIds ──
  blue "Invariant (b): R-CANARY carries both bot accountIds"
  if [ -z "$R_CANARY_FILE" ]; then
    fail "R-CANARY rule present" \
      "no R-CANARY export identified (need a rule whose name contains 'CANARY' or a comment.author.accountId condition) — the direct-FSM-write alert is unverifiable; failing closed"
  else
    assert_file_contains "R-CANARY carries gardenio@ accountId (${BOT_ID_PREFIX}${BOT_UUID_GARDENIO})"  "$R_CANARY_FILE" "$BOT_UUID_GARDENIO"
    assert_file_contains "R-CANARY carries gardenio2 accountId (${BOT_ID_PREFIX}${BOT_UUID_GARDENIO2})" "$R_CANARY_FILE" "$BOT_UUID_GARDENIO2"
  fi
  echo

  # ── (c) no PUBLIC comment marker anywhere (invariant #6) ──
  blue "Invariant (c): no public / 'Share with customer' comment marker in any rule (customer-notification safety)"
  # The Description is no longer posted to FSM as a comment (R-COMMENT retired); the
  # FSM→GBFSM link (R-LINK) is field metadata with no visibility concept and fires
  # NO customer email. Any optional internal note must be INTERNAL — so ANY public
  # comment marker ANYWHERE in the corpus is a customer-facing leak and fails the
  # gate closed. Denylist broadened (adversarial-review CRITICAL-2), case-insensitive.
  local corpus_compact
  corpus_compact="$(tr -d ' \t\n\r' <<< "$corpus" | tr '[:upper:]' '[:lower:]')"
  if grep -qF -- '"jsdpublic":true' <<< "$corpus_compact" \
     || grep -qF -- '"visibility":"public"' <<< "$corpus_compact" \
     || grep -qF -- 'sd.public.comment.public' <<< "$corpus_compact" \
     || grep -qiF -- 'Share with customer' <<< "$corpus"; then
    fail "no public comment marker" \
      "a rule carries a PUBLIC visibility marker (jsdPublic=true / visibility:public / 'Share with customer') — an FSM comment would leak to the customer desk; SECURITY breach of the notification-safety guarantee (invariant #6). Any note posted to FSM must be Internal-only, rule-hardcoded."
  else
    pass "no rule carries a public / 'Share with customer' comment marker (any internal note stays INTERNAL; R-LINK posts a link, not a comment)"
  fi
  echo

  # ── (d) reporter-is-bot condition + POLARITY on the apply rules ──
  blue "Invariant (d): reporter-is-bot condition on R-LABEL / R-VERSION / R-LINK — POLARITY $REPORTER_POS_CHECK, both bot ids (T11 confused-deputy guard, gd-zKkSOkK6df)"
  # The real Export-JSON schema is now PINNED in-repo (the four committed fixtures), so
  # (d) is no longer a bare both-ids presence grep — it asserts the reporter
  # jira.user.condition's actual POLARITY. A rule that listed both bot ids under an
  # INVERTED check (USER_IS_NOT — "reporter is NOT a bot") would have passed the old
  # presence grep while doing the OPPOSITE of the confused-deputy guard: it would let an
  # UNPRIVILEGED reporter's Analysis ticket propagate to FSM and BLOCK the legitimate
  # bot-reportered one. reporter_is_bot_verdict pins the check to $REPORTER_POS_CHECK and
  # fails closed on any other value. RESIDUAL a human still eyeballs on JSON commit: that
  # the condition sits in the rule's if-block guard (not a disabled/dangling component) —
  # but the polarity and both-ids wiring are now machine-enforced, not eyeballed.
  local rule name file verdict
  for rule in "R-LABEL:$R_LABEL_FILE" "R-VERSION:$R_VERSION_FILE" "R-LINK:$R_LINK_FILE"; do
    name="${rule%%:*}"; file="${rule#*:}"
    if [ -z "$file" ]; then
      fail "$name reporter-is-bot condition" \
        "no $name export identified — the confused-deputy guard is unverifiable for it; failing closed"
      continue
    fi
    verdict="$(reporter_is_bot_verdict "$file")"
    case "$verdict" in
      OK)
        pass "$name carries the reporter-is-bot condition with POSITIVE polarity ($REPORTER_POS_CHECK, both bot accountIds referenced)"
        ;;
      BAD_POLARITY:*)
        fail "$name reporter-is-bot condition polarity" \
          "$name ($(basename "$file")) has a reporter user-condition with a NON-positive check '${verdict#BAD_POLARITY:}' (expected $REPORTER_POS_CHECK). An INVERTED reporter condition (USER_IS_NOT) is the confused-deputy guard turned inside out: it would let an UNPRIVILEGED reporter's Analysis ticket propagate to FSM (as the Automation actor) while BLOCKING a legitimate bot-reportered one (v4 delta, ka-0000004008 / gd-zKkSOkK6df). The check MUST be $REPORTER_POS_CHECK."
        ;;
      MISSING_BOTH)
        fail "$name reporter-is-bot condition" \
          "$name ($(basename "$file")) has a $REPORTER_POS_CHECK reporter condition but does NOT reference both bot accountIds ($BOT_ID_PREFIX$BOT_UUID_GARDENIO / $BOT_ID_PREFIX$BOT_UUID_GARDENIO2) — the guard must admit BOTH gardenio bots, else one bot's tickets silently never propagate."
        ;;
      *)  # NONE (or a jq error coerced to NONE) — no reporter condition at all.
        fail "$name reporter-is-bot condition" \
          "$name ($(basename "$file")) carries NO reporter jira.user.condition — the reporter-is-bot confused-deputy condition (v4 delta, ka-0000004008) is missing; with GFO site-public an unprivileged reporter could file an Analysis ticket the rule applies to FSM as the Automation actor"
        ;;
    esac
  done
  echo

  # ── (e) rule scope {GFO 10037, FSM 10038}, phantom 10036 absent ──
  blue "Invariant (e): rule scope {GFO $GFO_PROJECT_ID, FSM $FSM_PROJECT_ID} — both ids present, phantom $PHANTOM_PROJECT_ID absent (gd-l0ffwDFvQ8 false-Synced defense)"
  # ── (e) ACCEPTED TOLERANT-MATCHING LIMIT (modeled on (a)/(d)/(f)/(h)) ──
  # The Jira Automation Export-JSON SCOPE-resource schema is not yet pinned in-repo
  # (the scope is an ARI list — e.g. ruleScope.resources[] = ari:cloud:jira::project/
  # 10037 — whose exact key path we cannot yet assert). So (e) is a CONSERVATIVE
  # FAIL-CLOSED set-membership check over whole-corpus WHOLE-NUMBER-TOKEN lookups (each
  # id must appear as a complete number, not embedded in a longer one — so a customfield
  # / version number that merely contains 10036/10037/10038 cannot false-pass or
  # false-fail): it PASSES only on an UNAMBIGUOUSLY-CORRECT scope that names GFO 10037
  # AND the REAL FSM id 10038 AND names the phantom 10036 NOWHERE. It CANNOT itself prove
  # those ids sit specifically in the scope resource (vs, say, an unrelated field/version
  # whose value equals 10038) — but the combination is unforgeable enough for the defect
  # at hand: the broken prod export {GFO 10037, PHANTOM 10036 / no 10038} necessarily
  # FAILS both the FSM-present and the phantom-absent clause. RESIDUAL a human eyeballs on
  # JSON commit: that 10037/10038 are the rule's SCOPE ari, not an unrelated field value.
  # When the scope schema is pinned this can tighten to `ruleScope.resources`.
  # The three clauses below call the SAME shared predicates self_test_scope exercises
  # (gfo_in_scope / fsm_in_scope / phantom_in_scope), so the live check and the
  # self-test cannot drift.
  # GFO 10037 present — the SOURCE/trigger side (unchanged from the prior (e)).
  if gfo_in_scope "$corpus"; then
    pass "exported rules reference GFO project id $GFO_PROJECT_ID (source/trigger side of the scope)"
  else
    fail "GFO project id $GFO_PROJECT_ID present" \
      "GFO project id $GFO_PROJECT_ID not found in the export — the outbox rules must reference GFO 10037 (ka-0000004008)"
  fi
  # FSM 10038 present — the REAL apply/target project. A scope naming ONLY GFO (or the
  # phantom) means key={{issue.customfield_10148}}=FSM-N finds no IN-SCOPE target, the
  # edit silently no-ops, and the intent STILL flips to Synced (the false-Synced defect).
  if fsm_in_scope "$corpus"; then
    pass "exported rules reference the REAL FSM project id $FSM_PROJECT_ID (apply/target side of the scope — the JQL branch can resolve an in-scope FSM-N)"
  else
    fail "FSM project id $FSM_PROJECT_ID present (scope {GFO $GFO_PROJECT_ID, FSM $FSM_PROJECT_ID})" \
      "the REAL FSM project id $FSM_PROJECT_ID is ABSENT from the export — a scope naming ONLY GFO (or the phantom $PHANTOM_PROJECT_ID) is the exact gd-l0ffwDFvQ8 defect: the apply rules' JQL branch key={{issue.customfield_10148}}=FSM-N finds no in-scope target, so the edit SILENTLY NO-OPS while the intent still transitions Pending→Synced (false-Synced). The rules MUST be scoped to FSM $FSM_PROJECT_ID. NOTE: the bare literal 'FSM' (a rule name / JQL branch / target regex) is NOT sufficient — only the real id $FSM_PROJECT_ID proves the scope resolves."
  fi
  # Phantom 10036 ABSENT — the non-existent project the prod defect scoped in FSM's
  # place. ANY occurrence anywhere in the corpus (scope resource OR a fat-fingered
  # version.create Space) reddens the gate closed.
  if phantom_in_scope "$corpus"; then
    fail "phantom project id $PHANTOM_PROJECT_ID absent everywhere" \
      "PHANTOM DETECTED: the export references project id $PHANTOM_PROJECT_ID, which DOES NOT EXIST (the Jira scope UI rendered it as an 'Unavailable value'). This is the exact gd-l0ffwDFvQ8 false-Synced defect: the 3 apply rules were scoped {GFO $GFO_PROJECT_ID, PHANTOM $PHANTOM_PROJECT_ID} with FSM $FSM_PROJECT_ID ABSENT, so every apply silently no-oped against an out-of-scope FSM-N while transitioning Synced. The scope (and every action's target project) must name FSM $FSM_PROJECT_ID, NEVER $PHANTOM_PROJECT_ID."
  else
    pass "no phantom project id $PHANTOM_PROJECT_ID anywhere in the corpus (the non-existent project the false-Synced defect scoped in FSM's place)"
  fi
  # R-VERSION jira.version.create target ("Space") pinned to FSM. Matt reported this
  # action was INDEPENDENTLY fat-fingered to 10036 — separate from the rule scope — so
  # pin it directly: the version-create action's own value must name FSM (key 'FSM' or
  # id 10038 as a whole token) and must NOT name the phantom. Fail-closed if R-VERSION or
  # its version.create action is missing (an id-only / actionless export cannot be
  # proven). RESIDUAL (accepted tolerant-matching): the positive accept looks for the
  # literal 'FSM' anywhere in the version.create value's leaves, so a version whose NAME
  # (not its target projectKey) contains 'FSM' would also satisfy it — the human reviewer
  # eyeballs on JSON commit that the FSM sits in the target Space/projectKey, not the
  # version name. The specific defect defended (the phantom 10036) is still caught
  # unconditionally by the phantom clause below (checked BEFORE the FSM accept) and
  # corpus-wide above.
  if [ -z "$R_VERSION_FILE" ]; then
    fail "R-VERSION version.create target pinned to FSM" \
      "no R-VERSION export identified — the version-create target project (Space) is unverifiable; failing closed"
  else
    local vc_leaves
    vc_leaves="$(version_create_target_leaves "$R_VERSION_FILE")"
    if [ -z "$vc_leaves" ]; then
      fail "R-VERSION version.create target pinned to FSM" \
        "R-VERSION ($(basename "$R_VERSION_FILE")) has no recognizable jira.version.create action value — the target project (Space) the version is created in is unverifiable; failing closed"
    elif phantom_in_scope "$vc_leaves"; then
      fail "R-VERSION version.create target pinned to FSM" \
        "R-VERSION's jira.version.create action targets the PHANTOM project $PHANTOM_PROJECT_ID (Matt reported this action was independently fat-fingered to it, gd-l0ffwDFvQ8) — the version is created in a non-existent project and never reaches FSM. Pin the version-create Space to FSM ($FSM_PROJECT_ID / projectKey FSM)."
    elif grep -qF -- 'FSM' <<< "$vc_leaves" || fsm_in_scope "$vc_leaves"; then
      pass "R-VERSION jira.version.create targets FSM (projectKey FSM / id $FSM_PROJECT_ID — the version is created in the real FSM project, not the phantom $PHANTOM_PROJECT_ID)"
    else
      fail "R-VERSION version.create target pinned to FSM" \
        "R-VERSION's jira.version.create action value names neither FSM nor id $FSM_PROJECT_ID — the version-create Space is not pinned to FSM, so the Affects-Version would be created in the wrong project. Pin it to FSM (projectKey FSM / id $FSM_PROJECT_ID)."
    fi
  fi
  echo

  # ── (f) fail-on-apply-error: each FIELD-APPLY rule Fails (not Syncs) on apply error ──
  blue "Invariant (f): field-apply rules transition to FAILED on the apply-action error path (P0 gd-l0ffwDFvQ8 / bug gd-d7U7jd6cyH)"
  dim  "  (R-LINK is EXEMPT — an issue/web link is idempotent field metadata with no Synced/Failed transition and no 'Synced != applied' risk.)"
  local frule fname ffile
  for frule in "R-LABEL:$R_LABEL_FILE" "R-VERSION:$R_VERSION_FILE"; do
    fname="${frule%%:*}"; ffile="${frule#*:}"
    if [ -z "$ffile" ]; then
      fail "$fname fail-on-apply-error branch" \
        "no $fname export identified — the apply-error → Failed branch is unverifiable for it; failing closed"
      continue
    fi
    fail_on_apply_error_check "$fname" "$ffile"
  done
  echo

  # ── (g) R-LINK present: FSM→GBFSM back-link (Phase 3), applied by Automation ──
  blue "Invariant (g): R-LINK present — Analysis fsm_target_key → clickable FSM→GBFSM back-link (Phase 3 ka-0000001070)"
  if [ -z "$R_LINK_FILE" ]; then
    fail "R-LINK rule present" \
      "no R-LINK export identified (need a rule with an issue-link / web-link action) — the FSM→GBFSM clickable back-link is unverifiable; failing closed. R-LINK is required in the 1:1 reshape (it REPLACES the retired plain-text Internal-Comment link-back)."
  else
    assert_file_contains "R-LINK references fsm_target_key (customfield_10148 — the FSM target)" "$R_LINK_FILE" "customfield_10148"
    if grep -qF -- 'FSM' "$R_LINK_FILE"; then
      pass "R-LINK targets FSM (the link is created on the FSM-N ticket by the Automation actor)"
    else
      fail "R-LINK targets FSM" \
        "R-LINK ($(basename "$R_LINK_FILE")) does not reference FSM — the back-link must be created on the FSM ticket (target ^FSM-[0-9]+\$)"
    fi
    if grep -qiE -- 'issuelink|issue[._]link|jira\.issue\.link|relates|remotelink|web ?link|link work item' "$R_LINK_FILE"; then
      pass "R-LINK carries a link action (issue-link / web-link — field metadata, fires NO customer email)"
    else
      fail "R-LINK carries a link action" \
        "R-LINK ($(basename "$R_LINK_FILE")) has no recognizable link action — the FSM→GBFSM back-link must be an issue-link or web/remote-link (field metadata), NOT a comment"
    fi
  fi
  echo

  # ── (h) target-key repoint defense: each apply rule PINS FSM_TARGET_RE ──
  blue "Invariant (h): each apply rule (R-LABEL / R-VERSION / R-LINK) pins fsm_target_key MATCHES $FSM_TARGET_RE (T7 repoint defense)"
  dim  "  (invariant (e)'s bare 'FSM' grep is satisfied by a rule NAME / JQL branch; (h) proves the per-rule target constraint on customfield_10148.)"
  # ACCEPTED TOLERANT-MATCHING LIMIT (modeled on the (a) leaf-scan limit): scanning
  # each rule file's string LEAF VALUES (jq '.. | strings') for the pinned regex
  # literal proves the constraint is a condition/apply VALUE (not a KEY or a mention
  # in prose we mistook for a value). Until the Export-JSON schema is pinned it
  # cannot itself prove that leaf is specifically the `second` of a MATCHES condition
  # whose `first` is {{issue.customfield_10148}} — a human reviewer eyeballs on JSON
  # commit that the pinned regex guards the target key, not some unrelated field.
  # Fail-closed: a missing rule OR a rule whose leaves lack the pinned regex reddens.
  local hrule hname hfile hleaves
  for hrule in "R-LABEL:$R_LABEL_FILE" "R-VERSION:$R_VERSION_FILE" "R-LINK:$R_LINK_FILE"; do
    hname="${hrule%%:*}"; hfile="${hrule#*:}"
    if [ -z "$hfile" ]; then
      fail "$hname pins fsm_target_key regex" \
        "no $hname export identified — the T7 repoint-defense target regex $FSM_TARGET_RE is unverifiable for it; failing closed"
      continue
    fi
    hleaves="$(jq -r '.. | strings' "$hfile" 2>/dev/null)"
    if grep -qF -- "$FSM_TARGET_RE" <<< "$hleaves"; then
      pass "$hname pins the fsm_target_key regex $FSM_TARGET_RE as a condition/apply leaf value (T7 repoint defense present)"
    else
      fail "$hname pins fsm_target_key regex" \
        "no string-leaf value in $(basename "$hfile") contains the pinned target-key regex $FSM_TARGET_RE — $hname does NOT constrain fsm_target_key (customfield_10148) to ^FSM-[0-9]+\$, so a crafted repoint of customfield_10148 to an arbitrary / cross-project key would be applied to FSM by the Automation actor (T7 repoint hole). Add the MATCHES $FSM_TARGET_RE condition on customfield_10148."
    fi
  done
  echo

  # ── (i) Automation actor is never a bot ──
  blue "Invariant (i): each rule's Automation actor (.rules[].actor.value) is a NON-bot accountId (confused-deputy / outbox-isolation defense, gd-RigNkSZKE6)"
  dim  "  (the whole outbox model rests on apply running AS the dedicated Automation actor, NEVER a bot; the narrative asserts it (e.g. 'by the Automation actor (never a bot)') but nothing machine-checked it until now.)"
  # ACCEPTED TOLERANT-MATCHING LIMIT: (i) is a DENYLIST of the KNOWN bot accountIds (the
  # bare-UUID substring match, exactly like (b)/(h)), not an allowlist of one blessed
  # actor — so a legitimately-different Automation service account still passes. It proves
  # the run-as identity is not a KNOWN gardenio bot; a human reviewer eyeballs on JSON
  # commit that actor.value is the intended dedicated Automation account (not some other
  # privileged human). Fail-closed: a rule with a missing/empty actor.value, OR any rule
  # whose actor.value carries a bot UUID, reddens the gate. Every real fixture carries
  # exactly one rule; the verdict scans .rules[] across the file, so a multi-rule export
  # is covered too.
  local irule iname ifile iverdict
  for irule in "R-LABEL:$R_LABEL_FILE" "R-VERSION:$R_VERSION_FILE" "R-LINK:$R_LINK_FILE" "R-CANARY:$R_CANARY_FILE"; do
    iname="${irule%%:*}"; ifile="${irule#*:}"
    if [ -z "$ifile" ]; then
      fail "$iname Automation actor is a non-bot accountId" \
        "no $iname export identified — the Automation actor (.rules[].actor.value) is unverifiable for it; failing closed"
      continue
    fi
    iverdict="$(actor_bot_verdict "$ifile")"
    case "$iverdict" in
      OK)
        pass "$iname runs AS a non-bot Automation actor (.rules[].actor.value carries neither gardenio bot accountId)"
        ;;
      BOT:*)
        fail "$iname Automation actor is a non-bot accountId" \
          "$iname ($(basename "$ifile")) has an Automation actor (.rules[].actor.value) set to a gardenio BOT accountId ('${iverdict#BOT:}'). The rule would run — and write FSM — AS the bot: an OUTBOX-ISOLATION break (apply must happen as the dedicated Automation actor, never a bot) AND a confused deputy (R-CANARY, which alerts on ANY bot-account FSM write, would fire on the outbox's own applies). The actor MUST be the non-bot Automation service account, NEVER $BOT_ID_PREFIX$BOT_UUID_GARDENIO / $BOT_ID_PREFIX$BOT_UUID_GARDENIO2 (gd-RigNkSZKE6)."
        ;;
      MISSING)
        fail "$iname Automation actor is a non-bot accountId" \
          "$iname ($(basename "$ifile")) has a rule with a MISSING/EMPTY Automation actor (.rules[].actor.value) — the run-as identity is unverifiable, so it cannot be proven non-bot; failing closed. Every rule must declare its Automation actor.value."
        ;;
      *)  # NONE (no .rules[] / jq error) — fail closed.
        fail "$iname Automation actor is a non-bot accountId" \
          "$iname ($(basename "$ifile")) exposes no .rules[] actor.value to inspect (empty rules array or unparseable) — the Automation actor is unverifiable; failing closed."
        ;;
    esac
  done
  echo

  # ── (j) R-COMMENT′ (folded internal-note rule): internal-only comment, non-bot
  #        actor, reporter-is-bot guard, target-key pin ──
  blue "Invariant (j): R-COMMENT′ (folded internal-note rule) — internal-only comment, non-bot actor, reporter-is-bot guard, target-key pin (ka-0000002637)"
  dim  "  (R-COMMENT′ is a COMMENT rule, not a field-apply rule — EXEMPT from (a)'s op_value allowlist and (f)'s apply-error sinks, same exemption rationale as R-LINK.)"
  if [ -z "$R_COMMENT_FILE" ]; then
    fail "R-COMMENT′ rule present" \
      "no R-COMMENT′ export identified (need a rule whose trigger carries triage_internal_note_full and whose comment action reads customfield_10156 via a jira.issue.comment action) — the folded internal-note rule is unverifiable; failing closed"
  else
    # (j1) LOAD-BEARING: the comment MUST be internal — a publicComment:false leaf
    # is present, and NO public marker (publicComment:true / 'reply to customer' /
    # 'share with customer') appears anywhere in the file. This ties directly to
    # (c)'s corpus-wide no-public-comment intent, scoped to this rule's own leaves
    # so a drift here is pinpointed rather than only caught by the corpus-wide scan.
    # Whitespace/case are normalized away first (same technique (c) uses on the
    # whole corpus), so this is tolerant of pretty-printed vs compact JSON and
    # does not depend on a specific key:value spacing convention.
    local jcomment_compact
    jcomment_compact="$(tr -d ' \t\n\r' < "$R_COMMENT_FILE" | tr '[:upper:]' '[:lower:]')"
    if grep -qF -- '"publiccomment":false' <<< "$jcomment_compact" \
       && ! grep -qF -- '"publiccomment":true' <<< "$jcomment_compact" \
       && ! grep -qF -- 'replytocustomer' <<< "$jcomment_compact" \
       && ! grep -qF -- 'sharewithcustomer' <<< "$jcomment_compact"; then
      pass "R-COMMENT′ comment action is INTERNAL (publicComment:false present; no publicComment:true / 'reply to customer' / 'share with customer' marker)"
    else
      fail "R-COMMENT′ comment is internal-only" \
        "$(basename "$R_COMMENT_FILE") does not prove the comment action is internal-only — expected a \"publicComment\":false leaf and NO public marker (publicComment:true / 'reply to customer' / 'share with customer'). A customer-facing note here would breach the notification-safety guarantee (invariant #6 / (c))."
    fi

    # (j2) Automation actor is never a bot — reuses the SAME actor_bot_verdict
    # predicate invariant (i) calls, so the two checks cannot drift.
    local jactor
    jactor="$(actor_bot_verdict "$R_COMMENT_FILE")"
    case "$jactor" in
      OK)
        pass "R-COMMENT′ runs AS a non-bot Automation actor (.rules[].actor.value carries neither gardenio bot accountId)"
        ;;
      BOT:*)
        fail "R-COMMENT′ Automation actor is a non-bot accountId" \
          "R-COMMENT′ ($(basename "$R_COMMENT_FILE")) has an Automation actor (.rules[].actor.value) set to a gardenio BOT accountId ('${jactor#BOT:}') — the same outbox-isolation break / confused deputy invariant (i) guards against on the apply rules (gd-RigNkSZKE6)."
        ;;
      MISSING)
        fail "R-COMMENT′ Automation actor is a non-bot accountId" \
          "R-COMMENT′ ($(basename "$R_COMMENT_FILE")) has a MISSING/EMPTY Automation actor (.rules[].actor.value) — the run-as identity is unverifiable, so it cannot be proven non-bot; failing closed."
        ;;
      *)
        fail "R-COMMENT′ Automation actor is a non-bot accountId" \
          "R-COMMENT′ ($(basename "$R_COMMENT_FILE")) exposes no .rules[] actor.value to inspect (empty rules array or unparseable) — the Automation actor is unverifiable; failing closed."
        ;;
    esac

    # (j3) reporter-is-bot confused-deputy guard, POSITIVE polarity — reuses the
    # SAME reporter_is_bot_verdict predicate invariant (d) calls, so the two checks
    # cannot drift.
    local jreporter
    jreporter="$(reporter_is_bot_verdict "$R_COMMENT_FILE")"
    case "$jreporter" in
      OK)
        pass "R-COMMENT′ carries the reporter-is-bot condition with POSITIVE polarity ($REPORTER_POS_CHECK, both bot accountIds referenced)"
        ;;
      BAD_POLARITY:*)
        fail "R-COMMENT′ reporter-is-bot condition polarity" \
          "R-COMMENT′ ($(basename "$R_COMMENT_FILE")) has a reporter user-condition with a NON-positive check '${jreporter#BAD_POLARITY:}' (expected $REPORTER_POS_CHECK) — the confused-deputy guard turned inside out, same hazard invariant (d) guards against on the apply rules."
        ;;
      MISSING_BOTH)
        fail "R-COMMENT′ reporter-is-bot condition" \
          "R-COMMENT′ ($(basename "$R_COMMENT_FILE")) has a $REPORTER_POS_CHECK reporter condition but does NOT reference both bot accountIds ($BOT_ID_PREFIX$BOT_UUID_GARDENIO / $BOT_ID_PREFIX$BOT_UUID_GARDENIO2) — the guard must admit BOTH gardenio bots, else one bot's tickets silently never propagate."
        ;;
      *)
        fail "R-COMMENT′ reporter-is-bot condition" \
          "R-COMMENT′ ($(basename "$R_COMMENT_FILE")) carries NO reporter jira.user.condition — the reporter-is-bot confused-deputy condition is missing; with GFO site-public an unprivileged reporter could file an Analysis ticket whose note the rule posts to FSM as the Automation actor."
        ;;
    esac

    # (j4) fsm_target_key pinned ^FSM-[0-9]+$ — same leaf-scan style as (h), scoped
    # to R-COMMENT′'s own file only (R-COMMENT′ is deliberately NOT added to (h)'s
    # apply-rule loop, since it is a comment rule, not a field-apply rule).
    local jleaves
    jleaves="$(jq -r '.. | strings' "$R_COMMENT_FILE" 2>/dev/null)"
    if grep -qF -- "$FSM_TARGET_RE" <<< "$jleaves"; then
      pass "R-COMMENT′ pins the fsm_target_key regex $FSM_TARGET_RE as a condition leaf value (T7 repoint defense present)"
    else
      fail "R-COMMENT′ pins fsm_target_key regex" \
        "no string-leaf value in $(basename "$R_COMMENT_FILE") contains the pinned target-key regex $FSM_TARGET_RE — R-COMMENT′ does NOT constrain fsm_target_key (customfield_10148) to ^FSM-[0-9]+\$, so a crafted repoint of customfield_10148 to an arbitrary / cross-project key would have its note posted to FSM by the Automation actor (T7 repoint hole)."
    fi
  fi
  echo
}

main() {
  # Inner reconciliation-only run for self_test_newshape: no banners, no self-tests
  # (so there is no unbounded recursion — the inner instance runs ONLY the artifact
  # reconciliation against the OUTBOX_RULES_FIXTURES_DIR override).
  if [ "${OUTBOX_SELFTEST_INNER:-}" = "1" ]; then
    reconcile_artifact
    [ "$FAIL" -eq 0 ]
    return
  fi

  blue "═══════════════════════════════════════════════════"
  blue " Outbox rule-JSON reconciliation gate (FAIL-CLOSED)"
  blue " gd-dq6raBnxLV / epic ka-0000002067 / reshape ka-0000002637"
  blue "═══════════════════════════════════════════════════"
  dim  " fixtures: $FIXTURES_DIR"
  echo

  self_test_allowlist
  self_test_apply_error
  self_test_scope
  self_test_reporter_polarity
  self_test_newshape
  reconcile_artifact

  # ── Summary ─────────────────────────────────────────────────────
  blue "═══════════════════════════════════════════════════"
  local total=$((PASS + FAIL))
  if [ "$FAIL" -eq 0 ]; then
    green " ✓ All $total checks passed — exported rule JSON reconciles with the in-repo invariants"
  else
    red " ✗ $FAIL/$total checks failed"
    echo
    for err in "${ERRORS[@]}"; do red "   - $err"; done
    echo
    dim " If the only failure is 'operator rule-JSON export present', this gate is"
    dim " working as designed: it is RED because the operator has not yet committed"
    dim " the exported Automation rule JSON. It goes GREEN once fixtures/outbox-rules/"
    dim " carries a faithful Export JSON that satisfies invariants (a)-(j)."
  fi
  blue "═══════════════════════════════════════════════════"

  [ "$FAIL" -eq 0 ]
}

main "$@"
