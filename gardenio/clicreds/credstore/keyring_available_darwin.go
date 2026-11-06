//go:build darwin

package credstore

import (
	"os"
	"os/exec"
	"strings"
)

// keyringAvailable checks whether macOS has a configured user-default
// Keychain without attempting to write a sentinel credential. go-keyring's
// Darwin Set implementation uses interactive `security -i`; using it as an
// availability probe can raise a "Keychain Not Found" GUI dialog for the
// sentinel even though the caller has not asked to store a credential.
//
// A locked Keychain still counts as available. If a later, real credential
// write requires user interaction, macOS may request it for that write.
func keyringAvailable() bool {
	out, err := exec.Command(
		"/usr/bin/security",
		"default-keychain",
		"-d",
		"user",
	).Output()
	if err != nil {
		return false
	}
	path := parseDefaultKeychainPath(string(out))
	if path == "" {
		return false
	}
	info, err := os.Stat(path)
	return err == nil && !info.IsDir()
}

func parseDefaultKeychainPath(output string) string {
	return strings.Trim(strings.TrimSpace(output), `"`)
}
