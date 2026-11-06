// Codex subscription refresh keeper (task gd-6ggiM3hiV5, ADR 0004 §5, contract
// gd-3gTWiNEhg1 §2/§3). Modelled on github_app_credentials.go: the controller is
// the only writer of the rotating credential.
//
// For every provider=openai account bead the keeper:
//
//  1. reads the real auth.json from the controller-side Secret (gardenio-codex-auth),
//  2. refreshes it at auth.openai.com BEFORE the access token expires,
//  3. persists the rotated document to that Secret FIRST (the refresh token is
//     single-use, so losing it kills the login), then derives the -session Secret
//     (placeholder refresh_token; the only one a pod mounts), then pushes the
//     session document to coopmux so live pods get the new access token,
//  4. reads wham/usage with the fresh access token and writes the limits into the
//     account bead (utilization_5h|7d, throttle_reset_*_at, throttle_status) so
//     account selection works for a Codex pool exactly as for Anthropic. A failed
//     or reshaped usage read leaves the account eligible (fail open).
//
// A refresh OpenAI rejects (reused, expired, revoked) is NEVER retried: the
// account is marked probe_auth_status=rejected, the rejected token's fingerprint
// is recorded, and a re-login escalation is filed. A different refresh token in
// the Secret (the operator re-logged in) is recognised by its fingerprint and
// clears the rejection.
//
// Single writer: the keeper runs inside the controller's leader-elected run loop,
// so exactly one replica writes. RBAC enforces it too (name-scoped update on the
// two Secrets, bound to the controller's own ServiceAccount); the keeper also
// refuses to rotate unless a SelfSubjectAccessReview says it can write both
// Secrets, because rotating a single-use token it cannot persist is destructive.
//
// Credential hygiene: no token, refresh token or auth.json appears in a log line,
// error, bead field or escalation. Recorded values are non-secret: Secret names,
// HTTP status, the upstream error code, and the refresh-token fingerprint.
package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"runtime/debug"
	"strings"
	"sync"
	"time"

	authorizationv1 "k8s.io/api/authorization/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"

	"gardenio/controller/internal/beadsapi"
	"gardenio/controller/internal/codexauth"
	"gardenio/controller/internal/coopmux"
	"gardenio/controller/internal/podmanager"
)

const (
	// codexKeeperTickInterval is both the refresh-check and the usage-read cadence.
	// The refresh window is 24h, so 5 minutes is far finer than needed for
	// rotation; it matches the Anthropic probe cadence for limits.
	codexKeeperTickInterval = 5 * time.Minute

	// codexRefreshWindow: rotate when the access token has less than this left.
	codexRefreshWindow = 24 * time.Hour
	// codexRefreshMaxAge: rotate when last_refresh is older than this. Codex
	// itself refreshes at about 8 days, so the keeper always acts first.
	codexRefreshMaxAge = 6 * 24 * time.Hour

	// codexRefreshMinInterval: never refresh a document refreshed more recently
	// than this, whatever its expiry says (including an unreadable expiry). A
	// single-use refresh token must not be burned in a loop.
	codexRefreshMinInterval = time.Hour

	codexKeeperOpTimeout = 60 * time.Second

	// codexRotateTimeout is the budget for the refresh call, and separately for the
	// persist that follows it. Both run on a context detached from the caller's:
	// once OpenAI has rotated the single-use refresh token, the new one exists only
	// in this process until it is written, so a SIGTERM or lost lease must not cancel
	// the write that saves it.
	codexRotateTimeout = 30 * time.Second

	// codexShutdownDrain is how long shutdown and OnStoppedLeading wait for a
	// rotation in flight before exiting. It covers the worst case of one rotation:
	// the refresh budget plus the persist budget (codexRotateTimeout each) and a
	// margin. Shutdown holds the leader lease while it drains (see
	// runLeaderElection), so this may exceed the 15s lease; the pod's
	// terminationGracePeriodSeconds must exceed it.
	codexShutdownDrain = 2*codexRotateTimeout + 5*time.Second

	// codexHandoverGrace is how long a newly elected leader's keeper idles before
	// its first pass. A leader that LOSES the lease (renew failure) cannot hold it
	// while it drains, so the successor may be elected up to a renew deadline
	// earlier than the old leader's drain ends; waiting out a full drain budget
	// guarantees the old leader's write-back landed before anyone reads the Secret.
	codexHandoverGrace = codexShutdownDrain

	// codexPersistAttempts bounds the write-back of a rotated document. The
	// tokens are held in memory across attempts and across ticks (pending).
	codexPersistAttempts = 5

	// Account-bead fields the keeper owns, beyond the shared probe fields.
	fieldCodexRejectedFP = "codex_rejected_refresh_fp"
	fieldCodexAuthError  = "codex_auth_error"

	codexKeeperManager = "gardenio-codex-keeper"
)

