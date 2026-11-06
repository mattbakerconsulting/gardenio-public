package commons

import (
	"context"
	"time"

	"golang.org/x/sync/errgroup"

	"gardenio/controller/internal/beadsapi"
)

// Batched channel reads (gd-OYKXtzN9kr). A land has a DM for each agent that
// has written to a person, hundreds of them holding a few messages each, and
// reading each on its own spent the list's and the counts' read budget
// (gd-WUojuuXdbc). So a land's DMs are read batchChannels at a time in one
// list (kbeads field_in on channel_id), newest first. Sorted that way, a batch
// that stops at its limit still holds every bead of its channels said after
// its oldest row; a channel whose answer lies in that span is answered from
// the batch, and the rest are read on their own as before.

const (
	// batchChannels is how many channels one batched read asks for.
	batchChannels = 50
	// batchLimit is how many beads one batched read returns.
	batchLimit = 500
)

// channelRows is one channel's beads from a batched read, newest first. When
// Complete they are every bead of the channel; otherwise they are every one
// said after Cutoff, and perhaps some said at it.
type channelRows struct {
	Beads    []*beadsapi.BeadDetail
	Complete bool
	Cutoff   time.Time
}

// batchable reports whether ch is read in a batch: a DM, which says little.
// A busy channel would fill a batch's limit and crowd the others out.
func batchable(ch Channel) bool { return ch.Kind == "dm" }

// readChannels reads each of chs once, unreadParallel reads at a time, and
// sends none once ctx is done. A batchable channel is first offered its rows
// from its batch's read to fromBatch, which reports whether they answered it;
// every other channel, and one its batch did not answer or whose batch failed,
// goes to one. Channels not batchable are read first, in chs' order.
func readChannels(ctx context.Context, rd Reader, chs []Channel, fromBatch func(i int, r channelRows) bool, one func(i int)) {
	answered := make([]bool, len(chs))
	var g errgroup.Group
	g.SetLimit(unreadParallel)
	run := func(g *errgroup.Group, f func()) {
		g.Go(func() error {
			if ctx.Err() == nil { // else the budget is spent: the rest go without
				f()
			}
			return nil
		})
	}
	for i, ch := range chs {
		if !batchable(ch) {
			answered[i] = true
			run(&g, func() { one(i) })
		}
	}
	for _, b := range batchesOf(chs) {
		run(&g, func() {
			rows, err := readBatch(ctx, rd, chs, b)
			if err != nil {
				return
			}
			for _, i := range b {
				answered[i] = fromBatch(i, rows[chs[i].ID])
			}
		})
	}
	_ = g.Wait()
	var rest errgroup.Group
	rest.SetLimit(unreadParallel)
	for i := range chs {
		if !answered[i] {
			run(&rest, func() { one(i) })
		}
	}
	_ = rest.Wait()
}

// batchesOf groups the batchable channels of chs by land, at most
// batchChannels to a group, each group's indices into chs in chs' order.
func batchesOf(chs []Channel) [][]int {
	var out [][]int
	open := map[string]int{} // land -> its batch still filling, in out
	for i, ch := range chs {
		if !batchable(ch) {
			continue
		}
		j, ok := open[ch.Land]
		if !ok || len(out[j]) == batchChannels {
			j = len(out)
			out = append(out, nil)
			open[ch.Land] = j
		}
		out[j] = append(out[j], i)
	}
	return out
}

// readBatch reads the newest batchLimit beads of the channels chs[i] for i in
// b, all of one land, and returns each channel's, by id. A row of no asked
// channel is dropped: a daemon older than field_in lists the whole land.
func readBatch(ctx context.Context, rd Reader, chs []Channel, b []int) (map[string]channelRows, error) {
	ids := make([]string, len(b))
	for k, i := range b {
		ids[k] = chs[i].ID
	}
	res, err := rd.ListBeadsFiltered(ctx, beadsapi.ListBeadsQuery{
		Labels:   []string{"project:" + chs[b[0]].Land},
		FieldsIn: map[string][]string{fieldChannelID: ids},
		Sort:     "-created_at",
		Limit:    batchLimit,
		NoTotal:  true,
	})
	if err != nil {
		return nil, readErr("batch of "+ids[0], err)
	}
	r := channelRows{Complete: len(res.Beads) < batchLimit}
	if !r.Complete {
		for _, bd := range res.Beads {
			if bd != nil {
				r.Cutoff = bd.CreatedAt // newest first: the last is the oldest
			}
		}
	}
	out := make(map[string]channelRows, len(ids))
	for _, id := range ids {
		out[id] = r
	}
	for _, bd := range res.Beads {
		if bd == nil {
			continue
		}
		id := bd.Fields[fieldChannelID]
		if cr, ok := out[id]; ok {
			cr.Beads = append(cr.Beads, bd)
			out[id] = cr
		}
	}
	return out, nil
}
