package main

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	authorizationv1 "k8s.io/api/authorization/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"

	"gardenio/controller/internal/beadsapi"
	"gardenio/controller/internal/codexauth"
	"gardenio/controller/internal/coopmux"
)

const (
	kTestNS      = "gardenio"
	kTestBase    = "gardenio-codex-auth"
	kTestSession = "gardenio-codex-auth-session"
)

var kNow = time.Date(2026, 10, 15, 12, 0, 0, 0, time.UTC)

func kJWT(exp time.Time) string {
	enc := func(s string) string { return base64.RawURLEncoding.EncodeToString([]byte(s)) }
	return enc(`{"alg":"none"}`) + "." + enc(fmt.Sprintf(`{"exp":%d}`, exp.Unix())) + ".sig"
}

func kAuthDoc(access, refresh string, lastRefresh time.Time) []byte {
	b, _ := json.Marshal(map[string]any{
		"auth_mode":    "chatgpt",
		"tokens":       map[string]any{"id_token": "id", "access_token": access, "refresh_token": refresh, "account_id": "acct-1"},
		"last_refresh": lastRefresh.UTC().Format(time.RFC3339Nano),
	})
	return b
}

type kFakeBeads struct {
	mu       sync.Mutex
	accounts []codexAccount
	updates  []map[string]string
	created  []beadsapi.CreateBeadRequest
	lists    int
}

func (f *kFakeBeads) ListCodexAccounts(context.Context) ([]codexAccount, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.lists++
	return append([]codexAccount(nil), f.accounts...), nil
}
func (f *kFakeBeads) UpdateFields(_ context.Context, _ string, fields map[string]string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.updates = append(f.updates, fields)
	return nil
}
func (f *kFakeBeads) CreateBead(_ context.Context, req beadsapi.CreateBeadRequest) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.created = append(f.created, req)
	return "gd-esc", nil
}
func (f *kFakeBeads) merged() map[string]string {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := map[string]string{}
	for _, u := range f.updates {
		for k, v := range u {
			out[k] = v
		}
	}
	return out
}

type kFakeOAuth struct {
	calls  []string
	result codexauth.Tokens
	err    error
	onCall func()
	onCtx  func(context.Context)
}

func (f *kFakeOAuth) Refresh(ctx context.Context, rt string) (codexauth.Tokens, error) {
	f.calls = append(f.calls, rt)
	if f.onCtx != nil {
		f.onCtx(ctx)
	}
	if f.onCall != nil {
		f.onCall()
	}
	return f.result, f.err
}

type kFakeUsage struct {
	u     codexauth.Usage
	err   error
	calls int
}

func (f *kFakeUsage) Read(context.Context, string, string) (codexauth.Usage, error) {
	f.calls++
	return f.u, f.err
}

type kFakeMux struct {
	events *[]string
	pushed []coopmux.AccountSync
	err    error
}

func (f *kFakeMux) SyncSingleAccount(_ context.Context, a coopmux.AccountSync) error {
	if f.err != nil {
		return f.err
	}
	*f.events = append(*f.events, "mux")
	f.pushed = append(f.pushed, a)
	return nil
}

type kEnv struct {
	k      *codexKeeper
	beads  *kFakeBeads
	oauth  *kFakeOAuth
	usage  *kFakeUsage
	mux    *kFakeMux
	k8s    *fake.Clientset
	events []string
	allow  bool
	failUp map[string]error // secret name -> update error

	// A fake API server that enforces optimistic concurrency like the real one:
	// every Secret has a resourceVersion, Get returns it, and an Update carrying a
	// stale one is a 409 Conflict.
	rvs       map[string]int
	conflicts int
	// raceOnce runs once, inside the first controller-Secret Update, before the
	// resourceVersion check: a writer that slips in between the keeper's Get and
	// its Update.
	raceOnce func()
}

func (e *kEnv) rvOf(name string) string {
	if e.rvs[name] == 0 {
		return "1"
	}
	return strconv.Itoa(e.rvs[name])
}

// touch changes a Secret out-of-band (as the operator or another controller would),
// bumping its resourceVersion.
func (e *kEnv) touch(name string, mutate func(*corev1.Secret)) {
	s, err := e.k8s.CoreV1().Secrets(kTestNS).Get(context.Background(), name, metav1.GetOptions{})
	if err != nil {
		panic(err)
	}
	mutate(s)
	if _, err := e.k8s.CoreV1().Secrets(kTestNS).Update(context.Background(), s, metav1.UpdateOptions{}); err != nil {
		panic(err)
	}
}

