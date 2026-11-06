package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"gardenio/controller/internal/beadsapi"
	"gardenio/controller/internal/bridge"
)

func TestEnvOrDefault(t *testing.T) {
	t.Setenv("GB_TEST_KEY", "set-value")
	if got := envOrDefault("GB_TEST_KEY", "fallback"); got != "set-value" {
		t.Errorf("got %q, want set-value", got)
	}
	if got := envOrDefault("GB_TEST_UNSET_KEY", "fallback"); got != "fallback" {
		t.Errorf("got %q, want fallback", got)
	}
}

func TestEnvBool(t *testing.T) {
	cases := []struct {
		val  string
		def  bool
		want bool
	}{
		{"1", false, true},
		{"true", false, true},
		{"YES", false, true},
		{"on", false, true},
		{"0", true, false},
		{"false", true, false},
		{"no", true, false},
		{"off", true, false},
		{"", true, true},        // unset → default
		{"", false, false},      // unset → default
		{"garbage", true, true}, // unrecognized → default
	}
	for _, c := range cases {
		if c.val == "" {
			os.Unsetenv("GB_TEST_BOOL")
		} else {
			t.Setenv("GB_TEST_BOOL", c.val)
		}
		if got := envBool("GB_TEST_BOOL", c.def); got != c.want {
			t.Errorf("envBool(%q, %v) = %v, want %v", c.val, c.def, got, c.want)
		}
	}
}

func TestParseConfig_Defaults(t *testing.T) {
	for _, k := range []string{
		"BEADS_HTTP_ADDR", "POD_NAMESPACE", "KUBECONFIG", "GRAFANA_DASHBOARDS_DIR",
		"GRAFANA_BACKFILL", "GRAFANA_CATCHUP", "GRAFANA_CATCHUP_LIMIT", "GRAFANA_PROJECT",
		"GRAFANA_LISTEN_ADDR", "LOG_LEVEL", "STATE_PATH",
		"GRAFANA_URL", "GRAFANA_TOKEN", "GRAFANA_READBACK_INTERVAL", "GRAFANA_CONFLICT_POLICY",
	} {
		os.Unsetenv(k)
	}
	cfg := parseConfig()
	if !cfg.catchup {
		t.Error("catchup should default true (startup self-heal)")
	}
	if cfg.catchupLimit != 256 {
		t.Errorf("catchupLimit default: %d, want 256", cfg.catchupLimit)
	}
	if cfg.grafanaURL != "" {
		t.Errorf("grafanaURL should default empty (read-back disabled), got %q", cfg.grafanaURL)
	}
	if cfg.readbackInterval != 60*time.Second {
		t.Errorf("readbackInterval default: %v", cfg.readbackInterval)
	}
	if cfg.conflictPolicy != "grafana-wins" {
		t.Errorf("conflictPolicy default: %q", cfg.conflictPolicy)
	}
	if cfg.beadsHTTPAddr != "http://localhost:8080" {
		t.Errorf("beadsHTTPAddr default: %q", cfg.beadsHTTPAddr)
	}
	if cfg.namespace != "default" {
		t.Errorf("namespace default: %q", cfg.namespace)
	}
	if cfg.backfill {
		t.Error("backfill should default false")
	}
	if cfg.project != "gardenio" {
		t.Errorf("project default: %q", cfg.project)
	}
	if cfg.listenAddr != ":8093" {
		t.Errorf("listenAddr default: %q", cfg.listenAddr)
	}
}

func TestParseConfig_Overrides(t *testing.T) {
	t.Setenv("BEADS_HTTP_ADDR", "http://beads:9090")
	t.Setenv("POD_NAMESPACE", "gardenio")
	t.Setenv("GRAFANA_DASHBOARDS_DIR", "/dash")
	t.Setenv("GRAFANA_BACKFILL", "true")
	t.Setenv("GRAFANA_CATCHUP", "false")
	t.Setenv("GRAFANA_CATCHUP_LIMIT", "50")
	t.Setenv("GRAFANA_PROJECT", "other")
	t.Setenv("GRAFANA_LISTEN_ADDR", ":7000")
	t.Setenv("GRAFANA_URL", "http://grafana:3000")
	t.Setenv("GRAFANA_TOKEN", "tok")
	t.Setenv("GRAFANA_READBACK_INTERVAL", "30s")
	t.Setenv("GRAFANA_CONFLICT_POLICY", "manual")
	cfg := parseConfig()
	if cfg.beadsHTTPAddr != "http://beads:9090" || cfg.namespace != "gardenio" ||
		cfg.dashboardsDir != "/dash" || !cfg.backfill || cfg.project != "other" ||
		cfg.listenAddr != ":7000" {
		t.Fatalf("unexpected cfg: %+v", cfg)
	}
	if cfg.catchup {
		t.Errorf("catchup override to false not honored: %+v", cfg)
	}
	if cfg.catchupLimit != 50 {
		t.Errorf("catchupLimit override: %d, want 50", cfg.catchupLimit)
	}
	if cfg.grafanaURL != "http://grafana:3000" || cfg.grafanaToken != "tok" ||
		cfg.readbackInterval != 30*time.Second || cfg.conflictPolicy != "manual" {
		t.Fatalf("unexpected read-back cfg: %+v", cfg)
	}
}

