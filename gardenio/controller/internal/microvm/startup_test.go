package microvm

import (
	"context"
	"errors"
	"log/slog"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/lambdamicrovms"
	"github.com/aws/aws-sdk-go-v2/service/secretsmanager"
)

// fakeStartupIdentity is a test double for the startupIdentity seam: it lets the
// verify branch fail/succeed and hands out real (never-called-in-test) clients
// so newBackendFromIdentity's config validation can run without AWS.
type fakeStartupIdentity struct {
	caller   *CallerIdentity
	verifErr error
	region   string
}

func (f fakeStartupIdentity) MicrovmClient() *lambdamicrovms.Client { return &lambdamicrovms.Client{} }
func (f fakeStartupIdentity) SecretsManagerClient() *secretsmanager.Client {
	return &secretsmanager.Client{}
}
func (f fakeStartupIdentity) Region() string { return f.region }
func (f fakeStartupIdentity) VerifyCallerIdentity(context.Context) (*CallerIdentity, error) {
	return f.caller, f.verifErr
}
func (f fakeStartupIdentity) OriginCredentials() aws.CredentialsProvider { return staticCreds{} }

func okCaller() *CallerIdentity {
	return &CallerIdentity{
		Account: "345678901234",
		ARN:     "arn:aws:sts::345678901234:assumed-role/ziti-microvm-poc-gardenio-launcher/gardenio-microvm-controller",
	}
}

// noIssuer is the issuer factory for tests that do not exercise the broker
// branch (Broker unset ⇒ never called).
func noIssuer(context.Context, BrokerOptions, aws.CredentialsProvider, *slog.Logger) (identityIssuer, error) {
	return nil, errors.New("noIssuer factory must not be called when the broker is unconfigured")
}

// TestStartupBackendDisabled: with the identity disabled (no launcher role /
// region), StartupBackend is a no-op returning (nil, nil) — the default-off
// contract, so the spawn door fails closed and the controller keeps booting.
func TestStartupBackendDisabled(t *testing.T) {
	called := false
	backend, err := startupBackend(context.Background(), discardLogger(),
		StartupConfig{Identity: Options{}, Backend: testBackendConfig()},
		func(context.Context, Options) (startupIdentity, error) {
			called = true
			return nil, nil
		}, noIssuer)
	if err != nil {
		t.Fatalf("disabled StartupBackend: unexpected error %v", err)
	}
	if backend != nil {
		t.Errorf("disabled StartupBackend: backend = %+v, want nil", backend)
	}
	if called {
		t.Error("disabled StartupBackend built an identity; want no AWS work")
	}
}

// TestStartupBackendConstructError: an identity-construction failure is returned
// best-effort (nil backend, err) — the controller keeps booting.
func TestStartupBackendConstructError(t *testing.T) {
	wantErr := errors.New("boom: bad IRSA")
	backend, err := startupBackend(context.Background(), discardLogger(),
		StartupConfig{Identity: Options{LauncherRoleARN: testLauncher, Region: "us-east-2"}, Backend: testBackendConfig()},
		func(context.Context, Options) (startupIdentity, error) {
			return nil, wantErr
		}, noIssuer)
	if !errors.Is(err, wantErr) {
		t.Fatalf("construct-error StartupBackend: err = %v, want %v", err, wantErr)
	}
	if backend != nil {
		t.Errorf("construct-error StartupBackend: backend = %+v, want nil", backend)
	}
}

// TestStartupBackendVerifyError: an assume-role verification failure (bad trust)
// yields (nil, err) best-effort.
func TestStartupBackendVerifyError(t *testing.T) {
	wantErr := errors.New("AccessDenied: not trusted")
	backend, err := startupBackend(context.Background(), discardLogger(),
		StartupConfig{Identity: Options{LauncherRoleARN: testLauncher, Region: "us-east-2"}, Backend: testBackendConfig()},
		func(context.Context, Options) (startupIdentity, error) {
			return fakeStartupIdentity{verifErr: wantErr}, nil
		}, noIssuer)
	if !errors.Is(err, wantErr) {
		t.Fatalf("verify-error StartupBackend: err = %v, want %v", err, wantErr)
	}
	if backend != nil {
		t.Errorf("verify-error StartupBackend: backend = %+v, want nil", backend)
	}
}

