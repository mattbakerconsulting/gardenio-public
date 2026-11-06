package main

import (
	"fmt"
	"log/slog"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"

	"gardenio/controller/internal/beadsapi"
	"gardenio/controller/internal/config"
	"gardenio/controller/internal/lands/landns"
	"gardenio/controller/internal/lands/purse"
	gardeniometrics "gardenio/controller/internal/metrics"
	"gardenio/controller/internal/podmanager"
	"gardenio/controller/internal/subscriber"
)

// warnedSecretPrefix dedupes "skipping secret with invalid prefix" warnings so
// the same misconfigured (kind, owner, secret) tuple does not spam the log on
// every pod build. Process-lifetime cache.
var warnedSecretPrefix sync.Map

// warnedForbiddenSecret dedupes "refusing to mount forbidden secret" warnings
// (same dedupe discipline as warnedSecretPrefix). Process-lifetime cache.
var warnedForbiddenSecret sync.Map

// forbiddenPodSecretMarkers lists K8s Secret name substrings that must NEVER be
// mounted into an agent pod, regardless of project/role prefix. This is the
// STRUCTURAL half of the two-account Jira split (epic ka-0000003475, task
// ka-0000003053): agent pods carry the browse-only gardenio@ Jira identity
// (secret `monorepo-jira-api-token`), while the privileged FSM-writer gardenio2@
// token (`gardenio/jira2-api-token`, mounted ONLY by the jira-bridge deployment)
// is architecturally out of reach.
//
// Why the `{project}-`/`{role}-` prefix guard is not sufficient on its own:
// a future project/role secret named e.g. `monorepo-jira2-api-token` or
// `gardenio-jira2-api-token` would satisfy the prefix check yet carry the
// privileged writer token. This deny-list fails closed on the `jira2` marker so
// no config change — accidental or malicious — can ever inject it into a pod.
//
// IMPORTANT — do NOT add "gardenio2" here: the LLM account named "gardenio2"
// legitimately mounts its Anthropic OAuth token via the K8s Secret
// `gardenio-claude-oauth-gardenio2` as CLAUDE_CODE_OAUTH_TOKEN. That is a
// credential-pool account, unrelated to the privileged Jira identity. The
// unique, unambiguous marker for the privileged Jira token is `jira2`.
//
// "github-app-private-key" is the GitHub App private key (gd-BHLDnjX1Hw): it
// mints installation tokens for EVERY repo the App is installed on, so it is
// readable only by the controller (mounted as a file there). The chart puts it
// in the same namespace the agents run in, so this deny-list is what stops a
// project/role bead's secrets[] entry (e.g. `gardenio-github-app-private-key`,
// which satisfies the {project}- prefix) from projecting it into an agent pod.
//
// "lands-github-app" and "lands-provisioner" are the Lands provisioner's two
// credentials (gd-ej7Iwq8Fuf): `gardenio-lands-github-app` is the Lands App
// private key, which mints tokens for every tenant repo, and
// `gardenio-lands-provisioner` is the Keycloak client secret that manages every
// land's groups. Only the controller reads them. Neither name contains
// "github-app-private-key", and both satisfy the gardenio- prefix, so without
// these markers a project bead could mount them. The chart's gVisor mirror
// guard refuses the same two (PR #735, gd-l1CTMNSH3A). No Secret an agent pod
// legitimately mounts contains either marker; `gardenio-machine-lands-provisioner`
// is a kbeads machine role, never a Secret name.
//
// "minio-credentials" is the in-box MinIO ROOT key pair (gd-urSiLLwPIz): the
// chart-default `<release>-minio-credentials` Secret can read, overwrite and
// delete every bucket and administer MinIO itself. It satisfies the gardenio-
// prefix, so without this marker a project bead could mount it. Agent pods get
// the least-privilege coop user instead (`<release>-minio-agent-credentials`,
// which does not contain the marker), projected via CoopS3CredentialsSecret.
//
// "machine-gardenio-", "keycloak-runtime-admin" and "keycloak-first-admin" are
// the controller's Keycloak credentials (gd-q8nBnOP65d):
// `gardenio-machine-gardenio-<client>` holds each gardenio machine client's
// secret (the controller's own included) and the two keycloak-*-admin Secrets
// are realm admin. All pass the gardenio- prefix. The markers are the floor; the
// class fix is the bead-secret allow-list (cfg.AgentSecretAllowlist), which
// also keeps out controller-only Secrets no marker names.
var forbiddenPodSecretMarkers = []string{
	"jira2", "github-app-private-key", "lands-github-app", "lands-provisioner", "minio-credentials",
	"machine-gardenio-", "keycloak-runtime-admin", "keycloak-first-admin",
}

// isForbiddenPodSecret reports whether a K8s Secret name is on the hard
// deny-list for agent-pod mounts (see forbiddenPodSecretMarkers). Matching is
// case-insensitive substring so name variants (jira2-api-token,
// monorepo-jira2-api-token, gardenio/jira2-api-token) are all caught.
func isForbiddenPodSecret(secretName string) bool {
	lower := strings.ToLower(secretName)
	for _, marker := range forbiddenPodSecretMarkers {
		if strings.Contains(lower, marker) {
			return true
		}
	}
	return false
}

// refuseBeadSecret reports whether a Secret named by a project, role or
// account bead must stay out of the agent pod: it is on the forbidden-marker
// deny-list, or a configured allow-list (cfg.AgentSecretAllowlist) does not
// name it. source is "project", "role" or "account" and owner the bead's name;
// both only label the deduped warning.
//
// A Secret named for a land account (lands-<land>) is that land's own
// credential (landCredentialSecret, gd-ApgYe740ps): only the account of the
// same name may read it, and no allow-list entry is needed for that, since
// the name itself is the check. Every other use is refused.
func refuseBeadSecret(cfg *config.Config, source, owner, secretName, envName string) bool {
	reason := ""
	landOwn := purse.LandOfAccount(secretName) != ""
	switch {
	case isForbiddenPodSecret(secretName):
		reason = "forbidden"
	case landOwn && (source != "account" || owner != secretName):
		reason = "a tenant land's own credential"
	case landOwn:
		return false
	case !cfg.AgentSecretAllowed(secretName):
		reason = "not in AGENT_SECRET_ALLOWLIST"
	default:
		return false
	}
	if _, seen := warnedForbiddenSecret.LoadOrStore(source+"|"+owner+"|"+secretName, struct{}{}); !seen {
		slog.Warn("refusing to mount secret into agent pod",
			"secret", secretName, "env", envName, source, owner, "reason", reason)
	}
	return true
}

// darkForbiddenEnvNames lists the pod environment variable names that must NEVER
// reach a DARK agent pod (AgentPodSpec.Dark). This is the pod-layer half of the
// dark-Jira write-token fence (ka-0000005242): R7 (ka-0000004670) proved the
// "keyless bead is un-syncable" guarantee is NOT structural — the JIRA write
// token can reach a dark pod through the UNGUARDED plaintext-env injection paths
// (project/role EnvVars + EnvOverrides), which — unlike the prefix-gated secrets
// path — apply no name filtering. A dark agent that holds JIRA_API_TOKEN plus an
// open network path to acme.atlassian.net can write to real upstream Jira,
// defeating the entire dark ("parallel universe") isolation model.
//
// JIRA_API_TOKEN is the actual write credential; JIRA_EMAIL / JIRA_BASE_URL are
// harmless without it, so they are intentionally NOT fenced — a dark agent may
// still know WHICH Jira exists and read it (browse-only), it just cannot carry
// the token that authorizes writes. On the monorepo project the token arrives
// via the SECRET path (monorepo-jira-api-token), so the fence also scrubs the
// SecretEnv, not just plaintext — the token must not reach a dark pod by ANY
// injection route.
var darkForbiddenEnvNames = map[string]bool{
	"JIRA_API_TOKEN": true,
}

// isDarkForbiddenEnv reports whether an env var name is fenced out of dark agent
// pods (see darkForbiddenEnvNames). Matching is case-insensitive so a config
// typo in casing cannot slip the write token past the fence.
func isDarkForbiddenEnv(name string) bool {
	return darkForbiddenEnvNames[strings.ToUpper(strings.TrimSpace(name))]
}

// scrubDarkForbiddenEnv is the last-line, injection-site-agnostic enforcement of
// the dark-Jira write-token fence: for a dark pod it removes every fenced env
// var (darkForbiddenEnvNames) from BOTH the plaintext Env map and the SecretEnv
// list, no matter which layer (project plaintext, role plaintext, project/role
// secret, account credential, or a future injection site) tried to set it. A
// non-dark pod is untouched. Warnings are deduped process-wide, mirroring
// scrubForbiddenSecretEnv.
//
// Runs LAST in applyCommonConfig (after every plaintext + secret injection) so
// the guarantee is structural — a dark pod is incapable of carrying the Jira
// write token BY CONSTRUCTION, not by the ordering of injection passes.
func scrubDarkForbiddenEnv(spec *podmanager.AgentPodSpec) {
	if !spec.Dark {
		return
	}
	for name := range spec.Env {
		if isDarkForbiddenEnv(name) {
			delete(spec.Env, name)
			if _, seen := warnedForbiddenSecret.LoadOrStore("dark-env|"+spec.Project+"|"+spec.Role+"|"+name, struct{}{}); !seen {
				slog.Warn("fenced Jira write-token env out of dark agent pod",
					"env", name, "project", spec.Project, "role", spec.Role, "agent", spec.AgentName)
			}
		}
	}
	if len(spec.SecretEnv) > 0 {
		kept := spec.SecretEnv[:0]
		for _, e := range spec.SecretEnv {
			if isDarkForbiddenEnv(e.EnvName) {
				if _, seen := warnedForbiddenSecret.LoadOrStore("dark-secret|"+spec.Project+"|"+spec.Role+"|"+e.EnvName, struct{}{}); !seen {
					slog.Warn("fenced Jira write-token secret out of dark agent pod",
						"env", e.EnvName, "secret", e.SecretName,
						"project", spec.Project, "role", spec.Role, "agent", spec.AgentName)
				}
				continue
			}
			kept = append(kept, e)
		}
		spec.SecretEnv = kept
	}
}

// parseTriBoolMeta parses a "true"/"false" agent metadata value into a *bool.
// Empty or unrecognized values return nil so the caller falls through to the
// project/role default. Use for fields that need to distinguish "unset" from
// explicit false.
func parseTriBoolMeta(raw string) *bool {
	switch raw {
	case "true":
		v := true
		return &v
	case "false":
		v := false
		return &v
	}
	return nil
}

// operatedByFromMetadata derives the BEADS_OPERATED_BY value to inject into
// the agent pod from the bead's metadata. Precedence:
//  1. explicit "operated_by" (already-prefixed opaque id)
//  2. "slack_user_id" → "slack:<id>"
//  3. "" (scheduler-driven agent; no human operator)
//
// The value flows into spec.OperatedBy → BEADS_OPERATED_BY env var → kd/gb
// SetOperatedBy → X-Operated-By header on outbound kbeads calls.
func operatedByFromMetadata(meta map[string]string) string {
	if op := strings.TrimSpace(meta["operated_by"]); op != "" {
		// Skip sentinel-class operators (system:*, fleet:*). These would be
		// rejected by the server's sanitizeOperatedByHeader (ka-0000002601)
		// and refused as header_non_human (ka-0000005841). Fall through to
		// slack_user_id / empty so the server-side agent-bead fallback or
		// spawn-chain walk can attribute writes to a real human ancestor.
		if !strings.HasPrefix(op, "system:") && !strings.HasPrefix(op, "fleet:") {
			return op
		}
	}
	if u := strings.TrimSpace(meta["slack_user_id"]); u != "" {
		return "slack:" + u
	}
	return ""
}

// userBeadIDFromMetadata extracts the user_bead_id from agent bead metadata,
// to be injected as BEADS_USER_BEAD_ID in the pod env. Empty string when no
// user bead is associated (scheduler/gardener-spawned agents).
func userBeadIDFromMetadata(meta map[string]string) string {
	return strings.TrimSpace(meta["user_bead_id"])
}

// modeForRole returns the canonical mode for a role.
// If mode is already set, it is returned unchanged.
// For comma-separated roles (e.g. "thread,crew"), the first role determines mode.
func modeForRole(mode, roles string) string {
	if mode != "" {
		return mode
	}
	role := roles
	if i := strings.Index(roles, ","); i >= 0 {
		role = strings.TrimSpace(roles[:i])
	}
	switch role {
	case "gardener", "crew":
		return "crew"
	case "job", "polecat":
		return "job"
	default:
		return "crew"
	}
}

// BuildSpecFromBeadInfo constructs an AgentPodSpec from config and bead identity,
// Used by the reconciler to produce specs identical to those created by
// handleEvent, using controller config for all metadata.
func BuildSpecFromBeadInfo(cfg *config.Config, project, mode, role, agentName string, metadata map[string]string) podmanager.AgentPodSpec {
	mode = modeForRole(mode, role)
	image := cfg.CoopImage
	if img := metadata["image"]; img != "" {
		image = img
	}
	// An explicit namespace pin in metadata wins over cfg.Namespace, matching
	// buildAgentPodSpec's namespaceFromEvent. (routeGVisorNamespace below also
	// keys off this pin, so the two builders agree on precedence.)
	ns := cfg.Namespace
	if pinned := metadata["namespace"]; pinned != "" {
		ns = pinned
	}
	spec := podmanager.AgentPodSpec{
		Project:       project,
		Mode:          mode,
		Role:          role,
		AgentName:     agentName,
		TaskID:        metadata["task_id"],
		TriggerBeadID: metadata["trigger_bead_id"],
		Image:         image,
		Namespace:     ns,
		// Dark-pod marker (ka-0000005242): plumbed via metadata so both spec
		// builders resolve it identically. Set BEFORE applyCommonConfig so its
		// dark-Jira write-token scrub sees the flag. beadsapi.ListAgentBeads
		// stamps the metadata key on the reconciler path.
		Dark:       metadata[beadsapi.MetaDarkPod] == "true",
		OperatedBy: operatedByFromMetadata(metadata),
		UserBeadID: userBeadIDFromMetadata(metadata),
		// Spawn-escalation provenance (gd-h7fgTgcuyn, design doc
		// gd-sGW5Y1B8sg §5) — empty on an ordinary, non-escalated spawn.
		SpawnedBy:         metadata["spawned_by"],
		SpawnApprovalMode: metadata["spawn_approval_mode"],
		SpawnApprovedBy:   metadata["spawn_approved_by"],
		SpawnSource:       metadata["spawn_source"],
		WakeReason:        metadata[beadsapi.FieldWakeReason],
		// Capture the agent-level model request up front so the fable-aware landing
		// gate (selectAccount, run inside applyCommonConfig below) can key on
		// the group the agent will actually use (ka-0000003806). The override is
		// re-applied authoritatively later by applyAgentModelOverride.
		RequestedModel:   metadata["model"],
		RequestedHarness: metadata["harness"],
		Env: map[string]string{
			"BEADS_GRPC_ADDR": cfg.BeadsGRPCAddr,
			"BEADS_HTTP_ADDR": cfg.BeadsHTTPAddr,
		},
	}

	defaults := podmanager.DefaultPodDefaults(mode, role)
	podmanager.ApplyDefaults(&spec, defaults)

	// Apply project-level overrides from project bead metadata.
	// Hold read lock on ProjectCache to prevent concurrent map read/write
	// with refreshProjectCache running in the periodic sync goroutine.
	cfg.ProjectCacheMu.RLock()
	applyProjectDefaults(cfg, &spec)
	cfg.ProjectCacheMu.RUnlock()

	// Apply role-level overrides (between project defaults and common config).
	cfg.RoleCacheMu.RLock()
	applyRoleDefaults(cfg, &spec)
	cfg.RoleCacheMu.RUnlock()

	// Apply controller-level common config.
	cfg.ProjectCacheMu.RLock()
	applyCommonConfig(cfg, &spec)
	cfg.ProjectCacheMu.RUnlock()

	// Docker-in-Docker sidecar: agent-level override via metadata.
	// When "docker" metadata is "true", inject the DinD sidecar.
	// When "false", explicitly disable even if project defaults enable it.
	if metadata["docker"] == "true" {
		spec.Docker = true
	} else if metadata["docker"] == "false" {
		spec.Docker = false
	}

	// PostgreSQL sidecar: agent-level override via metadata.
	if metadata["postgres"] == "true" {
		spec.Postgres = true
	} else if metadata["postgres"] == "false" {
		spec.Postgres = false
	}

	// K8s identity (ServiceAccount, API token): agent metadata may narrow but
	// never select or enable. See applyAgentK8sIdentity.
	applyAgentK8sIdentity(&spec, metadata, agentName)

	// Session resume: agent-level override (highest precedence after role/project).
	if v := parseTriBoolMeta(metadata["session_resume"]); v != nil {
		spec.SessionResume = v
	}

	// Model: agent-level override (highest precedence — beats account default and
	// any project/role model override applied in applyCommonConfig). Mirrors the
	// events path in buildAgentPodSpec so the reconciler never reverts a
	// per-agent model on restart. epic ka-0000005488.
	applyAgentModelOverride(&spec, metadata["model"])

	// Opus fallback (ka-0000003806, decision ka-0000003299): if the fable-aware
	// landing gate downgraded to opus because the requested group's pool was
	// exhausted, force the model LAST so it overrides even the agent-level pin
	// above — the pod must run the model the account it actually landed on can serve.
	applyModelDowngrade(&spec)

	// Harness env shaping (codex): needs the final model, so it runs after every
	// model layer. No-op for claude.
	shapeHarnessEnv(&spec)

	// Ultracode: resolve across project < role < agent (ka-0000001714). Mirrors
	// the model override in both builders so the reconciler never drops
	// BOAT_ULTRACODE on restart. The agent-metadata layer is the highest-
	// precedence input, so the per-spawn --ultracode flag still wins as before.
	applyUltracode(cfg, &spec, metadata)

	// gVisor freeze/thaw production PILOT class selection — see
	// applyPilotFreezeClass. Runs AFTER project/role RuntimeClass resolution so it
	// force-pins the one opt-in class to runsc, but BEFORE the per-agent
	// runtime_class override below so an explicit per-agent pin still wins
	// (project < role < pilot < agent). Shared by both spec builders.
	applyPilotFreezeClass(cfg, &spec)

	// RuntimeClass: agent-level override (highest precedence after role/project).
	// Pins the pod to a K8s RuntimeClass (e.g. "runsc" for gVisor). Empty value
	// in metadata leaves whatever role/project resolved.
	if rc := metadata["runtime_class"]; rc != "" {
		spec.RuntimeClassName = rc
	}

	// OpenZiti per-pod overlay access (epic ka-0000000990): resolve after the
	// agent-metadata overlays so project < role < agent precedence is complete.
	applyZitiAccess(cfg, &spec, metadata)

	// Execution backend selection (epic ka-0000005010): resolve after the
	// agent-metadata overlays so project < role < agent precedence is complete.
	// Only records the resolved target on the spec; the spawn doors route on it.
	spec.ExecutionTarget = resolveExecutionTarget(cfg, &spec, metadata)
	refuseHarnessOnTarget(&spec)

	// Mock mode: override BOAT_COMMAND to run claudeless with a scenario file.
	if scenario := metadata["mock_scenario"]; scenario != "" {
		spec.Env["BOAT_COMMAND"] = fmt.Sprintf("claudeless --scenario /scenarios/%s.toml --dangerously-skip-permissions", scenario)
	}

	// Custom prompt: pass through to entrypoint for initial nudge message.
	if prompt := metadata["prompt"]; prompt != "" {
		spec.Env["BOAT_PROMPT"] = prompt
	}

	// Wake reason: surface WHY a parked persistent identity was revived
	// (timer|manual|mention|assignment|decision) so the recreated pod's agent can
	// read it for trigger diagnostics (persistent-agents Phase 3, ka-0000000027).
	// The wake path stamps it on the agent bead; ListAgentBeads copies every bead
	// field into metadata, so it arrives here transparently on the reconciler
	// recreate path that revives the pod under the same name.
	if wr := metadata[beadsapi.FieldWakeReason]; wr != "" {
		spec.Env["BOAT_WAKE_REASON"] = wr
	}

	// Slack thread metadata.
	if ch := metadata["slack_thread_channel"]; ch != "" {
		spec.Env["SLACK_THREAD_CHANNEL"] = ch
	}
	if ts := metadata["slack_thread_ts"]; ts != "" {
		spec.Env["SLACK_THREAD_TS"] = ts
	}

	// JIRA ticket binding: inject the bound ticket key so `gb jira reply`
	// can target the originating ticket via the jira-bridge (F2 ka-0000000065).
	if key := metadata["jira_key"]; key != "" {
		spec.Env["JIRA_KEY"] = key
	}

	// Prewarmed agents: block in entrypoint standby mode (BOAT_STANDBY=true)
	// until the pool manager assigns work by transitioning agent_state from
	// "prewarmed" to "assigning". The entrypoint polls agent_state and starts
	// Claude only after assignment, preventing stampede from self-selection.
	if metadata["agent_state"] == "prewarmed" {
		spec.Env["BOAT_AGENT_STATE"] = "prewarmed"
		spec.Env["BOAT_STANDBY"] = "true"
		// Pod-side standby poll interval: how fast the parked pod discovers the
		// prewarmed→assigning flip. Drives user-perceived warm-spawn latency
		// (ka-0000003812). Default 500ms keeps end-to-end warm spawn sub-second.
		if cfg.StandbyPoll > 0 {
			spec.Env["BOAT_STANDBY_POLL"] = cfg.StandbyPoll.String()
		}
		spec.Prewarmed = true
	}

	// gVisor namespace routing — see routeGVisorNamespace. Done last, after
	// role/project/agent runtime_class resolution. An explicitly pinned
	// namespace still wins. This is the reconciler path; omitting this call is
	// exactly the regression that wedged runsc agents in the baseline namespace
	// (ka-0000003657).
	routeGVisorNamespace(cfg, &spec, metadata["namespace"])

	// Lands fence — see applyLandFence. After every namespace, runtime class
	// and service account choice above, so a tenant land's pods always end
	// in the land's namespace under the land RuntimeClass whatever a role or
	// agent pin says.
	applyLandFence(cfg, &spec)

	// Freeze-eligibility signal — see markFreezeEligible. Must follow the final
	// runtime_class resolution (and routeGVisorNamespace) so it sees the
	// resolved RuntimeClassName.
	markFreezeEligible(cfg, &spec)

	// Fat-node soft affinity for freeze-eligible agents — see
	// preferFatNodeForFreeze. Appends to whatever affinity the mode/project/role
	// already set, so it must run after all affinity is resolved (i.e. here, last).
	preferFatNodeForFreeze(cfg, &spec)

	warnK8sAPIOnSharedServiceAccount(cfg, &spec)

	return spec
}

