package main

import (
	"context"
	"html/template"
	"sort"
	"strconv"
	"strings"
	"sync"

	"gardenio/controller/internal/beadsapi"

	"github.com/yuin/goldmark"
)

// bundleRenderCtx carries cross-renderer services consumed by the per-format
// renderers in bundle_renderers.go. Mirrors widgetRenderCtx — split out so
// renderers can reach goldmark, the syntax highlighter, and the image-host
// whitelist without depending on the *Server type. Nil-safe: each renderer
// degrades gracefully when ctx (or any field) is unset.
type bundleRenderCtx struct {
	md         goldmark.Markdown
	hl         *codeHighlighter
	basePath   string
	imageHosts map[string]struct{}
	// imageBucket and imagePrefix configure the /image/* proxy rewrite
	// (ka-0000006131). When both are set and an inline image URL points at
	// s3://<imageBucket>/<imagePrefix>/images/... the renderer rewrites it
	// to a same-origin /image/<key> path so the proxy can stream the bytes
	// behind the viewerOnly middleware. Unset → no rewrite; whitelist path
	// stays in force as defense-in-depth.
	imageBucket string
	imagePrefix string
	// imageFallbackBucket is a second bucket whose raw S3 URLs are rewritten
	// the same way: the session bucket, for images uploaded before
	// imageBucket became their own (gd-CmjVAU3vgd).
	imageFallbackBucket string
	// SourceMR is the producing-MR linkage for this bundle (ka-0000006690).
	// buildBundleView walks reverse `produces` dep edges on the bundle and
	// stamps the first reached merge-request bead here. Renderers that emit
	// MR-derived cards (diff) surface it as a "Source: !iid · state" pill
	// above the card body. Nil when no producing MR is found.
	SourceMR *beadsapi.MergeRequestBeadFields
}

// bundleView is the per-page view model for /bundle/{id}. Mirrors
// richReportView but dispatches per-doc rendering by `fields.format` instead
// of widget slug. T2 stops at the placeholder renderer; T5 wires the real
// per-format renderers via registerBundleRenderer.
type bundleView struct {
	BundleID    string
	Title       string
	Kind        string
	Description string
	Docs        []bundleDocCard
	BackLink    *bundleBackLink
}

// bundleBackLink is the "← Back to …" header link to the originating bead
// (the first parent-child forward typed mention emitted by the bundle).
type bundleBackLink struct {
	ID    string
	Title string
}

// bundleDocCard is one rendered doc in a bundle. Format is the renderer-
// registry key; RenderedHTML is filled by a registered renderer (T5+).
// Empty RenderedHTML → template falls back to the placeholder card.
type bundleDocCard struct {
	BeadID       string
	Format       string
	Headline     string
	Position     int
	Language     string
	Content      string
	ContentURL   string
	RenderedHTML template.HTML
}

// bundleDocFetcher is the subset of beadAPI buildBundleView needs. Splitting
// it out keeps the test fixture minimal and matches widgetChildFetcher.
type bundleDocFetcher interface {
	BatchGetBeads(ctx context.Context, ids []string) (map[string]*beadsapi.BeadDetail, error)
}

// bundleRenderer is the per-format renderer signature. The registry binds
// format slugs ("md", "diff", "csv", "code", "image", "link", "html") to renderers
// in bundle_renderers.go; ctx may be nil, in which case renderers degrade
// to escaped/plain output rather than panic.
type bundleRenderer func(card bundleDocCard, ctx *bundleRenderCtx) (template.HTML, error)

var (
	bundleRendererMu       sync.RWMutex
	bundleRendererRegistry = map[string]bundleRenderer{}
)

// registerBundleRenderer wires a per-format renderer. Last write wins —
// T5 will register one renderer per format slug at init time.
func registerBundleRenderer(format string, r bundleRenderer) {
	bundleRendererMu.Lock()
	defer bundleRendererMu.Unlock()
	bundleRendererRegistry[format] = r
}

// lookupBundleRenderer mirrors lookupWidget. Unknown formats fall back to
// the placeholder card in templates/bundle.html.
func lookupBundleRenderer(format string) (bundleRenderer, bool) {
	bundleRendererMu.RLock()
	defer bundleRendererMu.RUnlock()
	r, ok := bundleRendererRegistry[format]
	return r, ok
}

