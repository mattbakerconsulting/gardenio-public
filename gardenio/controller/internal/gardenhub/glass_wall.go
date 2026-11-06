package gardenhub

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"sync"
	"time"

	"gardenio/controller/internal/beadsapi"
	"gardenio/controller/internal/coopmuxapi"
	"gardenio/controller/internal/glass"
)

// The Wall (Lands workbench-glass spec GL-5, §3.4 #2, §3.5; step 6.7, gd-Pjha29mbgN): the land's live sessions at once, as
// text-grid tiles. GET /garden/glass/wall answers the Workbench's fetch with the viewer's agents as JSON (the first paint)
// and an EventSource with the live feed as SSE: an "agents" event naming the tiles (who each is, from its bead), then the
// internal/glass Wall's events (tiles, screens, state, online, offline, ended) for exactly those agents.
//
// Which agents is decided here, the same way every Glass route decides it: the agent beads the viewer's own bearer lists
// (kbeads cuts what they may not see), kept where the bead's project is in the viewer's land scope and it names a session.
// The Wall then subscribes its one shared /ws/mux upstream to the union of every viewer's sessions and fans out each
// viewer's own; a session outside the viewer's scope never reaches them, and the browser is told agent beads, not
// sessions. The list is resolved again every 30 s, and soon after coopmux says a session nobody here knew came online, so
// a new gardener joins the Wall without a reload.
//
// ?agent=<id> narrows the feed to one agent, after glassAgent's scope and tier checks (an agent outside the viewer's lands
// is a 404): a gardener figure's card on the map shows that agent's tile at small size (web/wall.js).
//
// Watch is operator-tier on every land in v1 (spec §3.4 #5, glassWatchTier), the Wall included: a tile is a PTY too.

// GlassWallAgent is one tile's who: the agent bead and what its header says.
type GlassWallAgent struct {
	ID        string `json:"id"`
	Agent     string `json:"agent"`
	Project   string `json:"project,omitempty"`
	Role      string `json:"role,omitempty"`
	Task      string `json:"task,omitempty"`
	TaskTitle string `json:"task_title,omitempty"`
	// State is the bead's agent_state, for a tile coopmux has not described yet.
	State   string `json:"state,omitempty"`
	session string
}

// GlassWallDoc is GET /garden/glass/wall as JSON.
type GlassWallDoc struct {
	Agents []GlassWallAgent `json:"agents"`
	// Live: the Wall's feed is wired; Stream is where it is.
	Live   bool   `json:"live"`
	Stream string `json:"stream,omitempty"`
	Watch  string `json:"glass_watch"`
	// Partial: the agent list was cut at its limit.
	Partial bool `json:"partial,omitempty"`
}

const (
	glassWallAgentsMax = 500 // agent beads listed for the Wall
	glassWallTilesMax  = 120 // tiles a Wall holds
	glassWallTitleConc = 8   // task title lookups in flight
	glassWallResolve   = 30 * time.Second
	glassWallHint      = 5 * time.Second // the soonest a session coming online resolves the list again
)

// glassWall serves GET /garden/glass/wall.
func (h *Handler) glassWall(w http.ResponseWriter, r *http.Request) {
	if h.Glass == nil || h.Beads == nil {
		http.Error(w, "the glass is not enabled", http.StatusServiceUnavailable)
		return
	}
	var who coopmuxapi.Identity
	if h.Glass.Who != nil {
		who = h.Glass.Who(r)
	}
	if !glassWatchTier.Allows(who.Roles) {
		http.Error(w, "watching agents needs "+glassWatchTier.String(), http.StatusForbidden)
		return
	}
	if t := glass.RouteTier(http.MethodGet, "/ws/mux"); !t.Allows(who.Roles) {
		http.Error(w, "the wall needs "+t.String(), http.StatusForbidden)
		return
	}
	one := strings.TrimSpace(r.URL.Query().Get("agent"))
	res := &wallResolver{h: h, r: r, one: one, titles: map[string]string{}}
	if one != "" {
		// The scope and tier checks of every Glass route, answered in its words (404 for out of scope).
		if _, _, _, ok := h.glassAgentID(w, r, one); !ok {
			return
		}
	}
	agents, partial, err := res.resolve()
	if err != nil {
		http.Error(w, "the beads daemon did not answer", http.StatusBadGateway)
		return
	}
	stream := h.BasePath + "/garden/glass/wall"
	if one != "" {
		stream += "?agent=" + url.QueryEscape(one)
	}
	if !strings.Contains(r.Header.Get("Accept"), "text/event-stream") {
		doc := GlassWallDoc{Agents: agents, Live: h.Glass.Wall != nil, Watch: glassWatchPolicy, Partial: partial}
		if doc.Live {
			doc.Stream = stream
		}
		writeDoc(w, "glass", doc)
		return
	}
	if h.Glass.Wall == nil {
		http.Error(w, "the glass wall is not enabled", http.StatusServiceUnavailable)
		return
	}
	fl, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming not supported", http.StatusInternalServerError)
		return
	}
	viewer := h.Glass.Wall.Join(who, allowOf(agents))
	defer viewer.Close()

	hd := w.Header()
	hd.Set("Content-Type", "text/event-stream")
	hd.Set("Cache-Control", "no-cache")
	hd.Set("Connection", "keep-alive")
	hd.Set("X-Accel-Buffering", "no")
	fmt.Fprint(w, "retry: 2000\n: connected\n\n")
	said := agentsEvent(agents)
	fmt.Fprintf(w, "event: agents\ndata: %s\n\n", said)
	fl.Flush()

	hb := time.NewTicker(heartbeatEvery)
	defer hb.Stop()
	tick := time.NewTicker(glassWallHint)
	defer tick.Stop()
	last, pending := time.Now(), false
	again := func() {
		got, _, err := res.resolve()
		last, pending = time.Now(), false
		if err != nil {
			return // keep the tiles it has; the next resolve may answer
		}
		if b := agentsEvent(got); !bytes.Equal(b, said) {
			said = b
			fmt.Fprintf(w, "event: agents\ndata: %s\n\n", b)
			fl.Flush()
			viewer.Allow(allowOf(got))
		}
	}
	done := r.Context().Done()
	for {
		select {
		case <-done:
			return
		case <-viewer.Ready():
			evs, unknown, finished := viewer.Take()
			for _, ev := range evs {
				fmt.Fprintf(w, "event: %s\ndata: %s\n\n", ev.Kind, ev.Data)
			}
			fl.Flush()
			if finished {
				return
			}
			if unknown {
				pending = true
			}
		case <-tick.C:
			if pending || time.Since(last) >= glassWallResolve {
				again()
			}
		case <-hb.C:
			fmt.Fprint(w, "event: hb\ndata: {}\n\n")
			fl.Flush()
		}
	}
}

