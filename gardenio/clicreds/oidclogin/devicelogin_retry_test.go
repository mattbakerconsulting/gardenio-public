package oidclogin

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// deviceIssuer is a fake Keycloak whose token endpoint answers the device
// polls with replies[i] on call i (the last one repeats). "reset" hijacks the
// connection and closes it unanswered, as the edge did on 2026-11-01 ("read:
// connection reset by peer"); "503" and "404" are bare proxy pages; anything
// else is written as a JSON body, with a "400:" prefix setting the status.
func deviceIssuer(t *testing.T, replies ...string) (*httptest.Server, *atomic.Int32) {
	t.Helper()
	var calls atomic.Int32
	var srv *httptest.Server
	srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/.well-known/openid-configuration":
			_ = json.NewEncoder(w).Encode(map[string]string{
				"issuer": srv.URL, "token_endpoint": srv.URL + "/token",
				"device_authorization_endpoint": srv.URL + "/device",
			})
		case "/device":
			_ = json.NewEncoder(w).Encode(map[string]any{
				"device_code": "dev", "user_code": "ABCD-EFGH",
				"verification_uri": srv.URL + "/verify", "expires_in": 600, "interval": 5,
			})
		case "/token":
			i := int(calls.Add(1)) - 1
			if i >= len(replies) {
				i = len(replies) - 1
			}
			switch body := replies[i]; {
			case body == "reset":
				conn, _, err := w.(http.Hijacker).Hijack()
				if err != nil {
					t.Errorf("hijack: %v", err)
					return
				}
				_ = conn.Close()
			case body == "503" || body == "404":
				code := http.StatusServiceUnavailable
				if body == "404" {
					code = http.StatusNotFound
				}
				w.Header().Set("Content-Type", "text/html")
				w.WriteHeader(code)
				_, _ = w.Write([]byte("<html>" + body + " nginx</html>"))
			case strings.HasPrefix(body, "400:"):
				w.WriteHeader(http.StatusBadRequest)
				_, _ = w.Write([]byte(strings.TrimPrefix(body, "400:")))
			default:
				_, _ = w.Write([]byte(body))
			}
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)
	return srv, &calls
}

const pendingReply = `400:{"error":"authorization_pending"}`

// A reset connection and a bare 503 while the person approves the code are
// not their answer: the poll backs off and keeps going, and the Allow they
// click is still collected.
func TestDeviceLoginSurvivesTransientTokenErrors(t *testing.T) {
	srv, calls := deviceIssuer(t, pendingReply, "reset", "503", pendingReply,
		`{"access_token":"tok","refresh_token":"r","expires_in":300}`)
	var out bytes.Buffer
	var sleeps []time.Duration
	s, err := DeviceLogin(context.Background(), srv.Client(), srv.URL, "c", Prompt{
		CLI: "kd", Out: &out, Sleep: func(d time.Duration) { sleeps = append(sleeps, d) },
	})
	if err != nil {
		t.Fatalf("DeviceLogin: %v (output %q)", err, out.String())
	}
	if s.AccessToken != "tok" {
		t.Fatalf("access token = %q, want tok", s.AccessToken)
	}
	if got := calls.Load(); got != 5 {
		t.Fatalf("token calls = %d, want 5", got)
	}
	// interval, interval, then backoff 10s and 20s, then the interval again.
	want := []time.Duration{5 * time.Second, 5 * time.Second, 10 * time.Second, 20 * time.Second, 5 * time.Second}
	if len(sleeps) != len(want) {
		t.Fatalf("sleeps = %v, want %v", sleeps, want)
	}
	for i := range want {
		if sleeps[i] != want[i] {
			t.Fatalf("sleeps = %v, want %v", sleeps, want)
		}
	}
	if n := strings.Count(out.String(), "still waiting"); n != 1 {
		t.Fatalf("output names the outage %d times, want once: %q", n, out.String())
	}
}

// A 4xx with no OAuth body is a misconfiguration, not an outage: it still
// ends the login at once.
func TestDeviceLoginBare404IsFatal(t *testing.T) {
	srv, calls := deviceIssuer(t, "404")
	_, err := DeviceLogin(context.Background(), srv.Client(), srv.URL, "c", Prompt{
		CLI: "kd", Out: &bytes.Buffer{}, Sleep: func(time.Duration) {},
	})
	if err == nil || !strings.Contains(err.Error(), "404") {
		t.Fatalf("err = %v, want the 404", err)
	}
	if got := calls.Load(); got != 1 {
		t.Fatalf("token calls = %d, want 1", got)
	}
}

// access_denied still ends it, after a transient failure too.
func TestDeviceLoginDeniedAfterReset(t *testing.T) {
	srv, _ := deviceIssuer(t, "reset", `400:{"error":"access_denied"}`)
	_, err := DeviceLogin(context.Background(), srv.Client(), srv.URL, "c", Prompt{
		CLI: "kd", Out: &bytes.Buffer{}, Sleep: func(time.Duration) {},
	})
	if err == nil || err.Error() != "sign-in was denied" {
		t.Fatalf("err = %v, want sign-in was denied", err)
	}
}

func TestTransientTokenError(t *testing.T) {
	for _, tc := range []struct {
		err  error
		want bool
	}{
		{&HTTPStatusError{StatusCode: 502}, true},
		{&HTTPStatusError{StatusCode: 429}, true},
		{&HTTPStatusError{StatusCode: 404}, false},
		{&OAuthError{Code: "invalid_client"}, false},
	} {
		if got := transientTokenError(tc.err); got != tc.want {
			t.Errorf("transientTokenError(%v) = %v, want %v", tc.err, got, tc.want)
		}
	}
}
