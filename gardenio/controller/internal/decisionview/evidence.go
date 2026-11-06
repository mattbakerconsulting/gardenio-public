package decisionview

import (
	"context"
	"net/url"
	"path"
	"regexp"
	"sort"
	"strings"
	"time"

	"gardenio/controller/internal/beadsapi"
)

// Evidence is how a decision shows pictures without carrying copies of them
// (gd-JjsT1AZiny, as redirected by the operator 2026-10-29). A decision is
// almost always ABOUT another bead: the task that asked it (requesting_task,
// stamped on every `gb decision create`, auto-derived when omitted) or an
// evidence bead the asker names with `gb decision create --evidence <id>`.
// That bead is where screenshots already land: the acceptance skills upload
// with `gb image upload` and post the viewer links in a comment on the bead.
// So a surface shows the linked bead's images instead of a second upload on
// the decision, and a follow-up screenshot posted to the task after the ask
// (the decision's context is immutable) reaches the decision too.

// maxEvidenceBeads caps how many linked beads one decision reads images from.
const maxEvidenceBeads = 3

// MaxEvidenceFlag is how many beads `gb decision create --evidence` may name:
// one slot of maxEvidenceBeads is reserved for the requesting task, which is
// always read.
const MaxEvidenceFlag = maxEvidenceBeads - 1

// DefaultEvidenceImages is how many images a surface shows by default.
const DefaultEvidenceImages = 8

// EvidenceBeads is the beads a decision borrows images from, in order: the
// ids in Fields["evidence"] (comma or space separated, what --evidence
// stamps), then Fields["requesting_task"]. Duplicates and the decision's own
// id are dropped, and at most maxEvidenceBeads are returned. The requesting
// task always keeps its slot: a decision stamped with more evidence ids than
// MaxEvidenceFlag (an older or hand-written one) loses the extra evidence ids,
// never the task.
func EvidenceBeads(b *beadsapi.BeadDetail) []string {
	if b == nil {
		return nil
	}
	task := strings.TrimSpace(b.Fields["requesting_task"])
	if task == b.ID {
		task = ""
	}
	limit := maxEvidenceBeads
	if task != "" {
		limit--
	}
	var out []string
	seen := map[string]bool{b.ID: true, task: true}
	for _, id := range strings.FieldsFunc(b.Fields["evidence"], func(r rune) bool { return r == ',' || r == ' ' || r == '\n' || r == '\t' }) {
		if len(out) == limit {
			break
		}
		if seen[id] {
			continue
		}
		seen[id] = true
		out = append(out, id)
	}
	if task != "" {
		out = append(out, task)
	}
	return out
}

// Image is one picture found on a linked bead.
type Image struct {
	URL string
	// Alt is the Markdown alt text when the image was written as ![alt](url),
	// else the file name from the URL's path.
	Alt string
	// Source is the bead the image was found on, SourceTitle its title.
	Source      string
	SourceTitle string
	// At is when the comment that carries it was written; the zero time for
	// an image in the bead's description.
	At time.Time
}

var (
	mdImageRe = regexp.MustCompile(`!\[([^\]]*)\]\(\s*<?(https?://[^\s)>]+)>?(?:\s+"[^"]*")?\s*\)`)
	bareURLRe = regexp.MustCompile(`https?://[^\s<>()\[\]"'` + "`" + `]+`)
)

var imageExts = map[string]bool{".png": true, ".jpg": true, ".jpeg": true, ".gif": true, ".webp": true, ".svg": false}

// isImageURL is true for an absolute http(s) URL whose path ends in a raster
// image extension. That covers the viewer's /image/<key> links (keys keep the
// uploaded file's name), GitLab uploads and Slack-free hosts alike. SVG is
// left out: it is a document that can carry script.
func isImageURL(raw string) bool {
	u, err := url.Parse(raw)
	if err != nil || (u.Scheme != "https" && u.Scheme != "http") || u.Host == "" {
		return false
	}
	return imageExts[strings.ToLower(path.Ext(u.Path))]
}

