package bridge

import (
	"net/http"
	"testing"
)

func TestSetControllerAPIAuth_NoTokenNoHeader(t *testing.T) {
	t.Setenv(controllerAPITokenEnv, "")
	req, _ := http.NewRequest(http.MethodPost, "http://controller/api/v1/pool/assign", nil)
	setControllerAPIAuth(req)
	if got := req.Header.Get("Authorization"); got != "" {
		t.Fatalf("token unset: want no Authorization header, got %q", got)
	}
}

func TestSetControllerAPIAuth_AttachesBearer(t *testing.T) {
	t.Setenv(controllerAPITokenEnv, "  tok-123\n")
	req, _ := http.NewRequest(http.MethodPost, "http://controller/api/v1/pool/assign", nil)
	setControllerAPIAuth(req)
	// Whitespace trimmed so it matches the controller-side trimmed compare.
	if got := req.Header.Get("Authorization"); got != "Bearer tok-123" {
		t.Fatalf("want %q, got %q", "Bearer tok-123", got)
	}
}
