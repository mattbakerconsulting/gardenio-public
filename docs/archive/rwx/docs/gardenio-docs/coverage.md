# Coverage MR comments — rollout & operations

The gardenio coverage pipeline posts a **per-component combined-coverage table**
as a comment on every code-touching merge request. This doc covers the C5
activation (the flip from dry-run to live posting) and how to operate the
switch.

## What posts, and where the switch lives

On an MR pipeline the `.gitlab-ci.yml` `coverage` job dispatches the RWX
`gardenio-coverage` workflow (`.rwx/coverage.yml`). After the merge +
cobertura-derive steps, the `combined-comment` task:

1. Fetches `baselines.json` from the GitLab generic package registry
   (`coverage-baselines/latest/baselines.json`, published one-shot from main
   by `.rwx/coverage-baselines.yml` — W2b-6). A non-200 degrades the Δ column
   to em-dash rather than failing.
2. Renders the per-component table (Unit / E2E / Combined / Δ vs baseline)
   carrying the idempotency marker `<!-- gardenio-coverage-bot:combined-cov:v1 -->`,
   distinct from the patch-cov comment's marker so both co-exist on one MR.
3. Either **posts/upserts** the comment on the MR or **dry-runs** (renders to
   the RWX run log only), gated by a single switch.

The switch is the Helm value **`global.coverage.mrComment`** in
[`helm/gardenio/values.yaml`](../../../../../gardenio/helm/gardenio/values.yaml). The `coverage` job
reads this scalar's **checked-in default at dispatch time** (a static file
read, not a live-cluster read) and forwards it as
`gardenio-coverage --param mr-comment=`:

- `mrComment: true`  → post/upsert the combined-coverage comment on the MR.
- `mrComment: false` → `--dry-run`: render the table to the run log, post nothing.

## Rollout (C5, ka-0000000718)

The comment shipped behind `mrComment: false` (log-only) as part of C4
(ka-0000005662, MR !7108, merged 2026-08-11). It then SOAKed in dry-run for
just over two weeks (through 2026-08-28) across well over 10 MR pipelines.

The SOAK audit (dry-run RWX logs on live MR branches) confirmed:

- **baselines.json is served from main** — the `combined-comment` task fetched
  it (`fetched baselines.json (1071 bytes)`) from the registry blob published
  2026-05-20 (median over 10 green main pushes).
- **Δ math is correct** — the Δ column compared the combined LCOV against the
  published baseline and rendered signed pp deltas (e.g. controller +8.0pp,
  kbeads +12.8pp, coop +11.9pp).
- **No duplicate comments** — in dry-run nothing is posted; the marker-based
  upsert path replaces (rather than appends) a prior comment when live.
- **No false positives from missing inputs** — a missing baseline degrades to
  em-dash and a missing E2E run degrades to unit-only, so absent inputs render
  as "no signal" rather than a spurious regression.

C5 flips the checked-in default to `mrComment: true`, activating live posting
on the next dispatch after the flip merges.

## Revert (per-namespace, no code change)

`mrComment` is a plain Helm value, so an operator can turn posting **off for a
single namespace** without reverting the checked-in default:

```sh
helm upgrade <release> gardenio/helm/gardenio \
  --namespace <ns> --reuse-values \
  --set global.coverage.mrComment=false
```

This only affects any consumer that reads the value at runtime; the
`.gitlab-ci.yml` dispatch reads the **checked-in** default, so to stop posting
across all MR pipelines, revert the default in `values.yaml` back to `false`
(the dry-run posture is fully preserved — the table still renders to the run
log).

## Recomputing baselines

The Δ column's baseline is intentionally **one-shot**, not continuous — see
`.rwx/coverage-baselines.yml` and `docs/research/coverage-2b-w6-baselines.md`.
Recompute after a major refactor that legitimately moves the per-component
floor via the `coverage-baselines-recompute` manual job on a green main
pipeline.
