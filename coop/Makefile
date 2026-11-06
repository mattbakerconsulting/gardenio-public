.PHONY: check ci fmt web install coverage outdated try-claude try-claudeless try-gemini try-mux try-k8s capture-claude

# Quick checks
#
# Excluded:
#   SKIP `cargo audit`
#   SKIP `cargo deny`
#
check:
	cargo fmt --all
	cargo clippy --all -- -D warnings
	cd crates/web && bun run fix
	cd crates/web && tsc --noEmit
	quench check --fix
	cargo build --all
	cargo test --all

# Full pre-release checks
ci:
	cargo fmt --all
	cargo clippy --all -- -D warnings
	cd crates/web && bun run check
	quench check --fix
	cargo build --all
	cargo test --all
	tests/e2e/run.sh
	cargo audit
	cargo deny check licenses bans sources

# Build web UIs (terminal + mux dashboard)
web:
	cd crates/web && bun run build

# Format code
fmt:
	cargo fmt --all

# Add license headers (--ci required for --license)
license:
	quench check --ci --fix --license

# Build and install coop to ~/.local/bin
install:
	@scripts/install

# Generate coverage report
coverage:
	@scripts/coverage

# Check for outdated dependencies
outdated:
	cargo outdated

# Launch coop wrapping claude with browser terminal
try-claude:
	@COOP_AGENT=claude bun tests/debug/start.ts -- claude

# Launch coop wrapping claudeless with browser terminal
# Usage: make try-claudeless SCENARIO=crates/cli/tests/scenarios/claude_hello.toml
try-claudeless:
	@COOP_AGENT=claude bun tests/debug/start.ts -- claudeless --scenario $(SCENARIO)

# Launch coop wrapping gemini with browser terminal
try-gemini:
	@COOP_AGENT=gemini bun tests/debug/start.ts -- gemini

# Launch coopmux dashboard (sessions connect automatically)
try-mux:
	@bun tests/debug/start-mux.ts --launch claude

# Launch coopmux + claude in local k8s cluster (kind or k3d)
try-k8s:
	@bun tests/debug/start-k8s.ts

# Capture state changes during claude onboarding (interactive)
# Usage: make capture-claude CONFIG=empty    (full onboarding)
#        make capture-claude CONFIG=auth-only (skip login)
#        make capture-claude CONFIG=trusted   (skip to idle)
capture-claude:
	@bun tests/debug/capture-claude.ts --config $(or $(CONFIG),empty)
