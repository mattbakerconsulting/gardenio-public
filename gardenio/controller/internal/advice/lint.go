package advice

// lint.go implements the targeting-hygiene checks behind `gb advice lint`
// (Phase 3 of the context-crisis plan, ka-0000003733 / ka-0000005093). Where
// `gb advice audit` measures how much matched advice each archetype carries,
// lint answers the per-bead question: does this bead's targeting do what its
// author probably intended, and will it actually be delivered?

import (
	"regexp"
	"strings"

	"gardenio/controller/internal/beadsapi"
)

// Lint severities. Errors are targeting shapes the delivery path drops or
// that can never match; warnings are legal but suspicious.
const (
	LintError = "error"
	LintWarn  = "warn"
)

// Lint check identifiers, stable for --json consumers.
const (
	LintDeniedBroadcast  = "denied-broadcast"
	LintOptedInBroadcast = "opted-in-broadcast"
	LintNoTargeting      = "no-targeting"
	LintJunkProjectLabel = "junk-project-label"
	LintHippoMirror      = "hippo-mirror"
	LintUselessOptIn     = "useless-opt-in"
	LintDeadRoleTarget   = "dead-role-target"
	LintGlobalRoleClash  = "global-role-clash"
	LintUndeliverableCap = "undeliverable-capability"
	LintBrokenGlabFlag   = "broken-glab-mr-flag"
)

// brokenGlabMRListRE flags a description that TEACHES the gh-style
// `glab mr list --state=<value>` invocation. glab is NOT gh: it has no
// `--state` flag and errors "Unknown flag: --state"; the deprecated bare
// `--opened` still works but warns. The real filters are bare `glab mr list`
// (open), `-M`/`--merged`, `-c`/`--closed`, `-A`/`--all`.
//
// This is the exact predicate scripts/lint-glab-mr-flags.sh already encodes for
// the git tree — mirrored here because the advice corpus is a live DB, not git,
// so a NEW advice bead added via `gb advice add` bypasses the CI guard. The
// defect has recurred four times in advice (ka-0000000464 → ka-0000005306 →
// ka-0000006493 → ka-0000003906); this content-lint rule makes datapoint #5 a
// lint error instead of another audit cycle.
//
// Kept deliberately narrow — anchored on `mr list` and requiring a state VALUE
// — so it matches commands, not the word "state" in prose. Matched per-line
// (see the check below) so `[[:space:]]|$` anchors to the end of a line.
var brokenGlabMRListRE = regexp.MustCompile(
	"mr list[^`]*(--state[= ](opened|closed|merged|all)|--opened([[:space:]]|$))")

// glabFlagExemptRE marks a line as TEACHING AGAINST the broken flag rather than
// invoking it, so it is not a violation. Mirrors the shell guard's exempt_line
// set (Unknown flag / deprecated / the auditable inline `glab-lint-ok` token /
// the "NO `--state`" and "is NOT gh" warning phrasings).
var glabFlagExemptRE = regexp.MustCompile(
	"Unknown flag|deprecated|glab-lint-ok|NO `--state`|is NOT gh")

// teachesBrokenGlabFlag reports whether any single line of the description
// invokes the broken `glab mr list --state`/`--opened` form without a teaching
// exemption on that same line. Matched line-by-line exactly like the shell
// guard's `grep -E … | grep -vE exempt_line`.
func teachesBrokenGlabFlag(description string) bool {
	for _, line := range strings.Split(description, "\n") {
		if brokenGlabMRListRE.MatchString(line) && !glabFlagExemptRE.MatchString(line) {
			return true
		}
	}
	return false
}

// OnDemandLabel marks an advice bead as an intentionally-unmatched on-demand
// document: it is never delivered via subscription matching and is instead
// fetched explicitly with `gb advice show <id>` from a pointer elsewhere
// (e.g. the memory-protocol doc referenced by the session preamble). Such
// beads are exempt from the no-targeting lint error.
const OnDemandLabel = "scope:on-demand"

// isOnDemand reports whether the labels carry the OnDemandLabel.
func isOnDemand(labels []string) bool {
	for _, l := range labels {
		if StripGroupPrefix(l) == OnDemandLabel {
			return true
		}
	}
	return false
}

