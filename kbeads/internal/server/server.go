package server

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"

	beadsv1 "gardenio/kbeads/gen/beads/v1"
	"gardenio/kbeads/internal/edgeauth"
	"gardenio/kbeads/internal/embedding"
	"gardenio/kbeads/internal/eventbus"
	"gardenio/kbeads/internal/events"
	"gardenio/kbeads/internal/hooks"
	"gardenio/kbeads/internal/metrics"
	"gardenio/kbeads/internal/model"
	"gardenio/kbeads/internal/presence"
	"gardenio/kbeads/internal/store"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// BeadsServer implements the beadsv1.BeadsServiceServer interface.
type BeadsServer struct {
	beadsv1.UnimplementedBeadsServiceServer
	store           store.Store
	publisher       events.Publisher
	bus             *eventbus.Bus
	sseHub          *sseHub
	hooksHandler    *hooks.Handler
	Presence        *presence.Tracker
	userProvisioner *UserProvisioner
	fleetCfg        *FleetConfig
	trustedProxies  *TrustedProxyConfig
	// runnerRegisterLimiter caps POST /v1/runners/register per client IP and
	// runnerRegisterMu serializes invite redemption (runners.go).
	runnerRegisterLimiter registerLimiter
	runnerRegisterMu      sync.Mutex
	keycloakAdmin         *KeycloakAdminClient // optional, for session revocation
	// spawnLineageSealTTL / lineageClock: see spawn_lineage_seal.go. Zero/nil
	// means the default TTL and the wall clock.
	spawnLineageSealTTL time.Duration
	lineageClock        func() time.Time
	// impersonationAudited is the impersonated sessions this replica has
	// already written the audit bead for (impersonation.go).
	impersonationAudited impersonationSessionSet
	// coordinatorClients is BEADS_ESCALATION_COORDINATOR_CLIENTS: machine
	// clients that may act in a coordinator lane (escalation_lane_members.go).
	coordinatorClients map[string]struct{}
	// resultMachines is BEADS_ESCALATION_RESULT_MACHINES (escalation_result_gate.go).
	resultMachines map[string]struct{}
	// bridgeSubjects are the fleet subjects whose X-Operated-By relay may
	// stand in for a human in an access-control decision (relay_trust.go).
	bridgeSubjects map[string]bool
	// forwardAuthClients is the allowlist of OIDC azp values (the oauth2-proxy
	// edge client) whose tokens may take their identity and groups
	// from X-Auth-Request-Email / X-Auth-Request-Groups; see
	// forwardAuthClientTrusted. They are also the page session, the only OIDC
	// tokens that approve an escalation (escalationApprover). Empty trusts none.
	forwardAuthClients map[string]struct{}
	revocation         *TokenRevocationStore // DB-backed revoked-token blocklist
	refreshTokens      *RefreshTokenManager  // C1.2: refresh-token issuance/exchange/revoke
	refreshRotate      bool                  // when true, refresh exchange rotates (RFC 6749 §10.4)
	embedStats         metrics.StatsProvider
	embedder           embedding.Embedder
	rateLimitCfg       *RateLimitConfig
	edgeAuthCfg        *edgeauth.Config // Stage 3 / W-B-11: HMAC envelope from gardenio-edge-auth
	healthSnapshot     *healthSnapshotCache

	// taintContextCache memoises the resolved taint set of a calling agent
	// bead for identityCacheTTL (ADR docs/adr/taints.md §4.1/§4.2), so
	// repeated propagation-hook/read-gate checks within one cache window do
	// not re-query bead_taints. Initialised lazily on first use.
	taintContextCache *taintContextCache

	// teamMembershipCache memoises the resolved team membership of a calling
	// operator for identityCacheTTL (ADR docs/adr/taints.md §8.1/§8.3: "the
	// set is small and is cached per request"), so repeated grant/approval
	// checks within one cache window do not re-list type:team beads or
	// re-resolve operator_aliases. Initialised lazily on first use.
	teamMembershipCache *teamMembershipCache

	// taintRegistryCache memoises the ADR docs/adr/taints.md §10.1
	// zero-configuration fact "the taints registry holds at least one row"
	// process-wide, so a normal bead read pays no bead_taints (or taints)
	// query at all until the first taint is ever created. Constructed eagerly
	// by NewBeadsServer (never nil): the health snapshot fans tiles out
	// concurrently, and a lazy nil-check-then-assign here would be a data
	// race on this security gate's state. Invalidated by
	// InvalidateTaintRegistryCache when a taint is created.
	taintRegistryCache *taintRegistryCache

	// typeConfigs memoises the store's type:* config rows for
	// resolveTypeConfig (type_config_cache.go). A value, so it is never nil.
	typeConfigs typeConfigCache

	// grantSetCache memoises each principal's resolved taint grant set
	// (grantsOf) and spawnChainRootCache each agent's spawn-chain root human
	// (resolveSpawnChainRoot), so a gated read does not re-run the
	// taint_grants, operator_aliases and spawned_by lookups (taint_grant_cache.go,
	// gd-eawnzZZEjV). Constructed eagerly, like taintRegistryCache; nil caches
	// nothing.
	grantSetCache       *principalCache[[]int64]
	spawnChainRootCache *principalCache[string]

	// tenantLands memoises which projects are tenant lands for the tenant
	// read gate (read_authz_tenant.go). A value, so it is never nil.
	tenantLands tenantLandCache

	// landViews holds each land window's built view (window_land_cache.go).
	// A value, so it is never nil.
	landViews windowLandViewCache

	// prStateLookup answers the open-PR close guard (close_open_pr_guard.go).
	// nil reads the github-bridge's merge-request bead (projectedPRState).
	prStateLookup PRStateLookup

	// vmClient + uniqCache back the in-house chart backend's first endpoint,
	// GET /v1/charts/uniqueness (DAU/WAU/MAU query-time panels, task
	// ka-0000006905). Both are nil until SetVMClient wires a VictoriaMetrics
	// endpoint; when nil the endpoint returns 503 and kbeads serves no charts.
	vmClient  *VMClient
	uniqCache *uniquenessCache

	// serviceFieldFleets / serviceFieldRequireProject gate the
	// service-managed-fields ownership bypass on shared trigger/schedule beads
	// so a foreign environment's bridge cannot stamp last_checked on beads it
	// does not serve (cross-env cooldown starvation, ka-0000006237). Set via
	// SetServiceFieldPolicy from BEADS_SERVICE_FIELD_FLEETS and
	// BEADS_SERVICE_FIELD_BYPASS_REQUIRE_PROJECT. Empty/false = permissive
	// back-compat. See service_field_policy.go.
	serviceFieldFleets         map[string]bool
	serviceFieldRequireProject bool

	// controlMailFleets are the fleet identities that may create mail
	// carrying a control: label (control:kill, ...). Set from
	// KBEADS_CONTROL_MAIL_FLEETS at construction; see mail_control.go.
	controlMailFleets map[string]bool

	// landsOperatorOrgs are the operator's GitHub accounts, lower-cased: a
	// Lands-pinned project may not use a repo they own (land_operator_org.go).
	// Set via SetLandsOperatorGitHubOrgs from BEADS_LANDS_OPERATOR_GITHUB_ORGS.
	// Empty = no check.
	landsOperatorOrgs map[string]bool

	// mrUpsertLocks serialises daemon-side dedup of type=merge-request beads
	// per fields.mr_url (ka-0000002762). createBead acquires the per-URL mutex
	// via lockMergeRequestUpsert across the whole lookup-or-return + insert
	// window so two concurrent CreateBeads for one MR converge to a single
	// canonical bead — the store-enforced guarantee the bridge's in-process
	// client lock (beadsapi mrUpsertLock) structurally cannot be. See
	// beads_mr_dedup.go. The zero value is a ready-to-use sync.Map.
	mrUpsertLocks sync.Map

	// channelWriteMu serialises type=channel writes so the name-per-land and
	// slack_channel_id uniqueness lookups (channel_validation.go) cannot
	// interleave with a concurrent insert or update. Channel writes are rare.
	channelWriteMu sync.Mutex

	// cardWriteMu serialises the one-card-per-(subject_id, channel_id) lookup
	// with the card insert or update (card_validation.go). Cards are written
	// once per subject and channel, so contention is low.
	cardWriteMu sync.Mutex

	// gatehouseRequestMu serialises the gatehouse request dedup and hourly
	// count with the insert (gatehouse_request.go admitGatehouseRequest).
	gatehouseRequestMu sync.Mutex

	// landInviteMu serialises the one-open-invite-per-account lookup and the
	// open_invite_cap count with the insert (land_invite.go admitLandInvite).
	landInviteMu sync.Mutex

	// releaseWriteMu serialises release writes (releases.go): the derived
	// previous on create, and a deployment, which closes the open row of the
	// release that held the target and recomputes both statuses. Exactly one
	// release is open per (project, target) only if these cannot interleave.
	releaseWriteMu sync.Mutex

	// readCursors debounces writes of each person's read_cursors
	// (http_read_cursors.go).
	readCursors *readCursorBuffer
	// windowViews holds window view counts until they are written
	// (window_routes.go windowViewCounter).
	windowViews *windowViewTally
	// windowLive is the live window feed (window_live.go), a tap on sseHub.
	windowLive *windowLiveFeed

	// landDoorMessages caps type=message creates through a land's door
	// (land_door_message_rate.go); built from the env on first use unless a
	// test set it.
	landDoorMessages     *landDoorMessageRate
	landDoorMessagesOnce sync.Once

	// laneSeen is the presence lanes' last-seen throttle and read cache
	// (escalation_lane_presence.go).
	laneSeen laneSeenState
}

