# Project-Scope Hardening Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking. In gardenio this plan is executed as a plan epic: one bead per task below, phase epics in `phase_order`, crews dispatched by the bound plan-gardener. Each task is one branch `agent/project-scope/<task-bead-id>` and one PR.

**Goal:** Make project scoping in gardenio truthful and fail-loud: the `/claude-config` matrix shows what pods actually get, three silent fail-open/ordering gaps are closed, and `cmd/gb` has one project resolver instead of four.

**Architecture:** No new concepts. `project` stays the code-scope selector and `BOAT_PROJECT` stays the pod contract (spec D1). The viewer stops resolving the whole matrix against one viewer-wide project and resolves per (project, role) with the same `internal/claudeconfig` path pods use (D2), after an interim warning banner ships first (D3). Three guard fixes land in `kd claim`, the pool init loop, and the trigger engine (D4–D6). One `resolveProject` in `cmd/gb` replaces four precedence rules and the dead `BOAT_RIG` alias (D7). `projectCompatible` is aligned to `MatchesSubscriptions` (D8).

**Tech Stack:** Go 1.25.12 (`go.work` uses `./gardenio/controller` and `./kbeads`; `gardenio/helm/gardenio/tests` is its OWN module), `html/template` in `cmd/beads-viewer`, Helm chart tests via `helmTemplate`/`helmTemplateShowOnlyWithValues` + `strings.Contains`, bash e2e in `tests/e2e/scripts`.

**Spec:** `docs/superpowers/specs/2026-09-21-project-scope-hardening-design.md`. Read §0 first — the decision table is binding. Each task names the spec sections it argues from.

## Global Constraints

- Branch `agent/project-scope/<task-bead-id>`, one PR per task, NOT DONE UNTIL MERGED. Cite the spec D-number(s) in the PR description.
- Before opening a PR, from the module root that owns the package: `gofumpt -l .` (must print nothing), `go vet ./...`, `staticcheck ./...`, and `go test ./<touched pkgs>/...`. Chart tasks additionally run `cd gardenio/helm/gardenio/tests && go test ./...` (separate module) and `helm lint gardenio/helm/gardenio`.
- Every code task is test-first. No step may say "add tests", "handle errors", "TBD", or show a code step without a code block.
- Never touch terraform, RBAC, secrets, ziti config, or `values.schema.json`. Never run `helm upgrade`, `kubectl apply`, or any cluster mutation — rollout is operator-only and is requested via the Task 4 escalation.
- No `kd`/`gb` writes except: comments and close on your own task bead, and the single Task 4 `gb escalation create`.
- Do not change `internal/advice/matching.go` semantics. Spec §3 is the contract; only `projectCompatible` moves (Task 8).
- Preserve the pod write path's resolution behaviour (spec §6): a pod with no project still resolves `global` + `role:` only. This plan makes that **loud**, not different.
- `kbeads`' `agentProject()` precedence is unchanged (D7). No shared cross-module project package.

## File Structure

| File | Responsibility |
|---|---|
| `gardenio/helm/gardenio/values.yaml` | Declare `global.project`; document that it feeds BOTH the metrics label and `/claude-config` scope. |
| `deploy-values/gardenio-k3s.yaml` | Set `beadsViewer.project: gardenio` for this deployment. |
| `gardenio/helm/gardenio/tests/beads_viewer_project_test.go` | Chart tests pinning default-off, `global.project`, override, and the deploy-values render. |
| `gardenio/controller/internal/claudeconfig/resolve.go` | `CountProjectScopedSettings`, `listSettingsRoleScopes`, AND-semantics `projectCompatible`. |
| `gardenio/controller/internal/claudeconfig/effective.go` | `RoleScope`, `DiscoverRoleScopes`. |
| `gardenio/controller/cmd/beads-viewer/claude_config.go` | Scope banner (T2), then per-(project, role) rows + `?project=` filter (T3). |
| `gardenio/controller/cmd/beads-viewer/templates/claude_config.html`, `claude_config_detail.html` | Banner, project column, filter. |
| `gardenio/controller/cmd/beads-viewer/main.go` | Startup warning when no viewer project is configured. |
| `kbeads/cmd/kd/claim.go` | `checkProjectMatch`: warn on empty project, fail closed on fetch error. |
| `gardenio/controller/cmd/gb/init_agent.go`, `init_agent_loop.go`, `init_agent_setup.go` | Re-materialize Claude config after project/role hydration. |
| `gardenio/controller/internal/bridge/triggers.go` | Refuse `spawn_agent` on an empty project. |
| `gardenio/controller/cmd/gb/project.go`, `main.go`, `setup_config.go`, `setup_repos.go` | One `resolveProject`; warn on empty; drop `BOAT_RIG`. |
| `tests/e2e/scripts/test-config-subscriptions.sh` | Drop the `BOAT_RIG` scenarios. |
| `docs/agent-operations.md`, `gardenio/docs/compaction-settings.md`, `gardenio/docs/config-inventory.md`, `gardenio/docs/onboarding-new-project-and-role.md`, `gardenio/docs/research/spawn-escalated-agent/config-bead-surface.md` | Corrections + the published project-scope contract. |

Phases (`phase_order` = [1,2,3]):

- **Phase 1 — make `/claude-config` truthful:** Tasks 1–4, strictly ordered 1 → 2 → 3 → 4.
- **Phase 2 — close the fail-open and ordering gaps:** Tasks 5–8, mutually independent, independent of Phase 1.
- **Phase 3 — one resolver and correct docs:** Tasks 9–10; Task 10 lands after Task 9.

## Rollout (operator-only)

Task 1 changes chart values; nothing reaches the cluster until the operator rolls the release. Task 4 files one escalation carrying the exact recipe and verification, and closes only on a verified pass. A crew never runs `helm upgrade`.

## Out of scope

Spec D9, filed by the gardener as separate follow-up beads when Phase 3 closes: kbeads hooks `Enrich` drift; `slack-bridge` `bridge-notifications` first-bead-wins; the 9 Rust / 2 shell / 2 Python `BOAT_PROJECT` reads; jira-bridge `BOAT_PROJECTS`; `images/agent/resolve-auth.sh`; the `type:org` primitive; role resolution not being project-scoped (`gd-GDPF1nH95g`).

---

## Phase 1: make /claude-config truthful

### Task 1: Chart — declare `global.project`, wire this deployment, pin it with tests

Spec: §0 D1, §2 items 1–2, §5 "Chart".

**Files:**
- Modify: `gardenio/helm/gardenio/values.yaml` (the `global:` block beginning line 2039; and the `beadsViewer.project` comment at 10829-10832)
- Modify: `gardenio/helm/gardenio/templates/beads-viewer/deployment.yaml:85-90` (comment only)
- Modify: `deploy-values/gardenio-k3s.yaml` (git-tracked; add `beadsViewer.project`)
- Create: `gardenio/helm/gardenio/tests/beads_viewer_project_test.go`

**Interfaces:**
- Consumes: `helmTemplate(t *testing.T, showOnly string, setFlags ...string) string` (`tests/grafana_test.go:24`); `helmTemplateShowOnlyWithValues(t *testing.T, showOnly string, valuesFiles []string, setFlags ...string) string` (`tests/slack_bridge_assistant_pane_alert_test.go:213`) — note it resolves each values file as `chartDir(t)+"/"+f`, so a repo-root file is reached with `../../../`.
- Produces: chart value `global.project` (string, default `""`); `beadsViewer.project` unchanged in behaviour.

- [ ] **Step 1: Write the failing chart tests**

Create `gardenio/helm/gardenio/tests/beads_viewer_project_test.go`:

```go
package tests

import (
	"strings"
	"testing"
)

// The beads-viewer template sets BOAT_PROJECT from
// `.Values.beadsViewer.project | default .Values.global.project`. That value
// feeds TWO consumers: the gardenio_operator_activity_total metric label
// (ka-0000006752) and the /claude-config effective-config resolution scope.
// Until this task, global.project was never declared in values.yaml, so a
// default render silently emitted no BOAT_PROJECT at all.

func TestBeadsViewer_Project_DefaultEmitsNoBoatProject(t *testing.T) {
	out := helmTemplate(t, "templates/beads-viewer/deployment.yaml",
		"beadsViewer.enabled=true",
		"beadsViewer.oidc.issuerURL=https://auth.example.com/realms/x",
		"beadsViewer.oidc.clientID=viewer",
	)
	if strings.Contains(out, "BOAT_PROJECT") {
		t.Errorf("a default render must not emit BOAT_PROJECT (no project configured), got:\n%s", out)
	}
}

func TestBeadsViewer_Project_GlobalProjectEmitsEnv(t *testing.T) {
	out := helmTemplate(t, "templates/beads-viewer/deployment.yaml",
		"beadsViewer.enabled=true",
		"beadsViewer.oidc.issuerURL=https://auth.example.com/realms/x",
		"beadsViewer.oidc.clientID=viewer",
		"global.project=gardenio",
	)
	if !strings.Contains(out, "BOAT_PROJECT") {
		t.Fatalf("global.project must render BOAT_PROJECT on beads-viewer, got:\n%s", out)
	}
	if !strings.Contains(out, `value: "gardenio"`) {
		t.Errorf("BOAT_PROJECT should carry the global.project value, got:\n%s", out)
	}
}

func TestBeadsViewer_Project_ViewerOverrideWins(t *testing.T) {
	out := helmTemplate(t, "templates/beads-viewer/deployment.yaml",
		"beadsViewer.enabled=true",
		"beadsViewer.oidc.issuerURL=https://auth.example.com/realms/x",
		"beadsViewer.oidc.clientID=viewer",
		"global.project=fleetwide",
		"beadsViewer.project=viewerscoped",
	)
	if !strings.Contains(out, `value: "viewerscoped"`) {
		t.Errorf("beadsViewer.project must win over global.project, got:\n%s", out)
	}
	if strings.Contains(out, `value: "fleetwide"`) {
		t.Errorf("global.project must not leak through when the viewer override is set, got:\n%s", out)
	}
}

// The live k3s deployment must render BOAT_PROJECT=gardenio so /claude-config
// resolves the project-scoped claude-settings beads pods actually receive.
func TestBeadsViewer_Project_DeployValuesK3sSetsGardenio(t *testing.T) {
	out := helmTemplateShowOnlyWithValues(t,
		"templates/beads-viewer/deployment.yaml",
		[]string{"../../../deploy-values/gardenio-k3s.yaml"},
		"beadsViewer.enabled=true",
		"beadsViewer.oidc.issuerURL=https://auth.example.com/realms/x",
		"beadsViewer.oidc.clientID=viewer",
	)
	if !strings.Contains(out, "BOAT_PROJECT") {
		t.Fatalf("deploy-values/gardenio-k3s.yaml must set a viewer project, got:\n%s", out)
	}
	if !strings.Contains(out, `value: "gardenio"`) {
		t.Errorf("the k3s deployment's viewer project must be gardenio, got:\n%s", out)
	}
}
```