func TestParseConfig_BadReadbackInterval(t *testing.T) {
	t.Setenv("GRAFANA_READBACK_INTERVAL", "not-a-duration")
	cfg := parseConfig()
	if cfg.readbackInterval != 60*time.Second {
		t.Errorf("bad interval should fall back to 60s, got %v", cfg.readbackInterval)
	}
}

func TestSetupLogger(t *testing.T) {
	for _, lvl := range []string{"debug", "info", "warn", "error", "unknown"} {
		if l := setupLogger(lvl); l == nil {
			t.Errorf("setupLogger(%q) returned nil", lvl)
		}
	}
}

func TestRegisterHealthHandlers(t *testing.T) {
	mux := http.NewServeMux()
	rdy := &readiness{}
	rdy.markReady()
	registerHealthHandlers(mux, "v1.2.3", rdy)

	for _, path := range []string{"/healthz", "/readyz"} {
		req := httptest.NewRequest(http.MethodGet, path, nil)
		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, req)
		if rec.Code != http.StatusOK {
			t.Errorf("%s: status %d", path, rec.Code)
		}
		if ct := rec.Header().Get("Content-Type"); ct != "application/json" {
			t.Errorf("%s: content-type %q", path, ct)
		}
	}
	// /healthz reports the version.
	req := httptest.NewRequest(http.MethodGet, "/healthz", nil)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if body := rec.Body.String(); body != `{"status":"ok","version":"v1.2.3"}` {
		t.Errorf("healthz body: %q", body)
	}
}

func TestBuildK8sClient_BadKubeconfig(t *testing.T) {
	// A non-existent kubeconfig path must error rather than panic.
	if _, err := buildK8sClient("/nonexistent/kubeconfig/path"); err == nil {
		t.Fatal("expected error for bad kubeconfig path")
	}
}

// ---- runBackfill ----------------------------------------------------------

// fakeCmdDaemon implements bridge.DashboardDaemon for runBackfill tests.
type fakeCmdDaemon struct {
	mu     sync.Mutex
	beads  map[string]*beadsapi.BeadDetail
	deps   map[string][]beadsapi.Dependency
	nextID int
}

func newFakeCmdDaemon() *fakeCmdDaemon {
	return &fakeCmdDaemon{beads: map[string]*beadsapi.BeadDetail{}, deps: map[string][]beadsapi.Dependency{}}
}

func (f *fakeCmdDaemon) CreateBead(_ context.Context, req beadsapi.CreateBeadRequest) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.nextID++
	id := fmt.Sprintf("gd-c%d", f.nextID)
	fields := map[string]string{}
	if len(req.Fields) > 0 {
		var raw map[string]any
		_ = json.Unmarshal(req.Fields, &raw)
		for k, v := range raw {
			fields[k] = fmt.Sprintf("%v", v)
		}
	}
	f.beads[id] = &beadsapi.BeadDetail{ID: id, Title: req.Title, Type: req.Type, Status: "open", Labels: append([]string{}, req.Labels...), Fields: fields}
	return id, nil
}

func (f *fakeCmdDaemon) GetBead(_ context.Context, id string) (*beadsapi.BeadDetail, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	b, ok := f.beads[id]
	if !ok {
		return nil, fmt.Errorf("not found: %s", id)
	}
	cp := *b
	return &cp, nil
}

func (f *fakeCmdDaemon) UpdateBeadDescription(_ context.Context, _, _ string) error { return nil }

func (f *fakeCmdDaemon) AddComment(_ context.Context, _, _, _ string) error { return nil }

func (f *fakeCmdDaemon) UpdateBeadFields(_ context.Context, id string, fields map[string]string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if b, ok := f.beads[id]; ok {
		if b.Fields == nil {
			b.Fields = map[string]string{}
		}
		for k, v := range fields {
			b.Fields[k] = v
		}
	}
	return nil
}

