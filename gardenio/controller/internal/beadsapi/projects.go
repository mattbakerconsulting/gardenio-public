package beadsapi

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
)

// SecretEntry maps a K8s Secret key to a pod environment variable.
// Used in per-project secret overrides on project beads.
type SecretEntry struct {
	Env    string `json:"env"`    // env var name in the pod
	Secret string `json:"secret"` // K8s Secret name
	Key    string `json:"key"`    // key within the Secret
}

// EnvEntry maps a plain environment variable name to a value.
// Used for non-secret configuration on project beads.
type EnvEntry struct {
	Name  string `json:"name"`  // env var name in the pod
	Value string `json:"value"` // plain text value
}

// RepoEntry declares a repository to clone into the agent workspace.
type RepoEntry struct {
	URL    string `json:"url"`
	Branch string `json:"branch,omitempty"`
	Role   string `json:"role,omitempty"` // "primary" or "reference"
	Name   string `json:"name,omitempty"`
}

// ResourceMappingEntry declares one ACME resource tuple that a project
// exercises in a given environment. It is the per-row payload of the
// gardenio-owned `project → {ECS, RDS}` mapping defined by the FSM
// cross-estate correlation contract (gardenio/docs/FSM_CORRELATION_CONTRACT.md
// §2). The controller emits one `gardenio_resource_mapping` presence gauge
// series per (project, env, cluster, service, db_instance) tuple so Grafana
// can pivot from an agent project to the ACME resources it uses. A project
// may declare multiple entries (e.g. several services or DB instances, or the
// same project across envs).
type ResourceMappingEntry struct {
	Env        string `json:"env"`         // gardenio env (join key — must match ACME env, e.g. "prod")
	Cluster    string `json:"cluster"`     // ACME ECS cluster_name
	Service    string `json:"service"`     // ACME ECS service_name
	DBInstance string `json:"db_instance"` // ACME RDS aws_rds_db_instance_identifier
}

