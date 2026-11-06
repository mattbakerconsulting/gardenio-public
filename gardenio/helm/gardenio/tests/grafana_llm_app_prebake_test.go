package tests

import (
	"strings"
	"testing"
)

// Pre-baked grafana-llm-app plugin: zero-off-cluster-egress as a NETWORK
// guarantee, not a config-only promise (C6/prod prereq ka-0000007541, epic
// ka-0000002011 — Path C chat box).
//
// grafana-llm-app proxies ALL chat LLM completions + the embedded mcp-grafana
// and runs INSIDE the Grafana pod. Its DEFAULT delivery — the boot-time catalog
// install (GF_INSTALL_PLUGINS) — fetches from grafana.com at :443 and therefore
// REQUIRES grafana.plugins.allowCatalogEgress:true, which renders a 0.0.0.0/0
// :443+:80 allow on the grafana-egress NetworkPolicy. With that hole open
// NOTHING at the network layer stops the in-pod plugin from POSTing completions
// off-cluster; in-cluster-only rests solely on the provisioned openaiURL config.
//
// grafana.llm.prebake.enabled flips that to a network guarantee: a copy-only
// initContainer seeds a PINNED, CHECKSUMMED grafana-llm-app dist (built in CI,
// pinned in gardenio/images/grafana-llm-app-plugin/pin.env) so the env can run
// with allowCatalogEgress:FALSE under default-deny-egress. EXACTLY mirrors the
// C4 grafana-assistant-plugin seam.
//
// These tests pin: (1) the feature is fully OFF by default (byte-additive),
// (2) the seed initContainer + volume + mount render when enabled and add ZERO
// off-cluster egress (a pure cp; no npm/curl/wget/http), (3) the load-bearing
// property — with prebake on and allowCatalogEgress OFF under default-deny, the
// grafana-egress policy renders NO 0.0.0.0/0 / :443 allow while still allowing
// in-cluster litellm, (4) grafana-llm-app is signed so NO unsigned-allow is
// added for it, and (5) the conflict + require guards fail the render loudly.

// llmPrebakePluginID is the upstream plugin id == on-disk plugins subdir name.
const llmPrebakePluginID = "grafana-llm-app"

// prebakeFlags is the realistic enabled config — the full C6 PROD posture.
// prebake is the delivery mechanism for the LLM chat box, so prebake.enabled
// requires grafana.llm.enabled (+ litellm for the in-cluster transport).
// Critically it does NOT set grafana.plugins.install — prebake REPLACES the
// catalog install, and setting both trips the conflict guard. It also satisfies
// the COUPLING render-gate (ka-0000007541 QC): prebake.enabled refuses to render
// unless allowCatalogEgress is false (default) AND default-deny-egress is in
// force, so every prebake render here exercises the real zero-egress posture the
// network guarantee depends on. The dedicated egress-leak tests below assert that
// gate FIRES when those network preconditions are violated.
var prebakeFlags = []string{
	"grafana.enabled=true",
	"grafana.llm.enabled=true",
	"litellm.enabled=true",
	"grafana.llm.prebake.enabled=true",
	"networkPolicies.enabled=true",
	"networkPolicies.defaultDenyEgress=true",
	// grafana.plugins.allowCatalogEgress deliberately left at its default (false).
}

// TestGrafanaLLMPrebake_DisabledByDefault — with grafana + llm on but prebake
// untouched, none of the prebake artifacts render. Guards the additive contract:
// an env that does not opt into prebake is byte-identical to the catalog-install
// path (which other tests already cover).
func TestGrafanaLLMPrebake_DisabledByDefault(t *testing.T) {
	t.Parallel()
	// grafana on only — nothing prebake-related.
	bare := helmTemplate(t, "templates/grafana/deployment.yaml",
		"grafana.enabled=true",
	)
	for _, needle := range []string{"seed-llm-plugin", "name: llm-plugin", "grafana-llm-app-plugin"} {
		if strings.Contains(bare, needle) {
			t.Errorf("grafana deployment must not contain %q by default (additive contract, ka-0000007541)", needle)
		}
	}
	// Even with the LLM chat box on via the CATALOG path (install + llm), prebake
	// must stay off unless explicitly opted in — no seed initContainer.
	catalog := helmTemplate(t, "templates/grafana/deployment.yaml",
		"grafana.enabled=true",
		"grafana.llm.enabled=true",
		"litellm.enabled=true",
		"grafana.plugins.install={grafana-llm-app 1.0.8}",
	)
	if strings.Contains(catalog, "seed-llm-plugin") || strings.Contains(catalog, "name: llm-plugin") {
		t.Error("the catalog-install path must NOT render the prebake seed initContainer (ka-0000007541)")
	}
	// The catalog path is still expected to use GF_INSTALL_PLUGINS (no regression).
	if !strings.Contains(catalog, "GF_INSTALL_PLUGINS") {
		t.Error("catalog-install path must still render GF_INSTALL_PLUGINS (no regression, ka-0000004453)")
	}
}

