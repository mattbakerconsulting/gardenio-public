package bridge

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"slices"
	"sync"
	"testing"
	"time"

	"gardenio/controller/internal/beadsapi"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"
)

// fakeDashboardDaemon is an in-memory DashboardDaemon for tests.
type fakeDashboardDaemon struct {
	mu       sync.Mutex
	beads    map[string]*beadsapi.BeadDetail
	deps     map[string][]beadsapi.Dependency // forward deps keyed by bead id
	nextID   int
	created  []beadsapi.CreateBeadRequest
	comments map[string][]string // beadID → comment texts
}

func newFakeDashboardDaemon() *fakeDashboardDaemon {
	return &fakeDashboardDaemon{
		beads:    map[string]*beadsapi.BeadDetail{},
		deps:     map[string][]beadsapi.Dependency{},
		comments: map[string][]string{},
	}
}

func (f *fakeDashboardDaemon) AddComment(_ context.Context, beadID, _, text string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.comments[beadID] = append(f.comments[beadID], text)
	return nil
}

func (f *fakeDashboardDaemon) CreateBead(_ context.Context, req beadsapi.CreateBeadRequest) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.nextID++
	id := fmt.Sprintf("gd-fake%d", f.nextID)
	fields := map[string]string{}
	if len(req.Fields) > 0 {
		var raw map[string]any
		_ = json.Unmarshal(req.Fields, &raw)
		for k, v := range raw {
			fields[k] = fmt.Sprintf("%v", v)
		}
	}
	f.beads[id] = &beadsapi.BeadDetail{
		ID:     id,
		Title:  req.Title,
		Type:   req.Type,
		Kind:   req.Kind,
		Status: "open",
		Labels: append([]string{}, req.Labels...),
		Fields: fields,
	}
	f.created = append(f.created, req)
	return id, nil
}

func (f *fakeDashboardDaemon) GetBead(_ context.Context, id string) (*beadsapi.BeadDetail, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	b, ok := f.beads[id]
	if !ok {
		return nil, fmt.Errorf("not found: %s", id)
	}
	cp := *b
	cp.Fields = map[string]string{}
	for k, v := range b.Fields {
		cp.Fields[k] = v
	}
	cp.Labels = append([]string{}, b.Labels...)
	return &cp, nil
}

func (f *fakeDashboardDaemon) UpdateBeadDescription(_ context.Context, id, desc string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if b, ok := f.beads[id]; ok {
		b.Description = desc
	}
	return nil
}

func (f *fakeDashboardDaemon) UpdateBeadFields(_ context.Context, id string, fields map[string]string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	b, ok := f.beads[id]
	if !ok {
		return fmt.Errorf("not found: %s", id)
	}
	if b.Fields == nil {
		b.Fields = map[string]string{}
	}
	for k, v := range fields {
		b.Fields[k] = v
	}
	return nil
}

func (f *fakeDashboardDaemon) AddDependency(_ context.Context, beadID, dependsOnID, depType, _ string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	// Index by the depends-on target so GetReverseDependencies (rows where
	// depends_on_id == queried id) can find it.
	f.deps[dependsOnID] = append(f.deps[dependsOnID], beadsapi.Dependency{
		BeadID:      beadID,
		DependsOnID: dependsOnID,
		Type:        depType,
	})
	return nil
}

func (f *fakeDashboardDaemon) GetReverseDependencies(_ context.Context, beadID string) ([]beadsapi.Dependency, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]beadsapi.Dependency{}, f.deps[beadID]...), nil
}

func (f *fakeDashboardDaemon) AddLabel(_ context.Context, beadID, label string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if b, ok := f.beads[beadID]; ok {
		b.Labels = append(b.Labels, label)
	}
	return nil
}

