package server

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"sort"
	"testing"
)

// This file is the D5/D9 schema drift guard (design doc §5.2, §5.3, §8.4): the
// "controller reads it but the daemon rejects it" class of bug must never
// silently recur. It keeps a HAND-MAINTAINED list of the fields each controller
// parser reads ({project,role}InfoReadFields below), but does not trust it on
// its own: add a `fields["new_key"]` read to the controller parser and forget
// to append "new_key" to the list, and a list-only guard passes while the
// daemon silently 400-rejects the new key — exactly the D5 bug, one level up.
//
// So we parse the controller's ProjectInfo / RoleInfo parser SOURCE
// (gardenio/controller/internal/beadsapi/{projects,roles}.go), extract every
// fields["..."] index read via the Go AST, and assert:
//
//	(1) the extracted set equals the hand-maintained {project,role}InfoReadFields
//	    list — so the lists can never silently fall behind the parser; and
//	(2) every extracted key is declared in the builtin type:{project,role}
//	    schema — the authoritative D5/D9 guard, computed directly from source
//	    rather than from a list a human has to remember to update.
//
// The parser lives in a sibling Go module (gardenio/controller) reached by
// relative path; CI checks out the whole gardenio monorepo, so the path is
// stable.

// projectInfoReadFields enumerates every fields[...] key the controller's
// ProjectInfo parser reads (gardenio/controller/internal/beadsapi/projects.go).
// Every key here MUST be declared in the type:project schema, or the daemon
// will 400-reject any project bead that carries it. When the parser learns to
// read a new field, add it here AND to the type:project builtin in the same
// change; TestProjectSchema_ParserDrift fails until both are done.
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
// parser reads (gardenio/controller/internal/beadsapi/roles.go ListRoleBeads).
// Every key here MUST be declared in the type:role schema, or the daemon will
// 400-reject any role bead that carries it. When the parser learns to read a
// new field, add it here AND to the type:role builtin in the same change
// (mirrors projectInfoReadFields; enforced by TestRoleSchema_ParserDrift).
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

// controllerParserField is the controller-side parser source for one config
// type: the file that reads bead fields, and the name of the local variable it
// indexes (always assigned from b.fieldsMap()).
type controllerParserField struct {
	relPath string // path relative to kbeads/internal/server
	varName string // local var indexed as varName["..."]
}

var (
	projectInfoParserSrc = controllerParserField{
		relPath: "../../../gardenio/controller/internal/beadsapi/projects.go",
		varName: "fields",
	}
	roleInfoParserSrc = controllerParserField{
		relPath: "../../../gardenio/controller/internal/beadsapi/roles.go",
		varName: "fields",
	}
)

