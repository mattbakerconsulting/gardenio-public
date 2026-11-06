package main

import (
	"os"
	"path/filepath"
	"testing"
)

const testCodexAuthJSON = `{"auth_mode":"chatgpt","tokens":{"access_token":"a.b.c","refresh_token":"gardenio-broker-managed","id_token":"x.y.z"}}`

func withCodexAuthMount(t *testing.T, content string) {
	t.Helper()
	mount := t.TempDir()
	if content != "" {
		if err := os.WriteFile(filepath.Join(mount, "auth.json"), []byte(content), 0o400); err != nil {
			t.Fatal(err)
		}
	}
	old := codexAuthMountDir
	codexAuthMountDir = mount
	t.Cleanup(func() { codexAuthMountDir = old })
}

// A codex pod start must leave a non-empty, writable $CODEX_HOME/auth.json.
func TestInstallCodexAuth_CopiesMountedLogin(t *testing.T) {
	withCodexAuthMount(t, testCodexAuthJSON)
	home := filepath.Join(t.TempDir(), ".codex")
	ok, err := installCodexAuth(home)
	if err != nil || !ok {
		t.Fatalf("installCodexAuth = %v, %v", ok, err)
	}
	got, err := os.ReadFile(filepath.Join(home, "auth.json"))
	if err != nil || string(got) != testCodexAuthJSON {
		t.Fatalf("auth.json = %q, %v", got, err)
	}
	fi, _ := os.Stat(filepath.Join(home, "auth.json"))
	if fi.Mode().Perm() != 0o600 {
		t.Errorf("perm = %v, want 0600 (codex must be able to rewrite it)", fi.Mode().Perm())
	}
}

func TestInstallCodexAuth_ReplacesStaleFile(t *testing.T) {
	withCodexAuthMount(t, testCodexAuthJSON)
	home := t.TempDir()
	_ = os.WriteFile(filepath.Join(home, "auth.json"), []byte(`{"stale":true}`), 0o600)
	if ok, err := installCodexAuth(home); err != nil || !ok {
		t.Fatalf("installCodexAuth = %v, %v", ok, err)
	}
	if got, _ := os.ReadFile(filepath.Join(home, "auth.json")); string(got) != testCodexAuthJSON {
		t.Errorf("auth.json = %q", got)
	}
}

func TestInstallCodexAuth_NoMountKeepsExisting(t *testing.T) {
	withCodexAuthMount(t, "")
	home := t.TempDir()
	_ = os.WriteFile(filepath.Join(home, "auth.json"), []byte("pushed"), 0o600)
	ok, err := installCodexAuth(home)
	if err != nil || ok {
		t.Fatalf("installCodexAuth = %v, %v; want false, nil", ok, err)
	}
	if got, _ := os.ReadFile(filepath.Join(home, "auth.json")); string(got) != "pushed" {
		t.Errorf("existing auth.json clobbered: %q", got)
	}
}

func TestInstallCodexAuth_InvalidMountNeverReplaces(t *testing.T) {
	withCodexAuthMount(t, `{"tokens":{}}`)
	home := t.TempDir()
	_ = os.WriteFile(filepath.Join(home, "auth.json"), []byte("good"), 0o600)
	if ok, err := installCodexAuth(home); err == nil || ok {
		t.Fatalf("installCodexAuth = %v, %v; want error", ok, err)
	}
	if got, _ := os.ReadFile(filepath.Join(home, "auth.json")); string(got) != "good" {
		t.Errorf("working auth.json replaced: %q", got)
	}
}

// The pod boot path (setupCodexSettings) must install it alongside the
// config.toml write.
func TestSetupCodexSettings_InstallsAuthJSON(t *testing.T) {
	withCodexAuthMount(t, testCodexAuthJSON)
	home := filepath.Join(t.TempDir(), ".codex")
	t.Setenv(envCodexHome, home)
	codexConfigBeadServer(t, nil)
	setupCodexSettings(t.TempDir(), "crew", false)
	if fi, err := os.Stat(filepath.Join(home, "auth.json")); err != nil || fi.Size() == 0 {
		t.Fatalf("auth.json missing or empty after codex pod start: %v", err)
	}
}
