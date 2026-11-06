package k3sauditshipper

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

func appendFile(t *testing.T, p, s string) {
	t.Helper()
	f, err := os.OpenFile(p, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if _, err := f.WriteString(s); err != nil {
		t.Fatal(err)
	}
}

func drain(t *testing.T, tl *tailer) []string {
	t.Helper()
	var out []string
	for {
		line, _, err := tl.next()
		if errors.Is(err, errNoData) {
			return out
		}
		if err != nil {
			t.Fatal(err)
		}
		out = append(out, string(line))
	}
}

func TestTailer_PartialLineWaitsForNewline(t *testing.T) {
	p := filepath.Join(t.TempDir(), "audit.log")
	appendFile(t, p, "one\ntw")
	tl, _ := newTailer(p, 1024, Checkpoint{}, false, true)
	defer tl.close()
	if got := drain(t, tl); strings.Join(got, ",") != "one" {
		t.Fatalf("got %v, want [one]", got)
	}
	appendFile(t, p, "o\nthree\n")
	if got := drain(t, tl); strings.Join(got, ",") != "two,three" {
		t.Fatalf("got %v, want [two three]", got)
	}
}

func TestTailer_FollowsRotationWithoutLoss(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "audit.log")
	appendFile(t, p, "a1\na2\n")
	tl, _ := newTailer(p, 1024, Checkpoint{}, false, true)
	defer tl.close()
	if got := drain(t, tl); len(got) != 2 {
		t.Fatalf("got %v", got)
	}
	// Lines land after our last read, then the apiserver rotates.
	appendFile(t, p, "a3\n")
	if err := os.Rename(p, filepath.Join(dir, "audit-2026-10-15T01-00-00.000.log")); err != nil {
		t.Fatal(err)
	}
	appendFile(t, p, "b1\nb2\n")
	if got := strings.Join(drain(t, tl), ","); got != "a3,b1,b2" {
		t.Fatalf("got %s, want a3,b1,b2", got)
	}
	appendFile(t, p, "b3\n")
	if got := strings.Join(drain(t, tl), ","); got != "b3" {
		t.Fatalf("got %s, want b3", got)
	}
}

func TestTailer_WaitsForMissingFile(t *testing.T) {
	p := filepath.Join(t.TempDir(), "audit.log")
	tl, err := newTailer(p, 1024, Checkpoint{}, false, true)
	if err != nil {
		t.Fatal(err)
	}
	defer tl.close()
	if got := drain(t, tl); len(got) != 0 {
		t.Fatalf("got %v", got)
	}
	appendFile(t, p, "x\n")
	if got := strings.Join(drain(t, tl), ","); got != "x" {
		t.Fatalf("got %q", got)
	}
}

func TestTailer_OverlongLineSkippedPositionAdvances(t *testing.T) {
	p := filepath.Join(t.TempDir(), "audit.log")
	appendFile(t, p, "ok\n"+strings.Repeat("z", 300000)+"\nafter\n")
	tl, _ := newTailer(p, 1000, Checkpoint{}, false, true)
	defer tl.close()
	var got []string
	skipped := 0
	for {
		line, sk, err := tl.next()
		if errors.Is(err, errNoData) {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		if sk {
			skipped++
			continue
		}
		got = append(got, string(line))
	}
	if skipped != 1 || strings.Join(got, ",") != "ok,after" {
		t.Fatalf("skipped=%d got=%v", skipped, got)
	}
}

func TestTailer_StartAtEndIgnoresHistory(t *testing.T) {
	p := filepath.Join(t.TempDir(), "audit.log")
	appendFile(t, p, "old\n")
	tl, _ := newTailer(p, 1024, Checkpoint{}, false, false)
	defer tl.close()
	appendFile(t, p, "new\n")
	if got := strings.Join(drain(t, tl), ","); got != "new" {
		t.Fatalf("got %q", got)
	}
}

func TestTailer_ResumesFromCheckpointAcrossRotation(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "audit.log")
	appendFile(t, p, "a1\na2\na3\n")
	tl, _ := newTailer(p, 1024, Checkpoint{}, false, true)
	line, _, _ := tl.next() // a1
	if string(line) != "a1" {
		t.Fatal(line)
	}
	cp := tl.position()
	tl.close()

	// Shipper down: the file rotates twice and the live file gets new data.
	if err := os.Rename(p, filepath.Join(dir, "audit-2026-10-15T01-00-00.000.log")); err != nil {
		t.Fatal(err)
	}
	appendFile(t, p, "b1\n")
	if err := os.Rename(p, filepath.Join(dir, "audit-2026-10-15T02-00-00.000.log")); err != nil {
		t.Fatal(err)
	}
	appendFile(t, p, "c1\n")

	tl2, err := newTailer(p, 1024, cp, true, true)
	if err != nil {
		t.Fatal(err)
	}
	defer tl2.close()
	if got := strings.Join(drain(t, tl2), ","); got != "a2,a3,b1,c1" {
		t.Fatalf("got %s, want a2,a3,b1,c1", got)
	}
}

