package microvm

import "log/slog"

// newBackendWithClients is the test seam: it builds a Backend from already-
// constructed client fakes, bypassing the AWS credential chain. Production code
// uses newBackendFromIdentity.
func newBackendWithClients(mv microvmAPI, sm secretsAPI, cfg BackendConfig, logger *slog.Logger) (*Backend, error) {
	if logger == nil {
		logger = slog.Default()
	}
	cfg.SecretPrefix = normalizeSecretPrefix(cfg.SecretPrefix)
	cfg.normalizeLogging()
	if cfg.MaximumDurationSeconds <= 0 {
		cfg.MaximumDurationSeconds = defaultMaximumDurationSeconds
	}
	// Raise the AWS idle-suspend window above the VM lifetime cap so it can never
	// fire mid-turn in the egress-only topology (ka-0000002917); must run after
	// MaximumDurationSeconds is normalized.
	normalizeIdlePolicy(cfg.IdlePolicy, cfg.MaximumDurationSeconds)
	if err := cfg.validate(); err != nil {
		return nil, err
	}
	return &Backend{microvm: mv, secrets: sm, cfg: cfg, logger: logger}, nil
}
