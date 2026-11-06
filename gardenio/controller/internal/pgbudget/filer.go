package pgbudget

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"gardenio/controller/internal/beadsapi"
)

// MaxTasksPerDay caps the tasks the filer opens in any 24h.
const MaxTasksPerDay = 3

// Label and fields on every filed task. The key field is the dedupe key: one
// task per statement (datname + queryid) or table while it is open, or was
// closed in the last refileAfter (a fix that has not rolled yet).
const (
	TaskLabel    = "pg-budget"
	FieldKey     = "pg_budget_key"
	refileAfter  = 7 * 24 * time.Hour
	shortTitleMx = 40
)

// Store is the slice of the beads client the filer and runner use.
type Store interface {
	ListBeadsFiltered(ctx context.Context, q beadsapi.ListBeadsQuery) (*beadsapi.ListBeadsResult, error)
	CreateBead(ctx context.Context, req beadsapi.CreateBeadRequest) (string, error)
}

// Filer files a task per breach.
type Filer struct {
	Store Store
	// Land is the project the tasks are labelled with; Epic, when set, their
	// parent.
	Land, Epic string
	Actor      string
	Budgets    Budgets
}

// Filed is one task the filer opened.
type Filed struct {
	BeadID, Key, Subject string
}

// File opens a task for each breach not already tracked, heaviest first,
// until the day's cap. It returns what it filed and how many untracked
// breaches the cap held back.
func (f *Filer) File(ctx context.Context, r *Report, now time.Time) ([]Filed, int, error) {
	breaches := r.Breaches()
	if len(breaches) == 0 {
		return nil, 0, nil
	}
	recent, err := f.Store.ListBeadsFiltered(ctx, beadsapi.ListBeadsQuery{
		Types: []string{"task"}, Labels: []string{TaskLabel}, CreatedAfter: now.Add(-24 * time.Hour), Limit: 50,
	})
	if err != nil {
		return nil, 0, fmt.Errorf("pgbudget: count today's tasks: %w", err)
	}
	room := MaxTasksPerDay - len(recent.Beads)
	var filed []Filed
	capped := 0
	for _, b := range breaches {
		tracked, err := f.tracked(ctx, b.Key, now)
		if err != nil {
			return filed, capped, err
		}
		if tracked {
			continue
		}
		if room <= 0 {
			capped++
			continue
		}
		req := f.task(b, now)
		id, err := f.Store.CreateBead(ctx, req)
		if err != nil {
			return filed, capped, fmt.Errorf("pgbudget: file %s: %w", b.Key, err)
		}
		filed = append(filed, Filed{BeadID: id, Key: b.Key, Subject: subject(b)})
		room--
	}
	return filed, capped, nil
}

// tracked is whether a task for key is open, or closed within refileAfter.
func (f *Filer) tracked(ctx context.Context, key string, now time.Time) (bool, error) {
	for _, q := range []beadsapi.ListBeadsQuery{
		{Statuses: []string{"open", "in_progress", "blocked", "deferred"}},
		{Statuses: []string{"closed"}, ClosedAfter: now.Add(-refileAfter)},
	} {
		q.Labels, q.Fields, q.Limit, q.NoTotal = []string{TaskLabel}, map[string]string{FieldKey: key}, 1, true
		res, err := f.Store.ListBeadsFiltered(ctx, q)
		if err != nil {
			return false, fmt.Errorf("pgbudget: look up %s: %w", key, err)
		}
		if len(res.Beads) > 0 {
			return true, nil
		}
	}
	return false, nil
}

func subject(b Breach) string {
	if b.Statement != nil {
		return fmt.Sprintf("queryid %s (%s)", b.Statement.QueryID, b.Statement.Datname)
	}
	return b.Table.Name()
}

