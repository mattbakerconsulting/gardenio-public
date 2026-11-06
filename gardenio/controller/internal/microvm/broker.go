package microvm

// Per-run Ziti identity issuance for MicroVM agents (task ka-0000001924,
// against the Network Fabric contract ka-0000001858 / ka-0000006281).
//
// The ziti-access-broker (network-fabric-deployment, Phase 1.5) mints one
// unique short-lived OpenZiti identity per agent run, carrying the role
// attribute `gardenio-microvm-agents`, keyed on the agent bead ID as task_id.
// This client implements the MicroVM consumer side of that contract:
//
//   - POST /lease   — role-scoped lease: {auth: sts-proof, identity_roles,
//     task_id, ttl, enrollment_ttl[, enroll]} → {lease_id, identity_id,
//     enrollment_jwt | identity, expires_at, granted_ttl}
//   - DELETE /lease?task_id=… — idempotent revoke of ALL leases for the task
//     (204 including already-gone), fired from every exit path.
//
// Caller verification is the contract's Extension A: a sigv4-signed
// sts:GetCallerIdentity request the broker forwards to STS verbatim and
// matches against its aws_iam_role consumer policy. The proof is signed with
// the controller's OWN identity (IRSA web-identity in prod) — NEVER the
// assumed launcher role — because the consumer policy pins the controller's
// role ARN (contract §2/§8). A broker-specific bound header
// (X-Ziti-Broker-Server-Id) rides inside the signed headers so a proof
// captured for another service cannot be replayed here.
//
// Identity material handling (contract §3): the lease response's material is
// shaped into the per-run secret VALUE by IdentitySecretValue — either the
// enrollment JWT (Option A, preferred: the VM enrolls at boot and the private
// key never leaves it) or the broker-enrolled identity JSON verbatim
// (Option B fallback, zero image change). The material is never logged.

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	v4 "github.com/aws/aws-sdk-go-v2/aws/signer/v4"
	awsconfig "github.com/aws/aws-sdk-go-v2/config"
)

// brokerServerIDHeader is the broker-specific bound header included in the
// signed STS request so a signed proof captured for another service cannot be
// replayed against the broker (the Vault X-Vault-AWS-IAM-Server-ID defense;
// contract §2 Extension A rule 3).
const brokerServerIDHeader = "X-Ziti-Broker-Server-Id"

// brokerOpTimeout bounds each broker HTTP call so a wedged broker never blocks
// the spawn door (issuance) or teardown (revocation) indefinitely.
const brokerOpTimeout = 15 * time.Second

// defaultIdentityRole is the Ziti role attribute minted identities carry when
// the chart does not override it. The four production service policies (beads /
// coopmux / NATS / LiteLLM Dial) grant #gardenio-microvm-agents, so the identity
// inherits all four by attribute (contract §2 Extension B).
const defaultIdentityRole = "gardenio-microvm-agents"

// defaultEnrollmentTTL bounds the one-time enrollment JWT independently of the
// identity lifetime. Enrollment happens within minutes of launch, so a short
// OTT window limits how long a stolen-but-unconsumed JWT is useful.
const defaultEnrollmentTTL = 15 * time.Minute

// leaseTTLSlack is added on top of the MicroVM's MaximumDurationInSeconds when
// sizing the identity lease, so the identity comfortably outlives the VM plus
// one reconcile pass (contract §4: "ttl: maxDuration+15m"). The broker TTL
// reaper then bounds a controller crash.
const leaseTTLSlack = 15 * time.Minute

// stsGetCallerIdentityBody is the canonical form body of the signed proof
// request the broker forwards to STS.
const stsGetCallerIdentityBody = "Action=GetCallerIdentity&Version=2011-06-15"