func newKEnv(t *testing.T, authDoc []byte, fields map[string]string) *kEnv {
	t.Helper()
	e := &kEnv{allow: true, failUp: map[string]error{}, rvs: map[string]int{}}
	e.k8s = fake.NewSimpleClientset(&corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: kTestBase, Namespace: kTestNS},
		Data:       map[string][]byte{"auth.json": authDoc},
	}, &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: kTestSession, Namespace: kTestNS},
	})
	e.k8s.PrependReactor("create", "selfsubjectaccessreviews", func(a k8stesting.Action) (bool, runtime.Object, error) {
		rev := a.(k8stesting.CreateAction).GetObject().(*authorizationv1.SelfSubjectAccessReview)
		rev.Status.Allowed = e.allow
		return true, rev, nil
	})
	e.k8s.PrependReactor("get", "secrets", func(a k8stesting.Action) (bool, runtime.Object, error) {
		ga := a.(k8stesting.GetAction)
		obj, err := e.k8s.Tracker().Get(corev1.SchemeGroupVersion.WithResource("secrets"), ga.GetNamespace(), ga.GetName())
		if err != nil {
			return true, nil, err
		}
		s := obj.(*corev1.Secret).DeepCopy()
		s.ResourceVersion = e.rvOf(s.Name)
		return true, s, nil
	})
	e.k8s.PrependReactor("update", "secrets", func(a k8stesting.Action) (bool, runtime.Object, error) {
		s := a.(k8stesting.UpdateAction).GetObject().(*corev1.Secret)
		if err := e.failUp[s.Name]; err != nil {
			return true, nil, err
		}
		if f := e.raceOnce; f != nil && s.Name == kTestBase {
			e.raceOnce = nil
			f()
		}
		if s.ResourceVersion != e.rvOf(s.Name) {
			e.conflicts++
			return true, nil, apierrors.NewConflict(corev1.Resource("secrets"), s.Name, errors.New("the object has been modified"))
		}
		e.rvs[s.Name] = func() int { n, _ := strconv.Atoi(e.rvOf(s.Name)); return n + 1 }()
		s.ResourceVersion = e.rvOf(s.Name)
		e.events = append(e.events, "update:"+s.Name)
		return false, nil, nil
	})
	if fields == nil {
		fields = map[string]string{}
	}
	e.beads = &kFakeBeads{accounts: []codexAccount{{
		Name: "codex-main", BeadID: "gd-acct", Credential: "secret:" + kTestBase + "#auth.json", Model: "gpt-5-codex", Fields: fields,
	}}}
	e.oauth = &kFakeOAuth{}
	e.usage = &kFakeUsage{u: codexauth.Usage{Primary: &codexauth.Window{UsedFraction: 0.25}}}
	e.mux = &kFakeMux{events: &e.events}
	e.k = &codexKeeper{
		beads: e.beads, k8s: e.k8s, namespace: kTestNS, mux: e.mux, oauth: e.oauth, usage: e.usage,
		logger:   slog.New(slog.NewTextHandler(io.Discard, nil)),
		interval: time.Minute, now: func() time.Time { return kNow }, sleep: func(time.Duration) {},
		gate: &rotationGate{}, pending: map[string]*pendingWrite{}, pushed: map[string]string{},
	}
	return e
}

