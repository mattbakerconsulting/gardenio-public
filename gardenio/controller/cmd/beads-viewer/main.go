// Command beads-viewer is a web UI daemon for viewing beads.
// It renders beads in a deep-linkable HTML format, gated by Keycloak OIDC.
package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/collectors"
	"github.com/prometheus/client_golang/prometheus/promhttp"

	"gardenio/controller/cmd/beads-viewer/viewer"
	"gardenio/controller/internal/beadsapi"
	"gardenio/controller/internal/bridge"
	"gardenio/controller/internal/commons"
	"gardenio/controller/internal/conversation"
	"gardenio/controller/internal/coopmuxapi"
	"gardenio/controller/internal/covflush"
	"gardenio/controller/internal/decisionview"
	"gardenio/controller/internal/gardenhub"
	"gardenio/controller/internal/gardenhub/captioner"
	"gardenio/controller/internal/glass"
	"gardenio/controller/internal/gzipmw"
	"gardenio/controller/internal/mailer"
	"gardenio/controller/internal/oidcauth"
	"gardenio/controller/internal/servertiming"
	"gardenio/controller/internal/steward"
)

var (
	version = "dev"
	commit  = "unknown"
)

func main() {
	defer covflush.Flush()
	bridge.NudgeSender = "beads-viewer" // from: label on the mail bridge.NudgeAgent sends
	cfg, err := parseConfig()
	if err != nil {
		fmt.Fprintf(os.Stderr, "beads-viewer: %v\n", err)
		os.Exit(1)
	}

	logger := setupLogger(cfg.logLevel)
	logger.Info("starting beads-viewer",
		"version", version,
		"commit", commit,
		"beads_http", cfg.beadsHTTPAddr,
		"ziti_service", cfg.zitiService,
		"listen_addr", cfg.listenAddr)

	// Fail-fast authn config validation (ka-0000000212, epic ka-0000002784):
	// reject obviously-broken combinations (both auth paths off without dev
	// bypass, dev bypass + real auth set together, malformed AVA ARN or
	// region, non-https OIDC issuer) at startup so they surface as a crash
	// instead of as runtime 403s on protected routes. Exit 2 distinguishes
	// "config invalid" from a generic crash.
	if err := oidcauth.Validate(oidcauth.Config{
		IssuerURL:           cfg.oidcIssuerURL,
		AllowedEmailDomains: cfg.oidcAllowedEmailDomains,
		AVA: oidcauth.AVAConfig{
			TrustSigner: cfg.avaTrustSigner,
			Region:      cfg.avaRegion,
		},
		K8sSA: cfg.k8sSAConfig(),
	}, oidcauth.ValidateOptions{DevAuthBypassEnabled: cfg.devAuthBypass}); err != nil {
		logger.Error("authn config invalid — refusing to start", "error", err)
		os.Exit(2)
	}

	// Read once and pass to both the daemon HTTP client and the Server.
	// The Server still needs the raw token for the gb-prime subprocess
	// (prime.go) and the /health snapshot fetch (health.go), but reading the
	// env var twice was the kind of duplication the secrets audit flagged
	// (ka-0000004498, source ka-0000002836 §5.B).
	beadsDaemonToken := os.Getenv("BEADS_DAEMON_TOKEN")

	daemon, err := beadsapi.New(beadsapi.Config{
		HTTPAddr:          cfg.beadsHTTPAddr,
		Token:             beadsDaemonToken,
		ZitiIdentityPath:  cfg.zitiIdentityPath,
		ZitiService:       cfg.zitiService,
		ZitiTLSServerName: cfg.zitiTLSServerName,
	})
	if err != nil {
		logger.Error("failed to create beads daemon client", "error", err)
		os.Exit(1)
	}
	daemon.SetOnBeadList(observeBeadList)
	defer daemon.Close()

	// Write-bearer enforcement (ka-0000006379): when enabled, mutating daemon
	// calls fail closed unless they carry the human user's own OIDC bearer,
	// which then overrides the (downscoped, read-only) daemon token on the
	// wire — so a write authenticates and attributes AS THE USER via the
	// daemon's OIDC RBAC path. Opt-in so it lands as an operator-coordinated
	// step alongside provisioning a RoleViewer read token; default off keeps
	// today's static-token write path until the deployment flips the flag.
	if cfg.requireUserBearerForWrites {
		daemon.SetRequireUserBearerForWrites(true)
		logger.Info("write-bearer enforcement enabled — daemon writes require the per-request user OIDC bearer (BEADS_VIEWER_REQUIRE_USER_BEARER_WRITES=1)")
	}

	// Read-bearer enforcement (gd-akDjTS2eh7): the read-side twin of the flag
	// above. Daemon reads authenticate as the signed-in user's bearer, so the
	// daemon's project scoping applies to what the viewer renders; a read with
	// no bearer fails closed unless it is marked beadsapi.WithSystemRead (the
	// feed hub and startup caches). Default off: the deployment flips it after
	// every viewer route is classified.
	if cfg.requireUserBearerForReads {
		daemon.SetRequireUserBearerForReads(true)
		logger.Info("read-bearer enforcement enabled — daemon reads require the per-request user OIDC bearer or an explicit system-read marker (BEADS_VIEWER_REQUIRE_USER_BEARER_READS=1)")
	}

	// Edge-auth HMAC signing (decision ka-0000000960 → edge-auth-envelope):
	// when set, viewer writes triggered by an authenticated human sign an
	// edge-auth envelope asserting that human's operator identity (stamped by
	// operatorMiddleware via beadsapi.WithEdgeAuthOperator). The daemon's
	// EdgeAuthMiddleware then verifies the HMAC and stamps
	// AuthIdentity{Type:"edge", Subject:"oidc:<user>"} — so an AVA-routed write
	// (which carries NO forwardable OIDC bearer, AccessTokenFromRequest == "")
	// still attributes to the real human instead of the viewer's shared daemon
	// token. This mirrors slack-bridge (cmd/slack-bridge/main.go), the only
	// other component on this path.
	//
	// The key MUST be the same secret configured on the daemon's edge-auth
	// verifier (DAEMON_HMAC_KEY, from gardenio-edge-auth-hmac). Empty = signing
	// disabled (pre-rollout safe): with no key the client signs nothing and
	// writes continue on today's static-token path unchanged. This wiring is
	// deliberately inert-by-default and lands SEPARATELY from the prod token
	// downscope + write-bearer enable (those are prod-auth-mutating and gated
	// on a staging burn-in — see task ka-0000000898 Part 5).
	if key := os.Getenv("BEADS_VIEWER_EDGE_AUTH_HMAC_KEY"); key != "" {
		daemon.SetEdgeAuthHMACKey([]byte(key))
		logger.Info("edge-auth HMAC signing enabled for beads-viewer")
	}

	coopmuxClient := coopmuxapi.New(coopmuxapi.Config{
		BaseURL: cfg.coopmuxURL,
		Token:   os.Getenv("COOP_MUX_AUTH_TOKEN"),
		// The Glass relay's streams (gd-O31EYiDGzm) sign an edge-auth v2 envelope naming the human with the relay key,
		// which only coopmux verifies (DAEMON_HMAC_KEY_RELAY), never the v2 key the beads daemon trusts (gd-9ZGh01W5ub).
		// Unset, every Glass stream fails closed and ends "unavailable"; the rest of the client is unaffected.
		RelayHMACKey: []byte(os.Getenv("COOPMUX_RELAY_HMAC_KEY")),
	})
	if coopmuxClient.Disabled() {
		logger.Info("coopmux client disabled (COOPMUX_URL unset) — agent terminal first-paint will be empty")
	} else {
		logger.Info("coopmux client enabled", "base_url", cfg.coopmuxURL)
	}

	if missing := missingVendoredJS(); len(missing) > 0 {
		logger.Warn("vendored JS not embedded; run cmd/beads-viewer/fetch-vendored.sh before building", "missing", missing)
	}

	srv := NewServer(daemon, logger, cfg.basePath, cfg.beadsHTTPAddr, beadsDaemonToken, coopmuxClient)
	srv.theme = themeFromEnv(os.Getenv("BEADS_VIEWER_THEME"))
	// The Lands GitHub App's setup leg (lands_github.go). Unset leaves it off
	// and a partial config is refused; either way /lands/github/* answers 503.
	if lg, err := landsGitHubFromEnv(os.Getenv, os.ReadFile); err != nil {
		logger.Error("lands github app misconfigured; /lands/github/setup refuses until fixed", "error", err)
	} else if lg != nil {
		srv.landsGitHub = lg
		logger.Info("lands github app enabled", "slug", lg.slug, "allow_private_repos", lg.allowPrivate)
	}

	// Machine principal (gd-Hc34EENbO9, decision gd-ZsfxvXRiyv "tight"): the
	// viewer's OWN Keycloak service account, used for exactly the writes no
	// human triggers — the plan-label and plan-step reactors below. Every
	// human-driven write keeps carrying the signed-in user's bearer (above).
	// The three BEADS_OIDC_* variables are rendered by
	// templates/beads-viewer/deployment.yaml when beadsViewer.machinePrincipal
	// is enabled; unset = the reactors stay on the client's default credential.
	// The token is fetched lazily on the first reactor write (and refreshed at
	// half-life); a fetch failure fails that write closed with auth_unavailable
	// — it never falls back to BEADS_DAEMON_TOKEN. The same client is used, so
	// ziti options never apply to a machine call differently from any other.
	machineRoles := []string{"gardenio-machine-plan-reactor"}
	if landsGateEnabledFromEnv() {
		machineRoles = append(machineRoles, "gardenio-machine-lands-gate")
	}
	if machineTS, ok, err := beadsapi.MachineTokenSourceFromEnv(context.Background(), logger, beadsapi.WithExpectedRoles(machineRoles...)); err != nil {
		logger.Error("machine principal misconfigured", "error", err)
		os.Exit(1)
	} else if ok {
		srv.SetMachinePrincipal(machineTS)
		// Everything with no human behind it — the SSE streams, the WithSystemRead
		// caches, the health/charts proxies and gb prime — authenticates as the
		// same service account, so BEADS_DAEMON_TOKEN (and the fleet token behind
		// it) can be retired (gd-glMVEH8phE).
		daemon.SetSystemPrincipal(machineTS)
		logger.Info("machine principal enabled", "client", os.Getenv("BEADS_OIDC_CLIENT_ID"), "scope", "plan-label, plan-step and plan-answer reactors")
		if !cfg.requireUserBearerForWrites {
			logger.Warn("machine principal is enabled but BEADS_VIEWER_REQUIRE_USER_BEARER_WRITES is not: human-driven writes still go out on the static daemon token")
		}
	} else {
		logger.Info("machine principal disabled")
	}
	// The Lands plot application (lands_apply.go, step 7.7). It files
	// plot_application beads as the machine principal above, so it is wired
	// only with one, and only once the deployment turns it on
	// (LANDS_APPLY_ENABLED) after granting that principal
	// gardenio-machine-lands-gate. Off, /lands/apply answers 503.
	if landsGateEnabledFromEnv() {
		switch {
		case srv.machinePrincipal == nil:
			logger.Error("LANDS_APPLY_ENABLED is set but the viewer has no machine principal; /lands/apply stays closed")
		default:
			mail, tpl, err := landsMailerFromEnv(context.Background())
			if err != nil {
				logger.Error("lands mailer misconfigured; /lands/apply stays closed", "error", err)
				break
			}
			gate, err := landsGateFromEnv(os.Getenv, daemon, srv.machinePrincipal, mail, tpl, logger)
			if err != nil {
				logger.Error("lands apply misconfigured; /lands/apply stays closed", "error", err)
				break
			}
			srv.landsGate = gate
			logger.Info("lands apply enabled", "daily_limit", gate.dailyLimit, "default_credits", gate.defaultCredits, "mail", mailer.Enabled(mail))
		}
	}
	// The demo land's door (lands_demo.go, gd-c6LzZkk0qy): a guest pass for
	// anyone signed in with GitHub, filed as land_invite beads on the same
	// machine principal, so it is wired only with one.
	if os.Getenv(landsDemoLandEnv) != "" {
		switch demo, err := landsDemoFromEnv(os.Getenv, daemon, daemon, srv.machinePrincipal, logger); {
		case srv.machinePrincipal == nil:
			logger.Error(landsDemoLandEnv + " is set but the viewer has no machine principal; /lands/demo stays closed")
		case err != nil:
			logger.Error("lands demo misconfigured; /lands/demo stays closed", "error", err)
		default:
			srv.landsDemo = demo
			logger.Info("lands demo enabled", "land", demo.land, "max_guests", demo.maxGuests, "guest_ttl", demo.guestTTL)
		}
	}
	// The gatehouse (lands_gatehouse.go, step 9.12). A bad name is logged and
	// leaves it off: "/" stays the feed for everyone.
	if gh, err := landsGatehouseFromEnv(os.Getenv); err != nil {
		logger.Error("lands gatehouse misconfigured; \"/\" does not open it", "error", err)
	} else if gh != "" {
		srv.landsGatehouse = gh
		logger.Info("lands gatehouse enabled", "land", gh)
	}
	// The Lands home's "See a live plan" link (gd-PswcRiwYwS). A bad value
	// is logged and leaves the link off: no link beats a broken one.
	if live, err := landsLivePlanURL(os.Getenv(landsLivePlanEnv)); err != nil {
		logger.Error("lands live plan link misconfigured; the Lands home has none", "error", err)
	} else if live != "" {
		srv.landsLivePlan = live
		logger.Info("lands live plan link enabled", "url", live)
	}
	// Its "See the whole farm" link, a land window (LW.6, gd-ShRny4U2Ad), on
	// the same terms.
	if farm, err := landsLiveFarmURL(os.Getenv(landsLiveFarmEnv)); err != nil {
		logger.Error("lands live farm link misconfigured; the Lands home has none", "error", err)
	} else if farm != "" {
		srv.landsLiveFarm = farm
		logger.Info("lands live farm link enabled", "url", farm)
	}
	srv.coopmuxPublicURL = cfg.coopmuxPublicURL
	srv.slackWorkspaceDomain = cfg.slackWorkspaceDomain
	srv.escalationReviewURL = cfg.escalationReviewURL
	srv.grafanaBaseURL = cfg.grafanaBaseURL
	srv.SetViewerProject(cfg.project)
	if cfg.project == "" {
		logger.Warn("no viewer project configured (BOAT_PROJECT unset) — /claude-config cannot reflect project-scoped claude-settings beads; set beadsViewer.project or global.project in the chart")
	}
	// Dark-launch the automated crit-style inline plan-critique panel
	// (prototype, task ka-0000002614). Off unless
	// BEADS_VIEWER_PLAN_CRITIQUE_FINDINGS=1. Read once here so the hot page/SSE
	// path never touches the environment.
	srv.planCritiqueFindingsEnabled = planCritiqueFindingsEnabledFromEnv()
	if srv.planCritiqueFindingsEnabled {
		logger.Info("automated plan-critique findings panel enabled (BEADS_VIEWER_PLAN_CRITIQUE_FINDINGS=1)")
	}

	// G2 / ka-0000003651 — viewer-identity resolver primitive. Required by
	// the inbox v1 surface (G3 / ka-0000006888); without it /inbox renders
	// the Mode A blocking banner.
	srv.SetResolver(viewer.NewResolver(daemon))

	// Mezmo live-data client for incident-intel widgets carrying a
	// `mezmo_source` field (ka-0000002519). Both env vars must be set; missing
	// either falls through to the no-op client and widgets render from baked
	// fields. Credential strategy is decided in ka-0000007074.
	if mc := NewHTTPMezmoClient(os.Getenv("MEZMO_VIEWER_BASE_URL"), os.Getenv("MEZMO_VIEWER_SERVICE_KEY")); mc != nil {
		srv.SetMezmoClient(mc)
		logger.Info("mezmo viewer client enabled")
	}

	// Live GitLab client for the /index "Merge requests" rail panel
	// (ka-0000005424). Path 1 in the bead: live REST queries with a 60s
	// server-side cache. All three env vars must be set; missing any falls
	// through to an explanatory empty state on the panel.
	if mrLister := newIndexMRLister(logger, cfg.gitlabBaseURL, cfg.gitlabAPIToken); mrLister != nil && cfg.gardenioProjectPath != "" {
		srv.SetMRLister(mrLister, cfg.gardenioProjectPath, cfg.gardenioProjectURL)
		logger.Info("index MR status panel enabled",
			"project", cfg.gardenioProjectPath,
			"base_url", cfg.gitlabBaseURL)
	}

	// /image/{key...} proxy (ka-0000006131). Build the S3 client and inject
	// it when COOP_S3_BUCKET is set. Without the env var (typical for local
	// dev without AWS creds), the route registers but returns 503 — the
	// renderer-side URL rewrite stays disabled because imageBucket is empty.
	// COOP_S3_IMAGES_BUCKET (gd-CmjVAU3vgd) moves images to their own
	// bucket; a miss there is retried in COOP_S3_BUCKET.
	sessionBucket := strings.TrimSpace(os.Getenv("COOP_S3_BUCKET"))
	imageBucket := strings.TrimSpace(os.Getenv("COOP_S3_IMAGES_BUCKET"))
	if imageBucket == "" {
		imageBucket = sessionBucket
	}
	if imageBucket != "" {
		s3Client, err := newImageProxyS3Client(context.Background())
		if err != nil {
			logger.Warn("image proxy disabled — failed to build S3 client", "error", err)
		} else {
			srv.SetImageProxy(s3Client, imageBucket, imageProxyPrefixFromEnv())
			srv.SetImageFallbackBucket(sessionBucket)
			logger.Info("image proxy enabled",
				"bucket", imageBucket,
				"fallback_bucket", srv.imageFallbackBucket,
				"prefix", imageProxyPrefixFromEnv(),
			)
		}
	} else {
		logger.Info("image proxy disabled (COOP_S3_BUCKET unset)")
	}

	// /cassettes wire-audit surface (Phase-7 S6, ka-0000006302). Build the
	// read-side cassette store from the same env contract the claudemore proxy
	// reads (CLAUDEMORE_CASSETTE_DIR, else COOP_S3_*). Unset leaves the page in
	// degraded mode (explicit empty state); a build error is logged and the page
	// degrades rather than failing viewer boot — observability is never a gate.
	if store, err := newCassetteStoreFromEnv(context.Background()); err != nil {
		logger.Warn("cassette audit surface disabled — failed to build store", "error", err)
	} else if store != nil {
		srv.SetCassetteStore(store)
		logger.Info("cassette audit surface enabled")
	} else {
		logger.Info("cassette audit surface disabled (no cassette store configured)")
	}

	// Read-only vmsingle range-query proxy (ka-0000002747) backing /charts.
	// Defaults to the in-cluster gardenio-vmsingle Service; override with
	// VMSINGLE_QUERY_URL. Wiring is unconditional — when vmsingle is
	// unreachable the handler simply returns 502/503, which is the right
	// signal for a panel rendered against a not-yet-enabled vmsingle.
	metricsUpstream := metricsQueryURLFromEnv(os.Getenv)
	srv.SetMetricsProxy(metricsUpstream)
	logger.Info("metrics proxy enabled", "upstream", metricsUpstream)

	// AVA-authenticated /grafana reverse proxy (Option C, decision
	// ka-0000004242; epic ka-0000000746). Opt-in: only when GRAFANA_UPSTREAM_URL
	// is set does the viewer front the in-cluster Grafana under its own
	// same-origin /grafana subpath, stamping the verified viewer identity into
	// Grafana's auth-proxy header. Unset (every env that has not opted in) leaves
	// the /grafana routes in degraded mode (503) — the routes still register with
	// a stable shape so the surface is discoverable. See grafana_proxy.go.
	grafanaUpstream := strings.TrimSpace(os.Getenv("GRAFANA_UPSTREAM_URL"))
	srv.SetGrafanaProxy(grafanaUpstream, logger)
	// GRAFANA_AUTH_PROXY_HEADER must match Grafana's GF_AUTH_PROXY_HEADER_NAME
	// (the chart passes grafana.auth.proxy.headerName). Unset keeps X-WEBAUTH-USER.
	srv.SetGrafanaAuthProxyHeader(os.Getenv("GRAFANA_AUTH_PROXY_HEADER"))
	if grafanaUpstream != "" {
		logger.Info("grafana proxy enabled", "upstream", grafanaUpstream, "auth_header", srv.grafanaAuthProxyHeader())
	} else {
		logger.Info("grafana proxy disabled (GRAFANA_UPSTREAM_URL unset)")
	}

	// Runtime-tunable config watcher (env→config-bead pilot, ka-0000003317).
	// Polls type=config beads titled "tunables" labeled component:beads-viewer.
	// Today's only consumer is the V-R4 §3 migration-banner sunset flag
	// (viewer.show_migration_banner) — flipping it false on the config bead
	// retires the Phase-0 banner without a deploy.
	tunables, err := beadsapi.NewConfigWatcher(daemon, "beads-viewer",
		beadsapi.WithLogger(logger.With("component", "config-watcher")))
	if err != nil {
		logger.Error("failed to create config watcher", "error", err)
		os.Exit(1)
	}
	// Start synchronously so the first request sees a populated cache when the
	// daemon is reachable; tolerate a transient first-load failure — the
	// poll loop will retry, and Get falls back to env/compiled until then.
	if err := tunables.Start(beadsapi.WithSystemRead(context.Background())); err != nil {
		logger.Warn("config watcher initial load failed (will retry)", "error", err)
	}
	srv.SetTunables(tunables)

	// Telepathy thinking-trace feed (epic ka-0000007154, task ka-0000007528).
	// Subscribe to coop.events.messages.*.thinking and fan each agent thinking
	// block out to the /telepathy SSE page. NewThinkingHub returns nil when
	// NATS_URL is unset, leaving the feed disabled — the SSE page then renders
	// an empty live stream. A connect failure is non-fatal: the viewer must
	// still serve every other surface even when NATS is down.
	thinkingHub := NewThinkingHub(cfg.natsURL, cfg.natsToken, cfg.natsUser, logger)
	if thinkingHub != nil {
		if err := thinkingHub.Start(); err != nil {
			logger.Warn("telepathy: failed to start thinking hub — feed disabled", "error", err)
			thinkingHub = nil
		} else {
			defer thinkingHub.Close()
		}
	} else {
		logger.Info("telepathy: NATS_URL unset — thinking feed disabled")
	}
	srv.SetThinkingHub(thinkingHub)

	// Plan-chat assistant-turn persistence was REHOMED to the controller
	// (task ka-0000006891, P3 of plan ka-0000000187). The always-on
	// assistant_text NATS consumer that mints assistant transcript turns
	// (internal/assistantturnminter) now runs in the controller process so plan
	// transcripts persist independent of whether a beads-viewer replica is up.
	// The viewer still mints the OPERATOR half synchronously in
	// handlePlanAssistantPrompt and renders the transcript; it no longer owns
	// the assistant-turn subscription.

	// Activity-feed fan-out hub (Phase 2, task ka-0000000643). One upstream
	// daemon EventStream("beads.bead.>") is fanned out to every /feed/events
	// SSE connection with a bounded warm-paint ring, so a promoted front page
	// holds a single upstream connection regardless of viewer count. Nil-safe:
	// the SSE handler falls back to a per-connection stream if this is unset.
	beadFeedHub := NewBeadFeedHub(daemon, logger)
	beadFeedHub.Start()
	defer beadFeedHub.Close()
	srv.SetBeadFeedHub(beadFeedHub)

	// /garden live page (gd-BKx5ddIilZ): in-memory fleet snapshot kept current
	// by the daemon event stream and pushed to browsers as versioned SSE
	// patches, so the page renders without touching the daemon per request.
	// Start() warms the snapshot synchronously (bounded) before we serve.
	gardenHub := gardenhub.New(daemon, cfg.basePath, logger)
	// The Homestead farm read model (gd-L9fjdMGV7P) rides the hub's single
	// upstream event subscription rather than opening a second one.
	gardenFarm := gardenhub.NewFarm(daemon, logger)
	// The goats' line arms (heraldry 5.10) come through the Yard's line-arms cache, not a second one.
	gardenFarm.ShareLineArms(gardenHub)
	// Per-project spend and budget remaining (gd-T6zwDjWLBI), read from the
	// same costs rollup /costs shows by project.
	gardenFarm.SetSpendSource(farmSpendSource{srv})
	// Each release-enabled project's newest releases for the truck (plan gd-UZKZ9H25UD step 5), from the daemon's
	// /v1/releases.
	gardenFarm.SetReleaseSource(gardenhub.DaemonReleases(daemon))
	// Bird captions (gd-tGAvOOGeoM, plan gd-jMl4oF2cJO): Haiku writes the caption of a gardener's Bash call that has no
	// description, its reply passing the hub's scrubber. Off unless FARM_CAPTION_HAIKU=on with LITELLM_URL and
	// FARM_CAPTION_LITELLM_KEY set; off, every caption is still counted by source.
	farmCaptioner := captioner.New(captioner.ConfigFromEnv(os.Getenv), captioner.WithScrub(gardenhub.ScrubCaption), captioner.WithLogger(logger))
	gardenFarm.SetCaptioner(farmCaptioner)
	// The captions' kill switch (FARM_CAPTIONS=off: the wire before captions), the wire cap and the transcript ring's
	// rows (gd-FOzMlrXga6; Helm beadsViewer.captions.* and beadsViewer.transcript.rows).
	captionSettings := gardenhub.CaptionSettingsFromEnv(os.Getenv)
	gardenFarm.SetCaptionSettings(captionSettings)
	logger.Info("farm captions", "on", !captionSettings.Off, "haiku", farmCaptioner.Enabled() && !captionSettings.Off,
		"wire_cap", captionSettings.WireCap, "transcript_rows", captionSettings.TranscriptRows)
	gardenHub.OnEvent(gardenFarm.InvalidateFor)
	gardenHub.Start()
	defer gardenHub.Close()
	gardenFarm.Start()
	defer gardenFarm.Close()
	// What each gardener is doing (gd-H4jk9KgUAG): the hook events on core NATS, and the telepathy thoughts, feed the
	// farm's activity events and gardeners[].doing. Off without NATS; a connect failure only leaves the farm still.
	// The same hook events feed each agent's jobs record in the Commons (commons/jobs.go, gd-qlk31NBV6o), set once the
	// Commons hub is up below.
	var commonsJobs atomic.Pointer[commons.Jobs]
	activityFeed := NewActivityFeed(cfg.natsURL, cfg.natsToken, cfg.natsUser, func(data []byte) {
		gardenFarm.ObserveHook(data)
		commonsJobs.Load().Observe(data)
	}, logger)
	if err := activityFeed.Start(); err != nil {
		logger.Warn("activity: failed to subscribe to hook events — gardeners will not animate", "error", err)
	} else {
		defer activityFeed.Close()
	}
	thinkingHub.OnThought(gardenFarm.ObserveThinking)
	thinkingHub.OnSay(gardenFarm.ObserveSay) // what each gardener said: a say row and a robin (gd-HHGjxt59Mr)
	gardenPage := &gardenhub.Handler{
		Hub:      gardenHub,
		Farm:     gardenFarm,
		Beads:    daemon,
		Evidence: decisionview.NewEvidenceCache(0),
		Links:    daemon,
		// The Queues panel (GET /garden/queue/{kind}, Lands WB-6) reads each tab with the viewer's own page builders.
		Queues: gardenQueues{srv},
		// The search panel (GET /garden/search, Lands WB-5) asks the daemon as /search and the Cmd-K palette do.
		Search: gardenSearch{srv},
		// The bundle panel (GET /garden/bundle/{id}, Lands WB-9) reads the documents /bundle/{id} shows.
		Bundles: gardenBundles{srv},
		// The plan cockpit (GET /garden/plan/{id}, Lands WB-7/WB-8) reads with /plans/{id}'s own builders; its writes are
		// the /plans/{id}/* POSTs asked for JSON.
		Plans: gardenPlans{srv},
		// The project panel (GET /garden/project/{name}, Lands WB-10) reads the project hub's sections, the project's
		// decisions and escalations as the Queues panel's rows, and the land's purse.
		Projects: gardenProjects{srv},
		// The gardener panel (GET /garden/gardener/{id}, Lands WB-11) reads with /gardener/{id}'s own builders; its writes are
		// the /gardener/{id}/* POSTs asked for JSON.
		Gardeners: gardenGardeners{srv},
		// The settings panel (GET /garden/settings, Lands WB-13) reads the viewer's user bead as /settings does; its save is
		// POST /settings with scope=garden.
		Settings: gardenSettings{srv},
		// The costs charts (GET /garden/charts/{set}, plan gd-gh5J0vVbJP 1.2) are built against the same Grafana base
		// /costs embeds from; unset, every card degrades to its words.
		GrafanaBaseURL: cfg.grafanaBaseURL,
		// A tenant land's credit charts (GET /garden/charts/costs-land, routeScoped, plan gd-gh5J0vVbJP step 3.3): the 3.2
		// credit series read from vmsingle, cut to the lands the viewer's scope admits before any read.
		Credits: landCreditSource{srv},
		InLand:  gardenInLand,
		// The land header's steward notes (GET /garden/notes/{land} and POST .../holds, routeScoped, gd-q4NgYNGpy8): read
		// and written on the viewer's own bearer, so kbeads' edit_holds gate checks the person, and a person's edit wakes
		// the steward. Holds is offered to the land's owner and members, not guests (owner Q3).
		Notes: daemon,
		MayEditHolds: func(r *http.Request, land string) bool {
			return land != "" && oidcauth.EmailFromContext(r.Context()) != "" && viewerScopeFromContext(r.Context()).may(land, conversation.ActionEditHolds)
		},
		// A Holds save the steward's own end-of-run write could not cap (steward.CapNotes) is refused.
		NotesOver: func(notes string) int { return steward.NotesOverBytes(notes, steward.NotesCapBytes) },
		// The Costs panel's openers (the Menu's Costs item, the Porch's purse chip; plan gd-gh5J0vVbJP step 2.3) are
		// offered where GET /garden/charts/{set} (routeAdminOnly) would answer: a viewer the scope gate leaves unscoped.
		CanSeeCosts: func(r *http.Request) bool { return !viewerScopeFromContext(r.Context()).Scoped() },
		// Become (plan gd-0SUIdKXWV3 step 3): an admin is offered Become beside a tenant, opening the Become page. The
		// admin role itself, not "unscoped": an operator and a roleless session are unscoped and no admin.
		IsAdmin:   viewerCanAdmin,
		BecomeURL: cfg.becomeURL,
		// While a Become session lasts (step 4), every view carries a banner naming the admin behind it, with Stop. The name
		// is the verified edge bearer's impersonator claim (attachEdgeBearerRoles), never a header.
		Impersonator: viewerImpersonator,
		StopURL:      cfg.stopBecomeURL,
		// The Glass, read side (GET /garden/glass/{agent} and its stream, Lands GL-1/3/4/6/14): one shared coopmux
		// subscription per watched session, opened for the human watching; an agent outside the viewer's lands is a 404. The
		// Wall (GET /garden/glass/wall, Lands GL-5) is one /ws/mux upstream for every viewer, filtered per viewer.
		// Nudge and respond (POST .../send, GL-7/8/11): a nudge goes the bridge's way (NudgeAgent) and into the agent's
		// Commons thread (Say, set below once the Commons is built); a respond is checked against coop's prompt first.
		// Attach (POST .../control and the keyboard's frames on .../send, GL-9/10): an operator takes the keyboard on the
		// operator's own lands, only an admin on a tenant land until Fences (GlassAttachTier, read from the farm).
		Glass: &gardenhub.Glass{
			Relay:   glass.NewRelay(glass.DialCoopmux(coopmuxClient), logger),
			Wall:    glass.NewWall(glass.DialCoopmuxMux(coopmuxClient), logger),
			Screens: coopmuxClient,
			// The credential pool (GET /garden/glass/credentials, GL-15): mux.html's pool snapshot, read for an admin.
			Pool:    coopmuxClient,
			InScope: func(r *http.Request, project string) bool { return viewerScopeFromContext(r.Context()).allows(project) },
			Who:     glassIdentity,
			Nudge:   glassNudger(daemon, logger),
			Prompts: coopmuxClient,
			Logger:  logger,
			MayAttach: func(r *http.Request, project string) bool {
				return gardenhub.GlassAttachTier(gardenFarm, project).Allows(glassIdentity(r).Roles)
			},
			Keyboard: coopmuxClient,
		},
		BasePath: cfg.basePath,
		Viewer: func(r *http.Request) string {
			return strings.ToLower(canonicalOIDC(oidcauth.EmailFromContext(r.Context())))
		},
		// The Mailbox offers Approve/Reject only to a viewer carrying approve:escalation, the same
		// gate the bead page applies (POST /b/{id}/escalation/*), which still enforces it.
		CanApproveEscalation: func(r *http.Request) bool {
			ok, _ := srv.viewerCanApproveEscalation(r.Context(), oidcauth.EmailFromContext(r.Context()))
			return ok
		},
		// The bead view offers "Adopt into a plan" only where POST /garden/bead/{id}/adopt (operatorOnly) would take it.
		CanAdopt: viewerCanOperateAuthed,
		// The gardener panel's levers, config and successor, and the project panel's "New gardener", are offered where the
		// operator-gated /gardener/{id}/* and /create-gardener POSTs would take them (the gardener page's own gate).
		CanOperateGardeners: viewerCanOperateAuthed,
		// "Send a gardener" from the bead view (web/tend.js, gd-I3Qx07esJj) is a Commons mention: offered to an operator,
		// and the Commons checks their land role when it is said.
		CanTend: viewerCanOperateAuthed,
		// The bead view's writes (edit, comment, close/reopen/claim/release, parent, waits on / holds up; Lands WB-2..WB-4)
		// post to the viewer's own /b/{id}/* routes, which keep their gates; offered where the operator-gated ones take them.
		CanEdit: viewerCanOperateAuthed,
		// "Put an agent on this" (POST /quick-spawn, Lands WB-12) only in a land where the viewer may spawn: its owner and
		// members, not guests (Q8c, gd-fPXoeHDQS3). handleQuickSpawn checks the same again.
		MaySpawn: func(r *http.Request, project string) bool {
			return project != "" && oidcauth.EmailFromContext(r.Context()) != "" && viewerScopeFromContext(r.Context()).may(project, conversation.ActionSpawn)
		},
		// Wake / Restart a sleeping or down gardener from its card and its Commons thread (POST /garden/gardener/{id}/wake,
		// gd-3xrmzPMA1Y): the `gb wake` revive, offered to and taken from a gardenio-admin session only.
		Agents:  daemon,
		CanWake: viewerCanAdmin,
		// The activity events go only for a gardener the viewer may see: every one for an unrestricted viewer, else
		// one of the viewer's projects, and on a scoped viewer's stream only one in its cut of the farm as well
		// (gardenhub farm_cut_stream.go, gd-UKO0ZpXeha).
		// A scoped viewer sees /garden read only (gd-GuZ3tbnTFI): every /garden write route stays routeAdminOnly for them, so
		// none of the hooks above is asked (a scoped human holding the operator role would pass CanAdopt, CanTend, CanEdit
		// and CanOperateGardeners), the answer boxes lose their buttons and the header says "Read only".
		ReadOnly: func(r *http.Request) bool { return viewerScopeFromContext(r.Context()).Scoped() },
		SeesGardener: func(r *http.Request) func(gardenhub.FarmGardener) bool {
			sc := viewerScopeFromContext(r.Context())
			if !sc.Scoped() {
				return nil
			}
			return func(g gardenhub.FarmGardener) bool { return g.Project != "" && sc.allows(g.Project) }
		},
		// A read-only verifier reads the farm (gd-TRdxFvBjW2) but no transcript: those are not daemon reads.
		MayTranscript: func(r *http.Request) bool { return !isVerifier(r.Context()) },
	}
	// The farm document (gd-6UG1OGlqYg) and its stream (gd-UKO0ZpXeha) are cut to a scoped viewer's projects; an
	// unrestricted viewer gets the whole farm. GET /garden and the farm routes are routeScoped (gd-l7yYxZpoZo).
	gardenPage.Cut = gardenFarmCut
	// The welcome tour (step 9.14b, garden_welcome.go): a land's person's first /garden walks them round once; the seen
	// flag is on their own user bead, read and stamped on their own bearer.
	gardenPage.Welcome = srv.gardenWelcome
	gardenPage.WelcomeSeen = srv.gardenWelcomeSeen
	srv.SetGardenHandler(gardenPage)

	// The Commons (Lands Commons I steps 8 and 9, gd-LCtVzVbuWM): fragments
	// read and messages written on the viewer's own bearer, and an id-only event stream that rides
	// the feed hub's one upstream connection and is cut per subscriber by land.
	commonsHub := commons.NewHub(beadFeedHub.Subscribe, daemon, logger)
	commonsHub.Start()
	defer commonsHub.Close()
	// Presence (Lands Commons III step 4.3, gd-SFaw19TOiP): who is in each land
	// and channel, from the open event streams and the kbeads presence tracker.
	// In-memory only; changes go out through the hub, cut by land.
	commonsPresence := commons.NewPresence(commonsHub, commonsAgents(daemon), logger)
	commonsPresence.Start()
	defer commonsPresence.Close()
	// A link to a channel or thread opens /garden with the Commons pane there
	// (Lands Commons I step 9, gd-b4IDsCKWbB).
	commonsHandler := newCommonsHandler(daemon, commonsHub, commonsPresence, cfg.basePath, gardenPage.ServePage)
	// Each agent's jobs (Commons v2 step 3, gd-qlk31NBV6o): the hook feed's tool calls, in memory, sent on the hub.
	jobs := commons.NewJobs(daemon, commonsHub, logger)
	jobs.Start()
	defer jobs.Close()
	commonsJobs.Store(jobs)
	commonsHandler.Jobs = jobs
	// The steward node (/garden/c/s, Commons v2 steward view S1, gd-FsDEc7ZXGl): the land's steward read on the viewer's
	// bearer, its head's day from the farm's cost row.
	commonsHandler.Stewards = commonsStewards{src: daemon, farm: gardenFarm, now: time.Now}
	// An agent's node head says why it is not working, from the farm, and offers Wake or Restart there (gd-7DyHdnx8fC).
	commonsHandler.Reason = gardenFarm.GardenerReason
	// A nudge from the Glass shows in the agent's Commons thread as the person's message (Lands GL-7, Commons D6).
	gardenPage.Glass.Say = func(r *http.Request, agentID, text string) (string, error) {
		return commonsHandler.SayInThread(r, agentID, text, gardenhub.GlassOrigin)
	}
	// A permission prompt answered on the Glass closes its Commons card and Slack prompt too, through the bridge
	// (Lands spec 3.3: answering in either place resolves both; a card's answer reaches the Glass as the agent's verdict).
	gardenPage.Glass.Answered = commonsHandler.PermissionAnswered
	// Abuse control (Lands Track B §5.4, step 10.4): messages per member per
	// land per minute.
	commonsHandler.Rate = commonsMessageRate(os.Getenv)
	// Kill switch (Lands Track B §5.5, step 10.5): a frozen land's members get
	// a banner for the composer; the operator gets Freeze and Thaw.
	landSwitch := newLandSwitches(daemon, logger)
	stopLandSwitch := landSwitch.start()
	defer stopLandSwitch()
	commonsHandler.Lands = landSwitch.landSwitch(daemon)
	// Member invites (Lands Track B §3.7, step 8.9): the owner's People
	// lightbox. It writes land_invite beads as the Gate's machine principal
	// and resolves logins through the Lands App, so it is on only with both.
	if srv.landsGate != nil && srv.landsGitHub != nil {
		if users, ok := srv.landsGitHub.app.(githubUsers); ok {
			invites := &landInvites{daemon: daemon, machine: srv.machinePrincipal, projects: daemon, github: users, logger: logger}
			commonsHandler.Invites = invites.commons()
			logger.Info("lands member invites enabled")
		}
	}
	// Card buttons and lightboxes (Commons II step 5, gd-amX5uMx4KM): what needs
	// the fleet goes to the bridge's inbound endpoint on the viewer's bearer;
	// an escalation's Approve and Reject go to the daemon as on the bead page.
	commonsHandler.Escalations = commonsEscalations{s: srv}
	if inbound := commons.NewBridgeInbound(os.Getenv(commonsBridgeURLEnv), nil); inbound != nil {
		commonsHandler.Inbound = inbound
	} else {
		logger.Info("commons card actions answer 501: " + commonsBridgeURLEnv + " is unset")
	}
	// A wish head's Done (Commons II step 6) closes it at the bridge too; with
	// no bridge a head offers no Done.
	if react := commons.NewBridgeReactor(os.Getenv(commonsBridgeURLEnv), nil); react != nil {
		commonsHandler.React = react
	}
	// A mention (Commons II step 7) starts, wakes or stops agents at the
	// bridge; with no bridge it is just words.
	if compose := commons.NewBridgeComposer(os.Getenv(commonsBridgeURLEnv), nil); compose != nil {
		commonsHandler.Compose = compose
	}
	// Attachments (gd-S8ufEc3k6s): the composer offers attaching files only
	// when there is somewhere to keep them.
	if files, err := newCommonsFilesFromEnv(context.Background()); err != nil {
		logger.Warn("commons attachments disabled — failed to build S3 client", "error", err)
	} else if files != nil {
		commonsHandler.Files = files
		logger.Info("commons attachments enabled", "bucket", files.bucket, "prefix", files.prefix)
	} else {
		logger.Info("commons attachments disabled (no COMMONS_FILES_BUCKET or COOP_S3_BUCKET)")
	}
	srv.SetCommonsHandler(commonsHandler)

	// Single reactive-collaboration seam (epic ka-0000002170; born as the interim
	// seam ka-0000007841). It observes beads.dependency.added and NUDGES/WAKES the
	// plan's already-bound agent on a new step edge, behind a per-plan debounce.
	// Since ka-0000002533 deleted the browser handler's inline promptPlanAssistant,
	// this reactor is the SOLE reactive path for BOTH the browser "Add step" and
	// the daemon-direct `gb plan add-step` — one debounce, one decision engine, no
	// double-spawn fork (decision ka-0000007006). It deliberately never cold-spawns
	// from a bare edge (cold-spawn parity for an agent-less plan is the epic's
	// tracked exit; it needs the first-class beads.plan.step.added intent signal).
	// Nil-safe (no-op without a daemon).
	planStepReactor := NewPlanStepReactor(daemon, srv, logger)
	planStepReactor.Start()
	defer planStepReactor.Close()

	// The Lands apply reactor (lands_apply_reactor.go): resolved approval
	// decisions onto their plot applications. Nil-safe (no-op without the Gate).
	landsApplyReactor := NewLandsApplyReactor(srv.landsGate, logger)
	landsApplyReactor.Start()
	defer landsApplyReactor.Close()

	// The gatehouse reactor (lands_gatehouse_reactor.go, gd-K62coPtKkX): the
	// gatehouse steward's demo passes and application drafts, answered as the
	// machine principal. It acts only while the gatehouse project carries
	// land_gatehouse_tools=true. Nil-safe.
	srv.landsGatehouseTools = gatehouseReactorFor(srv, daemon, logger)
	srv.landsGatehouseTools.Start()
	defer srv.landsGatehouseTools.Close()

	// Durable auto-bind seam (task ka-0000006412, epic ka-0000003073). It
	// observes the daemon label-mutation stream (beads.label.added +
	// beads.bead.updated) and cold-binds the durable plan-gardener when an epic
	// gains the `plan` label by ANY writer — closing the raw `kd label add
	// <epic> plan` / formula-relabel bypass that the two inline viewer seams
	// (POST /plans, hand-mark) never covered, so a raw-labeled plan is no longer
	// born gardenerless and later HARD-409ing at handlePlanBuild. Goes through the
	// SAME shared plangardener.Bind (idempotent-adopt, born-paused per
	// EnabledOnBind) the viewer/CLI seams use. Nil-safe (no-op without a daemon).
	planLabelReactor := NewPlanLabelReactor(daemon, srv, logger)
	planLabelReactor.Start()
	defer planLabelReactor.Close()

	// The plan answer reactor (plan_answer_reactor.go, gd-Dl88Htipuq): applies
	// the owner's answer to a plan decision server-side, so nobody runs gb
	// lands plan answer and the owner clicks once. Nil-safe.
	planAnswerReactor := NewPlanAnswerReactor(daemon, srv, logger)
	planAnswerReactor.Start()
	defer planAnswerReactor.Close()

	mux := http.NewServeMux()

	// Signal-bound context drives both graceful shutdown and OIDC provider
	// discovery: passing it to oidcauth.New makes the background discovery
	// retry interruptible by SIGTERM (ka-0000001839) instead of blocking
	// shutdown on an uninterruptible retry budget.
	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT)
	defer cancel()

	var authMiddleware func(http.Handler) http.Handler
	readyFn := func() bool { return true }

	if cfg.devAuthBypass {
		logger.Warn("!!! BEADS_VIEWER_DEV_AUTH_BYPASS is active — OIDC is DISABLED. Do NOT use this in production.")
		if cfg.devAuthProxySecret == "" {
			logger.Warn("!!! BEADS_VIEWER_DEV_AUTH_PROXY_SECRET is unset — any pod that can reach this Service can forge X-Auth-Request-Email and act as any operator (gd-kRyMsWiDNZ)")
		}
		authMiddleware = devBypassMiddleware(logger, cfg.devAuthProxySecret, newDevAuthEdgeBearer(ctx, cfg, logger))
	} else {
		// Build OIDC handle early so /readyz can be gated on it (see ka-0000004618 F1).
		// Discovery runs in the background; /readyz fails closed (503) until
		// it succeeds, then flips ready without a pod restart (ka-0000001839).
		oidcHandle := oidcauth.New(ctx, oidcauth.Config{
			IssuerURL:           cfg.oidcIssuerURL,
			ClientID:            cfg.oidcClientID,
			RequiredRole:        cfg.oidcRequiredRole,
			AllowedEmailDomains: cfg.oidcAllowedEmailDomains,
			AVA: oidcauth.AVAConfig{
				TrustSigner: cfg.avaTrustSigner,
				Region:      cfg.avaRegion,
				DefaultRole: cfg.avaDefaultRole,
			},
			K8sSA: cfg.k8sSAConfig(),
		}, logger)
		authMiddleware = oidcHandle.Middleware
		readyFn = oidcHandle.Ready
	}

	// Health probes bypass auth.
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprintf(w, `{"status":"ok","version":"%s"}`, version)
	})
	mux.HandleFunc("/readyz", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if !readyFn() {
			w.WriteHeader(http.StatusServiceUnavailable)
			fmt.Fprintf(w, `{"status":"not_ready","reason":"oidc_provider_unavailable"}`)
			return
		}
		fmt.Fprintf(w, `{"status":"ok"}`)
	})

	// Login picker is anonymous — Traefik routes /login here without ForwardAuth,
	// so we register on the public mux outside the OIDC middleware chain.
	mux.HandleFunc("GET /login", srv.handleLogin(cfg.loginPicker))

	// Application routes (protected by middleware).
	appMux := http.NewServeMux()
	srv.RegisterRoutes(appMux)

	// View-engagement + request-latency instrumentation (Customer-1
	// telemetry Phase 1, ka-0000006752 / epic ka-0000007629). Promotes the
	// homepage_telemetry.go slog events to a whole-app Prometheus counter on
	// operator_id, plus a per-endpoint latency histogram. Registered against
	// a dedicated registry served on the internal-only metrics port (parallel
	// to cmd/controller). Failing to build it is fatal — a viewer that can't
	// register its metrics is a deploy-time misconfiguration, not a runtime
	// degradation we want to paper over.
	promRegistry := prometheus.NewRegistry()
	promRegistry.MustRegister(collectors.NewProcessCollector(collectors.ProcessCollectorOpts{}))
	promRegistry.MustRegister(collectors.NewGoCollector())
	viewTel, err := newViewTelemetry(promRegistry, srv.resolver, cfg.project)
	if err != nil {
		logger.Error("failed to register view telemetry metrics", "error", err)
		os.Exit(1)
	}
	// The Commons event latency SLI (Lands Track B §5.8): the hub's
	// write-to-broadcast histogram. Nil when the Commons has no event source.
	if c := commonsHub.LagCollector(); c != nil {
		promRegistry.MustRegister(c)
	}
	// The steward response SLI (gd-h5rDFoiOPh): human #general message to the
	// land steward's first answer.
	if c := commonsHub.StewardResponseCollector(); c != nil {
		promRegistry.MustRegister(c)
	}
	// The /garden routes' server time, whole and by phase (gd-7jlAbyk8Fy): the backend half of each Lands budget, the
	// split their Server-Timing header carries.
	promRegistry.MustRegister(servertiming.Collectors()...)
	// The viewer's bead lists by call site (gd-XOi5cqZP3I).
	promRegistry.MustRegister(beadListSeconds)
	// /image/ reads by the bucket that answered (gd-CmjVAU3vgd).
	promRegistry.MustRegister(imageProxyReads)
	// Bird captions by source, and the Haiku fallback's calls and skips (spec §8).
	promRegistry.MustRegister(farmCaptioner.Metrics())
	// The /garden farm's activity feed (gd-fBGGNdgrLc): what its SSE costs live, as
	// bytes written to the viewers' queues, and how many viewers declared a view.
	farmAct := func(get func(gardenhub.FarmStats) float64) func() float64 {
		return func() float64 { return get(gardenFarm.ActivityStats()) }
	}
	promRegistry.MustRegister(
		prometheus.NewCounterFunc(prometheus.CounterOpts{Name: "gardenio_farm_activity_bytes_total", Help: "Bytes of /garden/farm/events activity, busy and doing events queued to viewers."},
			farmAct(func(s gardenhub.FarmStats) float64 { return float64(s.ActivityBytes) })),
		prometheus.NewCounterFunc(prometheus.CounterOpts{Name: "gardenio_farm_activity_events_total", Help: "Activity feed events queued to viewers."},
			farmAct(func(s gardenhub.FarmStats) float64 { return float64(s.ActivitySent) })),
		prometheus.NewCounterFunc(prometheus.CounterOpts{Name: "gardenio_farm_activity_dropped_total", Help: "Activity feed events a full viewer queue refused."},
			farmAct(func(s gardenhub.FarmStats) float64 { return float64(s.ActivityDropped) })),
		prometheus.NewCounterFunc(prometheus.CounterOpts{Name: "gardenio_farm_activity_views_total", Help: "Views pages declared for their activity stream."},
			farmAct(func(s gardenhub.FarmStats) float64 { return float64(s.ActivityViews) })),
		prometheus.NewGaugeFunc(prometheus.GaugeOpts{Name: "gardenio_farm_activity_clients", Help: "Open activity feed streams."},
			farmAct(func(s gardenhub.FarmStats) float64 { return float64(s.ActivityClients) })),
		// The transcripts (bird captions step 7, gd-MsTDi7JYVs): rows written to the gardeners' rings, and the call events
		// sent to the streams holding one open, which no other stream pays for.
		prometheus.NewCounterFunc(prometheus.CounterOpts{Name: "gardenio_farm_transcript_rows_total", Help: "Transcript rows (finished calls) written to the gardeners' rings."},
			farmAct(func(s gardenhub.FarmStats) float64 { return float64(s.TranscriptRows) })),
		prometheus.NewCounterFunc(prometheus.CounterOpts{Name: "gardenio_farm_transcript_bytes_total", Help: "Bytes of /garden/farm/events call events queued to streams holding a transcript open."},
			farmAct(func(s gardenhub.FarmStats) float64 { return float64(s.TranscriptBytes) })),
		prometheus.NewGaugeFunc(prometheus.GaugeOpts{Name: "gardenio_farm_transcript_clients", Help: "Activity streams holding a transcript open."},
			farmAct(func(s gardenhub.FarmStats) float64 { return float64(s.TranscriptClients) })),
	)
	// The stream budget (gd-iAq4va9XDt): a stream with many busy gardeners in view (a desktop page framing the whole
	// farm) folds each one's calls longer than 250 ms. fold is one of five gaps, so a phone's stream and a whole-farm
	// one read apart live.
	for k, fold := range gardenhub.ActivityFolds {
		labels := prometheus.Labels{"fold": fold}
		promRegistry.MustRegister(
			prometheus.NewCounterFunc(prometheus.CounterOpts{Name: "gardenio_farm_activity_call_bytes_total", Help: "Bytes of /garden/farm/events activity (call) events queued to viewers, by the fold their stream was on.", ConstLabels: labels},
				farmAct(func(s gardenhub.FarmStats) float64 { return float64(s.ActivityFoldBytes[k]) })),
			prometheus.NewGaugeFunc(prometheus.GaugeOpts{Name: "gardenio_farm_activity_fold_clients", Help: "Open activity feed streams sent calls at each fold.", ConstLabels: labels},
				farmAct(func(s gardenhub.FarmStats) float64 { return float64(s.ActivityFoldClients[k]) })),
		)
	}

	// Epics with and without arms per project (gd-YvWv4mR4fu): the gap the plan
	// gardener's arms duty closes. Counted in the background, never on scrape.
	// The Window's views, daemon reads and hygiene withholds (window.go).
	promRegistry.MustRegister(windowMetrics()...)

	armsG := newArmsGauge(daemon)
	promRegistry.MustRegister(armsG)
	go armsG.run(ctx, armsGaugeInterval, logger)

	// Build middleware chain (outermost first):
	//   ipWhitelist -> OIDC (or dev bypass) -> operator -> machine -> verifier -> scope -> viewTelemetry -> appMux
	//
	// scope (scope.go) sits after operator because it needs the verified roles
	// and groups authMiddleware put in the context, and it classifies the request
	// by asking appMux which pattern it will dispatch to (route_classes.go).
	// machine (machine_route.go) sits between them: on a routeMachine route it
	// asks the daemon who the bearer operator put on the context is, and scope
	// refuses that route to a request it did not admit. verifier
	// (verifier.go) admits a read-only GET on a fleet agent's own token past
	// scope's no-edge-proof refusal.
	//
	// viewTelemetry sits INNERMOST, directly wrapping appMux, for two
	// reasons: (1) ServeMux populates r.Pattern in place on the request it
	// receives, and operatorMiddleware replaces the request via WithContext —
	// so only the wrapper adjacent to the mux observes the matched route
	// template; (2) the authenticated viewer email is stamped into the
	// context by authMiddleware further out and flows down intact through
	// operatorMiddleware's WithContext, so the operator resolve still sees it.
	var handler http.Handler = appMux
	handler = viewTel.Middleware(handler)
	patternOf := func(r *http.Request) string {
		_, pattern := appMux.Handler(r)
		return pattern
	}
	handler = scopeMiddleware(handler, patternOf)
	verifierRoles := func() string { return srv.tunables.GetString(verifierRolesKey, verifierRolesEnv, "") }
	handler = verifierMiddleware(handler, patternOf, verifierRoles, daemon.MeAs, daemonAgentRole(daemon), logger)
	handler = machineMiddleware(handler, patternOf, daemon.MeAs, logger)
	// The page's one connection (GET /garden/live/events, gardenhub/livemux.go) serves each stream it carries through
	// scope and machine again, on the connection's own request: a stream there is read, cut and refused as it would be
	// on its own. It skips viewTelemetry, which counted the connection already.
	gardenPage.Live = scopeMiddleware(machineMiddleware(appMux, patternOf, daemon.MeAs, logger), patternOf)
	handler = operatorMiddleware(handler)
	handler = authMiddleware(handler)
	handler = ipWhitelistMiddleware(handler, cfg.allowedCIDRs, cfg.trustedProxyHops, logger)

	mux.Handle("/", handler)
	// The Lands front of house is open to a visitor with no session (Track B
	// §2.3): the landing page, the legal documents and their stylesheet carry
	// nothing about the caller (routePublic), so they are also served without
	// the auth chain; so is the splash, /garden/splash, with its two files
	// (gd-hzRHm1KB5r). Who reaches them is the edge's call (step 7.8).
	public := ipWhitelistMiddleware(viewTel.Middleware(appMux), cfg.allowedCIDRs, cfg.trustedProxyHops, logger)
	mountNoSession(mux, public)
	// The Window (window.go, PW.3): /w/<token> is open to anyone holding the
	// link. The token is the credential, judged by kbeads alone; the edge and
	// viewer-authproxy pass ^~ /w/ with no session (PW.5).
	mountWindow(mux, public)

	// Metrics server on a separate internal-only port (parallel to
	// cmd/controller/main.go). Never exposed through the public IngressRoute;
	// scraped in-cluster by the VMServiceScrape on the metrics service port.
	metricsAddr := envOrDefault("METRICS_LISTEN_ADDR", ":9095")
	metricsMux := http.NewServeMux()
	metricsMux.Handle("GET /metrics", promhttp.HandlerFor(promRegistry, promhttp.HandlerOpts{}))
	metricsSrv := &http.Server{
		Addr:              metricsAddr,
		Handler:           metricsMux,
		ReadHeaderTimeout: 5 * time.Second,
	}
	go func() {
		logger.Info("starting metrics server", "addr", metricsAddr)
		if err := metricsSrv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			logger.Error("metrics server failed", "error", err)
		}
	}()

	// gzip wraps the whole listener, so the auth chain and the no-session
	// routes both compress (gzip.go; SSE and pre-encoded routes pass through).
	httpSrv := &http.Server{
		Addr:              cfg.listenAddr,
		Handler:           gzipmw.Middleware(mux),
		ReadHeaderTimeout: 10 * time.Second,
	}

	go func() {
		logger.Info("starting HTTP server", "addr", cfg.listenAddr)
		if err := httpSrv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			logger.Error("HTTP server failed", "error", err)
		}
	}()

	<-ctx.Done()
	logger.Info("shutting down beads-viewer")

	shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer shutdownCancel()
	if err := httpSrv.Shutdown(shutdownCtx); err != nil {
		logger.Error("HTTP server shutdown error", "error", err)
	}
	if err := metricsSrv.Shutdown(shutdownCtx); err != nil {
		logger.Error("metrics server shutdown error", "error", err)
	}
}

