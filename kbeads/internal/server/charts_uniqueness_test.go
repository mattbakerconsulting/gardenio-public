package server

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// TestUniquenessQuery_UsesLastOverTimeNotIncrease pins the exact PromQL the
// panels evaluate. The membership semantics are the whole correctness story
// (task ka-0000006905): last_over_time keeps an operator with a single sample
// or a counter reset inside the window in the distinct set, where increase()
// would drop it. If anyone "optimizes" this to increase()/rate(), this test
// fails loudly.
func TestUniquenessQuery_UsesLastOverTimeNotIncrease(t *testing.T) {
	for _, tc := range []struct{ window, want string }{
		{"1d", "count(count by (operator_id)(last_over_time(gardenio_operator_activity_total[1d])))"},
		{"7d", "count(count by (operator_id)(last_over_time(gardenio_operator_activity_total[7d])))"},
		{"30d", "count(count by (operator_id)(last_over_time(gardenio_operator_activity_total[30d])))"},
	} {
		got := uniquenessQuery(tc.window)
		if got != tc.want {
			t.Errorf("uniquenessQuery(%q) = %q, want %q", tc.window, got, tc.want)
		}
		if strings.Contains(got, "increase(") || strings.Contains(got, "rate(") {
			t.Errorf("uniquenessQuery(%q) must NOT use increase()/rate(): %q", tc.window, got)
		}
		if !strings.Contains(got, "last_over_time(") {
			t.Errorf("uniquenessQuery(%q) must use last_over_time(): %q", tc.window, got)
		}
		if !strings.Contains(got, "count by (operator_id)") {
			t.Errorf("uniquenessQuery(%q) must group by operator_id: %q", tc.window, got)
		}
	}
}

