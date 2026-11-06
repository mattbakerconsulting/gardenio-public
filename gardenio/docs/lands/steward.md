# The Lands steward roles

Step 9.1 (gd-VwIEAKypQq) of the [Track B spec](../superpowers/specs/2026-10-19-lands-track-b-design.md),
§4 and §7.3. Two roles and one prompt, content only:

- **`steward`**: one agent per tenant land, bound to the land rather than a plan (B-D8). It greets,
  turns asks into plans, spawns gardeners within the purse, reports daily and parks at the wall
  (§4.2).
- **`lands-gardener`**: the crew a steward spawns, one per plan step, on the land's one repo.
- **The steward playbook** is `StewardPlaybookPrompt` in
  `gardenio/controller/internal/steward/steward.go`, beside the plan-gardener's `PlaybookPrompt`.
  `RenderPrompt(Land)` fills it with the land's name, repo, purse (credits only), step estimate,
  `max_live_agents`, members and open plans. It is rendered on every wake (step 9.2, below).

Both role beads hold the capability tier `lands-tenant` (§5.1), registered by gd-Bcqj2r4m7E
(PR #950). The tier means nothing yet: what it permits and refuses is step 10.1 (gd-Pcyz0ftcTg
makes the spawn gate read tiers). Until then the two roles spawn like any unprivileged role, and
nothing spawns them but the provisioner's steward (step 9.3, below); the steward's own spawns are
9.5.

## The role beads

| Field | `steward` | `lands-gardener` | Why |
|---|---|---|---|
| `capability_tiers` | `["lands-tenant"]` | `["lands-tenant"]` | §5.1. `privileged` is derived from the set and stays false. |
| `model` | unset | unset | See below. |
| labels | `project:gardenio` | `project:gardenio` | Fleet config, like every other role bead; roles resolve by title in every project (`beadsapi.ListRoleBeads`). |

**Why no `model`.** OQ3 (gd-Se3rF0T5pu) chose Sonnet 5.5 for tenant agents, with Opus per land at
approval. That choice is already carried per land: the approval reactor defaults to
`claude-sonnet-5-5` and reads `model=opus` from the reply
(`cmd/beads-viewer/lands_apply_reactor.go`), and the provisioner writes the result onto the land's
project bead and its `lands-<land>` account (`internal/lands/provision/provision.go`). The
controller resolves a role's `model` **before** the project's (`resolveModelOverride`,
`cmd/controller/podspec.go`), so a `model` on these role beads would override every land's
approval, Opus lands included. Leaving it unset lets the land's choice stand.

### Creating them

Run once the release containing PR #950 is live (`kd version` at or after the tag that includes
`488df74a`); before that the daemon's `type:role` schema has no `capability_tiers`. A non-empty
`capability_tiers` on create is gated like `privileged`, so this needs a `gardenio-admin`
caller; an agent files an escalation with these commands.

```bash
kd create 'steward' --type=role -l project:gardenio \
  --short-title 'Lands steward role' \
  -d 'One agent per tenant land (Lands Track B §4). Playbook: internal/steward StewardPlaybookPrompt. Doc: gardenio/docs/lands/steward.md' \
  -f capability_tiers='["lands-tenant"]'

kd create 'lands-gardener' --type=role -l project:gardenio \
  --short-title 'Lands gardener role' \
  -d 'Crew a Lands steward spawns, one per plan step, on the land repo (Lands Track B §4.2). Doc: gardenio/docs/lands/steward.md' \
  -f capability_tiers='["lands-tenant"]'
```

Check each with `kd show <id> --json`: `fields.capability_tiers` is `["lands-tenant"]`,
`fields.privileged` is `false`, and there is no `fields.model`. `gb role list` shows both.

A tenant land's agent reads only the tenant-safe label-less advice and config beads: those
labelled `tenant-safe` (kbeads `read_authz_tenant.go`, gd-c1DOWNYy7W). The set is what a land
agent reads at `gb prime`: the global `claude-instructions`, `prerun` and `nudge-prompts` config
beads. Global advice and every `role:<fleet role>` bead stay hidden from it. There is no
`config:claude-instructions:role:<name>` bead for either role yet, so both roles read the global
instructions once they carry `tenant-safe`. Those speak of escalations, Slack and `gb decision` to
the operator, and the steward's playbook overrides them where they disagree ("Never", in the
prompt). A tenant-safe instructions bead for each role goes in with the `tenant-safe` label and no
`project:` label.

## The land-bound gardener (step 9.2)

`steward.Bind(ctx, daemon, land, BindSpec)` (`internal/steward/bind.go`) binds the one steward to
a tenant land, as `plangardener.Bind` binds a plan's gardener. It is idempotent: a land that has a
steward, enabled or paused, keeps it. The provisioner calls it for every new land (step 9.3, below).

The steward is a `type=trigger` bead titled `Steward (<land>)`:

- **Labels:** `trigger:multi`, `role:gardener`, `steward` (the marker), `gardener-land:<land>` (the
  binding), `project:<land>` (its beads stay inside the land's scope) and `perpetual:true`.
- **Fields:** `action=spawn_agent`, `role=steward`, `project=<land>`, `triggers` (below), and a
  `gardener` blob that is `standing`, `one_spawn_at_a_time`, `max_runs_per_day: 24`, with the
  bounded-stop guard's run budget and the 720h wall-clock ceiling. The steward is standing
  (`standing` in the blob and `perpetual:true`), so trigger-engine renews that wall clock in place
  and never closes it on age (ka-0000004573; pinned by `TestSteward_PastItsWallClockIsRenewedNotClosed`
  in `internal/bridge`). It closes on its `consecutive_failures` hard stop, the credential
  crash-loop gate, or by hand, and the provisioner then binds the land a fresh one
  ([provisioner.md](provisioner.md#a-closed-steward)).
- **Prompt:** trigger-engine renders the playbook with the land's live state at every fire
  (`steward.TriggerPromptRenderer`, registered on the `steward` label for triggers whose spawn role
  is `steward`). The stored prompt, rendered at bind time, is the fallback when a read fails.

### Wake events (§4.3)

Every event entry requires the land's `project:<land>` label, so an event in another land never
wakes the steward, and the entries are disjoint, so one event wakes it once
(`TestWakeTriggers_*`).

| Entry | Fires on |
|---|---|
| `general-message` | a `type=message` bead created in the land's `#general` (`land_default_channel`) by a person (`author` is `oidc:…`) |
| `steward-mention` | a person's message containing `@steward` in any other channel of the land |
| `decision-resolved` | a `type=decision` bead closed whose `created_by` is a steward run (`<land>-steward-<suffix>`). kbeads publishes no decision-resolved event, so the close is the signal. |
| `credit-grant` | a `type=credit_grant` bead created |
| `purse-threshold` | the purse watcher's 50/80/100 notice in `#general` (`origin=purse_notice`; its refill post is `purse_refill` and is left to the grant) |
| `gardener-finished` | a `role:lands-gardener` agent bead closed |
| `machines-delta` | machine traffic in the land's `#machines` (amendment gd-YGlfkPsFrO §2), the steward's own posts and the purse watcher aside; debounced 10 minutes and throttled to one wake per 30, since the run reads the delta. Only when the land has a `#machines` channel at bind. |
| `notes-edited` | a person (an OIDC `actor`, so not the steward's own end-of-run write) updates `steward-notes:<land>`, e.g. the tenant's Holds |
| `land-opened` | the provisioner's one wake post when the land opens (step 9.3): a message with `origin=land_opened` by `agent:gardenio-provisioner`. The `machines-delta` entry leaves it out, so it wakes the steward once. |
| `person-joined` | the provisioner's wake post when a person joins the land (step 9.14): a message with `origin=person_joined` by `agent:gardenio-provisioner`. The `machines-delta` entry leaves it out too. |
| `daily-digest` | a daily cron at 08:MM in the land's zone, the minute seeded from the land's name |
| `quiet-gardeners` | home garden only: a cron every two hours (`M */2 * * *`, the minute seeded from the land's name) whose preflight is the `quiet_gardeners` leaf, so it fires only while a plan's gardener is quiet or still timed ([below](#quiet-and-timed-plan-gardeners-gd-bo2rumtphb)) |

The `notes-edited` entry reads `actor` from the bead event: the multi-source payload now carries
the change's actor (an OIDC email, or an agent name) beside the bead's columns.

### The message that woke it (step 9.10)

A `general-message` or `steward-mention` wake renders the message itself under `## What woke
you`, after `#machines` and before the land snapshot: who wrote it and where, their words quoted
(cut at 4 KB, the rest read with `gb thread`), its files, and for a farm snapshot (`fields.snap`,
written by the Commons Snap button) the view link, where the camera was (`at`, `z`, who, night) and
what it framed. A Commons attachment comes with `gb commons file <message-id>` and the path it
writes each file to (`commons-files/<message-id>/<name>` in the scratch directory), and the
steward Reads each picture before it answers. A snapshot with no words gets the default ask, "Say
what you see on this part of the land and what, if anything, needs the owner." The steward
answers with `gb say --thread <thread>`, the message's thread head (or the message itself), so the
picture, the ask and the answer stay together; an answer that is a plan goes through duty 2.

The trigger engine hands the fire-time renderer the entry label and event bead
(`bridge.FireEvent`); the renderer reads the bead and renders it only when it is a message of the
steward's own land. Any other wake renders no section. A snap is an ordinary message to the wake
entries, so it wakes the steward once and a snap in another land never does
(`TestWakeTriggers_*`, `TestSnapToStewardEndToEnd`). The thread-agent path for the operator's own
land (no steward) is unchanged.

### Prompt inputs from the amendment

The playbook renders, in this order: the steward's notes, then `#machines` since the last run,
then the wake message (above), then the land snapshot. A run is short: notes first, then the event, then the land, then at most
one of plan, spawn, triage, digest or park.

- **Notes** are the `steward-notes:<land>` bead's body (`fields.content`, markdown), rendered in
  full. With no such bead the prompt says so. See [Steward memory](#steward-memory-the-notes-bead).
- **`#machines` by delta.** Messages after the high-water mark, oldest first, the newest 40 at
  200 characters each, with a count of the rest. The mark is the notes bead's
  `machines_high_water` field (RFC 3339, the newest message read; 9.2a writes it at the end of a
  run). Until it is set the delta starts when the previous run's agent bead was created, else a
  day back.

### Steward memory: the notes bead

Step 9.2a (gd-mN1TLhGpGe; amendment 2026-10-23 §3). One `type:doc` bead per land, titled
`steward-notes:<land>`, labelled `project:<land>`, `format: md`, the notes in `fields.content`
under four `## ` sections: **Holds**, **Answers** (newest first, one line each with the decision
id), **Promises** (what and when), **In flight**.

- **Born with the steward.** `steward.Bind` creates it with the sections empty
  (`steward.EnsureNotes`), and creates a missing one when it adopts an existing steward.
- **Written at the end of every run** with `gb lands notes <land> --write <file>
  [--machines-high-water <RFC 3339>]`. The write replaces Answers, Promises and In flight. It keeps
  the stored Holds, so a person's edit made during the run is not lost; the steward changes Holds
  only with `--add-hold` / `--lift-hold`. The prompt gives the exact command, with the newest
  `#machines` message shown as the mark.
- **Cap and rollover.** Notes are held to 4096 bytes (`steward.NotesCapBytes`): the oldest Answers
  roll out into a comment on the bead, newest first. Notes still too long are refused, and the
  steward shortens In flight or Promises.
- **Who writes it** (kbeads `steward_notes_authz.go`). The land's steward, a new agent each run,
  may update and comment on its own land's notes bead and no other doc. A land owner or member
  reads the whole bead and may patch only the Holds section: `fields.content` alone, with every
  line outside `## Holds` unchanged (the `edit_holds` scoped action). Guests may not. A person's
  edit wakes the steward (`notes-edited`).

### The runs-per-day cap

`max_runs_per_day` is a gardener blob field the engine enforces for any gardener: past that many
spawns in one UTC day the gardener stays open and enabled and does not fire until the next UTC
day (`gardenerSkipDailyRunCap`). The engine keeps the count in `runs_day` and `runs_today`, only
for a gardener with a cap. `bridge.GardenerDailyRunCapHit` is what the land header reads to show
the cap is hit.

### Parking at the wall (step 9.7)

At the purse wall the Lands spawn wall (step 8.3) refuses the steward's own fires as it refuses
any gardener's, so §4.2 duty 5's post cannot come from a steward run. The purse watcher
(`internal/lands/watcher`) does it through the steward hook (`steward.Hook`, wired in
`cmd/controller/purse_watcher.go`):

- **The post.** On the first pass that sees the land at the wall, the watcher calls
  `Hook.PurseEmpty`, which posts `steward.ParkText` ("The purse is empty; work is parked; ask the
  operator for credits in #support.") in the land's `#general` as `agent:<land>-steward`. A land
  with no steward, or no `#general`, has nothing posted. The post is keyed
  `steward-park:<land>:<granted credits>`, so a retry is not doubled and the next wall, after a
  refill, is said again. It carries no `origin` and no `oidc:` author, so it wakes nothing.
- **The reason.** Once the line is said, the watcher stamps `steward_park_reason=purse_empty` on
  the land's `lands-<land>` account bead, beside its own purse state, and clears it on the first
  pass whose purse is `ok` again. A failed post leaves the field unset and is retried the next
  minute; an unknown purse neither parks nor unparks. The land's gardeners carry
  `park_reason=purse_empty` on their own agent beads, as the watcher suspends them (step 8.4).
- **The sleep.** Nothing fires the steward while the purse is at the wall; the `credit-grant`
  wake is the one that runs it once credit arrives.
- **The resume.** Every steward prompt lists, under "Parked when the purse ran out", the land's
  open steps whose newest `lands-gardener` was parked for the purse and is not running again (a
  step with a newer gardener, or a closed step, drops off). On the grant wake the playbook has the
  steward choose which to pick up again, with the duty 3 purse check, and say once in `#general`
  what resumed and what waits. Nothing resumes on its own.

### Known limits

- An event that arrives while a steward run is still live is not queued: the engine's
  `one_spawn_at_a_time` gate skips the fire and the event is dropped. The next wake's run reads
  the land afresh, so a message is seen then, but a quiet land may not wake again until the
  digest.

## Conversation to plan (step 9.4)

Spec §4.2 duty 2 (gd-JW9EiMPVv9). A person asks for work in `#general` or with an `@steward`
mention; the steward turns the ask into a plan, asks the land's owner, and on approval runs it.
Two `gb lands plan` verbs do the writes (`internal/steward/plan.go`, `cmd/gb/lands_plan.go`), so a
short run makes them in one call each:

- **`gb lands plan write <land> --file plan.md --thread <message>`** reads the plan as markdown
  (`# <title>`, an `Arms: <three to five emoji>` line, what it does and why, then one `## <step
  title>` per step with what it changes, at most 12 steps) and writes it in the land: an epic
  labelled `plan` and `project:<land>`, born `plan_state=draft` and bearing the arms (checked
  first as `kd heraldry check` checks them, in the library, three to five, unique in the land, and
  refused in its words; a plan with no `Arms:` line is written with a warning naming `kd update
  <plan> --heraldry`), one `task` per step (`parent-child` to the epic, each `blocks`-ordered after
  the one before, `step_order` on the epic), then `plan_state=ready`. It files one decision,
  "Plan <id>: <title> - run it?", with `approve`, `change` and `drop`, addressed to the land's
  owner (`operated_by`; the one member the members API reports as `owner`, else `--for`). The
  decision names the plan in `requesting_task`, the plan is `blocks`-linked to it, and its context
  opens with the arms (`Arms: 🔥🧭📦`) and lists the steps and an estimate (steps × the step
  estimate). The options carry no
  `artifact_type`, so an answer never leaves the steward a report owed before it may stop. It
  then replies in the thread with the plan id, its arms before its title. The epic carries an idempotency key per ask, so a
  retried write finds the plan and refuses rather than asking twice.
- **`gb lands plan answer <land> <decision>`**, on the `decision-resolved` wake, applies the
  owner's answer: `approve` moves the plan `ready → building` and prints the first step not done
  yet (spawn a `lands-gardener` on it, duty 3); `change` and `drop` move it to `abandoned` and
  close it with its open steps (a change is a new plan written with `--replaces <plan>`, linked
  `related` and said on its card). A dismissed card or a written answer that chose no option
  changes nothing: the steward reads the words and answers or asks again. A second run on the
  same answer changes nothing. An answer an operator-lane human (`resolver_admin`) gave within
  the last day is also applied by the server (the beads-viewer's plan answer reactor, see
  [`plan-authoring-runbook.md`](../plan-authoring-runbook.md)); the tenant's own answer on a
  tenant land is still the steward's to apply.

Each step body must be at least 200 characters (`steward.MinPlanStepBody`): what it changes,
where, and how to tell it is done, because the gardener that takes the step reads only the bead.
`write` refuses a thinner step and `answer` refuses to approve a plan that has one; both errors
name the step.

**The home garden runs duty 2 too (step 9.11, gd-igAyMerw44).** The home garden is the operator
land on the Commons: its project bead has `land_default_channel` and names a person as its
operator (`operated_by` `oidc:<email>`; `steward.HomeOwner`). Any other operator land is still
refused. Its Commons thread agents use the same two verbs, and so does its steward once bound (step 9.13, below) (the thread-agent
workflow in `internal/bridge/init_prompts.go` says so). The owner asked is the project's operator
unless `--for` names another. Without `--thread`, `write` uses the thread head on the agent's
bead, else it replies in the agent's home thread. On approve, `answer` also does what this
steward's duty 3 would: it binds the plan's gardener enabled (`plangardener.Start`, the core
behind `gb plan gardener`, adopting and enabling a paused one) and fires one cycle
(`gb gardener fire-now`'s core), so the plan builds with no operator command.

### What woke the run

A steward run's prompt has a **This wake** section: the entry label and bead of the wake that
fired it (the message, the decision, the finished gardener's agent bead; no bead for the daily
digest), from the engine's `FireEvent` (step 9.10), and a line per entry saying where to start. The
People list carries each member's land role as their last sign-in recorded it (`role` on the
members API), which is how `gb lands plan write` finds the owner.

## The daily digest (step 9.6)

Spec §4.2 duty 4 (gd-soDpoj3JQb), as the amendment words it: the digest is the steward's only
scheduled post, written for a person (what happened, what it means for them, what if anything
they must do), and it reads memory's **In flight**. The `daily-digest` cron (`DigestCron`, 08:00
in the trigger's timezone at a minute seeded from the land name) wakes the steward; `gb lands digest`
(`internal/steward/digest.go`, `cmd/gb/lands_digest.go`) does the reading and the post:

- **`gb lands digest <land>`** reads what changed since the last digest, or a day back before the
  first: steps closed (`task`/`bug`/`feature`; duty 3 closes a step when its PR merged), plans
  written, finished or dropped, decisions answered (with the chosen option and who), decisions
  asked, and `lands-gardener` sessions started. Then where the land stands: steps in progress and
  their gardener, the notes' In flight lines, the credits spent since the last digest and
  remaining, and **What you must do** (below). It writes nothing.
- **What you must do: the owner errands list** (agent-access programme step P9.6,
  `internal/steward/errands.go`). Every open owner-only item in the land is one errand: a decision
  waiting on a person (`operated_by`), an open escalation whose effective audience is the owner
  (`beadsapi.EscalationForOwner`: the floor, a secret, credentials, a named approver, or
  `audience=owner`), and an open, in-progress or blocked step labeled `owner-at-device` (which is
  then left out of "In progress now"). Errands are grouped by the place the owner has to be, in
  this order: GitHub (settings, merges, re-runs), Slack admin, Keycloak, AWS console, Phone, At a device, Decisions to
  answer, then any place a filer named in their own words, then Elsewhere. Each line carries the
  clicks and the evidence that closes it. A filer says these with three optional fields, on the
  bead or (for an escalation) as keys of `operation_spec`:
  `errand_place` (`github`, `slack`, `keycloak`, `aws`, `phone`, `device`, or free words),
  `errand_clicks` (the exact steps) and `errand_evidence` (what closes it). Without
  `errand_place` the place is read off the operation type (`github_*`, `slack_*`, `keycloak_*`,
  `aws_*`, `terraform*`, ...) or a place word in the title; without clicks the line says to read
  the bead; without evidence it is the bead closing. The list is where an owner-only item waits,
  so nothing is filed twice: an escalation whose `requesting_task` is a listed step folds into the
  step, and an errand with the same place and title as another folds into it (`also <id>`). An
  approved escalation still open is listed as "approved, not yet done".
- **Quiet days post nothing.** When nothing was done, planned, answered, asked or run, it prints
  one line, "Quiet: ... Post nothing.", and no post command. A new errand is news; an errand still
  waiting from before is not, and spend alone is not either: the steward's own digest run spends credits every
  day.
- **`gb lands digest <land> --post digest.md --as-of <t> --used <credits>`** says the steward's
  message in the land's `#general` and records `digest_at` and `digest_used_credits` on the
  `steward-notes:<land>` bead, where the next digest starts. The read prints this command with
  its own as-of and used figures (used = spent + in flight), so nothing between the read and the
  post is missed and the credits spent since are a difference of two purse readings. A post as
  of a time at or before the recorded `digest_at` is refused, so a retry is not said twice. An
  unknown used figure is recorded as unknown, and the next digest says what is used so far
  instead of a difference.

## The provisioner hook and the first greeting (step 9.3)

The Lands provisioner's `steward` step (§3.3 step 6; [provisioner.md](provisioner.md)) calls
`steward.ProvisionHook`, which the controller wires with its daemon client:

- **`ProvisionHook.Bind`** runs `Bind` for the land, with the approver (`oidc:<decided_by>`) as operator and
  `gardenio-controller` as creator, and returns the trigger bead. The provisioner's machine
  principal writes it: `type:trigger` is project-scoped, not admin-gated, and the steward role is
  unprivileged, so its spawns need no lineage seal.
- **`ProvisionHook.Wake`** posts one `kind=notice` message in the land's `#machines` (its `#general` when it
  has none), `origin=land_opened`, author `agent:gardenio-provisioner`, with an idempotency key per
  application. The provisioner posts it only once the trigger is two minutes old, so trigger-engine
  has loaded the trigger and the event is not dropped.
- **`ProvisionHook.Retire`** is the close's `steward` step (`gb lands close`): it sets `enabled=false`
  on the land's steward trigger and closes it, noting why. trigger-engine loads open triggers only, so
  its wakes and daily digest stop; without it every digest would fire into the spawn wall's
  `CodeLandGone`. The provisioner created the trigger, so its machine principal owns the close.

The run it wakes reads "nothing posted in #general yet" and does duty 1. The greeting says, in
plain words: what the steward is and can do, the purse in credits, **the grant in force** and what
it lets the steward do alone and what it still brings to the tenant, **how to change it** (ask the
operator in `#support`: `steward_authority` is an admin-written project field and the land header
has no control for it yet), how to ask, that **the land's agents talk in `#machines`** (only when
the land has one), and that `#support` reaches the operator.

## First contact: the greeting when a person joins (step 9.14)

Decision D3 gd-NLc4LzmCAE (on-claim-and-tour) and D2 gd-R0hspYDBbq (no-room): the steward greets a
person when they join its land, in the land's `#general`; there is no private room. The provisioner
wakes it, at most once per invite:

- a claimed **sign-in invite** naming a land (`email_invites.go`), when the person did not already
  hold a role there;
- a claimed sign-in invite naming **no land**, when it made the person a **gatehouse** guest
  ([gatehouse.md](gatehouse.md));
- a joined **member invite** (`invites.go`), except a demo pass in the demo land: a day's look
  around wakes nobody.

**`ProvisionHook.Greet`** posts the `person_joined` notice where `Wake` posts (`#machines`, else
`#general`), idempotent per invite (`lands-invite:<id>:greet`), before the invite is marked, so a
crash greets once rather than never. The notice names the person by their Keycloak profile name
(the GitHub profile's name), else their username, and never by an email address: every guest reads
the land's channels. A failed post is logged; the person is let in either way. The run renders the
notice under `## What woke you` and posts one welcome in `#general` by name: at the gatehouse, that
they are a guest, that `#general` is read by every newcomer, and `/lands/apply` and `/lands/demo`;
in a tenant land, what the land is for, what their role lets them do and how to ask. An invite
that is never opened again leaves that one greeting and nothing else.

A steward bound before step 9.14 has no `person-joined` entry. The provisioner's pass upgrades each
open steward of a provisioned land in place (`ProvisionHook.Upgrade`, `steward.UpgradeWakes`: the
entry before the digest, and the `machines-delta` entry keeping the notice out); the home garden
is no provisioned land, so `gb lands steward gardenio --bind` upgrades its steward. Nothing adds a
person to the home garden yet, so it is never greeted today; whatever does adds a `Greet` call.

### The grant

`steward_authority` on the project bead (amendment gd-YGlfkPsFrO §4) is registered on
`type:project` here, in the kbeads builtin and the controller's copy: `ask`, `merge-on-green` or
`operate`. Unset or anything else reads as `ask` (`ParseAuthority`), so a value the steward cannot
read never widens what it may do. `Land.Authority` carries it into the prompt; only the greeting
uses it. Duty 3 and the "Never" list still state the `ask` level whatever the grant: acting under a
higher grant (merging, releases) needs the steps that wire it and the `lands-tenant` tier's
permissions (10.1).

## Spawning within the purse, liveness and the kill switch (step 9.5)

- **Step cost estimate** (`internal/steward/estimate.go`): the mean settled cost of the land's
  last 10 `lands-gardener` agent beads, at the `llm_multiplier` in force, in credits. Settled
  means closed with the controller's `litellm_spend_close` watermark, so `total_cost_usd` is the
  LiteLLM figure, not the in-pod estimate. Before there are 10, or with no rate in force, it is
  `DefaultStepEstimateCredits` (5). The prompt and the plan card (`gb lands plan write`) carry it.
- **Ask instead of spawn** (`steward.CheckStepSpawn`, `cmd/gb/spawn_purse.go`): before `gb spawn
  --role lands-gardener` creates anything on a tenant land, it refuses with `NOT SPAWNED` and a
  code: `purse_wall`, `purse_unknown`, `max_live_agents`, or `ask_owner` when the estimate is more
  than `remaining - margin`. The steward files the owner a decision for the step
  (`--requesting-task <step>`, options `spawn` / `wait`); their yes comes back as
  `--owner-approved <decision>`, which lifts only `ask_owner` and is stamped on the agent bead
  (`owner_approved_decision`). `steward.CheckOwnerApproval` takes the decision only when it is
  answered `spawn`, filed for the same `--task`, and answered by someone other than the caller,
  the decision's filer or any agent of the land; an unanswered decision, a `wait`, a self-answer
  or another step's decision is refused. Nothing is created on a refusal.
- **`max_live_agents` at the spawn wall** (`internal/lands/wall/live.go`): both controller pod
  doors refuse a new non-steward spawn with `max_live_agents` when the land's live agents, the
  steward's slot always held, fill the cap (unset is 3, what the provisioner writes). The steward
  is never refused there, so it can always wake to watch its gardeners. Refused, parked
  (`stopping`), queued and finished agents hold no slot. The namespace's pod quota
  (`max_live_agents + 1`) is the backstop for a race.
- **The spawn gate** (kbeads `lands_tenant_authz.go`, PR #954): a `lands-tenant` caller spawns
  only as the steward, only `lands-gardener`, only in its own project; tests cover each refusal.
- **Liveness** (`internal/steward/liveness.go`, amendment §5): every wake renders "Live agents",
  oldest change first, each marked `QUIET` after 45 minutes with no bead change and `PAST THE
  LIMIT` after more than 3 restarts (session count less one). Duty 3 says what to do: ask a quiet
  one once in `#machines`, kill it at the next wake if still quiet and reopen its step with a note;
  kill one past the limit and mark its step `blocked` with the reason.
- **Kill switch** (`cmd/gb/agent_kill_steward.go`): `gb agent kill <agent> --reason "..."` run by
  a steward (`BOAT_ROLE`) on an agent of its own land (`BOAT_PROJECT`) skips gb's spawned-by-you
  check, whatever the grant, closes the bead (`agent_state=failed`,
  `failure_reason=killed-by-steward`) and says the kill in `#machines` with the grant named. It
  never kills a steward. The daemon's land scope still decides the close.

## The home garden's steward (step 9.13)

Decision gd-B5HQhmG1cJ gives the home garden (`steward.HomeOwner`, above) a steward too. It is
bound by hand, once, since the home garden is no provisioned land:

    gb lands steward gardenio --bind

`steward.LoadLand` takes the home garden as it takes a tenant land and refuses any other operator
land. What differs (`Land.Home`):

- **The purse is unmetered.** `purse.Read` still returns `ErrOperatorLand` for it; `steward.ReadPurse`
  renders it `purse.Unmetered` (state `unmetered`, never blocked, no credit figures), and so do the
  playbook's purse line, `gb lands digest` (its Credits line says none) and `LandStepEstimate` (zero,
  so a home plan card names no credits). The controller's spawn wall already passes an operator land.
- **Fewer wakes** (`steward.HomeWakeTriggers`): a person's `@steward` in any channel, `#general`
  included; a decision a steward run filed; a person editing `steward-notes:gardenio`; the
  land-opened post; the digest; and one of its own, the `quiet-gardeners` watch (below). No wake on every `#general` message, where the fleet's `@gardenio`
  agents answer side by side (D7 gd-QaoNbHRJPC), none on `#machines`, which carries the whole
  fleet's traffic (well over a thousand messages a day; the run still reads the 40-line delta), and
  none for a purse or a `lands-gardener`.
- **Duties 3 and 5.** The home steward never `gb spawn`s and never kills: on an approve,
  `gb lands plan answer` starts the plan's gardener (step 9.11). It tells the owner in `#general`
  when a gardener of a plan it wrote is quiet or past the restart limit. There is no wall to park at.
  Its live agents are the fleet's, shown as facts only.
- **Plans with no live gardener** (gd-hVTkwcDwxK). Every wake the prompt lists each `building`
  plan of the land, whoever wrote it, with no open, enabled trigger labelled
  `gardener-epic:<plan>` (`steward.stalledPlans`): its newest closed gardener's
  `gardener_terminated_kind` (or `paused`, or none ever bound), the first line of its
  `last_fire_error`, its open and ready steps, and an ask. `rebind` (steps ready; a budget stop
  `max_runs`/`wall_clock`/`no_progress_for`/`bead_query`, refused spawns, paused or never bound)
  and `complete` (no open step) file the owner one decision naming `gb plan gardener <plan>` or
  `gb plan complete <plan>`; `waits` (every open step waits on something) and `tell` (stopped by
  hand or a fault) are said once in `#general`. The steward records the decision id in its notes
  so a later wake does not ask again. It asks with a decision, not an escalation: the lands-tenant
  tier files only a merge escalation about its own land (`restricted_ops.go`), and binding a
  plan gardener is privileged, so the owner runs the command.
- **Quiet and timed plan gardeners** (gd-bo2RUmtpHb): see the next section.
- **The land's advice** (gd-kEis8bURm5, owner pick on decision gd-c0b7m4dErB). An advice ask
  for the home garden goes to the `steward:<project>` lane, once the `escalation_policy` routes
  the `advice` class there. Duty 6 has the steward list what that lane holds
  (`gb escalation list --held-by steward:<project>`), hand up every non-advice ask and its own,
  and approve, claim, write and close an advice ask scoped to its project. kbeads admits the
  home steward to that lane for the advice class only, and its writes to the advice beads the
  approval named, labelled with that project only, never `global` (`escalation_steward_advice.go`,
  ownership row `steward-advice`, `kbeads/docs/ownership-grants.md`).
- **Model and grant.** The steward role bead names no model, so it runs on the gardenio project's
  default (D5 gd-lDbe3cv9Gh: Opus). Its grant is `ask` until 9.15 (gd-tcH835oCW6) flips it.

### Quiet and timed plan gardeners (gd-bo2RUmtpHb)

On 11-01 every home plan gardener moved from a 30-minute cron to event mode (gd-fULNT8wZNF):
`trigger:multi`, woken by bead events in its plan's subtree (debounce 10 minutes, throttle 15)
plus a daily fallback-watchdog cron. The 30-minute cron was 779 runs a day, most of which dispatched
nothing. The risk now is the other way: a gardener that stays quiet while its plan has work ready.
The owner ruled the same day that the land's steward owns watching for it, permanently.

**The check** is deterministic Go, `conditions.FindQuietGardeners`
(`internal/scheduler/conditions/leaf_quiet_gardeners.go`; it lives there because both
`internal/steward` and `internal/plangardener` import that package). For each `building` plan of
the land, it takes the newest open, enabled trigger labelled `plan-gardener` and
`gardener-epic:<plan>` (a plan with none is on the gardener-less list instead) and flags it:

- **quiet**: in event mode (`trigger:multi`), no live cycle (an open agent bead with
  `spawned_by=trigger:<id>`, the fire-now singleton's query), no run for 2 hours or more
  (`last_run`, else since the trigger was created), and at least one **ready** step under the plan:
  open, unassigned, of type task, bug, feature or chore, neither `blocked_on_decision` nor
  `parked_on_decision` (a mention of an open decision, what `gb ready` prints PARKED), and with no open
  blocking dependency (the daemon's `no_open_deps`, as `gb ready` and the `ready_queue` leaf use; an
  open blocker keeps its dependent waiting however deep its own chain goes);
- **timed**: still `trigger:schedule`, a regression whatever the plan holds. Approve & build binds
  every new plan gardener in event mode since gd-gsvlS6YDj3 (#1907).

It also reads the steward's own decisions about the gardener from the last 24 hours (`gb decision
create --subject <gardener>` stamps `subject_trigger_id`; only those whose `created_by` is a
`<land>-steward-` run count). A decision filed since the gardener's last run **raises** a quiet
spell; for a timed gardener, any decision in the 24 hours does. A spell that is not raised while an
earlier decision is in the window is the second time today.

For a quiet gardener no decision raises, it also reads the plan's comments for the steward's
**nudge**: a comment by a `<land>-steward-` run ending `; nudging`
(`conditions.QuietNudgeText`), made since the gardener's last run. A nudge less than 2 hours old
holds the spell off the way a decision does; one older than that, with the gardener still quiet,
does not. It nudges only a gardener whose `triggers` carry the `steward-nudge` entry (below); one
without it goes straight to the owner. Any read error fails the whole check, and so does a daemon
client that cannot read comments.

The steward nudges a plan **at most twice in a rolling 24 hours** (`conditions.QuietNudgeCap`). When
a third nudge would be due, the gardener has answered two nudges (it ran after each, or it would
still be in that nudge's spell) and is quiet again. The check then names the ready steps that were
already open before the first of the two (`Passed`): steps the gardener may be passing over. The
steward files one owner decision instead of nudging. Any steward decision about the gardener filed
after the second nudge raises it, so the decision is filed once until the nudges age out of the
window. A capped gardener whose ready steps all came after the first nudge is not passing over
anything, and gets the ordinary `fire` ask.

**The wake** (`steward.QuietGardenersEntry`) is a cron every two hours whose preflight is the
`quiet_gardeners` daemon leaf (`{"type":"quiet_gardeners","op":">","value":"0","project":"gardenio"}`).
trigger-engine evaluates a cron preflight itself, without spawning (`cronPreflightHolds`), so the
watch costs no agent while every gardener is fine. The leaf counts the flagged plans that are not
raised and not waiting out a nudge, so a plan the steward has already asked about, or nudged in
the last 2 hours, does not wake it. It fails closed: a read
error is returned as an error, which suppresses the fire and also survives a `not()`. A due cron's
preflight is re-evaluated on every engine tick for the rest of its hour (the cron-due cooldown
bypass), so the leaf memoizes its count per daemon client and land for 10 minutes; errors are not
memoized.

**What the steward does.** Every home wake renders "Plan gardeners to see to" after the gardener-less
list (`steward.quietPlans`, `describeQuiet`): the plan, its gardener, how long it has been quiet
and its last `last_outcome`, its ready steps, any decision already filed, the **Ask** and the
owner's **Fix**. A failed read is said in the prompt rather than failing the render. Duty 3 says,
by Ask:

| Ask | When | The steward |
|---|---|---|
| `nudge` | quiet, can be nudged, not nudged or asked this spell | runs the line's exact `kd comment add <plan> "steward: gardener <g> quiet <n>h with <k> ready steps; nudging"`, which wakes the gardener, and files nothing else |
| `nudged` | nudged less than 2 hours ago | nothing |
| `passed` | quiet after answering 2 nudges in 24 hours, with steps ready since before the first | files one decision (`--subject <gardener>`): "gardener <g> answered 2 nudges but steps <ids> are still ready: the gardener may be passing over them", options `look` / `pause` / `leave`, naming `gb trigger disable <gardener>` to pause, and notes it on the plan |
| `fire` | quiet 2 hours after a nudge, or it has no `steward-nudge` entry, not yet asked | files the owner a decision (`--subject <gardener>`, options `fired` / `leave`) naming `gb gardener fire-now <gardener>`, and comments on the plan: `steward: gardener <g> quiet <time> with <n> steps ready (...); asked the owner to fire it, decision <id>` |
| `again` | quiet, and it asked about this gardener earlier in the 24 hours | the same, and tells the owner once in `#general` that the gardener went quiet twice with work ready, so its event wake may not work |
| `event` | timed, not asked in 24 hours | files the owner a decision (`switched` / `leave`) naming `gb gardener mode <gardener> --event --fallback-cron "<m> <h> * * *"`: the daily watchdog shape the 11-01 flip used, at its old cron's minute and an hour seeded from the trigger (`steward.FallbackCron`) |
| `asked` | a decision already raises it | nothing |

**The nudge wake.** `gardenermode.EventTriggers`, the one builder of a plan gardener's event
shape (`gb gardener mode --event`, `gb gardener cadence` and `plangardener.Bind`), adds a
`steward-nudge` entry after `plan-activity` when the trigger has a project:

```json
{"kind":"bead_event","label":"steward-nudge","events":["comment.added"],"throttle":"1h",
 "match":{"all":[{"field":"id","op":"==","value":"<plan>"},
                 {"field":"comment_author","op":"contains","value":"<project>-steward-"},
                 {"field":"comment_text","op":"contains","value":"; nudging"}]}}
```

kbeads publishes `beads.comment.added` with the comment (`bead_id`, `author`, `text`); the author is
the daemon's authenticated subject (for an agent, its per-agent token's subject: its name), never
a body value. trigger-engine subscribes to it with the other edge topics, and the multi source
hydrates the commented bead into the payload, so `created_by` there is the **plan's** author, not
the comment's. The source therefore adds `comment_author` and `comment_text` from the frame
(`bridge.CommentAddedFields`). `id ==` matches the plan epic itself, not a comment anywhere in its
subtree. Events is `comment.added`, not `added`, since the short form would also take label,
dependency and follower adds. Crew runs are named after their task and gardener runs
`<project>-plan-gardener-for-...`, so their comments do not match the author leaf, and the text
leaf leaves out the steward's other notes. A crew whose title begins "steward" would carry the
prefix, but it would also have to write the marker on the plan epic, and a match only wakes the
gardener, at most once an hour.

**Upgrading the gardeners.** `gb gardener mode <trigger> --event` on a gardener already in event
mode now refreshes its event entries to the current shape: plan-activity and steward-nudge are
rewritten, other non-cron entries kept, and the watchdog kept unless `--fallback-cron` or
`--no-fallback-cron` names another. It writes nothing for a gardener already current, and the
spawn-seal guard runs when it would write. The gardeners flipped on 11-01 need it once, after the
release carrying the comment fields is live in trigger-engine. An older engine never carries
`comment_author`, so the entry never matches there, and the steward goes on to the owner 2 hours
after its nudge.

**Why the steward cannot fire.** `gb gardener fire-now` is a direct spawn of the
gardener's role, `plan-gardener`, from the caller's credential. The steward cannot make it: its
role bead holds the `lands-tenant` tier, and kbeads' `landsTenantSpawnTargetDenial`
(`lands_tenant_authz.go`) lets a lands-tenant steward put an agent only into `lands-gardener` in
its own project. The `plan-gardener` role bead holds an all-types `ownership_grants` row, so a non-human spawn
of it must escalate (`spawn_authz.go` `roleIsPrivileged`), and the tier may not turn a denial into an
escalation (`restricted_ops.go`: `opEscalationRetry` is `tenantNever`). No privilege was added for
this. Letting the steward fire would need a kbeads rule admitting the home garden's steward (an
operator land's, not a tenant's) to spawn `plan-gardener` for an open trigger labelled
`plan-gardener` and `gardener-epic:<plan>` of a `building` plan in its own project, plus an exemption
from the privileged-role escalation for that one case.

**Upgrading the bound steward.** A home steward bound before this has no `quiet-gardeners` entry.
`gb lands steward gardenio --bind` adopts the bound steward and, for the home garden, rewrites its
`triggers` with `steward.UpgradeHomeWakes`: the person-joined entry (9.14) if it is missing and the
watch before it, in the digest's zone. A steward already up to date is not written. Run it once the
release carrying the leaf is live in trigger-engine: an older engine fails the unknown leaf closed,
so the entry would never fire, but nothing breaks.

**Handoff.** Until the owner's three criteria on gd-bo2RUmtpHb are met (the steward flags and
notes a quiet gardener the laptop also caught on two occasions, catches a timed gardener, and 48
hours in which the laptop watch finds nothing the steward missed), the laptop loop keeps its watch.

## The gatehouse's steward (step 9.12)

The gatehouse ([gatehouse.md](gatehouse.md)) is a tenant land whose project bead carries
`land_gatehouse=true`. Its steward is bound by the provisioner like any tenant land's, with the
same wakes, cap and wall; `Land.Gatehouse` swaps duties 1 to 4 for the front door's: greet, answer
newcomers in their thread, build nothing (no plans, no spawns), no digest. Every guest reads its
`#general` (D2 no-room), so the greeting says so and the steward never asks for anything private.
It points to `/lands/apply` and `/lands/demo` until the D4 tools pass the 10.14 review. With
`land_gatehouse_tools=true` on the project bead (`Land.GatehouseTools`), duty 2 also hands out
demo passes and drafts applications with `gb lands gatehouse demo|apply` ([gatehouse.md](gatehouse.md#the-stewards-apply-and-demo-tools-d4)).

## The pending amendment

PR #948 (decision gd-YGlfkPsFrO, with the owner) would add a `steward_authority` land field (`ask`,
`merge-on-green`, `operate`) and `gb agent kill` within the land for the `lands-tenant` tier.
Neither is built here, and nothing here closes the door on them:

- **`steward_authority`** is a field on the land's project bead, not the role, so the role beads
  do not change. Step 9.3 registers it and renders it into the greeting (above); duty 3 (who
  merges a gardener's PR) and the "Never" list still state the `ask` level: the tenant merges on
  GitHub.
- **`gb agent kill` within the land** is a permission of the `lands-tenant` tier, so it lands in
  10.1's tier check, plus one line in the playbook saying when the steward may use it.