func (f *fakeDashboardDaemon) ListBeadsFiltered(_ context.Context, q beadsapi.ListBeadsQuery) (*beadsapi.ListBeadsResult, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []*beadsapi.BeadDetail
	for _, b := range f.beads {
		if len(q.Types) > 0 && !sliceContains(q.Types, b.Type) {
			continue
		}
		if len(q.Statuses) > 0 && !sliceContains(q.Statuses, b.Status) {
			continue
		}
		if len(q.LabelsAny) > 0 && !slices.ContainsFunc(b.Labels, func(l string) bool { return sliceContains(q.LabelsAny, l) }) {
			continue
		}
		match := true
		for k, v := range q.Fields {
			if b.Fields[k] != v {
				match = false
				break
			}
		}
		if !match {
			continue
		}
		cp := *b
		out = append(out, &cp)
	}
	return &beadsapi.ListBeadsResult{Beads: out, Total: len(out)}, nil
}

func sliceContains(s []string, v string) bool {
	for _, x := range s {
		if x == v {
			return true
		}
	}
	return false
}

// fakeWriter records the last model written per uid and models ConfigMap
// presence: Upsert makes a uid's CM present, and a test can simulate an
// out-of-band delete via deleteCM(uid) so Exists reports it absent.
type fakeWriter struct {
	mu sync.Mutex
	// chartOwned marks uids a chart bundle ConfigMap already provisions, so the
	// bridge must defer (ChartOwnsDashboard returns true and Upsert is skipped).
	chartOwned map[string]bool
	models     map[string][]byte
	// present tracks whether the per-uid ConfigMap currently exists (what the
	// Exists presence probe reads). Upsert sets it; deleteCM clears it.
	present map[string]bool
	// existsErr, when set for a uid, makes Exists return an error for it so a
	// test can exercise the fail-closed-toward-writing path.
	existsErr map[string]error
	writes    int
	removed   []string
}

func newFakeWriter() *fakeWriter {
	return &fakeWriter{
		models:     map[string][]byte{},
		chartOwned: map[string]bool{},
		present:    map[string]bool{},
		existsErr:  map[string]error{},
	}
}

func (w *fakeWriter) Upsert(_ context.Context, uid, _ string, model []byte) (string, string, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.models[uid] = append([]byte{}, model...)
	w.present[uid] = true
	w.writes++
	return "cm-" + uid, uid + ".json", nil
}

func (w *fakeWriter) ChartOwnsDashboard(_ context.Context, uid string) (bool, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.chartOwned[uid], nil
}

func (w *fakeWriter) RemoveOwn(_ context.Context, uid string) error {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.removed = append(w.removed, uid)
	w.present[uid] = false
	return nil
}

func (w *fakeWriter) Exists(_ context.Context, uid string) (bool, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if err := w.existsErr[uid]; err != nil {
		return false, err
	}
	return w.present[uid], nil
}

// deleteCM simulates an out-of-band ConfigMap delete: the bead keeps its stamped
// dashboard_hash but the artifact is gone (the ka-0000002299 failure).
func (w *fakeWriter) deleteCM(uid string) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.present[uid] = false
}

func TestGrafanaSync_WritesAndStampsHash(t *testing.T) {
	daemon := newFakeDashboardDaemon()
	store := NewDashboardStore(daemon, "test")
	writer := newFakeWriter()
	sync := NewGrafanaSync(GrafanaSyncConfig{Store: store, Writer: writer, Logger: slog.Default(), Projects: []string{"gardenio"}})
	ctx := context.Background()

	// Create a dashboard bead + model doc.
	model := []byte(`{"uid":"dash1","title":"D1","version":1,"panels":[]}`)
	dashID, _ := daemon.CreateBead(ctx, beadsapi.CreateBeadRequest{
		Type:   "dashboard",
		Labels: []string{"project:gardenio"},
		Title:  "D1",
		Fields: json.RawMessage(`{"dashboard_uid":"dash1"}`),
	})
	if _, err := store.PutModel(ctx, dashID, model); err != nil {
		t.Fatalf("put model: %v", err)
	}

	sync.SyncDashboard(ctx, dashID)

	if writer.writes != 1 {
		t.Fatalf("expected 1 write, got %d", writer.writes)
	}
	b, _ := daemon.GetBead(ctx, dashID)
	wantHash, _ := HashDashboardModel(model)
	if b.Fields["dashboard_hash"] != wantHash {
		t.Fatalf("hash not stamped: %q != %q", b.Fields["dashboard_hash"], wantHash)
	}
	if b.Fields["dashboard_source"] != "beads" {
		t.Fatalf("source not stamped: %q", b.Fields["dashboard_source"])
	}

	// Second sync with unchanged model must skip the write (loop-breaker).
	sync.SyncDashboard(ctx, dashID)
	if writer.writes != 1 {
		t.Fatalf("expected loop-breaker to skip; writes=%d", writer.writes)
	}
}

