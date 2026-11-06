package gardenhub

import (
	"context"
	"net/http"
	"strings"
	"time"
)

// Search (Lands workbench-glass spec WB-5, step 5.7, gd-Oqh2suV8ap): every bead the viewer may see, as one Workbench kind
// (web/search.js, /garden?q=<text>), opened from Cmd-K or Ctrl-K, and from the '/' jump's last row, anywhere in /garden.
// The reader is the viewer's own search (cmd/beads-viewer search_garden.go: an exact id first, then semantic matches,
// falling back to lexical ones when the embedder is off or fails, as /search does), on the request's bearer, so the daemon
// cuts each answer as it cuts the page. A hit opens its bead panel here, never a viewer page.

// SearchReader answers one query for the request's viewer.
type SearchReader interface {
	Search(ctx context.Context, q string) (*SearchDoc, error)
}

// Search modes: how the hits were found.
const (
	SearchSemantic = "semantic"
	SearchLexical  = "lexical"
)

const (
	searchTimeout  = 10 * time.Second
	searchQueryMax = 200 // runes of a query
	searchHitsMax  = 40  // hits sent: the id hit and the page's own limit (20) with room
	searchTextMax  = 240 // runes of a hit's snippet
)

// SearchHit is one bead a query found.
type SearchHit struct {
	ID         string `json:"id"`
	Title      string `json:"title"`
	ShortTitle string `json:"short_title,omitempty"`
	Type       string `json:"type,omitempty"`
	Status     string `json:"status,omitempty"`
	Project    string `json:"project,omitempty"`
	Priority   int    `json:"priority"`
	// Plan says the bead is a plan (an epic carrying the plan label), so the panel names it so.
	Plan bool `json:"plan,omitempty"`
	// Exact says the query was this bead's id.
	Exact bool `json:"exact,omitempty"`
	// Text is the start of the description.
	Text string `json:"text,omitempty"`
	// Updated is the bead's last change (unix seconds).
	Updated int64 `json:"updated,omitempty"`
}

// SearchDoc is one query's answer as the panel needs it.
type SearchDoc struct {
	Q string `json:"q"`
	// Mode is how the hits past an exact id were found: semantic, or lexical when the embedder is off or failed (Note then
	// says so); "" for an empty query.
	Mode string      `json:"mode,omitempty"`
	Hits []SearchHit `json:"hits"`
	// Note is an honest line about what could not be read (semantic search unavailable, the word search failing).
	Note string `json:"note,omitempty"`
}

// searchView serves GET /garden/search?q=<text>.
func (h *Handler) searchView(w http.ResponseWriter, r *http.Request) {
	if h.Search == nil {
		http.Error(w, "search not enabled", http.StatusServiceUnavailable)
		return
	}
	q := searchQuery(r.URL.Query().Get("q"))
	doc := &SearchDoc{}
	if q != "" {
		ctx, cancel := context.WithTimeout(r.Context(), searchTimeout)
		defer cancel()
		var err error
		if doc, err = h.Search.Search(ctx, q); err != nil || doc == nil {
			http.Error(w, "the beads daemon did not answer", http.StatusBadGateway)
			return
		}
	}
	doc.Q = q
	finishSearch(doc)
	writeDoc(w, "kbeads", doc)
}

// searchQuery is the query as asked: whitespace collapsed, at most searchQueryMax runes.
func searchQuery(q string) string {
	q = strings.Join(strings.Fields(q), " ")
	if r := []rune(q); len(r) > searchQueryMax {
		q = strings.TrimSpace(string(r[:searchQueryMax]))
	}
	return q
}

// finishSearch drops a repeated or idless hit (the exact id one leads), clips the snippets and caps the hits, so every
// reader sends the same shape.
func finishSearch(d *SearchDoc) {
	out, seen := make([]SearchHit, 0, len(d.Hits)), map[string]bool{}
	for _, h := range d.Hits {
		if h.ID == "" || seen[h.ID] {
			continue
		}
		seen[h.ID] = true
		h.Text = clip(h.Text, searchTextMax)
		out = append(out, h)
	}
	if len(out) > searchHitsMax {
		out = out[:searchHitsMax]
	}
	d.Hits = out
}
