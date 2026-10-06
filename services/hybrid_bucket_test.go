package services

import (
	"context"
	"os"
	"strconv"
	"strings"
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

			if d := mustReserve(t, first, first.capacity); d != 0 {
				t.Fatalf("%s: fresh bucket made its capacity %.0f wait %v, want none", order[0], first.capacity, d)
			}
			if d := mustReserve(t, second, second.capacity); d != 0 {
				t.Errorf("%s after %s was drained: its own capacity %.0f waits %v, want none (keys %v)",
					order[1], order[0], second.capacity, d, mr.Keys())
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

			if d := mustReserve(t, a, a.capacity); d != 0 {
				t.Fatalf("pod A: fresh bucket made its capacity wait %v, want none", d)
			}
			// Pod A just took the whole second; pod B's second waits for
			// it to accrue again, less the ms since.
			if d := mustReserve(t, b, b.capacity); d < 500*time.Millisecond {
				t.Errorf("pod B waits %v for %.0f right after pod A drained the session's bucket; want ~1s, the shared balance (keys %v)",
					d, b.capacity, mr.Keys())
			}
		})
	}
}

// A token minted before the viewer had a session (web-ui's first page
// without its cookie) carries sessionID "". Keyed on it, every such viewer
// on every pod drew on one bucket; the address the token names keys it.
func TestHybridBucketEmptySessionKeyedByAddress(t *testing.T) {
	mr := miniredis.RunT(t)
	bucket := func(p *HybridBucketPool, addr string) *HybridBucket {
		t.Helper()
		th, err := p.Get(jwt.MapClaims{"sessionID": "", "remoteAddress": addr, "rate": "5M"})
		hb, ok := th.(*HybridBucket)
		if err != nil || !ok {
			t.Fatalf("Get(%q) = %T, %v; want *HybridBucket", addr, th, err)
		}
		return hb
	}
	a := bucket(newPodPool(t, mr), "203.0.113.7")
	b := bucket(newPodPool(t, mr), "2001:db8::1")
	if d := mustReserve(t, a, a.capacity); d != 0 {
		t.Fatalf("viewer A: fresh bucket made its capacity wait %v, want none", d)
	}
	if d := mustReserve(t, b, b.capacity); d != 0 {
		t.Errorf("viewer B at another address waits %v for %.0f after A drained its bucket (keys %v)",
			d, b.capacity, mr.Keys())
	}
}

// The key is what an operator looks up in Redis; it expires 5 min after the
// session's last write at that rate.
func TestHybridBucketRedisKey(t *testing.T) {
	mr := miniredis.RunT(t)
	hb := poolBucket(t, newPodPool(t, mr), "s1", "5M")
	mustReserve(t, hb, 1)

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

// mustReserve reserves n bytes of hb's Redis balance and returns the wait.
func mustReserve(t *testing.T, hb *HybridBucket, n float64) time.Duration {
	t.Helper()
	d, err := hb.reserve(n, false)
	if err != nil {
		t.Fatalf("reserve(%.0f): %v", n, err)
	}
	return d
}

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
	// Verify rate limiting is active: throughput should be bounded, not
	// unbounded. TestLimiterLocalModeSharedBySession holds it to the rate.
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

	// Verifies multi-waiter coordination: N concurrent goroutines on one
	// bucket are bound by the Redis-accrual rate, not by N × rate. Allow 3×
	// for the initial burst (capacity). TestLimiterAggregateRate holds it
	// to the rate.
	throughput := float64(total.Load()) / duration.Seconds()
	if throughput > rate*3 {
		t.Errorf("throughput %.0f exceeds expected ceiling %.0f (N goroutines must not multiply effective rate)", throughput, rate*3)
	}
	t.Logf("concurrent throughput: %.0f B/s with %d goroutines (rate=%.0f)",
		throughput, goroutines, rate)
}

// ---------- reservations: rate, calls, local mode, transitions ----------

// Free's rate: "5M" bits/s. The lease is a quarter second of it.
const (
	freeRate   = 655360.0
	writeChunk = 32 << 10 // httputil.ReverseProxy's copy buffer
)

// limiterBackend is a Redis the reservation tests run against: miniredis
// always, plus the Dragonfly at $THP_TEST_DRAGONFLY when set, run like
// prod's: docker run -p 127.0.0.1:16399:6379
// docker.dragonflydb.io/dragonflydb/dragonfly:v1.26.0 --cache_mode=true
type limiterBackend struct{ name, addr string }

