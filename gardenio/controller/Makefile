VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
COMMIT  ?= $(shell git rev-parse --short HEAD 2>/dev/null || echo unknown)
BRANCH  ?= $(shell git rev-parse --abbrev-ref HEAD 2>/dev/null || echo unknown)
DATE    ?= $(shell date -u +%Y-%m-%dT%H:%M:%SZ)
IMAGE   ?= gardenio-controller
LDFLAGS  = -s -w -X main.version=$(VERSION) -X main.commit=$(COMMIT)
# gb additionally reports branch + build date via `gb version` and drives the
# version-skew signal (ka-0000002965).
GB_BUILDINFO = gardenio/controller/internal/buildinfo
GB_LDFLAGS = $(LDFLAGS) -X $(GB_BUILDINFO).Branch=$(BRANCH) -X $(GB_BUILDINFO).Date=$(DATE)

# Coverage instrumentation: set COVER=1 to build with runtime coverage profiling.
# Instrumented binaries write coverage data to GOCOVERDIR at process exit.
COVER ?= 0
ifeq ($(COVER),1)
GO_COVER_FLAGS = -cover -coverpkg=./...
else
GO_COVER_FLAGS =
endif

# macOS (Apple Silicon) requires ad-hoc signing for stripped binaries
ifeq ($(shell uname -s),Darwin)
CODESIGN = codesign --force --sign -
else
CODESIGN = @true
endif

.PHONY: build build-gb build-bridge test lint docker-build clean

# Unit-test coverage emission (Phase-2a W1-Go, ka-0000001270).
# Always-on (cheap) — atomic mode is required under -race anyway, and -coverpkg=./...
# instruments cross-package call paths so the W3 merge step sees real coverage.
COVER_DIR ?= coverage
COVER_OUT ?= $(COVER_DIR)/unit.out

build:
	go build $(GO_COVER_FLAGS) -ldflags="$(LDFLAGS)" -o bin/controller ./cmd/controller/
	$(CODESIGN) bin/controller

build-gb:
	go build $(GO_COVER_FLAGS) -ldflags="$(GB_LDFLAGS)" -o bin/gb ./cmd/gb/
	$(CODESIGN) bin/gb

build-bridge:
	go build $(GO_COVER_FLAGS) -ldflags="$(LDFLAGS)" -o bin/slack-bridge ./cmd/slack-bridge/
	$(CODESIGN) bin/slack-bridge

test:
	./cmd/beads-viewer/fetch-vendored.sh
	mkdir -p $(COVER_DIR)
	go test -v -race -covermode=atomic -coverpkg=./... \
		-coverprofile=$(COVER_OUT) ./...

lint:
	golangci-lint run ./...

docker-build:
	docker build --build-arg VERSION=$(VERSION) --build-arg COMMIT=$(COMMIT) -t $(IMAGE):$(VERSION) .

clean:
	rm -rf bin/
	rm -rf $(COVER_DIR)/
