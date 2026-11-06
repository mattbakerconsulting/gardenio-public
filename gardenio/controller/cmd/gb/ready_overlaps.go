package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"regexp"
	"slices"
	"strings"
	"time"

	"gardenio/controller/internal/beadsapi"
)

// `gb ready --plan <id> --hold-overlaps` is the plan gardener's dispatch hold
// (Task 4.2, gd-h7awQXGs8Z). A ready step whose description names a file (a
// path in backticks) that an open PR of another step in the same plan already
// changes is HELD: dispatching it now would race that PR for the same lines.
// The hold is advisory, not a lock: once it is older than readyHoldMax the row
// says so and the gardener dispatches it anyway, commenting why. A step that
// names no files is never held, and every failure to read a PR fails open (no
// hold), so the flag can only ever delay a dispatch, never lose one.

// readyHoldMax is how long a hold lasts before the step dispatches anyway.
const readyHoldMax = 12 * time.Hour

// readyHoldNow is the clock the hold age is read against. A var so tests pin it.
var readyHoldNow = time.Now

// overlapHold is a held row's reason, carried in `gb ready --json` as
// overlap_hold. Since is the later of the step's creation and the newest
// overlapping PR's opening: the step could not have been held before both
// existed. Expired means the hold is older than readyHoldMax.
type overlapHold struct {
	PRs     []overlapHoldPR `json:"prs"`
	Since   time.Time       `json:"since"`
	Expired bool            `json:"expired"`
}

// overlapHoldPR is one open PR a held step waits on, with the step's named
// files that PR changes.
type overlapHoldPR struct {
	PR    int      `json:"pr"`
	URL   string   `json:"url"`
	Task  string   `json:"task"`
	Files []string `json:"files"`
}

// readyRow is a `gb ready` JSON row: the bead, plus overlap_hold when
// --hold-overlaps holds it. Embedding keeps every bead key at the top level,
// so the output stays the same bare array scripts already read.
type readyRow struct {
	*beadsapi.BeadDetail
	OverlapHold *overlapHold `json:"overlap_hold,omitempty"`
}

// planOpenPR is an open PR whose task bead is in the plan.
type planOpenPR struct {
	Number   int
	URL      string
	Task     string
	OpenedAt time.Time
	Files    []string
}

// stepPathRE matches a backtick span. stepPaths keeps the spans that look like
// a repo path.
var stepPathRE = regexp.MustCompile("`([^`\n]+)`")

// stepPathExtRE is a file name ending in a lowercase extension (page.go,
// garden.css). Go selectors such as `opts.Plan` fail it on the capital.
var stepPathExtRE = regexp.MustCompile(`[A-Za-z0-9_-]\.[a-z][a-z0-9]{0,5}$`)

// stepPaths returns the distinct repo paths a step's description names in
// backticks, in order. A span is a path when it has no spaces or shell or
// template punctuation and either contains a slash or ends in a file
// extension; commands (`kd show x`), field names (`mr_overlaps`) and bead ids
// are not.
func stepPaths(description string) []string {
	var out []string
	for _, m := range stepPathRE.FindAllStringSubmatch(description, -1) {
		p := strings.TrimPrefix(strings.TrimSpace(m[1]), "./")
		if p == "" || strings.ContainsAny(p, " \t<>{}$*=:@,;()'\"|&?!#~[]") || strings.HasPrefix(p, "-") {
			continue
		}
		if !strings.Contains(p, "/") && !stepPathExtRE.MatchString(p) {
			continue
		}
		if !slices.Contains(out, p) {
			out = append(out, p)
		}
	}
	return out
}

// pathTouches reports whether a PR's changed file is the step's named path:
// the same path, a bare name or path suffix of it (`page.go` names
// gardenio/controller/cmd/gardenhub/page.go), or a directory above it.
func pathTouches(prFile, stepPath string) bool {
	dir := strings.TrimSuffix(stepPath, "/")
	return prFile == stepPath ||
		strings.HasSuffix(prFile, "/"+dir) ||
		strings.HasPrefix(prFile, dir+"/") ||
		strings.Contains(prFile, "/"+dir+"/")
}

