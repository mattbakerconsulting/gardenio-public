package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os/exec"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"gardenio/controller/internal/beadsapi"
	"gardenio/controller/internal/coopapi"
)

// podMailDaemon is a one-stream daemon for startPodMail: events pushed on
// stream reach the subscriber, and closes are recorded.
type podMailDaemon struct {
	stream chan beadsapi.SSEEvent
	mu     sync.Mutex
	closed []string
}

func (d *podMailDaemon) EventStreamScoped(context.Context, string, string, string) (<-chan beadsapi.SSEEvent, error) {
	return d.stream, nil
}

func (d *podMailDaemon) ListBeadsFiltered(context.Context, beadsapi.ListBeadsQuery) (*beadsapi.ListBeadsResult, error) {
	return &beadsapi.ListBeadsResult{}, nil
}

func (d *podMailDaemon) GetBead(_ context.Context, id string) (*beadsapi.BeadDetail, error) {
	return &beadsapi.BeadDetail{ID: id}, nil
}

func (d *podMailDaemon) CloseBead(_ context.Context, id string, _ map[string]string) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.closed = append(d.closed, id)
	return nil
}

func (d *podMailDaemon) UpdateBeadFields(context.Context, string, map[string]string) error {
	return nil
}

func (d *podMailDaemon) Reassign(context.Context, string, string) error {
	return nil
}

func (d *podMailDaemon) closedIDs() []string {
	d.mu.Lock()
	defer d.mu.Unlock()
	return append([]string(nil), d.closed...)
}

func podMailEvent(t *testing.T, id string, labels ...string) beadsapi.SSEEvent {
	t.Helper()
	data, err := json.Marshal(map[string]any{"bead": map[string]any{
		"id": id, "type": "mail", "status": "open", "assignee": "crew-x",
		"title": "please look", "description": "the body", "labels": labels,
	}})
	if err != nil {
		t.Fatal(err)
	}
	return beadsapi.SSEEvent{ID: id, Data: data}
}

// idleCoop is a coop HTTP server whose agent sits idle at its prompt: it
// accepts every nudge. onShutdown runs inside the shutdown handler.
type idleCoop struct {
	srv        *httptest.Server
	mu         sync.Mutex
	nudges     []string
	onShutdown func()
}

func newIdleCoop(t *testing.T) *idleCoop {
	c := &idleCoop{}
	c.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/v1/agent/nudge":
			var body map[string]any
			_ = json.NewDecoder(r.Body).Decode(&body)
			msg, _ := body["message"].(string)
			c.mu.Lock()
			c.nudges = append(c.nudges, msg)
			c.mu.Unlock()
			_ = json.NewEncoder(w).Encode(map[string]any{"delivered": true})
		case "/api/v1/shutdown":
			if c.onShutdown != nil {
				c.onShutdown()
			}
			w.WriteHeader(http.StatusOK)
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(c.srv.Close)
	return c
}

func (c *idleCoop) nudged() []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]string(nil), c.nudges...)
}

