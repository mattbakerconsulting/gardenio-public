package bridge

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"sort"
	"strings"
)

// Branch ownership: may an agent WRITE to this MR's branch?
//
// Bug ka-0000004809 (Sky Bennett, monorepo !2038). Both merge-queue sweep
// triggers decided "is this branch mine to push?" from the MR AUTHOR USERNAME
// alone, as prose duplicated across two trigger prompts:
//
//	"rebase onto current main ONLY where the branch is gardenio-owned.
//	 NEVER touch a human-authored branch without being asked."
//
// !2038's author was `gardeniobot`, so the check said agent-owned and the sweep
// rebased + force-pushed it. Every other signal said human: 3 of its 5 commits
// were authored by a human, its assignee was a human, and that human had pushed
// commits, resolved all four review threads, and enabled the auto-merge-train
// the day before. That is the AGENT-OPENED / HUMAN-ADOPTED shape — an agent
// opens an MR at a human's request and the human then drives it — and because
// GitLab never updates `author` after the fact, an author-only test misclassifies
// that shape 100% of the time.
//
// This file is the fix: ONE predicate, in code, with unit tests, replacing the
// prose in both trigger prompts. Two rules, in order:
//
//  1. WIDER OWNERSHIP. A branch is agent-owned only when the author, EVERY
//     commit author AND committer, and every assignee are agent identities. Any
//     human fingerprint anywhere makes it human-owned. Missing data fails
//     CLOSED (force-push is destructive; "I could not tell" must not authorize).
//  2. GUARD, regardless of the ownership verdict (the cheap belt-and-braces
//     required by decision ka-0000007743): never force-push an MR that is
//     APPROVED or ASSIGNED. Those are the states where a force-push can reset an
//     approval or invalidate a reviewer's checkout, and the owner is right there
//     to press the button themselves. "Approved" here means approved by a
//     HUMAN — a bot self-approval costs no human anything and must not stall
//     the sweep lane (monorepo !1998).

// BranchOwner is the ownership verdict for an MR's source branch.
type BranchOwner string

const (
	// BranchOwnerAgent means every ownership signal is an agent identity.
	BranchOwnerAgent BranchOwner = "agent"
	// BranchOwnerHuman means at least one signal is a human identity.
	BranchOwnerHuman BranchOwner = "human"
	// BranchOwnerUnknown means the signals could not be read (e.g. the commit
	// list was unavailable). Treated as human for write purposes: fail closed.
	BranchOwnerUnknown BranchOwner = "unknown"
)

// DefaultAgentGitLabUsernames are the GitLab usernames agents push under. An MR
// whose author/assignee is none of these carries a human fingerprint.
var DefaultAgentGitLabUsernames = []string{
	"gardeniobot",
	"gardenio",
	"project_14176856_bot", // GitLab group access-token bot identities
}

// DefaultAgentCommitEmails are the git author/committer emails agent commits
// carry. Matching is exact and case-insensitive.
var DefaultAgentCommitEmails = []string{
	"gardenio@acme.example",
}

// AgentIdentity is the set of identities considered "an agent" when deciding
// branch ownership. Empty fields fall back to the Default* vars above, so
// callers that do not care can pass the zero value.
type AgentIdentity struct {
	// Usernames are GitLab usernames (MR author, assignees).
	Usernames []string
	// Emails are git author/committer emails on branch commits.
	Emails []string
	// UsernamePrefixes matches bot usernames whose suffix varies per token,
	// e.g. "group_14176856_bot_<hash>". Case-insensitive prefix match.
	UsernamePrefixes []string
}

// DefaultAgentIdentity is the identity set used when none is supplied.
func DefaultAgentIdentity() AgentIdentity {
	return AgentIdentity{
		Usernames:        DefaultAgentGitLabUsernames,
		Emails:           DefaultAgentCommitEmails,
		UsernamePrefixes: []string{"group_14176856_bot_", "project_14176856_bot_"},
	}
}

func (ai AgentIdentity) withDefaults() AgentIdentity {
	def := DefaultAgentIdentity()
	if len(ai.Usernames) == 0 {
		ai.Usernames = def.Usernames
	}
	if len(ai.Emails) == 0 {
		ai.Emails = def.Emails
	}
	if len(ai.UsernamePrefixes) == 0 {
		ai.UsernamePrefixes = def.UsernamePrefixes
	}
	return ai
}

