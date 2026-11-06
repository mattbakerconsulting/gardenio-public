// Command grafana-bridge-probe is a TEST-ONLY driver that exercises the gardenio
// grafana-bridge dashboard-sync + read-back conflict-policy stack end-to-end,
// hermetically, so the e2e suite can assert the beads→Grafana ConfigMap write
// path, the one-time backfill, the SSE-driven sync, and all THREE
// ConflictPolicy branches (beads-wins / grafana-wins / manual) WITHOUT a live
// grafana-bridge, a live Grafana, a live k8s API, or real agent pods.
//
// ── Why a probe, not a live-bridge module ─────────────────────────────────
//
// The grafana-bridge read-back poll is DISABLED on the deployed fleet: on
// the e2e namespace (and prod) grafanaBridge.grafana.url is empty, so the bridge runs
// WRITE-ONLY (beads→ConfigMap only) — there is no live Grafana→beads reconcile
// loop to observe, and the conflict-policy branches never fire in-cluster. The
// existing test-grafana-iframe.sh only asserts X-Frame-Options embedding, and
// test-charts-hub.sh only asserts the viewer's read proxies. Nothing exercised
// the bridge's actual sync + conflict-resolution logic against a real cluster.
//
// That logic — GrafanaSync (beads→ConfigMap), BackfillDashboards, and
// GrafanaReadback's three ConflictPolicy branches — is all in-process Go with
// no product image. So this probe follows the EXACT precedent of
// cmd/gitlab-prov-probe / cmd/jira-conv-probe / cmd/trigger-probe (the blessed
// "real bridge components + stub external system + mock daemon, no product
// image" pattern): it wires the REAL bridge code —
//
//   - the REAL GrafanaConfigMapWriter against a fake k8s clientset (so a
//     provisioned dashboard is observed as a real grafana_dashboard="1"
//     ConfigMap, exactly the sidecar contract);
//   - the REAL GrafanaSync driven both directly (backfill/catch-up) and through
//     the REAL SSEStream (a stub SSE server emits a dashboard bead.updated frame,
//     the faithful `gb grafana put` → SSE → sync path);
//   - the REAL GrafanaReadback against a STUB Grafana httptest server serving the
//     /api/search + /api/dashboards/uid/<uid> endpoints the read client calls;
//   - an in-process mock daemon (mockdaemon.go) that RECORDS CreateBead /
//     UpdateBeadFields / AddDependency / AddComment — so every bead write is an
//     observed recorded call and NO real bead (hence NO pod) is ever created.
//
// Each leg prints a single line "LEG <name>: PASS|FAIL <detail>" and the probe
// exits non-zero if any leg fails, so the wrapping shell module (the
// `grafana-bridge` e2e module) surfaces a per-leg breakdown and fails the shard
// on any leg. It is NOT part of any release Makefile/Dockerfile target — it is
// built from source on demand inside the e2e shard, exactly like
// gitlab-prov-probe / jira-conv-probe, and never ships.
//
// Legs (task ka-0000002319; coverage-backfill epic ka-0000000958):
//
//	1  write-sync    A dashboard bead + model doc, driven through the REAL SSE
//	                 stream, provisions a grafana_dashboard="1" ConfigMap carrying
//	                 the model and stamps dashboard_hash/source=beads; a second
//	                 unchanged sync is a no-op (loop-breaker).
//	2  backfill      BackfillDashboards imports N JSON models as type=dashboard
//	                 beads (idempotent on uid), each with an attached model doc,
//	                 then a catch-up sync provisions each into Grafana.
//	3  grafana-wins  A Grafana-side edit + a pending beads-side edit (true
//	                 conflict) resolves grafana-wins: the bead model becomes the
//	                 Grafana model, the dropped beads model is archived, source=grafana.
//	4  beads-wins    The same conflict under beads-wins keeps the beads model and
//	                 archives the divergent Grafana model.
//	5  manual        The same conflict under manual opens a type=decision bead
//	                 (blocks-linked to the dashboard) and touches NEITHER live model.
//	6  loop-breaker  A read-back poll where Grafana's model matches the bead hash
//	                 (only a volatile version bump) is a no-op — no phantom edit.
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"time"

	"gardenio/controller/internal/beadsapi"
	"gardenio/controller/internal/bridge"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"
)

