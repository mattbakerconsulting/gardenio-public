package microvm

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/lambdamicrovms"
	mvtypes "github.com/aws/aws-sdk-go-v2/service/lambdamicrovms/types"
	"github.com/aws/aws-sdk-go-v2/service/secretsmanager"
	smtypes "github.com/aws/aws-sdk-go-v2/service/secretsmanager/types"
)

// awsOpTimeout bounds each AWS control-plane call so a wedged launcher/STS hop
// never blocks the spawn door indefinitely. Mirrors the identity package's
// configLoadTimeout discipline: generous for a healthy call, short enough that a
// misconfig surfaces as an error.
const awsOpTimeout = 30 * time.Second

// runHookMode is the fixed run-hook mode the MicroVM image expects: it reads its
// Ziti identity + agent config from Secrets Manager and starts in proxy mode
// (nfd!15 bin/ziti-microvm.sh).
const runHookMode = "proxy"

// internetEgressConnectorARN builds the AWS-managed INTERNET_EGRESS network
// connector ARN for a region. Per the handoff contract the controller
// configures ONLY this connector (public-internet egress; the MicroVM reaches
// beads/NATS over the Ziti overlay, not the VPC). No ingress connectors are
// configured — the first test is outbound-only.
func internetEgressConnectorARN(region string) string {
	return fmt.Sprintf("arn:aws:lambda:%s:aws:network-connector:aws-network-connector:INTERNET_EGRESS", region)
}

// microvmAPI is the subset of the AWS lambdamicrovms client the backend uses.
// Narrowed to an interface so tests can substitute a fake without a live
// cross-account credential chain. *lambdamicrovms.Client satisfies it.
type microvmAPI interface {
	RunMicrovm(ctx context.Context, in *lambdamicrovms.RunMicrovmInput, optFns ...func(*lambdamicrovms.Options)) (*lambdamicrovms.RunMicrovmOutput, error)
	GetMicrovm(ctx context.Context, in *lambdamicrovms.GetMicrovmInput, optFns ...func(*lambdamicrovms.Options)) (*lambdamicrovms.GetMicrovmOutput, error)
	ListMicrovms(ctx context.Context, in *lambdamicrovms.ListMicrovmsInput, optFns ...func(*lambdamicrovms.Options)) (*lambdamicrovms.ListMicrovmsOutput, error)
	TerminateMicrovm(ctx context.Context, in *lambdamicrovms.TerminateMicrovmInput, optFns ...func(*lambdamicrovms.Options)) (*lambdamicrovms.TerminateMicrovmOutput, error)
	SuspendMicrovm(ctx context.Context, in *lambdamicrovms.SuspendMicrovmInput, optFns ...func(*lambdamicrovms.Options)) (*lambdamicrovms.SuspendMicrovmOutput, error)
	ResumeMicrovm(ctx context.Context, in *lambdamicrovms.ResumeMicrovmInput, optFns ...func(*lambdamicrovms.Options)) (*lambdamicrovms.ResumeMicrovmOutput, error)
}

// secretsAPI is the subset of the AWS secretsmanager client the backend uses.
// Narrowed for the same testability reason. *secretsmanager.Client satisfies it.
type secretsAPI interface {
	CreateSecret(ctx context.Context, in *secretsmanager.CreateSecretInput, optFns ...func(*secretsmanager.Options)) (*secretsmanager.CreateSecretOutput, error)
	PutSecretValue(ctx context.Context, in *secretsmanager.PutSecretValueInput, optFns ...func(*secretsmanager.Options)) (*secretsmanager.PutSecretValueOutput, error)
	DeleteSecret(ctx context.Context, in *secretsmanager.DeleteSecretInput, optFns ...func(*secretsmanager.Options)) (*secretsmanager.DeleteSecretOutput, error)
	ListSecrets(ctx context.Context, in *secretsmanager.ListSecretsInput, optFns ...func(*secretsmanager.Options)) (*secretsmanager.ListSecretsOutput, error)
}

// BackendConfig is the fully-resolved MicroVM launch configuration, sourced from
// config.Config (the MICROVM_* env the Helm chart sets) plus the C3 Identity's
// region. All ARNs are the Fabric-staged, launcher-scoped values; a mismatch
// with the launcher's IAM boundary surfaces as an AccessDenied at RunMicrovm.
type BackendConfig struct {
	// Region is the MicroVM region (us-east-2) — used to build the egress
	// connector ARN. Required.
	Region string

	// ImageIdentifier is the approved MicroVM image ARN RunMicrovm launches
	// (config MicrovmImageIdentifier). Required — the launcher scopes RunMicrovm
	// to exactly this image.
	ImageIdentifier string

	// ImageVersion pins the image version (config MicrovmImageVersion, e.g.
	// "7.0"). Empty lets AWS use the image's active version.
	ImageVersion string

	// ExecutionRoleARN is the role the Lambda service assumes inside the MicroVM
	// (config MicrovmExecutionRoleARN). Required — passed as RunMicrovm's
	// ExecutionRoleArn and scoped by the launcher's iam:PassRole grant.
	ExecutionRoleARN string

	// SecretPrefix is the Secrets Manager name prefix per-run secrets are written
	// under (config MicrovmSecretPrefix, "ziti-microvm-agents/"). A trailing slash
	// is normalized on. Required — the launcher's Secrets Manager grant is scoped
	// to this prefix.
	SecretPrefix string

	// StaticIdentitySecretARN is the shared pre-enrolled Ziti identity reused for
	// the single-concurrency POC (config MicrovmStaticIdentitySecretARN). Passed
	// as the run-hook identitySecretArn ONLY when no per-run identity issuer is
	// wired (ka-0000001924). DEPRECATED: config-gated rollback until the per-run
	// path passes the production E2E gate (ka-0000000183); ignored entirely when
	// the ziti-access-broker is configured, and refused outright in production
	// mode (ka-0000006687).
	StaticIdentitySecretARN string

	// MaximumDurationSeconds caps a MicroVM's wall-clock lifetime (the epic's 8h
	// max per session; the POC helper uses 3600). <=0 falls back to the POC
	// default.
	MaximumDurationSeconds int32

	// LogGroup is the CloudWatch Logs log group MicroVM runtime stdout/stderr
	// (image entrypoint, in-VM Ziti proxy, Gardenio agent process) is delivered
	// to (config MicrovmLogGroup, e.g. "/aws/lambda/microvms/ziti-microvm-poc").
	// The group must already exist and the execution role must hold
	// logs:CreateLogStream + logs:PutLogEvents on it — the Fabric stack grants
	// both on its staged group (nfd!15 main.tf WriteRuntimeLogs). Empty leaves
	// RunMicrovmInput.Logging unset entirely, preserving the pre-logging
	// behavior (only the image-level logging configuration, if any, applies).
	LogGroup string

	// LogStreamPrefix prefixes the per-run log stream name inside LogGroup: each
	// launch logs to "<prefix><beadID>" so concurrent agents never interleave
	// and a bead's logs are addressable from its ID alone (config
	// MicrovmLogStreamPrefix). Ignored when LogGroup is empty; empty falls back
	// to defaultLogStreamPrefix. CloudWatch forbids ':' and '*' in stream names,
	// so validate() fails closed on either.
	LogStreamPrefix string

	// ProductionMode declares this deployment production (config
	// MicrovmProductionMode / chart agents.microvm.productionMode). It converts
	// every remaining POC assumption from tolerated to fail-closed at startup
	// (ka-0000006687): the static POC identity must be gone (per-run broker
	// required), the launcher hop must originate from the dedicated
	// controller-only role rather than the shared agent identity, and the
	// launcher account must be pinned. Off keeps the POC/rollback semantics
	// byte-identical.
	ProductionMode bool

	// ExpectedLauncherAccount pins the AWS account the assumed launcher
	// identity must resolve to (config MicrovmExpectedLauncherAccount). When
	// set, StartupBackend compares it against the verified GetCallerIdentity
	// account and refuses the backend on mismatch — a wrong-account launcher
	// ARN (e.g. a stale POC value pasted into a new environment) fails closed
	// at boot instead of surfacing as cross-account writes. Empty skips the
	// check; production mode requires it.
	ExpectedLauncherAccount string

	// IdlePolicy, when non-nil, sets the AWS auto-suspend/auto-resume policy on
	// every RunMicrovm launch (config MICROVM_IDLE_*). Nil (the default) leaves
	// RunMicrovmInput.IdlePolicy unset, preserving the pre-idle-policy launch
	// shape byte-for-byte. See NewIdlePolicy for the config→SDK mapping
	// and the topology caveat: our MicroVMs are egress-only (only
	// INTERNET_EGRESS, internetEgressConnectorARN) so the AWS idle clock — which
	// counts inbound requests through the proxy endpoint — and AutoResumeEnabled
	// (resume-on-inbound) cannot fire in-topology. SuspendedDurationSeconds is
	// therefore the operative knob: a standby-billing auto-terminate backstop
	// that reaps a MicroVM the controller suspended but never resumed. Resume is
	// controller-driven (backend.Resume) rather than inbound-traffic-driven.
	IdlePolicy *mvtypes.IdlePolicy
}

