{{/*
Gardenio chart helpers.
Deploys beads daemon, agent controller, coopmux, and optionally PostgreSQL and NATS.
*/}}

{{- define "gardenio.fullname" -}}
{{- .Release.Name | trunc 63 | trimSuffix "-" }}
{{- end }}

{{/*
gardenio.traefik.ingressRoutesEnabled

Outer gate for every Traefik CRD this chart emits (IngressRoute / Middleware /
TLSOption). Returns the string "true" when the chart should emit Traefik CRs,
"" (falsy) otherwise. Use as: {{- if eq (include "gardenio.traefik.ingressRoutesEnabled" .) "true" }}

Why this exists (ka-0000001357): route emission used to be gated directly on
`.Values.traefik.enabled`. But that flag was overloaded — it is also the "does a
Traefik workload exist?" intent for overlays that front traffic with a *different*
ingress controller (customer-poc POC) AND the "do not deploy a second Traefik"
intent for the BYO/in-box profile, which runs the k3s-BUNDLED Traefik in
kube-system. The chart deploys NO Traefik workload of its own, so on the in-box
profile `traefik.enabled=false` is correct for "don't deploy Traefik" yet WRONG
for "don't emit routes" — the bundled Traefik still needs the IngressRoutes. The
result was ZERO routes on the in-box profile, the LE perHost cert never bound, and
acceptance properties 1 (trusted cert) + 2 (auth-gating) unsatisfiable.

This helper decouples the two. The new inner flag `traefik.ingressRoutes.enabled`
gates CR emission. When it is explicitly set, it wins. When it is left unset
(null), we fall back to the legacy `traefik.enabled` so existing overlays —
including the customer-poc POC that sets `traefik.enabled=false` to emit zero CRs —
are byte-for-byte unchanged. The in-box overlay sets `traefik.ingressRoutes.enabled=true`
to emit routes for the bundled Traefik even with `traefik.enabled=false`.
*/}}
{{- define "gardenio.traefik.ingressRoutesEnabled" -}}
{{- $t := .Values.traefik | default dict -}}
{{- $ir := $t.ingressRoutes | default dict -}}
{{- if not (kindIs "invalid" $ir.enabled) -}}
{{- if $ir.enabled }}true{{ end -}}
{{- else if $t.enabled -}}
true
{{- end -}}
{{- end }}

{{/*
Common labels
*/}}
{{- define "gardenio.labels" -}}
helm.sh/chart: {{ printf "%s-%s" .Chart.Name .Chart.Version | replace "+" "_" | trunc 63 | trimSuffix "-" }}
app.kubernetes.io/managed-by: {{ .Release.Service }}
{{ include "gardenio.selectorLabels" . }}
{{- end }}

{{/*
Selector labels
*/}}
{{- define "gardenio.selectorLabels" -}}
app.kubernetes.io/name: {{ .Chart.Name }}
app.kubernetes.io/instance: {{ .Release.Name }}
{{- end }}

{{/*
Karpenter consolidation pin for single-replica spine services. Every
eviction of these pods is an outage (RWO volume re-attach, Slack socket
reconnect, DB restart), so they opt out of consolidation. Requires the
nodepool terminationGracePeriod backstop so pins cannot wedge node
deletion. Emits nothing when karpenter.doNotDisrupt is false.
*/}}
{{- define "gardenio.doNotDisrupt" -}}
{{- if .Values.karpenter.doNotDisrupt -}}
karpenter.sh/do-not-disrupt: "true"
{{- end -}}
{{- end -}}

{{/* ===== Image tag/pull policy helpers ===== */}}

{{/*
Effective latestMode: global.latestMode UNLESS global.airgap forces it off.
Air-gap blocker P4.3 (ka-0000007177): latestMode floats the "latest" tag,
forces ImagePullPolicy: Always, AND bypasses the pinned digest — any one of
which silently defeats a digest-pinned air-gap bundle. global.airgap therefore
hard-disables latestMode everywhere image refs are resolved. Returns "true" or
"" (empty = falsy in Helm `if`).
Usage: include "gardenio.latestMode" .global
*/}}
{{- define "gardenio.latestMode" -}}
{{- if and .latestMode (not .airgap) -}}
true
{{- end -}}
{{- end }}

{{/*
Resolve image tag for gardenio-owned images.
When latestMode is effectively on, returns "latest" regardless of per-component tag.
Otherwise resolves per-component tag > global.imageTag > Chart.AppVersion.
global.imageTag is the chart-wide pin knob the terraform overlays emit
(image_tag_override, ka-0000003658) — one value retagging every gardenio-owned
image without touching per-component overrides.
Usage: include "gardenio.imageTag" (dict "tag" .Values.agents.image.tag "global" .Values.global "Chart" .Chart)
*/}}
{{- define "gardenio.imageTag" -}}
{{- if include "gardenio.latestMode" .global -}}
latest
{{- else -}}
{{- .tag | default .global.imageTag | default .Chart.AppVersion -}}
{{- end -}}
{{- end }}

{{/*
Resolve image pull policy for gardenio-owned images.
When latestMode is effectively on, always returns "Always".
Otherwise returns the per-component pullPolicy, falling back to "IfNotPresent".
Usage: include "gardenio.imagePullPolicy" (dict "pullPolicy" .Values.agents.image.pullPolicy "global" .Values.global)
*/}}
{{- define "gardenio.imagePullPolicy" -}}
{{- if include "gardenio.latestMode" .global -}}
Always
{{- else -}}
{{- .pullPolicy | default "IfNotPresent" -}}
{{- end -}}
{{- end }}

{{/*
Resolve full image reference for gardenio-owned images.
Renders "<registry>/<repo>:<tag>@<digest>" when a non-empty digest is set,
else "<registry>/<repo>:<tag>". latestMode bypasses the digest (the "latest"
tag floats and would not match a pinned digest).
Usage:
  include "gardenio.imageRef" (dict "image" .Values.agents.image "global" .Values.global "Chart" .Chart)
*/}}
{{- define "gardenio.imageRef" -}}
{{- $tag := include "gardenio.imageTag" (dict "tag" .image.tag "global" .global "Chart" .Chart) -}}
{{- $repo := .image.repository -}}
{{- if and .global.coverage .global.coverage.enabled -}}
{{- $repo = printf "%s-cover" $repo -}}
{{- end -}}
{{- $ref := printf "%s/%s:%s" .global.imageRegistry $repo $tag -}}
{{- if and .image.digest (not (include "gardenio.latestMode" .global)) (not (and .global.coverage .global.coverage.enabled)) -}}
{{- printf "%s@%s" $ref .image.digest -}}
{{- else -}}
{{- $ref -}}
{{- end -}}
{{- end }}

{{/*
Resolve full image reference for Docker-Hub-mirrored upstream images.
Renders "<mirrorRegistry>/<repository>:<tag>@<digest>" when digest is set,
else "<mirrorRegistry>/<repository>:<tag>". `image` must carry repository
and tag (digest optional). Library/official images use the `library/<name>`
repository convention to match the mirror namespace layout.
Usage:
  include "gardenio.mirroredImage" (dict "image" .Values.busyboxImage "global" .Values.global)
*/}}
{{- define "gardenio.mirroredImage" -}}
{{- $registry := .image.registry | default .global.mirrorImageRegistry -}}
{{- $ref := printf "%s/%s:%s" $registry .image.repository .image.tag -}}
{{- if .image.digest -}}
{{- printf "%s@%s" $ref .image.digest -}}
{{- else -}}
{{- $ref -}}
{{- end -}}
{{- end }}

{{/* ===== Beads daemon connection helpers ===== */}}

{{/*
Beads daemon service host.
*/}}
{{- define "gardenio.beads.host" -}}
{{- include "gardenio.beads.fullname" . -}}
{{- end }}

{{/*
Beads daemon gRPC port.
*/}}
{{- define "gardenio.beads.port" -}}
{{- .Values.beads.service.grpcPort | default 9090 -}}
{{- end }}

{{/*
Beads daemon HTTP port.
*/}}
{{- define "gardenio.beads.httpPort" -}}
{{- .Values.beads.service.httpPort | default 8080 -}}
{{- end }}

{{/* ===== Agent Controller component helpers ===== */}}

{{/*
Agent Controller fully qualified name
*/}}
{{- define "gardenio.agents.fullname" -}}
{{- printf "%s-agents" (include "gardenio.fullname" .) | trunc 63 | trimSuffix "-" }}
{{- end }}

{{/*
Agent Controller labels
*/}}
{{- define "gardenio.agents.labels" -}}
{{ include "gardenio.labels" . }}
app.kubernetes.io/component: agents
{{- end }}

{{/*
Agent Controller selector labels
*/}}
{{- define "gardenio.agents.selectorLabels" -}}
{{ include "gardenio.selectorLabels" . }}
app.kubernetes.io/component: agents
{{- end }}

{{/*
Spawned-agent-pod matchLabels body for NetworkPolicies.

Emits `matchExpressions: gardenio.io/agent Exists` instead of
`matchLabels: {gardenio.io/app: gardenio}`. Rationale (ka-0000006038): every
spawned agent pod ALWAYS carries gardenio.io/agent=<agent-name> (podmanager
AgentPodSpec.Labels, LabelAgent), across all controller versions. The
gardenio.io/app=gardenio marker was added later (commit b729a77cd) specifically
for NetworkPolicy match, so a box whose deployed controller predates/omits that
label has agent pods that match NONE of the agent NetworkPolicies — they fall
into default-deny with zero egress (no DNS, no beads, no coopmux, no litellm)
and the spawned agent goes silent. Selecting on the always-present
gardenio.io/agent marker makes the policies controller-version-independent.
Use inside a podSelector body (top-level or a from/to peer).
*/}}
{{- define "gardenio.agentPodSelector" -}}
matchExpressions:
  - key: gardenio.io/agent
    operator: Exists
{{- end }}

{{/*
Agent Controller service account name
*/}}
{{- define "gardenio.agents.serviceAccountName" -}}
{{- if .Values.agents.serviceAccount.create }}
{{- default (printf "%s-sa" (include "gardenio.agents.fullname" .)) .Values.agents.serviceAccount.name }}
{{- else }}
{{- default "default" .Values.agents.serviceAccount.name }}
{{- end }}
{{- end }}

{{/*
Auxiliary service account name (gd-Hk4RBE2MC7): used by beads-viewer, which
reuses the controller's IRSA role but must not hold the
controller's grants (notably the Codex keeper's secrets update). When the chart
creates ServiceAccounts this is "<fullname>-aux-sa" (annotated identically to
the agents SA); when the operator supplies their own SA out-of-band
(agents.serviceAccount.create=false) the aux workloads keep using it unchanged.
*/}}
{{- define "gardenio.agents.auxServiceAccountName" -}}
{{- if .Values.agents.serviceAccount.create }}
{{- default (printf "%s-aux-sa" (include "gardenio.fullname" .)) .Values.agents.auxServiceAccount.name }}
{{- else }}
{{- include "gardenio.agents.serviceAccountName" . }}
{{- end }}
{{- end }}

{{/* ===== Agent Pod helpers (SA/RBAC for spawned agent pods) ===== */}}

{{/*
Agent Pod service account name — used by spawned agent pods (not the controller itself).
Defaults to "<release>-agent" when agents.agentServiceAccount.create is true.
*/}}
{{- define "gardenio.coop.serviceAccountName" -}}
{{- if .Values.agents.agentServiceAccount.create }}
{{- default (printf "%s-agent" (include "gardenio.fullname" .)) .Values.agents.agentServiceAccount.name }}
{{- else }}
{{- "default" }}
{{- end }}
{{- end }}

{{/*
Agent Pod cluster-admin service account name — elevated SA for projects that need
full cluster access. Defaults to "<release>-agent-cluster-admin".
*/}}
{{- define "gardenio.coop.clusterAdminServiceAccountName" -}}
{{- default (printf "%s-agent-cluster-admin" (include "gardenio.fullname" .)) .Values.agents.clusterAdmin.name }}
{{- end }}

{{/*
Agent Pod labels
*/}}
{{- define "gardenio.coop.labels" -}}
{{ include "gardenio.labels" . }}
app.kubernetes.io/component: agent
{{- end }}

{{/* ===== Coopmux component helpers ===== */}}

{{/*
Coopmux fully qualified name
*/}}
{{- define "gardenio.coopmux.fullname" -}}
{{- printf "%s-coopmux" (include "gardenio.fullname" .) | trunc 63 | trimSuffix "-" }}
{{- end }}

{{/*
Coopmux land token key Secret (coopmux.landTokens): the HMAC key coopmux and
the controller share for tenant lands' coopmux bearers.
*/}}
{{- define "gardenio.coopmux.landTokenKeySecretName" -}}
{{- printf "%s-coopmux-land-token-key" (include "gardenio.fullname" .) | trunc 63 | trimSuffix "-" }}
{{- end }}

{{/*
Coopmux labels
*/}}
{{- define "gardenio.coopmux.labels" -}}
{{ include "gardenio.labels" . }}
app.kubernetes.io/component: coopmux
{{- end }}

{{/*
Coopmux selector labels
*/}}
{{- define "gardenio.coopmux.selectorLabels" -}}
{{ include "gardenio.selectorLabels" . }}
app.kubernetes.io/component: coopmux
{{- end }}

{{/* ===== mcp-grafana component helpers (Track A, epic ka-0000005390) ===== */}}

{{/*
mcp-grafana fully qualified name
*/}}
{{- define "gardenio.mcpGrafana.fullname" -}}
{{- printf "%s-mcp-grafana" (include "gardenio.fullname" .) | trunc 63 | trimSuffix "-" }}
{{- end }}

{{/*
mcp-grafana labels
*/}}
{{- define "gardenio.mcpGrafana.labels" -}}
{{ include "gardenio.labels" . }}
app.kubernetes.io/component: mcp-grafana
{{- end }}

{{/*
mcp-grafana selector labels
*/}}
{{- define "gardenio.mcpGrafana.selectorLabels" -}}
{{ include "gardenio.selectorLabels" . }}
app.kubernetes.io/component: mcp-grafana
{{- end }}

{{/*
In-cluster gardenio Grafana base URL for mcp-grafana. Explicit mcpGrafana.grafanaURL
wins; otherwise the in-chart Grafana base HONORING serveFromSubPath
(gardenio.grafana.inClusterBaseURL): mcp-grafana hits the Grafana pod DIRECTLY
(no Traefik strip), so when serveFromSubPath=true the pod serves its API only
under /grafana and the base MUST carry the subpath — exactly like the
image-renderer callback and the controller's agent GRAFANA_URL. Using the bare
Service root here would 404 the API under every real overlay (all set
serveFromSubPath=true). Renders empty when grafana is disabled and no explicit
URL is set.
*/}}
{{- define "gardenio.mcpGrafana.grafanaURL" -}}
{{- if .Values.mcpGrafana.grafanaURL -}}
{{- .Values.mcpGrafana.grafanaURL -}}
{{- else if .Values.grafana.enabled -}}
{{- include "gardenio.grafana.inClusterBaseURL" . -}}
{{- end -}}
{{- end }}

{{/* ===== mcp-victorialogs component helpers (Track A, task ka-0000006956) ===== */}}

{{/*
mcp-victorialogs fully qualified name
*/}}
{{- define "gardenio.mcpVictoriaLogs.fullname" -}}
{{- printf "%s-mcp-victorialogs" (include "gardenio.fullname" .) | trunc 63 | trimSuffix "-" }}
{{- end }}

{{/*
mcp-victorialogs labels
*/}}
{{- define "gardenio.mcpVictoriaLogs.labels" -}}
{{ include "gardenio.labels" . }}
app.kubernetes.io/component: mcp-victorialogs
{{- end }}

{{/*
mcp-victorialogs selector labels
*/}}
{{- define "gardenio.mcpVictoriaLogs.selectorLabels" -}}
{{ include "gardenio.selectorLabels" . }}
app.kubernetes.io/component: mcp-victorialogs
{{- end }}

{{/*
In-cluster VictoriaLogs base URL for mcp-victorialogs (VL_INSTANCE_ENTRYPOINT).
Explicit mcpVictoriaLogs.vlEntrypoint wins; otherwise the in-chart vlsingle
Service (release-relative via gardenio.vlsingle.baseURL, so it binds on the prod
release AND the gardenio-e2e-* namespaces). Renders empty when neither is
set — the Deployment then omits the env var and the server fails its required
VL_INSTANCE_ENTRYPOINT check loudly rather than querying a phantom.
*/}}
{{- define "gardenio.mcpVictoriaLogs.vlEntrypoint" -}}
{{- if .Values.mcpVictoriaLogs.vlEntrypoint -}}
{{- .Values.mcpVictoriaLogs.vlEntrypoint -}}
{{- else if .Values.vlsingle.enabled -}}
{{- include "gardenio.vlsingle.baseURL" . -}}
{{- end -}}
{{- end }}

