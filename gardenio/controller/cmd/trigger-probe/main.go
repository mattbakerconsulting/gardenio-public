// Command trigger-probe is a TEST-ONLY driver that fires a single
// non-schedule trigger source against an in-test beads daemon so the
// claudeless e2e scenarios can assert the real evaluation→action loop end to
// end for the sources that the standalone `trigger-engine` binary does not
// register (jira, gitlab MR/pipeline, account-threshold, slack-reaction).
//
// These sources need a dependency the standalone engine cannot supply on its
// own, or are otherwise awkward to drive deterministically end-to-end:
//   - jira / gitlab    → a live JiraClient / GitLabClient HTTP API. The probe
//     stands up a stub httptest-style server returning one
//     canned matching item, exactly as the unit tests do.
//   - slack-reaction   → an in-process buffered reaction_added event.
//   - account          → account beads in the daemon (seeded by the caller).
//   - bead             → a synthetic bead lifecycle event injected into the
//     real BeadStateTriggerSource buffer. The standalone trigger-engine DOES
//     register the bead source, but driving it deterministically requires
//     racing a live SSE frame before DrainPending(); injecting the event here
//     makes the fan-out (one mutation → notify + create_bead + apply_formula)
//     deterministic in a single EvaluateSource pass with no reconcile wait.
//   - gardener          → the terminal_goal master gate's resume-or-escalate
//     decision (bead ka-0000005353). This is NOT a TriggerSource: the gate runs
//     inside evalOne's gardenerPreFireCheck, which the fleet-wide EvaluateSource
//     would fire against EVERY live gardener. The probe instead drives
//     TriggerEngine.ProbeGardenerResumeOrEscalate against exactly ONE named
//     gardener trigger bead, then (with --metrics-addr) serves the in-process
//     default prometheus registry on /metrics so the scenario can scrape
//     gardener_resume_total exactly as it would from the real gardener-firing
//     bridge. Observables: the gardener bead's post-state (re-armed blob +
//     resume comment, or gardener-stuck-escalate label + dormant on its plan,
//     or + closed when it has no open plan) and the scraped
//     counter.
//
// The probe wires the REAL bridge.TriggerSource + REAL bridge.TriggerEngine +
// REAL *beadsapi.Client (pointed at $BEADS_HTTP_URL), seeds the engine's
// dedup set with a sentinel so the runtime-bootstrap guard is skipped (we want
// the seeded match to fire on this first pass, not be silently absorbed), runs
// exactly one EvaluateSource cycle, and exits. The action's audit bead
// (type=event, label=trigger:<id>, fields.outcome=allow) is the observable the
// e2e scenario asserts on.
//
// This binary is NOT part of any release Makefile / Dockerfile target — it is
// built from source on demand inside the e2e shard, the same way
// trigger-engine is, and never ships in a product image.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"

	"gardenio/controller/internal/beadsapi"
	"gardenio/controller/internal/bridge"
)

