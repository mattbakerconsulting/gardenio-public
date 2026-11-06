package server

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"strconv"
	"strings"
	"time"

	"gardenio/kbeads/internal/events"
	"gardenio/kbeads/internal/landmap"
	"gardenio/kbeads/internal/model"
)

// A member's own agent (Lands API createAgent and the rest, D.17
// gd-foQLqIRXDq; owner choice C, doc gd-K7BUSVVHJF). A land's owner or
// member starts an agent on their own machine: the daemon writes its
// type=agent bead (execution_target=laptop, so the controller runs nothing
// for it; operated_by the member) and mints the per-agent token it runs kd and
// gb under, agent-<bead id> bound to the land, the same class the controller
// mints for a pod agent. Its role is lands-gardener, which holds the
// lands-tenant tier, so whatever the member's own role the agent cannot
// answer decisions, approve escalations or spawn (restricted_ops.go,
// land_door_agent.go): the tier comes from the role, never from the minter.
// No LiteLLM key is minted (byo, decision gd-09hIjRBKvf): the machine pays for
// its own model use and the purse is untouched. The bead holds one of the
// land's max_live_agents slots like a pod gardener's, so the controller's
// wall counts it, and the mint refuses a land that is full.
//
// Mint admits an owner or member on their own sign-in (or an unrestricted
// operator), never a guest, an agent, a land principal or any service token.
// Refresh admits the same, or the agent itself on its own token. Revoke
// admits the member who started it, the land's owner or an operator. The
// bead is written server-side (landAgentWriteContext) because the spawn gate
// refuses a member who holds no gardenio-operator role; the checks here are
// that write's authorization (see authorizeWriteTitled's call-site list).

const (
	// landAgentSpawnSource marks an agent bead the Lands API wrote, so the
	// listing finds only these.
	landAgentSpawnSource = "lands_api"
	// landAgentIdentityType is the AuthIdentity.Type of the bead writes made
	// for the member (landAgentWriteContext); the resolver attributes them to
	// the server-stamped X-Operated-By (audit.go).
	landAgentIdentityType = "land-agent"
	// landAgentBearerTTL and landAgentRefreshTTL match the controller's pod
	// agent tokens (defaultPATTokenTTL, defaultPATRefreshTTL).
	landAgentBearerTTL  = 24 * time.Hour
	landAgentRefreshTTL = 30 * 24 * time.Hour
	// landAgentDefaultCap is wall.DefaultMaxLiveAgents
	// (gardenio/controller/internal/lands/wall), the cap of a land whose
	// max_live_agents is unset.
	landAgentDefaultCap = 3
	landAgentScan       = 1000
	// landAgentSuffixChars has no vowels, so a suffix spells no word the
	// response denylist refuses.
	landAgentSuffixChars = "23456789bcdfghjkmnpqrstvwxz"
)

// landAgentNotLive are the agent states that hold no live slot: wall.notLive.
var landAgentNotLive = map[string]bool{
	"done": true, "failed": true, "killed": true, "budget_exceeded": true, "queued": true, "stopping": true,
}

// landAgentLiveStatuses are the statuses the wall reads agents in.
var landAgentLiveStatuses = []model.Status{model.StatusOpen, model.StatusInProgress, model.StatusBlocked, model.StatusDeferred}

// agentFleetName is the per-agent token name the controller gives an agent
// bead (agentTokenBridgeName).
func agentFleetName(beadID string) string { return agentFleetPrefix + beadID }

// landAgentMayStart reports whether the caller may start, or refresh for, an
// agent in land: an owner or member on their own sign-in, or an unrestricted
// operator. Auth-disabled mode (authMethodNone) admits, as windows do.
func landAgentMayStart(ctx context.Context, land string) bool {
	switch authMethodFromContext(ctx) {
	case authMethodNone:
		return true
	case authMethodOIDC:
	default:
		return false
	}
	if !isScopedPrincipal(ctx) {
		return hasMinimumRole(RolesFromContext(ctx), RoleOperator)
	}
	t := scopeFromContext(ctx).Tiers[land]
	return t == tierOwner || t == tierMember
}