// BrokerOptions configures the ziti-access-broker client. All fields are
// sourced from config.Config (MICROVM_ZITI_BROKER_* env set by the Helm chart).
type BrokerOptions struct {
	// URL is the broker base endpoint (e.g. "http://ziti-access-broker.fabric:8080").
	// Empty disables per-run identity issuance entirely — the backend falls back
	// to the static POC identity path unchanged.
	URL string

	// ServerID is the value of the bound X-Ziti-Broker-Server-Id header inside
	// the signed STS proof. It must match the broker's configured server id.
	// Empty defaults to the broker URL's host.
	ServerID string

	// IdentityRoles are the Ziti role attributes requested on every minted
	// identity. Empty defaults to [gardenio-microvm-agents]. The broker validates
	// each against its consumer-policy allow-list.
	IdentityRoles []string

	// Enroll requests broker-side enrollment (contract Option B): the broker
	// enrolls the identity itself and returns the full identity JSON, written
	// verbatim as the per-run secret (zero image change, but the private key
	// transits broker+controller memory). False (default) uses Option A: the
	// broker returns a one-time enrollment JWT and the VM enrolls at boot, so
	// the private key never exists outside the VM.
	Enroll bool

	// EnrollmentTTL bounds the one-time enrollment JWT (contract
	// `enrollment_ttl`), independent of the identity lease TTL. Zero defaults
	// to 15m. Ignored by brokers that keep expiresAt = now + grantedTTL.
	EnrollmentTTL time.Duration
}

// Enabled reports whether per-run identity issuance is configured. When false
// the MicroVM backend keeps the static POC identity path byte-identical.
func (o BrokerOptions) Enabled() bool { return strings.TrimSpace(o.URL) != "" }

// Lease is the broker's response to a successful POST /lease: the minted
// identity's ids plus exactly one delivery shape of its material.
type Lease struct {
	LeaseID    string `json:"lease_id"`
	IdentityID string `json:"identity_id"`

	// EnrollmentJWT is the one-time enrollment token (Option A). The VM
	// consumes it at boot; it is useless after enrollment or expiry.
	EnrollmentJWT string `json:"enrollment_jwt"`

	// Identity is the broker-enrolled identity JSON (Option B, only when the
	// lease was requested with enroll:true). Written verbatim as the per-run
	// secret — the same shape as the static POC secret.
	Identity json.RawMessage `json:"identity"`

	ExpiresAt  string `json:"expires_at"`
	GrantedTTL string `json:"granted_ttl"`
}

// IdentitySecretValue shapes a lease's identity material into the per-run
// secret VALUE the MicroVM image reads (contract §3): the enrolled identity
// JSON verbatim when present (Option B — today's static-secret shape), else
// the enrollment-JWT document {"type":"ziti_enrollment_jwt","jwt":…} the
// image enrolls at boot (Option A). Errors when the lease carries neither, so
// the launch fails closed instead of booting a VM that can never enroll.
func IdentitySecretValue(l *Lease) (string, error) {
	if l == nil {
		return "", errors.New("microvm: nil lease")
	}
	// A JSON round trip leaves an absent identity as the literal "null"
	// RawMessage — treat it as absent, not as material.
	if id := strings.TrimSpace(string(l.Identity)); id != "" && id != "null" {
		return id, nil
	}
	if strings.TrimSpace(l.EnrollmentJWT) != "" {
		body, err := json.Marshal(struct {
			Type string `json:"type"`
			JWT  string `json:"jwt"`
		}{Type: "ziti_enrollment_jwt", JWT: l.EnrollmentJWT})
		if err != nil {
			return "", fmt.Errorf("microvm: marshal enrollment-jwt secret: %w", err)
		}
		return string(body), nil
	}
	return "", errors.New("microvm: lease carried neither enrollment_jwt nor identity material")
}

// identityIssuer is the per-run identity seam the Backend drives. Satisfied by
// *BrokerClient; narrowed to an interface so backend/lifecycle tests can fake
// issuance and revocation without HTTP or a credential chain.
type identityIssuer interface {
	// IssueLease mints a fresh role-scoped identity lease for the task. It
	// must be treated fail-closed by callers: an error means NO launch.
	IssueLease(ctx context.Context, taskID string, ttl time.Duration) (*Lease, error)

	// RevokeLeases revokes ALL leases for the task (disable+delete identity).
	// Idempotent: revoking a task with no leases is a success.
	RevokeLeases(ctx context.Context, taskID string) error
}

