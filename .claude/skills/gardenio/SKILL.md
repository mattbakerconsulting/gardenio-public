---
name: gardenio
description: >-
  Get a repo onto gardenio and working with agents in minutes: onboard a new
  machine or pet project, diagnose a broken setup (doctor), join the fleet as a
  local agent, run a first task, or answer "how do I ...?" about beads (kd),
  gb, decisions, escalations and the Slack thread workflow. Use for "/gardenio
  onboard", "/gardenio doctor", "/gardenio join", "/gardenio first-task",
  "/gardenio how do I <x>", and when kd/gb fails (401, "login session expired",
  can't reach the daemon, version skew) or someone asks how gardenio works.
---

# gardenio

Gardenio runs Claude Code agents against a shared bead tracker and reports
everything in Slack: an agent card and a thread per task, decisions as buttons,
privileged actions as approve-or-deny escalations. This skill gets a person (or a
Claude session acting for them) from "never seen it" to "my laptop is on the roster
and has opened its first PR", on their own repo.

Slack is the headline. Wherever a step below can be seen in Slack, tell the user
where to look and what they should see, because that is the moment it becomes real.

Pick the mode from what the user typed, or ask once if it is unclear:

| Mode | When | Section |
|---|---|---|
| `onboard` | new machine or new repo, nothing works yet | [Onboard](#onboard) |
| `doctor` | something fails, or "am I set up right?" | [Doctor](#doctor) |
| `join` | start a local agent on this machine | [Join as a local agent](#join-as-a-local-agent) |
| `first-task` | the agent is on the roster, give it work | [First task](#first-task) |
| `how-do-i` | a workflow question | [How do I](#how-do-i) |

Commands, env vars and failure catalog are in [`reference.md`](reference.md).
Treat the binaries as the source of truth for the command surface: when this file
and `gb <cmd> --help` disagree, trust `--help` and say so.

## Ground rules

- **Read-only first.** Diagnose and show the plan before writing shell rc files,
  installing binaries, or creating beads. Ask before each of those.
- **Never print a token.** Report only "set (length N)" for credentials.
- **A local agent acts as its human owner.** It has the owner's permissions, no
  more: the projects and repos that person can reach in the viewer. Anything the
  owner cannot do directly is what escalations (`gb escalation create`) are for.
- **Say what is not built yet.** Some pieces are still landing (see
  [Status of the pieces](#status-of-the-pieces)). Never invent a command; check
  with `--help`, and if it is missing, say so and use the fallback given here.

## Onboard

Goal: `kd` and `gb` reach a daemon as this person, and a `type:project` bead
exists for the repo in the current directory.

0. **Script first.** In a gardenio checkout, `scripts/onboard.sh` does steps 1-5
   below: a read-only doctor pass, then a plan, then a yes/no for each change.
   Run `scripts/onboard.sh --doctor` to show the user where they stand. The
   steps below are the manual form, for when there is no checkout.
1. **Platform.** `uname -s` (Linux, Darwin; WSL reports Linux). Nothing below is
   Mac-only. Note whether `claude` and `git` are on `PATH`.
2. **Binaries.** `command -v kd gb coop`. `kd` and `gb` are required; `coop` is
   needed to run an agent session (`gb agent start --local` drives it). If any
   are missing, point at the release page for the repo
   (`https://github.com/mattbakerconsulting/gardenio/releases`): each calver
   release carries `kd-<os>-<arch>.tar.gz` and `gb-…` for `darwin-arm64`,
   `darwin-amd64` and `linux-amd64`, each with a `.sha256`
   (`gh release download <tag> -p '<bin>-<os>-<arch>.tar.gz*'`, then
   `shasum -a 256 -c` and untar into `~/.local/bin`; the full recipe is in
   `docs/onboarding-mac-operator.md`, "Prebuilt binaries"). `coop` ships for
   `linux-amd64` only (the tarball also holds `coopmux`). On macOS, build coop
   from source: `cd coop && cargo build --release`, about 6 min on an M-series
   Mac; it links Apple frameworks, so it cannot be cross-built (ka-0000001190).
   A release older than the binaries, or another platform, builds from source:
   `go build ./kbeads/cmd/kd ./gardenio/controller/cmd/gb` from a checkout. Ask
   before installing anywhere.
3. **Daemon.** Ask which they have: an existing gardenio daemon URL (most people),
   or none yet. If none, the zero-cluster path is the hosted **demo land**
   (runbook `gardenio/docs/lands/demo-land.md`). They open `/lands/demo` on the
   project's viewer, sign in with GitHub, accept the terms and get a guest pass
   for 24 hours. Then:
   `export BEADS_HTTP_URL=https://api.clonk.tv/land/<land>`, `gb login` (after
   the pass is ready), `gb ping`, `kd list`. A guest reads the land and posts
   messages; it cannot spawn agents, so stop at step 5 for them, and steps 6-7
   need a land of their own (`/lands/apply`). If `/lands/demo` says there is no
   demo land, it is not open on that server yet (see status). Do not improvise
   a cluster. There is no docker compose path; the owner chose hosted
   (decision gd-cGMRpc4uLM).
4. **Credentials.** Export `BEADS_HTTP_URL` (the daemon) and
   `BEADS_OPERATED_BY` (`oidc:<email>` or `slack:U...`: the human these writes are
   attributed to). Auth is one of: `gb login` or `kd login` (the same Keycloak
   device code: the user opens the printed link in their own browser and signs
   in; kd and gb then send that person's token, refreshed automatically), or a
   bearer in `GARDENIO_TOKEN`. Ask which the deployment uses; a public
   bearer-JWT endpoint such as `https://api.clonk.tv` accepts only the
   `gb login` / `kd login` token. The user must start the device code themselves. Offer to append the
   exports to the user's shell rc, show the exact lines first.
5. **Verify** with the [doctor](#doctor) checks until they are green.
6. **Register the repo.** From the repo root, with the remote already set:

   ```sh
   gb project create <name> --git-url "$(git remote get-url origin)" --dry-run
   gb project create <name> --git-url "$(git remote get-url origin)" --account <account>
   ```

   `--dry-run` validates without writing. `--account` names the `type:account`
   bead holding LLM credentials; list candidates with `kd list --type account`
   and ask which. It writes the project bead only; repo push credentials are a
   separate step, and the command prints what remains. Creating a project needs
   an OIDC `gardenio-admin` token (`gb login`): the daemon refuses it on the
   shared static token and fleet tokens (403, "requires gardenio-admin").
7. **Hand off** to [Join as a local agent](#join-as-a-local-agent).

## Doctor

Read-only. Run all of these, report one line each as ok / advisory / broken, and
for every broken line give the one fix, not a list of possibilities.
In a gardenio checkout, `scripts/onboard.sh --doctor` runs these checks for you
(exit 1 when something is broken); read its output, then add what it skips.

```sh
kd --version; gb --version        # both present, same calver as each other
gb ping                           # round-trip to the daemon; nonzero exit = unreachable
gb prime                          # identity, claimed work: who the daemon thinks you are
gb agent roster                   # is a local agent already registered?
git rev-parse --show-toplevel     # inside a repo?
command -v claude coop            # agent session prerequisites
```

Also confirm `BEADS_HTTP_URL` and `BEADS_OPERATED_BY` are set (never echo a token),
and that `gb prime` names the person, not a service identity. Match each failure
to the catalog in [`reference.md`](reference.md#symptom--cause--fix): 401, "login
session expired", wrong identity, unreachable daemon, version skew, no workflow
context in a bare `claude`.

If `kd`/`gb` are more than about two weeks behind the daemon, the CLI prints a
one-line warning on stderr. Treat a bug report from a stale binary as unconfirmed
until reproduced on a current one.

## Join as a local agent

A local agent is a Claude Code session on this machine, registered with the
daemon, visible in `gb agent roster`, claimable like a pod. Today it pulls work;
it does not yet receive Slack nudges (see status).

**Prompt for the local role.** Do not assume one. Run `gb role list` and ask:

> Which role should this agent run as? (Roles set the model and the behavioural
> instructions. If a `local` role is listed, that is the one built for laptops.
> If not, `crew` is the usual stand-in.)

Then summarise what the choice means in one sentence: the role's instructions
tell the agent how to behave; they do not widen or narrow its permissions, which
come from the owner. Do not create a role unless asked; `gb role scaffold <name>`
exists for that and writes beads.

Start it, attached, from the repo root:

```sh
gb agent start --local --role <role> --project <project-name>
# optional: --name <agent-name>, --dir <path>
```

It registers on start, heartbeats, and deregisters and closes its bead on exit
(ctrl-C). Started through `gb runner join` (which passes `--runner`), it also
executes the escalations approved for this machine. Confirm with `gb agent roster` from a second shell, and tell the
user what to look for in Slack (the roster entry, and an agent card once it has a
task).

Run `scripts/gb-claude` (or `gb-claude` if installed) rather than bare `claude` for any hand-started session: a bare
`claude` never goes through coop, so it has none of the gardenio workflow in its
system prompt. `gb agent start --local` already does this for you.

## First task

Goal: one visible loop, Slack thread to PR, on the user's own repo.

1. Pick something small and checkable: a typo, a missing test, a README section.
   Ask the user, offer two suggestions from reading the repo.
2. Create the bead, labelled to the project so it shows up in `gb ready`:

   ```sh
   kd create "<imperative title>" --type task -l project:<name> \
     -d '<what done looks like, in one or two sentences>'
   ```
3. Assign it: start the agent with `--task <bead-id>` (pre-assigned), or let it
   find the bead itself with `gb ready` then `kd claim <bead-id>`.
4. Watch it. The agent works on a branch, opens a PR, links it with
   `gb mr link <bead-id> <url>`, closes the bead and runs `gb stop`.
5. Tell the user where it shows: the thread and agent card in Slack, the bead in
   the viewer, the PR on the host.

Then offer next steps, one line each, and let the user choose: add a second
agent; trigger a decision (`gb decision create`) and answer it in Slack; try an
escalation; read the thread grammar in [`reference.md`](reference.md#the-agent-loop).

## How do I

Answer from live state, not memory. First line of the answer is the command; one
sentence of why follows. Common ones are tabulated in
[`reference.md`](reference.md#how-do-i-quick-answers). For anything else:

```sh
kd search "<words>"        # beads, advice, config, ranked
gb <group> --help          # real command surface
gb prime --system-prompt   # the full workflow rules an agent runs under
```

Keep answers to what was asked. If the question is really "should I?" (shared
infra, privileged action), the answer is an escalation or a decision, not a
command to run.

## Status of the pieces

Check before promising these; they are tracked under epic gd-txA1bV44cU (P5
Onboarding) and the local-agents program.

| Piece | State |
|---|---|
| `gb agent start --local`, roster, `--task`, `gb project create` | shipped |
| A `local` role and local-agent card | prompts shipped: `--role local` gets the laptop system prompt and start nudge (bead workflow, thread grammar, act-as-owner limits) even before `gb role list` shows `local`; the role bead (gd-SzPJDNTBXS) and local-agent card are pending |
| Slack nudges, decision answers and mail reaching a laptop (subscribe-from-agent) | pending; local agents pull work today |
| Onboarding formulas (register-project, add-local-agent, first-task, enable-slack) | shipped as files in `gardenio/formulas/onboarding/` of a gardenio checkout: `kd formula cook <file> --var project=<name> ...` previews, `kd formula pour <file> --var ...` instantiates. Human steps (sign-in, Slack install, credentials, channel binding) are manual gates; account, role and first-task choices are decision gates. add-local-agent and first-task default `role=local`; pass `--var role=crew` until the local role lands |
| Cross-platform `scripts/onboard.sh` (doctor first, idempotent; `--doctor` is read-only) | shipped |
| Prebuilt `kd`, `gb` tarballs on calver releases (darwin-arm64, darwin-amd64, linux-amd64) | shipped for releases cut after `release-binaries.yml` landed; `scripts/onboard.sh` installs them when the latest release carries them |
| Prebuilt `coop` | linux-amd64 only; on macOS build from source (ka-0000001190, gd-gNvy7dymbw) |
| Zero-cluster path: the hosted demo land (`/lands/demo`, guest pass, `gb ping` through the land door) | code shipped (gd-c6LzZkk0qy); live once the operator wires `LANDS_DEMO_*` and opens the land. Probe: open `/lands/demo` |

Probe with `gb role list`, `ls gardenio/formulas/onboarding` (in a gardenio checkout), `ls scripts/onboard.sh`, and the
demo land's `/lands/demo` page. When a
piece lands, prefer it over the manual steps here.
