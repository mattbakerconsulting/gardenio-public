package main

import (
	"html/template"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"gardenio/controller/internal/ratelimit"
)

// htmxTestEmail is the operator email tests claim via the X-Auth-Request-Email
// header — same convention as the rest of the htmx test suite.
const htmxTestEmail = "operator@example.com"

func htmxRequest(method, path, token string) *http.Request {
	req := httptest.NewRequest(method, path, nil)
	req.Header.Set("X-Auth-Request-Email", htmxTestEmail)
	if token != "" {
		req.Header.Set("X-Widget-Token", token)
	}
	return req
}

// mkServerWithFreshLimiters builds a server + mux and returns the Server so
// the test can mint tokens and drive the limiter clock deterministically.
func mkServerWithFreshLimiters(t *testing.T) (http.Handler, *Server) {
	t.Helper()
	srv := NewServer(newFake(), slog.Default(), "", "", "", nil)
	srv.htmx.pollLimiter = ratelimit.New(1, 1.0/float64(htmxPollMinIntervalSecs))
	srv.htmx.writeLimiter = ratelimit.New(htmxWriteBurst, htmxWriteRatePerSec)
	mux := http.NewServeMux()
	srv.RegisterRoutes(mux)
	return edgeAuth(mux), srv
}

func TestHtmxToken_RoundTrip(t *testing.T) {
	secret := []byte("supersecretkeydontleak")
	exp := time.Now().Add(time.Hour)
	tok := signHtmxToken(secret, "noop", "inst-1", htmxTestEmail, exp)
	if tok == "" {
		t.Fatal("empty token")
	}
	if err := verifyHtmxToken(secret, tok, "noop", "inst-1", htmxTestEmail, time.Now()); err != nil {
		t.Fatalf("verify: %v", err)
	}
}

func TestHtmxToken_RejectsTamper(t *testing.T) {
	secret := []byte("k1")
	tok := signHtmxToken(secret, "noop", "inst-1", htmxTestEmail, time.Now().Add(time.Hour))

	cases := map[string]struct {
		widget, instance, email string
	}{
		"wrong widget":   {"other", "inst-1", htmxTestEmail},
		"wrong instance": {"noop", "inst-2", htmxTestEmail},
		"wrong email":    {"noop", "inst-1", "evil@example.com"},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			if err := verifyHtmxToken(secret, tok, c.widget, c.instance, c.email, time.Now()); err == nil {
				t.Fatalf("expected error for %s", name)
			}
		})
	}
}

func TestHtmxToken_RejectsExpired(t *testing.T) {
	secret := []byte("k1")
	tok := signHtmxToken(secret, "noop", "inst-1", htmxTestEmail, time.Unix(1000, 0))
	if err := verifyHtmxToken(secret, tok, "noop", "inst-1", htmxTestEmail, time.Unix(2000, 0)); err == nil {
		t.Fatal("expected expired error")
	}
}

func TestHtmxToken_RejectsWrongSecret(t *testing.T) {
	tok := signHtmxToken([]byte("a"), "noop", "inst-1", htmxTestEmail, time.Now().Add(time.Hour))
	if err := verifyHtmxToken([]byte("b"), tok, "noop", "inst-1", htmxTestEmail, time.Now()); err == nil {
		t.Fatal("expected bad signature error")
	}
}

func TestHtmxToken_RejectsMalformed(t *testing.T) {
	cases := []string{"", "not-base64!!", "YWJj"}
	for _, tok := range cases {
		if err := verifyHtmxToken([]byte("k"), tok, "noop", "inst-1", htmxTestEmail, time.Now()); err == nil {
			t.Errorf("expected malformed error for %q", tok)
		}
	}
}

func TestHtmxToken_RejectsBadPrincipal(t *testing.T) {
	secret := []byte("k")
	tok := signHtmxToken(secret, "no", "inst", htmxTestEmail, time.Now().Add(time.Hour))
	if err := verifyHtmxToken(secret, tok, "no|op", "inst", htmxTestEmail, time.Now()); err == nil {
		t.Fatal("expected invalid principal error for widget with pipe")
	}
}

func TestHandleHtmxWidget_GET_HappyPath(t *testing.T) {
	mux, srv := mkServerWithFreshLimiters(t)
	tok := srv.MintHtmxWidgetToken("noop", "i1", htmxTestEmail)
	req := htmxRequest("GET", "/htmx/widget/noop/i1/render", tok)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("status=%d body=%q", w.Code, w.Body.String())
	}
	body := w.Body.String()
	if !strings.Contains(body, `id="htmx-widget-i1"`) {
		t.Fatalf("body missing widget id: %q", body)
	}
	if !strings.Contains(body, `data-state="rendered"`) {
		t.Fatalf("body missing rendered state: %q", body)
	}
	if got := w.Header().Get("Cache-Control"); got != "no-store" {
		t.Errorf("Cache-Control=%q want no-store", got)
	}
}

