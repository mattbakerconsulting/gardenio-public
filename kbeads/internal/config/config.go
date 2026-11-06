package config

import (
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"
)

type Config struct {
	DatabaseURL string // BEADS_DATABASE_URL (required)
	// DatabasePasswordFile is the path to a file holding the DB password
	// (BEADS_DATABASE_PASSWORD_FILE). When set, the password is read from
	// this file per DB connection and injected into DatabaseURL in-process,
	// instead of relying on the frozen $(BEADS_DATABASE_PASSWORD) env-var
	// substitution baked into DatabaseURL at container start. A K8s Secret
	// volume live-updates on the kubelet sync loop, so a rotated password is
	// picked up WITHOUT a pod restart — the rotation-tolerant reconnect fix
	// for the RDS master-password CrashLoop (ka-0000004150 / P0 ka-0000006811).
	// Empty = legacy env-var mode (no behavior change).
	DatabasePasswordFile string // BEADS_DATABASE_PASSWORD_FILE (optional)
	GRPCAddr       string // BEADS_GRPC_ADDR (default ":9090")
	GRPCReflection bool   // BEADS_GRPC_REFLECTION (default false; enable only in dev)
	HTTPAddr    string // BEADS_HTTP_ADDR (default ":8080")
	MetricsAddr string // BEADS_METRICS_ADDR (default ":9091")
	NATSURL     string // BEADS_NATS_URL (optional, empty = no events)
	NATSToken   string // BEADS_NATS_TOKEN (optional; auth secret for the NATS connection, e.g. the subject-isolation infra token)
	NATSUser    string // BEADS_NATS_USER (optional; NATS username — when set the token is sent as that user's password, which is how nats.auth.subjectIsolation authenticates. Bare tokens cannot match a server users[] entry.)
	RevokedTokens  []string // BEADS_REVOKED_TOKENS (optional, comma-separated SHA-256 hashes of revoked tokens)
	DefaultFleet   string   // BEADS_DEFAULT_FLEET (default "default")
	FleetMap     string // BEADS_FLEET_MAP (optional, format: token1:fleet1,token2:fleet2)
	// CrossFleetReadFleets is a comma-separated list of fleet names whose
	// tokens get cross-fleet read visibility (write attribution still goes
	// through the resolved fleet). Used to mark bridge fleets so they can
	// observe activity from every fleet for external relay (Slack/etc.).
	// See ka-0000008234 follow-up.
	CrossFleetReadFleets string // BEADS_CROSS_FLEET_READ_FLEETS
	// PrivilegedFleets is a comma-separated list of fleet names whose tokens
	// are PRIVILEGED service-fleet identities (ka-0000001946) — they may bypass
	// per-bead ownership for system-managed graph writes (produces/triggered-by
	// deps, mr_url back-fill). Applied to every token of a listed fleet at
	// startup regardless of load path (fleet-map env, config bead, rotator), so
	// the live gitlab-bridge token loaded from BEADS_FLEET_MAP — which has no
	// per-token privileged field — actually receives the bypass.
	PrivilegedFleets []string // BEADS_PRIVILEGED_FLEETS

	// MachineClients is a comma-separated list of Keycloak client ids whose
	// service-account JWTs are accepted as machine principals
	// (authMethodMachine, gd-gQ4iLIpWCm). Empty (default) disables the machine
	// door entirely: behavior is identical to a daemon without this feature.
	MachineClients []string // BEADS_MACHINE_CLIENTS
	// ServiceFieldFleets is a comma-separated allowlist of fleet names whose
	// (non-privileged) fleet tokens may use the service-managed-fields
	// ownership bypass on shared trigger/schedule beads (stamping
	// last_checked/last_run/...). Empty = allow all fleets (back-compat).
	// Closes the cross-env cooldown-starvation class (ka-0000006237): a
	// sandbox bridge fleet not on the list falls back to the plain ownership
	// gate and is denied on beads it does not own.
	ServiceFieldFleets []string // BEADS_SERVICE_FIELD_FLEETS
	// BridgeSubjects are the fleet subjects whose X-Operated-By relay may
	// stand in for a human in team, grant and decision-responder resolution
	// (P7 Task 3). Every other caller's relay is attribution only. Unset keeps
	// the server default (the four bridges).
	BridgeSubjects []string // BEADS_BRIDGE_SUBJECTS
	// ServiceFieldBypassRequireProject additionally refuses the bypass to an
	// UNRESTRICTED fleet token (no project scope) on project-labeled
	// trigger/schedule beads. Default off — flip only after this deployment's
	// own bridge tokens are project-scoped, or the gate denies the legitimate
	// writer too (ka-0000006237 sequencing: Layer-3 token scoping first).
	ServiceFieldBypassRequireProject bool // BEADS_SERVICE_FIELD_BYPASS_REQUIRE_PROJECT
	// LandsOperatorGitHubOrgs is a comma-separated list of the operator's
	// GitHub accounts (gd-72m3STiqya). A project that mints with the Lands
	// GitHub App may not be bound to a repo one of them owns. Empty = no check.
	LandsOperatorGitHubOrgs []string // BEADS_LANDS_OPERATOR_GITHUB_ORGS

	// OIDC settings
	OIDCIssuerURL          string   // BEADS_OIDC_ISSUER_URL (optional, enables OIDC JWT auth)
	OIDCDiscoveryURL       string   // BEADS_OIDC_DISCOVERY_URL (optional: fetch the primary issuer's discovery doc here instead of at OIDCIssuerURL)
	OIDCCAFile             string   // BEADS_OIDC_CA_FILE (optional: PEM CA trusted beside the system roots for discovery + JWKS, e.g. the keycloak-runtime in-cluster CA)
	OIDCIssuerURLs         []string // BEADS_OIDC_ISSUER_URLS (optional, comma-separated additional issuers)
	OIDCAudience           string   // BEADS_OIDC_AUDIENCE (required when OIDC is enabled)
	OIDCAllowedEmailDomains []string // BEADS_OIDC_ALLOWED_EMAIL_DOMAINS (optional, comma-separated)
	// EscalationCoordinatorClients (BEADS_ESCALATION_COORDINATOR_CLIENTS,
	// comma-separated) are the machine client ids that may act in an
	// escalation coordinator lane. Empty means none.
	EscalationCoordinatorClients []string
	// EscalationResultMachines (BEADS_ESCALATION_RESULT_MACHINES) are machine
	// client ids whose escalation results are accepted without an approval
	// (D6(d)). Empty at rollout.
	EscalationResultMachines []string
	// ClientRoleCeiling caps the gardenio ladder role of OIDC tokens issued
	// through the listed clients (azp), e.g. "gardenio-slack-link=gardenio-operator":
	// a person who holds gardenio-admin acts as gardenio-operator through that
	// client (BEADS_CLIENT_ROLE_CEILING, comma-separated client=role pairs).
	// The Slack door never carries admin (decision gd-8PT2Qr2gEo); enforcing
	// it at the resource server bounds a stolen Slack-door credential.
	ClientRoleCeiling map[string]string

	// Keycloak Admin API settings (for session revocation)
	KeycloakAdminURL          string // BEADS_KEYCLOAK_ADMIN_URL (optional, e.g., "https://keycloak.example.com/auth")
	KeycloakAdminRealm        string // BEADS_KEYCLOAK_ADMIN_REALM (default: same as OIDC realm)
	KeycloakAdminClientID     string // BEADS_KEYCLOAK_ADMIN_CLIENT_ID (service account client)
	KeycloakAdminClientSecret string // BEADS_KEYCLOAK_ADMIN_CLIENT_SECRET

	// Trusted proxy settings
	TrustedProxies []string // BEADS_TRUSTED_PROXIES (optional, comma-separated CIDRs)

	// Audit retention settings
	AuditRetentionDays int // BEADS_AUDIT_RETENTION_DAYS (default 90; 0 = disabled)

	// Slow-tool-call ledger retention (gd-4psiVnwXdj)
	SlowToolCallsRetentionDays int // BEADS_SLOW_TOOL_CALLS_RETENTION_DAYS (default 60; 0 = disabled)

	// Trigger/agent bead.updated event and fields-history retention (gd-1F8R0SQtiQ)
	HistoryRetentionDays int // BEADS_HISTORY_RETENTION_DAYS (default 14; 0 = disabled)

	// Embedding settings (P0: semantic search for beads)
	EmbedBackend string // BEADS_EMBED_BACKEND (default "noop"; supported: "noop", "openai")
	EmbedURL     string // BEADS_EMBED_URL (default "http://localhost:11434"; base URL for the embedding backend)
	EmbedAPIKey  string // BEADS_EMBED_API_KEY (optional; used by backends that require auth)
	EmbedModel   string // BEADS_EMBED_MODEL (optional; e.g. "bedrock-titan-embed-v2")

	// BridgeTokenResyncInterval is how often the daemon re-reads persisted
	// bridge_token:* rows into its in-memory token map (default 30s; 0 =
	// disabled). Without it a token registered on another daemon process after
	// this one started is unknown here until the next restart (gd-PPzLrS2k8e).
	BridgeTokenResyncInterval time.Duration // BEADS_BRIDGE_TOKEN_RESYNC_INTERVAL

	// Sync settings
	SyncInterval   time.Duration // BEADS_SYNC_INTERVAL (default 3m; 0 = disabled)
	SyncS3Bucket   string        // BEADS_SYNC_S3_BUCKET (enables S3 when set)
	SyncS3Endpoint string        // BEADS_SYNC_S3_ENDPOINT (custom endpoint for MinIO)
	SyncS3Region   string        // BEADS_SYNC_S3_REGION (default "us-east-1")
	SyncS3Key      string        // BEADS_SYNC_S3_KEY (default "beads/backup.jsonl")
	SyncGitRepo    string        // BEADS_SYNC_GIT_REPO (enables git when set; path to clone)
	SyncGitFile    string        // BEADS_SYNC_GIT_FILE (default "beads.jsonl")
	SyncGitBranch  string        // BEADS_SYNC_GIT_BRANCH (default "main")

	// References settings. Comment text is excluded by default until the
	// open decision (ka-0000000868) lands; flip on per-deployment via
	// BEADS_REFERENCES_EXTRACT_COMMENTS=1. See kbeads/docs/design-bead-references.md
	// § "Open questions" and the migration/rollout note.
	ExtractCommentReferences bool // BEADS_REFERENCES_EXTRACT_COMMENTS (default false)

	// ReconcileTypedMentionDeps, when true, makes replaceBeadReferences
	// DELETE parser-created (created_by='kbeads/references/typed-mention')
	// parent-child dep rows whose `parent:`/`child-of:` typed mention no
	// longer exists in any live source field. Off by default: it is
	// behavior-changing (every text write can now remove a load-bearing edge
	// from text state) and is opt-in per-deployment via
	// BEADS_REFERENCES_RECONCILE_DEPS=1. Manual `kd dep add` rows are never
	// touched. See kbeads/docs/design-reconcile-typed-mention-deps.md
	// (Proposal B, ka-0000006581).
	ReconcileTypedMentionDeps bool // BEADS_REFERENCES_RECONCILE_DEPS (default false)

	// Rate limiting settings. Per-identity token-bucket limits on HTTP
	// mutation and read endpoints (streaming/health are exempt; primary
	// static-token fleet backbone is exempt). Disable by setting
	// BEADS_RATE_LIMIT_ENABLED=false.
	RateLimitEnabled     bool    // BEADS_RATE_LIMIT_ENABLED (default true)
	RateLimitMutateRPS   float64 // BEADS_RATE_LIMIT_MUTATE_RPS (default 20)
	RateLimitMutateBurst int     // BEADS_RATE_LIMIT_MUTATE_BURST (default 40)
	RateLimitReadRPS     float64 // BEADS_RATE_LIMIT_READ_RPS (default 100)
	RateLimitReadBurst   int     // BEADS_RATE_LIMIT_READ_BURST (default 200)
}