func limiterBackends(t *testing.T) []limiterBackend {
	t.Helper()
	bs := []limiterBackend{{"miniredis", miniredis.RunT(t).Addr()}}
	if a := os.Getenv("THP_TEST_DRAGONFLY"); a != "" {
		bs = append(bs, limiterBackend{"dragonfly", a})
	}
	return bs
}

// uniqueSession keeps runs against a long-lived Dragonfly off each other's
// keys.
func uniqueSession(t *testing.T) string {
	return strings.NewReplacer("/", "-", " ", "-").Replace(t.Name()) + "-" + strconv.FormatInt(time.Now().UnixNano(), 36)
}

// pods returns n buckets of one session at rate, each on its own client:
// n thp pods serving the session.
func pods(t *testing.T, addr, session string, rate float64, n int, cc *callCounter) []*HybridBucket {
	t.Helper()
	var hbs []*HybridBucket
	for i := 0; i < n; i++ {
		rc := redis.NewClient(&redis.Options{Addr: addr})
		if cc != nil {
			rc.AddHook(cc)
		}
		t.Cleanup(func() { _ = rc.Close() })
		hbs = append(hbs, NewHybridBucket(rate, rate, rc, session))
	}
	return hbs
}

// drain empties the session's burst so a run measures the rate alone.
func drain(t *testing.T, hb *HybridBucket) {
	t.Helper()
	if d := mustReserve(t, hb, hb.capacity); d > 50*time.Millisecond {
		t.Fatalf("draining a fresh key waited %v", d)
	}
}

// stream writes chunks through hb until deadline and returns the bytes
// whose Wait returned before it.
func stream(hb *HybridBucket, deadline time.Time) int64 {
	var n int64
	for time.Now().Before(deadline) {
		hb.Wait(writeChunk)
		if time.Now().After(deadline) {
			break
		}
		n += writeChunk
	}
	return n
}

// N streams of one session over 3 pods, after its burst is spent, get the
// rate between them: not N times it (each sleeping its own reservation),
// and not much under it. A reservation is waited for before it is spent,
// so the bytes out never run ahead of the accrual; the one being accrued
// at the deadline (a lease, 0.25 s) is what they may run behind.
func TestLimiterAggregateRate(t *testing.T) {
	const d = 3 * time.Second
	for _, b := range limiterBackends(t) {
		for _, n := range []int{1, 4, 16} {
			t.Run(b.name+"/"+strconv.Itoa(n), func(t *testing.T) {
				t.Parallel()
				hbs := pods(t, b.addr, uniqueSession(t), freeRate, 3, nil)
				drain(t, hbs[0])
				var total atomic.Int64
				var wg sync.WaitGroup
				deadline := time.Now().Add(d)
				for i := 0; i < n; i++ {
					wg.Add(1)
					go func(hb *HybridBucket) {
						defer wg.Done()
						total.Add(stream(hb, deadline))
					}(hbs[i%len(hbs)])
				}
				wg.Wait()
				got := float64(total.Load()) / d.Seconds() / freeRate
				t.Logf("%d streams: %.3f of the rate", n, got)
				if got > 1.03 || got < 0.88 {
					t.Errorf("%d streams of one session got %.3f of its rate together, want within [0.88, 1.03]", n, got)
				}
			})
		}
	}
}

// A single stream reaches the rate: each lease is spent whole before the
// next reservation, and time spent writing (or oversleeping) accrues in the
// balance, up to capacity, so the next reservation waits that much less.
// Measured between writes that waited, which fall where a reservation was
// paid off, so the lease granularity drops out.
func TestLimiterSingleStreamReachesRate(t *testing.T) {
	for _, b := range limiterBackends(t) {
		t.Run(b.name, func(t *testing.T) {
			t.Parallel()
			hb := pods(t, b.addr, uniqueSession(t), freeRate, 1, nil)[0]
			drain(t, hb)
			type mark struct {
				at    time.Time
				bytes int64
			}
			var marks []mark
			var sent int64
			deadline := time.Now().Add(3 * time.Second)
			for time.Now().Before(deadline) {
				s := time.Now()
				hb.Wait(writeChunk)
				if time.Since(s) > time.Millisecond {
					marks = append(marks, mark{time.Now(), sent})
				}
				sent += writeChunk
				time.Sleep(5 * time.Millisecond) // downstream: 6.5 MB/s, 10x the rate
			}
			if len(marks) < 5 {
				t.Fatalf("%d waits in 3 s, want a reservation every 0.25 s", len(marks))
			}
			first, last := marks[0], marks[len(marks)-1]
			got := float64(last.bytes-first.bytes) / last.at.Sub(first.at).Seconds() / freeRate
			t.Logf("single stream: %.3f of the rate over %d reservations", got, len(marks)-1)
			if got < 0.95 || got > 1.02 {
				t.Errorf("single stream at %.3f of its rate, want within [0.95, 1.02]", got)
			}
		})
	}
}

