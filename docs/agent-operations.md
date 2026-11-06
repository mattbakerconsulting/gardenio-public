# Agent Operations Guide

Operational reference for gardenio agents — config systems, workspace management, CI/CD, and infrastructure.

## Config Beads

Config beads provide layered, label-scoped configuration for agents.

### Categories

| Category | Strategy | What it configures |
|---|---|---|
| `claude-settings` | MergeOverride | Model, permissions, plugins, thinking mode |
| `claude-hooks` | MergeConcat | SessionStart, PreCompact, Stop hooks |
| `claude-mcp` | MergeOverride | MCP server definitions |
| `claude-instructions` | MergeOverride | Workflow context, lifecycle, stop gate text |
| `type` | MergeOverride | Bead type schemas |
| `view` | MergeOverride | List view definitions |
| `context` | MergeOverride | Session context data |

### Label Scoping (specificity order)

```
global (lowest) < project:<name> < role:<name> < agent:<id> (highest)
```

Agent subscriptions are built from environment: `global`, `role:<BOAT_ROLE>` (one entry
per role), `project:<BOAT_PROJECT>` (omitted if empty), `agent:<BOAT_AGENT>`
(omitted if empty) — `claudeconfig.BuildSubscriptions`
(`gardenio/controller/internal/claudeconfig/settings.go:240-252`). There is no `rig:`
subscription and never was; `rig` was a dead alias for `project`, and its last
fallback reader in `cmd/gb/setup_config.go` was removed when `resolveProject`
became the one project resolver (`cmd/gb/project.go`).

**MergeOverride**: more-specific layers overwrite keys from less-specific layers. Unoverridden keys survive.
**MergeConcat**: hook arrays are concatenated across layers.

### Commands

```bash
gb config dump                    # Export everything
gb config load config.json        # Import (idempotent, deduped by title+labels)
kd list --type=config             # See all config beads
kd show <config-bead-id>          # Inspect a specific config bead
```

### How config is consumed

1. **`gb setup claude`** — Runs at pod startup. Writes `settings.json`, `.mcp.json`, `CLAUDE.md`, `stop-gate-text.md`.
2. **`gb prime`** — Runs on SessionStart/PreCompact. Injects workflow context from `claude-instructions`.

### claude-instructions keys

| Key | Used by | Purpose |
|---|---|---|
| `prime_header` | gb prime | Context recovery header |
| `session_close` | gb prime | Git push checklist |
| `core_rules` | gb prime | Fundamental rules |
| `commands` | gb prime | CLI command reference |
| `workflows` | gb prime | Common workflow patterns |
| `decisions` | gb prime | Human decision protocol |
| `session_resumption` | gb prime | How to resume after interruption |
| `lifecycle` | gb prime | Agent lifecycle |
| `stop_gate` | gb prime | Stop gate contract |
| `identity` | gb prime | Agent identity description |
| `stop_gate_blocked` | setup (file) | Text injected by stop-gate.sh hook |
| `claude_md` | setup (file) | CLAUDE.md content |

## kbeads Config Layers

kbeads has three config layers for type schemas:

1. **Builtins** (`kbeads/internal/server/config.go`): hardcoded Go defaults. Fallback for `/v1/configs/` API. `resolveTypeConfig()` checks Postgres first, falls back to builtins, merges missing fields.

2. **Config store** (`/v1/configs/` HTTP API, Postgres-backed): runtime-patchable overrides via `PUT /v1/configs/type:<type>`. Takes precedence over builtins but builtins are merged for missing fields.

3. **Bridge config beads** (`bridge/init.go` `configs()`, which loads `bridge/init_configs.yaml`, + `init_ensure.go` `EnsureConfigs()`): creates `type=config` beads via CreateBead API at startup. Consumed by `gb config dump/load`. NOT read by `resolveTypeConfig`.

**When adding a new field**: update builtins in `config.go` AND the type's entry in `bridge/init_configs.yaml`.

## Per-Project Secrets and Multi-Repo

### Secrets

Declared as JSON array on the project bead's `secrets` field:

```bash
kd update <project-id> -f 'secrets=[{"env":"GITHUB_TOKEN","secret":"my-secret","key":"token"}]'
```

- Entries override globals when `env` matches an existing global env var
- New env names are additive
- Git-related overrides (`GIT_TOKEN`, `GIT_USERNAME`, `GITLAB_TOKEN`) also update init container credentials
- Changes take effect on next pod creation (~60s cache refresh). Existing pods unaffected.

### Multi-Repo

```bash
kd update <project-id> -f 'repos=[{"url":"https://...","branch":"main","role":"primary"},{"url":"https://...","role":"reference","name":"shared-lib"}]'
```