// TestGrafanaSync_DefersToChartOwnedDashboard is the regression test for
// ka-0000002774: when a chart bundle ConfigMap already provisions a dashboard's
// file, the bridge must NOT write a competing ConfigMap (last-writer-wins sidecar
// collision) and must remove any shadow it previously wrote — even when the bead
// model is unchanged, so a startup catch-up self-heals a pre-existing shadow.
func TestGrafanaSync_DefersToChartOwnedDashboard(t *testing.T) {
	daemon := newFakeDashboardDaemon()
	store := NewDashboardStore(daemon, "test")
	writer := newFakeWriter()
	writer.chartOwned["gardenio-perf-traces"] = true // chart bundle CM owns it
	sync := NewGrafanaSync(GrafanaSyncConfig{Store: store, Writer: writer, Logger: slog.Default(), Projects: []string{"gardenio"}})
	ctx := context.Background()

	// A stale bead model (the 2-panel July-1 shadow, in the real incident).
	model := []byte(`{"uid":"gardenio-perf-traces","title":"perf","version":1,"panels":[]}`)
	dashID, _ := daemon.CreateBead(ctx, beadsapi.CreateBeadRequest{
		Type:   "dashboard",
		Labels: []string{"project:gardenio"},
		Title:  "perf",
		Fields: json.RawMessage(`{"dashboard_uid":"gardenio-perf-traces"}`),
	})
	if _, err := store.PutModel(ctx, dashID, model); err != nil {
		t.Fatalf("put model: %v", err)
	}

	sync.SyncDashboard(ctx, dashID)

	if writer.writes != 0 {
		t.Fatalf("expected no bridge write for a chart-owned dashboard, got %d", writer.writes)
	}
	if len(writer.removed) != 1 || writer.removed[0] != "gardenio-perf-traces" {
		t.Fatalf("expected shadow removal for gardenio-perf-traces, got %v", writer.removed)
	}
}

// TestGrafanaSync_MissingConfigMapForcesRewrite is the regression test for
// ka-0000002299: a dashboard bead whose model still matches its stamped
// dashboard_hash, but whose ConfigMap was deleted out-of-band, must be
// re-provisioned by SyncDashboard. Before the fix the hash loop-breaker
// short-circuited on the stamped hash and never wrote, stranding the dashboard
// dark. Now the presence probe forces the write when the CM is absent.
func TestGrafanaSync_MissingConfigMapForcesRewrite(t *testing.T) {
	daemon := newFakeDashboardDaemon()
	store := NewDashboardStore(daemon, "test")
	writer := newFakeWriter()
	sync := NewGrafanaSync(GrafanaSyncConfig{Store: store, Writer: writer, Logger: slog.Default(), Projects: []string{"gardenio"}})
	ctx := context.Background()

	model := []byte(`{"uid":"cicd-minutes","title":"CICD","version":1,"panels":[]}`)
	dashID, _ := daemon.CreateBead(ctx, beadsapi.CreateBeadRequest{
		Type:   "dashboard",
		Labels: []string{"project:gardenio"},
		Title:  "CICD",
		Fields: json.RawMessage(`{"dashboard_uid":"cicd-minutes"}`),
	})
	if _, err := store.PutModel(ctx, dashID, model); err != nil {
		t.Fatalf("put model: %v", err)
	}

	// First sync writes the CM and stamps the hash.
	sync.SyncDashboard(ctx, dashID)
	if writer.writes != 1 {
		t.Fatalf("expected initial write, got %d", writer.writes)
	}

	// Out-of-band delete: the CM is gone but the bead keeps its stamped hash. A
	// naive hash loop-breaker would now skip forever. Age the restart-dedup
	// entry too so this path exercises the hash-match branch specifically (the
	// dedup branch is covered by TestGrafanaSync_MissingConfigMapBypassesDedup).
	writer.deleteCM("cicd-minutes")
	sync.mu.Lock()
	sync.seen = map[string]time.Time{}
	sync.mu.Unlock()

	// Next sync must re-provision despite the unchanged hash.
	sync.SyncDashboard(ctx, dashID)
	if writer.writes != 2 {
		t.Fatalf("missing ConfigMap must force a re-provision; writes=%d (want 2)", writer.writes)
	}

	// And once healed, a subsequent sync is a no-op again (hash matches AND CM
	// present) — the loop-breaker still holds in the healthy case.
	sync.mu.Lock()
	sync.seen = map[string]time.Time{}
	sync.mu.Unlock()
	sync.SyncDashboard(ctx, dashID)
	if writer.writes != 2 {
		t.Fatalf("healthy re-sync must be a no-op; writes=%d (want 2)", writer.writes)
	}
}