const defaultMaximumDurationSeconds int32 = 3600

// defaultLogStreamPrefix names per-bead log streams "agent/<beadID>" when a log
// group is configured without an explicit stream prefix.
const defaultLogStreamPrefix = "agent/"

// tagResourceBead is the AWS resource tag key stamping every per-run secret (and
// MicroVM, where taggable) with its owning agent bead, so leak GC (C5) can find
// orphaned resources by bead.
const tagResourceBead = "gardenio:agent-bead"

// Backend is the lambda_microvm AgentRuntime: it translates a runtime-neutral
// AgentSpec into a per-run Secrets Manager credential document plus an
// idempotent RunMicrovm call, and drives GetMicrovm/TerminateMicrovm for status
// and teardown. It is constructed from a C3 *Identity (the cross-account
// launcher-role clients) and a BackendConfig, and is safe for concurrent use —
// the AWS clients are, and Backend holds no mutable state.
type Backend struct {
	microvm microvmAPI
	secrets secretsAPI
	cfg     BackendConfig
	logger  *slog.Logger

	// account is the AWS account the runtimes + per-run secrets live in (the
	// Fabric launcher account, e.g. 345678901234), observed from the assumed
	// caller identity at startup. Persisted on the agent bead as runtime_account
	// so operators/GC can locate a runtime cross-account. Empty when the
	// cross-account hop was not verified (e.g. the test seam).
	account string

	// issuer mints and revokes per-run Ziti identities via the
	// ziti-access-broker (ka-0000001924). Nil keeps the deprecated static-POC
	// identity path byte-identical (config-gated rollback). When set, every
	// launch fails closed on issuance and every exit path revokes by bead ID.
	issuer identityIssuer
}

// SetIssuer wires the per-run identity issuer. Called by startup wiring when
// the ziti-access-broker is configured; tests assign a fake.
func (b *Backend) SetIssuer(iss identityIssuer) { b.issuer = iss }

// PerRunIdentityEnabled reports whether launches mint per-run Ziti identities
// (broker configured) rather than reusing the deprecated static POC identity.
func (b *Backend) PerRunIdentityEnabled() bool { return b.issuer != nil }

// SetLauncherAccount records the AWS account the runtimes live in, as observed
// from the assumed caller identity at startup. Called by StartupBackend after
// VerifyCallerIdentity. Persisted onto agent beads as runtime_account.
func (b *Backend) SetLauncherAccount(account string) { b.account = account }

// LauncherAccount returns the AWS account the runtimes + per-run secrets live
// in (the Fabric launcher account), for persistence as runtime_account. Empty
// when the cross-account hop was not verified.
func (b *Backend) LauncherAccount() string { return b.account }

// compile-time assertion that Backend implements the runtime-neutral seam.
var _ AgentRuntime = (*Backend)(nil)

// backendIdentity is the subset of *Identity newBackendFromIdentity consumes: the verified
// cross-account clients plus the region fallback. Narrowed so StartupBackend can
// be exercised end-to-end with a fake that neither reaches AWS nor verifies a
// trust. *Identity satisfies it.
type backendIdentity interface {
	MicrovmClient() *lambdamicrovms.Client
	SecretsManagerClient() *secretsmanager.Client
	Region() string
}

// newBackendFromIdentity builds a MicroVM backend from a verified C3 Identity and
// the launch config. It returns an error if required config is missing so the
// spawn door fails closed rather than issuing a malformed RunMicrovm.
func newBackendFromIdentity(ident backendIdentity, cfg BackendConfig, logger *slog.Logger) (*Backend, error) {
	if logger == nil {
		logger = slog.Default()
	}
	cfg.SecretPrefix = normalizeSecretPrefix(cfg.SecretPrefix)
	cfg.normalizeLogging()
	if cfg.Region == "" {
		cfg.Region = ident.Region()
	}
	if cfg.MaximumDurationSeconds <= 0 {
		cfg.MaximumDurationSeconds = defaultMaximumDurationSeconds
	}
	// Raise the AWS idle-suspend window above the VM lifetime cap so it can never
	// fire mid-turn in the egress-only topology (ka-0000002917); must run after
	// MaximumDurationSeconds is normalized.
	normalizeIdlePolicy(cfg.IdlePolicy, cfg.MaximumDurationSeconds)
	if err := cfg.validate(); err != nil {
		return nil, err
	}
	return &Backend{
		microvm: ident.MicrovmClient(),
		secrets: ident.SecretsManagerClient(),
		cfg:     cfg,
		logger:  logger,
	}, nil
}

func (c BackendConfig) validate() error {
	var missing []string
	if strings.TrimSpace(c.Region) == "" {
		missing = append(missing, "Region")
	}
	if strings.TrimSpace(c.ImageIdentifier) == "" {
		missing = append(missing, "ImageIdentifier")
	}
	if strings.TrimSpace(c.ExecutionRoleARN) == "" {
		missing = append(missing, "ExecutionRoleARN")
	}
	if strings.TrimSpace(c.SecretPrefix) == "" {
		missing = append(missing, "SecretPrefix")
	}
	if len(missing) > 0 {
		return fmt.Errorf("microvm: backend config missing required fields: %s", strings.Join(missing, ", "))
	}
	// CloudWatch log group names allow [A-Za-z0-9_/.#-]; stream names forbid
	// ':' and '*'. A malformed value would surface as a per-launch AWS error, so
	// fail closed at boot instead — same discipline as the required fields.
	if strings.ContainsAny(c.LogGroup, ":*") {
		return fmt.Errorf("microvm: backend config LogGroup %q must not contain ':' or '*'", c.LogGroup)
	}
	if strings.ContainsAny(c.LogStreamPrefix, ":*") {
		return fmt.Errorf("microvm: backend config LogStreamPrefix %q must not contain ':' or '*'", c.LogStreamPrefix)
	}
	return nil
}

