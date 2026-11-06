package bridge

import (
	"go/ast"
	"go/parser"
	"go/token"
	"path/filepath"
	"sort"
	"testing"
)

// This file is the controller-side D5/D9 schema drift guard (design doc §5.3).
// The controller keeps its OWN copy of the type:project / type:role schemas in
// init.go (configs()), which EnsureConfigs upserts into the daemon on startup.
// That copy must stay field-for-field consistent with both (a) the controller's
// ProjectInfo / RoleInfo parser (beadsapi/{projects,roles}.go), which reads the
// fields, and (b) the kbeads builtin (the single source of truth). If the
// init.go copy omits a field the parser reads, an EnsureConfigs run can
// overwrite the daemon schema with one that 400-rejects the very field the
// controller then tries to read — the D5 bug (gd-u8AbgveHQW), which this guard
// makes impossible to reintroduce silently.
//
// Mirroring the kbeads side (config_driftguard_test.go), a HAND-MAINTAINED
// list ({project,role}InfoReadFields) is the readable, canonical enumeration,
// and a PARSE-THE-PARSER check AST-extracts every fields["..."] read from the
// parser source and asserts it both equals the hand list and is a subset of
// the init.go copy — so the hand list itself can never silently fall behind
// the parser. The same tests pin the type of the fields that have drifted
// before, since a wrong type is its own silent-rejection class.

// projectInfoReadFields enumerates every fields[...] key the controller's
// ProjectInfo parser reads (beadsapi/projects.go ListProjectBeads). Mirror of
// the kbeads-side guard (kbeads/internal/server/config_driftguard_test.go):
// every key here MUST be declared in the controller's type:project copy
// (init.go) AND in the kbeads builtin (the single source of truth), or the
// daemon 400-rejects project beads carrying it. When the parser learns a new field, add it here AND
// to both schemas. TestProjectSchema_ParserDrift_Controller keeps this list
// honest against the actual parser source.
var projectInfoReadFields = []string{
	"prefix", "git_url", "default_branch", "image", "storage_class",
	"service_account", "model", "runtime_class", "docker", "postgres",
	"needs_k8s_api", "ziti_access", "ziti_service_id", "ziti_bind",
	"ziti_ttl", "ziti_optional", "session_resume", "ultracode", "dm_default", "cpu_request",
	"cpu_limit", "memory_request", "memory_limit", "account",
	"default_timezone", "schedules_enabled", "triggers_enabled",
	"slack_channel", "channel_roles", "channel_modes",
	"channel_runtime_class", "secrets", "env", "repos", "resource_mapping",
	"prewarmed_pool", "claude_config", "env_json", "execution_target",
	"card_theme", "harness", "land_default_channel", "land_slack_mirror",
	"land_kind", "namespace", "nats_epoch", "github_app", "github_installation_id",
	"release_tag_pattern", "release_targets", "release_branch",
}

// roleInfoReadFields enumerates every fields[...] key the controller's RoleInfo
// parser reads (beadsapi/roles.go ListRoleBeads). Mirror of the kbeads-side
// guard (kbeads/internal/server/config_driftguard_test.go): every key here MUST
// be declared in the controller's type:role copy (init.go) AND in the kbeads
// builtin (the single source of truth), or the daemon 400-rejects role beads
// carrying it. When the parser learns a new field, add it here AND to both
// schemas. TestRoleSchema_ParserDrift_Controller keeps this list honest
// against the actual parser source.
var roleInfoReadFields = []string{
	"image", "storage_class", "service_account", "runtime_class", "docker",
	"postgres", "needs_k8s_api", "ziti_access", "ziti_service_id", "ziti_bind",
	"ziti_ttl", "ziti_optional", "session_resume", "ultracode", "cpu_request", "cpu_limit",
	"memory_request", "memory_limit", "account", "model", "secrets", "env",
	"env_json", "claude_config", "account_whitelist", "no_account_spill",
	"enabled_skills", "disabled_skills", "enabled_agents", "disabled_agents",
	"execution_target", "privileged", "harness", "allowed_harnesses",
	"machine_client", "verifier",
}

// --- parse-the-parser layer ------------------------------------------------

