package server

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"reflect"
	"strings"
	"time"

	"gardenio/kbeads/internal/model"
)

// Spawn-lineage seal (gd-is1H5NmI05, operator BLOCK on PR #426).
//
// The trigger-lineage spawn gate (spawn_lineage_authz.go) lets the
// trigger-engine spawn a privileged role without an escalation when an admin
// stands behind the trigger. The first cut read "who stands behind it" from the
// trigger's operated_by / created_by, and both are attacker-influenced: the
// static token and fleet tokens may relay X-Operated-By and X-Actor, an agent's
// writes are attributed to the human who spawned it, and a non-OIDC caller can
// rewrite an admin's trigger (the static token skips ownership on triggers).
//
// So the gate no longer trusts attribution at all. It trusts a SEAL that the
// server stamps on a type:trigger / type:schedule bead only when an OIDC session
// holding gardenio-admin wrote it:
//
//		fields.spawn_lineage = {"v":1,"author":"oidc:<email>","digest":"<sha256>","iat":<unix>}
//
//	  - author is the authenticated OIDC subject of the writing session, taken from
//	    the verified JWT, never from a header or field.
//	  - digest covers the spawn-relevant fields (spawnLineageDigestKeys), so ANY
//	    later change to them by anyone, however authenticated, makes the seal stale
//	    and the gate refuses. No write path has to be audited for "did it touch the
//	    role?": the comparison is against the stored content.
//	  - iat is when the server stamped it; the gate refuses a seal older than the
//	    seal TTL (spawnLineageSealTTL), so a demoted admin's triggers stop
//	    qualifying without the daemon holding any Keycloak credential.
//	  - the seal key itself is server-owned. Nobody can write it through create,
//	    update, the single-field mutation endpoints, the merge on close, or
//	    mergeBeadFields (spawnLineageSealDenial), so it cannot be forged or copied
//	    from another trigger.
//
// Opening a trigger back up is also an admin act. Content equality cannot see
// it (status and enabled are not spawn content, and "restore the old value"
// would re-validate the old digest), so updateBead strips the seal outright when
// anything but an OIDC gardenio-admin session reopens a closed trigger or turns
// a disabled one on (voidSpawnLineageOnReactivation). Disabling and closing
// only remove authority and leave the seal alone.
//
// An admin update re-stamps the seal only when the bead was already validly
// sealed before the patch (the admin is continuing their own work), when the
// admin passes the explicit control key reseal_spawn_lineage=true to adopt the
// current content, or when the admin's write is itself the reopen or re-enable
// (reactivates). Turning a trigger on is the admin saying "run this": the
// owner's enable click or Approve & build on a gardener a thread minted seals it
// (gd-RL1uPwUqAF, folded into gd-YaoqmhIx8x). Without that rule a one-field
// admin edit would silently bless whatever a non-admin had rewritten in the
// meantime. Existing triggers are unsealed until an admin reseals them.
//
// A plan's gardener bound by a thread after the owner approved the plan is
// sealed to that owner's answer (spawn_lineage_plan_approval.go).
const (
	spawnLineageField     = "spawn_lineage"
	spawnLineageResealKey = "reseal_spawn_lineage"
	// spawnLineageExpectedDigestKey is the companion control key `gb trigger
	// reseal` sends with reseal_spawn_lineage: the digest of the content the admin
	// was shown. The server refuses the reseal when the bead's spawn-relevant
	// content no longer hashes to it (a static-token agent rewrote the prompt
	// between the admin's read and the write). Like the reseal key, it is consumed
	// and never stored.
	spawnLineageExpectedDigestKey = "reseal_expected_digest"
	spawnLineageSealVersion       = 1
)

// defaultSpawnLineageSealTTL is how long a seal stays honoured. An admin who
// loses gardenio-admin stops being able to author new seals at once; the seals
// they already made stop working at most this long afterwards. Any OIDC admin
// write to a sealed trigger re-stamps it, and `kd update <id> -f
// reseal_spawn_lineage=true` renews one without changing it.
const defaultSpawnLineageSealTTL = 30 * 24 * time.Hour

