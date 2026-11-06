.PHONY: build build-gardenio build-kbeads build-coop test test-go test-rust test-coop test-coop-coverage test-out-of-workspace test-helm test-slackmock test-slack-session-guard test-grafana-plugin test-lands-contract test-lands-gl test-heraldry-proto test-lands-conformance lands-gl-tripwire generate-lands-contract lint ratchets-shrink clean release release-dry-run hooks gitleaks run-beads-viewer screenshot-beads-viewer screenshot-threads e2e-threads-seeded test-beads-viewer-e2e seed-beads-viewer-e2e coverage-go lcov-go

# Coverage instrumentation: set COVER=1 to build with runtime coverage profiling.
# Go uses -cover (1.20+), Rust uses -C instrument-coverage (1.60+).
# Coverage data is written to GOCOVERDIR (Go) or LLVM_PROFILE_FILE (Rust) at runtime.
COVER ?= 0

# Unit-test coverage emission: set COVERAGE=1 to make `test-coop` produce
# `.coverage/unit/coop.lcov` via cargo-llvm-cov. Default 0 keeps the dev loop
# fast (plain `cargo test --workspace`). Phase-2a coverage epic ka-0000000117
# (W1-Rust task ka-0000003976) — see docs/research/coverage-2a-w1-rust-coop.md.
COVERAGE ?= 0

# Go coverage flags
ifeq ($(COVER),1)
GO_COVER_FLAGS = -cover -covermode=atomic -coverpkg=./...
else
GO_COVER_FLAGS =
endif

# Rust coverage flags
ifeq ($(COVER),1)
RUSTFLAGS_COVER = -C instrument-coverage
else
RUSTFLAGS_COVER =
endif

# Build all components
build: build-gardenio build-kbeads build-coop

# Go components (via go.work)
build-gardenio:
	cd gardenio/controller && CGO_ENABLED=0 go build $(GO_COVER_FLAGS) -o bin/controller ./cmd/controller/
	cd gardenio/controller && CGO_ENABLED=0 go build $(GO_COVER_FLAGS) -o bin/slack-bridge ./cmd/slack-bridge/
	cd gardenio/controller && CGO_ENABLED=0 go build $(GO_COVER_FLAGS) -o bin/gb ./cmd/gb/
	cd gardenio/controller && CGO_ENABLED=0 go build $(GO_COVER_FLAGS) -o bin/jira-bridge ./cmd/jira-bridge/
	cd gardenio/controller && CGO_ENABLED=0 go build $(GO_COVER_FLAGS) -o bin/gitlab-bridge ./cmd/gitlab-bridge/
	cd gardenio/controller && CGO_ENABLED=0 go build $(GO_COVER_FLAGS) -o bin/github-bridge ./cmd/github-bridge/
	cd gardenio/controller && CGO_ENABLED=0 go build $(GO_COVER_FLAGS) -o bin/grafana-bridge ./cmd/grafana-bridge/
	cd gardenio/controller && CGO_ENABLED=0 go build $(GO_COVER_FLAGS) -o bin/edge-auth ./cmd/edge-auth/

build-kbeads:
	cd kbeads && CGO_ENABLED=0 go build $(GO_COVER_FLAGS) -o bin/kd ./cmd/kd/

# Rust components
build-coop:
	cd coop && RUSTFLAGS="$(RUSTFLAGS_COVER)" cargo build --workspace --release

# Test all
test: test-go test-rust

# Every go.work member is named here: a repo-root `go test ./...` would cover
# them all, but nothing runs one, so scripts/check-go-module-coverage.sh reds
# `make lint` if a member is missing from this recipe (gd-9DycTnzUo3).
test-go:
	$(MAKE) -C gardenio/controller test
	$(MAKE) -C kbeads test
	cd gardenio/clicreds && go test ./...
	cd scripts/build-dictation-corpus && go test ./...
	cd scripts/wer-eval && go test ./...

# ── Go modules OUTSIDE the root go.work workspace ────────────────────────────
# go.work links gardenio/clicreds, gardenio/controller, kbeads and the two
# scripts/ modules. In workspace mode `./...` resolves only against workspace
# members, so a repo-root `go test ./...` NEVER compiles the modules below — it
# reports success having skipped them entirely. That is how bug ka-0000008045
# happened: a green root run was pushed and CI came back red on `test-helm`
# with 6 failing chart/dashboard test functions.
#
# These modules stay out of the workspace on purpose (the helm suite alone is
# ~5 min and would join every root-level build and test; lands/contract and
# lands/gl must never be able to import kbeads or the controller). The price is that
# nothing finds them by default, so each one gets a named target here and
# scripts/check-go-module-coverage.sh — wired into `make lint` — reds the build
# if a new out-of-workspace module ever appears without one.
test-out-of-workspace: test-helm test-slackmock test-grafana-plugin test-lands-contract test-lands-gl test-heraldry-proto

