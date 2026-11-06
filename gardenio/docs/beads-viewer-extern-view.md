# Extern view — external objects (`/extern`)

**Epic:** [ka-0000006497](https://beads.gardenio.e2e.example.com/ka-0000006497) ·
**Operator:** Matt Baker (he/him) ·
**Slack thread:** thread-1781621652-772229

A new beads-viewer view, **`/extern`**, that surfaces externally-sourced
gardenio objects — for now **GitLab merge requests** and **JIRA tickets** — as
first-class rows with deep links out to GitLab/JIRA and status display. The
framing is gardenio-native: every row links back to the underlying bead, shows
the project, and reflects both the external state and the gardenio-side bead
status.

This is the **MVP scaffold** Matt asked for alongside the research; a gardener
([see below](#gardener)) drains the epic's child tasks to make it wonderful.

## What shipped (MVP)

A live list page at `/extern`, plus a `Extern` nav link:

- **Two sources, one feed.** Merge requests and JIRA tickets are unified into
  one newest-first stream (`-updated_at`). Source chips at the top filter to
  one or the other and show live counts.
- **Deep links out.** Each row leads with a bordered external-link pill — `!4459`
  → the GitLab MR, `PE-7001` → the JIRA issue — opening in a new tab.
- **Status pills.**
  - MRs: `mr_state` (opened/merged/closed/locked), pipeline status (clickable
    through to the pipeline when `mr_pipeline_url` is set), and target branch.
  - JIRA: issue type + `jira_status`.
  - Both: a secondary chip for the gardenio-side bead status (closed / in
    progress) so the local lifecycle is visible next to the external one.
- **Filters.** `?source=mr|jira`, `?project=<name>` (project chips are
  clickable), `?status=<status>` (default `open`; `all` shows every status).
- **Sort.** `?sort=updated|oldest|title` — newest activity (default), oldest
  first, or case-insensitive by title.
- **Group.** `?group=project|state|source` sections the list (flat by default);
  the densest section leads. State buckets MR `mr_state` and JIRA `jira_status`
  into one lens.
- **Live GitLab strip.** A 60s-cached open / mergeable / draft / merged-today
  count bar for the gardenio project, reusing the `/index` MR-status panel's
  lister + cache. Shown only when MRs are in view and no project filter narrows
  below the configured project; self-hides when no GitLab token is wired.
- **Empty + truncation states** mirror the `/escalations` / `/active`
  conventions. The empty state offers one-click *Show all statuses* / *Clear
  filters* recovery actions plus a hint on how rows arrive.

## Data model

Researched 2026-06-16 against the live daemon.

| | Merge requests | JIRA tickets |
|---|---|---|
| Identified by | `type=merge-request` bead | label `source:jira` (type is usually `task`) |
| Created by | `gitlab-bridge` dual-write | `jira-bridge` poller |
| Deep-link field | `mr_url` | `jira_url` (fallback: `<jiraBaseURL>/browse/<jira_key>`) |
| Short id | `mr_iid` / `gitlab_mr_iid` | `jira_key` |
| Title | `mr_title` → bead title | bead title |
| Status fields | `mr_state`, `mr_pipeline_status` (`pipeline_status` fallback), `mr_pipeline_url`, `target_branch`, `author` | `jira_status`, `jira_type`, `jira_assignee` |
| Open count (2026-06-16) | ~360 | (E2E test artifacts only in this env) |

> JIRA beads are queried by the `source:jira` **label**, not by a dedicated
> type. Both bridges write read-only mirrors — bead edits do **not** flow
> upstream (advice ka-0000003234).

## How it works

| Layer | File | Notes |
|---|---|---|
| Handler + projections | `cmd/beads-viewer/extern.go` | `handleExtern`, `externRowFromMR`, `externRowFromJira`, `externRowView`. Two daemon queries (merge-request type, source:jira label), merged then sorted/grouped. `externState` builds dimension-preserving chip URLs; `sortExternRows` / `groupExternRows` are pure, unit-tested projections. |
| Template | `cmd/beads-viewer/templates/extern.html` | Source/sort/group control rows, a reusable `extern_row` block shared by flat + grouped lists, the live MR-status strip, scoped CSS for MR-state / pipeline / JIRA pills. |
| Route + page reg | `cmd/beads-viewer/server.go` | `GET /extern`; `extern.html` added to `pageNames`. |
| Nav | `cmd/beads-viewer/templates/layout.html` | `Extern` link after `Escalations`. |
| Tests | `cmd/beads-viewer/extern_test.go` | Happy path, per-source filters, empty state, projection fallbacks. |

The page is **read-only** — it never mutates external objects. Status reflects
the last sync written by the bridges.

## Reuse / prior art

- `mr_pill` partial + `wrapUpMRView` (`agents.go`) — the MR state×pipeline pill
  shape; `/extern` uses the same `mr-state-*` / `pipe-*` class names so the
  palette stays consistent.
- `index_mr_status.go` — live GitLab open/mergeable/merged-today counts (the
  polish task wires this in for a header strip).
- `linkify.go` — `jiraBaseURL()` and the JIRA project-key allowlist
  (`FSM,PE,DEVOPS`), reused for the URL fallback.
- `escalations.go` / `active.go` — list-page handler + template patterns.

## Gardener

A scheduled gardener is provisioned against the epic to drive the remaining
child tasks (live status enrichment + caching, richer sort/group, polish) to
done. See the epic's children for the work breakdown.

## Polish (ka-0000005062)

The polish pass landed sort + group controls, the live GitLab MR-status strip
(reusing `index_mr_status.go`'s 60s-cached lister), and richer empty states with
one-click recovery actions. Render + projection unit tests cover each
(`sortExternRows`, `groupExternRows`, `externState.chipURL`, the MR-live strip,
and its suppression under `source=jira` / a project filter).

## Follow-ups / ideas (for the gardener)

- Per-source live JIRA enrichment (the strip is GitLab-MR only today).
- A dedicated "mergeable / needs-review" MR lens beyond `group=state`.
- Linked-agent / linked-task column (walk the `produces` edge for MRs).
- Approval state for MRs (`mr_approved` / `mr_approvers` are already written by
  the bridge for some MRs).
- Optional JIRA epic grouping via `jira_epic`.
