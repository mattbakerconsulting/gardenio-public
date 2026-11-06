package main

// codex_login_broker.go — controller-side ChatGPT-plan device login (ADR 0007,
// option A). An operator presses "Start device login" on the keeper's re-login
// escalation; the leader controller asks OpenAI for a device code, hands it only
// to that operator-authenticated caller, polls for the browser approval, and
// writes the resulting auth.json straight into the account's Secret through the
// same resourceVersion-CAS discipline the keeper uses. The refresh token exists
// only in this process and the Secret: never in a pod, bead, log or error.

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"gardenio/controller/internal/codexauth"
	"gardenio/controller/internal/podmanager"
)

const (
	codexLoginPersistTimeout = 30 * time.Second
	// codexLoginKeepFlow is how long a finished flow's (non-secret) outcome is
	// kept for status reads.
	codexLoginKeepFlow = 30 * time.Minute

	codexDeviceLoginAction = "codex_device_login"
)

// Flow states reported by Status. None of them carries a secret.
const (
	loginPending   = "pending"
	loginSucceeded = "succeeded"
	loginFailed    = "failed"
	loginExpired   = "expired"
	loginNone      = "none"
)

// codexLoginDevice is the slice of codexauth.DeviceLogin the broker uses.
type codexLoginDevice interface {
	Start(ctx context.Context) (codexauth.DeviceCode, error)
	Poll(ctx context.Context, dc codexauth.DeviceCode) (codexauth.Tokens, error)
}

// codexEscalations records the outcome on the escalation the operator pressed
// Start on. Non-secret facts only. Resolve closes it through the escalation
// result verb: the daemon refuses the generic bead close for an open escalation
// (409, escalationBlocksGenericClose).
type codexEscalations interface {
	Comment(ctx context.Context, beadID, text string) error
	Resolve(ctx context.Context, beadID string, result map[string]any) error
}

// codexLoginError carries an HTTP status for the handler. Messages are safe to return.
type codexLoginError struct {
	Status int
	Msg    string
}

func (e *codexLoginError) Error() string { return e.Msg }

func loginErr(status int, format string, a ...any) error {
	return &codexLoginError{Status: status, Msg: fmt.Sprintf(format, a...)}
}

// codexLoginRequest is what the operator-authenticated caller supplies.
type codexLoginRequest struct {
	Account            string `json:"account"`
	EscalationID       string `json:"escalation_id,omitempty"`
	StartedBy          string `json:"started_by,omitempty"` // audit only
	AllowAccountChange bool   `json:"allow_account_change,omitempty"`
}

// codexLoginStart is the only response that carries the user code.
type codexLoginStart struct {
	VerificationURL string    `json:"verification_url"`
	UserCode        string    `json:"user_code"`
	ExpiresAt       time.Time `json:"expires_at"`
	Reused          bool      `json:"reused,omitempty"`
}

// codexLoginStatus is the non-secret view of a flow.
type codexLoginStatus struct {
	State       string    `json:"state"`
	ExpiresAt   time.Time `json:"expires_at,omitempty"`
	ErrorCode   string    `json:"error_code,omitempty"`
	Fingerprint string    `json:"fingerprint,omitempty"`
}

type codexLoginFlow struct {
	req    codexLoginRequest
	secret string
	dc     codexauth.DeviceCode // in memory only
	state  string
	errc   string
	fp     string
	doneAt time.Time
}

type codexLoginBroker struct {
	k      *codexKeeper
	device codexLoginDevice
	esc    codexEscalations
	logger *slog.Logger
	// run is the controller's lifetime context: shutdown cancels polling.
	run context.Context

	mu    sync.Mutex
	flows map[string]*codexLoginFlow // account name -> latest flow
}

func newCodexLoginBroker(run context.Context, k *codexKeeper, esc codexEscalations) *codexLoginBroker {
	issuer := k.safeOverride("CODEX_AUTH_ISSUER_OVERRIDE", codexauth.IssuerAllowedHosts)
	return &codexLoginBroker{
		k: k, device: codexauth.NewDeviceLogin(issuer), esc: esc, run: run,
		logger: k.logger.With("component", "codex-login-broker"),
		flows:  map[string]*codexLoginFlow{},
	}
}

