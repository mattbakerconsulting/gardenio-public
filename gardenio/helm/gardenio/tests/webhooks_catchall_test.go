package tests

import (
	"strings"
	"testing"
)

// Webhooks-edge default-host catch-all → immediate close (task ka-0000007813,
// epic ka-0000000572, operator decision ka-0000001217 = alert_plus_catchall).
//
// A lowest-priority HostRegexp(`^.+$`) IngressRoute on the public `webhooks`
// entrypoint whose only middleware is an ipAllowList with an UNMATCHABLE
// sourceRange, so every request that matched no real webhook router is refused
// with closeStatus (default 444) before any backend. Off by default: the chart
// is byte-identical until an overlay opts in (AVA-transport dark-ship, MR !7100).
//
// The template lives in templates/webhooks-catchall/ingressroute.yaml and is
// COUPLED to the containment-breach alert (edgeContainment): when the catch-all
// is enabled the breach rule subtracts the sanctioned catch-all router so its
// own close does not page. See TestEdgeContainment_CatchAll* in
// edge_containment_alert_test.go.

// helmTemplateAllowFailNS renders the whole chart (no --show-only), returning
// output+error. Whole-chart render is used because --show-only on a template
// that renders empty (catch-all disabled) is a helm error, not an empty string.
func renderCatchall(t *testing.T, setFlags ...string) string {
	t.Helper()
	// Reuse the shared helmTemplate helper with no showOnly so the whole chart
	// renders; the catch-all disabled case then simply contains no catch-all.
	return helmTemplate(t, "", setFlags...)
}

func TestWebhooksCatchall_DefaultDisabled_RendersNothing(t *testing.T) {
	t.Parallel()
	out := renderCatchall(t)
	if strings.Contains(out, "webhooks-catchall") {
		t.Errorf("catch-all must NOT render when webhooksEdgeCatchall.enabled=false (default), got a match in:\n%s",
			grepLines(out, "webhooks-catchall"))
	}
}

func TestWebhooksCatchall_EnabledRendersRouteAndDeny(t *testing.T) {
	t.Parallel()
	out := renderCatchall(t, "webhooksEdgeCatchall.enabled=true")

	// The IngressRoute + the deny Middleware must both render.
	if !strings.Contains(out, "kind: IngressRoute") || !strings.Contains(out, "test-webhooks-catchall") {
		t.Fatalf("expected the catch-all IngressRoute to render, got:\n%s", grepLines(out, "webhooks-catchall"))
	}
	if !strings.Contains(out, "test-webhooks-catchall-close") {
		t.Fatalf("expected the catch-all deny Middleware to render, got:\n%s", grepLines(out, "webhooks-catchall"))
	}

	// It must be a lowest-priority catch-all so every REAL webhook Host router
	// (priority >=90) wins: HostRegexp(`^.+$`) at priority 1.
	if !strings.Contains(out, "HostRegexp(`^.+$`)") {
		t.Errorf("catch-all must match every host via HostRegexp(`^.+$`), got:\n%s", grepLines(out, "HostRegexp"))
	}
	if !strings.Contains(out, "priority: 1") {
		t.Errorf("catch-all must be priority 1 so real webhook routers outrank it, got:\n%s", grepLines(out, "priority"))
	}

	// The deny middleware keys on an UNMATCHABLE source range (never a source
	// IP the operator could get wrong) and refuses with the close status.
	if !strings.Contains(out, "ipAllowList:") {
		t.Errorf("catch-all deny must be an ipAllowList (allowlist-first, never a source denylist), got:\n%s",
			grepLines(out, "webhooks-catchall-close"))
	}
	if !strings.Contains(out, "192.0.2.0/24") {
		t.Errorf("catch-all must use the unmatchable RFC 5737 TEST-NET-1 range by default, got:\n%s",
			grepLines(out, "sourceRange"))
	}
	if !strings.Contains(out, "rejectStatusCode: 444") {
		t.Errorf("catch-all must close with status 444 by default, got:\n%s", grepLines(out, "rejectStatusCode"))
	}
}

func TestWebhooksCatchall_ScopedToWebhooksEntrypointOnly(t *testing.T) {
	t.Parallel()
	// Scope guard: the catch-all must bind the public webhooks entrypoint ONLY.
	// If it ever listed web/websecure it would shadow the AVA-gated apex, which
	// is explicitly out of scope for the scanner-response plan.
	out := renderCatchall(t, "webhooksEdgeCatchall.enabled=true")

	idx := strings.Index(out, "test-webhooks-catchall\n")
	if idx < 0 {
		// Fall back to locating the IngressRoute block by its component label.
		idx = strings.Index(out, "app.kubernetes.io/component: webhooks-catchall")
	}
	if idx < 0 {
		t.Fatalf("catch-all IngressRoute not found, got:\n%s", grepLines(out, "webhooks-catchall"))
	}
	// Inspect the entryPoints block of the IngressRoute.
	block := out[idx:]
	epStart := strings.Index(block, "entryPoints:")
	if epStart < 0 {
		t.Fatalf("catch-all IngressRoute has no entryPoints block, got:\n%s", block[:min(len(block), 800)])
	}
	epBlock := block[epStart : epStart+120]
	if !strings.Contains(epBlock, "- webhooks") {
		t.Errorf("catch-all must bind the webhooks entrypoint, got:\n%s", epBlock)
	}
	if strings.Contains(epBlock, "- web\n") || strings.Contains(epBlock, "- websecure") {
		t.Errorf("catch-all must NOT bind web/websecure (would shadow the AVA apex — out of scope), got:\n%s", epBlock)
	}
}

func TestWebhooksCatchall_CloseStatusAndRangeConfigurable(t *testing.T) {
	t.Parallel()
	out := renderCatchall(t,
		"webhooksEdgeCatchall.enabled=true",
		"webhooksEdgeCatchall.closeStatus=403",
		"webhooksEdgeCatchall.sourceRange={198.51.100.0/24}",
	)
	if !strings.Contains(out, "rejectStatusCode: 403") {
		t.Errorf("closeStatus override must flow into rejectStatusCode, got:\n%s", grepLines(out, "rejectStatusCode"))
	}
	if !strings.Contains(out, "198.51.100.0/24") {
		t.Errorf("sourceRange override must flow into the ipAllowList, got:\n%s", grepLines(out, "sourceRange"))
	}
}

// grepLines returns the lines of s containing sub, for readable failure output.
func grepLines(s, sub string) string {
	var b strings.Builder
	for ln := range strings.SplitSeq(s, "\n") {
		if strings.Contains(ln, sub) {
			b.WriteString(ln)
			b.WriteString("\n")
		}
	}
	if b.Len() == 0 {
		return "(no lines matched " + sub + ")"
	}
	return b.String()
}
