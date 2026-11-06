.PHONY: build build-bridge build-jira-bridge build-gitlab-bridge build-grafana-bridge build-claudemore test test-edge-auth test-edge-auth-integration lint e2e verify image image-agent image-bridge image-jira-bridge image-gitlab-bridge image-edge-auth image-all push push-agent push-bridge push-jira-bridge push-gitlab-bridge push-edge-auth push-all helm-package helm-template release release-dry-run clean

VERSION  ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
COMMIT   ?= $(shell git rev-parse --short HEAD 2>/dev/null || echo unknown)
BRANCH   ?= $(shell git rev-parse --abbrev-ref HEAD 2>/dev/null || echo unknown)
DATE     ?= $(shell date -u +%Y-%m-%dT%H:%M:%SZ)
REGISTRY ?= ghcr.io/mattbakerconsulting/gardenio

# macOS (Apple Silicon) requires ad-hoc signing for stripped binaries
ifeq ($(shell uname -s),Darwin)
CODESIGN = codesign --force --sign -
else
CODESIGN = @true
endif

# ── Controller ──────────────────────────────────────────────────────────

build:
	$(MAKE) -C controller build

build-gb:
	cd controller && go build -ldflags="-s -w -X main.version=$(VERSION) -X main.commit=$(COMMIT) -X gardenio/controller/internal/buildinfo.Branch=$(BRANCH) -X gardenio/controller/internal/buildinfo.Date=$(DATE)" -o bin/gb ./cmd/gb/
	$(CODESIGN) controller/bin/gb

build-bridge:
	cd controller && go build -ldflags="-s -w -X main.version=$(VERSION) -X main.commit=$(COMMIT)" -o bin/slack-bridge ./cmd/slack-bridge/
	$(CODESIGN) controller/bin/slack-bridge

build-jira-bridge:
	cd controller && go build -ldflags="-s -w -X main.version=$(VERSION) -X main.commit=$(COMMIT)" -o bin/jira-bridge ./cmd/jira-bridge/
	$(CODESIGN) controller/bin/jira-bridge

build-gitlab-bridge:
	cd controller && go build -ldflags="-s -w -X main.version=$(VERSION) -X main.commit=$(COMMIT)" -o bin/gitlab-bridge ./cmd/gitlab-bridge/
	$(CODESIGN) controller/bin/gitlab-bridge

build-grafana-bridge:
	cd controller && go build -ldflags="-s -w -X main.version=$(VERSION) -X main.commit=$(COMMIT)" -o bin/grafana-bridge ./cmd/grafana-bridge/
	$(CODESIGN) controller/bin/grafana-bridge

build-claudemore:
	cd controller && go build -ldflags="-s -w -X main.version=$(VERSION) -X main.commit=$(COMMIT)" -o bin/claudemore ./cmd/claudemore/
	$(CODESIGN) controller/bin/claudemore

test:
	$(MAKE) -C controller test

lint:
	$(MAKE) -C controller lint

e2e: build-gb
	./tests/e2e/scripts/test-pf-supervisor.sh
	./tests/e2e/scripts/test-decisions-yield.sh
	./tests/e2e/scripts/test-gate-system.sh

# Stage 3 gardenio-edge-auth scaffold-level e2e (ka-0000008114).
# Honors EDGE_AUTH_URL, NAMESPACE, GARDENIO_TOKEN — see
# tests/e2e/edge-auth/README.md.
test-edge-auth:
	./tests/e2e/edge-auth/test-edge-auth.sh

# Stage 3 gardenio-edge-auth integration tests (ka-0000008114).
# Runs the in-tree Go integration tests under cmd/edge-auth:
#   - hmac_integration_test.go (W-B-9, ka-0000001042) — no build tag, always on
#   - ocsf_integration_test.go (W-B-10, ka-0000004490) — no build tag, always on
#   - pat_integration_test.go (W-B-8, ka-0000005404) — //go:build integration,
#     opt-in, requires GITLAB_TEST_URL + GITLAB_TEST_GROUP + four PAT env vars
#     (see file header for the full env contract).
# This target runs both the default suite and the integration-tagged build so
# the PAT cases are compiled (and skipped at runtime when env vars are unset)
# every cycle — guarding against bit-rot in the integration-tagged file.
test-edge-auth-integration:
	cd controller && go test -race -count=1 ./cmd/edge-auth/...
	cd controller && go test -race -count=1 -tags=integration ./cmd/edge-auth/...

