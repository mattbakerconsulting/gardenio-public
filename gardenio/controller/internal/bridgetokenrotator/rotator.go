// Package bridgetokenrotator periodically rotates bridge tokens before
// their TTL expires.
//
// Lifecycle per fleet (slack-bridge, gitlab-bridge, jira-bridge):
//  1. GET /v1/fleet-tokens?detailed=true → daemon returns each fleet's
//     expires_at + subject.
//  2. If a fleet's token has < threshold (default 14d) until expiry, the
//     rotator mints a fresh access+refresh pair via
//     POST /v1/fleet-tokens (with_refresh=true), preserving the existing
//     bridge:<name> subject and TTL=720h.
//  3. The rotator patches the K8s Secret (default bridge-fleet-tokens),
//     replacing the per-fleet key (and the *-refresh key if present) plus
//     the fleet-map env entry.
//  4. The rotator patches the bridge Deployment with a fleet-token-rotated-at
//     annotation, causing a rolling restart so the pods pick up the new
//     credential. Bridges read BEADS_DAEMON_TOKEN at startup — no SIGHUP
//     handler is required.
//
// All writes are gated behind the master Enabled switch AND the DryRun flag.
// When DryRun=true the rotator logs "would rotate" decisions and exits the
// Rotate call without touching K8s. Master switch defaults to OFF; rollout
// goes preprod-dry-run → preprod-live → 7d soak → operator-gated prod.
package bridgetokenrotator

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"log/slog"
	"strings"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"

	"gardenio/controller/internal/beadsapi"
)

// DefaultAccessTTL is the access-token TTL the rotator requests for bridge
// tokens. Matches the initial hardening rollout (ka-0000002241).
const DefaultAccessTTL = 720 * time.Hour

// DefaultRefreshTTL is the companion refresh-token TTL minted alongside each
// access token. Bridges do not currently use the refresh-token flow at
// runtime (ka-0000002353 design Q3) but the daemon-side row is the lifecycle
// anchor for the refresh-token cascade, so we mint one to keep the surface
// consistent with agent-pod provisioning.
const DefaultRefreshTTL = 60 * 24 * time.Hour

// RotationAnnotation is the deployment-pod-template annotation the rotator
// stamps to trigger a rolling restart after rotating a fleet's token.
const RotationAnnotation = "gardenio.app/fleet-token-rotated-at"

// BridgeClient is the subset of beadsapi.Client the rotator depends on.
// Narrowing the surface keeps fake-client tests small.
type BridgeClient interface {
	ListBridgeTokensDetailed(ctx context.Context) ([]beadsapi.BridgeTokenInfo, error)
	RegisterBridgeTokenWithRefresh(ctx context.Context, token, fleet, project, subject string, accessTTL, refreshTTL time.Duration, privileged bool) (*beadsapi.BridgeTokenWithRefresh, error)
}

// BridgeSpec is one fleet the rotator manages. Subject overrides the default
// bridge:<fleet-without--bridge-suffix> attribution when set.
type BridgeSpec struct {
	Fleet          string
	Subject        string // empty → derive from Fleet (slack-bridge → bridge:slack)
	DeploymentName string // empty → default to Fleet (slack-bridge → slack-bridge)
	// Privileged marks this fleet's rotated tokens as a privileged
	// service-fleet identity that may bypass per-bead ownership for
	// system-managed graph writes (produces/triggered-by deps, mr_url
	// back-fill). Set for the gitlab-bridge (and any other bridge that does
	// cross-owner graph writes). See ka-0000001946.
	Privileged bool
}

// Config wires the rotator. Empty values are filled with package-level
// defaults at Validate time.
type Config struct {
	Enabled     bool          // master switch; false → RunLoop is a no-op
	DryRun      bool          // true → Rotate logs but does not write
	Interval    time.Duration // check cadence; default 1h
	Threshold   time.Duration // rotate when remaining < threshold; default 14d
	AccessTTL   time.Duration // requested access-token TTL; default 720h
	RefreshTTL  time.Duration // requested refresh-token TTL; default 60d
	Namespace   string        // K8s namespace for secret + deployments
	SecretName  string        // K8s Secret name (default bridge-fleet-tokens)
	FleetMapKey string        // Secret key that holds BEADS_FLEET_MAP; default fleet-map
	Fleets      []BridgeSpec  // fleets to rotate
}

