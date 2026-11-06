package releaserecord

import (
	"bytes"
	"context"
	"fmt"
	"os/exec"
	"strconv"
	"strings"
)

// maxFirstReleaseCommits caps the history listed for a project's first
// release, which has no previous tag to stop at; its contents are then marked
// incomplete.
const maxFirstReleaseCommits = 250

// GitSource reads tags and history from a local checkout with the git CLI.
// The cut script runs it in the checkout it just tagged; the backfill in one
// with the tags fetched.
type GitSource struct {
	// Dir is the checkout; "" is the working directory.
	Dir string
}

var _ Source = GitSource{}

func (g GitSource) git(ctx context.Context, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, "git", args...)
	cmd.Dir = g.Dir
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	if err := cmd.Run(); err != nil {
		return "", fmt.Errorf("git %s: %w: %s", strings.Join(args, " "), err, strings.TrimSpace(stderr.String()))
	}
	return stdout.String(), nil
}

// tagFormat prints one tag per line: name, peeled commit (annotated tags),
// object (lightweight tags: the commit), creator date and tagger name.
const tagFormat = "%(refname:short)%09%(*objectname)%09%(objectname)%09%(creatordate:iso-strict)%09%(taggername)"

func parseTagLine(line string) (Tag, bool) {
	f := strings.Split(line, "\t")
	if len(f) < 5 || f[0] == "" {
		return Tag{}, false
	}
	t := Tag{Name: f[0], SHA: f[1], Date: f[3], Tagger: f[4]}
	if t.SHA == "" {
		t.SHA = f[2]
	}
	return t, true
}

// Tags lists every tag with its commit, date and tagger.
func (g GitSource) Tags(ctx context.Context) ([]Tag, error) {
	out, err := g.git(ctx, "for-each-ref", "--format="+tagFormat, "refs/tags")
	if err != nil {
		return nil, err
	}
	var tags []Tag
	for _, line := range strings.Split(out, "\n") {
		if t, ok := parseTagLine(line); ok {
			tags = append(tags, t)
		}
	}
	return tags, nil
}

// Tag reads one tag.
func (g GitSource) Tag(ctx context.Context, name string) (Tag, error) {
	out, err := g.git(ctx, "for-each-ref", "--format="+tagFormat, "refs/tags/"+name)
	if err != nil {
		return Tag{}, err
	}
	for _, line := range strings.Split(out, "\n") {
		if t, ok := parseTagLine(line); ok && t.Name == name {
			return t, nil
		}
	}
	return Tag{}, fmt.Errorf("tag %s not found in the checkout (git fetch --tags?)", name)
}

// Field and record separators for git log output: unit and record separator
// control characters, which never appear in a commit message.
const (
	gitFS = "\x1f"
	gitRS = "\x1e"
)

// gitLogFormat is the log format parseGitLog reads.
const gitLogFormat = "--format=%H" + gitFS + "%s" + gitFS + "%cI" + gitFS + "%b" + gitRS

// FirstParent lists base..head on the first-parent line, newest first.
func (g GitSource) FirstParent(ctx context.Context, base, head string) ([]Commit, bool, error) {
	args := []string{"log", "--first-parent", gitLogFormat}
	complete := true
	if base == "" {
		args = append(args, "--max-count="+strconv.Itoa(maxFirstReleaseCommits+1), head)
	} else {
		args = append(args, base+".."+head)
	}
	out, err := g.git(ctx, args...)
	if err != nil {
		return nil, false, err
	}
	commits := parseGitLog(out)
	if base == "" && len(commits) > maxFirstReleaseCommits {
		commits, complete = commits[:maxFirstReleaseCommits], false
	}
	return commits, complete, nil
}

// Commit reads one commit.
func (g GitSource) Commit(ctx context.Context, sha string) (Commit, error) {
	out, err := g.git(ctx, "log", "-1", gitLogFormat, sha)
	if err != nil {
		return Commit{}, err
	}
	if commits := parseGitLog(out); len(commits) == 1 {
		return commits[0], nil
	}
	return Commit{}, fmt.Errorf("git log %s: no commit", sha)
}

func parseGitLog(out string) []Commit {
	var commits []Commit
	for _, rec := range strings.Split(out, gitRS) {
		rec = strings.TrimLeft(rec, "\n")
		if rec == "" {
			continue
		}
		f := strings.SplitN(rec, gitFS, 4)
		if len(f) < 4 {
			continue
		}
		commits = append(commits, Commit{SHA: f[0], Subject: f[1], Date: f[2], Body: strings.TrimSpace(f[3])})
	}
	return commits
}
