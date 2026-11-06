// Package pgbudget is the daily Postgres efficiency report and the loop that
// turns a budget breach into a filed task (gd-ylgk1jBhLi, epic
// gd-PkZRCb0BuB). It reads the postgres_exporter series back out of
// vmsingle, ranks the day's statements and tables, judges each of the last
// two days against the budgets the chart's vmalert rules use
// (templates/_postgres-budgets.tpl, vmalert.postgresBudgets), posts the
// report to a land's #machines and files one task per statement or table
// that was over budget on both days.
package pgbudget

import (
	"context"
	"encoding/json"
	"fmt"
	"regexp"
	"sort"
	"strings"
	"time"
)

// Budgets are vmalert.postgresBudgets' thresholds, by the same JSON keys, so
// the chart hands the controller the values it renders the rules from.
// Hourly budgets apply to a day as 24 hours' worth.
type Budgets struct {
	ShareRatio           float64 `json:"shareRatio"`
	ShareMinTotalSeconds float64 `json:"shareMinTotalSeconds"`
	SlowMeanSeconds      float64 `json:"slowMeanSeconds"`
	SlowMinCallsPerHour  float64 `json:"slowMinCallsPerHour"`
	ExcludeQueryRegex    string  `json:"excludeQueryRegex"`
	GrowthRatio          float64 `json:"growthRatio"`
	GrowthBytes          float64 `json:"growthBytes"`
	GrowthMinBytes       float64 `json:"growthMinBytes"`
}

// DefaultBudgets are the chart's defaults.
func DefaultBudgets() Budgets {
	return Budgets{
		ShareRatio:           0.2,
		ShareMinTotalSeconds: 60,
		SlowMeanSeconds:      1,
		SlowMinCallsPerHour:  100,
		ExcludeQueryRegex:    "COPY .*",
		GrowthRatio:          0.25,
		GrowthBytes:          500 << 20,
		GrowthMinBytes:       100 << 20,
	}
}

// ParseBudgets reads the chart's JSON over the defaults; "" is the defaults.
func ParseBudgets(raw string) (Budgets, error) {
	b := DefaultBudgets()
	if strings.TrimSpace(raw) == "" {
		return b, nil
	}
	if err := json.Unmarshal([]byte(raw), &b); err != nil {
		return b, fmt.Errorf("pgbudget: budgets: %w", err)
	}
	if _, err := regexp.Compile("^(?:" + b.ExcludeQueryRegex + ")$"); err != nil {
		return b, fmt.Errorf("pgbudget: excludeQueryRegex: %w", err)
	}
	return b, nil
}

// excludeRe is PromQL's fully anchored query!~ match; nil excludes nothing.
func (b Budgets) excludeRe() *regexp.Regexp {
	if b.ExcludeQueryRegex == "" {
		return nil
	}
	re, _ := regexp.Compile("^(?:" + b.ExcludeQueryRegex + ")$")
	return re
}

// The exporter series the report reads. component="postgres" is the scrape
// relabelling's label for the postgres pod.
const (
	qStatementExec  = `sum by (datname, queryid, query) (increase(pg_top_statements_exec_seconds{component="postgres"}[24h]))`
	qStatementCalls = `sum by (datname, queryid, query) (increase(pg_top_statements_calls{component="postgres"}[24h]))`
	qTableBytes     = `sum by (datname, schemaname, relname) (pg_top_tables_total_bytes{component="postgres"})`
	qCallers        = `max by (datname, queryid, application_name) (max_over_time(pg_active_statements_backends{component="postgres"}[48h]))`
	qFired          = `max by (alertname, budget, datname, queryid, schemaname, relname, application_name) (max_over_time(ALERTS{alertstate="firing",subsystem="postgres-budget"}[24h]))`
)

// Day is one statement's numbers over a 24h window.
type Day struct {
	ExecSeconds float64
	Calls       float64
	// Share is ExecSeconds over the window's total statement time.
	Share float64
	// Over names the budgets the statement broke that day.
	Over []string
}

// Mean is seconds per call, 0 with no calls.
func (d Day) Mean() float64 {
	if d.Calls <= 0 {
		return 0
	}
	return d.ExecSeconds / d.Calls
}

// Statement is one pg_stat_statements entry over the last two days.
type Statement struct {
	Datname, QueryID, Query string
	Today                   Day
	// Yesterday is the 24h before; Seen is false when the statement was not
	// among the exporter's top statements then.
	Yesterday Day
	Seen      bool
	// Callers are the application_names caught running it in the last 48h.
	Callers []string
}

// Key is the dedupe key for the statement's task.
func (s Statement) Key() string { return "query:" + s.Datname + ":" + s.QueryID }

