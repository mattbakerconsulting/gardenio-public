# Steward as land operator (amendment to Lands Track B §4, Phase 9)

Status: DRAFT for the owner, 2026-10-23. Amends
`2026-10-19-lands-track-b-design.md` §4 and §7.3. Nothing in Phase 7, 8, 10 or
11 changes. Written by the laptop operator at the owner's request after two
observations the same evening: "stewards feel like the missing piece", and
"the Commons is quite slow and very noisy, and barely usable".

## 0. Summary

The spec's steward (§4.2) is a dispatcher: it greets, turns a request into a
plan, spawns crews within the purse, posts a digest and parks at the wall. That
is the right skeleton and it stays. What it lacks is the part of the laptop
operator's job that makes a land livable for a person: the steward should be
the land's **operator**, the one voice that talks to the tenant, the one that
reads everything the machines say so the tenant never has to.

Five additions, each a small change to §4.2–§4.3:

1. **Single voice.** Agents report to the steward, never to the tenant. A
   land's `#general` is people plus the steward. Machine traffic goes to a
   channel the tenant can open but does not have to.
2. **Memory.** A notes bead per land that the steward reads on every wake and
   writes at the end of every run: holds, the owner's past answers, what is in
   flight, what it promised.
3. **Delegated authority.** The tenant grants the steward what it may do
   alone (merge on green, release, answer a crew's escalation) as an explicit
   setting with a safe default, instead of the spec's flat "it does not merge".
4. **Stewardship of its crews.** Liveness (a crew that loops, a crew that
   died), escalation triage (answer what it can, bring the tenant only the
   calls that are the tenant's), and the kill switch on a crew that misbehaves.
5. **Plain words.** Every message to a person is written for a person: what
   happened, what it means for them, what (if anything) they need to do.

And two decisions the owner took tonight that this amendment records:

- **The Commons is closed by default** in `/garden` (owner, 2026-10-23). The
  pane opens when a person opens it; it no longer reopens because it was open
  last time. Noise and speed, measured below, are the reasons.
- **Keycloak admin path for the provisioner: option A** (decision
  gd-F060vJkxoM): http is accepted for the exact in-cluster Keycloak Service
  host only; in-cluster TLS is a Phase 10 follow-up.

## 1. What the operator does that the dispatcher does not

The laptop operator's job, as it has run for the gardenio land since October,
in the order the time goes:

| Duty | What it looks like in a day | In §4.2 today |
|---|---|---|
| Read everything, say little | ~3,600 machine messages a day in; ~12 plain updates out, plus the questions that are genuinely the owner's | absent |
| Keep memory | holds (never merge #504), past answers (passkey-only admin), promises (re-run the VAP check in the provisioner roll window) | absent: each run renders a fresh snapshot |
| Act within delegated authority | merge on green, cut and roll releases, close escalations the owner has already answered in principle | "does not merge PRs"; everything else to the owner |
| Watch the crews | a crew that loops 13 h on a missing label; a groomer that woke three executors; a desk run holding a lease | absent |
| Enforce rules | no secrets in beads, confirm before anything outward-facing, decisions through decision beads | partly: the tier (§5.1) enforces the fence, nobody enforces conduct |

The dispatcher parts of §4.2 (greet, plan, spawn within the purse, digest,
park) are kept as written.

## 2. Single voice

**Rule.** In a land, a person hears from the steward and from other people.
Crews, gardeners, the controller and the bridge do not post into `#general`.

**Mechanics.**

- Every land gets a second channel, `#machines` (name open), with
  `mirror` unset. Everything that today posts into `#general` by role
  (`crew`, `gardener`, the controller's death reports, the trigger engine's
  squawks, escalation executors) posts there instead. The channel is in the
  Commons list, so a curious tenant can read it; it is not the default view
  and carries no unread badge in the rail.
- The steward reads `#machines` (it is one of its wake sources) and writes to
  `#general` only what a person needs: a plan ready to approve, a PR merged, a
  crew stuck and what it did about it, a question that is the tenant's to
  answer. Target: under ten steward messages a day on a busy land; a quiet
  day posts nothing (the digest rule, §4.2 item 4, already says this).
- Decision and escalation cards addressed to a person stay in `#general`:
  they are the tenant's to answer. Cards addressed to the steward
  (a crew asking its steward) go to `#machines`.
- Slack mirroring is unchanged: `mirror` on `#general` keeps working as
  today; `#machines` is never mirrored.

**Measured reason.** On 2026-10-22/23, in gardenio's Commons, 3,581 of 3,592
messages in 24 hours were machine-authored (slack-bridge 2,547, controller
446, plan gardeners 243, crews 212, trigger engine 47, escalation executors
45); 11 were from a person. `#general` alone holds 2,621 messages since
2026-10-20, about 1,000 a day.

**Gardenio itself** gets the same split as the first land to use it: the
operator's own `#machines`, the roles' default channel moved, and the Commons
closed by default (already decided). This is the dogfood for every rule above
and does not wait on Phase 9.

## 3. Memory

A `type:note` bead per land, `steward-notes:<land>`, owned by the steward,
inside the land's scope, with sections the playbook names:

- **Holds.** Things the tenant said never to do, or not yet ("hold PR 12
  until I've read it"). Read before every merge, release or spawn.
- **Answers.** Past decisions of the tenant, one line each, newest first, with
  the decision bead id. The steward does not ask the same question twice.
- **Promises.** What the steward said it would do and when. Checked on every
  wake; an overdue promise is the first thing the next run does.
- **In flight.** Plans, PRs and crews, one line each, so a run that starts
  from an event has the picture without reading `#machines` from the top.

Written at the end of every run (the playbook's last step), capped at a size
the prompt can carry (target 4 KB), oldest **Answers** rolled into the land's
decision history when it overflows. The prompt (`StewardPlaybookPrompt`,
§4.3) renders it in full, before the land snapshot.

This is the stand-in for the operator's continuous session: a tenant's
steward is event-driven and short-lived (cost, §6), and memory is what makes
twenty short runs behave like one long one.

## 4. Delegated authority

A land field `steward_authority` on the project bead, set by the tenant from
the land header (and by the operator at provisioning), one of:

| Value | The steward may, alone | It brings to the tenant |
|---|---|---|
| `ask` (default) | spawn within the purse, answer a crew's questions, triage, kill a crew | every merge, every release, every plan |
| `merge-on-green` | the above, plus merge a crew's PR that is green and the steward has read | releases, plans |
| `operate` | the above, plus roll releases and close escalations whose answer the tenant gave before (memory, §3) | plans, and anything not covered by a past answer |

The spec's "it does not merge PRs: the tenant merges on GitHub" becomes the
`ask` default. The fence (§5) is unchanged: whatever the grant, the tier
decides what the steward's token can do, and `operate` never reaches outside
the land.

Every action under a grant is written to `#machines` with the grant named
("merged #12 under merge-on-green"), so the tenant can audit it and lower the
grant at any time.

## 5. Stewardship of crews

Three duties, added to the playbook as wake sources and steps:

- **Liveness.** On every wake the steward lists the land's live agents and
  their last change. A crew with no bead change in N minutes (default 45) and
  no open escalation is asked once in `#machines`; with no answer by the next
  wake it is killed and its step reopened with a note. A crew that has
  restarted more than three times on one step is killed and the step marked
  `blocked` with the reason. (gardenio example: a crew looped 13 hours on a
  missing project label; a steward with this rule stops it at the first wake.)
- **Escalation triage.** A crew's escalation goes to the steward first. The
  steward answers what memory or the grant covers, kills what is unsafe, and
  forwards to the tenant only the calls that are the tenant's, rewritten in
  plain words with the steward's recommendation. The audience field from the
  escalation-routing plan (gd-KAX4iUAI42) carries `steward` as a value.
- **Kill switch.** `gb agent kill` within the land, always, whatever the
  grant. Logged to `#machines`.

## 6. Cost

The laptop operator is a long, high-context Opus session. A tenant's steward
cannot be that (tenant models decision gd-Se3rF0T5pu: Sonnet 5.5 by default,
Opus per land at approval). So:

- Event-driven, as §4.3 says, with the daily run cap (24) kept.
- A run is short: memory (§3) first, then the event, then the land snapshot,
  then at most one of: plan, spawn, triage, digest, park.
- `#machines` is read by delta (since the last run's high-water mark, kept in
  memory), never from the top.
- Target: a steward run under 30k input tokens on Sonnet; a busy land under
  $2 a day of steward time, charged to the purse like any other agent.

## 7. The Commons: closed by default, and the speed numbers

Decided by the owner 2026-10-23: the Commons pane in `/garden` is **closed by
default**. It opens when a person opens it (the rail button, the phone tab, a
deep link into a channel or thread) and does not reopen on the next visit
because it was open on the last one. PR `laptop/commons-closed-by-default`
makes that change (one branch in `web/commons.js`); the stored `garden.cm.open`
flag goes away.

Measured 2026-10-23 ~01:50Z in the owner's Chrome, cold load of `/garden`
on 2026.296.1 (bug gd-h2GZsjqwA7 has the full table):

| Request | Server wait | Size |
|---|---|---|
| `/garden` document | 1.16 s | 797 KB decoded, 4,029 DOM nodes |
| `/garden/farm` | 0.15 s | 173 KB |
| `/garden/c/gardenio/general` (timeline) | **3.1 s, then 3.0 s again** | 151 KB |
| `/garden/c/unread` | **1.0–1.9 s**, 8 times in 55 s | 3.5 KB |
| `/garden/events`, `/garden/farm/events` (SSE) | both closed at ~11 s | |

Closed by default removes the two timeline fetches from a plain `/garden`
visit (a 6-second saving on this load) and is the cheapest fix. The rest is
the bug's work: timeline and unread under 300 ms and 100 ms, SSE that stays
open, one fetch per view. The edge is not what closes SSE (its read timeout
is 3600 s on those locations); the viewer, the auth proxy or the page's own
route change are the suspects.

## 8. Steps (changes to §7.3)

Phase 9 keeps its ten steps. Their text changes as below; two steps are added.

- **9.0 (new) Single voice in gardenio.** `#machines` channel; roles'
  default channel moved; death reports, squawks and executors post there;
  `#general` left to people and the operator. Commons closed by default
  (its own PR, already open). Acceptance: 24 h with `#general` under 75
  machine messages (re-baselined from 50 by owner decision gd-XNWvRpYL4c,
  2026-10-27: what remains is addressed to people), and, as hard
  requirements, 0 comment relays, 0 desk cards and 0 top-level crew cards
  there.
- **9.1 Steward roles** also carries the `steward_authority` field and its
  three values; the `lands-tenant` tier allows `gb agent kill` within the land.
- **9.2 Land-bound gardener** adds `#machines` (delta) and the memory bead as
  wake sources and prompt inputs.
- **9.2a (new) Steward memory.** The notes bead, its four sections, the
  end-of-run write, the size cap, the rollover. Tests: a hold is honoured
  across two runs; a promise overdue is acted on first.
- **9.3 First greeting** names the grant in force and how to change it.
- **9.4 Conversation to plan** unchanged.
- **9.5 Spawn within the purse** adds the liveness rules (§5) and the kill
  switch.
- **9.6 Digest** is the only scheduled post; it is written for a person (§0
  item 5) and reads memory's **In flight**.
- **9.7 Parking** unchanged.
- **9.8 On the map** unchanged; the reason text comes from memory.
- **9.9 Check-in on the 8.10 land** adds: a looping crew is caught and
  killed; a tenant hold is honoured; `merge-on-green` merges one PR and
  `ask` does not; `#general` stays under ten steward messages for the day.
- **9.10 Steward sees what you see** unchanged.

Order on the critical path: capability_tiers (gd-Bcqj2r4m7E, crewed) → 9.1
→ 9.2 + 9.2a → 9.3 → 9.4/9.6/9.8 → 9.5/9.7 → 9.9. 9.0 runs now, in
parallel, on gardenio.

## 9. Owner questions

1. **Channel name.** `#machines`, `#agents`, or `#backstage`?
2. **Default grant.** `ask` as written, or `merge-on-green` from the start
   for lands whose owner is also the repo owner?
3. **Memory visibility.** Can the tenant read and edit the steward's notes
   bead from the land header (recommended: read yes, edit the Holds section
   yes, the rest read-only)?
4. **Gardenio's operator.** Should the laptop operator's own holds and
   answers move into a `steward-notes:gardenio` bead now, so the same code
   path is dogfooded before 9.2a ships? (Recommended: yes, by hand, this
   week.)
