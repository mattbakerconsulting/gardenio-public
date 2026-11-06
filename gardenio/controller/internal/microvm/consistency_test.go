package microvm

import (
	"context"
	"strings"
	"testing"
)

// Static account/region/role consistency (S3 ka-0000002156): the backend refuses
// a half-flipped account-migration config BEFORE any AWS work, so RunMicrovm can
// never straddle two accounts (launcher in one account, image/execution role in
// another) or two regions.

func TestParseARN(t *testing.T) {
	cases := []struct {
		name        string
		arn         string
		wantAccount string
		wantRegion  string
	}{
		{
			name:        "iam role (no region)",
			arn:         "arn:aws:iam::123456789012:role/gardenio-prod-microvm-agent-launcher",
			wantAccount: "123456789012",
			wantRegion:  "",
		},
		{
			name:        "lambda microvm image (region + account)",
			arn:         "arn:aws:lambda:us-east-1:123456789012:microvm-image:gardenio-prod-microvm-agent",
			wantAccount: "123456789012",
			wantRegion:  "us-east-1",
		},
		{
			name:        "secretsmanager arn with colons in resource",
			arn:         "arn:aws:secretsmanager:us-east-2:345678901234:secret:ziti-microvm-poc/ziti-identity-kCfNSS",
			wantAccount: "345678901234",
			wantRegion:  "us-east-2",
		},
		{name: "empty", arn: "", wantAccount: "", wantRegion: ""},
		{name: "not an arn", arn: "gardenio-prod-microvm-agent", wantAccount: "", wantRegion: ""},
		{name: "too few fields", arn: "arn:aws:iam", wantAccount: "", wantRegion: ""},
		{
			name:        "whitespace trimmed",
			arn:         "  arn:aws:iam::123456789012:role/x  ",
			wantAccount: "123456789012",
			wantRegion:  "",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := parseARN(tc.arn)
			if got.account != tc.wantAccount {
				t.Errorf("account = %q, want %q", got.account, tc.wantAccount)
			}
			if got.region != tc.wantRegion {
				t.Errorf("region = %q, want %q", got.region, tc.wantRegion)
			}
		})
	}
}

// gardenioAccountConfig is a fully same-account (post-S3-migration) StartupConfig
// targeting the Gardenio account 123456789012 in us-east-1: launcher, execution
// role, and image all in-account, region matched, launcher account pinned.
func gardenioAccountConfig() StartupConfig {
	return StartupConfig{
		Identity: Options{
			LauncherRoleARN:   "arn:aws:iam::123456789012:role/gardenio-prod-microvm-agent-launcher",
			ControllerRoleARN: "arn:aws:iam::123456789012:role/gardenio-prod-microvm-controller",
			Region:            "us-east-1",
		},
		Backend: BackendConfig{
			Region:                  "us-east-1",
			ImageIdentifier:         "arn:aws:lambda:us-east-1:123456789012:microvm-image:gardenio-prod-microvm-agent",
			ImageVersion:            "1.0",
			ExecutionRoleARN:        "arn:aws:iam::123456789012:role/gardenio-prod-microvm-agent-execution",
			SecretPrefix:            "ziti-microvm-agents/",
			ExpectedLauncherAccount: "123456789012",
			LogGroup:                "/aws/lambda/microvms/gardenio-prod-microvm-agent",
		},
	}
}

// TestValidateResourceConsistencySameAccount: the fully-migrated same-account
// Gardenio config (S3 target) is internally consistent and passes.
func TestValidateResourceConsistencySameAccount(t *testing.T) {
	if err := gardenioAccountConfig().validateResourceConsistency(); err != nil {
		t.Fatalf("same-account Gardenio config: unexpected inconsistency %v", err)
	}
}

// TestValidateResourceConsistencyPOCCrossAccount: the cross-account POC/rollback
// topology — controller role in the Gardenio account (133), launcher + image +
// execution role in the Network Fabric account (751) — is consistent: the
// controller ORIGIN role is deliberately not part of the launcher-side account
// family, so a legitimately cross-account launcher hop is NOT rejected.
func TestValidateResourceConsistencyPOCCrossAccount(t *testing.T) {
	sc := StartupConfig{
		Identity: Options{
			LauncherRoleARN:   testLauncher, // 751
			ControllerRoleARN: "arn:aws:iam::123456789012:role/gardenio-prod-microvm-controller",
			Region:            "us-east-2",
		},
		Backend: func() BackendConfig {
			c := testBackendConfig() // image + exec in 751, us-east-2
			c.ExpectedLauncherAccount = "345678901234"
			return c
		}(),
	}
	if err := sc.validateResourceConsistency(); err != nil {
		t.Fatalf("cross-account POC topology should be consistent: %v", err)
	}
}

