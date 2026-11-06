package tests

import (
	"strings"
	"testing"
)

// LiteLLM model_group_alias placement + grafana OpenAI->Bedrock aliases
// (ka-0000005911, epic ka-0000002011 Phase 0).
//
// EMPIRICAL BASIS (verified live against prod gardenio-litellm v1.84.8): LiteLLM
// only honors model_group_alias when it is nested under `router_settings`. A
// top-level model_group_alias block is silently ignored — even the master key
// 400s "Invalid model name" on a top-level-only alias, and the live router map
// stays {} . It is ALSO where the controller's runtime alias-sync
// (controller/cmd/controller/litellm_aliases.go) reads and writes. The chart
// formerly rendered the block at top level (inert); these tests pin the block
// to router_settings so the placement bug cannot silently regress.

// extractConfigYAML returns the inner config.yaml string from the rendered
// litellm ConfigMap. The ConfigMap renders config.yaml as a `|` block scalar
// whose body is indented under `  config.yaml: |`; we strip that 4-space
// indent so callers can reason about the config's own indentation.
func extractConfigYAML(t *testing.T, rendered string) string {
	t.Helper()
	lines := strings.Split(rendered, "\n")
	start := -1
	for i, line := range lines {
		if strings.TrimSpace(line) == "config.yaml: |" {
			start = i + 1
			break
		}
	}
	if start < 0 {
		t.Fatalf("rendered ConfigMap has no 'config.yaml: |' block:\n%s", rendered)
	}
	var body []string
	for _, line := range lines[start:] {
		if strings.TrimSpace(line) == "" {
			body = append(body, "")
			continue
		}
		// The block scalar body is indented 4 spaces under data.config.yaml.
		// A line that is NOT at least 4-space indented ends the block (the next
		// ConfigMap key, the `---` separator, or EOF).
		if !strings.HasPrefix(line, "    ") {
			break
		}
		body = append(body, strings.TrimPrefix(line, "    "))
	}
	return strings.Join(body, "\n")
}

// modelGroupAliasUnderRouterSettings returns the parsed model_group_alias map
// AND whether it was found nested under a `router_settings:` top-level key (as
// opposed to a bare top-level `model_group_alias:`). The config's own
// indentation (after extractConfigYAML strips the block-scalar indent) is:
//
//	router_settings:            # 0-space
//	  model_group_alias:        # 2-space
//	    <alias>: <target>       # 4-space
func modelGroupAliasUnderRouterSettings(cfg string) (aliases map[string]string, underRouter bool) {
	aliases = map[string]string{}
	lines := strings.Split(cfg, "\n")
	inRouter := false
	inAlias := false
	aliasIndent := -1
	for _, line := range lines {
		trimmed := strings.TrimSpace(line)
		if trimmed == "" || strings.HasPrefix(trimmed, "#") {
			continue
		}
		indent := len(line) - len(strings.TrimLeft(line, " "))

		// A 0-indent key ends any nested section we were tracking.
		if indent == 0 {
			inRouter = trimmed == "router_settings:"
			inAlias = false
			aliasIndent = -1
			continue
		}

		if inRouter && trimmed == "model_group_alias:" {
			inAlias = true
			underRouter = true
			aliasIndent = indent
			continue
		}
		// A bare top-level model_group_alias (indent>0 but not under router) —
		// record that we entered an alias block but DO NOT set underRouter.
		if !inRouter && trimmed == "model_group_alias:" {
			inAlias = true
			aliasIndent = indent
			continue
		}

		if inAlias {
			// Alias entries sit one level deeper than the model_group_alias key.
			if indent <= aliasIndent {
				inAlias = false
				aliasIndent = -1
				// fallthrough: this line may itself be a router_settings sibling
			} else if k, v, ok := strings.Cut(trimmed, ":"); ok {
				kv := strings.TrimSpace(v)
				if kv != "" {
					aliases[strings.TrimSpace(k)] = kv
				}
				continue
			}
		}
	}
	return aliases, underRouter
}