// buildAgentPodSpec constructs a full AgentPodSpec from an event and config.
// It applies role-specific defaults, then overlays event metadata.
func buildAgentPodSpec(cfg *config.Config, event subscriber.Event) podmanager.AgentPodSpec {
	ns := namespaceFromEvent(event, cfg.Namespace)
	mode := modeForRole(event.Mode, event.Role)

	// Fall back to the controller's configured agent image when the event
	// carries none, matching BuildSpecFromBeadInfo. Synthetic events do not
	// populate metadata["image"] — the stuck publisher sends only
	// {reason, source} — so without this the spec's Image depends entirely on
	// a warm ProjectCache/role cache. When that cache is cold the API rejects
	// the pod with "spec.containers[0].image: Required value" and, because the
	// AgentStuck handler deletes the old pod first, the agent is killed with
	// no replacement (ka-0000000084, recurrence of ka-0000005268).
	image := cfg.CoopImage
	if img := event.Metadata["image"]; img != "" {
		image = img
	}

	spec := podmanager.AgentPodSpec{
		Project:       event.Project,
		Mode:          mode,
		Role:          event.Role,
		AgentName:     event.AgentName,
		BeadID:        event.BeadID,
		TaskID:        event.Metadata["task_id"],
		TriggerBeadID: event.Metadata["trigger_bead_id"],
		Image:         image,
		Namespace:     ns,
		// Dark-pod marker (ka-0000005242): stamped by subscriber.buildEvent from
		// the bead's dark markers. Set BEFORE applyCommonConfig so its dark-Jira
		// write-token scrub sees the flag.
		Dark:       event.Metadata[beadsapi.MetaDarkPod] == "true",
		OperatedBy: operatedByFromMetadata(event.Metadata),
		UserBeadID: userBeadIDFromMetadata(event.Metadata),
		// Spawn-escalation provenance (gd-h7fgTgcuyn, design doc
		// gd-sGW5Y1B8sg §5) — empty on an ordinary, non-escalated spawn.
		SpawnedBy:         event.Metadata["spawned_by"],
		SpawnApprovalMode: event.Metadata["spawn_approval_mode"],
		SpawnApprovedBy:   event.Metadata["spawn_approved_by"],
		SpawnSource:       event.Metadata["spawn_source"],
		WakeReason:        event.Metadata[beadsapi.FieldWakeReason],
		// Capture the agent-level model request up front for the fable-aware landing
		// gate (ka-0000003806); applyAgentModelOverride re-applies it authoritatively
		// after spread.
		RequestedModel:   event.Metadata["model"],
		RequestedHarness: event.Metadata["harness"],
		Env: map[string]string{
			"BEADS_GRPC_ADDR": metadataOr(event, "beads_grpc_addr", cfg.BeadsGRPCAddr),
			"BEADS_HTTP_ADDR": cfg.BeadsHTTPAddr,
		},
	}

	// Apply mode-specific defaults (workspace storage, resources).
	defaults := podmanager.DefaultPodDefaults(mode, event.Role)
	podmanager.ApplyDefaults(&spec, defaults)

	// Apply project-level overrides under read lock.
	cfg.ProjectCacheMu.RLock()
	applyProjectDefaults(cfg, &spec)
	cfg.ProjectCacheMu.RUnlock()

	// Apply role-level overrides (between project defaults and common config).
	cfg.RoleCacheMu.RLock()
	applyRoleDefaults(cfg, &spec)
	cfg.RoleCacheMu.RUnlock()

	// Apply controller-level common config under read lock.
	cfg.ProjectCacheMu.RLock()
	applyCommonConfig(cfg, &spec)
	cfg.ProjectCacheMu.RUnlock()

	// Overlay event metadata for optional fields.
	if cm := event.Metadata["configmap"]; cm != "" {
		spec.ConfigMapName = cm
	}

	// Docker-in-Docker sidecar: enable via agent bead metadata.
	if event.Metadata["docker"] == "true" {
		spec.Docker = true
	} else if event.Metadata["docker"] == "false" {
		spec.Docker = false
	}

	// PostgreSQL sidecar: enable via agent bead metadata.
	if event.Metadata["postgres"] == "true" {
		spec.Postgres = true
	} else if event.Metadata["postgres"] == "false" {
		spec.Postgres = false
	}

	// K8s identity (ServiceAccount, API token): agent metadata may narrow but
	// never select or enable. See applyAgentK8sIdentity.
	applyAgentK8sIdentity(&spec, event.Metadata, event.AgentName)

	// Session resume: agent-level override (highest precedence after role/project).
	if v := parseTriBoolMeta(event.Metadata["session_resume"]); v != nil {
		spec.SessionResume = v
	}

	// Model: agent-level override (highest precedence — beats account default and
	// any project/role model override applied in applyCommonConfig). epic
	// ka-0000005488.
	applyAgentModelOverride(&spec, event.Metadata["model"])

	// Opus fallback (ka-0000003806, decision ka-0000003299): force the downgraded
	// model LAST when the fable-aware landing gate degraded to opus because the
	// requested group's pool was exhausted. Mirrors BuildSpecFromBeadInfo so the
	// reconciler restart path applies the same fallback.
	applyModelDowngrade(&spec)

	// Harness env shaping (codex): needs the final model, so it runs after every
	// model layer. No-op for claude.
	shapeHarnessEnv(&spec)

	// Ultracode: resolve across project < role < agent (ka-0000001714). Mirrors
	// the model override in both builders so the reconciler never drops
	// BOAT_ULTRACODE on restart. The agent-metadata layer is the highest-
	// precedence input, so the per-spawn --ultracode flag still wins as before.
	applyUltracode(cfg, &spec, event.Metadata)

	// gVisor freeze/thaw production PILOT class selection — see
	// applyPilotFreezeClass. Positioned identically to the BuildSpecFromBeadInfo
	// path (after project/role RuntimeClass resolution, before the per-agent
	// runtime_class override) so both builders resolve the pilot pin the same way.
	applyPilotFreezeClass(cfg, &spec)

	// RuntimeClass: agent-level override (highest precedence after role/project).
	// Pins the pod to a K8s RuntimeClass (e.g. "runsc" for gVisor). Empty value
	// in metadata leaves whatever role/project resolved. Mirrors the reconciler
	// path in BuildSpecFromBeadInfo — positioned identically (after the model +
	// ultracode overlays, BEFORE routeGVisorNamespace/markFreezeEligible/
	// preferFatNodeForFreeze) so the two builders resolve an agent-level pin
	// identically and the gVisor namespace routing + freeze-eligibility marking
	// key off the same resolved RuntimeClassName on both paths. Without this the
	// events/SSE spawn path silently dropped a channel_runtime_class=runsc pin —
	// runsc thread agents ran runc in gardenio-system and were never freeze-
	// eligible (ka-0000005431; same drift class as ka-0000003657 namespace
	// routing and ka-0000001990 service_account).
	if rc := event.Metadata["runtime_class"]; rc != "" {
		spec.RuntimeClassName = rc
	}

	// OpenZiti per-pod overlay access (epic ka-0000000990): resolve after the
	// agent-metadata overlays so project < role < agent precedence is complete.
	applyZitiAccess(cfg, &spec, event.Metadata)

	// Execution backend selection (epic ka-0000005010): resolve after the
	// agent-metadata overlays so project < role < agent precedence is complete.
	// Only records the resolved target on the spec; the spawn doors route on it.
	spec.ExecutionTarget = resolveExecutionTarget(cfg, &spec, event.Metadata)
	refuseHarnessOnTarget(&spec)

	// Mock mode: override BOAT_COMMAND to run claudeless with a scenario file.
	if scenario := event.Metadata["mock_scenario"]; scenario != "" {
		spec.Env["BOAT_COMMAND"] = fmt.Sprintf("claudeless --scenario /scenarios/%s.toml --dangerously-skip-permissions", scenario)
	}

	// Custom prompt: pass through to entrypoint for initial nudge message.
	if prompt := event.Metadata["prompt"]; prompt != "" {
		spec.Env["BOAT_PROMPT"] = prompt
	}

	// Wake reason: surface WHY a parked persistent identity was revived
	// (timer|manual|mention|assignment|decision) so the agent can read it for
	// trigger diagnostics (persistent-agents Phase 3, ka-0000000027). Mirrors the
	// reconciler path in BuildSpecFromBeadInfo so a wake surfaced through either
	// spec builder reaches the pod identically.
	if wr := event.Metadata[beadsapi.FieldWakeReason]; wr != "" {
		spec.Env["BOAT_WAKE_REASON"] = wr
	}

	// Slack thread metadata: inject thread coordinates so agents spawned
	// from a Slack thread can read/reply via the slack-bridge HTTP API.
	if ch := event.Metadata["slack_thread_channel"]; ch != "" {
		spec.Env["SLACK_THREAD_CHANNEL"] = ch
	}
	if ts := event.Metadata["slack_thread_ts"]; ts != "" {
		spec.Env["SLACK_THREAD_TS"] = ts
	}

	// JIRA ticket binding: inject the bound ticket key so `gb jira reply`
	// can target the originating ticket via the jira-bridge (F2 ka-0000000065).
	if key := event.Metadata["jira_key"]; key != "" {
		spec.Env["JIRA_KEY"] = key
	}

	// Prewarmed agents: block in entrypoint standby mode (BOAT_STANDBY=true)
	// until the pool manager assigns work by transitioning agent_state from
	// "prewarmed" to "assigning". The entrypoint polls agent_state and starts
	// Claude only after assignment, preventing stampede from self-selection.
	if event.Metadata["agent_state"] == "prewarmed" {
		spec.Env["BOAT_AGENT_STATE"] = "prewarmed"
		spec.Env["BOAT_STANDBY"] = "true"
		// Pod-side standby poll interval: how fast the parked pod discovers the
		// prewarmed→assigning flip. Drives user-perceived warm-spawn latency
		// (ka-0000003812). Default 500ms keeps end-to-end warm spawn sub-second.
		if cfg.StandbyPoll > 0 {
			spec.Env["BOAT_STANDBY_POLL"] = cfg.StandbyPoll.String()
		}
		spec.Prewarmed = true
	}

	// gVisor namespace routing — see routeGVisorNamespace. Done last, after
	// role/project/agent runtime_class resolution. Event metadata that
	// explicitly pins a namespace still wins.
	routeGVisorNamespace(cfg, &spec, event.Metadata["namespace"])

	// Lands fence — see applyLandFence. After every namespace, runtime class
	// and service account choice above, so a tenant land's pods always end
	// in the land's namespace under the land RuntimeClass whatever a role or
	// agent pin says.
	applyLandFence(cfg, &spec)

	// Freeze-eligibility signal — see markFreezeEligible. Must follow the final
	// runtime_class resolution (and routeGVisorNamespace) so it sees the
	// resolved RuntimeClassName.
	markFreezeEligible(cfg, &spec)

	// Fat-node soft affinity for freeze-eligible agents — see
	// preferFatNodeForFreeze. Appends to whatever affinity the mode/project/role
	// already set, so it must run after all affinity is resolved (i.e. here, last).
	preferFatNodeForFreeze(cfg, &spec)

	warnK8sAPIOnSharedServiceAccount(cfg, &spec)

	return spec
}

// sharedAgentServiceAccount is the ServiceAccount the whole fleet runs as unless
// a project/role/agent pins another. It must never carry a mounted API token:
// any RBAC bound to it is held by every agent pod (gd-KRHGbBJqRN).
const sharedAgentServiceAccount = "gardenio-agent"

// k8sAPIOnSharedServiceAccount reports whether a pod that opts into a mounted
// ServiceAccount token (needs_k8s_api) would run as the fleet-shared SA — either
// because no service_account resolved (the pod falls back to the default SA) or
// because one was pinned to gardenio-agent / the controller's default SA. A
// needs_k8s_api role must carry its own dedicated SA (e.g. gc-k8s-live for
// k8s-live) so its blast radius is a property of the role bead, not of whatever
// bindings accumulate on the shared SA.
func k8sAPIOnSharedServiceAccount(cfg *config.Config, spec *podmanager.AgentPodSpec) bool {
	if !spec.NeedsK8sAPI {
		return false
	}
	sa := spec.ServiceAccountName
	return sa == "" || sa == sharedAgentServiceAccount || (cfg != nil && sa == cfg.CoopServiceAccount)
}

// warnK8sAPIOnSharedServiceAccount logs (never refuses) a needs_k8s_api pod that
// resolved to the shared SA. Warn-only: refusing here would wedge reconciler
// recreation of live pods; the hard refusal is the daemon-side write check
// (kbeads rolePrivilegeError), which stops new offenders at bead-write time.
func warnK8sAPIOnSharedServiceAccount(cfg *config.Config, spec *podmanager.AgentPodSpec) {
	if !k8sAPIOnSharedServiceAccount(cfg, spec) {
		return
	}
	slog.Warn("needs_k8s_api pod resolved to the shared ServiceAccount; set a dedicated service_account on the role",
		"project", spec.Project, "role", spec.Role, "agent", spec.AgentName,
		"service_account", spec.ServiceAccountName)
}

// applyPilotFreezeClass force-pins the SINGLE opt-in gVisor freeze/thaw pilot
// class onto the runsc RuntimeClass. cfg.PilotFreezeClass names exactly one agent
// role (the chosen class is "plan-gardener" — a long-lived, HITL-heavy gardener
// that cold-boots every cron fire and cold-replays after a `gb decision` yield,
// so it is the class that most benefits from retained working memory). When that
// role matches AND the freeze/thaw lifecycle is enabled controller-wide
// (FreezeThawEnabled), the pod is pinned to runsc — which lands it on the
// dedicated gVisor node pool, routes it into GVisorNamespace (routeGVisorNamespace),
// and marks it freeze-eligible (markFreezeEligible, via FreezeRoleAllowed which
// always permits the pilot class). This is the single feature flag that scopes
// the production pilot's blast radius to one class: every other role is untouched
// and stays on the default runc path.
//
// Gated on FreezeThawEnabled because pinning a class to runsc is only meaningful
// when the freeze machinery is live — otherwise the class would run gVisor-
// sandboxed with a cold start and no node-pool guarantee. So clearing EITHER
// FreezeThawEnabled or PilotFreezeClass fully reverts the pilot (two independent
// rollback switches).
//
// Precedence: called after project/role RuntimeClass resolution (applyCommonConfig)
// so it overrides them for the pilot class, but before the per-agent
// runtime_class metadata override, so an explicit per-agent pin still wins
// (project < role < pilot < agent). ROLLBACK: clear PILOT_FREEZE_CLASS and the
// class reverts to whatever project/role resolved (runc) + today's stateless
// cold-start lifecycle on its next spawn; nothing is stranded.
func applyPilotFreezeClass(cfg *config.Config, spec *podmanager.AgentPodSpec) {
	if cfg.FreezeThawEnabled && cfg.IsPilotFreezeClass(spec.Role) {
		spec.RuntimeClassName = podmanager.RuntimeClassRunsc
	}
}

// routeGVisorNamespace routes a runsc (gVisor) freeze/thaw agent into the
// dedicated PSA-privileged namespace (bug ka-0000004301, epic ka-0000008224;
// reconciler-path regression ka-0000003657). A runsc agent pod carries a
// thaw-state hostPath that PodSecurity `baseline` (enforced on the main
// namespace) forbids, so when cfg.GVisorNamespace is set we move runsc agents
// into that namespace. Must be called LAST, after role/project/agent
// runtime_class resolution, so the decision sees the final RuntimeClassName.
// Only runsc agents move; everything else stays put. An explicitly pinned
// namespace (explicitNamespace != "") still wins.
//
// Shared by BOTH spec builders — buildAgentPodSpec (events path) AND
// BuildSpecFromBeadInfo (reconciler path) — so the two never drift. The
// reconciler path originally lacked this routing and wedged runsc agents in the
// PSA=baseline namespace ("violates PodSecurity baseline:latest: hostPath
// volumes (volume thaw-state)"); ka-0000003657.
func routeGVisorNamespace(cfg *config.Config, spec *podmanager.AgentPodSpec, explicitNamespace string) {
	if cfg.GVisorNamespace != "" &&
		spec.RuntimeClassName == podmanager.RuntimeClassRunsc &&
		explicitNamespace == "" {
		spec.Namespace = cfg.GVisorNamespace
		// Cross-namespace DNS (bug ka-0000001058): the controller injects BARE
		// service names (gardenio-coopmux, gardenio-nats) that only resolve in the
		// release namespace's search domain. A pod in cfg.GVisorNamespace gets
		// "<gvisor-ns>.svc.cluster.local …" searches and cannot resolve them,
		// so the coopmux PTY never registers and NATS never connects (the
		// long-standing canary boot-wedge). Append the release-namespace search
		// domain so the bare names resolve — no URL rewriting needed. Only when
		// we actually cross namespaces and we know the release namespace.
		if cfg.Namespace != "" && cfg.Namespace != cfg.GVisorNamespace {
			spec.DNSSearches = append(spec.DNSSearches,
				cfg.Namespace+".svc.cluster.local")
		}
	}
}

