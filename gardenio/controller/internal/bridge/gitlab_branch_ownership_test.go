package bridge

import (
	"context"
	"errors"
	"strings"
	"testing"
)

// agentCommit / humanCommit build the two commit shapes the predicate keys on.
func agentCommit(id string) BranchCommit {
	return BranchCommit{
		ShortID: id, AuthorName: "Gardenio Agent", AuthorEmail: "gardenio@acme.example",
		CommitterName: "Gardenio Agent", CommitterEmail: "gardenio@acme.example",
	}
}

func humanCommit(id, name, email string) BranchCommit {
	return BranchCommit{
		ShortID: id, AuthorName: name, AuthorEmail: email,
		CommitterName: name, CommitterEmail: email,
	}
}

// TestEvaluateBranchOwnership_MR2038 is the regression that names the bug.
//
// monorepo !2038 (bug ka-0000004809): author gardeniobot, so the old
// author-only prose said "agent-owned, push away" and the sweep force-pushed
// it. 3 of 5 commits were authored by a human, the assignee was a human, and
// jackiep had approved. Both the widened ownership test AND either guard must
// independently refuse this MR.
func TestEvaluateBranchOwnership_MR2038(t *testing.T) {
	in := BranchOwnershipInput{
		ProjectPath:       "Acme/Platform/monorepo",
		MRIID:             2038,
		AuthorUsername:    "gardeniobot", // the ONLY signal the old test read
		AssigneeUsernames: []string{"ak-acme"},
		CommitsRead:       true,
		Commits: []BranchCommit{
			agentCommit("aaa1111"),
			humanCommit("bbb2222", "Avery Locke", "avery.locke@acme.example"),
			humanCommit("ccc3333", "Avery Locke", "avery.locke@acme.example"),
			humanCommit("ddd4444", "Avery Locke", "avery.locke@acme.example"),
			agentCommit("eee5555"),
		},
		Approved:   true,
		ApprovedBy: []string{"jackiep"},
	}

	v := EvaluateBranchOwnership(in)

	if v.PushAllowed {
		t.Fatalf("!2038 must NOT be push-allowed; got %+v", v)
	}
	if v.Owner != BranchOwnerHuman {
		t.Errorf("owner = %q, want %q (human-adopted)", v.Owner, BranchOwnerHuman)
	}
	if v.Guard != "approved" {
		t.Errorf("guard = %q, want %q", v.Guard, "approved")
	}
	// The old test's signal is still reported — as an AGENT signal that is
	// nonetheless overridden. That is the whole point: author alone is not enough.
	if !containsOwnershipSignal(v.AgentSignals, "MR author @gardeniobot") {
		t.Errorf("expected the author signal to be reported; got %v", v.AgentSignals)
	}
	if !containsOwnershipSignal(v.HumanSignals, "Avery Locke") {
		t.Errorf("expected the human commit-author signal; got %v", v.HumanSignals)
	}
	if !containsOwnershipSignal(v.HumanSignals, "@ak-acme") {
		t.Errorf("expected the human assignee signal; got %v", v.HumanSignals)
	}
	if !strings.Contains(v.Reason, "APPROVED") {
		t.Errorf("reason should lead with the approval guard; got %q", v.Reason)
	}
}

// TestEvaluateBranchOwnership_AuthorAloneIsNotEnough isolates the defect: with
// EVERY other signal stripped to neutral except a human commit author, the
// verdict must still be human. This is the assertion the prose could not make.
func TestEvaluateBranchOwnership_AuthorAloneIsNotEnough(t *testing.T) {
	v := EvaluateBranchOwnership(BranchOwnershipInput{
		AuthorUsername: "gardeniobot",
		CommitsRead:    true,
		Commits:        []BranchCommit{agentCommit("aaa1111"), humanCommit("bbb2222", "Avery Locke", "avery.locke@acme.example")},
	})
	if v.PushAllowed || v.Owner != BranchOwnerHuman {
		t.Fatalf("one human commit author must flip ownership to human; got %+v", v)
	}
	if !containsOwnershipSignal(v.HumanSignals, "1 of 2 branch commits") {
		t.Errorf("expected the N-of-M commit count; got %v", v.HumanSignals)
	}
}

