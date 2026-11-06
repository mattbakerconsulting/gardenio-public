package main

// threads_herder.go — the herder's verdict on a wish, as a tag on its /threads
// row and in its rail on /threads/{id} (gd-RkkGXAz643). The scheduled herder
// (role herder, trigger gd-AHklx9rH70) stamps herder_verdict and
// herder_checked_at on each open head it judges; the Homestead's goat says the
// same verdict in the same words (gardenhub.WishHerder, HerderWords), so the
// owner sees in either place what the herder is about to close, whose agent
// it woke and whom it asked.

import (
	"time"

	"gardenio/controller/internal/beadsapi"
	"gardenio/controller/internal/gardenhub"
)

// threadHerder is the herder's last verdict on an open wish head. Zero-valued
// (Verdict == "") before the herder's first pass on it, or when the head is
// closed: a closed wish's own "fulfilled" chip says how it ended.
type threadHerder struct {
	// Verdict is one of the gardenhub.Herder* verdicts.
	Verdict string
	// CheckedAt is when the herder gave it; zero when the head carries no stamp.
	CheckedAt time.Time
	// Acted is set on an unfulfilled or unclear verdict when the herder woke
	// the thread's agent or asked the asker for it (herder_woke_at).
	Acted bool
}

// threadHerderOf reads the herder's verdict off a wish head.
func threadHerderOf(head *beadsapi.BeadDetail) threadHerder {
	if head == nil || head.Status == "closed" {
		return threadHerder{}
	}
	v, checked, acted := gardenhub.WishHerder(head)
	h := threadHerder{Verdict: v, Acted: acted}
	if checked > 0 {
		h.CheckedAt = time.Unix(checked, 0).UTC()
	}
	return h
}

// Words is the verdict and what the herder does about it: "fulfilled,
// closing", "unfulfilled, agent woken", "unclear, asked", "in progress".
func (h threadHerder) Words() string { return gardenhub.HerderWords(h.Verdict, h.Acted) }

// Tone is the tag's colour class: done for a fulfilled wish, ask for one the
// herder is acting on (unfulfilled, unclear), plain while it is in progress.
func (h threadHerder) Tone() string {
	switch h.Verdict {
	case gardenhub.HerderFulfilled:
		return "done"
	case gardenhub.HerderUnfulfilled, gardenhub.HerderUnclear:
		return "ask"
	}
	return "plain"
}

// Has reports whether there is a verdict to show.
func (h threadHerder) Has() bool { return h.Words() != "" }
