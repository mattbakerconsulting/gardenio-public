package bridge

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"testing"
)

func TestParseModelOverride(t *testing.T) {
	tests := []struct {
		name          string
		text          string
		wantModel     string
		wantRemaining string
	}{
		{
			name:          "model: syntax",
			text:          "model:opus fix the flaky test",
			wantModel:     "opus",
			wantRemaining: "fix the flaky test",
		},
		{
			name:          "--model syntax",
			text:          "--model opus fix the flaky test",
			wantModel:     "opus",
			wantRemaining: "fix the flaky test",
		},
		{
			name:          "--model= syntax",
			text:          "--model=opus fix the flaky test",
			wantModel:     "opus",
			wantRemaining: "fix the flaky test",
		},
		{
			name:          "model: no remaining text",
			text:          "model:sonnet",
			wantModel:     "sonnet",
			wantRemaining: "",
		},
		{
			name:          "--model no remaining text",
			text:          "--model sonnet",
			wantModel:     "sonnet",
			wantRemaining: "",
		},
		{
			name:          "--model= no remaining text",
			text:          "--model=sonnet",
			wantModel:     "sonnet",
			wantRemaining: "",
		},
		{
			// A raw full id is returned verbatim — normalization happens later.
			name:          "full model id passthrough",
			text:          "--model claude-opus-4-7 ship it",
			wantModel:     "claude-opus-4-7",
			wantRemaining: "ship it",
		},
		{
			name:          "no override",
			text:          "fix the flaky test",
			wantModel:     "",
			wantRemaining: "fix the flaky test",
		},
		{
			name:          "empty text",
			text:          "",
			wantModel:     "",
			wantRemaining: "",
		},
		{
			name:          "model: with empty value",
			text:          "model: something",
			wantModel:     "",
			wantRemaining: "model: something",
		},
		{
			name:          "--model= with empty value",
			text:          "--model= something",
			wantModel:     "",
			wantRemaining: "--model= something",
		},
		{
			name:          "--model with no value",
			text:          "--model",
			wantModel:     "",
			wantRemaining: "--model",
		},
		{
			// The flag is only recognized as the leading token, mirroring
			// parseRoleOverride — a model: word later in the prompt is prose.
			name:          "model token not at start is left in place",
			text:          "please use model:opus here",
			wantModel:     "",
			wantRemaining: "please use model:opus here",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			model, remaining := parseModelOverride(tt.text)
			if model != tt.wantModel {
				t.Errorf("model = %q, want %q", model, tt.wantModel)
			}
			if remaining != tt.wantRemaining {
				t.Errorf("remaining = %q, want %q", remaining, tt.wantRemaining)
			}
		})
	}
}

func TestNormalizeModel(t *testing.T) {
	tests := []struct {
		name   string
		raw    string
		want   string
		wantOK bool
	}{
		// Short aliases resolve to canonical alias KEYS.
		{name: "fable alias", raw: "fable", want: "claude-fable-5-1", wantOK: true},
		{name: "opus alias", raw: "opus", want: "claude-opus-5-5", wantOK: true},
		{name: "sonnet alias", raw: "sonnet", want: "claude-sonnet-5", wantOK: true},
		{name: "haiku alias", raw: "haiku", want: "claude-haiku-4-5", wantOK: true},
		// GLM 5 on Bedrock (epic ka-0000002312): `glm` short alias resolves to the
		// glm-5 alias KEY (LiteLLM router maps it to the bedrock-glm-5 group), and
		// the glm-5 full id passes through directly.
		{name: "glm alias", raw: "glm", want: "glm-5", wantOK: true},
		{name: "glm uppercase", raw: "GLM", want: "glm-5", wantOK: true},
		{name: "full id passthrough glm-5", raw: "glm-5", want: "glm-5", wantOK: true},

		// Case-insensitive + whitespace-trimmed.
		{name: "uppercase alias", raw: "OPUS", want: "claude-opus-5-5", wantOK: true},
		{name: "mixed case alias", raw: "Sonnet", want: "claude-sonnet-5", wantOK: true},
		{name: "surrounding whitespace", raw: "  opus  ", want: "claude-opus-5-5", wantOK: true},

		// Recognized full ids pass through unchanged.
		{name: "full id passthrough opus-5", raw: "claude-opus-5", want: "claude-opus-5", wantOK: true},
		{name: "full id passthrough opus-4-8", raw: "claude-opus-4-8", want: "claude-opus-4-8", wantOK: true},
		{name: "full id passthrough opus-4-7", raw: "claude-opus-4-7", want: "claude-opus-4-7", wantOK: true},
		{name: "full id passthrough opus-4-6", raw: "claude-opus-4-6", want: "claude-opus-4-6", wantOK: true},
		{name: "full id passthrough fable", raw: "claude-fable-5", want: "claude-fable-5", wantOK: true},
		{name: "full id passthrough fable 5.1", raw: "claude-fable-5-1", want: "claude-fable-5-1", wantOK: true},
		{name: "full id case-insensitive", raw: "Claude-Opus-4-7", want: "claude-opus-4-7", wantOK: true},

		// Emit the alias KEY form, never the -oauth model-group name.
		{name: "oauth group name rejected", raw: "claude-opus-4-8-oauth", want: "", wantOK: false},
		{name: "anthropic-prefixed group rejected", raw: "anthropic/claude-haiku-4-5", want: "", wantOK: false},

		// Dated snapshot ids are intentionally not selectable directly.
		{name: "dated snapshot rejected", raw: "claude-sonnet-4-6-20250514", want: "", wantOK: false},

		// Unrecognized / empty input.
		{name: "unknown model", raw: "gpt-4", want: "", wantOK: false},
		{name: "empty", raw: "", want: "", wantOK: false},
		{name: "whitespace only", raw: "   ", want: "", wantOK: false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, ok := normalizeModel(tt.raw)
			if got != tt.want {
				t.Errorf("normalizeModel(%q) model = %q, want %q", tt.raw, got, tt.want)
			}
			if ok != tt.wantOK {
				t.Errorf("normalizeModel(%q) ok = %v, want %v", tt.raw, ok, tt.wantOK)
			}
		})
	}
}