func TestEvaluateBranchOwnership_Table(t *testing.T) {
	tests := []struct {
		name        string
		in          BranchOwnershipInput
		wantOwner   BranchOwner
		wantPush    bool
		wantGuard   string
		wantSignals []string
	}{
		{
			name: "genuinely agent-owned MR is push-allowed (!2191/!2200 shape)",
			in: BranchOwnershipInput{
				ProjectPath: "Acme/Platform/monorepo", MRIID: 2191,
				AuthorUsername: "gardeniobot", CommitsRead: true,
				Commits: []BranchCommit{agentCommit("aaa1111"), agentCommit("bbb2222")},
			},
			wantOwner: BranchOwnerAgent, wantPush: true,
			wantSignals: []string{"all 2 branch commits are agent-authored"},
		},
		{
			name: "human MR author is refused even with all-agent commits",
			in: BranchOwnershipInput{
				AuthorUsername: "ak-acme", CommitsRead: true,
				Commits: []BranchCommit{agentCommit("aaa1111")},
			},
			wantOwner: BranchOwnerHuman, wantPush: false,
		},
		{
			name: "human REBASED agent commits — human committer, agent authors",
			in: BranchOwnershipInput{
				AuthorUsername: "gardeniobot", CommitsRead: true,
				Commits: []BranchCommit{{
					ShortID: "aaa1111", AuthorName: "Gardenio Agent", AuthorEmail: "gardenio@acme.example",
					CommitterName: "Avery Locke", CommitterEmail: "avery.locke@acme.example",
				}},
			},
			wantOwner: BranchOwnerHuman, wantPush: false,
			wantSignals: []string{"(committer)"},
		},
		{
			name: "unread commit list fails CLOSED as unknown",
			in: BranchOwnershipInput{
				AuthorUsername: "gardeniobot", CommitsRead: false,
			},
			wantOwner: BranchOwnerUnknown, wantPush: false,
			wantSignals: []string{"failing closed"},
		},
		{
			name: "guard: approved agent-owned MR is NOT push-allowed",
			in: BranchOwnershipInput{
				AuthorUsername: "gardeniobot", CommitsRead: true,
				Commits:  []BranchCommit{agentCommit("aaa1111")},
				Approved: true, ApprovedBy: []string{"jackiep"},
			},
			wantOwner: BranchOwnerAgent, wantPush: false, wantGuard: "approved",
		},
		{
			name: "guard: agent-owned MR assigned to a human is NOT push-allowed",
			in: BranchOwnershipInput{
				AuthorUsername: "gardeniobot", AssigneeUsernames: []string{"ak-acme"},
				CommitsRead: true, Commits: []BranchCommit{agentCommit("aaa1111")},
			},
			// A human assignee is itself a human ownership signal, so the owner
			// flips too — but the guard is what a report should lead with.
			wantOwner: BranchOwnerHuman, wantPush: false, wantGuard: "assigned",
		},
		{
			name: "agent assignee does not flip ownership",
			in: BranchOwnershipInput{
				AuthorUsername: "gardeniobot", AssigneeUsernames: []string{"gardeniobot"},
				CommitsRead: true, Commits: []BranchCommit{agentCommit("aaa1111")},
			},
			wantOwner: BranchOwnerAgent, wantPush: true,
		},
		{
			name: "empty branch with commits read is refused (nothing proves ownership)",
			in: BranchOwnershipInput{
				AuthorUsername: "gardeniobot", CommitsRead: true, Commits: nil,
			},
			wantOwner: BranchOwnerHuman, wantPush: false,
			wantSignals: []string{"no commits"},
		},
		{
			name: "unknown MR author is refused",
			in: BranchOwnershipInput{
				AuthorUsername: "", CommitsRead: true,
				Commits: []BranchCommit{agentCommit("aaa1111")},
			},
			wantOwner: BranchOwnerHuman, wantPush: false,
			wantSignals: []string{"MR author is unknown"},
		},
		{
			name: "bot SELF-approval does not trip the guard (!1998 shape)",
			in: BranchOwnershipInput{
				ProjectPath: "Acme/Platform/monorepo", MRIID: 1998,
				AuthorUsername: "gardeniobot", CommitsRead: true,
				Commits:  []BranchCommit{agentCommit("aaa1111")},
				Approved: true,
				ApprovedBy: []string{
					"group_14176856_bot_259bb18fe9d9d91bf5bbb2a4670f7eaa",
				},
			},
			wantOwner: BranchOwnerAgent, wantPush: true, wantGuard: "",
			wantSignals: []string{"not a human approval"},
		},
		{
			name: "MIXED approvals: one human approver among bots still guards",
			in: BranchOwnershipInput{
				AuthorUsername: "gardeniobot", CommitsRead: true,
				Commits:  []BranchCommit{agentCommit("aaa1111")},
				Approved: true,
				ApprovedBy: []string{
					"group_14176856_bot_259bb18fe9d9d91bf5bbb2a4670f7eaa",
					"jackiep",
				},
			},
			wantOwner: BranchOwnerAgent, wantPush: false, wantGuard: "approved",
		},
		{
			name: "approved boolean with UNREADABLE approver list fails closed",
			in: BranchOwnershipInput{
				AuthorUsername: "gardeniobot", CommitsRead: true,
				Commits:  []BranchCommit{agentCommit("aaa1111")},
				Approved: true, ApprovedBy: nil,
			},
			wantOwner: BranchOwnerAgent, wantPush: false, wantGuard: "approved",
		},
		{
			name: "a human APPROVAL alone does not make the branch human-owned",
			in: BranchOwnershipInput{
				AuthorUsername: "gardeniobot", CommitsRead: true,
				Commits:  []BranchCommit{agentCommit("aaa1111")},
				Approved: true, ApprovedBy: []string{"jackiep"},
			},
			// Ownership and review state are independent rules: jackiep
			// reviewing does not make it jackiep's branch. The guard is what
			// refuses the push.
			wantOwner: BranchOwnerAgent, wantPush: false, wantGuard: "approved",
		},
		{
			name: "access-token bot noreply commit email counts as an agent (!1843 shape)",
			in: BranchOwnershipInput{
				AuthorUsername: "gardeniobot", CommitsRead: true,
				Commits: []BranchCommit{{
					ShortID:        "aaa1111",
					AuthorName:     "gardenio",
					AuthorEmail:    "group_14176856_bot_259bb18fe9d9d91bf5bbb2a4670f7eaa@noreply.gitlab.com",
					CommitterName:  "gardenio",
					CommitterEmail: "group_14176856_bot_259bb18fe9d9d91bf5bbb2a4670f7eaa@noreply.gitlab.com",
				}},
			},
			wantOwner: BranchOwnerAgent, wantPush: true,
		},
		{
			name: "group access-token bot prefix counts as an agent",
			in: BranchOwnershipInput{
				AuthorUsername: "group_14176856_bot_259bb18fe9d9d91bf5bbb2a4670f7eaa",
				CommitsRead:    true, Commits: []BranchCommit{agentCommit("aaa1111")},
			},
			wantOwner: BranchOwnerAgent, wantPush: true,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			v := EvaluateBranchOwnership(tc.in)
			if v.Owner != tc.wantOwner {
				t.Errorf("owner = %q, want %q (signals: human=%v agent=%v)", v.Owner, tc.wantOwner, v.HumanSignals, v.AgentSignals)
			}
			if v.PushAllowed != tc.wantPush {
				t.Errorf("PushAllowed = %v, want %v (reason: %s)", v.PushAllowed, tc.wantPush, v.Reason)
			}
			if v.Guard != tc.wantGuard {
				t.Errorf("guard = %q, want %q", v.Guard, tc.wantGuard)
			}
			for _, want := range tc.wantSignals {
				if !containsOwnershipSignal(v.HumanSignals, want) && !containsOwnershipSignal(v.AgentSignals, want) {
					t.Errorf("missing signal %q; human=%v agent=%v", want, v.HumanSignals, v.AgentSignals)
				}
			}
			if v.Reason == "" {
				t.Error("Reason must never be empty — reports quote it")
			}
			if got := v.String(); tc.wantPush != strings.HasPrefix(got, "MAY PUSH") {
				t.Errorf("String() = %q, inconsistent with PushAllowed=%v", got, tc.wantPush)
			}
		})
	}
}