// codexAccount is a provider=openai account bead as the keeper sees it.
type codexAccount struct {
	Name       string
	BeadID     string
	Credential string
	Model      string
	Fields     map[string]string
}

// codexBeads is the bead surface the keeper needs; faked in tests.
type codexBeads interface {
	ListCodexAccounts(ctx context.Context) ([]codexAccount, error)
	UpdateFields(ctx context.Context, beadID string, fields map[string]string) error
	CreateBead(ctx context.Context, req beadsapi.CreateBeadRequest) (string, error)
}

// codexMux is the coopmux slice the keeper uses.
type codexMux interface {
	SyncSingleAccount(ctx context.Context, acct coopmux.AccountSync) error
}

type codexRefresher interface {
	Refresh(ctx context.Context, refreshToken string) (codexauth.Tokens, error)
}

type codexUsageReader interface {
	Read(ctx context.Context, accessToken, accountID string) (codexauth.Usage, error)
}

// beadsCodexAccounts adapts the daemon clients. Reads and escalation creates go
// through daemon; account-bead writes go through the machine-principal client
// (see rateLimitProbe.accounts).
type beadsCodexAccounts struct {
	daemon   *beadsapi.Client
	accounts *beadsapi.Client
}

func (b beadsCodexAccounts) ListCodexAccounts(ctx context.Context) ([]codexAccount, error) {
	res, err := b.daemon.ListBeadsFiltered(ctx, beadsapi.ListBeadsQuery{
		Types:    []string{"account"},
		Statuses: []string{"open"},
		Limit:    500,
	})
	if err != nil {
		return nil, err
	}
	var out []codexAccount
	for _, bd := range res.Beads {
		if bd == nil || bd.Title == "" {
			continue
		}
		f := bd.Fields
		if !strings.EqualFold(strings.TrimSpace(f["provider"]), providerOpenAI) {
			continue
		}
		out = append(out, codexAccount{Name: bd.Title, BeadID: bd.ID, Credential: f["credential"], Model: f["model"], Fields: f})
	}
	return out, nil
}

func (b beadsCodexAccounts) UpdateFields(ctx context.Context, beadID string, fields map[string]string) error {
	return b.accounts.UpdateBeadFields(ctx, beadID, fields)
}

// Comment and Resolve make beadsCodexAccounts the codexEscalations the device-login
// broker reports its (non-secret) outcome through.
func (b beadsCodexAccounts) Comment(ctx context.Context, beadID, text string) error {
	return b.daemon.AddComment(ctx, beadID, codexKeeperManager, text)
}

// Resolve closes the re-login escalation via the escalation result verb, as the
// EscalationReaper does; the generic bead close 409s for an open escalation.
func (b beadsCodexAccounts) Resolve(ctx context.Context, beadID string, result map[string]any) error {
	return b.daemon.CaptureEscalationResult(ctx, beadID, beadsapi.CaptureEscalationResultRequest{
		Executor:        "controller",
		ExecutionTarget: "server",
		ResultStatus:    "success",
		Result:          result,
	})
}

func (b beadsCodexAccounts) CreateBead(ctx context.Context, req beadsapi.CreateBeadRequest) (string, error) {
	return b.daemon.CreateBead(ctx, req)
}

// pendingWrite is a rotated document OpenAI already issued that has not yet
// reached the controller-side Secret. It lives in memory only; the keeper retries
// it before doing anything else on the account.
//
// It records the Secret it was read from and that Secret's resourceVersion at the
// time, because a held rotation must be written back to exactly where its old
// token came from: the target is never re-derived from the account bead, which
// can be edited, disabled or listed differently by the time it is retried.
type pendingWrite struct {
	secret    string // controller-side Secret the rotation belongs in
	baseRV    string // that Secret's resourceVersion when the old token was read
	doc       []byte
	prevToken string // the refresh token that was rotated away
	since     time.Time
}

