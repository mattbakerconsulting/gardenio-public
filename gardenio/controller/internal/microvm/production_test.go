package microvm

import (
	"context"
	"errors"
	"log/slog"
	"strings"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
)

// Production configuration & rollout (ka-0000006687): with ProductionMode set,
// every surviving POC assumption must refuse the backend at startup —
// fail-closed, never a degraded launch on the shared identity or a floating
// image version.

// prodStartupConfig is a fully production-clean StartupConfig: broker on, no
// static identity, dedicated controller role, pinned launcher account +
// image version. Tests knock out one field at a time.
func prodStartupConfig() StartupConfig {
	backend := testBackendConfig()
	backend.StaticIdentitySecretARN = ""
	backend.ProductionMode = true
	backend.ExpectedLauncherAccount = "345678901234"
	backend.LogGroup = "/aws/lambda/microvms/ziti-microvm-poc"
	return StartupConfig{
		Identity: Options{
			LauncherRoleARN:   testLauncher,
			Region:            "us-east-2",
			ControllerRoleARN: "arn:aws:iam::123456789012:role/gardenio-prod-microvm-controller",
		},
		Backend: backend,
		Broker:  BrokerOptions{URL: "http://ziti-access-broker.fabric:8080"},
	}
}

func prodIdentityFactory(context.Context, Options) (startupIdentity, error) {
	return fakeStartupIdentity{caller: okCaller(), region: "us-east-2"}, nil
}

func prodIssuerFactory(context.Context, BrokerOptions, aws.CredentialsProvider, *slog.Logger) (identityIssuer, error) {
	return &fakeIssuer{}, nil
}

// TestStartupBackendProductionClean: a fully production-clean config builds a
// ready backend with per-run identities on.
func TestStartupBackendProductionClean(t *testing.T) {
	backend, err := startupBackend(context.Background(), discardLogger(),
		prodStartupConfig(), prodIdentityFactory, prodIssuerFactory)
	if err != nil {
		t.Fatalf("production-clean StartupBackend: %v", err)
	}
	if backend == nil || !backend.PerRunIdentityEnabled() {
		t.Fatalf("production-clean StartupBackend: want ready per-run backend, got %+v", backend)
	}
	if !backend.cfg.ProductionMode {
		t.Error("backend did not carry ProductionMode")
	}
}

// TestStartupBackendProductionRefusesPOCLeftovers: each surviving POC
// assumption independently refuses the backend, and the error names the field
// so the operator remediation is legible.
func TestStartupBackendProductionRefusesPOCLeftovers(t *testing.T) {
	cases := []struct {
		name     string
		mutate   func(*StartupConfig)
		wantWord string
	}{
		{
			name: "static identity still set",
			mutate: func(sc *StartupConfig) {
				sc.Backend.StaticIdentitySecretARN = testBackendConfig().StaticIdentitySecretARN
			},
			wantWord: "StaticIdentitySecretARN",
		},
		{
			name:     "broker unset",
			mutate:   func(sc *StartupConfig) { sc.Broker = BrokerOptions{} },
			wantWord: "ZitiBrokerURL",
		},
		{
			name:     "controller role unset",
			mutate:   func(sc *StartupConfig) { sc.Identity.ControllerRoleARN = "" },
			wantWord: "ControllerRoleARN",
		},
		{
			name:     "launcher account unpinned",
			mutate:   func(sc *StartupConfig) { sc.Backend.ExpectedLauncherAccount = "" },
			wantWord: "ExpectedLauncherAccount",
		},
		{
			name:     "image version floating",
			mutate:   func(sc *StartupConfig) { sc.Backend.ImageVersion = "" },
			wantWord: "ImageVersion",
		},
		{
			name:     "log group unset",
			mutate:   func(sc *StartupConfig) { sc.Backend.LogGroup = "" },
			wantWord: "LogGroup",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			sc := prodStartupConfig()
			tc.mutate(&sc)
			identityBuilt := false
			backend, err := startupBackend(context.Background(), discardLogger(), sc,
				func(ctx context.Context, o Options) (startupIdentity, error) {
					identityBuilt = true
					return prodIdentityFactory(ctx, o)
				}, prodIssuerFactory)
			if err == nil {
				t.Fatal("want production-mode refusal, got nil error")
			}
			if backend != nil {
				t.Errorf("backend = %+v, want nil (fail closed)", backend)
			}
			if !strings.Contains(err.Error(), tc.wantWord) {
				t.Errorf("error %q does not name %q", err, tc.wantWord)
			}
			if identityBuilt {
				t.Error("identity was built despite production-mode refusal; validation must run before any AWS work")
			}
		})
	}
}

