// Bead-scoped S3 session-credential vending (S4 ka-0000006612, epic
// ka-0000007030).
//
// A Lambda MicroVM has NO IRSA: unlike a K8s pod (which reaches
// gardenio-sessions-prod through its projected ServiceAccount web-identity),
// a satellite has no ambient AWS identity, and the MINIMAL execution role
// (S2 ka-0000005082) deliberately grants it NO sessions-bucket access. So the
// controller mints short-lived, BEAD-SCOPED S3 credentials and delivers them
// in-VM via the existing per-run runtime-credential document (backend.go's
// runtimeDocument) — the same vehicle that carries the beads/Claude/git/NATS
// credentials. coop's default AWS credential chain reads them from the
// AWS_ACCESS_KEY_ID/AWS_SECRET_ACCESS_KEY/AWS_SESSION_TOKEN env the image
// materializes from that document.
//
// MINT MECHANISM: sts:AssumeRoleWithWebIdentity on the S2-provisioned
// gardenio-<env>-microvm-session-vendor role, signed with the CONTROLLER pod's
// projected web-identity token (AWS_WEB_IDENTITY_TOKEN_FILE — the vendor
// role's OIDC trust pins exactly the controller SA subject, so only the
// controller can vend), narrowed at mint time by an inline SESSION POLICY to
// coop/sessions/<bead-id>/* only. AssumeRoleWithWebIdentity (not sts:AssumeRole
// from the already-assumed launcher creds) is deliberate: role chaining caps at
// 1h, which cannot cover the up-to-8h VM wall-clock ceiling, whereas the vendor
// role's 12h max_session_duration lets a SINGLE mint cover the whole lifetime —
// including suspend/resume wall-clock — with no refresh machinery (the S2 role
// was sized 12h for exactly this).
//
// LEAST PRIVILEGE + BLAST RADIUS: the vendor role's ceiling already scopes to
// coop/sessions/* CRUD + prefix-scoped ListBucket + s3-ViaService KMS on the s3
// CMK; the per-mint inline session policy INTERSECTS that down to one bead's
// own prefix. A minted credential can therefore only touch the session data of
// the bead it was minted for — never another agent's, never the bucket at
// large, never a direct KMS call path.
//
// TERMINATION: STS session credentials cannot be individually revoked before
// expiry (only an account-wide "revoke sessions issued before <t>" exists,
// which would nuke every agent). Termination instead (a) deletes the per-run
// Secrets Manager document — the ONLY place the credential values live — on
// every exit path (backend.Terminate / timeout / failed-launch / leak-GC), and
// (b) relies on the credential's own short TTL. Because the session policy
// bounds a credential to the terminating bead's own (being-torn-down) session
// prefix, an un-expired copy has effectively no reach. There is thus nothing to
// add to microvmCredentialRevoker — the delivery-vehicle deletion IS the
// revocation, exactly like the git shared-fallback mode.
package microvm

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/sts"
)

// vendor session-duration bounds. STS AssumeRoleWithWebIdentity refuses a
// DurationSeconds below 900s (its hard floor) or above the assumed role's
// MaxSessionDuration. The S2 vendor role is provisioned with
// max_session_duration = 43200 (12h) precisely so one mint spans the whole VM
// lifetime; we clamp to that ceiling so an over-large config value fails safe
// (a shorter mint) rather than an STS ValidationError.
const (
	vendorMinSessionSeconds = 900
	vendorMaxSessionSeconds = 43200

	// defaultSessionCredentialDurationSeconds is the mint lifetime when config
	// leaves it unset: the vendor role's 12h ceiling, so a single mint covers
	// the 8h VM wall-clock ceiling plus suspended wall-clock plus slack with no
	// re-mint on resume.
	defaultSessionCredentialDurationSeconds = 43200
)

// defaultSessionPrefix is coop's own default S3 key prefix
// (coop/crates/cli/src/s3.rs) — used when the deployment sets a sessions bucket
// but no explicit COOP_S3_PREFIX, so the inline session policy's Resource/prefix
// scope matches the keys coop actually reads/writes.
const defaultSessionPrefix = "coop/sessions"