# Estimate the Helm release record (sh.helm.release.v1.gardenio.vN) the packed
# chart + deploy-values would store, with a per-part breakdown, and fail over
# 900 KiB of the 1 MiB Secret limit (gd-qiUiXvTX0k). Needs helm, yq, python3.
check-helm-release-size:
	bash scripts/check-helm-release-size.sh

# The gardenio chart + Grafana dashboard assertion suite (~5 min on CI, ~24 min
# on an agent pod). Mirrors the `chart tests` step of the checks.yml `helm` job,
# which installs pinned helm/yq/promtool first. The tests shell out to all three,
# so the guard below fails loudly with the missing tool named rather than with a
# confusing exec error mid-suite. NOT part of `make test`: those three binaries
# are not a given on a dev box (the agent image ships all three; promtool since
# gd-hZl1cKiELM). For chart work on a pod, run the tests you touched with
# `-run <TestName>` and leave the full suite to CI's `helm` job.
test-helm:
	@for t in helm yq promtool; do \
		command -v $$t >/dev/null 2>&1 || { \
			echo "make test-helm: '$$t' is not on PATH."; \
			echo "  The chart tests shell out to helm, yq and promtool; see the"; \
			echo "  helm job in .github/workflows/checks.yml for the versions CI pins."; \
			exit 1; \
		}; \
	done
	# -timeout is explicit because go test's silent 10m default is under this
	# suite's real runtime on a contended box, and a timeout panic reads like a
	# chart regression (gd-dnk0zE75rw). It is 40m here but 18m in the checks.yml
	# `helm` job, on purpose: CI runs the suite in ~5 min and its 18m sits under
	# the job's `timeout-minutes: 20`, while the full suite takes ~24 min on a
	# 2Gi agent pod, past 18m (gd-NW03955jbi). Do not sync them.
	cd gardenio/helm/gardenio/tests && GOWORK=off go test -timeout 40m -count=1 .

test-slack-session-guard:
	cd gardenio/tests/slack-e2e && node --test session-guard.test.mjs

test-slackmock:
	cd gardenio/tests/slackmock && GOWORK=off go test -count=1 ./...

# The Go backend of the Grafana assistant plugin. No CI job builds or tests
# this module (images.yml does not build its image; .github/known-failures.md).
test-grafana-plugin:
	cd gardenio/grafana-plugins/gardenio-assistant-app && GOWORK=off go test -count=1 ./...

# The Lands contract (lands/contract): spec rules, golden fixtures, hygiene and
# dependency guards. CI also regenerates both bindings and fails on drift
# (checks.yml `lands-contract`).
test-lands-contract:
	cd lands/contract && GOWORK=off go test -count=1 ./...

# gl, the tenant's CLI for a land (lands/gl): its commands against a fake land
# serving the contract's golden fixtures, plus the guard that it imports
# nothing but the contract binding and the shared sign-in (checks.yml
# `lands-gl`).
test-lands-gl:
	cd lands/gl && GOWORK=off go test -count=1 ./...

# The Phase 0 heraldry renderer prototype (scripts/heraldry-proto, throwaway):
# its shield geometry tests and the check that gardenio/docs/heraldry's SVGs
# are regenerated from it. Out of go.work so oksvg stays out of the workspace.
test-heraldry-proto:
	cd scripts/heraldry-proto && GOWORK=off go test -count=1 ./...

# The Lands conformance suite (Door D.11): gl and the contract's request
# fixtures through the land door against a real kd serve with two lands
# (lands/gl/conformance_test.go). Needs an EMPTY Postgres with pgvector in
# LANDS_CONFORMANCE_DATABASE_URL; the daemon migrates it. On an agent pod,
# pg-scratch (gardenio/images/agent/pg-scratch.sh) starts one. CI: ci.yml
# `lands-conformance`.
LANDS_CONFORMANCE_KD ?= $(CURDIR)/kbeads/kd-conformance
test-lands-conformance:
	@test -n "$$LANDS_CONFORMANCE_DATABASE_URL" || { echo "LANDS_CONFORMANCE_DATABASE_URL must name an empty Postgres with pgvector. On an agent pod: eval \"\$$(pg-scratch start)\" && make test-lands-conformance; pg-scratch stop" >&2; exit 1; }
	cd kbeads && GOWORK=off CGO_ENABLED=0 go build -o $(LANDS_CONFORMANCE_KD) ./cmd/kd
	cd lands/gl && LANDS_CONFORMANCE_KD=$(LANDS_CONFORMANCE_KD) GOWORK=off go test -tags conformance -run '^TestConformance$$' -count=1 -v .