// TestValidateResourceConsistencyHalfMigrated: the exact failure S3 guards —
// launcher role flipped to the Gardenio account but the image/execution ARNs (or
// the pin) left pointing at Network Fabric. Each half-flip must be refused,
// naming the offending field.
func TestValidateResourceConsistencyHalfMigrated(t *testing.T) {
	cases := []struct {
		name     string
		mutate   func(*StartupConfig)
		wantWord string
	}{
		{
			name: "image left in NF account",
			mutate: func(sc *StartupConfig) {
				sc.Backend.ImageIdentifier = "arn:aws:lambda:us-east-1:345678901234:microvm-image:ziti-microvm-poc"
			},
			wantWord: "ImageIdentifier account 345678901234",
		},
		{
			name: "execution role left in NF account",
			mutate: func(sc *StartupConfig) {
				sc.Backend.ExecutionRoleARN = "arn:aws:iam::345678901234:role/ziti-microvm-poc-execution"
			},
			wantWord: "ExecutionRoleARN account 345678901234",
		},
		{
			name: "launcher role left in NF account",
			mutate: func(sc *StartupConfig) {
				sc.Identity.LauncherRoleARN = testLauncher // 751
			},
			wantWord: "LauncherRoleARN account 345678901234",
		},
		{
			name: "pin left in NF account",
			mutate: func(sc *StartupConfig) {
				sc.Backend.ExpectedLauncherAccount = "345678901234"
			},
			wantWord: "does not match ExpectedLauncherAccount account 345678901234",
		},
		{
			name: "image region disagrees with launch region",
			mutate: func(sc *StartupConfig) {
				sc.Backend.ImageIdentifier = "arn:aws:lambda:us-east-2:123456789012:microvm-image:gardenio-prod-microvm-agent"
			},
			wantWord: "ImageIdentifier region us-east-2 does not match launch region us-east-1",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			sc := gardenioAccountConfig()
			tc.mutate(&sc)
			err := sc.validateResourceConsistency()
			if err == nil {
				t.Fatal("want inconsistency error, got nil")
			}
			if !strings.Contains(err.Error(), tc.wantWord) {
				t.Errorf("error %q does not name %q", err, tc.wantWord)
			}
		})
	}
}

// TestValidateResourceConsistencyEmptyARNsSkipped: unset launcher-side ARNs are
// this check's non-concern (validate() enforces required-field completeness), so
// a partial config with the pin set but the image unset is not flagged as an
// account mismatch here.
func TestValidateResourceConsistencyEmptyARNsSkipped(t *testing.T) {
	sc := gardenioAccountConfig()
	sc.Backend.ImageIdentifier = ""
	sc.Backend.ExecutionRoleARN = ""
	if err := sc.validateResourceConsistency(); err != nil {
		t.Fatalf("empty launcher-side ARNs should be skipped, not flagged: %v", err)
	}
}

// TestValidateResourceConsistencyNoPinInfersFamily: with no explicit pin, the
// account family is inferred from the first populated ARN, so a mismatch between
// two populated ARNs is still caught (the pin only seeds the expected value).
func TestValidateResourceConsistencyNoPinInfersFamily(t *testing.T) {
	sc := gardenioAccountConfig()
	sc.Backend.ExpectedLauncherAccount = ""
	sc.Backend.ImageIdentifier = "arn:aws:lambda:us-east-1:345678901234:microvm-image:ziti-microvm-poc"
	err := sc.validateResourceConsistency()
	if err == nil {
		t.Fatal("want inconsistency between launcher (133) and image (751), got nil")
	}
	if !strings.Contains(err.Error(), "345678901234") || !strings.Contains(err.Error(), "123456789012") {
		t.Errorf("error %q should name both the launcher and image accounts", err)
	}
}

// TestValidateResourceConsistencyUnparseableSkipped: a non-ARN image identifier
// is left to the launcher IAM boundary (surfaces as AccessDenied at RunMicrovm),
// not flagged as an account mismatch here — this check only rejects a well-formed
// ARN pointing at the wrong account.
func TestValidateResourceConsistencyUnparseableSkipped(t *testing.T) {
	sc := gardenioAccountConfig()
	sc.Backend.ImageIdentifier = "gardenio-prod-microvm-agent" // bare name, not an ARN
	if err := sc.validateResourceConsistency(); err != nil {
		t.Fatalf("unparseable ARN should be skipped by the consistency check: %v", err)
	}
}

// TestStartupBackendRefusesHalfMigratedConfig: the static consistency check runs
// in the startup path BEFORE any AWS work — a half-migrated config refuses the
// backend without building an identity or calling STS.
func TestStartupBackendRefusesHalfMigratedConfig(t *testing.T) {
	sc := gardenioAccountConfig()
	// Launcher flipped to Gardenio (133) but the image still points at NF (751).
	sc.Backend.ImageIdentifier = "arn:aws:lambda:us-east-1:345678901234:microvm-image:ziti-microvm-poc"
	// Broker + production mode ON to prove the consistency check is independent
	// of them (it fails closed regardless).
	sc.Backend.ProductionMode = true
	sc.Broker = BrokerOptions{URL: "http://ziti-access-broker.fabric:8080"}

	identityBuilt := false
	backend, err := startupBackend(context.Background(), discardLogger(), sc,
		func(ctx context.Context, o Options) (startupIdentity, error) {
			identityBuilt = true
			return fakeStartupIdentity{caller: okCaller(), region: "us-east-1"}, nil
		}, prodIssuerFactory)
	if err == nil {
		t.Fatal("want half-migrated refusal, got nil error")
	}
	if backend != nil {
		t.Errorf("backend = %+v, want nil (fail closed)", backend)
	}
	if !strings.Contains(err.Error(), "inconsistent") {
		t.Errorf("error %q should name the inconsistency", err)
	}
	if identityBuilt {
		t.Error("identity was built despite an inconsistent config; the static check must run before any AWS work")
	}
}
