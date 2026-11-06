package main

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"

	"gardenio/controller/internal/beadsapi"

	"github.com/spf13/pflag"
)

// bundleDaemonStub is a daemon-fake harness for the bundle subcommands.
// It records all interactions so tests can assert exact API surface.
type bundleDaemonStub struct {
	mu sync.Mutex

	// beads keyed by id; each carries title/type/fields plus children list
	// for mention-by traversal.
	beads map[string]map[string]any

	// captured creates/dependencies/updates for assertion.
	creates    []map[string]any
	deps       []map[string]string
	updates    []updateCall
	closes     []string
	depRemoves []map[string]string

	// nextID is the synthetic id assigned to created beads.
	nextID int
}

type updateCall struct {
	ID     string
	Fields map[string]any
}

func newBundleStub() *bundleDaemonStub {
	return &bundleDaemonStub{
		beads: map[string]map[string]any{},
	}
}

func (s *bundleDaemonStub) seed(id string, b map[string]any) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if b["id"] == nil {
		b["id"] = id
	}
	s.beads[id] = b
}

func (s *bundleDaemonStub) handler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		path := r.URL.Path

		switch {
		case r.Method == http.MethodPost && path == "/v1/beads":
			body, _ := io.ReadAll(r.Body)
			var parsed map[string]any
			_ = json.Unmarshal(body, &parsed)
			s.mu.Lock()
			s.nextID++
			id := "gd-NEW" + itoa(s.nextID)
			parsed["id"] = id
			s.creates = append(s.creates, parsed)
			// store as a bead so subsequent gets / dep lookups can resolve.
			fieldsRaw, _ := parsed["fields"].(string)
			fields := map[string]string{}
			if fieldsRaw != "" {
				_ = json.Unmarshal([]byte(fieldsRaw), &fields)
			} else if rm, ok := parsed["fields"].(json.RawMessage); ok && len(rm) > 0 {
				_ = json.Unmarshal(rm, &fields)
			}
			s.beads[id] = map[string]any{
				"id":     id,
				"title":  parsed["title"],
				"type":   parsed["type"],
				"kind":   parsed["kind"],
				"status": "open",
				"fields": fields,
			}
			s.mu.Unlock()
			_ = json.NewEncoder(w).Encode(map[string]string{"id": id})

		case r.Method == http.MethodGet && strings.HasPrefix(path, "/v1/beads/") && strings.HasSuffix(path, "/dependents"):
			id := strings.TrimSuffix(strings.TrimPrefix(path, "/v1/beads/"), "/dependents")
			s.mu.Lock()
			var deps []map[string]string
			for _, d := range s.deps {
				if d["depends_on_id"] == id {
					deps = append(deps, map[string]string{
						"bead_id":       d["bead_id"],
						"depends_on_id": d["depends_on_id"],
						"type":          d["type"],
					})
				}
			}
			s.mu.Unlock()
			_ = json.NewEncoder(w).Encode(map[string]any{"dependencies": deps})

		case r.Method == http.MethodGet && strings.HasPrefix(path, "/v1/beads/") && strings.HasSuffix(path, "/mentioned-by"):
			id := strings.TrimSuffix(strings.TrimPrefix(path, "/v1/beads/"), "/mentioned-by")
			s.mu.Lock()
			var refs []map[string]any
			parentChild := "parent-child"
			for cid, child := range s.beads {
				if cid == id {
					continue
				}
				// Find dep where bead_id=child depends on dependsOnID=id.
				for _, d := range s.deps {
					if d["bead_id"] == cid && d["depends_on_id"] == id && d["type"] == "parent-child" {
						refs = append(refs, map[string]any{
							"id":                cid,
							"title":             child["title"],
							"type":              child["type"],
							"status":            child["status"],
							"source_field":      "deps",
							"relationship_type": &parentChild,
						})
					}
				}
			}
			s.mu.Unlock()
			_ = json.NewEncoder(w).Encode(map[string]any{"references": refs})

		// Forward deps (child → parent). `gb bundle update` reads these to find
		// the doc's owning bundle for the printed anchor URL. Must precede the
		// generic bead GET, which would otherwise swallow the path.
		case r.Method == http.MethodGet && strings.HasPrefix(path, "/v1/beads/") && strings.HasSuffix(path, "/dependencies"):
			id := strings.TrimSuffix(strings.TrimPrefix(path, "/v1/beads/"), "/dependencies")
			s.mu.Lock()
			var deps []map[string]string
			for _, d := range s.deps {
				if d["bead_id"] == id {
					deps = append(deps, map[string]string{
						"bead_id":       d["bead_id"],
						"depends_on_id": d["depends_on_id"],
						"type":          d["type"],
					})
				}
			}
			s.mu.Unlock()
			_ = json.NewEncoder(w).Encode(map[string]any{"dependencies": deps})

		case r.Method == http.MethodGet && strings.HasPrefix(path, "/v1/beads/"):
			id := strings.TrimPrefix(path, "/v1/beads/")
			s.mu.Lock()
			b, ok := s.beads[id]
			s.mu.Unlock()
			if !ok {
				http.Error(w, `{"error":"not found"}`, http.StatusNotFound)
				return
			}
			// Encode using the same JSON shape beadsapi expects.
			out := map[string]any{
				"id":     b["id"],
				"title":  b["title"],
				"type":   b["type"],
				"kind":   b["kind"],
				"status": b["status"],
				"fields": b["fields"],
			}
			_ = json.NewEncoder(w).Encode(out)

		case r.Method == http.MethodPost && strings.HasSuffix(path, "/dependencies"):
			id := strings.TrimSuffix(strings.TrimPrefix(path, "/v1/beads/"), "/dependencies")
			body, _ := io.ReadAll(r.Body)
			var parsed map[string]string
			_ = json.Unmarshal(body, &parsed)
			parsed["bead_id"] = id
			s.mu.Lock()
			s.deps = append(s.deps, parsed)
			s.mu.Unlock()
			w.WriteHeader(http.StatusOK)
			_ = json.NewEncoder(w).Encode(map[string]any{"ok": true})

		case r.Method == http.MethodDelete && strings.HasSuffix(path, "/dependencies"):
			id := strings.TrimSuffix(strings.TrimPrefix(path, "/v1/beads/"), "/dependencies")
			q := r.URL.Query()
			dependsOn := q.Get("depends_on_id")
			depType := q.Get("type")
			s.mu.Lock()
			s.depRemoves = append(s.depRemoves, map[string]string{
				"bead_id":       id,
				"depends_on_id": dependsOn,
				"type":          depType,
			})
			// Drop matching edges so a follow-up dependents read reflects the removal.
			kept := s.deps[:0]
			for _, d := range s.deps {
				match := d["bead_id"] == id && d["depends_on_id"] == dependsOn &&
					(depType == "" || d["type"] == depType)
				if !match {
					kept = append(kept, d)
				}
			}
			s.deps = kept
			s.mu.Unlock()
			w.WriteHeader(http.StatusNoContent)

		case r.Method == http.MethodPatch && strings.HasPrefix(path, "/v1/beads/"):
			id := strings.TrimPrefix(path, "/v1/beads/")
			body, _ := io.ReadAll(r.Body)
			var parsed map[string]any
			_ = json.Unmarshal(body, &parsed)
			s.mu.Lock()
			s.updates = append(s.updates, updateCall{ID: id, Fields: parsed})
			// Also fold into bead fields for follow-up reads.
			if b, ok := s.beads[id]; ok {
				existing, _ := b["fields"].(map[string]string)
				if existing == nil {
					existing = map[string]string{}
				}
				if fields, ok := parsed["fields"].(map[string]any); ok {
					for k, v := range fields {
						// RFC-7386 additive merge, as the daemon applies it: a
						// null value DELETES the key (that is how `gb bundle
						// update` clears a stale content/content_url when a doc
						// switches between inline and out-of-band).
						if v == nil {
							delete(existing, k)
							continue
						}
						if s, ok := v.(string); ok {
							existing[k] = s
						}
					}
				}
				b["fields"] = existing
				if t, ok := parsed["title"].(string); ok {
					b["title"] = t
				}
			}
			s.mu.Unlock()
			w.WriteHeader(http.StatusOK)

		case r.Method == http.MethodPost && strings.HasSuffix(path, "/close"):
			id := strings.TrimSuffix(strings.TrimPrefix(path, "/v1/beads/"), "/close")
			s.mu.Lock()
			s.closes = append(s.closes, id)
			if b, ok := s.beads[id]; ok {
				b["status"] = "closed"
			}
			s.mu.Unlock()
			w.WriteHeader(http.StatusOK)

		default:
			http.NotFound(w, r)
		}
	})
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	digits := []byte{}
	for n > 0 {
		digits = append([]byte{byte('0' + n%10)}, digits...)
		n /= 10
	}
	return string(digits)
}

