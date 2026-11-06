package tests

import (
	"strings"
	"testing"
)

// LiteLLM in-cluster Bedrock Titan embedding model (ka-0000007591, epic
// ka-0000000560). The in-cluster proxy ships chat-only by default; these tests
// pin the embedding-model wiring that lets kbeads semantic-search embeddings
// resolve in-cluster (mirroring the external devops litellm) without breaking
// the default chat-only render.

//  1. Embedding off by default: no Titan model, no dedicated SA, the proxy pod
//     keeps automountServiceAccountToken:false.
func TestLitellmEmbedding_DisabledByDefault(t *testing.T) {
	t.Parallel()
	out := helmTemplate(t, "templates/litellm/configmap.yaml",
		"litellm.enabled=true",
	)
	if strings.Contains(out, "bedrock-titan-embed-v2") || strings.Contains(out, "amazon.titan-embed-text-v2") {
		t.Error("Titan embedding model should NOT render when litellm.embedding is disabled")
	}
}

func TestLitellmEmbedding_DisabledNoServiceAccount(t *testing.T) {
	t.Parallel()
	// Unconditional-sa (ka-0000000992): the SA object is ALWAYS rendered when
	// litellm is enabled, decoupled from the IRSA gate — so the deployment's
	// serviceAccountName reference can never point at a missing SA
	// (ka-0000006740). With embedding/IRSA off the SA exists but carries NO
	// role-arn annotation (an inert default-style SA).
	out := helmTemplate(t, "templates/litellm/serviceaccount.yaml",
		"litellm.enabled=true",
	)
	if !strings.Contains(out, "kind: ServiceAccount") {
		t.Error("litellm ServiceAccount should always render when litellm is enabled (unconditional-sa ka-0000000992)")
	}
	if strings.Contains(out, "eks.amazonaws.com/role-arn") {
		t.Error("litellm SA must NOT carry an IRSA role-arn annotation when embedding/IRSA is off")
	}
}

//  2. IRSA strategy: appends the Titan model, creates a dedicated SA carrying
//     the role-arn, and flips automountServiceAccountToken on for the proxy.
func TestLitellmEmbedding_IRSAStrategy(t *testing.T) {
	t.Parallel()
	const roleARN = "arn:aws:iam::234567890123:role/gardenio-litellm-bedrock"
	cfg := helmTemplate(t, "templates/litellm/configmap.yaml",
		"litellm.enabled=true",
		"litellm.embedding.enabled=true",
		"litellm.embedding.irsaRoleArn="+roleARN,
	)
	for _, want := range []string{
		"bedrock-titan-embed-v2",
		"model: bedrock/amazon.titan-embed-text-v2:0",
		`aws_region_name: "us-east-1"`,
		"mode: embedding",
	} {
		if !strings.Contains(cfg, want) {
			t.Errorf("configmap should contain %q under IRSA embedding strategy", want)
		}
	}

	sa := helmTemplate(t, "templates/litellm/serviceaccount.yaml",
		"litellm.enabled=true",
		"litellm.embedding.enabled=true",
		"litellm.embedding.irsaRoleArn="+roleARN,
	)
	if !strings.Contains(sa, "kind: ServiceAccount") {
		t.Error("dedicated litellm ServiceAccount should render under IRSA strategy")
	}
	if !strings.Contains(sa, "eks.amazonaws.com/role-arn: \""+roleARN+"\"") {
		t.Errorf("litellm SA must carry the configured role-arn annotation; got:\n%s", sa)
	}

	dep := helmTemplate(t, "templates/litellm/deployment.yaml",
		"litellm.enabled=true",
		"litellm.embedding.enabled=true",
		"litellm.embedding.irsaRoleArn="+roleARN,
	)
	if !strings.Contains(dep, "serviceAccountName: test-litellm") {
		t.Error("proxy pod should ride the dedicated litellm SA under IRSA strategy")
	}
	if !strings.Contains(dep, "automountServiceAccountToken: true") {
		t.Error("automountServiceAccountToken must be true under IRSA strategy so the web-identity token is injected")
	}
	if strings.Contains(dep, "AWS_ACCESS_KEY_ID") {
		t.Error("static AWS key env vars must NOT render under the IRSA strategy")
	}
}

