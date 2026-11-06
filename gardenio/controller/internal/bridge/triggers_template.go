package bridge

import (
	"fmt"
	"regexp"
	"sort"
	"strings"
	"time"

	"gardenio/controller/internal/beadsapi"
)

// ExpandTemplate replaces {{variable}} placeholders in a template string
// with values from the provided vars map. Keys in vars should include the
// double braces (e.g. "{{jira_key}}").
func ExpandTemplate(tmpl string, vars map[string]string) string {
	if tmpl == "" {
		return ""
	}
	result := tmpl
	for placeholder, value := range vars {
		result = strings.ReplaceAll(result, placeholder, value)
	}
	return result
}

// ensureFireTimeVars guarantees the fire-time template vars exist on a match
// before action dispatch. Clock-driven sources (schedule, multi, mirror)
// publish {{fire_time}}; {{fire_time_iso}} is a widely-authored alias that no
// source emitted, so trigger beads using it substituted the literal template
// string into molecule applied_vars and child titles (ka-0000006473). Sources
// with no fire time (bead, jira, gitlab, ...) get `now` so a fire-time
// placeholder in formula_vars/prompt always resolves. Existing values win.
func ensureFireTimeVars(m *TriggerMatch, now time.Time) {
	if m.TemplateVars == nil {
		m.TemplateVars = make(map[string]string)
	}
	if m.TemplateVars["{{fire_time}}"] == "" {
		m.TemplateVars["{{fire_time}}"] = now.UTC().Format(time.RFC3339)
	}
	if m.TemplateVars["{{fire_time_iso}}"] == "" {
		m.TemplateVars["{{fire_time_iso}}"] = m.TemplateVars["{{fire_time}}"]
	}
}

// knownTemplateVarNames renders the sorted placeholder names a match's
// TemplateVars actually offers — used in the unresolved-placeholder error so
// the trigger author can see what to write instead of the bad var.
func knownTemplateVarNames(vars map[string]string) string {
	if len(vars) == 0 {
		return "(none)"
	}
	names := make([]string, 0, len(vars))
	for k := range vars {
		names = append(names, k)
	}
	sort.Strings(names)
	return strings.Join(names, " ")
}

// unresolvedPlaceholderPattern matches {{name}} placeholders that survived
// template expansion — the signature of a var the firing source never
// published (e.g. a typo, or an alias like fire_time_iso before it existed).
var unresolvedPlaceholderPattern = regexp.MustCompile(`\{\{\w+\}\}`)

// findUnresolvedVars returns the formula-var names whose values still contain
// {{...}} placeholders after template expansion, sorted for stable error
// messages. Formula apply must reject these: the literal template string
// would otherwise be substituted into every molecule and step title, creating
// unclaimable beads that recur on every fire (ka-0000006473).
func findUnresolvedVars(vars map[string]string) []string {
	var bad []string
	for name, val := range vars {
		if unresolvedPlaceholderPattern.MatchString(val) {
			bad = append(bad, fmt.Sprintf("%s=%q", name, val))
		}
	}
	sort.Strings(bad)
	return bad
}

// registeredSourceKeyFields is the set of `<source>_key` agent-bead fields the
// kbeads `type:agent` schema explicitly declares (kbeads/internal/server/
// config.go). Only these source names may stamp their own dedicated key field;
// every other source falls back to the generic `source_key` field below.
//
// These specific fields are ALSO read back by name downstream — jira_key by the
// agent-card subject deriver (card_subject.go) and jira reply/thread forwarders,
// gitlab_key by the gitlab trigger source, schedule_key by the gardener timeline
// — so they must keep their dedicated names rather than collapse into the
// generic field (ka-0000001885).
var registeredSourceKeyFields = map[string]bool{
	"jira_key":            true,
	"gitlab_key":          true,
	"schedule_key":        true,
	"schedule_mirror_key": true,
	"multi_key":           true,
}

// genericSourceKeyField is the always-valid agent-bead field used to stamp a
// trigger match key for any source whose sanitized `<source>_key` is NOT a
// registered agent field. See triggerSourceFieldKey.
const genericSourceKeyField = "source_key"

// triggerSourceFieldKey returns the agent-bead field name used to stamp a
// trigger match key onto spawned beads.
//
// For the handful of sources the kbeads `type:agent` schema declares a
// dedicated `<source>_key` field for (jira, gitlab, schedule, schedule-mirror,
// multi) it returns that sanitized field name — those are read back by name
// downstream, so they keep their dedicated identity. For EVERY other source
// (bead, account, slack-reaction, and operator-named webhook sources whose
// names are dynamic and can never be pre-registered) it returns the generic,
// always-valid `source_key` field.
//
// Before this, the function unconditionally returned `<sanitized-source>_key`,
// which produced UNREGISTERED field names like `bead_key` / `account_key` for
// bead/account/webhook sources — so every bead-source apply_formula driver
// spawn (and spawn_agent fire) 400'd "bead_key: unknown field" and left the
// poured molecule driver-less (ka-0000001885). Sanitizing hyphens to
// underscores is still required so multi-word registered sources map to their
// declared field (`schedule_mirror_key`, not `schedule-mirror_key`) — the
// ka-0000000883 fix.
//
// The reader side (bot_agents.go cacheTriggerFieldsLocked) calls this same
// function, so writer and reader stay symmetric by construction.
func triggerSourceFieldKey(source string) string {
	sanitized := strings.ReplaceAll(source, "-", "_") + "_key"
	if registeredSourceKeyFields[sanitized] {
		return sanitized
	}
	return genericSourceKeyField
}