// TestGrafanaLLMPrebake_RendersSeedArtifacts — enabling prebake renders the seed
// initContainer (pointed at the gardenio-owned pinned artifact image), the
// emptyDir volume + its mount at the plugin's own subdir.
func TestGrafanaLLMPrebake_RendersSeedArtifacts(t *testing.T) {
	t.Parallel()
	dep := helmTemplate(t, "templates/grafana/deployment.yaml", prebakeFlags...)

	// (1) initContainer present and pointed at the gardenio-owned artifact image.
	if !strings.Contains(dep, "initContainers") || !strings.Contains(dep, "name: seed-llm-plugin") {
		t.Errorf("seed-llm-plugin initContainer must render when grafana.llm.prebake.enabled (ka-0000007541); got:\n%s", dep)
	}
	if !strings.Contains(dep, "/grafana-llm-app-plugin:") {
		t.Error("seed initContainer must use the gardenio-owned grafana-llm-app-plugin artifact image (ka-0000007541)")
	}

	// (2) copy-only, no-network seed: a plain `cp` of /plugin, plus the signed-
	// manifest + plugin.json sanity assertions. plugin.json is what Grafana keys
	// discovery on; MANIFEST.txt is the Grafana-root signature that lets the seeded
	// copy load WITHOUT an unsigned-allow — both must be present or the artifact
	// image is broken, so the seed `test -f`s each before copying (ka-0000007541).
	if !strings.Contains(dep, "cp -r /plugin/. /mnt/plugin/") {
		t.Error("seed initContainer must copy the pre-built dist (cp), not install at runtime (ka-0000007541)")
	}
	if !strings.Contains(dep, "test -f /plugin/plugin.json") {
		t.Error("seed initContainer must assert plugin.json is present before copying (Grafana discovery key, ka-0000007541)")
	}
	if !strings.Contains(dep, "test -f /plugin/MANIFEST.txt") {
		t.Error("seed initContainer must assert MANIFEST.txt is present before copying — it is the Grafana-root signature that loads the plugin as signed with no unsigned-allow (ka-0000007541)")
	}

	// (3) volume + mount at the plugin's own subdir under the plugins path.
	if !strings.Contains(dep, "mountPath: /var/lib/grafana/plugins/"+llmPrebakePluginID) {
		t.Errorf("plugin dist must mount at /var/lib/grafana/plugins/%s (ka-0000007541); got:\n%s", llmPrebakePluginID, dep)
	}
	if !strings.Contains(dep, "name: llm-plugin") {
		t.Error("an llm-plugin volume must be declared (ka-0000007541)")
	}
}

// TestGrafanaLLMPrebake_SeedAddsNoOffClusterEgress — the runtime seed must add
// ZERO off-cluster egress: the build (download + checksum) happened in CI, so the
// initContainer only `cp`s pre-built bytes. Scan EXECUTABLE lines (drop YAML "#"
// comment lines, which legitimately mention the rationale) for any
// network-fetch / package-install invocation. Mirrors the C4 seed-egress guard.
func TestGrafanaLLMPrebake_SeedAddsNoOffClusterEgress(t *testing.T) {
	t.Parallel()
	dep := helmTemplate(t, "templates/grafana/deployment.yaml", prebakeFlags...)
	region := seedLLMPluginRegion(t, dep)
	for _, banned := range []string{"npm ", "yarn ", "curl ", "wget ", "https://", "http://"} {
		for line := range strings.SplitSeq(region, "\n") {
			code := strings.TrimSpace(line)
			if code == "" || strings.HasPrefix(code, "#") {
				continue
			}
			if strings.Contains(code, banned) {
				t.Errorf("seed initContainer executable line must add no off-cluster egress; found %q in %q (network guarantee, ka-0000007541)", banned, code)
			}
		}
	}
}

