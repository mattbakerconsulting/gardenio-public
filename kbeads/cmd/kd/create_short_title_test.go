package main

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"gardenio/kbeads/internal/client"
	"gardenio/kbeads/internal/model"
)

// shortTitleServer is a stub daemon that records the last write body and
// answers every POST/PATCH /v1/beads… with a bead, plus any warning headers.
func shortTitleServer(t *testing.T, warnings ...string) (*httptest.Server, *map[string]any) {
	t.Helper()
	var body map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasPrefix(r.URL.Path, "/v1/beads") {
			http.NotFound(w, r)
			return
		}
		raw, _ := io.ReadAll(r.Body)
		if r.Method == http.MethodPost || r.Method == http.MethodPatch {
			body = nil
			if err := json.Unmarshal(raw, &body); err != nil {
				t.Errorf("request body is not JSON: %v\n%s", err, raw)
			}
		}
		for _, msg := range warnings {
			w.Header().Add(client.WarningHeader, msg)
		}
		status := http.StatusOK
		if r.Method == http.MethodPost {
			status = http.StatusCreated
		}
		w.WriteHeader(status)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"id": "gd-st1", "title": "a task", "type": "task", "kind": "issue", "status": "open",
		})
	}))
	t.Cleanup(srv.Close)
	return srv, &body
}

// withHTTPClient points beadsClient at srv for the test.
func withHTTPClient(t *testing.T, srv *httptest.Server) {
	t.Helper()
	orig := beadsClient
	beadsClient = client.NewHTTPClient(srv.URL, "")
	t.Cleanup(func() { beadsClient = orig })
}

// resetCreateShortTitleFlags clears the create flags these tests touch, and
// blanks --parent-task / --project so an ambient $BOAT_TASK_ID or project
// does not add a dep call or a cross-project refusal.
func resetCreateShortTitleFlags(t *testing.T) {
	t.Helper()
	clear := func() {
		for _, name := range []string{"short-title", "parent-task", "project"} {
			_ = createCmd.Flags().Set(name, "")
			createCmd.Flags().Lookup(name).Changed = false
		}
	}
	clear()
	t.Cleanup(clear)
}

func TestCreate_ShortTitleFlagSent(t *testing.T) {
	srv, body := shortTitleServer(t)
	withHTTPClient(t, srv)
	resetCreateShortTitleFlags(t)
	if err := createCmd.Flags().Set("short-title", "forward duration_ms through the hook bus"); err != nil {
		t.Fatalf("set --short-title: %v", err)
	}

	_ = captureStdout(t, func() {
		if err := createCmd.RunE(createCmd, []string{"a task"}); err != nil {
			t.Fatalf("RunE: %v", err)
		}
	})

	if got := (*body)["short_title"]; got != "forward duration_ms through the hook bus" {
		t.Errorf("POST body short_title = %v, want the flag value; body=%v", got, *body)
	}
}

func TestCreate_NoShortTitleOmitsKey(t *testing.T) {
	srv, body := shortTitleServer(t)
	withHTTPClient(t, srv)
	resetCreateShortTitleFlags(t)

	_ = captureStdout(t, func() {
		if err := createCmd.RunE(createCmd, []string{"a task"}); err != nil {
			t.Fatalf("RunE: %v", err)
		}
	})

	if _, ok := (*body)["short_title"]; ok {
		t.Errorf("POST body carries short_title without --short-title: %v", *body)
	}
}

func TestCreate_WarningHeaderEchoed(t *testing.T) {
	srv, _ := shortTitleServer(t, "short_title missing: set one with kd update gd-st1 --short-title")
	withHTTPClient(t, srv)
	resetCreateShortTitleFlags(t)

	var stdout string
	stderr := captureStderr(t, func() {
		stdout = captureStdout(t, func() {
			if err := createCmd.RunE(createCmd, []string{"a task"}); err != nil {
				t.Fatalf("RunE: %v", err)
			}
		})
	})

	if !strings.Contains(stderr, "warning: short_title missing: set one with kd update gd-st1 --short-title\n") {
		t.Errorf("stderr = %q, want the warning echoed", stderr)
	}
	if strings.Contains(stdout, "warning") {
		t.Errorf("warning leaked onto stdout: %q", stdout)
	}
}

func TestUpdate_ShortTitleOnlyIfEmpty(t *testing.T) {
	srv, body := shortTitleServer(t)
	withHTTPClient(t, srv)
	resetUpdateFlags(t)
	resetUpdateShortTitleFlags(t)
	if err := updateCmd.Flags().Set("short-title", "fill the gap"); err != nil {
		t.Fatalf("set --short-title: %v", err)
	}
	if err := updateCmd.Flags().Set("only-if-short-title-empty", "true"); err != nil {
		t.Fatalf("set --only-if-short-title-empty: %v", err)
	}

	_ = captureStdout(t, func() {
		if err := updateCmd.RunE(updateCmd, []string{"gd-st1"}); err != nil {
			t.Fatalf("RunE: %v", err)
		}
	})

	if got := (*body)["short_title"]; got != "fill the gap" {
		t.Errorf("PATCH body short_title = %v, want fill the gap; body=%v", got, *body)
	}
	if got := (*body)["only_if_short_title_empty"]; got != true {
		t.Errorf("PATCH body only_if_short_title_empty = %v, want true; body=%v", got, *body)
	}
}

