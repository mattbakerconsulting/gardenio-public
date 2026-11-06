# Agent Image Build Reference

The agent image is the container that runs Claude Code agents in K8s pods. It has one
build: `images/agent/Dockerfile`, which sources `versions.env` and calls the shared install
scripts. Local builds and CI (`.github/workflows/images.yml`) use that same Dockerfile; CI
publishes `ghcr.io/mattbakerconsulting/gardenio/agent:<tag>`. The second, RWX-based build that
used to have to be kept in sync with it was deleted on 2026-09-16.

## Contents

- [Shared install scripts (single source of truth)](#shared-install-scripts-single-source-of-truth)
- [How the image is built](#how-the-image-is-built)
- [How to add a tool](#how-to-add-a-tool)
- [How to update a tool version](#how-to-update-a-tool-version)
- [How to verify the agent image](#how-to-verify-the-agent-image)
- [Common pitfalls](#common-pitfalls)
- [All images built from monorepo source](#all-images-built-from-monorepo-source)

## Shared install scripts (single source of truth)

Tool versions, package lists, and install logic are defined ONCE in shared scripts under
`images/agent/install/`:

| File | Purpose |
|---|---|
| `install/versions.env` | All version numbers, package lists, and provider versions (sourced by the Dockerfile stages) |
| `install/clis.sh` | Binary CLI tools (kubectl, gh, glab, docker, aws, helm, terraform, promtool, etc.) |
| `install/node.sh` | Node.js + Claude Code |
| `install/codex.sh` | OpenAI Codex CLI — the second agent harness (plan gd-8s8FdZu8UA) — plus **ripgrep**, which Codex needs as a real executable. Static musl binaries; no Node dependency. Has a hermetic self-test (`install/codex_test.sh`) because the image itself is never built on a PR |
| `install/gemini.sh` | Google Gemini CLI — the third agent harness (ADR 0005). A platform-independent JS bundle (one asset, one `GEMINI_SHA256`), so unlike Codex it **needs Node** and must run after `node.sh`; extracted with python3 because `unzip` is not in the `base` stage. Hermetic self-test: `install/gemini_test.sh` |
| `install/go-tools.sh` | Go, gopls, golangci-lint, Task CLI, gofumpt, staticcheck (`go install`ed with the image's Go, so their pins must build with `GO_FULL_VERSION`) |
| `install/go-warm.sh` | Prewarms GOMODCACHE + GOCACHE with every third-party and std package the Go workspace imports, so a fresh pod's first `go test` compiles only this repo's packages (gd-j2ABjjrEL0). See "Warm Go caches" below |
| `install/rust-tools.sh` | Rust, rust-analyzer, quench |
| `install/playwright.sh` | Playwright, Chromium, Chrome, browser system deps |
| `install/terraform-providers.sh` | Pre-caches terraform providers (aws, local, keycloak) into shared plugin cache |

## How the image is built

| Where | When | How |
|---|---|---|
| **Local** | `docker build -f gardenio/images/agent/Dockerfile .` from the repo root | Multi-stage BuildKit build, calls shared scripts |
| **CI** | `images.yml` `build` matrix, arm `agent`: calver tag push or `workflow_dispatch` | Same Dockerfile via `docker/build-push-action`; layer cache in ghcr (`cache/agent`) |

The Dockerfile's builder stages (`go-tools`, `rust-tools`, `cli-tools`, `whisper-builder`,
`playwright-builder`, `terraform-providers`, plus the `gb`/`kd`/`coop` builders) start from
separate images, build concurrently, and are `COPY --from`'d into the final `agent` stage.
Stages **cannot share installed packages** — if stage A installs cmake, stage B cannot use
it.

### Warm Go caches

The `go-warm-*` stages ship `/home/agent/go/pkg/mod` (GOMODCACHE) and
`/home/agent/.cache/go-build` (GOCACHE) holding the workspace's dependencies, compiled
and vetted, so a crew's first scoped `go test` on a fresh pod skips the 15-20 min
compile of client-go & co. (`go test -run CIWait ./cmd/gb/` was 20m7s cold).

- **Dependencies only.** `go-warm.sh imports` lists the non-local packages the
  workspace imports; `go-warm.sh build` compiles them from a tree holding only the
  go.mod/go.sum files. BuildKit keys the expensive layer on that list, so it re-runs only
  when an import or a module file changes, not on every commit.
- **The pin must be what CI runs.** A GOCACHE entry is keyed on the compiler, so the warm
  stage builds with the image's own `/usr/local/go` and `GOTOOLCHAIN=local`.
  `GO_FULL_VERSION` must equal CI's `GO_VERSION` (and be at least go.work's `go` line);
  `scripts/check-go-toolcache-pin.py` reds a PR that lets them drift. Bump
  `GO_FULL_VERSION` and `GO_SHA256_*` together.
- **Same env as a pod.** Paths, `CGO_ENABLED=1` with ubuntu:24.04's gcc, default flags.
  `-race`, `-cover` or build tags miss the cache by design.
- **Far-future mtimes.** Go trims cache entries unused for 5 days by mtime and touches each
  entry it reuses. The image's entries are older than that, so the stage dates them 2100:
  otherwise the first go command on a pod would delete the whole warm cache.

**The agent image is not built on pull requests** (release-only, gd-9mtwbE3hoK), so an
agent Dockerfile or install-script break surfaces only when a release tag builds. To check
a change sooner, dispatch `images.yml` with `images=agent`; that pushes `:latest` and
`:<sha>`, which nothing deploys.

Because of that, an install script whose correctness matters before a tag is cut should
carry a hermetic `*_test.sh` next to it, wired into the `checks.yml` `script-selftests`
job — `install/codex_test.sh` is the worked example: it stubs `curl`, builds a synthetic
release tarball, and exercises both arches in about a second.

## How to add a tool

1. **Add version** to `install/versions.env` (if pinned).
2. **Add install logic** to the appropriate script:
   - apt packages → add to `BASE_PACKAGES` or `AGENT_PACKAGES` in `versions.env`
   - Binary CLI tools → add install function to `install/clis.sh`
   - Go tools → add to `install/go-tools.sh`
   - Rust tools → add to `install/rust-tools.sh`
   - npm packages → add to `install/node.sh` or `install/playwright.sh`
3. **If the tool needs build deps** (e.g. cmake for whisper-cli), install them in the same
   Dockerfile stage — they won't be available from other stages.

## How to update a tool version

1. Update the version in `install/versions.env`.
2. Commit and push; the next release tag (or an `images=agent` dispatch) builds it.

## How to verify the agent image

The build verifies some of this itself: the last step of the `agent` stage runs
`images/agent/smoke.sh` as the agent user, which fails the build unless promtool, gofumpt
and staticcheck print their pinned versions and `$TMPDIR` (`/tmp`) is writable. Add a
tool there when its absence should fail a cut; `smoke_test.sh` covers the script on PRs.
The step after it runs `pg-scratch start` and `pg-scratch stop` once, so a missing Postgres
16 server or pgvector also fails the build.

## Throwaway Postgres (`pg-scratch`)

The image ships a Postgres 16 server and pgvector (`PG_SERVER_PACKAGES`), but no cluster and
nothing running: pods have a 3Gi limit. `pg-scratch` (`images/agent/pg-scratch.sh`) starts a
small one on demand, as the agent user, under `$TMPDIR`, on 127.0.0.1 and a free port, with an
empty `scratch` database that has the vector extension:

```bash
eval "$(pg-scratch start)" && make test-lands-conformance; pg-scratch stop
```

`start` prints `export LANDS_CONFORMANCE_DATABASE_URL=...`; `pg-scratch env` prints it again in a
later shell. `start` refuses while a server is live, so `stop` before the next run that needs an
empty database. `pg-scratch_test.sh` covers it on PRs (its live cases skip without a server).

After a build, check that the other tools are present:

```bash
# Export and inspect (without pulling the full image)
crane export ghcr.io/mattbakerconsulting/gardenio/agent:<tag> - | tar -tf - | grep <binary-name>

# Or run a container
docker run --rm ghcr.io/mattbakerconsulting/gardenio/agent:<tag> which <tool-name>
```

Key tools to verify: `claude`, `coop`, `kd`, `gb`, `playwright`, `npx`, `ffmpeg`, `tmux`,
`whisper-cli`, `go`, `rustc`, `gh`, `glab`, `helm`, `terraform`, `kubectl`, `psql`,
`promtool`, `gofumpt`, `staticcheck`.

## Common pitfalls

**`rg` on an agent's PATH is a bash function, not a binary.** Claude Code's shell profile
defines `rg` as a function wrapping its own vendored ripgrep, so an interactive
`command -v rg` succeeds and makes it look like the image ships ripgrep. It does not:
`env rg` fails with ENOENT. Anything that `execve`s `rg` directly — Codex's search, any
non-bash child — sees nothing. `install/codex.sh` therefore installs a real `rg` into
`/usr/local/bin`. Before concluding "the image already has X", check with `env X` rather
than from an interactive shell.


- **Cross-stage dependencies**: Each builder stage starts from a clean image. If a stage
  needs cmake, install cmake in that stage — don't rely on it being in `base`.
- **Go template `default` with empty strings**: Helm's `default` function does NOT treat
  empty string as falsy. Use `{{ if .Values.x }}{{ .Values.x }}{{ else }}{{ .Chart.AppVersion }}{{ end }}`
  instead of `{{ .Values.x | default .Chart.AppVersion }}`.

## All images built from monorepo source

All images are built from source in this repo by `images.yml`'s `build` matrix — no
cross-repo dispatch. `.github/images.json` is the list: each entry names the image, its
build context and its Dockerfile. Entries with `"controllerBase": true` reuse the Go
binaries the `controller-base` job compiles once (`images/_controller-base/`). Every image
publishes under `ghcr.io/mattbakerconsulting/gardenio/<image>`: pull requests build the images
they touch without pushing, and a calver tag push publishes `:<calver>` (plus `:latest` and `:<sha>`).
