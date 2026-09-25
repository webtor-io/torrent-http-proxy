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

Tests live in `services/`, plus `configure_test.go` for `run()`'s startup
order: `go test -race -count=3 ./...`, all green, `TestClickHouse` included
(fixed in c990de3). The throttle, session-stats and shutdown suites test
atomics and goroutine handoffs, so `-race` is part of the bar. No linters or
Makefile configured.

## Architecture

### Entry Point & Initialization

`main.go` → `configure.go`: The app uses `urfave/cli` v1. All flags are registered in `configure()`, and `run()` constructs the full service dependency graph manually (no DI framework).

### Startup and shutdown

- `run()` calls `web.Listen()` before `cs.NewServe` starts any servable. The probe (`cs.Probe`) answers `/readiness` 200 as soon as it listens and the DaemonSet's `maxSurge: 1` retires the old pod on it, so the web port is bound first; a port that cannot be bound returns from `run()` before the probe exists (`TestRunBindsWebBeforeProbe`)
- Shutdown: `cs.Serve` returns on SIGTERM, then `run()` calls `web.Close()` explicitly, before its deferred closes (ClickHouse, Redis, probe, prom): proxyHTTP's deferred closure does `clickHouse.Add`. No `defer web.Close()`: the explicit call is the one place, and a defer would add a drain of up to the timeout to a panic
- `Web.Close`: `close(closing)` → `cs.GracefulServer.Close` (listener closed, in-flight requests get `--shutdown-timeout` / `WEB_SHUTDOWN_TIMEOUT`, chart 60s, then cut; returns once handlers have unwound) → stats janitor. Idempotent, concurrent-safe, safe before `Serve` (which then returns nil)
- `closing` ends the streams that never end on their own, or each would hold the drain for the whole timeout and be cut anyway: session-stats streams, and the seeder's `?stats` streams (web-ui's status page), whose outbound request `proxyHTTP` cancels on it; web-ui reopens them on the new pod. Keyed on the `stats` query key as the seeder dispatches, not on `text/event-stream`: `?warmup` is drained, its EOF reads as "warmup complete" (`TestWebCloseDrainsWarmupStream`)
- In prod the drain still runs the whole timeout on a busy pod (downloads outlast 60 s), so the Warn `web shutdown timed out, closing remaining connections` is expected on rollouts
- First rollout of this image + chart does not drain: old pods terminate under their own spec (no preStop, grace 30) and run the old binary, which exits on SIGTERM; only `maxSurge: 1` applies. Judge the drain on the second rollout: `closing web` / `web closed` in the old pod's log ~10 s after deletion, ingress 502s and TTFB count vs the first rollout and vs offset 1d

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
| **Web** | `web.go` | Main HTTP handler; orchestrates the full request pipeline; `Listen`/`Serve`/`Close` with a graceful drain (`cs.GracefulServer`) |
| **URLParser** | `url_parser.go` | Parses URLs into `Source` structs with info-hash, path, mods (`~` delimited) |
| **Resolver** | `resolver.go` | Resolves service type/mod to a `ServiceConfig`, supports role-specific variants |
| **ServiceLocationPool** | `service_location.go` | Finds service endpoints via K8s or env; supports Hash/NodeHash distribution |
| **HTTPProxy** | `http_proxy.go` | Cached reverse proxies (60s TTL via `lazymap`) |
| **Claims** | `claims.go` | JWT/API key validation; extracts role, rate, sessionID; `Rule` shape + `ExtractRules` helper for grace tokens |
| **Rules pipeline** | `rules.go`, `manifest_rewriter.go` | `applyResponseRules` (called from `modifyResponse`) dispatches to a registry of `responseRuleHandler`s based on the `RulesContext` attached to the request. Currently: `rewriteManifestForGrace` swaps `?token=PRIMARY`→`?token=GRACE` per segment in HLS m3u8 while movie-time stays inside the grace window |
| **Bucket** | `bucket.go` | Token-bucket rate limiting per session (via `juju/ratelimit`) |
| **retryTransport** | `retry_transport.go` | Wraps 200/206 bodies in `retryingReadCloser`: on dirty upstream close mid-stream, reconnects to a same-node fallback pod and resumes via Range. Resume offset derives from the RESPONSE (206 Content-Range, or 0 for 200) — never trust the request Range blindly; suffix ranges without Content-Range are left unwrapped. Fully-delivered bodies (bytesRead ≥ Content-Length) and retry-416-at-size (`Content-Range: bytes */total`) surface as clean io.EOF, not failures |
| **ClickHouse** | `clickhouse.go`, `clickhouse_db.go` | Batched analytics insert to ClickHouse |
| **AccessHistory** | `access_history.go` | IP+UA based access rate limiting (5 unique per 3 hours) |
| **SessionStats** | `session_stats.go` | Per-pod live totals per (sessionID, domain, infohash) behind `GET /session-stats/<infohash>` (SSE); see Observability |

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
- Metric labels: `source` (`internal`/`external`, lowercase), `role`, `name` (service), `status`

