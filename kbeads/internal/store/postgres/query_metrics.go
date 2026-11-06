package postgres

import (
	"context"
	"database/sql/driver"
	"io"
	"reflect"
	"runtime"
	"strings"
	"sync"
	"time"

	"gardenio/kbeads/internal/metrics"
)

// Per-store-method query metrics (gd-rXqkv4rSmG). pg_stat_statements groups
// load by normalized SQL text, which cannot say which code path sent a query.
// instrumentedConnector wraps the driver connector so every statement on every
// pooled connection (including inside transactions) is timed and, for queries,
// its rows counted, under the label of the store method that issued it. The
// label is read from the call stack, so no store method has to opt in. Query
// text, arguments and errors pass through unchanged.

// storePkgPrefix is the runtime function-name prefix of this package.
var storePkgPrefix = reflect.TypeOf(PostgresStore{}).PkgPath() + "."

// instrumentedConnector wraps a driver.Connector so each connection it opens
// records store query metrics. Driver() is promoted from the wrapped connector.
type instrumentedConnector struct {
	driver.Connector
}

// Connect opens a connection through the wrapped connector and instruments it.
func (c instrumentedConnector) Connect(ctx context.Context) (driver.Conn, error) {
	conn, err := c.Connector.Connect(ctx)
	if err != nil {
		return nil, err
	}
	return &instrumentedConn{Conn: conn}, nil
}

// instrumentedConn times QueryContext and ExecContext and delegates every other
// optional driver interface lib/pq implements, so database/sql takes the same
// paths it would on the bare connection. When the wrapped connection lacks an
// interface, each method falls back to what database/sql does without it.
type instrumentedConn struct {
	driver.Conn
}

var (
	_ driver.QueryerContext     = (*instrumentedConn)(nil)
	_ driver.ExecerContext      = (*instrumentedConn)(nil)
	_ driver.ConnPrepareContext = (*instrumentedConn)(nil)
	_ driver.ConnBeginTx        = (*instrumentedConn)(nil)
	_ driver.Pinger             = (*instrumentedConn)(nil)
	_ driver.SessionResetter    = (*instrumentedConn)(nil)
	_ driver.Validator          = (*instrumentedConn)(nil)
	_ driver.NamedValueChecker  = (*instrumentedConn)(nil)
)

func (c *instrumentedConn) QueryContext(ctx context.Context, query string, args []driver.NamedValue) (driver.Rows, error) {
	q, ok := c.Conn.(driver.QueryerContext)
	if !ok {
		return nil, driver.ErrSkip
	}
	method := storeMethod()
	start := time.Now()
	rows, err := q.QueryContext(ctx, query, args)
	if err != nil {
		if err != driver.ErrSkip {
			metrics.ObserveStoreQuery(method, time.Since(start))
		}
		return rows, err
	}
	return &instrumentedRows{Rows: rows, method: method, start: start}, nil
}

func (c *instrumentedConn) ExecContext(ctx context.Context, query string, args []driver.NamedValue) (driver.Result, error) {
	e, ok := c.Conn.(driver.ExecerContext)
	if !ok {
		return nil, driver.ErrSkip
	}
	method := storeMethod()
	start := time.Now()
	res, err := e.ExecContext(ctx, query, args)
	if err != driver.ErrSkip {
		metrics.ObserveStoreQuery(method, time.Since(start))
	}
	return res, err
}

func (c *instrumentedConn) PrepareContext(ctx context.Context, query string) (driver.Stmt, error) {
	if p, ok := c.Conn.(driver.ConnPrepareContext); ok {
		return p.PrepareContext(ctx, query)
	}
	return c.Conn.Prepare(query)
}

func (c *instrumentedConn) BeginTx(ctx context.Context, opts driver.TxOptions) (driver.Tx, error) {
	if b, ok := c.Conn.(driver.ConnBeginTx); ok {
		return b.BeginTx(ctx, opts)
	}
	// Begin is deprecated, but it is database/sql's own fallback for a driver
	// without ConnBeginTx; lib/pq has BeginTx, so this serves only such drivers.
	return c.Conn.Begin() //nolint:staticcheck // see above
}

func (c *instrumentedConn) Ping(ctx context.Context) error {
	if p, ok := c.Conn.(driver.Pinger); ok {
		return p.Ping(ctx)
	}
	return nil
}

func (c *instrumentedConn) ResetSession(ctx context.Context) error {
	if r, ok := c.Conn.(driver.SessionResetter); ok {
		return r.ResetSession(ctx)
	}
	return nil
}

func (c *instrumentedConn) IsValid() bool {
	if v, ok := c.Conn.(driver.Validator); ok {
		return v.IsValid()
	}
	return true
}