- [ ] **Step 2: Run the tests to verify they fail**

Run: `cd gardenio/helm/gardenio/tests && go test ./... -run 'TestBeadsViewer_Project' -v`
Expected: `TestBeadsViewer_Project_DefaultEmitsNoBoatProject` PASSES (it pins today's behaviour); `GlobalProjectEmitsEnv` and `ViewerOverrideWins` FAIL (no `BOAT_PROJECT` in output — `global.project` is undeclared so `--set` reaches a key nothing documents, and the current chart still renders it, so confirm which actually fail before proceeding); `DeployValuesK3sSetsGardenio` FAILS with "must set a viewer project".

> If `GlobalProjectEmitsEnv` already passes, that is expected — `--set` can set an undeclared key. The task's value is the declaration, the deploy-values wiring, and the test coverage. Record in the PR which tests were red.

- [ ] **Step 3: Declare `global.project` in `values.yaml`**

In the `global:` block (starts line 2039), add:

```yaml
  # The project this gardenio deployment serves, e.g. "gardenio". It is the
  # DEFAULT for every component that needs a project identity; a component may
  # override it (see beadsViewer.project).
  #
  # Two consumers, and the second is easy to miss:
  #   1. the `project` label on gardenio_operator_activity_total (ka-0000006752);
  #      empty collapses the label to "unknown".
  #   2. the beads-viewer /claude-config page's RESOLUTION SCOPE. Config beads
  #      carry `project:<name>` labels and MatchesSubscriptions hard-requires a
  #      matching subscription, so with no project the page silently omits every
  #      project-scoped claude-settings bead while pods still receive them.
  # Leave empty only for a deployment that genuinely serves no single project.
  project: ""
```

- [ ] **Step 4: Point the `beadsViewer.project` comment at both consumers**

At `values.yaml:10829-10832`, extend the existing comment so it names the `/claude-config` consumer as well as the metric label, and states that it overrides `global.project`.

- [ ] **Step 5: Correct the template comment**

At `templates/beads-viewer/deployment.yaml:85-90`, the `{{- /* ... */}}` comment currently cites only the metric counter. Add one line: `Also scopes the /claude-config effective-config resolution (spec 2026-09-21-project-scope-hardening-design §2).` Do not change the template logic.

- [ ] **Step 6: Wire this deployment**

In `deploy-values/gardenio-k3s.yaml`, under the existing `beadsViewer:` block, add:

```yaml
  # /claude-config resolves project-scoped claude-settings beads against this
  # value; without it the page silently hides every project-scoped bead.
  project: gardenio
```

- [ ] **Step 7: Run the tests to verify they pass**

Run: `cd gardenio/helm/gardenio/tests && go test ./... -run 'TestBeadsViewer_Project' -v`
Expected: all four PASS.

- [ ] **Step 8: Lint the chart and commit**

```bash
helm lint gardenio/helm/gardenio
git add gardenio/helm/gardenio/values.yaml \
        gardenio/helm/gardenio/templates/beads-viewer/deployment.yaml \
        gardenio/helm/gardenio/tests/beads_viewer_project_test.go \
        deploy-values/gardenio-k3s.yaml
git commit -m "fix(chart): declare global.project and set the viewer project for k3s"
```

- [ ] **Step 9: Push and open the PR**

```bash
git push -u origin agent/project-scope/<task-bead-id>
gh pr create --title "fix(chart): declare global.project, wire beadsViewer.project for k3s" \
  --body "Spec D1, §2. global.project was reached for by two templates but never declared; the live viewer therefore had no BOAT_PROJECT and /claude-config silently hid every project-scoped config bead. Declares the key, documents both consumers, sets beadsViewer.project=gardenio in deploy-values, and pins all four cases with chart tests. No template logic changed; no rollout (Task 4)."
```

---

### Task 2: Viewer — loud degrade when no project scope is configured

Spec: §0 D3, §2 items 3–5, §3 rule 7.

**Files:**
- Modify: `gardenio/controller/internal/claudeconfig/resolve.go` (add `CountProjectScopedSettings` beside `listSettingsRoleLabels`, line 216)
- Modify: `gardenio/controller/cmd/beads-viewer/claude_config.go` (`claudeConfigView`, `claudeConfigDetailView`, `buildClaudeConfigMatrix` line 113, `buildClaudeConfigDetail` line 342)
- Modify: `gardenio/controller/cmd/beads-viewer/templates/claude_config.html` (after the `banner-error` block, line 65)
- Modify: `gardenio/controller/cmd/beads-viewer/templates/claude_config_detail.html` (after the `banner-error` block, line 52)
- Modify: `gardenio/controller/cmd/beads-viewer/main.go` (near `srv.SetViewerProject(cfg.project)`, line 140)
- Test: `gardenio/controller/internal/claudeconfig/effective_test.go`, `gardenio/controller/cmd/beads-viewer/claude_config_test.go`

**Interfaces:**
- Consumes: `claudeconfig.BeadLister` (`resolve.go:18`) and its test fake `mockLister{beads []*beadsapi.BeadDetail; err error}` (`effective_test.go:10-21`); `beadsapi.ListBeadsQuery`; `advice.StripGroupPrefix`; viewer test helpers `newFake() *fakeDaemon` (`bead_show_test.go:2056`), `mkServer(t, beadAPI) http.Handler` (`bead_show_test.go:2098`), `claudeSettingsBead(id string, labels []string, desc string)` (`claude_config_test.go:12`), `edgeAuthMux(srv *Server) http.Handler` (`edge_auth_testhelper_test.go:33`), `NewServer(daemon, logger, basePath, beadsAddr, token, coopmux)`.
- Produces: `claudeconfig.CountProjectScopedSettings(ctx context.Context, lister BeadLister) (int, error)`; view fields `ScopeWarning string` and `UnreflectedCount int` on both view structs.

- [ ] **Step 1: Write the failing counter test**

Append to `gardenio/controller/internal/claudeconfig/effective_test.go`:

```go
func TestCountProjectScopedSettings(t *testing.T) {
	lister := &mockLister{beads: []*beadsapi.BeadDetail{
		{ID: "gd-1", Title: "claude-settings", Type: "config", Status: "open", Labels: []string{"global"}},
		{ID: "gd-2", Title: "claude-settings", Type: "config", Status: "open", Labels: []string{"project:gardenio", "role:crew"}},
		{ID: "gd-3", Title: "claude-settings", Type: "config", Status: "open", Labels: []string{"g1:project:gascity", "role:crew"}},
		{ID: "gd-4", Title: "claude-mcp", Type: "config", Status: "open", Labels: []string{"project:gardenio"}},
	}}
	got, err := CountProjectScopedSettings(context.Background(), lister)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got != 2 {
		t.Errorf("want 2 project-scoped claude-settings beads (group prefix stripped, other categories ignored), got %d", got)
	}
}
```

- [ ] **Step 2: Run it to verify it fails**

Run: `cd gardenio/controller && go test ./internal/claudeconfig/ -run TestCountProjectScopedSettings -v`
Expected: FAIL — `undefined: CountProjectScopedSettings`.

- [ ] **Step 3: Implement the counter**

In `resolve.go`, directly after `listSettingsRoleLabels`:

```go
// CountProjectScopedSettings returns how many OPEN claude-settings config beads
// carry at least one project: label. The beads-viewer uses it to say out loud
// how many beads its own (project-less) resolution is failing to reflect:
// MatchesSubscriptions hard-requires every project: label to be subscribed
// (internal/advice/matching.go:218-220), so a viewer with no project scope shows
// none of them even though pods in that project do receive them.
func CountProjectScopedSettings(ctx context.Context, lister BeadLister) (int, error) {
	result, err := lister.ListBeadsFiltered(ctx, beadsapi.ListBeadsQuery{
		Types:    []string{"config"},
		Statuses: []string{"open"},
		Limit:    500,
	})
	if err != nil {
		return 0, fmt.Errorf("listing config beads: %w", err)
	}
	n := 0
	for _, bead := range result.Beads {
		if bead.Title != "claude-settings" {
			continue
		}
		for _, l := range bead.Labels {
			if strings.HasPrefix(advice.StripGroupPrefix(l), "project:") {
				n++
				break
			}
		}
	}
	return n, nil
}
```

- [ ] **Step 4: Run it to verify it passes**

Run: `cd gardenio/controller && go test ./internal/claudeconfig/ -run TestCountProjectScopedSettings -v`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add gardenio/controller/internal/claudeconfig/resolve.go gardenio/controller/internal/claudeconfig/effective_test.go
git commit -m "feat(claudeconfig): count project-scoped claude-settings beads"
```

- [ ] **Step 6: Write the failing viewer tests (the combination nothing covers today)**

Append to `gardenio/controller/cmd/beads-viewer/claude_config_test.go`:

```go
// The LIVE shape as of 2026-09-21: the deployed viewer has no BOAT_PROJECT, so
// every project-scoped claude-settings bead is dropped by MatchesSubscriptions
// while pods still receive it. The page must SAY so rather than rendering
// defaults as if they were the whole truth.
func TestHandleClaudeConfig_NoViewerProject_WarnsAndStillRenders(t *testing.T) {
	f := newFake()
	f.listResp = &beadsapi.ListBeadsResult{
		Beads: []*beadsapi.BeadDetail{
			claudeSettingsBead("gd-cc-crew", []string{"project:gardenio", "role:crew"}, `{"ultracode":true}`),
		},
		Total: 1,
	}
	mux := mkServer(t, f) // NewServer without SetViewerProject → viewerProject == ""

	req := httptest.NewRequest("GET", "/claude-config", nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	body := w.Body.String()
	if !strings.Contains(body, "No project scope is configured") {
		t.Errorf("expected the scope warning banner, body=%s", body)
	}
	if !strings.Contains(body, "Pods DO receive them") {
		t.Errorf("the banner must say pods are unaffected, body=%s", body)
	}
	// We warn, we do not hide: the role is still discovered and rendered.
	if !strings.Contains(body, ">crew<") {
		t.Errorf("the crew row must still render from defaults, body=%s", body)
	}
}

func TestHandleClaudeConfig_WithViewerProject_NoWarning(t *testing.T) {
	f := newFake()
	f.listResp = &beadsapi.ListBeadsResult{
		Beads: []*beadsapi.BeadDetail{
			claudeSettingsBead("gd-cc-crew", []string{"project:gardenio", "role:crew"}, `{"ultracode":true}`),
		},
		Total: 1,
	}
	srv := NewServer(f, testLogger(), "", "", "", nil)
	srv.SetViewerProject("gardenio")
	handler := edgeAuthMux(srv)

	req := httptest.NewRequest("GET", "/claude-config", nil)
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)

	if strings.Contains(w.Body.String(), "No project scope is configured") {
		t.Error("a viewer with a project scope must not show the warning")
	}
}

func TestHandleClaudeConfigDetail_NoViewerProject_Warns(t *testing.T) {
	f := newFake()
	f.listResp = &beadsapi.ListBeadsResult{
		Beads: []*beadsapi.BeadDetail{
			claudeSettingsBead("gd-cc-crew", []string{"project:gardenio", "role:crew"}, `{"ultracode":true}`),
		},
		Total: 1,
	}
	mux := mkServer(t, f)

	req := httptest.NewRequest("GET", "/claude-config/crew", nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if !strings.Contains(w.Body.String(), "No project scope is configured") {
		t.Errorf("the detail page must carry the same warning, body=%s", w.Body.String())
	}
}
```

- [ ] **Step 7: Run them to verify they fail**

Run: `cd gardenio/controller && go test ./cmd/beads-viewer/ -run 'TestHandleClaudeConfig.*ViewerProject' -v`
Expected: FAIL — the banner copy is absent from every body.

- [ ] **Step 8: Add the view fields and populate them**

In `claude_config.go`, add to `claudeConfigView` and `claudeConfigDetailView`:

```go
	// ScopeWarning is set when this viewer has NO project scope configured
	// (BOAT_PROJECT unset). Resolution then carries no project: subscription, so
	// MatchesSubscriptions (internal/advice/matching.go:218-220) rejects every
	// project-scoped config bead while pods in that project still receive them.
	// UnreflectedCount is how many such beads exist.
	ScopeWarning     string
	UnreflectedCount int
```

Add one helper in `claude_config.go`:

```go
// scopeWarning reports the project-scope blind spot, or "" when a project is
// configured or nothing is being hidden. Best-effort: a daemon error yields no
// warning rather than failing the page.
func (s *Server) scopeWarning(ctx context.Context) (string, int) {
	if s.viewerProject != "" {
		return "", 0
	}
	n, err := claudeconfig.CountProjectScopedSettings(ctx, s.daemon)
	if err != nil {
		s.logger.Warn("claude-config: counting project-scoped settings", "error", err)
		return "", 0
	}
	if n == 0 {
		return "", 0
	}
	noun := "beads"
	if n == 1 {
		noun = "bead"
	}
	return fmt.Sprintf(
		"No project scope is configured for this viewer (BOAT_PROJECT is unset), so %d project-scoped claude-settings %s below. Pods DO receive them. Set beadsViewer.project in the chart.",
		n, noun), n
}
```

Call it in `buildClaudeConfigMatrix` (after `view.RoleCount` is set) and in `buildClaudeConfigDetail` (after `view.LayerCount` is set):

```go
	view.ScopeWarning, view.UnreflectedCount = s.scopeWarning(ctx)
```

- [ ] **Step 9: Render the banner in both templates**

In `templates/claude_config.html`, immediately after the `{{if $view.Error}}…{{else}}` opening (line 65) and before the `cc-band` div:

```html
{{if $view.ScopeWarning}}
<div class="banner banner-warn">⚠ {{$view.ScopeWarning}}</div>
{{end}}
```

In `templates/claude_config_detail.html`, immediately after the `{{else}}` that follows the error banner (line 52):

```html
{{if $view.ScopeWarning}}
<div class="banner banner-warn">⚠ {{$view.ScopeWarning}}</div>
{{end}}
```

> The banner copy must read "…so N project-scoped claude-settings bead(s) below." exactly as the helper formats it; the tests assert two substrings of it.

- [ ] **Step 10: Run the viewer tests to verify they pass**

Run: `cd gardenio/controller && go test ./cmd/beads-viewer/ -run 'TestHandleClaudeConfig' -v`
Expected: PASS, including the pre-existing `TestHandleClaudeConfig_Empty`, `_Matrix`, `_ProjectScoped`, `_RoleBeadOverlay`, `_DaemonError`.

- [ ] **Step 11: Warn at startup**

In `main.go`, immediately after `srv.SetViewerProject(cfg.project)` (line 140):

```go
	if cfg.project == "" {
		logger.Warn("no viewer project configured (BOAT_PROJECT unset) — /claude-config cannot reflect project-scoped claude-settings beads; set beadsViewer.project or global.project in the chart")
	}
```

- [ ] **Step 12: Verify and commit**

```bash
cd gardenio/controller && gofumpt -l . && go vet ./... && staticcheck ./... && go test ./cmd/beads-viewer/... ./internal/claudeconfig/...
cd - && git add gardenio/controller/cmd/beads-viewer/claude_config.go \
        gardenio/controller/cmd/beads-viewer/claude_config_test.go \
        gardenio/controller/cmd/beads-viewer/templates/claude_config.html \
        gardenio/controller/cmd/beads-viewer/templates/claude_config_detail.html \
        gardenio/controller/cmd/beads-viewer/main.go
git commit -m "fix(beads-viewer): warn when no project scope hides config beads"
```

- [ ] **Step 13: Push and open the PR** (branch/title/body as in Task 1; cite spec D3 and note this is the interim truth fix that Task 3 supersedes).

---

### Task 3: Viewer — resolve per (project, role); `viewerProject` becomes the default filter

Spec: §0 D2, §2 item 3, §3 rules 2/7/8, §4 live counts.

**Files:**
- Modify: `gardenio/controller/internal/claudeconfig/effective.go` (add `RoleScope`, `DiscoverRoleScopes` beside `DiscoverRoles`)
- Modify: `gardenio/controller/internal/claudeconfig/resolve.go` (add `listSettingsRoleScopes` beside `listSettingsRoleLabels`, line 216)
- Modify: `gardenio/controller/cmd/beads-viewer/claude_config.go`
- Modify: `gardenio/controller/cmd/beads-viewer/templates/claude_config.html`, `claude_config_detail.html`
- Test: `gardenio/controller/internal/claudeconfig/effective_test.go`, `gardenio/controller/cmd/beads-viewer/claude_config_test.go`

**Interfaces:**
- Consumes: Task 2's `ScopeWarning`/`UnreflectedCount` (this task repurposes them); `claudeconfig.BuildSubscriptions(roles, project, agent string) []string` (`settings.go:240`); `claudeconfig.ResolveEffectiveConfig(ctx, lister, subscriptions, extraLayers...)` (`effective.go:79`).
- Produces: `type RoleScope struct { Project, Role string }`; `claudeconfig.DiscoverRoleScopes(ctx context.Context, lister BeadLister, project string) ([]RoleScope, error)`; `claudeConfigRow.Project string`.

- [ ] **Step 1: Write the failing scope-discovery test**

Append to `effective_test.go`:

```go
func TestDiscoverRoleScopes(t *testing.T) {
	lister := &mockLister{beads: []*beadsapi.BeadDetail{
		{ID: "gd-g", Title: "claude-settings", Type: "config", Status: "open", Labels: []string{"global"}},
		{ID: "gd-a", Title: "claude-settings", Type: "config", Status: "open", Labels: []string{"project:gardenio", "role:crew"}},
		{ID: "gd-b", Title: "claude-settings", Type: "config", Status: "open", Labels: []string{"project:gascity", "role:crew"}},
		{ID: "gd-c", Title: "claude-settings", Type: "config", Status: "open", Labels: []string{"role:reviewer"}},
	}}

	all, err := DiscoverRoleScopes(context.Background(), lister, "")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	want := []RoleScope{{"", "reviewer"}, {"gardenio", "crew"}, {"gascity", "crew"}}
	if !reflect.DeepEqual(all, want) {
		t.Errorf("project-agnostic discovery: want %v, got %v", want, all)
	}

	scoped, err := DiscoverRoleScopes(context.Background(), lister, "gardenio")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	wantScoped := []RoleScope{{"", "reviewer"}, {"gardenio", "crew"}}
	if !reflect.DeepEqual(scoped, wantScoped) {
		t.Errorf("gardenio-scoped discovery: want %v, got %v", wantScoped, scoped)
	}
}
```

- [ ] **Step 2: Run it to verify it fails**

Run: `cd gardenio/controller && go test ./internal/claudeconfig/ -run TestDiscoverRoleScopes -v`
Expected: FAIL — `undefined: DiscoverRoleScopes`, `undefined: RoleScope`.

- [ ] **Step 3: Implement `RoleScope` and `DiscoverRoleScopes`**

In `effective.go`:

```go
// RoleScope is one (project, role) pair the /claude-config matrix renders as a
// row. A Project of "" means the role's only claude-settings beads carry no
// project: label, so it resolves identically in every project.
type RoleScope struct {
	Project string
	Role    string
}

// DiscoverRoleScopes returns every (project, role) pair that has at least one
// open claude-settings bead, sorted by project then role. When project is ""
// every project contributes; otherwise only beads compatible with that project
// do (projectCompatible).
//
// This exists because a single viewer-wide project cannot describe a
// multi-project daemon: the same role name resolves to DIFFERENT effective
// settings in different projects, and resolving every row against one project
// (or against none) misreports all but one of them.
func DiscoverRoleScopes(ctx context.Context, lister BeadLister, project string) ([]RoleScope, error) {
	return listSettingsRoleScopes(ctx, lister, project)
}
```

In `resolve.go`, beside `listSettingsRoleLabels`:

```go
// listSettingsRoleScopes returns the distinct (project, role) pairs carried by
// open claude-settings beads. A bead with role: labels and no project: label
// contributes {"" , role}; a bead with both contributes one pair per project
// label. Deduplicated and sorted by project then role.
func listSettingsRoleScopes(ctx context.Context, lister BeadLister, project string) ([]RoleScope, error) {
	result, err := lister.ListBeadsFiltered(ctx, beadsapi.ListBeadsQuery{
		Types:    []string{"config"},
		Statuses: []string{"open"},
		Limit:    500,
	})
	if err != nil {
		return nil, fmt.Errorf("listing config beads: %w", err)
	}
	seen := make(map[RoleScope]struct{})
	for _, bead := range result.Beads {
		if bead.Title != "claude-settings" {
			continue
		}
		if !projectCompatible(bead.Labels, project) {
			continue
		}
		var roles, projects []string
		for _, l := range bead.Labels {
			clean := advice.StripGroupPrefix(l)
			if r, ok := strings.CutPrefix(clean, "role:"); ok && r != "" {
				roles = append(roles, r)
			}
			if p, ok := strings.CutPrefix(clean, "project:"); ok && p != "" {
				projects = append(projects, p)
			}
		}
		if len(roles) == 0 {
			continue
		}
		if len(projects) == 0 {
			projects = []string{""}
		}
		for _, p := range projects {
			for _, r := range roles {
				seen[RoleScope{Project: p, Role: r}] = struct{}{}
			}
		}
	}
	out := make([]RoleScope, 0, len(seen))
	for rs := range seen {
		out = append(out, rs)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Project != out[j].Project {
			return out[i].Project < out[j].Project
		}
		return out[i].Role < out[j].Role
	})
	return out, nil
}
```

- [ ] **Step 4: Run it to verify it passes**

Run: `cd gardenio/controller && go test ./internal/claudeconfig/ -run TestDiscoverRoleScopes -v`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add gardenio/controller/internal/claudeconfig/effective.go gardenio/controller/internal/claudeconfig/resolve.go gardenio/controller/internal/claudeconfig/effective_test.go
git commit -m "feat(claudeconfig): discover (project, role) scopes for the config matrix"
```

- [ ] **Step 6: Write the failing viewer tests**

Append to `claude_config_test.go`:

```go
// The same role name resolves DIFFERENTLY per project. Both rows must render,
// each resolved against its own project.
func TestHandleClaudeConfig_MultiProjectRows(t *testing.T) {
	f := newFake()
	f.listResp = &beadsapi.ListBeadsResult{
		Beads: []*beadsapi.BeadDetail{
			claudeSettingsBead("gd-cc-gard", []string{"project:gardenio", "role:crew"}, `{"ultracode":true}`),
			claudeSettingsBead("gd-cc-gas", []string{"project:gascity", "role:crew"}, `{"model":"claude-opus-4-8"}`),
		},
		Total: 2,
	}
	mux := mkServer(t, f) // no viewer project → show every project

	req := httptest.NewRequest("GET", "/claude-config", nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	body := w.Body.String()
	if !strings.Contains(body, ">gardenio<") || !strings.Contains(body, ">gascity<") {
		t.Errorf("both project cells must render, body=%s", body)
	}
	if !strings.Contains(body, "claude-opus-4-8") {
		t.Errorf("the gascity crew row must carry its own model value, body=%s", body)
	}
	if !strings.Contains(body, "Showing all projects") {
		t.Errorf("expected the all-projects informational line, body=%s", body)
	}
}

func TestHandleClaudeConfig_ProjectFilter(t *testing.T) {
	f := newFake()
	f.listResp = &beadsapi.ListBeadsResult{
		Beads: []*beadsapi.BeadDetail{
			claudeSettingsBead("gd-cc-gard", []string{"project:gardenio", "role:crew"}, `{"ultracode":true}`),
			claudeSettingsBead("gd-cc-gas", []string{"project:gascity", "role:crew"}, `{"model":"claude-opus-4-8"}`),
		},
		Total: 2,
	}
	mux := mkServer(t, f)

	req := httptest.NewRequest("GET", "/claude-config?project=gardenio", nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	body := w.Body.String()
	if strings.Contains(body, ">gascity<") {
		t.Errorf("?project=gardenio must exclude gascity rows, body=%s", body)
	}
	if !strings.Contains(body, ">gardenio<") {
		t.Errorf("?project=gardenio must keep gardenio rows, body=%s", body)
	}
}

func TestHandleClaudeConfigDetail_ProjectQueryResolvesThatProject(t *testing.T) {
	f := newFake()
	f.listResp = &beadsapi.ListBeadsResult{
		Beads: []*beadsapi.BeadDetail{
			claudeSettingsBead("gd-cc-gas", []string{"project:gascity", "role:crew"}, `{"model":"claude-opus-4-8"}`),
		},
		Total: 1,
	}
	mux := mkServer(t, f)

	req := httptest.NewRequest("GET", "/claude-config/crew?project=gascity", nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	body := w.Body.String()
	if !strings.Contains(body, "project:gascity") {
		t.Errorf("the resolved subscriptions must name the queried project, body=%s", body)
	}
	if !strings.Contains(body, "claude-opus-4-8") {
		t.Errorf("the gascity bead must resolve, body=%s", body)
	}
}
```

Also update `TestHandleClaudeConfig_ProjectScoped` (line 198): with `SetViewerProject("gardenio")` now acting as the *default filter*, assert `ci-gardener` is absent by default and PRESENT for `GET /claude-config?project=monorepo`.

- [ ] **Step 7: Run them to verify they fail**

Run: `cd gardenio/controller && go test ./cmd/beads-viewer/ -run 'TestHandleClaudeConfig_(MultiProjectRows|ProjectFilter|ProjectScoped)|TestHandleClaudeConfigDetail_ProjectQuery' -v`
Expected: FAIL — no project cells, no filter, detail ignores `?project=`.

- [ ] **Step 8: Resolve per (project, role) in the matrix**

In `claude_config.go`:

1. Add `Project string` to `claudeConfigRow`.
2. In `handleClaudeConfig`, read the filter: `project := strings.TrimSpace(r.URL.Query().Get("project"))`; if the query key is absent entirely, default to `s.viewerProject`. Pass it to `buildClaudeConfigMatrix(ctx, query, project)`.
3. In `buildClaudeConfigMatrix`, replace the `DiscoverRoles` call with `claudeconfig.DiscoverRoleScopes(ctx, s.daemon, project)` and, per scope, resolve with `claudeconfig.BuildSubscriptions(scope.Role, scope.Project, "")`. Set `row.Project = scope.Project` and build `DetailHref` as:

```go
	href := s.basePath + "/claude-config/" + url.PathEscape(scope.Role)
	if scope.Project != "" {
		href += "?project=" + url.QueryEscape(scope.Project)
	}
```

4. Replace the Task 2 warning with an informational line when the effective filter is empty: set `view.ScopeWarning = "Showing all projects — no default project is configured for this viewer (beadsViewer.project). Each row is resolved against its own project."` and leave `UnreflectedCount` at 0 (nothing is hidden any more).
5. Keep the role-bead overlay keyed by role name; keep trap-first sort, then project, then role.
6. Extend `claudeConfigRowMatches` so the `?q=` filter also matches the project cell.

In `handleClaudeConfigDetail`, read `?project=` the same way (defaulting to `s.viewerProject`) and pass it to `buildClaudeConfigDetail(ctx, role, project)`, which uses `BuildSubscriptions(role, project, "")`.

- [ ] **Step 9: Render the project column and filter**

In `claude_config.html`: add a `Project` column header before the role column and `<td>{{if .Project}}{{.Project}}{{else}}—{{end}}</td>` in the row loop; add a `project` input to the existing `cc-search` form so the filter round-trips. The informational line renders through the existing `{{if $view.ScopeWarning}}` block from Task 2.

In `claude_config_detail.html`: the `Subscriptions:` line (line 62) already renders `$view.Subscriptions` — no change needed for the assertion; add the project next to the role in the `<h1>` for legibility.

- [ ] **Step 10: Run the viewer tests to verify they pass**

Run: `cd gardenio/controller && go test ./cmd/beads-viewer/ -run TestHandleClaudeConfig -v`
Expected: PASS, all pre-existing cases included.

- [ ] **Step 11: Verify and commit**

```bash
cd gardenio/controller && gofumpt -l . && go vet ./... && staticcheck ./... && go test ./cmd/beads-viewer/... ./internal/claudeconfig/...
cd - && git add gardenio/controller/cmd/beads-viewer/ gardenio/controller/internal/claudeconfig/
git commit -m "fix(beads-viewer): resolve /claude-config per (project, role)"
```

- [ ] **Step 12: Push and open the PR** (cite D2; state that `beadsViewer.project` now only chooses the default filter).

---

### Task 4: Rollout escalation and live verification

Spec: §2 item 5, and the plan's Rollout section. **No code.**

**Files:** none.

**Interfaces:**
- Consumes: Tasks 1–3 merged.
- Produces: one `gb escalation` and a verified `/claude-config/gardener`.

- [ ] **Step 1: Preflight — confirm Tasks 1–3 are merged**

```bash
gh pr list --state merged --search "project-scope" --limit 20
git log origin/main --oneline -20 | grep -i "project"
```
If any of the three is unmerged: `kd comment add <this-task-bead-id> --body @-` explaining which, then STOP. Do not escalate.

- [ ] **Step 2: Read the rollout recipe — do not invent it**

```bash
sed -n '1,80p' scripts/deploy-byo.sh
grep -n "imageTag" deploy-values/gardenio-k3s.yaml
```
Record the documented invocation and the CURRENT `global.imageTag` verbatim. Never guess a calver tag.

- [ ] **Step 3: Read the escalation CLI surface**

```bash
gb escalation create --help
```
Use only flags this prints. Do not copy flags from other plans.

- [ ] **Step 4: File exactly one escalation**

The body must carry, verbatim:

```
Rollout request: /claude-config project scope (plan 2026-09-21-project-scope-hardening, Tasks 1-3 merged)

WHY: the deployed beads-viewer has no BOAT_PROJECT, so /claude-config silently
omits every project-scoped claude-settings bead while pods receive them.
Task 1 added beadsViewer.project: gardenio to deploy-values/gardenio-k3s.yaml.

ROLL (operator):
  <the exact invocation from scripts/deploy-byo.sh, with -f deploy-values/gardenio-k3s.yaml
   and the CURRENT global.imageTag preserved>

VERIFY 1 — the env reaches the pod:
  kubectl -n gardenio get deploy gardenio-beads-viewer \
    -o jsonpath='{.spec.template.spec.containers[0].env[?(@.name=="BOAT_PROJECT")].value}'
  EXPECT: gardenio

VERIFY 2 — the page tells the truth:
  open https://gardenio.clonk.tv/claude-config/gardener
  EXPECT: ultracode = true with provenance chip "role", sourced from bead
          gd-1voySavBGZ; effort shows NO "blocks xhigh" trap marker.

VERIFY 3 — no regression for other projects:
  open https://gardenio.clonk.tv/claude-config
  EXPECT: a Project column; gardenio rows by default; ?project=gascity lists the
          gascity rows.

Paste all three outputs into the escalation result.
```

- [ ] **Step 5: Close only on a verified pass**

When the escalation resolves `success` with all three outputs pasted, comment them on this task bead and close it. On `failure`/`partial`, comment the diagnosis and leave the task open — do not close on an unverified rollout.

---

## Phase 2: close the fail-open and ordering gaps

### Task 5: `kd claim` — warn on no project, fail closed on a fetch error

Spec: §0 D4, §5.

**Files:**
- Modify: `kbeads/cmd/kd/claim.go` (`checkProjectMatch`)
- Test: `kbeads/cmd/kd/claim_project_test.go` (new)

**Interfaces:**
- Consumes: `agentProject()` (`kbeads/cmd/kd/project.go`); the package-level `beadsClient`, which tests swap (`origClient := beadsClient; beadsClient = mc; defer func() { beadsClient = origClient }()` — see `admin_audit_blocks_cycles_test.go:21-23`); the existing `mockBeadsClient` (`mock_client_test.go:16`) with its `Beads map[string]*model.Bead` and `GetErr error` fields (`GetBead` at `mock_client_test.go:370-381` returns `GetErr` first).
- Produces: `checkProjectMatch` returning a non-nil error on a `GetBead` failure. Signature unchanged: `func checkProjectMatch(ctx context.Context, beadID string) error`.

- [ ] **Step 1: Write the failing tests**

Create `kbeads/cmd/kd/claim_project_test.go`:

```go
package main

import (
	"context"
	"errors"
	"strings"
	"testing"

	"gardenio/kbeads/internal/model"
)

// A daemon fault is NOT evidence of "no cross-project conflict". Failing open
// here let an agent claim another project's bead whenever the daemon hiccuped.
func TestCheckProjectMatch_FetchErrorFailsClosed(t *testing.T) {
	t.Setenv("KD_PROJECT", "gardenio")
	t.Setenv("BOAT_PROJECT", "")
	t.Setenv("BEADS_AGENT_NAME", "")

	mc := &mockBeadsClient{GetErr: errors.New("daemon down")}
	origClient := beadsClient
	beadsClient = mc
	defer func() { beadsClient = origClient }()

	err := checkProjectMatch(context.Background(), "gd-other")
	if err == nil {
		t.Fatal("a GetBead failure must fail closed, got nil")
	}
	if !strings.Contains(err.Error(), "daemon down") {
		t.Errorf("the underlying error must be wrapped, got %v", err)
	}
	if !strings.Contains(err.Error(), "--force") {
		t.Errorf("the error must name the override, got %v", err)
	}
}

func TestCheckProjectMatch_NoProjectSkipsWithWarning(t *testing.T) {
	t.Setenv("KD_PROJECT", "")
	t.Setenv("BOAT_PROJECT", "")
	t.Setenv("BEADS_AGENT_NAME", "")

	mc := &mockBeadsClient{}
	origClient := beadsClient
	beadsClient = mc
	defer func() { beadsClient = origClient }()

	if err := checkProjectMatch(context.Background(), "gd-any"); err != nil {
		t.Fatalf("no configured project must skip the check, got %v", err)
	}
	if len(mc.GetBeadCalls) != 0 {
		t.Errorf("the skip must not fetch the bead at all, got %v", mc.GetBeadCalls)
	}
}

func TestCheckProjectMatch_MismatchStillErrors(t *testing.T) {
	t.Setenv("KD_PROJECT", "gardenio")
	t.Setenv("BOAT_PROJECT", "")
	t.Setenv("BEADS_AGENT_NAME", "")

	mc := &mockBeadsClient{Beads: map[string]*model.Bead{
		"gd-gas": {ID: "gd-gas", Labels: []string{"project:gascity"}},
	}}
	origClient := beadsClient
	beadsClient = mc
	defer func() { beadsClient = origClient }()

	err := checkProjectMatch(context.Background(), "gd-gas")
	if err == nil || !strings.Contains(err.Error(), "belongs to project") {
		t.Errorf("a genuine cross-project claim must still be refused, got %v", err)
	}
}

func TestCheckProjectMatch_SameProjectAllowed(t *testing.T) {
	t.Setenv("KD_PROJECT", "gardenio")
	t.Setenv("BOAT_PROJECT", "")
	t.Setenv("BEADS_AGENT_NAME", "")

	mc := &mockBeadsClient{Beads: map[string]*model.Bead{
		"gd-ours": {ID: "gd-ours", Labels: []string{"project:gardenio"}},
	}}
	origClient := beadsClient
	beadsClient = mc
	defer func() { beadsClient = origClient }()

	if err := checkProjectMatch(context.Background(), "gd-ours"); err != nil {
		t.Errorf("a same-project claim must be allowed, got %v", err)
	}
}
```

- [ ] **Step 2: Run them to verify they fail**

Run: `cd kbeads && go test ./cmd/kd/ -run TestCheckProjectMatch -v`
Expected: `FetchErrorFailsClosed` FAILS ("must fail closed, got nil"); the other two PASS (they pin existing behaviour).

- [ ] **Step 3: Make the two branches honest**

In `claim.go`, replace the two fail-open returns:

```go
	agentProject := agentProject()
	if agentProject == "" {
		// A laptop operator legitimately runs kd with no project, so this is a
		// skip rather than an error — but say so, because the guard silently
		// doing nothing is how a cross-project claim slips through unnoticed.
		fmt.Fprintln(os.Stderr, "kd claim: no project configured (KD_PROJECT/BOAT_PROJECT); cross-project check skipped")
		return nil
	}

	bead, err := beadsClient.GetBead(ctx, beadID)
	if err != nil {
		// Fail CLOSED: a daemon fault is not evidence that the bead is in our
		// project. Use --force to claim anyway.
		return fmt.Errorf("cannot verify the project of bead %s (%w) — retry, or use --force to override", beadID, err)
	}
```

- [ ] **Step 4: Run them to verify they pass**

Run: `cd kbeads && go test ./cmd/kd/ -run TestCheckProjectMatch -v`
Expected: all PASS.

- [ ] **Step 5: Verify and commit**

```bash
cd kbeads && gofumpt -l . && go vet ./... && staticcheck ./... && go test ./cmd/kd/...
cd - && git add kbeads/cmd/kd/claim.go kbeads/cmd/kd/claim_project_test.go
git commit -m "fix(kd): fail closed when a claim's project cannot be verified"
```

- [ ] **Step 6: Push and open the PR** (cite D4).

---

### Task 6: Pool pods — re-materialize Claude config after project hydration

Spec: §0 D5, §5 "Pool ordering".

**Files:**
- Modify: `gardenio/controller/cmd/gb/init_agent.go` (`hydrateAssignmentEnv`, `standbyWait`)
- Modify: `gardenio/controller/cmd/gb/init_agent_loop.go` (after the `standbyWait` call, line ~651)
- Test: `gardenio/controller/cmd/gb/init_agent_rematerialize_test.go` (new)

**Interfaces:**
- Consumes: `setupClaudeSettings(coopWorkdir, role string, mockMode bool)` (`init_agent_setup.go:292`); `hydrateAssignmentEnv(ctx context.Context, beadID string)` (`init_agent.go:611`); `standbyWait` (`init_agent.go:544`); the package-level `daemon *beadsapi.Client` and the existing test helper `installFakeDaemon(t *testing.T, handler http.Handler) (*httptest.Server, func())` (`init_agent_jira_probe_test.go:78-94`), which serves the daemon over `httptest` — `GetBead` is `GET /v1/beads/<id>` returning `beadJSON` (`beadsapi/client.go:1458`, `:2306-2336`; `fields` is raw JSON).
- Consumes: `isGenericSpecializationValue(v string) bool` and `genericPoolSentinel = "*"` (`init_agent.go:679-691`).
- Produces: `hydrateAssignmentEnv(ctx context.Context, beadID string) bool` — true when it changed `BOAT_PROJECT` or `BOAT_ROLE`; `standbyWait` propagates it to its caller.

**Why this is not currently broken in production:** no pools are deployed (0 of 575 agent beads carry a pool field), so this is a latent ordering bug on `main`. Say so in the PR.

- [ ] **Step 1: Write the failing ordering test**

Create `init_agent_rematerialize_test.go`:

```go
package main

import (
	"context"
	"net/http"
	"os"
	"testing"
)

// beadFieldsHandler serves one agent bead whose fields carry the given
// project/role, so hydrateAssignmentEnv has something to adopt.
func beadFieldsHandler(project, role string) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"id":"gd-pool-1","title":"pool member","type":"agent",` +
			`"status":"open","labels":[],"fields":{"project":"` + project + `","role":"` + role + `"}}`))
	})
}