# The strings tripwire (advisory, D.11): gl built the way a release is, its
# printable strings against lands/contract/hygiene_denylist.txt. Hits are
# ::warning:: lines, never a failure; it fails only when it cannot have
# scanned gl.
lands-gl-tripwire:
	cd lands/gl && GOWORK=off CGO_ENABLED=0 go build -trimpath -o $(CURDIR)/lands/gl/bin/gl-release .
	cd lands/gl && GL_TRIPWIRE_BIN=$(CURDIR)/lands/gl/bin/gl-release GOWORK=off go test -tags tripwire -run '^TestStringsTripwire$$' -count=1 -v .

# Regenerate the Go (oapi-codegen) and TypeScript (openapi-typescript) bindings
# from lands/contract/openapi.yaml. Commit the output.
generate-lands-contract:
	cd lands/contract && GOWORK=off go generate ./...
	cd lands/contract/ts && npm ci --no-audit --no-fund && npm run generate

# Aggregate per-module unit coverage profiles into a stable repo-relative path
# scheme — the contract the W3 merge job (ka-0000007391) `inputs:` against.
# See docs/research/coverage-2a-w1-go-emission.md.
COVERAGE_ROOT ?= .coverage/unit

coverage-go: test-go
	mkdir -p $(COVERAGE_ROOT)
	cp gardenio/controller/coverage/unit.out $(COVERAGE_ROOT)/gardenio-controller.out
	cp kbeads/coverage/unit.out             $(COVERAGE_ROOT)/kbeads.out

# Convert per-module Go cover profiles to LCOV via the helper script.
# Path normalization (SF: prefix rewrites) is the W3 merge job's job; this
# emits LCOV with module-local paths.
lcov-go: coverage-go
	./scripts/coverage/emit-go-lcov.sh

test-rust:
	cd coop && cargo test --workspace

# coop unit tests. Skip set mirrors the ci.yml coop `test` step — keep them
# in lock step. With COVERAGE=1 this redirects to `test-coop-coverage` which
# emits LCOV via cargo-llvm-cov to .coverage/unit/coop.lcov for the Phase-2
# coverage merge pipeline (epic ka-0000000117).
ifeq ($(COVERAGE),1)
test-coop: test-coop-coverage
else
test-coop:
	cd coop && cargo test --workspace -- \
	    --skip claude_ --skip attach_ --skip coop_vs_tmux_
endif

# cargo-llvm-cov performs its own clean rebuild with -C instrument-coverage,
# so this target uses a separate build cache from `build-coop`. The binary is
# installed on demand and cached in ~/.cargo/bin in CI.
test-coop-coverage:
	@command -v cargo-llvm-cov >/dev/null 2>&1 || cargo install cargo-llvm-cov --version "^0.6" --locked
	mkdir -p $(CURDIR)/.coverage/unit
	cd coop && cargo llvm-cov --workspace --lcov \
	    --output-path $(CURDIR)/.coverage/unit/coop.lcov \
	    -- --skip claude_ --skip attach_ --skip coop_vs_tmux_