// capabilityCanRE matches an emphatic capability assertion of the form
// "<subject> CAN <verb>" — the all-caps standalone word CAN (word-bounded, so
// "cannot"/"CANNOT"/"CAN'T" and words like "canary" or "scan" never match)
// immediately followed by a lowercase verb. This is the shout an author uses
// to correct a false "I can't do this" belief (ka-0000006777's title:
// "Gardenio-project agents CAN read pod/service logs cluster-wide").
var capabilityCanRE = regexp.MustCompile(`\bCAN\b +[a-z]`)

// deEscalationRE matches an assertion that no operator/escalation is needed for
// something — the other half of the same capability-correction pattern
// (ka-0000005080's title: "creds already fetchable via tf-dev-admin-role", i.e.
// "no operator/escalation needed"). Case-insensitive. Bounded [^.]{0,N} spans
// keep a match inside one clause so an unrelated later sentence cannot bridge
// into a false positive.
var deEscalationRE = regexp.MustCompile(
	`(?i)already (fetchable|available)` +
		`|no (operator|escalation)[^.]{0,20}(needed|required)` +
		`|do ?n.?t (file|need)[^.]{0,25}escalat` +
		`|without[^.]{0,15}escalat` +
		`|no need to escalat`)

// permissionGrantRE matches an explicit grant of permission to perform a
// privileged action without a human — the third face of the same
// capability-correction pattern, and the one that slipped through.
//
// ka-0000002230 ("Gardenio is in charge of its own repo — agents may
// self-merge any gardenio MR") is a standing operator grant, but it phrased
// itself with a lowercase "may" rather than an emphatic "CAN", so
// capabilityCanRE never matched and the bead sat on scope:on-demand —
// undeliverable — for four months. Meanwhile the boot capability probe asserted
// the opposite fleet-wide, and agents kept filing gitlab_mr_merge escalations
// for MRs they were explicitly allowed to merge (bug ka-0000007570). A grant no
// agent is shown is exactly the contradiction this check exists to catch.
//
// Deliberately narrow: only permission to MERGE without an operator, the class
// that produced the measured escalation noise. It must not fire on the legit
// on-demand pointer docs (the escalation how-to, the memory protocol), which
// discuss when to escalate without granting anyone merge rights.
var permissionGrantRE = regexp.MustCompile(`(?i)\b(may|are allowed to) (self-)?merge\b`)

// assertsCapability reports whether the text makes a self-standing capability
// claim: either an emphatic "X CAN do Y" or a "no escalation/operator needed"
// de-escalation. Applied to a bead's TITLE (the author's thesis) rather than
// the whole description on purpose — the body of a legitimately on-demand
// pointer doc (e.g. the escalation how-to ka-0000001089) routinely explains
// WHEN to escalate and cites capability checks, which would false-positive a
// body scan; the title is where a capability-correcting bead states its claim.
func assertsCapability(title string) bool {
	return capabilityCanRE.MatchString(title) ||
		deEscalationRE.MatchString(title) ||
		permissionGrantRE.MatchString(title)
}

// hasGlobalLabel reports whether the labels carry the `global` targeting label.
func hasGlobalLabel(labels []string) bool {
	for _, l := range labels {
		if StripGroupPrefix(l) == "global" {
			return true
		}
	}
	return false
}

// hasRoleLabel reports whether the labels carry any role: targeting label.
func hasRoleLabel(labels []string) bool {
	for _, l := range labels {
		if strings.HasPrefix(StripGroupPrefix(l), "role:") {
			return true
		}
	}
	return false
}

// LintFinding is one flagged advice bead.
type LintFinding struct {
	ID       string   `json:"id"`
	Title    string   `json:"title"`
	Labels   []string `json:"labels"`
	Check    string   `json:"check"`
	Severity string   `json:"severity"`
	Message  string   `json:"message"`
}

