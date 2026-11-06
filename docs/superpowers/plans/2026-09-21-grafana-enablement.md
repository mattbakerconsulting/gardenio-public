# Grafana Enablement Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking. In gardenio this plan is executed as a plan epic: one bead per task, phase epics in `phase_order`, crews dispatched by the bound plan-gardener. Each task is one branch `agent/grafana/<task-bead-id>` and one PR.

**Goal:** Stand up Grafana on gardenio-k3s reachable over ziti only, authenticated by the live Keycloak realm, showing the 26 existing dashboards against the 165 `gardenio_*` metrics already in vmsingle.

**Architecture:** The metrics half is already built and healthy (16/16 targets up), so this plan is about boot, auth and delivery. Grafana lands first WITHOUT auth, verified by port-forward, to isolate the PVC and datasource from the risky part (G9). Then OIDC is ADDED to the chart, which does not support it today (G3). Then Grafana and Keycloak are both exposed over ziti and `KC_HOSTNAME` is corrected, because OIDC redirects happen in the browser and the realm currently advertises in-cluster URLs (G4). Dashboards arrive last, via the grafana-bridge, because the chart deliberately ships none (G8).

**Tech Stack:** Helm (hand-rolled Grafana, `grafana/grafana:11.4.0`, no subchart), Go chart tests in `gardenio/helm/gardenio/tests` (own module), VictoriaMetrics (`vmsingle`/`vmagent`), Keycloak 26.x on k3s (`KC_HTTP_RELATIVE_PATH=/auth`), OpenZiti host-mode tunneler.

**Spec:** `docs/superpowers/specs/2026-09-21-grafana-enablement-design.md`. Read §0 first — G1–G10 are binding. Each task names the sections it argues from.

> **Superseded 2026-10-14 (Phase 3, Tasks 5-7, and every ziti-only statement below).**
> Exposure moved from ziti to the public edge: Grafana is at `https://grafana.clonk.tv`
> behind the edge sign-in, per plan `gd-P78ygCUgA9` (pilot `gd-iS4Uxv9tfN`). The
> ziti services in Task 5 were not the shipped design; read Phase 3 as history.
> The runbook (Task 9) was re-pointed by `gd-fmEvXckF7n`.

## Global Constraints

- Branch `agent/grafana/<task-bead-id>`, one PR per task, NOT DONE UNTIL MERGED. Cite the spec G-number(s) in the PR description.
- Chart tasks run, from `gardenio/helm/gardenio/tests` (a SEPARATE Go module): `go test ./...`. Plus `helm lint gardenio/helm/gardenio` and a render against the real overlay: `helm template gardenio gardenio/helm/gardenio -f deploy-values/gardenio-k3s.yaml --set <flags>`.
- **Never enable any `*.externalSecret.enabled`** (G7). There is no ESO on this cluster; it renders a CRD that does not exist and fails the install. Pre-seed Secrets by hand.
- **Never render an Ingress, IngressRoute, LoadBalancer, or public route** (G1). `grafana.ingress.enabled` stays `false`. Do not touch `web_ingress_cidrs` or any SG.
- Crews MAY run `helm upgrade` to roll a phase on this instance (operator ruling 2026-10-12, decision gd-ouDps1W6fq: gardenio is under active development here), using the merged overlay and an explicit `--set global.imageTag=<calver>`. A crew still does NOT run `kubectl apply`, `kubectl create secret`, or any ziti controller command; those go through an escalation to the operator. A crew whose pod has no cluster identity cannot roll anything and files the `helm_upgrade` escalation as before. Verification that needs a live cluster is requested in the same escalation.
- `values.schema.json` validates none of the grafana block (§2 item 8), so a typo'd key is silently ignored. **Every new values key MUST land with a chart test that fails when the key is absent or misspelled.** This is the only guard.
- Do not change the 26 dashboard JSONs, and do not move them into the chart (§5).
- `grafana.enabled=true` rolls the controller (§2 item 4). Say so in every PR that flips it.

## File Structure

| File | Responsibility |
|---|---|
| `deploy-values/gardenio-k3s.yaml` | The observability block (~line 608): flip `grafana`, add persistence, admin secret, nodeSelector/tolerations, later OIDC and sidecar/bridge. |
| `gardenio/helm/gardenio/values.yaml` | Declare the new `grafana.auth.oidc.*` block with documented defaults. |
| `gardenio/helm/gardenio/templates/grafana/deployment.yaml` | Render `GF_AUTH_GENERIC_OAUTH_*` and `GF_SERVER_ROOT_URL`; keep the existing render-gates intact. |
| `gardenio/helm/gardenio/tests/grafana_oidc_test.go` | New: OIDC env rendering, secret wiring, off-by-default, mutual exclusion with `auth.proxy`. |
| `gardenio/helm/gardenio/tests/grafana_k3s_overlay_test.go` | New: the k3s overlay renders a bootable Grafana (PVC + admin secret + datasource). |
| `gardenio/helm/gardenio-ziti/ziti-host-deployment.yaml` | Bind `gardenio-grafana` and `gardenio-keycloak` alongside the existing two services. |
| `gardenio/helm/gardenio-ziti/README.md` | Document the two new ziti services and the exact controller-side commands. |
| `gardenio/helm/gardenio/templates/keycloak/*` (locate first) | `KC_HOSTNAME` must become the ziti hostname; find the template that sets it. |
| `docs/` (file chosen in Task 8) | Operator runbook: how to reach Grafana, how to break-glass, what to do when OIDC fails. |

Phases (`phase_order` = [1,2,3,4]):