func (e *kEnv) secretAuth(t *testing.T, name string) *codexauth.Auth {
	t.Helper()
	s, err := e.k8s.CoreV1().Secrets(kTestNS).Get(context.Background(), name, metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	a, err := codexauth.Parse(s.Data["auth.json"])
	if err != nil {
		t.Fatalf("secret %s: %v", name, err)
	}
	return a
}

func (e *kEnv) tick() { e.k.tick(context.Background()) }

func dueDoc() []byte {
	return kAuthDoc(kJWT(kNow.Add(2*time.Hour)), "refresh-1", kNow.Add(-time.Hour))
}

func TestCodexKeeperRefreshPersistsFirstThenSessionThenMux(t *testing.T) {
	e := newKEnv(t, dueDoc(), nil)
	newAccess := kJWT(kNow.Add(240 * time.Hour))
	e.oauth.result = codexauth.Tokens{AccessToken: newAccess, RefreshToken: "refresh-2"}
	e.tick()

	if len(e.oauth.calls) != 1 || e.oauth.calls[0] != "refresh-1" {
		t.Fatalf("oauth calls: %v", e.oauth.calls)
	}
	want := []string{"update:" + kTestBase, "update:" + kTestSession, "mux"}
	if strings.Join(e.events, ",") != strings.Join(want, ",") {
		t.Fatalf("order: %v want %v", e.events, want)
	}
	real := e.secretAuth(t, kTestBase)
	if real.RefreshToken() != "refresh-2" || real.AccessToken() != newAccess {
		t.Fatalf("controller Secret not rotated")
	}
	sess := e.secretAuth(t, kTestSession)
	if sess.RefreshToken() != codexauth.PlaceholderRefreshToken || sess.AccessToken() != newAccess {
		t.Fatalf("session Secret must carry the new access token and a placeholder refresh token")
	}
	if len(e.mux.pushed) != 1 || e.mux.pushed[0].Provider != "openai" || e.mux.pushed[0].Name != "codex-main" ||
		strings.Contains(e.mux.pushed[0].Token, "refresh-2") || !strings.Contains(e.mux.pushed[0].Token, newAccess) {
		t.Fatalf("mux push wrong: %+v", e.mux.pushed)
	}
	f := e.beads.merged()
	if f["probe_auth_status"] != "ok" || f["throttle_status"] != "allowed" || f["utilization_5h"] != "0.2500" {
		t.Fatalf("bead fields: %v", f)
	}
}

func TestCodexKeeperNoRefreshWhenFreshButSyncsSessionOnce(t *testing.T) {
	e := newKEnv(t, kAuthDoc(kJWT(kNow.Add(100*time.Hour)), "refresh-1", kNow.Add(-time.Hour)), nil)
	e.tick()
	e.tick()
	if len(e.oauth.calls) != 0 {
		t.Fatalf("refreshed a fresh token: %v", e.oauth.calls)
	}
	if sess := e.secretAuth(t, kTestSession); sess.RefreshToken() != codexauth.PlaceholderRefreshToken {
		t.Fatal("session Secret not populated")
	}
	if len(e.mux.pushed) != 1 {
		t.Fatalf("unchanged document must be pushed once, got %d", len(e.mux.pushed))
	}
	updates := 0
	for _, ev := range e.events {
		if ev == "update:"+kTestSession {
			updates++
		}
	}
	if updates != 1 {
		t.Fatalf("session Secret rewritten %d times; want 1", updates)
	}
}

func TestCodexKeeperRejectionIsNeverRetriedAndFilesReloginOnce(t *testing.T) {
	e := newKEnv(t, dueDoc(), nil)
	e.oauth.err = &codexauth.RefreshError{Status: 401, Code: "refresh_token_reused", Permanent: true}
	e.tick()

	f := e.beads.merged()
	if f["probe_auth_status"] != "rejected" || f[fieldCodexAuthError] != "refresh_token_reused" || f[fieldCodexRejectedFP] == "" {
		t.Fatalf("rejection not recorded: %v", f)
	}
	if len(e.beads.created) != 1 || e.beads.created[0].Type != "escalation" ||
		!strings.Contains(e.beads.created[0].IdempotencyKey, "codex-relogin:codex-main:"+f[fieldCodexRejectedFP]) {
		t.Fatalf("escalation: %+v", e.beads.created)
	}
	if blob := string(e.beads.created[0].Fields) + e.beads.created[0].Title; strings.Contains(blob, "refresh-1") {
		t.Fatal("escalation leaked the refresh token")
	}

	// The daemon now reports the verdict back on the bead; later passes must not
	// call OpenAI again.
	e.beads.accounts[0].Fields = map[string]string{"probe_auth_status": "rejected", fieldCodexRejectedFP: f[fieldCodexRejectedFP]}
	e.tick()
	e.tick()
	if len(e.oauth.calls) != 1 {
		t.Fatalf("rejected token retried: %v", e.oauth.calls)
	}
	if len(e.mux.pushed) != 0 {
		t.Fatal("rejected account must not be pushed")
	}

	// Operator re-login: a different refresh token in the Secret clears it.
	s, _ := e.k8s.CoreV1().Secrets(kTestNS).Get(context.Background(), kTestBase, metav1.GetOptions{})
	s.Data["auth.json"] = kAuthDoc(kJWT(kNow.Add(200*time.Hour)), "relogin-token", kNow)
	if _, err := e.k8s.CoreV1().Secrets(kTestNS).Update(context.Background(), s, metav1.UpdateOptions{}); err != nil {
		t.Fatal(err)
	}
	e.beads.updates = nil
	e.tick()
	if len(e.oauth.calls) != 1 {
		t.Fatalf("a fresh login needed no refresh: %v", e.oauth.calls)
	}
	g := e.beads.merged()
	if g["probe_auth_status"] != "ok" || g[fieldCodexRejectedFP] != "" || g[fieldCodexAuthError] != "" {
		t.Fatalf("re-login did not clear the rejection: %v", g)
	}
}

func TestCodexKeeperTransientRefreshErrorDoesNotCondemn(t *testing.T) {
	e := newKEnv(t, dueDoc(), nil)
	e.oauth.err = &codexauth.RefreshError{Status: 503}
	e.tick()
	if got := e.beads.merged()["probe_auth_status"]; got == "rejected" {
		t.Fatal("transient error must not reject the account")
	}
	if len(e.beads.created) != 0 {
		t.Fatal("transient error must not escalate")
	}
	e.oauth.err = nil
	e.oauth.result = codexauth.Tokens{AccessToken: kJWT(kNow.Add(240 * time.Hour)), RefreshToken: "refresh-2"}
	e.tick()
	if len(e.oauth.calls) != 2 || e.secretAuth(t, kTestBase).RefreshToken() != "refresh-2" {
		t.Fatal("next pass must retry and succeed")
	}
}

func TestCodexKeeperHoldsRotationInMemoryWhenPersistFails(t *testing.T) {
	e := newKEnv(t, dueDoc(), nil)
	e.oauth.result = codexauth.Tokens{AccessToken: kJWT(kNow.Add(240 * time.Hour)), RefreshToken: "refresh-2"}
	e.failUp[kTestBase] = errors.New("forbidden")
	e.tick()

	if e.secretAuth(t, kTestBase).RefreshToken() != "refresh-1" {
		t.Fatal("persist should have failed")
	}
	if e.k.pending["codex-main"] == nil {
		t.Fatal("rotated document must be held in memory")
	}
	if len(e.beads.created) != 1 || !strings.Contains(e.beads.created[0].IdempotencyKey, "codex-persist-failed") {
		t.Fatalf("persist failure must escalate: %+v", e.beads.created)
	}
	if len(e.mux.pushed) != 0 {
		t.Fatal("nothing may be pushed before the rotation is persisted")
	}

	// Permission is fixed; the next pass writes the held document and must NOT
	// spend the (already rotated-away) refresh-1 again.
	delete(e.failUp, kTestBase)
	e.tick()
	if len(e.oauth.calls) != 1 {
		t.Fatalf("pending write was not used; oauth calls: %v", e.oauth.calls)
	}
	if e.secretAuth(t, kTestBase).RefreshToken() != "refresh-2" || e.k.pending["codex-main"] != nil {
		t.Fatal("held rotation not persisted")
	}
	if len(e.mux.pushed) != 1 {
		t.Fatal("persisted rotation must reach coopmux")
	}
}

func TestCodexKeeperDoesNotOverwriteAReLogin(t *testing.T) {
	e := newKEnv(t, dueDoc(), nil)
	e.oauth.result = codexauth.Tokens{AccessToken: kJWT(kNow.Add(240 * time.Hour)), RefreshToken: "refresh-2"}
	e.oauth.onCall = func() {
		s, _ := e.k8s.CoreV1().Secrets(kTestNS).Get(context.Background(), kTestBase, metav1.GetOptions{})
		s.Data["auth.json"] = kAuthDoc(kJWT(kNow.Add(500*time.Hour)), "operator-relogin", kNow)
		_, _ = e.k8s.CoreV1().Secrets(kTestNS).Update(context.Background(), s, metav1.UpdateOptions{})
	}
	e.tick()
	if got := e.secretAuth(t, kTestBase).RefreshToken(); got != "operator-relogin" {
		t.Fatalf("keeper overwrote a concurrent re-login with %q", got)
	}
	if e.k.pending["codex-main"] != nil {
		t.Fatal("superseded rotation must be dropped, not held")
	}
}

func TestCodexKeeperRefusesToRotateWithoutWritePermission(t *testing.T) {
	e := newKEnv(t, dueDoc(), nil)
	e.allow = false
	e.tick()
	if len(e.oauth.calls) != 0 {
		t.Fatal("rotating a token we cannot persist destroys the login")
	}
	if e.secretAuth(t, kTestBase).RefreshToken() != "refresh-1" {
		t.Fatal("Secret changed without permission")
	}
}

func TestCodexKeeperUsageFailsOpen(t *testing.T) {
	e := newKEnv(t, kAuthDoc(kJWT(kNow.Add(100*time.Hour)), "refresh-1", kNow), map[string]string{"probe_auth_status": "ok", "throttle_status": "allowed"})
	e.usage.err = errors.New("wham/usage: HTTP 500")
	e.tick()
	f := e.beads.merged()
	if _, touched := f["throttle_status"]; touched {
		t.Fatalf("failed usage read must not rewrite throttle state: %v", f)
	}
	if len(e.mux.pushed) != 1 {
		t.Fatal("usage failure must not stop the credential flow")
	}
}

func TestCodexKeeperLimitReachedMarksThrottledAndExhausted(t *testing.T) {
	e := newKEnv(t, kAuthDoc(kJWT(kNow.Add(100*time.Hour)), "refresh-1", kNow), map[string]string{"probe_auth_status": "ok"})
	e.usage.u = codexauth.Usage{
		Primary:      &codexauth.Window{UsedFraction: 1.0, ResetAt: kNow.Add(2 * time.Hour)},
		Secondary:    &codexauth.Window{UsedFraction: 0.5, ResetAt: kNow.Add(72 * time.Hour)},
		LimitReached: true,
	}
	e.tick()
	f := e.beads.merged()
	if f["throttle_status"] != "rejected" || f["status"] != "exhausted" || f["representative_claim"] != "five_hour" {
		t.Fatalf("fields: %v", f)
	}
	if f["throttle_reset_5h_at"] != kNow.Add(2*time.Hour).Format(time.RFC3339) || f["throttle_reset_7d_at"] == "" ||
		f["throttle_reset_at"] != f["throttle_reset_5h_at"] {
		t.Fatalf("reset fields: %v", f)
	}
}

func TestCodexKeeperSkipsDisabledAndCredentiallessAndMalformed(t *testing.T) {
	e := newKEnv(t, dueDoc(), nil)
	e.beads.accounts = []codexAccount{
		{Name: "a", BeadID: "1", Credential: "secret:" + kTestBase, Fields: map[string]string{"status": "disabled"}},
		{Name: "b", BeadID: "2", Credential: "", Fields: map[string]string{}},
		{Name: "c", BeadID: "3", Credential: "env:FOO", Fields: map[string]string{}},
		{Name: "d", BeadID: "4", Credential: "secret:" + kTestBase + "#token", Fields: map[string]string{}},
	}
	e.tick()
	if len(e.oauth.calls) != 0 || len(e.beads.updates) != 0 {
		t.Fatal("keeper acted on an account it must skip")
	}
}

func TestCodexKeeperMissingSecretIsQuiet(t *testing.T) {
	e := newKEnv(t, dueDoc(), nil)
	e.beads.accounts[0].Credential = "secret:does-not-exist#auth.json"
	e.tick()
	if len(e.oauth.calls) != 0 {
		t.Fatal("no Secret, nothing to refresh")
	}
}

func TestUsageToProbeResult(t *testing.T) {
	r := usageToProbeResult(codexauth.Usage{
		Primary:   &codexauth.Window{UsedFraction: 0.3, ResetAt: kNow.Add(time.Hour)},
		Secondary: &codexauth.Window{UsedFraction: 0.9, ResetAt: kNow.Add(48 * time.Hour)},
	}, kNow)
	if r.Status != "allowed" || r.RepresentativeClaim != "seven_day" || r.Reset != kNow.Add(48*time.Hour).Unix() ||
		r.Utilization5h != 0.3 || r.Utilization7d != 0.9 {
		t.Fatalf("%+v", r)
	}
	if r.Throttled() {
		t.Fatal("90% weekly is not throttled")
	}
	if !usageToProbeResult(codexauth.Usage{LimitReached: true, Primary: &codexauth.Window{UsedFraction: 1}}, kNow).Throttled() {
		t.Fatal("limit_reached must throttle")
	}
}

// --- gd-6ggiM3hiV5 operator must-fixes (M1 shutdown, M2 held rotations) and hardening ---

func TestCodexKeeperRotationSurvivesContextCancelledMidRotation(t *testing.T) {
	e := newKEnv(t, dueDoc(), nil)
	e.oauth.result = codexauth.Tokens{AccessToken: kJWT(kNow.Add(240 * time.Hour)), RefreshToken: "refresh-2"}
	ctx, cancel := context.WithCancel(context.Background())
	var refreshCtx context.Context
	var refreshDeadline time.Time
	var busyDuring, refreshCtxLive bool
	e.oauth.onCall = func() {
		busyDuring = e.k.gate.busy()
		cancel() // SIGTERM / lost lease lands between "token spent" and "token written"
		refreshCtxLive = ctx.Err() != nil && refreshCtx.Err() == nil
	}
	e.oauth.onCtx = func(c context.Context) {
		refreshCtx = c
		refreshDeadline, _ = c.Deadline()
	}
	e.k.tick(ctx)
	if !refreshCtxLive {
		t.Fatal("the refresh context must outlive the caller's cancellation")
	}

	if got := e.secretAuth(t, kTestBase).RefreshToken(); got != "refresh-2" {
		t.Fatalf("rotation cancelled mid-flight was not persisted: Secret holds %q", got)
	}
	if e.k.pending["codex-main"] != nil {
		t.Fatal("persist landed; nothing should be held")
	}
	if !busyDuring {
		t.Fatal("the rotation gate must be held while a rotation is in flight")
	}
	if e.k.gate.busy() {
		t.Fatal("gate leaked")
	}
	if refreshDeadline.IsZero() || refreshDeadline.After(time.Now().Add(codexRotateTimeout+time.Second)) {
		t.Fatalf("refresh must run on its own bounded context, deadline=%v", refreshDeadline)
	}
}

func TestCodexKeeperPersistUsesAContextDetachedFromTheCaller(t *testing.T) {
	// A fake clientset ignores ctx, so prove the property at the seam: persistRotated
	// refuses a cancelled context (as client-go would), and rotate must therefore
	// have handed it a live one.
	e := newKEnv(t, dueDoc(), nil)
	dead, cancel := context.WithCancel(context.Background())
	cancel()
	doc := kAuthDoc(kJWT(kNow.Add(240*time.Hour)), "refresh-2", kNow)
	if err := e.k.persistRotated(dead, kTestBase, "", "refresh-1", doc); err == nil {
		t.Fatal("persistRotated must not write under a cancelled context")
	}
	if e.secretAuth(t, kTestBase).RefreshToken() != "refresh-1" {
		t.Fatal("cancelled persist wrote anyway")
	}

	e.oauth.result = codexauth.Tokens{AccessToken: kJWT(kNow.Add(240 * time.Hour)), RefreshToken: "refresh-2"}
	ctx, cancel2 := context.WithCancel(context.Background())
	e.oauth.onCall = cancel2
	e.k.tick(ctx)
	if e.secretAuth(t, kTestBase).RefreshToken() != "refresh-2" {
		t.Fatal("persist after a cancelled caller context failed")
	}
}

func TestCodexKeeperDoesNotStartARotationOnACancelledContext(t *testing.T) {
	e := newKEnv(t, dueDoc(), nil)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	e.k.tick(ctx)
	if len(e.oauth.calls) != 0 {
		t.Fatalf("started a rotation while shutting down: %v", e.oauth.calls)
	}
	if e.k.gate.busy() {
		t.Fatal("gate leaked")
	}
}

func TestRotationGateWait(t *testing.T) {
	g := &rotationGate{}
	if !g.Wait(time.Second) {
		t.Fatal("idle gate must drain at once")
	}
	g.begin()
	start := time.Now()
	if g.Wait(60 * time.Millisecond) {
		t.Fatal("busy gate reported drained")
	}
	if time.Since(start) < 50*time.Millisecond {
		t.Fatal("Wait returned before its bound")
	}
	go func() { time.Sleep(40 * time.Millisecond); g.end() }()
	if !g.Wait(2 * time.Second) {
		t.Fatal("gate did not drain after end")
	}
}

// The drain must outlast one whole rotation: a refresh and then a persist, each
// with its own budget, run detached from the caller's context.
func TestCodexShutdownDrainCoversTheRotationBudget(t *testing.T) {
	if codexShutdownDrain < 2*codexRotateTimeout {
		t.Fatalf("drain %s must cover the refresh budget plus the persist budget (%s each)", codexShutdownDrain, codexRotateTimeout)
	}
	if codexHandoverGrace < codexShutdownDrain {
		t.Fatalf("a new leader must idle at least one drain budget (%s), got %s", codexShutdownDrain, codexHandoverGrace)
	}
}

func TestCodexKeeperRunLoopIdlesForStartDelay(t *testing.T) {
	e := newKEnv(t, dueDoc(), nil)
	e.k.startDelay = time.Second
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan struct{})
	// start is taken before RunLoop's timer exists, so elapsed bounds the time
	// the keeper has really idled from above.
	start := time.Now()
	go func() { e.k.RunLoop(ctx); close(done) }()

	time.Sleep(100 * time.Millisecond)
	e.beads.mu.Lock()
	early := e.beads.lists
	e.beads.mu.Unlock()
	// A loaded runner can overrun the whole delay before we look; a tick is
	// then legitimately due, so only a tick seen inside the delay is a failure.
	if elapsed := time.Since(start); elapsed >= e.k.startDelay {
		t.Logf("runner overran the %s start delay (%s elapsed); skipping the early check", e.k.startDelay, elapsed)
	} else if early != 0 {
		t.Fatalf("keeper ticked %d time(s) inside its start delay (%s elapsed)", early, elapsed)
	}
	deadline := time.Now().Add(5 * time.Second)
	for {
		e.beads.mu.Lock()
		n := e.beads.lists
		e.beads.mu.Unlock()
		if n > 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("keeper never ticked after its start delay")
		}
		time.Sleep(20 * time.Millisecond)
	}
	cancel()
	<-done
}

