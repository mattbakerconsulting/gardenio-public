package oidcauth

import (
	"errors"
	"fmt"
	"net/url"
	"regexp"
	"strings"
)

// avaInstanceARNRegex matches the prefix of an AVA instance ARN. Full ARNs
// look like `arn:aws:ec2:<region>:<acct>:verified-access-instance/vai-…`;
// we only sanity-check the prefix here — the verifier rejects any signer
// that does not exactly match cfg.AVA.TrustSigner at request time, so
// stricter parsing here would buy nothing.
var avaInstanceARNRegex = regexp.MustCompile(`^arn:aws:ec2:[a-z0-9-]+:[0-9]{12}:verified-access-instance/vai-[A-Za-z0-9]+$`)

// awsRegionRegex matches AWS region IDs (e.g. us-east-1, ap-southeast-2).
// Accepts the small number of valid shapes used by every commercial region.
var awsRegionRegex = regexp.MustCompile(`^[a-z]{2}-[a-z]+-[0-9]+$`)

// ValidateOptions tunes Validate's strictness for callers that intentionally
// run without any auth (tests, dev). devAuthBypassEnabled signals that the
// process is running with BEADS_VIEWER_DEV_AUTH_BYPASS or equivalent — in
// that mode we permit (and require) zero-auth config rather than failing.
type ValidateOptions struct {
	DevAuthBypassEnabled bool
}

// Validate checks the auth config for internal consistency at startup so
// misconfigurations surface as a fast crash instead of as runtime 403s on
// routes that should work. Implements ka-0000000212 (epic ka-0000002784).
//
// Rules:
//
//  1. If AVA is enabled (TrustSigner set), the signer MUST parse as an AVA
//     instance ARN and Region (if set) MUST look like an AWS region.
//  2. If Keycloak is enabled (IssuerURL set), the issuer MUST parse as an
//     https:// URL.
//  3. Both auth paths disabled is only allowed under DevAuthBypassEnabled.
//     Outside dev-bypass it is a misconfiguration ("world-readable
//     controller") and refuses to start.
//  4. Under DevAuthBypassEnabled, NO real auth env may be set — silently
//     shadowing real auth with the bypass would be worse than either
//     option chosen cleanly.
func Validate(cfg Config, opts ValidateOptions) error {
	avaEnabled := cfg.AVA.enabled()
	keycloakEnabled := cfg.IssuerURL != ""
	saEnabled := cfg.K8sSA.enabled()

	if opts.DevAuthBypassEnabled {
		if avaEnabled || keycloakEnabled || saEnabled {
			return errors.New("authn config invalid: BEADS_VIEWER_DEV_AUTH_BYPASS set together with real auth env (AVA_TRUST_SIGNER, OIDC_ISSUER_URL, or K8S_SA_ISSUER_URL); refusing to silently shadow real auth")
		}
		return nil
	}

	// The EKS SA-token branch is ADDITIVE — it is a machine identity that
	// clears the same human gates via a synthetic email + role floor, never a
	// standalone auth surface. Require a human path (AVA and/or Keycloak) so
	// enabling only the SA branch cannot silently produce a viewer reachable
	// exclusively by allow-listed pods with no human login at all.
	if !avaEnabled && !keycloakEnabled {
		if saEnabled {
			return errors.New("authn config invalid: K8S_SA_ISSUER_URL set without a human auth path (AVA_TRUST_SIGNER and/or OIDC_ISSUER_URL); the EKS SA-token branch is additive, not a standalone auth surface")
		}
		return errors.New("authn config invalid: neither AVA nor Keycloak configured (set AVA_TRUST_SIGNER and/or OIDC_ISSUER_URL); refusing to start a world-readable controller")
	}

	if avaEnabled {
		if !avaInstanceARNRegex.MatchString(cfg.AVA.TrustSigner) {
			return fmt.Errorf("authn config invalid: AVA_TRUST_SIGNER must be an AVA instance ARN (got %q)", cfg.AVA.TrustSigner)
		}
		if cfg.AVA.Region != "" && !awsRegionRegex.MatchString(cfg.AVA.Region) {
			return fmt.Errorf("authn config invalid: AVA_REGION must look like an AWS region (got %q)", cfg.AVA.Region)
		}
	}

	if keycloakEnabled {
		u, err := url.Parse(cfg.IssuerURL)
		if err != nil {
			return fmt.Errorf("authn config invalid: OIDC_ISSUER_URL is unparseable: %w", err)
		}
		if !strings.EqualFold(u.Scheme, "https") {
			return fmt.Errorf("authn config invalid: OIDC_ISSUER_URL must be an https:// URL (got scheme %q)", u.Scheme)
		}
		if u.Host == "" {
			return fmt.Errorf("authn config invalid: OIDC_ISSUER_URL has empty host (got %q)", cfg.IssuerURL)
		}
	}

	if saEnabled {
		if err := validateK8sSA(cfg.K8sSA, cfg.AllowedEmailDomains); err != nil {
			return err
		}
	}

	return nil
}

