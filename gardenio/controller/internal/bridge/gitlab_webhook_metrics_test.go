package bridge

import (
	"bytes"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/prometheus/client_golang/prometheus/testutil"
)

// TestGitLabWebhookDeliveries_TokenFailed proves that a delivery rejected at the
// perimeter (missing / wrong X-Gitlab-Token) still increments the receive-path
// counter under outcome=token_failed rather than vanishing into silence. This is
// the receive-path liveness signal for the GitLab door of the non-AVA webhook
// carve-out (ka-0000005632): a still-arriving-but-rejected stream (e.g. a hook
// re-secreted with the wrong token in the GitLab console) must be visible as
// token_failed, distinct from a genuinely dark path where nothing arrives.
func TestGitLabWebhookDeliveries_TokenFailed(t *testing.T) {
	gitlabWebhookDeliveries.Reset()

	handler := GitLabWebhookHandlerWithConfig(GitLabWebhookConfig{
		Daemon:        newE2EDaemon(),
		WebhookSecret: "the-real-secret",
		Logger:        slog.Default(),
	})

	// No X-Gitlab-Token header at all → 403, token_failed.
	req := httptest.NewRequest(http.MethodPost, "/webhook", bytes.NewReader([]byte(`{}`)))
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)
	if w.Code != http.StatusForbidden {
		t.Fatalf("missing token: got %d, want 403", w.Code)
	}

	// Wrong token → 403, token_failed.
	req = httptest.NewRequest(http.MethodPost, "/webhook", bytes.NewReader([]byte(`{}`)))
	req.Header.Set("X-Gitlab-Token", "wrong")
	w = httptest.NewRecorder()
	handler.ServeHTTP(w, req)
	if w.Code != http.StatusForbidden {
		t.Fatalf("wrong token: got %d, want 403", w.Code)
	}

	if got := testutil.ToFloat64(gitlabWebhookDeliveries.WithLabelValues(gitlabWebhookOutcomeTokenFailed)); got != 2 {
		t.Errorf("token_failed count = %v, want 2", got)
	}
	// A rejected stream must NOT read as processed liveness.
	if got := testutil.ToFloat64(gitlabWebhookDeliveries.WithLabelValues(gitlabWebhookOutcomeProcessed)); got != 0 {
		t.Errorf("processed count = %v, want 0 (rejected deliveries must not count as received)", got)
	}
}

// TestGitLabWebhookDeliveries_ProcessedAndIgnored proves the two authenticated
// outcomes are accounted separately: an authenticated, well-formed delivery of a
// handled kind records `processed`, while a handled envelope of an unhandled
// object_kind records `ignored`. Both prove the receive path is alive; the
// distinction lets a dashboard separate "doing work" from "authenticated but
// nothing to do."
func TestGitLabWebhookDeliveries_ProcessedAndIgnored(t *testing.T) {
	gitlabWebhookDeliveries.Reset()

	const secret = "secret"
	handler := GitLabWebhookHandlerWithConfig(GitLabWebhookConfig{
		Daemon:        newE2EDaemon(),
		WebhookSecret: secret,
		BotUsername:   "group_1_bot_x",
		Logger:        slog.Default(),
	})

	post := func(payload map[string]any) int {
		body, _ := json.Marshal(payload)
		req := httptest.NewRequest(http.MethodPost, "/webhook", bytes.NewReader(body))
		req.Header.Set("X-Gitlab-Token", secret)
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, req)
		return w.Code
	}

	// A pipeline event is authenticated + well-formed + handled → processed.
	if code := post(map[string]any{
		"object_kind":       "pipeline",
		"user":              map[string]any{"username": "someone"},
		"object_attributes": map[string]any{"id": 1, "status": "success", "url": "https://gitlab.com/o/r/-/pipelines/1"},
	}); code != http.StatusOK {
		t.Fatalf("pipeline event: got %d, want 200", code)
	}

	// An unknown object_kind is authenticated + decoded but unhandled → ignored.
	if code := post(map[string]any{
		"object_kind": "wiki_page",
		"user":        map[string]any{"username": "someone"},
	}); code != http.StatusOK {
		t.Fatalf("unknown kind: got %d, want 200", code)
	}

	if got := testutil.ToFloat64(gitlabWebhookDeliveries.WithLabelValues(gitlabWebhookOutcomeProcessed)); got != 1 {
		t.Errorf("processed count = %v, want 1", got)
	}
	if got := testutil.ToFloat64(gitlabWebhookDeliveries.WithLabelValues(gitlabWebhookOutcomeIgnored)); got != 1 {
		t.Errorf("ignored count = %v, want 1", got)
	}
}

// TestGitLabWebhookDeliveries_BadRequest proves malformed and wrong-method
// deliveries — authenticated at the token gate but unusable — record
// bad_request, again keeping them visible rather than silent.
func TestGitLabWebhookDeliveries_BadRequest(t *testing.T) {
	gitlabWebhookDeliveries.Reset()

	const secret = "secret"
	handler := GitLabWebhookHandlerWithConfig(GitLabWebhookConfig{
		Daemon:        newE2EDaemon(),
		WebhookSecret: secret,
		Logger:        slog.Default(),
	})

	// Wrong method (GET) with a valid token → 405, bad_request.
	req := httptest.NewRequest(http.MethodGet, "/webhook", nil)
	req.Header.Set("X-Gitlab-Token", secret)
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)
	if w.Code != http.StatusMethodNotAllowed {
		t.Fatalf("GET: got %d, want 405", w.Code)
	}

	// Undecodable body with a valid token → 400, bad_request.
	req = httptest.NewRequest(http.MethodPost, "/webhook", bytes.NewReader([]byte(`{not json`)))
	req.Header.Set("X-Gitlab-Token", secret)
	w = httptest.NewRecorder()
	handler.ServeHTTP(w, req)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("bad body: got %d, want 400", w.Code)
	}

	if got := testutil.ToFloat64(gitlabWebhookDeliveries.WithLabelValues(gitlabWebhookOutcomeBadRequest)); got != 2 {
		t.Errorf("bad_request count = %v, want 2", got)
	}
}

// TestInitGitLabWebhookMetrics_ZeroBaseline proves the init seeds every outcome
// series to a zero sample so increase()/absent()/rate() have a baseline the
// instant the pod is first scraped — the same guarantee initWebhookMetrics gives
// the Jira suite.
func TestInitGitLabWebhookMetrics_ZeroBaseline(t *testing.T) {
	gitlabWebhookDeliveries.Reset()
	InitGitLabWebhookMetrics()

	for _, outcome := range []string{
		gitlabWebhookOutcomeProcessed,
		gitlabWebhookOutcomeIgnored,
		gitlabWebhookOutcomeTokenFailed,
		gitlabWebhookOutcomeBadRequest,
		gitlabWebhookOutcomeError,
	} {
		if got := testutil.ToFloat64(gitlabWebhookDeliveries.WithLabelValues(outcome)); got != 0 {
			t.Errorf("outcome %q baseline = %v, want 0", outcome, got)
		}
	}
	if n := testutil.CollectAndCount(gitlabWebhookDeliveries); n != 5 {
		t.Errorf("series count = %d, want 5 (one per outcome)", n)
	}
}
