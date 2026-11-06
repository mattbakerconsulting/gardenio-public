# Release note: bird captions

Plan gd-jMl4oF2cJO (asked in thread gd-JmsR3RWvVd). Spec:
[`superpowers/specs/2026-10-28-bird-captions-design.md`](../superpowers/specs/2026-10-28-bird-captions-design.md), whose
§15 lists every step's PR. Rides the first release window after this PR merges.

## What changes for you

- **A bird says what its gardener is doing, not just "shell".** On `/garden`, each bird carries a caption on a plate,
  for example "Trial-merge main into #1282 in a detached worktree" or "Read web/birds.js". Most captions are the
  agent's own description of its call. A file path, a search pattern, a URL's host or a subagent's description fill in
  the rest.
- **Haiku captions the rest.** About 8% of shell calls come with no description. For those, Claude Haiku 4.5 writes an
  eight-word caption from the scrubbed command alone. It runs in beads-viewer, never in the agent pod, and a bird
  never waits for it: the caption lands a moment later on the plate and in the transcript.
- **Plates linger** for 6 s by their gardener after the bird has flown.
- **Long-press a plate or a bird** (500 ms), or right-click it, to open that gardener's terminal in the Glass. On the
  keyboard, `w` opens the Glass and `t` the transcript for the gardener at the cursor.
- **Transcripts.** Each gardener keeps its last 200 calls in memory: time, class, caption, duration and outcome. You
  can open one gardener's transcript from its card or the Glass, and a plot's, a project's or the farm's from its
  panels. New rows arrive live. A viewer restart empties them.
- **Who sees it:** the farm routes stay admin-only. Under the per-viewer cut, a scoped viewer gets captions only for
  its own gardeners, with bead ids masked.

## Operator notes

- **Values** (chart defaults; k3s turns Haiku on):

  | Value | Default | k3s |
  |---|---|---|
  | `beadsViewer.captions.enabled` | `true` | `true` |
  | `beadsViewer.captions.wireCap` | `80` | `80` |
  | `beadsViewer.captions.haiku.enabled` | `false` | **`true`** |
  | `beadsViewer.captions.haiku.secretName` | `""` | `gardenio-beads-viewer-litellm` |
  | `beadsViewer.captions.haiku.ratePerMinute` / `concurrency` / `timeout` / `dailyMax` / `cacheSize` | `30` / `4` / `3s` / `5000` / `4096` | defaults |
  | `beadsViewer.transcript.rows` | `200` | `200` |

  Full table, env names and metrics: `homestead-farm-read-model.md`, "Captions: switches, bounds, metrics".
- **Kill switches**, each a `helm upgrade` with `--reuse-values` away (nothing else restarts but beads-viewer):
  - `--set beadsViewer.captions.haiku.enabled=false`: no Haiku calls; deterministic captions only.
  - `--set beadsViewer.captions.enabled=false`: no captions at all. The wire, the transcript and the birds are as
    they were before captions.
- **Cost:** about $0.40 a day of Haiku, with a ceiling near $2 (5000 calls a UTC day) and the LiteLLM key's own $3 a
  day budget. LiteLLM spend is tagged `component:beads-viewer,feature:bird-captions`.
- **Dashboards:** Grafana **Gardenio Garden Health**, row "Farm: bird captions": caption sources, Haiku latency,
  Haiku spend (window), Haiku calls skipped and transcript bytes per viewer. The boards ship inside the grafana-bridge
  image, so the row appears with this release.
- **Watch after the deploy:** `description` should be most of the Caption sources panel. `haiku` should be a thin band
  with `cache` beside it. Haiku calls skipped should show neither a steady `off` (the key or the switch) nor a climbing
  `error` or `timeout` (LiteLLM). The beads-viewer log line `farm captions on=true haiku=true` confirms the switches.
- **Budgets held:** the Everyone page's activity stream is 9.7 KB/s against a 12 KB/s ceiling. An open transcript adds
  at most about 3 KB/s, to that page only.

## Known limits

- A viewer restart empties every transcript. Replay from `HOOK_EVENTS` is a later step.
- At 24 birds and one plate per gardener, a busy farm shows only each gardener's newest caption. The transcript has
  the rest.
- A description is free text, scrubbed for paths, secrets, URLs, tokens and emails. The scrubber cannot recognise
  every sensitive word, which is accepted for an admin-only surface. Re-grade a sample before the routes open to
  scoped viewers.
