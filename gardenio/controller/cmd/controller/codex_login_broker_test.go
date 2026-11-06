package main

import (
	"bytes"
	"context"
	"encoding/base64"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"gardenio/controller/internal/beadsapi"
	"gardenio/controller/internal/codexauth"
)

type corev1Secret = corev1.Secret

var metav1DeleteOpts = metav1.DeleteOptions{}

type bFakeDevice struct {
	startErr error
	starts   int
	result   chan bFakeResult
}

type bFakeResult struct {
	tr  codexauth.Tokens
	err error
}

func (d *bFakeDevice) Start(context.Context) (codexauth.DeviceCode, error) {
	d.starts++
	if d.startErr != nil {
		return codexauth.DeviceCode{}, d.startErr
	}
	return codexauth.DeviceCode{
		DeviceAuthID: "dev-1", UserCode: "ABCD-1234", VerificationURL: "https://auth.openai.com/codex/device",
		Interval: time.Second, ExpiresAt: kNow.Add(15 * time.Minute),
	}, nil
}

func (d *bFakeDevice) Poll(ctx context.Context, _ codexauth.DeviceCode) (codexauth.Tokens, error) {
	select {
	case r := <-d.result:
		return r.tr, r.err
	case <-ctx.Done():
		return codexauth.Tokens{}, ctx.Err()
	}
}

type bFakeEsc struct {
	mu       sync.Mutex
	comments []string
	closed   []string
}

func (e *bFakeEsc) Comment(_ context.Context, id, text string) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.comments = append(e.comments, id+": "+text)
	return nil
}

func (e *bFakeEsc) Resolve(_ context.Context, id string, _ map[string]any) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.closed = append(e.closed, id)
	return nil
}

func bIDToken(acct string) string {
	enc := func(s string) string { return base64.RawURLEncoding.EncodeToString([]byte(s)) }
	return enc(`{"alg":"none"}`) + "." + enc(`{"https://api.openai.com/auth":{"chatgpt_account_id":"`+acct+`"}}`) + ".sig"
}

func bTokens(acct, refresh string) codexauth.Tokens {
	return codexauth.Tokens{IDToken: bIDToken(acct), AccessToken: kJWT(kNow.Add(10 * 24 * time.Hour)), RefreshToken: refresh}
}

type bEnv struct {
	*kEnv
	b   *codexLoginBroker
	dev *bFakeDevice
	esc *bFakeEsc
}

// newBEnv: the account's login (refresh-1) was rejected and the keeper recorded it.
func newBEnv(t *testing.T) *bEnv {
	t.Helper()
	doc := kAuthDoc(kJWT(kNow.Add(2*time.Hour)), "refresh-1", kNow.Add(-time.Hour))
	old, _ := codexauth.Parse(doc)
	e := newKEnv(t, doc, map[string]string{"probe_auth_status": "rejected", fieldCodexRejectedFP: old.RefreshFingerprint()})
	dev := &bFakeDevice{result: make(chan bFakeResult, 1)}
	esc := &bFakeEsc{}
	b := &codexLoginBroker{
		k: e.k, device: dev, esc: esc, run: context.Background(),
		logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
		flows:  map[string]*codexLoginFlow{},
	}
	return &bEnv{kEnv: e, b: b, dev: dev, esc: esc}
}

