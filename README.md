# torrent-http-proxy

Special HTTP-proxy that has several features:

1. Routes requests to internal kubernetes resources (services/jobs).
2. Deploys kubernetes job on demand.

   For example if path `/08ada5a7a6183aae1e09d831df6748d566095a10/Sintel%2FSintel.mp4` was called
   then a new [torrent-web-seeder](https://github.com/webtor-io/torrent-web-seeder) job will be started with injected environment
   variable `INFO_HASH=08ada5a7a6183aae1e09d831df6748d566095a10`. Proxy will wait until pod will be ready and then proxy requst to
   it with path `/Sintel%2FSintel.mp4`. All following requests will be proxied to this pod.

3. Grants HTTP-access to GRPC-services (including jobs)
4. Provides Token-authentication
5. Performs chaining of service calls (matryoshka-style)

   For example `/08ada5a7a6183aae1e09d831df6748d566095a10/Sintel%2FSintel.mp4~hls/index.m3u8` will be processed with following steps:
   
   1. Proxy deploys [content-transcoder](https://github.com/webtor-io/content-transcoder) job with injected environment variable `SOURCE_URL=%PROXY_URL%/08ada5a7a6183aae1e09d831df6748d566095a10/Sintel%2FSintel.mp4`. `~hls` is the keyword that indicates what
   job or service should be invoked.
   2. [content-transcoder](https://github.com/webtor-io/content-transcoder) requests `SOURCE_URL` for transcoding.
   3. Proxy deploys [torrent-web-seeder](https://github.com/webtor-io/torrent-web-seeder) job with injected environment variable `INFO_HASH=08ada5a7a6183aae1e09d831df6748d566095a10`.
   4. Proxy serves `/index.m3u8` from [content-transcoder](https://github.com/webtor-io/content-transcoder)

   There might be more services in chain. There is no limitation.

## Server usage

```
% ./torrent-http-proxy help
NAME:
   torrent-http-proxy - Proxies all the things

USAGE:
   torrent-http-proxy [global options] command [command options] [arguments...]

VERSION:
   0.0.1

COMMANDS:
   help, h  Shows a list of commands or help for one command

GLOBAL OPTIONS:
   --host value                     listening host
   --port value                     http listening port (default: 8080)
   --jwt-secret value               JWT Secret [$SECRET]
   --redis-host value               redis host (default: "localhost") [$REDIS_MASTER_SERVICE_HOST, $ REDIS_SERVICE_HOST]
   --redis-port value               redis port (default: 6379) [$REDIS_MASTER_SERVICE_PORT, $ REDIS_SERVICE_PORT]
   --job-node-affinity-key value    Node Affinity Key [$JOB_NODE_AFFINITY_KEY]
   --job-node-affinity-value value  Node Affinity Key [$JOB_NODE_AFFINITY_VALUE]
   --job-namespace value            Job namespace (default: "webtor") [$JOB_NAMESPACE]
   --probe-host value               probe listening host
   --probe-port value               probe listening port (default: 8081)
   --help, -h                       show help
   --version, -v                    print the version
```

## Routing by infohash

Services with `distribution: NodeHash` are resolved in two rendezvous
steps (`services/rendezvous.go`: rank candidates by sha1(infohash,
candidate), highest wins). First the node, over node names — rest-api
ranks nodes with an identical copy of the function to send the client to
the node this proxy will call home, and lists the runners-up as fallbacks;
both repositories pin the same literal test vector. Then the pod within
that node, over pod IPs. A node or pod that leaves moves only the hashes
it owned; one that joins takes an even share from each. A retry that
excludes the failed pod lands on the same runner-up from every proxy
instance. `distribution: Hash` is the pod step alone.

## Shutdown

On SIGTERM the proxy stops accepting connections and lets the requests in
flight (downloads, segments) finish for up to `--shutdown-timeout` /
`WEB_SHUTDOWN_TIMEOUT` (default 20s; the chart sets 60s), then cuts what is
still open; a client resumes with Range. Streams that never end on their own
end first: session-stats streams and the seeder's `?stats` streams (web-ui's
status page, which reopens them on the new pod). `?warmup` streams are
drained like downloads: their end means "warmup complete". ClickHouse, Redis
and the probe close only after the drain, so a finishing request still gets
its analytics row. Keep `terminationGracePeriodSeconds` at least preStop
sleep + timeout + 2s.

A download longer than the timeout still holds the drain to its end: on a
busy pod the drain runs the whole timeout and logs the Warn `web shutdown
timed out, closing remaining connections`. That is expected on a rollout; it
means downloads were cut, not that the drain is stuck.

The first rollout of this version does not drain: each old pod terminates
under its own spec (no preStop, the old grace period) and runs the old
binary, which exits on SIGTERM. `maxSurge: 1` does apply, so every node gets
a Ready replacement first. Judge the drain on the second rollout
(`kubectl -n webtor rollout restart ds/torrent-http-proxy`): `closing web`
and `web closed` in the old pod's log about 10 s after its deletion, and
ingress 502s and the TTFB count against the first rollout and against the
same hour a day before.

The web port is bound before the probe starts listening, so the pod cannot
answer Ready while its web port is unbound; a port that cannot be bound ends
the process before any probe is served.

## Session stats

Contract for web-ui:

```
GET <scheme>://<node host>/session-stats/<infohash>?token=<JWT minted by web-ui: sessionID, domain, hash=<infohash>, the viewer's usual claims and exp, no iat/nbf>&api-key=<key>
host = the host of a thp export URL for this resource fetched with use-premium-domain=false (premium edge buffers SSE).
200 text/event-stream, X-Accel-Buffering: no, no CORS. Event every 1 s: {"window_sec":5,"bytes_per_sec":…, "conns":…, "active":true|false, "rate":"5M"?, "throttled":0..1?} — active = a content request of this session for this infohash was open at some moment since this stream's previous event (open now, or ended since, however short; the first event: open now); a request counts, in active as in conns, from its final response headers: one still waiting upstream for its first byte (a transcoder segment not produced yet) is in neither, however long it waits; judge presence by active, not conns: conns is read once a second and misses segment fetches shorter than that. Proxies before active send no such field: fall back to conns > 0 or bytes_per_sec > 0 (the latter stays up to window_sec after the last byte). throttled = the limiter wait of this session's requests for this infohash, summed, over the window's wall time, clamped to 1 (one request: the share of time the limiter held it; N parallel requests held together read 1 at 1/N of the time, so judge the plan by throttled with bytes_per_sec near rate); omitted when no limited request was open in the window. First event has a zero-length window: treat its speed as unknown; until the stream is window_sec old, bytes_per_sec and throttled cover only its age.
The stream ends at the token's exp and on thp shutdown: mint a new token for every open and reopen. 403 wrong or expired token, 429 over 4 streams per (session, domain, infohash) or 32 per session, 503 over 5000 per pod.
```

The proxy serves this itself. It streams Server-Sent Events, one per second
(the first one right away), about what this proxy instance delivers to the
token's session for that torrent:

```
data: {"window_sec":5,"bytes_per_sec":655360,"conns":1,"active":true,"rate":"5M","throttled":0.93}
```

- `bytes_per_sec`: bytes delivered to the session for the torrent over the
  last `window_sec` seconds (over the stream's age while it is younger)
- `conns`: content requests open now, read once a second. A segment
  fetched in less than that between two readings is not in it
- `active`: a content request was open at some moment since this stream's
  previous event: one is open now, or one ended since, however short. The
  first event has no previous one and says whether one is open now. A
  request is open, here and in `conns`, from its final response headers on:
  one still waiting upstream for its first byte (content-transcoder holds a
  segment request until the segment is produced) is in neither, however
  long it waits. Always present, `false` included; a proxy without it sends
  no such field. Tabs of one viewer each get their own answer
- `rate`: the rate claim of the latest content request; absent when none
- `throttled`: the limiter wait of all the session's requests for the
  torrent, summed, over the same span of wall time (the stream's age while
  it is younger), clamped to 1. For one request it is the share of the time
  the limiter held it. It is not the share of time any request was held:
  the session's requests share one bucket, a dry bucket holds them all at
  once and each one's wait counts, so N parallel requests held together for
  1/N of the window already read 1. With `bytes_per_sec` near the rate that
  is the tier binding; well below it, the sum overstates. Absent when no
  bandwidth-limited request was open in the window, which is not the same
  as 0

The first event covers no time yet: `bytes_per_sec` is 0 and `throttled`
is absent even while a download runs. Treat the speed as unknown until the
second event. Events two to five cover the 1 to 4 seconds the stream has
been open, not `window_sec`, so they swing with where a segment fetch falls.

Content counts when it is a 2xx, non-event-stream response to a request
with the same `sessionID` and `domain` claims for that torrent. A token
without `sessionID` (such as a grace segment token) counts nowhere.

The token is HS256 with the API secret, the one web-ui signs grace tokens
with: the viewer's ordinary claims plus the standard `hash` claim = the
infohash (any case), a non-empty `sessionID` and a numeric `exp` later than
the current second; `domain` keys the stream like content (absent reads as
`default`). Leave `iat` and `nbf` out: they are checked with no leeway, so a
web-ui clock a second ahead of the proxy's makes a fresh token a 403. The
token is checked when the stream opens and the stream ends at its `exp`, so
mint a new one for every open and reopen. Export tokens, which browsers see,
carry no `hash` and never open the stream. The stream's token is an ordinary
token bound to one torrent: on content it serves that torrent at the viewer's
rate, like the export token, so mint it with the viewer's `rate`.

Status codes: 400 for an infohash that is not 40 hex digits (checked first),
403 for anything wrong with the token or api-key (a missing `sessionID`
included), 429 over 4 streams per (session, domain, infohash) or 32 per
session, 503 over 5000 streams. The per-torrent cap is what one token can
take, so a leaked token cannot lock its session out of other torrents. No
answer carries CORS headers: the reader is web-ui's backend. Counts are per
proxy instance, so call it on the same node as the content URLs, on a host
with no buffering proxy in front: the events are ~120 bytes a second and a
buffering proxy holds them back.
