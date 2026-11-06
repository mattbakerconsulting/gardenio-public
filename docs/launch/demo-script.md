# In-Slack demo script (six steps)

The five-minute demo for the launch kit (plan gd-YbG2szFjAN, phase 3). Everything happens in
one Slack thread: a person asks for a change, an agent does it, asks the person one question,
gets one privileged action approved, opens a PR, and finishes when the PR merges.

This file is the script the recording (gd-4HBSQUraKz) follows. For each step it lists what
the presenter does, what Slack shows **today** (traced from the bridge code on `main`
8d89f2065 and checked against live threads read-only), what the viewer should see, and the
bugs that stand between the two. The rehearsal step is gd-gNVQWaWnjj.

**Where:** the Slack test workspace (`gardenio/docs/slack-e2e-provisioning.md`) and a
throwaway pet repo. Never a production channel. Nothing in frame may show internal channel
names, real user names other than the owner, tokens, account ids or Acme strings.

## Setup checklist

| # | Need | State on 2026-10-31 |
|---|---|---|
| S1 | Test workspace, test app, test user, a `#demo` channel with the app invited | **Not provisioned.** Owner-only escalation gd-fWeCNSRdCd (Part A of the provisioning runbook). |
| S2 | A gardenio bridge connected to the test app (Socket Mode), mapped to a demo project, with `threadCards: all` | **Does not exist.** The production bridge serves only the production workspace; the chart has no second-bridge option. Venue decision filed on gd-gNVQWaWnjj. |
| S3 | Test-user session on the recording pod (`SLACK_VIEW_STORAGE_STATE`) for `gb slack view` screenshots | Chart wiring shipped (gd-QpZeW4qxAU); the secret is empty until S1. |
| S4 | Pet GitHub repo (public, tiny, e.g. a 50-line CLI) registered with `gb project create <name> --git-url …` | Not created. |
| S5 | The fleet GitHub App installed on the pet repo **with its webhook**, and "Allow auto-merge" on | Not done. Onboarding never mentions it (gd-iLDfMYAgAO). Without it steps 5-6 post nothing. |
| S6 | Presenter's Slack user linked to a gardenio identity, with no laptop runner online (else the task goes to the laptop: "Sent to *x* on your laptop runner.") | Check before recording. |
| S7 | Project `card_theme` chosen (`garden` glyphs 🌱🍃🌳, or `business` ○ ◐ ●) | Garden is the default. |

## The six steps

### Step 1: ask in Slack

**Presenter:** in `#demo`, posts `@gardenio add a --version flag to the CLI that prints the
version from the build`.

**Slack today:**
- No acknowledgement on the presenter's message. Reactions go on the agent card instead
  (🌱 starting, 🍃 working, 🌳 done).
- A streaming message opens in the thread and updates every 3 s:
  `🍃 running commands... (12 tools) [2m30s] · last: Bash(show working tree status)`.
- The agent is named `thread-<slack timestamp>`, e.g. `thread-1793417384-374269`.
- The first stream lines can read `Session: startup` / `Instructions: session_start`
  (seen live).

**Viewer should see:** one card in the thread within a few seconds, named after the task
("add a --version flag"), saying it started.

**Bugs:** gd-oX0Dmg6lx3 (timestamp names), gd-0hRIDexc9U (stream and card both live; raw
session text), gd-sVai0aDv7T (claimed-task line uses the long title).

📸 Screenshot slot: `step1-desktop-light.png` (thread open, card visible).

### Step 2: the card updates in place

**Presenter:** says nothing; lets the card run for about a minute.

**Slack today:** the card is edited in place (at most every 5 s). The header reads
`🍃 running commands... (44 tools) [9m57s] · last: Bash(git status) · thread · *title*`,
and the context line reads ``🧺 82K ctx (41%) · 🍯 $1.23 · ♻️ 17 · `thread-…` ``. The first post
also carries "Decisions thread below", the image tag and the bridge version, which vanish
at the first update.

**Viewer should see:** one card that changes state in place (starting → working, with a
plain-words activity line) and a cost/time readout.

**Bugs:** gd-OkfvH4hCBw (internal bits on the card), gd-0hRIDexc9U.

📸 Screenshot slot: `step2-desktop-light.png`, `step2-mobile-dark.png`.

### Step 3: the agent asks, the person answers in Slack

**Presenter:** the agent has been told (in the ask, or by its role) to confirm the output
format first. A decision appears; the presenter clicks an option and confirms.

**Slack today:**
- The card with buttons goes to the presenter's **DM** (DM delivery is on by default).
- The thread gets only a pointer: `✉️ Decision sent to @presenter — <question> Asked and
  answered in DMs. gd-XXXX`.
- The card: `Decision Needed`, one section per option with `Choose 1`/`Choose 2` buttons, an
  "Other" row that asks for a "Required Artifact Type", and `Dismiss` / `Escalate (Agent)`.
- Clicking opens a "Resolve Decision" modal (`✓ Selected: …`, optional notes, Confirm).
- After confirming, both copies read `✅ Resolved: <label>` and `Chosen by @user via Slack`.
  The question is gone.
- The agent card goes 🧑‍🌾 "decision pending" while it waits, then back to working.

**Viewer should see:** the question card in the same thread, a click, the card turning into
"Answered: <choice> by @presenter" with the question kept, and the agent continuing.

**Workaround for a recording before the fix:** set `SLACK_DECISION_DM=off` on the demo
bridge so the full card posts in the thread.