// TestGrafanaSync_MissingConfigMapBypassesDedup covers the second short-circuit:
// the restart dedup (keyed on bead:hash) must not suppress a force-write when the
// ConfigMap is missing, even within the dedup TTL. Otherwise a bridge that wrote
// the CM, saw it deleted out-of-band, and restarted (SSE replay) within 10min
// would re-skip and leave the dashboard dark.
func TestGrafanaSync_MissingConfigMapBypassesDedup(t *testing.T) {
	daemon := newFakeDashboardDaemon()
	store := NewDashboardStore(daemon, "test")
	writer := newFakeWriter()
	sync := NewGrafanaSync(GrafanaSyncConfig{Store: store, Writer: writer, Logger: slog.Default(), Projects: []string{"gardenio"}})
	ctx := context.Background()

	model := []byte(`{"uid":"gitlab-dark-drift","title":"Drift","version":1,"panels":[]}`)
	dashID, _ := daemon.CreateBead(ctx, beadsapi.CreateBeadRequest{
		Type:   "dashboard",
		Labels: []string{"project:gardenio"},
		Fields: json.RawMessage(`{"dashboard_uid":"gitlab-dark-drift"}`),
	})
	if _, err := store.PutModel(ctx, dashID, model); err != nil {
		t.Fatalf("put model: %v", err)
	}

	sync.SyncDashboard(ctx, dashID) // writes CM + stamps hash + marks dedup entry
	if writer.writes != 1 {
		t.Fatalf("expected initial write, got %d", writer.writes)
	}

	// Delete the CM but leave the in-memory dedup entry fresh (do NOT clear
	// sync.seen). The dedup hit must be overridden by the presence re-check.
	writer.deleteCM("gitlab-dark-drift")
	sync.SyncDashboard(ctx, dashID)
	if writer.writes != 2 {
		t.Fatalf("missing ConfigMap must bypass the restart dedup; writes=%d (want 2)", writer.writes)
	}
}

// TestGrafanaSync_ExistsProbeErrorForcesWrite pins the fail-closed-toward-writing
// behavior: if the presence probe errors (we cannot prove the CM exists), the
// sync proceeds with the idempotent write rather than risk stranding a dashboard.
func TestGrafanaSync_ExistsProbeErrorForcesWrite(t *testing.T) {
	daemon := newFakeDashboardDaemon()
	store := NewDashboardStore(daemon, "test")
	writer := newFakeWriter()
	sync := NewGrafanaSync(GrafanaSyncConfig{Store: store, Writer: writer, Logger: slog.Default(), Projects: []string{"gardenio"}})
	ctx := context.Background()

	model := []byte(`{"uid":"probe-err","title":"P","version":1,"panels":[]}`)
	dashID, _ := daemon.CreateBead(ctx, beadsapi.CreateBeadRequest{
		Type:   "dashboard",
		Labels: []string{"project:gardenio"},
		Fields: json.RawMessage(`{"dashboard_uid":"probe-err"}`),
	})
	if _, err := store.PutModel(ctx, dashID, model); err != nil {
		t.Fatalf("put model: %v", err)
	}

	sync.SyncDashboard(ctx, dashID) // initial write + stamp
	if writer.writes != 1 {
		t.Fatalf("expected initial write, got %d", writer.writes)
	}

	// Probe now errors; clear dedup so the hash-match branch is what we exercise.
	writer.mu.Lock()
	writer.existsErr["probe-err"] = fmt.Errorf("k8s API unavailable")
	writer.mu.Unlock()
	sync.mu.Lock()
	sync.seen = map[string]time.Time{}
	sync.mu.Unlock()

	sync.SyncDashboard(ctx, dashID)
	if writer.writes != 2 {
		t.Fatalf("probe error must fail closed toward writing; writes=%d (want 2)", writer.writes)
	}
}

