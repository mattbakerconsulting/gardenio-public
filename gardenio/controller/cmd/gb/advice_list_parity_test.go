package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"gardenio/controller/internal/advice"
	"gardenio/controller/internal/beadsapi"
)

// adviceParityFixture is the single source of truth shared by the parity tests
// below: a representative mix of delivered advice and every flavour of bead
// gb prime drops — legacy hippo mirrors (label source:hippo-memory, label
// source:hippo-global, the source field variant) and the Phase-3 denied
// bare-project broadcast (ka-0000007790). Keep the two ID sets in sync with
// the asserts in the tests.
func adviceParityFixture() (beads []*beadsapi.BeadDetail, wantDelivered, wantUndelivered map[string]bool) {
	beads = []*beadsapi.BeadDetail{
		{ID: "gd-global1", Title: "plain global advice", Labels: []string{"global"}},
		{ID: "gd-proj1", Title: "project+role advice", Labels: []string{"g0:project:gardenio", "g0:role:crew"}},
		{ID: "gd-broadcastOptIn", Title: "intentional project broadcast", Labels: []string{"project:gardenio", advice.BroadcastOptInLabel}},
		{ID: "gd-hippoMemLbl", Title: "Q: Gardener cycle (2026-04-16) ... → A: ack", Labels: []string{"g0:project:gardenio", "source:hippo-memory"}},
		{ID: "gd-hippoGlobLbl", Title: "malformed mirror title", Labels: []string{"global", "source:hippo-global"}},
		{ID: "gd-hippoSrcFld", Title: "mirror via source field only", Labels: []string{"global"}, Fields: map[string]string{"source": "hippo-memory"}},
		{ID: "gd-bareBroadcast", Title: "bare-project broadcast without opt-in", Labels: []string{"project:gardenio"}},
	}
	wantDelivered = map[string]bool{"gd-global1": true, "gd-proj1": true, "gd-broadcastOptIn": true}
	wantUndelivered = map[string]bool{"gd-hippoMemLbl": true, "gd-hippoGlobLbl": true, "gd-hippoSrcFld": true, "gd-bareBroadcast": true}
	return beads, wantDelivered, wantUndelivered
}

func idSet(beads []*beadsapi.BeadDetail) map[string]bool {
	s := make(map[string]bool, len(beads))
	for _, b := range beads {
		s[b.ID] = true
	}
	return s
}

func sameSet(a, b map[string]bool) bool {
	if len(a) != len(b) {
		return false
	}
	for k := range a {
		if !b[k] {
			return false
		}
	}
	return true
}

// TestAdviceListDeliveryMatchesPrime is the core regression test for
// ka-0000002735: the set of beads `gb advice list` surfaces (by default) MUST
// equal the set `gb prime` actually delivers. Both must apply the identical
// hippo-mirror predicate. We assert this by running the SAME fixture through
// the list path (partitionDeliverableAdvice) and the prime path
// (filterOutHippoMirrors) and requiring the surviving ID sets to be equal.
//
// If list ever stops filtering (the original bug) or the two predicates drift,
// the delivered ID sets diverge and this fails.
func TestAdviceListDeliveryMatchesPrime(t *testing.T) {
	beads, wantDelivered, wantUndelivered := adviceParityFixture()

	// list path.
	delivered, undelivered := partitionDeliverableAdvice(beads)
	gotDelivered := idSet(delivered)
	gotUndelivered := idSet(undelivered)

	if !sameSet(gotDelivered, wantDelivered) {
		t.Errorf("list delivered set = %v, want %v", gotDelivered, wantDelivered)
	}
	if !sameSet(gotUndelivered, wantUndelivered) {
		t.Errorf("list undelivered set = %v, want %v", gotUndelivered, wantUndelivered)
	}

	// prime path: wrap the same beads as matched advice and run the prime
	// delivery filters — the broadcast deny ListAdviceForAgent applies plus
	// the hippo-mirror filter outputAdvice applies.
	var matched []advice.MatchedAdvice
	for _, b := range beads {
		if advice.IsDeniedBroadcast(b) {
			continue
		}
		matched = append(matched, advice.MatchedAdvice{Bead: b, Scope: "global", ScopeHeader: "Global"})
	}
	primeSurvivors := filterOutHippoMirrors(matched)
	primeSet := make(map[string]bool, len(primeSurvivors))
	for _, m := range primeSurvivors {
		primeSet[m.Bead.ID] = true
	}

	// The whole point of the fix: list delivery truth == prime delivery truth.
	if !sameSet(gotDelivered, primeSet) {
		t.Errorf("DIVERGENCE: gb advice list delivered=%v but gb prime delivers=%v (ka-0000002735)", gotDelivered, primeSet)
	}

	// And every bead is accounted for in exactly one bucket.
	if len(delivered)+len(undelivered) != len(beads) {
		t.Errorf("partition lost beads: %d delivered + %d undelivered != %d input",
			len(delivered), len(undelivered), len(beads))
	}
}

