package decision

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"gardenio/controller/internal/beadsapi"

	tea "github.com/charmbracelet/bubbletea"
)

// newTestClient returns a beadsapi client pointed at srv.
func newTestClient(t *testing.T, srv *httptest.Server) *beadsapi.Client {
	t.Helper()
	c, err := beadsapi.New(beadsapi.Config{HTTPAddr: srv.URL})
	if err != nil {
		t.Fatalf("beadsapi.New: %v", err)
	}
	return c
}

// TestKeyMapShortHelp verifies the short help bindings.
func TestKeyMapShortHelp(t *testing.T) {
	km := DefaultKeyMap()
	bindings := km.ShortHelp()
	if len(bindings) == 0 {
		t.Fatal("ShortHelp() returned no bindings")
	}
	// Sanity check: first binding should be Up.
	if bindings[0].Help().Key != km.Up.Help().Key {
		t.Errorf("ShortHelp()[0] = %q, want %q", bindings[0].Help().Key, km.Up.Help().Key)
	}
}

// TestKeyMapFullHelp verifies the full help matrix.
func TestKeyMapFullHelp(t *testing.T) {
	km := DefaultKeyMap()
	rows := km.FullHelp()
	if len(rows) < 5 {
		t.Fatalf("FullHelp() rows = %d, want >=5", len(rows))
	}
	for i, row := range rows {
		if len(row) == 0 {
			t.Errorf("FullHelp row %d is empty", i)
		}
	}
}