// BrokerClient is the ziti-access-broker HTTP client. It holds the
// controller's OWN AWS credentials (never the launcher role) for signing the
// caller-verification proof, and is safe for concurrent use.
type BrokerClient struct {
	opts      BrokerOptions
	creds     aws.CredentialsProvider
	stsRegion string
	http      *http.Client
	logger    *slog.Logger
}

// compile-time assertion that BrokerClient implements the issuer seam.
var _ identityIssuer = (*BrokerClient)(nil)

// NewBrokerClient builds the broker client from the controller's launcher-hop
// ORIGIN credentials (Identity.OriginCredentials — the dedicated controller
// role in production, the default IRSA chain in POC/rollback). That is the
// identity the broker's consumer policy pins (contract §2), so the proof must
// be signed with exactly these credentials: never the assumed launcher-role
// credentials (the proof must attest the controller, not the Fabric
// launcher), and never a separately resolved default chain (which attests the
// shared agent role even when the dedicated controller role is configured —
// ka-0000006814). Fails fast (bounded by configLoadTimeout) so a broken
// credential chain surfaces at startup rather than on the first launch.
func NewBrokerClient(ctx context.Context, opts BrokerOptions, creds aws.CredentialsProvider, logger *slog.Logger) (*BrokerClient, error) {
	if !opts.Enabled() {
		return nil, errors.New("microvm: broker client requires a broker URL")
	}
	if creds == nil {
		return nil, errors.New("microvm: broker client requires the controller's origin credentials")
	}
	if logger == nil {
		logger = slog.Default()
	}

	// The default config is loaded only to resolve the STS region for the
	// proof endpoint — the signing credentials come from the caller.
	loadCtx, cancel := context.WithTimeout(ctx, configLoadTimeout)
	defer cancel()
	base, err := awsconfig.LoadDefaultConfig(loadCtx)
	if err != nil {
		return nil, fmt.Errorf("microvm: load controller AWS config for broker proof: %w", err)
	}
	stsRegion := base.Region
	if stsRegion == "" {
		stsRegion = "us-east-1"
	}
	return newBrokerClientWithCreds(opts, creds, stsRegion, logger)
}

// newBrokerClientWithCreds is the test seam: it builds a client from an
// already-resolved credentials provider, bypassing the AWS config chain.
func newBrokerClientWithCreds(opts BrokerOptions, creds aws.CredentialsProvider, stsRegion string, logger *slog.Logger) (*BrokerClient, error) {
	opts.URL = strings.TrimRight(strings.TrimSpace(opts.URL), "/")
	if opts.URL == "" {
		return nil, errors.New("microvm: broker client requires a broker URL")
	}
	if creds == nil {
		return nil, errors.New("microvm: broker client requires a credentials provider")
	}
	if strings.TrimSpace(opts.ServerID) == "" {
		u, err := url.Parse(opts.URL)
		if err != nil || u.Host == "" {
			return nil, fmt.Errorf("microvm: cannot derive broker server id from URL %q", opts.URL)
		}
		opts.ServerID = u.Host
	}
	if len(opts.IdentityRoles) == 0 {
		opts.IdentityRoles = []string{defaultIdentityRole}
	}
	if opts.EnrollmentTTL <= 0 {
		opts.EnrollmentTTL = defaultEnrollmentTTL
	}
	if logger == nil {
		logger = slog.Default()
	}
	return &BrokerClient{
		opts:      opts,
		creds:     creds,
		stsRegion: stsRegion,
		http:      &http.Client{Timeout: brokerOpTimeout},
		logger:    logger,
	}, nil
}