//  1. The default chart renders model_group_alias UNDER router_settings (never
//     at top level) — the placement LiteLLM actually honors.
func TestLitellmAlias_DefaultUnderRouterSettings(t *testing.T) {
	t.Parallel()
	out := helmTemplate(t, "templates/litellm/configmap.yaml",
		"litellm.enabled=true",
	)
	cfg := extractConfigYAML(t, out)

	// There must be NO top-level `model_group_alias:` (0-space indent) line.
	for line := range strings.SplitSeq(cfg, "\n") {
		if line == "model_group_alias:" {
			t.Error("model_group_alias must NOT render at top level — LiteLLM ignores it there (ka-0000005911)")
		}
	}

	aliases, underRouter := modelGroupAliasUnderRouterSettings(cfg)
	if !underRouter {
		t.Fatal("model_group_alias must render under router_settings (ka-0000005911)")
	}
	// The claude-* default that every agent path depends on must be present.
	if got := aliases["claude-opus-4-8"]; got != "claude-opus-4-8-oauth" {
		t.Errorf("default claude-opus-4-8 must alias to claude-opus-4-8-oauth, got %q", got)
	}
	// opus-5 routing group (gd-XjWJfwqEP5): the additive default alias must be
	// present so `--model opus-5`/Claude Code's opus-5 requests route through the
	// claude-opus-5-oauth OAuth-forwarding group.
	if got := aliases["claude-opus-5"]; got != "claude-opus-5-oauth" {
		t.Errorf("default claude-opus-5 must alias to claude-opus-5-oauth, got %q", got)
	}
	// opus-5.5 routing group (plan:opus55-release T1): claude-opus-5-5 is the
	// fleet's new default Opus alias, routed through the claude-opus-5-5-oauth
	// OAuth-forwarding group. claude-opus-5 stays routable for rollback.
	if got := aliases["claude-opus-5-5"]; got != "claude-opus-5-5-oauth" {
		t.Errorf("default claude-opus-5-5 must alias to claude-opus-5-5-oauth, got %q", got)
	}
	// sonnet-5 routing group (ka-0000004084): Claude Code >= 2.1.257 resolves
	// its `sonnet` alias to the bare claude-sonnet-5 id, which 401'd at the key
	// ACL until this alias + group existed.
	if got := aliases["claude-sonnet-5"]; got != "claude-sonnet-5-oauth" {
		t.Errorf("default claude-sonnet-5 must alias to claude-sonnet-5-oauth, got %q", got)
	}
	if !strings.Contains(cfg, "- model_name: claude-sonnet-5-oauth") {
		t.Error("default model_list must define the claude-sonnet-5-oauth group the alias targets")
	}
}

