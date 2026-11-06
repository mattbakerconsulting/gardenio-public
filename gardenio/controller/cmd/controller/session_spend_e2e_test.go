package main

// session_spend_e2e_test.go — end-to-end integration of the session spend
// attribution pipeline, exercised as ONE flow the way the controller runs it
// in production, over REAL HTTP surfaces the unit tests never touch.
//
// WHY THIS FILE EXISTS (the gap it closes — task gd-NU1IbgLk5v):
//   The per-session spend attribution stack shipped end-to-end (F2/E3, close
//   reconciliation, coop cost-free usage) with strong UNIT coverage in
//   reconcile_session_spend_test.go and internal/metrics/spend_test.go, but
//   NO test drives the pieces together as production wires them:
//
//     - The unit tests read gardenio_agent_spend_usd_total via in-process
//       prometheus testutil.Gather(); NONE scrape the counter over a REAL
//       promhttp /metrics HTTP endpoint (the actual controller :9092 surface).
//     - The unit tests call captureBaseline (spawn) and reconcileAtClose
//       (close) against a daemon stub that RECORDS writes but never APPLIES
//       them, so no test proves the round-trip: the baseline the controller
//       writes onto the agent bead at spawn is the one it reads BACK at close
//       to compute the settlement delta. That daemon-persisted baseline is the
//       load-bearing invariant the whole "one increment per session" property
//       rests on, and it survives a controller restart between spawn and close
//       precisely because it lives on the bead — a property only a read-write
//       daemon can exercise.
//     - The task bead's "Proposed e2e" describes a NATS->counter forwarder that
//       was DELETED in 9add5d50c (ka-0000000717): coop never emits response_cost,
//       so the per-turn forwarder dropped 100% of events and never advanced the
//       counter. The counter is now driven SOLELY by reconcileAtClose at session
//       close. This file asserts the CURRENT topology, not the removed one.
//
// The daemon here is a READ-WRITE stub (rwDaemon): a PATCH mutates the stored
// bead so a later GET reflects it — the faithful production behavior. The
// metrics are served through the SAME promhttp.HandlerFor(...) the controller
// mounts on :9092 (main.go), scraped over a real httptest server, and parsed
// out of the Prometheus text exposition — so a regression in the served
// exposition (not just the in-process counter) is caught. Spawn and close share
// ONE SpendMetrics/registry, exactly as the controller wires them, so the
// born-at-0 series from spawn is the same series the close increment advances.
//
// The three legs mirror the task's three halves:
//   Leg A  spend-counter served-scrape: spawn (captureBaseline born-at-0) ->
//          close (reconcileAtClose) -> GET /metrics asserts
//          gardenio_agent_spend_usd_total{agent,project,role,model,account}
//          advanced by exactly the settled delta, and a nil/zero-cost close
//          does NOT advance it.
//   Leg B  close reconciliation round-trip: the baseline the controller WROTE
//          at spawn is read back at close from the daemon (not passed in), and
//          total_cost_usd / litellm_spend_close / litellm_spend_first_baseline
//          land on the agent bead from the live /spend/tags delta — proving the
//          CONTROLLER writes the authoritative cost, not a hand-written kd field.
//   Leg C  usage NATS model contract lives in internal/bridge (the consumer):
//          see hookstream_usage_contract_test.go.

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"

	"gardenio/controller/internal/beadsapi"
	gardeniometrics "gardenio/controller/internal/metrics"
)

// quietLogger discards log output so the spend-e2e flow (which logs at
// Info/Debug on every settle) does not spam the test runner.
func quietLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// rwDaemon is a READ-WRITE beads daemon stub: unlike the record-only daemonStub
// in spend_sync_test.go, a PATCH here MERGES the incoming fields into the stored
// bead so a subsequent GET reflects them. This is what lets the full spawn->close
// flow round-trip the baseline the controller writes at spawn through the daemon
// and back at close — the exact persistence path that survives a controller
// restart in production.
type rwDaemon struct {
	mu    sync.Mutex
	beads map[string]map[string]any // beadID -> bead object (with "fields" map[string]any)
}