func (f *fakeCmdDaemon) AddDependency(_ context.Context, beadID, dependsOnID, depType, _ string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.deps[dependsOnID] = append(f.deps[dependsOnID], beadsapi.Dependency{BeadID: beadID, DependsOnID: dependsOnID, Type: depType})
	return nil
}

func (f *fakeCmdDaemon) GetReverseDependencies(_ context.Context, beadID string) ([]beadsapi.Dependency, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]beadsapi.Dependency{}, f.deps[beadID]...), nil
}

func (f *fakeCmdDaemon) AddLabel(_ context.Context, beadID, label string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if b, ok := f.beads[beadID]; ok {
		b.Labels = append(b.Labels, label)
	}
	return nil
}

func (f *fakeCmdDaemon) ListBeadsFiltered(_ context.Context, q beadsapi.ListBeadsQuery) (*beadsapi.ListBeadsResult, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []*beadsapi.BeadDetail
	for _, b := range f.beads {
		if len(q.Types) > 0 && !containsStr(q.Types, b.Type) {
			continue
		}
		if len(q.Statuses) > 0 && !containsStr(q.Statuses, b.Status) {
			continue
		}
		match := true
		for k, v := range q.Fields {
			if b.Fields[k] != v {
				match = false
				break
			}
		}
		if match {
			cp := *b
			out = append(out, &cp)
		}
	}
	return &beadsapi.ListBeadsResult{Beads: out, Total: len(out)}, nil
}

func containsStr(ss []string, want string) bool {
	for _, s := range ss {
		if s == want {
			return true
		}
	}
	return false
}

func TestRunBackfill_EmptyDir(t *testing.T) {
	// Unset dir → no-op, no panic.
	store := bridge.NewDashboardStore(newFakeCmdDaemon(), "test")
	runBackfill(context.Background(), store, "", "gardenio", slog.Default())
}

func TestRunBackfill_MissingDir(t *testing.T) {
	store := bridge.NewDashboardStore(newFakeCmdDaemon(), "test")
	runBackfill(context.Background(), store, "/no/such/dir", "gardenio", slog.Default())
}

func TestRunBackfill_NoJSONFiles(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "readme.txt"), []byte("hi"), 0o644); err != nil {
		t.Fatal(err)
	}
	store := bridge.NewDashboardStore(newFakeCmdDaemon(), "test")
	runBackfill(context.Background(), store, dir, "gardenio", slog.Default())
}

func TestRunBackfill_ImportsDashboards(t *testing.T) {
	dir := t.TempDir()
	models := map[string]string{
		"cost.json":  `{"uid":"cost","title":"Cost","version":1}`,
		"embed.json": `{"uid":"embed","title":"Embed","version":1}`,
		"bad.json":   `{"title":"no uid"}`, // imported but errors (no uid)
	}
	for name, body := range models {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	daemon := newFakeCmdDaemon()
	store := bridge.NewDashboardStore(daemon, "test")
	runBackfill(context.Background(), store, dir, "gardenio", slog.Default())

	// The two valid dashboards should now have beads.
	list, _ := daemon.ListBeadsFiltered(context.Background(), beadsapi.ListBeadsQuery{})
	got := 0
	for _, b := range list.Beads {
		if b.Type == "dashboard" {
			got++
		}
	}
	if got != 2 {
		t.Fatalf("expected 2 dashboard beads, got %d", got)
	}
}

// countingWriter records how many Upsert calls (i.e. ConfigMap writes) happen,
// so catch-up tests can assert exactly which dashboards were synced. It also
// tracks per-uid ConfigMap presence so the presence probe (ka-0000002299) sees a
// realistic state: Upsert marks a uid present, and deleteCM simulates an
// out-of-band delete.
type countingWriter struct {
	mu      sync.Mutex
	uids    []string
	present map[string]bool
}

func (w *countingWriter) Upsert(_ context.Context, uid, _ string, _ []byte) (string, string, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.uids = append(w.uids, uid)
	if w.present == nil {
		w.present = map[string]bool{}
	}
	w.present[uid] = true
	return "cm-" + uid, uid + ".json", nil
}

func (*countingWriter) ChartOwnsDashboard(_ context.Context, _ string) (bool, error) {
	return false, nil
}

func (*countingWriter) RemoveOwn(_ context.Context, _ string) error { return nil }

func (w *countingWriter) Exists(_ context.Context, uid string) (bool, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.present[uid], nil
}

// deleteCM simulates an out-of-band ConfigMap delete for a uid: the bead keeps
// its stamped dashboard_hash but the artifact is gone.
func (w *countingWriter) deleteCM(uid string) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.present == nil {
		w.present = map[string]bool{}
	}
	w.present[uid] = false
}

