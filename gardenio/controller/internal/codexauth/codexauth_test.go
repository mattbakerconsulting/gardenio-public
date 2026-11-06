package codexauth

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func jwtWithExp(exp int64) string {
	enc := func(s string) string { return base64.RawURLEncoding.EncodeToString([]byte(s)) }
	return enc(`{"alg":"none"}`) + "." + enc(fmt.Sprintf(`{"exp":%d}`, exp)) + ".sig"
}

func authDoc(access, refresh string, lastRefresh time.Time) []byte {
	b, _ := json.Marshal(map[string]any{
		"auth_mode":      "chatgpt",
		"OPENAI_API_KEY": nil,
		"tokens": map[string]any{
			"id_token": "id", "access_token": access, "refresh_token": refresh, "account_id": "acct-1",
		},
		"last_refresh": lastRefresh.UTC().Format(time.RFC3339Nano),
	})
	return b
}

func TestParseRejectsBadDocuments(t *testing.T) {
	for name, doc := range map[string]string{
		"not json":      `nope`,
		"api key mode":  `{"auth_mode":"apikey","tokens":{"access_token":"a","refresh_token":"r"}}`,
		"no tokens":     `{"auth_mode":"chatgpt"}`,
		"no access":     `{"tokens":{"refresh_token":"r"}}`,
		"no refresh":    `{"tokens":{"access_token":"a"}}`,
		"tokens string": `{"tokens":"x"}`,
	} {
		if _, err := Parse([]byte(doc)); err == nil {
			t.Errorf("%s: expected error", name)
		}
	}
}

func TestRotatedKeepsUnknownFieldsAndSessionHidesRefreshToken(t *testing.T) {
	now := time.Now()
	a, err := Parse(authDoc("old-access", "old-refresh", now.Add(-time.Hour)))
	if err != nil {
		t.Fatal(err)
	}
	n, err := a.Rotated(Tokens{AccessToken: "new-access", RefreshToken: "new-refresh"}, now)
	if err != nil {
		t.Fatal(err)
	}
	if n.AccessToken() != "new-access" || n.RefreshToken() != "new-refresh" || n.IDToken() != "id" || n.AccountID() != "acct-1" {
		t.Fatalf("rotation wrong: %s", mustMarshal(t, n))
	}
	if a.AccessToken() != "old-access" {
		t.Fatal("Rotated mutated the receiver")
	}
	if !strings.Contains(mustMarshal(t, n), "OPENAI_API_KEY") {
		t.Fatal("unknown top-level field dropped")
	}
	if got := n.LastRefresh(); now.Sub(got) > time.Second || got.After(now.Add(time.Second)) {
		t.Fatalf("last_refresh not stamped: %v", got)
	}

	s := mustMarshal(t, n.SessionDocument())
	if strings.Contains(s, "new-refresh") || !strings.Contains(s, PlaceholderRefreshToken) || !strings.Contains(s, "new-access") {
		t.Fatalf("session doc wrong: %s", s)
	}
	// Deterministic: equal documents marshal to identical bytes.
	if mustMarshal(t, n.SessionDocument()) != s {
		t.Fatal("Marshal is not deterministic")
	}
}