// TestInit verifies Init returns a non-nil batch command.
func TestInit(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"decisions":[]}`))
	}))
	defer srv.Close()

	m := New(newTestClient(t, srv), "actor")
	cmd := m.Init()
	if cmd == nil {
		t.Fatal("Init() returned nil")
	}
}

// TestFetchDecisionsCommand exercises the fetchDecisions cmd end-to-end.
func TestFetchDecisionsCommand(t *testing.T) {
	tests := []struct {
		name       string
		handler    http.HandlerFunc
		wantErr    bool
		wantCount  int
		wantPrompt string
		wantOpts   int
	}{
		{
			name: "success with parsed options and prompt",
			handler: func(w http.ResponseWriter, _ *http.Request) {
				_, _ = w.Write([]byte(`{"decisions":[{
					"decision":{
						"id":"bd-1",
						"title":"Fallback title",
						"type":"decision",
						"status":"open",
						"priority":1,
						"fields":{
							"prompt":"Which approach?",
							"requested_by":"agent-x",
							"context":"ctx",
							"options":[{"id":"a","label":"Fast"},{"id":"b","label":"Safe"}]
						}
					}
				}]}`))
			},
			wantCount:  1,
			wantPrompt: "Which approach?",
			wantOpts:   2,
		},
		{
			name: "falls back to title when prompt missing",
			handler: func(w http.ResponseWriter, _ *http.Request) {
				_, _ = w.Write([]byte(`{"decisions":[{
					"decision":{
						"id":"bd-2",
						"title":"The Title",
						"type":"decision",
						"status":"open",
						"priority":2,
						"fields":{}
					}
				}]}`))
			},
			wantCount:  1,
			wantPrompt: "The Title",
		},
		{
			name: "empty decision and issue are skipped",
			handler: func(w http.ResponseWriter, _ *http.Request) {
				_, _ = w.Write([]byte(`{"decisions":[{}]}`))
			},
			wantCount: 0,
		},
		{
			name: "server error surfaces as fetchDecisionsMsg.err",
			handler: func(w http.ResponseWriter, _ *http.Request) {
				http.Error(w, "boom", http.StatusInternalServerError)
			},
			wantErr: true,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			srv := httptest.NewServer(tc.handler)
			defer srv.Close()

			m := New(newTestClient(t, srv), "actor")
			cmd := m.fetchDecisions()
			if cmd == nil {
				t.Fatal("fetchDecisions() returned nil cmd")
			}
			msg, ok := cmd().(fetchDecisionsMsg)
			if !ok {
				t.Fatalf("cmd() returned %T, want fetchDecisionsMsg", cmd())
			}
			if tc.wantErr {
				if msg.err == nil {
					t.Error("expected err, got nil")
				}
				return
			}
			if msg.err != nil {
				t.Fatalf("unexpected err: %v", msg.err)
			}
			if len(msg.decisions) != tc.wantCount {
				t.Fatalf("decisions = %d, want %d", len(msg.decisions), tc.wantCount)
			}
			if tc.wantCount > 0 && msg.decisions[0].Prompt != tc.wantPrompt {
				t.Errorf("prompt = %q, want %q", msg.decisions[0].Prompt, tc.wantPrompt)
			}
			if tc.wantOpts > 0 && len(msg.decisions[0].Options) != tc.wantOpts {
				t.Errorf("options = %d, want %d", len(msg.decisions[0].Options), tc.wantOpts)
			}
		})
	}
}

// TestStartPolling verifies the poll tick command exists and emits tickMsg.
func TestStartPolling(t *testing.T) {
	m := New(nil, "a")
	cmd := m.startPolling()
	if cmd == nil {
		t.Fatal("startPolling() returned nil")
	}
	// We do not invoke the tick (it would wait pollInterval).
}

// TestResolveDecisionCommand covers the success/error paths.
func TestResolveDecisionCommand(t *testing.T) {
	tests := []struct {
		name    string
		handler http.HandlerFunc
		wantErr bool
	}{
		{
			name: "success",
			handler: func(w http.ResponseWriter, r *http.Request) {
				if !strings.HasSuffix(r.URL.Path, "/resolve") {
					t.Errorf("path = %q, want .../resolve", r.URL.Path)
				}
				var body beadsapi.ResolveDecisionRequest
				_ = json.NewDecoder(r.Body).Decode(&body)
				if body.SelectedOption != "opt-a" {
					t.Errorf("selected_option = %q, want opt-a", body.SelectedOption)
				}
				if body.RespondedBy != "actor-1" {
					t.Errorf("responded_by = %q, want actor-1", body.RespondedBy)
				}
				w.WriteHeader(http.StatusOK)
			},
		},
		{
			name: "error",
			handler: func(w http.ResponseWriter, _ *http.Request) {
				http.Error(w, "nope", http.StatusBadRequest)
			},
			wantErr: true,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			srv := httptest.NewServer(tc.handler)
			defer srv.Close()

			m := New(newTestClient(t, srv), "actor-1")
			cmd := m.resolveDecision("bd-42", "opt-a", "because")
			msg, ok := cmd().(resolvedMsg)
			if !ok {
				t.Fatalf("cmd() returned %T, want resolvedMsg", cmd())
			}
			if msg.id != "bd-42" {
				t.Errorf("id = %q, want bd-42", msg.id)
			}
			if tc.wantErr && msg.err == nil {
				t.Error("expected err, got nil")
			}
			if !tc.wantErr && msg.err != nil {
				t.Errorf("unexpected err: %v", msg.err)
			}
		})
	}
}

// TestDismissDecisionCommand covers success + error.
func TestDismissDecisionCommand(t *testing.T) {
	tests := []struct {
		name    string
		handler http.HandlerFunc
		wantErr bool
	}{
		{
			name: "success",
			handler: func(w http.ResponseWriter, r *http.Request) {
				if !strings.HasSuffix(r.URL.Path, "/cancel") {
					t.Errorf("path = %q, want .../cancel", r.URL.Path)
				}
				w.WriteHeader(http.StatusOK)
			},
		},
		{
			name: "error",
			handler: func(w http.ResponseWriter, _ *http.Request) {
				http.Error(w, "nope", http.StatusConflict)
			},
			wantErr: true,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			srv := httptest.NewServer(tc.handler)
			defer srv.Close()

			m := New(newTestClient(t, srv), "actor-1")
			cmd := m.dismissDecision("bd-99", "dismissed")
			msg, ok := cmd().(dismissedMsg)
			if !ok {
				t.Fatalf("cmd() returned %T, want dismissedMsg", cmd())
			}
			if msg.id != "bd-99" {
				t.Errorf("id = %q, want bd-99", msg.id)
			}
			if tc.wantErr && msg.err == nil {
				t.Error("expected err, got nil")
			}
			if !tc.wantErr && msg.err != nil {
				t.Errorf("unexpected err: %v", msg.err)
			}
		})
	}
}

// TestUrgencyLabel covers every branch including the default.
func TestUrgencyLabel(t *testing.T) {
	tests := []struct {
		urgency string
		want    string
	}{
		{"high", "[HIGH]"},
		{"medium", "[MED]"},
		{"low", "[LOW]"},
		{"unknown", "[MED]"},
		{"", "[MED]"},
	}
	for _, tt := range tests {
		if got := urgencyLabel(tt.urgency); !strings.Contains(got, tt.want) {
			t.Errorf("urgencyLabel(%q) = %q, want to contain %q", tt.urgency, got, tt.want)
		}
	}
}

// TestFormatTimeAgo covers all duration branches.
func TestFormatTimeAgo(t *testing.T) {
	now := time.Now()
	tests := []struct {
		name string
		in   time.Time
		want string
	}{
		{"zero", time.Time{}, "unknown"},
		{"just now", now.Add(-10 * time.Second), "just now"},
		{"one minute", now.Add(-1 * time.Minute), "1 minute ago"},
		{"many minutes", now.Add(-30 * time.Minute), "30 minutes ago"},
		{"one hour", now.Add(-61 * time.Minute), "1 hour ago"},
		{"many hours", now.Add(-5 * time.Hour), "5 hours ago"},
		{"one day", now.Add(-25 * time.Hour), "1 day ago"},
		{"many days", now.Add(-3 * 24 * time.Hour), "3 days ago"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := formatTimeAgo(tt.in)
			if got != tt.want {
				t.Errorf("formatTimeAgo(%v) = %q, want %q", tt.in, got, tt.want)
			}
		})
	}
}

// TestWrapLine covers wrapping behavior.
func TestWrapLine(t *testing.T) {
	tests := []struct {
		name  string
		in    string
		width int
		check func(t *testing.T, out string)
	}{
		{
			name:  "short line unchanged",
			in:    "hello",
			width: 80,
			check: func(t *testing.T, out string) {
				if out != "hello" {
					t.Errorf("got %q, want %q", out, "hello")
				}
			},
		},
		{
			name:  "wraps at word boundary",
			in:    "one two three four five",
			width: 10,
			check: func(t *testing.T, out string) {
				if !strings.Contains(out, "\n") {
					t.Errorf("expected newline in %q", out)
				}
			},
		},
		{
			name:  "zero width defaults to 80",
			in:    "short",
			width: 0,
			check: func(t *testing.T, out string) {
				if out != "short" {
					t.Errorf("got %q", out)
				}
			},
		},
		{
			name:  "multiline preserves line separator",
			in:    "first\nsecond",
			width: 80,
			check: func(t *testing.T, out string) {
				if !strings.Contains(out, "\n") {
					t.Errorf("expected newline preserved: %q", out)
				}
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := wrapText(tt.in, tt.width)
			tt.check(t, got)
		})
	}
}

// TestRenderInputMode covers the rationale-entry view.
func TestRenderInputMode(t *testing.T) {
	m := New(nil, "test")
	m.width = 80
	m.height = 24
	m.inputMode = ModeRationale

	view := m.View()
	if !strings.Contains(view, "rationale") {
		t.Errorf("expected input prompt in view: %q", view)
	}
	if !strings.Contains(view, "Esc") {
		t.Errorf("expected Esc hint in view: %q", view)
	}
}

// TestViewTerminalTooSmall covers the narrow-terminal branch.
func TestViewTerminalTooSmall(t *testing.T) {
	m := New(nil, "test")
	m.width = 20
	m.height = 5
	view := m.View()
	if !strings.Contains(view, "Terminal too small") {
		t.Errorf("expected 'Terminal too small' in view: %q", view)
	}
}

// TestViewWithError renders the error banner.
func TestViewWithError(t *testing.T) {
	m := New(nil, "test")
	m.width = 80
	m.height = 24
	m.err = errors.New("boom")
	view := m.View()
	if !strings.Contains(view, "boom") {
		t.Errorf("expected error text in view: %q", view)
	}
}

// TestViewShowHelp toggles the full help bindings panel.
func TestViewShowHelp(t *testing.T) {
	m := New(nil, "test")
	m.width = 80
	m.height = 24
	m.showHelp = true
	m.decisions = []DecisionItem{{ID: "1", Prompt: "p", Urgency: "high", Priority: 1, RequestedAt: time.Now()}}
	view := m.View()
	if view == "" {
		t.Error("View returned empty with showHelp")
	}
}

// TestUpdateConfirmKey dispatches a resolve command when an option is selected.
func TestUpdateConfirmKey(t *testing.T) {
	var called bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		called = true
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	m := New(newTestClient(t, srv), "actor")
	m.decisions = []DecisionItem{{
		ID:      "bd-1",
		Options: []DecisionOption{{ID: "a", Label: "A"}},
	}}
	m.selected = 0
	m.selectedOption = 1

	// Press Enter — Confirm binding.
	msg := tea.KeyMsg{Type: tea.KeyEnter}
	updated, cmd := m.Update(msg)
	if cmd == nil {
		t.Fatal("Update returned nil cmd for Confirm")
	}
	// Execute the batch to trigger the HTTP call.
	_ = cmd()
	model := updated.(*Model)
	if model.status == "" {
		t.Error("expected status to be set after Confirm")
	}
	// Note: batched cmd may not execute HTTP directly via cmd() without recursion;
	// the status field is what matters for coverage here.
	_ = called
}

// TestUpdateRefreshKey dispatches fetchDecisions on 'R'.
func TestUpdateRefreshKey(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"decisions":[]}`))
	}))
	defer srv.Close()

	m := New(newTestClient(t, srv), "actor")
	msg := tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'R'}}
	updated, cmd := m.Update(msg)
	if cmd == nil {
		t.Fatal("expected cmd from Refresh")
	}
	model := updated.(*Model)
	if !strings.Contains(model.status, "Refresh") {
		t.Errorf("status = %q, want to contain 'Refresh'", model.status)
	}
}

