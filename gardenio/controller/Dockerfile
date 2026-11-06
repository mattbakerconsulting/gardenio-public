ARG GO_VERSION=1.25
FROM golang:${GO_VERSION} AS builder

WORKDIR /build
# Build context is gardenio/ (not controller/) because controller/go.mod has
# `replace gardenio/clicreds => ../clicreds`.
COPY controller/go.mod controller/go.sum ./
COPY clicreds/ /clicreds/
RUN go mod download

COPY controller/ ./
ARG VERSION=dev
ARG COMMIT=unknown
ARG COVER=0
RUN if [ "$COVER" = "1" ]; then \
      COVER_FLAGS="-cover -coverpkg=./..."; \
    else \
      COVER_FLAGS=""; \
    fi && \
    CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build ${COVER_FLAGS} \
    -ldflags="-s -w -X main.version=${VERSION} -X main.commit=${COMMIT}" \
    -o /controller ./cmd/controller/

FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=builder /controller /controller
USER nonroot:nonroot
ENTRYPOINT ["/controller"]
