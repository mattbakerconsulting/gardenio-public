package oidclogin

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"strings"
)

// The machine client an agent pod may act as (escalation routing Task 15,
// C5): the controller sets both on a pod whose role bead names an allowlisted
// machine_client, with the secret mounted as a 0400 file. kd --as-machine and
// gb --as-machine read them.
const (
	MachineClientIDEnv         = "GARDENIO_MACHINE_CLIENT_ID"
	MachineClientSecretFileEnv = "GARDENIO_MACHINE_CLIENT_SECRET_FILE"

	// IssuerEnv overrides DefaultIssuer, as for kd login and gb login.
	IssuerEnv = "GARDENIO_OIDC_ISSUER"

	// DiscoveryURLEnv, when set, is where MachineToken fetches the issuer's
	// discovery document instead of at the issuer itself: the in-cluster
	// Keycloak Service, since agent pods cannot dial the public edge that
	// serves the issuer (landgate crewDeny). The document must still name
	// the issuer, as BEADS_OIDC_DISCOVERY_URL does for the daemon.
	DiscoveryURLEnv = "GARDENIO_OIDC_DISCOVERY_URL"
	// CAFileEnv names a PEM bundle trusted, besides the system roots, for
	// the discovery and token requests: the keycloak-runtime CA when
	// DiscoveryURLEnv is the Service's https port.
	CAFileEnv = "GARDENIO_OIDC_CA_FILE"
)

// ClientCredentialsToken mints an access token for a confidential client with
// the client credentials grant (RFC 6749 §4.4) at the issuer's discovered
// token endpoint. The secret travels only in that POST body; the token is
// returned to the caller and neither is written anywhere else.
func ClientCredentialsToken(ctx context.Context, hc *http.Client, issuer, clientID, clientSecret string) (string, error) {
	return clientCredentialsTokenAt(ctx, hc, issuer, issuer, clientID, clientSecret)
}

// clientCredentialsTokenAt is ClientCredentialsToken with the discovery
// document fetched at discoveryBase, which must still describe issuer.
func clientCredentialsTokenAt(ctx context.Context, hc *http.Client, issuer, discoveryBase, clientID, clientSecret string) (string, error) {
	ep, err := DiscoverAt(ctx, hc, discoveryBase, issuer)
	if err != nil {
		return "", err
	}
	var tr tokenResponse
	if err := postForm(ctx, hc, ep.TokenEndpoint, url.Values{
		"grant_type":    {"client_credentials"},
		"client_id":     {clientID},
		"client_secret": {clientSecret},
	}, &tr); err != nil {
		return "", fmt.Errorf("client credentials grant for %q: %w", clientID, err)
	}
	if tr.AccessToken == "" {
		return "", errors.New("token response has no access_token")
	}
	return tr.AccessToken, nil
}

// MachineToken mints this invocation's machine-client token from
// MachineClientIDEnv and the secret file MachineClientSecretFileEnv names,
// against IssuerEnv (default DefaultIssuer), discovered at DiscoveryURLEnv
// when set. It is never cached: each kd or gb run mints its own and drops it
// on exit.
func MachineToken(ctx context.Context, hc *http.Client) (string, error) {
	clientID := strings.TrimSpace(os.Getenv(MachineClientIDEnv))
	path := os.Getenv(MachineClientSecretFileEnv)
	if clientID == "" || path == "" {
		return "", fmt.Errorf("--as-machine needs %s and %s (set only on a pod whose role bead has an allowlisted machine_client)", MachineClientIDEnv, MachineClientSecretFileEnv)
	}
	b, err := os.ReadFile(path)
	if err != nil {
		return "", fmt.Errorf("reading the machine client secret: %w", err)
	}
	secret := strings.TrimSpace(string(b))
	if secret == "" {
		return "", fmt.Errorf("machine client secret file %s is empty", path)
	}
	issuer := os.Getenv(IssuerEnv)
	if issuer == "" {
		issuer = DefaultIssuer
	}
	discovery := strings.TrimSpace(os.Getenv(DiscoveryURLEnv))
	if discovery == "" {
		discovery = issuer
	}
	if caFile := os.Getenv(CAFileEnv); caFile != "" {
		if hc, err = withExtraCA(hc, caFile); err != nil {
			return "", err
		}
	}
	return clientCredentialsTokenAt(ctx, hc, issuer, discovery, clientID, secret)
}

// withExtraCA returns a copy of hc whose transport also trusts the PEM
// certificates in caFile.
func withExtraCA(hc *http.Client, caFile string) (*http.Client, error) {
	pem, err := os.ReadFile(caFile)
	if err != nil {
		return nil, fmt.Errorf("reading %s: %w", CAFileEnv, err)
	}
	pool, err := x509.SystemCertPool()
	if err != nil {
		pool = x509.NewCertPool()
	}
	if !pool.AppendCertsFromPEM(pem) {
		return nil, fmt.Errorf("%s %s holds no PEM certificate", CAFileEnv, caFile)
	}
	rt := hc.Transport
	if rt == nil {
		rt = http.DefaultTransport
	}
	base, ok := rt.(*http.Transport)
	if !ok {
		return nil, fmt.Errorf("%s: the HTTP client's transport is not an *http.Transport", CAFileEnv)
	}
	tr := base.Clone()
	if tr.TLSClientConfig == nil {
		tr.TLSClientConfig = &tls.Config{MinVersion: tls.VersionTLS12}
	}
	tr.TLSClientConfig.RootCAs = pool
	c := *hc
	c.Transport = tr
	return &c, nil
}
