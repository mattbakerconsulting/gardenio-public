package bridge

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"

	"gardenio/controller/internal/beadsapi"
)

// Who calls the conversation agent routes (gd-P5wjHDweih; Lands Track B spec
// sections 5.1 and 5.3). bridgeRBAC admits a caller to POST
// /api/conversation/say, GET /api/conversation/thread and the legacy GET
// /api/slack/threads one of three ways, and marks the request context:
//
//   - an agent's own per-agent daemon token, in AgentTokenHeader, which the
//     daemon resolves to the agent and the project the token is bound to
//     (AgentTokenVerifier, WithAgentCaller). The agent is the token's: a ?agent=
//     or body agent that names another agent is refused, and the land is the
//     token's project. A tenant pod holds this token and no bridge bearer;
//   - an OIDC admin, an operator debugging (WithOperatorCaller): the request
//     names the agent, as before;
//   - the shared bridge bearer (SLACK_BRIDGE_AGENT_TOKEN), unmarked. The
//     agent it names is a claim any holder of the bearer can make, so the
//     bearer reaches only operator lands: a named agent or a target in a tenant
//     land is refused. SetSharedBearerAgents(false) (10.3, once agent pods
//     carry no bearer) refuses it on these routes altogether.

// AgentTokenHeader carries an agent's per-agent daemon token to the bridge.
// gb sends it beside the shared bearer, so a bridge that predates it still
// admits the call by the bearer.
const AgentTokenHeader = "X-Gardenio-Agent-Token"

// AgentCaller is the agent a per-agent daemon token names, as the daemon
// verified it.
type AgentCaller struct {
	Agent  string // the agent's name
	BeadID string // its agent bead
	Land   string // the project the token is bound to ("" when unbound)
}

type agentCallerKey struct{}

type operatorCallerKey struct{}

// WithAgentCaller marks ctx as a call authenticated by c's per-agent token.
func WithAgentCaller(ctx context.Context, c AgentCaller) context.Context {
	return context.WithValue(ctx, agentCallerKey{}, c)
}

// AgentCallerFromContext returns the agent a per-agent token on ctx names.
func AgentCallerFromContext(ctx context.Context) (AgentCaller, bool) {
	c, ok := ctx.Value(agentCallerKey{}).(AgentCaller)
	return c, ok
}

// WithOperatorCaller marks ctx as a call by an OIDC admin.
func WithOperatorCaller(ctx context.Context) context.Context {
	return context.WithValue(ctx, operatorCallerKey{}, true)
}

func operatorCaller(ctx context.Context) bool {
	v, _ := ctx.Value(operatorCallerKey{}).(bool)
	return v
}

// IsAgentTokenRoute reports whether a request is one of the routes that take
// an agent's identity from its per-agent token: the conversation routes, and
// the permission callback (askerOf, gd-eemeNhUt5I).
func IsAgentTokenRoute(method, path string) bool {
	switch path {
	case "/api/conversation/say", "/api/permission/callback":
		return method == http.MethodPost
	case "/api/conversation/thread", "/api/slack/threads":
		return method == http.MethodGet
	}
	return false
}

// ErrNotAgentToken is a token the daemon accepts that is not an agent's own
// (the primary static token of a bridge-less install, an operator's session).
// It names no agent; the call is admitted, or not, by the bridge bearer.
var ErrNotAgentToken = errors.New("not a per-agent daemon token")

// ErrAgentTokenRejected is a token the daemon refuses: unknown, expired or
// revoked.
var ErrAgentTokenRejected = errors.New("agent token rejected by the beads daemon")

// agentTokenCacheTTL bounds how long a verified token is trusted without
// asking the daemon again, so a revoked token stops working within it.
const agentTokenCacheTTL = 30 * time.Second

// agentTokenCacheMax bounds the verifier cache; expired entries are dropped
// when it is reached.
const agentTokenCacheMax = 1024

// AgentTokenVerifier resolves a per-agent daemon token to its agent through
// the daemon's GET /v1/me. A per-agent token is an opaque fleet token, so the
// daemon is the only party that can say whose it is: it reports the agent,
// the agent bead and the token's project from the credential, never from
// X-Actor. Answers are cached by the token's hash for agentTokenCacheTTL;
// refusals are not cached.
type AgentTokenVerifier struct {
	meURL  string
	client *http.Client
	now    func() time.Time

	mu    sync.Mutex
	cache map[[sha256.Size]byte]verifiedToken
}