func agentsEvent(agents []GlassWallAgent) []byte {
	b, _ := json.Marshal(struct {
		Agents []GlassWallAgent `json:"agents"`
	}{agents})
	return b
}

// allowOf is the Wall's session -> agent bead map for agents.
func allowOf(agents []GlassWallAgent) map[string]string {
	m := make(map[string]string, len(agents))
	for _, a := range agents {
		m[a.session] = a.ID
	}
	return m
}

// wallResolver lists one viewer's Wall agents, keeping the task titles it has read so a resolve again reads only new ones.
type wallResolver struct {
	h      *Handler
	r      *http.Request
	one    string // ?agent=: that agent only
	titles map[string]string
}

// resolve is the viewer's Wall: the agent beads they may see, in their land scope, that name a session, by name.
func (x *wallResolver) resolve() ([]GlassWallAgent, bool, error) {
	ctx, cancel := context.WithTimeout(x.r.Context(), glassTimeout)
	defer cancel()
	var beads []*beadsapi.BeadDetail
	partial := false
	if x.one != "" {
		b, err := x.h.Beads.GetBead(ctx, x.one)
		if err != nil {
			var ae *beadsapi.APIError
			if errors.As(err, &ae) && (ae.StatusCode == http.StatusNotFound || ae.StatusCode == http.StatusForbidden) {
				return []GlassWallAgent{}, false, nil // it went away: no tile
			}
			return nil, false, err
		}
		beads = []*beadsapi.BeadDetail{b}
	} else {
		l, ok := x.h.Beads.(beadLister)
		if !ok {
			return []GlassWallAgent{}, false, nil
		}
		res, err := l.ListBeadsFiltered(ctx, beadsapi.ListBeadsQuery{
			Types:    []string{"agent"},
			Statuses: []string{"open", "in_progress"},
			Sort:     "-updated_at",
			Limit:    glassWallAgentsMax,
			NoTotal:  true, // only "partial" is shown (gd-I2zntgOsCq)
		})
		if err != nil {
			return nil, false, err
		}
		beads, partial = res.Beads, res.MayHaveMore()
	}
	agents := []GlassWallAgent{}
	seen := map[string]bool{}
	for _, b := range beads {
		if b == nil || b.Type != "agent" {
			continue
		}
		session := sessionOf(b)
		project := strings.TrimSpace(b.Fields["project"])
		if project == "" {
			project = projectOf(b)
		}
		if session == "" || seen[session] || x.h.Glass.InScope == nil || !x.h.Glass.InScope(x.r, project) {
			continue
		}
		seen[session] = true
		d := glassDoc(b, project)
		agents = append(agents, GlassWallAgent{ID: b.ID, Agent: d.Agent, Project: project, Role: d.Role, Task: d.Task, State: d.State, session: session})
		if len(agents) == glassWallTilesMax {
			partial = true
			break
		}
	}
	x.hydrate(ctx, agents)
	sort.Slice(agents, func(i, j int) bool { // by the name a tile shows: its agent, else its bead
		ni, nj := firstNonEmpty(agents[i].Agent, agents[i].ID), firstNonEmpty(agents[j].Agent, agents[j].ID)
		if ni != nj {
			return ni < nj
		}
		return agents[i].ID < agents[j].ID
	})
	return agents, partial, nil
}

// hydrate fills each agent's task title, on the viewer's own bearer, reading only titles it has not read before.
func (x *wallResolver) hydrate(ctx context.Context, agents []GlassWallAgent) {
	var want []string
	for _, a := range agents {
		if _, ok := x.titles[a.Task]; a.Task != "" && !ok {
			want = append(want, a.Task)
		}
	}
	var mu sync.Mutex
	var wg sync.WaitGroup
	sem := make(chan struct{}, glassWallTitleConc)
	for _, id := range want {
		wg.Add(1)
		sem <- struct{}{}
		go func() {
			defer wg.Done()
			defer func() { <-sem }()
			t, err := x.h.Beads.GetBead(ctx, id)
			if err != nil || t == nil {
				return // read again on the next resolve
			}
			mu.Lock()
			x.titles[id] = clip(t.Title, glassTitleMax)
			mu.Unlock()
		}()
	}
	wg.Wait()
	for i := range agents {
		agents[i].TaskTitle = x.titles[agents[i].Task]
	}
}
