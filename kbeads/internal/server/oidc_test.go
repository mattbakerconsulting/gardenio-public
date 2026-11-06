package server

import (
	"context"
	"fmt"
	"strings"
	"testing"
)

// multiStubVerifier implements TokenVerifier, IdentityVerifier, and RoleExtractor for testing multiOIDCVerifier.
type multiStubVerifier struct {
	verifyErr error
	email     string
	emailErr  error
	roles     []string
	rolesErr  error
}

func (s *multiStubVerifier) Verify(_ context.Context, _ string) error { return s.verifyErr }
func (s *multiStubVerifier) VerifyAndIdentify(_ context.Context, _ string) (string, error) {
	return s.email, s.emailErr
}
func (s *multiStubVerifier) ExtractRoles(_ context.Context, _ string) ([]string, error) {
	return s.roles, s.rolesErr
}

func TestMultiOIDCVerifier_Verify(t *testing.T) {
	errBad := fmt.Errorf("bad token")

	tests := []struct {
		name      string
		verifiers []TokenVerifier
		wantErr   bool
	}{
		{
			name:      "first succeeds",
			verifiers: []TokenVerifier{&multiStubVerifier{}, &multiStubVerifier{verifyErr: errBad}},
			wantErr:   false,
		},
		{
			name:      "second succeeds",
			verifiers: []TokenVerifier{&multiStubVerifier{verifyErr: errBad}, &multiStubVerifier{}},
			wantErr:   false,
		},
		{
			name:      "all fail",
			verifiers: []TokenVerifier{&multiStubVerifier{verifyErr: errBad}, &multiStubVerifier{verifyErr: errBad}},
			wantErr:   true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			m := &multiOIDCVerifier{verifiers: tt.verifiers}
			err := m.Verify(context.Background(), "tok")
			if (err != nil) != tt.wantErr {
				t.Errorf("Verify() error = %v, wantErr %v", err, tt.wantErr)
			}
		})
	}
}

func TestMultiOIDCVerifier_VerifyAndIdentify(t *testing.T) {
	errBad := fmt.Errorf("bad token")

	tests := []struct {
		name      string
		verifiers []TokenVerifier
		wantEmail string
		wantErr   bool
	}{
		{
			name:      "first returns email",
			verifiers: []TokenVerifier{&multiStubVerifier{email: "a@x.com"}, &multiStubVerifier{email: "b@x.com"}},
			wantEmail: "a@x.com",
		},
		{
			name:      "first fails second returns email",
			verifiers: []TokenVerifier{&multiStubVerifier{emailErr: errBad}, &multiStubVerifier{email: "b@x.com"}},
			wantEmail: "b@x.com",
		},
		{
			name:      "all fail",
			verifiers: []TokenVerifier{&multiStubVerifier{emailErr: errBad}, &multiStubVerifier{emailErr: errBad}},
			wantErr:   true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			m := &multiOIDCVerifier{verifiers: tt.verifiers}
			email, err := m.VerifyAndIdentify(context.Background(), "tok")
			if (err != nil) != tt.wantErr {
				t.Errorf("VerifyAndIdentify() error = %v, wantErr %v", err, tt.wantErr)
			}
			if email != tt.wantEmail {
				t.Errorf("VerifyAndIdentify() email = %q, want %q", email, tt.wantEmail)
			}
		})
	}
}

func TestMultiOIDCVerifier_ExtractRoles(t *testing.T) {
	errBad := fmt.Errorf("bad token")

	tests := []struct {
		name      string
		verifiers []TokenVerifier
		wantRoles []string
		wantErr   bool
	}{
		{
			name:      "first returns roles",
			verifiers: []TokenVerifier{&multiStubVerifier{roles: []string{"admin"}}, &multiStubVerifier{roles: []string{"user"}}},
			wantRoles: []string{"admin"},
		},
		{
			name:      "first fails second returns roles",
			verifiers: []TokenVerifier{&multiStubVerifier{rolesErr: errBad}, &multiStubVerifier{roles: []string{"user"}}},
			wantRoles: []string{"user"},
		},
		{
			name:      "all fail",
			verifiers: []TokenVerifier{&multiStubVerifier{rolesErr: errBad}, &multiStubVerifier{rolesErr: errBad}},
			wantErr:   true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			m := &multiOIDCVerifier{verifiers: tt.verifiers}
			roles, err := m.ExtractRoles(context.Background(), "tok")
			if (err != nil) != tt.wantErr {
				t.Errorf("ExtractRoles() error = %v, wantErr %v", err, tt.wantErr)
			}
			if len(roles) != len(tt.wantRoles) {
				t.Errorf("ExtractRoles() roles = %v, want %v", roles, tt.wantRoles)
			}
		})
	}
}

