package bridge

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"gardenio/controller/internal/beadsapi"
)

// Machine principal for slack-bridge DM delivery (gd-Hc34EENbO9 component 4):
// every call on the dm bead's own path (its writes, and the read-modify-write
// GET the client issues inside them) carries the bridge's service-account
// token, every other read stays on the client's own (fleet) token, a token failure fails the write closed with
// no static fallback, and the secret never reaches the daemon wire or a log.

type recordedCall struct{ method, path, auth string }

type recordingDMDaemon struct {
	mu    sync.Mutex
	calls []recordedCall
}

func (d *recordingDMDaemon) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	d.mu.Lock()
	d.calls = append(d.calls, recordedCall{r.Method, r.URL.Path, r.Header.Get("Authorization")})
	d.mu.Unlock()
	w.Header().Set("Content-Type", "application/json")
	_, _ = w.Write([]byte(`{"id":"dm-001","beads":[]}`))
}

func (d *recordingDMDaemon) snapshot() []recordedCall {
	d.mu.Lock()
	defer d.mu.Unlock()
	return append([]recordedCall(nil), d.calls...)
}

func dmTestJWT(t *testing.T) string {
	t.Helper()
	enc := func(v any) string {
		b, err := json.Marshal(v)
		if err != nil {
			t.Fatal(err)
		}
		return base64.RawURLEncoding.EncodeToString(b)
	}
	return enc(map[string]any{"alg": "none"}) + "." + enc(map[string]any{
		"azp": "gardenio-slack-bridge", "preferred_username": "service-account-gardenio-slack-bridge",
		"aud": "gardenio-traefik-forwardauth", "realm_roles": []string{"gardenio-machine-slack-dm-delivery"},
		"iat": 1700000000, "exp": 1700001800,
	}) + ".sig"
}

func deliverOneDM(t *testing.T, daemonURL string, machine beadsapi.TokenSource) {
	t.Helper()
	client, err := beadsapi.New(beadsapi.Config{HTTPAddr: daemonURL, Token: "fleet-static"})
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	slackSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/conversations.open":
			_ = json.NewEncoder(w).Encode(map[string]any{"ok": true, "channel": map[string]any{"id": "D-dm"}})
		case "/chat.postMessage":
			_ = json.NewEncoder(w).Encode(map[string]any{"ok": true, "channel": "D-dm", "ts": "1234.5678"})
		default:
			_ = json.NewEncoder(w).Encode(map[string]any{"ok": true})
		}
	}))
	defer slackSrv.Close()
	delivery := NewDMDelivery(DMDeliveryConfig{
		Daemon:  client,
		Bot:     newTestBot(client, slackSrv),
		Logger:  slog.New(slog.NewTextHandler(&bytes.Buffer{}, nil)),
		Machine: machine,
	})
	delivery.handleCreated(context.Background(), marshalSSEBeadPayload(BeadEvent{
		ID: "dm-001", Type: "dm",
		Fields: map[string]string{"target_user": "U-ENGINEER", "source_agent": "test-bot", "subject": "FYI", "body": "done"},
	}))
}

func isDMWrite(c recordedCall) bool {
	return c.method != http.MethodGet
}

// onDMBead reports whether the call addresses the dm bead itself.
func onDMBead(c recordedCall) bool {
	return strings.Contains(c.path, "/v1/beads/dm-001")
}

