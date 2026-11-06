# gardenio in one page

gardenio runs a fleet of Claude Code agents on a small k3s cluster: work arrives as beads,
agents claim it, open PRs, and ask a human only for decisions and privileged actions.

## Numbers (30 days to 2026-11-01)

Run once with [`scripts/metrics/one-pager.sh`](../../scripts/metrics/one-pager.sh) (defaults)
at 2026-11-01T01:40:57Z on `main` b2e04de63718. Window 2026-10-02 to 2026-11-01.

| Metric | Value | Source |
|---|---|---|
| Merged PRs | 1,439 (48/day) | `gh pr list --state merged`, mergedAt in window |
| Tokens, recorded sessions | 40.4M output, 0.39M input, 226M cache write, 6.34B cache read | token counts on session beads |
| Token cost at list price, **recorded floor** | $4,669 ($3.24 per merged PR) | 3,239 of 11,480 ended sessions carry counts (**28.2% coverage**) |
| Token cost at list price, **extrapolated** | $15,463 ($10.75 per merged PR) | floor + each role's mean per session x its 8,241 sessions without counts |
| Decisions asked of a human | 107.6/week (461 total) | decision beads, fleet-wide |
| Median decision latency | 33.4 min (n=409) | responded_at - created_at |
| Captain no-op rate | 29.7% (1,035 of 3,484 runs) | wrap-up progress_score = 0 |

Cost is token counts priced at Anthropic list price, never the proxy's spend dollars. The
floor counts only sessions that recorded tokens; the gap is sessions the reaper closed before
gb wrote usage (gd-88NFBy95l7). The script's header lists the remaining caveats.

## Incidents: cause, fix, link

| Incident | Cause | Fix | Link |
|---|---|---|---|
| **etcd quorum lost** (3 servers, 2026-09) | Changing the server ASG's on-demand base capacity live: the ASG launched the on-demand server and terminated a spot server 10 s later with no notice, while a second server was being removed by hand. Two of three members gone. | `--cluster-reset` on the survivor, rejoin the others one at a time. Now one server is on-demand, AZRebalance stays suspended, servers are replaced one at a time after a memory check, and never sized below 4 GiB. | [beads-on-k3s caveats](../../gardenio/docs/beads-on-k3s.md#caveats-found-while-building), [k3s-cluster](../../gardenio/docs/k3s-cluster.md#how-it-stays-up-on-spot) |
| **Spot churn kills CI jobs** | The CI pool is 100% spot. A reclaim kills the runner mid-job and GitHub marks it failed: 7 of 42 failed jobs on 2026-09-21, all heavy build/test jobs. | Dead Node objects are deleted so runner slots free up. `retry-lost-runners.yml` spots both reclaim signatures (lost-runner annotation, shutdown-signal log) and re-runs only those jobs, capped at 3 attempts. Servers drain themselves on the 2-minute notice (drill: replacement Ready in 2 m 16 s, no quorum loss). | [retry-lost-runners.yml](../../.github/workflows/retry-lost-runners.yml), [k3s-cluster](../../gardenio/docs/k3s-cluster.md#how-it-stays-up-on-spot) |
| **Cordon/drain hazard** | State lives on local-path PVs pinned to a few on-demand nodes (Postgres, NATS, metrics, the overlay-network controller). A full drain moves the pods and leaves the data behind; stopping an ASG member gets it terminated with its PVs (a server and its PVs lost on 2026-10-14). | Standing rules: never cordon or drain a node with pinned PVs before a helm upgrade, and suspend HealthCheck/ReplaceUnhealthy before stopping any ASG member. Hourly `pg_dumpall` to S3 is the backup of record. | [aws-cost-cuts: known hazards](../superpowers/plans/2026-10-21-aws-cost-cuts.md#global-constraints), [k3s-cluster](../../gardenio/docs/k3s-cluster.md) |
