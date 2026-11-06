# Release-Drought Deadman Watchdog

> **Superseded (2026-10-27)** by [`releases-design.md`](releases-design.md) (gd-hls6BkIEiO). It watched GitLab Release objects from gitlab-bridge, which is off in the k3s values; a release bead's `cut_at` is the drought signal now. Kept for history.

> Bead: **ka-0000002594** · Origin: P0 post-mortem **ka-0000007176**
> Code: `gardenio/controller/internal/bridge/release_drought.go` (hosted by gitlab-bridge)

## Why this exists

On 2026-07-01/02, **16 consecutive tag releases failed silently over ~15h**
(ka-0000007176). The release pipeline's own failure notifiers were both down —
the `SLACK_RELEASE_WEBHOOK` RWX vault secret was missing and the release-gate
bug-bead auto-file POSTed 403 — so a broken pipeline hid behind a broken
notifier. The operator noticed only by the *absence* of releases.

This watchdog is the deadman switch for that failure class. It alerts on the
**absence of success**, not the presence of failure, and shares **no code path**
with the release pipeline's notify legs:

| Property | Release pipeline notify | This watchdog |
|---|---|---|
| Runs in | RWX release run (only when a run executes) | gitlab-bridge, on a ticker |
| Slack credential | `SLACK_RELEASE_WEBHOOK` (RWX vault) | slack-bridge's own bot token, via the trigger-notice bead relay |
| Beads credential | `BEADS_CI_WRITE_TOKEN` (RWX secret) | `BEADS_DAEMON_TOKEN` (bridge token, ESO-synced) |
| Fires on | a failure event happening | success **not** happening |

## Drought semantics

A **drought** is: *the oldest unreleased calver tag has been waiting longer
than the threshold (default 6h).*

- "Released" means a **GitLab Release object exists** for that calver or a
  newer one. `create-release` is the *last* step of the release pipeline, so a
  Release is the canonical "fully succeeded" artifact — on 2026.183.2 prod
  deployed but the pipeline still went red and no Release was cut; that
  correctly still counts as drought.
- Releases with `upcoming_release: true` are placeholders and don't count.
- Anchoring at the oldest waiting tag's commit time (rather than a wall-clock
  "no release in the last N hours" window) means:
  - a tag cut 20 minutes ago with its pipeline still in flight **never**
    false-positives, even after a long quiet period with no releases;
  - a tag stuck for 6h **always** alerts, exactly the P0 shape (tags pushed,
    nothing shipping);
  - a release cut for a *newer* calver retires all older waiting tags (a
    skipped build is not a drought).
- Quiet periods (no tags pushed) never alert — nothing is waiting.

## What fires when a drought is detected

1. **Slack**: a `🚨🚨 RELEASE DROUGHT` message via the trigger-notice bead
   relay (`NewNoticeNotifier` → closed `type=message` bead labeled
   `trigger-notice` → slack-bridge posts it on its own credentials to the
   default channel). Re-alerts every `repeat` (default = threshold) while the
   drought persists, and posts a ✅ recovery notice when a release lands.
2. **P0 bug bead**: filed once per episode, labeled `release-drought`,
   `source:release-drought-watchdog`, `project:gardenio` — it enters the ready
   queue for gardener-delivery to dispatch triage. Deduped against any
   open/in-progress bug already carrying the `release-drought` label, so
   bridge restarts mid-drought don't pile up beads.
3. **Prometheus gauges** on gitlab-bridge `:9092/metrics` (scraped via pod
   annotations):
   - `gardenio_release_drought_active{project}` — 0/1
   - `gardenio_release_oldest_unreleased_tag_age_seconds{project}`
   - `gardenio_release_unreleased_tags{project}`
   - `gardenio_release_last_release_timestamp_seconds{project}`
   - `gardenio_release_drought_last_check_timestamp_seconds{project}` — the
     watchdog's own deadman signal
   - `gardenio_release_drought_check_errors_total{project}`

   An optional PrometheusRule
   (`helm/gardenio/templates/gitlab-bridge/release-drought-alert.yaml`,
   `gitlabBridge.releaseDrought.prometheusRule.enabled`) adds `ReleaseDrought`
   and `ReleaseDroughtWatchdogSilent` alerts — a leg that survives even a
   beads-daemon outage.

