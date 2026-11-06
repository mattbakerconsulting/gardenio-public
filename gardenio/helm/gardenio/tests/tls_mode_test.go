package tests

import (
	"strings"
	"testing"
)

// P3.6 cert-manager + sslip.io + per-host HTTP-01 (ka-0000002443; DNS/TLS doc
// §3 ka-0000004570). tls.mode switches the single shared TLS Certificate
// between the legacy wildcard (DNS-01 + external-dns) and the in-box per-host
// HTTP-01 path (enumerated SANs, no DNSEndpoint, sslip.io-friendly).

//  1. mode=wildcard (default) preserves today: wildcard+apex dnsNames and, with
//     a dns.target, an external-dns DNSEndpoint.
func TestTLSMode_WildcardDefault(t *testing.T) {
	t.Parallel()
	out := helmTemplate(t, "templates/tls.yaml",
		"tls.enabled=true",
		"tls.domainName=*.gardenio.app.test",
		"tls.dns.target=nlb.example.com",
	)
	if !strings.Contains(out, `- "*.gardenio.app.test"`) {
		t.Error("wildcard mode must keep the wildcard dnsName")
	}
	if !strings.Contains(out, `- "gardenio.app.test"`) {
		t.Error("wildcard mode must add the apex dnsName")
	}
	if !strings.Contains(out, "kind: DNSEndpoint") {
		t.Error("wildcard mode with a dns.target must render the external-dns DNSEndpoint")
	}
}

//  2. mode=perHost renders enumerated SANs, an HTTP-01 issuer, and NO
//     DNSEndpoint (the box has no Route53).
func TestTLSMode_PerHost(t *testing.T) {
	t.Parallel()
	out := helmTemplate(t, "templates/tls.yaml",
		"tls.enabled=true",
		"tls.mode=perHost",
		"tls.issuer=letsencrypt-http01",
		"tls.domainName=10-0-0-1.sslip.io",
		"tls.hosts={auth.10-0-0-1.sslip.io,beads-viewer.10-0-0-1.sslip.io}",
	)
	if !strings.Contains(out, `- "auth.10-0-0-1.sslip.io"`) ||
		!strings.Contains(out, `- "beads-viewer.10-0-0-1.sslip.io"`) {
		t.Errorf("perHost must enumerate every host as a dnsName, got:\n%s", out)
	}
	if strings.Contains(out, `- "*.`) {
		t.Error("perHost must NOT emit a wildcard dnsName (LE HTTP-01 cannot issue wildcards)")
	}
	if !strings.Contains(out, "name: letsencrypt-http01") {
		t.Error("perHost must use the HTTP-01 ClusterIssuer")
	}
	if strings.Contains(out, "kind: DNSEndpoint") {
		t.Error("perHost must NOT render an external-dns DNSEndpoint")
	}
}

//  3. mode=perHost with an empty hosts list fails fast (a wildcard-free cert
//     with no SANs is meaningless and would silently drop TLS coverage).
func TestTLSMode_PerHostRequiresHosts(t *testing.T) {
	t.Parallel()
	out, err := helmTemplateAllowFail(t, "templates/tls.yaml",
		"tls.enabled=true",
		"tls.mode=perHost",
	)
	if err == nil {
		t.Fatalf("perHost with no tls.hosts must fail fast, got render:\n%s", out)
	}
	if !strings.Contains(out, "tls.hosts") {
		t.Errorf("fail-fast message should name tls.hosts, got: %v\n%s", err, out)
	}
}

//  4. existingSecretName still wins everywhere (air-gapped / static-cert
//     fallback): no Certificate, no DNSEndpoint, regardless of mode.
func TestTLSMode_ExistingSecretSkipsAll(t *testing.T) {
	t.Parallel()
	out, err := helmTemplateAllowFail(t, "templates/tls.yaml",
		"tls.enabled=true",
		"tls.mode=perHost",
		"tls.existingSecretName=gardenio-wildcard-tls",
	)
	// The template body is fully gated off, so --show-only yields no manifest
	// (helm errors with "could not find template"); either way there must be
	// no Certificate / DNSEndpoint.
	if err == nil && (strings.Contains(out, "kind: Certificate") || strings.Contains(out, "kind: DNSEndpoint")) {
		t.Errorf("existingSecretName must skip Certificate + DNSEndpoint, got:\n%s", out)
	}
}

// 4b. mode=localCA (Tarboat G1.4, ka-0000003591) — true air-gap TLS. The leaf
//
//	Certificate enumerates tls.hosts (like perHost) but its issuerRef points
//	at the in-cluster CA ClusterIssuer (tls.issuer), NOT an ACME issuer, and
//	there is no DNSEndpoint.
func TestTLSMode_LocalCALeaf(t *testing.T) {
	t.Parallel()
	out := helmTemplate(t, "templates/tls.yaml",
		"tls.enabled=true",
		"tls.mode=localCA",
		"tls.issuer=gardenio-local-ca-issuer",
		"tls.hosts={auth.10-0-0-1.box.local,beads-viewer.10-0-0-1.box.local}",
	)
	if !strings.Contains(out, `- "auth.10-0-0-1.box.local"`) ||
		!strings.Contains(out, `- "beads-viewer.10-0-0-1.box.local"`) {
		t.Errorf("localCA must enumerate every host as a dnsName, got:\n%s", out)
	}
	if !strings.Contains(out, "name: gardenio-local-ca-issuer") {
		t.Error("localCA leaf must reference the CA ClusterIssuer named by tls.issuer")
	}
	if strings.Contains(out, "kind: DNSEndpoint") {
		t.Error("localCA must NOT render an external-dns DNSEndpoint")
	}
}

