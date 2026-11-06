package gardenhub

// farm_releases.go — releases on the farm document (plan gd-UZKZ9H25UD step 5,
// gardenio/docs/releases-design.md §3.5).
//
// A release is a type=release bead, one tag of one project: the merged PRs it
// carries (contents) and where it runs (deployments), with a status the daemon
// derives (cut, built, live, superseded). The truck on /garden reads a
// project's newest release and what runs on each target from the `releases`
// blob, and a plot says which release shipped its plan's newest merged PR from
// FarmPlan.Shipped.
//
// The read rides the slow tier at most every farmReleasesEvery: release beads
// change a few times a day. It is bounded: one list of the project beads that
// carry a release_tag_pattern (ReleaseProjects), then the newest
// maxReleasesPerProject releases of each through the daemon's GET /v1/releases
// (newest cut first), never a read per PR. The counts come from each release's contents rows. A failed
// read keeps the last one read; it never fails the farm refresh.

import (
	"context"
	"encoding/json"
	"sort"
	"strings"
	"time"

	"gardenio/controller/internal/beadsapi"
)

const (
	// maxReleasesPerProject bounds the releases read and listed per project.
	maxReleasesPerProject = 12
	// farmReleasesEvery bounds how often the releases are re-read.
	farmReleasesEvery = 60 * time.Second
	// farmReleasesTimeout bounds the whole releases read, inside the farm
	// refresh's refreshTimeout like farmCostTimeout.
	farmReleasesTimeout = 3 * time.Second
)

// emptyReleases is the blob before the first read.
const emptyReleases = `{}`

// ReleaseSource names the projects that have releases and reads their
// releases. DaemonReleases is the daemon's.
type ReleaseSource interface {
	// ReleaseProjects names the live projects with a release source (a
	// release_tag_pattern on the project bead), sorted.
	ReleaseProjects(ctx context.Context) ([]string, error)
	// ListReleases reads a project's releases, newest cut first, at most limit
	// (beadsapi.Client.ListReleases).
	ListReleases(ctx context.Context, project, status string, limit int) ([]beadsapi.Release, error)
}

// releaseDaemon is the daemon client as DaemonReleases uses it.
type releaseDaemon interface {
	ListBeadsFiltered(ctx context.Context, q beadsapi.ListBeadsQuery) (*beadsapi.ListBeadsResult, error)
	ListReleases(ctx context.Context, project, status string, limit int) ([]beadsapi.Release, error)
}

// DaemonReleases is the ReleaseSource over the daemon client.
func DaemonReleases(c releaseDaemon) ReleaseSource { return daemonReleases{c} }

type daemonReleases struct{ releaseDaemon }

// SetReleaseSource wires the release read. Without one the releases blob is {}
// and the farm makes no release reads. Call it before Start.
func (f *Farm) SetReleaseSource(s ReleaseSource) { f.rel = s }

// releaseState is what the releases read last kept. Touched only by refresh,
// which is serialised; blob and shipped are replaced whole, never mutated.
type releaseState struct {
	at        time.Time
	byProject map[string][]beadsapi.Release // newest cut first, at most maxReleasesPerProject
	open      map[string]beadsapi.Release   // each project's open release, the one merged PRs board
	blob      FarmReleases
	shipped   shippedIndex
}

// releaseContent is the part of a contents row (releases-design.md §3.1) the
// farm reads.
type releaseContent struct {
	URL      string `json:"url"`
	MergedAt string `json:"merged_at"`
	Plan     string `json:"plan"`
}

// releaseContents decodes a release's contents; a missing or malformed list is
// no rows.
func releaseContents(r beadsapi.Release) []releaseContent {
	var rows []releaseContent
	if len(r.Contents) > 0 {
		_ = json.Unmarshal(r.Contents, &rows)
	}
	return rows
}

// shippedIndex answers FarmPlan.Shipped and Boarded from the releases read.
// Every map is keyed by project and id (shipKey), so a release of another
// project never answers for a plan.
type shippedIndex struct {
	byURL  map[string]string // project, PR url -> the tag of the release carrying it
	byPlan map[string]string // project, plan -> the tag of the release carrying its newest merged PR (by merged_at)
	// boarded holds the PR urls and plans the project's open release answers
	// for: a PR on it, a plan whose newest merged PR is.
	boarded map[string]bool
}