// landAgentMayRevoke reports whether the caller may stop agent: the member
// who started it, the land's owner, or an unrestricted operator.
func landAgentMayRevoke(ctx context.Context, land, operator string, agent *model.Bead) bool {
	switch authMethodFromContext(ctx) {
	case authMethodNone:
		return true
	case authMethodOIDC:
	default:
		return false
	}
	if !isScopedPrincipal(ctx) {
		return hasMinimumRole(RolesFromContext(ctx), RoleOperator)
	}
	switch scopeFromContext(ctx).Tiers[land] {
	case tierOwner:
		return true
	case tierMember:
		return operator != "" && landAgentStarter(agent) == operator
	}
	return false
}

// landAgentWriteContext is the context the agent bead's writes run under:
// attributed to the member (operator) and marked withMachineWrite. A fresh
// context, so no caller credential, header or token project leaks into it.
func landAgentWriteContext(parent context.Context, operator string) context.Context {
	ctx := context.WithoutCancel(parent)
	ctx = WithAuthIdentity(ctx, AuthIdentity{Type: landAgentIdentityType, Subject: operator})
	ctx = WithOperatedByHeader(ctx, operator)
	if fleet := FleetFromContext(parent); fleet != "" {
		ctx = WithFleet(ctx, fleet)
	}
	return withMachineWrite(ctx)
}

// landAgentMintKey marks the context MintAgent creates the agent bead under,
// naming the land; authorizeAgentRole admits that one create. Only MintAgent
// sets it, after landAgentMayStart.
type landAgentMintKey struct{}

func withLandAgentMint(ctx context.Context, land string) context.Context {
	return context.WithValue(ctx, landAgentMintKey{}, land)
}

func landAgentMintFromContext(ctx context.Context) (string, bool) {
	land, ok := ctx.Value(landAgentMintKey{}).(string)
	return land, ok && land != ""
}

// isLandAgent reports whether b is an agent the Lands API started in land.
func isLandAgent(b *model.Bead, land string) bool {
	if b == nil || b.Type != typeAgent || !slices.Contains(b.Labels, "project:"+land) {
		return false
	}
	f := parseFieldsToStringMap(b.Fields)
	return f["project"] == land && f["execution_target"] == "laptop" &&
		f["role"] == landsGardenerRole && f["spawn_source"] == landAgentSpawnSource
}

// landAgentStarter is the member an agent bead acts for.
func landAgentStarter(b *model.Bead) string {
	if v := parseFieldsToStringMap(b.Fields)["operated_by"]; v != "" {
		return v
	}
	return b.OperatedBy
}

func landAgentRecord(b *model.Bead) landmap.AgentRecord {
	f := parseFieldsToStringMap(b.Fields)
	return landmap.AgentRecord{
		ID: b.ID, Name: f["agent"], StartedBy: landAgentStarter(b), StartedAt: b.CreatedAt,
		Stopped: b.Status == model.StatusClosed || landAgentNotLive[f["agent_state"]],
	}
}

// landAgentBeads reads land's own-machine agent beads through the store: a
// land member may not read agent beads.
func (s *BeadsServer) landAgentBeads(ctx context.Context, land string) ([]*model.Bead, error) {
	beads, _, err := s.store.ListBeads(ctx, model.BeadFilter{
		Type:   []model.BeadType{typeAgent},
		Labels: []string{"project:" + land, "execution_target:laptop"},
		Fields: map[string]string{"spawn_source": landAgentSpawnSource},
		Limit:  landAgentScan,
	})
	if err != nil {
		return nil, err
	}
	return slices.DeleteFunc(beads, func(b *model.Bead) bool { return !isLandAgent(b, land) }), nil
}