// TestStartupBackendConfigIncomplete: a verified identity but incomplete launch
// config yields (nil, err) — the identity works, but the backend must not issue
// malformed RunMicrovm, so it stays unavailable and the spawn door fails closed.
func TestStartupBackendConfigIncomplete(t *testing.T) {
	backend, err := startupBackend(context.Background(), discardLogger(),
		StartupConfig{
			Identity: Options{LauncherRoleARN: testLauncher, Region: "us-east-2"},
			Backend:  BackendConfig{Region: "us-east-2", SecretPrefix: "p/"}, // no image/exec role
		},
		func(context.Context, Options) (startupIdentity, error) {
			return fakeStartupIdentity{caller: okCaller(), region: "us-east-2"}, nil
		}, noIssuer)
	if err == nil {
		t.Fatal("config-incomplete StartupBackend: want error, got nil")
	}
	if backend != nil {
		t.Errorf("config-incomplete StartupBackend: backend = %+v, want nil", backend)
	}
}

// TestStartupBackendSuccess: verified identity + complete config yields a ready
// Backend.
func TestStartupBackendSuccess(t *testing.T) {
	backend, err := startupBackend(context.Background(), discardLogger(),
		StartupConfig{
			Identity: Options{LauncherRoleARN: testLauncher, Region: "us-east-2"},
			Backend:  testBackendConfig(),
		},
		func(context.Context, Options) (startupIdentity, error) {
			return fakeStartupIdentity{caller: okCaller(), region: "us-east-2"}, nil
		}, noIssuer)
	if err != nil {
		t.Fatalf("success StartupBackend: %v", err)
	}
	if backend == nil {
		t.Fatal("success StartupBackend: backend = nil, want ready backend")
	}
	if backend.cfg.ImageIdentifier != testBackendConfig().ImageIdentifier {
		t.Errorf("backend image = %q, want %q", backend.cfg.ImageIdentifier, testBackendConfig().ImageIdentifier)
	}
}

// TestStartupBackendBrokerWired: with the broker configured, the built backend
// carries the issuer (per-run identity regime on).
func TestStartupBackendBrokerWired(t *testing.T) {
	iss := &fakeIssuer{}
	backend, err := startupBackend(context.Background(), discardLogger(),
		StartupConfig{
			Identity: Options{LauncherRoleARN: testLauncher, Region: "us-east-2"},
			Backend:  testBackendConfig(),
			Broker:   BrokerOptions{URL: "http://broker.fabric:8080"},
		},
		func(context.Context, Options) (startupIdentity, error) {
			return fakeStartupIdentity{caller: okCaller(), region: "us-east-2"}, nil
		},
		func(_ context.Context, o BrokerOptions, creds aws.CredentialsProvider, _ *slog.Logger) (identityIssuer, error) {
			if o.URL != "http://broker.fabric:8080" {
				t.Errorf("issuer factory got URL %q", o.URL)
			}
			if creds == nil {
				t.Error("issuer factory got nil origin credentials — the broker proof must be signed with the identity's origin credentials")
			}
			return iss, nil
		})
	if err != nil {
		t.Fatalf("broker-wired StartupBackend: %v", err)
	}
	if backend == nil || !backend.PerRunIdentityEnabled() {
		t.Fatalf("broker-wired StartupBackend: per-run identity not enabled (backend=%v)", backend)
	}
}