// TestRunCatchup_SyncsPreExistingDashboards is the regression test for the
// incident (ka-0000004760): a dashboard bead that already existed before the
// bridge started — i.e. never delivered via the "from now" SSE stream — must be
// provisioned by the startup catch-up. A closed dashboard must be skipped, and a
// dashboard already in sync (hash matches) must not be re-written.
func TestRunCatchup_SyncsPreExistingDashboards(t *testing.T) {
	ctx := context.Background()
	daemon := newFakeCmdDaemon()
	store := bridge.NewDashboardStore(daemon, "test")
	writer := &countingWriter{}
	sync := bridge.NewGrafanaSync(bridge.GrafanaSyncConfig{Store: store, Writer: writer, Logger: slog.Default(), Projects: []string{"gardenio"}})

	// Two active dashboards with model docs (the pre-existing case).
	d1, _ := daemon.CreateBead(ctx, beadsapi.CreateBeadRequest{Type: "dashboard", Labels: []string{"project:gardenio"}, Fields: json.RawMessage(`{"dashboard_uid":"fleet"}`)})
	if _, err := store.PutModel(ctx, d1, []byte(`{"uid":"fleet","title":"Fleet","panels":[]}`)); err != nil {
		t.Fatal(err)
	}
	d2, _ := daemon.CreateBead(ctx, beadsapi.CreateBeadRequest{Type: "dashboard", Labels: []string{"project:gardenio"}, Fields: json.RawMessage(`{"dashboard_uid":"costs"}`)})
	if _, err := store.PutModel(ctx, d2, []byte(`{"uid":"costs","title":"Costs","panels":[]}`)); err != nil {
		t.Fatal(err)
	}
	// A CLOSED dashboard must be excluded from the catch-up.
	dClosed, _ := daemon.CreateBead(ctx, beadsapi.CreateBeadRequest{Type: "dashboard", Labels: []string{"project:gardenio"}, Fields: json.RawMessage(`{"dashboard_uid":"dead"}`)})
	_, _ = store.PutModel(ctx, dClosed, []byte(`{"uid":"dead","title":"Dead","panels":[]}`))
	daemon.mu.Lock()
	daemon.beads[dClosed].Status = "closed"
	daemon.mu.Unlock()

	runCatchup(ctx, sync, store, 256, slog.Default())

	writer.mu.Lock()
	got := map[string]bool{}
	for _, u := range writer.uids {
		got[u] = true
	}
	n := len(writer.uids)
	writer.mu.Unlock()

	if !got["fleet"] || !got["costs"] {
		t.Fatalf("catch-up must sync active dashboards fleet+costs, wrote %v", writer.uids)
	}
	if got["dead"] {
		t.Fatalf("catch-up must NOT sync closed dashboard, wrote %v", writer.uids)
	}
	if n != 2 {
		t.Fatalf("expected exactly 2 catch-up writes, got %d (%v)", n, writer.uids)
	}

	// Idempotency: a second catch-up must be a no-op (hashes now match).
	runCatchup(ctx, sync, store, 256, slog.Default())
	writer.mu.Lock()
	n2 := len(writer.uids)
	writer.mu.Unlock()
	if n2 != 2 {
		t.Fatalf("second catch-up must be a no-op (loop-breaker), total writes now %d", n2)
	}
}