- **Phase 1 — boot Grafana, no auth:** Tasks 1–2. Proves PVC + datasource in isolation.
- **Phase 2 — add OIDC to the chart:** Tasks 3–4. Pure chart work, no cluster contact.
- **Phase 3 — expose over ziti and turn auth on:** Tasks 5–7. Operator-heavy.
- **Phase 4 — dashboards and runbook:** Tasks 8–9.

## Rollout (operator-only)

Three operator actions this plan cannot perform, each requested by an escalation from the task that needs it: (a) create the `gardenio-grafana-admin` Secret; (c) create two ziti services + policies on the controller and the router admit rule; (d) create the Keycloak `grafana` client and hand back its secret out of band. A crew never does any of these. (b) `helm upgrade` to roll each phase is no longer operator-only (see Global Constraints), but it needs a pod with cluster access; otherwise it is still an escalation.

## Out of scope

Spec G10: vmalert, alertmanager, vlsingle/VictoriaLogs (including `gd-q0Sy8rdaLc`, `gb logs` broken for all crews — real, adjacent, deliberately separate), vtsingle/traces, `grafana.llm.*`, `grafana.assistant.*`, imageRenderer, mcp-grafana and mcp-grafana-write, `plugins.install`, the external ACME-estate grafanaApiKey path, and `unifiedHost.paths.grafana`.

---

## Phase 1: boot Grafana, no auth

### Task 1: Make the k3s overlay render a bootable Grafana

Spec: G5, G6, G7, G9; §2 items 1, 4, 5, 6.

**Files:**
- Modify: `deploy-values/gardenio-k3s.yaml` (the observability block, `grafana: {enabled: false}` at ~line 651)
- Create: `gardenio/helm/gardenio/tests/grafana_k3s_overlay_test.go`

**Interfaces:**
- Consumes: `helmTemplateShowOnlyWithValues(t, showOnly string, valuesFiles []string, setFlags ...string) string` — resolves each values file as `chartDir(t)+"/"+f`, so the repo-root overlay is reached with `../../../deploy-values/gardenio-k3s.yaml`.
- Produces: an overlay that renders Grafana with a writable `/var/lib/grafana` and an admin password env.

- [ ] **Step 1: Write the failing overlay tests**

Create `gardenio/helm/gardenio/tests/grafana_k3s_overlay_test.go`:

```go
package tests

import (
	"strings"
	"testing"
)

const k3sOverlay = "../../../deploy-values/gardenio-k3s.yaml"

// G5: readOnlyRootFilesystem is true and the /var/lib/grafana volume renders
// ONLY when persistence (or a plugin path) is enabled. Without it Grafana
// cannot create grafana.db. A bare grafana.enabled=true is not a bootable
// Grafana, and nothing in the chart says so out loud.
func TestGrafana_K3sOverlay_HasWritableDataDir(t *testing.T) {
	out := helmTemplateShowOnlyWithValues(t,
		"templates/grafana/deployment.yaml", []string{k3sOverlay})
	if !strings.Contains(out, "/var/lib/grafana") {
		t.Fatalf("grafana must mount /var/lib/grafana or it cannot write grafana.db:\n%s", out)
	}
	if !strings.Contains(out, "readOnlyRootFilesystem: true") {
		t.Error("the hardened root filesystem must stay on")
	}
}

func TestGrafana_K3sOverlay_RendersPVC(t *testing.T) {
	out := helmTemplateShowOnlyWithValues(t,
		"templates/grafana/pvc.yaml", []string{k3sOverlay})
	if !strings.Contains(out, "PersistentVolumeClaim") {
		t.Fatalf("expected a grafana PVC, got:\n%s", out)
	}
	if !strings.Contains(out, "ReadWriteOnce") {
		t.Error("expected an RWO claim")
	}
}

// G6: both adminPassword and adminPasswordSecret default to "", which renders
// NO GF_SECURITY_ADMIN_PASSWORD at all and leaves Grafana on built-in
// admin/admin. Behind ziti is still shipped.
func TestGrafana_K3sOverlay_AdminPasswordFromSecret(t *testing.T) {
	out := helmTemplateShowOnlyWithValues(t,
		"templates/grafana/deployment.yaml", []string{k3sOverlay})
	if !strings.Contains(out, "GF_SECURITY_ADMIN_PASSWORD") {
		t.Fatalf("admin password env must render:\n%s", out)
	}
	if !strings.Contains(out, "admin-password") {
		t.Error("must read the exact secret key 'admin-password' (deployment.yaml:359)")
	}
	if strings.Contains(out, "GF_SECURITY_ADMIN_PASSWORD\n          value:") {
		t.Error("the password must come from secretKeyRef, never an inline value")
	}
}

// The datasource is already correct and already allowed by NetworkPolicy.
func TestGrafana_K3sOverlay_DatasourceIsVmsingle(t *testing.T) {
	out := helmTemplateShowOnlyWithValues(t,
		"templates/grafana/configmap.yaml", []string{k3sOverlay})
	if !strings.Contains(out, "http://gardenio-vmsingle:8428") {
		t.Errorf("expected the in-chart vmsingle datasource, got:\n%s", out)
	}
}

// G1: no public route, ever.
func TestGrafana_K3sOverlay_RendersNoIngress(t *testing.T) {
	out := helmTemplateShowOnlyWithValues(t,
		"templates/grafana/ingressroute.yaml", []string{k3sOverlay})
	if strings.Contains(out, "kind: IngressRoute") {
		t.Errorf("grafana must render NO public route (spec G1), got:\n%s", out)
	}
}

// G7: no ESO on this cluster — an ExternalSecret here fails the whole install.
func TestGrafana_K3sOverlay_RendersNoExternalSecret(t *testing.T) {
	for _, tpl := range []string{
		"templates/mcp-grafana-token-external-secret.yaml",
		"templates/grafana-bridge/token-external-secret.yaml",
	} {
		out := helmTemplateShowOnlyWithValues(t, tpl, []string{k3sOverlay})
		if strings.Contains(out, "kind: ExternalSecret") {
			t.Errorf("%s must not render on k3s (no ESO installed), got:\n%s", tpl, out)
		}
	}
}
```