// TestGrafanaSync_MissingConfigMapForcesRewrite_RealWriter is the end-to-end
// regression for ka-0000002299, exercised through the REAL
// GrafanaConfigMapWriter against a fake k8s clientset (not the fakeWriter) so it
// reproduces the exact prod incident: a chart dashboard whose bead still carries
// its stamped dashboard_hash, but whose grafana-dashboard-<uid> ConfigMap was
// manually deleted, must be re-provisioned by SyncDashboard.
func TestGrafanaSync_MissingConfigMapForcesRewrite_RealWriter(t *testing.T) {
	ctx := context.Background()
	daemon := newFakeDashboardDaemon()
	store := NewDashboardStore(daemon, "test")
	client := fake.NewClientset()
	writer := NewGrafanaConfigMapWriter(client, "gardenio-system", slog.Default())
	sync := NewGrafanaSync(GrafanaSyncConfig{Store: store, Writer: writer, Logger: slog.Default(), Projects: []string{"gardenio"}})

	uid := "cicd-minutes"
	cmName := configMapName(uid)
	model := []byte(`{"uid":"cicd-minutes","title":"CICD Minutes","version":1,"panels":[]}`)
	dashID, _ := daemon.CreateBead(ctx, beadsapi.CreateBeadRequest{
		Type:   "dashboard",
		Labels: []string{"project:gardenio"},
		Title:  "CICD Minutes",
		Fields: json.RawMessage(`{"dashboard_uid":"cicd-minutes"}`),
	})
	if _, err := store.PutModel(ctx, dashID, model); err != nil {
		t.Fatalf("put model: %v", err)
	}

	// First sync provisions the ConfigMap and stamps the hash on the bead.
	sync.SyncDashboard(ctx, dashID)
	if _, err := client.CoreV1().ConfigMaps("gardenio-system").Get(ctx, cmName, metav1.GetOptions{}); err != nil {
		t.Fatalf("expected CM %q after initial sync: %v", cmName, err)
	}
	b, _ := daemon.GetBead(ctx, dashID)
	if b.Fields["dashboard_hash"] == "" {
		t.Fatal("expected dashboard_hash stamped after initial sync")
	}

	// The incident: manually delete the ConfigMap (dup-uid lockout remediation)
	// while the bead keeps its stamped hash. Clear the in-memory restart dedup so
	// this asserts the hash-loop-breaker path via the real presence probe.
	if err := client.CoreV1().ConfigMaps("gardenio-system").Delete(ctx, cmName, metav1.DeleteOptions{}); err != nil {
		t.Fatalf("delete cm: %v", err)
	}
	sync.mu.Lock()
	sync.seen = map[string]time.Time{}
	sync.mu.Unlock()

	// Catch-up / SSE re-runs the sync. Before the fix this no-op'd on the stamped
	// hash and left the dashboard dark. Now it must re-provision the CM.
	sync.SyncDashboard(ctx, dashID)
	cm, err := client.CoreV1().ConfigMaps("gardenio-system").Get(ctx, cmName, metav1.GetOptions{})
	if err != nil {
		t.Fatalf("missing ConfigMap must be re-provisioned; got err=%v", err)
	}
	if cm.Data[configMapKey(uid)] != string(model) {
		t.Fatalf("re-provisioned CM has wrong data: %v", cm.Data)
	}
}

