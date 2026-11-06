# The security bar: `Security-Scan:` trailer + `gb mr security-gate`

> Bundle: [ka-0000005053 §2.3](https://gitlab.com) (R3 Quality Affordances Catalog).
> Task: ka-0000007861. Parent epic: ka-0000005876.

This is the machine-checkable **security affordance** a gardenio MR clears before
it is trickle-eligible upstream. It formalizes gardenio's previously-asymmetric
security posture — secrets gated (`gitleaks`), Helm/k8s manifests scanned
(`helm-security`), but Go/Rust/JS **dependency** scanning entirely manual — into
named, MR-gating status checks plus a one-line honest-provenance trailer the
upstream reviewer (and `gb mr security-gate`) can re-validate.

## The named status checks (RWX era)

> **The named statuses below were RWX tasks** in `.rwx/ci.yml` (archived at
> [`docs/archive/rwx/root/ci.yml`](archive/rwx/root/ci.yml); RWX was deleted on
> 2026-09-16). On GitHub Actions the same scanners run as `.github/workflows/checks.yml`
> jobs — `gitleaks`, `security` (govulncheck + osv) and `cargo-security` — all
> advisory, and nothing posts the `CI / *` statuses named here, so
> `gb mr security-gate` has no statuses to read. The table is kept as the spec.

Every scanner version is pinned in **`gardenio/locks/security-tools.lock`** — the single
source of truth. Bumping a version there invalidates the tool cache and is the
only place a scanner version changes.

| Status check | Task | What it gates | Rollout |
|---|---|---|---|
| `CI / Gitleaks` | `gitleaks` | leaked secrets, full git history — **coverage hard**, findings advisory (rollout), see below | mixed |
| `CI / Security-Poison` | `security-poison` | the scanners-are-wired self-test (see below) | **hard** |
| `CI / Go-Vuln` | `go-vuln` | **NEW reachable** critical/high Go vulns vs the merge-base baseline | advisory |
| `CI / OSV-Scan` | `osv-scan` | **NEW** OSV advisories on `bun.lock` surfaces vs the merge-base baseline (JS/TS) | advisory |
| `CI / Cargo-Audit` | `cargo-audit` | RustSec advisories on `coop/` (promoted from coop CLI-only) | advisory |
| `CI / Cargo-Deny` | `cargo-deny` | license/bans/sources policy on `coop/` | advisory |

**Rollout posture (advisory→enforce).** The named checks are all wired into the
`pull_request` + `merge-request` `status-checks` blocks, mirroring the
`Coverage / Patch` precedent. Two are **hard** from day one — `CI / Gitleaks`
(clean + fast) and `CI / Security-Poison` (proves the rig works). The four
**dependency scanners are ADVISORY** for the initial rollout: they run the real
scan, REPORT any finding loudly in the log, and exit 0 — because coop's cargo
checks were manual-only (so they run against pre-existing debt for the first
time: a duplicate `webpki-roots` `bans` violation) and the Go/OSV diffs need to
soak. A finding surfaces without blocking. Enforcement flips on per-check when
the operator sets the GitLab branch-protection "required" toggle (Settings →
Repository → Protected branches → main) — the last mile — after the pre-existing
debt is triaged. **`gb mr security-gate` is strict regardless** — it enforces the
full bar for any agent/human who opts into running it, so the enforcement logic
ships and is tested even while CI is advisory.

## `CI / Gitleaks` — history coverage is asserted, not assumed

`gitleaks detect` (run **without** `--no-git`) walks the full git history so it
catches a secret that was committed and later removed — which is precisely what
matters before an open-source sync, since `git push` carries the history. A
working-tree-only scan (`--no-git`) is blind to a since-removed secret.

The trap this gate defends against (ka-0000001621): on a checkout where the git
walk cannot complete, **gitleaks prints an error line and STILL EXITS 0**,
reporting "no leaks found". Two ways that happens:

- **No `.git` at all.** The default RWX `code` clone (RWX era) is a `--depth=1` shallow
  fetch that then does `rm -rf .git`. gitleaks emits `fatal: not a git
  repository` / `failed to scan Git repository` and exits 0 — a zero-coverage
  green light. (This was the live state of the gate.)
- **`blob:none` partial clone, promisor unreachable.** gitleaks emits
  `fatal: unable to read <blob>` / `failed to scan Git repository` and exits 0,
  after reaching only a fraction of history.

Two things close it:

1. **The task uses `code-git`, not `code`** — the full-depth, `.git`-preserving
   clone (the same fix as the coverage-patch / osv-scan / internal-strings-audit
   gates, ka-0000002355). This is what gives the walk real history to read.
2. **`scripts/security/gitleaks-history.sh` makes coverage load-bearing.** It
   refuses an absent or shallow `.git` up front, trips on gitleaks'
   `failed to scan Git repository` line **regardless of exit code**, requires a
   `N commits scanned` line, and asserts that `N` clears a floor
   (`GITLEAKS_HISTORY_FLOOR_PCT`, default 50%) of the non-merge reachable-commit
   count. gitleaks legitimately skips merge / pure-deletion / empty commits, so
   the floor is a ratio rather than an exact `git rev-list` equality — generous
   enough never to false-red a healthy full clone, strict enough that a
   ~15%-coverage run (the 2026-08-03 failure) can never read as a pass. A real
   finding still reds via gitleaks' own `--exit-code=1`.

The hermetic unit test `scripts/security/gitleaks-history_test.sh` (in the
`test-scripts` task) drives a **stub** gitleaks over throwaway repos to prove the
wrapper reds on every silent-noop mode: no `.git`, shallow clone, git-walk error,
missing count line, and below-floor coverage.

**Rollout: coverage hard, findings advisory (ka-0000001621).** The wrapper uses
distinct exit codes — `0` clean, `1` a gitleaks **finding** (coverage was proven
first), `2` a **coverage** failure. The CI task hard-fails on `2` from day one
(that is the bug this closes) but **swallows `1` as advisory** for the initial
rollout: fixing the silent no-op turned the history gate real for the first time
and it immediately surfaced ~89 pre-existing findings across full history —
mostly `*_test.sh` secret-pattern fixtures and already-escalated live creds, none
introduced by the fix. Reddening every MR on that debt would repeat the mistake
the go-vuln/osv-scan/cargo gates avoid, so the finding is reported loudly and the
job passes while the debt is triaged under the parent OSS-sanitization epic
(ka-0000007393). Enforcement flips on by dropping the advisory swallow (the
`[ "$rc" = 1 ] && exit 0` branch in the `gitleaks` task) once the history
findings are allowlisted / rotated / squashed away.

**Agent-workspace caveat.** Agent workspaces are `blob:none` partial clones, so
a *manual* `gitleaks detect` (history variant) run inside an agent pod hits the
same silent-noop — it will appear to pass while scanning almost nothing. For an
ad-hoc secret check in a workspace, either use the working-tree scan
(`gitleaks detect --no-git --config .gitleaks.toml`, which is complete for the
current tree) or run the history scan through
`scripts/security/gitleaks-history.sh`, which will refuse to report a false pass.
CI is the authoritative history gate.

## `CI / Go-Vuln` — reachability, not raw count

The gate runs `govulncheck -json ./...` on `gardenio/controller` + `kbeads` at
HEAD **and** at the upstream-main merge-base, and gates on the **diff**: it fails
only on a critical/high vuln that is **reachable** (a call-graph path to the
vulnerable symbol) on HEAD and was **not** reachable at the baseline.

Reachability — not the raw advisory count — is the signal. The 2026-04-19
controller-go audit found **1 of 118** known vulns actually reachable; a
count-based gate would red every MR the moment any transitive dependency picked
up a fresh advisory, reachable or not. A pre-existing reachable vuln (present on
both HEAD and baseline) is tracked but **non-gating** — it is debt, not
something this MR introduced.

- Decision function (pure, hermetically tested): `scripts/security/govulncheck-diff.py` (+ `_test.py`).
- CI orchestrator (runs the real scans at both revisions, worktree-isolated
  baseline): `scripts/security/govulncheck-baseline.sh`.
- Fails **open** on infra uncertainty (no target branch, no merge-base,
  toolchain error) so a flaky scan never reds a legitimate MR — it only ever
  HARD-fails on a concrete NEW reachable crit/high.

## `CI / OSV-Scan` — NEW-vs-baseline on bun.lock

The JS/TS dependency surface (which govulncheck does not cover). osv-scanner
lacks an npm call-graph reachability analysis, so instead of reachability the
gate uses the analogous **NEW-vs-baseline** signal: a vuln id present on HEAD
that was **not** present at the merge-base baseline. The repo's `bun.lock` files
already carry pre-existing advisories (hono, ip-address, qs, ws, …), so a
raw-count gate would red every MR — the diff gates only what this MR introduces.

- Requires **osv-scanner v2+** — the text `bun.lock` format (lockfileVersion 1)
  is only parsed by v2 (v1.9.x silently skips it as "could not determine
  extractor"). Pinned at `OSV_SCANNER_VERSION=2.3.8`.
- Decision function: `scripts/security/osv-diff.py` (+ `_test.py`).
- CI orchestrator: `scripts/security/osv-baseline.sh` (same worktree-isolated
  baseline pattern as Go-Vuln, same fail-open posture).

## The `Security-Scan:` MR-description trailer

A single machine-parseable line in the MR description records each scanner's
version and result:

```
Security-Scan: <sha> gitleaks=8.21.2=clean govulncheck=1.1.4=0-reachable \
    osv-scanner=1.9.2=clean cargo-audit=0.21.2=clean cargo-deny=0.16.4=pass \
    [security-review=high=0-findings]
```

- Format: an optional leading commit SHA, then whitespace-separated
  `scanner=version=result` tuples.
- The optional `security-review=<effort>=<result>` tuple records the two-tier
  `/security-review` outcome (see below).
- Generate it deterministically from the lock + your scan results:

  ```
  scripts/security/security-scan-block.sh --sha "$(git rev-parse HEAD)" \
      --result gitleaks=clean --result govulncheck=0-reachable \
      --result osv-scanner=clean --result cargo-audit=clean --result cargo-deny=pass \
      --review high=0-findings
  ```

  The generator pulls every **version** from `gardenio/locks/security-tools.lock` (never
  typed by hand), so the recorded versions cannot drift from the pins.

## `gb mr security-gate <bead-id-or-mr-url>` — the aggregator

Reports **GREEN** iff all of:

1. the named security status checks (`CI / Gitleaks`, `CI / Go-Vuln`,
   `CI / Cargo-Audit`, `CI / Cargo-Deny`, `CI / OSV-Scan`) are `success`;
2. the MR description carries a well-formed `Security-Scan:` block;
3. every scanner version recorded in that block **matches** the lock (version
   drift = block — an MR cannot claim a scan it ran against a stale scanner);
4. if the MR touches a **sensitive path glob**, the block records
   `security-review=high` triaged-to-zero.

Exit `0` = green (may merge), exit `3` = blocked (mirrors `gb mr relevance-gate`
/ `gb mr dispatch-gate`). `--json` for machine output. The gate does **not**
re-run scanners — it AGGREGATES their recorded verdicts, exactly as the
coverage-patch gate aggregates `patch-cov-result.json`.

Decision logic is a pure, table-tested function:
`gardenio/controller/internal/bridge/security_gate.go` (+ `_test.go`).

## Two-tier `/security-review`

Per §2.3, `/security-review` runs at **effort=low on every MR diff** and is
escalated to **effort=high** when any changed file matches a sensitive path glob:
auth/token/credential/crypto/deserialization code, `.gitleaks.toml`,
securityContext templates, or any dependency manifest/lockfile
(`go.mod`/`go.sum`, `Cargo.toml`/`Cargo.lock`, `bun.lock`, `package.json`/lock).

Classify an MR's tier from its diff:

```
gb mr security-tier <bead-id-or-mr-url>          # prints LOW/HIGH + the triggering paths
```

`/security-review` is **agent-driven** — a dark MR clearing CI does *not* mean it
ran. Run it on the branch diff at the classified effort, `--comment` (never
`--fix` on security findings), triage findings to zero, then record the outcome
in the `Security-Scan:` block's `security-review=` tuple. The security-gate is
what enforces that a high-tier diff actually recorded a high-effort review.

## `CI / Security-Poison` — proving the scanners are wired

The gardener-poison-module pattern applied to the security bar: plant a KNOWN bad
input, assert the gate goes RED. A scanner that silently stopped firing (wrong
config, disabled rule, un-invoked binary, a reachability regression) is the worst
failure mode — it green-washes every MR. `scripts/security/security-poison-selftest.sh`
converts that into a loud build failure:

- **G1** — `gitleaks` flags a planted fake AWS credential
  (`scripts/security/poison/fake-aws-key.txt`, allowlisted from the repo-wide
  scan in `.gitleaks.toml`).
- **G2** — the reachability-diff flips RED on a planted NEW reachable critical.
- **G3** — an imported-but-**not-called** critical does **not** gate (proves the
  reachability thesis, not just that the binary runs).

## Files

| File | Role |
|---|---|
| `gardenio/locks/security-tools.lock` | single source of truth for scanner versions |
| `.github/workflows/checks.yml` | the `gitleaks`, `security` and `cargo-security` jobs (the RWX tasks are archived in `docs/archive/rwx/root/ci.yml`) |
| `scripts/security/govulncheck-diff.py` (+ `_test.py`) | pure reachability-diff decision function |
| `scripts/security/govulncheck-baseline.sh` | Go-Vuln CI orchestrator (real scans at both revisions) |
| `scripts/security/osv-diff.py` (+ `_test.py`) | pure OSV NEW-vs-baseline decision function |
| `scripts/security/osv-baseline.sh` | OSV-Scan CI orchestrator (bun.lock scans at both revisions) |
| `scripts/security/security-scan-block.sh` (+ `_test.sh`) | `Security-Scan:` trailer generator |
| `scripts/security/security-poison-selftest.sh` + `poison/` | scanners-are-wired self-test + fixtures |
| `gardenio/controller/internal/bridge/security_gate.go` (+ `_test.go`) | lock/trailer parse, tier classifier, aggregate gate |
| `gardenio/controller/cmd/gb/mr_security.go` | `gb mr security-gate` + `gb mr security-tier` |