// Start begins (or re-shows) a device login for req.Account.
func (b *codexLoginBroker) Start(ctx context.Context, req codexLoginRequest) (codexLoginStart, error) {
	acct, ok := b.findAccount(ctx, req.Account)
	if !ok {
		return codexLoginStart{}, loginErr(http.StatusNotFound, "no provider=openai account %q", req.Account)
	}
	base, err := codexAuthSecretName(acct.Credential)
	if err != nil {
		return codexLoginStart{}, loginErr(http.StatusUnprocessableEntity, "account %q has no usable codex credential", req.Account)
	}

	b.mu.Lock()
	defer b.mu.Unlock()
	b.gcLocked()
	if f := b.flows[acct.Name]; f != nil && f.state == loginPending && b.k.now().Before(f.dc.ExpiresAt) {
		return codexLoginStart{VerificationURL: f.dc.VerificationURL, UserCode: f.dc.UserCode, ExpiresAt: f.dc.ExpiresAt, Reused: true}, nil
	}

	cur, _, err := b.currentLogin(ctx, base)
	if err != nil {
		return codexLoginStart{}, err
	}
	if err := b.needsLogin(acct, cur); err != nil {
		return codexLoginStart{}, err
	}
	if !b.k.canWrite(ctx, base) {
		return codexLoginStart{}, loginErr(http.StatusServiceUnavailable, "the controller cannot update Secret %s (RBAC not applied?)", base)
	}

	sctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	dc, err := b.device.Start(sctx)
	if err != nil {
		b.logger.Warn("codex device login: could not obtain a device code", "account", acct.Name, "error", err)
		return codexLoginStart{}, loginErr(http.StatusBadGateway, "OpenAI refused the device code request")
	}
	f := &codexLoginFlow{req: req, secret: base, dc: dc, state: loginPending}
	b.flows[acct.Name] = f
	b.logger.Info("codex device login started", "account", acct.Name, "secret", base,
		"started_by", req.StartedBy, "escalation", req.EscalationID, "expires_at", dc.ExpiresAt.UTC().Format(time.RFC3339))
	go b.await(acct, f)
	return codexLoginStart{VerificationURL: dc.VerificationURL, UserCode: dc.UserCode, ExpiresAt: dc.ExpiresAt}, nil
}

// Status is the non-secret state of the latest flow for account.
func (b *codexLoginBroker) Status(account string) codexLoginStatus {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.gcLocked()
	f := b.flows[account]
	if f == nil {
		return codexLoginStatus{State: loginNone}
	}
	return codexLoginStatus{State: f.state, ExpiresAt: f.dc.ExpiresAt, ErrorCode: f.errc, Fingerprint: f.fp}
}

func (b *codexLoginBroker) gcLocked() {
	for name, f := range b.flows {
		if f.state != loginPending && b.k.now().Sub(f.doneAt) > codexLoginKeepFlow {
			delete(b.flows, name)
		}
	}
}

func (b *codexLoginBroker) findAccount(ctx context.Context, name string) (codexAccount, bool) {
	accts, err := b.k.beads.ListCodexAccounts(ctx)
	if err != nil {
		return codexAccount{}, false
	}
	for _, a := range accts {
		if a.Name == name {
			return a, true
		}
	}
	return codexAccount{}, false
}

// currentLogin reads the controller-side Secret. auth is nil when the Secret holds
// no usable login (empty or unparseable), which a bootstrap may fill.
func (b *codexLoginBroker) currentLogin(ctx context.Context, base string) (auth *codexauth.Auth, rv string, err error) {
	sec, gerr := b.k.k8s.CoreV1().Secrets(b.k.namespace).Get(ctx, base, metav1.GetOptions{})
	if apierrors.IsNotFound(gerr) {
		return nil, "", loginErr(http.StatusConflict, "Secret %s does not exist; it must be pre-created (empty auth.json) before a login can fill it", base)
	}
	if gerr != nil {
		return nil, "", loginErr(http.StatusBadGateway, "Secret %s unreadable", base)
	}
	if a, perr := codexauth.Parse(sec.Data[podmanager.CodexAuthFileName]); perr == nil {
		auth = a
	}
	return auth, sec.ResourceVersion, nil
}

