// Package bridge — MR-URL autolink from bead comments.
//
// MRCommentLinker watches the kbeads SSE event stream for
// beads.comment.added events and, when a new comment contains a GitLab MR
// URL (e.g. an agent posting "MR opened: https://.../-/merge_requests/916"),
// back-fills the parent bead's mr_url field — the same effect as
// `gb mr link <bead> <url>`.
//
// Setting mr_url is the source action: the existing GitLabSync.handleUpdated
// handler (on beads.bead.updated) then fetches the MR from GitLab and upserts
// the type=merge-request bead plus the produces / triggered-by edges. This
// closes the gap where an MR URL that only ever lived in a free-text bead
// comment never reached mr_url, so no MR bead was ever projected (task
// ka-0000003369).
package bridge

import (
	"context"
	"encoding/json"
	"log/slog"
	"regexp"
	"sync"

	"gardenio/controller/internal/beadsapi"
)

// commentMRURLPattern matches a GitLab MR URL anywhere inside a comment body.
// Unlike bridge.mrURLPattern (anchored with ^, used by ParseMRURL for strict
// validation of a whole string), this one is unanchored so it can pull a URL
// out of surrounding prose like "MR opened: <url> — please review". Trailing
// punctuation and common wrapping characters are excluded from the match so
// the captured URL is clean and canonical (ends at the MR IID).
var commentMRURLPattern = regexp.MustCompile(`https?://[^\s<>"'` + "`" + `)\]}]+/-/merge_requests/\d+`)

// MRCommentLinkerDaemon is the subset of the beads daemon client used by the
// linker: read the parent bead, and back-fill its mr_url field. It is a
// strict subset of *beadsapi.Client (satisfied directly), kept small so unit
// tests can supply a fake.
type MRCommentLinkerDaemon interface {
	GetBead(ctx context.Context, beadID string) (*beadsapi.BeadDetail, error)
	UpdateBeadFields(ctx context.Context, beadID string, fields map[string]string) error
}

// MRCommentLinker back-fills mr_url from MR URLs found in new bead comments.
type MRCommentLinker struct {
	daemon MRCommentLinkerDaemon
	logger *slog.Logger

	mu   sync.Mutex
	seen map[int64]bool // dedup on SSE reconnect (keyed by comment ID)
}

// MRCommentLinkerConfig holds configuration for the linker.
type MRCommentLinkerConfig struct {
	Daemon MRCommentLinkerDaemon
	Logger *slog.Logger
}

// NewMRCommentLinker creates a new MR-URL comment autolinker.
func NewMRCommentLinker(cfg MRCommentLinkerConfig) *MRCommentLinker {
	return &MRCommentLinker{
		daemon: cfg.Daemon,
		logger: cfg.Logger,
		seen:   make(map[int64]bool),
	}
}

// RegisterHandlers registers the SSE handler for comment events.
func (l *MRCommentLinker) RegisterHandlers(stream *SSEStream) {
	stream.On("beads.comment.added", l.handleCommentAdded)
	l.logger.Info("mr-comment-linker registered SSE handlers",
		"topics", []string{"beads.comment.added"})
}

// mrCommentPayload mirrors the kbeads CommentAdded event structure (only the
// fields the linker needs).
type mrCommentPayload struct {
	Comment *mrCommentBody `json:"comment"`
}

type mrCommentBody struct {
	ID     int64  `json:"id"`
	BeadID string `json:"bead_id"`
	Author string `json:"author"`
	Text   string `json:"text"`
}

// nonWorkBeadTypes are bead types that should never carry an mr_url — the
// field belongs on the work bead (task/bug/feature/epic) that produced the
// MR, not on the MR bead itself nor on infra beads.
var nonWorkBeadTypes = map[string]bool{
	"merge-request": true,
	"agent":         true,
	"schedule":      true,
	"trigger":       true,
}

func (l *MRCommentLinker) handleCommentAdded(ctx context.Context, data []byte) {
	var payload mrCommentPayload
	if err := json.Unmarshal(data, &payload); err != nil {
		l.logger.Debug("mr-comment-linker: failed to parse comment event", "error", err)
		return
	}
	if payload.Comment == nil || payload.Comment.BeadID == "" {
		return
	}
	c := payload.Comment

	// Find an MR URL in the comment body. Bail cheaply when there is none —
	// the vast majority of comments don't contain one.
	mrURL := commentMRURLPattern.FindString(c.Text)
	if mrURL == "" {
		return
	}
	// Validate / canonicalize via the strict parser shared with gb mr link.
	if ParseMRURL(mrURL) == nil {
		l.logger.Debug("mr-comment-linker: matched URL failed strict parse, skipping",
			"bead_id", c.BeadID, "matched", mrURL)
		return
	}

	// Dedup on SSE reconnect: a redelivered comment must not re-trigger work.
	if l.alreadySeen(c.ID) {
		return
	}

	bead, err := l.daemon.GetBead(ctx, c.BeadID)
	if err != nil {
		l.logger.Warn("mr-comment-linker: failed to get parent bead",
			"bead_id", c.BeadID, "comment_id", c.ID, "error", err)
		return
	}
	if bead == nil {
		return
	}

	// Never link an MR bead to itself, and don't touch agent/schedule beads —
	// mr_url belongs on the work bead that produced the MR.
	if nonWorkBeadTypes[bead.Type] {
		l.logger.Debug("mr-comment-linker: skipping non-work bead type",
			"bead_id", c.BeadID, "type", bead.Type)
		return
	}

	// Idempotent: if mr_url is already set, do nothing. This also avoids
	// clobbering a deliberate earlier link with a different MR mentioned in
	// passing in a later comment.
	if existing := bead.Fields["mr_url"]; existing != "" {
		l.logger.Debug("mr-comment-linker: bead already has mr_url, skipping",
			"bead_id", c.BeadID, "existing", existing, "found", mrURL)
		return
	}

	if err := l.daemon.UpdateBeadFields(ctx, c.BeadID, map[string]string{"mr_url": mrURL}); err != nil {
		l.logger.Warn("mr-comment-linker: failed to set mr_url",
			"bead_id", c.BeadID, "mr_url", mrURL, "error", err)
		return
	}

	l.logger.Info("mr-comment-linker: back-filled mr_url from comment",
		"bead_id", c.BeadID, "comment_id", c.ID, "author", c.Author, "mr_url", mrURL)
}

// alreadySeen returns true if this comment has already been processed.
func (l *MRCommentLinker) alreadySeen(commentID int64) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.seen[commentID] {
		return true
	}
	l.seen[commentID] = true
	return false
}
