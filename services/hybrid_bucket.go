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
	mu       sync.Mutex
	local    float64 // current local token balance (bytes)
	rate     float64 // bytes per second
	capacity float64 // max burst (bytes)

	rc       redis.UniversalClient
	redisKey string
	redisOK  bool
	probing  bool
}

func NewHybridBucket(rate float64, capacity float64, rc redis.UniversalClient, sessionID string) *HybridBucket {
	return &HybridBucket{
		local:    capacity, // start full
		rate:     rate,
		capacity: capacity,
		rc:       rc,
		redisKey: "bw:limit:" + sessionID,
		redisOK:  rc != nil,
	}
}

// Wait blocks until count tokens are available, satisfying the Throttler interface.
func (hb *HybridBucket) Wait(count int64) {
	if count <= 0 {
		return
	}
	need := float64(count)

	for need > 0 {
		hb.mu.Lock()
		if hb.local >= need {
			hb.local -= need
			hb.mu.Unlock()
			return
		}
		// consume whatever is left locally
		need -= hb.local
		hb.local = 0

		// how many tokens to request from Redis: ~1 second worth, but at least what we need
		batch := hb.rate
		if batch < need {
			batch = need
		}

		canRedis := hb.redisOK && hb.rc != nil
		hb.mu.Unlock()

		if canRedis {
			granted := hb.refillFromRedis(batch)
			if granted > 0 {
				hb.mu.Lock()
				hb.local += granted
				hb.mu.Unlock()
				continue // re-check loop
			}
		}

		// If Redis is unavailable or granted 0: sleep proportionally, then
		// grant tokens locally at the configured rate (graceful degradation).
		sleepDur := time.Duration(need / hb.rate * float64(time.Second))
		if sleepDur < time.Millisecond {
			sleepDur = time.Millisecond
		}
		time.Sleep(sleepDur)
		// After sleeping, we consider `need` tokens consumed by elapsed time.
		return
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
	lazymap.LazyMap[Throttler]
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
		capacity := float64(r) // ~8 seconds burst at bytesPerSec
		return NewHybridBucket(bytesPerSec, capacity, s.rc, sessionID), nil
	})
}
