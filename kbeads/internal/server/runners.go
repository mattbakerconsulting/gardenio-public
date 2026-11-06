package server

// Runner registration, heartbeat and revoke (local-agents spec §4.1-§4.2,
// plan gd-fwFefkDP4H Task 7 gd-zPtKJKzZhE).
//
// A runner is one laptop registered to run local agents. It is a type=runner
// bead (owner, projects allowlist, state) plus one or more runner_tokens rows
// holding only the SHA-256 of the "gr_..." bearer the laptop keeps. A runner
// comes into being by redeeming a single-use type=runner_invite bead at
// POST /v1/runners/register, the one pre-auth route here: the invite code in
// the body is the credential (like /v1/auth/refresh), so the daemon's own
// writes on that path run withMachineWrite and attribute to the invitee.
//
// A runner then authenticates with its "gr_..." bearer: runner_auth.go (Task 8,
// gd-9toFMX9JjW) resolves it to the owner, stamps RunnerIDFromContext, and
// admits it only on its route allowlist.

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"database/sql"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"time"

	"gardenio/kbeads/internal/model"

	beadsv1 "gardenio/kbeads/gen/beads/v1"
)

const (
	runnerTokenPrefix       = "gr_"
	runnerRegisterPerHour   = 10
	runnerInviteNotFoundMsg = "invite not found"

	runnerStateOnline  = "online"
	runnerStateAway    = "away"
	runnerStateRevoked = "revoked"

	// runnerIdentityType is the AuthIdentity.Type of a write made for a
	// runner's owner: the register path here, and every request on a runner
	// bearer (runner_auth.go). resolveOperatedByPure honours the
	// server-stamped operated_by header for it.
	runnerIdentityType = "runner"
	// runnerRegisterIdentityType marks a request that reached
	// POST /v1/runners/register through the AuthMiddleware pre-auth exception.
	runnerRegisterIdentityType = "runner-register"
)

type runnerIDKey struct{}

// WithRunnerID stamps the authenticated runner's bead id (runnerContext sets
// it). RunnerIDFromContext returns "" for non-runner callers.
func WithRunnerID(ctx context.Context, id string) context.Context {
	return context.WithValue(ctx, runnerIDKey{}, id)
}

// RunnerIDFromContext returns the runner bead id stamped by WithRunnerID, or
// "" when the caller is not a runner.
func RunnerIDFromContext(ctx context.Context) string {
	v, _ := ctx.Value(runnerIDKey{}).(string)
	return v
}

// mintRunnerToken returns a fresh runner bearer ("gr_" + 32 random bytes,
// base64url) and the hash that is the only form ever stored.
func mintRunnerToken() (token, hash string, err error) {
	raw := make([]byte, 32)
	if _, err := rand.Read(raw); err != nil {
		return "", "", err
	}
	token = runnerTokenPrefix + base64.RawURLEncoding.EncodeToString(raw)
	return token, runnerTokenHash(token), nil
}

// runnerTokenHash is hex(sha256(s)). It hashes runner tokens and invite codes.
func runnerTokenHash(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:])
}

// registerLimiter is a fixed-window per-IP counter for /v1/runners/register
// (spec §10: 10 registrations per hour per IP). In-process, so it is per
// daemon replica; the public ingress (Task 10) rate-limits in front of it.
type registerLimiter struct {
	mu     sync.Mutex
	window time.Time
	counts map[string]int
}

func (l *registerLimiter) allow(ip string, now time.Time) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.counts == nil || now.Sub(l.window) >= time.Hour {
		l.window, l.counts = now, map[string]int{}
	}
	l.counts[ip]++
	return l.counts[ip] <= runnerRegisterPerHour
}

type registerRunnerRequest struct {
	InviteCode string   `json:"invite_code"`
	Hostname   string   `json:"hostname"`
	OSArch     string   `json:"os_arch"`
	Tags       []string `json:"tags"`
}

type registerRunnerResponse struct {
	RunnerID string   `json:"runner_id"`
	Token    string   `json:"token"`
	Owner    string   `json:"owner"`
	Projects []string `json:"projects"`
}

