package bridge

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/slack-go/slack"

	"gardenio/controller/internal/beadsapi"
)

// fakeKeycloak is a token + revoke endpoint that records what it was asked.
type fakeKeycloak struct {
	mu          sync.Mutex
	issuer      string
	clientID    string
	codes       map[string]string // code -> expected PKCE verifier
	refreshOK   map[string]string // refresh token -> email
	tokenCalls  []url.Values
	revoked     []string
	expiresIn   int
	noRefresh   bool
	rotate      bool // refresh grants hand back a new refresh token
	srv         *httptest.Server
	accessCount int
}

func newFakeKeycloak(t *testing.T) *fakeKeycloak {
	t.Helper()
	f := &fakeKeycloak{clientID: "gardenio-slack-link", codes: map[string]string{}, refreshOK: map[string]string{}, expiresIn: 300}
	mux := http.NewServeMux()
	mux.HandleFunc("/protocol/openid-connect/token", f.token)
	mux.HandleFunc("/protocol/openid-connect/revoke", func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		f.mu.Lock()
		f.revoked = append(f.revoked, r.Form.Get("token"))
		f.mu.Unlock()
		w.WriteHeader(http.StatusOK)
	})
	f.srv = httptest.NewServer(mux)
	t.Cleanup(f.srv.Close)
	f.issuer = f.srv.URL
	return f
}

func (f *fakeKeycloak) idToken(email string) string {
	claims := map[string]any{"iss": f.issuer, "sub": "sub-" + email, "email": email, "email_verified": true, "aud": f.clientID, "azp": f.clientID, "exp": time.Now().Add(time.Hour).Unix()}
	raw, _ := json.Marshal(claims)
	return "h." + base64.RawURLEncoding.EncodeToString(raw) + ".s"
}

func (f *fakeKeycloak) token(w http.ResponseWriter, r *http.Request) {
	_ = r.ParseForm()
	f.mu.Lock()
	defer f.mu.Unlock()
	f.tokenCalls = append(f.tokenCalls, r.Form)
	if r.Form.Get("client_id") != f.clientID || r.Form.Get("client_secret") != "s3cret" {
		http.Error(w, `{"error":"unauthorized_client"}`, http.StatusUnauthorized)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	switch r.Form.Get("grant_type") {
	case "authorization_code":
		want, ok := f.codes[r.Form.Get("code")]
		if !ok || want != r.Form.Get("code_verifier") {
			http.Error(w, `{"error":"invalid_grant","error_description":"bad code or verifier"}`, http.StatusBadRequest)
			return
		}
		email := strings.TrimPrefix(r.Form.Get("code"), "code-")
		resp := map[string]any{"access_token": "at-" + email, "id_token": f.idToken(email), "expires_in": f.expiresIn}
		if !f.noRefresh {
			resp["refresh_token"] = "rt-" + email
			f.refreshOK["rt-"+email] = email
		}
		_ = json.NewEncoder(w).Encode(resp)
	case "refresh_token":
		email, ok := f.refreshOK[r.Form.Get("refresh_token")]
		if !ok {
			http.Error(w, `{"error":"invalid_grant","error_description":"Session not active"}`, http.StatusBadRequest)
			return
		}
		f.accessCount++
		rt := r.Form.Get("refresh_token")
		if f.rotate {
			rt += "+"
			f.refreshOK[rt] = email
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"access_token": "at-" + email + "-r", "refresh_token": rt, "expires_in": f.expiresIn})
	default:
		http.Error(w, `{"error":"unsupported_grant_type"}`, http.StatusBadRequest)
	}
}

// fakeLinkDaemon records identity-link calls and can refuse them.
type fakeLinkDaemon struct {
	mu       sync.Mutex
	linked   []beadsapi.SlackIdentityLinkRequest
	unlinked []beadsapi.SlackIdentityLinkRequest
	bearers  []string
	linkErr  error
}

func (d *fakeLinkDaemon) LinkSlackIdentityAsUser(_ context.Context, bearer string, req beadsapi.SlackIdentityLinkRequest) (*beadsapi.SlackIdentityLinkResponse, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.bearers = append(d.bearers, bearer)
	if d.linkErr != nil {
		return nil, d.linkErr
	}
	d.linked = append(d.linked, req)
	return &beadsapi.SlackIdentityLinkResponse{SlackUserID: req.SlackUserID}, nil
}

func (d *fakeLinkDaemon) UnlinkSlackIdentityAsUser(_ context.Context, bearer string, req beadsapi.SlackIdentityLinkRequest) (*beadsapi.SlackIdentityLinkResponse, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.bearers = append(d.bearers, bearer)
	d.unlinked = append(d.unlinked, req)
	return &beadsapi.SlackIdentityLinkResponse{SlackUserID: req.SlackUserID}, nil
}

// fakeSlackEmails is the Slack profile-email lookup: user id -> email.
type fakeSlackEmails struct {
	emails map[string]string
	err    error
}

func (f *fakeSlackEmails) GetUserEmail(_ context.Context, id string) (string, error) {
	return f.emails[id], f.err
}

type linkFixture struct {
	emails *fakeSlackEmails
	kc     *fakeKeycloak
	daemon *fakeLinkDaemon
	store  SlackLinkStore
	linker *SlackLinker
	web    *httptest.Server
	now    time.Time

	pmu     sync.Mutex
	prompts []confirmPrompt
	hookErr error
}

// confirmPrompt is one "Link this Slack account to <email>?" the linker asked
// the bridge to show.
type confirmPrompt struct{ user, id, email, warning string }

