package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os/exec"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"
)

// ── which PR a merge escalation names (gd-3VaIxgZfDp) ──────────────────────
//
// gb escalation create used to fill operation_spec.mr_url from the requesting
// task's mr_url before the title, so a crew with two PRs on one task got the
// one it linked LAST: gd-05MVg33gqA filed "Merge PR #964" with mr_url #966 and
// had to withdraw and refile, because operation_spec was immutable. The fix:
//
//  1. Precedence: the spec or --mr-url, then the current branch's PR (gh pr
//     view), then a single PR URL in the title, then the task's mr_url, then
//     the context. The branch is what the filer is working on right now; the
//     task's mr_url is only whatever was linked most recently.
//  2. checkMergePRNamed refuses when the title (or, under a title that names
//     no PR, the context) names PRs and the resolved one is not among them, and when the title names several
//     PRs and nothing explicit picked one (gd-VeZIGcrSx0 kept #1401 of three).
//  3. gb escalation amend lets the filer correct the spec while the
//     escalation is open; kbeads allows exactly that (filerMayAmendMergeSpec).

// Sources resolveMergePRURL reports, for notes and the mismatch refusal.
const (
	mergePRFromSpec    = "operation_spec"
	mergePRFromBranch  = "the current branch's PR"
	mergePRFromTitle   = "the title"
	mergePRFromTask    = "--requesting-task's mr_url"
	mergePRFromContext = "--context"
)

// branchPRLookupTimeout bounds the gh call so a hung gh never stalls a create.
const branchPRLookupTimeout = 15 * time.Second

// escalationBranchPRURL returns the URL of the open PR for the current git
// branch, or "" when there is none or gh cannot say (no gh, no auth, not in a
// repo, detached HEAD). Quiet on failure: being on main is normal. A var so
// tests never reach the real gh.
var escalationBranchPRURL = func(ctx context.Context) string {
	ctx, cancel := context.WithTimeout(ctx, branchPRLookupTimeout)
	defer cancel()
	var out bytes.Buffer
	c := exec.CommandContext(ctx, "gh", "pr", "view", "--json", "url,state")
	c.Stdout = &out
	if c.Run() != nil {
		return ""
	}
	var pr struct {
		URL   string `json:"url"`
		State string `json:"state"`
	}
	if json.Unmarshal(out.Bytes(), &pr) != nil || !strings.EqualFold(pr.State, "OPEN") {
		return ""
	}
	return firstPRURL(pr.URL)
}

// resolveMergePRURL is the PR a new merge escalation is about, and where it
// came from. branchPR and taskMRURL are called only when every earlier source
// is empty, so a spec that names its PR costs no gh or daemon call.
func resolveMergePRURL(spec map[string]any, branchPR, taskMRURL func() string, title, context string) (string, string) {
	if u := specPRURL(spec); u != "" {
		return u, mergePRFromSpec
	}
	if u := firstPRURL(branchPR()); u != "" {
		return u, mergePRFromBranch
	}
	if urls := distinctPRURLs(title); len(urls) == 1 {
		return urls[0], mergePRFromTitle
	}
	if u := firstPRURL(taskMRURL()); u != "" {
		return u, mergePRFromTask
	}
	if u := firstPRURL(context); u != "" {
		return u, mergePRFromContext
	}
	return "", ""
}

// distinctPRURLs returns the PR URLs in s, one per PR, in order.
func distinctPRURLs(s string) []string {
	var out []string
	seen := map[string]bool{}
	for _, u := range prURLPattern.FindAllString(s, -1) {
		if k := prKey(u); !seen[k] {
			seen[k] = true
			out = append(out, u)
		}
	}
	return out
}

// prRefPattern matches a PR named by number: #964, !964 (GitLab), "PR 964",
// "MR 964". The leading class keeps "&#39;" and "issues#3" out.
var prRefPattern = regexp.MustCompile(`(?i)(?:(?:^|[^\w&/])[#!]|\b(?:PR|MR)\s+)(\d+)\b`)

// prNumbersIn returns the distinct PR numbers s names, as URLs or refs, in order.
func prNumbersIn(s string) []int {
	var out []int
	add := func(d string) {
		if n, err := strconv.Atoi(d); err == nil && !slices.Contains(out, n) {
			out = append(out, n)
		}
	}
	for _, m := range prURLPattern.FindAllStringSubmatch(s, -1) {
		add(m[3])
	}
	for _, m := range prRefPattern.FindAllStringSubmatch(s, -1) {
		add(m[1])
	}
	return out
}

// prNumber is the number of the PR prURL names, or 0.
func prNumber(prURL string) int {
	m := prURLPattern.FindStringSubmatch(prURL)
	if m == nil {
		return 0
	}
	n, _ := strconv.Atoi(m[3])
	return n
}

// checkMergePRNamed refuses a merge escalation whose resolved PR is not the
// one its title or context is about. An escalation that resolves no PR is
// left to the merge guard's own warning.
func checkMergePRNamed(prURL, source, title, context string) error {
	n := prNumber(prURL)
	if n == 0 {
		return nil
	}
	fix := "Pass --mr-url <the PR to merge> (it wins over every other source), or fix the title/--context"
	titleNums := prNumbersIn(title)
	if len(titleNums) > 0 && !slices.Contains(titleNums, n) {
		return fmt.Errorf("the title names %s, but operation_spec.mr_url would be %s (from %s). %s",
			joinPRNumbers(titleNums), prURL, source, fix)
	}
	if len(titleNums) > 1 && (source == mergePRFromTask || source == mergePRFromContext) {
		return fmt.Errorf("the title names %s; a merge escalation is for one PR and %s would pick %s. "+
			"File one escalation per PR, each with --mr-url", joinPRNumbers(titleNums), source, prURL)
	}
	// The context is read only when the title names no PR: a context that
	// says "after #962 merges" under a title naming the PR is fine.
	if ctxNums := prNumbersIn(context); len(titleNums) == 0 && len(ctxNums) > 0 && !slices.Contains(ctxNums, n) {
		return fmt.Errorf("--context names %s but not #%d, which operation_spec.mr_url would be (%s, from %s). %s",
			joinPRNumbers(ctxNums), n, prURL, source, fix)
	}
	return nil
}

func joinPRNumbers(ns []int) string {
	parts := make([]string, len(ns))
	for i, n := range ns {
		parts[i] = "#" + strconv.Itoa(n)
	}
	return "PR " + strings.Join(parts, ", ")
}

// setSpecMRURL points spec at prURL alone: it drops every other key that
// names the PR (mergeSpecURLKeys, and GitLab's project + iid) and sets mr_url.
// Other keys, like merge_after, are kept.
func setSpecMRURL(spec map[string]any, prURL string) {
	for _, k := range mergeSpecURLKeys {
		delete(spec, k)
	}
	if _, ok := parseMergeTarget(spec); ok {
		for _, k := range []string{"project", "mr_iid", "merge_request_iid", "iid"} {
			delete(spec, k)
		}
	}
	spec["mr_url"] = prURL
}
