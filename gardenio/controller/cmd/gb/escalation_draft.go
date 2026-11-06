package main

import (
	"context"
	"fmt"
	"io"
	"strings"
	"time"
)

// ── a merge escalation for a draft PR is refused (gd-9mAPy3HB8j) ───────────
//
// GitHub cannot merge a draft, so a merge escalation filed while the PR is
// still one asks the desk for something it cannot do. (Since gd-19ECZxvFQw a
// PR runs the light CI tier whether it is a draft or not; marking it ready
// starts no new run.) `gb pr ready` is the one way in: it flips the PR to
// ready and files the escalation itself. A hand-filed merge escalation for a
// draft is therefore refused, pointing at that command.
//
// The lookup fails open: if gh cannot say whether the PR is a draft (no gh,
// no auth, a network error), the escalation is created with a warning. Only a
// positive "isDraft: true" refuses.

// draftLookupTimeout bounds the gh call so a hung gh never stalls a create.
const draftLookupTimeout = 20 * time.Second

// checkMergeEscalationDraft returns an error when prURL is a GitHub PR that
// is still a draft. A lookup failure writes a warning to warn and returns
// nil. A URL that is not a GitHub PR (a GitLab MR, or none) is not checked.
func checkMergeEscalationDraft(ctx context.Context, prURL string, warn io.Writer) error {
	if prURL == "" || !strings.Contains(prURL, "/pull/") {
		return nil
	}
	ctx, cancel := context.WithTimeout(ctx, draftLookupTimeout)
	defer cancel()
	pr, err := viewPR(ctx, prURL)
	if err != nil {
		fmt.Fprintf(warn, "WARNING: could not check whether %s is a draft (%v); creating the escalation anyway\n", prURL, err)
		return nil
	}
	if pr.IsDraft {
		return fmt.Errorf("PR #%d is a draft; run 'gb pr ready' (it files this escalation for you)", pr.Number)
	}
	return nil
}