// Agents lists land's own-machine agents, newest first.
func (l landsForMap) Agents(ctx context.Context, land string) ([]landmap.AgentRecord, error) {
	beads, err := l.s.landAgentBeads(ctx, land)
	if err != nil {
		return nil, err
	}
	out := []landmap.AgentRecord{}
	for _, b := range beads {
		out = append(out, landAgentRecord(b))
	}
	slices.SortStableFunc(out, func(a, b landmap.AgentRecord) int { return b.StartedAt.Compare(a.StartedAt) })
	return out, nil
}

// Agent returns one of land's own-machine agents.
func (l landsForMap) Agent(ctx context.Context, land, id string) (landmap.AgentRecord, bool, error) {
	b, err := l.landAgent(ctx, land, id)
	if err != nil || b == nil {
		return landmap.AgentRecord{}, false, err
	}
	return landAgentRecord(b), true, nil
}

// landAgent reads id; nil when it is not one of land's own-machine agents.
func (l landsForMap) landAgent(ctx context.Context, land, id string) (*model.Bead, error) {
	b, err := l.s.store.GetBead(ctx, id)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		return nil, nil
	case err != nil:
		return nil, err
	case !isLandAgent(b, land):
		return nil, nil
	}
	return b, nil
}

// MintAgent starts an agent for the caller in land and mints its token.
func (l landsForMap) MintAgent(ctx context.Context, land string, req landmap.AgentMintRequest) (landmap.AgentGrantRecord, error) {
	s := l.s
	if !landAgentMayStart(ctx, land) {
		return landmap.AgentGrantRecord{}, landmap.ErrAgentForbidden
	}
	// The create below is a machine write, which the wish-head gate passes:
	// a wish head's identity is refused here (wish_head_authz.go).
	if isWishHeadIdentity(req.IdempotencyKey) {
		return landmap.AgentGrantRecord{}, landmap.ErrAgentForbidden
	}
	operator, _, _, ok := s.resolveAuditOperatorRequired(ctx)
	if !ok || operator == "" {
		return landmap.AgentGrantRecord{}, landmap.ErrAgentForbidden
	}
	// The role bead must hold exactly the lands-tenant tier, so a mis-written
	// role cannot hand a member an unrestricted agent.
	if d := landsGardenerRoleDenial(ctx, s.store, "create"); d != nil {
		return landmap.AgentGrantRecord{}, fmt.Errorf("%w: %s", landmap.ErrAgentConflict, d.Message)
	}

	if req.IdempotencyKey != "" {
		prior, err := s.lookupByIdempotencyKey(ctx, []string{"project:" + land}, FleetFromContext(ctx), req.IdempotencyKey)
		if err != nil {
			return landmap.AgentGrantRecord{}, err
		}
		if prior != nil {
			// A retry answers for the same agent with a fresh pair: the first
			// cannot be read back. Only for the member who started it, while
			// it runs.
			if !isLandAgent(prior, land) || landAgentStarter(prior) != operator || landAgentRecord(prior).Stopped {
				return landmap.AgentGrantRecord{}, landmap.ErrAgentConflict
			}
			if _, _, err := s.revokeBridgeName(ctx, agentFleetName(prior.ID)); err != nil {
				return landmap.AgentGrantRecord{}, err
			}
			return s.mintLandAgentToken(ctx, prior.ID, land, parseFieldsToStringMap(prior.Fields)["agent"])
		}
	}

	if err := l.landAgentRoom(ctx, land); err != nil {
		return landmap.AgentGrantRecord{}, err
	}
	name := req.Name + "-" + landAgentSuffix()
	fields, _ := json.Marshal(map[string]string{
		"agent":            name,
		"role":             landsGardenerRole,
		"project":          land,
		"mode":             "crew",
		"agent_state":      "spawning",
		"execution_target": "laptop",
		"operated_by":      operator,
		"spawn_source":     landAgentSpawnSource,
	})
	bead, err := s.createBead(withLandAgentMint(landAgentWriteContext(ctx, operator), land), createBeadInput{
		Title:          name,
		Kind:           string(model.KindConfig),
		Type:           string(typeAgent),
		Assignee:       name,
		Labels:         []string{"project:" + land, "execution_target:laptop"},
		Fields:         fields,
		IdempotencyKey: req.IdempotencyKey,
	})
	if err != nil {
		return landmap.AgentGrantRecord{}, err
	}
	grant, err := s.mintLandAgentToken(ctx, bead.ID, land, name)
	if err != nil {
		// No token, no agent: the bead must not hold a slot.
		s.stopLandAgent(ctx, bead, operator, "its token could not be minted")
		return landmap.AgentGrantRecord{}, err
	}
	return grant, nil
}

