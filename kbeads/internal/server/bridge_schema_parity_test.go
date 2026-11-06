package server

import (
	"encoding/json"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"

	"go.yaml.in/yaml/v3"

	"gardenio/kbeads/internal/model"
)

// parseBridgeInitGoFields locates the bridge's config sources relative to the
// monorepo root and returns the field names listed under the typeKey entry.
// It reads gardenio/controller/internal/bridge/init_configs.yaml (the data
// configs() embeds) first, then the Go entries split out of configs() such as
// type:message in init_message.go, extracting their FieldDef.Name literals by
// parsing the Go source. It parses rather than imports because
// "internal/bridge" is only importable from within the gardenio/controller
// module, even under the shared go.work. Its consumer is
// TestBridgeTypeConfig_SubsetOfBuiltin (config_bridge_subset_guard_test.go).
func parseBridgeInitGoFields(t *testing.T, typeKey string) []string {
	t.Helper()

	root := findRepoRoot(t)
	if names, ok := bridgeInitYAMLFields(t, root, typeKey); ok {
		return names
	}
	initGoPath := filepath.Join(root, "gardenio", "controller", "internal", "bridge", "init*.go")
	paths, err := filepath.Glob(initGoPath)
	if err != nil || len(paths) == 0 {
		t.Fatalf("glob %s: %v (found %d files)", initGoPath, err, len(paths))
	}

	var typeEntry *ast.CompositeLit
	for _, path := range paths {
		if typeEntry != nil {
			break
		}
		if strings.HasSuffix(path, "_test.go") {
			continue
		}
		file, err := parser.ParseFile(token.NewFileSet(), path, nil, 0)
		if err != nil {
			t.Fatalf("parse %s: %v", path, err)
		}
		typeEntry = findTypeEntry(file, typeKey)
	}
	if typeEntry == nil {
		t.Fatalf("no %s file has a %q entry (source reshape?)", initGoPath, typeKey)
	}

	var fieldsList *ast.CompositeLit
	for _, elt := range typeEntry.Elts {
		kv, ok := elt.(*ast.KeyValueExpr)
		if !ok {
			continue
		}
		ident, ok := kv.Key.(*ast.Ident)
		if !ok || ident.Name != "Fields" {
			continue
		}
		fieldsList, _ = kv.Value.(*ast.CompositeLit)
	}
	if fieldsList == nil {
		t.Fatalf("%q entry in %s has no Fields slice (source reshape?)", typeKey, initGoPath)
	}

	var names []string
	for _, elt := range fieldsList.Elts {
		fieldLit, ok := elt.(*ast.CompositeLit)
		if !ok {
			continue
		}
		for _, fe := range fieldLit.Elts {
			kv, ok := fe.(*ast.KeyValueExpr)
			if !ok {
				continue
			}
			ident, ok := kv.Key.(*ast.Ident)
			if !ok || ident.Name != "Name" {
				continue
			}
			nameLit, ok := kv.Value.(*ast.BasicLit)
			if !ok || nameLit.Kind != token.STRING {
				continue
			}
			name, err := strconv.Unquote(nameLit.Value)
			if err != nil {
				continue
			}
			names = append(names, name)
		}
	}
	return names
}

// bridgeInitYAMLFields returns the field names of typeKey's entry in the
// bridge's init_configs.yaml, and false when the file has no such entry.
func bridgeInitYAMLFields(t *testing.T, root, typeKey string) ([]string, bool) {
	t.Helper()
	path := filepath.Join(root, "gardenio", "controller", "internal", "bridge", "init_configs.yaml")
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	var entries map[string]struct {
		Fields []struct {
			Name string `yaml:"name"`
		} `yaml:"fields"`
	}
	if err := yaml.Unmarshal(data, &entries); err != nil {
		t.Fatalf("parse %s: %v", path, err)
	}
	entry, ok := entries[typeKey]
	if !ok {
		return nil, false
	}
	names := make([]string, 0, len(entry.Fields))
	for _, f := range entry.Fields {
		names = append(names, f.Name)
	}
	return names, true
}

// findTypeEntry returns the composite literal keyed typeKey in file (the
// `"type:<t>": TypeConfig{...}` map entry), or nil.
func findTypeEntry(file *ast.File, typeKey string) *ast.CompositeLit {
	var typeEntry *ast.CompositeLit
	ast.Inspect(file, func(n ast.Node) bool {
		if typeEntry != nil {
			return false
		}
		kv, ok := n.(*ast.KeyValueExpr)
		if !ok {
			return true
		}
		keyLit, ok := kv.Key.(*ast.BasicLit)
		if !ok || keyLit.Kind != token.STRING {
			return true
		}
		key, err := strconv.Unquote(keyLit.Value)
		if err != nil || key != typeKey {
			return true
		}
		cl, ok := kv.Value.(*ast.CompositeLit)
		if !ok {
			return true
		}
		typeEntry = cl
		return false
	})
	return typeEntry
}