const probeNamespace = "gardenio-system"

func main() {
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelWarn}))

	legs := []struct {
		name string
		run  func(*slog.Logger) legResult
	}{
		{"write-sync", runWriteSyncLeg},
		{"backfill", runBackfillLeg},
		{"grafana-wins", runGrafanaWinsLeg},
		{"beads-wins", runBeadsWinsLeg},
		{"manual", runManualLeg},
		{"loop-breaker", runLoopBreakerLeg},
	}

	failures := 0
	for _, leg := range legs {
		res := leg.run(logger)
		status := "PASS"
		if !res.ok {
			status = "FAIL"
			failures++
		}
		fmt.Printf("LEG %s: %s %s\n", leg.name, status, res.detail)
	}

	if failures > 0 {
		fmt.Printf("grafana-bridge-probe: %d/%d legs FAILED\n", failures, len(legs))
		os.Exit(1)
	}
	fmt.Printf("grafana-bridge-probe: all %d legs passed\n", len(legs))
}

// legResult is the outcome of one bridge leg.
type legResult struct {
	ok     bool
	detail string
}

func pass(format string, args ...any) legResult {
	return legResult{ok: true, detail: fmt.Sprintf(format, args...)}
}

func fail(format string, args ...any) legResult {
	return legResult{ok: false, detail: fmt.Sprintf(format, args...)}
}

// ─────────────────────────────────────────────────────────────────────────────
// Leg 1: write-sync — a dashboard bead → grafana_dashboard="1" ConfigMap via the
// REAL SSE stream, then a loop-breaker no-op on the unchanged re-sync.
//
// This is the beads→Grafana write path production runs write-only. We seed a
// type=dashboard bead + its model doc, then drive the REAL SSEStream (fed by a
// stub SSE server emitting one beads.bead.updated frame — the exact effect of
// `gb grafana put`) so GrafanaSync.handleEvent fires through the real dispatch.
// The load-bearing property: a real grafana_dashboard="1" ConfigMap (the sidecar
// discovery contract) appears in the fake k8s API carrying the model, and the
// bead is stamped dashboard_hash + source=beads. A second SyncDashboard on the
// unchanged model must NOT write again (the hash loop-breaker).
// ─────────────────────────────────────────────────────────────────────────────

