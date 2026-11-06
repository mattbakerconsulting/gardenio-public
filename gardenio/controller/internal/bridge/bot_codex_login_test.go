package bridge

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"gardenio/controller/internal/beadsapi"

	"github.com/slack-go/slack"
)

const codexReloginSpec = `{"action":"codex_device_login","account":"codex-main","secret":"gardenio-codex-auth","rejected_fingerprint":"ab12"}`

func seedCodexRelogin(daemon *mockDaemon, id, status string) {
	daemon.mu.Lock()
	defer daemon.mu.Unlock()
	daemon.beads[id] = &beadsapi.BeadDetail{
		ID: id, Type: "escalation", Status: status,
		Fields: map[string]string{"operation_type": "credential_provisioning", "operation_spec": codexReloginSpec},
	}
}

func seedOperator(daemon *mockDaemon, slackID, caps string) {
	daemon.mu.Lock()
	defer daemon.mu.Unlock()
	daemon.beads["user-"+slackID] = &beadsapi.BeadDetail{
		ID: "user-" + slackID, Title: "Op", Type: "user", Kind: "config", Status: "open",
		Fields: map[string]string{"slack_id": slackID, "email": slackID + "@example.com", "capabilities": caps},
	}
}

type fakeController struct {
	srv   *httptest.Server
	mu    sync.Mutex
	calls int
	got   map[string]string
	auth  string
	code  int
}

func newFakeController(t *testing.T, code int) *fakeController {
	t.Helper()
	fc := &fakeController{code: code}
	fc.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fc.mu.Lock()
		fc.calls++
		fc.auth = r.Header.Get("Authorization")
		_ = json.NewDecoder(r.Body).Decode(&fc.got)
		code := fc.code
		fc.mu.Unlock()
		w.WriteHeader(code)
		if code == http.StatusOK {
			_, _ = w.Write([]byte(`{"verification_url":"https://auth.openai.com/codex/device","user_code":"ABCD-1234","expires_at":"2026-10-16T07:00:00Z"}`))
		} else {
			_, _ = w.Write([]byte(`{"error":"Secret x does not exist"}`))
		}
	}))
	t.Cleanup(fc.srv.Close)
	return fc
}

func pressStart(beadID, user string, rc *responseURLCapture) slack.InteractionCallback {
	cb := slack.InteractionCallback{Type: slack.InteractionTypeBlockActions}
	cb.User.ID = user
	cb.Channel.ID = "C1"
	cb.ResponseURL = rc.srv.URL
	cb.ActionCallback.BlockActions = []*slack.BlockAction{{ActionID: codexDeviceLoginActionID, Value: beadID}}
	return cb
}

func TestCodexDeviceLoginAccountParsesOnlyTheReloginSpec(t *testing.T) {
	if a, ok := codexDeviceLoginAccount(map[string]string{"operation_spec": codexReloginSpec}); !ok || a != "codex-main" {
		t.Fatalf("got %q %v", a, ok)
	}
	for _, f := range []map[string]string{
		nil, {"operation_spec": `{"action":"merge","account":"x"}`}, {"operation_spec": `not json`}, {"operation_spec": `{"action":"codex_device_login"}`},
	} {
		if _, ok := codexDeviceLoginAccount(f); ok {
			t.Errorf("%v must not match", f)
		}
	}
	if codexDeviceLoginButton(BeadEvent{ID: "x", Fields: map[string]string{}}) != nil {
		t.Fatal("button for a non-relogin escalation")
	}
	if b := codexDeviceLoginButton(BeadEvent{ID: "gd-esc", Fields: map[string]string{"operation_spec": codexReloginSpec}}); b == nil || b.Value != "gd-esc" {
		t.Fatal("no button for a relogin escalation")
	}
}

func TestStartButtonShowsCodeOnlyToThePresserAndNeverLogsOrStoresIt(t *testing.T) {
	t.Setenv("CONTROLLER_API_TOKEN", "tok")
	daemon := newMockDaemon()
	seedCodexRelogin(daemon, "gd-esc", "open")
	seedOperator(daemon, "UOP", `["approve:escalation"]`)
	fc := newFakeController(t, http.StatusOK)
	slackSrv := newFakeSlackServer(t)
	defer slackSrv.Close()
	rc := newResponseURLCapture(t)
	bot := newTestBot(daemon, slackSrv)
	bot.controllerURL = fc.srv.URL

	bot.handleBlockActions(context.Background(), pressStart("gd-esc", "UOP", rc))

	posts := rc.all()
	if len(posts) != 1 || posts[0].ResponseType != slack.ResponseTypeEphemeral ||
		!strings.Contains(posts[0].Text, "ABCD-1234") || !strings.Contains(posts[0].Text, "auth.openai.com/codex/device") {
		t.Fatalf("expected one ephemeral with URL and code, got %+v", posts)
	}
	if fc.got["account"] != "codex-main" || fc.got["escalation_id"] != "gd-esc" || fc.got["started_by"] != "UOP@example.com" || fc.auth != "Bearer tok" {
		t.Fatalf("controller request: %v auth=%q", fc.got, fc.auth)
	}
	// The code must not reach any bead write.
	daemon.mu.Lock()
	defer daemon.mu.Unlock()
	for id, b := range daemon.beads {
		for k, v := range b.Fields {
			if strings.Contains(v, "ABCD-1234") {
				t.Fatalf("code persisted on bead %s field %s", id, k)
			}
		}
	}
}

