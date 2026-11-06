package main

import (
	"context"
	"html/template"
	"net/http"

	"gardenio/controller/internal/beadsapi"
	"gardenio/controller/internal/oidcauth"
)

// handleCommentPreview renders a markdown preview for the operator's in-flight
// comment draft, reusing the same goldmark + LinkifyHTML pipeline that produces
// the saved comment render (renderCommentMarkdown via linkifyComments). The
// debounced htmx POST from the comment textarea targets this endpoint so the
// preview pane matches the eventual saved render byte-for-byte. Anonymous
// viewers get 401 — the comment form is itself gated behind the same role.
func (s *Server) handleCommentPreview(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if id == "" {
		http.Error(w, "Missing bead id", http.StatusBadRequest)
		return
	}
	if oidcauth.EmailFromContext(r.Context()) == "" {
		http.Error(w, "Unauthenticated", http.StatusUnauthorized)
		return
	}
	if err := r.ParseForm(); err != nil {
		http.Error(w, "Bad form", http.StatusBadRequest)
		return
	}
	text := r.PostForm.Get("text")
	if len(text) > maxCommentLen {
		// Mirror the save-path cap rather than rendering an oversized preview;
		// the comment form's submit button surfaces the same error.
		http.Error(w, "Comment is too long.", http.StatusRequestEntityTooLarge)
		return
	}
	// Best-effort bead load so the preview resolves `!N` MR refs against the
	// bead's own project, matching the saved comment render. A load failure
	// degrades to default linkification rather than failing the preview.
	bead, err := s.daemon.GetBead(r.Context(), id)
	if err != nil {
		s.logger.Debug("getting bead for comment preview", "id", id, "error", err)
		bead = nil
	}
	s.renderCommentPreview(w, r, bead, text, http.StatusOK)
}

// renderCommentPreview writes the comment-preview partial standalone — used by
// the debounced htmx POST during comment composition. Same pipeline as
// linkifyComments so the preview matches the saved view byte-for-byte.
func (s *Server) renderCommentPreview(w http.ResponseWriter, r *http.Request, bead *beadsapi.BeadDetail, draft string, status int) {
	if status != http.StatusOK {
		w.WriteHeader(status)
	}
	s.renderPartial(w, r, "templates/partials/comment_preview.html", "comment_preview", map[string]any{
		"CommentHTML": s.renderMarkdownCommentDraftForBead(r.Context(), bead, draft),
	})
}

// renderMarkdownCommentDraftForBead is the shared markdown render path used by
// both the saved comment list (linkifyCommentsWithOpts → renderCommentMarkdownWithOpts)
// and the comment editor's preview pane. Keeping the inline-md instance in one
// wrapper guarantees the preview matches the saved view (ka-XXX linkify, code
// highlight, GFM features). The bead's project-resolved linkifier options are
// applied so a bare `!N` in a draft previews against the same repo the saved
// comment will render against — a nil bead degrades to the process-wide
// default (ka-0000007929). On empty input the rendered HTML is empty so the
// template falls back to the empty placeholder.
func (s *Server) renderMarkdownCommentDraftForBead(ctx context.Context, bead *beadsapi.BeadDetail, text string) template.HTML {
	if text == "" {
		return ""
	}
	return renderCommentMarkdownWithOpts(s.mdInline, text, s.basePath, s.linkifyOptsForBead(ctx, bead))
}
