# Coverage badge

At-a-glance combined-coverage % on the README, refreshed on every green
main-push pipeline. Tracks bead `ka-0000007141` (Coverage Phase 2b W2b-4);
spec: `docs/research/coverage-phase2b-design.md` §6.

> **This is a display-only badge cache.** It is *not* the §8.4 ratchet
> baseline storage (a 2c artifact). The two share a registry but use
> different package keys (`coverage-health/` vs `coverage-baselines/` vs
> `coverage-ratchet/<component>/`). Do not conflate.

## What you see

A single badge in the README:

- **coverage** — combined line-coverage % from the latest green main-push
  merge artifact (e.g. `coverage: 65.6%`). Color degrades green →
  yellowgreen → yellow → orange → red as the % drops.

## How it works

1. `coverage-report` (in `.gitlab-ci.yml`) already produces
   `coverage.merged.lcov` on every MR pipeline AND every main-push
   pipeline (W3 / Phase 2a contract).
2. The `coverage-badge-refresh` GitLab CI job (W2b-4) runs on main-push
   and tag pipelines, `needs: [coverage-report]`, fail-soft
   (`allow_failure: true`). It:
   - Computes the combined % from `LF:` / `LH:` records in the merged
     LCOV (no dependency on the merge task's stdout `Coverage: NN.N%`
     line — the LCOV artifact is the source of truth).
   - Picks a shields.io named color via the W4 §5b convention
     (≥90 brightgreen, ≥80 green, ≥70 yellowgreen, ≥60 yellow,
     ≥50 orange, <50 red).
   - Writes a shields.io `/endpoint` JSON document.
   - Uploads to the GitLab Generic Package Registry under
     `coverage-health/latest/coverage.json` (overwritten every main
     push — latest upload wins on `GET`).
   - On tag pipelines, also writes a stamped audit copy under
     `coverage-health/<calver>/coverage.json` (best-effort).
3. The README badge points at shields.io `/endpoint`, which renders an
   SVG from the JSON file.

## JSON schema

```json
{
  "schemaVersion": 1,
  "label": "coverage",
  "message": "65.6%",
  "color": "yellowgreen"
}
```

## Badge URL

```
https://img.shields.io/endpoint?url=https%3A%2F%2Fgitlab.com%2Fapi%2Fv4%2Fprojects%2FAcme%252FPlatform%252Fgardenio%2Fpackages%2Fgeneric%2Fcoverage-health%2Flatest%2Fcoverage.json&logo=gitlab
```

## Private-project caveat

Generic Packages inherit project visibility. On a private project,
`https://gitlab.com/api/v4/projects/.../packages/generic/...` returns
401 without an authorized caller — shields.io cannot fetch it
anonymously. The release-health badges have the same caveat
(`docs/release-health-badge.md`); the coverage badge piggybacks on the
same fallback, not on a new mechanism. Resolution is shared:

1. **Public Pages mirror** *(recommended)* — one GitLab Pages job
   mirrors both `release-health/latest/*.json` and
   `coverage-health/latest/coverage.json` to a public URL. shields.io
   can fetch from Pages even when the source project is private.
   Switching is one URL substitution per badge in the README.
2. Public sidecar project / gist mirror.
3. Make the project itself public.

While the project remains private and no Pages mirror is live, all
three shields.io badges (`release`, `release time`, `coverage`) render
as `inaccessible` for anonymous viewers; logged-in GitLab viewers see
the real values. This is the documented current state for
release-health and now coverage. Tracked as a single follow-up that
fixes both badges at once — **do not** file a coverage-specific
follow-up.

## Design distinctions

| Artifact path                                        | Purpose                              | Rewrite cadence              |
|------------------------------------------------------|--------------------------------------|------------------------------|
| `coverage-health/latest/coverage.json`               | **This badge** (display-only cache)  | Every green main push        |
| `coverage-health/<calver>/coverage.json`             | Audit history                        | Once per release tag         |
| `coverage-baselines/latest/baselines.json`           | Per-component baselines (W2b-6)      | Manual recompute (one-shot)  |
| `coverage-ratchet/<component>/...`                   | 2c ratchet floor (never landed; `gb coverage-ratchet` deleted, gd-nfGsFuy8Xb) | —        |

The badge cache and the ratchet floor are different artifacts at
different paths with different read/write semantics. Phase 2b ships
the badge only.