// A generic warm-pool pod runs runSetup (init_agent.go:79), which writes
// ~/.claude/settings.json, BEFORE standbyWait -> hydrateAssignmentEnv
// (init_agent_loop.go:651) sets BOAT_PROJECT/BOAT_ROLE from the assignment.
// Nothing re-runs setup, so a pooled pod's Claude config is permanently
// project-less while gb prime (which reads env at call time) sees the
// specialized project. The files and the prompts disagree.
func TestHydrateAssignmentEnv_ReportsSpecialization(t *testing.T) {
	t.Setenv("BOAT_PROJECT", "")
	t.Setenv("BOAT_ROLE", "")
	t.Setenv("ROLE", "")
	t.Setenv("PROJECT", "")

	_, cleanup := installFakeDaemon(t, beadFieldsHandler("gardenio", "crew"))
	defer cleanup()

	if !hydrateAssignmentEnv(context.Background(), "gd-pool-1") {
		t.Fatal("adopting a concrete project+role must report specialization")
	}
	if got := os.Getenv("BOAT_PROJECT"); got != "gardenio" {
		t.Errorf("BOAT_PROJECT = %q, want gardenio", got)
	}
	if got := os.Getenv("BOAT_ROLE"); got != "crew" {
		t.Errorf("BOAT_ROLE = %q, want crew", got)
	}
}