# Lint (matches CI: golangci-lint for Go, cargo fmt + clippy -D warnings for
# Rust, helm lint, and the repo-wide quench quality gate). The final `quench`
# invocation is byte-identical to the checks.yml `quench` job so `make lint`
# reproduces the CI quench gate locally. quench is auto-installed on demand from
# git if missing (CI installs the sha256-verified release binary instead).
lint:
	# Cold golangci-lint on the controller tree is nearly all type-check/compile
	# time and has grown past 5m (5m17s measured on HEAD 486d2aac, clean run);
	# 15m matches the checks.yml lint-go ceiling (ka-0000004865).
	cd gardenio/controller && GOWORK=off golangci-lint run --timeout=15m ./...
	cd gardenio/controller && ./scripts/lint-bridge-bead-ids.sh
	bash scripts/lint-trickle-mr_test.sh
	bash scripts/lint-trust-dossier_test.sh
	bash scripts/lint-terseness-mr_test.sh
	bash scripts/lint-mr-proof_test.sh
	bash scripts/bootstrap-ec2_test.sh
	bash scripts/inbox/tarboat-validate_test.sh
	bash scripts/inbox/tarboat-file-regressions_test.sh
	bash scripts/inbox/tarboat-provision-box_test.sh
	bash scripts/inbox/build-seed-config_test.sh
	bash scripts/inbox/seed-config-dump_test.sh
	bash scripts/tarboat/assemble-tarboat_test.sh
	bash scripts/gvisor/file-next-layer-bug_test.sh
	bash scripts/gvisor/freeze-thaw-rungs_test.sh
	bash scripts/gvisor/canary-loop-driver_test.sh
	bash scripts/check-deploy-values-dup-keys_test.sh
	bash scripts/check-deploy-values-dup-keys.sh
	bash scripts/check-large-tracked-files_test.sh
	bash scripts/check-large-tracked-files.sh
	bash scripts/check-golangci-pin_test.sh
	bash scripts/check-golangci-pin.sh
	# Go module coverage guard (ka-0000008045, gd-9DycTnzUo3): every go.mod in
	# the tree must be a go.work member named by `test-go` and a CI test job, or
	# registered against a make target, so a new module cannot land invisible to
	# every local and CI test run.
	bash scripts/check-go-module-coverage_test.sh
	bash scripts/check-go-module-coverage.sh
	bash scripts/check-claude-code-currency_test.sh
	bash gardenio/scripts/release-cut_test.sh
	bash gardenio/scripts/release-deploy_test.sh
	bash gardenio/scripts/release-deploy-record_test.sh
	bash gardenio/scripts/release-deploy-edge_test.sh
	bash gardenio/scripts/machine-principal-canary_test.sh
	bash scripts/lint-advice-over-constraint_test.sh
	bash scripts/lint-advice-over-constraint.sh
	# GRADE staleness-probe guard (bug ka-0000001289): the hermetic suite (which
	# replays the inert `timestamp(last_over_time(...))` form that made the 08-31
	# and 09-01 "0 STALE" claims unmeasured) plus the lint that keeps that form
	# out of every committed docs/grafana-grade/*/ harness.
	python3 scripts/grade_staleness_probe_test.py
	python3 scripts/grade_staleness_probe.py --lint
	bash scripts/pack-gardenio-chart_test.sh
	python3 scripts/strip-helm-yaml-comments_test.py
	python3 gardenio/scripts/keycloak-theme-configmap_test.py
	# Helm release record must stay under 900 KiB (gd-qiUiXvTX0k); also a CI
	# step in the checks.yml helm job. `make check-helm-release-size` alone
	# prints the breakdown.
	bash scripts/check-helm-release-size.sh
	# Ratchets (gd-d29n0m678l): the per-package line budget. The function caps
	# ride golangci-lint above and below; the deadcode ratchet runs only in CI
	# (lint-go), since it builds SSA for every shipped main.
	bash scripts/ratchets/line-budget_test.sh
	bash scripts/ratchets/line-budget.sh check
	bash scripts/ratchets/deadcode-ratchet_test.sh
	python3 scripts/ratchets/func-baseline_test.py
	cd kbeads && GOWORK=off golangci-lint run --timeout=5m ./...
	cd coop && cargo fmt --check && cargo clippy --workspace -- -D warnings
	helm lint gardenio/helm/gardenio/
	@command -v quench >/dev/null 2>&1 || cargo install quench --git https://github.com/alfredjeanlab/quench
	quench check --ci

# Shrink every ratchet baseline to what the tree has now (gd-d29n0m678l):
# the line budget and the funlen/gocyclo lists in each .golangci.yml. Both
# only ever lower or drop entries. Safe on a pod (source parse, ~1 min). The
# deadcode baselines need a whole-program build; run
# `scripts/ratchets/deadcode-ratchet.sh update <module>` where memory allows.
ratchets-shrink:
	bash scripts/ratchets/line-budget.sh update
	python3 scripts/ratchets/func-baseline.py gardenio/controller
	python3 scripts/ratchets/func-baseline.py kbeads
	python3 scripts/ratchets/func-baseline.py gardenio/clicreds

# The old calver release DAG lived in RWX (.rwx/release.yml + promote.yml,
# archived under docs/archive/rwx/root/); RWX is entirely absent from this
# checkout (no .rwx/, no preflight-targets.sh, no `make preflight` target),
# so there is no RWX preflight to call. Substitute this repo's own
# Landing-the-Plane checklist (root CLAUDE.md) as the pre-tag gate instead --
# it is the gate that actually exists and passes today. release.sh never
# pushes anything itself: its final step only prints the atomic
# `git push --atomic origin main <TAG>` command, so publishing stays a
# separate, explicit action.
release-dry-run:
	./gardenio/scripts/release.sh --dry-run

release: build test test-out-of-workspace lint
	helm lint gardenio/helm/gardenio/
	./gardenio/scripts/release.sh
# Install repo-managed git hooks (pre-commit gitleaks scan, etc.)
hooks:
	./scripts/install-hooks.sh

# Run gitleaks against the full working tree (same invocation as CI)
gitleaks:
	gitleaks detect --no-banner --redact --exit-code=1 --config .gitleaks.toml

