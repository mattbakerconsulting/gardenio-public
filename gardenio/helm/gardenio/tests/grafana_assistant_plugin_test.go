package tests

import (
	"slices"
	"strings"
	"testing"
)

// Custom gardenio-assistant-app plugin packaging (C4, ka-0000007568, epic
// ka-0000002011 Phase C — OSS Grafana chat frontend, Path C).
//
// DESCOPED per operator 2026-06-28: no plugin signing, no catalog distribution.
// The plugin ships straight in OUR chart. When grafana.assistant.enabled, a
// copy-only initContainer seeds the CI-built plugin dist into a writable
// emptyDir mounted at /var/lib/grafana/plugins/gardenio-assistant-app, and
// Grafana is told to load that ONE unsigned plugin by id.
//
// These tests pin: (1) the feature is fully OFF by default (byte-additive),
// (2) all four artifacts render when enabled (initContainer, volume,
// volumeMount, env), (3) the unsigned-allow is scoped to EXACTLY our plugin id
// — never the "*" allow-all that would load any unsigned plugin, and (4) the
// runtime path adds no off-cluster egress (the initContainer only `cp`s; no
// npm/network), preserving the security envelope.

const assistantPluginID = "gardenio-assistant-app"

// TestGrafanaAssistant_DisabledByDefault — with grafana on but assistant
// untouched, none of the new artifacts render. Guards the additive contract:
// an env that does not opt in is byte-identical to before this change.
func TestGrafanaAssistant_DisabledByDefault(t *testing.T) {
	t.Parallel()
	dep := helmTemplate(t, "templates/grafana/deployment.yaml",
		"grafana.enabled=true",
	)
	for _, needle := range []string{
		"GF_PLUGINS_ALLOW_LOADING_UNSIGNED_PLUGINS",
		"seed-assistant-plugin",
		"assistant-plugin",
		"initContainers",
		"grafana-assistant-plugin",
	} {
		if strings.Contains(dep, needle) {
			t.Errorf("grafana deployment must not contain %q by default (additive contract, ka-0000007568)", needle)
		}
	}
}

// assistantFlags is the realistic enabled config: the chat plugin hard-depends
// on grafana-llm-app, so grafana.assistant.enabled requires grafana.llm.enabled
// (+ the install) — enforced by a Helm fail guard in deployment.yaml. Every
// deployment-render test uses this set so it exercises the supported combo.
var assistantFlags = []string{
	"grafana.enabled=true",
	"grafana.assistant.enabled=true",
	"grafana.llm.enabled=true",
	"litellm.enabled=true",
	"grafana.plugins.install={grafana-llm-app}",
}

// TestGrafanaAssistant_RendersAllArtifacts — enabling the plugin renders the
// seed initContainer (pointed at the gardenio artifact image), the emptyDir
// volume + its mount at the plugin's own subdir, and the unsigned-allow env.
func TestGrafanaAssistant_RendersAllArtifacts(t *testing.T) {
	t.Parallel()
	dep := helmTemplate(t, "templates/grafana/deployment.yaml", assistantFlags...)

	// (1) initContainer present and pointed at the gardenio-owned artifact image.
	if !strings.Contains(dep, "initContainers") || !strings.Contains(dep, "name: seed-assistant-plugin") {
		t.Errorf("seed-assistant-plugin initContainer must render when grafana.assistant.enabled (ka-0000007568); got:\n%s", dep)
	}
	if !strings.Contains(dep, "/grafana-assistant-plugin:") {
		t.Error("seed initContainer must use the gardenio-owned grafana-assistant-plugin image (ka-0000007568)")
	}

	// (2) copy-only, no-network seed: the initContainer must run EXACTLY the
	// known cp-only script. assertSeedScriptCpOnly anchors on the seed
	// container's ACTUAL exec body and asserts its executable lines equal the
	// cp-only allow-list — far stronger than the old whole-Deployment token
	// denylist (which skipped every "#" line and so could miss an egressing
	// command outside a comment). Any added line — an `npm`/`curl`/`wget` or any
	// other runtime fetch/build — fails the equality, so the runtime seed
	// provably adds zero off-cluster egress (the build happened in CI),
	// preserving the security envelope (ka-0000007568, ka-0000006289).
	assertSeedScriptCpOnly(t, dep)

	// (3) volume + mount at the plugin's own subdir under the plugins path.
	if !strings.Contains(dep, "mountPath: /var/lib/grafana/plugins/"+assistantPluginID) {
		t.Errorf("plugin dist must mount at /var/lib/grafana/plugins/%s (ka-0000007568); got:\n%s", assistantPluginID, dep)
	}
	if !strings.Contains(dep, "name: assistant-plugin") {
		t.Error("an assistant-plugin volume must be declared (ka-0000007568)")
	}

	// (4) unsigned-allow env scoped to EXACTLY our id.
	if !strings.Contains(dep, "GF_PLUGINS_ALLOW_LOADING_UNSIGNED_PLUGINS") {
		t.Error("GF_PLUGINS_ALLOW_LOADING_UNSIGNED_PLUGINS must render when grafana.assistant.enabled (ka-0000007568)")
	}

	// (5) the plugins-provisioning configMap must be MOUNTED even when only the
	// assistant is on (not just when grafana.llm.enabled) — otherwise the
	// enablement provisioning below never reaches Grafana.
	if !strings.Contains(dep, "/etc/grafana/provisioning/plugins") {
		t.Error("provisioning-plugins must mount when grafana.assistant.enabled, so the app gets enabled (ka-0000007568)")
	}
}

