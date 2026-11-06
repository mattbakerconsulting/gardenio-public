package bridge

import (
	"context"
	"log/slog"
	"strings"
	"sync"

	"gardenio/controller/internal/beadsapi"
)

// JiraTriggerSource implements TriggerSource for JIRA JQL triggers.
type JiraTriggerSource struct {
	jira *JiraClient
	// projects, when non-empty, restricts LoadTriggers to trigger beads
	// carrying a matching "project:<name>" label. This is the bridge-side
	// guard against the cross-environment cooldown-starvation class
	// (ka-0000006237): a sandbox bridge pointed at a shared daemon must never
	// load (and stamp last_checked on) another environment's trigger beads.
	// Empty = no filter (back-compat). Set via JIRA_TRIGGER_PROJECTS.
	projects []string
	logger   *slog.Logger
}

// NewJiraTriggerSource creates a JIRA trigger source with no project filter.
func NewJiraTriggerSource(jira *JiraClient) *JiraTriggerSource {
	return NewJiraTriggerSourceScoped(jira, nil, nil)
}

// NewJiraTriggerSourceScoped creates a JIRA trigger source that only loads
// trigger beads labeled with one of the given projects. An empty projects
// list disables the filter (back-compat). See ka-0000006237.
func NewJiraTriggerSourceScoped(jira *JiraClient, projects []string, logger *slog.Logger) *JiraTriggerSource {
	if logger == nil {
		logger = slog.Default()
	}
	cleaned := make([]string, 0, len(projects))
	for _, p := range projects {
		if p = strings.TrimSpace(p); p != "" {
			cleaned = append(cleaned, p)
		}
	}
	return &JiraTriggerSource{jira: jira, projects: cleaned, logger: logger}
}

func (s *JiraTriggerSource) Name() string { return "jira" }

// jiraTriggerData holds the JIRA-specific data for a trigger.
type jiraTriggerData struct {
	JQL string
}

// matchesProjectScope reports whether a trigger bead's labels fall within the
// source's project filter. With no filter configured, everything matches.
// With a filter, the bead must carry "project:<p>" for one of the configured
// projects — an UNLABELED bead does NOT match (strict partitioning: if two
// bridges both adopted unlabeled beads they would race on them, which is the
// exact bug this filter exists to prevent).
func (s *JiraTriggerSource) matchesProjectScope(labels []string) bool {
	if len(s.projects) == 0 {
		return true
	}
	for _, l := range labels {
		for _, p := range s.projects {
			if l == "project:"+p {
				return true
			}
		}
	}
	return false
}

func (s *JiraTriggerSource) LoadTriggers(ctx context.Context, daemon TriggerBeadClient) ([]TriggerDef, error) {
	result, err := daemon.ListBeadsFiltered(ctx, beadsapi.ListBeadsQuery{
		Types:    []string{"trigger"},
		Statuses: []string{"open"},
		Labels:   []string{"trigger:jira"},
		Limit:    100,
	})
	if err != nil {
		return nil, err
	}

	var triggers []TriggerDef
	for _, bead := range result.Beads {
		if !s.matchesProjectScope(bead.Labels) {
			// Out-of-scope for this bridge's environment — do not evaluate
			// and, critically, do not stamp last_checked (ka-0000006237).
			// WARN for unlabeled beads: with a filter active they are
			// evaluated by NO bridge, which is a misconfiguration.
			if !hasAnyProjectLabel(bead.Labels) {
				s.logger.Warn("jira triggers: skipping trigger bead with no project label while JIRA_TRIGGER_PROJECTS is set — it will not be evaluated by any scoped bridge",
					"trigger", bead.ID, "title", bead.Title, "scope", s.projects)
			} else {
				s.logger.Debug("jira triggers: skipping out-of-scope trigger bead",
					"trigger", bead.ID, "labels", bead.Labels, "scope", s.projects)
			}
			continue
		}
		if bead.Fields["enabled"] == "false" {
			continue
		}
		jql := bead.Fields["jql"]
		if jql == "" {
			continue
		}

		t := ParseTriggerDef(bead, "jira")
		t.SourceData = jiraTriggerData{JQL: jql}
		triggers = append(triggers, t)
	}
	return triggers, nil
}