func TestCodexKeeperRunLoopStartDelayHonoursCancel(t *testing.T) {
	e := newKEnv(t, dueDoc(), nil)
	e.k.startDelay = time.Hour
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { e.k.RunLoop(ctx); close(done) }()
	cancel()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("RunLoop ignored cancellation during its start delay")
	}
	if e.beads.lists != 0 {
		t.Fatal("a cancelled keeper must not tick")
	}
}

func TestCodexKeeperDrainsHeldRotationBeforeSkippingDisabledAccount(t *testing.T) {
	e := newKEnv(t, dueDoc(), nil)
	e.oauth.result = codexauth.Tokens{AccessToken: kJWT(kNow.Add(240 * time.Hour)), RefreshToken: "refresh-2"}
	e.failUp[kTestBase] = errors.New("forbidden")
	e.tick()
	if e.k.pending["codex-main"] == nil {
		t.Fatal("setup: rotation should be held")
	}

	// The operator disables the account (and fixes RBAC). The held rotation must
	// still be written: the Secret holds a refresh token OpenAI already spent.
	delete(e.failUp, kTestBase)
	e.beads.accounts[0].Fields = map[string]string{"status": "disabled"}
	e.tick()
	if got := e.secretAuth(t, kTestBase).RefreshToken(); got != "refresh-2" {
		t.Fatalf("held rotation not written for a disabled account: %q", got)
	}
	if e.k.pending["codex-main"] != nil {
		t.Fatal("pending not cleared")
	}
	if len(e.oauth.calls) != 1 {
		t.Fatalf("spent token reused: %v", e.oauth.calls)
	}
}

