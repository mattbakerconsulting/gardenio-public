package main

import (
	"encoding/json"
	"maps"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"
	"time"

	"gardenio/clicreds/oidclogin"
)

const agentID = "or-Agt7kL2mNp" // Agent/active.json and AgentGrant/minted.json

// fakeGB puts a gb on lookPath that records its arguments, environment and
// token file and exits with code. It returns the directory the records land
// in.
func fakeGB(t *testing.T, code int) string {
	t.Helper()
	return fakeGBThen(t, "exit "+string(rune('0'+code)))
}

// fakeGBThen is fakeGB running then (shell, with $rec the record directory)
// after it records.
func fakeGBThen(t *testing.T, then string) string {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("the fake gb is a shell script")
	}
	dir := t.TempDir()
	gb := filepath.Join(dir, "gb")
	script := "#!/bin/sh\nrec='" + dir + "'\nfor a in \"$@\"; do printf '%s\\n' \"$a\"; done > \"$rec/args\"\nenv > \"$rec/env\"\n" +
		"cp \"$GARDENIO_TOKEN_FILE\" \"$rec/token\"\n" + then + "\n"
	if err := os.WriteFile(gb, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	old := lookPath
	lookPath = func(name string) (string, error) {
		if name == "gb" {
			return gb, nil
		}
		return "/usr/bin/" + name, nil
	}
	t.Cleanup(func() { lookPath = old })
	return dir
}

func readLines(t *testing.T, path string) []string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("gb did not run: %v", err)
	}
	return strings.Split(strings.TrimRight(string(b), "\n"), "\n")
}

// agentLand is a land that starts agentID with its door at doorHost (the fake
// land's own host when "").
func agentLand(t *testing.T, doorHost string) *fakeLand {
	t.Helper()
	f := newFakeLand(t, "acme")
	if doorHost == "" {
		u, _ := url.Parse(f.srv.URL)
		doorHost = u.Host
	}
	var grant map[string]any
	_ = json.Unmarshal(fixture(t, "AgentGrant/minted.json"), &grant)
	grant["door_url"] = "https://" + doorHost + "/land/acme"
	f.on("POST /agents", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(201)
		_ = json.NewEncoder(w).Encode(grant)
	})
	f.answer("GET /agents/"+agentID, 200, "Agent/active.json")
	f.answer("POST /agents/"+agentID+"/revoke", 200, "Agent/stopped.json")
	return f
}

func routes(f *fakeLand) []string {
	var out []string
	for _, r := range f.requests() {
		out = append(out, r.method+" "+r.path[strings.LastIndex(r.path, "/acme")+len("/acme"):])
	}
	return out
}

func TestAgentStart_RunsGBOnTheAgentsOwnTokenAtTheDoor(t *testing.T) {
	useTempState(t)
	f := agentLand(t, "")
	signIn(t, "acme", host, oidclogin.Session{AccessToken: "member-tok"})
	rec := fakeGB(t, 0)
	// The member's own kd and gb setup must not reach the agent.
	t.Setenv("BEADS_AUTH_TOKEN", "member-tok")
	t.Setenv("GARDENIO_TOKEN", "member-tok")
	t.Setenv("KD_PROJECT", "elsewhere")
	t.Setenv("BOAT_AGENT_BEAD_ID", "gd-0123456789")
	t.Setenv(oidclogin.NoSessionEnv, "0")
	work := t.TempDir()

	r := run(t, runOpts{}, "--edge", f.srv.URL, "--land", "acme", "agent", "start", "--name", "studio", "--dir", work, "--ticket", "or-Tkt4nB8vCx")
	if r.e != nil {
		t.Fatalf("gl: %v\n%s", r.e, r.err)
	}

	if got, want := routes(f), []string{"POST /agents", "GET /agents/" + agentID, "POST /agents/" + agentID + "/revoke"}; !slices.Equal(got, want) {
		t.Fatalf("land saw %v, want %v (start, read its name, stop after gb)", got, want)
	}
	create := f.requests()[0]
	if create.header.Get("Authorization") != "Bearer member-tok" || create.header.Get("Idempotency-Key") == "" {
		t.Errorf("createAgent sent Authorization %q, Idempotency-Key %q", create.header.Get("Authorization"), create.header.Get("Idempotency-Key"))
	}
	if create.body != `{"name":"studio"}` {
		t.Errorf("createAgent body = %s", create.body)
	}

	args := readLines(t, filepath.Join(rec, "args"))
	want := []string{"--http-url", f.srv.URL + "/land/acme", "agent", "start", "--local",
		"--agent-id", agentID, "--name", "laptop-x7rq", "--role", "lands-gardener",
		"--project", "acme", "--dir", work, "--task", "or-Tkt4nB8vCx"}
	if !slices.Equal(args, want) {
		t.Errorf("gb args\n got %q\nwant %q", args, want)
	}

	var grant struct{ Bearer string }
	_ = json.Unmarshal(fixture(t, "AgentGrant/minted.json"), &grant)
	env := readLines(t, filepath.Join(rec, "env"))
	for _, kv := range []string{"GARDENIO_TOKEN=" + grant.Bearer, oidclogin.NoSessionEnv + "=1"} {
		if !slices.Contains(env, kv) {
			t.Errorf("gb env lacks %s", kv)
		}
	}
	if got := readLines(t, filepath.Join(rec, "token")); !slices.Equal(got, []string{grant.Bearer}) {
		t.Errorf("GARDENIO_TOKEN_FILE held %q, want the agent's token", got)
	}
	tokenFile := ""
	for _, kv := range env {
		if v, ok := strings.CutPrefix(kv, "GARDENIO_TOKEN_FILE="); ok {
			tokenFile = v
		}
	}
	if tokenFile == "" {
		t.Error("gb env names no GARDENIO_TOKEN_FILE")
	} else if _, err := os.Stat(filepath.Dir(tokenFile)); !os.IsNotExist(err) {
		t.Errorf("the token file's directory outlived the session: %v", err)
	}
	for _, kv := range env {
		k, v, _ := strings.Cut(kv, "=")
		if strings.Contains(v, "member-tok") {
			t.Errorf("gb env carries the member's token in %s", k)
		}
		for _, p := range []string{"BEADS_", "KD_", "BOAT_"} {
			if strings.HasPrefix(k, p) {
				t.Errorf("gb env keeps %s", kv)
			}
		}
		if k == oidclogin.NoSessionEnv && v != "1" {
			t.Errorf("gb env %s", kv)
		}
	}
	if !strings.Contains(r.err, "stopped agent "+agentID) {
		t.Errorf("no word that the agent was stopped:\n%s", r.err)
	}
}

