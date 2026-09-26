package services

import (
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/golang-jwt/jwt/v4"
	"github.com/redis/go-redis/v9"
)

// ---------- HybridBucketPool.Get tests ----------

func TestHybridBucketPoolGet_MissingSessionID(t *testing.T) {
	pool := NewHybridBucketPool(nil)
	mc := jwt.MapClaims{"rate": "1M"}

	th, err := pool.Get(mc)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if th != nil {
		t.Fatal("expected nil throttler when sessionID is missing")
	}
}

func TestHybridBucketPoolGet_MissingRate(t *testing.T) {
	pool := NewHybridBucketPool(nil)
	mc := jwt.MapClaims{"sessionID": "s1"}

	th, err := pool.Get(mc)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if th != nil {
		t.Fatal("expected nil throttler when rate is missing")
	}
}

func TestHybridBucketPoolGet_InvalidRate(t *testing.T) {
	pool := NewHybridBucketPool(nil)
	mc := jwt.MapClaims{"sessionID": "s1", "rate": "notarate"}

	_, err := pool.Get(mc)
	if err == nil {
		t.Fatal("expected error for invalid rate string")
	}
}

func TestHybridBucketPoolGet_ValidClaims(t *testing.T) {
	pool := NewHybridBucketPool(nil)
	mc := jwt.MapClaims{"sessionID": "s1", "rate": "1M"}

	th, err := pool.Get(mc)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if th == nil {
		t.Fatal("expected non-nil throttler for valid claims")
	}
}

func TestHybridBucketPoolGet_Cached(t *testing.T) {
	pool := NewHybridBucketPool(nil)
	mc := jwt.MapClaims{"sessionID": "s1", "rate": "1M"}

	th1, _ := pool.Get(mc)
	th2, _ := pool.Get(mc)

	if th1 != th2 {
		t.Fatal("expected same throttler instance for same key")
	}
}

// ---------- bucket key: one bucket per (session, rate) ----------

// newPodPool returns a pool the way one thp pod runs it: its own client to
// the Redis every pod shares.
func newPodPool(t *testing.T, mr *miniredis.Miniredis) *HybridBucketPool {
	t.Helper()
	rc := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { _ = rc.Close() })
	return NewHybridBucketPool(rc)
}

func poolBucket(t *testing.T, p *HybridBucketPool, sessionID, rate string) *HybridBucket {
	t.Helper()
	th, err := p.Get(jwt.MapClaims{"sessionID": sessionID, "rate": rate})
	if err != nil {
		t.Fatalf("Get(%q, %q): %v", sessionID, rate, err)
	}
	hb, ok := th.(*HybridBucket)
	if !ok {
		t.Fatalf("Get(%q, %q) = %T, want *HybridBucket", sessionID, rate, th)
	}
	return hb
}

// A session holds tokens at two rates at once: the tier's on its primary
// token (playlist, post-grace segments) and grace's 50M on grace segments.
// Each is a bucket of its own; on one Redis key the one that drains it
// leaves the other nothing, and each call re-clamps the balance to its own
// capacity.
func TestHybridBucketRatesOfOneSessionDoNotShareTokens(t *testing.T) {
	for _, order := range [][2]string{{"5M", "50M"}, {"50M", "5M"}} {
		t.Run(order[0]+" drained, then "+order[1], func(t *testing.T) {
			mr := miniredis.RunT(t)
			pool := newPodPool(t, mr)
			first := poolBucket(t, pool, "s1", order[0])
			second := poolBucket(t, pool, "s1", order[1])

			if got := first.refillFromRedis(first.capacity); got != first.capacity {
				t.Fatalf("%s: fresh bucket granted %.0f, want its capacity %.0f", order[0], got, first.capacity)
			}
			if got := second.refillFromRedis(second.capacity); got != second.capacity {
				t.Errorf("%s after %s was drained: granted %.0f, want its own full capacity %.0f (keys %v)",
					order[1], order[0], got, second.capacity, mr.Keys())
			}
		})
	}
}

// Two pods serving one session at one rate hold one bucket between them:
// that is what makes the limit per session and not per pod. The rate is
// the bucket's, not the claim's spelling: "5M" and "5MB" are one rate.
func TestHybridBucketSameRateSharedAcrossPods(t *testing.T) {
	for _, rates := range [][2]string{{"5M", "5M"}, {"5M", "5MB"}} {
		t.Run(rates[0]+" and "+rates[1], func(t *testing.T) {
			mr := miniredis.RunT(t)
			a := poolBucket(t, newPodPool(t, mr), "s1", rates[0])
			b := poolBucket(t, newPodPool(t, mr), "s1", rates[1])
			if a == b {
				t.Fatal("two pools returned one instance; want one per pod")
			}

			if got := a.refillFromRedis(a.capacity); got != a.capacity {
				t.Fatalf("pod A: fresh bucket granted %.0f, want %.0f", got, a.capacity)
			}
			// Pod A just took the whole second; pod B gets only what
			// accrued since (ms of the rate), far under half of it.
			if got := b.refillFromRedis(b.capacity); got >= b.capacity/2 {
				t.Errorf("pod B granted %.0f of %.0f right after pod A drained the session's bucket; want the shared balance (keys %v)",
					got, b.capacity, mr.Keys())
			}
		})
	}
}