// extractFieldReads parses the given Go source file and returns the sorted set
// of distinct string keys read via `varName["literal"]`. It deliberately only
// recognises the exact index-with-string-literal idiom the parser uses
// (confirmed: every ProjectInfo/RoleInfo field read is `fields["x"]`); a read
// through any other shape (a ranged map, an aliased variable, a computed key)
// would not be picked up, so if the parser ever changes idiom this guard must
// be revisited rather than silently under-reporting.
func extractFieldReads(t *testing.T, relPath, varName string) []string {
	t.Helper()
	abs, err := filepath.Abs(relPath)
	if err != nil {
		t.Fatalf("resolve %s: %v", relPath, err)
	}
	// The drift guard parses sibling source in the gardenio/controller module.
	// CI checks out the whole monorepo, so the file is always there; a partial
	// checkout without it skips rather than fails (gd-uf5Nkji0Zz).
	if _, statErr := os.Stat(abs); os.IsNotExist(statErr) {
		t.Skipf("gardenio sibling source %s not checked out; "+
			"drift guard runs where the controller module is present", relPath)
	}
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, abs, nil, 0)
	if err != nil {
		t.Fatalf("parse %s: %v (is the gardenio sibling module checked out?)", relPath, err)
	}

	seen := map[string]struct{}{}
	ast.Inspect(file, func(n ast.Node) bool {
		idx, ok := n.(*ast.IndexExpr)
		if !ok {
			return true
		}
		ident, ok := idx.X.(*ast.Ident)
		if !ok || ident.Name != varName {
			return true
		}
		lit, ok := idx.Index.(*ast.BasicLit)
		if !ok || lit.Kind != token.STRING {
			return true
		}
		// lit.Value includes the surrounding quotes.
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

// setEqual reports the symmetric difference between two string sets, returning
// the elements only in a and only in b (both sorted).
func setEqual(a, b []string) (onlyA, onlyB []string) {
	bs := make(map[string]struct{}, len(b))
	for _, x := range b {
		bs[x] = struct{}{}
	}
	as := make(map[string]struct{}, len(a))
	for _, x := range a {
		as[x] = struct{}{}
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

// TestProjectSchema_ParserDrift parses the actual ProjectInfo parser source and
// asserts (1) the extracted fields["..."] read set matches the hand-maintained
// projectInfoReadFields list, and (2) every read field is declared in the
// builtin type:project schema. Parsing the parser removes the human-memory
// step that could let the hand-list (and thus the guard) silently fall behind
// the parser and re-introduce the D5 drift (ka-0000000828).
func TestProjectSchema_ParserDrift(t *testing.T) {
	read := extractFieldReads(t, projectInfoParserSrc.relPath, projectInfoParserSrc.varName)
	if len(read) == 0 {
		t.Fatal("extracted zero fields[...] reads from projects.go — parser idiom changed; update extractFieldReads")
	}

	// (1) Hand-list must equal the parser's actual reads.
	onlyParser, onlyList := setEqual(read, projectInfoReadFields)
	if len(onlyParser) > 0 {
		t.Errorf("ProjectInfo parser reads %v but projectInfoReadFields omits them — "+
			"add to the list AND the type:project builtin", onlyParser)
	}
	if len(onlyList) > 0 {
		t.Errorf("projectInfoReadFields lists %v but the ProjectInfo parser no longer reads them — "+
			"remove from the list", onlyList)
	}

	// (2) Every parser-read field must be declared in the builtin schema.
	tc := parseBuiltinSchema(t, "project")
	declared := make(map[string]struct{}, len(tc.Fields))
	for _, f := range tc.Fields {
		declared[f.Name] = struct{}{}
	}
	for _, name := range read {
		if _, ok := declared[name]; !ok {
			t.Errorf("ProjectInfo parser reads field %q but type:project schema omits it "+
				"(daemon will 400-reject writes carrying it) — add it to the type:project "+
				"builtin in kbeads/internal/server/config.go", name)
		}
	}
}

// TestRoleSchema_ParserDrift is the type:role analogue of
// TestProjectSchema_ParserDrift: it parses the RoleInfo parser source and
// asserts the extracted fields["..."] reads match roleInfoReadFields and are
// all declared in the builtin type:role schema (the D9 guard, computed from
// source; ka-0000000828).
func TestRoleSchema_ParserDrift(t *testing.T) {
	read := extractFieldReads(t, roleInfoParserSrc.relPath, roleInfoParserSrc.varName)
	if len(read) == 0 {
		t.Fatal("extracted zero fields[...] reads from roles.go — parser idiom changed; update extractFieldReads")
	}

	onlyParser, onlyList := setEqual(read, roleInfoReadFields)
	if len(onlyParser) > 0 {
		t.Errorf("RoleInfo parser reads %v but roleInfoReadFields omits them — "+
			"add to the list AND the type:role builtin", onlyParser)
	}
	if len(onlyList) > 0 {
		t.Errorf("roleInfoReadFields lists %v but the RoleInfo parser no longer reads them — "+
			"remove from the list", onlyList)
	}

	tc := parseBuiltinSchema(t, "role")
	declared := make(map[string]struct{}, len(tc.Fields))
	for _, f := range tc.Fields {
		declared[f.Name] = struct{}{}
	}
	for _, name := range read {
		if _, ok := declared[name]; !ok {
			t.Errorf("RoleInfo parser reads field %q but type:role schema omits it "+
				"(daemon will 400-reject writes carrying it) — add it to the type:role "+
				"builtin in kbeads/internal/server/config.go", name)
		}
	}
}
