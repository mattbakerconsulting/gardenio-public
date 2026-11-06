package codexauth

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func idTokenFor(acct string) string {
	enc := func(s string) string { return base64.RawURLEncoding.EncodeToString([]byte(s)) }
	return enc(`{"alg":"none"}`) + "." + enc(`{"https://api.openai.com/auth":{"chatgpt_account_id":"`+acct+`"}}`) + ".sig"
}

// fakeIssuer is a hermetic auth.openai.com: approveAfter polls answer 403, then 200.
type fakeIssuer struct {
	approveAfter int32
	polls        atomic.Int32
	pollStatus   int // if set, every poll answers this status
	pollBody     string
	exchangeForm map[string]string
}

func (f *fakeIssuer) handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/api/accounts/deviceauth/usercode", func(w http.ResponseWriter, r *http.Request) {
		var in map[string]string
		_ = json.NewDecoder(r.Body).Decode(&in)
		if in["client_id"] != ClientID {
			http.Error(w, `{"error":"bad_client"}`, http.StatusBadRequest)
			return
		}
		_, _ = w.Write([]byte(`{"device_auth_id":"dev-1","usercode":"ABCD-1234","interval":"1"}`))
	})
	mux.HandleFunc("/api/accounts/deviceauth/token", func(w http.ResponseWriter, r *http.Request) {
		var in map[string]string
		_ = json.NewDecoder(r.Body).Decode(&in)
		if in["device_auth_id"] != "dev-1" || in["user_code"] != "ABCD-1234" {
			http.Error(w, `{}`, http.StatusBadRequest)
			return
		}
		n := f.polls.Add(1)
		if f.pollStatus != 0 {
			w.WriteHeader(f.pollStatus)
			_, _ = w.Write([]byte(f.pollBody))
			return
		}
		if n <= f.approveAfter {
			w.WriteHeader(http.StatusForbidden)
			return
		}
		_, _ = w.Write([]byte(`{"authorization_code":"authz","code_challenge":"c","code_verifier":"verif"}`))
	})
	mux.HandleFunc("/oauth/token", func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		f.exchangeForm = map[string]string{}
		for k := range r.PostForm {
			f.exchangeForm[k] = r.PostForm.Get(k)
		}
		_, _ = w.Write([]byte(`{"id_token":"` + idTokenFor("acct-9") + `","access_token":"at","refresh_token":"rt"}`))
	})
	return mux
}

func testDeviceLogin(t *testing.T, f *fakeIssuer) (*DeviceLogin, *time.Time) {
	t.Helper()
	srv := httptest.NewServer(f.handler())
	t.Cleanup(srv.Close)
	d := NewDeviceLogin(srv.URL)
	now := time.Unix(1_800_000_000, 0)
	d.Now = func() time.Time { return now }
	d.Sleep = func(_ context.Context, dur time.Duration) error { now = now.Add(dur); return nil }
	return d, &now
}

func TestDeviceLoginHappyPath(t *testing.T) {
	f := &fakeIssuer{approveAfter: 2}
	d, _ := testDeviceLogin(t, f)
	dc, err := d.Start(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if dc.UserCode != "ABCD-1234" || dc.DeviceAuthID != "dev-1" || dc.Interval != time.Second ||
		!strings.HasSuffix(dc.VerificationURL, "/codex/device") {
		t.Fatalf("start: %+v", dc)
	}
	tr, err := d.Poll(context.Background(), dc)
	if err != nil {
		t.Fatal(err)
	}
	if tr.RefreshToken != "rt" || tr.AccessToken != "at" || f.polls.Load() != 3 {
		t.Fatalf("poll: %+v polls=%d", tr, f.polls.Load())
	}
	for k, want := range map[string]string{
		"grant_type": "authorization_code", "code": "authz", "client_id": ClientID,
		"code_verifier": "verif", "redirect_uri": d.Issuer + "/deviceauth/callback",
	} {
		if f.exchangeForm[k] != want {
			t.Errorf("exchange %s = %q, want %q", k, f.exchangeForm[k], want)
		}
	}
}

func TestDeviceLoginExpires(t *testing.T) {
	f := &fakeIssuer{approveAfter: 1 << 20}
	d, _ := testDeviceLogin(t, f)
	dc, err := d.Start(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := d.Poll(context.Background(), dc); !errors.Is(err, ErrDeviceCodeExpired) {
		t.Fatalf("want expired, got %v", err)
	}
}

func TestDeviceLoginHardPollFailureLeaksNothing(t *testing.T) {
	f := &fakeIssuer{pollStatus: http.StatusBadRequest, pollBody: `{"error":"access_denied","secret":"ABCD-1234"}`}
	d, _ := testDeviceLogin(t, f)
	dc, _ := d.Start(context.Background())
	_, err := d.Poll(context.Background(), dc)
	var de *DeviceLoginError
	if !errors.As(err, &de) || de.Stage != "poll" || de.Status != 400 || de.Code != "access_denied" {
		t.Fatalf("got %v", err)
	}
	if strings.Contains(err.Error(), "ABCD") || strings.Contains(err.Error(), "dev-1") {
		t.Fatalf("error leaks code: %v", err)
	}
}

func TestDeviceLoginSlowDownBacksOff(t *testing.T) {
	f := &fakeIssuer{pollStatus: http.StatusTooManyRequests}
	d, now := testDeviceLogin(t, f)
	var sleeps []time.Duration
	d.Sleep = func(_ context.Context, dur time.Duration) error {
		sleeps = append(sleeps, dur)
		f.pollStatus = 0 // approve on the next poll
		*now = now.Add(dur)
		return nil
	}
	dc, _ := d.Start(context.Background())
	if _, err := d.Poll(context.Background(), dc); err != nil {
		t.Fatal(err)
	}
	if len(sleeps) != 1 || sleeps[0] != 6*time.Second {
		t.Fatalf("sleeps=%v (want one 6s: 1s interval + 5s back-off)", sleeps)
	}
}

func TestDeviceLoginStartRejected(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusForbidden)
	}))
	defer srv.Close()
	if _, err := NewDeviceLogin(srv.URL).Start(context.Background()); err == nil {
		t.Fatal("expected error")
	}
}

func TestExchangeWithoutRefreshTokenFails(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/api/accounts/deviceauth/usercode", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"device_auth_id":"d","user_code":"U","interval":2}`))
	})
	mux.HandleFunc("/api/accounts/deviceauth/token", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"authorization_code":"a","code_verifier":"v"}`))
	})
	mux.HandleFunc("/oauth/token", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"access_token":"at"}`))
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()
	d := NewDeviceLogin(srv.URL)
	dc, err := d.Start(context.Background())
	if err != nil || dc.Interval != 2*time.Second {
		t.Fatalf("start: %v %+v", err, dc)
	}
	if _, err := d.Poll(context.Background(), dc); err == nil {
		t.Fatal("expected incomplete_response error")
	}
}

func TestNewAuthShapeAndAccountID(t *testing.T) {
	a, err := NewAuth(Tokens{IDToken: idTokenFor("acct-9"), AccessToken: "at", RefreshToken: "rt"}, time.Unix(1_800_000_000, 0))
	if err != nil {
		t.Fatal(err)
	}
	if a.AccountID() != "acct-9" || a.RefreshToken() != "rt" || a.LastRefresh().IsZero() {
		t.Fatalf("auth: %s", mustMarshal(t, a))
	}
	if _, err := NewAuth(Tokens{AccessToken: "at"}, time.Now()); err == nil {
		t.Fatal("expected error without refresh token")
	}
}
