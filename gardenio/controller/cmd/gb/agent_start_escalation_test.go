package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"gardenio/controller/internal/beadsapi"
	"gardenio/controller/internal/bridge"
	"gardenio/controller/internal/mailproto"
)

// Regression test for gd-DoIoK9llXd (second symptom): kbeads' GET
// /v1/escalations/{id} returns `escalation` as the extracted escalation
// fields (operation_type, approved_by, …) WITHOUT a bead id/title — the
// full bead lives under `issue`. The watcher previously required
// detail.Escalation.ID and bailed out with "escalation detail is nil",
// dropping the nudge entirely. The fix falls back to detail.Issue.
func TestEscalationWatcher_FallsBackToIssueWhenEscalationMetadataMissing(t *testing.T) {
	var mu sync.Mutex
	var nudged []string

	daemonServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Mirror the real kbeads response shape: `escalation` is just the
		// extracted fields map (no id/title), `issue` is the full bead.
		if r.URL.Path == "/v1/escalations/esc-shape" {
			json.NewEncoder(w).Encode(map[string]any{
				"escalation": map[string]any{
					"operation_type":   "create_role",
					"approved_by":      "user-1",
					"execution_target": "local",
				},
				"issue": map[string]any{
					"id":     "esc-shape",
					"title":  "Real-shape escalation",
					"type":   "escalation",
					"status": "in_progress",
					"fields": map[string]string{
						"operation_type":     "create_role",
						"approved_by":        "user-1",
						"execution_target":   "local",
						"execution_dispatch": "local",
					},
				},
			})
			return
		}
		if r.URL.Path == "/v1/escalations/esc-shape/result" {
			w.WriteHeader(http.StatusOK)
			return
		}
		if r.Method == "PATCH" {
			w.WriteHeader(http.StatusOK)
			return
		}
		http.NotFound(w, r)
	}))
	defer daemonServer.Close()

	coopServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/v1/agent" {
			json.NewEncoder(w).Encode(map[string]string{"state": "idle"})
			return
		}
		if r.URL.Path == "/api/v1/agent/nudge" {
			var body map[string]string
			json.NewDecoder(r.Body).Decode(&body)
			mu.Lock()
			nudged = append(nudged, body["message"])
			mu.Unlock()
			json.NewEncoder(w).Encode(map[string]any{"delivered": true})
			return
		}
		http.NotFound(w, r)
	}))
	defer coopServer.Close()

	daemon, err := beadsapi.New(beadsapi.Config{HTTPAddr: daemonServer.URL})
	if err != nil {
		t.Fatal(err)
	}
	defer daemon.Close()

	watcher := newEscalationWatcher(escalationWatcherConfig{
		Daemon:    daemon,
		CoopURL:   coopServer.URL,
		AgentName: "test-agent-shape",
		Logger:    slog.Default(),
	})

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	watcher.HandleControl(ctx, mailproto.ControlEscalationApproved, escalationApprovedMail("esc-shape"))

	deadline := time.After(2 * time.Second)
	for {
		mu.Lock()
		c := len(nudged)
		mu.Unlock()
		if c >= 1 {
			break
		}
		select {
		case <-deadline:
			t.Fatal("timed out waiting for nudge when only `issue` carried bead metadata")
		case <-time.After(50 * time.Millisecond):
		}
	}
	cancel()

	mu.Lock()
	defer mu.Unlock()
	if len(nudged) != 1 {
		t.Fatalf("expected 1 nudge, got %d", len(nudged))
	}
	if !strings.Contains(nudged[0], "esc-shape") {
		t.Errorf("nudge missing esc-shape: %s", nudged[0])
	}
	if !strings.Contains(nudged[0], "Real-shape escalation") {
		t.Errorf("nudge missing title from issue: %s", nudged[0])
	}
}

