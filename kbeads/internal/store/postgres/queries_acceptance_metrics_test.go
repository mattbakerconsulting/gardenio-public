package postgres

import (
	"context"
	"testing"

	"gardenio/kbeads/internal/store"
	"github.com/DATA-DOG/go-sqlmock"
)

// TestQueryAcceptanceMetricsAllThreeFire pins the round-trip count and
// the math. All three sub-queries fire on a happy path; fractions are
// computed in Go from the returned counters.
func TestQueryAcceptanceMetricsAllFire(t *testing.T) {
	db, mock := newMockDB(t)

	// owner-share resolves Matt through operator_aliases; the query is
	// parameterized by the two canonical subjects + the two canonical
	// "<kind>:<subject>" literals (ka-0000005200).
	mock.ExpectQuery(`matt_owner_strings`).
		WithArgs(mattSlackSubject, mattOidcSubject, MattSlackOperator, MattOidcOperator).
		WillReturnRows(sqlmock.NewRows([]string{"matt", "total"}).AddRow(2, 10))

	// agent-operator-leaks and fleet-fallback-count both scan
	// mutation_history; the fleet query is pinned by its kind='fleet'
	// predicate so the two do not race on the shared matcher.
	mock.ExpectQuery(`AND NOT \(`).
		WillReturnRows(sqlmock.NewRows([]string{"count"}).AddRow(0))
	mock.ExpectQuery(`o\.kind = 'fleet'`).
		WillReturnRows(sqlmock.NewRows([]string{"count"}).AddRow(3))

	mock.ExpectQuery(`type = 'user'`).
		WillReturnRows(sqlmock.NewRows([]string{"total", "matched"}).AddRow(4, 3))

	mock.ExpectQuery(`FROM human_ops`).
		WithArgs(sentinelEmailDomain).
		WillReturnRows(sqlmock.NewRows([]string{"humans", "operator_rows"}).AddRow(5, 6))

	m, err := queryAcceptanceMetrics(context.Background(), db)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if m.OperatorCanonicalCoverage.HumanEmails != 5 || m.OperatorCanonicalCoverage.OperatorRows != 6 {
		t.Errorf("canonical coverage counts: want 5 humans / 6 rows, got %d/%d",
			m.OperatorCanonicalCoverage.HumanEmails, m.OperatorCanonicalCoverage.OperatorRows)
	}
	if m.OperatorCanonicalCoverage.Fragmented != 1 {
		t.Errorf("canonical coverage fragmented: want 1, got %d", m.OperatorCanonicalCoverage.Fragmented)
	}
	if m.OwnerShareMatt.Matt != 2 || m.OwnerShareMatt.Total != 10 {
		t.Errorf("owner-share counts: want 2/10, got %d/%d", m.OwnerShareMatt.Matt, m.OwnerShareMatt.Total)
	}
	if m.OwnerShareMatt.Fraction < 0.199 || m.OwnerShareMatt.Fraction > 0.201 {
		t.Errorf("owner-share fraction: want ~0.2, got %f", m.OwnerShareMatt.Fraction)
	}
	if m.AgentOperatorLeaks.Count != 0 {
		t.Errorf("agent leaks: want 0, got %d", m.AgentOperatorLeaks.Count)
	}
	if m.FleetFallbackCount.Count != 3 {
		t.Errorf("fleet fallbacks: want 3, got %d", m.FleetFallbackCount.Count)
	}
	if m.OperatorAliasCoverage.Total != 4 || m.OperatorAliasCoverage.Matched != 3 {
		t.Errorf("alias coverage counts: want 3/4, got %d/%d",
			m.OperatorAliasCoverage.Matched, m.OperatorAliasCoverage.Total)
	}
	if m.OperatorAliasCoverage.Fraction < 0.749 || m.OperatorAliasCoverage.Fraction > 0.751 {
		t.Errorf("alias coverage fraction: want 0.75, got %f", m.OperatorAliasCoverage.Fraction)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

// TestQueryAcceptanceMetricsEmptyDenominator keeps the fraction at 0
// (no divide-by-zero) when there are no beads / no users in the
// window.
func TestQueryAcceptanceMetricsEmptyDenominator(t *testing.T) {
	db, mock := newMockDB(t)

	mock.ExpectQuery(`matt_owner_strings`).
		WithArgs(mattSlackSubject, mattOidcSubject, MattSlackOperator, MattOidcOperator).
		WillReturnRows(sqlmock.NewRows([]string{"matt", "total"}).AddRow(0, 0))

	mock.ExpectQuery(`AND NOT \(`).
		WillReturnRows(sqlmock.NewRows([]string{"count"}).AddRow(0))
	mock.ExpectQuery(`o\.kind = 'fleet'`).
		WillReturnRows(sqlmock.NewRows([]string{"count"}).AddRow(0))

	mock.ExpectQuery(`type = 'user'`).
		WillReturnRows(sqlmock.NewRows([]string{"total", "matched"}).AddRow(0, 0))

	mock.ExpectQuery(`FROM human_ops`).
		WithArgs(sentinelEmailDomain).
		WillReturnRows(sqlmock.NewRows([]string{"humans", "operator_rows"}).AddRow(0, 0))

	m, err := queryAcceptanceMetrics(context.Background(), db)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if m.OwnerShareMatt.Fraction != 0 {
		t.Errorf("owner-share fraction on empty: want 0, got %f", m.OwnerShareMatt.Fraction)
	}
	if m.OperatorAliasCoverage.Fraction != 0 {
		t.Errorf("alias coverage fraction on empty: want 0, got %f", m.OperatorAliasCoverage.Fraction)
	}
	// No operator rows at all → no fragmentation, fraction defaults to 1.0
	// (the invariant holds vacuously) rather than a divide-by-zero.
	if m.OperatorCanonicalCoverage.Fraction != 1 || m.OperatorCanonicalCoverage.Fragmented != 0 {
		t.Errorf("canonical coverage on empty: want fraction 1.0 / fragmented 0, got %f / %d",
			m.OperatorCanonicalCoverage.Fraction, m.OperatorCanonicalCoverage.Fragmented)
	}
}

// TestQueryOwnerShareMattResolvesThroughAliases pins the identity-canonical
// owner-share query (ka-0000005200): it must bind BOTH Matt's slack subject
// and his oidc subject (so the alias lookup finds either operator row) plus
// the two canonical "<kind>:<subject>" literals (so it degrades to exact
// match on an un-backfilled DB), and it must resolve owners via the
// matt_owner_strings CTE rather than a bare `owner = slack:U…` filter.
func TestQueryOwnerShareMattResolvesThroughAliases(t *testing.T) {
	db, mock := newMockDB(t)

	mock.ExpectQuery(`operator_aliases`).
		WithArgs(mattSlackSubject, mattOidcSubject, MattSlackOperator, MattOidcOperator).
		WillReturnRows(sqlmock.NewRows([]string{"matt", "total"}).AddRow(221, 442))

	var out store.AcceptanceOwnerShare
	if err := queryOwnerShareMatt(context.Background(), db, &out); err != nil {
		t.Fatalf("queryOwnerShareMatt: %v", err)
	}
	// 221 oidc-owned beads that the old exact-match-on-slack query missed
	// entirely now count as Matt.
	if out.Matt != 221 || out.Total != 442 {
		t.Errorf("counts: want 221/442, got %d/%d", out.Matt, out.Total)
	}
	if out.Fraction < 0.499 || out.Fraction > 0.501 {
		t.Errorf("fraction: want ~0.5, got %f", out.Fraction)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

// TestQueryFleetFallbackCountIsSeparateSignal pins the new fleet-fallback
// signal (ka-0000005200): it counts last-1h mutation_history rows whose
// operator is kind='fleet' — the exact rows queryAgentOperatorLeaks
// deliberately excludes, so that fleet fallbacks are no longer invisible to
// monitoring.
func TestQueryFleetFallbackCountIsSeparateSignal(t *testing.T) {
	db, mock := newMockDB(t)

	mock.ExpectQuery(`o\.kind = 'fleet'`).
		WillReturnRows(sqlmock.NewRows([]string{"count"}).AddRow(17))

	var out store.AcceptanceFleetFallback
	if err := queryFleetFallbackCount(context.Background(), db, &out); err != nil {
		t.Fatalf("queryFleetFallbackCount: %v", err)
	}
	if out.Count != 17 {
		t.Errorf("fleet fallback count: want 17, got %d", out.Count)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

// TestQueryOperatorCanonicalCoverage_NoFragmentation is the healthy steady
// state (ka-0000002687): every human owns exactly one operators row, so
// HumanEmails == OperatorRows, Fragmented == 0, and Fraction == 1.0. It also
// pins the sentinel-domain bind param so the query excludes @noreply.kbeads.local
// placeholder rows from the human count.
func TestQueryOperatorCanonicalCoverage_NoFragmentation(t *testing.T) {
	db, mock := newMockDB(t)

	mock.ExpectQuery(`FROM human_ops`).
		WithArgs(sentinelEmailDomain).
		WillReturnRows(sqlmock.NewRows([]string{"humans", "operator_rows"}).AddRow(42, 42))

	var out store.AcceptanceCanonicalCoverage
	if err := queryOperatorCanonicalCoverage(context.Background(), db, &out); err != nil {
		t.Fatalf("queryOperatorCanonicalCoverage: %v", err)
	}
	if out.HumanEmails != 42 || out.OperatorRows != 42 {
		t.Errorf("counts: want 42/42, got %d/%d", out.HumanEmails, out.OperatorRows)
	}
	if out.Fragmented != 0 {
		t.Errorf("fragmented: want 0, got %d", out.Fragmented)
	}
	if out.Fraction != 1 {
		t.Errorf("fraction: want 1.0, got %f", out.Fraction)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

// TestQueryOperatorCanonicalCoverage_DetectsFragmentation proves the metric
// surfaces a split human: 40 distinct emails spread across 43 operator rows
// means 3 surplus rows a MergeOperators consolidation must still collapse, and
// the fraction drops below 1.0 so the acceptance gate can catch a regression.
func TestQueryOperatorCanonicalCoverage_DetectsFragmentation(t *testing.T) {
	db, mock := newMockDB(t)

	mock.ExpectQuery(`FROM human_ops`).
		WithArgs(sentinelEmailDomain).
		WillReturnRows(sqlmock.NewRows([]string{"humans", "operator_rows"}).AddRow(40, 43))

	var out store.AcceptanceCanonicalCoverage
	if err := queryOperatorCanonicalCoverage(context.Background(), db, &out); err != nil {
		t.Fatalf("queryOperatorCanonicalCoverage: %v", err)
	}
	if out.Fragmented != 3 {
		t.Errorf("fragmented: want 3, got %d", out.Fragmented)
	}
	if out.Fraction >= 1 || out.Fraction < 0.929 || out.Fraction > 0.931 {
		t.Errorf("fraction: want ~0.930, got %f", out.Fraction)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}
