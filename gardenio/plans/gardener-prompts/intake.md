# intake.md — RETIRED (redirect stub)

> **This prompt is retired. It is NOT a live gardener runbook.** Do not act from it.

The `gardener-intake` lane described by this file (cadence *every 2h at :13*, mission
*"drive the ready-queue size down"*) no longer exists as its own gardener. The live
intake lane is the **gardener-intake groomer** (`ka-0000003841`), which grooms beads out
of the `needs-intake` lane and delegates its full charter to a versioned runbook:

- **Live lane bead:** `ka-0000003841` (successor chain
  `ka-0000001610` → `ka-0000006210` → `ka-0000003841`).
- **Runbook the live lane follows:** [`../../docs/runbooks/intake-groomer.md`](../../docs/runbooks/intake-groomer.md).

Because the groomer reads its runbook from `docs/runbooks/` on a fresh-`main` boot (not
the `gardener-prompts/<name>.md` inline-copy pattern), it is **out of the gardener-prompt
drift-alarm scope by design** and has no row in
[`gardeners.manifest.json`](gardeners.manifest.json). See the *Retired predecessors* table
in [`README.md`](README.md) and the design record `ka-0000003223`
(bug `ka-0000000874`).

This stub is kept (rather than deleted) so the historical plan-doc links to `intake.md`
in `gardenio/plans/chunk-cycle-fix.md` and `gardenio/plans/tree-hugger-gardener.md` still
resolve.