// TestPodMail_IdleAgentReceivesNudgeAndKillMail drives the gb init mail path
// end to end against a real coop client: an interrupt mail (what
// bridge.NudgeAgent sends) reaches the idle agent as a coop nudge, and a
// control:kill mail asks coop to shut down, marks the restart loop killed
// before that shutdown can let coop exit, and SIGTERMs the coop process
// running under sessionCtx.
func TestPodMail_IdleAgentReceivesNudgeAndKillMail(t *testing.T) {
	d := &podMailDaemon{stream: make(chan beadsapi.SSEEvent, 4)}
	coop := newIdleCoop(t)
	var mail *podMail
	var killedAtShutdown atomic.Bool
	// shutdownSeen closes once the shutdown POST reaches coop. killRequested
	// flips before that POST is sent, so waiting on it alone races the
	// handler that records killedAtShutdown (gd-5m8l2fhuvf).
	shutdownSeen := make(chan struct{})
	coop.onShutdown = func() {
		killedAtShutdown.Store(mail.killRequested())
		close(shutdownSeen)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	mail = startPodMail(ctx, d, coopapi.New(coop.srv.URL), "gd-agent", "crew-x", t.Logf)
	defer mail.stop()

	// Stand-in for the coop process runCoopSession starts under sessionCtx,
	// with the same SIGTERM cancel.
	proc := exec.CommandContext(mail.sessionCtx, "sleep", "60")
	proc.Cancel = func() error { return proc.Process.Signal(syscall.SIGTERM) }
	if err := proc.Start(); err != nil {
		t.Fatalf("start stand-in coop: %v", err)
	}
	procDone := make(chan error, 1)
	go func() { procDone <- proc.Wait() }()

	d.stream <- podMailEvent(t, "gd-m1", "from:slack-bridge", "delivery:interrupt")
	waitFor(t, func() bool { return len(coop.nudged()) == 1 })
	if got := coop.nudged()[0]; !strings.Contains(got, "please look") || !strings.Contains(got, "the body") {
		t.Fatalf("nudge should carry the mail title and body, got %q", got)
	}
	if mail.killRequested() {
		t.Fatal("an ordinary mail must not mark the loop killed")
	}

	d.stream <- podMailEvent(t, "gd-k1", "from:slack-bridge", "control:kill")
	select {
	case <-shutdownSeen:
	case <-time.After(5 * time.Second):
		t.Fatal("control:kill should ask coop to shut down")
	}
	if !killedAtShutdown.Load() {
		t.Fatal("the loop must be marked killed before coop is asked to shut down, or it can restart coop")
	}
	select {
	case <-procDone:
	case <-time.After(5 * time.Second):
		t.Fatal("control:kill should SIGTERM the coop running under sessionCtx")
	}
	if ids := d.closedIDs(); len(ids) != 1 || ids[0] != "gd-k1" {
		t.Fatalf("the kill mail should be closed so the next session does not drain it, got %v", ids)
	}
	if ctx.Err() != nil {
		t.Fatal("a kill must not cancel the pod context: the loop closes the bead itself")
	}
}

// TestPodMail_NoAgentBead_StartsNothing: with no agent bead there is no
// subscriber, and sessionCtx lives as long as the loop's context.
func TestPodMail_NoAgentBead_StartsNothing(t *testing.T) {
	d := &podMailDaemon{stream: make(chan beadsapi.SSEEvent)}
	mail := startPodMail(context.Background(), d, coopapi.New("http://127.0.0.1:1"), "", "crew-x", t.Logf)
	defer mail.stop()
	if mail.sessionCtx.Err() != nil || mail.killRequested() {
		t.Fatal("no subscriber: nothing should be killed or cancelled")
	}
	old := daemon
	daemon = nil
	defer func() { daemon = old }()
	if initMailDaemon() != nil {
		t.Fatal("initMailDaemon must be a nil interface when there is no daemon client")
	}
}

// TestCloseAgentBeadOnMailKill closes the agent bead done/control_kill, so
// the reconciler does not respawn an agent a kill stopped.
func TestCloseAgentBeadOnMailKill(t *testing.T) {
	const beadID = "gd-killed"
	var closeFields map[string]string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/v1/beads/"+beadID:
			_ = json.NewEncoder(w).Encode(map[string]any{"id": beadID, "type": "agent", "status": "open", "fields": map[string]string{}})
		case r.Method == http.MethodPost && r.URL.Path == "/v1/beads/"+beadID+"/close":
			_ = json.NewDecoder(r.Body).Decode(&closeFields)
			_ = json.NewEncoder(w).Encode(map[string]any{"id": beadID})
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)
	c, err := beadsapi.New(beadsapi.Config{HTTPAddr: srv.URL})
	if err != nil {
		t.Fatalf("beadsapi.New: %v", err)
	}
	old := daemon
	daemon = c
	t.Cleanup(func() { daemon = old })
	t.Setenv("KD_AGENT_ID", beadID)
	t.Setenv("BOAT_AGENT_BEAD_ID", "")

	closeAgentBeadOnMailKill(t.Logf)

	if closeFields["close_reason"] != "control_kill" || closeFields["agent_state"] != "done" {
		t.Fatalf("expected a done/control_kill close, got %v", closeFields)
	}
}