// applyLandFence places a tenant land's pod (Lands Track B spec §5.2, step
// 10.2): in the land's namespace from its project bead, under the land
// RuntimeClass (cfg.LandsRuntimeClass; empty is the node's default runtime,
// and any role, project or per-agent runtime_class is dropped), as a
// Kubernetes pod (never another execution target), as the
// land-agent ServiceAccount, with no Kubernetes API
// token and none of the freeze/thaw hostPath machinery. It runs LAST among the
// placement choices in both spec builders, so no role, pilot, per-agent pin or
// gVisor routing can move a tenant pod out of its fence. The namespace is
// always land-<land>, derived from the land's name: the project bead's
// namespace field only says the provisioner's namespace step ran, and is
// never trusted for placement, so a bead that names another namespace cannot
// move a tenant pod (landFenceRefusal refuses such a land at admission). A
// land whose name is not a namespace name keeps an empty Namespace and fails
// to create.
//
// The release namespace's search domain is added, as routeGVisorNamespace
// does, because the controller injects bare service names (gardenio-coopmux,
// gardenio-nats) that only resolve there.
//
// The affinity is replaced whole by podmanager.LandTenantAffinity: a tenant
// pod on runc never shares a node with a fleet agent, the platform pair or the
// control plane, whatever its role (gd-NDJIOutLsv). The spec it marks
// LandFenced also gets the lowest priority class (podmanager).
//
// Last, it keeps the pod to what §5.3 lets a tenant pod hold
// (fenceTenantPodContents, step 10.3).
func applyLandFence(cfg *config.Config, spec *podmanager.AgentPodSpec) {
	cfg.ProjectCacheMu.RLock()
	entry, ok := cfg.ProjectCache[spec.Project]
	acct, acctOK := cfg.AccountCache[purse.AccountName(spec.Project)]
	cfg.ProjectCacheMu.RUnlock()
	if !ok || !entry.IsTenantLand() {
		return
	}
	ns, _ := landns.Name(spec.Project)
	spec.ExecutionTarget = podmanager.ExecutionTargetKubernetes
	spec.Namespace = ns
	spec.RuntimeClassName = cfg.LandsRuntimeClass
	spec.ServiceAccountName = landns.ServiceAccount
	spec.NeedsK8sAPI = false
	spec.NoThawState = true
	spec.Affinity = podmanager.LandTenantAffinity()
	if ns != "" && cfg.Namespace != "" && cfg.Namespace != ns {
		search := cfg.Namespace + ".svc.cluster.local"
		if !slices.Contains(spec.DNSSearches, search) {
			spec.DNSSearches = append(spec.DNSSearches, search)
		}
	}
	fenceTenantPodContents(cfg, spec, acct, acctOK)
}

// EnvFreezeEligible is the pod env var the controller sets at spawn when an
// agent is freeze-eligible (freeze/thaw enabled AND pinned to the runsc
// RuntimeClass). It is the in-pod signal `gb stop` reads to decide whether to
// SUSPEND (transition agent_state=stopping → AgentStop → runsc checkpoint
// --leave-running → thaw-able snapshot) instead of terminally closing the bead
// to done. The value is the controller's authoritative spawn-time decision; the
// controller's own freeze gate (shouldFreeze) re-checks the LIVE pod's
// RuntimeClass, so a stale/missing var only ever costs a cold start, never a
// bad freeze. See cmd/gb/stop.go and the fast-hot-resume epic ka-0000003080.
const EnvFreezeEligible = "BOAT_FREEZE_ELIGIBLE"

// isFreezeEligible reports whether a spec is freeze-eligible: freeze/thaw is
// enabled controller-wide, the spec resolved to the runsc RuntimeClass, AND the
// agent's role is permitted by the WS3 canary allowlist (cfg.FreezeRoleAllowed;
// empty allowlist = every role allowed, the pre-WS3 behaviour). This mirrors the
// controller-side shouldFreeze gate. It is the single predicate both
// markFreezeEligible (env signal) and preferFatNodeForFreeze (fat-node affinity)
// key off, so the two spawn-time freeze affordances never drift. Must only be
// consulted AFTER role/project/agent runtime_class resolution so it sees the
// final RuntimeClassName.
func isFreezeEligible(cfg *config.Config, spec *podmanager.AgentPodSpec) bool {
	// A lambda_microvm agent has no Kubernetes pod, so the gVisor runsc
	// freeze/thaw path never applies to it — its standby is the native
	// SuspendMicrovm path (markMicrovmSuspendEligible), gated on its own master
	// switch. Excluding it here keeps the runsc-pod freeze signal and the MicroVM
	// suspend signal strictly separate (never both on one spec), so the in-VM
	// BOAT_FREEZE_ELIGIBLE is only ever set for a MicroVM agent when
	// MICROVM_SUSPEND_ENABLED is on — not incidentally via a runsc runtime_class.
	if spec.ExecutionTarget == podmanager.ExecutionTargetLambdaMicroVM {
		return false
	}
	// A tenant land pod has no thaw-state hostPath (applyLandFence), so it
	// cannot freeze; it resumes cold from its workspace like a runc pod.
	if spec.NoThawState {
		return false
	}
	return cfg.FreezeThawEnabled &&
		spec.RuntimeClassName == podmanager.RuntimeClassRunsc &&
		cfg.FreezeRoleAllowed(spec.Role)
}

// markFreezeEligible injects EnvFreezeEligible=true into the pod env when the
// agent is freeze-eligible (isFreezeEligible). Mirrors the controller-side
// shouldFreeze gate so the in-pod `gb stop` suspend decision and the
// controller's freeze decision stay in lockstep. Must be called LAST, after
// role/project/agent runtime_class resolution (alongside routeGVisorNamespace),
// so it sees the final RuntimeClassName. Shared by BOTH spec builders so they
// never drift.
func markFreezeEligible(cfg *config.Config, spec *podmanager.AgentPodSpec) {
	if isFreezeEligible(cfg, spec) {
		if spec.Env == nil {
			spec.Env = map[string]string{}
		}
		spec.Env[EnvFreezeEligible] = "true"
	}
	// MicroVM native suspend-on-stop shares the SAME in-VM signal (see
	// markMicrovmSuspendEligible): a suspend-eligible MicroVM thread agent's
	// `gb stop` must transition agent_state=stopping (AgentStop) rather than close
	// the bead, exactly like a runsc freeze. isFreezeEligible excludes MicroVM
	// agents, so at most one of these two branches ever fires for a given spec.
	markMicrovmSuspendEligible(cfg, spec)
}

// isMicrovmSuspendEligible reports whether a spec is eligible for the native
// suspend-on-stop path: the MICROVM_SUSPEND_ENABLED master switch is on, the spec
// resolved to the lambda_microvm backend, AND it is a thread agent (bound to a
// Slack thread). Only thread agents suspend — they are the ones a later
// re-mention resumes (step 3); a one-shot crew/gardener MicroVM agent has no wake
// to serve, so it terminates on stop as before. The thread binding is read from
// the SLACK_THREAD_* env the spec builders stamp from the bead's
// slack_thread_channel/slack_thread_ts metadata. This is the single predicate
// both the in-VM signal (markMicrovmSuspendEligible) and the controller teardown
// branch agree on, mirroring how isFreezeEligible/shouldFreeze stay in lockstep.
func isMicrovmSuspendEligible(cfg *config.Config, spec *podmanager.AgentPodSpec) bool {
	return cfg.MicrovmSuspendEnabled &&
		spec.ExecutionTarget == podmanager.ExecutionTargetLambdaMicroVM &&
		spec.Env["SLACK_THREAD_CHANNEL"] != "" &&
		spec.Env["SLACK_THREAD_TS"] != ""
}

// markMicrovmSuspendEligible injects EnvFreezeEligible=true into the (forwarded)
// MicroVM env when the agent is suspend-eligible (isMicrovmSuspendEligible), so
// the in-VM `gb stop` takes the SUSPEND path (transition agent_state=stopping,
// the sole AgentStop trigger) instead of CloseBead(done). Reuses the same
// BOAT_FREEZE_ELIGIBLE var `gb stop` already reads — the in-VM decision is
// backend-agnostic ("should this stop suspend or terminally close?"), and the
// controller's own teardown branch (handleEvent) re-derives the MicroVM suspend
// gate authoritatively, so a stale/missing var only ever costs a terminate,
// never a bad suspend. Must be called LAST (from markFreezeEligible), after
// execution-target + thread-metadata resolution. microvmAgentSpecFromPodSpec
// forwards spec.Env into the VM, so setting it here reaches the in-VM gb.
func markMicrovmSuspendEligible(cfg *config.Config, spec *podmanager.AgentPodSpec) {
	if isMicrovmSuspendEligible(cfg, spec) {
		if spec.Env == nil {
			spec.Env = map[string]string{}
		}
		spec.Env[EnvFreezeEligible] = "true"
	}
}

// FatNodeMemoryFloorMiB is the node total-memory threshold (in MiB) below which
// a freeze-eligible gVisor agent should avoid being placed, expressed against
// Karpenter's well-known `karpenter.k8s.aws/instance-memory` node label (integer
// MiB). It is the SAME threshold the durable NodePool re-size (ka-0000002348)
// enforces as a HARD requirement — this soft controller-side preference is its
// zero-cost, reversible analogue — and it tracks the LIVE prod gvisor-poc pool.
//
// Why 12000 (m6a.xlarge, 16384 MiB) and not the original 6000 (m6a.large, 8192):
// the pre-thaw checks a heavy restore must clear were BOTH fattened after 6000
// was first chosen, and neither fits an m6a.large any longer:
//   - NODE mem-gate (preThawMemoryGate → modeledRestoreNeed, ka-0000007579): a
//     heavy pages.img (max 2.947 GiB, PoC ka-0000002587) now needs
//     pages*1.5 + 768Mi*2 + 512Mi ≈ 6.42 GiB of FREE RAM at thaw.
//   - Pod B CGROUP LIMIT (modeledRestorePeakLimit, canary #15 ka-0000000413):
//     memory.max = pages*3.0 + 768Mi*2 + 512Mi ≈ 10.84 GiB, inside which the
//     `runsc restore` process peaks >2.71× the image. m6a.large (~7.1 GiB
//     allocatable) CANNOT hold that peak → cgroup-OOM SIGKILL mid-restore; only
//     m6a.xlarge (~15.1 GiB allocatable) does. So 6000 would admit a node that
//     schedules the pod but then OOM-kills every heavy rehydrate — worse than a
//     clean cold-start. `Gt 12000` excludes every 8 GiB *.large and admits the
//     16 GiB *.xlarge (m6a.xlarge = live prod today, 3× nodes), whose cheapest
//     [c,m] fit clears both checks with headroom.
const FatNodeMemoryFloorMiB = "12000"

// fatNodeAffinityWeight steers a freeze-eligible agent's spawn toward a fat
// (m6a) gVisor node so an m6a wins over a c6a within the gVisor pool. Kept at
// 100 (equal to the batch Spot preference, the highest default capacity
// preference in capacityAffinity): a heavy freeze that cannot clean-rehydrate is
// worse than any spot/capacity tradeoff. (The old weight-80 c-family preference
// this once had to outrank was removed as vestigial in ka-0000006729.)
const fatNodeAffinityWeight = 100

// preferFatNodeForFreeze appends a SOFT (preferred, not required) node-affinity
// term steering a freeze-eligible runsc agent's ORIGINAL spawn toward a gVisor
// node with enough total RAM (> FatNodeMemoryFloorMiB) to clean-rehydrate a
// heavy checkpoint on a later thaw. It is the zero-cost, fully reversible
// INTERIM half of the fat-node fix (ka-0000002348) for the node-fit blocker
// ka-0000006507: the freeze/thaw memory gate is fail-close + node-local (Pod B
// is hard-pinned to the freeze node), so steering only the original spawn onto
// the m6a is sufficient — Pod B inherits that node on thaw.
//
// SOFT by construction: if no fat node exists (or the fat node is full) the pod
// still schedules on a c6a exactly as today — it never blocks a spawn, it only
// re-orders otherwise-equal candidates. Non-freeze-eligible agents (runc, or
// freeze/thaw disabled) are untouched, so blast radius is zero outside the
// gVisor freeze path.
//
// Must be called LAST, after role/project/agent runtime_class resolution and
// after any mode/project/role affinity is applied (it appends to whatever
// affinity already exists), so it sees the final RuntimeClassName and never has
// its term clobbered. Shared by BOTH spec builders so they never drift.
func preferFatNodeForFreeze(cfg *config.Config, spec *podmanager.AgentPodSpec) {
	if !isFreezeEligible(cfg, spec) {
		return
	}
	term := corev1.PreferredSchedulingTerm{
		Weight: fatNodeAffinityWeight,
		Preference: corev1.NodeSelectorTerm{
			MatchExpressions: []corev1.NodeSelectorRequirement{{
				Key:      "karpenter.k8s.aws/instance-memory",
				Operator: corev1.NodeSelectorOpGt,
				Values:   []string{FatNodeMemoryFloorMiB},
			}},
		},
	}
	if spec.Affinity == nil {
		spec.Affinity = &corev1.Affinity{}
	}
	if spec.Affinity.NodeAffinity == nil {
		spec.Affinity.NodeAffinity = &corev1.NodeAffinity{}
	}
	spec.Affinity.NodeAffinity.PreferredDuringSchedulingIgnoredDuringExecution = append(
		spec.Affinity.NodeAffinity.PreferredDuringSchedulingIgnoredDuringExecution,
		term,
	)
}

// applyProjectDefaults applies per-project overrides from project bead metadata.
// Applied after mode defaults, before controller common config.
func applyProjectDefaults(cfg *config.Config, spec *podmanager.AgentPodSpec) {
	entry, ok := cfg.ProjectCache[spec.Project]
	if !ok {
		return
	}
	if entry.Image != "" {
		spec.Image = entry.Image
	}
	if entry.StorageClass != "" && spec.WorkspaceStorage != nil {
		spec.WorkspaceStorage.StorageClassName = entry.StorageClass
	}
	if entry.ServiceAccount != "" {
		spec.ServiceAccountName = entry.ServiceAccount
	}
	// RuntimeClass: pin all of this project's pods to a K8s RuntimeClass (e.g.
	// "runsc" for gVisor). Set below role (applyRoleDefaults) and per-agent
	// metadata, both of which run after this and override a non-empty value.
	// Empty leaves the cluster default. This makes "every agent in the gvisor
	// project rides runsc → auto freeze/thaw on resume" a single project-bead
	// field instead of a per-spawn tag (epic ka-0000007417).
	if entry.RuntimeClassName != "" {
		spec.RuntimeClassName = entry.RuntimeClassName
	}

	// Apply per-project resource overrides. Individual fields override the
	// corresponding resource value while preserving other defaults.
	if entry.CPURequest != "" || entry.CPULimit != "" || entry.MemoryRequest != "" || entry.MemoryLimit != "" {
		if spec.Resources == nil {
			spec.Resources = &corev1.ResourceRequirements{
				Requests: corev1.ResourceList{},
				Limits:   corev1.ResourceList{},
			}
		}
		applyResourceOverride(spec.Resources.Requests, corev1.ResourceCPU, entry.CPURequest)
		applyResourceOverride(spec.Resources.Limits, corev1.ResourceCPU, entry.CPULimit)
		applyResourceOverride(spec.Resources.Requests, corev1.ResourceMemory, entry.MemoryRequest)
		applyResourceOverride(spec.Resources.Limits, corev1.ResourceMemory, entry.MemoryLimit)
	}

	// Docker: enable DinD sidecar if project has docker enabled.
	if entry.DockerEnabled {
		spec.Docker = true
	}

	// Postgres: enable PostgreSQL sidecar if project has postgres enabled.
	if entry.PostgresEnabled {
		spec.Postgres = true
	}

	// K8s API access: opt-in at the project level.
	if entry.NeedsK8sAPI {
		spec.NeedsK8sAPI = true
	}

	// Session resume: project setting feeds the default. Role overrides project
	// in applyRoleDefaults; agent metadata overrides both in the caller.
	if entry.SessionResume != nil {
		v := *entry.SessionResume
		spec.SessionResume = &v
	}

	// Apply per-project env overrides (additive; project values take precedence).
	for k, v := range entry.EnvOverrides {
		if spec.Env == nil {
			spec.Env = make(map[string]string)
		}
		spec.Env[k] = v
	}

	// Inject per-project plain env vars. Project values do not override
	// env vars that are already set (e.g., from controller config).
	for _, ev := range entry.EnvVars {
		if _, exists := spec.Env[ev.Name]; !exists {
			spec.Env[ev.Name] = ev.Value
		}
	}
}

// applyRoleDefaults applies per-role overrides from role bead metadata.
// Applied after project defaults, before controller common config.
// For multi-role agents (e.g. "thread,crew"), applies the first role's
// non-additive fields; additive fields (secrets, env) are merged from all roles.
func applyRoleDefaults(cfg *config.Config, spec *podmanager.AgentPodSpec) {
	if len(cfg.RoleCache) == 0 || spec.Role == "" {
		return
	}

	primaryApplied := false
	for _, role := range parseRoleList(spec.Role) {
		entry, ok := cfg.RoleCache[role]
		if !ok {
			continue
		}

		// Non-additive fields: only apply from the first matching role.
		if !primaryApplied {
			primaryApplied = true
			if entry.Image != "" {
				spec.Image = entry.Image
			}
			if entry.StorageClass != "" && spec.WorkspaceStorage != nil {
				spec.WorkspaceStorage.StorageClassName = entry.StorageClass
			}
			if entry.ServiceAccount != "" {
				spec.ServiceAccountName = entry.ServiceAccount
			} else if entry.Verifier && cfg.VerifierServiceAccount != "" {
				spec.ServiceAccountName = cfg.VerifierServiceAccount
				spec.NeedsK8sAPI = true
			}
			if entry.RuntimeClassName != "" {
				spec.RuntimeClassName = entry.RuntimeClassName
			}
			if entry.CPURequest != "" || entry.CPULimit != "" || entry.MemoryRequest != "" || entry.MemoryLimit != "" {
				if spec.Resources == nil {
					spec.Resources = &corev1.ResourceRequirements{
						Requests: corev1.ResourceList{},
						Limits:   corev1.ResourceList{},
					}
				}
				applyResourceOverride(spec.Resources.Requests, corev1.ResourceCPU, entry.CPURequest)
				applyResourceOverride(spec.Resources.Limits, corev1.ResourceCPU, entry.CPULimit)
				applyResourceOverride(spec.Resources.Requests, corev1.ResourceMemory, entry.MemoryRequest)
				applyResourceOverride(spec.Resources.Limits, corev1.ResourceMemory, entry.MemoryLimit)
			}
			if entry.DockerEnabled {
				spec.Docker = true
			}
			if entry.PostgresEnabled {
				spec.Postgres = true
			}
			if entry.NeedsK8sAPI {
				spec.NeedsK8sAPI = true
			}
			if entry.MachineClient != "" {
				applyRoleMachineClient(cfg, spec, entry.MachineClient)
			}
			if entry.SessionResume != nil {
				v := *entry.SessionResume
				spec.SessionResume = &v
			}
		}

		// Additive fields: merge from all matching roles.
		for k, v := range entry.EnvOverrides {
			if spec.Env == nil {
				spec.Env = make(map[string]string)
			}
			spec.Env[k] = v
		}
		for _, ev := range entry.EnvVars {
			if _, exists := spec.Env[ev.Name]; !exists {
				if spec.Env == nil {
					spec.Env = make(map[string]string)
				}
				spec.Env[ev.Name] = ev.Value
			}
		}
	}
}

