package main

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"slices"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"gardenio/controller/internal/beadsapi"
	"gardenio/controller/internal/localreg"
)

// fakeLaptopReaper records what supersedeStaleLaptopAgentBeads asks of the daemon.
type fakeLaptopReaper struct {
	beads    []*beadsapi.BeadDetail
	roster   []beadsapi.RosterActor
	query    beadsapi.ListBeadsQuery
	closed   map[string]map[string]string
	comments map[string]string
}

func (f *fakeLaptopReaper) ListBeadsFiltered(_ context.Context, q beadsapi.ListBeadsQuery) (*beadsapi.ListBeadsResult, error) {
	f.query = q
	return &beadsapi.ListBeadsResult{Beads: f.beads, Total: len(f.beads)}, nil
}

func (f *fakeLaptopReaper) GetAgentRoster(context.Context, int) (*beadsapi.AgentRosterResponse, error) {
	return &beadsapi.AgentRosterResponse{Actors: f.roster}, nil
}

func (f *fakeLaptopReaper) CloseBead(_ context.Context, id string, fields map[string]string) error {
	if f.closed == nil {
		f.closed = map[string]map[string]string{}
	}
	f.closed[id] = fields
	return nil
}

func (f *fakeLaptopReaper) AddComment(_ context.Context, id, _, text string) error {
	if f.comments == nil {
		f.comments = map[string]string{}
	}
	f.comments[id] = text
	return nil
}

func laptopBead(id, name, owner string, age time.Duration) *beadsapi.BeadDetail {
	return &beadsapi.BeadDetail{
		ID:        id,
		Status:    "open",
		CreatedBy: "local-box",
		CreatedAt: time.Now().Add(-age),
		Fields:    map[string]string{"agent": name, "execution_target": "laptop", "operated_by": owner},
	}
}

// deadPID returns the pid of a process that has exited.
func deadPID(t *testing.T) int {
	t.Helper()
	cmd := exec.Command("true")
	if err := cmd.Run(); err != nil {
		t.Skipf("cannot run true: %v", err)
	}
	return cmd.Process.Pid
}

func useTempLocalreg(t *testing.T) {
	t.Helper()
	t.Setenv("GARDENIO_HOME", t.TempDir())
}

const laptopOwner = "oidc:a@example.com"

// A bead this host's registry holds with a dead pid is closed even though the
// actor's heartbeat is fresh: the run that just died is the fresh heartbeat.
func TestSupersede_ClosesBeadWhoseRegisteredProcessIsDead(t *testing.T) {
	useTempLocalreg(t)
	if _, err := localreg.Register(localreg.Entry{Name: "box", BeadID: "gd-old", PID: deadPID(t)}); err != nil {
		t.Fatal(err)
	}
	f := &fakeLaptopReaper{
		beads:  []*beadsapi.BeadDetail{laptopBead("gd-new", "box", laptopOwner, 0), laptopBead("gd-old", "box", laptopOwner, 2*time.Minute)},
		roster: []beadsapi.RosterActor{{Actor: "box", IdleSecs: 30}},
	}
	got := supersedeStaleLaptopAgentBeads(context.Background(), f, "box", "gd-new", []string{"a@example.com"})
	if !slices.Equal(got, []string{"gd-old"}) {
		t.Fatalf("closed %v, want [gd-old]", got)
	}
	if _, ok := f.closed["gd-new"]; ok {
		t.Fatal("closed the new run's own bead")
	}
	if f.closed["gd-old"]["agent_state"] != "done" || !strings.Contains(f.closed["gd-old"]["close_reason"], "gd-new") {
		t.Errorf("close fields = %v", f.closed["gd-old"])
	}
	if c := f.comments["gd-old"]; !strings.Contains(c, "closing this stale laptop agent bead, not adopting it") || !strings.Contains(c, "not running") {
		t.Errorf("comment = %q", c)
	}
	if f.query.Fields["agent"] != "box" || f.query.Fields["execution_target"] != "laptop" || !slices.Equal(f.query.Types, []string{"agent"}) {
		t.Errorf("query = %+v", f.query)
	}
}

// A bead whose registered process is alive is a live run on this host.
func TestSupersede_LeavesBeadWhoseRegisteredProcessIsAlive(t *testing.T) {
	useTempLocalreg(t)
	if _, err := localreg.Register(localreg.Entry{Name: "box", BeadID: "gd-live", PID: os.Getpid()}); err != nil {
		t.Fatal(err)
	}
	f := &fakeLaptopReaper{beads: []*beadsapi.BeadDetail{laptopBead("gd-live", "box", laptopOwner, time.Hour)}}
	if got := supersedeStaleLaptopAgentBeads(context.Background(), f, "box", "gd-new", []string{laptopOwner}); len(got) != 0 {
		t.Fatalf("closed %v, want none", got)
	}
}

