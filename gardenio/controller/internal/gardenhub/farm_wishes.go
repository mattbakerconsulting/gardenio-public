package gardenhub

// farm_wishes.go — wishes on the farm document (gd-EGpbDpqsec, Phase 1).
//
// A wish is a Slack or Commons thread head: a type=task bead with the `thread`
// label (internal/bridge/thread_bead.go). Everything a wish gave rise to
// points back at it with fields.thread_id = the head's id; the head itself
// carries none. Until now a head was one more loose bead in the meadow, so
// the Meadow total and the weeds rules counted it as work nobody planted.
// Here it is a wish: listed under `wishes`, never a plant.
//
// Everything is read from what the farm already reads: the open heads and
// their kin come from the fast tier's open rows (the head by its label, the
// kin by their own thread_id field), the closed heads and the closed kin from
// the closed rows the slow tier keeps (farm_closed.go). The one extra daemon
// call is the count of closed heads, in the slow tier with the other counts.

import (
	"context"
	"net/url"
	"sort"
	"strings"

	"gardenio/controller/internal/beadsapi"
)

const (
	// maxWishesPerProject bounds the listed open wishes per project (newest
	// first; FarmWishes.OpenMore counts the rest), like the meadow's cap.
	maxWishesPerProject = 40
	// maxClosedWishesPerProject bounds the listed headstones per project, like
	// the closed plans' cap.
	maxClosedWishesPerProject = 24
	// maxWishKin bounds the kin ids one wish lists (FarmWish.KinTotal is exact).
	maxWishKin = 12
	// maxWishProduce bounds the merged MRs one headstone lists
	// (FarmClosedWish.ProduceMore counts the rest).
	maxWishProduce = 6
	// maxWishArms bounds the distinct line arms one wish shows (FarmWish.Arms).
	maxWishArms = 2
)

// emptyWishes is the blob before the first refresh.
const emptyWishes = `{"open":[],"open_total":0,"closed":[],"closed_total":0}`

// wishLabel is the kind label every thread head carries (bridge threadKindLabel).
const wishLabel = "thread"

// countClosedWishes is the daemon's count of closed thread heads
// (FarmWishes.ClosedTotal), read in the slow tier with the other counts.
func (f *Farm) countClosedWishes(ctx context.Context) (int, error) {
	return f.count(ctx, beadsapi.ListBeadsQuery{Types: []string{"task"}, Labels: []string{wishLabel}, Statuses: []string{"closed"}})
}

// isWish reports whether a work row is a thread head.
func isWish(b *beadsapi.BeadDetail) bool {
	return b != nil && b.Type == "task" && hasLabel(b, wishLabel)
}

func wishSource(b *beadsapi.BeadDetail) string {
	for _, l := range b.Labels {
		if strings.HasPrefix(l, "source:") {
			return strings.TrimPrefix(l, "source:")
		}
	}
	return ""
}

func wishTitle(b *beadsapi.BeadDetail) string {
	if t := strings.TrimSpace(b.ShortTitle); t != "" {
		return t
	}
	return b.Title
}

// wishHref is the wish's thread in the Commons, below the viewer root.
func wishHref(id string) string {
	return "/garden/c/bead/" + url.PathEscape(id)
}

// wishOutcome says how a closed wish ended, from the fields the bridge stamps:
// an idle autoclose (close_reason slack-thread-idle or
// slack-thread-idle-backfill, historical: thread_task_autoclose.go stopped
// closing wishes in gd-Zhcpn9IZVl) is expired; a head marked needs-triage because
// its agent failed (triage_reason) and then closed, or whose close reason says
// failed or abandoned, is abandoned; everything else (a reaction on the card,
// kd close, the herder's fulfilled verdict, the owner's close on the herder's
// decision, an agent finishing) is done.
func wishOutcome(b *beadsapi.BeadDetail) string {
	reason := strings.ToLower(strings.TrimSpace(b.Fields["close_reason"]))
	switch {
	case strings.HasPrefix(reason, "slack-thread-idle"):
		return WishExpired
	case strings.TrimSpace(b.Fields["triage_reason"]) != "", strings.Contains(reason, "fail"), strings.Contains(reason, "abandon"):
		return WishAbandoned
	}
	return WishDone
}

// wishReason is why a closed wish closed, in the bridge's words: its
// close_reason, else the triage_reason that made it abandoned.
func wishReason(b *beadsapi.BeadDetail) string {
	if r := strings.TrimSpace(b.Fields["close_reason"]); r != "" {
		return r
	}
	return strings.TrimSpace(b.Fields["triage_reason"])
}

// herderWokeWindow is how long before a verdict a wake still counts as the
// herder acting on it: it wakes a wish's agent or asks its asker at most once
// a day (herder_woke_at), so a later pass with the same verdict still stands
// behind the wake it made.
const herderWokeWindow = 24 * 60 * 60