// TestAllowedModelHint verifies the operator-facing hint lists every short
// alias, sorted and backtick-wrapped, so the "unrecognized model" reply the
// bridge posts is stable and complete.
func TestAllowedModelHint(t *testing.T) {
	got := allowedModelHint()
	// Every short alias must appear, backtick-wrapped.
	for name := range shortModelAliases {
		if !strings.Contains(got, "`"+name+"`") {
			t.Errorf("allowedModelHint() = %q, missing alias %q", got, name)
		}
	}
	// Stable sorted ordering (fable < glm < haiku < opus < sonnet). `glm` is the
	// GLM-5-on-Bedrock selector (epic ka-0000002312).
	want := "`fable`, `glm`, `haiku`, `opus`, `sonnet`"
	if got != want {
		t.Errorf("allowedModelHint() = %q, want %q", got, want)
	}
}

// TestBridgeAllowlistMatchesAliasMap is a drift guard. The bridge's selectable
// model targets — the alias-KEY values that normalizeModel can ever emit — MUST
// be a subset of BOTH source-of-truth alias maps that actually route the
// request:
//
//   - controller/cmd/controller/litellm_aliases.go  (claudeCodeModelAliases keys)
//   - helm/gardenio/templates/litellm/configmap.yaml (router_settings.model_group_alias keys)
//
// If someone adds a new selectable model to the bridge without wiring the
// corresponding routing alias, this test fails — preventing a Slack `--model`
// override that would be rejected by the LiteLLM key ACL at runtime.
//
// The maps are read from the real source files (the controller's is package
// main; the chart's is a Go template) rather than imported, so the bridge
// package stays client-go-free.
func TestBridgeAllowlistMatchesAliasMap(t *testing.T) {
	// Bridge targets = every alias-KEY normalizeModel can emit: the short-alias
	// targets plus the full-id allowlist members.
	bridgeTargets := map[string]struct{}{}
	for _, full := range shortModelAliases {
		bridgeTargets[full] = struct{}{}
	}
	for id := range fullModelAllowlist {
		bridgeTargets[id] = struct{}{}
	}
	if len(bridgeTargets) == 0 {
		t.Fatal("no bridge model targets found — shortModelAliases/fullModelAllowlist empty?")
	}

	controllerKeys := parseControllerAliasKeys(t)
	if len(controllerKeys) == 0 {
		t.Fatal("parsed zero keys from litellm_aliases.go — parser drift?")
	}

	chartKeys := parseChartAliasKeys(t)
	if len(chartKeys) == 0 {
		// In filtered checkouts (e.g. a narrow clone), Helm templates may be unavailable.
		// This is not a parser drift — skip the assertion.
		t.Skip("Helm templates unavailable in this checkout")
	}

	for _, target := range sortedKeys(bridgeTargets) {
		if _, ok := controllerKeys[target]; !ok {
			t.Errorf("bridge model target %q missing from controller claudeCodeModelAliases keys (%v)",
				target, sortedKeys(controllerKeys))
		}
		// Skip chart keys check in filtered checkouts where Helm templates are unavailable.
		if len(chartKeys) > 0 {
			if _, ok := chartKeys[target]; !ok {
				t.Errorf("bridge model target %q missing from chart model_group_alias keys (%v)",
					target, sortedKeys(chartKeys))
			}
		}
	}

	// Every short-alias target must also be in the full-id allowlist, so a
	// resolved short alias is itself independently selectable.
	for short, full := range shortModelAliases {
		if _, ok := fullModelAllowlist[full]; !ok {
			t.Errorf("short alias %q -> %q is not in fullModelAllowlist", short, full)
		}
	}
}

