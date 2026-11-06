package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"gardenio/clicreds/oidclogin"
	"gardenio/controller/internal/localreg"
	"gardenio/controller/internal/runnerreg"
)

func TestJoinArgs_BuildsLowLevelAgentStart(t *testing.T) {
	args := joinArgs("gd-r1", "oidc:a@example.com", "pet", "/work", "laptop-a", "")
	want := []string{"agent", "start", "--local", "--role", "local", "--runner", "gd-r1", "--owner", "oidc:a@example.com", "--dir", "/work", "--project", "pet", "--name", "laptop-a"}
	if !slices.Equal(args, want) {
		t.Fatalf("got %v want %v", args, want)
	}
	withCmd := joinArgs("gd-r1", "o", "", ".", "", "claude --dangerously-skip-permissions")
	if withCmd[len(withCmd)-2] != "--command" {
		t.Fatalf("--command must be appended last: %v", withCmd)
	}
}

func TestJoinProject(t *testing.T) {
	t.Setenv("KD_PROJECT", "")
	t.Setenv("BOAT_PROJECT", "")
	cases := []struct {
		name, flag string
		allow      []string
		want       string
		wantErr    string
	}{
		{"sole project is the default", "", []string{"pet"}, "pet", ""},
		{"flag inside the allowlist", "b", []string{"a", "b"}, "b", ""},
		{"several projects need a flag", "", []string{"a", "b"}, "", "pass --project"},
		{"flag outside the allowlist", "c", []string{"a", "b"}, "", `not allowed project "c"`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := joinProject(tc.flag, tc.allow)
			if tc.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
					t.Fatalf("err = %v, want %q", err, tc.wantErr)
				}
				return
			}
			if err != nil || got != tc.want {
				t.Fatalf("got %q, %v; want %q", got, err, tc.want)
			}
		})
	}
	t.Run("KD_PROJECT counts as the flag", func(t *testing.T) {
		t.Setenv("KD_PROJECT", "b")
		if got, err := joinProject("", []string{"a", "b"}); err != nil || got != "b" {
			t.Fatalf("got %q, %v", got, err)
		}
	})
}

func TestJoinEnv_CarriesRunnerTokenOnlyUnderItsOwnName(t *testing.T) {
	parent := []string{"PATH=/bin", "BEADS_AUTH_TOKEN=owner-tok", runnerTokenEnv + "=stale", runnerURLEnv + "=stale"}
	env := joinEnv(parent, "https://runners.example.com", "gr_secret")
	if !slices.Contains(env, "BEADS_AUTH_TOKEN=owner-tok") || !slices.Contains(env, "PATH=/bin") {
		t.Fatalf("the owner's own env must pass through: %v", env)
	}
	if slices.Contains(env, runnerTokenEnv+"=stale") || slices.Contains(env, runnerURLEnv+"=stale") {
		t.Fatalf("inherited runner vars must be replaced: %v", env)
	}
	if !slices.Contains(env, runnerTokenEnv+"=gr_secret") || !slices.Contains(env, runnerURLEnv+"=https://runners.example.com") {
		t.Fatalf("runner token/url missing: %v", env)
	}
	for _, e := range env {
		if strings.Contains(e, "gr_secret") && !strings.HasPrefix(e, runnerTokenEnv+"=") {
			t.Fatalf("runner token leaked into %q", e)
		}
	}
}

func TestUseRunnerDaemon_UnsetsTheTokenBeforeTheSessionEnv(t *testing.T) {
	prev := daemon
	t.Cleanup(func() { daemon = prev })
	t.Setenv(runnerTokenEnv, "gr_secret")
	t.Setenv(runnerURLEnv, "http://127.0.0.1:1")
	if err := useRunnerDaemon(takeRunnerEnv()); err != nil {
		t.Fatal(err)
	}
	if daemon == nil || daemon == prev {
		t.Fatal("daemon client was not replaced with the runner client")
	}
	if _, ok := os.LookupEnv(runnerTokenEnv); ok {
		t.Fatalf("%s must be unset so localAgentEnv cannot copy it into coop", runnerTokenEnv)
	}
	for _, e := range localAgentEnv("a", "gd-a", "local", t.TempDir(), "http://127.0.0.1:2", "", localCreds{HTTPURL: "http://127.0.0.1:3", AuthToken: "owner-tok"}) {
		if strings.Contains(e, "gr_secret") {
			t.Fatalf("runner token reached the session env: %q", e)
		}
	}
}

