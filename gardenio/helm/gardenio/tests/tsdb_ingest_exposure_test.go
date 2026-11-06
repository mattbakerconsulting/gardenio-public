package tests

import (
	"strings"
	"testing"
)

// Internet-facing TSDB/trace ingest exposure guard — bug ka-0000001172.
//
// vmsingle (VictoriaMetrics) and vtsingle (VictoriaTraces) have no auth of
// their own. Their `ingest` blocks publish a write/insert surface through
// Traefik, and binding an entrypoint listed in traefik.internetFacingEntryPoints
// (default: `webhooks`, the internet-facing NLB on prod-gardenio-eks) puts that
// surface on the open internet. DNS is not a control there — the IngressRoute
// matches on Host + SNI, so any client sending the Host/SNI to the public edge
// IP reaches the route with or without an A record.
//
// templates/validate-tsdb-ingest-exposure.yaml turns values.yaml's prose
// SECURITY note into a render-time invariant:
//
//	1. ipWhitelist.enabled with an empty sourceRange           -> hard fail
//	2. public entrypoint + no auth AND no allow-list           -> hard fail
//	3. public entrypoint + allow-list off                      -> fail unless
//	   ipWhitelist.allowPublicWithoutSourceRange records the decision
//
// The whole feature is DEFAULT OFF (both ingest blocks default enabled=false),
// so a normal deploy renders byte-identically.

const (
	// Minimum viable ingest flip: enabled + host + a basicAuth secret.
	vmIngestOn   = "vmsingle.enabled=true"
	vmIngestOn2  = "vmsingle.ingest.enabled=true"
	vmIngestHost = "vmsingle.ingest.host=vm-ingest.gardenio-e2e.prod.gardenio.acme.example"
	vmIngestAuth = "vmsingle.ingest.auth.secretName=gardenio-vmsingle-ingest-auth"
	// The internet-facing entrypoint (traefik-webhooks NLB).
	vmPublicEP = "vmsingle.ingest.entryPoints={webhooks}"
)

func vmIngestBase(extra ...string) []string {
	return append([]string{vmIngestOn, vmIngestOn2, vmIngestHost, vmIngestAuth}, extra...)
}

// ---------------------------------------------------------------------------
// Default-off: the guard must never fire on an untouched chart.
// ---------------------------------------------------------------------------

func TestTSDBIngestExposure_DefaultsRender(t *testing.T) {
	t.Parallel()
	if out, err := helmTemplateAllowFail(t, ""); err != nil {
		t.Fatalf("chart defaults must render — both ingest blocks are enabled=false; err: %v\n%s", err, out)
	}
}

// The default entrypoints (empty => web/websecure) ride the INTERNAL,
// AVA-fronted NLB on this chart's edges, so leaving the allow-list off there is
// not the exposure this guard is about. It must render.
func TestTSDBIngestExposure_InternalEntrypoints_NoWhitelist_Renders(t *testing.T) {
	t.Parallel()
	if out, err := helmTemplateAllowFail(t, "", vmIngestBase()...); err != nil {
		t.Fatalf("vmsingle.ingest on the default web/websecure entrypoints must render with ipWhitelist off; err: %v\n%s", err, out)
	}
}

// ---------------------------------------------------------------------------
// The case this guard comes from: MR !7664 bound `webhooks` with the allow-list
// left at its permissive default and nothing objected.
// ---------------------------------------------------------------------------

func TestTSDBIngestExposure_PublicEntrypoint_NoWhitelist_Fails(t *testing.T) {
	t.Parallel()
	out, err := helmTemplateAllowFail(t, "", vmIngestBase(vmPublicEP)...)
	if err == nil {
		t.Fatal("vmsingle.ingest bound to the internet-facing `webhooks` entrypoint with ipWhitelist.enabled=false MUST fail the render — basicAuth alone would be the only thing in front of an unauthenticated TSDB write API (ka-0000001172)")
	}
	// The message must name the knob an operator has to touch, and the bead.
	for _, want := range []string{
		"vmsingle.ingest.ipWhitelist.enabled=true",
		"allowPublicWithoutSourceRange",
		"ka-0000001172",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("guard message must mention %q so the operator knows the two ways out; got:\n%s", want, out)
		}
	}
}

// Resolution (a): the originally-approved shape — allow-list on, real CIDR.
func TestTSDBIngestExposure_PublicEntrypoint_WithSourceRange_Renders(t *testing.T) {
	t.Parallel()
	out, err := helmTemplateAllowFail(t, "", vmIngestBase(
		vmPublicEP,
		"vmsingle.ingest.ipWhitelist.enabled=true",
		"vmsingle.ingest.ipWhitelist.sourceRange={198.51.100.7/32}",
	)...)
	if err != nil {
		t.Fatalf("the approved shape (allow-list on with the sender's egress CIDR) must render; err: %v\n%s", err, out)
	}
	if !strings.Contains(out, "198.51.100.7/32") {
		t.Error("expected the ipWhiteList middleware to carry the configured sourceRange")
	}
}

