package main

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"strings"

	"gardenio/controller/internal/beadsapi"
	"gardenio/controller/internal/bridge"
	"gardenio/controller/internal/coopmuxapi"
	"gardenio/controller/internal/gardenhub"
	"gardenio/controller/internal/glass"
	"gardenio/controller/internal/oidcauth"
)

// glassIdentity is the human behind a /garden/glass request as the Glass relay names them upstream (gd-O31EYiDGzm): the
// verified email, the browser's cookie trust path, and only the canonical viewer/operator/admin roles the OIDC
// middleware verified. Those are the only roles coopmux's v2 envelope accepts, and the relay's tier checks read the
// same list, so gardenio-project-member and Keycloak's own realm roles grant nothing here.
func glassIdentity(r *http.Request) coopmuxapi.Identity {
	ctx := r.Context()
	var roles []string
	for _, role := range oidcauth.RolesFromContext(ctx) {
		if _, ok := glass.TierForRole(role); ok {
			roles = append(roles, role)
		}
	}
	return coopmuxapi.Identity{
		Operator: strings.ToLower(oidcauth.EmailFromContext(ctx)),
		AuthMode: "cookie",
		Roles:    roles,
	}
}

// glassNudger is the Glass's nudge (Lands GL-7, G-D4): the bridge's one nudge path, NudgeAgent (an interrupt mail the
// agent's mail subscriber hands to coop), with its failures said as the Glass says them.
func glassNudger(daemon bridge.NudgeBeadClient, logger *slog.Logger) func(ctx context.Context, agent, message string) gardenhub.NudgeOutcome {
	return func(ctx context.Context, agent, message string) gardenhub.NudgeOutcome {
		return nudgeOutcomeOf(bridge.NudgeAgent(ctx, daemon, logger, agent, message))
	}
}

// nudgeOutcomeOf classifies a NudgeAgent error: stopped (a terminal or lame-duck agent) is not gone (no agent bead
// by that name). The mail is written whether or not the pod is up, so there is no busy outcome any more.
func nudgeOutcomeOf(err error) gardenhub.NudgeOutcome {
	switch {
	case err == nil:
		return gardenhub.NudgeDelivered
	case errors.Is(err, bridge.ErrAgentNotNudgeable):
		return gardenhub.NudgeStopped
	case errors.Is(err, beadsapi.ErrAgentNotFound):
		return gardenhub.NudgeGone
	}
	return gardenhub.NudgeFailed
}