// TestStartupBackendProductionRefusalListsAllViolations: multiple surviving
// assumptions surface in ONE error so the operator fixes them in one pass.
func TestStartupBackendProductionRefusalListsAllViolations(t *testing.T) {
	sc := prodStartupConfig()
	sc.Backend.StaticIdentitySecretARN = testBackendConfig().StaticIdentitySecretARN
	sc.Broker = BrokerOptions{}
	sc.Identity.ControllerRoleARN = ""
	_, err := startupBackend(context.Background(), discardLogger(), sc,
		prodIdentityFactory, prodIssuerFactory)
	if err == nil {
		t.Fatal("want production-mode refusal, got nil error")
	}
	for _, word := range []string{"StaticIdentitySecretARN", "ZitiBrokerURL", "ControllerRoleARN"} {
		if !strings.Contains(err.Error(), word) {
			t.Errorf("joined error %q does not name %q", err, word)
		}
	}
}

// TestStartupBackendProductionOffKeepsPOCSemantics: with ProductionMode off,
// the exact POC config (static identity, no broker, no controller role, no
// account pin) still builds — the rollback path stays byte-identical.
func TestStartupBackendProductionOffKeepsPOCSemantics(t *testing.T) {
	backend, err := startupBackend(context.Background(), discardLogger(),
		StartupConfig{
			Identity: Options{LauncherRoleARN: testLauncher, Region: "us-east-2"},
			Backend:  testBackendConfig(),
		}, prodIdentityFactory, noIssuer)
	if err != nil {
		t.Fatalf("POC-config StartupBackend with production off: %v", err)
	}
	if backend == nil {
		t.Fatal("POC-config StartupBackend: want ready backend, got nil")
	}
}

// TestStartupBackendLauncherAccountMismatch: the verified caller account must
// match the pin — a launcher that, once assumed, resolves to a different account
// than configured fails closed at boot. Applies even outside production mode
// whenever the pin is set. The config here is STATICALLY consistent (all
// launcher-side ARNs + the pin agree on 751), so the static consistency check
// passes and this isolates the DYNAMIC GetCallerIdentity check: the fake caller
// resolves to a different account.
func TestStartupBackendLauncherAccountMismatch(t *testing.T) {
	cfg := testBackendConfig()
	cfg.ExpectedLauncherAccount = "345678901234"
	mismatchCaller := &CallerIdentity{
		Account: "999999999999",
		ARN:     "arn:aws:sts::999999999999:assumed-role/x/y",
	}
	backend, err := startupBackend(context.Background(), discardLogger(),
		StartupConfig{
			Identity: Options{LauncherRoleARN: testLauncher, Region: "us-east-2"},
			Backend:  cfg,
		},
		func(context.Context, Options) (startupIdentity, error) {
			return fakeStartupIdentity{caller: mismatchCaller, region: "us-east-2"}, nil
		}, noIssuer)
	if err == nil {
		t.Fatal("want launcher-account mismatch error, got nil")
	}
	if backend != nil {
		t.Errorf("backend = %+v, want nil (fail closed)", backend)
	}
	if !strings.Contains(err.Error(), "999999999999") || !strings.Contains(err.Error(), "345678901234") {
		t.Errorf("error %q should name both expected and observed accounts", err)
	}
}

// TestStartupBackendLauncherAccountMatch: a correct pin passes.
func TestStartupBackendLauncherAccountMatch(t *testing.T) {
	cfg := testBackendConfig()
	cfg.ExpectedLauncherAccount = okCaller().Account
	backend, err := startupBackend(context.Background(), discardLogger(),
		StartupConfig{
			Identity: Options{LauncherRoleARN: testLauncher, Region: "us-east-2"},
			Backend:  cfg,
		}, prodIdentityFactory, noIssuer)
	if err != nil {
		t.Fatalf("account-pinned StartupBackend: %v", err)
	}
	if backend == nil {
		t.Fatal("account-pinned StartupBackend: want ready backend, got nil")
	}
}

// TestLaunchProductionModeRefusesWithoutIssuer: defense in depth — even if a
// production backend is somehow constructed without an issuer, Launch refuses
// rather than falling back to the static identity.
func TestLaunchProductionModeRefusesWithoutIssuer(t *testing.T) {
	cfg := testBackendConfig()
	cfg.ProductionMode = true
	mv := &fakeMicrovm{}
	sm := newFakeSecrets()
	b := mustBackend(t, mv, sm, cfg)
	_, err := b.Launch(context.Background(), AgentSpec{BeadID: "gd-ProdT1"})
	if err == nil {
		t.Fatal("production Launch without issuer: want refusal, got nil error")
	}
	if !strings.Contains(err.Error(), "production mode") {
		t.Errorf("error %q should name production mode", err)
	}
	if mv.runCalls != 0 {
		t.Errorf("RunMicrovm was called %d times; want 0 (fail closed)", mv.runCalls)
	}
	if len(sm.created) != 0 {
		t.Errorf("secrets were written (%d); want none (fail closed)", len(sm.created))
	}
}

