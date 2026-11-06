# CI runners on the k3s cluster

GitHub Actions runners for `mattbakerconsulting/gardenio` run as ephemeral pods on
the cluster's `ci` spot node pool, which scales 0 → N → 0. Design and cost:
`gardenio/plans/k3s-ci-runners.md`. Terraform half (node pool, IAM, tags):
`gardenio/terraform/modules/k3s`.

The Helm release name IS the `runs-on:` label IS the ARC scale-set name. The
gardenio repo is served by three tiers of that pool — see "Runner tiers" below.

## Install / upgrade (as run on 2026-09-17)

```sh
export KUBECONFIG=~/.kube/gardenio.yaml AWS_PROFILE=gardenio AWS_REGION=us-east-1
cd gardenio/helm/ci

kubectl create ns arc-systems; kubectl create ns arc-runners

# Credential for the scale sets: the `gardenio-ci` GitHub App (app id 5029021).
# Since the 2026-10-13 move the App is owned by and installed on the
# mattbakerconsulting org as installation 171077492 (fill in
# from Org → Settings → GitHub Apps → gardenio-ci → Configure: the number at
# the end of the URL). Before the move it was installation 163683294 on the
# joyousaversion user account; that installation is gone once the App
# transfers. Replaced the shared classic PAT on 2026-09-22 — see
# gardenio/plans/github-app-onboarding.md.
# `create`, never `apply`: apply merges, so a leftover github_token key from an
# older Secret would survive and ARC prefers it over the App keys.
aws ssm get-parameter --name /gardenio/github-app/arc/private-key \
  --with-decryption --query Parameter.Value --output text > /tmp/arc.pem
kubectl -n arc-runners create secret generic gardenio-github \
  --from-literal=github_app_id="$(aws ssm get-parameter --name /gardenio/github-app/arc/app-id --query Parameter.Value --output text)" \
  --from-literal=github_app_installation_id=171077492 \
  --from-file=github_app_private_key=/tmp/arc.pem
shred -u /tmp/arc.pem

# Rotating that Secret later is NOT enough on its own: each scale set
# materialises the credential into its own `<name>-listener-config` Secret in
# arc-systems, and a `helm upgrade` with unchanged values does not regenerate
# them. Delete the listener to force it (the ARS controller recreates it from
# the current githubConfigSecret; in-flight EphemeralRunners are unaffected —
# they talk to GitHub directly, only new dispatch pauses for ~10s):
#   kubectl delete autoscalinglistener -n arc-systems <release>-<hash>-listener

helm repo add autoscaler https://kubernetes.github.io/autoscaler && helm repo update autoscaler
helm upgrade --install cluster-autoscaler autoscaler/cluster-autoscaler \
  -n kube-system -f autoscaler-values.yaml            # chart 9.59.0, app 1.35.0

# ghcr pulls are anonymous, but a stale CREDENTIAL anywhere helm looks gives
#   403 ... denied  at the /token endpoint (even though that endpoint returns
# 200 for an anonymous curl). `helm registry logout ghcr.io` only clears
# ~/.config/helm/registry/config.json — helm ALSO reads ~/.docker/config.json,
# and on 2026-09-22 a stale `auths: {ghcr.io: ...}` entry there kept failing
# the pull after a successful logout. Don't delete that entry (it is the
# user's docker login); point helm at an empty config just for the pull:
helm registry logout ghcr.io
mkdir -p /tmp/emptydocker && echo '{}' > /tmp/emptydocker/config.json
export DOCKER_CONFIG=/tmp/emptydocker   # prefix the helm commands below
# Simpler alternative, used on 2026-09-22 when the above was not to hand:
# give helm a VALID credential instead of no credential. Works because the
# 403 is a bad-token rejection, not an authorization one.
#   GH_CONFIG_DIR=$HOME/.config/gh-ja gh auth token \
#     | helm registry login ghcr.io -u fimbaz --password-stdin
ARC=0.14.2
helm upgrade --install arc \
  oci://ghcr.io/actions/actions-runner-controller-charts/gha-runner-scale-set-controller \
  --version $ARC -n arc-systems -f arc-controller-values.yaml
helm upgrade --install gardenio-k3s \
  oci://ghcr.io/actions/actions-runner-controller-charts/gha-runner-scale-set \
  --version $ARC -n arc-runners -f arc-runners-values.yaml
```

## Second scale set: gascity-hq-k3s (private Gas City mirror)

`mattbakerconsulting/gascity-hq` runs Gas City's own gates (`make check`,
`make check-docs`) through `hq-gates.yml` on branches `hq/**`, with
`runs-on: gascity-hq-k3s`. Same controller, same PAT secret (admin on that
repo), same ci node pool, `maxRunners: 1`. All 27 inherited upstream
workflows on that repo are disabled individually (`gh api -X PUT
repos/mattbakerconsulting/gascity-hq/actions/workflows/<id>/disable`), so a push
to the mirror never spends hosted minutes and nothing touches the public fork.

```sh
helm upgrade --install gascity-hq-k3s \
  oci://ghcr.io/actions/actions-runner-controller-charts/gha-runner-scale-set \
  --version $ARC -n arc-runners -f arc-runners-gascity-hq-values.yaml
gh workflow run hq-gates.yml -R mattbakerconsulting/gascity-hq --ref hq/gates-baseline
```

## Third and fourth scale sets: gascity-packs-hq-k3s, beads-hq-k3s

Two more private mirrors, same pattern (same controller, same PAT secret --
admin on both verified with `GET /repos/joyousaversion/<r>` under that PAT --
same ci node pool, `maxRunners: 1`, dind). Each carries an `hq-gates.yml` on
`hq/**` branches only; the mirrors' inherited upstream workflows were all
disabled by API *before* Actions was enabled, so the first push ran nothing
but hq-gates.

