package services

import (
	"context"
	"math"
	"strconv"
	"sync"
	"time"

	"code.cloudfoundry.org/bytefmt"
	"github.com/golang-jwt/jwt/v4"
	"github.com/juju/ratelimit"
	"github.com/pkg/errors"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/redis/go-redis/v9"
	"github.com/sirupsen/logrus"
	"github.com/webtor-io/lazymap"
)

// Throttler abstracts bandwidth throttling so both the legacy ratelimit.Bucket
// and the new HybridBucket can be used interchangeably.
type Throttler interface {
	Wait(count int64)
}

// Verify that *ratelimit.Bucket satisfies Throttler at compile time.
var _ Throttler = (*ratelimit.Bucket)(nil)

// luaReserve debits a reservation from a session's balance in Redis, and
// returns how long the caller must wait before spending it: the seconds the
// balance takes to accrue back to zero, as a string ("0" when it stays
// non-negative). A string because Dragonfly returns a Lua number as a
// double and Redis truncates it to an integer.
//
// KEYS[1] is a Hash {tokens, last_tick}, the layout and units of the
// grant-only script this replaced, so old and new pods draw on one balance
// during a rollout: the old script grants nothing while tokens is negative.
// ARGV: [1] capacity, [2] rate (bytes/sec), [3] bytes to reserve, [4] now
// (ms, the caller's clock), [5] ttl (sec), [6] "1" to drop what accrued
// while the caller could not reach Redis.
//
// The balance may go negative: a debt that accrual repays at rate, so the
// reservations of all callers are spaced at rate whatever their number.
// last_tick only moves forward: a pod whose clock lags accrues nothing
// until the clock catches up, instead of accruing the lag again.
var luaReserve = redis.NewScript(`
local cap  = tonumber(ARGV[1])
local rate = tonumber(ARGV[2])
local now  = tonumber(ARGV[4])
local vals = redis.call("HMGET", KEYS[1], "tokens", "last_tick")
local tokens = tonumber(vals[1]) or cap
local last   = tonumber(vals[2]) or now
if now > last then
    tokens = math.min(cap, tokens + (now - last) / 1000 * rate)
    last = now
end
if ARGV[6] == "1" and tokens > 0 then tokens = 0 end
tokens = tokens - tonumber(ARGV[3])
redis.call("HSET", KEYS[1], "tokens", tostring(tokens), "last_tick", tostring(last))
redis.call("EXPIRE", KEYS[1], tonumber(ARGV[5]))
if tokens >= 0 then return "0" end
return tostring(-tokens / rate)
`)

// leaseSeconds is how much of its rate a bucket reserves in one Redis call.
// The bucket spends it locally over the following writes: one call per a
// quarter second of a session's traffic, whatever the write size or the
// number of its streams. It also bounds what a stream that stops leaves
// unspent on its pod.
const leaseSeconds = 0.25

// probeInterval is how often a bucket that fell back to local pings Redis.
var probeInterval = 5 * time.Second

var (
	promLimiterRedisCalls = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "webtor_http_proxy_limiter_redis_calls_total",
		Help: "HTTP Proxy bandwidth limiter reservations made in Redis, by result",
	}, []string{"result"})
	promLimiterWaiting = prometheus.NewGauge(prometheus.GaugeOpts{
		Name: "webtor_http_proxy_limiter_waiting",
		Help: "HTTP Proxy writes asleep in the bandwidth limiter",
	})
	promLimiterLocalFallbacks = prometheus.NewCounter(prometheus.CounterOpts{
		Name: "webtor_http_proxy_limiter_local_fallbacks_total",
		Help: "HTTP Proxy bandwidth limiter buckets that fell back to a local balance after a failed Redis call",
	})
)

func init() {
	prometheus.MustRegister(promLimiterRedisCalls, promLimiterWaiting, promLimiterLocalFallbacks)
}