// minIdlePolicyDurationSeconds is the AWS RunMicrovm floor for the IdlePolicy
// duration members. AWS 400-rejects any launch whose IdlePolicy carries a
// MaxIdleDurationSeconds (or SuspendedDurationSeconds) below 60 —
// "Member must have value greater than or equal to 60" (ka-0000005755). The
// config carries 0 to mean "no explicit idle deadline"; 0 is not a legal wire
// value, so it must be floored to the AWS minimum before it reaches RunMicrovm.
const minIdlePolicyDurationSeconds = 60

// idleLifetimeSlackSeconds is added on top of a MicroVM's wall-clock lifetime
// cap (MaximumDurationSeconds) when raising the effective IdlePolicy
// MaxIdleDurationSeconds in normalizeIdlePolicy. It guarantees AWS's idle
// evaluation window always sits strictly beyond the VM's own self-terminate
// deadline, so the lifetime cap wins the race and the AWS idle-suspend hook can
// never fire mid-turn. 300s (5m) mirrors the leaseTTLSlack discipline (a small
// margin above the cap) without materially extending any real suspend window
// (the VM is already gone by MaximumDurationSeconds).
const idleLifetimeSlackSeconds int32 = 300

// normalizeIdlePolicy raises the effective AWS IdlePolicy MaxIdleDurationSeconds
// so it always EXCEEDS the VM's own wall-clock lifetime cap (maxDurationSeconds).
//
// THE BUG THIS FIXES (ka-0000002917): our MicroVMs are egress-only (only
// INTERNET_EGRESS, internetEgressConnectorARN). The AWS IdlePolicy idle clock is
// measured by INBOUND traffic through the MicroVM proxy endpoint (SDK IdlePolicy
// doc), which in an egress-only topology never arrives — so the clock counts up
// from boot and never resets. AWS therefore suspends the VM at its first idle
// evaluation window (~MaxIdleDurationSeconds after boot). With maxIdle floored to
// 60, AWS suspended a WORKING agent ~60s after boot; the entrypoint /suspend hook
// then SIGTERM-killed coop mid-turn (do_POST -> RUNTIME.stop). maxIdle is NOT
// behaviourally inert here — it is the reaper.
//
// The fix keeps suspend purely controller-driven (from gb stop, via
// SuspendMicrovm) by making AWS's idle deadline unreachable within a VM's life:
// once MaxIdleDurationSeconds > MaximumDurationSeconds, the VM self-terminates at
// its lifetime cap before AWS's idle timer would ever fire. SuspendedDurationSeconds
// (the standby-billing auto-terminate backstop that DOES bite, once the controller
// has suspended a VM) is untouched. No-op when IdlePolicy is nil (feature disabled).
func normalizeIdlePolicy(policy *mvtypes.IdlePolicy, maxDurationSeconds int32) {
	if policy == nil {
		return
	}
	floor := maxDurationSeconds + idleLifetimeSlackSeconds
	if aws.ToInt32(policy.MaxIdleDurationSeconds) < floor {
		policy.MaxIdleDurationSeconds = aws.Int32(floor)
	}
}

// NewIdlePolicy builds the optional AWS IdlePolicy for BackendConfig.IdlePolicy
// from the three config knobs (MICROVM_IDLE_*), keeping AWS SDK types out of the
// controller wiring. When enabled is false it returns nil, so RunMicrovm's
// IdlePolicy stays unset and the launch shape is byte-for-byte identical to the
// pre-idle-policy path. When enabled, all three members are set — the SDK marks
// AutoResumeEnabled, MaxIdleDurationSeconds, and SuspendedDurationSeconds all
// required whenever an IdlePolicy is present. See BackendConfig.IdlePolicy for
// the egress-only topology caveat (SuspendedDurationSeconds is the operative
// standby-billing backstop; AutoResumeEnabled cannot fire in-topology).
//
// Both duration members are floored at the AWS minimum (minIdlePolicyDurationSeconds)
// so a config value of 0 can never reach RunMicrovm and 400 the launch
// (ka-0000005755: idlePolicy.maxIdleDurationSeconds=0 blocked EVERY lambda_microvm
// launch in prod). This is only the WIRE-LEGALITY floor; the backend constructor
// additionally calls normalizeIdlePolicy to raise MaxIdleDurationSeconds ABOVE the
// VM lifetime cap so AWS idle-suspend can never fire mid-turn in the egress-only
// topology (ka-0000002917) — the idle clock is NOT inert, it counts from boot and
// never resets without inbound traffic.
func NewIdlePolicy(enabled, autoResume bool, maxIdleSeconds, suspendedSeconds int) *mvtypes.IdlePolicy {
	if !enabled {
		return nil
	}
	if maxIdleSeconds < minIdlePolicyDurationSeconds {
		maxIdleSeconds = minIdlePolicyDurationSeconds
	}
	if suspendedSeconds < minIdlePolicyDurationSeconds {
		suspendedSeconds = minIdlePolicyDurationSeconds
	}
	return &mvtypes.IdlePolicy{
		AutoResumeEnabled:        aws.Bool(autoResume),
		MaxIdleDurationSeconds:   aws.Int32(int32(maxIdleSeconds)),
		SuspendedDurationSeconds: aws.Int32(int32(suspendedSeconds)),
	}
}

// normalizeLogging trims the logging fields and defaults the stream prefix when
// a log group is set. Logging stays entirely off (Logging member unset on
// RunMicrovmInput) when LogGroup is empty.
func (c *BackendConfig) normalizeLogging() {
	c.LogGroup = strings.TrimSpace(c.LogGroup)
	c.LogStreamPrefix = strings.TrimSpace(c.LogStreamPrefix)
	if c.LogGroup == "" {
		c.LogStreamPrefix = ""
		return
	}
	if c.LogStreamPrefix == "" {
		c.LogStreamPrefix = defaultLogStreamPrefix
	}
}

// validateProductionMode enforces the production configuration contract
// (ka-0000006687): with Backend.ProductionMode set, every POC-era assumption
// must be explicitly retired or the whole backend refuses to construct — the
// spawn door then fails closed rather than launching on a POC boundary.
// Violations are joined so an operator sees the complete remediation list in
// one startup log line instead of fixing them one boot at a time.
func (sc StartupConfig) validateProductionMode() error {
	if !sc.Backend.ProductionMode {
		return nil
	}
	var violations []string
	if strings.TrimSpace(sc.Backend.StaticIdentitySecretARN) != "" {
		violations = append(violations,
			"StaticIdentitySecretARN must be unset (the shared POC Ziti identity is refused in production; per-run identities only, ka-0000001924)")
	}
	if !sc.Broker.Enabled() {
		violations = append(violations,
			"ZitiBrokerURL must be set (production launches require per-run Ziti identity issuance)")
	}
	if strings.TrimSpace(sc.Identity.ControllerRoleARN) == "" {
		violations = append(violations,
			"ControllerRoleARN must be set (production requires the dedicated controller-only launcher-hop role, not the shared agent identity)")
	}
	if strings.TrimSpace(sc.Backend.ExpectedLauncherAccount) == "" {
		violations = append(violations,
			"ExpectedLauncherAccount must be set (production pins the launcher account explicitly)")
	}
	if strings.TrimSpace(sc.Backend.ImageVersion) == "" {
		violations = append(violations,
			"ImageVersion must be pinned (production never floats on the image's active version)")
	}
	if strings.TrimSpace(sc.Backend.LogGroup) == "" {
		violations = append(violations,
			"LogGroup must be set (production runs deliver runtime logs to CloudWatch, ka-0000002120)")
	}
	if len(violations) > 0 {
		return fmt.Errorf("microvm: production mode refused (fail closed): %s", strings.Join(violations, "; "))
	}
	return nil
}