func mustMarshal(t *testing.T, a *Auth) string {
	t.Helper()
	b, err := a.Marshal()
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func TestNeedsRefresh(t *testing.T) {
	now := time.Now()
	win, age := 24*time.Hour, 6*24*time.Hour
	cases := []struct {
		name   string
		access string
		last   time.Time
		want   bool
	}{
		{"fresh", jwtWithExp(now.Add(48 * time.Hour).Unix()), now.Add(-time.Hour), false},
		{"expires inside window", jwtWithExp(now.Add(2 * time.Hour).Unix()), now.Add(-time.Hour), true},
		{"already expired", jwtWithExp(now.Add(-time.Hour).Unix()), now.Add(-time.Hour), true},
		{"last_refresh too old", jwtWithExp(now.Add(48 * time.Hour).Unix()), now.Add(-7 * 24 * time.Hour), true},
		{"opaque token refreshes", "not-a-jwt", now.Add(-2 * time.Hour), true},
		{"opaque token inside min interval", "not-a-jwt", now.Add(-10 * time.Minute), false},
		{"expired token inside min interval", jwtWithExp(now.Add(-time.Hour).Unix()), now.Add(-10 * time.Minute), false},
	}
	for _, c := range cases {
		a, err := Parse(authDoc(c.access, "r", c.last))
		if err != nil {
			t.Fatal(err)
		}
		if got := a.NeedsRefresh(now, win, age, time.Hour); got != c.want {
			t.Errorf("%s: got %v want %v", c.name, got, c.want)
		}
	}
}

func TestFingerprintDoesNotRevealToken(t *testing.T) {
	a, _ := Parse(authDoc("a", "super-secret-refresh", time.Now()))
	b, _ := Parse(authDoc("a", "other-refresh", time.Now()))
	if strings.Contains(a.RefreshFingerprint(), "secret") || a.RefreshFingerprint() == b.RefreshFingerprint() || len(a.RefreshFingerprint()) != 16 {
		t.Fatalf("bad fingerprint %q", a.RefreshFingerprint())
	}
}

func TestRefreshSuccessAndRequestShape(t *testing.T) {
	var got map[string]string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.Header.Get("Content-Type") != "application/json" {
			t.Errorf("method/content-type: %s %s", r.Method, r.Header.Get("Content-Type"))
		}
		_ = json.NewDecoder(r.Body).Decode(&got)
		_, _ = w.Write([]byte(`{"access_token":"A2","refresh_token":"R2","id_token":"I2"}`))
	}))
	defer srv.Close()
	tr, err := NewRefresher(srv.URL).Refresh(context.Background(), "R1")
	if err != nil {
		t.Fatal(err)
	}
	if tr.AccessToken != "A2" || tr.RefreshToken != "R2" || tr.IDToken != "I2" {
		t.Fatalf("tokens: %+v", tr)
	}
	if got["client_id"] != ClientID || got["grant_type"] != "refresh_token" || got["refresh_token"] != "R1" {
		t.Fatalf("request body: %v", got)
	}
}

func TestRefreshErrorClassification(t *testing.T) {
	cases := []struct {
		status    int
		body      string
		permanent bool
		code      string
	}{
		{401, `{"error":{"code":"refresh_token_reused","message":"secret-ish text"}}`, true, "refresh_token_reused"},
		{400, `{"error":"invalid_grant"}`, true, "invalid_grant"},
		{403, `{}`, true, ""},
		{401, `{"error":"has spaces and token abc"}`, true, ""},
		{429, `{}`, false, ""},
		{408, `{}`, false, ""},
		{500, `{}`, false, ""},
		{503, `oops`, false, ""},
	}
	for _, c := range cases {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(c.status)
			_, _ = w.Write([]byte(c.body))
		}))
		_, err := NewRefresher(srv.URL).Refresh(context.Background(), "R1")
		srv.Close()
		if err == nil || IsPermanent(err) != c.permanent {
			t.Errorf("status %d: err=%v permanent=%v want %v", c.status, err, IsPermanent(err), c.permanent)
			continue
		}
		if re := err.(*RefreshError); re.Code != c.code {
			t.Errorf("status %d: code %q want %q", c.status, re.Code, c.code)
		}
		if strings.Contains(err.Error(), "secret-ish") || strings.Contains(err.Error(), "abc") {
			t.Errorf("status %d: error leaked body: %v", c.status, err)
		}
	}
}

func TestRefreshUnreadable200IsPermanent(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { _, _ = w.Write([]byte(`<html>`)) }))
	defer srv.Close()
	if _, err := NewRefresher(srv.URL).Refresh(context.Background(), "R"); !IsPermanent(err) {
		t.Fatalf("want permanent, got %v", err)
	}
}

func TestRefreshNetworkErrorIsTransient(t *testing.T) {
	srv := httptest.NewServer(http.NotFoundHandler())
	url := srv.URL
	srv.Close()
	if _, err := NewRefresher(url).Refresh(context.Background(), "R"); err == nil || IsPermanent(err) {
		t.Fatalf("want transient error, got %v", err)
	}
}