#### Tier-bound vs upstream-bound (throttle metrics)

- Population: responses a limiter was installed on (external, `--use-bandwidth-limit`, token with `rate` + `sessionID`), minus `text/event-stream` (seeder `?stats`, warmup)
- Counters `{source,role,name}`: `webtor_http_proxy_throttle_wait_seconds_total` (in limiter `Wait`), `webtor_http_proxy_throttled_request_downstream_seconds_total` (blocked writing downstream), `webtor_http_proxy_throttled_request_duration_seconds_total` (whole response). Remainder = upstream + TTFB
- Tier-bound share: `sum by (role,name) (rate(webtor_http_proxy_throttle_wait_seconds_total{source="external"}[1h])) / sum by (role,name) (rate(webtor_http_proxy_throttled_request_duration_seconds_total{source="external"}[1h]))`. Not `request_duration_seconds_sum`: it also holds internal, limiter-less and SSE requests
- `webtor_http_proxy_throttle_ratio{role,name,download}`: per-response wait/duration, 2xx of ≥ max(1 MiB, 4 s of the rate). Smaller ones fit the bucket burst (capacity = 1 s of rate, +1 s prefetched on Redis) and read ≈ 0 whatever the upstream did
- The histogram is one sample per response: multi-connection downloaders dominate its counts. Shares come from the counters; per-session answers from Loki (`throttled`, `duration`, `bytes` by `session_id`)
- `download` = the query key is present (seeder semantics). Also carries paid Stremio and WebDAV playback, which redirect to the download export URL
- Closing log line: `bytes` and `downstream_blocked` (s) always; `throttled` (s) only with a limiter, so absent means no limiter, not 0; `req_id` = ingress `X-Request-ID`, the last field of the ingress access log
- Blind spot: "downstream" is ingress-nginx with proxy buffering (~260 MiB per response). A client slower than the tier is invisible until that fills, and the limiter-paced response reads tier-bound. On segment traffic (HLS, small ranges) each response fits the buffer whole and it never fills: every segment reads tier-bound whatever the client's speed. Join on `req_id` and compare ingress `request_time` with `upstream_response_time`
- Blind spot: grace segment tokens have `rate` but no `sessionID`, so grace segments are not limited at all (their `rate=50M` log field is the claim, not an applied cap) and appear in no throttle metric. Free-tier HLS data covers only post-grace viewing

#### Session stats: `GET /session-stats/<infohash>?token=&api-key=`