// TestGrafanaLLMPrebake_NoUnsignedAllowForLLMApp — grafana-llm-app is
// Grafana-ROOT-SIGNED, so the seeded on-disk copy validates as signed and must
// load WITHOUT being named in GF_PLUGINS_ALLOW_LOADING_UNSIGNED_PLUGINS. (The
// only legitimate unsigned-allow is the custom gardenio-assistant-app, and only
// when grafana.assistant.enabled — not exercised here.) Guards against a future
// edit that "helpfully" unsigned-allows the foundation plugin.
func TestGrafanaLLMPrebake_NoUnsignedAllowForLLMApp(t *testing.T) {
	t.Parallel()
	dep := helmTemplate(t, "templates/grafana/deployment.yaml", prebakeFlags...)
	// Check the env-DECLARATION form ("- name: GF_...") so we do not false-match
	// the var names that appear in the rationale comments.
	if strings.Contains(dep, "name: GF_PLUGINS_ALLOW_LOADING_UNSIGNED_PLUGINS") {
		t.Errorf("prebake of the SIGNED grafana-llm-app must not add GF_PLUGINS_ALLOW_LOADING_UNSIGNED_PLUGINS (ka-0000007541); got:\n%s", dep)
	}
	// And it must not catalog-install grafana-llm-app either (prebake replaces it).
	if strings.Contains(dep, "name: GF_INSTALL_PLUGINS") {
		t.Error("prebake must NOT also render GF_INSTALL_PLUGINS for grafana-llm-app — prebake replaces the catalog install (ka-0000007541)")
	}
}

// TestGrafanaLLMPrebake_NoCatalogEgressNeeded — THE load-bearing test. With
// prebake on and grafana.plugins.allowCatalogEgress OFF under
// default-deny-egress, the grafana-egress NetworkPolicy must render NO internet
// 0.0.0.0/0 / :443 allow — i.e. allowCatalogEgress is NOT required for the chat
// box. It must still allow the in-cluster LiteLLM proxy so completions flow. This
// is the network guarantee the catalog path could not give.
func TestGrafanaLLMPrebake_NoCatalogEgressNeeded(t *testing.T) {
	t.Parallel()
	out := helmTemplate(t, "templates/networkpolicies/networkpolicies.yaml",
		"grafana.enabled=true",
		"grafana.llm.enabled=true",
		"litellm.enabled=true",
		"grafana.llm.prebake.enabled=true",
		// allowCatalogEgress deliberately left at its default (false).
		"networkPolicies.enabled=true",
		"networkPolicies.defaultDenyEgress=true",
	)
	egress := grafanaEgressBlock(t, out)
	// No off-cluster hole.
	if strings.Contains(egress, "cidr: 0.0.0.0/0") || strings.Contains(egress, "port: 443") {
		t.Errorf("with prebake on + allowCatalogEgress off, grafana-egress must NOT open internet 443 — zero off-cluster egress is a NETWORK guarantee (ka-0000007541); got:\n%s", egress)
	}
	// But the in-cluster inference path must still be allowed.
	if !strings.Contains(egress, "component: litellm") || !strings.Contains(egress, "port: 4000") {
		t.Errorf("grafana-egress must still allow the in-cluster LiteLLM proxy when grafana.llm.enabled; got:\n%s", egress)
	}
}

