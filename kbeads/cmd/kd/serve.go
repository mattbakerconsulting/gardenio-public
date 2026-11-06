package main

import (
	"context"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"net/http/pprof"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"gardenio/kbeads/internal/config"
	"gardenio/kbeads/internal/edgeauth"
	"gardenio/kbeads/internal/embedding"
	"gardenio/kbeads/internal/eventbus"
	"gardenio/kbeads/internal/events"
	"gardenio/kbeads/internal/hooks"
	"gardenio/kbeads/internal/landmap"
	"gardenio/kbeads/internal/presence"
	"gardenio/kbeads/internal/server"
	"gardenio/kbeads/internal/store/postgres"
	beadsync "gardenio/kbeads/internal/sync"
	"gardenio/kbeads/internal/tracing"
	"github.com/spf13/cobra"
	"go.opentelemetry.io/contrib/instrumentation/net/http/otelhttp"
)

var serveCmd = &cobra.Command{
	Use:     "serve",
	Short:   "Start the Beads gRPC server",
	GroupID: "system",
	// Override PersistentPreRunE so we don't create a gRPC client connection.
	// Still silence the usage dump on RunE failures (this override would
	// otherwise skip the root's SilenceUsage set); arg-parse errors fire
	// before this hook and keep their usage block. See ka-0000001023.
	PersistentPreRunE: func(cmd *cobra.Command, args []string) error { cmd.SilenceUsage = true; return nil },
	RunE: func(cmd *cobra.Command, args []string) error {
		logger := slog.New(slog.NewTextHandler(os.Stderr, nil))

		shutdownTracing, err := tracing.Init(context.Background(), logger, tracing.Options{
			ServiceName:    "kbeads",
			ServiceVersion: os.Getenv("BEADS_VERSION"),
			Environment:    os.Getenv("GARDENIO_ENV"),
		})
		if err != nil {
			return err
		}
		defer func() {
			shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			if serr := shutdownTracing(shutdownCtx); serr != nil {
				logger.Warn("tracing shutdown error", "error", serr)
			}
		}()

		// Load configuration.
		cfg, err := config.Load()
		if err != nil {
			return err
		}

		// The Lands API scans every answer against the contract's denylist
		// (gardenio_lands_denylist_hits_total); a list that does not build
		// stops the daemon here, not each request.
		if err := landmap.CheckDenylist(); err != nil {
			return fmt.Errorf("lands response denylist: %w", err)
		}

		// Connect to Postgres. When BEADS_DATABASE_PASSWORD_FILE is set, the
		// password is read from that (live-updating K8s Secret volume) file
		// per connection, so an RDS master-password rotation is picked up
		// without a pod restart — the rotation-tolerant reconnect fix for the
		// P0 CrashLoop (ka-0000004150 / ka-0000006811). Empty = legacy env mode.
		store, err := postgres.NewWithPasswordFile(cfg.DatabaseURL, cfg.DatabasePasswordFile)
		if err != nil {
			return err
		}

		// References: gate ka-id extraction from comment bodies behind a
		// config flag. Off by default until decision ka-0000000868 lands.
		store.SetExtractCommentReferences(cfg.ExtractCommentReferences)
		if cfg.ExtractCommentReferences {
			logger.Info("comment-text reference extraction enabled (BEADS_REFERENCES_EXTRACT_COMMENTS=1)")
		}

		// References: gate reconcile-on-reextract auto-delete of stale
		// parser-created parent-child dep rows behind a config flag. Off by
		// default (behavior-changing). See Proposal B (ka-0000006581).
		store.SetReconcileTypedMentionDeps(cfg.ReconcileTypedMentionDeps)
		if cfg.ReconcileTypedMentionDeps {
			logger.Info("typed-mention dep reconcile enabled (BEADS_REFERENCES_RECONCILE_DEPS=1)")
		}

		// Backfill fleet column for existing data if configured.
		if cfg.DefaultFleet != "default" {
			n, err := store.BackfillFleet(cmd.Context(), cfg.DefaultFleet)
			if err != nil {
				logger.Error("fleet backfill failed", "err", err)
			} else if n > 0 {
				logger.Info("fleet backfill complete", "fleet", cfg.DefaultFleet, "rows", n)
			}
		}

		// Create event publisher.
		var publisher events.Publisher
		if cfg.NATSURL != "" {
			pub, err := events.NewNATSPublisher(cfg.NATSURL, events.AuthOptions(cfg.NATSUser, cfg.NATSToken)...)
			if err != nil {
				store.Close()
				return err
			}
			publisher = pub
			logger.Info("events enabled", "nats_url", cfg.NATSURL, "nats_auth", cfg.NATSToken != "")
		} else {
			publisher = &events.NoopPublisher{}
			logger.Info("events disabled (BEADS_NATS_URL not set)")
		}

		// Create event bus with JetStream if NATS is available.
		// JetStream setup is deferred to a goroutine so the daemon can start
		// even when NATS is not yet reachable (e.g. during a fresh helm
		// install where the NATS Service DNS has not propagated). The NATS
		// client is in reconnecting state thanks to RetryOnFailedConnect; we
		// just wait for it to become Connected before creating streams.
		// See ka-0000000321.
		bus := eventbus.New()
		if pub, ok := publisher.(*events.NATSPublisher); ok {
			go func() {
				conn := pub.Conn()
				for !conn.IsConnected() {
					if conn.IsClosed() {
						return
					}
					time.Sleep(time.Second)
				}
				js, err := conn.JetStream()
				if err != nil {
					logger.Error("failed to create JetStream context", "err", err)
					return
				}
				if err := eventbus.EnsureStreams(js); err != nil {
					logger.Error("failed to ensure JetStream streams", "err", err)
					return
				}
				bus.SetJetStream(js)
				logger.Info("eventbus JetStream enabled")
			}()
		}

		// Load persisted external handlers from config table.
		allConfigs, err := store.ListAllConfigs(context.Background())
		if err != nil {
			logger.Warn("failed to list configs for handler loading", "err", err)
		} else {
			configMap := make(map[string]string, len(allConfigs))
			for _, c := range allConfigs {
				configMap[c.Key] = string(c.Value)
			}
			if n := bus.LoadPersistedHandlers(configMap); n > 0 {
				logger.Info("loaded persisted bus handlers", "count", n)
			}
		}

		// Build fleet config from env var map. `bead.fleet` is now an
		// audit-attribution field only — all tokens read across all fleets.
		// BEADS_CROSS_FLEET_READ_FLEETS and per-token cross_fleet_read flags
		// are silently ignored (kept parseable so existing operator configs
		// don't error on upgrade). Removed under epic ka-0000000871 / ka-0000003285.
		fleetCfg := server.NewFleetConfig(cfg.DefaultFleet, server.ParseFleetMap(cfg.FleetMap))
		if cfg.CrossFleetReadFleets != "" {
			logger.Info("BEADS_CROSS_FLEET_READ_FLEETS is deprecated and ignored; all tokens read across all fleets")
		}

		// Merge bridge tokens persisted in the config store. The canonical key
		// is bridge_token:<name> (ka-0000000310); the legacy fleet_token:<name>
		// key is dual-READ as a fallback during the compat window. New writes
		// use bridge_token:* — there is no eager backfill, so a host may carry
		// rows under either key. When BOTH keys exist for the same name we
		// prefer the canonical bridge_token:* row; the cleanup bead
		// (ka-0000004906) migrates legacy rows one-shot.
		//
		// Decoding (both persisted shapes, the canonical-over-legacy rule, the
		// expiry drop) lives in decodePersistedBridgeTokens, shared with the
		// re-sync loop started below.
		liveTokens, expiredTokens := decodePersistedBridgeTokens(allConfigs, time.Now())
		for _, t := range expiredTokens {
			logger.Info("skipping expired fleet token from config", "fleet", t.Name, "expires_at", t.ExpiresAtRaw)
		}
		for _, t := range liveTokens {
			if t.Legacy {
				fleetCfg.RegisterToken(t.Token, t.Name)
				logger.Info("loaded fleet token from config", "fleet", t.Name)
				continue
			}
			fleetCfg.RegisterTokenFull(t.Token, t.Name, t.Project, t.AllowedTypes, t.AllowedOps, t.ExpiresAt, t.Subject, t.Privileged)
			logger.Info("loaded fleet token from config", "fleet", t.Name, "project", t.Project, "has_expiry", !t.ExpiresAt.IsZero(), "has_subject", t.Subject != "", "privileged", t.Privileged)
		}

		// Designate privileged service fleets (ka-0000001946). Applied AFTER
		// both the fleet-map env tokens (NewFleetConfig) and the config-bead
		// tokens are registered, so every token of a listed fleet is re-marked
		// Privileged regardless of which path loaded it. This is what lets the
		// live gitlab-bridge token — loaded from the bridge-tokens Secret via
		// BEADS_FLEET_MAP, whose "token:fleet" format has no privileged field —
		// actually receive the ownership bypass instead of getting 403 on every
		// produces/triggered-by dep + mr_url back-fill.
		if len(cfg.PrivilegedFleets) > 0 {
			fleetCfg.SetPrivilegedFleets(cfg.PrivilegedFleets)
			logger.Info("designated privileged service fleets", "fleets", cfg.PrivilegedFleets)
		}

		// Register machine-principal clients (gd-gQ4iLIpWCm). Empty leaves the
		// machine door closed and the auth path unchanged.
		if len(cfg.MachineClients) > 0 {
			fleetCfg.SetMachineClients(cfg.MachineClients)
			logger.Info("registered machine principal clients", "clients", cfg.MachineClients)
		}

		// Initialize OIDC verifier if configured.
		// Collect all issuer URLs: BEADS_OIDC_ISSUER_URL (primary, e.g. Keycloak)
		// + BEADS_OIDC_ISSUER_URLS (additional, e.g. GitLab for CLI SSO).
		//
		// A failed init used to leave oidcVerifier nil for the pod's entire
		// lifetime — no retry, and no signal besides this log line (bug
		// gd-qZs1IbMhzQ: a misconfigured issuer scheme/port silently disabled
		// every "OIDC-only" rule with nothing paging on it). oidcVerifier is now
		// always an *server.OIDCVerifierHolder when any issuer is configured, so
		// NewGRPCServer/NewHTTPHandler capture one stable object whose
		// delegate can be installed later by oidcVerifierRetryLoop below — and
		// beads_oidc_verifier_healthy (internal/server/oidc_verifier_metrics.go)
		// is the persistent warning metric the bug asks for in place of gating
		// readiness (which would turn a partial auth-path degradation into a
		// hard outage on this single-replica daemon — see http.go
		// handleHealthLive).
		var oidcVerifier server.TokenVerifier
		// Always created (even when OIDC is unconfigured, or init succeeds
		// immediately) and deferred right here so the cancellation is
		// unconditionally reachable on every RunE return path, including the
		// early-return error paths below — go vet's lostcancel check wants the
		// defer next to the WithCancel call, not behind the conditional that
		// decides whether the retry goroutine actually runs.
		oidcRetryCtx, oidcRetryCancel := context.WithCancel(context.Background())
		defer oidcRetryCancel()
		var allIssuers []string
		if cfg.OIDCIssuerURL != "" {
			allIssuers = append(allIssuers, cfg.OIDCIssuerURL)
		}
		allIssuers = append(allIssuers, cfg.OIDCIssuerURLs...)
		// BEADS_OIDC_DISCOVERY_URL re-points discovery for the primary issuer
		// only; the issuer itself (and so the expected token "iss") is unchanged.
		var oidcDiscovery map[string]string
		if cfg.OIDCIssuerURL != "" && cfg.OIDCDiscoveryURL != "" {
			oidcDiscovery = map[string]string{cfg.OIDCIssuerURL: cfg.OIDCDiscoveryURL}
		}
		if len(allIssuers) > 0 {
			holder := server.NewOIDCVerifierHolder()
			oidcVerifier = holder
			// Per-client role ceiling (BEADS_CLIENT_ROLE_CEILING): wraps the
			// holder so a verifier installed later by the retry loop is capped
			// too. The Slack door (gardenio-slack-link) is capped at operator.
			if len(cfg.ClientRoleCeiling) > 0 {
				capped, err := server.NewClientRoleCeilingVerifier(holder, cfg.ClientRoleCeiling)
				if err != nil {
					return fmt.Errorf("BEADS_CLIENT_ROLE_CEILING: %w", err)
				}
				oidcVerifier = capped
				logger.Info("client role ceiling enabled", "ceilings", cfg.ClientRoleCeiling)
			}
			v, err := newOIDCVerifier(context.Background(), cfg.OIDCCAFile, allIssuers, oidcDiscovery, cfg.OIDCAudience, cfg.OIDCAllowedEmailDomains)
			server.RecordOIDCVerifierHealth(len(allIssuers), err)
			if err != nil {
				logger.Error("failed to initialize OIDC verifier — OIDC auth disabled, will retry", "issuers", allIssuers, "err", err)
				go oidcVerifierRetryLoop(oidcRetryCtx, logger, holder, cfg.OIDCCAFile, allIssuers, oidcDiscovery, cfg.OIDCAudience, cfg.OIDCAllowedEmailDomains)
			} else {
				holder.Set(v)
				logger.Info("OIDC auth enabled", "issuers", allIssuers, "audience", cfg.OIDCAudience, "allowedDomains", cfg.OIDCAllowedEmailDomains)
			}
		}

		// Create server components.
		beadsServer := server.NewBeadsServer(store, publisher)
		beadsServer.SetFleetAuth(fleetCfg)
		// The shared static bearer (BEADS_AUTH_TOKEN) and its rotation twin
		// (BEADS_PREVIOUS_TOKEN) are no longer accepted anywhere (gd-ke6PC7bRto).
		// A leftover value is inert; say so loudly so a stale chart or env file
		// is found.
		for _, k := range []string{"BEADS_AUTH_TOKEN", "BEADS_PREVIOUS_TOKEN"} {
			if os.Getenv(k) != "" {
				logger.Warn("static bearer token env is set but IGNORED: the shared static token is no longer accepted; remove it from the deployment", "env", k)
			}
		}
		beadsServer.SetEscalationCoordinatorClients(cfg.EscalationCoordinatorClients)
		logger.Info("escalation coordinator clients", "clients", cfg.EscalationCoordinatorClients)
		beadsServer.SetEscalationResultMachines(cfg.EscalationResultMachines)
		logger.Info("escalation result gate",
			"requires_approval", os.Getenv("BEADS_ESCALATION_RESULT_REQUIRES_APPROVAL"),
			"result_machines", cfg.EscalationResultMachines)
		// X-Auth-Request-Email stands in for a missing email claim only on
		// tokens minted for the edge forward-auth client, which is the OIDC
		// audience (oauth2-proxy / browser SSO). Any other azp (service
		// accounts, the device-grant client) is ignored (gd-uxLVWgL9qk).
		var forwardAuthClients []string
		if cfg.OIDCAudience != "" {
			forwardAuthClients = []string{cfg.OIDCAudience}
		}
		beadsServer.SetForwardAuthClients(forwardAuthClients)
		logger.Info("escalation approval page session", "clients", forwardAuthClients)
		// Writer-fleet policy for the trigger/schedule service-managed-fields
		// ownership bypass (cross-env cooldown starvation, ka-0000006237).
		beadsServer.SetServiceFieldPolicy(cfg.ServiceFieldFleets, cfg.ServiceFieldBypassRequireProject)
		if len(cfg.ServiceFieldFleets) > 0 || cfg.ServiceFieldBypassRequireProject {
			logger.Info("service-managed-field bypass policy active",
				"fleets", cfg.ServiceFieldFleets,
				"require_project", cfg.ServiceFieldBypassRequireProject)
		}
		// Only these fleet subjects' X-Operated-By relay counts for access
		// control; everyone else's is attribution only (P7 Task 3).
		beadsServer.SetBridgeSubjects(cfg.BridgeSubjects)
		// No Lands pin on an operator org's repo (gd-72m3STiqya).
		beadsServer.SetLandsOperatorGitHubOrgs(cfg.LandsOperatorGitHubOrgs)
		if len(cfg.LandsOperatorGitHubOrgs) > 0 {
			logger.Info("lands operator github orgs set", "orgs", cfg.LandsOperatorGitHubOrgs)
		}
		if len(cfg.RevokedTokens) > 0 {
			beadsServer.SetRevokedTokens(cfg.RevokedTokens)
			logger.Info("token revocation: seeded from env", "count", len(cfg.RevokedTokens))
		}
		if rs := beadsServer.RevocationStore(); rs != nil {
			if err := rs.Load(context.Background()); err != nil {
				logger.Warn("token revocation: failed to load from DB", "err", err)
			} else {
				logger.Info("token revocation: loaded persisted hashes", "total", rs.Size())
			}
		}

		// C1.2: refresh-token manager. Wired unconditionally — POST
		// /v1/auth/refresh requires it, and POST /v1/fleet-tokens with
		// with_refresh:true returns 503 when nil. Rotation is on by
		// default per decision ka-0000000806 (RFC 6749 §10.4 single-use
		// rotation); set BEADS_REFRESH_ROTATE=0 to disable.
		beadsServer.SetRefreshTokenManager(server.NewRefreshTokenManager(store, fleetCfg))
		if os.Getenv("BEADS_REFRESH_ROTATE") == "0" {
			logger.Info("refresh tokens: rotation disabled (BEADS_REFRESH_ROTATE=0)")
		} else {
			beadsServer.SetRefreshRotation(true)
			logger.Info("refresh tokens: rotation enabled (RFC 6749 §10.4)")
		}

		beadsServer.SetBus(bus)

		// Wire the in-house chart backend's VictoriaMetrics client. When
		// BEADS_VMSELECT_URL is set (e.g. the in-cluster vmsingle/vmselect
		// service), GET /v1/charts/uniqueness serves the DAU/WAU/MAU
		// query-time panels (task ka-0000006905), GET /v1/charts/engagement/*
		// serve the operator-engagement panels with server-side operator_id ->
		// display resolution (task ka-0000004824), and GET /v1/charts/security/*
		// serve the identity-free security-overview panels — authz denials,
		// decision/escalation lifecycle, and the v1-conditional token-revocation
		// SLO (task ka-0000001983). Unset → charts endpoints
		// return 503 and kbeads serves no charts. The optional bearer
		// BEADS_VMSELECT_TOKEN covers deployments that put vmselect behind
		// ForwardAuth.
		if vmURL := os.Getenv("BEADS_VMSELECT_URL"); vmURL != "" {
			beadsServer.SetVMClient(server.NewVMClient(vmURL, os.Getenv("BEADS_VMSELECT_TOKEN")))
			logger.Info("chart backend enabled", "vmselect_url", vmURL)
		}

		// Wire embedding backend for semantic search.
		embedder, err := embedding.NewEmbedder(cfg)
		if err != nil {
			publisher.Close()
			store.Close()
			return fmt.Errorf("embedding: %w", err)
		}
		beadsServer.SetEmbedder(embedder)
		// Wire the store-backed embedding stats provider so the /metrics
		// endpoint's coverage_ratio / stale_count gauges emit live series
		// instead of falling back to NoopStatsProvider (ka-0000003559).
		beadsServer.SetEmbeddingStatsProvider(server.NewStoreEmbeddingStatsProvider(store, cfg.DefaultFleet))
		logger.Info("embedding backend configured",
			"backend", cfg.EmbedBackend,
			"model", embedder.ModelName(),
			"dims", embedder.Dimensions(),
		)

		// Initialize Keycloak Admin client if configured (for session revocation).
		kcAdminCfg := server.KeycloakAdminConfig{
			BaseURL:      cfg.KeycloakAdminURL,
			Realm:        cfg.KeycloakAdminRealm,
			ClientID:     cfg.KeycloakAdminClientID,
			ClientSecret: cfg.KeycloakAdminClientSecret,
		}
		if kcAdminCfg.IsConfigured() {
			beadsServer.SetKeycloakAdmin(server.NewKeycloakAdminClient(kcAdminCfg))
			logger.Info("keycloak admin API enabled", "url", cfg.KeycloakAdminURL, "realm", cfg.KeycloakAdminRealm)
		}

		// How long a trigger-lineage seal stays honoured (default 30d).
		if v := os.Getenv("BEADS_SPAWN_LINEAGE_SEAL_TTL"); v != "" {
			ttl, err := time.ParseDuration(v)
			if err != nil || ttl <= 0 {
				publisher.Close()
				store.Close()
				return fmt.Errorf("BEADS_SPAWN_LINEAGE_SEAL_TTL: want a positive duration like 720h, got %q", v)
			}
			beadsServer.SetSpawnLineageSealTTL(ttl)
		}

		// Configure trusted proxy validation for X-Forwarded-For headers.
		if len(cfg.TrustedProxies) > 0 {
			tp, err := server.NewTrustedProxyConfig(cfg.TrustedProxies)
			if err != nil {
				publisher.Close()
				store.Close()
				return fmt.Errorf("BEADS_TRUSTED_PROXIES: %w", err)
			}
			beadsServer.SetTrustedProxies(tp)
			logger.Info("trusted proxy validation enabled", "cidrs", cfg.TrustedProxies)
		}

		// Configure per-identity HTTP rate limiting.
		beadsServer.SetRateLimit(server.RateLimitConfig{
			Enabled:     cfg.RateLimitEnabled,
			MutateRPS:   cfg.RateLimitMutateRPS,
			MutateBurst: cfg.RateLimitMutateBurst,
			ReadRPS:     cfg.RateLimitReadRPS,
			ReadBurst:   cfg.RateLimitReadBurst,
		})
		if cfg.RateLimitEnabled {
			logger.Info("rate limiting enabled",
				"mutate_rps", cfg.RateLimitMutateRPS,
				"mutate_burst", cfg.RateLimitMutateBurst,
				"read_rps", cfg.RateLimitReadRPS,
				"read_burst", cfg.RateLimitReadBurst,
			)
		} else {
			logger.Info("rate limiting disabled")
		}

		// Stage 3 / W-B-11 (ka-0000001678): daemon-side verification of the
		// gardenio-edge-auth HMAC envelope. Configured by env vars
		// DAEMON_HMAC_KEY and DAEMON_HMAC_VERIFY_ENFORCE — these come from
		// the chart's daemonHmacVerify section. When DAEMON_HMAC_KEY is
		// unset and enforce is false, EdgeAuthMiddleware is not installed.
		edgeAuthCfg := edgeauth.ConfigFromEnv()
		if edgeAuthCfg.Enabled() || edgeAuthCfg.Enforce {
			beadsServer.SetEdgeAuth(edgeAuthCfg)
			logger.Info("edge-auth HMAC verify enabled",
				"enforce", edgeAuthCfg.Enforce,
				"key_present", edgeAuthCfg.Enabled(),
			)
		} else {
			logger.Info("edge-auth HMAC verify disabled (no DAEMON_HMAC_KEY)")
		}

		// Free-htmx widget startup compile (ka-0000002807). Walk every
		// type=widget-type bead with renderer_kind=htmx and parse its
		// template_source. Failure aborts startup so a malformed widget
		// never reaches the renderer. Tolerated when BEADS_WIDGET_COMPILE_FAIL_OPEN=1
		// is set (operator override for emergency rollback).
		if err := server.CompileAllWidgetTemplates(context.Background(), store); err != nil {
			if os.Getenv("BEADS_WIDGET_COMPILE_FAIL_OPEN") == "1" {
				logger.Error("widget startup compile failed (fail-open override active)", "err", err)
			} else {
				publisher.Close()
				store.Close()
				return fmt.Errorf("widget startup compile: %w", err)
			}
		}

		grpcServer := server.NewGRPCServer(beadsServer, oidcVerifier, fleetCfg, server.GRPCServerOptions{
			EnableReflection: cfg.GRPCReflection,
		})
		if cfg.GRPCReflection {
			logger.Warn("gRPC reflection enabled — disable in production via BEADS_GRPC_REFLECTION")
		}

		// Start gRPC listener.
		lis, err := net.Listen("tcp", cfg.GRPCAddr)
		if err != nil {
			publisher.Close()
			store.Close()
			return err
		}

		go func() {
			logger.Info("gRPC server listening", "addr", cfg.GRPCAddr)
			if err := grpcServer.Serve(lis); err != nil {
				logger.Error("gRPC server error", "err", err)
			}
		}()

		// Start HTTP server.
		httpHandler := beadsServer.NewHTTPHandler(oidcVerifier, fleetCfg)
		// otelhttp wraps the daemon handler so each inbound request becomes a
		// server span continuing the trace context the controller/bridges
		// injected. No-op until tracing.Init installs a provider. The SSE event
		// stream is filtered out: a single long-lived connection would otherwise
		// hold one span open for its entire (effectively unbounded) lifetime.
		var tracedHandler http.Handler = otelhttp.NewHandler(httpHandler, "kbeads-http",
			otelhttp.WithFilter(func(r *http.Request) bool {
				return !strings.HasPrefix(r.URL.Path, "/v1/events/stream")
			}),
		)
		httpServer := &http.Server{
			Addr:              cfg.HTTPAddr,
			Handler:           tracedHandler,
			ReadHeaderTimeout: 30 * time.Second,
			IdleTimeout:       120 * time.Second,
			// WriteTimeout is intentionally omitted (0 = no limit) so that
			// long-lived SSE connections on /v1/events/stream are not killed.
			// Per-request write deadlines are enforced by requestTimeoutMiddleware
			// using http.ResponseController for non-streaming endpoints.
		}

		go func() {
			logger.Info("HTTP server listening", "addr", cfg.HTTPAddr)
			if err := httpServer.ListenAndServe(); err != nil && err != http.ErrServerClosed {
				logger.Error("HTTP server error", "err", err)
			}
		}()

		// Start metrics server on a separate internal-only port. The metrics
		// context bounds the lifetime of the DB-backed collectors' background
		// refreshers; it is cancelled during graceful shutdown so they stop
		// cleanly (ka-0000006261).
		metricsCtx, metricsCancel := context.WithCancel(context.Background())
		metricsMux := http.NewServeMux()
		metricsMux.Handle("GET /metrics", beadsServer.MetricsHandler(metricsCtx))
		metricsMux.HandleFunc("/debug/pprof/", pprof.Index)
		metricsMux.HandleFunc("/debug/pprof/cmdline", pprof.Cmdline)
		metricsMux.HandleFunc("/debug/pprof/profile", pprof.Profile)
		metricsMux.HandleFunc("/debug/pprof/symbol", pprof.Symbol)
		metricsMux.HandleFunc("/debug/pprof/trace", pprof.Trace)
		metricsServer := &http.Server{
			Addr:              cfg.MetricsAddr,
			Handler:           metricsMux,
			ReadHeaderTimeout: 5 * time.Second,
		}
		go func() {
			logger.Info("metrics server listening", "addr", cfg.MetricsAddr)
			if err := metricsServer.ListenAndServe(); err != nil && err != http.ErrServerClosed {
				logger.Error("metrics server error", "err", err)
			}
		}()

		// Start sync scheduler if any destinations are configured.
		var scheduler *beadsync.Scheduler
		if cfg.SyncInterval > 0 {
			var dests []beadsync.Destination

			if cfg.SyncS3Bucket != "" {
				s3Dest, err := beadsync.NewS3Destination(
					context.Background(),
					cfg.SyncS3Bucket,
					cfg.SyncS3Key,
					cfg.SyncS3Region,
					cfg.SyncS3Endpoint,
				)
				if err != nil {
					logger.Error("failed to create S3 sync destination", "err", err)
				} else {
					dests = append(dests, s3Dest)
					logger.Info("sync S3 destination enabled", "bucket", cfg.SyncS3Bucket, "key", cfg.SyncS3Key)
				}
			}

			if cfg.SyncGitRepo != "" {
				gitDest := beadsync.NewGitDestination(cfg.SyncGitRepo, cfg.SyncGitFile, cfg.SyncGitBranch)
				dests = append(dests, gitDest)
				logger.Info("sync git destination enabled", "repo", cfg.SyncGitRepo, "file", cfg.SyncGitFile)
			}

			if len(dests) > 0 {
				scheduler = beadsync.NewScheduler(store, dests, cfg.SyncInterval, logger)
				scheduler.Start()
				logger.Info("sync scheduler started", "interval", cfg.SyncInterval)
			}
		}

		// Start advice hooks subscriber if NATS is available. Like the
		// JetStream setup above, this waits for the NATS connection to
		// become ready so a slow-DNS startup doesn't block the daemon.
		// See ka-0000000321.
		var hooksCancel context.CancelFunc
		if cfg.NATSURL != "" {
			hooksSub, err := events.NewNATSSubscriber(cfg.NATSURL)
			if err != nil {
				logger.Error("failed to create hooks subscriber", "err", err)
			} else {
				hooksHandler := hooks.NewHandler(store, logger)
				var hooksCtx context.Context
				hooksCtx, hooksCancel = context.WithCancel(context.Background())
				go func() {
					conn := hooksSub.Conn()
					for !conn.IsConnected() {
						if conn.IsClosed() || hooksCtx.Err() != nil {
							hooksSub.Close()
							return
						}
						time.Sleep(time.Second)
					}
					if err := hooksHandler.StartSubscriber(hooksCtx, hooksSub); err != nil {
						logger.Error("hooks subscriber error", "err", err)
					}
					hooksSub.Close()
				}()
				logger.Info("hooks subscriber scheduled (waits for NATS connect)")
			}
		}

		// Start audit retention sweeper.
		auditRetentionCtx, auditRetentionCancel := context.WithCancel(context.Background())

		// Re-read persisted bridge tokens so one registered on another daemon
		// process after this one loaded the store (a rolling update's overlap)
		// becomes valid here within one interval, not at the next restart
		// (gd-PPzLrS2k8e). Stops with the other sweepers on auditRetentionCancel.
		if cfg.BridgeTokenResyncInterval > 0 {
			go bridgeTokenResyncLoop(auditRetentionCtx, store, fleetCfg, logger, cfg.BridgeTokenResyncInterval)
			logger.Info("bridge token re-sync enabled", "interval", cfg.BridgeTokenResyncInterval)
		}
		if cfg.AuditRetentionDays > 0 {
			beadsServer.StartAuditRetention(auditRetentionCtx, cfg.AuditRetentionDays)
			logger.Info("audit retention enabled", "retention_days", cfg.AuditRetentionDays)
		}

		// Start slow-tool-call ledger retention reaper (gd-4psiVnwXdj).
		if cfg.SlowToolCallsRetentionDays > 0 {
			beadsServer.StartSlowToolCallsRetention(auditRetentionCtx, cfg.SlowToolCallsRetentionDays)
			logger.Info("slow tool calls retention enabled", "retention_days", cfg.SlowToolCallsRetentionDays)
		}

		// Prune old trigger/agent bead.updated events and fields history
		// (gd-1F8R0SQtiQ).
		if cfg.HistoryRetentionDays > 0 {
			beadsServer.StartHistoryRetention(auditRetentionCtx, cfg.HistoryRetentionDays)
			logger.Info("history retention enabled", "retention_days", cfg.HistoryRetentionDays)
		}

		// Start decision-aging auto-default sweeper. Ships dark: only runs when
		// BEADS_DECISION_AGING_ENABLED is set. Drains stale, author-opted-in
		// decisions past the >14d threshold so the operator queue stops accruing
		// unbounded (ka-0000003434).
		if server.DecisionAgingEnabled() {
			beadsServer.StartDecisionAging(auditRetentionCtx)
			logger.Info("decision aging enabled", "threshold_days", server.DecisionAgingDays())
		}

		// Start spawn-policy sweeper. Ships dark: only runs when
		// BEADS_SPAWN_POLICY_ENABLED is set. Auto-approves privileged-role
		// agent spawns that match a declared, capped type:spawn_policy rule
		// instead of always requiring a human (gd-vmLhgqydEa).
		if server.SpawnPolicyEnabled() {
			beadsServer.StartSpawnPolicySweep(auditRetentionCtx)
			logger.Info("spawn policy evaluator enabled")
		}

		// Escalation routing sweep: stamps held_by & co. on open escalations
		// filed before routing shipped (escalation_policy.go).
		beadsServer.StartEscalationRouteSweep(auditRetentionCtx)

		// Start presence tracker reaper for agent roster.
		beadsServer.Presence.StartReaper(&presence.ReaperConfig{
			DeadThreshold: 15 * time.Minute,
			EvictAfter:    30 * time.Minute,
			SweepInterval: 60 * time.Second,
		})

		// Log startup info.
		logger.Info("beads server started",
			"grpc_addr", cfg.GRPCAddr,
			"http_addr", cfg.HTTPAddr,
			"metrics_addr", cfg.MetricsAddr,
			"auth_enabled", fleetCfg.HasTokens() || oidcVerifier != nil,
		)

		// Wait for SIGINT or SIGTERM.
		sigCh := make(chan os.Signal, 1)
		signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
		sig := <-sigCh
		logger.Info("received signal, shutting down", "signal", sig)

		// Graceful shutdown.
		auditRetentionCancel()

		oidcRetryCancel()
		logger.Info("oidc verifier retry loop stopped")

		beadsServer.Presence.Stop()
		logger.Info("presence tracker stopped")

		if hooksCancel != nil {
			hooksCancel()
			logger.Info("hooks subscriber stopped")
		}

		if scheduler != nil {
			scheduler.Stop()
			logger.Info("sync scheduler stopped")
		}

		grpcServer.GracefulStop()
		logger.Info("gRPC server stopped")

		metricsCancel() // stop the DB-backed collector refreshers
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if err := metricsServer.Shutdown(shutdownCtx); err != nil {
			logger.Error("metrics server shutdown error", "err", err)
		}
		logger.Info("metrics server stopped")
		if err := httpServer.Shutdown(shutdownCtx); err != nil {
			logger.Error("HTTP server shutdown error", "err", err)
		}
		logger.Info("HTTP server stopped")

		if err := publisher.Close(); err != nil {
			logger.Error("error closing publisher", "err", err)
		}
		if err := store.Close(); err != nil {
			logger.Error("error closing store", "err", err)
		}

		logger.Info("shutdown complete")
		return nil
	},
}