# Run beads-viewer locally against a port-forwarded beads daemon.
# Auth is bypassed via BEADS_VIEWER_DEV_AUTH_BYPASS=1 — see
# gardenio/docs/beads-viewer-dev.md for the full flow (kubectl port-forward,
# BEADS_DAEMON_TOKEN, etc.). Override BEADS_HTTP_ADDR or LISTEN_ADDR on the
# make invocation, e.g. `make run-beads-viewer LISTEN_ADDR=:8096`.
BEADS_VIEWER_ADDR ?= http://localhost:8080
BEADS_VIEWER_LISTEN ?= :8095
run-beads-viewer: fetch-viewer-vendor
	cd gardenio/controller && \
		BEADS_VIEWER_DEV_AUTH_BYPASS=1 \
		BEADS_HTTP_ADDR=$(BEADS_VIEWER_ADDR) \
		LISTEN_ADDR=$(BEADS_VIEWER_LISTEN) \
		LOG_LEVEL=debug \
		go run ./cmd/beads-viewer/

# Capture canonical beads-viewer pages with Playwright (desktop 1280x800 +
# narrow 480x800). Requires a beads-viewer reachable at BEADS_VIEWER_URL
# (default http://localhost:8095) — see gardenio/docs/beads-viewer-dev.md for
# the local dev boot recipe. Artifacts land in
# gardenio/tests/beads-viewer-screenshots/test-results/beads-viewer-screenshots/.
screenshot-beads-viewer:
	cd gardenio/tests/beads-viewer-screenshots && npm install --no-audit --no-fund
	cd gardenio/tests/beads-viewer-screenshots && npx playwright install --with-deps chromium
	cd gardenio/tests/beads-viewer-screenshots && npm test

# Hermetic /threads visual capture matrix (task 3-3, ka-0000004063). Boots a
# mock beads daemon with pinned fixtures + the real beads-viewer binary, then
# sweeps SKINS x WIDTHS x cells (index/detail/bead-show chip) over the Phase-3
# visual rubric ka-0000007635 (V1-V12). No Postgres / Keycloak / port-forward.
# PNGs land in gardenio/tests/beads-viewer-screenshots/test-results/threads/.
screenshot-threads:
	cd gardenio/tests/beads-viewer-screenshots && npm install --no-audit --no-fund
	cd gardenio/tests/beads-viewer-screenshots && npx playwright install --with-deps chromium
	gardenio/tests/beads-viewer-screenshots/tools/run-threads-capture.sh

# Seeded end-to-end /threads verification (task 3-6, ka-0000006435). Boots a
# PERSISTING throwaway beads daemon seeded with a multi-member thread + the real
# beads-viewer binary, then drives the LIVE compose -> ?since= poll -> GET
# /threads/{id}/audit chain and asserts structured PASS on every C/K/O gate
# (rubric ka-0000007635 A). Unlike screenshot-threads' read-only mock, this
# daemon persists writes so the composed bead is the bead the audit reads back.
# No Postgres / Keycloak / port-forward. Set VIEWER_BIN to a prebuilt binary to
# skip the go-run recompile.
e2e-threads-seeded:
	cd gardenio/controller && go build -o /tmp/beads-viewer-e2e ./cmd/beads-viewer/
	VIEWER_BIN=/tmp/beads-viewer-e2e OUT_DIR=test-results/threads-e2e \
		gardenio/tests/beads-viewer-screenshots/tools/run-e2e-seeded.sh

# Behavioral Playwright E2E suite for beads-viewer (ka-0000004611). The
# Playwright config auto-boots the viewer via `webServer:` — no separate
# `run-beads-viewer` shell needed — but a beads daemon must be reachable
# at BEADS_HTTP_ADDR (default http://localhost:8080; typically a kubectl
# port-forward of svc/gardenio-beads). See gardenio/docs/beads-viewer-dev.md
# § E2E harness.
test-beads-viewer-e2e:
	cd gardenio/tests/beads-viewer-e2e && npm install --no-audit --no-fund
	cd gardenio/tests/beads-viewer-e2e && npx playwright install --with-deps chromium
	cd gardenio/tests/beads-viewer-e2e && npm test