// gd-DIMb73cPM5: a runner session runs on the runner token, read from its
// own 0600 file, and never on the owner's login session or a token the
// owner's shell exported.
func TestRunnerSessionCreds_SessionRunsOnTheRunnerToken(t *testing.T) {
	t.Setenv("GARDENIO_HOME", t.TempDir())
	ownerFile := t.TempDir() + "/owner.token"
	for k, v := range map[string]string{
		"GARDENIO_TOKEN":       "owner-gardenio",
		"KBEADS_TOKEN":         "owner-kbeads",
		"BEADS_AUTH_TOKEN":     "owner-beads",
		"BEADS_DAEMON_TOKEN":   "owner-daemon",
		tokenFileEnv:           ownerFile,
		"BEADS_HTTP_URL":       "https://owner.example.com/kd",
		oidclogin.NoSessionEnv: "",
	} {
		t.Setenv(k, v)
	}
	creds, remove, err := runnerSessionCreds("gr_secret", "https://runner.example.com")
	if err != nil {
		t.Fatal(err)
	}
	fi, err := os.Stat(creds.TokenFile)
	if err != nil {
		t.Fatal(err)
	}
	if fi.Mode().Perm() != 0o600 {
		t.Errorf("token file mode = %v, want 0600", fi.Mode().Perm())
	}
	if tok, err := readTokenFile(creds.TokenFile); err != nil || tok != "gr_secret" {
		t.Errorf("token file holds %q (%v), want the runner token", tok, err)
	}

	env := localAgentEnv("a", "gd-a", "local", t.TempDir(), "http://127.0.0.1:2", "", creds)
	for k, want := range map[string]string{
		tokenFileEnv:           creds.TokenFile,
		oidclogin.NoSessionEnv: "1",
		"BEADS_HTTP_URL":       "https://runner.example.com",
	} {
		if got := findEnv(env, k); got != want {
			t.Errorf("%s = %q, want %q", k, got, want)
		}
	}
	for _, e := range env {
		key, _, _ := strings.Cut(e, "=")
		switch key {
		case "GARDENIO_TOKEN", "KBEADS_TOKEN", "BEADS_AUTH_TOKEN", "BEADS_DAEMON_TOKEN", "COOP_NATS_TOKEN":
			t.Errorf("session env carries %s; only the token file may be its credential", e)
		}
		if strings.Contains(e, "gr_secret") {
			t.Errorf("runner token is in the session env: %q", e)
		}
	}

	remove()
	if _, err := os.Stat(creds.TokenFile); !os.IsNotExist(err) {
		t.Errorf("token file left after remove: %v", err)
	}
}

func TestRunnerSessionCreds_NeedsTheRunnerURL(t *testing.T) {
	t.Setenv("GARDENIO_HOME", t.TempDir())
	if _, _, err := runnerSessionCreds("gr_secret", ""); err == nil {
		t.Fatal("a runner token without its URL was accepted")
	}
}

func TestUseRunnerDaemon_NoTokenLeavesTheClientAlone(t *testing.T) {
	prev := daemon
	t.Cleanup(func() { daemon = prev })
	t.Setenv(runnerTokenEnv, "")
	if err := useRunnerDaemon(takeRunnerEnv()); err != nil || daemon != prev {
		t.Fatalf("err=%v replaced=%v", err, daemon != prev)
	}
}

func TestAgentStart_DropsAShellRunnerTokenWithoutRunnerFlag(t *testing.T) {
	// A GB_RUNNER_TOKEN exported in the user's shell, with no --runner: it
	// must be unset before coop starts, not left for localAgentEnv to copy.
	t.Setenv(runnerTokenEnv, "gr_shell")
	t.Setenv(runnerURLEnv, "http://127.0.0.1:1")
	src, err := os.ReadFile("agent_start.go")
	if err != nil {
		t.Fatal(err)
	}
	take := strings.Index(string(src), "takeRunnerEnv()")
	gate := strings.Index(string(src), `if runnerID != "" {`)
	if take < 0 || gate < 0 || take > gate {
		t.Fatal("agent_start.go must call takeRunnerEnv() before, not inside, the --runner check")
	}
	token, _ := takeRunnerEnv()
	if token != "gr_shell" {
		t.Fatalf("takeRunnerEnv returned %q", token)
	}
	for _, k := range []string{runnerTokenEnv, runnerURLEnv} {
		if _, ok := os.LookupEnv(k); ok {
			t.Fatalf("%s still set after takeRunnerEnv", k)
		}
	}
}