// One script call per lease, a quarter second of the rate: ~4 a second
// per session at 5M whatever its write size, where polling made ~39 a
// second per stream. The counter on the clients and the limiter's metric
// agree.
func TestLimiterRedisCallsPerStream(t *testing.T) {
	const d = 2 * time.Second
	for _, b := range limiterBackends(t) {
		for _, n := range []int{1, 4} {
			t.Run(b.name+"/"+strconv.Itoa(n), func(t *testing.T) {
				cc := &callCounter{}
				hb := pods(t, b.addr, uniqueSession(t), freeRate, 1, cc)[0]
				drain(t, hb)
				before := counterValue(t, promLimiterRedisCalls.WithLabelValues("ok"))
				cc.calls.Store(0)
				var wg sync.WaitGroup
				deadline := time.Now().Add(d)
				for i := 0; i < n; i++ {
					wg.Add(1)
					go func() {
						defer wg.Done()
						stream(hb, deadline)
					}()
				}
				wg.Wait()
				calls := cc.calls.Load()
				perSec := float64(calls) / d.Seconds()
				t.Logf("%d streams: %d calls, %.1f/s", n, calls, perSec)
				if perSec > 4.5+float64(n)/d.Seconds() {
					t.Errorf("%d streams at 5M made %.1f script calls a second, want ~4 (one per lease)", n, perSec)
				}
				if m := counterValue(t, promLimiterRedisCalls.WithLabelValues("ok")) - before; m != float64(calls) {
					t.Errorf("metric counted %.0f calls, the client %d", m, calls)
				}
			})
		}
	}
}

// Without Redis the session's streams on a pod share one local balance:
// 4 streams get the rate between them, not 4 times it. With Redis down
// from the start, the streams whose calls fail together fall back once:
// a later one does not empty the balance the earlier ones are in debt to.
func TestLimiterLocalModeSharedBySession(t *testing.T) {
	const d = 2 * time.Second
	dead := miniredis.RunT(t)
	deadAddr := dead.Addr()
	dead.Close()
	for _, c := range []struct {
		name      string
		rc        redis.UniversalClient
		fallbacks float64
	}{
		{"no redis", nil, 0},
		{"redis down", redis.NewClient(&redis.Options{Addr: deadAddr}), 1},
	} {
		t.Run(c.name, func(t *testing.T) {
			hb := NewHybridBucket(freeRate, freeRate, c.rc, "local-shared-"+c.name)
			fallbacks := counterValue(t, promLimiterLocalFallbacks)
			var total atomic.Int64
			var wg sync.WaitGroup
			deadline := time.Now().Add(d)
			for i := 0; i < 4; i++ {
				wg.Add(1)
				go func() {
					defer wg.Done()
					total.Add(stream(hb, deadline))
				}()
			}
			wg.Wait()
			got := float64(total.Load()) / d.Seconds() / freeRate
			t.Logf("4 local streams: %.3f of the rate", got)
			if got > 1.05 || got < 0.85 {
				t.Errorf("4 streams on the local balance got %.3f of the rate, want within [0.85, 1.05]", got)
			}
			if n := counterValue(t, promLimiterLocalFallbacks) - fallbacks; n != c.fallbacks {
				t.Errorf("fallback metric grew by %.0f, want %.0f", n, c.fallbacks)
			}
		})
	}
}