// NewBeadsServer returns a new BeadsServer backed by the given store and publisher.
func NewBeadsServer(s store.Store, p events.Publisher) *BeadsServer {
	if p == nil {
		p = &events.NoopPublisher{}
	}
	srv := &BeadsServer{
		store:           s,
		publisher:       p,
		sseHub:          newSSEHub(),
		hooksHandler:    hooks.NewHandler(s, slog.Default()),
		Presence:        presence.New(),
		userProvisioner: NewUserProvisioner(s, p),
		revocation:      NewTokenRevocationStore(s),
		bridgeSubjects:  bridgeSubjectSet(defaultBridgeSubjects),

		controlMailFleets: controlMailFleetsFromEnv(),

		taintRegistryCache:  newTaintRegistryCache(),
		grantSetCache:       newPrincipalCache[[]int64](grantSetCacheTTL),
		spawnChainRootCache: newPrincipalCache[string](spawnChainRootCacheTTL),
	}
	srv.readCursors = newReadCursorBuffer(readCursorWriteInterval, srv.writeReadCursors)
	srv.windowLive = newWindowLiveFeed(srv.windowLiveFrame)
	srv.sseHub.addTap(srv.windowLive.publish)
	return srv
}

// RevocationStore returns the DB-backed revoked-token cache. Exposed so the
// cmd/kd/serve wiring can Load() it on startup and tests can inspect it.
func (s *BeadsServer) RevocationStore() *TokenRevocationStore {
	return s.revocation
}

// SetFleetAuth stores the fleet config so that fleet token management
// endpoints can update the in-memory map at runtime.
func (s *BeadsServer) SetFleetAuth(fc *FleetConfig) {
	s.fleetCfg = fc
}

// SetRevokedTokens seeds the in-memory revocation cache with the given
// SHA-256 hashes. Historically this was the only revocation path — the list
// came from BEADS_REVOKED_TOKENS and was lost on every restart. Runtime
// revocations now go through the DB-backed RevocationStore; this method
// remains for the env-seed path and is a no-op on empty input.
func (s *BeadsServer) SetRevokedTokens(hashes []string) {
	if s.revocation == nil {
		s.revocation = NewTokenRevocationStore(s.store)
	}
	s.revocation.Seed(hashes)
}

// SetRefreshTokenManager wires the refresh-token manager. When set, the
// POST /v1/auth/refresh endpoint is functional and POST /v1/fleet-tokens
// can opt in to with_refresh:true. The manager stores refresh hashes in
// the same store passed to NewBeadsServer.
func (s *BeadsServer) SetRefreshTokenManager(m *RefreshTokenManager) {
	s.refreshTokens = m
}

// RefreshTokenManager returns the wired refresh-token manager (or nil).
// Exposed for tests.
func (s *BeadsServer) RefreshTokenManager() *RefreshTokenManager {
	return s.refreshTokens
}

// SetRefreshRotation toggles the refresh-token rotation policy. When true,
// every successful exchange mints a new refresh and revokes the presented
// one (RFC 6749 §10.4 single-use rotation). When false, the original
// refresh stays valid until expiry. Operator decision ka-0000000806 resolved
// rotate-on; serve.go wires this on by default with a BEADS_REFRESH_ROTATE=0
// escape hatch. The struct default remains false so tests opt in explicitly.
func (s *BeadsServer) SetRefreshRotation(rotate bool) {
	s.refreshRotate = rotate
}

// SetEmbeddingStatsProvider wires a provider used by the /metrics endpoint
// to compute gardenio_beads_embedding_coverage_ratio and gardenio_beads_embedding_stale_count.
// Pass nil to disable (NoopStatsProvider is used automatically).
func (s *BeadsServer) SetEmbeddingStatsProvider(p metrics.StatsProvider) {
	s.embedStats = p
}

// SetBus attaches an event bus for JetStream publishing. When set, mutation
// events are published to JetStream streams in addition to basic NATS pub/sub.
func (s *BeadsServer) SetBus(bus *eventbus.Bus) {
	s.bus = bus
}

// SetEmbedder wires the embedding backend used for semantic search over beads.
// Pass a NoopEmbedder to leave embedding disabled.
func (s *BeadsServer) SetEmbedder(e embedding.Embedder) {
	s.embedder = e
}

// SetKeycloakAdmin configures the Keycloak Admin API client for session
// revocation. When set, the POST /v1/admin/revoke-user endpoint is enabled.
func (s *BeadsServer) SetKeycloakAdmin(client *KeycloakAdminClient) {
	s.keycloakAdmin = client
}

// SetTrustedProxies configures CIDR ranges for trusted reverse proxies.
// When set, X-Forwarded-For and X-Real-Ip headers are only trusted when
// the direct peer address falls within the configured ranges.
func (s *BeadsServer) SetTrustedProxies(tp *TrustedProxyConfig) {
	s.trustedProxies = tp
}

