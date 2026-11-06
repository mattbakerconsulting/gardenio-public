// Write-path shape validation for type:project and type:role config beads.
//
// PROBLEM (ka-0000006052, friction audit ka-0000003121): the daemon's generic
// FieldDef validation only enforces "is this field valid JSON" for the
// structured-JSON fields on project/role beads (secrets, repos, env, …). It
// does NOT enforce the concrete Go-struct SHAPE the controller parses them
// into. A wrong-shaped blob — an object where an array is expected, a
// string-into-int, truncated JSON — passes `kd create`/`kd update`, then is
// SILENTLY DROPPED by the controller parser at pod-spawn time (the parser uses
// `if json.Unmarshal(...) == nil { ... }`, so a shape mismatch reads as empty
// with no error). The failure surfaces far from its cause: an agent spawns
// with no secrets / no repos and nobody knows why.
//
// FIX: lift the EXISTING read-path validity detectors
// (gardenio/controller/cmd/beads-viewer/{project,role}_validity.go) into the
// daemon WRITE path so a malformed project/role bead is rejected (hard 400) or
// warned (soft, logged) at write time — before it is ever persisted.
//
// CROSS-MODULE NOTE: the detectors live in the gardenio/controller module; this
// is the kbeads module. We do NOT import the controller — instead the minimal
// shape-validation logic is PORTED here, self-contained, mirroring the existing
// formula/schedule/doc validators (runaway_validation.go, bundles.go). The
// expected struct shapes are reproduced from
// gardenio/controller/internal/beadsapi/projects.go and roles.go.
//
// SEVERITY MODEL (mirrors decision_validation.go):
//   - HARD error (HTTP 400): a structured-JSON field that does not decode into
//     its expected Go type. This is the class the controller silently drops, so
//     rejecting at write time is strictly safer — a legacy bead that already
//     parses cleanly is unaffected.
//   - SOFT warning (logged via slog, never blocks the write): a missing git_url
//     on a project bead, and a secret name that lacks the "<project>-" /
//     "<role>-" prefix that the secretreconciler enforces
//     (gardenio/controller/internal/secretreconciler/reconciler.go) — the
//     controller skips such secrets at reconcile time, so they would silently
//     never materialize. These are warnings (not hard rejects) because (a)
//     git_url is legitimately omitted by some beads (multi-repo via repos[],
//     in-cluster-only projects) and the type:project schema deliberately does
//     NOT mark it required so legacy beads are not wedged (config.go §5.2), and
//     (b) the prefix rule is a convention the operator may be mid-migration on.
package server

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"strconv"
	"strings"
)

// --- Ported struct shapes -------------------------------------------------
//
// These mirror the Go types the controller parser expects, reproduced from
// gardenio/controller/internal/beadsapi/{projects,roles}.go. Only the shape
// matters for validation; we do not use the decoded values (except secret
// names for the prefix check), so unused fields are kept for documentation /
// shape fidelity and silenced from linters via the json tags.

// projSecretEntry mirrors beadsapi.SecretEntry — a {env, secret, key} tuple
// mapping a K8s Secret key to a pod env var.
type projSecretEntry struct {
	Env    string `json:"env"`
	Secret string `json:"secret"`
	Key    string `json:"key"`
}

// projEnvEntry mirrors beadsapi.EnvEntry — a {name, value} plain env var.
type projEnvEntry struct {
	Name  string `json:"name"`
	Value string `json:"value"`
}

// projRepoEntry mirrors beadsapi.RepoEntry — a repo to clone into the workspace.
type projRepoEntry struct {
	URL    string `json:"url"`
	Branch string `json:"branch,omitempty"`
	Role   string `json:"role,omitempty"`
	Name   string `json:"name,omitempty"`
}

// projResourceMappingEntry mirrors beadsapi.ResourceMappingEntry.
type projResourceMappingEntry struct {
	Env        string `json:"env"`
	Cluster    string `json:"cluster"`
	Service    string `json:"service"`
	DBInstance string `json:"db_instance"`
}

// projPrewarmedPoolConfig mirrors beadsapi.PrewarmedPoolConfig (+ nested
// PrewarmedRolePool). Only the shape is validated.
type projPrewarmedPoolConfig struct {
	Enabled bool                    `json:"enabled"`
	MinSize int                     `json:"min_size"`
	MaxSize int                     `json:"max_size"`
	Role    string                  `json:"role"`
	Mode    string                  `json:"mode"`
	Roles   []projPrewarmedRolePool `json:"roles,omitempty"`
}

