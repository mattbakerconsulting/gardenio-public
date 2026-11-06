package releaserecord

import (
	"context"
	"fmt"
	"time"

	"gardenio/controller/internal/beadsapi"
)

// DefaultBackfill is how many of a project's newest release tags the backfill
// writes when no --since is given.
const DefaultBackfill = 30

// BackfillTags picks the release tags a backfill writes, oldest first: from
// since (inclusive) to the newest, or the newest last when since is "".
func (r *Recorder) BackfillTags(ctx context.Context, project, since string, last int) ([]Tag, error) {
	if err := r.Load(ctx, project); err != nil {
		return nil, err
	}
	tags, err := r.ReleaseTags(ctx)
	if err != nil {
		return nil, err
	}
	if since != "" {
		if !r.Matches(since) {
			return nil, fmt.Errorf("--since %s does not match %s's release_tag_pattern %q", since, project, r.pattern)
		}
		var out []Tag
		for _, t := range tags {
			if t.Name == since || versionLess(since, t.Name) {
				out = append(out, t)
			}
		}
		return out, nil
	}
	if last <= 0 {
		last = DefaultBackfill
	}
	if len(tags) > last {
		tags = tags[len(tags)-last:]
	}
	return tags, nil
}

// NewTags picks the release tags a tag poll records, oldest first: the tags
// above the greatest release tag among recorded (the tags of the project's
// recorded releases), at most the newest DefaultBackfill of them. A project
// with no recorded release gets only its newest tag: the backfill writes the
// history, not the poll.
func (r *Recorder) NewTags(ctx context.Context, project string, recorded []string) ([]Tag, error) {
	if err := r.Load(ctx, project); err != nil {
		return nil, err
	}
	tags, err := r.ReleaseTags(ctx)
	if err != nil {
		return nil, err
	}
	newest := ""
	for _, name := range recorded {
		if r.Matches(name) && (newest == "" || versionLess(newest, name)) {
			newest = name
		}
	}
	if newest == "" {
		if len(tags) == 0 {
			return nil, nil
		}
		return tags[len(tags)-1:], nil
	}
	var out []Tag
	for _, t := range tags {
		if versionLess(newest, t.Name) {
			out = append(out, t)
		}
	}
	if len(out) > DefaultBackfill {
		out = out[len(out)-DefaultBackfill:]
	}
	return out, nil
}

// BackfillItem is one tag of a backfill: its record, and what the write did
// (nil on a dry run).
type BackfillItem struct {
	Record *Record
	Result *Result
}

// Backfill records each tag oldest first, so a release's previous_tag finds
// the release written just before it. With write false it only plans. A tag
// already recorded with its contents is a no-op, so a second run writes
// nothing.
func (r *Recorder) Backfill(ctx context.Context, project string, tags []Tag, write bool) ([]BackfillItem, error) {
	var items []BackfillItem
	for _, t := range tags {
		rec, err := r.Plan(ctx, project, t.Name, SourceBackfill)
		if err != nil {
			return items, fmt.Errorf("%s: %w", t.Name, err)
		}
		item := BackfillItem{Record: rec}
		if write {
			res, err := r.Write(ctx, rec)
			item.Result = res
			if err != nil {
				return append(items, item), fmt.Errorf("%s: %w", t.Name, err)
			}
		}
		items = append(items, item)
	}
	return items, nil
}

// Image is one built artifact of a release: {"name","ref"}.
type Image struct {
	Name string `json:"name"`
	Ref  string `json:"ref"`
}

// MarkBuilt sets built_at (and images, when given) on the release of tag,
// moving it from cut to built. A release that already has built_at is left
// alone and false is returned.
func MarkBuilt(ctx context.Context, d Daemon, project, tag, builtAt string, images []Image) (*beadsapi.Release, bool, error) {
	rel, err := d.GetRelease(ctx, tag, project)
	if err != nil {
		return nil, false, err
	}
	if rel.BuiltAt != "" {
		return rel, false, nil
	}
	if builtAt == "" {
		builtAt = time.Now().UTC().Format(time.RFC3339)
	}
	fields := map[string]any{"built_at": normalizeTime(builtAt)}
	if len(images) > 0 {
		fields["images"] = images
	}
	if err := d.UpdateBeadFieldsTyped(ctx, rel.ID, fields); err != nil {
		return rel, false, fmt.Errorf("marking release %s built: %w", rel.ID, err)
	}
	return rel, true, nil
}