// TestGrafanaLLMPrebake_ConflictWithCatalogInstallFails — prebake SEEDS
// grafana-llm-app, so it must not ALSO be catalog-installed via
// grafana.plugins.install (that would re-introduce the grafana.com egress this
// feature removes and race the seeded copy). Setting both must FAIL the render
// loudly rather than silently ship a half-applied, egress-leaking config.
func TestGrafanaLLMPrebake_ConflictWithCatalogInstallFails(t *testing.T) {
	t.Parallel()
	out, err := helmTemplateAllowFail(t, "templates/grafana/deployment.yaml",
		"grafana.enabled=true",
		"grafana.llm.enabled=true",
		"litellm.enabled=true",
		"grafana.llm.prebake.enabled=true",
		"grafana.plugins.install={grafana-llm-app 1.0.8}",
	)
	if err == nil {
		t.Fatalf("helm template must FAIL when prebake AND a grafana-llm-app catalog install are both set; got success:\n%s", out)
	}
	if !strings.Contains(out, "not ALSO catalog-installed") && !strings.Contains(out, "remove the") {
		t.Errorf("failure must explain the prebake/catalog conflict; got:\n%s", out)
	}
}

// TestGrafanaLLMPrebake_ConflictGuardFiresForEveryInstallForm — the load-bearing
// hardening (ka-0000007541 QC findings #2+#3). GF_INSTALL_PLUGINS accepts more than
// the bare id, and Grafana ids are case-insensitive, so a catalog install of
// grafana-llm-app ALONGSIDE prebake — which re-opens the grafana.com egress hole
// this feature removes — can sneak past a naive `eq id "grafana-llm-app"` guard via
// a "<id> <version>" form, the "<url>;<id>" URL form, or a case variant. Each of
// these MUST fail the render. The guard must ALSO not over-match a distinct sibling
// id like "grafana-llm-app-extra" (no catalog install of grafana-llm-app there), so
// that one must still render.
func TestGrafanaLLMPrebake_ConflictGuardFiresForEveryInstallForm(t *testing.T) {
	t.Parallel()
	// Every leaking form of a grafana-llm-app catalog install. Each value is the
	// single grafana.plugins.install entry set alongside prebake.
	leaking := map[string]string{
		"bare id":           "grafana-llm-app",
		"id + version":      "grafana-llm-app 1.0.8",
		"URL form (;id)":    "https://repo/plugin.zip;grafana-llm-app",
		"URL form + @ver":   "https://repo/plugin.zip;grafana-llm-app@1.0.8",
		"case variant":      "Grafana-LLM-App",
		"case + version":    "Grafana-LLM-App 1.0.8",
		"case URL form":     "https://repo/plugin.zip;Grafana-LLM-App",
		"leading space pad": " grafana-llm-app ",
	}
	for name, entry := range leaking {
		t.Run("fires/"+name, func(t *testing.T) {
			// None of these entries contain a helm --set metacharacter (comma /
			// equals / brackets), so the value passes through verbatim inside the
			// {…} list literal — verified by rendering each form (ka-0000007541).
			out, err := helmTemplateAllowFail(t, "templates/grafana/deployment.yaml",
				append(prebakeFlags, "grafana.plugins.install={"+entry+"}")...)
			if err == nil {
				t.Fatalf("conflict guard must FIRE for a grafana-llm-app catalog install in form %q (%q); got success:\n%s", name, entry, out)
			}
			if !strings.Contains(out, "not ALSO catalog-installed") && !strings.Contains(out, "remove the") {
				t.Errorf("failure for form %q must explain the prebake/catalog conflict; got:\n%s", name, out)
			}
		})
	}

	// Negative: a DIFFERENT plugin id that merely shares the prefix is NOT a
	// grafana-llm-app catalog install — the guard must be id-exact, not a prefix
	// match, so this renders cleanly (seed initContainer present, no fail).
	t.Run("no false positive on sibling id", func(t *testing.T) {
		out, err := helmTemplateAllowFail(t, "templates/grafana/deployment.yaml",
			append(prebakeFlags, "grafana.plugins.install={grafana-llm-app-extra 1.0.0}")...)
		if err != nil {
			t.Fatalf("conflict guard must NOT over-match the sibling id grafana-llm-app-extra; got error:\n%s", out)
		}
		if !strings.Contains(out, "name: seed-llm-plugin") {
			t.Errorf("the sibling-id render should still seed the prebake plugin; got:\n%s", out)
		}
	})
}

