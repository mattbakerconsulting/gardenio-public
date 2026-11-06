package statusreporter

import (
	"context"
	"errors"
	"reflect"
	"slices"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"

	"gardenio/controller/internal/beadsapi"
)

// sessionMockDaemon adds the session list and close the OOM stamp needs to
// mockBeadUpdater.
type sessionMockDaemon struct {
	*mockBeadUpdater
	sessions []*beadsapi.BeadDetail
	listErr  error
	queries  []beadsapi.ListBeadsQuery
	closed   []string
}

func (m *sessionMockDaemon) ListBeadsFiltered(_ context.Context, q beadsapi.ListBeadsQuery) (*beadsapi.ListBeadsResult, error) {
	m.queries = append(m.queries, q)
	if m.listErr != nil {
		return nil, m.listErr
	}
	return &beadsapi.ListBeadsResult{Beads: m.sessions}, nil
}

func (m *sessionMockDaemon) CloseBead(_ context.Context, beadID string, _ map[string]string) error {
	m.closed = append(m.closed, beadID)
	return nil
}

func oomTestSession(id, agentBead, startedAt string) *beadsapi.BeadDetail {
	return &beadsapi.BeadDetail{ID: id, Type: "session", Status: "open", Fields: map[string]string{
		"agent_bead_id": agentBead, "status": "active", "started_at": startedAt,
	}}
}

// oomPod is crew pmt's pod after its agent container was OOM-killed at 01:53:12
// and restarted (the shape of gd-ZapVESzC3t, gd-DgSORP1h8P).
func oomPod(container string) *corev1.Pod {
	pod := makePod("crew-proj-dev-alpha", "ns", corev1.PodRunning, agentLabels("proj", "dev", "alpha"), "10.0.0.1")
	pod.Status.ContainerStatuses = []corev1.ContainerStatus{{
		Name: container,
		LastTerminationState: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{
			Reason:     "OOMKilled",
			ExitCode:   137,
			StartedAt:  metav1.NewTime(time.Date(2026, 10, 28, 1, 9, 50, 400_000_000, time.UTC)),
			FinishedAt: metav1.NewTime(time.Date(2026, 10, 28, 1, 53, 12, 0, time.UTC)),
		}},
	}}
	return pod
}

// TestSyncAll_StampsOOMKilledSession: the session the kill ended gets exit 137,
// restart_cause oomkill, status crashed and ended_at the kill, and is closed;
// the resumed session after the kill, an older session from an earlier
// container and an already-stamped one are left alone, and the kill is
// stamped once across syncs.
func TestSyncAll_StampsOOMKilledSession(t *testing.T) {
	const bead = "crew-proj-dev-alpha"
	stamped := oomTestSession("gd-done", bead, "2026-10-28T01:20:00Z")
	stamped.Fields["ended_at"] = "2026-10-28T01:25:00Z"
	daemon := &sessionMockDaemon{
		mockBeadUpdater: &mockBeadUpdater{},
		sessions: []*beadsapi.BeadDetail{
			oomTestSession("gd-earlier", bead, "2026-10-28T00:30:00Z"),
			oomTestSession("gd-killed", bead, "2026-10-28T01:09:55Z"),
			oomTestSession("gd-resumed", bead, "2026-10-28T01:53:27Z"),
			oomTestSession("gd-other", "crew-proj-dev-beta", "2026-10-28T01:10:00Z"),
			stamped,
		},
	}
	r := NewHTTPReporter(daemon, fake.NewSimpleClientset(oomPod("agent")), "ns", testLogger())

	for i := 0; i < 2; i++ {
		if err := r.SyncAll(context.Background()); err != nil {
			t.Fatalf("sync %d: %v", i+1, err)
		}
	}

	if len(daemon.queries) != 1 {
		t.Fatalf("the kill is looked up once across syncs, got %d lists", len(daemon.queries))
	}
	if q := daemon.queries[0]; !reflect.DeepEqual(q.Fields, map[string]string{"agent_bead_id": bead}) || !slices.Equal(q.Types, []string{"session"}) {
		t.Errorf("list query = types %v fields %v", q.Types, q.Fields)
	}
	var stamps []typedFieldCall
	for _, c := range daemon.typedFieldCall {
		if c.beadID != bead {
			stamps = append(stamps, c)
		}
	}
	want := []typedFieldCall{{beadID: "gd-killed", fields: map[string]any{
		"exit_code": 137, "restart_cause": "oomkill", "status": "crashed", "ended_at": "2026-10-28T01:53:12Z",
	}}}
	if !reflect.DeepEqual(stamps, want) {
		t.Errorf("session stamps = %+v, want %+v", stamps, want)
	}
	if !slices.Equal(daemon.closed, []string{"gd-killed"}) {
		t.Errorf("closed = %v, want [gd-killed]", daemon.closed)
	}
}

// TestSyncAll_OOMStampSkipsSidecarAndRetriesFailedList: a sidecar's OOM does
// not end the agent's session, and a failed lookup is retried next sync.
func TestSyncAll_OOMStampSkipsSidecarAndRetriesFailedList(t *testing.T) {
	const bead = "crew-proj-dev-alpha"
	sidecar := &sessionMockDaemon{
		mockBeadUpdater: &mockBeadUpdater{},
		sessions:        []*beadsapi.BeadDetail{oomTestSession("gd-killed", bead, "2026-10-28T01:09:55Z")},
	}
	r := NewHTTPReporter(sidecar, fake.NewSimpleClientset(oomPod("ziti")), "ns", testLogger())
	if err := r.SyncAll(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(sidecar.queries) != 0 || len(sidecar.closed) != 0 {
		t.Errorf("sidecar OOM looked up %d times, closed %v", len(sidecar.queries), sidecar.closed)
	}

	flaky := &sessionMockDaemon{
		mockBeadUpdater: &mockBeadUpdater{},
		sessions:        []*beadsapi.BeadDetail{oomTestSession("gd-killed", bead, "2026-10-28T01:09:55Z")},
		listErr:         errors.New("daemon unavailable"),
	}
	r = NewHTTPReporter(flaky, fake.NewSimpleClientset(oomPod("agent")), "ns", testLogger())
	if err := r.SyncAll(context.Background()); err != nil {
		t.Fatal(err)
	}
	flaky.listErr = nil
	if err := r.SyncAll(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(flaky.queries) != 2 || !slices.Equal(flaky.closed, []string{"gd-killed"}) {
		t.Errorf("after a failed list: %d lists, closed %v; want 2 lists, [gd-killed]", len(flaky.queries), flaky.closed)
	}
}