// TestGrafanaAssistant_AppIsEnabledNotJustDiscovered — the load-bearing check:
// a Grafana *app* plugin stays DISABLED until provisioned-enabled, even once it
// is discovered on disk and unsigned-allowed. The provisioning configMap must
// carry an apps: entry for our id with disabled: false, or the chat nav page
// never appears. Guards the C4 deliverable end-to-end.
func TestGrafanaAssistant_AppIsEnabledNotJustDiscovered(t *testing.T) {
	t.Parallel()
	// Realistic enabled config (assistant requires llm). The apps: block must
	// carry an entry that ENABLES our plugin, not merely rely on disk discovery.
	cm := helmTemplate(t, "templates/grafana/configmap.yaml", assistantFlags...)
	if !strings.Contains(cm, "plugins.yaml: |") {
		t.Fatalf("provisioning configmap must render plugins.yaml when grafana.assistant.enabled (ka-0000007568); got:\n%s", cm)
	}
	// Bind the assertion to the gardenio-assistant-app apps: entry SPECIFICALLY,
	// not the configmap at large. The grafana-llm-app foundation entry ALSO
	// carries `disabled: false`, so a bare strings.Contains(cm, "disabled:
	// false") false-passes even if OUR app were discovered-but-not-enabled
	// (ka-0000006289 finding 1). Extract our entry's block and require
	// disabled:false WITHIN it.
	entry := assistantAppEntry(t, cm)
	if !strings.Contains(entry, "disabled: false") {
		t.Errorf("the %s app entry must be provisioned with disabled: false (app plugins are discovered-but-disabled by default; enabling is what surfaces the chat nav page) (ka-0000007568, ka-0000006289); got entry:\n%s\nfull configmap:\n%s", assistantPluginID, entry, cm)
	}
	if strings.Contains(entry, "disabled: true") {
		t.Errorf("the %s app entry must NOT be disabled: true (ka-0000006289); got entry:\n%s", assistantPluginID, entry)
	}
}

// TestGrafanaAssistant_ConfigmapCleanByDefault — with grafana on but neither
// llm nor assistant enabled, the provisioning configMap renders no plugins.yaml
// (additive contract; the apps: block is gated on llm OR assistant).
func TestGrafanaAssistant_ConfigmapCleanByDefault(t *testing.T) {
	t.Parallel()
	cm := helmTemplate(t, "templates/grafana/configmap.yaml",
		"grafana.enabled=true",
	)
	if strings.Contains(cm, "plugins.yaml") || strings.Contains(cm, assistantPluginID) {
		t.Errorf("provisioning configmap must not provision any plugin app by default (ka-0000007568); got:\n%s", cm)
	}
}

