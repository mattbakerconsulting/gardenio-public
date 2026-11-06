package commons

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"time"

	"gardenio/controller/internal/beadsapi"
)

// The stream carries content (Commons v2 step 2, gd-RvJ6t2Nagr). It used to
// name what changed and leave the pane to refetch each message by id
// (GET /garden/c/m/{id}): a round trip per message, before it could paint.
// Now an event in the channel the stream's query names (?land=&channel=)
// carries what the pane shows, rendered on the viewer's own bearer by the same
// templates as the routes (D8: the server renders, the client inserts):
//
//	message, card  html: the row as the thread shows it; head: a
//	               reply's thread head, with its reply count and time; v: the
//	               row's version on this stream, and, when only paragraphs were
//	               added to a plain message since version on, append: those
//	               paragraphs (an agent's turn as it is written), so the pane
//	               appends them in place rather than replacing the row
//	typing         line: who else is typing there, "" for nobody
//	presence       html: the channel header's presence slot
//	node           node: a bead under the root the query names (?root=, and
//	               &node= for each child the pane shows), as the workbench draws
//	               it: type, status, title, and an agent's or a PR's state
//	job            job: a job a watched agent started or finished (jobs.go),
//	               as the Jobs rail shows it; id is the agent
//
// Anything outside that channel keeps carrying ids only (unread badges and the
// channel list want no more). stale on a message or card says it could not be
// read just now: refetch the channel. A bead the viewer may not read carries no
// content, as the routes would refuse it.

// saidMax bounds the plain messages one stream remembers the text of, for
// append; a full memory is dropped wholesale.
const saidMax = 256

// streamed is one event as the stream writes it: the invalidation's ids, and
// its content when the viewer has its channel or root on screen.
type streamed struct {
	Invalidation
	HTML   string `json:"html,omitempty"`
	Head   string `json:"head,omitempty"`
	V      int    `json:"v,omitempty"`
	On     int    `json:"on,omitempty"`
	Append string `json:"append,omitempty"`
	// Line is a pointer: "" (nobody else types) is content too.
	Line  *string `json:"line,omitempty"`
	Node  *Node   `json:"node,omitempty"`
	Job   *Job    `json:"job,omitempty"`
	Stale bool    `json:"stale,omitempty"`
}

// Node is one bead of the tree under a root, as a node event carries it.
type Node struct {
	ID         string `json:"id"`
	Type       string `json:"type"`
	Status     string `json:"status"`
	Title      string `json:"title"`
	AgentState string `json:"agent_state,omitempty"`
	MRState    string `json:"mr_state,omitempty"`
	MRPipeline string `json:"mr_pipeline,omitempty"`
}

// nodeTitleMax is the rune cap on a node event's title.
const nodeTitleMax = 200

// carrier dresses one stream's events with their content.
type carrier struct {
	h  *Handler
	r  *http.Request
	sc Scope
	ch *Channel
	// said is the text and version of each plain message row last sent.
	said map[string]said
	v    int
	// agents is whether the viewer may read each agent a job event named.
	agents map[string]bool
}

type said struct {
	text string
	v    int
}

func (h *Handler) carrier(r *http.Request, sc Scope, ch *Channel) *carrier {
	return &carrier{h: h, r: r, sc: sc, ch: ch, said: map[string]said{}, agents: map[string]bool{}}
}

// dress returns inv as the stream writes it, and false for an event this
// viewer must not see at all (a node it may not read).
func (c *carrier) dress(ctx context.Context, inv Invalidation) (streamed, bool) {
	out := streamed{Invalidation: inv}
	if inv.Kind == kindNode {
		n, ok := c.node(ctx, inv.ID)
		out.Node = n
		return out, ok
	}
	if inv.Kind == kindJob {
		// the agent is read on the viewer's bearer once per stream: its jobs
		// come every few seconds
		ok, seen := c.agents[inv.ID]
		if !seen {
			_, ok = c.node(ctx, inv.ID)
			if len(c.agents) >= watchMax {
				c.agents = map[string]bool{}
			}
			c.agents[inv.ID] = ok
		}
		out.Job = inv.job
		return out, ok && inv.job != nil
	}
	if c.ch == nil || inv.Land != c.ch.Land {
		return out, true
	}
	switch inv.Kind {
	case "message", "card":
		c.message(ctx, &out)
	case "typing":
		if inv.Channel == c.ch.ID {
			line := c.h.typingOf(c.r, *c.ch, inv.Thread)
			out.Line = &line
		}
	case "presence":
		if inv.Channel == "" || inv.Channel == c.ch.ID {
			if b, err := render("presence", c.h.presenceOf(*c.ch)); err == nil {
				out.HTML = string(bytes.TrimSpace(b))
			}
		}
	}
	return out, true
}