func setupBundleDaemon(t *testing.T, stub *bundleDaemonStub) {
	t.Helper()
	srv := httptest.NewServer(stub.handler())
	t.Cleanup(srv.Close)
	c, err := beadsapi.New(beadsapi.Config{HTTPAddr: srv.URL})
	if err != nil {
		t.Fatal(err)
	}
	old := daemon
	daemon = c
	t.Cleanup(func() { daemon = old })
}

// resetBundleCreateFlags / Add / Order wipes flag state between subtests.
func resetBundleFlags(t *testing.T) {
	t.Helper()
	t.Cleanup(func() {
		_ = bundleCreateCmd.Flags().Set("title", "")
		_ = bundleCreateCmd.Flags().Set("kind", "report")
		_ = bundleCreateCmd.Flags().Set("headline", "")
		_ = bundleCreateCmd.Flags().Set("summary", "")
		_ = bundleAddCmd.Flags().Set("doc", "")
		_ = bundleAddCmd.Flags().Set("content-url", "")
		_ = bundleAddCmd.Flags().Set("format", "")
		_ = bundleAddCmd.Flags().Set("title", "")
		_ = bundleAddCmd.Flags().Set("headline", "")
		_ = bundleAddCmd.Flags().Set("language", "")
		_ = bundleAddCmd.Flags().Set("position", "0")
		// `update` gates --headline on Flags().Changed (an empty --headline
		// legitimately CLEARS the TOC override), so resetting the value is not
		// enough — Changed stays sticky across subtests and would leak a
		// phantom headline patch into the next one.
		_ = bundleUpdateCmd.Flags().Set("doc", "")
		_ = bundleUpdateCmd.Flags().Set("content-url", "")
		_ = bundleUpdateCmd.Flags().Set("format", "")
		_ = bundleUpdateCmd.Flags().Set("title", "")
		_ = bundleUpdateCmd.Flags().Set("headline", "")
		_ = bundleUpdateCmd.Flags().Set("language", "")
		bundleUpdateCmd.Flags().Visit(func(f *pflag.Flag) { f.Changed = false })
	})
}

func TestBundleCreate_HappyPath(t *testing.T) {
	stub := newBundleStub()
	stub.seed("gd-Origin", map[string]any{
		"title": "origin task",
		"type":  "task",
		"kind":  "issue",
	})
	setupBundleDaemon(t, stub)
	resetBundleFlags(t)

	cmd := bundleCreateCmd
	cmd.SetContext(context.Background())
	_ = cmd.Flags().Set("title", "Auth refactor deliverables")
	_ = cmd.Flags().Set("kind", "plan")

	if err := cmd.RunE(cmd, []string{"gd-Origin"}); err != nil {
		t.Fatalf("RunE: %v", err)
	}

	if len(stub.creates) != 1 {
		t.Fatalf("expected 1 create, got %d", len(stub.creates))
	}
	created := stub.creates[0]
	if got := created["type"]; got != "bundle" {
		t.Errorf("type = %v, want bundle", got)
	}
	if got := created["kind"]; got != "issue" {
		t.Errorf("kind = %v, want issue", got)
	}
	if got := created["title"]; got != "Auth refactor deliverables" {
		t.Errorf("title = %v", got)
	}

	// Verify dependency: bundle (new id) → origin parent-child.
	if len(stub.deps) != 1 {
		t.Fatalf("expected 1 dep, got %d", len(stub.deps))
	}
	dep := stub.deps[0]
	if dep["depends_on_id"] != "gd-Origin" {
		t.Errorf("dep depends_on_id = %v, want gd-Origin", dep["depends_on_id"])
	}
	if dep["type"] != "parent-child" {
		t.Errorf("dep type = %v, want parent-child", dep["type"])
	}
}

func TestBundleCreate_RequiresTitle(t *testing.T) {
	stub := newBundleStub()
	stub.seed("gd-Origin", map[string]any{"title": "origin", "type": "task"})
	setupBundleDaemon(t, stub)
	resetBundleFlags(t)

	cmd := bundleCreateCmd
	cmd.SetContext(context.Background())
	if err := cmd.RunE(cmd, []string{"gd-Origin"}); err == nil {
		t.Fatal("expected error for missing --title")
	}
	if len(stub.creates) != 0 {
		t.Errorf("create should not fire; got %d", len(stub.creates))
	}
}

func TestBundleCreate_RejectsInvalidKind(t *testing.T) {
	stub := newBundleStub()
	stub.seed("gd-Origin", map[string]any{"title": "origin", "type": "task"})
	setupBundleDaemon(t, stub)
	resetBundleFlags(t)

	cmd := bundleCreateCmd
	cmd.SetContext(context.Background())
	_ = cmd.Flags().Set("title", "x")
	_ = cmd.Flags().Set("kind", "bogus")
	err := cmd.RunE(cmd, []string{"gd-Origin"})
	if err == nil || !strings.Contains(err.Error(), "invalid --kind") {
		t.Fatalf("expected invalid-kind error; got %v", err)
	}
}

func TestBundleAdd_ExistingBead(t *testing.T) {
	stub := newBundleStub()
	stub.seed("gd-Bundle", map[string]any{
		"title": "bundle", "type": "bundle", "kind": "issue",
	})
	stub.seed("gd-Doc", map[string]any{
		"title": "doc", "type": "doc", "kind": "data",
	})
	setupBundleDaemon(t, stub)
	resetBundleFlags(t)

	cmd := bundleAddCmd
	cmd.SetContext(context.Background())
	if err := cmd.RunE(cmd, []string{"gd-Bundle", "gd-Doc"}); err != nil {
		t.Fatalf("RunE: %v", err)
	}
	if len(stub.deps) != 1 {
		t.Fatalf("expected 1 dep, got %d", len(stub.deps))
	}
	dep := stub.deps[0]
	if dep["bead_id"] != "gd-Doc" || dep["depends_on_id"] != "gd-Bundle" || dep["type"] != "parent-child" {
		t.Errorf("dep mismatch: %+v", dep)
	}
}

// TestBundleAdd_ExistingBeadWithPosition covers the link path (`gb bundle add
// <bundle> <doc> --position N`), which set fields.position as a string and was
// silently rejected by the daemon validator (warning, not error). The fix sends
// it typed; the PATCH must carry a JSON number (ka-0000005409).
func TestBundleAdd_ExistingBeadWithPosition(t *testing.T) {
	stub := newBundleStub()
	stub.seed("gd-Bundle", map[string]any{
		"title": "bundle", "type": "bundle", "kind": "issue",
	})
	stub.seed("gd-Doc", map[string]any{
		"title": "doc", "type": "doc", "kind": "data",
	})
	setupBundleDaemon(t, stub)
	resetBundleFlags(t)

	cmd := bundleAddCmd
	cmd.SetContext(context.Background())
	_ = cmd.Flags().Set("position", "7")
	if err := cmd.RunE(cmd, []string{"gd-Bundle", "gd-Doc"}); err != nil {
		t.Fatalf("RunE: %v", err)
	}
	if len(stub.updates) != 1 {
		t.Fatalf("expected 1 position update, got %d", len(stub.updates))
	}
	fields, _ := stub.updates[0].Fields["fields"].(map[string]any)
	if v, ok := fields["position"].(float64); !ok || int(v) != 7 {
		t.Errorf("position = %#v, want integer 7", fields["position"])
	}
}

func TestBundleAdd_RejectsNonBundleParent(t *testing.T) {
	stub := newBundleStub()
	stub.seed("gd-Task", map[string]any{
		"title": "task", "type": "task", "kind": "issue",
	})
	setupBundleDaemon(t, stub)
	resetBundleFlags(t)

	cmd := bundleAddCmd
	cmd.SetContext(context.Background())
	err := cmd.RunE(cmd, []string{"gd-Task", "gd-Doc"})
	if err == nil || !strings.Contains(err.Error(), "is not a bundle") {
		t.Fatalf("expected non-bundle error; got %v", err)
	}
}