// TestRunCatchup_ReprovisionsMissingConfigMap is the catch-up-level regression
// for ka-0000002299: a dashboard bead whose model still matches its stamped
// dashboard_hash but whose ConfigMap was deleted out-of-band must be
// re-provisioned by the startup catch-up. Before the fix the hash loop-breaker
// short-circuited and catch-up left the dashboard permanently dark.
func TestRunCatchup_ReprovisionsMissingConfigMap(t *testing.T) {
	ctx := context.Background()
	daemon := newFakeCmdDaemon()
	store := bridge.NewDashboardStore(daemon, "test")
	writer := &countingWriter{}
	sync := bridge.NewGrafanaSync(bridge.GrafanaSyncConfig{Store: store, Writer: writer, Logger: slog.Default(), Projects: []string{"gardenio"}})

	d1, _ := daemon.CreateBead(ctx, beadsapi.CreateBeadRequest{Type: "dashboard", Labels: []string{"project:gardenio"}, Fields: json.RawMessage(`{"dashboard_uid":"cicd-minutes"}`)})
	if _, err := store.PutModel(ctx, d1, []byte(`{"uid":"cicd-minutes","title":"CICD","panels":[]}`)); err != nil {
		t.Fatal(err)
	}

	// First catch-up provisions the CM + stamps the hash.
	runCatchup(ctx, sync, store, 256, slog.Default())
	writer.mu.Lock()
	n1 := len(writer.uids)
	writer.mu.Unlock()
	if n1 != 1 {
		t.Fatalf("expected 1 initial catch-up write, got %d (%v)", n1, writer.uids)
	}

	// Out-of-band delete of the CM; the bead keeps its stamped hash.
	writer.deleteCM("cicd-minutes")

	// A second catch-up (bridge restart) must re-provision the missing CM rather
	// than no-op on the unchanged hash.
	runCatchup(ctx, sync, store, 256, slog.Default())
	writer.mu.Lock()
	n2 := len(writer.uids)
	writer.mu.Unlock()
	if n2 != 2 {
		t.Fatalf("catch-up must re-provision the deleted CM; total writes=%d (want 2, %v)", n2, writer.uids)
	}

	// Third catch-up with the CM now present must be a no-op again.
	runCatchup(ctx, sync, store, 256, slog.Default())
	writer.mu.Lock()
	n3 := len(writer.uids)
	writer.mu.Unlock()
	if n3 != 2 {
		t.Fatalf("healthy catch-up must be a no-op; total writes=%d (want 2)", n3)
	}
}

func TestRunCatchup_EmptyIsNoOp(t *testing.T) {
	daemon := newFakeCmdDaemon()
	store := bridge.NewDashboardStore(daemon, "test")
	writer := &countingWriter{}
	sync := bridge.NewGrafanaSync(bridge.GrafanaSyncConfig{Store: store, Writer: writer, Logger: slog.Default(), Projects: []string{"gardenio"}})
	// No dashboards at all → no panic, no writes.
	runCatchup(context.Background(), sync, store, 256, slog.Default())
	if len(writer.uids) != 0 {
		t.Fatalf("expected no writes for empty catch-up, got %v", writer.uids)
	}
}

func TestRun_StandsUpAndShutsDown(t *testing.T) {
	dir := t.TempDir()
	// A backfill dir with one valid dashboard so the backfill branch runs too.
	if err := os.WriteFile(filepath.Join(dir, "d.json"),
		[]byte(`{"uid":"d","title":"D","version":1}`), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg := &config{
		beadsHTTPAddr: "http://localhost:0",
		namespace:     "gardenio",
		dashboardsDir: dir,
		backfill:      true,
		project:       "gardenio",
		listenAddr:    "127.0.0.1:0", // ephemeral port
		statePath:     filepath.Join(t.TempDir(), "state.json"),
	}
	daemon := newFakeCmdDaemon()
	store := bridge.NewDashboardStore(daemon, "test")
	writer := &cmdFakeWriter{}

	// Pre-canceled context → run wires everything up, then returns at <-ctx.Done().
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	done := make(chan error, 1)
	go func() { done <- run(ctx, cfg, slog.Default(), store, writer, nil) }()

	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("run returned error: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("run did not return after context cancel")
	}

	// Backfill should have created the dashboard bead.
	list, _ := daemon.ListBeadsFiltered(context.Background(), beadsapi.ListBeadsQuery{})
	found := false
	for _, b := range list.Beads {
		if b.Type == "dashboard" {
			found = true
		}
	}
	if !found {
		t.Error("expected backfill to create a dashboard bead")
	}
}

// cmdFakeWriter is a no-op DashboardWriter for run() wiring tests.
type cmdFakeWriter struct{}

func (cmdFakeWriter) Upsert(_ context.Context, uid, _ string, _ []byte) (string, string, error) {
	return "cm-" + uid, uid + ".json", nil
}

func (cmdFakeWriter) ChartOwnsDashboard(_ context.Context, _ string) (bool, error) {
	return false, nil
}

func (cmdFakeWriter) RemoveOwn(_ context.Context, _ string) error { return nil }

func (cmdFakeWriter) Exists(_ context.Context, _ string) (bool, error) { return true, nil }

// TestNewBeadsClient_StampsSystemOperator pins the fix for bug ka-0000007346:
// the grafana-bridge daemon client MUST stamp X-System-Operator on its writes.
// Without it, EnsureConfigs and all 18 dashboard backfills hit the daemon's
// post-R2 fail-closed write path (HTTP 400 "operator could not be resolved"),
// so the double-store-removal deploy provisions 0/18 dashboards. This is a
// deploy-time-only failure (the chart-owned path used to defer the bridge), so
// it must be guarded by a test, not just caught in staging.
func TestNewBeadsClient_StampsSystemOperator(t *testing.T) {
	var gotSystem, gotOperator string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotSystem = r.Header.Get("X-System-Operator")
		gotOperator = r.Header.Get("X-Operated-By")
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte(`{"id":"gd-test0000001"}`))
	}))
	defer srv.Close()

	daemon, err := newBeadsClient(srv.URL, "test-token", nil)
	if err != nil {
		t.Fatalf("newBeadsClient: %v", err)
	}
	defer daemon.Close()

	// Exercise a real write through the same public API the backfill uses.
	if _, err := daemon.CreateBead(context.Background(), beadsapi.CreateBeadRequest{
		Title: "probe",
		Type:  "dashboard",
		Kind:  "issue",
	}); err != nil {
		t.Fatalf("CreateBead: %v", err)
	}

	if gotSystem != "grafana-bridge" {
		t.Errorf("X-System-Operator = %q; want %q — grafana-bridge writes must carry the machine operator or the daemon 400s (ka-0000007346)", gotSystem, "grafana-bridge")
	}
	if gotOperator != "" {
		t.Errorf("X-Operated-By = %q; want empty — machine-origin writes must not spoof a human operator", gotOperator)
	}
}