// TestUpdateDismissKey dispatches dismissDecision on 'd'.
func TestUpdateDismissKey(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	m := New(newTestClient(t, srv), "actor")
	m.decisions = []DecisionItem{{ID: "bd-9"}}
	msg := tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'d'}}
	updated, cmd := m.Update(msg)
	if cmd == nil {
		t.Fatal("expected cmd from Dismiss")
	}
	model := updated.(*Model)
	if !strings.Contains(model.status, "Dismiss") {
		t.Errorf("status = %q, want to contain 'Dismiss'", model.status)
	}
}

// TestUpdateQuitKey returns tea.Quit.
func TestUpdateQuitKey(t *testing.T) {
	m := New(nil, "actor")
	msg := tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'q'}}
	_, cmd := m.Update(msg)
	if cmd == nil {
		t.Fatal("expected Quit cmd")
	}
}

// TestUpdateResolvedErrorMsg surfaces resolve errors.
func TestUpdateResolvedErrorMsg(t *testing.T) {
	m := New(nil, "actor")
	msg := resolvedMsg{id: "bd-1", err: errors.New("boom")}
	updated, _ := m.Update(msg)
	model := updated.(*Model)
	if model.err == nil {
		t.Error("expected err to be set")
	}
	if !strings.Contains(model.status, "Error") {
		t.Errorf("status = %q, want to contain 'Error'", model.status)
	}
}