// landAgentRoom refuses a tenant land already running its max_live_agents:
// wall.LiveCap's arithmetic, the steward's slot held. An operator land has
// no cap. A land whose agents cannot be read is refused.
func (l landsForMap) landAgentRoom(ctx context.Context, land string) error {
	p, err := l.s.store.FindProjectByName(ctx, land)
	switch {
	case errors.Is(err, sql.ErrNoRows), err == nil && p == nil:
		return landmap.ErrAgentNotFound
	case err != nil:
		return err
	}
	pf := parseFieldsToStringMap(p.Fields)
	if pf["land_kind"] != landKindTenant {
		return nil
	}
	limit := landAgentDefaultCap
	if n, err := strconv.Atoi(strings.TrimSpace(pf["max_live_agents"])); err == nil && n > 0 {
		limit = n
	}
	agents, _, err := l.s.store.ListBeads(ctx, model.BeadFilter{
		Type: []model.BeadType{typeAgent}, Labels: []string{"project:" + land},
		Status: landAgentLiveStatuses, Limit: landAgentScan,
	})
	if err != nil {
		return err
	}
	others := 0
	for _, b := range agents {
		f := parseFieldsToStringMap(b.Fields)
		if b.Status == model.StatusClosed || landAgentNotLive[f["agent_state"]] || isStewardRole(f["role"]) {
			continue
		}
		others++
	}
	if others+1 >= limit {
		return fmt.Errorf("%w: land %s runs at most %d agents, its steward included, and %d are live", landmap.ErrAgentConflict, land, limit, others)
	}
	return nil
}

// isStewardRole reports whether a comma-joined role names the steward
// (wall.IsSteward).
func isStewardRole(role string) bool {
	return slices.Contains(parseRoleList(role), landsStewardRole)
}

// landAgentSuffix is four random characters of landAgentSuffixChars.
func landAgentSuffix() string {
	var b [4]byte
	_, _ = rand.Read(b[:])
	for i := range b {
		b[i] = landAgentSuffixChars[int(b[i])%len(landAgentSuffixChars)]
	}
	return string(b[:])
}

// newLandAgentBearer is 32 random bytes in hex: one base64url segment, the
// shape the land door sends to GET /v1/me (D.18a), and the shape the map's
// response denylist masks.
func newLandAgentBearer() (string, error) {
	var b [32]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	return hex.EncodeToString(b[:]), nil
}

// mintLandAgentToken registers agent-<beadID> bound to land, with a refresh,
// and persists it like POST /v1/bridge-tokens does, so it survives a restart
// and the resync reaches every replica.
func (s *BeadsServer) mintLandAgentToken(ctx context.Context, beadID, land, subject string) (landmap.AgentGrantRecord, error) {
	if s.fleetCfg == nil || s.refreshTokens == nil {
		return landmap.AgentGrantRecord{}, errors.New("token registry or refresh manager not configured")
	}
	name := agentFleetName(beadID)
	if err := validateAgentFleetRegistration(name, subject); err != nil {
		return landmap.AgentGrantRecord{}, err
	}
	bearer, err := newLandAgentBearer()
	if err != nil {
		return landmap.AgentGrantRecord{}, err
	}
	expiresAt := time.Now().Add(landAgentBearerTTL)
	s.fleetCfg.RegisterTokenFull(bearer, name, land, nil, nil, expiresAt, subject, false)
	if err := s.persistLandAgentBearer(ctx, name, bearer, land, subject, expiresAt); err != nil {
		s.fleetCfg.removeToken(bearer)
		return landmap.AgentGrantRecord{}, err
	}
	issue, err := s.refreshTokens.Issue(ctx, IssueRequest{Fleet: name, Project: land, Subject: subject, TTL: landAgentRefreshTTL})
	if err != nil {
		s.fleetCfg.removeToken(bearer)
		_ = s.store.DeleteConfig(ctx, "bridge_token:"+name)
		return landmap.AgentGrantRecord{}, err
	}
	return landmap.AgentGrantRecord{AgentID: beadID, Bearer: bearer, Refresh: issue.Plaintext, ExpiresAt: expiresAt}, nil
}