func TestGrafanaSync_SkipsBeadWithoutUID(t *testing.T) {
	daemon := newFakeDashboardDaemon()
	store := NewDashboardStore(daemon, "test")
	writer := newFakeWriter()
	sync := NewGrafanaSync(GrafanaSyncConfig{Store: store, Writer: writer, Logger: slog.Default(), Projects: []string{"gardenio"}})
	ctx := context.Background()

	dashID, _ := daemon.CreateBead(ctx, beadsapi.CreateBeadRequest{Type: "dashboard", Title: "no-uid", Labels: []string{"project:gardenio"}})
	_, _ = store.PutModel(ctx, dashID, []byte(`{"title":"x"}`))
	sync.SyncDashboard(ctx, dashID)
	if writer.writes != 0 {
		t.Fatalf("expected no write for uid-less bead, got %d", writer.writes)
	}
}

func TestGrafanaSync_HandleEventIgnoresNonDashboard(t *testing.T) {
	daemon := newFakeDashboardDaemon()
	store := NewDashboardStore(daemon, "test")
	writer := newFakeWriter()
	sync := NewGrafanaSync(GrafanaSyncConfig{Store: store, Writer: writer, Logger: slog.Default(), Projects: []string{"gardenio"}})

	sync.handleEvent(context.Background(), []byte(`{"bead":{"id":"gd-1","type":"task","status":"open"}}`))
	if writer.writes != 0 {
		t.Fatalf("expected non-dashboard event ignored, got %d writes", writer.writes)
	}
}

func TestGrafanaSync_RegisterHandlers(t *testing.T) {
	daemon := newFakeDashboardDaemon()
	store := NewDashboardStore(daemon, "test")
	sync := NewGrafanaSync(GrafanaSyncConfig{Store: store, Writer: newFakeWriter(), Logger: slog.Default(), Projects: []string{"gardenio"}})
	stream := NewSSEStream(SSEStreamConfig{BeadsHTTPAddr: "http://x", Logger: slog.Default()})
	sync.RegisterHandlers(stream) // must not panic; registers create+updated

	// Drive a dashboard create event through the stream handler path.
	dashID, _ := daemon.CreateBead(context.Background(), beadsapi.CreateBeadRequest{
		Type:   "dashboard",
		Labels: []string{"project:gardenio"},
		Fields: json.RawMessage(`{"dashboard_uid":"u1"}`),
	})
	_, _ = store.PutModel(context.Background(), dashID, []byte(`{"uid":"u1","title":"U"}`))
	// handleEvent is what the stream dispatches to (SSE wraps the bead).
	sync.handleEvent(context.Background(), []byte(`{"bead":{"id":"`+dashID+`","type":"dashboard","status":"open"}}`))
	b, _ := daemon.GetBead(context.Background(), dashID)
	if b.Fields["dashboard_hash"] == "" {
		t.Fatal("expected sync via handleEvent to stamp hash")
	}
}

func TestGrafanaSync_SkipsBeadWithoutModelDoc(t *testing.T) {
	daemon := newFakeDashboardDaemon()
	store := NewDashboardStore(daemon, "test")
	writer := newFakeWriter()
	sync := NewGrafanaSync(GrafanaSyncConfig{Store: store, Writer: writer, Logger: slog.Default(), Projects: []string{"gardenio"}})
	// Dashboard bead with a uid but NO attached model doc.
	dashID, _ := daemon.CreateBead(context.Background(), beadsapi.CreateBeadRequest{
		Type:   "dashboard",
		Labels: []string{"project:gardenio"},
		Fields: json.RawMessage(`{"dashboard_uid":"u2"}`),
	})
	sync.SyncDashboard(context.Background(), dashID)
	if writer.writes != 0 {
		t.Fatalf("expected no write when model doc missing, got %d", writer.writes)
	}
}

func TestGrafanaSync_MissingBead(t *testing.T) {
	daemon := newFakeDashboardDaemon()
	store := NewDashboardStore(daemon, "test")
	writer := newFakeWriter()
	sync := NewGrafanaSync(GrafanaSyncConfig{Store: store, Writer: writer, Logger: slog.Default(), Projects: []string{"gardenio"}})
	sync.SyncDashboard(context.Background(), "gd-nonexistent")
	if writer.writes != 0 {
		t.Fatalf("expected no write for missing bead, got %d", writer.writes)
	}
}