> `helmTemplateShowOnlyWithValues` errors when `--show-only` matches nothing. If the helper fails rather than returning "", wrap the two negative cases (`RendersNoIngress`, `RendersNoExternalSecret`) to treat a no-match error as a PASS. Check the helper's behaviour first and adapt; do not delete the assertions.

- [ ] **Step 2: Run them to verify they fail**

Run: `cd gardenio/helm/gardenio/tests && go test ./... -run TestGrafana_K3sOverlay -v`
Expected: `HasWritableDataDir`, `RendersPVC`, `AdminPasswordFromSecret` and `DatasourceIsVmsingle` all FAIL (Grafana is disabled in the overlay, so every template renders empty). The two negative tests PASS.

- [ ] **Step 3: Turn Grafana on in the overlay**

In `deploy-values/gardenio-k3s.yaml`, replace the `grafana:      {enabled: false}` line in the observability block. Keep the surrounding comment block accurate — it currently states grafana is deliberately off, and that sentence becomes false:

```yaml
# Grafana IS on as of the grafana-enablement plan
# (docs/superpowers/plans/2026-09-21-grafana-enablement.md). vmalert and
# alertmanager stay off: they are an ALERTING tier and nothing here consumes
# them yet. vlsingle/vtsingle remain off (logs/traces) — note that vlsingle
# being off is the cause of gd-q0Sy8rdaLc, gb logs failing for crews, which is
# deliberately a separate change.
grafana:
  enabled: true

  # MANDATORY, not cosmetic. readOnlyRootFilesystem is true and the
  # /var/lib/grafana volume renders only when persistence (or a plugin path) is
  # set, so without this Grafana cannot create grafana.db. Accepted tradeoff:
  # local-path is node-local, so losing the node loses Grafana's own state
  # (users, prefs, ad-hoc boards). That is survivable because the 26 dashboards
  # are provisioned from the grafana-bridge, not authored here, and the metrics
  # live in vmsingle.
  persistence:
    enabled: true
    size: 1Gi

  # Pinned to the on-demand platform pair for the same reason vmsingle and
  # postgres are: a spot reclaim would take the volume. BOTH labels required —
  # lifecycle=on-demand alone also matches the t3.small k3s server.
  nodeSelector:
    gardenio.io/lifecycle: on-demand
    gardenio.io/role: platform
  tolerations:
    - key: "gardenio.agent"
      operator: "Equal"
      value: "true"
      effect: "NoSchedule"

  # Operator pre-creates this Secret with key exactly 'admin-password'
  # (templates/grafana/deployment.yaml:359). Without it Grafana falls back to
  # built-in admin/admin. This account stays as BREAK-GLASS only once OIDC
  # lands in phase 3.
  adminPasswordSecret: "gardenio-grafana-admin"

  # No public route, ever (spec G1). ziti is the only path in.
  ingress:
    enabled: false
```

- [ ] **Step 4: Run the tests to verify they pass**

Run: `cd gardenio/helm/gardenio/tests && go test ./... -run TestGrafana_K3sOverlay -v`
Expected: all six PASS.

- [ ] **Step 5: Verify the whole chart still renders and lints**

```bash
helm lint gardenio/helm/gardenio
helm template gardenio gardenio/helm/gardenio -f deploy-values/gardenio-k3s.yaml > /dev/null && echo "render OK"
cd gardenio/helm/gardenio/tests && go test ./...
```
Expected: lint reports 0 failed; the full render exits 0; the whole chart-test module passes. If an unrelated test breaks, that is a real regression from flipping the flag — fix it or explain it in the PR, do not skip it.

- [ ] **Step 6: Commit**

```bash
git add deploy-values/gardenio-k3s.yaml gardenio/helm/gardenio/tests/grafana_k3s_overlay_test.go
git commit -m "feat(grafana): enable grafana on k3s with a writable data dir and admin secret"
```

- [ ] **Step 7: Push and open the PR**

Body must state: Grafana is ziti-only with no route rendered; `grafana.enabled=true` **rolls the controller** because `controller/deployment.yaml:646-648` injects `GRAFANA_URL`; and the rollout needs the operator to create the Secret first (Task 2).

---

### Task 2: Roll it out and prove it boots

Spec: G9; §1. **No code.**

**Files:** none.

**Interfaces:**
- Consumes: Task 1 merged.
- Produces: a running Grafana pod and a verified datasource.

- [ ] **Step 1: Confirm Task 1 is merged**

```bash
gh pr list --state merged --search "grafana" --limit 10
```
If unmerged, comment on this task bead and STOP.

- [ ] **Step 2: Read the rollout recipe — do not invent it**

```bash
sed -n '1,80p' scripts/deploy-byo.sh
grep -n "imageTag" deploy-values/gardenio-k3s.yaml
```
Record the documented invocation and the CURRENT `global.imageTag` verbatim.

- [ ] **Step 3: File ONE escalation**

Use only the flags `gb escalation create --help` prints. Body, verbatim:

```
Rollout request: Grafana phase 1 (plan 2026-09-21-grafana-enablement, Task 1 merged)

PRE-STEP (operator, before the upgrade) — create the admin Secret:
  kubectl -n gardenio create secret generic gardenio-grafana-admin \
    --from-literal=admin-password='<choose a strong password, store it in your manager>'
  The key MUST be exactly 'admin-password' (templates/grafana/deployment.yaml:359).

ROLL:
  <exact invocation from scripts/deploy-byo.sh, -f deploy-values/gardenio-k3s.yaml,
   CURRENT global.imageTag preserved>
  NOTE: this rolls the CONTROLLER too (GRAFANA_URL is injected when
  grafana.enabled). Do not run it while a migration is mid-flight.

VERIFY 1 — it boots and stays up:
  kubectl -n gardenio get pods -l app.kubernetes.io/name=grafana
  EXPECT: 1/1 Running, 0 restarts after 2 minutes.
  kubectl -n gardenio logs -l app.kubernetes.io/name=grafana --tail=50 | grep -i "error\|denied\|read-only"
  EXPECT: no read-only-filesystem errors, no sqlite errors.

VERIFY 2 — the PVC bound:
  kubectl -n gardenio get pvc | grep grafana
  EXPECT: Bound, 1Gi, local-path.

VERIFY 3 — the datasource works (this is the point of phase 1):
  kubectl -n gardenio port-forward svc/gardenio-grafana 3000:3000
  then, logged in as admin with the Secret's password:
    Connections -> Data sources -> VictoriaMetrics -> "Save & test"
  EXPECT: "Data source is working".
  Then Explore -> query: sum(gardenio_agent_pods_running) or any gardenio_* series
  EXPECT: data points, not "No data".

VERIFY 4 — still no public route:
  kubectl -n gardenio get ingressroute
  EXPECT: only gardenio-https-redirect. NO grafana route.

Paste all four outputs into the escalation result.
```

- [ ] **Step 4: Close only on a verified pass**

On `success` with all four outputs, comment them on this task bead and close it. On failure, comment the diagnosis and leave it open. Do not close on an unverified rollout.

---

## Phase 2: add OIDC to the chart

### Task 3: Declare and render `grafana.auth.oidc.*`

Spec: G2, G3; §3, §4.

**Files:**
- Modify: `gardenio/helm/gardenio/values.yaml` (inside the `auth:` block at ~9651, beside `proxy:`)
- Modify: `gardenio/helm/gardenio/templates/grafana/deployment.yaml` (env block near the existing `GF_AUTH_*` / `GF_SECURITY_*` renders, ~line 359-430)
- Create: `gardenio/helm/gardenio/tests/grafana_oidc_test.go`

**Interfaces:**
- Consumes: `helmTemplate(t, showOnly string, setFlags ...string) string`.
- Produces: values keys `grafana.auth.oidc.{enabled,name,clientId,clientSecretSecret,clientSecretKey,authURL,tokenURL,apiURL,scopes,roleAttributePath,allowSignUp,autoLogin}` and `grafana.rootURL` consumed by `GF_SERVER_ROOT_URL`.

- [ ] **Step 1: Write the failing OIDC tests**

Create `gardenio/helm/gardenio/tests/grafana_oidc_test.go`:

```go
package tests

import (
	"strings"
	"testing"
)

func oidcRender(t *testing.T, extra ...string) string {
	t.Helper()
	args := append([]string{
		"grafana.enabled=true",
		"grafana.persistence.enabled=true",
	}, extra...)
	return helmTemplate(t, "templates/grafana/deployment.yaml", args...)
}

// Off by default: enabling grafana must not silently turn on an auth flow.
func TestGrafanaOIDC_DisabledByDefault(t *testing.T) {
	out := oidcRender(t)
	if strings.Contains(out, "GF_AUTH_GENERIC_OAUTH_ENABLED") {
		t.Errorf("OIDC env must not render unless explicitly enabled:\n%s", out)
	}
}

func TestGrafanaOIDC_RendersEnv(t *testing.T) {
	out := oidcRender(t,
		"grafana.auth.oidc.enabled=true",
		"grafana.auth.oidc.clientId=grafana",
		"grafana.auth.oidc.clientSecretSecret=gardenio-grafana-oidc",
		"grafana.auth.oidc.authURL=http://gardenio.keycloak.ziti:8080/auth/realms/gardenio/protocol/openid-connect/auth",
		"grafana.auth.oidc.tokenURL=http://gardenio-keycloak-runtime:8080/auth/realms/gardenio/protocol/openid-connect/token",
		"grafana.auth.oidc.apiURL=http://gardenio-keycloak-runtime:8080/auth/realms/gardenio/protocol/openid-connect/userinfo",
		"grafana.rootURL=http://gardenio.grafana.ziti:3000",
	)
	for _, want := range []string{
		"GF_AUTH_GENERIC_OAUTH_ENABLED",
		"GF_AUTH_GENERIC_OAUTH_CLIENT_ID",
		"GF_AUTH_GENERIC_OAUTH_CLIENT_SECRET",
		"GF_AUTH_GENERIC_OAUTH_AUTH_URL",
		"GF_AUTH_GENERIC_OAUTH_TOKEN_URL",
		"GF_AUTH_GENERIC_OAUTH_API_URL",
		"GF_SERVER_ROOT_URL",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %s in:\n%s", want, out)
		}
	}
	// The browser follows auth_url, so it must be the ziti host, while the
	// back-channel token/userinfo calls stay in-cluster (spec G4, §3).
	if !strings.Contains(out, "gardenio.keycloak.ziti") {
		t.Error("auth_url must be the browser-reachable ziti hostname")
	}
}

// The client secret is a credential: secretKeyRef only, never an inline value.
func TestGrafanaOIDC_ClientSecretFromSecretRef(t *testing.T) {
	out := oidcRender(t,
		"grafana.auth.oidc.enabled=true",
		"grafana.auth.oidc.clientId=grafana",
		"grafana.auth.oidc.clientSecretSecret=gardenio-grafana-oidc",
	)
	i := strings.Index(out, "GF_AUTH_GENERIC_OAUTH_CLIENT_SECRET")
	if i < 0 {
		t.Fatal("client secret env not rendered")
	}
	if !strings.Contains(out[i:min(i+250, len(out))], "secretKeyRef") {
		t.Errorf("client secret must come from secretKeyRef:\n%s", out[i:])
	}
}

// A misconfigured OIDC block must fail the RENDER, not boot a Grafana that
// silently falls back to local login. values.schema.json validates none of
// this (spec §2 item 8), so the template gate is the only guard.
func TestGrafanaOIDC_RequiresClientIdAndSecret(t *testing.T) {
	_, err := helmTemplateErr(t, "templates/grafana/deployment.yaml",
		"grafana.enabled=true",
		"grafana.persistence.enabled=true",
		"grafana.auth.oidc.enabled=true",
	)
	if err == nil {
		t.Error("oidc.enabled with no clientId/clientSecretSecret must fail the render")
	}
}

// Two auth mechanisms at once is a misconfiguration, not a feature.
func TestGrafanaOIDC_MutuallyExclusiveWithAuthProxy(t *testing.T) {
	_, err := helmTemplateErr(t, "templates/grafana/deployment.yaml",
		"grafana.enabled=true",
		"grafana.persistence.enabled=true",
		"grafana.auth.oidc.enabled=true",
		"grafana.auth.oidc.clientId=grafana",
		"grafana.auth.oidc.clientSecretSecret=s",
		"grafana.auth.proxy.enabled=true",
	)
	if err == nil {
		t.Error("oidc and the header auth-proxy must not both be enabled")
	}
}
```