func TestRunnerHeartbeatLoop_StopsOnRejectedToken(t *testing.T) {
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if calls.Add(1) == 1 {
			w.WriteHeader(http.StatusInternalServerError) // transient: retried
			return
		}
		w.WriteHeader(http.StatusUnauthorized)
	}))
	defer srv.Close()
	var logs bytes.Buffer
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	err := runnerHeartbeatLoop(ctx, runnerreg.Client{BaseURL: srv.URL, Token: "gr_x"}, "gd-r1", 5*time.Millisecond, func() bool { return false }, &logs)
	if !errors.Is(err, runnerreg.ErrTokenRejected) {
		t.Fatalf("expected ErrTokenRejected, got %v", err)
	}
	if !strings.Contains(logs.String(), "will retry") {
		t.Fatalf("a transient failure must be logged and retried: %q", logs.String())
	}
}

func TestRunnerHeartbeatLoop_ReturnsNilWhenCancelled(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	defer srv.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
	defer cancel()
	if err := runnerHeartbeatLoop(ctx, runnerreg.Client{BaseURL: srv.URL}, "gd-r1", 5*time.Millisecond, func() bool { return false }, io.Discard); err != nil {
		t.Fatalf("got %v", err)
	}
}

func TestRunnerHeartbeatLoop_StopsMarkingOnlineAfterLeave(t *testing.T) {
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { calls.Add(1) }))
	defer srv.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	var ticks atomic.Int32
	left := func() bool { return ticks.Add(1) > 2 }
	var logs bytes.Buffer
	if err := runnerHeartbeatLoop(ctx, runnerreg.Client{BaseURL: srv.URL}, "gd-r1", 5*time.Millisecond, left, &logs); err != nil {
		t.Fatalf("got %v", err)
	}
	if ctx.Err() != nil {
		t.Fatal("the loop must return on leave, not wait for ctx")
	}
	if n := calls.Load(); n != 2 {
		t.Fatalf("heartbeats after leave: got %d calls, want 2", n)
	}
	if !strings.Contains(logs.String(), "no longer marking the runner online") {
		t.Fatalf("leave must be logged: %q", logs.String())
	}
}

func TestRunnerLeave_AgentStillRunningExitsNonZeroButMarksAway(t *testing.T) {
	home := t.TempDir()
	t.Setenv("GARDENIO_HOME", home)
	prevWait := runnerLeaveWait
	runnerLeaveWait = 20 * time.Millisecond
	t.Cleanup(func() { runnerLeaveWait = prevWait })

	// A child that ignores SIGTERM stands in for an agent that will not stop.
	stuck := exec.Command("sh", "-c", "trap '' TERM; sleep 30")
	if err := stuck.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = stuck.Process.Kill(); _ = stuck.Wait() })
	time.Sleep(50 * time.Millisecond) // let sh install the trap
	if _, err := localreg.Register(localreg.Entry{Name: "stuck-a", PID: stuck.Process.Pid, RunnerID: "gd-r1"}); err != nil {
		t.Fatal(err)
	}

	var states []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body struct{ State string }
		_ = json.NewDecoder(r.Body).Decode(&body)
		states = append(states, body.State)
	}))
	defer srv.Close()
	if err := runnerreg.Save(runnerreg.Runner{RunnerID: "gd-r1", DaemonURL: srv.URL}, "gr_x"); err != nil {
		t.Fatal(err)
	}

	before := time.Now().Add(-time.Second)
	var out, errOut bytes.Buffer
	cmd := runnerLeaveCmd
	cmd.SetOut(&out)
	cmd.SetErr(&errOut)
	cmd.SetContext(context.Background())
	t.Cleanup(func() { cmd.SetOut(nil); cmd.SetErr(nil) })
	err := runRunnerLeave(cmd, nil)
	if err == nil || !strings.Contains(err.Error(), "still running") || !strings.Contains(err.Error(), fmt.Sprint(stuck.Process.Pid)) {
		t.Fatalf("leave with a stuck agent must fail naming its pid, got %v", err)
	}
	if !slices.Equal(states, []string{"away"}) {
		t.Fatalf("leave must still mark the runner away: %v", states)
	}
	if !runnerreg.LeftSince(before) {
		t.Fatal("leave must touch the left marker so attached joins stop heartbeating")
	}
}

func TestRunnerLeave_NoAgentsExitsZero(t *testing.T) {
	t.Setenv("GARDENIO_HOME", t.TempDir())
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	defer srv.Close()
	if err := runnerreg.Save(runnerreg.Runner{RunnerID: "gd-r1", DaemonURL: srv.URL}, "gr_x"); err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	cmd := runnerLeaveCmd
	cmd.SetOut(&out)
	cmd.SetContext(context.Background())
	t.Cleanup(func() { cmd.SetOut(nil) })
	if err := runRunnerLeave(cmd, nil); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "Runner gd-r1 is away.") {
		t.Fatalf("unexpected output: %q", out.String())
	}
}