// TestGrafanaAssistant_UnsignedAllowScopedToExactID — the unsigned-allow must
// name EXACTLY gardenio-assistant-app and must never be the "*" allow-all
// (which would let any unsigned plugin load — a security regression).
func TestGrafanaAssistant_UnsignedAllowScopedToExactID(t *testing.T) {
	t.Parallel()
	dep := helmTemplate(t, "templates/grafana/deployment.yaml", assistantFlags...)
	if !strings.Contains(dep, `value: "`+assistantPluginID+`"`) {
		t.Errorf("unsigned-allow must be exactly %q; got:\n%s", assistantPluginID, dep)
	}
	// The env value must not be the wildcard. Scan the env value line directly.
	for line := range strings.SplitSeq(dep, "\n") {
		if strings.Contains(line, "value:") && strings.Contains(line, "*") &&
			strings.Contains(strings.ToLower(dep), "unsigned") {
			// Only fail if the wildcard is the unsigned-allow value specifically.
			if strings.TrimSpace(line) == `value: "*"` {
				t.Error("unsigned-allow must NOT be the wildcard '*' (would load any unsigned plugin)")
			}
		}
	}
}

// TestGrafanaAssistant_RootfsStaysReadOnly — enabling the plugin must not relax
// the main container's read-only root filesystem; the plugin lives on a mount,
// not the rootfs. The seed initContainer likewise runs non-root with a RO root.
func TestGrafanaAssistant_RootfsStaysReadOnly(t *testing.T) {
	t.Parallel()
	dep := helmTemplate(t, "templates/grafana/deployment.yaml",
		append(assistantFlags, "grafana.persistence.enabled=true")...)
	if !strings.Contains(dep, "readOnlyRootFilesystem: true") {
		t.Error("readOnlyRootFilesystem must stay true — the plugin lives on a mount (ka-0000007568)")
	}
	// The seed initContainer must not run as root (matches the litellm seed-app
	// pattern: runAsUser 65534 so the cp inherits a non-root uid).
	if !strings.Contains(dep, "runAsUser: 65534") {
		t.Error("seed initContainer must run as non-root uid 65534 (ka-0000007568)")
	}
}

// TestGrafanaAssistant_CoexistsWithLLMAndPersistence — the plugin packaging
// must compose with the grafana-llm-app install and a data PVC: the assistant
// mount is nested UNDER /var/lib/grafana (so the PVC at /var/lib/grafana does
// not shadow it) and must not collide with the sibling grafana-llm-app dir.
func TestGrafanaAssistant_CoexistsWithLLMAndPersistence(t *testing.T) {
	t.Parallel()
	dep := helmTemplate(t, "templates/grafana/deployment.yaml",
		"grafana.enabled=true",
		"grafana.assistant.enabled=true",
		"grafana.llm.enabled=true",
		"litellm.enabled=true",
		"grafana.plugins.install={grafana-llm-app}",
		"grafana.persistence.enabled=true",
	)
	// Both the LLM install env and our plugin mount must be present together.
	if !strings.Contains(dep, "GF_INSTALL_PLUGINS") {
		t.Error("grafana-llm-app install must still render alongside the assistant plugin")
	}
	if !strings.Contains(dep, "mountPath: /var/lib/grafana/plugins/"+assistantPluginID) {
		t.Error("assistant plugin must mount at its nested subdir even with persistence + llm on (ka-0000007568)")
	}
	// The data PVC mounts the parent /var/lib/grafana; the assistant mount is a
	// distinct, deeper path so it overlays only its own plugin subdir.
	if !strings.Contains(dep, "mountPath: /var/lib/grafana\n") {
		t.Error("data volume must still mount the /var/lib/grafana parent (persistence on)")
	}
}

// TestGrafanaAssistant_RequiresLLM — the chat plugin hard-depends on
// grafana-llm-app, so enabling the assistant without grafana.llm.enabled must
// FAIL the render loudly rather than ship a plugin Grafana silently refuses to
// load (its declared dependency would be absent). Guards the latent
// foot-gun a future overlay could hit (review finding, ka-0000007568).
func TestGrafanaAssistant_RequiresLLM(t *testing.T) {
	t.Parallel()
	out, err := helmTemplateAllowFail(t, "templates/grafana/deployment.yaml",
		"grafana.enabled=true",
		"grafana.assistant.enabled=true",
		// grafana.llm.enabled deliberately left off.
	)
	if err == nil {
		t.Fatalf("helm template must FAIL when grafana.assistant.enabled without grafana.llm.enabled; got success:\n%s", out)
	}
	if !strings.Contains(out, "requires grafana.llm.enabled") {
		t.Errorf("failure must explain the grafana-llm-app dependency; got:\n%s", out)
	}
	// One guard, both attack surfaces: helm template renders ALL templates and
	// THEN filters by --show-only, so the deployment.yaml fail above fires
	// regardless of which template the consumer asked for — `--show-only
	// configmap.yaml` also surfaces this exact error. Pin that consumer
	// guarantee explicitly so a refactor that moves the guard somewhere lazily-
	// evaluated (e.g. inside a configmap conditional that never fires for
	// assistant-only=true) gets caught (ka-0000006289 finding 3a).
	cmOut, cmErr := helmTemplateAllowFail(t, "templates/grafana/configmap.yaml",
		"grafana.enabled=true",
		"grafana.assistant.enabled=true",
	)
	if cmErr == nil {
		t.Fatalf("--show-only configmap.yaml must ALSO fail on assistant-without-llm (the deployment.yaml guard fires before --show-only filtering, ka-0000006289); got success:\n%s", cmOut)
	}
	if !strings.Contains(cmOut, "requires grafana.llm.enabled") {
		t.Errorf("configmap-path failure must surface the same dependency message (ka-0000006289); got:\n%s", cmOut)
	}
}

