# gl: the tenant's CLI for a land (v1)

`gl` works one land from a terminal: its tickets, decisions, escalations,
plans, threads, budget and live feed. It speaks only the Lands contract v1
(`../contract/openapi.yaml`) through the land door,
`https://<edge>/land/<name>/v1`, with a sign-in minted for that one land. It
holds no other credential and learns no other address. `gl` is the working
name until the owner confirms it.

Design: contract design gd-EWSKPH7pa5 (section 4) and boundary recommendation
gd-uYNbyW4a9Z (section 6, steps 5 and 7). Task gd-bR8MiyOpbe, plan
gd-vmw5s07A1F.

## Commands

```sh
gl login --land acme          # device code: finish signing in in your browser
gl use acme                   # the default land (the first login sets it)
gl whoami                     # you on the land, and both contract versions
gl ticket list|show|open|search|comment|close|reopen
gl decision list|show|resolve <id> --option <id|number|label>|cancel|comment
gl escalation list|show|create --title ... --reason ...|approve|reject <id> [--open]
gl plan list|show|answer <plan> --decision <id> --option ...
gl thread list|show <id>
gl say <thread> <text>        # post to a thread
gl budget [--history]         # --history: credits per UTC day (--days 1-30)
gl watch [--since <cursor>]   # the live feed, reconnecting on its own
gl agent start [--name n] [--dir d] [--ticket id]   # an agent on this machine
gl agent list|stop <id>
gl logout
```

Every command takes `--json` (the contract objects as they came; `watch`
prints one event per line) and `--land <name>`. Lists take `--limit` and
`--cursor`, and print the next cursor on stderr.

**Which land.** `--as-tenant`, then `--land`, then `GL_LAND`, then the land
`gl use` remembered (`gl.json` in the user config dir, e.g.
`~/.config/gardenio/gl.json`), then the only land you are signed in to.
**Which edge.** `--edge`, `GL_EDGE`, `gl use --edge`, then
`https://api.clonk.tv`; https only (plain http only to this machine), no path.

**Sign-in.** `gl login` is the shared Keycloak device grant
(`gardenio/clicreds/oidclogin`, the code behind `kd login` and `gb login`),
requesting the `land-<name>` scope, so the token's one audience is that land
(owner decision gd-zf6eceweED). The session is cached per land
(`~/.local/state/beads/oidc-login-land-<name>.json`, 0600), bound to the
edge's host, and the same file `gb login --http-url https://<edge>/land/<name>`
writes. `gl` renews it before it lapses; when the land answers 401 it renews
once and resends the same request (same `Idempotency-Key`, so it cannot act
twice). Every POST carries a fresh `Idempotency-Key`.

**Escalations are approved in the browser.** `gl` opens and reads them;
approving or rejecting one happens only on its page in the browser or on a
phone (device-grant tokens are refused for approval, P10.B), so `gl escalation
create`, `show`, `approve` and `reject` print the link and `approve`/`reject`
send nothing (`--open` also opens it; the link is `--web`, `GL_WEB`, then
`https://gardenio.clonk.tv`).

**Contract skew.** Every response carries `X-Lands-Contract`. When it differs
from the version `gl` was built for, `gl` warns once and carries on.

## An agent on your own machine: `gl agent start`

`gl agent start` runs an agent for the land on your machine. The land puts it
on its crew and answers a credential for that agent alone (`createAgent`);
`gl` then runs `gb agent start --local` with it at the land door, and the
session is the same as a land pod's: `kd` and `gb` under the agent's own land
token, primed as `lands-gardener`. When the session ends (exit claude, or
Ctrl-C) `gl` stops the agent on the land (`revokeAgent`). `gl agent list`
shows the land's laptop agents; `gl agent stop <id>` stops one.

- **Install first:** `gl`, `gb`, `kd` and `coop` on `PATH`, and `claude`
  signed in. `gl` checks for `gb` and `coop` before it asks the land, so a
  missing tool never takes one of the land's agent slots. On Linux, `coop`
  comes from the `coop-linux-amd64.tar.gz` asset of a gardenio release; on
  macOS, build it from `coop/`.
- **Who pays:** whatever `claude` on your machine is signed in with
  (decision gd-09hIjRBKvf). `gl` sets no model address or key, and the
  land's budget is not spent.
- **What the agent may do:** whatever your own role, it is a land agent at
  the restricted tier: it works tickets, comments and posts in threads, and
  cannot answer decisions, approve escalations or start other agents.
- **It never runs as you.** `gl` drops your `kd`/`gb` tokens, daemon address
  and project from `gb`'s environment, passes the agent's token in a file
  named by `GARDENIO_TOKEN_FILE` (0600, in a directory of its own, removed
  when the session ends), and sets `GARDENIO_NO_LOGIN_SESSION=1`, which keeps every
  `kd` and `gb` in the session from sending your cached sign-in for the land
  (both send a sign-in before any token in the environment). The token goes
  only to the door at the edge `gl` talks to; a land that names its door
  anywhere else is refused.
- **Renewed while it runs:** the agent's credential lasts about 24 hours.
  `gl` stays running beside the session and, past two thirds of that, renews
  it at the door on the agent's own token, rewriting the file that `kd` and
  `gb` read; the renewal value lives only in `gl`'s memory. A machine that
  slept past the end renews on your sign-in instead (run `gl login` in
  another terminal if that has lapsed too). Older `kd`/`gb` that do not read
  the file fall back to `GARDENIO_TOKEN`, the first token, for its day.

`gl agent start` refuses a support view (`--as-tenant`). Runbook:
`gardenio/docs/local-agent-runbook.md`, "A tenant's agent".

## Support: `--as-tenant`

Operators keep `kd` and `gb`. `gl --as-tenant <land> <command>` shows a land
exactly as its members see it, through the same door and contract; it is the
only operator feature and adds no verbs. Sign in for it with
`gl --as-tenant <land> login` (that never moves your default land). `gl`
prints a support banner on stderr, and anything you change is recorded under
your own sign-in (the land stamps `oidc:<email>` from your token). The door
lets you in only while your token makes you a member of that land. If `gl`
does not show a field, use `kd`: ids are the same, so a tenant can paste one
into a support thread and you can `kd show` it.

## Tests

```sh
make test-lands-gl            # or: cd lands/gl && GOWORK=off go test ./...
```

The module is outside `go.work` on purpose. Its tests drive every command
against a fake land that answers with the contract's golden fixtures
(`../contract/fixtures`), a fake Keycloak for sign-in and renewal, and
`deps_test.go`, which fails if `gl` imports anything from this repo except
`lands/contract/landsv1` and `clicreds/oidclogin`, or gains a `replace`
beyond those two modules, or a `go:embed`. CI: `checks.yml` job `lands-gl`.

Not here yet: release packaging of the binary, and the D.11 `strings`
tripwire over it (`../contract/hygiene_denylist.txt`). That tripwire will find
`daemon` (the shared session file's field name) and `bead` (the web
approval link's `?bead=` parameter), both from code `gl` shares rather than
from anything it says itself, and `bead` and `kbeads` again from the
environment prefixes `gl agent start` strips before it runs `gb`.