// findRepoRoot walks up from this test file's directory to the monorepo root
// (identified by go.work, which links the gardenio/controller and kbeads
// modules together).
func findRepoRoot(t *testing.T) string {
	t.Helper()
	_, thisFile, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime.Caller failed")
	}
	dir := filepath.Dir(thisFile)
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.work")); err == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			t.Fatalf("walked up from %s to filesystem root without finding go.work", thisFile)
		}
		dir = parent
	}
}

// TestMessageSchema_RouterFieldsDeclared reads the conversation router's
// message bead field constants (gardenio/controller/internal/conversation/
// router.go, the Bead* consts) and asserts the builtin declares each one, then
// validates a notice exactly as the router writes it. The router's detail and
// footnote were undeclared, so every routed notice (agent spawn, schedule,
// death, crash) 400'd "unknown field" and fell back to a bead-less Slack post.
func TestMessageSchema_RouterFieldsDeclared(t *testing.T) {
	routerGo := filepath.Join(findRepoRoot(t), "gardenio", "controller", "internal", "conversation", "router.go")
	file, err := parser.ParseFile(token.NewFileSet(), routerGo, nil, 0)
	if err != nil {
		t.Fatalf("parse %s: %v", routerGo, err)
	}
	var routerFields []string
	for _, decl := range file.Decls {
		gd, ok := decl.(*ast.GenDecl)
		if !ok || gd.Tok != token.CONST {
			continue
		}
		for _, spec := range gd.Specs {
			vs := spec.(*ast.ValueSpec)
			for i, name := range vs.Names {
				if !strings.HasPrefix(name.Name, "Bead") || i >= len(vs.Values) {
					continue
				}
				lit, ok := vs.Values[i].(*ast.BasicLit)
				if !ok || lit.Kind != token.STRING {
					continue
				}
				v, err := strconv.Unquote(lit.Value)
				if err != nil {
					t.Fatalf("unquote %s: %v", name.Name, err)
				}
				routerFields = append(routerFields, v)
			}
		}
	}
	if len(routerFields) < 9 {
		t.Fatalf("parsed %d Bead* consts out of router.go (%v); parser likely broke on a source reshape", len(routerFields), routerFields)
	}

	tc := parseBuiltinSchema(t, "message")
	declared := make(map[string]bool, len(tc.Fields))
	for _, fd := range tc.Fields {
		declared[fd.Name] = true
	}
	for _, name := range routerFields {
		if !declared[name] {
			t.Errorf("conversation router writes message field %q, which builtinConfigs[\"type:message\"] does not declare", name)
		}
	}

	// A routed agent spawn notice: the create, then the Slack ref stamped back.
	notice := json.RawMessage(`{"kind":"notice","text":"Agent spawned: crew-1","detail":"*Agent spawned:* crew-1","footnote":"Forensics on bead ` + "`gd-1`" + `","author":"agent:crew-1","channel_id":"gd-ch1","slack_channel":"C0CREW","slack_ts":"1690000000.000100"}`)
	if err := model.ValidateTypeFields(notice, tc); err != nil {
		t.Errorf("routed notice must validate on type:message: %v", err)
	}
	if err := validateMessageCardFields(notice); err != nil {
		t.Errorf("routed notice must pass the card rules: %v", err)
	}
}

// TestMessageSchema_SlackMirrorFields validates a Slack message with files as
// the bridge's Slack mirror writes it (commons_mirror.go mirrorSlackMessage,
// with bot_thread_forward.go slackFilesToFields). files,
// slack_attachment_count and slack_has_images were undeclared, so the daemon
// 400'd "unknown field" on every Slack post that carried a file and the post
// was not mirrored into the Commons at all (gd-2Ie1cDAm4K).
func TestMessageSchema_SlackMirrorFields(t *testing.T) {
	tc := parseBuiltinSchema(t, "message")
	files, _ := json.Marshal(`[{"name":"plot.png","mimetype":"image/png","size":2048,"url":"https://acme.slack.com/files/U0ADA/F1/plot.png"}]`)
	post := json.RawMessage(`{"channel_id":"gd-ch1","author":"oidc:ada@example.com","kind":"text","text":"see attached","origin":"slack",` +
		`"slack_channel":"C0BOTH","slack_ts":"1690000005.000100","slack_user_id":"U0ADA",` +
		`"slack_attachment_count":"1","slack_has_images":"true","files":` + string(files) + `}`)
	if err := model.ValidateTypeFields(post, tc); err != nil {
		t.Errorf("a mirrored Slack file post must validate on type:message: %v", err)
	}
}

