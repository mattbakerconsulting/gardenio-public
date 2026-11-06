package tests

import (
	"strings"
	"testing"
)

// Production configuration & rollout (ka-0000006687): the chart must fail the
// render — not silently produce a POC-shaped deployment — whenever
// agents.microvm.productionMode=true and any POC assumption survives. These
// mirror microvm.StartupConfig.validateProductionMode so a bad overlay dies at
// helm template time instead of at controller boot.

// microvmProdFlags is a fully production-clean flag set: broker on, no static
// identity, dedicated controller role, pinned launcher account + image
// version. Tests knock out one flag at a time.
func microvmProdFlags(extra ...string) []string {
	base := microvmBaseFlags(
		"agents.microvm.productionMode=true",
		"agents.microvm.imageVersion=8.0",
		"agents.microvm.controllerRoleARN=arn:aws:iam::123456789012:role/gardenio-prod-microvm-controller",
		"agents.microvm.expectedLauncherAccount=345678901234",
		"agents.microvm.zitiBroker.url=http://ziti-access-broker.fabric:8080",
		"agents.microvm.logGroup=/aws/lambda/microvms/ziti-microvm-poc",
	)
	return append(base, extra...)
}

func TestMicrovmProductionModeCleanRenders(t *testing.T) {
	t.Parallel()
	out, err := helmTemplateAllowFail(t, "templates/controller/deployment.yaml", microvmProdFlags()...)
	if err != nil {
		t.Fatalf("production-clean render failed: %v\n%s", err, out)
	}
	for _, env := range []string{
		"MICROVM_PRODUCTION_MODE",
		"MICROVM_CONTROLLER_ROLE_ARN",
		"MICROVM_EXPECTED_LAUNCHER_ACCOUNT",
		"MICROVM_ZITI_BROKER_URL",
	} {
		if !strings.Contains(out, env) {
			t.Errorf("expected %s to render in production mode", env)
		}
	}
	if strings.Contains(out, "MICROVM_STATIC_IDENTITY_SECRET_ARN") {
		t.Error("MICROVM_STATIC_IDENTITY_SECRET_ARN must not render in production mode")
	}
}

func TestMicrovmProductionModeFailClosed(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name     string
		flags    []string
		wantWord string
	}{
		{
			name: "static identity still set",
			flags: microvmProdFlags(
				"agents.microvm.staticIdentitySecretARN=arn:aws:secretsmanager:us-east-2:345678901234:secret:ziti-microvm-poc/ziti-identity-kCfNSS",
			),
			wantWord: "staticIdentitySecretARN",
		},
		{
			name:     "broker unset",
			flags:    microvmProdFlags("agents.microvm.zitiBroker.url="),
			wantWord: "zitiBroker.url",
		},
		{
			name:     "controller role unset",
			flags:    microvmProdFlags("agents.microvm.controllerRoleARN="),
			wantWord: "controllerRoleARN",
		},
		{
			name:     "launcher account unpinned",
			flags:    microvmProdFlags("agents.microvm.expectedLauncherAccount="),
			wantWord: "expectedLauncherAccount",
		},
		{
			name:     "image version floating",
			flags:    microvmProdFlags("agents.microvm.imageVersion="),
			wantWord: "imageVersion",
		},
		{
			name:     "log group unset",
			flags:    microvmProdFlags("agents.microvm.logGroup="),
			wantWord: "logGroup",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			out, err := helmTemplateAllowFail(t, "templates/controller/deployment.yaml", tc.flags...)
			if err == nil {
				t.Fatalf("expected render to fail closed, got success")
			}
			if !strings.Contains(out, tc.wantWord) {
				t.Errorf("failure output does not name %q:\n%s", tc.wantWord, out)
			}
		})
	}
}

// Production mode off (default): the POC combination keeps rendering — the
// config-gated rollback path must stay available until the E2E gate
// (ka-0000000183) retires it.
func TestMicrovmProductionModeOffKeepsPOCRender(t *testing.T) {
	t.Parallel()
	out, err := helmTemplateAllowFail(t, "templates/controller/deployment.yaml",
		microvmBaseFlags(
			"agents.microvm.staticIdentitySecretARN=arn:aws:secretsmanager:us-east-2:345678901234:secret:ziti-microvm-poc/ziti-identity-kCfNSS",
		)...)
	if err != nil {
		t.Fatalf("POC render with production mode off failed: %v\n%s", err, out)
	}
	if !strings.Contains(out, "MICROVM_STATIC_IDENTITY_SECRET_ARN") {
		t.Error("expected MICROVM_STATIC_IDENTITY_SECRET_ARN to render with production mode off")
	}
	if strings.Contains(out, "MICROVM_PRODUCTION_MODE") {
		t.Error("MICROVM_PRODUCTION_MODE must not render when productionMode=false")
	}
}

// controllerRoleARN + expectedLauncherAccount render independently of
// production mode (staged rollout: pin the account / migrate the role first,
// flip productionMode last).
func TestMicrovmControllerRoleAndAccountPinRenderWithoutProductionMode(t *testing.T) {
	t.Parallel()
	out := helmTemplate(t, "templates/controller/deployment.yaml",
		microvmBaseFlags(
			"agents.microvm.controllerRoleARN=arn:aws:iam::123456789012:role/gardenio-prod-microvm-controller",
			"agents.microvm.expectedLauncherAccount=345678901234",
		)...)
	if !strings.Contains(out, "MICROVM_CONTROLLER_ROLE_ARN") {
		t.Error("expected MICROVM_CONTROLLER_ROLE_ARN to render when controllerRoleARN is set")
	}
	if !strings.Contains(out, "MICROVM_EXPECTED_LAUNCHER_ACCOUNT") {
		t.Error("expected MICROVM_EXPECTED_LAUNCHER_ACCOUNT to render when expectedLauncherAccount is set")
	}
	if strings.Contains(out, "MICROVM_PRODUCTION_MODE") {
		t.Error("MICROVM_PRODUCTION_MODE must not render when productionMode=false")
	}
}
