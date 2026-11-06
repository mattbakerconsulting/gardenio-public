package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"regexp"
	"strings"
	"time"
)

// ── --close-on-execute never names an epic (gd-0SbA6Iyclm) ───────────────
//
// --requesting-task defaults to the agent's task_id, and a plan gardener's
// task_id is its plan epic. A gardener that filed the merge escalation for one
// step's PR therefore parked the whole plan behind it with --close-on-execute,
// and the merge closed the plan with 7 steps still open. When the requesting
// task is an epic, the step the PR delivers is found from the PR itself (its
// head branch's /<bead-id> suffix, or the agent its Bead: trailer names); with
// no such step the create is refused. The daemon also never closes an epic on
// an escalation executing (closeEscalationWrapper), so this is the cause, not
// the only guard.

// epicStepLookupTimeout bounds the gh call so a hung gh never stalls a create.
const epicStepLookupTimeout = 20 * time.Second

// branchBeadIDPattern matches a bead id as the last segment of a branch name
// (agent/<slug>/gd-ymB1Pgg0fQ, as gb pr open's branches are named).
var branchBeadIDPattern = regexp.MustCompile(`^[a-z]{2,}-[0-9A-Za-z]{6,}$`)

// beadTrailerPattern matches the Bead: trailer gb pr open writes.
var beadTrailerPattern = regexp.MustCompile(`(?m)^Bead:\s*(\S+)\s*$`)

// closeOnExecuteTarget returns the bead a --close-on-execute escalation should
// close. requestingTask is returned unchanged unless it is an epic; then the
// step prURL delivers under that epic is returned, or an error when there is
// none. A failed lookup of requestingTask itself fails open (the daemon still
// refuses to close an epic).
func closeOnExecuteTarget(ctx context.Context, requestingTask, prURL string, warn io.Writer) (string, error) {
	if daemon == nil || requestingTask == "" {
		return requestingTask, nil
	}
	bead, err := daemon.GetBead(ctx, requestingTask)
	if err != nil || bead == nil || bead.Type != "epic" {
		return requestingTask, nil
	}
	if step := epicStepForPR(ctx, requestingTask, prURL); step != "" {
		fmt.Fprintf(warn, "note: --requesting-task %s is an epic; using step %s, which %s delivers\n", requestingTask, step, prURL)
		return step, nil
	}
	return "", fmt.Errorf("--requesting-task %s is an epic, and --close-on-execute would close the whole epic when this escalation executes. "+
		"Pass --requesting-task <step-id> naming the step this PR delivers (gd-0SbA6Iyclm)", requestingTask)
}

// epicStepForPR returns the non-epic bead under epicID that the GitHub PR at
// prURL delivers, or "" when none can be found.
func epicStepForPR(ctx context.Context, epicID, prURL string) string {
	if !strings.Contains(prURL, "/pull/") {
		return ""
	}
	lctx, cancel := context.WithTimeout(ctx, epicStepLookupTimeout)
	defer cancel()
	out, err := prGHRun(lctx, "pr", "view", prURL, "--json", "headRefName,body")
	if err != nil {
		return ""
	}
	var pr struct {
		HeadRefName string `json:"headRefName"`
		Body        string `json:"body"`
	}
	if json.Unmarshal([]byte(out), &pr) != nil {
		return ""
	}
	var candidates []string
	if i := strings.LastIndex(pr.HeadRefName, "/"); i >= 0 && branchBeadIDPattern.MatchString(pr.HeadRefName[i+1:]) {
		candidates = append(candidates, pr.HeadRefName[i+1:])
	}
	if m := beadTrailerPattern.FindStringSubmatch(pr.Body); m != nil {
		// The trailer names the agent that opened the PR; its task is the step.
		if b, err := daemon.GetBead(ctx, m[1]); err == nil && b != nil {
			if b.Type == "agent" {
				candidates = append(candidates, b.Fields["task_id"])
			} else {
				candidates = append(candidates, b.ID)
			}
		}
	}
	for _, id := range candidates {
		if isStepUnderEpic(ctx, id, epicID) {
			return id
		}
	}
	return ""
}

// isStepUnderEpic reports whether id is a closable work bead (not an epic,
// escalation or decision) whose parent-child chain reaches epicID.
func isStepUnderEpic(ctx context.Context, id, epicID string) bool {
	if id == "" || id == epicID {
		return false
	}
	b, err := daemon.GetBead(ctx, id)
	if err != nil || b == nil {
		return false
	}
	switch b.Type {
	case "epic", "escalation", "decision", "agent":
		return false
	}
	const maxDepth = 4
	cur := id
	for range maxDepth {
		deps, err := daemon.GetDependencies(ctx, cur)
		if err != nil {
			return false
		}
		next := ""
		for _, d := range deps {
			if d.Type == "parent-child" && d.DependsOnID != "" {
				next = d.DependsOnID
				break
			}
		}
		if next == epicID {
			return true
		}
		if next == "" {
			return false
		}
		cur = next
	}
	return false
}