// IsAgentUsername reports whether a GitLab username is an agent identity.
func (ai AgentIdentity) IsAgentUsername(username string) bool {
	u := strings.ToLower(strings.TrimSpace(username))
	if u == "" {
		return false
	}
	ai = ai.withDefaults()
	for _, known := range ai.Usernames {
		if strings.EqualFold(u, strings.TrimSpace(known)) {
			return true
		}
	}
	for _, pfx := range ai.UsernamePrefixes {
		p := strings.ToLower(strings.TrimSpace(pfx))
		if p != "" && strings.HasPrefix(u, p) {
			return true
		}
	}
	return false
}

// noreplyEmailHosts are the hosts on which a commit email's LOCAL PART is a
// platform username rather than a mailbox. GitLab attributes bot/web-UI commits
// to `<username>@noreply.<host>` (sometimes `<id>-<username>@...`), so an
// identity known by username must also be recognized in that form — otherwise
// an access-token bot's own commits read as human. Caught on monorepo !1843,
// whose commits are `group_14176856_bot_<hash>@noreply.gitlab.com`.
var noreplyEmailHosts = []string{"noreply.gitlab.com", "users.noreply.github.com", "noreply.github.com"}

// IsAgentEmail reports whether a git author/committer email is an agent identity.
//
// Two forms match: an exact address in Emails, and a platform noreply address
// whose local part is an agent USERNAME (see noreplyEmailHosts).
func (ai AgentIdentity) IsAgentEmail(email string) bool {
	e := strings.ToLower(strings.TrimSpace(email))
	if e == "" {
		return false
	}
	for _, known := range ai.withDefaults().Emails {
		if strings.EqualFold(e, strings.TrimSpace(known)) {
			return true
		}
	}
	if local, host, ok := strings.Cut(e, "@"); ok {
		for _, nh := range noreplyEmailHosts {
			if host != nh {
				continue
			}
			// GitLab prefixes some noreply locals with the numeric user id.
			if _, rest, cut := strings.Cut(local, "-"); cut && isAllDigits(local[:len(local)-len(rest)-1]) {
				if ai.IsAgentUsername(rest) {
					return true
				}
			}
			return ai.IsAgentUsername(local)
		}
	}
	return false
}

// isAllDigits reports whether s is non-empty and entirely ASCII digits.
func isAllDigits(s string) bool {
	if s == "" {
		return false
	}
	for _, r := range s {
		if r < '0' || r > '9' {
			return false
		}
	}
	return true
}

// BranchCommit is one commit on an MR's source branch, reduced to the identity
// fields ownership keys on. BOTH author and committer matter: a human who
// rebases agent commits leaves agent authors but a human COMMITTER, and that is
// still a human actively working the branch.
type BranchCommit struct {
	ShortID        string
	AuthorName     string
	AuthorEmail    string
	CommitterName  string
	CommitterEmail string
}

// BranchOwnershipInput is everything the predicate reads. It is deliberately
// plain data so the decision is a pure function and fully unit-testable —
// the whole point of lifting it out of trigger prose.
type BranchOwnershipInput struct {
	// ProjectPath / MRIID identify the MR, for messages only.
	ProjectPath string
	MRIID       int
	// AuthorUsername is the MR author (the ONLY signal the old prose read).
	AuthorUsername string
	// AssigneeUsernames are the MR's current assignees.
	AssigneeUsernames []string
	// Commits are the commits on the source branch. An EMPTY slice means "not
	// read" and fails closed; use CommitsRead to assert a genuinely empty branch.
	Commits []BranchCommit
	// CommitsRead records that the commit list was fetched successfully, so an
	// MR with genuinely zero commits is distinguishable from an unread list.
	CommitsRead bool
	// Approved is true when the MR carries at least one approval.
	Approved   bool
	ApprovedBy []string
	// Identity is the agent identity set; zero value uses DefaultAgentIdentity.
	Identity AgentIdentity
}