| mirror | upstream | gates run by hq-gates.yml | toolchain |
|---|---|---|---|
| `mattbakerconsulting/gascity-packs-hq` | `gastownhall/gascity-packs` | upstream `ci.yml` "Check": `validate_registry.py --require-git`, the pytest suites, `gc lint` over the maintained packs + live-gc pytest suites, gastown shell tests, `go test` in the root/slack-*/runtime-cloudflare modules; RPP conformance advisory (advisory upstream too) | Go 1.25.9 via setup-go (`slack-full/adapter/go.mod`), Python 3.12 + PyYAML/pytest/jsonschema from apt, `gc` v1.4.1 release binary preinstalled on the pod (= what upstream's `go install ...cmd/gc@latest` resolves to) |
| `mattbakerconsulting/beads-hq` | `gastownhall/beads` | Linux half of upstream `pr.yml` "CI Gate / Required": `make ci-pr-policy`, `make ci-pr-lint` (golangci-lint v2.10.1, `--new-from-merge-base origin/main`), `check-migration-hygiene.sh`, `make ci-pr-core` (`go test -race -short ./...`), storage domain+uow, contract corpus; build-examples advisory | Go 1.26.5/toolchain 1.26.7 via setup-go, build-essential (cgo), Dolt 2.2.0 via `scripts/ci/install-dolt.sh`, `dolthub/dolt-sql-server:2.2.0` pulled into dind |

```sh
helm upgrade --install gascity-packs-hq-k3s \
  oci://ghcr.io/actions/actions-runner-controller-charts/gha-runner-scale-set \
  --version $ARC -n arc-runners -f arc-runners-gascity-packs-hq-values.yaml
helm upgrade --install beads-hq-k3s \
  oci://ghcr.io/actions/actions-runner-controller-charts/gha-runner-scale-set \
  --version $ARC -n arc-runners -f arc-runners-beads-hq-values.yaml
gh workflow run hq-gates.yml -R mattbakerconsulting/gascity-packs-hq --ref hq/gates-baseline
gh workflow run hq-gates.yml -R mattbakerconsulting/beads-hq --ref hq/gates-baseline
```

Validating a change on a mirror: PR branches must stay pristine (they are
what eventually goes upstream), so push a *second* branch
`hq/gates/<name>` = the PR branch + the one commit adding
`.github/workflows/hq-gates.yml` (cherry-pick it from `hq/gates-baseline`).
The push triggers the run; the gates summary step is the verdict. Never
push `hq/**` anywhere but the private mirror.

Known: `beads-hq` also shows a "Dependabot Updates" dynamic workflow that the
API refuses to disable (422); upstream's `.github/dependabot.yml` is on the
mirrored `main`, so Dependabot opened three dependency PRs on the mirror
within minutes of the `--mirror` push, before any of this. Turning
Dependabot version updates off is a Settings > Code security toggle with no
REST endpoint. Upstream `Check migration hygiene` is red on `main` at the
mirrored commit (frozen-migration diff against a base that a push has no
notion of), so it is red on the baseline too; see the run summary.

## Runner tiers for the gardenio repo

Three scale sets share the one `ci` node pool. They differ only in resource
shape; same controller, same PAT secret, same nodeSelector and toleration.

| `runs-on:` | values file | max | request | dind | for |
|---|---|---|---|---|---|
| `gardenio-k3s` | `arc-runners-values.yaml` | 7 | 3 CPU / 12Gi | yes | compilers, linters, image builds — 1 runner per 4-vCPU node |
| `gardenio-k3s-medium` | `arc-runners-medium-values.yaml` | 6 | 750m / 1Gi | no | short CPU-bound checks — ~5 per node |
| `gardenio-k3s-light` | `arc-runners-light-values.yaml` | 10 | 250m / 512Mi | no | hermetic static analysis and the path gates — packs freely |

```sh
helm upgrade --install gardenio-k3s-medium \
  oci://ghcr.io/actions/actions-runner-controller-charts/gha-runner-scale-set \
  --version $ARC -n arc-runners -f arc-runners-medium-values.yaml
```

**Why tiers at all.** Measured 2026-09-21/22 over a 9.9-hour window (734 heavy
jobs that actually took a slot; filter on conclusion `success`/`failure` or the
`skipped` and `cancelled` rows push utilisation to a meaningless 296%):

| heavy pool | |
|---|---|
| median queue wait | 1.1–2.3 h |
| median job duration | 1.9 m |
| utilisation | 57 % |
| minutes at the 6-runner cap | 2.9 % |

Jobs waited two hours for a pool running four of six slots, and every
turnover suspect was ruled out by measurement: pod startup 7–12 s, node boot
50–60 s, ASG activity 4 s. The cause is
**burstiness** — one push fans out to ~14 heavy jobs, so a few near-simultaneous
pushes drop 60–140 jobs into 6 slots at once, which at ~2.8 min mean duration is
exactly the ~65 min of drain observed.

So the lever is peak concurrency, and peak concurrency is capped by **nodes**,
not by `maxRunners`: a heavy runner reserves 3 of a node's 4 vCPUs, and the ci
ASG is pinned at MaxSize 8 with all 8 in service. `ci_max_nodes` is fimba-e6's
terraform lane. **Packing density is the lever that does not need it** — moving
four 23–33 s jobs off whole nodes each removed ~45% of `checks.yml`'s per-push
job count for ~4% of its node-minutes.

**Correction, 2026-09-22: `maxRunners` WAS also binding, and the "2.9% at the
cap" figure above is an artifact of how it was measured.** That number comes
from GitHub's job records, which only show jobs that eventually got a slot. The
listener knows better, because it logs the scaling decision it actually made:

```
kubectl -n arc-systems logs <scale-set>-<hash>-listener \
  | sed -n 's/.*"assigned job"=\([0-9]*\) decision=\([0-9]*\) min=\([0-9]*\) max=\([0-9]*\).*/\1 \4/p'
```

Read that way, over one 4h40m window: heavy was **busy 100% of samples, average
5.67 of 6 assigned, and pinned at its cap 65% of the time** — not 2.9%. At the
same moment GitHub held 56 queued heavy jobs and **two of the eight ci nodes
were carrying nothing but a 10m DaemonSet**. Both caps were binding at once;
looking only at the node ceiling hid the cheaper of the two.

Prefer the listener over `gh` for any "was the pool saturated" question. Job
records are survivorship-biased by construction — the jobs that waited longest
are the ones least likely to appear as completed rows in the window you sample.

The same scrape settles the sibling question that had held this pool at 6.
`arc-runners-values.yaml` argued 8 would "starve gascity-hq/packs/beads-hq",
which was never measured; measured, all three report **peak 0 assigned jobs** —
beads-hq over 30 hours, both gascity sets over 4h40m. `maxRunners` reserves
nothing, so an idle scale set holds no node whatever its cap says, and summing
the six caps against 8 nodes overstates contention.

Raised heavy **6 → 7** on that evidence; the listener went to `assigned=7
decision=7 max=7 current=7` immediately and the 7th runner scheduled onto a node
that had been idle. **7 and not 8** is deliberate: sibling runners are
heavy-shaped, so they need a whole free node rather than a gap, and a gardenio
image build can hold a node ~40 minutes. One node stays free at all times so a
sibling job never waits behind that. Revisit if any sibling's peak stops being 0
— the right response is 7 → 6, not more nodes.

**Measured result**, first two pushes after the medium tier went live
(2026-09-22 00:09Z / 00:11Z):

| job | wait before | wait after | duration before | duration after |
|---|---|---|---|---|
| quench quality gate | 1.9 h | 6 s | 8 s | 9–12 s |
| merge-conflict-marker guard | 2.3 h | 6 s | 27 s | 42–49 s |
| `scripts/build-dictation-corpus` | 2.2 h | 8 s | 32 s | 40 s |
| terraform fmt + validate | 1.9 h | 5 s | 38 s | 63 s |

Durations stretched 1.4–1.9× as the sizing note predicted, which is +9 to +30
**seconds** against two **hours** of queueing removed. Don't "fix" the slower
durations; that stretch is the price being paid deliberately. All four medium
runners were scheduled onto nodes already hosting a heavy runner, so the tier
added zero node demand and the ci node count went 8 → 7 — see the
`DO NOT RAISE THIS TO 1000m` note in the values file for the arithmetic that
makes that co-scheduling work, and why rounding the request up destroys it.

Adding a tier is additive by construction: copy a values file, resize it,
`helm install gardenio-k3s-<tier>` as its own release, repoint `runs-on:` on
only the jobs that belong to it. Nothing else is touched.

**Superseded push runs are NOT cancelled, and that is deliberate.** An earlier
version of this section said they were; that is true only for `pull_request`.
On `push` all three workflows key concurrency on `github.sha` with
`cancel-in-progress: false`, so nothing supersedes anything. Measured
2026-09-22 over 24 h: 99 of 136 push runs (73 %) had a newer push to the same
branch created before they finished, and those obsolete runs consumed 938
heavy node-minutes and 284 heavy jobs — 22 % and 18 % of the pool.

Do not "fix" this by sharing a concurrency group per ref. That was tried and
lost data on 2026-09-20: a group holds one running and one *pending* run, and a
new run always evicts the pending one regardless of `cancel-in-progress`, so
every main push after the first silently dropped its predecessor
(`gd-jrNLZaVGhb`, `gd-R1kL4sUECV`). The safe mechanism is a tip-of-main
staleness guard inside the job, which leaves a visible run and a logged reason.
It trades away per-commit CI coverage on main, so it is an operator decision,
filed as `gd-Y0nPTua3Gj`.

What *is* safe without a decision is path filtering, because it only skips work
whose result provably cannot differ. Both workflows now do it, by two different
mechanisms, and the difference is the interesting part.

**`ci.yml` filters at the workflow level** (`paths-ignore` on `push`). That
costs nothing and adds no latency — GitHub never dispatches the run. Measured
over 55 main commits in 24 h, 14 of them (25 %) touched only docs, plans or the
ARC values and were each spending 17.4 heavy node-minutes and 3 heavy slots on
an unchanged build. Use `paths-ignore`, never a positive `paths:` list: it fails
**open**, so a new module can never silently stop being tested. Two entries that
look ignorable and are not: `**.md`, because
`gardenio/controller/cmd/gb/claudemd_default.md` is `go:embed`-ed, and
`gardenio/helm/**`, because kbeads tests read the chart's Grafana dashboards and
`values.yaml`. It is on `push` only — a skipped *workflow* reports no status at
all, which would deadlock a PR if the check were ever required.

**`checks.yml` filters per job**, because it cannot do the above: `quench` runs
a docs-TOC and docs-link pass, and the conflict-marker and internal-strings
guards scan every tracked file, so a whole-workflow skip would disable real
coverage. Per-job filtering needs a gate job (`changes`, light tier, the same
`dorny/paths-filter` pattern `images.yml` uses) and four `if:` gates. Unlike a
skipped workflow, a skipped *job* does report a conclusion, so this form is safe
on `pull_request` too.

Measured over the 253 first-parent commits on main in the 14 days to
2026-09-22, the share that cannot change each gated job's result:

| gated job | tier | cannot change it |
|---|---|---|
| `golangci-lint (gardenio/controller)` | heavy | 80 % |
| `golangci-lint (kbeads)` | heavy | 75 % |
| `helm lint + chart tests` | heavy | 84 % |
| `cargo audit + deny (coop)` | heavy | 87 % |

60 % of main commits need none of the four — about **59 heavy slot-claims a day**
given back, on top of ci.yml's ~42.

A positive path list normally fails *closed*. It is safe here only because each
gated job's scope is **already hardcoded to the same paths** (`cd <matrix.dir>`,
`helm lint gardenio/helm/gardenio/`, `working-directory: coop`), so the filter
cannot be narrower than the job it gates. The part that did not follow from the
scope, and had to be audited by hand, is what each job reads from *outside* its
own directory: `gardenio/clicreds/**` (both Go modules `replace` it),
`deploy-values/**` and `gardenio/helm/values*` (the chart tests and the
duplicate-key guard read them), and `gardenio/locks/security-tools.lock` (the
cargo tool pins). Every gate is written to **fail open** — the job runs whenever the gate
did not succeed, and on `workflow_dispatch`, where paths-filter has no
meaningful base and would report nothing changed.