// SetRateLimit configures per-identity token-bucket rate limits for HTTP
// mutation and read endpoints. A zero / Enabled=false config disables the
// middleware entirely. Default config (DefaultRateLimitConfig) is applied
// when NewHTTPHandler runs and no SetRateLimit call has been made.
func (s *BeadsServer) SetRateLimit(cfg RateLimitConfig) {
	c := cfg
	s.rateLimitCfg = &c
}

// SetEdgeAuth configures daemon-side verification of the gardenio-edge-auth
// HMAC envelope (Stage 3 / W-B-11, ka-0000001678). When set, NewHTTPHandler
// installs EdgeAuthMiddleware at the top of the chain. A zero / Enabled()==false
// config plus Enforce=false is equivalent to not calling this at all
// (no-op pass-through). Enforce=true rejects requests with non-OK outcomes.
func (s *BeadsServer) SetEdgeAuth(cfg edgeauth.Config) {
	c := cfg
	s.edgeAuthCfg = &c
}

// Bus returns the event bus, or nil if not configured.
func (s *BeadsServer) Bus() *eventbus.Bus {
	return s.bus
}

// recordAndPublish persists an event to the store and publishes it to NATS.
// Both operations are best-effort; failures are logged but do not block the caller.
func (s *BeadsServer) recordAndPublish(ctx context.Context, topic, beadID, actor string, event any) {
	// Beads-events counter at the source (epic E4, ka-0000001152). Counted
	// UNCONDITIONALLY here at function entry — before the migration-026
	// fail-closed audit skip below — so gardenio_beads_events_total reflects
	// true emission volume, not the subset of events that reach the durable
	// events table. The events-table backfill (E4b) is a best-effort lower
	// bound, never an authoritative replay. See R4-delta §2.1.
	recordBeadEventMetric(topic, event)
	s.invalidateGrantSetsForEvent(topic, event)

	payload, err := json.Marshal(event)
	if err != nil {
		slog.Warn("failed to marshal event", "topic", topic, "bead_id", beadID, "error", err)
		return
	}
	// Best-effort observability: when the operator cannot be resolved (e.g.
	// auth-disabled smoke tests), log + skip the RecordEvent call so the
	// migration-026 NOT VALID constraint on events.operated_by_id never sees
	// a NULL insert. Publish/broadcast still fire so live SSE subscribers
	// observe the mutation; the durable events table loses this row.
	operatedBy, operatedByID, _, ok := s.resolveAuditOperatorRequired(ctx)
	// Per-operator bead.created counter (ka-0000007678): attribute the create
	// to the resolved operator so /charts can draw one line per human. Counted
	// here — after resolution — rather than in the unconditional
	// recordBeadEventMetric above, because the operator breakdown is the whole
	// point. Unattributable creates (ok==false → operatedBy=="") bucket to the
	// "unknown" label, so the counter never silently drops a create.
	recordBeadCreatedByOperator(topic, operatedBy)
	if !ok {
		slog.Warn("skipping RecordEvent: operator unresolvable",
			"topic", topic, "bead_id", beadID)
	} else {
		if err := s.store.RecordEvent(ctx, &model.Event{
			Topic:        topic,
			BeadID:       beadID,
			Actor:        actor,
			Payload:      persistedEventPayload(topic, event, payload),
			OperatedBy:   operatedBy,
			OperatedByID: &operatedByID,
		}); err != nil {
			slog.Warn("failed to record event", "topic", topic, "bead_id", beadID, "error", err)
		}
	}
	if err := s.publisher.Publish(ctx, topic, event); err != nil {
		slog.Warn("failed to publish event", "topic", topic, "bead_id", beadID, "error", err)
	}
	s.broadcastEvent(ctx, topic, beadID, event)

	// Operator-activity backbone counter (ka-0000006613). Only the
	// mutation-class topics (create/update/close/delete/comment) count as a
	// "mutation" surface=kbeads activity — dependency/advice/mail topics map
	// to "" via topicToMutationType and are skipped. operatedByID is the
	// resolved operators(id) FK; an unattributable operator (ok==false) keeps
	// the zero value, which recordOperatorActivity renders as the "0" sentinel.
	if topicToMutationType(topic) != "" {
		recordOperatorActivity(operatedByID, TokenProjectFromContext(ctx), activityTypeMutation, surfaceKbeads)
	}

	// Publish to JetStream mutation stream if bus is configured.
	if s.bus != nil {
		if mutationType := topicToMutationType(topic); mutationType != "" {
			s.bus.PublishRaw(
				eventbus.SubjectMutationPrefix+mutationType,
				payload,
			)
		}
	}

	// First-class plan step-added signal (P3 producer, ka-0000004264). Riding
	// this single dependency-added chokepoint means EVERY AddDependency path —
	// gRPC, HTTP, auto-parent, formula pour, triggered-by-on-claim — is covered
	// uniformly: if the new edge is a plan step, emit the durable deduped
	// beads.plan.step.added the controller reactor consumes. operatedBy (the
	// resolved oidc:/slack: identity) attributes the downstream nudge; it falls
	// back to the edge's created_by when auth is disabled / unattributable.
	if topic == events.TopicDependencyAdded {
		if da, ok := event.(events.DependencyAdded); ok && da.Dependency != nil {
			attributedTo := operatedBy
			if attributedTo == "" {
				attributedTo = da.Dependency.CreatedBy
			}
			s.maybeEmitPlanStepAdded(ctx, da.Dependency, attributedTo)
		}
	}
}

// topicToMutationType maps a legacy event topic to a JetStream mutation event
// type suffix. Returns "" for topics that are not mutation events.
func topicToMutationType(topic string) string {
	switch topic {
	case events.TopicBeadCreated:
		return string(eventbus.EventMutationCreate)
	case events.TopicBeadUpdated:
		return string(eventbus.EventMutationUpdate)
	case events.TopicBeadClosed:
		return string(eventbus.EventMutationStatus)
	case events.TopicBeadDeleted:
		return string(eventbus.EventMutationDelete)
	case events.TopicCommentAdded:
		return string(eventbus.EventMutationComment)
	}
	return ""
}

// inputError indicates invalid user input.
// Transport layers map this to 400 / InvalidArgument.
type inputError string

func (e inputError) Error() string { return string(e) }

// operatorRefusalErrorCode is the structured error code embedded in HTTP
// 400 bodies and gRPC InvalidArgument messages when the resolver fails
// closed. The kd CLI keys on this code (not the human message) to decide
// whether to exit with the EX_USAGE-family sentinel exit code 64 and emit
// its structured stderr payload. See ka-0000006731.
const operatorRefusalErrorCode = "operator_refusal"

// operatorRefusalMessage is the canonical human-readable message embedded
// in the error. The kd CLI's structured-stderr parser keys on
// operatorRefusalErrorCode, not on this string, but consumers may surface
// the message verbatim in logs.
const operatorRefusalMessage = "operator could not be resolved"

// operatorRefusalError is the structured error returned by write paths
// when resolveAuditOperatorRequired fails closed. The Source field carries
// the classifyOperatorRefusal label so clients can tell WHY attribution
// failed (header_missing, header_non_human, fallback_agent_only,
// chain_root_emailless). Transport layers serialize it into a JSON body
// for HTTP and into a structured message for gRPC; the kd CLI parses
// either form into a refusal exit. See ka-0000006731.
type operatorRefusalError struct {
	Source string
}

func (e *operatorRefusalError) Error() string {
	if e == nil || e.Source == "" {
		return operatorRefusalMessage
	}
	return operatorRefusalMessage + " (resolver_source=" + e.Source + ")"
}