func TestCodexKeeperDrainsHeldRotationWhenAccountNoLongerListed(t *testing.T) {
	e := newKEnv(t, dueDoc(), nil)
	e.oauth.result = codexauth.Tokens{AccessToken: kJWT(kNow.Add(240 * time.Hour)), RefreshToken: "refresh-2"}
	e.failUp[kTestBase] = errors.New("forbidden")
	e.tick()
	delete(e.failUp, kTestBase)
	e.beads.accounts = nil // bead closed, or the list is empty
	e.tick()
	if e.secretAuth(t, kTestBase).RefreshToken() != "refresh-2" || e.k.pending["codex-main"] != nil {
		t.Fatal("held rotation must be written even when the account is not listed")
	}
}

func TestCodexKeeperHeldRotationTargetsItsOwnSecretNotTheBead(t *testing.T) {
	e := newKEnv(t, dueDoc(), nil)
	e.oauth.result = codexauth.Tokens{AccessToken: kJWT(kNow.Add(240 * time.Hour)), RefreshToken: "refresh-2"}
	e.failUp[kTestBase] = errors.New("forbidden")
	e.tick()
	p := e.k.pending["codex-main"]
	if p == nil || p.secret != kTestBase || p.baseRV == "" {
		t.Fatalf("pending must record its Secret and base resourceVersion: %+v", p)
	}

	// The bead is re-pointed at a different Secret before the retry.
	other := kAuthDoc(kJWT(kNow.Add(100*time.Hour)), "other-refresh", kNow)
	if _, err := e.k8s.CoreV1().Secrets(kTestNS).Create(context.Background(), &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "other-codex-auth", Namespace: kTestNS},
		Data:       map[string][]byte{"auth.json": other},
	}, metav1.CreateOptions{}); err != nil {
		t.Fatal(err)
	}
	e.beads.accounts[0].Credential = "secret:other-codex-auth#auth.json"
	delete(e.failUp, kTestBase)
	e.tick()

	if got := e.secretAuth(t, kTestBase).RefreshToken(); got != "refresh-2" {
		t.Fatalf("held rotation was not written back to its own Secret: %q", got)
	}
	if got := e.secretAuth(t, "other-codex-auth").RefreshToken(); got != "other-refresh" {
		t.Fatalf("held rotation was written to the bead's new target: %q", got)
	}
}

