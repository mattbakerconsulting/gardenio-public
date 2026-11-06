package main

import (
	_ "embed"
	"fmt"
	"net/http"
	"path"
	"strings"
)

// The land door's route allowlist (Door D.18 gd-nvvTcZeBZD). landgate
// forwards only the daemon routes in land_door_routes.txt, a byte-for-byte
// copy of kbeads/internal/server/land_door_routes.txt (TestEdgeParity_Routes
// holds the two together); every other route is 404, as at the edge.

//go:embed land_door_routes.txt
var landDoorRoutesFile string

// landDoorRoutes is the allowlist. A file that does not parse leaves it nil,
// which forwards nothing; TestRoutes_Parse fails the build first.
var landDoorRoutes, landDoorRoutesErr = parseRoutes(landDoorRoutesFile)

// routeSet matches a request against the allowlist by http.ServeMux's own
// pattern rules, the rules kbeads routes with.
type routeSet struct {
	mux      *http.ServeMux
	patterns map[string]bool
}

// parseRoutes reads the allowlist: one "METHOD /v1/pattern" per line, "#"
// starts a comment.
func parseRoutes(text string) (rs *routeSet, err error) {
	rs = &routeSet{mux: http.NewServeMux(), patterns: map[string]bool{}}
	for i, line := range strings.Split(text, "\n") {
		line, _, _ = strings.Cut(line, "#")
		fields := strings.Fields(line)
		if len(fields) == 0 {
			continue
		}
		if len(fields) != 2 || !strings.HasPrefix(fields[1], "/v1/") || strings.ToUpper(fields[0]) != fields[0] {
			return nil, fmt.Errorf("land_door_routes.txt:%d: want METHOD /v1/pattern, got %q", i+1, line)
		}
		pattern := fields[0] + " " + fields[1]
		if rs.patterns[pattern] {
			return nil, fmt.Errorf("land_door_routes.txt:%d: %s listed twice", i+1, pattern)
		}
		if err := register(rs.mux, pattern); err != nil {
			return nil, fmt.Errorf("land_door_routes.txt:%d: %w", i+1, err)
		}
		rs.patterns[pattern] = true
	}
	return rs, nil
}

// register adds pattern to mux, turning ServeMux's panic on a malformed or
// conflicting pattern into an error.
func register(mux *http.ServeMux, pattern string) (err error) {
	defer func() {
		if r := recover(); r != nil {
			err = fmt.Errorf("%s: %v", pattern, r)
		}
	}()
	mux.HandleFunc(pattern, func(http.ResponseWriter, *http.Request) {})
	return nil
}

// allows reports whether r's method and path are on a listed route. The path
// must already be canonical: for one it would clean, ServeMux reports the
// pattern its redirect would reach, and landgate forwards the path it checked
// or nothing.
func (rs *routeSet) allows(r *http.Request) bool {
	if rs == nil || path.Clean(r.URL.Path) != r.URL.Path {
		return false
	}
	_, pattern := rs.mux.Handler(r)
	return rs.patterns[pattern]
}
