package main

import (
	"context"
	"net/http"
	"sync"

	"gardenio/controller/internal/bridge"
)

// The bridge's conversation routes (gb say, gb thread, and the legacy thread
// read) take the speaking agent from its own per-agent daemon token, not from
// the agent gb names (gd-P5wjHDweih). A tenant pod holds that token and no
// bridge bearer (Lands Track B spec section 5.3), so gb sends it in
// bridge.AgentTokenHeader beside SLACK_BRIDGE_TOKEN; a bridge that predates
// the header admits the call by the bearer as before.

var (
	bridgeAgentOnce                 sync.Once
	bridgeAgentName, bridgeAgentTok string
)

// bridgeAgentIdentity returns the agent gb's daemon credential is, and the
// credential, when it is that agent's own per-agent token: the daemon says so
// in GET /v1/me, which also refreshes an expired token first. ("", "") for
// any other credential (the shared static token, an operator's session), or
// when the daemon cannot say: gb then sends no token, and never hands the
// bridge a credential that is not an agent's own. Asked once per process.
func bridgeAgentIdentity(ctx context.Context) (agent, token string) {
	bridgeAgentOnce.Do(func() {
		if daemon == nil {
			return
		}
		me, err := daemon.Me(ctx)
		if err != nil || me.Agent == "" {
			return
		}
		if tok := daemon.StaticToken(); tok != "" {
			bridgeAgentName, bridgeAgentTok = me.Agent, tok
		}
	})
	return bridgeAgentName, bridgeAgentTok
}

// bridgeAgent is the agent gb names to the bridge: its token's agent when it
// has one (the bridge refuses any other), else actor.
func bridgeAgent(ctx context.Context) string {
	if agent, _ := bridgeAgentIdentity(ctx); agent != "" {
		return agent
	}
	return actor
}

// setBridgeAgentToken adds the agent's per-agent token to a request for one
// of the bridge's conversation routes, when gb has one.
func setBridgeAgentToken(ctx context.Context, req *http.Request) {
	if _, tok := bridgeAgentIdentity(ctx); tok != "" {
		req.Header.Set(bridge.AgentTokenHeader, tok)
	}
}