func TestDMDelivery_MachinePrincipalCarriesTheWritesOnly(t *testing.T) {
	const secret = "slack-s3cr3t"
	tok := dmTestJWT(t)
	tokenSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		id, sec, ok := r.BasicAuth()
		if !ok {
			id, sec = r.PostForm.Get("client_id"), r.PostForm.Get("client_secret")
		}
		if id != "gardenio-slack-bridge" || sec != secret {
			http.Error(w, `{"error":"invalid_client"}`, http.StatusUnauthorized)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"access_token": tok, "token_type": "Bearer", "expires_in": 1800})
	}))
	defer tokenSrv.Close()
	secretFile := filepath.Join(t.TempDir(), "clientSecret")
	if err := os.WriteFile(secretFile, []byte(secret+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	ts := beadsapi.NewMachineTokenSource(context.Background(), nil, tokenSrv.URL+"/token", "gardenio-slack-bridge", secretFile)
	d := &recordingDMDaemon{}
	daemonSrv := httptest.NewServer(d)
	defer daemonSrv.Close()

	deliverOneDM(t, daemonSrv.URL, ts)

	var writes int
	for _, c := range d.snapshot() {
		if strings.Contains(c.auth, secret) {
			t.Errorf("%s %s carried the client secret", c.method, c.path)
		}
		if isDMWrite(c) {
			writes++
		}
		if onDMBead(c) {
			if c.auth != "Bearer "+tok {
				t.Errorf("%s %s Authorization=%q, want the machine token", c.method, c.path, c.auth)
			}
			continue
		}
		if isDMWrite(c) {
			t.Errorf("write %s %s is not on the dm bead; delivery must not write elsewhere", c.method, c.path)
		}
		if c.auth != "Bearer fleet-static" {
			t.Errorf("%s %s Authorization=%q, want the client's own (fleet) token", c.method, c.path, c.auth)
		}
	}
	// delivery metadata PATCH + close of the fire-and-forget DM.
	if writes != 2 {
		t.Fatalf("recorded %d writes (%v), want 2 (delivery stamp + close)", writes, d.snapshot())
	}
}

func TestDMDelivery_StaticWhenMachineUnset(t *testing.T) {
	d := &recordingDMDaemon{}
	daemonSrv := httptest.NewServer(d)
	defer daemonSrv.Close()
	deliverOneDM(t, daemonSrv.URL, nil)
	calls := d.snapshot()
	if len(calls) == 0 {
		t.Fatal("daemon saw no calls")
	}
	for _, c := range calls {
		if c.auth != "Bearer fleet-static" {
			t.Errorf("%s %s Authorization=%q, want the fleet token (machine unset)", c.method, c.path, c.auth)
		}
	}
}

func TestDMDelivery_MachineFetchFailureFailsWritesClosed(t *testing.T) {
	const secret = "slack-s3cr3t"
	tokenSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, `{"error":"server_error"}`, http.StatusInternalServerError)
	}))
	defer tokenSrv.Close()
	secretFile := filepath.Join(t.TempDir(), "clientSecret")
	if err := os.WriteFile(secretFile, []byte(secret), 0o600); err != nil {
		t.Fatal(err)
	}
	ts := beadsapi.NewMachineTokenSource(context.Background(), nil, tokenSrv.URL+"/token", "gardenio-slack-bridge", secretFile)
	d := &recordingDMDaemon{}
	daemonSrv := httptest.NewServer(d)
	defer daemonSrv.Close()

	var logs bytes.Buffer
	client, err := beadsapi.New(beadsapi.Config{HTTPAddr: daemonSrv.URL, Token: "fleet-static"})
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	delivery := NewDMDelivery(DMDeliveryConfig{Daemon: client, Logger: slog.New(slog.NewTextHandler(&logs, nil)), Machine: ts})
	// The write helper is what every dm write goes through.
	err = client.UpdateBeadFields(delivery.writeCtx(context.Background()), "dm-001", map[string]string{"dm_channel": "D1"})
	if err == nil || !strings.Contains(err.Error(), "auth_unavailable") {
		t.Fatalf("err=%v, want auth_unavailable", err)
	}
	if strings.Contains(err.Error(), secret) || strings.Contains(logs.String(), secret) {
		t.Fatal("the client secret leaked into an error or a log")
	}
	for _, c := range d.snapshot() {
		if isDMWrite(c) {
			t.Fatalf("a write %s %s reached the daemon after a failed token fetch (no static fallback allowed)", c.method, c.path)
		}
	}
}
