// Package microvm holds the controller's cross-account AWS identity for the
// lambda_microvm execution backend (epic ka-0000005010, Change C3; production
// hardening ka-0000006687).
//
// The controller drives AWS Lambda MicroVMs (RunMicrovm / GetMicrovm /
// TerminateMicrovm) and writes per-run Secrets Manager values in the
// Fabric-owned launcher account. It reaches that account by assuming a
// Fabric-owned launcher role. Two credential chains exist:
//
// Production (Options.ControllerRoleARN set — required in production mode):
//
//	pod IRSA web-identity token (controller SA only)
//	  --AssumeRoleWithWebIdentity-->  gardenio-prod-microvm-controller  (own acct)
//	  --AssumeRole-->                 Fabric launcher role             (Fabric acct)
//
// The dedicated controller role's OIDC trust pins EXACTLY the controller
// ServiceAccount subject, so no ordinary agent pod can reach the launcher —
// closing the shared-identity boundary the POC accepted.
//
// POC/rollback (ControllerRoleARN empty): the controller rides its default
// credential chain — the shared gardenio-prod-agent IRSA identity the launcher
// trust temporarily accepts (network-fabric-deployment!15 deferred item) —
// and assumes the launcher directly. Any pod on the shared role could, in
// principle, assume the launcher in this mode; production mode refuses it.
//
// This package builds ONLY the credential/client plumbing plus a
// GetCallerIdentity verification affordance; the RunMicrovm/secret-write logic
// lands with the backend in C4 (ka-0000008160), which consumes the Lambda +
// SecretsManager clients returned here.
package microvm

import (
	"context"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/credentials/stscreds"
	"github.com/aws/aws-sdk-go-v2/service/lambdamicrovms"
	"github.com/aws/aws-sdk-go-v2/service/secretsmanager"
	"github.com/aws/aws-sdk-go-v2/service/sts"
)

// configLoadTimeout caps AWS credential resolution so a broken IRSA / STS
// environment doesn't deadlock the controller at Identity construction. Mirrors
// the beads-viewer imageProxyAWSConfigTimeout / gb awsConfigTimeout discipline:
// long enough for STS to issue web-identity creds on a healthy pod, short
// enough that a misconfig surfaces as an error rather than a stuck process.
const configLoadTimeout = 15 * time.Second

// Options configures the MicroVM controller identity. All fields are sourced
// from config.Config (env vars set by the Helm chart).
type Options struct {
	// LauncherRoleARN is the Fabric-owned cross-account role the controller
	// assumes to drive MicroVMs + per-run Secrets Manager (config
	// MicrovmLauncherRoleARN). Required.
	LauncherRoleARN string

	// Region is the AWS region the Lambda + Secrets Manager clients target
	// after the assume-role hop (config MicrovmRegion), e.g. "us-east-2".
	// Required — deliberately independent of the controller's own region.
	Region string

	// SessionName is the STS session name stamped on the assume-role call
	// (config MicrovmAssumeRoleSessionName). Surfaces in the launcher account's
	// CloudTrail as the caller identity. Empty falls back to a stable default.
	SessionName string

	// ControllerRoleARN is the dedicated controller-only IRSA role the launcher
	// hop originates from (config MicrovmControllerRoleARN, e.g.
	// arn:aws:iam::<acct>:role/gardenio-prod-microvm-controller). When set, the
	// controller assumes THIS role first via AssumeRoleWithWebIdentity using the
	// pod's projected ServiceAccount token — the role's OIDC trust pins exactly
	// the controller SA subject, so no other pod can originate the launcher hop
	// — and then assumes the launcher from it. Empty keeps the POC/rollback
	// chain: the controller's default credential chain (the shared agent IRSA
	// identity) assumes the launcher directly. Required in production mode
	// (BackendConfig.ProductionMode).
	ControllerRoleARN string
}

const defaultSessionName = "gardenio-microvm-controller"

// Enabled reports whether the MicroVM backend has enough config to construct an
// Identity. Both the launcher role and region must be set; the credential
// factory is never built otherwise, so the backend is a hard no-op unless an
// operator wires both (default-off — the chart leaves them empty until the C3
// terraform apply pins the role ARN).
func (o Options) Enabled() bool {
	return strings.TrimSpace(o.LauncherRoleARN) != "" && strings.TrimSpace(o.Region) != ""
}

