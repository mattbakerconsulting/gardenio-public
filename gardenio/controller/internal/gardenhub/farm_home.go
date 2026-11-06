package gardenhub

// farm_home.go — the home strips in counts (homestead-world-design.md section 28, step 2 gd-GYo4PWlGUb). Every biome,
// the Commons included, has a home strip: its Mailbox, Growing field, Barn and Pen. The page fills the strip's cells
// from the ledger and the document's rows; the counts here let a project's sign at the Project level say "3 letters ·
// 2 growing · 4 at the barn" without walking the ledger, and let layout() know a project with only home items is a
// biome (28.3) without reading the ledger at all.

import (
	"encoding/json"
	"strings"

	"gardenio/controller/internal/beadsapi"
)

// emptyHome is the home blob before the first refresh, and a farm with nothing at home.
const emptyHome = `{}`

// FarmHome is one biome's home strip in counts. A cell counts every item it holds, past what its rect can show.
type FarmHome struct {
	Letters int `json:"letters"` // open decisions and escalations: the Mailbox
	Field   int `json:"field"`   // loose work beads in any open state: the Growing field
	Growing int `json:"growing"` // of field, those in progress or in review: drawn growing, not as wildflowers
	Barn    int `json:"barn"`    // gardeners at home in this barn (barnOf)
	Wishes  int `json:"wishes"`  // open wishes, listed and past the cap (open_more): the Pen
}

// FarmHomes is the home blob: one FarmHome per project key, "" for the Commons (items with no project). A key whose
// counts are all zero is left out.
type FarmHomes map[string]FarmHome

// lettersOf counts the open decisions and escalations among rows by project: the Mailbox's letters, as the hub's
// decision section lists them (status open only).
func lettersOf(rows []*beadsapi.BeadDetail) map[string]int {
	out := map[string]int{}
	for _, b := range rows {
		if (b.Type == "decision" || b.Type == "escalation") && b.Status == "open" {
			out[projectOf(b)]++
		}
	}
	return out
}

// hasRole is whether a gardener's comma-separated role names one of want (web/world.js isSteward, isThread).
func hasRole(role string, want ...string) bool {
	for _, r := range strings.Split(role, ",") {
		r = strings.ToLower(strings.TrimSpace(r))
		for _, w := range want {
			if r == w {
				return true
			}
		}
	}
	return false
}

// barnOf is the project key of the barn a gardener is at home in, and false when it is not in a barn: a steward with a
// project stands at its land's sign, and an alive or stuck gardener with a plot is out on it (counted there, not
// in a barn). A thread agent lives in the Commons's barn whatever its project (28.4, H-D10).
func barnOf(g FarmGardener) (string, bool) {
	if hasRole(g.Role, StewardRole) && g.Project != "" {
		return "", false
	}
	if g.Plot != nil && (g.Bucket == GardenerAlive || g.Bucket == GardenerStuck) {
		return "", false
	}
	if hasRole(g.Role, "thread", "dm") {
		return "", true
	}
	return g.Project, true
}

// homesOf is the home blob from the letters by project, the meadow rows, the gardeners and the built wishes, keeping
// the keys allow admits (nil admits every key). A scoped cut passes the gardeners and wishes it admits, so nothing it
// hides is counted.
func homesOf(letters map[string]int, loose []cutLoose, gardeners []FarmGardener, wishes FarmWishes, allow func(string) bool) FarmHomes {
	out := FarmHomes{}
	add := func(key string, f func(*FarmHome)) {
		if allow != nil && !allow(key) {
			return
		}
		h := out[key]
		f(&h)
		out[key] = h
	}
	for key, n := range letters {
		if n > 0 {
			add(key, func(h *FarmHome) { h.Letters += n })
		}
	}
	for _, l := range loose {
		add(l.t.Project, func(h *FarmHome) {
			h.Field++
			if l.t.State == TaskInProgress || l.t.State == TaskReview {
				h.Growing++
			}
		})
	}
	for _, g := range gardeners {
		if key, ok := barnOf(g); ok {
			add(key, func(h *FarmHome) { h.Barn++ })
		}
	}
	for _, w := range wishes.Open {
		add(w.Project, func(h *FarmHome) { h.Wishes++ })
	}
	for key, n := range wishes.OpenMore {
		if n > 0 {
			add(key, func(h *FarmHome) { h.Wishes += n })
		}
	}
	return out
}

// homeRaw sets the cut's letters from rows and is the whole farm's home blob: the letters, the cut's meadow rows and
// gardener set, and the built wishes.
func (st *farmCutState) homeRaw(rows []*beadsapi.BeadDetail, wishes FarmWishes) []byte {
	st.letters = lettersOf(rows)
	gardeners := make([]FarmGardener, 0, len(st.gardenerSet))
	for _, g := range st.gardenerSet {
		gardeners = append(gardeners, g)
	}
	return homesRaw(st.letters, st.loose, gardeners, wishes, nil)
}

// homesRaw is homesOf marshalled.
func homesRaw(letters map[string]int, loose []cutLoose, gardeners []FarmGardener, wishes FarmWishes, allow func(string) bool) []byte {
	b, _ := json.Marshal(homesOf(letters, loose, gardeners, wishes, allow))
	return b
}
