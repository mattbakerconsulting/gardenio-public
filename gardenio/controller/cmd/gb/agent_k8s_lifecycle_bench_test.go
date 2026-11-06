package main

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// setupResumeFixture builds a Claude state dir with n historical .jsonl files
// in a single project subdir. Files are sized > 256B (so they'd otherwise be
// resume candidates) and given staggered mtimes so the most-recent ordering is
// well-defined. session-env/<uuid>/ is populated for each so v2.1+ validation
// passes for every file.
func setupResumeFixture(tb testing.TB, n int) string {
	tb.Helper()
	dir := tb.TempDir()
	projectDir := filepath.Join(dir, "projects", "-test-project")
	if err := os.MkdirAll(projectDir, 0o755); err != nil {
		tb.Fatal(err)
	}
	envRoot := filepath.Join(dir, "session-env")
	if err := os.MkdirAll(envRoot, 0o755); err != nil {
		tb.Fatal(err)
	}
	payload := bigJsonl()
	base := time.Now().Add(-time.Duration(n) * time.Second)
	for i := range n {
		uuid := fmt.Sprintf("session-%08d", i)
		logPath := filepath.Join(projectDir, uuid+".jsonl")
		if err := os.WriteFile(logPath, payload, 0o644); err != nil {
			tb.Fatal(err)
		}
		mt := base.Add(time.Duration(i) * time.Second)
		if err := os.Chtimes(logPath, mt, mt); err != nil {
			tb.Fatal(err)
		}
		if err := os.MkdirAll(filepath.Join(envRoot, uuid), 0o755); err != nil {
			tb.Fatal(err)
		}
	}
	return dir
}

func BenchmarkFindResumeSession(b *testing.B) {
	for _, n := range []int{10, 100, 1000, 10000} {
		b.Run(fmt.Sprintf("files=%d", n), func(b *testing.B) {
			dir := setupResumeFixture(b, n)
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				_ = findResumeSession(dir, true, true)
			}
		})
	}
}
