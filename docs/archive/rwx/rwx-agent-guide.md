# rwx CLI — Agent Usage Guide

Reference for gardenio agents driving CI/CD via the [rwx](https://rwx.com) CLI.
Workflow files live in `.rwx/*.yml` in the gardenio monorepo (gardenio, coop,
kbeads) and in acme-helm-chart.

Cross-linked advice beads:

- [ka-0000000227](../README.md) — rwx CLI: how to dispatch and monitor CI workflows
- [ka-0000003503](../README.md) — rwx CLI gotchas: v3.13 schema, ref-name, docker, sudo

For a deeper architectural reference on native OCI image building and the DAG
layout, see [coop-rwx-native-oci-migration.md](coop-rwx-native-oci-migration.md).

## When to use rwx

Prefer `rwx` over raw GitLab API calls. It handles auth, polling, and JSON
output uniformly across workflows.

| Situation | Use |
|---|---|
| Trigger a pipeline on a branch pushed to GitLab | `rwx dispatch <workflow>` |
| Iterate on a workflow from a local working copy (no push needed) | `rwx run <workflow>` |
| Poll status or fetch result of an existing run | `rwx results <RunID>` |

`dispatch` runs the workflow against the GitLab branch as-is. `run` uploads the
current repo tree (up to 5 MiB) and executes it — useful for fast iteration on
a workflow edit before committing.

## Authentication

All invocations require `RWX_ACCESS_TOKEN`. In-cluster agent pods have this
mounted from the `gardenio-rwx-access-token` secret into the `RWX_ACCESS_TOKEN`
env var — no further setup needed.

If running rwx locally, export the token yourself:

```sh
export RWX_ACCESS_TOKEN=$(kubectl -n gardenio get secret gardenio-rwx-access-token \
  -o jsonpath='{.data.token}' | base64 -d)
```

## Common commands

### Dispatch a workflow

```sh
rwx dispatch <workflow> --param key=value --param ref=<branch>
```

Dispatch prints a progress line on stdout before the JSON result. Parse with
`jq -c 'fromjson?'` or similar — a bare `jq '.RunID'` on the raw output will
fail.

```sh
rwx dispatch docker --param ref=main \
  | jq -c 'fromjson? | select(.RunID)'
```

### Run a workflow from your working copy

```sh
rwx run <workflow>
```

`run` uploads the current repo tree to RWX (5 MiB limit). Your git `origin`
MUST point at GitLab, not GitHub — rwx keys the upload off the origin URL.
See beads `ka-0000002001` for background.

### Poll or fetch a run

```sh
rwx results <RunID>
```

Returns JSON with `ResultStatus`, task results, and URLs.

## Gotchas

Source: `memory/rwx-ci.md` and advice bead `ka-0000003503`. These are the
things that will burn you.

### 1. v3.13+ JSON keys are capitalized

Since rwx CLI v3.13, dispatch and results output JSON with **capitalized**
keys: `RunID`, `RunURL`, `ResultStatus`. Any jq parse or script written for
the pre-v3.13 lowercase schema needs updating.

```sh
# Wrong (pre-v3.13):
jq '.run_id'

# Right (v3.13+):
jq '.RunID'
```

### 2. Dispatch output is not pure JSON

`rwx dispatch` prints a human-readable progress line to stdout BEFORE the JSON
object. Always filter:

```sh
rwx dispatch <workflow> | jq -c 'fromjson?' | jq '.RunID'
```

### 3. `ref-name: cli` is required for CLI-triggered workflows

In `.rwx/*.yml`, workflows triggered via `rwx run` or `rwx dispatch` must
declare `ref-name: cli` under `cli.init`. Without it, CLI-supplied vars are
undefined at task execution time.

```yaml
cli:
  init:
    ref-name: cli
    vars:
      - name: version
```

### 4. `docker preserve-data` does NOT cross tasks

`docker: true` + `preserve-data` keeps the docker daemon state within a single
task, but images built in one task are **not** visible in the next task's
daemon. If you `docker build` in task A and expect to `docker push` in task B,
you will get "image not found."

Build and push in the same task, or migrate to native OCI images
(`$RWX_IMAGE` + `rwx image push`) as in coop's migration plan.

### 5. `/usr/local/bin` needs sudo in rwx base containers

The `rwx/base` containers run as a non-root `ubuntu` user. Installing to
`/usr/local/bin` requires `sudo`:

```sh
# Wrong:
cp ./mybinary /usr/local/bin/

# Right:
sudo cp ./mybinary /usr/local/bin/
```

### 6. `rwx run` origin must be GitLab

`rwx run` refuses to upload if `git config --get remote.origin.url` points at
GitHub. Gardenio repos are mirrored to both — check your origin before
invoking:

```sh
git remote -v | grep origin
# If origin is github.com, switch:
git remote set-url origin https://gitlab.com/Acme/Platform/gardenio/<repo>.git
```

## Quick troubleshooting

| Symptom | Likely cause |
|---|---|
| `jq: error: Cannot index string with "RunID"` | Pre-v3.13 jq path on v3.13+ output. Use capitalized keys. |
| `jq: parse error` on dispatch output | Not filtering the progress line. Pipe through `jq -c 'fromjson?'`. |
| `undefined var` at task runtime for CLI-triggered workflow | Missing `ref-name: cli` in `cli.init`. |
| `image not found` on push task after build task | `docker preserve-data` doesn't cross tasks — build+push in one task. |
| `permission denied` copying to `/usr/local/bin` | Missing `sudo` in rwx base container. |
| `rwx run` refuses to upload | Origin points at GitHub, not GitLab. |
| `401 unauthorized` | `RWX_ACCESS_TOKEN` not exported or secret not mounted. |

## See also

- [coop-rwx-native-oci-migration.md](coop-rwx-native-oci-migration.md) — migration from docker-in-CI to native OCI
- `.rwx/` in each gardenio repo — workflow definitions
- `gb advice show ka-0000000227` — dispatch/run/results reference
- `gb advice show ka-0000003503` — gotcha details