// Rotator implements the periodic rotation loop.
type Rotator struct {
	cfg     Config
	daemon  BridgeClient
	k8s     kubernetes.Interface
	logger  *slog.Logger
	metrics *Metrics
	now     func() time.Time
	// generateToken mints a fresh access-token plaintext. Defaults to 32
	// hex bytes from crypto/rand. Test-overridable.
	generateToken func() (string, error)
}

// New constructs a Rotator with defaults applied. Returns an error when the
// config is unusable (no fleets, no k8s client, etc.).
func New(cfg Config, daemon BridgeClient, k8s kubernetes.Interface, logger *slog.Logger) (*Rotator, error) {
	if logger == nil {
		logger = slog.Default()
	}
	if cfg.Interval <= 0 {
		cfg.Interval = time.Hour
	}
	if cfg.Threshold <= 0 {
		cfg.Threshold = 14 * 24 * time.Hour
	}
	if cfg.AccessTTL <= 0 {
		cfg.AccessTTL = DefaultAccessTTL
	}
	if cfg.RefreshTTL <= 0 {
		cfg.RefreshTTL = DefaultRefreshTTL
	}
	if cfg.SecretName == "" {
		cfg.SecretName = "bridge-fleet-tokens"
	}
	if cfg.FleetMapKey == "" {
		cfg.FleetMapKey = "fleet-map"
	}
	if cfg.Enabled {
		if daemon == nil {
			return nil, fmt.Errorf("rotator enabled but daemon client is nil")
		}
		if !cfg.DryRun && k8s == nil {
			return nil, fmt.Errorf("rotator enabled in write mode but k8s client is nil")
		}
		if len(cfg.Fleets) == 0 {
			return nil, fmt.Errorf("rotator enabled but no fleets configured")
		}
		if cfg.Namespace == "" {
			return nil, fmt.Errorf("rotator enabled but namespace is empty")
		}
	}
	return &Rotator{
		cfg:           cfg,
		daemon:        daemon,
		k8s:           k8s,
		logger:        logger.With("component", "fleet-token-rotator"),
		now:           time.Now,
		generateToken: defaultGenerateToken,
	}, nil
}

func defaultGenerateToken() (string, error) {
	buf := make([]byte, 32)
	if _, err := rand.Read(buf); err != nil {
		return "", fmt.Errorf("generating token: %w", err)
	}
	return hex.EncodeToString(buf), nil
}

// SetNowFunc overrides the clock for tests.
func (r *Rotator) SetNowFunc(f func() time.Time) {
	if f != nil {
		r.now = f
	}
}

// SetMetrics attaches the Prometheus metrics sink. The controller constructs
// and registers the Metrics against its promRegistry, then hands it here.
// Nil-safe: with no metrics set the rotator emits no series (the observe*
// helpers short-circuit), so tests and the disabled path stay allocation-free.
func (r *Rotator) SetMetrics(m *Metrics) {
	r.metrics = m
}

// RunLoop checks immediately then every Interval until ctx is cancelled.
// When Enabled=false the loop is a no-op.
func (r *Rotator) RunLoop(ctx context.Context) {
	if !r.cfg.Enabled {
		r.logger.Info("fleet-token rotator disabled")
		return
	}
	r.logger.Info("fleet-token rotator starting",
		"interval", r.cfg.Interval,
		"threshold", r.cfg.Threshold,
		"dry_run", r.cfg.DryRun,
		"namespace", r.cfg.Namespace,
		"secret", r.cfg.SecretName,
		"fleets", fleetNames(r.cfg.Fleets),
	)
	r.tick(ctx)
	ticker := time.NewTicker(r.cfg.Interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			r.tick(ctx)
		}
	}
}

func fleetNames(fs []BridgeSpec) []string {
	out := make([]string, 0, len(fs))
	for _, f := range fs {
		out = append(out, f.Fleet)
	}
	return out
}

// tick performs one rotation check across all configured fleets.
func (r *Rotator) tick(ctx context.Context) {
	candidates, err := r.Check(ctx)
	if err != nil {
		r.logger.Warn("fleet-token rotator check failed", "error", err)
		return
	}
	if len(candidates) == 0 {
		return
	}
	for _, c := range candidates {
		if err := r.Rotate(ctx, c); err != nil {
			r.logger.Warn("fleet-token rotator rotate failed",
				"fleet", c.Fleet, "error", err)
			r.metrics.observeRotation(c.Fleet.Fleet, rotationResultError)
			continue
		}
		// Rotate counts the dry_run result itself (it returns nil without
		// writing); only a real, successful write is counted here so the two
		// results never double-count.
		if !r.cfg.DryRun {
			r.metrics.observeRotation(c.Fleet.Fleet, rotationResultSuccess)
		}
	}
}