// persistLandAgentBearer writes the bridge_token:<name> row
// handleRegisterBridgeToken writes for the same registration.
func (s *BeadsServer) persistLandAgentBearer(ctx context.Context, name, bearer, land, subject string, expiresAt time.Time) error {
	value, _ := json.Marshal(map[string]any{
		"token": bearer, "project": land, "subject": subject,
		"expires_at": expiresAt.UTC().Format(time.RFC3339Nano),
	})
	return s.store.SetConfig(ctx, &model.Config{Key: "bridge_token:" + name, Value: value})
}

// RefreshAgent spends refresh, which must be agent id's own, for a new token.
// The caller is the agent on its own token, or a member who may start agents.
func (l landsForMap) RefreshAgent(ctx context.Context, land, id, refresh string) (landmap.AgentGrantRecord, error) {
	s := l.s
	own, isAgent := agentBeadIDFromContext(ctx)
	switch {
	case isAgent && own == id && TokenProjectFromContext(ctx) == land:
	case isAgent || !landAgentMayStart(ctx, land):
		return landmap.AgentGrantRecord{}, landmap.ErrAgentForbidden
	}
	b, err := l.landAgent(ctx, land, id)
	if err != nil {
		return landmap.AgentGrantRecord{}, err
	}
	if b == nil {
		return landmap.AgentGrantRecord{}, landmap.ErrAgentNotFound
	}
	if landAgentRecord(b).Stopped {
		return landmap.AgentGrantRecord{}, landmap.ErrAgentConflict
	}
	if s.refreshTokens == nil {
		return landmap.AgentGrantRecord{}, errors.New("refresh manager not configured")
	}
	name := agentFleetName(id)
	row, err := s.store.GetRefreshToken(ctx, hashRefreshToken(refresh))
	switch {
	case errors.Is(err, sql.ErrNoRows):
		return landmap.AgentGrantRecord{}, landmap.ErrAgentNotFound
	case err != nil:
		return landmap.AgentGrantRecord{}, err
	case row == nil || row.Fleet != name || row.Project != land:
		return landmap.AgentGrantRecord{}, landmap.ErrAgentNotFound
	}
	res, err := s.refreshTokens.Exchange(ctx, refresh, ExchangeOptions{Rotate: true, AccessTTL: landAgentBearerTTL})
	switch {
	case errors.Is(err, ErrRefreshNotFound), errors.Is(err, ErrRefreshRevoked), errors.Is(err, ErrRefreshExpired):
		return landmap.AgentGrantRecord{}, landmap.ErrAgentNotFound
	case err != nil:
		return landmap.AgentGrantRecord{}, err
	}
	// Exchange registers the bearer in memory only; persist it as the mint
	// does, so the agent's next request after a restart still signs in.
	if err := s.persistLandAgentBearer(ctx, name, res.AccessToken, land, row.Subject, res.AccessExpiresAt); err != nil {
		return landmap.AgentGrantRecord{}, err
	}
	return landmap.AgentGrantRecord{AgentID: id, Bearer: res.AccessToken, Refresh: res.NewRefreshToken, ExpiresAt: res.AccessExpiresAt}, nil
}