func TestHandleHtmxWidget_POST_OOBSwap(t *testing.T) {
	mux, srv := mkServerWithFreshLimiters(t)
	tok := srv.MintHtmxWidgetToken("noop", "i1", htmxTestEmail)
	req := httptest.NewRequest("POST", "/htmx/widget/noop/i1/poke", strings.NewReader(""))
	req.Header.Set("X-Auth-Request-Email", htmxTestEmail)
	req.Header.Set("X-Widget-Token", tok)
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("status=%d body=%q", w.Code, w.Body.String())
	}
	body := w.Body.String()
	if !strings.Contains(body, `hx-swap-oob="true"`) {
		t.Fatalf("body missing OOB swap marker: %q", body)
	}
	if !strings.Contains(body, `id="htmx-status-i1"`) {
		t.Fatalf("body missing OOB status target: %q", body)
	}
	if !strings.Contains(body, `data-state="poked"`) {
		t.Fatalf("body missing poked state: %q", body)
	}
	trig := w.Header().Get("HX-Trigger")
	if !strings.Contains(trig, "widget:poked") {
		t.Errorf("HX-Trigger=%q missing widget:poked", trig)
	}
}

func TestHandleHtmxWidget_RejectsMissingToken(t *testing.T) {
	mux, _ := mkServerWithFreshLimiters(t)
	req := htmxRequest("GET", "/htmx/widget/noop/i1/render", "")
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("status=%d want 401", w.Code)
	}
}

func TestHandleHtmxWidget_RejectsBadToken(t *testing.T) {
	mux, _ := mkServerWithFreshLimiters(t)
	req := htmxRequest("GET", "/htmx/widget/noop/i1/render", "garbage")
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("status=%d want 401 body=%q", w.Code, w.Body.String())
	}
}

func TestHandleHtmxWidget_RejectsCrossInstanceToken(t *testing.T) {
	mux, srv := mkServerWithFreshLimiters(t)
	tok := srv.MintHtmxWidgetToken("noop", "instA", htmxTestEmail)
	req := htmxRequest("GET", "/htmx/widget/noop/instB/render", tok)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("status=%d want 401", w.Code)
	}
}

func TestHandleHtmxWidget_TokenViaQueryAlsoWorks(t *testing.T) {
	mux, srv := mkServerWithFreshLimiters(t)
	tok := srv.MintHtmxWidgetToken("noop", "i1", htmxTestEmail)
	q := url.Values{"t": []string{tok}}
	req := httptest.NewRequest("GET", "/htmx/widget/noop/i1/render?"+q.Encode(), nil)
	req.Header.Set("X-Auth-Request-Email", htmxTestEmail)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("status=%d body=%q", w.Code, w.Body.String())
	}
}

func TestHandleHtmxWidget_UnknownAction(t *testing.T) {
	mux, srv := mkServerWithFreshLimiters(t)
	tok := srv.MintHtmxWidgetToken("noop", "i1", htmxTestEmail)
	req := htmxRequest("GET", "/htmx/widget/noop/i1/explode", tok)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)
	if w.Code != http.StatusNotFound {
		t.Fatalf("status=%d want 404", w.Code)
	}
}

func TestHandleHtmxWidget_UnknownWidget(t *testing.T) {
	mux, srv := mkServerWithFreshLimiters(t)
	tok := srv.MintHtmxWidgetToken("ghost", "i1", htmxTestEmail)
	req := htmxRequest("GET", "/htmx/widget/ghost/i1/render", tok)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)
	if w.Code != http.StatusNotFound {
		t.Fatalf("status=%d want 404", w.Code)
	}
}

func TestHandleHtmxWidget_MethodMismatch(t *testing.T) {
	// `poke` is POST-only; hitting it with GET routes to the GET handler
	// (since both methods share the route prefix), looks up `poke`, and
	// returns 405.
	mux, srv := mkServerWithFreshLimiters(t)
	tok := srv.MintHtmxWidgetToken("noop", "i1", htmxTestEmail)
	req := htmxRequest("GET", "/htmx/widget/noop/i1/poke", tok)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)
	if w.Code != http.StatusMethodNotAllowed {
		t.Fatalf("status=%d want 405 body=%q", w.Code, w.Body.String())
	}
	if w.Header().Get("Allow") != "POST" {
		t.Errorf("Allow=%q want POST", w.Header().Get("Allow"))
	}
}

