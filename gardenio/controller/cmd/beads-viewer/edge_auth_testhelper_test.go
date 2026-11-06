package main

import "net/http"

// edgeAuth wraps a viewer handler so a test request's X-Auth-Request-Email
// header is promoted into the verified-identity context slot before it reaches
// the routes — exactly what devBypassMiddleware does on the Gardenio-in-a-Box
// forwardauth edge in production (dev_auth.go::forwardAuthTrustedEmailToContext).
//
// Handlers now read identity ONLY from oidcauth.EmailFromContext (ka-0000002979),
// so the mux built by mkServer/RegisterRoutes — which carries no auth middleware
// — would otherwise see no identity for a header-only test request. Routing the
// shared test builders through the SAME production seam keeps the ~240 existing
// `req.Header.Set("X-Auth-Request-Email", …)` call sites faithful: they simulate
// a request that arrived already authenticated by a trusted forwardauth proxy.
//
// It no-ops when the header is absent (so unauthenticated-request assertions
// still hit the handlers' 401 paths) and when the context already carries a
// verified email (so tests that inject identity via oidcauth.ContextWithEmail —
// simulating the AVA edge — are unaffected).
func edgeAuth(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		next.ServeHTTP(w, forwardAuthTrustedEmailToContext(r))
	})
}

// edgeAuthMux registers srv's routes on a fresh mux and returns it behind the
// edgeAuth seam. Convenience for tests that build a Server inline with custom
// config (base path, grafana URL, now-func, …) and cannot use the mkServer*
// helpers. The returned http.Handler serves requests exactly as production
// does (header identity promoted into the verified-context slot); it is not a
// *http.ServeMux, so route-pattern inspection tests must use mkRawMux instead.
func edgeAuthMux(srv *Server) http.Handler {
	mux := http.NewServeMux()
	srv.RegisterRoutes(mux)
	return edgeAuth(mux)
}
