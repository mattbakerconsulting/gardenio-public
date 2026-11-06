# `outbox-rules/` — operator-exported Jira Automation rule JSON

**Export LANDED 2026-07-11 (ka-0000005630 / gd-FHX7Ozb7rt): the four files below are genuine
Jira UI `Actions → Export JSON` artifacts of the live sandbox rules, exported from the
operator's authenticated admin session immediately after the 1:1-reshape build + live smoke
(headline multi-op pass + evil-label fail-closed + non-bot-reporter + idempotent re-triage all
green on FSM-440/FSM-441 · GFO-53/54/55). Do NOT hand-edit them — re-export from the UI on any
rule change.**

## What lands here

The operator-owned Jira Automation rules that implement the FSM **outbox** security model
(epic [[ka-0000002067]], rule build [[ka-0000004008]]), in the **1:1-reshape** shape (epic
[[ka-0000002637]], Phase 4 [[ka-0000003716]]). For each rule the operator exports its JSON
from the Jira UI — **Global automation → open the rule → Actions → Export JSON** — and commits
the file here, one file per rule:

| File             | Rule      | Live sandbox rule id (2026-07-11 build)  | Role (single-Analysis-ticket shape) |
|------------------|-----------|------------------------------------------|-------------------------------------|
| `R-LABEL.json`   | R-LABEL   | `019f54b5-c7bb-7c1d-86b9-65307c185308`   | GBFSM Analysis **labels field** (customfield_10153) → append EACH allowlisted label to FSM (whole-field fail-closed allowlist guard, Option A) |
| `R-VERSION.json` | R-VERSION | `019f54c6-31cd-7a3e-b13b-b43e61657f10`   | GBFSM Analysis **version field** (customfield_10154) → ensure-then-set FSM Affects-Version |
| `R-LINK.json`    | R-LINK    | `019f54d2-b46d-70f6-9b6b-131e7c44986a`   | GBFSM Analysis **fsm_target_key** → `relates to` FSM↔GBFSM issue link on FSM-N (Phase 3; field metadata, fires NO customer email; NO Synced/Failed transitions) |
| `R-CANARY.json`  | R-CANARY  | `019f4025-d02c-7a50-9fde-417511b234bf`   | Alerts on ANY direct bot-account FSM write (must never fire; scope re-pinned FSM 10038 — was phantom 10036) |

Sandbox scaffold ids the rules read from (captured at build, for the Phase 5/6 bridge wiring —
`JIRA_OUTBOX_ANALYSIS_ISSUE_TYPE` / `JIRA_OUTBOX_LABELS_FIELD` / `JIRA_OUTBOX_VERSION_FIELD`):
**Analysis issue type = `10043`**, **labels field = `customfield_10153`** (`triage_labels`,
Labels type), **version field = `customfield_10154`** (`triage_version`, short text). The three
pre-reshape one-op rules (old R-LABEL/R-VERSION/R-COMMENT on the Add-Label/Set-Version/
Internal-Comment intent types) remain live-but-inert (nothing creates those intent types
anymore); retire them at the operator's convenience.

**The 1:1 reshape (why the shape changed).** Phase 2 ([[ka-0000001491]]) COLLAPSED the writer:
instead of a fan-out of per-op intent tickets (Add-Label / Set-Version / Internal-Comment as
separate issues), gardenio now upserts **ONE GBFSM "Analysis" ticket per FSM ticket** carrying
every op as FIELDS (the N triage labels in a labels field, the recommended Affects-Version in a
version field, the analysis in the Description, `fsm_target_key` = customfield_10148 as the
authoritative link + upsert key). Phase 4 adapts the rules to read MULTIPLE ops from that ONE
ticket. Consequences for this fixtures set:

- **The old `R-COMMENT.json` (Internal-Comment intent → FSM internal note) RETIRES.** The
  analysis lives on the GBFSM ticket, reached from FSM via the R-LINK link; the Description is
  no longer posted to FSM as a comment. Any OPTIONAL human-readable note on FSM must stay
  INTERNAL, rule-hardcoded — the gate's check (c) fails closed on ANY public comment marker.
