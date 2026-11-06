// Package releaserecord writes a project's release record at cut time
// (gardenio/docs/releases-design.md §3-4, plan gd-UZKZ9H25UD step 3): for one
// tag it finds the previous release tag, lists the first-parent commits between
// them, turns each into a contents row joined to its merge-request bead, task,
// step and plan, and writes the type=release bead through the daemon's
// /v1/releases routes. The write is idempotent on (project, tag), so the cut
// script, a webhook and the backfill can all call it.
//
// It reads history through a Source (a local git checkout, or the GitHub API)
// and never shells out to the daemon: callers pass a Daemon.
package releaserecord

import (
	"context"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"unicode"
)

// Tag is one tag of the release repo.
type Tag struct {
	Name string
	// SHA is the commit the tag names (peeled for an annotated tag).
	SHA string
	// Date is the tagger date of an annotated tag, else the commit date,
	// RFC 3339. Empty when the source cannot tell cheaply.
	Date string
	// Tagger is the tagger name of an annotated tag.
	Tagger string
}

// Commit is one first-parent commit of the release branch.
type Commit struct {
	SHA     string
	Subject string
	Body    string
	// Date is the committer date, RFC 3339.
	Date string
}

// Source reads the release repo's tags and history.
type Source interface {
	// Tags lists the repo's tags. Date and Tagger may be empty.
	Tags(ctx context.Context) ([]Tag, error)
	// Tag reads one tag with its commit, date and tagger.
	Tag(ctx context.Context, name string) (Tag, error)
	// FirstParent lists the first-parent commits reachable from head and not
	// from base, newest first. An empty base lists from the start of history,
	// capped. complete is false when the list was truncated.
	FirstParent(ctx context.Context, base, head string) (commits []Commit, complete bool, err error)
}

// PR is a pull request found for a commit through the forge's API.
type PR struct {
	Number   int
	URL      string
	Title    string
	MergedAt string
	// HeadRef is the PR's branch, where an agent branch names its bead.
	HeadRef string
}

// PRLookup reads pull requests through the forge's API: the fallback for a
// commit whose subject names no #N, and for a PR with no MR bead (a PR a
// merge train carried, a PR from before github-bridge).
type PRLookup interface {
	PullForCommit(ctx context.Context, sha string) (PR, bool, error)
	Pull(ctx context.Context, number int) (PR, bool, error)
}

// branchBeadID is the bead id an agent branch (agent/<slug>/<id>) ends with,
// "" for any other branch.
func branchBeadID(branch string) string {
	if i := strings.LastIndex(branch, "/"); i >= 0 {
		if last := branch[i+1:]; branchBeadRe.MatchString(last) {
			return last
		}
	}
	return ""
}

var (
	// GitHub's merge-commit subject: "Merge pull request #N from owner/branch".
	mergeSubjectRe = regexp.MustCompile(`^Merge pull request #(\d+) from (\S+)`)
	// A squash or rebase subject ends with the PR number: "title (#N)".
	squashSubjectRe = regexp.MustCompile(`\(#(\d+)\)\s*$`)
	// A bead id as the last segment of an agent branch (agent/<slug>/<id>).
	branchBeadRe = regexp.MustCompile(`^[a-z]{2}-[A-Za-z0-9]{6,12}$`)
	// A bead id in prose: an 8-12 char suffix with a digit (checked in code),
	// so "re-evaluating" is never taken for one.
	proseBeadRe = regexp.MustCompile(`\b[a-z]{2}-[A-Za-z0-9]{8,12}\b`)
)

// parsedCommit is what a commit's message says about where it came from.
type parsedCommit struct {
	PR     int
	Title  string
	BeadID string
	// Train is set on a row of a merge train: the train's PR number.
	Train int
}

// trainTitleRe is the merge desk's train PR title:
// "merge train 2026-10-30T07:42Z: #1530 #1532 #1543".
var (
	trainTitleRe = regexp.MustCompile(`^merge train\b.*?:((?:\s*#\d+)+)\s*$`)
	prNumberRe   = regexp.MustCompile(`#(\d+)`)
)

// trainPRs returns the PRs a merge train carries, nil for any other commit.
func trainPRs(p parsedCommit) []int {
	m := trainTitleRe.FindStringSubmatch(p.Title)
	if m == nil {
		return nil
	}
	var out []int
	for _, n := range prNumberRe.FindAllStringSubmatch(m[1], -1) {
		if v, err := strconv.Atoi(n[1]); err == nil {
			out = append(out, v)
		}
	}
	return out
}