func newRWDaemon() *rwDaemon {
	return &rwDaemon{beads: map[string]map[string]any{}}
}

// seedAgent registers an open type=agent bead with the given field map.
func (d *rwDaemon) seedAgent(id string, fields map[string]any) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if fields == nil {
		fields = map[string]any{}
	}
	// Copy so later mutations don't alias the caller's map.
	fc := make(map[string]any, len(fields))
	for k, v := range fields {
		fc[k] = v
	}
	d.beads[id] = map[string]any{
		"id":     id,
		"title":  id,
		"type":   "agent",
		"status": "open",
		"fields": fc,
	}
}

// fieldString returns a stored field rendered the way the daemon would return it
// over the wire (numbers stringified), so tests read it exactly as the reconciler
// would after a GET round-trip. Empty string when absent.
func (d *rwDaemon) fieldString(id, key string) string {
	d.mu.Lock()
	defer d.mu.Unlock()
	b, ok := d.beads[id]
	if !ok {
		return ""
	}
	fields, _ := b["fields"].(map[string]any)
	v, ok := fields[key]
	if !ok {
		return ""
	}
	switch v := v.(type) {
	case string:
		return v
	default:
		bs, err := json.Marshal(v)
		if err != nil {
			return ""
		}
		return string(bs)
	}
}

func (d *rwDaemon) handler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		// GET /v1/beads/:id — GetBead + the read half of UpdateBeadFieldsTyped.
		case r.Method == http.MethodGet && strings.HasPrefix(r.URL.Path, "/v1/beads/") &&
			!strings.Contains(r.URL.Path[len("/v1/beads/"):], "/"):
			id := strings.TrimPrefix(r.URL.Path, "/v1/beads/")
			d.mu.Lock()
			b, ok := d.beads[id]
			var out map[string]any
			if ok {
				out = d.marshalBeadLocked(b)
			}
			d.mu.Unlock()
			if !ok {
				http.NotFound(w, r)
				return
			}
			_ = json.NewEncoder(w).Encode(out)

		// GET /v1/beads — ListBeadsFiltered (settleSessionBead). No session beads
		// are seeded here, so an empty list is the honest answer; the settlement's
		// session-bead write is best-effort and its absence never fails a leg.
		case r.Method == http.MethodGet && r.URL.Path == "/v1/beads":
			_ = json.NewEncoder(w).Encode(map[string]any{"beads": []any{}, "total": 0})

		// PATCH /v1/beads/:id — the write half of UpdateBeadFieldsTyped. MERGE the
		// incoming fields into the stored bead so a later GET reflects them.
		case r.Method == http.MethodPatch && strings.HasPrefix(r.URL.Path, "/v1/beads/"):
			id := strings.TrimPrefix(r.URL.Path, "/v1/beads/")
			body, _ := io.ReadAll(r.Body)
			var patch map[string]json.RawMessage
			_ = json.Unmarshal(body, &patch)
			var incoming map[string]any
			if raw, ok := patch["fields"]; ok {
				_ = json.Unmarshal(raw, &incoming)
			}
			d.mu.Lock()
			b, ok := d.beads[id]
			if ok {
				fields, _ := b["fields"].(map[string]any)
				if fields == nil {
					fields = map[string]any{}
				}
				for k, v := range incoming {
					fields[k] = v
				}
				b["fields"] = fields
			}
			d.mu.Unlock()
			if !ok {
				http.NotFound(w, r)
				return
			}
			w.WriteHeader(http.StatusOK)
			_ = json.NewEncoder(w).Encode(map[string]any{"ok": true})

		default:
			http.NotFound(w, r)
		}
	})
}