// arnParts is the account + region carried by an AWS ARN, extracted for the
// cross-field consistency check. Either field is "" when the ARN's segment is
// empty (e.g. IAM ARNs carry no region) or the string is not a well-formed ARN.
type arnParts struct {
	account string
	region  string
}

// parseARN splits an AWS ARN into its region + account segments. The ARN grammar
// is arn:partition:service:region:account-id:resource... — so region is field 3
// and account is field 4 (zero-indexed). A string that does not start with
// "arn:" or has too few fields yields a zero arnParts, which the consistency
// check treats as "unparseable, skip" rather than a mismatch: the resource ARN
// validity itself is the launcher IAM boundary's job (a bad ARN surfaces as an
// AccessDenied at RunMicrovm), whereas THIS check only guards against a
// well-formed ARN pointing at the WRONG account/region — the half-migrated
// overlay failure mode (S3 ka-0000002156).
func parseARN(arn string) arnParts {
	arn = strings.TrimSpace(arn)
	if !strings.HasPrefix(arn, "arn:") {
		return arnParts{}
	}
	// SplitN(6): partition, service, region, account, resource(+remainder).
	fields := strings.SplitN(arn, ":", 6)
	if len(fields) < 5 {
		return arnParts{}
	}
	return arnParts{account: fields[4], region: fields[3]}
}

// validateResourceConsistency fails the backend closed when the launcher-side
// resource configuration is internally inconsistent — the exact S3
// (ka-0000002156) requirement: "refuse to launch if account/region/role config
// is inconsistent". It is a STATIC, pre-AWS check (runs before any credential
// or STS work, like validateProductionMode) that catches a half-flipped
// account-migration overlay — e.g. the launcher role moved to the Gardenio
// account 123456789012 but the image/execution ARNs (or the region) still point
// at the Network Fabric account 345678901234 — at boot, before a single
// cross-account RunMicrovm/secret write can straddle two accounts.
//
// It complements, but does not replace, the DYNAMIC ExpectedLauncherAccount pin
// in startupBackend (which compares the *verified* GetCallerIdentity account
// after the assume-role hop): the dynamic check proves the assumed launcher
// lives where expected; this static check proves the launch config the launcher
// will act on (image + execution role + region) agrees with it, so RunMicrovm
// never passes a wrong-account image/role even if the launcher itself resolves.
//
// The launcher-hop ORIGIN role (Identity.ControllerRoleARN) is deliberately
// EXCLUDED from the account cross-check: in the cross-account POC/rollback chain
// the dedicated controller role legitimately lives in the controller's own
// account (123456789012) while the launcher/image/execution live in the Fabric
// account (345678901234), so requiring it to match would break that valid
// topology. Its region is IAM-less (no region segment) and never checked.
//
// Empty ARNs are skipped here (the required-field completeness check is
// validate()'s job), so a self-consistent POC/rollback config validates
// byte-identically — this method only rejects a config whose *populated*
// launcher-side ARNs disagree with each other, the region, or the pinned
// account.
func (sc StartupConfig) validateResourceConsistency() error {
	region := strings.TrimSpace(sc.Backend.Region)
	if region == "" {
		// Region defaults from the identity in newBackendFromIdentity; an empty
		// value here is already a validate() failure, so nothing to cross-check.
		region = strings.TrimSpace(sc.Identity.Region)
	}

	// Launcher-side resources whose ARN account must all agree (and, for the
	// image, whose region must match the launch region). Named so a violation
	// tells the operator exactly which field is out of family.
	type resource struct {
		name       string
		arn        string
		wantRegion bool // image ARN carries a region that must equal the launch region
	}
	resources := []resource{
		{name: "LauncherRoleARN", arn: strings.TrimSpace(sc.Identity.LauncherRoleARN)},
		{name: "ExecutionRoleARN", arn: strings.TrimSpace(sc.Backend.ExecutionRoleARN)},
		{name: "ImageIdentifier", arn: strings.TrimSpace(sc.Backend.ImageIdentifier), wantRegion: true},
	}

	var violations []string

	// Account family: every populated launcher-side ARN must share one account.
	// Seed the expected account with the explicit pin when set, so a mismatch is
	// reported against what the operator declared rather than an arbitrary
	// first-seen ARN.
	wantAccount := strings.TrimSpace(sc.Backend.ExpectedLauncherAccount)
	accountSource := "ExpectedLauncherAccount"
	for _, r := range resources {
		if r.arn == "" {
			continue
		}
		parts := parseARN(r.arn)
		if parts.account == "" {
			continue // unparseable account segment — not this check's concern
		}
		if wantAccount == "" {
			wantAccount = parts.account
			accountSource = r.name
			continue
		}
		if parts.account != wantAccount {
			violations = append(violations, fmt.Sprintf(
				"%s account %s does not match %s account %s",
				r.name, parts.account, accountSource, wantAccount))
		}
	}

	// Region family: the image ARN's region must equal the launch region, so
	// RunMicrovm's clients (built for the launch region) and the image it
	// references never straddle regions.
	if region != "" {
		for _, r := range resources {
			if !r.wantRegion || r.arn == "" {
				continue
			}
			parts := parseARN(r.arn)
			if parts.region == "" {
				continue
			}
			if parts.region != region {
				violations = append(violations, fmt.Sprintf(
					"%s region %s does not match launch region %s",
					r.name, parts.region, region))
			}
		}
	}

	if len(violations) > 0 {
		return fmt.Errorf("microvm: resource config inconsistent (fail closed): %s", strings.Join(violations, "; "))
	}
	return nil
}

// normalizeSecretPrefix trims whitespace and guarantees exactly one trailing
// slash, so callers can join with a bead ID unconditionally
// (prefix + beadID + "/agent-config").
func normalizeSecretPrefix(p string) string {
	p = strings.TrimSpace(p)
	if p == "" {
		return p
	}
	return strings.TrimRight(p, "/") + "/"
}

// agentSecretName is the Secrets Manager name for a bead's runtime-credential
// document: <prefix><beadID>/agent-config. It lives under the launcher-scoped
// prefix so the launcher's Secrets Manager grant covers it.
func (b *Backend) agentSecretName(beadID string) string {
	return b.cfg.SecretPrefix + beadID + "/agent-config"
}

// identitySecretName is the Secrets Manager name for a bead's per-run Ziti
// identity material: <prefix><beadID>/ziti-identity (contract §3). Same
// launcher-scoped prefix, so the existing IAM boundary covers it (launcher:
// write/delete, no read-back; execution role: read-only).
func (b *Backend) identitySecretName(beadID string) string {
	return b.cfg.SecretPrefix + beadID + "/ziti-identity"
}

// runtimeDocument is the credential/config document the MicroVM image reads from
// Secrets Manager after the /run hook (nfd!15 "Credential delivery contract").
// The image materializes `environment` as process env and the optional
// file-oriented fields to the UID-1000 runtime filesystem with restrictive
// modes. omitempty keeps absent optional credentials out of the JSON entirely.
type runtimeDocument struct {
	Environment       map[string]string `json:"environment"`
	NATSToken         string            `json:"natsToken,omitempty"`
	ClaudeCredentials string            `json:"claudeCredentials,omitempty"`
	SSHPrivateKey     string            `json:"sshPrivateKey,omitempty"`
}

// runHookPayload is the (non-secret) run-hook payload passed to RunMicrovm. It
// carries ONLY the two secret ARNs plus mode/runAgent — never any credential
// value (those live inside the referenced secrets). agentSecretArn is omitted
// when empty (matches the Fabric helper's shape).
type runHookPayload struct {
	IdentitySecretArn string `json:"identitySecretArn,omitempty"`
	AgentSecretArn    string `json:"agentSecretArn,omitempty"`
	Mode              string `json:"mode"`
	RunAgent          bool   `json:"runAgent"`
}

