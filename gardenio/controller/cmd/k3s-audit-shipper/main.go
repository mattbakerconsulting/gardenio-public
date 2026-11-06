// Command k3s-audit-shipper tails the k3s kube-apiserver audit log on a
// control-plane node and ships it, redacted, to the in-chart VictoriaLogs store
// (task gd-ZrPUxj8wyU, follow-up to gd-HXRD9yizAE; decision gd-bNVBdrmIo5 chose a
// 30 day shelf).
//
// # Why this exists
//
// The servers write /var/log/k3s-audit/audit.log, the only server-side record of
// what the cluster-admin ServiceAccount gardenio-infra-admin did. The servers are
// spot and the root volume is deleted on termination, so the log dies with the
// node. This shipper moves each event off the node as it is written.
//
// # How it differs from core-log-shipper
//
// core-log-shipper follows pod stdout through the pod-log API. This reads a FILE
// on the node through a hostPath mount (the apiserver is not a pod), so it runs
// as a DaemonSet pinned to the control plane, as root (the log is 0600 root),
// and keeps a checkpoint so a restart neither re-ships nor skips events. The
// redaction gate is internal/k3sauditshipper.Transform: sensitive kinds lose
// their bodies, sensitive keys are masked, and claudemore.RedactBytes scrubs
// credential shapes.
//
// Deployed by the operator-applied manifests in helm/k3s-audit-shipper/, NOT by the
// gardenio Helm release (its PSA-privileged namespace and hostPath DaemonSet are
// not something the release deployer may hold rights over). It writes to its own
// VictoriaLogs tenant (K3S_AUDIT_VICTORIALOGS_ACCOUNT_ID, never 0).
package main

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"gardenio/controller/internal/k3sauditshipper"
	"gardenio/controller/internal/vlsink"
)

func main() {
	log := slog.New(slog.NewJSONHandler(os.Stderr, nil))
	if err := run(log); err != nil {
		log.Error("k3s-audit-shipper exiting on error", "err", err)
		os.Exit(1)
	}
}