// marshalBeadLocked renders a stored bead as the daemon returns it on the wire:
// the "fields" map is emitted as a JSON object (numbers stay numbers), which
// beadsapi.ParseFieldsJSON stringifies on the client side exactly as it would
// for the real daemon. Caller holds d.mu.
func (d *rwDaemon) marshalBeadLocked(b map[string]any) map[string]any {
	fields, _ := b["fields"].(map[string]any)
	fieldsRaw, _ := json.Marshal(fields)
	out := map[string]any{}
	for k, v := range b {
		out[k] = v
	}
	out["fields"] = json.RawMessage(fieldsRaw)
	return out
}

// spendTagsHandler serves GET /spend/tags returning the supplied entries and
// requires the master key on the Authorization header, matching the live proxy
// contract the reconciler depends on.
func spendTagsHandler(entries []tagSpendEntry) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasPrefix(r.URL.Path, "/spend/tags") {
			http.NotFound(w, r)
			return
		}
		if got := r.Header.Get("Authorization"); got != "Bearer e2e-master-key" {
			http.Error(w, "missing master key", http.StatusForbidden)
			return
		}
		_ = json.NewEncoder(w).Encode(entries)
	})
}

// e2eReconciler builds a sessionSpendReconciler wired to the rwDaemon over a real
// httptest daemon server + a real httptest /spend/tags server serving `entries`,
// bound to the supplied SpendMetrics and a static label resolver (production
// resolves model/account from config caches; here we inject them directly).
//
// Spawn and close each get their own reconciler+servers (a fresh /spend/tags
// total per phase) but SHARE the passed-in SpendMetrics and the rwDaemon, exactly
// as the controller shares one metrics sink and one daemon across a pod's spawn
// and close.
func e2eReconciler(t *testing.T, daemon *rwDaemon, entries []tagSpendEntry, sm *gardeniometrics.SpendMetrics, model, account string) *sessionSpendReconciler {
	t.Helper()

	daemonSrv := httptest.NewServer(daemon.handler())
	t.Cleanup(daemonSrv.Close)
	litellmSrv := httptest.NewServer(spendTagsHandler(entries))
	t.Cleanup(litellmSrv.Close)

	client, err := beadsapi.New(beadsapi.Config{HTTPAddr: daemonSrv.URL})
	if err != nil {
		t.Fatalf("beadsapi.New: %v", err)
	}
	r := newSessionSpendReconciler(client, litellmSrv.URL, "e2e-master-key", quietLogger())
	r.metrics = sm
	r.resolveLabels = func(_, _ string) (string, string) { return model, account }
	return r
}

// newSpendMetricsReg constructs a SpendMetrics registered on a fresh registry,
// the pair the controller builds once and serves on :9092.
func newSpendMetricsReg(t *testing.T) (*gardeniometrics.SpendMetrics, *prometheus.Registry) {
	t.Helper()
	sm := gardeniometrics.NewSpendMetrics()
	reg := prometheus.NewRegistry()
	if err := sm.Register(reg); err != nil {
		t.Fatalf("register spend metrics: %v", err)
	}
	return sm, reg
}

// servedCounter scrapes GET /metrics from a promhttp server exposing reg (the
// exact promhttp.HandlerFor the controller mounts on :9092) and returns the
// value of gardenio_agent_spend_usd_total for the given label set, parsed out of
// the Prometheus text exposition. found=false when the series line is absent.
//
// Parsing the served text (rather than reg.Gather()) is the point of the e2e:
// it proves the counter reaches the wire in the exposition format Prometheus
// scrapes, with the label set intact.
func servedCounter(t *testing.T, reg *prometheus.Registry, want map[string]string) (value float64, found bool) {
	t.Helper()
	srv := httptest.NewServer(promhttp.HandlerFor(reg, promhttp.HandlerOpts{}))
	t.Cleanup(srv.Close)

	resp, err := http.Get(srv.URL + "/metrics")
	if err != nil {
		t.Fatalf("GET /metrics: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET /metrics status = %d, want 200", resp.StatusCode)
	}
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read /metrics body: %v", err)
	}
	return parseCounterLine(string(body), "gardenio_agent_spend_usd_total", want)
}