// Redis goes away mid-stream and comes back: no burst at either step. The
// local balance starts empty when the pod falls back (not a capacity's
// worth accrued since the bucket was made), and the first reservation back
// drops what the Redis balance accrued meanwhile.
func TestLimiterFallbackAndBackWithoutBurst(t *testing.T) {
	defer func(p time.Duration) { probeInterval = p }(probeInterval)
	probeInterval = 100 * time.Millisecond
	mr := miniredis.RunT(t)
	cc := &callCounter{}
	hb := pods(t, mr.Addr(), "fallback-burst", freeRate, 1, cc)[0]
	drain(t, hb)
	fallbacks := counterValue(t, promLimiterLocalFallbacks)

	const d = 3500 * time.Millisecond
	start := time.Now()
	var total int64
	done := make(chan struct{})
	go func() {
		total = stream(hb, start.Add(d))
		close(done)
	}()
	time.Sleep(time.Second)
	mr.Close()
	time.Sleep(time.Second)
	if err := mr.Restart(); err != nil {
		t.Fatal(err)
	}
	<-done
	hb.mu.Lock()
	back := hb.redisOK
	resync := hb.resync
	hb.mu.Unlock()
	if !back || cc.failed.Load() == 0 {
		t.Fatalf("redisOK %v after restart, %d failed calls: the case did not go local and back", back, cc.failed.Load())
	}
	if resync {
		t.Error("resync still set after reservations back on Redis: every later one would drop the session's burst")
	}
	if n := counterValue(t, promLimiterLocalFallbacks) - fallbacks; n != 1 {
		t.Errorf("fallback metric grew by %.0f, want 1", n)
	}
	got := float64(total) / d.Seconds() / freeRate
	t.Logf("Redis, local, Redis: %.3f of the rate", got)
	if got > 1.05 || got < 0.85 {
		t.Errorf("a stream through Redis, local and back got %.3f of its rate, want within [0.85, 1.05] (a capacity's burst is +0.29)", got)
	}
}

