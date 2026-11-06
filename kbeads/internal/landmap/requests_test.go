package landmap

import (
	"bytes"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"

	"gardenio/lands/contract/landsv1"
)

// counterSum is the sum of every series of c.
func counterSum(t *testing.T, c *prometheus.CounterVec) float64 {
	t.Helper()
	reg := prometheus.NewPedanticRegistry()
	reg.MustRegister(c)
	mfs, err := reg.Gather()
	if err != nil {
		t.Fatal(err)
	}
	var n float64
	for _, mf := range mfs {
		for _, m := range mf.GetMetric() {
			n += m.GetCounter().GetValue()
		}
	}
	return n
}

// requestLands is every land label value Requests has a series for.
func requestLands(t *testing.T) []string {
	t.Helper()
	reg := prometheus.NewPedanticRegistry()
	reg.MustRegister(Requests)
	mfs, err := reg.Gather()
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	for _, mf := range mfs {
		for _, m := range mf.GetMetric() {
			for _, l := range m.GetLabel() {
				if l.GetName() == "land" {
					out = append(out, l.GetValue())
				}
			}
		}
	}
	return out
}

// opRecorder is a landsv1.ServeMux that records the op of each route the
// generated registration hands it.
type opRecorder struct{ ops *[]string }

func (o opRecorder) HandleFunc(_ string, h func(http.ResponseWriter, *http.Request)) {
	*o.ops = append(*o.ops, operationID(h))
}

func (opRecorder) ServeHTTP(w http.ResponseWriter, r *http.Request) { http.NotFound(w, r) }

// Every route the contract registers is named for a row of Verbs, and every
// row is served by exactly one route: op is the contract's operationId.
func TestOperationID_EveryRouteNamesAVerb(t *testing.T) {
	var ops []string
	landsv1.HandlerWithOptions(New(http.NotFoundHandler(), fakeLands{}, nil), landsv1.StdHTTPServerOptions{
		BaseURL:    BaseURL,
		BaseRouter: opRecorder{&ops},
	})
	for _, v := range Verbs {
		n := 0
		for _, op := range ops {
			if op == v.Op {
				n++
			}
		}
		if n != 1 {
			t.Errorf("verb %s is served by %d routes", v.Op, n)
		}
	}
	if len(ops) != len(Verbs) {
		t.Errorf("%d routes for %d verbs: %v", len(ops), len(Verbs), ops)
	}
}

// Requests counts each Lands API response once, in Guard: an admitted land
// by name, an unknown land as "" (no series of its own), an answer from
// before the map with no op or caller, and a deferred verb as not_found.
func TestRequests_Counted(t *testing.T) {
	_, member := newMapFixture(t, fakeLands{})
	_, operator := newMapFixture(t, fakeLands{caller: CallerOperator})
	_, agent := newMapFixture(t, fakeLands{caller: CallerAgent})
	_, applicant := newMapFixture(t, fakeLands{applicant: true})
	for _, tc := range []struct {
		name   string
		h      http.Handler
		path   string
		refuse string
		labels [4]string // land, op, code, caller
	}{
		{"admitted land", member, "/acme", "", [4]string{"acme", "getLand", codeOK, "member"}},
		{"admitted land, a list", member, "/acme/tickets", "", [4]string{"acme", "listTickets", codeOK, "member"}},
		{"admitted land, an operator", operator, "/acme/tickets", "", [4]string{"acme", "listTickets", codeOK, "operator"}},
		{"admitted land, its agent", agent, "/acme/whoami", "", [4]string{"acme", "whoami", codeOK, "agent"}},
		{"admitted land, a bead it does not have", member, "/acme/tickets/zz-t1", "", [4]string{"acme", "getTicket", "not_found", "member"}},
		{"unknown land", member, "/nosuchland-a1b2c3", "", [4]string{"", "getLand", "not_found", "member"}},
		{"a land name the contract refuses", member, "/NoSuch", "", [4]string{"", "getLand", "not_found", "member"}},
		{"deferred verb", member, "/acme/threads", "", [4]string{"", "listThreads", "not_found", "member"}},
		{"applicant", applicant, "/acme", "", [4]string{"", "getLand", "forbidden", ""}},
		{"pre-map 401", member, "/acme", "401", [4]string{"", "", "unauthenticated", ""}},
		{"pre-map 429", member, "/acme/tickets", "429", [4]string{"", "", "rate_limited", ""}},
		{"no such route", member, "/acme/nowhere", "", [4]string{"", "", "not_found", ""}},
	} {
		c := Requests.WithLabelValues(tc.labels[:]...)
		before, total := testutil.ToFloat64(c), counterSum(t, Requests)
		var hdr []string
		if tc.refuse != "" {
			hdr = []string{"X-Test-Refuse", tc.refuse}
		}
		do(t, Guard(outerRefusal(tc.h)), "GET", tc.path, "", hdr...)
		if got := testutil.ToFloat64(c) - before; got != 1 {
			t.Errorf("%s: Requests%v rose by %v, want 1", tc.name, tc.labels, got)
		}
		if got := counterSum(t, Requests) - total; got != 1 {
			t.Errorf("%s: Requests rose by %v in all, want 1", tc.name, got)
		}
	}
	for _, land := range requestLands(t) {
		if land != "" && land != "acme" {
			t.Errorf("Requests has a series for land %q, which the map never admitted", land)
		}
	}
}