func TestBundleAdd_DocFromPath(t *testing.T) {
	stub := newBundleStub()
	stub.seed("gd-Bundle", map[string]any{
		"title": "bundle", "type": "bundle", "kind": "issue",
	})
	setupBundleDaemon(t, stub)
	resetBundleFlags(t)

	dir := t.TempDir()
	docPath := filepath.Join(dir, "report.md")
	if err := os.WriteFile(docPath, []byte("# Hello\nworld"), 0o644); err != nil {
		t.Fatal(err)
	}

	cmd := bundleAddCmd
	cmd.SetContext(context.Background())
	_ = cmd.Flags().Set("doc", docPath)
	_ = cmd.Flags().Set("position", "3")

	if err := cmd.RunE(cmd, []string{"gd-Bundle"}); err != nil {
		t.Fatalf("RunE: %v", err)
	}

	if len(stub.creates) != 1 {
		t.Fatalf("expected 1 create, got %d", len(stub.creates))
	}
	created := stub.creates[0]
	if created["type"] != "doc" {
		t.Errorf("type = %v, want doc", created["type"])
	}

	// fields encoded as json.RawMessage; round-trip to inspect.
	var fieldsRaw json.RawMessage
	if rm, ok := created["fields"].(string); ok {
		fieldsRaw = json.RawMessage(rm)
	} else if rm, ok := created["fields"].(json.RawMessage); ok {
		fieldsRaw = rm
	}
	// CreateBeadRequest serializes Fields as base64? No — it's json.RawMessage,
	// which the daemon emits inline. Decode the JSON and re-extract.
	rawJSON, _ := json.Marshal(created)
	var reparsed struct {
		Fields json.RawMessage `json:"fields"`
	}
	_ = json.Unmarshal(rawJSON, &reparsed)
	if len(reparsed.Fields) > 0 {
		fieldsRaw = reparsed.Fields
	}
	var fields map[string]any
	if err := json.Unmarshal(fieldsRaw, &fields); err != nil {
		t.Fatalf("unmarshal fields: %v (raw=%s)", err, string(fieldsRaw))
	}
	if fields["format"] != "md" {
		t.Errorf("format = %v, want md", fields["format"])
	}
	if !strings.Contains(asString(fields["content"]), "Hello") {
		t.Errorf("content missing file body: %v", fields["content"])
	}
	if v, ok := fields["position"].(float64); !ok || int(v) != 3 {
		t.Errorf("position = %v, want 3", fields["position"])
	}

	// dep linking doc → bundle.
	if len(stub.deps) != 1 || stub.deps[0]["depends_on_id"] != "gd-Bundle" {
		t.Errorf("expected dep doc→bundle, got %+v", stub.deps)
	}

	// doc should have been closed.
	if len(stub.closes) != 1 {
		t.Errorf("expected 1 close, got %d", len(stub.closes))
	}
}

func TestBundleAdd_RejectsOversizeFile(t *testing.T) {
	stub := newBundleStub()
	stub.seed("gd-Bundle", map[string]any{
		"title": "bundle", "type": "bundle", "kind": "issue",
	})
	setupBundleDaemon(t, stub)
	resetBundleFlags(t)

	dir := t.TempDir()
	big := filepath.Join(dir, "huge.md")
	if err := os.WriteFile(big, make([]byte, inlineDocCap+1), 0o644); err != nil {
		t.Fatal(err)
	}

	cmd := bundleAddCmd
	cmd.SetContext(context.Background())
	_ = cmd.Flags().Set("doc", big)

	err := cmd.RunE(cmd, []string{"gd-Bundle"})
	if err == nil || !strings.Contains(err.Error(), "256 KiB") {
		t.Fatalf("expected size-cap error; got %v", err)
	}
}

func TestBundleAdd_RejectsInvalidFormat(t *testing.T) {
	stub := newBundleStub()
	stub.seed("gd-Bundle", map[string]any{
		"title": "bundle", "type": "bundle", "kind": "issue",
	})
	setupBundleDaemon(t, stub)
	resetBundleFlags(t)

	cmd := bundleAddCmd
	cmd.SetContext(context.Background())
	_ = cmd.Flags().Set("content-url", "https://example.test/x.bin")
	_ = cmd.Flags().Set("format", "exe")

	err := cmd.RunE(cmd, []string{"gd-Bundle"})
	if err == nil || !strings.Contains(err.Error(), "invalid --format") {
		t.Fatalf("expected invalid-format error; got %v", err)
	}
}

// seedBundleWithDoc seeds a bundle plus one inline md doc linked under it,
// which is the starting state every `gb bundle update` test needs.
func seedBundleWithDoc(t *testing.T, stub *bundleDaemonStub, content string) {
	t.Helper()
	stub.seed("gd-Bundle", map[string]any{
		"title": "bundle", "type": "bundle", "kind": "issue",
		"fields": map[string]string{"kind": "plan"},
	})
	stub.seed("gd-Doc", map[string]any{
		"title": "plan.md", "type": "doc", "kind": "data",
		"fields": map[string]string{
			"format": "md", "content": content, "position": "1",
		},
	})
	stub.deps = append(stub.deps, map[string]string{
		"bead_id": "gd-Doc", "depends_on_id": "gd-Bundle", "type": "parent-child",
	})
}

// storedField reads one field off a seeded bead as the stub's PATCH handler
// left it, so a test can assert the POST-merge state rather than only the
// patch that was sent. ok is false when the key was deleted (RFC-7386 nil).
func storedField(t *testing.T, stub *bundleDaemonStub, beadID, key string) (string, bool) {
	t.Helper()
	bead, ok := stub.beads[beadID]
	if !ok {
		t.Fatalf("bead %s not in stub", beadID)
	}
	fields, _ := bead["fields"].(map[string]string)
	v, present := fields[key]
	return v, present
}

// TestBundleUpdate_RewritesBodyInPlace is the core converge guarantee
// (ka-0000003568): `update --doc` PATCHes fields.content on the EXISTING doc
// bead. It must create no bead and add no dep — the fork that `add --doc`
// produces is precisely the duplicate-doc bug this verb exists to avoid.
func TestBundleUpdate_RewritesBodyInPlace(t *testing.T) {
	stub := newBundleStub()
	seedBundleWithDoc(t, stub, "# Old\nstale body")
	setupBundleDaemon(t, stub)
	resetBundleFlags(t)

	dir := t.TempDir()
	newPath := filepath.Join(dir, "plan.md")
	if err := os.WriteFile(newPath, []byte("# New\nconverged body"), 0o644); err != nil {
		t.Fatal(err)
	}

	cmd := bundleUpdateCmd
	cmd.SetContext(context.Background())
	_ = cmd.Flags().Set("doc", newPath)

	if err := cmd.RunE(cmd, []string{"gd-Doc"}); err != nil {
		t.Fatalf("RunE: %v", err)
	}

	// No fork: nothing created, no new bundle edge.
	if len(stub.creates) != 0 {
		t.Errorf("update must not create a bead; got %d creates: %+v", len(stub.creates), stub.creates)
	}
	if len(stub.deps) != 1 {
		t.Errorf("update must not add a dep; deps = %+v", stub.deps)
	}
	if len(stub.closes) != 0 {
		t.Errorf("update must not close the doc; closes = %v", stub.closes)
	}

	// The body was replaced on the SAME doc bead.
	if len(stub.updates) != 1 {
		t.Fatalf("expected 1 PATCH, got %d: %+v", len(stub.updates), stub.updates)
	}
	if stub.updates[0].ID != "gd-Doc" {
		t.Errorf("PATCH target = %s, want gd-Doc", stub.updates[0].ID)
	}
	fields, _ := stub.updates[0].Fields["fields"].(map[string]any)
	if got := asString(fields["content"]); !strings.Contains(got, "converged body") {
		t.Errorf("patched content = %q, want the new file body", got)
	}
	// position must survive untouched — an in-place edit keeps the TOC slot.
	if _, patched := fields["position"]; patched {
		t.Errorf("update must not rewrite position; patch = %+v", fields)
	}
	if got, _ := storedField(t, stub, "gd-Doc", "position"); got != "1" {
		t.Errorf("stored position = %q, want 1 (unchanged)", got)
	}
}

// TestBundleUpdate_KeepsBundleTOCStable is the observable end-state assertion
// from the work order: after an update, `gb bundle show` lists the SAME single
// doc — no second TOC entry.
func TestBundleUpdate_KeepsBundleTOCStable(t *testing.T) {
	stub := newBundleStub()
	seedBundleWithDoc(t, stub, "old")
	setupBundleDaemon(t, stub)
	resetBundleFlags(t)

	dir := t.TempDir()
	newPath := filepath.Join(dir, "plan.md")
	if err := os.WriteFile(newPath, []byte("brand new body"), 0o644); err != nil {
		t.Fatal(err)
	}

	upd := bundleUpdateCmd
	upd.SetContext(context.Background())
	_ = upd.Flags().Set("doc", newPath)
	if err := upd.RunE(upd, []string{"gd-Doc"}); err != nil {
		t.Fatalf("update RunE: %v", err)
	}

	out := string(captureStdout(t, func() error {
		show := bundleShowCmd
		show.SetContext(context.Background())
		return show.RunE(show, []string{"gd-Bundle"})
	}))
	if !strings.Contains(out, "Docs (1):") {
		t.Errorf("bundle should still hold exactly 1 doc after update; got:\n%s", out)
	}

	// And the new body is what `gb bundle doc` reads back.
	body := string(captureStdout(t, func() error {
		doc := bundleDocCmd
		doc.SetContext(context.Background())
		return doc.RunE(doc, []string{"gd-Doc"})
	}))
	if !strings.Contains(body, "brand new body") {
		t.Errorf("gb bundle doc should read back the new body; got %q", body)
	}
}

