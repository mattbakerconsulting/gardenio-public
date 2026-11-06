---
name: slack-ux-acceptance
description: >-
  Grade how a gardenio Slack message really renders: post a Block Kit payload (a
  card builder's output or a fixture) to the Slack TEST workspace, screenshot it
  with `gb slack view`, score it against the U1-U12 rubric, fix and `chat.update` it,
  re-shoot, and attach before/after PNGs to the bead or MR. Use for "/slack-ux-acceptance",
  "UX-accept this Slack card", "does this card look right in Slack", and when an MR
  changes a Block Kit builder, a card theme, or any bridge message layout.
allowed-tools: "Bash,Read,Write,Glob,Grep"
version: "1.0.0"
author: "gardenio"
---

# slack-ux-acceptance

The loop from the Slack thread gd-H9rqCO6FYU: an agent changes a Slack message, **looks at it
in a real client**, grades it against a rubric, revises, and keeps the screenshots as
evidence. Without the screenshot you are guessing: the Block Kit builders are the suspect,
the rendered pixels are the witness.

| Piece | Where |
|---|---|
| Rubric U1-U12, capture matrix, bands, severities | `gardenio/docs/slack-ux-rubric.md` (read it first) |
| Screenshot tool | `gb slack view` (wraps `tools/slack-view`) |
| Post / edit / lint a payload | `scripts/slack-post.mjs`, `scripts/lint-blocks.mjs` (this skill) |
| Test workspace, app, test user, session refresh | `gardenio/docs/slack-e2e-provisioning.md` |

The skill **observes, grades and files**. A fix to bridge code is a normal change; the
grader does not edit the builder it is grading unless it is also the author of the MR under
review.

## Prerequisites (check all four, stop if any is missing)

Everything posts to the **test workspace**. Never point this at the production workspace or
its channels, and never use the production `SLACK_BOT_TOKEN` (a pod that has one has the
prod bridge's token; this skill does not read it).

| Need | Env | Notes |
|---|---|---|
| Test app bot token | `SLACK_UX_BOT_TOKEN` | `bot-token` in SM `gardenio/prod/slack-e2e/app-credentials` |
| Test channel id | `SLACK_UX_CHANNEL` | the `C0…` the test app was invited to |
| Test team id | `SLACK_TEAM_ID` | `slack-post.mjs` refuses to send if `auth.test` reports another team |
| Test-user session | `SLACK_VIEW_STORAGE_STATE` | Playwright storageState, a file path; keep it out of git and out of `--out` |

```bash
for v in SLACK_UX_BOT_TOKEN SLACK_UX_CHANNEL SLACK_TEAM_ID SLACK_VIEW_STORAGE_STATE; do
  [ -n "${!v}" ] && echo "ok  $v" || echo "MISSING $v"
done
```

**If any is missing, stop and report it. Do not fall back to another token or channel and
do not grade from the payload alone.** The test workspace is provisioned by the operator
(Part A of the provisioning runbook, escalation gd-fWeCNSRdCd) and the session is refreshed
per Part B. A payload lint is not a UX grade; you may run it (Step 2) and say clearly that
only the objective half was done.

Capture tooling needs `playwright` + Chromium, present in the agent image.

## Inputs

Decide the **message under test (MUT)** and write its `chat.postMessage` body to a file:

```json
{ "text": "one-line fallback", "blocks": [ ... ], "attachments": [ { "color": "#...", "blocks": [ ... ] } ] }
```

Get the payload from one of:
- the builder under test: a Go test or small program in `gardenio/controller/internal/bridge`
  that marshals the card (`theme_cards_test.go`, `agents_card_test.go` and `thread_cards_post_test.go` show how the cards are built; build only the
  package you need, never the whole module: pods OOM on `go build ./...`);
- a fixture file already in the repo or in the MR;
- a payload copied from a real posted message (`gb slack channel`/`read-thread` returns
  blocks).

For a card that exists in two card themes (`garden`, `business`), produce one payload per
theme: U5 needs both.

Pick a work dir, e.g. `OUT=$TMPDIR/slack-ux/<bead-id>`, and use `before/` and `after/`
subdirectories when the MR changes an existing card.

## The loop

### 1. Lint the payload (no network)

```bash
S=.claude/skills/slack-ux-acceptance/scripts
node $S/lint-blocks.mjs $OUT/payload.json
```

It checks the rubric §2 limits and the objective halves of U1, U3, U7, U9, U10, U12:
header 150, section 3000, 50 blocks, action_id uniqueness, one primary per row, `danger`
needs `confirm`, empty fallback `text`, missing `alt_text`. Exit 1 means errors, and a
posted message with these errors would return `invalid_blocks`. Warnings (long button
labels, >12 blocks, >600-char sections) go in the grade as U3/U10 notes.

### 2. Post to the test channel

```bash
node $S/slack-post.mjs post $OUT/payload.json            # add --thread-ts TS to post in a thread
# -> {"ok":true,"channel":"C0…","ts":"1791….","permalink":"https://…","lint":{…}}
```

Keep `ts` and `permalink`. It lints first and refuses a failing payload; to capture
Slack's actual `invalid_blocks` answer as U1/U12 evidence, re-run with `--no-lint` once
and record `error` / `errors` from the output. Unfurls are always disabled.

To grade a message in a thread pane (rubric cell `thread-open`), post a plain parent
message first, then post the MUT with `--thread-ts <parent ts>` and view the thread
permalink.

### 3. Screenshot it

```bash
gb slack view "<permalink>" --out $OUT/before    # or: gb slack view $SLACK_UX_CHANNEL --team $SLACK_TEAM_ID --out …
```

Writes `desktop-{light,dark}.png`, `mobile-{light,dark}.png`, `messages.json`. A
permalink with `?thread_ts=` opens the thread pane. Exit 3 means signed out: report
**"session needs refresh per Part B"** (gd-6Jq5oR9fpd) and grade nothing; a blank or
sign-in screenshot is not a FAIL of the message. Exit 4 means the message never appeared:
the post failed or landed elsewhere; report the API error text.

Mapping to the rubric's capture matrix (the tool's sizes differ from the rubric's
nominal ones, which is accepted):

| Rubric cell | `gb slack view` file | Note |
|---|---|---|
| desktop-light / desktop-dark | `desktop-light.png` / `desktop-dark.png` (1280x800) | rubric says 1440x900 |
| narrow-light / narrow-dark | `mobile-light.png` / `mobile-dark.png` (390x844) | proxy: web client at phone width, not the app |
| thread-open | `desktop-light.png` from a thread permalink | |
| card-theme x2 | run Steps 2-3 once per theme payload | |

Dark mode is `prefers-color-scheme` emulation: it only shows if the test user's Slack
theme is "Sync with OS". If `desktop-dark.png` is identical to `desktop-light.png`
(compare `sha256sum`), the toggle did not apply: **U4 is ungraded, not passed**.

### 4. Grade against the rubric

**Open every PNG with the Read tool.** Verdicts come from the screenshot; the lint output
and `messages.json` only triage. Then, per gate U1-U12, decide `PASS` / `FAIL` / `N/A`
using the objective and visual checks in `gardenio/docs/slack-ux-rubric.md` §3.

- `N/A` needs a stated reason (no buttons means U7 is `N/A`). Never `N/A` a cell that was
  merely inconvenient to capture.
- U3 is at best `PASS (proxy)`; anything native-only is "untested".
- U7 post-click honesty is `N/A` for a static grade; say so.
- U4 contrast: sample the pixels (e.g. Pillow `getpixel` on the glyph/bar) rather than
  eyeballing; record the ratio.
- Check `messages.json` text for literal `&lt;`, `:shortcode:`, `<@U…>`, bare `gd-…` ids.
- Every FAIL links its screenshot path (the V12 rule).

Write `$OUT/SLACK-UX-GRADE.md` in the shape from rubric §4 (table of gate / verdict /
cell(s) / evidence, the payload lint output, one paragraph per FAIL as mechanism, where
in code, the fix, and the list of `N/A` gates with reasons).

**Score** = PASS count / (12 - N/A count). 100% accept; >= 83% accept-with-nits (file the
FAILs, do not block); < 83% revise (block the MR until fixed and re-shot).

### 5. Revise: edit, `chat.update`, re-view

When the grade is "revise" and you are allowed to change the payload (it is a fixture, or
you own the builder change):

1. Change the builder or fixture, regenerate the payload.
2. `node $S/slack-post.mjs update $OUT/payload.json --ts <ts>` edits the same message in
   place (U10: a status that updates in place is `chat.update`, not a stream of new
   posts). A `chat.update` that returns `invalid_blocks` leaves the old card up; read
   the error, do not re-shoot as if it worked.
3. `gb slack view "<permalink>" --out $OUT/after`. Re-shoot **only the failing cells** on later
   passes, but keep a full matrix for the final accepted state.
4. Re-grade, writing a fresh `SLACK-UX-GRADE.md` (keep the earlier one as
   `SLACK-UX-GRADE.before.md`). Stop after the score reaches the accept band or after 3
   revise rounds; on round 3 with FAILs left, file the bugs and report.

If you are only grading someone else's MR, skip the edit: file bugs instead (Step 7).

### 6. Attach before/after PNGs to the bead and MR

Which command renders depends on where the reader is (the viewer-proxied URL from
`gb image upload` is browser-session-only and does not render in an MR, a Jira ticket or a
Slack unfurl):

| Destination | Command |
|---|---|
| Bead | `gb image upload $OUT/before/desktop-light.png` for each PNG, then `kd comment add <bead> --body @$OUT/SLACK-UX-GRADE.md` with the URLs appended. The URL needs a human with a viewer session to open it. |
| GitLab MR | `gb mr embed <mr-url-or-bead> before.png after.png --pair` (managed block, safe to re-run) |
| GitHub PR | `gb image upload` URLs in a PR comment link to the evidence; the PNGs do not render in the PR. Put the grade (score, FAIL list, `N/A` reasons) in the comment body as text so the PR stands on its own. |
| Slack thread | `gb slack upload $OUT/after/desktop-light.png --comment "after: U1-U12 score 92%"` renders inline |

Name pairs so they read as before/after: copy to `before-desktop-light.png`,
`after-desktop-light.png` etc. before uploading. The preferred MR evidence is a before/after
pair of the same MUT; for a brand-new card, attach `after` only and say so.

Do not commit PNGs or any `storageState` to the repo. Messages stay in the test channel as
evidence; do not delete them unless asked.

### 7. File what you found

Per FAIL, a bug bead (`kd create "..." --type=bug -l project:gardenio -l slack-ux:<gate>:<surface>`),
priority from the rubric's severity table: **P1** U1 auto-fail, U7 unsafe `danger` / missing
`confirm`, U8 lying card, U12 empty fallback; **P2** U2, U3, U4, U5, U9, U11; **P3** U6,
U10. Dedupe first: `kd list -l slack-ux:<gate>:<surface> --status=open`, and add a comment
to the open one instead of filing a second. Link each to the originating task with `kd dep add`.

## Output contract

When the run ends, report in the bead comment or `gb squawk`:

```
slack-ux-acceptance <MUT / builder> — score <n>% (<band>) — FAIL: U4, U9
grade: <path to SLACK-UX-GRADE.md or comment>  evidence: <before/after PNG links>
bugs filed: <bead ids>   ungraded: <gates + reason>   N/A: <gates + reason>
```

## Failure modes (report, never guess)

| Symptom | Meaning | Do |
|---|---|---|
| `slack-view` exit 3 / sign-in page | test-user session expired | report "session needs refresh per Part B"; grade nothing |
| `slack-view` exit 4 | message not in the DOM | post failed or wrong channel; report the API error; U1/U12 evidence |
| `slack-post` "refusing to post outside the test workspace" | token's team is not `SLACK_TEAM_ID` | stop; wrong token. Do not retry with another |
| `slack-post` exit 2 | missing env | fix the prerequisite; do not improvise |
| `invalid_blocks` from `post`/`update` | a §2 limit was hit | auto-FAIL U1; include `errors` from the output |
| dark == light screenshots | Slack theme is not "Sync with OS" | U4 ungraded; ask for the test user's theme to be set |
| selectors in `messages.json` empty on a good screenshot | Slack moved its `data-qa` hooks | pass `--message-selector`; note it in `tools/slack-view/README.md` Known limits |

## Self-test

The scripts are hermetic-tested against a fake Slack API (no network, no Slack):

```bash
node --test .claude/skills/slack-ux-acceptance/scripts/test/slack-post.test.mjs
```

## Known limits

- No live run has happened yet: the test workspace, app and session are not provisioned
  (gd-fWeCNSRdCd). The selectors in `slack-view` are unverified against a signed-in client.
  The first real run is the dogfood task gd-An3AH4Yqr7; fold what it finds hard back into
  this file and the rubric.
- Native iOS/Android are not reachable; mobile is a 390px web proxy.
- The bridge's own posting path (Socket Mode, `chat.update` from the streaming card) is
  not exercised here. This skill posts a *payload* as the test app; to grade a card exactly
  as the bridge builds it, trigger the real flow in an ephemeral bridge wired to the test
  app, then start at Step 3 with that message's permalink.
