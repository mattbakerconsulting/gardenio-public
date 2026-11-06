<!--
  TRUST-DOSSIER MR TEMPLATE — monorepo-dark trickle delivery to real main
  ========================================================================
  Design source of truth: R3 catalog /bundle/ka-0000005053 §2.5 (small-diff
  decomposition), §2.6 (human-readable rationale), §4 (this template).
  Master epic ka-0000005560. Task ka-0000005561.

  A trickle MR is a tiny, single-concern slice PEELED from already-merged
  monorepo-dark state and delivered to real main. The heavy lifting already
  happened in the lab — this MR is the trust-earning RECEIPT, not the
  exploration. The dossier is the RSI handoff artifact: it lets an upstream
  reviewer trust the MR without re-running scans — AI supplies the execution
  and the evidence; the human keeps research taste, judgment, and final
  verification.

  This is the FULLER sibling of trickle.md: where trickle.md carries a compact
  PROVENANCE receipt, the trust dossier adds the human-readable rationale
  (Why / Alternatives / Risk) that demonstrates UNDERSTANDING, not just
  extraction.

  The required sections below are machine-checked for PRESENCE and
  NON-TRIVIALITY by `scripts/lint-trust-dossier.sh` (run in `make lint`). Do
  not delete the `<!-- DOSSIER:BEGIN -->` / `<!-- DOSSIER:END -->` markers or
  the `## <Section>` headings between them. Replace the placeholder prose; keep
  the headings.

  Honest framing (R3 §6): the enforcing `gb mr trickle-gate` and the dark CI
  pipeline that would run the standalone/e2e legs do NOT exist yet. This
  template + its presence/non-triviality linter are the SHIPPED half; the run-ID
  Evidence lines below are filled by hand until that gate lands.
-->

<!-- DOSSIER:BEGIN -->

## Summary

<!-- What changed, 1-3 lines. Human-facing. NOT a re-derivation of correctness. -->
_One-to-three-line summary of what this slice delivers._

## Why

<!-- The problem this solves — prose, not a changelog restatement of Summary.
     The lint REJECTS if Why is empty or byte-equal to Summary. -->
_The problem this solves and why it was requested — distinct from Summary._

## Alternatives considered

<!-- >=1 named alternative approach + why it was rejected. -->
_At least one named alternative and why it was rejected._

## Risk assessment

<!-- Blast radius, rollback plan, what could break. -->
_Blast radius, rollback plan, and what could break._

## Evidence

<!-- Claim-matched evidence. Fill run-ID/artifact links by hand until the
     trickle-gate lands. Delete lines that do not apply to this slice. -->
- Standalone build: _upstream-HEAD CI run link + run ID_
- E2E: _run link + run ID_
- Coverage: _patch-cov-result link — +X% patch cov, N bypasses_
- /code-review: _review-provenance artifact link — findings + resolutions_
- /security-review: _artifact link + Security-Scan: result_
- /verify: _behavior observation, if behavior-changing_

## Provenance

<!-- The durable anchors back to the dark work this slice was peeled from.
     Dark commit SHAs are rewritten every rebase — anchor on bead ID + MR IID,
     never a SHA. -->
- Dark MR: _dark gitlab MR URL/IID_
- Dark bead: _kd-..._
- Peeled from: _kd-... (the dark work this slice was peeled from)_

<!-- DOSSIER:END -->

## What this is NOT

<!-- Scope boundary so the reviewer knows the edges without hunting.
     e.g. "Does NOT touch the auth middleware; does NOT change the public API." -->
_Explicit scope boundary goes here._

---

<!--
  Trailers. `Operator:` and `Bead:` are the fleet convention. The escape-hatch
  trailers are OPTIONAL — include one only when the corresponding bound is
  breached. Each is surfaced by the gate and files ONE follow-up bead; NONE
  auto-reverts. They mirror the `Coverage-Bypass:` organic-trailer path.

    Trickle-Size-Exception: <reason>   # only if net > 400 LOC or > 10 files
    Multi-Concern-Ack:      <reason>   # only if the diff spans > 1 concern

  Real main stays untouched until a human clicks merge — human-merge gated by
  design.
-->
Operator: <!-- name of the human who requested this peel (from BEADS_OPERATED_BY) -->
Bead: <!-- ka-XXXXXXXX (originating request) -->
