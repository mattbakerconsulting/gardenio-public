package commons

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	"gardenio/controller/internal/commons/commonstest"
)

// hookEvent is a gb hook relay payload for agent's tool call id.
func hookEvent(event, agent, id string, extra map[string]any) []byte {
	e := map[string]any{"event": event, "agent": "crew", "agent_bead_id": agent, "tool_use_id": id,
		"ts": t0.Add(time.Minute).Format(time.RFC3339Nano)}
	for k, v := range extra {
		e[k] = v
	}
	b, _ := json.Marshal(e)
	return b
}

// observeAll feeds the record's worker path synchronously.
func observeAll(j *Jobs, events ...[]byte) {
	for _, e := range events {
		j.observe(context.Background(), e)
	}
}

// TestJobsRecord pins the jobs record (gd-qlk31NBV6o): a PreToolUse starts a
// job and its PostToolUse finishes it with exit 0, duration_ms, output bytes
// and the first lines of output; a failure's exit code is read from its error;
// secrets in the command and the output are redacted.
func TestJobsRecord(t *testing.T) {
	d := commonstest.TwoLands(t0)
	commonstest.Workbench(d, t0)
	j := NewJobs(d, nil, nil)
	agent := commonstest.WorkbenchAgent
	// Built at runtime so no token-shaped literal is committed (gitleaks).
	ghToken := "ghp_" + strings.Repeat("abcdef0123", 4)[:36]
	observeAll(j,
		hookEvent("PreToolUse", agent, "tu1", map[string]any{"tool_name": "Bash",
			"tool_input": map[string]any{"command": "curl -H 'Authorization: token " + ghToken + "' https://api.github.com"}}),
		hookEvent("PostToolUse", agent, "tu1", map[string]any{"tool_name": "Bash", "duration_ms": 820, "output_bytes": 5120,
			"tool_response": map[string]any{"stdout": "line one\n\nline two\nline three\nline four\nline five", "stderr": ""}}),
		hookEvent("PreToolUse", agent, "tu2", map[string]any{"tool_name": "Bash", "tool_input": map[string]any{"command": "go test ./x"}}),
		hookEvent("PostToolUseFailure", agent, "tu2", map[string]any{"tool_name": "Bash", "error": "Exit code 2\n--- FAIL: TestX\nexport ANTHROPIC_API_KEY=sk-ant-abc123DEF456"}),
		hookEvent("PreToolUse", agent, "tu3", map[string]any{"tool_name": "Read", "tool_input": map[string]any{"file_path": "/repo/node.go"}}),
	)
	jobs, n := j.Of(agent)
	if n != 3 || len(jobs) != 3 {
		t.Fatalf("want 3 jobs, got %d (%d): %+v", len(jobs), n, jobs)
	}
	read, failed, ran := jobs[0], jobs[1], jobs[2]
	if !read.Running() || read.Command != "/repo/node.go" || read.Exit != nil {
		t.Errorf("a started Read: %+v", read)
	}
	if ran.Running() || ran.Failed() || ran.Exit == nil || *ran.Exit != 0 || ran.DurationMs != 820 || ran.OutBytes != 5120 {
		t.Errorf("a finished Bash: %+v", ran)
	}
	if got := strings.Join(ran.Head, "|"); got != "line one|line two|line three|line four" {
		t.Errorf("head: %q", got)
	}
	if strings.Contains(ran.Command, ghToken[4:]) || !strings.Contains(ran.Command, "api.github.com") {
		t.Errorf("command not redacted: %q", ran.Command)
	}
	if !failed.Failed() || failed.Exit == nil || *failed.Exit != 2 || len(failed.Head) != 2 || failed.Head[0] != "--- FAIL: TestX" {
		t.Errorf("a failed Bash: %+v", failed)
	}
	if strings.Contains(strings.Join(failed.Head, "\n"), "sk-ant-abc") {
		t.Errorf("output not redacted: %q", failed.Head)
	}
	// A repeat Post changes nothing.
	observeAll(j, hookEvent("PostToolUse", agent, "tu1", map[string]any{"tool_name": "Bash", "duration_ms": 1}))
	if jobs, _ := j.Of(agent); jobs[2].DurationMs != 820 {
		t.Errorf("a repeat finish rewrote the job: %+v", jobs[2])
	}
}