func newLinkFixture(t *testing.T, store SlackLinkStore) *linkFixture {
	t.Helper()
	f := &linkFixture{kc: newFakeKeycloak(t), daemon: &fakeLinkDaemon{}, store: store, now: time.Date(2026, 10, 14, 12, 0, 0, 0, time.UTC)}
	if f.store == nil {
		f.store = newMemorySlackLinkStore()
	}
	key := make([]byte, 32)
	for i := range key {
		key[i] = byte(i)
	}
	l, err := NewSlackLinker(SlackLinkConfig{
		PublicURL: "https://gardenio.example.com", IssuerURL: f.kc.issuer, ClientID: "gardenio-slack-link", ClientSecret: "s3cret",
		Key: key, StorePath: "unused", Now: func() time.Time { return f.now },
	}, f.store, f.daemon, nil)
	if err != nil {
		t.Fatal(err)
	}
	f.emails = &fakeSlackEmails{emails: map[string]string{"U1": "owner@example.com"}}
	l.SetEmailResolver(f.emails)
	l.SetOnConfirm(func(user, id, email, warning string) error {
		f.pmu.Lock()
		defer f.pmu.Unlock()
		if f.hookErr != nil {
			return f.hookErr
		}
		f.prompts = append(f.prompts, confirmPrompt{user, id, email, warning})
		return nil
	})
	f.linker = l
	f.web = httptest.NewServer(l.Handler())
	t.Cleanup(f.web.Close)
	return f
}

// start presses the button for user and returns the state the start URL carries.
func (f *linkFixture) start(t *testing.T, user string) string {
	t.Helper()
	u, err := f.linker.StartURL(user, "T1")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(u, "https://gardenio.example.com/slack/link/start?s=") {
		t.Fatalf("start url %q", u)
	}
	parsed, _ := url.Parse(u)
	return parsed.Query().Get("s")
}

func noRedirect() *http.Client {
	return &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
}

// authorize follows /slack/link/start and returns the PKCE challenge Keycloak was sent.
func (f *linkFixture) authorize(t *testing.T, state string) url.Values {
	t.Helper()
	resp, err := noRedirect().Get(f.web.URL + "/slack/link/start?s=" + url.QueryEscape(state))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusFound {
		t.Fatalf("start status %d", resp.StatusCode)
	}
	loc, _ := url.Parse(resp.Header.Get("Location"))
	if !strings.HasPrefix(loc.String(), f.kc.issuer+"/protocol/openid-connect/auth?") {
		t.Fatalf("redirect %q", loc)
	}
	return loc.Query()
}

func (f *linkFixture) callback(t *testing.T, state, code string) *http.Response {
	t.Helper()
	resp, err := http.Get(f.web.URL + "/slack/link/callback?state=" + url.QueryEscape(state) + "&code=" + url.QueryEscape(code))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { resp.Body.Close() })
	return resp
}

func (f *linkFixture) lastPrompt(t *testing.T) confirmPrompt {
	t.Helper()
	f.pmu.Lock()
	defer f.pmu.Unlock()
	if len(f.prompts) == 0 {
		t.Fatal("no confirm prompt was posted")
	}
	return f.prompts[len(f.prompts)-1]
}

func (f *linkFixture) promptCount() int {
	f.pmu.Lock()
	defer f.pmu.Unlock()
	return len(f.prompts)
}

// signIn starts the flow for slackUser, completes the Keycloak sign-in as email
// and returns the confirm prompt it produced. Nothing may be linked yet.
func (f *linkFixture) signIn(t *testing.T, slackUser, email string) confirmPrompt {
	t.Helper()
	state := f.start(t, slackUser)
	f.kc.codes["code-"+email] = f.verifierFor(state)
	if resp := f.callback(t, state, "code-"+email); resp.StatusCode != http.StatusOK {
		t.Fatalf("callback status %d", resp.StatusCode)
	}
	return f.lastPrompt(t)
}

// link is signIn plus the Slack user's Confirm press.
func (f *linkFixture) link(t *testing.T, slackUser, email string) {
	t.Helper()
	p := f.signIn(t, slackUser, email)
	if got, err := f.linker.ConfirmLink(context.Background(), p.id, slackUser); err != nil || got != email {
		t.Fatalf("ConfirmLink = %q %v", got, err)
	}
}

// verifierFor finds the pending state's PKCE verifier (test-only peek).
func (f *linkFixture) verifierFor(state string) string {
	f.linker.mu.Lock()
	defer f.linker.mu.Unlock()
	return f.linker.pending[state].codeVerifier
}

func TestSlackLink_HappyPathBindsOnlyAfterConfirm(t *testing.T) {
	f := newLinkFixture(t, nil)

	state := f.start(t, "U1")
	q := f.authorize(t, state)
	if q.Get("client_id") != "gardenio-slack-link" || q.Get("code_challenge_method") != "S256" || !strings.Contains(q.Get("scope"), "offline_access") || q.Get("redirect_uri") != "https://gardenio.example.com/slack/link/callback" {
		t.Fatalf("authorize params %v", q)
	}
	f.kc.codes["code-owner@example.com"] = f.verifierFor(state)

	resp := f.callback(t, state, "code-owner@example.com")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("callback status %d", resp.StatusCode)
	}
	// The browser sign-in alone binds and stores NOTHING: the Slack user is asked.
	if len(f.daemon.linked) != 0 {
		t.Fatalf("callback must not bind before confirm: %+v", f.daemon.linked)
	}
	if _, ok, _ := f.store.Get("U1"); ok {
		t.Fatal("callback must not store a link before confirm")
	}
	if _, ok := f.linker.Linked("U1"); ok {
		t.Fatal("Linked() must be false before confirm")
	}
	p := f.lastPrompt(t)
	if p.user != "U1" || p.email != "owner@example.com" || p.warning != "" || p.id == "" {
		t.Fatalf("prompt %+v, want U1 asked about owner@example.com with no warning", p)
	}

	// Confirm from that Slack user completes the link.
	email, err := f.linker.ConfirmLink(context.Background(), p.id, "U1")
	if err != nil || email != "owner@example.com" {
		t.Fatalf("ConfirmLink = %q %v", email, err)
	}
	// The bind went to the daemon AS THE PERSON with the Slack id from the state.
	if len(f.daemon.linked) != 1 || f.daemon.linked[0].SlackUserID != "U1" || f.daemon.linked[0].SlackTeamID != "T1" || f.daemon.bearers[0] != "at-owner@example.com" {
		t.Fatalf("daemon link calls %+v bearers %v", f.daemon.linked, f.daemon.bearers)
	}
	link, ok, _ := f.store.Get("U1")
	if !ok || link.Email != "owner@example.com" || len(link.Ciphertext) == 0 || strings.Contains(string(link.Ciphertext), "rt-owner") {
		t.Fatalf("stored link %+v", link)
	}
	if email, ok := f.linker.Linked("U1"); !ok || email != "owner@example.com" {
		t.Fatal("Linked() should report the email")
	}

	// Per write: the cached access token until it nears expiry, then a refresh.
	tok, email, err := f.linker.AccessToken(context.Background(), "U1")
	if err != nil || tok != "at-owner@example.com" || email != "owner@example.com" {
		t.Fatalf("AccessToken = %q %q %v", tok, email, err)
	}
	f.now = f.now.Add(290 * time.Second)
	tok, _, err = f.linker.AccessToken(context.Background(), "U1")
	if err != nil || tok != "at-owner@example.com-r" {
		t.Fatalf("after expiry AccessToken = %q %v", tok, err)
	}
	if f.kc.accessCount != 1 {
		t.Fatalf("refresh calls = %d", f.kc.accessCount)
	}
	link, _, _ = f.store.Get("U1")
	if !link.LastUsedAt.Equal(f.now) {
		t.Fatalf("last_used_at not recorded: %v", link.LastUsedAt)
	}
}