// ProjectInfo represents a registered project from daemon project beads.
type ProjectInfo struct {
	Name string // Project name (from bead title)

	// BeadID is the ID of the type=project config bead this ProjectInfo was
	// parsed from. ListProjectBeads previously discarded it; retaining it lets
	// the project hub deep-link to the config bead and the P4 config editor
	// PATCH the right bead (design doc §4, ka-0000008001). Empty only for
	// ProjectInfo values constructed in-memory rather than read from a bead.
	BeadID string

	// Status is the lifecycle status of the config bead (e.g. "open"). Surfaced
	// alongside BeadID so the hub can render a status chip without a second
	// fetch. Empty for in-memory ProjectInfo values.
	Status string

	Prefix          string // Beads prefix (e.g., "kd", "bot")
	GitURL          string // Repository URL
	DefaultBranch   string // Default branch (e.g., "main")
	Image           string // Per-project agent image override
	StorageClass    string // Per-project PVC storage class override
	ServiceAccount  string // Per-project K8s ServiceAccount override
	DockerEnabled   bool   // Enable Docker-in-Docker sidecar for this project
	PostgresEnabled bool   // Enable PostgreSQL sidecar for this project
	NeedsK8sAPI     bool   // Mount ServiceAccount token for this project's pods (default false)

	// OpenZiti per-pod overlay access (epic ka-0000000990). Parsed from the
	// "ziti_*" fields on project beads. ZitiAccessEnabled gates the feature for
	// this project's agents; the rest parameterize the lease request. Default
	// off — a project only gets Ziti wiring when it opts in AND the controller
	// has ZITI_ACCESS_BROKER_URL configured.
	ZitiAccessEnabled bool   // "ziti_access" == "true"
	ZitiServiceID     string // "ziti_service_id" — resolved OpenZiti service id
	ZitiBind          bool   // "ziti_bind" == "true" (default Dial)
	ZitiTTL           string // "ziti_ttl" — requested lease duration, e.g. "15m"
	ZitiOptional      bool   // "ziti_optional" == "true" — failed mint is non-fatal

	// ExecutionTarget selects the runtime backend for this project's agents.
	// "" / "kubernetes" (default) follows the existing pod path; "lambda_microvm"
	// routes to the AWS Lambda MicroVM backend (Network Fabric handoff nfd!15,
	// epic ka-0000005010). Role and per-agent overrides supersede it
	// (precedence: global < project < role < agent). Parsed from the
	// "execution_target" field on project beads.
	ExecutionTarget string

	// RuntimeClassName pins every pod in this project to a Kubernetes
	// RuntimeClass (e.g. "runsc" for gVisor) unless a role or per-agent
	// override supersedes it. Parsed from the "runtime_class" field on project
	// beads. Empty = inherit the cluster default. This is what makes "all
	// agents in the gvisor project ride runsc (→ freeze/thaw on resume)"
	// expressible without tagging every spawn (epic ka-0000007417).
	RuntimeClassName string

	// SessionResume controls whether agents in this project attempt to resume
	// their previous coop session on pod restart. nil = inherit (default true
	// when the pod has a workspace PVC); &true / &false = explicit opt-in/out.
	// Parsed from the "session_resume" field on project beads.
	SessionResume *bool

	// Ultracode opts this project's agents into (or out of) ultracode/workflow
	// mode. nil = unset (leave the pod's resolved claude-settings alone);
	// &true / &false = explicit opt-in/out. Role and per-agent overrides
	// supersede it (precedence: global < project < role < agent). Parsed from
	// the "ultracode" field on project beads. Ultracode is genuinely tri-state,
	// so an unset layer is SKIPPED (not read as off) when resolving (ka-0000001714).
	Ultracode *bool

	// CardTheme selects the Slack card visual system (glyphs, palette, bot
	// avatar) used for agent cards and unfurls rendered for this project's
	// channels. "" / "garden" (default) or "business". Parsed from the
	// "card_theme" field on project beads. See
	// gardenio/plans/slack-greenhouse-restyle.md for the design and
	// bridge.themeFor for resolution (channel -> project -> this field).
	CardTheme string

	// LandDefaultChannel is the type:channel bead id this project's land
	// opens on, normally #general (Lands Commons spec §2.1). "" = unset; a
	// reader falls back to the land's #general by name. Parsed from the
	// "land_default_channel" field; written by gb channel seed.
	LandDefaultChannel string

	// LandSlackMirror says whether new channels in this land default to
	// mirroring to Slack: "on", "off", or "" (unset). Parsed from the
	// "land_slack_mirror" field; written by gb channel seed.
	LandSlackMirror string

	// LandKind is "operator" (today's projects) or "tenant" (Lands Track B
	// spec §3.2). "" = unset, which is an operator land. Parsed from the
	// "land_kind" field. See IsOperatorLand.
	LandKind string

	// Namespace is the Kubernetes namespace a tenant land's pods run in
	// (land-<land>, Lands Track B spec §5.2), written by the provisioner.
	// Parsed from the "namespace" field.
	Namespace string

	// NatsEpoch is a tenant land's NATS password epoch as the bead holds it
	// (natsland.EpochField, gd-zo4W7Q8Yno); bumping it revokes the land's
	// password. Parsed from the "nats_epoch" field.
	NatsEpoch string

	// GithubApp names the GitHub App that mints this project's agent tokens:
	// "fleet" (or "", the default) or "lands" (Lands Track B spec §3.2).
	// Parsed verbatim from the "github_app" field; the controller refuses a
	// value it does not know, and a tenant land with anything but "lands".
	GithubApp string

	// GithubInstallationID is the pinned installation of GithubApp on this
	// project's repo, from "github_installation_id". 0 = unset or not a
	// number; the Lands App requires it.
	GithubInstallationID int64

	// Tier 1 enhancements: per-project pod resource overrides.
	CPURequest    string // Kubernetes quantity string, e.g. "500m"
	CPULimit      string // Kubernetes quantity string, e.g. "2000m"
	MemoryRequest string // Kubernetes quantity string, e.g. "512Mi"
	MemoryLimit   string // Kubernetes quantity string, e.g. "2Gi"

	// SlackChannels is a list of Slack channel IDs associated with this project.
	// Used to infer the default project when /spawn is invoked from a channel.
	// Populated from the comma-separated "slack_channel" field on project beads.
	SlackChannels []string

	// EnvOverrides holds extra env vars parsed from the env_json bead field.
	// Keys absent or empty in the JSON are silently skipped.
	EnvOverrides map[string]string

	// ChannelRoles maps Slack channel IDs to a list of agent role overrides.
	// When /spawn is invoked from a channel listed here, the agent is
	// assigned the first role in the list instead of the default. A channel
	// may map to multiple roles so that OUTBOUND card routing
	// (projectRoleChannel / ChannelForRole) can also send several roles'
	// notifications to the same channel — e.g. #fsm-troubleshooting collects
	// cards from monorepo-repro / -fix / -verify / -publish / fsm-observability.
	//
	// Wire shape (channel_roles JSON on project beads): the VALUE is
	// backwards-compatible — a plain string like "fsm-triage" parses as a
	// single-role list, and a comma-separated string like
	// "monorepo-repro,monorepo-fix" parses as a multi-role list. Existing
	// single-role bead entries keep working unchanged.
	ChannelRoles map[string][]string

	// ChannelModes maps Slack channel IDs to interaction modes.
	// Supported modes: "concierge" (auto-thread with Start/Dismiss buttons),
	// "mention" (default — require @mention to interact).
	// Parsed from the "channel_modes" JSON field on project beads.
	ChannelModes map[string]string

	// ChannelRuntimeClasses maps Slack channel IDs to a Kubernetes RuntimeClass
	// (e.g. "runsc") for thread/DM agents spawned in that channel. When set, a
	// thread agent born in the channel is pinned to that RuntimeClass instead
	// of the cluster default — the channel-scoped way to opt thread agents onto
	// gVisor freeze/thaw (fast hot resume) without widening runsc fleet-wide.
	// Parsed from the "channel_runtime_class" JSON field on project beads.
	// Operator: Matt Baker, Slack thread 1781385375-987669 ("agents responding
	// to threads in gb-turbo channel use runsc but limit it to that"). Epic
	// ka-0000003080.
	ChannelRuntimeClasses map[string]string

	// DMDefault marks this project as the default for DM-spawned agents when
	// no explicit --project flag is given and multiple projects exist.
	// Parsed from the "dm_default" field on project beads.
	DMDefault bool

	// Account references a type:account bead by name. When set, the controller
	// resolves the account bead and configures agent pods with the account's
	// base URL, model, and LiteLLM virtual key for API routing.
	Account string

	// Model overrides the model for this project's agents, independent of which
	// account they use. Applied after applyAccountConfig in the precedence
	// chain global < project < role < agent (role and per-agent overrides
	// supersede it). Empty = inherit the account's default model. Lets one
	// account back multiple models without cloning account beads (epic
	// ka-0000005488). Parsed from the "model" field on project beads.
	Model string

	// Harness selects the agent CLI this project's agents run ("claude" or
	// "codex"), applied below role and agent in the precedence chain. Empty =
	// inherit the controller default (claude). Parsed (unvalidated) from the
	// "harness" field on project beads.
	Harness string

	// DefaultTimezone is the IANA timezone for this project's schedules
	// (e.g. "America/Los_Angeles"). Used as fallback when a schedule bead
	// has no timezone field set.
	DefaultTimezone string

	// SchedulesEnabled controls whether schedules fire for this project.
	// Defaults to true. Set to false to pause all schedules for this project
	// without modifying individual schedule beads.
	SchedulesEnabled bool

	// TriggersEnabled controls whether triggers (type=trigger beads) fire for
	// this project. Defaults to true. Set to false to pause all triggers for
	// this project without modifying individual trigger beads. Used by the
	// emergency `/trigger off <project>` slash command.
	TriggersEnabled bool

	Secrets       []SecretEntry        // Per-project secret overrides
	EnvVars       []EnvEntry           // Per-project plain env vars
	Repos         []RepoEntry          // Multi-repo definitions
	PrewarmedPool *PrewarmedPoolConfig // Per-project prewarmed agent pool config (nil = disabled)

	// ResourceMapping declares the ACME ECS/RDS resources this project
	// exercises, parsed from the "resource_mapping" JSON field on the project
	// bead. It is the gardenio-owned join artifact for cross-estate FSM
	// correlation (see ResourceMappingEntry and
	// gardenio/docs/FSM_CORRELATION_CONTRACT.md §2). Empty when the project
	// declares no mapping.
	ResourceMapping []ResourceMappingEntry

	// ClaudeConfig holds per-category Claude config overrides stored inline
	// on the project bead (field: "claude_config"). Each key is a config
	// category name (e.g. "claude-settings", "claude-hooks") and the value
	// is the raw JSON config for that category. Used by gb setup claude to
	// inject project-level overrides between role and agent specificity.
	ClaudeConfig map[string]json.RawMessage

	// ReleaseSource is the project's release source, parsed from the
	// release_tag_pattern, release_targets and release_branch fields
	// (gardenio/docs/releases-design.md §4). nil when release_tag_pattern is
	// unset: the project has no releases, no truck and no Release page.
	ReleaseSource *ReleaseSource
}