// Without registry evidence the actor's heartbeat decides for the newest
// earlier bead; older ones are superseded by it.
func TestSupersede_HeartbeatDecidesWithoutRegistryEvidence(t *testing.T) {
	for _, tc := range []struct {
		name   string
		roster []beadsapi.RosterActor
		want   []string
	}{
		{"fresh heartbeat keeps the newest", []beadsapi.RosterActor{{Actor: "box", IdleSecs: 60}}, []string{"gd-older"}},
		{"stale heartbeat", []beadsapi.RosterActor{{Actor: "box", IdleSecs: 20 * 60}}, []string{"gd-newest", "gd-older"}},
		{"reaped", []beadsapi.RosterActor{{Actor: "box", IdleSecs: 60, Reaped: true}}, []string{"gd-newest", "gd-older"}},
		{"no heartbeat", []beadsapi.RosterActor{{Actor: "other", IdleSecs: 1}}, []string{"gd-newest", "gd-older"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			useTempLocalreg(t)
			f := &fakeLaptopReaper{
				beads:  []*beadsapi.BeadDetail{laptopBead("gd-older", "box", laptopOwner, 3*time.Hour), laptopBead("gd-newest", "box", laptopOwner, time.Hour)},
				roster: tc.roster,
			}
			got := supersedeStaleLaptopAgentBeads(context.Background(), f, "box", "gd-new", []string{laptopOwner})
			slices.Sort(got)
			if !slices.Equal(got, tc.want) {
				t.Fatalf("closed %v, want %v", got, tc.want)
			}
		})
	}
}

// Another owner's bead of the same name, and a bead the daemon matched loosely
// (another name or a pod), are never touched.
func TestSupersede_OnlySameOwnerSameNameLaptopBeads(t *testing.T) {
	useTempLocalreg(t)
	pod := laptopBead("gd-pod", "box", laptopOwner, time.Hour)
	pod.Fields["execution_target"] = "k8s"
	f := &fakeLaptopReaper{beads: []*beadsapi.BeadDetail{
		laptopBead("gd-theirs", "box", "oidc:b@example.com", time.Hour),
		laptopBead("gd-othername", "box2", laptopOwner, time.Hour),
		pod,
	}}
	if got := supersedeStaleLaptopAgentBeads(context.Background(), f, "box", "gd-new", []string{laptopOwner, "", "unknown"}); len(got) != 0 {
		t.Fatalf("closed %v, want none", got)
	}
}

func TestSameLaptopOwner_MatchesCreatedByAndOIDCPrefix(t *testing.T) {
	b := &beadsapi.BeadDetail{CreatedBy: "A@Example.com", Fields: map[string]string{}}
	if !sameLaptopOwner(b, []string{"oidc:a@example.com"}) {
		t.Fatal("created_by a@example.com should match oidc:a@example.com")
	}
	if sameLaptopOwner(&beadsapi.BeadDetail{CreatedBy: "unknown"}, []string{"unknown"}) {
		t.Fatal("the unknown actor must not match")
	}
}

// fakeCloser fails its first n closes.
type fakeCloser struct {
	fail  int
	calls int
}

func (f *fakeCloser) CloseBead(context.Context, string, map[string]string) error {
	f.calls++
	if f.calls <= f.fail {
		return &beadsapi.APIError{StatusCode: http.StatusUnauthorized, Message: "invalid token"}
	}
	return nil
}

// A 401 at exit is retried on a client with a fresh token.
func TestCloseLocalAgentBeadAtExit_RetriesWithFreshToken(t *testing.T) {
	useTempLocalreg(t)
	stale := &fakeCloser{fail: 1}
	fresh := &fakeCloser{}
	freshCalls := 0
	ok := closeLocalAgentBeadAtExit(stale, "box", "gd-bead", func(context.Context) (beadCloser, error) {
		freshCalls++
		return fresh, nil
	})
	if !ok || freshCalls != 1 || fresh.calls != 1 {
		t.Fatalf("ok=%v freshCalls=%d fresh.calls=%d", ok, freshCalls, fresh.calls)
	}
	if _, err := localreg.Read("box"); err == nil {
		t.Fatal("a closed bead must leave no registry entry")
	}
}