type projPrewarmedRolePool struct {
	Role    string `json:"role"`
	Mode    string `json:"mode,omitempty"`
	MinSize int    `json:"min_size,omitempty"`
	MaxSize int    `json:"max_size,omitempty"`
}

// --- Field → expected-shape registry -------------------------------------

// projectShapeChecks maps each structured-JSON field on a type:project bead to
// a parser that decodes the raw value into the controller's expected Go type.
// A non-empty value that fails to decode is the silent-drop class we reject.
// Mirrors buildProjectValidityReport's shapeFields list (project_validity.go).
var projectShapeChecks = []jsonFieldShape{
	{"secrets", shapeParse[[]projSecretEntry]},
	{"env", shapeParse[[]projEnvEntry]},
	{"repos", shapeParse[[]projRepoEntry]},
	{"resource_mapping", shapeParse[[]projResourceMappingEntry]},
	{"prewarmed_pool", shapeParse[projPrewarmedPoolConfig]},
	{"channel_roles", shapeParse[map[string]string]},
	{"channel_modes", shapeParse[map[string]string]},
	{"channel_runtime_class", shapeParse[map[string]string]},
	{"claude_config", shapeParse[map[string]json.RawMessage]},
	{"env_json", shapeParse[map[string]string]},
}

// roleShapeChecks maps each structured-JSON field on a type:role bead to its
// expected Go type. Mirrors buildRoleValidityReport's shapeFields list
// (role_validity.go).
var roleShapeChecks = []jsonFieldShape{
	{"secrets", shapeParse[[]projSecretEntry]},
	{"env", shapeParse[[]projEnvEntry]},
	{"env_json", shapeParse[map[string]string]},
	{"claude_config", shapeParse[map[string]json.RawMessage]},
}

// jsonFieldShape names a JSON bead field and the parser that round-trips its
// raw value through the expected Go type to detect a silent shape mismatch.
type jsonFieldShape struct {
	name  string
	parse func(raw string) error
}

// shapeParse decodes raw into a value of type T, mirroring EXACTLY the
// controller parser's lax json.Unmarshal (beadsapi unmarshalProjectField). It
// deliberately does NOT use DisallowUnknownFields — the controller tolerates
// unknown keys, so flagging them here would reject a config that actually
// works. We surface only what the controller genuinely drops: a fundamental
// shape mismatch (object vs array, string-into-int, truncated JSON) that makes
// json.Unmarshal itself return an error. Trailing garbage after the first
// value is rejected because json.Unmarshal also rejects it. Copied from
// project_validity.go shapeUnmarshal so the write-path and read-path verdicts
// match byte-for-byte.
func shapeParse[T any](raw string) error {
	var v T
	dec := json.NewDecoder(strings.NewReader(raw))
	if err := dec.Decode(&v); err != nil {
		return err
	}
	if dec.More() {
		return fmt.Errorf("unexpected trailing data after JSON value")
	}
	return nil
}

// --- Validation result ----------------------------------------------------

// projectRoleValidationResult is the outcome of validating a project/role
// bead's structured fields. RejectReason is non-empty when the bead must be
// rejected (HTTP 400); Warnings collects soft, non-blocking advisories. Mirrors
// the RejectReason/Warning split in decisionValidationResult.
type projectRoleValidationResult struct {
	RejectReason string
	Warnings     []string
}

// validateProjectFields validates a type:project bead's raw fields on the
// CREATE path. name is the resolved project name (bead title), used for the
// secret-name prefix warning; pass "" to skip that check. The result carries a
// hard RejectReason for any malformed structured-JSON field and soft Warnings
// for a missing git_url or a mis-prefixed secret name.
func validateProjectFields(fields json.RawMessage, name string) projectRoleValidationResult {
	return validateProjectFieldsImpl(fields, nil, name)
}

// validateProjectFieldsForUpdate is the UPDATE-path variant. prior is the
// bead's persisted fields blob BEFORE this patch was merged in. A structured-
// JSON field is hard-rejected only when its (normalized) raw value DIFFERS from
// its prior stored value — a malformed field that is byte-identical to what was
// already persisted is grandfathered (skipped), so a legacy bead carrying a
// malformed structured field is not wedged against unrelated-field PATCHes.
// This mirrors the grandfather semantics of ValidateTypeFieldsForUpdate
// (beads.go / validate_fields.go), which exists precisely so a value the daemon
// no longer accepts "must not wedge subsequent PATCHes" (ka-0000007952). The
// create-time guarantee is preserved: a NEWLY-introduced or CHANGED malformed
// field is still rejected.
func validateProjectFieldsForUpdate(fields, prior json.RawMessage, name string) projectRoleValidationResult {
	return validateProjectFieldsImpl(fields, parseFieldsToStringMap(prior), name)
}

