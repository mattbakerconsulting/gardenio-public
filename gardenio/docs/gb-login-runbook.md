# gb login — authentication runbook

User-facing guide to signing the `gb` CLI (and `kd`, which shares the session)
in to the gardenio daemon as yourself.

`gb login` and `kd login` are the same sign-in: the OAuth 2.0 device
authorization grant (RFC 8628) against Keycloak's public `gardenio-cli` client.
The code is shared, in `gardenio/clicreds/oidclogin` (gd-bFFquoScHy for `kd
login`, gd-5FThTGbOOP for `gb login`).

## TL;DR

```sh
gb login                # prints a link + code, opens the link, caches the token
gb login --status       # show the session: who, which daemon, token expiry
gb logout               # forget the session (kd's too: it is the same file)
```

## First-time setup

```sh
gb login
```

gb prints a verification URL and a short code and opens the URL in your
browser. Enter the code there (the link it opens already carries it) and sign
in to Keycloak. No password passes through gb. When you finish, gb prints
`Signed in as <you> (<roles>) for <daemon host>.`

Only confirm a code you started yourself. A process on any machine or agent
pod that can run gb can start a sign-in and ask you to confirm its code;
whoever holds the resulting token acts as you.

Use `--no-browser` on a headless machine: gb then only prints the link, and you
open it on any device.

## What gets stored, and where it is sent

- The session (access token, refresh token, issuer, client, expiry, bound
  daemon host) is cached 0600 in `~/.local/state/beads/oidc-login.json`
  (directory 0700). Tokens are never printed or logged.
- The session is **bound to the host of `--http-url`** at login time
  (`$BEADS_HTTP_URL`, then `$BEADS_HTTP_ADDR`, then the built-in default). gb and
  kd send it only to that host; pointed at any other daemon they do not send it
  and say "login session is not for <host>". To use another daemon, run
  `gb login --http-url <that daemon>`.
- `kd login` writes the same file, so one sign-in serves both CLIs as long as
  they target the same host.
- An explicit `--token` still wins over the session. The session wins over
  `GARDENIO_TOKEN` / `KBEADS_TOKEN` / `BEADS_AUTH_TOKEN` / `BEADS_DAEMON_TOKEN`.

## Refresh, and when you have to sign in again

The access token is short-lived; gb and kd refresh it through Keycloak with the
refresh token, 30 seconds before it expires, and write the new one back.

- **Keycloak unreachable** (typically a Keycloak restart, about two minutes):
  the session is kept. While the cached token is still valid it is used; after
  that the CLI says `could not reach … the session is kept, retry in a moment`.
  Do **not** sign in again; retry (gd-NrzwHXUxV0).
- **Keycloak refused the refresh** (`Keycloak ended the login session`): the
  session idled out, was revoked, or hit its lifetime. Run `gb login` again.

## Flags

| Flag | Meaning | Default |
|---|---|---|
| `--http-url <url>` | Daemon the session is bound to (root flag) | `$BEADS_HTTP_URL` → `$BEADS_HTTP_ADDR` → built-in host |
| `--issuer <url>` | Keycloak realm issuer | `$GARDENIO_OIDC_ISSUER` → `https://auth.clonk.tv/auth/realms/gardenio` |
| `--client-id <id>` | Public client with the device grant | `$GARDENIO_OIDC_CLIENT_ID` → `gardenio-cli` |
| `--no-browser` | Print the link, do not open it | off |
| `--timeout <d>` | Give up after this long | `10m` |
| `--status` | Show the session and any older credstore context | off |

`--viewer-url`, `--cli-exchange-url` and `--context` belonged to the retired
cookie handoff. They still parse, so old scripts do not break, and print a
deprecation note; they have no effect.

## Retired: the beads-viewer cookie handoff

Until gd-5FThTGbOOP, `gb login` opened `<viewer>/auth/cli-handoff`, which
stashed the browser's session cookies under a one-shot code that gb redeemed at
`/auth/cli-exchange` (A1 ka-0000003791, A2 ka-0000007034, AVA era). That handed
browser session cookies to a CLI; it is gone.

Both paths are gone from the viewer. For a month they answered 410 "upgrade
gb"; that stub was deleted in gd-llLzxjXc0K, so they now 404. If `gb login`
opens a browser at `/auth/cli-handoff`, that gb predates gd-5FThTGbOOP: press
Ctrl-C, upgrade gb and run `gb login` again.

A credstore context an older `gb login` left behind
(`$XDG_CONFIG_HOME/gardenio/credentials.json` or the OS keyring) still supplies
its OAuth bearer as a fallback when no session or token applies. A cookie stored
by the handoff is no longer read by gb or kd (gd-bK5sSblLz9, gd-Nza6by1LUT): with
only a cookie there, gb prints `gb: ignoring the stored context with no bearer
token …; run gb login`, and kd falls through to its other credentials.
`gb login --status` shows the context, and `gb logout` clears it with the session.

## Troubleshooting

**`login session is not for "<host>" (signed in for "<other>")`.** You signed in
for a different daemon host than this command targets. Run
`gb login --http-url <the daemon you use>`, or point `BEADS_HTTP_URL` at the host
you signed in for.

**`client "gardenio-cli" cannot use the device grant`.** The Keycloak client is
missing or does not have the device grant enabled; it is declared in
deploy-values (`keycloak.clients`). Operator fix.

**`the sign-in code expired before it was used`.** You took longer than the code's
lifetime (Keycloak's setting, usually 10 minutes). Run `gb login` again.

**`sign-in was denied`.** You (or someone) pressed Deny on the consent page.

## See also

- `gardenio/clicreds/oidclogin/oidclogin.go` — the shared device grant, cache and refresh.
- `gardenio/controller/cmd/gb/login.go`, `kbeads/cmd/kd/login.go` — the two command surfaces.
- `gardenio/controller/cmd/beads-viewer/auth_cli.go` — the 410 stubs for old gb binaries.
- [`agent-viewer-login.md`](agent-viewer-login.md) — `gb viewer-login`, the headless agent-pod path.