// Launch realizes the agent as exactly one MicroVM (AgentRuntime.Launch).
//
// Order matters for the fail-closed + rollback + idempotency invariants
// (contract §4 launch sequence):
//  1. With an issuer wired, mint a fresh per-run Ziti identity lease for the
//     bead FIRST. Fail closed: any issuance failure aborts the launch — the
//     backend never falls back to the static/shared identity (contract §5.3).
//  2. Write the per-run ziti-identity secret (the lease's material), then the
//     agent-config secret, both under ziti-microvm-agents/<id>/ and tagged
//     with the bead ID. Idempotent: a re-run overwrites values in place.
//  3. RunMicrovm with the bead ID as ClientToken so a duplicate spawn event
//     returns the SAME MicroVM instead of launching a second. INTERNET_EGRESS
//     is the only connector; the run-hook payload carries only the secret ARNs
//     (identitySecretArn now points at the per-run secret).
//  4. On any failure after a step, unwind everything that step left behind:
//     delete the secrets written so far and revoke the lease, so a failed
//     launch leaves no orphaned per-run material or live overlay identity.
func (b *Backend) Launch(ctx context.Context, spec AgentSpec) (RuntimeHandle, error) {
	beadID := strings.TrimSpace(spec.BeadID)
	if beadID == "" {
		return RuntimeHandle{}, errors.New("microvm: Launch requires spec.BeadID")
	}

	// Defense in depth for the production contract (ka-0000006687): startup
	// validation already refuses this combination, but a production backend
	// must never fall through to the shared static identity even if wiring
	// regresses — refuse the launch outright.
	if b.cfg.ProductionMode && b.issuer == nil {
		return RuntimeHandle{}, fmt.Errorf("microvm: refusing to launch %s — production mode requires a per-run identity issuer (no static-identity fallback)", beadID)
	}

	handle := RuntimeHandle{
		BeadID:            beadID,
		IdentitySecretARN: b.cfg.StaticIdentitySecretARN,
		IsPerRunIdentity:  false, // deprecated static path, never revoked here.
	}

	// (1) Per-run identity issuance — fail closed before any AWS write.
	if b.issuer != nil {
		leaseTTL := time.Duration(b.cfg.MaximumDurationSeconds)*time.Second + leaseTTLSlack
		lease, err := b.issuer.IssueLease(ctx, beadID, leaseTTL)
		if err != nil {
			return RuntimeHandle{}, fmt.Errorf("microvm: per-run identity issuance for %s failed (launch aborted, fail closed): %w", beadID, err)
		}
		material, err := IdentitySecretValue(lease)
		if err != nil {
			b.revokeLeaseBestEffort(ctx, beadID, "unusable lease rollback")
			return RuntimeHandle{}, fmt.Errorf("microvm: per-run identity for %s unusable (launch aborted, fail closed): %w", beadID, err)
		}
		identityARN, err := b.putSecret(ctx, b.identitySecretName(beadID), material,
			fmt.Sprintf("gardenio lambda_microvm per-run ziti identity for %s", beadID), beadID)
		if err != nil {
			b.revokeLeaseBestEffort(ctx, beadID, "identity secret write rollback")
			return RuntimeHandle{}, fmt.Errorf("microvm: write per-run identity secret for %s: %w", beadID, err)
		}
		handle.IdentitySecretARN = identityARN
		handle.IsPerRunIdentity = true
	}

	// (2) Per-run credential document.
	secretARN, err := b.putAgentSecret(ctx, spec)
	if err != nil {
		b.unwindLaunch(ctx, handle, "agent secret write rollback")
		return RuntimeHandle{}, fmt.Errorf("microvm: write per-run secret for %s: %w", beadID, err)
	}
	handle.AgentSecretARN = secretARN

	// (3) Idempotent launch.
	out, err := b.runMicrovm(ctx, spec, handle.IdentitySecretARN, secretARN)
	if err != nil {
		// (4) Rollback: revoke everything this launch created so a failed
		// launch is clean (secrets + identity lease).
		b.deleteSecretBestEffort(ctx, secretARN, beadID, "failed launch rollback")
		b.unwindLaunch(ctx, handle, "failed launch rollback")
		return RuntimeHandle{}, fmt.Errorf("microvm: RunMicrovm for %s: %w", beadID, err)
	}

	handle.MicrovmID = aws.ToString(out.MicrovmId)
	handle.StartedAt = aws.ToTime(out.StartedAt)
	logStream := ""
	if b.cfg.LogGroup != "" {
		logStream = b.cfg.LogStreamPrefix + beadID
	}
	b.logger.Info("lambda_microvm: launched",
		"bead", beadID, "microvm_id", handle.MicrovmID,
		"state", string(out.State), "image", aws.ToString(out.ImageArn),
		"image_version", aws.ToString(out.ImageVersion),
		"per_run_identity", handle.IsPerRunIdentity,
		"log_group", b.cfg.LogGroup, "log_stream", logStream)
	return handle, nil
}

// unwindLaunch tears down the per-run identity artifacts a partially-failed
// launch created: the identity secret (when per-run) and the broker lease.
// Best-effort — the caller is already returning the launch error, and leak GC
// plus the broker TTL reaper backstop anything missed here.
func (b *Backend) unwindLaunch(ctx context.Context, handle RuntimeHandle, why string) {
	if handle.IsPerRunIdentity && strings.TrimSpace(handle.IdentitySecretARN) != "" {
		b.deleteSecretBestEffort(ctx, handle.IdentitySecretARN, handle.BeadID, why)
	}
	b.revokeLeaseBestEffort(ctx, handle.BeadID, why)
}

// revokeLeaseBestEffort revokes the bead's identity leases and only logs on
// failure — used on rollback/teardown paths where the broker TTL reaper is the
// backstop and a revoke failure must not mask the primary error.
func (b *Backend) revokeLeaseBestEffort(ctx context.Context, beadID, why string) {
	if b.issuer == nil {
		return
	}
	if err := b.issuer.RevokeLeases(ctx, beadID); err != nil {
		b.logger.Warn("lambda_microvm: per-run identity revoke failed (broker TTL reaper backstops)",
			"bead", beadID, "why", why, "error", err)
	}
}

// putAgentSecret creates (or, if it already exists from a prior attempt,
// overwrites) the per-run runtime-credential secret and returns its ARN. It is
// idempotent so a retried launch reuses the same secret name rather than
// failing. The secret is tagged with the bead ID on creation so leak GC can
// find it. The secret VALUE is never logged.
func (b *Backend) putAgentSecret(ctx context.Context, spec AgentSpec) (string, error) {
	doc := runtimeDocument{
		Environment:       spec.Environment,
		NATSToken:         spec.NATSToken,
		ClaudeCredentials: spec.ClaudeCredentials,
		SSHPrivateKey:     spec.SSHPrivateKey,
	}
	if doc.Environment == nil {
		doc.Environment = map[string]string{}
	}
	body, err := json.Marshal(doc)
	if err != nil {
		return "", fmt.Errorf("marshal runtime document: %w", err)
	}
	return b.putSecret(ctx, b.agentSecretName(spec.BeadID), string(body),
		fmt.Sprintf("gardenio lambda_microvm per-run agent config for %s", spec.BeadID), spec.BeadID)
}

