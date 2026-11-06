package main

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

// harnessOverheadTestSpool points the per-pod spool at a fresh temp file per
// test, so parallel tests never share state, and makes the rotate settle instant.
func harnessOverheadTestSpool(t *testing.T) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "harness-overhead.spool")
	t.Setenv("GARDENIO_HARNESS_OVERHEAD_SPOOL", p)
	t.Setenv("GARDENIO_HARNESS", "") // unset = claude; tests opt into codex
	return p
}

// TestFlushHarnessOverheadSpool_CodexHarnessLabel: a codex pod's series carry
// harness="codex" so the dashboards can split hook-plumbing tax per agent CLI.
func TestFlushHarnessOverheadSpool_CodexHarnessLabel(t *testing.T) {
	harnessOverheadTestSpool(t)
	srv, got := vmagentCapture(t)
	t.Setenv("VMAGENT_URL", srv.URL)
	t.Setenv("KD_AGENT_ID", "gd-agent1")
	t.Setenv("BOAT_AGENT_BEAD_ID", "")
	t.Setenv("GARDENIO_HARNESS", "codex")

	appendHarnessSpool([]harnessObservation{{Phase: "relay", Seconds: 0.42}})
	flushHarnessOverheadSpool(context.Background(), harnessOverheadState{})

	want := `gardenio_tool_harness_overhead_seconds_count{phase="relay",agent="gd-agent1",harness="codex"} 1`
	if body := got(); !strings.Contains(body, want) {
		t.Errorf("push body missing %q\ngot:\n%s", want, body)
	}
}

func TestFlushHarnessOverheadSpool_SingleObservation(t *testing.T) {
	harnessOverheadTestSpool(t)
	srv, got := vmagentCapture(t)
	t.Setenv("VMAGENT_URL", srv.URL)
	t.Setenv("KD_AGENT_ID", "gd-agent1")
	t.Setenv("BOAT_AGENT_BEAD_ID", "")

	appendHarnessSpool([]harnessObservation{{Phase: "relay", Seconds: 0.42}})
	flushHarnessOverheadSpool(context.Background(), harnessOverheadState{})

	body := got()
	for _, want := range []string{
		`gardenio_tool_harness_overhead_seconds_bucket{phase="relay",agent="gd-agent1",harness="claude",le="0.5"} 1`,
		`gardenio_tool_harness_overhead_seconds_bucket{phase="relay",agent="gd-agent1",harness="claude",le="0.25"} 0`,
		`gardenio_tool_harness_overhead_seconds_bucket{phase="relay",agent="gd-agent1",harness="claude",le="+Inf"} 1`,
		`gardenio_tool_harness_overhead_seconds_sum{phase="relay",agent="gd-agent1",harness="claude"} 0.42`,
		`gardenio_tool_harness_overhead_seconds_count{phase="relay",agent="gd-agent1",harness="claude"} 1`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("push body missing %q\ngot:\n%s", want, body)
		}
	}
}

// TestFlushHarnessOverheadSpool_Cumulative: the flusher's state outlives any one
// flush, so each push is the cumulative snapshot (a real monotonic counter),
// not a fresh single observation.
func TestFlushHarnessOverheadSpool_Cumulative(t *testing.T) {
	harnessOverheadTestSpool(t)
	srv, got := vmagentCapture(t)
	t.Setenv("VMAGENT_URL", srv.URL)
	t.Setenv("KD_AGENT_ID", "gd-agent1")

	state := harnessOverheadState{}
	appendHarnessSpool([]harnessObservation{{Phase: "drain_queue", Seconds: 0.03}})
	appendHarnessSpool([]harnessObservation{{Phase: "drain_queue", Seconds: 0.04}})
	flushHarnessOverheadSpool(context.Background(), state)
	appendHarnessSpool([]harnessObservation{{Phase: "drain_queue", Seconds: 2.0}})
	flushHarnessOverheadSpool(context.Background(), state)

	body := got()
	for _, want := range []string{
		`gardenio_tool_harness_overhead_seconds_count{phase="drain_queue",agent="gd-agent1",harness="claude"} 2`, // first push
		`gardenio_tool_harness_overhead_seconds_count{phase="drain_queue",agent="gd-agent1",harness="claude"} 3`, // second, cumulative
		`gardenio_tool_harness_overhead_seconds_bucket{phase="drain_queue",agent="gd-agent1",harness="claude",le="0.05"} 2`,
		`gardenio_tool_harness_overhead_seconds_bucket{phase="drain_queue",agent="gd-agent1",harness="claude",le="1"} 2`,
		`gardenio_tool_harness_overhead_seconds_bucket{phase="drain_queue",agent="gd-agent1",harness="claude",le="2"} 3`,
		`gardenio_tool_harness_overhead_seconds_sum{phase="drain_queue",agent="gd-agent1",harness="claude"} 2.07`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("push bodies missing %q\ngot:\n%s", want, body)
		}
	}
}