// rotationGate counts rotations in flight so shutdown can wait for them. It is a
// process-wide singleton (codexRotations) because the keeper is built deep inside
// the controller's run function while the exit paths live in main.
type rotationGate struct {
	mu sync.Mutex
	n  int
}

func (g *rotationGate) begin() {
	g.mu.Lock()
	g.n++
	g.mu.Unlock()
}

func (g *rotationGate) end() {
	g.mu.Lock()
	g.n--
	g.mu.Unlock()
}

func (g *rotationGate) busy() bool {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.n > 0
}

// Wait blocks until no rotation is in flight or max elapses; it reports whether
// the gate drained.
func (g *rotationGate) Wait(max time.Duration) bool {
	deadline := time.Now().Add(max)
	for g.busy() {
		if !time.Now().Before(deadline) {
			return false
		}
		time.Sleep(20 * time.Millisecond)
	}
	return true
}

// codexRotations is waited on by every controller exit path (see
// waitForCodexRotations) before os.Exit.
var codexRotations = &rotationGate{}

// waitForCodexRotations gives a rotation in flight up to codexShutdownDrain to
// reach the Secret. Called before the process exits on lost leadership or shutdown.
func waitForCodexRotations(logger *slog.Logger) {
	if !codexRotations.busy() {
		return
	}
	logger.Warn("waiting for an in-flight Codex token rotation to persist before exiting", "max", codexShutdownDrain.String())
	if !codexRotations.Wait(codexShutdownDrain) {
		logger.Error("exiting with a Codex token rotation still in flight; the login may need re-provisioning")
	}
}

type codexKeeper struct {
	beads     codexBeads
	k8s       kubernetes.Interface
	namespace string
	mux       codexMux // nil: coopmux not configured
	oauth     codexRefresher
	usage     codexUsageReader
	logger    *slog.Logger

	interval time.Duration
	// startDelay holds the first pass back after this process becomes leader (see
	// codexHandoverGrace). Zero: start at once.
	startDelay time.Duration
	now        func() time.Time
	sleep      func(time.Duration)

	gate    *rotationGate
	pending map[string]*pendingWrite // account name -> unpersisted rotation
	pushed  map[string]string        // account name -> sha256 of last session doc pushed to coopmux
}

func newCodexKeeper(daemon, accounts *beadsapi.Client, k8s kubernetes.Interface, namespace string, mux *coopmux.Client, logger *slog.Logger) *codexKeeper {
	if accounts == nil {
		accounts = daemon
	}
	k := &codexKeeper{
		beads:     beadsCodexAccounts{daemon: daemon, accounts: accounts},
		k8s:       k8s,
		namespace: namespace,
		logger:    logger.With("component", "codex-keeper"),
		interval:  codexKeeperTickInterval,
		now:       time.Now,
		sleep:     time.Sleep,
		gate:      codexRotations,
		pending:   map[string]*pendingWrite{},
		pushed:    map[string]string{},
	}
	k.oauth = codexauth.NewRefresher(k.safeOverride("CODEX_REFRESH_TOKEN_URL_OVERRIDE", codexauth.TokenURLAllowedHosts))
	k.usage = codexauth.NewUsageReader(k.safeOverride("CODEX_USAGE_URL_OVERRIDE", codexauth.UsageURLAllowedHosts))
	// A nil *coopmux.Client stored in the interface would still be non-nil.
	if mux != nil {
		k.mux = mux
	}
	return k
}

// safeOverride reads a URL override from the environment and returns it only if
// it is https on an allowlisted host; otherwise "" (the built-in default). Both
// endpoints receive a bearer credential, so an override is an exfiltration vector.
func (k *codexKeeper) safeOverride(env string, allowed []string) string {
	raw := os.Getenv(env)
	if raw == "" {
		return ""
	}
	u, ok := codexauth.SafeOverrideURL(raw, allowed)
	if !ok {
		k.logger.Warn("codex-keeper: ignoring "+env+": an override must be https on an allowlisted host", "allowed", strings.Join(allowed, ","))
		return ""
	}
	return u
}

