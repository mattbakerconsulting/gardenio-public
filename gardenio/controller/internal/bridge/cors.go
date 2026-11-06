package bridge

import (
	"net/http"
	"slices"
)

// setCORSOrigin validates the request Origin header against the allowed origins
// list and sets the appropriate CORS response headers.
//
// When allowedOrigins is non-empty, only matching origins receive CORS headers.
// When allowedOrigins is empty, no CORS headers are set (denying cross-origin access).
func setCORSOrigin(w http.ResponseWriter, r *http.Request, allowedOrigins []string) {
	origin := r.Header.Get("Origin")
	if origin == "" || len(allowedOrigins) == 0 {
		return
	}
	if slices.Contains(allowedOrigins, origin) {
		w.Header().Set("Access-Control-Allow-Origin", origin)
		w.Header().Set("Access-Control-Allow-Credentials", "true")
		w.Header().Set("Vary", "Origin")
	}
}
