package contract

import (
	"os"
	"reflect"
	"slices"
	"sort"
	"strconv"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"

	"gardenio/lands/contract/landsv1"
)

const specPath = "openapi.yaml"

// fieldCap is the most properties a noun may carry (gd-EWSKPH7pa5 section 7:
// "~6 fields per noun"). Ticket, the widest, has 9: the heraldry spec's arms
// (2026-10-25-heraldry-design.md section 3.3) raised it from 8. Raising this is
// a design change, not a test fix.
const fieldCap = 9

// forbiddenFields may never appear as a property or parameter name: the
// caller's identity is derived from the bearer, and nothing upstream-shaped is
// exposed (gd-EWSKPH7pa5 section 2, Attribution).
var forbiddenFields = []string{
	"operated_by", "token", "daemon_url", "project", "labels", "fields",
	"relevance_score", "created_by", "owner", "notes",
}

var wantProblemCodes = []string{
	"unauthenticated", "not_found", "forbidden", "invalid_request", "conflict",
	"budget_exhausted", "land_paused", "rate_limited", "upstream_unavailable",
}

var wantEventTypes = []string{
	"ticket.updated", "decision.opened", "decision.resolved", "escalation.opened",
	"message.posted", "plan.updated", "budget.updated", "release.cut", "release.deployed",
}

// pictureTypes are the only image types a response may carry. SVG is never
// one: it is a document that can carry script.
var pictureTypes = []string{"image/png", "image/jpeg", "image/gif", "image/webp"}

var httpMethods = []string{"get", "put", "post", "patch", "delete", "head", "options", "trace"}

type node = map[string]any

func loadSpec(t *testing.T) node {
	t.Helper()
	raw, err := os.ReadFile(specPath)
	if err != nil {
		t.Fatal(err)
	}
	var doc node
	if err := yaml.Unmarshal(raw, &doc); err != nil {
		t.Fatalf("parse %s: %v", specPath, err)
	}
	return doc
}

// at walks a "#/a/b/c" JSON pointer.
func at(t *testing.T, doc node, ref string) node {
	t.Helper()
	cur := any(doc)
	for _, part := range strings.Split(strings.TrimPrefix(ref, "#/"), "/") {
		m, ok := cur.(node)
		if !ok {
			t.Fatalf("%s: %q is not an object", ref, part)
		}
		cur = m[part]
	}
	m, ok := cur.(node)
	if !ok {
		t.Fatalf("%s does not resolve to an object", ref)
	}
	return m
}

// deref follows one $ref, if present.
func deref(t *testing.T, doc, n node) node {
	t.Helper()
	if r, ok := n["$ref"].(string); ok {
		return at(t, doc, r)
	}
	return n
}

type operation struct {
	method, path string
	op           node
	params       []node // path-level then operation-level, dereferenced
}

