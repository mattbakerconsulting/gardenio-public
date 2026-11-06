package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestSaveLoadRoundTrip(t *testing.T) {
	t.Setenv("HOME", t.TempDir())

	in := RemotesConfig{
		Active: "prod",
		Remotes: map[string]Remote{
			"prod":  {URL: "prod.example.com:9090", Token: "tok_abc", NATSURL: "nats://prod:4222"},
			"local": {URL: "localhost:9090"},
		},
	}
	if err := saveRemotesConfig(in); err != nil {
		t.Fatalf("save: %v", err)
	}

	got, err := loadRemotesConfig()
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if got.Active != "prod" {
		t.Errorf("Active = %q, want %q", got.Active, "prod")
	}
	prod := got.Remotes["prod"]
	if prod.URL != "prod.example.com:9090" || prod.Token != "tok_abc" || prod.NATSURL != "nats://prod:4222" {
		t.Errorf("prod remote = %+v, wrong values", prod)
	}
	if got.Remotes == nil {
		t.Error("Remotes map must not be nil after load")
	}
}

func TestLoadRemotesConfig_NoFile(t *testing.T) {
	t.Setenv("HOME", t.TempDir())

	cfg, err := loadRemotesConfig()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cfg.Active != "" || len(cfg.Remotes) != 0 {
		t.Errorf("expected empty config, got %+v", cfg)
	}
}

func TestSaveRemotesConfig_Permissions(t *testing.T) {
	t.Setenv("HOME", t.TempDir())

	if err := saveRemotesConfig(RemotesConfig{Remotes: map[string]Remote{}}); err != nil {
		t.Fatalf("save: %v", err)
	}
	path, _ := remoteConfigPath()
	check := func(p string, want os.FileMode) {
		t.Helper()
		info, err := os.Stat(p)
		if err != nil {
			t.Fatalf("stat %s: %v", p, err)
		}
		if got := info.Mode().Perm(); got != want {
			t.Errorf("%s permissions = %04o, want %04o", p, got, want)
		}
	}
	check(path, 0o600)
	check(filepath.Dir(path), 0o700)
}

func TestRemoteLifecycle(t *testing.T) {
	t.Setenv("HOME", t.TempDir())

	// add → upsert → use → list → show → remove
	mustRun := func(fn func() error) {
		t.Helper()
		if err := fn(); err != nil {
			t.Fatal(err)
		}
	}

	mustRun(func() error { return remoteAddCmd.RunE(remoteAddCmd, []string{"local", "localhost:9090"}) })
	mustRun(func() error { return remoteAddCmd.RunE(remoteAddCmd, []string{"local", "localhost:9090"}) }) // upsert

	mustRun(func() error { return remoteUseCmd.RunE(remoteUseCmd, []string{"local"}) })

	cfg, _ := loadRemotesConfig()
	if cfg.Active != "local" {
		t.Fatalf("Active = %q, want %q", cfg.Active, "local")
	}

	// list should mark active with *
	var buf bytes.Buffer
	remoteListCmd.SetOut(&buf)
	mustRun(func() error { return remoteListCmd.RunE(remoteListCmd, nil) })
	if !strings.Contains(buf.String(), "* local") {
		t.Errorf("list missing active marker; got:\n%s", buf.String())
	}

	// show (active) should include name, URL, and (active)
	buf.Reset()
	remoteShowCmd.SetOut(&buf)
	mustRun(func() error { return remoteShowCmd.RunE(remoteShowCmd, nil) })
	out := buf.String()
	if !strings.Contains(out, "local") || !strings.Contains(out, "localhost:9090") || !strings.Contains(out, "(active)") {
		t.Errorf("show missing expected content; got:\n%s", out)
	}

	// show by explicit name
	buf.Reset()
	mustRun(func() error { return remoteShowCmd.RunE(remoteShowCmd, []string{"local"}) })
	if !strings.Contains(buf.String(), "localhost:9090") {
		t.Errorf("show by name missing URL; got:\n%s", buf.String())
	}

	// use with no args should clear active
	mustRun(func() error { return remoteUseCmd.RunE(remoteUseCmd, nil) })
	cfg, _ = loadRemotesConfig()
	if cfg.Active != "" {
		t.Errorf("Active should be cleared by bare 'use', got %q", cfg.Active)
	}

	// re-activate for remove test
	mustRun(func() error { return remoteUseCmd.RunE(remoteUseCmd, []string{"local"}) })

	// remove should clear active
	mustRun(func() error { return remoteRemoveCmd.RunE(remoteRemoveCmd, []string{"local"}) })
	cfg, _ = loadRemotesConfig()
	if _, ok := cfg.Remotes["local"]; ok {
		t.Error("remote 'local' should be gone")
	}
	if cfg.Active != "" {
		t.Errorf("Active should be cleared, got %q", cfg.Active)
	}
}