type config struct {
	beadsHTTPAddr        string
	zitiIdentityPath     string
	zitiService          string
	zitiTLSServerName    string
	listenAddr           string
	logLevel             string
	allowedCIDRs         string
	trustedProxyHops     int
	basePath             string
	coopmuxURL           string
	coopmuxPublicURL     string
	slackWorkspaceDomain string
	escalationReviewURL  string
	grafanaBaseURL       string
	// becomeURL is the Become page an admin's Become button opens (plan gd-0SUIdKXWV3; gardenio-ziti public-edge.yaml).
	becomeURL string
	// stopBecomeURL is where the impersonation banner's Stop goes (plan gd-0SUIdKXWV3 step 4).
	stopBecomeURL string

	oidcIssuerURL           string
	oidcClientID            string
	oidcRequiredRole        string
	oidcAllowedEmailDomains []string

	// AVA edge-auth trust pinning. When avaTrustSigner is non-empty,
	// requests carrying X-Amzn-Ava-User-Context are accepted in lieu of a
	// Keycloak bearer token (see oidcauth.AVAConfig). Set this from the
	// helm chart when beadsViewer.avaIngress.enabled is true. Without it,
	// AVA-routed requests would hit the Keycloak path and be 401'd.
	avaTrustSigner string
	avaRegion      string
	// avaDefaultRole, when non-empty, is appended to the role list extracted
	// from a verified AVA token before RBAC checks run. Mirrors Keycloak's
	// `isDefault: true` realm-role behavior for AVA-authenticated users
	// (decision ka-0000002146, epic ka-0000002784). Sourced from
	// AVA_DEFAULT_ROLE; empty disables the mechanism.
	avaDefaultRole string

	// EKS ServiceAccount-token machine-identity branch (ka-0000006288). When
	// k8sSAIssuerURL is set, beads-viewer accepts an audience-pinned projected
	// SA token from the EKS OIDC issuer as a SECOND verified issuer, mapping an
	// allow-listed sub (system:serviceaccount:<ns>:<sa>) to a synthetic
	// domain-valid email + a role floor so new-prod audit pods can drive the
	// authenticated-self-view dimensions. Real JWKS + iss + aud + exp
	// verification — not a trusted header. All four must be set together (see
	// oidcauth.Validate). Sourced from K8S_SA_*; empty issuer disables the
	// branch and renders the middleware byte-identical.
	k8sSAIssuerURL       string
	k8sSAAudience        string
	k8sSAAllowedSubjects []string
	k8sSAEmailDomain     string
	k8sSARoleFloor       string

	devAuthBypass bool
	// devAuthProxySecret, when set, gates trust of X-Auth-Request-Email on the
	// edge presenting it (BEADS_VIEWER_DEV_AUTH_PROXY_SECRET, gd-kRyMsWiDNZ).
	devAuthProxySecret string
	// devAuthEdgeBearer names the Keycloak realm whose access tokens the edge
	// forwards, so the dev-bypass path can verify one and attach its realm
	// roles (BEADS_VIEWER_DEV_AUTH_BEARER_*, gd-BmYmugDc4x). Off unless both
	// the issuer and the audience are set.
	devAuthEdgeBearer oidcauth.EdgeBearerConfig

	loginPicker loginPickerConfig

	// gitlabBaseURL / gitlabAPIToken / gardenioProjectPath back the /index
	// "Merge requests" rail panel (ka-0000005424). All three must be set for
	// the panel to fetch live data; gardenioProjectURL is optional and only
	// surfaces a header deep-link when present.
	gitlabBaseURL       string
	gitlabAPIToken      string
	gardenioProjectPath string
	gardenioProjectURL  string

	// project is the deployment's project label, stamped on the
	// gardenio_operator_activity_total view counter (Customer-1 telemetry,
	// ka-0000006752). Sourced from BOAT_PROJECT — the same env the gb/crew
	// commands read as the canonical project identity. Empty collapses to
	// "unknown" in newViewTelemetry so the label is never blank.
	project string

	// natsURL / natsToken back the telepathy thinking-trace feed
	// (epic ka-0000007154, task ka-0000007528). When natsURL is set, the
	// viewer subscribes to coop.events.messages.*.thinking and fans the
	// thinking blocks out to the /telepathy SSE page. Empty natsURL disables
	// the feed entirely (the hub is nil). Sourced from NATS_URL /
	// COOP_NATS_TOKEN — the same env pair the slack-bridge hookstream reads.
	natsURL   string
	natsToken string
	// natsUser: NATS username (COOP_NATS_USER). When set the token is sent
	// as this user's PASSWORD — multi-user server auth
	// (nats.auth.subjectIsolation) only supports user/password entries
	// (ka-0000000002).
	natsUser string

	// requireUserBearerForWrites fail-closes mutating daemon calls that lack the
	// human user's OIDC bearer and routes present bearers as the wire credential
	// (ka-0000006379). Sourced from BEADS_VIEWER_REQUIRE_USER_BEARER_WRITES=1.
	// Default off — enable only alongside a RoleViewer (read-only) daemon token
	// and after confirming the user-bearer path reaches the daemon (Keycloak
	// ForwardAuth forwards X-Auth-Request-Access-Token; AVA-gated hosts do not).
	requireUserBearerForWrites bool

	// requireUserBearerForReads is the read-side twin (gd-akDjTS2eh7). Sourced
	// from BEADS_VIEWER_REQUIRE_USER_BEARER_READS=1; default off.
	requireUserBearerForReads bool
}

