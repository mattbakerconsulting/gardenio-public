package beadsapi

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestWindows_Routes(t *testing.T) {
	type call struct{ method, path, query, body string }
	var got []call
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		got = append(got, call{r.Method, r.URL.Path, r.URL.RawQuery, string(b)})
		switch {
		case r.Method == http.MethodPost:
			w.WriteHeader(http.StatusCreated)
			_, _ = w.Write([]byte(`{"window":{"id":"gd-w1","plan_id":"gd-p","land":"gardenio","state":"live","view_count":0},"token":"gw_x","path":"/w/gw_x"}`))
		case r.Method == http.MethodDelete:
			_, _ = w.Write([]byte(`{"window":{"id":"gd-w1","state":"revoked"}}`))
		case r.URL.Path == "/v1/windows/preview":
			_, _ = w.Write([]byte(`{"plan":{"title":"T"}}`))
		default:
			_, _ = w.Write([]byte(`{"windows":[{"id":"gd-w1","state":"live","view_count":3}]}`))
		}
	}))
	defer srv.Close()
	c := newTestClient(srv)
	ctx := context.Background()

	m, err := c.CreateWindow(ctx, "gd-p", 7)
	if err != nil || m.Token != "gw_x" || m.Path != "/w/gw_x" || m.Window.ID != "gd-w1" {
		t.Fatalf("CreateWindow = %+v, %v", m, err)
	}
	if _, err := c.CreateWindow(ctx, "gd-p", 0); err != nil {
		t.Fatal(err)
	}
	ws, err := c.ListWindows(ctx, "gar den")
	if err != nil || len(ws) != 1 || ws[0].ViewCount != 3 {
		t.Fatalf("ListWindows = %+v, %v", ws, err)
	}
	rv, err := c.RevokeWindow(ctx, "gd-w1")
	if err != nil || rv.State != "revoked" {
		t.Fatalf("RevokeWindow = %+v, %v", rv, err)
	}
	pv, err := c.PreviewWindow(ctx, "gd-p", 0)
	if err != nil || string(pv) != `{"plan":{"title":"T"}}` {
		t.Fatalf("PreviewWindow = %s, %v", pv, err)
	}
	if _, err := c.CreateLandWindow(ctx, "gardenio", 90); err != nil {
		t.Fatal(err)
	}
	if _, err := c.PreviewLandWindow(ctx, "gardenio", 7); err != nil {
		t.Fatal(err)
	}

	want := []call{
		{http.MethodPost, "/v1/windows", "", `{"expires_in_days":7,"plan_id":"gd-p"}`},
		{http.MethodPost, "/v1/windows", "", `{"plan_id":"gd-p"}`},
		{http.MethodGet, "/v1/windows", "land=gar+den", ""},
		{http.MethodDelete, "/v1/windows/gd-w1", "", ""},
		{http.MethodGet, "/v1/windows/preview", "plan=gd-p", ""},
		{http.MethodPost, "/v1/windows", "", `{"expires_in_days":90,"land":"gardenio"}`},
		{http.MethodGet, "/v1/windows/preview", "expires_in_days=7&land=gardenio", ""},
	}
	if len(got) != len(want) {
		t.Fatalf("calls = %+v", got)
	}
	for i := range want {
		g := got[i]
		if g.body != "" {
			var a, b any
			_ = json.Unmarshal([]byte(g.body), &a)
			_ = json.Unmarshal([]byte(want[i].body), &b)
			ab, _ := json.Marshal(a)
			bb, _ := json.Marshal(b)
			g.body, want[i].body = string(ab), string(bb)
		}
		if g != want[i] {
			t.Errorf("call %d = %+v, want %+v", i, g, want[i])
		}
	}
}

func TestWindows_ErrorCarriesStatus(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusForbidden)
		_, _ = w.Write([]byte(`{"error":"only the owner of land gardenio or an operator, on their own sign-in, may manage its windows"}`))
	}))
	defer srv.Close()
	_, err := newTestClient(srv).CreateWindow(context.Background(), "gd-p", 0)
	var apiErr *APIError
	if !errors.As(err, &apiErr) || apiErr.StatusCode != http.StatusForbidden {
		t.Fatalf("err = %v, want an APIError 403", err)
	}
}