// parseCommit reads a first-parent commit's message: the PR number from a
// merge or squash subject, the PR title (a merge commit's body, a squash
// subject without its "(#N)"), and the bead id from the agent branch name or,
// failing that, the first bead id the subject or body mentions.
func parseCommit(c Commit) parsedCommit {
	var p parsedCommit
	subject := strings.TrimSpace(c.Subject)
	if m := mergeSubjectRe.FindStringSubmatch(subject); m != nil {
		p.PR, _ = strconv.Atoi(m[1])
		p.BeadID = branchBeadID(m[2])
		p.Title = firstLine(c.Body)
	} else if m := squashSubjectRe.FindStringSubmatchIndex(subject); m != nil {
		p.PR, _ = strconv.Atoi(subject[m[2]:m[3]])
		p.Title = strings.TrimSpace(subject[:m[0]])
	}
	if p.Title == "" {
		p.Title = subject
	}
	if p.BeadID == "" {
		p.BeadID = firstProseBeadID(subject + "\n" + c.Body)
	}
	return p
}

func firstLine(s string) string {
	for _, line := range strings.Split(s, "\n") {
		if line = strings.TrimSpace(line); line != "" {
			return line
		}
	}
	return ""
}

func firstProseBeadID(s string) string {
	for _, m := range proseBeadRe.FindAllString(s, -1) {
		if strings.ContainsAny(m, "0123456789") {
			return m
		}
	}
	return ""
}

// releaseTags returns the tags whose names match pattern, oldest version
// first (versionLess).
func releaseTags(tags []Tag, pattern *regexp.Regexp) []Tag {
	var out []Tag
	for _, t := range tags {
		if pattern.MatchString(t.Name) {
			out = append(out, t)
		}
	}
	sort.SliceStable(out, func(i, j int) bool { return versionLess(out[i].Name, out[j].Name) })
	return out
}

// previousTag is the greatest release tag below tag in version order, "" for
// the first release.
func previousTag(tags []Tag, tag string) string {
	prev := ""
	for _, t := range tags {
		if t.Name != tag && versionLess(t.Name, tag) {
			prev = t.Name
		}
	}
	return prev
}

// versionLess orders tag names by their digit runs as numbers and the rest as
// text, so 2026.303.10 sorts after 2026.303.9 and v1.10.0 after v1.9.2.
func versionLess(a, b string) bool {
	ca, cb := versionChunks(a), versionChunks(b)
	for i := 0; i < len(ca) && i < len(cb); i++ {
		x, y := ca[i], cb[i]
		xn, xerr := strconv.Atoi(x)
		yn, yerr := strconv.Atoi(y)
		switch {
		case xerr == nil && yerr == nil:
			if xn != yn {
				return xn < yn
			}
		case x != y:
			return x < y
		}
	}
	return len(ca) < len(cb)
}

// versionChunks splits s into alternating runs of digits and non-digits.
func versionChunks(s string) []string {
	var out []string
	start := 0
	for i := 1; i <= len(s); i++ {
		if i == len(s) || unicode.IsDigit(rune(s[i])) != unicode.IsDigit(rune(s[i-1])) {
			out = append(out, s[start:i])
			start = i
		}
	}
	return out
}

// RepoWebURL turns a project's git_url into the repo's web URL
// (https://github.com/owner/repo): an scp-style or ssh URL becomes https, and
// ".git" and a trailing slash are dropped. "" when gitURL is empty.
func RepoWebURL(gitURL string) string {
	u := strings.TrimSpace(gitURL)
	if u == "" {
		return ""
	}
	if strings.HasPrefix(u, "git@") {
		u = "https://" + strings.Replace(strings.TrimPrefix(u, "git@"), ":", "/", 1)
	}
	u = strings.Replace(u, "ssh://git@", "https://", 1)
	u = strings.TrimSuffix(strings.TrimSuffix(u, "/"), ".git")
	return u
}

// RepoFullName is the owner/repo part of a GitHub web URL, "" when it has none.
func RepoFullName(webURL string) string {
	rest := webURL
	if i := strings.Index(rest, "://"); i >= 0 {
		rest = rest[i+3:]
	}
	parts := strings.Split(rest, "/")
	if len(parts) < 3 || parts[1] == "" || parts[2] == "" {
		return ""
	}
	return parts[1] + "/" + parts[2]
}
