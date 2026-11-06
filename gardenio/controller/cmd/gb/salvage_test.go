package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/s3"
)

// ── read/write in-memory S3 fake ────────────────────────────────────────────
//
// Serves the PUT / GET / ListObjectsV2 calls gb salvage + gb salvage restore
// make, storing objects in a map keyed by object key. Path-style addressing
// (/<bucket>/<key>) mirrors the COOP_S3_ENDPOINT in-box MinIO wiring.

type memS3 struct {
	mu      sync.Mutex
	objects map[string][]byte
	// onPut, when set, is called with the object key on every PUT before the
	// object is stored. Tests use it to observe WHEN the upload happened
	// relative to the other salvage tiers. Set it before the run starts.
	onPut func(key string)
}

// newMemS3 starts an in-memory S3 fake and wires the process so newS3Client
// (used by the salvage code under test) resolves to it: COOP_S3_ENDPOINT →
// the httptest URL (path-style), and awsConfigLoader → static test creds. It
// also returns a directly-wired client for tests that want to assert via S3.
func newMemS3(t *testing.T) (*memS3, *s3.Client) {
	t.Helper()
	m := &memS3{objects: map[string][]byte{}}
	srv := httptest.NewServer(http.HandlerFunc(m.serve))
	t.Cleanup(srv.Close)

	// Point newS3Client at the fake: it reads COOP_S3_ENDPOINT (→ path-style)
	// and resolves creds/region via awsConfigLoader.
	t.Setenv("COOP_S3_ENDPOINT", srv.URL)
	orig := awsConfigLoader
	t.Cleanup(func() { awsConfigLoader = orig })
	awsConfigLoader = func(ctx context.Context, _ ...func(*awsconfig.LoadOptions) error) (aws.Config, error) {
		return aws.Config{
			Region:      "us-east-1",
			Credentials: credentials.NewStaticCredentialsProvider("test", "test", ""),
		}, nil
	}

	client := s3.NewFromConfig(aws.Config{
		Region:      "us-east-1",
		Credentials: credentials.NewStaticCredentialsProvider("test", "test", ""),
	}, func(o *s3.Options) {
		o.BaseEndpoint = aws.String(srv.URL)
		o.UsePathStyle = true
	})
	return m, client
}

func (m *memS3) serve(w http.ResponseWriter, r *http.Request) {
	p := strings.TrimPrefix(r.URL.Path, "/")
	_, key, _ := strings.Cut(p, "/") // strip "<bucket>/"

	switch r.Method {
	case http.MethodPut:
		if m.onPut != nil {
			m.onPut(key)
		}
		body, _ := readAllBody(r)
		m.mu.Lock()
		m.objects[key] = body
		m.mu.Unlock()
		w.Header().Set("ETag", "\"x\"")
		w.WriteHeader(http.StatusOK)
	case http.MethodGet:
		if r.URL.Query().Get("list-type") == "2" {
			m.serveList(w, r)
			return
		}
		m.mu.Lock()
		body, ok := m.objects[key]
		m.mu.Unlock()
		if !ok {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(body)
	default:
		w.WriteHeader(http.StatusMethodNotAllowed)
	}
}

func (m *memS3) serveList(w http.ResponseWriter, r *http.Request) {
	prefix := r.URL.Query().Get("prefix")
	m.mu.Lock()
	var b strings.Builder
	b.WriteString(`<?xml version="1.0"?><ListBucketResult xmlns="http://s3.amazonaws.com/doc/2006-03-01/">`)
	for k := range m.objects {
		if strings.HasPrefix(k, prefix) {
			fmt.Fprintf(&b, `<Contents><Key>%s</Key><Size>%d</Size></Contents>`, k, len(m.objects[k]))
		}
	}
	m.mu.Unlock()
	b.WriteString(`<IsTruncated>false</IsTruncated></ListBucketResult>`)
	w.Header().Set("Content-Type", "application/xml")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte(b.String()))
}

func readAllBody(r *http.Request) ([]byte, error) {
	var buf bytes.Buffer
	_, err := buf.ReadFrom(r.Body)
	return buf.Bytes(), err
}

func (m *memS3) get(key string) ([]byte, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	b, ok := m.objects[key]
	return b, ok
}

func (m *memS3) keys() []string {
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []string
	for k := range m.objects {
		out = append(out, k)
	}
	return out
}

// ── git worktree fixtures ────────────────────────────────────────────────────

// svGit runs git in dir with a deterministic identity and fails the test on
// error, returning trimmed combined output. Used only by salvage test fixtures.
func svGit(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
	cmd.Env = append(os.Environ(),
		"GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@t",
		"GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@t",
		"GIT_TERMINAL_PROMPT=0",
	)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %s: %v\n%s", strings.Join(args, " "), err, out)
	}
	return strings.TrimSpace(string(out))
}

