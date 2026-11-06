# Quickstart: a gardenio agent opens a PR on your repo

The shortest path from cloning gardenio to a first agent PR on your own repo.
The target is about ten minutes once the [prerequisites](#before-you-start-the-clock)
are in place. It runs on Linux, macOS and WSL.

Already in Claude Code? The `gardenio` skill (`.claude/skills/gardenio`) walks
you through the same steps and checks each one: run `/gardenio onboard`, then
`/gardenio join` and `/gardenio first-task`. When something fails, run
`/gardenio doctor`.

| Step | What you do | Target |
|---|---|---|
| [1](#1-install-kd-and-gb-and-sign-in) | Clone gardenio, run `scripts/onboard.sh`, sign in | 3 min |
| [2](#2-register-your-repo) | `gb project create` for your repo | 1 min |
| [3](#3-start-a-local-agent) | `gb agent start --local --role local` | 1 min |
| [4](#4-give-it-a-first-task) | `kd create` a small task and hand it over | 5 min, mostly the agent working |

## Before you start the clock

These take longer than ten minutes the first time, so do them first.

- **A gardenio daemon and an account on it.** You need its URL (for example
  `https://gardenio.example.com/kd`) and a sign-in that `gb login` accepts.
  Registering a project needs the `gardenio-admin` role; without it, ask whoever
  runs your gardenio to do step 2 for you. No daemon at all? See
  [No daemon yet](#no-daemon-yet-the-demo-land).
- **Read access to the gardenio repo**, so `git clone` works.
- **Claude Code**, installed and signed in (`claude` opens the browser sign-in on
  first run).
- **`gh`, signed in to the host of your repo** (`gh auth login`), with push
  access to that repo. The agent works as you: it pushes a branch and runs
  `gh pr create` with your credentials.
- **`coop`**, which runs the agent session. On Linux, take
  `coop-linux-amd64.tar.gz` (it holds `coop` and `coopmux`) from the
  [releases page](https://github.com/mattbakerconsulting/gardenio/releases). On
  macOS there is no prebuilt coop; build it from source, which takes about six
  minutes on an M-series Mac and needs a Rust toolchain
  ([rustup](https://rustup.rs)), so start it before anything else:

  ```sh
  git clone --depth 1 https://github.com/mattbakerconsulting/gardenio.git ~/gardenio
  cd ~/gardenio/coop && cargo build --release
  mkdir -p ~/.local/bin
  install -m 0755 target/release/coop target/release/coopmux ~/.local/bin/
  ```

- **Go 1.25 or newer, only if the latest release has no `kd`/`gb` tarballs.**
  `onboard.sh` then builds them from your checkout instead.

## 1. Install kd and gb, and sign in

```sh
git clone --depth 1 https://github.com/mattbakerconsulting/gardenio.git ~/gardenio   # skip if you cloned it for coop
~/gardenio/scripts/onboard.sh --url <daemon URL> --operated-by oidc:<your email>
```

`onboard.sh` runs a read-only check first and shows its plan. Then it asks
before each change: it installs `kd`, `gb` and `gb-claude` into `~/.local/bin`,
writes a marked env block (`PATH`, `BEADS_HTTP_URL`, `BEADS_OPERATED_BY`) into
your shell rc, wires the gb hooks into `~/.claude/settings.local.json`, and starts
`gb login`. Open the link `gb login` prints, check the code matches, and sign in.
A second run changes nothing. The full guide is
[`onboarding-mac-operator.md`](onboarding-mac-operator.md) (it covers every
platform, despite its name).

Open a new shell, then check:

```sh
gb ping                 # ok
gb prime | head -20     # names you, not a service identity
kd list --limit 2       # live beads
```

Anything red? `~/gardenio/scripts/onboard.sh --doctor` is read-only and prints
one fix per broken line.

**In Slack:** nothing yet. Signing in does not post anything.

## 2. Register your repo

From the root of your repo, with `origin` pointing at the host the agent will
push to. Pick a short project name; it becomes the `project:<name>` label on
every bead for this repo.

```sh
cd ~/src/my-repo
gb project create my-repo --git-url "$(git remote get-url origin)" --dry-run
gb project create my-repo --git-url "$(git remote get-url origin)"
```

`--dry-run` checks the fields and writes nothing; read the `default_branch` line
it prints. It assumes `main`: if your repo's default branch is something else
(`master`, say), add `--default-branch master` to both commands. The second command writes the
`type:project` bead and prints what is left to wire. A project worked only by
your local agent needs nothing more: the agent pushes with your own `git` and `gh`
credentials and runs on your own Claude Code sign-in. Add
`--account <type:account bead>` (list them with `kd list --type account`) only
when cluster agents should work the repo too.

If `gb project create` says `requires gardenio-admin`, you are signed in without
that role: ask your gardenio operator to run the same command, then go on.

Prefer a checklist that tracks itself? The `register-project` formula pours the
same steps as beads, with sign-in as a manual gate and the account choice as a
decision. The formula always asks for `account`, even for a repo only your
local agent works; pass any `type:account` bead from `kd list --type account`.
Pass `default_branch` when it is not `main`:

```sh
kd formula pour ~/gardenio/gardenio/formulas/onboarding/register-project.json \
  --var project=my-repo --var git_url="$(git remote get-url origin)" \
  --var account=<account> --var default_branch=main
```

`kd formula cook <file> --var ...` previews it without writing anything.

**In Slack:** nothing yet. Agent cards for a project go to the channel bound to
it. To bind one, pour `gardenio/formulas/onboarding/enable-slack.json`, or ask
your operator which channel your project's cards land in.

## 3. Start a local agent

In a terminal you keep open, from the root of your repo:

```sh
gb agent start --local --role local --project my-repo
```

It runs attached; ctrl-C stops it. It registers with the daemon on start,
heartbeats, and closes its agent bead when it exits. The `local` role gives it
the laptop prompts: it acts as you, with your permissions and no more, and files
an escalation for anything beyond them. `gb role list` may not list `local` yet;
`--role local` still gets those prompts.

From a second shell:

```sh
gb agent roster          # your agent, role local, project my-repo
```

**In Slack:** the agent card appears once the agent has a task (step 4).

## 4. Give it a first task

Pick something small whose result is easy to check: a typo, a missing test, a
README section. Create the bead with the project label, so it shows in
`gb ready`:

```sh
kd create "Fix the broken install link in README" --type task -l project:my-repo \
  --short-title "fix README install link" \
  -d 'The install link in README.md points at the right page; nothing else changes.'
```

Hand it over: stop the agent from step 3 (ctrl-C) and start it again,
pre-assigned to the bead:

```sh
gb agent start --local --role local --project my-repo --task <bead-id>
```

(An agent that is already running picks the bead up only the next time it runs
`gb ready`; pre-assigning is the quick way.)

The agent claims the bead, works on a branch, pushes, runs `gh pr create`,
links the PR with `gb mr link`, and runs `gb stop`. Check:

```sh
kd show <bead-id>        # in_progress or closed, with the PR linked
```

**In Slack:** the task gets a thread with the agent card at the top, showing its
state and the bead it claimed. Its `gb squawk` progress lines land in the
thread, a decision shows up as a row of buttons, and the last message is the
wrap-up from `gb stop`, with the PR link. Slack replies and decision answers do
not wake a stopped laptop agent yet; to pick the task up again, run
`gb agent start --local --role local --project my-repo --task <bead-id>`.

The clock stops when the PR is open. Review and merge it on your git host as
usual.

## No daemon yet: the demo land

Without a gardenio daemon of your own, the hosted demo land is the zero-cluster
way to look around. Open `/lands/demo` on your gardenio viewer, sign in with
GitHub, accept the terms, and select **Get a guest pass**. The page then prints
the `BEADS_HTTP_URL` to export. Sign in after the pass is ready:

```sh
export BEADS_HTTP_URL=https://api.clonk.tv/land/<land>
gb login && gb ping && kd list
```

A guest pass lasts 24 hours. A guest can read the land and post messages; it
cannot register a project or start an agent, so it stops before step 2. For a
PR on your own repo, apply for a land of your own at `/lands/apply`, or use a
gardenio someone runs for you. If `/lands/demo` says there is no demo land, it is
not open on that server yet. Runbook:
[`gardenio/docs/lands/demo-land.md`](../gardenio/docs/lands/demo-land.md).

## When it goes wrong

| Symptom | Fix |
|---|---|
| `git clone` asks for a password or says `Repository not found` | You have no read access to the gardenio repo; ask its owner to add you. |
| `onboard.sh` builds `kd`/`gb` from source (slow) | The latest release has no tarballs for your platform. It works, with Go 1.25+; it just takes longer. |
| `gb ping` 401, or `login session expired` | `gb login` again. A session is bound to one daemon: after changing `BEADS_HTTP_URL`, sign in again. |
| `gb project create` 403 `requires gardenio-admin` | Ask your operator to run it (step 2). |
| `gb agent start` fails with `"coop": executable file not found` | Install coop ([before you start](#before-you-start-the-clock)), then `gb agent reap-local` to clear the half-started agent. |
| The agent cannot push or open the PR | `gh auth status`; the agent uses your `gh` sign-in. |
| A bare `claude` knows nothing about beads | Use `gb-claude` (installed by `onboard.sh`) or `gb agent start --local`; see [`onboarding-mac-operator.md`](onboarding-mac-operator.md#run-gb-claude-not-bare-claude-workflow-priming). |

More: `.claude/skills/gardenio/reference.md` has the symptom-to-fix catalog and
the `how do I` table.

## Timed runs

Each run of this guide is recorded here: who ran it (a crew dry run, or a
stranger who had never used gardenio), the date, the platform, the minutes per
step, and where they got stuck.

### Crew dry run, 2026-10-28, Linux (agent pod)

A crew agent followed this guide inside its own Kubernetes pod, as a fresh user
(an empty `HOME`, no `kd`/`gb` on `PATH`, no token in the environment), on a
fresh public repo that is not gardenio (`octocat/Hello-World`). It is **not**
the stranger run: the agent already knows gardenio, and the pod could not do the
steps that need a person's sign-in.

| Step | Minutes | Notes |
|---|---|---|
| 1. clone | 0.2 | `git clone --depth 1`: 13 s, 354 MB. A full clone took 62 s and 1.3 GB, so the guide uses `--depth 1`. |
| 1. `onboard.sh` | 9.7 | No release carries `kd`/`gb` tarballs yet (the newest tag, `2026.301.1`, predates `release-binaries.yml`), so it built both from source with a cold Go module cache, held to 2 CPUs to fit the pod. The doctor pass, rc block, `gb-claude` and hooks took seconds. |
| 1. `gb login` | not run | The pod cannot reach the Keycloak host, so no device-code sign-in. |
| 2. `gb project create --dry-run` | under 0.1 | Passed. The repo's default branch is `master`, and the dry run showed `default_branch: main` until `--default-branch master` was added; the guide now says so. |
| 2. `gb project create` (real) | not run | Needs a `gardenio-admin` sign-in; the pod has only a fleet token. |
| 3. `gb agent start --local` | not run | Stopped at registration (`HTTP 401`, no sign-in). A real start needs a person's sign-in and their `gh` push access. |
| 4. first task to PR | not run | Same reason. |

Total to the end of step 1: about ten minutes, nearly all of it the
from-source build. Stuck points, and what was done about each:

1. **No `kd`/`gb` release tarballs yet.** Building from source takes the whole
   ten-minute budget. This clears once a release is cut after
   `release-binaries.yml`; `onboard.sh` then downloads instead of building.
2. **The gardenio repo is private.** A stranger without read access cannot clone
   it, and `onboard.sh` cannot fetch release assets anonymously from a private
   repo, so it builds from source even once tarballs exist. Listed under
   [Before you start the clock](#before-you-start-the-clock) and in the
   troubleshooting table.
3. **Default branch other than `main`.** Fixed in step 2.
4. **macOS coop builds from source (about 6 min).** Moved before the clock.
### Stranger run

Not run yet. Tracked on bead gd-hmQnD0UN94.