- **`R-LINK.json` is NEW and REQUIRED** — it replaces the retired plain-text Internal-Comment
  link-back with a real clickable FSM→GBFSM link (the Phase-3 [[ka-0000001070]] FSM half).
- The live sandbox rule ids differ from the pre-reshape ones (the operator adapts the existing
  rules by **duplication + edit**, per Matt's "don't rebuild from scratch" constraint) —
  **capture the new live rule ids into this table when the export lands.** Prod GBFSM gets its
  own project + field + rule ids at cutover ([[ka-0000007073]]).

File names are matched **tolerantly** by the verifier: any `*.json` whose content carries a
rule's discriminating apply-action / name literal is recognized as that rule regardless of exact
filename, so the operator may export with Jira's default names. The names above are the
recommended convention.

## Why the JSON is the security-critical artifact (and why it is operator-exported)

Rule creation has no supported REST API and multi-project scope requires Jira admin, so the
rules are built by an agent **through the operator's authenticated browser session** and the
**exported JSON is the reviewed source of truth** committed to the repo. The rules apply ops to
the customer-facing FSM project **as the Automation actor** (never as the bot), so a drifted or
hand-authored rule is a confused-deputy / silent-drop hazard. Two guardrails follow:

1. **Do NOT hand-author these files.** A hand-written substitute would diverge from the live
   rule while looking plausible in review — exactly the failure the gate exists to catch. The
   only legitimate content is a genuine Jira `Export JSON` of the live rule.
2. **The gate is fail-closed.** With no real export committed, `verify-outbox-rules-json.sh`
   exits non-zero (see below). That is the whole point: the manual-review risk becomes a
   machine-enforced RED signal in the scheduled E2E suite. The real export **LANDED
   2026-07-11** (the four files above), so the gate now reconciles GREEN — but the
   fail-closed contract is unchanged: remove the artifact and the gate reddens again.

## The gate: `../../scripts/verify-outbox-rules-json.sh`

The `outbox-rules-json` E2E module reconciles the exported JSON against the security invariants
already pinned in-repo. It is **fail-closed**:

- **No `*.json` present** → the verifier exits **non-zero**. An empty directory keeps the
  scheduled E2E suite RED — the intended "awaiting operator export" signal. It
  is registered as a hash-assigned full-suite module (NOT part of the fast main-push smoke
  subset), so it gates the scheduled/full E2E without blocking main-push. (The four real
  rule JSONs landed 2026-07-11, so the dir is NO LONGER empty and this branch no longer
  fires — the `JSON present` branch below does; the empty-dir contract stays as the
  fail-closed backstop if the artifact is ever removed.)
- **JSON present (today — the four exported rules above)** → the verifier asserts the semantic
  invariants, all already pinned in-repo (never re-derived here):
  - **(a)** the **R-LABEL** rule's `op_value` allowlist regex is `ALLOWLIST_RE` (the tight,
    `triaged-`-infix-carrying allowlist from the shared shape library `lib-outbox-shape.sh`) — and the no-infix
    DRIFT literals (`gardenio-sev-`, `gardenio-gxp-`) are absent everywhere. In the collapsed shape
    the allowlist is applied **element-wise** over the multi-value labels field via a smart-value
    expression, so `ALLOWLIST_RE` appears as a **substring of a leaf value** (not a standalone
    leaf as in the retired per-op `matches ALLOWLIST_RE` condition); (a) scans the **R-LABEL
    file's leaf VALUES** (`jq '.. | strings'`) for it, so a benign mention in another rule's prose
    can't false-pass. Catches the [[gd-YdG7ap12oB]] no-infix drift that would silently dead-letter
    every real sev/GxP classification;
  - **(b)** R-CANARY carries **both** bot accountIds
    (`712020:66bb0993-fdcf-4652-8941-d4ac9a81b093` = gardenio@,
    `712020:a2f3e121-ea2e-48d1-aa6e-241713778a2b` = gardenio2);
  - **(c)** **NO** rule carries a PUBLIC / "Share with customer" comment marker
    (`jsdPublic=true` / `visibility:public` / `sd.public.comment.public`) anywhere in the corpus —
    the customer-notification-safety guarantee (invariant #6). The Description is no longer posted
    to FSM as a comment; R-LINK posts a **link** (field metadata, no visibility concept, no
    customer email). Any optional internal note must be INTERNAL, so any public marker fails closed;
  - **(d)** each apply rule (R-LABEL / R-VERSION / R-LINK) carries the **reporter-is-bot**
    condition with the correct **POLARITY** — the v4 confused-deputy guard, T11. With the real
    Export-JSON schema now pinned in-repo (the four files above), the guard is a
    `jira.user.condition` whose `value.conditions[]` carries
    `{ "field": "reporter", "check": "USER_IS", "criteria": [{…both bot ids…}] }`. (d) no longer
    merely greps both accountIds — it asserts the reporter condition's `check` is the **positive**
    `USER_IS` **and** references **both** bots, and **fails closed** on any other check. The
    dangerous inversion `check: "USER_IS_NOT"` (reporter is **NOT** a bot, same ids) is the
    confused-deputy guard turned inside out — it would propagate an **unprivileged** reporter's
    Analysis ticket to FSM while **blocking** the legitimate bot-reportered one. The old
    both-ids-present grep false-passed that inversion; the polarity assertion closes it
    ([[gd-zKkSOkK6df]]). **Residual** a human still eyeballs on JSON commit: that the condition
    sits in the rule's active if-block guard (not a disabled/dangling component);
  - **(e)** the rule **scope** names GFO project id **10037** (source/trigger side) **and** the
    **real FSM** project id **10038** (apply/target side), **and the phantom id `10036` appears
    NOWHERE** in the corpus — the [[gd-l0ffwDFvQ8]] false-Synced defense. The prod defect scoped the
    3 apply rules **{GFO 10037, PHANTOM 10036}** with FSM **10038 ABSENT** (10036 does not exist — the
    Jira scope UI rendered it an "Unavailable value"), so the JQL branch
    `key = {{issue.customfield_10148}} = FSM-N` found **no in-scope target** and every apply
    **silently no-oped** while the intent **still transitioned `Pending → Synced`** (false-Synced).
    The bare literal `FSM` (a rule **name** / JQL branch / target regex) is **not** sufficient — it
    false-passes on the broken scope; only the real id **10038** proves the scope resolves. (e) also
    pins **R-VERSION's `jira.version.create` target ("Space") to FSM** (Matt reported that action was
    *independently* fat-fingered to `10036`). **Tolerant-matching limit** (like (a)/(f)/(h)): the
    Export-JSON scope-resource schema is not yet pinned, so (e) is a **conservative fail-closed**
    set-membership check over whole-corpus **whole-number-token** lookups (each id must appear as a
    complete number, not embedded in a longer customfield/version number) — it **passes only** on an
    unambiguously-correct **{GFO 10037, FSM 10038}** scope with no 10036 (a GFO-only / phantom-scoped /
    no-10038 export stays **RED**), but cannot itself prove those ids sit specifically in the scope
    ARI (vs an unrelated field whose value equals 10038) — **a human reviewer must still eyeball** that
    on JSON commit (and that the R-VERSION version-create `FSM` sits in the target Space, not the
    version name);
  - **(f)** each **field-apply** rule (R-LABEL / R-VERSION) transitions the Analysis ticket to
    **Failed on the apply-action's error path** and does **not** unconditionally flip
    `Pending → Synced` regardless of apply outcome — the structural fix demanded by P0
    [[gd-l0ffwDFvQ8]] / bug [[gd-d7U7jd6cyH]] ("ADD AN ERROR BRANCH… so Synced can NEVER again be a
    false-success signal"). **R-LINK is EXEMPT** — an issue/web link is idempotent field metadata
    with no Synced/Failed transition and no "Synced != applied" risk. **Tolerant-matching limit:**
    the historical *buggy* rule already carried **one** `Transition → Failed` on its validation-else
    branch, so a naive "has a Failed" grep false-passes on a still-broken export. (f) instead counts
    distinct transition-**action** sinks by target status and passes only when a rule has **≥2 Failed
    sinks and ≥1 Synced sink** (validation-reject Failed **plus** a second apply-error Failed, vs the
    single happy-path Synced) — the broken shape (1 Failed / 1 Synced, or a status-name-less id-only
    export) stays RED. It is a **conservative fail-closed** check: because the Export-JSON schema is
    not pinned, it proves a *distinct error-path Failed exists* but cannot itself prove that second
    Failed is structurally gated on the apply action's error/false branch — **a human reviewer must
    still eyeball that** on JSON commit;
  - **(g)** the **R-LINK** rule EXISTS, references `fsm_target_key` (customfield_10148) + FSM, and
    carries a link action (issue-link / web-link) — the FSM→GBFSM clickable back-link (Phase 3
    [[ka-0000001070]]) is machine-verified present, applied by the Automation actor (never a bot),
    as field metadata (no customer email);
  - **(h)** each **apply** rule (R-LABEL / R-VERSION / R-LINK) **pins the `fsm_target_key` regex**
    `^FSM-[0-9]+$` as a `MATCHES`/comparison condition **value** on `customfield_10148` — the **T7
    repoint defense** (source of truth: `test-jira-outbox-negative.sh` T7 case). Invariant (e) only
    greps the bare literal `FSM` corpus-wide, which a rule **name** (`GBFSM R-LABEL`) or a JQL branch
    trivially satisfies — it does **not** prove the per-rule target constraint. Without (h) a crafted
    repoint of `customfield_10148` to an arbitrary / cross-project key would be applied to FSM by the
    Automation actor. (h) scans each rule file's string **leaf values** (`jq '.. | strings'`, like (a))
    for the pinned regex literal and **fails closed** if any apply rule lacks it. **Tolerant-matching
    limit** (like (a)): the leaf-scan proves the pinned regex is a condition/apply *value* (not a key
    or prose), but until the Export-JSON schema is pinned it cannot itself prove that leaf is
    specifically the `second` of a `MATCHES` condition whose `first` is `{{issue.customfield_10148}}`
    — **a human reviewer must still eyeball** on JSON commit that the pinned regex guards the target
    key;
  - **(i)** each rule's **Automation actor** (`.rules[].actor.value` — the identity the rule **runs
    as**) is a **non-bot** accountId: non-empty and **neither** gardenio bot accountId
    (`712020:66bb0993-…` = gardenio@, `712020:a2f3e121-…` = gardenio2). The whole outbox model rests on
    apply running **as the dedicated Automation actor, never as a bot** — the narrative asserts this
    throughout (e.g. "applied by the Automation actor (never a bot)") but nothing machine-checked it
    until now ([[gd-RigNkSZKE6]]). A rule that ran **as a bot** would write FSM under a bot identity
    (an **outbox-isolation** break) **and** trip **R-CANARY** — which alerts on ANY bot-account FSM
    write — on the outbox's **own** applies (a **confused deputy**). (i) **fails closed** if any
    rule's `actor.value` is missing/empty **or** contains either bot UUID (bare-UUID `contains`
    substring match, exactly like (b)/(h)). All four real fixtures share the single non-bot actor
    `557058:f58131cb-…` (type `ACCOUNT_ID`). **Tolerant-matching limit:** (i) is a **denylist** of the
    known bot ids, not an allowlist of one blessed actor — it proves the actor is not a **known** bot,
    but **a human reviewer must still eyeball** on JSON commit that `actor.value` is the intended
    dedicated Automation service account (not some other privileged human).