{{/*
=== mcp-grafana-write: SECOND, WRITE-CAPABLE mcp-grafana instance (ka-0000000293) ===
A distinct Deployment/Service from the read-only `mcpGrafana` sidecar, gated on
mcpGrafanaWrite.enabled. The gardenio-assistant-app plugin BACKEND reverse-proxies
the chat box's MCP traffic here (NOT to grafana-llm-app's embedded server, which
dials Grafana's EXTERNAL edge-auth'd app_url and 403s). It runs WITHOUT
--disable-write so the curated dashboard/folder CREATE tools are advertised, and
authenticates per-request from the X-Grafana-URL / X-Grafana-API-Key headers the
plugin backend stamps (the plugin's own Grafana-minted managed SA + in-cluster
Grafana URL) — so it needs no env SA token of its own.

CRITICAL: the component label is DISTINCT ("mcp-grafana-write"). Sharing the read
sidecar's label would (a) make Traefik/agent ingress+egress NetworkPolicies select
this write instance too — widening the agent read-only contract — and (b) collide
selectors. The distinct label keeps the write instance reachable ONLY by the
grafana pod (the plugin backend) and NEVER by agents.
*/}}
{{- define "gardenio.mcpGrafanaWrite.fullname" -}}
{{- printf "%s-mcp-grafana-write" (include "gardenio.fullname" .) | trunc 63 | trimSuffix "-" }}
{{- end }}

{{- define "gardenio.mcpGrafanaWrite.labels" -}}
{{ include "gardenio.labels" . }}
app.kubernetes.io/component: mcp-grafana-write
{{- end }}

{{- define "gardenio.mcpGrafanaWrite.selectorLabels" -}}
{{ include "gardenio.selectorLabels" . }}
app.kubernetes.io/component: mcp-grafana-write
{{- end }}

{{/*
Coopmux auth token secret name
*/}}
{{- define "gardenio.coopmux.authTokenSecretName" -}}
{{- if .Values.coopmux.coopmuxTokenSecret }}
{{- .Values.coopmux.coopmuxTokenSecret }}
{{- else }}
{{- include "gardenio.beads.tokenSecretName" . }}
{{- end }}
{{- end }}

{{/*
NATS client credential Secret for infrastructure clients (coopmux, trigger-engine).
Under subject isolation this is the infra-creds Secret; otherwise
nats.auth.clientTokenSecret (the legacy single-token server's token, key
`token`). Empty means "send no token" (auth-disabled server). It is never the
beads token and never the agent-facing coopmux token: those are different
trust domains and must not double as the NATS credential (gd-5QYwXtV7IA).
*/}}
{{- define "gardenio.nats.clientTokenSecretName" -}}
{{- if .Values.nats.auth.subjectIsolation.enabled -}}
{{- printf "%s-nats-infra-creds" (include "gardenio.fullname" .) -}}
{{- else -}}
{{- .Values.nats.auth.clientTokenSecret | default "" -}}
{{- end -}}
{{- end }}

{{/*
Coopmux NATS URL — falls back to in-chart NATS when nats.enabled.
*/}}
{{- define "gardenio.coopmux.natsURL" -}}
{{- if .Values.coopmux.natsURL -}}
{{- .Values.coopmux.natsURL -}}
{{- else -}}
{{- include "gardenio.natsURL" . -}}
{{- end -}}
{{- end }}

{{/*
Coopmux service URL
*/}}
{{- define "gardenio.coopmux.serviceURL" -}}
{{- printf "http://%s:%d" (include "gardenio.coopmux.fullname" .) (int .Values.coopmux.service.port) }}
{{- end }}

{{/*
Slack bridge service URL (auto-wired when slackBridge is enabled)
*/}}
{{- define "gardenio.slackBridge.serviceURL" -}}
{{- printf "http://%s-slack-bridge:%d" (include "gardenio.fullname" .) (int (.Values.slackBridge.service.port | default 8090)) }}
{{- end }}

{{/*
Slack bridge land listener URL: the agent-token routes only, for tenant land
pods (gd-pXik57wZA9). Rendered with landNamespaces.enabled.
*/}}
{{- define "gardenio.slackBridge.landURL" -}}
{{- printf "http://%s-slack-bridge:8091" (include "gardenio.fullname" .) }}
{{- end }}

{{/*
Ingress rule admitting every tenant land namespace (gardenio.io/land) on the
given TCP ports. Each release-side caller passes exactly the ports its peer
has in landns.DefaultPeers, the land fence's egress list, so both ends of a
tenant flow open the same ports; tests/lands_release_ingress_test.go fails
when the two lists drift (gd-YOpXsJJYKc). Rendered with landNamespaces.enabled.
*/}}
{{- define "gardenio.landIngressRule" -}}
- from:
    - namespaceSelector:
        matchExpressions:
          - key: gardenio.io/land
            operator: Exists
  ports:
    {{- range . }}
    - protocol: TCP
      port: {{ . }}
    {{- end }}
{{- end }}

{{/*
Jira bridge service URL (auto-wired when jiraBridge is enabled)
*/}}
{{- define "gardenio.jiraBridge.serviceURL" -}}
{{- printf "http://%s-jira-bridge:%d" (include "gardenio.fullname" .) (int (.Values.jiraBridge.service.port | default 8091)) }}
{{- end }}

{{/*
Beads-viewer in-cluster Service base URL — the pod origin with NO base-path/edge
prefix, for BEADS_VIEWER_INTERNAL_URL (gb plan create/add-step). ka-0000000379.
Agent pods may run in a different namespace than the viewer, so this is the
fully-qualified cross-namespace DNS (…svc.cluster.local) rather than a bare
service name. Uses the beadsViewer ClusterIP Service (…-beads-viewer) on its
http port (default 8095), not the AVA edge Service.
*/}}
{{- define "gardenio.beadsViewer.internalURL" -}}
{{- printf "http://%s-beads-viewer.%s.svc.cluster.local:%d" (include "gardenio.fullname" .) .Release.Namespace (int (.Values.beadsViewer.service.port | default 8095)) }}
{{- end }}

{{/*
Coopmux ingress middlewares — shared middleware list for all ingress routes.
Accepts a dict with "fullname" and "Values" keys.
*/}}
{{- define "gardenio.coopmux.ingressMiddlewares" -}}
{{- /* Edge caps first (cheapest reject): buffering (413) then inFlightReq
       (429). releaseName == include "gardenio.fullname" . (ka-0000006519). */ -}}
{{- if and .Values.edge .Values.edge.buffering .Values.edge.buffering.enabled }}
- name: {{ .releaseName }}-edge-buffering
{{- end }}
{{- if and .Values.edge .Values.edge.inFlightReq .Values.edge.inFlightReq.enabled }}
- name: {{ .releaseName }}-edge-inflightreq
{{- end }}
{{- if .Values.global.securityHeaders.enabled }}
- name: {{ .releaseName }}-security-headers
{{- end }}
{{- if and .Values.edge .Values.edge.crowdsec .Values.edge.crowdsec.enabled (eq (include "gardenio.traefik.ingressRoutesEnabled" .) "true") }}
- name: {{ .releaseName }}-edge-crowdsec
{{- end }}
{{- if .Values.coopmux.ingress.ipWhitelist.enabled }}
- name: {{ .fullname }}-ipwhitelist
{{- end }}
{{- if .Values.coopmux.ingress.rateLimit.enabled }}
- name: {{ .fullname }}-ratelimit
{{- end }}
{{- if and .Values.keycloak.enabled .Values.keycloak.forwardAuth.enabled }}
- name: {{ .releaseName }}-forwardauth
{{- if .Values.keycloak.forwardAuth.forceLogoutOn403.enabled }}
- name: {{ .releaseName }}-forwardauth-errors
{{- end }}
{{- end }}
{{- end }}

{{/*
beads-viewer login picker — resolves the ForwardAuth (oauth2-proxy) base URL.
Falls back to https://<keycloak.forwardAuth.ingress.host> when
beadsViewer.loginPicker.forwardAuthURL is empty.
*/}}
{{- define "gardenio.beadsViewer.loginPicker.forwardAuthURL" -}}
{{- if .Values.beadsViewer.loginPicker.forwardAuthURL -}}
{{- .Values.beadsViewer.loginPicker.forwardAuthURL -}}
{{- else if .Values.keycloak.forwardAuth.ingress.host -}}
{{- printf "https://%s" .Values.keycloak.forwardAuth.ingress.host -}}
{{- else -}}
{{- "" -}}
{{- end -}}
{{- end }}

{{/* ===== PostgreSQL component helpers ===== */}}

{{/*
PostgreSQL fully qualified name
*/}}
{{- define "gardenio.postgres.fullname" -}}
{{- printf "%s-postgres" (include "gardenio.fullname" .) | trunc 63 | trimSuffix "-" }}
{{- end }}

{{/*
PostgreSQL labels
*/}}
{{- define "gardenio.postgres.labels" -}}
{{ include "gardenio.labels" . }}
app.kubernetes.io/component: postgres
{{- end }}

{{/*
PostgreSQL selector labels
*/}}
{{- define "gardenio.postgres.selectorLabels" -}}
{{ include "gardenio.selectorLabels" . }}
app.kubernetes.io/component: postgres
{{- end }}

{{/*
PostgreSQL password secret name
*/}}
{{- define "gardenio.postgres.passwordSecretName" -}}
{{- printf "%s-postgres-password" (include "gardenio.fullname" .) }}
{{- end }}

{{/*
In-box PostgreSQL password.

Used when postgres.enabled && not externalSecrets.enabled, where the
chart owns the <release>-postgres-password Secret directly (no AWS Secrets
Manager / ExternalSecrets). Mirrors gardenio.beads.externalRDS.password:
`lookup`s the existing Secret so an already-installed password is preserved
across upgrades; falls back to randAlphaNum 32 on first install.

Note: `lookup` returns nil during `helm template` (no API server), so the
fall-through path generates a fresh random password for rendered manifests.
On a real install/upgrade, lookup runs against the live cluster, and the
`helm.sh/resource-policy: keep` annotation prevents regeneration.
*/}}
{{- define "gardenio.postgres.password" -}}
{{- $secretName := include "gardenio.postgres.passwordSecretName" . -}}
{{- $existing := lookup "v1" "Secret" .Release.Namespace $secretName -}}
{{- if and $existing $existing.data (index $existing.data "password") -}}
{{- index $existing.data "password" | b64dec -}}
{{- else -}}
{{- randAlphaNum 32 -}}
{{- end -}}
{{- end }}

{{/*
===== In-box self-seeded secret value (G2, ka-0000003741) =====
Generate-if-missing helper for chart-managed Secrets on the in-box profile,
where there is no ExternalSecrets / AWS Secrets Manager backing store and the
chart must own the value directly (mirrors gardenio.postgres.password and the
crowdsec bouncer-key helper). `lookup`s an existing Secret/key so an
already-installed value is preserved across `helm upgrade` (the Secret carries
helm.sh/resource-policy: keep), and only mints a fresh `randAlphaNum <len>` on
first install.

Args (dict): "root" (root context), "secretName", "key", "len" (random length).
*/}}
{{- define "gardenio.inbox.seededValue" -}}
{{- $root := .root -}}
{{- $existing := lookup "v1" "Secret" $root.Release.Namespace .secretName -}}
{{- if and $existing $existing.data (index $existing.data .key) -}}
{{- index $existing.data .key | b64dec -}}
{{- else -}}
{{- randAlphaNum (.len | int) -}}
{{- end -}}
{{- end }}

{{/*
In-box self-seed gate: true when the chart must own a Secret directly because
there is no ExternalSecrets backing store. Used by the forwardauth / edge-auth
HMAC seed templates (G2). Returns "true"/"" so callers can `eq ... "true"`.
*/}}
{{- define "gardenio.inbox.selfSeedSecrets" -}}
{{- if not .Values.externalSecrets.enabled -}}true{{- end -}}
{{- end }}

{{/* ===== In-box object store (MinIO) helpers — ka-0000004955 ===== */}}

{{/*
MinIO fully qualified name (also the in-cluster Service DNS name).
*/}}
{{- define "gardenio.minio.fullname" -}}
{{- printf "%s-minio" (include "gardenio.fullname" .) | trunc 63 | trimSuffix "-" }}
{{- end }}

{{/*
MinIO labels
*/}}
{{- define "gardenio.minio.labels" -}}
{{ include "gardenio.labels" . }}
app.kubernetes.io/component: minio
{{- end }}

{{/*
MinIO selector labels
*/}}
{{- define "gardenio.minio.selectorLabels" -}}
{{ include "gardenio.selectorLabels" . }}
app.kubernetes.io/component: minio
{{- end }}

{{/*
MinIO root-credentials Secret name. Prefers an operator-supplied existing
Secret (objectStore.existingSecret) so deploy-byo.sh can seed it via
`openssl rand` create-if-missing; otherwise falls back to the chart-default
"<release>-minio-credentials". The Secret carries keys `accesskey`/`secretkey`.
*/}}
{{- define "gardenio.minio.secretName" -}}
{{- if .Values.objectStore.existingSecret -}}
{{- .Values.objectStore.existingSecret -}}
{{- else -}}
{{- printf "%s-minio-credentials" (include "gardenio.fullname" .) -}}
{{- end -}}
{{- end }}

{{/*
Least-privilege MinIO agent-credentials Secret name (gd-urSiLLwPIz): the
Secret the controller projects onto agent pods (COOP_S3_CREDENTIALS_SECRET)
and the bootstrap Job turns into a MinIO user. Keys `accesskey`/`secretkey`.
objectStore.agentCredentials.existingSecret, else
"<release>-minio-agent-credentials". The default must not contain
"minio-credentials": the controller refuses any Secret name carrying that
marker (the root Secret) as an agent-pod mount.
*/}}
{{- define "gardenio.minio.agentSecretName" -}}
{{- $ac := .Values.objectStore.agentCredentials | default dict -}}
{{- if $ac.existingSecret -}}
{{- $ac.existingSecret -}}
{{- else -}}
{{- printf "%s-minio-agent-credentials" (include "gardenio.fullname" .) -}}
{{- end -}}
{{- end }}

{{/*
MinIO policy for the agent user (gd-urSiLLwPIz), as JSON. Everything an
agent pod touches lives under <bucket>/<prefix>/: coop's
<prefix>/<session-key>/... session archive (upload and restore), gb's
salvage bundles and stdout tails beside it, and the shared
<prefix>/images/ and <prefix>/badges/ trees. No agent code deletes, so
there is no s3:DeleteObject. ListBucket is limited to keys under the prefix,
so the bucket root (and HeadBucket, which coop only warns about) is denied.
The prefix is shared by all agents; per-agent scoping needs per-agent
credentials (follow-up on gd-urSiLLwPIz). With objectStore.images.bucket set
(gd-CmjVAU3vgd), the same object actions also cover
<images-bucket>/<prefix>/images/, and nothing else in that bucket.
*/}}
{{- define "gardenio.minio.agentPolicy" -}}
{{- $bucket := .Values.agents.s3.bucket | default .Values.objectStore.bucket -}}
{{- $prefix := trimAll "/" (.Values.agents.s3.prefix | default "coop/sessions") -}}
{{- $bucketArn := printf "arn:aws:s3:::%s" $bucket -}}
{{- $statements := list
  (dict "Effect" "Allow" "Action" (list "s3:GetBucketLocation") "Resource" (list $bucketArn))
  (dict "Effect" "Allow" "Action" (list "s3:ListBucket") "Resource" (list $bucketArn)
        "Condition" (dict "StringLike" (dict "s3:prefix" (list (printf "%s/*" $prefix)))))
  (dict "Effect" "Allow"
        "Action" (list "s3:GetObject" "s3:PutObject" "s3:AbortMultipartUpload" "s3:ListMultipartUploadParts")
        "Resource" (list (printf "%s/%s/*" $bucketArn $prefix)))
-}}
{{- with include "gardenio.objectStore.imagesBucket" . }}
{{- $imagesArn := printf "arn:aws:s3:::%s" . }}
{{- $statements = append $statements (dict "Effect" "Allow" "Action" (list "s3:GetBucketLocation") "Resource" (list $imagesArn)) }}
{{- $statements = append $statements (dict "Effect" "Allow"
        "Action" (list "s3:GetObject" "s3:PutObject" "s3:AbortMultipartUpload" "s3:ListMultipartUploadParts")
        "Resource" (list (printf "%s/%s/images/*" $imagesArn $prefix))) }}
{{- end }}
{{- dict "Version" "2012-10-17" "Statement" $statements | toJson -}}
{{- end }}