func validateProjectFieldsImpl(fields json.RawMessage, prior map[string]string, name string) projectRoleValidationResult {
	var res projectRoleValidationResult
	if len(fields) == 0 {
		return res
	}
	f := parseFieldsToStringMap(fields)
	if f == nil {
		// Whole-object JSON is unparseable. Generic ValidateTypeFields already
		// rejects this upstream; defensive no-op (return clean).
		return res
	}

	// HARD: structured-JSON shape checks.
	if reason := firstShapeError(f, prior, projectShapeChecks); reason != "" {
		res.RejectReason = reason
		return res
	}
	// HARD: the release source (releases.go). A tag pattern that does not
	// compile or a target list the deploy route cannot read would refuse every
	// release of the project; checked only when the value changes.
	if v := f["release_tag_pattern"]; prior == nil || v != prior["release_tag_pattern"] {
		if reason := releaseTagPatternError(v); reason != "" {
			res.RejectReason = reason
			return res
		}
	}
	if v := f["release_targets"]; strings.TrimSpace(v) != "" && (prior == nil || v != prior["release_targets"]) {
		if reason := releaseTargetsShapeError(v); reason != "" {
			res.RejectReason = reason
			return res
		}
	}

	// SOFT: git_url presence. A project with no git_url and no repos[] will
	// clone nothing — almost always a mistake, but legitimately omitted by
	// in-cluster-only projects, so warn rather than reject.
	if strings.TrimSpace(f["git_url"]) == "" {
		res.Warnings = append(res.Warnings,
			"git_url is empty — agents in this project will have no primary repo to clone "+
				"(set git_url, or declare repos[] for a multi-repo project)")
	}

	// SOFT: secret-name prefix. The secretreconciler skips any project secret
	// whose K8s Secret name does not start with "<project>-", so a mis-prefixed
	// secret silently never materializes (reconciler.go). The expected prefix is
	// derived from the project NAME, which the controller resolves by stripping
	// the legacy "Project: " title prefix (beadsapi/projects.go:325) — mirror
	// that here so a bead titled "Project: gardenio" checks against "gardenio-",
	// not "Project: gardenio-".
	res.Warnings = append(res.Warnings,
		secretPrefixWarnings(f["secrets"], projectNameFromTitle(name), "project")...)

	return res
}

// validateRoleFields validates a type:role bead's raw fields on the CREATE
// path. name is the resolved role name (bead title), used for the secret-name
// prefix warning; pass "" to skip that check. Role beads carry no git_url, so
// only the structured-JSON shape check and the secret-prefix warning apply.
func validateRoleFields(fields json.RawMessage, name string) projectRoleValidationResult {
	return validateRoleFieldsImpl(fields, nil, name)
}

// validateRoleFieldsForUpdate is the UPDATE-path variant; see
// validateProjectFieldsForUpdate for the grandfather rationale. prior is the
// bead's persisted fields blob before this patch.
func validateRoleFieldsForUpdate(fields, prior json.RawMessage, name string) projectRoleValidationResult {
	return validateRoleFieldsImpl(fields, parseFieldsToStringMap(prior), name)
}

