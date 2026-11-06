# MR state: single source of truth

Status: canonical (ka-0000005367, W-A from ka-0000002952)

## What this doc settles

`type=merge-request` beads carry three closely-related fields that all answer
"what happened to this MR?":

| Field        | Domain                                | Where it comes from                             |
|--------------|---------------------------------------|-------------------------------------------------|
| `mr_state`   | `opened` \| `closed` \| `merged` \| `locked` | GitLab `merge_request.state` (live mirror) |
| `mr_outcome` | `merged` \| `closed`                   | Stamped once on terminal transition             |
| `mr_merged`  | `"true"` \| `"false"`                  | Derived from `mr_state` on terminal transition  |

Historically the bridge wrote these inconsistently — pipeline / note / emoji
webhooks did not forward `mr.state`, so `mr_state` could be missing or stale
even after the MR had merged. That drift is what the W-A workstream fixes.

## Precedence rules

When code anywhere in the platform needs to answer **"is this MR done?"** or
**"what was the outcome?"**, use this precedence:

1. **`mr_outcome` (when set) is authoritative for terminal state.**
   It is only written on a real terminal transition (merged or closed) and is
   never cleared. If `mr_outcome` is set, treat the MR as terminal regardless
   of what `mr_state` says — `mr_state` may briefly lag.

2. **`mr_state` is the live mirror.**
   It tracks GitLab's `merge_request.state` and is updated on every webhook
   the bridge receives (merge_request, pipeline, note, emoji). It is the
   right field for "what is the MR doing **right now**" — live UI, gating,
   filters in beads-viewer.

3. **`mr_merged` is a derived convenience flag.**
   `"true"` iff `mr_state == "merged"` (and therefore `mr_outcome == "merged"`).
   Provided for backwards compatibility with the legacy task-bead dual-write
   and pre-existing trigger queries. **Do not write `mr_merged` independently
   of the state transition** — let `mrFieldsMap` derive it.

### Quick decision table

| You want to answer…                              | Read this field      |
|--------------------------------------------------|----------------------|
| "Is this MR terminal (merged OR closed)?"        | `mr_outcome` is set  |
| "Was this MR specifically merged?"               | `mr_outcome=merged`  |
| "What is the MR doing right now?"                | `mr_state`           |
| "Is the MR currently open and reviewable?"       | `mr_state=opened`    |
| "Coarse done/not-done for UI grouping?"          | `bead.status=closed` |

Do **not** branch on `mr_merged` directly — read `mr_outcome` instead. The
`mr_merged` field exists only to keep legacy trigger queries (`field:mr_merged
value:true`) working until they migrate.

## Canonical reader patterns

For agents, gardeners, and viewer queries, prefer these shapes:

```text
# MRs currently open (live)
type:merge-request mr_state:opened

# MRs that ever merged
type:merge-request mr_outcome:merged

# MRs terminal in either direction
type:merge-request mr_outcome:*

# Avoid: relying on bead.status alone — it lags the live mr_state by one
# webhook for non-terminal transitions (e.g. closed -> reopened).
```

## The bead status vs the field

`type=merge-request` beads also have a `status` field (`open`, `closed`, …).
This is the *bead* lifecycle, not the *MR* lifecycle:

- `bead.status = "open"` while the MR is live (any non-terminal `mr_state`).
- `bead.status = "closed"` when the MR has reached a terminal `mr_state`.

The bridge transitions `bead.status` -> closed via `CloseBead` whenever it
sees a terminal MR state. Code consuming `bead.status` should treat it as a
coarse "is this MR done" signal; for the actual outcome, read `mr_outcome`.

## Who writes what

| Writer                                              | Fields it touches                                       |
|-----------------------------------------------------|---------------------------------------------------------|
| `internal/bridge/gitlab_sync.go` (webhook handlers) | `mr_state`, `mr_outcome` (on terminal), `mr_merged` (derived), pipeline / branch / approval fields |
| `internal/beadsapi/client_mr.go` (`mrFieldsMap`)    | Derives `mr_merged` from `mr_state` automatically       |
| `internal/bridge/gitlab_mr_terminalize_drift.go` (scheduled sweep) | `mr_state`, `mr_outcome`, `mr_merged` on beads whose MR terminalized without a webhook |

Everything else (formulas, gardeners, downstream agents) should be a **reader**
only.

## How the bridge keeps `mr_state` honest

Every GitLab webhook event whose payload includes a `merge_request` object now
forwards `mr.state` (plus source/target branches) through to
`UpsertMergeRequestBead`. The payload subset is captured by the named
`webhookMR` struct in `gitlab_sync.go`:

```go
type webhookMR struct {
    IID          int    `json:"iid"`
    URL          string `json:"url"`
    Title        string `json:"title"`
    State        string `json:"state"`         // opened, closed, merged, locked
    SourceBranch string `json:"source_branch"`
    TargetBranch string `json:"target_branch"`
}
```

Pipeline, note, and emoji webhooks all decode their embedded MR object into
`*webhookMR` and pass it through. Terminal states (`merged`, `closed`) also
trigger `UpsertMergeRequestBead(..., closed=true)`, which stamps `mr_outcome`
and transitions the bead to `status=closed`.

Tests pinning this behaviour:

- `TestGitLabWebhookHandler_PipelineEvent_ForwardsMRState`
- `TestGitLabWebhookHandler_PipelineEvent_ClosesOnTerminalState`

## Backfill (one-shot remediation, 2026-05-24)

A one-shot `cmd/mr-state-backfill` walked every `type=merge-request` bead,
fetched the canonical MR state from GitLab, and stamped `mr_state` /
`mr_outcome` / `mr_merged` where they drifted (ka-0000005367). It ran once
against the `gardenio` namespace beads daemon on 2026-05-24, after the bridge
fix in commit `244d529c6` deployed:

| Metric             | Count |
|--------------------|-------|
| MR beads scanned   | 1170  |
| Already correct    | 671   |
| `mr_state` drift   | 495   |
| `mr_outcome` drift | 488   |
| Beads closed       | 487   |
| Fields updated     | 8     |
| GitLab 404 errors  | 4 (test/garbage URLs, ignored) |

The drift population was larger than the initial 32+6 sample in the W-A
research because the bridge had been silently dropping `mr.state` from every
pipeline/note/emoji webhook for the lifetime of `type=merge-request` beads.
The bridge fix (`webhookMR` forwarding) closes the leak; the backfill cleared
the accumulated debt.

The command was deleted with the other unshipped tools (gd-Xoy0GRN58f). Drift
that slips past the webhooks now is handled by the gitlab-bridge MR-terminalize
sweep (`internal/bridge/gitlab_mr_terminalize_drift.go`, ka-0000001293), which
re-polls GitLab on a schedule. To rerun the original backfill, recover it from
git history: `git log --diff-filter=D -- gardenio/controller/cmd/mr-state-backfill`.

## See also

- Epic ka-0000002952 — Class A `mr_state` drift workstreams
- Task ka-0000005367 — W-A (this fix)
- Epic ka-0000001613 — `type=merge-request` as canonical MR source of truth
