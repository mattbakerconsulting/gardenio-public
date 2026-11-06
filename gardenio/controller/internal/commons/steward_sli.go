package commons

import (
	"context"
	"strings"
	"time"

	"github.com/prometheus/client_golang/prometheus"

	"gardenio/controller/internal/beadsapi"
)

// The steward response SLI (the Lands SLO steward_response, gd-h5rDFoiOPh):
// gardenio_lands_steward_response_seconds{land}, one sample per human message
// in a land's #general, the time from that message to the land steward's first
// post after it in the channel or in the message's thread. A message the
// steward leaves unanswered for stewardResponseCap is observed at the cap, so
// silence burns the error budget instead of vanishing.
//
// The pending messages live in the hub's memory: a viewer restart forgets the
// ones in flight (they are never observed). beads-viewer runs one replica; a
// second would observe every message twice.

const (
	// stewardResponseCap is what an unanswered message is observed at. It
	// must be a bucket bound, as must the objective's 900.
	stewardResponseCap = 2 * time.Hour
	// stewardPendingMax bounds the unanswered messages held across lands; a
	// message past it is not tracked.
	stewardPendingMax = 4096
	// stewardSweep is how often the hub observes the messages past the cap.
	stewardSweep = 30 * time.Second
	// generalMemoCap bounds the channel id -> #general memo; a full memo is
	// dropped wholesale.
	generalMemoCap = 1024
	// channelKindGeneral is a land's #general (lands/provision, the watcher).
	channelKindGeneral = "general"
	// stewardRole is the steward's role. Its runs are agents named
	// <land>-steward-<suffix> (internal/steward bind.go), which post as
	// agent:<name>; this package does not import the steward.
	stewardRole = "steward"
)

// pendingAsk is a human #general message the steward has not answered yet.
type pendingAsk struct {
	land    string
	channel string
	thread  string // the thread it was posted in, "" at the top level
	at      time.Time
}

// stewardSLI tracks the pending messages. Only the hub's worker touches it.
type stewardSLI struct {
	hist    *prometheus.HistogramVec
	pending map[string]pendingAsk // by message id
	general map[string]bool       // channel bead id -> it is its land's #general
}

func newStewardSLI() *stewardSLI {
	return &stewardSLI{
		hist: prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Name: "gardenio_lands_steward_response_seconds",
			Help: "Seconds from a human (oidc:) message in a tenant land's #general to the land steward's first post after it in that channel or its thread, one sample per message, by land. A message unanswered for 2h is observed at 7200. The steward_response SLI of the Lands SLOs (threshold 900s).",
			// The landsSLO steward_response threshold (900) and the cap (7200)
			// must stay bounds.
			Buckets: []float64{30, 60, 120, 300, 600, 900, 1800, 3600, 7200},
		}, []string{"land"}),
		pending: map[string]pendingAsk{},
		general: map[string]bool{},
	}
}

// message takes a message bead of land that was just created. A human's in
// #general starts waiting; the land steward's answers every waiting message
// of the land posted before it, at the top level of the same channel, or in
// the waiting message's thread. now stands in for a bead with no created_at.
func (s *stewardSLI) message(ctx context.Context, rd BeadGetter, land string, b *beadsapi.BeadDetail, now time.Time) {
	if b == nil || b.Type != typeMessage || b.Fields[fieldKind] == kindCard {
		return
	}
	at := b.CreatedAt
	if at.IsZero() {
		at = now
	}
	author := firstNonEmpty(b.Fields[fieldAuthor], b.CreatedBy)
	channel := strings.TrimSpace(b.Fields[fieldChannelID])
	thread := strings.TrimSpace(b.Fields[fieldThreadID])
	switch {
	case strings.HasPrefix(author, "oidc:"):
		if channel == "" {
			channel = s.rootChannel(ctx, rd, thread)
		}
		if len(s.pending) >= stewardPendingMax || !s.isGeneral(ctx, rd, land, channel) {
			return
		}
		s.pending[b.ID] = pendingAsk{land: land, channel: channel, thread: thread, at: at}
	case strings.HasPrefix(author, "agent:"+land+"-"+stewardRole+"-"):
		for id, p := range s.pending {
			if p.land != land || at.Before(p.at) {
				continue
			}
			if (thread == "" && channel == p.channel) || (thread != "" && (thread == id || thread == p.thread)) {
				s.hist.WithLabelValues(land).Observe(at.Sub(p.at).Seconds())
				delete(s.pending, id)
			}
		}
	}
}

// expire observes every message waiting stewardResponseCap or longer at the
// cap and stops waiting on it.
func (s *stewardSLI) expire(now time.Time) {
	for id, p := range s.pending {
		if now.Sub(p.at) >= stewardResponseCap {
			s.hist.WithLabelValues(p.land).Observe(stewardResponseCap.Seconds())
			delete(s.pending, id)
		}
	}
}

// isGeneral reports whether channel is land's #general, read from the daemon
// and memoized. A failed read is not memoized.
func (s *stewardSLI) isGeneral(ctx context.Context, rd BeadGetter, land, channel string) bool {
	if channel == "" {
		return false
	}
	if g, ok := s.general[channel]; ok {
		return g
	}
	ctx, cancel := context.WithTimeout(beadsapi.WithSystemRead(ctx), lookupTimeout)
	defer cancel()
	b, err := rd.GetBead(ctx, channel)
	if err != nil || b == nil {
		return false
	}
	g := b.Type == typeChannel && b.Fields[fieldChanKind] == channelKindGeneral && landOf(b.Labels) == land
	if len(s.general) >= generalMemoCap {
		s.general = map[string]bool{}
	}
	s.general[channel] = g
	return g
}

// rootChannel is the channel of a thread's head, for a reply forwarded before
// its channel_id was stamped; "" when it cannot be read.
func (s *stewardSLI) rootChannel(ctx context.Context, rd BeadGetter, thread string) string {
	if thread == "" {
		return ""
	}
	ctx, cancel := context.WithTimeout(beadsapi.WithSystemRead(ctx), lookupTimeout)
	defer cancel()
	b, err := rd.GetBead(ctx, thread)
	if err != nil || b == nil {
		return ""
	}
	return strings.TrimSpace(b.Fields[fieldChannelID])
}