//  2. The grafana OSS-chat Phase-0 aliases map the grafana-llm-app OpenAI model
//     ids onto the in-cluster Bedrock chat groups, and render under
//     router_settings with every target a defined model_name in model_list.
//     Co-renders litellm.bedrock.enabled=true (+ a credential path) so the
//     Bedrock target groups actually exist in model_list.
//
//     ADDITIVE-MERGE ACCEPTANCE (ka-0000003830): litellm.modelGroupAlias no
//     longer REPLACES the chart base map — the overlay sets ONLY its gpt-*
//     additions (mirroring the real deploy-values overlays post-dedup), and the
//     rendered config MUST contain BOTH the chart's single-source claude-*/glm-5
//     base AND those gpt-* additions. This is the drift-elimination guarantee:
//     an overlay never re-copies the base map, so it cannot drift from it.
func TestLitellmAlias_GrafanaOpenAIToBedrock(t *testing.T) {
	t.Parallel()
	out := helmTemplate(t, "templates/litellm/configmap.yaml",
		"litellm.enabled=true",
		"litellm.bedrock.enabled=true",
		"litellm.embedding.irsaRoleArn=arn:aws:iam::234567890123:role/gardenio-litellm-bedrock",
		// Overlay contributes ONLY the grafana gpt-* additions — the claude-*/glm-5
		// base is NOT repeated here (the chart renders it unconditionally and
		// merges these on top). helm --set treats "." as a key delimiter, so
		// escape the dot in gpt-3.5-turbo (otherwise it nests as gpt-3 -> 5-turbo).
		`litellm.modelGroupAlias.gpt-4o=bedrock-claude-opus-4-7`,
		`litellm.modelGroupAlias.gpt-4=bedrock-claude-opus-4-7`,
		`litellm.modelGroupAlias.gpt-4o-mini=bedrock-claude-haiku-4-5`,
		`litellm.modelGroupAlias.gpt-3\.5-turbo=bedrock-claude-haiku-4-5`,
	)
	cfg := extractConfigYAML(t, out)

	aliases, underRouter := modelGroupAliasUnderRouterSettings(cfg)
	if !underRouter {
		t.Fatal("grafana aliases must render under router_settings (ka-0000005911)")
	}

	// The chart's single-source base map must survive the overlay merge — an
	// overlay that only adds gpt-* keys must NOT drop the claude-* base or the
	// bedrock-gated glm-5 selector.
	wantBase := map[string]string{
		"claude-opus-5":    "claude-opus-5-oauth",
		"claude-opus-5-5":  "claude-opus-5-5-oauth",
		"claude-opus-4-8":  "claude-opus-4-8-oauth",
		"claude-fable-5-1": "claude-fable-5-1-oauth",
		"claude-fable-5":   "claude-fable-5-oauth",
		"claude-sonnet-5":  "claude-sonnet-5-oauth",
		"glm-5":            "bedrock-glm-5", // rendered because bedrock.enabled=true
	}
	for id, want := range wantBase {
		if got := aliases[id]; got != want {
			t.Errorf("base alias %q must survive the overlay merge as %q, got %q "+
				"(litellm.modelGroupAlias must be ADDITIVE, not replace the base — ka-0000003830)", id, want, got)
		}
	}

	wantGrafana := map[string]string{
		"gpt-4o":        "bedrock-claude-opus-4-7",
		"gpt-4":         "bedrock-claude-opus-4-7",
		"gpt-4o-mini":   "bedrock-claude-haiku-4-5",
		"gpt-3.5-turbo": "bedrock-claude-haiku-4-5",
	}
	for id, want := range wantGrafana {
		if got := aliases[id]; got != want {
			t.Errorf("grafana alias %q must map to %q, got %q", id, want, got)
		}
	}

	// Collect defined model_names from model_list. List items render as
	// "- model_name: <name>", so strip a leading "- " before matching.
	defined := map[string]bool{}
	for line := range strings.SplitSeq(cfg, "\n") {
		trimmed := strings.TrimPrefix(strings.TrimSpace(line), "- ")
		if rest, ok := strings.CutPrefix(trimmed, "model_name:"); ok {
			name := strings.Trim(strings.TrimSpace(rest), `"`)
			defined[name] = true
		}
	}
	for alias, target := range aliases {
		if !defined[target] {
			t.Errorf("model_group_alias %q -> %q, but %q is not a defined model_name "+
				"(litellm would 400 'no healthy deployments') — defined: %v",
				alias, target, target, defined)
		}
	}
	// Specifically the Bedrock chat groups the grafana aliases target.
	for _, m := range []string{"bedrock-claude-opus-4-7", "bedrock-claude-haiku-4-5"} {
		if !defined[m] {
			t.Errorf("Bedrock group %q must be a defined model_name when bedrock.enabled "+
				"(grafana aliases target it)", m)
		}
	}
}

//  3. Prod default (no modelGroupAlias override) is claude-*-only — the grafana
//     gpt-* aliases must NOT leak into a render that did not opt in. Guards
//     against accidentally defaulting the OpenAI aliases on (which, with bedrock
//     off, would render dangling targets).
func TestLitellmAlias_NoGrafanaAliasesByDefault(t *testing.T) {
	t.Parallel()
	out := helmTemplate(t, "templates/litellm/configmap.yaml",
		"litellm.enabled=true",
	)
	cfg := extractConfigYAML(t, out)
	aliases, _ := modelGroupAliasUnderRouterSettings(cfg)
	for _, id := range []string{"gpt-4o", "gpt-4", "gpt-4o-mini", "gpt-3.5-turbo"} {
		if _, ok := aliases[id]; ok {
			t.Errorf("grafana OpenAI alias %q must NOT be present in the default render "+
				"(opt-in via litellm.modelGroupAlias in the non-prod overlay only)", id)
		}
	}
}

