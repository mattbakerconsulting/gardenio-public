package tests

import (
	"strings"
	"testing"
)

// LiteLLM in-cluster Bedrock IMAGE-GENERATION model (ka-0000008102, plan
// ka-0000005980 Phase 5 — optional raster plan-badges). The in-cluster proxy
// mounts /v1/images/generations but ships NO image model by default; these
// tests pin the purely-additive, default-off image-model wiring that lets a
// POST /v1/images/generations resolve in-cluster once an operator enables it
// with a Bedrock credential path + an AWS model-access grant — without changing
// the default chat-only render. It reuses the SINGLE embedding.* Bedrock
// credential strategy (one pod, one SA/role) exactly like the Titan embedder
// and the bedrock-chat groups.

//  1. Image generation off by default: no image model renders, and the default
//     render is unchanged (no credential guard tripped).
func TestLitellmImageGeneration_DisabledByDefault(t *testing.T) {
	t.Parallel()
	out := helmTemplate(t, "templates/litellm/configmap.yaml",
		"litellm.enabled=true",
	)
	if strings.Contains(out, "image_generation") ||
		strings.Contains(out, "nova-canvas") ||
		strings.Contains(out, "bedrock-image") {
		t.Error("image-generation model should NOT render when litellm.imageGeneration is disabled")
	}
}

//  2. IRSA strategy: appends the image model with mode:image_generation, reusing
//     the shared embedding.* IRSA credential path (dedicated SA + token mount).
func TestLitellmImageGeneration_IRSAStrategy(t *testing.T) {
	t.Parallel()
	const roleARN = "arn:aws:iam::123456789012:role/gardenio-litellm-bedrock"
	cfg := helmTemplate(t, "templates/litellm/configmap.yaml",
		"litellm.enabled=true",
		"litellm.imageGeneration.enabled=true",
		"litellm.embedding.irsaRoleArn="+roleARN,
	)
	for _, want := range []string{
		"bedrock-image",
		"model: bedrock/amazon.nova-canvas-v1:0",
		`aws_region_name: "us-east-1"`,
		"mode: image_generation",
	} {
		if !strings.Contains(cfg, want) {
			t.Errorf("configmap should contain %q under IRSA image-generation strategy", want)
		}
	}

	// Image generation rides the shared embedding.* credential path via
	// needsBedrockCreds — so the dedicated SA carries the role-arn and the pod
	// mounts its web-identity token, identical to the embedder/bedrock-chat.
	sa := helmTemplate(t, "templates/litellm/serviceaccount.yaml",
		"litellm.enabled=true",
		"litellm.imageGeneration.enabled=true",
		"litellm.embedding.irsaRoleArn="+roleARN,
	)
	if !strings.Contains(sa, "eks.amazonaws.com/role-arn: \""+roleARN+"\"") {
		t.Errorf("litellm SA must carry the role-arn annotation when image generation rides IRSA; got:\n%s", sa)
	}
	dep := helmTemplate(t, "templates/litellm/deployment.yaml",
		"litellm.enabled=true",
		"litellm.imageGeneration.enabled=true",
		"litellm.embedding.irsaRoleArn="+roleARN,
	)
	if !strings.Contains(dep, "automountServiceAccountToken: true") {
		t.Error("automountServiceAccountToken must be true when image generation rides the IRSA strategy")
	}
}

//  3. Static-credential strategy: appends the image model and mounts AWS keys
//     from a Secret; no dedicated SA role-arn, automount stays off.
func TestLitellmImageGeneration_StaticCredsStrategy(t *testing.T) {
	t.Parallel()
	cfg := helmTemplate(t, "templates/litellm/configmap.yaml",
		"litellm.enabled=true",
		"litellm.imageGeneration.enabled=true",
		"litellm.embedding.staticCredsSecret=litellm-bedrock-aws",
	)
	if !strings.Contains(cfg, "mode: image_generation") {
		t.Error("image-generation model should render under the static-creds strategy")
	}
	dep := helmTemplate(t, "templates/litellm/deployment.yaml",
		"litellm.enabled=true",
		"litellm.imageGeneration.enabled=true",
		"litellm.embedding.staticCredsSecret=litellm-bedrock-aws",
	)
	if !strings.Contains(dep, "name: AWS_ACCESS_KEY_ID") {
		t.Error("deployment should mount AWS keys under the static-creds strategy for image generation")
	}
}

//  4. Overridable model id + region: an operator can point the routed group at a
//     different Bedrock image model / region the grant covers.
func TestLitellmImageGeneration_OverrideModelAndRegion(t *testing.T) {
	t.Parallel()
	cfg := helmTemplate(t, "templates/litellm/configmap.yaml",
		"litellm.enabled=true",
		"litellm.imageGeneration.enabled=true",
		"litellm.embedding.staticCredsSecret=litellm-bedrock-aws",
		"litellm.imageGeneration.modelName=plan-badge-image",
		"litellm.imageGeneration.bedrockModel=amazon.titan-image-generator-v2:0",
		"litellm.imageGeneration.region=us-west-2",
	)
	for _, want := range []string{
		"plan-badge-image",
		"model: bedrock/amazon.titan-image-generator-v2:0",
		`aws_region_name: "us-west-2"`,
	} {
		if !strings.Contains(cfg, want) {
			t.Errorf("configmap should honor the overridden %q", want)
		}
	}
}

//  5. Credential guard: image generation enabled with NO credential strategy
//     must fail the render so a misconfigured overlay never reaches a cluster —
//     the InvokeModel call would 5xx at the Bedrock auth boundary otherwise.
func TestLitellmImageGeneration_GuardNoStrategyFails(t *testing.T) {
	t.Parallel()
	_, err := helmTemplateAllowFail(t, "",
		"litellm.enabled=true",
		"litellm.imageGeneration.enabled=true",
	)
	if err == nil {
		t.Error("expected render to fail when image generation is enabled with NO credential strategy")
	}
}

// 6. Guard skipped when image generation is off (default chat-only render).
func TestLitellmImageGeneration_GuardSkippedWhenDisabled(t *testing.T) {
	t.Parallel()
	if _, err := helmTemplateAllowFail(t, "",
		"litellm.enabled=true",
	); err != nil {
		t.Fatalf("render must succeed when image generation is disabled; err: %v", err)
	}
}