// --- gd-pj5hT3V1la: startup must survive the machine-client reconcile race ---

// /readyz is 503 until run() marks the startup sequence done; /healthz is ok
// throughout, so liveness never restarts a pod that is waiting for its token.
func TestRegisterHealthHandlers_ReadyzGatedOnStartup(t *testing.T) {
	mux := http.NewServeMux()
	rdy := &readiness{}
	registerHealthHandlers(mux, "v1", rdy)
	get := func(path string) *httptest.ResponseRecorder {
		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))
		return rec
	}
	if rec := get("/healthz"); rec.Code != http.StatusOK {
		t.Errorf("healthz before ready: %d", rec.Code)
	}
	if rec := get("/readyz"); rec.Code != http.StatusServiceUnavailable || rec.Body.String() != `{"status":"starting"}` {
		t.Errorf("readyz before ready: %d %s", rec.Code, rec.Body.String())
	}
	rdy.markReady()
	if rec := get("/readyz"); rec.Code != http.StatusOK || rec.Body.String() != `{"status":"ok"}` {
		t.Errorf("readyz after ready: %d %s", rec.Code, rec.Body.String())
	}
}

func stubRetrySleep(t *testing.T) *[]time.Duration {
	t.Helper()
	var delays []time.Duration
	prev := retrySleep
	retrySleep = func(ctx context.Context, d time.Duration) error {
		delays = append(delays, d)
		return ctx.Err()
	}
	t.Cleanup(func() { retrySleep = prev })
	return &delays
}

func authUnavailableErr(attempt int) error {
	return fmt.Errorf("daemon write: %w: machine token fetch for client gardenio-grafana-bridge failed: oauth2: \"invalid_client\" (attempt %d)", beadsapi.ErrMachineAuthUnavailable, attempt)
}