func TestHydrateAssignmentEnv_NoChangeReportsFalse(t *testing.T) {
	t.Setenv("BOAT_PROJECT", "gardenio")
	t.Setenv("BOAT_ROLE", "crew")
	t.Setenv("ROLE", "crew")

	_, cleanup := installFakeDaemon(t, beadFieldsHandler("gardenio", "crew"))
	defer cleanup()

	if hydrateAssignmentEnv(context.Background(), "gd-pool-1") {
		t.Error("an ordinary pod whose env already matches must NOT trigger re-materialization")
	}
}

// The generic sentinel is not a project. Adopting "*" would write
// project:* subscriptions that match nothing.
func TestHydrateAssignmentEnv_SentinelIsNotSpecialization(t *testing.T) {
	t.Setenv("BOAT_PROJECT", "")
	t.Setenv("BOAT_ROLE", "")

	_, cleanup := installFakeDaemon(t, beadFieldsHandler(genericPoolSentinel, genericPoolSentinel))
	defer cleanup()

	if hydrateAssignmentEnv(context.Background(), "gd-pool-1") {
		t.Error("the generic sentinel must not count as specialization")
	}
	if got := os.Getenv("BOAT_PROJECT"); got != "" {
		t.Errorf("BOAT_PROJECT must stay empty, got %q", got)
	}
}