// TestAnnotateAdviceDelivery asserts the --show-undelivered JSON annotation
// flags each bead with prime delivery truth, preserving order.
func TestAnnotateAdviceDelivery(t *testing.T) {
	beads, wantDelivered, _ := adviceParityFixture()

	items := annotateAdviceDelivery(beads)
	if len(items) != len(beads) {
		t.Fatalf("annotate returned %d items, want %d", len(items), len(beads))
	}
	for i, it := range items {
		if it.BeadDetail != beads[i] {
			t.Errorf("item %d order/identity mismatch: got %s", i, it.ID)
		}
		want := wantDelivered[it.ID]
		if it.Delivered != want {
			t.Errorf("bead %s delivered=%v, want %v", it.ID, it.Delivered, want)
		}
	}

	// The embedded *BeadDetail must flatten into the JSON alongside the added
	// "delivered" boolean.
	data, err := json.Marshal(items[0])
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if !strings.Contains(string(data), `"id"`) || !strings.Contains(string(data), `"delivered"`) {
		t.Errorf("annotated JSON missing id/delivered fields: %s", data)
	}
}

// setupAdviceListDaemon points the global daemon at a fake server that returns
// the parity fixture from GET /v1/beads, and resets adviceListCmd flag state.
func setupAdviceListDaemon(t *testing.T) {
	t.Helper()
	beads, _, _ := adviceParityFixture()

	type wireBead struct {
		ID     string            `json:"id"`
		Title  string            `json:"title"`
		Type   string            `json:"type"`
		Status string            `json:"status"`
		Labels []string          `json:"labels"`
		Fields map[string]string `json:"fields,omitempty"`
	}
	wire := make([]wireBead, len(beads))
	for i, b := range beads {
		wire[i] = wireBead{ID: b.ID, Title: b.Title, Type: "advice", Status: "open", Labels: b.Labels, Fields: b.Fields}
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/v1/beads", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"beads": wire, "total": len(wire)})
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	c, err := beadsapi.New(beadsapi.Config{HTTPAddr: srv.URL})
	if err != nil {
		t.Fatalf("beadsapi.New: %v", err)
	}
	old := daemon
	daemon = c
	t.Cleanup(func() { daemon = old })

	// Calling RunE directly bypasses cobra's Execute, which is what normally
	// seeds the command context — set one explicitly.
	adviceListCmd.SetContext(context.Background())

	// Reset flag + global output state between subtests.
	t.Cleanup(func() {
		_ = adviceListCmd.Flags().Set("show-undelivered", "false")
		_ = adviceListCmd.Flags().Set("for", "")
		jsonOutput = false
	})
}

// TestAdviceListCmd_DefaultHidesHippoMirrors exercises the full command path:
// by default `gb advice list` shows only delivered beads, hides hippo mirrors,
// and prints the "use --show-undelivered" hint — matching gb prime.
func TestAdviceListCmd_DefaultHidesHippoMirrors(t *testing.T) {
	setupAdviceListDaemon(t)
	_ = adviceListCmd.Flags().Set("show-undelivered", "false")

	out := string(captureStdout(t, func() error {
		return adviceListCmd.RunE(adviceListCmd, nil)
	}))

	_, wantDelivered, wantUndelivered := adviceParityFixture()
	for id := range wantDelivered {
		if !strings.Contains(out, id) {
			t.Errorf("default list missing delivered bead %s:\n%s", id, out)
		}
	}
	for id := range wantUndelivered {
		if strings.Contains(out, id) {
			t.Errorf("default list leaked undelivered bead %s (this is the ka-0000002735 bug):\n%s", id, out)
		}
	}
	if !strings.Contains(out, "show-undelivered") {
		t.Errorf("default list missing the hidden-mirror hint:\n%s", out)
	}
}

// TestAdviceListCmd_ShowUndeliveredAnnotates exercises --show-undelivered:
// every bead appears, mirrors are marked NOT-DELIVERED, delivered beads are not.
func TestAdviceListCmd_ShowUndeliveredAnnotates(t *testing.T) {
	setupAdviceListDaemon(t)
	if err := adviceListCmd.Flags().Set("show-undelivered", "true"); err != nil {
		t.Fatal(err)
	}

	out := string(captureStdout(t, func() error {
		return adviceListCmd.RunE(adviceListCmd, nil)
	}))

	_, wantDelivered, wantUndelivered := adviceParityFixture()
	for id := range wantDelivered {
		if !strings.Contains(out, id) {
			t.Errorf("--show-undelivered list missing bead %s:\n%s", id, out)
		}
	}
	for id := range wantUndelivered {
		if !strings.Contains(out, id) {
			t.Errorf("--show-undelivered list missing bead %s:\n%s", id, out)
		}
	}
	if !strings.Contains(out, "NOT-DELIVERED") {
		t.Errorf("--show-undelivered list missing NOT-DELIVERED annotation:\n%s", out)
	}

	// Each undelivered line carries the NOT-DELIVERED marker; the delivered
	// lines do not (they show "yes").
	for line := range strings.SplitSeq(out, "\n") {
		id, _, _ := strings.Cut(line, " ")
		if wantUndelivered[id] && !strings.Contains(line, "NOT-DELIVERED") {
			t.Errorf("undelivered row not annotated NOT-DELIVERED: %q", line)
		}
		if wantDelivered[id] && strings.Contains(line, "NOT-DELIVERED") {
			t.Errorf("delivered row wrongly marked NOT-DELIVERED: %q", line)
		}
	}
}

// TestAdviceListCmd_JSONDefaultFiltered asserts the JSON path is filtered to
// delivery truth by default (no hippo mirrors), and --show-undelivered adds the
// delivered annotation across the full set.
func TestAdviceListCmd_JSONDefaultFiltered(t *testing.T) {
	setupAdviceListDaemon(t)
	jsonOutput = true

	// Default JSON: only delivered beads.
	out := captureStdout(t, func() error {
		return adviceListCmd.RunE(adviceListCmd, nil)
	})
	beads, wantDelivered, wantUndelivered := adviceParityFixture()
	var defaultItems []map[string]any
	if err := json.Unmarshal(out, &defaultItems); err != nil {
		t.Fatalf("default json unmarshal: %v\n%s", err, out)
	}
	if len(defaultItems) != len(wantDelivered) {
		t.Errorf("default json has %d items, want %d (delivered only): %s", len(defaultItems), len(wantDelivered), out)
	}
	for _, it := range defaultItems {
		if id, _ := it["id"].(string); wantUndelivered[id] {
			t.Errorf("default json leaked undelivered bead %s", id)
		}
	}

	// --show-undelivered JSON: all beads, each with a delivered flag.
	if err := adviceListCmd.Flags().Set("show-undelivered", "true"); err != nil {
		t.Fatal(err)
	}
	out = captureStdout(t, func() error {
		return adviceListCmd.RunE(adviceListCmd, nil)
	})
	var allItems []map[string]any
	if err := json.Unmarshal(out, &allItems); err != nil {
		t.Fatalf("show-undelivered json unmarshal: %v\n%s", err, out)
	}
	if len(allItems) != len(beads) {
		t.Errorf("--show-undelivered json has %d items, want %d: %s", len(allItems), len(beads), out)
	}
	for _, it := range allItems {
		id, _ := it["id"].(string)
		delivered, ok := it["delivered"].(bool)
		if !ok {
			t.Errorf("item %s missing delivered bool: %v", id, it)
			continue
		}
		if delivered != wantDelivered[id] {
			t.Errorf("item %s delivered=%v, want %v", id, delivered, wantDelivered[id])
		}
	}
}