// applyResourceOverride sets a resource quantity in a ResourceList if the value
// is non-empty and parses as a valid Kubernetes quantity.
func applyResourceOverride(list corev1.ResourceList, name corev1.ResourceName, value string) {
	if value == "" {
		return
	}
	q, err := resource.ParseQuantity(value)
	if err != nil {
		return // silently skip invalid values
	}
	list[name] = q
}

// applyCommonConfig wires controller-level config into an AgentPodSpec.
// Shared by both BuildSpecFromBeadInfo (reconciler) and buildAgentPodSpec (events).
func applyCommonConfig(cfg *config.Config, spec *podmanager.AgentPodSpec) {
	if spec.ServiceAccountName == "" && cfg.CoopServiceAccount != "" {
		spec.ServiceAccountName = cfg.CoopServiceAccount
	}
	if cfg.ClaudeOAuthSecret != "" {
		spec.CredentialsSecret = cfg.ClaudeOAuthSecret
	}
	if cfg.BeadsTokenSecret != "" {
		spec.DaemonTokenSecret = cfg.BeadsTokenSecret
	}
	applySlackViewSession(cfg, spec)
	if len(cfg.ImagePullSecrets) > 0 && len(spec.ImagePullSecrets) == 0 {
		spec.ImagePullSecrets = cfg.ImagePullSecrets
	}

	// Wire git info from project cache (multi-repo aware).
	if entry, ok := cfg.ProjectCache[spec.Project]; ok {
		if len(entry.Repos) > 0 {
			for _, r := range entry.Repos {
				if r.Role == "primary" {
					spec.GitURL = r.URL
					if r.Branch != "" {
						spec.GitDefaultBranch = r.Branch
					}
				} else {
					name := r.Name
					if name == "" {
						name = repoNameFromURL(r.URL)
					}
					branch := r.Branch
					spec.ReferenceRepos = append(spec.ReferenceRepos, podmanager.RepoRef{
						URL: r.URL, Branch: branch, Name: name,
					})
				}
			}
		} else {
			// Legacy single-repo fallback.
			if entry.GitURL != "" {
				spec.GitURL = entry.GitURL
			}
			if entry.DefaultBranch != "" {
				spec.GitDefaultBranch = entry.DefaultBranch
			}
		}
	}

	// Build BOAT_REFERENCE_REPOS env var for the entrypoint (fallback cloning).
	if len(spec.ReferenceRepos) > 0 {
		var entries []string
		for _, r := range spec.ReferenceRepos {
			b := r.Branch
			if b == "" {
				b = "main"
			}
			entries = append(entries, fmt.Sprintf("%s=%s:%s", r.Name, r.URL, b))
		}
		spec.Env["BOAT_REFERENCE_REPOS"] = strings.Join(entries, ",")
	}

	// Build BOAT_PROJECTS env var from project cache for entrypoint project registration.
	if len(cfg.ProjectCache) > 0 {
		var projectEntries []string
		for name, entry := range cfg.ProjectCache {
			if entry.GitURL != "" && entry.Prefix != "" {
				projectEntries = append(projectEntries, fmt.Sprintf("%s=%s:%s", name, entry.GitURL, entry.Prefix))
			}
		}
		if len(projectEntries) > 0 {
			spec.Env["BOAT_PROJECTS"] = strings.Join(projectEntries, ",")
		}
	}

	// Wire S3 config for session artifact persistence (transcripts, recordings, session logs).
	if cfg.CoopS3Bucket != "" {
		spec.Env["COOP_S3_BUCKET"] = cfg.CoopS3Bucket
	}
	if cfg.CoopS3Prefix != "" {
		spec.Env["COOP_S3_PREFIX"] = cfg.CoopS3Prefix
	}
	if cfg.CoopS3ImagesBucket != "" {
		spec.Env["COOP_S3_IMAGES_BUCKET"] = cfg.CoopS3ImagesBucket
	}
	if cfg.CoopS3Region != "" {
		spec.Env["COOP_S3_REGION"] = cfg.CoopS3Region
	}
	// In-box object store (ka-0000004955): when set, points coop at an
	// S3-compatible endpoint (e.g. an in-cluster MinIO) with path-style
	// addressing. Unset → default AWS S3. Object-store-agnostic plumbing.
	if cfg.CoopS3Endpoint != "" {
		spec.Env["COOP_S3_ENDPOINT"] = cfg.CoopS3Endpoint

		// Object-store credentials (gd-W8OwRoHLLn): endpoint mode has no
		// IRSA, so the MinIO access/secret key pair must be projected onto
		// the pod explicitly. Gated strictly on endpoint mode so the
		// AWS/IRSA path (endpoint unset) is byte-for-byte unchanged — IRSA
		// keeps minting credentials via the ServiceAccount web-identity
		// token, the way it always has. podmanager projects the Secret as a
		// tmpfs-backed volume (not spec.Env) so the keys never appear as a
		// literal pod env value; only the file paths are exposed here.
		//
		// The volume path skips the SecretEnv scrubs below, so the deny-list
		// is applied here too: pointing COOP_S3_CREDENTIALS_SECRET at the MinIO
		// root Secret (gd-urSiLLwPIz) leaves the pod without object-store
		// credentials (no session upload or resume) rather than handing every
		// agent the root keys.
		if cfg.CoopS3CredentialsSecret != "" && isForbiddenPodSecret(cfg.CoopS3CredentialsSecret) {
			if _, seen := warnedForbiddenSecret.LoadOrStore("coop-s3|"+cfg.CoopS3CredentialsSecret, struct{}{}); !seen {
				slog.Error("refusing to mount forbidden object-store secret into agent pods; set COOP_S3_CREDENTIALS_SECRET to the least-privilege agent Secret",
					"secret", cfg.CoopS3CredentialsSecret)
			}
		} else if cfg.CoopS3CredentialsSecret != "" {
			spec.ObjectStoreCredentialsSecret = cfg.CoopS3CredentialsSecret
			spec.Env["COOP_S3_ACCESS_KEY_ID_FILE"] = podmanager.MountObjectStoreCreds + "/" + podmanager.ObjectStoreAccessKeyFileName
			spec.Env["COOP_S3_SECRET_ACCESS_KEY_FILE"] = podmanager.MountObjectStoreCreds + "/" + podmanager.ObjectStoreSecretKeyFileName
		}
	}

	// Wire NATS config to all agents for beads decisions, coop events, and bus emit.
	if cfg.NatsURL != "" {
		spec.Env["BEADS_NATS_URL"] = cfg.NatsURL
		spec.Env["COOP_NATS_URL"] = cfg.NatsURL
		spec.Env["COOP_NATS_FORWARD"] = "1"
	}
	// NATS auth token for agent pods (W5, ka-0000006585): project the
	// token as a file at MountNatsToken/token via a tmpfs-backed Secret
	// volume and point COOP_NATS_TOKEN_FILE at it. Consumers (coop's
	// `--nats-token-file` and `gb hook-relay`) read the token from disk
	// at startup. Avoids env-var injection so the token does not appear
	// in /proc/self/environ or `kubectl describe pod`.
	//
	// When subject isolation is enabled (NatsAgentUserSecret), the secret
	// holds a restricted publish-only token; otherwise the legacy shared
	// token (NatsTokenSecret) is used.
	natsTokenSecret := cfg.NatsAgentUserSecret
	if natsTokenSecret == "" {
		natsTokenSecret = cfg.NatsTokenSecret
	}
	if natsTokenSecret != "" {
		spec.NatsTokenSecret = natsTokenSecret
		spec.Env["COOP_NATS_TOKEN_FILE"] = podmanager.MountNatsToken + "/" + podmanager.NatsTokenFileName
	}
	// Under subject isolation the server declares multi-user auth, whose
	// entries only support user/password — the projected token must be sent
	// as the agent user's PASSWORD, so stamp the (non-secret) username
	// (ka-0000000002). coop and gb hook-relay combine COOP_NATS_USER with the
	// token-file/env secret.
	if cfg.NatsAgentUserSecret != "" {
		spec.Env["COOP_NATS_USER"] = "agent"
	}

	// Default storage class for agent workspace PVCs. Applied only if no project
	// bead override already set it, so project-level config takes precedence.
	if cfg.AgentStorageClass != "" && spec.WorkspaceStorage != nil && spec.WorkspaceStorage.StorageClassName == "" {
		spec.WorkspaceStorage.StorageClassName = cfg.AgentStorageClass
	}

	// Default Claude model for agent pods (e.g., "claude-opus-4-7").
	if cfg.ClaudeModel != "" {
		spec.Env["CLAUDE_MODEL"] = cfg.ClaudeModel
	}

	// Pin the Claude Code harness `fable` alias to the LiteLLM model group the
	// fleet's virtual key actually allows. Without this, the harness resolves
	// `fable` (Agent-tool model:"fable", `--model=fable`, any fable-pinned
	// crew/formula) through its bundled model catalog to a bare "claude-sonnet-5"
	// id, which the LiteLLM key ACL rejects (401 "key not allowed to access model
	// … Tried to access claude-sonnet-5") — so a fable crew is dead on arrival
	// (ka-0000003175). ANTHROPIC_DEFAULT_FABLE_MODEL is read FIRST by the harness's
	// alias resolver and also flips its fable-availability gate true, so pinning it
	// to "claude-fable-5-oauth" (the ACL-allowed "-oauth" group LiteLLM serves —
	// NOT the bare "claude-fable-5" key, which the ACL checks literally BEFORE
	// model_group_alias resolution) routes fable correctly. Config default is
	// non-empty; an empty override disables injection (harness keeps its own
	// default). Unlike CLAUDE_MODEL this is the alias→model mapping, not the
	// session model, so it does not interact with the account/role model override
	// or the BOAT_MODEL_PINNED pin.
	if cfg.AgentDefaultFableModel != "" {
		spec.Env["ANTHROPIC_DEFAULT_FABLE_MODEL"] = cfg.AgentDefaultFableModel
	}

	// Pin the harness `opus` alias to the ACL-allowed Opus group (the fleet's
	// default Opus). Same mechanism and escape hatch as the fable pin above.
	if cfg.AgentDefaultOpusModel != "" {
		spec.Env["ANTHROPIC_DEFAULT_OPUS_MODEL"] = cfg.AgentDefaultOpusModel
	}

	// Pin the Claude Code harness `haiku` alias to the LiteLLM model group the
	// fleet's virtual key actually allows. Without this, the harness resolves
	// `haiku` (Agent-tool model:"haiku", `--model=haiku`, any haiku-pinned
	// crew/formula, and the harness's own small-model ops like WebFetch) through
	// its bundled model catalog to the DATED snapshot id
	// "claude-haiku-4-5-20251001", which the LiteLLM key ACL rejects (401 "key not
	// allowed to access model … Tried to access claude-haiku-4-5-20251001") — so a
	// haiku crew is dead on arrival and the small-model paths 401 silently
	// (ka-0000005599). ANTHROPIC_DEFAULT_HAIKU_MODEL is read FIRST by the harness's
	// alias resolver, so pinning it to "anthropic/claude-haiku-4-5" (the rolling
	// pass-through model GROUP LiteLLM serves and the ACL allows — NOT the dated
	// "…-20251001" snapshot the harness picks on its own, which the ACL checks
	// literally BEFORE model_group_alias resolution) routes haiku correctly. Note
	// the "anthropic/" pass-through prefix here vs. the fable pin's "-oauth" group:
	// mirror the ACL literal, which differs per model. Config default is non-empty;
	// an empty override disables injection (harness keeps its own default). Unlike
	// CLAUDE_MODEL this is the alias→model mapping, not the session model, so it
	// does not interact with the account/role model override or the
	// BOAT_MODEL_PINNED pin.
	if cfg.AgentDefaultHaikuModel != "" {
		spec.Env["ANTHROPIC_DEFAULT_HAIKU_MODEL"] = cfg.AgentDefaultHaikuModel
	}

	// vmagent ingest URL for agent-side metric pushes (epic ka-0000003733,
	// Part B). gb prime / gb session / session-resume gate their pushes on
	// VMAGENT_URL; without this injection the controller never set it on agent
	// pods, so gardenio_prime_hook_bytes and gardenio_session_boot_context_tokens
	// never landed in VictoriaMetrics. Empty AgentVmagentURL leaves the pod with
	// no VMAGENT_URL — the pre-existing silently-skipped behavior.
	if cfg.AgentVmagentURL != "" {
		spec.Env["VMAGENT_URL"] = cfg.AgentVmagentURL
	}

	// Account-based LLM routing: if the project references an account bead,
	// resolve it and configure the agent pod to route API requests through
	// the account's base URL (typically a LiteLLM proxy).
	applyAccountConfig(cfg, spec)

	// Model override: overlay a project- or role-level model on top of the
	// account's default model. Lets one account (one OAuth cred, one LiteLLM
	// key allowing N models) back multiple models without cloning account beads
	// (epic ka-0000005488). Precedence global < project < role < agent — the
	// agent-level metadata override is applied last by the spec builders.
	applyModelOverride(cfg, spec)

	// Harness: agent > role > project > default, validated against the account
	// provider. Env shaping for non-claude harnesses runs later (shapeHarnessEnv)
	// once the agent-level model override is in.
	applyHarness(cfg, spec)

	// Claude Agent Teams: enable team lead → teammate coordination.
	if cfg.ClaudeTeamsEnabled {
		spec.Env["CLAUDE_TEAMS_ENABLED"] = "true"

		// Apply teams-mode resource overrides. Each teammate runs its own
		// Claude Code session (Node.js process + context window), so pods
		// need more memory and CPU than single-session mode.
		if cfg.ClaudeTeamsCPURequest != "" || cfg.ClaudeTeamsCPULimit != "" ||
			cfg.ClaudeTeamsMemoryRequest != "" || cfg.ClaudeTeamsMemoryLimit != "" {
			if spec.Resources == nil {
				spec.Resources = &corev1.ResourceRequirements{
					Requests: corev1.ResourceList{},
					Limits:   corev1.ResourceList{},
				}
			}
			applyResourceOverride(spec.Resources.Requests, corev1.ResourceCPU, cfg.ClaudeTeamsCPURequest)
			applyResourceOverride(spec.Resources.Limits, corev1.ResourceCPU, cfg.ClaudeTeamsCPULimit)
			applyResourceOverride(spec.Resources.Requests, corev1.ResourceMemory, cfg.ClaudeTeamsMemoryRequest)
			applyResourceOverride(spec.Resources.Limits, corev1.ResourceMemory, cfg.ClaudeTeamsMemoryLimit)
		}
	}
	if cfg.ClaudeTeammateMode != "" {
		spec.Env["CLAUDE_TEAMMATE_MODE"] = cfg.ClaudeTeammateMode
	}
	if cfg.ClaudeTeamsMaxTeammates > 0 {
		spec.Env["CLAUDE_TEAMS_MAX_TEAMMATES"] = fmt.Sprintf("%d", cfg.ClaudeTeamsMaxTeammates)
	}

	// E2E beads address: isolated beads instance for e2e tests so spawn
	// events don't hit the production agents controller.
	if cfg.BeadsE2EHTTPAddr != "" {
		spec.Env["BEADS_E2E_HTTP_ADDR"] = cfg.BeadsE2EHTTPAddr
	}

	// Bind coop's HTTP listener to all interfaces so the controller's
	// statusreporter (running in-cluster) can reach the agent pod's coop
	// API at http://<podIP>:<port>/api/v1/agent. The coop CLI default
	// changed to 127.0.0.1 (safe-bind for laptops/dev), so cluster-reachable
	// callers must opt in explicitly. See ka-0000003486, ka-0000005675.
	spec.Env["COOP_HOST"] = "0.0.0.0"

	// Wire coopmux registration config. The agent runs coop directly (builtin)
	// so it gets COOP_BROKER_URL/TOKEN as env vars.
	if cfg.CoopmuxURL != "" {
		spec.Env["COOP_BROKER_URL"] = cfg.CoopmuxURL
		spec.Env["COOP_MUX_URL"] = cfg.CoopmuxURL
	}
	if cfg.CoopmuxTokenSecret != "" {
		spec.SecretEnv = append(spec.SecretEnv, podmanager.SecretEnvSource{
			EnvName:    "COOP_BROKER_TOKEN",
			SecretName: cfg.CoopmuxTokenSecret,
			SecretKey:  "token",
		})
		if cfg.CoopmuxURL != "" {
			spec.SecretEnv = append(spec.SecretEnv, podmanager.SecretEnvSource{
				EnvName:    "COOP_MUX_TOKEN",
				SecretName: cfg.CoopmuxTokenSecret,
				SecretKey:  "token",
			})
		}
	}

	// Slack bridge URL for agent gb slack commands.
	if cfg.SlackBridgeURL != "" {
		spec.Env["SLACK_BRIDGE_URL"] = cfg.SlackBridgeURL
	}

	// Jira bridge URL for agent gb jira reply (F2 ka-0000000065).
	if cfg.JiraBridgeURL != "" {
		spec.Env["JIRA_BRIDGE_URL"] = cfg.JiraBridgeURL
	}

	// Beads-viewer public base URL (bug ka-0000003798): inject BEADS_VIEWER_URL
	// so gb-emitted bundle / image-proxy / widget-preview links resolve to
	// absolute, clickable URLs on the real viewer host (with its /beads path
	// prefix). Without it, `gb bundle` printed a hostless "/bundle/<id>" that
	// Slack showed as plain text and 404'd when pasted onto the prod host.
	if cfg.BeadsViewerURL != "" {
		spec.Env["BEADS_VIEWER_URL"] = cfg.BeadsViewerURL
	}

	// Beads-viewer in-cluster Service base (ka-0000000379): inject
	// BEADS_VIEWER_INTERNAL_URL so `gb plan create` / `gb plan add-step` POST
	// straight to the viewer pod out-of-the-box, without a hand-supplied
	// --viewer-url. This base carries NO edge/base-path prefix, so gb reaches
	// the viewer's HTTP API directly instead of the AVA/OIDC edge (which
	// redirects and fails the write). Previously this was set only on the
	// plan-gardener role bead's env, leaving every other role's pods to fall
	// back to the public BEADS_VIEWER_URL edge. The plan-gardener role env still
	// overrides this in applyRoleSecrets (called later), so an explicit role
	// value continues to win.
	if cfg.BeadsViewerInternalURL != "" {
		spec.Env["BEADS_VIEWER_INTERNAL_URL"] = cfg.BeadsViewerInternalURL
	}

	// Default gardenio host (ka-0000002401): inject GARDENIO_DEFAULT_HOST so the
	// gb CLI inside the pod derives its zero-config daemon/viewer hosts from the
	// unified gardenio host instead of the COMPILED default baked into the binary
	// (cmd/gb/main.go resolveGardenioHost). Bare host, no scheme/path — gb
	// derives /beads + cli.<host> from it. Empty = not injected; gb falls back
	// to its compiled default, exactly as before this wiring. The compiled
	// default is deliberately unchanged. COUPLED to the gated default-URL switch
	// (ka-0000004747): the chart derives this value from `unifiedHost` via the
	// gardenio.link.defaultGardenioHost helper, so it stays "" (inert) until the
	// cutover.
	if cfg.GardenioDefaultHost != "" {
		spec.Env["GARDENIO_DEFAULT_HOST"] = cfg.GardenioDefaultHost
	}

	// Grafana in-cluster render affordance (bug ka-0000005842): inject
	// GRAFANA_URL (the in-cluster Service base) so `gb grafana render <bead>`
	// resolves a base without the agent hand-discovering the svc address, and
	// GRAFANA_AUTH_PROXY_HEADER so the render request can present the auth-proxy
	// identity header in-cluster (Grafana runs auth-proxy + disabled login form).
	if cfg.GrafanaURL != "" {
		spec.Env["GRAFANA_URL"] = cfg.GrafanaURL
	}
	if cfg.GrafanaAuthProxyHeader != "" {
		spec.Env["GRAFANA_AUTH_PROXY_HEADER"] = cfg.GrafanaAuthProxyHeader
	}

	// Service-log read affordance (design ka-0000005579, task ka-0000005209):
	// inject GARDENIO_LOGS_URL (the in-cluster VictoriaLogs Service base) so
	// `gb logs <component>` resolves the log store without the agent
	// hand-discovering the svc address — the READ counterpart to the pod-log
	// shippers, and the same env-injection affordance as GRAFANA_URL above. This
	// does NOT grant any k8s pod-log RBAC; the agent reads the store over HTTP.
	if cfg.LogsURL != "" {
		spec.Env["GARDENIO_LOGS_URL"] = cfg.LogsURL
	}

	// Env-side Grafana Editor token bundle (task ka-0000002337, design §1):
	// per-environment tokens for the ACME environment Grafana (lens.acme.net
	// etc.), distinct from the in-cluster gardenio Grafana above. Each env gets
	// GRAFANA_API_KEY_<ENV> from the shared `grafana-api-key` Secret (key
	// token-<env>) plus the non-secret GRAFANA_BASE_URL_<ENV>. Keys are marked
	// optional so a partially-provisioned bundle (operator wires devel before
	// prod) does not wedge pod creation.
	if cfg.GrafanaEnvAPIKeySecret != "" {
		for _, env := range cfg.GrafanaEnvList {
			upper := strings.ToUpper(env)
			spec.SecretEnv = append(spec.SecretEnv, podmanager.SecretEnvSource{
				EnvName:    "GRAFANA_API_KEY_" + upper,
				SecretName: cfg.GrafanaEnvAPIKeySecret,
				SecretKey:  "token-" + env,
				Optional:   true,
			})
			if url := cfg.GrafanaEnvBaseURLs[env]; url != "" {
				spec.Env["GRAFANA_BASE_URL_"+upper] = url
			}
		}
	}

	// Shared bearer token for /api/slack/* and /api/permission/* — required
	// for the bridge to accept agent traffic when SLACK_BRIDGE_AGENT_TOKEN is
	// set on the bridge side. Agents pick it up via SLACK_BRIDGE_TOKEN.
	if cfg.SlackBridgeTokenSecret != "" {
		spec.SecretEnv = append(spec.SecretEnv, podmanager.SecretEnvSource{
			EnvName:    "SLACK_BRIDGE_TOKEN",
			SecretName: cfg.SlackBridgeTokenSecret,
			SecretKey:  "token",
		})
	}

	// Jira bridge bearer token for gb jira reply — same contract as
	// SLACK_BRIDGE_TOKEN above (F2 ka-0000000065).
	if cfg.JiraBridgeTokenSecret != "" {
		spec.SecretEnv = append(spec.SecretEnv, podmanager.SecretEnvSource{
			EnvName:    "JIRA_BRIDGE_TOKEN",
			SecretName: cfg.JiraBridgeTokenSecret,
			SecretKey:  "token",
		})
	}

	// Channel delivery: enable MCP-based nudge delivery in agent pods.
	if cfg.GardenioChannel {
		spec.Env["GARDENIO_CHANNEL"] = "1"
	}

	// Per-project secrets: inject secret env vars declared on project beads.
	// Secrets must be named "{project}-*" to prevent cross-project access.
	// Also updates init-clone credential refs for git-related secrets.
	if entry, ok := cfg.ProjectCache[spec.Project]; ok {
		for _, ps := range entry.Secrets {
			if !strings.HasPrefix(ps.Secret, spec.Project+"-") {
				if _, seen := warnedSecretPrefix.LoadOrStore("project|"+spec.Project+"|"+ps.Secret, struct{}{}); !seen {
					slog.Warn("skipping secret with invalid prefix",
						"secret", ps.Secret, "project", spec.Project)
				}
				continue
			}
			// Structural guard: controller-only Secrets (the privileged
			// gardenio2@ Jira token, the App keys, the Keycloak credentials)
			// must never reach an agent pod, even when they satisfy the
			// {project}- prefix (ka-0000003053, gd-q8nBnOP65d).
			if refuseBeadSecret(cfg, "project", spec.Project, ps.Secret, ps.Env) {
				continue
			}
			src := podmanager.SecretEnvSource{
				EnvName: ps.Env, SecretName: ps.Secret, SecretKey: ps.Key,
			}
			overrideOrAppendSecretEnv(&spec.SecretEnv, src)

			// Update init container credential refs for git-related overrides.
			switch ps.Env {
			case "GIT_TOKEN", "GIT_USERNAME":
				spec.GitCredentialsSecret = ps.Secret
			case "GITLAB_TOKEN":
				spec.GitlabTokenSecret = ps.Secret
			}
		}

		// Per-project plain env vars: inject non-secret config like
		// JIRA_BASE_URL, JIRA_EMAIL, GIT_AUTHOR_EMAIL, etc.
		if spec.Env == nil {
			spec.Env = make(map[string]string)
		}
		for _, ev := range entry.EnvVars {
			spec.Env[ev.Name] = ev.Value
		}
	}

	// Per-role secrets: inject secret env vars declared on role beads.
	// Role secrets override project secrets with the same env var name
	// (role is more specific than project in the layering order).
	// Secret naming enforcement: role secrets must be prefixed with "{role}-".
	applyRoleSecrets(cfg, spec)

	// Final structural guarantee (epic ka-0000003475 / task ka-0000003053):
	// scrub any forbidden secret (e.g. the privileged gardenio2@ Jira writer
	// token, `jira2-*`) from the fully-assembled SecretEnv. The per-project
	// and per-role loops above already refuse to add it, but this catch-all
	// also covers the account-credential path (applyAccountConfig) and any
	// future injection site — so an agent pod is incapable of carrying the
	// privileged Jira identity BY CONSTRUCTION, not just by convention.
	scrubForbiddenSecretEnv(spec)

	// Dark-Jira write-token fence (ka-0000005242): for a dark pod, scrub the
	// Jira write credential (JIRA_API_TOKEN) from every plaintext + secret
	// injection site. Runs LAST so it catches the token no matter which layer
	// set it — the pod-layer half of the defense-in-depth fence (the network
	// half is the dark-agent-egress NetworkPolicy).
	scrubDarkForbiddenEnv(spec)

	// Dark marker env (ka-0000005242): expose the dark identity in-pod so the gb
	// CLI constructs a READ-ONLY Jira client (write methods fail closed) — the
	// client-layer half of the fence, belt-and-suspenders over the fenced token
	// and denied egress. Set after the scrub (the scrub only touches the fenced
	// write-token names, never this marker).
	if spec.Dark {
		if spec.Env == nil {
			spec.Env = make(map[string]string)
		}
		spec.Env[beadsapi.EnvGardenioDark] = "true"
	}
}