// The role can change at hydration too, so a re-run must use the HYDRATED role.
func TestHydrateAssignmentEnv_RoleChangeAloneSpecializes(t *testing.T) {
	t.Setenv("BOAT_PROJECT", "gardenio")
	t.Setenv("BOAT_ROLE", "generic")
	t.Setenv("ROLE", "generic")

	_, cleanup := installFakeDaemon(t, beadFieldsHandler("gardenio", "reviewer"))
	defer cleanup()

	if !hydrateAssignmentEnv(context.Background(), "gd-pool-1") {
		t.Fatal("a role-only change must still report specialization")
	}
	if got := os.Getenv("BOAT_ROLE"); got != "reviewer" {
		t.Errorf("BOAT_ROLE = %q, want reviewer", got)
	}
}
```

- [ ] **Step 2: Run to verify it fails**

Run: `cd gardenio/controller && go test ./cmd/gb/ -run TestHydrateAssignmentEnv -v`
Expected: FAIL — `hydrateAssignmentEnv(...) used as value` (it currently returns nothing).

- [ ] **Step 3: Report specialization from `hydrateAssignmentEnv`**

Change the signature to `func hydrateAssignmentEnv(ctx context.Context, beadID string) bool` and track the two canonical keys:

```go
	specialized := false
	...
	if proj := bead.Fields["project"]; proj != "" && !isGenericSpecializationValue(proj) {
		if os.Getenv("BOAT_PROJECT") != proj {
			specialized = true
		}
		os.Setenv("BOAT_PROJECT", proj)
		logInit("Hydrated BOAT_PROJECT=%s", proj)
	}
	if role := bead.Fields["role"]; role != "" && !isGenericSpecializationValue(role) {
		if os.Getenv("BOAT_ROLE") != role {
			specialized = true
		}
		os.Setenv("BOAT_ROLE", role)
		os.Setenv("ROLE", role)
		logInit("Hydrated BOAT_ROLE=%s", role)
	}
	...
	return specialized