# Browser checks for the /garden Homestead page (gd-D3G9dMA1TU): Chromium under
# playwright-core hovers and taps plots against /garden on an httptest server
# fed the fixture farm, driven by TestCardUnderChromium; TestPorchChatUnderChromium
# walks the in-garden Porch chat and Mailbox (gd-AXfec5afZo). Hermetic: no daemon,
# no viewer binary. Without gardenio/tests/gardenhub-browser/node_modules the
# Go test skips, so the ordinary package run never needs a browser. -timeout: the walk took 820s of the old 15m once the
# splash's time controls (TestSplashControlsUnderChromium, about 6 min of SwiftShader frames) joined it (gd-rAAWVP4qOj).
# The splash walks (GARDENHUB_SPLASH_WALKS, about 755s of SwiftShader frames on main, 1.3x that on a slow runner) run in
# their own go test with their own 25m, after the rest (about 650s): one shared 25m ran at 85-99% and panicked under
# runner load, naming whichever test was running (gd-RWh73aIIGW). Both runs always go, so one red run shows both.
# GARDENHUB_BROWSER_SKIP: a -skip regex, set by CI to the quarantined `go` rows of main's .github/quarantine.d.
GARDENHUB_SPLASH_WALKS := ^Test(Splash[A-Za-z]*|ExportSplash)UnderChromium$$
test-gardenhub-browser: gardenhub-browser-deps
	cd gardenio/controller || exit 1; rc=0; \
	go test ./internal/gardenhub/ ./cmd/beads-viewer/ -run 'UnderChromium$$' -skip '$(GARDENHUB_SPLASH_WALKS)$(if $(GARDENHUB_BROWSER_SKIP),|$(GARDENHUB_BROWSER_SKIP))' -timeout 25m -count=1 -v || rc=1; \
	go test ./internal/gardenhub/ -run '$(GARDENHUB_SPLASH_WALKS)' $(if $(GARDENHUB_BROWSER_SKIP),-skip '$(GARDENHUB_BROWSER_SKIP)') -timeout 25m -count=1 -v || rc=1; \
	exit $$rc

# An agent pod has Chromium pre-installed in a read-only PLAYWRIGHT_BROWSERS_PATH (/ms-playwright), where the install fails
# with EACCES; carry on when a Chromium is already there, so the goldens and commons-ux-shoot targets still run.
gardenhub-browser-deps:
	cd gardenio/tests/gardenhub-browser && npm install --no-audit --no-fund
	@# Agent images before gd-vK5BdVNUc6 bake Chromium into a read-only /ms-playwright: use it rather than fail EACCES.
	cd gardenio/tests/gardenhub-browser && if [ -n "$${PLAYWRIGHT_BROWSERS_PATH}" ] && [ ! -w "$$PLAYWRIGHT_BROWSERS_PATH" ] && ls -d "$$PLAYWRIGHT_BROWSERS_PATH"/chromium-* >/dev/null 2>&1; then \
		echo "gardenhub-browser-deps: $$PLAYWRIGHT_BROWSERS_PATH is read-only; using its baked-in Chromium"; \
	else { npx playwright-core install chromium || ls -d "$${PLAYWRIGHT_BROWSERS_PATH:-/nonexistent}"/chromium-* >/dev/null; }; fi

# beads-viewer's mermaid.min.js and echarts.min.js (gd-wNQzR9ptZI): upstream release files, fetched and sha256-checked
# into templates/static instead of committed. Run before building or testing beads-viewer outside make.
.PHONY: fetch-viewer-vendor
fetch-viewer-vendor:
	gardenio/controller/cmd/beads-viewer/fetch-vendored.sh

# The splash's three.js (gd-xN0lNXOT4l): re-download the pinned release, check its hash, bundle it into the one ES module
# the splash serves and check that hash too. Pins and provenance: gardenio/controller/internal/gardenhub/web/vendor/VENDOR.md.
.PHONY: vendor-three
vendor-three:
	scripts/vendor-three.sh

# Golden screenshots of /garden (gd-D953GccBTY): TestGardenGoldens shoots each surface at phone and desktop widths, light
# and dark, and the phone sideways in light (M4.1 gd-T3JPWCD9xM, which also holds every phone shot to the rubric's C3 probes),
# with pinned fonts and a frozen clock, and pixel-diffs every shot against
# gardenio/controller/internal/gardenhub/testdata/golden/. TestComposerGoldens does the same for the beads viewer's empty
# thread composer, the house voice (gd-5e6xgIOH6B), against gardenio/controller/cmd/beads-viewer/testdata/golden/, and
# TestLandsGoldens for the Lands front of house (gd-WFIUrR8dZA) against its lands/ subdirectory. A
# failure leaves actual/expected/diff PNGs in gardenio/tests/gardenhub-browser/golden-diffs/{garden,composer,lands}/. An intentional change: `make update-garden-goldens`, then commit the
# PNGs in the same PR. Linux only (the goldens are rendered there); how-to in gardenio/tests/gardenhub-browser/GOLDENS.md.
.PHONY: gardenhub-browser-deps test-gardenhub-browser test-garden-goldens update-garden-goldens
# -timeout: go test's silent 10m default is under TestGardenGoldens's real runtime (468-589s on ordinary runs, over 600s on a
# slow ARC runner, gd-AteqeWk2or), and a timeout panic reads like a golden regression. 20m went the same way as the sweep
# grew (870s and 1063s on main 2026-10-27, gd-JWgIQDt4Yr); the test logs its slowest surfaces ("golden time") for when
# 30m does too. Since gd-EwwEfGfe6H the sweep runs in up to 4 shards at once, one per CPU (GARDENHUB_GOLDEN_SHARDS=N
# overrides, 1 is the old serial sweep). ci.yml's gardenhub-browser job's timeout-minutes is sized to fit this and
# test-gardenhub-browser's -timeout.
test-garden-goldens: gardenhub-browser-deps
	cd gardenio/controller && GARDENHUB_GOLDENS=1 go test ./internal/gardenhub/ ./cmd/beads-viewer/ -run '^Test(Garden|Composer|Lands)Goldens$$' -timeout 30m -count=1 -v