Not gated, deliberately: `security`, `quench`, `script-selftests` and (until
they were removed) `conflict-markers` and `internal-strings-audit` are
whole-repo or whole-history scans, and a secret or a conflict marker in a
markdown file is exactly what they exist to catch. The conflict-marker guard
moved into `script-selftests` on 2026-09-23; that job is ungated too, so the
whole-tree property is preserved.
`terraform` and `dictation-corpus` sit on the medium tier where queue wait is
5–9 s, so `needs: changes` would roughly double their wall time to reclaim
uncontended slots.

That gate job is why the light pool went from `maxRunners: 4` to `10`. Every
heavy job in `checks.yml` now sits behind it, so a light runner that cannot
start is a heavy job that cannot start. At 4, two concurrent pushes saturated
the pool and the 16-pushes-in-50-minutes bursts this cluster actually sees would
have serialised the whole heavy fan-out behind a queue of 20-second gate jobs.
10 × 250m is 2.5 CPU across the pool, which fit in existing free space with no
new node.

Measured once live: the gate waits **7–14 s** for a light runner and runs for
**19–49 s** (the spread is the `fetch-depth: 0` checkout), so it delays a heavy
job by roughly a minute. Against a 1.1–2.3 h heavy queue wait that is under 1%,
and on the ~60% of main commits that need none of the four gated jobs it
removes them entirely. Light peaked at 2 of 10 assigned, so the raise to 10 is
headroom rather than a constraint — the gate is not the thing to optimise next.

**Expect one cosmetic oddity.** When the lint matrix comes back empty, the
skipped job renders in the checks list under its unexpanded template name,
`golangci-lint (${{ matrix.dir }})`, because there is no matrix value to
interpolate into it. That is a correctly skipped job, not a broken expression —
the in-file comment's claim that the matrix form "preserves the check names"
holds when the job runs and not when it is skipped. Leave it alone unless the
name is ever made a required check, in which case give the job a static `name:`
instead.

**Before moving a job to a smaller tier, verify the job — not its duration.**
The 2026-09-21 classify pass flagged five jobs as light-tier candidates on
duration alone and adversarial re-verification rejected all five, each for a
real cost the sample missed (fresh provider downloads, a whole-repo scan, a
cold-cache `go` build, a 15,000-subprocess fallback, a script that no longer
exists in the tree). Four of them are what the medium tier was then sized
*for*. A short duration is a hypothesis, not a measurement of cost.

### Verdict: `security` stays heavy. govulncheck needs ~7Gi of anon. (2026-09-22)

This is the tier decision the temporary `.github/actions/peak-memory` probe was
built to settle, and it settles it **against** the demotion. Two independent
runs, two different nodes, using the probe's background anon sampler:

| probe | `ip-10-90-31-27` | `ip-10-90-11-147` |
|---|---|---|
| baseline | peak 1056Mi / anonmax 53Mi | 1073Mi / 57Mi |
| after `gitleaks` | 1155Mi / **217Mi** | 1165Mi / **217Mi** |
| after `govulncheck` | 7960Mi / **6990Mi** | 8120Mi / **7153Mi** |
| after `osv` | no change | no change |

`govulncheck` is the whole cost, it is reproducible to within 2 %, and — the
part that decides it — its footprint is **anonymous, not page cache**. Only
~1Gi of the ~8Gi peak is reclaimable. Against medium's 1Gi request it would be
OOM-killed on every run, and because an OOM-killed runner produces the same
"lost communication with the server" annotation as a spot reclaim,
`retry-lost-runners.yml` would retry it three times before it ever went red.
`gitleaks` at 217Mi would be a fine medium tenant on its own; ~~it is not worth
splitting the job for, since the job's slot cost is set by its heaviest step.~~

**Correction, same day: that last clause is wrong, and the split was taken.**
A job's **tier** is set by its heaviest step. Its **slot cost** is tier ×
duration, and the second factor is not fixed by the first. Over 13 runs the
job's steps measured govulncheck 54.2 s, **gitleaks 30.8 s**, osv 1.7 s,
checkout + setup-go + tools 15.9 s. Splitting gitleaks out therefore does not
demote the job — the verdict above stands, `security` stays heavy — but it
does take ~31 s, about 30 % of the job, off the contended tier on every push,
since this job is ungated. Landed as the separate `gitleaks` job in
`.github/workflows/checks.yml`, on `gardenio-k3s-medium`.

Two things make the exchange worth it rather than merely neutral:

- The tiers are not equally scarce. Heavy is pinned at its cap 65 % of the
  time; medium averages 1.53 of 6. Moving 31 s from the first to the second is
  not a wash even though the wall-clock work is identical.
- The medium copy is nearly all scan. It pays a pod start, a 3.6 s checkout and
  a ~3 s tarball download, and it deliberately has **no `setup-go`** —
  `scripts/security/gitleaks-history.sh` shells out to exactly two binaries,
  `git` and `gitleaks`.

A wider confirmation of the split, on 20 runs of `anonmax` rather than the two
above: baseline 78Mi median / 103Mi max, after-gitleaks **240Mi / 263Mi**,
after-govulncheck **7028Mi / 7563Mi**. gitleaks clears medium's 4Gi limit with
15× headroom; govulncheck misses it by 1.7×.

**`osv-scanner` is still unmeasured and was deliberately left where it is.**
"after `osv` — no change" above does **not** mean osv costs nothing. `reset`
reads `unsupported` on these nodes, so `anonmax` is a running maximum that
never resets; equal readings mean only that osv never exceeded govulncheck's
watermark. Its footprint is **masked, not measured**. It is also 1.7 s, so
there is nothing to win by guessing at it.

**Read `anonmax`, never `anon`.** `anon` in `memory.stat` is instantaneous and
the kernel exposes no peak-anon counter, so a probe placed after a step reports
what that step already freed. Here it read **62Mi and 71Mi** after govulncheck
— against a true high-water mark of ~7Gi. Taken at face value it says "easy 4Gi
tenant", which is a worse error than the raw `peak` overestimate the sampler was
added to correct, because it is wrong in the direction that ships a job into an
OOM retry loop. Both readings are on the same output line for exactly this
reason.

`peak` is still worth keeping as a valid one-sided test: `anonmax <= peak`
always, so a job whose peak already clears the target limit is *proven* to fit
and needs no sampler run at all. Only a job that fails the peak test — like this
one — is ambiguous rather than rejected. (Refinement from fimba-a6.)

Do not try to make `reset` work on these nodes; it returns
`reset=unsupported` because the kernel rejects writes to `memory.peak`. It is
also unnecessary: `memory.peak` and `anonmax` are both monotonic, so probes on
either side of a suspect step localise it by subtraction.

## Node-local Go build cache (2026-09-22)

Everything above this section makes the pool *bigger* or gives it *less to do*.
This one makes each job cheaper, which is the lever that had been sitting
untouched the whole time because a comment said it was already pulled.

`ci.yml`'s Go job carried, from the day the pool was built:

> Self-hosted runner: GOCACHE/GOMODCACHE persist on the node.