func TestUsageRead(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer tok" || r.Header.Get("ChatGPT-Account-Id") != "acct-1" {
			t.Errorf("headers: %v", r.Header)
		}
		_, _ = w.Write([]byte(`{"rate_limit":{"limit_reached":false,
			"primary_window":{"used_percent":42.5,"limit_window_seconds":18000,"reset_after_seconds":600},
			"secondary_window":{"used_percent":80,"limit_window_seconds":604800,"reset_at":1800086400}}}`))
	}))
	defer srv.Close()
	r := NewUsageReader(srv.URL)
	r.Now = func() time.Time { return now }
	u, err := r.Read(context.Background(), "tok", "acct-1")
	if err != nil {
		t.Fatal(err)
	}
	if u.Primary.UsedFraction != 0.425 || !u.Primary.ResetAt.Equal(now.Add(600*time.Second)) {
		t.Fatalf("primary: %+v", u.Primary)
	}
	if u.Secondary.UsedFraction != 0.8 || u.Secondary.ResetAt.Unix() != 1800086400 || u.LimitReached {
		t.Fatalf("secondary: %+v limit=%v", u.Secondary, u.LimitReached)
	}
}

func TestUsageReadFailures(t *testing.T) {
	for name, h := range map[string]http.HandlerFunc{
		"401":       func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(401) },
		"reshaped":  func(w http.ResponseWriter, r *http.Request) { _, _ = w.Write([]byte(`{"something":"else"}`)) },
		"no window": func(w http.ResponseWriter, r *http.Request) { _, _ = w.Write([]byte(`{"rate_limit":{}}`)) },
		"garbage":   func(w http.ResponseWriter, r *http.Request) { _, _ = w.Write([]byte(`<`)) },
	} {
		srv := httptest.NewServer(h)
		if _, err := NewUsageReader(srv.URL).Read(context.Background(), "t", ""); err == nil {
			t.Errorf("%s: expected error", name)
		}
		srv.Close()
	}
}

func TestUsageLimitReachedViaAllowedFalse(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"rate_limit":{"allowed":false,"primary_window":{"used_percent":100}}}`))
	}))
	defer srv.Close()
	u, err := NewUsageReader(srv.URL).Read(context.Background(), "t", "")
	if err != nil || !u.LimitReached {
		t.Fatalf("u=%+v err=%v", u, err)
	}
}

func TestSessionDocumentIsAWhitelist(t *testing.T) {
	doc := `{"auth_mode":"chatgpt","OPENAI_API_KEY":"sk-secret","last_refresh":"2026-10-15T00:00:00Z","agent_identity":{"x":1},
"tokens":{"access_token":"acc","refresh_token":"real-refresh","id_token":"idt","account_id":"a1","extra":"nope"}}`
	a, err := Parse([]byte(doc))
	if err != nil {
		t.Fatal(err)
	}
	s := mustMarshal(t, a.SessionDocument())
	for _, bad := range []string{"sk-secret", "OPENAI_API_KEY", "agent_identity", "real-refresh", "nope", "extra"} {
		if strings.Contains(s, bad) {
			t.Errorf("session doc leaked %q: %s", bad, s)
		}
	}
	var got map[string]any
	if err := json.Unmarshal([]byte(s), &got); err != nil {
		t.Fatal(err)
	}
	if len(got) != 3 || got["auth_mode"] != "chatgpt" || got["last_refresh"] != "2026-10-15T00:00:00Z" {
		t.Fatalf("top-level keys: %v", got)
	}
	tok := got["tokens"].(map[string]any)
	if len(tok) != 4 || tok["access_token"] != "acc" || tok["id_token"] != "idt" || tok["account_id"] != "a1" ||
		tok["refresh_token"] != PlaceholderRefreshToken {
		t.Fatalf("tokens: %v", tok)
	}
	// The controller-side document keeps unknown fields.
	if !strings.Contains(mustMarshal(t, a), "OPENAI_API_KEY") {
		t.Fatal("controller-side document must keep unknown fields")
	}
}

func TestSafeOverrideURL(t *testing.T) {
	cases := []struct {
		raw string
		ok  bool
	}{
		{"https://auth.openai.com/oauth/token", true},
		{"https://AUTH.openai.com:443/oauth/token", true},
		{"http://auth.openai.com/oauth/token", false},
		{"https://evil.example.com/oauth/token", false},
		{"https://auth.openai.com.evil.example.com/oauth/token", false},
		{"https://evil.example.com@auth.openai.com/oauth/token", false},
		{"https://auth.openai.com@evil.example.com/oauth/token", false},
		{"https://auth.openai.com:8443/oauth/token", false},
		{"http://127.0.0.1:9999/token", false},
		{"", false},
		{"not a url", false},
	}
	for _, c := range cases {
		if _, ok := SafeOverrideURL(c.raw, TokenURLAllowedHosts); ok != c.ok {
			t.Errorf("%q: got %v want %v", c.raw, ok, c.ok)
		}
	}
}
