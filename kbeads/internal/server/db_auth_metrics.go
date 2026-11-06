package server

import (
	"errors"
	"strings"

	"github.com/prometheus/client_golang/prometheus"
)

// dbAuthErrorsTotal counts 5xx responses whose root cause is a PostgreSQL
// authentication failure (SQLSTATE 28P01, "invalid_password"). This is the
// signal an alert keys on: a live daemon that suddenly cannot authenticate to
// RDS — the exact fleet-wide blackout of bug ka-0000006554, where the beads
// daemon lost DB auth for ~4 minutes, failed 957 requests (40 of them writes),
// then self-healed with NO pod restart and, crucially, NO alert because the
// daemon exposed no request-status or DB-error metric at all.
//
// The metric is labelled by `handler` (the AttachServerError handler name) so a
// firing alert names which surfaces are 500ing, and so a partial fault (some
// pooled connections still valid, new dials failing) is visible per endpoint.
//
// A 28P01 is fundamentally TRANSIENT for this deployment: prod reads the DB
// password from a live-updating mounted Secret file (BEADS_DATABASE_PASSWORD_FILE,
// bug ka-0000004150) whose value lags an RDS master-password rotation by up to
// one ExternalSecret refresh interval (5m). During that stale-file window every
// NEW physical connection authenticates with the old password and gets 28P01;
// the daemon self-heals the moment ESO writes the fresh password. So the counter
// also gates the 500→503 reclassification in writeServerError: a retryable
// signal, not a permanent one.
var dbAuthErrorsTotal = prometheus.NewCounterVec(
	prometheus.CounterOpts{
		Name: "gardenio_beads_db_auth_errors_total",
		Help: "Count of 5xx responses root-caused to a PostgreSQL authentication " +
			"failure (SQLSTATE 28P01). Non-zero means the daemon cannot authenticate " +
			"to its database — a fleet-wide data-plane outage. Labelled by handler.",
	},
	[]string{"handler"},
)

// queryCanceledErrorsTotal counts 5xx/503 responses root-caused to a PostgreSQL
// statement cancellation (SQLSTATE 57014) — on this deployment, the 30s
// request-context deadline firing on a slow query. It is the observable signal
// for bug ka-0000003621: a rising rate on handler="handleListBeads" /
// "handleSemanticSearch" means `kd search` is crossing the deadline again
// (e.g. the trgm indexes regressed or a new query shape re-introduced a seq
// scan). Labelled by handler so an alert names which surface is timing out.
// Incremented in the same 5xx choke point (loggingMiddleware) as
// gardenio_beads_db_auth_errors_total, so the series stays complete regardless of which
// handler produced the cancellation.
var queryCanceledErrorsTotal = prometheus.NewCounterVec(
	prometheus.CounterOpts{
		Name: "gardenio_beads_query_canceled_errors_total",
		Help: "Count of responses root-caused to a PostgreSQL statement " +
			"cancellation (SQLSTATE 57014), i.e. a query cancelled by the 30s " +
			"request deadline. A rising rate means a slow query is crossing the " +
			"deadline (bug ka-0000003621). Labelled by handler.",
	},
	[]string{"handler"},
)

// pgInvalidPasswordSQLState is PostgreSQL SQLSTATE 28P01 (invalid_password) —
// the code lib/pq surfaces as `pq: password authentication failed for user`.
const pgInvalidPasswordSQLState = "28P01"

// pgQueryCanceledSQLState is PostgreSQL SQLSTATE 57014 (query_canceled) — raised
// when a statement is cancelled, which on this deployment is overwhelmingly the
// 30s request-context deadline in requestTimeoutMiddleware firing on a slow
// query (lib/pq surfaces it as `pq: canceling statement due to user request`).
const pgQueryCanceledSQLState = "57014"

// isDBAuthError reports whether err (anywhere in its wrap chain) is a
// PostgreSQL authentication failure (SQLSTATE 28P01).
//
// It checks the SQLSTATE via the same `interface{ SQLState() string }` shape the
// rest of the server uses to classify pq errors without importing lib/pq
// directly (cf. isUniqueViolation in beads_idempotency.go). It falls back to a
// substring match on the error text for the case where the pq error has been
// flattened into a string by an intermediate boundary — notably the gRPC
// ListBeads path, which wraps the cause as
// `rpc error: code = Internal desc = ...: pq: password authentication failed
// for user "gardenio" (28P01)` (observed verbatim in the ka-0000006554 incident
// logs), losing the typed error but preserving the SQLSTATE in the message.
func isDBAuthError(err error) bool {
	if err == nil {
		return false
	}
	var pqe interface{ SQLState() string }
	if errors.As(err, &pqe) && pqe.SQLState() == pgInvalidPasswordSQLState {
		return true
	}
	// Substring fallback for a stringified pq error (e.g. wrapped through a
	// gRPC status). Match on the SQLSTATE token AND the canonical pq message
	// so an unrelated error that merely mentions "28P01" cannot trip it.
	msg := err.Error()
	return strings.Contains(msg, "("+pgInvalidPasswordSQLState+")") &&
		strings.Contains(msg, "password authentication failed")
}

// isQueryCanceledError reports whether err (anywhere in its wrap chain) is a
// PostgreSQL statement cancellation (SQLSTATE 57014, query_canceled).
//
// On this deployment a 57014 is essentially always the 30s request-context
// deadline (requestTimeoutMiddleware, http.go) cancelling a slow query — most
// notably the full seq scan of the 695k-row beads table on a multi-term
// `kd search` (bug ka-0000003621 / design ka-0000004060), which surfaced to
// callers as a bare, permanent-looking `HTTP 500: failed to list beads`. That
// is a TRANSIENT, retryable condition — a caller that backs off and retries (or
// narrows the query) may well succeed — so writeServerError downgrades it to a
// 503 + Retry-After, the same "slow, back off & retry" contract used for the
// 28P01 stale-password window. This is a defense-in-depth safety net: the real
// fix is making the query fast (Phase 1/2 of ka-0000006300); this only changes
// the SHAPE of the error a still-slow query returns from "give up" to "retry".
//
// It uses the same interface{ SQLState() string } assertion as isDBAuthError,
// with the identical stringified-pq substring fallback for a 57014 flattened
// through a gRPC status boundary (matched on the SQLSTATE token AND the
// canonical pq message so an unrelated error that merely mentions "57014"
// cannot trip it).
func isQueryCanceledError(err error) bool {
	if err == nil {
		return false
	}
	var pqe interface{ SQLState() string }
	if errors.As(err, &pqe) && pqe.SQLState() == pgQueryCanceledSQLState {
		return true
	}
	msg := err.Error()
	return strings.Contains(msg, "("+pgQueryCanceledSQLState+")") &&
		strings.Contains(msg, "canceling statement due to user request")
}