func TestSlackLink_ForgedExpiredAndReplayedCallbacksAreRefused(t *testing.T) {
	f := newLinkFixture(t, nil)

	// Forged: a state nobody issued.
	if resp := f.callback(t, "forged", "code-owner@example.com"); resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("forged callback status %d", resp.StatusCode)
	}
	// Expired: issued, then the TTL passes.
	state := f.start(t, "U1")
	f.now = f.now.Add(slackLinkStateTTL + time.Second)
	if resp := f.callback(t, state, "code-owner@example.com"); resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("expired callback status %d", resp.StatusCode)
	}
	// Replayed: a good callback, then the same state again.
	state = f.start(t, "U1")
	f.kc.codes["code-owner@example.com"] = f.verifierFor(state)
	if resp := f.callback(t, state, "code-owner@example.com"); resp.StatusCode != http.StatusOK {
		t.Fatalf("first callback status %d", resp.StatusCode)
	}
	if resp := f.callback(t, state, "code-owner@example.com"); resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("replayed callback status %d", resp.StatusCode)
	}
	if n := f.promptCount(); n != 1 {
		t.Fatalf("exactly one confirm prompt expected, got %d", n)
	}
	// A start for an unknown state is refused too (no redirect to Keycloak).
	resp, _ := noRedirect().Get(f.web.URL + "/slack/link/start?s=nope")
	resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("forged start status %d", resp.StatusCode)
	}
}

func TestSlackLink_PKCEMismatchStoresNothingAndDaemonRefusalAtConfirmRevokes(t *testing.T) {
	f := newLinkFixture(t, nil)
	// Intercepted code without the verifier: Keycloak refuses, nothing stored.
	state := f.start(t, "U1")
	f.kc.codes["code-owner@example.com"] = "someone-elses-verifier"
	if resp := f.callback(t, state, "code-owner@example.com"); resp.StatusCode != http.StatusBadGateway {
		t.Fatalf("pkce mismatch status %d", resp.StatusCode)
	}
	if n := f.promptCount(); n != 0 {
		t.Fatalf("a failed exchange must not prompt, prompts=%d", n)
	}
	// Daemon 409 at Confirm: the refresh token is revoked and nothing stored.
	f.daemon.linkErr = &beadsapi.APIError{StatusCode: http.StatusConflict, Message: "slack_id_conflict"}
	p := f.signIn(t, "U1", "owner@example.com")
	_, err := f.linker.ConfirmLink(context.Background(), p.id, "U1")
	var refused *LinkRefusedError
	if !errors.As(err, &refused) || !strings.Contains(refused.Reason, "already bound") {
		t.Fatalf("ConfirmLink err = %v, want a LinkRefusedError about the conflict", err)
	}
	if _, ok, _ := f.store.Get("U1"); ok {
		t.Fatal("nothing must be stored when the daemon refuses the bind")
	}
	if len(f.kc.revoked) != 1 || f.kc.revoked[0] != "rt-owner@example.com" {
		t.Fatalf("refused bind must revoke the fresh offline token, revoked=%v", f.kc.revoked)
	}
	// No offline_access granted: refused before any prompt.
	f.daemon.linkErr = nil
	f.kc.noRefresh = true
	before := f.promptCount()
	state = f.start(t, "U1")
	f.kc.codes["code-owner@example.com"] = f.verifierFor(state)
	if resp := f.callback(t, state, "code-owner@example.com"); resp.StatusCode != http.StatusBadGateway {
		t.Fatalf("no refresh token status %d", resp.StatusCode)
	}
	if f.promptCount() != before {
		t.Fatal("no offline token: no prompt may be posted")
	}
}