// HybridBucket limits one session at one rate. Its balance is in Redis,
// shared by every pod; this pod reserves a lease from it and spends that
// locally. While Redis is unreachable it limits on a local balance instead,
// shared by the session's streams on this pod.
type HybridBucket struct {
	mu       sync.Mutex
	rate     float64 // bytes per second
	capacity float64 // max burst (bytes)

	// lease is bytes reserved in Redis and not yet allotted to a write;
	// paidAt is when the balance has accrued the last of them. They are
	// paid in order, the next one first: the first n of them by
	// paidAt - (lease-n)/rate.
	lease  float64
	paidAt time.Time

	// The local balance (may go negative) and when it last accrued.
	tokens     float64
	lastRefill time.Time

	rc       redis.UniversalClient
	redisKey string
	redisOK  bool
	probing  bool
	// resync makes the next reservation drop the Redis balance's surplus:
	// it accrued while this pod paced the session locally.
	resync bool
}

// NewHybridBucket returns sessionID's bucket at rate. Its Redis balance is
// shared with every bucket of the same session and rate, on any pod, and
// with no other: see bucketKey.
func NewHybridBucket(rate float64, capacity float64, rc redis.UniversalClient, sessionID string) *HybridBucket {
	return &HybridBucket{
		rate:       rate,
		capacity:   capacity,
		lastRefill: time.Now(),
		rc:         rc,
		redisKey:   "bw:limit:" + bucketKey(sessionID, rate),
		redisOK:    rc != nil,
	}
}

// bucketKey names one session's bucket at one rate: the suffix of its Redis
// key and its key in HybridBucketPool. A session holds tokens at two rates
// at once (its tier's, and grace's 50M on grace segments); on one key
// whichever drained the balance would leave the other nothing, and each
// call would clamp it to its own capacity. The rate is the one the Lua
// script is told (ARGV[2], whole bytes per second), not the claim's
// spelling ("5M" and "5MB" are one bucket). It never contains ':', so the
// last ':' splits the key whatever the sessionID holds. Capacity is not in
// it: the pool always sets it to the rate.
func bucketKey(sessionID string, bytesPerSec float64) string {
	return sessionID + ":" + strconv.FormatInt(int64(bytesPerSec), 10)
}

// Wait blocks until count bytes may be sent: until the session's balance
// has accrued them, after the bytes allotted to the writes before it.
func (hb *HybridBucket) Wait(count int64) {
	if count <= 0 {
		return
	}
	hb.mu.Lock()
	at := hb.take(float64(count))
	hb.mu.Unlock()
	limiterSleep(time.Until(at))
}

// take allots the next n bytes and returns when they are paid. It spends
// the lease first; past it, one Redis call reserves at least leaseSeconds
// of the rate. Caller holds mu, through the Redis call too: a lease is
// reserved only when the last one is all allotted, so the pod holds at
// most one lease unallotted, and a write waits for its own bytes, not for
// a lease of its own behind those of the session's other streams.
func (hb *HybridBucket) take(n float64) time.Time {
	if hb.lease >= n {
		hb.lease -= n
		return hb.paidAt.Add(-secondsDur(hb.lease / hb.rate))
	}
	// The rest of the lease is paid before the next one.
	n -= hb.lease
	hb.lease = 0
	if hb.redisOK {
		lease := max(n, hb.rate*leaseSeconds)
		now := time.Now()
		d, err := hb.reserve(lease, hb.resync)
		if err == nil {
			hb.resync = false
			hb.lease = lease - n
			hb.paidAt = now.Add(d)
			return hb.paidAt.Add(-secondsDur(hb.lease / hb.rate))
		}
		hb.fallBack()
	}
	return time.Now().Add(hb.reserveLocal(n))
}

func secondsDur(s float64) time.Duration {
	return time.Duration(s * float64(time.Second))
}

// reserveLocal debits n from the local balance and returns how long to
// wait for it. One balance for all of the session's streams on this pod:
// N of them get the rate between them, not N times it. Caller holds mu.
func (hb *HybridBucket) reserveLocal(n float64) time.Duration {
	now := time.Now()
	if el := now.Sub(hb.lastRefill).Seconds(); el > 0 {
		hb.tokens = min(hb.capacity, hb.tokens+el*hb.rate)
		hb.lastRefill = now
	}
	hb.tokens -= n
	if hb.tokens >= 0 || hb.rate <= 0 {
		return 0
	}
	return time.Duration(-hb.tokens / hb.rate * float64(time.Second))
}

// fallBack switches to the local balance, starting empty now: what the
// session drew from Redis until a moment ago is not known here, and a full
// one would hand it a burst on every failed call. Caller holds mu.
func (hb *HybridBucket) fallBack() {
	if !hb.redisOK {
		return
	}
	hb.redisOK = false
	hb.resync = true
	hb.tokens = 0
	hb.lastRefill = time.Now()
	promLimiterLocalFallbacks.Inc()
	if !hb.probing {
		hb.probing = true
		go hb.probeRedis(probeInterval)
	}
}