// TestFlushHarnessOverheadSpool_OnePushPerBatch: many observations spooled
// between flushes cost one push, with every phase in the snapshot.
func TestFlushHarnessOverheadSpool_OnePushPerBatch(t *testing.T) {
	harnessOverheadTestSpool(t)
	url, pushes := countingVMAgent(t)
	t.Setenv("VMAGENT_URL", url)
	t.Setenv("KD_AGENT_ID", "gd-agent1")

	for i := 0; i < 50; i++ {
		appendHarnessSpool([]harnessObservation{{Phase: "bus_publish", Seconds: 0.1}, {Phase: "relay", Seconds: 0.2}})
	}
	flushHarnessOverheadSpool(context.Background(), harnessOverheadState{})
	if n := pushes(); n != 1 {
		t.Errorf("a batch must be one push, got %d", n)
	}
	// An empty spool pushes nothing.
	flushHarnessOverheadSpool(context.Background(), harnessOverheadState{})
	if n := pushes(); n != 1 {
		t.Errorf("empty spool must not push, got %d total pushes", n)
	}
}

func TestFlushHarnessOverheadSpool_DropsMalformedAndUnknown(t *testing.T) {
	path := harnessOverheadTestSpool(t)
	srv, got := vmagentCapture(t)
	t.Setenv("VMAGENT_URL", srv.URL)
	t.Setenv("KD_AGENT_ID", "gd-agent1")

	junk := "relay -1\nnope 1\nrelay abc\nrelay\n\ntorn-no-newl\nrelay 0.1\n"
	if err := os.WriteFile(path, []byte(junk), 0o600); err != nil {
		t.Fatal(err)
	}
	flushHarnessOverheadSpool(context.Background(), harnessOverheadState{})

	body := got()
	if !strings.Contains(body, `gardenio_tool_harness_overhead_seconds_count{phase="relay",agent="gd-agent1",harness="claude"} 1`) {
		t.Errorf("only the one valid line should count, got:\n%s", body)
	}
	if strings.Contains(body, `phase="nope"`) {
		t.Errorf("unknown phase leaked into a label:\n%s", body)
	}
}

// A rotated spool left by a flusher that died mid-flush is not lost.
func TestFlushHarnessOverheadSpool_RecoversLeftoverRotatedFile(t *testing.T) {
	path := harnessOverheadTestSpool(t)
	srv, got := vmagentCapture(t)
	t.Setenv("VMAGENT_URL", srv.URL)
	t.Setenv("KD_AGENT_ID", "gd-agent1")

	if err := os.WriteFile(path+".flush", []byte("relay 0.1\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	flushHarnessOverheadSpool(context.Background(), harnessOverheadState{})
	if !strings.Contains(got(), `gardenio_tool_harness_overhead_seconds_count{phase="relay",agent="gd-agent1",harness="claude"} 1`) {
		t.Errorf("leftover rotated spool lost, got:\n%s", got())
	}
	if _, err := os.Stat(path + ".flush"); !os.IsNotExist(err) {
		t.Errorf("rotated spool must be removed after folding, stat err = %v", err)
	}
}

// Concurrent hooks append without a lock; nothing may be lost or torn.
func TestAppendHarnessSpool_ConcurrentAppendsLoseNothing(t *testing.T) {
	path := harnessOverheadTestSpool(t)
	var wg sync.WaitGroup
	const writers, each = 20, 50
	for w := 0; w < writers; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < each; i++ {
				appendHarnessSpool([]harnessObservation{{Phase: "bus_publish", Seconds: 0.011}, {Phase: "relay", Seconds: 0.022}})
			}
		}()
	}
	wg.Wait()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if got := len(parseHarnessSpool(data)); got != writers*each*2 {
		t.Errorf("parsed %d observations, want %d (lost or torn lines)", got, writers*each*2)
	}
}