// RunLoop ticks until ctx is cancelled, starting with a pass after startDelay.
func (k *codexKeeper) RunLoop(ctx context.Context) {
	if k.startDelay > 0 {
		k.logger.Info("codex-keeper: idling so the previous leader's in-flight rotation can land", "delay", k.startDelay.String())
		select {
		case <-time.After(k.startDelay):
		case <-ctx.Done():
			return
		}
	}
	ticker := time.NewTicker(k.interval)
	defer ticker.Stop()
	k.tick(ctx)
	for {
		select {
		case <-ticker.C:
			k.tick(ctx)
		case <-ctx.Done():
			return
		}
	}
}

func (k *codexKeeper) tick(ctx context.Context) {
	defer func() {
		if r := recover(); r != nil {
			k.logger.Error("codex-keeper: recovered panic in pass; loop continues", "panic", r, "stack", string(debug.Stack()))
		}
	}()
	// A held rotation outranks everything, including whether the account bead is
	// still listed, enabled or has a credential: the new refresh token exists only
	// in memory until it is written.
	k.drainPending(ctx)

	accts, err := k.beads.ListCodexAccounts(ctx)
	if err != nil {
		k.logger.Warn("codex-keeper: failed to list account beads", "error", err)
		return
	}
	for _, a := range accts {
		if a.Credential == "" || a.Fields["status"] == "disabled" {
			continue
		}
		k.syncAccountSafe(ctx, a)
	}
}

func (k *codexKeeper) syncAccountSafe(ctx context.Context, acct codexAccount) {
	defer func() {
		if r := recover(); r != nil {
			k.logger.Error("codex-keeper: recovered panic syncing account; skipping to next",
				"account", acct.Name, "panic", r, "stack", string(debug.Stack()))
		}
	}()
	ctx, cancel := context.WithTimeout(ctx, codexKeeperOpTimeout*2)
	defer cancel()
	k.syncAccount(ctx, acct)
}

func (k *codexKeeper) syncAccount(ctx context.Context, acct codexAccount) {
	log := k.logger.With("account", acct.Name)
	base, err := codexAuthSecretName(acct.Credential)
	if err != nil {
		log.Warn("codex account credential is malformed", "error", err)
		return
	}
	secrets := k.k8s.CoreV1().Secrets(k.namespace)

	// drainPending ran at the top of the tick. If this account's held rotation is
	// still unwritten the Secret holds a dead refresh token: do not read it, and
	// above all do not spend it again.
	if k.pending[acct.Name] != nil {
		return
	}

	secret, err := secrets.Get(ctx, base, metav1.GetOptions{})
	if err != nil {
		if apierrors.IsNotFound(err) {
			log.Warn("codex auth Secret does not exist; the operator must create it with --from-file", "secret", base)
		} else {
			log.Warn("codex auth Secret unreadable", "secret", base, "error", err)
		}
		return
	}
	auth, err := codexauth.Parse(secret.Data[podmanager.CodexAuthFileName])
	if err != nil {
		log.Warn("codex auth Secret holds an unusable auth.json", "secret", base, "error", err)
		return
	}

	// A token OpenAI already rejected is never retried; only a different one
	// (a re-login) clears the verdict.
	if acct.Fields["probe_auth_status"] == "rejected" && acct.Fields[fieldCodexRejectedFP] == auth.RefreshFingerprint() {
		log.Debug("codex refresh token was rejected; awaiting operator re-login", "fingerprint", auth.RefreshFingerprint())
		return
	}

	if auth.NeedsRefresh(k.now(), codexRefreshWindow, codexRefreshMaxAge, codexRefreshMinInterval) {
		rotated, ok := k.rotate(ctx, acct, base, secret.ResourceVersion, auth)
		if !ok {
			return
		}
		auth = rotated
	}

	if !k.ensureSession(ctx, acct, base, auth) {
		return
	}
	k.recordUsage(ctx, acct, auth)
}

var errCodexSuperseded = errors.New("auth Secret now holds a different refresh token")

// drainPending retries every held rotation against the Secret it was read from.
func (k *codexKeeper) drainPending(ctx context.Context) {
	for name, p := range k.pending {
		log := k.logger.With("account", name, "secret", p.secret)
		k.gate.begin()
		pctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), codexRotateTimeout)
		err := k.persistRotated(pctx, p.secret, p.baseRV, p.prevToken, p.doc)
		cancel()
		k.gate.end()
		switch {
		case err == nil:
			log.Info("codex-keeper: persisted a held rotation", "pending_since", p.since.UTC().Format(time.RFC3339))
			delete(k.pending, name)
		case errors.Is(err, errCodexSuperseded):
			log.Warn("codex-keeper: dropping a held rotation, because the auth Secret was replaced (operator re-login)")
			delete(k.pending, name)
		default:
			log.Error("codex-keeper: STILL cannot persist a rotated auth.json; the login dies if this controller restarts first",
				"pending_since", p.since.UTC().Format(time.RFC3339), "error", err)
		}
	}
}

