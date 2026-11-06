package releaserecord

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"

	"gardenio/controller/internal/beadsapi"
)

// The open release (plan gd-lgN4U7X5Tb, step gd-Pa4afIz6fR): a project's next
// release exists before its cut, as a release bead with no tag that merged PRs
// board. The recorder cuts a new tag onto it instead of minting a bead, and
// then opens the following one, so a release-enabled project always has one.

// openReleasesRead is how many of a project's newest releases are read to find
// its open release (listed first) and the newest recorded tag.
const openReleasesRead = 20

// Opener is what opening a release asks of the daemon; *beadsapi.Client
// satisfies it.
type Opener interface {
	ListReleases(ctx context.Context, project, status string, limit int) ([]beadsapi.Release, error)
	CreateRelease(ctx context.Context, req beadsapi.CreateReleaseRequest) (*beadsapi.Release, error)
}

var _ Opener = (*beadsapi.Client)(nil)

// FindOpen is project's open release, nil when it has none.
func FindOpen(ctx context.Context, d Opener, project string) (*beadsapi.Release, error) {
	rels, err := d.ListReleases(ctx, project, beadsapi.ReleaseStatusOpen, 1)
	if err != nil {
		return nil, fmt.Errorf("finding %s's open release: %w", project, err)
	}
	for i := range rels {
		if rels[i].Tag == "" {
			return &rels[i], nil
		}
	}
	return nil, nil
}

// OpenNext opens project's next release unless it has one: a single create on
// the daemon's next key, which refuses a second open release with a 409, so
// two writers racing converge on one. created is true when this call opened
// it.
func OpenNext(ctx context.Context, d Opener, project string) (rel *beadsapi.Release, created bool, err error) {
	if rel, err = FindOpen(ctx, d, project); err != nil || rel != nil {
		return rel, false, err
	}
	rel, err = d.CreateRelease(ctx, beadsapi.CreateReleaseRequest{Project: project})
	if err == nil {
		return rel, true, nil
	}
	if !isConflict(err) {
		return nil, false, fmt.Errorf("opening %s's next release: %w", project, err)
	}
	if rel, ferr := FindOpen(ctx, d, project); ferr == nil && rel != nil {
		return rel, false, nil
	}
	return nil, false, fmt.Errorf("opening %s's next release: %w", project, err)
}

func isConflict(err error) bool {
	var apiErr *beadsapi.APIError
	return errors.As(err, &apiErr) && apiErr.StatusCode == http.StatusConflict
}

// ParseRows reads a release's contents. A row that is not a contents row
// (an append the daemon accepted with a string pr, say) is skipped and
// counted in bad.
func ParseRows(raw json.RawMessage) (rows []Row, bad int) {
	var items []json.RawMessage
	if json.Unmarshal(raw, &items) != nil {
		return nil, 0
	}
	for _, item := range items {
		var row Row
		if json.Unmarshal(item, &row) != nil {
			bad++
			continue
		}
		rows = append(rows, row)
	}
	return rows, bad
}

// planOpen finds the open release rec's tag is cut onto: the project's open
// release, when the tag is above every release tag already recorded (an older
// tag, recorded late or by hand, is minted as before). It sets rec.Open and
// rec.Dropped.
func (r *Recorder) planOpen(ctx context.Context, rec *Record) error {
	rels, err := r.Daemon.ListReleases(ctx, rec.Project, "", openReleasesRead)
	if err != nil {
		return fmt.Errorf("reading %s's releases: %w", rec.Project, err)
	}
	var open *beadsapi.Release
	for i := range rels {
		switch t := rels[i].Tag; {
		case t == "":
			if open == nil {
				open = &rels[i]
			}
		case r.Matches(t) && versionLess(rec.Tag.Name, t):
			return nil
		}
	}
	if open == nil {
		return nil
	}
	rec.Open = open
	rec.Dropped = dropped(open.Contents, rec.Rows)
	return nil
}

// dropped is the rows an open release carries that git does not show between
// the previous tag and this one: the git rows win, so the cut drops them. A
// row is shown when git has its PR, or for a row with no PR, its merge sha.
func dropped(contents json.RawMessage, git []Row) []Row {
	prs, shas := map[int]bool{}, map[string]bool{}
	for _, g := range git {
		if g.PR != 0 {
			prs[g.PR] = true
		}
		shas[g.MergeSHA] = true
	}
	stamped, bad := ParseRows(contents)
	var out []Row
	for _, s := range stamped {
		if (s.PR != 0 && prs[s.PR]) || (s.PR == 0 && s.MergeSHA != "" && shas[s.MergeSHA]) {
			continue
		}
		out = append(out, s)
	}
	for range bad {
		out = append(out, Row{Title: "(a contents row the recorder cannot read)"})
	}
	return out
}

// cutFields is the one PATCH that cuts rec's tag onto its open release: the
// tag, sha, cut time, tagger and source, written once, with the previous tag
// and the contents from git.
func (rec *Record) cutFields() map[string]any {
	rows := rec.Rows
	if rows == nil {
		rows = []Row{}
	}
	f := map[string]any{
		"tag":               rec.Tag.Name,
		"source":            rec.Source,
		"contents":          rows,
		"contents_complete": rec.Complete,
	}
	for k, v := range map[string]string{
		"sha": rec.Tag.SHA, "cut_at": normalizeTime(rec.Tag.Date), "cut_by": rec.Tag.Tagger, "previous_tag": rec.Previous,
	} {
		if v != "" {
			f[k] = v
		}
	}
	return f
}

// cutOpen writes rec's tag onto rec.Open in one PATCH and returns the release
// as the daemon now holds it. A refused PATCH is checked against a fresh read:
// a release another writer already cut with this tag is the result (the
// webhook and gb release record both run on a cut); one still open is
// re-planned and written once more, since a 409 there means its contents
// changed under the write (gd-k8Z0vMaohu).
func (r *Recorder) cutOpen(ctx context.Context, rec *Record) (*beadsapi.Release, error) {
	id := rec.Open.ID
	for attempt := 0; ; attempt++ {
		werr := r.Daemon.UpdateBeadFieldsTyped(ctx, id, rec.cutFields())
		rel, err := r.Daemon.GetRelease(ctx, id, "")
		if err != nil {
			return nil, fmt.Errorf("reading release %s after its cut: %w", id, err)
		}
		switch {
		case rel.Tag == rec.Tag.Name:
			return rel, nil
		case werr == nil:
			return nil, fmt.Errorf("cutting %s onto release %s: it reads tag %q after the write", rec.Tag.Name, id, rel.Tag)
		case rel.Tag != "" || attempt > 0 || !isConflict(werr):
			return nil, fmt.Errorf("cutting %s onto release %s: %w", rec.Tag.Name, id, werr)
		}
		rec.Open = rel
		rec.Dropped = dropped(rel.Contents, rec.Rows)
	}
}

// DroppedSummary is the dropped rows on one line, for the log.
func DroppedSummary(rows []Row) string {
	parts := make([]string, 0, len(rows))
	for _, row := range rows {
		ref := row.MergeSHA
		if len(ref) > 8 {
			ref = ref[:8]
		}
		if row.PR != 0 {
			ref = fmt.Sprintf("#%d", row.PR)
		}
		parts = append(parts, strings.TrimSpace(ref+" "+row.Title))
	}
	return strings.Join(parts, "; ")
}