// Regression test for gd-DoIoK9llXd (third symptom): the escalation watcher
// fired the nudge the instant the SSE event arrived, racing the
// startup-prompt sequence. While coop was still in setup (bypass_permissions
// dialog), GetAgentState reported `setup` and POST /api/v1/agent/nudge
// returned 503 NotReady. The single-shot watcher dropped the nudge and the
// escalation stayed stuck in_progress until shutdown. The fix is a
// readiness gate + retry loop. This test simulates exactly that race:
//   - GetAgentState returns "setup" twice, then "idle" — exercises the gate.
//   - The nudge endpoint returns 503 once, then 200 delivered — exercises retry.
//
// Test-only knob overrides keep the runtime under a second.
func TestEscalationWatcher_WaitsForReadyAndRetriesNudge(t *testing.T) {
	// Tighten the knobs so the test doesn't burn its 5s budget polling.
	origReadyPoll := escalationReadyPollInterval
	origReadyMax := escalationReadyMaxPolls
	origNudgeDelay := escalationNudgeRetryDelay
	escalationReadyPollInterval = 20 * time.Millisecond
	escalationReadyMaxPolls = 100
	escalationNudgeRetryDelay = 20 * time.Millisecond
	t.Cleanup(func() {
		escalationReadyPollInterval = origReadyPoll
		escalationReadyMaxPolls = origReadyMax
		escalationNudgeRetryDelay = origNudgeDelay
	})

	var mu sync.Mutex
	var stateCalls, nudgeCalls int
	var nudged []string

	daemonServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/escalations/esc-race" {
			json.NewEncoder(w).Encode(map[string]any{
				"escalation": map[string]any{
					"id": "esc-race", "title": "Race fix", "type": "escalation",
					"status": "in_progress",
					"fields": map[string]string{
						"operation_type":     "create",
						"approved_by":        "user-1",
						"execution_target":   "local",
						"execution_dispatch": "local",
					},
				},
			})
			return
		}
		if r.URL.Path == "/v1/escalations/esc-race/result" {
			w.WriteHeader(http.StatusOK)
			return
		}
		if r.Method == "PATCH" {
			w.WriteHeader(http.StatusOK)
			return
		}
		http.NotFound(w, r)
	}))
	defer daemonServer.Close()

	coopServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/v1/agent" {
			mu.Lock()
			stateCalls++
			n := stateCalls
			mu.Unlock()
			// First two polls return setup (not ready); subsequent polls return idle.
			state := "idle"
			if n <= 2 {
				state = "setup"
			}
			json.NewEncoder(w).Encode(map[string]string{"state": state})
			return
		}
		if r.URL.Path == "/api/v1/agent/nudge" {
			mu.Lock()
			nudgeCalls++
			n := nudgeCalls
			mu.Unlock()
			// First attempt: simulate coop's 503 NotReady response.
			if n == 1 {
				w.WriteHeader(http.StatusServiceUnavailable)
				json.NewEncoder(w).Encode(map[string]any{"delivered": false, "reason": "agent not ready"})
				return
			}
			var body map[string]string
			json.NewDecoder(r.Body).Decode(&body)
			mu.Lock()
			nudged = append(nudged, body["message"])
			mu.Unlock()
			json.NewEncoder(w).Encode(map[string]any{"delivered": true})
			return
		}
		http.NotFound(w, r)
	}))
	defer coopServer.Close()

	daemon, err := beadsapi.New(beadsapi.Config{HTTPAddr: daemonServer.URL})
	if err != nil {
		t.Fatal(err)
	}
	defer daemon.Close()

	watcher := newEscalationWatcher(escalationWatcherConfig{
		Daemon:    daemon,
		CoopURL:   coopServer.URL,
		AgentName: "test-agent-race",
		Logger:    slog.Default(),
	})

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	watcher.HandleControl(ctx, mailproto.ControlEscalationApproved, escalationApprovedMail("esc-race"))

	deadline := time.After(3 * time.Second)
	for {
		mu.Lock()
		c := len(nudged)
		mu.Unlock()
		if c >= 1 {
			break
		}
		select {
		case <-deadline:
			mu.Lock()
			t.Fatalf("timed out waiting for nudge — state polls=%d, nudge attempts=%d", stateCalls, nudgeCalls)
		case <-time.After(20 * time.Millisecond):
		}
	}
	cancel()

	mu.Lock()
	defer mu.Unlock()
	if stateCalls < 3 {
		t.Errorf("expected >=3 state polls (gate waits past setup), got %d", stateCalls)
	}
	if nudgeCalls < 2 {
		t.Errorf("expected >=2 nudge attempts (first returns 503, second succeeds), got %d", nudgeCalls)
	}
	if len(nudged) != 1 {
		t.Fatalf("expected exactly 1 successfully delivered nudge, got %d", len(nudged))
	}
	if !strings.Contains(nudged[0], "esc-race") {
		t.Errorf("nudge missing esc-race: %s", nudged[0])
	}
}