> This needs a `helmTemplateErr` helper that returns `(string, error)` instead of failing the test. Check whether the tests module already has one (`grep -rn "func helmTemplate" gardenio/helm/gardenio/tests/`); if not, add it next to `helmTemplate` and have `helmTemplate` call it. Do not duplicate the exec logic. `min` is builtin in Go 1.21+.

- [ ] **Step 2: Run them to verify they fail**

Run: `cd gardenio/helm/gardenio/tests && go test ./... -run TestGrafanaOIDC -v`
Expected: `DisabledByDefault` PASSES; every other case FAILS (no OIDC support exists).

- [ ] **Step 3: Declare the values**

In `values.yaml`, inside the existing `grafana.auth:` block beside `proxy:`:

```yaml
    # Keycloak/OIDC login (spec G2/G3). OFF by default: enabling grafana must
    # never silently turn on an auth flow.
    #
    # SPLIT URLS ARE DELIBERATE. authURL is followed by the BROWSER, so it must
    # be a hostname the operator's machine can resolve — on this deployment the
    # ziti name. tokenURL and apiURL are Grafana's own back-channel calls and
    # should stay on the in-cluster Service. Pointing authURL at the in-cluster
    # name produces a login loop, which is exactly what the live realm's
    # discovery document advertises today.
    oidc:
      enabled: false
      name: "Keycloak"
      clientId: ""
      # Secret holding the client secret. Key defaults to 'client-secret'.
      # NEVER inline the secret in values.
      clientSecretSecret: ""
      clientSecretKey: "client-secret"
      authURL: ""
      tokenURL: ""
      apiURL: ""
      scopes: "openid profile email"
      # e.g. contains(groups[*], 'gardenio-admins') && 'Admin' || 'Viewer'
      roleAttributePath: ""
      allowSignUp: true
      autoLogin: false
```

- [ ] **Step 4: Render the env and the gates**

In `templates/grafana/deployment.yaml`, beside the existing `GF_SECURITY_*` env, add a block guarded by `.Values.grafana.auth.oidc.enabled` that renders `GF_AUTH_GENERIC_OAUTH_{ENABLED,NAME,CLIENT_ID,SCOPES,AUTH_URL,TOKEN_URL,API_URL,ROLE_ATTRIBUTE_PATH,ALLOW_SIGN_UP,AUTO_LOGIN}`, with `CLIENT_SECRET` from `secretKeyRef` on `clientSecretSecret`/`clientSecretKey`. Render `GF_SERVER_ROOT_URL` from `.Values.grafana.rootURL` when set. Add two `fail` gates matching the file's existing style (`:63`, `:92`, `:121`, `:124`, `:134`):

```
{{- if .Values.grafana.auth.oidc.enabled }}
  {{- if or (not .Values.grafana.auth.oidc.clientId) (not .Values.grafana.auth.oidc.clientSecretSecret) }}
    {{- fail "grafana.auth.oidc.enabled requires both clientId and clientSecretSecret" }}
  {{- end }}
  {{- if .Values.grafana.auth.proxy.enabled }}
    {{- fail "grafana.auth.oidc and grafana.auth.proxy are mutually exclusive" }}
  {{- end }}
{{- end }}
```

Do not touch the hardcoded `GF_AUTH_ANONYMOUS_ENABLED: "false"`.

- [ ] **Step 5: Run to verify they pass**

Run: `cd gardenio/helm/gardenio/tests && go test ./... -run TestGrafanaOIDC -v`
Expected: all five PASS.

- [ ] **Step 6: Full chart verification and commit**