func runWriteSyncLeg(logger *slog.Logger) legResult {
	ctx := context.Background()
	daemon := newMockDaemon()
	store := bridge.NewDashboardStore(daemon, "grafana-bridge-probe")
	client := fake.NewClientset()
	writer := bridge.NewGrafanaConfigMapWriter(client, probeNamespace, logger)
	sync := bridge.NewGrafanaSync(bridge.GrafanaSyncConfig{Store: store, Writer: writer, Logger: logger, Projects: []string{"gardenio"}})

	uid := "probe-write-sync"
	model := []byte(`{"uid":"probe-write-sync","title":"Write Sync","version":1,"panels":[{"id":1}]}`)
	dashID, err := daemon.CreateBead(ctx, beadsapi.CreateBeadRequest{
		Type:   "dashboard",
		Title:  "Write Sync",
		Labels: []string{"project:gardenio"},
		Fields: json.RawMessage(`{"dashboard_uid":"` + uid + `"}`),
	})
	if err != nil {
		return fail("create dashboard bead: %v", err)
	}
	if _, err := store.PutModel(ctx, dashID, model); err != nil {
		return fail("put model doc: %v", err)
	}

	// Drive the REAL SSE dispatch path: register the sync on a real SSEStream fed
	// by a stub SSE server emitting one dashboard bead.updated frame.
	sse := newStubSSE(beadUpdatedFrame("1", dashID, "dashboard"))
	defer sse.Close()
	stream := bridge.NewSSEStream(bridge.SSEStreamConfig{
		BeadsHTTPAddr: sse.URL, Topics: []string{"beads.bead.updated"}, Logger: logger,
	})
	sync.RegisterHandlers(stream)

	ctx2, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	go func() { _ = stream.Start(ctx2) }()

	// Wait for the bridge to stamp the hash (proof the SSE→sync path fired).
	if !waitFor(3*time.Second, func() bool { return daemon.beadField(dashID, "dashboard_hash") != "" }) {
		return fail("SSE-driven sync never stamped dashboard_hash within 3s")
	}
	cancel()

	// The ConfigMap must exist with the sidecar discovery label + the model data.
	cmName := "grafana-dashboard-" + uid
	cm, err := client.CoreV1().ConfigMaps(probeNamespace).Get(ctx, cmName, metav1.GetOptions{})
	if err != nil {
		return fail("expected grafana_dashboard ConfigMap %q after sync: %v", cmName, err)
	}
	if cm.Labels["grafana_dashboard"] != "1" {
		return fail("ConfigMap missing sidecar discovery label grafana_dashboard=1: %v", cm.Labels)
	}
	if cm.Data[uid+".json"] != string(model) {
		return fail("ConfigMap data does not carry the dashboard model: %v", cm.Data)
	}

	// Provenance: source=beads + a hash matching the normalized model.
	if got := daemon.beadField(dashID, "dashboard_source"); got != "beads" {
		return fail("expected dashboard_source=beads after write, got %q", got)
	}
	wantHash, _ := bridge.HashDashboardModel(model)
	if got := daemon.beadField(dashID, "dashboard_hash"); got != wantHash {
		return fail("stamped hash %q != normalized model hash %q", got, wantHash)
	}

	// Loop-breaker: an unchanged re-sync must NOT rewrite (same CM resourceVersion).
	before := cm.ResourceVersion
	sync.SyncDashboard(ctx, dashID)
	cm2, err := client.CoreV1().ConfigMaps(probeNamespace).Get(ctx, cmName, metav1.GetOptions{})
	if err != nil {
		return fail("re-get ConfigMap: %v", err)
	}
	if cm2.ResourceVersion != before {
		return fail("loop-breaker regressed: unchanged re-sync rewrote the ConfigMap (rv %q → %q)", before, cm2.ResourceVersion)
	}
	return pass("SSE→sync provisioned grafana_dashboard=1 CM %q (model + source=beads + hash), unchanged re-sync no-op", cmName)
}

// ─────────────────────────────────────────────────────────────────────────────
// Leg 2: backfill — BackfillDashboards imports N shipped JSON models as
// type=dashboard beads (idempotent on uid) with attached model docs, then a
// catch-up sync provisions each into Grafana.
// ─────────────────────────────────────────────────────────────────────────────

