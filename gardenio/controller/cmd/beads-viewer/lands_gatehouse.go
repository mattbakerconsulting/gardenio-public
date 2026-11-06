package main

import (
	"fmt"
	"net/http"
	"net/url"
	"slices"
	"strings"

	"gardenio/controller/internal/keycloakadmin"
)

// The gatehouse (Lands Track B step 9.12, gd-vXokVcVUf8): the tenant land
// every signed-in person with no land is a guest of. The controller's
// provisioner makes them its guests (internal/lands/provision/gatehouse.go);
// here, "/" for a person whose only land is the gatehouse opens its steward,
// which greets and answers newcomers in its #general, instead of the feed.
//
// The Lands home (/lands/) carries nothing about the caller (routePublic, no
// session), so it cannot tell who is signed in; with a gatehouse configured it
// says where someone already let in will land, and that every newcomer reads
// the gatehouse's #general (decision D2 no-room).

// landsGatehouseLandEnv names the gatehouse; the controller reads the same
// variable.
const landsGatehouseLandEnv = "LANDS_GATEHOUSE_LAND"

// landsGatehouseFromEnv reads LANDS_GATEHOUSE_LAND: "" when unset, and an
// error when it is not a name a land can have.
func landsGatehouseFromEnv(getenv func(string) string) (string, error) {
	land := strings.TrimSpace(getenv(landsGatehouseLandEnv))
	if land == "" {
		return "", nil
	}
	if _, err := keycloakadmin.GroupName(land); err != nil {
		return "", fmt.Errorf("%s=%q: %w", landsGatehouseLandEnv, land, err)
	}
	return land, nil
}

// gatehouseHome is where "/" sends r's caller: the gatehouse's steward, which
// greets in its #general (decision D2 no-room), when
// the caller is a scoped principal whose only land is the gatehouse, else "".
// An admin, and anyone with a land of their own, keeps the usual front door.
func (s *Server) gatehouseHome(r *http.Request) string {
	if s.landsGatehouse == "" || s.commons == nil {
		return ""
	}
	sc := viewerScopeFromContext(r.Context())
	if !sc.Scoped() || !slices.Equal(sc.Projects, []string{s.landsGatehouse}) {
		return ""
	}
	return s.basePath + "/garden/c/s/" + url.PathEscape(s.landsGatehouse)
}