func TestStartButtonRefusesWithoutCapabilityOrForOtherBeads(t *testing.T) {
	daemon := newMockDaemon()
	seedCodexRelogin(daemon, "gd-esc", "open")
	seedCodexRelogin(daemon, "gd-done", "closed")
	daemon.mu.Lock()
	daemon.beads["gd-other"] = &beadsapi.BeadDetail{ID: "gd-other", Type: "escalation", Status: "open", Fields: map[string]string{"operation_spec": `{"action":"merge"}`}}
	daemon.mu.Unlock()
	seedOperator(daemon, "UOP", `["approve:escalation"]`)
	seedOperator(daemon, "UNOCAP", `["mention"]`)
	fc := newFakeController(t, http.StatusOK)
	slackSrv := newFakeSlackServer(t)
	defer slackSrv.Close()
	bot := newTestBot(daemon, slackSrv)
	bot.controllerURL = fc.srv.URL

	for name, tc := range map[string]struct{ bead, user string }{
		"no capability": {"gd-esc", "UNOCAP"},
		"unknown user":  {"gd-esc", "UNKNOWN"},
		"closed":        {"gd-done", "UOP"},
		"not a relogin": {"gd-other", "UOP"},
	} {
		t.Run(name, func(t *testing.T) {
			rc := newResponseURLCapture(t)
			bot.handleBlockActions(context.Background(), pressStart(tc.bead, tc.user, rc))
			posts := rc.all()
			if len(posts) != 1 || strings.Contains(posts[0].Text, "ABCD") {
				t.Fatalf("expected one refusal, got %+v", posts)
			}
		})
	}
	fc.mu.Lock()
	defer fc.mu.Unlock()
	if fc.calls != 0 {
		t.Fatalf("controller was called %d times for refused presses", fc.calls)
	}
}

func TestStartButtonSurfacesControllerRefusal(t *testing.T) {
	daemon := newMockDaemon()
	seedCodexRelogin(daemon, "gd-esc", "open")
	seedOperator(daemon, "UOP", `["approve:escalation"]`)
	fc := newFakeController(t, http.StatusConflict)
	slackSrv := newFakeSlackServer(t)
	defer slackSrv.Close()
	rc := newResponseURLCapture(t)
	bot := newTestBot(daemon, slackSrv)
	bot.controllerURL = fc.srv.URL

	bot.handleBlockActions(context.Background(), pressStart("gd-esc", "UOP", rc))
	posts := rc.all()
	if len(posts) != 1 || !strings.Contains(posts[0].Text, "does not exist") || !strings.Contains(posts[0].Text, "gb codex login") {
		t.Fatalf("got %+v", posts)
	}
	if fc.calls != 1 {
		t.Fatalf("409 must not be retried, calls=%d", fc.calls)
	}
}

func TestStartButtonRetriesNonLeader503(t *testing.T) {
	daemon := newMockDaemon()
	seedCodexRelogin(daemon, "gd-esc", "open")
	seedOperator(daemon, "UOP", `["approve:escalation"]`)
	fc := newFakeController(t, http.StatusServiceUnavailable)
	slackSrv := newFakeSlackServer(t)
	defer slackSrv.Close()
	rc := newResponseURLCapture(t)
	bot := newTestBot(daemon, slackSrv)
	bot.controllerURL = fc.srv.URL
	go func() {
		time.Sleep(300 * time.Millisecond)
		fc.mu.Lock()
		fc.code = http.StatusOK
		fc.mu.Unlock()
	}()

	bot.handleBlockActions(context.Background(), pressStart("gd-esc", "UOP", rc))
	posts := rc.all()
	if len(posts) != 1 || !strings.Contains(posts[0].Text, "ABCD-1234") {
		t.Fatalf("got %+v", posts)
	}
}
