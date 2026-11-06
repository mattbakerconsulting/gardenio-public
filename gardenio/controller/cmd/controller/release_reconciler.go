package main

import (
	"bytes"
	"compress/gzip"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"regexp"
	"strconv"
	"strings"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/metadata"

	"gardenio/controller/internal/beadsapi"
)

// The release reconciler (plan gd-UZKZ9H25UD step 4, releases-design.md §4.3)
// records where gardenio's own release runs when nobody reported it: once a
// minute it reads what the Helm release in this namespace runs (its deployed
// revision and global.imageTag) and, when that differs from the newest
// deployment on the prod target, writes a deployment with by=observed,
// via=reconciler. release-deploy.sh and a helm_deploy escalation's close write
// the same row when they roll, so the reconciler only writes for a manual
// `helm upgrade`, a `helm rollback`, or a roll whose report was lost.
//
// What runs is read from the Helm release Secrets (owner=helm,
// name=<release>, status=deployed): their labels carry the revision, so each
// pass lists metadata only and decodes the release body (config, last
// deployed time) only when the revision changes. Without a readable Helm
// release (no RBAC, a box installed another way) it falls back to the
// controller's own version, with no revision.
//
// A new revision is left to settle (default 10 minutes from when this
// controller first sees it deployed, not from Helm's deploy time, which is
// when a --wait upgrade starts) before it is written, so a scripted roll
// records its own row first and a roll that verify-image-tags.sh rolls back
// is not reported as live.
//
// Env:
//
//	GARDENIO_RELEASE_RECONCILE_INTERVAL  pass interval (default 1m; 0 disables)
//	GARDENIO_RELEASE_PROJECT             project (default gardenio)
//	GARDENIO_RELEASE_TARGET              release target (default prod)
//	GARDENIO_RELEASE_HELM_RELEASE        Helm release name (default gardenio)
//	GARDENIO_RELEASE_HELM_NAMESPACE      its namespace (default: the controller's)
//	GARDENIO_RELEASE_SETTLE              settle time of a new revision (default 10m)
//
// A gardenio-in-a-box also reports what it runs to the home daemon (plan
// gd-UZKZ9H25UD step 4c): with GARDENIO_BOX_NAME and
// GARDENIO_RELEASE_HOME_ADDR set, a second reconciler runs the same passes
// against the home daemon, on target box-<name> (kind box, by box:<name>).
// The home project must declare that target in its release_targets. The box
// authenticates as a Keycloak confidential client of the home realm whose
// service account holds gardenio-machine-release-deploy: the daemon admits
// release writes only from short-lived machine JWTs, so the reporter fetches
// them by client credentials and refreshes them (beadsapi.NewMachineTokenSource),
// never a static token.
//
//	GARDENIO_BOX_NAME                             this box's name
//	GARDENIO_RELEASE_HOME_ADDR                    the home daemon's HTTP address
//	GARDENIO_RELEASE_HOME_PROJECT                 project on the home daemon (default gardenio)
//	GARDENIO_RELEASE_HOME_TOKEN_URL               the home realm's token endpoint
//	GARDENIO_RELEASE_HOME_CLIENT_ID               the box's confidential client
//	GARDENIO_RELEASE_HOME_CLIENT_SECRET_FILE      file holding its client secret
//	GARDENIO_RELEASE_HOME_CA_FILE                 optional PEM CA for the token endpoint

const (
	releaseReconcileDefaultInterval = time.Minute
	releaseReconcileDefaultSettle   = 10 * time.Minute
	releaseObservedBy               = "observed"
	releaseObservedVia              = "reconciler"
)

// observedRelease is what runs on the target: the image tag, the Helm
// revision and when it was deployed. Revision and Since are empty when only
// the controller's own version is known.
type observedRelease struct {
	ImageTag string
	Revision string
	Since    time.Time
}

// helmReleaseReader reads the deployed revision of a Helm release.
type helmReleaseReader interface {
	Deployed(ctx context.Context) (observedRelease, error)
}

// releaseDeployer is the slice of the daemon client the reconciler uses.
type releaseDeployer interface {
	ListReleases(ctx context.Context, project, status string, limit int) ([]beadsapi.Release, error)
	DeployRelease(ctx context.Context, ref, project string, req beadsapi.DeployReleaseRequest) (*beadsapi.Release, error)
}

var secretsGVR = schema.GroupVersionResource{Version: "v1", Resource: "secrets"}

// helmSecrets reads a Helm release from its release Secrets.
type helmSecrets struct {
	meta      metadata.Interface
	core      kubernetes.Interface
	namespace string
	release   string

	cachedName string
	cached     observedRelease
}

// errNoDeployedHelmRelease is a release with no Secret in status deployed.
var errNoDeployedHelmRelease = errors.New("no deployed helm release")

