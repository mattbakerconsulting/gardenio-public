package landmap

import (
	"errors"
	"net/http"
	"regexp"
	"strings"
	"time"

	"gardenio/lands/contract/landsv1"
)

// A member's own agent (D.17 gd-foQLqIRXDq, owner choice C in doc
// gd-K7BUSVVHJF): an agent a land member runs on their own machine, under a
// credential the land mints for it. The map only speaks the vocabulary; the
// server's Lands implementation judges the caller (owners and members on
// their own sign-in, never a guest or an agent), creates the agent's record,
// mints and revokes its credential, and holds the land's live agent cap. The
// credential is an agent's, at the restricted agent tier, whatever the
// member's own role.

// AgentRecord is one agent a member started on their own machine.
type AgentRecord struct {
	ID, Name string
	// StartedBy is the member who started it, as the server names a person
	// (displayName shows it).
	StartedBy string
	StartedAt time.Time
	// Stopped is set once the agent is revoked or has stopped.
	Stopped bool
}

// AgentGrantRecord is a freshly minted credential: the bearer the agent sends
// at the land's door, the refresh it trades for the next one, and when the
// bearer expires.
type AgentGrantRecord struct {
	AgentID, Bearer, Refresh string
	ExpiresAt                time.Time
}

// AgentMintRequest is what createAgent asks for: the agent's name (the server
// adds a suffix) and the request's Idempotency-Key, "" when none.
type AgentMintRequest struct {
	Name, IdempotencyKey string
}

// The refusals a Lands agent call answers with; anything else it returns is a
// failure (fail). They are the contract's words for what the server decided.
var (
	// ErrAgentForbidden: the caller may not do this (a guest, an agent
	// minting, a member revoking another's agent).
	ErrAgentForbidden = errors.New("landmap: agent call forbidden")
	// ErrAgentNotFound: no such agent in the land, or a refresh not its own.
	ErrAgentNotFound = errors.New("landmap: no such agent")
	// ErrAgentConflict: the land runs all the agents it may, or the agent has
	// stopped.
	ErrAgentConflict = errors.New("landmap: agent conflict")
)

// agentNameDefault is an agent's name when createAgent gives none.
const agentNameDefault = "laptop"

// agentName is AgentCreate.name's pattern.
var agentName = regexp.MustCompile(`^[a-z][a-z0-9-]{0,31}$`)

// maxCredential is AgentGrant.bearer and refresh's maxLength, and
// AgentRefresh.refresh's.
const maxCredential = 512

func (m *Map) ListAgents(w http.ResponseWriter, r *http.Request, land landsv1.LandPath, params landsv1.ListAgentsParams) {
	off, n, ok := pageParams(w, params.Cursor, params.Limit)
	if !ok || !m.admit(w, r, land) {
		return
	}
	recs, err := m.lands.Agents(r.Context(), land)
	if err != nil {
		agentFail(w, r, "listAgents", err)
		return
	}
	all := make([]landsv1.Agent, 0, len(recs))
	for _, rec := range recs {
		all = append(all, toAgent(rec))
	}
	items, next := pageOf(all, off, n)
	writeJSON(w, http.StatusOK, landsv1.AgentPage{Items: items, NextCursor: next})
}

func (m *Map) GetAgent(w http.ResponseWriter, r *http.Request, land landsv1.LandPath, agent landsv1.AgentPath) {
	if !m.admit(w, r, land) {
		return
	}
	rec, ok, err := m.lands.Agent(r.Context(), land, agent)
	if err != nil {
		agentFail(w, r, "getAgent", err)
		return
	}
	if !ok {
		problem(w, landsv1.NotFound, "")
		return
	}
	writeJSON(w, http.StatusOK, toAgent(rec))
}

