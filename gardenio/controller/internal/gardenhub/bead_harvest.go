package gardenhub

import (
	"context"
	"encoding/json"
	"fmt"
	"net/url"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"gardenio/controller/internal/beadsapi"
)

// The bead view's Harvest section (harvest spec gardenio/docs/superpowers/specs/2026-10-22-lands-harvest-icons-design.md
// §6, step 4b.6.3): what a closed step yielded, read on the request like the rest of the panel. The step's own fields
// carry the MR (mr_*), the merge-time signals github-bridge and the merge desk write (ci_state, tests_touched,
// goldens_touched, review_rounds, §2.3-2.4) and its close_reason; its parent-child children carry the UX grades
// (type=evidence, §2.1), the produce (bundle, report) and the follow-ups; and the agent beads whose task_id is the step
// carry the cost and the crew's wrap-up. The base word follows the cell's rule (§4.1), so the panel and the map agree.

const (
	harvestCrewsMax = 50 // agent beads read per step (crews and follow-through crews together)
	harvestURLsMax  = 24 // picture links kept per grade
	harvestTLDRMax  = 500
)

// Base sprites of a harvest cell (harvest spec §4.1).
const (
	HarvestSheaf    = "sheaf"    // it yielded something: a merged MR, produce, or a grade
	HarvestTrampled = "trampled" // abandoned
	HarvestBare     = "bare"     // closed with nothing attached
)

// harvestGrade is one evidence bead: a UX grade, with who filed it and when (the bead's own created_by and created_at).
type harvestGrade struct {
	ID      string   `json:"id"`
	Kind    string   `json:"kind,omitempty"`
	Subject string   `json:"subject,omitempty"`
	Score   int      `json:"score"`
	Of      int      `json:"of"`
	Percent int      `json:"percent,omitempty"`
	URLs    []string `json:"urls,omitempty"`
	PR      string   `json:"pr,omitempty"`
	By      string   `json:"by,omitempty"`
	At      int64    `json:"at,omitempty"`
}

type harvestMR struct {
	URL    string `json:"url"`
	Number string `json:"number,omitempty"` // #825 on GitHub, !825 on GitLab
	State  string `json:"state"`            // merged | opened | closed
}

// beadHarvest is the panel's harvest block. A count left nil was never recorded (the writers send "0" for zero).
type beadHarvest struct {
	Base      string         `json:"base"`
	Closed    int64          `json:"closed,omitempty"`
	Lead      int64          `json:"lead,omitempty"` // seconds from created to closed
	MR        *harvestMR     `json:"mr,omitempty"`
	CI        string         `json:"ci,omitempty"`
	Tests     *int           `json:"tests,omitempty"`
	Goldens   *int           `json:"goldens,omitempty"`
	Rounds    *int           `json:"rounds,omitempty"`
	Grades    []harvestGrade `json:"grades,omitempty"` // newest first, re-grades included
	Produce   []beadRel      `json:"produce,omitempty"`
	FollowUps []beadRel      `json:"follow_ups,omitempty"`
	CostUSD   float64        `json:"cost_usd,omitempty"`
	Crews     int            `json:"crews,omitempty"`
	TLDR      string         `json:"tldr,omitempty"`
	TLDRBy    string         `json:"tldr_by,omitempty"`
	// Partial names what could not be read ("crews"), so the panel never shows an unread cost as none.
	Partial []string `json:"partial,omitempty"`
}

// beadLister is the list read the harvest block needs for the step's crews. The daemon client has it; a reader
// without it sends the block without cost or wrap-up.
type beadLister interface {
	ListBeadsFiltered(ctx context.Context, q beadsapi.ListBeadsQuery) (*beadsapi.ListBeadsResult, error)
}

type harvestCrews struct {
	agents []*beadsapi.BeadDetail
	failed bool
}

// isHarvestable is a closed work bead: a step a crew could have claimed.
func isHarvestable(b *beadsapi.BeadDetail) bool {
	if b == nil || b.Status != "closed" {
		return false
	}
	for _, t := range produceWorkTypes {
		if b.Type == t {
			return true
		}
	}
	return false
}