func main() {
	var (
		source    = flag.String("source", "", "source to fire: jira|gitlab-mr|gitlab-pipeline|account|slack-reaction|bead|multi|gardener")
		triggerID = flag.String("trigger", "", "trigger bead ID to evaluate")
		httpURL   = flag.String("http-url", os.Getenv("BEADS_HTTP_URL"), "beads daemon HTTP URL")
		token     = flag.String("token", os.Getenv("BEADS_TOKEN"), "beads daemon bearer token")
		// gardener source knobs (terminal_goal master gate, bead ka-0000005353).
		// The gardener mode drives the resume-or-escalate decision against ONE
		// named gardener trigger bead via TriggerEngine.ProbeGardenerResumeOrEscalate
		// — it does NOT run a fleet-wide EvaluateSource, so it cannot touch any
		// other gardener. --gardener-kind is the premature stop kind that fired.
		gardenerKind = flag.String("gardener-kind", "max_runs", "gardener mode: premature stop kind that fired (max_runs|no_progress_for|wall_clock|consecutive_failures)")
		// After firing, optionally serve the in-process /metrics (the default
		// prometheus gatherer the gardener_resume_total counter is registered on)
		// so an e2e can `curl <addr>/metrics | grep gardener_resume_total` exactly
		// as it would scrape the real gardener-firing bridge. Empty = don't serve,
		// exit immediately after firing (the default for the non-gardener sources).
		metricsAddr  = flag.String("metrics-addr", "", "if set, serve the in-process /metrics on this addr after firing and block until SIGTERM or --serve-timeout")
		serveTimeout = flag.Duration("serve-timeout", 30*time.Second, "max time to serve /metrics before exiting (gardener mode with --metrics-addr)")
		// jira / gitlab canned-response knobs. The probe returns exactly one
		// item shaped to match the trigger bead the caller created.
		jiraKey      = flag.String("jira-key", "PROBE-1", "JIRA issue key the stub server returns")
		jiraSummary  = flag.String("jira-summary", "probe issue", "JIRA issue summary")
		mrIID        = flag.Int("mr-iid", 1, "GitLab MR iid the stub server returns")
		mrTitle      = flag.String("mr-title", "probe MR", "GitLab MR title")
		mrState      = flag.String("mr-state", "opened", "GitLab MR state")
		pipelineID   = flag.Int("pipeline-id", 1, "GitLab pipeline id the stub server returns")
		pipeStatus   = flag.String("pipeline-status", "failed", "GitLab pipeline status")
		pipeRef      = flag.String("pipeline-ref", "main", "GitLab pipeline ref")
		reactEmoji   = flag.String("reaction-emoji", "runner", "slack-reaction emoji to inject")
		reactChannel = flag.String("reaction-channel", "C_PROBE", "slack-reaction channel")
		reactTS      = flag.String("reaction-ts", "1700000000.000100", "slack-reaction message ts")
		reactText    = flag.String("reaction-text", "probe message", "slack-reaction message text")
		// bead source knobs. The probe injects one synthetic bead lifecycle
		// event into the real BeadStateTriggerSource buffer; every enabled
		// trigger:bead bead in the daemon is then evaluated against it in one
		// EvaluateSource pass (fan-out — one event can fire many triggers).
		beadID     = flag.String("bead-id", "PROBE-BEAD-1", "synthetic source bead id")
		beadTitle  = flag.String("bead-title", "probe bead", "synthetic source bead title")
		beadType   = flag.String("bead-type", "task", "synthetic source bead type")
		beadStatus = flag.String("bead-status", "open", "synthetic source bead status")
		beadEvent  = flag.String("bead-event", "created", "lifecycle event: created|updated|closed")
		beadLabels = flag.String("bead-labels", "", "comma-separated labels on the synthetic source bead")
		// ignoreCooldown bypasses the engine's per-trigger cooldown gate for the
		// --trigger this probe fires (ka-0000007982). On a SHARED daemon another
		// live evaluator — the E2E namespace runs a trigger-engine polling every
		// 30s — stamps last_checked on the freshly-created trigger:bead beads on
		// every non-firing poll, and the 30s cooldown floor then suppresses this
		// probe's single deliberate fire depending on race timing (the flake in
		// test-trigger-action-fidelity). Set it whenever the probe runs against a
		// daemon a live trigger-engine also drives.
		ignoreCooldown = flag.Bool("ignore-cooldown", false, "bypass the per-trigger cooldown gate for --trigger (needed when a live trigger-engine shares this daemon and may stamp last_checked; ka-0000007982)")
	)
	flag.Parse()

	if *source == "" || *triggerID == "" {
		fatal("both --source and --trigger are required")
	}
	if *httpURL == "" {
		fatal("--http-url (or $BEADS_HTTP_URL) is required")
	}

	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelInfo}))

	daemon, err := beadsapi.New(beadsapi.Config{HTTPAddr: *httpURL, Token: *token})
	if err != nil {
		fatal(fmt.Sprintf("beadsapi.New: %v", err))
	}
	defer daemon.Close()

	engine := bridge.NewTriggerEngine(bridge.TriggerEngineConfig{
		Daemon: daemon,
		Logger: logger,
	})

	// gardener mode is not a registered TriggerSource — it drives the
	// terminal_goal master gate's resume-or-escalate decision against exactly
	// ONE gardener trigger bead (bead ka-0000005353), bypassing the fleet-wide
	// EvaluateSource loop entirely. Handle it before registerSource so the
	// non-gardener switch stays unchanged.
	if *source == "gardener" {
		runGardenerProbe(engine, *triggerID, *gardenerKind, *metricsAddr, *serveTimeout, logger)
		return
	}

	srcName, cleanup := registerSource(engine, logger, *source, probeConfig{
		jiraKey:      *jiraKey,
		jiraSummary:  *jiraSummary,
		mrIID:        *mrIID,
		mrTitle:      *mrTitle,
		mrState:      *mrState,
		pipelineID:   *pipelineID,
		pipeStatus:   *pipeStatus,
		pipeRef:      *pipeRef,
		reactEmoji:   *reactEmoji,
		reactChannel: *reactChannel,
		reactTS:      *reactTS,
		reactText:    *reactText,
		beadID:       *beadID,
		beadTitle:    *beadTitle,
		beadType:     *beadType,
		beadStatus:   *beadStatus,
		beadEvent:    *beadEvent,
		beadLabels:   *beadLabels,
		triggerID:    *triggerID,
	})
	defer cleanup()

	// Skip the runtime-bootstrap guard for poll-based sources (jira, gitlab,
	// account). On a freshly-created trigger with no prior beads and no
	// in-memory seen state, evalOne would otherwise seed the current matches
	// WITHOUT firing — exactly the thundering-herd suppression the engine is
	// designed for. We want the single seeded match to FIRE on this pass, so
	// we pre-seed the trigger's seen set with a sentinel key. That makes
	// hasSeen=true, which bypasses the bootstrap branch while leaving the real
	// match key unseen so it still fires. Event-driven slack-reaction opts out
	// of bootstrap on its own (SkipRuntimeBootstrap), so the sentinel is a
	// harmless no-op there.
	engine.SeedSeen(*triggerID, "__probe_bootstrap_sentinel__")

	// Bypass the per-trigger cooldown gate for the trigger we fire when the
	// caller opts in (ka-0000007982). The engine floors the cooldown at 30s, and
	// on a daemon a live trigger-engine also drives (the E2E namespace runs one
	// polling every 30s) that engine stamps last_checked on the freshly-created
	// trigger:bead beads on every non-firing poll — which would cooldown-skip
	// this probe's single deliberate fire depending on race timing. The bypass
	// is scoped to exactly --trigger, so any OTHER trigger this pass fans out to
	// still honors its real cooldown.
	if *ignoreCooldown {
		engine.IgnoreCooldown(*triggerID)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	engine.EvaluateSource(ctx, srcName)

	fmt.Printf("trigger-probe: fired source=%s trigger=%s\n", srcName, *triggerID)
}

