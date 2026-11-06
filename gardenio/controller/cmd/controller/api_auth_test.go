package main

import (
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"
)

// okHandler is a trivial protected handler that records whether it ran.
func okHandler(ran *bool) http.HandlerFunc {
	return func(w http.ResponseWriter, _ *http.Request) {
		*ran = true
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, "ok")
	}
}

func TestControllerAPIAuth_DisabledWhenTokenUnset(t *testing.T) {
	t.Setenv(controllerAPITokenEnv, "")
	auth := newControllerAPIAuth(slog.New(slog.NewTextHandler(io.Discard, nil)))

	var ran bool
	h := auth(okHandler(&ran))

	// No Authorization header — must pass through unchanged (opt-in default).
	req := httptest.NewRequest(http.MethodPost, "/api/v1/pool/assign", nil)
	rec := httptest.NewRecorder()
	h(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("token unset: want 200 pass-through, got %d", rec.Code)
	}
	if !ran {
		t.Fatal("token unset: protected handler was not invoked")
	}
}

func TestControllerAPIAuth_EnforcesWhenTokenSet(t *testing.T) {
	const token = "s3cr3t-token"
	t.Setenv(controllerAPITokenEnv, token)
	auth := newControllerAPIAuth(slog.New(slog.NewTextHandler(io.Discard, nil)))

	cases := []struct {
		name       string
		header     string
		wantStatus int
		wantRan    bool
	}{
		{"valid bearer", "Bearer " + token, http.StatusOK, true},
		{"wrong token", "Bearer nope", http.StatusUnauthorized, false},
		{"missing bearer prefix", token, http.StatusUnauthorized, false},
		{"empty header", "", http.StatusUnauthorized, false},
		{"basic auth scheme", "Basic " + token, http.StatusUnauthorized, false},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var ran bool
			h := auth(okHandler(&ran))
			req := httptest.NewRequest(http.MethodPost, "/api/v1/pool/assign", nil)
			if tc.header != "" {
				req.Header.Set("Authorization", tc.header)
			}
			rec := httptest.NewRecorder()
			h(rec, req)

			if rec.Code != tc.wantStatus {
				t.Errorf("status: got %d want %d", rec.Code, tc.wantStatus)
			}
			if ran != tc.wantRan {
				t.Errorf("handler ran: got %v want %v", ran, tc.wantRan)
			}
		})
	}
}

// TestControllerAPIAuth_TrimsWhitespace guards that a token with surrounding
// whitespace in the env (a common secret-file artifact) is trimmed on the
// controller side, so it matches a clean bearer value from the bridge.
func TestControllerAPIAuth_TrimsWhitespace(t *testing.T) {
	t.Setenv(controllerAPITokenEnv, "  padded-token\n")
	auth := newControllerAPIAuth(slog.New(slog.NewTextHandler(io.Discard, nil)))

	var ran bool
	h := auth(okHandler(&ran))
	req := httptest.NewRequest(http.MethodPost, "/api/v1/pool/assign", nil)
	req.Header.Set("Authorization", "Bearer padded-token")
	rec := httptest.NewRecorder()
	h(rec, req)

	if rec.Code != http.StatusOK || !ran {
		t.Fatalf("trimmed token should authenticate: status=%d ran=%v", rec.Code, ran)
	}
}