{{/*
Read-only MinIO user for the beads daemon (gd-k3GmLJKW8R): "true" when the
in-box object store is on and objectStore.beadsCredentials.enabled is set,
else "". Gates the bootstrap Job's user creation, the beads Deployment's
AWS_* env and the credentials guard.
*/}}
{{- define "gardenio.minio.beadsCredentialsEnabled" -}}
{{- if and .Values.objectStore.enabled (.Values.objectStore.beadsCredentials | default dict).enabled -}}true{{- end -}}
{{- end }}

{{/*
Read-only MinIO beads-credentials Secret name (gd-k3GmLJKW8R): the Secret
the beads Deployment reads as AWS_ACCESS_KEY_ID/AWS_SECRET_ACCESS_KEY and the
bootstrap Job turns into a MinIO user. Keys `accesskey`/`secretkey`.
objectStore.beadsCredentials.existingSecret, else
"<release>-minio-beads-credentials". Never the root or agent Secret
(templates/validate-minio-agent-credentials.yaml refuses both).
*/}}
{{- define "gardenio.minio.beadsSecretName" -}}
{{- $bc := .Values.objectStore.beadsCredentials | default dict -}}
{{- if $bc.existingSecret -}}
{{- $bc.existingSecret -}}
{{- else -}}
{{- printf "%s-minio-beads-credentials" (include "gardenio.fullname" .) -}}
{{- end -}}
{{- end }}

{{/*
MinIO policy for the beads daemon user (gd-k3GmLJKW8R), as JSON. The land
door's decision evidence route (kbeads landmap.Pictures) serves only keys
under <bucket>/<prefix>/images/, the tree gb image upload writes and the
viewer's /image/ proxy reads. s3:GetObject there and nothing else: no
ListBucket (the daemon never lists), no Put or Delete, no other prefix or
bucket, no admin API. The prefix default matches landmap.PicturesFromEnv's.
With objectStore.images.bucket set (gd-CmjVAU3vgd) it also reads
<images-bucket>/<prefix>/images/, where new pictures land.
*/}}

{{/*
Session images bucket (gd-CmjVAU3vgd): objectStore.images.bucket when the
in-box object store is on, else "". Gates the bootstrap Job's images bucket,
the images statements in the agent and beads policies, and the
COOP_S3_IMAGES_BUCKET env on the controller, beads-viewer and beads daemon.
*/}}
{{- define "gardenio.objectStore.imagesBucket" -}}
{{- if .Values.objectStore.enabled -}}
{{- (.Values.objectStore.images | default dict).bucket | default "" -}}
{{- end -}}
{{- end }}
{{- define "gardenio.minio.beadsPolicy" -}}
{{- $bucket := .Values.agents.s3.bucket | default .Values.objectStore.bucket -}}
{{- $prefix := trimAll "/" (.Values.agents.s3.prefix | default "coop/sessions") -}}
{{- $resources := list (printf "arn:aws:s3:::%s/%s/images/*" $bucket $prefix) -}}
{{- with include "gardenio.objectStore.imagesBucket" . }}
{{- $resources = append $resources (printf "arn:aws:s3:::%s/%s/images/*" . $prefix) }}
{{- end }}
{{- $statements := list
  (dict "Effect" "Allow" "Action" (list "s3:GetObject") "Resource" $resources)
-}}
{{- dict "Version" "2012-10-17" "Statement" $statements | toJson -}}
{{- end }}

{{/*
TLS — name of the wildcard TLS Secret referenced by every IngressRoute.
Resolves to `tls.existingSecretName` when the operator wires the chart to
an externally-managed wildcard cert (D5 / ka-0000005757 — per-instance
gardenio-e2e-<id> releases), otherwise the chart-default
`<release>-wildcard-tls` Secret produced by the chart's cert-manager
Certificate template.
*/}}
{{- define "gardenio.tls.secretName" -}}
{{- if and .Values.tls.enabled .Values.tls.existingSecretName -}}
{{- .Values.tls.existingSecretName -}}
{{- else -}}
{{- printf "%s-wildcard-tls" (include "gardenio.fullname" .) -}}
{{- end -}}
{{- end }}

{{/* ===== Beads component helpers ===== */}}

{{/*
Beads fully qualified name
*/}}
{{- define "gardenio.beads.fullname" -}}
{{- printf "%s-beads" (include "gardenio.fullname" .) | trunc 63 | trimSuffix "-" }}
{{- end }}

{{/*
Beads labels
*/}}
{{- define "gardenio.beads.labels" -}}
{{ include "gardenio.labels" . }}
app.kubernetes.io/component: beads
{{- end }}

{{/*
Beads selector labels
*/}}
{{- define "gardenio.beads.selectorLabels" -}}
{{ include "gardenio.selectorLabels" . }}
app.kubernetes.io/component: beads
{{- end }}

{{/*
Bridge-credential token secret name — value-alias fallback for the
fleet→bridge token rename (ka-0000005788, decision ka-0000000201).

Precedence (so existing values files keep working through the compat window):
  1. .Values.beads.bridgeTokenSecret      — canonical key; explicit value wins,
                                             including "" to disable the bridge
                                             token path.
  2. .Values.beads.bridgeFleetTokenSecret — DEPRECATED alias; honoured when the
                                             canonical key is unset (older values
                                             files that still pin the old key).
  3. "bridge-tokens"                       — chart default secret name.

Both keys default to null in values.yaml so `kindIs "invalid"` distinguishes
"user did not set this" from an explicit "" (disable). NOTE: this renames the
bridge-CREDENTIAL secret only — unrelated to the tenancy fleet concept.
*/}}
{{- define "gardenio.beads.bridgeTokenSecretName" -}}
{{- if not (kindIs "invalid" .Values.beads.bridgeTokenSecret) -}}
{{- .Values.beads.bridgeTokenSecret -}}
{{- else if not (kindIs "invalid" .Values.beads.bridgeFleetTokenSecret) -}}
{{- .Values.beads.bridgeFleetTokenSecret -}}
{{- else -}}
bridge-tokens
{{- end -}}
{{- end -}}

{{/*
Beads database URL — constructs postgres connection string when postgres.enabled.
Uses K8s variable expansion $(BEADS_DATABASE_PASSWORD) so the password comes from the secret env var.
*/}}
{{- define "gardenio.beads.databaseURL" -}}
{{- if .Values.beads.databaseURL -}}
{{- .Values.beads.databaseURL -}}
{{- else if .Values.beads.externalRDS.enabled -}}
{{- printf "postgres://%s:$(BEADS_DATABASE_PASSWORD)@%s:%v/%s?sslmode=%s"
  (include "gardenio.beads.externalRDS.roleName" .)
  .Values.beads.externalRDS.host
  (.Values.beads.externalRDS.port | default 5432)
  (include "gardenio.beads.externalRDS.databaseName" .)
  (.Values.beads.externalRDS.sslmode | default "require") -}}
{{- else if .Values.postgresExternal.enabled -}}
{{- printf "postgres://%s:$(BEADS_DATABASE_PASSWORD)@%s:%v/%s?sslmode=%s"
  .Values.postgresExternal.username
  .Values.postgresExternal.host
  (.Values.postgresExternal.port | default 5432)
  (.Values.postgresExternal.database | default "gardenio")
  (.Values.postgresExternal.sslmode | default "require") -}}
{{- else if .Values.postgres.enabled -}}
{{- printf "postgres://postgres:$(BEADS_DATABASE_PASSWORD)@%s:5432/%s?sslmode=disable" (include "gardenio.postgres.fullname" .) (.Values.postgres.database | default "beads") -}}
{{- end -}}
{{- end }}

{{/*
Beads database URL WITHOUT the $(BEADS_DATABASE_PASSWORD) substitution — the
userinfo carries only the username. Used in rotation-tolerant password-file
mode (beads.databasePasswordFile.enabled): the daemon reads the password from
the mounted Secret file and injects it into this URL in-process, so no env-var
password is baked into the connection string at container start. ka-0000004150.
An explicit beads.databaseURL override is passed through verbatim (the daemon's
connector strips any embedded/placeholder password, the file value wins).
*/}}
{{- define "gardenio.beads.databaseURLNoPassword" -}}
{{- if .Values.beads.databaseURL -}}
{{- .Values.beads.databaseURL -}}
{{- else if .Values.beads.externalRDS.enabled -}}
{{- printf "postgres://%s@%s:%v/%s?sslmode=%s"
  (include "gardenio.beads.externalRDS.roleName" .)
  .Values.beads.externalRDS.host
  (.Values.beads.externalRDS.port | default 5432)
  (include "gardenio.beads.externalRDS.databaseName" .)
  (.Values.beads.externalRDS.sslmode | default "require") -}}
{{- else if .Values.postgresExternal.enabled -}}
{{- printf "postgres://%s@%s:%v/%s?sslmode=%s"
  .Values.postgresExternal.username
  .Values.postgresExternal.host
  (.Values.postgresExternal.port | default 5432)
  (.Values.postgresExternal.database | default "gardenio")
  (.Values.postgresExternal.sslmode | default "require") -}}
{{- else if .Values.postgres.enabled -}}
{{- printf "postgres://postgres@%s:5432/%s?sslmode=disable" (include "gardenio.postgres.fullname" .) (.Values.postgres.database | default "beads") -}}
{{- end -}}
{{- end }}

{{/*
Beads rotation-tolerant password FILE mount (ka-0000004150 / P0 ka-0000006811).
When beads.databasePasswordFile.enabled, the daemon reads the DB password from a
mounted K8s Secret volume (which live-updates on the kubelet sync loop) instead
of a frozen secretKeyRef env var, so an RDS master-password rotation reconnects
in-process without a CrashLoop. These helpers resolve the Secret name + the RAW
(NOT url-encoded) password key to mount — the daemon URL-encodes the password
itself, so it must read the literal value.
*/}}
{{- define "gardenio.beads.rawPasswordSecretName" -}}
{{- if .Values.beads.externalRDS.enabled -}}
{{- include "gardenio.beads.externalRDS.passwordSecretName" . -}}
{{- else if .Values.postgresExternal.enabled -}}
{{- include "gardenio.beads.postgresExternal.passwordSecretName" . -}}
{{- else if .Values.postgres.enabled -}}
{{- .Values.postgres.passwordSecret | default (include "gardenio.postgres.passwordSecretName" .) -}}
{{- end -}}
{{- end }}

{{- define "gardenio.beads.rawPasswordSecretKey" -}}
{{- if .Values.beads.externalRDS.enabled -}}
password
{{- else if .Values.postgresExternal.enabled -}}
{{- .Values.postgresExternal.passwordSecret.key | default "password" -}}
{{- else if .Values.postgres.enabled -}}
password
{{- end -}}
{{- end }}

{{/*
postgresExternal — name of the in-cluster K8s Secret holding the external
postgres user's password. Defaults to "<release>-postgres-external-password".
The operator's wiring layer (e.g. terraform ExternalSecret) is responsible for
materializing this Secret from AWS Secrets Manager before the chart installs.
*/}}
{{- define "gardenio.beads.postgresExternal.passwordSecretName" -}}
{{- if .Values.postgresExternal.passwordSecret.name -}}
{{- .Values.postgresExternal.passwordSecret.name -}}
{{- else -}}
{{- printf "%s-postgres-external-password" .Release.Name -}}
{{- end -}}
{{- end }}

{{/*
postgresExternal — the Secret key holding a URL-ENCODED copy of the external
postgres password, for embedding into connection-string env vars
(BEADS_DATABASE_URL / litellm DATABASE_URL). Defaults to
`postgresExternal.passwordSecret.urlKey`, falling back to the literal `key`
when `urlKey` is unset so single-key Secrets keep working unchanged. The
literal `key` is reserved for PGPASSWORD-style consumers that must NOT receive
a URL-encoded value (ka-0000005248).
*/}}
{{- define "gardenio.beads.postgresExternal.passwordSecretURLKey" -}}
{{- if .Values.postgresExternal.passwordSecret.urlKey -}}
{{- .Values.postgresExternal.passwordSecret.urlKey -}}
{{- else -}}
{{- .Values.postgresExternal.passwordSecret.key | default "password" -}}
{{- end -}}
{{- end }}

{{/*
External RDS — per-instance database name. Defaults to gardenio_<release> with
hyphens collapsed to underscores so the identifier is a legal Postgres name.
*/}}
{{- define "gardenio.beads.externalRDS.databaseName" -}}
{{- if .Values.beads.externalRDS.databaseName -}}
{{- .Values.beads.externalRDS.databaseName -}}
{{- else -}}
{{- printf "gardenio_%s" (.Release.Name | replace "-" "_" | trunc 50 | trimSuffix "_") -}}
{{- end -}}
{{- end }}

{{/*
External RDS — per-instance role name. Defaults to the same identifier as
the database name (one role owns the one database).
*/}}
{{- define "gardenio.beads.externalRDS.roleName" -}}
{{- if .Values.beads.externalRDS.roleName -}}
{{- .Values.beads.externalRDS.roleName -}}
{{- else -}}
{{- include "gardenio.beads.externalRDS.databaseName" . -}}
{{- end -}}
{{- end }}

{{/*
External RDS — name of the K8s Secret holding the per-instance role's
password (key: "password"). Defaults to "<release>-rds-instance-password".
*/}}
{{- define "gardenio.beads.externalRDS.passwordSecretName" -}}
{{- if .Values.beads.externalRDS.passwordSecret -}}
{{- .Values.beads.externalRDS.passwordSecret -}}
{{- else -}}
{{- printf "%s-rds-instance-password" .Release.Name -}}
{{- end -}}
{{- end }}

{{/*
External RDS — per-instance role password. Looks up an existing K8s Secret in
the release namespace and reuses its `password` key when present, otherwise
generates a fresh 32-char random password. This makes the chart idempotent
across `helm upgrade` runs (the password is stable once first written) without
requiring an out-of-band Secret.

Note: `lookup` returns nil during `helm template` (no API server). The fall-
through path covers that — the rendered manifests then carry a fresh random
password. On a real install/upgrade, lookup runs against the live cluster.
*/}}
{{- define "gardenio.beads.externalRDS.password" -}}
{{- $secretName := include "gardenio.beads.externalRDS.passwordSecretName" . -}}
{{- $existing := lookup "v1" "Secret" .Release.Namespace $secretName -}}
{{- if and $existing $existing.data (index $existing.data "password") -}}
{{- index $existing.data "password" | b64dec -}}
{{- else -}}
{{- randAlphaNum 32 -}}
{{- end -}}
{{- end }}

{{/*
Beads password secret name. Resolves to the external-RDS per-instance
password Secret when externalRDS is enabled; otherwise the in-cluster
postgres password Secret.
*/}}
{{- define "gardenio.beads.passwordSecretName" -}}
{{- if .Values.beads.externalRDS.enabled -}}
{{- include "gardenio.beads.externalRDS.passwordSecretName" . }}
{{- else -}}
{{- include "gardenio.postgres.passwordSecretName" . }}
{{- end -}}
{{- end }}

{{/*
Beads token secret name (daemon auth token)
*/}}
{{- define "gardenio.beads.tokenSecretName" -}}
{{- .Values.beads.tokenSecret }}
{{- end }}

{{/* ===== Keycloak runtime (in-cluster Keycloak server) helpers ===== */}}

{{/*
Keycloak runtime fully qualified name
*/}}
{{- define "gardenio.keycloakRuntime.fullname" -}}
{{- printf "%s-keycloak-runtime" (include "gardenio.fullname" .) | trunc 63 | trimSuffix "-" }}
{{- end }}

{{/*
Keycloak runtime labels
*/}}
{{- define "gardenio.keycloakRuntime.labels" -}}
{{ include "gardenio.labels" . }}
app.kubernetes.io/component: keycloak-runtime
{{- end }}

{{/*
Keycloak runtime theme ConfigMap name. The chart does NOT render this
ConfigMap (gd-qiUiXvTX0k: the theme tree was stored twice in the 1 MiB Helm
release Secret, as chart files and as the rendered ConfigMap, and tipped
release 2026.292.1 over the limit). The release deployer applies it from
gardenio/keycloak-theme before every `helm upgrade`
(gardenio/scripts/keycloak-theme-configmap.py); the StatefulSet mounts it by
this name. keycloak.runtime.theme.configMapName overrides the default
"<fullname>-theme-files", which is also the generator's default for the same
release name.
*/}}
{{- define "gardenio.keycloakRuntime.themeConfigMapName" -}}
{{- $theme := .Values.keycloak.runtime.theme | default dict -}}
{{- $theme.configMapName | default (printf "%s-theme-files" (include "gardenio.keycloakRuntime.fullname" .)) -}}
{{- end }}