// overlapHolds returns, per ready step id, why it is held. A step is held by
// each open PR, other than its own, whose changed files include one of the
// step's named paths.
func overlapHolds(steps []*beadsapi.BeadDetail, prs []planOpenPR, now time.Time) map[string]*overlapHold {
	holds := map[string]*overlapHold{}
	for _, s := range steps {
		paths := stepPaths(s.Description)
		if len(paths) == 0 {
			continue
		}
		var h overlapHold
		for _, pr := range prs {
			if pr.Task == s.ID {
				continue
			}
			var hit []string
			for _, p := range paths {
				if slices.ContainsFunc(pr.Files, func(f string) bool { return pathTouches(f, p) }) {
					hit = append(hit, p)
				}
			}
			if len(hit) == 0 {
				continue
			}
			h.PRs = append(h.PRs, overlapHoldPR{PR: pr.Number, URL: pr.URL, Task: pr.Task, Files: hit})
			if pr.OpenedAt.After(h.Since) {
				h.Since = pr.OpenedAt
			}
		}
		if len(h.PRs) == 0 {
			continue
		}
		if s.CreatedAt.After(h.Since) {
			h.Since = s.CreatedAt
		}
		h.Expired = now.Sub(h.Since) > readyHoldMax
		holds[s.ID] = &h
	}
	return holds
}

// planOpenPRs lists the open PRs of the plan's not-closed beads that carry an
// mr_url. A task stays open after its PR merges while it awaits a release, so
// each PR's state is read from GitHub; a PR that cannot be read is skipped and
// named on stderr.
func planOpenPRs(ctx context.Context, plan string) ([]planOpenPR, error) {
	res, err := daemon.ListBeadsFiltered(ctx, beadsapi.ListBeadsQuery{
		Parent:   plan,
		Statuses: []string{"open", "in_progress", "blocked", "deferred"},
		Limit:    readyJSONFullLimit,
	})
	if err != nil {
		return nil, fmt.Errorf("listing plan %s beads: %w", plan, err)
	}
	var prs []planOpenPR
	for _, b := range res.Beads {
		url := strings.TrimSpace(b.Fields["mr_url"])
		if !strings.Contains(url, "/pull/") {
			continue
		}
		out, err := prGHRun(ctx, "pr", "view", url, "--json", "number,state,createdAt,files")
		if err != nil {
			fmt.Fprintf(os.Stderr, "note: --hold-overlaps skips %s (%s): %v\n", url, b.ID, err)
			continue
		}
		var v struct {
			Number    int       `json:"number"`
			State     string    `json:"state"`
			CreatedAt time.Time `json:"createdAt"`
			Files     []struct {
				Path string `json:"path"`
			} `json:"files"`
		}
		if err := json.Unmarshal([]byte(out), &v); err != nil {
			fmt.Fprintf(os.Stderr, "note: --hold-overlaps skips %s (%s): parsing gh pr view: %v\n", url, b.ID, err)
			continue
		}
		if v.State != "OPEN" {
			continue
		}
		pr := planOpenPR{Number: v.Number, URL: url, Task: b.ID, OpenedAt: v.CreatedAt}
		for _, f := range v.Files {
			pr.Files = append(pr.Files, f.Path)
		}
		prs = append(prs, pr)
	}
	return prs, nil
}

// readyHoldNote is the human line under a held row: what it waits on, and
// what the gardener does with it.
func readyHoldNote(h *overlapHold, now time.Time) string {
	var on []string
	for _, pr := range h.PRs {
		on = append(on, fmt.Sprintf("#%d (%s) on %s", pr.PR, pr.Task, strings.Join(pr.Files, ", ")))
	}
	age := now.Sub(h.Since).Round(time.Minute)
	if h.Expired {
		return fmt.Sprintf("⏱ HOLD EXPIRED after %s (max %s): overlaps open PR %s. Dispatch it anyway and comment why on the step.",
			age, readyHoldMax, strings.Join(on, "; "))
	}
	return fmt.Sprintf("⏸ HELD %s: overlaps open PR %s. Dispatch it after that PR merges.", age, strings.Join(on, "; "))
}

// readyRows pairs each bead with its hold, for --json.
func readyRows(beads []*beadsapi.BeadDetail, holds map[string]*overlapHold) []readyRow {
	rows := make([]readyRow, 0, len(beads))
	for _, b := range beads {
		rows = append(rows, readyRow{BeadDetail: b, OverlapHold: holds[b.ID]})
	}
	return rows
}