// gd-L55f9EYFjN, the forwarded start URL: A (Slack U1) starts the flow and
// forwards the URL; V signs in at Keycloak. The confirm question is posted to A,
// not V, and V's identity is linked to nobody until A answers. Nobody else can
// answer for A.
func TestSlackLink_ForwardedButtonConfirmGoesToTheStarterNotTheSigner(t *testing.T) {
	f := newLinkFixture(t, nil)
	f.emails.emails["U1"] = "attacker@evil.example"
	f.emails.emails["UVICTIM"] = "victim@example.com"

	p := f.signIn(t, "U1", "victim@example.com")
	if p.user != "U1" {
		t.Fatalf("the confirm must be shown to the Slack user who started the flow (U1), got %q", p.user)
	}
	if p.email != "victim@example.com" {
		t.Fatalf("the prompt must name the account that signed in, got %q", p.email)
	}
	// The mismatch is surfaced as a warning, never as a decision.
	if !strings.Contains(p.warning, "attacker@evil.example") || !strings.Contains(p.warning, "victim@example.com") {
		t.Fatalf("warning %q should name both emails", p.warning)
	}
	// Before any press: nothing bound, stored or linked, for either Slack user.
	for _, u := range []string{"U1", "UVICTIM"} {
		if _, ok, _ := f.store.Get(u); ok {
			t.Fatalf("%s must not be linked before a confirm", u)
		}
	}
	if len(f.daemon.linked) != 0 {
		t.Fatalf("no bind before a confirm: %+v", f.daemon.linked)
	}
	// V (or anyone but the starter) cannot answer it, and the refusal leaves
	// the question intact for U1.
	for _, other := range []string{"UVICTIM", "UOTHER"} {
		if _, err := f.linker.ConfirmLink(context.Background(), p.id, other); !errors.Is(err, ErrLinkConfirmNotYours) {
			t.Fatalf("ConfirmLink by %s = %v, want ErrLinkConfirmNotYours", other, err)
		}
		if _, err := f.linker.CancelLink(context.Background(), p.id, other); !errors.Is(err, ErrLinkConfirmNotYours) {
			t.Fatalf("CancelLink by %s = %v, want ErrLinkConfirmNotYours", other, err)
		}
	}
	if len(f.daemon.linked) != 0 || len(f.kc.revoked) != 0 {
		t.Fatalf("a wrong-user press must change nothing: binds=%v revoked=%v", f.daemon.linked, f.kc.revoked)
	}
	// The starter declines: V's identity is never linked to A, token revoked.
	if _, err := f.linker.CancelLink(context.Background(), p.id, "U1"); err != nil {
		t.Fatal(err)
	}
	if _, ok, _ := f.store.Get("U1"); ok || len(f.daemon.linked) != 0 {
		t.Fatalf("V's identity must never be linked to A: stored=%v binds=%+v", ok, f.daemon.linked)
	}
	if len(f.kc.revoked) != 1 || f.kc.revoked[0] != "rt-victim@example.com" {
		t.Fatalf("the victim's fresh offline token must be revoked, revoked=%v", f.kc.revoked)
	}
}

func TestSlackLink_CancelStoresNothingAndRevokes(t *testing.T) {
	f := newLinkFixture(t, nil)
	p := f.signIn(t, "U1", "owner@example.com")
	email, err := f.linker.CancelLink(context.Background(), p.id, "U1")
	if err != nil || email != "owner@example.com" {
		t.Fatalf("CancelLink = %q %v", email, err)
	}
	if _, ok, _ := f.store.Get("U1"); ok || len(f.daemon.linked) != 0 {
		t.Fatalf("cancel must store and bind nothing: stored=%v binds=%+v", ok, f.daemon.linked)
	}
	if len(f.kc.revoked) != 1 || f.kc.revoked[0] != "rt-owner@example.com" {
		t.Fatalf("cancel must revoke the fresh offline token, revoked=%v", f.kc.revoked)
	}
	// A confirm after the cancel finds nothing.
	if _, err := f.linker.ConfirmLink(context.Background(), p.id, "U1"); !errors.Is(err, ErrLinkConfirmUnknown) {
		t.Fatalf("confirm after cancel = %v, want ErrLinkConfirmUnknown", err)
	}
	if len(f.daemon.linked) != 0 {
		t.Fatal("confirm after cancel must not bind")
	}
}

func TestSlackLink_ConfirmExpiresAndRevokes(t *testing.T) {
	f := newLinkFixture(t, nil)
	p := f.signIn(t, "U1", "owner@example.com")
	f.now = f.now.Add(slackLinkConfirmTTL + time.Second)
	if _, err := f.linker.ConfirmLink(context.Background(), p.id, "U1"); !errors.Is(err, ErrLinkConfirmUnknown) {
		t.Fatalf("expired confirm = %v, want ErrLinkConfirmUnknown", err)
	}
	if _, ok, _ := f.store.Get("U1"); ok || len(f.daemon.linked) != 0 {
		t.Fatalf("expired confirm must bind nothing: stored=%v binds=%+v", ok, f.daemon.linked)
	}
	if len(f.kc.revoked) != 1 || f.kc.revoked[0] != "rt-owner@example.com" {
		t.Fatalf("expiry must revoke the offline token, revoked=%v", f.kc.revoked)
	}
	// Just inside the TTL still works.
	f.now = f.now.Add(time.Hour)
	p = f.signIn(t, "U1", "owner@example.com")
	f.now = f.now.Add(slackLinkConfirmTTL - time.Second)
	if _, err := f.linker.ConfirmLink(context.Background(), p.id, "U1"); err != nil {
		t.Fatalf("confirm inside the TTL: %v", err)
	}
}

// The access token from the code exchange lives 300s but a confirm may wait
// slackLinkConfirmTTL (10 min): a late Confirm must bind with a refreshed token
// and cache an expiry counted from that refresh.
func TestSlackLink_LateConfirmRefreshesTheAccessToken(t *testing.T) {
	f := newLinkFixture(t, nil)
	f.kc.rotate = true
	p := f.signIn(t, "U1", "owner@example.com")
	f.now = f.now.Add(6 * time.Minute) // past the 300s access lifespan, inside the confirm TTL
	if _, err := f.linker.ConfirmLink(context.Background(), p.id, "U1"); err != nil {
		t.Fatalf("late confirm: %v", err)
	}
	if len(f.daemon.bearers) != 1 || f.daemon.bearers[0] != "at-owner@example.com-r" {
		t.Fatalf("bind must carry the refreshed token, bearers=%v", f.daemon.bearers)
	}
	if f.kc.accessCount != 1 {
		t.Fatalf("refresh calls = %d, want 1", f.kc.accessCount)
	}
	// The rotated refresh token is the one stored, and nothing is revoked.
	link, _, _ := f.store.Get("U1")
	rt, err := f.linker.open("U1", link.Ciphertext)
	if err != nil || rt != "rt-owner@example.com+" {
		t.Fatalf("stored refresh token = %q %v, want the rotated one", rt, err)
	}
	if len(f.kc.revoked) != 0 {
		t.Fatalf("a good link must keep its offline token, revoked=%v", f.kc.revoked)
	}
	// The cached token is good for ~300s from the refresh, not stale at once.
	f.now = f.now.Add(200 * time.Second)
	tok, _, err := f.linker.AccessToken(context.Background(), "U1")
	if err != nil || tok != "at-owner@example.com-r" || f.kc.accessCount != 1 {
		t.Fatalf("AccessToken = %q %v (refreshes=%d), want the cached post-refresh token", tok, err, f.kc.accessCount)
	}
}

