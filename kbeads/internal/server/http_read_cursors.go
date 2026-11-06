package server

import (
	"context"
	"fmt"
	"log/slog"
	"maps"
	"net/http"
	"regexp"
	"sync"
	"time"

	"gardenio/kbeads/internal/events"
	"gardenio/kbeads/internal/model"
)

// Read cursors (Lands Commons spec gardenio/docs/superpowers/specs/
// 2026-10-19-lands-commons-design.md §2.6, Commons III step 5).
//
// A person's read position in each Commons channel is one field on their own
// type:user bead, read_cursors: a map of channel bead id to the id of the
// last message they saw there. The Commons (beads-viewer) reports a position
// every time a person looks at a channel, so the write is debounced here: at
// most one write of the field per readCursorWriteInterval per user bead. A
// position reported inside the interval waits in memory and is written when
// the interval ends; a newer position for the same channel replaces it. GET
// answers the stored map with the waiting positions laid over it, so a reader
// never sees its own position go backwards. A daemon restart drops positions
// still waiting, at most one interval's worth.
//
// Like POST /v1/users/me/terms, the routes take the person's own OIDC token
// and touch only the bead the UserProvisioner resolved for its subject; the
// request names no bead. Whether a position moves forward is the Commons'
// call (it knows when each message was said); this only stores what it says.

const (
	readCursorsField = "read_cursors"
	// readCursorWriteInterval is the spec's "at most one write per 10 s per
	// user".
	readCursorWriteInterval = 10 * time.Second
	// readCursorsMax caps the channels one person holds a position in (tens
	// of channels per land, spec §2.7).
	readCursorsMax = 2000
	// readCursorsPerRequest caps the positions one POST reports.
	readCursorsPerRequest = 50
	// readCursorFlushTimeout bounds a deferred write.
	readCursorFlushTimeout = 10 * time.Second
)

