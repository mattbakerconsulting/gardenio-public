package microvm

import (
	"context"
	"io"
	"log/slog"
	"strings"
	"testing"
)

// discardLogger is a no-op slog.Logger for tests that only assert on return
// values, not log output.
func discardLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

func TestOptionsEnabled(t *testing.T) {
	cases := []struct {
		name string
		opts Options
		want bool
	}{
		{"both set", Options{LauncherRoleARN: "arn:aws:iam::345678901234:role/ziti-microvm-poc-gardenio-launcher", Region: "us-east-2"}, true},
		{"role only", Options{LauncherRoleARN: "arn:aws:iam::345678901234:role/x"}, false},
		{"region only", Options{Region: "us-east-2"}, false},
		{"neither", Options{}, false},
		{"whitespace role", Options{LauncherRoleARN: "   ", Region: "us-east-2"}, false},
		{"whitespace region", Options{LauncherRoleARN: "arn:x", Region: "  "}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.opts.Enabled(); got != tc.want {
				t.Errorf("Enabled() = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestNewIdentityRejectsIncompleteOptions(t *testing.T) {
	// A disabled Options must never yield an Identity — NewIdentity fails fast
	// before it ever touches the AWS credential chain.
	_, err := NewIdentity(context.Background(), Options{Region: "us-east-2"})
	if err == nil {
		t.Fatal("NewIdentity with no launcher role: want error, got nil")
	}
	if !strings.Contains(err.Error(), "LauncherRoleARN") {
		t.Errorf("error = %q, want mention of LauncherRoleARN", err.Error())
	}
}

// TestNewIdentitySucceedsWithStaticEnvCreds exercises the happy path of the
// credential-chain wiring without reaching AWS: static credentials in the
// environment satisfy LoadDefaultConfig's chain, so NewIdentity builds a
// fully-formed Identity (the stscreds provider is lazy — no STS call is made
// until a client actually needs credentials). This proves the region + client
// plumbing resolves and the accessors return the configured values.
func TestNewIdentitySucceedsWithStaticEnvCreds(t *testing.T) {
	t.Setenv("AWS_ACCESS_KEY_ID", "AKIAIOSFODNN7EXAMPLE")
	t.Setenv("AWS_SECRET_ACCESS_KEY", "wJalrXUtnFEMI/K7MDENG/bPxRfiCYEXAMPLEKEY")
	// Neutralize any ambient IRSA/profile config that could make the chain
	// reach out or pick a different region.
	t.Setenv("AWS_WEB_IDENTITY_TOKEN_FILE", "")
	t.Setenv("AWS_PROFILE", "")

	const launcher = "arn:aws:iam::345678901234:role/ziti-microvm-poc-gardenio-launcher"
	ident, err := NewIdentity(context.Background(), Options{
		LauncherRoleARN: launcher,
		Region:          "us-east-2",
		SessionName:     "test-session",
	})
	if err != nil {
		t.Fatalf("NewIdentity: unexpected error: %v", err)
	}
	if ident.Region() != "us-east-2" {
		t.Errorf("Region() = %q, want us-east-2", ident.Region())
	}
	if ident.LauncherRoleARN() != launcher {
		t.Errorf("LauncherRoleARN() = %q, want %q", ident.LauncherRoleARN(), launcher)
	}
	if ident.Config().Region != "us-east-2" {
		t.Errorf("Config().Region = %q, want us-east-2", ident.Config().Region)
	}
	// Clients construct without panicking (they lazily resolve creds on first call).
	if ident.MicrovmClient() == nil {
		t.Error("MicrovmClient() = nil")
	}
	if ident.SecretsManagerClient() == nil {
		t.Error("SecretsManagerClient() = nil")
	}
	if ident.OriginCredentials() == nil {
		t.Error("OriginCredentials() = nil; POC/rollback origin must be the default chain (ka-0000006814)")
	}
}

func TestNewIdentityDefaultsSessionName(t *testing.T) {
	t.Setenv("AWS_ACCESS_KEY_ID", "AKIAIOSFODNN7EXAMPLE")
	t.Setenv("AWS_SECRET_ACCESS_KEY", "wJalrXUtnFEMI/K7MDENG/bPxRfiCYEXAMPLEKEY")
	t.Setenv("AWS_WEB_IDENTITY_TOKEN_FILE", "")

	// An empty SessionName must not error — it falls back to the stable default.
	ident, err := NewIdentity(context.Background(), Options{
		LauncherRoleARN: "arn:aws:iam::345678901234:role/x",
		Region:          "us-east-2",
	})
	if err != nil {
		t.Fatalf("NewIdentity: unexpected error: %v", err)
	}
	if ident == nil {
		t.Fatal("NewIdentity returned nil Identity")
	}
}

const testLauncher = "arn:aws:iam::345678901234:role/ziti-microvm-poc-gardenio-launcher"