func (f *Filer) task(b Breach, now time.Time) beadsapi.CreateBeadRequest {
	var title, short, body string
	if s := b.Statement; s != nil {
		title = fmt.Sprintf("Postgres budget: make %s statement cheaper: %s", s.Datname, clip(oneLine(s.Query), 80))
		short = "cheapen " + s.Datname + ": " + oneLine(s.Query)
		body = f.statementBody(s)
	} else {
		t := b.Table
		title = fmt.Sprintf("Postgres budget: stop %s growing %s a day", t.Name(), bytesIEC(t.Growth))
		short = "curb growth of " + t.Rel
		body = f.tableBody(t)
	}
	fields, _ := json.Marshal(map[string]string{FieldKey: b.Key})
	req := beadsapi.CreateBeadRequest{
		Title:          title,
		ShortTitle:     clip(short, shortTitleMx),
		Type:           "task",
		Description:    body,
		Labels:         []string{"project:" + f.Land, TaskLabel},
		Priority:       2,
		CreatedBy:      f.Actor,
		Fields:         fields,
		IdempotencyKey: "pg-budget:" + b.Key + ":" + now.UTC().Format("2006-01-02"),
		ParentBeadID:   f.Epic,
	}
	return req
}

func (f *Filer) statementBody(s *Statement) string {
	var w strings.Builder
	fmt.Fprintf(&w, "The daily Postgres efficiency report found this statement over budget two days running (pgbudget, gd-ylgk1jBhLi).\n\n")
	fmt.Fprintf(&w, "- database: `%s`\n- queryid: `%s`\n", s.Datname, s.QueryID)
	fmt.Fprintf(&w, "- statement (normalized, literals stripped, first 120 characters):\n\n    %s\n\n", oneLine(s.Query))
	if len(s.Callers) > 0 {
		fmt.Fprintf(&w, "- caller (application_name seen running it in the last 48h): %s\n", strings.Join(s.Callers, ", "))
	} else {
		w.WriteString("- caller: no backend was caught running it at a scrape. For `beads`, `gardenio_beads_store_query_duration_seconds` by `method` ranks the kbeads store methods by time.\n")
	}
	w.WriteString("\n| | last 24h | the 24h before |\n|---|---|---|\n")
	fmt.Fprintf(&w, "| exec time | %s (%s of all) | %s (%s of all) |\n", secs(s.Today.ExecSeconds), pct(s.Today.Share), secs(s.Yesterday.ExecSeconds), pct(s.Yesterday.Share))
	fmt.Fprintf(&w, "| calls | %s | %s |\n", count(s.Today.Calls), count(s.Yesterday.Calls))
	fmt.Fprintf(&w, "| mean | %s | %s |\n", secs(s.Today.Mean()), secs(s.Yesterday.Mean()))
	fmt.Fprintf(&w, "| over budget | %s | %s |\n", strings.Join(s.Today.Over, "; "), strings.Join(s.Yesterday.Over, "; "))
	f.footer(&w, "the statement stays under budget for a day (the report stops listing it as over budget)")
	return w.String()
}

func (f *Filer) tableBody(t *Table) string {
	var w strings.Builder
	fmt.Fprintf(&w, "The daily Postgres efficiency report found this table over its growth budget two days running (pgbudget, gd-ylgk1jBhLi).\n\n")
	fmt.Fprintf(&w, "- table: `%s` (with indexes and TOAST)\n- size now: %s\n\n", t.Name(), bytesIEC(t.Bytes))
	w.WriteString("| | last 24h | the 24h before |\n|---|---|---|\n")
	fmt.Fprintf(&w, "| growth | %s | %s |\n", signedBytes(t.Growth), signedBytes(t.PrevGrowth))
	fmt.Fprintf(&w, "| over budget | %s | %s |\n", strings.Join(t.Over, "; "), strings.Join(t.PrevOver, "; "))
	w.WriteString("\nLook for a write path that stores more than it needs, or rows nothing prunes.\n")
	f.footer(&w, "a day's growth is back under budget")
	return w.String()
}

func (f *Filer) footer(w *strings.Builder, done string) {
	b := f.Budgets
	fmt.Fprintf(w, "\nBudgets (vmalert.postgresBudgets, per day): a statement over %s of statement time, or a mean over %s with over %.0f calls/h; a table growing over %s, or over %s above %s.\n",
		pct(b.ShareRatio), secs(b.SlowMeanSeconds), b.SlowMinCallsPerHour, bytesIEC(b.GrowthBytes), pct(b.GrowthRatio), bytesIEC(b.GrowthMinBytes))
	w.WriteString("Board: Grafana \"Postgres — Efficiency\" (uid gardenio-postgres-efficiency).\n")
	fmt.Fprintf(w, "Done when %s.\n", done)
}

func clip(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n-1]) + "…"
}