func runBackfillLeg(logger *slog.Logger) legResult {
	ctx := context.Background()
	daemon := newMockDaemon()
	store := bridge.NewDashboardStore(daemon, "grafana-bridge-probe")
	client := fake.NewClientset()
	writer := bridge.NewGrafanaConfigMapWriter(client, probeNamespace, logger)
	sync := bridge.NewGrafanaSync(bridge.GrafanaSyncConfig{Store: store, Writer: writer, Logger: logger, Projects: []string{"gardenio"}})

	dashboards := []bridge.BackfillDashboard{
		{Name: "cost-overview", Model: []byte(`{"uid":"probe-cost","title":"Cost","version":3,"panels":[]}`)},
		{Name: "embedding", Model: []byte(`{"uid":"probe-embed","title":"Embedding","version":1,"panels":[]}`)},
	}
	results := bridge.BackfillDashboards(ctx, store, dashboards, "gardenio", logger)
	created := 0
	for _, r := range results {
		if r.Err != nil {
			return fail("backfill %q errored: %v", r.Name, r.Err)
		}
		if r.Created {
			created++
		}
		if r.BeadID == "" || r.ModelDoc == "" {
			return fail("backfill %q left an empty bead/model-doc id (%q/%q)", r.Name, r.BeadID, r.ModelDoc)
		}
	}
	if created != len(dashboards) {
		return fail("expected %d dashboards created on first backfill, got %d", len(dashboards), created)
	}
	if got := len(daemon.createdOfType("dashboard")); got != len(dashboards) {
		return fail("expected %d type=dashboard beads created, got %d", len(dashboards), got)
	}

	// Idempotency: a second backfill of the same uids creates NO new dashboard
	// bead (found-by-uid), only refreshes the model doc.
	results2 := bridge.BackfillDashboards(ctx, store, dashboards, "gardenio", logger)
	for _, r := range results2 {
		if r.Created {
			return fail("second backfill of %q was not idempotent (Created=true)", r.Name)
		}
	}
	if got := len(daemon.createdOfType("dashboard")); got != len(dashboards) {
		return fail("idempotent backfill minted extra dashboard beads (now %d)", got)
	}

	// Catch-up provisioning: sync each backfilled bead → one ConfigMap per uid.
	ids, err := store.ListActiveDashboardIDs(ctx, 256)
	if err != nil {
		return fail("list active dashboards: %v", err)
	}
	if len(ids) != len(dashboards) {
		return fail("expected %d active dashboard beads, got %d", len(dashboards), len(ids))
	}
	for _, id := range ids {
		sync.SyncDashboard(ctx, id)
	}
	list, err := client.CoreV1().ConfigMaps(probeNamespace).List(ctx, metav1.ListOptions{
		LabelSelector: "grafana_dashboard=1",
	})
	if err != nil {
		return fail("list grafana ConfigMaps: %v", err)
	}
	if len(list.Items) != len(dashboards) {
		return fail("expected %d provisioned ConfigMaps after catch-up, got %d", len(dashboards), len(list.Items))
	}
	return pass("backfilled %d dashboards (idempotent on re-run), catch-up provisioned %d grafana_dashboard=1 ConfigMaps", len(dashboards), len(list.Items))
}

// ─────────────────────────────────────────────────────────────────────────────
// Legs 3–5: conflict policy. Each seeds a TRUE conflict (both the bead model doc
// AND the Grafana model diverged from the last agreed hash) and runs one REAL
// GrafanaReadback.PollOnce against a STUB Grafana, asserting the policy branch.
// ─────────────────────────────────────────────────────────────────────────────

// conflictRig builds a dashboard bead whose model doc + stamped hash agree
// (source=beads), then mutates the bead model doc WITHOUT restamping the hash (a
// pending beads-side edit) and points a stub Grafana at a DIFFERENT model for the
// same uid (a Grafana-side edit). PollOnce then sees both sides diverged → a true
// conflict the policy resolves. Returns the dashboard bead id, the stub, and the
// grafana model bytes.
func conflictRig(ctx context.Context, daemon *mockDaemon, store *bridge.DashboardStore, uid string) (string, *stubGrafana, []byte, error) {
	base := []byte(`{"uid":"` + uid + `","title":"base","version":1,"panels":[]}`)
	dashID, err := daemon.CreateBead(ctx, beadsapi.CreateBeadRequest{
		Type:   "dashboard",
		Title:  uid,
		Fields: json.RawMessage(`{"dashboard_uid":"` + uid + `","dashboard_source":"beads"}`),
	})
	if err != nil {
		return "", nil, nil, fmt.Errorf("create bead: %w", err)
	}
	if _, err := store.PutModel(ctx, dashID, base); err != nil {
		return "", nil, nil, fmt.Errorf("put base model: %w", err)
	}
	hash, _ := bridge.HashDashboardModel(base)
	if err := daemon.UpdateBeadFields(ctx, dashID, map[string]string{"dashboard_hash": hash}); err != nil {
		return "", nil, nil, fmt.Errorf("stamp hash: %w", err)
	}
	// Pending beads-side edit (model doc changes, hash NOT restamped).
	beadEdit := []byte(`{"uid":"` + uid + `","title":"beads-edit","version":1,"panels":[{"id":7}]}`)
	if _, err := store.PutModel(ctx, dashID, beadEdit); err != nil {
		return "", nil, nil, fmt.Errorf("bead edit: %w", err)
	}
	// Divergent Grafana-side edit.
	grafanaModel := []byte(`{"uid":"` + uid + `","title":"grafana-edit","version":9,"panels":[{"id":42}]}`)
	stub := newStubGrafana()
	stub.set(uid, grafanaModel, 9)
	return dashID, stub, grafanaModel, nil
}