// Regression test for gd-X8q4N828KL: after the nudge landed, the watcher
// POSTed /v1/escalations/{id}/result with result_status=success, and that
// endpoint closes the escalation (and a success unblocks its dependents), so
// "the laptop got the prompt" was recorded as "the operation ran". Delivery
// must leave the escalation open, recording only a non-terminal result.
func TestEscalationWatcher_DeliveryLeavesEscalationOpen(t *testing.T) {
	var mu sync.Mutex
	var nudges, resultPosts int
	var patches []map[string]any

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/v1/escalations/esc-open":
			json.NewEncoder(w).Encode(map[string]any{
				"issue": map[string]any{
					"id": "esc-open", "title": "Live check", "type": "escalation",
					"status": "in_progress",
					"fields": map[string]string{
						"operation_type": "live_check_runbook", "approved_by": "user-1",
						"execution_target":   "local",
						"execution_dispatch": "local",
					},
				},
			})
		case r.URL.Path == "/v1/escalations/esc-open/result":
			mu.Lock()
			resultPosts++
			mu.Unlock()
			w.WriteHeader(http.StatusOK)
		case r.URL.Path == "/v1/beads/esc-open" && r.Method == http.MethodGet:
			json.NewEncoder(w).Encode(map[string]any{
				"id": "esc-open", "type": "escalation", "status": "in_progress",
				"fields": map[string]any{"approved_by": "user-1", "execution_target": "local"},
			})
		case r.URL.Path == "/v1/beads/esc-open" && r.Method == http.MethodPatch:
			var body struct {
				Fields map[string]any `json:"fields"`
			}
			json.NewDecoder(r.Body).Decode(&body)
			mu.Lock()
			patches = append(patches, body.Fields)
			mu.Unlock()
			w.WriteHeader(http.StatusOK)
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()

	coopServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/v1/agent" {
			json.NewEncoder(w).Encode(map[string]string{"state": "idle"})
			return
		}
		if r.URL.Path == "/api/v1/agent/nudge" {
			mu.Lock()
			nudges++
			mu.Unlock()
			json.NewEncoder(w).Encode(map[string]any{"delivered": true})
			return
		}
		http.NotFound(w, r)
	}))
	defer coopServer.Close()

	daemon, err := beadsapi.New(beadsapi.Config{HTTPAddr: srv.URL})
	if err != nil {
		t.Fatal(err)
	}
	defer daemon.Close()

	watcher := newEscalationWatcher(escalationWatcherConfig{
		Daemon: daemon, CoopURL: coopServer.URL,
		AgentName: "matthew-laptop", Logger: slog.Default(),
	})

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	watcher.HandleControl(ctx, mailproto.ControlEscalationApproved, escalationApprovedMail("esc-open"))

	// Wait for the delivery record (the PATCH carrying prompt_delivered_at),
	// which lands after the nudge.
	deadline := time.After(2 * time.Second)
	for {
		mu.Lock()
		var delivered map[string]any
		for _, p := range patches {
			if _, ok := p["prompt_delivered_at"]; ok {
				delivered = p
			}
		}
		mu.Unlock()
		if delivered != nil {
			break
		}
		select {
		case <-deadline:
			t.Fatal("timed out waiting for the prompt-delivery record")
		case <-time.After(20 * time.Millisecond):
		}
	}
	cancel()

	mu.Lock()
	defer mu.Unlock()
	if nudges != 1 {
		t.Errorf("expected 1 nudge, got %d", nudges)
	}
	if resultPosts != 0 {
		t.Errorf("watcher POSTed the result endpoint %d time(s); that closes the escalation on mere delivery", resultPosts)
	}
	var rec map[string]any
	for _, p := range patches {
		if _, ok := p["prompt_delivered_at"]; ok {
			rec = p
		}
		// result and result_status are the daemon's (spec D8, P7 Task 11).
		if _, ok := p["result"]; ok {
			t.Errorf("delivery wrote result: %v", p)
		}
		if _, ok := p["result_status"]; ok {
			t.Errorf("delivery wrote result_status (terminal): %v", p)
		}
		if _, ok := p["status"]; ok {
			t.Errorf("delivery changed status: %v", p)
		}
	}
	// The daemon stamps executor at approval (P7 Task 13); the watcher never
	// writes it.
	for _, p := range patches {
		if _, ok := p["executor"]; ok {
			t.Errorf("watcher wrote executor: %v", p)
		}
	}
	if rec["prompt_delivered_to"] != "matthew-laptop" {
		t.Errorf("delivery record = %v, want prompt_delivered_at and prompt_delivered_to", rec)
	}
}