// TestStartupBackendBrokerFailClosed: a configured broker whose client cannot
// be built must yield (nil, err) — never a backend silently degraded to the
// static shared identity.
func TestStartupBackendBrokerFailClosed(t *testing.T) {
	wantErr := errors.New("no controller credentials")
	backend, err := startupBackend(context.Background(), discardLogger(),
		StartupConfig{
			Identity: Options{LauncherRoleARN: testLauncher, Region: "us-east-2"},
			Backend:  testBackendConfig(),
			Broker:   BrokerOptions{URL: "http://broker.fabric:8080"},
		},
		func(context.Context, Options) (startupIdentity, error) {
			return fakeStartupIdentity{caller: okCaller(), region: "us-east-2"}, nil
		},
		func(context.Context, BrokerOptions, aws.CredentialsProvider, *slog.Logger) (identityIssuer, error) {
			return nil, wantErr
		})
	if !errors.Is(err, wantErr) {
		t.Fatalf("broker-fail StartupBackend: err = %v, want %v", err, wantErr)
	}
	if backend != nil {
		t.Errorf("broker-fail StartupBackend: backend = %+v, want nil (fail closed)", backend)
	}
}

// TestStartupBackendRealConstructorDisabled exercises the exported StartupBackend
// (which wires in NewIdentity) on the disabled path, covering the adapter.
func TestStartupBackendRealConstructorDisabled(t *testing.T) {
	backend, err := StartupBackend(context.Background(), discardLogger(), StartupConfig{})
	if err != nil || backend != nil {
		t.Fatalf("StartupBackend(disabled) = (%+v, %v), want (nil, nil)", backend, err)
	}
}

// TestNewBackendRejectsIncompleteConfig: a verified identity with incomplete
// launch config must not yield a Backend (so the spawn door never issues a
// malformed RunMicrovm).
func TestNewBackendRejectsIncompleteConfig(t *testing.T) {
	t.Setenv("AWS_ACCESS_KEY_ID", "AKIAIOSFODNN7EXAMPLE")
	t.Setenv("AWS_SECRET_ACCESS_KEY", "wJalrXUtnFEMI/K7MDENG/bPxRfiCYEXAMPLEKEY")
	t.Setenv("AWS_WEB_IDENTITY_TOKEN_FILE", "")
	t.Setenv("AWS_PROFILE", "")

	ident, err := NewIdentity(context.Background(), Options{
		LauncherRoleARN: testLauncher,
		Region:          "us-east-2",
	})
	if err != nil {
		t.Fatalf("NewIdentity: %v", err)
	}
	// Missing ImageIdentifier / ExecutionRoleARN.
	_, err = newBackendFromIdentity(ident, BackendConfig{Region: "us-east-2", SecretPrefix: "p/"}, discardLogger())
	if err == nil {
		t.Fatal("newBackendFromIdentity with incomplete config: want error, got nil")
	}
}

// TestNewBackendDefaultsFromIdentity: an empty BackendConfig.Region falls back
// to the identity's region, and MaximumDurationSeconds defaults when unset.
func TestNewBackendDefaultsFromIdentity(t *testing.T) {
	t.Setenv("AWS_ACCESS_KEY_ID", "AKIAIOSFODNN7EXAMPLE")
	t.Setenv("AWS_SECRET_ACCESS_KEY", "wJalrXUtnFEMI/K7MDENG/bPxRfiCYEXAMPLEKEY")
	t.Setenv("AWS_WEB_IDENTITY_TOKEN_FILE", "")
	t.Setenv("AWS_PROFILE", "")

	ident, err := NewIdentity(context.Background(), Options{
		LauncherRoleARN: testLauncher,
		Region:          "us-east-2",
	})
	if err != nil {
		t.Fatalf("NewIdentity: %v", err)
	}
	cfg := testBackendConfig()
	cfg.Region = "" // should fall back to identity region
	cfg.MaximumDurationSeconds = 0
	b, err := newBackendFromIdentity(ident, cfg, discardLogger())
	if err != nil {
		t.Fatalf("newBackendFromIdentity: %v", err)
	}
	if b.cfg.Region != "us-east-2" {
		t.Errorf("cfg.Region = %q, want fallback us-east-2", b.cfg.Region)
	}
	if b.cfg.MaximumDurationSeconds != defaultMaximumDurationSeconds {
		t.Errorf("cfg.MaximumDurationSeconds = %d, want default %d", b.cfg.MaximumDurationSeconds, defaultMaximumDurationSeconds)
	}
}