// TestNewIdentityControllerRoleRequiresTokenFile: with ControllerRoleARN set
// but no projected IRSA token in the environment, NewIdentity must error —
// the dedicated-role hop cannot silently degrade to the default chain.
func TestNewIdentityControllerRoleRequiresTokenFile(t *testing.T) {
	t.Setenv("AWS_ACCESS_KEY_ID", "AKIAIOSFODNN7EXAMPLE")
	t.Setenv("AWS_SECRET_ACCESS_KEY", "wJalrXUtnFEMI/K7MDENG/bPxRfiCYEXAMPLEKEY")
	t.Setenv("AWS_WEB_IDENTITY_TOKEN_FILE", "")
	t.Setenv("AWS_PROFILE", "")

	_, err := NewIdentity(context.Background(), Options{
		LauncherRoleARN:   testLauncher,
		Region:            "us-east-2",
		ControllerRoleARN: "arn:aws:iam::123456789012:role/gardenio-prod-microvm-controller",
	})
	if err == nil {
		t.Fatal("NewIdentity with controller role but no token file: want error, got nil")
	}
	if !strings.Contains(err.Error(), webIdentityTokenFileEnv) {
		t.Errorf("error %q should name %s", err, webIdentityTokenFileEnv)
	}
}

// TestNewIdentityControllerRoleConstructs: with the token file present the
// dedicated-role chain constructs (no STS call is made at build time) and the
// Identity reports the controller role.
func TestNewIdentityControllerRoleConstructs(t *testing.T) {
	t.Setenv("AWS_ACCESS_KEY_ID", "AKIAIOSFODNN7EXAMPLE")
	t.Setenv("AWS_SECRET_ACCESS_KEY", "wJalrXUtnFEMI/K7MDENG/bPxRfiCYEXAMPLEKEY")
	tokenFile := t.TempDir() + "/token"
	t.Setenv("AWS_WEB_IDENTITY_TOKEN_FILE", tokenFile)
	t.Setenv("AWS_PROFILE", "")

	const controllerRole = "arn:aws:iam::123456789012:role/gardenio-prod-microvm-controller"
	ident, err := NewIdentity(context.Background(), Options{
		LauncherRoleARN:   testLauncher,
		Region:            "us-east-2",
		ControllerRoleARN: controllerRole,
	})
	if err != nil {
		t.Fatalf("NewIdentity with controller role: %v", err)
	}
	if ident.ControllerRoleARN() != controllerRole {
		t.Errorf("ControllerRoleARN() = %q, want %q", ident.ControllerRoleARN(), controllerRole)
	}
	if ident.LauncherRoleARN() != testLauncher {
		t.Errorf("LauncherRoleARN() = %q, want %q", ident.LauncherRoleARN(), testLauncher)
	}
	if ident.OriginCredentials() == nil {
		t.Error("OriginCredentials() = nil; the broker proof needs the dedicated-role origin credentials (ka-0000006814)")
	}
}

// TestValidateProductionModeOffIsNil: the validator is a no-op when production
// mode is off, regardless of POC leftovers.
func TestValidateProductionModeOffIsNil(t *testing.T) {
	sc := StartupConfig{Backend: testBackendConfig()}
	if err := sc.validateProductionMode(); err != nil {
		t.Fatalf("validateProductionMode with mode off: %v", err)
	}
}

// TestStartupBackendProductionBrokerBuildFailure: production mode does not
// weaken the existing broker fail-closed contract — a broker client build
// error still refuses the backend.
func TestStartupBackendProductionBrokerBuildFailure(t *testing.T) {
	wantErr := errors.New("no controller credentials")
	backend, err := startupBackend(context.Background(), discardLogger(),
		prodStartupConfig(), prodIdentityFactory,
		func(context.Context, BrokerOptions, aws.CredentialsProvider, *slog.Logger) (identityIssuer, error) {
			return nil, wantErr
		})
	if !errors.Is(err, wantErr) {
		t.Fatalf("broker-build-failure err = %v, want %v", err, wantErr)
	}
	if backend != nil {
		t.Errorf("backend = %+v, want nil (fail closed)", backend)
	}
}