// Candidate represents one fleet that needs rotation now.
type Candidate struct {
	Fleet     BridgeSpec
	ExpiresAt time.Time // zero means daemon reports no expiry (legacy seed token)
	Remaining time.Duration
}

// Check lists the daemon's per-fleet token state and returns the fleets
// whose remaining TTL is under the configured threshold. Pure function over
// daemon state — no writes. Tokens with zero ExpiresAt (legacy seed/admin)
// are always treated as rotation candidates so the bridges migrate onto the
// proper TTL-bearing rails.
func (r *Rotator) Check(ctx context.Context) ([]Candidate, error) {
	infos, err := r.daemon.ListBridgeTokensDetailed(ctx)
	if err != nil {
		// Leave the expiry gauges STALE (do not zero) so a transient daemon
		// blip cannot trip the overdue alert; count the failed check.
		r.metrics.observeCheckError()
		return nil, err
	}
	byFleet := make(map[string]beadsapi.BridgeTokenInfo, len(infos))
	for _, info := range infos {
		byFleet[info.Fleet] = info
	}
	now := r.now()
	var candidates []Candidate
	for _, spec := range r.cfg.Fleets {
		info, ok := byFleet[spec.Fleet]
		if !ok {
			r.logger.Warn("fleet-token rotator: configured fleet not registered on daemon",
				"fleet", spec.Fleet)
			continue
		}
		remaining := time.Duration(0)
		needsRotation := false
		if info.ExpiresAt.IsZero() {
			// Legacy/seed token without TTL — always rotate onto the
			// proper TTL-bearing rails.
			needsRotation = true
		} else {
			remaining = info.ExpiresAt.Sub(now)
			if remaining < r.cfg.Threshold {
				needsRotation = true
			}
		}
		// Publish the expiry series for EVERY configured fleet on every
		// successful check — not just candidates — so BridgeTokenRotationOverdue
		// evaluates each fleet's true remaining TTL regardless of whether the
		// rotator chose to act. A zero ExpiresAt is published as epoch (0),
		// which reads as maximally overdue by design.
		r.metrics.observeExpiry(spec.Fleet, expiresAtUnix(info.ExpiresAt), remaining.Seconds())
		if !needsRotation {
			continue
		}
		candidates = append(candidates, Candidate{
			Fleet:     spec,
			ExpiresAt: info.ExpiresAt,
			Remaining: remaining,
		})
	}
	// Successful check: stamp the deadman so a subsequently-frozen loop is
	// distinguishable from a healthy idle one.
	r.metrics.observeCheckOK(float64(now.Unix()))
	return candidates, nil
}

// expiresAtUnix converts a token expiry to Unix seconds for the metric, mapping
// a zero time to 0 (epoch) so an un-TTL'd token reads as maximally overdue.
func expiresAtUnix(t time.Time) float64 {
	if t.IsZero() {
		return 0
	}
	return float64(t.Unix())
}

// Rotate mints a fresh token+refresh pair for the given fleet, patches the
// K8s secret, and triggers a rolling restart of the bridge deployment.
// When DryRun=true the function logs the planned action and returns nil.
func (r *Rotator) Rotate(ctx context.Context, c Candidate) error {
	subject := c.Fleet.Subject
	if subject == "" {
		subject = deriveSubject(c.Fleet.Fleet)
	}
	logArgs := []any{
		"fleet", c.Fleet.Fleet,
		"subject", subject,
		"privileged", c.Fleet.Privileged,
		"expires_at", formatTime(c.ExpiresAt),
		"remaining", c.Remaining.Round(time.Second).String(),
		"access_ttl", r.cfg.AccessTTL.String(),
		"refresh_ttl", r.cfg.RefreshTTL.String(),
	}
	if r.cfg.DryRun {
		r.logger.Info("fleet-token rotator: would rotate (dry-run)", logArgs...)
		r.metrics.observeRotation(c.Fleet.Fleet, rotationResultDryRun)
		return nil
	}
	r.logger.Info("fleet-token rotator: rotating fleet token", logArgs...)

	plaintext, err := r.generateToken()
	if err != nil {
		return fmt.Errorf("mint token plaintext: %w", err)
	}
	res, err := r.daemon.RegisterBridgeTokenWithRefresh(
		ctx, plaintext, c.Fleet.Fleet, "", subject, r.cfg.AccessTTL, r.cfg.RefreshTTL, c.Fleet.Privileged,
	)
	if err != nil {
		return fmt.Errorf("register fleet token: %w", err)
	}
	refreshToken := ""
	if res != nil {
		refreshToken = res.RefreshToken
	}

	if err := r.patchSecret(ctx, c.Fleet.Fleet, plaintext, refreshToken); err != nil {
		return fmt.Errorf("patch secret: %w", err)
	}
	if err := r.bounceDeployment(ctx, c.Fleet); err != nil {
		return fmt.Errorf("bounce deployment: %w", err)
	}
	r.logger.Info("fleet-token rotator: rotation complete",
		"fleet", c.Fleet.Fleet, "subject", subject)
	return nil
}