They did not. "Self-hosted" is not "persistent": these are **ephemeral runner
pods**, and every volume on one was an `emptyDir` (`work`, `dind-sock`,
`dind-externals`). `GOCACHE` defaulted to `$HOME/.cache/go-build` inside the
container overlay and was destroyed with the pod after every job. `setup-go` is
pinned `cache: false` on the strength of the same belief, so there was no
`actions/cache` fallback either. **Every Go build in this repo was a cold
build**, and had been for the pool's entire life.

What that cost, from per-step durations over 20 successful `ci.yml` runs:

| job | `build` | `vet` | `test` | job avg | share of job |
|---|---|---|---|---|---|
| `build + test (controller)` | 190.2 s | 46.5 s | 208.8 s | 7.9 min | 95 % |
| `build + test (kbeads)` | 65.2 s | 25.6 s | 42.8 s | 2.6 min | 84 % |

Those two jobs are **215 of the 384 heavy slot-minutes** in the sample.
Compilation *is* the queue here; the ranking below shows nothing else close.

The fix is a `hostPath` at `/var/lib/gardenio-ci-cache`, mounted at `/cache`,
with `GOCACHE`/`GOMODCACHE` pointed into it — the pod is ephemeral, the **node**
is not. Nodes are spot but live hours to days and run job after job. Disk
measured 88.3 G free of 95.8 G (8 % used), `DiskPressure` False on all eight.

**Why sharing it between concurrent runners on one node is safe:** Go's build
cache and module cache are both designed for concurrent use by multiple `go`
processes and coordinate through their own lock files. That is the ordinary
case Go supports, not a trick.

**Why cargo is not here** *(superseded the same day — see "Rust joined the
cache after all" below; left in place because the reasoning is still correct
and still the reason sccache is the fallback)*. A cargo `target/` dir is
guarded by one whole-directory lock, so sharing it would make concurrent
`coop` jobs *serialise* rather than speed up — strictly worse than the cold
builds it replaces. `coop` is the single most expensive job (8.2 min avg,
168.8 min in the sample) and it needs **sccache** against this same volume,
which caches per-crate and handles concurrency itself. Deliberately left as a
separate change so this one's effect is measurable alone.

Two accepted consequences, both worth knowing before someone "fixes" them:

- `go test` caches **results**, so unchanged packages report `(cached)` rather
  than re-running. That is sound — the key covers the binary, its inputs and
  the env it reads — and it is most of the saving. It does mean a green `test`
  step may not have executed, which is the same bargain `ci.yml`'s
  `paths-ignore` already makes on purpose.
- The cache is writable by job code, so a job could poison it for later jobs on
  that node. **Not a new exposure class**: `containerMode: dind` already gives
  every job a privileged sidecar on the same node. Fix them together or not at
  all.

GC: `GOCACHE` trims itself (entries unused 5 days). `GOMODCACHE` does not, but
it is bounded by the repo's dependency set and spot churn empties it anyway.
Watch it only if ci nodes start living for weeks.

Mechanics: the chart **appends** `template.spec.initContainers` and `volumes`
to its own rather than replacing them — verified by rendering, the pod comes
out with `init-dind-externals`, `dind`, `prepare-build-cache`. Ordering is fine
because `dind` is a native sidecar (`restartPolicy: Always`, k3s 1.36.4), so
Kubernetes starts it and moves on instead of blocking on it. The init container
reuses the runner image (already pulled on every ci node, so no extra pull) and
its `chown` is **not** recursive on purpose — the runner always writes as 1001,
so ownership stays correct below the top level, and a `chown -R` over a warm
multi-GB cache would cost more startup time than the cache saves.

**Where the heavy slot-minutes actually go**, if you want to pick the next one
— total per job name over the same sample:

| heavy job | n | slot-min | avg |
|---|---|---|---|
| `build + test (coop)` | 13 | 107 | 8.2 min |
| `build + test (controller)` | 13 | 102 | 7.9 min |
| `build + test (kbeads)` | 13 | 34 | 2.6 min |
| everything else combined | — | 51 | < 6 min each |

**Two resource numbers that contradict the obvious next guesses**, measured
with `kubectl top pod` while four builds compiled:

- The 3-CPU request is **real**. Runners burst to 3805m / 3896m / 3447m /
  3612m — past the request, into the node's spare capacity. CPU is genuinely
  the binding resource, so cutting the request to pack two runners per node
  would just halve each one's compiler. Don't.
- The 12Gi memory request is **~3× the measured peak** of 3759Mi (4220Mi on a
  later sample), with node memory at 2–22 % used. It survives only because CPU
  binds first (3000m of 3990m allocatable), so lowering it frees nothing
  *today*. It becomes worth revisiting if the pool ever moves to higher-CPU
  instance types, where memory would start to bind.

A second sample a few hours later agreed (3231m / 2469m / 2999m / 3482m /
3020m, peak RSS 4220Mi), with each pod's workload identified by
`kubectl exec … -- ps -eo pcpu,rss,comm,args --sort=-pcpu` — which is worth
knowing as a technique: the ARC runner log does not carry the job name at
useful tail depths, but `ps` names the compiler, the module and the cache
paths it is actually reading, which is how both the Rust and golangci-lint
findings below were confirmed rather than assumed.

### Did it work? Paired cold/warm numbers, and a retraction

An earlier revision of this section claimed the cache was **not** hitting.
That was wrong, and the way it was wrong is the most useful thing here.

**The caches are per node.** A hostPath is node-local by construction, so
"was this job's cache warm?" is a question about *which node ran it*, and it
has a different answer for every node in the pool. Every controller run in
that first sample happened to be the first build of that module on its node,
so cold was being compared against cold. Runner pods therefore carry
`NODE_NAME` (fieldRef `spec.nodeName`) in their env for exactly this reason:
any future timing claim has to name the node and the cache size, or it means
nothing. Read the size with `df -Pm /cache` and `du -sm /cache/go-build`
from inside the pod — never compare two durations without both.

Measured that way, on 2026-09-22, same module, same commit:

| job | node cache | `build` | warm floor (2nd build, same job) |
|---|---|---|---|
| `controller` | 695 MB | 186 s | 63 s |
| `controller` | 3391 MB | 59 s | 53 s |
| `kbeads` | warm | 6 s | 2–3 s |

So the cache is worth **~123 s per controller job** and nearly the whole
build for `kbeads`. A warm controller build at 59 s is already within 6 s of
the floor it can possibly reach — there is **no further caching win in the
build step**. The remaining cost in that job is `test` (100 s warm on
controller, 225 s on `coop`), which is where to look next.

The instrumentation that produced the table was a second `go build` in the
job, and it was **deleted once it had answered** (`d30a2ef3`): it cost 53–63 s
per controller job, on the only contended tier. Re-add it the same way if you
need to re-measure — temporarily, and take it out again.

### Rust joined the cache after all (helm rev 15)

The paragraph above says a shared cargo target dir would serialise concurrent
builds. It would — but **there are no concurrent heavy builds on one node**.
CPU binds at 3000m of 3990m allocatable, so exactly one heavy runner fits per
node, structurally. The lock contention the objection describes cannot occur
while that stays true, which also makes it safe for the init container to
`rm -rf` the target dir outright.

`CARGO_HOME=/cache/cargo`, `RUSTUP_HOME=/cache/rustup`,
`CARGO_TARGET_DIR=/cache/cargo-target`. Verified **live**, not inferred —
`ps` inside a running runner pod shows `rustc` executing from
`/cache/rustup/toolchains/1.93.0-*/bin/rustc`, reading
`/cache/cargo/registry/src/...`, writing `/cache/cargo-target/debug/deps`
with `-C incremental=/cache/cargo-target/debug/incremental`.

Two consequences:

- `rustup toolchain install` becomes a no-op on a warm node, and rustup puts
  its binaries under `CARGO_HOME` — so nothing may hardcode
  `$HOME/.cargo/bin` once `CARGO_HOME` moves.
- cargo GCs **nothing**, and a target dir accumulates every commit's
  artifacts forever. Hence the `df`-based eviction in the init container: if
  `/cache` has under 20 GB free, drop `/cache/cargo-target` whole. It reads
  `df` and not `du` deliberately — this runs on *every* pod creation, and
  `du` over a 20 GB tree takes tens of seconds, which is the same
  startup-latency trap as a recursive `chown`.