func (h *helmSecrets) Deployed(ctx context.Context) (observedRelease, error) {
	list, err := h.meta.Resource(secretsGVR).Namespace(h.namespace).List(ctx, metav1.ListOptions{
		LabelSelector: "owner=helm,status=deployed,name=" + h.release,
	})
	if err != nil {
		return observedRelease{}, fmt.Errorf("listing helm release secrets: %w", err)
	}
	name, newest := "", -1
	for _, s := range list.Items {
		if v, err := strconv.Atoi(s.Labels["version"]); err == nil && v > newest {
			name, newest = s.Name, v
		}
	}
	if name == "" {
		return observedRelease{}, errNoDeployedHelmRelease
	}
	if name == h.cachedName {
		return h.cached, nil
	}
	sec, err := h.core.CoreV1().Secrets(h.namespace).Get(ctx, name, metav1.GetOptions{})
	if err != nil {
		return observedRelease{}, fmt.Errorf("reading helm release secret %s: %w", name, err)
	}
	obs, err := decodeHelmRelease(sec.Data["release"])
	if err != nil {
		return observedRelease{}, fmt.Errorf("decoding helm release secret %s: %w", name, err)
	}
	if obs.Revision == "" {
		obs.Revision = strconv.Itoa(newest)
	}
	h.cachedName, h.cached = name, obs
	return obs, nil
}

// gzipMagic starts a gzip stream; Helm 3 gzips the release JSON.
var gzipMagic = []byte{0x1f, 0x8b, 0x08}

// decodeHelmRelease reads a Helm 3 release Secret's "release" value: base64 of
// (usually gzipped) release JSON. The image tag is the release's
// global.imageTag, else the chart's appVersion.
func decodeHelmRelease(data []byte) (observedRelease, error) {
	raw, err := base64.StdEncoding.DecodeString(string(data))
	if err != nil {
		return observedRelease{}, fmt.Errorf("base64: %w", err)
	}
	if bytes.HasPrefix(raw, gzipMagic) {
		zr, err := gzip.NewReader(bytes.NewReader(raw))
		if err != nil {
			return observedRelease{}, fmt.Errorf("gzip: %w", err)
		}
		if raw, err = io.ReadAll(zr); err != nil {
			return observedRelease{}, fmt.Errorf("gzip: %w", err)
		}
	}
	var rel struct {
		Version int `json:"version"`
		Info    struct {
			LastDeployed time.Time `json:"last_deployed"`
		} `json:"info"`
		Config struct {
			Global struct {
				ImageTag string `json:"imageTag"`
			} `json:"global"`
		} `json:"config"`
		Chart struct {
			Metadata struct {
				AppVersion string `json:"appVersion"`
			} `json:"metadata"`
		} `json:"chart"`
	}
	if err := json.Unmarshal(raw, &rel); err != nil {
		return observedRelease{}, fmt.Errorf("json: %w", err)
	}
	obs := observedRelease{ImageTag: rel.Config.Global.ImageTag, Since: rel.Info.LastDeployed.UTC()}
	if obs.ImageTag == "" {
		obs.ImageTag = rel.Chart.Metadata.AppVersion
	}
	if rel.Version > 0 {
		obs.Revision = strconv.Itoa(rel.Version)
	}
	return obs, nil
}

// releaseVersionTag is a version that names a release (a calver or semver
// tag), not a dev build.
var releaseVersionTag = regexp.MustCompile(`^(20\d{2}\.\d{1,3}\.\d+|v?\d+\.\d+\.\d+)$`)

type releaseReconciler struct {
	daemon  releaseDeployer
	helm    helmReleaseReader // nil: the controller's own version only
	version string            // the controller's own version
	project string
	target  string
	kind    string // empty: the target's declared kind
	by      string
	settle  time.Duration
	now     func() time.Time
	logger  *slog.Logger

	firstSeen map[string]time.Time // revision@tag -> when this controller first saw it
	lastNote  string               // the last warning logged, so a steady fault logs once
}

func newReleaseReconciler(daemon releaseDeployer, helm helmReleaseReader, version string, getenv func(string) string, logger *slog.Logger) *releaseReconciler {
	r := &releaseReconciler{
		daemon: daemon, helm: helm, version: version,
		project: envOr(getenv, "GARDENIO_RELEASE_PROJECT", "gardenio"),
		target:  envOr(getenv, "GARDENIO_RELEASE_TARGET", "prod"),
		by:      releaseObservedBy,
		settle:  releaseReconcileDefaultSettle,
		now:     time.Now,
		logger:  logger.With("component", "release-reconciler"),

		firstSeen: map[string]time.Time{},
	}
	if d, err := time.ParseDuration(getenv("GARDENIO_RELEASE_SETTLE")); err == nil && d >= 0 {
		r.settle = d
	}
	return r
}