// ReleaseSource says which of a project's tags are releases and where they
// run. TagPattern is a Go regexp (compiled by the daemon on write); Branch is
// the first-parent line the contents are counted on, the project's
// default_branch when release_branch is unset.
type ReleaseSource struct {
	TagPattern string
	Targets    []ReleaseTarget
	Branch     string
}

// ReleaseTarget is one named place a project runs: {"name":"prod","kind":"k3s"}.
type ReleaseTarget struct {
	Name string `json:"name"`
	Kind string `json:"kind"`
}

// parseReleaseSource reads a project's release source from its fields; nil
// when release_tag_pattern is unset.
func parseReleaseSource(project string, fields map[string]string) *ReleaseSource {
	pattern := strings.TrimSpace(fields["release_tag_pattern"])
	if pattern == "" {
		return nil
	}
	src := &ReleaseSource{TagPattern: pattern, Branch: strings.TrimSpace(fields["release_branch"])}
	if src.Branch == "" {
		src.Branch = strings.TrimSpace(fields["default_branch"])
	}
	unmarshalProjectField(project, "release_targets", fields["release_targets"], &src.Targets)
	return src
}

// PrewarmedPoolConfig holds per-project prewarmed agent pool settings.
// Stored as the "prewarmed_pool" JSON field on project beads.
//
// A project may prewarm either a single role (top-level Role/Mode/MinSize/
// MaxSize) or several roles at once via the Roles list. When Roles is
// non-empty it takes precedence and each entry becomes its own (project,role)
// pool, inheriting any unset field from the top-level defaults. See
// EffectivePools for the normalization rules.
type PrewarmedPoolConfig struct {
	Enabled bool   `json:"enabled"`
	MinSize int    `json:"min_size"`
	MaxSize int    `json:"max_size"`
	Role    string `json:"role"`
	Mode    string `json:"mode"`
	// Roles enables prewarming multiple roles for one project. When set, each
	// entry is keyed by (project,role); the top-level Role/Mode/sizes act as
	// defaults for any field an entry leaves unset.
	Roles []PrewarmedRolePool `json:"roles,omitempty"`
}