//  3. Static-credential strategy: appends the Titan model and mounts AWS keys
//     from a Secret; no dedicated SA, automount stays off.
func TestLitellmEmbedding_StaticCredsStrategy(t *testing.T) {
	t.Parallel()
	dep := helmTemplate(t, "templates/litellm/deployment.yaml",
		"litellm.enabled=true",
		"litellm.embedding.enabled=true",
		"litellm.embedding.staticCredsSecret=litellm-bedrock-aws",
	)
	for _, want := range []string{
		"name: AWS_ACCESS_KEY_ID",
		"name: AWS_SECRET_ACCESS_KEY",
		"name: litellm-bedrock-aws",
		"key: AWS_ACCESS_KEY_ID",
		"key: AWS_SECRET_ACCESS_KEY",
	} {
		if !strings.Contains(dep, want) {
			t.Errorf("deployment should contain %q under static-creds strategy", want)
		}
	}
	if strings.Contains(dep, "AWS_SESSION_TOKEN") {
		t.Error("AWS_SESSION_TOKEN must NOT render unless sessionTokenKey is set")
	}
	// Unconditional-sa (ka-0000000992): the pod always names its SA (the SA
	// object always renders), but under static-creds it does NOT mount the
	// token — there is no IRSA role to assume.
	if !strings.Contains(dep, "serviceAccountName: test-litellm") {
		t.Error("pod should always name the litellm SA (unconditional-sa ka-0000000992)")
	}
	if !strings.Contains(dep, "automountServiceAccountToken: false") {
		t.Error("automountServiceAccountToken should stay false under static-creds strategy")
	}
}

func TestLitellmEmbedding_StaticCredsSessionToken(t *testing.T) {
	t.Parallel()
	dep := helmTemplate(t, "templates/litellm/deployment.yaml",
		"litellm.enabled=true",
		"litellm.embedding.enabled=true",
		"litellm.embedding.staticCredsSecret=litellm-bedrock-aws",
		"litellm.embedding.sessionTokenKey=AWS_SESSION_TOKEN",
	)
	if !strings.Contains(dep, "name: AWS_SESSION_TOKEN") {
		t.Error("AWS_SESSION_TOKEN env should render when sessionTokenKey is set")
	}
}

//  4. Credential guard: embedding on with both strategies, or neither, must
//     fail the render so a misconfigured overlay never reaches a cluster.
func TestLitellmEmbedding_GuardBothStrategiesFails(t *testing.T) {
	t.Parallel()
	_, err := helmTemplateAllowFail(t, "",
		"litellm.enabled=true",
		"litellm.embedding.enabled=true",
		"litellm.embedding.irsaRoleArn=arn:aws:iam::1:role/x",
		"litellm.embedding.staticCredsSecret=s",
	)
	if err == nil {
		t.Error("expected render to fail when BOTH embedding credential strategies are set")
	}
}

func TestLitellmEmbedding_GuardNoStrategyFails(t *testing.T) {
	t.Parallel()
	_, err := helmTemplateAllowFail(t, "",
		"litellm.enabled=true",
		"litellm.embedding.enabled=true",
	)
	if err == nil {
		t.Error("expected render to fail when embedding is enabled with NO credential strategy")
	}
}

// 5. Embedding off must not trip the credential guard.
func TestLitellmEmbedding_GuardSkippedWhenDisabled(t *testing.T) {
	t.Parallel()
	if _, err := helmTemplateAllowFail(t, "",
		"litellm.enabled=true",
	); err != nil {
		t.Fatalf("render must succeed when embedding is disabled; err: %v", err)
	}
}

