package vlsink

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestHTTPSink_PostsBatchAndSucceedsOn2xx(t *testing.T) {
	var gotBody string
	var gotCT string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		gotBody = string(b)
		gotCT = r.Header.Get("Content-Type")
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	sink := &HTTPSink{Endpoint: srv.URL, Client: srv.Client()}
	batch := []byte(`{"client_ip":"203.0.113.7"}` + "\n")
	if err := sink.Insert(context.Background(), batch); err != nil {
		t.Fatalf("Insert: %v", err)
	}
	if gotBody != string(batch) {
		t.Errorf("server received %q, want %q", gotBody, batch)
	}
	if gotCT != "application/x-ndjson" {
		t.Errorf("Content-Type = %q, want application/x-ndjson", gotCT)
	}
}

func TestHTTPSink_ErrorsOnNon2xx(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "boom", http.StatusInternalServerError)
	}))
	defer srv.Close()

	sink := &HTTPSink{Endpoint: srv.URL, Client: srv.Client()}
	err := sink.Insert(context.Background(), []byte("{}\n"))
	if err == nil {
		t.Fatal("expected an error on a non-2xx VictoriaLogs response (drives at-least-once re-pull)")
	}
}

func TestHTTPSink_SendsConfiguredHeaders(t *testing.T) {
	var gotAcct, gotProj string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAcct, gotProj = r.Header.Get("AccountID"), r.Header.Get("ProjectID")
	}))
	defer srv.Close()

	sink := &HTTPSink{Endpoint: srv.URL, Client: srv.Client(),
		Headers: map[string]string{"AccountID": "9", "ProjectID": "0"}}
	if err := sink.Insert(context.Background(), []byte("{}\n")); err != nil {
		t.Fatalf("Insert: %v", err)
	}
	if gotAcct != "9" || gotProj != "0" {
		t.Errorf("tenant headers = %q/%q, want 9/0", gotAcct, gotProj)
	}
}
