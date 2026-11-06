package commons

import (
	"context"
	"html/template"
	"net/url"

	"gardenio/controller/internal/beadsapi"
	"gardenio/controller/internal/decisionview"
)

// A decision card's evidence (gd-Pi7C83262N): the screenshots on the beads the
// decision is about (its requesting task, its --evidence beads), drawn under
// its context as the shared strip (decisionview.EvidenceStrip). They are read
// on the viewer's bearer like the card, and only from beads of the card's own
// land: a card never shows another land's pictures.

// commentReader is the comment read evidence needs, which the daemon client
// has and Reader leaves out. A Beads without it shows no evidence.
type commentReader interface {
	GetComments(ctx context.Context, id string) ([]beadsapi.Comment, error)
}

// landEvidence reads the beads of one land only: a bead of any other land
// reads as not found.
type landEvidence struct {
	r    Reader
	c    commentReader
	land string
}

func (l landEvidence) GetBead(ctx context.Context, id string) (*beadsapi.BeadDetail, error) {
	b, err := l.r.GetBead(ctx, id)
	if err != nil || b == nil || landOf(b.Labels) != l.land {
		return nil, errNotFound
	}
	return b, nil
}

func (l landEvidence) GetComments(ctx context.Context, id string) ([]beadsapi.Comment, error) {
	return l.c.GetComments(ctx, id)
}

// decisionEvidence is decision b's evidence strip on a card of land, slot
// keeping its DOM ids apart from another strip of the same decision on the
// page; "" when it has none, or the reads fail.
func (h *Handler) decisionEvidence(ctx context.Context, b *beadsapi.BeadDetail, land, slot string) template.HTML {
	c, ok := h.Beads.(commentReader)
	if !ok || b == nil || land == "" {
		return ""
	}
	imgs := h.Evidence.Load(ctx, landEvidence{r: h.Beads, c: c, land: land}, b, 0)
	base := h.BasePath
	return decisionview.EvidenceStrip(b.ID, imgs, decisionview.StripOptions{
		Base: base, Slot: slot,
		Href: func(id string) string { return base + "/garden?bead=" + url.QueryEscape(id) },
	})
}