// handleRegisterRunner redeems an invite: POST /v1/runners/register.
// Unknown, expired and already-redeemed codes all get the same 404 body.
func (s *BeadsServer) handleRegisterRunner(w http.ResponseWriter, r *http.Request) {
	if !s.runnerRegisterLimiter.allow(clientIP(r, s.trustedProxies), time.Now()) {
		writeError(w, http.StatusTooManyRequests, "too many registrations from this address; try again later")
		return
	}
	var req registerRunnerRequest
	if !decodeJSONBody(w, r, &req) {
		return
	}
	if req.InviteCode == "" || req.Hostname == "" {
		writeError(w, http.StatusBadRequest, "invite_code and hostname are required")
		return
	}
	ctx := r.Context()

	// One redemption at a time, so two requests racing on the same code
	// cannot both find it open. Registrations are rare and rate-limited.
	s.runnerRegisterMu.Lock()
	defer s.runnerRegisterMu.Unlock()

	invite, fields, err := s.findOpenInviteByHash(ctx, runnerTokenHash(req.InviteCode))
	if err != nil {
		writeServerError(ctx, w, "handleRegisterRunner", err, "failed to look up invite")
		return
	}
	if invite == nil || fields.RedeemedAt != "" {
		writeError(w, http.StatusNotFound, runnerInviteNotFoundMsg)
		return
	}
	if exp, err := time.Parse(time.RFC3339, fields.ExpiresAt); err != nil || !time.Now().Before(exp) {
		writeError(w, http.StatusNotFound, runnerInviteNotFoundMsg)
		return
	}
	if len(fields.Projects) == 0 {
		writeError(w, http.StatusBadRequest, "invite carries no projects; the runner allowlist cannot be empty")
		return
	}
	owner := fields.Invitee
	if _, ok := sanitizeOperatedByHeader(owner); !ok || owner == "" {
		writeError(w, http.StatusBadRequest, "invite has no valid invitee")
		return
	}

	token, hash, err := mintRunnerToken()
	if err != nil {
		writeServerError(ctx, w, "handleRegisterRunner", err, "failed to mint token")
		return
	}
	now := time.Now().UTC()
	runnerFields, err := json.Marshal(map[string]any{
		"owner":           owner,
		"hostname":        req.Hostname,
		"os_arch":         req.OSArch,
		"tags":            req.Tags,
		"projects":        fields.Projects,
		"token_id":        hash[:16],
		"credential_mode": "owner_login",
		"last_seen_at":    now.Format(time.RFC3339),
		"state":           runnerStateOnline,
	})
	if err != nil {
		writeServerError(ctx, w, "handleRegisterRunner", err, "failed to encode runner fields")
		return
	}

	wctx := s.runnerOwnerWriteContext(ctx, owner, invite.Fleet)
	runner, err := s.createBead(wctx, createBeadInput{
		Title:  req.Hostname,
		Kind:   string(model.KindConfig),
		Type:   "runner",
		Owner:  owner,
		Labels: projectLabels(fields.Projects),
		Fields: runnerFields,
	})
	if err != nil {
		writeServerError(ctx, w, "handleRegisterRunner", err, "failed to create runner")
		return
	}
	if err := s.store.InsertRunnerToken(ctx, &model.RunnerToken{TokenHash: hash, RunnerID: runner.ID, CreatedAt: now}); err != nil {
		writeServerError(ctx, w, "handleRegisterRunner", err, "failed to store runner token")
		return
	}
	if err := s.markInviteRedeemed(wctx, invite.ID, runner.ID, now); err != nil {
		// The token is already issued; the invite must not stay redeemable.
		// Report the failure rather than hand out a token for a reusable code.
		writeServerError(ctx, w, "handleRegisterRunner", err, "failed to redeem invite")
		return
	}
	writeJSON(w, http.StatusCreated, registerRunnerResponse{RunnerID: runner.ID, Token: token, Owner: owner, Projects: fields.Projects})
}

// runnerOwnerWriteContext is the context the daemon's own runner writes run
// under on the register path: attributed to the runner's owner (D6) and
// marked withMachineWrite, because type=runner and type=runner_invite are
// admin-gated and the authorization here is the redeemed invite itself, which
// only an admin could have created. A fresh context, so no caller-supplied
// header or identity leaks into it.
func (s *BeadsServer) runnerOwnerWriteContext(parent context.Context, owner, fleet string) context.Context {
	ctx := context.WithoutCancel(parent)
	ctx = WithAuthIdentity(ctx, AuthIdentity{Type: runnerIdentityType, Subject: owner})
	ctx = WithOperatedByHeader(ctx, owner)
	if fleet != "" {
		ctx = WithFleet(ctx, fleet)
	}
	return withMachineWrite(ctx)
}

// runnerInviteFields is the decoded fields map of a type=runner_invite bead.
type runnerInviteFields struct {
	CodeHash   string   `json:"code_hash"`
	Invitee    string   `json:"invitee"`
	Projects   []string `json:"projects"`
	ExpiresAt  string   `json:"expires_at"`
	RedeemedAt string   `json:"redeemed_at"`
}