func TestUpdate_WarningHeaderEchoed(t *testing.T) {
	srv, _ := shortTitleServer(t, "short_title missing: first", "second")
	withHTTPClient(t, srv)
	resetUpdateFlags(t)
	resetUpdateShortTitleFlags(t)
	if err := updateCmd.Flags().Set("title", "renamed"); err != nil {
		t.Fatalf("set --title: %v", err)
	}

	stderr := captureStderr(t, func() {
		_ = captureStdout(t, func() {
			if err := updateCmd.RunE(updateCmd, []string{"gd-st1"}); err != nil {
				t.Fatalf("RunE: %v", err)
			}
		})
	})

	if want := "warning: short_title missing: first\nwarning: second\n"; !strings.Contains(stderr, want) {
		t.Errorf("stderr = %q, want both warnings in order", stderr)
	}
}

func TestUpdate_OnlyIfShortTitleEmptyNeedsShortTitle(t *testing.T) {
	mc := mockClientWithBead("gd-st1")
	withMockClient(t, mc)
	resetUpdateFlags(t)
	resetUpdateShortTitleFlags(t)
	if err := updateCmd.Flags().Set("only-if-short-title-empty", "true"); err != nil {
		t.Fatalf("set --only-if-short-title-empty: %v", err)
	}

	err := updateCmd.RunE(updateCmd, []string{"gd-st1"})
	if err == nil || !strings.Contains(err.Error(), "--short-title") {
		t.Fatalf("err = %v, want an error naming --short-title", err)
	}
	if len(mc.UpdateBeadCalls) != 0 {
		t.Errorf("UpdateBead called %d times, want 0", len(mc.UpdateBeadCalls))
	}
}

// resetUpdateShortTitleFlags clears the short-title flags on updateCmd.
func resetUpdateShortTitleFlags(t *testing.T) {
	t.Helper()
	clear := func() {
		for _, name := range []string{"short-title", "only-if-short-title-empty"} {
			f := updateCmd.Flags().Lookup(name)
			_ = f.Value.Set(f.DefValue)
			f.Changed = false
		}
	}
	clear()
	t.Cleanup(clear)
}

func TestShortTitleFlagHelp(t *testing.T) {
	for _, f := range []string{
		createCmd.Flags().Lookup("short-title").Usage,
		updateCmd.Flags().Lookup("short-title").Usage,
	} {
		if f != model.ShortTitleHelp {
			t.Errorf("--short-title help = %q, want model.ShortTitleHelp", f)
		}
	}
}

func TestPrintBeadTable_ShortTitleUnderTitle(t *testing.T) {
	out := captureStdout(t, func() {
		printBeadTable(&model.Bead{ID: "gd-1", Title: "the long title", ShortTitle: "short one"})
	})
	if !strings.Contains(out, "Title:       the long title\nShort title: short one\nType:") {
		t.Errorf("Short title line missing or misplaced:\n%s", out)
	}

	out = captureStdout(t, func() {
		printBeadTable(&model.Bead{ID: "gd-2", Title: "no short title"})
	})
	if strings.Contains(out, "Short title:") {
		t.Errorf("empty short_title should print no line:\n%s", out)
	}
}

func TestPrintBeadTableFiltered_ShortTitle(t *testing.T) {
	out := captureStdout(t, func() {
		printBeadTableFiltered(&model.Bead{ID: "gd-1", ShortTitle: "short one"}, []string{"short_title"})
	})
	if out != "Short title: short one\n" {
		t.Errorf("filtered output = %q", out)
	}
}

func TestPrintBeadListTable_ShortColumn(t *testing.T) {
	out := captureStdout(t, func() {
		printBeadListTable([]*model.Bead{
			{ID: "gd-1", Status: "open", Type: "task", Title: "has one", ShortTitle: "forward duration_ms through the hook bus"},
			{ID: "gd-2", Status: "open", Type: "task", Title: "has none"},
		}, 2)
	})
	lines := strings.Split(out, "\n")
	header := strings.Fields(lines[0])
	want := []string{"ID", "STATUS", "TYPE", "SHORT", "PRIORITY", "TITLE", "ASSIGNEE"}
	if strings.Join(header, " ") != strings.Join(want, " ") {
		t.Fatalf("header = %v, want %v", header, want)
	}
	if !strings.Contains(lines[1], "forward duration_ms through the hook bus") {
		t.Errorf("row 1 missing the full 40-char short title:\n%s", out)
	}
	if f := strings.Fields(lines[2]); len(f) < 4 || f[3] != "-" {
		t.Errorf("row 2 SHORT cell = %v, want -", f)
	}
}

func TestShortTitleCell_CutsOverMax(t *testing.T) {
	long := strings.Repeat("é", model.ShortTitleMax+5)
	got := shortTitleCell(long)
	if n := len([]rune(got)); n != model.ShortTitleMax {
		t.Errorf("cell is %d runes, want %d", n, model.ShortTitleMax)
	}
	if !strings.HasSuffix(got, "...") {
		t.Errorf("cell = %q, want a ... suffix", got)
	}
}

func TestCount_MissingShortTitleFlag(t *testing.T) {
	mc := newMockClient()
	withMockClient(t, mc)
	resetCountFlags(t)
	t.Cleanup(func() {
		f := countCmd.Flags().Lookup("missing-short-title")
		_ = f.Value.Set(f.DefValue)
		f.Changed = false
	})
	if err := countCmd.Flags().Set("missing-short-title", "true"); err != nil {
		t.Fatalf("set --missing-short-title: %v", err)
	}

	captureStdout(t, func() {
		if err := countCmd.RunE(countCmd, nil); err != nil {
			t.Fatalf("RunE: %v", err)
		}
	})

	if !mc.ListBeadsCalls[0].ShortTitleMissing {
		t.Error("ShortTitleMissing = false, want true")
	}
}
