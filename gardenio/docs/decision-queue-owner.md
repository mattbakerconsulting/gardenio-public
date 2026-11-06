# Decision / Escalation Queue Owner — the SUCCESSOR fallback

**Audience:** operators and agents who manage the operator decision/escalation
queue. Explains the `successor` field and `gb {decision,escalation} successor`
commands (ka-0000006921).

**Sibling docs:**
- `gardenio/docs/escalation-request-runbook.md` — the agent-side escalation flow.

---

## The problem

A decision or escalation is filed by an ephemeral agent and **addressed to an
operator** (a human, via `operated_by`). The normal flow is:

1. The agent files the item with `--requesting-task`, then `gb stop`s.
2. The item persists as a durable re-entry handle.
3. When the addressed operator resolves it in Slack, the agent is auto-respawned
   to continue.

That works as long as **someone eventually acts on the item**. When the
addressed operator goes quiet — offline, on leave, or simply swamped under a
queue of 150+ escalations — an item can sit open indefinitely with nobody
minding it and no live agent parked on a `gb yield`. There is no owner-of-last-
resort.

## The mechanism: a `successor` field

Each open decision/escalation can carry a **`successor`** field — a fallback
**human** (a `type=user` bead id) who should pick the item up when its primary
owner has been idle past a threshold. The field is:

- **type_constraint:** a `type=user` bead id. `gb ... successor set` validates
  the target against the live daemon and refuses an agent/task/other id — the
  successor must be a principal who can actually act on the queue.
- **nullable:** absent by default; `--clear` removes it.

### It is a routing lever, not an auto-decider

The single most important property: **the successor mechanism never resolves an
item on anyone's behalf.** It does not close, decide, approve, or reject. It only
re-surfaces (re-routes) a stalled item to a designated backup human who *can*
act.

This is deliberate, for two reasons:

1. The daemon's **cross-operator guard** already refuses a close by a principal
   other than the addressed operator (this guard is correct — see ka-0000000798).
   An "auto-decide on their behalf" feature would be rejected by it anyway.
2. Quietly deciding an operator's escalation from an agent is exactly the failure
   the whole gate model exists to prevent. "Escalate on their behalf" here means
   **re-route and re-notify the backup human** — surfacing the item to someone
   who can act, never acting for them.

A cross-operator resolution, once a successor does act, already DMs the
originally-addressed operator (gardenio !7703), so the hand-off is visible.

## Commands

```
gb decision  successor set <id> <user-bead-id>   # stamp the fallback owner
gb decision  successor set <id> --clear          # remove it
gb decision  successor resolve [--idle 1h] [--dry-run]

gb escalation successor set <id> <user-bead-id>
gb escalation successor set <id> --clear
gb escalation successor resolve [--idle 1h] [--dry-run]
```

### `set`

Stamps `fields.successor` (plus `successor_set_at` / `successor_set_by` for
audit) after validating the target is a `type=user` bead. Refuses a closed item.

### `resolve`

Scans the **full** open queue for the type (paged at 500, so the oldest rows are
never hidden by the 50-row `gb escalation list` cap) and re-routes every item
that is **eligible**:

- it is **open**;
- it names a non-empty **successor**;
- it has **not already been answered** (no `chosen` / `response_text` — an
  answered-but-not-yet-closed item is left alone);
- its **idle window** ≥ threshold (default **1h**).

The **idle window** is `now − last owner-facing touch`, where the last touch is
the freshest of the item's create time and its explicit re-surface stamps
(`reflagged_at`, `pingback_requested_at`). It is deliberately **not** the bead's
`updated_at`: any field write (including stamping a successor, or a prior route)
bumps `updated_at`, which would reset the idle clock and hide a genuinely stale
item.

For each eligible item, `resolve` (unless `--dry-run`):

- records `successor_routed_at` / `successor_route_count` / `successor_routed_by`;
- requests a Slack re-post via the same **pingback** handshake `reflag` uses
  (`pingback_requested_at` etc., `pingback_source=successor`);
- adds an audit comment naming the successor.

Items are processed **most-idle-first** so the stalest items drain before the
merely-stale ones. `--dry-run` reports what would route and changes nothing.

## Resolution logic (for maintainers)

The pure, `now`-injected core is `resolveSuccessors([]*BeadDetail, threshold,
now) []SuccessorCandidate` in `gardenio/controller/cmd/gb/successor.go`, unit-
tested in `successor_test.go`. Keeping the eligibility decision free of daemon
I/O is what makes the ">1h idle, answered, closed, custom-threshold, sort-order"
matrix cheap to test deterministically; the command layer only adds the list +
side-effect wiring around it.