// TestGrafanaLLMPrebake_CouplingGateFiresOnEgressLeak — the coupling render-gate
// (ka-0000007541 QC finding #1, the HIGH-framing item). prebake exists ONLY to make
// zero off-cluster egress a NETWORK guarantee, so enabling prebake while leaving the
// egress hole open is a misconfiguration that silently downgrades the guarantee back
// to a config-only convention. The chart must REFUSE to render such a config. Each
// leak below — the catalog hole open, or default-deny not in force — must FAIL.
func TestGrafanaLLMPrebake_CouplingGateFiresOnEgressLeak(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name   string
		flags  []string // overrides layered on top of prebakeFlags
		expect string   // substring the failure message must contain
	}{
		{
			name:   "allowCatalogEgress=true opens the 0.0.0.0/0 hole",
			flags:  []string{"grafana.plugins.allowCatalogEgress=true"},
			expect: "allowCatalogEgress",
		},
		{
			name:   "defaultDenyEgress=false leaves egress unbounded",
			flags:  []string{"networkPolicies.defaultDenyEgress=false"},
			expect: "default-deny-egress",
		},
		{
			name:   "networkPolicies disabled entirely",
			flags:  []string{"networkPolicies.enabled=false"},
			expect: "default-deny-egress",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			// Start from the full prod posture, then apply the leak override. Helm
			// --set is last-wins, so the trailing override flips just that knob.
			flags := append(append([]string{}, prebakeFlags...), tc.flags...)
			out, err := helmTemplateAllowFail(t, "templates/grafana/deployment.yaml", flags...)
			if err == nil {
				t.Fatalf("coupling gate must FIRE when prebake is on but %s; got success:\n%s", tc.name, out)
			}
			if !strings.Contains(out, tc.expect) {
				t.Errorf("failure must mention %q so the operator knows which knob is wrong; got:\n%s", tc.expect, out)
			}
			// Whatever the leak, the message must point at the prebake guarantee.
			if !strings.Contains(out, "ka-0000007541") {
				t.Errorf("coupling-gate failure should cite the prebake network guarantee (ka-0000007541); got:\n%s", out)
			}
		})
	}
}

// TestGrafanaLLMPrebake_CouplingGateScopedToPrebake — the coupling gate must NOT
// red the STAGING posture: the catalog-install path deliberately runs
// grafana.llm.enabled=true with allowCatalogEgress=true + defaultDenyEgress=true
// (it needs the grafana.com hole for the boot-time fetch). The gate is keyed on
// prebake.enabled, so staging — which never sets prebake — must render cleanly even
// with the catalog hole open. This is what lets the SAME chart serve both staging
// (catalog) and prod (prebake) without forking.
func TestGrafanaLLMPrebake_CouplingGateScopedToPrebake(t *testing.T) {
	t.Parallel()
	out, err := helmTemplateAllowFail(t, "templates/grafana/deployment.yaml",
		"grafana.enabled=true",
		"grafana.llm.enabled=true",
		"litellm.enabled=true",
		// Staging shape: catalog install + the egress hole, no prebake.
		"grafana.plugins.install={grafana-llm-app 1.0.8}",
		"grafana.plugins.allowCatalogEgress=true",
		"networkPolicies.enabled=true",
		"networkPolicies.defaultDenyEgress=true",
	)
	if err != nil {
		t.Fatalf("the coupling gate must be scoped to prebake.enabled — the staging catalog path (allowCatalogEgress=true, no prebake) must still render; got error:\n%s", out)
	}
	// Sanity: this really is the catalog path (GF_INSTALL_PLUGINS present, no seed).
	if !strings.Contains(out, "GF_INSTALL_PLUGINS") {
		t.Errorf("staging shape must use the catalog install (GF_INSTALL_PLUGINS); got:\n%s", out)
	}
	if strings.Contains(out, "name: seed-llm-plugin") {
		t.Errorf("staging shape must NOT render the prebake seed initContainer; got:\n%s", out)
	}
}

