# The gatehouse: the front door for people with no land

Task gd-vXokVcVUf8 (Lands Track B step 9.12). Decision D1 gd-B5HQhmG1cJ chose a
gatehouse land with its own steward as the front door for everyone who signs in
and has no land yet. The cards that shaped it:

| Card | Answer | What it means here |
|---|---|---|
| D2 gd-R0hspYDBbq | no-room | The steward greets and answers in the gatehouse `#general`. No private room is built. |
| D3 gd-NLc4LzmCAE | on-claim-and-tour | First contact (greet on claim, tour on first `/garden`) is step 9.14 gd-bW2rxexVIi. |
| D4 gd-pbDt7wyQ3z | apply-and-demo | The steward will take an application in conversation and hand out demo passes. Those tools wait for the 10.14 review gd-Ahv1F4wPUO. Until then it points to `/lands/apply` and `/lands/demo`. |
| D5 gd-lDbe3cv9Gh | opus-split | The steward runs on Opus with authority `ask`, and never `operate`. |
| D6 gd-fXOvEv5uD7 | gatehouse-purse | The operator grants the gatehouse a monthly purse. The steward parks at the wall, and the 24 runs a day cap holds. There is no grant or cap per person. |

The gatehouse is an ordinary tenant land. It is provisioned from a plot
application like any other land, and the provisioner binds it a plain tenant
steward. What is new is small:

- **Membership.** Being a guest means sitting in the gatehouse's Keycloak group
  `project-<gatehouse>/guests`, plus `lands-tenants`. A guest list kept on a bead
  would grant nothing: kbeads builds a person's lands only from their token's
  groups. The edge admits `lands-tenants` to `/garden`, and that is how a guest
  reaches the Commons.
  - **On claim.** When an invite that names no land is claimed, the provisioner
    adds its person to the gatehouse as a guest, unless they already hold a land.
    The invite records `claimed_gatehouse`, and the steward is woken to welcome
    them by name in `#general` (step 9.14, [steward.md](steward.md)).
  - **Backfill, once.** A claimed invite is terminal, so anyone already in
    `invited` with no land never passes the claim again. When the gatehouse is
    open, the provisioner makes each of those people a guest. It then stamps
    `land_gatehouse_backfilled_at` on the gatehouse's project bead and never runs
    the backfill again, so a person you take out in Keycloak stays out. A claim
    made before the gatehouse opens is also caught by this backfill.
  - Nobody is taken out of the gatehouse when they later get a land of their own.
    A person picks up a new group at their next token refresh or sign-in.
- **The mark.** The gatehouse's project bead carries `land_gatehouse=true`. The
  project step stamps it when the land is provisioned as the gatehouse, and the
  provisioner's pass stamps it on a land that was already open.
- **The playbook.** `steward.LoadLand` reads the mark into `Land.Gatehouse`.
  The playbook (`internal/steward/steward.go`) then plays the front door:
  1. **Greet** once in `#general`. The greeting says that every newcomer reads
     `#general`, so nobody should post anything private there, and that the
     steward will never ask for anything private. On a `person-joined` wake it
     also posts one short welcome to the person who just joined. Backfilled
     guests are not welcomed one by one.
  2. **Answer newcomers** in their own thread: what a land is, how to apply
     (`/lands/apply`), and how the demo works (`/lands/demo`). The steward never
     promises a land, credits or a pass.
  3. **Build nothing.** No plans and no spawns.
  4. **No digest.**
  5. **Park at the wall**, as any tenant steward does.

  A tenant land without the mark, and the home garden, keep their own playbooks
  unchanged.
- **The viewer.** `/` sends a person whose only land is the gatehouse to
  `/garden/c/<gatehouse>/general`. The Lands home (`/lands/`) is public and cannot
  tell who is signed in. With a gatehouse configured, it says where someone who
  was let in will land, and that every newcomer reads its `#general`.

## Setting it up (operator)

Crews do not edit `gardenio/helm/**`, `deploy-values/**` or GitHub org settings,
so the operator does these steps (escalation filed from gd-vXokVcVUf8).

1. **The repo.** Create a small public repo for the gatehouse that holds only a
   README, and install the Lands GitHub App on it. A steward refuses a land with
   no repo (`steward.LoadLand`). The repo **cannot** be in an org listed in
   `agents.lands.operatorGithubOrgs`. That list includes `mattbakerconsulting`,
   where gardenio itself lives, by default. The daemon refuses a Lands App land
   bound to one of those orgs, and the provisioner's project step would fail.
   Use a GitHub account or org that is not on the list.