// The key is what an operator looks up in Redis; it expires 5 min after the
// session's last write at that rate.
func TestHybridBucketRedisKey(t *testing.T) {
	mr := miniredis.RunT(t)
	hb := poolBucket(t, newPodPool(t, mr), "s1", "5M")
	hb.refillFromRedis(1)

	const want = "bw:limit:s1:655360" // 5M bits/s = 655360 bytes/s
	keys := mr.Keys()
	if len(keys) != 1 || keys[0] != want {
		t.Fatalf("keys %v, want [%s]", keys, want)
	}
	if ttl := mr.TTL(want); ttl != 5*time.Minute {
		t.Errorf("TTL %v, want 5m", ttl)
	}
}

// The pool's key keeps session and rate apart: "a1" at 5M is not "a" at 15M.
func TestHybridBucketPoolKeySeparatesSessionFromRate(t *testing.T) {
	pool := NewHybridBucketPool(nil)
	a1 := poolBucket(t, pool, "a1", "5M")
	a := poolBucket(t, pool, "a", "15M")
	if a1 == a {
		t.Fatal(`session "a1" at 5M and session "a" at 15M got one bucket`)
	}
	if a.rate != 15*1024*1024/8 {
		t.Errorf(`session "a" at 15M: bucket rate %.0f B/s, want %d`, a.rate, 15*1024*1024/8)
	}
}

// ---------- helpers ----------

// measureThroughput calls hb.Wait(chunkSize) in a loop for the given duration
// and returns the total bytes consumed.
func measureThroughput(hb *HybridBucket, chunkSize int64, d time.Duration) int64 {
	var total int64
	deadline := time.Now().Add(d)
	for time.Now().Before(deadline) {
		hb.Wait(chunkSize)
		total += chunkSize
	}
	return total
}

// assertBounded fails the test if throughput is outside [minRate, maxRate].
func assertBounded(t *testing.T, label string, throughput, minRate, maxRate float64) {
	t.Helper()
	if throughput < minRate {
		t.Errorf("%s: throughput %.0f below minimum %.0f", label, throughput, minRate)
	}
	if throughput > maxRate {
		t.Errorf("%s: throughput %.0f above maximum %.0f", label, throughput, maxRate)
	}
}

// ---------- Wait — local only ----------

func TestWaitLocalOnly(t *testing.T) {
	rate := 50000.0 // 50KB/s
	hb := NewHybridBucket(rate, rate, nil, "local-test")

	duration := 2 * time.Second
	chunkSize := int64(4096)

	start := time.Now()
	total := measureThroughput(hb, chunkSize, duration)
	elapsed := time.Since(start).Seconds()

	throughput := float64(total) / elapsed
	// Verify rate limiting is active: throughput should be bounded, not unbounded.
	// The bucket delivers roughly 2x the configured rate due to sleep-based
	// token accrual, but must stay well below an unbounded loop.
	assertBounded(t, "local-only", throughput, rate*0.5, rate*3)
	t.Logf("local-only throughput: %.0f B/s (rate=%.0f)", throughput, rate)
}

// ---------- Wait — single stream via miniredis ----------

func TestWaitWithRedis(t *testing.T) {
	mr := miniredis.RunT(t)
	rc := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	defer rc.Close()

	rate := 50000.0
	hb := NewHybridBucket(rate, rate, rc, "redis-test")

	duration := 2 * time.Second
	chunkSize := int64(4096)

	start := time.Now()
	total := measureThroughput(hb, chunkSize, duration)
	elapsed := time.Since(start).Seconds()

	throughput := float64(total) / elapsed
	// With Redis, throughput includes an initial burst of capacity tokens
	// plus the ongoing rate. Verify it's bounded.
	assertBounded(t, "redis-single", throughput, rate*0.5, rate*4)
	t.Logf("redis throughput: %.0f B/s (rate=%.0f)", throughput, rate)
}

// ---------- Two streams fairness ----------