func TestBuildEscalationPrompt(t *testing.T) {
	watcher := &escalationWatcher{}
	detail := &beadsapi.EscalationDetail{
		Escalation: &beadsapi.BeadDetail{
			ID:    "esc-test-1",
			Title: "Create S3 bucket",
			Fields: map[string]string{
				"operation_type": "aws_s3_create_bucket",
				"operation_spec": `{"bucket":"my-bucket","region":"us-east-1"}`,
				"context":        "Need bucket for logs",
				"approved_by":    "user-admin",
			},
		},
	}

	prompt := watcher.buildEscalationPrompt(detail)

	checks := []string{
		"ESCALATION APPROVED",
		"esc-test-1",
		"Create S3 bucket",
		"aws_s3_create_bucket",
		"my-bucket",
		"Need bucket for logs",
		"user-admin",
		"gb escalation close esc-test-1",
	}

	for _, check := range checks {
		if !strings.Contains(prompt, check) {
			t.Errorf("prompt missing %q:\n%s", check, prompt)
		}
	}
}

// TestEscalationCloseCommandResolvesInGB pins gd-aTfaK8HgTV: the report
// command both local nudges name (bridge.EscalationCloseCommand) must be a
// real gb subcommand with the flags it passes. The nudge once said
// `gb escalation result`, which gb never had, so cobra printed the group
// help and the escalation stayed in_progress forever.
func TestEscalationCloseCommandResolvesInGB(t *testing.T) {
	if err := gbCommandProblem(bridge.EscalationCloseCommand("gd-x"), "gd-x"); err != nil {
		t.Fatal(err)
	}
	// Positive control: the verb the nudge used to name must fail the check.
	if err := gbCommandProblem("gb escalation result gd-x --status <s> --result '<j>'", "gd-x"); err == nil {
		t.Fatal("gbCommandProblem accepted `gb escalation result`, which gb does not have")
	}
	if err := gbCommandProblem("gb escalation close gd-x --outcome <s>", "gd-x"); err == nil {
		t.Fatal("gbCommandProblem accepted an undefined --outcome flag")
	}
}