```

Both early returns (`daemon.GetBead` error) return `false`.

- [ ] **Step 4: Run to verify it passes**

Run: `cd gardenio/controller && go test ./cmd/gb/ -run TestHydrateAssignmentEnv -v`
Expected: PASS.

- [ ] **Step 5: Write the failing re-materialization test**

`runSetup` does not keep the workdir on `cfg` — it computes `cwd := resolveCoopWorkdir(initToK8sConfig(cfg))` locally (`init_agent_setup.go:54-56`), so the re-run must recompute it the same way. Make the decision a testable seam. Append to `init_agent_rematerialize_test.go`:

```go
func TestRematerializeAfterSpecialization(t *testing.T) {
	var gotWorkdir, gotRole string
	var calls int
	orig := setupClaudeSettingsFn
	setupClaudeSettingsFn = func(coopWorkdir, role string, mockMode bool) {
		calls++
		gotWorkdir, gotRole = coopWorkdir, role
	}
	defer func() { setupClaudeSettingsFn = orig }()

	cfg := initConfig{role: "generic", project: "", mockMode: true}

	// Not specialized → no rewrite. An ordinary pod must not pay for this.
	rematerializeAfterSpecialization(cfg, false)
	if calls != 0 {
		t.Fatalf("a non-specialized pod must not re-materialize, got %d calls", calls)
	}

	// Specialized → rewrite with the HYDRATED role, not the stale cfg.role.
	t.Setenv("BOAT_PROJECT", "gardenio")
	t.Setenv("BOAT_ROLE", "reviewer")
	rematerializeAfterSpecialization(cfg, true)
	if calls != 1 {
		t.Fatalf("a specialized pod must re-materialize exactly once, got %d calls", calls)
	}
	if gotRole != "reviewer" {
		t.Errorf("must use the hydrated BOAT_ROLE, got %q (stale cfg.role was %q)", gotRole, cfg.role)
	}
	if gotWorkdir == "" {
		t.Error("the workdir must be recomputed via resolveCoopWorkdir, got empty")
	}
}

func TestRematerializeAfterSpecialization_FallsBackToCfgRole(t *testing.T) {
	var gotRole string
	orig := setupClaudeSettingsFn
	setupClaudeSettingsFn = func(coopWorkdir, role string, mockMode bool) { gotRole = role }
	defer func() { setupClaudeSettingsFn = orig }()

	t.Setenv("BOAT_PROJECT", "gardenio")
	t.Setenv("BOAT_ROLE", "")

	rematerializeAfterSpecialization(initConfig{role: "crew", mockMode: true}, true)
	if gotRole != "crew" {
		t.Errorf("with no hydrated role, fall back to cfg.role, got %q", gotRole)
	}
}
```

Run: `cd gardenio/controller && go test ./cmd/gb/ -run TestRematerialize -v`
Expected: FAIL — `undefined: setupClaudeSettingsFn`, `undefined: rematerializeAfterSpecialization`.

- [ ] **Step 6: Implement the seam and the re-materialization**

In `init_agent_setup.go`, add the indirection and route the existing call through it:

```go
// setupClaudeSettingsFn indirects setupClaudeSettings so the pool
// re-materialization path can be tested without writing a real ~/.claude tree.
var setupClaudeSettingsFn = setupClaudeSettings
```

Change `init_agent_setup.go:56` to `setupClaudeSettingsFn(cwd, cfg.role, cfg.mockMode)`.

In `init_agent.go`, add:

```go
// rematerializeAfterSpecialization rewrites the pod's Claude config when a
// warm-pool assignment gave it a concrete (project, role) AFTER runSetup had
// already written ~/.claude/*. Without this, a pooled pod's settings.json,
// .mcp.json and CLAUDE.md are resolved with NO project subscription for the
// pod's whole life, while gb prime (which reads env at call time) sees the
// specialized project — the files and the prompts disagree.
//
// The role comes from the env, not cfg.role: hydrateAssignmentEnv can change
// BOAT_ROLE as well as BOAT_PROJECT.
func rematerializeAfterSpecialization(cfg initConfig, specialized bool) {
	if !specialized {
		return
	}
	role := os.Getenv("BOAT_ROLE")
	if role == "" {
		role = cfg.role
	}
	project := os.Getenv("BOAT_PROJECT")
	if project == "" {
		project = "none"
	}
	logInit("Re-materializing Claude config after pool specialization (project=%s role=%s)", project, role)
	setupClaudeSettingsFn(resolveCoopWorkdir(initToK8sConfig(cfg)), role, cfg.mockMode)
}
```

- [ ] **Step 7: Propagate specialization out of `standbyWait` and call it**

Change `standbyWait(ctx context.Context, coop *coopapi.Client) error` (`init_agent.go:544`) to return `(bool, error)`, carrying through whatever `hydrateAssignmentEnv` returned. At the call site (`init_agent_loop.go:650-654`):

```go
		specialized, err := standbyWait(ctx, coop)
		if err != nil {
			logInit("standby wait failed: %v", err)
			return
		}
		rematerializeAfterSpecialization(cfg, specialized)
```

Every other `return` inside `standbyWait` returns `false` alongside its error.

- [ ] **Step 8: Run the whole set to verify it passes**

Run: `cd gardenio/controller && go test ./cmd/gb/ -run 'TestHydrateAssignmentEnv|TestRematerialize' -v`
Expected: all PASS.

- [ ] **Step 9: Verify and commit**

```bash
cd gardenio/controller && gofumpt -l . && go vet ./... && staticcheck ./... && go test ./cmd/gb/...
cd - && git add gardenio/controller/cmd/gb/init_agent.go gardenio/controller/cmd/gb/init_agent_loop.go \
        gardenio/controller/cmd/gb/init_agent_setup.go gardenio/controller/cmd/gb/init_agent_rematerialize_test.go
