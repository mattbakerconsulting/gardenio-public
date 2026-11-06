package main

// Stateless cursor pagination for the gardener life-thread tail
// (/gardener/{id}/runs). Design doc §7/§8 (task ka-0000006972).
//
// The previous behaviour fetched the whole thread (capped at
// gardenerIterationPageLimit) and sliced the ENTIRE tail beyond the eager head
// into a single HTMX response. p95 renderable iterations is 81 and the max is
// 167 today, so that worked — but it scales O(tail) per reveal and would dump
// hundreds of cards in one response if bead retention is ever extended.
//
// Instead we serve the tail in bounded pages of gardenerRunsPageSize, each page
// carrying a continuation cursor for the next. The cursor is keyed on
// (schedule_key, id): schedule_key is the gardener's per-fire slot (a monotonic,
// lexically-sortable RFC3339-minute prefix stamped on every trigger-path spawn)
// and id is the bead id tie-breaker. It is fully STATELESS — no server-side
// page state. Each /runs request re-runs the same cheap indexed iteration
// lookup, re-derives the canonical newest-first ordering, locates the cursor
// anchor, and emits the next page.
//
// Anchor resolution is positional-first (exact bead-id match in the ordered
// slice — always correct regardless of how ordering was derived) with a
// (schedule_key, id) resync fallback for the rare case where the anchor bead was
// pruned between two reveals: we resume at the first card strictly older than
// the cursor position. This is why the cursor is keyed on (schedule_key, id)
// rather than a raw offset — an offset would silently skip or repeat cards when
// the underlying set shrinks mid-pagination.

import (
	"encoding/base64"
	"strings"
)

// gardenerRunsPageSize bounds how many older-iteration cards a single
// /gardener/{id}/runs reveal returns. The tail is paged in chunks of this size,
// each chunk handing back a continuation cursor for the next. Matches
// gardenerEagerRuns so every page (eager head included) is the same height.
const gardenerRunsPageSize = 25

// runsCursor marks a position in the newest-first life-thread: the
// (schedule_key, id) of the last card already shown. The next page is the cards
// strictly older than this position. The zero value is not a valid cursor;
// callers pass a nil *runsCursor to mean "start at the eager-head boundary".
type runsCursor struct {
	Key string // schedule_key of the boundary card ("" on legacy iterations)
	ID  string // bead id of the boundary card (the authoritative tie-breaker)
}

// cursorFieldSep separates the two cursor components before base64 encoding.
// A newline can never appear in a schedule_key (an RFC3339-minute prefix) or a
// bead id (gd-…), so it is a safe, unambiguous delimiter.
const cursorFieldSep = "\n"

// encode renders the cursor as a URL-safe opaque token for the ?cursor= query
// param. It is base64url(key + "\n" + id) — compact, copy-pasteable, and never
// needs URL-escaping in an href.
func (c runsCursor) encode() string {
	raw := c.Key + cursorFieldSep + c.ID
	return base64.RawURLEncoding.EncodeToString([]byte(raw))
}

// decodeRunsCursor parses a ?cursor= token back into a runsCursor. It returns
// ok=false for an empty, malformed, or id-less token so the handler can treat a
// junk cursor as "start from the eager-head boundary" rather than erroring — a
// stale or hand-edited cursor should degrade to the first tail page, never 500.
func decodeRunsCursor(tok string) (runsCursor, bool) {
	tok = strings.TrimSpace(tok)
	if tok == "" {
		return runsCursor{}, false
	}
	raw, err := base64.RawURLEncoding.DecodeString(tok)
	if err != nil {
		return runsCursor{}, false
	}
	key, id, found := strings.Cut(string(raw), cursorFieldSep)
	if !found || id == "" {
		return runsCursor{}, false
	}
	return runsCursor{Key: key, ID: id}, true
}

// runsStartIndex resolves the index in the newest-first ordered thread at which
// the next page begins, given the continuation cursor.
//
//   - cur == nil → start at the eager-head boundary (gardenerEagerRuns): the
//     first /runs reveal picks up exactly where the eager page left off.
//   - cur set, anchor bead present → start right after it (exact positional
//     match; correct no matter how the ordering was derived).
//   - cur set, anchor bead pruned → resync by (schedule_key, id): resume at the
//     first card strictly older than the cursor position so we neither skip nor
//     repeat cards when the set shrinks between reveals.
//
// The returned index is clamped to [0, len(iters)].
func runsStartIndex(iters []gardenerIterationView, cur *runsCursor) int {
	if cur == nil {
		if gardenerEagerRuns < len(iters) {
			return gardenerEagerRuns
		}
		return len(iters)
	}
	// Exact anchor match: next page is everything after the anchor.
	for i := range iters {
		if iters[i].ID == cur.ID {
			return i + 1
		}
	}
	// Pruned anchor: resume at the first card strictly older than the cursor in
	// newest-first (schedule_key, id) order.
	for i := range iters {
		if runsOlder(iters[i], *cur) {
			return i
		}
	}
	// Nothing is older than the cursor — the tail is exhausted.
	return len(iters)
}

// runsOlder reports whether iteration it sorts strictly OLDER than the cursor
// position in the newest-first thread, comparing on (schedule_key, id): older
// means a smaller schedule_key, or an equal key and a smaller id.
//
// This is the BEST-EFFORT resync comparator used ONLY when the cursor's anchor
// bead was pruned between two reveals — the common case is the exact id match
// in runsStartIndex. chainOrder's full tie-break within an equal schedule_key is
// (created_at, id); this comparator uses id alone, so in the rare pruned-anchor
// + same-minute-fire + differing-created_at case the resume point can be off by
// at most one same-key card. That is an acceptable degradation for a fallback
// that only fires when the precise anchor no longer exists.
func runsOlder(it gardenerIterationView, cur runsCursor) bool {
	if it.ScheduleKey != cur.Key {
		return it.ScheduleKey < cur.Key
	}
	return it.ID < cur.ID
}

// runsPage slices one bounded page of older iterations from the ordered
// newest-first thread, starting after the cursor, and returns a continuation
// cursor for the page after it (nil when the tail is exhausted). It never
// allocates a copy of the underlying cards — page is a sub-slice of iters.
func runsPage(iters []gardenerIterationView, cur *runsCursor) (page []gardenerIterationView, next *runsCursor) {
	start := runsStartIndex(iters, cur)
	if start >= len(iters) {
		return nil, nil
	}
	end := start + gardenerRunsPageSize
	if end >= len(iters) {
		return iters[start:], nil
	}
	last := iters[end-1]
	return iters[start:end], &runsCursor{Key: last.ScheduleKey, ID: last.ID}
}
