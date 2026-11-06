# gardenio skill: reference

Backing detail for [`SKILL.md`](SKILL.md). Commands here were checked against the
`kd`/`gb` help output; when they drift, `--help` wins.

## Environment

| Variable | Meaning |
|---|---|
| `BEADS_HTTP_URL` | Daemon base URL. Unset means the CLI cannot find the daemon, or falls back to the browser-session path. |
| `BEADS_OPERATED_BY` | The human these writes are attributed to: `oidc:<email>` or `slack:U...`. Writes without it are refused or misattributed. Same as `--operated-by`. |
| `GARDENIO_TOKEN` | Bearer for outbound requests. `--token` overrides it; `KBEADS_TOKEN`, `BEADS_AUTH_TOKEN`, `BEADS_DAEMON_TOKEN` are also read. |
| `BEADS_NO_VERSION_WARN=1` | Silence the stale-binary warning (auto-suppressed under `--json`). |

Auth is one of:
- `kd login` or `gb login`: the Keycloak device grant (one shared implementation, `gardenio/clicreds/oidclogin`). The CLI prints a link and a code (gb also opens the link); the user signs in in their own browser. The token (with realm roles) is cached 0600 at `~/.local/state/beads/oidc-login.json`, bound to the host of `--http-url`, refreshed automatically, and used by both kd and gb in place of `GARDENIO_TOKEN`. `kd logout` / `gb logout` forgets it. This is the only auth a public bearer-JWT endpoint (e.g. `https://api.clonk.tv`) accepts. Only confirm a code you started yourself.
- a bearer token in the env.

`gb login` used to copy the viewer's browser cookies over a loopback listener (`/auth/cli-handoff` + `/auth/cli-exchange`). That is gone, and both paths now 404. If a user's `gb login` opens a browser at `/auth/cli-handoff`, their binary predates the change; upgrade it and rerun `gb login`.

## Symptom → cause → fix

**`HTTP 401` / `invalid_token`**
With `kd login` / `gb login`, read the stderr line above the 401. "Keycloak ended the login
session" means the refresh token lapsed or was revoked: rerun `kd login`. "could
not reach ... the session is kept, retry in a moment" means Keycloak (or the edge
in front of it) is unreachable, typically a Keycloak restart, which takes about
two minutes; the session is intact, do NOT re-login, just retry. kd retries a
401 once with a freshly refreshed token on its own (gd-NrzwHXUxV0). Otherwise
the bearer is expired, revoked or for a different daemon. Check which one the
deployment expects, mint a fresh one, re-export. Verify the daemon accepts it with
`gb ping`, which is a single health round-trip.

**`login session expired — run gb login`**
The edge bounced the call to a sign-in page: no valid bearer reached it. (Neither
gb nor kd reads the cookie an older `gb login` stored any more; gb says
`gb: ignoring the stored context with no bearer token` when that is all it finds.)
Run `gb login` (a Keycloak sign-in now), and make sure `BEADS_HTTP_URL` points at
the bearer endpoint the session is bound to.

**`gb ping` hangs or times out**
The daemon or the network path, not your credentials. Check the URL for typos and
whether the daemon needs a VPN or an OpenZiti identity (`--ziti-identity` plus
`--ziti-service`). Retry once before concluding anything.

**`gb prime` shows a service identity, or the wrong person**
Writes are going out as whoever the token belongs to. Fix `BEADS_OPERATED_BY`, or
re-login so the daemon sees your identity. A service identity on bead writes is how
work ends up attributed to nobody.

**A bare `claude` does not know the workflow (session close, kd/gb surface)**
It never went through coop, so its system prompt has no gardenio rules. Start it
with `scripts/gb-claude`, which splices in `gb prime --system-prompt`, or use
`gb agent start --local`.

**`kd`/`gb` disagree with the daemon, or a fixed bug reappears**
Version skew. Compare `kd --version`, `gb --version` and what the daemon reports
(the CLI warns on stderr past about 14 calver-days). A `dev` version means the
build carried no calver stamp, so the warning cannot work: rebuild with the version
ldflags or take a release binary.

**`gb agent start --local` fails immediately**
Check that `coop` and `claude` are on `PATH`, that you are in the repo you mean
(`--dir`), and that the daemon is reachable. A previous crash can leave a stale
registry entry and an orphaned agent bead: `gb agent reap-local` cleans both.

**Agent registered but never picks up work**
It is only pulling. Give it a bead (`--task <id>`, or label a bead
`project:<name>` so `gb ready` lists it), and make sure `--project` matches.

## The agent loop

The rules agents run under are printed by `gb prime --system-prompt`; this is the
short form.

```
gb ready                → what is claimable (open, unblocked)
kd claim <id>           → mark it yours (assignee + in_progress)
... work on a branch, commit, push, open a PR ...
gb mr link <id> <url>   → bridge tracks the PR
kd close <id>           → done, unless the fix is on an unmerged PR
gb stop                 → despawn; exiting without it just restarts the agent
```

Need a human: `gb decision create` with two or more genuinely different options,
then `gb yield` to wait for the answer. Need a privileged action you cannot do:
`gb escalation create`. Status for the operator: `gb squawk '<msg>'`.

The thread grammar in Slack: one thread per task; the agent card at the top shows
state and the claimed bead; decisions arrive as buttons; escalations as
approve/deny cards that name the operation and where it will run; the final
message is the wrap-up from `gb stop`.

## How do I: quick answers

| Question | Answer |
|---|---|
| See what I can work on | `gb ready` |
| See what teammates are doing | `gb news` |
| Search for prior work or advice | `kd search "<words>"` (add `--semantic` for fuzzy) |
| Read a bead | `kd show <id>` |
| Create a task | `kd create "<title>" --type task -l project:<name> -d '<desc>'` |
| Start working on a bead | `kd claim <id>` |
| Link a bead to its epic | `kd dep add <child> <parent> --type parent-child` |
| Mark a blocker | `kd dep add <bead> <blocker> --type blocks` |
| Ask a human to choose | `gb decision create ...` then `gb yield` |
| Get a privileged action done | `gb escalation create` (see `--help` for operation types) |
| Send a status line to your thread | `gb say '<msg>'` (alias: `gb squawk`); `gb say --thread <head-id>` / `--channel <name>` elsewhere; `gb thread` to read |
| Tell the daemon about a PR | `gb mr link <bead-id> <url>` |
| Finish a session | `kd close <id>`, then `gb stop` |
| See who is on the roster | `gb agent roster`; `gb agent list --local` for this machine |
| Stop a local agent | ctrl-C in its terminal, or `gb agent stop` |
| Clean up after a crash | `gb agent reap-local` |
| List available roles | `gb role list` |
| Add a role | `gb role scaffold <name> [--from <role>]` |
| Add a project | `gb project create <name> --git-url <url> --account <acct>` |
| Wire a Slack channel to a project | `gb project channel add <channel> --role <role>` (`list`, `rm`) |
| Repeat a known sequence of steps | `kd formula list`, then `kd formula pour <name>` |
| Re-orient after a context reset | `gb prime` |

Use `kd` for bead CRUD and `gb` for orchestration. Avoid commands that open
`$EDITOR`; they hang a headless session.

## Trust model, in two lines

A local agent runs with its owner's permissions: the projects visible to that
person in the viewer and the repos they can push to. It is not a sandboxed
role. The default direction of escalation is cloud to local: a cluster agent asks
a named human's machine to do something, that human approves, their local agent
executes. A laptop asking the cluster for privileged work is the second case.