// scrubForbiddenSecretEnv removes any SecretEnvSource whose K8s Secret is on the
// hard deny-list (isForbiddenPodSecret) from the assembled pod spec. It is the
// last-line, injection-site-agnostic enforcement of the two-account Jira split:
// no matter which layer tried to mount the privileged gardenio2@ token, it never
// reaches the pod. Warnings are deduped process-wide.
func scrubForbiddenSecretEnv(spec *podmanager.AgentPodSpec) {
	if len(spec.SecretEnv) == 0 {
		return
	}
	kept := spec.SecretEnv[:0]
	for _, e := range spec.SecretEnv {
		if isForbiddenPodSecret(e.SecretName) {
			if _, seen := warnedForbiddenSecret.LoadOrStore("scrub|"+spec.Project+"|"+spec.Role+"|"+e.SecretName, struct{}{}); !seen {
				slog.Warn("scrubbed forbidden secret from agent pod spec",
					"secret", e.SecretName, "env", e.EnvName,
					"project", spec.Project, "role", spec.Role)
			}
			continue
		}
		kept = append(kept, e)
	}
	spec.SecretEnv = kept
}

// overrideOrAppendSecretEnv replaces an existing SecretEnvSource with the
// same EnvName, or appends if no match exists.
func overrideOrAppendSecretEnv(envs *[]podmanager.SecretEnvSource, src podmanager.SecretEnvSource) {
	for i, e := range *envs {
		if e.EnvName == src.EnvName {
			(*envs)[i] = src
			return
		}
	}
	*envs = append(*envs, src)
}

// applyRoleSecrets injects role-level secrets and plain env vars into the pod spec.
// Called from applyCommonConfig after project secrets have been applied.
// Role secrets override project secrets with the same env var name.
// Acquires RoleCacheMu.RLock internally.
func applyRoleSecrets(cfg *config.Config, spec *podmanager.AgentPodSpec) {
	if spec.Role == "" {
		return
	}

	// Parse comma-separated roles and apply secrets from all matching roles.
	roles := parseRoleList(spec.Role)

	cfg.RoleCacheMu.RLock()
	defer cfg.RoleCacheMu.RUnlock()

	for _, role := range roles {
		entry, ok := cfg.RoleCache[role]
		if !ok {
			continue
		}

		// Inject role-level secrets. Secret names must be prefixed with "{role}-"
		// to prevent cross-role access (same pattern as project prefix enforcement).
		for _, rs := range entry.Secrets {
			if !strings.HasPrefix(rs.Secret, role+"-") {
				if _, seen := warnedSecretPrefix.LoadOrStore("role|"+role+"|"+rs.Secret, struct{}{}); !seen {
					slog.Warn("skipping role secret with invalid prefix",
						"secret", rs.Secret, "role", role)
				}
				continue
			}
			// Structural guard: same as the project path. A role named
			// "gardenio-machine" passes the {role}- prefix for every
			// gardenio-machine-* Secret (ka-0000003053, gd-q8nBnOP65d).
			if refuseBeadSecret(cfg, "role", role, rs.Secret, rs.Env) {
				continue
			}
			src := podmanager.SecretEnvSource{
				EnvName: rs.Env, SecretName: rs.Secret, SecretKey: rs.Key,
			}
			overrideOrAppendSecretEnv(&spec.SecretEnv, src)

			// Update init container credential refs for git-related overrides.
			switch rs.Env {
			case "GIT_TOKEN", "GIT_USERNAME":
				spec.GitCredentialsSecret = rs.Secret
			case "GITLAB_TOKEN":
				spec.GitlabTokenSecret = rs.Secret
			}
		}

		// Inject role-level plain env vars. Role values override project values
		// (role is more specific in the layering order).
		if spec.Env == nil {
			spec.Env = make(map[string]string)
		}
		for _, ev := range entry.EnvVars {
			spec.Env[ev.Name] = ev.Value
		}
	}
}

// repoNameFromURL extracts the repository name from a URL.
// "https://github.com/org/my-repo.git" → "my-repo"
func repoNameFromURL(rawURL string) string {
	u := strings.TrimSuffix(rawURL, ".git")
	parts := strings.Split(u, "/")
	if len(parts) > 0 {
		return parts[len(parts)-1]
	}
	return "repo"
}

// namespaceFromEvent returns the namespace from event metadata or a default.
func namespaceFromEvent(event subscriber.Event, defaultNS string) string {
	if ns := event.Metadata["namespace"]; ns != "" {
		return ns
	}
	return defaultNS
}

// metadataOr returns the event metadata value for key, or fallback if empty.
func metadataOr(event subscriber.Event, key, fallback string) string {
	if v := event.Metadata[key]; v != "" {
		return v
	}
	return fallback
}

// zitiAccessResolution carries the per-pod Ziti settings resolved across the
// project < role < agent precedence chain before they are combined with
// controller-level config in applyZitiAccess.
type zitiAccessResolution struct {
	enabled   bool
	serviceID string
	bind      bool
	ttl       string
	optional  bool
}

// applyZitiAccess resolves per-pod OpenZiti overlay access (epic ka-0000000990)
// and, when enabled, populates spec.ZitiAccess so the pod builders add the
// projected broker token, the ziti-lease init container, and the ziti-tunnel
// sidecar. It is the FINAL Ziti resolver: call it after project/role/common
// config and after the agent-metadata overlays, so all four precedence layers
// (global < project < role < agent) have been read.
//
// The master switch is cfg.ZitiAccessBrokerURL: if the controller has no broker
// URL configured, per-pod Ziti access stays OFF no matter what beads request —
// so a misconfigured bead can never wedge pod creation on a cluster without a
// broker. When a bead opts in (ziti_access=true) but a required input is
// missing (no resolved service id, or no tunneler image configured), this logs
// a clear warning and leaves the feature OFF rather than emitting a broken pod.
//
// Takes its own brief read locks on the project/role caches, mirroring
// applyRoleSecrets, so it is safe to call from either spec builder.
func applyZitiAccess(cfg *config.Config, spec *podmanager.AgentPodSpec, metadata map[string]string) {
	// Master switch: no broker configured → feature globally off.
	if cfg.ZitiAccessBrokerURL == "" {
		return
	}

	res := resolveZitiAccess(cfg, spec, metadata)
	if !res.enabled {
		return
	}

	if res.serviceID == "" {
		slog.Warn("ziti_access requested but no ziti_service_id resolved; leaving overlay access OFF",
			"project", spec.Project, "role", spec.Role, "agent", spec.AgentName)
		return
	}
	if cfg.ZitiTunnelerImage == "" {
		slog.Warn("ziti_access requested but ZITI_TUNNELER_IMAGE is not configured on the controller; leaving overlay access OFF",
			"project", spec.Project, "role", spec.Role, "agent", spec.AgentName, "service_id", res.serviceID)
		return
	}

	ttl := res.ttl
	if ttl == "" {
		ttl = cfg.ZitiAccessDefaultTTL
	}

	spec.ZitiAccess = &podmanager.ZitiAccessSpec{
		BrokerURL:     cfg.ZitiAccessBrokerURL,
		TokenAudience: cfg.ZitiAccessTokenAudience, // empty → podmanager default
		ServiceID:     res.serviceID,
		Bind:          res.bind,
		TTL:           ttl,
		TunnelerImage: cfg.ZitiTunnelerImage,
		Optional:      res.optional,
	}
}

// resolveZitiAccess merges the Ziti settings across project < role < agent.
// Each more-specific layer overrides the less-specific one. The agent metadata
// layer also supports an explicit "ziti_access=false" kill-switch that disables
// the feature even if a project/role enabled it.
func resolveZitiAccess(cfg *config.Config, spec *podmanager.AgentPodSpec, metadata map[string]string) zitiAccessResolution {
	var res zitiAccessResolution

	// Project layer.
	cfg.ProjectCacheMu.RLock()
	if entry, ok := cfg.ProjectCache[spec.Project]; ok {
		if entry.ZitiAccessEnabled {
			res.enabled = true
		}
		if entry.ZitiServiceID != "" {
			res.serviceID = entry.ZitiServiceID
		}
		res.bind = entry.ZitiBind
		if entry.ZitiTTL != "" {
			res.ttl = entry.ZitiTTL
		}
		res.optional = entry.ZitiOptional
	}
	cfg.ProjectCacheMu.RUnlock()

	// Role layer (overrides project). Multi-role: the first matching role that
	// sets a value wins, mirroring applyRoleDefaults' iteration.
	cfg.RoleCacheMu.RLock()
	for _, role := range parseRoleList(spec.Role) {
		entry, ok := cfg.RoleCache[role]
		if !ok {
			continue
		}
		if entry.ZitiAccessEnabled {
			res.enabled = true
		}
		if entry.ZitiServiceID != "" {
			res.serviceID = entry.ZitiServiceID
		}
		if entry.ZitiBind {
			res.bind = true
		}
		if entry.ZitiTTL != "" {
			res.ttl = entry.ZitiTTL
		}
		if entry.ZitiOptional {
			res.optional = true
		}
	}
	cfg.RoleCacheMu.RUnlock()

	// Agent metadata layer (highest precedence). "false" is an explicit
	// kill-switch; "true" forces on.
	switch metadata["ziti_access"] {
	case "true":
		res.enabled = true
	case "false":
		res.enabled = false
	}
	if v := metadata["ziti_service_id"]; v != "" {
		res.serviceID = v
	}
	switch metadata["ziti_bind"] {
	case "true":
		res.bind = true
	case "false":
		res.bind = false
	}
	if v := metadata["ziti_ttl"]; v != "" {
		res.ttl = v
	}
	switch metadata["ziti_optional"] {
	case "true":
		res.optional = true
	case "false":
		res.optional = false
	}

	return res
}

// resolveExecutionTarget resolves the runtime backend for an agent across the
// standard precedence global < project < role < agent. Empty layers are
// skipped; the most-specific non-empty layer wins. The returned value is
// normalized to a known target: an unrecognized value logs a warning and falls
// back to the Kubernetes pod path so a typo can never silently strand an agent.
func resolveExecutionTarget(cfg *config.Config, spec *podmanager.AgentPodSpec, metadata map[string]string) string {
	target := ""

	// Project layer.
	cfg.ProjectCacheMu.RLock()
	if entry, ok := cfg.ProjectCache[spec.Project]; ok && entry.ExecutionTarget != "" {
		target = entry.ExecutionTarget
	}
	cfg.ProjectCacheMu.RUnlock()

	// Role layer (overrides project). Multi-role: the first matching role that
	// sets a value wins, mirroring resolveZitiAccess' iteration.
	cfg.RoleCacheMu.RLock()
	for _, role := range parseRoleList(spec.Role) {
		entry, ok := cfg.RoleCache[role]
		if !ok {
			continue
		}
		if entry.ExecutionTarget != "" {
			target = entry.ExecutionTarget
			break
		}
	}
	cfg.RoleCacheMu.RUnlock()

	// Agent metadata layer (highest precedence).
	if v := metadata["execution_target"]; v != "" {
		target = v
	}

	switch target {
	case "", podmanager.ExecutionTargetKubernetes:
		return podmanager.ExecutionTargetKubernetes
	case podmanager.ExecutionTargetLambdaMicroVM:
		return podmanager.ExecutionTargetLambdaMicroVM
	case podmanager.ExecutionTargetProbe:
		return podmanager.ExecutionTargetProbe
	default:
		slog.Warn("unrecognized execution_target; falling back to kubernetes pod path",
			"project", spec.Project, "role", spec.Role, "agent", spec.AgentName,
			"execution_target", target)
		return podmanager.ExecutionTargetKubernetes
	}
}