{{/*
Keycloak runtime theme checksum: sha256 over the theme FILES only (one
"<path> <sha256 of content>" line per file, paths sorted), never over a
rendered manifest. Keycloak caches theme templates in production, so the
StatefulSet pod template carries this as checksum/theme to restart the pod on a
real theme edit and on nothing else (gd-cW3og8FqhN: hashing a rendered object
pulled in its helm.sh/chart label and restarted Keycloak on every calver
release). The generator computes it and stamps it on the theme ConfigMap as the
gardenio.dev/theme-checksum annotation; this reads it back from the live
ConfigMap at install/upgrade time (`lookup`), so a plain `helm upgrade` after
the deployer applied a changed theme restarts the pod. keycloak.runtime.theme.
checksum overrides the lookup (tests; `helm template` has no cluster and
renders "" otherwise).
*/}}
{{- define "gardenio.keycloakRuntime.themeChecksum" -}}
{{- $theme := .Values.keycloak.runtime.theme | default dict -}}
{{- $sum := $theme.checksum | default "" -}}
{{- if not $sum -}}
{{- $cm := lookup "v1" "ConfigMap" .Release.Namespace (include "gardenio.keycloakRuntime.themeConfigMapName" .) -}}
{{- if $cm -}}
{{- $sum = index ($cm.metadata.annotations | default dict) "gardenio.dev/theme-checksum" | default "" -}}
{{- end -}}
{{- end -}}
{{- $sum -}}
{{- end }}

{{/*
Keycloak runtime selector labels
*/}}
{{- define "gardenio.keycloakRuntime.selectorLabels" -}}
{{ include "gardenio.selectorLabels" . }}
app.kubernetes.io/component: keycloak-runtime
{{- end }}

{{/*
Keycloak role-drift probe CronJob name (keycloak-runtime/role-drift-cronjob.yaml).
The vmalert rule selects on it, so both read it from here.
*/}}
{{- /*
"true" when the role-drift probe renders: the in-box Keycloak and the probe are
on, and no keycloak.realm.defaultGroups entry is declared with a gardenio-*
realm role. A realm that grants one to every new user on purpose has nothing to
drift from, and the probe would fail on every run (gd-L2MyfEe9lL).
*/ -}}
{{- define "gardenio.keycloakRuntime.roleDriftProbe.on" -}}
{{- if and .Values.keycloak.enabled .Values.keycloak.runtime.enabled .Values.keycloak.runtime.roleDriftProbe.enabled -}}
{{- if not (include "gardenio.keycloak.defaultGroupsGrantGardenioRole" .) -}}true{{- end -}}
{{- end -}}
{{- end }}

{{- /* "true" when a keycloak.realm.defaultGroups entry names a keycloak.groups
group holding a gardenio-* realm role. */ -}}
{{- define "gardenio.keycloak.defaultGroupsGrantGardenioRole" -}}
{{- $defaults := .Values.keycloak.realm.defaultGroups | default list -}}
{{- $grants := false -}}
{{- range .Values.keycloak.groups | default list -}}
{{- if has .name $defaults -}}
{{- range .realmRoles | default list -}}
{{- if hasPrefix "gardenio-" . -}}{{- $grants = true -}}{{- end -}}
{{- end -}}
{{- end -}}
{{- end -}}
{{- if $grants -}}true{{- end -}}
{{- end }}

{{- /* The role-drift probe's client Secret, and its secret: generated once,
then read back with lookup so an upgrade never rotates it under Keycloak. */ -}}
{{- define "gardenio.keycloakRuntime.roleDriftProbe.clientSecretName" -}}
{{- printf "%s-role-drift-client" (include "gardenio.keycloakRuntime.fullname" .) | trunc 63 | trimSuffix "-" -}}
{{- end }}

{{- define "gardenio.keycloakRuntime.roleDriftProbe.clientSecret" -}}
{{- $existing := lookup "v1" "Secret" .Release.Namespace (include "gardenio.keycloakRuntime.roleDriftProbe.clientSecretName" .) -}}
{{- if and $existing $existing.data (index $existing.data "clientSecret") -}}
{{- index $existing.data "clientSecret" | b64dec -}}
{{- else -}}
{{- randAlphaNum 40 -}}
{{- end -}}
{{- end }}

{{- /* The role-drift probe's keycloak.clients-shaped entry, which
keycloak/clients.yaml appends while the probe renders: a confidential client
whose service account holds realm-management view-realm only (gd-TgsaQWJPQ3). */ -}}
{{- define "gardenio.keycloakRuntime.roleDriftProbe.client" -}}
clientId: {{ .Values.keycloak.runtime.roleDriftProbe.clientId | quote }}
name: "Gardenio role-drift probe (reads default-roles, view-realm only)"
public: false
standardFlowEnabled: false
directAccess: false
secret: {{ printf "$%s:clientSecret" (include "gardenio.keycloakRuntime.roleDriftProbe.clientSecretName" .) | quote }}
serviceAccount:
  enabled: true
  realmRoles: []
  clientRoles:
    - clientId: realm-management
      roles: [view-realm]
{{- end }}

{{- define "gardenio.keycloakRuntime.roleDriftProbe.name" -}}
{{- printf "%s-role-drift" (include "gardenio.keycloakRuntime.fullname" .) | trunc 52 | trimSuffix "-" -}}
{{- end }}

{{/*
Keycloak realm OIDC issuer URL.

Returns keycloak.realm.issuerURL verbatim when set (manual overlay / external
Keycloak). Otherwise, when the in-box runtime is enabled, derives it from the
runtime ingress: https://<runtime.host><runtime.httpRelativePath>/realms/<realm>
where <realm> = realm.name or .Release.Name, mirroring realm.yaml's
(.Values.keycloak.realm.name | default .Release.Name) idiom. Scheme is always
https: Keycloak runs HTTP behind Traefik (TLS terminated there); KC_HOSTNAME +
proxyHeaders=xforwarded make Keycloak render https issuer URLs, so the derived
value must match. Returns "" when neither is set, preserving the
"no issuer => skip env var" behaviour in the consumers. (ka-0000004234 §7.4)

When keycloak.runtime.hostnameUrl is set it is KC_HOSTNAME, and Keycloak stamps
THAT (full URL, relative path included) into every issuer, whatever host the
caller dialed. The derived issuer follows it: hostnameUrl + /realms/<realm>.
Deriving from runtime.host instead yields https://<host>/... which is neither
what Keycloak stamps nor, for an in-cluster Service host, reachable (gd-l1Jh9aFEXY).
*/}}
{{- define "gardenio.keycloak.issuerURL" -}}
{{- if .Values.keycloak.realm.issuerURL -}}
{{- .Values.keycloak.realm.issuerURL -}}
{{- else if and .Values.keycloak.runtime.enabled .Values.keycloak.runtime.hostnameUrl -}}
{{- $realm := .Values.keycloak.realm.name | default .Release.Name -}}
{{- printf "%s/realms/%s" (.Values.keycloak.runtime.hostnameUrl | trimSuffix "/") $realm -}}
{{- else if and .Values.keycloak.runtime.enabled .Values.keycloak.runtime.host -}}
{{- $realm := .Values.keycloak.realm.name | default .Release.Name -}}
{{- $path := .Values.keycloak.runtime.httpRelativePath | default "" | trimSuffix "/" -}}
{{- printf "https://%s%s/realms/%s" .Values.keycloak.runtime.host $path $realm -}}
{{- end -}}
{{- end }}

{{/*
In-cluster OIDC discovery base URL for the Keycloak realm, or "" when the issuer
is directly dialable.

Only needed when keycloak.runtime.hostnameUrl pins the issuer to a public
hostname: the daemon must still validate iss == that public issuer, but it
should fetch the discovery document over the Service, not by dialing the
public edge. Points at
https://<runtime Service>.<ns>.svc:<service.httpsPort><httpRelativePath>/realms/<realm>
when gardenio.keycloakRuntime.clientsTLS (the client trusts the CA mounted by
gardenio.keycloakRuntime.caVolume, gd-lYPZlslOM7), and at the plain-http
http://<runtime Service>.<ns>.svc:<service.httpPort>... otherwise.
Consumers pass it as BEADS_OIDC_DISCOVERY_URL. (gd-l1Jh9aFEXY)
*/}}
{{- define "gardenio.keycloak.discoveryURL" -}}
{{- if and .Values.keycloak.runtime.enabled .Values.keycloak.runtime.hostnameUrl (not .Values.keycloak.realm.issuerURL) -}}
{{- $realm := .Values.keycloak.realm.name | default .Release.Name -}}
{{- $path := .Values.keycloak.runtime.httpRelativePath | default "" | trimSuffix "/" -}}
{{- if include "gardenio.keycloakRuntime.clientsTLS" . -}}
{{- $port := .Values.keycloak.runtime.service.httpsPort | default 8443 -}}
{{- printf "https://%s.%s.svc:%v%s/realms/%s" (include "gardenio.keycloakRuntime.fullname" .) .Release.Namespace $port $path $realm -}}
{{- else -}}
{{- $port := .Values.keycloak.runtime.service.httpPort | default 8080 -}}
{{- printf "http://%s.%s.svc:%v%s/realms/%s" (include "gardenio.keycloakRuntime.fullname" .) .Release.Namespace $port $path $realm -}}
{{- end -}}
{{- end -}}
{{- end }}

{{/*
Whether the in-cluster OIDC endpoints (gardenio.keycloak.discoveryURL, and the
tokenURL derived from it) are the https Service port, so a consumer must mount
the keycloak-runtime CA and point its *_CA_FILE env at it
(gardenio.keycloakRuntime.caFile). "true" or "". When the realm base is the
public issuer instead, the system roots already trust it and no CA is mounted.
*/}}
{{- define "gardenio.keycloak.inClusterCA" -}}
{{- if and (include "gardenio.keycloakRuntime.clientsTLS" .) (include "gardenio.keycloak.discoveryURL" .) -}}true{{- end -}}
{{- end }}

{{/*
OAuth 2.0 token endpoint a platform component dials for its own machine
principal (client-credentials grant, gd-s7aPIf0Bgp). The realm base is the
in-cluster discoveryURL when one exists (the Service, never the public edge or
a ziti host) and the issuerURL otherwise, plus Keycloak's fixed
/protocol/openid-connect/token path. Consumers pass it as BEADS_OIDC_TOKEN_URL.
*/}}
{{- define "gardenio.keycloak.tokenURL" -}}
{{- $base := include "gardenio.keycloak.discoveryURL" . -}}
{{- if not $base -}}
{{- $base = include "gardenio.keycloak.issuerURL" . -}}
{{- end -}}
{{- if $base -}}
{{- printf "%s/protocol/openid-connect/token" ($base | trimSuffix "/") -}}
{{- end -}}
{{- end }}

{{/*
Keycloak base URL the Lands provisioner's admin client dials when
agents.lands.provisioner.keycloak.url is empty: the keycloak-runtime Service
over in-cluster TLS (gd-XRsosVFAau),
https://<runtime Service>.<ns>.svc.cluster.local:<service.httpsPort><httpRelativePath>,
with no /realms suffix (keycloakadmin appends /admin/realms/<realm> and
/realms/<realm>/protocol/...). "" unless the runtime serves https
(gardenio.keycloakRuntime.tlsEnabled): keycloakadmin refuses plain http.
*/}}
{{- define "gardenio.keycloak.adminBaseURL" -}}
{{- if include "gardenio.keycloakRuntime.tlsEnabled" . -}}
{{- $path := .Values.keycloak.runtime.httpRelativePath | default "" | trimSuffix "/" -}}
{{- $port := .Values.keycloak.runtime.service.httpsPort | default 8443 -}}
{{- printf "https://%s.%s.svc.cluster.local:%v%s" (include "gardenio.keycloakRuntime.fullname" .) .Release.Namespace $port $path -}}
{{- end -}}
{{- end }}

{{/*
keycloak-runtime in-cluster TLS (gd-XRsosVFAau): the Secret cert-manager
writes the Service's leaf cert to (tls.crt, tls.key and ca.crt, the CA that
signed it). Keycloak mounts tls.crt/tls.key; every client mounts ca.crt
only.
*/}}
{{- define "gardenio.keycloakRuntime.tlsSecretName" -}}
{{- printf "%s-tls" (include "gardenio.keycloakRuntime.fullname" .) -}}
{{- end }}

{{/*
Whether the keycloak-runtime serves in-cluster https: keycloak, its runtime
and keycloak.runtime.tls all on. "true" or "".
*/}}
{{- define "gardenio.keycloakRuntime.tlsEnabled" -}}
{{- if and .Values.keycloak.enabled .Values.keycloak.runtime.enabled .Values.keycloak.runtime.tls.enabled -}}true{{- end -}}
{{- end }}

{{/*
Whether the chart's other in-cluster Keycloak clients use the https port too
(gd-lYPZlslOM7): the runtime serves https (tlsEnabled) and
keycloak.runtime.tls.inClusterClients is not false. The machine-principal
token fetches, the beads daemon's discovery, and the first-admin kcadm Job
then dial service.httpsPort and trust the CA, and the http port admits only
Traefik (and the EDP operator while its ClusterKeycloak dials http, see
clusterKeycloakTLS). "true" or "".
*/}}
{{- define "gardenio.keycloakRuntime.clientsTLS" -}}
{{- if and (include "gardenio.keycloakRuntime.tlsEnabled" .) (ne (toString .Values.keycloak.runtime.tls.inClusterClients) "false") -}}true{{- end -}}
{{- end }}

{{/*
Whether the EDP keycloak-operator's ClusterKeycloak dials the https port
(gd-PhU6jPtSxa): the runtime serves https (tlsEnabled) and
keycloak.runtime.clusterKeycloak.tls is on. The operator then trusts the CA
from the ConfigMap caConfigMapName renders in its own namespace, and the http
port no longer admits it. "true" or "".
*/}}
{{- define "gardenio.keycloakRuntime.clusterKeycloakTLS" -}}
{{- if and (include "gardenio.keycloakRuntime.tlsEnabled" .) .Values.keycloak.runtime.clusterKeycloak.tls -}}true{{- end -}}
{{- end }}

{{/*
The ConfigMap holding the keycloak-runtime CA (key ca.crt) in the EDP
operator's namespace, for ClusterKeycloak spec.caCert. Not "<fullname>-ca":
that is the CA's own Secret, private key included.
*/}}
{{- define "gardenio.keycloakRuntime.caConfigMapName" -}}
{{- printf "%s-ca-bundle" (include "gardenio.keycloakRuntime.fullname" .) -}}
{{- end }}

{{/*
Whether a machine-principal consumer fetches its token from the
keycloak-runtime https port, so the NetworkPolicies must let it reach 8443
(gd-PhU6jPtSxa). Takes (dict "root" $ "c" <component values>), where the
component has enabled and machinePrincipal.enabled. "true" or "".
*/}}
{{- define "gardenio.keycloakRuntime.machineHTTPS" -}}
{{- if and .c.enabled .c.machinePrincipal.enabled (include "gardenio.keycloak.inClusterCA" .root) -}}true{{- end -}}
{{- end }}

{{/*
The keycloak-runtime CA for an in-cluster https client (gd-lYPZlslOM7):
caFile is the path a consumer's *_CA_FILE env names, caVolumeMount and
caVolume mount ca.crt ONLY from the leaf Secret (never tls.key). optional, so
a pod starts before cert-manager has issued: its token fetch then fails
closed (auth_unavailable) and retries until kubelet projects the file.
Callers gate all three on gardenio.keycloak.inClusterCA.
*/}}
{{- define "gardenio.keycloakRuntime.caFile" -}}
{{- printf "%s/ca.crt" (.Values.keycloak.runtime.tls.caMountPath | default "/etc/gardenio/keycloak-runtime-ca" | trimSuffix "/") -}}
{{- end }}

{{- define "gardenio.keycloakRuntime.caVolumeMount" -}}
- name: keycloak-runtime-ca
  mountPath: {{ .Values.keycloak.runtime.tls.caMountPath | default "/etc/gardenio/keycloak-runtime-ca" | quote }}
  readOnly: true
{{- end }}

{{- define "gardenio.keycloakRuntime.caVolume" -}}
- name: keycloak-runtime-ca
  secret:
    secretName: {{ include "gardenio.keycloakRuntime.tlsSecretName" . | quote }}
    optional: true
    defaultMode: 0444
    items:
      - key: ca.crt
        path: ca.crt
{{- end }}