- web-ui's resource status bar: the viewer's own speed and "limited by your plan". Served by thp itself (mux, next to `/speedtest`), never proxied; code in `services/session_stats.go`
- Auth: an ordinary viewer token web-ui mints server side (HS256, API secret, as grace tokens) bound by the standard `hash` claim = infohash (case-insensitive); no special scope — the owner chose the standard mechanism. Also required: non-empty `sessionID`, numeric `exp` later than the current second (`tokenExpiry`; jwt-go alone accepts a missing or non-numeric `exp`, and a token in its `exp` second, whose stream would end at once). `iat`/`nbf` ahead of thp's clock fail in jwt-go with no leeway: the contract tells web-ui to leave them out. Anything else 403, a missing `sessionID` included; infohash not 40 hex 400, checked before the token. Wrong-kind tokens log Warn `not a session-stats token` with `reason` (hash/exp/sessionID), never the URL
- The token is checked once, at open; the stream ends at its `exp` (wall-clock timer), so web-ui mints a new token for every open and reopen (its `sessionWatch.dial` does)
- Caps: 4 streams per key (sessionID, domain, infohash: what one token grants) → 429; 32 per sessionID → 429; 5000 per pod → 503. The per-key cap keeps a leaked token from locking its session out of other torrents (`TestSessionStatsLeakedTokenCannotLockOutSession`)
- `200 text/event-stream`, `Cache-Control: no-cache, no-store, no-transform`, `X-Accel-Buffering: no` (ingress buffers this host; without it events sit in nginx), no CORS on any status (reader is web-ui's backend). One `data: <json>\n\n` per second, the first at once, until the client leaves, the token's `exp` or `Web.Close`
- JSON: `window_sec` (5); `bytes_per_sec` over the last window (over the stream's age while younger: events 2–5 cover 1–4 s; the first spans nothing: 0 and no `throttled`, speed unknown until the second); `conns` open content requests now; `rate` claim of the latest request, absent when none; `throttled` = Δ limiter wait of all the key's requests, summed / the same wall-clock span, clamped to 0..1, absent when no limited request was open in the window (no limiter ≠ 0; the open-time integral `limitedOpen` only answers that)
- Wall time, not open time, on purpose: over open time an HLS segment reads ≈ 1 whatever the client (ingress takes it whole at the tier's pace, then the player idles), and N parallel ranges stalled mid-body split a binding tier into 1/(N+1)
- Sum, not union: a dry bucket holds all the session's requests at once and each wait counts, so N parallel requests held together 1/N of the window read 1 while the time any was held is 1/N (2026-09-24 review, real bucket, 4 ranges on a 30%-on upstream: 1.00 vs 0.44). At the cap the sum is right (the tier binds them all); below it it overstates, and consumers gate on `bytes_per_sec` near the rate as well (web-ui: `throttled ≥ 0.5` and use ≥ 0.9). The union would need a per-key count of requests inside `Wait`, updated under a lock on the 0↔1 transitions, i.e. on every Write of a lone limited request; the Write path is lock-free by design (`TestSessionStatsWriterWriteTakesNoLock`). Pinned by the `the sum, not the union` case of `TestStatsRingWindow`
- Counted: external requests with a `sessionID` and a 40-hex infohash that `proxyHTTP` hands to the upstream, final status 2xx, minus `text/event-stream` responses. The key is attached at the final `WriteHeader` or first `Write` (1xx skipped: interim headers), so `conns` excludes TTFB. Bytes and wait are added on every `Write`, so a long download shows speed while it runs
- Also counted: web-ui's own server-side fetches (probe, subtitles, transcoder session, HLS buffering polls) reach thp through the public ingress with the viewer's token (`USE_INTERNAL_TORRENT_HTTP_PROXY` is off in prod), so they read as the viewer's: before playback a KB/s trickle with `throttled` 0
- Key = (sessionID, `domain` claim, infohash): an embed's visitors all carry its owner's sessionID, the domain keeps them out of the owner's stream. Other shared sessionIDs (RapidAPI: one per customer) add up
- Per pod on purpose: rest-api puts the infohash's rendezvous home node in the export URL, ingress there reaches the local thp pod (`internalTrafficPolicy: Local`); what another pod serves is not seen. web-ui must open the stream on the node's direct-domain host (the `torrent_client_stat` item's, or an export with `use-premium-domain=false`), not a paid download/stream URL: the premium edge keeps `proxy_buffering on`, and ingress-nginx does not pass `X-Accel-Buffering` on, so ~120 B/s events sit in its 64 KiB buffers
- Entries: refcounted by open requests; a janitor (10 s) drops those idle > 60 s; at most 50 000 keys and 32 per (sessionID, domain), beyond that new keys go uncounted. Keys are copied: an entry never pins the request line its infohash was sliced from
- Metrics: `webtor_http_proxy_session_stats_streams` (gauge), `..._streams_rejected_total{reason=torrent|session|global}`, `..._entries` (gauge), `..._entries_dropped_total{reason=global|session}` (map or the session's share full; embeds with many visitors hit `session` normally)
- Blind spot, the same as above: bytes are what thp hands to ingress-nginx, which buffers ~260 MiB per response. On one large response a client slower than its tier reads `throttled` ≈ 1 until that fills (~7 min at 5M, ~40 s at 50M). On segment traffic the gaps between segments follow the client, so the share falls with it (the 2026-09-24 review measured 0.05–0.10 for slow clients, ≈ 0.99 when the tier binds). A player idling on a full buffer reads about bitrate/rate − 1 s/segment length, not bitrate/rate: after an idle gap of ≥ 1 s the bucket (capacity 1 s of the rate) lets each segment's first second of the rate through without waiting, and a segment under 1 s of the rate reads 0. Simulated with the real `HybridBucket` at 5M (2026-09-24 review): 4 s segments at 60% of the rate 0.35, 6 s at 60% 0.43, 2 s at 40% 0.00; content-transcoder cuts 4 s, nginx-vod 2/2/2/4/4/6 s. web-ui also gates the CTA on `bytes_per_sec` near the rate
- Blind spot: grace segment tokens carry no `sessionID`, so free-tier HLS inside the grace window counts only the playlist polls on the primary token. Consumers hide the speed while in grace
- Rollouts: `maxSurge: 1` runs two pods per node for a while. A stream that (re)connects lands on the new pod and does not see downloads still draining on the old one: up to the 10 s preStop plus the drain (≤ 60 s; `Close` ends the never-ending streams first)
- Exposure: export URLs (and their tokens) reach browsers and cannot open the stream. A leaked session-stats token tells whether one session fetches the one torrent it names until its `exp`, open streams included, and can take that torrent's 4 stream slots, not the session's others; it is no content credential, and no thp log line records it. Do not add the `remoteAddress` check: web-ui reads the stream server-side, from its own IP