sccache is no longer needed *today* (the shared target dir subsumes it), but
it stays the documented fallback for the one case that would bring the
objection back: lowering the heavy CPU request below ~1995m, which would let
two heavy runners share a node.

### Go *tools* are a separate cache, and GOCACHE never covered them (helm rev 16)

`go install golangci-lint@v2.13.2` took **60/66/67/67/68/67 s** over six
consecutive `lint-go` jobs. That flatness *is* the finding: a step reading a
warm cache is not steady to ±1 s. `ps` inside the pod explained it — the
install downloads and runs a **separate Go toolchain**
(`/cache/go-mod/golang.org/toolchain@v0.0.1-go1.26.8`), because
golangci-lint's own `go.mod` outranks CI's pinned 1.25.14. GOCACHE is keyed
on the release patch, so a 1.25.14-keyed cache could never help a 1.26.8
build. It was cold every single time, by construction.

`lint-go` is the largest heavy-tier consumer in `checks.yml` — **1368 s of
slot time over 8 runs**, against 724 s for `cargo-security` and 714 s for
`security`.

Two env vars, plus a guard in the workflow (`fc9395e9`):

- `GOLANGCI_LINT_CACHE=/cache/golangci-lint` — golangci-lint's *analysis*
  cache, which is separate from GOCACHE and defaulted inside the container
  overlay. It was always cold too; lint itself ran 228–292 s on controller,
  98–107 s on kbeads.
- `GOTOOLS_BIN=/cache/go-bin` — **deliberately not spelled `GOBIN`**. Setting
  `GOBIN` pod-wide would silently redirect *every* `go install` in *every*
  job into one shared directory, and a job that then ran the tool off `PATH`
  would get whatever version some earlier job happened to leave there.
  Opt-in means the two callers that use it also carry the version guard that
  makes a shared bindir safe.

The guard, in both `checks.yml` callers (`golangci-lint`, `govulncheck`),
installs only when the binary is absent or the wrong version. One detail in
it is load-bearing and easy to "clean up" into a bug: under
`set -euo pipefail`, `have=$("$BIN/tool" version | sed ...)` exits **127**
when the binary is absent and aborts the step — on precisely the cold-node
first run the guard exists to serve. The `|| true` is the fix, and it was
caught by reproducing rc=127 before the change was pushed. `govulncheck`'s
version is read with `go version -m` rather than `govulncheck -version`,
because the latter contacts the vulnerability DB to print a timestamp.

### Pin the Go *patch*, not the minor (`2e8f6768`)

`GO_VERSION: "1.25"` let `setup-go` drift to the newest patch on its own. The
release patch is part of every action ID in GOCACHE, so an upstream patch
release **invalidates the cache on every node in the pool simultaneously** —
a silent, cluster-wide cold start triggered by someone else's release
schedule. Both `ci.yml` and `checks.yml` now pin `"1.25.14"` (the patch that
was already running, so the change invalidated nothing). Bump them together,
and expect the whole pool to run cold once when you do.