// rotate refreshes auth and persists the result. ok is true only when the
// rotated document is durably in the controller-side Secret. baseRV is the
// resourceVersion of the Secret auth was read from.
//
// The refresh and the persist each run on a context detached from ctx with their
// own timeout, and are counted by the rotation gate: cancelling ctx (SIGTERM, lost
// lease) between OpenAI's response and the write must not strand the new token.
func (k *codexKeeper) rotate(ctx context.Context, acct codexAccount, base, baseRV string, auth *codexauth.Auth) (*codexauth.Auth, bool) {
	log := k.logger.With("account", acct.Name, "secret", base)
	// Register before checking ctx: shutdown cancels ctx and only then looks at the
	// gate, so a rotation that passes the check below is always visible to it.
	k.gate.begin()
	defer k.gate.end()
	if ctx.Err() != nil {
		return nil, false // never START a rotation while shutting down
	}
	session := base + codexSessionSecretSuffix
	if !k.canWrite(ctx, base, session) {
		log.Warn("codex-keeper: not rotating, because the controller cannot write both Secrets (RBAC not applied?); rotating a single-use token it cannot persist would kill the login")
		return nil, false
	}

	rctx, rcancel := context.WithTimeout(context.WithoutCancel(ctx), codexRotateTimeout)
	tr, err := k.oauth.Refresh(rctx, auth.RefreshToken())
	rcancel()
	if err != nil {
		if codexauth.IsPermanent(err) {
			k.markRejected(ctx, acct, auth, err)
		} else {
			log.Warn("codex-keeper: refresh failed transiently; will retry next pass", "error", err)
		}
		return nil, false
	}
	rotated, err := auth.Rotated(tr, k.now())
	if err != nil {
		k.markRejected(ctx, acct, auth, err)
		return nil, false
	}
	doc, err := rotated.Marshal()
	if err != nil {
		k.markRejected(ctx, acct, auth, err)
		return nil, false
	}

	// Persist FIRST. No yielding between the refresh response and this write.
	pctx, pcancel := context.WithTimeout(context.WithoutCancel(ctx), codexRotateTimeout)
	defer pcancel()
	if err := k.persistRotated(pctx, base, baseRV, auth.RefreshToken(), doc); err != nil {
		if errors.Is(err, errCodexSuperseded) {
			log.Warn("codex-keeper: auth Secret was replaced during rotation (operator re-login); dropping the rotated document")
			return nil, false
		}
		k.pending[acct.Name] = &pendingWrite{secret: base, baseRV: baseRV, doc: doc, prevToken: auth.RefreshToken(), since: k.now()}
		log.Error("codex-keeper: rotated the refresh token but cannot persist it; holding it in memory and retrying", "error", err)
		k.escalatePersistFailure(ctx, acct, base, err)
		return nil, false
	}
	log.Info("codex auth.json refreshed", "last_refresh", k.now().UTC().Format(time.RFC3339))
	return rotated, true
}