// PrewarmedRolePool describes a single role-keyed pool within a project's
// prewarmed_pool config. Empty fields inherit the project-level defaults.
type PrewarmedRolePool struct {
	Role    string `json:"role"`
	Mode    string `json:"mode,omitempty"`
	MinSize int    `json:"min_size,omitempty"`
	MaxSize int    `json:"max_size,omitempty"`
}

// EffectivePools expands the pool config into one normalized pool per role.
// When the Roles list is non-empty, each entry yields its own (project,role)
// pool, inheriting any unset field from the top-level defaults. Otherwise the
// single top-level Role/Mode/MinSize/MaxSize describes one pool. A nil or
// disabled config returns nil. Duplicate role keys collapse to the first one.
func (p *PrewarmedPoolConfig) EffectivePools() []PrewarmedRolePool {
	if p == nil || !p.Enabled {
		return nil
	}
	defMin := p.MinSize
	if defMin <= 0 {
		defMin = 2
	}
	defMax := p.MaxSize
	if defMax <= 0 {
		defMax = 5
	}
	defMode := p.Mode
	if defMode == "" {
		defMode = "crew"
	}
	defRole := p.Role
	if defRole == "" {
		defRole = "thread"
	}

	if len(p.Roles) == 0 {
		return []PrewarmedRolePool{{
			Role: defRole, Mode: defMode, MinSize: defMin, MaxSize: defMax,
		}}
	}

	out := make([]PrewarmedRolePool, 0, len(p.Roles))
	seen := make(map[string]bool, len(p.Roles))
	for _, rp := range p.Roles {
		role := rp.Role
		if role == "" {
			role = defRole
		}
		if seen[role] {
			continue
		}
		seen[role] = true
		mode := rp.Mode
		if mode == "" {
			mode = defMode
		}
		minSize := rp.MinSize
		if minSize <= 0 {
			minSize = defMin
		}
		maxSize := rp.MaxSize
		if maxSize <= 0 {
			maxSize = defMax
		}
		out = append(out, PrewarmedRolePool{
			Role: role, Mode: mode, MinSize: minSize, MaxSize: maxSize,
		})
	}
	return out
}

// ConfiguredMinSize returns the operator-configured min_size for the pool that
// serves the given (already-resolved) role, WITHOUT the K8s default fill that
// EffectivePools applies. It is the explicit-opt-in signal for cost-billed
// backends: a lambda_microvm pool whose configured min is 0 has NOT been sized,
// so the pool manager keeps it at 0/off rather than minting the K8s default of
// 2 continuously-billing VMs (feature ka-0000005825, cost model §3c). Idle K8s
// pods are free, so the K8s path ignores this and keeps its EffectivePools
// default.
//
// role must be a role name as produced by EffectivePools (single-role form →
// p.Role or "thread"; multi-role → each entry's role or the top-level default),
// and resolution here mirrors EffectivePools exactly — including the
// first-entry-wins dedup and the multi-role inheritance of the top-level
// min_size when an entry leaves it unset. Returns 0 for a nil/disabled config
// or a role the config does not serve.
func (p *PrewarmedPoolConfig) ConfiguredMinSize(role string) int {
	if p == nil || !p.Enabled {
		return 0
	}
	defRole := p.Role
	if defRole == "" {
		defRole = "thread"
	}
	if len(p.Roles) == 0 {
		if role == defRole {
			return p.MinSize
		}
		return 0
	}
	seen := make(map[string]bool, len(p.Roles))
	for _, rp := range p.Roles {
		r := rp.Role
		if r == "" {
			r = defRole
		}
		if seen[r] {
			continue
		}
		seen[r] = true
		if r != role {
			continue
		}
		if rp.MinSize > 0 {
			return rp.MinSize
		}
		return p.MinSize // inherit top-level raw min (0 when unset)
	}
	return 0
}