// TestBundleUpdate_IdenticalBodyIsNoOp keeps a per-cycle converge loop from
// spamming the activity feed with no-op PATCH events.
func TestBundleUpdate_IdenticalBodyIsNoOp(t *testing.T) {
	stub := newBundleStub()
	seedBundleWithDoc(t, stub, "same body")
	setupBundleDaemon(t, stub)
	resetBundleFlags(t)

	dir := t.TempDir()
	samePath := filepath.Join(dir, "plan.md")
	if err := os.WriteFile(samePath, []byte("same body"), 0o644); err != nil {
		t.Fatal(err)
	}

	cmd := bundleUpdateCmd
	cmd.SetContext(context.Background())
	_ = cmd.Flags().Set("doc", samePath)

	out := string(captureStdout(t, func() error { return cmd.RunE(cmd, []string{"gd-Doc"}) }))
	if !strings.Contains(out, "already matches") {
		t.Errorf("identical body should report unchanged; got:\n%s", out)
	}
	// UpdateBeadFieldsTyped skips the wire PATCH when every key already holds
	// the target value, so no update is recorded at all.
	if len(stub.updates) != 0 {
		t.Errorf("no-op update must not PATCH; got %+v", stub.updates)
	}
}

// TestBundleUpdate_RejectsBundleID: pointing the verb at the bundle instead of
// a doc must fail loudly, not stamp a `content` field onto the bundle bead.
func TestBundleUpdate_RejectsBundleID(t *testing.T) {
	stub := newBundleStub()
	seedBundleWithDoc(t, stub, "body")
	setupBundleDaemon(t, stub)
	resetBundleFlags(t)

	dir := t.TempDir()
	p := filepath.Join(dir, "plan.md")
	if err := os.WriteFile(p, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}

	cmd := bundleUpdateCmd
	cmd.SetContext(context.Background())
	_ = cmd.Flags().Set("doc", p)

	err := cmd.RunE(cmd, []string{"gd-Bundle"})
	if err == nil || !strings.Contains(err.Error(), "is a bundle, not a doc") {
		t.Fatalf("expected bundle-id error; got %v", err)
	}
	if len(stub.updates) != 0 {
		t.Errorf("rejected update must not PATCH; got %+v", stub.updates)
	}
}

func TestBundleUpdate_RejectsOversizeFile(t *testing.T) {
	stub := newBundleStub()
	seedBundleWithDoc(t, stub, "body")
	setupBundleDaemon(t, stub)
	resetBundleFlags(t)

	dir := t.TempDir()
	big := filepath.Join(dir, "huge.md")
	if err := os.WriteFile(big, make([]byte, inlineDocCap+1), 0o644); err != nil {
		t.Fatal(err)
	}

	cmd := bundleUpdateCmd
	cmd.SetContext(context.Background())
	_ = cmd.Flags().Set("doc", big)

	err := cmd.RunE(cmd, []string{"gd-Doc"})
	if err == nil || !strings.Contains(err.Error(), "256 KiB") {
		t.Fatalf("expected size-cap error; got %v", err)
	}
	if len(stub.updates) != 0 {
		t.Errorf("over-cap update must not PATCH; got %+v", stub.updates)
	}
}

// TestBundleUpdate_SwitchToContentURLClearsInline: a doc must never carry both
// a stale inline body and a new URL, or the viewer renders the old content.
func TestBundleUpdate_SwitchToContentURLClearsInline(t *testing.T) {
	stub := newBundleStub()
	seedBundleWithDoc(t, stub, "stale inline body")
	setupBundleDaemon(t, stub)
	resetBundleFlags(t)

	cmd := bundleUpdateCmd
	cmd.SetContext(context.Background())
	_ = cmd.Flags().Set("content-url", "https://example.test/big.md")

	if err := cmd.RunE(cmd, []string{"gd-Doc"}); err != nil {
		t.Fatalf("RunE: %v", err)
	}
	if len(stub.updates) != 1 {
		t.Fatalf("expected 1 PATCH, got %+v", stub.updates)
	}
	fields, _ := stub.updates[0].Fields["fields"].(map[string]any)
	if got := asString(fields["content_url"]); got != "https://example.test/big.md" {
		t.Errorf("content_url = %q", got)
	}
	// nil is the RFC-7386 delete — the only way to return the key to absent.
	v, present := fields["content"]
	if !present || v != nil {
		t.Errorf("expected content patched to nil (delete); got present=%v value=%v", present, v)
	}
	if got, present := storedField(t, stub, "gd-Doc", "content"); present {
		t.Errorf("stale inline content survived the switch: %q", got)
	}
}

// TestBundleUpdate_SwitchToInlineClearsContentURL is the mirror of the case
// above: promoting an out-of-band doc back to an inline body must drop the
// stale content_url. Without the clear, a doc carries BOTH — and the viewer's
// doc card populates Content and ContentURL from whatever is stored, so the
// old URL keeps rendering alongside the new body.
func TestBundleUpdate_SwitchToInlineClearsContentURL(t *testing.T) {
	stub := newBundleStub()
	stub.seed("gd-Bundle", map[string]any{
		"title": "bundle", "type": "bundle", "kind": "issue",
		"fields": map[string]string{"kind": "plan"},
	})
	stub.seed("gd-Doc", map[string]any{
		"title": "big.md", "type": "doc", "kind": "data",
		"fields": map[string]string{
			"format": "md", "content_url": "https://example.test/stale.md",
		},
	})
	stub.deps = append(stub.deps, map[string]string{
		"bead_id": "gd-Doc", "depends_on_id": "gd-Bundle", "type": "parent-child",
	})
	setupBundleDaemon(t, stub)
	resetBundleFlags(t)

	dir := t.TempDir()
	p := filepath.Join(dir, "big.md")
	if err := os.WriteFile(p, []byte("now inline"), 0o644); err != nil {
		t.Fatal(err)
	}

	cmd := bundleUpdateCmd
	cmd.SetContext(context.Background())
	_ = cmd.Flags().Set("doc", p)

	if err := cmd.RunE(cmd, []string{"gd-Doc"}); err != nil {
		t.Fatalf("RunE: %v", err)
	}
	if len(stub.updates) != 1 {
		t.Fatalf("expected 1 PATCH, got %+v", stub.updates)
	}
	fields, _ := stub.updates[0].Fields["fields"].(map[string]any)
	if got := asString(fields["content"]); got != "now inline" {
		t.Errorf("content = %q, want the new inline body", got)
	}
	v, present := fields["content_url"]
	if !present || v != nil {
		t.Errorf("expected content_url patched to nil (delete); got present=%v value=%v", present, v)
	}
	if got, present := storedField(t, stub, "gd-Doc", "content_url"); present {
		t.Errorf("stale content_url survived the switch to inline: %q", got)
	}
}

// TestBundleUpdate_HeadlineOnly: the TOC line can be reworded without touching
// the body, and an explicit empty --headline clears the override.
func TestBundleUpdate_HeadlineOnly(t *testing.T) {
	stub := newBundleStub()
	seedBundleWithDoc(t, stub, "untouched body")
	setupBundleDaemon(t, stub)
	resetBundleFlags(t)

	cmd := bundleUpdateCmd
	cmd.SetContext(context.Background())
	_ = cmd.Flags().Set("headline", "Status: phase 3")

	if err := cmd.RunE(cmd, []string{"gd-Doc"}); err != nil {
		t.Fatalf("RunE: %v", err)
	}
	if len(stub.updates) != 1 {
		t.Fatalf("expected 1 PATCH, got %+v", stub.updates)
	}
	fields, _ := stub.updates[0].Fields["fields"].(map[string]any)
	if got := asString(fields["headline"]); got != "Status: phase 3" {
		t.Errorf("headline = %q", got)
	}
	if _, patched := fields["content"]; patched {
		t.Errorf("headline-only update must not touch content; patch = %+v", fields)
	}
}