type verifiedToken struct {
	caller AgentCaller
	err    error // nil or ErrNotAgentToken
	until  time.Time
}

// NewAgentTokenVerifier returns a verifier asking the daemon at daemonURL.
// client defaults to one with a 5s timeout.
func NewAgentTokenVerifier(daemonURL string, client *http.Client) *AgentTokenVerifier {
	if client == nil {
		client = &http.Client{Timeout: 5 * time.Second}
	}
	return &AgentTokenVerifier{
		meURL:  strings.TrimRight(daemonURL, "/") + "/v1/me",
		client: client,
		now:    time.Now,
		cache:  map[[sha256.Size]byte]verifiedToken{},
	}
}

// Verify returns the agent token names. It fails with ErrNotAgentToken for a
// token the daemon accepts as someone else's, ErrAgentTokenRejected for one
// it refuses, and any other error when the daemon cannot be asked.
func (v *AgentTokenVerifier) Verify(ctx context.Context, token string) (AgentCaller, error) {
	token = strings.TrimSpace(token)
	if token == "" {
		return AgentCaller{}, ErrAgentTokenRejected
	}
	key := sha256.Sum256([]byte(token))
	now := v.now()
	v.mu.Lock()
	hit, ok := v.cache[key]
	v.mu.Unlock()
	if ok && now.Before(hit.until) {
		return hit.caller, hit.err
	}

	caller, err := v.ask(ctx, token)
	if err != nil && !errors.Is(err, ErrNotAgentToken) {
		return AgentCaller{}, err
	}
	v.mu.Lock()
	if len(v.cache) >= agentTokenCacheMax {
		for k, e := range v.cache {
			if !now.Before(e.until) {
				delete(v.cache, k)
			}
		}
		if len(v.cache) >= agentTokenCacheMax {
			clear(v.cache)
		}
	}
	v.cache[key] = verifiedToken{caller: caller, err: err, until: now.Add(agentTokenCacheTTL)}
	v.mu.Unlock()
	return caller, err
}

func (v *AgentTokenVerifier) ask(ctx context.Context, token string) (AgentCaller, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, v.meURL, nil)
	if err != nil {
		return AgentCaller{}, err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := v.client.Do(req)
	if err != nil {
		return AgentCaller{}, fmt.Errorf("verifying agent token: %w", err)
	}
	defer resp.Body.Close()
	switch {
	case resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden:
		return AgentCaller{}, ErrAgentTokenRejected
	case resp.StatusCode != http.StatusOK:
		return AgentCaller{}, fmt.Errorf("verifying agent token: daemon returned HTTP %d", resp.StatusCode)
	}
	var me beadsapi.MeResponse
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&me); err != nil {
		return AgentCaller{}, fmt.Errorf("verifying agent token: %w", err)
	}
	if me.Agent == "" {
		return AgentCaller{}, ErrNotAgentToken
	}
	return AgentCaller{Agent: me.Agent, BeadID: me.AgentBead, Land: me.TokenProject}, nil
}

// SetSharedBearerAgents sets whether the shared bridge bearer may still name
// an agent on the conversation routes (operator lands only). It is on by
// default; 10.3 turns it off once agent pods carry no bearer.
func (a *ConversationAgentAPI) SetSharedBearerAgents(on bool) {
	a.refuseSharedBearer = !on
}

// callerSpeaker resolves the agent a say or read is made as, from the
// request's caller (see the top of this file). named is the agent the request
// names (?agent= or the body's agent), "" for none.
//
// sharedBearer is true when the caller is the shared bridge bearer, whose
// target must then also be outside tenant lands (fenceTarget).
func (a *ConversationAgentAPI) callerSpeaker(ctx context.Context, named string) (sp speaker, sharedBearer bool, err error) {
	named = extractAgentName(named)
	if c, ok := AgentCallerFromContext(ctx); ok {
		if named != "" && named != c.Agent {
			return speaker{}, false, sayErrorf(http.StatusForbidden,
				"the agent token is agent %s's; the request names agent %s", c.Agent, named)
		}
		sp = a.tokenSpeaker(ctx, c)
		kind, err := a.bot.tenantKind(ctx, sp.land)
		if err != nil {
			return speaker{}, false, err
		}
		sp.coordsWalled = kind != ""
		return sp, false, nil
	}
	if named == "" {
		return speaker{}, false, sayErrorf(http.StatusBadRequest, "agent is required")
	}
	if operatorCaller(ctx) {
		return a.speakerOf(ctx, named), false, nil
	}
	if a.refuseSharedBearer {
		return speaker{}, true, errSharedBearerRetired
	}
	sp = a.speakerOf(ctx, named)
	if err := a.bot.refuseTenantLand(ctx, sp.land, "agent "+named); err != nil {
		return speaker{}, true, err
	}
	return sp, true, nil
}

