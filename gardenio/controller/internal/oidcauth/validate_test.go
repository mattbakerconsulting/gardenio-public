package oidcauth

import (
	"strings"
	"testing"
)

func TestValidate(t *testing.T) {
	const goodARN = "arn:aws:ec2:us-east-1:234567890123:verified-access-instance/vai-0123456789abcdef0"

	tests := []struct {
		name    string
		cfg     Config
		opts    ValidateOptions
		wantErr string
	}{
		{
			name: "dev bypass alone is ok",
			opts: ValidateOptions{DevAuthBypassEnabled: true},
		},
		{
			name:    "dev bypass + AVA is conflict",
			cfg:     Config{AVA: AVAConfig{TrustSigner: goodARN}},
			opts:    ValidateOptions{DevAuthBypassEnabled: true},
			wantErr: "BEADS_VIEWER_DEV_AUTH_BYPASS set together with real auth env",
		},
		{
			name:    "dev bypass + Keycloak is conflict",
			cfg:     Config{IssuerURL: "https://kc.example.com/realms/x"},
			opts:    ValidateOptions{DevAuthBypassEnabled: true},
			wantErr: "BEADS_VIEWER_DEV_AUTH_BYPASS set together with real auth env",
		},
		{
			name:    "neither auth nor bypass is refused",
			wantErr: "neither AVA nor Keycloak configured",
		},
		{
			name:    "malformed AVA ARN is refused",
			cfg:     Config{AVA: AVAConfig{TrustSigner: "not-an-arn"}},
			wantErr: "AVA_TRUST_SIGNER must be an AVA instance ARN",
		},
		{
			name:    "malformed AVA region is refused",
			cfg:     Config{AVA: AVAConfig{TrustSigner: goodARN, Region: "US-EAST-1"}},
			wantErr: "AVA_REGION must look like an AWS region",
		},
		{
			name: "AVA-only with no region defaults ok",
			cfg:  Config{AVA: AVAConfig{TrustSigner: goodARN}},
		},
		{
			name: "AVA-only with good region ok",
			cfg:  Config{AVA: AVAConfig{TrustSigner: goodARN, Region: "us-west-2"}},
		},
		{
			name:    "non-https OIDC issuer is refused",
			cfg:     Config{IssuerURL: "http://kc.example.com/realms/x"},
			wantErr: `OIDC_ISSUER_URL must be an https:// URL`,
		},
		{
			name:    "unparseable OIDC URL is refused",
			cfg:     Config{IssuerURL: "://broken"},
			wantErr: "OIDC_ISSUER_URL is unparseable",
		},
		{
			name:    "empty-host OIDC URL is refused",
			cfg:     Config{IssuerURL: "https:///realms/x"},
			wantErr: "OIDC_ISSUER_URL has empty host",
		},
		{
			name: "good OIDC-only is ok",
			cfg:  Config{IssuerURL: "https://kc.example.com/realms/x"},
		},
		{
			name: "AVA + Keycloak both configured is ok",
			cfg: Config{
				IssuerURL: "https://kc.example.com/realms/x",
				AVA:       AVAConfig{TrustSigner: goodARN, Region: "us-east-1"},
			},
		},

		// EKS SA-token branch (ka-0000006288).
		{
			name: "SA branch alongside a human path with full config is ok",
			cfg: Config{
				IssuerURL:           "https://kc.example.com/realms/x",
				AllowedEmailDomains: []string{"acme.example"},
				K8sSA:               goodSACfg(),
			},
		},
		{
			name:    "SA branch without a human path is refused",
			cfg:     Config{K8sSA: goodSACfg()},
			wantErr: "the EKS SA-token branch is additive",
		},
		{
			name:    "dev bypass + SA branch is conflict",
			cfg:     Config{K8sSA: goodSACfg()},
			opts:    ValidateOptions{DevAuthBypassEnabled: true},
			wantErr: "BEADS_VIEWER_DEV_AUTH_BYPASS set together with real auth env",
		},
		{
			name: "SA non-https issuer is refused",
			cfg: Config{
				IssuerURL: "https://kc.example.com/realms/x",
				K8sSA:     withSA(func(c *K8sSAConfig) { c.IssuerURL = "http://oidc.eks.example/id/x" }),
			},
			wantErr: "K8S_SA_ISSUER_URL must be an https:// URL",
		},
		{
			name: "SA empty audience is refused",
			cfg: Config{
				IssuerURL: "https://kc.example.com/realms/x",
				K8sSA:     withSA(func(c *K8sSAConfig) { c.Audience = "" }),
			},
			wantErr: "K8S_SA_AUDIENCE must be set",
		},
		{
			name: "SA apiserver audience is refused",
			cfg: Config{
				IssuerURL: "https://kc.example.com/realms/x",
				K8sSA:     withSA(func(c *K8sSAConfig) { c.Audience = "https://kubernetes.default.svc" }),
			},
			wantErr: "must not be the apiserver or IRSA audience",
		},
		{
			name: "SA IRSA audience is refused",
			cfg: Config{
				IssuerURL: "https://kc.example.com/realms/x",
				K8sSA:     withSA(func(c *K8sSAConfig) { c.Audience = "sts.amazonaws.com" }),
			},
			wantErr: "must not be the apiserver or IRSA audience",
		},
		{
			name: "SA empty allow-list is refused",
			cfg: Config{
				IssuerURL: "https://kc.example.com/realms/x",
				K8sSA:     withSA(func(c *K8sSAConfig) { c.AllowedSubjects = nil }),
			},
			wantErr: "K8S_SA_ALLOWED_SUBJECTS must list at least one",
		},
		{
			name: "SA malformed allow-list entry is refused",
			cfg: Config{
				IssuerURL: "https://kc.example.com/realms/x",
				K8sSA:     withSA(func(c *K8sSAConfig) { c.AllowedSubjects = []string{"not-a-sa-subject"} }),
			},
			wantErr: "is not a well-formed system:serviceaccount",
		},
		{
			name: "SA empty synthetic domain is refused",
			cfg: Config{
				IssuerURL: "https://kc.example.com/realms/x",
				K8sSA:     withSA(func(c *K8sSAConfig) { c.SyntheticEmailDomain = "" }),
			},
			wantErr: "K8S_SA_SYNTHETIC_EMAIL_DOMAIN must be set",
		},
		{
			name: "SA synthetic domain outside allowed domains is refused",
			cfg: Config{
				IssuerURL:           "https://kc.example.com/realms/x",
				AllowedEmailDomains: []string{"acme.example"},
				K8sSA:               withSA(func(c *K8sSAConfig) { c.SyntheticEmailDomain = "evil.example" }),
			},
			wantErr: "is not in OIDC_ALLOWED_EMAIL_DOMAINS",
		},
		{
			name: "SA synthetic domain inside allowed domains is ok",
			cfg: Config{
				IssuerURL:           "https://kc.example.com/realms/x",
				AllowedEmailDomains: []string{"acme.example", "acme.example"},
				K8sSA:               goodSACfg(),
			},
		},
		{
			name: "SA empty role floor is refused",
			cfg: Config{
				IssuerURL: "https://kc.example.com/realms/x",
				K8sSA:     withSA(func(c *K8sSAConfig) { c.RoleFloor = "" }),
			},
			wantErr: "K8S_SA_ROLE_FLOOR must be set",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			err := Validate(tc.cfg, tc.opts)
			if tc.wantErr == "" {
				if err != nil {
					t.Fatalf("Validate() = %v, want nil", err)
				}
				return
			}
			if err == nil {
				t.Fatalf("Validate() = nil, want error containing %q", tc.wantErr)
			}
			if !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("Validate() err = %q, want substring %q", err.Error(), tc.wantErr)
			}
		})
	}
}

// goodSACfg returns a fully-valid enabled K8sSAConfig for the Validate tests
// (issuer https, dedicated audience, one well-formed allow-listed subject,
// synthetic domain acme.example, viewer role floor).
func goodSACfg() K8sSAConfig {
	return K8sSAConfig{
		IssuerURL:            "https://oidc.eks.us-east-1.amazonaws.com/id/2CF256B9108E7AD0AF9AC2451950078B",
		Audience:             "gardenio-beads-viewer-audit",
		AllowedSubjects:      []string{"system:serviceaccount:gardenio-system:gardenio-audit"},
		SyntheticEmailDomain: "acme.example",
		RoleFloor:            RoleViewer,
	}
}

// withSA returns goodSACfg mutated by fn, so a test case can flip exactly one
// field invalid without restating the whole struct.
func withSA(fn func(*K8sSAConfig)) K8sSAConfig {
	c := goodSACfg()
	fn(&c)
	return c
}
