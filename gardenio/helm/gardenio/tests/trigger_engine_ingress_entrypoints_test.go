package tests

import (
	"strings"
	"testing"
)

// The trigger-engine webhook route is gardenio's one internet-facing path
// (decision gd-Bz9rkZg5Q0). With TLS on it must listen on websecure only, so a
// webhook body and its HMAC signature are never served over plain HTTP.
func TestTriggerEngineIngress_TLSOn_WebsecureOnly(t *testing.T) {
	t.Parallel()
	out := helmTemplate(t, "templates/trigger-engine/ingressroute.yaml",
		"triggerEngine.enabled=true",
		"triggerEngine.ingress.enabled=true",
		"triggerEngine.ingress.host=hooks.example.test",
		"tls.enabled=true",
	)
	if !strings.Contains(out, "- websecure") {
		t.Fatalf("route must listen on websecure; got:\n%s", out)
	}
	if strings.Contains(out, "- web\n") {
		t.Errorf("with TLS on the webhook route must NOT listen on web (plaintext); got:\n%s", out)
	}
}

func TestTriggerEngineIngress_TLSOff_KeepsWeb(t *testing.T) {
	t.Parallel()
	out := helmTemplate(t, "templates/trigger-engine/ingressroute.yaml",
		"triggerEngine.enabled=true",
		"triggerEngine.ingress.enabled=true",
		"triggerEngine.ingress.host=hooks.example.test",
		"tls.enabled=false",
	)
	if !strings.Contains(out, "- web\n") {
		t.Errorf("with TLS off the route needs the web entrypoint; got:\n%s", out)
	}
}