# ── Docker ──────────────────────────────────────────────────────────────

image:
	docker build \
		--platform linux/amd64 \
		--build-arg VERSION=$(VERSION) \
		--build-arg COMMIT=$(COMMIT) \
		-t $(REGISTRY)/controller:$(VERSION) \
		-t $(REGISTRY)/controller:latest \
		controller/

image-agent:
	docker build \
		--platform linux/amd64 \
		-t $(REGISTRY)/agent:$(VERSION) \
		-t $(REGISTRY)/agent:latest \
		-f images/agent/Dockerfile .

image-bridge:
	docker build \
		--platform linux/amd64 \
		--build-arg VERSION=$(VERSION) \
		--build-arg COMMIT=$(COMMIT) \
		-t $(REGISTRY)/slack-bridge:$(VERSION) \
		-t $(REGISTRY)/slack-bridge:latest \
		-f images/slack-bridge/Dockerfile .

image-jira-bridge:
	docker build \
		--platform linux/amd64 \
		--build-arg VERSION=$(VERSION) \
		--build-arg COMMIT=$(COMMIT) \
		-t $(REGISTRY)/jira-bridge:$(VERSION) \
		-t $(REGISTRY)/jira-bridge:latest \
		-f images/jira-bridge/Dockerfile .

image-gitlab-bridge:
	docker build \
		--platform linux/amd64 \
		--build-arg VERSION=$(VERSION) \
		--build-arg COMMIT=$(COMMIT) \
		-t $(REGISTRY)/gitlab-bridge:$(VERSION) \
		-t $(REGISTRY)/gitlab-bridge:latest \
		-f images/gitlab-bridge/Dockerfile .

image-edge-auth:
	docker build \
		--platform linux/amd64 \
		--build-arg VERSION=$(VERSION) \
		--build-arg COMMIT=$(COMMIT) \
		-t $(REGISTRY)/edge-auth:$(VERSION) \
		-t $(REGISTRY)/edge-auth:latest \
		-f images/edge-auth/Dockerfile .

image-all: image image-agent image-bridge image-jira-bridge image-gitlab-bridge image-edge-auth

push: image
	docker push $(REGISTRY)/controller:$(VERSION)
	docker push $(REGISTRY)/controller:latest

push-agent: image-agent
	docker push $(REGISTRY)/agent:$(VERSION)
	docker push $(REGISTRY)/agent:latest

push-bridge: image-bridge
	docker push $(REGISTRY)/slack-bridge:$(VERSION)
	docker push $(REGISTRY)/slack-bridge:latest

push-jira-bridge: image-jira-bridge
	docker push $(REGISTRY)/jira-bridge:$(VERSION)
	docker push $(REGISTRY)/jira-bridge:latest

push-gitlab-bridge: image-gitlab-bridge
	docker push $(REGISTRY)/gitlab-bridge:$(VERSION)
	docker push $(REGISTRY)/gitlab-bridge:latest

push-edge-auth: image-edge-auth
	docker push $(REGISTRY)/edge-auth:$(VERSION)
	docker push $(REGISTRY)/edge-auth:latest

push-all: push push-agent push-bridge push-jira-bridge push-gitlab-bridge push-edge-auth

# ── Helm ────────────────────────────────────────────────────────────────

helm-template:
	helm template gardenio helm/gardenio/ \
		--set agents.enabled=true \
		--set coopmux.enabled=true \
		--set slackBridge.enabled=true

helm-package:
	helm package helm/gardenio/ --version $(VERSION) --app-version $(VERSION)

# ── Release ────────────────────────────────────────────────────────────

release:
	./scripts/release.sh

release-dry-run:
	./scripts/release.sh --dry-run

verify:
	./scripts/verify-deploy.sh --version=$(VERSION)

# ── Clean ───────────────────────────────────────────────────────────────

clean:
	$(MAKE) -C controller clean
	rm -f gardenio-*.tgz