// TestGrafanaAssistant_LLMOnlyRendersNoAssistantArtifacts — inverse-additive
// guard (ka-0000006289 finding 3b): with grafana.llm.enabled BUT
// grafana.assistant.enabled=false (explicitly), neither the deployment nor the
// configmap may carry any assistant artifact. Together with
// TestGrafanaAssistant_DisabledByDefault (assistant block unset) this pins the
// additive contract along BOTH axes: leaving assistant out and explicitly
// setting it false must both render byte-identically with respect to assistant
// artifacts. The C1/C7 grafana-llm-app foundation must still render normally.
func TestGrafanaAssistant_LLMOnlyRendersNoAssistantArtifacts(t *testing.T) {
	t.Parallel()
	llmOnlyFlags := []string{
		"grafana.enabled=true",
		"grafana.llm.enabled=true",
		"litellm.enabled=true",
		"grafana.assistant.enabled=false",
		"grafana.plugins.install={grafana-llm-app}",
	}
	// Assistant artifacts must be absent from both templates.
	for _, target := range []string{
		"templates/grafana/deployment.yaml",
		"templates/grafana/configmap.yaml",
	} {
		out := helmTemplate(t, target, llmOnlyFlags...)
		for _, needle := range []string{
			"GF_PLUGINS_ALLOW_LOADING_UNSIGNED_PLUGINS",
			"seed-assistant-plugin",
			assistantPluginID,
			"enableDashboardWrites",
		} {
			if strings.Contains(out, needle) {
				t.Errorf("%s must not contain %q when grafana.assistant.enabled=false (inverse additive, ka-0000006289); got:\n%s", target, needle, out)
			}
		}
	}
	// The C1/C7 foundation must still render — this is what makes the inverse
	// case "LLM-only", not "everything off". The grafana-llm-app provisioning
	// entry and its mcp.toolsets allow-list must be intact.
	cm := helmTemplate(t, "templates/grafana/configmap.yaml", llmOnlyFlags...)
	if !strings.Contains(cm, "- type: grafana-llm-app") {
		t.Errorf("grafana-llm-app foundation entry must still render in the LLM-only case (ka-0000006289); got:\n%s", cm)
	}
	if !strings.Contains(cm, "toolsets:") {
		t.Errorf("C7 mcp.toolsets allow-list must still render in the LLM-only case (ka-0000006289); got:\n%s", cm)
	}
}

