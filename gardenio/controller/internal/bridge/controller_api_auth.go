package bridge

import (
	"net/http"
	"os"
	"strings"
)

// controllerAPITokenEnv is the shared bearer secret the bridge attaches when
// calling the controller's state-mutating :8091 endpoints (pool/assign,
// freeze/interrupt, microvm/resume). It mirrors the controller-side guard in
// cmd/controller/api_auth.go: when the env var is empty (default) no header is
// added and the controller treats the request as before, so enabling auth is a
// single shared secret on both sides with zero blast radius until set.
const controllerAPITokenEnv = "CONTROLLER_API_TOKEN"

// setControllerAPIAuth attaches the Authorization bearer header to a controller
// API request when CONTROLLER_API_TOKEN is configured. It is a no-op when the
// var is unset, so callers can invoke it unconditionally.
func setControllerAPIAuth(req *http.Request) {
	if token := strings.TrimSpace(os.Getenv(controllerAPITokenEnv)); token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
}