```bash
helm lint gardenio/helm/gardenio
cd gardenio/helm/gardenio/tests && go test ./...
cd - && git add gardenio/helm/gardenio/values.yaml \
        gardenio/helm/gardenio/templates/grafana/deployment.yaml \
        gardenio/helm/gardenio/tests/grafana_oidc_test.go
git commit -m "feat(grafana): add Keycloak OIDC login support to the chart"
```

- [ ] **Step 7: Push and open the PR** (cite G3; state that no deployment changes behaviour until Task 6 sets the values).

---

### Task 4: Pin `disableLoginForm` and break-glass behaviour

Spec: G2, G6.

**Files:**
- Modify: `gardenio/helm/gardenio/templates/grafana/deployment.yaml` (render `GF_AUTH_DISABLE_LOGIN_FORM`)
- Modify: `gardenio/helm/gardenio/tests/grafana_oidc_test.go`

**Interfaces:**
- Consumes: `grafana.auth.disableLoginForm` (`values.yaml:9655`, default `false`) and Task 3's OIDC block.
- Produces: `GF_AUTH_DISABLE_LOGIN_FORM` rendered from that key.

- [ ] **Step 1: Write the failing tests**

Append to `grafana_oidc_test.go`:

```go
// The local admin account is BREAK-GLASS. The login form must therefore still
// be reachable by default even with OIDC on, and hiding it must be a
// deliberate, tested choice rather than an accident of enabling OIDC.
func TestGrafanaOIDC_LoginFormStaysByDefault(t *testing.T) {
	out := oidcRender(t,
		"grafana.auth.oidc.enabled=true",
		"grafana.auth.oidc.clientId=grafana",
		"grafana.auth.oidc.clientSecretSecret=s",
	)
	if strings.Contains(out, `GF_AUTH_DISABLE_LOGIN_FORM`) &&
		strings.Contains(out, `"true"`) &&
		!strings.Contains(out, "grafana.auth.disableLoginForm") {
		t.Error("enabling OIDC must not implicitly disable the break-glass login form")
	}
}

func TestGrafanaOIDC_LoginFormCanBeDisabled(t *testing.T) {
	out := oidcRender(t,
		"grafana.auth.oidc.enabled=true",
		"grafana.auth.oidc.clientId=grafana",
		"grafana.auth.oidc.clientSecretSecret=s",
		"grafana.auth.disableLoginForm=true",
	)
	if !strings.Contains(out, "GF_AUTH_DISABLE_LOGIN_FORM") {
		t.Errorf("disableLoginForm=true must render the env:\n%s", out)
	}
}
```

- [ ] **Step 2: Run to verify `LoginFormCanBeDisabled` fails**

Run: `cd gardenio/helm/gardenio/tests && go test ./... -run TestGrafanaOIDC_LoginForm -v`
Expected: `LoginFormCanBeDisabled` FAILS (the env is never rendered today).

- [ ] **Step 3: Render it**

Add `GF_AUTH_DISABLE_LOGIN_FORM` to the deployment env, quoted from
`.Values.grafana.auth.disableLoginForm`, rendered unconditionally (the key
already exists with a `false` default).

- [ ] **Step 4: Run to verify both pass**

Run: `cd gardenio/helm/gardenio/tests && go test ./... -run TestGrafanaOIDC -v`
Expected: all PASS.

- [ ] **Step 5: Commit and open the PR**

```bash
cd gardenio/helm/gardenio/tests && go test ./...
cd - && git add gardenio/helm/gardenio/templates/grafana/deployment.yaml gardenio/helm/gardenio/tests/grafana_oidc_test.go
git commit -m "feat(grafana): render GF_AUTH_DISABLE_LOGIN_FORM for break-glass control"
```

---

## Phase 3: expose over ziti and turn auth on

### Task 5: Bind Grafana and Keycloak on the ziti host, and document the controller side

Spec: G1, G4; §3.

**Files:**
- Modify: `gardenio/helm/gardenio-ziti/ziti-host-deployment.yaml` (header comment + the bound services)
- Modify: `gardenio/helm/gardenio-ziti/README.md` ("The nine ziti services" section and the rebuild recipe)

**Interfaces:**
- Consumes: the existing two bindings (`gardenio-viewer` → `gardenio-beads-viewer...:8095`, `gardenio-beads` → `gardenio-beads...:8080`).
- Produces: bindings for `gardenio-grafana` → `gardenio-grafana.gardenio.svc.cluster.local:3000` and `gardenio-keycloak` → `gardenio-keycloak-runtime.gardenio.svc.cluster.local:8080`.

- [ ] **Step 1: Read how the two existing services are bound**

```bash
sed -n '1,120p' gardenio/helm/gardenio-ziti/ziti-host-deployment.yaml
sed -n '29,110p' gardenio/helm/gardenio-ziti/README.md
```
The tunneler runs in **host mode only** — it never intercepts. Match the existing shape exactly; do not invent a different mechanism.

- [ ] **Step 2: Add the two bindings**

Extend the manifest the same way the existing two are expressed, and update the header comment (currently lists exactly two services) to list four, with the same `->` arrow format.

- [ ] **Step 3: Document the controller-side commands**

In `README.md`, beside the existing `ziti edge create service gardenio-viewer` example, add the exact commands for both new services — `host.v1` config, `intercept.v1` config, the service, the Bind policy (`--identity-roles '#gardenio-hosts'`), the Dial policy (`--identity-roles '#gc-admins'`), and the in-AWS router named-identity admit rule that the README already flags as "REQUIRED, easily missed". Use `gardenio.grafana.ziti:3000` and `gardenio.keycloak.ziti:8080` as the intercept addresses. Update the "nine ziti services" heading and count.

- [ ] **Step 4: Verify nothing else changed**