// persistRotated writes doc to the controller-side Secret with optimistic
// concurrency, retrying with the tokens held in memory. prevToken is the refresh
// token that was rotated away: if the Secret no longer holds it (nor the new
// one), someone else replaced the login and we must not overwrite it.
//
// The first attempt is conditional on baseRV, the resourceVersion the old token
// was read at, so the API server itself rejects the write (409) if anything
// touched the Secret since. After a conflict the Secret is re-read and judged by
// its refresh token: still prevToken means the change was unrelated (retry on the
// fresh version), anything else means a re-login (errCodexSuperseded).
func (k *codexKeeper) persistRotated(ctx context.Context, base, baseRV, prevToken string, doc []byte) error {
	secrets := k.k8s.CoreV1().Secrets(k.namespace)
	newAuth, err := codexauth.Parse(doc)
	if err != nil {
		return err
	}
	var lastErr error
	delay := 500 * time.Millisecond
	for attempt := 0; attempt < codexPersistAttempts; attempt++ {
		if err := ctx.Err(); err != nil {
			return fmt.Errorf("persist to Secret %s: %w", base, err)
		}
		if attempt > 0 {
			k.sleep(delay)
			delay *= 2
		}
		cur, err := secrets.Get(ctx, base, metav1.GetOptions{})
		if err != nil {
			lastErr = err
			continue
		}
		if existing, perr := codexauth.Parse(cur.Data[podmanager.CodexAuthFileName]); perr == nil {
			switch existing.RefreshToken() {
			case newAuth.RefreshToken():
				return nil // already persisted (a previous attempt landed)
			case prevToken:
			default:
				return errCodexSuperseded
			}
		}
		if cur.Data == nil {
			cur.Data = map[string][]byte{}
		}
		cur.Data[podmanager.CodexAuthFileName] = doc
		if attempt == 0 && baseRV != "" {
			cur.ResourceVersion = baseRV
		}
		if _, err := secrets.Update(ctx, cur, metav1.UpdateOptions{}); err != nil {
			lastErr = err
			continue
		}
		return nil
	}
	return fmt.Errorf("persist to Secret %s failed after %d attempts: %w", base, codexPersistAttempts, lastErr)
}

// canWrite asks the API server whether this ServiceAccount may update both
// Secrets. Fail closed: if the review itself cannot be made, assume no.
func (k *codexKeeper) canWrite(ctx context.Context, names ...string) bool {
	for _, name := range names {
		review := &authorizationv1.SelfSubjectAccessReview{
			Spec: authorizationv1.SelfSubjectAccessReviewSpec{
				ResourceAttributes: &authorizationv1.ResourceAttributes{
					Namespace: k.namespace, Verb: "update", Resource: "secrets", Name: name,
				},
			},
		}
		res, err := k.k8s.AuthorizationV1().SelfSubjectAccessReviews().Create(ctx, review, metav1.CreateOptions{})
		if err != nil || !res.Status.Allowed {
			return false
		}
	}
	return true
}

// ensureSession makes the pod-facing -session Secret match the persisted
// document (placeholder refresh token) and pushes it to coopmux when it changed.
// It is derived purely from the controller-side Secret, so a failure here heals
// on the next pass. Returns false if the session Secret could not be made current.
func (k *codexKeeper) ensureSession(ctx context.Context, acct codexAccount, base string, auth *codexauth.Auth) bool {
	log := k.logger.With("account", acct.Name)
	sessionName := base + codexSessionSecretSuffix
	doc, err := auth.SessionDocument().Marshal()
	if err != nil {
		log.Warn("codex-keeper: cannot build session document", "error", err)
		return false
	}
	secrets := k.k8s.CoreV1().Secrets(k.namespace)
	cur, err := secrets.Get(ctx, sessionName, metav1.GetOptions{})
	switch {
	case apierrors.IsNotFound(err):
		_, cerr := secrets.Create(ctx, &corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{
				Name:      sessionName,
				Namespace: k.namespace,
				Labels:    map[string]string{"app.kubernetes.io/managed-by": codexKeeperManager},
			},
			Type: corev1.SecretTypeOpaque,
			Data: map[string][]byte{podmanager.CodexAuthFileName: doc},
		}, metav1.CreateOptions{})
		if cerr != nil {
			log.Warn("codex session Secret is missing and cannot be created; the operator must pre-create it empty", "secret", sessionName, "error", cerr)
			return false
		}
	case err != nil:
		log.Warn("codex session Secret unreadable", "secret", sessionName, "error", err)
		return false
	case !bytes.Equal(cur.Data[podmanager.CodexAuthFileName], doc):
		if cur.Data == nil {
			cur.Data = map[string][]byte{}
		}
		cur.Data[podmanager.CodexAuthFileName] = doc
		if _, uerr := secrets.Update(ctx, cur, metav1.UpdateOptions{}); uerr != nil {
			log.Warn("codex session Secret update failed", "secret", sessionName, "error", uerr)
			return false
		}
	}

	if k.mux != nil {
		sum := sha256.Sum256(doc)
		digest := hex.EncodeToString(sum[:])
		if k.pushed[acct.Name] != digest {
			if err := k.mux.SyncSingleAccount(ctx, coopmux.AccountSync{
				Name: acct.Name, Provider: providerOpenAI, Token: string(doc), Model: acct.Model,
			}); err != nil {
				// Non-fatal: pods still read the -session Secret. Retried next pass.
				log.Warn("codex-keeper: coopmux push failed; will retry", "error", err)
			} else {
				k.pushed[acct.Name] = digest
			}
		}
	}
	return true
}

