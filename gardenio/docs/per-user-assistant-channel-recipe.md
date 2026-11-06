# Per-user private gardenio-assistant channel + personal role

Recipe for giving a user their own private `#gardenio-assistant-<initials>` channel plus a
personal role, so `@gardenio` in that channel spawns *their* assistant (with per-user
personality + cross-session memory) instead of the default `thread` role. Established
2026-06-25 for `#gardenio-assistant-mb` (Matt Baker). Repeat per user.

Advice bead: `ka-0000006009`.

## Steps (substitute `<initials>`, `<Full Name>`, `<slack-user-id U…>`)

1. **Create private channel + invite the user** (bot auto-joins as creator):

   ```sh
   gb slack channel-create gardenio-assistant-<initials> --private \
     --purpose "<Full Name>'s private channel for working monorepo tickets with gardenio" \
     --topic "Mention @gardenio to work tickets with your personal assistant" \
     --invite <slack-user-id>
   # -> note the new channel ID (C...).
   ```

2. **Scaffold the `type:role` bead — BEFORE step 4** (the step-4 advice bead needs a live
   role to target, or the hygiene sweep closes it):

   ```sh
   gb role scaffold <initials>-assistant --no-instructions
   ```

   `--no-instructions` is deliberate: the personality lives in the step-4 advice bead, and
   the role still inherits monorepo + thread advice from the global instruction defaults.
   Do NOT skip this (see HISTORY).

3. **Bind channel→project and channel→role** on the monorepo project bead (`ka-0000000797`).
   Both fields are shared across ALL monorepo channels, so use the clobber-safe sub-field
   verbs — ONE command, no read-modify-write:

   ```sh
   kd update ka-0000000797 \
     --field-append slack_channel=<C...> \
     --field-set channel_roles.<C...>=<initials>-assistant
   ```

   Do NOT use plain `-f` on either field: `-f channel_roles=@file` reads the value verbatim
   as a STRING and silently flips the field's type from object to string (`ka-0000000372`),
   and `-f slack_channel=...` clobbers every other channel.

4. **Author the role advice** (private to that user's assistant via AND-grouped labels):

   ```sh
   gb advice add "<one-line summary>" --project monorepo --role <initials>-assistant \
     -t "<initials>-assistant role: <Full Name>'s personal user-assistant" \
     -d "<full role description>" -p 1
   ```

   The role description should say: you are `<Name>`'s personal user-assistant; serve them in
   their private channel; **file anything useful to the user into an advice bead
   (`--project monorepo --role <initials>-assistant`) for later**; inherit monorepo+thread
   advice; follow the MR decision-checkpoint protocol.

5. **VERIFY** — all four must pass before you call the onboarding done:

   ```sh
   gb role list | grep <initials>-assistant                       # role resolves
   gb advice list -l g0:role:<initials>-assistant                 # advice targeted
   gb advice audit --project monorepo --role <initials>-assistant --by-bead | head
                                                                  # personality bead DELIVERED by the real matcher
   gb advice lint | grep <initials>-assistant                     # no dead-role-target
   ```

   GOTCHA: `gb advice list --for <role>` appends its argument as a raw LABEL
   (`cmd/gb/advice.go`), so it prints "No advice beads found" for a perfectly wired role. Use
   `-l g0:role:<initials>-assistant` as shown.

   Step 5 is the step that would have caught the 3-week outage below.

## How it works (mechanism)

- **Channel→project:** `ProjectInfo.SlackChannels` from the `slack_channel` field
  (`beadsapi/projects.go`).
- **Channel→role:** `ProjectInfo.ChannelRoles` from the `channel_roles` JSON; resolved by
  `roleFromChannel()` and applied at `bot_mentions.go` (threadRole override) — so `@gardenio`
  in that channel spawns `role=<initials>-assistant` instead of the default `thread`.
- **Role advice delivery:** the spawned agent's subscriptions include
  `role:<initials>-assistant` + `project:monorepo` (`advice/matching.go`); the AND-grouped
  labels `g0:role` + `g0:project` keep the advice private to that user's assistant.
- **Thread affordances** (`gb slack reply`, respond-then-stop) are inherited automatically —
  the nudge type is auto-detected from `SLACK_THREAD_CHANNEL`, independent of role name.

## HISTORY: why step 2 exists (ka-0000002346)

Until 2026-08-14 this recipe had no role-scaffold step. The `channel_roles` wiring is
INDEPENDENT of the role bead, so every assistant still spawned under the right role name and
looked fine — but with no `type:role` bead, its advice bead targeted a role that formally did
not exist. On 2026-07-25 the advice-corpus hygiene sweep (`ka-0000003292`) correctly flagged
all 11 as "dead-role-target — reaches no agent" and closed them. For ~3 weeks
mbaker/pt/legend/joon/akim/ak/dc/dj/jp/hw/cg ran on global defaults with no personality and no
cross-session memory, and nothing appeared broken. Repaired 2026-08-14: 11 role beads
scaffolded, 11 advice beads reopened, all verified through `gb advice audit`.