// Resolution (b): a credential-only transport stays POSSIBLE, but only as an
// explicit acknowledgment carrying the decision reference.
func TestTSDBIngestExposure_PublicEntrypoint_Acknowledged_Renders(t *testing.T) {
	t.Parallel()
	out, err := helmTemplateAllowFail(t, "", vmIngestBase(
		vmPublicEP,
		"vmsingle.ingest.ipWhitelist.allowPublicWithoutSourceRange=ka-0000004494",
	)...)
	if err != nil {
		t.Fatalf("an explicitly acknowledged credential-only transport must still render — the guard forces a recorded decision, it does not forbid the posture; err: %v\n%s", err, out)
	}
}

// ---------------------------------------------------------------------------
// Hard fails — no acknowledgment can buy these off.
// ---------------------------------------------------------------------------

// Zero controls on a public edge: an unauthenticated TSDB write API open to the
// internet. The acknowledgment knob must NOT unlock this.
func TestTSDBIngestExposure_PublicEntrypoint_NoAuthNoWhitelist_FailsEvenAcknowledged(t *testing.T) {
	t.Parallel()
	out, err := helmTemplateAllowFail(t, "", vmIngestBase(
		vmPublicEP,
		"vmsingle.ingest.auth.enabled=false",
		"vmsingle.ingest.ipWhitelist.allowPublicWithoutSourceRange=ka-0000004494",
	)...)
	if err == nil {
		t.Fatal("auth OFF and ipWhitelist OFF on an internet-facing entrypoint must fail regardless of the acknowledgment knob — that is an unauthenticated TSDB write API on the open internet")
	}
	if !strings.Contains(out, "no control in front of it") {
		t.Errorf("expected the zero-controls message, got:\n%s", out)
	}
}

// An allow-list with no CIDRs is not a control. Caught on any entrypoint.
func TestTSDBIngestExposure_WhitelistEnabledEmptySourceRange_Fails(t *testing.T) {
	t.Parallel()
	out, err := helmTemplateAllowFail(t, "", vmIngestBase(
		"vmsingle.ingest.ipWhitelist.enabled=true",
	)...)
	if err == nil {
		t.Fatal("ipWhitelist.enabled=true with an empty sourceRange must fail — a half-configured allow-list reads as a control but is not one")
	}
	if !strings.Contains(out, "sourceRange is empty") {
		t.Errorf("expected the empty-sourceRange message, got:\n%s", out)
	}
}

// ---------------------------------------------------------------------------
// vtsingle carries the identical shape and the identical SECURITY note, so the
// guard covers both stores rather than leaving the adjacent hole open.
// ---------------------------------------------------------------------------

func TestTSDBIngestExposure_VTSinglePublicEntrypoint_NoWhitelist_Fails(t *testing.T) {
	t.Parallel()
	out, err := helmTemplateAllowFail(t, "",
		"vtsingle.enabled=true",
		"vtsingle.ingest.enabled=true",
		"vtsingle.ingest.host=vt-ingest.gardenio-e2e.prod.gardenio.acme.example",
		"vtsingle.ingest.auth.secretName=gardenio-vtsingle-ingest-auth",
		"vtsingle.ingest.entryPoints={webhooks}",
	)
	if err == nil {
		t.Fatal("vtsingle.ingest on the internet-facing `webhooks` entrypoint with ipWhitelist off must fail the same way vmsingle does — VictoriaTraces has no auth of its own either")
	}
	if !strings.Contains(out, "vtsingle.ingest.ipWhitelist.enabled=true") {
		t.Errorf("expected the vtsingle-scoped guard message, got:\n%s", out)
	}
}

// ---------------------------------------------------------------------------
// The public-entrypoint set is data, not a hardcoded string.
// ---------------------------------------------------------------------------

// An edge whose shared web/websecure ARE public declares that, and the guard
// then covers routes bound to them.
func TestTSDBIngestExposure_InternetFacingEntryPointsIsConfigurable(t *testing.T) {
	t.Parallel()
	out, err := helmTemplateAllowFail(t, "", vmIngestBase(
		"traefik.internetFacingEntryPoints={web,websecure}",
	)...)
	if err == nil {
		t.Fatal("declaring web/websecure as internet-facing must bring routes bound to them (the empty-entryPoints default) under the guard")
	}
	if !strings.Contains(out, "web,websecure") {
		t.Errorf("expected the failure to name the bound entrypoints, got:\n%s", out)
	}
}

// Conversely, an edge with no internet-facing entrypoints at all opts every
// route out — the list is the single source of truth.
func TestTSDBIngestExposure_NoInternetFacingEntryPoints_Renders(t *testing.T) {
	t.Parallel()
	if out, err := helmTemplateAllowFail(t, "", vmIngestBase(
		vmPublicEP,
		"traefik.internetFacingEntryPoints=null",
	)...); err != nil {
		t.Fatalf("with traefik.internetFacingEntryPoints cleared the guard must not fire; err: %v\n%s", err, out)
	}
}