// LintAdvice runs every targeting-hygiene check over the corpus and returns
// findings ordered by input position (a bead may yield several findings).
// Closed and expired beads are skipped — they are not delivered, so their
// targeting cannot leak.
//
// liveRoles is the union-registry of roles that can currently spawn (builtins
// + type:role bead names + the role field of open triggers) used by the
// dead-role-target check. Pass nil to skip that check entirely — the delivery
// path and unit tests that do not have a daemon handy call it that way, and the
// `gb advice lint` command builds the set and passes it (fail-open to nil on a
// fetch error). A nil map is treated as "unknown", never "empty", so a fetch
// failure can never manufacture spurious dead-role findings.
//
// replacedByPrerun is the set of advice bead ids that a boot-time prerun entry
// already delivers live (its `replaces_advice`, e.g. the capability-probe's
// [ka-0000006777, ka-0000005080]). Such a bead is intentionally scope:on-demand
// — its capability correction reaches every agent per-pod via the probe output,
// so the undeliverable-capability check must NOT flag it. Without this exemption
// the check tells agents to retarget the bead back to project:/role: — the exact
// churn that decision ka-0000005100 had to undo after the 2026-07-30 revert. Pass
// nil to disable the exemption (unit tests and the delivery path that has no
// prerun set handy call it that way); a nil set exempts nothing.
func LintAdvice(beads []*beadsapi.BeadDetail, liveRoles, replacedByPrerun map[string]struct{}) []LintFinding {
	var findings []LintFinding
	add := func(b *beadsapi.BeadDetail, check, severity, msg string) {
		findings = append(findings, LintFinding{
			ID:       b.ID,
			Title:    b.Title,
			Labels:   b.Labels,
			Check:    check,
			Severity: severity,
			Message:  msg,
		})
	}

	// The delivery path treats singular/plural role: labels as equivalent
	// (BuildAgentSubscriptions subscribes to both forms; detectRoleMismatch,
	// ArchetypeSubscriptions and ArchetypesFromAgentBeads all Singularize), so the
	// dead-role-target check must normalize too — otherwise a genuinely-live
	// plural-labelled bead (role:crews against a live singular "crew") is
	// spuriously flagged, the exact failure the check is meant to avoid.
	// Singularize the live-role keys once here and each label-derived role name
	// at the comparison below. Left nil when liveRoles is nil so the check stays
	// skipped for nil (unknown-registry) callers.
	var liveRolesSingular map[string]struct{}
	if liveRoles != nil {
		liveRolesSingular = make(map[string]struct{}, len(liveRoles))
		for name := range liveRoles {
			liveRolesSingular[Singularize(name)] = struct{}{}
		}
	}

	for _, b := range beads {
		if b == nil || b.Status == "closed" || IsExpired(b) || IsFixture(b) {
			continue
		}

		if IsHippoMirror(b) {
			add(b, LintHippoMirror, LintWarn,
				"legacy hippo-mirror bead — never delivered; close it")
			continue
		}

		broadcast := IsProjectBroadcast(b.Labels)
		optIn := HasBroadcastOptIn(b.Labels)
		switch {
		case broadcast && !optIn:
			add(b, LintDeniedBroadcast, LintError,
				"bare-project broadcast without "+BroadcastOptInLabel+" — dropped by delivery; add role:/agent: targeting or the opt-in label")
		case broadcast && optIn:
			add(b, LintOptedInBroadcast, LintWarn,
				"intentional project-wide broadcast — every agent in the project pays for this bead; keep it short and reconsider a role: target")
		case !broadcast && optIn:
			add(b, LintUselessOptIn, LintWarn,
				BroadcastOptInLabel+" present but the bead is not a bare-project broadcast — remove the stray opt-in label")
		}

		if !HasTargetingLabel(b.Labels) && !isOnDemand(b.Labels) {
			add(b, LintNoTargeting, LintError,
				"no targeting label (global/project:/role:/agent:) — matches no agent and is never delivered; if this is an intentional on-demand doc, add "+OnDemandLabel)
		}

		// undeliverable-capability: a bead whose TITLE asserts a capability
		// ("X CAN do Y") or de-escalates ("no operator/escalation needed") but
		// is simultaneously UNDELIVERABLE is a self-defeating contradiction —
		// the fleet's correction to a false "I can't do this" belief is stored
		// exactly where no agent will ever be shown it. This is the defect
		// behind ka-0000003515: ka-0000006777 ("agents CAN read pod logs") and
		// ka-0000005080 ("creds already fetchable") both carried scope:on-demand
		// ONLY, so role-matching never delivered them while 35 escalations in
		// the corpus cited the exact denial they refute. A bead is undeliverable
		// here if it is on-demand (never role-matched) or a denied bare-project
		// broadcast (dropped by delivery). ERROR because the label set defeats
		// the bead's own purpose: retarget it to the project/role whose agents
		// hold the capability so they actually receive the correction. The
		// denied-broadcast case already gets its own error above; this check is
		// what catches the on-demand escape hatch that no other check flags.
		//
		// EXEMPTION: a bead whose id is named in a boot-time prerun's
		// `replaces_advice` is delivered LIVE per-pod by that probe (e.g. the
		// capability-probe prints "can-i get pods/log -A: yes — read logs
		// directly, no escalation needed", replacing ka-0000006777). Its
		// scope:on-demand label is then deliberate, not a delivery gap, so the
		// correction is NOT undeliverable and must not be flagged — otherwise the
		// check pushes agents to retarget it back to project:/role:, the exact
		// churn decision ka-0000005100 had to undo after the 2026-07-30 revert.
		if assertsCapability(b.Title) && (isOnDemand(b.Labels) || IsDeniedBroadcast(b)) {
			if _, replaced := replacedByPrerun[b.ID]; !replaced {
				add(b, LintUndeliverableCap, LintError,
					"title asserts a capability / 'no escalation needed' but the bead is undeliverable (on-demand or denied broadcast) — a capability correction no agent is shown defeats its own purpose; retarget it to the project:/role: whose agents hold the capability")
			}
		}

		// global-role-clash: a bead labelled BOTH `global` AND `role:` is
		// self-contradictory. `global` means "every agent, every project"
		// (advice-hygiene.md), but MatchesSubscriptions' role gate vetoes any
		// bead carrying role: labels the agent doesn't match — even when the
		// bead is also `global` — so the role set silently WINS and the bead
		// reaches only those roles, never the whole fleet. The `global` label
		// is inert, and CategorizeScope renders it under a role header anyway.
		// This is the exact defect behind ka-0000004354: ka-0000004818 carried
		// `global, role:gardener, role:crew, role:jira-mention` and its
		// load-bearing thread consumer (not in the role set) never received it.
		// ERROR because the label set does not do what its author intended:
		// either drop `global` (the bead is role-scoped) or drop the role:
		// labels (the bead is truly fleet-wide).
		if hasGlobalLabel(b.Labels) && hasRoleLabel(b.Labels) {
			add(b, LintGlobalRoleClash, LintError,
				"labelled both `global` and `role:` — contradictory: the role gate narrows delivery to those roles and `global` is inert (never reaches the whole fleet). Drop `global` if role-scoped, or drop the role: labels if truly fleet-wide")
		}

		// broken-glab-mr-flag: a description that teaches the gh-style
		// `glab mr list --state=<value>` (or bare `--opened`) invocation ships
		// a command that errors "Unknown flag: --state" to every agent that
		// reads it. The in-repo lint-glab-mr-flags job guards the git tree, but
		// advice lives in a live DB, so a bead added via `gb advice add` bypasses
		// it — this rule is the data-plane belt-and-suspenders (ka-0000003528).
		// ERROR because it disseminates a command that cannot run.
		if teachesBrokenGlabFlag(b.Description) {
			add(b, LintBrokenGlabFlag, LintError,
				"description teaches the gh-style `glab mr list --state=<value>` (or bare `--opened`) invocation — glab has NO `--state` flag and errors \"Unknown flag: --state\". Use bare `glab mr list` (open), `-M`/`--merged`, `-c`/`--closed`, or `-A`/`--all`")
		}

		for _, l := range b.Labels {
			clean := StripGroupPrefix(l)
			if name, ok := strings.CutPrefix(clean, "project:"); ok && (strings.HasPrefix(name, "gd-") || strings.HasPrefix(name, "ka-")) {
				add(b, LintJunkProjectLabel, LintWarn,
					"junk label "+clean+" — bead-ID-shaped project target matches no real project")
			}
		}

		// dead-role-target: a role: target outside the live-role union can no
		// longer spawn an agent, so the advice is dead-lettered (never matches).
		// WARN not ERROR because the union is intentionally incomplete — roles
		// are free strings (ka-0000001425) and a new role can precede its config
		// bead. Skipped when liveRoles is nil (unknown registry / nil callers)
		// and for on-demand docs, which are never delivered by role matching.
		if liveRoles != nil && !isOnDemand(b.Labels) {
			for _, l := range b.Labels {
				clean := StripGroupPrefix(l)
				name, ok := strings.CutPrefix(clean, "role:")
				if !ok || name == "" {
					continue
				}
				if _, live := liveRolesSingular[Singularize(name)]; !live {
					add(b, LintDeadRoleTarget, LintWarn,
						"role target "+clean+" has no type:role bead, no builtin, no open trigger, and no channel_roles wiring — likely retired; close or retarget this advice")
					break
				}
			}
		}
	}
	return findings
}
