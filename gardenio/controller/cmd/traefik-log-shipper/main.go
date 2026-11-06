// Command traefik-log-shipper is the edge-IDP L0 transport: it streams the
// Traefik edge pod's JSON access log off the Kubernetes pod-log API and POSTs it
// to the already-live VictoriaLogs store (gardenio-victoria-logs:9428
// /insert/jsonline), giving the edge intrusion-detection program (design
// ka-0000007287) a queryable per-request record of who touches the public edge.
//
// Task ka-0000001563, epic ka-0000008216. All stream/filter/ship logic lives in
// internal/traefikshipper (unit-tested with fakes); this binary is the thin
// adapter that reads config from the environment, builds the real in-cluster k8s
// + HTTP clients, and runs until SIGTERM.
//
// Deployed by the off-by-default `traefikLogShipper` Helm component
// (templates/traefik-log-shipper/). Config env vars are rendered there. See the
// MR PROOF block for the live volume measurement that justified this transport
// (a small stdout streamer rather than a sidecar or DaemonSet).
package main

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"

	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"

	"gardenio/controller/internal/auditxform"
	"gardenio/controller/internal/traefikshipper"
	"gardenio/controller/internal/vlsink"
)

func main() {
	log := slog.New(slog.NewJSONHandler(os.Stderr, nil))
	if err := run(log); err != nil {
		log.Error("traefik-log-shipper exiting on error", "err", err)
		os.Exit(1)
	}
}

func run(log *slog.Logger) error {
	// Resolve every required var up front so a misconfigured overlay fails fast
	// with ONE error naming ALL of them — rather than starting a transport that
	// silently ships nothing (e.g. a hostless VictoriaLogs URL POSTed to forever).
	env := &requiredEnv{}
	vlBase := env.get("TRAEFIK_VICTORIALOGS_ENDPOINT") // e.g. http://gardenio-victoria-logs:9428
	podNamespace := env.get("TRAEFIK_POD_NAMESPACE")   // e.g. traefik
	podSelector := env.get("TRAEFIK_POD_SELECTOR")     // e.g. app.kubernetes.io/name=traefik
	if err := env.err(); err != nil {
		return err
	}

	cfg := traefikshipper.Config{
		ReconnectBackoff:   envDuration("TRAEFIK_RECONNECT_BACKOFF", 5*time.Second),
		StreamSinceSeconds: int64(envInt("TRAEFIK_STREAM_SINCE_SECONDS", 300)),
		FlushInterval:      envDuration("TRAEFIK_FLUSH_INTERVAL", 2*time.Second),
		MaxBatchLines:      envInt("TRAEFIK_MAX_BATCH_LINES", 500),
		MaxLineBytes:       envInt("TRAEFIK_MAX_LINE_BYTES", 0),
	}

	// Root a cancellable context on SIGINT/SIGTERM so the stream loop drains and
	// returns instead of being killed mid-flush.
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	client, err := buildK8sClient(os.Getenv("KUBECONFIG"))
	if err != nil {
		return fmt.Errorf("build k8s client: %w", err)
	}

	streamer := &traefikshipper.PodLogStreamer{
		Client:        client,
		Namespace:     podNamespace,
		Selector:      podSelector,
		ContainerName: os.Getenv("TRAEFIK_CONTAINER_NAME"), // empty = default container
	}

	sink := &vlsink.HTTPSink{
		Endpoint: jsonlineURL(vlBase),
		Client:   &http.Client{Timeout: 30 * time.Second},
	}

	// OPTIONAL L1 geo/ASN enrichment (edge-IDP L1, task ka-0000001136). Two
	// mutually-exclusive tables:
	//   - TRAEFIK_ASN_TABLE_PATH -> a CIDR->ASN table; each shipped row's
	//     X-Forwarded-For client IP resolves to a scalar sourceASN.
	//   - TRAEFIK_GEO_TABLE_PATH -> a CIDR->{ASN,country} table (a superset); each
	//     row resolves to scalar sourceASN + sourceCountry, so the edge geomap
	//     console (ka-0000003019) and the L2 novel-geo detections have a country
	//     dimension.
	// Neither set = nil resolver = byte-identical RAW shipping (no client_ip/
	// sourceASN/sourceCountry stamped) — matching the deploy-raw-first, enrich-
	// second sequencing resolved on ka-0000001136. A configured-but-unreadable/
	// garbled table fails fast rather than silently shipping without enrichment.
	resolver, geoEnabled, err := buildResolver(os.Getenv("TRAEFIK_ASN_TABLE_PATH"), os.Getenv("TRAEFIK_GEO_TABLE_PATH"))
	if err != nil {
		return err
	}

	log.Info("traefik-log-shipper starting",
		"pod_namespace", podNamespace,
		"pod_selector", podSelector,
		"container", streamer.ContainerName,
		"victorialogs", sink.Endpoint,
		"reconnect_backoff", cfg.ReconnectBackoff.String(),
		"stream_since_seconds", cfg.StreamSinceSeconds,
		"flush_interval", cfg.FlushInterval.String(),
		"max_batch_lines", cfg.MaxBatchLines,
		"asn_enrichment", resolver != nil,
		"geo_enrichment", geoEnabled,
	)

	s := traefikshipper.New(cfg, streamer, sink, log, resolver)
	if err := s.Run(ctx); err != nil && ctx.Err() == nil {
		return err
	}
	log.Info("traefik-log-shipper stopped")
	return nil
}