// validateK8sSA enforces the EKS SA-token branch's own consistency rules so a
// half-configured deploy fails fast at boot instead of silently no-op'ing or
// exposing a malformed identity path (ka-0000006288):
//
//   - the issuer must be an https:// URL with a host (the JWKS is fetched over
//     it, exactly like the Keycloak issuer);
//   - the pinned audience must be non-empty and must NOT be the apiserver or
//     IRSA/STS audience — those are live on agent pods today and pinning one
//     would accept a repurposed infra token;
//   - the subject allow-list must be non-empty and every entry must be a
//     well-formed system:serviceaccount:<ns>:<sa> subject (an empty list would
//     admit no one and a malformed entry can never match a verified sub);
//   - the synthetic email domain must be non-empty and, when the viewer pins
//     AllowedEmailDomains, must be one of them — otherwise the synthesized
//     email would 403 on the domain gate the branch exists to satisfy.
func validateK8sSA(cfg K8sSAConfig, allowedEmailDomains []string) error {
	u, err := url.Parse(cfg.IssuerURL)
	if err != nil {
		return fmt.Errorf("authn config invalid: K8S_SA_ISSUER_URL is unparseable: %w", err)
	}
	if !strings.EqualFold(u.Scheme, "https") {
		return fmt.Errorf("authn config invalid: K8S_SA_ISSUER_URL must be an https:// URL (got scheme %q)", u.Scheme)
	}
	if u.Host == "" {
		return fmt.Errorf("authn config invalid: K8S_SA_ISSUER_URL has empty host (got %q)", cfg.IssuerURL)
	}

	if cfg.Audience == "" {
		return errors.New("authn config invalid: K8S_SA_AUDIENCE must be set (the dedicated audience the projected SA token is pinned to) when the EKS SA-token branch is enabled")
	}
	if isForbiddenSAAudience(cfg.Audience) {
		return fmt.Errorf("authn config invalid: K8S_SA_AUDIENCE must not be the apiserver or IRSA audience (got %q); mint a dedicated audience for the audit token", cfg.Audience)
	}

	if len(cfg.AllowedSubjects) == 0 {
		return errors.New("authn config invalid: K8S_SA_ALLOWED_SUBJECTS must list at least one system:serviceaccount:<ns>:<sa> subject when the EKS SA-token branch is enabled (an empty allow-list admits no one)") // gitleaks:allow: error text naming an env var, not a secret
	}
	for _, sub := range cfg.AllowedSubjects {
		if _, ok := syntheticSAEmail(sub, "x"); !ok {
			return fmt.Errorf("authn config invalid: K8S_SA_ALLOWED_SUBJECTS entry %q is not a well-formed system:serviceaccount:<ns>:<sa> subject", sub) // gitleaks:allow: error text naming an env var, not a secret
		}
	}

	if cfg.SyntheticEmailDomain == "" {
		return errors.New("authn config invalid: K8S_SA_SYNTHETIC_EMAIL_DOMAIN must be set when the EKS SA-token branch is enabled")
	}
	if len(allowedEmailDomains) > 0 {
		lower := strings.ToLower(cfg.SyntheticEmailDomain)
		matched := false
		for _, d := range allowedEmailDomains {
			if strings.ToLower(d) == lower {
				matched = true
				break
			}
		}
		if !matched {
			return fmt.Errorf("authn config invalid: K8S_SA_SYNTHETIC_EMAIL_DOMAIN %q is not in OIDC_ALLOWED_EMAIL_DOMAINS %v; the synthetic machine email would be rejected by the domain gate", cfg.SyntheticEmailDomain, allowedEmailDomains)
		}
	}

	if cfg.RoleFloor == "" {
		return errors.New("authn config invalid: K8S_SA_ROLE_FLOOR must be set (e.g. gardenio-viewer) when the EKS SA-token branch is enabled; a projected SA token carries no roles claim")
	}

	return nil
}