func TestRunnerEntries_FiltersByRunner(t *testing.T) {
	got := runnerEntries([]localreg.Entry{{Name: "a", RunnerID: "gd-r1"}, {Name: "b"}, {Name: "c", RunnerID: "gd-r2"}}, "gd-r1")
	if len(got) != 1 || got[0].Name != "a" {
		t.Fatalf("got %+v", got)
	}
}

func TestRunnerField(t *testing.T) {
	fields := map[string]any{"state": "online", "projects": []any{"a", "b"}}
	if runnerField(fields, "state") != "online" || runnerField(fields, "projects") != "a, b" || runnerField(fields, "missing") != "" || runnerField(nil, "state") != "" {
		t.Fatalf("runnerField wrong: %v", fields)
	}
}

func TestWaitForExit_ReturnsAtOnceForDeadPIDs(t *testing.T) {
	start := time.Now()
	if got := waitForExit(context.Background(), []localreg.Entry{{Name: "gone", PID: 0}}, time.Minute); len(got) != 0 {
		t.Fatalf("dead entry reported running: %+v", got)
	}
	if time.Since(start) > 5*time.Second {
		t.Fatal("waitForExit waited for an already-dead PID")
	}
	if got := waitForExit(context.Background(), []localreg.Entry{{Name: "me", PID: os.Getpid()}}, 10*time.Millisecond); len(got) != 1 {
		t.Fatalf("live entry must be returned after the timeout: %+v", got)
	}
}

func TestRunnerRegister_SavesAndNeverPrintsTheToken(t *testing.T) {
	home := t.TempDir()
	t.Setenv("GARDENIO_HOME", home)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusCreated)
		_ = json.NewEncoder(w).Encode(map[string]any{"runner_id": "gd-r1", "token": "gr_new", "owner": "oidc:a@example.com", "projects": []string{"pet", "zoo"}})
	}))
	defer srv.Close()

	var out bytes.Buffer
	cmd := runnerRegisterCmd
	cmd.SetOut(&out)
	cmd.SetContext(context.Background())
	t.Cleanup(func() {
		_ = cmd.Flags().Set("invite", "")
		_ = cmd.Flags().Set("daemon", "")
		cmd.SetOut(nil)
	})
	_ = cmd.Flags().Set("invite", "GRI-A")
	_ = cmd.Flags().Set("daemon", srv.URL+"/")
	if err := runRunnerRegister(cmd, nil); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(out.String(), "gr_new") {
		t.Fatalf("register printed the token: %q", out.String())
	}
	if !strings.Contains(out.String(), "as gd-r1 for oidc:a@example.com (projects: pet, zoo)") {
		t.Fatalf("unexpected output: %q", out.String())
	}
	reg, tok, err := runnerreg.Load()
	if err != nil || tok != "gr_new" || reg.DaemonURL != srv.URL {
		t.Fatalf("saved registration: %+v %q %v", reg, tok, err)
	}
}

func TestRunnerJoin_NotRegistered(t *testing.T) {
	t.Setenv("GARDENIO_HOME", t.TempDir())
	if err := runRunnerJoin(runnerJoinCmd, nil); !errors.Is(err, runnerreg.ErrNotRegistered) {
		t.Fatalf("expected ErrNotRegistered, got %v", err)
	}
}

func TestRunnerJoin_RevokedTokenFailsBeforeTheAgentStarts(t *testing.T) {
	t.Setenv("GARDENIO_HOME", t.TempDir())
	t.Setenv("KD_PROJECT", "")
	t.Setenv("BOAT_PROJECT", "")
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasSuffix(r.URL.Path, "/heartbeat") {
			t.Errorf("only a heartbeat may run before the child: %s", r.URL.Path)
		}
		w.WriteHeader(http.StatusUnauthorized)
	}))
	defer srv.Close()
	if err := runnerreg.Save(runnerreg.Runner{RunnerID: "gd-r1", DaemonURL: srv.URL, Projects: []string{"pet"}}, "gr_revoked"); err != nil {
		t.Fatal(err)
	}
	err := runRunnerJoin(runnerJoinCmd, nil)
	if !errors.Is(err, runnerreg.ErrTokenRejected) || err.Error() != "token revoked or expired; ask for a new invite" {
		t.Fatalf("got %v", err)
	}
}