{{/*
ForwardAuth (oauth2-proxy) OIDC issuer URL.

Returns keycloak.forwardAuth.oidcIssuerUrl verbatim when set — this is the
prod/staging/CI path, where the issuer is pinned explicitly (gitlab.com for the
GitLab-direct front door, or the external Keycloak realm for ephemeral CI). Only
when it is left EMPTY *and* the in-box Keycloak runtime is enabled does this fall
back to the realm issuer derived from runtime.host (reusing
gardenio.keycloak.issuerURL). That is the "box auth profile" path (ka-0000006087,
doc §0/§6): the air-gapped box has no gitlab.com to federate to, so the human
edge must point oauth2-proxy at the in-cluster Keycloak realm — and the operator
should not have to hand-copy the issuer host/realm that the runtime block already
declares. Returns "" when neither is set; the forwardauth deployment's https
fail-fast then refuses to render, surfacing the misconfiguration loudly.
*/}}
{{- define "gardenio.forwardAuth.issuerURL" -}}
{{- if .Values.keycloak.forwardAuth.oidcIssuerUrl -}}
{{- .Values.keycloak.forwardAuth.oidcIssuerUrl -}}
{{- else if and .Values.keycloak.runtime.enabled .Values.keycloak.runtime.host -}}
{{- include "gardenio.keycloak.issuerURL" . -}}
{{- end -}}
{{- end }}

{{/*
Keycloak runtime bootstrap-admin password. Looks up the existing admin Secret
in the release namespace and reuses its password key when present, otherwise
generates a fresh 32-char random password. Makes the chart idempotent across
`helm upgrade` runs (the password is stable once first written).

Stability requires `helm install/upgrade` against a live cluster; `lookup`
returns nil under `helm template`, so offline `helm template | kubectl apply`
would rotate the password on every apply. Precedent:
gardenio.beads.externalRDS.password. (ka-0000004234 §7.3)
*/}}
{{- define "gardenio.keycloakRuntime.admin.password" -}}
{{- $runtime := .Values.keycloak.runtime -}}
{{- $secretName := $runtime.admin.existingSecret -}}
{{- $passwordKey := $runtime.admin.passwordKey | default "password" -}}
{{- $existing := lookup "v1" "Secret" .Release.Namespace $secretName -}}
{{- if and $existing $existing.data (index $existing.data $passwordKey) -}}
{{- index $existing.data $passwordKey | b64dec -}}
{{- else -}}
{{- randAlphaNum 32 -}}
{{- end -}}
{{- end }}

{{/*
First-admin Secret name. Defaults to "<release>-keycloak-first-admin" when
keycloak.runtime.firstAdmin.passwordSecret.name is empty.
*/}}
{{- define "gardenio.keycloakRuntime.firstAdmin.secretName" -}}
{{- $fa := .Values.keycloak.runtime.firstAdmin -}}
{{- $fa.passwordSecret.name | default (printf "%s-keycloak-first-admin" (include "gardenio.fullname" .)) -}}
{{- end }}

{{/*
First-admin realm username/email. Defaults to "admin@<runtime.host>" when
keycloak.runtime.firstAdmin.username is empty. The realm has emailAsUsername=true
so this doubles as the login email.
*/}}
{{- define "gardenio.keycloakRuntime.firstAdmin.username" -}}
{{- $runtime := .Values.keycloak.runtime -}}
{{- $runtime.firstAdmin.username | default (printf "admin@%s" $runtime.host) -}}
{{- end }}

{{/*
First-admin bootstrap password. Looks up the existing first-admin Secret in the
release namespace and reuses its password key when present, otherwise generates
a fresh 32-char random password. Same idempotency contract as the master-admin
helper above: stable once first written under `helm install/upgrade` against a
live cluster (`lookup` returns nil under `helm template`).
*/}}
{{- define "gardenio.keycloakRuntime.firstAdmin.password" -}}
{{- $fa := .Values.keycloak.runtime.firstAdmin -}}
{{- $secretName := include "gardenio.keycloakRuntime.firstAdmin.secretName" . -}}
{{- $passwordKey := $fa.passwordSecret.passwordKey | default "password" -}}
{{- $existing := lookup "v1" "Secret" .Release.Namespace $secretName -}}
{{- if and $existing $existing.data (index $existing.data $passwordKey) -}}
{{- index $existing.data $passwordKey | b64dec -}}
{{- else -}}
{{- /*
     BUG FIX (2026-09-19): this was `randAlphaNum 32`, which can NEVER satisfy
     this chart's own DEFAULT realm passwordPolicy —
       length(8) and upperCase(1) and lowerCase(1) and specialChars(1) and digits(1)
     — because randAlphaNum emits no special characters. The bootstrap Job's
     `kcadm set-password` was therefore rejected with
       "Invalid password: must contain at least 1 special characters"
     on every fresh install. The Job discarded that output and did not check
     the exit code, so it reported success while leaving the realm admin with
     an EMPTY credentials list: login then failed "Invalid username or
     password" and the box was locked read-only — precisely the hard lock-out
     this whole first-admin mechanism exists to prevent (ka-0000004895 §3).
     Verified in-cluster before and after.

     randAlphaNum 32 supplies the entropy and (with overwhelming probability,
     and guaranteed in practice at this length) the upper/lower/digit classes;
     the appended "!aA1" makes the special-character, upper, lower and digit
     requirements unconditional rather than probabilistic. This remains a
     ONE-TIME installer password that the operator retrieves and rotates.
*/ -}}
{{- printf "%s!aA1" (randAlphaNum 32) -}}
{{- end -}}
{{- end }}

{{/* ===== NATS component helpers ===== */}}

{{/*
NATS fully qualified name
*/}}
{{- define "gardenio.nats.fullname" -}}
{{- printf "%s-nats" (include "gardenio.fullname" .) | trunc 63 | trimSuffix "-" }}
{{- end }}

{{/*
NATS labels
*/}}
{{- define "gardenio.nats.labels" -}}
{{ include "gardenio.labels" . }}
app.kubernetes.io/component: nats
{{- end }}

{{/*
NATS selector labels
*/}}
{{- define "gardenio.nats.selectorLabels" -}}
{{ include "gardenio.selectorLabels" . }}
app.kubernetes.io/component: nats
{{- end }}

{{/*
NATS URL — returns nats://{svc}:4222 when nats.enabled or fleet.enabled
(fleet mode creates an ExternalName service with the same name).
*/}}
{{- define "gardenio.natsURL" -}}
{{- if or .Values.nats.enabled .Values.fleet.enabled -}}
{{- printf "nats://%s:4222" (include "gardenio.nats.fullname" .) -}}
{{- end -}}
{{- end }}

{{/* ===== Landing Page component helpers ===== */}}

{{/*
Landing Page fully qualified name
*/}}
{{- define "gardenio.landing.fullname" -}}
{{- printf "%s-landing" (include "gardenio.fullname" .) | trunc 63 | trimSuffix "-" }}
{{- end }}

{{/*
Landing Page labels
*/}}
{{- define "gardenio.landing.labels" -}}
{{ include "gardenio.labels" . }}
app.kubernetes.io/component: landing
{{- end }}

{{/*
Landing Page selector labels
*/}}
{{- define "gardenio.landing.selectorLabels" -}}
{{ include "gardenio.selectorLabels" . }}
app.kubernetes.io/component: landing
{{- end }}

{{/* ===== VictoriaMetrics (vmsingle) component helpers ===== */}}

{{/*
VictoriaMetrics fully qualified name
*/}}
{{- define "gardenio.vmsingle.fullname" -}}
{{- printf "%s-vmsingle" (include "gardenio.fullname" .) | trunc 63 | trimSuffix "-" }}
{{- end }}

{{/*
VictoriaMetrics labels
*/}}
{{- define "gardenio.vmsingle.labels" -}}
{{ include "gardenio.labels" . }}
app.kubernetes.io/component: vmsingle
{{- end }}

{{/*
VictoriaMetrics selector labels
*/}}
{{- define "gardenio.vmsingle.selectorLabels" -}}
{{ include "gardenio.selectorLabels" . }}
app.kubernetes.io/component: vmsingle
{{- end }}

{{/*
VictoriaMetrics write URL — used by Prometheus remoteWrite config.
*/}}
{{- define "gardenio.vmsingle.writeURL" -}}
{{- printf "http://%s:8428/api/v1/write" (include "gardenio.vmsingle.fullname" .) -}}
{{- end }}

{{/* ===== VictoriaTraces (vtsingle) component helpers ===== */}}

{{/*
VictoriaTraces fully qualified name
*/}}
{{- define "gardenio.vtsingle.fullname" -}}
{{- printf "%s-victoria-traces" (include "gardenio.fullname" .) | trunc 63 | trimSuffix "-" }}
{{- end }}

{{/*
VictoriaTraces labels
*/}}
{{- define "gardenio.vtsingle.labels" -}}
{{ include "gardenio.labels" . }}
app.kubernetes.io/component: vtsingle
{{- end }}

{{/*
VictoriaTraces selector labels
*/}}
{{- define "gardenio.vtsingle.selectorLabels" -}}
{{ include "gardenio.selectorLabels" . }}
app.kubernetes.io/component: vtsingle
{{- end }}

{{/*
VictoriaTraces base URL — the single HTTP listener that serves both
OTLP/HTTP ingest (/insert/opentelemetry/v1/traces) and the Jaeger/native
select API (/select/jaeger, /select/vmui). Used by the Grafana Jaeger
datasource and the perf-traces relay (sibling tasks).
*/}}
{{- define "gardenio.vtsingle.baseURL" -}}
{{- printf "http://%s:10428" (include "gardenio.vtsingle.fullname" .) -}}
{{- end }}

{{/*
perf-traces relay fully qualified name (PULL transport, task ka-0000004875).
*/}}
{{- define "gardenio.vtsingle.relay.fullname" -}}
{{- printf "%s-perf-traces-relay" (include "gardenio.fullname" .) | trunc 63 | trimSuffix "-" }}
{{- end }}

{{/*
perf-traces relay labels
*/}}
{{- define "gardenio.vtsingle.relay.labels" -}}
{{ include "gardenio.labels" . }}
app.kubernetes.io/component: perf-traces-relay
{{- end }}

{{/*
perf-traces relay selector labels
*/}}
{{- define "gardenio.vtsingle.relay.selectorLabels" -}}
{{ include "gardenio.selectorLabels" . }}
app.kubernetes.io/component: perf-traces-relay
{{- end }}

{{/*
perf-traces relay image reference. Defaults to the controller image
(agents.image) since the relay IS a controller subcommand reusing that image;
vtsingle.relay.image.{repository,tag,digest} override per-field when set.
*/}}
{{- define "gardenio.vtsingle.relay.image" -}}
{{- $relay := .Values.vtsingle.relay.image -}}
{{- $base := .Values.agents.image -}}
{{- $repo := $base.repository -}}
{{- if $relay.repository }}{{ $repo = $relay.repository }}{{ end -}}
{{- $tag := $base.tag -}}
{{- if $relay.tag }}{{ $tag = $relay.tag }}{{ end -}}
{{- $digest := $base.digest -}}
{{- if $relay.digest }}{{ $digest = $relay.digest }}{{ end -}}
{{- $img := dict "repository" $repo "tag" $tag "digest" $digest -}}
{{- include "gardenio.imageRef" (dict "image" $img "global" .Values.global "Chart" .Chart) -}}
{{- end }}

{{/*
dev-traces relay fully qualified name (PULL transport, task ka-0000003142).
*/}}
{{- define "gardenio.vtsingle.devRelay.fullname" -}}
{{- printf "%s-dev-traces-relay" (include "gardenio.fullname" .) | trunc 63 | trimSuffix "-" }}
{{- end }}

{{/*
dev-traces relay labels
*/}}
{{- define "gardenio.vtsingle.devRelay.labels" -}}
{{ include "gardenio.labels" . }}
app.kubernetes.io/component: dev-traces-relay
{{- end }}

{{/*
dev-traces relay selector labels
*/}}
{{- define "gardenio.vtsingle.devRelay.selectorLabels" -}}
{{ include "gardenio.selectorLabels" . }}
app.kubernetes.io/component: dev-traces-relay
{{- end }}

{{/*
dev-traces relay image reference. Identical resolution to the perf relay —
defaults to the controller image (agents.image) since the relay IS a controller
subcommand reusing that image; vtsingle.devRelay.image.{repository,tag,digest}
override per-field when set.
*/}}
{{- define "gardenio.vtsingle.devRelay.image" -}}
{{- $relay := .Values.vtsingle.devRelay.image -}}
{{- $base := .Values.agents.image -}}
{{- $repo := $base.repository -}}
{{- if $relay.repository }}{{ $repo = $relay.repository }}{{ end -}}
{{- $tag := $base.tag -}}
{{- if $relay.tag }}{{ $tag = $relay.tag }}{{ end -}}
{{- $digest := $base.digest -}}
{{- if $relay.digest }}{{ $digest = $relay.digest }}{{ end -}}
{{- $img := dict "repository" $repo "tag" $tag "digest" $digest -}}
{{- include "gardenio.imageRef" (dict "image" $img "global" .Values.global "Chart" .Chart) -}}
{{- end }}

{{/* ===== VictoriaLogs (vlsingle) component helpers =====

Single-node VictoriaLogs StatefulSet, mirroring the vmsingle/vtsingle shape (gp3
PVC, ClusterIP Service, securityContext, nodeSelector/tolerations helpers). It is
the log store the Phase-2 EKS-audit LogsQL detections (#3/#5/#6, ka-0000006485)
query: the vmalert-logs evaluator's -datasource.url auto-wires to this Service
(gardenio.vmalertLogs.datasourceURL default `gardenio-victoria-logs`:9428, so the
fullname below MUST resolve to that name on the prod release). Disabled by
default; the chart renders identically until an env opts in. Task ka-0000007261,
epic ka-0000007666. */}}

{{/*
VictoriaLogs fully qualified name. MUST match .Values.vmalertLogs.victoriaLogs.serviceName
(default `gardenio-victoria-logs`) on the prod release (name `gardenio`) so the
LogsQL rule evaluator's datasource resolves without an explicit override.
*/}}
{{- define "gardenio.vlsingle.fullname" -}}
{{- printf "%s-victoria-logs" (include "gardenio.fullname" .) | trunc 63 | trimSuffix "-" }}
{{- end }}

{{/*
VictoriaLogs labels
*/}}
{{- define "gardenio.vlsingle.labels" -}}
{{ include "gardenio.labels" . }}
app.kubernetes.io/component: vlsingle
{{- end }}

{{/*
VictoriaLogs selector labels
*/}}
{{- define "gardenio.vlsingle.selectorLabels" -}}
{{ include "gardenio.selectorLabels" . }}
app.kubernetes.io/component: vlsingle
{{- end }}

{{/*
VictoriaLogs base URL — the single HTTP listener on :9428 that serves BOTH the
ingest API (/insert/{jsonline,elasticsearch/_bulk,loki/api/v1/push}) AND the
LogsQL select API (/select/logsql/{query,stats_query}). Used by the log
shippers and any in-cluster reader.
*/}}
{{- define "gardenio.vlsingle.baseURL" -}}
{{- printf "http://%s:9428" (include "gardenio.vlsingle.fullname" .) -}}
{{- end }}

{{/* ===== Grafana component helpers ===== */}}

{{/*
Grafana fully qualified name
*/}}
{{- define "gardenio.grafana.fullname" -}}
{{- printf "%s-grafana" (include "gardenio.fullname" .) | trunc 63 | trimSuffix "-" }}
{{- end }}

{{/*
Grafana in-cluster Service URL (root, no /grafana subpath).

In-cluster the Grafana API is served at the Service ROOT — even when the public
ingress fronts Grafana under /grafana — because prod runs serveFromSubPath=false
with the subpath living only in GF_SERVER_ROOT_URL (appSubUrl) for generated
links; the stripPrefix(/grafana) middleware removes the prefix before the
request reaches the pod. So `<svc>:<port>/api/...` and `<svc>:<port>/render/...`
resolve, while `<svc>:<port>/grafana/api/...` 404s to the SPA. Callers that need
the in-cluster API base (renderer callback, GRAFANA_URL for `gb grafana render`)
must therefore use this ROOT URL, not one carrying the public subpath.
See bug ka-0000005842 §3.
*/}}
{{- define "gardenio.grafana.serviceURL" -}}
{{- printf "http://%s:%d" (include "gardenio.grafana.fullname" .) (int (.Values.grafana.service.port | default 3000)) -}}
{{- end }}

