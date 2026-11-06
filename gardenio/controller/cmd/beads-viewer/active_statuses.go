package main

// active_statuses.go — the one canonical "non-terminal bead status" set shared
// by every viewer surface that enumerates beads to build a LIVE list/grid/hub
// where closing a bead is meant to remove it (ka-0000005651, follow-up to the
// /charts "Dashboard not found" fix ka-0000004760).
//
// Why this exists as a shared constant rather than inline slices: the /charts
// hub bug was an instance of a class — "a bead's mere existence in the store is
// assumed to mean it belongs on this surface" — and the fix (a Statuses filter)
// had to be copied to each enumeration surface that predated it. Sweeping the
// viewer (ka-0000005651) found three more surfaces missing the same filter
// (/plans, the role-hub config/advice panels, the project-hub gardeners digest).
// Centralizing the active set here means a surface author reaches for ONE named
// value instead of re-deriving the string list (and re-omitting it), and a
// future change to what "active" means happens in one place.
//
// Non-terminal statuses: a bead is "active" (still live on a hub) unless it has
// been closed. open / in_progress / blocked / deferred are all pre-terminal —
// the operator closes a bead to retire it from these surfaces. This mirrors the
// active set the grafana-bridge store uses and the `?filter=` "Live" default on
// /gardeners (Status != "closed").
//
// NOT for surfaces that INTENTIONALLY show closed beads — audit logs, "recently
// closed" panels, history/timeline feeds, the /gardeners "Archived" view. Those
// are correct to omit this filter; this constant is only for surfaces where a
// closed item should vanish.
var activeEnumStatuses = []string{"open", "in_progress", "blocked", "deferred"}