// message renders a message or card event in the channel on screen, and a
// reply's thread head. A reply written with no channel_id names only its
// thread, so it is read to learn its channel.
func (c *carrier) message(ctx context.Context, out *streamed) {
	inv := out.Invalidation
	mine := inv.Channel == c.ch.ID
	if !mine && (inv.Channel != "" || inv.Thread == "") {
		return
	}
	ctx, cancel := context.WithTimeout(ctx, readTimeout)
	defer cancel()
	rw, err := c.h.messageRow(ctx, c.r, c.sc, inv.ID)
	if err != nil {
		out.Stale = mine && !errors.Is(err, errNotFound)
		return
	}
	if rw.M.Channel != c.ch.ID {
		return
	}
	out.Channel, out.Thread = rw.M.Channel, rw.M.Thread
	b, err := render("message", rw)
	if err != nil {
		out.Stale = true
		return
	}
	out.HTML = string(bytes.TrimSpace(b))
	out.V, out.On, out.Append = c.version(rw)
	if rw.M.Thread == "" {
		return
	}
	head, err := c.h.messageRow(ctx, c.r, c.sc, rw.M.Thread)
	if err == nil {
		b, err = render("message", head)
	}
	if err != nil {
		out.Stale = !errors.Is(err, errNotFound)
		return
	}
	out.Head = string(bytes.TrimSpace(b))
}

// version numbers the row sent for rw on this stream and, when rw is a plain
// message whose text only gained paragraphs since the version on sent, renders
// those paragraphs as the row's text renders them (its .cm-text is
// pre-wrap, so they append as they stand).
func (c *carrier) version(rw row) (v, on int, add string) {
	c.v++
	id, text := rw.M.ID, rw.M.Text
	plain := !rw.M.Deleted && !rw.HasFace() && rw.M.Notice == nil && len(rw.M.Files) == 0 && rw.M.SubjectID == "" && rw.M.Snap == nil
	was, seen := c.said[id]
	if !plain {
		delete(c.said, id)
		return c.v, 0, ""
	}
	if len(c.said) >= saidMax {
		c.said = map[string]said{}
	}
	c.said[id] = said{text: text, v: c.v}
	if !seen || was.text == "" || len(text) <= len(was.text) || !strings.HasPrefix(text, was.text) || !strings.HasPrefix(text[len(was.text):], "\n") {
		return c.v, 0, ""
	}
	return c.v, was.v, string(linkify(text[len(was.text):], rw.Base, rw.Names))
}

// node reads a bead under the stream's root on the viewer's bearer.
func (c *carrier) node(ctx context.Context, id string) (*Node, bool) {
	if c.h.Beads == nil {
		return nil, false
	}
	ctx, cancel := context.WithTimeout(ctx, lookupTimeout)
	defer cancel()
	b, err := c.h.Beads.GetBead(ctx, id)
	if err != nil || b == nil || !c.sc.Allows(landOf(b.Labels)) {
		return nil, false
	}
	return nodeOf(b), true
}

func nodeOf(b *beadsapi.BeadDetail) *Node {
	return &Node{
		ID: b.ID, Type: b.Type, Status: b.Status, Title: clipRunes(strings.TrimSpace(b.Title), nodeTitleMax),
		AgentState: b.Fields["agent_state"], MRState: b.Fields["mr_state"], MRPipeline: b.Fields["mr_pipeline_status"],
	}
}

// writeStreamed writes one dressed event, stamped as its invalidation.
func writeStreamed(w http.ResponseWriter, s streamed) {
	var b bytes.Buffer
	enc := json.NewEncoder(&b)
	enc.SetEscapeHTML(false) // an SSE data line, not HTML: the fragment's < and > go as they are
	_ = enc.Encode(s)        // strings and ints always encode
	id := ""
	if !s.At.IsZero() {
		id = s.At.UTC().Format(time.RFC3339Nano)
	}
	writeEvent(w, id, s.Kind, strings.TrimSpace(b.String()))
}

// streamWatch is the root and the nodes a stream's query names (?root=,
// &node=), bounded: node events reach it for those beads and for the beads
// that hang from them (hub.go).
func streamWatch(r *http.Request) []string {
	q := r.URL.Query()
	var ids []string
	for _, id := range append([]string{q.Get("root")}, q["node"]...) {
		if id = strings.TrimSpace(id); id != "" && len(id) <= 200 && len(ids) < watchMax {
			ids = append(ids, id)
		}
	}
	return ids
}