func runGrafanaWinsLeg(logger *slog.Logger) legResult {
	ctx := context.Background()
	daemon := newMockDaemon()
	store := bridge.NewDashboardStore(daemon, "grafana-bridge-probe")
	uid := "probe-conflict-gw"
	dashID, stub, grafanaModel, err := conflictRig(ctx, daemon, store, uid)
	if err != nil {
		return fail("rig: %v", err)
	}
	defer stub.Close()

	rb := bridge.NewGrafanaReadback(bridge.GrafanaReadbackConfig{
		Store:   store,
		Grafana: bridge.NewGrafanaClient(bridge.GrafanaClientConfig{BaseURL: stub.URL(), Logger: logger}),
		Policy:  bridge.ConflictGrafanaWins,
		Logger:  logger,
	})
	rb.PollOnce(ctx)

	if got := daemon.beadField(dashID, "dashboard_source"); got != "grafana" {
		return fail("grafana-wins: expected source=grafana, got %q", got)
	}
	_, gotModel, _ := store.ModelDoc(ctx, dashID)
	meta, _ := bridge.ParseDashboardMeta(gotModel)
	if meta.Title != "grafana-edit" {
		return fail("grafana-wins: bead model should be the grafana model, title=%q", meta.Title)
	}
	wantHash, _ := bridge.HashDashboardModel(grafanaModel)
	if got := daemon.beadField(dashID, "dashboard_hash"); got != wantHash {
		return fail("grafana-wins: hash not updated to grafana model")
	}
	if !daemon.hasConflictArchive() {
		return fail("grafana-wins: expected the dropped beads model archived as a conflict doc")
	}
	if !anyCommentContains(daemon.commentsFor(dashID), "grafana-wins") {
		return fail("grafana-wins: expected a provenance comment mentioning grafana-wins")
	}
	return pass("true conflict resolved grafana-wins: pulled grafana model, archived beads model, source=grafana")
}

func runBeadsWinsLeg(logger *slog.Logger) legResult {
	ctx := context.Background()
	daemon := newMockDaemon()
	store := bridge.NewDashboardStore(daemon, "grafana-bridge-probe")
	uid := "probe-conflict-bw"
	dashID, stub, _, err := conflictRig(ctx, daemon, store, uid)
	if err != nil {
		return fail("rig: %v", err)
	}
	defer stub.Close()

	rb := bridge.NewGrafanaReadback(bridge.GrafanaReadbackConfig{
		Store:   store,
		Grafana: bridge.NewGrafanaClient(bridge.GrafanaClientConfig{BaseURL: stub.URL(), Logger: logger}),
		Policy:  bridge.ConflictBeadsWins,
		Logger:  logger,
	})
	rb.PollOnce(ctx)

	// beads-wins: the bead model doc is UNTOUCHED (still the beads edit).
	_, gotModel, _ := store.ModelDoc(ctx, dashID)
	meta, _ := bridge.ParseDashboardMeta(gotModel)
	if meta.Title != "beads-edit" {
		return fail("beads-wins: bead model should remain the beads edit, title=%q", meta.Title)
	}
	if !daemon.hasConflictArchive() {
		return fail("beads-wins: expected the divergent Grafana model archived as a conflict doc")
	}
	if !anyCommentContains(daemon.commentsFor(dashID), "beads-wins") {
		return fail("beads-wins: expected a provenance comment mentioning beads-wins")
	}
	return pass("true conflict resolved beads-wins: kept beads model, archived grafana model")
}