func TestAgentStart_RefusesADoorAtAnotherHost(t *testing.T) {
	useTempState(t)
	f := agentLand(t, "evil.example")
	signIn(t, "acme", host, oidclogin.Session{})
	rec := fakeGB(t, 0)

	r := run(t, runOpts{}, "--edge", f.srv.URL, "--land", "acme", "agent", "start")
	if r.e == nil || !strings.Contains(r.e.Error(), "not sending the agent's credential") {
		t.Fatalf("err = %v, want the door refused", r.e)
	}
	if _, err := os.Stat(filepath.Join(rec, "args")); err == nil {
		t.Error("gb ran with a door gl did not choose")
	}
	if got := routes(f); got[len(got)-1] != "POST /agents/"+agentID+"/revoke" {
		t.Errorf("the started agent was not stopped: %v", got)
	}
}

func TestAgentStart_StopsTheAgentWhenGBFails(t *testing.T) {
	useTempState(t)
	f := agentLand(t, "")
	signIn(t, "acme", host, oidclogin.Session{})
	fakeGB(t, 3)

	r := run(t, runOpts{}, "--edge", f.srv.URL, "--land", "acme", "agent", "start", "--dir", t.TempDir())
	if r.e == nil || !strings.Contains(r.e.Error(), "exit status 3") {
		t.Fatalf("err = %v, want gb's exit", r.e)
	}
	if got := routes(f); got[len(got)-1] != "POST /agents/"+agentID+"/revoke" {
		t.Errorf("the agent was not stopped after gb failed: %v", got)
	}
}

func TestAgentStart_NoGBAsksTheLandNothing(t *testing.T) {
	useTempState(t)
	f := agentLand(t, "")
	signIn(t, "acme", host, oidclogin.Session{})
	old := lookPath
	lookPath = func(name string) (string, error) { return "", os.ErrNotExist }
	t.Cleanup(func() { lookPath = old })

	r := run(t, runOpts{}, "--edge", f.srv.URL, "--land", "acme", "agent", "start")
	if r.e == nil || !strings.Contains(r.e.Error(), "gb is not on PATH") {
		t.Fatalf("err = %v", r.e)
	}
	if n := len(f.requests()); n != 0 {
		t.Errorf("land saw %d requests; a missing gb must not take an agent slot", n)
	}
}

func TestAgentStart_RefusedInASupportView(t *testing.T) {
	useTempState(t)
	f := agentLand(t, "")
	signIn(t, "acme", host, oidclogin.Session{})
	fakeGB(t, 0)

	r := run(t, runOpts{}, "--edge", f.srv.URL, "--as-tenant", "acme", "agent", "start")
	if r.e == nil || !strings.Contains(r.e.Error(), "support view") {
		t.Fatalf("err = %v", r.e)
	}
	if n := len(f.requests()); n != 0 {
		t.Errorf("land saw %d requests", n)
	}
}

