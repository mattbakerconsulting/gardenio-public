# Code reduction targets: baseline and target per component

> **Status:** baseline measured 2026-10-31 on `main` @ `cfbe53002`; targets fixed the same
> day. **Task:** gd-OxJf5mmbEQ (phase 0 *Ground truth & rubric* gd-agyF0escIN of plan
> gd-eRFLdnAEeB, *audit and shrink gardenio*). The final re-measurement, gd-T9Ao5UDjCO,
> filled in the **actual** columns on 2026-11-03 (`main` @ `03250ef28`) and did not move
> the baseline or the targets. Read [Result](#result-2026-11-03-gd-t9ao5udjco) for what was
> met, what was missed and why.

The plan is held to these numbers. The audit rubric,
[`code-audit-rubric.md`](code-audit-rubric.md), decides *what* goes; this doc says *how
much* has to go, per component, by the end of the plan.

## How it is measured

[`scripts/reachability/code-size.sh`](../../scripts/reachability/code-size.sh) produces
every number here. It reads the source and builds nothing, so it is safe on a pod (about
25 s), and the `size` job of [`reachability.yml`](../../.github/workflows/reachability.yml)
runs it on every dispatch:

```bash
go install github.com/fzipp/gocyclo/cmd/gocyclo@v0.6.0   # once
scripts/reachability/code-size.sh . /tmp/size            # writes components.tsv, long-funcs.tsv, complex-funcs.tsv
```

- **Lines** are physical lines (`wc -l`) of git-tracked files, comments and blanks
  included. Generated Go is left out: `*.pb.go` and any file with a
  `// Code generated ... DO NOT EDIT.` line (13k lines today, mostly
  `lands/contract/landsv1/landsv1.gen.go`). **Test** lines are `*_test.go`; for coop's
  Rust, files named `*_tests.rs` or under a `tests/` dir.
- **Long** is a top-level Go func or method over **100 lines**, `func` line to closing
  brace. The scan matched `go/ast` exactly on all 716 such funcs on this tree.
- **Complex** is a Go func over **30** cyclomatic complexity, as `gocyclo -over 30`
  reports it (closures count toward the func that holds them).
- **src** counts non-test files, **test** counts `_test.go` files. Rust has no function
  counts.

The script was checked against the plan's own figures: run on `main` of 2026-10-12
(`3f007e72b`) it gives 573,909 Go source and 825,685 test lines, the "574k + 825k" the
plan was filed with.

### Components

A file belongs to the first row whose prefix matches. The list is `COMPONENTS` in
`code-size.sh`; keep the two in step.

| component | prefix | what is in it |
|---|---|---|
| `beads-viewer` | `gardenio/controller/cmd/beads-viewer/` | the viewer binary (pages, routes, templates are protected by R2) |
| `gb` | `gardenio/controller/cmd/gb/` | the agent CLI |
| `controller` | `gardenio/controller/cmd/controller/` | the controller main |
| `controller-cmd-other` | `gardenio/controller/cmd/` | the other 46 mains: the small shipped ones (slack-bridge, grafana-bridge, trigger-engine, landgate) and every built-off bridge, probe and tool |
| `bridge` | `gardenio/controller/internal/bridge/` | Slack bridge and trigger engine |
| `controller-internal` | `gardenio/controller/` | every other controller package (`gardenhub`, `podmanager`, `beadsapi`, `commons`, ...) |
| `kbeads` | `kbeads/` | daemon and `kd` |
| `helm-tests` | `gardenio/helm/gardenio/tests/` | chart and dashboard tests (all test lines) |
| `coop` | `coop/` | Rust: coop, coopmux, specs |
| `other` | everything else | `lands/`, `scripts/`, `gardenio/clicreds`, `gardenio/tests/slackmock`, ... |

## Lines: baseline and target

Targets are rounded to the nearest 100 lines. Coop's numbers are Rust; every other row is
Go.

| component | src baseline | src target | cut | src actual | test baseline | test target | cut | test actual |
|---|---:|---:|---:|---:|---:|---:|---:|---:|
| `beads-viewer` | 114,626 | 97,400 | −15% | 113,444 | 166,406 | 133,100 | −20% | 170,024 |
| `gb` | 106,047 | 84,800 | −20% | 105,388 | 123,906 | 92,900 | −25% | 123,850 |
| `controller` | 25,103 | 21,300 | −15% | 26,669 | 36,827 | 29,500 | −20% | 38,799 |
| `controller-cmd-other` | 44,303 | 22,200 | −50% | 29,395 | 34,582 | 17,300 | −50% | 22,711 |
| `bridge` | 129,784 | 97,300 | −25% | 123,246 | 207,010 | 144,900 | −30% | 206,977 |
| `controller-internal` | 155,582 | 124,500 | −20% | 156,497 | 185,589 | 139,200 | −25% | 190,085 |
| `kbeads` | 145,151 | 123,400 | −15% | 153,086 | 210,501 | 168,400 | −20% | 223,319 |
| `helm-tests` | 0 | 0 | | 0 | 57,453 | 43,100 | −25% | 56,347 |
| `other` | 9,215 | 6,500 | −30% | 10,341 | 7,045 | 5,600 | −20% | 8,155 |
| **Go total** | **729,811** | **577,400** | **−21%** | **718,066** | **1,029,319** | **774,000** | **−25%** | **1,040,267** |
| `coop` (Rust) | 46,460 | 41,800 | −10% | 46,990 | 44,363 | 37,700 | −15% | 45,146 |

Actual: `main` @ `03250ef28`, 2026-11-03. Every line target was missed; the
[Result](#result-2026-11-03-gd-t9ao5udjco) section splits each change into plan cut and
feature growth.

The Go source target puts the code back at about its 2026-10-12 size, which is where the
plan started.

## Long and complex functions: baseline and cap

A cap is the most a component may have when the plan closes. Source caps are half the
baseline (rounded down); test caps are three quarters for long funcs, since table-driven
tests run long by design, and half for complex ones.

| component | long src | cap | actual | long test | cap | actual | complex src | cap | actual | complex test | cap | actual |
|---|---:|---:|---:|---:|---:|---:|---:|---:|---:|---:|---:|---:|
| `beads-viewer` | 81 | 40 | 79 | 34 | 25 | 34 | 21 | 10 | 21 | 7 | 3 | 8 |
| `gb` | 47 | 23 | 46 | 38 | 28 | 32 | 16 | 8 | 16 | 9 | 4 | 9 |
| `controller` | 24 | 12 | 23 | 5 | 3 | 5 | 7 | 3 | 7 | 0 | 0 | 0 |
| `controller-cmd-other` | 31 | 15 | 19 | 7 | 5 | 5 | 7 | 3 | 4 | 3 | 1 | 3 |
| `bridge` | 96 | 48 | 87 | 53 | 39 | 52 | 27 | 13 | 28 | 5 | 2 | 5 |
| `controller-internal` | 55 | 27 | 53 | 67 | 50 | 65 | 25 | 12 | 24 | 37 | 18 | 40 |
| `kbeads` | 90 | 45 | 83 | 78 | 58 | 79 | 31 | 15 | 27 | 15 | 7 | 17 |
| `helm-tests` | 0 | 0 | 0 | 6 | 4 | 6 | 0 | 0 | 0 | 3 | 1 | 4 |
| `other` | 2 | 1 | 3 | 2 | 1 | 2 | 1 | 0 | 1 | 2 | 1 | 2 |
| **total** | **426** | **211** | **393** | **290** | **213** | **280** | **135** | **64** | **128** | **81** | **37** | **88** |

Actual: `main` @ `03250ef28`, 2026-11-03. One cap was met by cutting:
`controller-cmd-other` long test, 7 → 5. The other cells that meet their cap had nothing
to cut (`controller` complex test, `helm-tests` src). Every other cap was missed.

Two ceilings hold for every component as well, so the worst offenders cannot survive by
being outnumbered:

- **No source func over 300 lines.** 55 today. The longest are `bridge` `configs`
  (1,468, `internal/bridge/init.go`), `kbeads` `chartCatalog` (1,150), and
  `(*Bot).respawnThreadAgentAt` (1,043). The first two are data tables written as code;
  moving the data to an embedded file is a fair way to meet the ceiling.
- **No source func over complexity 60.** 30 today. The worst are `kbeads`
  `(*BeadsServer).updateBead` (239) and `createBead` (171).

Under R2, `beads-viewer`'s `(*Server).RegisterRoutes` (835 lines) keeps every route; it
meets the ceiling by registering each area's routes from its own func, not by dropping any.

## Reachability: baseline and target

From the reachability run of 2026-10-28,
[40654341447](https://github.com/mattbakerconsulting/gardenio/actions/runs/40654341447)
(`SUMMARY.md` in its artifact). "Union dead" is the set no shipped main reaches; test-only
is the part of it only tests reach.

| measure | baseline | target | actual |
|---|---:|---:|---:|
| controller: union dead funcs (test-only) | 935 (850) | ≤ 100 (≤ 25) | 625 (580) |
| kbeads `cmd/kd`: dead funcs (test-only) | 62 (56) | ≤ 10 (≤ 5) | 8 (6) |
| controller packages no main imports | 4 | 0 | 6 |
| controller packages no shipped main imports | 21 | each has an audit verdict; ≤ 5 left, every one named with its R1 lane or owner answer | 21 |
| kbeads packages no shipped main imports | 1 | 0 | 1 |

Actual: the scheduled reachability run of 2026-11-03,
[41689652526](https://github.com/mattbakerconsulting/gardenio/actions/runs/41689652526),
on `main` @ `4e5f0fda4`, 34 commits before `03250ef28`. Only kbeads' dead count (8 ≤ 10)
was met; its test-only count missed by one.

## Why these cuts

Growth from 2026-10-12 to the baseline, same script:

| component | src 10-12 | src 10-31 | growth | test 10-12 | test 10-31 | growth |
|---|---:|---:|---:|---:|---:|---:|
| `beads-viewer` | 105,207 | 114,626 | +9% | 150,398 | 166,406 | +11% |
| `gb` | 89,043 | 106,047 | +19% | 108,085 | 123,906 | +15% |
| `controller` | 20,756 | 25,103 | +21% | 29,854 | 36,827 | +23% |
| `controller-cmd-other` | 39,258 | 44,303 | +13% | 28,396 | 34,582 | +22% |
| `bridge` | 113,505 | 129,784 | +14% | 187,522 | 207,010 | +10% |
| `controller-internal` | 92,806 | 155,582 | +68% | 118,937 | 185,589 | +56% |
| `kbeads` | 109,837 | 145,151 | +32% | 162,204 | 210,501 | +30% |
| `helm-tests` | 0 | 0 | | 37,716 | 57,453 | +52% |
| `other` | 3,497 | 9,215 | +164% | 2,573 | 7,045 | +174% |
| `coop` (Rust) | 40,813 | 46,460 | +14% | 38,627 | 44,363 | +15% |
| **Go total** | **573,909** | **729,811** | **+27%** | **825,685** | **1,029,319** | **+25%** |

- **`controller-cmd-other`, −50%.** Most of its 46 mains are BUILT, NOT DEPLOYED, TOOL or
  UNREFERENCED under the rubric. The cut assumes the owner deletes about half of the
  built-off bridges and services when asked; if the owner keeps them, the miss is recorded
  against those answers, not against the audit.
- **`bridge`, −25% / −30%.** The biggest test-to-source ratio (1.6), 29% comment, and the
  GitLab and Jira paths the project left or turned off.
- **`gb`, −20% / −25%.** A CLI at 106k lines, grown 19% in under three weeks.
- **`controller-internal`, −20% / −25%.** Has packages only tools or off binaries use:
  the RWX-era `internal/coverage/*` tree, `edgebaseline` (2.1k, reached only from
  `edge-idp-demo`) and `scifportal`. It also has the Lands packages being built now, so
  the cut is kept below the bridge's.
- **`beads-viewer` and `controller`, −15% / −20%.** R2 keeps every viewer page, route and
  template, so only helpers, dead branches and comments can go. Both are 32 to 34% comment.
- **`kbeads`, −15% / −20%.** The daemon is on and reachable almost everywhere (62 dead
  funcs); the cut comes from comments, duplicated helpers and the long handlers.
- **`helm-tests`, −25%.** Grew 52%; phase 3 (gd-IPtgGT24AG) owns redundant tests.
- **`other`, −30%.** `scripts/heraldry-proto` (3,100 lines) is a throwaway prototype;
  `lands/` is new and stays.
- **`coop`, −10% / −15%.** Rust, audited separately (gd-R0jeUZAl3F); G-D6 keeps
  `mux.html` and `/mux`.

## Measuring at the end (gd-T9Ao5UDjCO)

1. Run `code-size.sh` on `main` of that day (or dispatch `reachability.yml` and take
   `size/components.tsv` and `SUMMARY.md` from the artifact). Name the sha and run ID.
2. Fill the **actual** columns above and add actual columns to the function and
   reachability tables. Do not edit a baseline or target.
3. Split each component's change into **plan cut** and **feature growth**, because the
   code grew 27% in the 19 days before this baseline and will keep growing. Plan cut is the
   net lines removed by plan PRs: the merges whose branch ends in the bead ID of a step
   under gd-eRFLdnAEeB (`git diff --numstat <merge>^1 <merge>`, bucketed by the component
   prefixes). Feature growth is everything else since `cfbe53002`.
4. A target is **met** when actual ≤ target. It is **met net of growth** when actual is
   over the target but plan cut ≥ baseline − target: the plan removed what it promised and
   new features added the rest. Report the two apart. Anything else is **missed**; say why
   (an owner kept a built-off binary, a ruling protected the code, the step did not land).

Phase 5 (gd-sfMiLksZNA) turns the function caps into ratchets so the counts cannot climb
back after the plan closes. They are below.

## Result (2026-11-03, gd-T9Ao5UDjCO)

Lines and functions: `code-size.sh` on `main` @ `03250ef28`. Run again on `cfbe53002`, it
gives the baseline above exactly. Reachability: run
[41689652526](https://github.com/mattbakerconsulting/gardenio/actions/runs/41689652526)
on `4e5f0fda4`. Plan PRs: the 82 first-parent merges since `cfbe53002` whose branch ends
in one of the 221 beads under gd-eRFLdnAEeB (parent-child, any depth). #1885, the desk's
revert of plan PR #1791, also counts against the plan; the step was re-landed as #1895.

### Headline

- **Every line target was missed, and none was met net of growth.** Go source went from
  729,811 to 718,066 (−1.6%), against a target of 577,400. Go test went from 1,029,319 to
  1,040,267 (+1.1%), against 774,000.
- **The plan cut less than its targets assumed.** Plan PRs removed 47,702 Go source lines,
  31% of the 152,411 promised, and 36,187 test lines, 14% of the 255,319 promised.
- **Feature growth added 35,957 source lines (+4.9%) and 47,135 test lines (+4.6%)** in
  the three days after the baseline. For source it was the smaller part: with no growth at
  all, Go source would be 682,109, still 104,709 over the target. For tests it was larger
  than the plan cut, which is why test lines went up.
- **Functions**: long source funcs 426 → 393 (cap 211), complex 135 → 128 (cap 64).
  Source funcs over 300 lines went 55 → 43, and over complexity 60 went 30 → 22; both
  ceilings are 0. The two worst, kbeads `updateBead` (239) and `createBead` (171), are now
  under 60.
- **Reachability**: controller union dead 935 → 625 (target ≤ 100). kbeads `cmd/kd`
  62 → 8, which meets its target of ≤ 10.
- **Binaries**: controller `cmd/` mains went 51 → 34, and built but not deployed went
  17 → 7.

### Lines: plan cut and feature growth

Plan cut is the net lines plan PRs changed (negative is removed). Feature growth is the
rest of the change since the baseline. "Done" is plan cut as a share of the promised cut,
baseline − target.

| component | kind | baseline | actual | change | plan cut | feature growth | promised | done | verdict |
|---|---|---:|---:|---:|---:|---:|---:|---:|---|
| `beads-viewer` | src | 114,626 | 113,444 | −1,182 | −4,371 | +3,189 | 17,226 | 25% | missed |
| `beads-viewer` | test | 166,406 | 170,024 | +3,618 | −1,135 | +4,753 | 33,306 | 3% | missed |
| `gb` | src | 106,047 | 105,388 | −659 | −2,649 | +1,990 | 21,247 | 12% | missed |
| `gb` | test | 123,906 | 123,850 | −56 | −2,052 | +1,996 | 31,006 | 7% | missed |
| `controller` | src | 25,103 | 26,669 | +1,566 | −560 | +2,126 | 3,803 | 15% | missed |
| `controller` | test | 36,827 | 38,799 | +1,972 | −626 | +2,598 | 7,327 | 9% | missed |
| `controller-cmd-other` | src | 44,303 | 29,395 | −14,908 | −15,889 | +981 | 22,103 | 72% | missed |
| `controller-cmd-other` | test | 34,582 | 22,711 | −11,871 | −13,451 | +1,580 | 17,282 | 78% | missed |
| `bridge` | src | 129,784 | 123,246 | −6,538 | −8,590 | +2,052 | 32,484 | 26% | missed |
| `bridge` | test | 207,010 | 206,977 | −33 | −3,944 | +3,911 | 62,110 | 6% | missed |
| `controller-internal` | src | 155,582 | 156,497 | +915 | −12,212 | +13,127 | 31,082 | 39% | missed |
| `controller-internal` | test | 185,589 | 190,085 | +4,496 | −9,083 | +13,579 | 46,389 | 20% | missed |
| `kbeads` | src | 145,151 | 153,086 | +7,935 | −3,788 | +11,723 | 21,751 | 17% | missed |
| `kbeads` | test | 210,501 | 223,319 | +12,818 | −1,780 | +14,598 | 42,101 | 4% | missed |
| `helm-tests` | test | 57,453 | 56,347 | −1,106 | −4,412 | +3,306 | 14,353 | 31% | missed |
| `other` | src | 9,215 | 10,341 | +1,126 | +357 | +769 | 2,715 | −13% | missed |
| `other` | test | 7,045 | 8,155 | +1,110 | +296 | +814 | 1,445 | −20% | missed |
| **Go total** | src | 729,811 | 718,066 | −11,745 | −47,702 | +35,957 | 152,411 | 31% | missed |
| **Go total** | test | 1,029,319 | 1,040,267 | +10,948 | −36,187 | +47,135 | 255,319 | 14% | missed |
| `coop` (Rust) | src | 46,460 | 46,990 | +530 | −107 | +637 | 4,660 | 2% | missed |
| `coop` (Rust) | test | 44,363 | 45,146 | +783 | 0 | +783 | 6,663 | 0% | missed |

The biggest plan PRs were #1712 (dead services, −39.2k Go), #2085 (the coverage stack,
−6.6k), #1701 (unused kbeads endpoints, −3.8k) and #1937 (wire or delete, −3.2k). Some
plan PRs added lines on purpose. The phase 4 refactors of the long kbeads handlers and
the controller reconciler (#1767, #1768, #1771, #1772, #1774) added +2.3k, and the helm
test split (#1722) added +1.7k.

### Why each line target was missed

- **`controller-cmd-other`** (72% / 78% of the cut). The owner chose *dead-only* on
  decision gd-rZJdGewUse: delete the six untouched services and fold `claudemore`, but keep
  `jira-bridge`, `gitlab-bridge`, `push-bridge` and `edge-auth`. `github-bridge` was kept
  too, and k3s turned it on in #2083. Those five mains hold 9,558 source and 12,794 test
  lines today. Deleting them would have met both targets. Per the targets above, this
  miss is recorded against the owner's answer, not against the audit. The 14 probes
  (11,307 source lines) stay as test-lane binaries under R1.
- **`bridge`** (26% / 6%). The owner kept the Jira and GitLab bridges, so their code in
  `internal/bridge` stays. Files named for them hold 24.2k source and 48.0k test lines.
  The cut came from the dead services (#1712, −5.9k), the comment diet (#1925, −2.5k),
  the embedded `configs` table (#1822) and the audit (#1699).
- **`controller-internal`** (39% / 20%). It has the second-largest plan cut (−12.2k src).
  Most came from the dead services' packages (#1712, −7.8k), the podmanager split (#1765,
  −2.9k), the coverage stack (#2085) and image-upgrade-reboot (#1819). Feature growth
  matched it (+13.1k src, +13.6k test), mostly `gardenhub`, `commons`, `steward`,
  `pgbudget` and `beadsapi`. Decision gd-Yd6ekxLlai kept `internal/coverage/*` for the
  patch-coverage gate (gd-roE5roXBKQ).
- **`kbeads`** (17% / 4%). The audit removed the unused endpoints (#1701) and the
  test-only funcs (#1895), and #1773 moved `chartCatalog` to an embedded file. The four
  handler refactors added +1.9k back. Feature growth (+11.7k src, +14.6k test, most of it
  in `internal/server` and `internal/landmap`) was three times the cut.
- **`beads-viewer`, `controller`, `gb`** (3% to 25%). R2 protects every viewer page,
  route and template. The cuts came from one comment-diet slice (#1930, −2.7k in
  `beads-viewer`), the coverage stack (#2085, #2100) and the audits (#1694, #1683,
  #1697). These targets assumed wide comment and helper cuts; the comment diet stopped
  after two slices (#1925, #1930).
- **`helm-tests`** (31%). The cuts came with deleted features: the EKS-only chart
  (#2007, −2.9k), the dead services (#1712, −1.8k) and the RWX leftovers (#1707).
  Splitting the suite to run in parallel (#1722) added +1.7k back.
- **`other`** (grew). The plan moved shared wire helpers into
  `gardenio/clicreds/beadswire` (#1769, #1780). `scripts/heraldry-proto` (3,381 lines), the
  cut this target assumed, is still in the tree.
- **`coop`** (2% / 0%). The coop audit (gd-R0jeUZAl3F, #1681) cut terraform, helm and
  scripts, not Rust; G-D6 keeps `mux.html` and `/mux`.

### Long and complex functions

Phase 4 (gd-0yUyWEztMq) took on the worst funcs, not the counts. It refactored the kbeads
handlers, split the giant files and embedded the two data tables (`configs`,
`chartCatalog`). So the ceilings moved more (55 → 43 over 300 lines, 30 → 22 over
complexity 60) than the counts did (−33 long, −7 complex source funcs). No step tried to
halve the counts. The ratchets of gd-d29n0m678l now list every func over a cap in
`.golangci.yml`, so the counts can only go down.

The longest source funcs today: `beads-viewer` `main` (927 lines), `controller` `main`
(898), `run` (712), `(*Bot).respawnThreadAgentAt` (696), `(*Bot).handleThreadSpawnAt`
(668). The most complex: `respawnThreadAgentAt` (139), `handleThreadSpawnAt` (119),
`(*Farm).refreshOnce` (115), kbeads `handlePourFormula` (100).

### Reachability

- **controller union dead, 935 → 625 (580 test-only).** Plan deletions, the test-only
  removal steps among them (gd-TnbXaEWQul, gd-O5QbnKpDU0), took off 310 funcs. The rest
  are still in `gardenio/controller/.deadcode-baseline.tsv`, which may only shrink.
- **controller packages no main imports, 4 → 6.** The four test-helper packages
  (`commons/commonstest`, `conversation/surfacetest`, `golden`, `grafanagrade`) are still
  here, and two were added. `decisionview/decisiontest` is a new test helper.
  `edgebaseline` (2,085 source lines) lost its only main when the plan deleted
  `edge-idp-demo`; it was deleted after this measurement (gd-ERGt5eaBUH).
- **controller packages no shipped main imports, still 21**, over the ≤ 5 target:
  - The seven `internal/coverage/*` packages are kept for the patch-coverage gate
    (decision gd-Yd6ekxLlai).
  - `edgeauth`, and the log-shipper packages `bridgeshipper`, `traefikshipper` and
    `vlsink`, back the built-off mains the owner kept.
  - `k3sauditshipper` runs in prod, but its DaemonSet is applied outside the chart, so it
    is not one of the reachability roots.
  - The rest are `auditxform`, `ciwait`, `heraldry/kbeadssrc`, and the six packages from
    the bullet above.
- **kbeads `heraldry/themeglyphs`** is still the one kbeads package no shipped main
  imports.

### Binaries built versus deployed

Controller `cmd/` mains, sorted by the rubric's status words. "Built" means compiled in
`_controller-base`'s `for bin in` loop, with an enabled entry in `.github/images.json`.

| | 10-31 (`cfbe53002`) | 11-03 (`03250ef28`) |
|---|---:|---:|
| `cmd/` mains | 51 | 34 |
| run in prod | 8 | 9 (`github-bridge` turned on, #2083) |
| shipped at build time (`splash-export`) | 1 | 1 |
| built, not deployed | 17 | 7: `edge-auth`, `gitlab-bridge`, `jira-bridge`, `push-bridge`, and the `bridge`, `core` and `traefik` log shippers |
| no image (probes, test lane, CI tools) | 25 | 17: 14 probes, `claudemore`, `coverage-patch`, `coverage-baselines` |
| compiled by `_controller-base`'s loop | 24 | 15 |
| images in `images.json` (enabled / off) | 30 (13 / 17) | 21 (14 / 7) |

Deleted since the baseline: `alertmanager-bridge`, `audit-enrich`, `ava-log-shipper`,
`coverage-bot`, `edge-idp-demo`, `idp-log-shipper`, `inviteservice`,
`microvm-image-register`, `mr-state-backfill`, `peel-broker`, `repro-cockpit`,
`roles-server`, `scif-portal`, `scrub-coop-token`, `spendlogs-probe`, `stub-mcp-server`,
`wl-bridge`.

### Follow-ups

- PR #2155 (gd-c3yecUhhrY) only regenerates the coverage baselines. Merging it later
  changes no number here.
- Deletion candidates left by the plan: `scripts/heraldry-proto`. `internal/edgebaseline`
  (no main imported it) was deleted in gd-ERGt5eaBUH. The five owner-kept mains can go
  when the owner revisits gd-rZJdGewUse.

## Ratchets

Task gd-d29n0m678l. Three checks in [`checks.yml`](../../.github/workflows/checks.yml)
stop the numbers above from climbing back, and an advisory fourth in
[`ci.yml`](../../.github/workflows/ci.yml) holds new code to a test-coverage bar. Each one fails only when something **grows or
appears**. Deleting code never turns one red and never needs a baseline edit, so the
deletion PRs of this plan can land in any order.

| ratchet | fails when | runs in | baseline (may only shrink) |
|---|---|---|---|
| line budget | a Go package's src or test lines pass its row plus its grants | `quench` on every ready PR, ungated; `make lint` | [`scripts/ratchets/line-budget.tsv`](../../scripts/ratchets/line-budget.tsv), plus the grants in `scripts/ratchets/line-budget.d/` |
| function caps | an unlisted func is long (over 100 lines) or complex (over 30); a listed func passes its own cap | golangci-lint (`funlen`, `gocyclo`) in `lint-go`; `make lint` | the `func-baseline` block of each module's `.golangci.yml` |
| deadcode | a func no shipped main reaches is not in the baseline | `lint-go`, after golangci-lint, controller and kbeads arms | `gardenio/controller/.deadcode-baseline.tsv`, `kbeads/.deadcode-baseline.tsv` |
| patch coverage (advisory) | of the lines a PR adds to a component, its tests run fewer than the threshold (60% to start) | the `go` job's `coverage / patch (advisory)` step, controller and kbeads arms, ready PRs; a warning, never red | [`scripts/ratchets/coverage-baselines.json`](../../scripts/ratchets/coverage-baselines.json) |

### Headroom

Every feature PR in the fleet runs these, so the budgets carry room:

- **Line budget.** A package of *n* lines gets *n* + max(5% of *n*, 500), rounded up to a
  multiple of 100, for src and test apart. A package with no row gets 500 and 500, so a
  new package past that needs a row (`scripts/ratchets/line-budget.sh update` adds it).
  The lines are `code-size.sh`'s (its `packages.tsv`), the same counter as the targets.
- **Function caps.** The caps are the ones above: long is over 100 lines, complex is over
  30. funlen counts body lines, two fewer than `code-size.sh`, so `.golangci.yml` sets
  `lines: 98`. A func that was already over a cap is listed with its own cap: its count
  plus max(10%, 10 lines) for length, plus max(10%, 3) for complexity. A listed func can
  still take a small change; it cannot grow much, and it can always shrink. This task does
  not lower the caps; phase 4 (gd-0yUyWEztMq) does.
- **Deadcode.** No headroom. The roots are the shipped mains of
  [`reachability.yml`](../../.github/workflows/reachability.yml) (controller, slack-bridge,
  grafana-bridge, trigger-engine, landgate, beads-viewer, gb; kbeads `cmd/kd`), and the
  check runs deadcode once without `-test` over all of them: the "shipped-union" dead list.

### When one is red

- **Line budget**: cut lines from the package. If it really has to grow, add a grant,
  `scripts/ratchets/line-budget.d/<bead-id>.tsv`, with a `package<TAB>src<TAB>test` row
  holding the lines it adds, and say why in the PR. Do not edit the package's row in
  `line-budget.tsv`: busy packages (gardenhub) had every concurrent PR conflict on that one
  line (gd-jMdrPDGpPw). Grants add up, so two PRs that grow one package side by side each
  bring their own and git merges both. `update` folds the grants into the rows and deletes
  them.
- **Function cap**: split the func. Do not add a rule to the `func-baseline` block by hand.
- **Deadcode**: call the func from shipped code or delete it. A helper only tests use goes
  in a `_test.go` file. Do not add a row to the baseline.
- **Patch coverage**: the job summary lists the changed files whose new lines no test runs.
  Test them, or leave the warning: the gate is advisory.

### Shrinking the baselines

A stale entry only makes a ratchet looser; it never reds anything. To tighten them to the
tree, `make ratchets-shrink` lowers the line budget and the funlen/gocyclo lists (a
source parse, about a minute, safe on a pod). Neither tool ever adds an entry or raises
one. The deadcode baselines need a whole-program build of every shipped main, past a
pod's memory for the controller: run `scripts/ratchets/deadcode-ratchet.sh update
<module>` on a bigger machine, or leave them to the next re-baseline.

### Baselines as landed

Measured on `main` @ `678b9aafe` (2026-11-01):

| | controller | kbeads | clicreds |
|---|---:|---:|---:|
| listed long funcs (funlen) | 516 | 138 | 1 |
| listed complex funcs (gocyclo) | 165 | 42 | 0 |
| dead funcs (shipped union) | 768 | 70 | |

The line budget has a row for each of the 204 Go packages in the tree. Five rows were
raised by hand to the rule's budget of today's count, because main grew past the first
cut before this landed: `cmd/github-bridge` test (#1729, #1814), `kbeads/internal/landmap`
src and test (#1674, #1720, #1736), `lands/gl` src and test (#1674, #1736, #1738, #1812).
A pod cannot hold the controller's deadcode build, so its dead list came from a
`deadcode-ratchet.sh init` run on a heavy runner
([41263828795](https://github.com/mattbakerconsulting/gardenio/actions/runs/41263828795),
on `ac8351f98`), checked again on `678b9aafe` with this change merged
([41298828462](https://github.com/mattbakerconsulting/gardenio/actions/runs/41298828462):
709 dead, none new; the 59 no longer dead go at the next `update`).

### Patch coverage

Task gd-roE5roXBKQ wired in the RWX-era `cmd/coverage-patch`, which decision gd-Yd6ekxLlai
kept. It judges only the lines a PR adds: old untested code never counts, so like the
ratchets above it fails only on growth.

- **Input.** The `go` job's test step writes `-coverprofile` (each package counts its own
  tests; no `-coverpkg`). `go-to-lcov.sh` and `normalize-paths.py`, the `make lcov-go`
  scripts, turn it into LCOV with repo paths. The diff is the merge ref against its first
  parent. Each arm judges its own component; the others have no lines there and are exempt.
- **Rule.** Per gated component (controller, kbeads, coop), covered / added executable lines
  must reach the threshold, max(60, baseline − 10). Under 10 added lines, it is exempt.
- **Cost.** Measured on a pod (three kbeads packages, warm): `go test` 114 s plain,
  135 s with the profile. The first cover build on a node costs about 145 s more, once.
  gcov2lcov installs in 11 s, once per node, and converts in about 13 s. Drafts never reach
  the `go` job, so this adds no PR runs.
- **Baselines.** The file starts at the floor, 60 for every component. Every release-tag
  push and nightly run of `ci.yml` uploads each arm's LCOV as `coverage-lcov-<module>`
  (kept 14 days); main pushes no longer run `ci.yml`. Once 10 green release-tag or nightly
  runs have one, `scripts/coverage/regen-baselines.sh` takes the median per component and
  rewrites the file. Commit that in its own PR: the bar moves only when someone does.
- **Advisory.** Under threshold is a `::warning::` and the verdict goes to the job summary,
  which replaces the GitLab comment `cmd/coverage-bot` used to post. To make it required,
  drop the step's `continue-on-error` and its exit-1 arm, and pass the PR's commits as
  `--commits-json` so a `Coverage-Bypass:` trailer can waive it.

`gb coverage-ratchet` (a whole-component floor kept in a coverage-baseline bead) was the
other coverage gate. It was never wired, since it needed the kbeads daemon from CI, and was
deleted with `cmd/coverage-bot` (gd-nfGsFuy8Xb).