// LiteLLM router-level fallbacks (ka-0000008144). The chart bakes a default
// `fallbacks: [{claude-fable-5-oauth: [claude-opus-5-oauth]},
// {claude-opus-4-8-oauth: [claude-opus-5-oauth]}]` under router_settings so a
// usage-limit 429 on either the fleet-default fable-5 account model or the
// crew-e2e-pinned opus-4-8 model (ka-0000004014) auto-retries on opus-5. The
// opus-4-8 arm was added for bug ka-0000007458 (crew pods spun forever on
// "No fallback model group found for original model_group=claude-opus-4-8-oauth"
// once their per-agent key budget exhausted).
//
// EMPIRICAL BASIS (verified source-level against LiteLLM v1.84.8, the pinned
// image tag): the /v1/messages anthropic_messages passthrough route (which
// 100% of gardenio agents use) dispatches through Router.async_function_with_fallbacks,
// which reads router_settings.fallbacks; an Anthropic HTTP 429 rate_limit_error
// (incl. the "monthly spend limit" message) maps deterministically to
// litellm.RateLimitError, handled by the GENERIC `fallbacks` chain (NOT
// content_policy_fallbacks / context_window_fallbacks). Like model_group_alias,
// `fallbacks` is ONLY honored nested under router_settings — a top-level block
// is silently ignored. These tests pin placement + shape so neither regresses.

// fallbacksUnderRouterSettings parses the `fallbacks:` block-sequence nested
// under `router_settings:` into a map primary-group -> ordered fallback groups,
// and reports whether it was found under router_settings (as opposed to a bare
// top-level `fallbacks:`). The config's own indentation (after extractConfigYAML
// strips the block-scalar indent) is:
//
//	router_settings:                 # 0-space
//	  fallbacks:                      # 2-space
//	  - <primary>:                    # 2-space "- " item, key at 4
//	    - <fallback>                  # 6-space list entry
//
// It also reports whether ANY top-level (0-space) `fallbacks:` key was seen,
// which must never happen (LiteLLM ignores it there).
func fallbacksUnderRouterSettings(cfg string) (fb map[string][]string, underRouter, topLevel bool) {
	fb = map[string][]string{}
	lines := strings.Split(cfg, "\n")
	inRouter := false
	inFallbacks := false
	fbIndent := -1
	curKey := ""
	for _, line := range lines {
		trimmed := strings.TrimSpace(line)
		if trimmed == "" || strings.HasPrefix(trimmed, "#") {
			continue
		}
		indent := len(line) - len(strings.TrimLeft(line, " "))

		// A 0-indent key ends any nested section we were tracking.
		if indent == 0 {
			if trimmed == "fallbacks:" {
				topLevel = true
			}
			inRouter = trimmed == "router_settings:"
			inFallbacks = false
			fbIndent = -1
			curKey = ""
			continue
		}

		if inRouter && trimmed == "fallbacks:" {
			inFallbacks = true
			underRouter = true
			fbIndent = indent
			continue
		}
		// A fallbacks key nested somewhere OTHER than router_settings — record we
		// entered a fallbacks block but DO NOT set underRouter.
		if !inRouter && trimmed == "fallbacks:" {
			inFallbacks = true
			fbIndent = indent
			continue
		}

		if inFallbacks {
			// The block ends when indentation returns to at-or-above the
			// `fallbacks:` key level (a router_settings sibling like
			// extraRouterSettings; a 0-indent line was handled above).
			if indent <= fbIndent {
				inFallbacks = false
				fbIndent = -1
				curKey = ""
				continue
			}
			// Strip an optional leading "- " block-sequence item marker. In
			// toYaml block form, BOTH the primary-key line ("- fable:") and each
			// target entry ("- opus") carry a dash; they are distinguished by
			// whether the remainder contains a ":" (key) or not (scalar entry).
			item, hadDash := strings.CutPrefix(trimmed, "- ")
			if k, v, ok := strings.Cut(item, ":"); ok {
				// "<primary>:" (optionally "<primary>: [inline list]") — a new
				// primary group in the fallbacks list.
				curKey = strings.TrimSpace(k)
				if _, seen := fb[curKey]; !seen {
					fb[curKey] = []string{}
				}
				if iv := strings.TrimSpace(v); iv != "" {
					fb[curKey] = append(fb[curKey], parseFlowList(iv)...)
				}
			} else if hadDash && curKey != "" {
				// "- <fallback>" — a target entry for the current primary key.
				fb[curKey] = append(fb[curKey], strings.Trim(item, `"`))
			}
			continue
		}
	}
	return fb, underRouter, topLevel
}

// parseFlowList parses a YAML flow sequence like `["a", "b"]` into []string.
// Only used for the inline-list rendering variant; the toYaml block form is
// handled by the "- entry" path above.
func parseFlowList(s string) []string {
	s = strings.TrimSpace(s)
	s = strings.TrimPrefix(s, "[")
	s = strings.TrimSuffix(s, "]")
	var out []string
	for part := range strings.SplitSeq(s, ",") {
		p := strings.Trim(strings.TrimSpace(part), `"'`)
		if p != "" {
			out = append(out, p)
		}
	}
	return out
}

