# Deploy inventory: what gardenio actually ships and runs

> **Status:** snapshot, generated 2026-10-28 from `main` @ `cef62beb`.
> **Task:** gd-YzJ9Jj4gmR (phase 0 *Ground truth & rubric* gd-agyF0escIN of plan
> gd-eRFLdnAEeB, *audit and shrink gardenio*). The phase 2 audits and the deletion
> steps start from this table. It is a snapshot, not a living doc: regenerate it
> (see [Method](#method)) rather than hand-editing rows.

Every directory under `gardenio/controller/cmd` (50) and every Go package under
`gardenio/controller/internal` (112) has a row below, plus kbeads and coop. Each row
says which image builds it, which helm template runs it, and whether
`deploy-values/gardenio-k3s.yaml` (the only deploy target) turns it on.

## Headline

- **8 of 50 `cmd/` binaries run in prod.** These are `controller`, `slack-bridge`, `grafana-bridge`,
  `trigger-engine`, `landgate`, `beads-viewer` and `gb`, plus `k3s-audit-shipper`, which is applied by
  the operator outside the chart. kbeads (`kd`) and coop (`coop`, `coopmux`) also ship.
- **17 binaries have an image and a chart template but are off in prod.** CI no longer builds
  their images: `.github/images.json` marks them `"enabled": false` (gd-OQSDvm7cHz, 2026-10-15).
  The task's premise that "images.yml builds them all on every release" is out of date.
  **One cost remains:** `_controller-base` still compiles all 17 of them (its `for bin in` list)
  on every run that builds any controller image, so their code is still on the build path.
- **25 binaries have no image at all.** They are e2e probes, CI coverage tools and one-shot
  tools. Of the probes, only the ones a `HERMETIC_MODULES` e2e module uses ever run, and only in
  the optional `e2e.yml` (label, nightly or dispatch). The cluster-suite probes never run, because
  CI deploys no cluster.
- **internal/: 93 of 112 packages are reachable from a prod binary.** 6 are reachable only from built-off binaries, 7 only from tools or probes, and 6 from no binary at all (test helpers and generators).
- **Lines** (non-test / `_test.go`, `wc -l`): cmd/ 277.5k / 349.2k, internal/ 271.6k / 374.4k.
  About 27.1k source and 28.9k test lines are reachable only from built-off binaries
  (their `cmd/` dirs plus their exclusive internal packages). Tools and probes add about 18.9k more source lines, and packages
  reached by no binary add 3.3k. Most of the code is in the prod binaries: `gb` (97.3k), `beads-viewer` (111.8k) and `controller` (24.6k) in `cmd/` alone. So shrinking the code means cutting inside prod packages, not only deleting whole binaries.
- **Reachable is not the same as used.** A package counted as PROD here is linked into a prod
  binary. It can still be dead code inside it, or sit behind a default-off knob that k3s never sets.
  [Default-off knobs](#default-off-knobs) lists those knobs, and the CI reachability data
  (gd-Nd2ciHTvTE) answers the function-level question.

## Status key

| Status | Meaning |
|---|---|
| PROD | Rendered by `helm template -f deploy-values/gardenio-k3s.yaml`, or shipped inside an image that is |
| PROD (out of chart) | Deployed by an operator-applied manifest outside the gardenio chart |
| BUILT-OFF | Has an image and a chart template, but `images.json` gates the image off and k3s leaves the chart flag off |
| E2E-ONLY (hermetic) | No image; a test driver used by a `HERMETIC_MODULES` module of `run-shard.sh`, which runs only in optional `e2e.yml` |
| E2E-ONLY (cluster) | No image; a test driver used only by cluster e2e modules, which no CI job runs today |
| TOOL | No image and no CI caller: a one-shot, manual or RWX-era tool |

## What k3s actually runs

Workloads from `helm template gardenio gardenio/helm/gardenio -f deploy-values/gardenio-k3s.yaml`
(254 objects). Images we build are in **bold**.

| Kind | Name | Images |
|---|---|---|
| Deployment | `gardenio-alertmanager` | `docker.io/prom/alertmanager:v0.28.1` |
| Deployment | `gardenio-beads-viewer` | **gardenio/beads-viewer** |
| Deployment | `gardenio-beads` | **gardenio/kbeads** |
| Deployment | `gardenio-agents` | `docker.io/natsio/nats-box:0.14.5`, **gardenio/controller** |
| Deployment | `gardenio-coopmux` | **gardenio/coopmux** |
| Deployment | `gardenio-forwardauth` | `quay.io/oauth2-proxy/oauth2-proxy:v7.15.2` |
| Deployment | `gardenio-forwardauth-redis` | `docker.io/library/redis:7.4-alpine` |
| Deployment | `gardenio-grafana-bridge` | **gardenio/grafana-bridge** |
| Deployment | `gardenio-grafana` | `grafana/grafana:11.4.0`, `ghcr.io/kiwigrid/k8s-sidecar:1.28.0`, `nginxinc/nginx-unprivileged:1.27-alpine` |
| Deployment | `gardenio-landgate-scopetest` | **gardenio/landgate** |
| Deployment | `gardenio-litellm` | `docker.io/pgvector/pgvector:pg17`, `ghcr.io/berriai/litellm:v1.95.0` |
| Deployment | `gardenio-minio` | `docker.io/pgsty/minio:RELEASE.2026-08-04T00-00-00Z` |
| Deployment | `gardenio-pod-state-metrics` | `registry.k8s.io/kube-state-metrics/kube-state-metrics:v2.13.0` |
| Deployment | `gardenio-slack-bridge` | **gardenio/slack-bridge** |
| Deployment | `gardenio-trigger-engine` | **gardenio/trigger-engine** |
| Deployment | `gardenio-vmagent` | `docker.io/victoriametrics/vmagent:v1.108.1` |
| Deployment | `gardenio-vmalert` | `docker.io/victoriametrics/vmalert:v1.108.1` |
| StatefulSet | `gardenio-keycloak-runtime` | `docker.io/pgvector/pgvector:pg17`, `docker.io/library/busybox:1.36`, `quay.io/keycloak/keycloak:26.7.5` |
| StatefulSet | `gardenio-nats` | `docker.io/library/nats:2.10-alpine` |
| StatefulSet | `gardenio-postgres` | `docker.io/pgvector/pgvector:pg17` |
| StatefulSet | `gardenio-victoria-logs` | `docker.io/victoriametrics/victoria-logs:v1.52.0` |
| StatefulSet | `gardenio-vmsingle` | `docker.io/victoriametrics/victoria-metrics:v1.108.1` |
| CronJob | `gardenio-postgres-s3-backup` | `docker.io/pgvector/pgvector:pg17`, `amazon/aws-cli:2.36.48` |
| CronJob | `gardenio-release-deployer` | **gardenio/agent** |
| Job | `gardenio-beads-migrate` | **gardenio/kbeads** |
| Job | `gardenio-keycloak-runtime-first-admin` | `quay.io/keycloak/keycloak:26.7.5` |
| Job | `gardenio-minio-bootstrap` | `docker.io/pgsty/mc:latest` |
| Job | `gardenio-psa-labels` | `docker.io/bitnamilegacy/kubectl:1.33.4` |
| Job | `gardenio-adopt-pvcs` | `docker.io/bitnamilegacy/kubectl:1.33.4` |

Agent pods are not in the render. The controller spawns them from `COOP_IMAGE` = **gardenio/agent**,
which carries `gb`, `kd` and `coop`. Two more deploy paths sit outside the chart: the operator-applied
`gardenio/helm/k3s-audit-shipper/` DaemonSet, and the separate `gardenio/helm/ziti` chart, which runs
`ziti-ctrl-backup` from `templates/controller.yaml`. This pod has no kubectl, so neither one's live
state was checked here.

## Images (`.github/images.json`)

| Image | CI builds on tag/PR | Binaries inside | Used by k3s |
|---|---|---|---|
| kbeads | yes | `kd` (daemon via `kd serve`) | yes |
| controller | yes | `controller` | yes |
| agent | yes | `gb`, `kd`, `coop` | yes |
| coop-claude | yes | `coop` | **no**: nothing references it; the agent image builds its own `coop` |
| coopmux | yes | `coopmux`, `coop-launch` | yes |
| alertmanager-bridge (**deleted 2026-10-31, gd-Y4ZCGr2m2t**) | **off** | `alertmanager-bridge` | **no** |
| beads-viewer | yes | `beads-viewer`, `gb` | yes |
| bridge-log-shipper | **off** | `bridge-log-shipper` | **no** |
| claudemore (**image removed: folded to the e2e test lane 2026-10-31, gd-Y4ZCGr2m2t**) | **off** | `claudemore` | **no** |
| edge-auth | **off** | `edge-auth` | **no** |
| gitlab-bridge | **off** | `gitlab-bridge` | **no** |
| idp-log-shipper | **off** | `idp-log-shipper` | **no** |
| inviteservice (**deleted 2026-10-31, gd-Y4ZCGr2m2t**) | **off** | `inviteservice` | **no** |
| jira-bridge | **off** | `jira-bridge` | **no** |
| landgate | yes | `landgate` | yes |
| push-bridge | **off** | `push-bridge` | **no** |
| roles-server (**deleted 2026-10-31, gd-Y4ZCGr2m2t**) | **off** | `roles-server` | **no** |
| scif-portal (**deleted 2026-10-31, gd-Y4ZCGr2m2t**) | **off** | `scif-portal` | **no** |
| slack-bridge | yes | `slack-bridge` | yes |
| traefik-log-shipper | **off** | `traefik-log-shipper` | **no** |
| trigger-engine | yes | `trigger-engine` | yes |
| wl-bridge (**deleted 2026-10-31, gd-Y4ZCGr2m2t**) | **off** | `wl-bridge` | **no** |
| ava-log-shipper | **off** | `ava-log-shipper` | **no** |
| core-log-shipper | **off** | `core-log-shipper` | **no** |
| k3s-audit-shipper | yes | `k3s-audit-shipper` | out of chart (operator-applied DaemonSet) |
| grafana-bridge | yes | `grafana-bridge` | yes |
| github-bridge | **off** | `github-bridge` | **no** |
| repro-cockpit (**deleted 2026-10-31, gd-Y4ZCGr2m2t**) | **off** | `repro-cockpit` | **no** |
| ziti-ctrl-backup | yes | aws-cli script (no Go) | ziti chart |

Image dirs not in `images.json`, so CI never builds them: `ci-runner`, `geo-table`, `microvm-agent`,
`grafana-assistant-plugin`, `grafana-llm-app-plugin`, `grafana-vl-datasource-plugin`. They have their
own build paths or none; the phase 2 audit should check each one.

## Off in prod (explicit list)

Chart components whose `values.yaml` default is off and that `gardenio-k3s.yaml` leaves off
(unset or `false`):

- **Our binaries, off:** `alertmanager-bridge` (`alertmanagerBridge`, deleted 2026-10-31, gd-Y4ZCGr2m2t), `ava-log-shipper` (`avaLogShipper`), `bridge-log-shipper` (`bridgeLogShipper`), `claudemore` (`claudemore`, chart objects removed: folded to the e2e test lane 2026-10-31, gd-Y4ZCGr2m2t), `core-log-shipper` (`coreLogShipper`), `edge-auth` (`edgeAuth`), `github-bridge` (`githubBridge`), `gitlab-bridge` (`gitlabBridge`), `idp-log-shipper` (`idpLogShipper`), `inviteservice` (`inviteService`, deleted 2026-10-31, gd-Y4ZCGr2m2t), `jira-bridge` (`jiraBridge`), `push-bridge` (`pushBridge`), `repro-cockpit` (`reproCockpit`, deleted 2026-10-31, gd-Y4ZCGr2m2t), `roles-server` (`rolesServer`, deleted 2026-10-31, gd-Y4ZCGr2m2t), `scif-portal` (`scifPortal`, deleted 2026-10-31, gd-Y4ZCGr2m2t), `traefik-log-shipper` (`traefikLogShipper`), `wl-bridge` (`wlBridge`, deleted 2026-10-31, gd-Y4ZCGr2m2t).
  `edgeAuth` is off too; k3s only seeds its `hmacKey.secretKeyRelay` for the Glass relay (gd-4HWvqrxCdH).
- **Other chart components, off:** `cloudwatchExporter`, `edgeContainment`, `edgeShipperLiveness`, `eksApiIntrusionAlerts`, `eksApiIntrusionLogs`, `externalSecrets`, `fleet`, `fsmCompositeAlert`, `gvisor`, `landNamespaces`, `landing`, `mcpGrafana`, `mcpGrafanaWrite`, `mcpVictoriaLogs`, `networkPolicies`, `postgresExternal`, `registrySecrets`, `rwxReaper`, `safeDeployAlerts`, `scannerSurge`, `tokenRotation`, `unifiedHost`, `vmalertLogs`, `vpaRightSizing`, `vtsingle`, `webhooksEdgeCatchall`, `zitiBrowser`.
- **On in k3s, for contrast:** `agents`, `alertmanager`, `beadsViewer`, `coopmux`, `edgeBackends`, `grafana`, `grafanaBridge`, `imdsDeny`, `keycloak`, `landgate`, `limitRange`, `litellm`, `nats`, `objectStore`, `platformPdb`, `podSecurityAdmission`, `postgres`, `projectNamespaceRbac`, `releaseDeployer`, `resourceQuota`, `slackBridge`, `tls`, `traefik`, `triggerEngine`, `vlsingle`, `vmagent`, `vmalert`, `vmsingle`.

## cmd/ binaries

`src`/`test` are `wc -l` of the package's `.go` / `_test.go` files, including subpackages.
`int.` is how many `internal/` packages the binary links, from `go list -deps`. `values` is
`<key>`: chart default → k3s.

| cmd | status | image (CI) | helm template | values (default→k3s) | src | test | int. | e2e users | what it is |
|---|---|---|---|---|---|---|---|---|---|
| `beads-viewer` | PROD | beads-viewer (yes) | `templates/beads-viewer/` | `beadsViewer`: false→true | 111.8k | 162.8k | 47 | agent-cost-context, agents-live-view, grafana-directive +3 | Command beads-viewer is a web UI daemon for viewing beads |
| `controller` | PROD | controller (yes) | `templates/controller/` | `agents`: false→true | 24.6k | 36.2k | 62 | cred-rotation-resume, credential-rotation, gb-stop-suspend +5 | Per-agent scoped beads daemon tokens |
| `gb` | PROD | agent, beads-viewer (yes) | — | — | 97.3k | 116.3k | 59 | advice-lint-undeliverable, claudemore-agentchoice, claudemore-hooks +22 | Package main — `gb gardener` subcommand: ergonomic CLI over the gardener-loop primitive (role=gardener) |
| `grafana-bridge` | PROD | grafana-bridge (yes) | `templates/grafana-bridge/` | `grafanaBridge`: false→true | 632 | 1.1k | 27 | — | Command grafana-bridge is a standalone service that syncs Grafana dashboards authored as beads (type=dashboar… |
| `landgate` | PROD | landgate (yes) | `templates/landgate/` | `landgate`: false→true | 522 | 1.1k | 2 | — | Command landgate is one land's second wall (Lands boundary gd-uYNbyW4a9Z §6 step 11, Design B v0): a transpar… |
| `slack-bridge` | PROD | slack-bridge (yes) | `templates/slack-bridge/` | `slackBridge`: false→true | 2.7k | 2.8k | 28 | slackbridge-respawn-phase-metrics, slackmock-loop | Helper functions extracted from main() to keep the entry-point wiring thin and the branch-carrying logic unit… |
| `trigger-engine` | PROD | trigger-engine (yes) | `templates/trigger-engine/` | `triggerEngine`: false→true | 1.8k | 2.1k | 36 | — | Command trigger-engine is a standalone service that evaluates webhook-based trigger beads and dispatches acti… |
| `k3s-audit-shipper` | PROD (out of chart) | k3s-audit-shipper (yes) | `helm/k3s-audit-shipper/ (raw)` | — | 221 | 64 | 4 | — | Command k3s-audit-shipper tails the k3s kube-apiserver audit log on a control-plane node and ships it, redact… |
| `alertmanager-bridge` (**deleted 2026-10-31, gd-Y4ZCGr2m2t**) | BUILT-OFF | alertmanager-bridge (off (images.json)) | `templates/alertmanager-bridge/` | `alertmanagerBridge`: false→unset | 362 | 315 | 27 | — | Command alertmanager-bridge is a standalone service that bridges beads and alerting |
| `ava-log-shipper` | BUILT-OFF | ava-log-shipper (off (images.json)) | `templates/ava-log-shipper/` | `avaLogShipper`: false→false | 233 | 182 | 3 | — | Command ava-log-shipper is the edge-IDP apex transport: it PULLS AWS Verified Access (AVA) OCSF access record… |
| `bridge-log-shipper` | BUILT-OFF | bridge-log-shipper (off (images.json)) | `templates/bridge-log-shipper/` | `bridgeLogShipper`: false→unset | 212 | 65 | 4 | — | Command bridge-log-shipper is the non-AVA webhook-audit transport (GAP-B of the webhook carve-out, task ka-00… |
| `claudemore` (**folded to the e2e test lane 2026-10-31, gd-Y4ZCGr2m2t**: no image, no chart; built from source by the e2e replay modules) | E2E-ONLY (hermetic) | — | — | — | 1.2k | 1.7k | 2 | claudemore-agentchoice, claudemore-hooks, claudemore-replay-fail-on-miss +1 | claudemore is a thin Anthropic Messages-API recording reverse-proxy that FRONTS (never replaces) litellm |
| `core-log-shipper` | BUILT-OFF | core-log-shipper (off (images.json)) | `templates/core-log-shipper/` | `coreLogShipper`: false→unset | 242 | 126 | 5 | — | Command core-log-shipper is the general gardenio-pod slog transport (design ka-0000005579 option A step 3, ta… |
| `edge-auth` | BUILT-OFF | edge-auth (off (images.json)) | `templates/edge-auth/` | `edgeAuth`: false→false (relay key only) | 2.5k | 5.6k | 3 | edge-auth-box-auth | Command edge-auth is the gardenio-edge-auth service: a non-AVA edge that fronts in-cluster daemons (kbeads, c… |
| `github-bridge` | BUILT-OFF | github-bridge (off (images.json)) | `templates/github-bridge/` | `githubBridge`: false→unset | 3.0k | 3.0k | 5 | — | Command github-bridge is the GitHub counterpart of gitlab-bridge |
| `gitlab-bridge` | BUILT-OFF | gitlab-bridge (off (images.json)) | `templates/gitlab-bridge/` | `gitlabBridge`: false→unset | 1.4k | 1.5k | 27 | — | Command gitlab-bridge is a standalone service that watches GitLab for merge request merges and updates bead f… |
| `idp-log-shipper` | BUILT-OFF | idp-log-shipper (off (images.json)) | `templates/idp-log-shipper/` | `idpLogShipper`: false→false | 217 | 129 | 2 | idp-shipper-asn-gate | Command idp-log-shipper is the IDP Phase-2 CloudWatch->VictoriaLogs transport |
| `inviteservice` (**deleted 2026-10-31, gd-Y4ZCGr2m2t**) | BUILT-OFF | inviteservice (off (images.json)) | `templates/inviteservice/` | `inviteService`: false→unset | 274 | 165 | 6 | — | Command inviteservice serves the front-door invite mint/redeem/revoke/list HTTP surface (internal/inviteservi… |
| `jira-bridge` | BUILT-OFF | jira-bridge (off (images.json)) | `templates/jira-bridge/` | `jiraBridge`: false→unset | 1.1k | 985 | 27 | jira-conversational | Command jira-bridge is a standalone service that polls JIRA for new issues, creates task beads in the beads d… |
| `push-bridge` | BUILT-OFF | push-bridge (off (images.json)) | `templates/push-bridge/` | `pushBridge`: false→unset | 250 | 382 | 27 | — | push-bridge is a standalone bead → APNs notification bridge |
| `repro-cockpit` (**deleted 2026-10-31, gd-Y4ZCGr2m2t**) | BUILT-OFF | repro-cockpit (off (images.json)) | `templates/repro-cockpit/` | `reproCockpit`: false→unset | 5.6k | 3.6k | 4 | repro-cockpit | allowlist.go implements the repro-cockpit's fail-closed deployment-host allowlist — defense layer 2 for the D… |
| `roles-server` (**deleted 2026-10-31, gd-Y4ZCGr2m2t**) | BUILT-OFF | roles-server (off (images.json)) | `templates/roles-server/` | `rolesServer`: false→unset | 2.1k | 1.3k | 3 | — | Command roles-server is a read-only HTTP API for viewing agent roles and their configuration (config beads, a… |
| `scif-portal` (**deleted 2026-10-31, gd-Y4ZCGr2m2t**) | BUILT-OFF | scif-portal (off (images.json)) | `templates/scif-portal/` | `scifPortal`: false→false | 3.8k | 5.1k | 5 | — | handlers_form.go — the browser form transport for BOTH gates (ka-0000007989) |
| `traefik-log-shipper` | BUILT-OFF | traefik-log-shipper (off (images.json)) | `templates/traefik-log-shipper/` | `traefikLogShipper`: false→unset | 241 | 138 | 3 | — | Command traefik-log-shipper is the edge-IDP L0 transport: it streams the Traefik edge pod's JSON access log o… |
| `wl-bridge` (**deleted 2026-10-31, gd-Y4ZCGr2m2t**) | BUILT-OFF | wl-bridge (off (images.json)) | `templates/wl-bridge/` | `wlBridge`: false→unset | 312 | 440 | 27 | — | Command wl-bridge is a standalone service that polls the Wasteland (steveyegge/wl-commons on DoltHub) for wan… |
| `agent-feedback-probe` | E2E-ONLY (cluster) | — | — | — | 607 | 0 | 26 | agent-feedback-writepath | Command agent-feedback-probe is a TEST-ONLY binary that drives the REAL always-on agent-feedback write path (… |
| `escalation-reap-probe` | E2E-ONLY (hermetic) | — | — | — | 1.3k | 0 | 26 | escalation-auto-reaper | Command escalation-reap-probe is a TEST-ONLY binary that drives the REAL escalation auto-reaper (internal/bri… |
| `freeze-interrupt-probe` | E2E-ONLY (cluster) | — | — | — | 479 | 0 | 3 | gvisor-freeze-interrupt | Command freeze-interrupt-probe is a TEST-ONLY driver for the interruptible- freeze E2E module (test-gvisor-fr… |
| `ftrestart-probe` | E2E-ONLY (cluster) | — | — | — | 504 | 0 | 3 | gvisor-controller-restart | Command ftrestart-probe is a TEST-ONLY driver for the gvisor-controller-restart E2E module (test-gvisor-contr… |
| `gitlab-pipeline-minduration-probe` | E2E-ONLY (cluster) | — | — | — | 781 | 0 | 26 | gitlab-pipeline-minduration | Command gitlab-pipeline-minduration-probe is a TEST-ONLY binary that drives the REAL gitlab-pipeline trigger … |
| `gitlab-pipeline-trigger-probe` | E2E-ONLY (cluster) | — | — | — | 673 | 0 | 26 | gitlab-pipeline-refpattern | Command gitlab-pipeline-trigger-probe is a TEST-ONLY binary that drives the REAL gitlab-pipeline trigger sour… |
| `gitlab-prov-probe` | E2E-ONLY (hermetic) | — | — | — | 1.3k | 0 | 26 | gitlab-dark-provenance, grafana-bridge, mr-reopen-drift | Command gitlab-prov-probe is a TEST-ONLY driver that exercises the gardenio E-PROV forward-provenance stack e… |
| `grafana-bridge-probe` | E2E-ONLY (hermetic) | — | — | — | 884 | 0 | 26 | grafana-bridge | Command grafana-bridge-probe is a TEST-ONLY driver that exercises the gardenio grafana-bridge dashboard-sync … |
| `jira-conv-probe` | E2E-ONLY (hermetic) | — | — | — | 2.6k | 0 | 26 | gitlab-dark-provenance, grafana-bridge, jira-conversational +1 | Command jira-conv-probe is a TEST-ONLY driver that exercises the jira-bridge CONVERSATIONAL round-trip end-to… |
| `jira-syncback-probe` | E2E-ONLY (hermetic) | — | — | — | 563 | 0 | 26 | jira-syncback-denybydefault | Command jira-syncback-probe is a TEST-ONLY driver that exercises the jira-bridge SYNC-BACK write path (JiraSy… |
| `mr-reopen-probe` | E2E-ONLY (cluster) | — | — | — | 551 | 0 | 26 | mr-reopen-drift | Command mr-reopen-probe drives the REAL gitlab-bridge MR-reopen drift sweep (internal/bridge/gitlab_mr_reopen… |
| `renudge-thaw-probe` | E2E-ONLY (cluster) | — | — | — | 393 | 0 | 3 | gvisor-renudge-thaw | Command renudge-thaw-probe is a TEST-ONLY driver for the gvisor-renudge-thaw E2E module (test-gvisor-renudge-… |
| `thawepoch-probe` | E2E-ONLY (cluster) | — | — | — | 237 | 0 | 1 | gvisor-freeze-interrupt, gvisor-rehydrate-ack, gvisor-resume-into-pool | Command thawepoch-probe is a TEST-ONLY driver for the gvisor-rehydrate-ack E2E module (test-gvisor-rehydrate-… |
| `trigger-probe` | E2E-ONLY (hermetic) | — | — | — | 420 | 0 | 26 | jira-conversational, trigger-action-fidelity, trigger-cooldown-event-race +1 | Command trigger-probe is a TEST-ONLY driver that fires a single non-schedule trigger source against an in-tes… |
| `coverage-baselines` | TOOL (CI coverage, RWX-era) | — | — | — | 147 | 141 | 3 | — | coverage-baselines is a one-shot tool that computes per-component coverage baselines from a window of merged … |
| `coverage-bot` (**deleted 2026-11-03, gd-nfGsFuy8Xb**) | TOOL (CI coverage, RWX-era) | — | — | — | 1.2k | 673 | 7 | — | coverage-bot is the W2b-3 sticky PR-comment bot |
| `coverage-patch` | TOOL (CI coverage, RWX-era) | — | — | — | 383 | 227 | 7 | — | coverage-patch is the W2b-2 command-line entry point: it consumes the merged LCOV from W3 (2a), a unified-dif… |
| `edge-idp-demo` | TOOL / orphan | — | — | — | 126 | 0 | 1 | — | Command edge-idp-demo exercises the offline edge intrusion-detection engine (gardenio/controller/internal/edg… |
| `mr-state-backfill` | TOOL / orphan | — | — | — | 225 | 0 | 26 | — | Command mr-state-backfill reconciles mr_state / mr_outcome / mr_merged on type=merge-request beads against ca… |
| `scrub-coop-token` | TOOL / orphan | — | — | — | 196 | 301 | 2 | — | Command scrub-coop-token is a one-shot backfill that removes any "coop_token: <value>" lines from existing ag… |
| `spendlogs-probe` | TOOL / orphan | — | — | — | 153 | 0 | 3 | — | Command spendlogs-probe runs the stateless per-agent cost projection (internal/spendlogs) against live LiteLL… |
| `stub-mcp-server` | TOOL / orphan | — | — | — | 57 | 0 | 1 | — | Command stub-mcp-server is a deterministic, hermetic stdio MCP server used as the connect+invoke target for t… |

Notes:
- `gb` is in two images: agent, which every agent pod and the `release-deployer` CronJob run, and
  beads-viewer, which runs it as a subprocess for the prime view.
- `landgate` runs one Deployment per land. k3s renders `gardenio-landgate-scopetest`.
- `coverage-baselines`, `coverage-bot` and `coverage-patch` were called only by the RWX pipelines
  archived under `docs/archive/rwx/` (RWX was deleted 2026-09-16). No `.github/workflows` job calls them.
- `coverage-bot`, with gb's `coverage-ratchet`, `coverage-baseline` and `coverage-bypass`
  commands and `internal/coverage/{regression,pin,bypass/followup}`, was deleted on 2026-11-03
  (gd-nfGsFuy8Xb): nothing ran them. `coverage-patch` now runs in `ci.yml` (gd-roE5roXBKQ).
- `mr-state-backfill` and `scrub-coop-token` are one-shot remediations. `spendlogs-probe` and
  `edge-idp-demo` are manual. `stub-mcp-server` has no caller outside its own tests and docs.
- `audit-enrich`, `microvm-image-register` and `peel-broker` (with `internal/peelbroker`) were
  deleted on 2026-11-02 (gd-jOl9itDY0D): nothing built, shipped or ran them.

## internal/ packages

`prod users` are the prod binaries that link the package. `other users` are built-off binaries,
tools and probes. A package whose only user is `gb` still ships, in every agent pod.

| package | status | src | test | prod users | other users | what it is |
|---|---|---|---|---|---|---|
| `advice` | PROD | 2.8k | 3.3k | beads-viewer, controller, gb, grafana-bridge, slack-bridge, trigger-engine | alertmanager-bridge, gitlab-bridge, jira-bridge, push-bridge, wl-bridge, agent-feedback-probe +10 | Package advice provides subscription matching and advice listing for the beads advice sys… |
| `assistantturnminter` | PROD | 1.1k | 1.2k | controller | — | Package assistantturnminter is the always-on, watcher-independent WRITE side of the /plan… |
| `badge` | PROD | 1.3k | 1.7k | gb | — | Raster output for plan badges |
| `beadsapi` | PROD | 13.8k | 18.4k | beads-viewer, controller, gb, grafana-bridge, slack-bridge, trigger-engine | alertmanager-bridge, github-bridge, gitlab-bridge, inviteservice, jira-bridge, push-bridge +18 | Package beadsapi queries the beads daemon for bead state via HTTP/JSON |
| `bridge` | PROD | 127.4k | 203.7k | beads-viewer, controller, gb, grafana-bridge, slack-bridge, trigger-engine | alertmanager-bridge, gitlab-bridge, jira-bridge, push-bridge, wl-bridge, agent-feedback-probe +10 | Package bridge provides the advice update nudge watcher |
| `bridgetokenrotator` | PROD | 626 | 576 | controller | — | Prometheus metrics for the bridge fleet-token rotator (task ka-0000002353, plan item 3 — … |
| `buildinfo` | PROD | 173 | 77 | gb | — | Package buildinfo exposes the build-time version/commit/branch/date of the gardenio binar… |
| `claudeconfig` | PROD | 1.3k | 321 | beads-viewer, gb | — | Package claudeconfig is the shared source of truth for resolving and merging gardenio "co… |
| `claudemore` | PROD | 1.6k | 2.1k | beads-viewer, k3s-audit-shipper | claudemore, core-log-shipper | Package claudemore holds the record/replay substrate for the claudemore proxy (epic ka-00… |
| `codexauth` | PROD | 872 | 503 | controller, gb | — | Package codexauth models the Codex CLI's ChatGPT-subscription auth.json and the two upstr… |
| `commons` | PROD | 7.9k | 5.8k | beads-viewer, controller, gb, trigger-engine | — | Package commons is the Commons read model: the channels, timelines, threads and messages … |
| `config` | PROD | 2.9k | 947 | controller | — | Package config provides controller configuration from environment variables |
| `contextwindow` | PROD | 215 | 104 | beads-viewer, controller, gb, grafana-bridge, slack-bridge, trigger-engine | alertmanager-bridge, gitlab-bridge, jira-bridge, push-bridge, wl-bridge, agent-feedback-probe +10 | Package contextwindow centralizes the context-window accounting used to turn an agent's r… |
| `conversation` | PROD | 1.7k | 1.3k | beads-viewer, controller, gb, grafana-bridge, slack-bridge, trigger-engine | alertmanager-bridge, gitlab-bridge, jira-bridge, push-bridge, wl-bridge, agent-feedback-probe +10 | Package conversation is the seam between the bridge's handlers and the places a conversat… |
| `coopapi` | PROD | 431 | 849 | gb | — | Package coopapi provides an HTTP client for the coop REST API |
| `coopmux` | PROD | 305 | 734 | controller | — | Package coopmux provides an HTTP client for the coopmux credential broker API |
| `coopmuxapi` | PROD | 936 | 881 | beads-viewer, controller, gb, trigger-engine | — | Package coopmuxapi is a thin HTTP client to the coopmux mux service |
| `coverage/bypass` | PROD | 414 | 585 | gb (its use deleted 2026-11-03, gd-nfGsFuy8Xb) | coverage-bot, coverage-patch | Package bypass parses Coverage-Bypass trailers from MR commit messages and emits a Bypass… |
| `coverage/bypass/followup` (**deleted 2026-11-03, gd-nfGsFuy8Xb**) | PROD | 366 | 357 | gb | — | Package followup files §8.3 follow-up `coverage-bypass` bug beads from the patch-cov gate… |
| `coverage/components` | PROD | 50 | 38 | gb (its use deleted 2026-11-03, gd-nfGsFuy8Xb) | coverage-baselines, coverage-bot, coverage-patch | Package components maps repo-root-relative paths to the component (gated unit) that owns … |
| `coverage/lcov` | PROD | 103 | 69 | gb (its use deleted 2026-11-03, gd-nfGsFuy8Xb) | coverage-baselines, coverage-bot, coverage-patch | Package lcov is a minimal LCOV-tracefile parser, scoped to the records the patch-cov libr… |
| `coverage/pin` (**deleted 2026-11-03, gd-nfGsFuy8Xb**) | PROD | 283 | 498 | gb | — | Package pin resolves which coverage-baseline bead a consumer (CLI, viewer, ratchet wrappe… |
| `coverage/regression` (**deleted 2026-11-03, gd-nfGsFuy8Xb**) | PROD | 228 | 284 | gb | — | Package regression files coverage-regression bug beads from the per-component ratchet job… |
| `covflush` | PROD | 53 | 109 | beads-viewer, controller, grafana-bridge, landgate, slack-bridge, trigger-engine | alertmanager-bridge, claudemore, edge-auth, github-bridge, gitlab-bridge, inviteservice +4 | Package covflush is the runtime coverage flusher used by every gardenio long-running Go b… |
| `deathreport` | PROD | 1.4k | 1.0k | beads-viewer, controller, gb, grafana-bridge, slack-bridge, trigger-engine | alertmanager-bridge, gitlab-bridge, jira-bridge, push-bridge, wl-bridge, agent-feedback-probe +10 | Package deathreport builds a "why did this agent die" forensic report from an involuntari… |
| `deathtranscript` | PROD | 901 | 798 | beads-viewer, controller, gb, grafana-bridge, slack-bridge, trigger-engine | alertmanager-bridge, gitlab-bridge, jira-bridge, push-bridge, wl-bridge, agent-feedback-probe +10 | Package deathtranscript is the mapping layer of the auto-forensics pipeline (epic ka-0000… |
| `decisionview` | PROD | 219 | 224 | beads-viewer, controller, gb, trigger-engine | — | Package decisionview is the one read model of a decision bead for display: what every sur… |
| `forgebeads` | PROD | 64 | 73 | beads-viewer, controller, gb, grafana-bridge, slack-bridge, trigger-engine | alertmanager-bridge, github-bridge, gitlab-bridge, jira-bridge, push-bridge, wl-bridge +11 | Package forgebeads holds the bead-graph writes that gitlab-bridge's sync paths and github… |
| `freehtmx` | PROD | 385 | 662 | beads-viewer | — (repro-cockpit deleted 2026-10-31, gd-Y4ZCGr2m2t) | Package freehtmx implements the sanitizer + CSP plumbing for the free-form htmx widget su… |
| `gardenermode` | PROD | 436 | 467 | beads-viewer, controller, gb, trigger-engine | — | Package gardenermode owns the timed⇄event firing-mode flip for gardener trigger beads, pl… |
| `gardenhub` | PROD | 14.4k | 23.2k | beads-viewer | — | Package gardenhub is the backend for the /garden page: an in-memory snapshot read model o… |
| `githubapp` | PROD | 1.1k | 1.0k | beads-viewer, controller | github-bridge | Package githubapp mints short-lived, repo-scoped GitHub App installation tokens for agent… |
| `glass` | PROD | 1.9k | 1.2k | beads-viewer | — | Package glass holds the server side of the Lands Glass: the relay that lets a browser wat… |
| `grafanaembed` | PROD | 519 | 657 | beads-viewer | — | Package grafanaembed builds the sandboxed Grafana iframe every gardenio surface embeds a … |
| `harness` | PROD | 237 | 172 | gb | — | Package harness is the single source of truth for how a pod launches its agent CLI per ha… |
| `healthsnapshot` | PROD | 437 | 276 | controller | — | Package healthsnapshot collects controller-side health signals (pod counts by phase, rece… |
| `heraldry` | PROD | 2.0k | 729 | beads-viewer, controller, gb, grafana-bridge, slack-bridge, trigger-engine | alertmanager-bridge, gitlab-bridge, jira-bridge, push-bridge, wl-bridge, agent-feedback-probe +10 | Package heraldry is the one place a bead's arms become pixels (heraldry spec gardenio/doc… |
| `jqout` | PROD | 124 | 152 | gb | — | Package jqout applies an embedded jq filter (gojq) to a value about to be emitted as CLI … |
| `k3sauditshipper` | PROD | 1.0k | 641 | k3s-audit-shipper | — | Package k3sauditshipper ships the k3s kube-apiserver audit log to the in-chart VictoriaLo… |
| `keycloakadmin` | PROD | 1.0k | 1.3k | controller, gb | — | Package keycloakadmin is the Lands provisioner's Keycloak admin client (Lands Track B spe… |
| `lands/freeze` | PROD | 173 | 170 | beads-viewer, gb | — | Package freeze is the per-land kill switch of Lands Track B (spec gardenio/docs/superpowe… |
| `lands/landns` | PROD | 533 | 305 | controller, gb | — | Package landns is a tenant land's Kubernetes namespace (Lands Track B spec gardenio/docs/… |
| `lands/landscope` | PROD | 180 | 147 | controller | — | Package landscope is a land's per-land Keycloak audience (owner decision gd-zf6eceweED, t… |
| `lands/provision` | PROD | 2.0k | 2.1k | controller, gb | — | Package provision is the Lands provisioner (Lands Track B spec gardenio/docs/superpowers/… |
| `lands/purge` | PROD | 549 | 539 | controller | — | Package purge is the Lands purge sweep (Lands Track B spec §3.3, step 8.11): it deletes w… |
| `lands/purse` | PROD | 681 | 415 | beads-viewer, controller, gb, grafana-bridge, slack-bridge, trigger-engine | alertmanager-bridge, gitlab-bridge, jira-bridge, push-bridge, wl-bridge, agent-feedback-probe +10 | Package purse computes a tenant land's purse (Lands Track B spec gardenio/docs/superpower… |
| `lands/wall` | PROD | 380 | 507 | beads-viewer, controller, gb, grafana-bridge, slack-bridge, trigger-engine | alertmanager-bridge, gitlab-bridge, jira-bridge, push-bridge, wl-bridge, agent-feedback-probe +10 | Package wall is the Lands spawn wall (Lands Track B spec gardenio/docs/superpowers/specs/… |
| `lands/watcher` | PROD | 813 | 765 | controller, gb, trigger-engine | — | Package watcher is the Lands purse watcher (Lands Track B spec gardenio/docs/superpowers/… |
| `localreg` | PROD | 262 | 233 | gb | — | Package localreg tracks locally-spawned `gb agent start --local` sessions via a small on-… |
| `mailer` | PROD | 916 | 512 | beads-viewer, controller, gb, trigger-engine | — | Package mailer is the one transactional mailer for Lands (Track B decision B-D10, gd-ESQO… |
| `metrics` | PROD | 3.7k | 3.8k | beads-viewer, controller, gb, grafana-bridge, slack-bridge, trigger-engine | alertmanager-bridge, gitlab-bridge, jira-bridge, push-bridge, wl-bridge, agent-feedback-probe +10 | Package metrics provides Prometheus metrics for the gardenio controller |
| `microvm` | PROD | 3.9k | 4.9k | controller | — | Package microvm holds the controller's cross-account AWS identity for the lambda_microvm … |
| `namegen` | PROD | 217 | 203 | slack-bridge | — | Package namegen generates short, meaningful agent names and bead titles from long prompt … |
| `natsauth` | PROD | 31 | 108 | beads-viewer, controller, gb, grafana-bridge, slack-bridge, trigger-engine | alertmanager-bridge, gitlab-bridge, jira-bridge, push-bridge, wl-bridge, agent-feedback-probe +10 | Package natsauth builds NATS client auth options for the gardenio deployment's two auth m… |
| `nodelifecycle` | PROD | 464 | 640 | controller | — | Package nodelifecycle emits a counter of node-lifecycle transitions (gardenio_node_lifecy… |
| `nscount` | PROD | 200 | 287 | controller | — | Package nscount emits a current-state gauge of ephemeral gardenio-rwx-* namespaces so the… |
| `obspii` | PROD | 207 | 359 | gb | — | Package obspii is the single source of truth for the operator-keyed metric label allow-li… |
| `oidcauth` | PROD | 2.0k | 3.2k | beads-viewer, controller, gb, grafana-bridge, landgate, slack-bridge +1 | alertmanager-bridge, edge-auth, gitlab-bridge, inviteservice, jira-bridge, push-bridge +13 | Package oidcauth provides HTTP middleware for OIDC token validation |
| `pausegate` | PROD | 113 | 141 | beads-viewer, controller, gb, grafana-bridge, slack-bridge, trigger-engine | alertmanager-bridge, gitlab-bridge, jira-bridge, push-bridge, wl-bridge, agent-feedback-probe +10 | Package pausegate is the single source of truth for the fleet-wide pause gates — the gard… |
| `perftracesrelay` | PROD | 590 | 401 | controller | — | Package perftracesrelay implements the PULL/relay transport that copies perf spans into t… |
| `planagent` | PROD | 763 | 732 | beads-viewer, gb | — | Package planagent is the ONE shared engine for a /plans plan's bound agent: resolving the… |
| `plangardener` | PROD | 958 | 468 | beads-viewer, controller, gb, trigger-engine | — | Package plangardener is the ONE gardener-spec builder for /plans plan gardeners, shared b… |
| `plans` | PROD | 1.3k | 536 | beads-viewer, gb | — | Package plans is the plan cockpit's write service: the bodies of the /plans/{id}/ |
| `plantranscript` | PROD | 213 | 145 | beads-viewer, controller | — | Package plantranscript is the ONE shared WRITE contract for /plans chat transcript turns … |
| `podmanager` | PROD | 14.8k | 22.6k | beads-viewer, controller, gb, grafana-bridge, slack-bridge, trigger-engine | alertmanager-bridge, gitlab-bridge, jira-bridge, push-bridge, wl-bridge, agent-feedback-probe +13 | freezethaw.go implements the PATH-B (multi-container / sidecar) gVisor freeze/thaw verbs … |
| `poolmanager` | PROD | 2.9k | 6.4k | controller | — | Warm runsc placeholder ("warm-B") pool — sub-second-resume-integration.md C3 (ka-00000042… |
| `pvcgc` | PROD | 185 | 371 | controller | — | Package pvcgc implements garbage collection for orphaned workspace PVCs |
| `pvcobs` | PROD | 268 | 261 | controller | — | Package pvcobs emits observability gauges for orphaned Pending PersistentVolumeClaims — P… |
| `pvgc` | PROD | 180 | 310 | controller | — | Package pvgc implements garbage collection for orphaned PersistentVolumes |
| `ratelimit` | PROD | 121 | 114 | beads-viewer | — | Package ratelimit implements a per-key token-bucket rate limiter used to gate the htmx wi… |
| `reconciler` | PROD | 2.9k | 6.2k | controller | — | Package reconciler — image digest tracking for automatic pod rolling |
| `respond` | PROD | 373 | 215 | beads-viewer, controller, gb, trigger-engine | — | Package respond is the garden's one way to answer an agent's prompt (Lands workbench-glas… |
| `scheduler` | PROD | 4.4k | 5.8k | beads-viewer, controller, gb, grafana-bridge, slack-bridge, trigger-engine | alertmanager-bridge, gitlab-bridge, jira-bridge, push-bridge, wl-bridge, agent-feedback-probe +10 |  |
| `scheduler/conditions` | PROD | 1.9k | 2.8k | beads-viewer, controller, gb, grafana-bridge, slack-bridge, trigger-engine | alertmanager-bridge, gitlab-bridge, jira-bridge, push-bridge, wl-bridge, agent-feedback-probe +10 | Package conditions defines the shared condition DSL used by the scheduler's stop/exit con… |
| `scheduler/progress` | PROD | 1.2k | 970 | beads-viewer, controller, gb, grafana-bridge, slack-bridge, trigger-engine | alertmanager-bridge, gitlab-bridge, jira-bridge, push-bridge, wl-bridge, agent-feedback-probe +10 | Package progress implements the schedule-bead `progress:` instrumentation described in ga… |
| `scif` (**deleted 2026-10-31, gd-Y4ZCGr2m2t**) | PROD | 1.9k | 1.6k | beads-viewer, controller, gb, grafana-bridge, slack-bridge, trigger-engine | alertmanager-bridge, gitlab-bridge, jira-bridge, push-bridge, scif-portal, wl-bridge +11 | Package scif implements the acme-scif v1.1 approval-portal domain: the two-gate human app… |
| `secretreconciler` | PROD | 522 | 1.1k | controller | — | Package secretreconciler reconciles ExternalSecret CRDs from project bead secrets |
| `shorttitle` | PROD | 178 | 203 | beads-viewer, controller, gb, grafana-bridge, slack-bridge, trigger-engine | alertmanager-bridge, github-bridge, gitlab-bridge, inviteservice, jira-bridge, push-bridge +18 | Package shorttitle derives the short human name a bead is shown under from its type and t… |
| `spawnguard` | PROD | 313 | 332 | beads-viewer, controller, gb, grafana-bridge, slack-bridge, trigger-engine | alertmanager-bridge, gitlab-bridge, jira-bridge, push-bridge, wl-bridge, agent-feedback-probe +10 | Package spawnguard provides a global rate cap and circuit breaker for agent-bead spawns |
| `spendlogs` | PROD | 539 | 420 | controller | spendlogs-probe | Package spendlogs recomputes per-agent LiteLLM cost for an arbitrary time window directly… |
| `statusreporter` | PROD | 3.5k | 8.1k | controller | — | Package statusreporter syncs K8s pod status back to beads via the daemon HTTP API |
| `steward` | PROD | 3.8k | 3.1k | controller, gb, trigger-engine | — | Package steward is the Lands steward: the one agent per tenant land that greets the land'… |
| `subscriber` | PROD | 723 | 1.7k | controller | — | Package subscriber watches for agent lifecycle events and emits them on a channel |
| `thawepoch` | PROD | 634 | 600 | beads-viewer, controller, gb, grafana-bridge, slack-bridge, trigger-engine | alertmanager-bridge, gitlab-bridge, jira-bridge, push-bridge, wl-bridge, agent-feedback-probe +14 | Package thawepoch implements the thaw-epoch signal — Stage 0 of the agent rehydration des… |
| `toollatency` | PROD | 818 | 729 | beads-viewer, controller, gb, grafana-bridge, slack-bridge, trigger-engine | alertmanager-bridge, gitlab-bridge, jira-bridge, push-bridge, wl-bridge, agent-feedback-probe +10 | Package toollatency implements the P2 HOOK_EVENTS consumer (plan gardenio/plans/agent-too… |
| `tracing` | PROD | 179 | 77 | beads-viewer, controller, gb, grafana-bridge, slack-bridge, trigger-engine | alertmanager-bridge, gitlab-bridge, jira-bridge, push-bridge, wl-bridge, agent-feedback-probe +13 | Package tracing wires OpenTelemetry trace export for the gardenio controller and its sibl… |
| `trustmetrics` | PROD | 903 | 550 | beads-viewer, controller, gb, grafana-bridge, slack-bridge, trigger-engine | alertmanager-bridge, gitlab-bridge, jira-bridge, push-bridge, wl-bridge, agent-feedback-probe +10 | Package trustmetrics lands the R5 "Trusted Throughput" success-metrics pipeline (bead ka-… |
| `tui/decision` | PROD | 1.0k | 1.2k | gb | — | Package decision provides a Bubbletea TUI for monitoring and responding to decision points |
| `tunes` | PROD | 36 | 84 | beads-viewer | — | Package tunes is the teensy music player on the Homestead (/garden) HUD and the Lands fro… |
| `vlogs` | PROD | 339 | 228 | gb | — | Package vlogs is the READ-side counterpart to the pod-log shippers (internal/bridgeshippe… |
| `vlsink` | PROD | 60 | 63 | k3s-audit-shipper | bridge-log-shipper, core-log-shipper, traefik-log-shipper | Package vlsink holds the VictoriaLogs write sink every in-cluster log shipper feeds: HTTPSi… |
| `workflowhost` | PROD | 217 | 154 | gb | — | Package workflowhost holds the re-entrancy logic for a formula step that hosts a Claude C… |
| `auditxform` | BUILT-OFF only | 387 | 224 | — | bridge-log-shipper, core-log-shipper, traefik-log-shipper | Package auditxform is the edge log-field contract: the field names an L0 edge shipper (in… |
| `avashipper` | BUILT-OFF only | 243 | 334 | — | ava-log-shipper | Package avashipper is the edge-IDP surface adapter for the AWS Verified Access (AVA) OCSF… |
| `bridgeshipper` | BUILT-OFF only | 324 | 262 | — | bridge-log-shipper, core-log-shipper | Package bridgeshipper is the non-AVA webhook-audit transport (GAP-B of the webhook carve-… |
| `edgeauth` | BUILT-OFF only | 810 | 787 | — | edge-auth, inviteservice, scif-portal (deleted 2026-10-31, gd-Y4ZCGr2m2t) | Package edgeauth provides GitLab PAT validation and group-membership checks for the garde… |
| `inviteservice` (**deleted 2026-10-31, gd-Y4ZCGr2m2t**) | BUILT-OFF only | 1.1k | 1.4k | — | inviteservice | Package inviteservice implements the front-door invite flow: an admin mints a single-use,… |
| `scifportal` (**deleted 2026-10-31, gd-Y4ZCGr2m2t**) | BUILT-OFF only | 961 | 786 | — | scif-portal | Package scifportal renders the acme-scif approval portal UI — v1.1 Phase D2 (bead ka-0000… |
| `traefikshipper` | BUILT-OFF only | 646 | 570 | — | bridge-log-shipper, core-log-shipper, traefik-log-shipper | Package traefikshipper is the edge-IDP L0 transport: it streams the Traefik edge pod's JS… |
| `coverage/baselines` | TOOL/E2E only | 185 | 231 | — | coverage-baselines, coverage-bot, coverage-patch | Package baselines computes per-component coverage baselines from a window of merged LCOV … |
| `coverage/diff` | TOOL/E2E only | 172 | 88 | — | coverage-bot, coverage-patch | Package diff parses unified-diff output (`git diff --unified=N`) into a structure tailore… |
| `coverage/exclusions` | TOOL/E2E only | 123 | 46 | — | coverage-bot, coverage-patch | Package exclusions is the single source of truth for path globs that must be dropped from… |
| `coverage/patch` | TOOL/E2E only | 240 | 291 | — | coverage-bot, coverage-patch | Package patch computes per-component patch coverage from a merged LCOV tracefile, a unifi… |
| `edgebaseline` (**deleted 2026-11-03, gd-ERGt5eaBUH**) | TOOL/E2E only | 2.1k | 1.1k | — | edge-idp-demo | Package edgebaseline is the OFFLINE detection-math substrate for the edge intrusion-detec… |
| `stubmcp` | TOOL/E2E only | 305 | 279 | — | stub-mcp-server | Package stubmcp implements a deterministic, hermetic Model Context Protocol server used a… |
| `commons/commonstest` | NO BINARY (test-only or dead) | 606 | 0 | — | — | Package commonstest is a fake beads daemon for the Commons read model's tests: it holds b… |
| `conversation/surfacetest` | NO BINARY (test-only or dead) | 876 | 0 | — | — | Package surfacetest holds the test doubles for conversation.Surface: a Recorder fake, a t… |
| `golden` | NO BINARY (test-only or dead) | 437 | 135 | — | — | Package golden is the comparison half of the golden-screenshot gates (gd-D953GccBTY): a b… |
| `grafanagrade` | NO BINARY (test-only or dead) | 1.1k | 1.2k | — | — | Package grafanagrade holds the static detectors the Grafana GRADE audit (epic ka-00000080… |
| `heraldry/genassets` | NO BINARY (test-only or dead) | 121 | 0 | — | — | Command genassets vendors the heraldry renderer's artwork and writes its library table |
| `heraldry/kbeadssrc` | NO BINARY (test-only or dead) | 146 | 0 | — | — | Package kbeadssrc reads the heraldry library out of kbeads' Go source (kbeads/internal/he… |

Packages with no binary user: `commons/commonstest` and `conversation/surfacetest` are test
doubles, imported only from `_test.go`. `golden` and `grafanagrade` are gate and audit harnesses
that only tests drive. `heraldry/genassets` is a `go generate` command, and `heraldry/kbeadssrc`
is its helper. None of them ships. The audit question is whether the gates they serve still
run in CI.

## kbeads

kbeads builds one binary, `kd`. It is both the daemon (`kd serve`, image **kbeads**, Deployment
`gardenio-beads` plus Job `gardenio-beads-migrate`) and the CLI (in the agent image). All of its
packages are in `kd`'s dependency graph except two generators:

| package | in `kd` | what it is |
|---|---|---|
| `cmd/kd` | yes | — |
| `gen/beads/v1` | yes | — |
| `internal/buildinfo` | yes | Package buildinfo exposes the build-time version/commit/branch/date of the kbeads binaries (the kd … |
| `internal/client` | yes | Package client provides a transport-agnostic interface for the kbeads service and an HTTP/JSON impl… |
| `internal/config` | yes | — |
| `internal/edgeauth` | yes | Package edgeauth implements the daemon side of the Stage 3 / W-B-11 trust handshake (ka-0000001678)… |
| `internal/embedding` | yes | Package embedding provides text-to-vector embedding primitives used by kbeads' semantic search pipe… |
| `internal/eventbus` | yes | — |
| `internal/events` | yes | — |
| `internal/formula` | yes | Package formula is the canonical implementation of the formula instantiation pipeline (resolve → ex… |
| `internal/heraldry` | yes | Package heraldry is a bead's arms: a run of three to five emoji ("charges") in order, 🔥🧭📦 (spec gar… |
| `internal/heraldry/genlibrary` | **no** (generator) | Command genlibrary generates the heraldry library: library.go and theme_exclusions.go in kbeads/int… |
| `internal/heraldry/themeglyphs` | **no** (generator) | Package themeglyphs reads the glyphs the bridge's Garden and Business themes use straight out of th… |
| `internal/hooks` | yes | Package hooks provides advice hook execution for session lifecycle events |
| `internal/idgen` | yes | Package idgen provides short, URL-safe unique ID generation backed by nanoid |
| `internal/jqout` | yes | Package jqout applies an embedded jq filter (gojq) to a value about to be emitted as CLI JSON, so `… |
| `internal/landmap` | yes | Package landmap is the map: the Lands API (lands/contract, the /lands/v1/ vocabulary gl and the web… |
| `internal/metrics` | yes | Package metrics defines cross-cutting Prometheus metrics shared between the embedding package and t… |
| `internal/model` | yes | — |
| `internal/presence` | yes | Package presence tracks live agent activity for the agent roster |
| `internal/references` | yes | Package references extracts bead-id references (ka-0000001713) from text |
| `internal/server` | yes | Server-side decision-aging / auto-default policy |
| `internal/store` | yes | — |
| `internal/store/postgres` | yes | Package postgres implements the store.Store interface backed by PostgreSQL |
| `internal/sync` | yes | — |
| `internal/tracing` | yes | Package tracing wires OpenTelemetry trace export for the kbeads daemon |
| `internal/ui` | yes | — |

`gardenio/clicreds` (agentname, avareauth, credstore, oidclogin) is linked into `kd` and into
controller binaries (`avareauth` 31 cmds, `agentname` 22, `credstore` and `oidclogin` 1 each).

## coop

| crate | binary | image | runs in k3s |
|---|---|---|---|
| `coop/crates/cli` | `coop` | agent (own cargo build), coop-claude | yes, in every agent pod; coop-claude image unused |
| `coop/crates/mux` | `coopmux`, `coop-launch` | coopmux | yes: Deployment `gardenio-coopmux` |
| `coop/crates/web` (TypeScript) | — | embedded: `dist/terminal.html` into `coop`, `dist/mux.html` into `coopmux` (`include_str!`) | with its hosts |

## Default-off knobs

### Controller env knobs (`internal/config/config.go`)

`config.go` reads **219 distinct env vars** through `os.Getenv` and the `env*Or`
helpers (the task's estimate was about 111). The k3s render sets 53 of them on the
controller; the other 166 run on their code default. The feature switches, all
booleans:

| env | default | k3s | effect in prod |
|---|---|---|---|
| `GARDENIO_AGENT_COVERAGE` | false | unset | **dormant** (off, never set) |
| `GEMINI_HARNESS_ENABLED` | false | unset | **dormant** (off, never set) |
| `CLAUDE_TEAMS_ENABLED` | false | unset | **dormant** (off, never set) |
| `GARDENIO_CHANNEL` | false | unset | **dormant** (off, never set) |
| `PER_AGENT_TOKENS` | false | true | **on** |
| `LANDS_TENANT_SPAWNS` | false | unset | **dormant** (off, never set) |
| `GITHUB_APP_ENABLED` | false | unset | **dormant** (off, never set) |
| `FREEZE_THAW_ENABLED` | false | unset | **dormant** (off, never set) |
| `FREEZE_INTERRUPT_ENABLED` | false | unset | **dormant** (off, never set) |
| `FREEZE_ARMED_GUARD_ENABLED` | false | unset | **dormant** (off, never set) |
| `THAW_RETRY_ONCE` | false | unset | **dormant** (off, never set) |
| `POST_THAW_TOKEN_REFRESH_ENABLED` | false | unset | **dormant** (off, never set) |
| `VPA_MEMORY_STAMP_ENABLED` | false | unset | **dormant** (off, never set) |
| `WARM_PLACEHOLDER_POOL_ENABLED` | false | unset | **dormant** (off, never set) |
| `ZITI_ACCESS_REVOKE_ON_EXIT` | true | unset | default |
| `MICROVM_ZITI_BROKER_ENROLL` | false | unset | **dormant** (off, never set) |
| `MICROVM_PRODUCTION_MODE` | false | unset | **dormant** (off, never set) |
| `MICROVM_IDLE_POLICY_ENABLED` | false | unset | **dormant** (off, never set) |
| `MICROVM_IDLE_AUTO_RESUME_ENABLED` | false | unset | **dormant** (off, never set) |
| `MICROVM_SUSPEND_ENABLED` | false | unset | **dormant** (off, never set) |
| `MICROVM_RESUME_REHYDRATE_ENABLED` | true | unset | default |
| `ACCOUNT_ROTATION_RESTART_ENABLED` | true | true | **on** |
| `STALE_VKEY_RESTART_ENABLED` | true | true | **on** |
| `GARDENIO_CONFIG_RELOAD_RESTART_ENABLED` | false | false | off |
| `SPREAD_SERVED_MODEL_GATE_ENABLED` | true | true | **on** |
| `ENABLE_LEADER_ELECTION` | false | true | **on** |
| `LITELLM_KEY_ROTATION_ENABLED` | false | unset | **dormant** (off, never set) |
| `COOP_NODE_DRAIN_NUDGE` | false | false | off |
| `STUCK_RESTART_IN_PLACE` | true | unset | default |
| `GARDENIO_ADVICE_COMPACTION` | true | unset | default |
| `GARDENIO_SPEND_BACKFILL_HISTORICAL` | false | unset | **dormant** (off, never set) |
| `GARDENIO_SPEND_BACKFILL_DRY_RUN` | true | unset | default |
| `BRIDGE_TOKEN_ROTATION_ENABLED` | false | unset | **dormant** (off, never set) |
| `BRIDGE_TOKEN_ROTATION_DRY_RUN` | true | unset | default |

Knob families never set in k3s. Their code paths run only on code defaults, and a feature
that needs a value (an ARN, a URL, an image) is effectively off:

| prefix | unset | keys |
|---|---|---|
| `MICROVM_` | 29 | `MICROVM_LAUNCHER_ROLE_ARN`, `MICROVM_REGION`, `MICROVM_ASSUME_ROLE_SESSION_NAME`, `MICROVM_SECRET_PREFIX`, `MICROVM_IMAGE_IDENTIFIER`, `MICROVM_IMAGE_VERSION`, `MICROVM_EXECUTION_ROLE_ARN`, `MICROVM_STATIC_IDENTITY_SECRET_ARN`, `MICROVM_GITLAB_BASE_URL`, `MICROVM_GIT_FACTORY_TOKEN`, `MICROVM_ZITI_BROKER_URL`, `MICROVM_ZITI_BROKER_SERVER_ID`, `MICROVM_ZITI_BROKER_ENROLL`, `MICROVM_LOG_GROUP`, `MICROVM_LOG_STREAM_PREFIX`, `MICROVM_CONTROLLER_ROLE_ARN`, `MICROVM_EXPECTED_LAUNCHER_ACCOUNT`, `MICROVM_PRODUCTION_MODE`, `MICROVM_MAXIMUM_DURATION_SECONDS`, `MICROVM_SESSION_VENDOR_ROLE_ARN`, `MICROVM_SESSION_CREDENTIAL_DURATION_SECONDS`, `MICROVM_IDLE_POLICY_ENABLED`, `MICROVM_IDLE_AUTO_RESUME_ENABLED`, `MICROVM_IDLE_MAX_IDLE_DURATION_SECONDS`, `MICROVM_IDLE_SUSPENDED_DURATION_SECONDS`, `MICROVM_MAX_STANDBY_AGE`, `MICROVM_SUSPEND_ENABLED`, `MICROVM_RESUME_REHYDRATE_ENABLED`, `MICROVM_RESUME_REHYDRATE_DEADLINE_SECONDS` |
| `GARDENIO_` | 17 | `GARDENIO_ENV`, `GARDENIO_AGENT_COVERAGE`, `GARDENIO_AGENT_COVERAGE_CLAIM`, `GARDENIO_AGENT_COVERAGE_MOUNT`, `GARDENIO_CHANNEL`, `GARDENIO_ADVICE_SWEEP_INTERVAL`, `GARDENIO_ADVICE_COMPACTION`, `GARDENIO_SPEND_SWEEP_INTERVAL`, `GARDENIO_SPEND_BACKFILL_HISTORICAL`, `GARDENIO_SPEND_BACKFILL_DRY_RUN`, `GARDENIO_XCHECK_SWEEP_INTERVAL`, `GARDENIO_SESSION_REAPER_INTERVAL`, `GARDENIO_CLAUDE_STATUS_POLL_INTERVAL`, `GARDENIO_CLAUDE_STATUS_URL`, `GARDENIO_CLAUDE_STATUS_FALLBACK_URLS`, `GARDENIO_SPENDLOGS_SWEEP_INTERVAL`, `GARDENIO_SPENDLOGS_SWEEP_LOOKBACK` |
| `BRIDGE_` | 10 | `BRIDGE_TOKEN_ROTATION_ENABLED`, `BRIDGE_TOKEN_ROTATION_DRY_RUN`, `BRIDGE_TOKEN_ROTATION_INTERVAL`, `BRIDGE_TOKEN_ROTATION_THRESHOLD`, `BRIDGE_TOKEN_ROTATION_ACCESS_TTL`, `BRIDGE_TOKEN_ROTATION_REFRESH_TTL`, `BRIDGE_TOKEN_ROTATION_SECRET`, `BRIDGE_TOKEN_ROTATION_FLEET_MAP_KEY`, `BRIDGE_TOKEN_ROTATION_FLEETS`, `BRIDGE_TOKEN_ROTATION_PRIVILEGED_FLEETS` |
| `COOP_` | 9 | `COOP_MAX_PODS_GVISOR`, `COOP_RATE_LIMIT_WINDOW`, `COOP_RATE_LIMIT_MAX`, `COOP_CIRCUIT_BREAKER_THRESHOLD`, `COOP_CIRCUIT_BREAKER_WINDOW`, `COOP_CIRCUIT_BREAKER_COOLDOWN`, `COOP_QUEUE_EXPIRY_TIMEOUT`, `COOP_SYNC_INTERVAL`, `COOP_UPGRADE_DRAIN_TIMEOUT` |
| `FREEZE_` | 9 | `FREEZE_THAW_ENABLED`, `FREEZE_SNAPSHOT_S3_BUCKET`, `FREEZE_SNAPSHOT_S3_PREFIX`, `FREEZE_THAW_ROLE_ALLOWLIST`, `FREEZE_INTERRUPT_ENABLED`, `FREEZE_POST_PUT_DELAY`, `FREEZE_ARMED_GUARD_ENABLED`, `FREEZE_ARMED_WAIT`, `FREEZE_HARD_DEADLINE` |
| `CLAUDE_` | 7 | `CLAUDE_TEAMS_ENABLED`, `CLAUDE_TEAMMATE_MODE`, `CLAUDE_TEAMS_MAX_TEAMMATES`, `CLAUDE_TEAMS_CPU_REQUEST`, `CLAUDE_TEAMS_CPU_LIMIT`, `CLAUDE_TEAMS_MEMORY_REQUEST`, `CLAUDE_TEAMS_MEMORY_LIMIT` |
| `ZITI_` | 6 | `ZITI_ACCESS_BROKER_URL`, `ZITI_ACCESS_TOKEN_AUDIENCE`, `ZITI_TUNNELER_IMAGE`, `ZITI_ACCESS_DEFAULT_TTL`, `ZITI_ACCESS_REVOKE_ON_EXIT`, `ZITI_ACCESS_CONTROLLER_TOKEN_FILE` |
| `GITHUB_` | 5 | `GITHUB_APP_ENABLED`, `GITHUB_APP_ID`, `GITHUB_APP_INSTALLATION_ID`, `GITHUB_APP_PRIVATE_KEY_FILE`, `GITHUB_APP_API_URL` |
| `SPAWN_` | 5 | `SPAWN_GUARD_RATE_MAX`, `SPAWN_GUARD_RATE_WINDOW`, `SPAWN_GUARD_TRIP_THRESHOLD`, `SPAWN_GUARD_TRIP_WINDOW`, `SPAWN_GUARD_TRIP_COOLDOWN` |
| `THAW_` | 5 | `THAW_RETRY_ONCE`, `THAW_REHYDRATE_DEADLINE`, `THAW_CREATE_RESTORE_POD_READY_TIMEOUT`, `THAW_HARD_DEADLINE`, `THAW_DRAIN_TIMEOUT` |
| `WARM_` | 5 | `WARM_PLACEHOLDER_POOL_ENABLED`, `WARM_PLACEHOLDER_POOL_MIN_SIZE`, `WARM_PLACEHOLDER_STALE_DRAIN_RATE`, `WARM_PLACEHOLDER_NODE_FLOOR`, `WARM_PLACEHOLDER_MAX_CONCURRENT` |
| `AGENT_` | 4 | `AGENT_STARTUP_PROBE_FAILURE_THRESHOLD`, `AGENT_INIT_CLONE_JITTER_MAX_SECONDS`, `AGENT_PULL_POLICY`, `AGENT_DEFAULT_HARNESS` |
| `NATS_` | 4 | `NATS_TOKEN`, `NATS_USER`, `NATS_TOKEN_SECRET`, `NATS_AGENT_USER_SECRET` |
| `PVC_` | 4 | `PVC_GC_INTERVAL`, `PVC_GC_TTL`, `PVC_OBS_INTERVAL`, `PVC_OBS_THRESHOLD` |
| `VPA_` | 4 | `VPA_MEMORY_STAMP_ENABLED`, `VPA_OBJECT_NAME_TEMPLATE`, `VPA_MEMORY_FLOOR`, `VPA_MEMORY_MAX` |
| `AUTH_` | 3 | `AUTH_EXHAUSTED_SWEEP_INTERVAL`, `AUTH_FAILOVER_COOLDOWN`, `AUTH_MAX_FAILOVER_DURATION` |
| `LITELLM_` | 3 | `LITELLM_KEY_ROTATION_ENABLED`, `LITELLM_KEY_ROTATION_INTERVAL`, `LITELLM_KEY_ROTATION_MAX_AGE` |
| `NODE_` | 3 | `NODE_LIFECYCLE_INTERVAL`, `NODE_LIFECYCLE_NODE_SELECTOR`, `NODE_LIFECYCLE_IDLE_POD_NAMESPACES` |
| `PREWARMED_` | 3 | `PREWARMED_PRIORITY_CLASS`, `PREWARMED_POOL_INTERVAL`, `PREWARMED_MICROVM_MAX_AGE` |
| `PV_` | 3 | `PV_GC_INTERVAL`, `PV_GC_TTL`, `PV_GC_NAMESPACES` |
| `STALEREAPER_` | 3 | `STALEREAPER_RUNTIME_MAX_CREW`, `STALEREAPER_RUNTIME_MAX_THREAD`, `STALEREAPER_RUNTIME_MAX_PLAN_GARDENER` |
| `STUCK_` | 3 | `STUCK_AGENT_TIMEOUT`, `STUCK_LONG_LIVED_MULTIPLIER`, `STUCK_RESTART_IN_PLACE` |
| `GRAFANA_` | 2 | `GRAFANA_ENV_API_KEY_SECRET`, `GRAFANA_ENV_LIST` |
| `JIRA_` | 2 | `JIRA_BRIDGE_URL`, `JIRA_BRIDGE_TOKEN_SECRET` |
| `LANDS_` | 2 | `LANDS_TENANT_SPAWNS`, `LANDS_SPAWNS_PER_HOUR` |
| `POST_` | 2 | `POST_THAW_TOKEN_REFRESH_ENABLED`, `POST_THAW_TOKEN_REFRESH_INTERVAL` |
| `BEADS_` | 1 | `BEADS_E2E_HTTP_ADDR` |
| `DIND_` | 1 | `DIND_IMAGE` |
| `GEMINI_` | 1 | `GEMINI_HARNESS_ENABLED` |
| `GVISOR_` | 1 | `GVISOR_NAMESPACE` |
| `INIT_` | 1 | `INIT_CLONE_IMAGE` |
| `KUBECONFIG_` | 1 | `KUBECONFIG` |
| `LEADER_` | 1 | `LEADER_ELECTION_ID` |
| `LOG_` | 1 | `LOG_LEVEL` |
| `PILOT_` | 1 | `PILOT_FREEZE_CLASS` |
| `RWX_` | 1 | `RWX_NS_COUNT_INTERVAL` |
| `STALE_` | 1 | `STALE_AGENT_TIMEOUT` |
| `STANDBY_` | 1 | `STANDBY_POLL` |
| `TOOL_` | 1 | `TOOL_LATENCY_SLOW_THRESHOLD` |
| `VMSINGLE_` | 1 | `VMSINGLE_QUERY_URL` |

Large families that look dormant in prod: `MICROVM_*` (29, the Lambda MicroVM backend);
`FREEZE_*`/`THAW_*`/`POST_THAW_*`/`WARM_PLACEHOLDER_*`/`PILOT_FREEZE_CLASS`/`GVISOR_NAMESPACE`
(gVisor freeze/thaw and the warm pool; `gvisor.enabled` is false in k3s); `CLAUDE_TEAMS_*`; `ZITI_*`;
`GITHUB_APP_*`; `BRIDGE_TOKEN_ROTATION_*`; and `JIRA_BRIDGE_*` (jira-bridge is off). Other
packages read their own env vars too (for example `LANDS_*` and `MAILER_*`, which k3s does set), so
this table covers `config.go` only.

### Chart `enabled:` flags (`values.yaml`)

`values.yaml` has 418 `enabled:` keys. 292 default to `false`, and k3s turns on 78 keys. The nested flags that stay off in prod (195),
grouped by top-level key:

| top-level key | top-level in k3s | nested `enabled` flags off in prod |
|---|---|---|
| `agents` | on | `agentServiceAccount.clusterReadOnly`, `bridgeTokenAdmissionPolicy`, `bridgeTokenRotation`, `claudeTeams`, `clusterAdmin`, `fleetManager`, `githubApp`, `githubApp.externalSecret`, `grafanaApiKeyExternalSecret`, `infraReader`, `metrics.grafanaDashboard`, `metrics.prometheusRule.adviceBudget`, `metrics.prometheusRule.harnessOverhead`, `metrics.prometheusRule.landsAbuse`, `metrics.prometheusRule.operatedBy`, `metrics.prometheusRule.restartChurn`, `metrics.prometheusRule.resumeLatency`, `metrics.prometheusRule.schemaDrift`, `metrics.prometheusRule.serviceFieldCrossFleet`, `metrics.prometheusRule.spendReconciliation`, `metrics.prometheusRule.tagCostValidation`, `metrics.prometheusRule.vpaGuardrails`, `metrics.prometheusRule.vpaGuardrails.grafanaDashboard`, `metrics.prometheusRule.vpaGuardrails.recommendationExporter`, `metrics.prometheusRule.vpaGuardrails.recommenderServiceMonitor`, `metrics.serviceMonitor`, `microvm`, `microvm.idlePolicy`, `microvm.suspendOnStop`, `nodeLifecycle`, `nodeLifecycle.idleNodes`, `prewarmImage`, `pvcObs`, `rwxNsCount`, `zitiAccess` |
| `alertmanager` | on | `costRoute`, `persistence`, `securityRoute` |
| `alertmanagerBridge` (deleted 2026-10-31, gd-Y4ZCGr2m2t) | off | `webhook` |
| `backup` | n/a | `nats`, `postgres` |
| `beads` | n/a | `charts`, `databasePasswordFile`, `embedding`, `embedding.backfill`, `externalRDS`, `ingress`, `ingress.avaIngress`, `ingress.ciRoute`, `ingress.ciRoute.ipWhitelist`, `ingress.natsWebsocketRoute`, `ingress.natsWebsocketRoute.ipWhitelist`, `ingress.rateLimit` |
| `beadsViewer` | on | `avaIngress`, `edgeService`, `ingress`, `ingress.forwardAuthIngress`, `ingress.ipWhitelist`, `ingress.rateLimit`, `k8sSA`, `loginPicker` |
| `claudemore` (removed: folded to the e2e test lane 2026-10-31, gd-Y4ZCGr2m2t) | off | `isolation`, `recording` |
| `cloudwatchExporter` | off | `independentExternalReach` |
| `coopmux` | on | `ingress`, `ingress.rateLimit`, `natsRelay`, `persistence` |
| `coreLogShipper` | off | `components.controller`, `components.grafanaBridge`, `components.slackBridge`, `components.triggerEngine` |
| `edge` | n/a | `buffering`, `crowdsec`, `crowdsec.appsec`, `inFlightReq` |
| `edgeAuth` | off | `cookieAuth`, `entra`, `ipWhitelist`, `podDisruptionBudget`, `rateLimit` |
| `githubBridge` | off | `ingress`, `ingress.ipWhitelist`, `webhookSecret.externalSecret` |
| `gitlabBridge` | off | `aiApproval`, `cicdMetrics.rwx`, `ingress`, `ingress.ipWhitelist`, `metrics.prometheusRule.webhookDeadman`, `persistence`, `releaseDrought`, `releaseDrought.prometheusRule`, `webhookIngress` |
| `global` | n/a | `admissionPolicies.requireResourceLimits`, `admissionPolicies.requireRunsc`, `admissionPolicies.restrictRegistries`, `admissionPolicies.restrictSecurityContext`, `coverage`, `coverage.disallowCoverImages`, `imageSigning.verify` |
| `grafana` | on | `assistant`, `auth.oidc`, `imageRenderer`, `ingress`, `ingress.ipWhitelist`, `ingress.rateLimit`, `llm`, `llm.prebake`, `victoriaLogsDatasource` |
| `grafanaBridge` | on | `grafana.externalSecret`, `persistence` |
| `gvisor` | off | `freezeThaw`, `freezeThaw.postThawTokenRefresh`, `warmPlaceholderPool` |
| `jiraBridge` | off | `metrics.prometheusRule.pollerStall`, `metrics.prometheusRule.rateLimitRecovery`, `metrics.prometheusRule.triggerStarved`, `metrics.prometheusRule.webhookDeadman`, `persistence`, `webhook`, `webhook.ipWhitelist`, `webhookIngress` |
| `keycloak` | on | `admin`, `corporateIdp`, `forwardAuth.forceLogoutOn403`, `githubIdp`, `runtime.persistence` |
| `landing` | off | `ingress`, `ingress.ipWhitelist`, `ingress.rateLimit` |
| `litellm` | on | `autoscaling.customMetric`, `bedrock`, `cacheTTL`, `embedding`, `imageGeneration`, `jemalloc`, `redis`, `redis.deploy`, `thinkingCapture` |
| `mcpGrafana` | off | `token.externalSecret` |
| `mcpGrafanaWrite` | off | `token.externalSecret` |
| `monitoring` | n/a | `clusterMetrics.kubeStateMetrics`, `clusterMetrics.kubeletCadvisor`, `monorepoDashboards`, `victoriaMetrics` |
| `nats` | on | `auth`, `auth.subjectIsolation`, `websocket` |
| `otel` | n/a | `traces` |
| `postgres` | on | `backup` |
| `reproCockpit` (deleted 2026-10-31, gd-Y4ZCGr2m2t) | off | `avaIngress`, `ingress` |
| `rolesServer` (deleted 2026-10-31, gd-Y4ZCGr2m2t) | off | `ingress`, `ingress.ipWhitelist`, `ingress.rateLimit` |
| `rwxReaper` | off | `nsCountAlert`, `rdsCleanup` |
| `scifPortal` (deleted 2026-10-31, gd-Y4ZCGr2m2t) | off | `avaIngress` |
| `slackBridge` | on | `bouncer`, `ingress`, `ingress.ipWhitelist`, `mentionGate`, `metrics.prometheusRule.assistantPane`, `metrics.serviceMonitor` |
| `triggerEngine` | on | `ingress.ipWhitelist`, `reconcile.persistence` |
| `unifiedHost` | off | `beadsForwardAuth`, `forwardAuth`, `rootRedirect` |
| `vmagent` | on | `cloudwatchExporterScrape`, `karpenterScrape`, `kubeletScrape`, `kubeletScrape.cadvisorOOM` |
| `vmalert` | on | `gapAlerts.accountThrottled`, `gapAlerts.containerOOMKilled`, `gapAlerts.darkSpend`, `gapAlerts.fleetSize`, `gapAlerts.idleNodes`, `gapAlerts.nodeLifecycle`, `gapAlerts.nodepoolMemory`, `gapAlerts.nsAge`, `gapAlerts.nsCount`, `gapAlerts.pendingPods`, `gapAlerts.pvcObs` |
| `vmsingle` | on | `ingest`, `ingest.ipWhitelist`, `ingest.rateLimit` |
| `vpaRightSizing` | off | `stamp` |
| `vtsingle` | off | `devRelay`, `ingest`, `ingest.ipWhitelist`, `ingest.rateLimit`, `otlpGRPC`, `relay` |
| `wlBridge` (deleted 2026-10-31, gd-Y4ZCGr2m2t) | off | `persistence` |

## Method

Everything was read on `main` @ `cef62beb`, without compiling. `go list` only parses
imports, so it is safe in a 2Gi pod (gd-QLhMjEsBsR).

```bash
# what prod runs
helm template gardenio gardenio/helm/gardenio -n gardenio \
  -f deploy-values/gardenio-k3s.yaml --set global.imageTag=TAG > k3s.yaml
# binary -> internal packages
(cd gardenio/controller && go list -f '{{.ImportPath}} {{join .Deps " "}}' ./cmd/...)
(cd gardenio/controller && go list ./internal/...)
(cd kbeads && go list -f '{{.ImportPath}} {{join .Deps " "}}' ./cmd/...)
# images: .github/images.json ("enabled": false gates), the `for bin in` list in
#   gardenio/images/_controller-base/Dockerfile, and each images/<name>/Dockerfile COPY
# chart flags: every map with an `enabled` key, values.yaml vs deploy-values/gardenio-k3s.yaml
yq -N '.. | select(type=="!!map" and has("enabled")) | (path|join(".")) + "=" + (.enabled|tostring)' <file>
# e2e users: grep for cmd/<name> in gardenio/tests/e2e/scripts/test-*.sh; hermetic =
#   module listed in HERMETIC_MODULES in run-shard.sh
# config knobs: regex over os.Getenv/os.LookupEnv/env*Or("KEY", default) in config.go,
#   joined with the env of every container in the k3s render
```

Limits: this pod has no kubectl, so "runs in prod" means "the k3s values render it", not
"observed live". Operator-applied manifests (`helm/k3s-audit-shipper/`) and the ziti chart are
listed from their source. Hand-applied drift on the cluster would not show here.