// Identity holds the cross-account credentials + region for the MicroVM
// backend and hands out the AWS service clients the C4 backend needs. It is
// constructed once at controller startup (when enabled) and shared; the
// underlying stscreds provider caches and auto-refreshes the assumed-role
// credentials, so callers do not manage credential lifetime.
type Identity struct {
	// cfg is the fully-resolved AWS config: the controller's own web-identity
	// (IRSA) credentials wrapped by an stscreds assume-role provider targeting
	// the launcher role, pinned to the MicroVM region.
	cfg aws.Config

	// originCreds are the launcher hop's ORIGIN credentials — the dedicated
	// controller role (ControllerRoleARN set) or the default chain (POC/
	// rollback). This is the identity the broker's consumer policy pins, so
	// the ziti-access-broker proof must be signed with THESE credentials,
	// never cfg's assumed launcher-role credentials and never a separately
	// resolved default chain (which would attest the shared agent role even
	// when the dedicated role is configured).
	originCreds aws.CredentialsProvider

	launcherRoleARN   string
	controllerRoleARN string
	region            string
}

// webIdentityTokenFileEnv is the standard env var IRSA sets on every pod whose
// ServiceAccount carries an eks.amazonaws.com/role-arn annotation — the path of
// the projected OIDC token the dedicated-controller-role hop exchanges via
// AssumeRoleWithWebIdentity.
const webIdentityTokenFileEnv = "AWS_WEB_IDENTITY_TOKEN_FILE"

// NewIdentity builds the cross-account credential chain:
//
//  1. Resolve the controller's originating credentials. With ControllerRoleARN
//     set (production), a web-identity provider exchanges the pod's projected
//     SA token (AWS_WEB_IDENTITY_TOKEN_FILE) for the dedicated
//     controller-only role — the identity the launcher's production trust
//     policy pins. Without it (POC/rollback), LoadDefaultConfig resolves the
//     default chain — the shared agent IRSA role the launcher trust
//     temporarily accepts.
//  2. An stscreds assume-role provider layers on top, exchanging those
//     credentials for launcher-role credentials in the Fabric account. The
//     provider caches + refreshes automatically.
//  3. The region is pinned to the MicroVM region (us-east-2), independent of the
//     controller's own region, since the launcher + image live there.
//
// It fails fast (bounded by configLoadTimeout) so a broken IRSA/STS setup
// surfaces at startup rather than on the first MicroVM launch. Returns an error
// — never a partial Identity — when the config cannot be resolved or the
// options are incomplete.
func NewIdentity(ctx context.Context, opts Options) (*Identity, error) {
	if !opts.Enabled() {
		return nil, fmt.Errorf("microvm: identity requires both LauncherRoleARN and Region (got role=%q region=%q)", opts.LauncherRoleARN, opts.Region)
	}

	sessionName := strings.TrimSpace(opts.SessionName)
	if sessionName == "" {
		sessionName = defaultSessionName
	}
	region := strings.TrimSpace(opts.Region)
	launcherRoleARN := strings.TrimSpace(opts.LauncherRoleARN)
	controllerRoleARN := strings.TrimSpace(opts.ControllerRoleARN)

	loadCtx, cancel := context.WithTimeout(ctx, configLoadTimeout)
	defer cancel()

	// Base config: the controller's own identity (IRSA web-identity in prod),
	// pinned to the MicroVM region so the STS assume-role calls and every
	// service client target it.
	base, err := awsconfig.LoadDefaultConfig(loadCtx, awsconfig.WithRegion(region))
	if err != nil {
		return nil, fmt.Errorf("microvm: load base AWS config: %w", err)
	}

	// Launcher-hop origin: the dedicated controller role when configured
	// (production), else the base default-chain credentials (POC/rollback).
	originCreds := base.Credentials
	if controllerRoleARN != "" {
		tokenFile := strings.TrimSpace(os.Getenv(webIdentityTokenFileEnv))
		if tokenFile == "" {
			return nil, fmt.Errorf("microvm: MicrovmControllerRoleARN is set but %s is empty — the dedicated controller role requires the pod's projected IRSA token", webIdentityTokenFileEnv)
		}
		// An anonymous STS client: AssumeRoleWithWebIdentity is unsigned (the
		// OIDC token IS the credential), so this hop must not depend on — and
		// must not be attributable to — the default-chain (shared) identity.
		anonSTS := sts.NewFromConfig(base, func(o *sts.Options) {
			o.Credentials = aws.AnonymousCredentials{}
		})
		webIdentity := stscreds.NewWebIdentityRoleProvider(anonSTS, controllerRoleARN,
			stscreds.IdentityTokenFile(tokenFile),
			func(o *stscreds.WebIdentityRoleOptions) {
				o.RoleSessionName = sessionName
			})
		originCreds = aws.NewCredentialsCache(webIdentity)
	}

	// Assume-role provider: exchange the origin creds for launcher-role creds
	// in the Fabric account. STS is region-scoped from the base config; the
	// provider caches and refreshes the assumed credentials on expiry.
	originCfg := base.Copy()
	originCfg.Credentials = originCreds
	stsClient := sts.NewFromConfig(originCfg)
	provider := stscreds.NewAssumeRoleProvider(stsClient, launcherRoleARN, func(o *stscreds.AssumeRoleOptions) {
		o.RoleSessionName = sessionName
	})

	cfg := base.Copy()
	cfg.Credentials = aws.NewCredentialsCache(provider)

	return &Identity{
		cfg:               cfg,
		originCreds:       originCreds,
		launcherRoleARN:   launcherRoleARN,
		controllerRoleARN: controllerRoleARN,
		region:            region,
	}, nil
}