// putSecret creates (or, if it already exists from a prior attempt, overwrites)
// a per-run secret and returns its ARN. Idempotent so a retried launch reuses
// the same secret name rather than failing. Tagged with the bead ID on creation
// so leak GC can find it. The secret VALUE is never logged.
func (b *Backend) putSecret(ctx context.Context, name, payload, description, beadID string) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, awsOpTimeout)
	defer cancel()

	created, err := b.secrets.CreateSecret(ctx, &secretsmanager.CreateSecretInput{
		Name:         aws.String(name),
		SecretString: aws.String(payload),
		Description:  aws.String(description),
		Tags: []smtypes.Tag{
			{Key: aws.String(tagResourceBead), Value: aws.String(beadID)},
		},
	})
	if err == nil {
		return aws.ToString(created.ARN), nil
	}

	// A prior launch attempt for this bead may have already created the secret
	// (e.g. a retry after RunMicrovm failed post-secret, or a scheduled-for-
	// deletion secret with the same name). Overwrite the value in place so the
	// launch stays idempotent on the bead ID.
	var exists *smtypes.ResourceExistsException
	if errors.As(err, &exists) {
		put, putErr := b.secrets.PutSecretValue(ctx, &secretsmanager.PutSecretValueInput{
			SecretId:     aws.String(name),
			SecretString: aws.String(payload),
		})
		if putErr != nil {
			return "", fmt.Errorf("overwrite existing secret %s: %w", name, putErr)
		}
		return aws.ToString(put.ARN), nil
	}
	return "", fmt.Errorf("create secret %s: %w", name, err)
}

// runMicrovm issues the idempotent RunMicrovm call. The bead ID is the AWS
// ClientToken, so AWS collapses duplicate spawn events for the same bead onto a
// single MicroVM. Only INTERNET_EGRESS is configured; the run-hook payload
// references the identity + agent secrets by ARN only (the per-run identity
// secret when the issuer is wired, the deprecated static one otherwise).
func (b *Backend) runMicrovm(ctx context.Context, spec AgentSpec, identitySecretARN, agentSecretARN string) (*lambdamicrovms.RunMicrovmOutput, error) {
	ctx, cancel := context.WithTimeout(ctx, awsOpTimeout)
	defer cancel()

	hook := runHookPayload{
		IdentitySecretArn: identitySecretARN,
		AgentSecretArn:    agentSecretARN,
		Mode:              runHookMode,
		RunAgent:          agentSecretARN != "",
	}
	hookJSON, err := json.Marshal(hook)
	if err != nil {
		return nil, fmt.Errorf("marshal run-hook payload: %w", err)
	}

	in := &lambdamicrovms.RunMicrovmInput{
		ImageIdentifier:          aws.String(b.cfg.ImageIdentifier),
		ClientToken:              aws.String(spec.BeadID),
		ExecutionRoleArn:         aws.String(b.cfg.ExecutionRoleARN),
		EgressNetworkConnectors:  []string{internetEgressConnectorARN(b.cfg.Region)},
		MaximumDurationInSeconds: aws.Int32(b.cfg.MaximumDurationSeconds),
		RunHookPayload:           aws.String(string(hookJSON)),
		Logging:                  b.logging(spec.BeadID),
	}
	if b.cfg.ImageVersion != "" {
		in.ImageVersion = aws.String(b.cfg.ImageVersion)
	}
	// IdlePolicy is opt-in (config MICROVM_IDLE_*). Nil leaves the field unset so
	// the launch shape stays byte-for-byte identical to the pre-idle-policy path.
	if b.cfg.IdlePolicy != nil {
		in.IdlePolicy = b.cfg.IdlePolicy
	}
	return b.microvm.RunMicrovm(ctx, in)
}

// logging builds the per-run CloudWatch Logging member for RunMicrovmInput:
// the configured group plus a per-bead stream ("<prefix><beadID>") so
// concurrent agents never interleave and a run's logs are addressable from its
// bead ID alone. Returns nil (Logging unset — the image-level configuration,
// if any, applies) when no log group is configured, preserving the pre-logging
// launch shape byte-for-byte.
func (b *Backend) logging(beadID string) mvtypes.Logging {
	if b.cfg.LogGroup == "" {
		return nil
	}
	return &mvtypes.LoggingMemberCloudWatch{Value: mvtypes.CloudWatchLogging{
		LogGroup:  aws.String(b.cfg.LogGroup),
		LogStream: aws.String(b.cfg.LogStreamPrefix + beadID),
	}}
}

// Status maps GetMicrovm to the backend-neutral RuntimeStatus (AgentRuntime.
// Status). A vanished MicroVM (ResourceNotFound) is reported Terminal, not an
// error, so the reconciler treats "gone" as gone rather than as a transient
// failure.
func (b *Backend) Status(ctx context.Context, handle RuntimeHandle) (RuntimeStatus, error) {
	if strings.TrimSpace(handle.MicrovmID) == "" {
		// No VM was ever created (e.g. a launch that failed before RunMicrovm):
		// nothing runs, treat as terminal + non-existent.
		return RuntimeStatus{State: string(mvtypes.MicrovmStateTerminated), Terminal: true, Exists: false}, nil
	}
	ctx, cancel := context.WithTimeout(ctx, awsOpTimeout)
	defer cancel()

	out, err := b.microvm.GetMicrovm(ctx, &lambdamicrovms.GetMicrovmInput{
		MicrovmIdentifier: aws.String(handle.MicrovmID),
	})
	if err != nil {
		var notFound *mvtypes.ResourceNotFoundException
		if errors.As(err, &notFound) {
			// Gone entirely: the reconciler treats a vanished runtime as an
			// unambiguously-closable leaked bead (Exists=false), not a transient.
			return RuntimeStatus{State: string(mvtypes.MicrovmStateTerminated), Terminal: true, Exists: false}, nil
		}
		return RuntimeStatus{}, fmt.Errorf("microvm: GetMicrovm %s: %w", handle.MicrovmID, err)
	}
	s := statusFromState(out.State, aws.ToString(out.StateReason))
	s.Exists = true
	s.StartedAt = aws.ToTime(out.StartedAt)
	return s, nil
}

// statusFromState maps an AWS MicrovmState to the backend-neutral RuntimeStatus.
func statusFromState(state mvtypes.MicrovmState, reason string) RuntimeStatus {
	s := RuntimeStatus{State: string(state), StateReason: reason}
	switch state {
	case mvtypes.MicrovmStatePending, mvtypes.MicrovmStateRunning,
		mvtypes.MicrovmStateSuspending, mvtypes.MicrovmStateSuspended:
		s.Live = true
	case mvtypes.MicrovmStateTerminating, mvtypes.MicrovmStateTerminated:
		s.Terminal = true
	default:
		// Unknown/future state: treat conservatively as neither confirmed-live
		// nor confirmed-terminal so the reconciler neither reaps nor trusts it
		// blindly.
	}
	return s
}

