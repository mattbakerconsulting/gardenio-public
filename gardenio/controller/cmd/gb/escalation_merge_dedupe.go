package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"regexp"
	"strings"

	"gardenio/controller/internal/beadsapi"
)

// ── merge escalations are idempotent per PR (gd-f7GvZ7qahY) ────────────────
//
// Crews filed every PR-merge escalation twice, 20-30s apart, from the same
// agent: six pairs on 2026-10-19 between 02:26 and 03:41Z (PRs #550, #551,
// #554, #556, #560). In every pair the FIRST bead had an empty (or off-key)
// operation_spec and the SECOND carried operation_spec.mr_url. The filer said
// why on gd-o4qHbD4Q8P: "Superseded by gd-zTyBlvV4At (same merge, with
// operation_spec.mr_url; spec is immutable after create)". The first create
// printed the merge guard's "names no MR … Add the MR to --operation-spec
// (e.g. {"mr_url":"…"})" warning, and the agent did as told by filing again.
//
// Two fixes, both here at the gb layer so no prompt can bring it back:
//
//  1. fillMergeSpecURL copies the PR URL into operation_spec.mr_url when the
//     spec names none but the title, --context or the requesting task's
//     mr_url does. The first create is then already complete, and the
//     warning that prompted the refile does not fire.
//  2. findOpenMergeEscalation keys every open merge escalation by the PR it
//     names. A create for a PR that already has one prints the existing id
//     and creates nothing, so a second create is a no-op, not a second
//     reviewer pass.

// mergeOperationTypes are the operation types that ask for one PR to merge.
// Agents file GitHub PR merges under gitlab_mr_merge (the name predates the
// GitHub move), so both spellings share one key space.
var mergeOperationTypes = map[string]bool{
	"gitlab_mr_merge": true,
	"github_pr_merge": true,
}

// prURLPattern matches a GitHub pull request or a GitLab merge request URL
// anywhere in a string. Groups: host, project path, number.
var prURLPattern = regexp.MustCompile(`https?://([^/\s]+)/([^\s]+?)/(?:-/merge_requests|pull)/(\d+)`)

// mergeSpecURLKeys are the operation_spec keys that name the PR itself, in
// preference order. The first three are the ones parseMergeTarget reads; the
// rest are spellings agents used for GitHub PRs (gd-2NcHetEbul wrote "pr").
// Deliberately an allowlist: other keys, like "merge_after", name a
// different PR and must not become the dedupe key.
var mergeSpecURLKeys = []string{"mr_url", "url", "merge_request_url", "pr_url", "pr", "pull_request_url"}

// prKey canonicalises a PR/MR URL to "host/project#number" (lower-cased), or
// "" when s holds none. Only the first URL counts.
func prKey(s string) string {
	m := prURLPattern.FindStringSubmatch(s)
	if m == nil {
		return ""
	}
	return strings.ToLower(m[1] + "/" + strings.TrimSuffix(m[2], "/") + "#" + m[3])
}

// firstPRURL returns the first PR/MR URL in s, or "".
func firstPRURL(s string) string {
	loc := prURLPattern.FindStringIndex(s)
	if loc == nil {
		return ""
	}
	return s[loc[0]:loc[1]]
}

// specPRURL returns the PR URL an operation_spec names under one of
// mergeSpecURLKeys, or "".
func specPRURL(spec map[string]any) string {
	for _, k := range mergeSpecURLKeys {
		if v, _ := spec[k].(string); firstPRURL(v) != "" {
			return firstPRURL(v)
		}
	}
	return ""
}

// mergeEscalationPRURL is the PR a merge escalation is about: the spec first,
// then the title, then the context. extra (the requesting task's mr_url)
// slots in after the spec, since it is recorded by `gb mr link` and is more
// specific than prose.
func mergeEscalationPRURL(spec map[string]any, extra, title, context string) string {
	if u := specPRURL(spec); u != "" {
		return u
	}
	for _, s := range []string{extra, title, context} {
		if u := firstPRURL(s); u != "" {
			return u
		}
	}
	return ""
}

// fillMergeSpecURL sets spec["mr_url"] to prURL when the spec names no PR
// under any recognised key. Returns true when it changed the spec. A spec
// that names its PR already is left untouched, even if prURL differs.
func fillMergeSpecURL(spec map[string]any, prURL string) bool {
	if spec == nil || prURL == "" || specPRURL(spec) != "" {
		return false
	}
	// project + iid is the GitLab guard's other form; it already names the MR.
	if _, ok := parseMergeTarget(spec); ok {
		return false
	}
	spec["mr_url"] = prURL
	return true
}

// escalationPRKey reads the PR key off a stored escalation bead.
func escalationPRKey(b *beadsapi.BeadDetail) string {
	if b == nil {
		return ""
	}
	var spec map[string]any
	if raw := b.Fields["operation_spec"]; raw != "" {
		_ = json.Unmarshal([]byte(raw), &spec)
	}
	return prKey(mergeEscalationPRURL(spec, "", b.Title, b.Fields["context"]))
}

// mergeEscalationLister is the slice of the daemon client findOpenMergeEscalation needs.
type mergeEscalationLister interface {
	ListBeadsFiltered(ctx context.Context, q beadsapi.ListBeadsQuery) (*beadsapi.ListBeadsResult, error)
}

// findOpenMergeEscalation returns the open merge escalation for key, or nil.
// A list error returns nil: failing to dedupe must never block a create.
func findOpenMergeEscalation(ctx context.Context, d mergeEscalationLister, key string) *beadsapi.BeadDetail {
	if d == nil || key == "" {
		return nil
	}
	res, err := d.ListBeadsFiltered(ctx, beadsapi.ListBeadsQuery{
		Types:    []string{"escalation"},
		Statuses: []string{"open", "in_progress"},
		Limit:    500,
	})
	if err != nil || res == nil {
		return nil
	}
	for _, b := range res.Beads {
		if b == nil || !mergeOperationTypes[b.Fields["operation_type"]] {
			continue
		}
		if escalationPRKey(b) == key {
			return b
		}
	}
	return nil
}

// reportExistingMergeEscalation prints the existing escalation's id in the
// same escalation_id= form a create prints, so a caller that parses stdout
// sees one id for the PR either way. It also says what to do on a new head:
// crews told to "file a fresh merge escalation for the new head" read the
// refusal as a missing re-pin verb (session sweep gd-KBdgGYAZOP), but the
// escalation names the PR, not a commit, and the desk merges the PR's head as
// it stands when it acts.
func reportExistingMergeEscalation(stdout, stderr io.Writer, b *beadsapi.BeadDetail, key string) {
	fmt.Fprintf(stderr, "note: merge escalation %s (%s) is already open for %s — not filing a second one.\n"+
		"It names the PR, not a commit: the merge desk merges the PR's current head, so a new push needs no new escalation.\n"+
		"If its context names an older head, say so on it: kd comment add %s \"Head is now <sha>; CI <state>.\"\n"+
		"Nothing more to do: gb stop, and the operator's action on %s wakes you.\n", b.ID, b.Status, key, b.ID, b.ID)
	fmt.Fprintf(stdout, "escalation_id=%s\n", b.ID)
}