// RevokeAgent kills agent id's token and closes its bead.
func (l landsForMap) RevokeAgent(ctx context.Context, land, id string) (landmap.AgentRecord, error) {
	s := l.s
	b, err := l.landAgent(ctx, land, id)
	if err != nil {
		return landmap.AgentRecord{}, err
	}
	if b == nil {
		return landmap.AgentRecord{}, landmap.ErrAgentNotFound
	}
	operator, _, _, _ := s.resolveAuditOperatorRequired(ctx)
	if !landAgentMayRevoke(ctx, land, operator, b) {
		return landmap.AgentRecord{}, landmap.ErrAgentForbidden
	}
	// The token goes first and whatever state the bead is in: a stopped agent
	// whose token outlived it is revoked again.
	if _, _, err := s.revokeBridgeName(ctx, agentFleetName(id)); err != nil {
		return landmap.AgentRecord{}, err
	}
	if b.Status == model.StatusClosed {
		return landAgentRecord(b), nil
	}
	if operator == "" {
		operator = landAgentStarter(b)
	}
	closed, err := s.stopLandAgent(ctx, b, operator, "revoked through the Lands API")
	if err != nil {
		return landmap.AgentRecord{}, err
	}
	return landAgentRecord(closed), nil
}

// revokeFrozenLandAgents is a freeze's hold on a land's own-machine agents
// (gd-nHh7M0k3ps): an update that sets land_frozen on a project bead revokes
// every agent the Lands API started in that land, as RevokeAgent does, so
// none keeps a bearer or refresh through the freeze. A thaw gives none back:
// createAgent and refreshAgent refuse a frozen land, and after the thaw the
// members start new ones. It runs on the daemon, whoever flipped the switch
// (gb lands freeze, the Freeze button, a close), and is best-effort: a
// failure is logged and does not fail the freeze.
func (s *BeadsServer) revokeFrozenLandAgents(ctx context.Context, u *beadUpdate) {
	if u.bead.Type != "project" || !frozenValue(u.fieldChanges["land_frozen"]) {
		return
	}
	land := projectNameFromTitle(u.bead.Title)
	agents, err := s.landAgentBeads(ctx, land)
	if err != nil {
		slog.Warn("lands: freeze could not list the land's own-machine agents", "land", land, "error", err)
		return
	}
	by, _, _, _ := s.resolveAuditOperatorRequired(ctx)
	for _, b := range agents {
		if _, _, err := s.revokeBridgeName(ctx, agentFleetName(b.ID)); err != nil {
			slog.Warn("lands: freeze could not revoke an agent's token", "land", land, "agent", b.ID, "error", err)
			continue
		}
		if b.Status == model.StatusClosed {
			continue
		}
		stopBy := by
		if stopBy == "" {
			stopBy = landAgentStarter(b)
		}
		if _, err := s.stopLandAgent(ctx, b, stopBy, "its land was frozen"); err != nil {
			slog.Warn("lands: freeze could not stop an agent", "land", land, "agent", b.ID, "error", err)
		}
	}
}

// frozenValue reports whether a land_frozen value turns the switch on: a
// JSON true, or the string "true" (landFrozen reads both).
func frozenValue(v any) bool {
	switch v := v.(type) {
	case bool:
		return v
	case string:
		return strings.TrimSpace(v) == "true"
	}
	return false
}

// stopLandAgent marks the agent killed and closes its bead, as by.
func (s *BeadsServer) stopLandAgent(ctx context.Context, b *model.Bead, by, reason string) (*model.Bead, error) {
	wctx := landAgentWriteContext(ctx, by)
	if err := s.mergeBeadFields(wctx, b.ID, map[string]any{"agent_state": "killed", "close_reason": reason}); err != nil {
		return nil, err
	}
	closed, err := s.store.CloseBead(wctx, b.ID, by)
	if err != nil {
		return nil, err
	}
	s.recordAndPublish(wctx, events.TopicBeadClosed, closed.ID, by, events.BeadClosed{Bead: closed, ClosedBy: by})
	return closed, nil
}