// A confirm inside the access lifespan binds with the original token, and the
// cached expiry counts from the exchange, not from the Confirm press.
func TestSlackLink_EarlyConfirmCachesExpiryFromTheExchange(t *testing.T) {
	f := newLinkFixture(t, nil)
	p := f.signIn(t, "U1", "owner@example.com")
	f.now = f.now.Add(200 * time.Second)
	if _, err := f.linker.ConfirmLink(context.Background(), p.id, "U1"); err != nil {
		t.Fatal(err)
	}
	if f.daemon.bearers[0] != "at-owner@example.com" || f.kc.accessCount != 0 {
		t.Fatalf("early confirm must not refresh: bearers=%v refreshes=%d", f.daemon.bearers, f.kc.accessCount)
	}
	// 280s after the exchange the token is inside the skew; computed from the
	// confirm time (80s old) it would wrongly still be served.
	f.now = f.now.Add(80 * time.Second)
	tok, _, err := f.linker.AccessToken(context.Background(), "U1")
	if err != nil || tok != "at-owner@example.com-r" || f.kc.accessCount != 1 {
		t.Fatalf("AccessToken = %q %v (refreshes=%d), want a refreshed token", tok, err, f.kc.accessCount)
	}
}

// If Keycloak refuses the refresh at Confirm (offline session gone), nothing is
// bound or stored and the person is told to start over.
func TestSlackLink_LateConfirmWithDeadSessionBindsNothing(t *testing.T) {
	f := newLinkFixture(t, nil)
	p := f.signIn(t, "U1", "owner@example.com")
	delete(f.kc.refreshOK, "rt-owner@example.com")
	f.now = f.now.Add(6 * time.Minute)
	_, err := f.linker.ConfirmLink(context.Background(), p.id, "U1")
	var refused *LinkRefusedError
	if !errors.As(err, &refused) {
		t.Fatalf("ConfirmLink = %v, want LinkRefusedError", err)
	}
	if _, ok, _ := f.store.Get("U1"); ok || len(f.daemon.linked) != 0 {
		t.Fatalf("must bind nothing: stored=%v binds=%+v", ok, f.daemon.linked)
	}
	if len(f.kc.revoked) != 1 {
		t.Fatalf("revoked=%v, want the token revoked", f.kc.revoked)
	}
}

func TestSlackLink_DoubleConfirmBindsOnce(t *testing.T) {
	f := newLinkFixture(t, nil)
	p := f.signIn(t, "U1", "owner@example.com")
	if _, err := f.linker.ConfirmLink(context.Background(), p.id, "U1"); err != nil {
		t.Fatal(err)
	}
	if _, err := f.linker.ConfirmLink(context.Background(), p.id, "U1"); !errors.Is(err, ErrLinkConfirmUnknown) {
		t.Fatalf("second confirm = %v, want ErrLinkConfirmUnknown", err)
	}
	if len(f.daemon.linked) != 1 {
		t.Fatalf("exactly one bind expected, got %d", len(f.daemon.linked))
	}
	if len(f.kc.revoked) != 0 {
		t.Fatalf("a good link must keep its offline token, revoked=%v", f.kc.revoked)
	}
	// Concurrent double press: still one bind.
	p = f.signIn(t, "U1", "owner@example.com")
	var wg sync.WaitGroup
	var okN, unknownN int32
	var mu sync.Mutex
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, err := f.linker.ConfirmLink(context.Background(), p.id, "U1")
			mu.Lock()
			defer mu.Unlock()
			if err == nil {
				okN++
			} else if errors.Is(err, ErrLinkConfirmUnknown) {
				unknownN++
			}
		}()
	}
	wg.Wait()
	if okN != 1 || unknownN != 7 {
		t.Fatalf("concurrent confirms: ok=%d unknown=%d, want 1 and 7", okN, unknownN)
	}
	if len(f.daemon.linked) != 2 {
		t.Fatalf("binds = %d, want 2 (one per signed-in flow)", len(f.daemon.linked))
	}
}

// Email is a label, not a credential: no Slack profile email, no email alias
// and no pinned slack_id decides a link. A mismatch is only a warning line on
// the prompt; a missing profile or a failing lookup still reaches Confirm.
func TestSlackLink_SlackEmailIsOnlyASoftWarning(t *testing.T) {
	cases := []struct {
		name        string
		setup       func(f *linkFixture)
		wantWarning bool
	}{
		{"matching email, no warning", func(f *linkFixture) {}, false},
		{"case and space ignored", func(f *linkFixture) { f.emails.emails["U1"] = "  Owner@Example.com " }, false},
		{"different email warns", func(f *linkFixture) { f.emails.emails["U1"] = "someone-else@example.com" }, true},
		{"slack profile has no email", func(f *linkFixture) { f.emails.emails["U1"] = "" }, false},
		{"unknown slack user", func(f *linkFixture) { delete(f.emails.emails, "U1") }, false},
		{"lookup error", func(f *linkFixture) { f.emails.err = errors.New("slack down") }, false},
		{"no resolver", func(f *linkFixture) { f.linker.SetEmailResolver(nil) }, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newLinkFixture(t, nil)
			tc.setup(f)
			p := f.signIn(t, "U1", "owner@example.com")
			if (p.warning != "") != tc.wantWarning {
				t.Fatalf("warning = %q, want warning=%v", p.warning, tc.wantWarning)
			}
			if _, err := f.linker.ConfirmLink(context.Background(), p.id, "U1"); err != nil {
				t.Fatalf("a profile-email mismatch must not block Confirm: %v", err)
			}
			if _, ok, _ := f.store.Get("U1"); !ok || len(f.daemon.linked) != 1 {
				t.Fatalf("expected the link after Confirm: stored=%v binds=%d", ok, len(f.daemon.linked))
			}
		})
	}
}