- **primary**: cloned to workspace work directory
- **reference**: cloned to `workspace/repos/{name}` (read-only access)
- Name derived from URL last segment if omitted

## Workspace Management

### Your clone is blobless-partial, not shallow

Every gardenio agent workspace is a **blobless partial clone**
(`git clone --filter=blob:none`), created by the pod init-clone
(`gardenio/controller/internal/podmanager/initclone.go`). This replaced the old
`--depth 1 --single-branch --no-tags` shallow clone (epic ka-0000005334).

What that means in practice:

- **Full commit + tree history is present.** `git log`, `git describe`,
  `git merge-base`, `git rebase`, and `--is-ancestor` all work normally against
  any commit, branch, or tag. `git rev-parse --is-shallow-repository` is `false`.
- **All branches and all tags are present** — the fetch refspec is
  `+refs/heads/*:refs/remotes/origin/*`, so `git branch -r` lists every remote
  branch (thousands on this repo) and `git tag -l` lists every tag. You do **not**
  need to fetch a branch on demand before you can `git log` or diff it.
- **Only historical file *content* (blobs) is deferred.** The clone downloads the
  whole commit + tree graph up front (cheap — ~75–90 MB) but leaves blobs for
  historical commits unfetched. When a command actually needs the *bytes* of a
  historical file, git lazily fetches that blob from the remote (`origin` is
  configured as a promisor: `remote.origin.promisor=true`,
  `remote.origin.partialclonefilter=blob:none`).

> **Do NOT "deepen before an ancestry check."** The old advice — run
> `git fetch --unshallow` / `--deepen` before `merge-base` or a main-vs-tag
> comparison because a shallow clone reports "no common history" — is **obsolete**.
> A blobless clone already has the full commit graph, so ancestry checks are
> accurate out of the box. If `git merge-base --is-ancestor` returns a surprising
> result, it is a real answer, not a clone artifact — verify against
> `git ls-remote` / the GitLab compare API before concluding force-push or
> corruption, but there is nothing to deepen.

#### Blob-fetch caveats (when a command falls back to fetching from the remote)

Because historical blobs are deferred, a handful of commands transparently reach
out to `origin` to backfill the file bytes they need. Each is **fine when the
remote is reachable** (the fetch is automatic and usually sub-second per blob) but
will **stall or fail if `origin` is unreachable**:

| Command | Why it backfills |
|---|---|
| `git blame <path>` | reads the file's content at every historical commit |
| `git log -p` / `git log --follow -p` | prints historical diffs → needs old blobs |
| `git show <old-commit>:<path>` | dereferences a historical blob directly |
| `git checkout <old-commit>` / `git switch --detach <tag>` | materializes the old working tree |
| `git diff <old>..<new>` (or `git diff <tag>`) | needs both sides' blobs |
| `git log -S<string>` / `-G<regex>` (pickaxe), `git grep <old-rev>` | scans content across history — can backfill **many** blobs and run slow/lazy |

**A backfill failure is not corruption.** If one of these fails with a fetch /
`upload-pack` / "unable to read" error, the cause is almost always that `origin`
was unreachable (network blip, throttled egress, expired creds), **not** a broken
repo. Re-run once the remote is reachable, or force a clean re-attempt with:

```bash
git fetch --refetch --filter=blob:none      # re-negotiate promisor fetch; recovers deferred blobs
```

**To avoid on-demand fetches entirely (make the clone fully self-contained /
offline-safe)** — e.g. before a long pickaxe search, a `git blame` sweep, or work
on a flaky network — hydrate every blob once, up front:

```bash
git fetch --refetch --no-filter             # drop the blob:none filter and backfill ALL history
```

This downloads the full object store (large — ~1.5 GB on the monorepo, the cost
the blobless clone exists to defer), after which every history command runs purely
locally. Reserve it for when you genuinely need offline history; the default
lazy-fetch behaviour is cheaper for normal work.

> **Note:** an empty filter-spec (`git fetch --refetch --filter=`) is rejected by
> git ("invalid filter-spec") — use `--no-filter` to fully hydrate, or
> `--filter=blob:none` to just re-attempt the deferred fetch.

**Resumed pods (PVC reuse):** a workspace first cloned by an *older* controller
may still be shallow on disk. The init-clone script detects this
(`git rev-parse --is-shallow-repository`) and upgrades it **in place** to blobless
on the next boot (marks `origin` a promisor, then `git fetch --unshallow
--filter=blob:none --tags`). This is a one-time ~2 s migration; after it, the
notes above apply.

### Isolated worktrees

```bash
gb workspace setup <bead-id>              # create worktree with auto-named branch
gb workspace setup <bead-id> --repo <path> # specify different repo root
gb workspace list                          # see active worktrees
gb workspace teardown <bead-id>            # clean up after PR merged
```