// findOpenInviteByHash returns the open invite whose code_hash equals hash.
// Every open invite is compared in constant time so the lookup does not leak
// how much of a guessed code matched (spec §10).
func (s *BeadsServer) findOpenInviteByHash(ctx context.Context, hash string) (*model.Bead, runnerInviteFields, error) {
	invites, _, err := s.store.ListBeads(ctx, model.BeadFilter{
		Type:   []model.BeadType{"runner_invite"},
		Status: []model.Status{model.StatusOpen},
	})
	if err != nil {
		return nil, runnerInviteFields{}, err
	}
	var (
		match       *model.Bead
		matchFields runnerInviteFields
	)
	for _, b := range invites {
		var f runnerInviteFields
		if len(b.Fields) > 0 {
			if err := json.Unmarshal(b.Fields, &f); err != nil {
				continue
			}
		}
		if subtle.ConstantTimeCompare([]byte(f.CodeHash), []byte(hash)) == 1 && match == nil {
			match, matchFields = b, f
		}
	}
	return match, matchFields, nil
}

// markInviteRedeemed stamps redeemed_at/redeemed_by and closes the invite.
func (s *BeadsServer) markInviteRedeemed(ctx context.Context, inviteID, runnerID string, at time.Time) error {
	if err := s.mergeBeadFields(ctx, inviteID, map[string]any{
		"redeemed_at": at.Format(time.RFC3339),
		"redeemed_by": runnerID,
	}); err != nil {
		return err
	}
	_, err := s.CloseBead(ctx, &beadsv1.CloseBeadRequest{Id: inviteID})
	return err
}

func projectLabels(projects []string) []string {
	out := make([]string, 0, len(projects))
	for _, p := range projects {
		out = append(out, "project:"+p)
	}
	return out
}

// getRunnerBead loads bead id and its fields, writing 404 when it is missing
// or not a runner. ok is false when a response has been written.
func (s *BeadsServer) getRunnerBead(w http.ResponseWriter, r *http.Request, handler string) (*model.Bead, map[string]any, bool) {
	id := r.PathValue("id")
	bead, err := s.store.GetBead(r.Context(), id)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		writeServerError(r.Context(), w, handler, err, "failed to get runner")
		return nil, nil, false
	}
	if bead == nil || bead.Type != "runner" {
		writeError(w, http.StatusNotFound, "runner not found")
		return nil, nil, false
	}
	fields := map[string]any{}
	if len(bead.Fields) > 0 {
		if err := json.Unmarshal(bead.Fields, &fields); err != nil {
			writeServerError(r.Context(), w, handler, err, "failed to decode runner fields")
			return nil, nil, false
		}
	}
	return bead, fields, true
}

type runnerHeartbeatRequest struct {
	State string `json:"state"`
}

type runnerHeartbeatResponse struct {
	State      string `json:"state"`
	LastSeenAt string `json:"last_seen_at"`
}

// handleRunnerHeartbeat: POST /v1/runners/{id}/heartbeat. Sets last_seen_at
// and state (online, or away when the body asks). A runner may heartbeat only
// itself; an operator may heartbeat any runner. A revoked runner gets 409.
func (s *BeadsServer) handleRunnerHeartbeat(w http.ResponseWriter, r *http.Request) {
	bead, fields, ok := s.getRunnerBead(w, r, "handleRunnerHeartbeat")
	if !ok {
		return
	}
	ctx := r.Context()
	if rid := RunnerIDFromContext(ctx); rid != "" && rid != bead.ID {
		writeError(w, http.StatusForbidden, "runner may only heartbeat itself")
		return
	}
	if state, _ := fields["state"].(string); state == runnerStateRevoked {
		writeError(w, http.StatusConflict, "runner is revoked")
		return
	}
	var req runnerHeartbeatRequest
	if !decodeJSONBodyOptional(w, r, &req) {
		return
	}
	state := runnerStateOnline
	switch req.State {
	case "", runnerStateOnline:
	case runnerStateAway:
		state = runnerStateAway
	default:
		writeError(w, http.StatusBadRequest, `state must be "online" or "away"`)
		return
	}
	now := time.Now().UTC().Format(time.RFC3339)
	if err := s.mergeBeadFields(ctx, bead.ID, map[string]any{"state": state, "last_seen_at": now}); err != nil {
		writeServerError(ctx, w, "handleRunnerHeartbeat", err, "failed to record heartbeat")
		return
	}
	writeJSON(w, http.StatusOK, runnerHeartbeatResponse{State: state, LastSeenAt: now})
}

// handleGetRunner: GET /v1/runners/{id}. The bead JSON of a type=runner bead,
// served by handleGetBead so read authz and the response shape match it.
func (s *BeadsServer) handleGetRunner(w http.ResponseWriter, r *http.Request) {
	if _, _, ok := s.getRunnerBead(w, r, "handleGetRunner"); !ok {
		return
	}
	s.handleGetBead(w, r)
}

