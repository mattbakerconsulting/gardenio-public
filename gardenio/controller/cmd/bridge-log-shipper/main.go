// Command bridge-log-shipper is the non-AVA webhook-audit transport (GAP-B of
// the webhook carve-out, task ka-0000000320, epic ka-0000000914). It streams the
// jira-bridge and gitlab-bridge pods' webhook receive-path slog JSON off the
// Kubernetes pod-log API and POSTs it to the already-live in-chart VictoriaLogs
// store (gardenio-victoria-logs:9428 /insert/jsonline), giving the two inbound
// webhook doors that stay NON-AVA by design a durable, queryable audit record of
// every delivery and its outcome.
//
// All stream/filter/ship logic lives in internal/bridgeshipper (unit-tested with
// fakes) and reuses the sanctioned building blocks — internal/traefikshipper's
// pod-log follow streamer and internal/vlsink's VictoriaLogs HTTPSink. This
// binary is the thin adapter that reads config from the environment, builds the
// real in-cluster k8s + HTTP clients, wires one Source per enabled bridge door,
// and runs until SIGTERM.
//
// Deployed by the off-by-default `bridgeLogShipper` Helm component
// (templates/bridge-log-shipper/). Config env vars are rendered there. See
// gardenio/docs/security/non-ava-webhook-carveout.md for the carve-out this
// closes (GAP-B).
package main

import (
	"context"
	"fmt"
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

	"gardenio/controller/internal/bridgeshipper"
	"gardenio/controller/internal/traefikshipper"
	"gardenio/controller/internal/vlsink"
)

func main() {
	log := slog.New(slog.NewJSONHandler(os.Stderr, nil))
	if err := run(log); err != nil {
		log.Error("bridge-log-shipper exiting on error", "err", err)
		os.Exit(1)
	}
}

func run(log *slog.Logger) error {
	// Resolve every required var up front so a misconfigured overlay fails fast
	// with ONE error naming ALL of them — rather than starting a transport that
	// silently ships nothing.
	env := &requiredEnv{}
	vlBase := env.get("BRIDGE_VICTORIALOGS_ENDPOINT") // e.g. http://gardenio-victoria-logs:9428
	podNamespace := env.get("BRIDGE_POD_NAMESPACE")   // the release namespace both bridges run in
	if err := env.err(); err != nil {
		return err
	}

	cfg := bridgeshipper.Config{
		ReconnectBackoff:   envDuration("BRIDGE_RECONNECT_BACKOFF", 5*time.Second),
		StreamSinceSeconds: int64(envInt("BRIDGE_STREAM_SINCE_SECONDS", 300)),
		FlushInterval:      envDuration("BRIDGE_FLUSH_INTERVAL", 2*time.Second),
		MaxBatchLines:      envInt("BRIDGE_MAX_BATCH_LINES", 500),
	}

	// Root a cancellable context on SIGINT/SIGTERM so the stream loops drain and
	// return instead of being killed mid-flush.
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	client, err := buildK8sClient(os.Getenv("KUBECONFIG"))
	if err != nil {
		return fmt.Errorf("build k8s client: %w", err)
	}

	// Build one Source per bridge door whose selector is configured. A door with
	// no selector is simply not streamed (its bridge is not deployed in this
	// overlay). At least one must be configured — a shipper with zero sources
	// would run forever shipping nothing, so fail fast rather than start dark.
	var sources []bridgeshipper.Source
	if sel := os.Getenv("BRIDGE_JIRA_POD_SELECTOR"); sel != "" {
		sources = append(sources, bridgeshipper.Source{
			Name:      "jira-bridge",
			MsgPrefix: os.Getenv("BRIDGE_JIRA_MSG_PREFIX"), // e.g. "jira-webhook:"
			Stream: &traefikshipper.PodLogStreamer{
				Client:        client,
				Namespace:     podNamespace,
				Selector:      sel,
				ContainerName: os.Getenv("BRIDGE_JIRA_CONTAINER_NAME"),
			},
		})
	}
	if sel := os.Getenv("BRIDGE_GITLAB_POD_SELECTOR"); sel != "" {
		sources = append(sources, bridgeshipper.Source{
			Name:      "gitlab-bridge",
			MsgPrefix: os.Getenv("BRIDGE_GITLAB_MSG_PREFIX"), // e.g. "webhook:"
			Stream: &traefikshipper.PodLogStreamer{
				Client:        client,
				Namespace:     podNamespace,
				Selector:      sel,
				ContainerName: os.Getenv("BRIDGE_GITLAB_CONTAINER_NAME"),
			},
		})
	}
	if len(sources) == 0 {
		return fmt.Errorf("no bridge doors configured: set at least one of BRIDGE_JIRA_POD_SELECTOR / BRIDGE_GITLAB_POD_SELECTOR")
	}

	sink := &vlsink.HTTPSink{
		Endpoint: jsonlineURL(vlBase),
		Client:   &http.Client{Timeout: 30 * time.Second},
	}

	names := make([]string, 0, len(sources))
	for _, s := range sources {
		names = append(names, s.Name)
	}
	log.Info("bridge-log-shipper starting",
		"pod_namespace", podNamespace,
		"sources", strings.Join(names, ","),
		"victorialogs", sink.Endpoint,
		"reconnect_backoff", cfg.ReconnectBackoff.String(),
		"stream_since_seconds", cfg.StreamSinceSeconds,
		"flush_interval", cfg.FlushInterval.String(),
		"max_batch_lines", cfg.MaxBatchLines,
	)

	s := bridgeshipper.New(cfg, sources, sink, log)
	if err := s.Run(ctx); err != nil && ctx.Err() == nil {
		return err
	}
	log.Info("bridge-log-shipper stopped")
	return nil
}

// jsonlineURL builds the VictoriaLogs /insert/jsonline URL from the base
// endpoint, tagging every ingested record with the stream/time/msg fields the
// audit queries partition on. _stream_fields uses `source` (low-cardinality:
// jira-bridge/gitlab-bridge) so streams don't explode; _time_field points at the
// bridges' slog `time` field so the LogsQL _time window matches when the webhook
// was received, not when it was shipped; _msg_field points at slog `msg` so a
// bare LogsQL word-search hits the human-readable outcome line.
func jsonlineURL(base string) string {
	base = strings.TrimRight(base, "/")
	q := url.Values{}
	q.Set("_stream_fields", "source")
	q.Set("_time_field", "time")
	q.Set("_msg_field", "msg")
	return base + "/insert/jsonline?" + q.Encode()
}

// buildK8sClient builds a kubernetes clientset from an explicit kubeconfig path
// or the in-cluster config. Mirrors cmd/traefik-log-shipper's helper.
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
// all of them. Mirrors cmd/traefik-log-shipper.
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