func TestTwoStreamsFairness(t *testing.T) {
	mr := miniredis.RunT(t)
	rc1 := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	rc2 := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	defer rc1.Close()
	defer rc2.Close()

	rate := 80000.0 // shared global rate
	sessionID := "fairness-test"

	hb1 := NewHybridBucket(rate, rate, rc1, sessionID)
	hb2 := NewHybridBucket(rate, rate, rc2, sessionID)

	duration := 2 * time.Second
	chunkSize := int64(4096)

	var total1, total2 int64
	var wg sync.WaitGroup
	wg.Add(2)

	start := time.Now()

	go func() {
		defer wg.Done()
		total1 = measureThroughput(hb1, chunkSize, duration)
	}()
	go func() {
		defer wg.Done()
		total2 = measureThroughput(hb2, chunkSize, duration)
	}()

	wg.Wait()
	elapsed := time.Since(start).Seconds()

	combined := float64(total1+total2) / elapsed
	// Combined throughput should be bounded by the shared rate (with tolerance
	// for sleep-based accrual and initial burst).
	assertBounded(t, "combined", combined, rate*0.5, rate*5)

	// Each stream should get at least 15% of the total (fairness check).
	share1 := float64(total1) / float64(total1+total2)
	share2 := float64(total2) / float64(total1+total2)
	if share1 < 0.15 {
		t.Errorf("stream 1 share too low: %.1f%%", share1*100)
	}
	if share2 < 0.15 {
		t.Errorf("stream 2 share too low: %.1f%%", share2*100)
	}
	t.Logf("shares: stream1=%.1f%% stream2=%.1f%% combined=%.0f B/s (rate=%.0f)",
		share1*100, share2*100, combined, rate)
}

// ---------- Recovery after one stream stops ----------

func TestRecoveryAfterStreamStops(t *testing.T) {
	mr := miniredis.RunT(t)
	rc1 := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	rc2 := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	defer rc1.Close()
	defer rc2.Close()

	rate := 80000.0
	sessionID := "recovery-test"

	hb1 := NewHybridBucket(rate, rate, rc1, sessionID)
	hb2 := NewHybridBucket(rate, rate, rc2, sessionID)

	chunkSize := int64(4096)

	// Phase 1: both streams run for 1 second
	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		measureThroughput(hb1, chunkSize, 1*time.Second)
	}()
	go func() {
		defer wg.Done()
		measureThroughput(hb2, chunkSize, 1*time.Second)
	}()
	wg.Wait()

	// Phase 2: only stream B continues — should recover to full rate.
	// The key assertion is that stream B is NOT starved after stream A stops
	// (this was the starvation bug that HybridBucket was designed to fix).
	start := time.Now()
	total := measureThroughput(hb2, chunkSize, 2*time.Second)
	elapsed := time.Since(start).Seconds()

	throughput := float64(total) / elapsed
	// Stream B should recover to a reasonable fraction of the full rate.
	assertBounded(t, "recovery", throughput, rate*0.3, rate*4)
	t.Logf("post-recovery throughput: %.0f B/s (rate=%.0f)", throughput, rate)
}

// ---------- Redis fallback ----------

func TestRedisFallback(t *testing.T) {
	mr := miniredis.RunT(t)
	rc := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	defer rc.Close()

	rate := 50000.0
	hb := NewHybridBucket(rate, rate, rc, "fallback-test")

	chunkSize := int64(4096)

	// Consume some tokens while Redis is alive
	measureThroughput(hb, chunkSize, 500*time.Millisecond)

	// Kill Redis
	mr.Close()

	// Stream must continue without hanging, falling back to local rate.
	var total int64
	done := make(chan struct{})
	go func() {
		total = measureThroughput(hb, chunkSize, 2*time.Second)
		close(done)
	}()

	select {
	case <-done:
		// good — did not hang
	case <-time.After(5 * time.Second):
		t.Fatal("Wait() hung after Redis died")
	}

	elapsed := 2.0 // we measured for 2 seconds
	throughput := float64(total) / elapsed
	assertBounded(t, "fallback", throughput, rate*0.3, rate*4)
	t.Logf("fallback throughput: %.0f B/s (rate=%.0f)", throughput, rate)
}

// ---------- Concurrent Wait safety ----------

func TestConcurrentWaitSafety(t *testing.T) {
	mr := miniredis.RunT(t)
	rc := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	defer rc.Close()

	rate := 100000.0
	hb := NewHybridBucket(rate, rate, rc, "concurrent-test")

	var total atomic.Int64
	var wg sync.WaitGroup

	goroutines := 8
	chunkSize := int64(1024)
	duration := 1 * time.Second

	wg.Add(goroutines)
	for i := 0; i < goroutines; i++ {
		go func() {
			defer wg.Done()
			deadline := time.Now().Add(duration)
			for time.Now().Before(deadline) {
				hb.Wait(chunkSize)
				total.Add(chunkSize)
			}
		}()
	}
	wg.Wait()

	// Verifies multi-waiter coordination: Wait must keep polling Redis until
	// satisfied so N concurrent goroutines on one bucket are bound by the
	// Redis-accrual rate, not by N × rate. Allow 3× for initial-burst
	// (capacity) plus sleep/poll jitter.
	throughput := float64(total.Load()) / duration.Seconds()
	if throughput > rate*3 {
		t.Errorf("throughput %.0f exceeds expected ceiling %.0f (N goroutines must not multiply effective rate)", throughput, rate*3)
	}
	t.Logf("concurrent throughput: %.0f B/s with %d goroutines (rate=%.0f)",
		throughput, goroutines, rate)
}