// TestListActiveDashboardIDs verifies the startup-catch-up enumerator returns
// every active dashboard bead and excludes closed ones (so a scoped-off /
// deprecated dashboard is not re-provisioned on restart) and non-dashboard
// types.
func TestListActiveDashboardIDs(t *testing.T) {
	daemon := newFakeDashboardDaemon()
	store := NewDashboardStore(daemon, "test")
	ctx := context.Background()

	openID, _ := daemon.CreateBead(ctx, beadsapi.CreateBeadRequest{Type: "dashboard", Fields: json.RawMessage(`{"dashboard_uid":"a"}`)})
	blockedID, _ := daemon.CreateBead(ctx, beadsapi.CreateBeadRequest{Type: "dashboard", Fields: json.RawMessage(`{"dashboard_uid":"b"}`)})
	closedID, _ := daemon.CreateBead(ctx, beadsapi.CreateBeadRequest{Type: "dashboard", Fields: json.RawMessage(`{"dashboard_uid":"c"}`)})
	otherID, _ := daemon.CreateBead(ctx, beadsapi.CreateBeadRequest{Type: "task"})
	// CreateBead stamps status=open; mutate two directly (same-package test).
	daemon.beads[blockedID].Status = "blocked"
	daemon.beads[closedID].Status = "closed"

	ids, err := store.ListActiveDashboardIDs(ctx, 256)
	if err != nil {
		t.Fatalf("ListActiveDashboardIDs: %v", err)
	}
	got := map[string]bool{}
	for _, id := range ids {
		got[id] = true
	}
	if !got[openID] || !got[blockedID] {
		t.Fatalf("expected open + blocked dashboards, got %v", ids)
	}
	if got[closedID] {
		t.Fatalf("closed dashboard must be excluded, got %v", ids)
	}
	if got[otherID] {
		t.Fatalf("non-dashboard type must be excluded, got %v", ids)
	}
	if len(ids) != 2 {
		t.Fatalf("expected exactly 2 active dashboards, got %d (%v)", len(ids), ids)
	}
}

// A dashboard bead syncs into fleet Grafana only when every project: label it
// carries is on the allow-list, and a refused bead neither writes nor removes a
// ConfigMap (gd-rbEoXGobIg).
func TestSyncDashboard_ProjectFence(t *testing.T) {
	cases := []struct {
		name   string
		labels []string
		sync   bool
	}{
		{"fleet project", []string{"project:gardenio"}, true},
		{"other project", []string{"project:tomato-patch"}, false},
		{"one of two labels differs", []string{"project:gardenio", "project:tomato-patch"}, false},
		{"no project label", []string{"grafana:dashboard"}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			daemon := newFakeDashboardDaemon()
			store := NewDashboardStore(daemon, "test")
			writer := newFakeWriter()
			writer.chartOwned["fleet-board"] = true
			sync := NewGrafanaSync(GrafanaSyncConfig{Store: store, Writer: writer, Logger: slog.Default(), Projects: []string{"gardenio"}})
			ctx := context.Background()

			dashID, _ := daemon.CreateBead(ctx, beadsapi.CreateBeadRequest{
				Type:   "dashboard",
				Labels: tc.labels,
				Fields: json.RawMessage(`{"dashboard_uid":"other-board"}`),
			})
			if _, err := store.PutModel(ctx, dashID, []byte(`{"uid":"other-board","title":"O"}`)); err != nil {
				t.Fatalf("put model: %v", err)
			}
			// A chart-owned uid would trigger RemoveOwn; a refused bead must not.
			chartID, _ := daemon.CreateBead(ctx, beadsapi.CreateBeadRequest{
				Type:   "dashboard",
				Labels: tc.labels,
				Fields: json.RawMessage(`{"dashboard_uid":"fleet-board"}`),
			})

			sync.SyncDashboard(ctx, dashID)
			sync.SyncDashboard(ctx, chartID)

			if got := writer.writes == 1; got != tc.sync {
				t.Errorf("wrote = %v (writes=%d), want %v", got, writer.writes, tc.sync)
			}
			if got := len(writer.removed) == 1; got != tc.sync {
				t.Errorf("removed shadow = %v (%v), want %v", got, writer.removed, tc.sync)
			}
		})
	}

	t.Run("empty allow-list syncs nothing", func(t *testing.T) {
		daemon := newFakeDashboardDaemon()
		store := NewDashboardStore(daemon, "test")
		writer := newFakeWriter()
		sync := NewGrafanaSync(GrafanaSyncConfig{Store: store, Writer: writer, Logger: slog.Default()})
		ctx := context.Background()
		dashID, _ := daemon.CreateBead(ctx, beadsapi.CreateBeadRequest{
			Type:   "dashboard",
			Labels: []string{"project:gardenio"},
			Fields: json.RawMessage(`{"dashboard_uid":"u"}`),
		})
		_, _ = store.PutModel(ctx, dashID, []byte(`{"uid":"u"}`))
		sync.SyncDashboard(ctx, dashID)
		if writer.writes != 0 {
			t.Fatalf("expected no write with no allowed projects, got %d", writer.writes)
		}
	})
}

