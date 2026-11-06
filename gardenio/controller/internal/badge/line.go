package badge

import (
	"context"
	"strings"

	"gardenio/controller/internal/beadsapi"
	"gardenio/controller/internal/heraldry"
)

// line.go reads a bead's line from the daemon for the surfaces that draw its
// line arms: /garden (gardenhub line_arms.go), the beads-viewer agent page and
// the Slack agent card. They all read the line through Node, so a bead's line
// ends in the same arms everywhere, and resolve it through ResolveArms.

// LineReader is the slice of the daemon client ResolveLine reads, one bead at
// a time.
type LineReader interface {
	GetBead(ctx context.Context, id string) (*beadsapi.BeadDetail, error)
	GetDependencies(ctx context.Context, id string) ([]beadsapi.Dependency, error)
}

// bearerTypes are the bead types that may bear arms (kbeads heraldryTypes,
// heraldry spec R6).
var bearerTypes = map[string]bool{"epic": true, "project": true}

// Node is bead b as the line walk reads it, deps its forward edges. It
// carries b's arms only when its type may bear them and they parse, so a task
// that stored arms before R6 never shadows its epic's (heraldry step 3.4).
// Its parents are its parent-child parents in edge order; an agent bead's is
// the task it is assigned (fields.task_id, its `assigned` edge), since an
// agent works a task rather than sitting under one and bears no arms of its
// own: it shows its task's line (gd-ASLjQPkHJz).
func Node(b *beadsapi.BeadDetail, deps []beadsapi.Dependency) LineNode {
	n := LineNode{ID: b.ID, Type: b.Type}
	if h := strings.TrimSpace(b.Heraldry); h != "" && bearerTypes[b.Type] {
		if _, err := heraldry.Parse(h); err == nil {
			n.Heraldry = h
		}
	}
	if b.Type == "agent" {
		if task := AgentTask(b, deps); task != "" {
			n.Parents = append(n.Parents, task)
		}
	}
	for _, d := range deps {
		if d.Type == "parent-child" && d.BeadID == b.ID && strings.TrimSpace(d.DependsOnID) != "" {
			n.Parents = append(n.Parents, d.DependsOnID)
		}
	}
	return n
}

// AgentTask is the task agent bead b is assigned: its task_id field, else the
// target of its first `assigned` edge among deps; "" when it has neither.
func AgentTask(b *beadsapi.BeadDetail, deps []beadsapi.Dependency) string {
	if t := strings.TrimSpace(b.Fields["task_id"]); t != "" {
		return t
	}
	for _, d := range deps {
		if d.Type == "assigned" && d.BeadID == b.ID {
			if t := strings.TrimSpace(d.DependsOnID); t != "" {
				return t
			}
		}
	}
	return ""
}

// ResolveLine is the line arms of b, which is read already, fwd its forward
// edges: its own, else its nearest armed ancestor's (ResolveArms), with the
// beads the walk read by id, so a caller can name the one that bears them.
// Each ancestor costs a GetBead, and a GetDependencies when it bears no arms.
// An ancestor src cannot read ends that branch, not the walk, so an epic the
// reader may not see lends no arms.
func ResolveLine(ctx context.Context, src LineReader, b *beadsapi.BeadDetail, fwd []beadsapi.Dependency) (Arms, map[string]*beadsapi.BeadDetail) {
	read := map[string]*beadsapi.BeadDetail{b.ID: b}
	lookup := func(ctx context.Context, id string) (LineNode, error) {
		if id == b.ID {
			return Node(b, fwd), nil
		}
		p, err := src.GetBead(ctx, id)
		if err != nil || p == nil {
			return LineNode{ID: id}, nil
		}
		read[id] = p
		n := Node(p, nil)
		if n.Heraldry == "" { // only an unarmed ancestor's own parents are needed
			if deps, err := src.GetDependencies(ctx, id); err == nil {
				n = Node(p, deps)
			}
		}
		return n, nil
	}
	a, err := ResolveArms(ctx, b.ID, lookup)
	if err != nil {
		return Arms{}, read
	}
	return a, read
}
