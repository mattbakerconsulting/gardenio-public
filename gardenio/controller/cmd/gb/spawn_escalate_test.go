package main

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"gardenio/controller/internal/beadsapi"
)

// captureSpawnStderr runs fn with os.Stderr redirected to a pipe and returns
// what it printed. reportSpawnEscalation's WARNING lines go to os.Stderr
// directly (mirroring captureSpawnStdout's rationale for the SPAWNED line).
func captureSpawnStderr(t *testing.T, fn func()) string {
	t.Helper()
	old := os.Stderr
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	os.Stderr = w
	done := make(chan string, 1)
	go func() {
		var buf bytes.Buffer
		_, _ = io.Copy(&buf, r)
		done <- buf.String()
	}()
	fn()
	w.Close()
	os.Stderr = old
	return <-done
}

// TestRunSpawn_EscalateFlagFilesEscalation is the core `--escalate` regression
// (gd-lsgF8rL4Zm): when the daemon denies a privileged-role spawn as
// escalatable and the caller sent X-Escalate: true, the daemon auto-files an
// escalation bead and responds 202 instead of 403. `gb spawn --escalate` must
// treat that as SUCCESS (nil error, exit 0) and print the machine-greppable
// `ESCALATED ...` contract line — not surface it as a spawn failure.
func TestRunSpawn_EscalateFlagFilesEscalation(t *testing.T) {
	t.Setenv("BOAT_PROJECT", "gardenio")
	t.Setenv("KD_PROJECT", "")
	// No agent identity resolvable — isolates the "no caller task" branch of
	// reportSpawnEscalation from the requesting_task-homing branch (covered
	// separately below).
	t.Setenv("KD_AGENT_ID", "")
	t.Setenv("BOAT_AGENT_BEAD_ID", "")
	t.Setenv("KD_ACTOR", "")

	var sawEscalateHeader bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.Method == http.MethodGet && strings.HasPrefix(r.URL.Path, "/v1/agents-by-name/"):
			http.Error(w, `{"error":"not found"}`, http.StatusNotFound)
		case r.Method == http.MethodGet && r.URL.Path == "/v1/beads":
			_ = json.NewEncoder(w).Encode(map[string]any{"beads": []any{}})
		case r.URL.Path == "/v1/beads" && r.Method == http.MethodPost:
			sawEscalateHeader = r.Header.Get("X-Escalate") == "true"
			w.WriteHeader(http.StatusAccepted)
			_ = json.NewEncoder(w).Encode(map[string]string{
				"escalation_id": "gd-ESCSPAWN1",
				"message":       "role k8s-live is privileged; a non-human caller must escalate",
				"denied_action": "create",
				"bead_type":     "agent",
			})
		default:
			t.Errorf("unexpected daemon call: %s %s", r.Method, r.URL.Path)
			_ = json.NewEncoder(w).Encode(map[string]string{})
		}
	}))
	t.Cleanup(srv.Close)

	c, err := beadsapi.New(beadsapi.Config{HTTPAddr: srv.URL})
	if err != nil {
		t.Fatal(err)
	}
	oldDaemon := daemon
	daemon = c
	t.Cleanup(func() { daemon = oldDaemon })

	var runErr error
	var stdout string
	stderr := captureSpawnStderr(t, func() {
		stdout = captureSpawnStdout(t, func() {
			_, _, runErr = newSpawnTestCmd(t, []string{"--role", "k8s-live", "--escalate"})
		})
	})
	if runErr != nil {
		t.Fatalf("gb spawn --escalate on an escalatable denial must succeed (nil error), got: %v\nstdout: %s\nstderr: %s", runErr, stdout, stderr)
	}
	if !sawEscalateHeader {
		t.Fatal("expected the create POST to carry X-Escalate: true")
	}
	first := strings.SplitN(stdout, "\n", 2)[0]
	if !strings.HasPrefix(first, "ESCALATED ") {
		t.Errorf("first stdout line must be the ESCALATED success token, got: %q", first)
	}
	for _, want := range []string{"escalation=gd-ESCSPAWN1", "role=k8s-live", "project=gardenio"} {
		if !strings.Contains(first, want) {
			t.Errorf("ESCALATED line missing %q, got: %q", want, first)
		}
	}
	if !strings.Contains(stderr, "no caller task to link") {
		t.Errorf("expected the orphan-escalation warning on stderr, got: %q", stderr)
	}
}