Because the Jira Automation Export-JSON schema is not yet pinned in-repo, the verifier asserts
these as **tolerant lookups** over the exported structure rather than a brittle exact-shape
schema. The security-critical op_value check (a) scans JSON string **leaf values**
(`jq '.. | strings'`) so the allowlist regex must be an actual condition/apply *value*, not merely
a substring of a description/comment; the coarser signals (accountId, project id, FSM scope) use
whole-corpus literal lookups. When the schema is later pinned, the lookups can be tightened to
exact key paths.

Five **always-on self-tests** run before the artifact gate (so the module stays non-vacuous
regardless of the fixtures dir — they would still run even if this dir were empty): the
ALLOWLIST_RE drift guard, the (f) discriminator soundness proof
(synthetic broken shape FAILS, corrected shape PASSES), the **(e) scope discriminator** soundness
proof (synthetic broken prod scope `{GFO 10037, PHANTOM 10036}`/no-10038 FAILS, corrected
`{GFO 10037, FSM 10038}` PASSES, plus whole-number-token anchoring: a correct scope citing a
customfield that embeds `10036` still PASSES and a broken scope where `10038` appears only inside a
longer customfield number stays RED), the **(d) reporter-polarity discriminator** soundness proof
(a synthetic POSITIVE reporter condition `check: "USER_IS"` with both bot ids PASSES, and the
INVERTED `check: "USER_IS_NOT"` with the same ids FAILS — the confused-deputy-inside-out hole the old
both-ids grep missed; plus legacy-flat-shape, single-bot, no-reporter-condition, and a **split** two-OR'd-single-id
positive shape that must still PASS — the both-ids requirement is a file-wide union over positive reporter
conditions, so a faithful alternate export form is not false-failed), and a
**new-shape reconciliation** proof that runs the verifier recursively against a synthetic FAITHFUL
single-Analysis-ticket export (GREEN) and five drifted variants (RED): the [[gd-YdG7ap12oB]] no-infix
allowlist drift (invariant (a) catch), a **target-key repoint** that strips the `^FSM-[0-9]+$` pin from
one apply rule (invariant (h) catch), a **scope repoint** from the real FSM id `10038` to the phantom
`10036` across every rule (invariant (e) catch, the [[gd-l0ffwDFvQ8]] false-Synced defect), a
**reporter-polarity inversion** (`check` → `USER_IS_NOT`, both bot ids kept) in one apply rule
(invariant (d) catch, the [[gd-zKkSOkK6df]] polarity hole), and an **actor-is-bot repoint** that sets
one apply rule's `actor.value` to a gardenio bot accountId (invariant (i) catch, the [[gd-RigNkSZKE6]]
confused-deputy / outbox-isolation hole) — all in private mktemp dirs, never touching this
fail-closed dir.

**Export one file per rule** (open each rule → Actions → Export JSON). A single combined file
carrying several rules still fails closed on any public marker but muddies per-rule isolation;
per-rule files keep the assertions unambiguous.

## Provenance

- Epic: [[ka-0000002067]] (outbox pivot) · 1:1 reshape epic: [[ka-0000002637]] · Rule build:
  [[ka-0000004008]] · Phase 4 (this shape): [[ka-0000003716]]
- This gate: [[gd-dq6raBnxLV]] · Drift escalation this gate defends: [[gd-YdG7ap12oB]]
- Apply-bug (Synced != applied): [[gd-l0ffwDFvQ8]] / [[gd-d7U7jd6cyH]]
- Invariant source of truth: `gardenio/tests/e2e/scripts/test-jira-outbox-negative.sh` (live T7 /
  apply matrix) and `gardenio/tests/e2e/scripts/lib-outbox-shape.sh` (the shared `ALLOWLIST_RE`
  the drift guard reconciles against)
