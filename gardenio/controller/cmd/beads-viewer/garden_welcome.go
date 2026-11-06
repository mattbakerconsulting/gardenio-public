package main

import (
	"context"
	"errors"
	"net/http"
	"slices"

	"gardenio/controller/internal/beadsapi"
	"gardenio/controller/internal/gardenhub"
)

// The /garden welcome tour (Lands Track B step 9.14b, gd-aEIUgpyxpi;
// gardenhub welcome.go, web/tour.js TOUR.welcome). A land's person, a scoped
// principal, is walked round once on their first /garden: their land, how to
// ask, and its #general, where its steward greeted them (step 9.14). Whether
// they have had it is welcome_tour_seen_at on their own user bead, which
// kbeads reads and stamps on their own bearer alone (/v1/users/me/welcome-tour,
// classScopedSafe), so a second device does not run it again. An unrestricted
// viewer (an operator, an admin) is never toured: /garden is theirs already.
// POST /garden/welcome is routeScoped: it writes only the caller's own flag.

// welcomeTours is the slice of the daemon client the tour's flag goes through.
type welcomeTours interface {
	MyWelcomeTour(ctx context.Context) (*beadsapi.WelcomeTour, error)
	SeenMyWelcomeTour(ctx context.Context) (*beadsapi.WelcomeTour, error)
}

// welcomeLand is the land a scoped person's welcome tour shows them round: the
// first of their own lands by name, else the gatehouse, where everyone with no
// land of their own is a guest. "" for an unrestricted caller or one with no
// land.
func welcomeLand(sc viewerScope, gatehouse string) string {
	if !sc.Scoped() || len(sc.Projects) == 0 {
		return ""
	}
	lands := slices.Sorted(slices.Values(sc.Projects))
	for _, l := range lands {
		if l != gatehouse {
			return l
		}
	}
	return lands[0]
}

// welcomeCtx is ctx on which a daemon call authenticates as the signed-in
// person and nothing else; with no bearer it fails closed, unsent.
func welcomeCtx(ctx context.Context) context.Context {
	return beadsapi.WithUserBearerOnly(ctx, beadsapi.UserBearerFromContext(ctx))
}

// gardenWelcome is gardenhub.Handler.Welcome: the tour r's page is due, the
// zero value when the caller is not a land's person, has had it, or the flag
// cannot be read (the page then runs no tour; the next visit asks again).
func (s *Server) gardenWelcome(r *http.Request) gardenhub.WelcomeTour {
	sc := viewerScopeFromContext(r.Context())
	land := welcomeLand(sc, s.landsGatehouse)
	if land == "" {
		return gardenhub.WelcomeTour{}
	}
	wt, ok := s.daemon.(welcomeTours)
	if !ok {
		return gardenhub.WelcomeTour{}
	}
	got, err := wt.MyWelcomeTour(welcomeCtx(r.Context()))
	if err != nil {
		s.logger.Debug("garden welcome: reading the seen flag", "error", err)
		return gardenhub.WelcomeTour{}
	}
	if got == nil || got.SeenAt != "" {
		return gardenhub.WelcomeTour{}
	}
	return gardenhub.WelcomeTour{Land: land, Role: string(sc.landRole(land)), Gate: land == s.landsGatehouse}
}

// errNoWelcomeTours is gardenWelcomeSeen's answer when the daemon client keeps
// no welcome tour flag.
var errNoWelcomeTours = errors.New("the daemon client keeps no welcome tour flag")

// gardenWelcomeSeen is gardenhub.Handler.WelcomeSeen: the caller has had the
// tour, recorded on their own user bead.
func (s *Server) gardenWelcomeSeen(r *http.Request) error {
	wt, ok := s.daemon.(welcomeTours)
	if !ok {
		return errNoWelcomeTours
	}
	if _, err := wt.SeenMyWelcomeTour(welcomeCtx(r.Context())); err != nil {
		s.logger.Warn("garden welcome: stamping the seen flag", "error", err)
		return err
	}
	return nil
}