// A map answer that carries a daemon field key is a structural hit: counted,
// and logged by rule, op, land and correlation id, never with its body.
func TestDenylistHits_ForcedLeak(t *testing.T) {
	var logs bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&logs, nil)))
	defer slog.SetDefault(prev)

	structural := DenylistHits.WithLabelValues(ruleFieldKey, hitStructural)
	before := testutil.ToFloat64(structural)
	w := httptest.NewRecorder()
	tw := &tallyWriter{ResponseWriter: w, t: &tally{op: "getTicket", land: "acme"}}
	writeJSON(tw, http.StatusOK, map[string]any{"id": "ac-t1", "fields": map[string]any{"leak-marker": "x"}})
	if got := testutil.ToFloat64(structural) - before; got != 1 {
		t.Errorf("DenylistHits{%s,%s} rose by %v, want 1", ruleFieldKey, hitStructural, got)
	}
	if !strings.Contains(w.Body.String(), "leak-marker") {
		t.Errorf("the answer was not written: %q", w.Body.String())
	}
	out := logs.String()
	for _, want := range []string{"rule=field_key", "kind=structural", "op=getTicket", "land=acme"} {
		if !strings.Contains(out, want) {
			t.Errorf("hit log lacks %s:\n%s", want, out)
		}
	}
	if strings.Contains(out, "leak-marker") || strings.Contains(out, "ac-t1") {
		t.Errorf("hit log carries the answer:\n%s", out)
	}

	// A text rule is labelled with its line, and a problem's hit is logged
	// under its correlation id.
	text := DenylistHits.WithLabelValues("daemon", hitText)
	before = testutil.ToFloat64(text)
	logs.Reset()
	writeProblem(tw, landsv1.InvalidRequest, "c0ffee", "the daemon said no")
	if got := testutil.ToFloat64(text) - before; got != 1 {
		t.Errorf("DenylistHits{daemon,text} rose by %v, want 1", got)
	}
	if out := logs.String(); !strings.Contains(out, "correlation_id=c0ffee") || strings.Contains(out, "said no") {
		t.Errorf("problem hit log:\n%s", out)
	}

	// The minted correlation id is masked: hex that spells a token is no hit.
	before = counterSum(t, DenylistHits)
	writeProblem(httptest.NewRecorder(), landsv1.NotFound, "4bead31d6905e1493668ced4", "")
	if got := counterSum(t, DenylistHits) - before; got != 0 {
		t.Errorf("a minted correlation id counted %v hits", got)
	}
}
