package httpjson

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestDoSendsJSONAndDecodes(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			t.Errorf("method = %s, want POST", r.Method)
		}
		if got := r.Header.Get("Content-Type"); got != "application/json" {
			t.Errorf("Content-Type = %q", got)
		}
		if got := r.Header.Get("Accept"); got != "application/json" {
			t.Errorf("Accept = %q", got)
		}
		if got := r.Header.Get("Authorization"); got != "Bearer tok" {
			t.Errorf("Authorization = %q", got)
		}
		b, _ := io.ReadAll(r.Body)
		if string(b) != `{"a":1}` {
			t.Errorf("body = %s", b)
		}
		w.Write([]byte(`{"ok":true}`))
	}))
	defer srv.Close()

	var out struct{ OK bool }
	err := Do(context.Background(), srv.Client(), Request{
		Method: http.MethodPost, URL: srv.URL, Header: Bearer("tok"), Body: map[string]int{"a": 1},
	}, &out)
	if err != nil || !out.OK {
		t.Fatalf("Do = %v, out = %+v", err, out)
	}
}

func TestDoNoBodyNoContentType(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("Content-Type"); got != "" {
			t.Errorf("Content-Type = %q, want none", got)
		}
		if got := r.Header.Get("Accept"); got != "application/vnd.github+json" {
			t.Errorf("Accept = %q, want the caller's", got)
		}
		if got := r.Header.Get("Authorization"); got != "" {
			t.Errorf("Authorization = %q, want none", got)
		}
	}))
	defer srv.Close()

	h := http.Header{"Accept": {"application/vnd.github+json"}}
	if b := Bearer(""); b != nil {
		t.Fatalf("Bearer(\"\") = %v, want nil", b)
	}
	if err := Do(context.Background(), srv.Client(), Request{Method: http.MethodGet, URL: srv.URL, Header: h}, nil); err != nil {
		t.Fatal(err)
	}
}

func TestDoOutKinds(t *testing.T) {
	tests := []struct {
		name    string
		status  int
		body    string
		out     func() any
		check   func(t *testing.T, out any)
		wantErr string
	}{
		{
			name: "raw bytes", status: 200, body: `not json`,
			out:   func() any { return new([]byte) },
			check: func(t *testing.T, out any) { assertEq(t, string(*out.(*[]byte)), "not json") },
		},
		{
			name: "204 leaves out untouched", status: 204,
			out:   func() any { s := "keep"; return &s },
			check: func(t *testing.T, out any) { assertEq(t, *out.(*string), "keep") },
		},
		{
			name: "201 decodes", status: 201, body: `"made"`,
			out:   func() any { return new(string) },
			check: func(t *testing.T, out any) { assertEq(t, *out.(*string), "made") },
		},
		{
			name: "bad json", status: 200, body: `{`,
			out:     func() any { return new(map[string]any) },
			wantErr: "decode response",
		},
		{
			name: "empty 200 body is a decode error", status: 200,
			out:     func() any { return new(map[string]any) },
			wantErr: "decode response",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(tc.status)
				w.Write([]byte(tc.body))
			}))
			defer srv.Close()
			out := tc.out()
			err := Do(context.Background(), srv.Client(), Request{Method: http.MethodGet, URL: srv.URL}, out)
			if tc.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
					t.Fatalf("err = %v, want %q", err, tc.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			tc.check(t, out)
		})
	}
}

func TestDoStatusError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Retry-After", "7")
		w.WriteHeader(http.StatusTooManyRequests)
		w.Write([]byte("  slow down\n" + strings.Repeat("x", 2*ErrorBodyLimit)))
	}))
	defer srv.Close()

	err := Do(context.Background(), srv.Client(), Request{Method: http.MethodDelete, URL: srv.URL + "/a"}, nil)
	var se *StatusError
	if !errors.As(err, &se) {
		t.Fatalf("err = %v, want *StatusError", err)
	}
	if se.StatusCode != 429 || se.Method != http.MethodDelete || se.URL != srv.URL+"/a" {
		t.Errorf("StatusError = %+v", se)
	}
	if !strings.HasPrefix(se.Body, "slow down") || len(se.Body) > ErrorBodyLimit {
		t.Errorf("Body = %q (len %d)", se.Body[:20], len(se.Body))
	}
	if se.Header.Get("Retry-After") != "7" {
		t.Errorf("Retry-After = %q", se.Header.Get("Retry-After"))
	}
	if !strings.HasPrefix(err.Error(), "HTTP 429: slow down") {
		t.Errorf("Error() = %q", err.Error()[:30])
	}
}

func TestDoTransportAndMarshalErrors(t *testing.T) {
	err := Do(context.Background(), http.DefaultClient, Request{Method: http.MethodPost, URL: "http://x", Body: make(chan int)}, nil)
	if err == nil || !strings.Contains(err.Error(), "marshal body") {
		t.Errorf("marshal err = %v", err)
	}
	err = Do(context.Background(), http.DefaultClient, Request{Method: "BAD METHOD", URL: "http://x"}, nil)
	if err == nil || !strings.Contains(err.Error(), "build request") {
		t.Errorf("build err = %v", err)
	}
	srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	url := srv.URL
	srv.Close()
	err = Do(context.Background(), http.DefaultClient, Request{Method: http.MethodGet, URL: url}, nil)
	if err == nil || !strings.Contains(err.Error(), "do request") {
		t.Errorf("transport err = %v", err)
	}
}

func assertEq(t *testing.T, got, want string) {
	t.Helper()
	if got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}
