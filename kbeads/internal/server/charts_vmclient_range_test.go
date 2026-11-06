package server

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"testing"
	"time"
)

// charts_vmclient_range_test.go — unit tests for VMClient.QueryRange (E5.1,
// gd-Tuf8bJTEDI). These mock the VictoriaMetrics /api/v1/query_range endpoint
// with httptest so they need no running VM; the real-VM correctness story for
// the catalog lives in the end-to-end handler tests (E5.5).

// newRangeMock returns a test server that asserts the request shape and
// replies with the given JSON body. It records the last received form so a
// test can assert the start/end/step the client sent.
func newRangeMock(t *testing.T, body string) (*httptest.Server, *map[string]string) {
	t.Helper()
	got := map[string]string{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v1/query_range" {
			t.Errorf("unexpected path %q", r.URL.Path)
		}
		if r.Method != http.MethodPost {
			t.Errorf("unexpected method %q", r.Method)
		}
		_ = r.ParseForm()
		for _, k := range []string{"query", "start", "end", "step"} {
			got[k] = r.FormValue(k)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, body)
	}))
	t.Cleanup(srv.Close)
	return srv, &got
}

func TestQueryRange_MatrixDecodesPointsInOrder(t *testing.T) {
	body := `{"status":"success","data":{"resultType":"matrix","result":[
	  {"metric":{"agent":"a1"},"values":[[1700000000,"1.5"],[1700003600,"2.5"]]},
	  {"metric":{"agent":"a2"},"values":[[1700000000,"10"],[1700003600,"20"]]}
	]}}`
	srv, got := newRangeMock(t, body)
	c := NewVMClient(srv.URL, "")

	start := time.Unix(1700000000, 0).UTC()
	end := time.Unix(1700003600, 0).UTC()
	series, err := c.QueryRange(context.Background(), `sum by(agent)(x)`, start, end, time.Hour)
	if err != nil {
		t.Fatalf("QueryRange: %v", err)
	}
	if len(series) != 2 {
		t.Fatalf("want 2 series, got %d", len(series))
	}
	// Request shape: step in seconds, start/end as unix seconds.
	if (*got)["step"] != strconv.Itoa(3600) {
		t.Errorf("step = %q, want 3600", (*got)["step"])
	}
	if (*got)["start"] != "1700000000" || (*got)["end"] != "1700003600" {
		t.Errorf("start/end = %q/%q", (*got)["start"], (*got)["end"])
	}
	// First series points decode in ascending time order with correct values.
	a1 := series[0]
	if a1.Metric["agent"] != "a1" {
		t.Errorf("series[0] agent = %q", a1.Metric["agent"])
	}
	if len(a1.Points) != 2 {
		t.Fatalf("series[0] want 2 points, got %d", len(a1.Points))
	}
	if !a1.Points[0].T.Equal(start) || a1.Points[0].Value != 1.5 {
		t.Errorf("point0 = %+v", a1.Points[0])
	}
	if a1.Points[1].Value != 2.5 {
		t.Errorf("point1 value = %v, want 2.5", a1.Points[1].Value)
	}
}

func TestQueryRange_EmptyResultIsHonestEmpty(t *testing.T) {
	body := `{"status":"success","data":{"resultType":"matrix","result":[]}}`
	srv, _ := newRangeMock(t, body)
	c := NewVMClient(srv.URL, "")

	series, err := c.QueryRange(context.Background(), `x`, time.Unix(0, 0), time.Unix(60, 0), time.Minute)
	if err != nil {
		t.Fatalf("QueryRange: %v", err)
	}
	if series == nil {
		t.Fatal("want non-nil empty slice, got nil")
	}
	if len(series) != 0 {
		t.Fatalf("want 0 series, got %d", len(series))
	}
}

func TestQueryRange_NaNGapsDropped(t *testing.T) {
	// A series with a NaN gap in the middle should yield only the real points.
	body := `{"status":"success","data":{"resultType":"matrix","result":[
	  {"metric":{},"values":[[1700000000,"1"],[1700003600,"NaN"],[1700007200,"3"]]}
	]}}`
	srv, _ := newRangeMock(t, body)
	c := NewVMClient(srv.URL, "")

	series, err := c.QueryRange(context.Background(), `x`,
		time.Unix(1700000000, 0), time.Unix(1700007200, 0), time.Hour)
	if err != nil {
		t.Fatalf("QueryRange: %v", err)
	}
	if len(series) != 1 {
		t.Fatalf("want 1 series, got %d", len(series))
	}
	if len(series[0].Points) != 2 {
		t.Fatalf("want 2 non-gap points, got %d", len(series[0].Points))
	}
	if series[0].Points[0].Value != 1 || series[0].Points[1].Value != 3 {
		t.Errorf("values = %v", series[0].Points)
	}
}

func TestQueryRange_FractionalTimestamp(t *testing.T) {
	body := `{"status":"success","data":{"resultType":"matrix","result":[
	  {"metric":{},"values":[[1700000000.5,"1"]]}
	]}}`
	srv, _ := newRangeMock(t, body)
	c := NewVMClient(srv.URL, "")

	series, err := c.QueryRange(context.Background(), `x`,
		time.Unix(1700000000, 0), time.Unix(1700000001, 0), time.Second)
	if err != nil {
		t.Fatalf("QueryRange: %v", err)
	}
	got := series[0].Points[0].T
	want := time.Unix(1700000000, 500000000).UTC()
	if got.Sub(want).Abs() > time.Millisecond {
		t.Errorf("ts = %v, want ~%v", got, want)
	}
}

func TestQueryRange_VMErrorStatusSurfaces(t *testing.T) {
	body := `{"status":"error","errorType":"bad_data","error":"unexpected token"}`
	srv, _ := newRangeMock(t, body)
	c := NewVMClient(srv.URL, "")

	_, err := c.QueryRange(context.Background(), `bogus(`,
		time.Unix(0, 0), time.Unix(60, 0), time.Minute)
	if err == nil {
		t.Fatal("want error on status:error response, got nil")
	}
}

func TestQueryRange_RejectsBadArgs(t *testing.T) {
	c := NewVMClient("http://unused", "")
	now := time.Unix(1700000000, 0)

	if _, err := c.QueryRange(context.Background(), `x`, now, now.Add(time.Hour), 0); err == nil {
		t.Error("want error for non-positive step")
	}
	if _, err := c.QueryRange(context.Background(), `x`, now.Add(time.Hour), now, time.Minute); err == nil {
		t.Error("want error for start after end")
	}
}

func TestQueryRange_HTTPErrorSurfaces(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadGateway)
	}))
	defer srv.Close()
	c := NewVMClient(srv.URL, "")

	_, err := c.QueryRange(context.Background(), `x`, time.Unix(0, 0), time.Unix(60, 0), time.Minute)
	if err == nil {
		t.Fatal("want error on HTTP 502, got nil")
	}
}