func TestRetryAuthUnavailable_RetriesWithBackoffUntilSuccess(t *testing.T) {
	delays := stubRetrySleep(t)
	var logBuf bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&logBuf, nil))
	calls := 0
	err := retryAuthUnavailable(context.Background(), logger, "ensure beads configs", func(context.Context) error {
		calls++
		if calls < 4 {
			return authUnavailableErr(calls)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if calls != 4 {
		t.Errorf("calls = %d, want 4", calls)
	}
	want := []time.Duration{time.Second, 2 * time.Second, 4 * time.Second}
	if len(*delays) != len(want) {
		t.Fatalf("delays = %v, want %v", *delays, want)
	}
	for i := range want {
		if (*delays)[i] != want[i] {
			t.Errorf("delay[%d] = %v, want %v", i, (*delays)[i], want[i])
		}
	}
	log := logBuf.String()
	if !strings.Contains(log, "machine credential unavailable; retrying") || !strings.Contains(log, "attempt=3") {
		t.Errorf("each retried attempt must be logged:\n%s", log)
	}
	if !strings.Contains(log, "succeeded after retry") || !strings.Contains(log, "attempts=4") {
		t.Errorf("success after retries must be logged:\n%s", log)
	}
}

func TestRetryAuthUnavailable_BackoffCapsAt30s(t *testing.T) {
	delays := stubRetrySleep(t)
	calls := 0
	_ = retryAuthUnavailable(context.Background(), slog.New(slog.NewTextHandler(&bytes.Buffer{}, nil)), "op", func(context.Context) error {
		calls++
		if calls <= 8 {
			return authUnavailableErr(calls)
		}
		return nil
	})
	for i, d := range *delays {
		if d > retryBackoffMax {
			t.Errorf("delay[%d] = %v exceeds the cap", i, d)
		}
	}
	if last := (*delays)[len(*delays)-1]; last != retryBackoffMax {
		t.Errorf("delays never reached the cap: %v", *delays)
	}
}

// A failure that is not the machine credential is a real answer: it is logged
// as non-fatal and not retried, exactly as the old one-shot seed behaved.
func TestRetryAuthUnavailable_OtherErrorsAreNotRetried(t *testing.T) {
	delays := stubRetrySleep(t)
	var logBuf bytes.Buffer
	calls := 0
	err := retryAuthUnavailable(context.Background(), slog.New(slog.NewTextHandler(&logBuf, nil)), "ensure beads configs", func(context.Context) error {
		calls++
		return errors.New("daemon returned 403 forbidden")
	})
	if err != nil {
		t.Fatalf("non-credential failures are non-fatal: %v", err)
	}
	if calls != 1 || len(*delays) != 0 {
		t.Errorf("calls=%d delays=%v, want one attempt and no sleep", calls, *delays)
	}
	if !strings.Contains(logBuf.String(), "failed to ensure beads configs (non-fatal)") {
		t.Errorf("non-fatal failure must be logged:\n%s", logBuf.String())
	}
}

func TestRetryAuthUnavailable_StopsWhenContextEnds(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	prev := retrySleep
	t.Cleanup(func() { retrySleep = prev })
	sleeps := 0
	retrySleep = func(ctx context.Context, _ time.Duration) error {
		sleeps++
		if sleeps == 2 {
			cancel()
		}
		return ctx.Err()
	}
	calls := 0
	err := retryAuthUnavailable(ctx, slog.New(slog.NewTextHandler(&bytes.Buffer{}, nil)), "op", func(context.Context) error {
		calls++
		return authUnavailableErr(calls)
	})
	if err == nil || !beadsapi.IsAuthUnavailable(err) {
		t.Fatalf("abandoned retry must surface auth_unavailable, got %v", err)
	}
	if calls != 2 {
		t.Errorf("calls = %d, want 2", calls)
	}
}

// flakyDaemon fails every daemon call with auth_unavailable until failures
// are used up: the bridge's client during the seconds before its
// KeycloakClient is reconciled.
type flakyDaemon struct {
	*fakeCmdDaemon
	mu       sync.Mutex
	failures int
	denied   int
}

func (f *flakyDaemon) deny() error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.failures > 0 {
		f.failures--
		f.denied++
		return authUnavailableErr(f.denied)
	}
	return nil
}

func (f *flakyDaemon) ListBeadsFiltered(ctx context.Context, q beadsapi.ListBeadsQuery) (*beadsapi.ListBeadsResult, error) {
	if err := f.deny(); err != nil {
		return nil, err
	}
	return f.fakeCmdDaemon.ListBeadsFiltered(ctx, q)
}

func (f *flakyDaemon) CreateBead(ctx context.Context, req beadsapi.CreateBeadRequest) (string, error) {
	if err := f.deny(); err != nil {
		return "", err
	}
	return f.fakeCmdDaemon.CreateBead(ctx, req)
}

// The backfill pass is rerun after a backoff while its failures are only the
// machine credential, and the rerun imports every dashboard exactly once.
func TestRunBackfill_RetriesWhenMachineCredentialUnavailable(t *testing.T) {
	delays := stubRetrySleep(t)
	dir := t.TempDir()
	for name, body := range map[string]string{
		"cost.json":  `{"uid":"cost","title":"Cost","version":1}`,
		"embed.json": `{"uid":"embed","title":"Embed","version":1}`,
	} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	// Three denied calls span more than one pass: pass 1 fails both
	// dashboards (2 FindByUID), pass 2 fails the first and imports the second,
	// pass 3 imports the first and refreshes the second.
	daemon := &flakyDaemon{fakeCmdDaemon: newFakeCmdDaemon(), failures: 3}
	store := bridge.NewDashboardStore(daemon, "test")
	var logBuf bytes.Buffer
	runBackfill(context.Background(), store, dir, "gardenio", slog.New(slog.NewTextHandler(&logBuf, nil)))

	if len(*delays) != 2 {
		t.Errorf("expected two backoff sleeps, got %v", *delays)
	}
	list, _ := daemon.fakeCmdDaemon.ListBeadsFiltered(context.Background(), beadsapi.ListBeadsQuery{})
	uids := map[string]int{}
	for _, b := range list.Beads {
		if b.Type == "dashboard" {
			uids[b.Fields["dashboard_uid"]]++
		}
	}
	if uids["cost"] != 1 || uids["embed"] != 1 || len(uids) != 2 {
		t.Fatalf("expected each dashboard imported exactly once, got %v", uids)
	}
	if !strings.Contains(logBuf.String(), `"backfill complete" created=1 refreshed=1 failed=0`) &&
		!strings.Contains(logBuf.String(), `msg="backfill complete" created=1 refreshed=1 failed=0`) {
		t.Errorf("final pass must report created=1 refreshed=1 failed=0:\n%s", logBuf.String())
	}
}

// fakeWarmer is a TokenSource whose Warm records when it ran, standing in for
// beadsapi's machine token source (whose Warm is tested in beadsapi).
type fakeWarmer struct {
	events *[]string
	err    error
}

func (w *fakeWarmer) Token(context.Context) (string, error) { return "tok", nil }

func (w *fakeWarmer) Warm(context.Context) error {
	*w.events = append(*w.events, "warm")
	return w.err
}

// orderedSetter records the config-seed calls so the test can prove the
// machine token was awaited BEFORE the first seed write.
type orderedSetter struct {
	*fakeCmdDaemon
	events *[]string
}

func (o *orderedSetter) ListBeadsFiltered(ctx context.Context, q beadsapi.ListBeadsQuery) (*beadsapi.ListBeadsResult, error) {
	*o.events = append(*o.events, "seed")
	return o.fakeCmdDaemon.ListBeadsFiltered(ctx, q)
}

func TestSeedStartup_WaitsForMachineTokenBeforeSeeding(t *testing.T) {
	var events []string
	ts := &fakeWarmer{events: &events}
	setter := &orderedSetter{fakeCmdDaemon: newFakeCmdDaemon(), events: &events}
	seed := seedStartup(ts, setter)
	if err := seed(context.Background(), slog.New(slog.NewTextHandler(&bytes.Buffer{}, nil))); err != nil {
		t.Fatalf("seed: %v", err)
	}
	if len(events) < 2 || events[0] != "warm" {
		t.Fatalf("the token must be warmed before any seed write, got %v", events)
	}
	for _, e := range events[1:] {
		if e != "seed" {
			t.Fatalf("unexpected event after warm: %v", events)
		}
	}
	// The dashboard type config bead now exists.
	list, _ := setter.fakeCmdDaemon.ListBeadsFiltered(context.Background(), beadsapi.ListBeadsQuery{})
	found := false
	for _, b := range list.Beads {
		if b.Type == "config" && b.Title == "type:dashboard" {
			found = true
		}
	}
	if !found {
		t.Errorf("expected the dashboard type config bead to be seeded")
	}
}

func TestSeedStartup_NoMachinePrincipalSeedsDirectly(t *testing.T) {
	var events []string
	setter := &orderedSetter{fakeCmdDaemon: newFakeCmdDaemon(), events: &events}
	if err := seedStartup(nil, setter)(context.Background(), slog.New(slog.NewTextHandler(&bytes.Buffer{}, nil))); err != nil {
		t.Fatalf("seed: %v", err)
	}
	if len(events) == 0 || events[0] != "seed" {
		t.Fatalf("without a machine principal the seed runs immediately, got %v", events)
	}
}

// A shutdown signal while still waiting for the token is a clean exit, not a
// run() failure: the pod must not be reported as crashed.
func TestRun_ShutdownWhileWaitingForTokenIsClean(t *testing.T) {
	cfg := &config{
		beadsHTTPAddr: "http://localhost:0",
		namespace:     "gardenio",
		listenAddr:    "127.0.0.1:0",
		statePath:     filepath.Join(t.TempDir(), "state.json"),
	}
	ctx, cancel := context.WithCancel(context.Background())
	seed := func(ctx context.Context, _ *slog.Logger) error {
		cancel()
		return fmt.Errorf("%w: machine token warm-up abandoned: %w", beadsapi.ErrMachineAuthUnavailable, ctx.Err())
	}
	store := bridge.NewDashboardStore(newFakeCmdDaemon(), "test")
	if err := run(ctx, cfg, slog.New(slog.NewTextHandler(&bytes.Buffer{}, nil)), store, &cmdFakeWriter{}, seed); err != nil {
		t.Fatalf("shutdown during the seed must return nil, got %v", err)
	}
}