// hasAnyProjectLabel reports whether the labels carry any "project:<name>"
// label with a non-empty name.
func hasAnyProjectLabel(labels []string) bool {
	for _, l := range labels {
		if v, ok := strings.CutPrefix(l, "project:"); ok && v != "" {
			return true
		}
	}
	return false
}

func (s *JiraTriggerSource) Evaluate(ctx context.Context, trig TriggerDef) ([]TriggerMatch, error) {
	data, ok := trig.SourceData.(jiraTriggerData)
	if !ok {
		return nil, nil
	}

	fields := []string{"summary", "description", "status", "issuetype", "priority", "reporter", "assignee", "labels"}
	issues, err := s.jira.SearchIssues(ctx, data.JQL, fields, 50)
	if err != nil {
		return nil, err
	}

	var matches []TriggerMatch
	for _, issue := range issues {
		matches = append(matches, jiraIssueToMatch(issue))
	}
	return matches, nil
}

// ApproximateMatchCount implements the engine's approximateCounter interface
// (trigger_starve.go): the upstream-match arm of the cooldown-starvation
// verdict (Alert B of epic ka-0000005107). It answers "does this trigger's
// JQL currently match anything?" via the cheap
// /rest/api/3/search/approximate-count endpoint — the same call the JQL-count
// metrics exporter uses (commit 94a188743) — so confirming a starved trigger
// costs one rate-limit point instead of a full search.
func (s *JiraTriggerSource) ApproximateMatchCount(ctx context.Context, trig TriggerDef) (int64, bool, error) {
	data, ok := trig.SourceData.(jiraTriggerData)
	if !ok || data.JQL == "" {
		return 0, false, nil
	}
	count, err := s.jira.ApproximateCount(ctx, data.JQL)
	return count, true, err
}

func (s *JiraTriggerSource) CatchUpKeys(ctx context.Context, daemon TriggerBeadClient, triggerID string) ([]string, error) {
	result, err := daemon.ListBeadsFiltered(ctx, beadsapi.ListBeadsQuery{
		Kinds:    []string{"issue"},
		Statuses: []string{"open", "in_progress", "closed"},
		Labels:   []string{"trigger:" + triggerID},
		Limit:    500,
	})
	if err != nil {
		return nil, err
	}

	var keys []string
	for _, bead := range result.Beads {
		if key := bead.Fields["jira_key"]; key != "" {
			keys = append(keys, key)
		}
	}
	return keys, nil
}