2. **Open the land.** As the operator, apply for a plot at `/lands/apply` with
   that repo and the land name `gatehouse`. Approve it with the monthly purse as
   the credit grant (D6) and an Opus model (D5). The approval's model becomes the
   project's `model`, which the steward runs on. Leave `steward_authority` unset;
   unset means `ask`.
3. **Wire the settings.** These need a small chart change first, then the values:

   | Env | Component | Value |
   |---|---|---|
   | `LANDS_GATEHOUSE_LAND` | controller **and** beads-viewer | `gatehouse` |

   The provisioner refuses to start if the value is not a land name, or if it is
   the same as `LANDS_DEMO_LAND`: a demo pass that ended would take its guest out
   of the gatehouse. The viewer logs `lands gatehouse misconfigured` and leaves
   `/` as it was.
   The order of steps 2 and 3 does not matter. A land that opens before the
   setting is in place is marked and backfilled on the first pass after it.
4. **Roll out** the controller and the viewer in the next release window.
5. **Check it.** Run `kd show <gatehouse project>` and confirm that it shows
   `land_gatehouse=true` and a `land_gatehouse_backfilled_at` stamp. Then sign
   in as a person with no land, with a fresh invite (`gb lands invite <email>`).
   Open `/`, which should land in the gatehouse's `#general`, and confirm that
   the steward answers a message there.
6. **Each month,** grant the purse: `gb lands credit add gatehouse <credits>`.

## Running it

```sh
gb lands purse gatehouse          # what the steward has spent this month
gb lands notes gatehouse          # the steward's memory
kd list --type invite --status closed   # claimed_gatehouse shows who joined on claim
```

- **Take someone out.** Remove them from `project-gatehouse/guests` in Keycloak.
  Nothing puts them back.
- **Quiet it.** `gb lands freeze gatehouse` refuses Commons posts and spawns.
  `gb lands thaw gatehouse` undoes it.
- **Turn it off.** Unset `LANDS_GATEHOUSE_LAND` on both components. New claims
  stop joining the gatehouse, and the people already in it stay guests until the
  land is closed.

## The steward's apply and demo tools (D4)

Decision D4 gd-pbDt7wyQ3z chose apply-and-demo: the steward takes an
application in conversation and hands out demo passes (task gd-K62coPtKkX). The
tools are built and **off**. They stay off until the 10.14 security review
gd-Ahv1F4wPUO signs off, because the steward is the first agent that reads free
text from people who are tenants of no land.

**Turn them on** (operator, after the sign-off): set the boolean field
`land_gatehouse_tools=true` on the gatehouse's project bead. That one field
switches every part: kbeads admits the steward's request, the viewer answers
it, and the playbook's duty 2 uses the tools. Unset or false turns them off
again, and duty 2 goes back to pointing at `/lands/apply` and `/lands/demo`.
Only an OIDC `gardenio-admin` writes the field, either way: kbeads refuses it
from the static and fleet tokens and from the Lands provisioner, like the
other Lands kill switches (`land_switch_authz.go`, gd-Ymdaxee0sX). The
provisioner may write only its own marks, `land_gatehouse=true` and the
one-time `land_gatehouse_backfilled_at` stamp, on a tenant land.

How a request goes:

1. A newcomer asks in `#general`. The steward runs
   `gb lands gatehouse demo gatehouse --message <id>` or
   `gb lands gatehouse apply gatehouse --message <id> --land-name <n> --purpose "<text>" [--credits <n>]`.
   That files one `type=gatehouse_request` in the gatehouse naming the message.
   kbeads admits one request per (message, kind): asking again for the same
   pair returns the first request, unless it was refused. It admits at most
   `gatehouseRequestsPerHour` (30) requests an hour in the gatehouse
   (`admitGatehouseRequest`, gd-EaTO1lFPAG).
2. kbeads stamps every message an OIDC person writes in the gatehouse with
   their verified GitHub account (`author_github_id`, `author_github_login`),
   and refuses those fields from any client in any land, on create and on every
   later write. A message in any other land, and an approved escalation's
   replay of a message create, carries no stamp (gd-R7meWqNxqr).
3. The viewer's gatehouse reactor, as the Gate's machine principal, reads the
   message and takes the person from that stamp. The steward never names who it
   is for. It refuses a message that is not in the gatehouse, is older than a day,
   or has no stamp, and a person who has not accepted the current terms.