{{/*
Grafana in-cluster base URL HONORING serveFromSubPath (bug ka-0000001440).

This is the base that in-cluster, non-Traefik callers — the image-renderer
GF_RENDERING_CALLBACK_URL and the agent `gb grafana render` GRAFANA_URL — must
use to reach Grafana. Those callers hit the pod DIRECTLY: no unified-host
stripPrefix middleware sits in front, so the URL they use must match what the
pod actually serves:

  - serveFromSubPath=false (legacy "proxy strips the prefix" mode): the pod
    serves at the Service ROOT, so the in-cluster base is just <svc>:<port>.
  - serveFromSubPath=true  (Grafana owns the /grafana prefix end-to-end): the
    pod serves EVERYTHING under the subpath, so the in-cluster base MUST carry
    it: <svc>:<port>/grafana. Without this, the renderer's headless browser
    loads the SPA whose <base href> resolves to /grafana/ while it fetches
    assets from a base at root → every /grafana/public/build/*.js 404s →
    Grafana's "failed to load application files" page (the whole bug).

The subpath is the path component of the effective rootURL (explicit
grafana.rootURL wins), falling back to unifiedHost.paths.grafana.
*/}}
{{- define "gardenio.grafana.inClusterBaseURL" -}}
{{- $svc := include "gardenio.grafana.serviceURL" . -}}
{{- $sub := "" -}}
{{- if .Values.grafana.serveFromSubPath -}}
{{- $rootURL := .Values.grafana.rootURL | default "" -}}
{{- $unifiedPath := (.Values.unifiedHost.paths | default dict).grafana | default "" -}}
{{- if ne $rootURL "" -}}
{{- $sub = regexReplaceAll "^https?://[^/]+" $rootURL "" -}}
{{- else if ne $unifiedPath "" -}}
{{- $sub = $unifiedPath -}}
{{- end -}}
{{- end -}}
{{- $sub = $sub | trimSuffix "/" -}}
{{- printf "%s%s" $svc $sub -}}
{{- end }}

{{/*
Grafana port agent pods use (gd-RUhOpXWwmz): the header-stripping agentPath
port when it is on, else the Service port. Agents never get the trusted port
while the agent path is on.
*/}}
{{- define "gardenio.grafana.agentPort" -}}
{{- if .Values.grafana.auth.proxy.agentPath.enabled -}}
{{- int .Values.grafana.auth.proxy.agentPath.port -}}
{{- else -}}
{{- int (.Values.grafana.service.port | default 3000) -}}
{{- end -}}
{{- end }}

{{/*
Grafana in-cluster base URL for agent pods' GRAFANA_URL: inClusterBaseURL
(subpath included) on gardenio.grafana.agentPort.
*/}}
{{- define "gardenio.grafana.agentBaseURL" -}}
{{- $base := include "gardenio.grafana.inClusterBaseURL" . -}}
{{- $svc := include "gardenio.grafana.serviceURL" . -}}
{{- printf "http://%s:%s%s" (include "gardenio.grafana.fullname" .) (include "gardenio.grafana.agentPort" .) (trimPrefix $svc $base) -}}
{{- end }}

{{/*
Grafana labels
*/}}
{{- define "gardenio.grafana.labels" -}}
{{ include "gardenio.labels" . }}
app.kubernetes.io/component: grafana
{{- end }}

{{/*
Grafana selector labels
*/}}
{{- define "gardenio.grafana.selectorLabels" -}}
{{ include "gardenio.selectorLabels" . }}
app.kubernetes.io/component: grafana
{{- end }}

{{/* ===== vmagent component helpers ===== */}}

{{/*
vmagent fully qualified name
*/}}
{{- define "gardenio.vmagent.fullname" -}}
{{- printf "%s-vmagent" (include "gardenio.fullname" .) | trunc 63 | trimSuffix "-" }}
{{- end }}

{{/*
vmagent labels
*/}}
{{- define "gardenio.vmagent.labels" -}}
{{ include "gardenio.labels" . }}
app.kubernetes.io/component: vmagent
{{- end }}

{{/*
vmagent selector labels
*/}}
{{- define "gardenio.vmagent.selectorLabels" -}}
{{ include "gardenio.selectorLabels" . }}
app.kubernetes.io/component: vmagent
{{- end }}

{{/* ===== traefik-log-shipper (edge-IDP L0 transport) component helpers ===== */}}

{{/*
traefik-log-shipper fully qualified name (task ka-0000001563, epic
ka-0000008216). Streams the Traefik edge pod's JSON access log off the pod-log
API and POSTs it to VictoriaLogs /insert/jsonline.
*/}}
{{- define "gardenio.traefikLogShipper.fullname" -}}
{{- printf "%s-traefik-log-shipper" (include "gardenio.fullname" .) | trunc 63 | trimSuffix "-" }}
{{- end }}

{{/*
traefik-log-shipper labels
*/}}
{{- define "gardenio.traefikLogShipper.labels" -}}
{{ include "gardenio.labels" . }}
app.kubernetes.io/component: traefik-log-shipper
{{- end }}

{{/*
traefik-log-shipper selector labels
*/}}
{{- define "gardenio.traefikLogShipper.selectorLabels" -}}
{{ include "gardenio.selectorLabels" . }}
app.kubernetes.io/component: traefik-log-shipper
{{- end }}

{{/*
traefik-log-shipper VictoriaLogs BASE endpoint (scheme://host:port) — the
shipper appends /insert/jsonline + the stream/time query params itself. An
explicit .Values.traefikLogShipper.victoriaLogs.endpoint always wins; otherwise
auto-wire to the in-chart vlsingle Service (release-relative via
gardenio.vlsingle.baseURL, so it resolves on the prod `gardenio` release AND
the gardenio-e2e-* namespaces alike).
*/}}
{{- define "gardenio.traefikLogShipper.victoriaLogsEndpoint" -}}
{{- if .Values.traefikLogShipper.victoriaLogs.endpoint -}}
{{- .Values.traefikLogShipper.victoriaLogs.endpoint -}}
{{- else -}}
{{- include "gardenio.vlsingle.baseURL" . -}}
{{- end -}}
{{- end }}

{{/* ===== bridge-log-shipper (non-AVA webhook-audit transport) component helpers ===== */}}

{{/*
bridge-log-shipper fully qualified name (GAP-B of the webhook carve-out, task
ka-0000000320, epic ka-0000000914). Streams the jira-bridge + gitlab-bridge
webhook receive-path stdout off the Kubernetes pod-log API and POSTs it to
VictoriaLogs /insert/jsonline, so the two non-AVA webhook doors have a durable
audit-log pipeline.
*/}}
{{- define "gardenio.bridgeLogShipper.fullname" -}}
{{- printf "%s-bridge-log-shipper" (include "gardenio.fullname" .) | trunc 63 | trimSuffix "-" }}
{{- end }}

{{/*
bridge-log-shipper labels
*/}}
{{- define "gardenio.bridgeLogShipper.labels" -}}
{{ include "gardenio.labels" . }}
app.kubernetes.io/component: bridge-log-shipper
{{- end }}

{{/*
bridge-log-shipper selector labels
*/}}
{{- define "gardenio.bridgeLogShipper.selectorLabels" -}}
{{ include "gardenio.selectorLabels" . }}
app.kubernetes.io/component: bridge-log-shipper
{{- end }}

{{/*
bridge-log-shipper VictoriaLogs BASE endpoint (scheme://host:port) — the shipper
appends /insert/jsonline + the stream/time/msg query params itself. Same
precedence as the traefik-log-shipper resolver: an explicit
.Values.bridgeLogShipper.victoriaLogs.endpoint always wins; otherwise auto-wire
to the in-chart vlsingle Service (release-relative via gardenio.vlsingle.baseURL,
so it resolves on the prod `gardenio` release AND the gardenio-e2e-*
namespaces alike).
*/}}
{{- define "gardenio.bridgeLogShipper.victoriaLogsEndpoint" -}}
{{- if .Values.bridgeLogShipper.victoriaLogs.endpoint -}}
{{- .Values.bridgeLogShipper.victoriaLogs.endpoint -}}
{{- else -}}
{{- include "gardenio.vlsingle.baseURL" . -}}
{{- end -}}
{{- end }}

{{/* ===== core-log-shipper component helpers ===== */}}

{{/*
core-log-shipper fully qualified name (general gardenio-pod slog transport,
design ka-0000005579 option A step 3, task ka-0000005209). Streams the core
components' (controller / slack-bridge / trigger-engine / grafana-bridge) stdout
slog JSON off the Kubernetes pod-log API and POSTs it, SECRET-REDACTED, to
VictoriaLogs /insert/jsonline, so an agent can read those components' behavior
logs back through `gb logs <component>` with no pod-log RBAC of its own. Sibling
to bridge-log-shipper — same stream->filter->sink shape, broader source set,
plus a ship-time redaction gate. OFF BY DEFAULT, one selector per component.
*/}}
{{- define "gardenio.coreLogShipper.fullname" -}}
{{- printf "%s-core-log-shipper" (include "gardenio.fullname" .) | trunc 63 | trimSuffix "-" }}
{{- end }}

{{/*
core-log-shipper labels
*/}}
{{- define "gardenio.coreLogShipper.labels" -}}
{{ include "gardenio.labels" . }}
app.kubernetes.io/component: core-log-shipper
{{- end }}

{{/*
core-log-shipper selector labels
*/}}
{{- define "gardenio.coreLogShipper.selectorLabels" -}}
{{ include "gardenio.selectorLabels" . }}
app.kubernetes.io/component: core-log-shipper
{{- end }}

{{/*
core-log-shipper VictoriaLogs BASE endpoint (scheme://host:port) — the shipper
appends /insert/jsonline + the stream/time/msg query params itself. Same
precedence as the bridge-log-shipper resolver: an explicit
.Values.coreLogShipper.victoriaLogs.endpoint always wins; otherwise auto-wire to
the in-chart vlsingle Service (release-relative via gardenio.vlsingle.baseURL, so
it resolves on the prod `gardenio` release AND the gardenio-e2e-* namespaces
alike). This is the SAME store `gb logs` reads back from, so the read and write
sides stay release-relative and in lock-step.
*/}}
{{- define "gardenio.coreLogShipper.victoriaLogsEndpoint" -}}
{{- if .Values.coreLogShipper.victoriaLogs.endpoint -}}
{{- .Values.coreLogShipper.victoriaLogs.endpoint -}}
{{- else -}}
{{- include "gardenio.vlsingle.baseURL" . -}}
{{- end -}}
{{- end }}

{{/* ===== vmalert (recording-rule evaluator) component helpers ===== */}}

{{/*
vmalert fully qualified name
*/}}
{{- define "gardenio.vmalert.fullname" -}}
{{- printf "%s-vmalert" (include "gardenio.fullname" .) | trunc 63 | trimSuffix "-" }}
{{- end }}

{{/*
vmalert labels
*/}}
{{- define "gardenio.vmalert.labels" -}}
{{ include "gardenio.labels" . }}
app.kubernetes.io/component: vmalert
{{- end }}

{{/*
vmalert selector labels
*/}}
{{- define "gardenio.vmalert.selectorLabels" -}}
{{ include "gardenio.selectorLabels" . }}
app.kubernetes.io/component: vmalert
{{- end }}

{{/*
vmalert datasource/query URL — vmalert evaluates recording rules by querying
vmsingle (-datasource.url) and writes the resulting series back to the same
vmsingle (-remoteWrite.url). Both point at the in-chart vmsingle Service.
*/}}
{{- define "gardenio.vmalert.datasourceURL" -}}
{{- printf "http://%s:8428" (include "gardenio.vmsingle.fullname" .) -}}
{{- end }}

{{/*
vmalert rules ConfigMap name — holds the chart-shipped rules
(templates/vmalert/configmap.yaml writes the recording.yaml key); vmalert globs
every *.yaml key in it. (alertmanager-bridge, which also wrote a bead-authored
beads-rules.yaml key here, was deleted 2026-10-31, gd-Y4ZCGr2m2t.)
*/}}
{{- define "gardenio.vmalert.rulesConfigMapName" -}}
{{- printf "%s-rules" (include "gardenio.vmalert.fullname" .) -}}
{{- end }}

{{/*
vmalert effective notifier URL — the -notifier.url vmalert dials to POST firing
alerts. Precedence (ka-0000003392, epic ka-0000000770):
  1. explicit .Values.vmalert.notifierURL (operator override — always wins);
  2. else, when the in-chart Alertmanager is enabled, auto-wire to its
     ClusterIP Service (the option-(a) delivery path decision ka-0000006149);
  3. else empty — recording-rules-only, falls back to -notifier.blackhole.
Returns the URL string (possibly empty). The vmalert deployment consumes this.
*/}}
{{- define "gardenio.vmalert.effectiveNotifierURL" -}}
{{- if .Values.vmalert.notifierURL -}}
{{- .Values.vmalert.notifierURL -}}
{{- else if .Values.alertmanager.enabled -}}
{{- printf "http://%s:%d" (include "gardenio.alertmanager.fullname" .) (int (.Values.alertmanager.service.port | default 9093)) -}}
{{- end -}}
{{- end }}

{{/* ===== vmalert-logs (VictoriaLogs / LogsQL rule evaluator) helpers =====

A SECOND, dedicated vmalert instance whose -datasource.url points at
VictoriaLogs (not vmsingle). It exists because vmalert's -datasource.url is a
single global flag: the existing gardenio-vmalert evaluates PromQL against
vmsingle:8428, but the LogsQL detections (edgeContainment, scannerSurge,
edgeShipperLiveness) are `type: vlogs` rules that must be evaluated against the
VictoriaLogs stats API (/select/logsql/stats_query on :9428). Rather than
retarget the shared recording-rule evaluator, we stand up a small sibling that
reads VictoriaLogs and notifies the SAME Alertmanager → Slack path. Off by
default; enabled per-env once VictoriaLogs (ka-0000007261) is live. */}}

{{/*
vmalert-logs fully qualified name
*/}}
{{- define "gardenio.vmalertLogs.fullname" -}}
{{- printf "%s-vmalert-logs" (include "gardenio.fullname" .) | trunc 63 | trimSuffix "-" }}
{{- end }}

{{/*
vmalert-logs labels
*/}}
{{- define "gardenio.vmalertLogs.labels" -}}
{{ include "gardenio.labels" . }}
app.kubernetes.io/component: vmalert-logs
{{- end }}

{{/*
vmalert-logs selector labels
*/}}
{{- define "gardenio.vmalertLogs.selectorLabels" -}}
{{ include "gardenio.selectorLabels" . }}
app.kubernetes.io/component: vmalert-logs
{{- end }}

{{/*
vmalert-logs rules ConfigMap name — carries ONLY the LogsQL (type: vlogs)
detection rules, kept separate from the vmsingle recording-rule ConfigMap so a
vlogs rule can never be evaluated against vmsingle (wrong datasource) or vice
versa.
*/}}
{{- define "gardenio.vmalertLogs.rulesConfigMapName" -}}
{{- printf "%s-rules" (include "gardenio.vmalertLogs.fullname" .) -}}
{{- end }}

{{/*
vmalert-logs datasource URL — the VictoriaLogs query endpoint the vlogs rules
evaluate against (/select/logsql/stats_query lives under this base). Precedence:
  1. explicit .Values.vmalertLogs.datasourceURL — always wins (point at an
     out-of-chart VictoriaLogs);
  2. else, when the in-chart vlsingle store is enabled (task ka-0000007261),
     auto-wire to ITS Service — release-relative via gardenio.vlsingle.baseURL, so
     the evaluator and store stay co-wired on ANY release name (the prod release
     `gardenio` AND the gardenio-e2e-* namespaces alike). This is the normal
     in-chart path now that VictoriaLogs ships in this chart;
  3. else fall back to the configured out-of-chart Service name + port
     (vmalertLogs.victoriaLogs.*), the legacy path from when VictoriaLogs was
     deployed separately. Its literal default (`gardenio-victoria-logs`) only
     resolves on the prod release, hence the vlsingle-derived path above.
Until a datasource actually answers, every vlogs expr returns an empty result
and NOTHING fires (inert-but-ready).
*/}}
{{- define "gardenio.vmalertLogs.datasourceURL" -}}
{{- if .Values.vmalertLogs.datasourceURL -}}
{{- .Values.vmalertLogs.datasourceURL -}}
{{- else if .Values.vlsingle.enabled -}}
{{- include "gardenio.vlsingle.baseURL" . -}}
{{- else -}}
{{- printf "http://%s:%d" (.Values.vmalertLogs.victoriaLogs.serviceName | default "gardenio-victoria-logs") (int (.Values.vmalertLogs.victoriaLogs.servicePort | default 9428)) -}}
{{- end -}}
{{- end }}

{{/*
vmalert-logs effective notifier URL — same precedence as the vmalert notifier
(explicit override wins; else the in-chart Alertmanager Service). vlogs ALERTING
rules require a notifier for the same reason: vmalert crashloops if it loads
alerting rules with neither -notifier.url nor -notifier.blackhole set.
*/}}
{{- define "gardenio.vmalertLogs.effectiveNotifierURL" -}}
{{- if .Values.vmalertLogs.notifierURL -}}
{{- .Values.vmalertLogs.notifierURL -}}
{{- else if .Values.alertmanager.enabled -}}
{{- printf "http://%s:%d" (include "gardenio.alertmanager.fullname" .) (int (.Values.alertmanager.service.port | default 9093)) -}}
{{- end -}}
{{- end }}