// TestGrafanaAssistant_C7ToolsetsSurviveAlongsideAssistant — rebase-regression
// guard (ka-0000006289 finding 3c). The C4 merge had to thread its new
// gardenio-assistant-app apps: entry into the SAME `apps:` block that already
// carried the C7 (ka-0000001262) grafana-llm-app entry with its curated
// jsonData.mcp.toolsets allow-list. A future rebase or refactor that drops or
// truncates the toolsets while editing the assistant entry would silently
// re-open the chat box's write surface (a nil family defaults to ENABLED
// upstream), bypassing the C7 RBAC narrative. Pin BOTH entries side-by-side and
// pin the FULL family allow-list to fail loudly if either regresses.
func TestGrafanaAssistant_C7ToolsetsSurviveAlongsideAssistant(t *testing.T) {
	t.Parallel()
	cm := helmTemplate(t, "templates/grafana/configmap.yaml", assistantFlags...)
	// Both apps: entries must render side-by-side.
	if !strings.Contains(cm, "- type: grafana-llm-app") {
		t.Errorf("grafana-llm-app entry must survive alongside the assistant entry (rebase-regression guard, ka-0000006289); got:\n%s", cm)
	}
	if !strings.Contains(cm, "- type: "+assistantPluginID) {
		t.Errorf("assistant entry must render when grafana.assistant.enabled (ka-0000006289); got:\n%s", cm)
	}
	// The C7 mcp.toolsets allow-list must be intact inside the grafana-llm-app
	// entry — bound to that entry, not the configmap at large, so an
	// accidentally-relocated toolsets block does not let the test false-pass.
	llmEntry := llmAppEntry(t, cm)
	if !strings.Contains(llmEntry, "mcp:") || !strings.Contains(llmEntry, "toolsets:") {
		t.Fatalf("grafana-llm-app entry must carry jsonData.mcp.toolsets (C7 ka-0000001262 survives C4, ka-0000006289); got entry:\n%s", llmEntry)
	}
	// Pin every read/dashboard-create family to true and every write/admin
	// family to false — the full C7 allow-list. A nil family defaults to ENABLED
	// upstream, so EXPLICIT false on the denied set is load-bearing.
	enabled := []string{
		"search: true", "datasource: true", "prometheus: true",
		"navigation: true", "rendering: true", "dashboard: true", "folder: true",
	}
	disabled := []string{
		"admin: false", "alerting: false", "annotations: false", "oncall: false",
		"incident: false", "asserts: false", "sift: false", "pyroscope: false",
		"loki: false", "clickhouse: false", "cloudwatch: false",
		"elasticsearch: false", "examples: false", "searchlogs: false",
	}
	for _, kv := range enabled {
		if !strings.Contains(llmEntry, kv) {
			t.Errorf("C7 toolset %q must still be enabled alongside the assistant (ka-0000001262, ka-0000006289); got entry:\n%s", kv, llmEntry)
		}
	}
	for _, kv := range disabled {
		if !strings.Contains(llmEntry, kv) {
			t.Errorf("C7 toolset %q must still be EXPLICITLY disabled alongside the assistant — a nil family would re-open the surface (ka-0000001262, ka-0000006289); got entry:\n%s", kv, llmEntry)
		}
	}
	// Defense-in-depth: NO write/admin family may be true.
	for _, kv := range []string{
		"admin: true", "alerting: true", "annotations: true", "oncall: true",
	} {
		if strings.Contains(llmEntry, kv) {
			t.Errorf("write/admin family %q must NEVER be enabled alongside the assistant (ka-0000006289); got entry:\n%s", kv, llmEntry)
		}
	}
}

// --- helpers (ka-0000006289) ---

