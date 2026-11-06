# R5 Trusted-Throughput metrics pipeline

> Bead: `ka-0000000493` · Source: R5 bundle `ka-0000008221` §3.4 · Operator: matthewbaker (`slack:U0OPERATOR1`)

This is the blocking instrumentation infra for the trust **dashboard**
(`ka-0000007085`) and the verbosity/terseness **gate** (`ka-0000007571`). It
lands R5 metrics 1-10 — plus their anti-gaming balancing metrics — as queryable
Prometheus time series.

## Where it runs

The collector lives in the **gitlab-bridge** binary, which already owns the dark
GitLab MR API client, the beads daemon client, and an internal-only Prometheus
`/metrics` endpoint (`:9092`). The R5 series are registered on that same
endpoint.

It is **opt-in** and off by default:

| env | default | purpose |
|---|---|---|
| `TRUST_METRICS_ENABLED` | `false` | master switch; collector only starts when `true` AND `GITLAB_GROUP_ID != 0` |
| `TRUST_METRICS_INTERVAL` | `10m` | collection cadence |
| `TRUST_METRICS_WINDOW` | `28d` | look-back window for "recent" trickle MRs |
| `TRUST_METRICS_TARGET_BRANCHES` | `main` | csv of trickle target branches; `*`/`all` disables the branch filter |

## Data sources (§3.4)

- **dark GitLab MR API** — diff stats (`/changes`), open/merge timestamps,
  approvals (`/approvals`, gracefully treated as "no gate" on a CE 404), and
  review notes (`/notes`).
- **beads** — the peel-request timestamp (earliest producing-bead `created_at`),
  provenance bead links (the `produces` reverse edge), the merge-time trust
  survey score (`trust_score` field), and the escaped-defect marker
  (`escaped_defect` field).

The join key is the MR web URL ⟷ the `type=merge-request` bead's `mr_url`.

## The series

All series are **gauges** recomputed each window (no double-counting), prefixed
`gardenio_trust_`. Distribution metrics carry a `{quantile="p50"|"p90"}` label;
reviewer load carries a `{reviewer}` label.

| # | §3.4 metric | series | leading/lagging |
|---|---|---|---|
| 1 ⭐ | Trusted Throughput | `gardenio_trust_trusted_throughput_per_week` | lagging |
| 1/4 balance | Escaped-defect rate | `gardenio_trust_escaped_defect_rate`, `..._count` | — |
| 2 | Review effort | `gardenio_trust_review_effort_rounds{quantile}` | lagging |
| 3 | Trust index | `gardenio_trust_trust_index_median`, `..._responses` | lagging |
| 3 balance | Approval-without-comment | `gardenio_trust_approval_without_comment_rate` | — |
| 4 | Rework cycles | `gardenio_trust_rework_cycles{quantile}` | lagging |
| 5 | Time-to-merge | `gardenio_trust_time_to_merge_seconds{quantile}` | lagging |
| 6 | Diff size | `gardenio_trust_diff_size_lines{quantile}` | leading |
| 7 | Verbosity ratio | `gardenio_trust_verbosity_ratio{quantile}` | leading |
| 8 | Provenance completeness | `gardenio_trust_provenance_completeness` | leading |
| 9 | Reviewer load | `gardenio_trust_reviewer_load_per_week{reviewer}`, `..._max_per_week`, `..._mean_per_week` | lagging |
| 10 | Peel latency | `gardenio_trust_peel_latency_seconds{quantile}` | leading |

Context series: `gardenio_trust_delivered_mr_count`,
`gardenio_trust_total_mr_count`, `gardenio_trust_window_days`,
`gardenio_trust_last_collect_timestamp_seconds`.

## Definitions of note

- **Trusted Throughput (#1):** merged trickle MRs with ≤1 rework cycle, scaled
  to a per-week rate from the window length. Reaching `merged` state on the dark
  instance satisfies the "passed human review" qualifier.
- **Rework cycles (#4):** `review_rounds − 1` — the first round is the baseline
  review, not rework. Review rounds count substantive human (non-bot,
  non-system) notes.
- **Verbosity ratio (#7):** `description_chars / (1 + code_diff_chars)` with the
  structured provenance block **excluded** from the prose count (so a complete
  receipt never inflates the ratio). The gate (`ka-0000007571`) reads this.
- **Provenance completeness (#8):** an MR counts as complete only when it carries
  the structured provenance block (Dark-Bead:/Dark-MR:/Provenance-Hash:/Bead: or
  a `<!-- provenance:begin/end -->` fence) **and** is linked to its producing
  bead(s).
- **Trust index (#3):** median of the 1-5 merge-time micro-survey scores
  (responses only). The survey itself is sibling work; this pipeline reads the
  `trust_score` bead field it writes.

## Package layout

- `internal/trustmetrics/record.go` — `MRRecord`: the joined per-MR fact row.
- `internal/trustmetrics/compute.go` — **pure** functions: `MRRecord`s →
  `Metrics`. Fully unit-tested, no I/O.
- `internal/trustmetrics/provenance.go` — splits MR description prose from the
  structured provenance block.
- `internal/trustmetrics/metrics.go` — the Prometheus gauges + `Publish`.
- `internal/trustmetrics/collector.go` — the periodic collect→compute→publish
  loop with the narrow `GitLabSource` / `BeadSource` interfaces (fakeable).
- `internal/bridge/gitlab_mr_stats.go` — new `GetMergeRequestDiffStats` +
  `ListMergeRequestApprovals` GitLab client methods.
- `internal/bridge/trustmetrics_adapter.go` — wires `*GitLabClient` +
  `*beadsapi.Client` into the trustmetrics interfaces.