func run(log *slog.Logger) error {
	env := &requiredEnv{}
	vlBase := env.get("K3S_AUDIT_VICTORIALOGS_ENDPOINT")    // e.g. http://gardenio-victoria-logs.gardenio.svc:9428
	node := env.get("K3S_AUDIT_NODE_NAME")                  // downward API spec.nodeName
	account := env.get("K3S_AUDIT_VICTORIALOGS_ACCOUNT_ID") // tenant; never the default 0
	if err := env.err(); err != nil {
		return err
	}
	tenantHdr, err := tenantHeaders(account, os.Getenv("K3S_AUDIT_VICTORIALOGS_PROJECT_ID"))
	if err != nil {
		return err
	}

	bodies := k3sauditshipper.BodyMode(envString("K3S_AUDIT_BODIES", string(k3sauditshipper.BodiesRedacted)))
	switch bodies {
	case k3sauditshipper.BodiesRedacted, k3sauditshipper.BodiesNone:
	default:
		return fmt.Errorf("K3S_AUDIT_BODIES=%q: want %q or %q", bodies, k3sauditshipper.BodiesRedacted, k3sauditshipper.BodiesNone)
	}

	cfg := k3sauditshipper.Config{
		Path:          envString("K3S_AUDIT_LOG_PATH", "/var/log/k3s-audit/audit.log"),
		StatePath:     envString("K3S_AUDIT_STATE_PATH", "/var/lib/k3s-audit-shipper/state.json"),
		FromStart:     envBool("K3S_AUDIT_FROM_START", true),
		PollInterval:  envDuration("K3S_AUDIT_POLL_INTERVAL", time.Second),
		FlushInterval: envDuration("K3S_AUDIT_FLUSH_INTERVAL", 2*time.Second),
		MaxBatchLines: envInt("K3S_AUDIT_MAX_BATCH_LINES", 500),
		MaxLineBytes:  envInt("K3S_AUDIT_MAX_LINE_BYTES", 4*1024*1024),
		RetryBackoff:  envDuration("K3S_AUDIT_RETRY_BACKOFF", 5*time.Second),
		Options: k3sauditshipper.Options{
			Node:           node,
			Bodies:         bodies,
			MaxBodyBytes:   envInt("K3S_AUDIT_MAX_BODY_BYTES", k3sauditshipper.DefaultMaxBodyBytes),
			DropReads:      envBool("K3S_AUDIT_DROP_READS", false),
			KeepReadsUsers: splitList(os.Getenv("K3S_AUDIT_KEEP_READS_USERS")),
		},
	}
	if err := os.MkdirAll(filepath.Dir(cfg.StatePath), 0o700); err != nil {
		return fmt.Errorf("create state dir: %w", err)
	}

	sink := &vlsink.HTTPSink{
		Endpoint: jsonlineURL(vlBase),
		Client:   &http.Client{Timeout: 30 * time.Second},
		Headers:  tenantHdr,
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	log.Info("k3s-audit-shipper starting",
		"node", node,
		"path", cfg.Path,
		"state", cfg.StatePath,
		"victorialogs", sink.Endpoint,
		"account_id", tenantHdr["AccountID"],
		"bodies", string(bodies),
		"max_body_bytes", cfg.Options.MaxBodyBytes,
		"drop_reads", cfg.Options.DropReads,
		"from_start", cfg.FromStart,
		"flush_interval", cfg.FlushInterval.String(),
		"max_batch_lines", cfg.MaxBatchLines,
	)
	if err := k3sauditshipper.New(cfg, sink, log).Run(ctx); err != nil && ctx.Err() == nil {
		return err
	}
	log.Info("k3s-audit-shipper stopped")
	return nil
}

// jsonlineURL builds the VictoriaLogs /insert/jsonline URL. The stream is
// (source, node): both are low cardinality (one source, a handful of servers).
// _time_field=time is the event's own stageTimestamp, so a back-fill lands at
// the time the call happened, not the time it was shipped.
func jsonlineURL(base string) string {
	base = strings.TrimRight(base, "/")
	q := url.Values{}
	q.Set("_stream_fields", "source,node")
	q.Set("_time_field", "time")
	q.Set("_msg_field", "msg")
	return base + "/insert/jsonline?" + q.Encode()
}

// tenantHeaders builds the VictoriaLogs tenant selectors. The audit log is
// written to its own tenant so that a plain query against the default tenant
// (what gb logs and every other reader uses) never returns, or is polluted by,
// audit records. The default account 0 is refused: shipping there would put the
// audit log in the same tenant as everything the agents can read.
func tenantHeaders(account, project string) (map[string]string, error) {
	acct, err := strconv.ParseUint(strings.TrimSpace(account), 10, 32)
	if err != nil || acct == 0 {
		return nil, fmt.Errorf("K3S_AUDIT_VICTORIALOGS_ACCOUNT_ID=%q: want a non-zero unsigned integer (0 is the default tenant agents can read)", account)
	}
	proj := uint64(0)
	if p := strings.TrimSpace(project); p != "" {
		if proj, err = strconv.ParseUint(p, 10, 32); err != nil {
			return nil, fmt.Errorf("K3S_AUDIT_VICTORIALOGS_PROJECT_ID=%q: want an unsigned integer", project)
		}
	}
	return map[string]string{
		"AccountID": strconv.FormatUint(acct, 10),
		"ProjectID": strconv.FormatUint(proj, 10),
	}, nil
}

// requiredEnv collects every missing required variable so a misconfigured
// release fails fast with one error naming all of them.
type requiredEnv struct{ missing []string }

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

func envString(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
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

func envBool(k string, def bool) bool {
	if v := os.Getenv(k); v != "" {
		if b, err := strconv.ParseBool(v); err == nil {
			return b
		}
	}
	return def
}

func splitList(s string) []string {
	var out []string
	for _, p := range strings.Split(s, ",") {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}