func operations(t *testing.T, doc node) []operation {
	t.Helper()
	var ops []operation
	paths := doc["paths"].(node)
	keys := make([]string, 0, len(paths))
	for k := range paths {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, p := range keys {
		item := paths[p].(node)
		var shared []node
		if ps, ok := item["parameters"].([]any); ok {
			for _, x := range ps {
				shared = append(shared, deref(t, doc, x.(node)))
			}
		}
		for _, m := range httpMethods {
			op, ok := item[m].(node)
			if !ok {
				continue
			}
			params := slices.Clone(shared)
			if ps, ok := op["parameters"].([]any); ok {
				for _, x := range ps {
					params = append(params, deref(t, doc, x.(node)))
				}
			}
			ops = append(ops, operation{method: m, path: p, op: op, params: params})
		}
	}
	if len(ops) == 0 {
		t.Fatal("no operations found")
	}
	return ops
}

func (o operation) id() string { s, _ := o.op["operationId"].(string); return s }

func (o operation) hasParam(name, in string) bool {
	for _, p := range o.params {
		if p["name"] == name && p["in"] == in {
			return true
		}
	}
	return false
}

func TestSpecVersion(t *testing.T) {
	doc := loadSpec(t)
	if doc["openapi"] != "3.1.0" {
		t.Errorf("openapi = %v, want 3.1.0", doc["openapi"])
	}
	if v := doc["info"].(node)["version"]; v != landsv1.ContractVersion {
		t.Errorf("info.version %v != landsv1.ContractVersion %q; bump both together", v, landsv1.ContractVersion)
	}
}

func TestOperations(t *testing.T) {
	doc := loadSpec(t)
	seen := map[string]bool{}
	for _, o := range operations(t, doc) {
		name := strings.ToUpper(o.method) + " " + o.path
		if !strings.HasPrefix(o.path, "/lands/v1/") {
			t.Errorf("%s: every path lives under /lands/v1/", name)
		}
		if o.id() == "" || seen[o.id()] {
			t.Errorf("%s: operationId %q missing or duplicated", name, o.id())
		}
		seen[o.id()] = true
		if len(o.op["tags"].([]any)) != 1 {
			t.Errorf("%s: want exactly one tag (the noun)", name)
		}
		if o.method == "post" && !o.hasParam("Idempotency-Key", "header") {
			t.Errorf("%s: every POST accepts Idempotency-Key", name)
		}
		if o.method == "put" || o.method == "delete" {
			t.Errorf("%s: v1 has no %s verbs; state moves through named POST verbs", name, o.method)
		}

		responses := o.op["responses"].(node)
		def, ok := responses["default"].(node)
		if !ok || def["$ref"] != "#/components/responses/Problem" {
			t.Errorf("%s: default response must be #/components/responses/Problem", name)
		}
		for code, r := range responses {
			resp := deref(t, doc, r.(node))
			headers, _ := resp["headers"].(node)
			if _, ok := headers[landsv1.HeaderContract]; !ok {
				t.Errorf("%s %s: response lacks the %s header", name, code, landsv1.HeaderContract)
			}
			if code == "default" {
				continue
			}
			if !strings.HasPrefix(code, "2") {
				t.Errorf("%s: explicit %s response; errors go through default (problem+json)", name, code)
			}
			content, _ := resp["content"].(node)
			for ct, media := range content {
				if slices.Contains(pictureTypes, ct) {
					// A picture (getDecisionEvidence): raw bytes, never a list.
					if sch, _ := media.(node)["schema"].(node); sch["type"] != "string" || sch["format"] != "binary" {
						t.Errorf("%s %s: %s is a binary string", name, code, ct)
					}
					continue
				}
				if ct != "application/json" && ct != "text/event-stream" {
					t.Errorf("%s %s: content type %s", name, code, ct)
				}
				// A list returns a *Page and takes cursor + limit; a Page is only ever
				// returned by a list.
				ref, _ := media.(node)["schema"].(node)["$ref"].(string)
				isPage := strings.HasSuffix(ref, "Page")
				isList := strings.HasPrefix(o.id(), "list") || strings.HasPrefix(o.id(), "search")
				if isPage != isList {
					t.Errorf("%s: list/search operations (and only they) return a *Page (got %q)", name, ref)
				}
				if isList && !(o.hasParam("cursor", "query") && o.hasParam("limit", "query")) {
					t.Errorf("%s: a list takes cursor and limit", name)
				}
			}
		}
	}
}

func TestProblemIsProblemJSON(t *testing.T) {
	doc := loadSpec(t)
	content := at(t, doc, "#/components/responses/Problem")["content"].(node)
	if len(content) != 1 || content["application/problem+json"] == nil {
		t.Errorf("Problem response must be application/problem+json only, got %v", content)
	}
}

func enumOf(t *testing.T, doc node, schema string) []string {
	t.Helper()
	var out []string
	for _, v := range at(t, doc, "#/components/schemas/"+schema)["enum"].([]any) {
		out = append(out, v.(string))
	}
	return out
}

// The error and event enums are closed: changing them is a contract change
// that has to be made here too, on purpose.
func TestClosedEnums(t *testing.T) {
	doc := loadSpec(t)
	if got := enumOf(t, doc, "ProblemCode"); !reflect.DeepEqual(got, wantProblemCodes) {
		t.Errorf("ProblemCode = %v, want %v", got, wantProblemCodes)
	}
	if got := enumOf(t, doc, "EventType"); !reflect.DeepEqual(got, wantEventTypes) {
		t.Errorf("EventType = %v, want %v", got, wantEventTypes)
	}
}

func TestPagesAreItemsAndNextCursor(t *testing.T) {
	doc := loadSpec(t)
	for name := range doc["components"].(node)["schemas"].(node) {
		if !strings.HasSuffix(name, "Page") {
			continue
		}
		s := at(t, doc, "#/components/schemas/"+name)
		props := s["properties"].(node)
		if len(props) != 2 || props["items"] == nil || props["next_cursor"] == nil {
			t.Errorf("%s: a page is exactly {items, next_cursor}", name)
			continue
		}
		items := props["items"].(node)
		if items["maxItems"] != 200 {
			t.Errorf("%s.items: maxItems must be the limit cap, 200", name)
		}
		if props["next_cursor"].(node)["$ref"] != "#/components/schemas/Cursor" {
			t.Errorf("%s.next_cursor must be a Cursor", name)
		}
		if !reflect.DeepEqual(s["required"], []any{"items"}) {
			t.Errorf("%s: only items is required; next_cursor is absent on the last page", name)
		}
	}
}

// walk visits every map in the document with its JSON-pointer-ish path.
func walk(path string, v any, fn func(path string, n node)) {
	switch x := v.(type) {
	case node:
		fn(path, x)
		for k, c := range x {
			walk(path+"/"+k, c, fn)
		}
	case []any:
		for i, c := range x {
			walk(path+"/"+strconv.Itoa(i), c, fn)
		}
	}
}

func TestSchemas(t *testing.T) {
	doc := loadSpec(t)
	walk("#", doc, func(path string, n node) {
		if n["nullable"] != nil {
			t.Errorf("%s: no nullable; absent means none", path)
		}
		if ty, ok := n["type"].([]any); ok {
			t.Errorf("%s: type list %v; absent means none, never null", path, ty)
		}
		if n["type"] == "object" {
			if n["additionalProperties"] != false {
				t.Errorf("%s: every object is closed (additionalProperties: false)", path)
			}
			props, _ := n["properties"].(node)
			if len(props) > fieldCap {
				t.Errorf("%s: %d fields, cap is %d", path, len(props), fieldCap)
			}
			for f := range props {
				if slices.Contains(forbiddenFields, f) {
					t.Errorf("%s: field %q is forbidden in the contract", path, f)
				}
			}
			if req, ok := n["required"].([]any); ok {
				for _, r := range req {
					if _, ok := props[r.(string)]; !ok {
						t.Errorf("%s: required %q is not a property", path, r)
					}
				}
			}
		}
		if in, ok := n["in"].(string); ok && in != "" {
			name, _ := n["name"].(string)
			if slices.Contains(forbiddenFields, strings.ToLower(name)) {
				t.Errorf("%s: parameter %q is forbidden in the contract", path, name)
			}
		}
	})
}