// oldReserveScript and oldWait are the limiter before reservations
// (d4e8fa3), still on the pods a rollout has not reached: grant what is
// there, poll every <=100 ms for the rest, prefetch up to a second.
var oldReserveScript = redis.NewScript(`
local key   = KEYS[1]
local cap   = tonumber(ARGV[1])
local rate  = tonumber(ARGV[2])
local req   = tonumber(ARGV[3])
local now   = tonumber(ARGV[4])
local ttl   = tonumber(ARGV[5])
local vals = redis.call("HMGET", key, "tokens", "last_tick")
local tokens   = tonumber(vals[1]) or cap
local lastTick = tonumber(vals[2]) or now
local elapsed = (now - lastTick) / 1000
if elapsed > 0 then
    tokens = tokens + elapsed * rate
    if tokens > cap then tokens = cap end
end
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

type oldBucket struct {
	rc    redis.UniversalClient
	key   string
	rate  float64
	local float64
}

func (o *oldBucket) Wait(count int64) {
	need := float64(count)
	if o.local >= need {
		o.local -= need
		return
	}
	need -= o.local
	o.local = 0
	for need > 0 {
		res, err := oldReserveScript.Run(context.Background(), o.rc, []string{o.key},
			int64(o.rate), int64(o.rate), int64(max(need, o.rate)), time.Now().UnixMilli(), 300).Text()
		if err != nil {
			panic(err)
		}
		granted, _ := strconv.ParseFloat(res, 64)
		if granted >= need {
			o.local += granted - need
			return
		}
		need -= granted
		time.Sleep(min(max(time.Duration(need/o.rate*float64(time.Second)), time.Millisecond), 100*time.Millisecond))
	}
}

// During a rollout an old pod and a new one serve one session on one key:
// together they stay within the rate. The old script grants nothing while
// the balance is in debt, so the old pod gets less than its share while a
// new pod streams the session; it never gets more.
func TestLimiterMixedOldAndNewPods(t *testing.T) {
	const d = 3 * time.Second
	for _, b := range limiterBackends(t) {
		t.Run(b.name, func(t *testing.T) {
			t.Parallel()
			session := uniqueSession(t)
			hb := pods(t, b.addr, session, freeRate, 1, nil)[0]
			rc := redis.NewClient(&redis.Options{Addr: b.addr})
			t.Cleanup(func() { _ = rc.Close() })
			old := &oldBucket{rc: rc, key: hb.redisKey, rate: freeRate}
			drain(t, hb)
			deadline := time.Now().Add(d)
			var oldN, newN int64
			var wg sync.WaitGroup
			wg.Add(2)
			go func() {
				defer wg.Done()
				for time.Now().Before(deadline) {
					old.Wait(writeChunk)
					if time.Now().Before(deadline) {
						oldN += writeChunk
					}
				}
			}()
			go func() {
				defer wg.Done()
				newN = stream(hb, deadline)
			}()
			wg.Wait()
			got := float64(oldN+newN) / d.Seconds() / freeRate
			t.Logf("old pod %.3f, new pod %.3f, together %.3f of the rate",
				float64(oldN)/d.Seconds()/freeRate, float64(newN)/d.Seconds()/freeRate, got)
			if got > 1.05 {
				t.Errorf("an old and a new pod on one session got %.3f of its rate together, want <= 1.05", got)
			}
			if got < 0.85 {
				t.Errorf("an old and a new pod on one session got %.3f of its rate together, want >= 0.85", got)
			}
		})
	}
}

// Reservations whose streams then stop (the client left while they slept)
// are a debt the accrual repays at rate, not a lasting one: a new stream
// waits for it once, and an idle second later the session has its full
// burst again. The Redis key expires 5 min after its last call.
func TestLimiterAbandonedReservationsDecay(t *testing.T) {
	for _, b := range limiterBackends(t) {
		t.Run(b.name, func(t *testing.T) {
			t.Parallel()
			session := uniqueSession(t)
			// One pod per stream: on a shared pod a stream could spend
			// another's leftover lease instead of reserving its own.
			hbs := pods(t, b.addr, session, freeRate, 17, nil)
			// 16 streams reserve a lease each on a fresh key, then stop:
			// 16 leases (4 s) against a 1 s burst, 3 s of debt; 12 of them
			// asleep for it.
			var wg sync.WaitGroup
			for i := 0; i < 16; i++ {
				wg.Add(1)
				go func(hb *HybridBucket) {
					defer wg.Done()
					hb.Wait(writeChunk)
				}(hbs[i])
			}
			time.Sleep(200 * time.Millisecond) // all 16 have reserved
			if w := gaugeValue(t, promLimiterWaiting); w < 12 {
				t.Errorf("waiting gauge %.0f with 12 streams asleep behind the burst, want >= 12", w)
			}
			s := time.Now()
			hbs[16].Wait(writeChunk)
			waited := time.Since(s)
			if waited < 2500*time.Millisecond || waited > 3500*time.Millisecond {
				t.Errorf("a new stream behind 3 s of abandoned reservations waited %v, want ~3 s", waited)
			}
			wg.Wait()
			// The new stream's own lease is the last debt; repaid, and a
			// second of accrual, the burst is whole again.
			time.Sleep(time.Duration((leaseSeconds + 1.1) * float64(time.Second)))
			if d := mustReserve(t, hbs[16], hbs[16].capacity); d > 10*time.Millisecond {
				t.Errorf("after the debt was repaid and 1 s idle, the session's burst waits %v, want none", d)
			}
		})
	}
}

// Two pods whose clocks are 50 ms apart call alternately every 10 ms for
// 1 s, starting 10 s in debt. last_tick only moves forward, so the session
// accrues 1 s (plus the skew once), not the skew on every alternation.
func TestLimiterScriptClockSkew(t *testing.T) {
	for _, b := range limiterBackends(t) {
		t.Run(b.name, func(t *testing.T) {
			rc := redis.NewClient(&redis.Options{Addr: b.addr})
			t.Cleanup(func() { _ = rc.Close() })
			key := "bw:limit:" + uniqueSession(t)
			call := func(nowMs int64, n float64) {
				t.Helper()
				if err := luaReserve.Run(context.Background(), rc, []string{key},
					int64(freeRate), int64(freeRate), int64(n), nowMs, 300, "0").Err(); err != nil {
					t.Fatal(err)
				}
			}
			t0 := time.Now().UnixMilli()
			call(t0, 11*freeRate)
			for i := int64(1); i <= 100; i++ {
				call(t0+10*i, 0)    // pod B
				call(t0+50+10*i, 0) // pod A, 50 ms ahead
			}
			v, err := rc.HGet(context.Background(), key, "tokens").Float64()
			if err != nil {
				t.Fatal(err)
			}
			if got := v / freeRate; got > -8.9 {
				t.Errorf("balance %.2f s of the rate after 1 s from -10 s, want ~-8.95 (1 s and the skew once)", got)
			}
		})
	}
}