// WishHerder is the herder's verdict on an open head (FarmWish.HerderVerdict,
// HerderCheckedAt, HerderActed): none when the head carries no verdict the
// farm knows. The wish rail (beads-viewer threads_herder.go) reads it too.
func WishHerder(b *beadsapi.BeadDetail) (verdict string, checked int64, acted bool) {
	switch v := strings.ToLower(strings.TrimSpace(b.Fields["herder_verdict"])); v {
	case HerderFulfilled, HerderInProgress, HerderUnfulfilled, HerderUnclear:
		verdict = v
	default:
		return "", 0, false
	}
	checked = fieldUnix(b.Fields["herder_checked_at"])
	if verdict == HerderUnfulfilled || verdict == HerderUnclear {
		woke := fieldUnix(b.Fields["herder_woke_at"])
		acted = woke > 0 && woke >= checked-herderWokeWindow
	}
	return verdict, checked, acted
}

// HerderWords is a verdict in words with what the herder does about it, as
// the goat's tip says it (web/goats.js herderWords): a fulfilled wish it
// closes, an unfulfilled one's agent it wakes, an unclear one's asker it asks
// (acted: only when it did). "" for no verdict.
func HerderWords(verdict string, acted bool) string {
	switch verdict {
	case HerderFulfilled:
		return "fulfilled, closing"
	case HerderInProgress:
		return "in progress"
	case HerderUnfulfilled:
		if acted {
			return "unfulfilled, agent woken"
		}
		return "unfulfilled"
	case HerderUnclear:
		if acted {
			return "unclear, asked"
		}
		return "unclear"
	}
	return ""
}

func wishClosedAt(b *beadsapi.BeadDetail) int64 {
	if t := fieldUnix(b.ClosedAt); t > 0 {
		return t
	}
	return b.UpdatedAt.Unix()
}

// kinOf groups rows by the wish their thread_id names, newest change first.
// A row naming itself is skipped.
func kinOf(rows []*beadsapi.BeadDetail) map[string][]*beadsapi.BeadDetail {
	kin := map[string][]*beadsapi.BeadDetail{}
	for _, b := range rows {
		if b == nil {
			continue
		}
		id := strings.TrimSpace(b.Fields["thread_id"])
		if id == "" || id == b.ID {
			continue
		}
		kin[id] = append(kin[id], b)
	}
	for _, ks := range kin {
		sort.Slice(ks, func(i, j int) bool {
			if !ks[i].UpdatedAt.Equal(ks[j].UpdatedAt) {
				return ks[i].UpdatedAt.After(ks[j].UpdatedAt)
			}
			return ks[i].ID < ks[j].ID
		})
	}
	return kin
}

// wishSet is the wishes as a refresh read them, before the per-project caps:
// build caps it for the whole farm (refreshOnce) and for a project set
// (farm_cut.go), from the same read.
type wishSet struct {
	open        []openWish   // newest first
	closed      []closedWish // newest first
	closedTotal int          // the daemon's count of closed heads
}

// wishKin is one open row whose thread_id names a wish.
type wishKin struct {
	id, project string
	tending     bool      // in progress or in review
	plot        *FarmPlot // the plot it is drawn on, nil when none
	arms        *beadArms // its line arms (dressArms), nil when its line bears none
}

// openWish is an open head with every kin, uncapped.
type openWish struct {
	w         FarmWish  // the head's own fields
	kin       []wishKin // newest change first
	closedKin []string  // the project: label of each closed kin
}

// closedWish is a closed head with its uncapped produce and the projects of
// its kin.
type closedWish struct {
	w           FarmClosedWish // Kin unset, Produce uncapped
	kin         []string       // the project: label of each kin, open and closed
	produceProj []string       // the project: label of each Produce item's bead
}

// readWishSet reads the open wishes (heads) with their kin among rows (the
// open work rows), and the closed wishes among closedRows (the slow tier's
// kept closed rows). closedTotal is the daemon's count of closed heads;
// produce is each wish's kin's merged MRs (wishProduce). plotOf is the farm's
// plot resolver for the kin.
func readWishSet(heads, rows, closedRows []*beadsapi.BeadDetail, closedTotal int, produce wishYield, plotOf func(string) *FarmPlot) *wishSet {
	openKin, closedKin := kinOf(rows), kinOf(closedRows)
	ws := &wishSet{closedTotal: closedTotal}
	projects := func(ks []*beadsapi.BeadDetail) []string {
		out := make([]string, len(ks))
		for i, k := range ks {
			out[i] = projectOf(k)
		}
		return out
	}

	sort.Slice(heads, func(i, j int) bool {
		if !heads[i].CreatedAt.Equal(heads[j].CreatedAt) {
			return heads[i].CreatedAt.After(heads[j].CreatedAt)
		}
		return heads[i].ID < heads[j].ID
	})
	for _, h := range heads {
		ow := openWish{
			w: FarmWish{
				ID: h.ID, Title: wishTitle(h), Project: projectOf(h), Source: wishSource(h), Asker: lower(h.OperatedBy),
				Opened: h.CreatedAt.Unix(), Updated: h.UpdatedAt.Unix(), Href: wishHref(h.ID),
			},
			closedKin: projects(closedKin[h.ID]),
		}
		ow.w.HerderVerdict, ow.w.HerderCheckedAt, ow.w.HerderActed = WishHerder(h)
		for _, k := range openKin[h.ID] {
			st := taskState(k, mrOf(k))
			ow.kin = append(ow.kin, wishKin{id: k.ID, project: projectOf(k), tending: st == TaskInProgress || st == TaskReview, plot: plotOf(k.ID)})
		}
		ws.open = append(ws.open, ow)
	}

	var closed []*beadsapi.BeadDetail
	for _, b := range closedRows {
		if isWish(b) {
			closed = append(closed, b)
		}
	}
	sort.Slice(closed, func(i, j int) bool {
		if a, b := wishClosedAt(closed[i]), wishClosedAt(closed[j]); a != b {
			return a > b
		}
		return closed[i].ID < closed[j].ID
	})
	for _, b := range closed {
		cw := closedWish{
			w: FarmClosedWish{
				ID: b.ID, Title: wishTitle(b), Project: projectOf(b), Source: wishSource(b), Asker: lower(b.OperatedBy),
				Closed: wishClosedAt(b), Outcome: wishOutcome(b), Reason: wishReason(b), Produce: produce.byWish[b.ID], Href: wishHref(b.ID),
			},
			kin: append(projects(openKin[b.ID]), projects(closedKin[b.ID])...),
		}
		for _, p := range cw.w.Produce {
			cw.produceProj = append(cw.produceProj, produce.project[p.ID])
		}
		ws.closed = append(ws.closed, cw)
	}
	return ws
}

