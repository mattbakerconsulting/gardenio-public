package releaserecord

import (
	"context"
	"errors"
	"fmt"

	"gardenio/controller/internal/beadsapi"
)

// The merge stamp (plan gd-lgN4U7X5Tb, step gd-6LuAI5uRLC): a PR merged on a
// project's release branch boards the project's open release as it lands, its
// rows joined by commitRows, the join the cut's reconcile runs, so the cut
// changes no row a merge boarded.

// CommitReader reads one commit of the release repo; GitSource and GitHub
// satisfy it.
type CommitReader interface {
	Commit(ctx context.Context, sha string) (Commit, error)
}

var (
	_ CommitReader = GitSource{}
	_ CommitReader = GitHub{}
)

// Appender appends rows to an open release's contents (POST
// /v1/releases/{id}/contents); *beadsapi.Client satisfies it.
type Appender interface {
	AppendReleaseContents(ctx context.Context, ref string, rows any) (*beadsapi.Release, error)
}

var _ Appender = (*beadsapi.Client)(nil)

// Boarded is what Board did: the open release the rows boarded, nil when the
// project has none.
type Boarded struct {
	Release *beadsapi.Release
	Rows    []Row
}

// Board appends the rows of the merge commit sha, on project's release branch,
// to the project's open release through a. A project with no open release
// boards nothing (Boarded.Release is nil): the recorder opens one at the next
// cut, and that cut's git range carries the PR. The daemon keeps a row once by
// (merge_sha, pr), so a redelivered merge boards once. A release cut under the
// append (409) is re-read once: the cut opened the next one, which the merge
// boards.
func (r *Recorder) Board(ctx context.Context, a Appender, project, sha string) (*Boarded, error) {
	if err := r.Load(ctx, project); err != nil {
		return nil, err
	}
	cr, ok := r.Source.(CommitReader)
	if !ok {
		return nil, errors.New("the release source cannot read one commit")
	}
	open, err := FindOpen(ctx, r.Daemon, project)
	if err != nil || open == nil {
		return &Boarded{}, err
	}
	c, err := cr.Commit(ctx, sha)
	if err != nil {
		return nil, fmt.Errorf("reading commit %s: %w", sha, err)
	}
	rows, err := r.commitRows(ctx, c)
	if err != nil {
		return nil, err
	}
	for attempt := 0; ; attempt++ {
		rel, err := a.AppendReleaseContents(ctx, open.ID, rows)
		if err == nil {
			return &Boarded{Release: rel, Rows: rows}, nil
		}
		if attempt > 0 || !isConflict(err) {
			return nil, fmt.Errorf("boarding %s on release %s: %w", sha, open.ID, err)
		}
		next, ferr := FindOpen(ctx, r.Daemon, project)
		if ferr != nil || next == nil || next.ID == open.ID {
			return nil, fmt.Errorf("boarding %s on release %s: %w", sha, open.ID, err)
		}
		open = next
	}
}
