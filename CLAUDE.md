# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

## Project Overview

`torrent-http-proxy` is a Go HTTP proxy for the [webtor.io](https://github.com/webtor-io) platform. It routes requests to internal Kubernetes services/jobs, deploys jobs on demand, provides JWT-based authentication, and supports matryoshka-style service chaining via URL path modifications (e.g., `~hls`, `~vod`).

## Build & Run

```bash
# Build
go build -o server

# Run (requires a YAML config file)
./server --config config.yaml

# Docker build
docker build -t torrent-http-proxy .
```

There are no tests, linters, or Makefile configured in this project.

## Architecture

### Entry Point & Initialization

`main.go` → `configure.go`: The app uses `urfave/cli` v1. All flags are registered in `configure()`, and `run()` constructs the full service dependency graph manually (no DI framework).

### Request Flow

```
HTTP Request → Web.ServeHTTP()
  → URLParser.Parse()       — extracts info-hash, path, and modification chain from URL
  → Claims.Get()            — validates JWT token or API key
  → Resolver.Resolve()      — maps edge type + role to a ServiceConfig
  → ServiceLocationPool     — locates the target service (K8s endpoints or env vars)
  → HTTPProxy               — reverse-proxies the request to the resolved location
  → ResponseWriterInterceptor — captures status, bytes written, TTFB
  → ClickHouse (optional)   — stores analytics records
```

### Key Services (all in `services/`)

| Service | File | Purpose |
|---------|------|---------|
| **Web** | `web.go` | Main HTTP handler; orchestrates the full request pipeline |
| **URLParser** | `url_parser.go` | Parses URLs into `Source` structs with info-hash, path, mods (`~` delimited) |
| **Resolver** | `resolver.go` | Resolves service type/mod to a `ServiceConfig`, supports role-specific variants |
| **ServiceLocationPool** | `service_location.go` | Finds service endpoints via K8s or env; supports Hash/NodeHash distribution |
| **HTTPProxy** | `http_proxy.go` | Cached reverse proxies (60s TTL via `lazymap`) |
| **Claims** | `claims.go` | JWT/API key validation; extracts role, rate, sessionID |
| **Bucket** | `bucket.go` | Token-bucket rate limiting per session (via `juju/ratelimit`) |
| **ClickHouse** | `clickhouse.go`, `clickhouse_db.go` | Batched analytics insert to ClickHouse |
| **AccessHistory** | `access_history.go` | IP+UA based access rate limiting (5 unique per 3 hours) |

### Kubernetes Integration (`services/k8s/`)

- **Client** (`client.go`): Lazy-initialized K8s client (in-cluster or local kubeconfig)
- **Endpoints** (`endpoints.go`): Queries K8s service endpoints with 60s cache TTL
- **NodesStat** (`nodes_stat.go`): Lists ready nodes with role labels (prefix: `webtor.io/`)

### Configuration

Service routing is defined in a YAML config file (`--config` / `CONFIG_PATH` env var). Each entry maps a service type to a K8s service name and distribution strategy:

```yaml
default:
  name: torrent-web-seeder
  distribution: NodeHash
  preferLocalNode: true
hls:
  name: content-transcoder
  distribution: NodeHash
```

Distribution strategies: `Hash` (by info-hash) and `NodeHash` (by node then hash).

### URL Structure

URLs follow the pattern: `/{info-hash}/{file-path}~{mod1}/{mod-path}~{mod2}/...`

The `~` delimiter triggers service chaining — each modification maps to a different backend service via the YAML config.

### Caching Pattern

The project uses `github.com/webtor-io/lazymap` extensively for TTL-based lazy-loading caches (service locations, HTTP proxies, K8s endpoints, buckets). This is the primary caching abstraction throughout the codebase.

### Observability

- **Prometheus metrics** on configurable port (request duration, TTFB, bytes, current connections)
- **Health probe** on port 8081
- **pprof** on configurable port
- Metric labels: `source` (Internal/External), `role`, `name` (service), `status`