```bash
git diff --stat
kubectl --kubeconfig ~/.kube/gardenio-ziti.yaml -n gardenio get deploy ziti-host -o yaml | head -40
```
The live Deployment is applied by hand (`kubectl apply -f`), not by Helm — say so in the PR so the operator knows this file needs an explicit apply, not a chart upgrade.

- [ ] **Step 5: Commit and open the PR**

```bash
git add gardenio/helm/gardenio-ziti/ziti-host-deployment.yaml gardenio/helm/gardenio-ziti/README.md
git commit -m "feat(ziti): bind grafana and keycloak services on the ziti host"
```

---

### Task 6: Point Keycloak at its ziti hostname

Spec: G4; §3.

**Files:**
- Modify: wherever `KC_HOSTNAME` is set (find it — `grep -rn "KC_HOSTNAME" gardenio/helm/`)
- Test: add a chart test asserting the rendered `KC_HOSTNAME`

**Interfaces:**
- Consumes: the live value `KC_HOSTNAME=gardenio-keycloak-runtime.gardenio.svc.cluster.local`, `KC_HOSTNAME_STRICT=false`, `KC_HTTP_RELATIVE_PATH=/auth`.
- Produces: a values key for the browser-facing Keycloak hostname, defaulting to today's in-cluster value so no other deployment changes.

- [ ] **Step 1: Locate the setting and confirm the live state**

```bash
grep -rn "KC_HOSTNAME\|hostnameStrict\|relativePath" gardenio/helm/gardenio/ | grep -v _test
```
Confirm against the live StatefulSet before editing:
```bash
kubectl --kubeconfig ~/.kube/gardenio-ziti.yaml -n gardenio get sts gardenio-keycloak-runtime \
  -o jsonpath='{range .spec.template.spec.containers[0].env[*]}{.name}={.value}{"\n"}{end}' | grep KC_
```

- [ ] **Step 2: Write the failing test**

Assert that with the new key set, the rendered Keycloak workload carries
`KC_HOSTNAME=gardenio.keycloak.ziti` (or whatever key name you chose), and that
with it unset the rendered value is unchanged from today's in-cluster DNS name.
Model the test on the existing keycloak chart tests if any exist
(`ls gardenio/helm/gardenio/tests | grep -i keycloak`); otherwise follow
`grafana_test.go` style.

- [ ] **Step 3: Run to verify it fails, implement, run to verify it passes**

Run: `cd gardenio/helm/gardenio/tests && go test ./... -run '<your test name>' -v`

- [ ] **Step 4: Set it in the overlay**

In `deploy-values/gardenio-k3s.yaml`, set the browser-facing hostname to the
ziti name, with a comment explaining that OIDC redirects are followed by the
browser and the in-cluster name is unreachable from there.

- [ ] **Step 5: Commit and open the PR**

Warn in the body: changing `KC_HOSTNAME` affects **every** Keycloak client, not
just Grafana. Confirm in the PR which clients exist in realm `gardenio` before
merging — `gardenio-traefik-forwardauth` is known to exist and its forwardAuth
is currently off, but verify rather than assume.

---

### Task 7: Turn OIDC on and verify the login end to end

Spec: G2, G4, G9. **No code.**

**Files:** none.

**Interfaces:**
- Consumes: Tasks 3–6 merged and rolled out.
- Produces: a verified Keycloak-authenticated Grafana.

- [ ] **Step 1: Confirm Tasks 3, 4, 5 and 6 are all merged**

If any is not, comment on this bead and STOP. This task cannot be partially done.

- [ ] **Step 2: File ONE escalation covering the operator steps**

Body, verbatim:

```
Grafana phase 3: ziti exposure + Keycloak OIDC
(plan 2026-09-21-grafana-enablement, Tasks 3-6 merged)

OPERATOR STEP A — Keycloak client, in realm 'gardenio':
  clientId: grafana
  access type: confidential (client authentication ON)
  standard flow: ON ; direct access grants: OFF
  valid redirect URI:  http://gardenio.grafana.ziti:3000/login/generic_oauth
  valid post-logout redirect URI: http://gardenio.grafana.ziti:3000/*
  web origins: http://gardenio.grafana.ziti:3000
  Add a group or client-role mapper into the token so Grafana can map roles.
  Then hand the client secret over out of band and create:
    kubectl -n gardenio create secret generic gardenio-grafana-oidc \
      --from-literal=client-secret='<the client secret>'
  Key MUST be exactly 'client-secret'.

OPERATOR STEP B — two ziti services on the controller:
  Follow gardenio/helm/gardenio-ziti/README.md (updated in Task 5) for
  gardenio-grafana (:3000) and gardenio-keycloak (:8080): host.v1 + intercept.v1
  configs, the service, Bind policy for #gardenio-hosts, Dial policy for
  #gc-admins, and the in-AWS router named-identity admit rule.
  Then: kubectl apply -f gardenio/helm/gardenio-ziti/ziti-host-deployment.yaml

OPERATOR STEP C — set the values and roll:
  grafana.rootURL: http://gardenio.grafana.ziti:3000
  grafana.auth.oidc.enabled: true
  grafana.auth.oidc.clientId: grafana
  grafana.auth.oidc.clientSecretSecret: gardenio-grafana-oidc
  grafana.auth.oidc.authURL:  http://gardenio.keycloak.ziti:8080/auth/realms/gardenio/protocol/openid-connect/auth
  grafana.auth.oidc.tokenURL: http://gardenio-keycloak-runtime:8080/auth/realms/gardenio/protocol/openid-connect/token
  grafana.auth.oidc.apiURL:   http://gardenio-keycloak-runtime:8080/auth/realms/gardenio/protocol/openid-connect/userinfo
  (authURL is browser-facing = ziti; token/api are back-channel = in-cluster.)
  Keep grafana.auth.disableLoginForm FALSE for now — break-glass.
  Then the usual deploy-byo.sh upgrade.

VERIFY 1 — discovery advertises the ziti hostname:
  curl -s http://gardenio.keycloak.ziti:8080/auth/realms/gardenio/.well-known/openid-configuration | jq -r .authorization_endpoint
  EXPECT: a gardenio.keycloak.ziti URL, NOT *.svc.cluster.local.

VERIFY 2 — the login round-trips:
  open http://gardenio.grafana.ziti:3000
  EXPECT: a "Sign in with Keycloak" button; clicking it reaches the Keycloak
  login page (not a connection error), and a successful login lands back in
  Grafana with your Keycloak identity in the user menu.

VERIFY 3 — the datasource still works as the OIDC user:
  Explore -> any gardenio_* series. EXPECT data points.

VERIFY 4 — still no public route, and ziti is still the only path:
  kubectl -n gardenio get ingressroute
  EXPECT: only gardenio-https-redirect.

Paste all four outputs into the result.
```

