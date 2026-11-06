package main

// setup_codex_auth.go — install the mounted ChatGPT-subscription login at pod
// start (ADR 0004 §5, gd-2OYgUcnZcV).
//
// The controller projects the derived gardenio-codex-auth*-session Secret
// read-only at podmanager.MountCodexAuth. Codex reads $CODEX_HOME/auth.json
// (cli_auth_credentials_store = "file"), so the pod's start step must copy it
// there. Without the copy, the only way auth.json ever arrives is the coopmux
// credential push, which skips a session until its account assignment lines up;
// until then every turn is a 401 "Missing bearer". The copy makes the file
// present before codex launches, and the keeper-driven push still overwrites it
// later (rename-replace) when tokens rotate.

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"gardenio/controller/internal/codexauth"
	"gardenio/controller/internal/podmanager"
)

// codexAuthMountDir is the read-only -session Secret mount. A var so tests can
// point it at a temp dir.
var codexAuthMountDir = podmanager.MountCodexAuth

// installCodexAuth copies <mount>/auth.json to <codexHome>/auth.json (0600,
// writable by codex) and reports whether it did. A missing mount file is not an
// error (no codex account bound yet): any auth.json already in codexHome, e.g.
// from a coopmux push or a resumed workspace, is left alone. A present but
// unparseable mount file is an error and never replaces a working file.
func installCodexAuth(codexHome string) (bool, error) {
	src := filepath.Join(codexAuthMountDir, podmanager.CodexAuthFileName)
	data, err := os.ReadFile(src)
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("read %s: %w", src, err)
	}
	if _, err := codexauth.Parse(data); err != nil {
		return false, fmt.Errorf("%s: %w", src, err)
	}
	if err := os.MkdirAll(codexHome, 0o700); err != nil {
		return false, err
	}
	if err := writeHarnessFileAtomic(filepath.Join(codexHome, podmanager.CodexAuthFileName), data, 0o600); err != nil {
		return false, err
	}
	return true, nil
}

// installCodexAuthLogged is installCodexAuth for the boot path: failures are
// logged, never fatal, so a bad Secret cannot wedge pod start.
func installCodexAuthLogged(codexHome string) {
	switch ok, err := installCodexAuth(codexHome); {
	case err != nil:
		logInit("warning: codex auth.json not installed: %v", err)
	case ok:
		logInit("installed codex auth.json into %s", codexHome)
	default:
		logInit("no mounted codex login at %s; relying on coopmux credential push", codexAuthMountDir)
	}
}