// spawnLineageClockSkew is how far in the future a seal's iat may be.
const spawnLineageClockSkew = 5 * time.Minute

// spawnLineageDigestKeys are the trigger/schedule fields that decide WHICH
// agent is spawned, in which project, with what instructions, when, and how
// often. Observability stamps the engine writes on every cycle (last_checked,
// last_run, runs_spawned, ...) are deliberately outside the digest so normal
// operation does not void a seal. `enabled` and the open/closed status are not
// content either; voidSpawnLineageOnReactivation handles those.
var spawnLineageDigestKeys = []string{
	// what runs
	"action", "role", "project", "prompt", "name",
	"formula_id", "formula_vars", "cross_pour_upstream_step",
	"triggers", "chain", "captain",
	"assign_bead", "execution_target", "terminal_goal", "exit_condition", "on_exit",
	"bead_type", "bead_priority", "parent", "extra_labels",
	// when it fires
	"source", "condition_type", "field", "op", "threshold", "match",
	"cron", "timed_cron", "timezone", "trigger_mode",
	"conditions", "assert", "on_violation", "blocked_by",
	"jql", "jira_project", "project_path", "event_type", "event_types", "state",
	"gitlab_labels", "author", "target_branch",
	"pipeline_ref", "pipeline_ref_pattern", "pipeline_status",
	"account_filter", "channel_filter", "emoji", "allow_unsigned",
	"watch_bead_id", "watch_channel", "watch_from", "watch_labels",
	"watch_project", "watch_role", "watch_types",
	// how often / how many
	"max_concurrent", "max_concurrent_runs", "cooldown_minutes", "max_runs",
	"max_retries", "retry_on_failure", "run_timeout", "timeout",
	"allowed_roles", "allowed_projects",
	// where it reports
	"slack_channel", "slack_user_id", "user_bead_id",
}

// spawnLineageGardenerConfigKeys are the keys of the `gardener` blob that an
// author configures. Everything else in the blob is engine-owned runtime state
// rewritten on every fire (runs_spawned, runs_completed, run_budget,
// consecutive_failures, last_agent_id, lineage_root, run_log, progress_state,
// resumes_without_progress, last_resume_*, second_life_count, dormant_*,
// runs_finalized_after, wall_clock_since, last_checked_agent, force_close, ...).
// Hashing those would void the seal after the first spawn. It is an allow-list:
// a key added to the blob later is runtime until someone decides otherwise,
// which fails towards a still-valid seal, so a new CONFIG key must be added
// here. run_budget is excluded because the engine tops it up (it is bounded by
// the digested max_runs and resume_budget).
var spawnLineageGardenerConfigKeys = []string{
	"name", "terminal_goal", "exit_condition", "on_exit", "standing",
	"stop", "progress", "max_runs", "retry_on_failure", "max_retries",
	"run_timeout", "timeout", "resume_budget", "run_budget_refill",
	"max_second_lives", "agent_daily_budget_usd", "one_spawn_at_a_time",
	"max_runs_per_day", "assign_bead", "slack_channel", "slack_user_id", "user_bead_id",
	"watch_project", "watch_role", "watch_bead_id", "watch_labels",
	"watch_types", "watch_channel", "watch_from", "trigger_mode",
}

// gardenerConfigOnly reduces the `gardener` field (a JSON object, or the JSON
// string of one) to its configuration keys. A value that does not parse is kept
// whole, so garbage still changes the digest.
func gardenerConfigOnly(v any) any {
	var blob map[string]any
	switch g := v.(type) {
	case map[string]any:
		blob = g
	case string:
		if json.Unmarshal([]byte(g), &blob) != nil {
			return v
		}
	default:
		return v
	}
	out := make(map[string]any, len(spawnLineageGardenerConfigKeys))
	for _, k := range spawnLineageGardenerConfigKeys {
		if val, ok := blob[k]; ok && val != nil {
			out[k] = val
		}
	}
	return out
}