// applyAccountConfig resolves a project's account bead reference and configures
// the agent pod for LLM API routing through the account's base URL (typically
// a LiteLLM proxy for Claude Code Max subscriptions).
//
// When a project bead has account="claude-max-litellm", the controller looks up
// the matching type:account bead and sets:
//   - ANTHROPIC_BASE_URL → account's proxy_url (claudemore) if set, else
//     base_url (or cfg.LiteLLMURL as fallback)
//   - ANTHROPIC_MODEL → account's model (if set)
//   - ANTHROPIC_CUSTOM_HEADERS → "x-litellm-api-key: Bearer <key>" plus an
//     "x-litellm-tags: agent:<name>" line (newline-separated) so LiteLLM tracks
//     per-agent spend, read back at session close (if litellm_key set)
//
// The agent's OAuth token (from credentials.json) flows through to Anthropic via
// LiteLLM's forward_client_headers_to_llm_api setting, while the virtual key
// enables per-project budget tracking and usage attribution.
func applyAccountConfig(cfg *config.Config, spec *podmanager.AgentPodSpec) {
	// Resolve the harness before choosing an account: selection filters to the
	// harness's providers (ADR 0004 §4), so a codex spec must know it is codex
	// here. A resolution error is left for applyHarness to report (it re-runs the
	// same resolution), and selection then proceeds as claude.
	if spec.Harness == "" {
		if h, err := resolveHarness(cfg, spec.Project, spec.Role, spec.RequestedHarness); err == nil {
			spec.Harness = h
		}
	}
	// Role-level account takes precedence over project-level — roles like
	// fsm-triage can be pinned to a specific LLM account (e.g. litellm-bedrock)
	// regardless of which project they run in.
	// layer tracks which resolution layer produced accountName (Phase 0
	// instrumentation, ka-0000002142). Recorded on spec.AccountLayer at every
	// exit — success and refusal alike — so the spawn path can count
	// gardenio_agent_account_layer_total. Pure bookkeeping; no behavior change.
	layer := gardeniometrics.AccountLayerRolePin
	accountName := roleAccountPin(cfg, spec.Project, spec.Role)
	if accountName == "" {
		// Spread across the project's healthy account pool by live-pod count
		// instead of pinning every pod to the single cached account
		// (ka-0000004861). selectAccount preserves the role-pin and
		// account-exclusivity guards; it returns "" for a role that owns an
		// exclusive account and when no healthy project account exists. Post-Phase-2
		// (ka-0000004094) an empty spread pool with no opus fallback is an explicit
		// SpawnBlock, not a fall-through to a project-cache/DefaultAccount tail.
		//
		// Abolish-primary Phase 4 invariant (ka-0000005978): SPREAD WINS over the
		// "primary account" marker. selectAccount ranks over
		// cfg.AccountCache, whose entries carry no primary field at all, so a
		// project:<name> pool with a primary-marked account still spreads new pods
		// across the whole healthy pool — the marker cannot pin.
		liveCounts := cfg.SnapshotAccountLiveCounts()
		if spread := selectAccount(cfg, spec.Project, spec, liveCounts); spread != "" {
			accountName = spread
			layer = gardeniometrics.AccountLayerSpread
		} else if fbAccount, fbModel := fallbackModelGroupAccount(cfg, spec, liveCounts); fbAccount != "" {
			// Opus fallback (ka-0000003806, decision ka-0000003299): the requested
			// model group's pool is fully gated (e.g. every account fable-exhausted),
			// but an opus-capable account has capacity. Land there and downgrade the
			// pod's model to opus rather than blocking the spawn. applyModelDowngrade
			// (spec builders) forces the model last so it wins over the role/agent
			// fable pin.
			accountName = fbAccount
			spec.ModelDowngrade = fbModel
			layer = gardeniometrics.AccountLayerOpusFallback
			slog.Warn("model-group pool exhausted — falling back to opus on a healthy account (decision ka-0000003299)",
				"project", spec.Project, "agent", spec.AgentName, "role", spec.Role,
				"requested_model", spec.RequestedModel, "fallback_account", fbAccount, "fallback_model", fbModel)
		}
	}
	if accountName == "" {
		// No account resolved. Phase 2 (ka-0000004094, account-routing.md §7)
		// collapsed the old layer-3 project-cache fall-through, the layer-4
		// DefaultAccount fall-through, and the layer-5 resolveNonExhaustedAccount
		// redirect into this single explicit block. Distinguish two cases.
		//
		// A project that simply has no account beads is a benign Debug (most
		// projects use direct provider auth). But a project that DOES have
		// credentialed account beads labeled project:<name> and still resolves to
		// no account means the resolver excluded every candidate — role-pin,
		// spread, and opus-fallback all declined. Before Phase 2 that fell through
		// to the project-cache/DefaultAccount tail and pods spawned with NO
		// ANTHROPIC_* env, falling back to synthetic turns with no loud signal.
		// That was the gasbox outage (ka-0000007343): a stale controller binary
		// treated out_of_credits as a hard block, so selectAccount +
		// buildProjectAccountOverride excluded the only project accounts and
		// applyAccountConfig early-returned here silently.
		//
		// Now, reusing the pool-exhaustion escape-hatch machinery (ka-0000002560),
		// mark the spec SpawnBlocked with an explicit reason so the spawn path
		// declines CreateAgentPod and the reconciler re-attempts on the next sync
		// cycle once an account becomes healthy — rather than baking a
		// credential-less pod. This is a TRANSIENT refusal: the bead is NOT failed
		// and no escalation is filed. The WARN is preserved as the block reason so
		// the "credentialed accounts but none picked" condition is never invisible.
		if n := countCredentialedProjectAccounts(cfg, spec.Project, spec.Harness); n > 0 {
			resetAt := earliestPoolResetAt(cfg, spec.Project)
			if resetAt == "" {
				resetAt = "unknown"
			}
			spec.SpawnBlocked = true
			spec.SpawnBlockReason = fmt.Sprintf("project has %d credentialed account bead(s) but resolution selected none (all excluded by throttle/overage/exhausted gating); blocking new spawns until %s", n, resetAt)
			spec.AccountLayer = gardeniometrics.AccountLayerNoCredentialWarn
			slog.Warn("project has credentialed account beads but resolution selected none — blocking spawn instead of baking a credential-less pod that would produce synthetic turns; check account throttle/overage/exhausted gating",
				"project", spec.Project, "agent", spec.AgentName,
				"labeled_credentialed_accounts", n, "earliest_reset_at", resetAt,
				"account_layer", spec.AccountLayer)
			return
		}
		// A project bead that PINS an account (`account: <name>`) which exists in
		// the cache but carries no project:<name> label is dead config, NOT the
		// benign direct-provider-auth case (ka-0000006314). Phase 2 retired the
		// layer-3 reader of that field, so the pin no longer resolves; because
		// countCredentialedProjectAccounts also keys off the label it reports 0 and
		// the branch above does not fire. The result was a credential-less pod
		// spawned at Debug level: every agent 401s on request 1 with zero tokens
		// and is post-mortemed as a generic "platform failure — never got an
		// inference turn" (the devsecops outage, silent 2026-08-12 → 2026-08-27).
		// Warn and stamp a distinct layer so the condition is countable in
		// gardenio_agent_account_layer_total. Deliberately still a spawn (not a
		// SpawnBlock): a project whose stale account field is genuinely vestigial
		// must keep spawning exactly as before — the fix is to add the label.
		if pinned := projectPinnedAccount(cfg, spec.Project); pinned != "" {
			if _, known := cfg.AccountCache[pinned]; known {
				spec.AccountLayer = gardeniometrics.AccountLayerUnlabeledPin
				slog.Warn("project bead pins an account that carries no project:<name> label — the pin is NOT read on the spawn path (layer 3 retired in Phase 2, ka-0000004094), so this pod gets NO ANTHROPIC_* env and will 401 on its first request with zero tokens; add the project:<name> label to the pinned account bead",
					"project", spec.Project, "agent", spec.AgentName,
					"pinned_account", pinned, "want_label", "project:"+spec.Project,
					"account_layer", spec.AccountLayer)
				return
			}
		}
		spec.AccountLayer = gardeniometrics.AccountLayerNoCredentialBenign
		slog.Debug("no account configured for project",
			"project", spec.Project, "agent", spec.AgentName,
			"account_layer", spec.AccountLayer)
		return
	}

	// Pool-exhaustion escape hatch (ka-0000002560). A role-pinned or spread-picked
	// account can still be capacity-exhausted by the time we get here (the spread
	// selector filters on a snapshot; a role pin is not spread-filtered at all).
	// During the 2026-06-29 monorepo incident every account in the pool was
	// exhausted simultaneously, so resolution handed back an exhausted account and
	// pods baked onto a doomed key — instant 429 on turn 1, fleet-wide. Detect
	// that terminal case here: if the finally-resolved account is STILL
	// capacity-exhausted, refuse to bake a known-dead credential.
	//
	// Mark the spec SpawnBlocked so the spawn path declines CreateAgentPod and
	// the reconciler re-attempts on the next sync cycle, once a window resets.
	// This is a TRANSIENT refusal — the bead is NOT failed and no escalation is
	// filed; the pool recovers on its own when an account's reset_at passes.
	//
	// Two deliberate exemptions keep this from altering happy-path behavior:
	//   - accountCapacityExhausted is NARROWER than !isHealthyAuthorized: a
	//     keyless-but-not-exhausted LiteLLM account (base_url set, no litellm_key)
	//     is a misconfiguration the existing proceed-with-warn path handles, not a
	//     capacity stall, so it is NOT blocked here.
	//   - An exclusive account (exclusive_to_role) is left alone: its design is to
	//     surface the failure on its reserved role rather than spill, so an
	//     exhausted exclusive account spawns-and-fails-visibly exactly as before
	//     (the general project pool is what this guard protects).
	if acct, ok := cfg.AccountCache[accountName]; ok &&
		accountCapacityExhausted(acct) && strings.TrimSpace(acct.ExclusiveToRole) == "" {
		// Role-pin exhaustion spill (ka-0000001307). A role that PINS a single
		// account (resolveRoleAccount) bypasses the spread pool entirely, so when
		// that pinned account is capacity-exhausted here — and the project has
		// OTHER healthy accounts the pin could never reach — blocking would park
		// the agent queued indefinitely until the pinned account's rate window
		// resets (observed: 45+ min to days, stalling the release lane twice:
		// ka-0000006248, ka-0000007145). Before falling into the block, try to
		// spill onto the project's healthy spread pool. Guarded so the happy path
		// and every deliberate pin-forever lane are unchanged:
		//   - Only when the account was RESOLVED BY THE ROLE PIN (layer ==
		//     AccountLayerRolePin). A spread/opus pick that lands exhausted is
		//     already the whole-pool-exhausted case the block handles correctly —
		//     spilling would just re-run the same selector.
		//   - Only when the role permits spill (roleAllowsAccountSpill): a role
		//     that MUST stay pinned sets no_account_spill=true and keeps the block.
		//   - selectSpreadAccountAt returns "" for an exclusive-owning role and
		//     when no healthy project account exists, so an exclusive lane never
		//     spills and a genuinely all-exhausted pool still blocks.
		// A pin whose provider does not match the harness never spills: that is a
		// misconfiguration applyHarness reports, not capacity to route around.
		if layer == gardeniometrics.AccountLayerRolePin && roleAllowsAccountSpill(cfg, spec.Role) && accountMatchesHarness(acct, spec.Harness) {
			liveCounts := cfg.SnapshotAccountLiveCounts()
			if spill := selectSpreadAccountAt(cfg, spec.Project, spec, liveCounts, time.Now().UTC()); spill != "" && spill != accountName {
				slog.Warn("role-pinned account is capacity-exhausted — spilling to the project's healthy spread pool instead of parking the agent queued (ka-0000001307)",
					"project", spec.Project, "agent", spec.AgentName, "role", spec.Role,
					"pinned_account", accountName, "pinned_status", acct.Status,
					"spill_account", spill)
				accountName = spill
				layer = gardeniometrics.AccountLayerRolePinSpill
				// Fall through to the whitelist gate + credential bake below with
				// the spilled account (re-fetched from the cache there). The
				// spilled account came from selectSpreadAccountAt, which already
				// excludes capacity-exhausted accounts, so it will not re-trip this
				// block.
			}
		}
		if layer != gardeniometrics.AccountLayerRolePinSpill {
			resetAt := earliestPoolResetAt(cfg, spec.Project)
			if resetAt == "" {
				resetAt = "unknown"
			}
			spec.SpawnBlocked = true
			spec.SpawnBlockReason = fmt.Sprintf("project pool fully exhausted, blocking new spawns until %s", resetAt)
			spec.AccountLayer = gardeniometrics.AccountLayerEscapeHatchBlocked
			slog.Warn("project pool fully exhausted — refusing to spawn onto a known-dead account; reconciler will re-attempt next sync cycle",
				"project", spec.Project, "agent", spec.AgentName, "role", spec.Role,
				"resolved_account", accountName, "account_status", acct.Status,
				"throttle_status", acct.ThrottleStatus, "earliest_reset_at", resetAt,
				"account_layer", spec.AccountLayer)
			return
		}
	}

	// Role account_whitelist final gate (fail-closed, ka-0000003061): every path
	// that reaches here — spread pick and role pin — must respect the role's
	// whitelist. selectAccount already restricts the spread pool, but a role
	// pin (resolveRoleAccount) bypasses it, so re-check the finally-resolved
	// account here. A whitelisted role resolving to a forbidden account bakes NO
	// credential (BOAT_ACCOUNT stays unset), exactly as gb-init's pre-MR-!4740
	// filter did, rather than silently credentialing against a forbidden pool.
	if !accountAllowedByRoleWhitelist(cfg, spec.Role, accountName) {
		spec.AccountLayer = gardeniometrics.AccountLayerWhitelistBlocked
		slog.Error("role account_whitelist has no overlap with resolved account — baking no credential (fail-closed)",
			"project", spec.Project, "agent", spec.AgentName, "role", spec.Role,
			"resolved_account", accountName,
			"whitelist", resolveRoleAccountWhitelist(cfg, spec.Role),
			"account_layer", spec.AccountLayer)
		return
	}

	// Record the finally-resolved account so the spawn path can optimistically
	// bump its live-pod count for spread selection (ka-0000004861).
	spec.ResolvedAccount = accountName
	// Phase 0 (ka-0000002142): stamp the winning layer for the credentialed
	// happy path. This is `layer` as tracked through the resolution above
	// (role-pin / spread / opus-fallback — the only credential-producing paths
	// left after Phase 2, ka-0000004094), recorded here where a credential is
	// actually baked.
	spec.AccountLayer = layer

	acct, ok := cfg.AccountCache[accountName]
	if !ok {
		// An account name resolved but it is absent from the cache — config drift.
		// No credential is baked, so record the terminal layer as unknown-account
		// rather than leaving the earlier happy-path stamp (ka-0000002142).
		spec.AccountLayer = gardeniometrics.AccountLayerUnknownAccount
		slog.Warn("project references unknown account",
			"project", spec.Project, "account", accountName, "agent", spec.AgentName,
			"cache_size", len(cfg.AccountCache),
			"account_layer", spec.AccountLayer)
		return
	}

	slog.Info("applying account config to agent pod",
		"project", spec.Project, "agent", spec.AgentName,
		"account", accountName, "status", acct.Status,
		"account_layer", spec.AccountLayer,
		"has_litellm_key", acct.LiteLLMKey != "",
		"has_credential", acct.Credential != "",
		"has_base_url", acct.BaseURL != "",
		"model", acct.Model)

	if spec.Env == nil {
		spec.Env = make(map[string]string)
	}

	// Pin BOAT_ACCOUNT to the account whose credential we are about to bake into
	// this pod's env (spec.ResolvedAccount). This is the authoritative initial
	// account identity: it names exactly the OAuth token injected below as
	// CLAUDE_CODE_OAUTH_TOKEN.
	//
	// Why set it here rather than let `gb init` resolve it: init's
	// resolveProjectAccount picks an account by FNV-hashing the pod hostname over
	// the project pool — a DIFFERENT algorithm than applyAccountConfig's
	// live-count spread (selectAccount). For multi-account projects the two
	// can diverge, so the account coop reports (init's pick) would not match the
	// credential the controller actually baked (this pick). Setting BOAT_ACCOUNT
	// here makes init skip its own resolution (it early-returns when BOAT_ACCOUNT
	// is already set), so the reported account always equals the baked one.
	//
	// This is only the INITIAL seed, not an ongoing source of truth: coop seeds a
	// named credential profile from BOAT_ACCOUNT at startup, and on any
	// failover/switchback the coopmux distributor moves the active profile via
	// set_active(), after which coop reports the live account via active_name()
	// and coopmux reconciles assigned_account from the session health response.
	// So the earlier staleness concern (which motivated removing this env var) no
	// longer applies — the dynamic path overrides this seed on every switch.
	// See ka-0000002509.
	spec.Env["BOAT_ACCOUNT"] = accountName

	// Codex subscription login (provider=openai, ADR 0004 §C): mount the derived
	// -session Secret and stop. None of the Anthropic/LiteLLM/CLAUDE_CODE_OAUTH_TOKEN
	// baking below applies, and no model pin or LiteLLM tag is emitted.
	if isOpenAIAccount(acct) {
		applyCodexAccountConfig(spec, accountName, acct)
		return
	}

	// Gemini Google-account login (provider=gemini, credential
	// secret:<name>#oauth_creds.json): mount the login Secret and stop, for the
	// same reason as Codex. An API-key gemini account falls through unchanged.
	if applyGeminiAccountConfig(spec, accountName, acct) {
		return
	}

	// Set the API base URL (resolution order documented on
	// resolveAccountBaseURL).
	baseURL := resolveAccountBaseURL(acct.ProxyURL, acct.BaseURL, cfg.LiteLLMURL)
	if baseURL != "" {
		spec.Env["ANTHROPIC_BASE_URL"] = baseURL
	}

	// Set the model name. This is the model alias registered in LiteLLM's
	// model_list (e.g., "anthropic-claude"), not the raw model ID.
	// Both ANTHROPIC_MODEL and CLAUDE_MODEL must be set — Claude Code
	// uses CLAUDE_MODEL as the primary model selection, and the controller's
	// global ClaudeModel config would otherwise override the account model.
	if acct.Model != "" {
		spec.Env["ANTHROPIC_MODEL"] = acct.Model
		spec.Env["CLAUDE_MODEL"] = acct.Model
	}

	// Set LiteLLM virtual key header for per-project budget tracking.
	// The x-litellm-api-key header authenticates with LiteLLM for usage
	// tracking. The agent's OAuth token (Authorization: Bearer) is forwarded
	// by LiteLLM to Anthropic's direct API for Max subscription billing
	// (requires forward_client_headers_to_llm_api: true and an anthropic/
	// model group — NOT bedrock/ — in LiteLLM config).
	if acct.LiteLLMKey != "" {
		// Tag every request with the agent identity AND its attribution dimensions
		// so LiteLLM's spend tracking attributes per-agent AND per-{role,project,
		// model} cost. The tags surface in GET /spend/tags as individual_request_tag
		// "agent:<name>" / "role:<role>" / "project:<project>" / "model:<model>".
		//
		// agent:<name> — read at session close to reconcile authoritative cost onto
		//   the session bead and agent rollup (reconcile_session_spend.go,
		//   ka-0000004228).
		// role:/project:/model: — polled by tag_spend_sync.go into the real-bill
		//   gauge gardenio_tag_cost_usd so the cost-rollup by-role/by-project/by-model
		//   views read the REAL LiteLLM bill instead of the at-close counter
		//   (ka-0000000478). LiteLLM books the full per-request cost under EVERY tag,
		//   so each dimension independently sums to the real account bill.
		//
		// Multiple ANTHROPIC_CUSTOM_HEADERS are newline-separated (Claude Code
		// env-vars reference); LiteLLM consumes x-litellm-tags (comma-separated)
		// rather than forwarding it upstream. Commas are the tag delimiter, so the
		// role value is comma-sanitized (multi-role "crew,thread" → "crew-thread")
		// via SanitizeRole; project/model are constrained identifiers with no commas.
		//
		// The model value is SEEDED here from the account default (or the
		// controller-global ClaudeModel) because a role/project/agent model override
		// is not yet resolved at credential time. It is then re-tagged to the
		// OVERRIDE-EFFECTIVE model by retagLiteLLMModel, called from applyModelOverride
		// and applyAgentModelOverride once spec.ResolvedModel is finalized — so a
		// fable role riding an opus-default account books its spend under model:fable,
		// not model:opus (ka-0000003806, closing the former account-default-granularity
		// follow-up).
		headers := "x-litellm-api-key: Bearer " + acct.LiteLLMKey
		// Commas delimit x-litellm-tags entries, so the new role/project/model
		// values are comma-stripped (→ dash) before they go in — multi-role
		// "crew,thread" must not split into two tags, and a stray comma in any
		// value would corrupt the list. SanitizeRole does exactly comma→dash. The
		// agent: tag value is left verbatim: reconcile_session_spend.go reads it
		// back as exactly "agent:"+AgentName, and agent names are K8s-name-safe
		// (comma-free) so sanitizing would be a no-op that only risks contract drift.
		var tags []string
		if spec.AgentName != "" {
			tags = append(tags, "agent:"+spec.AgentName)
		}
		if spec.Role != "" {
			tags = append(tags, "role:"+podmanager.SanitizeRole(spec.Role))
		}
		if spec.Project != "" {
			tags = append(tags, "project:"+podmanager.SanitizeRole(spec.Project))
		}
		if model := acct.Model; model != "" {
			tags = append(tags, "model:"+podmanager.SanitizeRole(model))
		} else if cfg.ClaudeModel != "" {
			tags = append(tags, "model:"+podmanager.SanitizeRole(cfg.ClaudeModel))
		}
		if len(tags) > 0 {
			headers += "\nx-litellm-tags: " + strings.Join(tags, ",")
		}
		spec.Env["ANTHROPIC_CUSTOM_HEADERS"] = headers
	} else if baseURL != "" {
		slog.Warn("account has base_url but no litellm_key — agent will fail to authenticate with LiteLLM proxy",
			"project", spec.Project, "agent", spec.AgentName,
			"account", accountName, "base_url", baseURL)
	}

	// Inject OAuth credentials from the account bead's credential field.
	// This centralises token rotation: one K8s secret per account, shared
	// by all projects that reference it.
	if cred := acct.Credential; cred != "" {
		parts := strings.SplitN(cred, ":", 2)
		// An account credential has no name prefix to satisfy, so the
		// forbidden markers and the allow-list are its only check
		// (gd-q8nBnOP65d).
		if len(parts) == 2 && parts[0] == "secret" && !refuseBeadSecret(cfg, "account", accountName, parts[1], "CLAUDE_CODE_OAUTH_TOKEN") {
			overrideOrAppendSecretEnv(&spec.SecretEnv, podmanager.SecretEnvSource{
				EnvName:    "CLAUDE_CODE_OAUTH_TOKEN",
				SecretName: parts[1],
				SecretKey:  "token",
			})
		}
	}
}