// newOperatorRefusal builds a refusal error with the resolver-source label
// classified from the request context. Used by every server write path that
// calls resolveAuditOperatorRequired.
func newOperatorRefusal(ctx context.Context) *operatorRefusalError {
	return &operatorRefusalError{Source: classifyOperatorRefusal(ctx)}
}

// grpcOperatorRefusal builds a gRPC InvalidArgument status whose message
// embeds the structured refusal payload as JSON. The kd CLI's gRPC client
// path parses this back into a refusal exit (code 64) symmetrically with
// the HTTP `code`/`resolver_source` JSON body. See ka-0000006731.
func grpcOperatorRefusal(source string) error {
	if source == "" {
		source = refusalSourceUnknown
	}
	// JSON-shaped message so a single client-side parser handles both
	// transports. Format kept simple (no nested structs) so the embedded
	// payload survives status.Error's plain-text round-tripping.
	msg := fmt.Sprintf(
		`%s {"code":%q,"resolver_source":%q}`,
		operatorRefusalMessage,
		operatorRefusalErrorCode,
		source,
	)
	return status.Error(codes.InvalidArgument, msg)
}

// formatFieldErrors renders a slice of model.FieldError as a single
// human-readable string suitable for an inputError message.
func formatFieldErrors(errs []model.FieldError) string {
	parts := make([]string, len(errs))
	for i, fe := range errs {
		parts[i] = fe.Field + ": " + fe.Message
	}
	return strings.Join(parts, "; ")
}

// AddDependency creates a dependency between two beads.
func (s *BeadsServer) AddDependency(ctx context.Context, req *beadsv1.AddDependencyRequest) (*beadsv1.AddDependencyResponse, error) {
	if req.GetBeadId() == "" {
		return nil, status.Error(codes.InvalidArgument, "bead_id is required")
	}
	if req.GetDependsOnId() == "" {
		return nil, status.Error(codes.InvalidArgument, "depends_on_id is required")
	}
	if _, err := s.loadBeadForWriteGRPC(ctx, req.GetBeadId(), "update", true); err != nil {
		return nil, err
	}
	hidden, denial := s.scopedDependencyTargetDenial(ctx, req.GetDependsOnId())
	if hidden {
		return nil, status.Error(codes.NotFound, "depends_on bead not found")
	}
	if denial != nil {
		return nil, status.Error(codes.PermissionDenied, denial.Error())
	}

	// SECURITY (AUDIT §C3): ignore req.CreatedBy — derive from auth identity.
	// Post-cutover (017): deps.operated_by_id is NOT NULL (ka-0000001391).
	now := time.Now().UTC()
	operatedBy, operatedByID, refusalSource, ok := s.resolveAuditOperatorRequired(ctx)
	if !ok {
		return nil, grpcOperatorRefusal(refusalSource)
	}
	dep := &model.Dependency{
		BeadID:       req.GetBeadId(),
		DependsOnID:  req.GetDependsOnId(),
		Type:         model.DependencyType(req.GetType()),
		CreatedAt:    now,
		CreatedBy:    actorFromContext(ctx),
		OperatedBy:   operatedBy,
		OperatedByID: &operatedByID,
	}

	// Data-hygiene guard (ka-0000006880): auto-canonicalize a reversed
	// parent-child edge (epic as bead_id / child side) before the insert.
	s.canonicalizeParentChildDep(ctx, dep)

	// Cycle guard (ka-0000006899): reject a parent-child edge that would
	// close a cycle in the parent-child graph. A cycle makes an open epic a
	// descendant of itself, wedging subtree-drain gardener dormancy preflights
	// awake forever. Checked after canonicalization so the orientation is
	// final.
	if s.wouldCreateParentChildCycle(ctx, dep) {
		slog.Warn("rejecting parent-child dep that would create a cycle",
			"bead_id", dep.BeadID,
			"depends_on_id", dep.DependsOnID,
			"created_by", dep.CreatedBy,
			"bead", "ka-0000006899",
		)
		return nil, status.Errorf(codes.FailedPrecondition,
			"parent-child dependency %s -> %s would create a cycle", dep.BeadID, dep.DependsOnID)
	}

	// Closed-parent guard (ka-0000005756): reject a parent-child edge whose
	// PARENT is already closed. A child grafted onto a dead epic enters
	// `gb ready` as a claimable orphan the auto-close cascade can never drain
	// (auto_close.go skips closed parents). The author named the parent
	// deliberately, so REJECT loudly (vs the auto-parent path's silent skip)
	// and point them at a live parent. X-Force-Parent-Edge (`kd dep add
	// --force`) overrides for legitimate re-parenting onto a recently-closed
	// epic. Checked after canonicalization so dep.DependsOnID is the true parent.
	if !forceParentEdgeFromContext(ctx) {
		if parent := s.closedParentSide(ctx, dep); parent != nil {
			slog.Warn("rejecting parent-child dep onto a CLOSED parent",
				"bead_id", dep.BeadID,
				"closed_parent", parent.ID,
				"parent_closed_at", closedAtString(parent),
				"created_by", dep.CreatedBy,
				"bead", "ka-0000005756",
			)
			return nil, status.Errorf(codes.FailedPrecondition,
				"parent-child dependency: parent %s is closed; pick a live parent or pass --force to re-parent onto it", dep.DependsOnID)
		}
	}

	// Cycle guard (ka-0000007984): reject a blocks edge that would close a cycle
	// in the blocks graph. A blocks cycle (e.g. the mutual pair A blocked-by B,
	// B blocked-by A) makes every bead in and downstream of the cycle
	// permanently undispatchable — nothing surfaces the condition, so catch it
	// at the write, the cheapest place.
	if s.wouldCreateBlocksCycle(ctx, dep) {
		rejectBlocksCycleLog(dep)
		return nil, status.Errorf(codes.FailedPrecondition,
			"blocks dependency %s -> %s would create a cycle", dep.BeadID, dep.DependsOnID)
	}

	if err := s.addDependencyWithTaintPropagation(ctx, dep); err != nil {
		return nil, status.Errorf(codes.Internal, "failed to add dependency: %v", err)
	}

	// Taint propagation on write (ADR docs/adr/taints.md §4.2 row 4): the
	// dependent bead also gets the WRITER's own taints, independent of the
	// triggered-by/derived-from target-taint union addDependencyWithTaintPropagation
	// already applied above.
	if err := s.propagateWriteTaint(ctx, dep.BeadID, dep.CreatedBy); err != nil {
		return nil, status.Errorf(codes.Internal, "failed to propagate write taint: %v", err)
	}

	s.recordAndPublish(ctx, events.TopicDependencyAdded, dep.BeadID, dep.CreatedBy, events.DependencyAdded{Dependency: dep, Fleet: FleetFromContext(ctx)})
	// Proposal C (ka-0000007756): audit the edge add so `kd audit bead` can
	// answer "why did this edge change" post-hoc. Uses the canonicalized dep.
	s.recordDependencyMutation(ctx, dep.BeadID, dep.DependsOnID, string(dep.Type), dep.CreatedBy, model.MutationTypeCreate)
	s.inheritParentProjectLabel(ctx, dep)

	return &beadsv1.AddDependencyResponse{
		Dependency: dependencyToProto(dep),
	}, nil
}

