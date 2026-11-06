# Code audit rubric: what "needed" means, and how to judge a package

> **Status:** standing rubric, written 2026-10-31 against `main` @ `2d252ff4`.
> **Task:** gd-lWMD9js5bT (phase 0 *Ground truth & rubric* gd-agyF0escIN of plan
> gd-eRFLdnAEeB, *audit and shrink gardenio*). Every phase 2 audit step, and every
> deletion PR the plan produces, cites this doc by path and records its verdicts in the
> form set out in [The audit record](#the-audit-record).

The plan cuts code that gardenio does not need. This doc pins down "needed" so that seven
audits run by seven different crews reach the same verdict on the same evidence. It has
four parts: the definition of **shipped**, the **standing rulings** no audit may override,
the **six questions** asked of every package, and the **verdict table** that turns the
answers into keep, trim, fold or delete.

## Inputs

Read these before judging anything. Do not rebuild them on a pod: a whole-module build
OOMs at 3Gi (advice gd-QLhMjEsBsR).

| Input | Where | What it answers |
|---|---|---|
| Deploy inventory | [`gardenio/docs/deploy-inventory.md`](deploy-inventory.md) (gd-YzJ9Jj4gmR) | Which image builds each `cmd/`, which template runs it, whether k3s turns it on; which binaries link each `internal/` package; default-off knobs |
| Reachability artifact | `.github/workflows/reachability.yml` (gd-Nd2ciHTvTE), artifact `reachability` | Per shipped main: `<module>/dead/<main>.tsv` (unreachable), `<module>/test-only/<main>.tsv` (reached only by tests), `<module>/packages-*.txt` (whole packages no main imports), `coverage/<module>.tsv` |
| The deploy | `deploy-values/gardenio-k3s.yaml` | What prod actually turns on. It is the only checked-in deploy target |
| Chart defaults | `gardenio/helm/gardenio/values.yaml` | What a flag is when k3s does not set it |
| Image build | `.github/images.json`, `gardenio/images/*/Dockerfile`, `gardenio/images/_controller-base/Dockerfile` | What CI builds and what goes into each image |
| e2e lanes | `.github/workflows/e2e.yml`, `gardenio/tests/e2e/scripts/run-shard.sh` | What the nightly lane runs (see [R1](#r1-anything-the-e2e-lanes-run-stays)) |

The inventory and the reachability run are snapshots. Both have drifted already: the
inventory lists `landing` as off, but k3s has turned it on since (`landing.enabled: true`,
the clonk.tv front door), and `cmd/splash-export` was added after the inventory counted 50
`cmd/` dirs. **Before a verdict that deletes anything, re-check the row against today's
`main`** (`grep` the values file and `images.json`) and take the latest nightly reachability
run on `main` (`gh run list --workflow reachability.yml --branch main --limit 1`), or dispatch
one (`gh workflow run reachability.yml`) if none ran in the last day. Name the run ID in your
audit record.

## What "shipped" means

A **`cmd/` binary is shipped** only when both are true:

1. **It is built into something prod runs.** An image that CI builds (`.github/images.json`
   entry not marked `"enabled": false`) contains it, or a shipped image runs it at build
   time and ships its output.
2. **The deploy turns it on.** `deploy-values/gardenio-k3s.yaml`, merged over the chart
   defaults, renders a workload that runs that image, or the image is one the controller
   spawns (the agent image, `COOP_IMAGE`), or an operator-applied manifest outside the
   chart runs it.

Status words, and how each maps to the inventory's status column:

| Status | Rule | Inventory status | Today's examples |
|---|---|---|---|
| **SHIPPED** | Both conditions hold | PROD, PROD (out of chart) | `controller`, `slack-bridge`, `grafana-bridge`, `trigger-engine`, `landgate`, `beads-viewer`, `gb` (agent and beads-viewer images), `k3s-audit-shipper` (operator DaemonSet); `kd`; `coop`, `coopmux`, `coop-launch` |
| **SHIPPED (build-time)** | Not in any running container, but a shipped image runs it during its build and ships the output | — (post-inventory) | `splash-export`, which `gardenio/images/landing/Dockerfile` runs to bake the splash |
| **BUILT, NOT DEPLOYED** | Has an image, chart template or `_controller-base` compile, but k3s leaves it off | BUILT-OFF | `jira-bridge`, `gitlab-bridge`, `github-bridge`, `push-bridge`, `edge-auth`, the five log shippers other than `k3s-audit-shipper`; the `coop-claude` image |
| **TEST LANE** | No image; built and run by a lane of `e2e.yml` | E2E-ONLY (hermetic), E2E-ONLY (cluster) | `escalation-reap-probe`, `trigger-probe`, the gvisor probes, `claudemore` (built from source by the replay modules; image and chart removed, gd-Y4ZCGr2m2t) |
| **TOOL** | No image, no deploy, no CI caller; run by hand if at all | TOOL | `coverage-baselines` |
| **UNREFERENCED** | Nothing builds it, runs it or imports it outside its own dir and tests | TOOL / orphan, "none" | `spendlogs-probe`, `edge-idp-demo`, `stub-mcp-server` |

**Built, not deployed is its own verdict, not a kind of shipped.** CI stopped building
those images on 2026-10-15 (gd-OQSDvm7cHz), but `_controller-base` still compiles every
one of them (24 names in its `for bin in` loop today, `gb` built separately), so their code
stays on every controller image build and in every `make test`. They are neither kept
by default nor deleted by default: see [Verdicts](#verdicts).

### Inside a shipped binary

Shipped is a property of a binary. A package or function inside one needs two more tests:

- **Reachable.** A function is reachable when the reachability run does *not* list it in
  `dead/<main>.tsv` for at least one shipped main. A function that appears in the dead list
  of *every* shipped main (the union the SUMMARY reports) is dead. A function in
  `test-only/<main>.tsv` is dead code kept alive by its own tests: it is dead.
  Template-called methods (beads-viewer `html/template`) and `reflect` or `plugin` users are
  invisible to `deadcode`; grep the templates before trusting a dead verdict there.
- **On in prod.** Reachable code behind a switch k3s leaves off is dormant. The switches
  are the chart `enabled:` flags and the env knobs (`internal/config/config.go` reads 219
  env vars; k3s sets 53). The inventory's [Default-off knobs](deploy-inventory.md#default-off-knobs)
  lists them. Code that runs only when a dormant knob is set is judged as if it were
  BUILT, NOT DEPLOYED.

A package is shipped when a shipped binary links it **and** some of its code is reachable
and on. Linked but wholly dead or wholly dormant counts as not shipped.

## Standing rulings

These are owner rulings. An audit cites them; it does not re-open them. A verdict that
conflicts with one is wrong, whatever the reachability data says.

### R1. Anything the e2e lanes run stays

**Source:** decision gd-aSmZUcH9ml, operator 2026-10-16: e2e is back as a nightly job; do
not blanket-delete the e2e layer.

`.github/workflows/e2e.yml` runs nightly (`cron: '17 9 * * *'`), on an `e2e` PR label and by
dispatch. Everything it runs, or is wired to run, stays:

- every module in `run-shard.sh`'s registry. The `shell-hermetic` lane runs
  `HERMETIC_MODULES`, `live-smoke` runs `SMOKE_MODULES`, and the dispatch-only `live-shards`
  lane runs the rest of the registry, so the whole registry counts;
- every `cmd/*-probe` or helper binary a registered module builds or calls;
- the Playwright viewer specs (`viewer` lane), the coop specs (`coop` lane), and the Go
  files under `-tags e2e` and `-tags integration` (`go-e2e`, `go-integration`, `landgate`
  lanes);
- the production code those tests drive. A function reachable only from a TEST LANE probe
  is not dead.

What R1 allows: deleting a probe or script with **zero references outside its own dir**
(the ruling names `spendlogs-probe`, `scrub-coop-token`, `mr-state-backfill`,
`edge-idp-demo`, `stub-mcp-server`), and anything the nightly set explicitly excludes. When
an off-in-prod service is deleted under [Verdicts](#verdicts), its e2e module and probe go
in the same PR, with the registry entry, so the lane never runs a module whose subject is
gone. That is the one way a TEST LANE item leaves, and the audit record must say so.

### R2. beads-viewer keeps every page, route and template

**Source:** owner ruling W-D7 (owner instruction 2026-10-25, thread gd-BGtml5qKgd), in
[`gardenio/docs/superpowers/specs/2026-10-19-lands-workbench-glass-design.md`](superpowers/specs/2026-10-19-lands-workbench-glass-design.md).

beads-viewer stays, whole, as the administrative interface. No template, page or route is
deleted, the spec's DROP list included. "Retire" means a 302 to `/garden` with `?old=1`
keeping the old page, never a removed template. The same spec adds two rulings an audit
must also obey:

- **W-D6:** GET pages may retire to a redirect; **POST endpoints never do.** The
  `/b/{id}/*`, `/plans/{id}/*` and `/gardener/{id}/*` POSTs are the Workbench's write API.
- **G-D6:** coopmux's `mux.html` and the `/mux` edge route stay, operator-tier, as the admin
  dashboard. This binds the coop audit.

What R2 allows in beads-viewer: deleting Go helpers, template methods and handler branches
that no page reaches (grep the templates first), code behind flags k3s leaves off, and
copy-paste between pages, as long as every route still serves the same page.

## The six questions

Ask all six of every package (a Go package, a Rust crate, a chart template directory, a
script directory). Record the answer and the evidence, not just the verdict.

### Q1. What is it for?

One sentence, in present tense, naming the user or system that depends on it. Start from
the package doc comment and the inventory's "what it is" column, then check the claim:
who calls it, and what breaks if it goes. "Nobody can say" is an answer, and it points to
delete.

### Q2. Is it reachable from a shipped main?

Answer from the inventory's `prod users` column and the reachability artifact:

- **yes**: linked by a SHIPPED binary and has functions outside the dead lists;
- **partly**: linked, but some functions are in every shipped main's dead or test-only list.
  Count them;
- **only from off binaries, tools or test lanes**: name which;
- **no**: in `packages-*.txt`, or reached by nothing.

### Q3. Is it on in prod config?

Find the switch that gates it, if any, and read its value in `deploy-values/gardenio-k3s.yaml`
over `values.yaml`, or its env default in `config.go` over the k3s render. Answer **on**,
**off** (name the flag), or **no switch**. A knob set by no deploy and defaulting off is
**dormant**.

### Q4. Is it a legacy or compat shim?

A shim exists to bridge an old shape to a new one: a fallback path for data written before
a migration, a second wire format, an alias for a renamed flag, an env var kept "for old
deploys", support for a forge or CI system the project left. gardenio left GitLab for
GitHub and RWX for GitHub Actions on 2026-09-16; code that serves only those is a shim
until shown otherwise. Signals: `legacy` (372 non-test Go files mention it),
`back-compat`/`backward compat` (68), `Deprecated` (56), `RWX` (65), comments like
"until every pod has rolled", a read path with no matching write path.

Answer **no**, or **yes** plus what it bridges and whether the far side still exists. A
shim whose old side is gone (no deploy, no stored data, no client older than the last
release) is dead. Prove the "no stored data" part with a query or a migration, not a guess;
if you cannot, ask the owner on the step bead.

### Q5. Does it duplicate a helper elsewhere?

Look for a second implementation of the same thing, in this package or another. The common
families today, counting non-test `func` definitions whose names start with each prefix:
`truncate*` 36, `envOr*` 24, `contains*` 21, `writeJSON*` 17, `firstNonEmpty*` 16. Also
look for copy-pasted handlers between pages or bridges, and a client reimplemented per
caller (beads, coop, coopmux, GitHub).

Answer **no**, or **yes** naming the canonical copy the others should call. The canonical
copy is the one with the best tests and the most callers; it moves to a shared package
only when two or more components need it.

### Q6. How much of it is comment?

Measure the share of non-test lines that are `//` comments:

```bash
find <dir> -name '*.go' ! -name '*_test.go' ! -name '*.pb.go' -print0 | xargs -0 cat |
  awk '{t++} /^[[:space:]]*\/\//{c++} END{printf "%d/%d = %.0f%%\n", c, t, 100*c/t}'
```

Baseline on 2026-10-31: `cmd/kd` 17%, `cmd/gb` 22%, `kbeads/internal/store/postgres` 23%,
`internal/bridge` 29%, `kbeads/internal/server` 29%, `cmd/beads-viewer` 32%,
`cmd/controller` 34%, `internal/beadsapi` 34%, `internal/config` 62%.

Over **35%** is a trim candidate; over **50%** almost certainly is one. The ratio only
flags a package; the cut is made comment by comment. Cut comments that narrate history
(which bead, which incident, which plan item, who decided), restate the code, or keep a
dead alternative alive in prose. Keep doc comments on exported names, the *why* behind
non-obvious code, and invariants a reader would otherwise break. One bead ID per decision
is enough: the bead holds the history.

## Verdicts

Every package gets exactly one verdict.

| Verdict | Meaning | The PR does |
|---|---|---|
| **keep** | Needed as it is | Nothing |
| **trim** | Needed, but carries dead functions, dormant branches, dead shims or comment bloat | Deletes those parts; the package stays |
| **fold** | Needed, but duplicates something or is too small to stand alone | Moves callers to the canonical copy or merges the package into its one user, then deletes the duplicate |
| **delete** | Not needed | Removes it and everything that exists only for it (see [Deleting a binary](#deleting-a-binary)) |
| **built, not deployed: ask** | BUILT, NOT DEPLOYED binary, or a package that only such binaries use | Posts the question on the step bead; the owner picks keep (planned use, with a date or bead), fold or delete |

How the answers map to a verdict. Apply the first row that matches:

| # | If | Verdict |
|---|---|---|
| 1 | Protected by R1 or R2 | **keep** (trim allowed only inside what the ruling permits) |
| 2 | Q2 = no, and nothing outside its own dir references it | **delete** |
| 3 | Q2 = only from BUILT, NOT DEPLOYED binaries, or Q3 = off/dormant for the whole package | **built, not deployed: ask** |
| 4 | Q2 = only from TOOL binaries | **delete**, unless Q1 names a human who runs it, then **keep** with that name recorded |
| 5 | Q4 = yes and the old side is gone, for the whole package | **delete** |
| 6 | Q5 = yes, and this is not the canonical copy | **fold** |
| 7 | Q2 = partly, Q3 = off for some branches, Q4 = yes for some paths, or Q6 over 35% | **trim** |
| 8 | Otherwise | **keep** |

A package can hit both 6 and 7; record **fold**, and trim the canonical copy in the same
PR if it needs it.

Tests follow their subject. Tests of a deleted function go with it; a test file kept alive
only by `test-only` symbols is deleted with them. Phase 3 (gd-IPtgGT24AG) owns redundant
tests whose subject stays.

### Deleting a binary

A binary is gone only when every trace is gone. One PR removes:

- the `cmd/<name>/` dir, and every `internal/` package no other binary links;
- `gardenio/images/<name>/`, its `.github/images.json` entry, its name in the
  `_controller-base` `for bin in` loop;
- `gardenio/helm/gardenio/templates/<name>/`, its `values.yaml` block, any
  `deploy-values/` keys, and the chart tests that render it;
- Makefile targets and `scripts/` callers;
- its e2e modules and their `run-shard.sh` registry entries, and its probes (R1 permits this
  only because the subject is gone);
- docs that describe it as live. Archive design history under `docs/archive/` rather than
  editing it.

Then `make test-helm` and `images.yml` must be green on the PR. The chart suite runs in CI's
`checks.yml` `helm` job; on a pod run only the tests you touched.

## The audit record

Each phase 2 step posts one table, as a bead comment or a doc linked from the step bead,
with one row per package:

```markdown
Rubric: gardenio/docs/code-audit-rubric.md. Reachability run: <run URL>. main @ <sha>.

| package | Q1 for | Q2 reachable | Q3 on in prod | Q4 shim | Q5 dup | Q6 comment % | verdict | rule | notes |
|---|---|---|---|---|---|---|---|---|---|
| internal/foo | bridge outbox retry | partly (12 dead) | on | no | yes: truncate → internal/textutil | 41% | trim | 7 | delete 12 dead funcs, cut history comments |
```

`rule` is the row number from the mapping table, or `R1`/`R2`. A row with verdict
**built, not deployed: ask** links the owner question; the PR acts on it only after the
owner answers. A deletion PR names the audit row it implements.

Reduction targets per component (gd-OxJf5mmbEQ) are measured against the same line counts
and are in the dated tables of [`code-reduction-targets.md`](code-reduction-targets.md).