// unmarshalProjectField parses a project bead's JSON field into *out, logging a
// structured slog.Warn(project, field, error) on failure instead of silently
// swallowing it. The malformed blob is skipped (out is left at its zero value)
// rather than failing the whole project refresh — but the failure is now
// surfaced loudly in the logs, so a bad blob no longer silently blanks a
// feature (design doc §5.3, ka-0000003239). Returns true iff the field was
// non-empty and parsed cleanly, so callers can gate post-parse handling.
// Empty raw is a no-op and never warns.
func unmarshalProjectField[T any](project, field, raw string, out *T) bool {
	if raw == "" {
		return false
	}
	if err := json.Unmarshal([]byte(raw), out); err != nil {
		slog.Warn("malformed project JSON field (skipped)",
			"project", project, "field", field, "error", err)
		return false
	}
	return true
}

// ListProjectBeads queries the daemon for project beads (type=project) and extracts
// project metadata from fields. Returns a map of project name -> ProjectInfo.
func (c *Client) ListProjectBeads(ctx context.Context) (map[string]ProjectInfo, error) {
	resp, err := c.listBeads(ctx, []string{"project"}, activeStatuses)
	if err != nil {
		return nil, fmt.Errorf("listing project beads: %w", err)
	}

	projects := make(map[string]ProjectInfo)
	// seen maps a resolved project name to the bead ID that currently owns it.
	// Without this, two project beads sharing a name would silently shadow each
	// other and the surviving entry would depend on nondeterministic map
	// iteration order — a sparse duplicate could blank git_url/account for ANY
	// agent in that project from one refresh to the next. We instead pick a
	// deterministic winner (lowest bead ID) and warn loudly so the duplicate
	// gets cleaned up (ka-0000000415).
	seen := make(map[string]string)
	for _, b := range resp.Beads {
		// Strip "Project: " prefix from title -- legacy project beads may have titles
		// like "Project: beads" instead of just "beads".
		name := strings.TrimPrefix(b.Title, "Project: ")
		fields := b.fieldsMap()
		info := ProjectInfo{
			Name:              name,
			BeadID:            b.ID,
			Status:            b.Status,
			Prefix:            fields["prefix"],
			GitURL:            fields["git_url"],
			DefaultBranch:     fields["default_branch"],
			Image:             fields["image"],
			StorageClass:      fields["storage_class"],
			ServiceAccount:    fields["service_account"],
			Model:             fields["model"],
			Harness:           strings.TrimSpace(fields["harness"]),
			RuntimeClassName:  fields["runtime_class"],
			DockerEnabled:     fields["docker"] == "true",
			PostgresEnabled:   fields["postgres"] == "true",
			NeedsK8sAPI:       fields["needs_k8s_api"] == "true",
			ZitiAccessEnabled: fields["ziti_access"] == "true",
			ZitiServiceID:     fields["ziti_service_id"],
			ZitiBind:          fields["ziti_bind"] == "true",
			ZitiTTL:           fields["ziti_ttl"],
			ZitiOptional:      fields["ziti_optional"] == "true",
			ExecutionTarget:   fields["execution_target"],
			SessionResume:     parseTriBool(fields["session_resume"]),
			Ultracode:         parseTriBool(fields["ultracode"]),
			CardTheme:         fields["card_theme"],
			DMDefault:         fields["dm_default"] == "true",
			CPURequest:        fields["cpu_request"],
			CPULimit:          fields["cpu_limit"],
			MemoryRequest:     fields["memory_request"],
			MemoryLimit:       fields["memory_limit"],
			Account:           fields["account"],
			DefaultTimezone:   fields["default_timezone"],
			SchedulesEnabled:  fields["schedules_enabled"] != "false", // default true
			TriggersEnabled:   fields["triggers_enabled"] != "false",  // default true
			SlackChannels:     parseSlackChannels(fields["slack_channel"]),
		}
		// Land fields (Lands Commons spec §2.1).
		info.LandDefaultChannel = fields["land_default_channel"]
		info.LandSlackMirror = fields["land_slack_mirror"]
		info.LandKind = fields["land_kind"]
		info.Namespace = fields["namespace"]
		info.NatsEpoch = fields["nats_epoch"]
		info.GithubApp = strings.TrimSpace(fields["github_app"])
		info.GithubInstallationID, _ = strconv.ParseInt(strings.TrimSpace(fields["github_installation_id"]), 10, 64)
		// Parse per-channel role overrides from JSON field. Values are
		// comma-split so `"C123": "role-a,role-b"` maps that channel to both
		// roles (many-role-per-channel — ka-0000006075). A plain "role-a"
		// stays a single-element list, preserving the pre-2026-07 shape.
		var roles map[string]string
		if unmarshalProjectField(name, "channel_roles", fields["channel_roles"], &roles) {
			info.ChannelRoles = ParseChannelRolesMap(roles)
		}
		// Parse per-channel interaction modes from JSON field.
		var modes map[string]string
		if unmarshalProjectField(name, "channel_modes", fields["channel_modes"], &modes) {
			info.ChannelModes = modes
		}
		// Parse per-channel RuntimeClass overrides from JSON field. Lets a
		// single channel (e.g. gb-turbo) opt its thread agents onto gVisor
		// (runsc) freeze/thaw without widening runsc project- or fleet-wide.
		var rcs map[string]string
		if unmarshalProjectField(name, "channel_runtime_class", fields["channel_runtime_class"], &rcs) {
			info.ChannelRuntimeClasses = rcs
		}
		// Parse per-project secrets from JSON field.
		var secrets []SecretEntry
		if unmarshalProjectField(name, "secrets", fields["secrets"], &secrets) {
			info.Secrets = secrets
		}
		// Parse per-project plain env vars from JSON field.
		var envVars []EnvEntry
		if unmarshalProjectField(name, "env", fields["env"], &envVars) {
			info.EnvVars = envVars
		}
		// Parse multi-repo definitions from JSON field.
		var repos []RepoEntry
		if unmarshalProjectField(name, "repos", fields["repos"], &repos) {
			info.Repos = repos
		}
		// Parse FSM resource-mapping definitions from JSON field. Skip
		// malformed JSON rather than failing the whole project refresh.
		var mapping []ResourceMappingEntry
		if unmarshalProjectField(name, "resource_mapping", fields["resource_mapping"], &mapping) {
			info.ResourceMapping = mapping
		}
		// Parse prewarmed pool config from JSON field. The RAW config is retained
		// verbatim (no min/max/role/mode defaulting here): EffectivePools is the
		// single normalization point that fills the K8s defaults, and
		// ConfiguredMinSize needs the operator's true min_size (0 = unset) to gate
		// the lambda_microvm opt-in — pre-filling min_size=2 here would make an
		// un-sized MicroVM pool look opted-in and silently mint billing VMs
		// (feature ka-0000005825). No caller reads these fields raw; all go through
		// EffectivePools/ConfiguredMinSize.
		var poolCfg PrewarmedPoolConfig
		if unmarshalProjectField(name, "prewarmed_pool", fields["prewarmed_pool"], &poolCfg) && poolCfg.Enabled {
			info.PrewarmedPool = &poolCfg
		}
		// Parse inline Claude config overrides from JSON field.
		var cfg map[string]json.RawMessage
		if unmarshalProjectField(name, "claude_config", fields["claude_config"], &cfg) {
			info.ClaudeConfig = cfg
		}
		// Parse env_json field.
		var envMap map[string]string
		if unmarshalProjectField(name, "env_json", fields["env_json"], &envMap) {
			info.EnvOverrides = envMap
		}
		info.ReleaseSource = parseReleaseSource(name, fields)
		if name == "" {
			continue
		}
		// Collision guard: if another project bead already claimed this name,
		// keep a deterministic winner (lowest bead ID) instead of letting map
		// iteration order decide, and warn so an operator removes the stray
		// duplicate. Deterministic resolution prevents a sparse duplicate from
		// intermittently blanking git_url/account across refreshes.
		if priorID, dup := seen[name]; dup {
			winnerID, loserID := priorID, b.ID
			if b.ID < priorID {
				winnerID, loserID = b.ID, priorID
			}
			slog.Warn("duplicate project-name bead: shadowing avoided, picking deterministic winner",
				"project", name, "winner_bead", winnerID, "shadowed_bead", loserID)
			seen[name] = winnerID
			// Only overwrite the stored info when the new bead is the winner.
			if winnerID == b.ID {
				projects[name] = info
			}
			continue
		}
		seen[name] = b.ID
		projects[name] = info
	}

	return projects, nil
}