Worktrees are created under `$KD_WORKSPACE/.beads/worktrees/`. Downstream beads auto-use your branch as base.

### Linking MRs to beads

After creating a GitLab MR, immediately run:

```bash
gb mr link <bead-id> <mr-url>
```

This enables review comment notifications, MR state tracking, and agent resolver (crash recovery).

### MR proof packets (required)

Every gardenio MR description must carry a PROOF block — Claim / Surface /
Evidence / Not established — with evidence that exercises the user-visible
claim (screenshot for viewer changes, run link for pipeline changes,
before/after query for daemon changes). Enforced by the `lint-mr-proof` CI
job on every MR pipeline. Template:
`.gitlab/merge_request_templates/Default.md`; norm + reviewer checklist:
[`gardenio/docs/mr-proof-packet.md`](../gardenio/docs/mr-proof-packet.md).

## Work Organization

### Epics and child tasks

```bash
kd create "Refactor auth" --type epic -l project:gardenio
# returns ka-EPIC_ID

kd create "Extract token validation" -l project:gardenio
# returns ka-CHILD_ID
kd dep add ka-CHILD_ID ka-EPIC_ID --type parent-child

# Blocking deps between tasks
kd dep add ka-TASK_B ka-TASK_A --type blocks
```

- `kd dep list <child>` shows parent-child dep
- No reverse lookup from epic to children yet — use `kd list -l project:<name>`
- Close children as you finish, close epic when all are done

### Reading a `type=doc` / bundle body

To read the body of a `type=doc` bead (including the docs hanging under a
`type=bundle`) use the first-class verb:

```bash
gb bundle doc <doc-id>          # prints the rendered fields.content
gb bundle show <bundle-id>      # lists the bundle's docs + their IDs (read-hint on line 5)
```

If you must go raw, read the body field directly — never the description:

```bash
kd show <doc-id> --json | jq -r '.fields.content'
```

**Never `.description // .fields.content`.** A doc bead's `.description` is
**not** the body — it holds a `parent(<bundle>)` typed-mention sentinel that the
references extractor re-materializes into the doc→bundle parent-child edge on
every write (`queries_references.go`, `bundle.go:286-289`). ~88% of doc beads
carry that bare sentinel, so a `.description // .fields.content` jq fallback
short-circuits on it (jq `//` truthiness footgun) and returns the sentinel
instead of the body. The body always lives in `fields.content`; treat
`.description` as a parent-edge field, never as prose.

### Assigning work to agents

| Method | How | When |
|--------|-----|------|
| Pre-assign (new agent) | `kd dep add <agent-bead> <task> --type=assigned` | Entrypoint injects prompt at start |
| Mail (running agent) | `gb mail send <agent-name> -s "subject" -b "body"` | Delivered on next UserPromptSubmit hook |
| Decision resolution | Resolve via `gb yield` response | Agent unblocks and receives instruction |
| Idle polling | Agent calls `gb ready` | Agent discovers task on its own |

**Does NOT work**: `kd update --assignee` or `kd claim` on behalf — sets field but doesn't notify.

### Molecule deduplication

`kd formula apply/pour` rejects duplicates (same formula + same `applied_vars`). Use `--force` to override. Before dispatching crew, check `kd list --type=molecule --status=open,in_progress -l project:gardenio`.

## CI/CD

### GitLab acme-helm-chart deploys

Use API trigger, not manual job play (bot token is Developer/30, needs Maintainer/40):

```bash
glab api -X POST projects/Acme%2FPlatform%2Ffics-helm-chart/pipeline \
  -f ref=main -f 'variables[][key]=GARDENIO_DEPLOY' -f 'variables[][value]=1'
```

When pushing branches to acme-helm-chart, use `agent/` prefix (all other patterns are protected).

## Infrastructure

### Docker-in-agent-pods (EKS)

Use `docker:dind` as a native sidecar (initContainer with `restartPolicy: Always`, K8s >=1.29) with `privileged: true`. Set `DOCKER_HOST=tcp://127.0.0.1:2375` and `DOCKER_TLS_CERTDIR=""`. Use emptyDir for storage. Host socket mount is not viable on EKS (containerd).

### Agent image compilation

gcc and Rust toolchains are installed in agent images. If compilation fails, check:
- `/usr/bin/gcc` and `/usr/bin/cc` (cc -> gcc symlink)
- `/usr/local/cargo/bin/rustc` and `/usr/local/cargo/bin/cargo`
- CRT objects: `ls /usr/lib/x86_64-linux-gnu/crt1.o`
- Linker scripts: `file /usr/lib/x86_64-linux-gnu/libc.so` (should be ASCII text)

### Playwright in containers