func parseConfig() (*config, error) {
	return parseConfigWith(fileExists)
}

func parseConfigWith(fileExistsFn func(string) bool) (*config, error) {
	bp := strings.TrimRight(os.Getenv("BASE_PATH"), "/")
	cfg := &config{
		beadsHTTPAddr:        envOrDefault("BEADS_HTTP_ADDR", "http://localhost:8080"),
		zitiIdentityPath:     strings.TrimSpace(os.Getenv("GARDENIO_ZITI_IDENTITY")),
		zitiService:          strings.TrimSpace(os.Getenv("GARDENIO_ZITI_SERVICE")),
		zitiTLSServerName:    strings.TrimSpace(os.Getenv("GARDENIO_ZITI_TLS_SERVER_NAME")),
		listenAddr:           envOrDefault("LISTEN_ADDR", ":8095"),
		logLevel:             envOrDefault("LOG_LEVEL", "info"),
		allowedCIDRs:         os.Getenv("ALLOWED_CIDRS"),
		trustedProxyHops:     envIntOrDefault("TRUSTED_PROXY_HOPS", 0),
		basePath:             bp,
		coopmuxURL:           os.Getenv("COOPMUX_URL"),
		coopmuxPublicURL:     strings.TrimRight(os.Getenv("COOPMUX_PUBLIC_URL"), "/"),
		slackWorkspaceDomain: strings.TrimSpace(os.Getenv("SLACK_WORKSPACE_DOMAIN")),
		escalationReviewURL:  strings.TrimRight(os.Getenv("ESCALATION_REVIEW_URL"), "/"),
		grafanaBaseURL:       strings.TrimRight(os.Getenv("GRAFANA_BASE_URL"), "/"),
		becomeURL:            envOrDefault("BEADS_VIEWER_BECOME_URL", defaultBecomeURL),
		stopBecomeURL:        envOrDefault("BEADS_VIEWER_STOP_BECOME_URL", defaultStopBecomeURL),

		oidcIssuerURL:           os.Getenv("OIDC_ISSUER_URL"),
		oidcClientID:            os.Getenv("OIDC_CLIENT_ID"),
		oidcRequiredRole:        os.Getenv("OIDC_REQUIRED_ROLE"),
		oidcAllowedEmailDomains: parseCommaSeparated(os.Getenv("OIDC_ALLOWED_EMAIL_DOMAINS")),
		avaTrustSigner:          strings.TrimSpace(os.Getenv("AVA_TRUST_SIGNER")),
		avaRegion:               strings.TrimSpace(os.Getenv("AVA_REGION")),
		avaDefaultRole:          strings.TrimSpace(os.Getenv("AVA_DEFAULT_ROLE")),
		k8sSAIssuerURL:          strings.TrimSpace(os.Getenv("K8S_SA_ISSUER_URL")),
		k8sSAAudience:           strings.TrimSpace(os.Getenv("K8S_SA_AUDIENCE")),
		k8sSAAllowedSubjects:    parseCommaSeparated(os.Getenv("K8S_SA_ALLOWED_SUBJECTS")),
		k8sSAEmailDomain:        strings.TrimSpace(os.Getenv("K8S_SA_SYNTHETIC_EMAIL_DOMAIN")),
		k8sSARoleFloor:          strings.TrimSpace(os.Getenv("K8S_SA_ROLE_FLOOR")),
		devAuthBypass:           devAuthBypassEnabled(),
		devAuthProxySecret:      strings.TrimSpace(os.Getenv(devAuthProxySecretEnv)),
		devAuthEdgeBearer: oidcauth.EdgeBearerConfig{
			IssuerURL:    strings.TrimSpace(os.Getenv(devAuthBearerIssuerURLEnv)),
			DiscoveryURL: strings.TrimSpace(os.Getenv(devAuthBearerDiscoveryURLEnv)),
			CAFile:       strings.TrimSpace(os.Getenv(devAuthBearerCAFileEnv)),
			Audience:     strings.TrimSpace(os.Getenv(devAuthBearerAudienceEnv)),
		},
		gitlabBaseURL:              strings.TrimRight(envOrDefault("GITLAB_BASE_URL", "https://gitlab.com"), "/"),
		gitlabAPIToken:             os.Getenv("GITLAB_API_TOKEN"),
		gardenioProjectPath:        strings.TrimSpace(os.Getenv("GARDENIO_GITLAB_PROJECT")),
		gardenioProjectURL:         strings.TrimRight(os.Getenv("GARDENIO_GITLAB_PROJECT_URL"), "/"),
		project:                    strings.TrimSpace(os.Getenv("BOAT_PROJECT")),
		natsURL:                    strings.TrimSpace(os.Getenv("NATS_URL")),
		natsToken:                  os.Getenv("COOP_NATS_TOKEN"),
		natsUser:                   os.Getenv("COOP_NATS_USER"),
		requireUserBearerForWrites: os.Getenv("BEADS_VIEWER_REQUIRE_USER_BEARER_WRITES") == "1",
		requireUserBearerForReads:  os.Getenv("BEADS_VIEWER_REQUIRE_USER_BEARER_READS") == "1",
		loginPicker:                loginPickerConfigFromEnv(),
	}

	if cfg.devAuthBypass {
		// Refuse to start in anything that looks like a real cluster unless
		// the agent-pod ACK env opts in (audit-formula use case).
		if err := checkDevAuthBypassAllowed(fileExistsFn); err != nil {
			return nil, err
		}
		// When the ACK is honored, the SA-token guard is downgraded but we
		// force LISTEN_ADDR to a loopback interface so the bypass surface
		// can never accidentally listen on the pod network.
		if devAuthBypassAckAgentPod() {
			forced, err := forceLoopbackListenAddr(cfg.listenAddr)
			if err != nil {
				return nil, err
			}
			cfg.listenAddr = forced
		}
		// Skip OIDC requirement when bypass is active.
		return cfg, nil
	}

	// beads-viewer requires either Keycloak OIDC or AVA edge-auth (or both)
	// — no basic-auth fallback. When AVA_TRUST_SIGNER is set, AVA performs
	// the auth upstream of the pod and the OIDC env pair becomes optional;
	// this mirrors oidcauth.Validate (internal/oidcauth/validate.go), which
	// accepts an AVA-only config. The aws-poc tier-(g) overlay relies on
	// this path (ka-0000003205): without it, beadsViewer.enabled=true with
	// only avaIngress.* set CrashLooped the pod and stalled helm --wait
	// until terraform rolled back.
	if cfg.avaTrustSigner == "" {
		if cfg.oidcIssuerURL == "" || cfg.oidcClientID == "" {
			return nil, errors.New("OIDC_ISSUER_URL and OIDC_CLIENT_ID are required when AVA_TRUST_SIGNER is unset (no basic-auth fallback); set BEADS_VIEWER_DEV_AUTH_BYPASS=1 for local dev")
		}
	} else if (cfg.oidcIssuerURL == "") != (cfg.oidcClientID == "") {
		// Half-configured Keycloak alongside AVA is almost certainly a typo;
		// fail fast rather than silently dropping the broken half.
		return nil, errors.New("OIDC_ISSUER_URL and OIDC_CLIENT_ID must be set together (or both empty) when AVA_TRUST_SIGNER is set")
	}
	return cfg, nil
}

