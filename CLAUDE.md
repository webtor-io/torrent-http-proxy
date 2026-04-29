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
| **Claims** | `claims.go` | JWT/API key validation; extracts role, rate, sessionID; `Rule` shape + `ExtractRules` helper for grace tokens |
| **Rules pipeline** | `rules.go`, `manifest_rewriter.go` | `applyResponseRules` (called from `modifyResponse`) dispatches to a registry of `responseRuleHandler`s based on the `RulesContext` attached to the request. Currently: `rewriteManifestForGrace` swaps `?token=PRIMARY`→`?token=GRACE` per segment in HLS m3u8 while movie-time stays inside the grace window |
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

### Rules pipeline

Tokens may carry a `rules` claim — typed as `[]Rule{Kind, Scope, DurationSec, Token}` in `claims.go`. THP processes them through two generic extension points so new rule kinds plug in without touching the proxy hook:

1. **`RulesContext`** (`claims.go`): per-request bundle (`Claims` + `PrimaryToken` + `InfoHash`) attached to the request in `web.go` `proxyHTTP` via `WithRulesContext`. This is the only thing rule handlers need to make decisions.

2. **`applyResponseRules`** (`rules.go`): single dispatcher invoked from `modifyResponse`. Iterates over `responseRuleHandlers` — a registry of `func(r *http.Response, rc *RulesContext) error` handlers. Each handler self-gates against `rc.Claims` rules and the response (path, content-type). Adding a new response-side rule = appending to the registry.

Currently registered handlers:

- **`rewriteManifestForGrace`** (`manifest_rewriter.go`): for `kind=grace, scope=manifest`. On `.m3u8` responses, parses `#EXT-X-SESSION-OFFSET:<sec>` (defaults to 0 — back-compat with content-transcoder versions that don't emit the tag), walks `#EXTINF`/segment lines, and swaps `?token=PRIMARY` → `?token=GRACE` for segments whose movie-time start is below `rule.duration_sec`. No-op when no grace rule.

Request-side rule check (runs before the response pipeline):

- **Hash binding** (`web.go` `proxyHTTP`): if the token carries a `hash` claim, the request is rejected (403) unless `hash` equals the request's infohash. Prevents replay across content for any hash-bound token kind. Applied early — before resolver, bucket, etc.

Gating of rule emission lives upstream — web-ui only issues grace rules when its `GRACE_RULES_ENABLED` flag is on. THP has no flag; absence of the rule short-circuits.

See `web-ui/docs/grace_token.md` for the full grace-token design (token shape, anti-fraud, rollout).

### Observability

- **Prometheus metrics** on configurable port (request duration, TTFB, bytes, current connections)
- **Health probe** on port 8081
- **pprof** on configurable port
- Metric labels: `source` (Internal/External), `role`, `name` (service), `status`