git commit -m "fix(gb): rewrite Claude config after warm-pool project specialization"
```

- [ ] **Step 10: Push and open the PR** (cite D5; state that this is latent today — no pools deployed — and wrong on `main`).

---

### Task 7: Trigger engine — refuse `spawn_agent` with an empty project

Spec: §0 D6.

**Files:**
- Modify: `gardenio/controller/internal/bridge/triggers.go` — `actionSpawnAgent` (`:3426`, whose `SpawnAgent` call is `:3578`) and `actionApplyFormula` (whose driver spawn is `:3892`)
- Test: `gardenio/controller/internal/bridge/trigger_project_guard_test.go` (new)

**Interfaces:**
- Consumes: `TriggerDef.Project` (populated from the trigger bead's `project` field, `triggers.go:4476`); `mockDaemon` (`decisions_mock_test.go:348`), whose `SpawnAgent` appends an agent bead to `m.beads` with `fields["project"]`.
- Produces: no new exported API; a refusal path before both `SpawnAgent` calls. `actionSpawnAgent` and `actionApplyFormula` keep their `(string, error)` signatures.

- [ ] **Step 1: Read both call sites**

```bash
sed -n '3426,3450p;3570,3585p;3870,3900p' gardenio/controller/internal/bridge/triggers.go
```
There are exactly two spawn sites and both are reached from a function returning `(string, error)`, so the guard returns an error rather than logging and continuing. Confirm this before writing the guard — if a third site has appeared, guard it too and say so in the PR.

- [ ] **Step 2: Write the failing test**

Create `gardenio/controller/internal/bridge/trigger_project_guard_test.go`. Build the engine exactly as the package's existing `mockDaemon` tests do (copy the constructor call from whichever test in `decisions_*_test.go` builds a `TriggerEngine` around `mockDaemon`):

```go
package bridge

import (
	"context"
	"strings"
	"testing"
)

// A project-less spawn writes an agent bead the controller's credential guard
// then refuses permanently (spawn_credential_guard.go:70-77), leaving an
// orphan agent bead nobody reaps. Refuse before the write.
func TestActionSpawnAgent_EmptyProjectRefused(t *testing.T) {
	md := newMockDaemon()
	e := newTestTriggerEngine(t, md)

	trig := TriggerDef{
		ID:      "gd-trig-noproj",
		Title:   "project-less trigger",
		Action:  "spawn_agent",
		Project: "",
		Role:    "crew",
		Prompt:  "do the thing",
	}

	_, err := e.actionSpawnAgent(context.Background(), trig, TriggerMatch{Key: "k1"})
	if err == nil {
		t.Fatal("a project-less spawn_agent must be refused, got nil")
	}
	if !strings.Contains(err.Error(), "gd-trig-noproj") || !strings.Contains(err.Error(), "project") {
		t.Errorf("the error must name the trigger and the reason, got %v", err)
	}
	if n := md.countBeadsOfType("agent"); n != 0 {
		t.Errorf("no agent bead may be written, got %d", n)
	}
}

func TestActionSpawnAgent_WithProjectStillSpawns(t *testing.T) {
	md := newMockDaemon()
	e := newTestTriggerEngine(t, md)

	trig := TriggerDef{
		ID:      "gd-trig-ok",
		Title:   "normal trigger",
		Action:  "spawn_agent",
		Project: "gardenio",
		Role:    "crew",
		Prompt:  "do the thing",
	}

	agentID, err := e.actionSpawnAgent(context.Background(), trig, TriggerMatch{Key: "k1"})
	if err != nil {
		t.Fatalf("a project-scoped spawn must still work, got %v", err)
	}
	if agentID == "" {
		t.Error("expected an agent id")
	}
}

func TestActionApplyFormula_EmptyProjectRefused(t *testing.T) {
	md := newMockDaemon()
	e := newTestTriggerEngine(t, md)

	trig := TriggerDef{
		ID:        "gd-trig-formula",
		Title:     "formula trigger",
		Action:    "apply_formula",
		Project:   "",
		FormulaID: "gd-formula-1",
		Prompt:    "pour it",
	}

	_, err := e.actionApplyFormula(context.Background(), trig, TriggerMatch{Key: "k1"})
	if err == nil {
		t.Fatal("a project-less formula pour must be refused before the driver spawn, got nil")
	}
	if !strings.Contains(err.Error(), "project") {
		t.Errorf("the error must name the reason, got %v", err)
	}
}
```

> `newMockDaemon`, `newTestTriggerEngine` and `countBeadsOfType` are shorthand for whatever the package already provides — before running, replace each with the real constructor/helper from `decisions_mock_test.go` and the nearest existing `TriggerEngine` test. Do not add a second mock daemon type. If no bead-counting helper exists, count over `md.beads` inline under `md.mu`.

- [ ] **Step 3: Run to verify it fails**

Run: `cd gardenio/controller && go test ./internal/bridge/ -run 'TestActionSpawnAgent_EmptyProjectRefused|TestActionApplyFormula_EmptyProjectRefused' -v`
Expected: FAIL — no error is returned and an agent bead is written.

- [ ] **Step 4: Add the guard**

At the top of `actionSpawnAgent` (`:3426`, before `ctxWithTriggerOperatorOrDefault`) and at the top of `actionApplyFormula`:

```go
	if strings.TrimSpace(trig.Project) == "" {
		// A project-less spawn writes an agent bead the controller's credential
		// guard then refuses permanently (spawn_credential_guard.go:70-77),
		// leaving an orphan. The daemon rejects such triggers at AUTHORING
		// (runaway_validation.go:275-306); this is the fire-time backstop for
		// triggers that predate that gate or were written around it.
		e.logger.Error("triggers: refusing to spawn — trigger has no project",
			"trigger", trig.ID, "action", trig.Action)
		return "", fmt.Errorf("trigger %s has no project — refusing to spawn a project-less agent", trig.ID)
	}
```

Use the file's own logger field (`e.logger`) and its existing `"triggers: "` log-message prefix.

- [ ] **Step 5: Run to verify it passes**

Run: `cd gardenio/controller && go test ./internal/bridge/ -v`
Expected: PASS — including every pre-existing trigger test. If an existing test fires a spawn trigger with no project, that test was encoding the bug: give its fixture a project and note the change in the PR.

- [ ] **Step 6: Confirm the authoring gate's coverage for the PR**

```bash
sed -n '270,310p' kbeads/internal/server/runaway_validation.go
```
State in the PR whether it covers every create path (HTTP `POST /v1/beads` and `kd create`) or only some.

- [ ] **Step 7: Verify and commit**

```bash
cd gardenio/controller && gofumpt -l . && go vet ./... && staticcheck ./... && go test ./internal/bridge/...
cd - && git add gardenio/controller/internal/bridge/
git commit -m "fix(bridge): refuse spawn_agent on a project-less trigger"
```

- [ ] **Step 8: Push and open the PR** (cite D6).

---

### Task 8: `projectCompatible` — require every project label to match

Spec: §0 D8, §3 rule 2.

**Files:**
- Modify: `gardenio/controller/internal/claudeconfig/resolve.go:243-262`
- Test: `gardenio/controller/internal/claudeconfig/resolve_test.go` (create if absent)

**Interfaces:**
- Consumes: `advice.StripGroupPrefix`.
- Produces: `projectCompatible` with AND semantics (unexported; same signature).

- [ ] **Step 1: Write the failing table test**

```go
func TestProjectCompatible(t *testing.T) {
	cases := []struct {
		name    string
		labels  []string
		project string
		want    bool
	}{
		{"no labels", nil, "gardenio", true},
		{"matching single", []string{"project:gardenio"}, "gardenio", true},
		{"non-matching single", []string{"project:gardenio"}, "gascity", false},
		{"empty project means all", []string{"project:gardenio"}, "", true},
		{"group prefix stripped", []string{"g1:project:gardenio"}, "gardenio", true},
		// MatchesSubscriptions requires EVERY project: label to be subscribed
		// (internal/advice/matching.go:218-220). This mirrored helper was OR,
		// so a two-project bead resolved for a viewer in only one of them.
		{"two projects, only one matches", []string{"project:gardenio", "project:gascity"}, "gardenio", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := projectCompatible(tc.labels, tc.project); got != tc.want {
				t.Errorf("projectCompatible(%v, %q) = %v, want %v", tc.labels, tc.project, got, tc.want)
			}
		})
	}
}
```

- [ ] **Step 2: Run to verify it fails**

Run: `cd gardenio/controller && go test ./internal/claudeconfig/ -run TestProjectCompatible -v`
Expected: FAIL on "two projects, only one matches" (got true, want false).

- [ ] **Step 3: Switch to AND**

```go
// projectCompatible reports whether a bead's project: labels are compatible with
// the target project. A bead with no project: label is always compatible. A
// project of "" means "no project filtering" (every bead is compatible) — that
// is this helper's only divergence from MatchesSubscriptions, and it is
// deliberate: callers use it for project-AGNOSTIC listing.
//
// Otherwise it mirrors MatchesSubscriptions (internal/advice/matching.go:218-220)
// exactly: EVERY project: label must match, not merely one of them.
func projectCompatible(labels []string, project string) bool {
	if project == "" {
		return true
	}
	for _, l := range labels {
		clean := advice.StripGroupPrefix(l)
		if p, ok := strings.CutPrefix(clean, "project:"); ok && p != project {
			return false
		}
	}
	return true
}
```

- [ ] **Step 4: Run to verify it passes**

Run: `cd gardenio/controller && go test ./internal/claudeconfig/... -v`
Expected: PASS, including the existing `DiscoverRoles` tests.

- [ ] **Step 5: Verify and commit**

```bash
cd gardenio/controller && gofumpt -l . && go vet ./... && staticcheck ./... && go test ./internal/claudeconfig/...
cd - && git add gardenio/controller/internal/claudeconfig/resolve.go gardenio/controller/internal/claudeconfig/resolve_test.go
git commit -m "fix(claudeconfig): require every project label to match"
```

- [ ] **Step 6: Push and open the PR** (cite D8).

---

## Phase 3: one resolver and correct docs

### Task 9: `cmd/gb` — one `resolveProject`; warn on empty; drop `BOAT_RIG`

Spec: §0 D7, §1 resolver table, §4 "rig".

**Files:**
- Modify: `gardenio/controller/cmd/gb/project.go` (`defaultGBProject`, line 25)
- Modify: `gardenio/controller/cmd/gb/main.go` (`defaultProject`, line 145)
- Modify: `gardenio/controller/cmd/gb/setup_config.go` (`buildSubscriptions`, line 803)
- Modify: `gardenio/controller/cmd/gb/setup_repos.go` (`runSetupRepos`, line 128)
- Modify: `tests/e2e/scripts/test-config-subscriptions.sh` (the `BOAT_RIG` scenarios at 338-355, 410-423, and the scenario list at the top)
- Test: `gardenio/controller/cmd/gb/project_test.go` (create if absent), `gardenio/controller/cmd/gb/setup_config_test.go`

**Interfaces:**
- Consumes: the `primeProject` package var (the `--project` flag backing `defaultGBProject`).
- Produces: `resolveProject(flag string) string` — `flag` > `KD_PROJECT` > `BOAT_PROJECT`, each `strings.TrimSpace`d.

- [ ] **Step 1: Inventory the callers for the PR**

```bash
grep -rn "defaultProject()\|defaultGBProject()" gardenio/controller/cmd/gb/ | grep -v _test
```
List them in the PR so a reviewer can confirm no precedence change leaks past the three sites this task touches.

- [ ] **Step 2: Write the failing resolver test**

```go
func TestResolveProject(t *testing.T) {
	cases := []struct {
		name, flag, kd, boat, want string
	}{
		{"flag wins", "flagproj", "kdproj", "boatproj", "flagproj"},
		{"kd over boat", "", "kdproj", "boatproj", "kdproj"},
		{"boat alone", "", "", "boatproj", "boatproj"},
		{"all empty", "", "", "", ""},
		{"whitespace trimmed", "", "  kdproj  ", "", "kdproj"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("KD_PROJECT", tc.kd)
			t.Setenv("BOAT_PROJECT", tc.boat)
			if got := resolveProject(tc.flag); got != tc.want {
				t.Errorf("resolveProject(%q) with KD=%q BOAT=%q = %q, want %q", tc.flag, tc.kd, tc.boat, got, tc.want)
			}
		})
	}
}
```

- [ ] **Step 3: Run to verify it fails**

Run: `cd gardenio/controller && go test ./cmd/gb/ -run TestResolveProject -v`
Expected: FAIL — `undefined: resolveProject`.

- [ ] **Step 4: Implement it and route the three resolvers through it**

In `project.go`:

```go
// resolveProject is the ONE project resolver for cmd/gb: an explicit flag, then
// KD_PROJECT, then BOAT_PROJECT. Before this, three call sites disagreed —
// notably buildSubscriptions, which decided WHICH config beads a pod receives,
// ignored KD_PROJECT and honoured the dead BOAT_RIG alias.
func resolveProject(flag string) string {
	if p := strings.TrimSpace(flag); p != "" {
		return p
	}
	if p := strings.TrimSpace(os.Getenv("KD_PROJECT")); p != "" {
		return p
	}
	return strings.TrimSpace(os.Getenv("BOAT_PROJECT"))
}

