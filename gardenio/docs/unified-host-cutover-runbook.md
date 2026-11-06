# Unified-host cutover runbook (default-URL switch + legacy-host deprecation)

Operator runbook for the final lap of the path-prefix unification (epic
[ka-0000006723] under Stage 3 [ka-0000003384]). Prod is already mid-cutover:
`unifiedHost.enabled=true` + `edgeAuth.enabled=true`/`dryRun=false` +
`forwardAuth` + `viaShim:[mux,kd]` are live in `deploy-values/gardenio.yaml`.
What remains is switching the **default URLs** to the unified host and
**deprecating** the legacy per-app hosts.

**This is an atomic, coupled operation — not a set of independent edits.**
Setting `beadsViewer.ingress.pathPrefix=/beads` changes the viewer's `BASE_PATH`
globally, so every host that serves the viewer at root breaks its self-links
unless that host is redirect-only at the same time. Apply the deploy-values
block below in one change, after the prerequisites are met.

## Blast radius (verified 2026-05-29)

`legacyRedirects` (template `helm/gardenio/templates/unified-host/ingressroute.yaml`)
301s four legacy hosts to the unified host:

| key | legacy host | → | non-browser callers / coupling |
|---|---|---|---|
| `beads` | `beads-viewer.e2e.example.com` (avaIngress) | `gardenio.app/beads` | browser only — safe |
| `mux` | `coopmux.gardenio.e2e.example.com` | `gardenio.app/mux` | browser only — safe (`coopmux.basePath=/mux` already set) |
| `kd` | `beads.gardenio.e2e.example.com` (daemon HTTP) | `gardenio.app/kd` | **CI/automation hit this with a daemon token; the unified host is AVA-gated (303→GitLab). Probe: `beads.gardenio.app/v1/health`=200 today, `gardenio.app/kd/v1/health`=303.** |
| `jira` | jira-bridge host | `gardenio.app/jira` | **Jira outbound webhook POSTs here; a 301 drops the body.** |

Two more hosts serve the viewer at root and are **not** in the redirect set, so
`pathPrefix=/beads` breaks their self-links:
- `beads-viewer.gardenio.e2e.example.com` (the legacy viewer host; the
  former `directRoute` / `X-Beads-Viewer-Bypass` escape hatch was removed in
  ka-0000000153 — abandoned via decision ka-0000005537). Deprecate/redirect
  separately ([ka-0000004107]).

## Mechanism added by this MR

`unifiedHost.legacyRedirects.exclude: [<keys>]` holds back per-host redirects so
deprecation can start on the safe browser hosts while `kd`/`jira` wait for their
prerequisites. Empty list = full cutover.

## Sequence

### Step A — browser-host cutover (after Step-A prereqs)
Apply to `deploy-values/gardenio.yaml`:

```yaml
beadsViewer:
  ingress:
    pathPrefix: "/beads"          # viewer BASE_PATH=/beads; self-links carry the prefix

slackBridge:
  beadsViewerURL: "https://gardenio.e2e.example.com/beads"   # Slack permalinks -> unified host

unifiedHost:
  legacyRedirects:
    enabled: true
    exclude: ["kd", "jira"]       # daemon + jira stay on their legacy hosts until Steps B/C
```

Step-A prerequisites:
1. Deprecate/redirect `beads-viewer.gardenio.app` or move the audit agent onto the
   unified host first ([ka-0000004107]) — otherwise its captures render unstyled
   under `BASE_PATH=/beads`.
2. 48h gardenio-rwx soak with the edge-auth e2e suite green + Mezmo dashboard
   reviewed (HARD GATE, runbook [ka-0000000026]).

After Step A: Slack/viewer default URLs are unified; `beads-viewer.app` and
`coopmux.gardenio.app` 301 to `gardenio.app/{beads,mux}`. Daemon + Jira unaffected.

### Step B — daemon-host deprecation (remove "kd" from exclude)
Prereq: migrate CI/automation off the daemon token to the edge-auth shim
(`cli.gardenio.example.com/kd` is live and PAT-gated — `cli.gardenio.example.com/kd/v1/health`=401; the old 909 shim `cli.gardenio.e2e.example.com` is dead, ka-0000002359).
- Provision a gardenio-group GitLab PAT as a masked/protected CI variable.
- Repoint `.gitlab-ci.yml:1033` (`BEADS_HTTP_URL`), `.rwx/coverage-ratchet.yml:65,72`,
  `.rwx/dictation-corpus-nightly.yml:35,41` from `https://beads.gardenio.e2e.example.com`
  to `https://cli.gardenio.example.com/kd`, and swap `BEADS_AUTH_TOKEN`
  for the PAT.
- Then drop `"kd"` from `legacyRedirects.exclude`.

### Step C — jira-host deprecation (remove "jira" from exclude)
Prereq: repoint Jira's outbound webhook (acme.atlassian.net admin) to
`https://gardenio.e2e.example.com/jira/...`, confirm delivery, then drop
`"jira"` from `legacyRedirects.exclude`.

### Step D — endpoint deletion (irreversible)
After ≥1 release cycle of clean 301 burn-in, delete the per-app AVA verified-access
endpoints one at a time (out-of-repo terraform; `LoadBalancerArn` is immutable —
plan learning #2), then prune the per-app `ingress.host` blocks from deploy-values.

## gb login

`gb login` is fixed under [ka-0000005013] / MR !3191 (cli-exchange carve-out +
`--cli-exchange-url`). Independent of this cutover; can land in either order.

[ka-0000006723]: https://gardenio.e2e.example.com/beads/ka-0000006723
[ka-0000003384]: https://gardenio.e2e.example.com/beads/ka-0000003384
[ka-0000000026]: https://gardenio.e2e.example.com/beads/ka-0000000026
[ka-0000005013]: https://gardenio.e2e.example.com/beads/ka-0000005013
[ka-0000004107]: https://gardenio.e2e.example.com/beads/ka-0000004107