func TestRemoteTokenHandling(t *testing.T) {
	t.Setenv("HOME", t.TempDir())

	if err := remoteAddCmd.Flags().Set("token", "tok_verylongsecret"); err != nil {
		t.Fatalf("set token flag: %v", err)
	}
	t.Cleanup(func() { _ = remoteAddCmd.Flags().Set("token", "") })

	if err := remoteAddCmd.RunE(remoteAddCmd, []string{"prod", "prod.example.com:9090"}); err != nil {
		t.Fatal(err)
	}
	if err := remoteUseCmd.RunE(remoteUseCmd, []string{"prod"}); err != nil {
		t.Fatal(err)
	}

	var buf bytes.Buffer

	// list: token truncated, full token hidden
	remoteListCmd.SetOut(&buf)
	if err := remoteListCmd.RunE(remoteListCmd, nil); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(buf.String(), "tok_verylongsecret") {
		t.Error("full token must not appear in list output")
	}
	if !strings.Contains(buf.String(), "tok_very...") {
		t.Errorf("expected truncated token in list; got:\n%s", buf.String())
	}

	// show: first 8 chars visible, full token hidden
	buf.Reset()
	remoteShowCmd.SetOut(&buf)
	if err := remoteShowCmd.RunE(remoteShowCmd, nil); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(buf.String(), "tok_verylongsecret") {
		t.Error("full token must not appear in show output")
	}
	if !strings.Contains(buf.String(), "tok_very") {
		t.Errorf("expected first 8 chars of token in show; got:\n%s", buf.String())
	}
}

func TestRemoteDescription(t *testing.T) {
	t.Setenv("HOME", t.TempDir())

	mustRun := func(fn func() error) {
		t.Helper()
		if err := fn(); err != nil {
			t.Fatal(err)
		}
	}

	if err := remoteAddCmd.Flags().Set("description", "staging cluster"); err != nil {
		t.Fatalf("set description flag: %v", err)
	}
	t.Cleanup(func() { _ = remoteAddCmd.Flags().Set("description", "") })

	mustRun(func() error { return remoteAddCmd.RunE(remoteAddCmd, []string{"stg", "stg.example.com:9090"}) })

	// round-trip through config file
	cfg, err := loadRemotesConfig()
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if got := cfg.Remotes["stg"].Description; got != "staging cluster" {
		t.Fatalf("Description = %q, want %q", got, "staging cluster")
	}

	mustRun(func() error { return remoteUseCmd.RunE(remoteUseCmd, []string{"stg"}) })

	var buf bytes.Buffer

	// list includes description
	remoteListCmd.SetOut(&buf)
	mustRun(func() error { return remoteListCmd.RunE(remoteListCmd, nil) })
	if !strings.Contains(buf.String(), "staging cluster") {
		t.Errorf("list missing description; got:\n%s", buf.String())
	}

	// show includes description
	buf.Reset()
	remoteShowCmd.SetOut(&buf)
	mustRun(func() error { return remoteShowCmd.RunE(remoteShowCmd, nil) })
	if !strings.Contains(buf.String(), "staging cluster") {
		t.Errorf("show missing description; got:\n%s", buf.String())
	}

	// show omits description line when empty
	_ = remoteAddCmd.Flags().Set("description", "")
	mustRun(func() error { return remoteAddCmd.RunE(remoteAddCmd, []string{"bare", "bare.example.com:9090"}) })
	buf.Reset()
	mustRun(func() error { return remoteShowCmd.RunE(remoteShowCmd, []string{"bare"}) })
	if strings.Contains(buf.String(), "description:") {
		t.Errorf("show should omit description line when empty; got:\n%s", buf.String())
	}
}

func TestRemoteErrorCases(t *testing.T) {
	tests := []struct {
		name string
		fn   func() error
	}{
		{"use unknown", func() error { return remoteUseCmd.RunE(remoteUseCmd, []string{"ghost"}) }},
		{"remove unknown", func() error { return remoteRemoveCmd.RunE(remoteRemoveCmd, []string{"ghost"}) }},
		{"show no active", func() error { return remoteShowCmd.RunE(remoteShowCmd, nil) }},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("HOME", t.TempDir())
			if err := tc.fn(); err == nil {
				t.Fatal("expected error, got nil")
			}
		})
	}
}