// When the retry fails too, a dead registry entry names the bead, and the next
// start of the same name closes it.
func TestCloseLocalAgentBeadAtExit_RecordsBeadWhenRetryFails(t *testing.T) {
	for _, tc := range []struct {
		name  string
		fresh func(context.Context) (beadCloser, error)
	}{
		{"retry fails", func(context.Context) (beadCloser, error) { return &fakeCloser{fail: 1}, nil }},
		{"no fresh token", func(context.Context) (beadCloser, error) { return nil, errors.New("no bearer") }},
		{"no retry (runner)", nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			useTempLocalreg(t)
			if closeLocalAgentBeadAtExit(&fakeCloser{fail: 1}, "box", "gd-bead", tc.fresh) {
				t.Fatal("reported closed")
			}
			e, err := localreg.Read("box")
			if err != nil || e.BeadID != "gd-bead" || localreg.Alive(e) {
				t.Fatalf("registry entry = %+v, %v; want gd-bead with no live pid", e, err)
			}
			f := &fakeLaptopReaper{
				beads:  []*beadsapi.BeadDetail{laptopBead("gd-bead", "box", laptopOwner, time.Minute)},
				roster: []beadsapi.RosterActor{{Actor: "box", IdleSecs: 5}},
			}
			if got := supersedeStaleLaptopAgentBeads(context.Background(), f, "box", "gd-next", []string{laptopOwner}); !slices.Equal(got, []string{"gd-bead"}) {
				t.Fatalf("next start closed %v, want [gd-bead]", got)
			}
		})
	}
}

func TestFreshExitDaemon_NoRetryForRunner(t *testing.T) {
	if freshExitDaemon(true) != nil {
		t.Fatal("a runner token does not expire with a login; no retry")
	}
}

func TestLocalAgentEnv_CarriesAgentStartPID(t *testing.T) {
	env := localAgentEnv("agent", "gd-bead", "crew", "/tmp", "http://127.0.0.1:8080", "", localCreds{HTTPURL: "http://localhost:8080"})
	want := localAgentStartPIDEnv + "=" + strconv.Itoa(os.Getpid())
	if !slices.Contains(env, want) {
		t.Fatalf("env lacks %s", want)
	}
}

// stopGateCloseServer serves an open working agent bead and counts requests
// and close POSTs.
func stopGateCloseServer(t *testing.T, beadID string) (requests, closes *atomic.Int64) {
	t.Helper()
	requests, closes = &atomic.Int64{}, &atomic.Int64{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/v1/beads/"+beadID:
			_ = json.NewEncoder(w).Encode(map[string]any{
				"id": beadID, "type": "agent", "status": "open",
				"fields": map[string]string{"agent_state": "working", "execution_target": "laptop"},
			})
		case r.Method == http.MethodGet:
			_ = json.NewEncoder(w).Encode(map[string]any{"beads": []any{}, "total": 0})
		case r.Method == http.MethodPost && r.URL.Path == "/v1/beads/"+beadID+"/close":
			closes.Add(1)
			_ = json.NewEncoder(w).Encode(map[string]any{"id": beadID})
		default:
			w.WriteHeader(http.StatusOK)
		}
	}))
	t.Cleanup(srv.Close)
	c, err := beadsapi.New(beadsapi.Config{HTTPAddr: srv.URL})
	if err != nil {
		t.Fatalf("beadsapi.New: %v", err)
	}
	oldDaemon := daemon
	daemon = c
	t.Cleanup(func() { daemon = oldDaemon })
	t.Setenv("KD_AGENT_ID", beadID)
	return requests, closes
}

// The stop gate leaves a laptop bead alone while its gb agent start lives.
func TestCloseAgentBeadOnExit_LeavesBeadWhileLocalAgentStartAlive(t *testing.T) {
	requests, _ := stopGateCloseServer(t, "gd-laptop-live")
	t.Setenv(localAgentStartPIDEnv, strconv.Itoa(os.Getpid()))

	closeAgentBeadOnExit("escape_hatch")

	if n := requests.Load(); n != 0 {
		t.Fatalf("expected no daemon calls while gb agent start is alive, got %d", n)
	}
}

// Once the owning gb agent start is gone, the stop gate closes as before.
func TestCloseAgentBeadOnExit_ClosesOnceLocalAgentStartGone(t *testing.T) {
	_, closes := stopGateCloseServer(t, "gd-laptop-dead")
	t.Setenv(localAgentStartPIDEnv, strconv.Itoa(deadPID(t)))

	closeAgentBeadOnExit("escape_hatch")

	if n := closes.Load(); n != 1 {
		t.Fatalf("expected 1 close POST once gb agent start is gone, got %d", n)
	}
}

// A pod never sets the pid: the stop gate closes exactly as before.
func TestCloseAgentBeadOnExit_PodUnaffected(t *testing.T) {
	_, closes := stopGateCloseServer(t, "gd-pod-bead")
	t.Setenv(localAgentStartPIDEnv, "")

	closeAgentBeadOnExit("escape_hatch")

	if n := closes.Load(); n != 1 {
		t.Fatalf("expected 1 close POST on a pod, got %d", n)
	}
}