// TestEscalationReportCommandResolvesInGB pins gd-1MT3ZL6vqN: the worked
// success/failure lines the escalation execution prompts show
// (bridge.EscalationReportCommand) resolve to gb escalation close with real
// flags.
func TestEscalationReportCommandResolvesInGB(t *testing.T) {
	for _, status := range []string{"success", "failure"} {
		cmd := bridge.EscalationReportCommand("gd-x", status, `{"output":"<summary of what was done>"}`)
		if err := gbCommandProblem(cmd, "gd-x"); err != nil {
			t.Fatal(err)
		}
	}
}

// gbCommandProblem checks that cmdline ("gb <verbs...> <id> --flag v ...")
// resolves to a runnable gb subcommand whose path is every word before id,
// and that each --flag it passes is defined on that subcommand.
func gbCommandProblem(cmdline, id string) error {
	words := strings.Fields(cmdline)
	if len(words) == 0 || words[0] != "gb" {
		return fmt.Errorf("command %q does not start with gb", cmdline)
	}
	idx := slices.Index(words, id)
	if idx < 2 {
		return fmt.Errorf("command %q does not name a subcommand before %s", cmdline, id)
	}
	path := words[1:idx]
	c, rest, err := rootCmd.Find(path)
	if err != nil || len(rest) != 0 || !c.Runnable() {
		return fmt.Errorf("%q: gb has no runnable %q (resolved %q, unresolved %v, err %v)",
			cmdline, strings.Join(path, " "), c.CommandPath(), rest, err)
	}
	for _, w := range words[idx+1:] {
		if name, ok := strings.CutPrefix(w, "--"); ok && c.Flags().Lookup(name) == nil {
			return fmt.Errorf("%q: %s has no --%s flag", cmdline, c.CommandPath(), name)
		}
	}
	return nil
}

// escalationApprovedMail is the control:escalation-approved mail the daemon
// sends the executor of an approved local escalation (spec §8.1).
func escalationApprovedMail(escalationID string) beadsapi.BeadDetail {
	return beadsapi.BeadDetail{
		ID:     "gd-mail-" + escalationID,
		Type:   "mail",
		Status: "open",
		Labels: []string{"from:kbeads", mailproto.ControlLabel(mailproto.ControlEscalationApproved)},
		Fields: map[string]string{"escalation_id": escalationID},
	}
}

func TestEscalationWatcher_HandleControl_RunsOnceForEscalationApproved(t *testing.T) {
	ran := make(chan string, 2)
	release := make(chan struct{})
	w := &escalationWatcher{processing: map[string]bool{}, logger: slog.Default(), execute: func(_ context.Context, id string) {
		ran <- id
		<-release
	}}
	mail := escalationApprovedMail("gd-e1")
	w.HandleControl(context.Background(), mailproto.ControlEscalationApproved, mail)
	w.HandleControl(context.Background(), mailproto.ControlEscalationApproved, mail)
	select {
	case id := <-ran:
		if id != "gd-e1" {
			t.Fatalf("ran %q", id)
		}
	case <-time.After(time.Second):
		t.Fatal("executor not invoked")
	}
	select {
	case <-ran:
		t.Fatal("duplicate control mail must not run the escalation twice")
	case <-time.After(50 * time.Millisecond):
	}
	close(release)
}

// Only escalation-approved mail runs an escalation, and only one that names
// an escalation_id.
func TestEscalationWatcher_HandleControl_IgnoresOtherMail(t *testing.T) {
	ran := make(chan string, 2)
	w := &escalationWatcher{processing: map[string]bool{}, logger: slog.Default(), execute: func(_ context.Context, id string) { ran <- id }}
	w.HandleControl(context.Background(), mailproto.ControlKill, escalationApprovedMail("gd-e1"))
	w.HandleControl(context.Background(), mailproto.ControlEscalationApproved, beadsapi.BeadDetail{ID: "gd-m2"})
	select {
	case id := <-ran:
		t.Fatalf("ran %q from mail that names no approved escalation", id)
	case <-time.After(50 * time.Millisecond):
	}
}

