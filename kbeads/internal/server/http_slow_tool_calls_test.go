package server

import (
	"net/http"
	"testing"
	"time"

	"gardenio/kbeads/internal/model"
)

func TestHandleInsertSlowToolCall_PersistsRow(t *testing.T) {
	_, ms, h := newTestServer()
	body := map[string]any{
		"tool_name":        "Bash",
		"duration_ms":      4200,
		"source":           "claude",
		"cmd_class":        "git_push",
		"command_redacted": "git push origin main",
	}
	rec := doJSON(t, h, http.MethodPost, "/v1/metrics/slow-tool-calls", body)
	requireStatus(t, rec, http.StatusNoContent)

	if got, want := len(ms.slowToolCalls), 1; got != want {
		t.Fatalf("slowToolCalls rows = %d, want %d", got, want)
	}
	row := ms.slowToolCalls[0]
	if row.ToolName != "Bash" || row.CmdClass != "git_push" || row.DurationMs != 4200 {
		t.Errorf("unexpected row: %+v", row)
	}
	if row.ID == 0 {
		t.Error("expected ID assigned by store")
	}
}

func TestHandleInsertSlowToolCall_RejectsMissingToolName(t *testing.T) {
	_, _, h := newTestServer()
	rec := doJSON(t, h, http.MethodPost, "/v1/metrics/slow-tool-calls", map[string]any{
		"source": "claude",
	})
	requireStatus(t, rec, http.StatusBadRequest)
}

func TestHandleInsertSlowToolCall_RejectsBadSource(t *testing.T) {
	_, _, h := newTestServer()
	rec := doJSON(t, h, http.MethodPost, "/v1/metrics/slow-tool-calls", map[string]any{
		"tool_name": "Bash",
		"source":    "bogus",
	})
	requireStatus(t, rec, http.StatusBadRequest)
}

func TestHandleListSlowToolCalls_SlowestFirst(t *testing.T) {
	_, _, h := newTestServer()
	for _, ms := range []int64{1000, 9000, 3000} {
		body := map[string]any{"tool_name": "Bash", "source": "claude", "duration_ms": ms}
		rec := doJSON(t, h, http.MethodPost, "/v1/metrics/slow-tool-calls", body)
		requireStatus(t, rec, http.StatusNoContent)
	}
	rec := doJSON(t, h, http.MethodGet, "/v1/metrics/slow-tool-calls", nil)
	requireStatus(t, rec, http.StatusOK)
	var resp struct {
		Rows []*struct {
			DurationMs int64 `json:"duration_ms"`
		} `json:"rows"`
	}
	decodeJSON(t, rec, &resp)
	if len(resp.Rows) != 3 {
		t.Fatalf("rows=%d want 3", len(resp.Rows))
	}
	want := []int64{9000, 3000, 1000}
	for i, r := range resp.Rows {
		if r.DurationMs != want[i] {
			t.Errorf("rows[%d].DurationMs=%d want %d", i, r.DurationMs, want[i])
		}
	}
}

func TestHandleListSlowToolCalls_FiltersCmdClass(t *testing.T) {
	_, _, h := newTestServer()
	for _, cc := range []string{"git_push", "make", "git_push"} {
		body := map[string]any{"tool_name": "Bash", "source": "claude", "duration_ms": 2000, "cmd_class": cc}
		rec := doJSON(t, h, http.MethodPost, "/v1/metrics/slow-tool-calls", body)
		requireStatus(t, rec, http.StatusNoContent)
	}
	rec := doJSON(t, h, http.MethodGet, "/v1/metrics/slow-tool-calls?cmd_class=git_push", nil)
	requireStatus(t, rec, http.StatusOK)
	var resp struct {
		Rows []*struct {
			CmdClass string `json:"cmd_class"`
		} `json:"rows"`
	}
	decodeJSON(t, rec, &resp)
	if len(resp.Rows) != 2 {
		t.Fatalf("rows=%d want 2", len(resp.Rows))
	}
	for _, r := range resp.Rows {
		if r.CmdClass != "git_push" {
			t.Errorf("filter leaked: got %q", r.CmdClass)
		}
	}
}

func TestHandleListSlowToolCalls_BadLimit(t *testing.T) {
	_, _, h := newTestServer()
	rec := doJSON(t, h, http.MethodGet, "/v1/metrics/slow-tool-calls?limit=oops", nil)
	requireStatus(t, rec, http.StatusBadRequest)
}

func TestHandleListSlowToolCalls_SinceWindowFilter(t *testing.T) {
	_, ms, h := newTestServer()
	now := time.Now()
	ms.slowToolCalls = []*model.SlowToolCallRow{
		{ID: 1, ToolName: "Bash", DurationMs: 5000, TS: now.Add(-2 * time.Hour)},
		{ID: 2, ToolName: "Bash", DurationMs: 6000, TS: now.Add(-5 * time.Minute)},
	}
	rec := doJSON(t, h, http.MethodGet, "/v1/metrics/slow-tool-calls?since="+now.Add(-1*time.Hour).UTC().Format(time.RFC3339), nil)
	requireStatus(t, rec, http.StatusOK)
	var resp struct {
		Rows []*struct {
			ID int64 `json:"id"`
		} `json:"rows"`
	}
	decodeJSON(t, rec, &resp)
	if len(resp.Rows) != 1 || resp.Rows[0].ID != 2 {
		t.Fatalf("unexpected rows: %+v", resp.Rows)
	}
}