// TestMessageSchema_CommonsSnapField validates a Commons farm snapshot as the
// Commons composer writes it (gardenio/controller/internal/commons/write.go
// and snap.go): the picture in files and the view's context in snap, both
// JSON in a string as every Commons field is. An undeclared snap would 400
// every Snap post "unknown field" (the #645 class, gd-GbF2JS04h5).
func TestMessageSchema_CommonsSnapField(t *testing.T) {
	tc := parseBuiltinSchema(t, "message")
	files, _ := json.Marshal(`[{"name":"farm-gardenio-yard-20261019-2340.png","mimetype":"image/png","size":48213,"url":"https://garden.example/garden/c/f/gardenio/gd-ch1/0123456789abcdef0123456789abcdef/farm-gardenio-yard-20261019-2340.png"}]`)
	snap, _ := json.Marshal(`{"kind":"farm","url":"https://garden.example/garden?at=yard&z=2","at":"yard","z":2,"who":"mine","framed":[{"kind":"bead","id":"gd-1","title":"Dig"}],"vp":[1280,800],"taken_at":"2026-10-19T23:40:00Z"}`)
	post := json.RawMessage(`{"channel_id":"gd-ch1","author":"oidc:ada@example.com","kind":"text","origin":"commons","files":` + string(files) + `,"snap":` + string(snap) + `}`)
	if err := model.ValidateTypeFields(post, tc); err != nil {
		t.Errorf("a Commons snapshot must validate on type:message: %v", err)
	}
}

// TestMessageSchema_CommonsDMFields validates the beads gb say --to and --ask
// write into a Commons DM channel (gardenio/controller/internal/bridge/
// api_conversation_dm.go), its Slack IM ref stamped after, and a Slack reply
// to the ask (dm_replies.go). An undeclared `to` would 400 every DM "unknown
// field" (the #645 class, gd-F4oSp9S96G).
func TestMessageSchema_CommonsDMFields(t *testing.T) {
	tc := parseBuiltinSchema(t, "message")
	for name, raw := range map[string]string{
		"ask":   `{"channel_id":"gd-dm1","author":"agent:crew-1","kind":"ask","origin":"cli","text":"ship it?","to":"oidc:ada@example.com","slack_channel":"D0ADA","slack_ts":"1700000000.000100"}`,
		"reply": `{"channel_id":"gd-dm1","thread_id":"gd-ask1","author":"oidc:ada@example.com","kind":"text","origin":"slack","text":"yes","slack_channel":"D0ADA","slack_ts":"1700000001.000200","slack_user_id":"U0ADA"}`,
	} {
		if err := model.ValidateTypeFields(json.RawMessage(raw), tc); err != nil {
			t.Errorf("a Commons DM %s must validate on type:message: %v", name, err)
		}
	}
}

// TestMessageSchema_CommonsNonceField validates a Commons say and reply as the
// workbench sends them (gardenio/controller/internal/commons/write.go and
// sendonce.go): each carries its client nonce. An undeclared nonce would 400
// every workbench send "unknown field" (the #645 class, gd-JpYV1FWtnZ).
func TestMessageSchema_CommonsNonceField(t *testing.T) {
	tc := parseBuiltinSchema(t, "message")
	for name, raw := range map[string]string{
		"say":   `{"channel_id":"gd-ch1","author":"oidc:ada@example.com","kind":"text","origin":"commons","text":"hello","nonce":"n-0123456789abcdef"}`,
		"reply": `{"channel_id":"gd-ch1","thread_id":"gd-say1","author":"oidc:ada@example.com","kind":"text","origin":"commons","text":"yes","nonce":"n-fedcba9876543210"}`,
	} {
		if err := model.ValidateTypeFields(json.RawMessage(raw), tc); err != nil {
			t.Errorf("a Commons %s with a nonce must validate on type:message: %v", name, err)
		}
	}
}

// TestMessageSchema_CommonsAboutField validates a Commons say as the workbench's
// "Ask the steward about this" sends it (gardenio/controller/internal/commons/
// write.go): it names the bead it asks about. An undeclared about would 400
// every such ask "unknown field" (the #645 class, gd-LisW87zrnf).
func TestMessageSchema_CommonsAboutField(t *testing.T) {
	tc := parseBuiltinSchema(t, "message")
	raw := `{"channel_id":"gd-ch1","author":"oidc:ada@example.com","kind":"text","origin":"commons","text":"@steward why is this stuck?","nonce":"n-0123456789abcdef","about":"gd-wbEpic"}`
	if err := model.ValidateTypeFields(json.RawMessage(raw), tc); err != nil {
		t.Errorf("a Commons say with about must validate on type:message: %v", err)
	}
}
