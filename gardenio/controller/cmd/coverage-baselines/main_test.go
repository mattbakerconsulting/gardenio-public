package main

import (
	"os"
	"path/filepath"
	"testing"
)

func TestLoadCommitsEmptyPath(t *testing.T) {
	t.Parallel()
	m, err := loadCommits("")
	if err != nil {
		t.Fatalf("loadCommits(\"\"): %v", err)
	}
	if len(m) != 0 {
		t.Errorf("got %d entries, want empty map for empty path", len(m))
	}
}

func TestLoadCommitsMissingFile(t *testing.T) {
	t.Parallel()
	_, err := loadCommits(filepath.Join(t.TempDir(), "nope.tsv"))
	if err == nil {
		t.Error("expected error for missing file")
	}
}

func TestLoadCommitsParsing(t *testing.T) {
	t.Parallel()
	content := "" +
		"# a comment line\n" +
		"\n" + // blank line
		"123\tabc123\n" +
		"456\tdef456\n" +
		"  789\tghi789  \n" + // surrounding whitespace trimmed
		"malformed-no-tab\n" + // dropped (no tab)
		"999\tsha\twith\ttabs\n" // SplitN(2) keeps remainder intact

	path := filepath.Join(t.TempDir(), "commits.tsv")
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}

	m, err := loadCommits(path)
	if err != nil {
		t.Fatalf("loadCommits: %v", err)
	}

	want := map[string]string{
		"123": "abc123",
		"456": "def456",
		"789": "ghi789",
		"999": "sha\twith\ttabs",
	}
	if len(m) != len(want) {
		t.Fatalf("got %d entries, want %d: %#v", len(m), len(want), m)
	}
	for k, v := range want {
		if m[k] != v {
			t.Errorf("m[%q] = %q, want %q", k, m[k], v)
		}
	}
	if _, ok := m["malformed-no-tab"]; ok {
		t.Error("line without a tab should be dropped")
	}
}

func TestLoadSamplesNoFiles(t *testing.T) {
	t.Parallel()
	samples, err := loadSamples(t.TempDir(), map[string]string{})
	if err != nil {
		t.Fatalf("loadSamples: %v", err)
	}
	if len(samples) != 0 {
		t.Errorf("got %d samples, want 0 for empty dir", len(samples))
	}
}

func TestLoadSamplesSortedAndCommitMapped(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	// Minimal valid LCOV: one source file with two lines, one covered.
	lcovBody := "SF:gardenio/controller/x.go\nDA:1,1\nDA:2,0\nend_of_record\n"
	// Write out of lexical order to prove sorting.
	for _, id := range []string{"300", "100", "200"} {
		if err := os.WriteFile(filepath.Join(dir, id+".lcov"), []byte(lcovBody), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	// A non-lcov file must be ignored by the glob.
	if err := os.WriteFile(filepath.Join(dir, "notes.txt"), []byte("ignore me"), 0o644); err != nil {
		t.Fatal(err)
	}

	commits := map[string]string{"100": "sha100", "200": "sha200"} // 300 intentionally missing

	samples, err := loadSamples(dir, commits)
	if err != nil {
		t.Fatalf("loadSamples: %v", err)
	}
	if len(samples) != 3 {
		t.Fatalf("got %d samples, want 3", len(samples))
	}

	// Sorted by glob path → "100","200","300".
	wantOrder := []string{"100", "200", "300"}
	for i, s := range samples {
		if s.Pipeline != wantOrder[i] {
			t.Errorf("samples[%d].Pipeline = %q, want %q", i, s.Pipeline, wantOrder[i])
		}
		if s.LCOV == nil {
			t.Errorf("samples[%d].LCOV is nil", i)
		}
	}

	// Commit mapping: present → mapped; absent → "unknown".
	got := map[string]string{}
	for _, s := range samples {
		got[s.Pipeline] = s.Commit
	}
	if got["100"] != "sha100" || got["200"] != "sha200" {
		t.Errorf("commit mapping wrong: %#v", got)
	}
	if got["300"] != "unknown" {
		t.Errorf("missing commit should map to 'unknown', got %q", got["300"])
	}
}

func TestLoadSamplesParseErrorPropagates(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	// A directory named like an .lcov file makes ParseFile's os.Open fail
	// (open on a directory then read errors), exercising the error path.
	if err := os.Mkdir(filepath.Join(dir, "bad.lcov"), 0o755); err != nil {
		t.Fatal(err)
	}
	_, err := loadSamples(dir, map[string]string{})
	if err == nil {
		t.Error("expected parse error to propagate when an .lcov entry is unreadable")
	}
}