// resolveModelOverride returns the role/project model override for a spec, or
// "" when neither sets one. Precedence is role > project: a role's model field
// supersedes the project's, matching the layering order global < project < role
// < agent (the agent-level override is applied separately by the spec builders).
// For multi-role agents ("thread,crew") the first role in the list with a
// non-empty model wins, mirroring resolveRoleAccount's first-match semantics.
//
// Lock discipline mirrors applyAccountConfig: the caller (applyCommonConfig)
// already holds ProjectCacheMu.RLock, so cfg.ProjectCache is read directly
// WITHOUT re-acquiring that mutex (a nested RLock can deadlock against a pending
// writer). RoleCacheMu is a separate mutex and is acquired internally.
func resolveModelOverride(cfg *config.Config, project, role string) string {
	if role != "" && len(cfg.RoleCache) > 0 {
		cfg.RoleCacheMu.RLock()
		for _, r := range parseRoleList(role) {
			if entry, ok := cfg.RoleCache[r]; ok && entry.Model != "" {
				cfg.RoleCacheMu.RUnlock()
				return entry.Model
			}
		}
		cfg.RoleCacheMu.RUnlock()
	}
	if project != "" {
		if entry, ok := cfg.ProjectCache[project]; ok && entry.Model != "" {
			return entry.Model
		}
	}
	return ""
}

// applyModelOverride overlays a role- or project-level model on top of the
// account's default model (set by applyAccountConfig). When a role or project
// pins a model, both ANTHROPIC_MODEL and CLAUDE_MODEL are overwritten so the
// agent uses the overridden model regardless of which account backs it — one
// account (one OAuth cred, one LiteLLM key allowing N models) can serve
// fable/opus/sonnet roles by setting model on the role bead, no account cloning
// (epic ka-0000005488). The agent-level metadata override (highest precedence)
// is applied afterward by the spec builders. A no-op when no override is set,
// so accounts with a pinned account.Model keep their default.
//
// Invoked from applyCommonConfig immediately after applyAccountConfig, under
// the caller's ProjectCacheMu.RLock — see resolveModelOverride for the lock
// discipline.
func applyModelOverride(cfg *config.Config, spec *podmanager.AgentPodSpec) {
	model := resolveModelOverride(cfg, spec.Project, spec.Role)
	if model == "" {
		return
	}
	if spec.Env == nil {
		spec.Env = make(map[string]string)
	}
	spec.Env["ANTHROPIC_MODEL"] = model
	spec.Env["CLAUDE_MODEL"] = model
	// Keep the LiteLLM cost tag in lockstep with the override (ka-0000003806): the
	// model: tag was baked from the account default in applyAccountConfig before
	// this override was known, so re-tag it to the override-effective model.
	retagLiteLLMModel(spec, model)
	// Pin the model on the pod so a coopmux credential reload cannot revert it
	// (epic ka-0000005488, scope item 3). The coopmux credential broker is
	// account-keyed: it stores ONE model per account and pushes it as
	// CLAUDE_MODEL/ANTHROPIC_MODEL to every session on that account during a
	// reload/rotation. Without a pin, coop's apply_credentials would overwrite
	// this per-pod override with the account default the moment credentials
	// rotate. Because a single account can now back multiple models (the whole
	// point of this epic), the override cannot live on the account record — it
	// must be re-asserted pod-side. The in-pod gb init loop reads
	// BOAT_MODEL_PINNED and forces CLAUDE_MODEL/ANTHROPIC_MODEL back to it after
	// applying the credentials coop wrote on a switch. See
	// cmd/gb/init_agent_loop.go loadSwitchCredentials.
	spec.Env[EnvModelPinned] = model
	// Record the resolved model as an out-parameter for the spawn path /
	// observability (mirrors ResolvedAccount). Empty when no override applied.
	spec.ResolvedModel = model
}

// EnvModelPinned is the pod env var the controller sets when a role/project/
// agent model override is applied. The in-pod gb init loop reads it and
// re-asserts CLAUDE_MODEL/ANTHROPIC_MODEL to this value after applying
// credentials pushed by the coopmux broker, so an account-keyed credential
// reload never reverts a per-pod model override (epic ka-0000005488). See
// cmd/gb/init_agent_loop.go loadSwitchCredentials.
const EnvModelPinned = "BOAT_MODEL_PINNED"

// applyAgentModelOverride applies the agent-level model override from bead
// metadata ("model"), the highest-precedence layer in global < project < role
// < agent (epic ka-0000005488). Called by both spec builders AFTER
// applyCommonConfig (which runs applyAccountConfig + applyModelOverride), so a
// per-agent model wins over the account default and any project/role override.
// A no-op when metadata carries no model. Also sets the BOAT_MODEL_PINNED pin
// so a coopmux credential reload cannot revert the override.
func applyAgentModelOverride(spec *podmanager.AgentPodSpec, model string) {
	model = strings.TrimSpace(model)
	if model == "" {
		return
	}
	if spec.Env == nil {
		spec.Env = make(map[string]string)
	}
	spec.Env["ANTHROPIC_MODEL"] = model
	spec.Env["CLAUDE_MODEL"] = model
	spec.Env[EnvModelPinned] = model
	spec.ResolvedModel = model
	retagLiteLLMModel(spec, model)
}

// retagLiteLLMModel rewrites the model:<x> entry in the pod's x-litellm-tags to
// `model:<sanitized override>` so LiteLLM books this agent's spend under the
// OVERRIDE-EFFECTIVE model, not the account default (ka-0000003806, the L1739
// follow-up). Background: applyAccountConfig bakes the model: tag from
// acct.Model at credential time, BEFORE any role/project/agent model override is
// resolved — so a fable role riding an opus-default account books its cost under
// model:opus, making the by-model cost view wrong exactly where fable matters.
// Both model-override entry points call this after they finalize spec.ResolvedModel,
// keeping the cost tag in lockstep with the pinned model.
//
// A no-op when the pod carries no ANTHROPIC_CUSTOM_HEADERS (keyless account — no
// LiteLLM tagging at all) or no x-litellm-tags line. If a model: tag is present it
// is replaced; if the tags line exists without one (older/edge specs) a model: tag
// is appended so attribution is never dropped. The value is comma-sanitized via
// SanitizeRole, matching how applyAccountConfig writes every tag.
func retagLiteLLMModel(spec *podmanager.AgentPodSpec, model string) {
	model = strings.TrimSpace(model)
	if model == "" || spec.Env == nil {
		return
	}
	header := spec.Env["ANTHROPIC_CUSTOM_HEADERS"]
	if header == "" {
		return
	}
	const tagsPrefix = "x-litellm-tags: "
	lines := strings.Split(header, "\n")
	changed := false
	for i, line := range lines {
		rest, ok := strings.CutPrefix(line, tagsPrefix)
		if !ok {
			continue
		}
		tags := strings.Split(rest, ",")
		want := "model:" + podmanager.SanitizeRole(model)
		replaced := false
		for j, tag := range tags {
			if strings.HasPrefix(tag, "model:") {
				tags[j] = want
				replaced = true
				break
			}
		}
		if !replaced {
			tags = append(tags, want)
		}
		lines[i] = tagsPrefix + strings.Join(tags, ",")
		changed = true
		break
	}
	if changed {
		spec.Env["ANTHROPIC_CUSTOM_HEADERS"] = strings.Join(lines, "\n")
	}
}

// EnvUltracode is the pod env var that carries a per-spawn ultracode override
// ("on" or "off"). `gb setup claude` (runSetupClaude) reads it and forces the
// ultracode knob on/off in ~/.claude/settings.json authoritatively over any
// resolved claude-settings config bead. Unset/empty means "no per-spawn
// override — honor resolved beads". The literal "on"/"off" vocabulary is
// deliberately distinct from parseTriBoolMeta's "true"/"false" so the two
// tri-states never collide.
const EnvUltracode = "BOAT_ULTRACODE"

// applyUltracode resolves the ultracode/workflow-mode toggle across the full
// project < role < agent precedence chain and bakes it into BOAT_ULTRACODE. It
// is called by BOTH spec builders (mirroring applyModelOverride +
// applyAgentModelOverride) so a reconcile/restart never strips the resolved
// value. A strict no-op when no layer opts in either way, so an agent with no
// project/role ultracode config and no per-spawn override honors the pod's
// resolved claude-settings beads exactly as before this layer existed.
//
// This is the fix for ka-0000001714: before it, ultracode existed ONLY as a
// per-agent field stamped by the --ultracode Slack flag, so a role-default
// MicroVM agent (which lands on the MicroVM backend via the gardenio-microvms
// role's execution_target, never touching the flag) got no ultracode. Adding
// the project/role layer lets that role opt its agents in via role config.
func applyUltracode(cfg *config.Config, spec *podmanager.AgentPodSpec, metadata map[string]string) {
	if (spec.Harness == beadsapi.HarnessCodex || spec.Harness == beadsapi.HarnessGemini) && spec.HarnessError == "" {
		applyUltracodeCodex(spec, metadata)
		return
	}
	v := resolveUltracode(cfg, spec, metadata)
	if v != "on" && v != "off" {
		return
	}
	if spec.Env == nil {
		spec.Env = make(map[string]string)
	}
	spec.Env[EnvUltracode] = v
}

// applyUltracodeCodex is the non-Claude branch of applyUltracode (ADR 0004
// section 13; Gemini shares it, ADR 0005 section 8). Ultracode is Claude Code's
// workflow/subagent knob, written into ~/.claude/settings.json by `gb setup
// claude`; Codex and Gemini have no equivalent setting, so BOAT_ULTRACODE is
// never baked into a codex or gemini pod. An explicit per-spawn
// --ultracode=on (agent metadata) is REFUSED via HarnessError, because the
// operator asked for behaviour the pod cannot deliver and silently dropping it
// would look like a working ultracode run. Everything else is a quiet no-op:
// --ultracode=off asks for what a codex pod already is, and a project/role
// ultracode setting is a Claude-side default that must not break a mixed
// project's codex roles.
func applyUltracodeCodex(spec *podmanager.AgentPodSpec, metadata map[string]string) {
	if strings.TrimSpace(metadata["ultracode"]) == "on" {
		spec.HarnessError = fmt.Sprintf(
			"--ultracode=on is not supported on harness %q: ultracode is a Claude Code workflow/subagent setting "+
				"and %s has no equivalent; drop --ultracode, or spawn with --harness claude",
			spec.Harness, harnessDisplayName(spec.Harness))
	}
}

// resolveUltracode resolves the ultracode toggle across the standard precedence
// global < project < role < agent. Ultracode is genuinely TRI-STATE
// (on / off / unset): an unset layer is SKIPPED — never read as off — so a
// more-specific layer's explicit choice always wins and the resolver returns
// "" only when NO layer opts in either way. The project and role layers carry a
// *bool (nil = unset), while the agent-metadata layer carries the canonical
// "on"/"off" vocabulary stamped by the --ultracode Slack flag parser
// (deliberately distinct from parseTriBool's true/false), so it is matched
// directly rather than through parseTriBoolMeta. Takes its own brief read locks
// on the project/role caches, mirroring resolveExecutionTarget, so it is safe
// to call from either spec builder after the lock-guarded config phases.
func resolveUltracode(cfg *config.Config, spec *podmanager.AgentPodSpec, metadata map[string]string) string {
	result := "" // unset

	// Project layer.
	cfg.ProjectCacheMu.RLock()
	if entry, ok := cfg.ProjectCache[spec.Project]; ok && entry.Ultracode != nil {
		result = ultracodeWord(*entry.Ultracode)
	}
	cfg.ProjectCacheMu.RUnlock()

	// Role layer (overrides project). Multi-role: the first matching role that
	// sets a value wins, mirroring resolveExecutionTarget's iteration.
	cfg.RoleCacheMu.RLock()
	for _, role := range parseRoleList(spec.Role) {
		entry, ok := cfg.RoleCache[role]
		if !ok {
			continue
		}
		if entry.Ultracode != nil {
			result = ultracodeWord(*entry.Ultracode)
			break
		}
	}
	cfg.RoleCacheMu.RUnlock()

	// Agent metadata layer (highest precedence). Canonical "on"/"off" only; any
	// other value (absent/unrecognized) leaves the resolved layer untouched.
	if v := strings.TrimSpace(metadata["ultracode"]); v == "on" || v == "off" {
		result = v
	}

	return result
}

// ultracodeWord maps a resolved *bool layer to the canonical BOAT_ULTRACODE
// vocabulary ("on"/"off"), the same words the --ultracode flag parser emits.
func ultracodeWord(on bool) string {
	if on {
		return "on"
	}
	return "off"
}

// Account exclusivity (exclusive_to_role) — the data-driven replacement for the
// formerly-hardcoded Bedrock partition (ka-0000005223, policy ka-0000002660).
//
// An account bead may carry an `exclusive_to_role: <role>` field. When set, the
// account is reserved to that role: it is excluded from every other role's
// spread pool (selectAccount) and is never offered to a non-matching role
// as an exhaustion fallback (resolveNonExhaustedAccount). An agent pinned to an
// exclusive account (e.g. via a role account pin) stays on it even when the
// account is exhausted — an exclusive account never spills onto the general
// pool. This replaces BOTH halves of the old partition:
//   - (a) "the owner uses ONLY this account" — expressed by the role's account
//     pin plus the no-spill rule below.
//   - (b) "no other role may touch this account" — expressed by the per-account
//     exclusion in the spread and fallback paths.
//
// The Bedrock account (litellm-bedrock) sets exclusive_to_role=fsm-triage, so a
// new exclusive account needs no controller change. Keying the no-spill rule on
// the ACCOUNT (not a role string literal) also unifies every role pinned to the
// exclusive account: the old isBedrockOnlyRole guard matched the literal
// "fsm-triage" only, so a second Bedrock-pinned role (hema-analyst) was wrongly
// redirected onto a Claude Max account on exhaustion. exclusive_to_role fixes
// that latent inconsistency.

// resolveAccountBaseURL resolves the API base URL for an account. Resolution
// order:
//  1. proxyURL (account.proxy_url) — a fronting proxy (e.g. claudemore) that
//     records/replays and forwards to litellm. claudemore AUGMENTS, never
//     replaces: litellm still sees every request, so spend tagging is
//     unchanged. proxy_url is the instant-rollback lever (ka-0000007035):
//     clearing it on the account bead reverts the next pod spawn straight to
//     litellm within ~60s, no code change or redeploy. See
//     gardenio/docs/claudemore-rollback-runbook.md.
//  2. baseURL (account.base_url) — the litellm proxy directly (legacy default).
//  3. defaultURL (cfg.LiteLLMURL) — controller-level fallback.
//
// Used by both the pod-spawn path (applyAccountConfig → ANTHROPIC_BASE_URL env)
// and the coopmux credential sync (syncAccountsToCoopmux) so that staged
// rotation profiles carry the same endpoint a fresh pod on that account would
// get (ka-0000002623).
func resolveAccountBaseURL(proxyURL, baseURL, defaultURL string) string {
	if proxyURL != "" {
		return proxyURL
	}
	if baseURL != "" {
		return baseURL
	}
	return defaultURL
}

// resolveSpendReadBaseURL resolves the LiteLLM base URL the controller's spend
// READERS (spend_sync + reconcile_session_spend) query — and it deliberately
// IGNORES proxy_url. This is the load-bearing half of the claudemore
// transparency property (A1, task ka-0000007533 / R5 §5.2): agents WRITE their
// LLM traffic through claudemore (resolveAccountBaseURL, which honors proxy_url),
// but claudemore meters NO spend itself — litellm remains the single source of
// truth. So the controller must read spend from litellm DIRECTLY, at base_url,
// never at the fronting proxy. Routing the read through claudemore would make
// the /key/info + /spend/tags reconciliation depend on claudemore correctly
// proxying litellm's spend endpoints (which it does not — it is a Messages-API
// proxy only), silently breaking total_cost_usd / gardenio_agent_spend_usd_total
// the moment a proxy is attached.
//
// Because proxy_url is skipped, an account fronted by claudemore and the same
// account with proxy_url cleared (the A2 rollback) read spend from the IDENTICAL
// endpoint — which is exactly why the metering pipeline is byte-identical to a
// litellm-direct baseline whether or not claudemore is in front (A1).
//
// Resolution order (the write-path proxyURL rung is intentionally absent):
//  1. baseURL (account.base_url) — the litellm proxy directly.
//  2. defaultURL (the controller-global LITELLM_URL) — fallback.
//
// See gardenio/docs/claudemore-transparency-proof.md.
func resolveSpendReadBaseURL(baseURL, defaultURL string) string {
	if trimmed := strings.TrimRight(strings.TrimSpace(baseURL), "/"); trimmed != "" {
		return trimmed
	}
	return strings.TrimRight(strings.TrimSpace(defaultURL), "/")
}