// stsRequestProof is the signed sts:GetCallerIdentity request material the
// broker executes verbatim against STS to authenticate the caller.
type stsRequestProof struct {
	URL     string            `json:"url"`
	Body    string            `json:"body"`
	Headers map[string]string `json:"headers"`
}

// authProof is the caller-verification envelope on every broker call.
type authProof struct {
	Kind       string          `json:"kind"`
	STSRequest stsRequestProof `json:"sts_request"`
}

// leaseRequest is the POST /lease body for a role-scoped lease (contract §2
// Extension B). Mutually exclusive with the K8s pod path's service_name shape.
type leaseRequest struct {
	Auth          authProof `json:"auth"`
	IdentityRoles []string  `json:"identity_roles"`
	TaskID        string    `json:"task_id"`
	TTL           string    `json:"ttl"`
	EnrollmentTTL string    `json:"enrollment_ttl,omitempty"`
	Enroll        bool      `json:"enroll,omitempty"`
}

// buildAuthProof sigv4-signs a fresh sts:GetCallerIdentity request with the
// controller's own credentials and serializes it for the broker. The bound
// server-id header is set BEFORE signing so it is covered by the signature.
// STS enforces a ~15-minute signature window, so proofs are built per call,
// never cached.
func (c *BrokerClient) buildAuthProof(ctx context.Context) (*authProof, error) {
	stsURL := fmt.Sprintf("https://sts.%s.amazonaws.com/", c.stsRegion)
	req, err := http.NewRequest(http.MethodPost, stsURL, strings.NewReader(stsGetCallerIdentityBody))
	if err != nil {
		return nil, fmt.Errorf("microvm: build sts proof request: %w", err)
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded; charset=utf-8")
	req.Header.Set(brokerServerIDHeader, c.opts.ServerID)

	sum := sha256.Sum256([]byte(stsGetCallerIdentityBody))
	payloadHash := hex.EncodeToString(sum[:])

	creds, err := c.creds.Retrieve(ctx)
	if err != nil {
		return nil, fmt.Errorf("microvm: resolve controller credentials for broker proof: %w", err)
	}
	if err := v4.NewSigner().SignHTTP(ctx, creds, req, payloadHash, "sts", c.stsRegion, time.Now().UTC()); err != nil {
		return nil, fmt.Errorf("microvm: sign sts proof: %w", err)
	}

	// Host is signed from the URL, not stored in req.Header — serialize it
	// explicitly so the broker can reconstruct the exact signed request.
	headers := map[string]string{"Host": req.URL.Host}
	for k, vs := range req.Header {
		headers[k] = strings.Join(vs, ",")
	}
	return &authProof{
		Kind:       "aws_sts_get_caller_identity",
		STSRequest: stsRequestProof{URL: stsURL, Body: stsGetCallerIdentityBody, Headers: headers},
	}, nil
}

// IssueLease mints a fresh role-scoped identity lease for taskID (the agent
// bead ID). ttl must cover the MicroVM's maximum duration plus reconcile slack;
// the broker clamps it to the policy max_ttl. Every call mints a NEW identity —
// the client never pre-revokes, because a duplicate spawn event for a live VM
// (idempotent RunMicrovm) must not sever that VM's existing enrollment; stale
// extra leases are swept by the task-keyed revoke on every exit path.
//
// Callers MUST treat an error as fail-closed: no launch happens without a
// freshly minted identity (contract §5.3 — never launch on a shared or stale
// identity because issuance failed).
func (c *BrokerClient) IssueLease(ctx context.Context, taskID string, ttl time.Duration) (*Lease, error) {
	taskID = strings.TrimSpace(taskID)
	if taskID == "" {
		return nil, errors.New("microvm: IssueLease requires a task id")
	}
	if ttl <= 0 {
		return nil, fmt.Errorf("microvm: IssueLease requires a positive ttl, got %s", ttl)
	}

	callCtx, cancel := context.WithTimeout(ctx, brokerOpTimeout)
	defer cancel()

	proof, err := c.buildAuthProof(callCtx)
	if err != nil {
		return nil, err
	}
	body, err := json.Marshal(leaseRequest{
		Auth:          *proof,
		IdentityRoles: c.opts.IdentityRoles,
		TaskID:        taskID,
		TTL:           ttl.String(),
		EnrollmentTTL: c.opts.EnrollmentTTL.String(),
		Enroll:        c.opts.Enroll,
	})
	if err != nil {
		return nil, fmt.Errorf("microvm: marshal lease request: %w", err)
	}

	req, err := http.NewRequestWithContext(callCtx, http.MethodPost, c.opts.URL+"/lease", bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("microvm: build lease request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := c.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("microvm: broker unreachable at %s: %w", c.opts.URL, err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("microvm: broker lease for %s returned http %d: %s (401=proof/replay, 403=role policy/subject, 409=lease conflict)",
			taskID, resp.StatusCode, snippet(resp.Body))
	}
	var lease Lease
	if err := json.NewDecoder(resp.Body).Decode(&lease); err != nil {
		return nil, fmt.Errorf("microvm: decode lease response for %s: %w", taskID, err)
	}
	// Validate the material now so the failure surfaces as "issuance failed"
	// (abort launch) rather than a half-launched VM that can never enroll.
	if _, err := IdentitySecretValue(&lease); err != nil {
		return nil, fmt.Errorf("microvm: broker lease for %s unusable: %w", taskID, err)
	}
	c.logger.Info("microvm: per-run ziti identity minted",
		"task_id", taskID, "lease_id", lease.LeaseID, "identity_id", lease.IdentityID,
		"expires_at", lease.ExpiresAt, "granted_ttl", lease.GrantedTTL,
		"broker_enrolled", len(lease.Identity) > 0)
	return &lease, nil
}

// RevokeLeases revokes ALL leases (disable+delete identities) for taskID via
// the idempotent DELETE /lease?task_id=… (contract §4: 204 including
// already-gone). Fired from all four exit paths — normal termination, timeout
// reconcile-close, failed-launch rollback, and leak GC — so idempotency here
// is what makes those paths safe to overlap. The DELETE carries the same
// signed STS proof as the mint, in the request body.
func (c *BrokerClient) RevokeLeases(ctx context.Context, taskID string) error {
	taskID = strings.TrimSpace(taskID)
	if taskID == "" {
		return nil
	}

	callCtx, cancel := context.WithTimeout(ctx, brokerOpTimeout)
	defer cancel()

	proof, err := c.buildAuthProof(callCtx)
	if err != nil {
		return err
	}
	body, err := json.Marshal(struct {
		Auth authProof `json:"auth"`
	}{Auth: *proof})
	if err != nil {
		return fmt.Errorf("microvm: marshal revoke request: %w", err)
	}

	endpoint := c.opts.URL + "/lease?task_id=" + url.QueryEscape(taskID)
	req, err := http.NewRequestWithContext(callCtx, http.MethodDelete, endpoint, bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("microvm: build revoke request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := c.http.Do(req)
	if err != nil {
		return fmt.Errorf("microvm: broker unreachable for revoke of %s: %w", taskID, err)
	}
	defer resp.Body.Close()

	// 204 = revoked or already gone (idempotent). Tolerate 404 from brokers
	// that report no-such-lease explicitly — the identity is equally gone.
	if resp.StatusCode != http.StatusNoContent && resp.StatusCode != http.StatusNotFound {
		return fmt.Errorf("microvm: broker revoke for %s returned http %d: %s", taskID, resp.StatusCode, snippet(resp.Body))
	}
	c.logger.Info("microvm: per-run ziti identity leases revoked", "task_id", taskID)
	return nil
}

// snippet reads a short prefix of an error response body for diagnostics.
// Broker error bodies are plain error messages, never identity material.
func snippet(r io.Reader) string {
	b, _ := io.ReadAll(io.LimitReader(r, 512))
	return strings.TrimSpace(string(b))
}