// The confirm step cannot be skipped: no hook, or a prompt that cannot be
// delivered, refuses the flow and revokes the fresh offline token.
func TestSlackLink_NoConfirmPromptFailsClosed(t *testing.T) {
	for name, setup := range map[string]func(f *linkFixture){
		"no hook":            func(f *linkFixture) { f.linker.SetOnConfirm(nil) },
		"prompt undelivered": func(f *linkFixture) { f.hookErr = errors.New("slack down") },
	} {
		t.Run(name, func(t *testing.T) {
			f := newLinkFixture(t, nil)
			setup(f)
			state := f.start(t, "U1")
			f.kc.codes["code-owner@example.com"] = f.verifierFor(state)
			if resp := f.callback(t, state, "code-owner@example.com"); resp.StatusCode < 500 {
				t.Fatalf("status %d, want a 5xx refusal", resp.StatusCode)
			}
			if _, ok, _ := f.store.Get("U1"); ok || len(f.daemon.linked) != 0 {
				t.Fatalf("nothing may be bound or stored: stored=%v binds=%+v", ok, f.daemon.linked)
			}
			if len(f.kc.revoked) != 1 || f.kc.revoked[0] != "rt-owner@example.com" {
				t.Fatalf("the fresh offline token must be revoked, revoked=%v", f.kc.revoked)
			}
		})
	}
}

// Abandoned prompts do not pile up forever holding offline tokens: the next
// callback after the TTL sweeps (and revokes) them.
func TestSlackLink_AbandonedConfirmIsSweptAndRevoked(t *testing.T) {
	f := newLinkFixture(t, nil)
	f.signIn(t, "U1", "owner@example.com")
	f.now = f.now.Add(slackLinkConfirmTTL + time.Second)
	f.signIn(t, "U1", "owner@example.com")
	deadline := time.Now().Add(2 * time.Second)
	for {
		f.kc.mu.Lock()
		n := len(f.kc.revoked)
		f.kc.mu.Unlock()
		if n == 1 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("the abandoned prompt's token was not revoked (revoked=%d)", n)
		}
		time.Sleep(10 * time.Millisecond)
	}
	f.linker.mu.Lock()
	n := len(f.linker.confirms)
	f.linker.mu.Unlock()
	if n != 1 {
		t.Fatalf("pending confirms = %d, want only the fresh one", n)
	}
}

func TestSlackLink_IDTokenFromAnotherIssuerOrClientIsRefused(t *testing.T) {
	f := newLinkFixture(t, nil)
	bad := func(claims map[string]any) {
		raw, _ := json.Marshal(claims)
		if _, err := f.linker.parseIDToken("h." + base64.RawURLEncoding.EncodeToString(raw) + ".s"); err == nil {
			t.Fatalf("id_token %v must be refused", claims)
		}
	}
	bad(map[string]any{"iss": "https://evil", "sub": "s", "email": "a@b", "aud": "gardenio-slack-link"})
	bad(map[string]any{"iss": f.kc.issuer, "sub": "s", "email": "a@b", "aud": "gardenio-cli"})
	bad(map[string]any{"iss": f.kc.issuer, "sub": "s", "email": "a@b", "aud": []any{"gardenio-slack-link"}, "azp": "gardenio-cli"})
	bad(map[string]any{"iss": f.kc.issuer, "sub": "s", "email": "a@b", "aud": "gardenio-slack-link", "exp": f.now.Add(-time.Minute).Unix()})
	bad(map[string]any{"iss": f.kc.issuer, "sub": "s", "aud": "gardenio-slack-link"})
	if c, err := f.linker.parseIDToken(f.kc.idToken("Owner@Example.com")); err != nil || c.Email != "owner@example.com" {
		t.Fatalf("good token refused: %+v %v", c, err)
	}
}

// TestSlackLink_IDTokenEmailMustBeVerified: the linked identity is the email
// claim, so a Keycloak user whose unverified email names someone else must not
// be able to link a Slack account to that person (gd-wVdGvh6VbB).
func TestSlackLink_IDTokenEmailMustBeVerified(t *testing.T) {
	f := newLinkFixture(t, nil)
	tok := func(extra map[string]any) string {
		claims := map[string]any{"iss": f.kc.issuer, "sub": "s", "email": "victim@example.com", "aud": f.kc.clientID}
		for k, v := range extra {
			claims[k] = v
		}
		raw, _ := json.Marshal(claims)
		return "h." + base64.RawURLEncoding.EncodeToString(raw) + ".s"
	}
	for name, extra := range map[string]map[string]any{
		"absent":       nil,
		"false":        {"email_verified": false},
		"string false": {"email_verified": "false"},
		"null":         {"email_verified": nil},
		"number":       {"email_verified": 1},
	} {
		if _, err := f.linker.parseIDToken(tok(extra)); err == nil {
			t.Errorf("email_verified %s: id_token must be refused", name)
		}
	}
	for name, v := range map[string]any{"true": true, "string true": "true"} {
		if c, err := f.linker.parseIDToken(tok(map[string]any{"email_verified": v})); err != nil || c.Email != "victim@example.com" {
			t.Errorf("email_verified %s: good token refused: %+v %v", name, c, err)
		}
	}
}

func TestSlackLink_RevokedAtKeycloakDropsLink(t *testing.T) {
	f := newLinkFixture(t, nil)
	f.link(t, "U1", "owner@example.com")
	// Admin revokes the consent / offline session in Keycloak.
	delete(f.kc.refreshOK, "rt-owner@example.com")
	f.now = f.now.Add(10 * time.Minute)
	_, _, err := f.linker.AccessToken(context.Background(), "U1")
	if !errors.Is(err, ErrSlackNotLinked) {
		t.Fatalf("revoked refresh must read as not linked, got %v", err)
	}
	if _, ok, _ := f.store.Get("U1"); ok {
		t.Fatal("revoked link must be dropped from the store")
	}
	// A Keycloak outage is NOT a revoke: the link stays, the write fails closed.
	f.link(t, "U1", "owner@example.com")
	f.now = f.now.Add(10 * time.Minute)
	f.kc.srv.Close()
	_, _, err = f.linker.AccessToken(context.Background(), "U1")
	if err == nil || errors.Is(err, ErrSlackNotLinked) {
		t.Fatalf("outage must be an error, not a drop: %v", err)
	}
	if _, ok, _ := f.store.Get("U1"); !ok {
		t.Fatal("outage must keep the link")
	}
}