- [ ] **Step 3: On a verified pass, consider disabling the login form**

Only once VERIFY 2 has passed for at least one real operator login, propose (do
not perform) setting `grafana.auth.disableLoginForm: true`. File it as a
follow-up bead; leaving break-glass on is the safer default until OIDC has been
exercised for a few days.

- [ ] **Step 4: Close only on a verified pass**

---

## Phase 4: dashboards and runbook

### Task 8: Deliver the 26 dashboards via the bridge

Spec: G8; §1, §2 item 7.

**Files:**
- Modify: `deploy-values/gardenio-k3s.yaml` (`grafana.sidecar`, `grafanaBridge`)
- Create: `gardenio/helm/gardenio/tests/grafana_dashboards_delivery_test.go`

**Interfaces:**
- Consumes: `grafana.sidecar.enabled` (`values.yaml:9681`), `grafanaBridge.enabled`, `grafanaBridge.backfill.enabled` (`values.yaml:11737`, default `false`), and `templates/grafana/rbac.yaml` (gated on `sidecar.enabled`).
- Produces: dashboards visible in Grafana.

- [ ] **Step 1: Verify the bridge image actually exists — BEFORE anything else**

```bash
grep -rn "grafana-bridge" deploy-values/gardenio-k3s.yaml gardenio/helm/gardenio/values.yaml | head
```
Resolve the full image ref, then confirm the tag is published. If it is not, STOP and comment on this bead: the rest of the task will fail on ImagePullBackOff, and building/publishing that image is a separate piece of work.

- [ ] **Step 2: Write the failing delivery tests**

Assert against the k3s overlay that: the sidecar container renders; the grafana
ServiceAccount + Role + RoleBinding render (they are gated on `sidecar.enabled`,
so this is the guard against a dangling SA); the `dashboards.yaml` provisioning
entry appears in the grafana ConfigMap; and the grafana-bridge Deployment
renders with backfill on. Use `helmTemplateShowOnlyWithValues` with
`k3sOverlay` as in Task 1.

- [ ] **Step 3: Run to verify they fail**

Run: `cd gardenio/helm/gardenio/tests && go test ./... -run TestGrafanaDashboards -v`

- [ ] **Step 4: Enable sidecar and bridge in the overlay**

```yaml
  # Dashboards are NOT shipped by the chart — it renders no dashboard
  # ConfigMaps because ~200KB per revision overflowed the 1 MiB release-Secret
  # limit (values.yaml:11732-11734). The 26 JSONs under
  # gardenio/helm/gardenio/dashboards/ are baked into the grafana-bridge image
  # and reach Grafana as runtime ConfigMaps that this sidecar watches.
  sidecar:
    enabled: true
```
plus the `grafanaBridge` block with `enabled: true` and `backfill.enabled: true`.
**Do not** set `grafanaBridge.externalSecret.enabled` (G7).

- [ ] **Step 5: Run to verify they pass, then full chart verification**

```bash
cd gardenio/helm/gardenio/tests && go test ./...
helm lint gardenio/helm/gardenio
helm template gardenio gardenio/helm/gardenio -f deploy-values/gardenio-k3s.yaml > /dev/null && echo "render OK"
```

- [ ] **Step 6: Commit, PR, and file the rollout escalation**

The escalation must verify: the bridge pod is Running (not ImagePullBackOff);
`kubectl -n gardenio get cm -l grafana_dashboard=1 | wc -l` is non-zero; and at
least three named dashboards (cost-overview, system-health, fleet-capacity)
render with data rather than "No data".

---

### Task 9: Operator runbook

Spec: §3, §5.

**Files:**
- Create or extend one doc under `docs/` (pick the file that already covers cluster access; `grep -rln "ziti" docs/ | head` before creating a new one)

- [ ] **Step 1: Write the runbook**

It must answer, concretely: how to reach Grafana (the ziti hostname and that it
requires a `#gc-admins` identity); how to break-glass with the local admin
account and where its Secret lives; what to do when OIDC login fails (check
discovery's `authorization_endpoint` first — if it shows `.svc.cluster.local`,
`KC_HOSTNAME` regressed); that Grafana's PVC is node-local and what is lost if
the node dies (users/prefs/ad-hoc boards, NOT the 26 provisioned dashboards and
NOT the metrics); that dashboards come from the bridge, so a missing board is a
bridge problem not a Grafana one; and that `grafana.enabled` rolls the
controller.

- [ ] **Step 2: Cross-link and commit**

Link the spec and this plan. Commit and open the PR.
