package main

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/pem"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func freeAddrs(t *testing.T, n int) []string {
	t.Helper()
	var out []string
	for range n {
		l, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatal(err)
		}
		out = append(out, l.Addr().String())
		l.Close()
	}
	return out
}

func quiet() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

func TestRun_RefusesToStartWithoutSecret(t *testing.T) {
	a := freeAddrs(t, 2)
	err := run(context.Background(), &config{listenAddr: a[0], metricsAddr: a[1], replayTTL: time.Hour, replayMax: 10}, quiet())
	if err == nil || !strings.Contains(err.Error(), "webhook secret") {
		t.Fatalf("err = %v, want webhook secret error", err)
	}
}

func TestRun_ServesHealthWebhookAndMetrics(t *testing.T) {
	a := freeAddrs(t, 2)
	cfg := &config{listenAddr: a[0], metricsAddr: a[1], webhookSecret: testSecret, replayTTL: time.Hour, replayMax: 10}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- run(ctx, cfg, quiet()) }()

	waitFor(t, "http://"+a[0]+"/healthz")
	resp, err := http.Post("http://"+a[0]+"/webhook", "application/json", strings.NewReader(prBody))
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Errorf("unsigned webhook = %d, want 403", resp.StatusCode)
	}
	m, err := http.Get("http://" + a[1] + "/metrics")
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(m.Body)
	m.Body.Close()
	if !strings.Contains(string(body), `gardenio_github_webhook_deliveries_total{outcome="bad_signature"} 1`) {
		t.Errorf("metrics missing bad_signature count:\n%s", body)
	}
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Errorf("run returned %v on clean shutdown", err)
		}
	case <-time.After(15 * time.Second):
		t.Fatal("run did not return after cancel")
	}
}

func waitFor(t *testing.T, url string) {
	t.Helper()
	for range 100 {
		if r, err := http.Get(url); err == nil {
			r.Body.Close()
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("%s never came up", url)
}

func TestParseConfig_SecretFileWinsAndIsTrimmed(t *testing.T) {
	dir := t.TempDir()
	f := filepath.Join(dir, "secret")
	os.WriteFile(f, []byte("from-file\n"), 0o600)
	t.Setenv("GITHUB_WEBHOOK_SECRET_FILE", f)
	t.Setenv("GITHUB_WEBHOOK_SECRET", "from-env")
	cfg, err := parseConfig()
	if err != nil {
		t.Fatal(err)
	}
	if string(cfg.webhookSecret) != "from-file" {
		t.Errorf("secret = %q", cfg.webhookSecret)
	}
}

func TestParseConfig_MissingSecretFileIsAnError(t *testing.T) {
	t.Setenv("GITHUB_WEBHOOK_SECRET_FILE", filepath.Join(t.TempDir(), "absent"))
	if _, err := parseConfig(); err == nil {
		t.Fatal("an unreadable secret file must be an error, not an empty secret")
	}
}

func TestParseConfig_RejectsBadNumbers(t *testing.T) {
	t.Setenv("GITHUB_WEBHOOK_SECRET", "s")
	for k, v := range map[string]string{"REPLAY_TTL": "soon", "REPLAY_MAX_ENTRIES": "-3", "GITHUB_APP_INSTALLATION_ID": "abc", "RELEASE_POLL_INTERVAL": "0s"} {
		t.Run(k, func(t *testing.T) {
			t.Setenv(k, v)
			if _, err := parseConfig(); err == nil {
				t.Errorf("%s=%q must be rejected", k, v)
			}
		})
	}
}

func TestBuildTokenSource(t *testing.T) {
	if ts, err := buildTokenSource(&config{}); ts != nil || err != nil {
		t.Errorf("no app configured = (%v, %v), want (nil, nil)", ts, err)
	}
	if _, err := buildTokenSource(&config{appID: "1"}); err == nil {
		t.Error("app id without a key must be an error")
	}
	key, _ := rsa.GenerateKey(rand.Reader, 2048)
	f := filepath.Join(t.TempDir(), "private-key")
	os.WriteFile(f, pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(key)}), 0o600)
	if ts, err := buildTokenSource(&config{appID: "5029022", privateKeyFile: f}); err != nil || ts == nil {
		t.Errorf("valid app = (%v, %v)", ts, err)
	}
}

func TestParseConfig_DraftAgentPRs(t *testing.T) {
	t.Setenv("GITHUB_WEBHOOK_SECRET", "s")
	cfg, err := parseConfig()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.draftAgentPRs {
		t.Error("GITHUB_BRIDGE_DRAFT_AGENT_PRS unset must default to off")
	}
	t.Setenv("GITHUB_BRIDGE_DRAFT_AGENT_PRS", "true")
	if cfg, err = parseConfig(); err != nil || !cfg.draftAgentPRs {
		t.Errorf("GITHUB_BRIDGE_DRAFT_AGENT_PRS=true did not switch it on (err %v)", err)
	}
	t.Setenv("GITHUB_BRIDGE_DRAFT_AGENT_PRS", "yes please")
	if _, err := parseConfig(); err == nil {
		t.Error("a malformed GITHUB_BRIDGE_DRAFT_AGENT_PRS must be rejected")
	}
}

// The flag is the only switch: off (the default) wires no drafter even with an
// App, so no PR is ever converted.
func TestNewAgentPRDrafter(t *testing.T) {
	tokens := newTokenSource(&fakeMinter{}, 0, nil)
	if d := newAgentPRDrafter(&config{}, tokens); d != nil {
		t.Errorf("flag off = %v, want no drafter", d)
	}
	if d := newAgentPRDrafter(&config{draftAgentPRs: true}, nil); d != nil {
		t.Errorf("flag on without an App = %v, want no drafter", d)
	}
	if d := newAgentPRDrafter(&config{draftAgentPRs: true}, tokens); d == nil {
		t.Error("flag on with an App wired no drafter")
	}
}