func runManualLeg(logger *slog.Logger) legResult {
	ctx := context.Background()
	daemon := newMockDaemon()
	store := bridge.NewDashboardStore(daemon, "grafana-bridge-probe")
	uid := "probe-conflict-manual"
	dashID, stub, _, err := conflictRig(ctx, daemon, store, uid)
	if err != nil {
		return fail("rig: %v", err)
	}
	defer stub.Close()

	rb := bridge.NewGrafanaReadback(bridge.GrafanaReadbackConfig{
		Store:   store,
		Grafana: bridge.NewGrafanaClient(bridge.GrafanaClientConfig{BaseURL: stub.URL(), Logger: logger}),
		Policy:  bridge.ConflictManual,
		Logger:  logger,
	})
	rb.PollOnce(ctx)

	// manual: neither live model changed — the bead model doc is still the beads edit.
	_, gotModel, _ := store.ModelDoc(ctx, dashID)
	meta, _ := bridge.ParseDashboardMeta(gotModel)
	if meta.Title != "beads-edit" {
		return fail("manual: bead model must be untouched, title=%q", meta.Title)
	}
	if got := daemon.beadField(dashID, "dashboard_source"); got == "grafana" {
		return fail("manual: source must NOT flip to grafana (both sides left untouched)")
	}
	// A type=decision bead must have been opened.
	decisions := daemon.createdOfType("decision")
	if len(decisions) != 1 {
		return fail("manual: expected exactly 1 decision bead opened, got %d", len(decisions))
	}
	// …and it must be blocks-linked: the bridge adds AddDependency(dashboard,
	// decision, "blocks"), i.e. the dashboard depends-on the decision (the
	// decision blocks the dashboard). That edge is indexed by its depends-on
	// target (the decision), so the row lives under depsInto(decision).
	linked := false
	for _, d := range daemon.depsInto(decisions[0].ID) {
		if d.Type == "blocks" && d.BeadID == dashID {
			linked = true
		}
	}
	if !linked {
		return fail("manual: decision bead %q not blocks-linked to dashboard %q", decisions[0].ID, dashID)
	}
	if !anyCommentContains(daemon.commentsFor(dashID), "manual") {
		return fail("manual: expected a provenance comment mentioning policy=manual")
	}
	return pass("true conflict under manual opened decision %s (blocks-linked), touched neither live model", decisions[0].ID)
}

// ─────────────────────────────────────────────────────────────────────────────
// Leg 6: loop-breaker — the read-back no-op that stops a beads→Grafana write from
// echoing back as a phantom Grafana "edit". The bead is fully in sync; Grafana
// holds the same model up to a volatile version bump (which NormalizeDashboardJSON
// strips), so the hashes match and PollOnce leaves the bead untouched.
// ─────────────────────────────────────────────────────────────────────────────

func runLoopBreakerLeg(logger *slog.Logger) legResult {
	ctx := context.Background()
	daemon := newMockDaemon()
	store := bridge.NewDashboardStore(daemon, "grafana-bridge-probe")
	uid := "probe-loopbreak"
	model := []byte(`{"uid":"` + uid + `","title":"Loop","version":1,"panels":[{"id":1}]}`)
	dashID, err := daemon.CreateBead(ctx, beadsapi.CreateBeadRequest{
		Type:   "dashboard",
		Title:  "Loop",
		Fields: json.RawMessage(`{"dashboard_uid":"` + uid + `","dashboard_source":"beads"}`),
	})
	if err != nil {
		return fail("create bead: %v", err)
	}
	if _, err := store.PutModel(ctx, dashID, model); err != nil {
		return fail("put model: %v", err)
	}
	hash, _ := bridge.HashDashboardModel(model)
	if err := daemon.UpdateBeadFields(ctx, dashID, map[string]string{"dashboard_hash": hash}); err != nil {
		return fail("stamp hash: %v", err)
	}
	// Grafana holds the SAME model, only a version bump (volatile, stripped before hashing).
	stub := newStubGrafana()
	stub.set(uid, []byte(`{"uid":"`+uid+`","title":"Loop","version":7,"panels":[{"id":1}]}`), 7)
	defer stub.Close()

	rb := bridge.NewGrafanaReadback(bridge.GrafanaReadbackConfig{
		Store:   store,
		Grafana: bridge.NewGrafanaClient(bridge.GrafanaClientConfig{BaseURL: stub.URL(), Logger: logger}),
		Logger:  logger,
	})
	rb.PollOnce(ctx)

	if got := daemon.beadField(dashID, "dashboard_source"); got != "beads" {
		return fail("loop-breaker: source flipped to %q — a volatile version bump was misread as a Grafana edit", got)
	}
	if got := daemon.beadField(dashID, "dashboard_hash"); got != hash {
		return fail("loop-breaker: hash changed (%q → %q) on a no-op poll", hash, got)
	}
	if daemon.hasConflictArchive() || len(daemon.createdOfType("decision")) != 0 {
		return fail("loop-breaker: a no-op poll spuriously archived/opened a decision")
	}
	return pass("in-sync poll (Grafana version bump only) is a no-op: source=beads, hash unchanged, no phantom edit")
}

