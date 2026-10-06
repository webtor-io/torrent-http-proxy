package services

import (
	"context"
	"os"
	"strconv"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"
)

// callCounter counts the limiter's script calls (EVALSHA, and EVAL when
// the script is not loaded yet) and the failed ones on one client.
type callCounter struct{ calls, failed atomic.Int64 }

func (c *callCounter) DialHook(next redis.DialHook) redis.DialHook { return next }
func (c *callCounter) ProcessPipelineHook(next redis.ProcessPipelineHook) redis.ProcessPipelineHook {
	return next
}
func (c *callCounter) ProcessHook(next redis.ProcessHook) redis.ProcessHook {
	return func(ctx context.Context, cmd redis.Cmder) error {
		err := next(ctx, cmd)
		if n := cmd.Name(); n == "evalsha" || n == "eval" {
			if err == nil || !redis.HasErrorPrefix(err, "NOSCRIPT") {
				c.calls.Add(1)
			}
			if err != nil && !redis.HasErrorPrefix(err, "NOSCRIPT") {
				c.failed.Add(1)
			}
		}
		return err
	}
}

func cpuSeconds(b *testing.B) float64 {
	var ru syscall.Rusage
	if err := syscall.Getrusage(syscall.RUSAGE_SELF, &ru); err != nil {
		b.Fatal(err)
	}
	tv := func(t syscall.Timeval) float64 { return float64(t.Sec) + float64(t.Usec)/1e6 }
	return tv(ru.Utime) + tv(ru.Stime)
}

// BenchmarkLimiterFreeStreams runs 1000 free sessions (5M), one stream
// each, writing 32 KiB chunks for 5 s after their burst, the way thp's
// copy loop does, and reports per wall second: this process's CPU, script
// calls and failed ones (miniredis's CPU is in this process; a Dragonfly's
// is its container's cgroup cpu.stat). Redis is $THP_BENCH_REDIS (a
// Dragonfly as in prod), else miniredis. Prod's pods run GOMAXPROCS=3 and
// a pool of 30:
//
//	THP_BENCH_REDIS=127.0.0.1:16399 go test ./services -run '^$' -bench LimiterFreeStreams -benchtime 1x -cpu 3
//
// It uses only NewHybridBucket and Wait, so it runs unchanged against the
// limiter before reservations.
func BenchmarkLimiterFreeStreams(b *testing.B) {
	const (
		streams = 1000
		rate    = 655360.0
		chunk   = 32 << 10
		d       = 5 * time.Second
	)
	addr := os.Getenv("THP_BENCH_REDIS")
	if addr == "" {
		mr := miniredis.NewMiniRedis()
		if err := mr.Start(); err != nil {
			b.Fatal(err)
		}
		defer mr.Close()
		addr = mr.Addr()
	}
	rc := redis.NewClient(&redis.Options{Addr: addr})
	defer rc.Close()
	cc := &callCounter{}
	rc.AddHook(cc)
	for it := 0; it < b.N; it++ {
		run := strconv.FormatInt(time.Now().UnixNano(), 36)
		hbs := make([]*HybridBucket, streams)
		for i := range hbs {
			hbs[i] = NewHybridBucket(rate, rate, rc, "bench-"+run+"-"+strconv.Itoa(i))
			hbs[i].Wait(int64(rate))
		}
		cc.calls.Store(0)
		cc.failed.Store(0)
		cpu0, t0 := cpuSeconds(b), time.Now()
		deadline := t0.Add(d)
		var bytes atomic.Int64
		var wg sync.WaitGroup
		for _, hb := range hbs {
			wg.Add(1)
			go func(hb *HybridBucket) {
				defer wg.Done()
				for time.Now().Before(deadline) {
					hb.Wait(chunk)
					bytes.Add(chunk)
				}
			}(hb)
		}
		wg.Wait()
		wall := time.Since(t0).Seconds()
		b.ReportMetric((cpuSeconds(b)-cpu0)/wall, "cpu-s/s")
		b.ReportMetric(float64(cc.calls.Load())/wall, "calls/s")
		b.ReportMetric(float64(cc.failed.Load())/wall, "failed/s")
		b.ReportMetric(float64(bytes.Load())/wall/(streams*rate), "of-rate")
	}
}