func TestHandleHtmxWidget_PollRateLimit(t *testing.T) {
	mux, srv := mkServerWithFreshLimiters(t)
	tok := srv.MintHtmxWidgetToken("noop", "i1", htmxTestEmail)

	req := htmxRequest("GET", "/htmx/widget/noop/i1/render", tok)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("first GET status=%d body=%q", w.Code, w.Body.String())
	}

	req = htmxRequest("GET", "/htmx/widget/noop/i1/render", tok)
	w = httptest.NewRecorder()
	mux.ServeHTTP(w, req)
	if w.Code != http.StatusTooManyRequests {
		t.Fatalf("second GET status=%d want 429 body=%q", w.Code, w.Body.String())
	}
	if w.Header().Get("Retry-After") == "" {
		t.Error("missing Retry-After header on 429")
	}
}

func TestHandleHtmxWidget_PollRateLimitRefills(t *testing.T) {
	mux, srv := mkServerWithFreshLimiters(t)
	tok := srv.MintHtmxWidgetToken("noop", "i1", htmxTestEmail)

	now := time.Unix(1_000_000, 0)
	srv.htmx.pollLimiter.SetClock(func() time.Time { return now })

	req := htmxRequest("GET", "/htmx/widget/noop/i1/render", tok)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("first GET status=%d", w.Code)
	}

	now = now.Add(4 * time.Second)
	req = htmxRequest("GET", "/htmx/widget/noop/i1/render", tok)
	w = httptest.NewRecorder()
	mux.ServeHTTP(w, req)
	if w.Code != http.StatusTooManyRequests {
		t.Fatalf("4s later: status=%d want 429", w.Code)
	}

	now = now.Add(1 * time.Second)
	req = htmxRequest("GET", "/htmx/widget/noop/i1/render", tok)
	w = httptest.NewRecorder()
	mux.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("5s later: status=%d want 200", w.Code)
	}
}

func TestHandleHtmxWidget_RateLimitPerInstance(t *testing.T) {
	mux, srv := mkServerWithFreshLimiters(t)
	tokA := srv.MintHtmxWidgetToken("noop", "instA", htmxTestEmail)
	tokB := srv.MintHtmxWidgetToken("noop", "instB", htmxTestEmail)

	for _, p := range []struct {
		path, tok string
	}{
		{"/htmx/widget/noop/instA/render", tokA},
		{"/htmx/widget/noop/instB/render", tokB},
	} {
		req := htmxRequest("GET", p.path, p.tok)
		w := httptest.NewRecorder()
		mux.ServeHTTP(w, req)
		if w.Code != http.StatusOK {
			t.Fatalf("%s status=%d want 200", p.path, w.Code)
		}
	}
}

func TestHandleHtmxWidget_MissingPathParts(t *testing.T) {
	// http.ServeMux normalizes `//` segments via 301 redirect — accept
	// either 404 (preferred) or 301 (mux-normalized). Both are non-200,
	// which is the contract the test enforces: empty path segments must
	// not authorize any widget.
	mux, _ := mkServerWithFreshLimiters(t)
	req := httptest.NewRequest("GET", "/htmx/widget//i1/render", nil)
	req.Header.Set("X-Auth-Request-Email", htmxTestEmail)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)
	if w.Code == http.StatusOK {
		t.Fatalf("empty widget segment should not 200; got %d", w.Code)
	}
}

func TestHandleHtmxWidget_MissingEmail_401(t *testing.T) {
	mux, srv := mkServerWithFreshLimiters(t)
	tok := srv.MintHtmxWidgetToken("noop", "i1", htmxTestEmail)
	req := httptest.NewRequest("GET", "/htmx/widget/noop/i1/render", nil)
	req.Header.Set("X-Widget-Token", tok)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("status=%d want 401", w.Code)
	}
}

func TestExtractHtmxToken_PrefersHeader(t *testing.T) {
	req := httptest.NewRequest("GET", "/x?t=fromquery", nil)
	req.Header.Set("X-Widget-Token", "fromheader")
	if got := extractHtmxToken(req); got != "fromheader" {
		t.Fatalf("got %q want fromheader", got)
	}
}

func TestEscapeAttr(t *testing.T) {
	in := `<a href="x">&'`
	out := escapeAttr(in)
	if strings.Contains(out, "<") || strings.Contains(out, `"`) || strings.Contains(out, "'") {
		t.Fatalf("unescaped chars: %q", out)
	}
}

func TestRegisterHtmxWidget_DuplicatePanics(t *testing.T) {
	defer func() {
		if r := recover(); r == nil {
			t.Fatal("expected panic on duplicate registration")
		}
	}()
	registerHtmxWidget("noop", "render", htmxAction{
		Method: http.MethodGet,
		Handler: func(http.ResponseWriter, *http.Request, *htmxActionCtx) (template.HTML, error) {
			return "", nil
		},
	})
}

func TestRegisterHtmxWidget_NilHandlerPanics(t *testing.T) {
	defer func() {
		if r := recover(); r == nil {
			t.Fatal("expected panic on nil handler registration")
		}
	}()
	registerHtmxWidget("brand-new-widget-for-nil-test", "x", htmxAction{Method: http.MethodGet})
}