func shipKey(project, id string) string { return project + "\x00" + id }

func prKey(u string) string { return strings.TrimRight(strings.TrimSpace(u), "/") }

// of is the tag the plan's newest merged PR shipped in, "" when none of the
// releases read carries it; boarded is true when the open release carries it
// instead. The newest merged PR is the first merged MR in the plan's produce
// bin (merged MRs come newest first); a bin with none listed (capped, or not
// read) falls back to the plan's newest PR in any release.
func (ix shippedIndex) of(plan, project string, produce []FarmProduce) (tag string, boarded bool) {
	k := shipKey(project, plan)
	for _, p := range produce {
		if p.Kind == ProduceMR && p.Ready {
			k = shipKey(project, prKey(p.URL))
			return ix.byURL[k], ix.boarded[k]
		}
	}
	return ix.byPlan[k], ix.boarded[k]
}

// board indexes each project's open release: a PR on it that no cut release
// carries, and each plan it carries rows of. A merged PR boards the open
// release after every cut, so its plans' newest merged PR is there.
func (ix *shippedIndex) board(open map[string]beadsapi.Release) {
	ix.boarded = map[string]bool{}
	for project, r := range open {
		for _, c := range releaseContents(r) {
			if u := prKey(c.URL); u != "" && ix.byURL[shipKey(project, u)] == "" {
				ix.boarded[shipKey(project, u)] = true
			}
			if c.Plan != "" {
				ix.boarded[shipKey(project, c.Plan)] = true
				delete(ix.byPlan, shipKey(project, c.Plan))
			}
		}
	}
}

// projectBeadName is a type:project bead's project name.
func projectBeadName(b *beadsapi.BeadDetail) string {
	return strings.TrimSpace(strings.TrimPrefix(b.Title, "Project: "))
}

// ReleaseProjects lists the live project beads that carry a
// release_tag_pattern, by name.
func (d daemonReleases) ReleaseProjects(ctx context.Context) ([]string, error) {
	res, err := d.ListBeadsFiltered(ctx, beadsapi.ListBeadsQuery{
		Types: []string{"project"}, Statuses: farmOpenStatuses, Fields: map[string]string{"release_tag_pattern": "*"}, Limit: listLimit,
	}.OnlyFields("release_tag_pattern"))
	if err != nil {
		return nil, err
	}
	seen := map[string]bool{}
	var out []string
	for _, b := range res.Beads {
		if b == nil {
			continue
		}
		if name := projectBeadName(b); name != "" && !seen[name] {
			seen[name] = true
			out = append(out, name)
		}
	}
	sort.Strings(out)
	return out, nil
}

// refreshReleases re-reads every release-enabled project's newest releases. A
// failed project list keeps every project's last read; a failed project keeps
// its own. It never fails the farm refresh.
func (f *Farm) refreshReleases(ctx context.Context) {
	ctx, cancel := context.WithTimeout(ctx, farmReleasesTimeout)
	defer cancel()
	f.relSt.at = f.now()
	projects, err := f.rel.ReleaseProjects(ctx)
	if err != nil {
		f.logger.Warn("garden farm: release project read failed; keeping the last releases", "error", err)
		return
	}
	by := make(map[string][]beadsapi.Release, len(projects))
	open := map[string]beadsapi.Release{}
	for _, p := range projects {
		rels, err := f.rel.ListReleases(ctx, p, "", maxReleasesPerProject)
		if err != nil {
			f.logger.Warn("garden farm: release read failed; keeping the project's last releases", "project", p, "error", err)
			if old, ok := f.relSt.byProject[p]; ok {
				by[p] = old
			}
			if old, ok := f.relSt.open[p]; ok {
				open[p] = old
			}
			continue
		}
		for _, r := range rels {
			if r.Tag == "" {
				open[p] = r
				break
			}
		}
		rels = beadsapi.CutReleases(rels)
		sortReleases(rels)
		if len(rels) > maxReleasesPerProject {
			rels = rels[:maxReleasesPerProject]
		}
		by[p] = rels
	}
	f.relSt.byProject, f.relSt.open = by, open
	f.relSt.blob, f.relSt.shipped = buildReleases(by)
	f.relSt.shipped.board(open)
}

