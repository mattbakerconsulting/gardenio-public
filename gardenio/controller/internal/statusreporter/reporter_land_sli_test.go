package statusreporter

import (
	"context"
	"errors"
	"maps"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"

	"gardenio/controller/internal/beadsapi"
)

// Tenant-land agents run in land-<land> (the land fence). SyncAll must sweep
// those namespaces, and the first time it moves a tenant agent's bead from a
// startup state to working it observes gardenio_lands_spawn_to_working_seconds
// once (gd-Vuu1J7y4J0).

type landObservation struct {
	land    string
	seconds float64
}

// landBeadDaemon is mockBeadUpdater plus GetBead, with field writes merged
// onto the beads so a later sweep sees the first-working stamp.
type landBeadDaemon struct {
	*mockBeadUpdater
	beads      map[string]*beadsapi.BeadDetail
	getBeadErr error
}

func (d *landBeadDaemon) GetBead(_ context.Context, beadID string) (*beadsapi.BeadDetail, error) {
	if d.getBeadErr != nil {
		return nil, d.getBeadErr
	}
	b, ok := d.beads[beadID]
	if !ok {
		return nil, errors.New("not found")
	}
	cp := *b
	cp.Fields = maps.Clone(b.Fields)
	return &cp, nil
}

func (d *landBeadDaemon) UpdateBeadFields(ctx context.Context, beadID string, fields map[string]string) error {
	if err := d.mockBeadUpdater.UpdateBeadFields(ctx, beadID, fields); err != nil {
		return err
	}
	if b, ok := d.beads[beadID]; ok {
		if b.Fields == nil {
			b.Fields = map[string]string{}
		}
		maps.Copy(b.Fields, fields)
	}
	return nil
}

const tenantBead = "crew-acme-dev-alpha"

func newLandSLIReporter(t *testing.T, state string, created time.Time, pods ...runtime.Object) (*HTTPReporter, *landBeadDaemon, *fakeEventRecorder, time.Time) {
	t.Helper()
	daemon := &landBeadDaemon{
		mockBeadUpdater: &mockBeadUpdater{stateByID: map[string]string{tenantBead: state}},
		beads:           map[string]*beadsapi.BeadDetail{tenantBead: {ID: tenantBead, CreatedAt: created}},
	}
	rec := &fakeEventRecorder{}
	r := NewHTTPReporter(daemon, fake.NewSimpleClientset(pods...), "gardenio", testLogger())
	r.SetEventRecorder(rec)
	r.SetNamespaceSource(func() []string { return []string{"land-acme"} })
	now := created.Add(90 * time.Second)
	r.now = func() time.Time { return now }
	return r, daemon, rec, now
}

func TestSyncAll_LandSpawnToWorking_ObservedOnce(t *testing.T) {
	created := time.Date(2026, 10, 21, 12, 0, 0, 0, time.UTC)
	pod := makePod("crew-acme-dev-alpha", "land-acme", corev1.PodRunning,
		agentLabels("acme", "dev", "alpha"), "10.0.0.9")
	r, daemon, rec, now := newLandSLIReporter(t, "spawning", created, pod)

	if err := r.SyncAll(context.Background()); err != nil {
		t.Fatalf("sync 1: %v", err)
	}
	if len(daemon.stateCalls) != 1 || daemon.stateCalls[0].state != "working" {
		t.Fatalf("the land namespace pod must be swept and reported working, got %+v", daemon.stateCalls)
	}
	if len(rec.landObserved) != 1 {
		t.Fatalf("want one observation, got %+v", rec.landObserved)
	}
	if got := rec.landObserved[0]; got.land != "acme" || got.seconds != 90 {
		t.Fatalf("want {acme 90}, got %+v", got)
	}
	if got := daemon.beads[tenantBead].Fields[FieldLandFirstWorkingAt]; got != now.Format(time.RFC3339) {
		t.Fatalf("bead must be stamped %s, got %q", FieldLandFirstWorkingAt, got)
	}

	// The mock's agent_state stays "spawning" (as after a stuck restart, which
	// takes the bead back through spawning): the stamp alone stops a second
	// observation.
	if err := r.SyncAll(context.Background()); err != nil {
		t.Fatalf("sync 2: %v", err)
	}
	if len(rec.landObserved) != 1 {
		t.Fatalf("a stamped bead must not be observed again, got %+v", rec.landObserved)
	}
}

// After a controller restart the bead is already working: not a startup
// state, so nothing is observed even though the bead was never stamped.
func TestSyncAll_LandSpawnToWorking_AlreadyWorkingNotObserved(t *testing.T) {
	created := time.Date(2026, 10, 21, 12, 0, 0, 0, time.UTC)
	pod := makePod("crew-acme-dev-alpha", "land-acme", corev1.PodRunning,
		agentLabels("acme", "dev", "alpha"), "10.0.0.9")
	r, daemon, rec, _ := newLandSLIReporter(t, "working", created, pod)

	if err := r.SyncAll(context.Background()); err != nil {
		t.Fatalf("sync: %v", err)
	}
	if len(rec.landObserved) != 0 {
		t.Fatalf("already-working bead must not be observed, got %+v", rec.landObserved)
	}
	if len(daemon.fieldCalls) != 0 {
		t.Fatalf("already-working bead must not be stamped, got %+v", daemon.fieldCalls)
	}
}