// Terminate tears down the MicroVM and revokes its per-run secrets and Ziti
// identity leases (AgentRuntime.Terminate). Idempotent: a missing MicroVM,
// already-deleted secret, or already-revoked lease is a success, so every exit
// path — normal termination, timeout reconcile-close, failed-launch retry, and
// leak GC — can call it freely. The shared static POC identity secret is NEVER
// deleted (it outlives any single run); only a per-run identity is revoked.
func (b *Backend) Terminate(ctx context.Context, handle RuntimeHandle, reason string) error {
	var firstErr error

	if id := strings.TrimSpace(handle.MicrovmID); id != "" {
		tctx, cancel := context.WithTimeout(ctx, awsOpTimeout)
		_, err := b.microvm.TerminateMicrovm(tctx, &lambdamicrovms.TerminateMicrovmInput{
			MicrovmIdentifier: aws.String(id),
		})
		cancel()
		if err != nil {
			var notFound *mvtypes.ResourceNotFoundException
			if !errors.As(err, &notFound) {
				firstErr = fmt.Errorf("microvm: TerminateMicrovm %s: %w", id, err)
				b.logger.Warn("lambda_microvm: terminate failed",
					"bead", handle.BeadID, "microvm_id", id, "reason", reason, "error", err)
			}
		} else {
			b.logger.Info("lambda_microvm: terminated",
				"bead", handle.BeadID, "microvm_id", id, "reason", reason)
		}
	}

	// Revoke the per-run agent-config secret. Force-delete (no recovery window)
	// so a re-launch of the same bead can immediately recreate the name.
	if arn := strings.TrimSpace(handle.AgentSecretARN); arn != "" {
		if err := b.deleteSecret(ctx, arn); err != nil && firstErr == nil {
			firstErr = err
		}
	}

	// A per-run identity secret is task-scoped and is revoked with the run.
	// The shared static POC identity is left intact.
	if handle.IsPerRunIdentity {
		if arn := strings.TrimSpace(handle.IdentitySecretARN); arn != "" {
			if err := b.deleteSecret(ctx, arn); err != nil && firstErr == nil {
				firstErr = err
			}
		}
	}

	// Revoke the bead's Ziti identity leases at the broker (disable+delete the
	// overlay identity — the secret deletes above only remove the material).
	// Keyed purely by bead ID (contract §4), so this works from every exit
	// path, including ones that never persisted a lease id. A revoke failure
	// surfaces so C5 retries; the broker TTL reaper is the crash backstop.
	if b.issuer != nil {
		if beadID := strings.TrimSpace(handle.BeadID); beadID != "" {
			if err := b.issuer.RevokeLeases(ctx, beadID); err != nil {
				b.logger.Warn("lambda_microvm: per-run identity revoke failed (broker TTL reaper backstops)",
					"bead", beadID, "reason", reason, "error", err)
				if firstErr == nil {
					firstErr = err
				}
			}
		}
	}

	return firstErr
}

// Suspend suspends the MicroVM identified by handle, preserving its full memory
// and disk state (AWS SuspendMicrovm — SUSPENDING → SUSPENDED). It is the
// primary standby mechanism for the native suspend/resume path: the controller
// calls it on gb stop, leaving the bead open, rather than terminating the VM
// (plan ka-0000004518 / epic ka-0000004689).
//
// Idempotent in the same spirit as Terminate: identifier-only input, and a
// MicroVM that is already gone (ResourceNotFound) or not in a suspendable state
// (Conflict — e.g. already SUSPENDED/SUSPENDING, or TERMINATING) is a success,
// not a hard error, so a duplicate stop event or a race with the reconciler
// never surfaces as a spurious failure. A missing id (no VM was ever created)
// is a no-op success. Any other AWS error propagates so the caller can retry.
func (b *Backend) Suspend(ctx context.Context, handle RuntimeHandle) error {
	id := strings.TrimSpace(handle.MicrovmID)
	if id == "" {
		return nil
	}
	ctx, cancel := context.WithTimeout(ctx, awsOpTimeout)
	defer cancel()

	_, err := b.microvm.SuspendMicrovm(ctx, &lambdamicrovms.SuspendMicrovmInput{
		MicrovmIdentifier: aws.String(id),
	})
	if isNotFoundOrWrongState(err) {
		b.logger.Info("lambda_microvm: suspend no-op (already gone or not suspendable)",
			"bead", handle.BeadID, "microvm_id", id)
		return nil
	}
	if err != nil {
		return fmt.Errorf("microvm: SuspendMicrovm %s: %w", id, err)
	}
	b.logger.Info("lambda_microvm: suspended", "bead", handle.BeadID, "microvm_id", id)
	return nil
}

// Resume resumes the suspended MicroVM identified by handle, restoring it to
// RUNNING with all state intact (AWS ResumeMicrovm — the VM must be SUSPENDED).
// Because our MicroVMs are egress-only (internetEgressConnectorARN), AWS
// resume-on-inbound cannot fire, so resume is controller-driven: the controller
// calls this on the next message for a suspended agent (mirroring the gVisor
// name-keyed thaw).
//
// Idempotent like Suspend: a MicroVM that is already gone (ResourceNotFound) or
// not in a resumable state (Conflict — e.g. already RUNNING/PENDING, or
// TERMINATING) is a success, not a hard error, so a duplicate resume event or a
// race never surfaces as a spurious failure. A missing id is a no-op success.
// Any other AWS error propagates so the caller can retry.
func (b *Backend) Resume(ctx context.Context, handle RuntimeHandle) error {
	id := strings.TrimSpace(handle.MicrovmID)
	if id == "" {
		return nil
	}
	ctx, cancel := context.WithTimeout(ctx, awsOpTimeout)
	defer cancel()

	_, err := b.microvm.ResumeMicrovm(ctx, &lambdamicrovms.ResumeMicrovmInput{
		MicrovmIdentifier: aws.String(id),
	})
	if isNotFoundOrWrongState(err) {
		b.logger.Info("lambda_microvm: resume no-op (already gone or not resumable)",
			"bead", handle.BeadID, "microvm_id", id)
		return nil
	}
	if err != nil {
		return fmt.Errorf("microvm: ResumeMicrovm %s: %w", id, err)
	}
	b.logger.Info("lambda_microvm: resumed", "bead", handle.BeadID, "microvm_id", id)
	return nil
}

// isNotFoundOrWrongState reports whether err is the benign "the MicroVM is not
// in a state this transition applies to" family that Suspend/Resume tolerate:
// ResourceNotFoundException (the VM is already gone) or ConflictException (the
// request conflicts with the current MicroVM state — already suspended/resumed,
// mid-transition, or terminating). Both make an idempotent suspend/resume a
// no-op success rather than a hard error. A nil err returns false.
func isNotFoundOrWrongState(err error) bool {
	if err == nil {
		return false
	}
	var notFound *mvtypes.ResourceNotFoundException
	var conflict *mvtypes.ConflictException
	return errors.As(err, &notFound) || errors.As(err, &conflict)
}

// deleteSecret force-deletes a secret by ARN (no recovery window), treating an
// already-absent secret as success so teardown is idempotent.
func (b *Backend) deleteSecret(ctx context.Context, arn string) error {
	ctx, cancel := context.WithTimeout(ctx, awsOpTimeout)
	defer cancel()

	_, err := b.secrets.DeleteSecret(ctx, &secretsmanager.DeleteSecretInput{
		SecretId:                   aws.String(arn),
		ForceDeleteWithoutRecovery: aws.Bool(true),
	})
	if err != nil {
		var notFound *smtypes.ResourceNotFoundException
		if errors.As(err, &notFound) {
			return nil
		}
		return fmt.Errorf("microvm: DeleteSecret %s: %w", arn, err)
	}
	return nil
}

// deleteSecretBestEffort deletes a secret and only logs on failure — used on the
// launch rollback path, where the caller is already returning the launch error
// and a rollback failure must not mask it.
func (b *Backend) deleteSecretBestEffort(ctx context.Context, arn, beadID, why string) {
	if err := b.deleteSecret(ctx, arn); err != nil {
		b.logger.Warn("lambda_microvm: rollback secret delete failed (leaked per-run secret)",
			"bead", beadID, "secret_arn", arn, "why", why, "error", err)
	}
}

// StartupConfig bundles the C3 cross-account identity options with the C4 launch
// config and the per-run identity broker options (ka-0000001924), as sourced
// from config.Config's MICROVM_* fields. It is the single input to
// StartupBackend.
type StartupConfig struct {
	Identity Options
	Backend  BackendConfig
	Broker   BrokerOptions
}