// newOriginWithClone builds an "origin" bare-ish repo with one commit on main
// and a clone at <root>/work whose origin/main resolves. Returns the work path.
func newOriginWithClone(t *testing.T, root string) string {
	t.Helper()
	origin := filepath.Join(root, "origin")
	if err := os.MkdirAll(origin, 0o755); err != nil {
		t.Fatal(err)
	}
	svGit(t, origin, "init", "-q", "-b", "main")
	if err := os.WriteFile(filepath.Join(origin, "base.txt"), []byte("base\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	svGit(t, origin, "add", "-A")
	svGit(t, origin, "commit", "-qm", "base")

	work := filepath.Join(root, "work")
	svGit(t, root, "clone", "-q", origin, work)
	return work
}

// dirtyUp makes the worktree at path dirty: an unpushed commit, a tracked edit,
// an untracked file, and a .gitignored file (which must NOT be salvaged).
func dirtyUp(t *testing.T, path string) {
	t.Helper()
	svGit(t, path, "checkout", "-q", "-b", "feature")
	if err := os.WriteFile(filepath.Join(path, "committed.txt"), []byte("committed\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	svGit(t, path, "add", "-A")
	svGit(t, path, "commit", "-qm", "unpushed work")
	svAppend(t, filepath.Join(path, "base.txt"), "wip-edit\n")
	if err := os.WriteFile(filepath.Join(path, "untracked.txt"), []byte("untracked\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(path, ".gitignore"), []byte("*.log\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(path, "secret.log"), []byte("SHOULD-NOT-SALVAGE\n"), 0o644); err != nil {
		t.Fatal(err)
	}
}

func svAppend(t *testing.T, path, s string) {
	t.Helper()
	f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if _, err := f.WriteString(s); err != nil {
		t.Fatal(err)
	}
}

// salvageTestEnv clears the ambient agent env vars that runSalvage reads so
// each test controls them explicitly, and sets a bucket + session key.
func salvageTestEnv(t *testing.T) {
	t.Helper()
	for _, k := range []string{
		"COOP_S3_BUCKET", "COOP_S3_PREFIX", "COOP_S3_SESSION_KEY", "COOP_S3_REGION",
		"KD_AGENT_ID", "BOAT_AGENT_BEAD_ID", "KD_ACTOR", "BOAT_AGENT",
		"BOAT_TASK_ID", "BOAT_PROJECT", "BOAT_WORKSPACE",
	} {
		t.Setenv(k, "")
	}
	t.Setenv("COOP_S3_BUCKET", "test-bucket")
	t.Setenv("COOP_S3_PREFIX", "coop/sessions")
	t.Setenv("KD_AGENT_ID", "gd-agent1")
	t.Setenv("COOP_S3_SESSION_KEY", "gd-agent1")
}

// ── tests ────────────────────────────────────────────────────────────────────

// TestRunSalvage_DirtySingleWorktree is the golden path: a dirty primary
// checkout is bundled to S3, a manifest is written, and the bundle recovers the
// unpushed commit + tracked edit + untracked file while excluding the ignored
// file. The agent branch pointer is untouched.
func TestRunSalvage_DirtySingleWorktree(t *testing.T) {
	root := t.TempDir()
	work := newOriginWithClone(t, root)
	dirtyUp(t, work)
	headBefore := svGit(t, work, "rev-parse", "HEAD")
	branchBefore := svGit(t, work, "rev-parse", "feature")

	salvageTestEnv(t)
	// newMemS3 wires newS3Client (used by runSalvage) at the in-memory fake; we
	// assert on the objects it stored.
	mem, _ := newMemS3(t)

	if err := runSalvage(context.Background(), work, "unit_test", salvageOpts{}); err != nil {
		t.Fatalf("runSalvage: %v", err)
	}

	// The bundle + manifest must exist under the expected prefix.
	bundleKey := "coop/sessions/gd-agent1/gd-agent1/salvage/work.bundle"
	manifestKey := "coop/sessions/gd-agent1/gd-agent1/salvage/salvage-manifest.json"
	bundle, ok := mem.get(bundleKey)
	if !ok {
		t.Fatalf("bundle not uploaded; keys=%v", mem.keys())
	}
	if len(bundle) == 0 {
		t.Fatal("bundle is empty")
	}
	manifestRaw, ok := mem.get(manifestKey)
	if !ok {
		t.Fatalf("manifest not uploaded; keys=%v", mem.keys())
	}
	var m salvageManifest
	if err := json.Unmarshal(manifestRaw, &m); err != nil {
		t.Fatalf("manifest json: %v", err)
	}
	if len(m.Targets) != 1 || m.Targets[0].Name != "work" {
		t.Fatalf("expected 1 target 'work', got %+v", m.Targets)
	}
	if m.Targets[0].BundleKey != bundleKey {
		t.Errorf("manifest bundle key = %q, want %q", m.Targets[0].BundleKey, bundleKey)
	}
	if m.Targets[0].AnchorRef != "origin/main" {
		t.Errorf("anchor ref = %q, want origin/main", m.Targets[0].AnchorRef)
	}

	// Branch pointer + HEAD must be unchanged by salvage.
	if got := svGit(t, work, "rev-parse", "HEAD"); got != headBefore {
		t.Errorf("HEAD moved: %s -> %s", headBefore, got)
	}
	if got := svGit(t, work, "rev-parse", "feature"); got != branchBefore {
		t.Errorf("branch feature moved: %s -> %s", branchBefore, got)
	}
	// The temporary salvage ref must be cleaned up.
	if _, err := exec.Command("git", "-C", work, "show-ref", "--verify", "--quiet", "refs/salvage/work").Output(); err == nil {
		t.Error("refs/salvage/work should have been deleted after bundling")
	}

	// Round-trip: a fresh clone that fetches the bundle recovers the WIP.
	assertBundleRecovers(t, root, bundle, map[string]string{
		"base.txt":      "base\nwip-edit\n",
		"committed.txt": "committed\n",
		"untracked.txt": "untracked\n",
	}, []string{"secret.log"})
}

// TestRunSalvage_MultipleWorktrees salvages the primary checkout plus two dirty
// per-bead worktrees at the SIBLING location (<project>/worktrees/<bead>, bug
// ka-0000007049), and leaves a clean worktree alone.
func TestRunSalvage_MultipleWorktrees(t *testing.T) {
	root := t.TempDir()
	work := newOriginWithClone(t, root)
	dirtyUp(t, work)

	// Two per-bead worktrees at the sibling location; one dirty, one clean.
	mkWorktree := func(bead string, dirty bool) {
		wt := filepath.Join(worktreesDir(work), bead)
		svGit(t, work, "worktree", "add", "-q", "-b", "wt-"+bead, wt, "main")
		if dirty {
			svAppend(t, filepath.Join(wt, "base.txt"), "wip-"+bead+"\n")
		}
	}
	mkWorktree("gd-dirtyone", true)
	mkWorktree("gd-cleanone", false)

	salvageTestEnv(t)
	mem, _ := newMemS3(t)

	if err := runSalvage(context.Background(), work, "unit_test", salvageOpts{}); err != nil {
		t.Fatalf("runSalvage: %v", err)
	}

	base := "coop/sessions/gd-agent1/gd-agent1/salvage"
	if _, ok := mem.get(base + "/work.bundle"); !ok {
		t.Errorf("missing work.bundle; keys=%v", mem.keys())
	}
	if _, ok := mem.get(base + "/gd-dirtyone.bundle"); !ok {
		t.Errorf("missing gd-dirtyone.bundle; keys=%v", mem.keys())
	}
	if _, ok := mem.get(base + "/gd-cleanone.bundle"); ok {
		t.Error("clean worktree gd-cleanone should NOT have been bundled")
	}

	manifestRaw, _ := mem.get(base + "/salvage-manifest.json")
	var m salvageManifest
	_ = json.Unmarshal(manifestRaw, &m)
	if len(m.Targets) != 2 {
		t.Fatalf("expected 2 salvaged targets, got %d: %+v", len(m.Targets), m.Targets)
	}
}

// TestRunSalvage_LegacyNestedWorktreeStillBundled verifies that a pod still on
// the OLD nested layout (<workspace>/.beads/worktrees/<bead>) is still salvaged
// for one release after the sibling relocation (bug ka-0000007049). A dirty
// worktree living ONLY at the legacy nested path must still land a bundle.
func TestRunSalvage_LegacyNestedWorktreeStillBundled(t *testing.T) {
	root := t.TempDir()
	work := newOriginWithClone(t, root)

	// Dirty per-bead worktree at the LEGACY nested location only.
	wt := filepath.Join(work, ".beads", "worktrees", "gd-legacy")
	svGit(t, work, "worktree", "add", "-q", "-b", "wt-legacy", wt, "main")
	svAppend(t, filepath.Join(wt, "base.txt"), "wip-legacy\n")

	salvageTestEnv(t)
	mem, _ := newMemS3(t)

	if err := runSalvage(context.Background(), work, "unit_test", salvageOpts{}); err != nil {
		t.Fatalf("runSalvage: %v", err)
	}

	base := "coop/sessions/gd-agent1/gd-agent1/salvage"
	if _, ok := mem.get(base + "/gd-legacy.bundle"); !ok {
		t.Errorf("legacy nested worktree not salvaged; keys=%v", mem.keys())
	}
}

// TestRunSalvage_WorktreeDedupAcrossLayouts verifies that when the SAME bead id
// appears at BOTH the sibling and legacy nested locations (e.g. a pod caught
// mid-migration), salvage bundles it exactly ONCE — the sibling copy wins
// (scanned first) and the duplicate bead name is skipped.
func TestRunSalvage_WorktreeDedupAcrossLayouts(t *testing.T) {
	root := t.TempDir()
	work := newOriginWithClone(t, root)

	// Same bead id at both locations, both dirty.
	sib := filepath.Join(worktreesDir(work), "gd-dup")
	svGit(t, work, "worktree", "add", "-q", "-b", "wt-dup-sib", sib, "main")
	svAppend(t, filepath.Join(sib, "base.txt"), "wip-sibling\n")

	leg := filepath.Join(work, ".beads", "worktrees", "gd-dup")
	svGit(t, work, "worktree", "add", "-q", "-b", "wt-dup-leg", leg, "main")
	svAppend(t, filepath.Join(leg, "base.txt"), "wip-legacy\n")

	salvageTestEnv(t)
	mem, _ := newMemS3(t)

	if err := runSalvage(context.Background(), work, "unit_test", salvageOpts{}); err != nil {
		t.Fatalf("runSalvage: %v", err)
	}

	base := "coop/sessions/gd-agent1/gd-agent1/salvage"
	if _, ok := mem.get(base + "/gd-dup.bundle"); !ok {
		t.Errorf("dup worktree not salvaged; keys=%v", mem.keys())
	}
	manifestRaw, _ := mem.get(base + "/salvage-manifest.json")
	var m salvageManifest
	_ = json.Unmarshal(manifestRaw, &m)
	dupCount := 0
	for _, tgt := range m.Targets {
		if tgt.Name == "gd-dup" {
			dupCount++
		}
	}
	if dupCount != 1 {
		t.Fatalf("expected bead gd-dup bundled exactly once, got %d: %+v", dupCount, m.Targets)
	}
}

// newNestedRepo seeds a nested working repo (its own origin + clone) at
// <parent>/<name>, dirties it exactly like dirtyUp (unpushed commit + tracked
// edit + untracked file + gitignored file), and returns (clonePath,
// unpushedSHA). The unpushed commit's sha is the object that is UNRECOVERABLE
// under the pre-fix salvage (the outer tree records the nested repo as a gitlink
// only) — the round-trip test cat-file's exactly this sha.
func newNestedRepo(t *testing.T, root, parent, name string) (string, string) {
	t.Helper()
	origin := filepath.Join(root, name+"-origin")
	if err := os.MkdirAll(origin, 0o755); err != nil {
		t.Fatal(err)
	}
	svGit(t, origin, "init", "-q", "-b", "main")
	if err := os.WriteFile(filepath.Join(origin, "base.txt"), []byte("nested-base\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	svGit(t, origin, "add", "-A")
	svGit(t, origin, "commit", "-qm", "nested base")

	clone := filepath.Join(parent, name)
	if err := os.MkdirAll(parent, 0o755); err != nil {
		t.Fatal(err)
	}
	svGit(t, root, "clone", "-q", origin, clone)

	// Unpushed WIP branch + commit — this commit's sha is what gets lost.
	svGit(t, clone, "checkout", "-q", "-b", "wip-feature")
	if err := os.WriteFile(filepath.Join(clone, "feature.txt"), []byte("nested-feature\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	svGit(t, clone, "add", "-A")
	svGit(t, clone, "commit", "-qm", "nested unpushed work")
	unpushed := svGit(t, clone, "rev-parse", "HEAD")

	// Working-tree dirt so salvageWorktreeDirty fires (an unpushed commit alone
	// is not "dirty" per `git status --porcelain`).
	svAppend(t, filepath.Join(clone, "base.txt"), "nested-wip-edit\n")
	if err := os.WriteFile(filepath.Join(clone, "untracked.txt"), []byte("nested-untracked\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(clone, ".gitignore"), []byte("*.log\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(clone, "secret.log"), []byte("SHOULD-NOT-SALVAGE\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	return clone, unpushed
}

// TestRunSalvage_NestedReposSiblingLayout is the regression guard for bug
// ka-0000001959: a nested reference repo under <parent>/repos/<name> (the
// standard crew layout, where the salvage root is <project>/work and repos is
// its SIBLING) must be salvaged as its OWN bundle carrying its own git objects —
// NOT reduced to an outer-tree gitlink, which would leave the nested WIP
// unrecoverable. Round-trips through restore and asserts the nested unpushed sha
// resolves in a fresh clone.
func TestRunSalvage_NestedReposSiblingLayout(t *testing.T) {
	root := t.TempDir()
	work := newOriginWithClone(t, root) // root/work + root/origin
	// Sibling repos dir: filepath.Dir(work) == root, so root/repos/demo.
	demo, unpushed := newNestedRepo(t, root, filepath.Join(root, "repos"), "demo")

	salvageTestEnv(t)
	mem, _ := newMemS3(t)
	if err := runSalvage(context.Background(), work, "unit_test", salvageOpts{}); err != nil {
		t.Fatalf("runSalvage: %v", err)
	}

	base := "coop/sessions/gd-agent1/gd-agent1/salvage"
	bundle, ok := mem.get(base + "/repos-demo.bundle")
	if !ok {
		t.Fatalf("missing repos-demo.bundle (nested repo not salvaged); keys=%v", mem.keys())
	}

	// The nested repo's branch pointer must be untouched by salvage.
	if got := svGit(t, demo, "rev-parse", "wip-feature"); got != unpushed {
		t.Errorf("nested branch moved: %s -> %s", unpushed, got)
	}

	// Round-trip: restore into a fresh clone of the nested origin and assert the
	// UNPUSHED commit sha is recoverable (the exact object lost pre-fix) plus the
	// working-tree WIP, while the gitignored file is excluded.
	assertNestedBundleRecovers(t, root, "demo", bundle, unpushed,
		map[string]string{
			"base.txt":      "nested-base\nnested-wip-edit\n",
			"feature.txt":   "nested-feature\n",
			"untracked.txt": "nested-untracked\n",
		},
		[]string{"secret.log"})
}

// TestRunSalvage_NestedReposInsideWorkspace covers the alternate layout where a
// nested repo is checked out INSIDE the workspace at <workspace>/repos/<name>
// (recorded by the outer tree as a gitlink). It must still be salvaged as its
// own bundle.
func TestRunSalvage_NestedReposInsideWorkspace(t *testing.T) {
	root := t.TempDir()
	work := newOriginWithClone(t, root)
	// Nested inside the workspace: <work>/repos/inner.
	_, unpushed := newNestedRepo(t, root, filepath.Join(work, "repos"), "inner")

	salvageTestEnv(t)
	mem, _ := newMemS3(t)
	if err := runSalvage(context.Background(), work, "unit_test", salvageOpts{}); err != nil {
		t.Fatalf("runSalvage: %v", err)
	}

	base := "coop/sessions/gd-agent1/gd-agent1/salvage"
	bundle, ok := mem.get(base + "/repos-inner.bundle")
	if !ok {
		t.Fatalf("missing repos-inner.bundle (nested-in-workspace repo not salvaged); keys=%v", mem.keys())
	}
	assertNestedBundleRecovers(t, root, "inner", bundle, unpushed,
		map[string]string{"feature.txt": "nested-feature\n"}, nil)
}

// assertNestedBundleRecovers clones the nested repo's origin fresh, fetches the
// (incremental) salvage bundle, asserts the unpushed sha now resolves, checks
// out the salvage ref, and verifies file contents + absences.
func assertNestedBundleRecovers(t *testing.T, root, name string, bundle []byte, unpushedSHA string, wantFiles map[string]string, wantAbsent []string) {
	t.Helper()
	bundlePath := filepath.Join(t.TempDir(), name+".bundle")
	if err := os.WriteFile(bundlePath, bundle, 0o644); err != nil {
		t.Fatal(err)
	}
	recover := filepath.Join(t.TempDir(), "recover-"+name)
	svGit(t, root, "clone", "-q", filepath.Join(root, name+"-origin"), recover)

	// Pre-fix, this object is NOT in the fresh clone.
	if _, err := exec.Command("git", "-C", recover, "cat-file", "-t", unpushedSHA).Output(); err == nil {
		t.Fatalf("precondition: unpushed sha %s already present before fetch", unpushedSHA)
	}

	svGit(t, recover, "fetch", bundlePath, "refs/salvage/*:refs/salvage/*")

	// The crux of bug ka-0000001959: the nested unpushed commit's objects must
	// have been bundled, so cat-file resolves it after the fetch.
	if out, err := exec.Command("git", "-C", recover, "cat-file", "-t", unpushedSHA).CombinedOutput(); err != nil {
		t.Fatalf("nested unpushed sha %s unrecoverable after restore (the exact data-loss bug): %v\n%s", unpushedSHA, err, out)
	}

	ref := svGit(t, recover, "for-each-ref", "--format=%(refname)", "refs/salvage/")
	ref = strings.Split(ref, "\n")[0]
	svGit(t, recover, "checkout", "-q", ref)
	for f, want := range wantFiles {
		assertFile(t, filepath.Join(recover, f), want)
	}
	for _, f := range wantAbsent {
		if _, err := os.Stat(filepath.Join(recover, f)); err == nil {
			t.Errorf("file %s should be absent after recovery", f)
		}
	}
}

// TestRunSalvage_CleanWorktreeNoOp: a pristine clone on main writes nothing.
func TestRunSalvage_CleanWorktreeNoOp(t *testing.T) {
	root := t.TempDir()
	work := newOriginWithClone(t, root)

	salvageTestEnv(t)
	mem, _ := newMemS3(t)

	if err := runSalvage(context.Background(), work, "unit_test", salvageOpts{}); err != nil {
		t.Fatalf("runSalvage: %v", err)
	}
	if len(mem.keys()) != 0 {
		t.Errorf("clean worktree should upload nothing, got keys=%v", mem.keys())
	}
}

// commitUnpushedCleanTree checks out a feature branch, makes ONE commit, and
// leaves the working tree spotless — the exact shape bug ka-0000005216 is about:
// the agent committed its work and died before `git push`. `git status
// --porcelain` is empty here, so the pre-fix dirty-only discovery salvaged
// nothing. Returns the unpushed commit sha (the object that would be lost).
func commitUnpushedCleanTree(t *testing.T, path string) string {
	t.Helper()
	svGit(t, path, "checkout", "-q", "-b", "feature")
	if err := os.WriteFile(filepath.Join(path, "committed.txt"), []byte("committed work\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	svGit(t, path, "add", "-A")
	svGit(t, path, "commit", "-qm", "unpushed committed work")
	if out := svGit(t, path, "status", "--porcelain"); out != "" {
		t.Fatalf("precondition: working tree must be CLEAN, got porcelain %q", out)
	}
	return svGit(t, path, "rev-parse", "HEAD")
}

// TestRunSalvage_UnpushedCommitCleanTree is the core regression guard for bug
// ka-0000005216: a primary checkout whose working tree is CLEAN but whose HEAD
// carries a committed-but-unpushed commit must still be salvaged. Pre-fix,
// discoverSalvageTargets keyed only on `git status --porcelain` and salvaged
// NOTHING — the commit died with the EmptyDir pod. Round-trips through a fresh
// clone to prove the unpushed commit's objects are recoverable.
func TestRunSalvage_UnpushedCommitCleanTree(t *testing.T) {
	root := t.TempDir()
	work := newOriginWithClone(t, root)
	unpushed := commitUnpushedCleanTree(t, work)
	headBefore := svGit(t, work, "rev-parse", "HEAD")
	branchBefore := svGit(t, work, "rev-parse", "feature")

	salvageTestEnv(t)
	mem, _ := newMemS3(t)

	if err := runSalvage(context.Background(), work, "unit_test", salvageOpts{}); err != nil {
		t.Fatalf("runSalvage: %v", err)
	}

	bundleKey := "coop/sessions/gd-agent1/gd-agent1/salvage/work.bundle"
	bundle, ok := mem.get(bundleKey)
	if !ok {
		t.Fatalf("clean-tree-but-unpushed-commit was NOT salvaged (the exact data-loss bug); keys=%v", mem.keys())
	}
	if len(bundle) == 0 {
		t.Fatal("bundle is empty")
	}

	// Branch pointer + HEAD must be unchanged by salvage.
	if got := svGit(t, work, "rev-parse", "HEAD"); got != headBefore {
		t.Errorf("HEAD moved: %s -> %s", headBefore, got)
	}
	if got := svGit(t, work, "rev-parse", "feature"); got != branchBefore {
		t.Errorf("branch feature moved: %s -> %s", branchBefore, got)
	}

	// Round-trip: a fresh clone that fetches the bundle recovers the unpushed
	// commit's objects and its file content.
	bundlePath := filepath.Join(t.TempDir(), "sv.bundle")
	if err := os.WriteFile(bundlePath, bundle, 0o644); err != nil {
		t.Fatal(err)
	}
	recover := filepath.Join(t.TempDir(), "recover")
	svGit(t, root, "clone", "-q", filepath.Join(root, "origin"), recover)
	// Precondition: the unpushed commit is absent from a fresh clone.
	if _, err := exec.Command("git", "-C", recover, "cat-file", "-t", unpushed).Output(); err == nil {
		t.Fatalf("precondition: unpushed sha %s already present before fetch", unpushed)
	}
	svGit(t, recover, "fetch", bundlePath, "refs/salvage/*:refs/salvage/*")
	if out, err := exec.Command("git", "-C", recover, "cat-file", "-t", unpushed).CombinedOutput(); err != nil {
		t.Fatalf("unpushed commit %s unrecoverable after restore: %v\n%s", unpushed, err, out)
	}
	ref := svGit(t, recover, "for-each-ref", "--format=%(refname)", "refs/salvage/")
	ref = strings.Split(ref, "\n")[0]
	svGit(t, recover, "checkout", "-q", ref)
	assertFile(t, filepath.Join(recover, "committed.txt"), "committed work\n")
}

// TestRunSalvage_UnpushedCommitPerBeadWorktree: a per-bead worktree whose tree
// is clean but which carries an unpushed commit is salvaged too (the discovery
// gate for per-bead worktrees now ORs unpushed-commits with dirtiness).
func TestRunSalvage_UnpushedCommitPerBeadWorktree(t *testing.T) {
	root := t.TempDir()
	work := newOriginWithClone(t, root) // primary stays clean+pushed → not salvaged

	// A per-bead worktree with a clean tree but an unpushed commit.
	wt := filepath.Join(work, ".beads", "worktrees", "gd-committed")
	svGit(t, work, "worktree", "add", "-q", "-b", "wt-committed", wt, "main")
	if err := os.WriteFile(filepath.Join(wt, "wtwork.txt"), []byte("wt committed\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	svGit(t, wt, "add", "-A")
	svGit(t, wt, "commit", "-qm", "unpushed wt commit")
	if out := svGit(t, wt, "status", "--porcelain"); out != "" {
		t.Fatalf("precondition: worktree must be CLEAN, got %q", out)
	}

	salvageTestEnv(t)
	mem, _ := newMemS3(t)
	if err := runSalvage(context.Background(), work, "unit_test", salvageOpts{}); err != nil {
		t.Fatalf("runSalvage: %v", err)
	}

	// The per-bead worktree carries only an unpushed commit (clean tree); the
	// unpushed-commit discovery pass is what surfaces it. (The primary is also
	// salvaged here because `git worktree add` leaves an untracked `.beads/` dir
	// in it — a fixture artifact orthogonal to this test; the false-primary case
	// is guarded by TestRunSalvage_CleanPushedNoOp.)
	base := "coop/sessions/gd-agent1/gd-agent1/salvage"
	if _, ok := mem.get(base + "/gd-committed.bundle"); !ok {
		t.Fatalf("per-bead worktree with unpushed commit was NOT salvaged; keys=%v", mem.keys())
	}
}

// TestRunSalvage_CleanPushedNoOp: a checkout that is clean AND fully pushed
// (nothing beyond origin) salvages nothing — the unpushed-commit pass must not
// manufacture a false target when HEAD == origin/main. Distinct from
// TestRunSalvage_CleanWorktreeNoOp (which stays on the initial branch): here we
// create and push a feature branch so origin tracks it, then re-run salvage.
func TestRunSalvage_CleanPushedNoOp(t *testing.T) {
	root := t.TempDir()
	work := newOriginWithClone(t, root)
	// Make origin accept a push to a feature branch, then push HEAD there so the
	// commit is reachable from a remote-tracking ref → NOT unpushed.
	svGit(t, filepath.Join(root, "origin"), "config", "receive.denyCurrentBranch", "ignore")
	svGit(t, work, "checkout", "-q", "-b", "feature")
	if err := os.WriteFile(filepath.Join(work, "committed.txt"), []byte("pushed work\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	svGit(t, work, "add", "-A")
	svGit(t, work, "commit", "-qm", "work")
	svGit(t, work, "push", "-q", "origin", "feature") // updates refs/remotes/origin/feature

	if worktreeHasUnpushedCommits(context.Background(), work) {
		t.Fatal("HEAD is pushed to origin/feature; must not read as unpushed")
	}

	salvageTestEnv(t)
	mem, _ := newMemS3(t)
	if err := runSalvage(context.Background(), work, "unit_test", salvageOpts{}); err != nil {
		t.Fatalf("runSalvage: %v", err)
	}
	if len(mem.keys()) != 0 {
		t.Errorf("clean+pushed checkout should upload nothing, got keys=%v", mem.keys())
	}
}

// TestWorktreeHasUnpushedCommits exercises the helper directly across the
// boundary shapes discovery relies on.
func TestWorktreeHasUnpushedCommits(t *testing.T) {
	root := t.TempDir()

	// (1) clean clone sitting on origin/main → no unpushed commits.
	work := newOriginWithClone(t, root)
	if worktreeHasUnpushedCommits(context.Background(), work) {
		t.Error("fresh clone on origin/main must have no unpushed commits")
	}

	// (2) a local commit not on any remote → unpushed.
	svGit(t, work, "checkout", "-q", "-b", "feature")
	if err := os.WriteFile(filepath.Join(work, "x.txt"), []byte("x\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	svGit(t, work, "add", "-A")
	svGit(t, work, "commit", "-qm", "local")
	if !worktreeHasUnpushedCommits(context.Background(), work) {
		t.Error("a local-only commit must read as unpushed")
	}

	// (3) unborn HEAD (no commits) → rev-list errors → false (nothing to lose).
	unborn := filepath.Join(root, "unborn")
	if err := os.MkdirAll(unborn, 0o755); err != nil {
		t.Fatal(err)
	}
	svGit(t, unborn, "init", "-q", "-b", "main")
	if worktreeHasUnpushedCommits(context.Background(), unborn) {
		t.Error("unborn HEAD must not read as having unpushed commits")
	}

	// (4) remoteless repo with a commit → no remotes at all, so the commit is
	// unpushed by definition (it lives nowhere durable).
	solo := filepath.Join(root, "solo")
	if err := os.MkdirAll(solo, 0o755); err != nil {
		t.Fatal(err)
	}
	svGit(t, solo, "init", "-q", "-b", "main")
	if err := os.WriteFile(filepath.Join(solo, "s.txt"), []byte("s\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	svGit(t, solo, "add", "-A")
	svGit(t, solo, "commit", "-qm", "only-here")
	if !worktreeHasUnpushedCommits(context.Background(), solo) {
		t.Error("a commit in a remoteless repo must read as unpushed")
	}
}

// TestRunSalvage_MissingBucketNoOp: no COOP_S3_BUCKET → quiet no-op, no client,
// no error (even with a dirty worktree).
func TestRunSalvage_MissingBucketNoOp(t *testing.T) {
	root := t.TempDir()
	work := newOriginWithClone(t, root)
	dirtyUp(t, work)

	salvageTestEnv(t)
	t.Setenv("COOP_S3_BUCKET", "") // unset the sink

	if err := runSalvage(context.Background(), work, "unit_test", salvageOpts{}); err != nil {
		t.Fatalf("missing bucket must be a no-op, got: %v", err)
	}
}

// TestRunSalvage_TimeoutIsNonFatal: an already-cancelled context must not panic
// or error out of runSalvage — the run degrades to skips.
func TestRunSalvage_TimeoutIsNonFatal(t *testing.T) {
	root := t.TempDir()
	work := newOriginWithClone(t, root)
	dirtyUp(t, work)

	salvageTestEnv(t)
	_, _ = newMemS3(t)

	ctx, cancel := context.WithCancel(context.Background())
	cancel() // already dead

	// Must return without error regardless (best-effort). It may or may not
	// upload depending on where the deadline bites; the contract is only
	// "never fatal".
	if err := runSalvage(ctx, work, "unit_test", salvageOpts{}); err != nil {
		// A cancelled S3 client build surfaces as a logged whole-run error; the
		// RunE wrapper swallows it. runSalvage itself may return it — that's the
		// caller-logs contract. Accept either, but never panic.
		t.Logf("runSalvage on cancelled ctx returned (logged, non-fatal): %v", err)
	}
}

// TestSnapshotWorktree_BranchImmutability: the detached snapshot never moves the
// branch pointer or the real index, even though it stages -A into a throwaway.
func TestSnapshotWorktree_BranchImmutability(t *testing.T) {
	root := t.TempDir()
	work := newOriginWithClone(t, root)
	dirtyUp(t, work)

	// Capture real index state (staged nothing) + branch before.
	branchBefore := svGit(t, work, "rev-parse", "feature")
	statusBefore := svGit(t, work, "--no-optional-locks", "status", "--porcelain")

	tgt, ok := snapshotWorktree(context.Background(), work, "work")
	if !ok {
		t.Fatal("snapshotWorktree returned not-ok on a dirty worktree")
	}
	if tgt.SalvageSHA == "" {
		t.Fatal("no salvage sha")
	}
	if tgt.HeadSHA == "" {
		t.Fatal("expected a born HEAD sha")
	}

	if got := svGit(t, work, "rev-parse", "feature"); got != branchBefore {
		t.Errorf("branch moved by snapshot: %s -> %s", branchBefore, got)
	}
	// The real working-tree status must be byte-identical: snapshot must not
	// stage anything into the agent's real index.
	if got := svGit(t, work, "--no-optional-locks", "status", "--porcelain"); got != statusBefore {
		t.Errorf("real index/status mutated by snapshot:\nbefore=%q\nafter =%q", statusBefore, got)
	}
}

// TestSnapshotWorktree_NoRemoteAnchor: a repo with no origin/* falls back to a
// self-contained bundle (AnchorSHA empty) that still recovers.
func TestSnapshotWorktree_NoRemoteAnchor(t *testing.T) {
	root := t.TempDir()
	work := filepath.Join(root, "solo")
	if err := os.MkdirAll(work, 0o755); err != nil {
		t.Fatal(err)
	}
	svGit(t, work, "init", "-q", "-b", "main")
	if err := os.WriteFile(filepath.Join(work, "f.txt"), []byte("v1\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	svGit(t, work, "add", "-A")
	svGit(t, work, "commit", "-qm", "base")
	svAppend(t, filepath.Join(work, "f.txt"), "wip\n")

	tgt, ok := snapshotWorktree(context.Background(), work, "solo")
	if !ok {
		t.Fatal("snapshot not ok")
	}
	if tgt.AnchorSHA != "" {
		t.Errorf("expected no durable anchor for a remoteless repo, got %q", tgt.AnchorSHA)
	}

	salvageTestEnv(t)
	mem, _ := newMemS3(t)
	if err := runSalvage(context.Background(), work, "unit_test", salvageOpts{}); err != nil {
		t.Fatalf("runSalvage: %v", err)
	}
	// runSalvage names the primary checkout "work" regardless of on-disk dir.
	bundle, ok := mem.get("coop/sessions/gd-agent1/gd-agent1/salvage/work.bundle")
	if !ok {
		t.Fatalf("no bundle; keys=%v", mem.keys())
	}
	// Self-contained bundle recovers into an empty repo (no prerequisites).
	assertBundleRecoversStandalone(t, bundle, map[string]string{"f.txt": "v1\nwip\n"})
}

// TestSnapshotWorktree_UnbornHead: a repo with NO commits (unborn HEAD) but
// staged/untracked WIP must still be captured. Regression guard for the 0-byte
// GIT_INDEX_FILE bug: git rejects a 0-byte index ("index file smaller than
// expected"), so os.CreateTemp's empty file must be removed before use or the
// snapshot silently drops all WIP.
func TestSnapshotWorktree_UnbornHead(t *testing.T) {
	root := t.TempDir()
	work := filepath.Join(root, "unborn")
	if err := os.MkdirAll(work, 0o755); err != nil {
		t.Fatal(err)
	}
	svGit(t, work, "init", "-q", "-b", "main")
	// No commit → HEAD is unborn. Add tracked + untracked WIP.
	if err := os.WriteFile(filepath.Join(work, "wip.txt"), []byte("unborn-wip\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	tgt, ok := snapshotWorktree(context.Background(), work, "unborn")
	if !ok {
		t.Fatal("snapshotWorktree returned not-ok on an unborn HEAD with WIP — data would be lost")
	}
	if tgt.SalvageSHA == "" {
		t.Fatal("no salvage sha for unborn HEAD")
	}
	if tgt.HeadSHA != "" {
		t.Errorf("expected empty HeadSHA for unborn HEAD, got %q", tgt.HeadSHA)
	}

	// The salvage commit must be parentless and its tree must contain wip.txt.
	salvageTestEnv(t)
	mem, _ := newMemS3(t)
	if err := runSalvage(context.Background(), work, "unit_test", salvageOpts{}); err != nil {
		t.Fatalf("runSalvage: %v", err)
	}
	bundle, ok := mem.get("coop/sessions/gd-agent1/gd-agent1/salvage/work.bundle")
	if !ok {
		t.Fatalf("no bundle for unborn HEAD; keys=%v", mem.keys())
	}
	assertBundleRecoversStandalone(t, bundle, map[string]string{"wip.txt": "unborn-wip\n"})
}

// TestRunSalvageRestore_RoundTrip: a salvage followed by a restore into a fresh
// clone recovers the salvaged refs and files end-to-end.
func TestRunSalvageRestore_RoundTrip(t *testing.T) {
	root := t.TempDir()
	work := newOriginWithClone(t, root)
	dirtyUp(t, work)

	salvageTestEnv(t)
	mem, _ := newMemS3(t)
	if err := runSalvage(context.Background(), work, "unit_test", salvageOpts{}); err != nil {
		t.Fatalf("runSalvage: %v", err)
	}
	_ = mem

	// Fresh clone of origin to restore into.
	recover := filepath.Join(root, "recover")
	svGit(t, root, "clone", "-q", filepath.Join(root, "origin"), recover)
	outDir := filepath.Join(root, "dl")

	// Restore must run with cwd inside the recover clone so the fetch lands
	// there. Change dir for the duration.
	oldWd, _ := os.Getwd()
	if err := os.Chdir(recover); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chdir(oldWd) })

	if err := runSalvageRestore(context.Background(), "gd-agent1", "gd-agent1", outDir, false); err != nil {
		t.Fatalf("runSalvageRestore: %v", err)
	}

	// The salvage ref must now be fetchable, and checking it out recovers WIP.
	if _, err := exec.Command("git", "-C", recover, "show-ref", "--verify", "--quiet", "refs/salvage/work").Output(); err != nil {
		t.Fatalf("refs/salvage/work not fetched into recover clone")
	}
	svGit(t, recover, "checkout", "-q", "refs/salvage/work")
	assertFile(t, filepath.Join(recover, "base.txt"), "base\nwip-edit\n")
	assertFile(t, filepath.Join(recover, "committed.txt"), "committed\n")
	assertFile(t, filepath.Join(recover, "untracked.txt"), "untracked\n")
	if _, err := os.Stat(filepath.Join(recover, "secret.log")); err == nil {
		t.Error("gitignored secret.log must not be recovered")
	}
}

// TestRunSalvageRestore_NoSalvageFound errors clearly when the prefix is empty.
func TestRunSalvageRestore_NoSalvageFound(t *testing.T) {
	salvageTestEnv(t)
	_, _ = newMemS3(t)
	err := runSalvageRestore(context.Background(), "gd-nope", "gd-nope", t.TempDir(), true)
	if err == nil || !strings.Contains(err.Error(), "no salvage found") {
		t.Fatalf("expected 'no salvage found' error, got: %v", err)
	}
}

// ── recovery assertion helpers ───────────────────────────────────────────────

// assertBundleRecovers clones origin fresh, fetches the (incremental) bundle,
// checks out the salvage ref, and asserts file contents + absences.
func assertBundleRecovers(t *testing.T, root string, bundle []byte, wantFiles map[string]string, wantAbsent []string) {
	t.Helper()
	bundlePath := filepath.Join(t.TempDir(), "sv.bundle")
	if err := os.WriteFile(bundlePath, bundle, 0o644); err != nil {
		t.Fatal(err)
	}
	recover := filepath.Join(t.TempDir(), "recover")
	svGit(t, root, "clone", "-q", filepath.Join(root, "origin"), recover)
	svGit(t, recover, "fetch", bundlePath, "refs/salvage/*:refs/salvage/*")
	// Find the single salvage ref and check it out.
	ref := svGit(t, recover, "for-each-ref", "--format=%(refname)", "refs/salvage/")
	ref = strings.Split(ref, "\n")[0]
	svGit(t, recover, "checkout", "-q", ref)
	for f, want := range wantFiles {
		assertFile(t, filepath.Join(recover, f), want)
	}
	for _, f := range wantAbsent {
		if _, err := os.Stat(filepath.Join(recover, f)); err == nil {
			t.Errorf("file %s should be absent after recovery", f)
		}
	}
}

// assertBundleRecoversStandalone fetches a self-contained bundle into a brand
// new empty repo (no origin) and asserts files.
func assertBundleRecoversStandalone(t *testing.T, bundle []byte, wantFiles map[string]string) {
	t.Helper()
	bundlePath := filepath.Join(t.TempDir(), "solo.bundle")
	if err := os.WriteFile(bundlePath, bundle, 0o644); err != nil {
		t.Fatal(err)
	}
	recover := filepath.Join(t.TempDir(), "recover-solo")
	if err := os.MkdirAll(recover, 0o755); err != nil {
		t.Fatal(err)
	}
	svGit(t, recover, "init", "-q")
	svGit(t, recover, "fetch", bundlePath, "refs/salvage/*:refs/salvage/*")
	ref := svGit(t, recover, "for-each-ref", "--format=%(refname)", "refs/salvage/")
	ref = strings.Split(ref, "\n")[0]
	svGit(t, recover, "checkout", "-q", ref)
	for f, want := range wantFiles {
		assertFile(t, filepath.Join(recover, f), want)
	}
}

func assertFile(t *testing.T, path, want string) {
	t.Helper()
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	if string(got) != want {
		t.Errorf("file %s = %q, want %q", path, got, want)
	}
}