func TestSlackLink_UnlinkClearsBeadRevokesAndDeletes(t *testing.T) {
	f := newLinkFixture(t, nil)
	if email, err := f.linker.Unlink(context.Background(), "U1"); err != nil || email != "" {
		t.Fatalf("unlink of unlinked user = %q %v", email, err)
	}
	f.link(t, "U1", "owner@example.com")

	email, err := f.linker.Unlink(context.Background(), "U1")
	if err != nil || email != "owner@example.com" {
		t.Fatalf("unlink = %q %v", email, err)
	}
	if len(f.daemon.unlinked) != 1 || f.daemon.unlinked[0].SlackUserID != "U1" {
		t.Fatalf("daemon unlink calls %+v", f.daemon.unlinked)
	}
	if len(f.kc.revoked) != 1 || f.kc.revoked[0] != "rt-owner@example.com" {
		t.Fatalf("revoked = %v", f.kc.revoked)
	}
	if _, ok, _ := f.store.Get("U1"); ok {
		t.Fatal("row must be deleted")
	}
	if _, _, err := f.linker.AccessToken(context.Background(), "U1"); !errors.Is(err, ErrSlackNotLinked) {
		t.Fatalf("after unlink the next write must be refused: %v", err)
	}
}

func TestSlackLink_FileStoreRoundTripAndKeyBinding(t *testing.T) {
	path := filepath.Join(t.TempDir(), "links.json")
	store, err := NewFileSlackLinkStore(path)
	if err != nil {
		t.Fatal(err)
	}
	f := newLinkFixture(t, store)
	f.link(t, "U1", "owner@example.com")

	// A fresh store on the same file sees the row; the ciphertext is bound to
	// the Slack id, so a row copied under another id does not decrypt.
	store2, err := NewFileSlackLinkStore(path)
	if err != nil {
		t.Fatal(err)
	}
	link, ok, _ := store2.Get("U1")
	if !ok {
		t.Fatal("row not persisted")
	}
	if _, err := f.linker.open("U1", link.Ciphertext); err != nil {
		t.Fatalf("open: %v", err)
	}
	if _, err := f.linker.open("U2", link.Ciphertext); err == nil {
		t.Fatal("ciphertext must not open under another slack id")
	}
	// A different key does not decrypt and the link is dropped, not used.
	other := make([]byte, 32)
	l2, _ := NewSlackLinker(SlackLinkConfig{PublicURL: "https://x.example", IssuerURL: f.kc.issuer, ClientID: "gardenio-slack-link", ClientSecret: "s3cret", Key: other}, store2, f.daemon, nil)
	if _, _, err := l2.AccessToken(context.Background(), "U1"); !errors.Is(err, ErrSlackNotLinked) {
		t.Fatalf("rotated key must read as not linked: %v", err)
	}
	if _, ok, _ := store2.Get("U1"); ok {
		t.Fatal("undecryptable row must be dropped")
	}
	if err := store2.Delete("nobody"); err != nil {
		t.Fatal(err)
	}
}

func TestNewSlackLinker_Validation(t *testing.T) {
	good := SlackLinkConfig{PublicURL: "https://x", IssuerURL: "https://kc/realms/g", ClientID: "c", ClientSecret: "s", Key: make([]byte, 32)}
	for name, mut := range map[string]func(*SlackLinkConfig){
		"http public url": func(c *SlackLinkConfig) { c.PublicURL = "http://x" },
		"no issuer":       func(c *SlackLinkConfig) { c.IssuerURL = "" },
		"no secret":       func(c *SlackLinkConfig) { c.ClientSecret = "" },
		"short key":       func(c *SlackLinkConfig) { c.Key = make([]byte, 16) },
	} {
		c := good
		mut(&c)
		if _, err := NewSlackLinker(c, newMemorySlackLinkStore(), &fakeLinkDaemon{}, nil); err == nil {
			t.Fatalf("%s must be refused", name)
		}
	}
	if _, err := NewSlackLinker(good, nil, &fakeLinkDaemon{}, nil); err == nil {
		t.Fatal("nil store must be refused")
	}
	if _, err := NewSlackLinker(good, newMemorySlackLinkStore(), &fakeLinkDaemon{}, nil); err != nil {
		t.Fatal(err)
	}
}

// Account linking end to end through the bot: an unlinked user is shown the
// link prompt and no write is sent; a linked user's write reaches the daemon
// with the person's OWN access token as the bearer and no edge-auth envelope.
func TestHandleInteraction_LinkedUserWritesWithOwnBearer(t *testing.T) {
	var (
		mu   sync.Mutex
		seen []http.Header
	)
	daemonSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		if r.Method != http.MethodGet {
			seen = append(seen, r.Header.Clone())
		}
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"dec-p1","type":"decision","status":"open"}`))
	}))
	defer daemonSrv.Close()
	real, err := beadsapi.New(beadsapi.Config{HTTPAddr: daemonSrv.URL, Token: "bridge-static-token"})
	if err != nil {
		t.Fatal(err)
	}
	real.SetPreferUserBearerForWrites(true)

	f := newLinkFixture(t, nil)
	f.emails.emails[ownerSlackID] = ownerEmail
	f.link(t, ownerSlackID, ownerEmail)

	bot, mock, ec := personBot(t, nil)
	bot.daemon = real
	bot.userDir = NewUserDirectory(mock, slog.Default())
	bot.SetSlackLinker(f.linker)

	bot.handleInteraction(context.Background(), dismissCallback(ownerSlackID))
	bot.handleInteraction(context.Background(), dismissCallback("USTRANGER"))

	mu.Lock()
	defer mu.Unlock()
	if len(seen) != 1 {
		t.Fatalf("daemon saw %d writes, want exactly the linked owner's one", len(seen))
	}
	h := seen[0]
	if h.Get("Authorization") != "Bearer at-"+ownerEmail {
		t.Errorf("bearer = %q, want the person's own access token", h.Get("Authorization"))
	}
	if h.Get("X-Edge-Auth-Version") != "" || h.Get("X-Edge-Auth-Sig-V2") != "" || h.Get("X-Edge-Auth-Operator") != "" {
		t.Errorf("linked write must carry no edge-auth envelope: %v", h)
	}
	if h.Get("X-Operated-By") != "oidc:"+ownerEmail {
		t.Errorf("operator hint = %q", h.Get("X-Operated-By"))
	}
	msgs := ec.all()
	if len(msgs) != 1 || msgs[0].user != "USTRANGER" || !strings.Contains(msgs[0].text, "Link your gardenio account") {
		t.Fatalf("ephemeral = %+v, want one link prompt to USTRANGER", msgs)
	}
}

// slackCapture records chat.postMessage / chat.postEphemeral calls.
type slackCapture struct {
	srv *httptest.Server
	mu  sync.Mutex
	got []url.Values
}

func newSlackCapture(t *testing.T) *slackCapture {
	t.Helper()
	c := &slackCapture{}
	c.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		form := url.Values{"_method": {r.URL.Path[strings.LastIndex(r.URL.Path, "/")+1:]}}
		for k, v := range r.Form {
			form[k] = v
		}
		c.mu.Lock()
		c.got = append(c.got, form)
		c.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"ok":true,"message_ts":"1.2","channel":"D1"}`))
	}))
	t.Cleanup(c.srv.Close)
	return c
}