// GetProjectBead returns the ProjectInfo for a single project by name, including
// its config-bead ID and status (ProjectInfo.BeadID / ProjectInfo.Status). It is
// a thin convenience wrapper over ListProjectBeads for callers — like the project
// hub and the P4 config editor — that need the config bead behind one project to
// link to it or PATCH it (design doc §4, ka-0000008001). Returns an error if the
// project is not registered. Name matching is exact (the "Project: " title prefix
// is already stripped by ListProjectBeads).
func (c *Client) GetProjectBead(ctx context.Context, name string) (ProjectInfo, error) {
	projects, err := c.ListProjectBeads(ctx)
	if err != nil {
		return ProjectInfo{}, err
	}
	info, ok := projects[name]
	if !ok {
		return ProjectInfo{}, fmt.Errorf("project %q not found", name)
	}
	return info, nil
}

// SetStewardDelegation sets the land's land_steward_approves list through the
// daemon's owner-only endpoint, POST /v1/lands/{land}/steward-delegation
// (escalation-routing D8), on the caller's bearer: the user bearer when the
// context carries one (the ApproveEscalation pattern), so the daemon judges
// the human. An empty ops revokes the delegation.
func (c *Client) SetStewardDelegation(ctx context.Context, land string, ops []string) error {
	path := "/v1/lands/" + url.PathEscape(land) + "/steward-delegation"
	if ops == nil {
		ops = []string{}
	}
	body := map[string]any{"operation_types": ops}
	var err error
	if bearer := UserBearerFromContext(ctx); bearer != "" {
		err = c.doJSONAsUser(ctx, http.MethodPost, path, bearer, body, nil)
	} else {
		err = c.doJSON(ctx, http.MethodPost, path, body, nil)
	}
	if err != nil {
		return fmt.Errorf("setting steward delegation for %s: %w", land, err)
	}
	return nil
}