// runGardenerProbe drives the terminal_goal master gate's resume-or-escalate
// decision against exactly one gardener trigger bead (bead ka-0000005353) and,
// when metricsAddr is set, serves the in-process default prometheus registry on
// /metrics so an e2e can scrape gardener_resume_total exactly as it would from
// the real gardener-firing bridge (trigger-engine / gitlab-bridge). The counter
// is promauto-registered on the default registry (internal/bridge/
// gardener_resume_metrics.go), so serving prometheus.DefaultGatherer here mirrors
// buildMetricsServer in cmd/trigger-engine.
func runGardenerProbe(engine *bridge.TriggerEngine, triggerID, kind, metricsAddr string, serveTimeout time.Duration, logger *slog.Logger) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	if err := engine.ProbeGardenerResumeOrEscalate(ctx, triggerID, kind); err != nil {
		fatal(fmt.Sprintf("gardener probe: %v", err))
	}
	fmt.Printf("trigger-probe: fired source=gardener trigger=%s kind=%s\n", triggerID, kind)

	if metricsAddr == "" {
		return
	}

	// Serve the default gatherer so the scenario can curl /metrics. Block until
	// SIGTERM (the scenario kills us after scraping) or serveTimeout so a
	// misbehaving run can never wedge the shard.
	mux := http.NewServeMux()
	mux.Handle("GET /metrics", promhttp.HandlerFor(prometheus.DefaultGatherer, promhttp.HandlerOpts{}))
	srv := &http.Server{Addr: metricsAddr, Handler: mux, ReadHeaderTimeout: 5 * time.Second}

	go func() {
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			logger.Error("gardener probe: metrics server error", "error", err)
		}
	}()
	fmt.Printf("trigger-probe: serving /metrics on %s (timeout %s)\n", metricsAddr, serveTimeout)

	sigCtx, sigCancel := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT)
	defer sigCancel()
	select {
	case <-sigCtx.Done():
	case <-time.After(serveTimeout):
	}
	shutCtx, shutCancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer shutCancel()
	_ = srv.Shutdown(shutCtx)
}