// fakeVM is an httptest server that records the PromQL it receives and returns
// a caller-supplied value per window selector. It lets us drive the handler
// and computeUniqueness without a real VictoriaMetrics while still exercising
// the full request/parse path of VMClient.QueryScalar.
func fakeVM(t *testing.T, byWindow map[string]string) (*VMClient, *[]string) {
	t.Helper()
	var seen []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := r.ParseForm(); err != nil {
			http.Error(w, "bad form", http.StatusBadRequest)
			return
		}
		q := r.Form.Get("query")
		seen = append(seen, q)
		val := "0"
		for win, v := range byWindow {
			if strings.Contains(q, "["+win+"]") {
				val = v
			}
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"status":"success","data":{"resultType":"vector","result":[{"metric":{},"value":[1780000000,"` + val + `"]}]}}`))
	}))
	t.Cleanup(srv.Close)
	return NewVMClient(srv.URL, ""), &seen
}

func TestComputeUniqueness_ParsesAllWindows(t *testing.T) {
	vm, seen := fakeVM(t, map[string]string{"1d": "4", "7d": "5", "30d": "6"})
	s := NewBeadsServer(newMockStore(), nil)
	s.SetVMClient(vm)

	at := time.Date(2026, 5, 30, 0, 0, 0, 0, time.UTC)
	resp, err := s.computeUniqueness(context.Background(), at)
	if err != nil {
		t.Fatalf("computeUniqueness: %v", err)
	}
	if resp.EvaluatedAt != at.Format(time.RFC3339) {
		t.Errorf("EvaluatedAt = %q, want %q", resp.EvaluatedAt, at.Format(time.RFC3339))
	}
	want := map[string]int64{"dau": 4, "wau": 5, "mau": 6}
	if len(resp.Panels) != 3 {
		t.Fatalf("got %d panels, want 3", len(resp.Panels))
	}
	for _, p := range resp.Panels {
		if want[p.Key] != p.Count {
			t.Errorf("panel %s: count = %d, want %d", p.Key, p.Count, want[p.Key])
		}
	}
	// DAU ≤ WAU ≤ MAU invariant for a single evaluation instant.
	var dau, wau, mau int64
	for _, p := range resp.Panels {
		switch p.Key {
		case "dau":
			dau = p.Count
		case "wau":
			wau = p.Count
		case "mau":
			mau = p.Count
		}
	}
	if !(dau <= wau && wau <= mau) {
		t.Errorf("expected DAU<=WAU<=MAU, got %d/%d/%d", dau, wau, mau)
	}
	if len(*seen) != 3 {
		t.Errorf("expected 3 VM queries, got %d: %v", len(*seen), *seen)
	}
}

func TestHandleChartsUniqueness_503WhenUnconfigured(t *testing.T) {
	_, _, h := newTestServer() // no VM client wired
	rec := doJSON(t, h, http.MethodGet, "/v1/charts/uniqueness", nil)
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status: got %d, want 503; body=%s", rec.Code, rec.Body.String())
	}
}

func TestHandleChartsUniqueness_HappyPath(t *testing.T) {
	s, _, h := newTestServer()
	vm, _ := fakeVM(t, map[string]string{"1d": "4", "7d": "5", "30d": "6"})
	s.SetVMClient(vm)

	rec := doJSON(t, h, http.MethodGet, "/v1/charts/uniqueness", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("status: got %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	var resp UniquenessResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(resp.Panels) != 3 {
		t.Fatalf("got %d panels, want 3", len(resp.Panels))
	}
}

// TestHandleChartsUniqueness_CacheCollapsesQueries asserts the L1 TTL cache
// serves the second request from memory — the fake VM sees exactly one batch
// of three queries across two handler calls.
func TestHandleChartsUniqueness_CacheCollapsesQueries(t *testing.T) {
	s, _, h := newTestServer()
	vm, seen := fakeVM(t, map[string]string{"1d": "4", "7d": "5", "30d": "6"})
	s.SetVMClient(vm)

	if rec := doJSON(t, h, http.MethodGet, "/v1/charts/uniqueness", nil); rec.Code != http.StatusOK {
		t.Fatalf("first request: %d %s", rec.Code, rec.Body.String())
	}
	if rec := doJSON(t, h, http.MethodGet, "/v1/charts/uniqueness", nil); rec.Code != http.StatusOK {
		t.Fatalf("second request: %d %s", rec.Code, rec.Body.String())
	}
	if len(*seen) != 3 {
		t.Errorf("expected cache to collapse to 3 VM queries total, got %d: %v", len(*seen), *seen)
	}
}

func TestUniquenessCache_GetExpires(t *testing.T) {
	c := &uniquenessCache{ttl: 20 * time.Millisecond}
	if c.get() != nil {
		t.Fatal("empty cache should return nil")
	}
	c.put(&UniquenessResponse{EvaluatedAt: "x"})
	if c.get() == nil {
		t.Fatal("fresh cache should return snapshot")
	}
	time.Sleep(30 * time.Millisecond)
	if c.get() != nil {
		t.Fatal("expired cache should return nil")
	}
}

// TestVMClient_QueryScalar_EmptyVectorIsZero verifies an empty instant vector
// (no operators active in the window) decodes to an honest zero, not an error.
func TestVMClient_QueryScalar_EmptyVectorIsZero(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"status":"success","data":{"resultType":"vector","result":[]}}`))
	}))
	defer srv.Close()
	vm := NewVMClient(srv.URL, "")
	val, ok, err := vm.QueryScalar(context.Background(), "count(x)", time.Now())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !ok || val != 0 {
		t.Fatalf("empty vector: got (val=%v, ok=%v), want (0, true)", val, ok)
	}
}

func TestVMClient_QueryScalar_PromQLError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"status":"error","errorType":"bad_data","error":"parse failure"}`))
	}))
	defer srv.Close()
	vm := NewVMClient(srv.URL, "")
	if _, _, err := vm.QueryScalar(context.Background(), "count(", time.Now()); err == nil {
		t.Fatal("expected error on status:error response")
	}
}

func TestNewVMClient_NilWhenUnconfigured(t *testing.T) {
	if NewVMClient("", "") != nil {
		t.Error("NewVMClient with empty baseURL should return nil")
	}
	if NewVMClient("  ", "tok") != nil {
		t.Error("NewVMClient with blank baseURL should return nil")
	}
}