// needsLogin refuses a login that would clobber a working one: it is only for a
// login the keeper has seen rejected (and still holds), or one that does not exist.
func (b *codexLoginBroker) needsLogin(acct codexAccount, cur *codexauth.Auth) error {
	if cur == nil {
		return nil
	}
	if acct.Fields["probe_auth_status"] == "rejected" && acct.Fields[fieldCodexRejectedFP] == cur.RefreshFingerprint() {
		return nil
	}
	return loginErr(http.StatusConflict, "account %q has a login the keeper has not found rejected; refusing to replace it", acct.Name)
}

// await polls for the approval and persists the result. It runs on the controller
// lifetime context, never the request's.
func (b *codexLoginBroker) await(acct codexAccount, f *codexLoginFlow) {
	log := b.logger.With("account", acct.Name)
	finish := func(state, code, fp string) {
		b.mu.Lock()
		f.state, f.errc, f.fp, f.doneAt = state, code, fp, b.k.now()
		b.mu.Unlock()
	}

	tr, err := b.device.Poll(b.run, f.dc)
	switch {
	case errors.Is(err, codexauth.ErrDeviceCodeExpired):
		log.Info("codex device login expired unapproved")
		b.report(acct, f, "Device login expired before it was approved. Press Start again when ready.", false, "")
		finish(loginExpired, "expired", "")
		return
	case err != nil:
		code := "poll_failed"
		var de *codexauth.DeviceLoginError
		if errors.As(err, &de) {
			code = fmt.Sprintf("%s_http_%d", de.Stage, de.Status)
			if de.Code != "" {
				code += "_" + de.Code
			}
		}
		log.Warn("codex device login failed", "error", err)
		b.report(acct, f, "Device login failed ("+code+"). Nothing was written.", false, "")
		finish(loginFailed, code, "")
		return
	}

	// Past this point the refresh token exists only here: gate shutdown on it.
	b.k.gate.begin()
	defer b.k.gate.end()
	fp, err := b.persist(acct, f, tr)
	if err != nil {
		var le *codexLoginError
		code := "persist_failed"
		if errors.As(err, &le) {
			code = le.Msg
		}
		log.Error("codex device login approved but the login was not stored; it was dropped", "error", err)
		b.report(acct, f, "Device login was approved but not stored ("+code+"). The login was discarded; start again.", false, "")
		finish(loginFailed, code, "")
		return
	}
	log.Info("codex device login stored", "secret", f.secret, "fingerprint", fp)
	// Report before finishing so a status read of "succeeded" implies the
	// escalation has already been updated.
	b.report(acct, f, fmt.Sprintf("New login stored in Secret %s (refresh fingerprint %s). The keeper picks it up on its next pass.", f.secret, fp), true, fp)
	finish(loginSucceeded, "", fp)
}

// persist validates the login and writes it with a resourceVersion CAS. The
// precondition is re-checked against the freshly read Secret, so a re-login by
// another route is never overwritten.
func (b *codexLoginBroker) persist(acct codexAccount, f *codexLoginFlow, tr codexauth.Tokens) (string, error) {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(b.run), codexLoginPersistTimeout)
	defer cancel()
	nauth, err := codexauth.NewAuth(tr, b.k.now())
	if err != nil {
		return "", loginErr(http.StatusBadGateway, "incomplete_login")
	}
	secrets := b.k.k8s.CoreV1().Secrets(b.k.namespace)
	sec, err := secrets.Get(ctx, f.secret, metav1.GetOptions{})
	if err != nil {
		return "", loginErr(http.StatusBadGateway, "secret_unreadable")
	}
	var cur *codexauth.Auth
	if a, perr := codexauth.Parse(sec.Data[podmanager.CodexAuthFileName]); perr == nil {
		cur = a
	}
	// Re-read the account so the rejected fingerprint is the current one.
	fresh := acct
	if a, ok := b.findAccount(ctx, acct.Name); ok {
		fresh = a
	}
	if err := b.needsLogin(fresh, cur); err != nil {
		return "", loginErr(http.StatusConflict, "superseded")
	}
	if cur != nil && !f.req.AllowAccountChange {
		if want, got := cur.AccountID(), nauth.AccountID(); want != "" && got != want {
			return "", loginErr(http.StatusConflict, "chatgpt_account_mismatch")
		}
	}
	doc, err := nauth.Marshal()
	if err != nil {
		return "", loginErr(http.StatusInternalServerError, "marshal_failed")
	}
	if sec.Data == nil {
		sec.Data = map[string][]byte{}
	}
	sec.Data[podmanager.CodexAuthFileName] = doc
	if _, err := secrets.Update(ctx, sec, metav1.UpdateOptions{}); err != nil {
		if apierrors.IsConflict(err) {
			return "", loginErr(http.StatusConflict, "superseded")
		}
		return "", loginErr(http.StatusBadGateway, "update_failed")
	}
	return nauth.RefreshFingerprint(), nil
}