func validateRoleFieldsImpl(fields json.RawMessage, prior map[string]string, name string) projectRoleValidationResult {
	var res projectRoleValidationResult
	if len(fields) == 0 {
		return res
	}
	f := parseFieldsToStringMap(fields)
	if f == nil {
		return res
	}

	// HARD: structured-JSON shape checks.
	if reason := firstShapeError(f, prior, roleShapeChecks); reason != "" {
		res.RejectReason = reason
		return res
	}

	// HARD: privilege-gate — a role that grants elevated runtime capability
	// must say so via privileged=true. Deliberately NOT grandfathered by prior
	// value (unlike firstShapeError above): this is a privilege-escalation
	// class check, not a silent-drop-shape class check, so an existing role
	// bead that already carries an ungated privileged field is rejected on its
	// next write until it is fixed, rather than permanently exempted.
	if reason := rolePrivilegeError(f); reason != "" {
		res.RejectReason = reason
		return res
	}

	// HARD: ownership_grants is an authority grant, so a value the ownership
	// gate would read as no grant is refused rather than stored silently
	// (parseRoleOwnershipGrants).
	if _, err := parseRoleOwnershipGrants(f[roleOwnershipGrantsField]); err != nil {
		res.RejectReason = fmt.Sprintf("field %q: %v", roleOwnershipGrantsField, err)
		return res
	}

	// SOFT: secret-name prefix ("<role>-").
	res.Warnings = append(res.Warnings, secretPrefixWarnings(f["secrets"], name, "role")...)

	// SOFT: a token-mounting role with no dedicated ServiceAccount falls back to
	// the fleet-shared one. Not a hard reject: existing live roles have no
	// service_account until their SA lands, and every write would 400.
	if isTruthyField(f["needs_k8s_api"]) && strings.TrimSpace(f["service_account"]) == "" {
		res.Warnings = append(res.Warnings,
			"role sets needs_k8s_api without service_account; it will run as the fleet-shared "+
				"ServiceAccount — set a dedicated service_account (gd-KRHGbBJqRN)")
	}

	return res
}

// sharedAgentServiceAccount is the fleet-shared ServiceAccount every agent pod
// runs as by default. Mirrors sharedAgentServiceAccount in
// gardenio/controller/cmd/controller/podspec.go.
const sharedAgentServiceAccount = "gardenio-agent"

// privilegedRoleFields are the type:role fields that grant elevated runtime
// capability — cluster API access, a named ServiceAccount, a ziti bind
// service, Docker-in-docker, or a machine client's secret (Task 15 C5). A role bead may only set one of these when
// privileged=true, so a role's blast radius is legible from the bead itself
// (gd-qbBJGHiGHo, gd-5qSNRn14ra).
var privilegedRoleFields = []string{"needs_k8s_api", "service_account", "ziti_bind", "docker", "machine_client"}

// defaultExecutionTargets are execution_target values that carry no elevated
// capability — the default in-cluster Kubernetes agent-pod path. Mirrors
// podmanager.IsKubernetesTarget ("" / ExecutionTargetKubernetes;
// gardenio/controller/internal/podmanager/manager.go). Note "probe" is
// deliberately NOT here: like lambda_microvm it selects a different backend,
// so it is gated.
var defaultExecutionTargets = map[string]bool{
	"":           true,
	"kubernetes": true,
}

// rolePrivilegeError returns a hard reject reason when a type:role bead sets
// any privilege-bearing field (privilegedRoleFields, or a non-default
// execution_target) without also setting privileged=true, or "" when the
// bead is consistent. f is the role's merged field map (parseFieldsToStringMap).
func rolePrivilegeError(f map[string]string) string {
	if isTruthyField(f["needs_k8s_api"]) && strings.TrimSpace(f["service_account"]) == sharedAgentServiceAccount {
		return fmt.Sprintf(
			"role sets needs_k8s_api with service_account=%q, the fleet-shared ServiceAccount; "+
				"a role that mounts an API token needs its own dedicated service_account "+
				"(e.g. gc-k8s-live) so its blast radius is not shared with every agent pod",
			sharedAgentServiceAccount)
	}
	privileged, _ := strconv.ParseBool(f["privileged"])
	if privileged {
		return ""
	}
	for _, name := range privilegedRoleFields {
		if isTruthyField(f[name]) {
			return fmt.Sprintf(
				"role sets %q, which grants elevated runtime capability and requires "+
					"`privileged: true` — set -f privileged=true or drop %q", name, name)
		}
	}
	if et := strings.TrimSpace(f["execution_target"]); et != "" && !defaultExecutionTargets[et] {
		return fmt.Sprintf(
			"role sets execution_target=%q, a non-default execution backend, which requires "+
				"`privileged: true` — set -f privileged=true or drop execution_target", et)
	}
	return ""
}

// isTruthyField reports whether a role field's string value should be
// treated as "set" for the privilege-gate check. Boolean fields are stored
// as "true"/"false" text (parseFieldsToStringMap); a string field like
// service_account is gated whenever it carries any non-blank value.
func isTruthyField(raw string) bool {
	if strings.TrimSpace(raw) == "" {
		return false
	}
	if b, err := strconv.ParseBool(raw); err == nil {
		return b
	}
	return true
}