// readCrews lists the agent beads that claimed step, every status, follow-through crews included.
func readCrews(ctx context.Context, src BeadReader, step string) harvestCrews {
	l, ok := src.(beadLister)
	if !ok {
		return harvestCrews{failed: true}
	}
	res, err := l.ListBeadsFiltered(ctx, beadsapi.ListBeadsQuery{
		Types: []string{"agent"}, Statuses: produceStatuses, Fields: map[string]string{"task_id": step}, Limit: harvestCrewsMax,
	})
	if err != nil || res == nil {
		return harvestCrews{failed: true}
	}
	return harvestCrews{agents: res.Beads}
}

// abandonedReasons are the close_reason prefixes of a step given up on (harvest spec §4.1 rule 1).
var abandonedReasons = []string{"wontfix", "abandon", "duplicate", "obsolete", "supersede"}

// harvestBase is the cell's base by the harvest spec's §4.1 rule, first match wins: trampled when abandoned (an
// abandoning close_reason, or an MR closed unmerged with no other yield), a sheaf when there is a yield, else bare.
func harvestBase(closeReason, mrState string, otherYield bool) string {
	r := strings.ToLower(strings.TrimSpace(closeReason))
	for _, p := range abandonedReasons {
		if strings.HasPrefix(r, p) {
			return HarvestTrampled
		}
	}
	if mrState == "closed" && !otherYield {
		return HarvestTrampled
	}
	if mrState == "merged" || otherYield {
		return HarvestSheaf
	}
	return HarvestBare
}

// stepMR is the step's merge request: merged, opened (a draft counts as opened) or closed unmerged.
func stepMR(b *beadsapi.BeadDetail) *harvestMR {
	mr := mrOf(b)
	if mr == nil {
		return nil
	}
	state := strings.ToLower(strings.TrimSpace(mr.State))
	switch {
	case state == "merged" || strings.EqualFold(strings.TrimSpace(b.Fields["mr_merged"]), "true"):
		state = "merged"
	case state == "closed":
	default:
		state = "opened"
	}
	return &harvestMR{URL: mr.URL, Number: mrNumber(mr.URL, mr.IID), State: state}
}

var mrNumRE = regexp.MustCompile(`/(pull|merge_requests)/(\d+)(?:[/?#]|$)`)

// mrNumber is the forge's way of writing an MR's number: #825 for a GitHub pull request, !825 for a GitLab merge request.
func mrNumber(u, iid string) string {
	m := mrNumRE.FindStringSubmatch(u)
	n := strings.TrimSpace(iid)
	if n == "" && m != nil {
		n = m[2]
	}
	if _, err := strconv.Atoi(n); err != nil {
		return ""
	}
	if m != nil && m[1] == "merge_requests" {
		return "!" + n
	}
	return "#" + n
}

// fieldCount reads a recorded count: nil when absent or not a number, so "not recorded" is never shown as zero.
func fieldCount(f map[string]string, k string) *int {
	v, err := strconv.Atoi(strings.TrimSpace(f[k]))
	if err != nil || v < 0 {
		return nil
	}
	return &v
}

func fieldInt(f map[string]string, k string) int {
	v, _ := strconv.Atoi(strings.TrimSpace(f[k]))
	return v
}

// gradeURLs keeps the http(s) links of an evidence bead's urls field (newline-joined, gb image upload output).
func gradeURLs(raw string) []string {
	var out []string
	for _, ln := range strings.FieldsFunc(raw, func(r rune) bool { return r == '\n' || r == '\r' || r == ' ' || r == '\t' }) {
		u, err := url.Parse(ln)
		if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
			continue
		}
		out = append(out, ln)
		if len(out) >= harvestURLsMax {
			break
		}
	}
	return out
}

func unixOf(t time.Time) int64 {
	if t.IsZero() {
		return 0
	}
	return t.Unix()
}