func Load() (*Config, error) {
	c := &Config{
		DatabaseURL:          os.Getenv("BEADS_DATABASE_URL"),
		DatabasePasswordFile: os.Getenv("BEADS_DATABASE_PASSWORD_FILE"),
		GRPCAddr:       envOrDefault("BEADS_GRPC_ADDR", ":9090"),
		GRPCReflection: parseBool(os.Getenv("BEADS_GRPC_REFLECTION")),
		HTTPAddr:       envOrDefault("BEADS_HTTP_ADDR", ":8080"),
		MetricsAddr:    envOrDefault("BEADS_METRICS_ADDR", ":9091"),
		NATSURL:        os.Getenv("BEADS_NATS_URL"),
		NATSToken:      os.Getenv("BEADS_NATS_TOKEN"),
		NATSUser:       os.Getenv("BEADS_NATS_USER"),
		RevokedTokens:  parseCommaSeparated(os.Getenv("BEADS_REVOKED_TOKENS")),
		DefaultFleet:         envOrDefault("BEADS_DEFAULT_FLEET", "default"),
		FleetMap:             os.Getenv("BEADS_FLEET_MAP"),
		CrossFleetReadFleets: os.Getenv("BEADS_CROSS_FLEET_READ_FLEETS"),
		PrivilegedFleets:     parseCommaSeparated(os.Getenv("BEADS_PRIVILEGED_FLEETS")),
		MachineClients:       parseCommaSeparated(os.Getenv("BEADS_MACHINE_CLIENTS")),
		ServiceFieldFleets:   parseCommaSeparated(os.Getenv("BEADS_SERVICE_FIELD_FLEETS")),
		BridgeSubjects:       parseCommaSeparated(os.Getenv("BEADS_BRIDGE_SUBJECTS")),
		LandsOperatorGitHubOrgs: parseCommaSeparated(os.Getenv("BEADS_LANDS_OPERATOR_GITHUB_ORGS")),
		OIDCIssuerURL:           os.Getenv("BEADS_OIDC_ISSUER_URL"),
		OIDCDiscoveryURL:        os.Getenv("BEADS_OIDC_DISCOVERY_URL"),
		OIDCCAFile:              strings.TrimSpace(os.Getenv("BEADS_OIDC_CA_FILE")),
		OIDCIssuerURLs:         parseCommaSeparated(os.Getenv("BEADS_OIDC_ISSUER_URLS")),
		OIDCAudience:            os.Getenv("BEADS_OIDC_AUDIENCE"),
		OIDCAllowedEmailDomains: parseCommaSeparated(os.Getenv("BEADS_OIDC_ALLOWED_EMAIL_DOMAINS")),
		EscalationCoordinatorClients: parseCommaSeparated(os.Getenv("BEADS_ESCALATION_COORDINATOR_CLIENTS")),
		EscalationResultMachines:     parseCommaSeparated(os.Getenv("BEADS_ESCALATION_RESULT_MACHINES")),
		ClientRoleCeiling:         parseKeyValuePairs(os.Getenv("BEADS_CLIENT_ROLE_CEILING")),
		KeycloakAdminURL:          os.Getenv("BEADS_KEYCLOAK_ADMIN_URL"),
		KeycloakAdminRealm:        os.Getenv("BEADS_KEYCLOAK_ADMIN_REALM"),
		KeycloakAdminClientID:     os.Getenv("BEADS_KEYCLOAK_ADMIN_CLIENT_ID"),
		KeycloakAdminClientSecret: os.Getenv("BEADS_KEYCLOAK_ADMIN_CLIENT_SECRET"),
		TrustedProxies: parseCommaSeparated(os.Getenv("BEADS_TRUSTED_PROXIES")),
		EmbedBackend:   envOrDefault("BEADS_EMBED_BACKEND", "noop"),
		EmbedURL:       envOrDefault("BEADS_EMBED_URL", "http://localhost:11434"),
		EmbedAPIKey:    os.Getenv("BEADS_EMBED_API_KEY"),
		EmbedModel:     os.Getenv("BEADS_EMBED_MODEL"),
		SyncS3Bucket:   os.Getenv("BEADS_SYNC_S3_BUCKET"),
		SyncS3Endpoint: os.Getenv("BEADS_SYNC_S3_ENDPOINT"),
		SyncS3Region:   envOrDefault("BEADS_SYNC_S3_REGION", "us-east-1"),
		SyncS3Key:      envOrDefault("BEADS_SYNC_S3_KEY", "beads/backup.jsonl"),
		SyncGitRepo:    os.Getenv("BEADS_SYNC_GIT_REPO"),
		SyncGitFile:    envOrDefault("BEADS_SYNC_GIT_FILE", "beads.jsonl"),
		SyncGitBranch:  envOrDefault("BEADS_SYNC_GIT_BRANCH", "main"),
	}
	if c.DatabaseURL == "" {
		return nil, fmt.Errorf("BEADS_DATABASE_URL is required")
	}

	// Fail-closed: when OIDC is enabled, audience is required to prevent
	// accepting tokens from any client ID (SkipClientIDCheck).
	hasOIDCIssuers := c.OIDCIssuerURL != "" || len(c.OIDCIssuerURLs) > 0
	if hasOIDCIssuers && c.OIDCAudience == "" {
		return nil, fmt.Errorf("BEADS_OIDC_AUDIENCE is required when OIDC is enabled (BEADS_OIDC_ISSUER_URL or BEADS_OIDC_ISSUER_URLS is set)")
	}

	retentionStr := envOrDefault("BEADS_AUDIT_RETENTION_DAYS", "90")
	if retentionStr != "" && retentionStr != "0" {
		n, err := strconv.Atoi(retentionStr)
		if err != nil {
			return nil, fmt.Errorf("BEADS_AUDIT_RETENTION_DAYS: %w", err)
		}
		c.AuditRetentionDays = n
	}

	slowRetentionStr := envOrDefault("BEADS_SLOW_TOOL_CALLS_RETENTION_DAYS", "60")
	if slowRetentionStr != "" && slowRetentionStr != "0" {
		n, err := strconv.Atoi(slowRetentionStr)
		if err != nil {
			return nil, fmt.Errorf("BEADS_SLOW_TOOL_CALLS_RETENTION_DAYS: %w", err)
		}
		if n < 0 {
			return nil, fmt.Errorf("BEADS_SLOW_TOOL_CALLS_RETENTION_DAYS: must be >= 0, got %d", n)
		}
		c.SlowToolCallsRetentionDays = n
	}

	historyRetentionStr := envOrDefault("BEADS_HISTORY_RETENTION_DAYS", "14")
	if historyRetentionStr != "" && historyRetentionStr != "0" {
		n, err := strconv.Atoi(historyRetentionStr)
		if err != nil {
			return nil, fmt.Errorf("BEADS_HISTORY_RETENTION_DAYS: %w", err)
		}
		if n < 0 {
			return nil, fmt.Errorf("BEADS_HISTORY_RETENTION_DAYS: must be >= 0, got %d", n)
		}
		c.HistoryRetentionDays = n
	}

	c.ExtractCommentReferences = parseBoolWithDefault(os.Getenv("BEADS_REFERENCES_EXTRACT_COMMENTS"), false)

	c.ReconcileTypedMentionDeps = parseBoolWithDefault(os.Getenv("BEADS_REFERENCES_RECONCILE_DEPS"), false)

	c.ServiceFieldBypassRequireProject = parseBoolWithDefault(os.Getenv("BEADS_SERVICE_FIELD_BYPASS_REQUIRE_PROJECT"), false)

	c.RateLimitEnabled = parseBoolWithDefault(os.Getenv("BEADS_RATE_LIMIT_ENABLED"), true)
	var rlErr error
	if c.RateLimitMutateRPS, rlErr = parseFloatDefault("BEADS_RATE_LIMIT_MUTATE_RPS", 20); rlErr != nil {
		return nil, rlErr
	}
	if c.RateLimitMutateBurst, rlErr = parseIntDefault("BEADS_RATE_LIMIT_MUTATE_BURST", 40); rlErr != nil {
		return nil, rlErr
	}
	if c.RateLimitReadRPS, rlErr = parseFloatDefault("BEADS_RATE_LIMIT_READ_RPS", 100); rlErr != nil {
		return nil, rlErr
	}
	if c.RateLimitReadBurst, rlErr = parseIntDefault("BEADS_RATE_LIMIT_READ_BURST", 200); rlErr != nil {
		return nil, rlErr
	}

	resyncStr := envOrDefault("BEADS_BRIDGE_TOKEN_RESYNC_INTERVAL", "30s")
	resync, err := time.ParseDuration(resyncStr)
	if err != nil {
		return nil, fmt.Errorf("BEADS_BRIDGE_TOKEN_RESYNC_INTERVAL: %w", err)
	}
	if resync < 0 {
		return nil, fmt.Errorf("BEADS_BRIDGE_TOKEN_RESYNC_INTERVAL: must not be negative")
	}
	c.BridgeTokenResyncInterval = resync

	intervalStr := envOrDefault("BEADS_SYNC_INTERVAL", "3m")
	if intervalStr != "" {
		d, err := time.ParseDuration(intervalStr)
		if err != nil {
			return nil, fmt.Errorf("BEADS_SYNC_INTERVAL: %w", err)
		}
		c.SyncInterval = d
	}

	return c, nil
}