// Table is one relation's size and growth over the last two days.
type Table struct {
	Datname, Schema, Rel string
	Bytes                float64
	// Growth is the last 24h, PrevGrowth the 24h before; Has* are false
	// when the table was not among the exporter's top tables at the window's
	// start.
	Growth, PrevGrowth       float64
	HasGrowth, HasPrevGrowth bool
	Over, PrevOver           []string
}

// Name is datname.schema.relname.
func (t Table) Name() string { return t.Datname + "." + t.Schema + "." + t.Rel }

// Key is the dedupe key for the table's task.
func (t Table) Key() string { return "table:" + t.Name() }

// Fired is one budget alert that fired in the last 24h.
type Fired struct {
	Alert  string
	Labels map[string]string
}

// Report is one day's efficiency report.
type Report struct {
	AsOf time.Time
	// Total statement time over each window, the sum over the exporter's
	// top statements.
	TotalSeconds, PrevTotalSeconds float64
	Statements                     []Statement // by Today.ExecSeconds, descending
	Tables                         []Table     // by Growth, descending
	Fired                          []Fired
}

// TopStatements returns the first n statements by total time.
func (r *Report) TopStatements(n int) []Statement { return r.Statements[:min(n, len(r.Statements))] }

// TopGrowth returns the first n tables by 24h growth that have one.
func (r *Report) TopGrowth(n int) []Table {
	var out []Table
	for _, t := range r.Tables {
		if t.HasGrowth && len(out) < n {
			out = append(out, t)
		}
	}
	return out
}

// Breach is a statement or table over budget on both of the last two days.
type Breach struct {
	Key       string
	Statement *Statement
	Table     *Table
	// Weight orders breaches when the daily cap cuts the list: statement
	// seconds or table bytes grown today.
	Weight float64
}

// Breaches are the statements and tables over budget today and the day
// before, heaviest first.
func (r *Report) Breaches() []Breach {
	var out []Breach
	for i := range r.Statements {
		s := &r.Statements[i]
		if s.Seen && len(s.Today.Over) > 0 && len(s.Yesterday.Over) > 0 {
			out = append(out, Breach{Key: s.Key(), Statement: s, Weight: s.Today.ExecSeconds})
		}
	}
	// Bytes outweigh seconds by orders of magnitude, so statements stay first
	// only by order of append; sort each kind on its own.
	stmts := len(out)
	for i := range r.Tables {
		t := &r.Tables[i]
		if len(t.Over) > 0 && len(t.PrevOver) > 0 {
			out = append(out, Breach{Key: t.Key(), Table: t, Weight: t.Growth})
		}
	}
	sort.SliceStable(out[:stmts], func(i, j int) bool { return out[i].Weight > out[j].Weight })
	sort.SliceStable(out[stmts:], func(i, j int) bool { return out[stmts+i].Weight > out[stmts+j].Weight })
	return out
}

const day = 24 * time.Hour

// Gather reads the report as of now.
func Gather(ctx context.Context, q Querier, b Budgets, now time.Time) (*Report, error) {
	now = now.UTC().Truncate(time.Minute)
	r := &Report{AsOf: now}
	if err := r.gatherStatements(ctx, q, b); err != nil {
		return nil, err
	}
	if err := r.gatherTables(ctx, q, b); err != nil {
		return nil, err
	}
	if err := r.gatherFired(ctx, q); err != nil {
		return nil, err
	}
	return r, nil
}

func (r *Report) gatherStatements(ctx context.Context, q Querier, b Budgets) error {
	now := r.AsOf
	type sk struct{ db, id string }
	stmts := map[sk]*Statement{}
	read := func(expr string, at time.Time, set func(*Statement, float64)) error {
		res, err := q.Query(ctx, expr, at)
		if err != nil {
			return err
		}
		for _, s := range res {
			k := sk{s.Labels["datname"], s.Labels["queryid"]}
			if k.id == "" {
				continue
			}
			st := stmts[k]
			if st == nil {
				st = &Statement{Datname: k.db, QueryID: k.id, Query: s.Labels["query"]}
				stmts[k] = st
			}
			set(st, s.Value)
		}
		return nil
	}
	if err := read(qStatementExec, now, func(s *Statement, v float64) { s.Today.ExecSeconds = v }); err != nil {
		return err
	}
	if err := read(qStatementCalls, now, func(s *Statement, v float64) { s.Today.Calls = v }); err != nil {
		return err
	}
	if err := read(qStatementExec, now.Add(-day), func(s *Statement, v float64) { s.Yesterday.ExecSeconds, s.Seen = v, true }); err != nil {
		return err
	}
	if err := read(qStatementCalls, now.Add(-day), func(s *Statement, v float64) { s.Yesterday.Calls = v }); err != nil {
		return err
	}
	callers, err := q.Query(ctx, qCallers, now)
	if err != nil {
		return err
	}
	for _, c := range callers {
		if st := stmts[sk{c.Labels["datname"], c.Labels["queryid"]}]; st != nil && c.Labels["application_name"] != "" {
			st.Callers = append(st.Callers, c.Labels["application_name"])
		}
	}
	for _, s := range stmts {
		r.TotalSeconds += s.Today.ExecSeconds
		r.PrevTotalSeconds += s.Yesterday.ExecSeconds
	}
	excl := b.excludeRe()
	for _, s := range stmts {
		if r.TotalSeconds > 0 {
			s.Today.Share = s.Today.ExecSeconds / r.TotalSeconds
		}
		if r.PrevTotalSeconds > 0 {
			s.Yesterday.Share = s.Yesterday.ExecSeconds / r.PrevTotalSeconds
		}
		if excl == nil || !excl.MatchString(s.Query) {
			s.Today.Over = b.statementOver(s.Today, r.TotalSeconds)
			if s.Seen {
				s.Yesterday.Over = b.statementOver(s.Yesterday, r.PrevTotalSeconds)
			}
		}
		sort.Strings(s.Callers)
		r.Statements = append(r.Statements, *s)
	}
	sort.Slice(r.Statements, func(i, j int) bool {
		a, c := r.Statements[i], r.Statements[j]
		if a.Today.ExecSeconds != c.Today.ExecSeconds {
			return a.Today.ExecSeconds > c.Today.ExecSeconds
		}
		return a.Key() < c.Key()
	})
	return nil
}