var errSharedBearerRetired = sayErrorf(http.StatusUnauthorized,
	"the shared bridge bearer no longer names an agent; send the agent's own daemon token in "+AgentTokenHeader)

// tokenSpeaker is the speaker a per-agent token names: its land is the
// token's project, and its thread binding is read from its own agent bead, by
// the bead id the token carries (never by name: names are not unique across
// lands).
func (a *ConversationAgentAPI) tokenSpeaker(ctx context.Context, c AgentCaller) speaker {
	sp := speaker{agent: c.Agent, land: c.Land, beadRead: true, fromToken: true}
	if a.bot.daemon == nil || c.BeadID == "" {
		return sp
	}
	b, err := a.bot.daemon.GetBead(ctx, c.BeadID)
	if err != nil || b == nil || b.Type != "agent" {
		if err != nil {
			a.logger.Warn("conversation land wall: reading the token's agent bead failed",
				"agent", c.Agent, "bead", c.BeadID, "error", err)
		}
		return sp
	}
	if sp.land == "" {
		sp.land = projectFromLabels(b.Labels)
		if sp.land == "" {
			sp.land = strings.TrimSpace(b.Fields["project"])
		}
	}
	sp.threadCh, sp.threadTS = b.Fields["slack_thread_channel"], b.Fields["slack_thread_ts"]
	return sp
}

// fenceTarget is the wall's last check on a resolved target t, by caller.
//
//   - The shared bearer reaches no target in a tenant land.
//   - A token's agent of an operator land says by Slack coordinates in another
//     land's thread as before (resolveTarget exempts that), unless the thread
//     is in a tenant land: there it says only in its own thread. (A tenant
//     land's agent is walled by coordinates in resolveTarget already.)
func (a *ConversationAgentAPI) fenceTarget(ctx context.Context, sp *speaker, sharedBearer bool, t sayTarget, headID, channel, threadTS string) error {
	if sharedBearer {
		return a.bot.refuseTenantLand(ctx, t.ch.Land, targetName(t, headID, channel, threadTS))
	}
	if !sp.fromToken || sp.walledByCoords() || t.named || t.ch.Land == "" || t.ch.Land == sp.land {
		return nil
	}
	kind, err := a.bot.tenantKind(ctx, t.ch.Land)
	if err != nil || kind == "" || a.readsOwnThread(ctx, sp, channel, threadTS) {
		return err
	}
	return sayErrorf(http.StatusForbidden,
		"%s is in %s land %q; an agent of %q may speak there only in its own thread",
		targetName(t, headID, channel, threadTS), kind, t.ch.Land, sp.land)
}

// refuseTenantLand refuses land, the land of what is named, when it is a
// tenant land: the shared bearer reaches operator lands only.
func (b *Bot) refuseTenantLand(ctx context.Context, land, what string) error {
	kind, err := b.tenantKind(ctx, land)
	if err != nil || kind == "" {
		return err
	}
	return sayErrorf(http.StatusForbidden,
		"%s is in %s land %q; the shared bridge bearer reaches only operator lands: send the agent's own daemon token in %s",
		what, kind, land, AgentTokenHeader)
}

// tenantKind returns land's land_kind when its project bead makes it other
// than an operator land (IsOperatorLand), else "". No land, and a land with no
// project bead (whose land_kind is unset, so the operator's), are operator
// lands. A daemon that cannot list the lands fails the call.
func (b *Bot) tenantKind(ctx context.Context, land string) (string, error) {
	if land == "" || b.daemon == nil {
		return "", nil
	}
	projects, err := b.daemon.ListProjectBeads(ctx)
	if err != nil {
		return "", fmt.Errorf("reading land %s: %w", land, err)
	}
	if p, ok := projects[land]; ok && !p.IsOperatorLand() {
		return p.LandKind, nil
	}
	return "", nil
}