func TestAgentIdentity(t *testing.T) {
	id := AgentIdentity{}
	if !id.IsAgentUsername("gardeniobot") || !id.IsAgentUsername("GardenioBot") {
		t.Error("zero-value identity must fall back to defaults and be case-insensitive")
	}
	if id.IsAgentUsername("ak-acme") || id.IsAgentUsername("") {
		t.Error("human / empty usernames must not match")
	}
	if !id.IsAgentEmail("Gardenio@acme.example") {
		t.Error("agent email match must be case-insensitive")
	}
	if id.IsAgentEmail("avery.locke@acme.example") {
		t.Error("a human @acme.example email must NOT match — matching is exact, not by domain")
	}
	if id.IsAgentEmail("") {
		t.Error("empty email must not match")
	}
	// Platform noreply form: local part is a USERNAME, not a mailbox. The
	// group access-token bot commits as this on monorepo !1843, and reading it
	// as human was a false positive found against live data.
	for _, agentNoreply := range []string{
		"group_14176856_bot_259bb18fe9d9d91bf5bbb2a4670f7eaa@noreply.gitlab.com",
		"gardeniobot@noreply.gitlab.com",
		"12345-gardeniobot@noreply.gitlab.com", // GitLab's <id>-<username> variant
	} {
		if !id.IsAgentEmail(agentNoreply) {
			t.Errorf("agent noreply email %q must match", agentNoreply)
		}
	}
	for _, humanNoreply := range []string{
		"ak-acme@noreply.gitlab.com",
		"gardeniobot@evil.example.com", // right local part, wrong host
		"12345-ak-acme@noreply.gitlab.com",
	} {
		if id.IsAgentEmail(humanNoreply) {
			t.Errorf("non-agent noreply email %q must NOT match", humanNoreply)
		}
	}
	// An explicit identity set overrides the defaults for that field only.
	custom := AgentIdentity{Usernames: []string{"otherbot"}}
	if custom.IsAgentUsername("gardeniobot") {
		t.Error("explicit Usernames must replace the default list")
	}
	if !custom.IsAgentUsername("otherbot") || !custom.IsAgentEmail("gardenio@acme.example") {
		t.Error("unset fields must still fall back to defaults")
	}
}