// A second bead claiming a uid already provisioned from another bead must not
// replace that board, even inside an allowed project (gd-rbEoXGobIg).
func TestSyncDashboard_UIDCollisionRefused(t *testing.T) {
	ctx := context.Background()
	daemon := newFakeDashboardDaemon()
	store := NewDashboardStore(daemon, "test")
	client := fake.NewClientset()
	writer := NewGrafanaConfigMapWriter(client, "gardenio-system", slog.Default())
	sync := NewGrafanaSync(GrafanaSyncConfig{Store: store, Writer: writer, Logger: slog.Default(), Projects: []string{"gardenio"}})

	mk := func(title string) string {
		id, _ := daemon.CreateBead(ctx, beadsapi.CreateBeadRequest{
			Type:   "dashboard",
			Labels: []string{"project:gardenio"},
			Fields: json.RawMessage(`{"dashboard_uid":"fleet-costs"}`),
		})
		if _, err := store.PutModel(ctx, id, []byte(`{"uid":"fleet-costs","title":"`+title+`"}`)); err != nil {
			t.Fatalf("put model: %v", err)
		}
		return id
	}
	owner := mk("Owner")
	intruder := mk("Intruder")

	sync.SyncDashboard(ctx, owner)
	sync.SyncDashboard(ctx, intruder)

	cm, err := client.CoreV1().ConfigMaps("gardenio-system").Get(ctx, configMapName("fleet-costs"), metav1.GetOptions{})
	if err != nil {
		t.Fatalf("get cm: %v", err)
	}
	if got := cm.Annotations[grafanaBeadAnnotation]; got != owner {
		t.Fatalf("cm provisioned from %q, want owner %q", got, owner)
	}
	if want := `{"uid":"fleet-costs","title":"Owner"}`; cm.Data[configMapKey("fleet-costs")] != want {
		t.Fatalf("intruder replaced the board: %v", cm.Data)
	}
	if b, _ := daemon.GetBead(ctx, intruder); b.Fields["dashboard_hash"] != "" {
		t.Fatalf("refused bead must not be stamped as synced, got hash %q", b.Fields["dashboard_hash"])
	}

	// The owner still updates its own board.
	if _, err := store.PutModel(ctx, owner, []byte(`{"uid":"fleet-costs","title":"Owner v2"}`)); err != nil {
		t.Fatalf("put model v2: %v", err)
	}
	sync.SyncDashboard(ctx, owner)
	cm, _ = client.CoreV1().ConfigMaps("gardenio-system").Get(ctx, configMapName("fleet-costs"), metav1.GetOptions{})
	if want := `{"uid":"fleet-costs","title":"Owner v2"}`; cm.Data[configMapKey("fleet-costs")] != want {
		t.Fatalf("owner update not written: %v", cm.Data)
	}
}