// The mail subscriber has one OnControl hook: escalation-approved goes to the
// watcher, everything else to the permission handler.
func TestLocalControlHandler_RoutesByVerb(t *testing.T) {
	var got []string
	h := localControlHandler(
		func(_ context.Context, verb string, _ beadsapi.BeadDetail) { got = append(got, "watcher:"+verb) },
		func(_ context.Context, verb string, _ beadsapi.BeadDetail) { got = append(got, "other:"+verb) },
	)
	h(context.Background(), mailproto.ControlEscalationApproved, beadsapi.BeadDetail{})
	h(context.Background(), mailproto.ControlPermission, beadsapi.BeadDetail{})
	if want := []string{"watcher:escalation-approved", "other:permission"}; !slices.Equal(got, want) {
		t.Fatalf("routed %v, want %v", got, want)
	}
	// No watcher: escalation-approved falls through to the other handler,
	// which logs it as unhandled.
	got = nil
	localControlHandler(nil, func(_ context.Context, verb string, _ beadsapi.BeadDetail) { got = append(got, "other:"+verb) })(
		context.Background(), mailproto.ControlEscalationApproved, beadsapi.BeadDetail{})
	if want := []string{"other:escalation-approved"}; !slices.Equal(got, want) {
		t.Fatalf("routed %v, want %v", got, want)
	}
}

// Handled escalation-approved mail is closed stamped control_acted, and mail
// redrained for an escalation already delivered (a session restarted before
// the close landed) does not nudge the session again.
func TestEscalationWatcher_ClosesMailAndSkipsDelivered(t *testing.T) {
	var mu sync.Mutex
	var nudges int
	closes := map[string]map[string]any{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/v1/escalations/esc-done":
			json.NewEncoder(w).Encode(map[string]any{"issue": map[string]any{
				"id": "esc-done", "title": "Already run", "type": "escalation", "status": "in_progress",
				"fields": map[string]string{"execution_target": "local", "execution_dispatch": "local", "prompt_delivered_at": "2026-11-01T20:00:00Z", "prompt_delivered_to": "matthew-laptop"},
			}})
		case strings.HasSuffix(r.URL.Path, "/close") && r.Method == http.MethodPost:
			var body map[string]any
			json.NewDecoder(r.Body).Decode(&body)
			mu.Lock()
			closes[strings.TrimSuffix(strings.TrimPrefix(r.URL.Path, "/v1/beads/"), "/close")] = body
			mu.Unlock()
			w.WriteHeader(http.StatusOK)
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()
	coopServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		nudges++
		mu.Unlock()
		json.NewEncoder(w).Encode(map[string]any{"state": "idle", "delivered": true})
	}))
	defer coopServer.Close()
	daemon, err := beadsapi.New(beadsapi.Config{HTTPAddr: srv.URL})
	if err != nil {
		t.Fatal(err)
	}
	defer daemon.Close()

	watcher := newEscalationWatcher(escalationWatcherConfig{Daemon: daemon, CoopURL: coopServer.URL, AgentName: "matthew-laptop", Logger: slog.Default()})
	watcher.HandleControl(context.Background(), mailproto.ControlEscalationApproved, escalationApprovedMail("esc-done"))

	deadline := time.After(2 * time.Second)
	for {
		mu.Lock()
		body, ok := closes["gd-mail-esc-done"]
		mu.Unlock()
		if ok {
			if body[mailproto.FieldControlActed] == nil {
				t.Fatalf("mail closed without %s: %v", mailproto.FieldControlActed, body)
			}
			break
		}
		select {
		case <-deadline:
			t.Fatal("handled escalation-approved mail was not closed")
		case <-time.After(20 * time.Millisecond):
		}
	}
	mu.Lock()
	defer mu.Unlock()
	if nudges != 0 {
		t.Fatalf("escalation already delivered was nudged again (%d coop calls)", nudges)
	}
}