// SessionVendorOptions configures the bead-scoped S3 credential vendor. All
// fields are sourced from config.Config (the MICROVM_* / COOP_S3_* env the Helm
// chart sets).
type SessionVendorOptions struct {
	// RoleARN is the S2-provisioned vendor role the controller assumes via
	// AssumeRoleWithWebIdentity (config MicrovmSessionVendorRoleARN, e.g.
	// arn:aws:iam::123456789012:role/gardenio-prod-microvm-session-vendor).
	// Required — empty disables vending entirely (a satellite then gets no S3
	// session credential, exactly as before S4).
	RoleARN string

	// Region is the region the STS mint targets and the sessions bucket + s3
	// CMK live in (config CoopS3Region, falling back to MicrovmRegion).
	// Required.
	Region string

	// BucketName is the session-persistence bucket the inline policy scopes to
	// (config CoopS3Bucket / chart agents.s3.bucket). Required — empty disables
	// vending (no bucket = no session persistence to scope a credential to).
	BucketName string

	// SessionPrefix is the S3 key prefix sessions live under (config
	// CoopS3Prefix / chart agents.s3.prefix). Empty falls back to coop's own
	// default (coop/sessions). A trailing slash is normalized off.
	SessionPrefix string

	// DurationSeconds is the requested credential lifetime, clamped to
	// [900, 43200] (config MicrovmSessionCredentialDurationSeconds). <=0 uses
	// the 12h default. Must exceed the VM wall-clock ceiling plus expected
	// suspended wall-clock so a single mint needs no refresh.
	DurationSeconds int32
}

// Enabled reports whether the vendor has enough config to mint. The role,
// region, and bucket must all be set; otherwise the vendor is never built and
// S3 session credentials are simply not delivered (default-off, matching the
// MicroVM backend's own enablement discipline).
func (o SessionVendorOptions) Enabled() bool {
	return strings.TrimSpace(o.RoleARN) != "" &&
		strings.TrimSpace(o.Region) != "" &&
		strings.TrimSpace(o.BucketName) != ""
}

// assumeRoleWebIdentityAPI is the one STS call the vendor makes, narrowed so
// tests can inject a fake without reaching AWS. *sts.Client satisfies it.
type assumeRoleWebIdentityAPI interface {
	AssumeRoleWithWebIdentity(ctx context.Context, in *sts.AssumeRoleWithWebIdentityInput, optFns ...func(*sts.Options)) (*sts.AssumeRoleWithWebIdentityOutput, error)
}

// SessionVendor mints bead-scoped S3 session credentials. Constructed once at
// controller startup (when enabled) and safe for concurrent use — the STS
// client is, and the vendor holds no mutable state.
type SessionVendor struct {
	sts       assumeRoleWebIdentityAPI
	tokenFile string // AWS_WEB_IDENTITY_TOKEN_FILE — read fresh per mint

	roleARN         string
	region          string
	bucketName      string
	sessionPrefix   string // no trailing slash
	durationSeconds int32
}

// SessionCredentials is one minted, bead-scoped, short-lived S3 credential set.
// The three secret values ride ONLY the per-run Secrets Manager document; they
// are never logged, never placed in the run-hook payload, image, or TF state.
type SessionCredentials struct {
	AccessKeyID     string
	SecretAccessKey string
	SessionToken    string
	Expiration      time.Time
}

// NewSessionVendor builds the vendor from options, resolving an ANONYMOUS
// region-pinned STS client (AssumeRoleWithWebIdentity is authenticated by the
// OIDC token, not SigV4 — signing with an ambient identity would be wrong and
// could mis-attribute the call). It fails fast, bounded by configLoadTimeout,
// so a broken projected-token / STS setup surfaces at startup rather than on
// the first launch. Returns (nil, nil) when disabled — the caller treats that
// as "no S3 session credential delivered", never an error.
func NewSessionVendor(ctx context.Context, opts SessionVendorOptions) (*SessionVendor, error) {
	if !opts.Enabled() {
		return nil, nil
	}

	tokenFile := strings.TrimSpace(os.Getenv(webIdentityTokenFileEnv))
	if tokenFile == "" {
		// The vendor role's trust requires the controller SA's projected
		// web-identity token; without it the mint can never succeed, so fail
		// closed at startup rather than silently degrading.
		return nil, fmt.Errorf("microvm: session vendor requires %s (the controller pod's projected IRSA token) but it is empty", webIdentityTokenFileEnv)
	}

	region := strings.TrimSpace(opts.Region)
	loadCtx, cancel := context.WithTimeout(ctx, configLoadTimeout)
	defer cancel()
	base, err := awsconfig.LoadDefaultConfig(loadCtx, awsconfig.WithRegion(region))
	if err != nil {
		return nil, fmt.Errorf("microvm: load base AWS config for session vendor: %w", err)
	}
	// Anonymous STS: the web-identity token IS the credential, so this call must
	// not depend on — or be attributable to — the controller's default chain.
	anonSTS := sts.NewFromConfig(base, func(o *sts.Options) {
		o.Credentials = aws.AnonymousCredentials{}
	})

	return newSessionVendorWithClient(anonSTS, tokenFile, opts), nil
}

