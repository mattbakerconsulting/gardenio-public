package main

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"

	"gardenio/controller/internal/config"
)

// newServedModelTestServer returns an httptest server that serves the given
// /v1/models ids and (optionally) a model_group_alias map on
// /get/config/callbacks. A models value of nil makes /v1/models return a 500 so
// tests can exercise the fetch-failure path.
func newServedModelTestServer(t *testing.T, models []string, aliases map[string]string) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v1/models":
			if models == nil {
				http.Error(w, "boom", http.StatusInternalServerError)
				return
			}
			w.Header().Set("Content-Type", "application/json")
			io.WriteString(w, `{"data":[`)
			for i, m := range models {
				if i > 0 {
					io.WriteString(w, ",")
				}
				io.WriteString(w, `{"id":"`+m+`"}`)
			}
			io.WriteString(w, `]}`)
		case "/get/config/callbacks":
			w.Header().Set("Content-Type", "application/json")
			io.WriteString(w, `{"router_settings":{"model_group_alias":{`)
			first := true
			for k, v := range aliases {
				if !first {
					io.WriteString(w, ",")
				}
				first = false
				io.WriteString(w, `"`+k+`":"`+v+`"`)
			}
			io.WriteString(w, `}}}`)
		default:
			http.NotFound(w, r)
		}
	}))
}

// TestServedModelSync_Refresh_PublishesUnion verifies a successful refresh
// publishes the union of /v1/models ids and model_group_alias keys.
func TestServedModelSync_Refresh_PublishesUnion(t *testing.T) {
	srv := newServedModelTestServer(t,
		[]string{"claude-opus-4-8-oauth", "anthropic/claude-haiku-4-5"},
		map[string]string{"glm-5": "bedrock-glm-5"},
	)
	defer srv.Close()

	cfg := &config.Config{}
	s := newServedModelSync(cfg, srv.URL, "sk-master", slog.New(slog.NewTextHandler(io.Discard, nil)))
	s.refresh(context.Background())

	got := cfg.SnapshotServedModels()
	for _, want := range []string{"claude-opus-4-8-oauth", "anthropic/claude-haiku-4-5", "glm-5"} {
		if !got[want] {
			t.Errorf("expected served set to contain %q; got %v", want, got)
		}
	}
	// bedrock-glm-5 is the alias TARGET, not a servable name here — must NOT leak
	// into the served set (only /v1/models ids and alias KEYS are served names).
	if got["bedrock-glm-5"] {
		t.Errorf("alias target bedrock-glm-5 must not be in the served set; got %v", got)
	}
}

// TestServedModelSync_Refresh_FailOpenOnError verifies that a fetch failure does
// NOT clobber a previously published set — the fail-open contract that keeps a
// transient LiteLLM blip from stripping every account from the spread pool.
func TestServedModelSync_Refresh_FailOpenOnError(t *testing.T) {
	// Prime a good set.
	good := newServedModelTestServer(t, []string{"claude-opus-4-8-oauth"}, nil)
	cfg := &config.Config{}
	s := newServedModelSync(cfg, good.URL, "sk", slog.New(slog.NewTextHandler(io.Discard, nil)))
	s.refresh(context.Background())
	good.Close()
	if !cfg.SnapshotServedModels()["claude-opus-4-8-oauth"] {
		t.Fatalf("precondition: expected primed served set")
	}

	// Now point at a failing server and refresh — the primed set must survive.
	bad := newServedModelTestServer(t, nil, nil)
	defer bad.Close()
	s.litellmURL = bad.URL
	s.refresh(context.Background())
	if !cfg.SnapshotServedModels()["claude-opus-4-8-oauth"] {
		t.Errorf("fetch failure must not clobber the previous served set; got %v", cfg.SnapshotServedModels())
	}
}

// TestServedModelSync_Refresh_DoesNotPublishEmpty verifies a reachable LiteLLM
// that returns zero models does not collapse the set to known-empty (which would
// gate every account). The set stays UNKNOWN (nil).
func TestServedModelSync_Refresh_DoesNotPublishEmpty(t *testing.T) {
	srv := newServedModelTestServer(t, []string{}, nil)
	defer srv.Close()

	cfg := &config.Config{}
	s := newServedModelSync(cfg, srv.URL, "sk", slog.New(slog.NewTextHandler(io.Discard, nil)))
	s.refresh(context.Background())

	if got := cfg.SnapshotServedModels(); got != nil {
		t.Errorf("empty /v1/models must leave the set UNKNOWN (nil); got %v", got)
	}
}