func (r *Report) gatherTables(ctx context.Context, q Querier, b Budgets) error {
	now := r.AsOf
	type tk struct{ db, schema, rel string }
	sizes := make([]map[tk]float64, 3) // now, a day back, two days back
	for i := range sizes {
		res, err := q.Query(ctx, qTableBytes, now.Add(-time.Duration(i)*day))
		if err != nil {
			return err
		}
		sizes[i] = map[tk]float64{}
		for _, s := range res {
			sizes[i][tk{s.Labels["datname"], s.Labels["schemaname"], s.Labels["relname"]}] = s.Value
		}
	}
	for k, bytes := range sizes[0] {
		t := Table{Datname: k.db, Schema: k.schema, Rel: k.rel, Bytes: bytes}
		if prev, ok := sizes[1][k]; ok {
			t.Growth, t.HasGrowth = bytes-prev, true
			t.Over = b.tableOver(bytes, prev)
			if prev2, ok := sizes[2][k]; ok {
				t.PrevGrowth, t.HasPrevGrowth = prev-prev2, true
				t.PrevOver = b.tableOver(prev, prev2)
			}
		}
		r.Tables = append(r.Tables, t)
	}
	sort.Slice(r.Tables, func(i, j int) bool {
		a, c := r.Tables[i], r.Tables[j]
		if a.Growth != c.Growth {
			return a.Growth > c.Growth
		}
		return a.Name() < c.Name()
	})
	return nil
}

func (r *Report) gatherFired(ctx context.Context, q Querier) error {
	fired, err := q.Query(ctx, qFired, r.AsOf)
	if err != nil {
		return err
	}
	for _, f := range fired {
		labels := map[string]string{}
		for k, v := range f.Labels {
			if k != "alertname" && v != "" {
				labels[k] = v
			}
		}
		r.Fired = append(r.Fired, Fired{Alert: f.Labels["alertname"], Labels: labels})
	}
	sort.Slice(r.Fired, func(i, j int) bool { return r.Fired[i].String() < r.Fired[j].String() })
	return nil
}

// statementOver is the budgets one day of a statement broke: rules a and b
// of the chart, with the hourly floors scaled to a day.
func (b Budgets) statementOver(d Day, total float64) []string {
	var over []string
	if total > b.ShareMinTotalSeconds*24 && d.Share > b.ShareRatio {
		over = append(over, fmt.Sprintf("over %s of statement time", pct(b.ShareRatio)))
	}
	if d.Calls > b.SlowMinCallsPerHour*24 && d.Mean() > b.SlowMeanSeconds {
		over = append(over, fmt.Sprintf("mean over %s with over %.0f calls/h", secs(b.SlowMeanSeconds), b.SlowMinCallsPerHour))
	}
	return over
}

// tableOver is rule d of the chart for one day: from start bytes to end.
func (b Budgets) tableOver(end, start float64) []string {
	g := end - start
	var over []string
	if g > b.GrowthBytes {
		over = append(over, "grew over "+bytesIEC(b.GrowthBytes))
	}
	if g > b.GrowthRatio*start && end > b.GrowthMinBytes {
		over = append(over, "grew over "+pct(b.GrowthRatio))
	}
	return over
}

func (f Fired) String() string {
	keys := make([]string, 0, len(f.Labels))
	for k := range f.Labels {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var parts []string
	for _, k := range keys {
		if k == "budget" {
			continue
		}
		parts = append(parts, k+"="+f.Labels[k])
	}
	if len(parts) == 0 {
		return f.Alert
	}
	return f.Alert + " " + strings.Join(parts, " ")
}