// callerMayRevokeRunner is the revoke authz (spec D6: a runner is its owner's
// to switch off; only an admin may act on someone else's). The route's
// requireRole(RoleOperator) lets every service token through, so it is not
// enough on its own:
//   - a person (OIDC, a land, or the bridge's v2 envelope) must own the runner
//     or hold gardenio-admin;
//   - a service token is admitted only if it may also send the control:kill
//     mail (mailControlDenial): the bridge fleets in
//     KBEADS_CONTROL_MAIL_FLEETS (the primary static token that was also
//     admitted is retired, gd-ke6PC7bRto). A bridge relays a person who
//     cannot reach the daemon as themselves, so it checks that the person
//     owns the runner before it calls (bridge /runner revoke). A per-agent
//     token, a machine principal and a runner bearer are refused.
//
// Once a caller is admitted, handleRevokeRunner sends the kill mails as the
// daemon (withRunnerRevokeKill): a caller admitted here but not by the control:
// mail guard (a daemon with auth disabled, identity none) still kills the
// runner's agents (gd-qoiurW3S7m).
func (s *BeadsServer) callerMayRevokeRunner(ctx context.Context, bead *model.Bead, fields map[string]any) bool {
	method := authMethodFromContext(ctx)
	switch {
	case method == authMethodNone:
		return true
	case roleBearing(method):
		if hasMinimumRole(RolesFromContext(ctx), RoleAdmin) {
			return true
		}
		owner, _ := fields["owner"].(string)
		if owner == "" {
			owner = bead.Owner
		}
		sub := AuthIdentityFromContext(ctx).Subject
		return owner != "" && sub != "" && (strings.EqualFold(owner, sub) || strings.EqualFold(owner, "oidc:"+sub))
	}
	id := AuthIdentityFromContext(ctx)
	switch id.Type {
	case "fleet":
		fleets := s.controlMailFleets
		if fleets == nil {
			fleets = controlMailFleetsFromEnv()
		}
		return fleets[FleetFromContext(ctx)]
	}
	return false
}

type runnerRevokeResponse struct {
	RevokedTokens int `json:"revoked_tokens"`
	AgentsKilled  int `json:"agents_killed"`
}

// handleRevokeRunner: POST /v1/runners/{id}/revoke (spec §4.2 step 4).
// Revokes every token of the runner, sets state=revoked, and sends a
// control:kill mail to each open agent bead running on it.
func (s *BeadsServer) handleRevokeRunner(w http.ResponseWriter, r *http.Request) {
	bead, fields, ok := s.getRunnerBead(w, r, "handleRevokeRunner")
	if !ok {
		return
	}
	ctx := r.Context()
	if !s.callerMayRevokeRunner(ctx, bead, fields) {
		writeError(w, http.StatusForbidden, "only the runner's owner or an admin may revoke it")
		return
	}
	revoked, err := s.store.RevokeRunnerTokens(ctx, bead.ID, time.Now().UTC())
	if err != nil {
		writeServerError(ctx, w, "handleRevokeRunner", err, "failed to revoke runner tokens")
		return
	}
	if err := s.mergeBeadFields(ctx, bead.ID, map[string]any{"state": runnerStateRevoked}); err != nil {
		writeServerError(ctx, w, "handleRevokeRunner", err, "failed to mark runner revoked")
		return
	}

	agents, _, err := s.store.ListBeads(ctx, model.BeadFilter{
		Type:   []model.BeadType{"agent"},
		Status: []model.Status{model.StatusOpen, model.StatusInProgress},
		Fields: map[string]string{"runtime_id": bead.ID},
	})
	if err != nil {
		writeServerError(ctx, w, "handleRevokeRunner", err, "failed to list runner agents")
		return
	}
	from := "from:" + actorFromContext(ctx)
	killCtx := withRunnerRevokeKill(ctx)
	killed := 0
	for _, a := range agents {
		name := a.Assignee
		if name == "" {
			name = a.Title
		}
		if _, err := s.createBead(killCtx, createBeadInput{
			Title:       "Stop",
			Kind:        string(model.KindData),
			Type:        "mail",
			Assignee:    name,
			Labels:      []string{from, "control:kill"},
			Description: "Runner " + bead.Title + " (" + bead.ID + ") was revoked.",
		}); err != nil {
			slog.Warn("runner revoke: kill mail failed", "runner", bead.ID, "agent", a.ID, "err", err)
			continue
		}
		killed++
	}
	writeJSON(w, http.StatusOK, runnerRevokeResponse{RevokedTokens: revoked, AgentsKilled: killed})
}