// k8sSAConfig builds the EKS ServiceAccount-token branch config from the parsed
// env (ka-0000006288). An empty issuer leaves K8sSAConfig disabled so the
// middleware is byte-identical for an unconfigured deploy; the full consistency
// rules (audience non-empty and not the apiserver/IRSA audience, allow-list
// non-empty + well-formed, synthetic domain inside AllowedEmailDomains, role
// floor set) are enforced by oidcauth.Validate at startup.
func (c *config) k8sSAConfig() oidcauth.K8sSAConfig {
	return oidcauth.K8sSAConfig{
		IssuerURL:            c.k8sSAIssuerURL,
		Audience:             c.k8sSAAudience,
		AllowedSubjects:      c.k8sSAAllowedSubjects,
		SyntheticEmailDomain: c.k8sSAEmailDomain,
		RoleFloor:            c.k8sSARoleFloor,
	}
}

// defaultBecomeURL is the one-click Become page on the Keycloak host (gardenio-ziti public-edge.yaml, PR #1675); it
// takes ?email=<tenant>.
const defaultBecomeURL = "https://auth.clonk.tv/gardenio/become/"

// defaultStopBecomeURL ends a Become session (plan gd-0SUIdKXWV3 step 4): the edge's oauth2-proxy sign-out clears the
// gardenio cookie, then Keycloak's end-session for the gardenio realm ends the tenant's session and returns to /garden,
// where the edge asks the admin to sign in as themselves. client_id names the forwardauth client, whose post-logout
// redirect URIs must allow /garden (deploy-values/gardenio-k3s.yaml); sign_out may redirect to the issuer's host, which
// the chart whitelists.
var defaultStopBecomeURL = "https://gardenio.clonk.tv/oauth2/sign_out?rd=" + url.QueryEscape(
	"https://auth.clonk.tv/auth/realms/gardenio/protocol/openid-connect/logout?client_id=gardenio-traefik-forwardauth&post_logout_redirect_uri="+
		url.QueryEscape("https://gardenio.clonk.tv/garden"))