func TestTailer_ResumeWhenCheckpointedFileIsLive(t *testing.T) {
	p := filepath.Join(t.TempDir(), "audit.log")
	appendFile(t, p, "a1\na2\n")
	tl, _ := newTailer(p, 1024, Checkpoint{}, false, true)
	tl.next()
	cp := tl.position()
	tl.close()
	appendFile(t, p, "a3\n")
	tl2, err := newTailer(p, 1024, cp, true, true)
	if err != nil {
		t.Fatal(err)
	}
	defer tl2.close()
	if got := strings.Join(drain(t, tl2), ","); got != "a2,a3" {
		t.Fatalf("got %s", got)
	}
}

func TestTailer_CheckpointedFileGoneReadsEverythingLeft(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "audit.log")
	appendFile(t, p, "old\n")
	tl, _ := newTailer(p, 1024, Checkpoint{}, false, true)
	tl.next()
	cp := tl.position()
	tl.close()
	os.Remove(p)
	appendFile(t, p, "n1\nn2\n")
	tl2, err := newTailer(p, 1024, cp, true, true)
	if err != nil {
		t.Fatal(err)
	}
	defer tl2.close()
	if got := strings.Join(drain(t, tl2), ","); got != "n1,n2" {
		t.Fatalf("got %s", got)
	}
}

func TestTailer_TruncationRestarts(t *testing.T) {
	p := filepath.Join(t.TempDir(), "audit.log")
	appendFile(t, p, "aaaaaaaa\nbbbbbbbb\n")
	tl, _ := newTailer(p, 1024, Checkpoint{}, false, true)
	defer tl.close()
	drain(t, tl)
	if err := os.WriteFile(p, []byte("c\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	drain(t, tl) // notices the shrink and rewinds
	if got := strings.Join(drain(t, tl), ","); got != "c" {
		t.Fatalf("got %q", got)
	}
}

// --- shipper ---------------------------------------------------------------

type recSink struct {
	mu      sync.Mutex
	batches []string
	fail    int
}

func (s *recSink) Insert(_ context.Context, b []byte) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.fail > 0 {
		s.fail--
		return fmt.Errorf("sink down")
	}
	s.batches = append(s.batches, string(b))
	return nil
}

func (s *recSink) all() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return strings.Join(s.batches, "")
}

func ev(id string) string {
	return fmt.Sprintf(`{"kind":"Event","auditID":%q,"verb":"create","stageTimestamp":"2026-10-15T01:00:00Z","user":{"username":"u"},"objectRef":{"resource":"pods","name":"p"}}`+"\n", id)
}

func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

func fastCfg(dir string) Config {
	return Config{
		Path: filepath.Join(dir, "audit.log"), StatePath: filepath.Join(dir, "state.json"),
		FromStart: true, PollInterval: 5 * time.Millisecond, FlushInterval: 10 * time.Millisecond,
		RetryBackoff: 10 * time.Millisecond, Options: Options{Node: "n1"},
	}
}

func TestShipper_RetriesFailedBatchWithoutLossOrAdvance(t *testing.T) {
	dir := t.TempDir()
	cfg := fastCfg(dir)
	appendFile(t, cfg.Path, ev("1")+"not json\n"+ev("2"))
	sink := &recSink{fail: 2}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- New(cfg, sink, nil).Run(ctx) }()

	waitFor(t, "both records delivered", func() bool {
		a := sink.all()
		return strings.Contains(a, `"auditID":"1"`) && strings.Contains(a, `"auditID":"2"`)
	})
	// Each record exactly once despite the two failed attempts.
	if n := strings.Count(sink.all(), `"auditID":"1"`); n != 1 {
		t.Errorf("record 1 delivered %d times", n)
	}
	waitFor(t, "checkpoint saved", func() bool { _, ok := loadCheckpoint(cfg.StatePath); return ok })
	cancel()
	<-done
	cp, _ := loadCheckpoint(cfg.StatePath)
	fi, _ := os.Stat(cfg.Path)
	if cp.Offset != fi.Size() || cp.Inode != inodeOf(fi) {
		t.Errorf("checkpoint %+v, want offset %d inode %d", cp, fi.Size(), inodeOf(fi))
	}
}

func TestShipper_RestartResumesWithoutDuplicates(t *testing.T) {
	dir := t.TempDir()
	cfg := fastCfg(dir)
	appendFile(t, cfg.Path, ev("1")+ev("2"))
	run := func(sink *recSink, want string) {
		ctx, cancel := context.WithCancel(context.Background())
		done := make(chan error, 1)
		go func() { done <- New(cfg, sink, nil).Run(ctx) }()
		waitFor(t, want, func() bool { return strings.Contains(sink.all(), want) })
		time.Sleep(100 * time.Millisecond) // let the checkpoint commit
		cancel()
		<-done
	}
	s1 := &recSink{}
	run(s1, `"auditID":"2"`)
	appendFile(t, cfg.Path, ev("3"))
	s2 := &recSink{}
	run(s2, `"auditID":"3"`)
	if strings.Contains(s2.all(), `"auditID":"1"`) || strings.Contains(s2.all(), `"auditID":"2"`) {
		t.Errorf("restart re-shipped old records: %s", s2.all())
	}
}