// jiraIssueToMatch converts a JiraIssue to a TriggerMatch with template vars.
func jiraIssueToMatch(issue JiraIssue) TriggerMatch {
	vars := map[string]string{
		"{{jira_key}}":       issue.Key,
		"{{jira_key_lower}}": strings.ToLower(issue.Key),
		"{{jira_summary}}":   issue.Fields.Summary,
		"{{jira_url}}":       "https://acme.atlassian.net/browse/" + issue.Key,
		"{{jira_labels}}":    strings.Join(issue.Fields.Labels, ", "),
	}

	if issue.Fields.Status != nil {
		vars["{{jira_status}}"] = issue.Fields.Status.Name
	}
	if issue.Fields.IssueType != nil {
		vars["{{jira_type}}"] = issue.Fields.IssueType.Name
	}
	if issue.Fields.Priority != nil {
		vars["{{jira_priority}}"] = issue.Fields.Priority.Name
	}
	if issue.Fields.Reporter != nil {
		vars["{{jira_reporter}}"] = issue.Fields.Reporter.DisplayName
	}
	if issue.Fields.Assignee != nil {
		vars["{{jira_assignee}}"] = issue.Fields.Assignee.DisplayName
	} else {
		vars["{{jira_assignee}}"] = "Unassigned"
	}

	if len(issue.Fields.Description) > 0 {
		vars["{{jira_description}}"] = adfToMarkdown(issue.Fields.Description)
	} else {
		vars["{{jira_description}}"] = ""
	}

	fields := map[string]string{
		"jira_key": issue.Key,
		"jira_url": "https://acme.atlassian.net/browse/" + issue.Key,
	}
	if issue.Fields.Status != nil {
		fields["jira_status"] = issue.Fields.Status.Name
	}
	if issue.Fields.IssueType != nil {
		fields["jira_type"] = issue.Fields.IssueType.Name
	}
	if issue.Fields.Priority != nil {
		fields["jira_priority"] = issue.Fields.Priority.Name
	}
	// Carry the reporter's resolvable identity so the trigger engine's
	// MatchOperatorResolver can attribute an auto-spawn to the ticket reporter
	// (mapped to a fleet human) instead of fleet:jira-bridge (ka-0000001781).
	// DisplayName alone is not resolvable; the accountId/email are the lookup
	// keys. Email is present only when the JIRA app can expose it.
	if issue.Fields.Reporter != nil {
		if issue.Fields.Reporter.AccountID != "" {
			fields["jira_reporter_id"] = issue.Fields.Reporter.AccountID
		}
		if issue.Fields.Reporter.EmailAddress != "" {
			fields["jira_reporter_email"] = issue.Fields.Reporter.EmailAddress
		}
	}

	return TriggerMatch{
		Key:   issue.Key,
		Title: issue.Fields.Summary,
		Fields: fields,
		Labels: []string{
			"jira:" + issue.Key,
		},
		TemplateVars: vars,
	}
}

// JiraTriggerEvaluator is the backward-compatible wrapper that uses the shared
// TriggerEngine with a JiraTriggerSource. Existing callers (JiraPoller) can
// continue using this without changes.
type JiraTriggerEvaluator struct {
	engine *TriggerEngine
	source *JiraTriggerSource

	// mu and seen are exposed for backward-compatible test access.
	mu   sync.Mutex
	seen map[string]map[string]bool
}

// JiraTriggerConfig holds configuration for the JIRA trigger evaluator.
type JiraTriggerConfig struct {
	Jira     *JiraClient
	Daemon   TriggerBeadClient
	Notifier TriggerNotifier
	Logger   *slog.Logger
	// TriggerProjects restricts trigger-bead loading to beads labeled
	// "project:<name>" for one of the listed projects. Empty = load all
	// (back-compat). Set from JIRA_TRIGGER_PROJECTS so two bridges sharing a
	// daemon never evaluate (and cooldown-starve) each other's triggers
	// (ka-0000006237).
	TriggerProjects []string

	// Users resolves a matched ticket's reporter (accountId/email) to a fleet
	// human so an auto-spawn is attributed to the reporter instead of
	// fleet:jira-bridge (ka-0000001781). Nil ⇒ no reporter resolution is wired
	// and the engine falls back to its DefaultOperator / fleet attribution,
	// exactly as before this knob existed.
	Users JiraUserLookup
}

// NewJiraTriggerEvaluator creates a backward-compatible JIRA trigger evaluator
// backed by the shared TriggerEngine.
func NewJiraTriggerEvaluator(cfg JiraTriggerConfig) *JiraTriggerEvaluator {
	logger := cfg.Logger
	if logger == nil {
		logger = slog.Default()
	}
	engineCfg := TriggerEngineConfig{
		Daemon:   cfg.Daemon,
		Notifier: cfg.Notifier,
		Logger:   cfg.Logger,
	}
	if cfg.Users != nil {
		engineCfg.MatchOperatorResolver = newJiraReporterOperatorResolver(cfg.Users, logger)
	}
	engine := NewTriggerEngine(engineCfg)

	source := NewJiraTriggerSourceScoped(cfg.Jira, cfg.TriggerProjects, cfg.Logger)
	engine.RegisterSource(source)

	return &JiraTriggerEvaluator{
		engine: engine,
		source: source,
		seen:   engine.seen, // share the same seen map for test access
	}
}