//  4. The default chart renders the fable-5 -> opus-5 fallback UNDER
//     router_settings (never at top level), as a valid List[Dict[str,List[str]]]
//     whose primary and target are both defined model groups in model_list.
func TestLitellmFallbacks_DefaultFableToOpus(t *testing.T) {
	t.Parallel()
	out := helmTemplate(t, "templates/litellm/configmap.yaml",
		"litellm.enabled=true",
	)
	cfg := extractConfigYAML(t, out)

	fb, underRouter, topLevel := fallbacksUnderRouterSettings(cfg)
	if topLevel {
		t.Error("fallbacks must NOT render at top level — LiteLLM ignores it there (ka-0000008144)")
	}
	if !underRouter {
		t.Fatal("fallbacks must render under router_settings (ka-0000008144)")
	}
	// Both fable groups carry the arm: 5.1 is the live fable tier
	// (ka-0000002744) and 5 is kept for rollback / beads still pinning it.
	for _, primary := range []string{"claude-fable-5-1-oauth", "claude-fable-5-oauth"} {
		targets, ok := fb[primary]
		if !ok {
			t.Fatalf("default fallbacks must map %s, got %v", primary, fb)
		}
		if len(targets) != 1 || targets[0] != "claude-opus-5-5-oauth" {
			t.Errorf("%s must fall back to [claude-opus-5-5-oauth], got %v", primary, targets)
		}
	}

	// Regression guard (bug ka-0000007458): the crew-e2e role bead (ka-0000004014)
	// PINS claude-opus-4-8-oauth. Without this arm an exhausted per-agent key
	// yielded "No fallback model group found for original
	// model_group=claude-opus-4-8-oauth" and the harness spin-looped a monthly
	// spend-limit 429 forever. opus-4-8 must fall back to opus-5.5.
	opusTargets, ok := fb["claude-opus-4-8-oauth"]
	if !ok {
		t.Fatalf("default fallbacks must map claude-opus-4-8-oauth (ka-0000007458), got %v", fb)
	}
	if len(opusTargets) != 1 || opusTargets[0] != "claude-opus-5-5-oauth" {
		t.Errorf("claude-opus-4-8-oauth must fall back to [claude-opus-5-5-oauth], got %v", opusTargets)
	}

	// plan:opus55-release T1: a pinned claude-opus-5-oauth key that exhausts its
	// own budget fails over to the new default opus-5.5 group too.
	opus5Targets, ok := fb["claude-opus-5-oauth"]
	if !ok {
		t.Fatalf("default fallbacks must map claude-opus-5-oauth, got %v", fb)
	}
	if len(opus5Targets) != 1 || opus5Targets[0] != "claude-opus-5-5-oauth" {
		t.Errorf("claude-opus-5-oauth must fall back to [claude-opus-5-5-oauth], got %v", opus5Targets)
	}

	// Every fallback primary key AND target must be a defined model_name in
	// model_list — else LiteLLM 400s "no healthy deployments" on the retry.
	defined := definedModelNames(cfg)
	for primary, tgts := range fb {
		if !defined[primary] {
			t.Errorf("fallbacks primary %q is not a defined model_name in model_list — defined: %v", primary, defined)
		}
		for _, tg := range tgts {
			if !defined[tg] {
				t.Errorf("fallbacks target %q (for %q) is not a defined model_name in model_list — defined: %v", tg, primary, defined)
			}
		}
	}

	// Regression guard (bug ka-0000001221): the ungated cheap Haiku group must
	// NEVER fall back onto the gated, expensive claude-opus-5-oauth. That arm
	// (once added in ka-0000000592) drags a WORKING request on an ungated model
	// onto a gated one that 429s on the monthly spend cap, killing Haiku-pinned
	// agents whose own model is perfectly healthy. Falling back from the
	// cheapest, ungated model to the most expensive, gated one inverts the
	// intent of a fallback and can only make things worse.
	if _, present := fb["anthropic/claude-haiku-4-5"]; present {
		t.Errorf("anthropic/claude-haiku-4-5 must have NO fallback arm — the ungated cheap model must not spill onto gated opus-5 (ka-0000001221); got %v", fb["anthropic/claude-haiku-4-5"])
	}
}

