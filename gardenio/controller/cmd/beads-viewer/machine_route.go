package main

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"gardenio/controller/internal/beadsapi"
	"gardenio/controller/internal/oidcauth"
)

// The machine routes (routeMachine, gd-qnF6mJZtkQ): in-cluster callers that
// hold a machine principal, today the slack-bridge fetching a Commons file to
// upload into Slack. The viewer cannot verify such a caller's token itself:
// behind the edge it runs devAuthBypass, which verifies no bearer, and its OIDC
// mode wants a human's audience and email, so a service-account token is
// refused there before this runs (the bridge then keeps its viewer links). The
// daemon can: it verifies every bearer and classifies a Keycloak service
// account as machine:<client>. So machineMiddleware asks kbeads who the
// request's own bearer is, on that bearer alone, and admits only a machine
// principal. scopeMiddleware refuses a routeMachine route to every request
// this did not admit.
//
// An agent's own per-agent token is admitted too (gd-WXY7Jm5ugY, Commons II
// 3.14): `gb commons file` fetches an attachment an agent was nudged with. The
// daemon names such a token by its agent and its land (token_project), and the
// agent reads only its own land's files (machineCaller.mayRead).

// machineMeTimeout bounds the daemon's answer about one caller.
const machineMeTimeout = 5 * time.Second

// machineSubjectPrefix is how kbeads names a machine principal's subject
// (machine:<client id>).
const machineSubjectPrefix = "machine:"

// machineCaller is the principal machineMiddleware admitted.
type machineCaller struct {
	// Subject is the daemon's operator id for the bearer: machine:<client>,
	// or an agent token's subject.
	Subject string
	// Agent and Land are set for a per-agent token: the agent's name and the
	// one land its token is bound to. A service account has neither.
	Agent string
	Land  string
}

// mayRead reports whether c may read files in land: a service account any
// land, an agent only its own.
func (c machineCaller) mayRead(land string) bool {
	return c.Agent == "" || c.Land == land
}

type machineCallerKey struct{}

func withMachineCaller(ctx context.Context, c machineCaller) context.Context {
	return context.WithValue(ctx, machineCallerKey{}, c)
}

// machineCallerFromContext returns the caller machineMiddleware admitted, if any.
func machineCallerFromContext(ctx context.Context) (machineCaller, bool) {
	c, ok := ctx.Value(machineCallerKey{}).(machineCaller)
	return c, ok
}

// machineWho asks the daemon who a bearer is (beadsapi.Client.MeAs).
type machineWho func(ctx context.Context, bearer string) (*beadsapi.MeResponse, error)

// machineFromMe is the machine principal a /v1/me answer names, if it names
// one: a service account (machine:<client>), or a per-agent fleet token, which
// answers with its agent, its agent bead and the land it is bound to. An agent
// token without all three is not one. The file route still reads the channel
// on the caller's bearer before it serves a byte.
func machineFromMe(me *beadsapi.MeResponse) (machineCaller, bool) {
	if me == nil {
		return machineCaller{}, false
	}
	if me.Agent != "" || me.AgentBead != "" {
		if me.Agent == "" || me.AgentBead == "" || me.TokenProject == "" {
			return machineCaller{}, false
		}
		return machineCaller{Subject: me.OperatorID, Agent: me.Agent, Land: me.TokenProject}, true
	}
	client, ok := strings.CutPrefix(me.OperatorID, machineSubjectPrefix)
	if !ok || client == "" {
		return machineCaller{}, false
	}
	return machineCaller{Subject: me.OperatorID}, true
}

// machineMiddleware admits a machine principal to the routeMachine routes. For
// any other route it does nothing. A request that carries a viewer identity (a
// human the edge signed in) or no bearer is passed on unadmitted, and
// scopeMiddleware answers 403; so is one the daemon does not name a machine
// principal. A daemon that cannot answer is a 502.
//
// It runs after operatorMiddleware (which put the request's bearer on the
// context) and before scopeMiddleware.
func machineMiddleware(next http.Handler, patternOf func(*http.Request) string, who machineWho, logger *slog.Logger) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if viewerRouteClasses[patternOf(r)] != routeMachine || who == nil {
			next.ServeHTTP(w, r)
			return
		}
		bearer := beadsapi.UserBearerFromContext(r.Context())
		if bearer == "" || oidcauth.EmailFromContext(r.Context()) != "" {
			next.ServeHTTP(w, r)
			return
		}
		ctx, cancel := context.WithTimeout(r.Context(), machineMeTimeout)
		me, err := who(ctx, bearer)
		cancel()
		if err != nil {
			var apiErr *beadsapi.APIError
			if errors.As(err, &apiErr) && (apiErr.StatusCode == http.StatusUnauthorized || apiErr.StatusCode == http.StatusForbidden) {
				next.ServeHTTP(w, r) // a bearer the daemon refuses: not admitted
				return
			}
			logger.Warn("machine route: the daemon did not say who the caller is", "path", r.URL.Path, "error", err)
			http.Error(w, "could not verify the caller", http.StatusBadGateway)
			return
		}
		c, ok := machineFromMe(me)
		if !ok {
			next.ServeHTTP(w, r)
			return
		}
		next.ServeHTTP(w, r.WithContext(withMachineCaller(r.Context(), c)))
	})
}