// parseCounterLine finds the sample line for metric with EXACTLY the wanted
// label set in Prometheus text exposition and returns its float value. A line
// matches only when every wanted label is present with the wanted value AND the
// line carries no extra labels beyond those wanted (an exact label-set match, so
// a sibling series with an extra label cannot be mistaken for the target).
func parseCounterLine(text, metric string, want map[string]string) (float64, bool) {
	for _, line := range strings.Split(text, "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		if !strings.HasPrefix(line, metric+"{") {
			continue
		}
		open := strings.IndexByte(line, '{')
		closeIdx := strings.LastIndexByte(line, '}')
		if open < 0 || closeIdx < 0 || closeIdx < open {
			continue
		}
		labels := parseLabelSet(line[open+1 : closeIdx])
		if len(labels) != len(want) {
			continue
		}
		match := true
		for k, v := range want {
			if labels[k] != v {
				match = false
				break
			}
		}
		if !match {
			continue
		}
		valStr := strings.TrimSpace(line[closeIdx+1:])
		f, err := strconv.ParseFloat(valStr, 64)
		if err != nil {
			return 0, false
		}
		return f, true
	}
	return 0, false
}

// parseLabelSet parses a Prometheus label block body (the text between { and })
// into a map. Values are double-quoted; this handles the simple, unescaped label
// values this metric emits (agent/project/role/model/account are identifier-safe).
func parseLabelSet(body string) map[string]string {
	out := map[string]string{}
	for _, pair := range strings.Split(body, ",") {
		pair = strings.TrimSpace(pair)
		if pair == "" {
			continue
		}
		eq := strings.IndexByte(pair, '=')
		if eq < 0 {
			continue
		}
		key := strings.TrimSpace(pair[:eq])
		val := strings.TrimSpace(pair[eq+1:])
		val = strings.TrimPrefix(val, `"`)
		val = strings.TrimSuffix(val, `"`)
		out[key] = val
	}
	return out
}

// ─────────────────────────────────────────────────────────────────────────────
// Leg A + B: spawn -> close -> served /metrics scrape, full round-trip.
// ─────────────────────────────────────────────────────────────────────────────