func TestAppendHarnessSpool_CappedAndFailureIsolated(t *testing.T) {
	path := harnessOverheadTestSpool(t)
	if err := os.WriteFile(path, make([]byte, harnessOverheadSpoolMax+1), 0o600); err != nil {
		t.Fatal(err)
	}
	appendHarnessSpool([]harnessObservation{{Phase: "relay", Seconds: 0.1}})
	if fi, _ := os.Stat(path); fi.Size() != harnessOverheadSpoolMax+1 {
		t.Errorf("spool past its cap must drop, size = %d", fi.Size())
	}

	// An unwritable spool is a quiet, fast drop — never a panic or hang.
	t.Setenv("GARDENIO_HARNESS_OVERHEAD_SPOOL", filepath.Join(t.TempDir(), "no-such-dir", "s"))
	start := time.Now()
	appendHarnessSpool([]harnessObservation{{Phase: "relay", Seconds: 0.1}})
	if d := time.Since(start); d > 500*time.Millisecond {
		t.Errorf("unwritable spool took %v", d)
	}
}

func TestFormatBucketLe(t *testing.T) {
	for _, tc := range []struct {
		in   float64
		want string
	}{
		{0.01, "0.01"},
		{0.25, "0.25"},
		{1, "1"},
		{5, "5"},
	} {
		if got := formatBucketLe(tc.in); got != tc.want {
			t.Errorf("formatBucketLe(%v) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

func TestHookHarnessOverheadPhaseValid(t *testing.T) {
	for _, tc := range []struct {
		phase string
		want  bool
	}{
		{"relay", true},
		{"bus_publish", true},
		{"drain_queue", true},
		{"check-mail", false},
		{"", false},
		{"relay ", false},
	} {
		if got := hookHarnessOverheadPhaseValid(tc.phase); got != tc.want {
			t.Errorf("hookHarnessOverheadPhaseValid(%q) = %v, want %v", tc.phase, got, tc.want)
		}
	}
}

func TestHarnessRecorder_InertUnlessArmed(t *testing.T) {
	path := harnessOverheadTestSpool(t)

	rec := newHarnessRecorder()
	rec.observe("bus_publish", time.Millisecond)
	rec.finish("relay") // not armed: SessionStart/Notification etc.
	if _, err := os.Stat(path); !os.IsNotExist(err) || len(rec.obs) != 0 {
		t.Errorf("unarmed recorder must record and spool nothing; stat err=%v obs=%v", err, rec.obs)
	}

	var nilRec *harnessRecorder
	nilRec.arm()
	nilRec.observe("relay", time.Second)
	nilRec.finish("relay") // must not panic
}

func TestHarnessRecorder_FinishSpoolsAllObservationsInOneWrite(t *testing.T) {
	path := harnessOverheadTestSpool(t)

	rec := newHarnessRecorder()
	rec.arm()
	rec.observe("bus_publish", 250*time.Millisecond)
	rec.finish("relay")

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSpace(string(data)), "\n")
	if len(lines) != 2 || lines[0] != "bus_publish 0.25" || !strings.HasPrefix(lines[1], "relay ") {
		t.Errorf("spool = %q, want bus_publish 0.25 then relay <secs>", data)
	}
}

// childPIDs lists processes whose parent is this test process, live or
// zombie. Linux only (reads /proc).
func childPIDs(t *testing.T) []int {
	t.Helper()
	entries, err := os.ReadDir("/proc")
	if err != nil {
		t.Skip("no /proc")
	}
	self := os.Getpid()
	var kids []int
	for _, e := range entries {
		pid, err := strconv.Atoi(e.Name())
		if err != nil {
			continue
		}
		stat, err := os.ReadFile(fmt.Sprintf("/proc/%d/stat", pid))
		if err != nil {
			continue
		}
		// "pid (comm) S ppid ..." — comm may contain spaces/parens, so cut at the last ')'.
		s := string(stat)
		fields := strings.Fields(s[strings.LastIndexByte(s, ')')+1:])
		if len(fields) > 1 && fields[1] == strconv.Itoa(self) {
			kids = append(kids, pid)
		}
	}
	return kids
}

// TestHarnessRecorder_ForksNothing is the regression for the merge-desk
// zombie finding on PR #202: PID 1 in the agent container does not reap
// orphans, so a hook that forks a detached recorder leaks a zombie per tool
// call until pids.max is hit. Reporting must be pure file I/O — N tool calls
// leave no child process (live or zombie) behind.
func TestHarnessRecorder_ForksNothing(t *testing.T) {
	harnessOverheadTestSpool(t)
	t.Setenv("VMAGENT_URL", "http://127.0.0.1:1") // would be slow/refused if the hook pushed
	before := len(childPIDs(t))

	start := time.Now()
	for i := 0; i < 200; i++ {
		rec := newHarnessRecorder()
		rec.arm()
		rec.observe("bus_publish", time.Millisecond)
		rec.finish("relay")
	}
	if d := time.Since(start); d > 2*time.Second {
		t.Errorf("200 foreground reports took %v; reporting must not touch the network", d)
	}
	if after := len(childPIDs(t)); after != before {
		t.Errorf("hook reporting left %d new child process(es); it must not fork", after-before)
	}
}

// TestRunHarnessOverheadFlusher_FlushesOnTickAndOnShutdown covers the
// init-hosted loop: periodic push, plus a final drain when ctx is cancelled.
func TestRunHarnessOverheadFlusher_FlushesOnTickAndOnShutdown(t *testing.T) {
	harnessOverheadTestSpool(t)
	srv, got := vmagentCapture(t)
	t.Setenv("VMAGENT_URL", srv.URL)
	t.Setenv("KD_AGENT_ID", "gd-agent1")

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { runHarnessOverheadFlusher(ctx, 50*time.Millisecond); close(done) }()

	appendHarnessSpool([]harnessObservation{{Phase: "relay", Seconds: 0.1}})
	waitFor(t, func() bool {
		return strings.Contains(got(), `gardenio_tool_harness_overhead_seconds_count{phase="relay",agent="gd-agent1",harness="claude"} 1`)
	})

	// Appended just before shutdown: the final drain must still deliver it.
	appendHarnessSpool([]harnessObservation{{Phase: "relay", Seconds: 0.2}})
	cancel()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("flusher did not stop on ctx cancel")
	}
	if !strings.Contains(got(), `gardenio_tool_harness_overhead_seconds_count{phase="relay",agent="gd-agent1",harness="claude"} 2`) {
		t.Errorf("final drain lost the last observation, got:\n%s", got())
	}
}

func waitFor(t *testing.T, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatal("condition not met in time")
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// countingVMAgent returns a server that counts pushes it receives.
func countingVMAgent(t *testing.T) (url string, pushes func() int) {
	t.Helper()
	var mu sync.Mutex
	n := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		n++
		mu.Unlock()
		w.WriteHeader(http.StatusNoContent)
	}))
	t.Cleanup(srv.Close)
	return srv.URL, func() int { mu.Lock(); defer mu.Unlock(); return n }
}
