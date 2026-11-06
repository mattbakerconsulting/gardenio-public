package postgres

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"io"
	"testing"

	"github.com/prometheus/client_golang/prometheus"
	dto "github.com/prometheus/client_model/go"

	"gardenio/kbeads/internal/metrics"
	"gardenio/kbeads/internal/store"
)

// fakeConnector hands out fakeConns: every query returns rows rows of cols
// string columns, every exec affects one row.
type fakeConnector struct{ rows, cols int }

func (c fakeConnector) Connect(context.Context) (driver.Conn, error) {
	return &fakeConn{rows: c.rows, cols: c.cols}, nil
}
func (fakeConnector) Driver() driver.Driver { return fakeDriver{} }

type fakeDriver struct{}

func (fakeDriver) Open(string) (driver.Conn, error) {
	return nil, errors.New("open through the connector")
}

type fakeConn struct{ rows, cols int }

func (*fakeConn) Prepare(string) (driver.Stmt, error) { return nil, errors.New("no prepare") }
func (*fakeConn) Close() error                        { return nil }
func (*fakeConn) Begin() (driver.Tx, error)           { return fakeTx{}, nil }
func (c *fakeConn) QueryContext(context.Context, string, []driver.NamedValue) (driver.Rows, error) {
	return &fakeRows{left: c.rows, cols: c.cols}, nil
}
func (*fakeConn) ExecContext(context.Context, string, []driver.NamedValue) (driver.Result, error) {
	return driver.RowsAffected(1), nil
}

type fakeTx struct{}

func (fakeTx) Commit() error   { return nil }
func (fakeTx) Rollback() error { return nil }

type fakeRows struct{ left, cols int }

func (r *fakeRows) Columns() []string { return make([]string, r.cols) }
func (*fakeRows) Close() error        { return nil }
func (r *fakeRows) Next(dest []driver.Value) error {
	if r.left == 0 {
		return io.EOF
	}
	r.left--
	for i := range dest {
		dest[i] = "x"
	}
	return nil
}

func newInstrumentedFakeStore(t *testing.T, rows, cols int) *PostgresStore {
	t.Helper()
	db := sql.OpenDB(instrumentedConnector{fakeConnector{rows: rows, cols: cols}})
	t.Cleanup(func() { db.Close() })
	return &PostgresStore{db: db}
}

// histCounts returns the sample count and sum of h's series for method.
func histCounts(t *testing.T, h *prometheus.HistogramVec, method string) (uint64, float64) {
	t.Helper()
	var m dto.Metric
	if err := h.WithLabelValues(method).(prometheus.Histogram).Write(&m); err != nil {
		t.Fatalf("read histogram: %v", err)
	}
	return m.GetHistogram().GetSampleCount(), m.GetHistogram().GetSampleSum()
}

func TestStoreQueryMetrics_QueryLabelledByStoreMethod(t *testing.T) {
	s := newInstrumentedFakeStore(t, 3, 1)
	durBefore, _ := histCounts(t, metrics.StoreQueryDuration, "GetLabels")
	rowsBefore, rowSumBefore := histCounts(t, metrics.StoreRowsReturned, "GetLabels")

	labels, err := s.GetLabels(context.Background(), "gd-x")
	if err != nil {
		t.Fatalf("GetLabels: %v", err)
	}
	if len(labels) != 3 {
		t.Fatalf("GetLabels returned %d labels, want 3", len(labels))
	}

	if n, _ := histCounts(t, metrics.StoreQueryDuration, "GetLabels"); n != durBefore+1 {
		t.Errorf("duration samples for GetLabels = %d, want %d", n, durBefore+1)
	}
	n, sum := histCounts(t, metrics.StoreRowsReturned, "GetLabels")
	if n != rowsBefore+1 || sum-rowSumBefore != 3 {
		t.Errorf("rows for GetLabels: samples %d (+%d), sum +%v; want +1 sample of 3 rows", n, n-rowsBefore, sum-rowSumBefore)
	}
}

func TestStoreQueryMetrics_TxQueryLabelledByTxStoreMethod(t *testing.T) {
	s := newInstrumentedFakeStore(t, 2, 2)
	before, sumBefore := histCounts(t, metrics.StoreRowsReturned, "GetLabelsForBeads")

	err := s.RunInTransaction(context.Background(), func(tx store.Store) error {
		_, err := tx.GetLabelsForBeads(context.Background(), []string{"gd-x"})
		return err
	})
	if err != nil {
		t.Fatalf("RunInTransaction: %v", err)
	}

	n, sum := histCounts(t, metrics.StoreRowsReturned, "GetLabelsForBeads")
	if n != before+1 || sum-sumBefore != 2 {
		t.Errorf("rows for GetLabelsForBeads: +%d samples, +%v rows; want +1 sample of 2 rows", n-before, sum-sumBefore)
	}
}

func TestStoreQueryMetrics_ExecTimedNotRowCounted(t *testing.T) {
	s := newInstrumentedFakeStore(t, 0, 0)
	durBefore, _ := histCounts(t, metrics.StoreQueryDuration, "UpsertGate")
	rowsBefore, _ := histCounts(t, metrics.StoreRowsReturned, "UpsertGate")

	if err := s.UpsertGate(context.Background(), "gd-agent", "decision"); err != nil {
		t.Fatalf("UpsertGate: %v", err)
	}

	if n, _ := histCounts(t, metrics.StoreQueryDuration, "UpsertGate"); n != durBefore+1 {
		t.Errorf("duration samples for UpsertGate = +%d, want +1", n-durBefore)
	}
	if n, _ := histCounts(t, metrics.StoreRowsReturned, "UpsertGate"); n != rowsBefore {
		t.Errorf("an exec must not observe rows returned, got +%d samples", n-rowsBefore)
	}
}

func TestStoreQueryMetrics_QueryOutsideStoreIsOther(t *testing.T) {
	s := newInstrumentedFakeStore(t, 1, 1)
	before, _ := histCounts(t, metrics.StoreQueryDuration, metrics.StoreMethodOther)

	rows, err := s.db.QueryContext(context.Background(), "SELECT 1")
	if err != nil {
		t.Fatalf("query: %v", err)
	}
	for rows.Next() {
	}
	rows.Close()

	if n, _ := histCounts(t, metrics.StoreQueryDuration, metrics.StoreMethodOther); n != before+1 {
		t.Errorf("duration samples for %q = +%d, want +1", metrics.StoreMethodOther, n-before)
	}
}

func TestStoreMethodLabel(t *testing.T) {
	for _, tc := range []struct {
		name         string
		wantLabel    string
		wantExported bool
	}{
		{"(*PostgresStore).ListBeads", "ListBeads", true},
		{"(*PostgresStore).RunInTransaction.func1", "RunInTransaction", true},
		{"(*txStore).CreateBead", "CreateBead", true},
		{"(*PostgresStore).listHelper", "listHelper", false},
		{"queryListBeads", "queryListBeads", false},
		{"queryListBeads.func2.1", "queryListBeads", false},
		{"scanRows[...]", "scanRows", false},
		{"(*passwordFileConnector).Connect", "Connect", false},
		{"(*instrumentedConn).QueryContext", "", false},
		{"(*instrumentedRows).Close", "", false},
		{"TestStoreMethodLabel.func1", "", false},
		{"(*PostgresStore", "", false},
	} {
		label, exported := storeMethodLabel(tc.name)
		if label != tc.wantLabel || exported != tc.wantExported {
			t.Errorf("storeMethodLabel(%q) = (%q, %v), want (%q, %v)", tc.name, label, exported, tc.wantLabel, tc.wantExported)
		}
	}
}