// newSessionVendorWithClient assembles a vendor around an already-built STS
// client + token-file path, applying prefix normalization and duration
// clamping. Split out so tests can inject a fake STS and a temp token file
// without reaching AWS.
func newSessionVendorWithClient(client assumeRoleWebIdentityAPI, tokenFile string, opts SessionVendorOptions) *SessionVendor {
	prefix := strings.Trim(strings.TrimSpace(opts.SessionPrefix), "/")
	if prefix == "" {
		prefix = defaultSessionPrefix
	}
	return &SessionVendor{
		sts:             client,
		tokenFile:       tokenFile,
		roleARN:         strings.TrimSpace(opts.RoleARN),
		region:          strings.TrimSpace(opts.Region),
		bucketName:      strings.TrimSpace(opts.BucketName),
		sessionPrefix:   prefix,
		durationSeconds: clampSessionDuration(opts.DurationSeconds),
	}
}

// Region is the region the minted credentials + sessions bucket live in, so the
// caller can stamp COOP_S3_REGION for the in-VM coop S3 client.
func (v *SessionVendor) Region() string { return v.region }

// Vend mints a bead-scoped S3 credential set for beadID. It reads the pod's
// projected web-identity token FRESH (the kubelet keeps AWS_WEB_IDENTITY_TOKEN_FILE
// rotated), calls AssumeRoleWithWebIdentity on the vendor role with the inline
// session policy narrowing to coop/sessions/<beadID>/*, and returns the temporary
// credentials. The credential VALUES are never logged.
func (v *SessionVendor) Vend(ctx context.Context, beadID string) (*SessionCredentials, error) {
	beadID = strings.TrimSpace(beadID)
	if beadID == "" {
		return nil, fmt.Errorf("microvm: session vend requires a bead id")
	}

	tokenBytes, err := os.ReadFile(v.tokenFile)
	if err != nil {
		return nil, fmt.Errorf("microvm: read web-identity token for session vend: %w", err)
	}
	token := strings.TrimSpace(string(tokenBytes))
	if token == "" {
		return nil, fmt.Errorf("microvm: web-identity token file %s is empty", v.tokenFile)
	}

	policy, err := beadSessionPolicy(v.bucketName, v.sessionPrefix, beadID)
	if err != nil {
		return nil, fmt.Errorf("microvm: build bead-scoped session policy for %s: %w", beadID, err)
	}

	callCtx, cancel := context.WithTimeout(ctx, awsOpTimeout)
	defer cancel()

	out, err := v.sts.AssumeRoleWithWebIdentity(callCtx, &sts.AssumeRoleWithWebIdentityInput{
		RoleArn:          aws.String(v.roleARN),
		RoleSessionName:  aws.String(sessionNameForBead(beadID)),
		WebIdentityToken: aws.String(token),
		DurationSeconds:  aws.Int32(v.durationSeconds),
		Policy:           aws.String(policy),
	})
	if err != nil {
		return nil, fmt.Errorf("microvm: AssumeRoleWithWebIdentity on session vendor %s for %s: %w", v.roleARN, beadID, err)
	}
	if out.Credentials == nil {
		return nil, fmt.Errorf("microvm: session vend for %s returned no credentials", beadID)
	}
	c := out.Credentials
	if aws.ToString(c.AccessKeyId) == "" || aws.ToString(c.SecretAccessKey) == "" || aws.ToString(c.SessionToken) == "" {
		return nil, fmt.Errorf("microvm: session vend for %s returned incomplete credentials", beadID)
	}
	return &SessionCredentials{
		AccessKeyID:     aws.ToString(c.AccessKeyId),
		SecretAccessKey: aws.ToString(c.SecretAccessKey),
		SessionToken:    aws.ToString(c.SessionToken),
		Expiration:      aws.ToTime(c.Expiration),
	}, nil
}