// ImagesIn finds the image links in Markdown or plain text, in the order they
// appear, without duplicates: ![alt](url) images and bare image URLs (the
// form `gb image upload` links are pasted in).
func ImagesIn(text string) []Image {
	var out []Image
	seen := map[string]bool{}
	add := func(raw, alt string) {
		raw = strings.TrimRight(raw, ".,;:!?")
		if seen[raw] || !isImageURL(raw) {
			return
		}
		seen[raw] = true
		if alt = strings.TrimSpace(alt); alt == "" {
			u, _ := url.Parse(raw)
			alt = path.Base(u.Path)
		}
		out = append(out, Image{URL: raw, Alt: alt})
	}
	type hit struct {
		at       int
		url, alt string
	}
	var hits []hit
	for _, m := range mdImageRe.FindAllStringSubmatchIndex(text, -1) {
		hits = append(hits, hit{m[0], text[m[4]:m[5]], text[m[2]:m[3]]})
	}
	for _, m := range bareURLRe.FindAllStringIndex(text, -1) {
		hits = append(hits, hit{m[0], text[m[0]:m[1]], ""})
	}
	// A Markdown image's URL is also a bare-URL match; stable order by
	// position with the Markdown hit first keeps its alt text.
	sort.SliceStable(hits, func(i, j int) bool { return hits[i].at < hits[j].at })
	for _, h := range hits {
		add(h.url, h.alt)
	}
	return out
}

// EvidenceSource is one linked bead with the comments a caller fetched.
type EvidenceSource struct {
	Bead     *beadsapi.BeadDetail
	Comments []beadsapi.Comment
}

// EvidenceImages gathers the images of the linked beads, newest first within
// each bead (latest comment first, the description last, since the latest
// screenshots show the state being decided on), sources in order, without
// duplicates across them, and at most max (DefaultEvidenceImages when max <= 0).
func EvidenceImages(sources []EvidenceSource, max int) []Image {
	if max <= 0 {
		max = DefaultEvidenceImages
	}
	var out []Image
	seen := map[string]bool{}
	take := func(imgs []Image, source *beadsapi.BeadDetail, at time.Time) bool {
		for _, im := range imgs {
			if seen[im.URL] {
				continue
			}
			seen[im.URL] = true
			im.Source, im.SourceTitle, im.At = source.ID, source.Title, at
			out = append(out, im)
			if len(out) == max {
				return false
			}
		}
		return true
	}
	for _, s := range sources {
		if s.Bead == nil {
			continue
		}
		cs := append([]beadsapi.Comment(nil), s.Comments...)
		sort.SliceStable(cs, func(i, j int) bool { return cs[i].CreatedAt.After(cs[j].CreatedAt) })
		for _, c := range cs {
			if !take(ImagesIn(c.Text), s.Bead, c.CreatedAt) {
				return out
			}
		}
		if !take(ImagesIn(s.Bead.Description), s.Bead, time.Time{}) {
			return out
		}
	}
	return out
}

// EvidenceReader is the slice of the beads client LoadEvidence needs.
type EvidenceReader interface {
	GetBead(ctx context.Context, id string) (*beadsapi.BeadDetail, error)
	GetComments(ctx context.Context, id string) ([]beadsapi.Comment, error)
}

// LoadEvidence reads the decision's linked beads and returns their images
// (see EvidenceImages). A linked bead that cannot be read is skipped, and
// its comments are optional: a decision must render without its evidence
// rather than fail.
func LoadEvidence(ctx context.Context, r EvidenceReader, decision *beadsapi.BeadDetail, max int) []Image {
	ids := EvidenceBeads(decision)
	if r == nil || len(ids) == 0 {
		return nil
	}
	sources := make([]EvidenceSource, 0, len(ids))
	for _, id := range ids {
		b, err := r.GetBead(ctx, id)
		if err != nil || b == nil {
			continue
		}
		cs, _ := r.GetComments(ctx, id)
		sources = append(sources, EvidenceSource{Bead: b, Comments: cs})
	}
	return EvidenceImages(sources, max)
}