## Configuration

Helm values under `gitlabBridge.releaseDrought` (env vars in parentheses):

| Value | Default | Meaning |
|---|---|---|
| `enabled` (`RELEASE_DROUGHT_ENABLED`) | `false` | Master switch. Enable on exactly **one** bridge per watched release lane (prod) — staging/ephemeral envs watching the same GitLab project would duplicate every alert. |
| `project` (`RELEASE_DROUGHT_PROJECT`) | `example-org/gardenio` | GitLab project path to watch. |
| `threshold` (`RELEASE_DROUGHT_THRESHOLD`) | `6h` | Max age of the oldest unreleased calver tag before alarm. |
| `interval` (`RELEASE_DROUGHT_INTERVAL`) | `15m` | Poll cadence (two GETs per tick). |
| `repeat` (`RELEASE_DROUGHT_REPEAT`) | = threshold | Re-alert cadence while drought persists. |
| `createBead` (`RELEASE_DROUGHT_CREATE_BEAD`) | `true` | File the P0 bug bead on episode start. |
| `beadProject` (`RELEASE_DROUGHT_BEAD_PROJECT`) | `gardenio` | Project label on the filed bead. |

Prod enables it in `deploy-values/gardenio.yaml`.

## When the alarm fires — triage runbook

1. **Confirm**: `gb release status` gives the structured verdict
   (`state`, `golden_tag_age_min`, `consecutive_rc_count`); or look at
   https://gitlab.com/example-org/gardenio/-/pipelines filtered to the
   newest calver tags.
2. **Read the failing job**: the failed GitLab `rwx-*` job log prints the RWX
   `RunURL` early — click through for full task logs (`rwx logs <run-id>
   --task <key>`). Common culprits and history live in the P0 post-mortem
   ka-0000007176 (release-gate module parsing, verify-prod timeouts, flaky E2E
   modules).
3. **Distinguish** repeated same-module E2E flake (fix/quarantine the module)
   from structural pipeline failure (fix the pipeline) from infra wedge
   (see `docs/ci-green-watchdog-gardener.md` for the shared-infra checks).
4. **Close the loop**: the watchdog files/dedups one P0 bug bead per episode —
   work it, don't silence it. The alarm clears itself (and posts a recovery
   notice) on the next tick after a Release lands.

### False-alarm checklist

- **Deliberate release freeze**: if tags are being cut but promotion is
  intentionally held (e.g. prod-freeze), either stop cutting tags or bump
  `threshold` for the freeze window.
- **Watchdog itself down**: `ReleaseDroughtWatchdogSilent` (PrometheusRule
  leg) or a stale `gardenio_release_drought_last_check_timestamp_seconds`
  means the bridge pod or its GitLab token needs attention — check
  `gardenio_release_drought_check_errors_total` and the gitlab-bridge logs
  (`release-drought watchdog: check failed`).

## Design notes

- Tag age uses the tagged **commit's** timestamp (`commit.created_at`).
  Release tags are cut on a fresh HEAD by `scripts/release.sh`, so this is
  within seconds of the push; for a tag on an older commit it only errs
  earlier — the conservative direction for a deadman.
- `ListTags` fetches one page (100 tags, newest-first). At ~20 tags/day that
  is ~5 days of history; a drought older than that would have alerted long
  before the anchor could fall off the page.
- Episode state is in-memory. A bridge restart mid-drought re-alerts once
  (the notice) but the bug-bead leg dedups against the still-open bead, and
  `repeat` bounds the notice noise.
- A transient GitLab API failure neither alerts nor resets episode state —
  it increments `..._check_errors_total` and retries next tick.