// report records a non-secret outcome on the escalation and, on success, closes it.
func (b *codexLoginBroker) report(acct codexAccount, f *codexLoginFlow, text string, ok bool, fp string) {
	id := f.req.EscalationID
	if id == "" || b.esc == nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.WithoutCancel(b.run), 15*time.Second)
	defer cancel()
	if err := b.esc.Comment(ctx, id, text); err != nil {
		b.logger.Warn("codex device login: could not comment on escalation", "escalation", id, "error", err)
	}
	if ok {
		if err := b.esc.Resolve(ctx, id, map[string]any{"account": acct.Name, "fingerprint": fp}); err != nil {
			b.logger.Warn("codex device login: could not close escalation", "escalation", id, "error", err)
		}
	}
}

// ── HTTP ────────────────────────────────────────────────────────────────

// codexLoginBrokerHolder lets the :8091 routes (registered before leader election)
// reach a broker that exists only while this process leads.
type codexLoginBrokerHolder struct {
	p atomic.Pointer[codexLoginBroker]
}

// codexLoginBrokers is the process-wide holder: the leader sets it, the routes read it.
var codexLoginBrokers = &codexLoginBrokerHolder{}

func (h *codexLoginBrokerHolder) Set(b *codexLoginBroker) { h.p.Store(b) }

// requireControllerToken is strict, unlike newControllerAPIAuth: the device-login
// routes hand out a live code, so with CONTROLLER_API_TOKEN unset they are off.
func requireControllerToken(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		token := strings.TrimSpace(os.Getenv(controllerAPITokenEnv))
		if token == "" {
			writeLoginJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "device login disabled: CONTROLLER_API_TOKEN is not set"})
			return
		}
		auth := r.Header.Get("Authorization")
		provided := strings.TrimPrefix(auth, "Bearer ")
		if provided == auth || subtle.ConstantTimeCompare([]byte(provided), []byte(token)) != 1 {
			writeLoginJSON(w, http.StatusUnauthorized, map[string]string{"error": "unauthorized"})
			return
		}
		next(w, r)
	}
}

func writeLoginJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func registerCodexLoginEndpoints(mux *http.ServeMux, h *codexLoginBrokerHolder) {
	mux.HandleFunc("POST /api/v1/codex/device-login", requireControllerToken(func(w http.ResponseWriter, r *http.Request) {
		b := h.p.Load()
		if b == nil {
			writeLoginJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "this controller replica is not the leader; retry"})
			return
		}
		var req codexLoginRequest
		if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<14)).Decode(&req); err != nil || req.Account == "" {
			writeLoginJSON(w, http.StatusBadRequest, map[string]string{"error": "body must be JSON with an account"})
			return
		}
		res, err := b.Start(r.Context(), req)
		if err != nil {
			var le *codexLoginError
			if errors.As(err, &le) {
				writeLoginJSON(w, le.Status, map[string]string{"error": le.Msg})
				return
			}
			writeLoginJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal error"})
			return
		}
		writeLoginJSON(w, http.StatusOK, res)
	}))
	mux.HandleFunc("GET /api/v1/codex/device-login/status", requireControllerToken(func(w http.ResponseWriter, r *http.Request) {
		b := h.p.Load()
		if b == nil {
			writeLoginJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "this controller replica is not the leader; retry"})
			return
		}
		writeLoginJSON(w, http.StatusOK, b.Status(r.URL.Query().Get("account")))
	}))
}