**Bugs:** gd-Qs9zpSPNJf (DM, not thread; question dropped), gd-zfWrlDdasv (artifact type,
Escalate (Agent), CLI hints).

📸 Screenshot slots: `step3-ask.png`, `step3-modal.png`, `step3-answered.png`.

### Step 4: a privileged action is approved

**Presenter:** the agent needs one privileged action (the beat in gd-BzL2OD4b3n: a privileged
spawn or a read-only cluster check), files an escalation and stops. The presenter approves it.

**Slack today:**
- **There is no Approve button in Slack.** The card's only button is `Review in Web UI`.
  The operator DM says "approval happens there, not in Slack."
- The card shows raw values: `🚨 Escalation Request`, ``Type: `helm_deploy` ``,
  ``Target: `local` ``, ``Sensitivity: `internal` ``.
- After approval in the web UI the card becomes ``✓ Escalation Approved … Method: `web_ui` ``.
- The executor is an agent named `escalation-gd-xxxx`. Its card goes to the project
  channel, not the thread, and the name's link 404s.
- The result card says `Escalation Complete: success` and ``Result: `success` ``. The output
  itself never reaches Slack.
- Push text has read "Escalation success: … denied" (seen live).

**Viewer should see:** a calm "Needs approval" card in the thread naming the action in words,
Approve/Deny buttons with a confirm step, a "Done: <one-line result>" update in the same
thread, and the agent card going back to working on its own.

**Bugs:** gd-GzsOR7JDPj (no approve in Slack), gd-Sv85FM7JFd (raw enums, no result),
gd-Q553TCVjN4 (executor outside the thread), gd-6Ygi5hkqrx (404 links inside agent names).

📸 Screenshot slots: `step4-request.png`, `step4-approved.png`, `step4-result.png`,
`step4-resumed.png`.

### Step 5: a PR opens and review lands in the thread

**Presenter:** the agent opens the PR. The presenter (or a second account) leaves an approving
review on GitHub.

**Slack today** (needs S5, or nothing posts):
- `💬 agent: 📝 Draft PR opened: #N Title · agent/<role>/gd-… → main`, then
  `💬 agent: 👀 PR ready for review: #N Title`.
- A merge escalation card: `🚨 Escalation Request` "Merge PR #N: …", ``Type:
  `gitlab_mr_merge` `` (on GitHub), "Marked ready by gb pr ready.", `Review in Web UI`.
- Reviews: `💬 agent: ✅ *login* approved #N Title · view on GitHub`, with the comment quoted.
- CI: `✅ CI passed on PR #N (abc1234)`, posted only when the result changes.

**Viewer should see:** PR opened → review approved → CI green as three short lines from
GitHub, not from "agent", in the thread.

**Bugs:** gd-B4jpwRxppI ("agent:" prefix, branch with a bead id), gd-ePqHPcuoEa
(gitlab_mr_merge alarm card on GitHub), gd-iLDfMYAgAO (pet repo App + webhook not in
onboarding).

📸 Screenshot slots: `step5-pr-opened.png`, `step5-review.png`.

### Step 6: the PR merges and the card shows done

**Presenter:** merges the PR (or lets auto-merge do it).

**Slack today:**
- The thread gets `💬 agent: ✅ PR merged by <login>: #N Title`.
- **The card does not change on merge.** It went final at the agent's stop, before the merge:
  `🌳 done — <tldr>` with Accomplishments / MRs/PRs / `Beads: gd-…` and a Clear button.
  If the agent said it would wake on merge, the card stays "progressing" with
  "🌾 Restart: Wakes when gd-… merges", and nothing wakes it on GitHub.
- Runs over 120 s add a separate reply: `🌼 agent finished — see card above; …`.
- The merge escalation card stays open on a pet repo; after 12 h a pingback posts.

**Viewer should see:** the merge line, and the card collapsing to `🌳 done — added --version
(PR #N merged)`.

**Bugs:** gd-Nyw0LzsInb (no card update on merge), gd-W3usan6txs (internal fields on the
wrap-up card), gd-fOH615HsE7 (duplicate finished reply), gd-ePqHPcuoEa (stale merge card).

📸 Screenshot slots: `step6-merged.png`, `step6-mobile-light.png`.

## Recording notes

- Hide the sidebar or crop to the thread pane; channel names other than `#demo` must not show.
- Hover-free: link hovers reveal the viewer host and `?glass=<bead id>` URLs.
- Garden theme glyphs: 🌱 starting, 🍃 working, 🧑‍🌾 decision pending, 🌳 done,
  🥀 blocked, 🌾 progressing.
- Capture each step with `gb slack view "<thread permalink>" --out <dir>`; it writes desktop
  and mobile, light and dark. Dark mode only shows when the test user's theme is "Sync with OS".

## Rehearsal log

| Date | Run | Result |
|---|---|---|
| 2026-10-31 | Desk rehearsal: each step traced through the bridge, github-bridge and `gb pr` code on `main` 8d89f2065; live production threads read (never posted to) for rendered text | No live run possible (S1, S2 missing). 16 bugs filed, all linked to gd-gNVQWaWnjj with label `demo-rehearsal`: 7 at P1 (steps 1, 3, 4, 5, 6 cannot read right today), 7 at P2, 2 at P3. Steps 3, 4 and 6 do not work as scripted: the decision is answered in a DM, the escalation is approved off-Slack, and the card does not react to the merge. |