// 6. Regression for ka-0000006740 (unconditional-sa decision ka-0000000992).
// Previously the SA object (serviceaccount.yaml) and the proxy pod's
// serviceAccountName reference (deployment.yaml) each gated on the IRSA
// expression; a render-time divergence left the deployment referencing
// gardenio-litellm while no SA object rendered → ReplicaFailure/FailedCreate
// "serviceaccount gardenio-litellm not found" and red deploy-prod for 3
// consecutive goldens.
//
// The chosen durable fix decouples the two: the SA object is rendered
// UNCONDITIONALLY whenever litellm is enabled, so the deployment's
// serviceAccountName reference can never point at a missing SA — by
// construction, not by a shared boolean. The IRSA strategy only governs whether
// that SA carries the role-arn annotation and whether the pod mounts its token.
//
// This asserts the strengthened invariant across every strategy permutation:
//   - SA object ALWAYS present, and serviceAccountName ref ALWAYS present;
//   - role-arn annotation + automount:true present IFF the IRSA strategy is on.
func TestLitellmEmbedding_SAGateCannotDiverge(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name     string
		flags    []string
		wantIRSA bool // role-arn annotation + automountServiceAccountToken:true
	}{
		{
			name:     "embedding IRSA strategy → SA annotated + token mounted",
			flags:    []string{"litellm.enabled=true", "litellm.embedding.enabled=true", "litellm.embedding.irsaRoleArn=arn:aws:iam::1:role/x"},
			wantIRSA: true,
		},
		{
			// Stage-3 (ka-0000002573): bedrock-chat also rides the IRSA role
			// via needsBedrockCreds. The IRSA role-arn lives under embedding.*
			// (single shared credential strategy for the one pod).
			name:     "bedrock-chat IRSA strategy → SA annotated + token mounted",
			flags:    []string{"litellm.enabled=true", "litellm.bedrock.enabled=true", "litellm.embedding.irsaRoleArn=arn:aws:iam::1:role/x"},
			wantIRSA: true,
		},
		{
			name:     "static-creds strategy → SA present but inert (no annotation, no token)",
			flags:    []string{"litellm.enabled=true", "litellm.embedding.enabled=true", "litellm.embedding.staticCredsSecret=s"},
			wantIRSA: false,
		},
		{
			name:     "embedding+bedrock off → SA present but inert (no annotation, no token)",
			flags:    []string{"litellm.enabled=true"},
			wantIRSA: false,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			sa := helmTemplate(t, "templates/litellm/serviceaccount.yaml", tc.flags...)
			dep := helmTemplate(t, "templates/litellm/deployment.yaml", tc.flags...)

			// Core invariant: the SA object and the serviceAccountName reference
			// are BOTH always present, so they can never diverge (ka-0000006740).
			if !strings.Contains(sa, "kind: ServiceAccount") {
				t.Fatalf("SA object MUST always render when litellm is enabled (unconditional-sa ka-0000000992); got:\n%s", sa)
			}
			if !strings.Contains(dep, "serviceAccountName: test-litellm") {
				t.Fatalf("deployment MUST always name the litellm SA (unconditional-sa ka-0000000992); got:\n%s", dep)
			}

			// IRSA-conditional bits: role-arn annotation and token mount must
			// track the IRSA strategy together.
			haveAnnotation := strings.Contains(sa, "eks.amazonaws.com/role-arn")
			haveMount := strings.Contains(dep, "automountServiceAccountToken: true")
			if haveAnnotation != tc.wantIRSA {
				t.Errorf("role-arn annotation present = %v, want %v", haveAnnotation, tc.wantIRSA)
			}
			if haveMount != tc.wantIRSA {
				t.Errorf("automountServiceAccountToken:true present = %v, want %v", haveMount, tc.wantIRSA)
			}
		})
	}
}