// TestJobsCutResponse: a response the relay cut to 1024 bytes arrives as the
// start of its JSON text; its stdout is still read, and the command of a cut
// tool_input.
func TestJobsCutResponse(t *testing.T) {
	d := commonstest.TwoLands(t0)
	commonstest.Workbench(d, t0)
	j := NewJobs(d, nil, nil)
	full, _ := json.Marshal(map[string]any{"interrupted": false, "stderr": "", "stdout": "ok  \tgardenio/x\t0.1s\n" + strings.Repeat("y", 2000)})
	in, _ := json.Marshal(map[string]any{"command": "make lint && echo \"done\"", "description": strings.Repeat("d", 2000)})
	observeAll(j, hookEvent("PostToolUse", commonstest.WorkbenchAgent, "tu9", map[string]any{"tool_name": "Bash",
		"tool_input": string(in[:1024]) + "...", "tool_response": string(full[:1024]) + "..."}))
	jobs, _ := j.Of(commonstest.WorkbenchAgent)
	if len(jobs) != 1 || jobs[0].Command != `make lint && echo "done"` || len(jobs[0].Head) == 0 || jobs[0].Head[0] != "ok  \tgardenio/x\t0.1s" {
		t.Fatalf("cut response: %+v", jobs)
	}
}

// TestJobsDropsWhatIsNotAnAgent: the land is read from the agent bead, never
// the payload; a bead that is not an agent, or is not there, records nothing.
func TestJobsDropsWhatIsNotAnAgent(t *testing.T) {
	d := commonstest.TwoLands(t0)
	commonstest.Workbench(d, t0)
	j := NewJobs(d, nil, nil)
	for _, id := range []string{"cl-s3", "cl-missing"} {
		observeAll(j, hookEvent("PreToolUse", id, "tu1", map[string]any{"tool_name": "Bash", "project": "claimlayer"}))
		if jobs, n := j.Of(id); len(jobs) != 0 || n != 0 {
			t.Errorf("%s: recorded %+v", id, jobs)
		}
	}
}

// TestJobsRingKeepsTheNewest: an agent's ring keeps its last jobsKept jobs and
// counts every one.
func TestJobsRingKeepsTheNewest(t *testing.T) {
	d := commonstest.TwoLands(t0)
	commonstest.Workbench(d, t0)
	j := NewJobs(d, nil, nil)
	for i := range jobsKept + 5 {
		observeAll(j, hookEvent("PostToolUse", commonstest.WorkbenchAgent, fmt.Sprintf("tu%d", i), map[string]any{"tool_name": "Grep"}))
	}
	jobs, n := j.Of(commonstest.WorkbenchAgent)
	if n != jobsKept+5 || len(jobs) != jobsKept || jobs[0].ID != fmt.Sprintf("tu%d", jobsKept+4) || jobs[jobsKept-1].ID != "tu5" {
		t.Fatalf("ring: n=%d len=%d first=%s last=%s", n, len(jobs), jobs[0].ID, jobs[len(jobs)-1].ID)
	}
	if got := j.Count(commonstest.WorkbenchAgent); got != n {
		t.Errorf("Count %d, Of %d", got, n)
	}
}

func TestJobDuration(t *testing.T) {
	for ms, want := range map[int64]string{0: "0 ms", 820: "820 ms", 3400: "3.4 s", 125_000: "2m 5s"} {
		if got := jobDuration(ms); got != want {
			t.Errorf("jobDuration(%d) = %q, want %q", ms, got, want)
		}
	}
}

// TestNilJobs: a nil record observes nothing and serves no jobs.
func TestNilJobs(t *testing.T) {
	var j *Jobs
	j.Start()
	j.Observe([]byte(`{}`))
	j.Close()
	if jobs, n := j.Of("x"); jobs != nil || n != 0 || j.Count("x") != 0 {
		t.Fatal("a nil record served jobs")
	}
	if NewJobs(nil, nil, nil) != nil {
		t.Fatal("NewJobs without a reader")
	}
}