// BranchOwnershipVerdict is the answer: who owns the branch, and may an agent
// force-push it.
type BranchOwnershipVerdict struct {
	// Owner is the widened ownership verdict.
	Owner BranchOwner
	// PushAllowed is the ONLY field a caller should gate a force-push on. It is
	// Owner==agent AND no guard fired.
	PushAllowed bool
	// Guard names the guard that vetoed an otherwise-allowed push
	// ("approved", "assigned"), or "" when none fired.
	Guard string
	// HumanSignals / AgentSignals are the human-readable evidence, so a report
	// can say WHY rather than just yes/no.
	HumanSignals []string
	AgentSignals []string
	// Reason is a one-line summary suitable for a log line or MR comment.
	Reason string
	// humanApprovers is the approver subset that is not an agent identity,
	// retained for the reason line.
	humanApprovers []string
}

// String renders the verdict for a terminal.
func (v BranchOwnershipVerdict) String() string {
	if v.PushAllowed {
		return "MAY PUSH — " + v.Reason
	}
	return "DO NOT PUSH — " + v.Reason
}

// EvaluateBranchOwnership decides whether an agent may write to an MR's source
// branch. Pure function: no I/O, no clock. See the file header for the two
// rules and the !2038 failure it encodes.
func EvaluateBranchOwnership(in BranchOwnershipInput) BranchOwnershipVerdict {
	id := in.Identity.withDefaults()
	v := BranchOwnershipVerdict{}

	// --- Signal 1: MR author (the old, insufficient test). ---
	switch {
	case strings.TrimSpace(in.AuthorUsername) == "":
		v.HumanSignals = append(v.HumanSignals, "MR author is unknown (unread)")
	case id.IsAgentUsername(in.AuthorUsername):
		v.AgentSignals = append(v.AgentSignals, "MR author @"+in.AuthorUsername+" is an agent identity")
	default:
		v.HumanSignals = append(v.HumanSignals, "MR author @"+in.AuthorUsername+" is not an agent identity")
	}

	// --- Signal 2: assignees. A human assignee has ADOPTED the MR. ---
	var humanAssignees []string
	for _, a := range dedupeStrings(in.AssigneeUsernames) {
		if id.IsAgentUsername(a) {
			continue
		}
		humanAssignees = append(humanAssignees, a)
	}
	if len(humanAssignees) > 0 {
		v.HumanSignals = append(v.HumanSignals,
			fmt.Sprintf("assigned to %s — a human owns this MR now", atJoin(humanAssignees)))
	} else if len(in.AssigneeUsernames) > 0 {
		v.AgentSignals = append(v.AgentSignals, "assignees are all agent identities")
	}

	// --- Signal 3: commit authors AND committers on the branch. ---
	// This is the signal !2038 needed and nothing read: 3 of 5 commits were
	// authored by a human on an MR whose author field said gardeniobot.
	switch {
	case !in.CommitsRead:
		v.HumanSignals = append(v.HumanSignals,
			"branch commit authors could NOT be read — failing closed (a force-push is destructive; unknown must never authorize it)")
	case len(in.Commits) == 0:
		v.HumanSignals = append(v.HumanSignals, "branch has no commits — nothing establishes agent ownership")
	default:
		humans := map[string]int{}
		for _, c := range in.Commits {
			if !id.IsAgentEmail(c.AuthorEmail) {
				humans[displayIdentity(c.AuthorName, c.AuthorEmail)]++
			} else if c.CommitterEmail != "" && !id.IsAgentEmail(c.CommitterEmail) {
				// Author is an agent but a human committed it — a human rebase.
				humans[displayIdentity(c.CommitterName, c.CommitterEmail)+" (committer)"]++
			}
		}
		if len(humans) > 0 {
			names := make([]string, 0, len(humans))
			total := 0
			for n, cnt := range humans {
				names = append(names, n)
				total += cnt
			}
			sort.Strings(names)
			v.HumanSignals = append(v.HumanSignals,
				fmt.Sprintf("%d of %d branch commits carry a human author/committer: %s",
					total, len(in.Commits), strings.Join(names, ", ")))
		} else {
			v.AgentSignals = append(v.AgentSignals,
				fmt.Sprintf("all %d branch commits are agent-authored", len(in.Commits)))
		}
	}

	// --- Approvals feed the GUARD only, never the ownership verdict. ---
	// A human APPROVING an MR is review state, not branch ownership: it says
	// nothing about who writes the branch. Keeping it out of the ownership
	// signals keeps the two rules independent — ownership answers "whose
	// branch is this", the guard answers "is there review state a force-push
	// would destroy".
	//
	// Only a HUMAN approval is worth protecting. The group access-token bot
	// approving a bot MR (monorepo !1998) costs no human anything if it is
	// reset, so it must not stall the sweep lane.
	var humanApprovers []string
	for _, a := range dedupeStrings(in.ApprovedBy) {
		if !id.IsAgentUsername(a) {
			humanApprovers = append(humanApprovers, a)
		}
	}
	// Approved=true with an unreadable approver list (GitLab CE reports only
	// the boolean) is treated as a human approval: fail closed.
	humanApproved := len(humanApprovers) > 0 || (in.Approved && len(in.ApprovedBy) == 0)
	if !humanApproved && len(in.ApprovedBy) > 0 {
		v.AgentSignals = append(v.AgentSignals,
			fmt.Sprintf("approved only by agent identities (%s) — not a human approval", atJoin(in.ApprovedBy)))
	}

	// --- Ownership verdict: any human fingerprint wins. ---
	switch {
	case len(v.HumanSignals) == 0:
		v.Owner = BranchOwnerAgent
	case !in.CommitsRead:
		v.Owner = BranchOwnerUnknown
	default:
		v.Owner = BranchOwnerHuman
	}

	// --- Guards (rule 2): applied even when ownership says agent. ---
	if humanApproved {
		v.Guard = "approved"
	} else if len(humanAssignees) > 0 {
		v.Guard = "assigned"
	}

	v.humanApprovers = humanApprovers
	v.PushAllowed = v.Owner == BranchOwnerAgent && v.Guard == ""
	v.Reason = branchOwnershipReason(in, v)
	return v
}