// TestBundleUpdate_EmptyHeadlineClearsIt: an explicit `--headline ""` is a
// CLEAR, not a no-op. This is why the flag is gated on Flags().Changed rather
// than on a non-empty value — gating on non-empty would make the override
// impossible to remove once set (the viewer then falls back to the doc title).
func TestBundleUpdate_EmptyHeadlineClearsIt(t *testing.T) {
	stub := newBundleStub()
	stub.seed("gd-Bundle", map[string]any{
		"title": "bundle", "type": "bundle", "kind": "issue",
		"fields": map[string]string{"kind": "plan"},
	})
	stub.seed("gd-Doc", map[string]any{
		"title": "plan.md", "type": "doc", "kind": "data",
		"fields": map[string]string{
			"format": "md", "content": "body", "headline": "stale headline",
		},
	})
	setupBundleDaemon(t, stub)
	resetBundleFlags(t)

	cmd := bundleUpdateCmd
	cmd.SetContext(context.Background())
	_ = cmd.Flags().Set("headline", "")

	if err := cmd.RunE(cmd, []string{"gd-Doc"}); err != nil {
		t.Fatalf("RunE: %v", err)
	}
	if len(stub.updates) != 1 {
		t.Fatalf("empty --headline must still PATCH (it clears the override); got %+v", stub.updates)
	}
	fields, _ := stub.updates[0].Fields["fields"].(map[string]any)
	if got := asString(fields["headline"]); got != "" {
		t.Errorf("headline = %q, want cleared", got)
	}
	if got, _ := storedField(t, stub, "gd-Doc", "headline"); got != "" {
		t.Errorf("stored headline = %q, want cleared", got)
	}
}

// TestBundleUpdate_RewritesTitle covers --title, which is a bead COLUMN (not a
// field), so it travels on a separate UpdateBead PATCH from the fields patch.
func TestBundleUpdate_RewritesTitle(t *testing.T) {
	stub := newBundleStub()
	seedBundleWithDoc(t, stub, "body")
	setupBundleDaemon(t, stub)
	resetBundleFlags(t)

	cmd := bundleUpdateCmd
	cmd.SetContext(context.Background())
	_ = cmd.Flags().Set("title", "Converged plan of record")

	out := string(captureStdout(t, func() error { return cmd.RunE(cmd, []string{"gd-Doc"}) }))
	if !strings.Contains(out, "updated in place") {
		t.Errorf("title rewrite should report a change; got:\n%s", out)
	}

	var sawTitle bool
	for _, u := range stub.updates {
		if u.ID == "gd-Doc" && u.Fields["title"] == "Converged plan of record" {
			sawTitle = true
		}
	}
	if !sawTitle {
		t.Fatalf("expected a title PATCH on gd-Doc; got %+v", stub.updates)
	}
	if got := stub.beads["gd-Doc"]["title"]; got != "Converged plan of record" {
		t.Errorf("stored title = %v, want the new title", got)
	}
}

// TestBundleUpdate_SameTitleIsNoOp: re-pushing the title a doc already has must
// not manufacture a PATCH, so a converge loop stays quiet.
func TestBundleUpdate_SameTitleIsNoOp(t *testing.T) {
	stub := newBundleStub()
	seedBundleWithDoc(t, stub, "body")
	setupBundleDaemon(t, stub)
	resetBundleFlags(t)

	cmd := bundleUpdateCmd
	cmd.SetContext(context.Background())
	_ = cmd.Flags().Set("title", "plan.md") // already the seeded title

	out := string(captureStdout(t, func() error { return cmd.RunE(cmd, []string{"gd-Doc"}) }))
	if !strings.Contains(out, "already matches") {
		t.Errorf("identical title should report unchanged; got:\n%s", out)
	}
	if len(stub.updates) != 0 {
		t.Errorf("no-op title update must not PATCH; got %+v", stub.updates)
	}
}

// TestBundleUpdate_RequiresSomethingToDo: a bare `gb bundle update <id>` is a
// usage error, not a silent success that reads as "converged".
func TestBundleUpdate_RequiresSomethingToDo(t *testing.T) {
	stub := newBundleStub()
	seedBundleWithDoc(t, stub, "body")
	setupBundleDaemon(t, stub)
	resetBundleFlags(t)

	cmd := bundleUpdateCmd
	cmd.SetContext(context.Background())

	err := cmd.RunE(cmd, []string{"gd-Doc"})
	if err == nil || !strings.Contains(err.Error(), "nothing to update") {
		t.Fatalf("expected nothing-to-update error; got %v", err)
	}
}

func TestBundleUpdate_RejectsDocAndContentURLTogether(t *testing.T) {
	stub := newBundleStub()
	seedBundleWithDoc(t, stub, "body")
	setupBundleDaemon(t, stub)
	resetBundleFlags(t)

	dir := t.TempDir()
	p := filepath.Join(dir, "plan.md")
	if err := os.WriteFile(p, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}

	cmd := bundleUpdateCmd
	cmd.SetContext(context.Background())
	_ = cmd.Flags().Set("doc", p)
	_ = cmd.Flags().Set("content-url", "https://example.test/x")

	err := cmd.RunE(cmd, []string{"gd-Doc"})
	if err == nil || !strings.Contains(err.Error(), "not both") {
		t.Fatalf("expected mutually-exclusive error; got %v", err)
	}
}

// TestBundleUpdate_KeepsFormatByDefault: a differently-suffixed file must not
// silently reclassify a doc's format (the flag help promises "keep existing").
func TestBundleUpdate_KeepsFormatByDefault(t *testing.T) {
	stub := newBundleStub()
	seedBundleWithDoc(t, stub, "old")
	setupBundleDaemon(t, stub)
	resetBundleFlags(t)

	dir := t.TempDir()
	// .txt infers NO format; .go would infer `code`. Either way, the stored
	// `md` must survive an update that does not pass --format.
	p := filepath.Join(dir, "plan.go")
	if err := os.WriteFile(p, []byte("package main"), 0o644); err != nil {
		t.Fatal(err)
	}

	cmd := bundleUpdateCmd
	cmd.SetContext(context.Background())
	_ = cmd.Flags().Set("doc", p)

	out := string(captureStdout(t, func() error { return cmd.RunE(cmd, []string{"gd-Doc"}) }))

	fields, _ := stub.updates[0].Fields["fields"].(map[string]any)
	if _, patched := fields["format"]; patched {
		t.Errorf("update must not reclassify format without --format; patch = %+v", fields)
	}
	if got, _ := storedField(t, stub, "gd-Doc", "format"); got != "md" {
		t.Errorf("stored format = %q, want md (unchanged)", got)
	}
	// The REPORTED format must also be the stored one, not one re-inferred from
	// the new file's extension — a .go file must not make the CLI claim
	// "format=code" while the bead still says md.
	if !strings.Contains(out, "format=md") {
		t.Errorf("output should report the doc's real format (md), not one inferred from the .go path; got:\n%s", out)
	}
}

func TestBundleUpdate_RejectsInvalidFormat(t *testing.T) {
	stub := newBundleStub()
	seedBundleWithDoc(t, stub, "body")
	setupBundleDaemon(t, stub)
	resetBundleFlags(t)

	cmd := bundleUpdateCmd
	cmd.SetContext(context.Background())
	_ = cmd.Flags().Set("format", "exe")

	err := cmd.RunE(cmd, []string{"gd-Doc"})
	if err == nil || !strings.Contains(err.Error(), "invalid --format") {
		t.Fatalf("expected invalid-format error; got %v", err)
	}
}

// TestBundleUpdate_JSONShape pins the machine-readable contract a gardener
// branches on, including the resolved bundle anchor URL.
func TestBundleUpdate_JSONShape(t *testing.T) {
	stub := newBundleStub()
	seedBundleWithDoc(t, stub, "old")
	setupBundleDaemon(t, stub)
	resetBundleFlags(t)

	dir := t.TempDir()
	p := filepath.Join(dir, "plan.md")
	if err := os.WriteFile(p, []byte("new"), 0o644); err != nil {
		t.Fatal(err)
	}

	oldJSON := jsonOutput
	jsonOutput = true
	t.Cleanup(func() { jsonOutput = oldJSON })

	cmd := bundleUpdateCmd
	cmd.SetContext(context.Background())
	_ = cmd.Flags().Set("doc", p)

	out := captureStdout(t, func() error { return cmd.RunE(cmd, []string{"gd-Doc"}) })

	var parsed map[string]any
	if err := json.Unmarshal(out, &parsed); err != nil {
		t.Fatalf("unmarshal %q: %v", out, err)
	}
	if parsed["id"] != "gd-Doc" {
		t.Errorf("id = %v", parsed["id"])
	}
	if parsed["bundle"] != "gd-Bundle" {
		t.Errorf("bundle = %v, want the resolved owning bundle", parsed["bundle"])
	}
	if parsed["format"] != "md" {
		t.Errorf("format = %v, want the doc's existing md", parsed["format"])
	}
	if parsed["unchanged"] != false {
		t.Errorf("unchanged = %v, want false", parsed["unchanged"])
	}
	if u, _ := parsed["url"].(string); !strings.Contains(u, "/bundle/gd-Bundle#doc-gd-Doc") {
		t.Errorf("url = %v, want the doc anchor", parsed["url"])
	}
}