// TestSessionSpendE2E_SpawnCloseServedCounter drives the WHOLE per-session
// pipeline through real HTTP: captureBaseline at spawn stashes the /spend/tags
// snapshot ONTO the agent bead (born-at-0 counter series), then reconcileAtClose
// reads that daemon-persisted baseline BACK, computes the delta against the
// close-time /spend/tags total, writes the authoritative fields onto the bead,
// and advances the served counter by exactly the settled delta.
func TestSessionSpendE2E_SpawnCloseServedCounter(t *testing.T) {
	const (
		agent   = "gardenio-spendcrew-a1"
		beadID  = "crew-gardenio-crew-spendcrew-a1"
		project = "gardenio"
		role    = "crew"
		model   = "claude-opus-4-8"
		account = "litellm-prod"
	)
	// The agent tag reads 4.25 at spawn and 7.75 at close -> settled 3.50.
	spawnEntries := []tagSpendEntry{{IndividualRequestTag: agentTag(agent), LogCount: 8, TotalSpend: 4.25}}
	closeEntries := []tagSpendEntry{{IndividualRequestTag: agentTag(agent), LogCount: 20, TotalSpend: 7.75}}

	daemon := newRWDaemon()
	daemon.seedAgent(beadID, map[string]any{
		"agent": agent, "project": project, "role": role, "agent_state": "spawning",
	})

	sm, reg := newSpendMetricsReg(t)
	labels := map[string]string{
		"agent": agent, "project": project, "role": role, "model": model, "account": account,
	}

	// ── Spawn: capture the baseline against the spawn-time /spend/tags total. ──
	rSpawn := e2eReconciler(t, daemon, spawnEntries, sm, model, account)
	rSpawn.captureBaseline(context.Background(), beadID, agent, project, role)

	// The controller WROTE the baseline onto the bead (persistence path). Read it
	// back the way the close path will — via the daemon, not from memory.
	if got := daemon.fieldString(beadID, spendBaselineField); got != "4.25" {
		t.Fatalf("after captureBaseline, bead %s = %q, want \"4.25\" (baseline must be persisted on the bead)", spendBaselineField, got)
	}
	// The series exists at 0 immediately after spawn (increase()-visibility fix).
	if v, found := servedCounter(t, reg, labels); !found || v != 0 {
		t.Fatalf("post-spawn served counter = (%v, found=%v), want (0, true) — born-at-0 series must be scrapeable", v, found)
	}

	// ── Close: read the persisted baseline back, settle against the close total. ──
	rClose := e2eReconciler(t, daemon, closeEntries, sm, model, account)
	bead, err := rClose.daemon.GetBead(context.Background(), beadID)
	if err != nil {
		t.Fatalf("GetBead at close: %v", err)
	}
	rClose.reconcileAtClose(context.Background(), bead, agent)

	// Leg B: the CONTROLLER wrote the authoritative cost onto the bead from the
	// live /spend/tags delta — total_cost_usd = lifetime (7.75 − first-baseline
	// 4.25 = 3.50), plus the settlement watermark and the write-once first baseline.
	if got := daemon.fieldString(beadID, "total_cost_usd"); got != "3.5" {
		t.Errorf("total_cost_usd on bead = %q, want \"3.5\" (controller-written lifetime, not a hand-set field)", got)
	}
	if got := daemon.fieldString(beadID, spendCloseField); got != "7.75" {
		t.Errorf("%s on bead = %q, want \"7.75\" (settlement watermark)", spendCloseField, got)
	}
	if got := daemon.fieldString(beadID, spendFirstBaselineField); got != "4.25" {
		t.Errorf("%s on bead = %q, want \"4.25\" (write-once first baseline)", spendFirstBaselineField, got)
	}

	// Leg A: the served counter advanced by exactly the settled delta.
	if v, found := servedCounter(t, reg, labels); !found || v != 3.50 {
		t.Fatalf("post-close served counter = (%v, found=%v), want (3.50, true)", v, found)
	}
}

// TestSessionSpendE2E_ZeroCostCloseDoesNotAdvance proves a session that made no
// billable spend (the agent tag total is unchanged from the baseline) leaves the
// served counter at 0 — RecordSpend drops the non-positive delta, so a nil/zero
// cost close never advances the counter. This is the "a nil-cost event does NOT
// advance it" half of the task, in the CURRENT (close-driven) topology.
func TestSessionSpendE2E_ZeroCostCloseDoesNotAdvance(t *testing.T) {
	const (
		agent   = "gardenio-spendcrew-z0"
		beadID  = "crew-gardenio-crew-spendcrew-z0"
		project = "gardenio"
		role    = "crew"
		model   = "claude-opus-4-8"
		account = "litellm-prod"
	)
	// Same tag total at spawn and close: 5.00 -> 5.00, so the session cost is 0.
	entries := []tagSpendEntry{{IndividualRequestTag: agentTag(agent), LogCount: 3, TotalSpend: 5.00}}

	daemon := newRWDaemon()
	daemon.seedAgent(beadID, map[string]any{
		"agent": agent, "project": project, "role": role, "agent_state": "spawning",
	})

	sm, reg := newSpendMetricsReg(t)
	labels := map[string]string{
		"agent": agent, "project": project, "role": role, "model": model, "account": account,
	}

	rSpawn := e2eReconciler(t, daemon, entries, sm, model, account)
	rSpawn.captureBaseline(context.Background(), beadID, agent, project, role)

	rClose := e2eReconciler(t, daemon, entries, sm, model, account)
	bead, err := rClose.daemon.GetBead(context.Background(), beadID)
	if err != nil {
		t.Fatalf("GetBead at close: %v", err)
	}
	rClose.reconcileAtClose(context.Background(), bead, agent)

	// The born-at-0 series exists and stays at 0 — a zero-cost close is a no-op.
	v, found := servedCounter(t, reg, labels)
	if !found {
		t.Fatalf("zero-cost series missing from served /metrics — the born-at-0 series must still be scrapeable")
	}
	if v != 0 {
		t.Errorf("zero-cost served counter = %v, want 0 (a nil/zero-cost close must not advance the counter)", v)
	}
}