// LandKindOperator is the land_kind of the operator's own lands.
const LandKindOperator = "operator"

// IsOperatorLand reports whether the project is one of the operator's own
// lands: land_kind unset or "operator". Any other value, "tenant" or one
// nobody defined, is not.
func (p ProjectInfo) IsOperatorLand() bool {
	return p.LandKind == "" || p.LandKind == LandKindOperator
}

// parseSlackChannels splits a comma-separated channel ID string into a slice.
// Handles single values ("C123") and multi-values ("C123,C456, C789").
// Returns nil for empty input.
func parseSlackChannels(raw string) []string {
	if raw == "" {
		return nil
	}
	parts := strings.Split(raw, ",")
	channels := make([]string, 0, len(parts))
	for _, p := range parts {
		p = strings.TrimSpace(p)
		if p != "" {
			channels = append(channels, p)
		}
	}
	if len(channels) == 0 {
		return nil
	}
	return channels
}

// HasChannel reports whether the project is associated with the given Slack channel.
func (p ProjectInfo) HasChannel(channelID string) bool {
	for _, ch := range p.SlackChannels {
		if ch == channelID {
			return true
		}
	}
	return false
}

// ChannelConflicts scans a set of projects and returns a map of channel IDs
// that are claimed by more than one project. The value is the sorted list of
// project names that share that channel. Returns nil when there are no conflicts.
func ChannelConflicts(projects map[string]ProjectInfo) map[string][]string {
	owners := make(map[string][]string) // channelID → []projectName
	for name, info := range projects {
		for _, ch := range info.SlackChannels {
			owners[ch] = append(owners[ch], name)
		}
	}
	var conflicts map[string][]string
	for ch, names := range owners {
		if len(names) > 1 {
			if conflicts == nil {
				conflicts = make(map[string][]string)
			}
			// Sort for deterministic output.
			sort.Strings(names)
			conflicts[ch] = names
		}
	}
	return conflicts
}

// NormalizeGitURL strips trailing slashes and the ".git" suffix and lowercases
// a Git URL so two spellings of the same repo compare equal
// ("https://gitlab.com/org/repo", ".../repo/", ".../repo.git"). It is the
// canonical normalization used by both MR-URL project resolution
// (bridge.projectFromMRURL) and the GitURLConflicts/ReposConflicts detectors,
// so a collision the resolver would have to tie-break is exactly the one the
// detectors flag.
func NormalizeGitURL(u string) string {
	u = strings.TrimRight(u, "/")
	u = strings.TrimSuffix(u, ".git")
	return strings.ToLower(u)
}

// GitURLConflicts scans a set of projects and returns a map of normalized
// git_url -> sorted []projectName for any top-level git_url claimed by more
// than one project. Returns nil when there are no conflicts. A git_url claimed
// by two projects is the latent bug behind MR-mention split-routing
// (ka-0000002164): projectFromMRURL must tie-break it deterministically, and
// this detector surfaces it so an operator can de-dup the project beads.
// Projects with an empty git_url are ignored (a satellite that only holds
// repos[] references legitimately has no git_url).
func GitURLConflicts(projects map[string]ProjectInfo) map[string][]string {
	owners := make(map[string][]string) // normalizedGitURL → []projectName
	for name, info := range projects {
		if info.GitURL == "" {
			continue
		}
		key := NormalizeGitURL(info.GitURL)
		owners[key] = append(owners[key], name)
	}
	return conflictsFromOwners(owners)
}