// newBoxReleaseReconciler is the reconciler a box runs against the home
// daemon: target box-<name>, kind box, by box:<name>. It returns nil unless
// GARDENIO_BOX_NAME is set.
func newBoxReleaseReconciler(home releaseDeployer, helm helmReleaseReader, version string, getenv func(string) string, logger *slog.Logger) *releaseReconciler {
	box := strings.TrimSpace(getenv("GARDENIO_BOX_NAME"))
	if box == "" {
		return nil
	}
	r := newReleaseReconciler(home, helm, version, getenv, logger.With("home", true))
	r.project = envOr(getenv, "GARDENIO_RELEASE_HOME_PROJECT", "gardenio")
	r.target, r.kind, r.by = "box-"+box, "box", "box:"+box
	return r
}

func envOr(getenv func(string) string, key, def string) string {
	if v := getenv(key); v != "" {
		return v
	}
	return def
}

// note logs a warning once until a different one (or success) replaces it.
func (r *releaseReconciler) note(msg string, args ...any) {
	key := fmt.Sprint(append([]any{msg}, args...)...)
	if key == r.lastNote {
		return
	}
	r.lastNote = key
	r.logger.Warn(msg, args...)
}

// observe is what runs on the target now: the Helm release, else the
// controller's own version when it names a release.
func (r *releaseReconciler) observe(ctx context.Context) (observedRelease, bool) {
	if r.helm != nil {
		obs, err := r.helm.Deployed(ctx)
		if err == nil && obs.ImageTag != "" {
			return obs, true
		}
		if err != nil {
			r.note("release reconciler: cannot read the helm release; using the controller's own version", "error", err)
		}
	}
	if releaseVersionTag.MatchString(r.version) {
		return observedRelease{ImageTag: r.version}, true
	}
	return observedRelease{}, false
}

// reconcileOnce runs one pass. It reports whether it wrote a deployment.
func (r *releaseReconciler) reconcileOnce(ctx context.Context) bool {
	obs, ok := r.observe(ctx)
	if !ok {
		return false
	}
	now := r.now().UTC()

	key := obs.Revision + "@" + obs.ImageTag
	seen, ok := r.firstSeen[key]
	if !ok {
		seen = now
		r.firstSeen = map[string]time.Time{key: now}
	}
	if now.Sub(seen) < r.settle {
		return false
	}

	live, err := r.daemon.ListReleases(ctx, r.project, beadsapi.ReleaseStatusLive, 0)
	if err != nil {
		r.note("release reconciler: cannot list live releases", "project", r.project, "error", err)
		return false
	}
	var current beadsapi.ReleaseDeployment
	for _, rel := range live {
		for _, d := range rel.Deployments {
			if d.Target == r.target && d.Until == "" {
				current = d
			}
		}
	}
	// The same tag is in sync when either side has no revision: a row written
	// without one (a helm_deploy close, the own-version fallback) is never
	// matched by revision, and rewriting it would add a row every pass.
	if current.ImageTag == obs.ImageTag && (obs.Revision == "" || current.Revision == "" || current.Revision == obs.Revision) {
		r.lastNote = ""
		return false
	}

	// The observed row must become the open one: a deploy time at or before
	// the current row's start is replaced by now.
	since := obs.Since
	if cur, err := time.Parse(time.RFC3339, current.Since); since.IsZero() || (err == nil && !since.After(cur)) {
		since = now
	}
	_, err = r.daemon.DeployRelease(ctx, obs.ImageTag, r.project, beadsapi.DeployReleaseRequest{
		Target:   r.target,
		Kind:     r.kind,
		Since:    since.Truncate(time.Second).Format(time.RFC3339),
		By:       r.by,
		Via:      releaseObservedVia,
		Revision: obs.Revision,
		ImageTag: obs.ImageTag,
	})
	var apiErr *beadsapi.APIError
	switch {
	case err == nil:
		r.lastNote = ""
		r.logger.Info("release reconciler: recorded the observed deployment",
			"project", r.project, "target", r.target, "tag", obs.ImageTag, "revision", obs.Revision,
			"was_tag", current.ImageTag, "was_revision", current.Revision)
		return true
	case errors.As(err, &apiErr) && apiErr.StatusCode == http.StatusNotFound:
		r.note("release reconciler: the running tag has no release record (gb release record <tag>)", "project", r.project, "tag", obs.ImageTag)
	default:
		r.note("release reconciler: cannot record the observed deployment", "project", r.project, "tag", obs.ImageTag, "error", err)
	}
	return false
}

// RunLoop runs a pass every interval until ctx is done, the first at once.
func (r *releaseReconciler) RunLoop(ctx context.Context, interval time.Duration) {
	r.reconcileOnce(ctx)
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-t.C:
			r.reconcileOnce(ctx)
		case <-ctx.Done():
			return
		}
	}
}