func (c *instrumentedConn) CheckNamedValue(nv *driver.NamedValue) error {
	if ch, ok := c.Conn.(driver.NamedValueChecker); ok {
		return ch.CheckNamedValue(nv)
	}
	return driver.ErrSkip
}

// instrumentedRows counts rows as database/sql reads them and records the
// query's duration and row count once, when the rows are closed.
type instrumentedRows struct {
	driver.Rows
	method string
	start  time.Time
	n      int
	once   sync.Once
}

func (r *instrumentedRows) Next(dest []driver.Value) error {
	err := r.Rows.Next(dest)
	if err == nil {
		r.n++
	}
	return err
}

func (r *instrumentedRows) Close() error {
	err := r.Rows.Close()
	r.once.Do(func() {
		metrics.ObserveStoreQuery(r.method, time.Since(r.start))
		metrics.ObserveStoreRows(r.method, r.n)
	})
	return err
}

func (r *instrumentedRows) HasNextResultSet() bool {
	if m, ok := r.Rows.(driver.RowsNextResultSet); ok {
		return m.HasNextResultSet()
	}
	return false
}

func (r *instrumentedRows) NextResultSet() error {
	if m, ok := r.Rows.(driver.RowsNextResultSet); ok {
		return m.NextResultSet()
	}
	return io.EOF
}

func (r *instrumentedRows) ColumnTypeScanType(index int) reflect.Type {
	if m, ok := r.Rows.(driver.RowsColumnTypeScanType); ok {
		return m.ColumnTypeScanType(index)
	}
	return reflect.TypeFor[any]()
}

func (r *instrumentedRows) ColumnTypeDatabaseTypeName(index int) string {
	if m, ok := r.Rows.(driver.RowsColumnTypeDatabaseTypeName); ok {
		return m.ColumnTypeDatabaseTypeName(index)
	}
	return ""
}

func (r *instrumentedRows) ColumnTypeLength(index int) (int64, bool) {
	if m, ok := r.Rows.(driver.RowsColumnTypeLength); ok {
		return m.ColumnTypeLength(index)
	}
	return 0, false
}

func (r *instrumentedRows) ColumnTypeNullable(index int) (nullable, ok bool) {
	if m, ok := r.Rows.(driver.RowsColumnTypeNullable); ok {
		return m.ColumnTypeNullable(index)
	}
	return false, false
}

func (r *instrumentedRows) ColumnTypePrecisionScale(index int) (precision, scale int64, ok bool) {
	if m, ok := r.Rows.(driver.RowsColumnTypePrecisionScale); ok {
		return m.ColumnTypePrecisionScale(index)
	}
	return 0, 0, false
}

// storeMethod names the code in this package that issued the current query:
// the innermost exported method of PostgresStore or txStore on the stack
// (ListBeads, GetLabelsForBeads, ...), else the innermost other function of
// this package (runMigrations, a query helper reached without a store method),
// else metrics.StoreMethodOther. Labels are function names, so the set is
// bounded by the package's code. PostgresStore.X and txStore.X share label X.
func storeMethod() string {
	var pcs [64]uintptr
	n := runtime.Callers(2, pcs[:])
	frames := runtime.CallersFrames(pcs[:n])
	fallback := ""
	for {
		f, more := frames.Next()
		if name, ok := strings.CutPrefix(f.Function, storePkgPrefix); ok {
			label, exported := storeMethodLabel(name)
			if exported {
				return label
			}
			if fallback == "" && label != "" {
				fallback = label
			}
		}
		if !more {
			break
		}
	}
	if fallback != "" {
		return fallback
	}
	return metrics.StoreMethodOther
}

// storeMethodLabel maps a package-relative runtime function name to its label.
// exported is true for an exported method of PostgresStore or txStore. The
// instrumentation's own frames, and a test function's, map to "".
//
//	(*PostgresStore).ListBeads        -> ListBeads, true
//	(*txStore).CreateBead.func1       -> CreateBead, true
//	(*PostgresStore).listHelper       -> listHelper, false
//	queryListBeads.func2              -> queryListBeads, false
//	(*instrumentedConn).QueryContext  -> "", false
func storeMethodLabel(name string) (label string, exported bool) {
	recv := ""
	if strings.HasPrefix(name, "(") {
		end := strings.Index(name, ").")
		if end < 0 {
			return "", false
		}
		recv, name = name[1:end], name[end+2:]
	}
	if i := strings.IndexAny(name, ".["); i >= 0 {
		name = name[:i]
	}
	if strings.HasPrefix(strings.TrimPrefix(recv, "*"), "instrumented") ||
		strings.HasPrefix(name, "Test") || name == "" {
		return "", false
	}
	isStore := recv == "*PostgresStore" || recv == "*txStore"
	return name, isStore && name[0] >= 'A' && name[0] <= 'Z'
}