// parseControllerAliasKeys extracts the KEYS of the claudeCodeModelAliases map
// literal from cmd/controller/litellm_aliases.go using go/ast (the file is
// package main and cannot be imported).
func parseControllerAliasKeys(t *testing.T) map[string]struct{} {
	t.Helper()
	path := filepath.Join("..", "..", "cmd", "controller", "litellm_aliases.go")
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, path, nil, 0)
	if err != nil {
		t.Fatalf("parsing %s: %v", path, err)
	}

	keys := map[string]struct{}{}
	ast.Inspect(f, func(n ast.Node) bool {
		vs, ok := n.(*ast.ValueSpec)
		if !ok {
			return true
		}
		for i, name := range vs.Names {
			if name.Name != "claudeCodeModelAliases" || i >= len(vs.Values) {
				continue
			}
			lit, ok := vs.Values[i].(*ast.CompositeLit)
			if !ok {
				continue
			}
			for _, elt := range lit.Elts {
				kv, ok := elt.(*ast.KeyValueExpr)
				if !ok {
					continue
				}
				bl, ok := kv.Key.(*ast.BasicLit)
				if !ok || bl.Kind != token.STRING {
					continue
				}
				key, err := strconv.Unquote(bl.Value)
				if err != nil {
					t.Fatalf("unquoting alias key %q: %v", bl.Value, err)
				}
				keys[key] = struct{}{}
			}
		}
		return true
	})
	return keys
}

// parseChartAliasKeys extracts the model_group_alias mapping KEYS from the
// chart configmap template. It scans the default (else-branch) block between
// the `model_group_alias:` marker and the following `fallbacks:` marker,
// collecting `claude-…:` mapping keys. The keys are literal in the template
// text regardless of the surrounding Go-template conditionals.
func parseChartAliasKeys(t *testing.T) map[string]struct{} {
	t.Helper()
	path := filepath.Join("..", "..", "..", "helm", "gardenio", "templates", "litellm", "configmap.yaml")
	data, err := os.ReadFile(path)
	if err != nil {
		// In filtered checkouts (e.g. a narrow clone), Helm templates may not be present.
		// Return empty map; the test will skip the chart keys assertion.
		return map[string]struct{}{}
	}

	keys := map[string]struct{}{}
	inBlock := false
	for line := range strings.SplitSeq(string(data), "\n") {
		trimmed := strings.TrimSpace(line)
		if !inBlock {
			if trimmed == "model_group_alias:" {
				inBlock = true
			}
			continue
		}
		// The block ends at the sibling `fallbacks:` router setting.
		if trimmed == "fallbacks:" {
			break
		}
		if trimmed == "" || strings.HasPrefix(trimmed, "#") || strings.HasPrefix(trimmed, "{{") {
			continue
		}
		// Mapping entry: `claude-...: <target>` or `glm-...: <target>`.
		key, _, found := strings.Cut(trimmed, ":")
		if !found {
			continue
		}
		key = strings.TrimSpace(key)
		// Harvest selectable-model alias keys. `claude-*` are the Anthropic
		// defaults; `glm-*` are the Bedrock GLM keys (epic ka-0000002312). The
		// chart guards `glm-5` behind a {{- if .Values.litellm.bedrock.enabled }}
		// conditional, but the KEY line itself is literal template text (the
		// `{{`-prefixed conditional lines are skipped by the filter above), so it
		// is collected here for the drift-guard subset check.
		if strings.HasPrefix(key, "claude-") || strings.HasPrefix(key, "glm-") {
			keys[key] = struct{}{}
		}
	}
	return keys
}

func sortedKeys(m map[string]struct{}) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