// newOIDCVerifier runs OIDC discovery for issuers, trusting caFile
// (BEADS_OIDC_CA_FILE) beside the system roots when set. The CA is re-read on
// every call, so the retry loop recovers once a not-yet-issued CA appears.
func newOIDCVerifier(ctx context.Context, caFile string, issuers []string, discoveryURLs map[string]string, audience string, allowedDomains []string) (server.TokenVerifier, error) {
	ctx, err := server.OIDCCAContext(ctx, caFile)
	if err != nil {
		return nil, err
	}
	return server.NewMultiOIDCVerifierWithDiscovery(ctx, issuers, discoveryURLs, audience, allowedDomains...)
}

// oidcVerifierRetryInterval is the polling cadence for oidcVerifierRetryLoop,
// matching the refresh cadence already used for the other background
// DB-backed metric collectors in this daemon (internal/server/http_metrics.go
// newFleetCollector).
const oidcVerifierRetryInterval = 30 * time.Second

// oidcVerifierRetryLoop retries OIDC provider discovery on a ticker until it
// succeeds or ctx is cancelled (pod shutdown), so a transient issuer outage
// at startup (e.g. Keycloak not yet reachable, or — the acceptance case for
// gd-qZs1IbMhzQ — a deploy-values fix landing without a pod restart) doesn't
// disable OIDC auth for the pod's entire remaining lifetime. It exits after
// the first success: holder.Set installs the working verifier for every
// request from then on, and there is nothing further for this loop to do —
// re-detecting a *later* degradation would need the same
// gate-readiness-on-a-non-fatal-condition tradeoff this bug's fix explicitly
// avoids (see the comment at the call site).
func oidcVerifierRetryLoop(ctx context.Context, logger *slog.Logger, holder *server.OIDCVerifierHolder, caFile string, issuers []string, discoveryURLs map[string]string, audience string, allowedDomains []string) {
	ticker := time.NewTicker(oidcVerifierRetryInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			v, err := newOIDCVerifier(ctx, caFile, issuers, discoveryURLs, audience, allowedDomains)
			server.RecordOIDCVerifierHealth(len(issuers), err)
			if err != nil {
				logger.Error("oidc verifier retry failed — OIDC auth still disabled", "issuers", issuers, "err", err)
				continue
			}
			holder.Set(v)
			logger.Info("OIDC auth enabled (recovered after retry)", "issuers", issuers, "audience", audience, "allowedDomains", allowedDomains)
			return
		}
	}
}
