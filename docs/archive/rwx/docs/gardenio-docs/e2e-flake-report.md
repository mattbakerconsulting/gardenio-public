# E2E flake report

_Generated: 2026-07-29T17:41:43Z_

Source: RWX Captain flaky-tests API for suite `gardenio-e2e` — per-module flakiness over Captain's rolling 7-day window.

- `flakes (7d)` is the number of flaky occurrences Captain recorded for that module in the last 7 days (a flake = a test that both passed and failed for the same commit).
- `last flake` is the date of the most recent flaky occurrence.
- `status` flags a module `⚠️ flaky` at ≥ 1 flake(s) over the window, or `🔒 quarantined` if Captain is currently suppressing it.
- **3 of 26 module(s) flagged** at the current flake threshold (1).

Regenerate: `go run ./gardenio/controller/cmd/e2e-flake-report`

| module | flakes (7d) | last flake | status |
|---|---:|---|---|
| decisions-yield | 14 | 2026-07-29 | ⚠️ flaky |
| trigger-system | 1 | 2026-07-27 | ⚠️ flaky |
| gate-system | 1 | 2026-07-25 | ⚠️ flaky |
| agent-cost-context | 0 | — | ok |
| bridge-crossowner-mr-linking | 0 | — | ok |
| bridge-fleet-token-rotation | 0 | — | ok |
| config-bead-hotreload | 0 | — | ok |
| config-role-model | 0 | — | ok |
| cred-rotation-resume | 0 | — | ok |
| credential-rotation | 0 | — | ok |
| escalation-approval | 0 | — | ok |
| escalation-crud | 0 | — | ok |
| escalation-execution | 0 | — | ok |
| escalation-v2 | 0 | — | ok |
| fleet-metrics | 0 | — | ok |
| forwardauth-403-force-logout | 0 | — | ok |
| gitlab-bridge | 0 | — | ok |
| jira-write-control-sandbox | 0 | — | ok |
| litellm-cost-tracking | 0 | — | ok |
| pool-assign-attribution | 0 | — | ok |
| pool-recycle | 0 | — | ok |
| scheduler | 0 | — | ok |
| secret-reconciler | 0 | — | ok |
| sse-reliability | 0 | — | ok |
| stale-reaper | 0 | — | ok |
| trigger-engine-smoke | 0 | — | ok |