func limiterSleep(d time.Duration) {
	if d <= 0 {
		return
	}
	promLimiterWaiting.Inc()
	time.Sleep(d)
	promLimiterWaiting.Dec()
}

// reserve runs luaReserve for n bytes and returns how long to wait for
// them.
func (hb *HybridBucket) reserve(n float64, resync bool) (time.Duration, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()

	flag := "0"
	if resync {
		flag = "1"
	}
	res, err := luaReserve.Run(ctx, hb.rc, []string{hb.redisKey},
		int64(hb.capacity),     // ARGV[1] cap
		int64(hb.rate),         // ARGV[2] rate (bytes/sec)
		int64(math.Ceil(n)),    // ARGV[3] reserved
		time.Now().UnixMilli(), // ARGV[4] now_ms
		300,                    // ARGV[5] ttl 5min
		flag,                   // ARGV[6] resync
	).Text()
	var sec float64
	if err == nil {
		sec, err = strconv.ParseFloat(res, 64)
	}
	if err == nil && (math.IsNaN(sec) || math.IsInf(sec, 0) || sec < 0) {
		err = errors.Errorf("reservation returned %q", res)
	}
	if err != nil {
		promLimiterRedisCalls.WithLabelValues("error").Inc()
		logrus.WithError(err).Warn("Redis token-bucket call failed, falling back to local")
		return 0, err
	}
	promLimiterRedisCalls.WithLabelValues("ok").Inc()
	return time.Duration(sec * float64(time.Second)), nil
}

// probeRedis pings Redis every interval until it responds, then re-enables
// it.
func (hb *HybridBucket) probeRedis(interval time.Duration) {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for range ticker.C {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		err := hb.rc.Ping(ctx).Err()
		cancel()
		if err == nil {
			hb.mu.Lock()
			hb.redisOK = true
			hb.probing = false
			hb.mu.Unlock()
			logrus.Info("Redis connection restored for bandwidth limiting")
			return
		}
	}
}

// HybridBucketPool manages HybridBucket instances per (session, rate) via
// lazymap.
type HybridBucketPool struct {
	*lazymap.LazyMap[Throttler]
	rc redis.UniversalClient
}

func NewHybridBucketPool(rc redis.UniversalClient) *HybridBucketPool {
	return &HybridBucketPool{
		LazyMap: lazymap.New[Throttler](&lazymap.Config{
			Expire: 5 * 60 * time.Second,
		}),
		rc: rc,
	}
}

func (s *HybridBucketPool) Get(mc jwt.MapClaims) (Throttler, error) {
	sessionID, ok := mc["sessionID"].(string)
	if !ok {
		return nil, nil
	}
	if sessionID == "" {
		// web-ui mints sessionID "" for a viewer it has no session for yet
		// (a first page without its cookie). On that key every such viewer
		// on every pod drew on one bucket; the address web-ui minted the
		// token for, signed like the session, keys it instead. An IP and a
		// sessionID (hex) never spell the same key.
		sessionID, _ = mc["remoteAddress"].(string)
	}
	rate, ok := mc["rate"].(string)
	if !ok {
		return nil, nil
	}
	bytesPerSec, err := rateBytesPerSec(rate)
	if err != nil {
		return nil, err
	}
	return s.LazyMap.Get(bucketKey(sessionID, bytesPerSec), func() (Throttler, error) {
		// capacity == rate: at most one second of idle accrual, no extra
		// burst beyond what the configured rate allows.
		return NewHybridBucket(bytesPerSec, bytesPerSec, s.rc, sessionID), nil
	})
}

// rateBytesPerSec converts a token's rate claim, bits per second in bytefmt
// units ("5M", "50M"), to bytes per second. Under 1 byte per second ("0M")
// is refused: the script is told whole bytes per second and would divide
// by zero.
func rateBytesPerSec(rate string) (float64, error) {
	r, err := bytefmt.ToBytes(rate)
	if err != nil || r < 8 {
		return 0, errors.Errorf("failed to parse rate %v", rate)
	}
	return float64(r) / 8, nil
}