// TestBundleUpdate_LooseDocIsNotAnError: editing a doc with no bundle parent is
// harmless (the work order says do not hard-require membership) — it just omits
// the viewer URL rather than failing.
func TestBundleUpdate_LooseDocIsNotAnError(t *testing.T) {
	stub := newBundleStub()
	stub.seed("gd-Loose", map[string]any{
		"title": "loose.md", "type": "doc", "kind": "data",
		"fields": map[string]string{"format": "md", "content": "old"},
	})
	setupBundleDaemon(t, stub)
	resetBundleFlags(t)

	dir := t.TempDir()
	p := filepath.Join(dir, "loose.md")
	if err := os.WriteFile(p, []byte("new body"), 0o644); err != nil {
		t.Fatal(err)
	}

	cmd := bundleUpdateCmd
	cmd.SetContext(context.Background())
	_ = cmd.Flags().Set("doc", p)

	out := string(captureStdout(t, func() error { return cmd.RunE(cmd, []string{"gd-Loose"}) }))
	if !strings.Contains(out, "updated in place") {
		t.Errorf("loose doc edit should succeed; got:\n%s", out)
	}
	if strings.Contains(out, "/bundle/") {
		t.Errorf("loose doc has no bundle — should not print a bundle URL; got:\n%s", out)
	}
}

// TestBundleRemove_DetachesDoc covers the default detach-only path: the doc →
// bundle parent-child edge is deleted (so it leaves the TOC) but the doc bead
// is NOT closed (ka-0000004867).
func TestBundleRemove_DetachesDoc(t *testing.T) {
	stub := newBundleStub()
	stub.seed("gd-Bundle", map[string]any{
		"title": "bundle", "type": "bundle", "kind": "issue",
	})
	stub.seed("gd-Doc1", map[string]any{
		"title": "survivor", "type": "doc", "kind": "data",
	})
	stub.seed("gd-Doc2", map[string]any{
		"title": "stale", "type": "doc", "kind": "data",
	})
	stub.deps = []map[string]string{
		{"bead_id": "gd-Doc1", "depends_on_id": "gd-Bundle", "type": "parent-child"},
		{"bead_id": "gd-Doc2", "depends_on_id": "gd-Bundle", "type": "parent-child"},
	}
	setupBundleDaemon(t, stub)
	resetBundleFlags(t)

	cmd := bundleRemoveCmd
	cmd.SetContext(context.Background())
	if err := cmd.RunE(cmd, []string{"gd-Bundle", "gd-Doc2"}); err != nil {
		t.Fatalf("RunE: %v", err)
	}

	if len(stub.depRemoves) != 1 {
		t.Fatalf("expected 1 dep removal, got %d", len(stub.depRemoves))
	}
	rm := stub.depRemoves[0]
	if rm["bead_id"] != "gd-Doc2" || rm["depends_on_id"] != "gd-Bundle" || rm["type"] != "parent-child" {
		t.Errorf("removal mismatch: %+v", rm)
	}
	// Detach-only default: doc bead must NOT be closed.
	if len(stub.closes) != 0 {
		t.Errorf("expected no close (detach-only default), got %v", stub.closes)
	}
	// Only the survivor edge remains.
	if len(stub.deps) != 1 || stub.deps[0]["bead_id"] != "gd-Doc1" {
		t.Errorf("expected only gd-Doc1 to remain, got %+v", stub.deps)
	}
}

// TestBundleRemove_WithClose covers --close: detach AND close the doc bead.
func TestBundleRemove_WithClose(t *testing.T) {
	stub := newBundleStub()
	stub.seed("gd-Bundle", map[string]any{
		"title": "bundle", "type": "bundle", "kind": "issue",
	})
	stub.seed("gd-Doc", map[string]any{
		"title": "stale", "type": "doc", "kind": "data",
	})
	stub.deps = []map[string]string{
		{"bead_id": "gd-Doc", "depends_on_id": "gd-Bundle", "type": "parent-child"},
	}
	setupBundleDaemon(t, stub)
	resetBundleFlags(t)

	cmd := bundleRemoveCmd
	cmd.SetContext(context.Background())
	_ = cmd.Flags().Set("close", "true")
	if err := cmd.RunE(cmd, []string{"gd-Bundle", "gd-Doc"}); err != nil {
		t.Fatalf("RunE: %v", err)
	}

	if len(stub.depRemoves) != 1 {
		t.Fatalf("expected 1 dep removal, got %d", len(stub.depRemoves))
	}
	if len(stub.closes) != 1 || stub.closes[0] != "gd-Doc" {
		t.Errorf("expected doc close, got %v", stub.closes)
	}
}

// TestBundleRemove_RejectsUnlinkedDoc: removing a doc that is not a member of
// the bundle reports a clear error rather than a silent no-op.
func TestBundleRemove_RejectsUnlinkedDoc(t *testing.T) {
	stub := newBundleStub()
	stub.seed("gd-Bundle", map[string]any{
		"title": "bundle", "type": "bundle", "kind": "issue",
	})
	stub.seed("gd-Other", map[string]any{
		"title": "unrelated", "type": "doc", "kind": "data",
	})
	setupBundleDaemon(t, stub)
	resetBundleFlags(t)

	cmd := bundleRemoveCmd
	cmd.SetContext(context.Background())
	err := cmd.RunE(cmd, []string{"gd-Bundle", "gd-Other"})
	if err == nil || !strings.Contains(err.Error(), "not a member") {
		t.Fatalf("expected not-a-member error; got %v", err)
	}
	if len(stub.depRemoves) != 0 {
		t.Errorf("no removal should fire for unlinked doc; got %d", len(stub.depRemoves))
	}
}

// TestBundleRemove_RejectsNonBundle: a non-bundle parent id fails clearly.
func TestBundleRemove_RejectsNonBundle(t *testing.T) {
	stub := newBundleStub()
	stub.seed("gd-Task", map[string]any{
		"title": "task", "type": "task", "kind": "issue",
	})
	setupBundleDaemon(t, stub)
	resetBundleFlags(t)

	cmd := bundleRemoveCmd
	cmd.SetContext(context.Background())
	err := cmd.RunE(cmd, []string{"gd-Task", "gd-Doc"})
	if err == nil || !strings.Contains(err.Error(), "is not a bundle") {
		t.Fatalf("expected non-bundle error; got %v", err)
	}
}

func TestBundleOrder_SetsPosition(t *testing.T) {
	stub := newBundleStub()
	stub.seed("gd-Bundle", map[string]any{
		"title": "bundle", "type": "bundle", "kind": "issue",
	})
	stub.seed("gd-Doc", map[string]any{
		"title": "doc", "type": "doc", "kind": "data",
	})
	setupBundleDaemon(t, stub)
	resetBundleFlags(t)

	cmd := bundleOrderCmd
	cmd.SetContext(context.Background())
	if err := cmd.RunE(cmd, []string{"gd-Bundle", "gd-Doc", "5"}); err != nil {
		t.Fatalf("RunE: %v", err)
	}
	if len(stub.updates) != 1 {
		t.Fatalf("expected 1 update, got %d", len(stub.updates))
	}
	upd := stub.updates[0]
	if upd.ID != "gd-Doc" {
		t.Errorf("update id = %v, want gd-Doc", upd.ID)
	}
	fields, _ := upd.Fields["fields"].(map[string]any)
	// position is an integer-typed schema field: the PATCH body must carry a
	// JSON number, which round-trips through map[string]any as float64. A string
	// "5" would be rejected by the daemon validator with HTTP 400
	// "position: must be an integer" (ka-0000005409).
	if v, ok := fields["position"].(float64); !ok || int(v) != 5 {
		t.Errorf("position = %#v, want integer 5", fields["position"])
	}
}

func TestBundleOrder_RejectsNonInt(t *testing.T) {
	stub := newBundleStub()
	stub.seed("gd-Bundle", map[string]any{
		"title": "bundle", "type": "bundle", "kind": "issue",
	})
	setupBundleDaemon(t, stub)
	resetBundleFlags(t)

	cmd := bundleOrderCmd
	cmd.SetContext(context.Background())
	err := cmd.RunE(cmd, []string{"gd-Bundle", "gd-Doc", "abc"})
	if err == nil || !strings.Contains(err.Error(), "must be an integer") {
		t.Fatalf("expected integer-parse error; got %v", err)
	}
}