// branchOwnershipReason composes the one-line summary.
func branchOwnershipReason(in BranchOwnershipInput, v BranchOwnershipVerdict) string {
	mr := "this MR"
	if in.ProjectPath != "" && in.MRIID > 0 {
		mr = fmt.Sprintf("%s!%d", in.ProjectPath, in.MRIID)
	} else if in.MRIID > 0 {
		mr = fmt.Sprintf("!%d", in.MRIID)
	}
	switch {
	case v.PushAllowed:
		return fmt.Sprintf("%s is agent-owned and carries no approval or human assignee: %s",
			mr, strings.Join(v.AgentSignals, "; "))
	case v.Guard == "approved":
		who := ""
		if len(v.humanApprovers) > 0 {
			who = " (by " + atJoin(v.humanApprovers) + ")"
		}
		return fmt.Sprintf("%s is APPROVED%s — never force-push an approved MR; comment with the diagnosis and let the owner press the button. Ownership verdict: %s",
			mr, who, v.Owner)
	case v.Guard == "assigned":
		return fmt.Sprintf("%s is ASSIGNED to a human — never force-push under someone actively iterating. %s",
			mr, strings.Join(v.HumanSignals, "; "))
	default:
		return fmt.Sprintf("%s is %s-owned: %s", mr, v.Owner, strings.Join(v.HumanSignals, "; "))
	}
}

// displayIdentity renders "Name <email>" for whichever parts are present.
func displayIdentity(name, email string) string {
	name, email = strings.TrimSpace(name), strings.TrimSpace(email)
	switch {
	case name != "" && email != "":
		return fmt.Sprintf("%s <%s>", name, email)
	case name != "":
		return name
	case email != "":
		return email
	default:
		return "unknown"
	}
}

// atJoin renders usernames as "@a, @b".
func atJoin(users []string) string {
	out := make([]string, 0, len(users))
	for _, u := range users {
		if t := strings.TrimSpace(u); t != "" {
			out = append(out, "@"+t)
		}
	}
	return strings.Join(out, ", ")
}

// dedupeStrings returns the non-empty, trimmed, order-preserving unique values.
func dedupeStrings(in []string) []string {
	seen := map[string]struct{}{}
	out := make([]string, 0, len(in))
	for _, s := range in {
		t := strings.TrimSpace(s)
		if t == "" {
			continue
		}
		k := strings.ToLower(t)
		if _, dup := seen[k]; dup {
			continue
		}
		seen[k] = struct{}{}
		out = append(out, t)
	}
	return out
}

