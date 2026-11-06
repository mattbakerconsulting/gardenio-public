package main

import (
	"crypto/subtle"
	"encoding/json"
	"log/slog"
	"net/http"
	"os"
	"strings"
)

// controllerAPIToken is the shared bearer secret guarding the state-mutating
// endpoints on the health server (:8091): POST /api/v1/pool/assign,
// POST /api/v1/freeze/interrupt, POST /api/v1/microvm/resume. These are reached
// over the in-cluster ClusterIP agents Service by the slack-bridge, so any pod
// with network reach could previously drive them unauthenticated — assign pool
// agents, abort in-flight checkpoints, or resume MicroVMs. This mirrors the
// existing AUTODESTRUCT_TOKEN guard on the sibling /autodestruct endpoints.
//
// OPT-IN / zero-blast-radius by default: when CONTROLLER_API_TOKEN is empty the
// wrapper is a pass-through (byte-identical to pre-change behavior) and logs a
// one-time startup warning. When set, every wrapped endpoint requires
// `Authorization: Bearer <token>` and 401s otherwise. The bridge callers attach
// the same token from their own CONTROLLER_API_TOKEN env var; helm plumbs both
// from one optional Secret so enabling it is a single chart value.
const controllerAPITokenEnv = "CONTROLLER_API_TOKEN"

// newControllerAPIAuth reads CONTROLLER_API_TOKEN and returns a middleware that
// gates a handler behind it. It emits exactly one startup log line describing
// whether enforcement is active, so an operator can confirm the posture from
// the controller logs without probing the endpoint.
func newControllerAPIAuth(logger *slog.Logger) func(http.HandlerFunc) http.HandlerFunc {
	token := strings.TrimSpace(os.Getenv(controllerAPITokenEnv))
	if token == "" {
		logger.Warn("controller API auth DISABLED — CONTROLLER_API_TOKEN unset; " +
			"/api/v1/pool/assign, /api/v1/freeze/interrupt and /api/v1/microvm/resume " +
			"are reachable unauthenticated by any pod on the agents Service. " +
			"Set CONTROLLER_API_TOKEN (helm: agents.controllerApi.tokenSecret) to enforce.")
		return func(next http.HandlerFunc) http.HandlerFunc { return next }
	}
	logger.Info("controller API auth ENABLED — bearer token required on mutating :8091 endpoints")
	return func(next http.HandlerFunc) http.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) {
			auth := r.Header.Get("Authorization")
			provided := strings.TrimPrefix(auth, "Bearer ")
			// provided == auth means the "Bearer " prefix was absent — reject.
			if provided == auth || subtle.ConstantTimeCompare([]byte(provided), []byte(token)) != 1 {
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(http.StatusUnauthorized)
				_ = json.NewEncoder(w).Encode(map[string]string{"error": "unauthorized"})
				return
			}
			next(w, r)
		}
	}
}