// newJiraReporterOperatorResolver returns a MatchOperatorResolver that maps a
// matched Jira issue's reporter — carried on the match as jira_reporter_id /
// jira_reporter_email (jiraIssueToMatch) — to a HUMAN operator so the
// auto-spawned agent boots with a resolvable BEADS_OPERATED_BY. Resolution
// order:
//
//  1. UserDirectory match (accountId then email) → slack:<id> / oidc:<email>.
//     This is the canonical fleet identity and stays identical to the
//     @-mention path's resolveJiraOperator (ka-0000001781).
//  2. Directory MISS but the reporter carries an email → oidc:<email>. The
//     JQL trigger already knows the reporter's email; that email is itself a
//     daemon-accepted human operator (oidc:<local>@<dotted-domain>, per
//     kbeads isHumanOperator / sanitizeOperatedByHeader). Falling back to it
//     closes the "auto-spawned crew pod booted with BEADS_OPERATED_BY unset →
//     every operator-attributed write hard-400s (operator could not be
//     resolved)" bug (ka-0000004653) for the common case: a JIRA ticket whose
//     reporter is not yet a fleet user bead. This is the same value a human
//     agent hit the bug and had to `export BEADS_OPERATED_BY=oidc:<email>` by
//     hand — now stamped by the spawn path automatically.
//
// Returns "" when the match has no reporter identity, or a directory miss with
// no usable email, so the engine falls back to its DefaultOperator / fleet
// attribution exactly as before. The lookup is read-only (no jira_user
// backfill) so a fire never blocks on a write.
func newJiraReporterOperatorResolver(users JiraUserLookup, logger *slog.Logger) MatchOperatorResolver {
	return func(ctx context.Context, m TriggerMatch) string {
		accountID := m.Fields["jira_reporter_id"]
		email := m.Fields["jira_reporter_email"]
		if accountID == "" && email == "" {
			return ""
		}
		if op := jiraUserToOperator(lookupJiraUserIdentity(ctx, users, accountID, email)); op != "" {
			logger.Info("jira trigger: attributing auto-spawn to ticket reporter (fleet user)",
				"match", m.Key, "account_id", accountID, "operator", op)
			return op
		}
		// Directory miss: the reporter is not (yet) a fleet user bead. Fall back
		// to the reporter's raw email as an oidc: operator when it is a
		// daemon-accepted human shape, so the pod still boots attributed instead
		// of unresolvable (ka-0000004653).
		if op := reporterEmailOperator(email); op != "" {
			logger.Info("jira trigger: attributing auto-spawn to ticket reporter (email fallback)",
				"match", m.Key, "account_id", accountID, "operator", op)
			return op
		}
		return ""
	}
}

// reporterEmailOperator maps a Jira reporter email to an "oidc:<email>"
// operator id, but only when the result is a daemon-accepted human operator
// (IsHumanOperator: non-empty local part + dotted domain). Returns "" for an
// empty or malformed email so a bogus reporter address never becomes a value
// the kbeads sanitizer would reject as header_non_human.
func reporterEmailOperator(email string) string {
	email = strings.TrimSpace(email)
	if email == "" {
		return ""
	}
	op := "oidc:" + email
	if !IsHumanOperator(op) {
		return ""
	}
	return op
}

// Evaluate fetches all enabled JIRA trigger beads and evaluates each one.
func (e *JiraTriggerEvaluator) Evaluate(ctx context.Context) {
	e.engine.EvaluateSource(ctx, "jira")
}

// CatchUp populates the seen sets for all JIRA triggers from existing beads.
func (e *JiraTriggerEvaluator) CatchUp(ctx context.Context) {
	e.engine.CatchUpSource(ctx, "jira")
}
