# Archived plans

Plans here are **superseded, shipped, or otherwise no longer the plan-of-record**.
They are kept for historical context — to understand what was planned, what
trade-offs were considered, and what shipped — but they should NOT be treated as
authoritative for current work.

When you find yourself reaching for a plan in this directory, first check:

- Is there a newer plan that supersedes it? (e.g. `escalation-v2-agent-driven-pipeline.md`
  supersedes the V1 plans here.)
- Is the feature already shipped? (Most plans here describe work that has
  fully landed in `main`.)
- If you genuinely need the historical detail, fine — just be aware the file
  reflects the world at the time it was written.

## Why this directory exists

Active plans live in `gardenio/plans/`. The directory was getting cluttered
with completed work, making it hard to tell at a glance which plans are
still in flight. Moving completed plans here keeps the active surface
focused on actual planning work.

## How to archive a plan

```sh
git mv gardenio/plans/<plan>.md gardenio/plans/archive/
# Then update any inbound references (grep -r '<plan>.md' .)
```

A plan is a candidate for archival if:

- Its description starts with **Status: Complete / Implemented / Shipped**.
- The work it describes has fully landed and no follow-up is open.
- It has been explicitly superseded by a newer plan (link the newer plan
  from this README or the archived file).