// ReposConflicts scans a set of projects and returns a map of normalized
// repo URL -> sorted []projectName for any repo listed in the repos[] of more
// than one project. Returns nil when there are no conflicts. devsecops and the
// monorepo shared all five references before the data-plane trim
// (ka-0000002164); this detector keeps a future re-add visible so it never
// silently reintroduces split-routing. A repo listed twice within the same
// project counts once for that project.
func ReposConflicts(projects map[string]ProjectInfo) map[string][]string {
	owners := make(map[string][]string) // normalizedRepoURL → []projectName
	for name, info := range projects {
		seen := make(map[string]bool)
		for _, r := range info.Repos {
			if r.URL == "" {
				continue
			}
			key := NormalizeGitURL(r.URL)
			if seen[key] {
				continue
			}
			seen[key] = true
			owners[key] = append(owners[key], name)
		}
	}
	return conflictsFromOwners(owners)
}

// conflictsFromOwners collapses an owner map (key → []projectName) into the
// subset of keys owned by more than one project, with each name list sorted
// for deterministic output. Returns nil when there are no conflicts. Shared by
// GitURLConflicts and ReposConflicts.
func conflictsFromOwners(owners map[string][]string) map[string][]string {
	var conflicts map[string][]string
	for key, names := range owners {
		if len(names) > 1 {
			if conflicts == nil {
				conflicts = make(map[string][]string)
			}
			sort.Strings(names)
			conflicts[key] = names
		}
	}
	return conflicts
}

// ProjectForChannel resolves a channel ID to a single project name from
// the given project map. If multiple projects claim the same channel it
// returns "" and the list of conflicting project names so the caller can
// report the ambiguity.
func ProjectForChannel(projects map[string]ProjectInfo, channelID string) (project string, conflicts []string) {
	var matched []string
	for name, info := range projects {
		if info.HasChannel(channelID) {
			matched = append(matched, name)
		}
	}
	switch len(matched) {
	case 0:
		return "", nil
	case 1:
		return matched[0], nil
	default:
		sort.Strings(matched)
		return "", matched
	}
}

// ParseChannelRolesMap normalizes the raw JSON-map form of channel_roles
// (value = comma-separated role list or single role) into the in-memory
// []string form. A blank value or channel key is dropped. Roles are trimmed
// of whitespace; empty comma-splits (e.g. "a,,b") are skipped. Order is
// preserved — the first role is what ChannelRole() returns for /spawn-style
// callers that expect a single role per channel.
func ParseChannelRolesMap(raw map[string]string) map[string][]string {
	if len(raw) == 0 {
		return nil
	}
	out := make(map[string][]string, len(raw))
	for ch, v := range raw {
		if ch == "" || v == "" {
			continue
		}
		var roles []string
		for _, part := range strings.Split(v, ",") {
			r := strings.TrimSpace(part)
			if r != "" {
				roles = append(roles, r)
			}
		}
		if len(roles) > 0 {
			out[ch] = roles
		}
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// ChannelRole returns the FIRST role override for the given Slack channel,
// or empty string if none. Callers wanting the full role list (e.g.
// dashboard filtering, outbound-card routing) should read
// p.ChannelRoles[channelID] directly. Kept single-valued because every
// /spawn-style caller needs exactly one role.
func (p ProjectInfo) ChannelRole(channelID string) string {
	roles := p.ChannelRoles[channelID]
	if len(roles) == 0 {
		return ""
	}
	return roles[0]
}

// ChannelRuntimeClass returns the Kubernetes RuntimeClass override for the
// given Slack channel, or empty string if none. A thread/DM agent spawned in a
// channel with a configured RuntimeClass (e.g. "runsc") is pinned to it,
// opting that channel's agents onto gVisor freeze/thaw fast hot resume.
func (p ProjectInfo) ChannelRuntimeClass(channelID string) string {
	return p.ChannelRuntimeClasses[channelID]
}

// ChannelForRole returns the Slack channel ID mapped to the given role, or
// empty string if no mapping exists. This is the reverse of ChannelRole and
// is used to route outbound notifications for role-scoped agents (e.g. JIRA-
// triggered agents with no Slack spawn channel) to the channel dedicated to
// their role. A channel matches when the role appears anywhere in its role
// list (many-role-per-channel — ka-0000006075). If multiple channels claim
// the same role, returns the lexicographically first channel ID for
// deterministic routing.
func (p ProjectInfo) ChannelForRole(role string) string {
	if role == "" || len(p.ChannelRoles) == 0 {
		return ""
	}
	var matches []string
	for ch, roles := range p.ChannelRoles {
		for _, r := range roles {
			if r == role {
				matches = append(matches, ch)
				break
			}
		}
	}
	if len(matches) == 0 {
		return ""
	}
	sort.Strings(matches)
	return matches[0]
}

// ChannelMode returns the interaction mode for the given Slack channel.
// Returns "concierge" for channels configured in concierge mode, or
// "mention" (the default) for unconfigured channels.
func (p ProjectInfo) ChannelMode(channelID string) string {
	if mode := p.ChannelModes[channelID]; mode != "" {
		return mode
	}
	return "mention"
}