// TestBundleLocate_FindsBundleForOrigin: the reverse lookup — given the origin
// bead, one call yields the bundle id + viewer URL (gd-hiI1VbSHVs). Non-bundle
// children of the same origin must not be reported as bundles.
func TestBundleLocate_FindsBundleForOrigin(t *testing.T) {
	stub := newBundleStub()
	stub.seed("gd-Origin", map[string]any{
		"title": "the plan epic", "type": "epic", "kind": "issue",
	})
	stub.seed("gd-Bundle", map[string]any{
		"title": "deliverables", "type": "bundle", "kind": "issue",
		"fields": map[string]string{"kind": "plan", "headline": "plan of record"},
	})
	// A sibling child that is NOT a bundle — must be filtered out.
	stub.seed("gd-Child", map[string]any{
		"title": "a step task", "type": "task", "kind": "issue",
	})
	stub.deps = []map[string]string{
		{"bead_id": "gd-Bundle", "depends_on_id": "gd-Origin", "type": "parent-child"},
		{"bead_id": "gd-Child", "depends_on_id": "gd-Origin", "type": "parent-child"},
	}
	setupBundleDaemon(t, stub)
	resetBundleFlags(t)

	out := string(captureStdout(t, func() error {
		cmd := bundleLocateCmd
		cmd.SetContext(context.Background())
		return cmd.RunE(cmd, []string{"gd-Origin"})
	}))

	if !strings.Contains(out, "gd-Bundle") {
		t.Errorf("expected bundle id in output, got %q", out)
	}
	if !strings.Contains(out, "/bundle/gd-Bundle") {
		t.Errorf("expected viewer URL in output, got %q", out)
	}
	if strings.Contains(out, "gd-Child") {
		t.Errorf("non-bundle child leaked into output: %q", out)
	}
	if strings.Contains(out, "no bundle attached") {
		t.Errorf("reported a negative despite an attached bundle: %q", out)
	}
}

// TestBundleLocate_JSONShape asserts the machine-readable contract a gardener
// pipes into jq: {"origin","bundles":[{"id","url",...}]}.
func TestBundleLocate_JSONShape(t *testing.T) {
	stub := newBundleStub()
	stub.seed("gd-Origin", map[string]any{
		"title": "the plan epic", "type": "epic", "kind": "issue",
	})
	stub.seed("gd-Bundle", map[string]any{
		"title": "deliverables", "type": "bundle", "kind": "issue",
		"fields": map[string]string{"kind": "plan"},
	})
	stub.deps = []map[string]string{
		{"bead_id": "gd-Bundle", "depends_on_id": "gd-Origin", "type": "parent-child"},
	}
	setupBundleDaemon(t, stub)
	resetBundleFlags(t)

	oldJSON := jsonOutput
	jsonOutput = true
	t.Cleanup(func() { jsonOutput = oldJSON })

	out := captureStdout(t, func() error {
		cmd := bundleLocateCmd
		cmd.SetContext(context.Background())
		return cmd.RunE(cmd, []string{"gd-Origin"})
	})

	var got struct {
		Origin  string `json:"origin"`
		Bundles []struct {
			ID   string `json:"id"`
			URL  string `json:"url"`
			Kind string `json:"kind"`
		} `json:"bundles"`
	}
	if err := json.Unmarshal(out, &got); err != nil {
		t.Fatalf("unmarshal JSON output %q: %v", out, err)
	}
	if got.Origin != "gd-Origin" {
		t.Errorf("origin = %q, want gd-Origin", got.Origin)
	}
	if len(got.Bundles) != 1 {
		t.Fatalf("expected 1 bundle, got %d (%q)", len(got.Bundles), out)
	}
	if got.Bundles[0].ID != "gd-Bundle" {
		t.Errorf("bundle id = %q, want gd-Bundle", got.Bundles[0].ID)
	}
	if !strings.HasSuffix(got.Bundles[0].URL, "/bundle/gd-Bundle") {
		t.Errorf("bundle url = %q, want .../bundle/gd-Bundle", got.Bundles[0].URL)
	}
	if got.Bundles[0].Kind != "plan" {
		t.Errorf("bundle kind = %q, want plan", got.Bundles[0].Kind)
	}
}

// TestBundleLocate_NoBundleIsAuthoritativeNegative: an origin with no bundle is
// NOT an error — it prints "no bundle attached" and exits 0 so a gardener can
// branch on it cleanly.
func TestBundleLocate_NoBundleIsAuthoritativeNegative(t *testing.T) {
	stub := newBundleStub()
	stub.seed("gd-Origin", map[string]any{
		"title": "bundle-less plan", "type": "epic", "kind": "issue",
	})
	setupBundleDaemon(t, stub)
	resetBundleFlags(t)

	out := string(captureStdout(t, func() error {
		cmd := bundleLocateCmd
		cmd.SetContext(context.Background())
		// A nil error here IS the assertion: exit 0 on the negative.
		return cmd.RunE(cmd, []string{"gd-Origin"})
	}))

	if !strings.Contains(out, "no bundle attached to gd-Origin") {
		t.Errorf("expected authoritative negative, got %q", out)
	}
}

// TestBundleLocate_NoBundleJSONIsEmptyArray: the negative must be an empty JSON
// array, never null — `jq '.bundles | length'` has to work on it.
func TestBundleLocate_NoBundleJSONIsEmptyArray(t *testing.T) {
	stub := newBundleStub()
	stub.seed("gd-Origin", map[string]any{
		"title": "bundle-less plan", "type": "epic", "kind": "issue",
	})
	setupBundleDaemon(t, stub)
	resetBundleFlags(t)

	oldJSON := jsonOutput
	jsonOutput = true
	t.Cleanup(func() { jsonOutput = oldJSON })

	out := captureStdout(t, func() error {
		cmd := bundleLocateCmd
		cmd.SetContext(context.Background())
		return cmd.RunE(cmd, []string{"gd-Origin"})
	})

	var got map[string]any
	if err := json.Unmarshal(out, &got); err != nil {
		t.Fatalf("unmarshal JSON output %q: %v", out, err)
	}
	arr, ok := got["bundles"].([]any)
	if !ok {
		t.Fatalf("bundles must be a JSON array (not null) on the negative; got %q", out)
	}
	if len(arr) != 0 {
		t.Errorf("expected empty bundles array, got %v", arr)
	}
}

// TestBundleLocate_RejectsUnknownOrigin: a typo'd origin id must fail loudly
// rather than masquerade as an authoritative "no bundle attached".
func TestBundleLocate_RejectsUnknownOrigin(t *testing.T) {
	stub := newBundleStub()
	setupBundleDaemon(t, stub)
	resetBundleFlags(t)

	cmd := bundleLocateCmd
	cmd.SetContext(context.Background())
	err := cmd.RunE(cmd, []string{"gd-Nonexistent"})
	if err == nil {
		t.Fatal("expected an error for an unknown origin id, got nil")
	}
	if !strings.Contains(err.Error(), "fetching origin") {
		t.Errorf("expected a fetching-origin error, got %v", err)
	}
}

func TestBundleShow_ListsChildrenInOrder(t *testing.T) {
	stub := newBundleStub()
	stub.seed("gd-Bundle", map[string]any{
		"title": "bundle title", "type": "bundle", "kind": "issue",
		"fields": map[string]string{"kind": "plan", "headline": "h"},
	})
	stub.seed("gd-Doc1", map[string]any{
		"title": "doc1", "type": "doc", "kind": "data",
		"fields": map[string]string{"format": "md", "position": "2"},
	})
	stub.seed("gd-Doc2", map[string]any{
		"title": "doc2", "type": "doc", "kind": "data",
		"fields": map[string]string{"format": "diff", "position": "1"},
	})
	// pre-seed deps so the mention-by handler returns them.
	stub.deps = []map[string]string{
		{"bead_id": "gd-Doc1", "depends_on_id": "gd-Bundle", "type": "parent-child"},
		{"bead_id": "gd-Doc2", "depends_on_id": "gd-Bundle", "type": "parent-child"},
	}
	setupBundleDaemon(t, stub)
	resetBundleFlags(t)

	cmd := bundleShowCmd
	cmd.SetContext(context.Background())
	if err := cmd.RunE(cmd, []string{"gd-Bundle"}); err != nil {
		t.Fatalf("RunE: %v", err)
	}
}

