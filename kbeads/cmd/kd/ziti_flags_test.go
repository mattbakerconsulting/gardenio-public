package main

import (
	"os"
	"os/exec"
	"strings"
	"testing"
)

// Ziti is off in the stack by default, but kd/gb keep their in-process ziti
// dial (S9b gd-qstfURyZz9): the three flags must exist and default from the
// GARDENIO_ZITI_* environment. Cobra captures a flag's default when the flag
// is registered at init, so the env-defaulting case re-executes this test
// binary with the variables set and reads the flags back in the child.

var zitiFlagEnv = map[string]string{
	"ziti-identity":        "GARDENIO_ZITI_IDENTITY",
	"ziti-service":         "GARDENIO_ZITI_SERVICE",
	"ziti-tls-server-name": "GARDENIO_ZITI_TLS_SERVER_NAME",
}

func TestZitiFlagsExist(t *testing.T) {
	for name := range zitiFlagEnv {
		if rootCmd.PersistentFlags().Lookup(name) == nil {
			t.Errorf("--%s is not registered on the root command", name)
		}
	}
}

// TestZitiFlagsChild is the child half of TestZitiFlagsDefaultFromEnv. It is a
// no-op unless the parent set ZITI_FLAGS_CHILD.
func TestZitiFlagsChild(t *testing.T) {
	if os.Getenv("ZITI_FLAGS_CHILD") == "" {
		t.Skip("child of TestZitiFlagsDefaultFromEnv")
	}
	for name, env := range zitiFlagEnv {
		f := rootCmd.PersistentFlags().Lookup(name)
		if f == nil {
			t.Fatalf("--%s is not registered", name)
		}
		if want := os.Getenv(env); f.DefValue != want || f.Value.String() != want {
			t.Errorf("--%s default = %q (value %q), want %q from %s", name, f.DefValue, f.Value.String(), want, env)
		}
	}
}

func TestZitiFlagsDefaultFromEnv(t *testing.T) {
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(exe, "-test.run=^TestZitiFlagsChild$", "-test.v")
	cmd.Env = append(os.Environ(),
		"ZITI_FLAGS_CHILD=1",
		"GARDENIO_ZITI_IDENTITY=/tmp/id.json",
		"GARDENIO_ZITI_SERVICE=gardenio-beads",
		"GARDENIO_ZITI_TLS_SERVER_NAME=beads.example",
	)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("child failed: %v\n%s", err, out)
	}
	if strings.Contains(string(out), "SKIP") {
		t.Fatalf("child skipped, so nothing was checked:\n%s", out)
	}
}