func TestNewOIDCVerifier_EmptyAudience(t *testing.T) {
	_, err := NewOIDCVerifierWithDiscovery(context.Background(), "https://issuer.example.com", "", "")
	if err == nil {
		t.Fatal("expected error for empty audience, got nil")
	}
	if got := err.Error(); !strings.Contains(got, "audience must not be empty") {
		t.Errorf("error = %q, want it to mention empty audience", got)
	}
}

func TestNewOIDCVerifier_NonEmptyAudience_BadIssuer(t *testing.T) {
	// With a non-empty audience, the error should come from provider discovery,
	// not from the audience check.
	_, err := NewOIDCVerifierWithDiscovery(context.Background(), "https://invalid.example.com", "", "my-client-id")
	if err == nil {
		t.Fatal("expected provider discovery error, got nil")
	}
	if got := err.Error(); !strings.Contains(got, "oidc provider discovery") {
		t.Errorf("error = %q, want provider discovery error", got)
	}
}

func TestMergeGroupClaims(t *testing.T) {
	tests := []struct {
		name         string
		groups       []string // Keycloak "groups"
		groupsDirect []string // GitLab "groups_direct"
		want         []string
	}{
		{
			name:   "keycloak only",
			groups: []string{"/project-alpha", "/project-beta"},
			want:   []string{"/project-alpha", "/project-beta"},
		},
		{
			name:         "gitlab only",
			groupsDirect: []string{"acme/platform", "acme"},
			want:         []string{"acme/platform", "acme"},
		},
		{
			name:         "both merged and deduped",
			groups:       []string{"acme/platform", "/project-alpha"},
			groupsDirect: []string{"acme/platform", "acme"},
			want:         []string{"acme/platform", "/project-alpha", "acme"},
		},
		{
			name:         "empties dropped",
			groups:       []string{"", "acme"},
			groupsDirect: []string{""},
			want:         []string{"acme"},
		},
		{
			name: "both empty yields nil",
			want: nil,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := mergeGroupClaims(tt.groups, tt.groupsDirect)
			if len(got) != len(tt.want) {
				t.Fatalf("mergeGroupClaims() = %v, want %v", got, tt.want)
			}
			for i := range got {
				if got[i] != tt.want[i] {
					t.Errorf("mergeGroupClaims()[%d] = %q, want %q", i, got[i], tt.want[i])
				}
			}
		})
	}
}

func TestIsAllowedEmailDomain(t *testing.T) {
	tests := []struct {
		email   string
		allowed []string
		want    bool
	}{
		{"user@acme.example", []string{"acme.example"}, true},
		{"user@acme.example", []string{"acme.example"}, true},
		{"USER@acme.example", []string{"acme.example"}, true},
		{"user@other.com", []string{"acme.example"}, false},
		{"user@acme.example", []string{"acme.example", "acme.example"}, true},
		{"user@acme.example", []string{"acme.example", "acme.example"}, true},
		{"user@evil.com", []string{"acme.example", "acme.example"}, false},
		{"invalid-email", []string{"acme.example"}, false},
		{"", []string{"acme.example"}, false},
		{"user@acme.example", nil, false},
		{"user@acme.example", []string{}, false},
	}

	for _, tt := range tests {
		got := isAllowedEmailDomain(tt.email, tt.allowed)
		if got != tt.want {
			t.Errorf("isAllowedEmailDomain(%q, %v) = %v, want %v", tt.email, tt.allowed, got, tt.want)
		}
	}
}
