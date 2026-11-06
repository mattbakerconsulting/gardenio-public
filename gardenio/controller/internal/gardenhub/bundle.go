package gardenhub

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"time"
	"unicode/utf8"

	"gardenio/controller/internal/beadsapi"
)

// Bundles (Lands workbench-glass spec WB-9, step 5.10, gd-UFvVkR7Mwx): a bundle bead's documents read as one, the
// Workbench's bundle kind (web/bundle.js, /garden?bundle=<id>), opened from a bundle's bead face ("Read as one document")
// and every link to the viewer's retired /bundle/{id} page (R6). The reader is the viewer's own
// (cmd/beads-viewer bundle_garden.go: the doc children by the parent-child edges gb bundle add writes, the producing pull
// request by its produces edge, the bead it was filed against by its parent mention), on the request's bearer, so the
// daemon cuts the answer as it cuts the page. The documents come raw, each with its format; the panel renders every format
// itself, node by node (markdown through the report face's reader, a diff by its lines, a csv as a table, a screenshot
// through the viewer's image proxy), never server HTML.

// BundleReader reads one bundle for the request's viewer. A bead that is not there, not visible or not a bundle answers a
// *beadsapi.APIError with StatusNotFound.
type BundleReader interface {
	Bundle(ctx context.Context, id string) (*BundleDoc, error)
}

const (
	bundleTimeout    = 10 * time.Second
	bundleDocsMax    = 40      // documents sent, in their order (the bead face's table of contents lists as many)
	bundleContentMax = 200_000 // bytes of one document's content
	bundleTotalMax   = 1 << 20 // bytes of content across the documents; past it a document says it was too long to send
)

// BundleMR is the pull request that produced a bundle (its produces edge).
type BundleMR struct {
	URL    string `json:"url,omitempty"`
	Number string `json:"number,omitempty"`
	State  string `json:"state,omitempty"`
	Title  string `json:"title,omitempty"`
}

// BundleRef is a bead a bundle names.
type BundleRef struct {
	ID    string `json:"id"`
	Title string `json:"title,omitempty"`
}

// BundlePart is one document of a bundle.
type BundlePart struct {
	ID string `json:"id"`
	// Title is the document's headline, else its bead title.
	Title string `json:"title,omitempty"`
	// Format is how the content reads (gb bundle add): md, diff, csv, code, image, link, html; a report child's markdown is md.
	Format   string `json:"format,omitempty"`
	Language string `json:"language,omitempty"` // a code or diff document's language
	Content  string `json:"content,omitempty"`
	URL      string `json:"url,omitempty"` // an image's or a link's address (fields.content_url)
	// Cut says the content was longer than the panel is sent: the start is shown, and the doc's bead has the rest.
	Cut bool `json:"cut,omitempty"`
}

// BundleDoc is one bundle as the panel needs it.
type BundleDoc struct {
	ID          string `json:"id"`
	Title       string `json:"title"`
	Kind        string `json:"kind,omitempty"` // fields.kind
	Status      string `json:"status,omitempty"`
	Description string `json:"description,omitempty"`
	// Parent is the bead the bundle was filed against (its first parent mention), the panel's "For" line.
	Parent *BundleRef   `json:"parent,omitempty"`
	MR     *BundleMR    `json:"mr,omitempty"`
	Docs   []BundlePart `json:"docs"`
	// More counts the documents past bundleDocsMax, not sent.
	More int `json:"more,omitempty"`
	// Partial names what the daemon did not answer (docs), so the panel says so instead of showing none.
	Partial []string `json:"partial,omitempty"`
	// Mentions and Unseen title the bead ids the description and the markdown documents name, as the bead view's do
	// (bead.go beadDoc, bead_mentions.go bundleMentions).
	Mentions map[string]string `json:"mentions,omitempty"`
	Unseen   []string          `json:"unseen,omitempty"`
}

// bundleView serves GET /garden/bundle/{id}.
func (h *Handler) bundleView(w http.ResponseWriter, r *http.Request) {
	if h.Bundles == nil {
		http.Error(w, "bundles not enabled", http.StatusServiceUnavailable)
		return
	}
	id := strings.TrimSpace(r.PathValue("id"))
	if id == "" || len(id) > 200 {
		http.Error(w, "bad bundle id", http.StatusBadRequest)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), bundleTimeout)
	defer cancel()
	doc, err := h.Bundles.Bundle(ctx, id)
	if err == nil && doc == nil {
		err = &beadsapi.APIError{StatusCode: http.StatusNotFound, Message: "no such bundle"}
	}
	if err != nil {
		var ae *beadsapi.APIError
		switch {
		case errors.As(err, &ae) && (ae.StatusCode == http.StatusNotFound || ae.StatusCode == http.StatusForbidden):
			http.Error(w, "no such bundle", http.StatusNotFound) // one the viewer may not see is not there, as on the page
		default:
			http.Error(w, "the beads daemon did not answer", http.StatusBadGateway)
		}
		return
	}
	finishBundle(doc)
	if h.Beads != nil {
		bundleMentions(ctx, h.Beads, doc)
	}
	writeDoc(w, "kbeads", doc)
}

// finishBundle drops a repeated or idless document, caps the documents and their content, so every reader sends the same
// shape.
func finishBundle(d *BundleDoc) {
	d.Description = clipKeep(d.Description, beadDescMax)
	out, seen, total := make([]BundlePart, 0, len(d.Docs)), map[string]bool{}, 0
	for _, p := range d.Docs {
		if p.ID == "" || seen[p.ID] {
			continue
		}
		seen[p.ID] = true
		if len(out) >= bundleDocsMax {
			d.More++
			continue
		}
		room := min(bundleContentMax, max(0, bundleTotalMax-total))
		if len(p.Content) > room {
			p.Content, p.Cut = cutBytes(p.Content, room), true
		}
		total += len(p.Content)
		out = append(out, p)
	}
	d.Docs = out
}

// cutBytes is s cut to at most n bytes, at a line break when one is near the end, never inside a rune.
func cutBytes(s string, n int) string {
	if len(s) <= n {
		return s
	}
	for n > 0 && !utf8.RuneStart(s[n]) {
		n--
	}
	s = s[:n]
	if i := strings.LastIndexByte(s, '\n'); i >= n-n/8 && i > 0 {
		s = s[:i]
	}
	return s
}