// triggerAgentName generates a K8s-safe agent name for a triggered spawn.
// It prefers a slug derived from the trigger's human-readable title, falling
// back to the match key when the title yields no usable slug (e.g. an
// untitled trigger or all-symbol title). A random suffix preserves
// per-spawn uniqueness.
//
// Examples (project="gardenio"):
//
//	title "Gardener: AWS POC litellm enablement (ka-0000004609)" → "gardenio-aws-poc-litellm-enablement-a7k2"
//	title "JIRA PE Triage",        key "PE-1234"             → "gardenio-jira-pe-triage-c9z4"
//	title "",                       key "example-org/gardenio/pipeline/12345"   → "gardenio-pipeline-12345-b3x1"
//	title "",                       key "example-org/monorepo!42"               → "gardenio-example-org-monorepo-mr-42-d4y2"
func triggerAgentName(trig TriggerDef, matchKey string) string {
	slug := slugifyTriggerTitle(trig.Title)
	if slug == "" {
		slug = slugifyMatchKey(matchKey)
	}
	if slug == "" {
		slug = "trigger"
	}

	if trig.Project != "" {
		return fmt.Sprintf("%s-%s-%s", trig.Project, slug, randomSuffix(4))
	}
	return fmt.Sprintf("trigger-%s-%s", slug, randomSuffix(4))
}

// slugifyTriggerTitle reduces a trigger's title to a short, K8s-safe slug.
// It strips common framing — leading role prefixes like "Gardener:" /
// "Scheduled:" and trailing parentheticals like "(ka-xyz)" or "(every 30m)"
// — that carry no identifying signal, then collapses to kebab-case and
// truncates to a budget that leaves room for the project prefix + random
// suffix inside the K8s 63-char pod-name limit.
func slugifyTriggerTitle(title string) string {
	t := strings.TrimSpace(title)
	if t == "" {
		return ""
	}
	// Strip common leading framing. Repeat once in case of nested
	// prefixes like "Gardener: Scheduled: foo".
	for range 2 {
		for _, prefix := range []string{"Gardener:", "gardener:", "Scheduled:", "scheduled:", "ScheduleMirror:", "ScheduleMirror"} {
			t = strings.TrimSpace(strings.TrimPrefix(t, prefix))
		}
	}
	// Strip trailing parentheticals (cadence metadata, ka-id refs, etc.).
	// Only strip when the title actually ends with a closing paren so
	// titles with mid-string parens aren't chopped.
	for strings.HasSuffix(t, ")") {
		i := strings.LastIndex(t, "(")
		if i < 0 {
			break
		}
		t = strings.TrimSpace(t[:i])
	}
	t = strings.ToLower(t)
	t = reNonAlnum.ReplaceAllString(t, "-")
	t = reMultiHyphen.ReplaceAllString(t, "-")
	t = strings.Trim(t, "-")
	const maxLen = 32
	if len(t) > maxLen {
		t = strings.TrimRight(t[:maxLen], "-")
	}
	return t
}

// slugifyMatchKey reduces an opaque trigger match key to a K8s-safe slug.
// Match keys can be hierarchical paths ("example-org/gardenio/pipeline/12345"),
// MR refs ("monorepo!42"), JIRA keys ("PE-1234"), or schedule-mirror tuples
// ("0:0:2026-05-27T01:19") — this is the legacy sanitizeTriggerAgentName
// behavior, used only as a fallback when the trigger title is missing.
func slugifyMatchKey(matchKey string) string {
	parts := strings.Split(matchKey, "/")
	slug := matchKey
	if len(parts) > 2 {
		slug = strings.Join(parts[len(parts)-2:], "-")
	}
	slug = strings.ReplaceAll(slug, "!", "-mr-")
	slug = strings.ToLower(slug)
	slug = reNonAlnum.ReplaceAllString(slug, "-")
	slug = reMultiHyphen.ReplaceAllString(slug, "-")
	slug = strings.Trim(slug, "-")
	const maxSlugLen = 42
	if len(slug) > maxSlugLen {
		slug = strings.TrimRight(slug[:maxSlugLen], "-")
	}
	return slug
}

var (
	reNonAlnum    = regexp.MustCompile(`[^a-z0-9-]`)
	reMultiHyphen = regexp.MustCompile(`-{2,}`)
)

// e2eFixtureLabelPrefix marks beads seeded by the beads-viewer-e2e suite
// (gardenio/tests/beads-viewer-e2e/seed/fixtures.mjs). The suite upserts these
// fixtures by a stable `fixture:e2e:<key>` label into the SHARED PRODUCTION
// beads daemon so the viewer's behavioral specs have deterministic rows to
// assert against. Several of those rows are trigger:schedule beads with
// enabled=true — which the production scheduler would otherwise fire, spawning
// throwaway crew agents on the fixture's cron (ka-0000005435). The fixtures
// must still LOAD and RENDER as enabled in beads-viewer (the specs require it),
// so we cannot flip enabled=false; instead the trigger sources skip firing any
// bead carrying this label prefix.
const e2eFixtureLabelPrefix = "fixture:e2e:"

// isE2EFixture reports whether a bead was seeded by the beads-viewer-e2e suite
// (carries a `fixture:e2e:<key>` label). Trigger sources that fire spawn_agent
// must skip such beads so the shared prod scheduler never spawns a live agent
// for an E2E fixture, while leaving the bead's enabled field untouched so it
// still renders in beads-viewer. See e2eFixtureLabelPrefix (ka-0000005435).
func isE2EFixture(bead *beadsapi.BeadDetail) bool {
	for _, l := range bead.Labels {
		if strings.HasPrefix(l, e2eFixtureLabelPrefix) {
			return true
		}
	}
	return false
}