// buildBundleView walks parent-child graph reverse-dependencies on the
// bundle bead, picks out type=doc children, fetches each child's fields,
// and assembles the ordered doc list. Per-doc fetch errors degrade that
// card to a placeholder rather than failing the page render.
//
// Reverse deps (the `deps` table) — not `bead_references` — are the source
// of truth: `gb bundle add` writes a parent-child graph dep, and reading
// the same table here keeps new and legacy bundles displaying uniformly
// (ka-0000003477).
func buildBundleView(
	ctx context.Context,
	bundle *beadsapi.BeadDetail,
	mentions []beadsapi.BeadReferenceSummary,
	reverseDeps []beadsapi.Dependency,
	fetcher bundleDocFetcher,
	rctx *bundleRenderCtx,
) *bundleView {
	if bundle == nil {
		return nil
	}
	v := &bundleView{
		BundleID:    bundle.ID,
		Title:       bundle.Title,
		Kind:        bundle.Fields["kind"],
		Description: bundle.Description,
		BackLink:    firstBundleBackLink(mentions),
	}

	ids := dedupedParentChildIDs(reverseDeps)
	// Reverse `produces` edges on the bundle surface the originating MR for
	// the diff cards inside (ka-0000006690). Batch their beads alongside the
	// doc children so the lookup stays one BatchGetBeads call.
	mrIDs := producesEdgeBeadIDs(reverseDeps)
	fetchIDs := ids
	if len(mrIDs) > 0 {
		fetchIDs = make([]string, 0, len(ids)+len(mrIDs))
		fetchIDs = append(fetchIDs, ids...)
		fetchIDs = append(fetchIDs, mrIDs...)
	}
	children, _ := fetcher.BatchGetBeads(ctx, fetchIDs)

	if rctx != nil && rctx.SourceMR == nil {
		for _, id := range mrIDs {
			b := children[id]
			if b == nil || b.Type != "merge-request" {
				continue
			}
			rctx.SourceMR = mrFieldsFromBead(b)
			break
		}
	}

	for _, id := range ids {
		card := bundleDocCard{BeadID: id}
		child, ok := children[id]
		if !ok || child == nil {
			// Batch fetch returned nothing for this id. This is the steady
			// state after a doc bead is DELETED (e.g. a bundle re-cut that
			// replaces its docs): the parent-child reference edge can linger
			// while the target 404s. Rendering an empty "(unset) / No content"
			// placeholder for a deleted doc is worse than dropping it — it
			// litters every edited bundle with ghost cards (ka-0000007961
			// follow-up). Skip it; a genuinely transient batch failure simply
			// omits the doc this render rather than showing a permanent blank.
			continue
		}
		if child.Type != "doc" {
			continue
		}
		card.Format = child.Fields["format"]
		if h := child.Fields["headline"]; h != "" {
			card.Headline = h
		} else if child.Title != "" {
			card.Headline = child.Title
		}
		if pos := child.Fields["position"]; pos != "" {
			if n, perr := strconv.Atoi(pos); perr == nil {
				card.Position = n
			}
		}
		card.Language = child.Fields["language"]
		card.Content = child.Fields["content"]
		card.ContentURL = child.Fields["content_url"]
		if r, ok := lookupBundleRenderer(card.Format); ok {
			if html, rerr := r(card, rctx); rerr == nil {
				card.RenderedHTML = html
			}
		}
		v.Docs = append(v.Docs, card)
	}

	sort.SliceStable(v.Docs, func(i, j int) bool {
		return v.Docs[i].Position < v.Docs[j].Position
	})
	return v
}

// dedupedParentChildIDs walks reverseDeps preserving first-seen order, keeps
// only parent-child edges, and dedupes by BeadID. Used by buildBundleView /
// buildBundleSummaryView to collect the IDs they batch-fetch.
func dedupedParentChildIDs(reverseDeps []beadsapi.Dependency) []string {
	seen := map[string]bool{}
	ids := make([]string, 0, len(reverseDeps))
	for _, dep := range reverseDeps {
		if dep.Type != "parent-child" {
			continue
		}
		if seen[dep.BeadID] {
			continue
		}
		seen[dep.BeadID] = true
		ids = append(ids, dep.BeadID)
	}
	return ids
}

// producesEdgeBeadIDs walks reverseDeps preserving first-seen order, keeps
// only `produces` edges, and dedupes by BeadID. Used by buildBundleView to
// surface the originating MR for a bundle of MR-produced diff cards
// (ka-0000006690). An MR bead writes a `produces` dep to the bundle, so the
// reverse-dep view sees rows reading "BeadID(MR) produces bundle".
func producesEdgeBeadIDs(reverseDeps []beadsapi.Dependency) []string {
	seen := map[string]bool{}
	var ids []string
	for _, dep := range reverseDeps {
		if dep.Type != "produces" {
			continue
		}
		if seen[dep.BeadID] {
			continue
		}
		seen[dep.BeadID] = true
		ids = append(ids, dep.BeadID)
	}
	return ids
}

// mrFieldsFromBead projects a type=merge-request bead's `fields` map onto
// MergeRequestBeadFields. Mirrors the inverse of mrFieldsMap in
// beadsapi/client_mr.go: only the subset the bundle renderers need is
// populated. Returns nil on a nil bead.
func mrFieldsFromBead(b *beadsapi.BeadDetail) *beadsapi.MergeRequestBeadFields {
	if b == nil {
		return nil
	}
	return &beadsapi.MergeRequestBeadFields{
		MRURL:        b.Fields["mr_url"],
		MRIID:        b.Fields["mr_iid"],
		MRState:      b.Fields["mr_state"],
		MROutcome:    b.Fields["mr_outcome"],
		ProjectPath:  b.Fields["project_path"],
		SourceBranch: b.Fields["source_branch"],
		TargetBranch: b.Fields["target_branch"],
		Title:        b.Fields["mr_title"],
		Author:       b.Fields["author"],
	}
}