// startReleaseReconciler starts the reconciler on the leader unless
// GARDENIO_RELEASE_RECONCILE_INTERVAL is 0. Without a metadata client it
// runs on the controller's own version alone.
func startReleaseReconciler(ctx context.Context, daemon releaseDeployer, k8sClient kubernetes.Interface, kubeConfig, namespace string, logger *slog.Logger) {
	interval := releaseReconcileDefaultInterval
	if v := os.Getenv("GARDENIO_RELEASE_RECONCILE_INTERVAL"); v != "" {
		d, err := time.ParseDuration(v)
		if err != nil {
			logger.Warn("release reconciler: bad GARDENIO_RELEASE_RECONCILE_INTERVAL; using the default", "value", v, "error", err)
		} else {
			interval = d
		}
	}
	if interval <= 0 {
		logger.Info("release reconciler disabled (GARDENIO_RELEASE_RECONCILE_INTERVAL=0)")
		return
	}
	var helm, homeHelm helmReleaseReader
	if cfg, err := buildK8sConfig(kubeConfig); err != nil {
		logger.Warn("release reconciler: no kubernetes config; using the controller's own version", "error", err)
	} else if meta, err := metadata.NewForConfig(cfg); err != nil {
		logger.Warn("release reconciler: no metadata client; using the controller's own version", "error", err)
	} else {
		newHelm := func() *helmSecrets {
			return &helmSecrets{
				meta: meta, core: k8sClient,
				namespace: envOr(os.Getenv, "GARDENIO_RELEASE_HELM_NAMESPACE", namespace),
				release:   envOr(os.Getenv, "GARDENIO_RELEASE_HELM_RELEASE", "gardenio"),
			}
		}
		// One reader per loop: helmSecrets caches without a lock.
		helm, homeHelm = newHelm(), newHelm()
	}
	go newReleaseReconciler(daemon, helm, version, os.Getenv, logger).RunLoop(ctx, interval)
	startBoxReleaseReporter(ctx, homeHelm, interval, os.Getenv, logger)
}

// releaseDeployRole is the realm role the box's home client needs for
// release writes on the home daemon.
const releaseDeployRole = "gardenio-machine-release-deploy"

// startBoxReleaseReporter starts the box's reconciler against the home daemon
// when GARDENIO_BOX_NAME and GARDENIO_RELEASE_HOME_ADDR are set, and reports
// whether it did. The home client credentials must all be set: without them
// every write would be refused, so the box is not reported and says why.
func startBoxReleaseReporter(ctx context.Context, helm helmReleaseReader, interval time.Duration, getenv func(string) string, logger *slog.Logger) bool {
	addr := strings.TrimSpace(getenv("GARDENIO_RELEASE_HOME_ADDR"))
	if addr == "" || strings.TrimSpace(getenv("GARDENIO_BOX_NAME")) == "" {
		return false
	}
	tokenURL := strings.TrimSpace(getenv("GARDENIO_RELEASE_HOME_TOKEN_URL"))
	clientID := strings.TrimSpace(getenv("GARDENIO_RELEASE_HOME_CLIENT_ID"))
	secretFile := strings.TrimSpace(getenv("GARDENIO_RELEASE_HOME_CLIENT_SECRET_FILE"))
	if tokenURL == "" || clientID == "" || secretFile == "" {
		logger.Warn("release reconciler: GARDENIO_RELEASE_HOME_TOKEN_URL, _CLIENT_ID and _CLIENT_SECRET_FILE must all be set; not reporting this box", "addr", addr)
		return false
	}
	opts := []beadsapi.MachineTokenOption{beadsapi.WithExpectedRoles(releaseDeployRole)}
	if ca := getenv("GARDENIO_RELEASE_HOME_CA_FILE"); strings.TrimSpace(ca) != "" {
		opts = append(opts, beadsapi.WithCAFile(ca))
	}
	ts := beadsapi.NewMachineTokenSource(ctx, logger.With("home", true), tokenURL, clientID, secretFile, opts...)
	home, err := beadsapi.New(beadsapi.Config{HTTPAddr: addr, TokenSource: ts})
	if err != nil {
		logger.Warn("release reconciler: cannot reach the home daemon; not reporting this box", "addr", addr, "error", err)
		return false
	}
	r := newBoxReleaseReconciler(home, helm, version, getenv, logger)
	if r == nil {
		home.Close()
		return false
	}
	logger.Info("release reconciler: reporting this box to the home daemon", "addr", addr, "project", r.project, "target", r.target, "client", clientID)
	go func() {
		defer home.Close()
		r.RunLoop(ctx, interval)
	}()
	return true
}