// TestGrafanaLLMPrebake_RequiresLLMEnabled — prebake is the delivery mechanism
// for the LLM chat box; the plugin's provisioning/key/managed-SA all gate on
// grafana.llm.enabled. Enabling prebake without grafana.llm.enabled must FAIL
// the render loudly rather than seed an unprovisioned plugin (symmetric with the
// assistant→llm guard).
func TestGrafanaLLMPrebake_RequiresLLMEnabled(t *testing.T) {
	t.Parallel()
	out, err := helmTemplateAllowFail(t, "templates/grafana/deployment.yaml",
		"grafana.enabled=true",
		"grafana.llm.prebake.enabled=true",
		// grafana.llm.enabled deliberately left off.
	)
	if err == nil {
		t.Fatalf("helm template must FAIL when grafana.llm.prebake.enabled without grafana.llm.enabled; got success:\n%s", out)
	}
	if !strings.Contains(out, "requires grafana.llm.enabled") {
		t.Errorf("failure must explain the llm.enabled requirement; got:\n%s", out)
	}
}

// TestGrafanaLLMPrebake_RootfsStaysReadOnly — enabling prebake must not relax the
// main container's read-only root filesystem; the plugin lives on a mount, not
// the rootfs. The seed initContainer likewise runs non-root with a RO root.
func TestGrafanaLLMPrebake_RootfsStaysReadOnly(t *testing.T) {
	t.Parallel()
	dep := helmTemplate(t, "templates/grafana/deployment.yaml",
		append(prebakeFlags, "grafana.persistence.enabled=true")...)
	// The main container must keep its read-only rootfs (whole-render check is
	// fine: there is only one main container).
	if !strings.Contains(dep, "readOnlyRootFilesystem: true") {
		t.Error("readOnlyRootFilesystem must stay true — the plugin lives on a mount (ka-0000007541)")
	}
	// The seed initContainer's OWN securityContext must run non-root with a RO
	// root. Scope to the seed-llm-plugin region so we are not satisfied by the
	// pod-level runAsUser or the main container's readOnlyRootFilesystem — i.e.
	// this would FAIL if the initContainer's hardening were dropped.
	region := seedLLMPluginRegion(t, dep)
	if !strings.Contains(region, "runAsUser: 65534") {
		t.Errorf("seed initContainer must run as non-root uid 65534 (ka-0000007541); got:\n%s", region)
	}
	if !strings.Contains(region, "readOnlyRootFilesystem: true") {
		t.Errorf("seed initContainer must set readOnlyRootFilesystem: true (ka-0000007541); got:\n%s", region)
	}
}

// TestGrafanaLLMPrebake_WritableDataDirWhenEphemeral — with persistence OFF and
// the catalog install replaced by prebake (so grafana.plugins.install is empty),
// Grafana still needs a writable /var/lib/grafana for its own state (grafana.db).
// A non-PVC emptyDir must back /var/lib/grafana so the pod boots under
// readOnlyRootFilesystem. Guards the ephemeral case the install-gated fallback
// would otherwise miss.
func TestGrafanaLLMPrebake_WritableDataDirWhenEphemeral(t *testing.T) {
	t.Parallel()
	dep := helmTemplate(t, "templates/grafana/deployment.yaml",
		append(prebakeFlags, "grafana.persistence.enabled=false")...)

	// Anchor to the EXACT data mount, NOT the substring. With prebake on, the
	// deployment ALWAYS renders the plugin mount line
	// "mountPath: /var/lib/grafana/plugins/grafana-llm-app", which also contains
	// "mountPath: /var/lib/grafana" — so a bare strings.Contains false-passes even
	// when the writable data emptyDir for /var/lib/grafana is gone, defeating the
	// very ephemeral case this test claims to guard. Require the contiguous
	// `- name: data` + `mountPath: /var/lib/grafana` volumeMount pair, with the
	// mountPath at end-of-line (the Grafana data root, not the nested /plugins
	// subdir), so only the real data mount can satisfy it (gd-bldXxnY8MP,
	// hardening ka-0000007541).
	if !hasDataMountAtGrafanaRoot(dep) {
		t.Errorf("a writable /var/lib/grafana must be mounted via the `data` volume with prebake on + persistence off — the nested /plugins mount must NOT satisfy this (ka-0000007541, gd-bldXxnY8MP); got:\n%s", dep)
	}

	// And in the ephemeral (persistence-off) case the data volume must be backed
	// by an emptyDir, never a PVC. Reuse the shared volume-definition extractor
	// (skips the volumeMount entry, returns the `volumes:` definition).
	dataVol := dataVolumeBlock(dep)
	if dataVol == "" {
		t.Fatalf("no `- name: data` volume definition found — the data mount would dangle (gd-bldXxnY8MP); got:\n%s", dep)
	}
	if !strings.Contains(dataVol, "emptyDir") {
		t.Errorf("with persistence off, the `data` volume must be backed by emptyDir (not a PVC), got:\n%s", dataVol)
	}
	if strings.Contains(dep, "claimName") {
		t.Error("with persistence off, /var/lib/grafana must be an emptyDir, not a PVC claim")
	}
}

