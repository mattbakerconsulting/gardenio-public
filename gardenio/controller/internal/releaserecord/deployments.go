package releaserecord

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"regexp"
	"sort"
	"strings"
	"time"

	"gardenio/controller/internal/beadsapi"
)

// The deployment backfill (plan gd-UZKZ9H25UD step 4): before release-deploy.sh
// wrote deployments, each roll left a closed type=event bead titled
// "release-deploy <tag>: <outcome>", labelled release-deploy, with the helm
// revision in its description: "helm revision after: N" from the CronJob, or
// prose ("helm rev 259") from an operator's hand roll. BackfillDeployments
// replays the successful ones, oldest first, as deployments on the target, so
// the release history has where each release ran. The daemon slots a row older
// than the target's newest into history (closed), so the release running there
// now keeps its open row.

// DeployEventLabel is the label release-deploy.sh puts on its event beads.
const DeployEventLabel = "release-deploy"

// DeployDaemon is the slice of the daemon client the deployment backfill uses.
type DeployDaemon interface {
	ListBeadsFiltered(ctx context.Context, q beadsapi.ListBeadsQuery) (*beadsapi.ListBeadsResult, error)
	DeployRelease(ctx context.Context, ref, project string, req beadsapi.DeployReleaseRequest) (*beadsapi.Release, error)
}

var _ DeployDaemon = (*beadsapi.Client)(nil)

// DeployEvent is one successful roll a release-deploy event bead records.
type DeployEvent struct {
	Bead     string `json:"bead"`
	Tag      string `json:"tag"`
	Revision string `json:"revision,omitempty"`
	At       string `json:"at"`
}

var (
	deployEventTitle = regexp.MustCompile(`^release-deploy (\S+): (.*)$`)
	// The helm revision the roll made, most specific form first: the
	// CronJob's "helm revision after: N", an operator's "became rev N", then
	// "helm rev N" / "helm revision N".
	deployEventRevision = []*regexp.Regexp{
		regexp.MustCompile(`(?i)helm revision after:\s*(\d+)`),
		regexp.MustCompile(`(?i)became rev(?:ision)?\s+(\d+)`),
		regexp.MustCompile(`(?i)helm rev(?:ision)?\s+(\d+)`),
	}
)

// ParseDeployEvent reads one release-deploy event bead. ok is false unless its
// outcome is a verified roll ("deployed and verified", or "ok"); a dry run,
// a skip and a failure are not deployments. Revision is empty when the
// description names none.
func ParseDeployEvent(title, description string, createdAt time.Time) (DeployEvent, bool) {
	m := deployEventTitle.FindStringSubmatch(strings.TrimSpace(title))
	if m == nil {
		return DeployEvent{}, false
	}
	switch strings.TrimSpace(m[2]) {
	case "deployed and verified", "ok":
	default:
		return DeployEvent{}, false
	}
	ev := DeployEvent{Tag: m[1], At: createdAt.UTC().Truncate(time.Second).Format(time.RFC3339)}
	for _, re := range deployEventRevision {
		if r := re.FindStringSubmatch(description); r != nil {
			ev.Revision = r[1]
			break
		}
	}
	return ev, true
}

// DeployBackfillItem is one event of a deployment backfill and what became of
// it: recorded, skipped (Skip says why) or, on a dry run, neither.
type DeployBackfillItem struct {
	DeployEvent
	Recorded bool   `json:"recorded"`
	Skip     string `json:"skip,omitempty"`
}

// DeployBackfillOptions are the knobs of BackfillDeployments.
type DeployBackfillOptions struct {
	Project string
	Target  string
	// Since drops events before it (zero: every event).
	Since time.Time
	// Write false only plans.
	Write bool
}

// deployEventPage bounds each list call; the backfill pages until the daemon
// has no more.
const deployEventPage = 500

// BackfillDeployments replays the project's successful release-deploy events
// as deployments on opts.Target, oldest first. A roll reported twice (the
// CronJob's event and an operator's note on the same revision) is written
// once, at the first report. A tag with no recorded release is skipped, not
// an error: run gb release backfill first. A replay is a no-op on the daemon,
// which records a helm revision once per target: a revision another release
// already holds is a 409, skipped here.
func BackfillDeployments(ctx context.Context, d DeployDaemon, opts DeployBackfillOptions) ([]DeployBackfillItem, error) {
	if opts.Project == "" || opts.Target == "" {
		return nil, errors.New("deployment backfill: project and target are required")
	}
	var events []DeployEvent
	for offset := 0; ; offset += deployEventPage {
		res, err := d.ListBeadsFiltered(ctx, beadsapi.ListBeadsQuery{
			Types:    []string{"event"},
			Statuses: []string{"closed"},
			Labels:   []string{DeployEventLabel, "project:" + opts.Project},
			Limit:    deployEventPage,
			Offset:   offset,
		})
		if err != nil {
			return nil, fmt.Errorf("listing release-deploy events: %w", err)
		}
		for _, b := range res.Beads {
			if !opts.Since.IsZero() && b.CreatedAt.Before(opts.Since) {
				continue
			}
			if ev, ok := ParseDeployEvent(b.Title, b.Description, b.CreatedAt); ok {
				ev.Bead = b.ID
				events = append(events, ev)
			}
		}
		if len(res.Beads) < deployEventPage {
			break
		}
	}
	sort.SliceStable(events, func(i, j int) bool { return events[i].At < events[j].At })

	seen := map[string]bool{}
	var items []DeployBackfillItem
	for _, ev := range events {
		it := DeployBackfillItem{DeployEvent: ev}
		key := ev.Tag + "@" + ev.Revision
		switch {
		case ev.Revision != "" && seen[key]:
			it.Skip = "this roll is already recorded from an earlier event"
		case !opts.Write:
		default:
			_, err := d.DeployRelease(ctx, ev.Tag, opts.Project, beadsapi.DeployReleaseRequest{
				Target: opts.Target, Since: ev.At, By: "backfill", Via: "script",
				Revision: ev.Revision, ImageTag: ev.Tag,
			})
			var apiErr *beadsapi.APIError
			switch {
			case err == nil:
				it.Recorded = true
			case errors.As(err, &apiErr) && apiErr.StatusCode == http.StatusNotFound:
				it.Skip = "no release is recorded for this tag"
			case errors.As(err, &apiErr) && apiErr.StatusCode == http.StatusConflict:
				it.Skip = "another release records this revision: " + apiErr.Message
			default:
				return items, fmt.Errorf("recording %s %s from %s: %w", opts.Project, ev.Tag, ev.Bead, err)
			}
		}
		seen[key] = true
		items = append(items, it)
	}
	return items, nil
}
