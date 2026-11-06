package gardenhub

import (
	"context"
	"sync"
	"time"

	"gardenio/controller/internal/badge"
	"gardenio/controller/internal/beadsapi"
)

// A bead's line arms (gd-PUTYxm5BtX, the owner's ask in Commons thread gd-Rf8rJQhkRE): wherever /garden shows a bead's
// title, the arms of its epic sit before it, so a growing task in a Growing field says which plan it belongs to at a glance. Only
// epics, plans and projects bear arms (heraldry spec R6); a task never bears its own, it shows its line's. The line's arms
// are the nearest armed ancestor's along every parent-child edge, an epic winning at the same distance: badge.ResolveArms,
// the rule `gb badge` already draws a task's badge by (gd-NgFx1XzPAL), so a bead reads the same in both places.
//
// The ledger's rows are shared by every viewer and rebuilt on every hub refresh, so the hub resolves them in batches (one
// BatchGetBeads and one BatchGetDependencies per level of the walk, for the beads it has not read lately) through a
// cache, never a read per bead per refresh.

// lineSource is the slice of the daemon client the line walk reads. The viewer's daemon client has both; a hub source
// without them (an older test fake) simply shows no line arms.
type lineSource interface {
	BatchGetBeads(ctx context.Context, ids []string) (map[string]*beadsapi.BeadDetail, error)
	BatchGetDependencies(ctx context.Context, ids []string, direction string) (map[string][]beadsapi.Dependency, error)
}

const (
	// lineArmsTTL is how long a bead's arms and parents are trusted before they are read again. Arms change rarely, and
	// a changed one shows within this.
	lineArmsTTL = 2 * time.Minute
	// lineArmsLevels bounds the batched walk: task, phase, plan, project and a thread bead or two above.
	lineArmsLevels = 6
	// lineArmsMax bounds the cache; past it, expired entries are dropped, and then everything.
	lineArmsMax = 8192
)

type lineEntry struct {
	node badge.LineNode
	at   time.Time
}

// lineArms caches the line nodes the walk read. Safe for concurrent use.
type lineArms struct {
	mu    sync.Mutex
	nodes map[string]lineEntry
	now   func() time.Time
}

func newLineArms() *lineArms { return &lineArms{nodes: map[string]lineEntry{}, now: time.Now} }

// resolve returns the line arms for each of ids whose line bears any: the bead's own when it is an armed epic, else its
// nearest armed ancestor's, as beadArms. A read that fails leaves the beads it would have answered without arms (a
// title is never held back for its picture) and is tried again on the next call.
func (c *lineArms) resolve(ctx context.Context, src lineSource, ids []string) map[string]*beadArms {
	if c == nil || src == nil || len(ids) == 0 {
		return nil
	}
	now := c.now()
	c.mu.Lock()
	known := make(map[string]badge.LineNode, len(ids)*2)
	for id, e := range c.nodes {
		if now.Sub(e.at) < lineArmsTTL {
			known[id] = e.node
		}
	}
	c.mu.Unlock()

	// Read the line level by level, only past beads that bear no arms: the walk stops where the arms are.
	frontier, seen := ids, map[string]bool{}
	fresh := map[string]badge.LineNode{}
	for level := 0; level < lineArmsLevels && len(frontier) > 0; level++ {
		var missing []string
		for _, id := range frontier {
			if _, ok := known[id]; !ok {
				missing = append(missing, id)
			}
		}
		if len(missing) > 0 {
			beads, err := src.BatchGetBeads(ctx, missing)
			if err != nil {
				break
			}
			deps, err := src.BatchGetDependencies(ctx, missing, "forward")
			if err != nil {
				break
			}
			for _, id := range missing {
				n := badge.LineNode{ID: id} // gone or out of sight: an end of the line, remembered so it is not asked again
				if b := beads[id]; b != nil {
					n = badge.Node(b, deps[id]) // an agent walks on to its task (gd-ASLjQPkHJz)
				}
				known[id], fresh[id] = n, n
			}
		}
		var next []string
		for _, id := range frontier {
			n := known[id]
			if seen[id] || n.Heraldry != "" {
				continue
			}
			seen[id] = true
			for _, p := range n.Parents {
				if !seen[p] {
					next = append(next, p)
				}
			}
		}
		frontier = next
	}

	if len(fresh) > 0 {
		c.mu.Lock()
		if len(c.nodes)+len(fresh) > lineArmsMax {
			for id, e := range c.nodes {
				if now.Sub(e.at) >= lineArmsTTL {
					delete(c.nodes, id)
				}
			}
			if len(c.nodes)+len(fresh) > lineArmsMax {
				c.nodes = map[string]lineEntry{}
			}
		}
		for id, n := range fresh {
			c.nodes[id] = lineEntry{node: n, at: now}
		}
		c.mu.Unlock()
	}

	lookup := func(_ context.Context, id string) (badge.LineNode, error) {
		if n, ok := known[id]; ok {
			return n, nil
		}
		return badge.LineNode{ID: id}, nil // past the walk's reach: no arms from here
	}
	out := make(map[string]*beadArms, len(ids))
	for _, id := range ids {
		a, err := badge.ResolveArms(ctx, id, lookup)
		if err != nil || a.None() {
			continue
		}
		if ba := armsOf(a.Heraldry); ba != nil {
			out[id] = ba
		}
	}
	return out
}

// beadLineArms is the line arms of one bead for the bead view: b itself is read already and fwd holds its forward edges.
// The walk reads the daemon with the viewer's own bearer (the bead view's src), one ancestor at a time
// (badge.ResolveLine), so an epic the viewer may not see lends no arms. It returns the arms and the bead that bears
// them, nil when the line bears none.
func beadLineArms(ctx context.Context, src BeadReader, b *beadsapi.BeadDetail, fwd []beadsapi.Dependency) (*beadArms, *beadRel) {
	a, read := badge.ResolveLine(ctx, src, b, fwd)
	if a.None() {
		return nil, nil
	}
	ba := armsOf(a.Heraldry)
	if ba == nil {
		return nil, nil
	}
	from := &beadRel{ID: a.From}
	if p := read[a.From]; p != nil {
		from.Title, from.Type, from.Status, from.Project = p.Title, p.Type, p.Status, projectOf(p)
	}
	return ba, from
}