func (e *bEnv) waitState(t *testing.T, want string) codexLoginStatus {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if st := e.b.Status("codex-main"); st.State == want {
			return st
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("flow never reached %q; at %+v", want, e.b.Status("codex-main"))
	return codexLoginStatus{}
}

var bReq = codexLoginRequest{Account: "codex-main", EscalationID: "gd-esc", StartedBy: "op@example.com"}

func TestBrokerHappyPathStoresLoginAndClosesEscalation(t *testing.T) {
	e := newBEnv(t)
	res, err := e.b.Start(context.Background(), bReq)
	if err != nil {
		t.Fatal(err)
	}
	if res.UserCode != "ABCD-1234" || !strings.HasSuffix(res.VerificationURL, "/codex/device") {
		t.Fatalf("start: %+v", res)
	}
	e.dev.result <- bFakeResult{tr: bTokens("acct-1", "refresh-NEW")}
	st := e.waitState(t, loginSucceeded)

	got := e.secretAuth(t, kTestBase)
	if got.RefreshToken() != "refresh-NEW" || st.Fingerprint != got.RefreshFingerprint() || got.AccountID() != "acct-1" {
		t.Fatalf("secret not updated: fp=%s", st.Fingerprint)
	}
	if len(e.esc.closed) != 1 || e.esc.closed[0] != "gd-esc" {
		t.Fatalf("escalation not closed: %v", e.esc.closed)
	}
	blob := strings.Join(e.esc.comments, "\n")
	for _, secret := range []string{"refresh-NEW", "ABCD-1234", "dev-1"} {
		if strings.Contains(blob, secret) {
			t.Fatalf("escalation comment leaks %q: %s", secret, blob)
		}
	}
	// The keeper then clears the rejection by itself: a different token is in the Secret.
	e.beads.accounts[0].Fields = map[string]string{"probe_auth_status": "rejected", fieldCodexRejectedFP: "stale-fp"}
	e.tick()
	if e.beads.merged()["probe_auth_status"] != "ok" {
		t.Fatalf("keeper did not clear the rejection: %v", e.beads.merged())
	}
}

func TestBrokerRefusesToReplaceAWorkingLogin(t *testing.T) {
	e := newBEnv(t)
	e.beads.accounts[0].Fields = map[string]string{} // not rejected
	_, err := e.b.Start(context.Background(), bReq)
	var le *codexLoginError
	if !errors.As(err, &le) || le.Status != http.StatusConflict {
		t.Fatalf("want 409, got %v", err)
	}
	if e.dev.starts != 0 {
		t.Fatal("must not request a device code for a healthy login")
	}
}

func TestBrokerUnknownAccountAndMissingSecret(t *testing.T) {
	e := newBEnv(t)
	var le *codexLoginError
	if _, err := e.b.Start(context.Background(), codexLoginRequest{Account: "nope"}); !errors.As(err, &le) || le.Status != 404 {
		t.Fatalf("want 404, got %v", err)
	}
	if err := e.k8s.CoreV1().Secrets(kTestNS).Delete(context.Background(), kTestBase, metav1DeleteOpts); err != nil {
		t.Fatal(err)
	}
	if _, err := e.b.Start(context.Background(), bReq); !errors.As(err, &le) || le.Status != http.StatusConflict {
		t.Fatalf("want 409 for a missing Secret, got %v", err)
	}
}

func TestBrokerRefusesWithoutWritePermission(t *testing.T) {
	e := newBEnv(t)
	e.allow = false
	var le *codexLoginError
	if _, err := e.b.Start(context.Background(), bReq); !errors.As(err, &le) || le.Status != http.StatusServiceUnavailable {
		t.Fatalf("want 503, got %v", err)
	}
}

func TestBrokerBootstrapsAnEmptySecret(t *testing.T) {
	e := newBEnv(t)
	e.beads.accounts[0].Fields = map[string]string{}
	e.touch(kTestBase, func(s *corev1Secret) { s.Data = map[string][]byte{} })
	if _, err := e.b.Start(context.Background(), bReq); err != nil {
		t.Fatal(err)
	}
	e.dev.result <- bFakeResult{tr: bTokens("acct-9", "refresh-FIRST")}
	e.waitState(t, loginSucceeded)
	if got := e.secretAuth(t, kTestBase); got.RefreshToken() != "refresh-FIRST" || got.AccountID() != "acct-9" {
		t.Fatal("bootstrap login not stored")
	}
}

func TestBrokerRejectsADifferentChatGPTAccount(t *testing.T) {
	e := newBEnv(t)
	if _, err := e.b.Start(context.Background(), bReq); err != nil {
		t.Fatal(err)
	}
	e.dev.result <- bFakeResult{tr: bTokens("someone-else", "refresh-NEW")}
	st := e.waitState(t, loginFailed)
	if st.ErrorCode != "chatgpt_account_mismatch" {
		t.Fatalf("code=%q", st.ErrorCode)
	}
	if e.secretAuth(t, kTestBase).RefreshToken() != "refresh-1" {
		t.Fatal("the Secret was overwritten with another account's login")
	}
	if len(e.esc.closed) != 0 {
		t.Fatal("escalation must stay open on failure")
	}
}

func TestBrokerAllowAccountChange(t *testing.T) {
	e := newBEnv(t)
	req := bReq
	req.AllowAccountChange = true
	if _, err := e.b.Start(context.Background(), req); err != nil {
		t.Fatal(err)
	}
	e.dev.result <- bFakeResult{tr: bTokens("someone-else", "refresh-NEW")}
	e.waitState(t, loginSucceeded)
}

func TestBrokerNeverOverwritesAReLoginMadeMeanwhile(t *testing.T) {
	e := newBEnv(t)
	if _, err := e.b.Start(context.Background(), bReq); err != nil {
		t.Fatal(err)
	}
	e.touch(kTestBase, func(s *corev1Secret) {
		s.Data["auth.json"] = kAuthDoc(kJWT(kNow.Add(24*time.Hour)), "operator-relogin", kNow)
	})
	e.dev.result <- bFakeResult{tr: bTokens("acct-1", "refresh-NEW")}
	st := e.waitState(t, loginFailed)
	if st.ErrorCode != "superseded" || e.secretAuth(t, kTestBase).RefreshToken() != "operator-relogin" {
		t.Fatalf("overwrote a concurrent re-login: %+v", st)
	}
}

func TestBrokerRaceAtUpdateIsAConflictNotAnOverwrite(t *testing.T) {
	e := newBEnv(t)
	if _, err := e.b.Start(context.Background(), bReq); err != nil {
		t.Fatal(err)
	}
	e.raceOnce = func() { // a writer slips in between the broker's Get and Update
		e.rvs[kTestBase] = 41
	}
	e.dev.result <- bFakeResult{tr: bTokens("acct-1", "refresh-NEW")}
	st := e.waitState(t, loginFailed)
	if st.ErrorCode != "superseded" || e.secretAuth(t, kTestBase).RefreshToken() != "refresh-1" {
		t.Fatalf("%+v", st)
	}
}

func TestBrokerReshowsAPendingFlowInsteadOfStartingASecond(t *testing.T) {
	e := newBEnv(t)
	first, err := e.b.Start(context.Background(), bReq)
	if err != nil {
		t.Fatal(err)
	}
	second, err := e.b.Start(context.Background(), bReq)
	if err != nil || !second.Reused || second.UserCode != first.UserCode || e.dev.starts != 1 {
		t.Fatalf("second=%+v err=%v starts=%d", second, err, e.dev.starts)
	}
}

func TestBrokerExpiryAndHardFailureLeaveTheSecretAlone(t *testing.T) {
	for name, tc := range map[string]struct {
		err  error
		want string
		code string
	}{
		"expired": {codexauth.ErrDeviceCodeExpired, loginExpired, "expired"},
		"denied":  {&codexauth.DeviceLoginError{Stage: "poll", Status: 400, Code: "access_denied"}, loginFailed, "poll_http_400_access_denied"},
	} {
		t.Run(name, func(t *testing.T) {
			e := newBEnv(t)
			if _, err := e.b.Start(context.Background(), bReq); err != nil {
				t.Fatal(err)
			}
			e.dev.result <- bFakeResult{err: tc.err}
			if st := e.waitState(t, tc.want); st.ErrorCode != tc.code {
				t.Fatalf("code=%q", st.ErrorCode)
			}
			if e.secretAuth(t, kTestBase).RefreshToken() != "refresh-1" || len(e.esc.closed) != 0 {
				t.Fatal("state changed on a failed login")
			}
		})
	}
}

func TestBrokerStartFailureSurfacesAsBadGatewayWithoutDetail(t *testing.T) {
	e := newBEnv(t)
	e.dev.startErr = errors.New("boom https://x/?user_code=SECRET")
	_, err := e.b.Start(context.Background(), bReq)
	var le *codexLoginError
	if !errors.As(err, &le) || le.Status != http.StatusBadGateway || strings.Contains(le.Msg, "SECRET") {
		t.Fatalf("%v", err)
	}
}

func TestCodexLoginEndpointsAuthAndLeadership(t *testing.T) {
	e := newBEnv(t)
	h := &codexLoginBrokerHolder{}
	mux := http.NewServeMux()
	registerCodexLoginEndpoints(mux, h)
	do := func(token string) *httptest.ResponseRecorder {
		r := httptest.NewRequest("POST", "/api/v1/codex/device-login", bytes.NewBufferString(`{"account":"codex-main"}`))
		if token != "" {
			r.Header.Set("Authorization", "Bearer "+token)
		}
		w := httptest.NewRecorder()
		mux.ServeHTTP(w, r)
		return w
	}

	t.Setenv(controllerAPITokenEnv, "")
	if w := do("anything"); w.Code != http.StatusServiceUnavailable {
		t.Fatalf("no token configured must disable the route, got %d", w.Code)
	}
	t.Setenv(controllerAPITokenEnv, "s3cret")
	if w := do(""); w.Code != http.StatusUnauthorized {
		t.Fatalf("missing bearer: %d", w.Code)
	}
	if w := do("wrong"); w.Code != http.StatusUnauthorized {
		t.Fatalf("wrong bearer: %d", w.Code)
	}
	if w := do("s3cret"); w.Code != http.StatusServiceUnavailable {
		t.Fatalf("non-leader must 503, got %d", w.Code)
	}
	h.Set(e.b)
	w := do("s3cret")
	if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), "ABCD-1234") || w.Header().Get("Cache-Control") != "no-store" {
		t.Fatalf("start: %d %s", w.Code, w.Body.String())
	}

	r := httptest.NewRequest("GET", "/api/v1/codex/device-login/status?account=codex-main", nil)
	r.Header.Set("Authorization", "Bearer s3cret")
	sw := httptest.NewRecorder()
	mux.ServeHTTP(sw, r)
	if sw.Code != http.StatusOK || !strings.Contains(sw.Body.String(), `"pending"`) || strings.Contains(sw.Body.String(), "ABCD") {
		t.Fatalf("status: %d %s", sw.Code, sw.Body.String())
	}
}