func TestCodexKeeperAccountWithHeldRotationNeverSpendsTheDeadTokenAgain(t *testing.T) {
	e := newKEnv(t, dueDoc(), nil)
	e.oauth.result = codexauth.Tokens{AccessToken: kJWT(kNow.Add(240 * time.Hour)), RefreshToken: "refresh-2"}
	e.failUp[kTestBase] = errors.New("forbidden")
	e.tick()
	e.tick()
	e.tick()
	if len(e.oauth.calls) != 1 {
		t.Fatalf("a held rotation must block further refreshes of the same account: %v", e.oauth.calls)
	}
	if len(e.mux.pushed) != 0 {
		t.Fatal("nothing may be pushed while the rotation is unwritten")
	}
}

func TestCodexKeeperRealConflictRetriesOnTheFreshVersion(t *testing.T) {
	e := newKEnv(t, dueDoc(), nil)
	e.oauth.result = codexauth.Tokens{AccessToken: kJWT(kNow.Add(240 * time.Hour)), RefreshToken: "refresh-2"}
	// An unrelated writer touches the Secret (same login) while the refresh is in
	// flight: the keeper's conditional Update is a genuine 409.
	e.oauth.onCall = func() {
		e.touch(kTestBase, func(s *corev1.Secret) {
			s.Annotations = map[string]string{"unrelated": "change"}
		})
	}
	e.tick()
	if e.conflicts != 1 {
		t.Fatalf("expected exactly one 409 from the fake API server, got %d", e.conflicts)
	}
	if len(e.oauth.calls) != 1 {
		t.Fatalf("a conflict must not spend another refresh token: %v", e.oauth.calls)
	}
	if got := e.secretAuth(t, kTestBase).RefreshToken(); got != "refresh-2" {
		t.Fatalf("rotation lost after conflict: %q", got)
	}
	s, _ := e.k8s.CoreV1().Secrets(kTestNS).Get(context.Background(), kTestBase, metav1.GetOptions{})
	if s.Annotations["unrelated"] != "change" {
		t.Fatal("the unrelated writer's change was clobbered")
	}
}

