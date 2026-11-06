# Onboarding — Gardenio Operator (Linux, macOS, WSL)

Set up a machine to **use gardenio**: the `kd` and `gb` CLIs talking to a
gardenio daemon as you. This is the *operator/client* layer only. (The file
name is historical; nothing here is Mac-only.)

**Not covered** (separate concerns): `kubectl`/`helm`/`aws` (cluster admin and
platform dev), running a daemon, and `coop`/`coopmux`, which you need only to
run *local agents*; on macOS you build coop from source, see
[Prebuilt binaries](#prebuilt-binaries).

## TL;DR

```sh
git clone https://github.com/mattbakerconsulting/gardenio.git ~/gardenio
~/gardenio/scripts/onboard.sh
# open a new shell, then:
kd list --limit 2   # → live beads
gb ready
```

`scripts/onboard.sh` runs a doctor pass first and changes nothing until it has
shown you its plan. Then it asks before each change:

1. install `kd` + `gb` into `~/.local/bin`: the release binaries when the
   latest release carries them, otherwise `go build` from your checkout with
   the calver ldflags (`scripts/gardenio-version.sh --build`), so the
   version-skew warning works;
2. install `gb-claude` and `gardenio-version` next to them;
3. write a marked env block (`PATH`, `BEADS_HTTP_URL`, `BEADS_OPERATED_BY`)
   into your shell rc, which a rerun replaces in place;
4. merge the gb hooks into `~/.claude/settings.local.json`;
5. start `gb login`, which you finish in your browser.

It never writes or prints a token. Run it twice and the second run changes
nothing. Pass `--url` and `--operated-by` to skip the questions, `--yes` to
accept every change, and `--doctor` for the read-only report (this is also what
`scripts/gardenio-doctor.sh` runs). `--help` lists the environment overrides.

| Platform | Notes |
|---|---|
| Linux | rc is `~/.bashrc` for bash, `~/.zshrc` for zsh, `~/.profile` otherwise. On a headless box `gb login` prints the link instead of opening a browser. |
| macOS | bash login shells read `~/.bash_profile`, so that is the rc for bash. `timeout(1)` is optional. |
| WSL | detected from `/proc/version`. Keep the checkout inside WSL (`~/gardenio`), not on `/mnt/c`: builds there are slow and CRLF checkouts break scripts. |
| fish | the plan prints the `set -gx` lines for you to add to `config.fish`. |

## Prebuilt binaries

Every calver release carries `kd` and `gb` for `darwin-arm64` (Apple silicon),
`darwin-amd64` (Intel) and `linux-amd64`, and `coop` for `linux-amd64` only, as
`<bin>-<os>-<arch>.tar.gz` with a `.sha256` beside each, on the
[releases page](https://github.com/mattbakerconsulting/gardenio/releases).
`.github/workflows/release-binaries.yml` cross-compiles kd and gb on Linux
(they are `CGO_ENABLED=0`, see below) and stamps them with the release tag, so
`kd --version` and `gb version` report it and the version-skew check works.

```sh
TAG=2026.301.1                      # the release you want
P=darwin-arm64                      # or darwin-amd64, linux-amd64
mkdir -p ~/.local/bin && cd "$(mktemp -d)"
for b in kd gb; do
  gh release download "$TAG" -R mattbakerconsulting/gardenio -p "$b-$P.tar.gz*"
  shasum -a 256 -c "$b-$P.tar.gz.sha256" && tar -C ~/.local/bin -xzf "$b-$P.tar.gz"
done
kd --version && gb version
```

The darwin binaries are ad-hoc signed (arm64) and not notarized. Downloaded
with `gh` or `curl` they run as is; a browser download gets the quarantine
flag, so clear it with `xattr -d com.apple.quarantine ~/.local/bin/{kd,gb}`.
`scripts/onboard.sh` installs these for you when the latest release carries
them; releases cut before the workflow landed have none, and then it builds kd
and gb from source instead.

**coop on macOS: build it from source.** There is no darwin coop tarball:

```sh
cd ~/gardenio/coop && cargo build --release   # about 6 min on an M-series Mac
install -m 0755 target/release/coop target/release/coopmux ~/.local/bin/
```

coop links Apple's Security and CoreFoundation frameworks, which only the
license-restricted macOS SDK provides, so it cannot be cross-linked from Linux
(ka-0000001190 still stands for coop), and hosted macOS runners were ruled out
on cost (decision gd-gNvy7dymbw). Linux users can take `coop-linux-amd64.tar.gz`,
which holds `coop` and `coopmux`.

## What actually makes it work

`kd` and `gb` are **static `CGO_ENABLED=0` Go binaries** with no runtime
dependency on the repo. They need three things: where the daemon is, who you
are, and a credential.

| Variable | Value | Role |
|---|---|---|
| `BEADS_HTTP_URL` | your deployment's daemon URL, e.g. `https://gardenio.example.com/kd` | which daemon |
| `BEADS_OPERATED_BY` | `oidc:you@example.com` (or `slack:U…`) | who your writes are attributed to |
| credential | a `gb login` session, or a bearer in `GARDENIO_TOKEN` | auth |

**Sign in with `gb login`** (or `kd login`; they share one session). It is the
Keycloak device grant: gb prints a link and a code, you open the link in your
own browser and sign in, and the token is cached 0600 in
`~/.local/state/beads/oidc-login.json`, bound to the host of `BEADS_HTTP_URL`
and refreshed automatically. A deployment may instead hand you a bearer token
for `GARDENIO_TOKEN`; put that in your own secret store, not in the gardenio
block. Ask whoever runs your gardenio which URL and which credential to use.

### Manual setup (what the script automates)

1. **Binaries.** Take `kd` and `gb` from the release page, or build them with a
   version stamp: `GARDENIO_REPO=~/gardenio ~/gardenio/scripts/gardenio-version.sh --build`.
   A plain `go build` reports version `dev`, and the skew warning cannot fire.
2. **Shell rc.** Add:
   ```sh
   export PATH="$HOME/.local/bin:$PATH"
   export BEADS_HTTP_URL='https://gardenio.example.com/kd'
   export BEADS_OPERATED_BY='oidc:you@example.com'
   ```
3. **Sign in.** `gb login`.
4. **Verify.** `gb ping` exits 0, `gb prime` names you, and `kd list --limit 2`
   returns beads.

## Claude Code (optional but recommended)

```sh
npm i -g @anthropic-ai/claude-code   # or see https://claude.com/claude-code
claude                               # first run opens browser OAuth login
```

Proven: a headless Claude agent given only its OAuth token plus the env vars
and credential above can drive `kd`/`gb` end-to-end from a clean container — no VPN, no
mounts, no config files.

### Run `gb-claude`, not bare `claude` (workflow priming)

A bare `claude` you start by hand has **no gardenio workflow context** — it
doesn't know the SESSION CLOSE PROTOCOL, the kd/gb command surface, or any
advice beads. That's not a bug in the hooks: `gb hook prime` (the `SessionStart`
hook) is deliberately terse and emits only *live state* (identity, active jacks,
claimed work). In production the heavy 12 KB context rides in via the **system
prompt** — coop launches `claude --append-system-prompt-file=<workdir>/.gb/system-prompt.md`,
where that file is `gb prime --system-prompt` (see `writeSystemPromptFile` in
`gardenio/controller/cmd/gb/agent_system_prompt.go`). A hand-started `claude`
skips coop, so the system-prompt file is never written and the agent flies blind.

`gb-claude` (installed to `~/.local/bin` by the onboard script) closes the gap:
it regenerates `gb prime --system-prompt` each launch and splices it in with
`--append-system-prompt-file`, exactly the way coop does. All `claude` flags pass
through.

```sh
gb-claude                 # interactive, fully primed
gb-claude -p "..."        # headless, fully primed
```

The onboard script also aliases `claude` → `gb-claude` in your shell rc, so
muscle-memory `claude` is primed; run `command claude` for a vanilla session.
For a full claim-a-bead-and-loop agent run, prefer `gb agent start --local`,
which goes through the same system-prompt machinery.

Quick A/B to confirm it's working — primed answers, vanilla says it has none:

```sh
gb-claude   -p 'From your system context only, what is the SESSION CLOSE PROTOCOL?'
command claude -p 'From your system context only, what is the SESSION CLOSE PROTOCOL?'
```

### The gb workflow hooks (`gb prime` / `gb stop-gate`)

The way we *actually* work has Claude Code wired into gardenio via hooks in
`~/.claude/settings.local.json`. The script installs these from
`scripts/claude-hooks.json`; manually it's a merge of that file's `hooks` block.
Every command is `gb hook <subcommand>` (verify with `gb hook --help`):

| Event | Command | Purpose |
|---|---|---|
| `SessionStart` (startup) | `gb hook prime` | inject *live state* (identity, jacks, claimed work) — the workflow rules come from the system prompt via `gb-claude`, see above |
| `SessionStart` | `gb hook load-state` / `check-mail` / `gb workspace audit` | restore state, unread mail, worktree warnings |
| `Stop` | `gb hook stop-gate` | **decision/stop-gate enforcement** (the only non-`|| true` hook) |
| `UserPromptSubmit` | `gb hook check-mail` | surface new mail mid-session |
| `PreCompact` | `gb hook prime --enqueue` / `load-state` | survive context compaction |
| `PreToolUse`/`PostToolUse`/`Subagent*`/`TaskCompleted`/`TeammateIdle` | `gb hook relay` | publish events to the `HOOK_EVENTS` NATS stream (no live consumer — see note) |

These require `gb` on `PATH` and signed in (manual steps 1–3). All but `stop-gate` are
`|| true` best-effort, so a missing daemon degrades gracefully.

> **`gb hook relay` has no live consumer (verified 2026-09-21).** The events are
> published and durably stored, but the only subscriber ships inside the
> disabled slack-bridge, and the original "doots" viewer (beads3d) was deleted
> in `f1fd0c5f`. Running these hooks is still correct — it fills the stream the
> read side will be built against — but do not expect to see the output
> anywhere yet. See `gardenio/plans/claude-session-subagent-tracking.md`. The container
agent image uses an equivalent set generated from `gb hook`; the local
`scripts/claude-hooks.json` omits the container-only `/hooks/drain-queue.sh`
line (it would only no-op on a laptop).

**Verify the hooks function** (not just that the binaries run):

```sh
./scripts/test-gb-hooks.sh
```

It asserts `gb hook prime` emits a `<system-reminder>` preamble, that the
`stop-gate` **allows** (exit 0) with nothing pending and **blocks** (exit 2 +
`{"decision":"block"}`) on an unsatisfied decision gate, and that `check-mail`/
`load-state` run clean. All five checks were verified green against the live
daemon, both on the host and inside the clean-room container.

## Gotchas

- **origin is `https://github.com/mattbakerconsulting/gardenio`**, the sole
  source of record.
- **A session is bound to one host.** `gb login --status` names the daemon it
  was made for; after changing `BEADS_HTTP_URL`, sign in again.
- **The old `onboard-mac.sh` block** (`# >>> gardenio operator env >>>`) exported
  a GitLab PAT and a retired host. `onboard.sh` replaces it with the new block;
  revoke that PAT if nothing else uses it.
