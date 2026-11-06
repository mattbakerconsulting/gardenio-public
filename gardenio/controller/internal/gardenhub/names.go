package gardenhub

import (
	"context"
	"net/http"
	"strings"
)

// Bead names (gd-QTukIA8Nvv): GET /garden/names?id=a&id=b answers {"names": {id: name}} for the ids the daemon returns to
// the viewer, each named by its short title, else its title. The page asks for the ids it shows that nothing it loaded
// titled (web/titles.js resolve: a thread's text past a fragment's name cap, a wrap-up's beads on the workbench page), so
// a bead reads as its short title and not as a bare id. At most namesMax ids a call, the rest dropped; an id the daemon
// does not return (gone, or not this viewer's to see) is absent, and the page keeps its id and does not ask again.
const namesMax = 40

type namesDoc struct {
	Names map[string]string `json:"names"`
}

func (h *Handler) namesView(w http.ResponseWriter, r *http.Request) {
	if h.Beads == nil {
		http.Error(w, "bead view not enabled", http.StatusServiceUnavailable)
		return
	}
	var rels []*beadRel
	seen := map[string]bool{}
	for _, id := range r.URL.Query()["id"] {
		id = strings.TrimSpace(id)
		if id == "" || seen[id] || !beadIDOK(id) {
			continue
		}
		seen[id] = true
		rels = append(rels, &beadRel{ID: id})
		if len(rels) >= namesMax {
			break
		}
	}
	ctx, cancel := context.WithTimeout(r.Context(), beadViewTimeout)
	defer cancel()
	hydrate(ctx, h.Beads, rels)
	doc := namesDoc{Names: map[string]string{}}
	for _, rel := range rels {
		if name := firstNonEmpty(rel.ShortTitle, strings.TrimSpace(rel.Title)); name != "" {
			doc.Names[rel.ID] = name
		}
	}
	writeDoc(w, "kbeads", doc)
}

// beadIDOK says id has a bead id's shape (mentionID's rule: a prefix, a dash and a run of letters or digits), so the
// endpoint reads nothing a page could not have shown as an id.
func beadIDOK(id string) bool {
	return len(id) <= 200 && mentionID.FindString(id) == id
}
