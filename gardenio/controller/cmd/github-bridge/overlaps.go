package main

// The open-PR overlap pass (task gd-Lnfac5eRIH): after a full scan, which
// open PRs, each clean against main, conflict with each other, written as
// mr_overlaps on both PRs' beads (docs/pr-conflicts.md).
//
// Only pairs whose changed-file sets intersect are merged: PRs that touch
// disjoint files cannot conflict, so the pass stays near-linear in the number
// of open PRs. Mechanical paths are left out of both the intersection and the
// result; they resolve themselves (Task 3.2) or never enter a PR (Task 1.1).

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"strings"
)

const (
	fieldOverlaps = "mr_overlaps"
	// maxOverlapFiles caps the files of one mr_overlaps entry; the last one
	// then says how many were left out.
	maxOverlapFiles = 10
)

// prOverlap is one mr_overlaps entry: another open PR whose head conflicts
// with this one, and the conflicting files.
type prOverlap struct {
	PR    int      `json:"pr"`
	Files []string `json:"files"`
}

// prHead is an open PR clean against main, at the head the scan merged.
type prHead struct {
	n    int
	head string
}

// changedFiles is the paths head changed since its merge base with base.
// Renames count as both paths, and need no blobs that way.
func (m *gitMirror) changedFiles(ctx context.Context, base, head string) ([]string, error) {
	out, rc, err := m.run(ctx, gitCommandTimeout, "diff", "--no-renames", "--name-only", "-z", base+"..."+head)
	if err != nil {
		return nil, err
	}
	if rc != 0 {
		return nil, fmt.Errorf("git diff: rc %d", rc)
	}
	var files []string
	for _, p := range strings.Split(string(out), "\x00") {
		if p != "" {
			files = append(files, p)
		}
	}
	return files, nil
}

// handPaths is files without the mechanical paths.
func handPaths(files []string) []string {
	var out []string
	for _, f := range files {
		if !isMechanicalPath(f) {
			out = append(out, f)
		}
	}
	return out
}

// overlapsJSON is the mr_overlaps value: entries sorted by PR number, each
// with at most maxOverlapFiles files. "" when there are none.
func overlapsJSON(ov []prOverlap) string {
	if len(ov) == 0 {
		return ""
	}
	ov = slices.Clone(ov)
	slices.SortFunc(ov, func(a, b prOverlap) int { return a.PR - b.PR })
	for i := range ov {
		files := slices.Sorted(slices.Values(ov[i].Files))
		if len(files) > maxOverlapFiles {
			keep := maxOverlapFiles - 1
			files = append(files[:keep:keep], fmt.Sprintf("…+%d more", len(files)-keep))
		}
		ov[i].Files = files
	}
	raw, _ := json.Marshal(ov)
	return string(raw)
}

// overlaps merges every pair of heads whose changed files intersect and
// returns each PR's mr_overlaps value. A PR is missing from the result when
// its files or one of its merges could not be computed: its beads then keep
// what they have.
func (s *conflictScanner) overlaps(ctx context.Context, base string, heads []prHead) map[int]string {
	heads = slices.Clone(heads)
	slices.SortFunc(heads, func(a, b prHead) int { return a.n - b.n })
	files := map[int][]string{}
	known := map[int]bool{}
	var ok []prHead
	for _, h := range heads {
		fs, err := s.mirror.changedFiles(ctx, base, h.head)
		if err != nil {
			s.metrics.errors.Inc()
			s.logger.Warn("overlap scan: changed files unknown", "repo", s.repo, "pr", h.n, "error", err)
			continue
		}
		files[h.n] = handPaths(fs)
		known[h.n] = true
		ok = append(ok, h)
	}
	found := map[int][]prOverlap{}
	merges, pairs := 0, 0
	for i, a := range ok {
		for _, b := range ok[i+1:] {
			if !slices.ContainsFunc(files[a.n], func(f string) bool { return slices.Contains(files[b.n], f) }) {
				continue
			}
			merges++
			state, conflicted, err := s.mirror.mergeTree(ctx, a.head, b.head)
			if err != nil {
				s.metrics.errors.Inc()
				s.logger.Warn("overlap scan: merge unknown", "repo", s.repo, "pr", a.n, "other", b.n, "error", err)
				known[a.n], known[b.n] = false, false
				continue
			}
			conflicted = handPaths(conflicted)
			if state != mergeConflicting || len(conflicted) == 0 {
				continue
			}
			pairs++
			found[a.n] = append(found[a.n], prOverlap{PR: b.n, Files: conflicted})
			found[b.n] = append(found[b.n], prOverlap{PR: a.n, Files: conflicted})
		}
	}
	s.metrics.overlapPairs.Set(float64(pairs))
	s.logger.Info("overlap scan done", "repo", s.repo, "prs", len(ok), "merges", merges, "pairs", pairs)
	out := map[int]string{}
	for n, k := range known {
		if k {
			out[n] = overlapsJSON(found[n])
		}
	}
	return out
}

// clearOverlaps empties mr_overlaps on the beads of a PR that is no longer
// open.
func (s *conflictScanner) clearOverlaps(ctx context.Context, n int, url string) {
	beads, err := s.beadsFor(ctx, url)
	if err != nil {
		s.metrics.errors.Inc()
		s.logger.Warn("overlap scan: bead lookup failed", "pr", n, "error", err)
		return
	}
	for _, b := range beads {
		if b.Fields[fieldOverlaps] == "" {
			continue
		}
		if err := s.daemon.UpdateBeadFields(ctx, b.ID, map[string]string{fieldOverlaps: ""}); err != nil {
			s.metrics.errors.Inc()
			s.logger.Warn("overlap scan: bead write failed", "pr", n, "bead", b.ID, "error", err)
		}
	}
}