// patchSecret updates the per-fleet keys in the bridge-fleet-tokens Secret.
// The Secret is created with K8s' apply semantics — Get → mutate → Update.
// Conflict on Update would be rare (only the rotator writes here) so we keep
// it simple; a retry-on-conflict can be added if it becomes an issue.
func (r *Rotator) patchSecret(ctx context.Context, fleet, access, refresh string) error {
	secrets := r.k8s.CoreV1().Secrets(r.cfg.Namespace)
	sec, err := secrets.Get(ctx, r.cfg.SecretName, metav1.GetOptions{})
	if err != nil {
		if apierrors.IsNotFound(err) {
			return fmt.Errorf("secret %s/%s not found — operator must seed it before rotation", r.cfg.Namespace, r.cfg.SecretName)
		}
		return err
	}
	if sec.Data == nil {
		sec.Data = map[string][]byte{}
	}
	sec.Data[fleet] = []byte(access)
	if refresh != "" {
		sec.Data[fleet+"-refresh"] = []byte(refresh)
	}
	// Refresh the fleet-map env value (still seeded for daemon-side parsing
	// as belt-and-suspenders; see ka-0000002241 comment from operator).
	sec.Data[r.cfg.FleetMapKey] = []byte(rebuildFleetMap(sec.Data, r.cfg.FleetMapKey))

	if _, err := secrets.Update(ctx, sec, metav1.UpdateOptions{}); err != nil {
		return err
	}
	return nil
}

// rebuildFleetMap reconstructs the BEADS_FLEET_MAP value from the per-fleet
// secret keys. We deliberately re-derive rather than parse-and-edit so the
// map stays consistent with the active access tokens.
func rebuildFleetMap(data map[string][]byte, fleetMapKey string) string {
	var parts []string
	for k, v := range data {
		if k == fleetMapKey || strings.HasSuffix(k, "-refresh") {
			continue
		}
		parts = append(parts, string(v)+":"+k)
	}
	return strings.Join(parts, ",")
}

// bounceDeployment stamps the RotationAnnotation onto the bridge
// Deployment's pod template, triggering a rolling restart.
func (r *Rotator) bounceDeployment(ctx context.Context, spec BridgeSpec) error {
	name := spec.DeploymentName
	if name == "" {
		name = spec.Fleet
	}
	deployments := r.k8s.AppsV1().Deployments(r.cfg.Namespace)
	dep, err := deployments.Get(ctx, name, metav1.GetOptions{})
	if err != nil {
		if apierrors.IsNotFound(err) {
			r.logger.Warn("fleet-token rotator: deployment not found, skipping bounce",
				"deployment", name, "namespace", r.cfg.Namespace)
			return nil
		}
		return err
	}
	if dep.Spec.Template.Annotations == nil {
		dep.Spec.Template.Annotations = map[string]string{}
	}
	dep.Spec.Template.Annotations[RotationAnnotation] = r.now().UTC().Format(time.RFC3339)
	if _, err := deployments.Update(ctx, dep, metav1.UpdateOptions{}); err != nil {
		return err
	}
	return nil
}

// deriveSubject maps a fleet name to its default bridge: subject.
//   - slack-bridge  → bridge:slack
//   - gitlab-bridge → bridge:gitlab
//   - jira-bridge   → bridge:jira
//   - other         → bridge:<fleet>
func deriveSubject(fleet string) string {
	name := strings.TrimSuffix(fleet, "-bridge")
	return "bridge:" + name
}

func formatTime(t time.Time) string {
	if t.IsZero() {
		return ""
	}
	return t.UTC().Format(time.RFC3339)
}
