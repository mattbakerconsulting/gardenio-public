package bridge

import (
	"context"
	"fmt"
	"log/slog"
	"strconv"
	"strings"
)

// DefaultAIApprovalRuleName is the name given to the approval rule applied to
// AI-authored merge requests. It is matched verbatim to keep rule creation
// idempotent across re-opens and updates. See DEVOPS-730.
const DefaultAIApprovalRuleName = "AI-generated change review"

// AIApprovalGitLab is the subset of GitLab client methods the enforcer needs.
// It is satisfied by *GitLabClient and mocked in tests.
type AIApprovalGitLab interface {
	ListMergeRequestApprovalRules(ctx context.Context, projectPath string, mrIID int) ([]GitLabMRApprovalRule, error)
	CreateMergeRequestApprovalRule(ctx context.Context, projectPath string, mrIID int, spec MRApprovalRuleSpec) (*GitLabMRApprovalRule, error)
}

// AIApprovalConfig configures the AI-authored-MR approval-rule enforcer.
//
// The enforcer is opt-in: with Enabled=false (the default) it is a no-op, so
// the mechanism can ship dark and be turned on once the team agrees the policy
// (required reviewer count, reviewer group) per DEVOPS-730. None of the policy
// values are hard-coded — they are all driven from this config.
type AIApprovalConfig struct {
	// Enabled gates the whole feature. When false the enforcer does nothing.
	Enabled bool
	// BotUsernames are the GitLab usernames whose authored MRs are considered
	// AI-authored. An MR whose author is none of these is left untouched.
	BotUsernames []string
	// RuleName is the approval-rule name; defaults to DefaultAIApprovalRuleName.
	RuleName string
	// ApprovalsRequired is the number of human approvals the rule demands
	// (the 1-vs-2 policy decision). Values < 1 are clamped to 1.
	ApprovalsRequired int
	// ReviewerGroupIDs / ReviewerUserIDs identify who may satisfy the rule —
	// typically the `ai-reviewers` group. At least one must be set for the
	// rule to be enforceable; with neither, the enforcer logs and skips so it
	// never creates a rule no human can satisfy.
	ReviewerGroupIDs []int
	ReviewerUserIDs  []int
}

// AIApprovalEnforcer applies a required human-approval rule to AI-authored MRs
// at open/reopen time. It is idempotent: an MR that already carries a rule with
// the configured name is left untouched, so webhook re-deliveries and MR
// updates never stack duplicate rules.
type AIApprovalEnforcer struct {
	gitlab AIApprovalGitLab
	cfg    AIApprovalConfig
	logger *slog.Logger
	bots   map[string]struct{}
}

// NewAIApprovalEnforcer builds an enforcer from config. Returns nil when the
// feature is disabled or misconfigured (no GitLab client, no bot usernames),
// so callers can treat a nil enforcer as "feature off".
func NewAIApprovalEnforcer(gitlab AIApprovalGitLab, cfg AIApprovalConfig, logger *slog.Logger) *AIApprovalEnforcer {
	if logger == nil {
		logger = slog.Default()
	}
	if !cfg.Enabled || gitlab == nil {
		return nil
	}
	bots := make(map[string]struct{}, len(cfg.BotUsernames))
	for _, u := range cfg.BotUsernames {
		if t := strings.TrimSpace(u); t != "" {
			bots[t] = struct{}{}
		}
	}
	if len(bots) == 0 {
		logger.Warn("ai-approval: enabled but no bot usernames configured — disabling")
		return nil
	}
	if cfg.RuleName == "" {
		cfg.RuleName = DefaultAIApprovalRuleName
	}
	if cfg.ApprovalsRequired < 1 {
		cfg.ApprovalsRequired = 1
	}
	return &AIApprovalEnforcer{gitlab: gitlab, cfg: cfg, logger: logger, bots: bots}
}

// IsAIAuthor reports whether the given MR author username is one of the
// configured bot identities.
func (e *AIApprovalEnforcer) IsAIAuthor(author string) bool {
	if e == nil {
		return false
	}
	_, ok := e.bots[strings.TrimSpace(author)]
	return ok
}

// EnsureRule applies the AI-review approval rule to an AI-authored MR if it is
// not already present. It is safe to call on every open/reopen/update webhook:
//   - non-AI authors are skipped,
//   - an MR that already has the rule is skipped,
//   - missing reviewer config is skipped (never creates an unsatisfiable rule).
//
// Errors are returned for the caller to log; the webhook path treats them as
// non-fatal so a transient GitLab failure never rejects the event.
func (e *AIApprovalEnforcer) EnsureRule(ctx context.Context, projectPath string, mrIID int, author string) error {
	if e == nil {
		return nil
	}
	if !e.IsAIAuthor(author) {
		return nil
	}
	if projectPath == "" || mrIID <= 0 {
		return nil
	}
	if len(e.cfg.ReviewerGroupIDs) == 0 && len(e.cfg.ReviewerUserIDs) == 0 {
		e.logger.Warn("ai-approval: no reviewer group/users configured — skipping rule application",
			"project", projectPath, "mr_iid", mrIID)
		return nil
	}

	existing, err := e.gitlab.ListMergeRequestApprovalRules(ctx, projectPath, mrIID)
	if err != nil {
		return fmt.Errorf("ai-approval: list rules %s!%d: %w", projectPath, mrIID, err)
	}
	for _, r := range existing {
		if strings.EqualFold(r.Name, e.cfg.RuleName) {
			e.logger.Debug("ai-approval: rule already present, skipping",
				"project", projectPath, "mr_iid", mrIID, "rule", r.Name)
			return nil
		}
	}

	spec := MRApprovalRuleSpec{
		Name:              e.cfg.RuleName,
		ApprovalsRequired: e.cfg.ApprovalsRequired,
		GroupIDs:          e.cfg.ReviewerGroupIDs,
		UserIDs:           e.cfg.ReviewerUserIDs,
	}
	if _, err := e.gitlab.CreateMergeRequestApprovalRule(ctx, projectPath, mrIID, spec); err != nil {
		return fmt.Errorf("ai-approval: create rule %s!%d: %w", projectPath, mrIID, err)
	}
	e.logger.Info("ai-approval: applied human-approval rule to AI-authored MR",
		"project", projectPath, "mr_iid", mrIID, "author", author,
		"rule", e.cfg.RuleName, "approvals_required", e.cfg.ApprovalsRequired)
	return nil
}

// ParseIntCSV parses a comma-separated list of integers, skipping blanks and
// non-numeric entries. Used to read reviewer group/user IDs from env config.
func ParseIntCSV(s string) []int {
	var out []int
	for part := range strings.SplitSeq(s, ",") {
		t := strings.TrimSpace(part)
		if t == "" {
			continue
		}
		if n, err := strconv.Atoi(t); err == nil {
			out = append(out, n)
		}
	}
	return out
}