// dressArms gives every open wish's kin its line arms through the line-arms
// cache (line_arms.go): one batched walk for all of them, the beads read
// lately not read again. A failed read leaves kin without arms.
func (ws *wishSet) dressArms(ctx context.Context, line *lineArms, src lineSource) {
	var ids []string
	for _, ow := range ws.open {
		for _, k := range ow.kin {
			ids = append(ids, k.id)
		}
	}
	arms := line.resolve(ctx, src, ids)
	if len(arms) == 0 {
		return
	}
	for _, ow := range ws.open {
		for i := range ow.kin {
			ow.kin[i].arms = arms[ow.kin[i].id]
		}
	}
}

// build is the wishes blob: every wish when allow is nil, else the cut's
// (farm_cut.go). Fulfilled counts the admitted wishes the herder judged
// fulfilled, past the caps too. A cut lists the wishes of its projects, and inside each only
// the kin, plots and produce it admits: a kin or a produce item by its own
// project: label (none is not admitted), a plot when plotOK admits its plan.
// The counts and tending are recounted over what it admits, and a closed
// wish's reason, which can name any bead, is dropped. closedTotal is the
// closed heads' count for the farm or the set.
func (ws *wishSet) build(allow func(string) bool, plotOK func(FarmPlot) bool, closedTotal int) FarmWishes {
	admit := func(p string) bool { return allow == nil || allow(p) }
	out := FarmWishes{Open: []FarmWish{}, Closed: []FarmClosedWish{}, ClosedTotal: closedTotal}

	perProject := map[string]int{}
	for _, ow := range ws.open {
		project := ow.w.Project
		if !admit(project) {
			continue
		}
		out.OpenTotal++
		if ow.w.HerderVerdict == HerderFulfilled {
			out.Fulfilled++
		}
		perProject[project]++
		if perProject[project] > maxWishesPerProject {
			if out.OpenMore == nil {
				out.OpenMore = map[string]int{}
			}
			out.OpenMore[project]++
			continue
		}
		w := ow.w
		for _, p := range ow.closedKin {
			if admit(p) {
				w.KinClosed++
			}
		}
		seen := map[FarmPlot]bool{}
		for _, k := range ow.kin {
			if !admit(k.project) {
				continue
			}
			if w.KinTotal < maxWishKin {
				w.Kin = append(w.Kin, k.id)
			}
			w.KinTotal++
			if k.tending {
				w.Tending = true
			}
			if p := k.plot; p != nil && !seen[*p] && (plotOK == nil || plotOK(*p)) {
				seen[*p] = true
				w.Plots = append(w.Plots, *p)
			}
			if a := k.arms; a != nil && len(w.Arms) < maxWishArms && (len(w.Arms) == 0 || w.Arms[0].Canonical != a.Canonical) {
				w.Arms = append(w.Arms, *a)
			}
		}
		out.Open = append(out.Open, w)
	}

	perProject = map[string]int{}
	for _, cw := range ws.closed {
		project := cw.w.Project
		if !admit(project) {
			continue
		}
		perProject[project]++
		if perProject[project] > maxClosedWishesPerProject {
			continue
		}
		w := cw.w
		for _, p := range cw.kin {
			if admit(p) {
				w.Kin++
			}
		}
		if allow != nil {
			w.Reason = ""
			var produce []FarmProduce
			for i, p := range w.Produce {
				if admit(cw.produceProj[i]) {
					produce = append(produce, p)
				}
			}
			w.Produce = produce
		}
		if len(w.Produce) > maxWishProduce {
			w.ProduceMore = len(w.Produce) - maxWishProduce
			w.Produce = w.Produce[:maxWishProduce]
		}
		out.Closed = append(out.Closed, w)
	}
	return out
}