// promptDMs are the chat.postMessage calls that carry the confirm buttons.
func (c *slackCapture) promptDMs() []url.Values {
	var out []url.Values
	for _, v := range c.calls("chat.postMessage") {
		if v.Get("blocks") != "" {
			out = append(out, v)
		}
	}
	return out
}

func (c *slackCapture) calls(method string) []url.Values {
	c.mu.Lock()
	defer c.mu.Unlock()
	var out []url.Values
	for _, v := range c.got {
		if v.Get("_method") == method {
			out = append(out, v)
		}
	}
	return out
}

var confirmValueRE = regexp.MustCompile(`"value":"([^"]+)"`)

func confirmIDFromDM(t *testing.T, dm url.Values) string {
	t.Helper()
	m := confirmValueRE.FindStringSubmatch(dm.Get("blocks"))
	if m == nil {
		t.Fatalf("no button value in %s", dm.Get("blocks"))
	}
	return m[1]
}

func linkConfirmCallback(user, actionID, confirmID string) slack.InteractionCallback {
	return slack.InteractionCallback{
		Type: slack.InteractionTypeBlockActions,
		User: slack.User{ID: user},
		ActionCallback: slack.ActionCallbacks{
			BlockActions: []*slack.BlockAction{{ActionID: actionID, Value: confirmID}},
		},
	}
}

// The confirm prompt is DM'd to the Slack user from the link state, and a
// Confirm / Cancel press from a user who is NOT linked yet reaches the linker
// (it must not be answered with the link prompt).
func TestBot_SlackLinkConfirmFlowThroughInteractions(t *testing.T) {
	sc := newSlackCapture(t)
	bot := newTestBot(newMockDaemon(), sc.srv)
	f := newLinkFixture(t, nil)
	bot.SetSlackLinker(f.linker)

	// Callback -> prompt DM'd to the starter U1 with Confirm and Cancel buttons.
	state := f.start(t, "U1")
	f.kc.codes["code-owner@example.com"] = f.verifierFor(state)
	if resp := f.callback(t, state, "code-owner@example.com"); resp.StatusCode != http.StatusOK {
		t.Fatalf("callback status %d", resp.StatusCode)
	}
	dms := sc.promptDMs()
	if len(dms) != 1 || dms[0].Get("channel") != "U1" {
		t.Fatalf("expected one DM to U1, got %v", dms)
	}
	blocks := dms[0].Get("blocks")
	for _, want := range []string{"owner@example.com", slackLinkConfirmActionID, slackLinkCancelActionID} {
		if !strings.Contains(blocks, want) {
			t.Fatalf("prompt blocks lack %q: %s", want, blocks)
		}
	}
	id := confirmIDFromDM(t, dms[0])

	// U1 is not linked, yet the press is handled (no link prompt ephemeral).
	// Another user pressing it changes nothing.
	bot.handleInteraction(context.Background(), linkConfirmCallback("UOTHER", slackLinkConfirmActionID, id))
	if _, ok, _ := f.store.Get("U1"); ok || len(f.daemon.linked) != 0 {
		t.Fatal("a press by another Slack user must not link")
	}
	bot.handleInteraction(context.Background(), linkConfirmCallback("U1", slackLinkConfirmActionID, id))
	if email, ok := f.linker.Linked("U1"); !ok || email != "owner@example.com" {
		t.Fatalf("U1's confirm should link, Linked=%q %v", email, ok)
	}
	// Second press: nothing more is bound, and the user is told it is stale.
	bot.handleInteraction(context.Background(), linkConfirmCallback("U1", slackLinkConfirmActionID, id))
	if len(f.daemon.linked) != 1 {
		t.Fatalf("double confirm must bind once, binds=%d", len(f.daemon.linked))
	}
	for _, e := range sc.calls("chat.postEphemeral") {
		if strings.Contains(e.Get("text"), "Link your gardenio account") {
			t.Fatalf("a confirm press must never be answered with the link prompt: %v", e)
		}
	}

	// Cancel path.
	state = f.start(t, "U2")
	f.kc.codes["code-other@example.com"] = f.verifierFor(state)
	if resp := f.callback(t, state, "code-other@example.com"); resp.StatusCode != http.StatusOK {
		t.Fatalf("callback status %d", resp.StatusCode)
	}
	dms = sc.promptDMs()
	if len(dms) != 2 || dms[1].Get("channel") != "U2" {
		t.Fatalf("expected a second DM to U2, got %v", dms)
	}
	bot.handleInteraction(context.Background(), linkConfirmCallback("U2", slackLinkCancelActionID, confirmIDFromDM(t, dms[1])))
	if _, ok := f.linker.Linked("U2"); ok {
		t.Fatal("cancel must not link")
	}
	if len(f.kc.revoked) != 1 || f.kc.revoked[0] != "rt-other@example.com" {
		t.Fatalf("cancel must revoke the offline token, revoked=%v", f.kc.revoked)
	}
}