// clampSessionDuration bounds a requested credential lifetime to STS's
// [900s, role-max] window. <=0 selects the 12h default.
func clampSessionDuration(req int32) int32 {
	if req <= 0 {
		return defaultSessionCredentialDurationSeconds
	}
	if req < vendorMinSessionSeconds {
		return vendorMinSessionSeconds
	}
	if req > vendorMaxSessionSeconds {
		return vendorMaxSessionSeconds
	}
	return req
}

// s3PolicyStatement is one statement of an inline session policy. Condition is
// omitted (via omitempty) for statements that don't scope by prefix.
type s3PolicyStatement struct {
	Sid       string         `json:"Sid"`
	Effect    string         `json:"Effect"`
	Action    []string       `json:"Action"`
	Resource  string         `json:"Resource"`
	Condition map[string]any `json:"Condition,omitempty"`
}

type s3SessionPolicy struct {
	Version   string              `json:"Version"`
	Statement []s3PolicyStatement `json:"Statement"`
}

// beadSessionPolicy builds the inline STS session policy that narrows the
// vendor role's ceiling to exactly one bead's session prefix. Because a session
// policy INTERSECTS the role's permission policy, this need only grant the
// bead-scoped subset:
//
//   - object CRUD on <bucket>/<prefix>/<bead>/*  (transcripts, meta, recordings)
//   - prefix-scoped ListBucket on the bucket for <prefix>/<bead>/*  (coop's
//     spawn-picker + transcript enumeration list under the bead prefix)
//   - KMS Decrypt/Encrypt/GenerateDataKey on * — safe because it is intersected
//     with the role ceiling's specific s3 CMK, ViaService-scoped to S3; the
//     effective grant is therefore that one CMK for sessions-bucket SSE only,
//     never a direct KMS path. Scoping to * here avoids threading the CMK ARN
//     through controller config (which does not otherwise know it).
func beadSessionPolicy(bucketName, sessionPrefix, beadID string) (string, error) {
	bucketARN := "arn:aws:s3:::" + bucketName
	beadPrefix := sessionPrefix + "/" + beadID // e.g. coop/sessions/ka-XXXX

	policy := s3SessionPolicy{
		Version: "2012-10-17",
		Statement: []s3PolicyStatement{
			{
				Sid:      "BeadSessionObjects",
				Effect:   "Allow",
				Action:   []string{"s3:GetObject", "s3:PutObject", "s3:DeleteObject", "s3:AbortMultipartUpload"},
				Resource: bucketARN + "/" + beadPrefix + "/*",
			},
			{
				Sid:      "BeadSessionList",
				Effect:   "Allow",
				Action:   []string{"s3:ListBucket"},
				Resource: bucketARN,
				Condition: map[string]any{
					"StringLike": map[string]any{
						"s3:prefix": []string{beadPrefix + "/*"},
					},
				},
			},
			{
				Sid:      "BeadSessionKMS",
				Effect:   "Allow",
				Action:   []string{"kms:Decrypt", "kms:Encrypt", "kms:GenerateDataKey"},
				Resource: "*",
			},
		},
	}
	body, err := json.Marshal(policy)
	if err != nil {
		return "", fmt.Errorf("marshal session policy: %w", err)
	}
	return string(body), nil
}

// sessionNameForBead derives an STS RoleSessionName from a bead ID. STS allows
// [A-Za-z0-9+=,.@_-] and 2..64 chars; bead IDs (e.g. ka-0000006612) are already
// in-charset, but we sanitize + bound defensively so a non-standard ID can
// never make the mint call fail its own argument validation.
func sessionNameForBead(beadID string) string {
	const prefix = "gardenio-mvm-sess-"
	var b strings.Builder
	b.WriteString(prefix)
	for _, r := range beadID {
		switch {
		case r >= 'A' && r <= 'Z', r >= 'a' && r <= 'z', r >= '0' && r <= '9':
			b.WriteRune(r)
		case r == '+' || r == '=' || r == ',' || r == '.' || r == '@' || r == '_' || r == '-':
			b.WriteRune(r)
		default:
			b.WriteRune('-')
		}
	}
	name := b.String()
	if len(name) > 64 {
		name = name[:64]
	}
	return name
}