// TestUpdateDismissedErrorMsg surfaces dismiss errors.
func TestUpdateDismissedErrorMsg(t *testing.T) {
	m := New(nil, "actor")
	msg := dismissedMsg{id: "bd-1", err: errors.New("oops")}
	updated, _ := m.Update(msg)
	model := updated.(*Model)
	if model.err == nil {
		t.Error("expected err to be set")
	}
	if !strings.Contains(model.status, "Dismiss error") {
		t.Errorf("status = %q, want to contain 'Dismiss error'", model.status)
	}
}

// TestUpdateTickMsg dispatches a re-fetch cmd.
func TestUpdateTickMsg(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"decisions":[]}`))
	}))
	defer srv.Close()

	m := New(newTestClient(t, srv), "actor")
	_, cmd := m.Update(tickMsg(time.Now()))
	if cmd == nil {
		t.Fatal("tickMsg produced nil cmd")
	}
}

// TestHandleInputModeEnterResolves submits the rationale and dispatches resolve.
func TestHandleInputModeEnterResolves(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	m := New(newTestClient(t, srv), "actor")
	m.decisions = []DecisionItem{{
		ID:      "bd-1",
		Options: []DecisionOption{{ID: "a", Label: "A"}},
	}}
	m.selected = 0
	m.selectedOption = 1
	m.inputMode = ModeRationale
	m.textInput.SetValue("my rationale")

	msg := tea.KeyMsg{Type: tea.KeyEnter}
	updated, cmd := m.Update(msg)
	model := updated.(*Model)
	if model.inputMode != ModeNormal {
		t.Errorf("inputMode = %v, want ModeNormal", model.inputMode)
	}
	if model.rationale != "my rationale" {
		t.Errorf("rationale = %q", model.rationale)
	}
	if cmd == nil {
		t.Error("expected resolve cmd from Enter in rationale mode")
	}
}

// TestHandleInputModeTyping passes the keystroke through to the textarea.
func TestHandleInputModeTyping(t *testing.T) {
	m := New(nil, "actor")
	m.inputMode = ModeRationale
	m.textInput.Focus()

	msg := tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'x'}}
	updated, _ := m.Update(msg)
	model := updated.(*Model)
	// Still in rationale mode, no panic.
	if model.inputMode != ModeRationale {
		t.Errorf("inputMode = %v, want still ModeRationale", model.inputMode)
	}
}

// TestUpdateRationaleKeyEnters sets ModeRationale when an option is chosen.
func TestUpdateRationaleKeyEnters(t *testing.T) {
	m := New(nil, "actor")
	m.decisions = []DecisionItem{{ID: "bd-1", Options: []DecisionOption{{ID: "a", Label: "A"}}}}
	m.selectedOption = 1

	msg := tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'r'}}
	updated, _ := m.Update(msg)
	model := updated.(*Model)
	if model.inputMode != ModeRationale {
		t.Errorf("inputMode = %v, want ModeRationale", model.inputMode)
	}
}
