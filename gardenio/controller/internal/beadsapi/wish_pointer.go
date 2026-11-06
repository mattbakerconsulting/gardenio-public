package beadsapi

// Wish-pointer field duality (Threads initiative, Phase 1b, task ka-0000000078).
//
// A thread (wish) head's ka-id is carried on every derivative bead — agent,
// member task, message — via the origin-agnostic wish-pointer field
// `thread_id`. The head itself carries NO thread_id: it IS the wish, so it does
// not point at itself (see bridge/thread_bead.go's threadIdentityLabel doc).
//
// Historically two fields carried this same value:
//
//   - FieldThreadID ("thread_id")           — the CANONICAL wish pointer. The
//     beads-viewer /threads INDEX enumerates members via this field ONLY
//     (threadList), and membership resolves as
//     UNION(fields.thread_id == headID, parent-child closure).
//   - FieldSlackThreadBeadID                 — a LEGACY compatibility alias
//     ("slack_thread_bead_id") holding the same value. The event-driven thread
//     autocloser (bridge/thread_task_autoclose.go) still keys off this field,
//     so it must be stamped alongside thread_id or a resumed/restarted thread
//     agent becomes unreachable to the autocloser (ka-0000007241).
//
// Every producer that stamps the wish pointer must set BOTH fields to the same
// value. That duplication was open-coded at six call sites across the bridge
// and pool-manager, each an independent chance to set one field and forget the
// other — precisely the drift that made /threads cards read "no work yet"
// (ka-0000007241). SetWishPointer collapses the duality into one helper so the
// invariant "the alias always mirrors thread_id" holds by construction.
const (
	// FieldThreadID is the canonical origin-agnostic wish-pointer field: its
	// value is the thread (wish) head's ka-id. This is the field the
	// beads-viewer thread index and membership queries read.
	FieldThreadID = "thread_id"
	// FieldSlackThreadBeadID is the legacy compatibility alias for the wish
	// pointer, retained because the event-driven thread autocloser keys off it.
	// It always holds the same value as FieldThreadID.
	FieldSlackThreadBeadID = "slack_thread_bead_id"
)

// SetWishPointer stamps the thread (wish) head id onto a bead's field map,
// writing BOTH the canonical wish-pointer field (FieldThreadID) and its legacy
// compatibility alias (FieldSlackThreadBeadID) to the same value so the two can
// never drift apart.
//
// It is a no-op when headID is empty — the head itself, and any bead not bound
// to a thread, carries neither field — so callers can invoke it unconditionally
// in place of the guarded `if headID != "" { ... }` two-line stamp. The fields
// map must be non-nil (all current callers construct it before calling).
func SetWishPointer(fields map[string]string, headID string) {
	if headID == "" {
		return
	}
	fields[FieldThreadID] = headID
	fields[FieldSlackThreadBeadID] = headID
}