{{/* ===== Alertmanager (alert → Slack delivery path) component helpers ===== */}}

{{/*
Alertmanager fully qualified name. The genuine Prometheus Alertmanager that
receives firing alerts from vmalert and posts them to Slack via an incoming
webhook (option (a), decision ka-0000006149). (The alerts-as-beads
alertmanager-bridge, option (c), was deleted 2026-10-31, gd-Y4ZCGr2m2t.)
*/}}
{{- define "gardenio.alertmanager.fullname" -}}
{{- printf "%s-alertmanager" (include "gardenio.fullname" .) | trunc 63 | trimSuffix "-" }}
{{- end }}

{{/*
Alertmanager labels
*/}}
{{- define "gardenio.alertmanager.labels" -}}
{{ include "gardenio.labels" . }}
app.kubernetes.io/component: alertmanager
{{- end }}

{{/*
Alertmanager selector labels
*/}}
{{- define "gardenio.alertmanager.selectorLabels" -}}
{{ include "gardenio.selectorLabels" . }}
app.kubernetes.io/component: alertmanager
{{- end }}

{{/*
Alertmanager config ConfigMap name — holds alertmanager.yml (route + receivers).
The Slack webhook URL is NEVER in this ConfigMap; it is mounted as a file from
the ESO-synced Secret and referenced via global.slack_api_url_file.
*/}}
{{- define "gardenio.alertmanager.configMapName" -}}
{{- printf "%s-config" (include "gardenio.alertmanager.fullname" .) -}}
{{- end }}

{{/* ===== LiteLLM component helpers ===== */}}

{{/*
LiteLLM fully qualified name
*/}}
{{- define "gardenio.litellm.fullname" -}}
{{- printf "%s-litellm" (include "gardenio.fullname" .) | trunc 63 | trimSuffix "-" }}
{{- end }}

{{/*
LiteLLM labels
*/}}
{{- define "gardenio.litellm.labels" -}}
{{ include "gardenio.labels" . }}
app.kubernetes.io/component: litellm
{{- end }}

{{/*
LiteLLM selector labels
*/}}
{{- define "gardenio.litellm.selectorLabels" -}}
{{ include "gardenio.selectorLabels" . }}
app.kubernetes.io/component: litellm
{{- end }}

{{/*
LiteLLM ServiceAccount name. Used only when the Bedrock embedding model is on
with the IRSA credential strategy (litellm.embedding.irsaRoleArn) — the proxy
pod rides a dedicated SA annotated with the Bedrock-capable role-arn.
*/}}
{{- define "gardenio.litellm.serviceAccountName" -}}
{{- include "gardenio.litellm.fullname" . -}}
{{- end }}

{{/*
Whether the litellm pod needs an AWS Bedrock credential path. True when the
Titan embedding model OR any Bedrock chat model group OR the Bedrock
image-generation model is enabled — all invoke Bedrock from the same pod and
share the single embedding.* credential strategy (litellm Stage-3,
ka-0000002573; image-gen ka-0000008102). Emits "true" or "" (falsy).
*/}}
{{- define "gardenio.litellm.needsBedrockCreds" -}}
{{- if or .Values.litellm.embedding.enabled .Values.litellm.bedrock.enabled .Values.litellm.imageGeneration.enabled -}}
true
{{- end -}}
{{- end }}

{{/*
LiteLLM IRSA gate — true when the proxy needs to assume a Bedrock-capable role
(the IRSA credential strategy). This gates the EKS role-arn annotation on the SA
(templates/litellm/serviceaccount.yaml) and automountServiceAccountToken on the
deployment (templates/litellm/deployment.yaml). Wraps needsBedrockCreds
(embedding OR bedrock-chat, litellm Stage-3 ka-0000002573) so the broader Bedrock
semantics stay in one place. Outputs "true" when on, empty (falsy) otherwise.

NOTE: this does NOT gate whether the SA *object* exists. Under the unconditional-sa
decision (ka-0000000992) the SA is rendered whenever litellm.enabled, decoupled
from this gate, so the deployment's serviceAccountName reference can never point at
a missing SA (ka-0000006740: SA-not-found / FailedCreate when the two gates
diverged). This helper only decides whether that SA carries the IRSA role-arn and
whether the pod mounts its web-identity token.
*/}}
{{- define "gardenio.litellm.usesDedicatedSA" -}}
{{- if and .Values.litellm.enabled (include "gardenio.litellm.needsBedrockCreds" .) .Values.litellm.embedding.irsaRoleArn -}}
true
{{- end -}}
{{- end }}

{{/*
LiteLLM database URL — constructs postgres connection string for the litellm database.
Uses K8s variable expansion $(LITELLM_DATABASE_PASSWORD) so the password comes from the secret env var.
application_name=litellm names the connections in pg_stat_activity (gd-rXqkv4rSmG); Prisma
reads it from the URL, not PGAPPNAME. A litellm.databaseURL override is used verbatim.
*/}}
{{- define "gardenio.litellm.databaseURL" -}}
{{- if .Values.litellm.databaseURL -}}
{{- .Values.litellm.databaseURL -}}
{{- else if .Values.postgres.enabled -}}
{{- printf "postgresql://postgres:$(LITELLM_DATABASE_PASSWORD)@%s:5432/%s?application_name=litellm" (include "gardenio.postgres.fullname" .) (.Values.litellm.database | default "litellm") -}}
{{- else if .Values.postgresExternal.enabled -}}
{{- printf "postgresql://%s:$(LITELLM_DATABASE_PASSWORD)@%s:%v/%s?sslmode=%s&application_name=litellm" .Values.postgresExternal.username .Values.postgresExternal.host (.Values.postgresExternal.port | default 5432) (.Values.litellm.database | default "litellm") (.Values.postgresExternal.sslmode | default "require") -}}
{{- end -}}
{{- end }}

{{/*
LiteLLM master key secret name.
Prefers explicit litellm.masterKeySecret, then the ExternalSecret-managed
"litellm-master-key", then falls back to "<release>-litellm-master-key".
*/}}
{{- define "gardenio.litellm.masterKeySecretName" -}}
{{- if .Values.litellm.masterKeySecret -}}
{{- .Values.litellm.masterKeySecret -}}
{{- else if and .Values.externalSecrets.enabled .Values.externalSecrets.litellmMasterKey.remoteRef -}}
litellm-master-key
{{- else -}}
{{- printf "%s-litellm-master-key" (include "gardenio.fullname" .) -}}
{{- end -}}
{{- end }}

{{/*
LiteLLM Redis (router coordination, H4 ka-0000003463) fully qualified name for
the OPTIONAL in-chart Redis Deployment/Service. Only rendered when
litellm.redis.deploy.enabled.
*/}}
{{- define "gardenio.litellm.redis.fullname" -}}
{{- printf "%s-litellm-redis" (include "gardenio.fullname" .) | trunc 63 | trimSuffix "-" }}
{{- end }}

{{/*
LiteLLM Redis labels.
*/}}
{{- define "gardenio.litellm.redis.labels" -}}
{{ include "gardenio.labels" . }}
app.kubernetes.io/component: litellm-redis
{{- end }}

{{/*
LiteLLM Redis selector labels.
*/}}
{{- define "gardenio.litellm.redis.selectorLabels" -}}
{{ include "gardenio.selectorLabels" . }}
app.kubernetes.io/component: litellm-redis
{{- end }}

{{/*
LiteLLM Redis host the Router connects to. Prefers an explicit
litellm.redis.host; otherwise defaults to the in-chart Redis Service (which only
exists when litellm.redis.deploy.enabled — the validate-redis.yaml guard rejects
an empty host with deploy off). Emits nothing when litellm.redis is disabled.
*/}}
{{- define "gardenio.litellm.redis.host" -}}
{{- if .Values.litellm.redis.host -}}
{{- .Values.litellm.redis.host -}}
{{- else -}}
{{- include "gardenio.litellm.redis.fullname" . -}}
{{- end -}}
{{- end }}

{{/*
ForwardAuth (oauth2-proxy) session-store Redis (gd-C2Sl6ClxAY). Name, labels and
selector labels for the OPTIONAL in-chart Redis that holds oauth2-proxy
sessions server-side (keycloak.forwardAuth.sessionStore.type=redis), so the
browser cookie is a small ticket and a refresh is one atomic server-side write.
*/}}
{{/*
forwardAuth templates ConfigMap labels: the standard labels WITHOUT
helm.sh/chart. The Deployment's pod template hashes this ConfigMap as
checksum/error-template (oauth2-proxy reads templates only at start), so the
ConfigMap must render byte-identically across chart versions or every calver
release rolls oauth2-proxy. A rolled oauth2-proxy starts with an empty JWKS
cache; when the same release also restarted Keycloak, its first bearer
verification fetched the keys through the edge while Keycloak was still booting,
got a 502 and answered 401 to a valid `kd login` token (gd-NrzwHXUxV0). Nothing
selects on helm.sh/chart; every other label stays.
*/}}
{{/*
Checksum of a rendered single-object manifest without its metadata, for the
checksum/* pod annotations that roll a pod when its ConfigMap or Secret
changes. Hashing the whole include also hashed helm.sh/chart, so every calver
release rolled pods whose config had not changed; litellm is a single
Recreate replica, so each release cut LiteLLM for minutes and failed in-flight
land provisioning (gd-AQ9iCiKxML). Usage:
  checksum/config: {{ include "gardenio.contentChecksum" (include (print $.Template.BasePath "/x/configmap.yaml") .) }}
*/}}
{{- define "gardenio.contentChecksum" -}}
{{- $obj := fromYaml . -}}
{{- if hasKey $obj "Error" }}{{ fail (printf "gardenio.contentChecksum: hashed manifest is not one YAML object: %s" (get $obj "Error")) }}{{ end -}}
{{- omit $obj "metadata" | toYaml | sha256sum -}}
{{- end }}

{{- define "gardenio.forwardAuth.templateLabels" -}}
app.kubernetes.io/managed-by: {{ .Release.Service }}
{{ include "gardenio.selectorLabels" . }}
app.kubernetes.io/component: forwardauth
{{- end }}

{{- define "gardenio.forwardAuth.redis.fullname" -}}
{{- printf "%s-forwardauth-redis" (include "gardenio.fullname" .) | trunc 63 | trimSuffix "-" }}
{{- end }}

{{- define "gardenio.forwardAuth.redis.labels" -}}
{{ include "gardenio.labels" . }}
app.kubernetes.io/component: forwardauth-redis
{{- end }}

{{- define "gardenio.forwardAuth.redis.selectorLabels" -}}
{{ include "gardenio.selectorLabels" . }}
app.kubernetes.io/component: forwardauth-redis
{{- end }}

{{/*
Redis host oauth2-proxy connects to: an explicit sessionStore.redis.host, else
the in-chart Redis Service.
*/}}
{{- define "gardenio.forwardAuth.redis.host" -}}
{{- if .Values.keycloak.forwardAuth.sessionStore.redis.host -}}
{{- .Values.keycloak.forwardAuth.sessionStore.redis.host -}}
{{- else -}}
{{- include "gardenio.forwardAuth.redis.fullname" . -}}
{{- end -}}
{{- end }}

{{/* ===== Edge-auth component helpers (Stage 3 / W-B-12) ===== */}}

{{/*
Edge-auth fully qualified name.
*/}}
{{- define "gardenio.edgeAuth.fullname" -}}
{{- printf "%s-edge-auth" (include "gardenio.fullname" .) | trunc 63 | trimSuffix "-" }}
{{- end }}

{{/*
Edge-auth labels.
*/}}
{{- define "gardenio.edgeAuth.labels" -}}
{{ include "gardenio.labels" . }}
app.kubernetes.io/component: edge-auth
{{- end }}

{{/*
Edge-auth selector labels.
*/}}
{{- define "gardenio.edgeAuth.selectorLabels" -}}
{{ include "gardenio.selectorLabels" . }}
app.kubernetes.io/component: edge-auth
{{- end }}

{{/*
Edge-auth HMAC secret name. Honors an env-local override
(edgeAuth.hmacKey.secretName) for dev / e2e harnesses; otherwise picks
the chart-managed name that the ExternalSecret populates.
*/}}
{{- define "gardenio.edgeAuth.hmacSecretName" -}}
{{- if .Values.edgeAuth.hmacKey.secretName -}}
{{- .Values.edgeAuth.hmacKey.secretName -}}
{{- else -}}
{{- printf "%s-edge-auth-hmac" (include "gardenio.fullname" .) | trunc 63 | trimSuffix "-" -}}
{{- end -}}
{{- end }}

{{/*
Edge-auth upstream URL for kbeads. Operator override wins; otherwise
auto-wires to the in-chart beads Service over its HTTP port.
*/}}
{{- define "gardenio.edgeAuth.upstreamKbeads" -}}
{{- if .Values.edgeAuth.upstreams.kbeads -}}
{{- .Values.edgeAuth.upstreams.kbeads -}}
{{- else -}}
{{- printf "http://%s:%s" (include "gardenio.beads.host" .) (include "gardenio.beads.httpPort" .) -}}
{{- end -}}
{{- end }}

{{/*
Edge-auth upstream URL for coopmux. Operator override wins; otherwise
auto-wires to the in-chart coopmux Service.
*/}}
{{- define "gardenio.edgeAuth.upstreamCoopmux" -}}
{{- if .Values.edgeAuth.upstreams.coopmux -}}
{{- .Values.edgeAuth.upstreams.coopmux -}}
{{- else if .Values.coopmux.enabled -}}
{{- include "gardenio.coopmux.serviceURL" . -}}
{{- else -}}
{{- "" -}}
{{- end -}}
{{- end }}

{{/*
Edge-auth upstream URL for the NATS WebSocket channel. Operator override
wins; otherwise auto-wires to the in-chart NATS Service over its plaintext
WebSocket port (8443) when websocket is enabled. Dispatched on /kd/nats/ so
gb agent start --local's realtime channel reaches NATS instead of the kbeads
daemon (ka-0000006000). Empty -> NATS route not registered.
*/}}
{{- define "gardenio.edgeAuth.upstreamNats" -}}
{{- if .Values.edgeAuth.upstreams.nats -}}
{{- .Values.edgeAuth.upstreams.nats -}}
{{- else if .Values.nats.websocket.enabled -}}
{{- printf "http://%s:8443" (include "gardenio.nats.fullname" .) -}}
{{- else -}}
{{- "" -}}
{{- end -}}
{{- end }}

{{/* ===== Pod scheduling helpers ===== */}}

{{/*
Merged nodeSelector — combines global.nodeSelector with per-component nodeSelector.
Per-component values take precedence over global values.
Usage: include "gardenio.nodeSelector" (dict "local" .Values.beads.nodeSelector "global" .Values.global.nodeSelector)
*/}}
{{- define "gardenio.nodeSelector" -}}
{{- $merged := merge (deepCopy (.local | default dict)) (.global | default dict) -}}
{{- if $merged }}
nodeSelector:
  {{- toYaml $merged | nindent 2 }}
{{- end }}
{{- end }}

{{/*
Merged tolerations — concatenates global.tolerations with per-component
tolerations (global first, then local). Both are lists, so they are appended
rather than merged-by-key. Emits nothing when the combined list is empty, so
output is byte-identical for any env that sets neither.

The combined list is the union of cluster-wide tolerations (e.g. an autoscaling
node pool taint every workload must tolerate to use its headroom) and any
component-specific tolerations. Toleration duplication is harmless to the
scheduler, so no dedup is performed.

Usage: include "gardenio.tolerations" (dict "local" .Values.beads.tolerations "global" .Values.global.tolerations)
*/}}
{{- define "gardenio.tolerations" -}}
{{- $combined := concat (.global | default list) (.local | default list) -}}
{{- if $combined }}
tolerations:
  {{- toYaml $combined | nindent 2 }}
{{- end }}
{{- end }}

{{/* ===== ExternalSecret refresh-interval helper ===== */}}

{{/*
Resolve refreshInterval for an ExternalSecret by category. Categories are
defined under .Values.externalSecrets.refreshIntervals (rotated / rotated-db /
static / oauth). Falls back to .Values.externalSecrets.refreshInterval, then
to "1h". See gardenio/docs/secrets-refresh.md for category guidance.
Usage: {{ include "gardenio.externalSecrets.refreshInterval" (dict "category" "rotated" "root" .) }}
*/}}
{{- define "gardenio.externalSecrets.refreshInterval" -}}
{{- $cat := .category -}}
{{- $intervals := .root.Values.externalSecrets.refreshIntervals | default dict -}}
{{- $val := index $intervals $cat -}}
{{- if $val -}}
{{- $val -}}
{{- else -}}
{{- .root.Values.externalSecrets.refreshInterval | default "1h" -}}
{{- end -}}
{{- end }}