// isParentChildMention reports whether a bead_references.RelationshipType
// value encodes the structural "source is a child of target" relationship.
// Three forms are accepted: the legacy canonical "parent-child" (rows
// written before M0 of auto-link epic ka-0000000933 / fix ka-0000006126),
// the forward surface verb "parent" (`parent(ka-X)`), and the inverse
// surface verb "child-of" (`child-of(ka-X)`). Under surface-verb storage
// "parent" and "child-of" coexist as distinct rows but encode the same
// child→parent edge from the perspective of the source bead.
func isParentChildMention(relationshipType *string) bool {
	if relationshipType == nil {
		return false
	}
	switch *relationshipType {
	case "parent-child", "parent", "child-of":
		return true
	}
	return false
}

// firstBundleBackLink scans forward typed mentions for the first parent-child
// reference. The originating bead (the task or epic the bundle was filed
// against) is conventionally the bundle's parent. Returns nil when none is
// found.
func firstBundleBackLink(mentions []beadsapi.BeadReferenceSummary) *bundleBackLink {
	for _, m := range mentions {
		if !isParentChildMention(m.RelationshipType) {
			continue
		}
		title := strings.TrimSpace(m.Title)
		if title == "" {
			title = m.ID
		}
		return &bundleBackLink{ID: m.ID, Title: title}
	}
	return nil
}

// bundleDocEntry is one entry in a bundle's table of contents.
// Position mirrors the type=doc `position` field; ties break on bead ID
// (asc) for a stable order. Format mirrors the doc's `format` field —
// "md" / "diff" / "csv" / "code" / "image" / "link" / "html" — purely informational
// here (T7 only renders the TOC; per-format rendering is T5).
type bundleDocEntry struct {
	BeadID   string
	Title    string
	Format   string
	Position int
}

// bundleSummaryView is the per-type view model for `bundle` beads. Docs is
// the ordered TOC of type=doc children linked via parent-child mention
// edges. CTA is the relative URL of the immersive /bundle/<id> surface
// (route lands in T2; T7 just emits the link).
type bundleSummaryView struct {
	Docs   []bundleDocEntry
	CTAURL string
}

// bundleChildFetcher is the subset of beadAPI buildBundleSummaryView needs.
// Splitting it out keeps the test fixture small (mirrors widgetChildFetcher).
type bundleChildFetcher interface {
	BatchGetBeads(ctx context.Context, ids []string) (map[string]*beadsapi.BeadDetail, error)
}

// buildBundleSummaryView walks the parent-child graph reverse-dependency
// edges pointing at the bundle bead, picks out type=doc children, fetches
// each child to resolve the position/headline/format, and assembles an
// ordered TOC. Per-child fetch failures degrade that entry to a
// placeholder (title falls back to the bead ID) rather than failing the
// whole page. The CTA URL is built from basePath so a viewer mounted
// under a sub-path (e.g. /beads-viewer) still emits a correct link.
//
// Reads the same `deps` table that `gb bundle add` writes to, so legacy
// bundles (created before the writer-side typed-mention fix) still display
// their docs (ka-0000003477).
func buildBundleSummaryView(
	ctx context.Context,
	bead *beadsapi.BeadDetail,
	reverseDeps []beadsapi.Dependency,
	fetcher bundleChildFetcher,
	basePath string,
) *bundleSummaryView {
	if bead == nil {
		return nil
	}
	v := &bundleSummaryView{
		CTAURL: basePath + "/bundle/" + bead.ID,
	}

	ids := dedupedParentChildIDs(reverseDeps)
	children, _ := fetcher.BatchGetBeads(ctx, ids)

	for _, id := range ids {
		entry := bundleDocEntry{
			BeadID: id,
			Title:  id,
		}
		child, ok := children[id]
		if !ok || child == nil {
			// Batch fetch absent / failed — keep the entry as a placeholder
			// so the page still renders rather than silently dropping the doc.
			v.Docs = append(v.Docs, entry)
			continue
		}
		if child.Type != "doc" {
			continue
		}
		if h := child.Fields["headline"]; h != "" {
			entry.Title = h
		} else if child.Title != "" {
			entry.Title = child.Title
		}
		entry.Format = child.Fields["format"]
		if pos := child.Fields["position"]; pos != "" {
			if n, perr := strconv.Atoi(pos); perr == nil {
				entry.Position = n
			}
		}
		v.Docs = append(v.Docs, entry)
	}

	sort.SliceStable(v.Docs, func(i, j int) bool {
		if v.Docs[i].Position != v.Docs[j].Position {
			return v.Docs[i].Position < v.Docs[j].Position
		}
		return v.Docs[i].BeadID < v.Docs[j].BeadID
	})
	return v
}