func defaultGBProject() string { return resolveProject(primeProject) }
```

In `main.go`: `func defaultProject() string { return resolveProject("") }`.

In `setup_repos.go`, replace the inline `BOAT_PROJECT`-then-`KD_PROJECT` block with `project := resolveProject("")` (this corrects the inverted precedence).

- [ ] **Step 5: Write the failing subscription tests**

Add to `setup_config_test.go` (alongside the existing cases at 41-92):

```go
func TestBuildSubscriptions_HonoursKDProject(t *testing.T) {
	t.Setenv("KD_PROJECT", "gardenio")
	t.Setenv("BOAT_PROJECT", "")
	t.Setenv("BOAT_RIG", "")
	got := buildSubscriptions("crew")
	if !slices.Contains(got, "project:gardenio") {
		t.Errorf("KD_PROJECT must contribute a project subscription, got %v", got)
	}
}

func TestBuildSubscriptions_BoatRigIsGone(t *testing.T) {
	t.Setenv("KD_PROJECT", "")
	t.Setenv("BOAT_PROJECT", "")
	t.Setenv("BOAT_RIG", "legacyrig")
	got := buildSubscriptions("crew")
	for _, s := range got {
		if strings.HasPrefix(s, "project:") {
			t.Errorf("BOAT_RIG must no longer contribute a project subscription, got %v", got)
		}
	}
}
```

- [ ] **Step 6: Run to verify they fail**

Run: `cd gardenio/controller && go test ./cmd/gb/ -run TestBuildSubscriptions -v`
Expected: `HonoursKDProject` FAILS (no `project:gardenio`); `BoatRigIsGone` FAILS (`project:legacyrig` present).

- [ ] **Step 7: Rewrite `buildSubscriptions`**

```go
func buildSubscriptions(roles string) []string {
	project := resolveProject("")
	if project == "" {
		fmt.Fprintf(os.Stderr, "[setup] warning: no project in env (KD_PROJECT/BOAT_PROJECT unset); project-scoped config beads will not apply\n")
	}
	return claudeconfig.BuildSubscriptions(roles, project, os.Getenv("BOAT_AGENT"))
}
```

(The `BOAT_RIG` fallback is deleted.)

- [ ] **Step 8: Run to verify they pass**

Run: `cd gardenio/controller && go test ./cmd/gb/ -run 'TestBuildSubscriptions|TestResolveProject' -v`
Expected: PASS.

- [ ] **Step 9: Purge `BOAT_RIG` from the e2e script**

Edit `tests/e2e/scripts/test-config-subscriptions.sh`: delete the `BOAT_RIG` scenarios (338-355, 410-423) or convert them to `KD_PROJECT` precedence checks, and update the scenario list near line 8-12 to match.

```bash
grep -rn "BOAT_RIG" gardenio/ tests/ docs/ || echo "clean"
```
Expected: only `docs/agent-operations.md:27` remains (Task 10 fixes it).

- [ ] **Step 10: Verify and commit**

```bash
cd gardenio/controller && gofumpt -l . && go vet ./... && staticcheck ./... && go test ./cmd/gb/...
cd - && git add gardenio/controller/cmd/gb/project.go gardenio/controller/cmd/gb/main.go \
        gardenio/controller/cmd/gb/setup_config.go gardenio/controller/cmd/gb/setup_repos.go \
        gardenio/controller/cmd/gb/project_test.go gardenio/controller/cmd/gb/setup_config_test.go \
        tests/e2e/scripts/test-config-subscriptions.sh
git commit -m "refactor(gb): one project resolver; drop the dead BOAT_RIG alias"
```

- [ ] **Step 11: Push and open the PR** (cite D7; list the caller inventory from Step 1).

---

### Task 10: Docs — correct four wrong statements, publish the contract

Spec: §3 (the contract), §5 "Docs that are wrong today", §4 "rig". Lands after Task 9.

**Files:**
- Modify: `docs/agent-operations.md` (~line 27)
- Modify: `gardenio/docs/compaction-settings.md` (~159-161)
- Modify: `gardenio/docs/config-inventory.md` (~227-233, plus a new section)
- Modify: `gardenio/docs/onboarding-new-project-and-role.md` (~262-264)
- Modify: `gardenio/docs/research/spawn-escalated-agent/config-bead-surface.md` (67, 183, 401)

**Interfaces:** none (docs only). Line numbers are from 2026-09-21 — anchor on the quoted phrases, not the numbers.

- [ ] **Step 1: Remove the phantom `rig:` subscription**

In `docs/agent-operations.md:27`, delete the `rig:<BOAT_RIG>` subscription claim — `claudeconfig.BuildSubscriptions` (`settings.go:240-252`) never emitted it, and Task 9 removed the last reader of `BOAT_RIG`. State the real set: `[global, role:<r>…, project:<p>, agent:<a>]`.

- [ ] **Step 2: Fix the viewer-equivalence claim**

In `gardenio/docs/compaction-settings.md:159-161`, replace "cannot disagree" with the precise claim: the viewer matches the pod **only when it resolves with the same (project, role) subscriptions**; point at the `/claude-config` project filter added in Task 3.

- [ ] **Step 3: Correct the precedence string and the source pointer**

In `gardenio/docs/config-inventory.md:227-233`: add the missing `2^:role-inline` tier (`resolve.go:58-60`), and move the source pointer from `cmd/gb/config_resolve.go` (now a thin wrapper) to `internal/claudeconfig/resolve.go`. Add one sentence: a `project:` label on a config bead is a HARD requirement — a subscription set without it rejects the bead.

- [ ] **Step 4: Document the empty-project contract for new projects**

In `gardenio/docs/onboarding-new-project-and-role.md` near 262-264, add: a pod with no `BOAT_PROJECT`/`KD_PROJECT` resolves `global` + `role:` layers only, receives no project-scoped config bead, and now emits `[setup] warning: no project in env …` (Task 9).

- [ ] **Step 5: Footnote the two different matchers**

In `gardenio/docs/research/spawn-escalated-agent/config-bead-surface.md` at 67, 183 and 401, add a footnote: `kd`'s superset `labelsMatchConfig` and `claudeconfig`'s `MatchesSubscriptions` are **different matchers** with different semantics; this document describes the former.

- [ ] **Step 6: Publish the contract**

Add a "Project scope contract" section to `gardenio/docs/config-inventory.md` carrying spec §3's eight numbered rules and the worked-example table **verbatim** (copy, do not paraphrase), with a line stating that `MatchesSubscriptions` means `""` = *in no project* while `projectCompatible` means `""` = *all projects*, and that this asymmetry is deliberate and tested.

- [ ] **Step 7: Verify and commit**

```bash
grep -rn "rig:" docs/ gardenio/docs/ | grep -i subscription || echo "clean"
git add docs/agent-operations.md gardenio/docs/compaction-settings.md \
        gardenio/docs/config-inventory.md gardenio/docs/onboarding-new-project-and-role.md \
        gardenio/docs/research/spawn-escalated-agent/config-bead-surface.md
git commit -m "docs: correct the project-scope claims and publish the contract"
```

- [ ] **Step 8: Push and open the PR** (cite §3/§5; note it lands after Task 9).
