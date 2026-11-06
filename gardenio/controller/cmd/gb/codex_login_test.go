package main

import (
	"bytes"
	"context"
	"encoding/base64"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"gardenio/controller/internal/codexauth"
)

const testLoginDoc = `{"auth_mode":"chatgpt","tokens":{"access_token":"at-SECRET","refresh_token":"rt-SECRET","id_token":"id","account_id":"a"}}`

func TestCodexLoginStoresWithoutPrinting(t *testing.T) {
	var home string
	var calls [][]string
	var applied []byte
	d := codexLoginDeps{
		runCodex: func(_ context.Context, _, h string, stdout, _ io.Writer) error {
			home = h
			if h == "" {
				t.Fatal("no throwaway home")
			}
			io.WriteString(stdout, "Visit https://auth.openai.com/codex/device code ABCD-1234\n")
			return os.WriteFile(filepath.Join(h, "auth.json"), []byte(testLoginDoc), 0o600)
		},
		kubectl: func(_ context.Context, _ string, args []string, stdin []byte) ([]byte, error) {
			calls = append(calls, args)
			if args[len(args)-1] == "yaml" {
				return []byte("kind: Secret\n"), nil
			}
			applied = stdin
			return nil, nil
		},
	}
	var out, errb bytes.Buffer
	if err := codexLogin(context.Background(), "gardenio-codex-auth", "gardenio", "k3s", "codex", "kubectl", d, &out, &errb); err != nil {
		t.Fatal(err)
	}
	if string(applied) != "kind: Secret\n" || len(calls) != 2 {
		t.Fatalf("calls=%v applied=%q", calls, applied)
	}
	if got := strings.Join(calls[0], " "); !strings.HasPrefix(got, "--context k3s -n gardenio create secret generic gardenio-codex-auth --from-file=auth.json="+home) {
		t.Fatalf("create args: %s", got)
	}
	for _, s := range []string{out.String(), errb.String()} {
		if strings.Contains(s, "SECRET") {
			t.Fatalf("token leaked to terminal: %q", s)
		}
	}
	if !strings.Contains(errb.String(), "refresh fingerprint") {
		t.Fatalf("no fingerprint: %q", errb.String())
	}
	if _, err := os.Stat(home); !os.IsNotExist(err) {
		t.Fatalf("throwaway home not removed: %v", err)
	}
}

func TestCodexLoginRejectsNonChatGPTResultAndCleansUp(t *testing.T) {
	var home string
	d := codexLoginDeps{
		runCodex: func(_ context.Context, _, h string, _, _ io.Writer) error {
			home = h
			return os.WriteFile(filepath.Join(h, "auth.json"), []byte(`{"OPENAI_API_KEY":"sk-x"}`), 0o600)
		},
		kubectl: func(context.Context, string, []string, []byte) ([]byte, error) {
			t.Fatal("kubectl must not run")
			return nil, nil
		},
	}
	err := codexLogin(context.Background(), "s", "", "", "codex", "kubectl", d, io.Discard, io.Discard)
	if err == nil || strings.Contains(err.Error(), "sk-x") {
		t.Fatalf("err=%v", err)
	}
	if _, serr := os.Stat(home); !os.IsNotExist(serr) {
		t.Fatal("home not removed")
	}
}

func TestCodexLoginCodexFailure(t *testing.T) {
	d := codexLoginDeps{
		runCodex: func(context.Context, string, string, io.Writer, io.Writer) error { return errors.New("exit 1") },
	}
	if err := codexLogin(context.Background(), "s", "", "", "codex", "kubectl", d, io.Discard, io.Discard); err == nil {
		t.Fatal("expected error")
	}
}

func TestCodexDeviceProbePrintsOnlyNonSecretFacts(t *testing.T) {
	enc := func(s string) string { return base64.RawURLEncoding.EncodeToString([]byte(s)) }
	id := enc(`{"alg":"none"}`) + "." + enc(`{"https://api.openai.com/auth":{"chatgpt_account_id":"acct-1"}}`) + ".s"
	mux := http.NewServeMux()
	mux.HandleFunc("/api/accounts/deviceauth/usercode", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"device_auth_id":"d","user_code":"ABCD-1234","interval":1}`))
	})
	mux.HandleFunc("/api/accounts/deviceauth/token", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"authorization_code":"a","code_verifier":"v"}`))
	})
	mux.HandleFunc("/oauth/token", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"id_token":"` + id + `","access_token":"at-SECRET","refresh_token":"rt-SECRET"}`))
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	var out, errb bytes.Buffer
	if err := codexDeviceProbe(context.Background(), codexauth.NewDeviceLogin(srv.URL), &out, &errb); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(out.String(), "SECRET") || strings.Contains(errb.String(), "SECRET") {
		t.Fatal("token leaked")
	}
	if !strings.Contains(out.String(), "chatgpt_account_id present: true") || !strings.Contains(errb.String(), "ABCD-1234") {
		t.Fatalf("out=%q err=%q", out.String(), errb.String())
	}
}