// CallerIdentity is the resolved STS GetCallerIdentity of the assumed launcher
// credentials — the assumed-role ARN, account, and user id observed in the
// Fabric account. Used to verify (and log) that the cross-account hop actually
// works from inside the controller pod.
type CallerIdentity struct {
	Account string
	ARN     string
	UserID  string
}

// VerifyCallerIdentity calls sts:GetCallerIdentity USING THE ASSUMED launcher
// credentials, proving the full chain (origin identity — the dedicated
// controller role or the POC default chain -> AssumeRole launcher) resolves
// and that the launcher trust accepts this controller. This is the operator-requested startup smoke: on a healthy pod it
// returns the assumed-role identity in the Fabric account (345678901234); on a
// broken trust/permission setup it returns the STS error verbatim so the
// failure is legible in controller logs rather than surfacing later as an
// opaque RunMicrovm denial. Bounded by configLoadTimeout.
func (i *Identity) VerifyCallerIdentity(ctx context.Context) (*CallerIdentity, error) {
	callCtx, cancel := context.WithTimeout(ctx, configLoadTimeout)
	defer cancel()

	out, err := sts.NewFromConfig(i.cfg).GetCallerIdentity(callCtx, &sts.GetCallerIdentityInput{})
	if err != nil {
		return nil, fmt.Errorf("microvm: GetCallerIdentity as launcher %s: %w", i.launcherRoleARN, err)
	}
	return &CallerIdentity{
		Account: aws.ToString(out.Account),
		ARN:     aws.ToString(out.Arn),
		UserID:  aws.ToString(out.UserId),
	}, nil
}

// MicrovmClient returns a Lambda MicroVMs client authenticated as the launcher
// role in the MicroVM region. Used by the C4 backend for RunMicrovm /
// GetMicrovm / ListMicrovms / TerminateMicrovm. The MicroVM control-plane lives
// in AWS's dedicated `lambda-microvms` service (github.com/aws/aws-sdk-go-v2/
// service/lambdamicrovms), NOT the base `lambda` service — the latter has no
// RunMicrovm/GetMicrovm ops.
func (i *Identity) MicrovmClient() *lambdamicrovms.Client {
	return lambdamicrovms.NewFromConfig(i.cfg)
}

// SecretsManagerClient returns a Secrets Manager client authenticated as the
// launcher role in the MicroVM region. Used by the C4 backend to write/delete
// the per-run identity + runtime-credential secrets under the launcher's
// scoped prefix.
func (i *Identity) SecretsManagerClient() *secretsmanager.Client {
	return secretsmanager.NewFromConfig(i.cfg)
}

// Region is the AWS region the MicroVM clients target (us-east-2 in prod).
func (i *Identity) Region() string { return i.region }

// LauncherRoleARN is the cross-account role the controller assumed.
func (i *Identity) LauncherRoleARN() string { return i.launcherRoleARN }

// ControllerRoleARN is the dedicated controller-only role the launcher hop
// originated from, or "" when the POC/rollback default-chain origin was used.
func (i *Identity) ControllerRoleARN() string { return i.controllerRoleARN }

// OriginCredentials returns the launcher hop's ORIGIN credentials — the
// dedicated controller role when ControllerRoleARN is set, else the default
// chain. The ziti-access-broker's consumer policy pins this identity, so the
// broker caller-verification proof MUST be signed with these credentials —
// signing with a separately resolved default chain attests the shared agent
// role even when the dedicated role is configured, and the broker 403s every
// lease (ka-0000006814).
func (i *Identity) OriginCredentials() aws.CredentialsProvider { return i.originCreds }

// Config exposes the resolved cross-account AWS config for callers that need to
// build a service client this package does not wrap directly. Prefer the typed
// LambdaClient / SecretsManagerClient helpers.
func (i *Identity) Config() aws.Config { return i.cfg }