func TestCodexKeeperConflictRaceWithAReLoginDropsTheRotation(t *testing.T) {
	e := newKEnv(t, dueDoc(), nil)
	e.oauth.result = codexauth.Tokens{AccessToken: kJWT(kNow.Add(240 * time.Hour)), RefreshToken: "refresh-2"}
	// The operator re-logs in after the keeper's last Get but before its Update.
	e.raceOnce = func() {
		// Straight to the object tracker: calling the clientset from inside one of its
		// own reactors would deadlock on the fake's lock.
		obj, err := e.k8s.Tracker().Get(corev1.SchemeGroupVersion.WithResource("secrets"), kTestNS, kTestBase)
		if err != nil {
			panic(err)
		}
		s := obj.(*corev1.Secret).DeepCopy()
		s.Data["auth.json"] = kAuthDoc(kJWT(kNow.Add(500*time.Hour)), "operator-relogin", kNow)
		n, _ := strconv.Atoi(e.rvOf(kTestBase))
		e.rvs[kTestBase] = n + 1
		if err := e.k8s.Tracker().Update(corev1.SchemeGroupVersion.WithResource("secrets"), s, kTestNS); err != nil {
			panic(err)
		}
	}
	e.tick()
	if e.conflicts != 1 {
		t.Fatalf("the stale-version write should have been rejected by the API server, conflicts=%d", e.conflicts)
	}
	if got := e.secretAuth(t, kTestBase).RefreshToken(); got != "operator-relogin" {
		t.Fatalf("keeper overwrote a re-login: %q", got)
	}
	if e.k.pending["codex-main"] != nil {
		t.Fatal("superseded rotation must be dropped")
	}
}

func TestCodexKeeperMinRefreshIntervalHoldsEvenWithUnreadableExpiry(t *testing.T) {
	e := newKEnv(t, kAuthDoc("opaque-not-a-jwt", "refresh-1", kNow.Add(-10*time.Minute)), nil)
	e.tick()
	e.tick()
	if len(e.oauth.calls) != 0 {
		t.Fatalf("refreshed a token refreshed 10 minutes ago: %v", e.oauth.calls)
	}
	// Past the floor, an unreadable expiry does refresh.
	e.k.now = func() time.Time { return kNow.Add(2 * time.Hour) }
	e.oauth.result = codexauth.Tokens{AccessToken: kJWT(kNow.Add(300 * time.Hour)), RefreshToken: "refresh-2"}
	e.tick()
	if len(e.oauth.calls) != 1 {
		t.Fatalf("expected one refresh once past the floor: %v", e.oauth.calls)
	}
}