type probeConfig struct {
	jiraKey, jiraSummary                         string
	mrIID                                        int
	mrTitle, mrState                             string
	pipelineID                                   int
	pipeStatus, pipeRef                          string
	reactEmoji, reactChannel, reactTS, reactText string
	beadID, beadTitle, beadType, beadStatus      string
	beadEvent, beadLabels                        string
	triggerID                                    string
}

// registerSource builds the requested real TriggerSource, registers it on the
// engine, and returns its Name() plus a cleanup func (closes any stub server).
func registerSource(engine *bridge.TriggerEngine, logger *slog.Logger, source string, cfg probeConfig) (string, func()) {
	switch source {
	case "jira":
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			// JiraClient.SearchIssuesPage → GET /rest/api/3/search/jql
			if !strings.HasPrefix(r.URL.Path, "/rest/api/3/search") {
				http.NotFound(w, r)
				return
			}
			writeJSON(w, map[string]any{
				"issues": []map[string]any{{
					"key": cfg.jiraKey,
					"id":  "10001",
					"fields": map[string]any{
						"summary":   cfg.jiraSummary,
						"status":    map[string]string{"name": "Open"},
						"issuetype": map[string]string{"name": "Task"},
					},
				}},
				"nextPageToken": "",
			})
		}))
		jira := bridge.NewJiraClient(bridge.JiraClientConfig{
			BaseURL: srv.URL, Email: "probe@example.com", APIToken: "probe", Logger: logger,
		})
		engine.RegisterSource(bridge.NewJiraTriggerSource(jira))
		return "jira", srv.Close

	case "gitlab-mr":
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			// GitLabClient.ListMergeRequests → GET /api/v4/projects/:id/merge_requests
			if !strings.Contains(r.URL.Path, "/merge_requests") {
				writeJSON(w, []any{})
				return
			}
			writeJSON(w, []map[string]any{{
				"id":            cfg.mrIID * 1000,
				"iid":           cfg.mrIID,
				"title":         cfg.mrTitle,
				"state":         cfg.mrState,
				"web_url":       "https://gitlab.example/mr/" + itoa(cfg.mrIID),
				"source_branch": "feature/probe",
				"target_branch": "main",
				"author":        map[string]any{"username": "probe-user", "name": "Probe User"},
			}})
		}))
		gl := bridge.NewGitLabClient(bridge.GitLabClientConfig{BaseURL: srv.URL, Token: "probe", Logger: logger})
		engine.RegisterSource(bridge.NewGitLabTriggerSourceWithLogger(gl, logger))
		return "gitlab", srv.Close

	case "gitlab-pipeline":
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			// GitLabClient.ListPipelines → GET /api/v4/projects/:id/pipelines
			if !strings.Contains(r.URL.Path, "/pipelines") {
				writeJSON(w, []any{})
				return
			}
			writeJSON(w, []map[string]any{{
				"id":      cfg.pipelineID,
				"status":  cfg.pipeStatus,
				"ref":     cfg.pipeRef,
				"sha":     "deadbeefcafe",
				"web_url": "https://gitlab.example/pipeline/" + itoa(cfg.pipelineID),
			}})
		}))
		gl := bridge.NewGitLabClient(bridge.GitLabClientConfig{BaseURL: srv.URL, Token: "probe", Logger: logger})
		engine.RegisterSource(bridge.NewGitLabTriggerSourceWithLogger(gl, logger))
		return "gitlab", srv.Close

	case "account":
		// Account source reads account beads from the daemon directly — no
		// external HTTP. The caller seeds an account bead whose field crosses
		// the trigger's threshold before invoking the probe.
		engine.RegisterSource(bridge.NewAccountTriggerSource(logger))
		return "account", func() {}

	case "slack-reaction":
		// Event-driven: buffer one reaction_added event matching the trigger.
		src := bridge.NewSlackReactionTriggerSource(logger)
		src.BufferReaction("U_PROBE", cfg.reactEmoji, cfg.reactChannel, cfg.reactTS, cfg.reactText)
		engine.RegisterSource(src)
		return "slack-reaction", func() {}

	case "bead":
		// Event-driven: inject one synthetic bead lifecycle event into the
		// real BeadStateTriggerSource buffer. EvaluateSource("bead") then loads
		// every enabled trigger:bead bead from the daemon and evaluates each
		// against this one event — so a single source mutation fans out to
		// however many triggers match it (notify / create_bead / apply_formula).
		// Like slack-reaction, the bead source opts out of the runtime-bootstrap
		// guard (SkipRuntimeBootstrap), so the buffered event fires on this pass.
		src := bridge.NewBeadStateTriggerSource(logger)
		var labels []string
		for l := range strings.SplitSeq(cfg.beadLabels, ",") {
			if l = strings.TrimSpace(l); l != "" {
				labels = append(labels, l)
			}
		}
		topic := "beads.bead." + cfg.beadEvent
		src.InjectBeadEvent(topic, &bridge.BeadEvent{
			ID:     cfg.beadID,
			Type:   cfg.beadType,
			Title:  cfg.beadTitle,
			Status: cfg.beadStatus,
			Labels: labels,
		})
		engine.RegisterSource(src)
		return "bead", func() {}

	case "multi":
		// Event-driven: inject one synthetic bead lifecycle event into the
		// MultiTriggerSource's underlying EntryEngine buffer, then evaluate
		// every enabled `trigger:multi` bead against it in one
		// EvaluateSource("multi") pass. This drives MultiTriggerSource.EventReady
		// — the SAME eventReadySource the engine's cooldown gate switches on
		// (triggers.go), whose body delegates to scheduler.HasPendingEventMatch
		// — end-to-end, the multi sibling of the "bead" mode above. Like the
		// bead source, multi opts out of the runtime-bootstrap guard
		// (SkipRuntimeBootstrap), so the buffered event fires on this pass.
		src := bridge.NewMultiTriggerSource(logger, nil)
		var labels []string
		for l := range strings.SplitSeq(cfg.beadLabels, ",") {
			if l = strings.TrimSpace(l); l != "" {
				labels = append(labels, l)
			}
		}
		topic := "beads.bead." + cfg.beadEvent
		src.InjectBeadEvent(topic, &bridge.BeadEvent{
			ID:     cfg.beadID,
			Type:   cfg.beadType,
			Title:  cfg.beadTitle,
			Status: cfg.beadStatus,
			Labels: labels,
		})
		engine.RegisterSource(src)
		return "multi", func() {}

	default:
		fatal(fmt.Sprintf("unknown --source %q", source))
		return "", func() {}
	}
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(v)
}

func itoa(n int) string { return fmt.Sprintf("%d", n) }

func fatal(msg string) {
	fmt.Fprintln(os.Stderr, "trigger-probe: "+msg)
	os.Exit(2)
}