// RemoveDependency removes a dependency between two beads.
func (s *BeadsServer) RemoveDependency(ctx context.Context, req *beadsv1.RemoveDependencyRequest) (*beadsv1.RemoveDependencyResponse, error) {
	if req.GetBeadId() == "" {
		return nil, status.Error(codes.InvalidArgument, "bead_id is required")
	}
	if req.GetDependsOnId() == "" {
		return nil, status.Error(codes.InvalidArgument, "depends_on_id is required")
	}
	if _, err := s.loadBeadForWriteGRPC(ctx, req.GetBeadId(), "update", true); err != nil {
		return nil, err
	}
	// Both ends, as on add: a scoped principal may not unlink a bead outside
	// its projects. A target the caller cannot see unlinks as a missing one
	// does: nothing removed.
	hidden, denial := s.scopedDependencyTargetDenial(ctx, req.GetDependsOnId())
	if hidden {
		removed := false
		return &beadsv1.RemoveDependencyResponse{Removed: &removed}, nil
	}
	if denial != nil {
		return nil, status.Error(codes.PermissionDenied, denial.Error())
	}

	removed, err := s.removeDependencyEdge(ctx, req.GetBeadId(), req.GetDependsOnId(), req.GetType())
	if err != nil {
		return nil, status.Errorf(codes.Internal, "failed to remove dependency: %v", err)
	}

	return &beadsv1.RemoveDependencyResponse{Removed: &removed}, nil
}

// removeDependencyEdge deletes one dependency edge and records the removal
// event + audit row ONLY when a row actually matched, returning whether it did.
//
// Both transports (this gRPC method and the HTTP handleRemoveDependency) go
// through here so the removed-vs-no-op invariant lives in exactly ONE place.
// That matters: the original bug — recording a "dependency removed" event and
// audit row for a delete that matched nothing, so `kd audit bead` reported
// removals that never happened — was duplicated across both transports
// precisely because each had its own copy of this sequence (ka-0000001966).
//
// A 0-match is deliberately NOT an error: idempotent callers (`kd reparent`,
// `kd admin backfill-mr-edges`) rely on removing an already-absent edge
// succeeding.
func (s *BeadsServer) removeDependencyEdge(ctx context.Context, beadID, dependsOnID, depType string) (bool, error) {
	removed, err := s.store.RemoveDependency(ctx, beadID, dependsOnID, model.DependencyType(depType))
	if err != nil {
		return false, err
	}
	if removed {
		s.recordAndPublish(ctx, events.TopicDependencyRemoved, beadID, "", events.DependencyRemoved{
			BeadID:      beadID,
			DependsOnID: dependsOnID,
			Type:        depType,
			Fleet:       FleetFromContext(ctx),
		})
		// Proposal C (ka-0000007756): audit the edge removal.
		s.recordDependencyMutation(ctx, beadID, dependsOnID, depType, actorFromContext(ctx), model.MutationTypeDelete)
	}
	return removed, nil
}

// GetDependencies returns all dependencies for a bead.
func (s *BeadsServer) GetDependencies(ctx context.Context, req *beadsv1.GetDependenciesRequest) (*beadsv1.GetDependenciesResponse, error) {
	if req.GetBeadId() == "" {
		return nil, status.Error(codes.InvalidArgument, "bead_id is required")
	}
	if ok, err := s.authorizeSubresourceReadGRPC(ctx, req.GetBeadId()); err != nil {
		return nil, err
	} else if !ok {
		return &beadsv1.GetDependenciesResponse{}, nil
	}

	deps, err := s.store.GetDependencies(ctx, req.GetBeadId())
	if err != nil {
		return nil, status.Errorf(codes.Internal, "failed to get dependencies: %v", err)
	}

	pbDeps := make([]*beadsv1.Dependency, 0, len(deps))
	for _, d := range deps {
		pbDeps = append(pbDeps, dependencyToProto(d))
	}

	return &beadsv1.GetDependenciesResponse{Dependencies: pbDeps}, nil
}

// childrenStatusHistogramMaxParentsGRPC mirrors the HTTP cap so the gRPC
// surface enforces the same bound; see handleBatchChildrenByStatus.
const childrenStatusHistogramMaxParentsGRPC = 100

// GetChildrenStatusHistogram returns a per-parent histogram of parent-child
// children by status. Parents that requested but have no parent-child
// children appear with an empty status_counts map so the caller can rely on
// a stable shape. See gardenio/plans/project-view-epic-sequencer.md AC-2.
func (s *BeadsServer) GetChildrenStatusHistogram(ctx context.Context, req *beadsv1.GetChildrenStatusHistogramRequest) (*beadsv1.GetChildrenStatusHistogramResponse, error) {
	parentIDs := req.GetParentIds()
	if len(parentIDs) == 0 {
		return nil, status.Error(codes.InvalidArgument, "parent_ids is required")
	}
	if len(parentIDs) > childrenStatusHistogramMaxParentsGRPC {
		return nil, status.Errorf(codes.InvalidArgument,
			"maximum %d parent_ids per request", childrenStatusHistogramMaxParentsGRPC)
	}
	cleaned := make([]string, 0, len(parentIDs))
	for _, id := range parentIDs {
		id = strings.TrimSpace(id)
		if id == "" {
			continue
		}
		cleaned = append(cleaned, id)
	}
	if len(cleaned) == 0 {
		return nil, status.Error(codes.InvalidArgument, "parent_ids is required")
	}

	// Enforce cross-project read scope (ka-0000008132): mirror the HTTP twin
	// handleBatchChildrenByStatus. A parent's child-status histogram is scoped
	// metadata (subtree shape), so resolve which requested parents the caller
	// may read under BOTH gates (fleet-token project + OIDC allowed_projects)
	// and return an empty histogram for any parent outside scope. Computed once
	// (single GetBeadsByIDs + one oidcAllowedProjects lookup), not per row.
	authorized := make(map[string]bool, len(cleaned))
	scopeBeads, err := s.store.GetBeadsByIDs(ctx, cleaned)
	if err != nil {
		return nil, status.Errorf(codes.Internal, "failed to fetch beads for scope check: %v", err)
	}
	allowedOIDC := oidcAllowedProjects(ctx, s.store)
	taintGate, err := s.resolveBeadTaintGateState(ctx)
	if err != nil {
		return nil, status.Errorf(codes.Internal, "failed to resolve taint grants: %v", err)
	}
	var taintsByBead map[string][]int64
	if !taintGate.exempt {
		taintsByBead, err = s.store.GetTaintsForBeads(ctx, cleaned)
		if err != nil {
			return nil, status.Errorf(codes.Internal, "failed to fetch bead taints: %v", err)
		}
	}
	for _, b := range scopeBeads {
		if s.beadReadAuthorized(ctx, b, allowedOIDC, taintsByBead[b.ID], taintGate) {
			authorized[b.ID] = true
		}
	}

	hist, err := s.store.GetChildrenStatusHistogram(ctx, cleaned)
	if err != nil {
		return nil, status.Errorf(codes.Internal, "failed to get children status histogram: %v", err)
	}

	histograms := make(map[string]*beadsv1.ChildStatusHistogram, len(cleaned))
	for _, id := range cleaned {
		counts := make(map[string]int32)
		if authorized[id] {
			for st, n := range hist[id] {
				counts[st] = int32(n)
			}
		}
		histograms[id] = &beadsv1.ChildStatusHistogram{StatusCounts: counts}
	}
	return &beadsv1.GetChildrenStatusHistogramResponse{Histograms: histograms}, nil
}