// startupIdentity is the subset of *Identity StartupBackend drives: verify the
// cross-account hop, hand its clients to newBackendFromIdentity, and supply the origin
// credentials the broker proof is signed with. Narrowed so the startup
// branches can be tested without a live credential chain or trust.
type startupIdentity interface {
	backendIdentity
	VerifyCallerIdentity(ctx context.Context) (*CallerIdentity, error)
	OriginCredentials() aws.CredentialsProvider
}

// StartupBackend constructs the MicroVM backend at controller boot (epic
// ka-0000005010, C3 identity + C4 backend). It folds in the operator-requested
// C3 assume-role smoke (GetCallerIdentity), so it is the single boot entry point
// for the whole MicroVM path. It preserves the C3 default-off + best-effort
// contract:
//
//   - identity disabled (launcher role or region unset) → returns (nil, nil):
//     the MicroVM backend simply stays unavailable and the spawn door fails
//     closed exactly as before.
//   - identity build / assume-role verification fails → logs a warning and
//     returns (nil, err); the controller keeps booting the Kubernetes backend.
//   - launch config incomplete (image/exec-role/prefix unset) → logs a warning
//     and returns (nil, err); the identity works but launches can't, so the
//     backend stays unavailable rather than issuing malformed RunMicrovm calls.
//   - broker configured but its client cannot be built → logs a warning and
//     returns (nil, err). Fail closed: a per-run-identity deployment must
//     never silently degrade to the shared static identity (ka-0000001924).
//   - success → verifies the cross-account hop (logging GetCallerIdentity) and
//     returns a ready *Backend (with the per-run identity issuer wired when
//     the broker is configured).
//
// The returned error is informational; callers treat a nil Backend as "MicroVM
// backend unavailable" and continue.
func StartupBackend(ctx context.Context, logger *slog.Logger, sc StartupConfig) (*Backend, error) {
	// Adapt NewIdentity / NewBrokerClient (which return concrete types) to the
	// seams startupBackend drives, so tests can inject fakes without AWS.
	return startupBackend(ctx, logger, sc,
		func(ctx context.Context, o Options) (startupIdentity, error) {
			return NewIdentity(ctx, o)
		},
		func(ctx context.Context, o BrokerOptions, creds aws.CredentialsProvider, l *slog.Logger) (identityIssuer, error) {
			return NewBrokerClient(ctx, o, creds, l)
		})
}

func startupBackend(
	ctx context.Context,
	logger *slog.Logger,
	sc StartupConfig,
	newIdentity func(context.Context, Options) (startupIdentity, error),
	newIssuer func(context.Context, BrokerOptions, aws.CredentialsProvider, *slog.Logger) (identityIssuer, error),
) (*Backend, error) {
	if logger == nil {
		logger = slog.Default()
	}
	if !sc.Identity.Enabled() {
		logger.Debug("lambda_microvm: backend disabled (launcher role or region unset)")
		return nil, nil
	}

	// Production-mode contract (ka-0000006687): refuse the whole backend
	// before any AWS work when a POC assumption survives into a deployment
	// declared production — never launch on the shared identity or the static
	// POC Ziti secret.
	if err := sc.validateProductionMode(); err != nil {
		logger.Warn("lambda_microvm: production-mode validation failed; MicroVM backend unavailable (fail closed)",
			"error", err)
		return nil, err
	}

	// Static account/region/role consistency (S3 ka-0000002156): refuse a
	// half-flipped migration config — e.g. the launcher role moved to the
	// Gardenio account but the image/execution ARNs still point at Network
	// Fabric — before any AWS work, so RunMicrovm never straddles two accounts.
	// Complements the dynamic ExpectedLauncherAccount pin below (verified caller
	// account) with a check of what the launcher will actually act on.
	if err := sc.validateResourceConsistency(); err != nil {
		logger.Warn("lambda_microvm: resource-consistency validation failed; MicroVM backend unavailable (fail closed)",
			"error", err)
		return nil, err
	}

	ident, err := newIdentity(ctx, sc.Identity)
	if err != nil {
		logger.Warn("lambda_microvm: failed to build cross-account identity; MicroVM backend unavailable",
			"launcher_role", sc.Identity.LauncherRoleARN, "region", sc.Identity.Region, "error", err)
		return nil, err
	}

	// Verify the assume-role hop from inside the pod (operator-requested C3
	// smoke), logging the assumed identity so a broken trust surfaces at boot.
	caller, err := ident.VerifyCallerIdentity(ctx)
	if err != nil {
		logger.Warn("lambda_microvm: assume-role verification failed; MicroVM backend unavailable",
			"launcher_role", sc.Identity.LauncherRoleARN, "region", sc.Identity.Region, "error", err)
		return nil, err
	}

	// Launcher-account pin (ka-0000006687): the verified caller must live in
	// the explicitly-configured account, so a stale/wrong launcher ARN fails
	// closed at boot instead of surfacing as cross-account writes.
	if want := strings.TrimSpace(sc.Backend.ExpectedLauncherAccount); want != "" && caller.Account != want {
		err := fmt.Errorf("microvm: assumed launcher account %s does not match ExpectedLauncherAccount %s (fail closed)", caller.Account, want)
		logger.Warn("lambda_microvm: launcher account mismatch; MicroVM backend unavailable (fail closed)",
			"launcher_role", sc.Identity.LauncherRoleARN, "assumed_account", caller.Account,
			"expected_account", want, "error", err)
		return nil, err
	}

	backend, err := newBackendFromIdentity(ident, sc.Backend, logger)
	if err != nil {
		logger.Warn("lambda_microvm: identity verified but launch config incomplete; MicroVM backend unavailable",
			"launcher_role", sc.Identity.LauncherRoleARN, "region", sc.Identity.Region, "error", err)
		return nil, err
	}
	// Record the launcher account observed from the verified cross-account hop
	// so C5 can persist runtime_account on every agent bead it launches.
	backend.SetLauncherAccount(caller.Account)

	// Per-run Ziti identity issuer (ka-0000001924). Fail closed: when the
	// broker is configured but the client cannot be built, the whole backend
	// stays unavailable — a per-run-identity deployment must never silently
	// degrade to launching on the shared static identity. The proof is signed
	// with the identity's ORIGIN credentials (the dedicated controller role
	// when configured — the identity the broker's consumer policy pins,
	// ka-0000006814), never a separately resolved default chain.
	if sc.Broker.Enabled() {
		issuer, err := newIssuer(ctx, sc.Broker, ident.OriginCredentials(), logger)
		if err != nil {
			logger.Warn("lambda_microvm: ziti-access-broker configured but client construction failed; MicroVM backend unavailable (fail closed)",
				"broker_url", sc.Broker.URL, "error", err)
			return nil, err
		}
		backend.SetIssuer(issuer)
	}

	logger.Info("lambda_microvm: backend ready",
		"launcher_role", sc.Identity.LauncherRoleARN, "region", backend.cfg.Region,
		"assumed_arn", caller.ARN, "assumed_account", caller.Account,
		"image", backend.cfg.ImageIdentifier, "image_version", backend.cfg.ImageVersion,
		"per_run_identity", backend.PerRunIdentityEnabled(),
		"has_static_identity", backend.cfg.StaticIdentitySecretARN != "",
		"log_group", backend.cfg.LogGroup, "log_stream_prefix", backend.cfg.LogStreamPrefix,
		"production_mode", backend.cfg.ProductionMode,
		"controller_role", sc.Identity.ControllerRoleARN)
	return backend, nil
}