// assertSeedScriptCpOnly extracts the seed-assistant-plugin initContainer's
// exec body from the rendered deployment YAML and asserts its executable lines
// (post-strip of leading whitespace and YAML "#" comments) equal the known
// cp-only allow-list — finding 2. The old check scanned the entire Deployment
// for banned tokens AFTER skipping every "#" line, which conflated YAML
// comments with shell comments and could miss an off-cluster fetch placed
// outside a YAML comment but inside the script. Anchoring on the actual
// initContainer block + asserting equality means any added line (npm/curl/wget
// or anything else) flips the equality and fails the test, preserving the
// zero-off-cluster-egress security envelope by construction.
func assertSeedScriptCpOnly(t *testing.T, dep string) {
	t.Helper()
	// 1. Locate the seed-assistant-plugin initContainer block. SplitAfter at
	//    `- name:` boundaries to scope strictly to that one container — the
	//    grafana-llm-app prebake container has its own `cp` script and we must
	//    not let it satisfy this assertion.
	const startMarker = "- name: seed-assistant-plugin"
	const cmdMarker = "command:"
	const tail = "volumeMounts:"
	startIdx := strings.Index(dep, startMarker)
	if startIdx < 0 {
		t.Fatalf("seed-assistant-plugin initContainer not found in deployment (ka-0000006289); got:\n%s", dep)
	}
	cmdIdx := strings.Index(dep[startIdx:], cmdMarker)
	if cmdIdx < 0 {
		t.Fatalf("seed-assistant-plugin initContainer missing command: block (ka-0000006289); got:\n%s", dep[startIdx:])
	}
	cmdStart := startIdx + cmdIdx
	tailIdx := strings.Index(dep[cmdStart:], tail)
	if tailIdx < 0 {
		t.Fatalf("seed-assistant-plugin command block not terminated by volumeMounts (ka-0000006289); got:\n%s", dep[cmdStart:])
	}
	block := dep[cmdStart : cmdStart+tailIdx]
	// 2. The command MUST be the literal "sh -c <script>" shape — anything else
	//    (a bespoke entrypoint, a different shell, an exec form with args)
	//    would bypass the script-line check below.
	if !strings.Contains(block, "- sh") || !strings.Contains(block, "- -c") {
		t.Errorf("seed initContainer command must be `sh -c <script>`; got:\n%s", block)
	}
	// 3. Extract the post-`-c |` script body (everything after the "- |" pipe
	//    literal) and walk it line by line. The cp-only allow-list pins the
	//    semantic intent: a guard (set -eu), a plugin.json sanity check, and a
	//    cp into the shared emptyDir. Comments are documentation only.
	pipeIdx := strings.Index(block, "- |")
	if pipeIdx < 0 {
		t.Fatalf("seed initContainer script must use the `- |` literal-block form (ka-0000006289); got:\n%s", block)
	}
	script := block[pipeIdx+len("- |"):]
	allowed := []string{
		"set -eu",
		"test -f /plugin/plugin.json",
		// The plugin now ships a BACKEND binary (ka-0000000293): assert the
		// arch-suffixed gpx_ binary exists before copying so a missing Go build
		// fails the init rather than silently seeding a frontend-only dist (which
		// would dark out the chat box's in-cluster MCP proxy). Still a pure test
		// — no network, envelope intact.
		"test -f /plugin/gpx_gardenio_assistant_app_linux_amd64",
		"cp -r /plugin/. /mnt/plugin/",
	}
	got := []string{}
	for line := range strings.SplitSeq(script, "\n") {
		code := strings.TrimSpace(line)
		// Drop blank lines and YAML/shell comments — both legitimately appear
		// in the rendered script (and the chart-author rationale references npm
		// + egress, which the OLD denylist had to special-case around).
		if code == "" || strings.HasPrefix(code, "#") {
			continue
		}
		got = append(got, code)
	}
	if !slices.Equal(got, allowed) {
		t.Errorf("seed initContainer executable lines must equal the cp-only allow-list (security envelope, ka-0000007568, ka-0000006289).\nwant: %q\ngot:  %q\nscript:\n%s", allowed, got, script)
	}
}

// assistantAppEntry extracts the `- type: gardenio-assistant-app` apps: block
// from the rendered configmap so assertions can bind to OUR entry — not the
// sibling grafana-llm-app entry, which also carries `disabled: false` and would
// otherwise let a substring check false-pass (ka-0000006289 finding 1). Scopes
// from the entry's `- type:` line up to the next `- type:` boundary (the next
// apps[] element) or the end of the YAML — whichever comes first.
func assistantAppEntry(t *testing.T, cm string) string {
	t.Helper()
	return appEntry(t, cm, assistantPluginID)
}

// llmAppEntry is the symmetric helper for the grafana-llm-app entry, used to
// scope the C7 toolsets allow-list assertion (ka-0000006289 finding 3c).
func llmAppEntry(t *testing.T, cm string) string {
	t.Helper()
	return appEntry(t, cm, "grafana-llm-app")
}

// appEntry returns the apps[] entry whose `- type:` value matches pluginID,
// scoped from its `- type:` line up to (but excluding) the next `- type:`
// element or end-of-YAML. Fails the test if the entry is missing.
func appEntry(t *testing.T, cm, pluginID string) string {
	t.Helper()
	marker := "- type: " + pluginID
	start := strings.Index(cm, marker)
	if start < 0 {
		t.Fatalf("apps[] entry %q not found in configmap (ka-0000006289); got:\n%s", pluginID, cm)
	}
	// Scope from this entry's `- type:` line up to the next `- type:` element
	// at the same indentation. The rendered YAML uses 6-space indent at the
	// apps[] list, so "\n      - type:" uniquely matches the next element (a
	// deeper-nested type: key — none exist in this configmap, but be precise to
	// avoid surprises). If there is no next element, the entry runs to EOF.
	const nextBoundary = "\n      - type: "
	rest := cm[start+len(marker):]
	before, _, found := strings.Cut(rest, nextBoundary)
	if !found {
		return cm[start:]
	}
	return cm[start:start+len(marker)] + before
}
