<!--
  TRICKLE MR TEMPLATE — monorepo-dark peel-on-demand delivery
  ===========================================================
  Source spec: R5 bundle ka-0000008221 §2.3 "Anatomy of an ideal trickle MR".

  A trickle MR is a tiny, single-purpose slice peeled out of already-merged
  monorepo-dark state and delivered to real main. The heavy lifting already
  happened in the lab — this MR is the *receipt*, not the exploration.

  Principle: verbose-in-lab, terse-on-delivery. Keep the diff small and the
  summary to one paragraph; put the trust-earning detail in the PROVENANCE
  block below — it is structured on purpose, not prose.

  The provenance block is REQUIRED and enforced by
  `scripts/lint-trickle-mr.sh` (run in CI / `make lint`). Do not delete the
  `<!-- PROVENANCE:BEGIN -->` / `<!-- PROVENANCE:END -->` markers or the
  `- **…:**` field lines between them. Replace the placeholder values; do not
  remove the keys.
-->

## Summary

<!-- One paragraph. What this does and why it was requested. NOT a re-derivation
     of correctness — that lives in the provenance receipt below. -->

_One-paragraph, human-facing summary goes here._

<!-- PROVENANCE:BEGIN -->
## Provenance

- **Source dark commits/MRs:** <!-- e.g. dark!1234, abc1234 — the slice was peeled from these -->
- **Bead IDs:** <!-- ka-XXXXXXXX (originating request) + any related beads -->
- **Affordance results:** <!-- /code-review: pass · /security-review: pass · tests: pass — link the dark runs -->
- **Dwell time in dark:** <!-- how long this slice has lived + been exercised in dark, e.g. "6 days, 3 dark MRs" -->
- **Beads-viewer link:** <!-- https://gardenio.e2e.example.com/beads/ka-XXXXXXXX -->
<!-- PROVENANCE:END -->

<!--
  Security receipt (docs/security-scan-gate.md) — the machine-parseable
  companion to the Affordance results line above. Generate with
  scripts/security/security-scan-block.sh; `gb mr security-gate` re-validates
  the recorded versions against gardenio/locks/security-tools.lock + the named CI checks.
-->
<!-- Security-Scan: <sha> gitleaks=8.21.2=clean govulncheck=1.1.4=0-reachable osv-scanner=1.9.2=clean cargo-audit=0.21.2=clean cargo-deny=0.16.4=pass [security-review=high=0-findings] -->

## What this is NOT

<!-- Scope boundary so the reviewer knows the edges without hunting.
     e.g. "Does NOT touch the auth middleware; does NOT change the public API." -->

_Explicit scope boundary goes here._

## Tests

- [ ] Tests exercising exactly this slice are included in the diff.

---

<!-- Real main stays untouched until a human clicks merge. This MR is
     human-merge gated by design. -->

Operator: <!-- name of the human who requested this peel -->