// The mail's escalation_id is not proof of approval (the daemon's control mail
// guard admits more senders than the approval path): an escalation that is
// not approved, not dispatched locally, or approved for another runner or
// executor is refused. The mail is closed, the session is not nudged, and no
// result is posted on an escalation that may not be this agent's.
func TestEscalationWatcher_HandleControl_RefusesUnapprovedEscalation(t *testing.T) {
	approved := map[string]string{"execution_target": "local", "execution_dispatch": "local", "target_runner": "gd-r1", "executor": "matthew-laptop"}
	with := func(k, v string) map[string]string {
		f := map[string]string{}
		for key, val := range approved {
			f[key] = val
		}
		f[k] = v
		return f
	}
	for _, tc := range []struct {
		name   string
		status string
		fields map[string]string
		runs   bool
	}{
		{"approved for this runner and agent (control)", "in_progress", approved, true},
		{"open, not approved", "open", approved, false},
		{"dispatch not local", "in_progress", with("execution_dispatch", "service_account"), false},
		{"no dispatch claim", "in_progress", with("execution_dispatch", ""), false},
		{"another runner", "in_progress", with("target_runner", "gd-r2"), false},
		{"another executor", "in_progress", with("executor", "someone-else"), false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var mu sync.Mutex
			var nudges, results, patches int
			closed := make(chan string, 1)
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				switch {
				case r.URL.Path == "/v1/escalations/esc-x":
					json.NewEncoder(w).Encode(map[string]any{"issue": map[string]any{
						"id": "esc-x", "title": "Forged", "type": "escalation", "status": tc.status, "fields": tc.fields,
					}})
				case strings.HasSuffix(r.URL.Path, "/result"):
					mu.Lock()
					results++
					mu.Unlock()
					w.WriteHeader(http.StatusOK)
				case strings.HasSuffix(r.URL.Path, "/close") && r.Method == http.MethodPost:
					closed <- strings.TrimSuffix(strings.TrimPrefix(r.URL.Path, "/v1/beads/"), "/close")
					w.WriteHeader(http.StatusOK)
				case r.URL.Path == "/v1/beads/esc-x" && r.Method == http.MethodGet:
					json.NewEncoder(w).Encode(map[string]any{"id": "esc-x", "type": "escalation", "status": tc.status, "fields": tc.fields})
				case r.Method == http.MethodPatch:
					mu.Lock()
					patches++
					mu.Unlock()
					w.WriteHeader(http.StatusOK)
				default:
					http.NotFound(w, r)
				}
			}))
			defer srv.Close()
			coopServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path == "/api/v1/agent/nudge" {
					mu.Lock()
					nudges++
					mu.Unlock()
				}
				json.NewEncoder(w).Encode(map[string]any{"state": "idle", "delivered": true})
			}))
			defer coopServer.Close()
			daemon, err := beadsapi.New(beadsapi.Config{HTTPAddr: srv.URL})
			if err != nil {
				t.Fatal(err)
			}
			defer daemon.Close()

			watcher := newEscalationWatcher(escalationWatcherConfig{Daemon: daemon, CoopURL: coopServer.URL, AgentName: "matthew-laptop", RunnerID: "gd-r1", Logger: slog.Default()})
			watcher.HandleControl(context.Background(), mailproto.ControlEscalationApproved, escalationApprovedMail("esc-x"))
			select {
			case id := <-closed:
				if id != "gd-mail-esc-x" {
					t.Fatalf("closed %q", id)
				}
			case <-time.After(2 * time.Second):
				t.Fatal("escalation-approved mail was not closed")
			}
			mu.Lock()
			defer mu.Unlock()
			if tc.runs {
				if nudges != 1 || results != 0 || patches != 1 {
					t.Fatalf("approved escalation: %d nudges, %d result posts, %d patches; want 1, 0, 1", nudges, results, patches)
				}
				return
			}
			if nudges != 0 || results != 0 || patches != 0 {
				t.Fatalf("refused escalation acted on: %d nudges, %d result posts, %d patches", nudges, results, patches)
			}
		})
	}
}