func envOrDefault(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

func parseBool(s string) bool {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "1", "true", "yes", "on":
		return true
	}
	return false
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

// parseKeyValuePairs parses "k1=v1,k2=v2" into a map. Entries without "=" or
// with an empty key or value are dropped; nil when nothing parses.
func parseKeyValuePairs(s string) map[string]string {
	var out map[string]string
	for _, part := range parseCommaSeparated(s) {
		k, v, ok := strings.Cut(part, "=")
		k, v = strings.TrimSpace(k), strings.TrimSpace(v)
		if !ok || k == "" || v == "" {
			continue
		}
		if out == nil {
			out = map[string]string{}
		}
		out[k] = v
	}
	return out
}

func parseBoolWithDefault(s string, def bool) bool {
	if s == "" {
		return def
	}
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "1", "true", "yes", "on":
		return true
	case "0", "false", "no", "off":
		return false
	}
	return def
}

func parseFloatDefault(key string, def float64) (float64, error) {
	v := os.Getenv(key)
	if v == "" {
		return def, nil
	}
	f, err := strconv.ParseFloat(v, 64)
	if err != nil {
		return 0, fmt.Errorf("%s: %w", key, err)
	}
	return f, nil
}

func parseIntDefault(key string, def int) (int, error) {
	v := os.Getenv(key)
	if v == "" {
		return def, nil
	}
	n, err := strconv.Atoi(v)
	if err != nil {
		return 0, fmt.Errorf("%s: %w", key, err)
	}
	return n, nil
}