func TestMRAssigneeAndAuthorAccessors(t *testing.T) {
	var nilMR *GitLabMR
	if nilMR.AuthorUsername() != "" || nilMR.AssigneeUsernames() != nil {
		t.Error("nil MR accessors must be safe")
	}
	mr := &GitLabMR{
		Author:    &GitLabUser{Username: "gardeniobot"},
		Assignee:  &GitLabUser{Username: "ak-acme"},
		Assignees: []GitLabUser{{Username: "ak-acme"}, {Username: "jackiep"}, {}},
	}
	if got := mr.AuthorUsername(); got != "gardeniobot" {
		t.Errorf("AuthorUsername = %q", got)
	}
	got := mr.AssigneeUsernames()
	if len(got) != 2 || got[0] != "ak-acme" || got[1] != "jackiep" {
		t.Errorf("AssigneeUsernames = %v, want deduped [ak-acme jackiep]", got)
	}
}

// --- FetchBranchOwnership ---

type fakeOwnershipGitLab struct {
	mr        *GitLabMR
	mrErr     error
	commits   []BranchCommit
	commitErr error
	approvals *GitLabMRApprovals
	aprErr    error
}

func (f *fakeOwnershipGitLab) GetMergeRequestByPath(context.Context, string, int) (*GitLabMR, error) {
	return f.mr, f.mrErr
}