// TestRunSpawn_EscalateFlagHomesToCallerTask covers the linking half of the
// acceptance criteria: when the caller's own agent bead resolves a task_id,
// the auto-filed escalation must be homed to it (requesting_task field +
// blocks-edge) and a comment posted — the same convention `gb escalation
// create` uses — so the escalation surfaces on /plans and `gb news` instead
// of landing as an orphan in the flat /escalations inbox.
func TestRunSpawn_EscalateFlagHomesToCallerTask(t *testing.T) {
	t.Setenv("BOAT_PROJECT", "gardenio")
	t.Setenv("KD_PROJECT", "")
	t.Setenv("KD_AGENT_ID", "gd-CALLERAGENT")
	t.Setenv("BOAT_AGENT_BEAD_ID", "")

	const callerTask = "gd-CALLERTASK"
	var (
		sawFieldsPatch  bool
		patchedField    string
		sawDependency   bool
		depDependsOn    string
		depType         string
		sawComment      bool
		commentBeadPath string
	)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.Method == http.MethodGet && strings.HasPrefix(r.URL.Path, "/v1/agents-by-name/"):
			http.Error(w, `{"error":"not found"}`, http.StatusNotFound)
		case r.Method == http.MethodGet && r.URL.Path == "/v1/beads":
			_ = json.NewEncoder(w).Encode(map[string]any{"beads": []any{}})
		case r.URL.Path == "/v1/beads" && r.Method == http.MethodPost:
			w.WriteHeader(http.StatusAccepted)
			_ = json.NewEncoder(w).Encode(map[string]string{
				"escalation_id": "gd-ESCSPAWN2",
				"message":       "role k8s-live is privileged; a non-human caller must escalate",
				"denied_action": "create",
				"bead_type":     "agent",
			})
		// deriveRequestingTask -> GetBead(caller agent bead).
		case r.Method == http.MethodGet && r.URL.Path == "/v1/beads/gd-CALLERAGENT":
			_ = json.NewEncoder(w).Encode(map[string]any{
				"id": "gd-CALLERAGENT", "title": "caller", "type": "agent",
				"kind": "config", "status": "open",
				"fields": map[string]string{"task_id": callerTask},
			})
		// UpdateBeadFields's read-modify-write GET on the escalation bead.
		case r.Method == http.MethodGet && r.URL.Path == "/v1/beads/gd-ESCSPAWN2":
			_ = json.NewEncoder(w).Encode(map[string]any{
				"id": "gd-ESCSPAWN2", "title": "spawn escalation", "type": "escalation",
				"kind": "issue", "status": "open",
				"fields": map[string]any{},
			})
		// resolveOwningPlan(callerTask) -> GetBead + GetDependencies walk.
		case r.Method == http.MethodGet && r.URL.Path == "/v1/beads/"+callerTask:
			_ = json.NewEncoder(w).Encode(map[string]any{
				"id": callerTask, "title": "caller's own task", "type": "task",
				"kind": "issue", "status": "in_progress",
			})
		case r.Method == http.MethodGet && r.URL.Path == "/v1/beads/"+callerTask+"/dependencies":
			_ = json.NewEncoder(w).Encode(map[string]any{"dependencies": []any{}})
		case r.Method == http.MethodPatch && r.URL.Path == "/v1/beads/gd-ESCSPAWN2":
			sawFieldsPatch = true
			var body struct {
				Fields json.RawMessage `json:"fields"`
			}
			_ = json.NewDecoder(r.Body).Decode(&body)
			var fields map[string]any
			_ = json.Unmarshal(body.Fields, &fields)
			if v, ok := fields["requesting_task"]; ok {
				patchedField, _ = v.(string)
			}
			_ = json.NewEncoder(w).Encode(map[string]string{})
		case r.Method == http.MethodPost && r.URL.Path == "/v1/beads/"+callerTask+"/dependencies":
			sawDependency = true
			var body struct {
				DependsOnID string `json:"depends_on_id"`
				Type        string `json:"type"`
			}
			_ = json.NewDecoder(r.Body).Decode(&body)
			depDependsOn = body.DependsOnID
			depType = body.Type
			_ = json.NewEncoder(w).Encode(map[string]string{})
		case r.Method == http.MethodPost && r.URL.Path == "/v1/beads/"+callerTask+"/comments":
			sawComment = true
			commentBeadPath = r.URL.Path
			_ = json.NewEncoder(w).Encode(map[string]string{})
		default:
			t.Errorf("unexpected daemon call: %s %s", r.Method, r.URL.Path)
			_ = json.NewEncoder(w).Encode(map[string]string{})
		}
	}))
	t.Cleanup(srv.Close)

	c, err := beadsapi.New(beadsapi.Config{HTTPAddr: srv.URL})
	if err != nil {
		t.Fatal(err)
	}
	oldDaemon := daemon
	daemon = c
	t.Cleanup(func() { daemon = oldDaemon })

	var runErr error
	stdout := captureSpawnStdout(t, func() {
		_, _, runErr = newSpawnTestCmd(t, []string{"--role", "k8s-live", "--escalate"})
	})
	if runErr != nil {
		t.Fatalf("gb spawn --escalate must succeed, got: %v\nstdout: %s", runErr, stdout)
	}
	if !sawFieldsPatch || patchedField != callerTask {
		t.Errorf("expected escalation to be patched with requesting_task=%s, sawPatch=%v got=%q", callerTask, sawFieldsPatch, patchedField)
	}
	if !sawDependency || depDependsOn != "gd-ESCSPAWN2" || depType != "blocks" {
		t.Errorf("expected a blocks-edge from %s -> gd-ESCSPAWN2, got sawDependency=%v dependsOn=%q type=%q", callerTask, sawDependency, depDependsOn, depType)
	}
	if !sawComment || commentBeadPath != "/v1/beads/"+callerTask+"/comments" {
		t.Errorf("expected a comment on caller task %s, got sawComment=%v path=%q", callerTask, sawComment, commentBeadPath)
	}
	if !strings.Contains(stdout, "ESCALATED escalation=gd-ESCSPAWN2") {
		t.Errorf("expected ESCALATED line, got: %q", stdout)
	}
	if !strings.Contains(stdout, "Commented on caller task: "+callerTask) {
		t.Errorf("expected caller-task confirmation line, got: %q", stdout)
	}
}