// readCursorIDPattern is a bead id: a channel id key or a message id value.
var readCursorIDPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._:-]{0,127}$`)

// ReadCursorsRequest is the body for POST /v1/users/me/read-cursors: the
// positions to record, channel id -> message id.
type ReadCursorsRequest struct {
	ReadCursors map[string]string `json:"read_cursors"`
}

// ReadCursorsResponse is the caller's positions, stored and waiting.
type ReadCursorsResponse struct {
	UserBeadID  string            `json:"user_bead_id"`
	ReadCursors map[string]string `json:"read_cursors"`
	// Written is true on a POST whose positions went to the bead now, false
	// when they wait for the interval to end.
	Written bool `json:"written,omitempty"`
}

// handleGetReadCursors handles GET /v1/users/me/read-cursors.
func (s *BeadsServer) handleGetReadCursors(w http.ResponseWriter, r *http.Request) {
	bead, _, ok := s.readCursorsCaller(w, r)
	if !ok {
		return
	}
	cursors := storedReadCursors(bead)
	maps.Copy(cursors, s.readCursors.pending(bead.ID))
	writeJSON(w, http.StatusOK, &ReadCursorsResponse{UserBeadID: bead.ID, ReadCursors: cursors})
}

// handleSetReadCursors handles POST /v1/users/me/read-cursors.
//
//   - OIDC only: the person's own token, holding a gardenio role or a group.
//     403 otherwise, 404 with no user bead.
//   - Every key and value is a bead id; at most readCursorsPerRequest per
//     request and readCursorsMax per person.
//   - Answers the merged positions; the bead is written now or when the
//     user's write interval ends.
func (s *BeadsServer) handleSetReadCursors(w http.ResponseWriter, r *http.Request) {
	bead, email, ok := s.readCursorsCaller(w, r)
	if !ok {
		return
	}
	var req ReadCursorsRequest
	if !decodeJSONBodyLimit(w, r, &req, 64<<10) {
		return
	}
	if len(req.ReadCursors) == 0 || len(req.ReadCursors) > readCursorsPerRequest {
		writeError(w, http.StatusBadRequest, fmt.Sprintf("read_cursors needs 1 to %d channel ids", readCursorsPerRequest))
		return
	}
	for ch, msg := range req.ReadCursors {
		if !readCursorIDPattern.MatchString(ch) || !readCursorIDPattern.MatchString(msg) {
			writeError(w, http.StatusBadRequest, "read_cursors maps a channel bead id to a message bead id")
			return
		}
	}
	merged := storedReadCursors(bead)
	maps.Copy(merged, s.readCursors.pending(bead.ID))
	maps.Copy(merged, req.ReadCursors)
	if len(merged) > readCursorsMax {
		writeError(w, http.StatusBadRequest, fmt.Sprintf("at most %d read cursors per person", readCursorsMax))
		return
	}
	written, err := s.readCursors.put(r.Context(), bead.ID, email, req.ReadCursors)
	if err != nil {
		writeServerError(r.Context(), w, "handleSetReadCursors", err, "failed to update user bead")
		return
	}
	writeJSON(w, http.StatusOK, &ReadCursorsResponse{UserBeadID: bead.ID, ReadCursors: merged, Written: written})
}

// readCursorsCaller resolves the caller's own user bead, answering the
// refusal itself when there is none to use.
func (s *BeadsServer) readCursorsCaller(w http.ResponseWriter, r *http.Request) (*model.Bead, string, bool) {
	id := AuthIdentityFromContext(r.Context())
	if authMethodFromContext(r.Context()) != authMethodOIDC || id.Type != "oidc" || id.Subject == "" {
		writeError(w, http.StatusForbidden, "read cursors need the person's own OIDC token")
		return nil, "", false
	}
	// An applicant (a realm sign-in with no gardenio role and no group, Track
	// B B-D1) has no Commons to read, so no positions either: unlike terms,
	// these routes are not in applicantOpenRoutes.
	if len(RolesFromContext(r.Context())) == 0 && len(GroupsFromContext(r.Context())) == 0 {
		writeError(w, http.StatusForbidden, "read cursors need a gardenio role or a land")
		return nil, "", false
	}
	email := canonicalAliasSubject("oidc", id.Subject)
	bead, err := s.userBeadForOIDCCaller(r.Context(), email)
	if err != nil {
		writeServerError(r.Context(), w, "readCursorsCaller", err, "failed to find user bead")
		return nil, "", false
	}
	if bead == nil {
		writeError(w, http.StatusNotFound, "no user bead for "+email)
		return nil, "", false
	}
	return bead, email, true
}

// storedReadCursors decodes a user bead's read_cursors (empty when unset or
// not a map of strings).
func storedReadCursors(b *model.Bead) map[string]string {
	out := map[string]string{}
	if raw, ok := userBeadFields(b)[readCursorsField].(map[string]any); ok {
		for ch, v := range raw {
			if msg, ok := v.(string); ok {
				out[ch] = msg
			}
		}
	}
	return out
}

// writeReadCursors lays set over the bead's stored read_cursors and writes the
// field back in one merge.
func (s *BeadsServer) writeReadCursors(ctx context.Context, beadID, actor string, set map[string]string) error {
	bead, err := s.store.GetBead(ctx, beadID)
	if err != nil {
		return err
	}
	if bead == nil {
		return fmt.Errorf("user bead %s: not found", beadID)
	}
	cursors := storedReadCursors(bead)
	maps.Copy(cursors, set)
	extra := map[string]any{readCursorsField: cursors}
	updated, err := s.store.MergeBeadFields(ctx, beadID, extra)
	if err != nil {
		return err
	}
	if updated != nil {
		s.recordAndPublish(ctx, events.TopicBeadUpdated, beadID, actor, events.BeadUpdated{
			Bead:    updated,
			Changes: map[string]any{"fields": extra},
			Actor:   actor,
		})
	}
	return nil
}

// readCursorBuffer holds the positions waiting for each user bead's write
// interval to end, and does the writes.
type readCursorBuffer struct {
	interval time.Duration
	write    func(ctx context.Context, beadID, actor string, set map[string]string) error

	mu    sync.Mutex
	users map[string]*cursorUser // by user bead id
}

type cursorUser struct {
	lastWrite time.Time
	actor     string
	pending   map[string]string
	timer     *time.Timer
	writing   bool
}

func newReadCursorBuffer(interval time.Duration, write func(context.Context, string, string, map[string]string) error) *readCursorBuffer {
	return &readCursorBuffer{interval: interval, write: write, users: map[string]*cursorUser{}}
}

// pending returns a copy of the positions waiting for beadID.
func (b *readCursorBuffer) pending(beadID string) map[string]string {
	b.mu.Lock()
	defer b.mu.Unlock()
	if u := b.users[beadID]; u != nil {
		return maps.Clone(u.pending)
	}
	return nil
}

// put records set for beadID. When the user's interval has passed and no
// write is under way it writes now, on ctx, and reports written; otherwise
// the positions wait and a timer writes them when the interval ends.
func (b *readCursorBuffer) put(ctx context.Context, beadID, actor string, set map[string]string) (bool, error) {
	b.mu.Lock()
	b.prune()
	u := b.users[beadID]
	if u == nil {
		u = &cursorUser{}
		b.users[beadID] = u
	}
	if u.pending == nil {
		u.pending = map[string]string{}
	}
	maps.Copy(u.pending, set)
	u.actor = actor
	if u.timer != nil || u.writing {
		b.mu.Unlock()
		return false, nil
	}
	if wait := time.Until(u.lastWrite.Add(b.interval)); wait > 0 {
		b.schedule(beadID, u, wait)
		b.mu.Unlock()
		return false, nil
	}
	take := b.take(u)
	b.mu.Unlock()
	err := b.write(ctx, beadID, actor, take)
	b.done(beadID, take, err)
	return true, err
}

// take moves u's positions out for a write. Callers hold mu.
func (b *readCursorBuffer) take(u *cursorUser) map[string]string {
	take := u.pending
	u.pending, u.writing, u.lastWrite = nil, true, time.Now()
	return take
}

// schedule arms u's timer. Callers hold mu.
func (b *readCursorBuffer) schedule(beadID string, u *cursorUser, wait time.Duration) {
	u.timer = time.AfterFunc(wait, func() { b.flush(beadID) })
}

// done ends a write: a failed write's positions wait again unless a newer one
// replaced them, and anything that arrived meanwhile is scheduled.
func (b *readCursorBuffer) done(beadID string, took map[string]string, err error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	u := b.users[beadID]
	if u == nil {
		return
	}
	u.writing = false
	if err != nil {
		if u.pending == nil {
			u.pending = map[string]string{}
		}
		for ch, msg := range took {
			if _, newer := u.pending[ch]; !newer {
				u.pending[ch] = msg
			}
		}
	}
	if len(u.pending) > 0 && u.timer == nil {
		b.schedule(beadID, u, b.interval)
	}
}

// flush is the timer's write of the positions waiting for beadID. It runs
// from a clean server context: the request that reported them is gone.
func (b *readCursorBuffer) flush(beadID string) {
	b.mu.Lock()
	u := b.users[beadID]
	if u == nil {
		b.mu.Unlock()
		return
	}
	u.timer = nil
	if len(u.pending) == 0 || u.writing {
		b.mu.Unlock()
		return
	}
	actor := u.actor
	take := b.take(u)
	b.mu.Unlock()
	ctx, cancel := context.WithTimeout(context.Background(), readCursorFlushTimeout)
	defer cancel()
	err := b.write(ctx, beadID, actor, take)
	if err != nil {
		slog.Warn("read cursors: deferred write failed", "bead_id", beadID, "error", err)
	}
	b.done(beadID, take, err)
}

// prune drops idle users once the map grows, so it stays bounded by the
// people active in the last interval. Callers hold mu.
func (b *readCursorBuffer) prune() {
	if len(b.users) < 1024 {
		return
	}
	cut := time.Now().Add(-b.interval)
	for id, u := range b.users {
		if len(u.pending) == 0 && u.timer == nil && !u.writing && u.lastWrite.Before(cut) {
			delete(b.users, id)
		}
	}
}