// buildResolver selects the L1 enrichment resolver from the two mutually-exclusive
// table env vars. It returns (resolver, geoEnabled, err): geoEnabled is true only
// when a geo table was loaded (so startup can log it). Neither var set =>
// (nil, false, nil), the byte-identical raw-shipping path. A geo table is a
// superset of an ASN table (it also emits sourceASN), so configuring both is an
// error rather than a silent precedence rule — a detective control's enrichment
// source must be unambiguous.
func buildResolver(asnTablePath, geoTablePath string) (auditxform.ASNResolver, bool, error) {
	switch {
	case asnTablePath != "" && geoTablePath != "":
		return nil, false, fmt.Errorf("TRAEFIK_ASN_TABLE_PATH and TRAEFIK_GEO_TABLE_PATH are mutually exclusive; a geo table already emits sourceASN")
	case geoTablePath != "":
		r, err := loadTable(geoTablePath, "TRAEFIK_GEO_TABLE_PATH", auditxform.ParseGeoTable)
		return r, err == nil, err
	case asnTablePath != "":
		r, err := loadTable(asnTablePath, "TRAEFIK_ASN_TABLE_PATH", auditxform.ParseASNTable)
		return r, false, err
	default:
		return nil, false, nil
	}
}

// loadTable reads and parses the operator-provided table at path with the given
// parser (the shared auditxform table format). A missing or malformed table is
// an error — the caller aborts startup rather than silently ship without the
// enrichment the detections need.
func loadTable(path, envName string, parse func(io.Reader) (*auditxform.PrefixTableResolver, error)) (auditxform.ASNResolver, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("open %s %q: %w", envName, path, err)
	}
	defer f.Close()
	r, err := parse(f)
	if err != nil {
		return nil, fmt.Errorf("parse %s %q: %w", envName, path, err)
	}
	return r, nil
}

// jsonlineURL builds the VictoriaLogs /insert/jsonline URL from the base
// endpoint, tagging every ingested record with the stream/time fields the edge
// detections partition on. _stream_fields uses `entryPointName` (low-cardinality:
// web/websecure/ziti/webhooks) so streams don't explode; _time_field points at
// Traefik's own StartUTC so the LogsQL _time window matches when the request
// happened, not when it was shipped; _msg_field points at RequestPath so a bare
// LogsQL word-search hits the path (the field analysts scan first).
func jsonlineURL(base string) string {
	base = strings.TrimRight(base, "/")
	q := url.Values{}
	q.Set("_stream_fields", "entryPointName")
	q.Set("_time_field", "StartUTC")
	q.Set("_msg_field", "RequestPath")
	return base + "/insert/jsonline?" + q.Encode()
}

// buildK8sClient builds a kubernetes clientset from an explicit kubeconfig path
// or the in-cluster config. Mirrors grafana-bridge's helper.
func buildK8sClient(kubeconfig string) (kubernetes.Interface, error) {
	var (
		restCfg *rest.Config
		err     error
	)
	if kubeconfig != "" {
		restCfg, err = clientcmd.BuildConfigFromFlags("", kubeconfig)
	} else {
		restCfg, err = rest.InClusterConfig()
	}
	if err != nil {
		return nil, fmt.Errorf("building k8s config: %w", err)
	}
	return kubernetes.NewForConfig(restCfg)
}

// requiredEnv reads required environment variables, remembering the names of any
// that are unset or empty so the caller can fail fast with a single error listing
// all of them (rather than one at a time, or — worse — not at all).
type requiredEnv struct {
	missing []string
}

func (r *requiredEnv) get(k string) string {
	v := os.Getenv(k)
	if v == "" {
		r.missing = append(r.missing, k)
	}
	return v
}

func (r *requiredEnv) err() error {
	if len(r.missing) == 0 {
		return nil
	}
	return fmt.Errorf("missing required environment variable(s): %s", strings.Join(r.missing, ", "))
}

func envDuration(k string, def time.Duration) time.Duration {
	if v := os.Getenv(k); v != "" {
		if d, err := time.ParseDuration(v); err == nil {
			return d
		}
	}
	return def
}

func envInt(k string, def int) int {
	if v := os.Getenv(k); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			return n
		}
	}
	return def
}
