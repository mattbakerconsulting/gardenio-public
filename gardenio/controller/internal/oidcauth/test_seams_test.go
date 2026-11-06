package oidcauth

// oidcDiscovered reports whether the OIDC/Keycloak request branch is verifying
// (discovery finished and the fail-closed 503 middleware has been swapped out),
// or true immediately when Keycloak is disabled. Unlike Ready() — which a dual
// AVA+OIDC handle satisfies via its synchronous AVA branch before discovery
// completes — this reflects the OIDC branch specifically, so a test that
// exercises the Keycloak path can wait for the swap instead of racing it.
func (h *Handle) oidcDiscovered() bool {
	return h.oidcReady.Load()
}

// saDiscovered reports whether the EKS SA-token branch is verifying (discovery
// finished and the fail-closed 503 middleware has been swapped out), or true
// immediately when the branch is disabled. Consumed by tests to gate on the
// swap instead of racing it, mirroring oidcDiscovered() for the Keycloak path.
func (h *Handle) saDiscovered() bool {
	return h.saReady.Load()
}
