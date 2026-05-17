package services

import (
	"context"
	"strconv"
	"sync"
	"time"

	"code.cloudfoundry.org/bytefmt"
	"github.com/dgrijalva/jwt-go"
	"github.com/juju/ratelimit"
	"github.com/pkg/errors"
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

// luaTokenBucket is an atomic Redis token-bucket implemented as a Lua script.
// Key layout: KEYS[1] is a Redis Hash with fields "tokens" and "last_tick".
// ARGV: [1] capacity, [2] rate (tokens/sec), [3] requested, [4] now_ms, [5] ttl_sec.
// Returns: number of tokens actually granted (may be 0).
var luaTokenBucket = redis.NewScript(`
local key   = KEYS[1]
local cap   = tonumber(ARGV[1])
local rate  = tonumber(ARGV[2])
local req   = tonumber(ARGV[3])
local now   = tonumber(ARGV[4])
local ttl   = tonumber(ARGV[5])

local vals = redis.call("HMGET", key, "tokens", "last_tick")
local tokens   = tonumber(vals[1]) or cap
local lastTick = tonumber(vals[2]) or now

-- accrue tokens since last tick
local elapsed = (now - lastTick) / 1000  -- seconds
if elapsed > 0 then
    tokens = tokens + elapsed * rate
    if tokens > cap then tokens = cap end
end

-- grant up to what is available
local granted = 0
if tokens >= req then
    granted = req
    tokens  = tokens - req
elseif tokens > 0 then
    granted = tokens
    tokens  = 0
end

redis.call("HMSET", key, "tokens", tostring(tokens), "last_tick", tostring(now))
redis.call("EXPIRE", key, ttl)
return tostring(granted)
`)

// HybridBucket implements Throttler with a two-tier token bucket:
// a fast local tier and a global Redis tier that acts as the source of truth.
type HybridBucket struct {
	mu         sync.Mutex
	local      float64   // current local token balance (bytes)
	rate       float64   // bytes per second
	capacity   float64   // max burst (bytes)
	lastRefill time.Time // for local accrual when Redis is unavailable

	rc       redis.UniversalClient
	redisKey string
	redisOK  bool
	probing  bool
}

func NewHybridBucket(rate float64, capacity float64, rc redis.UniversalClient, sessionID string) *HybridBucket {
	return &HybridBucket{
		local:      0, // start empty — first write goes to Redis for coordination
		rate:       rate,
		capacity:   capacity,
		lastRefill: time.Now(),
		rc:         rc,
		redisKey:   "bw:limit:" + sessionID,
		redisOK:    rc != nil,
	}
}

// Wait blocks until count tokens are available, satisfying the Throttler interface.
// Design: at most one Redis call per Write(); sleep for any deficit. No retry loop.
func (hb *HybridBucket) Wait(count int64) {
	if count <= 0 {
		return
	}
	need := float64(count)

	hb.mu.Lock()
	canRedis := hb.redisOK && hb.rc != nil

	// When Redis is unavailable, accrue tokens locally by elapsed time
	// (graceful degradation — same behavior as the old ratelimit.Bucket).
	if !canRedis {
		now := time.Now()
		elapsed := now.Sub(hb.lastRefill).Seconds()
		if elapsed > 0 {
			hb.local += elapsed * hb.rate
			if hb.local > hb.capacity {
				hb.local = hb.capacity
			}
			hb.lastRefill = now
		}
	}

	// Fast path: serve entirely from local tokens.
	if hb.local >= need {
		hb.local -= need
		hb.mu.Unlock()
		return
	}

	// Consume whatever local tokens are available.
	need -= hb.local
	hb.local = 0
	hb.mu.Unlock()

	if !canRedis {
		// Local-only degraded mode: single sleep is the best we can do.
		// N parallel waiters here each advance independently, so total
		// throughput scales with concurrency — acceptable trade-off
		// when Redis is unavailable.
		if hb.rate > 0 {
			sleepDur := time.Duration(need / hb.rate * float64(time.Second))
			if sleepDur > 0 {
				time.Sleep(sleepDur)
			}
		}
		return
	}

	// Slow path: poll Redis until satisfied. A single-shot Redis call
	// followed by a local sleep would let N parallel waiters each
	// independently sleep need/rate and then write, yielding N×rate total
	// throughput regardless of the configured limit. Looping back to Redis
	// on each partial/empty grant pins total throughput to Redis's accrual
	// rate (hb.rate) regardless of waiter count.
	for need > 0 {
		// Prefetch up to 1 second's worth on a single Redis call to
		// amortize round-trips when contention is low.
		batch := need
		if hb.rate > batch {
			batch = hb.rate
		}
		granted := hb.refillFromRedis(batch)
		if granted >= need {
			hb.mu.Lock()
			hb.local += granted - need
			hb.mu.Unlock()
			return
		}
		if granted > 0 {
			need -= granted
		}
		// If Redis flipped to unavailable mid-loop, degrade and exit.
		hb.mu.Lock()
		stillRedisOK := hb.redisOK
		hb.mu.Unlock()
		if !stillRedisOK {
			if hb.rate > 0 {
				sleepDur := time.Duration(need / hb.rate * float64(time.Second))
				if sleepDur > 0 {
					time.Sleep(sleepDur)
				}
			}
			return
		}
		// Sleep for the time Redis needs to refill the deficit, capped
		// so we re-poll often and share fairly with other waiters; floored
		// at 1ms to avoid busy-waiting on sub-ms grants.
		sleepDur := time.Duration(need / hb.rate * float64(time.Second))
		if sleepDur > 100*time.Millisecond {
			sleepDur = 100 * time.Millisecond
		}
		if sleepDur < time.Millisecond {
			sleepDur = time.Millisecond
		}
		time.Sleep(sleepDur)
	}
}

// refillFromRedis executes the Lua token-bucket script against Redis.
// Returns number of tokens granted. On error, marks Redis as unavailable
// and starts a probe goroutine.
func (hb *HybridBucket) refillFromRedis(requested float64) float64 {
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()

	nowMs := time.Now().UnixMilli()
	result, err := luaTokenBucket.Run(ctx, hb.rc, []string{hb.redisKey},
		int64(hb.capacity),  // ARGV[1] cap
		int64(hb.rate),      // ARGV[2] rate (bytes/sec)
		int64(requested),    // ARGV[3] requested
		nowMs,               // ARGV[4] now_ms
		300,                 // ARGV[5] ttl 5min
	).Text()

	if err != nil {
		logrus.WithError(err).Warn("Redis token-bucket call failed, falling back to local")
		hb.mu.Lock()
		hb.redisOK = false
		shouldProbe := !hb.probing
		hb.probing = true
		hb.mu.Unlock()
		if shouldProbe {
			go hb.probeRedis()
		}
		return 0
	}

	granted, _ := strconv.ParseFloat(result, 64)
	return granted
}

// probeRedis pings Redis every 5 seconds until it responds, then re-enables it.
func (hb *HybridBucket) probeRedis() {
	ticker := time.NewTicker(5 * time.Second)
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

// HybridBucketPool manages per-session HybridBucket instances via lazymap.
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
	rate, ok := mc["rate"].(string)
	if !ok {
		return nil, nil
	}
	key := sessionID + rate
	r, err := bytefmt.ToBytes(rate)
	if err != nil {
		return nil, errors.Errorf("failed to parse rate %v", rate)
	}
	return s.LazyMap.Get(key, func() (Throttler, error) {
		bytesPerSec := float64(r) / 8
		// capacity == rate: at most one second of idle accrual, no extra
		// burst beyond what the configured rate allows.
		return NewHybridBucket(bytesPerSec, bytesPerSec, s.rc, sessionID), nil
	})
}