// extractControllerFieldReads parses the controller parser source at relPath
// (relative to internal/bridge) and returns the sorted distinct set of keys
// read via fields["literal"]. It recognises only the exact index-with-string-
// literal idiom the parser uses (every ProjectInfo/RoleInfo read is
// fields["x"]); a read through any other shape would be missed, so if the
// parser changes idiom this guard must be revisited rather than silently
// under-reporting.
func extractControllerFieldReads(t *testing.T, relPath string) []string {
	t.Helper()
	abs, err := filepath.Abs(relPath)
	if err != nil {
		t.Fatalf("resolve %s: %v", relPath, err)
	}
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, abs, nil, 0)
	if err != nil {
		t.Fatalf("parse %s: %v", relPath, err)
	}
	seen := map[string]struct{}{}
	ast.Inspect(file, func(n ast.Node) bool {
		idx, ok := n.(*ast.IndexExpr)
		if !ok {
			return true
		}
		ident, ok := idx.X.(*ast.Ident)
		if !ok || ident.Name != "fields" {
			return true
		}
		lit, ok := idx.Index.(*ast.BasicLit)
		if !ok || lit.Kind != token.STRING {
			return true
		}
		key := lit.Value
		if len(key) >= 2 && key[0] == '"' && key[len(key)-1] == '"' {
			key = key[1 : len(key)-1]
		}
		if key != "" {
			seen[key] = struct{}{}
		}
		return true
	})
	out := make([]string, 0, len(seen))
	for k := range seen {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// symmetricDiff returns elements only in a and only in b (both sorted).
func symmetricDiff(a, b []string) (onlyA, onlyB []string) {
	as := make(map[string]struct{}, len(a))
	for _, x := range a {
		as[x] = struct{}{}
	}
	bs := make(map[string]struct{}, len(b))
	for _, x := range b {
		bs[x] = struct{}{}
	}
	for _, x := range a {
		if _, ok := bs[x]; !ok {
			onlyA = append(onlyA, x)
		}
	}
	for _, x := range b {
		if _, ok := as[x]; !ok {
			onlyB = append(onlyB, x)
		}
	}
	sort.Strings(onlyA)
	sort.Strings(onlyB)
	return onlyA, onlyB
}

// TestProjectSchema_ParserDrift_Controller AST-extracts every fields["..."]
// read from the ProjectInfo parser source and asserts (1) it matches the
// hand-maintained projectInfoReadFields list and (2) every read field is
// declared in the controller's type:project copy (init.go). This removes the
// human-memory step that could let the hand list (and thus the guard) silently
// fall behind the parser — the §5.3 drift guard. It also pins the
// previously-drifted fields (account/model/runtime_class/triggers_enabled/
// dm_default/resource_mapping/claude_config — the D5 set, gd-u8AbgveHQW) to the
// right primitive type.
func TestProjectSchema_ParserDrift_Controller(t *testing.T) {
	read := extractControllerFieldReads(t, "../beadsapi/projects.go")
	if len(read) == 0 {
		t.Fatal("extracted zero fields[...] reads from projects.go — parser idiom changed; update extractControllerFieldReads")
	}
	onlyParser, onlyList := symmetricDiff(read, projectInfoReadFields)
	if len(onlyParser) > 0 {
		t.Errorf("ProjectInfo parser reads %v but projectInfoReadFields omits them — "+
			"add to the list AND the type:project copy in init.go (and the kbeads builtin)", onlyParser)
	}
	if len(onlyList) > 0 {
		t.Errorf("projectInfoReadFields lists %v but the ProjectInfo parser no longer reads them — "+
			"remove from the list", onlyList)
	}
	cfg, ok := configs()["type:project"].(TypeConfig)
	if !ok {
		t.Fatal("configs() missing type:project entry")
	}
	byName := make(map[string]FieldDef, len(cfg.Fields))
	for _, f := range cfg.Fields {
		byName[f.Name] = f
	}
	for _, name := range read {
		if _, ok := byName[name]; !ok {
			t.Errorf("ProjectInfo parser reads field %q but the controller's type:project "+
				"copy omits it — add it to the type:project TypeConfig in init.go", name)
		}
	}
	// The D5 drift set: the 7 fields the schema historically omitted. Pin the
	// type of each so a future edit can't re-add them with the wrong primitive.
	wantTypes := map[string]string{
		"account":          "string",
		"model":            "string",
		"runtime_class":    "string",
		"triggers_enabled": "boolean",
		"dm_default":       "boolean",
		"resource_mapping": "json",
		"claude_config":    "json",
	}
	for name, want := range wantTypes {
		f, ok := byName[name]
		if !ok {
			t.Errorf("type:project missing field %q (D5 drift set)", name)
			continue
		}
		if f.Type != want {
			t.Errorf("type:project field %q type=%q, want %q", name, f.Type, want)
		}
	}
}

// TestRoleSchema_ParserDrift_Controller is the type:role analogue: it
// AST-extracts the RoleInfo parser reads and asserts they match
// roleInfoReadFields and are all declared in the controller's type:role copy
// (design doc §8.4). It also pins the type of the model field, the reason for
// gd-7xcz6dWHaD.
func TestRoleSchema_ParserDrift_Controller(t *testing.T) {
	read := extractControllerFieldReads(t, "../beadsapi/roles.go")
	if len(read) == 0 {
		t.Fatal("extracted zero fields[...] reads from roles.go — parser idiom changed; update extractControllerFieldReads")
	}
	onlyParser, onlyList := symmetricDiff(read, roleInfoReadFields)
	if len(onlyParser) > 0 {
		t.Errorf("RoleInfo parser reads %v but roleInfoReadFields omits them — "+
			"add to the list AND the type:role copy in init.go (and the kbeads builtin)", onlyParser)
	}
	if len(onlyList) > 0 {
		t.Errorf("roleInfoReadFields lists %v but the RoleInfo parser no longer reads them — "+
			"remove from the list", onlyList)
	}
	cfg, ok := configs()["type:role"].(TypeConfig)
	if !ok {
		t.Fatal("configs() missing type:role entry")
	}
	byName := make(map[string]FieldDef, len(cfg.Fields))
	for _, f := range cfg.Fields {
		byName[f.Name] = f
	}
	for _, name := range read {
		if _, ok := byName[name]; !ok {
			t.Errorf("RoleInfo parser reads field %q but the controller's type:role "+
				"copy omits it — add it to the type:role TypeConfig in init.go", name)
		}
	}
	if f, ok := byName["model"]; !ok {
		t.Error("type:role missing field model (RoleInfo reads fields[model])")
	} else if f.Type != "string" {
		t.Errorf("type:role model type=%q, want string", f.Type)
	}
}