update-garden-goldens: gardenhub-browser-deps
	cd gardenio/controller && GARDENHUB_GOLDENS=1 GARDENHUB_GOLDEN_UPDATE=1 go test ./internal/gardenhub/ ./cmd/beads-viewer/ -run '^Test(Garden|Composer|Lands)Goldens$$' -timeout 30m -count=1 -v

# Shoot /garden surfaces for a Commons UX grade (gd-vzsNYKoGNX): every capture-matrix cell of
# gardenio/docs/commons-ux-rubric.md, its DOM probes and a draft grade per surface, into OUT. Not a check; the
# commons-ux-acceptance skill (.claude/skills/commons-ux-acceptance/SKILL.md) runs it and grades the PNGs.
# SURFACES / CELLS are comma lists that narrow the run, e.g. `make commons-ux-shoot OUT=/tmp/cux SURFACES=cursorproject,bead`.
# A run that names SURFACES also measures them against the rubric's perf budgets (section 7, gd-qeaDwFSDqq), about 4 min a
# surface: PERF=0 skips it, PERF=1 forces it on a full run, PERF_RUNS (3) and PERF_IDLE (60 s) shorten it.
.PHONY: commons-ux-shoot
commons-ux-shoot: gardenhub-browser-deps
	@test -n "$(OUT)" || { echo "usage: make commons-ux-shoot OUT=<dir> [SURFACES=a,b] [CELLS=a,b] [PERF=0|1] [PERF_RUNS=3] [PERF_IDLE=60]"; exit 2; }
	cd gardenio/controller && COMMONS_UX_OUT="$(abspath $(OUT))" COMMONS_UX_SURFACES="$(SURFACES)" COMMONS_UX_CELLS="$(CELLS)" \
		$(if $(PERF),COMMONS_UX_PERF="$(PERF)") $(if $(PERF_RUNS),COMMONS_UX_PERF_RUNS="$(PERF_RUNS)") $(if $(PERF_IDLE),COMMONS_UX_PERF_IDLE="$(PERF_IDLE)") \
		go test ./internal/gardenhub/ -run '^TestCommonsUXShoot$$' -timeout 120m -count=1 -v

# The network inventory of every /garden surface (P0.4, gd-hsekIVjOWI): each fetch and EventSource while idle, while the
# farm is busy, and across a dropped connection and a 502 outage, into OUT/netinv.{md,json}. Not a check. A full run is
# about 40 minutes (5 min idle + 5 min active, three surfaces at a time); IDLE_S / ACTIVE_S shorten it, SURFACES narrows
# it, e.g. `make garden-netinv OUT=/tmp/ni IDLE_S=30 ACTIVE_S=30 SURFACES=world,bead`. Last results: gardenio/docs/perf/.
.PHONY: garden-netinv
garden-netinv: gardenhub-browser-deps
	@test -n "$(OUT)" || { echo "usage: make garden-netinv OUT=<dir> [IDLE_S=300] [ACTIVE_S=300] [SURFACES=a,b]"; exit 2; }
	cd gardenio/controller && GARDENHUB_NETINV_OUT="$(abspath $(OUT))" NETINV_IDLE_S="$(IDLE_S)" NETINV_ACTIVE_S="$(ACTIVE_S)" \
		NETINV_SURFACES="$(SURFACES)" go test ./internal/gardenhub/ -run '^TestNetworkInventory$$' -count=1 -v -timeout 90m

