<!--
  GARDENIO DEFAULT MR TEMPLATE — proof packet required
  ===================================================
  Norm: gardenio/docs/mr-proof-packet.md (W2, ka-0000004403).

  Green CI proves the change compiles and its internal assertions hold.
  It does NOT prove the user-visible claim. The PROOF block below carries
  claim-matched evidence and is REQUIRED on every non-draft MR — enforced
  by `scripts/lint-mr-proof.sh` (CI job `lint-mr-proof`). Do not delete the
  `<!-- PROOF:BEGIN -->` / `<!-- PROOF:END -->` markers or the `- **…:**`
  field lines between them. Replace the placeholder values; keep the keys.

  Evidence must match the surface of the claim:
    viewer-ui      → screenshot (use `gb image upload <png>` → proxied URL)
    pipeline-ci    → link to the pipeline/job/CI run that exercised it
    daemon-api     → before/after query or API output (fenced block or link)
    agent-behavior → session/bead link or transcript excerpt
    chart-infra    → helm/terraform plan-or-apply output (fenced) or run link
    docs-tooling   → the diff itself may suffice; say so

  Escape hatch (rare, audited): replace the PROOF block with a single line
  `Proof-Exempt: <reason>` — exemptions are counted in the KPI pass.
-->

## Summary

_One-paragraph, human-facing summary: what this does and why it was requested._

<!-- PROOF:BEGIN -->
## Proof

- **Claim:** <!-- the user/operator-visible behavior this MR delivers, in one sentence -->
- **Surface:** <!-- one of: viewer-ui | pipeline-ci | daemon-api | agent-behavior | chart-infra | docs-tooling -->
- **Evidence:** <!-- links/output that exercise the CLAIM itself, not just internal assertions -->
- **Not established:** <!-- what this evidence does NOT prove (untested paths, envs, scales) -->
<!-- PROOF:END -->

## Beads

- ka-0000001713 <!-- originating task/bug + any related beads -->

Operator: <!-- the human who triggered this work (from BEADS_OPERATED_BY) -->
<!--
  Optional security receipt (the security bar — docs/security-scan-gate.md).
  Generate with scripts/security/security-scan-block.sh; `gb mr security-gate`
  re-validates it against gardenio/locks/security-tools.lock + the named status checks.
  Required in spirit whenever the MR touches auth/secret/crypto/deser code or a
  dependency manifest (go.mod/Cargo.toml/bun.lock) — run `gb mr security-tier`.
-->
<!-- Security-Scan: <sha> gitleaks=8.21.2=clean govulncheck=1.1.4=0-reachable osv-scanner=1.9.2=clean cargo-audit=0.21.2=clean cargo-deny=0.16.4=pass [security-review=high=0-findings] -->