Agent containers have no system fonts by default. Before screenshots:

1. Install a font: `curl -sfL 'https://github.com/google/fonts/raw/main/ofl/notosans/NotoSans[wdth,wght].ttf' -o ~/.local/share/fonts/NotoSans.ttf`
2. Create fontconfig: `~/.config/fontconfig/fonts.conf` with `<dir>~/.local/share/fonts</dir>`
3. Set viewport: `mcp__playwright__browser_resize(width=1280, height=720)`

### Playwright MCP as the Slack test user (`playwright-slack`)

When `SLACK_VIEW_STORAGE_STATE` points at an existing Playwright storageState file (the Slack test user's `d`/`d-s` cookies, delivered via ESO from `slack-e2e/test-user-session`), `gb setup` adds a second MCP server, `playwright-slack`: `playwright-mcp --browser chrome --headless --no-sandbox --isolated --storage-state $SLACK_VIEW_STORAGE_STATE`. It lets an agent click through threads, modals and buttons in the real Slack web client (tools appear as `mcp__playwright-slack__browser_*`). The controller mounts the session Secret (`slack-e2e-session`, key `storage-state.json`; chart value `agents.slackViewSession`, default roles `thread`; `secretName: "-"` or `enabled: false` turns it off) as an *optional* volume at `/var/run/gardenio/slack-view`, and the agent entrypoint exports `SLACK_VIEW_STORAGE_STATE` only when that file exists; without it `gb slack view` prints "Slack viewer session not provisioned". The Secret comes from either the ESO block (`externalSecrets.slackE2ESession.remoteRef`) or a hand-created Secret of that name. With the env var unset or the file missing nothing is added, and the default `playwright` server (bundled Chromium, no session) is unchanged. `--isolated` is required by `--storage-state`: each browser session starts from the cookies and discards what it learns. Use a test workspace only; never a real person's session.

### Stale pod reaper rules

Before terminating: check `agent_state` (only reap terminal/unknown, NOT yielding/working), check for open decision beads, verify coop session is dead, require >2h in this state. Err on the side of NOT reaping.

### gardenio-channel

MCP-based nudge replacement. Enable `GARDENIO_CHANNEL=1`. Channel server (port 8789) replaces PTY nudges. Bridge tries channel first, falls back to coop nudge.

### Group DMs (Slack mpim)

gardenio is a full participant in Slack group DMs: it responds when @mentioned, can start new group DMs bound to an agent (`gb slack group open`), and can "invite" people into bot-started ones (`gb slack group invite`). Slack has no in-place mpim invite, so invite re-opens a superset conversation and migrates the binding. Full semantics, the no-in-place-invite constraint, and project/role selection: [`../gardenio/docs/group-dm.md`](../gardenio/docs/group-dm.md).

## Debugging

### kubectl logs

```bash
kubectl get pods -n gardenio | grep slack-bridge
kubectl logs -f -n gardenio <pod-name>
kubectl logs -n gardenio <pod-name> | grep nudge
```

Key pods: `gardenio-slack-bridge-*` (nudge delivery, mentions, decisions), `gardenio-controller-*` (pod lifecycle, bead sync), agent pods (named after agent bead).

## Quality Checks (Landing the Plane)

Before closing any code bead:

**gardenio (controller)**:
```bash
cd gardenio/controller
make build && make build-bridge && make test
quench check
helm lint ../helm/gardenio/
```

**kbeads**:
```bash
cd kbeads
go test ./...
quench check
```

If you modified `.proto` files: `make proto` to regenerate `gen/beads/v1/`.

## Parallel Tool Calls

Batch independent tool calls in a single response to save 3-7s per serial call:

```bash
# Instead of 3 serial calls, use kd-batch:
kd-batch "show ka-ABC" "show ka-DEF" "dep list ka-GHI"
```

SessionStart hooks pre-fetch `gb news`, `gb ready`, and in-progress work — use that data instead of re-querying.

## Miscellaneous

- **Bot avatar**: GitLab: `curl -X PUT -H 'PRIVATE-TOKEN: $GITLAB_TOKEN' -F 'avatar=@image.jpg' https://gitlab.com/api/v4/user/avatar`. Slack: must be set manually in App Settings UI.
- **Jira webhooks**: see `gardenio/controller/internal/bridge/` for registration, HMAC setup, and troubleshooting.
- **Slack AI Apps**: epic created (ka-0000001211) for Agents & AI Apps framework adoption. Needs `assistant:write` scope + new events. Changes are in bridge code only.
- **Feature branches**: prefer branches + PRs over direct push to main, even for urgent fixes.
- **Watch for duplicated work**: check each gardener cycle for multiple agents on same bead/MR, duplicate molecules, stale overlaps.
