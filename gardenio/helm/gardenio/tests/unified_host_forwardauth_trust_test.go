package tests

import (
	"strings"
	"testing"
)

// Regression (gd-qYD8piysYM): the unified-host forwardAuth Middleware's
// `trustForwardHeader` must honour an operator's explicit `false`.
//
// The template previously read
//   trustForwardHeader: {{ .Values.unifiedHost.forwardAuth.trustForwardHeader | default true }}
// and Sprig's `default` treats a boolean `false` as empty, forcing it back to
// `true`. That silently discarded a `trustForwardHeader: false` override — the
// exact auth-bypass exposure the sibling forwardauth/middleware.yaml documents
// and guards against (ingress audit F9): with Traefik internet-facing and no
// trusted front proxy, attacker-controlled X-Forwarded-* headers keep feeding
// the auth decision. The fix references the value directly.

// unifiedHostForwardAuthMiddlewareBlock returns the `spec:` region of the
// unified-host forwardauth Middleware object, scoped to that object so an
// assertion cannot accidentally match the standalone forwardauth/middleware.yaml
// Middleware (which always renders trustForwardHeader: true).
func unifiedHostForwardAuthMiddlewareBlock(t *testing.T, out string) string {
	t.Helper()
	for doc := range strings.SplitSeq(out, "\n---") {
		if !strings.Contains(doc, "kind: Middleware") {
			continue
		}
		if !strings.Contains(doc, "-unified-host-forwardauth") {
			continue
		}
		return doc
	}
	t.Fatalf("unified-host forwardauth Middleware not found in render:\n%s", out)
	return ""
}

func TestUnifiedHostForwardAuth_TrustForwardHeader_DefaultTrue(t *testing.T) {
	t.Parallel()
	out := helmTemplate(t, "templates/unified-host/ingressroute.yaml",
		append(unifiedHostBase(),
			"unifiedHost.forwardAuth.enabled=true",
			"unifiedHost.forwardAuth.address=http://gardenio-edge-auth.gardenio-system.svc.cluster.local:8096/forwardauth/verify",
		)...,
	)
	block := unifiedHostForwardAuthMiddlewareBlock(t, out)
	if !strings.Contains(block, "trustForwardHeader: true") {
		t.Errorf("default (unset) must render trustForwardHeader: true, got:\n%s", block)
	}
}

func TestUnifiedHostForwardAuth_TrustForwardHeader_FalseHonored(t *testing.T) {
	t.Parallel()
	out := helmTemplate(t, "templates/unified-host/ingressroute.yaml",
		append(unifiedHostBase(),
			"unifiedHost.forwardAuth.enabled=true",
			"unifiedHost.forwardAuth.address=http://gardenio-edge-auth.gardenio-system.svc.cluster.local:8096/forwardauth/verify",
			"unifiedHost.forwardAuth.trustForwardHeader=false",
		)...,
	)
	block := unifiedHostForwardAuthMiddlewareBlock(t, out)
	if !strings.Contains(block, "trustForwardHeader: false") {
		t.Errorf("trustForwardHeader=false must be honoured (fix for `| default true` clobber), got:\n%s", block)
	}
	if strings.Contains(block, "trustForwardHeader: true") {
		t.Errorf("trustForwardHeader=false must NOT render true (default clobbered the override), got:\n%s", block)
	}
}