// markRejected records a terminal refresh rejection and asks for a re-login.
func (k *codexKeeper) markRejected(ctx context.Context, acct codexAccount, auth *codexauth.Auth, cause error) {
	fp := auth.RefreshFingerprint()
	reason := "refresh_failed"
	var re *codexauth.RefreshError
	if errors.As(cause, &re) && re.Code != "" {
		reason = re.Code
	}
	k.logger.Error("codex refresh token rejected; marking the account probe_auth_status=rejected and never retrying it",
		"account", acct.Name, "reason", reason, "fingerprint", fp)
	if acct.BeadID != "" {
		if err := k.beads.UpdateFields(ctx, acct.BeadID, map[string]string{
			"probe_auth_status":  "rejected",
			fieldCodexRejectedFP: fp,
			fieldCodexAuthError:  reason,
			"probe_checked_at":   k.now().UTC().Format(time.RFC3339),
		}); err != nil {
			k.logger.Warn("codex-keeper: failed to persist probe_auth_status=rejected", "account", acct.Name, "error", err)
		}
	}
	if _, err := k.beads.CreateBead(ctx, buildCodexReloginEscalation(acct, fp, reason)); err != nil {
		k.logger.Warn("codex-keeper: failed to file re-login escalation", "account", acct.Name, "error", err)
	}
}

func (k *codexKeeper) escalatePersistFailure(ctx context.Context, acct codexAccount, base string, cause error) {
	fields, _ := json.Marshal(map[string]any{
		"operation_type": "credential_provisioning",
		"operation_spec": map[string]any{"account": acct.Name, "secret": base, "reason": "persist_failed"},
		"context": fmt.Sprintf("The Codex refresh keeper rotated the refresh token for account %q but could not write it to Secret %q (%v). "+
			"It is holding the new tokens in controller memory and retrying every pass; if the controller restarts first the login is lost and a "+
			"`codex login --device-auth` re-login is needed. Check the controller's update permission on that Secret.", acct.Name, base, cause),
	})
	req := beadsapi.CreateBeadRequest{
		Title:          fmt.Sprintf("Codex keeper cannot persist rotated login: %s", acct.Name),
		Type:           "escalation",
		Kind:           "data",
		Fields:         json.RawMessage(fields),
		Labels:         []string{"guard:codex-keeper", "project:gardenio"},
		Priority:       0,
		CreatedBy:      "gardenio-controller",
		IdempotencyKey: "codex-persist-failed:" + acct.Name,
	}
	if _, err := k.beads.CreateBead(ctx, req); err != nil {
		k.logger.Warn("codex-keeper: failed to file persist-failure escalation", "account", acct.Name, "error", err)
	}
}

// buildCodexReloginEscalation asks the operator to mint a fresh login. The
// idempotency key includes the rejected token's fingerprint, so one escalation
// per dead login, and a new one only if a later login dies too.
func buildCodexReloginEscalation(acct codexAccount, fp, reason string) beadsapi.CreateBeadRequest {
	base, _ := codexAuthSecretName(acct.Credential)
	fields, _ := json.Marshal(map[string]any{
		"operation_type": "credential_provisioning",
		"operation_spec": map[string]any{
			"action": codexDeviceLoginAction, "account": acct.Name, "secret": base, "reason": reason, "rejected_fingerprint": fp,
		},
		"context": fmt.Sprintf("OpenAI rejected the Codex refresh token for account %q (%s). The login is dead and the keeper will not retry it. "+
			"Press *Start device login* on this card (Slack) and approve the code it shows you privately at the URL, in a browser signed in as the ChatGPT plan owner. "+
			"The controller stores the new login itself; nothing is pasted anywhere and this escalation closes on its own. "+
			"Approve only a code you just requested here. "+
			"Fallback if the button is unavailable: `gb codex login --secret %s` on your machine (throwaway CODEX_HOME, stored without displaying it). "+
			"Never copy a laptop ~/.codex/auth.json: rotation would knock the laptop and the cluster out of each other. "+
			"The keeper notices the new token on its next pass and clears probe_auth_status.", acct.Name, reason, base),
	})
	return beadsapi.CreateBeadRequest{
		Title:          fmt.Sprintf("Codex login rejected, re-login needed: %s", acct.Name),
		Type:           "escalation",
		Kind:           "data",
		Fields:         json.RawMessage(fields),
		Labels:         []string{"guard:codex-keeper", "project:gardenio"},
		Priority:       0,
		CreatedBy:      "gardenio-controller",
		IdempotencyKey: "codex-relogin:" + acct.Name + ":" + fp,
	}
}

