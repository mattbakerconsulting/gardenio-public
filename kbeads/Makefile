.PHONY: build test proto clean

# Unit-test coverage emission (Phase-2a W1-Go, ka-0000001270).
# Atomic mode + -coverpkg=./... so cross-package call paths are instrumented
# for the W3 merge step. Always-on; profiles are cheap.
COVER_DIR ?= coverage
COVER_OUT ?= $(COVER_DIR)/unit.out

# Build-time version stamps for `kd version`/`kd --version` and the
# version-skew signal (ka-0000002965). Injected via ldflags; a plain
# `go build` still backfills commit+date from the Go VCS stamps.
VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
COMMIT  ?= $(shell git rev-parse --short HEAD 2>/dev/null || echo unknown)
BRANCH  ?= $(shell git rev-parse --abbrev-ref HEAD 2>/dev/null || echo unknown)
DATE    ?= $(shell date -u +%Y-%m-%dT%H:%M:%SZ)
BUILDINFO = gardenio/kbeads/internal/buildinfo
LDFLAGS = -X main.Version=$(VERSION) -X main.Commit=$(COMMIT) \
	-X $(BUILDINFO).Branch=$(BRANCH) -X $(BUILDINFO).Date=$(DATE)

build:
	go build -ldflags "$(LDFLAGS)" -o kd ./cmd/kd

test:
	mkdir -p $(COVER_DIR)
	go test -covermode=atomic -coverpkg=./... \
		-coverprofile=$(COVER_OUT) ./...

proto:
	./scripts/gen-proto.sh

clean:
	rm -rf $(COVER_DIR)/