func envOrDefault(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

// envIntOrDefault reads a non-negative integer env var, returning fallback when
// unset, empty, unparseable, or negative. Used for TRUSTED_PROXY_HOPS, where a
// bad value must fail safe to the default (0 = trust no proxy) rather than
// silently widening the trust boundary.
func envIntOrDefault(key string, fallback int) int {
	v := os.Getenv(key)
	if v == "" {
		return fallback
	}
	n, err := strconv.Atoi(v)
	if err != nil || n < 0 {
		return fallback
	}
	return n
}

func parseCommaSeparated(s string) []string {
	if s == "" {
		return nil
	}
	parts := strings.Split(s, ",")
	result := make([]string, 0, len(parts))
	for _, p := range parts {
		if v := strings.TrimSpace(p); v != "" {
			result = append(result, v)
		}
	}
	return result
}

func setupLogger(level string) *slog.Logger {
	var logLevel slog.Level
	switch level {
	case "debug":
		logLevel = slog.LevelDebug
	case "warn":
		logLevel = slog.LevelWarn
	case "error":
		logLevel = slog.LevelError
	default:
		logLevel = slog.LevelInfo
	}
	return slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: logLevel}))
}

func init() {
	if v := os.Getenv("VERSION"); v != "" {
		version = v
	}
}