// sortReleases orders releases newest cut first, then newest created, as the
// daemon lists them (kbeads sortReleasesNewestFirst). RFC 3339 UTC strings sort
// as times.
func sortReleases(rels []beadsapi.Release) {
	sort.SliceStable(rels, func(i, j int) bool {
		if rels[i].CutAt != rels[j].CutAt {
			return rels[i].CutAt > rels[j].CutAt
		}
		return rels[i].CreatedAt.After(rels[j].CreatedAt)
	})
}

// buildReleases is the releases blob and the shipped index from each
// project's releases, newest cut first. A project with no release yet has a
// key with a null latest: its truck stands empty.
func buildReleases(by map[string][]beadsapi.Release) (FarmReleases, shippedIndex) {
	out := FarmReleases{}
	ix := shippedIndex{byURL: map[string]string{}, byPlan: map[string]string{}}
	newest := map[string]string{} // byPlan's key -> the merged_at of the row it holds
	for project, rels := range by {
		pr := FarmProjectReleases{Deployments: []FarmReleaseDeployment{}, Recent: make([]FarmReleaseRow, 0, len(rels))}
		held := map[string]bool{} // targets a newer release already holds
		for i, r := range rels {
			rows := releaseContents(r)
			plans := map[string]int{}
			unplanned := 0
			for _, c := range rows {
				if u := prKey(c.URL); u != "" {
					if _, ok := ix.byURL[shipKey(project, u)]; !ok {
						ix.byURL[shipKey(project, u)] = r.Tag
					}
				}
				if c.Plan == "" {
					unplanned++
					continue
				}
				plans[c.Plan]++
				// Newest cut first, so on a tie the newer release keeps it.
				k := shipKey(project, c.Plan)
				if at, ok := newest[k]; !ok || c.MergedAt > at {
					newest[k], ix.byPlan[k] = c.MergedAt, r.Tag
				}
			}
			pr.Recent = append(pr.Recent, FarmReleaseRow{ID: r.ID, Tag: r.Tag, Status: r.Status, CutAt: fieldUnix(r.CutAt), Count: len(rows), Plans: len(plans)})

			var liveSince int64
			for _, d := range r.Deployments {
				if d.Until != "" {
					continue
				}
				since := fieldUnix(d.Since)
				if liveSince == 0 || (since > 0 && since < liveSince) {
					liveSince = since
				}
				if held[d.Target] {
					continue
				}
				held[d.Target] = true
				pr.Deployments = append(pr.Deployments, FarmReleaseDeployment{Target: d.Target, Kind: d.Kind, Tag: r.Tag, Release: r.ID, Since: since, Revision: d.Revision})
			}

			if i == 0 {
				lt := &FarmRelease{ID: r.ID, Tag: r.Tag, Status: r.Status, CutAt: fieldUnix(r.CutAt), BuiltAt: fieldUnix(r.BuiltAt), Count: len(rows), Plans: []FarmReleasePlan{}, Unplanned: unplanned}
				if r.Status == beadsapi.ReleaseStatusLive {
					lt.LiveSince = liveSince
				}
				for id, n := range plans {
					lt.Plans = append(lt.Plans, FarmReleasePlan{ID: id, Count: n})
				}
				sort.Slice(lt.Plans, func(a, b int) bool {
					if lt.Plans[a].Count != lt.Plans[b].Count {
						return lt.Plans[a].Count > lt.Plans[b].Count
					}
					return lt.Plans[a].ID < lt.Plans[b].ID
				})
				pr.Latest = lt
				pr.Shipping = r.Status == beadsapi.ReleaseStatusCut || r.Status == beadsapi.ReleaseStatusBuilt
			}
		}
		sort.Slice(pr.Deployments, func(a, b int) bool { return pr.Deployments[a].Target < pr.Deployments[b].Target })
		out[project] = pr
	}
	return out, ix
}

// cutReleases is the releases blob cut to allow: the keys of its projects.
func cutReleases(all FarmReleases, allow func(string) bool) FarmReleases {
	out := FarmReleases{}
	for p, r := range all {
		if allow(p) {
			out[p] = r
		}
	}
	return out
}