// Operator-land pods (the release namespace) and pods still Pending are not
// observed.
func TestSyncAll_LandSpawnToWorking_OnlyTenantWorking(t *testing.T) {
	created := time.Date(2026, 10, 21, 12, 0, 0, 0, time.UTC)
	operatorPod := makePod("crew-acme-dev-alpha", "gardenio", corev1.PodRunning,
		agentLabels("acme", "dev", "alpha"), "10.0.0.9")
	pendingPod := makePod("crew-acme-dev-beta", "land-acme", corev1.PodPending,
		agentLabels("acme", "dev", "beta"), "")
	r, daemon, rec, _ := newLandSLIReporter(t, "spawning", created, operatorPod, pendingPod)
	daemon.beads["crew-acme-dev-beta"] = &beadsapi.BeadDetail{ID: "crew-acme-dev-beta", CreatedAt: created}

	if err := r.SyncAll(context.Background()); err != nil {
		t.Fatalf("sync: %v", err)
	}
	if len(daemon.stateCalls) != 2 {
		t.Fatalf("both pods must be reported, got %+v", daemon.stateCalls)
	}
	if len(rec.landObserved) != 0 {
		t.Fatalf("operator-namespace and pending pods must not be observed, got %+v", rec.landObserved)
	}
}

// A failed stamp skips the observation so a retry can never count twice.
func TestSyncAll_LandSpawnToWorking_StampFailureSkips(t *testing.T) {
	created := time.Date(2026, 10, 21, 12, 0, 0, 0, time.UTC)
	pod := makePod("crew-acme-dev-alpha", "land-acme", corev1.PodRunning,
		agentLabels("acme", "dev", "alpha"), "10.0.0.9")
	r, daemon, rec, _ := newLandSLIReporter(t, "spawning", created, pod)
	daemon.fieldErr = errors.New("daemon down")

	if err := r.SyncAll(context.Background()); err != nil {
		t.Fatalf("sync: %v", err)
	}
	if len(rec.landObserved) != 0 {
		t.Fatalf("a failed stamp must not observe, got %+v", rec.landObserved)
	}
}

// A daemon without GetBead (no created_at) observes nothing and still reports.
func TestSyncAll_LandSpawnToWorking_NoBeadReader(t *testing.T) {
	pod := makePod("crew-acme-dev-alpha", "land-acme", corev1.PodRunning,
		agentLabels("acme", "dev", "alpha"), "10.0.0.9")
	daemon := &mockBeadUpdater{stateByID: map[string]string{tenantBead: "spawning"}}
	rec := &fakeEventRecorder{}
	r := NewHTTPReporter(daemon, fake.NewSimpleClientset(pod), "gardenio", testLogger())
	r.SetEventRecorder(rec)
	r.SetNamespaceSource(func() []string { return []string{"land-acme"} })

	if err := r.SyncAll(context.Background()); err != nil {
		t.Fatalf("sync: %v", err)
	}
	if len(daemon.stateCalls) != 1 {
		t.Fatalf("pod must still be reported, got %+v", daemon.stateCalls)
	}
	if len(rec.landObserved) != 0 {
		t.Fatalf("no bead reader, no observation; got %+v", rec.landObserved)
	}
}

// The source is re-read every sweep, duplicates of the primary/extra
// namespaces are not listed twice, and a land namespace the controller cannot
// list yet (forbidden: no RoleBinding until the provisioner runs) is skipped
// without failing the sweep or counting a sync error.
func TestSyncAll_NamespaceSource(t *testing.T) {
	mainPod := makePod("crew-proj-dev-alpha", "gardenio", corev1.PodRunning,
		agentLabels("proj", "dev", "alpha"), "10.0.0.1")
	landPod := makePod("crew-acme-dev-alpha", "land-acme", corev1.PodRunning,
		agentLabels("acme", "dev", "alpha"), "10.0.0.2")
	client := fake.NewSimpleClientset(mainPod, landPod)
	client.PrependReactor("list", "pods", func(a k8stesting.Action) (bool, runtime.Object, error) {
		if a.GetNamespace() == "land-new" {
			return true, nil, apierrors.NewForbidden(schema.GroupResource{Resource: "pods"}, "", errors.New("no binding"))
		}
		return false, nil, nil
	})
	daemon := &mockBeadUpdater{}
	r := NewHTTPReporter(daemon, client, "gardenio", testLogger())
	var lands []string
	r.SetNamespaceSource(func() []string { return lands })

	if err := r.SyncAll(context.Background()); err != nil {
		t.Fatalf("sync 1: %v", err)
	}
	if len(daemon.stateCalls) != 1 {
		t.Fatalf("no lands: only the primary pod, got %+v", daemon.stateCalls)
	}

	lands = []string{"land-acme", "gardenio", "land-acme", "land-new", ""}
	daemon.stateCalls = nil
	if err := r.SyncAll(context.Background()); err != nil {
		t.Fatalf("sync 2: %v", err)
	}
	if len(daemon.stateCalls) != 2 {
		t.Fatalf("want primary + land pod once each, got %+v", daemon.stateCalls)
	}
	if got := r.syncErrors.Load(); got != 0 {
		t.Fatalf("a forbidden land namespace is not a sync error, got %d", got)
	}
}