func (f *fakeOwnershipGitLab) ListMergeRequestCommits(context.Context, string, int) ([]BranchCommit, error) {
	return f.commits, f.commitErr
}

func (f *fakeOwnershipGitLab) ListMergeRequestApprovals(context.Context, string, int) (*GitLabMRApprovals, error) {
	return f.approvals, f.aprErr
}

func TestFetchBranchOwnership(t *testing.T) {
	ctx := context.Background()

	t.Run("assembles all three signals", func(t *testing.T) {
		gl := &fakeOwnershipGitLab{
			mr: &GitLabMR{
				Author:   &GitLabUser{Username: "gardeniobot"},
				Assignee: &GitLabUser{Username: "ak-acme"},
			},
			commits:   []BranchCommit{humanCommit("aaa1111", "Avery Locke", "avery.locke@acme.example")},
			approvals: &GitLabMRApprovals{ApprovedBy: []string{"jackiep"}},
		}
		v, in, err := FetchBranchOwnership(ctx, gl, "Acme/Platform/monorepo", 2038, AgentIdentity{})
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if v.PushAllowed {
			t.Error("must refuse the !2038 shape")
		}
		if !in.CommitsRead || len(in.Commits) != 1 {
			t.Errorf("commits not carried through: %+v", in)
		}
		// approvals.Approved is false but approved_by is non-empty: the fetcher
		// ORs them, so a CE instance that only reports approved_by still guards.
		if !in.Approved || v.Guard != "approved" {
			t.Errorf("approved_by alone must set the approval guard; got Approved=%v guard=%q", in.Approved, v.Guard)
		}
	})

	t.Run("commit-read failure fails closed, not fatal", func(t *testing.T) {
		gl := &fakeOwnershipGitLab{
			mr:        &GitLabMR{Author: &GitLabUser{Username: "gardeniobot"}},
			commitErr: errors.New("500 boom"),
			approvals: &GitLabMRApprovals{},
		}
		v, in, err := FetchBranchOwnership(ctx, gl, "p", 1, AgentIdentity{})
		if err != nil {
			t.Fatalf("a commit-read failure must not be fatal: %v", err)
		}
		if in.CommitsRead {
			t.Error("CommitsRead must stay false on error")
		}
		if v.PushAllowed || v.Owner != BranchOwnerUnknown {
			t.Errorf("must fail closed; got %+v", v)
		}
	})

	t.Run("MR-read failure is fatal", func(t *testing.T) {
		gl := &fakeOwnershipGitLab{mrErr: errors.New("404")}
		if _, _, err := FetchBranchOwnership(ctx, gl, "p", 1, AgentIdentity{}); err == nil {
			t.Fatal("expected an error when the MR cannot be read")
		}
	})

	t.Run("approvals-read failure leaves ownership governing", func(t *testing.T) {
		gl := &fakeOwnershipGitLab{
			mr:      &GitLabMR{Author: &GitLabUser{Username: "gardeniobot"}},
			commits: []BranchCommit{agentCommit("aaa1111")},
			aprErr:  errors.New("403"),
		}
		v, _, err := FetchBranchOwnership(ctx, gl, "p", 1, AgentIdentity{})
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if !v.PushAllowed {
			t.Errorf("agent-owned MR with unreadable approvals should still be push-allowed; got %s", v.Reason)
		}
	})

	// *GitLabClient must satisfy the interface the fetcher takes.
	var _ BranchOwnershipGitLab = (*GitLabClient)(nil)
}

func containsOwnershipSignal(haystack []string, needle string) bool {
	for _, h := range haystack {
		if strings.Contains(h, needle) {
			return true
		}
	}
	return false
}
