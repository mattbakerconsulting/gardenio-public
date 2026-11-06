// Command core-log-shipper is the general gardenio-pod slog transport (design
// ka-0000005579 option A step 3, task ka-0000005209). It STREAMS the core
// components' stdout slog JSON off the Kubernetes pod-log API and POSTs it,
// SECRET-REDACTED, to the already-live in-chart VictoriaLogs store
// (gardenio-victoria-logs:9428 /insert/jsonline) — so an agent can read those
// components' behavior logs back through the `gb logs <component>` verb without
// any k8s pod-log RBAC of its own.
//
// # Why this exists
//
// The four existing shippers (bridge/traefik/idp/ava) ship only narrow edge and
// webhook AUDIT paths. The components an agent most needs to debug — the
// controller, slack-bridge, trigger-engine, grafana-bridge — do not land in
// VictoriaLogs at all. This shipper closes that gap. It is the "hard
// prerequisite" the design calls out: `gb logs` is only as useful as shipper
// coverage.
//
// # Why it reuses bridgeshipper
//
// Structurally this is bridgeshipper with (a) more sources, (b) an EMPTY
// MsgPrefix per source (ship every JSON record, not just a webhook receive-path
// slice), and (c) a per-source Redact hook wired to claudemore.RedactBytes — the
// same secret-shape contract coop and the claudemore proxy enforce. Broad debug
// logs can echo a provider credential, so redaction at ship time is the gate
// design §5 requires before this shipper may be enabled.
//
// # Off by default
//
// Deployed by the off-by-default `coreLogShipper` Helm component
// (templates/core-log-shipper/), one selector env per component. A component
// with no selector configured is simply not streamed; at least one must be set
// or the binary fails fast rather than running dark.
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
	"gardenio/controller/internal/claudemore"
	"gardenio/controller/internal/traefikshipper"
	"gardenio/controller/internal/vlsink"
)

// coreComponent is one queryable component and the env var naming its pod
// selector. The Name is stamped as the VictoriaLogs `source` field and MUST
// match a key in internal/vlogs.componentSelectors, or `gb logs <name>` will
// refuse to query what this ships. containerEnv names the optional container
// override (a multi-container pod).
type coreComponent struct {
	Name         string
	SelectorEnv  string
	ContainerEnv string
}

// coreComponents is the fixed set this shipper knows how to stream. Each is
// gated on its SelectorEnv being set (off by default); the helm chart renders a
// selector only for components whose logs the operator opted to ship.
var coreComponents = []coreComponent{
	{Name: "controller", SelectorEnv: "CORE_CONTROLLER_POD_SELECTOR", ContainerEnv: "CORE_CONTROLLER_CONTAINER_NAME"},
	{Name: "slack-bridge", SelectorEnv: "CORE_SLACK_BRIDGE_POD_SELECTOR", ContainerEnv: "CORE_SLACK_BRIDGE_CONTAINER_NAME"},
	{Name: "trigger-engine", SelectorEnv: "CORE_TRIGGER_ENGINE_POD_SELECTOR", ContainerEnv: "CORE_TRIGGER_ENGINE_CONTAINER_NAME"},
	{Name: "grafana-bridge", SelectorEnv: "CORE_GRAFANA_BRIDGE_POD_SELECTOR", ContainerEnv: "CORE_GRAFANA_BRIDGE_CONTAINER_NAME"},
}

func main() {
	log := slog.New(slog.NewJSONHandler(os.Stderr, nil))
	if err := run(log); err != nil {
		log.Error("core-log-shipper exiting on error", "err", err)
		os.Exit(1)
	}
}

func run(log *slog.Logger) error {
	env := &requiredEnv{}
	vlBase := env.get("CORE_VICTORIALOGS_ENDPOINT") // e.g. http://gardenio-victoria-logs:9428
	podNamespace := env.get("CORE_POD_NAMESPACE")   // the release namespace the core pods run in
	if err := env.err(); err != nil {
		return err
	}

	cfg := bridgeshipper.Config{
		ReconnectBackoff:   envDuration("CORE_RECONNECT_BACKOFF", 5*time.Second),
		StreamSinceSeconds: int64(envInt("CORE_STREAM_SINCE_SECONDS", 300)),
		FlushInterval:      envDuration("CORE_FLUSH_INTERVAL", 2*time.Second),
		MaxBatchLines:      envInt("CORE_MAX_BATCH_LINES", 500),
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	client, err := buildK8sClient(os.Getenv("KUBECONFIG"))
	if err != nil {
		return fmt.Errorf("build k8s client: %w", err)
	}

	sources := buildSources(client, podNamespace)
	if len(sources) == 0 {
		return fmt.Errorf("no core components configured: set at least one of %s", strings.Join(selectorEnvNames(), " / "))
	}

	sink := &vlsink.HTTPSink{
		Endpoint: jsonlineURL(vlBase),
		Client:   &http.Client{Timeout: 30 * time.Second},
	}

	names := make([]string, 0, len(sources))
	for _, s := range sources {
		names = append(names, s.Name)
	}
	log.Info("core-log-shipper starting",
		"pod_namespace", podNamespace,
		"sources", strings.Join(names, ","),
		"victorialogs", sink.Endpoint,
		"redaction", "claudemore.RedactBytes",
		"reconnect_backoff", cfg.ReconnectBackoff.String(),
		"stream_since_seconds", cfg.StreamSinceSeconds,
		"flush_interval", cfg.FlushInterval.String(),
		"max_batch_lines", cfg.MaxBatchLines,
	)

	s := bridgeshipper.New(cfg, sources, sink, log)
	if err := s.Run(ctx); err != nil && ctx.Err() == nil {
		return err
	}
	log.Info("core-log-shipper stopped")
	return nil
}

// buildSources builds one Source per core component whose selector env is set.
// EVERY source ships all JSON (empty MsgPrefix) and is REDACTED through
// claudemore.RedactBytes — the non-negotiable scrub gate for broad debug logs.
func buildSources(client kubernetes.Interface, podNamespace string) []bridgeshipper.Source {
	var sources []bridgeshipper.Source
	for _, c := range coreComponents {
		sel := os.Getenv(c.SelectorEnv)
		if sel == "" {
			continue // this component's logs are not opted in
		}
		sources = append(sources, bridgeshipper.Source{
			Name:      c.Name,
			MsgPrefix: "", // ship every JSON record — this is a behavior-log surface, not an audit slice
			Redact:    claudemore.RedactBytes,
			Stream: &traefikshipper.PodLogStreamer{
				Client:        client,
				Namespace:     podNamespace,
				Selector:      sel,
				ContainerName: os.Getenv(c.ContainerEnv),
			},
		})
	}
	return sources
}

func selectorEnvNames() []string {
	out := make([]string, 0, len(coreComponents))
	for _, c := range coreComponents {
		out = append(out, c.SelectorEnv)
	}
	return out
}

// jsonlineURL builds the VictoriaLogs /insert/jsonline URL, tagging every record
// with the same stream/time/msg fields the read side (internal/vlogs) and the
// other shippers partition on: _stream_fields=source (low cardinality: the
// component name), _time_field=time (slog time), _msg_field=msg (slog msg).
func jsonlineURL(base string) string {
	base = strings.TrimRight(base, "/")
	q := url.Values{}
	q.Set("_stream_fields", "source")
	q.Set("_time_field", "time")
	q.Set("_msg_field", "msg")
	return base + "/insert/jsonline?" + q.Encode()
}

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

// requiredEnv mirrors cmd/bridge-log-shipper: collect all missing required vars
// so a misconfigured overlay fails fast with one error naming all of them.
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