//  5. Adding fallbacks does NOT disturb the model_group_alias defaults — the two
//     router_settings siblings must coexist. Guards against an indentation
//     regression that would swallow one into the other.
func TestLitellmFallbacks_AliasStillIntact(t *testing.T) {
	t.Parallel()
	out := helmTemplate(t, "templates/litellm/configmap.yaml",
		"litellm.enabled=true",
	)
	cfg := extractConfigYAML(t, out)

	aliases, underRouter := modelGroupAliasUnderRouterSettings(cfg)
	if !underRouter {
		t.Fatal("model_group_alias must still render under router_settings alongside fallbacks")
	}
	if got := aliases["claude-opus-4-8"]; got != "claude-opus-4-8-oauth" {
		t.Errorf("model_group_alias claude-opus-4-8 must survive fallbacks addition, got %q", got)
	}
	if got := aliases["claude-fable-5"]; got != "claude-fable-5-oauth" {
		t.Errorf("model_group_alias claude-fable-5 must survive fallbacks addition, got %q", got)
	}
	if got := aliases["claude-fable-5-1"]; got != "claude-fable-5-1-oauth" {
		t.Errorf("model_group_alias claude-fable-5-1 must survive fallbacks addition, got %q", got)
	}
}

//  6. An empty litellm.fallbacks override renders NO fallbacks key — the per-env
//     kill switch. `{{- with }}` drops the block on an empty list.
func TestLitellmFallbacks_EmptyDisables(t *testing.T) {
	t.Parallel()
	out := helmTemplateJSON(t, "templates/litellm/configmap.yaml",
		map[string]string{"litellm.fallbacks": "[]"},
		"litellm.enabled=true",
	)
	cfg := extractConfigYAML(t, out)
	if _, underRouter, topLevel := fallbacksUnderRouterSettings(cfg); underRouter || topLevel {
		t.Error("litellm.fallbacks=[] must render NO fallbacks block (per-env kill switch, ka-0000008144)")
	}
	// model_group_alias must be unaffected by the disable.
	if aliases, underRouter := modelGroupAliasUnderRouterSettings(cfg); !underRouter ||
		aliases["claude-opus-4-8"] != "claude-opus-4-8-oauth" {
		t.Error("disabling fallbacks must not disturb model_group_alias")
	}
}

//  7. A custom litellm.fallbacks override renders verbatim under router_settings
//     (multi-target, multiple primaries), proving the block is data-driven and
//     not hardcoded to the fable->opus default.
func TestLitellmFallbacks_CustomOverride(t *testing.T) {
	t.Parallel()
	out := helmTemplateJSON(t, "templates/litellm/configmap.yaml",
		map[string]string{
			"litellm.fallbacks": `[{"claude-fable-5-oauth":["claude-opus-4-8-oauth","claude-opus-4-7-oauth"]},{"claude-opus-4-8-oauth":["claude-opus-4-7-oauth"]}]`,
		},
		"litellm.enabled=true",
	)
	cfg := extractConfigYAML(t, out)
	fb, underRouter, _ := fallbacksUnderRouterSettings(cfg)
	if !underRouter {
		t.Fatal("custom fallbacks must render under router_settings")
	}
	if got := fb["claude-fable-5-oauth"]; len(got) != 2 ||
		got[0] != "claude-opus-4-8-oauth" || got[1] != "claude-opus-4-7-oauth" {
		t.Errorf("custom fable-5 fallback chain wrong, got %v", got)
	}
	if got := fb["claude-opus-4-8-oauth"]; len(got) != 1 || got[0] != "claude-opus-4-7-oauth" {
		t.Errorf("custom opus-4-8 fallback chain wrong, got %v", got)
	}
}

// definedModelNames collects the set of model_name values from the rendered
// model_list. List items render as "- model_name: <name>" (optionally quoted).
func definedModelNames(cfg string) map[string]bool {
	defined := map[string]bool{}
	for line := range strings.SplitSeq(cfg, "\n") {
		trimmed := strings.TrimPrefix(strings.TrimSpace(line), "- ")
		if rest, ok := strings.CutPrefix(trimmed, "model_name:"); ok {
			defined[strings.Trim(strings.TrimSpace(rest), `"`)] = true
		}
	}
	return defined
}