// The daemon answers the generic bead close of an open escalation with 409 and
// only the escalation result verb closes it, so pin the verb through the real client.
func TestCodexEscalationResolveUsesTheResultVerb(t *testing.T) {
	var mu sync.Mutex
	var paths []string
	var body string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		paths = append(paths, r.Method+" "+r.URL.Path)
		switch r.URL.Path {
		case "/v1/escalations/gd-esc/result":
			b, _ := io.ReadAll(r.Body)
			body = string(b)
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{}`))
		default: // incl. POST /v1/beads/gd-esc/close
			http.Error(w, "use approve/reject/result", http.StatusConflict)
		}
	}))
	defer srv.Close()
	dc, err := beadsapi.New(beadsapi.Config{HTTPAddr: srv.URL, Token: "t"})
	if err != nil {
		t.Fatal(err)
	}
	acc := beadsCodexAccounts{daemon: dc, accounts: dc}
	if err := acc.Resolve(context.Background(), "gd-esc", map[string]any{"account": "codex-main", "fingerprint": "fp1"}); err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(paths) != 1 || paths[0] != "POST /v1/escalations/gd-esc/result" {
		t.Fatalf("expected only the escalation result verb, got %v", paths)
	}
	for _, want := range []string{`"result_status":"success"`, `"account":"codex-main"`, `"fingerprint":"fp1"`} {
		if !strings.Contains(body, want) {
			t.Fatalf("result body missing %s: %s", want, body)
		}
	}
}
