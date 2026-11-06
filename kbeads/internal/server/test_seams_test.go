package server

// test_seams_test.go holds the entry points only tests call: each is a
// production function with the arguments the daemon's own wiring passes
// pinned to their zero value, so tests can drive one layer without building
// the whole server.

import (
	"context"
	"net/http"

	"google.golang.org/grpc"

	"gardenio/kbeads/internal/model"
)

// AuthInterceptor is windowAuthInterceptor with no window resolver.
func AuthInterceptor(fleetCfg *FleetConfig, verifier TokenVerifier) grpc.UnaryServerInterceptor {
	return windowAuthInterceptor(nil, fleetCfg, verifier)
}

// authMiddleware is windowAuthMiddleware with no window resolver: a gw_ bearer
// is 401.
func authMiddleware(edgeClients map[string]struct{}, fleetCfg *FleetConfig, verifier TokenVerifier, next http.Handler) http.Handler {
	return windowAuthMiddleware(nil, edgeClients, fleetCfg, verifier, next)
}

// authorizeWrite is authorizeWriteTitled with no title, so a machine
// principal gets no config grant.
func (s *BeadsServer) authorizeWrite(ctx context.Context, action string, beadType model.BeadType, labels []string) *AuthzDenial {
	return s.authorizeWriteTitled(ctx, action, beadType, labels, "")
}

// operatedByFromContext is resolveOperatedBy without the source label.
func (s *BeadsServer) operatedByFromContext(ctx context.Context) string {
	value, _ := s.resolveOperatedBy(ctx)
	return value
}