// usageToProbeResult maps wham/usage onto the Anthropic probe's result shape so
// the existing field writers and selection logic apply unchanged: primary window
// -> 5h, secondary (weekly) -> 7d, representative_claim = the binding window,
// limit_reached -> rejected.
func usageToProbeResult(u codexauth.Usage, now time.Time) probeResult {
	r := probeResult{Status: "allowed", HTTPStatus: 200, CheckedAt: now}
	if w := u.Primary; w != nil {
		r.Utilization5h = clampFraction(w.UsedFraction)
		if !w.ResetAt.IsZero() {
			r.Reset5h = w.ResetAt.Unix()
		}
	}
	if w := u.Secondary; w != nil {
		r.Utilization7d = clampFraction(w.UsedFraction)
		if !w.ResetAt.IsZero() {
			r.Reset7d = w.ResetAt.Unix()
		}
	}
	if r.Utilization7d > r.Utilization5h {
		r.RepresentativeClaim, r.Reset = "seven_day", r.Reset7d
	} else {
		r.RepresentativeClaim, r.Reset = "five_hour", r.Reset5h
	}
	if u.LimitReached {
		r.Status = "rejected"
	}
	return r
}

func clampFraction(f float64) float64 {
	if f < 0 {
		return 0
	}
	return f
}

// recordUsage reads wham/usage and writes the limits into the account bead. Fail
// open: any failure leaves the previously recorded limits, probe_auth_status and
// the account's eligibility untouched. probe_auth_status=ok is a claim that the
// credential was just verified against OpenAI, so it is written only after a
// successful read; a failed read must not upgrade a previously-unknown or
// rejected account to ok. (A re-login whose first usage read fails therefore
// keeps its stale rejection until a read succeeds.)
func (k *codexKeeper) recordUsage(ctx context.Context, acct codexAccount, auth *codexauth.Auth) {
	log := k.logger.With("account", acct.Name)
	if acct.BeadID == "" {
		return
	}
	u, err := k.usage.Read(ctx, auth.AccessToken(), auth.AccountID())
	if err != nil {
		log.Warn("codex-keeper: wham/usage read failed; leaving the account's recorded state untouched", "error", err)
		return
	}
	res := usageToProbeResult(u, k.now())
	fields := map[string]string{
		"throttle_status":      res.Status,
		"utilization_5h":       fmt.Sprintf("%.4f", res.Utilization5h),
		"utilization_7d":       fmt.Sprintf("%.4f", res.Utilization7d),
		"representative_claim": res.RepresentativeClaim,
		"probe_checked_at":     res.CheckedAt.UTC().Format(time.RFC3339),
	}
	if res.Reset > 0 {
		fields["throttle_reset_at"] = time.Unix(res.Reset, 0).UTC().Format(time.RFC3339)
	}
	if res.Reset5h > 0 {
		fields["throttle_reset_5h_at"] = time.Unix(res.Reset5h, 0).UTC().Format(time.RFC3339)
	}
	if res.Reset7d > 0 {
		fields["throttle_reset_7d_at"] = time.Unix(res.Reset7d, 0).UTC().Format(time.RFC3339)
	}
	reconcileLegacyStatusFields(acct.Fields, res, fields)
	// The read just succeeded with this login: clear any rejection left by a
	// previous one.
	if acct.Fields["probe_auth_status"] != "ok" {
		fields["probe_auth_status"] = "ok"
	}
	if acct.Fields[fieldCodexRejectedFP] != "" {
		fields[fieldCodexRejectedFP] = ""
		fields[fieldCodexAuthError] = ""
	}
	if err := k.beads.UpdateFields(ctx, acct.BeadID, fields); err != nil {
		log.Warn("codex-keeper: failed to update account bead fields", "error", err)
	}
}