// ListMentionedBy returns the beads that reference req.BeadId. See
// kbeads/docs/design-bead-references.md § 5a.
func (s *BeadsServer) ListMentionedBy(ctx context.Context, req *beadsv1.ListMentionedByRequest) (*beadsv1.ListMentionedByResponse, error) {
	if req.GetBeadId() == "" {
		return nil, status.Error(codes.InvalidArgument, "bead_id is required")
	}
	limit := int(req.GetLimit())
	offset := int(req.GetOffset())
	if limit < 0 {
		return nil, status.Error(codes.InvalidArgument, "limit must be >= 0")
	}
	if offset < 0 {
		return nil, status.Error(codes.InvalidArgument, "offset must be >= 0")
	}
	if ok, err := s.authorizeSubresourceReadGRPC(ctx, req.GetBeadId()); err != nil {
		return nil, err
	} else if !ok {
		return &beadsv1.ListMentionedByResponse{Limit: int32(limit), Offset: int32(offset)}, nil
	}

	// relationship_type filter (typed-mention verb). Three-state semantics
	// match the HTTP ?type= shape: nil = no filter, &"" = untyped only,
	// &"<verb>" = exact match. The "mentions" alias for untyped-only is
	// applied client-side before the request is sent.
	var relType *string
	if req.RelationshipType != nil {
		v := req.GetRelationshipType()
		relType = &v
	}
	rows, total, err := s.store.ListBeadsMentioning(ctx, req.GetBeadId(), limit, offset, relType)
	if err != nil {
		return nil, status.Errorf(codes.Internal, "failed to list mentioned-by: %v", err)
	}
	rows, dropped, err := s.gateReferenceRows(ctx, rows, true)
	if err != nil {
		return nil, status.Errorf(codes.Internal, "failed to resolve taint grants: %v", err)
	}
	total -= dropped
	// Each row carries the other bead's title: inherit:read for those too,
	// as the HTTP twin does.
	if err := s.propagateReadTaintBeads(ctx, referenceSummaryIDs(rows)); err != nil {
		return nil, status.Errorf(codes.Internal, "failed to propagate read taint: %v", err)
	}

	pbRows := make([]*beadsv1.BeadReferenceSummary, 0, len(rows))
	for _, r := range rows {
		pbRows = append(pbRows, beadReferenceSummaryToProto(r))
	}

	return &beadsv1.ListMentionedByResponse{
		References: pbRows,
		Total:      int32(total),
		Limit:      int32(limit),
		Offset:     int32(offset),
	}, nil
}

// ListMentions returns the ka-IDs that req.BeadId references. See
// kbeads/docs/design-bead-references.md § 5a.
func (s *BeadsServer) ListMentions(ctx context.Context, req *beadsv1.ListMentionsRequest) (*beadsv1.ListMentionsResponse, error) {
	if req.GetBeadId() == "" {
		return nil, status.Error(codes.InvalidArgument, "bead_id is required")
	}
	limit := int(req.GetLimit())
	offset := int(req.GetOffset())
	if limit < 0 {
		return nil, status.Error(codes.InvalidArgument, "limit must be >= 0")
	}
	if offset < 0 {
		return nil, status.Error(codes.InvalidArgument, "offset must be >= 0")
	}
	if ok, err := s.authorizeSubresourceReadGRPC(ctx, req.GetBeadId()); err != nil {
		return nil, err
	} else if !ok {
		return &beadsv1.ListMentionsResponse{Limit: int32(limit), Offset: int32(offset)}, nil
	}

	// relationship_type filter (typed-mention verb). See ListMentionedBy for
	// the three-state semantics.
	var relType *string
	if req.RelationshipType != nil {
		v := req.GetRelationshipType()
		relType = &v
	}
	rows, total, err := s.store.ListBeadReferencesFrom(ctx, req.GetBeadId(), limit, offset, relType)
	if err != nil {
		return nil, status.Errorf(codes.Internal, "failed to list mentions: %v", err)
	}
	rows, _, err = s.gateReferenceRows(ctx, rows, false)
	if err != nil {
		return nil, status.Errorf(codes.Internal, "failed to resolve taint grants: %v", err)
	}
	// Each row carries the other bead's title: inherit:read for those too,
	// as the HTTP twin does.
	if err := s.propagateReadTaintBeads(ctx, referenceSummaryIDs(rows)); err != nil {
		return nil, status.Errorf(codes.Internal, "failed to propagate read taint: %v", err)
	}

	pbRows := make([]*beadsv1.BeadReferenceSummary, 0, len(rows))
	for _, r := range rows {
		pbRows = append(pbRows, beadReferenceSummaryToProto(r))
	}

	return &beadsv1.ListMentionsResponse{
		References: pbRows,
		Total:      int32(total),
		Limit:      int32(limit),
		Offset:     int32(offset),
	}, nil
}

// AddLabel adds a label to a bead.
func (s *BeadsServer) AddLabel(ctx context.Context, req *beadsv1.AddLabelRequest) (*beadsv1.AddLabelResponse, error) {
	if req.GetBeadId() == "" {
		return nil, status.Error(codes.InvalidArgument, "bead_id is required")
	}
	if req.GetLabel() == "" {
		return nil, status.Error(codes.InvalidArgument, "label is required")
	}
	target, err := s.loadBeadForWriteGRPC(ctx, req.GetBeadId(), "update", true)
	if err != nil {
		return nil, err
	}
	if denial := scopedLabelChangeDenial(ctx, s.store, target.Type, req.GetLabel(), true); denial != nil {
		return nil, status.Error(codes.PermissionDenied, denial.Error())
	}
	if denial := s.tenantAgentLabelDenial(ctx, "update", target.Type, []string{req.GetLabel()}); denial != nil {
		return nil, status.Error(codes.PermissionDenied, denial.Error())
	}
	if denial := commonsMessageLockDenial(ctx, "update", "change its labels", target); denial != nil {
		return nil, status.Error(codes.PermissionDenied, denial.Error())
	}
	if err := conversationLabelChangeError(ctx, s.store, target, req.GetLabel(), true); err != nil {
		return nil, status.Error(codes.InvalidArgument, err.Error())
	}
	if err := plotApplicationLabelChangeError(target, req.GetLabel(), true); err != nil {
		return nil, status.Error(codes.InvalidArgument, err.Error())
	}
	if err := s.projectLabelChangeError(ctx, target, req.GetLabel(), true); err != nil {
		return nil, status.Error(codes.InvalidArgument, err.Error())
	}
	if err := creditGrantLabelChangeError(target, req.GetLabel(), true); err != nil {
		return nil, status.Error(codes.InvalidArgument, err.Error())
	}
	if err := landInviteLabelChangeError(target, req.GetLabel(), true); err != nil {
		return nil, status.Error(codes.InvalidArgument, err.Error())
	}
	if err := s.heraldryLabelChangeError(ctx, target, req.GetLabel(), true); err != nil {
		return nil, heraldryGRPCError(err)
	}
	if denial := s.singleLabelChangeDenial(ctx, target, req.GetLabel()); denial != nil {
		return nil, status.Error(codes.PermissionDenied, denial.Error())
	}

	if err := s.store.AddLabel(ctx, req.GetBeadId(), req.GetLabel()); err != nil {
		return nil, status.Errorf(codes.Internal, "failed to add label: %v", err)
	}

	// Taint propagation on write (ADR docs/adr/taints.md §4.2 row 4).
	if err := s.propagateWriteTaint(ctx, req.GetBeadId(), actorFromContext(ctx)); err != nil {
		return nil, status.Errorf(codes.Internal, "failed to propagate write taint: %v", err)
	}

	s.recordAndPublish(ctx, events.TopicLabelAdded, req.GetBeadId(), "", events.LabelAdded{
		BeadID: req.GetBeadId(),
		Label:  req.GetLabel(),
		Fleet:  FleetFromContext(ctx),
	})

	// Fetch the updated bead to return.
	bead, err := s.store.GetBead(ctx, req.GetBeadId())
	if err != nil {
		return nil, status.Errorf(codes.Internal, "failed to get bead after adding label: %v", err)
	}
	if bead == nil {
		return nil, status.Error(codes.NotFound, "bead not found")
	}

	return &beadsv1.AddLabelResponse{Bead: beadToProto(bead)}, nil
}