func TestCodexKeeperSessionSecretAndMuxCarryOnlyWhitelistedFields(t *testing.T) {
	doc, _ := json.Marshal(map[string]any{
		"auth_mode":      "chatgpt",
		"OPENAI_API_KEY": "sk-should-not-reach-pods",
		"agent_identity": map[string]any{"k": "v"},
		"last_refresh":   kNow.Add(-time.Hour).UTC().Format(time.RFC3339Nano),
		"tokens": map[string]any{
			"id_token": "id", "access_token": kJWT(kNow.Add(100 * time.Hour)), "refresh_token": "real-refresh",
			"account_id": "acct-1", "future_secret": "nope",
		},
	})
	e := newKEnv(t, doc, nil)
	e.tick()
	s, _ := e.k8s.CoreV1().Secrets(kTestNS).Get(context.Background(), kTestSession, metav1.GetOptions{})
	for name, blob := range map[string]string{"session Secret": string(s.Data["auth.json"]), "mux push": e.mux.pushed[0].Token} {
		for _, bad := range []string{"sk-should-not-reach-pods", "OPENAI_API_KEY", "agent_identity", "real-refresh", "future_secret"} {
			if strings.Contains(blob, bad) {
				t.Errorf("%s leaked %q", name, bad)
			}
		}
		if !strings.Contains(blob, codexauth.PlaceholderRefreshToken) || !strings.Contains(blob, "acct-1") {
			t.Errorf("%s lost a whitelisted field: %s", name, blob)
		}
	}
	// The controller-side Secret is untouched by the whitelist.
	if !strings.Contains(string(e.k8sSecretData(t, kTestBase)), "OPENAI_API_KEY") {
		t.Fatal("controller-side document must keep unknown fields")
	}
}

func (e *kEnv) k8sSecretData(t *testing.T, name string) []byte {
	t.Helper()
	s, err := e.k8s.CoreV1().Secrets(kTestNS).Get(context.Background(), name, metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	return s.Data["auth.json"]
}

func TestCodexKeeperFailedUsageReadNeverMarksAuthOK(t *testing.T) {
	// Unknown account, usage down: no bead write at all.
	e := newKEnv(t, kAuthDoc(kJWT(kNow.Add(100*time.Hour)), "refresh-1", kNow), nil)
	e.usage.err = errors.New("wham/usage: HTTP 500")
	e.tick()
	if len(e.beads.updates) != 0 {
		t.Fatalf("failed usage read wrote to the bead: %v", e.beads.updates)
	}

	// Rejected account whose login was replaced, usage still down: the verdict is
	// not upgraded to ok on an unverified read.
	e2 := newKEnv(t, kAuthDoc(kJWT(kNow.Add(100*time.Hour)), "relogin", kNow),
		map[string]string{"probe_auth_status": "rejected", fieldCodexRejectedFP: "deadbeefdeadbeef"})
	e2.usage.err = errors.New("wham/usage: HTTP 500")
	e2.tick()
	if got := e2.beads.merged()["probe_auth_status"]; got == "ok" {
		t.Fatal("probe_auth_status=ok written without a successful usage read")
	}
	if len(e2.mux.pushed) != 1 {
		t.Fatal("a usage failure must not stop the credential flow")
	}
}

func TestCodexKeeperURLOverridesRequireHTTPSAllowlistedHost(t *testing.T) {
	k := &codexKeeper{logger: slog.New(slog.NewTextHandler(io.Discard, nil))}
	cases := []struct {
		val, want string
	}{
		{"", ""},
		{"https://auth.openai.com/oauth/token", "https://auth.openai.com/oauth/token"},
		{"http://auth.openai.com/oauth/token", ""},
		{"https://attacker.example/oauth/token", ""},
		{"http://127.0.0.1:8080/token", ""},
		{"https://auth.openai.com@attacker.example/x", ""},
	}
	for _, c := range cases {
		t.Setenv("CODEX_REFRESH_TOKEN_URL_OVERRIDE", c.val)
		if got := k.safeOverride("CODEX_REFRESH_TOKEN_URL_OVERRIDE", codexauth.TokenURLAllowedHosts); got != c.want {
			t.Errorf("override %q: got %q want %q", c.val, got, c.want)
		}
	}
	t.Setenv("CODEX_USAGE_URL_OVERRIDE", "https://chatgpt.com/backend-api/wham/usage")
	if k.safeOverride("CODEX_USAGE_URL_OVERRIDE", codexauth.UsageURLAllowedHosts) == "" {
		t.Error("the chatgpt.com usage URL must be accepted")
	}
	t.Setenv("CODEX_USAGE_URL_OVERRIDE", "https://auth.openai.com/x")
	if k.safeOverride("CODEX_USAGE_URL_OVERRIDE", codexauth.UsageURLAllowedHosts) != "" {
		t.Error("the usage override must not accept the token host")
	}
}
