# tools/slack-view

Playwright script that screenshots a Slack channel or thread in the web client
and dumps the visible message text, so an agent can see how its messages render.
Part of epic gd-UHKfSegMUy. Agents normally run it through `gb slack view`.

```
gb slack view <channel|permalink> [--out <dir>] [--team T…] [--json]
```

`gb slack view` embeds a copy of `slack-view.mjs`
(`gardenio/controller/cmd/gb/slack_view.mjs`; a Go test fails if the two differ, so
after editing the script run `cp tools/slack-view/slack-view.mjs gardenio/controller/cmd/gb/slack_view.mjs`).
It reads the session from `--storage-state` or `$SLACK_VIEW_STORAGE_STATE`, passes the
script's exit codes through, prints the output paths, or with `--json` the
`messages.json` document plus `outDir`, `messagesPath` and a `path` per screenshot.

```
node tools/slack-view/slack-view.mjs --storage-state <state.json> --out <dir> <target>
```

Targets, options, output layout and exit codes are documented in the header of
`slack-view.mjs`. In short: it writes `desktop-{light,dark}.png` (1280x800),
`mobile-{light,dark}.png` (390x844) and `messages.json`, and prints the path of
`messages.json` as the last stdout line. Exit 3 means the session is signed out.

Needs `playwright` + Chromium (present in the agent image) and a Playwright
`storageState` for a test-user session (epic gd-0t6m7X5KLC). The storageState holds
session cookies: keep it out of `--out` and out of git.

## Tests

Hermetic, no Slack: they drive the script against the fixture pages in `test/fixture/`.

```
node --test tools/slack-view/test/slack-view.test.mjs
```

## Known limits

- The Slack selectors (`data-qa="message_container"` etc.) are not yet verified
  against a live signed-in session; none exists until the test-user session lands.
  Override with `--message-selector` / `--scope-selector` if they have moved, and fix
  the defaults in `DEFAULTS` once checked.
- Dark mode is emulated with `prefers-color-scheme`. It only shows if the test user's
  Slack theme is "Sync with OS".
- Mobile is the desktop web client at 390px wide, not Slack's mobile app.