// RemoveLabel removes a label from a bead.
func (s *BeadsServer) RemoveLabel(ctx context.Context, req *beadsv1.RemoveLabelRequest) (*beadsv1.RemoveLabelResponse, error) {
	if req.GetBeadId() == "" {
		return nil, status.Error(codes.InvalidArgument, "bead_id is required")
	}
	if req.GetLabel() == "" {
		return nil, status.Error(codes.InvalidArgument, "label is required")
	}
	target, err := s.loadBeadForWriteGRPC(ctx, req.GetBeadId(), "update", true)
	if err != nil {
		return nil, err
	}
	if denial := scopedLabelChangeDenial(ctx, s.store, target.Type, req.GetLabel(), false); denial != nil {
		return nil, status.Error(codes.PermissionDenied, denial.Error())
	}
	if denial := frozenAgentLabelDenial(ctx, s.store, target, labelsWithout(target.Labels, req.GetLabel())); denial != nil {
		return nil, status.Error(codes.PermissionDenied, denial.Error())
	}
	if denial := s.tenantAgentProjectLabelDenial(ctx, target.Type, target.Labels, labelsWithout(target.Labels, req.GetLabel())); denial != nil {
		return nil, status.Error(codes.PermissionDenied, denial.Error())
	}
	if denial := commonsMessageLockDenial(ctx, "update", "change its labels", target); denial != nil {
		return nil, status.Error(codes.PermissionDenied, denial.Error())
	}
	if err := conversationLabelChangeError(ctx, s.store, target, req.GetLabel(), false); err != nil {
		return nil, status.Error(codes.InvalidArgument, err.Error())
	}
	if err := plotApplicationLabelChangeError(target, req.GetLabel(), false); err != nil {
		return nil, status.Error(codes.InvalidArgument, err.Error())
	}
	if err := s.projectLabelChangeError(ctx, target, req.GetLabel(), false); err != nil {
		return nil, status.Error(codes.InvalidArgument, err.Error())
	}
	if err := creditGrantLabelChangeError(target, req.GetLabel(), false); err != nil {
		return nil, status.Error(codes.InvalidArgument, err.Error())
	}
	if err := landInviteLabelChangeError(target, req.GetLabel(), false); err != nil {
		return nil, status.Error(codes.InvalidArgument, err.Error())
	}
	if err := s.heraldryLabelChangeError(ctx, target, req.GetLabel(), false); err != nil {
		return nil, heraldryGRPCError(err)
	}
	if denial := s.singleLabelChangeDenial(ctx, target, req.GetLabel()); denial != nil {
		return nil, status.Error(codes.PermissionDenied, denial.Error())
	}

	removed, err := s.removeLabelEdge(ctx, req.GetBeadId(), req.GetLabel())
	if err != nil {
		return nil, status.Errorf(codes.Internal, "failed to remove label: %v", err)
	}

	// Taint propagation on write (ADR docs/adr/taints.md §4.2 row 4).
	if err := s.propagateWriteTaint(ctx, req.GetBeadId(), actorFromContext(ctx)); err != nil {
		return nil, status.Errorf(codes.Internal, "failed to propagate write taint: %v", err)
	}

	return &beadsv1.RemoveLabelResponse{Removed: &removed}, nil
}

// removeLabelEdge deletes one label and records the removal event ONLY when a
// row actually matched, returning whether it did.
//
// Both transports (the gRPC RemoveLabel above and the HTTP handleRemoveLabel)
// go through here so the removed-vs-no-op invariant lives in exactly ONE place.
// That matters: emitting a "label removed" event for a delete that matched
// nothing (as both paths did before ka-0000000424) fabricates a removal that
// never happened in bead history — the same phantom-history flaw the sibling
// dep-remove fix closed (ka-0000001966).
//
// A 0-match is deliberately NOT an error: idempotent callers (`kd update
// --remove-label`, SetLabels, the controller's label reconcilers) rely on
// removing an already-absent label succeeding.
func (s *BeadsServer) removeLabelEdge(ctx context.Context, beadID, label string) (bool, error) {
	removed, err := s.store.RemoveLabel(ctx, beadID, label)
	if err != nil {
		return false, err
	}
	if removed {
		s.recordAndPublish(ctx, events.TopicLabelRemoved, beadID, "", events.LabelRemoved{
			BeadID: beadID,
			Label:  label,
			Fleet:  FleetFromContext(ctx),
		})
	}
	return removed, nil
}

// GetLabels returns all labels for a bead.
func (s *BeadsServer) GetLabels(ctx context.Context, req *beadsv1.GetLabelsRequest) (*beadsv1.GetLabelsResponse, error) {
	if req.GetBeadId() == "" {
		return nil, status.Error(codes.InvalidArgument, "bead_id is required")
	}
	if ok, err := s.authorizeSubresourceReadGRPC(ctx, req.GetBeadId()); err != nil {
		return nil, err
	} else if !ok {
		return &beadsv1.GetLabelsResponse{}, nil
	}

	labels, err := s.store.GetLabels(ctx, req.GetBeadId())
	if err != nil {
		return nil, status.Errorf(codes.Internal, "failed to get labels: %v", err)
	}

	return &beadsv1.GetLabelsResponse{Labels: labels}, nil
}