// TestSessionSpendE2E_RespawnAccumulatesAcrossSessions proves the "one increment
// per session, accumulate across a respawning agent" property over the real
// round-trip: session 1 settles, then session 2 (re-spawn: fresh baseline at the
// prior close total) settles its OWN delta, and the served counter reads the SUM
// of both sessions — not the last session's delta, and not a double-count of a
// re-delivered close.
func TestSessionSpendE2E_RespawnAccumulatesAcrossSessions(t *testing.T) {
	const (
		agent   = "gardenio-spendcrew-r2"
		beadID  = "crew-gardenio-crew-spendcrew-r2"
		project = "gardenio"
		role    = "crew"
		model   = "m"
		account = "acct"
	)
	daemon := newRWDaemon()
	daemon.seedAgent(beadID, map[string]any{
		"agent": agent, "project": project, "role": role, "agent_state": "spawning",
	})
	sm, reg := newSpendMetricsReg(t)
	labels := map[string]string{
		"agent": agent, "project": project, "role": role, "model": model, "account": account,
	}

	// ── Session 1: baseline 4.25, close 7.75 -> settled 3.50. ──
	e2eReconciler(t, daemon, []tagSpendEntry{{IndividualRequestTag: agentTag(agent), TotalSpend: 4.25}}, sm, model, account).
		captureBaseline(context.Background(), beadID, agent, project, role)
	rClose1 := e2eReconciler(t, daemon, []tagSpendEntry{{IndividualRequestTag: agentTag(agent), TotalSpend: 7.75}}, sm, model, account)
	bead1, err := rClose1.daemon.GetBead(context.Background(), beadID)
	if err != nil {
		t.Fatalf("GetBead close 1: %v", err)
	}
	rClose1.reconcileAtClose(context.Background(), bead1, agent)

	if v, found := servedCounter(t, reg, labels); !found || v != 3.50 {
		t.Fatalf("after session 1, served counter = (%v, %v), want (3.50, true)", v, found)
	}

	// ── Session 2 (re-spawn): fresh baseline at the current tag total (7.75),
	// close 10.00 -> settled 2.25. Lifetime becomes 10.00 − first-baseline 4.25 =
	// 5.75; the counter accumulates 3.50 + 2.25 = 5.75. ──
	e2eReconciler(t, daemon, []tagSpendEntry{{IndividualRequestTag: agentTag(agent), TotalSpend: 7.75}}, sm, model, account).
		captureBaseline(context.Background(), beadID, agent, project, role)
	if got := daemon.fieldString(beadID, spendBaselineField); got != "7.75" {
		t.Fatalf("session 2 baseline on bead = %q, want \"7.75\" (re-captured at respawn)", got)
	}
	rClose2 := e2eReconciler(t, daemon, []tagSpendEntry{{IndividualRequestTag: agentTag(agent), TotalSpend: 10.00}}, sm, model, account)
	bead2, err := rClose2.daemon.GetBead(context.Background(), beadID)
	if err != nil {
		t.Fatalf("GetBead close 2: %v", err)
	}
	rClose2.reconcileAtClose(context.Background(), bead2, agent)

	if got := daemon.fieldString(beadID, "total_cost_usd"); got != "5.75" {
		t.Errorf("lifetime total_cost_usd = %q, want \"5.75\" (accumulate across sessions)", got)
	}
	if v, found := servedCounter(t, reg, labels); !found || v != 5.75 {
		t.Fatalf("after session 2, served counter = (%v, %v), want (5.75, true) — must SUM both sessions", v, found)
	}
}
