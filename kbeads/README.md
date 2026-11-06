# Beads

An event-driven work-tracking system. Manages hierarchical work items ("beads") with dependencies, labels, comments, and event history. Exposes gRPC and REST APIs and ships a Cobra-based CLI (`kd`).

## Quick start

```sh
go build ./cmd/kd

export BEADS_DATABASE_URL="postgres://user:pass@localhost:5432/beads?sslmode=disable"
kd serve
```

Or via Docker:

```sh
docker build -t kd .
docker run -e BEADS_DATABASE_URL="..." -p 9090:9090 -p 8080:8080 kd
```

## CLI examples

```sh
kd create "Fix login bug" --type bug
kd list --status open
kd show ka-abc123
kd show ka-abc123 ka-def456 --json   # batch: JSON array, one object per bead
kd update ka-abc123 --status in_progress
kd close ka-abc123
kd comment ka-abc123 "Root cause was a nil pointer"
kd label ka-abc123 add backend
kd dep ka-abc123 add ka-def456
kd search "login"
```

Custom types can be registered at runtime:

```sh
kd config create type:decision '{"kind":"issue","fields":[{"name":"outcome","type":"enum","values":["approved","rejected","pending"],"required":true}]}'
kd create "Approve Q1 roadmap" --type decision --fields '{"outcome":"pending"}'
```

## Configuration

| Variable | Default | Purpose |
|---|---|---|
| `BEADS_DATABASE_URL` | *(required)* | Postgres connection string |
| `BEADS_GRPC_ADDR` | `:9090` | gRPC listen address |
| `BEADS_HTTP_ADDR` | `:8080` | HTTP listen address |
| `BEADS_NATS_URL` | *(optional)* | NATS event bus URL |
| `BEADS_SERVER` | `localhost:9090` | CLI client target address |

## Testing

```sh
go test ./...    # uses go-sqlmock; no running Postgres needed
```