func TestAgentStop_RevokesOnTheMembersSignIn(t *testing.T) {
	useTempState(t)
	f := agentLand(t, "")
	signIn(t, "acme", host, oidclogin.Session{AccessToken: "member-tok"})

	r := run(t, runOpts{}, "--edge", f.srv.URL, "--land", "acme", "agent", "stop", agentID)
	if r.e != nil {
		t.Fatalf("gl: %v\n%s", r.e, r.err)
	}
	req := f.requests()[0]
	if req.header.Get("Authorization") != "Bearer member-tok" {
		t.Errorf("Authorization = %q", req.header.Get("Authorization"))
	}
	if !strings.Contains(r.out, "stopped") {
		t.Errorf("output:\n%s", r.out)
	}
}

func TestAgentList(t *testing.T) {
	useTempState(t)
	f := newFakeLand(t, "acme")
	f.answer("GET /agents", 200, "AgentPage/single.json")
	signIn(t, "acme", host, oidclogin.Session{})

	r := run(t, runOpts{}, "--edge", f.srv.URL, "--land", "acme", "agent", "list")
	if r.e != nil {
		t.Fatalf("gl: %v\n%s", r.e, r.err)
	}
	var p struct{ Items []struct{ Id, Name string } }
	_ = json.Unmarshal(fixture(t, "AgentPage/single.json"), &p)
	if !strings.Contains(r.out, p.Items[0].Id) || !strings.Contains(r.out, p.Items[0].Name) {
		t.Errorf("output does not list the agent:\n%s", r.out)
	}
}

func TestAgentEnv(t *testing.T) {
	got := agentEnv([]string{"PATH=/bin", "HOME=/h", "BEADS_HTTP_URL=x", "KBEADS_TOKEN=y", "GB_RUNNER_TOKEN=z",
		"COOP_NATS_TOKEN=n", "GARDENIO_TOKEN=m", "GARDENIO_TOKEN_FILE=/member", oidclogin.NoSessionEnv + "=0", "ANTHROPIC_API_KEY=k"}, "/t/token", "agent")
	want := []string{"PATH=/bin", "HOME=/h", "ANTHROPIC_API_KEY=k", "GARDENIO_TOKEN_FILE=/t/token", "GARDENIO_TOKEN=agent", oidclogin.NoSessionEnv + "=1"}
	if !slices.Equal(got, want) {
		t.Errorf("agentEnv\n got %q\nwant %q", got, want)
	}
}

// The token file is the owner's alone and replaced whole.
func TestAgentToken(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX modes")
	}
	tok, err := newAgentToken("first")
	if err != nil {
		t.Fatal(err)
	}
	if err := tok.write("second"); err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(tok.path); string(b) != "second\n" {
		t.Errorf("token file = %q", b)
	}
	for path, want := range map[string]os.FileMode{tok.dir: 0o700, tok.path: 0o600} {
		if st, err := os.Stat(path); err != nil || st.Mode().Perm() != want {
			t.Errorf("%s: mode %v (%v), want %v", path, st.Mode().Perm(), err, want)
		}
	}
	if names, _ := os.ReadDir(tok.dir); len(names) != 1 {
		t.Errorf("token dir holds %d files, want only the token", len(names))
	}
	tok.remove()
	if _, err := os.Stat(tok.dir); !os.IsNotExist(err) {
		t.Errorf("token dir not removed: %v", err)
	}
}

// renewingLand is agentLand whose token is due for renewal in moments; its
// refresh answers renewedBearer to whoever refresh admits.
func renewingLand(t *testing.T, admit func(*http.Request) bool) *fakeLand {
	t.Helper()
	old := agentRenewCheck
	agentRenewCheck = 10 * time.Millisecond
	t.Cleanup(func() { agentRenewCheck = old })
	f := agentLand(t, "")
	var grant map[string]any
	_ = json.Unmarshal(fixture(t, "AgentGrant/minted.json"), &grant)
	u, _ := url.Parse(f.srv.URL)
	grant["door_url"] = "https://" + u.Host + "/land/acme"
	first := maps.Clone(grant)
	first["expires_at"] = time.Now().Add(300 * time.Millisecond).UTC().Format(time.RFC3339Nano)
	f.on("POST /agents", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(201)
		_ = json.NewEncoder(w).Encode(first)
	})
	renewed := maps.Clone(grant)
	renewed["bearer"], renewed["refresh"] = renewedBearer, "refresh-2"
	renewed["expires_at"] = time.Now().Add(24 * time.Hour).UTC().Format(time.RFC3339Nano)
	f.on("POST /agents/"+agentID+"/refresh", func(w http.ResponseWriter, r *http.Request) {
		if !admit(r) {
			problem(w, t, 401, "Problem/unauthenticated.json")
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(renewed)
	})
	return f
}