{{/* ===== unifiedHost-derived link helpers (R3, ka-0000003816 / ka-0000006743) =====

These helpers derive the outbound link env vars (COOPMUX_PUBLIC_URL,
BEADS_VIEWER_URL, BEADS_WEB_URL, GRAFANA_BASE_URL, viewer/coopmux BASE_PATH,
GARDENIO_DEFAULT_HOST, ESCALATION_REVIEW_URL) from a single per-env
`unifiedHost` knob, replacing the per-app host wiring that envs hand-set today.

Each link helper applies the same THREE-TIER PRECEDENCE:
  1. explicit per-var override  (wins when non-empty)
  2. unified-derived            (when unifiedHost.enabled + host + the path)
  3. legacy per-app host        (today's expression, byte-for-byte)

SAFETY: with unifiedHost.enabled=false (every committed env) tier-2 never
fires, so every helper falls to tier-3 and `helm template` output is
byte-identical to pre-helper main. The helpers are inert until an operator
flips unifiedHost.enabled=true + host (the gated cutover, out of scope here).
Empty string ⇒ caller omits the env var, exactly as the GRAFANA_BASE_URL block
this generalizes. See docs/url-unify/R3-single-host-knob-design.md §3. */}}

{{/*
gardenio.unified.host — the single per-env knob, or "" when unified is off.
Guard used by every derivation below.
*/}}
{{- define "gardenio.unified.host" -}}
{{- if and .Values.unifiedHost.enabled .Values.unifiedHost.host -}}
{{- .Values.unifiedHost.host -}}
{{- end -}}
{{- end -}}

{{/*
gardenio.unified.url — derive https://<unifiedHost.host><path> when unified is on
AND the given path is non-empty; else "". Args: dict "ctx" . "path" <pathStr>.
*/}}
{{- define "gardenio.unified.url" -}}
{{- $ctx := .ctx -}}
{{- $h := include "gardenio.unified.host" $ctx -}}
{{- if and (ne $h "") (ne .path "") -}}
{{- printf "https://%s%s" $h .path -}}
{{- end -}}
{{- end -}}

{{/*
gardenio.link.coopmuxPublicURL — COOPMUX_PUBLIC_URL.
1) explicit slackBridge.coopmuxPublicURL (NEW optional override; add to values)
2) unified: https://<host><paths.mux>   (only when coopmux.enabled)
3) legacy:  https://<coopmux.ingress.host>/mux  (only when coopmux.enabled+ingress.host)
*/}}
{{- define "gardenio.link.coopmuxPublicURL" -}}
{{- $o := .Values.slackBridge.coopmuxPublicURL | default "" -}}
{{- if ne $o "" -}}{{ $o }}{{- else if .Values.coopmux.enabled -}}
{{-   $u := include "gardenio.unified.url" (dict "ctx" . "path" ((.Values.unifiedHost.paths | default dict).mux | default "")) -}}
{{-   if ne $u "" -}}{{ $u }}{{- else if .Values.coopmux.ingress.host -}}
{{-     printf "https://%s/mux" .Values.coopmux.ingress.host -}}
{{-   end -}}
{{- end -}}
{{- end -}}

{{/*
gardenio.link.beadsViewerURL — BEADS_VIEWER_URL.
1) explicit slackBridge.beadsViewerURL
2) unified: https://<host><paths.beads>
3) legacy:  avaIngress.host → else ingress.host+pathPrefix  (current chain)
*/}}
{{- define "gardenio.link.beadsViewerURL" -}}
{{- $o := .Values.slackBridge.beadsViewerURL | default "" -}}
{{- if ne $o "" -}}{{ $o }}{{- else -}}
{{-   $u := include "gardenio.unified.url" (dict "ctx" . "path" ((.Values.unifiedHost.paths | default dict).beads | default "")) -}}
{{-   if ne $u "" -}}{{ $u }}
{{-   else if and .Values.beadsViewer.avaIngress .Values.beadsViewer.avaIngress.enabled .Values.beadsViewer.avaIngress.host -}}
{{-     printf "https://%s" .Values.beadsViewer.avaIngress.host -}}
{{-   else if and .Values.beadsViewer.ingress.enabled .Values.beadsViewer.ingress.host -}}
{{-     printf "https://%s%s" .Values.beadsViewer.ingress.host .Values.beadsViewer.ingress.pathPrefix -}}
{{-   end -}}
{{- end -}}
{{- end -}}

{{/*
gardenio.link.beadsWebURL — BEADS_WEB_URL (escalation deep links).
1) explicit slackBridge.beadsWebURL
2) unified: https://<host><paths.beads>   (served by beads-viewer)
NOTE: tier-2 is COUPLED-to-gated — see design §6 note A (escalations HTML must be
served under /beads on the unified host first). Inert until cutover.
The legacy beads3d host fallback was removed when beads3d was retired (ka-0000006444).
*/}}
{{- define "gardenio.link.beadsWebURL" -}}
{{- $o := .Values.slackBridge.beadsWebURL | default "" -}}
{{- if ne $o "" -}}{{ $o }}{{- else -}}
{{-   $u := include "gardenio.unified.url" (dict "ctx" . "path" ((.Values.unifiedHost.paths | default dict).beads | default "")) -}}
{{-   if ne $u "" -}}{{ $u }}{{- end -}}
{{- end -}}
{{- end -}}

{{/*
gardenio.link.beadURL — a bead permalink for alert annotations: "<beadsWebURL>/<id>"
when gardenio.link.beadsWebURL resolves, else the bare bead id (never a link to a
host this deployment does not serve). Call with (dict "ctx" $ "id" "<bead-id>").
*/}}
{{- define "gardenio.link.beadURL" -}}
{{- $base := include "gardenio.link.beadsWebURL" .ctx | trimSuffix "/" -}}
{{- if ne $base "" -}}{{ printf "%s/%s" $base .id }}{{- else -}}{{ .id }}{{- end -}}
{{- end -}}

{{/*
gardenio.link.grafanaBaseURL — GRAFANA_BASE_URL. Lifts the EXISTING block at
beads-viewer/deployment.yaml verbatim into a helper. Already correct; lifting it
just lets the design treat all link vars uniformly.
*/}}
{{- define "gardenio.link.grafanaBaseURL" -}}
{{- $o := .Values.beadsViewer.grafanaBaseURL | default "" -}}
{{- if ne $o "" -}}{{ $o }}{{- else -}}
{{-   $p := (.Values.unifiedHost.paths | default dict).grafana | default "" -}}
{{-   if and .Values.grafana.enabled .Values.unifiedHost.enabled .Values.unifiedHost.host (ne $p "") -}}
{{-     printf "https://%s%s" .Values.unifiedHost.host $p -}}
{{-   end -}}
{{- end -}}
{{- end -}}

{{/*
gardenio.link.defaultGardenioHost — GARDENIO_DEFAULT_HOST for agent pods + gb CLI.
Bare host (no scheme/path); gb derives /beads + cli.<host> from it.
1) explicit agents.defaultGardenioHost (NEW optional override)
2) unified: <unifiedHost.host>
3) legacy:  "" → gb falls back to compiled default. NOT set in helm today.
COUPLED-to-gated — see design §6 note D (ka-0000004747 default-URL switch).
*/}}
{{- define "gardenio.link.defaultGardenioHost" -}}
{{- $o := (.Values.agents.defaultGardenioHost | default "") -}}
{{- if ne $o "" -}}{{ $o }}{{- else -}}{{ include "gardenio.unified.host" . }}{{- end -}}
{{- end -}}

{{/*
gardenio.link.escalationReviewURL — ESCALATION_REVIEW_URL (viewer "Review →" deep
link). Unset in every env today → link silently disabled. Additive win.
1) explicit beadsViewer.escalationReviewURL (NEW optional override)
2) unified: https://<host>/escalations
3) legacy:  "" (stays disabled)
*/}}
{{- define "gardenio.link.escalationReviewURL" -}}
{{- $o := (.Values.beadsViewer.escalationReviewURL | default "") -}}
{{- if ne $o "" -}}{{ $o }}{{- else -}}
{{-   $h := include "gardenio.unified.host" . -}}
{{-   if ne $h "" -}}{{ printf "https://%s/escalations" $h }}{{- end -}}
{{- end -}}
{{- end -}}

{{/*
gardenio.unified.beadsBasePath — viewer BASE_PATH. When unified is on, returns
paths.beads (so BASE_PATH == Traefik strip-prefix by construction); else today's
hand-set beadsViewer.ingress.pathPrefix.
*/}}
{{- define "gardenio.unified.beadsBasePath" -}}
{{- if include "gardenio.unified.host" . -}}{{ (.Values.unifiedHost.paths | default dict).beads | default "" }}
{{- else -}}{{ .Values.beadsViewer.ingress.pathPrefix }}{{- end -}}
{{- end -}}

{{/*
gardenio.unified.muxBasePath — coopmux BASE_PATH. When unified is on, returns
paths.mux; else today's hand-set coopmux.basePath.
*/}}
{{- define "gardenio.unified.muxBasePath" -}}
{{- if include "gardenio.unified.host" . -}}{{ (.Values.unifiedHost.paths | default dict).mux | default "" }}
{{- else -}}{{ .Values.coopmux.basePath }}{{- end -}}
{{- end -}}

{{/*
Edge middlewares — NEW global buffering (request-body cap) + inFlightReq
(concurrency cap) middleware names, prepended to every IngressRoute middleware
chain when enabled. Order: buffering (413) then inFlightReq (429) — cheapest
rejects first, matching the in-box edge chain (research doc §4, ka-0000002581;
task ka-0000006519). Both gated and DEFAULT OFF in the base chart; the
Gardenio-in-a-Box overlay (deploy-values/gardenio-inbox.yaml) turns both on.
Accepts the root context (.). Emits "- name:" list items; callers wrap with
`{{- include "gardenio.edgeMiddlewares" . | nindent 8 }}`.
*/}}
{{- define "gardenio.edgeMiddlewares" -}}
{{- $fullname := include "gardenio.fullname" . -}}
{{- if and .Values.edge .Values.edge.buffering .Values.edge.buffering.enabled }}
- name: {{ $fullname }}-edge-buffering
{{- end }}
{{- if and .Values.edge .Values.edge.inFlightReq .Values.edge.inFlightReq.enabled }}
- name: {{ $fullname }}-edge-inflightreq
{{- end }}
{{- if and .Values.edge .Values.edge.crowdsec .Values.edge.crowdsec.enabled (eq (include "gardenio.traefik.ingressRoutesEnabled" .) "true") }}
- name: {{ $fullname }}-edge-crowdsec
{{- end }}
{{- end }}

{{/*
CrowdSec bouncer API key — stable across upgrades.
Returns edge.crowdsec.bouncerKeySecret verbatim when set (operator-provided);
otherwise looks up the chart-managed Secret and reuses its key, generating a
fresh randAlphaNum 48 only on first install (mirrors the keycloak admin pattern).
*/}}
{{- define "gardenio.crowdsec.bouncerKey" -}}
{{- $cs := .Values.edge.crowdsec -}}
{{- if $cs.bouncerKeySecret -}}
{{- $cs.bouncerKeySecret -}}
{{- else -}}
{{- $secretName := printf "%s-edge-crowdsec" (include "gardenio.fullname" .) -}}
{{- $existing := lookup "v1" "Secret" .Release.Namespace $secretName -}}
{{- if and $existing $existing.data (index $existing.data "bouncer-key") -}}
{{- index $existing.data "bouncer-key" | b64dec -}}
{{- else -}}
{{- randAlphaNum 48 -}}
{{- end -}}
{{- end -}}
{{- end }}

{{/*
OpenTelemetry trace-export env vars (epic ka-0000000598, task ka-0000005802).
Renders the GARDENIO_OTEL_* env block consumed by the Go tracing package in the
controller, bridges, and kbeads daemon. Emits nothing when otel.traces.enabled
is false, so the binaries keep their no-op tracer. The endpoint is tpl-rendered
so it can reference the release fullname (the in-chart VictoriaTraces service).
Include with the root context: {{ include "gardenio.otelTracingEnv" . | nindent 12 }}
*/}}
{{- define "gardenio.otelTracingEnv" -}}
{{- if .Values.otel.traces.enabled -}}
- name: GARDENIO_OTEL_TRACES_ENABLED
  value: "true"
- name: GARDENIO_OTEL_EXPORTER_OTLP_ENDPOINT
  value: {{ tpl .Values.otel.traces.endpoint . | quote }}
- name: GARDENIO_OTEL_TRACES_SAMPLER_RATIO
  value: {{ .Values.otel.traces.samplerRatio | quote }}
- name: GARDENIO_OTEL_EXPORTER_TIMEOUT
  value: {{ .Values.otel.traces.exporterTimeout | quote }}
{{- end -}}
{{- end }}

{{/*
k3s post-DNAT apiserver egress rule for long-running apiserver clients
(ka-0000004987; hook-Job twin ka-0000003176 — pre-upgrade-hooks-egress.yaml
carries the same rule inline, unconditionally, with the full mechanism
write-up). On k3s, kube-proxy (iptables mode) DNATs the in-cluster apiserver
VIP 10.43.0.1:443 to <node-private-IP>:6443 in nat/PREROUTING, BEFORE the
embedded kube-router netpol controller evaluates egress ipBlocks — so a
policy's 443 rules (kubeApiServer.cidrs pre-DNAT VIP; internet-443 with
RFC1918 excluded) never match and every apiserver call is REJECTED. The node
IP is DHCP-assigned (unknowable in a tracked values file), so the allow must
name the private ranges on the apiserver port. 6443 ONLY — 443 keeps its
RFC1918/IMDS/CGNAT exclusions, preserving the no-pivot posture for everything
that is not a kube-apiserver. Callers gate on networkPolicies.apiServerPostDNAT.
Include with: {{ include "gardenio.netpol.k3sApiServerPostDNATEgress" . | nindent 4 }}
*/}}
{{- define "gardenio.netpol.k3sApiServerPostDNATEgress" -}}
# Kubernetes API server, k3s post-DNAT path (ka-0000004987 — see the
# gardenio.netpol.k3sApiServerPostDNATEgress helper for the mechanism).
- to:
    - ipBlock:
        cidr: 10.0.0.0/8
    - ipBlock:
        cidr: 172.16.0.0/12
    - ipBlock:
        cidr: 192.168.0.0/16
  ports:
    - protocol: TCP
      port: 6443
{{- end }}

{{/*
priorityClassName line for a pre-install/pre-upgrade hook pod, or "".
Pre-hooks run before the release's own objects are applied, so on the release
that adds a PriorityClass (or a first install) the class does not exist yet
and a pod naming it would be rejected at admission, failing the upgrade. Name
the class only once the cluster has it (gd-2qjxTVMBkC). `lookup` is empty
under `helm template`, so offline renders show no class here.
Usage: {{- with include "gardenio.hookPriorityClassName" . }}
         {{ . }}
       {{- end }}
*/}}
{{- define "gardenio.hookPriorityClassName" -}}
{{- $pc := .Values.global.systemPriorityClass -}}
{{- if and $pc.create (lookup "scheduling.k8s.io/v1" "PriorityClass" "" $pc.infra.name) -}}
priorityClassName: {{ $pc.infra.name }}
{{- end -}}
{{- end }}

{{/*
The release deployer's git credential Secret (gd-q48ldwLspl): an explicit
releaseDeployer.gitSecret, else the controller-minted App token Secret when
agents.githubApp.enabled, else the shared gardenio-git-credentials.
*/}}
{{- define "gardenio.releaseDeployer.appTokenSecret" -}}
release-deployer-github-token
{{- end }}

{{- define "gardenio.releaseDeployer.gitSecret" -}}
{{- if .Values.releaseDeployer.gitSecret -}}
{{ .Values.releaseDeployer.gitSecret }}
{{- else if and .Values.agents.enabled .Values.agents.githubApp.enabled -}}
{{ include "gardenio.releaseDeployer.appTokenSecret" . }}
{{- else -}}
gardenio-git-credentials
{{- end -}}
{{- end }}

{{/*
"true" when the controller must mint the release deployer's token: the
deployer is on and its Secret resolves to the App token Secret.
*/}}
{{- define "gardenio.releaseDeployer.mintsAppToken" -}}
{{- if and .Values.releaseDeployer.enabled .Values.agents.enabled .Values.agents.githubApp.enabled (eq (include "gardenio.releaseDeployer.gitSecret" .) (include "gardenio.releaseDeployer.appTokenSecret" .)) -}}
true
{{- end -}}
{{- end }}
