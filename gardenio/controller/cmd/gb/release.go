package main

// gb release — a project's release record (gardenio/docs/releases-design.md,
// plan gd-UZKZ9H25UD step 3): `record`, `built` and `backfill` write it
// (release_record.go), and `smoke` is the read-only prod UAT probe
// (release_smoke.go). The GitLab-only `gb release status` was retired with
// the release record; read releases with `kd release list --project <p>`.

import (
	"regexp"
	"sort"
	"strconv"

	"github.com/spf13/cobra"
)

// defaultReleaseProject is the gardenio GitLab project `gb release smoke`
// reads tags and releases from. Overridable with --project.
const defaultReleaseProject = "example-org/gardenio"

var releaseCmd = &cobra.Command{
	Use:     "release",
	Short:   "Record a project's releases (record, built, backfill) and probe prod (smoke)",
	GroupID: "orchestration",
}

// releaseCalverRe matches calver tags like "2026.177.7" or "v2026.177.7".
// Mirrors bridge.calverRe; release tags are bare calver (the "-rc" suffix
// lives on image tags, not git tags — see doc ka-0000000327).
var releaseCalverRe = regexp.MustCompile(`^v?(\d{4})\.(\d+)\.(\d+)$`)

// calverTag is a parsed calver tag carrying its sort key.
type calverTag struct {
	name             string
	year, doy, build int
}

// parseCalverTag extracts the calver sort key from a tag name, or ok=false
// when the name is not a calver tag.
func parseCalverTag(name string) (calverTag, bool) {
	m := releaseCalverRe.FindStringSubmatch(name)
	if m == nil {
		return calverTag{}, false
	}
	y, _ := strconv.Atoi(m[1])
	d, _ := strconv.Atoi(m[2])
	b, _ := strconv.Atoi(m[3])
	return calverTag{name: name, year: y, doy: d, build: b}, true
}

// less reports whether a sorts before b in ascending calver order.
func (a calverTag) less(b calverTag) bool {
	if a.year != b.year {
		return a.year < b.year
	}
	if a.doy != b.doy {
		return a.doy < b.doy
	}
	return a.build < b.build
}

// sortCalverDesc parses tag names, drops non-calver names, and returns them
// newest-first. The input order is not trusted — the GitLab tags API default
// ordering is by commit/update date, which can diverge from calver order.
func sortCalverDesc(names []string) []calverTag {
	var cv []calverTag
	for _, n := range names {
		if t, ok := parseCalverTag(n); ok {
			cv = append(cv, t)
		}
	}
	sort.Slice(cv, func(i, j int) bool { return cv[j].less(cv[i]) })
	return cv
}
