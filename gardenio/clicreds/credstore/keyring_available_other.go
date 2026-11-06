//go:build !darwin

package credstore

import "github.com/zalando/go-keyring"

// keyringAvailable probes the OS secret store. A successful set/delete on a
// sentinel key confirms the platform daemon is reachable; any error falls back
// to the protected file backend in auto mode.
//
// On Unix, go-keyring ships a fallback provider that can write to disk without
// a real Secret Service. The active probe keeps auto mode from mistaking that
// fallback for an OS-managed keyring when no DBus session is available.
func keyringAvailable() bool {
	const probeAccount = "__gardenio_probe__"
	if err := keyring.Set(keyringService, probeAccount, "ok"); err != nil {
		return false
	}
	_ = keyring.Delete(keyringService, probeAccount)
	return true
}
