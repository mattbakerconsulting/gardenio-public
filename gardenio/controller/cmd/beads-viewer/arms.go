package main

import (
	"context"
	"fmt"
	"html/template"

	"gardenio/controller/internal/badge"
	"gardenio/controller/internal/beadsapi"
	"gardenio/controller/internal/heraldry"
)

// arms.go — the bead's arms in the viewer (heraldry plan gd-N6I3Oa7uDg; spec
// gardenio/docs/superpowers/specs/2026-10-25-heraldry-design.md §5, §8, R9).
//
// The viewer can show text emoji, so the arms are their emoji run before the
// bead's title, as in Slack (step 5.5a, gd-nLVyw2i9fN, the owner's linear-form
// verdict): <span class="arms arms-40" role="img" aria-label="…">🔥🧭📦</span>.
// The arms are never drawn as a picture; a bead's badge (badge_image.go) is a
// separate, derived picture shown only where a large one belongs.

// armsSlots are the slots a surface puts arms in: 24 rows, 40 cards, 64 the
// bead page H1. Each has one font-size rule in layout.html.
var armsSlots = map[int]bool{24: true, 40: true, 64: true}

// armsBearerTypes are the bead types that may bear arms (kbeads heraldryTypes,
// R6: epics and projects).
var armsBearerTypes = map[string]bool{"epic": true, "project": true}

// bearerArms is b's arms when its type may bear them, else "": a task whose
// row holds arms (an older row, or one written before R6) draws none
// (heraldry step 3.4). Every surface reads a bead's arms through it.
func bearerArms(b *beadsapi.BeadDetail) string {
	if b == nil || !armsBearerTypes[b.Type] {
		return ""
	}
	return b.Heraldry
}

// heraldrySpan renders a bead's arms in a slot (24, 40 or 64) as the emoji
// run, read through heraldry.Parse, with the plain-words name as its
// accessible name (spec §8). It renders nothing when the bead bears no arms,
// so a surface writes {{heraldry .Heraldry 40}} unconditionally and keeps its
// own fallback (the "no arms yet" chip) for {{else}}.
func heraldrySpan(stored string, slot int) template.HTML {
	if !armsSlots[slot] {
		return ""
	}
	a, err := heraldry.Parse(stored)
	if err != nil {
		return ""
	}
	name := template.HTMLEscapeString(a.PlainName())
	return template.HTML(fmt.Sprintf(`<span class="arms arms-%d" role="img" aria-label="%s" title="%s">%s</span>`, //nolint:gosec // every interpolation is escaped
		slot, name, name, template.HTMLEscapeString(a.Run())))
}

// taskArms is the line arms of the task an agent bead is assigned (gd-ASLjQPkHJz): the agent bears none of its own
// (R6), so its page draws its task's line's run before the task's link, keyed to the task so only that link wears it.
type taskArms struct {
	TaskID   string
	Heraldry string
}

// agentTaskArms resolves agent b's task's line arms through badge.ResolveLine, the walk /garden and the Slack agent
// card read too (agent -> task -> its nearest armed ancestor); deps are b's forward edges. nil when b is no agent, has
// no task, or the line bears no arms.
func agentTaskArms(ctx context.Context, src badge.LineReader, b *beadsapi.BeadDetail, deps []beadsapi.Dependency) *taskArms {
	if b == nil || b.Type != "agent" || src == nil {
		return nil
	}
	task := badge.AgentTask(b, deps)
	if task == "" {
		return nil
	}
	a, _ := badge.ResolveLine(ctx, src, b, deps)
	if a.None() {
		return nil
	}
	return &taskArms{TaskID: task, Heraldry: a.Heraldry}
}