# Measure /garden and each Lands surface cold and warm on a throttled phone and a desktop (Lands performance pass P0.1,
# gd-j8jXq1TC1m): page weight, requests, TTFB, FCP, LCP, TTI, INP, long tasks and the idle heap, into OUT/results.json and
# OUT/summary.md. Not a check. A full run takes about an hour; SURFACES, PROFILES (phone,desktop), RUNS (3) and IDLE
# (seconds, 300; 0 skips the idle pass) narrow it, SETTLE (seconds, 10; 0 skips) the settled window after each cold
# load's first interaction, PAN (runs, default RUNS; 0 skips) the World's pan pass on the phone (B7: world_pan_profile.js's
# calibration, 10 s pan and zoom on the 10x farm), and TRACE=<dir> adds one traced, unmeasured cold load per surface on the
# phone (<dir>/phone-<surface>.zip). LATENCY=1 adds the Commons v2 latency pass (B12-B15) on the Commons channel and thread
# rows, on a farm whose Commons is live (perfWorld). Against a deployed /garden: run testdata/perf_baseline.js by hand (its header).
.PHONY: lands-perf-baseline
lands-perf-baseline: gardenhub-browser-deps
	@test -n "$(OUT)" || { echo "usage: make lands-perf-baseline OUT=<dir> [SURFACES=a,b] [PROFILES=phone,desktop] [RUNS=3] [IDLE=300] [SETTLE=10] [PAN=<runs>] [TRACE=<dir>] [LATENCY=1]"; exit 2; }
	cd gardenio/controller && LANDS_PERF_OUT="$(abspath $(OUT))" LANDS_PERF_SURFACES="$(SURFACES)" LANDS_PERF_PROFILES="$(PROFILES)" \
		$(if $(RUNS),LANDS_PERF_RUNS="$(RUNS)") $(if $(IDLE),LANDS_PERF_IDLE="$(IDLE)") $(if $(SETTLE),LANDS_PERF_SETTLE="$(SETTLE)") $(if $(PAN),LANDS_PERF_PAN="$(PAN)") \
		$(if $(LATENCY),LANDS_PERF_LATENCY="$(LATENCY)") \
		$(if $(TRACE),LANDS_PERF_TRACE="$(abspath $(TRACE))") \
		go test ./internal/gardenhub/ -run '^TestLandsPerfBaseline$$' -timeout 150m -count=1 -v

# The World's render profile on the 10x farm (Lands performance pass P2.1, gd-PoCdbE1SJX): idle, a 10 s pan and a zoom on
# the throttled phone, into OUT/report.json; fails when a long task in the pan runs over 100 ms. Not a check (a shared
# runner's phone numbers move with its load; the CI gate grades the same pan as B7, against its calibration floor). About 2 minutes. Knobs (CPU throttle, DPR, CPU profile, trace) are in the
# header of gardenio/controller/internal/gardenhub/testdata/world_pan_profile.js; results: gardenio/docs/lands-perf-world-render.md.
.PHONY: world-pan-profile
world-pan-profile: gardenhub-browser-deps
	@test -n "$(OUT)" || { echo "usage: make world-pan-profile OUT=<dir>"; exit 2; }
	cd gardenio/controller && WORLD_PAN_OUT="$(abspath $(OUT))" go test ./internal/gardenhub/ -run '^TestWorldPanProfile$$' -timeout 20m -count=1 -v

# The Lands performance gate (P3.1, gd-7Ns0SDsF7R): grade a lands-perf-baseline run (HEAD) against the B1-B17 budgets of
# gardenio/docs/commons-ux-rubric.md section 7, next to a run of the base (BASE), into OUT/gate.md and OUT/gate.json;
# exits 1 when a budget regressed. .github/workflows/lands-perf.yml runs it nightly on main and on dispatch, never on a
# PR; the rules are in testdata/perf_gate.js's header. test-lands-perf-gate is its self-test, which also holds its
# budgets to the rubric's table.
.PHONY: lands-perf-gate test-lands-perf-gate
lands-perf-gate:
	@test -n "$(HEAD)" && test -n "$(OUT)" || { echo "usage: make lands-perf-gate HEAD=<results dir> [BASE=<results dir>] OUT=<dir>"; exit 2; }
	node gardenio/controller/internal/gardenhub/testdata/perf_gate.js "$(HEAD)/results.json" $(if $(BASE),"$(BASE)/results.json") "$(OUT)"

test-lands-perf-gate:
	node --test gardenio/controller/internal/gardenhub/testdata/perf_gate.test.js

# Seed canonical fixtures into the beads daemon (ka-0000006317 / T2).
# Idempotent against the same daemon — re-running yields the same ids.
# `make test-beads-viewer-e2e` calls this implicitly via Playwright's
# globalSetup; this target exists for inner-loop dev when iterating on
# fixtures or the seed script itself.
seed-beads-viewer-e2e:
	node gardenio/tests/beads-viewer-e2e/seed/seed.mjs

# Clean
clean:
	rm -rf gardenio/controller/bin/
	rm -rf kbeads/bin/
	cd coop && cargo clean
	rm -rf gardenio/controller/coverage/ kbeads/coverage/ .coverage/