// ─────────────────────────────────────────────────────────────────────────────
// Stub Grafana: an httptest server implementing the two read endpoints the REAL
// GrafanaClient calls — GET /api/search?type=dash-db and
// GET /api/dashboards/uid/<uid> (returning the {meta,dashboard} envelope). uid →
// (model, version). A missing uid 404s so the read client's not-found path fires.
// ─────────────────────────────────────────────────────────────────────────────

type stubGrafana struct {
	srv      *httptest.Server
	models   map[string][]byte
	versions map[string]int
}

func newStubGrafana() *stubGrafana {
	s := &stubGrafana{models: map[string][]byte{}, versions: map[string]int{}}
	mux := http.NewServeMux()
	mux.HandleFunc("/api/search", func(w http.ResponseWriter, _ *http.Request) {
		var refs []map[string]any
		for uid := range s.models {
			refs = append(refs, map[string]any{"uid": uid, "type": "dash-db"})
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(refs)
	})
	mux.HandleFunc("/api/dashboards/uid/", func(w http.ResponseWriter, r *http.Request) {
		uid := strings.TrimPrefix(r.URL.Path, "/api/dashboards/uid/")
		model, ok := s.models[uid]
		if !ok {
			http.NotFound(w, r)
			return
		}
		env := map[string]any{
			"meta":      map[string]any{"version": s.versions[uid]},
			"dashboard": json.RawMessage(model),
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(env)
	})
	s.srv = httptest.NewServer(mux)
	return s
}

func (s *stubGrafana) set(uid string, model []byte, version int) {
	s.models[uid] = model
	s.versions[uid] = version
}

func (s *stubGrafana) URL() string { return s.srv.URL }
func (s *stubGrafana) Close()      { s.srv.Close() }

// ─────────────────────────────────────────────────────────────────────────────
// Stub SSE server: emits the given event frames once, then holds the connection
// open so the SSEStream does not reconnect-spam (mirrors cmd/gitlab-prov-probe).
// ─────────────────────────────────────────────────────────────────────────────

type stubSSE struct {
	*httptest.Server
}

func newStubSSE(frames ...string) *stubSSE {
	s := &stubSSE{}
	s.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		flusher, ok := w.(http.Flusher)
		if !ok {
			http.Error(w, "no flush", http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		flusher.Flush()
		for _, f := range frames {
			fmt.Fprint(w, f)
			flusher.Flush()
		}
		time.Sleep(3 * time.Second)
	}))
	return s
}

// beadUpdatedFrame builds one SSE frame for a beads.bead.updated event carrying a
// dashboard bead — the exact effect of `gb grafana put` firing a bead write.
func beadUpdatedFrame(id, beadID, btype string) string {
	inner, _ := json.Marshal(map[string]any{
		"id":     beadID,
		"type":   btype,
		"status": "open",
	})
	data, _ := json.Marshal(map[string]json.RawMessage{"bead": inner})
	return fmt.Sprintf("id:%s\nevent:beads.bead.updated\ndata:%s\n\n", id, data)
}

// ─────────────────────────────────────────────────────────────────────────────
// small helpers
// ─────────────────────────────────────────────────────────────────────────────

// waitFor polls cond every 10ms until true or the deadline elapses.
func waitFor(timeout time.Duration, cond func() bool) bool {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if cond() {
			return true
		}
		time.Sleep(10 * time.Millisecond)
	}
	return cond()
}

// anyCommentContains reports whether any recorded comment text contains sub.
func anyCommentContains(comments []commentRecord, sub string) bool {
	for _, c := range comments {
		if strings.Contains(c.Text, sub) {
			return true
		}
	}
	return false
}