// The doc-read hint must survive `gb bundle show <id> | head -5`. It used to be
// the last line printed, below the doc list, so the common peek-at-a-TOC pipeline
// truncated it and readers improvised malformed commands (ka-0000002294). Seeded
// with BOTH optional header fields (kind + headline) and 4 docs — the widest real
// header, which is what pushed the old hint to line 10.
func TestBundleShow_ReadHintSurvivesHead5(t *testing.T) {
	stub := newBundleStub()
	stub.seed("gd-Bundle", map[string]any{
		"title": "bundle title", "type": "bundle", "kind": "issue",
		"fields": map[string]string{"kind": "report", "headline": "a headline"},
	})
	stub.deps = nil
	for i, id := range []string{"gd-DocA", "gd-DocB", "gd-DocC", "gd-DocD"} {
		stub.seed(id, map[string]any{
			"title": "doc " + id, "type": "doc", "kind": "data",
			"fields": map[string]string{"format": "md", "position": strconv.Itoa(i)},
		})
		stub.deps = append(stub.deps, map[string]string{
			"bead_id": id, "depends_on_id": "gd-Bundle", "type": "parent-child",
		})
	}
	setupBundleDaemon(t, stub)
	resetBundleFlags(t)

	out := string(captureStdout(t, func() error {
		cmd := bundleShowCmd
		cmd.SetContext(context.Background())
		return cmd.RunE(cmd, []string{"gd-Bundle"})
	}))

	lines := strings.Split(strings.TrimRight(out, "\n"), "\n")
	if len(lines) < 5 {
		t.Fatalf("expected >=5 lines of output, got %d:\n%s", len(lines), out)
	}
	head5 := strings.Join(lines[:5], "\n")
	if !strings.Contains(head5, "gb bundle doc") {
		t.Errorf("read verb must appear within the first 5 lines (survive `| head -5`); got:\n%s", head5)
	}
	// The hint has to name a REAL doc id, not just the <doc-id> placeholder —
	// that is what makes it copy-pasteable rather than another lookup.
	if !strings.Contains(head5, "gb bundle doc gd-DocA") {
		t.Errorf("hint within head -5 must name a real doc id (gd-DocA); got:\n%s", head5)
	}
	// Every doc line carries its own read verb, so ANY truncation that shows a
	// doc at all also shows how to read that exact doc.
	for _, id := range []string{"gd-DocA", "gd-DocB", "gd-DocC", "gd-DocD"} {
		if !strings.Contains(out, "read: gb bundle doc "+id) {
			t.Errorf("doc line for %s missing inline read verb; got:\n%s", id, out)
		}
	}
}

// A bundle with no docs must not advertise a read verb — there is nothing to read.
func TestBundleShow_NoDocsOmitsReadHint(t *testing.T) {
	stub := newBundleStub()
	stub.seed("gd-Bundle", map[string]any{
		"title": "empty bundle", "type": "bundle", "kind": "issue",
		"fields": map[string]string{"kind": "plan"},
	})
	setupBundleDaemon(t, stub)
	resetBundleFlags(t)

	out := string(captureStdout(t, func() error {
		cmd := bundleShowCmd
		cmd.SetContext(context.Background())
		return cmd.RunE(cmd, []string{"gd-Bundle"})
	}))

	if strings.Contains(out, "gb bundle doc") {
		t.Errorf("empty bundle must not advertise a doc-read verb; got:\n%s", out)
	}
	if !strings.Contains(out, "gb bundle add") {
		t.Errorf("empty bundle should still point at `gb bundle add`; got:\n%s", out)
	}
}

func TestBundleShow_RejectsNonBundle(t *testing.T) {
	stub := newBundleStub()
	stub.seed("gd-Task", map[string]any{
		"title": "task", "type": "task", "kind": "issue",
	})
	setupBundleDaemon(t, stub)
	resetBundleFlags(t)

	cmd := bundleShowCmd
	cmd.SetContext(context.Background())
	err := cmd.RunE(cmd, []string{"gd-Task"})
	if err == nil || !strings.Contains(err.Error(), "is not a bundle") {
		t.Fatalf("expected non-bundle error; got %v", err)
	}
}

func TestBundleDoc_PrintsInlineContent(t *testing.T) {
	stub := newBundleStub()
	stub.seed("gd-Doc", map[string]any{
		"title": "report doc", "type": "doc", "kind": "data",
		"fields": map[string]string{
			"format":   "md",
			"headline": "the headline",
			"content":  "# Hello\nworld body",
		},
	})
	setupBundleDaemon(t, stub)
	resetBundleFlags(t)

	out := string(captureStdout(t, func() error {
		cmd := bundleDocCmd
		cmd.SetContext(context.Background())
		return cmd.RunE(cmd, []string{"gd-Doc"})
	}))
	if !strings.Contains(out, "world body") {
		t.Errorf("stdout missing doc content body: %q", out)
	}
}

func TestBundleDoc_JSONReturnsContent(t *testing.T) {
	stub := newBundleStub()
	stub.seed("gd-Doc", map[string]any{
		"title": "report doc", "type": "doc", "kind": "data",
		"fields": map[string]string{
			"format":   "md",
			"headline": "the headline",
			"content":  "body text here",
		},
	})
	setupBundleDaemon(t, stub)
	resetBundleFlags(t)

	oldJSON := jsonOutput
	jsonOutput = true
	t.Cleanup(func() { jsonOutput = oldJSON })

	out := string(captureStdout(t, func() error {
		cmd := bundleDocCmd
		cmd.SetContext(context.Background())
		return cmd.RunE(cmd, []string{"gd-Doc"})
	}))
	var got map[string]string
	if err := json.Unmarshal([]byte(out), &got); err != nil {
		t.Fatalf("unmarshal JSON output %q: %v", out, err)
	}
	if got["content"] != "body text here" {
		t.Errorf("content = %q, want %q", got["content"], "body text here")
	}
	if got["id"] != "gd-Doc" {
		t.Errorf("id = %q, want gd-Doc", got["id"])
	}
	if got["format"] != "md" {
		t.Errorf("format = %q, want md", got["format"])
	}
}

func TestBundleDoc_RejectsNonDoc(t *testing.T) {
	stub := newBundleStub()
	stub.seed("gd-Bundle", map[string]any{
		"title": "bundle", "type": "bundle", "kind": "issue",
	})
	setupBundleDaemon(t, stub)
	resetBundleFlags(t)

	cmd := bundleDocCmd
	cmd.SetContext(context.Background())
	err := cmd.RunE(cmd, []string{"gd-Bundle"})
	if err == nil || !strings.Contains(err.Error(), "is not a doc") {
		t.Fatalf("expected non-doc error; got %v", err)
	}
}

func TestBundleDoc_ContentURLFallback(t *testing.T) {
	stub := newBundleStub()
	stub.seed("gd-Doc", map[string]any{
		"title": "big doc", "type": "doc", "kind": "data",
		"fields": map[string]string{
			"format":      "link",
			"content_url": "https://example.test/big.md",
		},
	})
	setupBundleDaemon(t, stub)
	resetBundleFlags(t)

	out := string(captureStdout(t, func() error {
		cmd := bundleDocCmd
		cmd.SetContext(context.Background())
		return cmd.RunE(cmd, []string{"gd-Doc"})
	}))
	if !strings.Contains(out, "https://example.test/big.md") {
		t.Errorf("stdout missing content_url fallback: %q", out)
	}
}

func TestFormatFromExt(t *testing.T) {
	cases := []struct {
		path string
		want string
	}{
		{"/tmp/a.md", "md"},
		{"/tmp/a.markdown", "md"},
		{"x.diff", "diff"},
		{"x.patch", "diff"},
		{"x.csv", "csv"},
		{"x.png", "image"},
		{"x.go", "code"},
		{"x.rs", "code"},
		{"x.html", "html"},
		{"x.htm", "html"},
		{"x.unknown", ""},
		{"x", ""},
	}
	for _, c := range cases {
		if got := formatFromExt(c.path); got != c.want {
			t.Errorf("formatFromExt(%q) = %q, want %q", c.path, got, c.want)
		}
	}
}

func TestBundleURL_DefaultAbsolute(t *testing.T) {
	prev := os.Getenv("BEADS_VIEWER_URL")
	t.Cleanup(func() { _ = os.Setenv("BEADS_VIEWER_URL", prev) })

	// Unset → fall back to the built-in absolute default (ka-0000003798), NOT a
	// hostless "/bundle/<id>". The default carries the /beads path prefix and a
	// host, so the printed link is always clickable — even from an agent pod,
	// which never gets BEADS_VIEWER_URL injected.
	_ = os.Unsetenv("BEADS_VIEWER_URL")
	want := strings.TrimRight(defaultGardenioViewerURL, "/") + "/bundle/gd-X"
	if got := bundleURL("gd-X"); got != want {
		t.Errorf("got %q, want %q", got, want)
	}

	_ = os.Setenv("BEADS_VIEWER_URL", "https://viewer.example/")
	if got := bundleURL("gd-X"); got != "https://viewer.example/bundle/gd-X" {
		t.Errorf("got %q, want https://viewer.example/bundle/gd-X", got)
	}
}