4. **demo**: the demo door's checks run (one pass per GitHub account, at most
   `LANDS_DEMO_MAX_GUESTS` open), and the request becomes a draft. Nothing is
   filed: `/lands/demo` quotes the person's message and the steward's ask, and
   only their own Get a guest pass files the pass, which marks the draft done.
   **apply**: the form's checks run on what the steward took down, and the
   request becomes a draft. The person then opens `/lands/apply`, which quotes
   their message and offers the draft; the form is filled in only when they
   press Use this draft. They connect their repository, check the form and press
   Apply. The Gate files the application and the owner's card exactly as for the
   form, and marks the draft done.

Only the person can prove they control the repository (the GitHub App
install checks their own github_id is an admin of it), so a draft is as far as
a conversation can go. Filing without that proof would drop the check.

### For the 10.14 review: the free-text and taint path

- **What the steward controls**: which message a request names, and for an
  application the land name, purpose and credits. It cannot name a person, a
  GitHub id or an email. It cannot write a `land_invite` or a
  `plot_application`, and it cannot change a request's outcome
  (`gatehouse_request` is admin-gated; the only tenant opening is
  `opGatehouseRequest` in `restricted_ops.go`: the gatehouse's steward, in its
  own land, while the switch is on). A person or a land principal reads no
  `gatehouse_request` (`scopedHiddenTypes`, `read_authz_tenant.go`), so a guest
  never sees another's request, outcome or draft; the person sees their own
  draft through the `/lands/apply` prefill.
- **Who gets the pass or the draft**: the GitHub account kbeads verified on the
  message's write (`stampMessageGitHubAuthor`). A steward led astray by
  someone's words can at worst name another person's message from the last
  day. That person is then offered a pass or a draft on their own page, with
  the message it came from quoted. Nothing is joined, mailed or counted against
  `LANDS_DEMO_MAX_GUESTS` until they ask for the pass, and the apply form is
  filled in only when they choose the draft (gd-EaTO1lFPAG).
- **How much the steward can file**: one request per (message, kind), and 30
  an hour in the gatehouse, so a flood cannot starve the reactor's sweep
  (at most 500 every 10 seconds).
- **Purpose text** is the person's own words. It reaches the operator's card
  only through the Gate, which already fences it in a code block.
- **What a guest can read**: a request lives in the gatehouse, so every guest
  can read it. It holds the message id, the GitHub login (already shown on the
  message) and the outcome, never an email.
- **Taint**: the steward carries the untrusted-input taint. Filing a request is
  not an escalation replay, so the taint does not block it; the outcome is a
  draft the person has to act on themselves.

## Not yet

- The apply and demo tools are off until 10.14 signs off (above).
- When a person's invite is claimed, the steward does not yet greet them by name,
  and there is no tour on their first visit. Both are step 9.14 gd-bW2rxexVIi.
- For a scoped person, the Commons serves the `/garden` page on a navigation to
  `/garden/c/...` only once `/garden` itself is open to scoped people (PR #1668)
  and the Commons page hook serves them (follow-up gd-xopPYSZMbG).

## Code

| Piece | File |
|---|---|
| Claim, backfill, mark | `gardenio/controller/internal/lands/provision/gatehouse.go`, `email_invites.go` |
| Controller env | `gardenio/controller/cmd/controller/lands_provisioner.go` |
| `InvitedMembers` | `gardenio/controller/internal/keycloakadmin/keycloakadmin.go` |
| Playbook | `gardenio/controller/internal/steward/steward.go` (`Land.Gatehouse`), `bind.go` |
| `/` and the Lands home | `gardenio/controller/cmd/beads-viewer/lands_gatehouse.go`, `templates/lands.html` |
| Fields | `kbeads/internal/server/config.go` (`land_gatehouse`, `land_gatehouse_backfilled_at`, `land_gatehouse_tools`, `claimed_gatehouse`) |
| Apply and demo tools: request type, carve-out, message stamp | `kbeads/internal/server/gatehouse_request.go`, `restricted_ops.go` |
| Apply and demo tools: reactor and form prefill | `gardenio/controller/cmd/beads-viewer/lands_gatehouse_reactor.go`, `lands_apply.go` |
| Apply and demo tools: the steward's verbs | `gardenio/controller/cmd/gb/lands_gatehouse.go` |