// resolveSpendReadBaseURLForAccount is the session-spend reconciler's
// resolveBaseURL hook (spendReconciler.resolveBaseURL, main.go), lifted out of
// the inline closure so the transparency guarantee it encodes is exercised
// end-to-end by a test that drives THIS production function — not a re-implemented
// copy of it.
//
// It resolves the account for the session's project/role (same precedence as the
// resolveLabels hook), then returns resolveSpendReadBaseURL(acct.BaseURL, "") —
// which reads litellm DIRECTLY at base_url and DELIBERATELY IGNORES acct.ProxyURL
// (claudemore). That omission is the load-bearing half of the claudemore
// transparency property A1 (ka-0000007533): the spend read must never route
// through claudemore, so metering stays byte-identical whether or not a proxy
// fronts the write path. A "" result means the account is unknown (or has no
// base_url) and the reconciler falls back to its global litellmURL.
//
// The write path (resolveAccountBaseURL) DOES honor acct.ProxyURL — so for a
// proxied account write != read, and that divergence is exactly what this
// function must preserve. Passing acct.ProxyURL here (a tempting "unify the
// resolvers" cleanup) would silently break total_cost_usd /
// gardenio_agent_spend_usd_total the moment a proxy is attached, because
// claudemore is a Messages-API proxy and does not serve litellm's /spend/tags.
// See gardenio/docs/claudemore-transparency-proof.md.
func resolveSpendReadBaseURLForAccount(cfg *config.Config, project, role string) string {
	account := resolveSpendAccount(cfg, project, role)
	cfg.ProjectCacheMu.RLock()
	defer cfg.ProjectCacheMu.RUnlock()
	if acct, ok := cfg.AccountCache[account]; ok {
		return resolveSpendReadBaseURL(acct.BaseURL, "")
	}
	return ""
}

// Phase 2 (ka-0000004094, account-routing.md §7) removed resolveNonExhaustedAccount
// and accountFallbackChain from the spawn path. The empty-pool tail they served
// (walk the per-account fallback chain + DefaultAccount, or return the original
// exhausted account with a Warn) is now an explicit SpawnBlock in
// applyAccountConfig: a role-pin / spread / opus-fallback either produces a
// healthy account or the spec is SpawnBlocked. The account exclusivity semantics
// (exclusive_to_role => no spill) are preserved by selectAccount, which
// never offers a reserved account to a non-owner role, and by the escape hatch
// leaving exclusive accounts unblocked so they surface failure on their reserved
// role. The reactive mid-session failover (auth_failover.go findHealthyFallback)
// still walks the per-account Fallback chain and is unaffected.

// isHealthyAuthorized reports whether an account is fit to bake into a pod's env
// — the ONE health predicate the account-selection pipeline gates on. It is the
// exact inverse of the old accountNeedsRedirect gate (renamed for intent in
// Phase 4, ka-0000001577): after the fallback-chain redirect was retired in
// Phase 2 (ka-0000004094) there is no "redirect" left to name, only a health
// filter — selectAccount includes an account in the spread pool iff it is
// healthy-authorized. An account is UNFIT (this returns false) if ANY of:
//
//   - Status == "exhausted": the budget/rate-limit lifecycle (set by the probe)
//     has marked the account spent. The original exhaustion trigger.
//   - BaseURL set but LiteLLMKey empty: the account routes through a LiteLLM
//     proxy but carries no virtual key, so applyAccountConfig cannot emit the
//     `x-litellm-api-key` header. Every agent baked against it gets a 401 from
//     the proxy on its first request and parks idle — the fleet-wide idle-agent
//     buildup in ka-0000003769. The probe leaves Status="" for such an account
//     (it is throttle-allowed, just keyless/out-of-credits), so a
//     Status=="exhausted"-only gate would never exclude it. Treating it as
//     unfit keeps the spread pool on key-present accounts instead of silently
//     401-ing agents.
//   - OverageDisabledReason is a *genuinely blocking* billing reason (see
//     overageReasonBlocks): the account can authenticate (it may have a
//     litellm_key and be throttle-allowed) but cannot bill a successful
//     request, so every call fails on billing. The probe leaves Status="" for
//     these, so they would otherwise stay in the spawn pool and silently fail
//     agents. NOTE: not every overage_disabled_reason blocks — see below.
//   - MaxBudget exceeded (accountOverBudget): the controller-synced spend
//     since the last reset has reached the operator-set per-account budget. The
//     probe only flips Status="exhausted" on the PROVIDER's hard rate-limit
//     ceiling (utilization >= 1.0); this gate trips earlier, at the soft dollar
//     cap the operator wrote on the account bead, so a capped account is
//     excluded BEFORE the provider 429s every agent on turn 1 (ka-0000005505).
//     Opt-in: an account with no max_budget set behaves exactly as before.
//   - ProbeAuthStatus == "rejected": a read-only probe that returned 401/403
//     marks the account terminally auth-dead (revoked token, or org has
//     disabled Claude Code access, ka-0000002085). Such an account can read
//     pool-healthy (Status="", throttle allowed, 5h util=0) yet fail every
//     request, so pool status is NOT sufficient to bake it into a pod or pick it
//     as a rotation switch-target. Excluding it stops the spawn path and the
//     stranded-recovery sweep (both consult this gate via selectAccount) from
//     landing a session on a subscription-dead account — the matthewbaker1110
//     hard error.
//
// A keyless account with no BaseURL is treated as healthy: those use the
// controller's global LiteLLM URL / direct provider auth and are not affected
// by the keyless-proxy gap.
func isHealthyAuthorized(acct config.AccountCacheEntry) bool {
	if acct.Status == "exhausted" {
		return false
	}
	if acct.BaseURL != "" && acct.LiteLLMKey == "" {
		return false
	}
	if overageReasonBlocks(acctBillingType(acct), acct.OverageDisabledReason) {
		return false
	}
	if accountOverBudget(acct) {
		return false
	}
	if acct.ProbeAuthStatus == "rejected" {
		return false
	}
	return true
}

// accountCapacityExhausted reports whether an account is unusable specifically
// because its CAPACITY is spent — the narrow subset of !isHealthyAuthorized that
// the pool-exhaustion escape hatch (ka-0000002560) acts on. It is deliberately
// NARROWER than !isHealthyAuthorized: it excludes the keyless-LiteLLM case
// (base_url set, no litellm_key), which is a misconfiguration the existing
// proceed-with-warn path in applyAccountConfig already surfaces — not a
// transient capacity stall that resolves when a rate-limit window resets. The
// three capacity signals:
//
//   - Status == "exhausted": the probe flipped the budget/rate-limit lifecycle.
//   - overageReasonBlocks: a genuinely-blocking billing reason (NOT the benign
//     subscription-overage reasons — see overageReasonBlocks).
//   - accountOverBudget: the operator-set max_budget has been reached.
//
// When EVERY account a project could resolve to satisfies this predicate, the
// pool is fully exhausted and the escape hatch blocks the spawn rather than
// baking a dead key (the 2026-06-29 monorepo incident). The keyless case is
// intentionally NOT treated as capacity-exhausted so the existing keyless
// behavior (TestApplyAccountConfig_NoLiteLLMKey, ka-0000003769) is unchanged.
func accountCapacityExhausted(acct config.AccountCacheEntry) bool {
	if acct.Status == "exhausted" {
		return true
	}
	if overageReasonBlocks(acctBillingType(acct), acct.OverageDisabledReason) {
		return true
	}
	if accountOverBudget(acct) {
		return true
	}
	return false
}

// earliestPoolResetAt returns the earliest throttle_reset_at (RFC3339) across the
// accounts labeled project:<project> that are capacity-exhausted, for the
// operator-facing "blocking new spawns until <reset>" message the pool-exhaustion
// escape hatch emits (ka-0000002560). RFC3339 timestamps sort lexicographically
// in chronological order for a fixed offset, matching earliestResetAt's string
// comparison. Returns "" when no exhausted pool account carries a reset time.
func earliestPoolResetAt(cfg *config.Config, project string) string {
	if project == "" {
		return ""
	}
	wantLabel := "project:" + project
	var earliest string
	for _, acct := range cfg.AccountCache {
		if !accountLabeledForProject(acct.Labels, wantLabel) {
			continue
		}
		ra := strings.TrimSpace(acct.ThrottleResetAt)
		if ra == "" {
			continue
		}
		if earliest == "" || ra < earliest {
			earliest = ra
		}
	}
	return earliest
}

// accountOverBudget reports whether an account's controller-synced spend since
// its last reset has reached the operator-set per-account budget (max_budget).
//
// This is the proactive, code-fixable half of the "429 kills agents on turn 1"
// finding (ka-0000005505): the controller already syncs total_cost_usd onto
// every account bead every 5 minutes (spend_sync.go), and the account-reset
// path snapshots spend_at_last_reset. Their difference is the billing-period
// spend — the same since_reset figure gb usage prints. When an operator sets
// max_budget below the provider's hard ceiling, this gate marks the account
// needs-redirect once spend reaches it, so the existing fallback chain steers
// new agents onto a sibling account instead of spawning them against a key
// that will 429 on the first inference.
//
// Strictly opt-in and fail-open:
//   - Empty/zero/unparseable max_budget → never over budget (every account
//     today has no max_budget, so behavior is byte-for-byte unchanged).
//   - Unparseable total_cost_usd → treated as zero spend (never blocks on a
//     malformed sync value; a stale/missing cost must not strand a healthy
//     account).
//
// The comparison is >= so an account exactly at its cap is redirected (the next
// request would push it over).
func accountOverBudget(acct config.AccountCacheEntry) bool {
	budget := parseAccountFloat(acct.MaxBudget)
	if budget <= 0 {
		return false
	}
	spent := parseAccountFloat(acct.TotalCostUSD) - parseAccountFloat(acct.SpendAtLastReset)
	if spent < 0 {
		spent = 0
	}
	return spent >= budget
}

// parseAccountFloat parses a numeric account-bead field as float64, returning 0
// for empty or malformed values (fail-open). Mirrors gb's parseFieldFloat.
func parseAccountFloat(s string) float64 {
	s = strings.TrimSpace(s)
	if s == "" {
		return 0
	}
	f, err := strconv.ParseFloat(s, 64)
	if err != nil {
		return 0
	}
	return f
}

// overageReasonBlocks reports whether an account's overage_disabled_reason
// genuinely makes the account unusable for spawning, given how the account is
// BILLED.
//
// The `anthropic-ratelimit-unified-overage-disabled-reason` header describes why
// pay-as-you-go *overage* billing is unavailable. For a SUBSCRIPTION account
// (Claude Max OAuth — every account in our fleet, and the billingType() default)
// that is orthogonal to whether the account can serve: the subscription quota is
// billed by the plan, overage is simply off, and the reason string only names
// which org-level knob turned it off. So for subscription accounts NO overage
// reason blocks a spawn. The real health signals are throttle_status /
// utilization (probeResult.Throttled()) and probe_auth_status, all of which
// isHealthyAuthorized already gates on independently.
//
// Proven empirically (ka-0000004929): a /v1/messages call from the GIB box's
// own egress, with the same token that carries overage-disabled-reason=
// out_of_credits, returns HTTP 200 when the request includes the Claude Code
// system prompt — the account authenticates and bills fine.
//
// Why the old string allowlist was wrong (2026-08-28 fleet outage). This
// predicate used to allow exactly "org_spend_cap_reached" and "out_of_credits"
// and treat every other non-empty reason as blocking, "conservatively". But the
// blocking direction is not the conservative one: excluding an account is what
// empties the pool. Anthropic then began reporting "org_level_disabled" /
// "org_level_disabled_until" — an org admin having switched extra usage off,
// which is the fleet's steady state — and the two accounts still holding
// capacity (fimbaz and matthewbaker1110, both throttle_status=allowed with
// utilization_5h=0) were dropped from every project pool while the other five
// were genuinely 7-day exhausted. applyAccountConfig then SpawnBlocked every
// agent fleet-wide, and because the reason string never changes on its own the
// block would not have self-healed at any window reset. That is the same failure
// as ka-0000003769 and ka-0000004929, twice re-litigated one string at a time;
// keying on billing type instead of an enumerable reason retires the class.
//
// For a METERED (pay-as-you-go) account the header IS load-bearing — no credits
// means requests genuinely fail — so the conservative default is kept there:
// anything but the empty reason blocks. This mirrors overageOutOfCredits, which
// already gates the probe's reactive exhausted-hold on billing type.
func overageReasonBlocks(billing, reason string) bool {
	if reason == "" {
		return false
	}
	if billing != billingTypeMetered {
		// Subscription (or unset, which defaults to subscription): overage
		// availability says nothing about the plan quota. Never blocking.
		return false
	}
	// Metered: overage IS the billing path. Any disabled reason blocks.
	return true
}

// acctBillingType is billingType() for a cache entry: the account's billing_type
// with the same subscription default.
func acctBillingType(acct config.AccountCacheEntry) string {
	if acct.BillingType != "" {
		return acct.BillingType
	}
	return billingTypeSubscription
}

// roleMatchesExclusive reports whether a (possibly comma-separated) agent role
// string includes the account's exclusive_to_role — i.e. the agent is the
// authorized owner of that exclusive account.
func roleMatchesExclusive(role, exclusiveRole string) bool {
	exclusiveRole = strings.TrimSpace(exclusiveRole)
	if exclusiveRole == "" {
		return false
	}
	for _, r := range parseRoleList(role) {
		if r == exclusiveRole {
			return true
		}
	}
	return false
}

// roleOwnsExclusiveAccount reports whether some account in the cache is
// exclusive to (one of) the given role(s). Such a role is pinned to its own
// exclusive account and must never be spread across the general project pool —
// the data-driven replacement for the old fsm-triage "never spread" carve-out.
func roleOwnsExclusiveAccount(cfg *config.Config, role string) bool {
	for _, acct := range cfg.AccountCache {
		if roleMatchesExclusive(role, acct.ExclusiveToRole) {
			return true
		}
	}
	return false
}

// resolveRoleAccountWhitelist returns the union of account_whitelist entries
// across all roles in a (possibly comma-separated) role string. Returns nil
// when no role declares a whitelist. The union is the correct combination for a
// multi-role agent: each role independently authorizes its listed accounts, so
// the agent may use any account allowed by any of its roles.
//
// This is the controller-side counterpart to gb-init's lookupRoleAccountWhitelist.
// Once the controller pins BOAT_ACCOUNT (ka-0000002509 / MR !4740), gb-init's
// whitelist filter early-returns and never runs, so enforcement must live here
// on the spawn path (ka-0000003061).
func resolveRoleAccountWhitelist(cfg *config.Config, role string) []string {
	if role == "" || len(cfg.RoleCache) == 0 {
		return nil
	}
	cfg.RoleCacheMu.RLock()
	defer cfg.RoleCacheMu.RUnlock()
	var union []string
	seen := make(map[string]struct{})
	for _, r := range parseRoleList(role) {
		entry, ok := cfg.RoleCache[r]
		if !ok {
			continue
		}
		for _, a := range entry.AccountWhitelist {
			if _, dup := seen[a]; dup {
				continue
			}
			seen[a] = struct{}{}
			union = append(union, a)
		}
	}
	return union
}

// accountAllowedByRoleWhitelist reports whether account is permitted for the
// given role's whitelist. An empty/absent whitelist imposes no restriction
// (returns true). A non-empty whitelist that does not contain account is
// fail-closed (returns false) — a role pinned to a restricted pool must never
// be credentialed against an account outside it.
func accountAllowedByRoleWhitelist(cfg *config.Config, role, account string) bool {
	whitelist := resolveRoleAccountWhitelist(cfg, role)
	if len(whitelist) == 0 {
		return true
	}
	for _, a := range whitelist {
		if a == account {
			return true
		}
	}
	return false
}

// resolveRoleAccount returns the account name pinned by the agent's role, if
// any. For multi-role agents (comma-separated spec.Role), the first role with
// a non-empty Account wins. Returns "" when no role pins an account.
func resolveRoleAccount(cfg *config.Config, role string) string {
	if role == "" || len(cfg.RoleCache) == 0 {
		return ""
	}
	cfg.RoleCacheMu.RLock()
	defer cfg.RoleCacheMu.RUnlock()
	for _, r := range parseRoleList(role) {
		if entry, ok := cfg.RoleCache[r]; ok && entry.Account != "" {
			return entry.Account
		}
	}
	return ""
}

// roleAccountPin is the role's pinned account (resolveRoleAccount), or "" for
// a tenant land: its pods bill the land's own lands-<land> account, never one
// a fleet role pins (Lands Track B §5.3). The caller holds ProjectCacheMu.
func roleAccountPin(cfg *config.Config, project, role string) string {
	if entry, ok := cfg.ProjectCache[project]; ok && entry.IsTenantLand() {
		return ""
	}
	return resolveRoleAccount(cfg, role)
}

// roleAllowsAccountSpill reports whether a role permits spilling to the
// project's healthy spread pool when its pinned account is capacity-exhausted at
// spawn time (ka-0000001307). The default is TRUE (spill allowed) — the safer
// behavior that keeps a pinned lane running on a healthy sibling account instead
// of parking it queued indefinitely until the pinned account's rate window
// resets (which can be days). A role opts OUT by setting no_account_spill=true
// on its role bead, for a lane that MUST stay on its pinned account (e.g. a
// cost/model-locked lane where landing on a different account would be wrong).
//
// For a multi-role agent (comma-separated spec.Role) spill is refused if ANY
// role opts out — fail-closed toward respecting the strictest pin, mirroring how
// resolveRoleAccount lets the first pinning role win. An unknown role (no role
// bead) imposes no opt-out.
func roleAllowsAccountSpill(cfg *config.Config, role string) bool {
	if role == "" || len(cfg.RoleCache) == 0 {
		return true
	}
	cfg.RoleCacheMu.RLock()
	defer cfg.RoleCacheMu.RUnlock()
	for _, r := range parseRoleList(role) {
		if entry, ok := cfg.RoleCache[r]; ok && entry.NoAccountSpill {
			return false
		}
	}
	return true
}

// resolveSpendAccount resolves the billing account for a session from its
// project and role, mirroring applyAccountConfig's precedence: a role-pinned
// account wins, then the project's account. Shared by the per-turn spend
// forwarder and the session-close spend reconciler so the `account` label on
// gardenio_agent_spend_usd_total is resolved identically on both paths. Returns
// "" when neither a role pin nor a project-cache account resolves (the terminal
// DefaultAccount fall-through was removed in account-routing.md §7 Phase 3,
// ka-0000007959; it was unset in production and Phase 0 telemetry proved the
// layer never won).
func resolveSpendAccount(cfg *config.Config, project, role string) string {
	if a := resolveRoleAccount(cfg, role); a != "" {
		return a
	}
	cfg.ProjectCacheMu.RLock()
	entry, ok := cfg.ProjectCache[project]
	cfg.ProjectCacheMu.RUnlock()
	if ok && entry.Account != "" {
		return entry.Account
	}
	return ""
}

// applySlackViewSession mounts the Slack test user's session Secret (optional
// volume) into pods whose role is on cfg.SlackViewSessionRoles, so those agents
// can screenshot Slack with `gb slack view`. It sets no env: the agent
// entrypoint exports SLACK_VIEW_STORAGE_STATE only when the file is present.
func applySlackViewSession(cfg *config.Config, spec *podmanager.AgentPodSpec) {
	if cfg.SlackViewSessionSecret == "" || spec.Role == "" {
		return
	}
	allowed := parseRoleList(cfg.SlackViewSessionRoles)
	for _, role := range parseRoleList(spec.Role) {
		if slices.Contains(allowed, role) {
			spec.SlackViewSessionSecret = cfg.SlackViewSessionSecret
			return
		}
	}
}
