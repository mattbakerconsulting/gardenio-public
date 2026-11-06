package server

import (
	_ "embed"
	"fmt"
	"net/http"
	"strings"
)

// The land door's route allowlist (Door D.18 gd-nvvTcZeBZD). A request that
// came through the land door (LandDoorHeader set) reaches only the routes in
// land_door_routes.txt: the Lands API that gl speaks, and the daemon routes a
// tenant's agent loop (kd and gb, owner choice C, doc gd-K7BUSVVHJF) calls.
// Every other route answers 404 on the land path, whoever asks.
//
// The file is the one list. The edge's $land_door_route map
// (gardenio/helm/gardenio-ziti/public-edge.yaml) and landgate's copy
// (gardenio/controller/cmd/landgate/land_door_routes.txt) are held to it by
// tests, so a route opened in one place and not the others fails the build.

//go:embed land_door_routes.txt
var landDoorRoutesFile string

// landDoorRoutes is the allowlist, keyed by the exact mux pattern string
// registerRoutes registers ("GET /v1/beads/{id}"). A file that does not parse
// leaves it nil, which closes every route on the land door;
// TestLandDoorRoutes_Parse fails the build first.
var landDoorRoutes, landDoorRoutesErr = parseLandDoorRoutes(landDoorRoutesFile)

// parseLandDoorRoutes reads the allowlist: one "METHOD /pattern" per line,
// "#" starts a comment.
func parseLandDoorRoutes(text string) (map[string]bool, error) {
	routes := map[string]bool{}
	for i, line := range strings.Split(text, "\n") {
		line, _, _ = strings.Cut(line, "#")
		fields := strings.Fields(line)
		if len(fields) == 0 {
			continue
		}
		if len(fields) != 2 || !strings.HasPrefix(fields[1], "/v1/") || strings.ToUpper(fields[0]) != fields[0] {
			return nil, fmt.Errorf("land_door_routes.txt:%d: want METHOD /v1/pattern, got %q", i+1, line)
		}
		key := fields[0] + " " + fields[1]
		if routes[key] {
			return nil, fmt.Errorf("land_door_routes.txt:%d: %s listed twice", i+1, key)
		}
		routes[key] = true
	}
	return routes, nil
}

// landDoorRouteClosed is the 404's message. It names no route and no reason
// beyond the door, as the edge's bare 404 does.
const landDoorRouteClosed = "not found on the land door"

// landDoorRouteGate answers 404 to a land-door request for a route the
// allowlist does not name. It reads r.Pattern, so scopeGatedMux installs it
// per route, inside the mux match. The Lands map's in-process calls carry no
// LandDoorHeader (landmap/daemon.go builds them fresh), so the map still
// reaches the daemon routes in its own route table.
func landDoorRouteGate(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if len(r.Header.Values(LandDoorHeader)) > 0 && !landDoorRoutes[r.Pattern] {
			writeError(w, http.StatusNotFound, landDoorRouteClosed)
			return
		}
		next.ServeHTTP(w, r)
	})
}