`scripts/check-golangci-pin.sh` reads the CI pin from
`.github/workflows/checks.yml` (#1842) and checks it against
`images/agent/install/versions.env`; bump both together.

### The GitHub *tool cache* was the one cache still dying with the pod (2026-09-23)

Everything above moves a cache off the container overlay and onto the node.
One was missed, and it was the expensive one: `$RUNNER_TOOL_CACHE`, where
`setup-go` puts the toolchain itself.

With `RUNNER_TOOL_CACHE` unset the runner defaults it to `_work/_tool`, and
`_work` is the chart's `work` **emptyDir** — confirmed directly rather than
inferred, from `/proc/1/mountinfo` in a live runner:

```
642 614 259:1 /var/lib/kubelet/pods/…/volumes/kubernetes.io~empty-dir/work /home/runner/_work
635 614 259:1 /var/lib/gardenio-ci-cache                                    /cache
```

`/cache` survives the pod. `_work` does not. So all **243 MB** of
`_tool/go/1.25.14/x64` was thrown away with every pod and re-fetched by every
job, and a job log says so in as many words: `Attempting to download
1.25.14...`, never `Found in cache`.

**What it cost.** Over 99 `Run actions/setup-go@v5` steps, sampled across 76
runs: p50 **18 s**, p90 35 s, max 56 s, **2386 of 27034 pool cpu-seconds =
8.8 %** — the 4th largest consumer on the pool, ahead of checkout and ahead of
every guard, and it produced nothing but an identical Go. Split 46 heavy / 53
medium, so the medium tier is the larger half.

**The fix, and the part of it that is not obvious.** `prepare-build-cache`
seeds `/cache/tool-cache/go/$V/x64` once per **node**, under `flock`, and
verifies a pinned sha256 before it moves the tree into place. The runner is
then pointed at a **pod-local emptyDir** holding a *symlink* to that tree —
not at the shared directory itself.

That indirection is the whole safety argument. `@actions/tool-cache` calls
`rm -rf` on its destination before writing it (`_createToolPath`), which is
fine for a private directory and *not* fine for a shared one: a runner that
cold-installs Go can delete a `GOROOT` that another runner on the same node is
mid-build against. Two heavy runners now share the big nodes (the CPU request
dropped 3000m → 1900m), and up to five medium ones do, so that is a live
arrangement, not a hypothetical. With the symlink, the shared tree is written
**only** by the root init container under the lock; the runner never writes it.
If the seed fails there is simply no symlink, `RUNNER_TOOL_CACHE` is an empty
writable dir, and `setup-go` installs into it exactly as before. Degraded,
never broken.

Exercised before shipping by extracting the block and running it for real:
cold 6 s, warm 0 s, 243 MB (matching the live pod). Four copies racing on an
empty dir → exactly one seeder, no temp leftovers, working toolchain. A bad
sha256, an unresolvable host, and a held lock each exit 0 leaving no marker and
no symlink.

**Deployed and measured the same day** (`helm upgrade` both tiers — heavy
rev 23, medium rev 9; there is no CD on push to main, so merging this changed
nothing until that ran). ARC rolled a new `EphemeralRunnerSet` per tier and
left the busy runners in the old ones alone, which is what makes the
before/after honest: the runner pod name carries the set name, so the two
generations can be separated exactly even though they ran *concurrently*
during the drain.

In-cluster, on live pods of both tiers: `RUNNER_TOOL_CACHE=/toolcache`,
`x64 -> /cache/tool-cache/go/1.25.14/x64`, 243 MB, `go version go1.25.14`.
A medium pod's init log shows the cold path running for real on a live node:
`seeded go 1.25.14 on ip-10-90-23-108`. And a job log closes it:

```
23:01:46.7825741Z Setup go version spec 1.25.14
23:01:46.7872401Z Found in cache @ /toolcache/go/1.25.14/x64
23:01:46.7875244Z Successfully set up Go version 1.25.14
```

Five milliseconds, against `Attempting to download 1.25.14...` before.

**A measurement trap worth writing down.** The first post-deploy pass reported
p50 **6 s** for the *pre-change* population and nearly contradicted the
original 18 s. It was wrong: the jq filter was `test("setup-go")`, which also
matches `Post Run actions/setup-go@v5` — a cleanup step that takes 0–2 s and
occurs exactly as often. 74 real steps totalling 1355 s, plus 74 Post steps
totalling 21 s, gives a median of 2 s and a "median" of 6 s once the sets are
mixed. Anchor on `^Run actions/setup-go`. Clean re-measure of the same window:
n=74, min 6 s, p50 **16 s**, p90 34 s, max 50 s — consistent with the original.
Note `min=6`: nothing in the pre-change population *ever* hit the cache, which
is the root cause stated as a distribution.

Guarded by `scripts/check-go-toolcache-pin.py`, wired into `checks.yml`,
because **both** ways this stops working are silent — the pin drifting away
from `GO_VERSION`, and the `RUNNER_TOOL_CACHE` env being dropped so the seed is
written and then ignored. Neither reds anything; CI just quietly costs 8.8 %
more, which is the same shape as the bug being fixed.

Two things found on the way, neither fixed here:

- The heavy tier's `/cache/cargo-target` eviction justified its `rm -rf` with
  "only one heavy runner fits per node". That has been false since the 1900m
  repack — `ip-10-90-7-78` and `ip-10-90-8-104` each carried two. The comment
  is corrected in place; the hazard is `gd-uUT5o5sqFI`.
- `scripts/check-no-curl-pipe-shell.sh` did not scan `gardenio/helm/` at all,
  so ~80 lines of root-privileged init-container shell were outside the
  repo's installer-pipe guard. Found with a **positive control** — planting the
  literal pattern and watching the guard stay green. (The first attempt at that
  control planted it in a *comment*, which the guard strips: it read green for
  the wrong reason and proved nothing.) Scope widened, 391 → 974 candidate
  files, still 1.66 s, and the planted line now reds.

`e2e.yml` still carries the loose `GO_VERSION: "1.25"` that the section above
argues against. Left alone: on a seeded node `tc.find` matches 1.25.14 from the
local cache before the manifest is consulted, so the seed pins it *in practice*
— but only while the seed succeeded, so this is a reason the patch pin is still
worth doing, not a replacement for it.

#### Correction, hours later: the cold-node first wave was not cheap

The first version chose `flock -n` and had the losers **skip**, on the reasoning
that `-w` "buys it by blocking pod startup on a network fetch, which is the one
thing an init container must not do". That priced the wait at the length of a
*download*. A loser does not wait for a download; it waits for a **seed that
has already started**, and the post-deploy numbers priced both sides:

| | |
|---|---|
| seeding pod's whole init container, cold, on live ci nodes | 2, 2, 2, 3, 4 s (n=5) |
| non-seeding pod's init container | 0 s |
| first 10 post-deploy `Run actions/setup-go@v5` steps | 8 hits at 0–1 s, 2 misses at 7 s and 20 s |

Both misses were a cold node's first wave, and the mechanism is visible in the
init logs rather than assumed: on `ip-10-90-25-233` one pod logged `seeded go
1.25.14` and two siblings logged nothing at all — they lost `flock -n` and went
straight to a per-pod download. Cold waves are not a corner case on a pool that
scales to 13 nodes and churns on spot.

So the loser now **waits** for the winner's marker, bounded at 30 s, and the
wait ends the moment the lock is *free with no marker* — a seeder that dies
costs about a second rather than the bound. `flock -n <file> true` as that
liveness probe is verified in this exact image (util-linux 2.39.3): free → 0,
held → 1, released → 0. Driven against the real extracted block:

- live seeder → waited 4 s, then symlinked
- seeder dies at 3 s → gave up at **2 s**, WARN, falls back to per-pod download
- seeder holds the lock 45 s → capped at **exactly 30 s**, not 45
- cold winner and warm pod unchanged; `shellcheck -s sh` clean

Only an *alive-but-slow* seeder can reach the bound, which is precisely the case
where waiting beats downloading. The seed now also logs its own duration, so the
next person choosing this bound has the number instead of an argument.

`check-go-toolcache-pin.py` additionally asserts the two tiers' seeding blocks
are **byte-identical** once comments and the common indent are stripped (48 code
lines each today). Both files claim to be copies of each other and they write
one hostPath under one lock, so a fix landing in one tier and not the other is a
live hazard — and an invisible one, because the tier that still works keeps the
node seeded for both. Positive control on the real tree: planting one extra line
in the heavy file reds the guard and names it.

## Two tree-scanning guards were forking once per tracked file (2026-09-23)

Re-ranking the pool after the tool-cache landed turned up an unexpected entry.
Over 114 jobs from 60 runs, weighting each step by its tier's cpu request
(heavy 1.920, medium 0.770, light 0.260) against 11,790 pool cpu-seconds:

| step | n | cpu-sec | % pool | max wall |
|---|---|---|---|---|
| `build` (controller) | 18 | 2929 | 24.8% | 344s |
| `test` | 14 | 1372 | 11.6% | 200s |
| `govulncheck` | 6 | 918 | 7.8% | 107s |
| … | | | | |
| **`bash scripts/check-merge-conflict-markers_test.sh`** | **7** | **354** | **3.0%** | **185s** |
| `Run actions/setup-go@v5` | 37 | 155 | 1.3% | 28s |

setup-go at 1.3%/28s max is the tool-cache fix holding (it was 8.8%, p50 18s).
But a guard that reads ~30 MB of text should not be the 9th largest consumer of
the pool, and its per-run spread — 21, 26, 36, 61, 65, 66, **185**s — is the
shape of CPU contention, not of work.

**Root cause, same in two guards.** Both walked `git ls-files` and forked
*inside* the loop:

- `check-merge-conflict-markers.sh` ran a binary probe **and** a match pass per
  file — two processes × 7,654 tracked files ≈ 15,300 execs.
- `check-large-tracked-files.sh` ran `size="$(stat -c %s "$f" …)"` per file — a
  subshell plus a `stat` exec, ≈ 15,300 again.

Almost none of the time was scanning. Measured locally, three runs each:

| guard | before | after | |
|---|---|---|---|
| `check-merge-conflict-markers.sh` | 17.5 / 17.8 / 18.0s | 0.28 / 0.28 / 0.29s | **~62×** |
| `check-large-tracked-files.sh` | 20.2s | 0.48 / 0.50 / 0.50s | **~40×** |

The fix is the same shape in both: filter with `[[ -f ]]`, which is a bash
builtin and costs no process, then hand the whole list to **one** command per
`xargs` batch — `grep -HnIE -e … -e … --` for the markers, `stat
--printf='%s\t%n\0' --` for the sizes.

`check-large-tracked-files.sh` keeps the BSD/macOS path as a per-file loop
behind a **single** dialect probe (`stat --printf='%s' /dev/null`), rather than
re-probing per file as the old fallback chain effectively did. CI is Linux and
never takes it.

### What the local box could not tell us

This laptop has **ugrep 7.8.4 shadowing `grep`** and **uutils-coreutils `stat`**.
Everything load-bearing was therefore re-probed inside the runner image
(`ghcr.io/actions/actions-runner:latest` — GNU grep 3.11, coreutils 9.4,
findutils 4.9.0):

- `stat --printf='%s\t%n\0'` really does emit NUL, so a path containing a
  newline still reads back as one record
- `-H` forces the filename prefix — without it, an `xargs` batch holding a
  single file would report `CONFLICT MARKER: 12:<<<<<<< HEAD` with no path
- `xargs` exits **123** when a batch matches nothing, which is the healthy case,
  so the status is discarded and violations are counted from the output
- a 2,001-file round trip through `xargs -0 stat` parsed 2,001 records across
  several batches and found the one file over 3 MiB

### A test that looked like a control and was not

The new "a binary file carrying a marker is skipped" case passes **with or
without `grep -I`**, on both greps to hand: GNU grep ≥ 3.5 sends
`binary file matches` to *stderr*, and ugrep skips binaries unconditionally.
So it pins the behaviour, not the flag — and the guard now says so in the field
rather than claiming a verification that does not exist. `-I` is kept for
grep < 3.5, where that line went to **stdout** and this loop would have counted
it as a violation against a path that has none.

Two other things the measurement forced:

- the first binary fixture used `head -c 64 /dev/urandom`, which contains no NUL
  about 78% of the time — it was "binary" only on most runs. Now `/dev/zero`.
- `2>/dev/null` came off the grep. `[[ -f ]]` already rules out the directory
  args that produced the old noise, so what remains is an unreadable file — a
  silently unscanned one — and that belongs in the job log.

### Both guards now report what they read

A tree-scanning guard whose file list collapses to zero exits 0 and prints the
same success line as a real clean scan. Both now end with
`(scanned N files)`, and both self-tests assert a floor of 1,000 on it.
Negative controls run before committing: deleting the count reds the new case;
emptying the allowlist reds three cases including the live-tree one. 9 and 7
cases pass, `shellcheck -x` clean, and positive controls on the **live** tree
(a planted marker, a marker in a path with a space, a marker inside a binary,
and a 4 MB blob, all staged) produce exactly the expected verdicts.

### ...and one of the two was never running in CI at all

Chasing the timings surfaced something worse than the timings.
`check-large-tracked-files.sh` is named at `checks.yml:427`, in the
`shellcheck` list — and executed by **no workflow**. It was *linted*, never
*run*. Under RWX it was wired: `docs/archive/rwx/root/ci.yml:3123-3124` runs
the self-test and then the live guard, and the `script-selftests` job's own
provenance comment still lists `lint-large-files` among the RWX tasks it
replaces. The Actions migration carried the comment and dropped the step.

What had no gate in the meantime is ka-0000000431: a 1.55 GB tip tree that
crash-looped every agent pod on the cold-start clone. A guard's presence in a
shellcheck list is not evidence that it runs — the same trap as a docstring.

It now runs, as two lines in the existing `script-selftests` job:

```yaml
bash scripts/check-large-tracked-files_test.sh
bash scripts/check-large-tracked-files.sh
```

Wiring it there rather than as its own job is deliberate: `script-selftests`
already exists, runs on the **light** pool, and carries no `needs: changes` /
`if:` gate (only `lint-go`, `helm` and `cargo-security` are path-gated), so the
scan covers the whole tree on every `checks` run — a blob committed under
`docs/` is caught, not just one under `scripts/**` — while adding no new job,
no new runner and no queue pressure. It is affordable only as of this commit:
the live scan was 19.53s of fork-per-file and is now 0.49s (self-test 1.07s).
At the old cost this step would have added ~40s to a light-pool job.

Auditing the rest of that shellcheck list for the same gap:

| script | executed by | verdict |
|---|---|---|
| `check-large-tracked-files.sh` | nothing → **now `script-selftests`** | migration regression, fixed |
| `check-go-module-coverage.sh` | nothing (RWX: `ci.yml:3163-3164`) | ~~reds on main today, 8 failures — cannot simply be wired~~ **WRONG, see below.** Green on main; wired |
| `check-claude-code-currency.sh` | nothing | *deliberate*: `ci.yml:3172` records the decision not to run the live check in CI |

#### The "reds on main" row was my own worktree

Struck rather than edited, because the *way* it was wrong is the reusable part.
`check-go-module-coverage.sh` did exit 1 with 8 failures when I ran it — naming
real module paths, in the repo's own error prose:

    FAIL: .worktrees/gvisor-k3s/gardenio/controller is a Go module that is
          NEITHER in go.work NOR registered against a make target

Every one of them was under `.worktrees/gvisor-k3s`: a nested **git worktree**,
gitignored, with 0 tracked files. The guard enumerates with `find "$REPO_ROOT"
-name go.mod`, pruning only `vendor/`, `node_modules/` and `.git/` — and a
worktree is a second full checkout, so every module in the repo is found twice,
the second time under a path in no `go.work` and no make target.

The same commit, exported clean (`git archive HEAD | tar -x`), passes:
`all 8 Go module(s) accounted for`, self-test 13/13. **CI checks out a clean
tree and would never have seen this.** So the guard's verdict depended on
whether the person running it happened to have a worktree on disk — and because
the message names real paths and real modules, it reads as a genuine finding.
It cost a wiring decision: recorded as blocked when it was green.

Fixed by pruning git-ignored paths — `git check-ignore`, deliberately **not**
`git ls-files`, because an untracked *new* module is exactly what the guard must
still catch and `ls-files` would hide it until staged. When `REPO_ROOT` is not a
git repo (the synthetic-fixture mode) nothing is pruned. Two cases pin it, and
the second is the one that matters: reverting the pruning reds the ignored-path
case, while an untracked, never-staged module under `tools/` still reds either
way — the control proving the fix did not turn this into a guard that cannot
fail. Self-test 13 → 15 cases.

The transferable rule: **a guard that walks the working directory is measuring
your disk, not the repo.** Before believing a local red, re-run it against
`git archive HEAD | tar -x` — and treat a divergence as a bug in the guard, not
as noise to work around.

### The markers guard was applied only as a side effect of its own self-test

The same audit, run with comments stripped, says `check-merge-conflict-markers.sh`
is **also** invoked directly by no workflow. Unlike the large-files one it was
still being *applied* — case 8 of its self-test `cd`s to the repo root and runs
the live guard over the real tree, which is where the 21–185s came from. RWX ran
both lines (`archive ci.yml:3207-3208`); the migration kept the self-test and
dropped the live invocation, and the coverage survived by accident.

That is a live hazard, not a tidiness point: deleting case 8 as "redundant with
the synthetic cases" would end the whole-tree scan with nothing going red.
Both lines now run explicitly, as they did under RWX.

### ...so the guard no longer needs a job of its own

`conflict-markers` was a dedicated **medium** job whose only step was that
self-test. Measured over 40 `checks` runs (empty `runner_name` excluded):

| | p50 | p90 | max | total |
|---|---|---|---|---|
| queue wait | 9s | 40s | 77s | 638s |
| job wall | 63s | 106s | 206s | 2555s |

At 0.770 cpu that is ~1,970 cpu-seconds for a scan that is now 1s. The first run
after the fix (`53b29201`, run 35934381824) shows what is left: **12s of job —
3s set-up, 6s checkout, 1s of guard.** A whole runner claim per run, almost
entirely overhead.

It is now two lines in `script-selftests`: ungated like the job was, already
checked out, on the 0.260 light tier. `checks` claims **one fewer medium runner
per run**, which is the part that matters — with idle nodes and a cap, demand is
counted in claims, not cpu-seconds. The `script self-tests` step measured 36s on
that run against a prior spread of 36–197s, so both this and the large-files
wiring were absorbed without moving it.

Verified before removing the job: this repo's plan has **no** branch protection
or rulesets at all (`/branches/main/protection` and `/rulesets` both 403
"Upgrade to GitHub Pro"), so no required status check referenced the job name.

## Dead-node cleanup: aws-cloud-controller-manager + node-termination-handler

Added 2026-09-21 after zombie runner slots were measured as the single
biggest cause of runner queue congestion — bigger than `maxRunners`.

The ci pool is 100% spot and churns hard (seven reclaims plus autoscaler
scale-in/out between 11:45 and 23:05 on 2026-09-21). Nothing deleted a Node
object when its instance went away: k3s runs its own stub cloud-controller
whose `InstanceExists` always says yes, and there was no AWS CCM and no
termination handler. Every reclaim therefore left a `Ready=Unknown` ghost
Node forever. Pods on a ghost get a `deletionTimestamp` from the unreachable
eviction but never finish terminating, because the dead kubelet never acks —
and **ARC counts a Terminating runner pod against `maxRunners`**. So the
scale set reports 6/6 busy while only 4 jobs actually run, and the
cluster-autoscaler sees no Pending pods and happily scales a healthy node
*in* under a 24-run backlog.

Measured at 23:20Z before the fix: **14 ci Node objects, 5 live instances**
— 9 ghosts, one runner slot dead for 2h10m, `FailedScheduling` reporting
`0/24 nodes are available`. The CCM's first reconcile deleted 11 ghosts in
under two seconds and Kubernetes pod GC then freed the slots.

```sh
helm repo add aws-ccm https://kubernetes.github.io/cloud-provider-aws && helm repo update aws-ccm
kubectl apply -f aws-ccm-cloud-config.yaml       # ClusterID; see the file's header
helm upgrade --install aws-cloud-controller-manager \
  aws-ccm/aws-cloud-controller-manager --version 0.0.11 -n kube-system -f aws-ccm-values.yaml

helm upgrade --install aws-node-termination-handler \
  oci://public.ecr.aws/aws-ec2/helm/aws-node-termination-handler \
  --version 0.27.6 -n kube-system -f nth-values.yaml
```

- **aws-ccm** (`aws-ccm-values.yaml`): `cloud-node-lifecycle-controller`
  **only** — deletes a Node once EC2 says its `providerID` instance is gone.
  Enabling the `cloud-node` controller instead would require
  `--disable-cloud-controller` on the servers and `cloud-provider=external`
  on every kubelet (terraform, rolling restart); not needed for this.
- **nth** (`nth-values.yaml`): IMDS mode on ci nodes only. Cordons, taints
  and drains on the two-minute spot notice so the slot is returned in
  seconds instead of after the instance is confirmed dead. No SQS, no
  EventBridge, no new IAM.

Two ordering traps, both hit on the way:

- The AWS provider will not start without a ClusterID and discovers one only
  from a `kubernetes.io/cluster/<id>` or `KubernetesCluster` **instance
  tag**, which these instances do not have (they carry
  `k8s.io/cluster-autoscaler/gardenio`, a different key). All three pods
  `CrashLoopBackOff` with `AWS cloud failed to find ClusterID`. Supplied via
  ConfigMap instead of a tag deliberately — a tag dies with a replaced
  server and takes node GC down silently. Read
  `aws-ccm-cloud-config.yaml`'s header before touching this: setting a
  ClusterID also switches on tag filtering for *filter-based* lookups, and
  the reason that is safe here is specific to the one enabled controller.
- Both charts reach AWS through the node instance role via IMDS, exactly
  like the cluster-autoscaler. The CCM runs in the pod network, so closing
  IMDS to pods (`gd-fTCYDqMYeH`) breaks node GC *silently* — errors are
  logged and nodes skipped. NTH is hostNetwork and unaffected.

```sh
kubectl get nodes -l gardenio.io/role=ci      # must equal the live instance count:
aws ec2 describe-instances --filters Name=tag:aws:autoscaling:groupName,Values=gardenio-k3s-ci \
  Name=instance-state-name,Values=running,pending --query 'length(Reservations[].Instances[])'
kubectl logs -n kube-system -l k8s-app=aws-cloud-controller-manager | grep "deleting node"
```

Never count ci nodes without checking `Ready` against the ASG — a plain
`kubectl get nodes` count read 11 when only 6 instances existed.

## Check

```sh
kubectl get autoscalingrunnersets -n arc-runners          # min 0 / max 2, listener registered
kubectl get pods -n arc-systems                           # controller + gardenio-k3s-*-listener
kubectl logs -n kube-system -l app.kubernetes.io/name=aws-cluster-autoscaler | grep gardenio-k3s-ci
aws autoscaling describe-auto-scaling-groups --auto-scaling-group-names gardenio-k3s-ci \
  --query 'AutoScalingGroups[0].[DesiredCapacity,Instances[].InstanceType]'
```

**That `grep gardenio-k3s-ci` is a trap — never read `max size reached` off
it.** cluster-autoscaler filters node groups by max size **before** it
simulates predicates (`orchestrator.go:427` vs `:629`), so whenever the ci ASG
happens to be full, *every* unschedulable pod in the cluster emits

    Skipping node group gardenio-k3s-ci - max size reached

including pods that CA itself would reject on taints a few lines later, and
that could never run on a ci node at all. Measured 2026-09-23 over a 55-minute
window containing three genuine scale-ups: **142 of 142** such lines were
driven by `beads/dolt-0` (permanently unschedulable, see `gd-7TGb2DTSFC`) and
**zero** by a runner pod, while only 4 runner pods went unschedulable in the
whole window. The string has already been miscited once as evidence of CI
node-ceiling pressure. Before believing it, attribute it: walk back to the
`Pod <ns>/<name> is unschedulable` line in the *same* CA iteration and check
the pod is actually a runner. `ci_max_nodes` is still not the lever.

A queued job → listener asks for a runner → pending pod (nodeSelector
`gardenio.io/role=ci`) → cluster-autoscaler sets the ASG to 1 → spot node
joins → job runs → pod gone → node removed after
`scale-down-unneeded-time` (10 min).

The join leg was long carried as "~2–3 min"; measured end-to-end on
2026-09-23 (n=3 scale-ups, cross-checked against ASG scaling activities and
node `creationTimestamp`) it is closer to **85 s**, and it is almost entirely
EC2:

| leg | measured |
|---|---|
| pod unschedulable → CA decides | same second (+ ≤10 s scan interval) |
| CA decides → ASG launch activity | 12 s, 11 s, 16 s |
| ASG launch → node registers | 73 s, 68 s |
| node registers → `Ready` | 0–1 s |

So neither cluster-autoscaler nor kubelet join is a queue term worth tuning —
~80% of a cold node is EC2 boot to k3s registration. Anything aimed at this
leg has to shorten the bootstrap itself (AMI bake, smaller user_data), and
bare EC2 boot is already ~30–40 s of it, so the addressable share is bounded
at roughly half. Measure before investing.

## Gotchas met on the way

- The servers advertised their public IPs to the in-cluster `kubernetes`
  Service, so every pod → apiserver call timed out (SG allows 6443 only from
  the operator). Fixed in the bootstrap template (`--advertise-address`).
- cluster-autoscaler warns `wrong id: ... got k3s://ip-...` for the three
  servers: they predate the `provider-id=aws:///` kubelet arg and are not in
  a discovered ASG, so the warning is harmless. CI nodes register correctly.
- A stale `ghcr.io` entry in `~/.docker/config.json` (an expired `ghs_` token
  from a workflow) also makes the OCI pull 403, and `helm registry logout`
  does not touch it. Run helm with `DOCKER_CONFIG=$(mktemp -d)` (write `{}`
  to `config.json` there) instead of editing the file.
- The gascity-hq runner pod apt-installs `lsof netcat-openbsd python3` on top
  of the toolchain: the stock image lacks them, GitHub's ubuntu-latest has
  them, and `make check` fails (doctor `lsof-binary`, dolt `sql.sh` `nc -z`
  probe under dash) or silently skips tests without them.
- The scale set's `template.spec.containers[0]` must be named `runner` and
  keep `command: ["/home/runner/run.sh"]`; the chart adds the dind sidecar.
- **A past `kubectl patch` on a scale set permanently blocks `helm upgrade` of
  the field it touched.** Changing `maxRunners` on 2026-09-22 failed twice
  with `Apply failed with 1 conflict: conflict with "kubectl-patch" ...
  .spec.maxRunners`. Server-side apply records a per-field owner; a `kubectl
  patch` registers the manager `kubectl-patch` with operation `Update`, and it
  keeps co-owning that field forever, so helm may keep re-applying the same
  value but can never *change* it. `--take-ownership` does **not** clear this.
  Note `kubectl` hides the evidence by default — you need
  `-o json --show-managed-fields` to see the owner at all, and without that
  flag `managedFields` reads as empty and the conflict looks inexplicable.
  Drop the stale entry, asserting the manager name so the index cannot shift
  under you, then re-run the upgrade:

  ```bash
  kubectl -n arc-runners patch autoscalingrunnerset <name> --type=json -p \
    '[{"op":"test","path":"/metadata/managedFields/1/manager","value":"kubectl-patch"},
      {"op":"remove","path":"/metadata/managedFields/1"}]'
  ```

  This rewrites only apply bookkeeping; spec and status are untouched, and no
  runner restarts. The real fix is upstream of it: **change these scale sets
  through their values file and `helm upgrade`, never `kubectl patch`** — a
  patch is invisible in the values file, so the two drift silently until an
  upgrade fails. The live value here was 6 while the release recorded 7.
- **`helm template` and `--dry-run` cannot render these scale sets.** Both are
  client-side, so the chart's `lookup` of in-cluster state returns nothing and
  the render fails. Use `--dry-run=server`. Do not pipe its output into a
  naive YAML parser either — it emits multiple documents plus hook notes and
  crashed one; `grep -n` it instead. Note the container `name:` renders
  *after* its `command:`, so `sed -n '/prepare-build-cache/,/securityContext/p'`
  prints the name without the command it belongs to.
- **`gh` needs `GH_CONFIG_DIR=~/.config/gh-ja` for this repo.** The default
  `~/.config/gh` on this machine is authenticated as `groblegark`, which has
  no access, so every call returns a bare `HTTP 404: Not Found` on
  `mattbakerconsulting/gardenio` — indistinguishable from a wrong repo name.
- **`gh api` cannot fetch job logs; `curl` can.** The
  `actions/jobs/<id>/logs` endpoint 302s to Azure blob storage, which `gh api`
  will not follow. Use
  `curl -sL -H "Authorization: Bearer $(gh auth token)" <url>`. The download
  is UTF-8-with-BOM and CRLF, so `file` reports it as `data` and plain `grep`
  finds nothing in what looks like an empty file — run it through
  `tr -d '\r\0'` and use `grep -a`.
- **GitHub's run-level `status` lies about queueing, in two ways.** A run
  reports `queued` while its jobs are already completing, and a job blocked on
  an unsatisfied `needs:` *also* reports `status: queued` while holding no
  runner slot at all. Measure at job level, or better, ask the ARC listener
  what it has actually assigned:

  ```bash
  kubectl -n arc-systems logs <scale-set>-<hash>-listener --tail=400 \
    | sed -n 's/.*"assigned job"=\([0-9]*\).*max=\([0-9]*\).*/\1\/\2/p' | tail -1
  ```
- **`dorny/paths-filter` on a push diffs `github.event.before..after`** — the
  union over every commit in the push, not just the tip. A markdown-only tip
  commit legitimately triggers Go jobs if an earlier commit in the same push
  touched Go. Check the gate's own log before filing it as a broken filter;
  the one time it looked broken here, the gate was right.