func (m *Map) CreateAgent(w http.ResponseWriter, r *http.Request, land landsv1.LandPath, params landsv1.CreateAgentParams) {
	if !m.writable(w, r, "createAgent", land) {
		return
	}
	var in landsv1.AgentCreate
	if !decodeBody(w, r, &in) {
		return
	}
	req := AgentMintRequest{Name: agentNameDefault}
	if in.Name != nil {
		if !agentName.MatchString(*in.Name) {
			problem(w, landsv1.InvalidRequest, "name must be a lowercase letter, then up to 31 lowercase letters, digits or hyphens")
			return
		}
		req.Name = *in.Name
	}
	if params.IdempotencyKey != nil {
		req.IdempotencyKey = *params.IdempotencyKey
	}
	grant, err := m.lands.MintAgent(r.Context(), land, req)
	if err != nil {
		agentFail(w, r, "createAgent", err)
		return
	}
	writeGrant(w, r, http.StatusCreated, land, grant)
}

func (m *Map) RefreshAgent(w http.ResponseWriter, r *http.Request, land landsv1.LandPath, agent landsv1.AgentPath, _ landsv1.RefreshAgentParams) {
	if !m.writable(w, r, "refreshAgent", land) {
		return
	}
	var in landsv1.AgentRefresh
	if !decodeBody(w, r, &in) || !textOK(w, "refresh", in.Refresh, 1, maxCredential) {
		return
	}
	grant, err := m.lands.RefreshAgent(r.Context(), land, agent, in.Refresh)
	if err != nil {
		agentFail(w, r, "refreshAgent", err)
		return
	}
	writeGrant(w, r, http.StatusOK, land, grant)
}

// RevokeAgent takes only admit: stopping an agent is not a write a paused
// land refuses.
func (m *Map) RevokeAgent(w http.ResponseWriter, r *http.Request, land landsv1.LandPath, agent landsv1.AgentPath, _ landsv1.RevokeAgentParams) {
	if !m.admit(w, r, land) {
		return
	}
	rec, err := m.lands.RevokeAgent(r.Context(), land, agent)
	if err != nil {
		agentFail(w, r, "revokeAgent", err)
		return
	}
	writeJSON(w, http.StatusOK, toAgent(rec))
}

func toAgent(rec AgentRecord) landsv1.Agent {
	out := landsv1.Agent{
		Id: rec.ID, Name: rec.Name, Status: landsv1.AgentStatusActive,
		StartedBy: displayName(rec.StartedBy), StartedAt: stamp(rec.StartedAt),
	}
	if out.StartedBy == "" {
		out.StartedBy = "unknown"
	}
	if rec.Stopped {
		out.Status = landsv1.AgentStatusStopped
	}
	return out
}

// writeGrant answers a credential, never cached.
func writeGrant(w http.ResponseWriter, r *http.Request, status int, land string, g AgentGrantRecord) {
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, status, landsv1.AgentGrant{
		AgentId: g.AgentID, Bearer: g.Bearer, Refresh: g.Refresh,
		ExpiresAt: stamp(g.ExpiresAt), DoorUrl: doorURL(r, land),
	})
}

// doorURL is the land's door at the public edge the request came through,
// which forwards its own host (/land/<name>, D.2). An agent's kd and gb use it
// as their daemon address; gl's base URL is it plus /v1.
func doorURL(r *http.Request, land string) string {
	host := strings.TrimSpace(r.Host)
	if host == "" {
		host = "localhost"
	}
	return "https://" + host + "/land/" + land
}

// agentFail answers a Lands agent refusal with its code, and anything else as
// a failure.
func agentFail(w http.ResponseWriter, r *http.Request, op string, err error) {
	switch {
	case errors.Is(err, ErrAgentForbidden):
		problem(w, landsv1.Forbidden, "")
	case errors.Is(err, ErrAgentNotFound):
		problem(w, landsv1.NotFound, "")
	case errors.Is(err, ErrAgentConflict):
		problem(w, landsv1.Conflict, "")
	default:
		fail(w, r, op, err)
	}
}