// closedAt parses the daemon's closed_at (RFC 3339), 0 when absent.
func closedAt(s string) int64 {
	t, err := time.Parse(time.RFC3339Nano, strings.TrimSpace(s))
	if err != nil {
		return 0
	}
	return t.Unix()
}

// buildHarvest assembles the block from the step, its hydrated children and its crews.
func buildHarvest(b *beadsapi.BeadDetail, children []beadRel, got map[string]*beadsapi.BeadDetail, crews harvestCrews) *beadHarvest {
	h := &beadHarvest{
		Closed: closedAt(b.ClosedAt), MR: stepMR(b), CI: strings.TrimSpace(b.Fields["ci_state"]),
		Tests: fieldCount(b.Fields, "tests_touched"), Goldens: fieldCount(b.Fields, "goldens_touched"), Rounds: fieldCount(b.Fields, "review_rounds"),
	}
	if c := unixOf(b.CreatedAt); c > 0 && h.Closed >= c {
		h.Lead = h.Closed - c
	}
	for _, r := range children {
		c := got[r.ID]
		switch {
		case c == nil:
		case c.Type == "evidence":
			h.Grades = append(h.Grades, harvestGrade{
				ID: c.ID, Kind: strings.TrimSpace(c.Fields["kind"]), Subject: clip(c.Fields["subject"], 120),
				Score: fieldInt(c.Fields, "score"), Of: fieldInt(c.Fields, "of"), Percent: fieldInt(c.Fields, "percent"),
				URLs: gradeURLs(c.Fields["urls"]), PR: strings.TrimSpace(c.Fields["pr"]), By: c.CreatedBy, At: unixOf(c.CreatedAt),
			})
		case c.Type == "bundle" || c.Type == "report":
			h.Produce = append(h.Produce, r)
		case isWorkType(c.Type):
			h.FollowUps = append(h.FollowUps, r)
		}
	}
	sort.SliceStable(h.Grades, func(i, j int) bool {
		if h.Grades[i].At != h.Grades[j].At {
			return h.Grades[i].At > h.Grades[j].At
		}
		return h.Grades[i].ID > h.Grades[j].ID
	})
	mrState := ""
	if h.MR != nil {
		mrState = h.MR.State
	}
	h.Base = harvestBase(b.Fields["close_reason"], mrState, len(h.Produce) > 0 || len(h.Grades) > 0)

	if crews.failed {
		h.Partial = append(h.Partial, "crews")
	}
	agents := append([]*beadsapi.BeadDetail(nil), crews.agents...)
	sort.SliceStable(agents, func(i, j int) bool { return agents[i].CreatedAt.After(agents[j].CreatedAt) }) // newest first
	for _, a := range agents {
		if a == nil {
			continue
		}
		h.Crews++
		h.CostUSD += agentCost(a.Fields)
		if h.TLDR == "" {
			if t := wrapTLDR(a.Fields["wrapup"]); t != "" {
				h.TLDR, h.TLDRBy = t, firstNonEmpty(a.Fields["agent"], a.Title, a.ID)
			}
		}
	}
	return h
}

func isWorkType(t string) bool {
	for _, w := range produceWorkTypes {
		if t == w {
			return true
		}
	}
	return false
}

// agentCost is what one crew spent: its final total, or the live figure of a crew still running.
func agentCost(f map[string]string) float64 {
	for _, k := range []string{"total_cost_usd", "live_cost_usd"} {
		if v, err := strconv.ParseFloat(strings.TrimSpace(f[k]), 64); err == nil && v > 0 {
			return v
		}
	}
	return 0
}

// wrapTLDR is a gb stop wrap-up's tldr. A wrap-up the controller synthesized for a crew it reaped says only why it
// died, which is not the step's outcome, so it is skipped.
func wrapTLDR(raw string) string {
	var w struct {
		TLDR   string         `json:"tldr"`
		Custom map[string]any `json:"custom"`
	}
	if strings.TrimSpace(raw) == "" || json.Unmarshal([]byte(raw), &w) != nil || fmt.Sprint(w.Custom["synthesized"]) == "true" {
		return ""
	}
	return clipKeep(w.TLDR, harvestTLDRMax)
}