// 4c. mode=localCA with an empty hosts list fails fast (same contract as
//
//	perHost — a SAN-less single served cert silently drops TLS coverage).
func TestTLSMode_LocalCARequiresHosts(t *testing.T) {
	t.Parallel()
	out, err := helmTemplateAllowFail(t, "templates/tls.yaml",
		"tls.enabled=true",
		"tls.mode=localCA",
	)
	if err == nil {
		t.Fatalf("localCA with no tls.hosts must fail fast, got render:\n%s", out)
	}
	if !strings.Contains(out, "tls.hosts") {
		t.Errorf("fail-fast message should name tls.hosts, got: %v\n%s", err, out)
	}
}

// 4d. mode=localCA renders the full in-cluster CA chain — a SelfSigned
//
//	ClusterIssuer, a CA Certificate (isCA), and a CA ClusterIssuer named
//	tls.issuer that signs from the CA secret. NO ACME (no internet).
func TestTLSClusterIssuer_LocalCAChain(t *testing.T) {
	t.Parallel()
	out := helmTemplate(t, "templates/tls-clusterissuer.yaml",
		"tls.enabled=true",
		"tls.mode=localCA",
		"tls.issuer=gardenio-local-ca-issuer",
		"tls.clusterIssuer.create=false",
		"tls.hosts={auth.10-0-0-1.box.local}",
	)
	if !strings.Contains(out, "selfSigned: {}") {
		t.Error("localCA chain must include a SelfSigned ClusterIssuer to bootstrap the CA")
	}
	if !strings.Contains(out, "isCA: true") {
		t.Error("localCA chain must include a CA Certificate (isCA: true)")
	}
	if !strings.Contains(out, `name: "gardenio-local-ca-issuer"`) {
		t.Error("CA ClusterIssuer must be named tls.issuer so the leaf issuerRef resolves")
	}
	if !strings.Contains(out, "ca:") {
		t.Error("localCA chain must include a CA-type ClusterIssuer (signs from the CA secret)")
	}
	if strings.Contains(out, "acme:") || strings.Contains(out, "http01:") {
		t.Error("localCA must NOT render any ACME/HTTP-01 issuer (true air-gap, no internet)")
	}
}

// --- ClusterIssuer (ka-0000005963) ------------------------------------------
// The chart's Certificate references tls.issuer BY NAME. In a cloud path that
// issuer is created outside the chart; in-box
// the chart must render its own HTTP-01 ClusterIssuer. Gated on
// tls.clusterIssuer.create so the cloud path is untouched by default.

//  5. clusterIssuer.create defaults OFF — the cloud path keeps its
//     terraform-created issuer and the chart renders no ClusterIssuer.
func TestTLSClusterIssuer_DefaultOff(t *testing.T) {
	t.Parallel()
	out, err := helmTemplateAllowFail(t, "templates/tls-clusterissuer.yaml",
		"tls.enabled=true",
		"tls.mode=perHost",
		"tls.issuer=letsencrypt-http01",
		"tls.hosts={auth.10-0-0-1.sslip.io}",
	)
	// Fully gated off => no manifest (helm errors "could not find template")
	// or, if rendered, must contain no ClusterIssuer.
	if err == nil && strings.Contains(out, "kind: ClusterIssuer") {
		t.Errorf("clusterIssuer.create defaults false — no ClusterIssuer should render, got:\n%s", out)
	}
}

//  6. clusterIssuer.create=true renders an HTTP-01 ACME ClusterIssuer NAMED
//     tls.issuer (so the Certificate's issuerRef resolves), with no DNS-01
//     solver (no DNS credential in-box).
func TestTLSClusterIssuer_HTTP01Rendered(t *testing.T) {
	t.Parallel()
	out := helmTemplate(t, "templates/tls-clusterissuer.yaml",
		"tls.enabled=true",
		"tls.mode=perHost",
		"tls.issuer=letsencrypt-http01",
		"tls.clusterIssuer.create=true",
		"tls.clusterIssuer.email=ops@example.com",
		"tls.hosts={auth.10-0-0-1.sslip.io}",
	)
	if !strings.Contains(out, "kind: ClusterIssuer") {
		t.Fatalf("clusterIssuer.create=true must render a ClusterIssuer, got:\n%s", out)
	}
	if !strings.Contains(out, `name: "letsencrypt-http01"`) {
		t.Error("ClusterIssuer name must equal tls.issuer so the Certificate issuerRef resolves")
	}
	if !strings.Contains(out, "http01:") {
		t.Error("in-box ClusterIssuer must use the HTTP-01 solver")
	}
	if strings.Contains(out, "dns01:") || strings.Contains(out, "route53:") {
		t.Error("in-box ClusterIssuer must NOT use a DNS-01/Route53 solver (no DNS credential in-box)")
	}
	if !strings.Contains(out, "ops@example.com") {
		t.Error("ACME email should propagate when set")
	}
}

//  7. existingSecretName (static-cert fallback) skips the ClusterIssuer even
//     when create=true — the static path manages the cert externally.
func TestTLSClusterIssuer_ExistingSecretSkips(t *testing.T) {
	t.Parallel()
	out, err := helmTemplateAllowFail(t, "templates/tls-clusterissuer.yaml",
		"tls.enabled=true",
		"tls.clusterIssuer.create=true",
		"tls.existingSecretName=gardenio-wildcard-tls",
	)
	if err == nil && strings.Contains(out, "kind: ClusterIssuer") {
		t.Errorf("existingSecretName must skip the ClusterIssuer, got:\n%s", out)
	}
}