// AssigneeUsernames returns the MR's assignee usernames, merging the legacy
// single `assignee` field with the multi-assignee `assignees` array.
func (mr *GitLabMR) AssigneeUsernames() []string {
	if mr == nil {
		return nil
	}
	var out []string
	if mr.Assignee != nil && mr.Assignee.Username != "" {
		out = append(out, mr.Assignee.Username)
	}
	for _, a := range mr.Assignees {
		if a.Username != "" {
			out = append(out, a.Username)
		}
	}
	return dedupeStrings(out)
}

// AuthorUsername returns the MR author's username, or "" when unknown.
func (mr *GitLabMR) AuthorUsername() string {
	if mr == nil || mr.Author == nil {
		return ""
	}
	return mr.Author.Username
}

// BranchOwnershipGitLab is the subset of GitLab reads the fetcher needs, so
// tests can supply a fake.
type BranchOwnershipGitLab interface {
	GetMergeRequestByPath(ctx context.Context, projectPath string, mrIID int) (*GitLabMR, error)
	ListMergeRequestCommits(ctx context.Context, projectPath string, mrIID int) ([]BranchCommit, error)
	ListMergeRequestApprovals(ctx context.Context, projectPath string, mrIID int) (*GitLabMRApprovals, error)
}

// FetchBranchOwnership gathers the ownership signals for an MR from GitLab and
// evaluates them. All three reads are plain GETs.
//
// A failed commit-list read is NOT fatal: it is recorded as CommitsRead=false,
// which the predicate turns into a fail-closed "unknown" verdict. A failed MR
// read IS fatal — without the MR there is nothing to judge. A failed approvals
// read (404 on Community Edition is already normalized to a zero value by
// ListMergeRequestApprovals) leaves Approved=false, so the ownership rule still
// governs.
func FetchBranchOwnership(ctx context.Context, gl BranchOwnershipGitLab, projectPath string, mrIID int, identity AgentIdentity) (BranchOwnershipVerdict, BranchOwnershipInput, error) {
	in := BranchOwnershipInput{ProjectPath: projectPath, MRIID: mrIID, Identity: identity}
	mr, err := gl.GetMergeRequestByPath(ctx, projectPath, mrIID)
	if err != nil {
		return BranchOwnershipVerdict{}, in, fmt.Errorf("branch-ownership: fetching %s!%d: %w", projectPath, mrIID, err)
	}
	in.AuthorUsername = mr.AuthorUsername()
	in.AssigneeUsernames = mr.AssigneeUsernames()

	if commits, err := gl.ListMergeRequestCommits(ctx, projectPath, mrIID); err == nil {
		in.Commits, in.CommitsRead = commits, true
	}
	if apr, err := gl.ListMergeRequestApprovals(ctx, projectPath, mrIID); err == nil && apr != nil {
		in.Approved = apr.Approved || len(apr.ApprovedBy) > 0
		in.ApprovedBy = apr.ApprovedBy
	}
	return EvaluateBranchOwnership(in), in, nil
}

// mrCommitsResponse is the subset of GET .../merge_requests/:iid/commits we read.
type mrCommitsResponse []struct {
	ShortID        string `json:"short_id"`
	AuthorName     string `json:"author_name"`
	AuthorEmail    string `json:"author_email"`
	CommitterName  string `json:"committer_name"`
	CommitterEmail string `json:"committer_email"`
}

// ListMergeRequestCommits fetches the commits on an MR's source branch.
// GET /projects/:id/merge_requests/:iid/commits
func (c *GitLabClient) ListMergeRequestCommits(ctx context.Context, projectPath string, mrIID int) ([]BranchCommit, error) {
	encoded := url.PathEscape(projectPath)
	path := fmt.Sprintf("/api/v4/projects/%s/merge_requests/%d/commits?per_page=100", encoded, mrIID)
	var resp mrCommitsResponse
	if err := c.doJSON(ctx, http.MethodGet, path, nil, &resp); err != nil {
		return nil, fmt.Errorf("GitLab list MR commits %s!%d: %w", projectPath, mrIID, err)
	}
	out := make([]BranchCommit, 0, len(resp))
	for _, cm := range resp {
		out = append(out, BranchCommit{
			ShortID:        cm.ShortID,
			AuthorName:     cm.AuthorName,
			AuthorEmail:    cm.AuthorEmail,
			CommitterName:  cm.CommitterName,
			CommitterEmail: cm.CommitterEmail,
		})
	}
	return out, nil
}