// hasDataMountAtGrafanaRoot reports whether the rendered deployment declares the
// EXACT writable-data volumeMount: a `- name: data` entry whose mountPath is
// /var/lib/grafana at end-of-line (the Grafana data root). It deliberately does
// NOT match the nested /var/lib/grafana/plugins/<id> subdir mount that the
// always-rendered plugin dist declares — that line is what makes a naive
// strings.Contains(dep, "mountPath: /var/lib/grafana") false-pass (gd-bldXxnY8MP).
func hasDataMountAtGrafanaRoot(dep string) bool {
	lines := strings.Split(dep, "\n")
	for i := 0; i+1 < len(lines); i++ {
		if strings.TrimSpace(lines[i]) != "- name: data" {
			continue
		}
		// The mount is the contiguous next line; require the data root exactly,
		// so /var/lib/grafana/plugins/... cannot satisfy it.
		if strings.TrimSpace(lines[i+1]) == "mountPath: /var/lib/grafana" {
			return true
		}
	}
	return false
}

// TestGrafanaLLMPrebake_CoexistsWithAssistant — the prod shape: prebake seeds the
// foundation plugin AND the custom gardenio-assistant-app is on. Both seed
// initContainers must render, each mounting its own distinct plugin subdir
// (no collision). Guards the C6 rollout shape end-to-end.
func TestGrafanaLLMPrebake_CoexistsWithAssistant(t *testing.T) {
	t.Parallel()
	dep := helmTemplate(t, "templates/grafana/deployment.yaml",
		append(prebakeFlags,
			"grafana.assistant.enabled=true",
			"grafana.persistence.enabled=true",
		)...,
	)
	if !strings.Contains(dep, "name: seed-llm-plugin") || !strings.Contains(dep, "name: seed-assistant-plugin") {
		t.Errorf("both seed initContainers must render in the prod shape (prebake + assistant); got:\n%s", dep)
	}
	if !strings.Contains(dep, "mountPath: /var/lib/grafana/plugins/grafana-llm-app") ||
		!strings.Contains(dep, "mountPath: /var/lib/grafana/plugins/gardenio-assistant-app") {
		t.Errorf("each plugin must mount at its own distinct subdir (no collision); got:\n%s", dep)
	}
	// The two plugin dists must be backed by DISTINCT emptyDir volumes — if both
	// mounts aliased one volume, each initContainer's cp would clobber the other.
	// Assert both named volumes are declared.
	if strings.Count(dep, "name: llm-plugin") < 2 || strings.Count(dep, "name: assistant-plugin") < 2 {
		// Each volume name appears at least twice: once in a volumeMount and once
		// in the volumes: declaration. Fewer means a missing volume or an alias.
		t.Errorf("llm-plugin and assistant-plugin must each be a distinct declared volume + mount (no aliasing); got:\n%s", dep)
	}
}

// seedLLMPluginRegion returns the slice of the rendered deployment YAML covering
// the seed-llm-plugin initContainer only — from its name marker up to the main
// `containers:` key — so per-initContainer assertions are not satisfied by
// ambient pod-level or main-container fields elsewhere in the render.
func seedLLMPluginRegion(t *testing.T, dep string) string {
	t.Helper()
	start := strings.Index(dep, "name: seed-llm-plugin")
	if start < 0 {
		t.Fatalf("seed-llm-plugin initContainer must render; got:\n%s", dep)
	}
	region := dep[start:]
	if end := strings.Index(region, "\n      containers:"); end >= 0 {
		region = region[:end]
	}
	return region
}