const renewedBearer = "renewed-bearer"

// waitForRenewal is a fake gb's wait until the token file holds the renewed
// token (exit 0), or ten seconds (exit 4).
const waitForRenewal = `i=0
while [ $i -lt 1000 ]; do
  if [ "$(cat "$GARDENIO_TOKEN_FILE")" = "` + renewedBearer + `" ]; then cp "$GARDENIO_TOKEN_FILE" "$rec/renewed"; exit 0; fi
  sleep 0.01; i=$((i+1))
done
exit 4`

func TestAgentStart_RenewsTheTokenAtTheDoorWhileGBRuns(t *testing.T) {
	useTempState(t)
	var grant struct{ Bearer, Refresh string }
	_ = json.Unmarshal(fixture(t, "AgentGrant/minted.json"), &grant)
	f := renewingLand(t, func(r *http.Request) bool { return r.Header.Get("Authorization") == "Bearer "+grant.Bearer })
	signIn(t, "acme", host, oidclogin.Session{AccessToken: "member-tok"})
	rec := fakeGBThen(t, waitForRenewal)

	r := run(t, runOpts{}, "--edge", f.srv.URL, "--land", "acme", "agent", "start", "--dir", t.TempDir())
	if r.e != nil {
		t.Fatalf("gl: %v\n%s", r.e, r.err)
	}
	if got := readLines(t, filepath.Join(rec, "renewed")); !slices.Equal(got, []string{renewedBearer}) {
		t.Errorf("gb read %q from its token file, want the renewed token", got)
	}
	var refreshes []seen
	for _, q := range f.requests() {
		if strings.HasSuffix(q.path, "/refresh") {
			refreshes = append(refreshes, q)
		}
	}
	if len(refreshes) != 1 {
		t.Fatalf("land saw %d refreshes, want one: %v", len(refreshes), routes(f))
	}
	q := refreshes[0]
	if q.body != `{"refresh":"`+grant.Refresh+`"}` || q.header.Get("Idempotency-Key") == "" {
		t.Errorf("refresh sent body %s, Idempotency-Key %q", q.body, q.header.Get("Idempotency-Key"))
	}
	if strings.Contains(r.err, "could not") || strings.Contains(r.err, "will not") {
		t.Errorf("a renewal that worked said something:\n%s", r.err)
	}
}

// A machine that slept past the token's end renews on the member's sign-in.
func TestAgentStart_RenewsOnTheMembersSignInWhenTheAgentsTokenIsRefused(t *testing.T) {
	useTempState(t)
	f := renewingLand(t, func(r *http.Request) bool { return r.Header.Get("Authorization") == "Bearer member-tok" })
	signIn(t, "acme", host, oidclogin.Session{AccessToken: "member-tok"})
	rec := fakeGBThen(t, waitForRenewal)

	r := run(t, runOpts{}, "--edge", f.srv.URL, "--land", "acme", "agent", "start", "--dir", t.TempDir())
	if r.e != nil {
		t.Fatalf("gl: %v\n%s", r.e, r.err)
	}
	if got := readLines(t, filepath.Join(rec, "renewed")); !slices.Equal(got, []string{renewedBearer}) {
		t.Errorf("gb read %q from its token file, want the renewed token", got)
	}
}

// A land that will not renew (the agent was stopped) is said once, and gl
// stops asking.
func TestAgentStart_StopsRenewingWhenTheLandRefuses(t *testing.T) {
	useTempState(t)
	f := renewingLand(t, func(*http.Request) bool { return true })
	f.answer("POST /agents/"+agentID+"/refresh", 409, "Problem/conflict.json")
	signIn(t, "acme", host, oidclogin.Session{AccessToken: "member-tok"})
	fakeGBThen(t, "sleep 1")

	r := run(t, runOpts{}, "--edge", f.srv.URL, "--land", "acme", "agent", "start", "--dir", t.TempDir())
	if r.e != nil {
		t.Fatalf("gl: %v\n%s", r.e, r.err)
	}
	n := 0
	for _, q := range f.requests() {
		if strings.HasSuffix(q.path, "/refresh") {
			n++
		}
	}
	if n != 1 || strings.Count(r.err, "will not renew") != 1 {
		t.Errorf("refreshes = %d, want 1; routes %v; stderr:\n%s", n, routes(f), r.err)
	}
}
