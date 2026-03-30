package services

import (
	"io"
	"sync"
	"sync/atomic"

	"github.com/prometheus/client_golang/prometheus"
)

var (
	promPrefetchCapacity = prometheus.NewGauge(prometheus.GaugeOpts{
		Name: "webtor_http_proxy_prefetch_pool_capacity",
		Help: "Maximum number of prefetch buffers allowed",
	})
	promPrefetchAllocated = prometheus.NewGauge(prometheus.GaugeOpts{
		Name: "webtor_http_proxy_prefetch_pool_allocated",
		Help: "Number of prefetch buffers allocated so far",
	})
	promPrefetchInUse = prometheus.NewGauge(prometheus.GaugeOpts{
		Name: "webtor_http_proxy_prefetch_pool_in_use",
		Help: "Number of prefetch buffers currently in use",
	})
	promPrefetchMisses = prometheus.NewCounter(prometheus.CounterOpts{
		Name: "webtor_http_proxy_prefetch_pool_misses_total",
		Help: "Number of times a prefetch buffer was requested but pool capacity was reached",
	})
)

func init() {
	prometheus.MustRegister(promPrefetchCapacity)
	prometheus.MustRegister(promPrefetchAllocated)
	prometheus.MustRegister(promPrefetchInUse)
	prometheus.MustRegister(promPrefetchMisses)
}

// PrefetchPool manages a lazy-growing set of reusable byte buffers.
// Buffers are allocated on demand up to maxCount. When all are in use,
// new requests pass through without prefetching.
type PrefetchPool struct {
	buffers   chan []byte
	bufSize   int
	maxCount  int
	allocated atomic.Int64
	inUse     atomic.Int64
}

func NewPrefetchPool(poolSize, bufSize int) *PrefetchPool {
	if poolSize <= 0 || bufSize <= 0 {
		return nil
	}
	maxCount := poolSize / bufSize
	if maxCount == 0 {
		return nil
	}
	p := &PrefetchPool{
		buffers:  make(chan []byte, maxCount),
		bufSize:  bufSize,
		maxCount: maxCount,
	}
	promPrefetchCapacity.Set(float64(maxCount))
	return p
}

func (p *PrefetchPool) Get() []byte {
	// Try to reuse an existing buffer first.
	select {
	case buf := <-p.buffers:
		inUse := p.inUse.Add(1)
		promPrefetchInUse.Set(float64(inUse))
		return buf
	default:
	}
	// No free buffer — try to allocate a new one if under limit.
	cur := p.allocated.Load()
	for cur < int64(p.maxCount) {
		if p.allocated.CompareAndSwap(cur, cur+1) {
			promPrefetchAllocated.Set(float64(cur + 1))
			inUse := p.inUse.Add(1)
			promPrefetchInUse.Set(float64(inUse))
			return make([]byte, p.bufSize)
		}
		cur = p.allocated.Load()
	}
	// Pool exhausted.
	promPrefetchMisses.Inc()
	return nil
}

func (p *PrefetchPool) Put(buf []byte) {
	inUse := p.inUse.Add(-1)
	promPrefetchInUse.Set(float64(inUse))
	select {
	case p.buffers <- buf[:p.bufSize]:
	default:
	}
}

// PrefetchReader wraps an io.ReadCloser with a background goroutine that
// eagerly reads ahead into a ring buffer borrowed from a PrefetchPool.
// The buffer is returned to the pool on Close().
type PrefetchReader struct {
	src    io.ReadCloser
	buf    []byte
	size   int
	r      int
	count  int
	mu     sync.Mutex
	cond   *sync.Cond
	done   bool
	err    error
	closed bool
	pool   *PrefetchPool
}

func NewPrefetchReader(src io.ReadCloser, buf []byte, pool *PrefetchPool) *PrefetchReader {
	p := &PrefetchReader{
		src:  src,
		buf:  buf,
		size: len(buf),
		pool: pool,
	}
	p.cond = sync.NewCond(&p.mu)
	go p.fill()
	return p
}

func (p *PrefetchReader) fill() {
	tmp := make([]byte, 64<<10)
	for {
		n, err := p.src.Read(tmp)

		p.mu.Lock()
		if n > 0 {
			copied := 0
			for copied < n {
				for p.count == p.size && !p.closed {
					p.cond.Wait()
				}
				if p.closed {
					p.mu.Unlock()
					return
				}
				w := (p.r + p.count) % p.size
				space := p.size - p.count
				contiguous := p.size - w
				toWrite := n - copied
				if toWrite > space {
					toWrite = space
				}
				if toWrite > contiguous {
					toWrite = contiguous
				}
				copy(p.buf[w:w+toWrite], tmp[copied:copied+toWrite])
				p.count += toWrite
				copied += toWrite
				p.cond.Broadcast()
			}
		}
		if err != nil {
			p.done = true
			p.err = err
			p.cond.Broadcast()
			p.mu.Unlock()
			return
		}
		p.mu.Unlock()
	}
}

func (p *PrefetchReader) Read(dst []byte) (int, error) {
	p.mu.Lock()
	defer p.mu.Unlock()

	for p.count == 0 && !p.done {
		p.cond.Wait()
	}

	if p.count == 0 {
		if p.err != nil {
			return 0, p.err
		}
		return 0, io.EOF
	}

	n := len(dst)
	if n > p.count {
		n = p.count
	}
	contiguous := p.size - p.r
	if n <= contiguous {
		copy(dst[:n], p.buf[p.r:p.r+n])
	} else {
		copy(dst[:contiguous], p.buf[p.r:p.size])
		copy(dst[contiguous:n], p.buf[:n-contiguous])
	}
	p.r = (p.r + n) % p.size
	p.count -= n
	p.cond.Broadcast()

	return n, nil
}

func (p *PrefetchReader) Close() error {
	p.mu.Lock()
	p.closed = true
	p.cond.Broadcast()
	p.mu.Unlock()
	if p.pool != nil {
		p.pool.Put(p.buf)
	}
	return p.src.Close()
}