type spawnLineageSeal struct {
	V      int    `json:"v"`
	Author string `json:"author"`
	Digest string `json:"digest"`
	IAT    int64  `json:"iat"`
}

// spawnLineageContent is the spawn-relevant content a seal covers: the
// spawnLineageDigestKeys fields present in fields, plus the configuration keys
// of the gardener blob. It is the exact value spawnLineageDigest hashes, and is
// what `gb trigger reseal` shows the admin before they adopt it.
func spawnLineageContent(fields json.RawMessage) map[string]any {
	all := fieldsAsMap(fields)
	picked := make(map[string]any, len(spawnLineageDigestKeys))
	for _, k := range spawnLineageDigestKeys {
		if v, ok := all[k]; ok && v != nil {
			picked[k] = v
		}
	}
	if g, ok := all["gardener"]; ok && g != nil {
		picked["gardener"] = gardenerConfigOnly(g)
	}
	return picked
}

// spawnLineageDigest is the sha256 of the canonical (key-sorted) JSON of the
// spawn-relevant content (spawnLineageContent) of fields.
func spawnLineageDigest(fields json.RawMessage) string {
	b, _ := json.Marshal(spawnLineageContent(fields)) // map keys are sorted: canonical
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

// validSpawnSeal returns the seal a trigger/schedule bead carries, or nil when
// it is unsealed, malformed, or its spawn-relevant fields have changed since the
// seal was stamped. It does not check age; see (*BeadsServer).spawnSealFresh.
func validSpawnSeal(bead *model.Bead) *spawnLineageSeal {
	if bead == nil || !isAutonomousBeadType(bead.Type) {
		return nil
	}
	raw, _ := fieldsAsMap(bead.Fields)[spawnLineageField].(string)
	if raw == "" {
		return nil
	}
	var seal spawnLineageSeal
	if json.Unmarshal([]byte(raw), &seal) != nil || seal.V != spawnLineageSealVersion ||
		!strings.HasPrefix(seal.Author, "oidc:") || seal.Digest == "" || seal.IAT <= 0 {
		return nil
	}
	if seal.Digest != spawnLineageDigest(bead.Fields) {
		return nil
	}
	return &seal
}

// sealedSpawnAuthor returns the OIDC admin a trigger/schedule bead is sealed to,
// or "" when it carries no valid seal (age is not considered).
func sealedSpawnAuthor(bead *model.Bead) string {
	if seal := validSpawnSeal(bead); seal != nil {
		return seal.Author
	}
	return ""
}

func (s *BeadsServer) lineageNow() time.Time {
	if s.lineageClock != nil {
		return s.lineageClock()
	}
	return time.Now()
}

func (s *BeadsServer) lineageSealTTL() time.Duration {
	if s.spawnLineageSealTTL > 0 {
		return s.spawnLineageSealTTL
	}
	return defaultSpawnLineageSealTTL
}

// SetSpawnLineageSealTTL sets how long a spawn-lineage seal is honoured. A
// non-positive value restores the default.
func (s *BeadsServer) SetSpawnLineageSealTTL(d time.Duration) { s.spawnLineageSealTTL = d }

// spawnSealFresh reports whether seal was stamped no longer ago than the TTL
// and not in the future.
func (s *BeadsServer) spawnSealFresh(seal *spawnLineageSeal) bool {
	issued := time.Unix(seal.IAT, 0)
	now := s.lineageNow()
	return !issued.After(now.Add(spawnLineageClockSkew)) && now.Sub(issued) <= s.lineageSealTTL()
}

// voidSpawnLineageOnReactivation strips the seal when a caller that is not an
// OIDC gardenio-admin session reopens a closed trigger/schedule or turns a
// disabled one on. wasClosed and wasEnabled describe the bead BEFORE the patch;
// bead is the patched result. It reports whether it removed a seal.
func voidSpawnLineageOnReactivation(ctx context.Context, bead *model.Bead, wasClosed, wasEnabled bool) bool {
	if !isAutonomousBeadType(bead.Type) || isOIDCAdminSession(ctx) || engineCarriesSealAcrossGardenerRevival(ctx, bead) {
		return false
	}
	if !reactivates(bead, wasClosed, wasEnabled) {
		return false
	}
	fields := fieldsAsMap(bead.Fields)
	if _, had := fields[spawnLineageField]; !had {
		return false
	}
	delete(fields, spawnLineageField)
	merged, err := json.Marshal(fields)
	if err != nil {
		return false
	}
	bead.Fields = merged
	return true
}

// reactivates reports whether a patch reopened a closed trigger/schedule or
// turned a disabled one on. wasClosed and wasEnabled describe the bead BEFORE
// the patch; bead is the patched result.
func reactivates(bead *model.Bead, wasClosed, wasEnabled bool) bool {
	if !isAutonomousBeadType(bead.Type) {
		return false
	}
	return (wasClosed && bead.Status != model.StatusClosed) ||
		(!wasEnabled && effectiveGardenerEnabled(bead.Type, bead.Fields))
}

// engineCarriesSealAcrossGardenerRevival reports whether the reopen or re-enable
// is the trigger-engine reviving a gardener (gd-VuYwGGYLLT). A gardener's second
// life reopens the SAME bead the engine closed on max_runs and re-arms it
// (gardener_second_life.go), and a seal that every such revival voided would
// make the revived lane's spawns escalate until an admin resealed it. The engine
// is the principal the seal exists for (it holds capTriggerLineage and is the
// only caller whose spawns the seal authorizes); what the rule guards against is
// anyone ELSE turning a stopped trigger back on, and that still voids. The
// carve-out is limited to a bead with a gardener blob: other triggers have no
// second life, so an engine reopen of one is not a revival. It leaves the seal
// itself alone, so the digest still voids it if spawn-relevant content changed.
func engineCarriesSealAcrossGardenerRevival(ctx context.Context, bead *model.Bead) bool {
	if !machineHasCapability(ctx, capTriggerLineage) {
		return false
	}
	g, ok := fieldsAsMap(bead.Fields)["gardener"]
	return ok && g != nil
}

// triggerSpawnRole is the role the trigger is configured to spawn
// (canonicalised the way agent creates are), or "".
func triggerSpawnRole(bead *model.Bead) string {
	role, _, err := exactFieldString(bead.Fields, agentRoleField)
	if err != nil {
		return ""
	}
	return canonicalRole(role)
}

// spawnLineageSealDenial refuses any write of the server-owned seal key on a
// trigger/schedule bead, whoever the caller is. keys are the field keys the
// write touches (for the whole-body paths, only those whose value changes).
func spawnLineageSealDenial(action string, beadType model.BeadType, keys ...string) *AuthzDenial {
	if !isAutonomousBeadType(beadType) {
		return nil
	}
	for _, k := range keys {
		if k == spawnLineageField {
			return &AuthzDenial{
				Action:   action,
				BeadType: string(beadType),
				Message: "fields." + spawnLineageField + " on a " + string(beadType) +
					" is stamped by the server when a gardenio-admin OIDC session writes the bead; it cannot be set directly",
			}
		}
	}
	return nil
}

// spawnLineageSealPatchDenial is spawnLineageSealDenial for the whole-body
// create/update paths: an unchanged value echoed back by a read-modify-write
// client is tolerated, a changed or new one is refused.
func spawnLineageSealPatchDenial(action string, beadType model.BeadType, patch, existing json.RawMessage) *AuthzDenial {
	if !isAutonomousBeadType(beadType) || len(patch) == 0 {
		return nil
	}
	pv, ok := fieldsAsMap(patch)[spawnLineageField]
	if !ok {
		return nil
	}
	if ev, had := fieldsAsMap(existing)[spawnLineageField]; had && reflect.DeepEqual(pv, ev) {
		return nil
	}
	return spawnLineageSealDenial(action, beadType, spawnLineageField)
}

// isOIDCAdminSession reports whether ctx is an OIDC session holding
// gardenio-admin: the only credential type whose writes can seal a trigger.
func isOIDCAdminSession(ctx context.Context) bool {
	return authMethodFromContext(ctx) == authMethodOIDC && hasMinimumRole(RolesFromContext(ctx), RoleAdmin)
}

// spawnLineageDigestMismatchError is returned when a reseal pins the digest the
// admin saw and the bead's spawn-relevant content no longer hashes to it.
// Transport layers map it to 409 / FailedPrecondition.
type spawnLineageDigestMismatchError struct{ expected, current string }

func (e *spawnLineageDigestMismatchError) Error() string {
	return "spawn-lineage reseal refused: the trigger changed since it was read (expected digest " +
		e.expected + ", current " + e.current + "); re-read it, review the new content and reseal again"
}

// takeResealFlag removes the transient reseal control keys from an update patch
// and reports whether reseal_spawn_lineage was set true and the digest the
// caller pinned with reseal_expected_digest ("" when it pinned none). Neither
// key is ever stored.
func takeResealFlag(patch json.RawMessage) (json.RawMessage, bool, string) {
	if len(patch) == 0 {
		return patch, false, ""
	}
	var m map[string]json.RawMessage
	if json.Unmarshal(patch, &m) != nil {
		return patch, false, ""
	}
	raw, hasReseal := m[spawnLineageResealKey]
	rawDigest, hasDigest := m[spawnLineageExpectedDigestKey]
	if !hasReseal && !hasDigest {
		return patch, false, ""
	}
	delete(m, spawnLineageResealKey)
	delete(m, spawnLineageExpectedDigestKey)
	cleaned, err := json.Marshal(m)
	if err != nil {
		return patch, false, ""
	}
	var asBool bool
	if hasReseal && json.Unmarshal(raw, &asBool) != nil {
		var asStr string
		_ = json.Unmarshal(raw, &asStr)
		asBool = strings.EqualFold(strings.TrimSpace(asStr), "true")
	}
	var expected string
	if hasDigest {
		_ = json.Unmarshal(rawDigest, &expected)
		expected = strings.TrimSpace(expected)
	}
	return cleaned, asBool, expected
}

// stampSpawnLineage seals bead.Fields to the writing OIDC admin. It is a no-op
// unless the bead is a trigger/schedule and the caller is an OIDC admin
// session. mayStamp is the caller's policy decision (always on create; on
// update only when the bead was sealed before the patch or the admin asked to
// reseal). It must run after field validation, on the final merged fields.
func (s *BeadsServer) stampSpawnLineage(ctx context.Context, bead *model.Bead, mayStamp bool) bool {
	if !mayStamp || !isAutonomousBeadType(bead.Type) || !isOIDCAdminSession(ctx) {
		return false
	}
	author, _ := s.resolveOperatedByPure(ctx)
	return s.sealSpawnLineageAs(bead, canonicalizeHumanOperator(author))
}

// sealSpawnLineageAs writes the seal for author ("oidc:<email>") over
// bead.Fields as they stand. Callers decide that author may seal: the writing
// OIDC admin (stampSpawnLineage) or the admin who approved the plan a gardener
// runs (stampSpawnLineageFromPlanApproval).
func (s *BeadsServer) sealSpawnLineageAs(bead *model.Bead, author string) bool {
	if !strings.HasPrefix(author, "oidc:") {
		return false
	}
	sealJSON, err := json.Marshal(spawnLineageSeal{V: spawnLineageSealVersion, Author: author, Digest: spawnLineageDigest(bead.Fields), IAT: s.lineageNow().Unix()})
	if err != nil {
		return false
	}
	fields := fieldsAsMap(bead.Fields)
	if fields == nil {
		fields = map[string]any{}
	}
	fields[spawnLineageField] = string(sealJSON)
	merged, err := json.Marshal(fields)
	if err != nil {
		return false
	}
	bead.Fields = merged
	return true
}