// AddComment adds a comment to a bead.
func (s *BeadsServer) AddComment(ctx context.Context, req *beadsv1.AddCommentRequest) (*beadsv1.AddCommentResponse, error) {
	if req.GetBeadId() == "" {
		return nil, status.Error(codes.InvalidArgument, "bead_id is required")
	}
	if req.GetText() == "" {
		return nil, status.Error(codes.InvalidArgument, "text is required")
	}
	// Comment-create skips the ownership gate, as handleAddComment does
	// (ka-0000005884); token-project scope and write_policy still apply.
	if _, err := s.loadBeadForWriteGRPC(ctx, req.GetBeadId(), "comment", false); err != nil {
		return nil, err
	}

	// SECURITY (AUDIT §C3): ignore req.Author — derive from auth identity.
	// Post-cutover (017): comments.operated_by_id is NOT NULL (ka-0000001391).
	now := time.Now().UTC()
	operatedBy, operatedByID, refusalSource, ok := s.resolveAuditOperatorRequired(ctx)
	if !ok {
		return nil, grpcOperatorRefusal(refusalSource)
	}
	comment := &model.Comment{
		BeadID:       req.GetBeadId(),
		Author:       actorFromContext(ctx),
		Text:         redactCredShapes(req.GetText()), // F17 (R4-SEC-4): strip bare credential shapes
		CreatedAt:    now,
		OperatedBy:   operatedBy,
		OperatedByID: &operatedByID,
		// Stamped from the verified token, never from the request
		// (impersonation.go).
		ImpersonatedBy: impersonatedByFromContext(ctx),
	}

	if err := s.store.AddComment(ctx, comment); err != nil {
		return nil, status.Errorf(codes.Internal, "failed to add comment: %v", err)
	}

	// Taint propagation on write (ADR docs/adr/taints.md §4.2 row 4). Runs
	// post-commit, so a failure here errors a comment that already persisted —
	// a client retry would duplicate it; accepted as a known hazard on this
	// path only.
	if err := s.propagateWriteTaint(ctx, comment.BeadID, comment.Author); err != nil {
		return nil, status.Errorf(codes.Internal, "failed to propagate write taint: %v", err)
	}

	s.recordAndPublish(ctx, events.TopicCommentAdded, comment.BeadID, comment.Author, events.CommentAdded{Comment: comment, Fleet: FleetFromContext(ctx)})

	return &beadsv1.AddCommentResponse{
		Comment: commentToProto(comment),
	}, nil
}

// DeleteComment removes a single comment by id. Idempotent: returning
// deleted=false (rather than NotFound) when no comment matched on an existing
// bead lets the scheduler retry trim sweeps without distinguishing "already
// gone" from "never existed"; a missing bead is NotFound, as on HTTP.
// Successful deletes record a TopicCommentDeleted event;
// migration 022 makes comments.operated_by_id NOT NULL but the audit row
// itself lives in events, where operated_by_id is resolved by the
// recordAndPublish path.
func (s *BeadsServer) DeleteComment(ctx context.Context, req *beadsv1.DeleteCommentRequest) (*beadsv1.DeleteCommentResponse, error) {
	if req.GetBeadId() == "" {
		return nil, status.Error(codes.InvalidArgument, "bead_id is required")
	}
	if req.GetCommentId() <= 0 {
		return nil, status.Error(codes.InvalidArgument, "comment_id is required")
	}
	// Same gates as handleDeleteComment: token-project scope, write_policy and
	// ownership (gd-G0DPygReo5). Unlike comment-create, removing history is a
	// mutation of the bead, so the ownership gate applies.
	if _, err := s.loadBeadForWriteGRPC(ctx, req.GetBeadId(), "update", true); err != nil {
		return nil, err
	}

	deleted, err := s.store.DeleteComment(ctx, req.GetBeadId(), req.GetCommentId())
	if err != nil {
		return nil, status.Errorf(codes.Internal, "failed to delete comment: %v", err)
	}
	if deleted == nil {
		return &beadsv1.DeleteCommentResponse{Deleted: false}, nil
	}

	s.recordAndPublish(ctx, events.TopicCommentDeleted, deleted.BeadID, actorFromContext(ctx), events.CommentDeleted{
		BeadID:    deleted.BeadID,
		CommentID: deleted.ID,
		Author:    deleted.Author,
		Reason:    "delete",
		Fleet:     FleetFromContext(ctx),
	})

	return &beadsv1.DeleteCommentResponse{Deleted: true}, nil
}

// TrimComments keeps only the most recent keep_last comments on a bead,
// optionally restricted to comments authored by a single author so the
// scheduler can prune its own per-run history without disturbing other
// commentary. Each removed comment fans out one TopicCommentDeleted event
// (Reason="trim") so downstream consumers see a uniform per-row signal.
func (s *BeadsServer) TrimComments(ctx context.Context, req *beadsv1.TrimCommentsRequest) (*beadsv1.TrimCommentsResponse, error) {
	if req.GetBeadId() == "" {
		return nil, status.Error(codes.InvalidArgument, "bead_id is required")
	}
	if req.GetKeepLast() < 0 {
		return nil, status.Error(codes.InvalidArgument, "keep_last must be >= 0")
	}
	// Same gates as handleTrimComments (gd-G0DPygReo5); see DeleteComment.
	if _, err := s.loadBeadForWriteGRPC(ctx, req.GetBeadId(), "update", true); err != nil {
		return nil, err
	}

	deleted, err := s.store.TrimComments(ctx, req.GetBeadId(), int(req.GetKeepLast()), req.GetAuthor())
	if err != nil {
		return nil, status.Errorf(codes.Internal, "failed to trim comments: %v", err)
	}

	deletedIDs := make([]int64, 0, len(deleted))
	actor := actorFromContext(ctx)
	fleet := FleetFromContext(ctx)
	for _, c := range deleted {
		deletedIDs = append(deletedIDs, c.ID)
		s.recordAndPublish(ctx, events.TopicCommentDeleted, c.BeadID, actor, events.CommentDeleted{
			BeadID:    c.BeadID,
			CommentID: c.ID,
			Author:    c.Author,
			Reason:    "trim",
			Fleet:     fleet,
		})
	}

	return &beadsv1.TrimCommentsResponse{DeletedIds: deletedIDs}, nil
}

// GetComments returns all comments for a bead.
func (s *BeadsServer) GetComments(ctx context.Context, req *beadsv1.GetCommentsRequest) (*beadsv1.GetCommentsResponse, error) {
	if req.GetBeadId() == "" {
		return nil, status.Error(codes.InvalidArgument, "bead_id is required")
	}
	if ok, err := s.authorizeSubresourceReadGRPC(ctx, req.GetBeadId()); err != nil {
		return nil, err
	} else if !ok {
		return &beadsv1.GetCommentsResponse{}, nil
	}

	comments, err := s.store.GetComments(ctx, req.GetBeadId())
	if err != nil {
		return nil, status.Errorf(codes.Internal, "failed to get comments: %v", err)
	}

	pbComments := make([]*beadsv1.Comment, 0, len(comments))
	for _, c := range comments {
		pbComments = append(pbComments, commentToProto(c))
	}

	return &beadsv1.GetCommentsResponse{Comments: pbComments}, nil
}

// GetEvents returns all persisted events for a bead.
func (s *BeadsServer) GetEvents(ctx context.Context, req *beadsv1.GetEventsRequest) (*beadsv1.GetEventsResponse, error) {
	if req.GetBeadId() == "" {
		return nil, status.Error(codes.InvalidArgument, "bead_id is required")
	}
	if ok, err := s.authorizeSubresourceReadGRPC(ctx, req.GetBeadId()); err != nil {
		return nil, err
	} else if !ok {
		return &beadsv1.GetEventsResponse{}, nil
	}

	evts, err := s.store.GetEvents(ctx, req.GetBeadId())
	if err != nil {
		return nil, status.Errorf(codes.Internal, "failed to get events: %v", err)
	}

	pbEvents := make([]*beadsv1.Event, 0, len(evts))
	for _, e := range evts {
		pbEvents = append(pbEvents, eventToProto(e))
	}

	return &beadsv1.GetEventsResponse{Events: pbEvents}, nil
}

// Health returns the service health status.
func (s *BeadsServer) Health(_ context.Context, _ *beadsv1.HealthRequest) (*beadsv1.HealthResponse, error) {
	return &beadsv1.HealthResponse{Status: "ok"}, nil
}