// projectNameFromTitle resolves a project's canonical name from its bead
// title, mirroring ListProjectBeads (beadsapi/projects.go:325): legacy project
// beads carry titles like "Project: beads" rather than the bare name "beads".
// The controller strips that prefix before everything downstream (including the
// secret-prefix convention), so the write-path validator must too — otherwise a
// correctly-prefixed secret on a "Project: <name>" bead would spuriously warn.
func projectNameFromTitle(title string) string {
	return strings.TrimPrefix(title, "Project: ")
}

// firstShapeError returns a user-facing reject reason for the first
// structured-JSON field that fails its shape check, or "" if all are valid /
// absent. Deterministic: checks run in the fixed registry order.
//
// prior is the bead's persisted field map before the current write (nil on the
// create path). A malformed field whose normalized raw value is byte-identical
// to its prior stored value is GRANDFATHERED — skipped — so an update that does
// not touch that field is not wedged by a malformed value that was accepted
// under the old (shape-unaware) rules. A field that is newly introduced or
// whose value changed is always shape-checked.
func firstShapeError(f, prior map[string]string, checks []jsonFieldShape) string {
	for _, c := range checks {
		raw := f[c.name]
		if strings.TrimSpace(raw) == "" {
			continue // absent/empty is valid — every structured field is optional
		}
		if prior != nil && raw == prior[c.name] {
			continue // unchanged from the prior stored value — grandfather it
		}
		if err := c.parse(raw); err != nil {
			return fmt.Sprintf(
				"field %q is malformed JSON for its expected shape (the controller would "+
					"silently drop it at pod spawn): %v", c.name, err)
		}
	}
	return ""
}

// logProjectRoleWarnings emits soft validation advisories to the server log.
// Warnings never block the write (mirrors decision_validation.go's handling of
// a soft Warning), but surfacing them at write time means a mis-shaped project/
// role bead is flagged at its CAUSE rather than silently at pod-spawn. scope is
// "project" or "role"; name is the bead title. No-op when warnings is empty.
func logProjectRoleWarnings(ctx context.Context, scope, name string, warnings []string) {
	for _, w := range warnings {
		slog.Warn("project/role config bead created with validation warning",
			"actor", actorFromContext(ctx),
			"scope", scope,
			"name", name,
			"warning", w,
		)
	}
}

// secretPrefixWarnings returns one soft warning per secret entry whose K8s
// Secret name lacks the required "<name>-" prefix. scope is "project" or "role"
// (for the message). Returns nil when name is empty, secrets is empty, or the
// secrets blob is malformed (the shape check already covers malformed JSON).
func secretPrefixWarnings(secretsRaw, name, scope string) []string {
	if name == "" || strings.TrimSpace(secretsRaw) == "" {
		return nil
	}
	var entries []projSecretEntry
	if err := json.Unmarshal([]byte(secretsRaw), &entries); err != nil {
		return nil // malformed shape is a hard error elsewhere; don't double-report
	}
	prefix := name + "-"
	var warnings []string
	for _, e := range entries {
		if e.Secret == "" {
			continue
		}
		if !strings.HasPrefix(e.Secret, prefix) {
			warnings = append(warnings, fmt.Sprintf(
				"secret %q does not start with the required %s prefix %q — the secret "+
					"reconciler will skip it and it will never materialize",
				e.Secret, scope, prefix))
		}
	}
	return warnings
}

// parseFieldsToStringMap normalizes a raw fields object into a string map,
// re-marshaling complex (array/object) values back to their JSON text. This
// mirrors the controller's ParseFieldsJSON (beadsapi/client.go) so the
// write-path validator sees fields exactly as the controller read path does —
// regardless of whether a structured field was stored as a nested JSON value
// (e.g. {"secrets":[...]}) or as a string-of-JSON (e.g. {"secrets":"[...]"}).
// Returns nil when the whole fields object is not a JSON object.
func parseFieldsToStringMap(raw json.RawMessage) map[string]string {
	if len(raw) == 0 {
		return map[string]string{}
	}
	// Fast path: already a flat string map.
	m := map[string]string{}
	if err := json.Unmarshal(raw, &m); err == nil {
		return m
	}
	// Fallback: decode as map[string]any and re-marshal complex values to text.
	var anyMap map[string]any
	if err := json.Unmarshal(raw, &anyMap); err != nil {
		return nil
	}
	out := make(map[string]string, len(anyMap))
	for k, v := range anyMap {
		switch vv := v.(type) {
		case string:
			out[k] = vv
		default:
			if bs, err := json.Marshal(vv); err == nil {
				out[k] = string(bs)
			} else {
				out[k] = fmt.Sprintf("%v", vv)
			}
		}
	}
	return out
}
